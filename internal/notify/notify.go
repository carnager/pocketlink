// Package notify mirrors phone notifications to the desktop notification
// daemon (mako, swaync, dunst, ...) via org.freedesktop.Notifications.
package notify

import (
	"log/slog"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/godbus/dbus/v5"

	"tether/internal/pair"
	"tether/internal/proto"
)

const (
	busName = "org.freedesktop.Notifications"
	objPath = "/org/freedesktop/Notifications"
)

var escapeMarkup = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

type Sink struct {
	obj dbus.BusObject

	mu      sync.Mutex
	ids     map[string]uint32              // device id + "/" + phone key -> desktop notification id
	actions map[uint32]func(action string) // desktop notification id -> action handler
}

func New() (*Sink, error) {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return nil, err
	}
	s := &Sink{
		obj:     conn.Object(busName, objPath),
		ids:     map[string]uint32{},
		actions: map[uint32]func(string){},
	}
	for _, member := range []string{"ActionInvoked", "NotificationClosed"} {
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

// FileReceived announces a saved file, with actions to open it or its folder.
func (s *Sink) FileReceived(dev pair.Device, path string) error {
	_, err := s.show("tether", 0, "document-save", "File from "+dev.Name, filepath.Base(path),
		[]string{"default", "Open", "folder", "Show in folder"},
		func(action string) {
			switch action {
			case "default":
				xdgOpen(path)
			case "folder":
				xdgOpen(filepath.Dir(path))
			}
		})
	return err
}

// IncomingCall shows (or updates) a ringing call with a Mute action.
func (s *Sink) IncomingCall(dev pair.Device, replaces uint32, caller string, onMute func()) (uint32, error) {
	return s.show(dev.Name, replaces, "call-start", "Incoming call", caller,
		[]string{"mute", "Mute ringer"},
		func(action string) {
			if action == "mute" {
				onMute()
			}
		})
}

// MissedCall replaces the ringing notification with a missed-call one.
func (s *Sink) MissedCall(dev pair.Device, replaces uint32, caller string) error {
	_, err := s.show(dev.Name, replaces, "call-missed", "Missed call", caller, nil, nil)
	return err
}

// Close removes a notification shown by this sink.
func (s *Sink) Close(id uint32) error {
	return s.obj.Call(busName+".CloseNotification", 0, id).Err
}

func (s *Sink) show(app string, replaces uint32, icon, title, body string, actions []string, onAction func(string)) (uint32, error) {
	if actions == nil {
		actions = []string{}
	}
	var id uint32
	err := s.obj.Call(busName+".Notify", 0,
		app, replaces, icon, title, escapeMarkup.Replace(body),
		actions, map[string]dbus.Variant{}, int32(-1),
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
		s.mu.Lock()
		h := s.actions[id]
		if sig.Name == busName+".NotificationClosed" {
			delete(s.actions, id)
		}
		s.mu.Unlock()
		if action, ok := sig.Body[1].(string); ok && h != nil && sig.Name == busName+".ActionInvoked" {
			h(action)
		}
	}
}

func xdgOpen(path string) {
	cmd := exec.Command("xdg-open", path)
	if err := cmd.Start(); err != nil {
		slog.Warn("xdg-open", "err", err)
		return
	}
	go cmd.Wait()
}
