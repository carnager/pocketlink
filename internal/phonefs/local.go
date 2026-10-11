package phonefs

import (
	"context"
	"encoding/json"
	"errors"
	"io"
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
	l.count(typ)

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

// StreamIn runs the phone's side of a stream request in-process.
func (l *Local) StreamIn(ctx context.Context, dev, typ string, body any) (io.ReadCloser, error) {
	e, err := l.start(typ, body)
	if err != nil {
		return nil, err
	}
	pr, pw := io.Pipe()
	started := make(chan struct{})
	replied := make(chan proto.Envelope, 1)
	go func() {
		replied <- l.Server.ServeStream(e, func(r io.Reader, _ int64) error {
			close(started)
			_, err := io.Copy(pw, r)
			pw.CloseWithError(err)
			return err
		}, nil)
	}()
	select {
	case <-started:
		return pr, nil
	case r := <-replied:
		if r.Error != nil {
			return nil, r.Error
		}
		return nil, errors.New("stream ended before it started")
	}
}

// StreamOut runs the phone's side of a stream request in-process.
func (l *Local) StreamOut(ctx context.Context, dev, typ string, body any) (conn.Upload, error) {
	e, err := l.start(typ, body)
	if err != nil {
		return nil, err
	}
	pr, pw := io.Pipe()
	u := &localUpload{pw: pw, done: make(chan struct{})}
	go func() {
		u.reply = l.Server.ServeStream(e, nil, func() (io.ReadCloser, error) { return pr, nil })
		pr.CloseWithError(errors.New("stream ended"))
		close(u.done)
	}()
	return u, nil
}

func (l *Local) start(typ string, body any) (proto.Envelope, error) {
	if l.Offline {
		return proto.Envelope{}, conn.ErrOffline
	}
	l.count(typ)
	e, err := proto.NewRequest(typ, body)
	if err != nil {
		return e, err
	}
	data, _ := json.Marshal(e)
	var got proto.Envelope
	return got, json.Unmarshal(data, &got)
}

type localUpload struct {
	pw    *io.PipeWriter
	done  chan struct{}
	reply proto.Envelope
}

func (u *localUpload) Write(p []byte) (int, error) { return u.pw.Write(p) }
func (u *localUpload) Abort(err error)             { u.pw.CloseWithError(err) }

func (u *localUpload) Finish(ctx context.Context) (conn.Reply, error) {
	u.pw.Close()
	<-u.done
	if u.reply.Error != nil {
		return conn.Reply{}, u.reply.Error
	}
	return conn.Reply{Body: u.reply.Body}, nil
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

func (l *Local) count(typ string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.calls == nil {
		l.calls = map[string]int{}
	}
	l.calls[typ]++
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
