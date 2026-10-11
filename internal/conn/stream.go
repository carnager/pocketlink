package conn

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/carnager/pocketlink/internal/proto"
)

// Streams move a live request's bulk data over its own HTTPS connection,
// the way file transfers do: one continuous body instead of many frames on
// the shared WebSocket, so throughput doesn't wait on its round trips.

// streamStall fails a stream that moves no data for this long.
const streamStall = 30 * time.Second

var errStreamEnded = errors.New("stream ended")

// stream is a request waiting for, or using, its HTTPS connection.
type stream struct {
	dev  string
	in   chan *streamBody // StreamIn: the device's PUT
	out  *io.PipeReader   // StreamOut: what the device's GET receives
	gone chan struct{}    // closed when the caller gave up
}

func (h *Hub) addStream(req string, st *stream) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.streams[req] = st
}

func (h *Hub) removeStream(req string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.streams, req)
}

// StreamIn sends a request that the device answers by PUTting a stream to
// proto.StreamPath. ctx bounds the wait for the stream to start. The body
// must be closed; closing it early ends the stream.
func (h *Hub) StreamIn(ctx context.Context, id, typ string, body any) (io.ReadCloser, error) {
	s := h.session(id)
	if s == nil {
		return nil, ErrOffline
	}
	e, err := proto.NewRequest(typ, body)
	if err != nil {
		return nil, err
	}
	st := &stream{dev: id, in: make(chan *streamBody, 1), gone: make(chan struct{})}
	h.addStream(e.Req, st)
	defer h.removeStream(e.Req)

	// The reply follows the stream, or comes instead of it on an error.
	rctx, stopWaiting := context.WithCancel(context.WithoutCancel(ctx))
	replied := make(chan error, 1)
	go func() {
		_, err := s.request(rctx, e, nil)
		replied <- err
	}()
	select {
	case b := <-st.in:
		b.onClose = stopWaiting
		return b, nil
	case err := <-replied:
		stopWaiting()
		if err == nil {
			err = errStreamEnded
		}
		return nil, err
	case <-ctx.Done():
		close(st.gone)
		stopWaiting()
		return nil, ctx.Err()
	}
}

// Upload is the sending end of a StreamOut.
type Upload interface {
	io.Writer
	// Finish ends the stream and waits for the device's reply.
	Finish(ctx context.Context) (Reply, error)
	// Abort ends the stream as failed.
	Abort(err error)
}

// StreamOut sends a request that the device answers by GETting a stream
// from proto.StreamPath. Write the data to the Upload, then Finish it.
func (h *Hub) StreamOut(ctx context.Context, id, typ string, body any) (Upload, error) {
	s := h.session(id)
	if s == nil {
		return nil, ErrOffline
	}
	e, err := proto.NewRequest(typ, body)
	if err != nil {
		return nil, err
	}
	pr, pw := io.Pipe()
	h.addStream(e.Req, &stream{dev: id, out: pr, gone: make(chan struct{})})
	u := &upload{pw: pw, done: make(chan struct{})}
	go func() {
		u.reply, u.err = s.request(context.WithoutCancel(ctx), e, nil)
		h.removeStream(e.Req)
		// Writes after the reply fail with its error rather than block.
		pr.CloseWithError(cmpErr(u.err, errStreamEnded))
		close(u.done)
	}()
	return u, nil
}

func cmpErr(err, fallback error) error {
	if err != nil {
		return err
	}
	return fallback
}

type upload struct {
	pw    *io.PipeWriter
	done  chan struct{}
	reply Reply
	err   error
}

func (u *upload) Write(p []byte) (int, error) { return u.pw.Write(p) }
func (u *upload) Abort(err error)             { u.pw.CloseWithError(err) }

func (u *upload) Finish(ctx context.Context) (Reply, error) {
	u.pw.Close()
	select {
	case <-u.done:
		return u.reply, u.err
	case <-ctx.Done():
		return Reply{}, ctx.Err()
	}
}

// streamBody is the body of a device's PUT, handed to StreamIn's caller.
type streamBody struct {
	r       io.Reader
	rc      *http.ResponseController
	done    chan struct{}
	once    sync.Once
	onClose func()
}

func (b *streamBody) Read(p []byte) (int, error) {
	b.rc.SetReadDeadline(time.Now().Add(streamStall))
	return b.r.Read(p)
}

func (b *streamBody) Close() error {
	b.once.Do(func() {
		close(b.done)
		if b.onClose != nil {
			b.onClose()
		}
	})
	return nil
}

// ServeStream handles a device's PUT or GET of proto.StreamPath + "{req}".
func (h *Hub) ServeStream(w http.ResponseWriter, r *http.Request) {
	dev, ok := h.store.Peer(r.TLS)
	if !ok {
		http.Error(w, "not paired", http.StatusForbidden)
		return
	}
	req := r.PathValue("req")
	h.mu.Lock()
	st := h.streams[req]
	if st != nil && st.dev == dev.ID {
		delete(h.streams, req) // each stream is used once
	} else {
		st = nil
	}
	h.mu.Unlock()
	if st == nil {
		http.Error(w, "no such stream", http.StatusNotFound)
		return
	}

	switch {
	case r.Method == http.MethodPut && st.in != nil:
		b := &streamBody{r: r.Body, rc: http.NewResponseController(w), done: make(chan struct{})}
		st.in <- b
		select {
		case <-b.done:
		case <-st.gone:
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusNoContent)

	case r.Method == http.MethodGet && st.out != nil:
		rc := http.NewResponseController(w)
		w.Header().Set("Content-Type", "application/octet-stream")
		buf := make([]byte, 256<<10)
		for {
			n, err := st.out.Read(buf)
			if n > 0 {
				rc.SetWriteDeadline(time.Now().Add(streamStall))
				if _, werr := w.Write(buf[:n]); werr != nil {
					st.out.CloseWithError(werr)
					return
				}
				rc.Flush()
			}
			if errors.Is(err, io.EOF) {
				return
			}
			if err != nil {
				// Cut the connection, so the device can't mistake what it
				// got so far for the whole stream.
				panic(http.ErrAbortHandler)
			}
		}

	default:
		http.Error(w, "wrong method for this stream", http.StatusMethodNotAllowed)
	}
}
