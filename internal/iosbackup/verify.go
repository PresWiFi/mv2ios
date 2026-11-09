package iosbackup

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/preswifi/mv2ios/internal/archive"
)

// ConvReport compares one exported conversation with what is in a backup.
type ConvReport struct {
	ID      string
	Name    string
	Group   bool
	Members int
	// Expected is how many messages should be on the phone.
	Expected int
	// Imported are messages mv2ios added that are where they belong.
	Imported int
	// AlreadyThere are messages the phone had before mv2ios ran.
	AlreadyThere int
	Missing      int
	// WrongChat are messages filed under a different conversation.
	WrongChat int
	// WrongDirection are sent messages shown as received, or the reverse.
	WrongDirection int
	// WrongSender are group messages shown as coming from the wrong person.
	WrongSender int
	// BlankBubble are messages with text that the phone shows as empty.
	BlankBubble int
	// MissingAttachments are attachment files the archive has but the backup does not.
	MissingAttachments int

	// Notes about the archive itself rather than the backup.

	// FromSelfEntry are messages sent from an extra entry for the owner,
	// which older versions imported as received from someone else.
	FromSelfEntry int
	// UnknownSender are group messages whose sender is not in the
	// participant list, shown as coming from another member.
	UnknownSender int
	// Dropped lists participants left out because they have no usable address.
	Dropped []string
	// Skipped says why the conversation is not imported at all, if it is not.
	Skipped string
}

// Problems is the number of messages that are not on the phone as they should be.
func (r ConvReport) Problems() int {
	return r.Missing + r.WrongChat + r.WrongDirection + r.WrongSender + r.BlankBubble + r.MissingAttachments
}

type VerifyResult struct {
	Conversations []ConvReport
	Totals        ConvReport
	// SelfAddresses are the addresses treated as the owner besides the main number.
	SelfAddresses []string
}

type VerifyOptions struct {
	SelfAddresses []string
}

// Verify checks every message in archiveDir against the Messages database in
// the backup at backupDir, without changing either. Run it on the backup
// inject wrote, or better, on a fresh backup taken after restoring it to the
// iPhone, which shows what the phone actually kept.
func Verify(backupDir, archiveDir string, opts VerifyOptions) (VerifyResult, error) {
	var out VerifyResult
	info, err := ReadInfo(backupDir)
	if err != nil {
		return out, err
	}
	if info.Encrypted {
		return out, ErrEncrypted
	}
	convs, err := archive.Load(archiveDir)
	if err != nil {
		return out, err
	}
	work, err := os.MkdirTemp("", "mv2ios-verify-")
	if err != nil {
		return out, err
	}
	defer os.RemoveAll(work)
	workSMS := filepath.Join(work, "sms.db")
	if err := copyFile(filepath.Join(backupDir, storedPath(fileID(smsDomain, smsRelativePath))), workSMS); err != nil {
		return out, fmt.Errorf("the backup has no Messages database: %w", err)
	}
	db, err := openSQLite(workSMS)
	if err != nil {
		return out, err
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		return out, err
	}
	// Nothing is written; this only undoes openSMS dropping the triggers.
	defer tx.Rollback()
	sms, err := openSMS(tx)
	if err != nil {
		return out, fmt.Errorf("open Messages database: %w", err)
	}

	ppl := identify(convs, opts.SelfAddresses)
	out.SelfAddresses = ppl.detected
	for _, conv := range convs {
		r, err := verifyConversation(sms, backupDir, archiveDir, ppl, conv)
		if err != nil {
			return out, fmt.Errorf("%s: %w", conv.Name, err)
		}
		out.Conversations = append(out.Conversations, r)
		t := &out.Totals
		t.Expected += r.Expected
		t.Imported += r.Imported
		t.AlreadyThere += r.AlreadyThere
		t.Missing += r.Missing
		t.WrongChat += r.WrongChat
		t.WrongDirection += r.WrongDirection
		t.WrongSender += r.WrongSender
		t.BlankBubble += r.BlankBubble
		t.MissingAttachments += r.MissingAttachments
		t.FromSelfEntry += r.FromSelfEntry
		t.UnknownSender += r.UnknownSender
	}
	return out, nil
}

// acceptableChats returns the chats a conversation's messages may be in. A
// message counts as in the right place if it is in any of them, since the
// phone can keep separate SMS and iMessage threads with the same people.
func (s *smsDB) acceptableChats(sourceID string, cp convPlan) (map[int64]bool, error) {
	chats := map[int64]bool{}
	style := chatStyleDirect
	if cp.isGroup {
		style = chatStyleGroup
	}
	members, err := s.chatMembers(style)
	if err != nil {
		return nil, err
	}
	want := groupKey(cp.members)
	for id, m := range members {
		if groupKey(dedupe(m)) == want {
			chats[id] = true
		}
	}
	if cp.isGroup {
		var id int64
		err := s.tx.QueryRow(`SELECT ROWID FROM chat WHERE room_name = ? AND style = ?`, groupRoom(sourceID), chatStyleGroup).Scan(&id)
		if err == nil {
			chats[id] = true
		} else if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
	}
	return chats, nil
}

// chatMembers returns the members of every chat of one style, read once.
func (s *smsDB) chatMembers(style int) (map[int64][]string, error) {
	if m, ok := s.membersByStyle[style]; ok {
		return m, nil
	}
	rows, err := s.tx.Query(`
		SELECT c.ROWID, h.id FROM chat c
		JOIN chat_handle_join j ON j.chat_id = c.ROWID
		JOIN handle h ON h.ROWID = j.handle_id
		WHERE c.style = ?`, style)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	members := map[int64][]string{}
	for rows.Next() {
		var id int64
		var handle string
		if err := rows.Scan(&id, &handle); err != nil {
			return nil, err
		}
		members[id] = append(members[id], handle)
	}
	if s.membersByStyle == nil {
		s.membersByStyle = map[int]map[int64][]string{}
	}
	s.membersByStyle[style] = members
	return members, rows.Err()
}

func dedupe(ids []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

type storedMessage struct {
	rowID       int64
	fromMe      bool
	sender      string
	chatID      int64
	hasBody     bool
	attachments []string
}

func (s *smsDB) storedMessage(guid string) (storedMessage, bool, error) {
	var m storedMessage
	body := "0"
	if s.columns["message"]["attributedBody"] {
		body = "m.attributedBody IS NOT NULL"
	}
	err := s.tx.QueryRow(`
		SELECT m.ROWID, m.is_from_me, COALESCE(h.id, ''), `+body+`,
			COALESCE((SELECT chat_id FROM chat_message_join WHERE message_id = m.ROWID ORDER BY chat_id LIMIT 1), 0)
		FROM message m LEFT JOIN handle h ON h.ROWID = m.handle_id
		WHERE m.guid = ?`, guid).Scan(&m.rowID, &m.fromMe, &m.sender, &m.hasBody, &m.chatID)
	if errors.Is(err, sql.ErrNoRows) {
		return m, false, nil
	}
	if err != nil {
		return m, false, err
	}
	rows, err := s.tx.Query(`
		SELECT COALESCE(a.filename, '') FROM attachment a
		JOIN message_attachment_join j ON j.attachment_id = a.ROWID
		WHERE j.message_id = ?`, m.rowID)
	if err != nil {
		return m, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return m, false, err
		}
		m.attachments = append(m.attachments, name)
	}
	return m, true, rows.Err()
}

// attachmentInBackup reports whether the file for an attachment path such as
// "~/Library/SMS/Attachments/..." is stored in the backup.
func attachmentInBackup(backupDir, filename string) bool {
	rel := strings.TrimPrefix(filename, "~/")
	return fileExists(filepath.Join(backupDir, storedPath(fileID(mediaDomain, rel))))
}

func verifyConversation(s *smsDB, backupDir, archiveDir string, ppl people, conv *archive.Conversation) (ConvReport, error) {
	cp := ppl.plan(conv)
	r := ConvReport{ID: conv.ID, Name: conv.Name, Group: cp.isGroup, Members: len(cp.members), Dropped: cp.dropped}
	for _, m := range conv.Messages {
		if !m.FromMe && cp.selfIDs[m.SenderID] {
			r.FromSelfEntry++
		}
	}
	switch {
	case len(conv.Messages) == 0:
		r.Skipped = "no messages"
	case len(cp.members) == 0:
		r.Skipped = "no one but you, or no usable phone number"
	}
	if r.Skipped != "" {
		return r, nil
	}

	chats, err := s.acceptableChats(conv.ID, cp)
	if err != nil {
		return r, err
	}
	existing := existingIndex{}
	for id := range chats {
		idx, err := s.existingMessages(id)
		if err != nil {
			return r, err
		}
		for k, v := range idx {
			existing[k] = append(existing[k], v...)
		}
	}

	for _, msg := range conv.Messages {
		fromMe := cp.fromMe(msg)
		sender, known := "", true
		if cp.isGroup && !fromMe {
			sender, known = cp.sender(msg)
			if !known {
				r.UnknownSender++
			}
		}
		usable, _ := usableAttachments(archiveDir, msg)
		if msg.Text == "" && msg.Subject == "" && len(usable) == 0 {
			continue // inject leaves these out on purpose
		}
		r.Expected++

		stored, ok, err := s.storedMessage(derivedGUID("message", msg.ID))
		if err != nil {
			return r, err
		}
		if !ok {
			if existing.contains(fromMe, msg.Text, msg.Timestamp) {
				r.AlreadyThere++
			} else {
				r.Missing++
			}
			continue
		}
		bad := false
		if !chats[stored.chatID] {
			r.WrongChat++
			bad = true
		}
		if stored.fromMe != fromMe {
			r.WrongDirection++
			bad = true
		} else if cp.isGroup && !fromMe && known && stored.sender != sender {
			r.WrongSender++
			bad = true
		}
		if msg.Text != "" && !stored.hasBody && s.columns["message"]["attributedBody"] {
			r.BlankBubble++
			bad = true
		}
		present := 0
		for _, name := range stored.attachments {
			if attachmentInBackup(backupDir, name) {
				present++
			}
		}
		if present < len(usable) {
			r.MissingAttachments += len(usable) - present
			bad = true
		}
		if !bad {
			r.Imported++
		}
	}
	return r, nil
}
