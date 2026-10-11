// Package davfs serves paired phones' files over WebDAV on localhost, so
// file managers can browse them: webdav://127.0.0.1:1765/ in Dolphin,
// dav://127.0.0.1:1765/ in Thunar or Nautilus. Each phone is a folder at
// the top level.
package davfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/webdav"

	"github.com/carnager/pocketlink/internal/pair"
	"github.com/carnager/pocketlink/internal/phonefs"
	"github.com/carnager/pocketlink/internal/proto"
)

// Devices lists paired devices; *pair.Store implements it.
type Devices interface {
	List() []pair.Device
}

type Server struct {
	caller  phonefs.Caller
	devices Devices
	dav     *webdav.Handler

	mu     sync.Mutex
	caches map[string]*phonefs.Cache
}

func New(caller phonefs.Caller, devices Devices) *Server {
	s := &Server{caller: caller, devices: devices, caches: map[string]*phonefs.Cache{}}
	s.dav = &webdav.Handler{
		FileSystem: davFS{s},
		LockSystem: webdav.NewMemLS(),
		Logger: func(r *http.Request, err error) {
			if err != nil {
				slog.Debug("webdav", "method", r.Method, "path", r.URL.Path, "err", err)
			}
		},
	}
	return s
}

// Changed handles an fs.changed notification from a device.
func (s *Server) Changed(dev, dir string) {
	s.cache(dev).Changed(dir)
}

// Reset forgets what's cached about a device, e.g. when it reconnects.
func (s *Server) Reset(dev string) {
	s.cache(dev).Reset()
}

func (s *Server) cache(dev string) *phonefs.Cache {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.caches[dev]
	if c == nil {
		c = phonefs.NewCache(phonefs.New(s.caller, dev))
		s.caches[dev] = c
	}
	return c
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Without a depth limit, a PROPFIND would walk the whole phone. RFC 4918
	// lets servers refuse that.
	if r.Method == "PROPFIND" {
		if d := r.Header.Get("Depth"); d == "" || strings.EqualFold(d, "infinity") {
			w.Header().Set("Content-Type", "application/xml; charset=utf-8")
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `<?xml version="1.0" encoding="utf-8"?><D:error xmlns:D="DAV:"><D:propfind-finite-depth/></D:error>`)
			return
		}
	}
	// Say plainly why a phone can't be reached, rather than letting it
	// surface as whatever status WebDAV picks for a failed operation.
	if dev, p, ok := s.resolve(r.URL.Path); ok && dev.ID != "" {
		c := s.cache(dev.ID)
		err := c.Ready()
		if err == nil && r.Method == "PROPFIND" && r.Header.Get("Depth") == "1" {
			// WebDAV would show a folder it may not list as empty. Listing
			// it first (which the cache then answers) lets us say why.
			if _, lerr := c.List(r.Context(), p); errors.Is(lerr, fs.ErrPermission) || phonefs.Offline(lerr) {
				err = lerr
			}
		}
		switch {
		case err == nil:
		case phonefs.Offline(err):
			http.Error(w, "The phone isn't connected.", http.StatusServiceUnavailable)
			return
		case errors.Is(err, fs.ErrPermission):
			http.Error(w, "File access isn't allowed on the phone. Turn on File access in the pocketlink app.", http.StatusForbidden)
			return
		default:
			http.Error(w, err.Error(), http.StatusNotImplemented)
			return
		}
	}
	s.dav.ServeHTTP(w, r)
}

// dirNames gives each paired device a folder name: its name, made safe for
// a path element, with its short ID added if two devices share a name.
func (s *Server) dirNames() map[string]pair.Device {
	devs := s.devices.List()
	count := map[string]int{}
	for _, d := range devs {
		count[safeName(d.Name)]++
	}
	out := make(map[string]pair.Device, len(devs))
	for _, d := range devs {
		n := safeName(d.Name)
		if count[n] > 1 {
			n += " (" + pair.ShortID(d.ID) + ")"
		}
		out[n] = d
	}
	return out
}

// Folder returns the URL path of a device's folder, e.g. "/Pixel%208/".
func (s *Server) Folder(dev string) string {
	for n, d := range s.dirNames() {
		if d.ID == dev {
			return (&url.URL{Path: "/" + n + "/"}).EscapedPath()
		}
	}
	return ""
}

func safeName(n string) string {
	n = strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r < 0x20 {
			return '_'
		}
		return r
	}, strings.TrimSpace(n))
	if n == "" || n == "." || n == ".." {
		n = "phone"
	}
	return n
}

// resolve splits a WebDAV path into the device it's on and the path on the
// phone. dev.ID is "" for the top level; ok is false if there's no such
// device.
func (s *Server) resolve(name string) (dev pair.Device, phonePath string, ok bool) {
	name = path.Clean("/" + name)
	if name == "/" {
		return pair.Device{}, "/", true
	}
	first, rest, _ := strings.Cut(name[1:], "/")
	d, ok := s.dirNames()[first]
	if !ok {
		return pair.Device{}, "", false
	}
	return d, "/" + rest, true
}

// deviceDir is the entry for a device's own folder. It exists whether or
// not the phone is connected, so listing the top level never needs a phone.
func deviceDir(d pair.Device) proto.FSEntry {
	return proto.FSEntry{Dir: true, MTime: d.LastSeen.UnixMilli()}
}

// davFS is the webdav.FileSystem: the top level lists devices, and
// everything below goes to that device's phonefs.Cache.
type davFS struct{ s *Server }

func (d davFS) open(op, name string) (*phonefs.Cache, pair.Device, string, error) {
	dev, p, ok := d.s.resolve(name)
	if !ok {
		return nil, dev, "", &fs.PathError{Op: op, Path: name, Err: fs.ErrNotExist}
	}
	if dev.ID == "" {
		return nil, dev, "/", nil
	}
	return d.s.cache(dev.ID), dev, p, nil
}

// readOnly refuses changes to the top level and to the device folders
// themselves.
func readOnly(op, name string) error {
	return &fs.PathError{Op: op, Path: name, Err: fs.ErrPermission}
}

func (d davFS) Mkdir(ctx context.Context, name string, perm os.FileMode) error {
	c, _, p, err := d.open("mkdir", name)
	if err != nil {
		return err
	}
	if c == nil || p == "/" {
		return readOnly("mkdir", name)
	}
	_, err = c.Mkdir(ctx, p)
	return osErr("mkdir", name, err)
}

func (d davFS) OpenFile(ctx context.Context, name string, flag int, perm os.FileMode) (webdav.File, error) {
	c, dev, p, err := d.open("open", name)
	if err != nil {
		return nil, err
	}
	writable := flag&(os.O_WRONLY|os.O_RDWR) != 0
	if c == nil {
		if writable || flag&os.O_CREATE != 0 {
			return nil, readOnly("open", name)
		}
		return &rootDir{s: d.s}, nil
	}

	var e proto.FSEntry
	switch {
	case p == "/":
		if writable || flag&os.O_CREATE != 0 {
			return nil, readOnly("open", name)
		}
		e = deviceDir(dev)
	case flag&os.O_CREATE != 0 && flag&os.O_TRUNC != 0 && flag&os.O_EXCL == 0 && writable && c.CanWriteFile():
		// A whole file being written (a PUT): it's created together with
		// its contents, in a single request if it's small. Check what we
		// can now; that's usually from the cache.
		if parent, perr := c.Stat(ctx, path.Dir(p)); perr != nil {
			return nil, osErr("open", name, perr)
		} else if !parent.Dir {
			return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
		}
		if cur, serr := c.Stat(ctx, p); serr == nil && cur.Dir {
			return nil, &fs.PathError{Op: "open", Path: name, Err: errIsDir}
		}
		f := newFile(ctx, c, p, path.Base(path.Clean("/"+name)), proto.FSEntry{Name: path.Base(p)}, true)
		f.pending = true
		return f, nil
	case flag&os.O_CREATE != 0:
		e, err = c.Create(ctx, p, flag&os.O_EXCL != 0, flag&os.O_TRUNC != 0)
	default:
		e, err = c.Stat(ctx, p)
		if err == nil && flag&os.O_TRUNC != 0 && writable && !e.Dir {
			zero := int64(0)
			e, err = c.SetAttr(ctx, p, &zero, nil)
		}
	}
	if err != nil {
		return nil, osErr("open", name, err)
	}
	return newFile(ctx, c, p, path.Base(path.Clean("/"+name)), e, writable), nil
}

func (d davFS) RemoveAll(ctx context.Context, name string) error {
	c, _, p, err := d.open("remove", name)
	if err != nil {
		return err
	}
	if c == nil || p == "/" {
		return readOnly("remove", name)
	}
	err = c.Remove(ctx, p, true)
	if errors.Is(err, fs.ErrNotExist) {
		return nil // like os.RemoveAll
	}
	return osErr("remove", name, err)
}

func (d davFS) Rename(ctx context.Context, oldName, newName string) error {
	dev1, from, ok1 := d.s.resolve(oldName)
	dev2, to, ok2 := d.s.resolve(newName)
	if !ok1 || !ok2 {
		return &fs.PathError{Op: "rename", Path: oldName, Err: fs.ErrNotExist}
	}
	if dev1.ID == "" || from == "/" || to == "/" || dev1.ID != dev2.ID {
		return readOnly("rename", oldName)
	}
	return osErr("rename", oldName, d.s.cache(dev1.ID).Rename(ctx, from, to, false))
}

func (d davFS) Stat(ctx context.Context, name string) (os.FileInfo, error) {
	c, dev, p, err := d.open("stat", name)
	if err != nil {
		return nil, err
	}
	if c == nil {
		return fileInfo{e: proto.FSEntry{Dir: true}}, nil
	}
	if p == "/" {
		return fileInfo{name: path.Base(path.Clean("/" + name)), e: deviceDir(dev)}, nil
	}
	e, err := c.Stat(ctx, p)
	if err != nil {
		return nil, osErr("stat", name, err)
	}
	return fileInfo{name: path.Base(path.Clean("/" + name)), e: e}, nil
}

// osErr turns a phone's error into one the os package recognizes, which is
// how WebDAV picks the status code.
func osErr(op, name string, err error) error {
	var pe *proto.Error
	if err == nil || !errors.As(err, &pe) {
		return err
	}
	switch pe.Code {
	case proto.ErrNotFound:
		err = fs.ErrNotExist
	case proto.ErrExist:
		err = fs.ErrExist
	case proto.ErrPerm:
		err = fs.ErrPermission
	}
	return &fs.PathError{Op: op, Path: name, Err: err}
}

type fileInfo struct {
	name string
	e    proto.FSEntry
}

func (fi fileInfo) Name() string { return fi.name }
func (fi fileInfo) Size() int64  { return fi.e.Size }
func (fi fileInfo) IsDir() bool  { return fi.e.Dir }
func (fi fileInfo) Sys() any     { return nil }

func (fi fileInfo) Mode() fs.FileMode {
	if fi.e.Dir {
		return fs.ModeDir | 0o755
	}
	return 0o644
}

func (fi fileInfo) ModTime() time.Time {
	if fi.e.MTime == 0 {
		return time.Time{}
	}
	return time.UnixMilli(fi.e.MTime)
}

// ContentType goes by the extension. Otherwise WebDAV would read the start
// of every file in a listing to sniff it.
func (fi fileInfo) ContentType(context.Context) (string, error) {
	if t := mime.TypeByExtension(path.Ext(fi.name)); t != "" {
		return t, nil
	}
	return "application/octet-stream", nil
}

// rootDir is the top level: one folder per paired device.
type rootDir struct {
	s    *Server
	ents []fs.FileInfo
	pos  int
	done bool
}

func (r *rootDir) Close() error { return nil }
func (r *rootDir) Read([]byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: "/", Err: errIsDir}
}
func (r *rootDir) Write([]byte) (int, error)      { return 0, readOnly("write", "/") }
func (r *rootDir) Seek(int64, int) (int64, error) { return 0, nil }
func (r *rootDir) Stat() (fs.FileInfo, error)     { return fileInfo{e: proto.FSEntry{Dir: true}}, nil }

func (r *rootDir) Readdir(count int) ([]fs.FileInfo, error) {
	if !r.done {
		for n, d := range r.s.dirNames() {
			r.ents = append(r.ents, fileInfo{name: n, e: deviceDir(d)})
		}
		slices.SortFunc(r.ents, func(a, b fs.FileInfo) int { return strings.Compare(a.Name(), b.Name()) })
		r.done = true
	}
	return readdir(r.ents, &r.pos, count)
}

func readdir(ents []fs.FileInfo, pos *int, count int) ([]fs.FileInfo, error) {
	rest := ents[*pos:]
	if count <= 0 {
		*pos = len(ents)
		return rest, nil
	}
	if len(rest) == 0 {
		return nil, io.EOF
	}
	n := min(count, len(rest))
	*pos += n
	return rest[:n], nil
}
