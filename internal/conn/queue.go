package conn

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"tether/internal/proto"
)

const maxQueued = 1000

// Queue is a per-device outbox persisted to disk. Frames stay queued until
// the device acknowledges them, so nothing is lost across disconnects or
// daemon restarts.
type Queue struct {
	mu    sync.Mutex
	path  string
	items []proto.Envelope
	dead  bool
	wake  chan struct{}
}

func OpenQueue(path string) (*Queue, error) {
	q := &Queue{path: path, wake: make(chan struct{}, 1)}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(data, &q.items); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	return q, nil
}

func (q *Queue) Push(e proto.Envelope) error {
	q.mu.Lock()
	if proto.Coalesces(e.Type) {
		q.items = slices.DeleteFunc(q.items, func(o proto.Envelope) bool { return o.Type == e.Type })
	}
	q.items = append(q.items, e)
	if over := len(q.items) - maxQueued; over > 0 {
		q.items = slices.Delete(q.items, 0, over)
	}
	err := q.saveLocked()
	q.mu.Unlock()

	select {
	case q.wake <- struct{}{}:
	default:
	}
	return err
}

func (q *Queue) Ack(id string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	i := slices.IndexFunc(q.items, func(e proto.Envelope) bool { return e.ID == id })
	if i < 0 {
		return nil
	}
	q.items = slices.Delete(q.items, i, i+1)
	return q.saveLocked()
}

func (q *Queue) Pending() []proto.Envelope {
	q.mu.Lock()
	defer q.mu.Unlock()
	return slices.Clone(q.items)
}

func (q *Queue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}

// Wake fires after Push. It has a buffer of one, so a single waiter never
// misses a push.
func (q *Queue) Wake() <-chan struct{} {
	return q.wake
}

// Remove deletes the queue file and stops further writes to it.
func (q *Queue) Remove() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.dead = true
	q.items = nil
	if err := os.Remove(q.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func (q *Queue) saveLocked() error {
	if q.dead {
		return nil
	}
	data, err := json.Marshal(q.items)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(q.path), 0o700); err != nil {
		return err
	}
	tmp := q.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, q.path)
}
