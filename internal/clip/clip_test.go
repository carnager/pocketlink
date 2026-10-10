package clip

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
)

func TestToPNG(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 8, 6))
	src.Set(3, 2, color.RGBA{200, 10, 10, 255})
	var j bytes.Buffer
	if err := jpeg.Encode(&j, src, nil); err != nil {
		t.Fatal(err)
	}

	out, err := toPNG("image/jpeg", j.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("result is not a PNG: %v", err)
	}
	if img.Bounds() != src.Bounds() {
		t.Errorf("size changed: %v, want %v", img.Bounds(), src.Bounds())
	}
	if _, ok := img.(*image.RGBA64); ok {
		t.Error("JPEG became a 16-bit PNG")
	}

	same, _ := toPNG("image/png", out)
	if !bytes.Equal(same, out) {
		t.Error("PNG input was re-encoded")
	}
	if _, err := toPNG("image/jpeg", []byte("not an image")); err == nil {
		t.Error("garbage was accepted")
	}
}

func TestImageType(t *testing.T) {
	for _, tc := range []struct {
		types []string
		want  string
	}{
		{[]string{"image/png"}, "image/png"},
		{[]string{"image/jpeg"}, "image/jpeg"},
		{[]string{"text/html", "image/png"}, "image/png"},       // browser "copy image"
		{[]string{"text/plain;charset=utf-8", "image/png"}, ""}, // text wins
		{[]string{"UTF8_STRING", "TEXT"}, ""},
	} {
		if got := ImageType(tc.types); got != tc.want {
			t.Errorf("ImageType(%v) = %q, want %q", tc.types, got, tc.want)
		}
	}
}
