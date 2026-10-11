package phonefs

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/carnager/pocketlink/internal/proto"
)

func newCacheEnv(t *testing.T) (*Cache, *Local, string) {
	t.Helper()
	root := t.TempDir()
	for _, f := range []string{"a.jpg", "b.jpg", "sub/c.txt"} {
		p := filepath.Join(root, f)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	l := &Local{Server: &Server{Root: root}, Features: []string{proto.FeatureFS, proto.FeatureFSWatch, proto.FeatureFSWriteCreate}}
	return NewCache(New(l, "dev")), l, root
}

func TestCacheServesStatsFromListing(t *testing.T) {
	c, l, _ := newCacheEnv(t)
	ctx := context.Background()

	if _, err := c.List(ctx, "/"); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		for _, p := range []string{"/a.jpg", "/b.jpg", "/sub"} {
			if _, err := c.Stat(ctx, p); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := c.Stat(ctx, "/missing"); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("stat of missing file: %v", err)
		}
		if _, err := c.List(ctx, "/"); err != nil {
			t.Fatal(err)
		}
	}
	if n := l.Calls(proto.TypeFSList); n != 1 {
		t.Errorf("%d list requests, want 1", n)
	}
	if n := l.Calls(proto.TypeFSStat); n != 0 {
		t.Errorf("%d stat requests, want 0", n)
	}
}

func TestCacheFollowsChanges(t *testing.T) {
	c, l, root := newCacheEnv(t)
	ctx := context.Background()
	c.List(ctx, "/")

	// Our own changes update the cache without asking again.
	if _, err := c.Create(ctx, "/new.txt", true, false); err != nil {
		t.Fatal(err)
	}
	if e, err := c.WriteAt(ctx, "/new.txt", []byte("hello"), 0); err != nil || e.Size != 5 {
		t.Fatalf("write: %+v, %v", e, err)
	}
	if e, err := c.Stat(ctx, "/new.txt"); err != nil || e.Size != 5 {
		t.Fatalf("stat after write: %+v, %v", e, err)
	}
	if err := c.Rename(ctx, "/a.jpg", "/sub/a.jpg", false); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Stat(ctx, "/a.jpg"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("stat of moved file: %v", err)
	}
	if _, err := c.Stat(ctx, "/sub/a.jpg"); err != nil {
		t.Fatalf("stat at new place: %v", err)
	}
	if err := c.Remove(ctx, "/sub", true); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Stat(ctx, "/sub/c.txt"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("stat inside removed dir: %v", err)
	}
	l.Calls(proto.TypeFSList)

	// A change on the phone shows up after a notification, not before.
	os.WriteFile(filepath.Join(root, "phone.txt"), nil, 0o644)
	if _, err := c.Stat(ctx, "/phone.txt"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("cache should still say missing: %v", err)
	}
	c.Changed("/")
	if _, err := c.Stat(ctx, "/phone.txt"); err != nil {
		t.Fatalf("after fs.changed: %v", err)
	}
	// The stat fetched the folder again, which then answers other stats.
	if _, err := c.Stat(ctx, "/b.jpg"); err != nil {
		t.Fatal(err)
	}
	if lists, stats := l.Calls(proto.TypeFSList), l.Calls(proto.TypeFSStat); lists != 1 || stats != 0 {
		t.Errorf("%d list and %d stat requests, want 1 and 0", lists, stats)
	}
}

// Checking one target after another in a folder costs one request.
func TestCacheStatsNewNames(t *testing.T) {
	c, l, _ := newCacheEnv(t)
	ctx := context.Background()
	for i := range 20 {
		name := "/sub/new" + string(rune('a'+i))
		if _, err := c.Stat(ctx, name); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("stat %s: %v", name, err)
		}
		if _, err := c.WriteFile(ctx, name, []byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.Stat(ctx, "/sub/newa"); err != nil {
		t.Fatalf("stat of a written file: %v", err)
	}
	if lists, stats := l.Calls(proto.TypeFSList), l.Calls(proto.TypeFSStat); lists != 1 || stats != 0 {
		t.Errorf("%d list and %d stat requests, want 1 and 0", lists, stats)
	}
	if _, err := c.Stat(ctx, "/nodir/x"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("stat in a missing folder: %v", err)
	}
}

func TestCacheOffline(t *testing.T) {
	c, l, _ := newCacheEnv(t)
	ctx := context.Background()
	c.List(ctx, "/")
	l.Offline = true
	if _, err := c.Stat(ctx, "/a.jpg"); !Offline(err) {
		t.Fatalf("cached stat while offline: %v", err)
	}
}
