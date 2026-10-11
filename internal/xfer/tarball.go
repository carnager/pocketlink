package xfer

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Entry is a file or directory in a tar offer.
type Entry struct {
	Rel     string    `json:"rel"`            // slash-separated path in the archive
	Path    string    `json:"path,omitempty"` // the file on disk; empty for directories
	Size    int64     `json:"size,omitempty"`
	ModTime time.Time `json:"mod_time"`
	Dir     bool      `json:"dir,omitempty"`
}

// Collect lists what sending paths as one archive holds. A single folder
// is sent as its contents, to be unpacked into a folder of its name;
// anything else is sent as is, files at the top and folders with their
// contents, and name is "". Symlinks and other special files are skipped.
func Collect(paths []string) (name string, entries []Entry, files int, err error) {
	if len(paths) == 1 {
		if fi, err := os.Stat(paths[0]); err == nil && fi.IsDir() {
			name = filepath.Base(filepath.Clean(paths[0]))
			entries, err := walk(paths[0], "")
			return name, entries, countFiles(entries), err
		}
	}
	taken := map[string]bool{}
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil {
			return "", nil, 0, err
		}
		top := unique(taken, filepath.Base(filepath.Clean(p)))
		if !fi.IsDir() {
			if !fi.Mode().IsRegular() {
				return "", nil, 0, fmt.Errorf("%s is not a regular file", p)
			}
			entries = append(entries, Entry{Rel: top, Path: p, Size: fi.Size(), ModTime: fi.ModTime().Truncate(time.Second)})
			continue
		}
		sub, err := walk(p, top)
		if err != nil {
			return "", nil, 0, err
		}
		entries = append(entries, sub...)
	}
	return "", entries, countFiles(entries), nil
}

// walk lists a directory tree, with paths in the archive under prefix.
func walk(root, prefix string) ([]Entry, error) {
	var out []Entry
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil // the folder itself; added below if it has a name
		}
		rel = path.Join(prefix, filepath.ToSlash(rel))
		if !d.IsDir() && !d.Type().IsRegular() {
			return nil // symlinks, sockets, ...
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		e := Entry{Rel: rel, ModTime: fi.ModTime().Truncate(time.Second), Dir: d.IsDir()}
		if !e.Dir {
			e.Path, e.Size = p, fi.Size()
		}
		out = append(out, e)
		return nil
	})
	if prefix != "" && err == nil {
		fi, serr := os.Stat(root)
		if serr != nil {
			return nil, serr
		}
		out = append([]Entry{{Rel: prefix, ModTime: fi.ModTime().Truncate(time.Second), Dir: true}}, out...)
	}
	return out, err
}

// unique returns name, or "name (N).ext" if it's taken already.
func unique(taken map[string]bool, name string) string {
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	cand := name
	for i := 1; taken[cand]; i++ {
		cand = fmt.Sprintf("%s (%d)%s", base, i, ext)
	}
	taken[cand] = true
	return cand
}

func countFiles(entries []Entry) int {
	n := 0
	for _, e := range entries {
		if !e.Dir {
			n++
		}
	}
	return n
}

// tarball is a tar archive of entries, read straight from the files. Its
// bytes depend only on the entries, so every request for it sees the same
// archive and a Range request can continue an interrupted one.
type tarball struct {
	segs []segment
	size int64
}

// segment is a stretch of the archive: header or padding bytes, or a file.
type segment struct {
	off  int64
	n    int64
	data []byte
	file *Entry
}

func newTarball(entries []Entry) (*tarball, error) {
	t := &tarball{}
	add := func(s segment) {
		s.off = t.size
		t.size += s.n
		t.segs = append(t.segs, s)
	}
	for i := range entries {
		e := &entries[i]
		h := &tar.Header{Name: e.Rel, ModTime: e.ModTime, Mode: 0o644, Typeflag: tar.TypeReg, Size: e.Size}
		if e.Dir {
			h.Name, h.Mode, h.Typeflag, h.Size = e.Rel+"/", 0o755, tar.TypeDir, 0
		}
		var buf bytes.Buffer
		// WriteHeader writes the header blocks (with a PAX header first for
		// long names) right away; the data is ours to add.
		if err := tar.NewWriter(&buf).WriteHeader(h); err != nil {
			return nil, fmt.Errorf("%s: %w", e.Rel, err)
		}
		add(segment{n: int64(buf.Len()), data: buf.Bytes()})
		if !e.Dir && e.Size > 0 {
			add(segment{n: e.Size, file: e})
			if pad := (512 - e.Size%512) % 512; pad > 0 {
				add(segment{n: pad, data: make([]byte, pad)})
			}
		}
	}
	add(segment{n: 1024, data: make([]byte, 1024)}) // end of archive
	return t, nil
}

// check reports whether every file is still as it was when offered.
func (t *tarball) check() error {
	for _, s := range t.segs {
		if s.file == nil {
			continue
		}
		fi, err := os.Stat(s.file.Path)
		if err != nil {
			return err
		}
		if fi.Size() != s.file.Size || !fi.ModTime().Truncate(time.Second).Equal(s.file.ModTime) {
			return fmt.Errorf("%s changed since it was offered", s.file.Path)
		}
	}
	return nil
}

func (t *tarball) open() *tarReader { return &tarReader{t: t} }

// tarReader reads a tarball, as an io.ReadSeeker for http.ServeContent.
type tarReader struct {
	t   *tarball
	pos int64
	f   *os.File // the file being read, if any
	fe  *Entry
}

var errFileChanged = errors.New("file changed while sending it")

func (r *tarReader) Read(p []byte) (int, error) {
	if r.pos >= r.t.size {
		return 0, io.EOF
	}
	i := sort.Search(len(r.t.segs), func(i int) bool { return r.t.segs[i].off+r.t.segs[i].n > r.pos })
	s := r.t.segs[i]
	at := r.pos - s.off
	p = p[:min(int64(len(p)), s.n-at)]
	var n int
	if s.data != nil {
		n = copy(p, s.data[at:])
	} else {
		if r.fe != s.file {
			r.closeFile()
			f, err := os.Open(s.file.Path)
			if err != nil {
				return 0, err
			}
			r.f, r.fe = f, s.file
		}
		var err error
		n, err = r.f.ReadAt(p, at)
		if n < len(p) {
			if err == nil || errors.Is(err, io.EOF) {
				err = errFileChanged // shorter than when offered
			}
			r.pos += int64(n)
			return n, err
		}
	}
	r.pos += int64(n)
	return n, nil
}

func (r *tarReader) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekCurrent:
		offset += r.pos
	case io.SeekEnd:
		offset += r.t.size
	}
	if offset < 0 {
		return 0, errors.New("negative position")
	}
	r.pos = offset
	return offset, nil
}

func (r *tarReader) closeFile() {
	if r.f != nil {
		r.f.Close()
		r.f, r.fe = nil, nil
	}
}

func (r *tarReader) Close() error {
	r.closeFile()
	return nil
}
