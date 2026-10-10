// Package clip syncs the Wayland clipboard using wl-clipboard, which works
// on any compositor implementing wlr- or ext-data-control (sway, niri,
// Hyprland, river, ...).
package clip

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"image"
	"image/draw"
	_ "image/gif"  // decoders for toPNG
	_ "image/jpeg" //
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "golang.org/x/image/bmp"  // more decoders for toPNG
	_ "golang.org/x/image/webp" //
)

const (
	// MaxText is the largest clipboard text that is synced.
	MaxText = 256 << 10
	// MaxImage is the largest clipboard image that is synced.
	MaxImage = 20 << 20
)

// Clipboard tracks the last synced content so changes don't bounce back and
// forth between desktop and phone.
type Clipboard struct {
	mu   sync.Mutex
	last [32]byte
}

// SetImage puts an image on the clipboard, as PNG when it can be converted:
// many apps (Signal and other Chromium/Electron ones) only paste image/png.
func (c *Clipboard) SetImage(mime string, data []byte) error {
	if png, err := toPNG(mime, data); err == nil {
		mime, data = "image/png", png
	} else {
		slog.Warn("keeping clipboard image as is", "type", mime, "err", err)
	}
	// Record what actually lands on the clipboard, so the watcher sees
	// it as an echo and doesn't send it back.
	c.Changed(string(data))
	return wlCopy(mime, bytes.NewReader(data))
}

// toPNG converts an image to PNG; PNG input is returned unchanged.
func toPNG(mime string, data []byte) ([]byte, error) {
	if mime == "image/png" {
		return data, nil
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	// The PNG encoder writes anything but these as 16 bits per channel
	// (e.g. JPEG's YCbCr), doubling the size for nothing.
	switch img.(type) {
	case *image.RGBA, *image.NRGBA, *image.Gray, *image.Paletted:
	default:
		rgba := image.NewRGBA(img.Bounds())
		draw.Draw(rgba, rgba.Bounds(), img, img.Bounds().Min, draw.Src)
		img = rgba
	}
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := enc.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Set writes text to the clipboard.
func (c *Clipboard) Set(text string) error {
	c.Changed(text)
	return wlCopy("text/plain;charset=utf-8", strings.NewReader(text))
}

// wlCopy runs wl-copy, which stays in the background to serve the
// selection. It gets a session of its own so stopping the daemon (Ctrl-C,
// a restart) doesn't take the clipboard contents with it.
func wlCopy(mime string, data io.Reader) error {
	cmd := exec.Command("wl-copy", "--type", mime)
	cmd.Stdin = data
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd.Run()
}

// Changed reports whether text differs from the last text synced in either
// direction, and records it as the latest.
func (c *Clipboard) Changed(text string) bool {
	h := sha256.Sum256([]byte(text))
	c.mu.Lock()
	defer c.mu.Unlock()
	if h == c.last {
		return false
	}
	c.last = h
	return true
}

// Watch runs `wl-paste --watch exe clip --watch`, so each selection change
// starts the CLI, which forwards it to the daemon. wl-paste is
// restarted if it exits, until ctx is cancelled.
func Watch(ctx context.Context, exe string) {
	for {
		cmd := exec.CommandContext(ctx, "wl-paste", "--watch", exe, "clip", "--watch")
		cmd.Stderr = os.Stderr
		err := cmd.Run()
		if ctx.Err() != nil {
			return
		}
		slog.Warn("wl-paste exited, restarting", "err", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

// Get returns the current clipboard text.
func Get() (string, error) {
	out, err := exec.Command("wl-paste", "--no-newline", "--type", "text").Output()
	if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
		return "", fmt.Errorf("wl-paste: %s", strings.TrimSpace(string(ee.Stderr)))
	}
	if err != nil {
		return "", fmt.Errorf("wl-paste: %w", err)
	}
	return string(out), nil
}

// Types lists the MIME types the current selection is offered as.
func Types() []string {
	out, err := exec.Command("wl-paste", "--list-types").Output()
	if err != nil {
		return nil
	}
	return strings.Fields(string(out))
}

// IsSensitive reports whether a password manager (KeePassXC, ...) flagged a
// selection offered as types as secret.
func IsSensitive(types []string) bool {
	return slices.Contains(types, "x-kde-passwordManagerHint")
}

// ImageType returns the image type to sync for a selection offered as
// types, or "" if it's text (or neither). Anything that offers plain text
// is treated as text, so copying text from a browser stays text.
func ImageType(types []string) string {
	isText := func(t string) bool {
		return t == "text/plain" || strings.HasPrefix(t, "text/plain;") || t == "UTF8_STRING" || t == "STRING" || t == "TEXT"
	}
	if slices.ContainsFunc(types, isText) {
		return ""
	}
	if slices.Contains(types, "image/png") {
		return "image/png"
	}
	for _, t := range types {
		if strings.HasPrefix(t, "image/") {
			return t
		}
	}
	return ""
}

// GetImage reads the selection as mime, up to MaxImage+1 bytes.
func GetImage(mime string) ([]byte, error) {
	cmd := exec.Command("wl-paste", "--type", mime)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("wl-paste: %w", err)
	}
	data, err := io.ReadAll(io.LimitReader(out, MaxImage+1))
	cmd.Process.Kill()
	cmd.Wait()
	return data, err
}

// ImageFile returns the image type of the file at path, judged by its
// content, or "" if it isn't an image the clipboard can take.
func ImageFile(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || fi.Size() > MaxImage {
		return ""
	}
	head := make([]byte, 512)
	n, _ := io.ReadFull(f, head)
	if mime := http.DetectContentType(head[:n]); strings.HasPrefix(mime, "image/") {
		return mime
	}
	return ""
}

// Images keeps the most recent desktop clipboard image for phones to fetch.
type Images struct {
	mu   sync.Mutex
	id   string
	mime string
	data []byte
}

// Put replaces the stored image and returns its id.
func (im *Images) Put(mime string, data []byte) string {
	im.mu.Lock()
	defer im.mu.Unlock()
	im.id, im.mime, im.data = rand.Text(), mime, data
	return im.id
}

// Get returns the image with id, if it is still the latest.
func (im *Images) Get(id string) (string, []byte, bool) {
	im.mu.Lock()
	defer im.mu.Unlock()
	if id == "" || id != im.id {
		return "", nil, false
	}
	return im.mime, im.data, true
}
