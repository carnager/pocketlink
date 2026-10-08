// Package clip syncs the Wayland clipboard using wl-clipboard, which works
// on any compositor implementing wlr- or ext-data-control (sway, niri,
// Hyprland, river, ...).
package clip

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// MaxText is the largest clipboard text that is synced.
const MaxText = 256 << 10

// Clipboard tracks the last synced content so changes don't bounce back and
// forth between desktop and phone.
type Clipboard struct {
	mu   sync.Mutex
	last [32]byte
}

// Set writes text to the clipboard.
func (c *Clipboard) Set(text string) error {
	c.Changed(text)
	cmd := exec.Command("wl-copy", "--type", "text/plain;charset=utf-8")
	cmd.Stdin = strings.NewReader(text)
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
// is piped into the CLI, which forwards it to the daemon. wl-paste is
// restarted if it exits, until ctx is cancelled.
func Watch(ctx context.Context, exe string) {
	for {
		cmd := exec.CommandContext(ctx, "wl-paste", "--type", "text", "--watch", exe, "clip", "--watch")
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

// IsSensitive reports whether a password manager (KeePassXC, ...) flagged
// the current selection as secret.
func IsSensitive() bool {
	out, err := exec.Command("wl-paste", "--list-types").Output()
	if err != nil {
		return false
	}
	return bytes.Contains(out, []byte("x-kde-passwordManagerHint"))
}
