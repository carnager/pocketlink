package notify

import (
	"slices"
	"testing"
)

func TestShowArgs(t *testing.T) {
	for _, tc := range []struct {
		fm   []string
		want []string
	}{
		{[]string{"/usr/bin/dolphin"}, []string{"/usr/bin/dolphin", "--new-window", "--select", "/d/a b.jpg"}},
		{[]string{"nautilus"}, []string{"nautilus", "--new-window", "--select", "/d/a b.jpg"}},
		{[]string{"thunar"}, []string{"thunar", "/d"}},
		{[]string{"nemo", "--no-desktop"}, []string{"nemo", "--no-desktop", "/d"}},
	} {
		if got := showArgs(tc.fm, "/d/a b.jpg"); !slices.Equal(got, tc.want) {
			t.Errorf("showArgs(%v) = %v, want %v", tc.fm, got, tc.want)
		}
	}
}
