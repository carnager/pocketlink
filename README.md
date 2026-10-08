# tether

A small phone companion for standalone Wayland compositors (sway, niri, …):
notifications, clipboard sync, file transfer, media remote control and call
handling, without KDE libraries. A phone can be paired with several
computers.

## Design

- **The phone always dials the desktop.** No UDP broadcast discovery; the
  phone remembers the desktop's addresses from pairing, so it also works over
  Tailscale/WireGuard.
- **Disconnects are expected.** Every frame has an ID and is acked. Outgoing
  frames sit in a per-device queue on disk (`$XDG_STATE_HOME/tether/queue`)
  until acked and are resent on reconnect. Receivers dedupe by ID.
- **Pinned self-signed certs.** Pairing is a QR code holding the desktop's
  addresses, its certificate fingerprint and a one-time token (valid 5 min).
  The phone pins the desktop cert, and the desktop adds the phone's cert to
  `$XDG_CONFIG_HOME/tether/devices.json`.
- Transport: TLS 1.3 + WebSocket on `/ws`, port 1764. One JSON envelope per
  message, see `internal/proto`.
- **Files go over plain HTTPS** on the same port, and both directions resume
  (see `internal/xfer`):
  - phone → desktop: `PUT /files/in/{id}` with `X-Tether-Name`, `-Size` and
    `-Offset` headers; `HEAD` asks where to resume. A finished transfer
    answers 201 again rather than creating a duplicate. A resumed upload
    takes over from a stalled one on a dead connection.
  - desktop → phone: a `file.offer` frame, then `GET /files/out/{id}` with
    `Range`, then a `file.done` frame. Offers expire after 7 days.
  - Received files go to `$XDG_DOWNLOAD_DIR`. The desktop notification has
    Open / Show in folder actions.

## Build & run

```sh
go build -o ~/.local/bin/ ./cmd/tether
cp contrib/tether.service ~/.config/systemd/user/
systemctl --user enable --now tether
```

The daemon needs `WAYLAND_DISPLAY` in the systemd user environment for clipboard
sync. niri-session sets it up; on sway add
`exec systemctl --user import-environment WAYLAND_DISPLAY` to your config.
Open TCP 1764 in your firewall if you run one.

```sh
tether pair           # show QR code
tether status         # paired devices, connection state, queued frames
tether unpair NAME
wl-paste | tether clip
tether send [-to NAME] FILE...
```

Desktop clipboard changes go to phones automatically (`-clipboard=false` turns
that off). Selections that password managers mark with
`x-kde-passwordManagerHint` are skipped.

Settings live in `~/.config/tether/config.json`:

```sh
tether config                      # show
tether config downloads ~/Inbox    # applies immediately
tether config clipboard false      # applies immediately
tether config name laptop          # needs a daemon restart
tether config ring_action lower    # while the phone rings: pause | lower | none
tether config talk_action pause    # during a call: pause | lower | none
tether config call_volume 40       # "lower" turns the volume down to 40% of what it was
```

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

`contrib/dms/tether` is a bar widget: connection state, pairing with an
on-screen QR code, sending files, unpairing, and the download folder and
clipboard settings.

```sh
ln -s "$PWD/contrib/dms/tether" ~/.config/DankMaterialShell/plugins/tether
```

Then enable it in DMS settings → Plugins and add the widget to the bar. It
runs `tether` from `PATH` (set a different binary in the plugin settings).

## Testing without a phone

`tether-sim` acts like a phone:

```sh
go build -o bin/ ./cmd/...
bin/tether-sim pair "$(bin/tether pair | grep ^tether://)"
bin/tether-sim run    # then type: "title | body", "/rm sim-1", "/clip text",
                      #   "/send FILE", "/sendcut FILE BYTES"
```

## Layout

```
cmd/tether        daemon + CLI
cmd/tether-sim    fake phone for testing
internal/proto    wire format
internal/pair     identity, trust store, pairing offer/token
internal/conn     TLS WebSocket hub, sessions, ack queue
internal/notify   org.freedesktop.Notifications sink
internal/clip     wl-clipboard integration
internal/control  unix socket CLI <-> daemon
internal/xfer     resumable HTTPS file transfer
```
