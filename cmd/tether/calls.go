package main

import (
	"fmt"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/carnager/tether/internal/conn"
	"github.com/carnager/tether/internal/media"
	"github.com/carnager/tether/internal/notify"
	"github.com/carnager/tether/internal/pair"
	"github.com/carnager/tether/internal/proto"
)

// Events older than this were queued while disconnected; acting on them
// (pausing music for a call that's long over) would only confuse.
const callEventMaxAge = 2 * time.Minute

// calls reacts to phone calls: a desktop notification, and pausing media
// or lowering the volume (separately configurable for ringing and talking)
// until the call ends.
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
	quiet    quieting
}

// quieting is what was done to desktop audio, so it can be undone.
type quieting struct {
	action string   // "pause", "lower" or "" for nothing
	paused []string // players to resume
	volume string   // volume to restore, as wpctl prints it
}

func newCalls(settings *settingsState, players *media.Watcher, sink *notify.Sink, hub *conn.Hub) *calls {
	return &calls{settings: settings, players: players, sink: sink, hub: hub, active: map[string]*call{}}
}

func (c *calls) handle(dev pair.Device, ev proto.CallState) error {
	slog.Info("call", "device", dev.Name, "event", ev.Event, "caller", callerName(ev))
	c.mu.Lock()
	defer c.mu.Unlock()
	cur := c.active[dev.ID]

	if ev.Event == "ended" {
		if cur == nil {
			return nil
		}
		delete(c.active, dev.ID)
		c.restore(cur.quiet)
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
	}
	cur.caller = callerName(ev)
	cfg := c.settings.get().Config
	switch ev.Event {
	case "ringing":
		c.switchTo(cur, cfg.RingAction, cfg.CallVolume)
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
		c.switchTo(cur, cfg.TalkAction, cfg.CallVolume)
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

// switchTo changes what is done to desktop audio, e.g. from a lowered
// volume while ringing to paused media once answered. The new action is
// applied before the old one is undone, so audio never comes back at full
// volume in between.
func (c *calls) switchTo(cur *call, action string, volume int) {
	if action == "none" {
		action = ""
	}
	if action == cur.quiet.action {
		return
	}
	old := cur.quiet
	cur.quiet = quieting{action: action}
	switch action {
	case "pause":
		cur.quiet.paused = c.players.PauseAll()
		slog.Info("call: paused media", "players", cur.quiet.paused)
	case "lower":
		cur.quiet.volume = lower(volume)
		slog.Info("call: lowered volume", "to", volume, "restore", cur.quiet.volume)
	}
	c.restore(old)
}

// lower turns the output volume down to percent of what it is now, and
// returns the volume to restore later ("" if nothing changed). Volumes are
// on wpctl's cubic scale, so 50 is clearly quieter but still audible.
func lower(percent int) string {
	cur, err := sinkVolume()
	if err != nil {
		slog.Warn("reading volume", "err", err)
		return ""
	}
	v, err := strconv.ParseFloat(cur, 64)
	if err != nil || percent >= 100 {
		return ""
	}
	target := v * float64(percent) / 100
	if err := setSinkVolume(strconv.FormatFloat(target, 'f', 3, 64)); err != nil {
		slog.Warn("lowering volume", "err", err)
		return ""
	}
	return cur
}

func (c *calls) restore(q quieting) {
	if len(q.paused) > 0 || q.volume != "" {
		slog.Info("call: restoring", "resume", q.paused, "volume", q.volume)
	}
	c.players.Resume(q.paused)
	if q.volume != "" {
		if err := setSinkVolume(q.volume); err != nil {
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
