// Package gmessages pulls the full message history out of Google Messages by
// pairing with the phone the same way "Messages for Web" does. This is the
// only route to RCS and group chat history that does not need root.
package gmessages

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/events"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
	"go.mau.fi/util/exhttp"
	"google.golang.org/protobuf/proto"

	"github.com/preswifi/mv2ios/internal/archive"
)

const (
	qrLifetime     = 30 * time.Second
	qrMaxRefreshes = 10
	// libgm marks the session active a few seconds after connecting and has
	// no event for it; requests sent earlier go unanswered.
	settleTime = 6 * time.Second
	// Connecting normally takes a second or two; past this the network or
	// Google is not answering.
	connectTimeout = 30 * time.Second
	// A request for 200 conversations went unanswered by a real phone, so
	// stay close to the page sizes the web client itself uses.
	messagePage    = 100
	conversationPg = 25
	// How long to wait for the phone to upload originals after being asked:
	// at least fullMediaWait, longer while uploads keep arriving, until none
	// has arrived for uploadIdle, and never more than maxOriginalsWait.
	fullMediaWait    = time.Minute
	uploadIdle       = time.Minute
	maxOriginalsWait = 30 * time.Minute
	maxAttempts      = 5
	mediaAttempts    = 3
)

func logger() zerolog.Logger {
	if os.Getenv("MV2IOS_DEBUG") == "" {
		return zerolog.Nop()
	}
	return zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr}).With().Timestamp().Logger()
}

func saveSession(sessionPath string, auth *libgm.AuthData) error {
	data, err := json.Marshal(auth)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(sessionPath), 0o700); err != nil {
		return err
	}
	return os.WriteFile(sessionPath, data, 0o600)
}

// Pair links this computer to the phone. showQR is called with the text to
// render as a QR code, and again each time the code expires and is replaced.
func Pair(ctx context.Context, sessionPath string, showQR func(qr string)) error {
	cli := libgm.NewClient(libgm.NewAuthData(), nil, logger(), exhttp.SensibleClientSettings)
	cli.SetEventHandler(func(any) {})
	paired := make(chan struct{}, 1)
	callback := func(*gmproto.PairedData) {
		select {
		case paired <- struct{}{}:
		default:
		}
	}
	cli.PairCallback.Store(&callback)
	defer cli.Disconnect()

	if err := cli.FetchConfig(ctx); err != nil {
		return fmt.Errorf("fetch Google Messages config: %w", err)
	}
	qr, err := cli.StartLogin(ctx)
	if err != nil {
		return fmt.Errorf("start pairing: %w", err)
	}
	for refreshes := 0; ; refreshes++ {
		showQR(qr)
		select {
		case <-paired:
			// The phone needs a moment to store the pairing; connecting
			// straight away gets the new session unpaired again.
			cli.Disconnect()
			time.Sleep(2 * time.Second)
			return saveSession(sessionPath, cli.AuthData)
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(qrLifetime):
		}
		if refreshes >= qrMaxRefreshes {
			return errors.New("timed out waiting for the QR code to be scanned")
		}
		if qr, err = cli.RefreshPhoneRelay(); err != nil {
			return fmt.Errorf("refresh QR code: %w", err)
		}
	}
}

// Client is a connected, paired session.
type Client struct {
	cli         *libgm.Client
	sessionPath string
	fatal       chan error

	// updates holds the newest version of each message the phone has
	// pushed, which is how full-size attachments arrive after being requested.
	updatesMu  sync.Mutex
	updates    map[string]*gmproto.Message
	updated    chan struct{}
	lastUpdate time.Time
}

// Open connects using the session stored by Pair.
func Open(ctx context.Context, sessionPath string) (*Client, error) {
	data, err := os.ReadFile(sessionPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errors.New("not paired yet: run the pair step first")
		}
		return nil, err
	}
	auth := &libgm.AuthData{}
	if err := json.Unmarshal(data, auth); err != nil {
		return nil, fmt.Errorf("read session: %w", err)
	}
	c := &Client{
		cli:         libgm.NewClient(auth, nil, logger(), exhttp.SensibleClientSettings),
		sessionPath: sessionPath,
		fatal:       make(chan error, 1),
		updates:     map[string]*gmproto.Message{},
		updated:     make(chan struct{}),
	}
	c.cli.SetEventHandler(c.handleEvent)
	// The timer goes around the call, not on ctx: libgm keeps using ctx for
	// the whole connection, so a deadline on it would end the export.
	connected := make(chan error, 1)
	go func() { connected <- c.cli.Connect(ctx) }()
	select {
	case err := <-connected:
		if err != nil {
			return nil, fmt.Errorf("connect: %w", err)
		}
	case <-time.After(connectTimeout):
		c.cli.Disconnect()
		return nil, fmt.Errorf("could not reach Google Messages within %s; check the internet connection and try again", connectTimeout)
	case <-ctx.Done():
		c.cli.Disconnect()
		return nil, ctx.Err()
	}
	select {
	case <-time.After(settleTime):
	case err := <-c.fatal:
		c.cli.Disconnect()
		return nil, err
	case <-ctx.Done():
		c.cli.Disconnect()
		return nil, ctx.Err()
	}
	return c, nil
}

// handleEvent runs on libgm's receive loop, so it must not block or send requests.
func (c *Client) handleEvent(evt any) {
	fail := func(err error) {
		select {
		case c.fatal <- err:
		default:
		}
	}
	switch e := evt.(type) {
	case *libgm.WrappedMessage:
		c.updatesMu.Lock()
		c.updates[e.GetMessageID()] = e.Message
		c.lastUpdate = time.Now()
		close(c.updated)
		c.updated = make(chan struct{})
		c.updatesMu.Unlock()
	case *events.AuthTokenRefreshed:
		_ = saveSession(c.sessionPath, c.cli.AuthData)
	case *events.GaiaLoggedOut:
		fail(errors.New("the Google account was logged out; pair again"))
	case *gmproto.RevokePairData:
		fail(errors.New("the phone removed this pairing; pair again"))
	case *events.ListenFatalError:
		fail(fmt.Errorf("connection to Google Messages failed: %w", e.Error))
	}
}

func (c *Client) Close() {
	_ = saveSession(c.sessionPath, c.cli.AuthData)
	c.cli.Disconnect()
}

// Unpair removes this computer from the phone's list of paired devices.
func (c *Client) Unpair(ctx context.Context) error {
	err := c.cli.Unpair(ctx)
	c.cli.Disconnect()
	if rmErr := os.Remove(c.sessionPath); err == nil {
		err = rmErr
	}
	return err
}

// retry covers the phone briefly dropping off the network mid-export.
// Each attempt can take a minute when the phone is silent, so failures are
// reported as they happen rather than only at the end.
func retry[T any](ctx context.Context, logf func(string, ...any), attempts int, fn func() (T, error)) (T, error) {
	var zero T
	var err error
	for attempt := 1; attempt <= attempts; attempt++ {
		var v T
		if v, err = fn(); err == nil {
			return v, nil
		}
		if isSignedOut(err) {
			return zero, fmt.Errorf("%w: %w", ErrSignedOut, err)
		}
		if attempt == attempts || ctx.Err() != nil {
			break
		}
		logf("  %v; retrying (attempt %d of %d)", err, attempt+1, attempts)
		select {
		case <-ctx.Done():
			return zero, ctx.Err()
		case <-time.After(time.Duration(attempt) * 5 * time.Second):
		}
	}
	return zero, err
}

// ErrSignedOut means Google stopped accepting the copied sign-in. Retrying
// cannot help; the user has to pair again.
var ErrSignedOut = errors.New("Google no longer accepts this computer's sign-in")

func isSignedOut(err error) bool {
	var httpErr events.HTTPError
	return errors.Is(err, events.ErrInvalidCredentials) ||
		(errors.As(err, &httpErr) && httpErr.Resp != nil && httpErr.Resp.StatusCode == 401) ||
		strings.Contains(err.Error(), "HTTP 401")
}

func isUnanswered(err error) bool {
	return errors.Is(err, libgm.ErrPhoneNotResponding)
}

// listTimeout bounds each conversation-list request after the first. The
// phone answers those within a second or two when it answers at all, and the
// library's own one-minute limit makes skipping past a conversation it will
// not send very slow.
const listTimeout = 15 * time.Second

func (c *Client) listQuick(ctx context.Context, req *gmproto.ListConversationsRequest) (*gmproto.ListConversationsResponse, error) {
	quick, cancel := context.WithTimeout(ctx, listTimeout)
	defer cancel()
	resp, err := c.cli.ListConversations(quick, req)
	if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
		return nil, fmt.Errorf("%w in %s", libgm.ErrPhoneNotResponding, listTimeout)
	}
	return resp, err
}

// ErrListIncomplete means the phone stopped answering partway through the
// conversation list. Exporting only part of the list would silently leave
// conversations behind, so the export stops instead.
var ErrListIncomplete = errors.New("the phone stopped sending the conversation list partway through")

// singleSteps is how many conversations are fetched one at a time after a
// page goes unanswered, to step up to the conversation that caused it.
const singleSteps = 25

// skipUnsendable finds the next conversation the phone will send after one
// it will not. A request whose page would include certain conversations is
// never answered, even when that conversation is the only one asked for, so
// the only way past is a cursor that starts beyond it. Cursors carry the
// last-activity time of the conversation before the page, so this searches
// for the latest time that gets an answer. It returns that answer and the
// time range in which the skipped conversation was last active.
func (c *Client) skipUnsendable(ctx context.Context, logf func(string, ...any), folder gmproto.ListConversationsRequest_Folder, cursor *gmproto.Cursor) (resp *gmproto.ListConversationsResponse, from, to time.Time, err error) {
	probe := func(before int64) (*gmproto.ListConversationsResponse, error) {
		return c.listQuick(ctx, &gmproto.ListConversationsRequest{
			Count:  1,
			Folder: folder,
			Cursor: &gmproto.Cursor{LastItemID: cursor.GetLastItemID(), LastItemTimestamp: before},
		})
	}
	const minute = int64(time.Minute / time.Millisecond)
	hi := cursor.GetLastItemTimestamp()
	logf("  The phone will not send the conversation after the one last active on %s. Searching past it...",
		time.UnixMilli(hi).Format("2 Jan 2006 15:04"))
	var lo int64
	found := false
	for _, back := range []time.Duration{time.Second, time.Minute, time.Hour, 24 * time.Hour, 7 * 24 * time.Hour,
		30 * 24 * time.Hour, 365 * 24 * time.Hour, 5 * 365 * 24 * time.Hour} {
		before := cursor.GetLastItemTimestamp() - int64(back/time.Millisecond)
		r, err := probe(before)
		if err == nil {
			lo, resp, found = before, r, true
			break
		}
		if !isUnanswered(err) || ctx.Err() != nil {
			return nil, from, to, err
		}
		hi = before
	}
	if !found {
		return nil, from, to, errNothingOlder
	}
	// Narrow the gap to a minute so that only the one conversation is skipped.
	for hi-lo > minute {
		mid := lo + (hi-lo)/2
		r, err := probe(mid)
		switch {
		case err == nil:
			lo, resp = mid, r
		case isUnanswered(err) && ctx.Err() == nil:
			hi = mid
		default:
			return nil, from, to, err
		}
	}
	return resp, time.UnixMilli(lo), time.UnixMilli(hi), nil
}

// errNothingOlder means no probe got an answer even years further back. The
// phone also ignores requests that start past the end of the list, so this
// is the end of the list, possibly with one hidden conversation at the bottom.
var errNothingOlder = errors.New("no older conversations")

// Skipped describes a conversation the phone would not send.
type Skipped struct{ From, To time.Time }

func (s Skipped) String() string {
	return fmt.Sprintf("last active between %s and %s", s.From.Format("2 Jan 2006 15:04"), s.To.Format("15:04"))
}

func (c *Client) listConversations(ctx context.Context, logf func(string, ...any)) ([]*gmproto.Conversation, []Skipped, error) {
	var out []*gmproto.Conversation
	var skipped []Skipped
	seen := map[string]bool{}
	for _, folder := range []gmproto.ListConversationsRequest_Folder{
		gmproto.ListConversationsRequest_INBOX,
		gmproto.ListConversationsRequest_ARCHIVE,
	} {
		var cursor *gmproto.Cursor
		singles := 0
		for {
			count := int64(conversationPg)
			if singles > 0 {
				count = 1
			}
			var resp *gmproto.ListConversationsResponse
			var err error
			if cursor == nil && folder == gmproto.ListConversationsRequest_INBOX {
				resp, err = retry(ctx, logf, maxAttempts, func() (*gmproto.ListConversationsResponse, error) {
					return c.cli.ListConversations(ctx, &gmproto.ListConversationsRequest{Count: count, Folder: folder})
				})
			} else {
				resp, err = c.listQuick(ctx, &gmproto.ListConversationsRequest{Count: count, Folder: folder, Cursor: cursor})
			}
			if err != nil && isUnanswered(err) && ctx.Err() == nil {
				switch {
				case cursor == nil && folder == gmproto.ListConversationsRequest_ARCHIVE:
					// The phone does not answer a request with nothing to send.
					logf("  No archived conversations.")
				case cursor == nil:
					return out, skipped, fmt.Errorf("%w (it did not answer at all)", ErrListIncomplete)
				case count > 1:
					logf("  No answer; going one conversation at a time to find the one causing it.")
					singles = singleSteps
					continue
				default:
					var from, to time.Time
					resp, from, to, err = c.skipUnsendable(ctx, logf, folder, cursor)
					if errors.Is(err, errNothingOlder) {
						logf("  Nothing older than that; this is the end of the %s.", strings.ToLower(folder.String()))
						resp = nil
						break
					}
					if err != nil {
						return out, skipped, fmt.Errorf("%w (after %d conversations: %w)", ErrListIncomplete, len(out), err)
					}
					skipped = append(skipped, Skipped{From: from, To: to})
					logf("  Skipped one conversation %s.", skipped[len(skipped)-1])
					singles = singleSteps
				}
				if resp == nil {
					break
				}
			} else if err != nil {
				return out, skipped, fmt.Errorf("list conversations: %w", err)
			} else if singles > 0 {
				singles--
			}
			added := 0
			for _, conv := range resp.GetConversations() {
				if id := conv.GetConversationID(); id != "" && !seen[id] {
					seen[id] = true
					out = append(out, conv)
					added++
				}
			}
			if added > 0 && (singles == 0 || len(out)%10 == 0) {
				logf("  %d conversations so far", len(out))
			}
			// Pages overlap a little, so a page of only repeats is not the end;
			// the end is an empty page or a cursor that no longer moves.
			next := resp.GetCursor()
			if len(resp.GetConversations()) == 0 || next == nil ||
				(cursor != nil && next.GetLastItemID() == cursor.GetLastItemID()) {
				break
			}
			cursor = next
		}
	}
	return out, skipped, nil
}

// fetchAllMessages pages back through a conversation. partial is set when a
// later page went unanswered: that is probably the start of the conversation,
// but it cannot be told apart from the phone dropping out.
func (c *Client) fetchAllMessages(ctx context.Context, logf func(string, ...any), convID string, progress func(n int)) (out []*gmproto.Message, partial bool, err error) {
	seen := map[string]bool{}
	var cursor *gmproto.Cursor
	for {
		attempts := maxAttempts
		if cursor != nil {
			attempts = 2
		}
		resp, err := retry(ctx, logf, attempts, func() (*gmproto.ListMessagesResponse, error) {
			return c.cli.FetchMessages(ctx, convID, messagePage, cursor)
		})
		if cursor != nil && isUnanswered(err) {
			return out, true, nil
		}
		if err != nil {
			return nil, false, err
		}
		added := 0
		var oldest *gmproto.Message
		for _, m := range resp.GetMessages() {
			if oldest == nil || m.GetTimestamp() < oldest.GetTimestamp() {
				oldest = m
			}
			if !seen[m.GetMessageID()] {
				seen[m.GetMessageID()] = true
				out = append(out, m)
				added++
			}
		}
		if added == 0 {
			return out, false, nil
		}
		progress(len(out))
		// The phone answers the request after the oldest page with silence,
		// which costs minutes to detect, but the oldest page is also the only
		// one that comes back short.
		if len(resp.GetMessages()) < messagePage {
			return out, false, nil
		}
		cursor = resp.GetCursor()
		if cursor == nil {
			cursor = &gmproto.Cursor{
				LastItemID:        oldest.GetMessageID(),
				LastItemTimestamp: time.UnixMicro(oldest.GetTimestamp()).UnixMilli(),
			}
		}
	}
}

func protocolOf(messageType int64, fallback string) string {
	switch messageType {
	case 1:
		return archive.ProtocolSMS
	case 2, 3:
		return archive.ProtocolMMS
	case 4:
		return archive.ProtocolRCS
	}
	return fallback
}

func mediaInfo(m *gmproto.MediaContent) (mimeType, ext string) {
	known := libgm.FormatToMediaType[m.GetFormat()]
	mimeType = m.GetMimeType()
	if mimeType == "" {
		mimeType = known.Format
	}
	ext = strings.TrimPrefix(path.Ext(m.GetMediaName()), ".")
	if ext == "" {
		ext = known.Extension
	}
	if ext == "" && mimeType != "" {
		if exts, _ := mime.ExtensionsByType(mimeType); len(exts) > 0 {
			ext = strings.TrimPrefix(exts[0], ".")
		}
	}
	if ext == "" {
		ext = "bin"
	}
	return mimeType, archive.SafeName(ext)
}

func (c *Client) openMedia(m *gmproto.MediaContent) (io.ReadCloser, error) {
	switch {
	case m.GetMediaID() != "":
		return c.cli.DownloadMedia(m.GetMediaID(), m.GetDecryptionKey())
	case m.GetThumbnailMediaID() != "":
		return c.cli.DownloadMedia(m.GetThumbnailMediaID(), m.GetThumbnailDecryptionKey())
	case len(m.GetMediaData()) > 0:
		return io.NopCloser(strings.NewReader(string(m.GetMediaData()))), nil
	}
	return nil, errors.New("message has no downloadable media")
}

// requestOriginals asks the phone to upload the full-size file for every
// attachment it has only sent a preview of. The phone answers each request
// straight away and pushes the upload later as a message update.
func (c *Client) requestOriginals(ctx context.Context, messages []*gmproto.Message, progress func(n int)) (requested int) {
	failures := 0
	for _, m := range messages {
		for _, part := range m.GetMessageInfo() {
			media := part.GetMediaContent()
			if media == nil || media.GetMediaID() != "" || part.GetActionMessageID() == "" {
				continue
			}
			if _, err := c.cli.GetFullSizeImage(ctx, m.GetMessageID(), part.GetActionMessageID()); err != nil {
				// Stop asking once the phone has clearly stopped listening;
				// the previews are still saved.
				if failures++; failures >= 3 || ctx.Err() != nil {
					return requested
				}
				continue
			}
			failures = 0
			if requested++; requested%100 == 0 {
				progress(requested)
			}
		}
	}
	return requested
}

// awaitOriginal waits for the phone to push the full-size version of one
// attachment, and returns nil if it does not arrive. The phone uploads the
// requested originals one after another, so the wait lasts as long as uploads
// keep arriving rather than a fixed time.
func (c *Client) awaitOriginal(ctx context.Context, messageID, partID string, requested time.Time) *gmproto.MediaContent {
	for {
		c.updatesMu.Lock()
		updated := c.updated
		deadline := requested.Add(fullMediaWait)
		if idle := c.lastUpdate.Add(uploadIdle); idle.After(deadline) {
			deadline = idle
		}
		if limit := requested.Add(maxOriginalsWait); deadline.After(limit) {
			deadline = limit
		}
		if m := c.updates[messageID]; m != nil {
			for _, part := range m.GetMessageInfo() {
				if part.GetActionMessageID() == partID && part.GetMediaContent().GetMediaID() != "" {
					c.updatesMu.Unlock()
					return part.GetMediaContent()
				}
			}
		}
		c.updatesMu.Unlock()
		wait := time.Until(deadline)
		if wait <= 0 {
			return nil
		}
		select {
		case <-updated:
		case <-time.After(wait):
		case <-ctx.Done():
			return nil
		}
	}
}

func (c *Client) saveMedia(ctx context.Context, dir, relPath string, m *gmproto.MediaContent) (int64, error) {
	full := filepath.Join(dir, filepath.FromSlash(relPath))
	if st, err := os.Stat(full); err == nil && st.Size() > 0 {
		return st.Size(), nil
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		return 0, err
	}
	return retry(ctx, func(string, ...any) {}, mediaAttempts, func() (int64, error) {
		src, err := c.openMedia(m)
		if err != nil {
			return 0, err
		}
		defer src.Close()
		tmp := full + ".tmp"
		dst, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			return 0, err
		}
		n, err := io.Copy(dst, src)
		if closeErr := dst.Close(); err == nil {
			err = closeErr
		}
		if err == nil && n == 0 {
			err = errors.New("empty download")
		}
		if err != nil {
			os.Remove(tmp)
			return 0, err
		}
		return n, os.Rename(tmp, full)
	})
}

// mediaRun tracks attachment downloads for one conversation.
type mediaRun struct {
	requested   time.Time
	done, total int
	progress    func(done, total int)
}

func (c *Client) convertMessage(ctx context.Context, dir string, conv *archive.Conversation, m *gmproto.Message, run *mediaRun, stats *Stats) (archive.Message, bool) {
	status := m.GetMessageStatus().GetStatus()
	// 200 and up are system notices ("X joined"), not messages.
	if status >= gmproto.MessageStatusType_TOMBSTONE_PARTICIPANT_JOINED || status == gmproto.MessageStatusType_OUTGOING_DRAFT {
		return archive.Message{}, false
	}
	out := archive.Message{
		ID:        m.GetMessageID(),
		Timestamp: time.UnixMicro(m.GetTimestamp()).UTC(),
		SenderID:  m.GetParticipantID(),
		Protocol:  protocolOf(m.GetType(), conv.Protocol),
		Subject:   m.GetSubject(),
	}
	if sender, ok := conv.Participant(out.SenderID); ok {
		out.FromMe = sender.IsMe
	} else {
		out.FromMe = status != gmproto.MessageStatusType_STATUS_UNKNOWN && status < gmproto.MessageStatusType_INCOMING_COMPLETE
	}
	var text []string
	for i, part := range m.GetMessageInfo() {
		if content := part.GetMessageContent(); content != nil {
			if content.GetContent() != "" {
				text = append(text, content.GetContent())
			}
			continue
		}
		media := part.GetMediaContent()
		if media == nil {
			continue
		}
		preview := media.GetMediaID() == ""
		if preview {
			if original := c.awaitOriginal(ctx, out.ID, part.GetActionMessageID(), run.requested); original != nil {
				media, preview = original, false
			}
		}
		mimeType, ext := mediaInfo(media)
		att := archive.Attachment{Name: media.GetMediaName(), MimeType: mimeType, Thumbnail: preview}
		suffix := ""
		if preview {
			// Previews are small JPEGs whatever the original was, and a
			// distinct name lets a later run fetch the original alongside.
			suffix, ext, att.MimeType = "-preview", "jpg", "image/jpeg"
		}
		rel := path.Join("media", archive.SafeName(conv.ID), fmt.Sprintf("%s-%d%s.%s", archive.SafeName(out.ID), i, suffix, ext))
		size, err := c.saveMedia(ctx, dir, rel, media)
		switch {
		case err != nil:
			att.Error = err.Error()
			stats.MediaFailed++
		case preview:
			att.Path, att.Size = rel, size
			stats.Previews++
		default:
			att.Path, att.Size = rel, size
			stats.Media++
			// A preview saved by an earlier run is no longer needed.
			os.Remove(filepath.Join(dir, "media", archive.SafeName(conv.ID), fmt.Sprintf("%s-%d-preview.jpg", archive.SafeName(out.ID), i)))
		}
		out.Attachments = append(out.Attachments, att)
		run.done++
		run.progress(run.done, run.total)
	}
	out.Text = strings.Join(text, "\n")
	for _, r := range m.GetReactions() {
		if emoji := r.GetData().GetUnicode(); emoji != "" {
			out.Reactions = append(out.Reactions, archive.Reaction{Emoji: emoji, ParticipantIDs: r.GetParticipantIDs()})
		}
	}
	if out.Text == "" && out.Subject == "" && len(out.Attachments) == 0 {
		return archive.Message{}, false
	}
	return out, true
}

func convertConversation(conv *gmproto.Conversation) *archive.Conversation {
	out := &archive.Conversation{
		ID:       conv.GetConversationID(),
		Name:     conv.GetName(),
		IsGroup:  conv.GetIsGroupChat(),
		Protocol: archive.ProtocolSMS,
	}
	if conv.GetType() == gmproto.ConversationType_RCS {
		out.Protocol = archive.ProtocolRCS
	}
	for _, p := range conv.GetParticipants() {
		name := p.GetFullName()
		if name == "" {
			name = p.GetFirstName()
		}
		out.Participants = append(out.Participants, archive.Participant{
			ID:     p.GetID().GetParticipantID(),
			Number: p.GetID().GetNumber(),
			Name:   name,
			IsMe:   p.GetIsMe(),
		})
	}
	return out
}

type Stats struct {
	Conversations int
	Skipped       int
	Messages      int
	Media         int
	MediaFailed   int
	// Previews counts attachments saved only as the small preview because
	// the phone did not supply the original.
	Previews int
	Failed   []string
	// Partial lists conversations whose oldest messages may be missing.
	Partial []string
}

const savedListName = "conversation-list.pb"

type ExportOptions struct {
	// RefreshList fetches the conversation list again even if one was saved.
	RefreshList bool
	// ListOnly stops after fetching the conversation list.
	ListOnly bool
	// Parallel is how many conversations are exported at the same time.
	Parallel int
	// RetryPreviews exports again any saved conversation that still has
	// attachments saved only as previews, asking the phone for the originals.
	RetryPreviews bool
}

// conversationList returns the saved list from an earlier run if there is
// one, because getting a complete list out of the phone can take several tries.
func (c *Client) conversationList(ctx context.Context, dir string, refresh bool, logf func(string, ...any)) ([]*gmproto.Conversation, error) {
	path := filepath.Join(dir, savedListName)
	if !refresh {
		if data, err := os.ReadFile(path); err == nil {
			var saved gmproto.ListConversationsResponse
			if err := proto.Unmarshal(data, &saved); err == nil {
				st, _ := os.Stat(path)
				logf("Using the conversation list saved on %s. Pass -refresh-list to fetch it again,", st.ModTime().Format("2 January 15:04"))
				logf("for example to pick up conversations started since then.")
				return saved.GetConversations(), nil
			}
		}
	}
	logf("Asking the phone for the conversation list (this can take a few minutes)...")
	convs, skipped, err := c.listConversations(ctx, logf)
	if err != nil {
		return nil, err
	}
	if len(skipped) > 0 {
		logf("WARNING: the phone refused to send %d conversations, so they cannot be exported:", len(skipped))
		for _, sk := range skipped {
			logf("  - a conversation %s", sk)
		}
		logf("These are usually deleted or hidden conversations that Google Messages no longer shows.")
	}
	data, err := proto.Marshal(&gmproto.ListConversationsResponse{Conversations: convs})
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return convs, os.WriteFile(path, data, 0o600)
}

// Export writes every conversation to dir. Conversations already present in
// dir are skipped, so an interrupted export can be resumed by running it again.
func (c *Client) Export(ctx context.Context, dir string, opts ExportOptions, logf func(format string, args ...any)) (Stats, error) {
	var stats Stats
	convs, err := c.conversationList(ctx, dir, opts.RefreshList, logf)
	if err != nil {
		return stats, err
	}
	oldest := time.Now().UnixMicro()
	for _, conv := range convs {
		oldest = min(oldest, conv.GetLastMessageTimestamp())
	}
	logf("Found %d conversations. The least recently active one was last used on %s.", len(convs), time.UnixMicro(oldest).Format("2 January 2006"))
	if opts.ListOnly {
		return stats, nil
	}
	workers := max(opts.Parallel, 1)
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	var mu sync.Mutex
	jobs := make(chan int)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				raw := convs[i]
				label := fmt.Sprintf("[%d/%d] %s", i+1, len(convs), raw.GetName())
				var one Stats
				err := c.exportConversation(ctx, dir, raw, label, logf, &one)
				mu.Lock()
				stats.add(one)
				mu.Unlock()
				if err != nil {
					// Signed out, out of disk space and the like stop everything.
					cancel(err)
				}
			}
		}()
	}
	for i, raw := range convs {
		id := raw.GetConversationID()
		if archive.HasConversation(dir, id) && !(opts.RetryPreviews && archive.HasPreviews(dir, id)) {
			mu.Lock()
			stats.Skipped++
			mu.Unlock()
			continue
		}
		select {
		case jobs <- i:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
	}
	close(jobs)
	wg.Wait()
	if err := context.Cause(ctx); err != nil {
		return stats, err
	}
	return stats, nil
}

func (s *Stats) add(o Stats) {
	s.Conversations += o.Conversations
	s.Skipped += o.Skipped
	s.Messages += o.Messages
	s.Media += o.Media
	s.MediaFailed += o.MediaFailed
	s.Previews += o.Previews
	s.Failed = append(s.Failed, o.Failed...)
	s.Partial = append(s.Partial, o.Partial...)
}

// exportConversation saves one conversation. It returns an error only for
// problems that should stop the whole export; a conversation that fails on
// its own is recorded in stats instead.
func (c *Client) exportConversation(ctx context.Context, dir string, raw *gmproto.Conversation, label string, logf func(string, ...any), stats *Stats) error {
	id := raw.GetConversationID()
	if len(raw.GetParticipants()) == 0 {
		if full, err := c.cli.GetConversation(ctx, id); err == nil && full != nil {
			raw = full
		}
	}
	conv := convertConversation(raw)
	messages, partial, err := c.fetchAllMessages(ctx, logf, id, func(n int) { logf("%s: %d messages", label, n) })
	if err != nil {
		if ctx.Err() != nil || errors.Is(err, ErrSignedOut) {
			return err
		}
		logf("%s: FAILED: %v", label, err)
		stats.Failed = append(stats.Failed, raw.GetName())
		return nil
	}
	if partial {
		stats.Partial = append(stats.Partial, raw.GetName())
	}
	run := &mediaRun{progress: func(done, total int) {
		if done%25 == 0 || done == total {
			logf("%s: %d of %d attachments saved", label, done, total)
		}
	}}
	for _, m := range messages {
		for _, part := range m.GetMessageInfo() {
			if part.GetMediaContent() != nil {
				run.total++
			}
		}
	}
	if run.total > 0 {
		if n := c.requestOriginals(ctx, messages, func(n int) {
			logf("%s: asked the phone for %d original photos and videos so far", label, n)
		}); n > 0 {
			logf("%s: waiting for the phone to upload %d original photos and videos...", label, n)
		}
		run.requested = time.Now()
	}
	for _, m := range messages {
		if msg, ok := c.convertMessage(ctx, dir, conv, m, run, stats); ok {
			conv.Messages = append(conv.Messages, msg)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	if err := archive.WriteConversation(dir, conv); err != nil {
		return err
	}
	stats.Conversations++
	stats.Messages += len(conv.Messages)
	logf("%s: saved %d messages", label, len(conv.Messages))
	return nil
}
