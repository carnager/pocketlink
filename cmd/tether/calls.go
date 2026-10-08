package main

import (
	"fmt"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"tether/internal/conn"
	"tether/internal/media"
	"tether/internal/notify"
	"tether/internal/pair"
	"tether/internal/proto"
)

// Events older than this were queued while disconnected; acting on them
// (pausing music for a call that's long over) would only confuse.
const callEventMaxAge = 2 * time.Minute

// calls reacts to phone calls: a desktop notification, and pausing media
// or lowering the volume until the call ends.
type calls struct {
	settings *settingsState
	players  *media.Watcher
	sink     *notify.Sink
	hub      *conn.Hub

	mu     sync.Mutex
	active map[string]*call // by device
}

type call struct {
	caller   string
	notif    uint32
	answered bool
	paused   []string // players to resume
	volume   string   // volume to restore, as wpctl prints it
}

func newCalls(settings *settingsState, players *media.Watcher, sink *notify.Sink, hub *conn.Hub) *calls {
	return &calls{settings: settings, players: players, sink: sink, hub: hub, active: map[string]*call{}}
}

func (c *calls) handle(dev pair.Device, ev proto.CallState) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	cur := c.active[dev.ID]

	if ev.Event == "ended" {
		if cur == nil {
			return nil
		}
		delete(c.active, dev.ID)
		c.restore(cur)
		if cur.notif != 0 {
			if cur.answered {
				return c.sink.Close(cur.notif)
			}
			return c.sink.MissedCall(dev, cur.notif, cur.caller)
		}
		return nil
	}
	if age := time.Since(time.UnixMilli(ev.Time)); age > callEventMaxAge {
		slog.Info("ignoring stale call event", "event", ev.Event, "age", age.Round(time.Second))
		return nil
	}

	if cur == nil {
		cur = &call{}
		c.active[dev.ID] = cur
		c.quieten(cur)
	}
	cur.caller = callerName(ev)
	switch ev.Event {
	case "ringing":
		id, err := c.sink.IncomingCall(dev, cur.notif, cur.caller, func() {
			if err := c.hub.Send(dev.ID, proto.TypeCallMute, struct{}{}); err != nil {
				slog.Warn("muting ringer", "err", err)
			}
		})
		if err != nil {
			return err
		}
		cur.notif = id
	case "talking":
		cur.answered = true
		if cur.notif != 0 {
			c.sink.Close(cur.notif)
			cur.notif = 0
		}
	}
	return nil
}

func callerName(ev proto.CallState) string {
	switch {
	case ev.Name != "" && ev.Number != "":
		return ev.Name + " (" + ev.Number + ")"
	case ev.Name != "":
		return ev.Name
	case ev.Number != "":
		return ev.Number
	}
	return "Unknown caller"
}

// quieten applies the call_action setting.
func (c *calls) quieten(cur *call) {
	cfg := c.settings.get().Config
	switch cfg.CallAction {
	case "pause":
		cur.paused = c.players.PauseAll()
	case "lower":
		vol, err := sinkVolume()
		if err != nil {
			slog.Warn("reading volume", "err", err)
			return
		}
		target := float64(cfg.CallVolume) / 100
		if v, err := strconv.ParseFloat(vol, 64); err == nil && v > target {
			if err := setSinkVolume(strconv.FormatFloat(target, 'f', 2, 64)); err != nil {
				slog.Warn("lowering volume", "err", err)
				return
			}
			cur.volume = vol
		}
	}
}

func (c *calls) restore(cur *call) {
	c.players.Resume(cur.paused)
	if cur.volume != "" {
		if err := setSinkVolume(cur.volume); err != nil {
			slog.Warn("restoring volume", "err", err)
		}
	}
}

// sinkVolume returns the default output's volume, e.g. "0.95".
func sinkVolume() (string, error) {
	out, err := exec.Command("wpctl", "get-volume", "@DEFAULT_AUDIO_SINK@").Output()
	if err != nil {
		return "", fmt.Errorf("wpctl: %w", err)
	}
	// "Volume: 0.95" or "Volume: 0.95 [MUTED]"
	fields := strings.Fields(string(out))
	if len(fields) < 2 {
		return "", fmt.Errorf("unexpected wpctl output %q", out)
	}
	return fields[1], nil
}

func setSinkVolume(v string) error {
	return exec.Command("wpctl", "set-volume", "@DEFAULT_AUDIO_SINK@", v).Run()
}
