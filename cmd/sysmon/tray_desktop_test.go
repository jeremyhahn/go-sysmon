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
	"strconv"
	"strings"
	"testing"
	"time"

	"fyne.io/systray"
	"github.com/jeremyhahn/go-sysmon/internal/await"
	"github.com/jeremyhahn/go-sysmon/pkg/monitor"
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

// title reads a menu item's title through the only accessor systray offers,
// its String form: MenuItem[<id>, "<title>"].
func title(t *testing.T, item *systray.MenuItem) string {
	t.Helper()
	s := item.String()
	start := strings.Index(s, `"`)
	if start < 0 || !strings.HasSuffix(s, `"]`) {
		t.Fatalf("unexpected MenuItem form %q", s)
	}
	unquoted, err := strconv.Unquote(s[start : len(s)-1])
	if err != nil {
		t.Fatalf("unquote %q: %v", s, err)
	}
	return unquoted
}

// TestNewTrayMenu_StartsWithPlaceholders verifies a fresh menu shows "--"
// readings that cannot be clicked, and actions that can. systray keeps the
// menu in memory and only emits it over D-Bus when a session is connected, so
// this runs without one.
func TestNewTrayMenu_StartsWithPlaceholders(t *testing.T) {
	m := newTrayMenu()

	for want, item := range map[string]*systray.MenuItem{
		"CPU:  --": m.cpu, "RAM:  --": m.ram, "Swap: --": m.swap, "Load: --": m.load,
	} {
		if got := title(t, item); got != want {
			t.Errorf("title = %q, want %q", got, want)
		}
		if !item.Disabled() {
			t.Errorf("%q is clickable, want a read-only reading", want)
		}
	}
	if m.show.Disabled() || m.quit.Disabled() {
		t.Error("an action item is disabled")
	}
	if len(m.gpus) != 0 {
		t.Errorf("a new menu has %d GPU items, want none before a snapshot", len(m.gpus))
	}
}

// TestTrayMenuUpdate_AddsGPUItemsOnceAndReusesThem verifies GPU lines appear
// with the first snapshot that reports the GPUs, are rewritten in place after
// that, and grow when more GPUs appear -- never duplicated.
func TestTrayMenuUpdate_AddsGPUItemsOnceAndReusesThem(t *testing.T) {
	m := newTrayMenu()
	oneGPU := &types.Snapshot{GPUs: []types.GPUInfo{{GPUUtilPercent: 10, MemoryPercent: 20}}}

	m.update(oneGPU)
	first := m.gpus
	if len(first) != 1 || title(t, first[0]) != "GPU0: 10.0%  mem 20.0%" {
		t.Fatalf("after one GPU: %d items, want one titled with its usage", len(first))
	}
	if got := title(t, m.load); got != "Load: 0.00 / 0.00 / 0.00" {
		t.Errorf("load = %q, want the snapshot's load", got)
	}

	oneGPU.GPUs[0].GPUUtilPercent = 55
	m.update(oneGPU)
	if len(m.gpus) != 1 || m.gpus[0] != first[0] {
		t.Fatal("a second snapshot with the same GPUs added items instead of reusing them")
	}
	if got := title(t, m.gpus[0]); got != "GPU0: 55.0%  mem 20.0%" {
		t.Errorf("GPU0 = %q, want it rewritten in place", got)
	}

	m.update(&types.Snapshot{GPUs: []types.GPUInfo{{}, {GPUUtilPercent: 1}}})
	if len(m.gpus) != 2 || m.gpus[0] != first[0] || !m.gpus[1].Disabled() {
		t.Fatalf("a newly reported GPU did not get one new read-only item: %d items", len(m.gpus))
	}
}

// serveMenu runs menu.serve against a subscription of an unstarted monitor,
// whose channel the test feeds directly. done is closed when serve returns.
func serveMenu(t *testing.T, stopCh <-chan struct{}) (m *trayMenu, sub *monitor.Subscriber, shown <-chan struct{}, quit <-chan struct{}, done <-chan struct{}) {
	t.Helper()
	mon, err := monitor.New(serveStub{}, time.Second)
	if err != nil {
		t.Fatalf("monitor.New: %v", err)
	}
	m = newTrayMenu()
	sub = mon.Subscribe()
	// Unbuffered, where the monitor's is buffered: a send completes only when
	// serve has taken the snapshot, so a click sent after it is handled after
	// the update. The monitor is not started and never sends on it.
	sub.Ch = make(chan *types.Snapshot)
	showCh, quitCh, doneCh := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(doneCh)
		m.serve(mon, sub,
			func() { showCh <- struct{}{} },
			func() { close(quitCh) },
			stopCh)
	}()
	return m, sub, showCh, quitCh, doneCh
}

// TestTrayMenuServe_UpdatesAndOpensTheWindow verifies a snapshot rewrites the
// menu and "Open Monitor" shows the window, while the loop keeps running.
func TestTrayMenuServe_UpdatesAndOpensTheWindow(t *testing.T) {
	stop := make(chan struct{})
	m, sub, shown, _, done := serveMenu(t, stop)

	await.Send(t, "a snapshot to reach the tray", sub.Ch, &types.Snapshot{LoadAvg: types.LoadAverage{Load1: 1.5}})
	// serve handles one channel at a time and took the snapshot before the
	// click could be sent, so once the window is shown the update is done.
	await.Send(t, "the Open Monitor click to be taken", m.show.ClickedCh, struct{}{})
	await.Recv(t, "the window to be shown", shown)
	if got := title(t, m.load); got != "Load: 1.50 / 0.00 / 0.00" {
		t.Errorf("load = %q, want the snapshot's reading", got)
	}

	close(stop)
	await.Recv(t, "the tray loop to stop", done)
	await.Recv(t, "the tray to unsubscribe", sub.Done)
}

// TestTrayMenuServe_EndsOnEachStop verifies every way the loop ends -- Quit,
// the tray stopping, the subscription ending, the channel closing -- returns
// and leaves the monitor unsubscribed; Quit also quits the application.
func TestTrayMenuServe_EndsOnEachStop(t *testing.T) {
	tests := map[string]func(t *testing.T, m *trayMenu, sub *monitor.Subscriber, stop chan struct{}){
		"quit clicked": func(t *testing.T, m *trayMenu, _ *monitor.Subscriber, _ chan struct{}) {
			await.Send(t, "the Quit click to be taken", m.quit.ClickedCh, struct{}{})
		},
		"tray stopped":        func(_ *testing.T, _ *trayMenu, _ *monitor.Subscriber, stop chan struct{}) { close(stop) },
		"subscription closed": func(_ *testing.T, _ *trayMenu, sub *monitor.Subscriber, _ chan struct{}) { close(sub.Ch) },
	}
	for name, end := range tests {
		t.Run(name, func(t *testing.T) {
			stop := make(chan struct{})
			m, sub, _, quit, done := serveMenu(t, stop)

			end(t, m, sub, stop)
			await.Recv(t, "the tray loop to return", done)
			await.Recv(t, "the tray to unsubscribe", sub.Done)

			select {
			case <-quit:
				if name != "quit clicked" {
					t.Error("the application was quit by something other than Quit")
				}
			default:
				if name == "quit clicked" {
					t.Error("Quit did not quit the application")
				}
			}
		})
	}
}
