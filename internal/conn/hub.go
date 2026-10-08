// Package conn runs the device-facing side of the daemon: it accepts TLS
// WebSocket connections from paired phones, delivers queued frames with
// acknowledgements, and hands incoming frames to a Handler.
//
// The phone always dials the desktop. A connection can drop at any time;
// nothing depends on it staying up, because unacknowledged frames are
// resent on the next connection.
package conn

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/carnager/tether/internal/pair"
	"github.com/carnager/tether/internal/proto"
)

// Handler processes one incoming frame. The frame is acknowledged after
// Handler returns, even if it returns an error, so a frame that can never
// be handled doesn't get redelivered forever.
type Handler func(dev pair.Device, e proto.Envelope) error

type Hub struct {
	// OnConnect, if set, runs when a device connects, before queued frames
	// are delivered; frames it sends go out on the new connection.
	OnConnect func(pair.Device)

	ctx      context.Context
	name     string
	store    *pair.Store
	pairing  *pair.Pairing
	queueDir string
	handle   Handler

	mu       sync.Mutex
	queues   map[string]*Queue
	sessions map[string]*session
	seen     map[string]*seenSet
}

func NewHub(ctx context.Context, name string, store *pair.Store, pairing *pair.Pairing, queueDir string, h Handler) *Hub {
	return &Hub{
		ctx:      ctx,
		name:     name,
		store:    store,
		pairing:  pairing,
		queueDir: queueDir,
		handle:   h,
		queues:   map[string]*Queue{},
		sessions: map[string]*session{},
		seen:     map[string]*seenSet{},
	}
}

func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		http.Error(w, "client certificate required", http.StatusUnauthorized)
		return
	}
	id := pair.Fingerprint(r.TLS.PeerCertificates[0].Raw)

	dev, ok := h.store.Peer(r.TLS)
	if !ok {
		if !h.pairing.Consume(r.URL.Query().Get("pair")) {
			slog.Warn("rejected unpaired device", "id", pair.ShortID(id), "addr", r.RemoteAddr)
			http.Error(w, "not paired", http.StatusForbidden)
			return
		}
		name := r.URL.Query().Get("name")
		if name == "" {
			name = "phone"
		}
		var err error
		if dev, err = h.store.Add(id, name); err != nil {
			slog.Error("saving paired device", "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		slog.Info("paired new device", "id", pair.ShortID(id), "name", name)
	}

	q, err := h.queue(id)
	if err != nil {
		slog.Error("opening queue", "id", pair.ShortID(id), "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	c.SetReadLimit(1 << 20)

	ctx, cancel := context.WithCancel(h.ctx)
	defer cancel()
	s := &session{dev: dev, c: c, q: q, addr: r.RemoteAddr, cancel: cancel}
	h.attach(s)
	if h.OnConnect != nil {
		h.OnConnect(dev)
	}
	slog.Info("device connected", "name", dev.Name, "addr", r.RemoteAddr, "pending", q.Len())
	err = s.run(ctx, h)
	h.detach(s)
	slog.Info("device disconnected", "name", dev.Name, "err", err)
}

// Send queues a frame for one device. It is delivered as soon as the device
// is connected, or on its next connection.
func (h *Hub) Send(id, typ string, body any) error {
	if _, ok := h.store.Get(id); !ok {
		return fmt.Errorf("unknown device %s", pair.ShortID(id))
	}
	e, err := proto.New(typ, body)
	if err != nil {
		return err
	}
	q, err := h.queue(id)
	if err != nil {
		return err
	}
	return q.Push(e)
}

// Broadcast queues a frame for every paired device.
func (h *Hub) Broadcast(typ string, body any) error {
	var errs []error
	for _, d := range h.store.List() {
		errs = append(errs, h.Send(d.ID, typ, body))
	}
	return errors.Join(errs...)
}

// Forget unpairs a device, drops its connection and deletes its queue.
func (h *Hub) Forget(id string) error {
	if err := h.store.Remove(id); err != nil {
		return err
	}
	h.mu.Lock()
	if s := h.sessions[id]; s != nil {
		s.cancel()
	}
	q := h.queues[id]
	delete(h.queues, id)
	delete(h.seen, id)
	h.mu.Unlock()

	if q != nil {
		return q.Remove()
	}
	err := os.Remove(h.queuePath(id))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// Resolve finds a paired device by ID prefix or exact name.
func (h *Hub) Resolve(ref string) (string, error) {
	var matches []string
	for _, d := range h.store.List() {
		if d.ID == ref || d.Name == ref {
			return d.ID, nil
		}
		if strings.HasPrefix(d.ID, ref) {
			matches = append(matches, d.ID)
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no device matches %q", ref)
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("%q is ambiguous", ref)
	}
}

type DeviceStatus struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Connected bool      `json:"connected"`
	Addr      string    `json:"addr,omitempty"`
	Pending   int       `json:"pending"`
	LastSeen  time.Time `json:"last_seen,omitzero"`
}

func (h *Hub) Status() []DeviceStatus {
	var out []DeviceStatus
	for _, d := range h.store.List() {
		st := DeviceStatus{ID: d.ID, Name: d.Name, LastSeen: d.LastSeen}
		h.mu.Lock()
		if s := h.sessions[d.ID]; s != nil {
			st.Connected = true
			st.Addr = s.addr
		}
		h.mu.Unlock()
		if q, err := h.queue(d.ID); err == nil {
			st.Pending = q.Len()
		}
		out = append(out, st)
	}
	return out
}

func (h *Hub) queue(id string) (*Queue, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if q := h.queues[id]; q != nil {
		return q, nil
	}
	q, err := OpenQueue(h.queuePath(id))
	if err != nil {
		return nil, err
	}
	h.queues[id] = q
	return q, nil
}

func (h *Hub) queuePath(id string) string {
	return filepath.Join(h.queueDir, id+".json")
}

// attach registers s as the device's active session. A phone that
// reconnects before its old connection timed out replaces it.
func (h *Hub) attach(s *session) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if old := h.sessions[s.dev.ID]; old != nil {
		old.cancel()
	}
	h.sessions[s.dev.ID] = s
}

func (h *Hub) detach(s *session) {
	h.mu.Lock()
	if h.sessions[s.dev.ID] == s {
		delete(h.sessions, s.dev.ID)
	}
	h.mu.Unlock()
	if err := h.store.Touch(s.dev.ID); err != nil {
		slog.Warn("updating last seen", "err", err)
	}
}

// firstSeen records id as handled for dev and reports whether it is new.
func (h *Hub) firstSeen(devID, id string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.seen[devID]
	if s == nil {
		s = newSeenSet(512)
		h.seen[devID] = s
	}
	return s.add(id)
}

// seenSet remembers recently handled frame IDs so retransmitted frames are
// acknowledged again but not handled twice.
type seenSet struct {
	ids  map[string]struct{}
	ring []string
	next int
}

func newSeenSet(n int) *seenSet {
	return &seenSet{ids: make(map[string]struct{}, n), ring: make([]string, n)}
}

func (s *seenSet) add(id string) bool {
	if _, ok := s.ids[id]; ok {
		return false
	}
	if old := s.ring[s.next]; old != "" {
		delete(s.ids, old)
	}
	s.ring[s.next] = id
	s.ids[id] = struct{}{}
	s.next = (s.next + 1) % len(s.ring)
	return true
}
