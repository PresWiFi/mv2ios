// Package gui serves a small web page on this computer that walks through
// pair, export and inject without the command line. The page is only
// reachable from this computer and only with the secret in its address.
package gui

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"rsc.io/qr"

	"github.com/preswifi/mv2ios/internal/gmessages"
	"github.com/preswifi/mv2ios/internal/iosbackup"
)

// DonateURL is the "Buy me a coffee" link shown in the app. Leave it empty to
// hide the button.
const DonateURL = "https://buymeacoffee.com/blakeriding"

//go:embed index.html
var indexHTML []byte

// Config holds the paths the command line also uses.
type Config struct {
	Version     string
	SessionPath string
	UndoRoot    string
}

// event is one message pushed to the page.
type event struct {
	Kind string `json:"kind"` // log, emoji, qr, done, error, paste
	Text string `json:"text,omitempty"`
	Job  string `json:"job,omitempty"`
	Data any    `json:"data,omitempty"`
}

type server struct {
	cfg   Config
	token string

	mu      sync.Mutex
	job     string // name of the running job, empty when idle
	cancel  context.CancelFunc
	history []event
	subs    map[chan event]struct{}
}

// Run starts the page, opens it in the default browser, and serves until ctx ends.
func Run(ctx context.Context, cfg Config) error {
	secret := make([]byte, 16)
	if _, err := rand.Read(secret); err != nil {
		return err
	}
	s := &server{cfg: cfg, token: hex.EncodeToString(secret), subs: map[chan event]struct{}{}}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	addr := ln.Addr().String()
	url := fmt.Sprintf("http://%s/?t=%s", addr, s.token)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("GET /api/state", s.state)
	mux.HandleFunc("GET /api/events", s.events)
	mux.HandleFunc("POST /api/pair", s.pair)
	mux.HandleFunc("POST /api/export", s.export)
	mux.HandleFunc("POST /api/inject", s.inject)
	mux.HandleFunc("POST /api/undo", s.undo)
	mux.HandleFunc("POST /api/unpair", s.unpair)
	mux.HandleFunc("POST /api/cancel", s.cancelJob)

	srv := &http.Server{Handler: s.guard(addr, mux), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		s.mu.Lock()
		if s.cancel != nil {
			s.cancel()
		}
		s.mu.Unlock()
		shutdown, done := context.WithTimeout(context.Background(), 3*time.Second)
		defer done()
		srv.Shutdown(shutdown)
	}()

	fmt.Println("mv2ios is open in your web browser. If it did not open, go to:")
	fmt.Println(" ", url)
	fmt.Println("Keep this window open while you use mv2ios. Close it (or press Ctrl+C) to quit.")
	if os.Getenv("MV2IOS_NO_BROWSER") != "" {
		// Leave it to the user.
	} else if err := openBrowser(url); err != nil {
		fmt.Println("Could not open the browser:", err)
	}
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// guard refuses requests that do not come from the page this process opened:
// the Host must be our own address (no DNS rebinding), and the secret must be
// in the address or the cookie the page was given.
func (s *server) guard(addr string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != addr {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		got := r.URL.Query().Get("t")
		if c, err := r.Cookie("mv2ios"); err == nil && got == "" {
			got = c.Value
		}
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
			http.Error(w, "Open mv2ios from the address it printed when it started.", http.StatusForbidden)
			return
		}
		// Browsers send Origin on cross-site POSTs; refuse any that is not ours.
		if o := r.Header.Get("Origin"); o != "" && o != "http://"+addr {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

func (s *server) index(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: "mv2ios", Value: s.token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src 'self' data:; connect-src 'self'")
	w.Write(indexHTML)
}

// broadcast records ev and sends it to every open page.
func (s *server) broadcast(ev event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.job != "" {
		ev.Job = s.job
	}
	s.history = append(s.history, ev)
	if len(s.history) > 2000 {
		s.history = s.history[len(s.history)-1000:]
	}
	for ch := range s.subs {
		select {
		case ch <- ev:
		default: // a stalled page misses lines rather than stalling the job
		}
	}
}

func (s *server) logf(format string, args ...any) {
	s.broadcast(event{Kind: "log", Text: fmt.Sprintf(format, args...)})
}

func (s *server) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	ch := make(chan event, 256)
	s.mu.Lock()
	past := append([]event(nil), s.history...)
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.subs, ch)
		s.mu.Unlock()
	}()

	send := func(ev event) {
		b, _ := json.Marshal(ev)
		fmt.Fprintf(w, "data: %s\n\n", b)
	}
	for _, ev := range past {
		send(ev)
	}
	flusher.Flush()
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case ev := <-ch:
			send(ev)
			flusher.Flush()
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

// start runs fn as the one job allowed at a time.
func (s *server) start(w http.ResponseWriter, name string, fn func(ctx context.Context) (any, error)) {
	s.mu.Lock()
	if s.job != "" {
		s.mu.Unlock()
		http.Error(w, "Another step is still running.", http.StatusConflict)
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.job, s.cancel, s.history = name, cancel, nil
	s.mu.Unlock()

	go func() {
		result, err := fn(ctx)
		cancel()
		if err != nil {
			if errors.Is(err, context.Canceled) {
				err = errors.New("stopped")
			}
			s.broadcast(event{Kind: "error", Text: err.Error()})
		} else {
			s.broadcast(event{Kind: "done", Data: result})
		}
		s.mu.Lock()
		s.job, s.cancel = "", nil
		s.mu.Unlock()
	}()
	w.WriteHeader(http.StatusAccepted)
}

func (s *server) cancelJob(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	if s.cancel != nil {
		s.cancel()
	}
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) error {
	return json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(v)
}

// defaultArchive is ./mv2ios-archive when it already exists (as the command
// line uses), otherwise a folder in Documents.
func defaultArchive() string {
	if st, err := os.Stat("mv2ios-archive"); err == nil && st.IsDir() {
		if abs, err := filepath.Abs("mv2ios-archive"); err == nil {
			return abs
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "mv2ios-archive"
	}
	if st, err := os.Stat(filepath.Join(home, "Documents")); err == nil && st.IsDir() {
		return filepath.Join(home, "Documents", "mv2ios-archive")
	}
	return filepath.Join(home, "mv2ios-archive")
}

func countConversations(dir string) int {
	m, _ := filepath.Glob(filepath.Join(dir, "conversations", "*.json"))
	return len(m)
}

type backupJSON struct {
	Path       string `json:"path"`
	Device     string `json:"device"`
	IOS        string `json:"ios"`
	LastBackup string `json:"lastBackup"`
	Encrypted  bool   `json:"encrypted"`
	CanUndo    bool   `json:"canUndo"`
}

func (s *server) state(w http.ResponseWriter, r *http.Request) {
	archiveDir := r.URL.Query().Get("archive")
	if archiveDir == "" {
		archiveDir = defaultArchive()
	}
	_, err := os.Stat(s.cfg.SessionPath)
	list := iosbackup.List(iosbackup.DefaultDirs())
	backups := make([]backupJSON, 0, len(list))
	for _, b := range list {
		_, undoErr := iosbackup.LatestUndo(s.cfg.UndoRoot, filepath.Base(b.Path))
		backups = append(backups, backupJSON{
			Path: b.Path, Device: b.DeviceName, IOS: b.ProductVersion,
			LastBackup: b.LastBackup.Local().Format("2 Jan 2006, 15:04"),
			Encrypted:  b.Encrypted, CanUndo: undoErr == nil,
		})
	}
	s.mu.Lock()
	job := s.job
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"version":       s.cfg.Version,
		"paired":        err == nil,
		"sessionPath":   s.cfg.SessionPath,
		"archive":       archiveDir,
		"conversations": countConversations(archiveDir),
		"backups":       backups,
		"backupDirs":    iosbackup.DefaultDirs(),
		"job":           job,
		"donateURL":     DonateURL,
		"cookieURL":     gmessages.CookieURL,
	})
}

func (s *server) pair(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Method string `json:"method"` // browser, qr, paste
		Pasted string `json:"pasted"`
	}
	if err := readJSON(w, r, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.start(w, "pair", func(ctx context.Context) (any, error) {
		if req.Method == "qr" {
			s.logf("Waiting for the phone to scan the code.")
			return nil, gmessages.Pair(ctx, s.cfg.SessionPath, func(code string) {
				c, err := qr.Encode(code, qr.L)
				if err != nil {
					s.logf("Could not draw the QR code: %v", err)
					return
				}
				c.Scale = 8
				s.broadcast(event{Kind: "qr", Text: "data:image/png;base64," + base64.StdEncoding.EncodeToString(c.PNG())})
			})
		}
		var cookies map[string]string
		var err error
		if req.Method == "paste" {
			cookies, err = gmessages.ParseCookies(req.Pasted)
		} else {
			s.logf("A browser window is opening. Sign in with the Google account used by Google Messages on the phone.")
			s.logf("When the Google Messages page has loaded, close that window to continue.")
			cookies, err = gmessages.BrowserCookies(ctx)
			if err != nil && ctx.Err() == nil {
				s.broadcast(event{Kind: "paste", Text: err.Error()})
				return nil, fmt.Errorf("the browser sign-in did not work (%v). Copy the sign-in by hand instead", err)
			}
		}
		if err != nil {
			return nil, err
		}
		s.logf("Contacting your phone. Keep it unlocked with Google Messages open.")
		return nil, gmessages.PairGoogle(ctx, s.cfg.SessionPath, cookies, func(emoji string) {
			s.broadcast(event{Kind: "emoji", Text: emoji})
		})
	})
}

func (s *server) export(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Archive       string `json:"archive"`
		RefreshList   bool   `json:"refreshList"`
		RetryPreviews bool   `json:"retryPreviews"`
	}
	if err := readJSON(w, r, &req); err != nil || req.Archive == "" {
		http.Error(w, "choose a folder to save to", http.StatusBadRequest)
		return
	}
	s.start(w, "export", func(ctx context.Context) (any, error) {
		s.logf("Connecting. Keep the Android phone unlocked and online until this finishes.")
		client, err := gmessages.Open(ctx, s.cfg.SessionPath)
		if err != nil {
			return nil, err
		}
		defer client.Close()
		s.logf("Connected.")
		stats, err := client.Export(ctx, req.Archive, gmessages.ExportOptions{
			RefreshList: req.RefreshList, RetryPreviews: req.RetryPreviews, Parallel: 3,
		}, s.logf)
		switch {
		case errors.Is(err, gmessages.ErrListIncomplete):
			return stats, fmt.Errorf("%w. Nothing was exported. Unlock the phone, open Google Messages, and try again", err)
		case errors.Is(err, gmessages.ErrSignedOut):
			return stats, errors.New("Google stopped accepting this computer's sign-in. Pair again, then export again; finished conversations are kept")
		case errors.Is(err, context.Canceled):
			return stats, errors.New("stopped before finishing; export again to continue where it left off")
		}
		return stats, err
	})
}

func (s *server) inject(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Archive string `json:"archive"`
		Backup  string `json:"backup"`
		Write   bool   `json:"write"`
	}
	if err := readJSON(w, r, &req); err != nil || req.Backup == "" || req.Archive == "" {
		http.Error(w, "choose a backup", http.StatusBadRequest)
		return
	}
	s.start(w, "inject", func(ctx context.Context) (any, error) {
		s.logf("Backup: %s", req.Backup)
		return iosbackup.Inject(req.Backup, req.Archive, iosbackup.Options{DryRun: !req.Write, UndoRoot: s.cfg.UndoRoot, Logf: s.logf})
	})
}

func (s *server) undo(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Backup string `json:"backup"`
	}
	if err := readJSON(w, r, &req); err != nil || req.Backup == "" {
		http.Error(w, "choose a backup", http.StatusBadRequest)
		return
	}
	s.start(w, "undo", func(ctx context.Context) (any, error) {
		latest, err := iosbackup.LatestUndo(s.cfg.UndoRoot, filepath.Base(req.Backup))
		if err != nil {
			return nil, err
		}
		if err := iosbackup.Undo(latest); err != nil {
			return nil, err
		}
		s.logf("Restored the original backup files in %s", req.Backup)
		return nil, nil
	})
}

func (s *server) unpair(w http.ResponseWriter, r *http.Request) {
	s.start(w, "unpair", func(ctx context.Context) (any, error) {
		client, err := gmessages.Open(ctx, s.cfg.SessionPath)
		if err != nil {
			if strings.Contains(err.Error(), "not paired") {
				return nil, err
			}
			// Still delete the local sign-in so nothing is left behind.
			s.logf("Could not reach the phone (%v); removing the sign-in from this computer only.", err)
			return nil, os.Remove(s.cfg.SessionPath)
		}
		// No Close: it would save the session that Unpair just deleted.
		return nil, client.Unpair(ctx)
	})
}
