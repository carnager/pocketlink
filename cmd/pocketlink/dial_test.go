package main

import "testing"

func TestDialNumber(t *testing.T) {
	ok := map[string]string{
		"tel:+49-30-1234567":                 "+49-30-1234567",
		"TEL:+1 (201) 555-0123":              "+1 (201) 555-0123",
		"tel://+4930123":                     "+4930123",
		"tel:%2B49%2030%20123":               "+49 30 123",
		"tel:7042;phone-context=example.com": "7042",
		"tel:+1-201-555-0123;ext=42":         "+1-201-555-0123",
		" 030 123 456 ":                      "030 123 456",
		"*#06#":                              "*#06#",
	}
	for in, want := range ok {
		got, err := dialNumber(in)
		if err != nil || got != want {
			t.Errorf("dialNumber(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "tel:", "tel:abc", "1+2", "https://example.com", "tel:123\n456", "+"} {
		if got, err := dialNumber(in); err == nil {
			t.Errorf("dialNumber(%q) = %q; want an error", in, got)
		}
	}
}
