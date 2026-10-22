// Package archive defines the neutral on-disk format that sits between the
// Android extractor and the iOS injector.
//
// Layout of an archive directory:
//
//	conversations/<id>.json   one Conversation per file
//	media/<conversation id>/  attachment files referenced by Attachment.Path
package archive

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	ProtocolSMS = "sms"
	ProtocolMMS = "mms"
	ProtocolRCS = "rcs"
)

type Participant struct {
	ID     string `json:"id"`
	Number string `json:"number"`
	Name   string `json:"name,omitempty"`
	IsMe   bool   `json:"is_me,omitempty"`
}

type Attachment struct {
	// Path is relative to the archive directory and uses forward slashes.
	// It is empty when the download failed; Error then says why.
	Path     string `json:"path,omitempty"`
	Name     string `json:"name,omitempty"`
	MimeType string `json:"mime_type,omitempty"`
	Size     int64  `json:"size,omitempty"`
	// Thumbnail means only a small preview could be saved, not the original.
	Thumbnail bool   `json:"thumbnail,omitempty"`
	Error     string `json:"error,omitempty"`
}

type Reaction struct {
	Emoji          string   `json:"emoji"`
	ParticipantIDs []string `json:"participant_ids"`
}

type Message struct {
	ID          string       `json:"id"`
	Timestamp   time.Time    `json:"timestamp"`
	SenderID    string       `json:"sender_id,omitempty"`
	FromMe      bool         `json:"from_me,omitempty"`
	Protocol    string       `json:"protocol"`
	Text        string       `json:"text,omitempty"`
	Subject     string       `json:"subject,omitempty"`
	Attachments []Attachment `json:"attachments,omitempty"`
	Reactions   []Reaction   `json:"reactions,omitempty"`
}

type Conversation struct {
	ID           string        `json:"id"`
	Name         string        `json:"name,omitempty"`
	IsGroup      bool          `json:"is_group,omitempty"`
	Protocol     string        `json:"protocol"`
	Participants []Participant `json:"participants"`
	// Messages are sorted oldest first.
	Messages []Message `json:"messages"`
}

// Others returns the participants that are not the phone's owner.
func (c *Conversation) Others() []Participant {
	var out []Participant
	for _, p := range c.Participants {
		if !p.IsMe {
			out = append(out, p)
		}
	}
	return out
}

func (c *Conversation) Participant(id string) (Participant, bool) {
	for _, p := range c.Participants {
		if p.ID == id {
			return p, true
		}
	}
	return Participant{}, false
}

// SafeName turns an arbitrary ID into something usable as a file name.
func SafeName(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			return r
		}
		return '_'
	}, s)
}

func conversationPath(dir, id string) string {
	return filepath.Join(dir, "conversations", SafeName(id)+".json")
}

// HasConversation reports whether a conversation was already fully written.
func HasConversation(dir, id string) bool {
	_, err := os.Stat(conversationPath(dir, id))
	return err == nil
}

func WriteConversation(dir string, c *Conversation) error {
	sort.SliceStable(c.Messages, func(i, j int) bool {
		return c.Messages[i].Timestamp.Before(c.Messages[j].Timestamp)
	})
	path := conversationPath(dir, c.ID)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	// Write to a temp file first so an interrupted export never leaves a
	// truncated conversation that a resumed run would treat as complete.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// HasPreviews reports whether a saved conversation has attachments that were
// saved only as small previews.
func HasPreviews(dir, id string) bool {
	data, err := os.ReadFile(conversationPath(dir, id))
	if err != nil {
		return false
	}
	var c Conversation
	if json.Unmarshal(data, &c) != nil {
		return false
	}
	for _, m := range c.Messages {
		for _, a := range m.Attachments {
			if a.Thumbnail {
				return true
			}
		}
	}
	return false
}

// Load reads every conversation in an archive directory.
func Load(dir string) ([]*Conversation, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "conversations", "*.json"))
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("no conversations found in %s", dir)
	}
	sort.Strings(paths)
	out := make([]*Conversation, 0, len(paths))
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		var c Conversation
		if err := json.Unmarshal(data, &c); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		out = append(out, &c)
	}
	return out, nil
}
