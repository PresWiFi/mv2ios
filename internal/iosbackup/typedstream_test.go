package iosbackup

import (
	"bytes"
	"encoding/hex"
	"os"
	"strings"
	"testing"
)

// The attributedBody iOS itself writes for an SMS reading "Hello".
const helloBody = "040b73747265616d747970656481e803840140848484124e5341747472696275746564537472696e67008484084e534f626a656374008592848484084e53537472696e67019484012b0548656c6c6f86840269490105928484840c4e5344696374696f6e617279009484016901928496961d5f5f6b494d4d657373616765506172744174747269627574654e616d658692848484084e534e756d626572008484074e5356616c7565009484012a84999900868686"

func TestAttributedBodyMatchesIOS(t *testing.T) {
	got := hex.EncodeToString(attributedBody("Hello", nil))
	if got != helloBody {
		t.Fatalf("attributedBody(\"Hello\")\n got %s\nwant %s", got, helloBody)
	}
}

func TestAttributedBodyLongAndAttachments(t *testing.T) {
	long := strings.Repeat("héllo 🥰 ", 40)
	b := attributedBody(long, []string{"AAAA-1", "BBBB-2"})
	if !bytes.Contains(b, []byte(attachmentPlaceholder+attachmentPlaceholder+long)) {
		t.Fatal("text missing from attributedBody")
	}
	if dir := os.Getenv("MV2IOS_TS_DUMP"); dir != "" {
		_ = os.WriteFile(dir+"/long.bin", b, 0o644)
		_ = os.WriteFile(dir+"/hello.bin", attributedBody("Hello", nil), 0o644)
		_ = os.WriteFile(dir+"/photo.bin", attributedBody("", []string{"CCCC-3"}), 0o644)
	}
}
