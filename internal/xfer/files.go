package xfer

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unicode"
	"unicode/utf8"
)

const maxNameBytes = 200

// finish moves a completed upload into destDir under a sanitized, unused
// name and returns the final path.
func finish(part, destDir, name string) (string, error) {
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", err
	}
	dst, err := reserve(destDir, sanitizeName(name))
	if err != nil {
		return "", err
	}
	if err := moveFile(part, dst); err != nil {
		os.Remove(dst)
		return "", err
	}
	os.Chmod(dst, 0o644)
	return dst, nil
}

// reserve atomically creates an empty file named name, or "name (N).ext" if
// that is taken, so concurrent transfers never overwrite each other.
func reserve(dir, name string) (string, error) {
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	for i := range 1000 {
		cand := name
		if i > 0 {
			cand = fmt.Sprintf("%s (%d)%s", base, i, ext)
		}
		p := filepath.Join(dir, cand)
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err == nil {
			f.Close()
			return p, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", err
		}
	}
	return "", fmt.Errorf("no free file name for %q", name)
}

// moveFile renames src over dst, copying if they are on different
// filesystems.
func moveFile(src, dst string) error {
	err := os.Rename(src, dst)
	if !errors.Is(err, syscall.EXDEV) {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Remove(src)
}

// sanitizeName turns a name chosen by the phone into a safe single path
// element: no separators, no control characters, not hidden, bounded length.
func sanitizeName(name string) string {
	name = strings.Map(func(r rune) rune {
		switch {
		case r == '/' || r == '\\':
			return '_'
		case unicode.IsControl(r):
			return -1
		}
		return r
	}, name)
	name = strings.TrimLeft(strings.TrimSpace(name), ".")
	if name == "" {
		return "file"
	}
	if len(name) > maxNameBytes {
		ext := filepath.Ext(name)
		if len(ext) > 20 {
			ext = ""
		}
		base := strings.TrimSuffix(name, ext)[:maxNameBytes-len(ext)]
		for !utf8.ValidString(base) {
			base = base[:len(base)-1]
		}
		name = base + ext
	}
	return name
}
