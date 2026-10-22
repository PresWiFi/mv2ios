package gmessages

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/util/exhttp"
)

// CookieURL is the page to be signed in to when copying cookies.
const CookieURL = "https://accounts.google.com/AccountChooser?continue=https://messages.google.com/web/config"

var requiredCookies = []string{"SID", "HSID", "OSID", "SSID", "APISID", "SAPISID"}

// Matches the cookie header inside a "Copy as cURL" command or a pasted
// "Cookie: ..." request header line.
var cookieHeader = regexp.MustCompile(`(?i)(?:-b |--cookie |cookie: ?)\s*['"^]*([^'"\r\n]+)`)

// ErrNoCookieHeader means a copied request had its cookies left out. Browsers
// do that for a request they only hold provisional headers for, which is the
// usual result of copying the request that followed the sign-in redirect.
var ErrNoCookieHeader = errors.New("the copied request contains no cookies: with the Network tab still open, " +
	"press F5 on the messages.google.com page and copy the \"config\" request again")

// ParseCookies accepts what people can realistically copy out of a browser:
// a JSON object of name/value pairs, a "Copy as cURL" command, a "Cookie:"
// header line, or the bare "name=value; name=value" string.
func ParseCookies(text string) (map[string]string, error) {
	text = strings.TrimSpace(text)
	cookies := map[string]string{}
	if strings.HasPrefix(text, "{") {
		if err := json.Unmarshal([]byte(text), &cookies); err != nil {
			return nil, fmt.Errorf("cookies look like JSON but could not be read: %w", err)
		}
	} else {
		if m := cookieHeader.FindStringSubmatch(text); m != nil {
			text = m[1]
		} else if strings.HasPrefix(text, "curl ") {
			return nil, ErrNoCookieHeader
		}
		for _, pair := range strings.Split(text, ";") {
			if name, value, ok := strings.Cut(strings.TrimSpace(pair), "="); ok && name != "" {
				cookies[name] = value
			}
		}
	}
	var missing []string
	for _, name := range requiredCookies {
		if cookies[name] == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("these Google cookies are missing: %s. Copy them from a request to messages.google.com (not google.com) while signed in", strings.Join(missing, ", "))
	}
	return cookies, nil
}

// PairGoogle links this computer to the phone through the Google account
// both are signed in to. showEmoji is called with the emoji the user must
// tap in Google Messages on the phone to approve the pairing.
func PairGoogle(ctx context.Context, sessionPath string, cookies map[string]string, showEmoji func(emoji string)) error {
	auth := libgm.NewAuthData()
	auth.SetCookies(cookies)
	cli := libgm.NewClient(auth, nil, logger(), exhttp.ClientSettings{})
	cli.SetEventHandler(func(any) {})
	defer cli.Disconnect()

	if err := cli.FetchConfig(ctx); err != nil {
		return fmt.Errorf("fetch Google Messages config: %w", err)
	}
	emoji, session, err := cli.StartGaiaPairing(ctx, ctx)
	switch {
	case errors.Is(err, libgm.ErrNoDevicesFound):
		return errors.New("Google found no phone for this account: check that Google Messages on the phone is signed in to the same Google account and that RCS chats are turned on")
	case errors.Is(err, libgm.ErrPairingInitTimeout):
		return errors.New("the phone did not respond: unlock it, open Google Messages, and try again")
	case err != nil:
		return fmt.Errorf("start pairing (the cookies may be expired or from the wrong account): %w", err)
	}
	showEmoji(emoji)
	_, err = cli.FinishGaiaPairing(ctx, session)
	switch {
	case errors.Is(err, libgm.ErrIncorrectEmoji):
		return errors.New("the wrong emoji was tapped on the phone; run pair again")
	case errors.Is(err, libgm.ErrPairingCancelled):
		return errors.New("pairing was cancelled on the phone")
	case errors.Is(err, libgm.ErrPairingTimeout):
		return errors.New("timed out waiting for the emoji to be tapped on the phone")
	case err != nil:
		return fmt.Errorf("finish pairing: %w", err)
	}
	return saveSession(sessionPath, cli.AuthData)
}
