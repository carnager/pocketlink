package phonefs

import (
	"context"
	"encoding/json"
	"slices"
	"sync"

	"github.com/carnager/pocketlink/internal/conn"
	"github.com/carnager/pocketlink/internal/proto"
)

// Local is a Caller whose single device answers in-process from a Server,
// through the same encoding as the wire. It counts requests by type, for
// tests.
type Local struct {
	Server   *Server
	Features []string // default: FeatureFS
	Offline  bool

	mu    sync.Mutex
	calls map[string]int
}

func (l *Local) Call(ctx context.Context, dev, typ string, body any, payload []byte) (conn.Reply, error) {
	if l.Offline {
		return conn.Reply{}, conn.ErrOffline
	}
	l.mu.Lock()
	if l.calls == nil {
		l.calls = map[string]int{}
	}
	l.calls[typ]++
	l.mu.Unlock()

	e, err := proto.NewRequest(typ, body)
	if err != nil {
		return conn.Reply{}, err
	}
	// Round-trip through JSON, as the phone would see it.
	data, _ := json.Marshal(e)
	var got proto.Envelope
	if err := json.Unmarshal(data, &got); err != nil {
		return conn.Reply{}, err
	}
	r, out := l.Server.Handle(got, slices.Clone(payload))
	if r.Error != nil {
		return conn.Reply{}, r.Error
	}
	return conn.Reply{Body: r.Body, Payload: out}, nil
}

func (l *Local) Connected(string) bool { return !l.Offline }

func (l *Local) Supports(_, feature string) bool {
	if l.Offline {
		return false
	}
	if l.Features == nil {
		return feature == proto.FeatureFS
	}
	return slices.Contains(l.Features, feature)
}

// Calls returns how many requests of type typ were made, and resets the
// count.
func (l *Local) Calls(typ string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := l.calls[typ]
	delete(l.calls, typ)
	return n
}
