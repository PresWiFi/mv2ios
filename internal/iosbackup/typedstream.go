package iosbackup

import (
	"bytes"
	"encoding/binary"
	"unicode/utf16"
)

// Messages on recent iOS versions draws each bubble from message.attributedBody,
// not from message.text. When attributedBody is empty the phone shows the
// sender and time with a blank bubble. The column holds an NSAttributedString
// archived in Apple's old "typedstream" (NSArchiver) format, which this file
// writes.

const (
	tsTagInt16 = 0x81
	tsTagInt32 = 0x82
	tsTagNew   = 0x84
	tsTagNil   = 0x85
	tsTagEnd   = 0x86
	// Reference number 0 is stored as -110 (0x92); the bytes below it are tags.
	tsFirstRef = -110
)

type tsClass struct {
	name    string
	version int64
	super   *tsClass
}

var (
	tsNSObject           = &tsClass{"NSObject", 0, nil}
	tsNSString           = &tsClass{"NSString", 1, tsNSObject}
	tsNSAttributedString = &tsClass{"NSAttributedString", 0, tsNSObject}
	tsNSDictionary       = &tsClass{"NSDictionary", 0, tsNSObject}
	tsNSValue            = &tsClass{"NSValue", 0, tsNSObject}
	tsNSNumber           = &tsClass{"NSNumber", 0, tsNSValue}
)

type tsWriter struct {
	buf bytes.Buffer
	// strs holds the shared strings (type encodings and class names).
	strs map[string]int
	// objects counts the shared object table: objects, classes and C strings.
	objects int
	classes map[*tsClass]int
	cstrs   map[string]int
}

func newTSWriter() *tsWriter {
	w := &tsWriter{strs: map[string]int{}, classes: map[*tsClass]int{}, cstrs: map[string]int{}}
	w.buf.WriteByte(4) // streamer version
	w.rawString("streamtyped")
	w.int(1000) // system version
	return w
}

func (w *tsWriter) int(n int64) {
	switch {
	case n >= -110 && n <= 127:
		w.buf.WriteByte(byte(int8(n)))
	case n >= -32768 && n <= 32767:
		w.buf.WriteByte(tsTagInt16)
		_ = binary.Write(&w.buf, binary.LittleEndian, int16(n))
	default:
		w.buf.WriteByte(tsTagInt32)
		_ = binary.Write(&w.buf, binary.LittleEndian, int32(n))
	}
}

// uint writes a length or other unsigned value. Values above 127 always use a
// wider form so a signed reader cannot take them for tags.
func (w *tsWriter) uint(n int) {
	if n <= 127 {
		w.buf.WriteByte(byte(n))
	} else if n <= 32767 {
		w.buf.WriteByte(tsTagInt16)
		_ = binary.Write(&w.buf, binary.LittleEndian, uint16(n))
	} else {
		w.buf.WriteByte(tsTagInt32)
		_ = binary.Write(&w.buf, binary.LittleEndian, uint32(n))
	}
}

func (w *tsWriter) rawString(s string) {
	w.uint(len(s))
	w.buf.WriteString(s)
}

func (w *tsWriter) ref(index int) { w.int(int64(index + tsFirstRef)) }

func (w *tsWriter) shared(s string) {
	if i, ok := w.strs[s]; ok {
		w.ref(i)
		return
	}
	w.buf.WriteByte(tsTagNew)
	w.rawString(s)
	w.strs[s] = len(w.strs)
}

func (w *tsWriter) class(c *tsClass) {
	for ; c != nil; c = c.super {
		if i, ok := w.classes[c]; ok {
			w.ref(i)
			return
		}
		w.buf.WriteByte(tsTagNew)
		w.shared(c.name)
		w.int(c.version)
		w.classes[c] = w.objects
		w.objects++
	}
	w.buf.WriteByte(tsTagNil)
}

// begin starts a new object of class c. Its contents follow, then end.
func (w *tsWriter) begin(c *tsClass) {
	w.buf.WriteByte(tsTagNew)
	w.objects++ // the object is numbered before its class
	w.class(c)
}

func (w *tsWriter) end() { w.buf.WriteByte(tsTagEnd) }

func (w *tsWriter) cString(s string) {
	if i, ok := w.cstrs[s]; ok {
		w.ref(i)
		return
	}
	w.buf.WriteByte(tsTagNew)
	w.shared(s)
	w.cstrs[s] = w.objects
	w.objects++
}

func (w *tsWriter) nsString(s string) {
	w.begin(tsNSString)
	w.shared("+")
	w.rawString(s)
	w.end()
}

func (w *tsWriter) nsInt(n int64) {
	w.begin(tsNSNumber)
	w.shared("*")
	w.cString("i")
	w.shared("i")
	w.int(n)
	w.end()
}

// attrRun is a stretch of the text sharing one set of attributes.
type attrRun struct {
	length int // in UTF-16 code units, as NSString counts
	attrs  []attrPair
}

type attrPair struct {
	key   string
	value any // string or int64
}

func (w *tsWriter) dictionary(pairs []attrPair) {
	w.begin(tsNSDictionary)
	w.shared("i")
	w.int(int64(len(pairs)))
	for _, p := range pairs {
		w.shared("@")
		w.nsString(p.key)
		w.shared("@")
		switch v := p.value.(type) {
		case string:
			w.nsString(v)
		case int64:
			w.nsInt(v)
		}
	}
	w.end()
}

func utf16Len(s string) int { return len(utf16.Encode([]rune(s))) }

// attributedBody builds the attributedBody value for a message's text.
// attachmentGUIDs are the attachments in order; each has one placeholder
// character at the start of text.
func attributedBody(text string, attachmentGUIDs []string) []byte {
	var runs []attrRun
	part := int64(0)
	for _, guid := range attachmentGUIDs {
		runs = append(runs, attrRun{length: 1, attrs: []attrPair{
			{"__kIMFileTransferGUIDAttributeName", guid},
			{"__kIMMessagePartAttributeName", part},
		}})
		part++
	}
	full := text
	for range attachmentGUIDs {
		full = attachmentPlaceholder + full
	}
	if rest := utf16Len(text); rest > 0 {
		runs = append(runs, attrRun{length: rest, attrs: []attrPair{
			{"__kIMMessagePartAttributeName", part},
		}})
	}

	w := newTSWriter()
	w.shared("@")
	w.begin(tsNSAttributedString)
	w.shared("@")
	w.nsString(full)
	// Each run is (attribute set number, length). A set is written out the
	// first time it is used; sets are numbered from 1.
	for i, r := range runs {
		w.shared("iI")
		w.int(int64(i + 1))
		w.uint(r.length)
		w.shared("@")
		w.dictionary(r.attrs)
	}
	w.end()
	return w.buf.Bytes()
}
