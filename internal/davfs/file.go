package davfs

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"slices"
	"strings"
	"syscall"

	"github.com/carnager/pocketlink/internal/phonefs"
	"github.com/carnager/pocketlink/internal/proto"
)

var errIsDir = syscall.EISDIR

const (
	// Reads start small, since a lot of them only sniff a file's first
	// bytes, and grow while they stay sequential. Once a read is streaming,
	// the next window is fetched while the current one is consumed.
	minWindow = 128 << 10
	maxWindow = 4 * proto.MaxPayload

	// Writes are collected into chunks this big, and one chunk is sent
	// while the next is being collected.
	writeChunk = 4 * proto.MaxPayload
)

// file is an open file or directory on a phone. It lives for one WebDAV
// request, and nothing is held open on the phone.
type file struct {
	ctx      context.Context
	c        *phonefs.Cache
	path     string // on the phone
	name     string
	e        proto.FSEntry
	writable bool
	pos      int64

	// Reading: the current window and, while streaming, the next one.
	rbuf   []byte
	roff   int64
	window int
	ahead  *window

	// Writing: data not yet sent, and the chunk being sent.
	wbuf    []byte
	woff    int64
	sending *chunk
	werr    error

	ents []fs.FileInfo
	dpos int
	read bool // ents is loaded
}

type chunk struct {
	done chan struct{}
	e    proto.FSEntry
	err  error
}

type window struct {
	off  int64
	done chan struct{}
	data []byte
	err  error
}

func newFile(ctx context.Context, c *phonefs.Cache, p, name string, e proto.FSEntry, writable bool) *file {
	return &file{ctx: ctx, c: c, path: p, name: name, e: e, writable: writable, window: minWindow}
}

func (f *file) pathErr(op string, err error) error {
	if err == nil || errors.Is(err, io.EOF) {
		return err
	}
	return osErr(op, f.name, err)
}

func (f *file) Read(p []byte) (int, error) {
	if f.e.Dir {
		return 0, &fs.PathError{Op: "read", Path: f.name, Err: errIsDir}
	}
	if err := f.flush(); err != nil {
		return 0, f.pathErr("read", err)
	}
	if len(p) == 0 {
		return 0, nil
	}
	if f.pos < f.roff || f.pos >= f.roff+int64(len(f.rbuf)) {
		if err := f.fill(); err != nil {
			return 0, f.pathErr("read", err)
		}
		if len(f.rbuf) == 0 {
			return 0, io.EOF
		}
	}
	n := copy(p, f.rbuf[f.pos-f.roff:])
	f.pos += int64(n)
	return n, nil
}

// fill loads the window at f.pos.
func (f *file) fill() error {
	sequential := f.rbuf != nil && f.pos == f.roff+int64(len(f.rbuf))
	if sequential {
		f.window = min(2*f.window, maxWindow)
	} else {
		f.window = minWindow
	}

	var w *window
	if a := f.ahead; a != nil && a.off == f.pos {
		w = a
	} else {
		w = f.fetch(f.pos, f.window)
	}
	f.ahead = nil
	<-w.done
	if w.err != nil && !errors.Is(w.err, io.EOF) {
		f.rbuf = nil
		return w.err
	}
	f.rbuf, f.roff = w.data, w.off
	// A short window means the end of the file; otherwise, if this is a
	// stream, fetch on.
	if next := w.off + int64(len(w.data)); sequential && w.err == nil && next < f.e.Size {
		f.ahead = f.fetch(next, f.window)
	}
	return nil
}

// fetch starts reading n bytes at off in the background.
func (f *file) fetch(off int64, n int) *window {
	// No point asking for much more than the file has.
	if left := f.e.Size - off; left >= 0 && left < int64(n) {
		n = max(int(left), 4096)
	}
	w := &window{off: off, done: make(chan struct{})}
	go func() {
		defer close(w.done)
		buf := make([]byte, n)
		k, err := f.c.ReadAt(f.ctx, f.path, buf, off)
		w.data, w.err = buf[:k], err
	}()
	return w
}

func (f *file) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		offset += f.pos
	case io.SeekEnd:
		if err := f.flush(); err != nil {
			return 0, f.pathErr("seek", err)
		}
		offset += f.e.Size
	default:
		return 0, &fs.PathError{Op: "seek", Path: f.name, Err: fs.ErrInvalid}
	}
	if offset < 0 {
		return 0, &fs.PathError{Op: "seek", Path: f.name, Err: fs.ErrInvalid}
	}
	f.pos = offset
	return offset, nil
}

func (f *file) Write(p []byte) (int, error) {
	if !f.writable || f.e.Dir {
		return 0, &fs.PathError{Op: "write", Path: f.name, Err: fs.ErrPermission}
	}
	if f.werr != nil {
		return 0, f.pathErr("write", f.werr)
	}
	// What we read before may be stale now.
	f.rbuf, f.ahead = nil, nil
	if len(f.wbuf) > 0 && f.pos != f.woff+int64(len(f.wbuf)) {
		f.send()
	}
	if len(f.wbuf) == 0 {
		f.woff = f.pos
	}
	f.wbuf = append(f.wbuf, p...)
	f.pos += int64(len(p))
	// Whole chunks only, so each fills its requests to the phone.
	if len(f.wbuf) >= writeChunk {
		f.sendN(writeChunk)
	}
	return len(p), nil
}

// send starts writing all collected data.
func (f *file) send() {
	f.sendN(len(f.wbuf))
}

// sendN starts writing the first n collected bytes, after the previous
// chunk is done.
func (f *file) sendN(n int) {
	f.wait()
	if n == 0 || f.werr != nil {
		return
	}
	data, off := f.wbuf[:n:n], f.woff
	f.wbuf = append([]byte(nil), f.wbuf[n:]...)
	f.woff += int64(n)
	c := &chunk{done: make(chan struct{})}
	f.sending = c
	go func() {
		defer close(c.done)
		c.e, c.err = f.c.WriteAt(f.ctx, f.path, data, off)
	}()
}

// wait waits for the chunk being sent, if any.
func (f *file) wait() {
	c := f.sending
	if c == nil {
		return
	}
	<-c.done
	f.sending = nil
	if c.err != nil {
		f.werr = c.err
		return
	}
	f.e.Size, f.e.MTime = max(f.e.Size, c.e.Size), c.e.MTime
}

// flush sends everything written so far and waits for it.
func (f *file) flush() error {
	f.send()
	f.wait()
	return f.werr
}

func (f *file) Close() error {
	return f.pathErr("close", f.flush())
}

func (f *file) Stat() (fs.FileInfo, error) {
	if err := f.flush(); err != nil {
		return nil, f.pathErr("stat", err)
	}
	return fileInfo{name: f.name, e: f.e}, nil
}

func (f *file) Readdir(count int) ([]fs.FileInfo, error) {
	if !f.e.Dir {
		return nil, &fs.PathError{Op: "readdir", Path: f.name, Err: syscall.ENOTDIR}
	}
	if !f.read {
		ents, err := f.c.List(f.ctx, f.path)
		if err != nil {
			return nil, f.pathErr("readdir", err)
		}
		f.ents = make([]fs.FileInfo, len(ents))
		for i, e := range ents {
			f.ents[i] = fileInfo{name: e.Name, e: e}
		}
		slices.SortFunc(f.ents, func(a, b fs.FileInfo) int { return strings.Compare(a.Name(), b.Name()) })
		f.read = true
	}
	return readdir(f.ents, &f.dpos, count)
}
