//go:build desktop && systray

package main

import (
	"bytes"
	"image/color"
	"image/png"
	"testing"
)

// TestFmtBytes verifies each unit boundary: a value just below a unit stays in
// the smaller one, and the unit's own size switches to it.
func TestFmtBytes(t *testing.T) {
	tests := []struct {
		n    uint64
		want string
	}{
		{0, "0 B"},
		{1, "1 B"},
		{1023, "1023 B"},
		{1024, "1.0 KB"},
		{1536, "1.5 KB"},
		{1 << 20, "1.0 MB"},
		{1 << 30, "1.0 GB"},
		{12 << 30, "12.0 GB"},
		{1 << 40, "1.0 TB"},
		{^uint64(0), "16777216.0 TB"},
	}
	for _, tt := range tests {
		if got := fmtBytes(tt.n); got != tt.want {
			t.Errorf("fmtBytes(%d) = %q, want %q", tt.n, got, tt.want)
		}
	}
}

// TestFmtPercent verifies one decimal place, including rounding and values the
// kernel can report outside 0-100 on an oversubscribed counter.
func TestFmtPercent(t *testing.T) {
	tests := []struct {
		v    float64
		want string
	}{
		{0, "0.0%"},
		{42.25, "42.2%"},
		{42.26, "42.3%"},
		{100, "100.0%"},
		{-1, "-1.0%"},
	}
	for _, tt := range tests {
		if got := fmtPercent(tt.v); got != tt.want {
			t.Errorf("fmtPercent(%v) = %q, want %q", tt.v, got, tt.want)
		}
	}
}

// TestGenerateTrayIcon verifies the icon is a decodable 22x22 PNG with the
// monitor drawn where the tray expects it and the corners left transparent.
func TestGenerateTrayIcon(t *testing.T) {
	img, err := png.Decode(bytes.NewReader(generateTrayIcon()))
	if err != nil {
		t.Fatalf("decode tray icon: %v", err)
	}
	if b := img.Bounds(); b.Dx() != 22 || b.Dy() != 22 {
		t.Fatalf("icon is %dx%d, want 22x22", b.Dx(), b.Dy())
	}

	pixels := []struct {
		name string
		x, y int
		want color.RGBA
	}{
		{"screen border", 2, 2, color.RGBA{0x1a, 0x8a, 0x60, 0xff}},
		{"screen", 10, 8, color.RGBA{0x36, 0xd3, 0x99, 0xff}},
		{"stand", 10, 15, color.RGBA{0x80, 0x80, 0x80, 0xff}},
		{"corner", 0, 0, color.RGBA{}},
	}
	for _, p := range pixels {
		if got := color.RGBAModel.Convert(img.At(p.x, p.y)).(color.RGBA); got != p.want {
			t.Errorf("%s pixel (%d,%d) = %v, want %v", p.name, p.x, p.y, got, p.want)
		}
	}
}

// TestTrayRunning_FalseBeforeStart verifies the window-close handler sees no
// tray until one is started, so closing the window before then quits rather
// than hiding it with nothing to bring it back.
func TestTrayRunning_FalseBeforeStart(t *testing.T) {
	if trayRunning() {
		t.Error("trayRunning() = true before startSystray")
	}
}
