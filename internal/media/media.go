// Package media exposes the desktop's MPRIS media players to phones: it
// reports what's playing and carries out remote-control commands.
package media

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
)

const (
	busPrefix   = "org.mpris.MediaPlayer2."
	objPath     = "/org/mpris/MediaPlayer2"
	rootIface   = "org.mpris.MediaPlayer2"
	playerIface = "org.mpris.MediaPlayer2.Player"

	debounce = 250 * time.Millisecond
	// A hung player (seen with mpd's MPRIS bridge) would otherwise block for
	// the bus's 25 second default on every call.
	readTimeout = time.Second
	cmdTimeout  = 2 * time.Second
	maxArtSize  = 5 << 20
)

type Player struct {
	ID         string  `json:"id"` // D-Bus name, used to address commands
	Name       string  `json:"name"`
	Status     string  `json:"status"` // Playing, Paused or Stopped
	Title      string  `json:"title,omitempty"`
	Artist     string  `json:"artist,omitempty"`
	Album      string  `json:"album,omitempty"`
	LengthMs   int64   `json:"length_ms,omitempty"`
	PositionMs int64   `json:"position_ms,omitempty"`
	Volume     float64 `json:"volume"` // 0..1, or -1 if the player has none
	CanNext    bool    `json:"can_next"`
	CanPrev    bool    `json:"can_prev"`
	CanPlay    bool    `json:"can_play"`
	CanPause   bool    `json:"can_pause"`
	CanSeek    bool    `json:"can_seek"`
	Art        string  `json:"art,omitempty"` // key for GET /media/art/{key}

	trackID dbus.ObjectPath
	artURL  string
}

type State struct {
	Players []Player `json:"players"`
	Active  string   `json:"active,omitempty"` // the player phones should control
}

// Command is a remote-control request from a phone.
type Command struct {
	Player string  `json:"player"`
	Action string  `json:"action"`          // play_pause, play, pause, next, previous, seek, set_position, volume
	Value  float64 `json:"value,omitempty"` // ms for seek and set_position, 0..1 for volume
}

type Watcher struct {
	conn     *dbus.Conn
	onChange func(State)
	refresh  chan struct{}

	mu          sync.Mutex
	state       State
	lastJSON    string
	lastPlaying string
	art         map[string]string // key -> artUrl
}

func New(onChange func(State)) (*Watcher, error) {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return nil, err
	}
	return &Watcher{conn: conn, onChange: onChange, refresh: make(chan struct{}, 1), art: map[string]string{}}, nil
}

// Run follows player changes until ctx is done.
func (w *Watcher) Run(ctx context.Context) {
	matches := [][]dbus.MatchOption{
		{dbus.WithMatchObjectPath(objPath), dbus.WithMatchInterface("org.freedesktop.DBus.Properties"), dbus.WithMatchMember("PropertiesChanged")},
		{dbus.WithMatchObjectPath(objPath), dbus.WithMatchInterface(playerIface), dbus.WithMatchMember("Seeked")},
		{dbus.WithMatchSender("org.freedesktop.DBus"), dbus.WithMatchInterface("org.freedesktop.DBus"), dbus.WithMatchMember("NameOwnerChanged")},
	}
	for _, m := range matches {
		if err := w.conn.AddMatchSignal(m...); err != nil {
			slog.Warn("media: subscribing to player signals", "err", err)
		}
	}
	signals := make(chan *dbus.Signal, 64)
	w.conn.Signal(signals)
	defer w.conn.RemoveSignal(signals)

	w.update()
	var timer <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case sig := <-signals:
			if sig.Name == "org.freedesktop.DBus.NameOwnerChanged" {
				if len(sig.Body) == 0 || !strings.HasPrefix(fmt.Sprint(sig.Body[0]), busPrefix) {
					continue
				}
			}
			if timer == nil {
				timer = time.After(debounce)
			}
		case <-w.refresh:
			if timer == nil {
				timer = time.After(debounce)
			}
		case <-timer:
			timer = nil
			w.update()
		}
	}
}

// State returns the latest known player state.
func (w *Watcher) State() State {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.state
}

func (w *Watcher) update() {
	players := w.readPlayers()

	w.mu.Lock()
	active := w.pickActive(players)
	for i := range players {
		if players[i].artURL != "" {
			key := artKey(players[i].artURL)
			players[i].Art = key
			w.art[key] = players[i].artURL
		}
	}
	state := State{Players: players, Active: active}
	data, _ := json.Marshal(state)
	changed := string(data) != w.lastJSON
	w.state, w.lastJSON = state, string(data)
	w.mu.Unlock()

	if changed {
		w.onChange(state)
	}
}

// pickActive prefers a playing player, sticking with the one that was
// playing most recently.
func (w *Watcher) pickActive(players []Player) string {
	var playing []string
	for _, p := range players {
		if p.Status == "Playing" {
			playing = append(playing, p.ID)
		}
	}
	switch {
	case slices.Contains(playing, w.lastPlaying):
	case len(playing) > 0:
		w.lastPlaying = playing[0]
	case slices.ContainsFunc(players, func(p Player) bool { return p.ID == w.lastPlaying }):
	case len(players) > 0:
		w.lastPlaying = players[0].ID
	default:
		w.lastPlaying = ""
	}
	return w.lastPlaying
}

func (w *Watcher) readPlayers() []Player {
	var names []string
	ctx, cancel := context.WithTimeout(context.Background(), readTimeout)
	defer cancel()
	if err := w.conn.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.ListNames", 0).Store(&names); err != nil {
		slog.Warn("media: listing players", "err", err)
		return nil
	}
	slices.Sort(names)
	var players []Player
	for _, name := range names {
		// playerctld mirrors other players; showing it would duplicate them.
		if !strings.HasPrefix(name, busPrefix) || name == busPrefix+"playerctld" {
			continue
		}
		p, err := w.readPlayer(name)
		if err != nil {
			continue
		}
		// Idle players (e.g. a browser with nothing loaded) aren't worth a remote.
		if p.Status == "Stopped" && p.Title == "" {
			continue
		}
		players = append(players, p)
	}
	return players
}

func (w *Watcher) readPlayer(name string) (Player, error) {
	ctx, cancel := context.WithTimeout(context.Background(), readTimeout)
	defer cancel()
	obj := w.conn.Object(name, objPath)
	var props map[string]dbus.Variant
	if err := obj.CallWithContext(ctx, "org.freedesktop.DBus.Properties.GetAll", 0, playerIface).Store(&props); err != nil {
		return Player{}, err
	}
	p := Player{ID: name, Volume: -1}
	var identity dbus.Variant
	if obj.CallWithContext(ctx, "org.freedesktop.DBus.Properties.Get", 0, rootIface, "Identity").Store(&identity) == nil {
		p.Name, _ = identity.Value().(string)
	}
	if p.Name == "" {
		p.Name, _, _ = strings.Cut(strings.TrimPrefix(name, busPrefix), ".")
	}
	p.Status, _ = props["PlaybackStatus"].Value().(string)
	p.PositionMs = toInt64(props["Position"].Value()) / 1000
	if v, ok := props["Volume"].Value().(float64); ok {
		p.Volume = v
	}
	p.CanNext, _ = props["CanGoNext"].Value().(bool)
	p.CanPrev, _ = props["CanGoPrevious"].Value().(bool)
	p.CanPlay, _ = props["CanPlay"].Value().(bool)
	p.CanPause, _ = props["CanPause"].Value().(bool)
	p.CanSeek, _ = props["CanSeek"].Value().(bool)

	md, _ := props["Metadata"].Value().(map[string]dbus.Variant)
	p.Title, _ = md["xesam:title"].Value().(string)
	p.Album, _ = md["xesam:album"].Value().(string)
	switch a := md["xesam:artist"].Value().(type) {
	case []string:
		p.Artist = strings.Join(a, ", ")
	case string:
		p.Artist = a
	}
	p.LengthMs = toInt64(md["mpris:length"].Value()) / 1000
	p.artURL, _ = md["mpris:artUrl"].Value().(string)
	switch t := md["mpris:trackid"].Value().(type) {
	case dbus.ObjectPath:
		p.trackID = t
	case string:
		p.trackID = dbus.ObjectPath(t)
	}
	return p, nil
}

// Command carries out a phone's remote-control request.
func (w *Watcher) Command(c Command) error {
	if !strings.HasPrefix(c.Player, busPrefix) {
		return fmt.Errorf("media: not a player: %q", c.Player)
	}
	ctx, cancel := context.WithTimeout(context.Background(), cmdTimeout)
	defer cancel()
	obj := w.conn.Object(c.Player, objPath)
	call := func(method string, args ...any) error {
		return obj.CallWithContext(ctx, playerIface+"."+method, 0, args...).Err
	}
	var err error
	switch c.Action {
	case "play_pause":
		err = call("PlayPause")
	case "play":
		err = call("Play")
	case "pause":
		err = call("Pause")
	case "next":
		err = call("Next")
	case "previous":
		err = call("Previous")
	case "seek":
		err = call("Seek", int64(c.Value*1000))
	case "set_position":
		var track dbus.ObjectPath
		for _, p := range w.State().Players {
			if p.ID == c.Player {
				track = p.trackID
			}
		}
		if !track.IsValid() {
			return errors.New("media: player has no track id")
		}
		err = call("SetPosition", track, int64(c.Value*1000))
	case "volume":
		err = obj.CallWithContext(ctx, "org.freedesktop.DBus.Properties.Set", 0,
			playerIface, "Volume", dbus.MakeVariant(max(0, min(1, c.Value)))).Err
	default:
		return fmt.Errorf("media: unknown action %q", c.Action)
	}
	// Not every player signals promptly, so look again ourselves.
	select {
	case w.refresh <- struct{}{}:
	default:
	}
	return err
}

// PauseAll pauses every playing player and returns their IDs, for Resume.
func (w *Watcher) PauseAll() []string {
	var paused []string
	for _, p := range w.readPlayers() {
		if p.Status == "Playing" && w.callPlayer(p.ID, "Pause") == nil {
			paused = append(paused, p.ID)
		}
	}
	return paused
}

// Resume restarts the players PauseAll paused.
func (w *Watcher) Resume(ids []string) {
	for _, id := range ids {
		if err := w.callPlayer(id, "Play"); err != nil {
			slog.Warn("media: resuming", "player", id, "err", err)
		}
	}
}

func (w *Watcher) callPlayer(id, method string) error {
	ctx, cancel := context.WithTimeout(context.Background(), cmdTimeout)
	defer cancel()
	return w.conn.Object(id, objPath).CallWithContext(ctx, playerIface+"."+method, 0).Err
}

// Art returns the image behind an art key from the current state.
func (w *Watcher) Art(ctx context.Context, key string) ([]byte, error) {
	w.mu.Lock()
	src, ok := w.art[key]
	w.mu.Unlock()
	if !ok {
		return nil, os.ErrNotExist
	}
	u, err := url.Parse(src)
	if err != nil {
		return nil, err
	}
	switch u.Scheme {
	case "file":
		f, err := os.Open(u.Path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		return io.ReadAll(io.LimitReader(f, maxArtSize))
	case "http", "https":
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
		if err != nil {
			return nil, err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("fetching art: %s", resp.Status)
		}
		return io.ReadAll(io.LimitReader(resp.Body, maxArtSize))
	default:
		return nil, fmt.Errorf("unsupported art url %q", src)
	}
}

func artKey(u string) string {
	sum := sha256.Sum256([]byte(u))
	return hex.EncodeToString(sum[:8])
}

func toInt64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case uint64:
		return int64(n)
	case int32:
		return int64(n)
	case uint32:
		return int64(n)
	case float64:
		return int64(n)
	}
	return 0
}
