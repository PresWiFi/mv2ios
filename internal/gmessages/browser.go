package gmessages

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/coder/websocket"
)

const messagesHost = "messages.google.com"

// findBrowser locates a Chromium-based browser, the only kind that exposes
// the DevTools protocol used to read the sign-in cookies.
func findBrowser() (string, error) {
	var candidates []string
	switch runtime.GOOS {
	case "windows":
		for _, root := range []string{os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)"), os.Getenv("LocalAppData")} {
			if root == "" {
				continue
			}
			candidates = append(candidates,
				filepath.Join(root, "Google", "Chrome", "Application", "chrome.exe"),
				filepath.Join(root, "Microsoft", "Edge", "Application", "msedge.exe"),
				filepath.Join(root, "BraveSoftware", "Brave-Browser", "Application", "brave.exe"),
			)
		}
	case "darwin":
		for _, app := range []string{"Google Chrome", "Microsoft Edge", "Brave Browser", "Chromium"} {
			candidates = append(candidates, filepath.Join("/Applications", app+".app", "Contents", "MacOS", app))
		}
	default:
		for _, name := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "microsoft-edge", "brave-browser"} {
			if p, err := exec.LookPath(name); err == nil {
				candidates = append(candidates, p)
			}
		}
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c, nil
		}
	}
	return "", errors.New("could not find Chrome, Edge or Brave on this computer")
}

type browserCookie struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Domain string `json:"domain"`
}

// pickCookies selects the cookies a request to messages.google.com would
// carry. Several Google sites set a cookie with the same name (OSID
// especially), so the most specific matching domain wins.
func pickCookies(all []browserCookie) map[string]string {
	out := map[string]string{}
	specificity := map[string]int{}
	for _, c := range all {
		domain := strings.TrimPrefix(c.Domain, ".")
		if domain != messagesHost && !strings.HasSuffix(messagesHost, "."+domain) {
			continue
		}
		if len(domain) > specificity[c.Name] {
			specificity[c.Name] = len(domain)
			out[c.Name] = c.Value
		}
	}
	return out
}

func hasRequired(cookies map[string]string) bool {
	for _, name := range requiredCookies {
		if cookies[name] == "" {
			return false
		}
	}
	return true
}

// devToolsURL waits for the browser to report where its DevTools endpoint is.
func devToolsURL(ctx context.Context, profile string) (string, error) {
	portFile := filepath.Join(profile, "DevToolsActivePort")
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		// The file holds the port on the first line and the path on the second.
		if data, err := os.ReadFile(portFile); err == nil {
			if lines := strings.Fields(string(data)); len(lines) == 2 {
				return "ws://127.0.0.1:" + lines[0] + lines[1], nil
			}
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return "", errors.New("the browser started but did not open its control channel")
}

type devTools struct {
	conn *websocket.Conn
	next int
}

func (d *devTools) call(ctx context.Context, method string, result any) error {
	d.next++
	req, _ := json.Marshal(map[string]any{"id": d.next, "method": method})
	if err := d.conn.Write(ctx, websocket.MessageText, req); err != nil {
		return err
	}
	for {
		_, data, err := d.conn.Read(ctx)
		if err != nil {
			return err
		}
		var resp struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		// Anything without our ID is an unrelated browser event.
		if json.Unmarshal(data, &resp) != nil || resp.ID != d.next {
			continue
		}
		if resp.Error != nil {
			return fmt.Errorf("%s: %s", method, resp.Error.Message)
		}
		if result == nil {
			return nil
		}
		return json.Unmarshal(resp.Result, result)
	}
}

type browserProcess struct {
	cmd    *exec.Cmd
	exited chan struct{}
}

func startBrowser(path string, args ...string) (*browserProcess, error) {
	cmd := exec.Command(path, args...)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start browser: %w", err)
	}
	b := &browserProcess{cmd: cmd, exited: make(chan struct{})}
	go func() {
		cmd.Wait()
		close(b.exited)
	}()
	return b, nil
}

func (b *browserProcess) stop() {
	b.cmd.Process.Kill()
	<-b.exited
}

// readProfileCookies starts the browser hidden on an existing profile and
// reads its cookies over the DevTools protocol. Google refuses sign-in in a
// browser that has this protocol switched on, which is why it is only
// switched on after the user has signed in and closed the window.
func readProfileCookies(ctx context.Context, browser, profile string) (map[string]string, error) {
	// A stale port file from an earlier launch would point at a dead browser.
	os.Remove(filepath.Join(profile, "DevToolsActivePort"))
	b, err := startBrowser(browser,
		"--headless=new",
		"--remote-debugging-port=0",
		"--user-data-dir="+profile,
		"--no-first-run",
		"--no-default-browser-check",
		"about:blank",
	)
	if err != nil {
		return nil, err
	}
	defer b.stop()
	url, err := devToolsURL(ctx, profile)
	if err != nil {
		return nil, err
	}
	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		return nil, fmt.Errorf("connect to browser: %w", err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(32 << 20)
	dt := &devTools{conn: conn}
	var result struct {
		Cookies []browserCookie `json:"cookies"`
	}
	if err := dt.call(ctx, "Storage.getCookies", &result); err != nil {
		return nil, fmt.Errorf("read cookies from browser: %w", err)
	}
	dt.call(ctx, "Browser.close", nil)
	return pickCookies(result.Cookies), nil
}

// BrowserCookies opens an ordinary browser window on Google's sign-in page,
// waits for the user to sign in and close it, then reads the resulting
// cookies. The window uses a throwaway profile that is deleted afterwards,
// so nothing is left signed in.
func BrowserCookies(ctx context.Context) (map[string]string, error) {
	browser, err := findBrowser()
	if err != nil {
		return nil, err
	}
	profile, err := os.MkdirTemp("", "mv2ios-browser-")
	if err != nil {
		return nil, err
	}
	defer func() {
		// The browser can hold files open for a moment after it exits.
		for i := 0; i < 10 && os.RemoveAll(profile) != nil; i++ {
			time.Sleep(300 * time.Millisecond)
		}
	}()

	window, err := startBrowser(browser,
		"--user-data-dir="+profile,
		"--no-first-run",
		"--no-default-browser-check",
		CookieURL,
	)
	if err != nil {
		return nil, err
	}
	select {
	case <-window.exited:
	case <-ctx.Done():
		window.stop()
		return nil, ctx.Err()
	}

	cookies, err := readProfileCookies(ctx, browser, profile)
	if err != nil {
		return nil, err
	}
	if !hasRequired(cookies) {
		return nil, errors.New("the window was closed before sign-in finished; " +
			"close it only once the Google Messages page has loaded")
	}
	return cookies, nil
}
