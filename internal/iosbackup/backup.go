// Package iosbackup reads and modifies local iPhone backups (the kind made by
// Finder, iTunes, Apple Devices or idevicebackup2).
package iosbackup

import (
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"time"

	"howett.net/plist"
	_ "modernc.org/sqlite"
)

const (
	smsDomain       = "HomeDomain"
	smsRelativePath = "Library/SMS/sms.db"
	mediaDomain     = "MediaDomain"
	attachmentsRoot = "Library/SMS/Attachments"

	flagFile      = 1
	flagDirectory = 2
)

var ErrEncrypted = errors.New("this backup is encrypted, which is not supported yet: " +
	"turn off \"Encrypt local backup\", make a new backup, and try again")

// Info describes one backup folder.
type Info struct {
	Path           string
	UDID           string
	DeviceName     string
	ProductVersion string
	LastBackup     time.Time
	Encrypted      bool
}

// DefaultDirs lists the places backup software stores backups on this OS.
func DefaultDirs() []string {
	home, _ := os.UserHomeDir()
	var dirs []string
	switch runtime.GOOS {
	case "windows":
		dirs = append(dirs,
			filepath.Join(os.Getenv("APPDATA"), "Apple Computer", "MobileSync", "Backup"),
			filepath.Join(home, "Apple", "MobileSync", "Backup"),
		)
	case "darwin":
		dirs = append(dirs, filepath.Join(home, "Library", "Application Support", "MobileSync", "Backup"))
	default:
		// Under WSL the backups made by Windows are reachable through /mnt.
		for _, pattern := range []string{
			"/mnt/*/Users/*/Apple/MobileSync/Backup",
			"/mnt/*/Users/*/AppData/Roaming/Apple Computer/MobileSync/Backup",
		} {
			matches, _ := filepath.Glob(pattern)
			dirs = append(dirs, matches...)
		}
	}
	return dirs
}

func readPlist(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	_, err = plist.Unmarshal(data, v)
	return err
}

// ReadInfo inspects a single backup folder.
func ReadInfo(dir string) (Info, error) {
	info := Info{Path: dir, UDID: filepath.Base(dir)}
	var manifest struct {
		IsEncrypted bool `plist:"IsEncrypted"`
	}
	if err := readPlist(filepath.Join(dir, "Manifest.plist"), &manifest); err != nil {
		return info, fmt.Errorf("%s is not an iPhone backup: %w", dir, err)
	}
	info.Encrypted = manifest.IsEncrypted
	var details struct {
		DeviceName     string    `plist:"Device Name"`
		ProductVersion string    `plist:"Product Version"`
		LastBackup     time.Time `plist:"Last Backup Date"`
	}
	// Info.plist only feeds the listing, so a backup without one is still usable.
	if err := readPlist(filepath.Join(dir, "Info.plist"), &details); err == nil {
		info.DeviceName = details.DeviceName
		info.ProductVersion = details.ProductVersion
		info.LastBackup = details.LastBackup
	}
	return info, nil
}

// List finds backups in dirs, newest first.
func List(dirs []string) []Info {
	var out []Info
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			if info, err := ReadInfo(filepath.Join(dir, e.Name())); err == nil {
				out = append(out, info)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].LastBackup.After(out[j].LastBackup) })
	return out
}

// fileID is the name a file is stored under inside the backup folder.
func fileID(domain, relativePath string) string {
	sum := sha1.Sum([]byte(domain + "-" + relativePath))
	return hex.EncodeToString(sum[:])
}

// storedPath is the location of a file relative to the backup folder.
func storedPath(id string) string {
	return filepath.Join(id[:2], id)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func sha1File(path string) ([]byte, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	h := sha1.New()
	n, err := io.Copy(h, f)
	return h.Sum(nil), n, err
}

func openSQLite(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// One connection, so pragmas and the transaction share a session.
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// closeSQLite folds any write-ahead log back into the main file, because only
// the main file is copied into the backup.
func closeSQLite(db *sql.DB) error {
	if _, err := db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		db.Close()
		return err
	}
	return db.Close()
}
