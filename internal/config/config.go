// Package config holds the daemon's user-editable settings, stored as JSON
// in $XDG_CONFIG_HOME/tether/config.json.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"

	"github.com/adrg/xdg"
)

type Config struct {
	Name      string `json:"name"`      // shown on the phone
	Listen    string `json:"listen"`    // address phones connect to
	Downloads string `json:"downloads"` // where received files are saved
	Clipboard bool   `json:"clipboard"` // send desktop clipboard changes to phones

	// What to do with desktop audio while the phone rings and during a call:
	// "pause" media, "lower" the volume to CallVolume percent of what it
	// was, or "none".
	RingAction string `json:"ring_action"`
	TalkAction string `json:"talk_action"`
	CallVolume int    `json:"call_volume"`

	// LegacyCallAction is the single setting older versions had for both.
	LegacyCallAction string `json:"call_action,omitempty"`
}

// Keys lists the settable keys, in display order.
var Keys = []string{"name", "listen", "downloads", "clipboard", "ring_action", "talk_action", "call_volume"}

// NeedsRestart reports whether changing key only takes effect after the
// daemon restarts.
func NeedsRestart(key string) bool {
	return key == "name" || key == "listen"
}

func Defaults() Config {
	host, _ := os.Hostname()
	downloads := xdg.UserDirs.Download
	if downloads == "" {
		downloads = filepath.Join(xdg.Home, "Downloads")
	}
	return Config{Name: host, Listen: ":1764", Downloads: downloads, Clipboard: true, RingAction: "lower", TalkAction: "pause", CallVolume: 40}
}

// Load reads path, filling in defaults for anything missing.
func Load(path string) (Config, error) {
	c := Defaults()
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	if c.LegacyCallAction != "" {
		var keys map[string]json.RawMessage
		json.Unmarshal(data, &keys)
		if _, ok := keys["ring_action"]; !ok {
			c.RingAction = c.LegacyCallAction
		}
		if _, ok := keys["talk_action"]; !ok {
			c.TalkAction = c.LegacyCallAction
		}
		c.LegacyCallAction = ""
	}
	return c, nil
}

func (c Config) Save(path string) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Set validates value and assigns it to key.
func (c *Config) Set(key, value string) error {
	switch key {
	case "name":
		if value == "" {
			return errors.New("name must not be empty")
		}
		c.Name = value
	case "listen":
		if _, _, err := net.SplitHostPort(value); err != nil {
			return fmt.Errorf("listen: %w", err)
		}
		c.Listen = value
	case "downloads":
		if !filepath.IsAbs(value) {
			return errors.New("downloads must be an absolute path")
		}
		if err := os.MkdirAll(value, 0o755); err != nil {
			return err
		}
		c.Downloads = filepath.Clean(value)
	case "clipboard":
		b, err := strconv.ParseBool(value)
		if err != nil {
			return errors.New("clipboard must be true or false")
		}
		c.Clipboard = b
	case "ring_action", "talk_action":
		if value != "pause" && value != "lower" && value != "none" {
			return fmt.Errorf("%s must be pause, lower or none", key)
		}
		if key == "ring_action" {
			c.RingAction = value
		} else {
			c.TalkAction = value
		}
	case "call_volume":
		n, err := strconv.Atoi(value)
		if err != nil || n < 0 || n > 100 {
			return errors.New("call_volume must be a percentage from 0 to 100")
		}
		c.CallVolume = n
	default:
		return fmt.Errorf("unknown setting %q", key)
	}
	return nil
}

func Path() string {
	return filepath.Join(xdg.ConfigHome, "tether", "config.json")
}
