package phonefs

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"syscall"
	"time"

	"github.com/carnager/pocketlink/internal/proto"
)

// Server answers fs.* requests from a local directory. It is the phone side
// of the protocol, as used by pocketlink-sim and the tests, and the
// reference for what the Android app does.
type Server struct {
	Root string
}

// Handle answers one request. The reply goes back in a binary frame if it
// has a payload.
func (s *Server) Handle(e proto.Envelope, payload []byte) (proto.Envelope, []byte) {
	body, data, err := s.handle(e, payload)
	if err != nil {
		var pe *proto.Error
		if !errors.As(err, &pe) {
			pe = toError(err)
		}
		return proto.ErrorReply(e.Req, pe.Code, pe.Message), nil
	}
	r, err := proto.Reply(e.Req, body)
	if err != nil {
		return proto.ErrorReply(e.Req, proto.ErrIO, err.Error()), nil
	}
	return r, data
}

func decode[T any](e proto.Envelope) (T, error) {
	v, err := proto.Decode[T](e)
	if err != nil {
		return v, &proto.Error{Code: proto.ErrInvalid, Message: err.Error()}
	}
	return v, nil
}

func (s *Server) handle(e proto.Envelope, payload []byte) (any, []byte, error) {
	switch e.Type {
	case proto.TypeFSStat:
		r, err := decode[proto.FSPath](e)
		if err != nil {
			return nil, nil, err
		}
		ent, err := s.stat(r.Path)
		return ent, nil, err

	case proto.TypeFSList:
		r, err := decode[proto.FSPath](e)
		if err != nil {
			return nil, nil, err
		}
		des, err := os.ReadDir(s.local(r.Path))
		if err != nil {
			return nil, nil, err
		}
		l := proto.FSList{Entries: []proto.FSEntry{}}
		for _, d := range des {
			fi, err := d.Info()
			if err != nil {
				continue // vanished meanwhile
			}
			l.Entries = append(l.Entries, entry(d.Name(), fi))
		}
		return l, nil, nil

	case proto.TypeFSRead:
		r, err := decode[proto.FSRead](e)
		if err != nil {
			return nil, nil, err
		}
		if r.Offset < 0 || r.Length < 0 || r.Length > proto.MaxPayload {
			return nil, nil, &proto.Error{Code: proto.ErrInvalid, Message: "bad offset or length"}
		}
		f, err := os.Open(s.local(r.Path))
		if err != nil {
			return nil, nil, err
		}
		defer f.Close()
		buf := make([]byte, r.Length)
		n, err := f.ReadAt(buf, r.Offset)
		if err != nil && err != io.EOF {
			return nil, nil, err
		}
		return struct{}{}, buf[:n], nil

	case proto.TypeFSWrite:
		r, err := decode[proto.FSWrite](e)
		if err != nil {
			return nil, nil, err
		}
		if r.Offset < 0 || len(payload) > proto.MaxPayload {
			return nil, nil, &proto.Error{Code: proto.ErrInvalid, Message: "bad offset or length"}
		}
		flags := os.O_WRONLY
		if r.Create {
			flags |= os.O_CREATE | os.O_TRUNC
		}
		f, err := os.OpenFile(s.local(r.Path), flags, 0o644)
		if err != nil {
			return nil, nil, err
		}
		if _, err := f.WriteAt(payload, r.Offset); err != nil {
			f.Close()
			return nil, nil, err
		}
		if err := f.Close(); err != nil {
			return nil, nil, err
		}
		ent, err := s.stat(r.Path)
		return ent, nil, err

	case proto.TypeFSCreate:
		r, err := decode[proto.FSCreate](e)
		if err != nil {
			return nil, nil, err
		}
		flags := os.O_WRONLY | os.O_CREATE
		if r.Exclusive {
			flags |= os.O_EXCL
		}
		if r.Truncate {
			flags |= os.O_TRUNC
		}
		f, err := os.OpenFile(s.local(r.Path), flags, 0o644)
		if err != nil {
			return nil, nil, err
		}
		if err := f.Close(); err != nil {
			return nil, nil, err
		}
		ent, err := s.stat(r.Path)
		return ent, nil, err

	case proto.TypeFSMkdir:
		r, err := decode[proto.FSPath](e)
		if err != nil {
			return nil, nil, err
		}
		if err := os.Mkdir(s.local(r.Path), 0o755); err != nil {
			return nil, nil, err
		}
		ent, err := s.stat(r.Path)
		return ent, nil, err

	case proto.TypeFSRemove:
		r, err := decode[proto.FSRemove](e)
		if err != nil {
			return nil, nil, err
		}
		if Clean(r.Path) == "/" {
			return nil, nil, &proto.Error{Code: proto.ErrPerm, Message: "can't remove the root"}
		}
		p := s.local(r.Path)
		if _, err := os.Lstat(p); err != nil {
			return nil, nil, err
		}
		if r.Recursive {
			err = os.RemoveAll(p)
		} else {
			err = os.Remove(p)
		}
		return struct{}{}, nil, err

	case proto.TypeFSRename:
		r, err := decode[proto.FSRename](e)
		if err != nil {
			return nil, nil, err
		}
		if Clean(r.From) == "/" || Clean(r.To) == "/" {
			return nil, nil, &proto.Error{Code: proto.ErrPerm, Message: "can't move the root"}
		}
		to := s.local(r.To)
		if r.NoReplace {
			if _, err := os.Lstat(to); err == nil {
				return nil, nil, fs.ErrExist
			}
		}
		return struct{}{}, nil, os.Rename(s.local(r.From), to)

	case proto.TypeFSSetAttr:
		r, err := decode[proto.FSSetAttr](e)
		if err != nil {
			return nil, nil, err
		}
		p := s.local(r.Path)
		if r.Size != nil {
			if fi, err := os.Stat(p); err == nil && fi.IsDir() {
				return nil, nil, syscall.EISDIR
			}
			if err := os.Truncate(p, *r.Size); err != nil {
				return nil, nil, err
			}
		}
		if r.MTime != nil {
			t := time.UnixMilli(*r.MTime)
			if err := os.Chtimes(p, time.Time{}, t); err != nil {
				return nil, nil, err
			}
		}
		ent, err := s.stat(r.Path)
		return ent, nil, err

	case proto.TypeFSStatFS:
		r, err := decode[proto.FSPath](e)
		if err != nil {
			return nil, nil, err
		}
		var st syscall.Statfs_t
		if err := syscall.Statfs(s.local(r.Path), &st); err != nil {
			return nil, nil, err
		}
		// Bsize is 32 bits wide on 32-bit systems.
		return proto.FSSpace{Total: int64(st.Blocks) * int64(st.Bsize), Free: int64(st.Bavail) * int64(st.Bsize)}, nil, nil
	}
	return nil, nil, &proto.Error{Code: proto.ErrUnsupported, Message: e.Type}
}

// local maps a protocol path into the root. Cleaning it as a rooted path
// first means ".." can't climb out.
func (s *Server) local(p string) string {
	return filepath.Join(s.Root, filepath.FromSlash(Clean(p)))
}

func (s *Server) stat(p string) (proto.FSEntry, error) {
	fi, err := os.Stat(s.local(p))
	if err != nil {
		return proto.FSEntry{}, err
	}
	name := path.Base(Clean(p))
	if name == "/" {
		name = ""
	}
	return entry(name, fi), nil
}

func entry(name string, fi fs.FileInfo) proto.FSEntry {
	e := proto.FSEntry{Name: name, Dir: fi.IsDir(), MTime: fi.ModTime().UnixMilli()}
	if !e.Dir {
		e.Size = fi.Size()
	}
	return e
}

func toError(err error) *proto.Error {
	code := proto.ErrIO
	var errno syscall.Errno
	errors.As(err, &errno)
	// Errno first: Go also counts ENOTEMPTY as fs.ErrExist.
	switch {
	case errno == syscall.ENOTEMPTY:
		code = proto.ErrNotEmpty
	case errno == syscall.ENOTDIR:
		code = proto.ErrNotDir
	case errno == syscall.EISDIR:
		code = proto.ErrIsDir
	case errno == syscall.ENOSPC:
		code = proto.ErrNoSpace
	case errno == syscall.EINVAL:
		code = proto.ErrInvalid
	case errors.Is(err, fs.ErrNotExist):
		code = proto.ErrNotFound
	case errors.Is(err, fs.ErrExist):
		code = proto.ErrExist
	case errors.Is(err, fs.ErrPermission):
		code = proto.ErrPerm
	}
	return &proto.Error{Code: code, Message: err.Error()}
}

// ServeStream answers fs.readstream and fs.writestream, moving the data
// with send (to the desktop: the file from the offset, and its length) or
// recv (from the desktop). It returns the reply to send afterwards.
func (s *Server) ServeStream(e proto.Envelope, send func(io.Reader, int64) error, recv func() (io.ReadCloser, error)) proto.Envelope {
	fail := func(err error) proto.Envelope {
		var pe *proto.Error
		if !errors.As(err, &pe) {
			pe = toError(err)
		}
		return proto.ErrorReply(e.Req, pe.Code, pe.Message)
	}
	r, err := decode[proto.FSStream](e)
	if err != nil {
		return fail(err)
	}
	if r.Offset < 0 {
		return fail(&proto.Error{Code: proto.ErrInvalid, Message: "bad offset"})
	}
	p := s.local(r.Path)

	switch e.Type {
	case proto.TypeFSReadStream:
		f, err := os.Open(p)
		if err != nil {
			return fail(err)
		}
		defer f.Close()
		fi, err := f.Stat()
		if err != nil {
			return fail(err)
		}
		if fi.IsDir() {
			return fail(syscall.EISDIR)
		}
		n := max(fi.Size()-r.Offset, 0)
		if err := send(io.NewSectionReader(f, r.Offset, n), n); err != nil {
			return fail(err)
		}
		reply, _ := proto.Reply(e.Req, struct{}{})
		return reply

	case proto.TypeFSWriteStream:
		f, err := os.OpenFile(p, os.O_WRONLY, 0)
		if err != nil {
			return fail(err)
		}
		body, err := recv()
		if err != nil {
			f.Close()
			return fail(err)
		}
		_, err = io.Copy(io.NewOffsetWriter(f, r.Offset), body)
		body.Close()
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return fail(err)
		}
		ent, err := s.stat(r.Path)
		if err != nil {
			return fail(err)
		}
		reply, _ := proto.Reply(e.Req, ent)
		return reply
	}
	return fail(&proto.Error{Code: proto.ErrUnsupported, Message: e.Type})
}
