package gmessages

import "testing"

const all = "SID=a; HSID=b; OSID=c; SSID=d; APISID=e; SAPISID=f=="

func TestParseCookies(t *testing.T) {
	for name, in := range map[string]string{
		"bare":   all,
		"header": "Cookie: " + all,
		"curl":   "curl 'https://messages.google.com/web/config' \\\n  -H 'accept: */*' \\\n  -b '" + all + "' \\\n  -H 'user-agent: x'",
		"curl-H": `curl "https://messages.google.com/web/config" -H "cookie: ` + all + `" -H "user-agent: x"`,
		"json":   `{"SID":"a","HSID":"b","OSID":"c","SSID":"d","APISID":"e","SAPISID":"f=="}`,
	} {
		got, err := ParseCookies(in)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if len(got) != 6 || got["SAPISID"] != "f==" || got["SID"] != "a" {
			t.Errorf("%s: got %v", name, got)
		}
	}
	if _, err := ParseCookies("SID=a; HSID=b"); err == nil {
		t.Error("missing cookies were accepted")
	}
}

func TestPickCookies(t *testing.T) {
	got := pickCookies([]browserCookie{
		{Name: "SID", Value: "sid", Domain: ".google.com"},
		{Name: "OSID", Value: "mail", Domain: "mail.google.com"},
		{Name: "OSID", Value: "messages", Domain: "messages.google.com"},
		{Name: "OSID", Value: "generic", Domain: ".google.com"},
		{Name: "NID", Value: "other", Domain: ".youtube.com"},
	})
	if len(got) != 2 || got["OSID"] != "messages" || got["SID"] != "sid" {
		t.Errorf("got %v", got)
	}
}

func TestParseCookiesWithoutCookieHeader(t *testing.T) {
	_, err := ParseCookies("curl --url 'https://messages.google.com/web/config?pli=1' \\\n  -H 'Referer: https://accounts.google.com/' \\\n  -H 'sec-ch-ua-wow64: ?0'")
	if err == nil || err != ErrNoCookieHeader {
		t.Errorf("err = %v, want ErrNoCookieHeader", err)
	}
}
