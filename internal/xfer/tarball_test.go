package xfer

import (
	"archive/tar"
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func writeTree(t *testing.T, root string, files map[string][]byte) {
	t.Helper()
	for rel, data := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func readTar(t *testing.T, data []byte) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeDir {
			out[h.Name] = nil
		} else {
			out[h.Name] = b
		}
	}
}

func TestTarball(t *testing.T) {
	dir := t.TempDir()
	big := make([]byte, 100000)
	rand.Read(big)
	long := strings.Repeat("long-name-", 15) + ".txt" // needs a PAX header
	writeTree(t, dir, map[string][]byte{
		"Photos/a.jpg":     big,
		"Photos/sub/b.txt": []byte("hello"),
		"Photos/empty.bin": {},
		"Photos/" + long:   []byte("long"),
		"loose.txt":        []byte("loose"),
		"other/loose.txt":  []byte("second loose"),
	})
	os.Symlink("a.jpg", filepath.Join(dir, "Photos", "link.jpg"))

	// A single folder: its contents, unpacked into a folder of its name.
	name, entries, files, err := Collect([]string{filepath.Join(dir, "Photos")})
	if err != nil || name != "Photos" || files != 4 {
		t.Fatalf("Collect folder: %q, %d files, %v", name, files, err)
	}
	tb, err := newTarball(entries)
	if err != nil {
		t.Fatal(err)
	}
	r := tb.open()
	all, err := io.ReadAll(r)
	r.Close()
	if err != nil || int64(len(all)) != tb.size {
		t.Fatalf("read %d of %d bytes: %v", len(all), tb.size, err)
	}
	got := readTar(t, all)
	want := map[string][]byte{"a.jpg": big, "sub/": nil, "sub/b.txt": []byte("hello"), "empty.bin": {}, long: []byte("long")}
	for k, v := range want {
		if g, ok := got[k]; !ok || !bytes.Equal(g, v) {
			t.Errorf("%s: got %d bytes (present %v), want %d", k, len(g), ok, len(v))
		}
	}
	if _, ok := got["link.jpg"]; ok || len(got) != len(want) {
		t.Errorf("entries: %v", slices.Collect(func(yield func(string) bool) {
			for k := range got {
				yield(k)
			}
		}))
	}

	// The same archive every time, and reading from any offset continues it.
	tb2, _ := newTarball(entries)
	for _, off := range []int64{0, 1, 511, 512, 1000, 70000, tb.size - 1024, tb.size - 1} {
		r := tb2.open()
		r.Seek(off, io.SeekStart)
		rest, err := io.ReadAll(r)
		r.Close()
		if err != nil || !bytes.Equal(rest, all[off:]) {
			t.Fatalf("resuming at %d: %d bytes, %v", off, len(rest), err)
		}
	}
	if n, _ := tb.open().Seek(0, io.SeekEnd); n != tb.size {
		t.Fatalf("size by seeking: %d, want %d", n, tb.size)
	}

	// Several things: as they are, clashing names made unique.
	name, entries, files, err = Collect([]string{filepath.Join(dir, "loose.txt"), filepath.Join(dir, "other", "loose.txt"), filepath.Join(dir, "Photos", "sub")})
	if err != nil || name != "" || files != 3 {
		t.Fatalf("Collect several: %q, %d files, %v", name, files, err)
	}
	tb, _ = newTarball(entries)
	r = tb.open()
	all, _ = io.ReadAll(r)
	got = readTar(t, all)
	for k, v := range map[string]string{"loose.txt": "loose", "loose (1).txt": "second loose", "sub/b.txt": "hello"} {
		if string(got[k]) != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if _, ok := got["sub/"]; !ok {
		t.Error("folder entry missing")
	}

	// A file that changes after the offer is caught.
	if err := tb.check(); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "loose.txt"), []byte("changed!"), 0o644)
	if err := tb.check(); err == nil {
		t.Fatal("changed file not noticed")
	}
}
