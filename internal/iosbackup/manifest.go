package iosbackup

import (
	"database/sql"
	"errors"
	"fmt"
	"path"
	"time"

	"howett.net/plist"
)

// mbFile is the per-file metadata record stored in Manifest.db. It is an
// NSKeyedArchiver property list: values live in a flat "$objects" array and
// refer to each other by index (plist.UID).
type mbFile struct {
	root    map[string]any
	objects []any
	fields  map[string]any
}

func parseMBFile(blob []byte) (*mbFile, error) {
	m := &mbFile{}
	if _, err := plist.Unmarshal(blob, &m.root); err != nil {
		return nil, err
	}
	top, _ := m.root["$top"].(map[string]any)
	rootUID, ok := top["root"].(plist.UID)
	m.objects, _ = m.root["$objects"].([]any)
	if !ok || int(rootUID) >= len(m.objects) {
		return nil, errors.New("unexpected file record layout")
	}
	if m.fields, ok = m.objects[rootUID].(map[string]any); !ok {
		return nil, errors.New("unexpected file record layout")
	}
	return m, nil
}

func (m *mbFile) encode() ([]byte, error) {
	m.root["$objects"] = m.objects
	return plist.Marshal(m.root, plist.BinaryFormat)
}

func (m *mbFile) isEncrypted() bool {
	_, ok := m.fields["EncryptionKey"]
	return ok
}

// setReferenced replaces a value that the record points to by index.
func (m *mbFile) setReferenced(key string, value any) {
	if uid, ok := m.fields[key].(plist.UID); ok && int(uid) < len(m.objects) {
		// NSData is sometimes wrapped in an object rather than stored bare.
		if wrapper, ok := m.objects[uid].(map[string]any); ok {
			if _, ok := wrapper["NS.data"]; ok {
				wrapper["NS.data"] = value
				return
			}
		}
		m.objects[uid] = value
		return
	}
	m.objects = append(m.objects, value)
	m.fields[key] = plist.UID(len(m.objects) - 1)
}

func (m *mbFile) setContent(size int64, digest []byte, modified time.Time) {
	m.fields["Size"] = uint64(size)
	m.fields["LastModified"] = uint64(modified.Unix())
	if _, ok := m.fields["Digest"]; ok {
		m.setReferenced("Digest", digest)
	}
}

// retarget turns a copy of an existing record into the record for a new path.
func (m *mbFile) retarget(relativePath string, mode uint64, inode uint64, now time.Time) {
	m.setReferenced("RelativePath", relativePath)
	m.fields["Mode"] = mode
	m.fields["InodeNumber"] = inode
	m.fields["Birth"] = uint64(now.Unix())
	m.fields["LastStatusChange"] = uint64(now.Unix())
	m.fields["LastModified"] = uint64(now.Unix())
	// Extended attributes and link targets describe the file this was copied from.
	delete(m.fields, "ExtendedAttributes")
	delete(m.fields, "Target")
}

const (
	modeRegularFile = 0o100644
	modeDirectory   = 0o040755
)

type manifest struct {
	tx        *sql.Tx
	template  []byte
	nextInode uint64
	now       time.Time
}

func newManifest(tx *sql.Tx, now time.Time) (*manifest, error) {
	m := &manifest{tx: tx, now: now, nextInode: uint64(now.Unix())<<8 | 1}
	// Prefer an existing attachment as the template for new records, since it
	// already has the right owner and protection class.
	err := tx.QueryRow(`SELECT file FROM Files WHERE domain = ? AND flags = ? AND relativePath LIKE ? LIMIT 1`,
		mediaDomain, flagFile, attachmentsRoot+"/%").Scan(&m.template)
	if errors.Is(err, sql.ErrNoRows) {
		err = tx.QueryRow(`SELECT file FROM Files WHERE domain = ? AND relativePath = ?`,
			smsDomain, smsRelativePath).Scan(&m.template)
	}
	if err != nil {
		return nil, fmt.Errorf("find a file record to use as a template: %w", err)
	}
	return m, nil
}

func (m *manifest) exists(domain, relativePath string) (bool, error) {
	var n int
	err := m.tx.QueryRow(`SELECT COUNT(*) FROM Files WHERE fileID = ?`, fileID(domain, relativePath)).Scan(&n)
	return n > 0, err
}

func (m *manifest) add(domain, relativePath string, flags int, mode uint64, size int64, digest []byte) error {
	rec, err := parseMBFile(m.template)
	if err != nil {
		return err
	}
	m.nextInode++
	rec.retarget(relativePath, mode, m.nextInode, m.now)
	rec.setContent(size, digest, m.now)
	if flags == flagDirectory {
		delete(rec.fields, "Digest")
	}
	blob, err := rec.encode()
	if err != nil {
		return err
	}
	_, err = m.tx.Exec(`INSERT INTO Files (fileID, domain, relativePath, flags, file) VALUES (?, ?, ?, ?, ?)`,
		fileID(domain, relativePath), domain, relativePath, flags, blob)
	return err
}

// addDirs registers every missing parent directory of relativePath.
func (m *manifest) addDirs(domain, relativePath string) error {
	var missing []string
	for dir := path.Dir(relativePath); dir != "." && dir != "/"; dir = path.Dir(dir) {
		found, err := m.exists(domain, dir)
		if err != nil {
			return err
		}
		if found {
			break
		}
		missing = append(missing, dir)
	}
	for i := len(missing) - 1; i >= 0; i-- {
		if err := m.add(domain, missing[i], flagDirectory, modeDirectory, 0, nil); err != nil {
			return err
		}
	}
	return nil
}

// addFile registers a new file and returns the name it must be stored under.
func (m *manifest) addFile(domain, relativePath string, size int64, digest []byte) (string, error) {
	if err := m.addDirs(domain, relativePath); err != nil {
		return "", err
	}
	if err := m.add(domain, relativePath, flagFile, modeRegularFile, size, digest); err != nil {
		return "", err
	}
	return fileID(domain, relativePath), nil
}

// updateFile records new contents for a file that is already in the backup.
func (m *manifest) updateFile(domain, relativePath string, size int64, digest []byte) error {
	id := fileID(domain, relativePath)
	var blob []byte
	if err := m.tx.QueryRow(`SELECT file FROM Files WHERE fileID = ?`, id).Scan(&blob); err != nil {
		return err
	}
	rec, err := parseMBFile(blob)
	if err != nil {
		return err
	}
	rec.setContent(size, digest, m.now)
	if blob, err = rec.encode(); err != nil {
		return err
	}
	_, err = m.tx.Exec(`UPDATE Files SET file = ? WHERE fileID = ?`, blob, id)
	return err
}
