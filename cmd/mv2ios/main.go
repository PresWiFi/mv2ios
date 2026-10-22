// Command mv2ios moves Google Messages history, including RCS and group
// chats, into the Messages app on an iPhone.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"github.com/mdp/qrterminal/v3"

	"github.com/preswifi/mv2ios/internal/gmessages"
	"github.com/preswifi/mv2ios/internal/gui"
	"github.com/preswifi/mv2ios/internal/iosbackup"
)

const usage = `mv2ios moves your Android message history, including RCS and group chats,
into the Messages app on an iPhone.

Run "mv2ios" with no step, or "mv2ios gui", to use it from a window instead.

Steps, in order:

  mv2ios pair       Link this computer to Google Messages on the Android phone
  mv2ios export     Download every conversation and attachment to this computer
  mv2ios backups    List the iPhone backups found on this computer
  mv2ios inject     Add the downloaded conversations to an iPhone backup
  mv2ios undo       Put a backup back the way it was before inject
  mv2ios unpair     Remove this computer from the Android phone's paired devices

Run "mv2ios <step> -h" for the options of a step.
`

// version is set at build time with -ldflags "-X main.version=v1.2.3".
var version = "dev"

func configDir() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = "."
	}
	return filepath.Join(dir, "mv2ios")
}

func sessionPath() string { return filepath.Join(configDir(), "session.json") }
func undoRoot() string    { return filepath.Join(configDir(), "undo") }

func logf(format string, args ...any) {
	fmt.Printf(format+"\n", args...)
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if len(os.Args) < 2 {
		// Started by double-clicking, most likely: open the window.
		if err := runGUI(ctx); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}

	var err error
	switch cmd, args := os.Args[1], os.Args[2:]; cmd {
	case "pair":
		err = pair(ctx, args)
	case "export":
		err = export(ctx, args)
	case "unpair":
		err = unpair(ctx)
	case "backups":
		err = backups()
	case "inject":
		err = inject(args)
	case "undo":
		err = undo(args)
	case "gui":
		err = runGUI(ctx)
	case "version", "-version", "--version":
		fmt.Println("mv2ios", version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown step %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func pair(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("pair", flag.ExitOnError)
	qr := fs.Bool("qr", false, "pair by scanning a QR code instead of through your Google account")
	cookieFile := fs.String("cookies", "", "read the Google sign-in cookies from this file instead of opening a browser")
	paste := fs.Bool("paste", false, "paste the Google sign-in cookies instead of opening a browser")
	fs.Parse(args)

	var err error
	if *qr {
		err = pairQR(ctx)
	} else {
		err = pairGoogle(ctx, *cookieFile, *paste)
	}
	if err != nil {
		return err
	}
	fmt.Println("Paired. Next: mv2ios export")
	return nil
}

func pairQR(ctx context.Context) error {
	fmt.Println("On the Android phone open Google Messages, tap your profile picture,")
	fmt.Println("choose \"Device pairing\", then \"QR code scanner\", and scan this code:")
	return gmessages.Pair(ctx, sessionPath(), func(qr string) {
		fmt.Println()
		qrterminal.GenerateHalfBlock(qr, qrterminal.L, os.Stdout)
	})
}

const pasteHelp = `
  1. Open a private window (Incognito in Chrome, InPrivate in Edge) and go to:
     %s
     Sign in with the Google account used by Google Messages on the phone.
     A private window matters: a normal window keeps using the sign-in and
     Google soon cancels the copy mv2ios holds.
  2. Once the page has loaded, press F12, open the Network tab, and press F5.
  3. Right-click the request named "config", choose Copy > Copy as cURL (bash).

Close the private window after copying, WITHOUT signing out.
Paste what you copied here, then press Enter on an empty line:
`

func pastedCookies() (map[string]string, error) {
	fmt.Printf(pasteHelp, gmessages.CookieURL)
	// A cURL command spans several lines, so read until a blank one.
	var text []byte
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for scanner.Scan() && strings.TrimSpace(scanner.Text()) != "" {
		text = append(append(text, scanner.Bytes()...), '\n')
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return gmessages.ParseCookies(string(text))
}

func pairGoogle(ctx context.Context, cookieFile string, paste bool) error {
	fmt.Println("Google Messages on your phone pairs through your Google account.")
	fmt.Printf("The sign-in is saved only on this computer, in %s,\nand \"mv2ios unpair\" deletes it.\n\n", sessionPath())

	var cookies map[string]string
	var err error
	switch {
	case cookieFile != "":
		var text []byte
		if text, err = os.ReadFile(cookieFile); err == nil {
			cookies, err = gmessages.ParseCookies(string(text))
		}
	case paste:
		cookies, err = pastedCookies()
	default:
		fmt.Println("A browser window is opening. Sign in with the Google account used by")
		fmt.Println("Google Messages on the phone. When the Google Messages page has loaded,")
		fmt.Println("CLOSE THE WINDOW to continue.")
		if cookies, err = gmessages.BrowserCookies(ctx); err != nil && ctx.Err() == nil {
			fmt.Printf("\nThe browser sign-in did not work (%v).\nFalling back to copying the sign-in by hand.\n", err)
			cookies, err = pastedCookies()
		}
	}
	if err != nil {
		return err
	}
	fmt.Println("\nContacting your phone. Keep it unlocked with Google Messages open.")
	return gmessages.PairGoogle(ctx, sessionPath(), cookies, func(emoji string) {
		fmt.Printf("\nYour phone is showing three emoji. Tap this one:  %s\n", emoji)
	})
}

func export(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("export", flag.ExitOnError)
	out := fs.String("out", "mv2ios-archive", "folder to save conversations and attachments in")
	refresh := fs.Bool("refresh-list", false, "fetch the conversation list from the phone again instead of reusing the saved one")
	listOnly := fs.Bool("list-only", false, "only fetch the conversation list, then stop")
	parallel := fs.Int("parallel", 3, "how many conversations to export at the same time")
	retryPreviews := fs.Bool("retry-previews", false, "export again the saved conversations that have photos saved only as previews")
	fs.Parse(args)

	fmt.Println("Connecting. Keep the Android phone unlocked and online until this finishes.")
	client, err := gmessages.Open(ctx, sessionPath())
	if err != nil {
		return err
	}
	defer client.Close()
	fmt.Println("Connected.")
	stats, err := client.Export(ctx, *out, gmessages.ExportOptions{RefreshList: *refresh, ListOnly: *listOnly, RetryPreviews: *retryPreviews, Parallel: *parallel}, logf)
	if errors.Is(err, gmessages.ErrListIncomplete) {
		return fmt.Errorf("%w.\nNothing was exported, so no conversations are left behind unnoticed. Unlock the phone,\nopen Google Messages, and run export again", err)
	}
	if *listOnly {
		return err
	}
	fmt.Printf("\nSaved %d conversations (%d messages, %d attachments) to %s\n",
		stats.Conversations, stats.Messages, stats.Media, *out)
	if stats.Skipped > 0 {
		fmt.Printf("%d conversations were already saved by an earlier run and were left alone.\n", stats.Skipped)
	}
	if stats.Previews > 0 {
		fmt.Printf("%d attachments were only available as small previews; the phone did not send the originals.\n", stats.Previews)
		fmt.Println("Run \"mv2ios export -retry-previews\" to ask for them again.")
	}
	if stats.MediaFailed > 0 {
		fmt.Printf("%d attachments could not be downloaded; their messages were saved without them.\n", stats.MediaFailed)
	}
	if len(stats.Partial) > 0 {
		fmt.Printf("%d conversations stopped early when the phone did not answer; their oldest messages may be missing: %q\n", len(stats.Partial), stats.Partial)
	}
	if len(stats.Failed) > 0 {
		fmt.Printf("%d conversations failed and were NOT saved: %q\nRun export again to retry them.\n", len(stats.Failed), stats.Failed)
	}
	if errors.Is(err, gmessages.ErrSignedOut) {
		return errors.New("Google stopped accepting this computer's sign-in.\n" +
			"Run \"mv2ios pair -paste\" again, then run export again; finished conversations are kept")
	}
	if errors.Is(err, context.Canceled) {
		return errors.New("stopped before finishing; run export again to continue where it left off")
	}
	return err
}

func unpair(ctx context.Context) error {
	client, err := gmessages.Open(ctx, sessionPath())
	if err != nil {
		return err
	}
	if err := client.Unpair(ctx); err != nil {
		return err
	}
	fmt.Println("Unpaired.")
	return nil
}

func backups() error {
	dirs := iosbackup.DefaultDirs()
	list := iosbackup.List(dirs)
	if len(list) == 0 {
		return fmt.Errorf("no iPhone backups found in %q; pass the backup folder to inject with -backup", dirs)
	}
	for _, b := range list {
		note := ""
		if b.Encrypted {
			note = "  (encrypted: not supported yet)"
		}
		fmt.Printf("%s  iOS %s  %s%s\n    %s\n", b.DeviceName, b.ProductVersion, b.LastBackup.Local().Format("2006-01-02 15:04"), note, b.Path)
	}
	return nil
}

// pickBackup uses the given folder, or the only backup on this computer.
func pickBackup(path string) (string, error) {
	if path != "" {
		return path, nil
	}
	list := iosbackup.List(iosbackup.DefaultDirs())
	switch len(list) {
	case 0:
		return "", errors.New("no iPhone backups found; pass the backup folder with -backup")
	case 1:
		return list[0].Path, nil
	}
	return "", errors.New("more than one iPhone backup found; run \"mv2ios backups\" and pass one with -backup")
}

func inject(args []string) error {
	fs := flag.NewFlagSet("inject", flag.ExitOnError)
	backup := fs.String("backup", "", "iPhone backup folder (default: the only backup on this computer)")
	archiveDir := fs.String("archive", "mv2ios-archive", "folder written by export")
	write := fs.Bool("write", false, "actually change the backup; without this, only report what would change")
	fs.Parse(args)

	dir, err := pickBackup(*backup)
	if err != nil {
		return err
	}
	fmt.Println("Backup:", dir)
	res, err := iosbackup.Inject(dir, *archiveDir, iosbackup.Options{DryRun: !*write, UndoRoot: undoRoot(), Logf: logf})
	if err != nil {
		return err
	}
	verb := "Would add"
	if *write {
		verb = "Added"
	}
	fmt.Printf("\n%s %d messages and %d attachments across %d conversations (%d of them new threads).\n",
		verb, res.Messages, res.Attachments, res.Conversations, res.NewChats)
	fmt.Printf("%d messages were already on the iPhone and were skipped.\n", res.Duplicates)
	if res.MissingAttachments > 0 {
		fmt.Printf("%d attachments were missing from the archive and were left out.\n", res.MissingAttachments)
	}
	if res.SkippedConversations > 0 {
		fmt.Printf("%d conversations had no messages or no usable phone number and were skipped.\n", res.SkippedConversations)
	}
	switch {
	case !*write:
		fmt.Println("\nNothing was changed. Run again with -write to modify the backup.")
	case res.UndoDir != "":
		fmt.Println("\nThe backup is ready. Restore it to the iPhone with Finder, iTunes or Apple Devices.")
		fmt.Println("To put the backup back as it was: mv2ios undo -backup", dir)
	}
	return nil
}

func undo(args []string) error {
	fs := flag.NewFlagSet("undo", flag.ExitOnError)
	backup := fs.String("backup", "", "iPhone backup folder (default: the only backup on this computer)")
	fs.Parse(args)

	dir, err := pickBackup(*backup)
	if err != nil {
		return err
	}
	latest, err := iosbackup.LatestUndo(undoRoot(), filepath.Base(dir))
	if err != nil {
		return err
	}
	if err := iosbackup.Undo(latest); err != nil {
		return err
	}
	fmt.Println("Restored the original backup files in", dir)
	return nil
}

func runGUI(ctx context.Context) error {
	return gui.Run(ctx, gui.Config{Version: version, SessionPath: sessionPath(), UndoRoot: undoRoot()})
}
