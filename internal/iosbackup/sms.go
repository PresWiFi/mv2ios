package iosbackup

import (
	"crypto/sha1"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	// Messages stores dates relative to 2001-01-01 rather than 1970-01-01.
	appleEpochOffset = 978307200

	chatStyleGroup  = 43
	chatStyleDirect = 45

	serviceSMS = "SMS"

	// attachmentPlaceholder marks where an attachment sits in a message's text.
	attachmentPlaceholder = "￼"

	// Two copies of a message count as the same one when their timestamps
	// differ by no more than this. Clocks and rounding differ between phones.
	duplicateWindow = 5 * time.Second
)

// derivedGUID gives the same GUID for the same source ID on every run, so
// importing twice cannot create duplicates.
func derivedGUID(kind, sourceID string) string {
	sum := sha1.Sum([]byte("mv2ios:" + kind + ":" + sourceID))
	b := sum[:16]
	return strings.ToUpper(fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]))
}

type trigger struct{ name, sql string }

// smsDB wraps the Messages database from a backup.
type smsDB struct {
	tx       *sql.Tx
	columns  map[string]map[string]bool
	triggers []trigger

	// Conventions copied from what is already in the database, so imported
	// rows look like the ones this iOS version writes itself.
	nanoseconds bool
	guidPrefix  string
	country     any
	account     any
	accountGUID any
	accountID   any
	login       any

	handles map[string]int64
	groups  map[string]groupChat
}

type groupChat struct {
	id       int64
	roomName string
}

func (s *smsDB) loadColumns(table string) error {
	rows, err := s.tx.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return err
	}
	defer rows.Close()
	cols := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		cols[name] = true
	}
	if len(cols) == 0 {
		return fmt.Errorf("the Messages database has no %q table", table)
	}
	s.columns[table] = cols
	return rows.Err()
}

func openSMS(tx *sql.Tx) (*smsDB, error) {
	s := &smsDB{
		tx:          tx,
		columns:     map[string]map[string]bool{},
		nanoseconds: true,
		guidPrefix:  serviceSMS,
		country:     "us",
		handles:     map[string]int64{},
		groups:      map[string]groupChat{},
	}
	for _, table := range []string{"handle", "chat", "message", "attachment", "chat_handle_join", "chat_message_join", "message_attachment_join"} {
		if err := s.loadColumns(table); err != nil {
			return nil, err
		}
	}

	var maxDate sql.NullInt64
	if err := tx.QueryRow(`SELECT MAX(date) FROM message`).Scan(&maxDate); err != nil {
		return nil, err
	}
	// Old iOS versions stored whole seconds; a seconds value this large would be year 33000.
	if maxDate.Valid && maxDate.Int64 > 0 && maxDate.Int64 < 1e12 {
		s.nanoseconds = false
	}
	var anyStyle int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM chat WHERE guid LIKE 'any;%'`).Scan(&anyStyle); err != nil {
		return nil, err
	}
	if anyStyle > 0 {
		s.guidPrefix = "any"
	}
	// Each of these is optional: an empty database simply has nothing to copy.
	_ = tx.QueryRow(`SELECT country FROM handle WHERE country IS NOT NULL GROUP BY country ORDER BY COUNT(*) DESC LIMIT 1`).Scan(&s.country)
	_ = tx.QueryRow(`SELECT account, account_guid FROM message WHERE service = ? AND account IS NOT NULL ORDER BY date DESC LIMIT 1`, serviceSMS).Scan(&s.account, &s.accountGUID)
	_ = tx.QueryRow(`SELECT account_id, account_login FROM chat WHERE service_name = ? AND account_id IS NOT NULL ORDER BY ROWID DESC LIMIT 1`, serviceSMS).Scan(&s.accountID, &s.login)

	if err := s.loadGroups(); err != nil {
		return nil, err
	}
	return s, s.dropTriggers()
}

// dropTriggers removes the database's triggers for the duration of the
// import. Several of them call functions that only exist inside iOS, so any
// statement that fires them fails here. finish puts them back unchanged.
func (s *smsDB) dropTriggers() error {
	rows, err := s.tx.Query(`SELECT name, sql FROM sqlite_master WHERE type = 'trigger' AND sql IS NOT NULL`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var t trigger
		if err := rows.Scan(&t.name, &t.sql); err != nil {
			rows.Close()
			return err
		}
		s.triggers = append(s.triggers, t)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, t := range s.triggers {
		if _, err := s.tx.Exec(`DROP TRIGGER "` + strings.ReplaceAll(t.name, `"`, `""`) + `"`); err != nil {
			return err
		}
	}
	return nil
}

func (s *smsDB) finish() error {
	for _, t := range s.triggers {
		if _, err := s.tx.Exec(t.sql); err != nil {
			return fmt.Errorf("restore trigger %s: %w", t.name, err)
		}
	}
	return nil
}

// insert writes a row, silently leaving out columns this iOS version lacks.
func (s *smsDB) insert(table string, values map[string]any) (int64, error) {
	var cols []string
	for col := range values {
		if s.columns[table][col] {
			cols = append(cols, col)
		}
	}
	sort.Strings(cols)
	args := make([]any, len(cols))
	for i, col := range cols {
		args[i] = values[col]
		// The schema uses integer flags; do not rely on the driver's bool mapping.
		if b, ok := args[i].(bool); ok {
			args[i] = 0
			if b {
				args[i] = 1
			}
		}
	}
	query := fmt.Sprintf(`INSERT INTO %s (%s) VALUES (%s)`, table, strings.Join(cols, ", "),
		strings.TrimSuffix(strings.Repeat("?, ", len(cols)), ", "))
	res, err := s.tx.Exec(query, args...)
	if err != nil {
		return 0, fmt.Errorf("insert into %s: %w", table, err)
	}
	return res.LastInsertId()
}

func (s *smsDB) date(t time.Time) int64 {
	if s.nanoseconds {
		return t.UnixNano() - appleEpochOffset*int64(time.Second)
	}
	return t.Unix() - appleEpochOffset
}

func (s *smsDB) fromDate(d int64) time.Time {
	if s.nanoseconds {
		return time.Unix(appleEpochOffset, d)
	}
	return time.Unix(appleEpochOffset+d, 0)
}

func (s *smsDB) handle(id string) (int64, error) {
	if rowID, ok := s.handles[id]; ok {
		return rowID, nil
	}
	var rowID int64
	err := s.tx.QueryRow(`SELECT ROWID FROM handle WHERE id = ? AND service = ?`, id, serviceSMS).Scan(&rowID)
	if errors.Is(err, sql.ErrNoRows) {
		rowID, err = s.insert("handle", map[string]any{
			"id":                 id,
			"country":            s.country,
			"service":            serviceSMS,
			"uncanonicalized_id": id,
		})
	}
	if err != nil {
		return 0, err
	}
	s.handles[id] = rowID
	return rowID, nil
}

func groupKey(ids []string) string {
	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)
	return strings.Join(sorted, "\n")
}

// loadGroups indexes existing text-message group chats by their member list,
// so history is merged into a group that is already on the phone.
func (s *smsDB) loadGroups() error {
	rows, err := s.tx.Query(`
		SELECT c.ROWID, COALESCE(c.room_name, c.chat_identifier), h.id
		FROM chat c
		JOIN chat_handle_join j ON j.chat_id = c.ROWID
		JOIN handle h ON h.ROWID = j.handle_id
		WHERE c.style = ? AND c.service_name != 'iMessage'
		ORDER BY c.ROWID`, chatStyleGroup)
	if err != nil {
		return err
	}
	defer rows.Close()
	members := map[int64][]string{}
	rooms := map[int64]string{}
	var order []int64
	for rows.Next() {
		var chatID int64
		var room, member string
		if err := rows.Scan(&chatID, &room, &member); err != nil {
			return err
		}
		if _, ok := rooms[chatID]; !ok {
			order = append(order, chatID)
		}
		rooms[chatID] = room
		members[chatID] = append(members[chatID], member)
	}
	for _, chatID := range order {
		s.groups[groupKey(members[chatID])] = groupChat{id: chatID, roomName: rooms[chatID]}
	}
	return rows.Err()
}

func (s *smsDB) newChat(values map[string]any) (int64, error) {
	base := map[string]any{
		"state":            3,
		"account_id":       s.accountID,
		"account_login":    s.login,
		"service_name":     serviceSMS,
		"is_archived":      0,
		"is_filtered":      0,
		"successful_query": 1,
	}
	for k, v := range values {
		base[k] = v
	}
	return s.insert("chat", base)
}

func (s *smsDB) joinHandle(chatID, handleID int64) error {
	_, err := s.insert("chat_handle_join", map[string]any{"chat_id": chatID, "handle_id": handleID})
	return err
}

// directChat finds or creates the one-to-one chat with a contact.
func (s *smsDB) directChat(contact string) (chatID, handleID int64, isNew bool, err error) {
	if handleID, err = s.handle(contact); err != nil {
		return
	}
	guid := s.guidPrefix + ";-;" + contact
	err = s.tx.QueryRow(`SELECT ROWID FROM chat WHERE guid = ?`, guid).Scan(&chatID)
	if !errors.Is(err, sql.ErrNoRows) {
		return
	}
	chatID, err = s.newChat(map[string]any{
		"guid":            guid,
		"style":           chatStyleDirect,
		"chat_identifier": contact,
	})
	if err == nil {
		isNew = true
		err = s.joinHandle(chatID, handleID)
	}
	return
}

// groupChatFor finds or creates the group chat with exactly these members.
func (s *smsDB) groupChatFor(sourceID, displayName string, members []string) (chat groupChat, isNew bool, err error) {
	key := groupKey(members)
	if existing, ok := s.groups[key]; ok {
		return existing, false, nil
	}
	// Group identifiers are "chat" followed by a long number; derive it from
	// the source conversation so a second import finds the same chat.
	sum := sha1.Sum([]byte("mv2ios:group:" + sourceID))
	var n uint64
	for _, b := range sum[:8] {
		n = n<<8 | uint64(b)
	}
	room := fmt.Sprintf("chat%018d", n%1e18)
	guid := s.guidPrefix + ";+;" + room
	err = s.tx.QueryRow(`SELECT ROWID FROM chat WHERE guid = ?`, guid).Scan(&chat.id)
	chat.roomName = room
	if errors.Is(err, sql.ErrNoRows) {
		isNew = true
		values := map[string]any{
			"guid":            guid,
			"style":           chatStyleGroup,
			"chat_identifier": room,
			"room_name":       room,
			"group_id":        derivedGUID("group", sourceID),
		}
		if displayName != "" {
			values["display_name"] = displayName
		}
		if chat.id, err = s.newChat(values); err != nil {
			return
		}
		for _, member := range members {
			var handleID int64
			if handleID, err = s.handle(member); err != nil {
				return
			}
			if err = s.joinHandle(chat.id, handleID); err != nil {
				return
			}
		}
	}
	if err == nil {
		s.groups[key] = chat
	}
	return
}

// existingIndex records what a chat already holds, to spot messages that
// "Move to iOS" (or an earlier run) has already brought across.
type existingIndex map[string][]time.Time

func comparableText(text string) string {
	return strings.TrimSpace(strings.ReplaceAll(text, attachmentPlaceholder, ""))
}

func indexKey(fromMe bool, text string) string {
	return fmt.Sprintf("%t|%s", fromMe, comparableText(text))
}

func (s *smsDB) existingMessages(chatID int64) (existingIndex, error) {
	rows, err := s.tx.Query(`
		SELECT m.is_from_me, m.date, COALESCE(m.text, '')
		FROM message m JOIN chat_message_join j ON j.message_id = m.ROWID
		WHERE j.chat_id = ?`, chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	idx := existingIndex{}
	for rows.Next() {
		var fromMe bool
		var date int64
		var text string
		if err := rows.Scan(&fromMe, &date, &text); err != nil {
			return nil, err
		}
		key := indexKey(fromMe, text)
		idx[key] = append(idx[key], s.fromDate(date))
	}
	return idx, rows.Err()
}

func (idx existingIndex) contains(fromMe bool, text string, at time.Time) bool {
	for _, t := range idx[indexKey(fromMe, text)] {
		if d := t.Sub(at); d >= -duplicateWindow && d <= duplicateWindow {
			return true
		}
	}
	return false
}

func (s *smsDB) hasMessageGUID(guid string) (bool, error) {
	var n int
	err := s.tx.QueryRow(`SELECT COUNT(*) FROM message WHERE guid = ?`, guid).Scan(&n)
	return n > 0, err
}

type newMessage struct {
	guid     string
	chatID   int64
	handleID int64 // 0 for the owner's own messages in a group
	roomName string
	fromMe   bool
	at       time.Time
	text     string
	subject  string
	ownerID  string
	// attachments are counted before the row is written because the text
	// carries one placeholder for each.
	attachments int
}

func (s *smsDB) addMessage(m newMessage) (int64, error) {
	date := s.date(m.at)
	values := map[string]any{
		"guid":                  m.guid,
		"text":                  strings.Repeat(attachmentPlaceholder, m.attachments) + m.text,
		"handle_id":             m.handleID,
		"service":               serviceSMS,
		"account":               s.account,
		"account_guid":          s.accountGUID,
		"date":                  date,
		"is_finished":           1,
		"is_from_me":            m.fromMe,
		"cache_has_attachments": m.attachments > 0,
	}
	if m.subject != "" {
		values["subject"] = m.subject
	}
	if m.roomName != "" {
		values["cache_roomnames"] = m.roomName
	}
	if m.ownerID != "" {
		values["destination_caller_id"] = m.ownerID
	}
	if m.fromMe {
		values["is_sent"] = 1
		values["is_delivered"] = 1
		values["date_delivered"] = date
	} else {
		// Imported history is marked read; otherwise the phone would show
		// every old message as new.
		values["is_read"] = 1
		values["date_read"] = date
	}
	rowID, err := s.insert("message", values)
	if err != nil {
		return 0, err
	}
	_, err = s.insert("chat_message_join", map[string]any{"chat_id": m.chatID, "message_id": rowID, "message_date": date})
	return rowID, err
}

type newAttachment struct {
	guid      string
	messageID int64
	// devicePath is where the file lives on the phone, relative to the home folder.
	devicePath string
	name       string
	mimeType   string
	size       int64
	outgoing   bool
	at         time.Time
}

func (s *smsDB) addAttachment(a newAttachment) error {
	rowID, err := s.insert("attachment", map[string]any{
		"guid":          a.guid,
		"original_guid": a.guid,
		// Unlike message dates, this one is always whole seconds.
		"created_date":    a.at.Unix() - appleEpochOffset,
		"start_date":      0,
		"filename":        "~/" + a.devicePath,
		"uti":             utiFor(a.mimeType, a.name),
		"mime_type":       a.mimeType,
		"transfer_state":  5,
		"is_outgoing":     a.outgoing,
		"transfer_name":   a.name,
		"total_bytes":     a.size,
		"is_sticker":      0,
		"hide_attachment": 0,
	})
	if err != nil {
		return err
	}
	_, err = s.insert("message_attachment_join", map[string]any{"message_id": a.messageID, "attachment_id": rowID})
	return err
}

var utiByMime = map[string]string{
	"image/jpeg":      "public.jpeg",
	"image/jpg":       "public.jpeg",
	"image/png":       "public.png",
	"image/gif":       "com.compuserve.gif",
	"image/heic":      "public.heic",
	"image/webp":      "org.webmproject.webp",
	"image/bmp":       "com.microsoft.bmp",
	"video/mp4":       "public.mpeg-4",
	"video/quicktime": "com.apple.quicktime-movie",
	"video/3gpp":      "public.3gpp",
	"video/3gpp2":     "public.3gpp2",
	"audio/mpeg":      "public.mp3",
	"audio/mp3":       "public.mp3",
	"audio/mp4":       "public.mpeg-4-audio",
	"audio/aac":       "public.aac-audio",
	"audio/amr":       "org.3gpp.adaptive-multi-rate-audio",
	"text/vcard":      "public.vcard",
	"text/x-vcard":    "public.vcard",
	"text/plain":      "public.plain-text",
	"application/pdf": "com.adobe.pdf",
}

func utiFor(mimeType, name string) string {
	if uti, ok := utiByMime[strings.ToLower(mimeType)]; ok {
		return uti
	}
	switch class, _, _ := strings.Cut(mimeType, "/"); class {
	case "image":
		return "public.image"
	case "video":
		return "public.movie"
	case "audio":
		return "public.audio"
	}
	return "public.data"
}
