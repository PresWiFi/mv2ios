package iosbackup

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/preswifi/mv2ios/internal/archive"
)

// selfArchive writes conversations where the owner also appears as an
// ordinary participant, the way Google Messages exports them. With fixed
// false, those extra entries carry details that are not recognisable as the
// owner, which reproduces what older versions of mv2ios imported.
func selfArchive(t *testing.T, dir string, fixed bool) {
	t.Helper()
	oldNumber, oldName := "+15558887777", "Someone Else"
	if fixed {
		oldNumber, oldName = "(555) 999-0000", "Blake Riding"
	}
	me := archive.Participant{ID: "me", Number: "+15559990000", Name: "Blake Riding", IsMe: true}
	meOld := archive.Participant{ID: "me2", Number: oldNumber, Name: "Blake Riding"}
	meEmail := archive.Participant{ID: "me3", Number: "blake.old@gmail.com", Name: oldName}
	alice := archive.Participant{ID: "a", Number: "+15550001111", Name: "Alice Smith"}
	bob := archive.Participant{ID: "b", Number: "+15550002222", Name: "Bob Jones"}
	unknown := archive.Participant{ID: "u", Number: "ʼUNKNOWN_SENDER!ʼ", Name: "Unknown sender"}
	at := firstMessage.Add(24 * time.Hour)
	convs := []*archive.Conversation{
		{
			ID: "d1", Name: "Alice Smith", Protocol: archive.ProtocolSMS,
			Participants: []archive.Participant{me, alice, meOld},
			Messages: []archive.Message{
				{ID: "d1m1", Timestamp: at, SenderID: "a", Text: "are you coming", Protocol: archive.ProtocolSMS},
				{ID: "d1m2", Timestamp: at.Add(time.Minute), SenderID: "me2", Text: "on my way", Protocol: archive.ProtocolMMS},
			},
		},
		{
			ID: "g1", Name: "Weekend", IsGroup: true, Protocol: archive.ProtocolRCS,
			Participants: []archive.Participant{me, alice, bob, unknown, meEmail},
			Messages: []archive.Message{
				{ID: "g1m1", Timestamp: at.Add(time.Hour), SenderID: "b", Text: "who has the keys", Protocol: archive.ProtocolRCS},
				{ID: "g1m2", Timestamp: at.Add(2 * time.Hour), SenderID: "me3", Text: "I do", Protocol: archive.ProtocolRCS},
			},
		},
	}
	for _, c := range convs {
		if err := archive.WriteConversation(dir, c); err != nil {
			t.Fatal(err)
		}
	}
}

func TestOwnerEntriesAreFixedAndVerified(t *testing.T) {
	backup := makeBackup(t)
	smsPath := filepath.Join(backup, storedPath(fileID(smsDomain, smsRelativePath)))
	arch := t.TempDir()
	undoRoot := t.TempDir()

	// The first import goes wrong the way older versions did.
	selfArchive(t, arch, false)
	if _, err := Inject(backup, arch, Options{UndoRoot: undoRoot}); err != nil {
		t.Fatal(err)
	}
	// The chat with Alice became a group of three, and the group has four members.
	if n := queryInt(t, smsPath, `SELECT COUNT(*) FROM chat WHERE style = 43`); n != 2 {
		t.Fatalf("set-up: %d group chats, want 2", n)
	}

	selfArchive(t, arch, true)
	before, err := Verify(backup, arch, VerifyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := before.SelfAddresses; len(got) != 1 || got[0] != "blake.old@gmail.com" {
		t.Errorf("self addresses = %q, want the old email", got)
	}
	tot := before.Totals
	// d1m1 and d1m2 sit in the made-up group; d1m2 and g1m2 show as received.
	if tot.Expected != 4 || tot.WrongChat != 2 || tot.WrongDirection != 2 || tot.Imported != 1 {
		t.Errorf("before the fix: %+v", tot)
	}

	res, err := Inject(backup, arch, Options{UndoRoot: undoRoot})
	if err != nil {
		t.Fatal(err)
	}
	if res.Moved != 3 || res.RemovedChats != 1 || res.Messages != 0 {
		t.Errorf("fix run = %+v, want 3 moved, 1 chat removed, nothing new", res)
	}
	checks := []struct {
		query string
		want  int
	}{
		// Only the real group is left, with Alice and Bob and nobody else.
		{`SELECT COUNT(*) FROM chat WHERE style = 43`, 1},
		{`SELECT COUNT(*) FROM chat_handle_join j JOIN chat c ON c.ROWID = j.chat_id WHERE c.style = 43`, 2},
		{`SELECT COUNT(*) FROM handle h JOIN chat_handle_join j ON j.handle_id = h.ROWID WHERE h.id LIKE '%UNKNOWN%' OR h.id = '+15559990000'`, 0},
		// The chat with Alice is one-to-one again and holds both messages.
		{`SELECT COUNT(*) FROM chat_message_join j JOIN chat c ON c.ROWID = j.chat_id WHERE c.guid = 'SMS;-;+15550001111'`, 3},
		{`SELECT COUNT(*) FROM message WHERE text IN ('on my way', 'I do') AND is_from_me = 1 AND is_sent = 1 AND is_read = 0`, 2},
		{`SELECT COUNT(*) FROM message WHERE text = 'I do' AND handle_id = 0`, 1},
	}
	for _, c := range checks {
		if got := queryInt(t, smsPath, c.query); got != c.want {
			t.Errorf("%s = %d, want %d", c.query, got, c.want)
		}
	}

	after, err := Verify(backup, arch, VerifyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if after.Totals.Problems() != 0 || after.Totals.Imported != 4 {
		t.Errorf("after the fix: %+v", after.Totals)
	}
	for _, r := range after.Conversations {
		if r.ID == "g1" && (len(r.Dropped) != 1 || r.Members != 2 || r.FromSelfEntry != 1) {
			t.Errorf("group report = %+v", r)
		}
	}

	again, err := Inject(backup, arch, Options{DryRun: true, UndoRoot: undoRoot})
	if err != nil {
		t.Fatal(err)
	}
	if again.Moved != 0 || again.RemovedChats != 0 || again.Duplicates != 4 {
		t.Errorf("third run = %+v, want nothing left to fix", again)
	}
}

func TestVerifyFindsMissingAndBlank(t *testing.T) {
	backup := makeBackup(t)
	arch := makeArchive(t)
	before, err := Verify(backup, arch, VerifyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// "hello" was on the phone already; the other four are not there yet.
	if before.Totals.AlreadyThere != 1 || before.Totals.Missing != 4 {
		t.Errorf("before inject: %+v", before.Totals)
	}
	if _, err := Inject(backup, arch, Options{UndoRoot: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	smsPath := filepath.Join(backup, storedPath(fileID(smsDomain, smsRelativePath)))
	db, err := openSQLite(smsPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE message SET attributedBody = NULL WHERE text = 'me'`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	after, err := Verify(backup, arch, VerifyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if after.Totals.Missing != 0 || after.Totals.BlankBubble != 1 || after.Totals.Imported != 3 || after.Totals.MissingAttachments != 0 {
		t.Errorf("after inject: %+v", after.Totals)
	}
}
