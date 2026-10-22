# mv2ios

Move your Android message history into the Messages app on an iPhone,
including the RCS chats and group chats that Apple's "Move to iOS" leaves behind.

Free and open source. Nothing leaves your computer: messages go from your
Android phone to a folder on your disk, and from there into your own iPhone backup.

> **Status: early and not yet proven on real devices.** The Android export and
> the backup changes are implemented and tested against a synthetic backup.
> Nobody has yet restored a modified backup to a real iPhone with this tool.
> Until that is confirmed, try it only on an iPhone you are prepared to set up again.

## How it works

1. **Export from Android.** mv2ios pairs with Google Messages the same way
   "Messages for Web" does and downloads every conversation and attachment.
   No root needed.
2. **Back up the iPhone** to your computer with Finder, iTunes, Apple Devices
   or `idevicebackup2`. The backup must not be encrypted (see Limitations).
3. **Inject.** mv2ios adds the exported conversations to the Messages database
   inside that backup. Messages that are already on the iPhone are skipped.
4. **Restore the backup** to the iPhone with the same software that made it.

## Usage

### The easy way

Download mv2ios for your computer from the
[Releases page](https://github.com/PresWiFi/mv2ios/releases) and double-click it.
A page opens in your web browser that walks you through every step: link the
Android phone, download the messages, pick the iPhone backup, and add them to it.
Keep the small mv2ios window open while you use it; close it to quit.

- **Windows:** if SmartScreen warns about an unknown app, choose
  *More info > Run anyway*.
- **macOS:** right-click the file and choose *Open* the first time, because
  the app is not signed by Apple.

The page only works on this computer and only from the address mv2ios opens.

### The command line

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

`pair` opens a Chrome, Edge or Brave window with a temporary profile and reads
the Google sign-in from it. The sign-in is stored only on your computer and
`mv2ios unpair` deletes it. If no such browser is installed, or Google refuses
the sign-in, `pair -paste` lets you copy it from your own browser by hand.

`export` can be stopped and run again; it continues where it left off.
`inject -write` saves the original files first, and `undo` puts them back.

Before restoring, turn off **Settings > [your name] > iCloud > Messages** on
the iPhone, so iCloud does not overwrite the restored history. Turn it back on
after checking that the messages arrived.

## Limitations

- **Encrypted backups are not supported yet.** An unencrypted backup does not
  contain saved passwords, Wi-Fi passwords, Health data or call history, so
  restoring one loses those. The best moment to use mv2ios is on a freshly set
  up iPhone, straight after "Move to iOS".
- **Google Messages only.** Samsung Messages has no web pairing.
- Imported RCS messages appear as green "Text Message" bubbles.
- Reactions are saved in the export but not yet shown on the iPhone.
- Attachments arrive in the quality Google Messages gives to paired devices.

## Building

Needs Go 1.26 or newer. No C compiler is required.

```
go build -o mv2ios ./cmd/mv2ios
go test ./...
```

Set `MV2IOS_DEBUG=1` to see the Google Messages protocol log.
Set `MV2IOS_NO_BROWSER=1` to stop `gui` from opening a browser by itself.

Releases are built by GitHub Actions: push a tag like `v0.1.0` and the
Windows, macOS and Linux binaries are attached to a draft release.

## Support

mv2ios is free. If it saved your move to iPhone and you want to say thanks,
you can [buy me a coffee](https://buymeacoffee.com/blakeriding).

## License

AGPL-3.0, see [LICENSE](LICENSE). The Android export is built on
[libgm](https://github.com/mautrix/gmessages) from the mautrix-gmessages
project, which is AGPL licensed.
