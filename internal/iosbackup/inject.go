package iosbackup

import (
	"crypto/sha1"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/preswifi/mv2ios/internal/archive"
)

type Options struct {
	// DryRun does all the work on scratch copies and reports what would
	// change, without touching the backup.
	DryRun bool
	// UndoRoot is where the original files are kept so Undo can put them back.
	UndoRoot string
	Logf     func(format string, args ...any)
}

type Result struct {
	Conversations        int
	NewChats             int
	SkippedConversations int
	Messages             int
	Duplicates           int
	Attachments          int
	MissingAttachments   int
	// UndoDir is empty when nothing was written to the backup.
	UndoDir string
}

type journal struct {
	BackupPath string    `json:"backup_path"`
	Created    time.Time `json:"created"`
	// Replaced files have their original saved next to the journal under the same name.
	Replaced []string `json:"replaced"`
	Added    []string `json:"added"`
}

const journalName = "journal.json"

// normalizeNumber makes a phone number comparable with the ones Messages
// stores. ownerNumber is used to guess the country of numbers written without one.
func normalizeNumber(number, ownerNumber string) string {
	number = strings.TrimSpace(number)
	if strings.Contains(number, "@") {
		return strings.ToLower(number)
	}
	var b strings.Builder
	for i, r := range number {
		if (r >= '0' && r <= '9') || (r == '+' && i == 0) {
			b.WriteRune(r)
		}
	}
	digits := b.String()
	if digits == "" {
		// Alphanumeric senders ("AMAZON") have no digits to keep.
		return number
	}
	if strings.HasPrefix(ownerNumber, "+1") && !strings.HasPrefix(digits, "+") {
		switch {
		case len(digits) == 10:
			return "+1" + digits
		case len(digits) == 11 && digits[0] == '1':
			return "+" + digits
		}
	}
	return digits
}

func ownerNumber(convs []*archive.Conversation) string {
	for _, c := range convs {
		for _, p := range c.Participants {
			if p.IsMe && strings.HasPrefix(p.Number, "+") {
				return p.Number
			}
		}
	}
	return ""
}

// groupDisplayName returns the group's name only if someone actually named
// it. Unnamed groups arrive labelled with a list of their members, which
// Messages generates by itself and should not be stored as a title.
func groupDisplayName(c *archive.Conversation) string {
	if c.Name == "" {
		return ""
	}
	for _, p := range c.Others() {
		first, _, _ := strings.Cut(p.Name, " ")
		if (first == "" || !strings.Contains(c.Name, first)) && !strings.Contains(c.Name, p.Number) {
			return c.Name
		}
	}
	return ""
}

func attachmentName(a archive.Attachment) string {
	name := path.Base(strings.ReplaceAll(a.Name, "\\", "/"))
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || strings.ContainsRune(`/:*?"<>|`, r) {
			return '_'
		}
		return r
	}, name)
	ext := path.Ext(a.Path)
	if name == "" || name == "." || name == "_" {
		return "attachment" + ext
	}
	if path.Ext(name) == "" {
		name += ext
	}
	return name
}

// attachmentDevicePath mirrors the folder layout Messages uses on the phone.
func attachmentDevicePath(guid, name string) string {
	sum := sha1.Sum([]byte(guid))
	return path.Join(attachmentsRoot, fmt.Sprintf("%02x", sum[0]), fmt.Sprintf("%02d", sum[1]%16), guid, name)
}

type pendingCopy struct{ src, dst string }

// Inject adds the conversations in archiveDir to the Messages database inside
// the backup at backupDir.
func Inject(backupDir, archiveDir string, opts Options) (Result, error) {
	var res Result
	logf := opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	info, err := ReadInfo(backupDir)
	if err != nil {
		return res, err
	}
	if info.Encrypted {
		return res, ErrEncrypted
	}
	convs, err := archive.Load(archiveDir)
	if err != nil {
		return res, err
	}

	smsStored := storedPath(fileID(smsDomain, smsRelativePath))
	work, err := os.MkdirTemp("", "mv2ios-")
	if err != nil {
		return res, err
	}
	defer os.RemoveAll(work)
	workManifest := filepath.Join(work, "Manifest.db")
	workSMS := filepath.Join(work, "sms.db")
	if err := copyFile(filepath.Join(backupDir, "Manifest.db"), workManifest); err != nil {
		return res, fmt.Errorf("read backup manifest: %w", err)
	}
	if err := copyFile(filepath.Join(backupDir, smsStored), workSMS); err != nil {
		return res, fmt.Errorf("the backup has no Messages database: %w", err)
	}

	now := time.Now()
	manifestDB, err := openSQLite(workManifest)
	if err != nil {
		return res, err
	}
	defer manifestDB.Close()
	manifestTx, err := manifestDB.Begin()
	if err != nil {
		return res, fmt.Errorf("open backup manifest: %w", err)
	}
	defer manifestTx.Rollback()
	man, err := newManifest(manifestTx, now)
	if err != nil {
		return res, err
	}
	if rec, err := parseMBFile(man.template); err != nil {
		return res, fmt.Errorf("read backup manifest: %w", err)
	} else if rec.isEncrypted() {
		return res, ErrEncrypted
	}

	smsSQL, err := openSQLite(workSMS)
	if err != nil {
		return res, err
	}
	defer smsSQL.Close()
	smsTx, err := smsSQL.Begin()
	if err != nil {
		return res, fmt.Errorf("open Messages database: %w", err)
	}
	defer smsTx.Rollback()
	sms, err := openSMS(smsTx)
	if err != nil {
		return res, fmt.Errorf("open Messages database: %w", err)
	}

	owner := ownerNumber(convs)
	var copies []pendingCopy
	for _, conv := range convs {
		numbers := map[string]string{} // participant ID -> normalized number
		var members []string
		seen := map[string]bool{}
		for _, p := range conv.Others() {
			n := normalizeNumber(p.Number, owner)
			if n == "" {
				continue
			}
			numbers[p.ID] = n
			if !seen[n] {
				seen[n] = true
				members = append(members, n)
			}
		}
		if len(members) == 0 || len(conv.Messages) == 0 {
			res.SkippedConversations++
			continue
		}

		isGroup := conv.IsGroup || len(members) > 1
		var chatID, directHandle int64
		var roomName string
		var isNew bool
		if isGroup {
			var chat groupChat
			if chat, isNew, err = sms.groupChatFor(conv.ID, groupDisplayName(conv), members); err != nil {
				return res, err
			}
			chatID, roomName = chat.id, chat.roomName
		} else if chatID, directHandle, isNew, err = sms.directChat(members[0]); err != nil {
			return res, err
		}
		existing, err := sms.existingMessages(chatID)
		if err != nil {
			return res, err
		}

		added := 0
		for _, msg := range conv.Messages {
			guid := derivedGUID("message", msg.ID)
			dup, err := sms.hasMessageGUID(guid)
			if err != nil {
				return res, err
			}
			if dup || existing.contains(msg.FromMe, msg.Text, msg.Timestamp) {
				res.Duplicates++
				continue
			}
			var usable []archive.Attachment
			for _, a := range msg.Attachments {
				if a.Path == "" {
					res.MissingAttachments++
					continue
				}
				if _, err := os.Stat(filepath.Join(archiveDir, filepath.FromSlash(a.Path))); err != nil {
					res.MissingAttachments++
					continue
				}
				usable = append(usable, a)
			}
			if msg.Text == "" && msg.Subject == "" && len(usable) == 0 {
				continue
			}

			handleID := directHandle
			switch {
			case msg.FromMe && isGroup:
				handleID = 0
			case !msg.FromMe && isGroup:
				sender, ok := numbers[msg.SenderID]
				if !ok {
					// The sender has since left the group; attribute the
					// message to a current member rather than drop it.
					sender = members[0]
				}
				if handleID, err = sms.handle(sender); err != nil {
					return res, err
				}
			}
			rowID, err := sms.addMessage(newMessage{
				guid:        guid,
				chatID:      chatID,
				handleID:    handleID,
				roomName:    roomName,
				fromMe:      msg.FromMe,
				at:          msg.Timestamp,
				text:        msg.Text,
				subject:     msg.Subject,
				ownerID:     owner,
				attachments: len(usable),
			})
			if err != nil {
				return res, err
			}
			for i, a := range usable {
				src := filepath.Join(archiveDir, filepath.FromSlash(a.Path))
				digest, size, err := sha1File(src)
				if err != nil {
					return res, err
				}
				attGUID := derivedGUID("attachment", fmt.Sprintf("%s#%d", msg.ID, i))
				name := attachmentName(a)
				devicePath := attachmentDevicePath(attGUID, name)
				id, err := man.addFile(mediaDomain, devicePath, size, digest)
				if err != nil {
					return res, fmt.Errorf("register attachment: %w", err)
				}
				copies = append(copies, pendingCopy{src: src, dst: storedPath(id)})
				err = sms.addAttachment(newAttachment{
					guid:       attGUID,
					messageID:  rowID,
					devicePath: devicePath,
					name:       name,
					mimeType:   a.MimeType,
					size:       size,
					outgoing:   msg.FromMe,
					at:         msg.Timestamp,
				})
				if err != nil {
					return res, err
				}
				res.Attachments++
			}
			added++
		}
		if added > 0 {
			res.Conversations++
			res.Messages += added
			if isNew {
				res.NewChats++
			}
			logf("%s: %d messages", conv.Name, added)
		}
	}

	if err := sms.finish(); err != nil {
		return res, err
	}
	if err := smsTx.Commit(); err != nil {
		return res, err
	}
	var integrity string
	if err := smsSQL.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
		return res, fmt.Errorf("the modified Messages database failed its integrity check (%s %v); the backup was not changed", integrity, err)
	}
	if err := closeSQLite(smsSQL); err != nil {
		return res, err
	}
	digest, size, err := sha1File(workSMS)
	if err != nil {
		return res, err
	}
	if err := man.updateFile(smsDomain, smsRelativePath, size, digest); err != nil {
		return res, fmt.Errorf("update backup manifest: %w", err)
	}
	if err := manifestTx.Commit(); err != nil {
		return res, err
	}
	if err := closeSQLite(manifestDB); err != nil {
		return res, err
	}
	if opts.DryRun || res.Messages == 0 {
		return res, nil
	}

	// Everything below changes the real backup. The originals are saved and
	// the journal written first, so an interrupted run can still be undone.
	undoDir := filepath.Join(opts.UndoRoot, fmt.Sprintf("%s-%s", info.UDID, now.Format("20060102-150405")))
	j := journal{BackupPath: backupDir, Created: now, Replaced: []string{"Manifest.db", filepath.ToSlash(smsStored)}}
	for _, c := range copies {
		j.Added = append(j.Added, filepath.ToSlash(c.dst))
	}
	for _, rel := range j.Replaced {
		if err := copyFile(filepath.Join(backupDir, filepath.FromSlash(rel)), filepath.Join(undoDir, filepath.FromSlash(rel))); err != nil {
			return res, fmt.Errorf("save original files: %w", err)
		}
	}
	data, _ := json.MarshalIndent(j, "", "  ")
	if err := os.WriteFile(filepath.Join(undoDir, journalName), data, 0o600); err != nil {
		return res, err
	}
	res.UndoDir = undoDir

	for _, c := range copies {
		if err := copyFile(c.src, filepath.Join(backupDir, c.dst)); err != nil {
			return res, fmt.Errorf("copy attachment into backup: %w", err)
		}
	}
	if err := replaceFile(workSMS, filepath.Join(backupDir, smsStored)); err != nil {
		return res, err
	}
	if err := replaceFile(workManifest, filepath.Join(backupDir, "Manifest.db")); err != nil {
		return res, err
	}
	return res, nil
}

// replaceFile swaps dst for src in one step, so dst is never left half written.
func replaceFile(src, dst string) error {
	tmp := dst + ".mv2ios-tmp"
	if err := copyFile(src, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// Undo puts a backup back the way it was before the Inject that created undoDir.
func Undo(undoDir string) error {
	data, err := os.ReadFile(filepath.Join(undoDir, journalName))
	if err != nil {
		return err
	}
	var j journal
	if err := json.Unmarshal(data, &j); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(j.BackupPath, "Manifest.db")); err != nil {
		return fmt.Errorf("the backup at %s is no longer there: %w", j.BackupPath, err)
	}
	for _, rel := range j.Replaced {
		if err := replaceFile(filepath.Join(undoDir, filepath.FromSlash(rel)), filepath.Join(j.BackupPath, filepath.FromSlash(rel))); err != nil {
			return err
		}
	}
	for _, rel := range j.Added {
		if err := os.Remove(filepath.Join(j.BackupPath, filepath.FromSlash(rel))); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return os.RemoveAll(undoDir)
}

// LatestUndo returns the most recent undo folder for a backup, if any.
func LatestUndo(undoRoot, udid string) (string, error) {
	matches, _ := filepath.Glob(filepath.Join(undoRoot, udid+"-*"))
	if len(matches) == 0 {
		return "", errors.New("nothing to undo for this backup")
	}
	// Folder names end in a timestamp, so the last one sorted is the newest.
	return matches[len(matches)-1], nil
}
