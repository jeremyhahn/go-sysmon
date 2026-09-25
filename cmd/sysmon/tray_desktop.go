//go:build desktop && systray

package main

import (
	"bytes"
	_ "embed"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"log/slog"
	"sync/atomic"

	"fyne.io/systray"

	"github.com/jeremyhahn/go-sysmon/pkg/cli"
	"github.com/jeremyhahn/go-sysmon/pkg/monitor"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// trayState holds the live state of the system tray.
var trayState struct {
	// stopCh is closed to signal the tray goroutine to exit.
	stopCh chan struct{}
	// running guards against double-start.
	running atomic.Bool
}

// startSystray launches the system tray in a goroutine. It subscribes to the
// monitor for snapshot data and updates menu items in real-time.
// showWindow is called when "Open Monitor" is clicked.
// quit is called when "Quit" is clicked.
func startSystray(mon *monitor.Monitor, showWindow func(), quit func()) {
	if !trayState.running.CompareAndSwap(false, true) {
		return
	}

	trayState.stopCh = make(chan struct{})

	go func() {
		systray.Run(
			func() { onTrayReady(mon, showWindow, quit, trayState.stopCh) },
			func() { trayState.running.Store(false) },
		)
	}()
}

// stopSystray signals the tray to exit.
func stopSystray() {
	if !trayState.running.Load() {
		return
	}
	systray.Quit()
}

// trayRunning reports whether the tray icon is up, and so whether the window
// can be reopened from it.
func trayRunning() bool {
	return trayState.running.Load()
}

// onTrayReady is called by systray once the tray icon is initialised. It
// builds the menu, subscribes to the monitor, and drives the update loop.
func onTrayReady(mon *monitor.Monitor, showWindow func(), quit func(), stopCh <-chan struct{}) {
	systray.SetIcon(trayIcon(drawTrayIcon(), slog.Default()))
	systray.SetTooltip("System Monitor")

	menu := newTrayMenu()
	go menu.serve(mon, mon.Subscribe(), showWindow, quit, stopCh)
}

// trayMenu is the tray's menu: the read-only stat items, one item per GPU
// once a snapshot has reported them, and the two actions.
type trayMenu struct {
	cpu, ram, swap, load *systray.MenuItem
	gpus                 []*systray.MenuItem
	show, quit           *systray.MenuItem
}

// newTrayMenu adds the menu's items to the tray. The stat items are disabled,
// since there is nothing to do by clicking a reading, and read "--" until the
// first snapshot arrives.
func newTrayMenu() *trayMenu {
	stat := func(title, tooltip string) *systray.MenuItem {
		item := systray.AddMenuItem(title, tooltip)
		item.Disable()
		return item
	}
	m := &trayMenu{
		cpu:  stat("CPU:  --", "CPU usage"),
		ram:  stat("RAM:  --", "Memory usage"),
		swap: stat("Swap: --", "Swap usage"),
		load: stat("Load: --", "Load average (1m/5m/15m)"),
	}
	systray.AddSeparator()
	m.show = systray.AddMenuItem("Open Monitor", "Show the monitor window")
	systray.AddSeparator()
	m.quit = systray.AddMenuItem("Quit", "Quit the application")
	return m
}

// serve keeps the menu current from sub and answers its actions, until
// stopCh is closed, the subscription ends, or Quit is clicked. It
// unsubscribes from mon when it returns.
func (m *trayMenu) serve(mon *monitor.Monitor, sub *monitor.Subscriber, showWindow, quit func(), stopCh <-chan struct{}) {
	defer mon.Unsubscribe(sub.ID)
	for {
		select {
		case <-stopCh:
			return

		case <-sub.Done:
			return

		case snap, ok := <-sub.Ch:
			if !ok {
				return
			}
			m.update(snap)

		case <-m.show.ClickedCh:
			showWindow()

		case <-m.quit.ClickedCh:
			quit()
			return
		}
	}
}

// trayMenuTitles holds the title of every stat item in the tray menu for one
// snapshot.
type trayMenuTitles struct {
	CPU  string
	RAM  string
	Swap string
	Load string
	// GPUs has one title per GPU in the snapshot, in snapshot order.
	GPUs []string
}

// newTrayMenuTitles formats snap into the tray menu's item titles, using the
// same byte and percentage formatting as the CLI.
func newTrayMenuTitles(snap *types.Snapshot) trayMenuTitles {
	titles := trayMenuTitles{
		CPU: "CPU:  " + cli.FormatMeanCPUUsage(snap.CPUs),
		RAM: fmt.Sprintf(
			"RAM:  %s / %s (%s)",
			cli.FormatBytes(snap.Memory.UsedBytes),
			cli.FormatBytes(snap.Memory.TotalBytes),
			cli.FormatPercent(snap.Memory.UsedPercent),
		),
		Swap: fmt.Sprintf(
			"Swap: %s / %s (%s)",
			cli.FormatBytes(snap.Memory.SwapUsedBytes),
			cli.FormatBytes(snap.Memory.SwapTotalBytes),
			cli.FormatPercent(snap.Memory.SwapPercent),
		),
		Load: fmt.Sprintf(
			"Load: %.2f / %.2f / %.2f",
			snap.LoadAvg.Load1,
			snap.LoadAvg.Load5,
			snap.LoadAvg.Load15,
		),
		GPUs: make([]string, len(snap.GPUs)),
	}
	for i, gpu := range snap.GPUs {
		titles.GPUs[i] = fmt.Sprintf(
			"GPU%d: %s  mem %s",
			i,
			cli.FormatPercent(gpu.GPUUtilPercent),
			cli.FormatPercent(gpu.MemoryPercent),
		)
	}
	return titles
}

// update refreshes the title of each stat item from snap. A GPU item is
// added the first time a snapshot reports that many GPUs, and updated in
// place after that.
func (m *trayMenu) update(snap *types.Snapshot) {
	titles := newTrayMenuTitles(snap)
	m.cpu.SetTitle(titles.CPU)
	m.ram.SetTitle(titles.RAM)
	m.swap.SetTitle(titles.Swap)
	m.load.SetTitle(titles.Load)

	for i := len(m.gpus); i < len(titles.GPUs); i++ {
		item := systray.AddMenuItem(fmt.Sprintf("GPU%d: --", i), fmt.Sprintf("GPU %d usage", i))
		item.Disable()
		m.gpus = append(m.gpus, item)
	}
	for i, title := range titles.GPUs {
		m.gpus[i].SetTitle(title)
	}
}

// fallbackTrayIcon is the tray icon shipped as a file, shown when the drawn
// icon cannot be encoded. It is the PNG encoding of drawTrayIcon, pinned by a
// test so the two cannot drift apart.
//
//go:embed trayicon.png
var fallbackTrayIcon []byte

// trayIcon returns img encoded as the tray's PNG icon. An encode failure is
// logged and answered with fallbackTrayIcon, so the tray never shows a blank
// or truncated icon.
func trayIcon(img image.Image, logger *slog.Logger) []byte {
	icon, err := encodeTrayIcon(img)
	if err != nil {
		logger.Warn("tray icon: showing the embedded fallback", "error", err)
		return fallbackTrayIcon
	}
	return icon
}

// encodeTrayIcon encodes img as a PNG. It returns a TrayIconEncodeError when
// the encoder rejects the image.
func encodeTrayIcon(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, &types.TrayIconEncodeError{Cause: err}
	}
	return buf.Bytes(), nil
}

// drawTrayIcon draws the 22x22 tray icon: a green monitor outline with a grey
// stand, recognisable at small sizes.
func drawTrayIcon() image.Image {
	const size = 22
	img := image.NewRGBA(image.Rect(0, 0, size, size))

	green := color.RGBA{0x36, 0xd3, 0x99, 0xff}
	grey := color.RGBA{0x80, 0x80, 0x80, 0xff}
	dark := color.RGBA{0x1a, 0x8a, 0x60, 0xff}

	// Monitor screen area: rows 2-14, cols 2-19.
	for y := 2; y <= 14; y++ {
		for x := 2; x <= 19; x++ {
			c := green
			// Draw a 1-pixel dark border.
			if y == 2 || y == 14 || x == 2 || x == 19 {
				c = dark
			}
			img.Set(x, y, c)
		}
	}

	// Neck: rows 15-16, cols 10-11.
	for y := 15; y <= 16; y++ {
		img.Set(10, y, grey)
		img.Set(11, y, grey)
	}

	// Base: row 17, cols 7-14.
	for x := 7; x <= 14; x++ {
		img.Set(x, 17, grey)
	}

	return img
}
