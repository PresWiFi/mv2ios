package iosbackup

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"howett.net/plist"

	"github.com/preswifi/mv2ios/internal/archive"
)

// A cut-down copy of the real Messages schema. The trigger calls a function
// that only exists on iOS, like several triggers in the real database do.
const smsSchema = `
CREATE TABLE handle (ROWID INTEGER PRIMARY KEY AUTOINCREMENT UNIQUE, id TEXT NOT NULL, country TEXT, service TEXT NOT NULL, uncanonicalized_id TEXT, person_centric_id TEXT, UNIQUE (id, service));
CREATE TABLE chat (ROWID INTEGER PRIMARY KEY AUTOINCREMENT, guid TEXT UNIQUE NOT NULL, style INTEGER, state INTEGER, account_id TEXT, properties BLOB, chat_identifier TEXT, service_name TEXT, room_name TEXT, account_login TEXT, is_archived INTEGER DEFAULT 0, last_addressed_handle TEXT, display_name TEXT, group_id TEXT, is_filtered INTEGER DEFAULT 0, successful_query INTEGER);
CREATE TABLE message (ROWID INTEGER PRIMARY KEY AUTOINCREMENT, guid TEXT UNIQUE NOT NULL, text TEXT, handle_id INTEGER DEFAULT 0, subject TEXT, attributedBody BLOB, service TEXT, account TEXT, account_guid TEXT, date INTEGER, date_read INTEGER, date_delivered INTEGER, is_delivered INTEGER DEFAULT 0, is_finished INTEGER DEFAULT 0, is_from_me INTEGER DEFAULT 0, is_read INTEGER DEFAULT 0, is_sent INTEGER DEFAULT 0, cache_has_attachments INTEGER DEFAULT 0, cache_roomnames TEXT, destination_caller_id TEXT);
CREATE TABLE attachment (ROWID INTEGER PRIMARY KEY AUTOINCREMENT, guid TEXT UNIQUE NOT NULL, created_date INTEGER DEFAULT 0, start_date INTEGER DEFAULT 0, filename TEXT, uti TEXT, mime_type TEXT, transfer_state INTEGER DEFAULT 0, is_outgoing INTEGER DEFAULT 0, transfer_name TEXT, total_bytes INTEGER DEFAULT 0, is_sticker INTEGER DEFAULT 0, hide_attachment INTEGER DEFAULT 0, original_guid TEXT UNIQUE NOT NULL);
CREATE TABLE chat_handle_join (chat_id INTEGER, handle_id INTEGER, UNIQUE(chat_id, handle_id));
CREATE TABLE chat_message_join (chat_id INTEGER, message_id INTEGER, message_date INTEGER DEFAULT 0, PRIMARY KEY (chat_id, message_id));
CREATE TABLE message_attachment_join (message_id INTEGER, attachment_id INTEGER, UNIQUE(message_id, attachment_id));
CREATE TRIGGER after_insert_on_message_attachment_join AFTER INSERT ON message_attachment_join BEGIN SELECT ios_only_function(NEW.message_id); END;
`

func mbFileBlob(t *testing.T, relativePath string, size int) []byte {
	t.Helper()
	blob, err := plist.Marshal(map[string]any{
		"$version":  100000,
		"$archiver": "NSKeyedArchiver",
		"$top":      map[string]any{"root": plist.UID(1)},
		"$objects": []any{
			"$null",
			map[string]any{
				"$class": plist.UID(3), "RelativePath": plist.UID(2), "Size": size, "Mode": 33188,
				"InodeNumber": 5, "UserID": 501, "GroupID": 501, "LastModified": 1700000000,
				"Birth": 1700000000, "LastStatusChange": 1700000000, "ProtectionClass": 3, "Flags": 0,
			},
			relativePath,
			map[string]any{"$classname": "MBFile", "$classes": []any{"MBFile", "NSObject"}},
		},
	}, plist.BinaryFormat)
	if err != nil {
		t.Fatal(err)
	}
	return blob
}

func writePlist(t *testing.T, path string, v any) {
	t.Helper()
	data, err := plist.Marshal(v, plist.XMLFormat)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

var firstMessage = time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC)

func makeBackup(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "00008030-TESTDEVICE")
	smsPath := filepath.Join(dir, storedPath(fileID(smsDomain, smsRelativePath)))
	if err := os.MkdirAll(filepath.Dir(smsPath), 0o700); err != nil {
		t.Fatal(err)
	}
	writePlist(t, filepath.Join(dir, "Manifest.plist"), map[string]any{"IsEncrypted": false})
	writePlist(t, filepath.Join(dir, "Info.plist"), map[string]any{"Device Name": "Test iPhone", "Product Version": "18.0"})

	sms, err := openSQLite(smsPath)
	if err != nil {
		t.Fatal(err)
	}
	date := firstMessage.UnixNano() - appleEpochOffset*int64(time.Second)
	for _, stmt := range []string{
		smsSchema,
		`INSERT INTO handle (id, country, service) VALUES ('+15550001111', 'us', 'SMS')`,
		`INSERT INTO chat (guid, style, chat_identifier, service_name) VALUES ('SMS;-;+15550001111', 45, '+15550001111', 'SMS')`,
		`INSERT INTO chat_handle_join VALUES (1, 1)`,
	} {
		if _, err := sms.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := sms.Exec(`INSERT INTO message (guid, text, handle_id, service, date) VALUES ('EXISTING', 'hello', 1, 'SMS', ?)`, date); err != nil {
		t.Fatal(err)
	}
	if _, err := sms.Exec(`INSERT INTO chat_message_join VALUES (1, 1, ?)`, date); err != nil {
		t.Fatal(err)
	}
	if err := sms.Close(); err != nil {
		t.Fatal(err)
	}

	manifest, err := openSQLite(filepath.Join(dir, "Manifest.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manifest.Exec(`CREATE TABLE Files (fileID TEXT PRIMARY KEY, domain TEXT, relativePath TEXT, flags INTEGER, file BLOB)`); err != nil {
		t.Fatal(err)
	}
	if _, err := manifest.Exec(`INSERT INTO Files VALUES (?, ?, ?, 1, ?)`,
		fileID(smsDomain, smsRelativePath), smsDomain, smsRelativePath, mbFileBlob(t, smsRelativePath, 1)); err != nil {
		t.Fatal(err)
	}
	if err := manifest.Close(); err != nil {
		t.Fatal(err)
	}
	return dir
}

func makeArchive(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "media", "c1"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "media", "c1", "m3-0.jpg"), []byte("jpeg bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	me := archive.Participant{ID: "me", Number: "+15559990000", IsMe: true}
	alice := archive.Participant{ID: "a", Number: "(555) 000-1111", Name: "Alice Smith"}
	bob := archive.Participant{ID: "b", Number: "+15550002222", Name: "Bob Jones"}
	convs := []*archive.Conversation{
		{
			ID: "c1", Name: "Alice Smith", Protocol: archive.ProtocolRCS,
			Participants: []archive.Participant{me, alice},
			Messages: []archive.Message{
				// Already on the phone, two seconds apart.
				{ID: "m1", Timestamp: firstMessage.Add(2 * time.Second), SenderID: "a", Text: "hello", Protocol: archive.ProtocolSMS},
				{ID: "m2", Timestamp: firstMessage.Add(time.Minute), SenderID: "me", FromMe: true, Text: "hi over RCS", Protocol: archive.ProtocolRCS},
				{ID: "m3", Timestamp: firstMessage.Add(2 * time.Minute), SenderID: "a", Protocol: archive.ProtocolRCS, Attachments: []archive.Attachment{
					{Path: "media/c1/m3-0.jpg", Name: "IMG_1.jpg", MimeType: "image/jpeg"},
					{Name: "lost.jpg", Error: "download failed"},
				}},
			},
		},
		{
			ID: "c2", Name: "Camping trip", IsGroup: true, Protocol: archive.ProtocolRCS,
			Participants: []archive.Participant{me, alice, bob},
			Messages: []archive.Message{
				{ID: "g1", Timestamp: firstMessage.Add(time.Hour), SenderID: "b", Text: "who is bringing the tent", Protocol: archive.ProtocolRCS},
				{ID: "g2", Timestamp: firstMessage.Add(2 * time.Hour), SenderID: "me", FromMe: true, Text: "me", Protocol: archive.ProtocolRCS},
			},
		},
	}
	for _, c := range convs {
		if err := archive.WriteConversation(dir, c); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func queryInt(t *testing.T, dbPath, query string, args ...any) int {
	t.Helper()
	db, err := openSQLite(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

func TestInjectAndUndo(t *testing.T) {
	backup := makeBackup(t)
	arch := makeArchive(t)
	undoRoot := t.TempDir()
	smsPath := filepath.Join(backup, storedPath(fileID(smsDomain, smsRelativePath)))
	original, err := os.ReadFile(smsPath)
	if err != nil {
		t.Fatal(err)
	}

	dry, err := Inject(backup, arch, Options{DryRun: true, UndoRoot: undoRoot})
	if err != nil {
		t.Fatal(err)
	}
	if after, _ := os.ReadFile(smsPath); !bytes.Equal(original, after) || dry.Messages != 4 || dry.UndoDir != "" {
		t.Fatalf("dry run changed the backup or miscounted: %+v", dry)
	}

	res, err := Inject(backup, arch, Options{UndoRoot: undoRoot})
	if err != nil {
		t.Fatal(err)
	}
	want := Result{Conversations: 2, NewChats: 1, Messages: 4, Duplicates: 1, Attachments: 1, MissingAttachments: 1, UndoDir: res.UndoDir}
	if res != want {
		t.Fatalf("got %+v, want %+v", res, want)
	}

	checks := []struct {
		query string
		want  int
	}{
		{`SELECT COUNT(*) FROM message`, 5},
		{`SELECT COUNT(*) FROM chat`, 2},
		{`SELECT COUNT(*) FROM handle`, 2},
		{`SELECT COUNT(*) FROM sqlite_master WHERE type = 'trigger'`, 1},
		// The RCS reply joined the existing thread with Alice.
		{`SELECT COUNT(*) FROM chat_message_join WHERE chat_id = 1`, 3},
		{`SELECT COUNT(*) FROM chat_handle_join j JOIN chat c ON c.ROWID = j.chat_id WHERE c.style = 43 AND c.display_name = 'Camping trip'`, 2},
		{`SELECT COUNT(*) FROM message WHERE cache_roomnames LIKE 'chat%' AND is_from_me = 1 AND handle_id = 0`, 1},
		{`SELECT COUNT(*) FROM message WHERE cache_has_attachments = 1 AND text = char(65532)`, 1},
		{`SELECT COUNT(*) FROM attachment WHERE filename LIKE '~/Library/SMS/Attachments/%/IMG_1.jpg' AND uti = 'public.jpeg' AND total_bytes = 10`, 1},
		{`SELECT COUNT(*) FROM message WHERE is_from_me = 0 AND is_read = 0 AND guid != 'EXISTING'`, 0},
	}
	for _, c := range checks {
		if got := queryInt(t, smsPath, c.query); got != c.want {
			t.Errorf("%s = %d, want %d", c.query, got, c.want)
		}
	}

	manifestPath := filepath.Join(backup, "Manifest.db")
	st, _ := os.Stat(smsPath)
	db, err := openSQLite(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var blob []byte
	if err := db.QueryRow(`SELECT file FROM Files WHERE relativePath = ?`, smsRelativePath).Scan(&blob); err != nil {
		t.Fatal(err)
	}
	if rec, err := parseMBFile(blob); err != nil || rec.fields["Size"] != uint64(st.Size()) {
		t.Errorf("manifest size for sms.db = %v (%v), want %d", rec.fields["Size"], err, st.Size())
	}
	var id, relPath string
	if err := db.QueryRow(`SELECT fileID, relativePath, file FROM Files WHERE domain = 'MediaDomain' AND flags = 1`).Scan(&id, &relPath, &blob); err != nil {
		t.Fatal(err)
	}
	rec, err := parseMBFile(blob)
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.objects[rec.fields["RelativePath"].(plist.UID)]; got != relPath || rec.fields["Size"] != uint64(10) {
		t.Errorf("attachment record = %v size %v, want %s size 10", got, rec.fields["Size"], relPath)
	}
	// Library, Library/SMS, Library/SMS/Attachments and the three folders below it.
	var dirs int
	if err := db.QueryRow(`SELECT COUNT(*) FROM Files WHERE domain = 'MediaDomain' AND flags = 2`).Scan(&dirs); err != nil || dirs != 6 {
		t.Errorf("directory records = %d (%v), want 6", dirs, err)
	}
	db.Close()
	attachmentPath := filepath.Join(backup, storedPath(id))
	if data, _ := os.ReadFile(attachmentPath); string(data) != "jpeg bytes" {
		t.Errorf("attachment contents = %q", data)
	}

	again, err := Inject(backup, arch, Options{UndoRoot: undoRoot})
	if err != nil {
		t.Fatal(err)
	}
	if again.Messages != 0 || again.Duplicates != 5 {
		t.Errorf("second run = %+v, want no new messages and 5 duplicates", again)
	}

	latest, err := LatestUndo(undoRoot, filepath.Base(backup))
	if err != nil || latest != res.UndoDir {
		t.Fatalf("LatestUndo = %q, %v; want %q", latest, err, res.UndoDir)
	}
	if err := Undo(latest); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.ReadFile(smsPath); !bytes.Equal(original, after) {
		t.Error("undo did not restore the original Messages database")
	}
	if _, err := os.Stat(attachmentPath); !os.IsNotExist(err) {
		t.Error("undo left the attachment in the backup")
	}
}

func TestEncryptedBackupRefused(t *testing.T) {
	backup := makeBackup(t)
	writePlist(t, filepath.Join(backup, "Manifest.plist"), map[string]any{"IsEncrypted": true})
	if _, err := Inject(backup, makeArchive(t), Options{UndoRoot: t.TempDir()}); err != ErrEncrypted {
		t.Fatalf("err = %v, want ErrEncrypted", err)
	}
}

func TestNormalizeNumber(t *testing.T) {
	for _, c := range []struct{ in, owner, want string }{
		{"(555) 000-1111", "+15559990000", "+15550001111"},
		{"1 555 000 1111", "+15559990000", "+15550001111"},
		{"+44 20 7946 0000", "+15559990000", "+442079460000"},
		{"555-000-1111", "+447000000000", "5550001111"},
		{"72345", "+15559990000", "72345"},
		{"AMAZON", "+15559990000", "AMAZON"},
		{"Someone@Example.com", "", "someone@example.com"},
	} {
		if got := normalizeNumber(c.in, c.owner); got != c.want {
			t.Errorf("normalizeNumber(%q, %q) = %q, want %q", c.in, c.owner, got, c.want)
		}
	}
}
