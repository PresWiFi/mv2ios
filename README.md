# mv2ios

Moves Android message history, including RCS and group chats that "Move to iOS" skips, into iPhone Messages. Runs locally; nothing is uploaded.

> **Status: untested on real devices.** Tested only against a synthetic backup. Use only on an iPhone you're willing to set up again.

## How it works

1. **Export:** pairs with Google Messages like Messages for Web and downloads all conversations and attachments. No root.
2. **Back up the iPhone** (Finder, iTunes, Apple Devices or `idevicebackup2`), unencrypted.
3. **Inject** the messages into the backup. Duplicates are skipped.
4. **Restore** with the same software.

## Usage

### GUI

Run the download from [Releases](https://github.com/PresWiFi/mv2ios/releases). A local browser page walks through each step. Close the mv2ios window to quit.

- **Windows:** SmartScreen → *More info > Run anyway*.
- **macOS:** right-click → *Open* the first time (unsigned).

### CLI

```
mv2ios pair                 # sign in to your Google account in the window that opens
mv2ios pair -qr             # or scan a QR code, on phones that still offer it
mv2ios export               # writes ./mv2ios-archive
mv2ios backups              # list iPhone backups on this computer
mv2ios inject               # report what would be added; changes nothing
mv2ios inject -write        # modify the backup
mv2ios undo                 # put the backup back as it was
mv2ios unpair               # remove this computer from the Android phone
mv2ios gui                  # open the browser page (same as running with no step)
```

`pair` uses a temporary Chrome/Edge/Brave profile to get the Google sign-in, stored locally until `unpair`. If that fails, use `pair -paste`.

`export` resumes where it stopped. `inject -write` backs up the originals for `undo`.

Before restoring, turn off **Settings > [your name] > iCloud > Messages** so iCloud doesn't overwrite the history. Turn it back on after.

## Limitations

- **No encrypted backups yet.** Unencrypted restores lose passwords, Wi-Fi, Health and call history, so use it on a fresh iPhone right after "Move to iOS".
- **Google Messages only.** Samsung Messages has no web pairing.
- Imported RCS messages appear as green "Text Message" bubbles.
- Reactions are saved in the export but not yet shown on the iPhone.
- Attachments are at paired-device quality.

## Building

Needs Go 1.26 or newer. No C compiler is required.

```
go build -o mv2ios ./cmd/mv2ios
go test ./...
```

Set `MV2IOS_DEBUG=1` to see the Google Messages protocol log.
Set `MV2IOS_NO_BROWSER=1` to stop `gui` from opening a browser by itself.

Push a tag like `v0.1.0`; GitHub Actions attaches Windows, macOS and Linux binaries to a draft release.

## Support

[Buy me a coffee](https://buymeacoffee.com/blakeriding).

## License

AGPL-3.0. Uses [libgm](https://github.com/mautrix/gmessages) (AGPL).
