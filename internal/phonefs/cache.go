package phonefs

import (
	"context"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/carnager/pocketlink/internal/proto"
)

const (
	// How long listings and stats stay valid. A phone that sends fs.changed
	// for directories it has listed keeps the cache correct, so entries can
	// live long. Without that, the cache only absorbs the bursts of
	// identical requests a file manager makes when it opens a folder.
	watchedTTL   = 5 * time.Minute
	unwatchedTTL = 2 * time.Second

	maxCached = 20000 // entries per map before stale ones are swept
)

// Cache is a Client with a metadata cache in front of it. Stats are answered
// from the parent directory's listing when that is cached, so opening a
// folder costs one round trip however many files it holds. Changes made
// through the Cache update it; changes made on the phone arrive with
// Changed.
//
// While the phone isn't connected, nothing is served from the cache.
type Cache struct {
	*Client

	mu       sync.Mutex
	gen      uint64 // bumped by every change; a fetch from before one isn't stored
	dirs     map[string]cachedDir
	stats    map[string]cachedStat
	inflight map[string]*flight
}

type cachedDir struct {
	entries map[string]proto.FSEntry
	at      time.Time
}

type cachedStat struct {
	e       proto.FSEntry
	missing bool
	at      time.Time
}

type flight struct {
	done chan struct{}
	ents []proto.FSEntry
	err  error
}

func NewCache(cl *Client) *Cache {
	return &Cache{Client: cl, dirs: map[string]cachedDir{}, stats: map[string]cachedStat{}, inflight: map[string]*flight{}}
}

func (c *Cache) ttl() time.Duration {
	if c.c.Supports(c.dev, proto.FeatureFSWatch) {
		return watchedTTL
	}
	return unwatchedTTL
}

func notFound(p string) error {
	return &proto.Error{Code: proto.ErrNotFound, Message: p}
}

func (c *Cache) Stat(ctx context.Context, p string) (proto.FSEntry, error) {
	p = Clean(p)
	if err := c.Ready(); err != nil {
		return proto.FSEntry{}, err
	}
	since := time.Now().Add(-c.ttl())
	c.mu.Lock()
	if p != "/" {
		if d, ok := c.dirs[path.Dir(p)]; ok && d.at.After(since) {
			e, ok := d.entries[path.Base(p)]
			c.mu.Unlock()
			if !ok {
				return proto.FSEntry{}, notFound(p)
			}
			return e, nil
		}
	}
	if s, ok := c.stats[p]; ok && s.at.After(since) {
		c.mu.Unlock()
		if s.missing {
			return proto.FSEntry{}, notFound(p)
		}
		return s.e, nil
	}
	gen := c.gen
	c.mu.Unlock()

	e, err := c.Client.Stat(ctx, p)
	missing := isNotFound(err)
	if err == nil || missing {
		c.mu.Lock()
		if c.gen == gen {
			c.putStatLocked(p, cachedStat{e: e, missing: missing, at: time.Now()})
		}
		c.mu.Unlock()
	}
	return e, err
}

// List returns a directory's entries, sorted by nothing in particular.
// Concurrent calls for the same directory share one request.
func (c *Cache) List(ctx context.Context, p string) ([]proto.FSEntry, error) {
	p = Clean(p)
	if err := c.Ready(); err != nil {
		return nil, err
	}
	since := time.Now().Add(-c.ttl())
	c.mu.Lock()
	if d, ok := c.dirs[p]; ok && d.at.After(since) {
		ents := values(d.entries)
		c.mu.Unlock()
		return ents, nil
	}
	if f := c.inflight[p]; f != nil {
		c.mu.Unlock()
		select {
		case <-f.done:
			return f.ents, f.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f := &flight{done: make(chan struct{})}
	c.inflight[p] = f
	gen := c.gen
	c.mu.Unlock()

	// Not tied to one caller's context, since others may be waiting on it.
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), DefaultTimeout)
	f.ents, f.err = c.Client.List(fctx, p)
	cancel()

	c.mu.Lock()
	delete(c.inflight, p)
	if f.err == nil && c.gen == gen {
		m := make(map[string]proto.FSEntry, len(f.ents))
		for _, e := range f.ents {
			m[e.Name] = e
		}
		if len(c.dirs) >= maxCached {
			c.sweepLocked()
		}
		c.dirs[p] = cachedDir{entries: m, at: time.Now()}
	}
	c.mu.Unlock()
	close(f.done)
	return f.ents, f.err
}

func (c *Cache) WriteAt(ctx context.Context, p string, data []byte, off int64) (proto.FSEntry, error) {
	e, err := c.Client.WriteAt(ctx, p, data, off)
	c.updated(p, e, err)
	return e, err
}

func (c *Cache) Create(ctx context.Context, p string, exclusive, truncate bool) (proto.FSEntry, error) {
	e, err := c.Client.Create(ctx, p, exclusive, truncate)
	c.updated(p, e, err)
	return e, err
}

func (c *Cache) Mkdir(ctx context.Context, p string) (proto.FSEntry, error) {
	e, err := c.Client.Mkdir(ctx, p)
	c.updated(p, e, err)
	return e, err
}

func (c *Cache) SetAttr(ctx context.Context, p string, size *int64, mtime *time.Time) (proto.FSEntry, error) {
	e, err := c.Client.SetAttr(ctx, p, size, mtime)
	c.updated(p, e, err)
	return e, err
}

func (c *Cache) Remove(ctx context.Context, p string, recursive bool) error {
	err := c.Client.Remove(ctx, p, recursive)
	p = Clean(p)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gen++
	if err != nil {
		c.forgetLocked(p)
		return err
	}
	c.goneLocked(p)
	return nil
}

func (c *Cache) Rename(ctx context.Context, from, to string, noReplace bool) error {
	err := c.Client.Rename(ctx, from, to, noReplace)
	from, to = Clean(from), Clean(to)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gen++
	c.forgetLocked(to)
	if err != nil {
		c.forgetLocked(from)
		return err
	}
	c.goneLocked(from)
	return nil
}

// Changed handles an fs.changed notification for directory p.
func (c *Cache) Changed(p string) {
	p = Clean(p)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gen++
	delete(c.dirs, p)
	delete(c.stats, p)
	for k := range c.stats {
		if k != "/" && path.Dir(k) == p {
			delete(c.stats, k)
		}
	}
}

// Reset forgets everything, e.g. after a reconnect, since notifications may
// have been lost.
func (c *Cache) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gen++
	clear(c.dirs)
	clear(c.stats)
}

// updated records the result of a change that returns the entry afterwards.
func (c *Cache) updated(p string, e proto.FSEntry, err error) {
	p = Clean(p)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gen++
	if err != nil {
		c.forgetLocked(p)
		return
	}
	c.putStatLocked(p, cachedStat{e: e, at: time.Now()})
	if d, ok := c.dirs[path.Dir(p)]; ok && p != "/" {
		d.entries[path.Base(p)] = e
	}
}

func (c *Cache) putStatLocked(p string, s cachedStat) {
	if len(c.stats) >= maxCached {
		c.sweepLocked()
	}
	c.stats[p] = s
}

// forgetLocked drops everything known about p, its contents and its
// parent's listing.
func (c *Cache) forgetLocked(p string) {
	c.dropTreeLocked(p)
	delete(c.dirs, path.Dir(p))
}

// goneLocked records that p and everything under it no longer exist.
func (c *Cache) goneLocked(p string) {
	c.dropTreeLocked(p)
	if p == "/" {
		return
	}
	c.stats[p] = cachedStat{missing: true, at: time.Now()}
	if d, ok := c.dirs[path.Dir(p)]; ok {
		delete(d.entries, path.Base(p))
	}
}

func (c *Cache) dropTreeLocked(p string) {
	prefix := strings.TrimSuffix(p, "/") + "/"
	for k := range c.dirs {
		if k == p || strings.HasPrefix(k, prefix) {
			delete(c.dirs, k)
		}
	}
	for k := range c.stats {
		if k == p || strings.HasPrefix(k, prefix) {
			delete(c.stats, k)
		}
	}
}

func (c *Cache) sweepLocked() {
	since := time.Now().Add(-c.ttl())
	for k, d := range c.dirs {
		if !d.at.After(since) {
			delete(c.dirs, k)
		}
	}
	for k, s := range c.stats {
		if !s.at.After(since) {
			delete(c.stats, k)
		}
	}
}

func values(m map[string]proto.FSEntry) []proto.FSEntry {
	out := make([]proto.FSEntry, 0, len(m))
	for _, e := range m {
		out = append(out, e)
	}
	return out
}

func isNotFound(err error) bool {
	pe, ok := err.(*proto.Error)
	return ok && pe.Code == proto.ErrNotFound
}
