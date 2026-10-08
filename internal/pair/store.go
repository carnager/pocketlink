package pair

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

type Device struct {
	ID       string    `json:"id"` // certificate fingerprint
	Name     string    `json:"name"`
	PairedAt time.Time `json:"paired_at"`
	LastSeen time.Time `json:"last_seen,omitzero"`
}

// Store is the set of trusted devices, persisted as JSON.
type Store struct {
	mu      sync.Mutex
	path    string
	devices map[string]*Device
}

func OpenStore(path string) (*Store, error) {
	s := &Store{path: path, devices: map[string]*Device{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var list []*Device
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, err
	}
	for _, d := range list {
		s.devices[d.ID] = d
	}
	return s, nil
}

func (s *Store) Get(id string) (Device, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.devices[id]
	if !ok {
		return Device{}, false
	}
	return *d, true
}

// Peer returns the paired device that presented the client certificate of
// a TLS connection.
func (s *Store) Peer(cs *tls.ConnectionState) (Device, bool) {
	if cs == nil || len(cs.PeerCertificates) == 0 {
		return Device{}, false
	}
	return s.Get(Fingerprint(cs.PeerCertificates[0].Raw))
}

func (s *Store) Add(id, name string) (Device, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := &Device{ID: id, Name: name, PairedAt: time.Now()}
	s.devices[id] = d
	return *d, s.saveLocked()
}

func (s *Store) Remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.devices, id)
	return s.saveLocked()
}

func (s *Store) Rename(id, name string) error {
	return s.update(id, func(d *Device) { d.Name = name })
}

func (s *Store) Touch(id string) error {
	return s.update(id, func(d *Device) { d.LastSeen = time.Now() })
}

func (s *Store) update(id string, f func(*Device)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.devices[id]
	if !ok {
		return nil
	}
	f(d)
	return s.saveLocked()
}

func (s *Store) List() []Device {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Device, 0, len(s.devices))
	for _, d := range s.devices {
		out = append(out, *d)
	}
	slices.SortFunc(out, func(a, b Device) int { return strings.Compare(a.Name, b.Name) })
	return out
}

func (s *Store) saveLocked() error {
	list := make([]*Device, 0, len(s.devices))
	for _, d := range s.devices {
		list = append(list, d)
	}
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
