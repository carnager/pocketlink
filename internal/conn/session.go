package conn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/carnager/pocketlink/internal/pair"
	"github.com/carnager/pocketlink/internal/proto"
)

const (
	pingInterval = 60 * time.Second
	pingTimeout  = 15 * time.Second
	writeTimeout = 30 * time.Second
	probeTimeout = 5 * time.Second
)

// session is one live connection to a device.
type session struct {
	dev    pair.Device
	c      *websocket.Conn
	q      *Queue
	addr   string
	cancel context.CancelFunc
	done   chan struct{} // closed when the connection is gone

	mu       sync.Mutex
	features []string
	calls    map[string]chan reply // outstanding requests by Req
	probing  atomic.Bool
}

type reply struct {
	e       proto.Envelope
	payload []byte
}

func (s *session) run(ctx context.Context, h *Hub) error {
	defer close(s.done)
	defer s.c.CloseNow()

	hello, err := proto.NewUnacked(proto.TypeHello, proto.Hello{Name: h.name, Platform: "linux"})
	if err != nil {
		return err
	}
	if err := s.write(ctx, hello); err != nil {
		return err
	}

	errc := make(chan error, 3)
	go func() { errc <- s.readLoop(ctx, h) }()
	go func() { errc <- s.sendLoop(ctx) }()
	go func() { errc <- s.pingLoop(ctx) }()
	return <-errc
}

// sendLoop writes every queued frame not yet sent on this connection.
// Frames leave the queue only when acked, so a fresh session resends
// whatever the previous one didn't get confirmed.
func (s *session) sendLoop(ctx context.Context) error {
	sent := map[string]bool{}
	for {
		pending := s.q.Pending()
		still := make(map[string]bool, len(pending))
		for _, e := range pending {
			if !sent[e.ID] {
				if err := s.write(ctx, e); err != nil {
					return err
				}
			}
			still[e.ID] = true
		}
		sent = still

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.q.Wake():
		}
	}
}

func (s *session) readLoop(ctx context.Context, h *Hub) error {
	for {
		typ, data, err := s.c.Read(ctx)
		if err != nil {
			return err
		}
		var e proto.Envelope
		var payload []byte
		if typ == websocket.MessageBinary {
			e, payload, err = proto.DecodeBinary(data)
		} else {
			err = json.Unmarshal(data, &e)
		}
		if err != nil {
			slog.Warn("malformed frame", "device", s.dev.Name, "err", err)
			continue
		}

		switch {
		case e.Type == proto.TypeAck:
			if err := s.q.Ack(e.Ref); err != nil {
				slog.Warn("saving queue", "err", err)
			}
			continue
		case e.Type == proto.TypeReply:
			s.mu.Lock()
			ch := s.calls[e.Ref]
			delete(s.calls, e.Ref)
			s.mu.Unlock()
			if ch != nil {
				ch <- reply{e, payload}
			}
			continue
		case e.Req != "":
			// Nothing on the desktop answers requests yet.
			if err := s.write(ctx, proto.ErrorReply(e.Req, proto.ErrUnsupported, e.Type)); err != nil {
				return err
			}
			continue
		case e.Type == proto.TypeHello:
			hello, err := proto.Decode[proto.Hello](e)
			if err != nil {
				continue
			}
			s.mu.Lock()
			s.features = hello.Features
			s.mu.Unlock()
			if err := h.store.SetFeatures(s.dev.ID, hello.Features); err != nil {
				slog.Warn("saving device features", "err", err)
			}
			if hello.Name != "" && hello.Name != s.dev.Name {
				if err := h.store.Rename(s.dev.ID, hello.Name); err != nil {
					slog.Warn("renaming device", "err", err)
				}
				s.dev.Name = hello.Name
			}
			continue
		}

		if e.ID == "" || h.firstSeen(s.dev.ID, e.ID) {
			if err := h.handle(s.dev, e); err != nil {
				slog.Warn("handling frame", "device", s.dev.Name, "type", e.Type, "err", err)
			}
		}
		if e.ID != "" {
			if err := s.write(ctx, proto.Ack(e.ID)); err != nil {
				return err
			}
		}
	}
}

func (s *session) pingLoop(ctx context.Context) error {
	t := time.NewTicker(pingInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
		pctx, cancel := context.WithTimeout(ctx, pingTimeout)
		err := s.c.Ping(pctx)
		cancel()
		if err != nil {
			return fmt.Errorf("ping: %w", err)
		}
	}
}

func (s *session) write(ctx context.Context, e proto.Envelope) error {
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return s.c.Write(ctx, websocket.MessageText, data)
}

func (s *session) writeBinary(ctx context.Context, e proto.Envelope, payload []byte) error {
	data, err := proto.EncodeBinary(e, payload)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return s.c.Write(ctx, websocket.MessageBinary, data)
}

func (s *session) supports(feature string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Contains(s.features, feature)
}

// call sends a live request and waits for its reply.
func (s *session) call(ctx context.Context, typ string, body any, payload []byte) (Reply, error) {
	e, err := proto.NewRequest(typ, body)
	if err != nil {
		return Reply{}, err
	}
	return s.request(ctx, e, payload)
}

// request sends a request frame built by the caller and waits for its reply.
func (s *session) request(ctx context.Context, e proto.Envelope, payload []byte) (Reply, error) {
	var err error
	ch := make(chan reply, 1)
	s.mu.Lock()
	s.calls[e.Req] = ch
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.calls, e.Req)
		s.mu.Unlock()
	}()

	if payload != nil {
		err = s.writeBinary(ctx, e, payload)
	} else {
		err = s.write(ctx, e)
	}
	if err != nil {
		select {
		case <-s.done:
			return Reply{}, ErrOffline
		default:
			return Reply{}, err
		}
	}

	select {
	case r := <-ch:
		if r.e.Error != nil {
			return Reply{}, r.e.Error
		}
		return Reply{Body: r.e.Body, Payload: r.payload}, nil
	case <-s.done:
		return Reply{}, ErrOffline
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			s.probe()
		}
		return Reply{}, ctx.Err()
	}
}

// probe checks a connection that just let a request time out. A dead
// connection may otherwise linger until the next regular ping, failing
// every request only after its full timeout; once it's dropped, requests
// fail immediately.
func (s *session) probe() {
	if !s.probing.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer s.probing.Store(false)
		ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
		defer cancel()
		if err := s.c.Ping(ctx); err != nil {
			slog.Info("dropping unresponsive connection", "device", s.dev.Name, "err", err)
			s.cancel()
		}
	}()
}
