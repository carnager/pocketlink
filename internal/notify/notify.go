// Package notify mirrors phone notifications to the desktop notification
// daemon (mako, swaync, dunst, ...) via org.freedesktop.Notifications.
package notify

import (
	"bytes"
	"fmt"
	"image"
	"image/draw"
	_ "image/jpeg" // contact photos
	_ "image/png"  //
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/godbus/dbus/v5"

	"github.com/carnager/pocketlink/internal/pair"
	"github.com/carnager/pocketlink/internal/proto"
)

const (
	busName = "org.freedesktop.Notifications"
	objPath = "/org/freedesktop/Notifications"
)

var escapeMarkup = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

type Sink struct {
	obj  dbus.BusObject
	conn *dbus.Conn

	fmMu        sync.Mutex
	fileManager string // command for "Show in folder"; "" for the desktop's own

	mu      sync.Mutex
	ids     map[string]uint32                     // device id + "/" + phone key -> desktop notification id
	actions map[uint32]func(action, token string) // desktop notification id -> action handler
	tokens  map[uint32]string                     // activation token sent with the latest click
}

func New() (*Sink, error) {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return nil, err
	}
	s := &Sink{
		obj:     conn.Object(busName, objPath),
		conn:    conn,
		ids:     map[string]uint32{},
		actions: map[uint32]func(string, string){},
		tokens:  map[uint32]string{},
	}
	// ActivationToken comes right before ActionInvoked: what a program
	// started for the click needs to be allowed to raise its window.
	for _, member := range []string{"ActivationToken", "ActionInvoked", "NotificationClosed"} {
		err := conn.AddMatchSignal(
			dbus.WithMatchObjectPath(objPath),
			dbus.WithMatchInterface(busName),
			dbus.WithMatchMember(member),
		)
		if err != nil {
			return nil, err
		}
	}
	signals := make(chan *dbus.Signal, 16)
	conn.Signal(signals)
	go s.handleSignals(signals)
	return s, nil
}

// Notice shows a one-off notification without actions, for commands that
// have no other way to report back, such as a file manager's "Send to phone".
func Notice(icon, title, body string) error {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return err
	}
	defer conn.Close()
	return conn.Object(busName, objPath).Call(busName+".Notify", 0,
		"pocketlink", uint32(0), icon, title, escapeMarkup.Replace(body),
		[]string{}, map[string]dbus.Variant{}, int32(-1),
	).Err
}

// Posted shows a notification, replacing the previous one with the same key
// so updates (e.g. a growing message thread) don't stack up.
func (s *Sink) Posted(dev pair.Device, n proto.NotifPosted) error {
	k := dev.ID + "/" + n.Key
	s.mu.Lock()
	replaces := s.ids[k]
	s.mu.Unlock()

	app := n.AppName
	if app == "" {
		app = dev.Name
	}
	id, err := s.show(app, replaces, "phone", n.Title, n.Text, nil, nil)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.ids[k] = id
	s.mu.Unlock()
	return nil
}

// Removed closes the desktop copy of a notification dismissed on the phone.
func (s *Sink) Removed(dev pair.Device, key string) error {
	k := dev.ID + "/" + key
	s.mu.Lock()
	id, ok := s.ids[k]
	delete(s.ids, k)
	s.mu.Unlock()
	if !ok {
		return nil
	}
	return s.obj.Call(busName+".CloseNotification", 0, id).Err
}

// FileReceived announces a saved file, with actions to open it or its
// folder, plus "Copy to clipboard" when onCopy is given (for images).
func (s *Sink) FileReceived(dev pair.Device, path string, onCopy func() error) error {
	actions := []string{"default", "Open", "folder", "Show in folder"}
	if onCopy != nil {
		actions = append(actions, "copy", "Copy to clipboard")
	}
	_, err := s.show("pocketlink", 0, "document-save", "File from "+dev.Name, filepath.Base(path), actions,
		func(action, token string) {
			switch action {
			case "default":
				xdgOpen(path, token)
			case "folder":
				s.showInFolder(path, token)
			case "copy":
				if err := onCopy(); err != nil {
					slog.Warn("copying received file to the clipboard", "path", path, "err", err)
				}
			}
		})
	return err
}

// IncomingCall shows (or updates) a ringing call with a Mute action.
func (s *Sink) IncomingCall(dev pair.Device, replaces uint32, caller string, photo []byte, onMute func()) (uint32, error) {
	return s.showWith(dev.Name, replaces, "call-start", "Incoming call", caller, imageHints(photo),
		[]string{"mute", "Mute ringer"},
		func(action, _ string) {
			if action == "mute" {
				onMute()
			}
		})
}

// MissedCall replaces the ringing notification with a missed-call one.
func (s *Sink) MissedCall(dev pair.Device, replaces uint32, caller string, photo []byte) error {
	_, err := s.showWith(dev.Name, replaces, "call-missed", "Missed call", caller, imageHints(photo), nil, nil)
	return err
}

// imageData is the freedesktop "image-data" hint, signature (iiibiiay).
type imageData struct {
	Width, Height, Rowstride int32
	HasAlpha                 bool
	BitsPerSample, Channels  int32
	Data                     []byte
}

// imageHints turns an encoded image (e.g. a contact photo) into the
// "image-data" hint, which notification daemons show instead of the icon.
// Anything that doesn't decode just gets no image.
func imageHints(encoded []byte) map[string]dbus.Variant {
	if len(encoded) == 0 {
		return nil
	}
	img, _, err := image.Decode(bytes.NewReader(encoded))
	if err != nil {
		slog.Warn("notification image", "err", err)
		return nil
	}
	b := img.Bounds()
	rgba := image.NewNRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(rgba, rgba.Bounds(), img, b.Min, draw.Src)
	return map[string]dbus.Variant{"image-data": dbus.MakeVariant(imageData{
		Width: int32(b.Dx()), Height: int32(b.Dy()), Rowstride: int32(rgba.Stride),
		HasAlpha: true, BitsPerSample: 8, Channels: 4, Data: rgba.Pix,
	})}
}

// Close removes a notification shown by this sink.
func (s *Sink) Close(id uint32) error {
	return s.obj.Call(busName+".CloseNotification", 0, id).Err
}

func (s *Sink) show(app string, replaces uint32, icon, title, body string, actions []string, onAction func(action, token string)) (uint32, error) {
	return s.showWith(app, replaces, icon, title, body, nil, actions, onAction)
}

func (s *Sink) showWith(app string, replaces uint32, icon, title, body string, hints map[string]dbus.Variant, actions []string, onAction func(action, token string)) (uint32, error) {
	if actions == nil {
		actions = []string{}
	}
	if hints == nil {
		hints = map[string]dbus.Variant{}
	}
	var id uint32
	err := s.obj.Call(busName+".Notify", 0,
		app, replaces, icon, title, escapeMarkup.Replace(body),
		actions, hints, int32(-1),
	).Store(&id)
	if err != nil {
		return 0, err
	}
	if onAction != nil {
		s.mu.Lock()
		s.actions[id] = onAction
		s.mu.Unlock()
	}
	return id, nil
}

func (s *Sink) handleSignals(signals <-chan *dbus.Signal) {
	for sig := range signals {
		if len(sig.Body) < 2 {
			continue
		}
		id, ok := sig.Body[0].(uint32)
		if !ok {
			continue
		}
		arg, _ := sig.Body[1].(string)
		s.mu.Lock()
		h := s.actions[id]
		token := s.tokens[id]
		switch sig.Name {
		case busName + ".ActivationToken":
			s.tokens[id] = arg
		case busName + ".ActionInvoked":
			delete(s.tokens, id)
		case busName + ".NotificationClosed":
			delete(s.actions, id)
			delete(s.tokens, id)
		}
		s.mu.Unlock()
		if sig.Name == busName+".ActionInvoked" && h != nil {
			h(arg, token)
		}
	}
}

// SetFileManager sets the command "Show in folder" runs, or "" to ask the
// desktop's file manager service.
func (s *Sink) SetFileManager(cmd string) {
	s.fmMu.Lock()
	defer s.fmMu.Unlock()
	s.fileManager = strings.TrimSpace(cmd)
}

// showInFolder opens the folder holding path, with the file selected where
// the file manager can do that. token is the click's activation token.
//
// With a token, the desktop's file manager service shows the file. Without
// one (not every notification daemon sends it), a window that service
// opens or reuses can stay in the background, so a new file manager window
// is started instead: Wayland compositors focus new windows.
func (s *Sink) showInFolder(path, token string) {
	s.fmMu.Lock()
	fm := s.fileManager
	s.fmMu.Unlock()
	if fm == "" && token != "" {
		uri := (&url.URL{Scheme: "file", Path: path}).String()
		err := s.conn.Object("org.freedesktop.FileManager1", "/org/freedesktop/FileManager1").
			Call("org.freedesktop.FileManager1.ShowItems", 0, []string{uri}, token).Err
		if err == nil {
			return
		}
		slog.Info("file manager service failed", "err", err)
	}
	if fm == "" {
		fm = s.detectFileManager()
	}
	if fm == "" {
		xdgOpen(filepath.Dir(path), token)
		return
	}
	args := showArgs(strings.Fields(fm), path)
	start(token, args[0], args[1:]...)
}

// showArgs is the command line that opens a new window of file manager fm
// showing path.
func showArgs(fm []string, path string) []string {
	switch filepath.Base(fm[0]) {
	case "dolphin", "nautilus":
		return append(fm, "--new-window", "--select", path)
	default:
		return append(fm, filepath.Dir(path))
	}
}

var knownFileManagers = []string{"dolphin", "nautilus", "thunar", "nemo", "pcmanfm-qt", "pcmanfm", "caja"}

// detectFileManager picks the file manager that runs the desktop's file
// manager service, else the first one installed.
func (s *Sink) detectFileManager() string {
	var pid uint32
	err := s.conn.BusObject().Call("org.freedesktop.DBus.GetConnectionUnixProcessID", 0, "org.freedesktop.FileManager1").Store(&pid)
	if err == nil {
		if exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid)); err == nil {
			name := strings.ToLower(filepath.Base(exe))
			for _, k := range knownFileManagers {
				if name == k {
					return exe
				}
			}
		}
	}
	for _, k := range knownFileManagers {
		if p, err := exec.LookPath(k); err == nil {
			return p
		}
	}
	return ""
}

// start runs a program for a notification click, handing it the click's
// activation token the usual ways, so it may raise its window.
func start(token, name string, args ...string) {
	cmd := exec.Command(name, args...)
	if token != "" {
		cmd.Env = append(os.Environ(), "XDG_ACTIVATION_TOKEN="+token, "DESKTOP_STARTUP_ID="+token)
	}
	if err := cmd.Start(); err != nil {
		slog.Warn("starting "+name, "err", err)
		return
	}
	go cmd.Wait()
}

func xdgOpen(path, token string) {
	start(token, "xdg-open", path)
}
