package xfer

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/carnager/pocketlink/internal/pair"
)

// Offer is a desktop file made available to some devices. The file is
// served from its original location rather than copied, so it must not
// change until the phone has fetched it.
type Offer struct {
	ID      string    `json:"id"`
	Path    string    `json:"path"`
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	Mime    string    `json:"mime"`
	ModTime time.Time `json:"mod_time"`
	Devices []string  `json:"devices"` // devices that haven't sent file.done yet
	Created time.Time `json:"created"`
}

// Outgoing is the persisted registry of offers, and serves them at
// GET /files/out/{id}.
type Outgoing struct {
	store *pair.Store

	mu     sync.Mutex
	path   string
	offers map[string]*Offer
}

func OpenOutgoing(path string, store *pair.Store) (*Outgoing, error) {
	o := &Outgoing{store: store, path: path, offers: map[string]*Offer{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return o, nil
	}
	if err != nil {
		return nil, err
	}
	var list []*Offer
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for _, off := range list {
		o.offers[off.ID] = off
	}
	return o, nil
}

func (o *Outgoing) Add(path string, devices []string) (Offer, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return Offer{}, err
	}
	if !fi.Mode().IsRegular() {
		return Offer{}, fmt.Errorf("%s is not a regular file", path)
	}
	mt := mime.TypeByExtension(filepath.Ext(path))
	if mt == "" {
		mt = "application/octet-stream"
	}
	off := &Offer{
		ID:      rand.Text(),
		Path:    path,
		Name:    filepath.Base(path),
		Size:    fi.Size(),
		Mime:    mt,
		ModTime: fi.ModTime(),
		Devices: slices.Clone(devices),
		Created: time.Now(),
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.offers[off.ID] = off
	return *off, o.saveLocked()
}

// Done records that a device is finished with an offer.
func (o *Outgoing) Done(devID, id string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	off := o.offers[id]
	if off == nil {
		return nil
	}
	off.Devices = slices.DeleteFunc(off.Devices, func(d string) bool { return d == devID })
	if len(off.Devices) == 0 {
		delete(o.offers, id)
	}
	return o.saveLocked()
}

// ForgetDevice withdraws all offers to an unpaired device.
func (o *Outgoing) ForgetDevice(devID string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	for id, off := range o.offers {
		off.Devices = slices.DeleteFunc(off.Devices, func(d string) bool { return d == devID })
		if len(off.Devices) == 0 {
			delete(o.offers, id)
		}
	}
	return o.saveLocked()
}

// Expire drops offers older than maxAge.
func (o *Outgoing) Expire(maxAge time.Duration) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	for id, off := range o.offers {
		if time.Since(off.Created) > maxAge {
			delete(o.offers, id)
		}
	}
	return o.saveLocked()
}

func (o *Outgoing) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	dev, ok := o.store.Peer(r.TLS)
	if !ok {
		http.Error(w, "not paired", http.StatusForbidden)
		return
	}
	o.mu.Lock()
	var off Offer
	if p := o.offers[r.PathValue("id")]; p != nil {
		off = *p
	}
	o.mu.Unlock()
	if off.ID == "" || !slices.Contains(off.Devices, dev.ID) {
		http.Error(w, "no such offer", http.StatusNotFound)
		return
	}

	f, err := os.Open(off.Path)
	if err != nil {
		http.Error(w, "file no longer available", http.StatusGone)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || fi.Size() != off.Size || !fi.ModTime().Equal(off.ModTime) {
		http.Error(w, "file changed since it was offered", http.StatusGone)
		return
	}
	w.Header().Set("Content-Type", off.Mime)
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": off.Name}))
	// ServeContent handles Range and If-Range, which is all resuming needs.
	http.ServeContent(w, r, off.Name, fi.ModTime(), f)
}

func (o *Outgoing) saveLocked() error {
	list := make([]*Offer, 0, len(o.offers))
	for _, off := range o.offers {
		list = append(list, off)
	}
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(o.path), 0o700); err != nil {
		return err
	}
	tmp := o.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, o.path)
}
