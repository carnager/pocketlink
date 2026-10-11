// Package phonefs gives the desktop access to a phone's shared storage over
// the live request channel (see the fs.* types in package proto). It is the
// layer the WebDAV and FUSE front ends build on.
package phonefs

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"path"
	"sync"
	"time"

	"github.com/carnager/pocketlink/internal/conn"
	"github.com/carnager/pocketlink/internal/proto"
)

// DefaultTimeout bounds one request when the caller's context has no
// deadline. It's generous enough for a full payload on a slow mobile link.
const DefaultTimeout = 30 * time.Second

// parallel is how many chunk requests of one large read or write are in
// flight at once, so throughput isn't capped at one payload per round trip.
const parallel = 4

// Caller sends live requests to devices; *conn.Hub implements it.
type Caller interface {
	Call(ctx context.Context, dev, typ string, body any, payload []byte) (conn.Reply, error)
	StreamIn(ctx context.Context, dev, typ string, body any) (io.ReadCloser, error)
	StreamOut(ctx context.Context, dev, typ string, body any) (conn.Upload, error)
	Connected(dev string) bool
	Supports(dev, feature string) bool
}

// ErrNoFS means the phone is connected, but its app doesn't offer file
// access (it's too old).
var ErrNoFS = errors.New("the phone app doesn't offer file access; update it")

// Client accesses one phone's files. Errors are *proto.Error values (which
// match fs.ErrNotExist and friends with errors.Is), conn.ErrOffline, ErrNoFS
// or context errors.
type Client struct {
	c   Caller
	dev string
}

func New(c Caller, dev string) *Client {
	return &Client{c: c, dev: dev}
}

// Ready reports why requests would fail right now, if they would: the
// phone isn't connected (conn.ErrOffline), or its app is too old (ErrNoFS).
func (cl *Client) Ready() error {
	// An old app would ignore requests, and we'd wait out the timeout.
	if !cl.c.Supports(cl.dev, proto.FeatureFS) {
		if cl.c.Connected(cl.dev) {
			return ErrNoFS
		}
		return conn.ErrOffline
	}
	return nil
}

func (cl *Client) call(ctx context.Context, typ string, body any, payload []byte, out any) (conn.Reply, error) {
	if err := cl.Ready(); err != nil {
		return conn.Reply{}, err
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, DefaultTimeout)
		defer cancel()
	}
	r, err := cl.c.Call(ctx, cl.dev, typ, body, payload)
	if err != nil {
		return r, err
	}
	if out != nil {
		if err := json.Unmarshal(r.Body, out); err != nil {
			return r, &proto.Error{Code: proto.ErrIO, Message: "bad reply to " + typ + ": " + err.Error()}
		}
	}
	return r, nil
}

// Clean turns any path into the rooted form the phone expects. It can't
// climb above the root.
func Clean(p string) string {
	return path.Clean("/" + p)
}

func (cl *Client) Stat(ctx context.Context, p string) (proto.FSEntry, error) {
	var e proto.FSEntry
	_, err := cl.call(ctx, proto.TypeFSStat, proto.FSPath{Path: Clean(p)}, nil, &e)
	return e, err
}

func (cl *Client) List(ctx context.Context, p string) ([]proto.FSEntry, error) {
	var l proto.FSList
	_, err := cl.call(ctx, proto.TypeFSList, proto.FSPath{Path: Clean(p)}, nil, &l)
	return l.Entries, err
}

// ReadAt reads len(buf) bytes at off, like io.ReaderAt: if it reads fewer,
// the error says why, and is io.EOF at the end of the file.
func (cl *Client) ReadAt(ctx context.Context, p string, buf []byte, off int64) (int, error) {
	p = Clean(p)
	chunks := split(len(buf))
	got := make([]int, len(chunks))
	err := each(len(chunks), func(i int) error {
		c := chunks[i]
		r, err := cl.call(ctx, proto.TypeFSRead, proto.FSRead{Path: p, Offset: off + int64(c.off), Length: c.n}, nil, nil)
		if err != nil {
			return err
		}
		got[i] = copy(buf[c.off:c.off+c.n], r.Payload)
		return nil
	})
	// The data is good up to the first failed or short chunk.
	n := 0
	for i, c := range chunks {
		n += got[i]
		if got[i] < c.n {
			break
		}
	}
	switch {
	case n == len(buf):
		return n, nil
	case err != nil:
		return n, err
	default:
		return n, io.EOF
	}
}

// WriteAt writes data at off into an existing file and returns the file
// afterwards.
func (cl *Client) WriteAt(ctx context.Context, p string, data []byte, off int64) (proto.FSEntry, error) {
	p = Clean(p)
	if len(data) == 0 {
		return cl.Stat(ctx, p)
	}
	chunks := split(len(data))
	ents := make([]proto.FSEntry, len(chunks))
	err := each(len(chunks), func(i int) error {
		c := chunks[i]
		_, err := cl.call(ctx, proto.TypeFSWrite, proto.FSWrite{Path: p, Offset: off + int64(c.off)}, data[c.off:c.off+c.n], &ents[i])
		return err
	})
	if err != nil {
		return proto.FSEntry{}, err
	}
	// Chunks may finish in any order; sizes and times only grow while
	// they're written.
	e := ents[0]
	for _, o := range ents[1:] {
		e.Size = max(e.Size, o.Size)
		e.MTime = max(e.MTime, o.MTime)
	}
	return e, nil
}

func (cl *Client) Create(ctx context.Context, p string, exclusive, truncate bool) (proto.FSEntry, error) {
	var e proto.FSEntry
	_, err := cl.call(ctx, proto.TypeFSCreate, proto.FSCreate{Path: Clean(p), Exclusive: exclusive, Truncate: truncate}, nil, &e)
	return e, err
}

func (cl *Client) Mkdir(ctx context.Context, p string) (proto.FSEntry, error) {
	var e proto.FSEntry
	_, err := cl.call(ctx, proto.TypeFSMkdir, proto.FSPath{Path: Clean(p)}, nil, &e)
	return e, err
}

func (cl *Client) Remove(ctx context.Context, p string, recursive bool) error {
	_, err := cl.call(ctx, proto.TypeFSRemove, proto.FSRemove{Path: Clean(p), Recursive: recursive}, nil, nil)
	return err
}

func (cl *Client) Rename(ctx context.Context, from, to string, noReplace bool) error {
	_, err := cl.call(ctx, proto.TypeFSRename, proto.FSRename{From: Clean(from), To: Clean(to), NoReplace: noReplace}, nil, nil)
	return err
}

// SetAttr changes size and/or modification time; nil leaves one alone.
func (cl *Client) SetAttr(ctx context.Context, p string, size *int64, mtime *time.Time) (proto.FSEntry, error) {
	req := proto.FSSetAttr{Path: Clean(p), Size: size}
	if mtime != nil {
		ms := mtime.UnixMilli()
		req.MTime = &ms
	}
	var e proto.FSEntry
	_, err := cl.call(ctx, proto.TypeFSSetAttr, req, nil, &e)
	return e, err
}

func (cl *Client) StatFS(ctx context.Context, p string) (proto.FSSpace, error) {
	var s proto.FSSpace
	_, err := cl.call(ctx, proto.TypeFSStatFS, proto.FSPath{Path: Clean(p)}, nil, &s)
	return s, err
}

// Streams reports whether the phone can stream files (OpenRead and
// OpenWrite), which is much faster for sequential transfers than ReadAt and
// WriteAt.
func (cl *Client) Streams() bool {
	return cl.c.Supports(cl.dev, proto.FeatureFSStream)
}

// OpenRead streams a file from off to its end over its own connection.
// Close it when done; closing it early stops the stream.
func (cl *Client) OpenRead(ctx context.Context, p string, off int64) (io.ReadCloser, error) {
	if err := cl.Ready(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, DefaultTimeout) // to start
	defer cancel()
	return cl.c.StreamIn(ctx, cl.dev, proto.TypeFSReadStream, proto.FSStream{Path: Clean(p), Offset: off})
}

// OpenWrite streams data into an existing file from off on. Write to the
// Writer, then Close it.
func (cl *Client) OpenWrite(ctx context.Context, p string, off int64) (*Writer, error) {
	if err := cl.Ready(); err != nil {
		return nil, err
	}
	u, err := cl.c.StreamOut(ctx, cl.dev, proto.TypeFSWriteStream, proto.FSStream{Path: Clean(p), Offset: off})
	if err != nil {
		return nil, err
	}
	return &Writer{u: u}, nil
}

// Writer is a stream into a file on the phone.
type Writer struct {
	u      conn.Upload
	onDone func(proto.FSEntry, error)
}

func (w *Writer) Write(p []byte) (int, error) { return w.u.Write(p) }

// Abort ends the stream as failed; the file keeps what arrived.
func (w *Writer) Abort(err error) {
	w.u.Abort(err)
}

// Close ends the stream and returns the file afterwards.
func (w *Writer) Close(ctx context.Context) (proto.FSEntry, error) {
	ctx, cancel := context.WithTimeout(ctx, DefaultTimeout) // for the phone to finish
	defer cancel()
	var e proto.FSEntry
	r, err := w.u.Finish(ctx)
	if err == nil {
		if jerr := json.Unmarshal(r.Body, &e); jerr != nil {
			err = &proto.Error{Code: proto.ErrIO, Message: "bad reply to fs.writestream: " + jerr.Error()}
		}
	}
	if w.onDone != nil {
		w.onDone(e, err)
	}
	return e, err
}

type chunk struct{ off, n int }

func split(n int) []chunk {
	var out []chunk
	for off := 0; off < n; off += proto.MaxPayload {
		out = append(out, chunk{off, min(proto.MaxPayload, n-off)})
	}
	return out
}

// each runs f(0..n-1), up to parallel at a time, and returns the first
// error. After an error, no new calls start.
func each(n int, f func(int) error) error {
	if n == 1 {
		return f(0)
	}
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		first error
		sem   = make(chan struct{}, parallel)
	)
	for i := range n {
		sem <- struct{}{}
		mu.Lock()
		failed := first != nil
		mu.Unlock()
		if failed {
			break
		}
		wg.Add(1)
		go func() {
			defer func() { <-sem; wg.Done() }()
			if err := f(i); err != nil {
				mu.Lock()
				if first == nil {
					first = err
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return first
}

// Offline reports whether err means the phone isn't reachable right now,
// as opposed to the request itself failing.
func Offline(err error) bool {
	return errors.Is(err, conn.ErrOffline) || errors.Is(err, context.DeadlineExceeded)
}
