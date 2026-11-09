package iosbackup

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/preswifi/mv2ios/internal/archive"
)

// people knows which addresses in the archive belong to the phone's owner.
//
// Google Messages often lists the owner more than once in a conversation:
// once flagged as "me", and again as an ordinary participant with the same
// number (left over from an older SIM or RCS identity) or with an email
// address from an old Google account. Messages from those extra entries are
// the owner's own, and the entries must not count as group members, or a
// one-to-one chat turns into a group of two.
type people struct {
	// owner is the owner's main number, used to guess the country of
	// numbers written without one.
	owner string
	self  map[string]bool
	// detected lists the addresses recognised as the owner besides the
	// main number, so the user can see what was assumed.
	detected []string
}

func identify(convs []*archive.Conversation, extraSelf []string) people {
	p := people{owner: ownerNumber(convs), self: map[string]bool{}}
	names := map[string]int{}
	for _, c := range convs {
		for _, part := range c.Participants {
			if !part.IsMe {
				continue
			}
			if n := normalizeNumber(part.Number, p.owner); n != "" {
				p.self[n] = true
			}
			if part.Name != "" {
				names[part.Name]++
			}
		}
	}
	ownerName, best := "", 0
	for name, n := range names {
		if n > best || (n == best && name < ownerName) {
			ownerName, best = name, n
		}
	}
	add := func(addr string) {
		if addr != "" && !p.self[addr] {
			p.self[addr] = true
			p.detected = append(p.detected, addr)
		}
	}
	for _, a := range extraSelf {
		add(normalizeNumber(a, p.owner))
	}
	// An email address under the owner's exact name is the owner's own
	// Google account; contacts are listed by phone number.
	if ownerName != "" {
		for _, c := range convs {
			for _, part := range c.Participants {
				if !part.IsMe && part.Name == ownerName && strings.Contains(part.Number, "@") {
					add(normalizeNumber(part.Number, p.owner))
				}
			}
		}
	}
	sort.Strings(p.detected)
	return p
}

// convPlan says how one archived conversation maps onto the iPhone.
type convPlan struct {
	// members are the other people in the chat, normalized and without the
	// owner, in the order they first appear.
	members []string
	isGroup bool
	// numbers maps participant IDs to normalized addresses.
	numbers map[string]string
	selfIDs map[string]bool
	// dropped lists participants that could not be added to the chat, such
	// as Google's "UNKNOWN_SENDER!" placeholder.
	dropped []string
}

func (p people) plan(c *archive.Conversation) convPlan {
	cp := convPlan{numbers: map[string]string{}, selfIDs: map[string]bool{}}
	type entry struct{ id, addr string }
	var others []entry
	for _, part := range c.Participants {
		addr := normalizeNumber(part.Number, p.owner)
		if part.IsMe || (addr != "" && p.self[addr]) {
			cp.selfIDs[part.ID] = true
			continue
		}
		if addr != "" {
			others = append(others, entry{part.ID, addr})
		}
	}
	seen := map[string]bool{}
	for _, o := range others {
		// A name without digits ("AMAZON") is a fine sender on its own, but
		// next to real people it is a placeholder nobody can be texted at.
		if len(others) > 1 && !addressable(o.addr) {
			cp.dropped = append(cp.dropped, o.addr)
			continue
		}
		cp.numbers[o.id] = o.addr
		if !seen[o.addr] {
			seen[o.addr] = true
			cp.members = append(cp.members, o.addr)
		}
	}
	cp.isGroup = c.IsGroup || len(cp.members) > 1
	return cp
}

func addressable(addr string) bool {
	return strings.ContainsAny(addr, "0123456789@")
}

func (cp convPlan) fromMe(m archive.Message) bool {
	return m.FromMe || cp.selfIDs[m.SenderID]
}

// sender returns the address an incoming message is shown as coming from,
// and whether that is really who sent it.
func (cp convPlan) sender(m archive.Message) (string, bool) {
	if n, ok := cp.numbers[m.SenderID]; ok {
		return n, true
	}
	// The sender has since left the group, or has no usable number;
	// attribute the message to a current member rather than drop it.
	return cp.members[0], false
}

// usableAttachments returns the attachments whose files are in the archive.
func usableAttachments(archiveDir string, m archive.Message) (usable []archive.Attachment, missing int) {
	for _, a := range m.Attachments {
		if a.Path == "" || !fileExists(archivePath(archiveDir, a.Path)) {
			missing++
			continue
		}
		usable = append(usable, a)
	}
	return usable, missing
}

func archivePath(dir, rel string) string { return filepath.Join(dir, filepath.FromSlash(rel)) }

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
