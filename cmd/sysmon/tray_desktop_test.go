//go:build desktop && systray

package main

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/png"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// TestNewTrayMenuTitles pins the tray's menu text: the CPU average across
// cores, the memory, swap and load lines, and one line per GPU, formatted with
// the CLI's byte and percentage formatters.
func TestNewTrayMenuTitles(t *testing.T) {
	snap := &types.Snapshot{
		CPUs: []types.CPUInfo{{UsagePercent: 10, UsageAvailable: true}, {UsagePercent: 35, UsageAvailable: true}},
		Memory: types.MemoryInfo{
			UsedBytes:      1536 << 20,
			TotalBytes:     16 << 30,
			UsedPercent:    9.375,
			SwapUsedBytes:  0,
			SwapTotalBytes: 1023,
			SwapPercent:    0,
		},
		LoadAvg: types.LoadAverage{Load1: 0.5, Load5: 1.25, Load15: 2},
		GPUs: []types.GPUInfo{
			{GPUUtilPercent: 42.26, MemoryPercent: 100},
			{GPUUtilPercent: 0, MemoryPercent: 3.14},
		},
	}

	got := newTrayMenuTitles(snap)
	want := trayMenuTitles{
		CPU:  "CPU:  22.5%",
		RAM:  "RAM:  1.5 GB / 16.0 GB (9.4%)",
		Swap: "Swap: 0 B / 1023 B (0.0%)",
		Load: "Load: 0.50 / 1.25 / 2.00",
		GPUs: []string{"GPU0: 42.3%  mem 100.0%", "GPU1: 0.0%  mem 3.1%"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("newTrayMenuTitles() =\n%#v\nwant\n%#v", got, want)
	}
}

// TestNewTrayMenuTitles_EmptySnapshot verifies a snapshot with no CPUs and no
// GPUs -- the first one on a host whose collectors have not reported yet --
// reads CPU as unknown rather than dividing by zero or claiming 0%, and adds no
// GPU lines.
func TestNewTrayMenuTitles_EmptySnapshot(t *testing.T) {
	got := newTrayMenuTitles(&types.Snapshot{})
	if got.CPU != "CPU:  -" {
		t.Errorf("CPU = %q, want %q", got.CPU, "CPU:  -")
	}
	if got.RAM != "RAM:  0 B / 0 B (0.0%)" {
		t.Errorf("RAM = %q, want %q", got.RAM, "RAM:  0 B / 0 B (0.0%)")
	}
	if len(got.GPUs) != 0 {
		t.Errorf("GPUs = %q, want none", got.GPUs)
	}
}

// TestNewTrayMenuTitles_UnmeasuredCPU verifies the tray reads CPU as unknown on
// the monitor's first collection, when no core's usage has been measured, and
// averages only the measured cores after that.
func TestNewTrayMenuTitles_UnmeasuredCPU(t *testing.T) {
	first := newTrayMenuTitles(&types.Snapshot{CPUs: []types.CPUInfo{{}, {}}})
	if first.CPU != "CPU:  -" {
		t.Errorf("no core measured: CPU = %q, want %q", first.CPU, "CPU:  -")
	}
	partial := newTrayMenuTitles(&types.Snapshot{CPUs: []types.CPUInfo{{UsagePercent: 30, UsageAvailable: true}, {}}})
	if partial.CPU != "CPU:  30.0%" {
		t.Errorf("one core measured: CPU = %q, want %q", partial.CPU, "CPU:  30.0%")
	}
}

// decodeIcon decodes a PNG tray icon, failing the test if it is not one.
func decodeIcon(t *testing.T, icon []byte) image.Image {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(icon))
	if err != nil {
		t.Fatalf("decode tray icon: %v", err)
	}
	return img
}

// TestDrawTrayIcon verifies the icon is 22x22 with the monitor drawn where the
// tray expects it and the corners left transparent.
func TestDrawTrayIcon(t *testing.T) {
	img := drawTrayIcon()
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

// TestFallbackTrayIcon_MatchesDrawnIcon verifies the embedded fallback is the
// drawn icon pixel for pixel, so an encode failure changes nothing the user
// sees and a redrawn icon cannot ship with a stale fallback.
func TestFallbackTrayIcon_MatchesDrawnIcon(t *testing.T) {
	fallback := decodeIcon(t, fallbackTrayIcon)
	drawn := drawTrayIcon()
	if fallback.Bounds() != drawn.Bounds() {
		t.Fatalf("fallback bounds = %v, drawn = %v", fallback.Bounds(), drawn.Bounds())
	}
	b := drawn.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			got := color.RGBAModel.Convert(fallback.At(x, y))
			want := color.RGBAModel.Convert(drawn.At(x, y))
			if got != want {
				t.Fatalf("fallback pixel (%d,%d) = %v, drawn = %v", x, y, got, want)
			}
		}
	}
}

// TestEncodeTrayIcon verifies the drawn icon encodes to a PNG that decodes back
// to the same size.
func TestEncodeTrayIcon(t *testing.T) {
	icon, err := encodeTrayIcon(drawTrayIcon())
	if err != nil {
		t.Fatalf("encodeTrayIcon() error = %v", err)
	}
	if b := decodeIcon(t, icon).Bounds(); b.Dx() != 22 || b.Dy() != 22 {
		t.Errorf("encoded icon is %dx%d, want 22x22", b.Dx(), b.Dy())
	}
}

// TestEncodeTrayIcon_RejectedImage verifies an image the PNG encoder refuses --
// one with no pixels -- is reported as a TrayIconEncodeError that carries the
// encoder's error.
func TestEncodeTrayIcon_RejectedImage(t *testing.T) {
	icon, err := encodeTrayIcon(image.NewRGBA(image.Rectangle{}))
	var encErr *types.TrayIconEncodeError
	if !errors.As(err, &encErr) {
		t.Fatalf("encodeTrayIcon() error = %T (%v), want *types.TrayIconEncodeError", err, err)
	}
	if encErr.Cause == nil {
		t.Error("TrayIconEncodeError.Cause = nil, want the encoder's error")
	}
	if icon != nil {
		t.Errorf("encodeTrayIcon() icon = %d bytes, want nil on error", len(icon))
	}
}

// TestTrayIcon_Encodes verifies a drawable icon is used as drawn and nothing
// is logged.
func TestTrayIcon_Encodes(t *testing.T) {
	var logs bytes.Buffer
	icon := trayIcon(drawTrayIcon(), slog.New(slog.NewTextHandler(&logs, nil)))

	want, err := encodeTrayIcon(drawTrayIcon())
	if err != nil {
		t.Fatalf("encodeTrayIcon() error = %v", err)
	}
	if !bytes.Equal(icon, want) {
		t.Error("trayIcon() did not return the encoded drawn icon")
	}
	if logs.Len() != 0 {
		t.Errorf("trayIcon() logged %q, want nothing", logs.String())
	}
}

// TestTrayIcon_EncodeFailureFallsBack verifies an icon that cannot be encoded
// is replaced by the embedded fallback and the failure is logged with its
// cause, rather than the tray showing a blank icon with no signal.
func TestTrayIcon_EncodeFailureFallsBack(t *testing.T) {
	var logs bytes.Buffer
	icon := trayIcon(image.NewRGBA(image.Rectangle{}), slog.New(slog.NewTextHandler(&logs, nil)))

	if !bytes.Equal(icon, fallbackTrayIcon) {
		t.Error("trayIcon() did not return the embedded fallback icon")
	}
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "tray icon encode") {
		t.Errorf("trayIcon() logged %q, want a warning carrying the encode error", logs.String())
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
