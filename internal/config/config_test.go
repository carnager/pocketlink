package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLegacyCallActionMigrates(t *testing.T) {
	for _, tc := range []struct {
		file       string
		ring, talk string
	}{
		{`{"call_action":"none"}`, "none", "none"},
		{`{"call_action":"pause","ring_action":"lower"}`, "lower", "pause"},
		{`{}`, Defaults().RingAction, Defaults().TalkAction},
	} {
		path := filepath.Join(t.TempDir(), "config.json")
		os.WriteFile(path, []byte(tc.file), 0o600)
		c, err := Load(path)
		if err != nil {
			t.Fatal(err)
		}
		if c.RingAction != tc.ring || c.TalkAction != tc.talk {
			t.Errorf("%s: ring=%q talk=%q, want %q %q", tc.file, c.RingAction, c.TalkAction, tc.ring, tc.talk)
		}
		if err := c.Save(path); err != nil {
			t.Fatal(err)
		}
		data, _ := os.ReadFile(path)
		if c2, _ := Load(path); c2 != c {
			t.Errorf("%s: round trip changed config: %s", tc.file, data)
		}
	}
}

func TestSetActions(t *testing.T) {
	c := Defaults()
	if err := c.Set("ring_action", "none"); err != nil || c.RingAction != "none" {
		t.Fatalf("ring_action: %v %q", err, c.RingAction)
	}
	if err := c.Set("talk_action", "loud"); err == nil {
		t.Fatal("talk_action accepted an invalid value")
	}
}
