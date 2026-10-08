// Package xfer implements resumable file transfer over plain HTTPS, served
// on the same TLS listener as the WebSocket channel and authenticated by the
// same client certificate.
//
// Phone → desktop: the phone picks a transfer ID (stable for the same file,
// so it survives app restarts) and PUTs the file body to /files/in/{id} with
// the headers below. If the connection drops, HEAD /files/in/{id} reports how
// many bytes arrived, and the phone PUTs the rest starting at that offset.
// Repeating a PUT for a finished transfer just reports it complete, so a lost
// response never causes a duplicate file.
//
// Desktop → phone: the daemon sends a file.offer frame; the phone GETs
// /files/out/{id}, resuming with standard Range requests, and sends a
// file.done frame when it's finished with the offer.
package xfer

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"tether/internal/pair"
)

const (
	HeaderName     = "X-Tether-Name" // URL query-escaped file name
	HeaderSize     = "X-Tether-Size" // total file size in bytes
	HeaderOffset   = "X-Tether-Offset"
	HeaderComplete = "X-Tether-Complete"
)

const (
	stallTimeout = 60 * time.Second
	partMaxAge   = 7 * 24 * time.Hour
	doneMaxAge   = 24 * time.Hour
)

var idRE = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)

var errSuperseded = errors.New("superseded by a newer request for the same transfer")

type Incoming struct {
	dir    string // partial uploads, per device
	store  *pair.Store
	onDone func(dev pair.Device, path string)

	mu      sync.Mutex
	destDir string // where finished files go
	active  map[string]*upload
}

type upload struct {
	rc   *http.ResponseController
	done chan struct{}

	mu         sync.Mutex
	superseded bool
}

type meta struct {
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	Created time.Time `json:"created"`
	SavedAs string    `json:"saved_as,omitempty"`
}

func NewIncoming(dir, destDir string, store *pair.Store, onDone func(pair.Device, string)) *Incoming {
	return &Incoming{dir: dir, destDir: destDir, store: store, onDone: onDone, active: map[string]*upload{}}
}

// ServeHEAD reports the progress of a transfer: 404 if unknown, otherwise
// the received byte count in X-Tether-Offset.
func (in *Incoming) ServeHEAD(w http.ResponseWriter, r *http.Request) {
	dev, id, ok := in.request(w, r)
	if !ok {
		return
	}
	partPath, metaPath := in.paths(dev, id)
	m, err := readMeta(metaPath)
	if errors.Is(err, fs.ErrNotExist) {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	setProgress(w, m, offset(m, partPath))
	w.WriteHeader(http.StatusOK)
}

// ServePUT appends the request body at X-Tether-Offset. It answers 204 while
// the file is incomplete, 201 once it is saved, and 409 with the current
// offset if the client's offset is wrong.
func (in *Incoming) ServePUT(w http.ResponseWriter, r *http.Request) {
	dev, id, ok := in.request(w, r)
	if !ok {
		return
	}
	name, nameErr := url.QueryUnescape(r.Header.Get(HeaderName))
	size, sizeErr := strconv.ParseInt(r.Header.Get(HeaderSize), 10, 64)
	start, offErr := strconv.ParseInt(r.Header.Get(HeaderOffset), 10, 64)
	if nameErr != nil || name == "" || sizeErr != nil || offErr != nil || size < 0 || start < 0 || start > size {
		http.Error(w, "missing or invalid transfer headers", http.StatusBadRequest)
		return
	}

	key := dev.ID + "/" + id
	u := in.acquire(key, http.NewResponseController(w))
	defer in.release(key, u)

	partPath, metaPath := in.paths(dev, id)
	m, err := readMeta(metaPath)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		m = meta{Name: name, Size: size, Created: time.Now()}
		if err := writeMeta(metaPath, m); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	case err != nil:
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	case m.Name != name || m.Size != size:
		http.Error(w, "transfer id reused for a different file", http.StatusConflict)
		return
	}

	cur := offset(m, partPath)
	if m.SavedAs != "" {
		setProgress(w, m, cur)
		w.WriteHeader(http.StatusCreated)
		return
	}
	if start != cur {
		setProgress(w, m, cur)
		http.Error(w, "offset mismatch", http.StatusConflict)
		return
	}

	f, err := os.OpenFile(partPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	n, copyErr := io.Copy(f, io.LimitReader(&stallReader{r: r.Body, u: u}, size-cur))
	cur += n
	if copyErr == nil && cur == size {
		copyErr = f.Sync()
	}
	if err := f.Close(); copyErr == nil {
		copyErr = err
	}
	setProgress(w, m, cur)
	if copyErr != nil {
		slog.Info("upload interrupted", "device", dev.Name, "name", m.Name, "offset", cur, "size", size, "err", copyErr)
		http.Error(w, "upload interrupted", http.StatusBadRequest)
		return
	}
	if cur < size {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	saved, err := finish(partPath, in.DestDir(), m.Name)
	if err != nil {
		slog.Error("saving received file", "name", m.Name, "err", err)
		http.Error(w, "could not save file", http.StatusInternalServerError)
		return
	}
	m.SavedAs = saved
	if err := writeMeta(metaPath, m); err != nil {
		slog.Warn("recording finished transfer", "err", err)
	}
	slog.Info("received file", "device", dev.Name, "path", saved, "size", size)
	if in.onDone != nil {
		in.onDone(dev, saved)
	}
	setProgress(w, m, cur)
	w.WriteHeader(http.StatusCreated)
}

// DestDir returns where finished files are saved.
func (in *Incoming) DestDir() string {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.destDir
}

// SetDestDir changes where finished files are saved, from the next
// completed transfer on.
func (in *Incoming) SetDestDir(dir string) {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.destDir = dir
}

// Cleanup deletes abandoned partial uploads and old completion records.
func (in *Incoming) Cleanup() {
	filepath.WalkDir(in.dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		age := time.Since(info.ModTime())
		switch filepath.Ext(p) {
		case ".json":
			if m, err := readMeta(p); err == nil && m.SavedAs != "" {
				if age > doneMaxAge {
					os.Remove(p)
				}
				return nil
			}
			part := strings.TrimSuffix(p, ".json") + ".part"
			if pi, err := os.Stat(part); err == nil {
				age = min(age, time.Since(pi.ModTime()))
			}
			if age > partMaxAge {
				os.Remove(p)
				os.Remove(part)
			}
		case ".part":
			if _, err := os.Stat(strings.TrimSuffix(p, ".part") + ".json"); errors.Is(err, fs.ErrNotExist) {
				os.Remove(p)
			}
		}
		return nil
	})
}

func (in *Incoming) request(w http.ResponseWriter, r *http.Request) (pair.Device, string, bool) {
	dev, ok := in.store.Peer(r.TLS)
	if !ok {
		http.Error(w, "not paired", http.StatusForbidden)
		return pair.Device{}, "", false
	}
	id := r.PathValue("id")
	if !idRE.MatchString(id) {
		http.Error(w, "invalid transfer id", http.StatusBadRequest)
		return pair.Device{}, "", false
	}
	return dev, id, true
}

func (in *Incoming) paths(dev pair.Device, id string) (part, meta string) {
	dir := filepath.Join(in.dir, pair.ShortID(dev.ID))
	return filepath.Join(dir, id+".part"), filepath.Join(dir, id+".json")
}

// acquire makes the caller the only request writing to a transfer. When a
// phone resumes after a network change, the request from the old connection
// may still be blocked reading a body that will never arrive; it is kicked
// out rather than making the new request wait for a TCP timeout.
func (in *Incoming) acquire(key string, rc *http.ResponseController) *upload {
	in.mu.Lock()
	for {
		old := in.active[key]
		if old == nil {
			break
		}
		old.mu.Lock()
		old.superseded = true
		old.rc.SetReadDeadline(time.Now())
		old.mu.Unlock()
		in.mu.Unlock()
		<-old.done
		in.mu.Lock()
	}
	u := &upload{rc: rc, done: make(chan struct{})}
	in.active[key] = u
	in.mu.Unlock()
	return u
}

func (in *Incoming) release(key string, u *upload) {
	in.mu.Lock()
	if in.active[key] == u {
		delete(in.active, key)
	}
	in.mu.Unlock()
	close(u.done)
}

// stallReader fails the upload if no data arrives for stallTimeout, or once
// a newer request for the same transfer has taken over.
type stallReader struct {
	r io.Reader
	u *upload
}

func (s *stallReader) Read(p []byte) (int, error) {
	s.u.mu.Lock()
	if s.u.superseded {
		s.u.mu.Unlock()
		return 0, errSuperseded
	}
	s.u.rc.SetReadDeadline(time.Now().Add(stallTimeout))
	s.u.mu.Unlock()
	return s.r.Read(p)
}

func offset(m meta, partPath string) int64 {
	if m.SavedAs != "" {
		return m.Size
	}
	fi, err := os.Stat(partPath)
	if err != nil {
		return 0
	}
	return fi.Size()
}

func setProgress(w http.ResponseWriter, m meta, cur int64) {
	w.Header().Set(HeaderOffset, strconv.FormatInt(cur, 10))
	if m.SavedAs != "" {
		w.Header().Set(HeaderComplete, "1")
	}
}

func readMeta(path string) (meta, error) {
	var m meta
	data, err := os.ReadFile(path)
	if err != nil {
		return m, err
	}
	return m, json.Unmarshal(data, &m)
}

func writeMeta(path string, m meta) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
