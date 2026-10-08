package conn

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/coder/websocket"

	"tether/internal/pair"
	"tether/internal/proto"
)

const (
	pingInterval = 60 * time.Second
	pingTimeout  = 15 * time.Second
	writeTimeout = 30 * time.Second
)

// session is one live connection to a device.
type session struct {
	dev    pair.Device
	c      *websocket.Conn
	q      *Queue
	addr   string
	cancel context.CancelFunc
}

func (s *session) run(ctx context.Context, h *Hub) error {
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
		_, data, err := s.c.Read(ctx)
		if err != nil {
			return err
		}
		var e proto.Envelope
		if err := json.Unmarshal(data, &e); err != nil {
			slog.Warn("malformed frame", "device", s.dev.Name, "err", err)
			continue
		}

		switch e.Type {
		case proto.TypeAck:
			if err := s.q.Ack(e.Ref); err != nil {
				slog.Warn("saving queue", "err", err)
			}
			continue
		case proto.TypeHello:
			if hello, err := proto.Decode[proto.Hello](e); err == nil && hello.Name != "" && hello.Name != s.dev.Name {
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
