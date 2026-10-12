# pocketlink

A small phone companion for standalone Wayland compositors (sway, niri, …):
notifications, clipboard sync, file transfer, media remote control and call
handling, without KDE libraries. A phone can be paired with several
computers.

## Design

- **The phone always dials the desktop.** No UDP broadcast discovery; the
  phone remembers the desktop's addresses from pairing, so it also works over
  Tailscale/WireGuard.
- **Disconnects are expected.** Every frame has an ID and is acked. Outgoing
  frames sit in a per-device queue on disk (`$XDG_STATE_HOME/pocketlink/queue`)
  until acked and are resent on reconnect. Receivers dedupe by ID.
- **Pinned self-signed certs.** Pairing is a QR code holding the desktop's
  addresses, its certificate fingerprint and a one-time token (valid 5 min).
  The phone pins the desktop cert, and the desktop adds the phone's cert to
  `$XDG_CONFIG_HOME/pocketlink/devices.json`.
- Transport: TLS 1.3 + WebSocket on `/ws`, port 1764. One JSON envelope per
  message, see `internal/proto`.
- **Files go over plain HTTPS** on the same port, and both directions resume
  (see `internal/xfer`):
  - phone → desktop: `PUT /files/in/{id}` with `X-Pocketlink-Name`, `-Size` and
    `-Offset` headers; `HEAD` asks where to resume. A finished transfer
    answers 201 again rather than creating a duplicate. A resumed upload
    takes over from a stalled one on a dead connection.
  - desktop → phone: a `file.offer` frame, then `GET /files/out/{id}` with
    `Range`, then a `file.done` frame. Offers expire after 7 days.
  - Received files go to `$XDG_DOWNLOAD_DIR`. The desktop notification has
    Open / Show in folder actions.
- **Live requests** are the exception to queueing: a frame with a `req` ID is
  answered by exactly one `reply` frame, and fails instead of waiting if the
  phone isn't connected. The desktop uses them to browse and change the
  phone's files (`fs.*`, see `internal/proto/fs.go`), with file data in binary
  WebSocket frames, and serves them to file managers over WebDAV
  (`internal/davfs`).

## Install

Arch Linux: [`pocketlink-git`](https://aur.archlinux.org/packages/pocketlink-git)
from the AUR, then `systemctl --user enable --now pocketlink`.

Android: the APK is attached to the
[latest release](https://github.com/carnager/pocketlink/releases/latest),
or build it yourself (below). [Obtainium](https://obtainium.imranr.dev/) can
install and update it straight from this repository.

If the notification access switch is greyed out ("restricted setting"),
Android is blocking it because the APK was installed from a browser download:
open the app's *App info*, tap ⋮ → *Allow restricted settings*, and try again.
Installing with Obtainium, F-Droid or `adb install` avoids this.

From source, desktop (needs Go ≥ 1.24, `wl-clipboard`, a notification daemon, and
`wpctl` for lowering the volume during calls):

```sh
go install github.com/carnager/pocketlink/cmd/pocketlink@latest   # into ~/go/bin
# or from a checkout:
go build -o ~/.local/bin/ ./cmd/pocketlink

cp contrib/pocketlink.service ~/.config/systemd/user/   # adjust ExecStart if needed
systemctl --user enable --now pocketlink
```

Android app (Android 10+):

```sh
cd android && ./gradlew assembleRelease   # ~3 MB, minified
adb install app/build/outputs/apk/release/app-release.apk
```

Without signing keys configured this is signed with your debug key; see
[android/SIGNING.md](android/SIGNING.md) for release signing.

The daemon needs `WAYLAND_DISPLAY` in the systemd user environment for clipboard
sync. niri-session sets it up; on sway add
`exec systemctl --user import-environment WAYLAND_DISPLAY` to your config.
Open TCP 1764 in your firewall if you run one.

```sh
pocketlink pair           # show QR code
pocketlink status         # paired devices, connection state, queued frames
pocketlink unpair NAME
wl-paste | pocketlink clip
pocketlink send [-to NAME] FILE|FOLDER...
```

Several files, or folders, go as a single tar stream that the phone unpacks
as it arrives, several files at a time: hundreds of small files take about a
seventh of the time they would one by one. A folder lands as a folder of its
name in Downloads (or the folder picked in the app). Interrupted transfers
continue where they stopped, without duplicates.

Desktop clipboard changes go to phones automatically (`-clipboard=false` turns
that off), text and images (up to 20 MB, e.g. a `grim` screenshot) alike;
a copied image lands on the phone's clipboard, ready to paste. "Send
clipboard" on the phone (tile, notification or app) sends images too.
Selections that password managers mark with `x-kde-passwordManagerHint` are
skipped.

Settings live in `~/.config/pocketlink/config.json`:

```sh
pocketlink config                      # show
pocketlink config downloads ~/Inbox    # applies immediately
pocketlink config clipboard false      # applies immediately
pocketlink config name laptop          # needs a daemon restart
pocketlink config ring_action lower    # while the phone rings: pause | lower | none
pocketlink config talk_action pause    # during a call: pause | lower | none
pocketlink config call_volume 40       # "lower" turns the volume down to 40% of what it was
pocketlink config files off            # needs a daemon restart; default 127.0.0.1:1765
pocketlink config file_manager dolphin # for "Show in folder"; empty asks the desktop's file manager service
```

## Phone files in your file manager

Turn on *File access* in the app's settings, then open
`webdav://127.0.0.1:1765/` in Dolphin, or `dav://127.0.0.1:1765/` in Thunar
or Nautilus (Thunar needs `gvfs`), and bookmark it. Each paired phone is a
folder there. You can browse, open, copy, move, rename and delete files
both ways. `pocketlink status` shows the address.

The server only listens on localhost and only accepts your own user's
programs (it checks who owns the connecting socket), so it needs no
password. Opening a folder costs one round trip to the phone, and the
phone reports changes to folders you've opened, so going back to one is
instant. Over a slow link, keep previews of remote files off (or limited in
size) in your file manager's preview settings; otherwise it downloads
every image to make a thumbnail.

## "Send to phone" in file managers

The AUR package adds *Send to phone* to the right-click menu of Dolphin,
Thunar (*Send To → Phone*), Nemo and Nautilus (Nautilus needs
`nautilus-python`). It sends the selected files and folders to all paired phones and
reports the result as a desktop notification (`pocketlink send -notify`).
Installing from source, copy them from `contrib/filemanager`:

```sh
cd contrib/filemanager
install -Dm755 pocketlink-send.servicemenu.desktop ~/.local/share/kio/servicemenus/pocketlink-send.desktop
install -Dm644 pocketlink.thunar-sendto.desktop ~/.local/share/Thunar/sendto/pocketlink.desktop
install -Dm644 pocketlink-send.nemo_action -t ~/.local/share/nemo/actions/
install -Dm644 pocketlink-nautilus.py ~/.local/share/nautilus-python/extensions/pocketlink.py
```

(Dolphin only runs service menus from your home directory if they're
executable.)

## Media remote and calls

The daemon follows MPRIS players (anything with media controls) and the
phone shows the active one as a regular media notification: play/pause,
next/previous, seeking, album art, and the phone's volume keys control the
player while the notification is active.

With "Phone calls" enabled in the app, an incoming call shows on the
desktop with a *Mute ringer* action (and as *Missed call* if not answered).
Media is paused or the volume lowered, separately for ringing
(`ring_action`) and talking (`talk_action`), and restored when the call
ends. Volume changes use `wpctl` (PipeWire).

## DankMaterialShell plugin

`contrib/dms/pocketlink` is a bar widget: connection state, pairing with an
on-screen QR code, sending files, opening phones' files in a file manager,
unpairing, and the download folder and clipboard settings. The file manager
is the first of Dolphin, Thunar, Nautilus, Nemo and PCManFM that's
installed, unless you pick one in the plugin settings.

```sh
ln -s "$PWD/contrib/dms/pocketlink" ~/.config/DankMaterialShell/plugins/pocketlink
```

Then enable it in DMS settings → Plugins and add the widget to the bar. It
runs `pocketlink` from `PATH` (set a different binary in the plugin settings).

## Testing without a phone

`pocketlink-sim` acts like a phone:

```sh
go build -o bin/ ./cmd/...
bin/pocketlink-sim pair "$(bin/pocketlink pair | grep ^pocketlink://)"
bin/pocketlink-sim run    # then type: "title | body", "/rm sim-1", "/clip text",
                      #   "/send FILE", "/sendcut FILE BYTES"
```

## Layout

```
cmd/pocketlink        daemon + CLI
cmd/pocketlink-sim    fake phone for testing
internal/proto    wire format
internal/pair     identity, trust store, pairing offer/token
internal/conn     TLS WebSocket hub, sessions, ack queue
internal/notify   org.freedesktop.Notifications sink
internal/clip     wl-clipboard integration
internal/control  unix socket CLI <-> daemon
internal/xfer     resumable HTTPS file transfer
internal/media    MPRIS players for the media remote
internal/config   config.json
android/          the phone app (Kotlin, Compose)
contrib/dms       DankMaterialShell plugin
```

## License

GPL-3.0-or-later, see [LICENSE](LICENSE).
