//go:build desktop

package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"sync/atomic"

	"fyne.io/systray"

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

// onTrayReady is called by systray once the tray icon is initialised. It
// builds the menu, subscribes to the monitor, and drives the update loop.
func onTrayReady(mon *monitor.Monitor, showWindow func(), quit func(), stopCh <-chan struct{}) {
	systray.SetIcon(generateTrayIcon())
	systray.SetTooltip("System Monitor")

	// Stat display items (read-only).
	itemCPU := systray.AddMenuItem("CPU:  --", "CPU usage")
	itemCPU.Disable()

	itemRAM := systray.AddMenuItem("RAM:  --", "Memory usage")
	itemRAM.Disable()

	itemSwap := systray.AddMenuItem("Swap: --", "Swap usage")
	itemSwap.Disable()

	itemLoad := systray.AddMenuItem("Load: --", "Load average (1m/5m/15m)")
	itemLoad.Disable()

	// GPU items are added dynamically on first snapshot; we track them here.
	gpuItems := make([]*systray.MenuItem, 0)

	systray.AddSeparator()
	mShow := systray.AddMenuItem("Open Monitor", "Show the monitor window")
	systray.AddSeparator()
	mQuit := systray.AddMenuItem("Quit", "Quit the application")

	sub := mon.Subscribe()

	go func() {
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
				updateTrayItems(snap, itemCPU, itemRAM, itemSwap, itemLoad, &gpuItems)

			case <-mShow.ClickedCh:
				showWindow()

			case <-mQuit.ClickedCh:
				quit()
				return
			}
		}
	}()
}

// updateTrayItems refreshes the title of each stat menu item from snap.
// gpuItems is grown on the first snapshot that contains GPUs; subsequent
// calls update the existing items in-place.
func updateTrayItems(
	snap *types.Snapshot,
	itemCPU, itemRAM, itemSwap, itemLoad *systray.MenuItem,
	gpuItems *[]*systray.MenuItem,
) {
	// Aggregate CPU usage across all cores.
	var totalCPU float64
	if len(snap.CPUs) > 0 {
		for i := range snap.CPUs {
			totalCPU += snap.CPUs[i].UsagePercent
		}
		totalCPU /= float64(len(snap.CPUs))
	}
	itemCPU.SetTitle(fmt.Sprintf("CPU:  %s", fmtPercent(totalCPU)))

	itemRAM.SetTitle(fmt.Sprintf(
		"RAM:  %s / %s (%s)",
		fmtBytes(snap.Memory.UsedBytes),
		fmtBytes(snap.Memory.TotalBytes),
		fmtPercent(snap.Memory.UsedPercent),
	))

	itemSwap.SetTitle(fmt.Sprintf(
		"Swap: %s / %s (%s)",
		fmtBytes(snap.Memory.SwapUsedBytes),
		fmtBytes(snap.Memory.SwapTotalBytes),
		fmtPercent(snap.Memory.SwapPercent),
	))

	itemLoad.SetTitle(fmt.Sprintf(
		"Load: %.2f / %.2f / %.2f",
		snap.LoadAvg.Load1,
		snap.LoadAvg.Load5,
		snap.LoadAvg.Load15,
	))

	// Grow GPU item slice to match the number of GPUs in this snapshot.
	for i := len(*gpuItems); i < len(snap.GPUs); i++ {
		item := systray.AddMenuItem(
			fmt.Sprintf("GPU%d: --", i),
			fmt.Sprintf("GPU %d usage", i),
		)
		item.Disable()
		*gpuItems = append(*gpuItems, item)
	}

	for i, gpu := range snap.GPUs {
		if i >= len(*gpuItems) {
			break
		}
		(*gpuItems)[i].SetTitle(fmt.Sprintf(
			"GPU%d: %s  mem %s",
			i,
			fmtPercent(gpu.GPUUtilPercent),
			fmtPercent(gpu.MemoryPercent),
		))
	}
}

// fmtPercent formats a float64 as a one-decimal-place percentage string.
func fmtPercent(v float64) string {
	return fmt.Sprintf("%.1f%%", v)
}

// fmtBytes formats a byte count as a human-readable string.
func fmtBytes(n uint64) string {
	const (
		kb = 1024
		mb = kb * 1024
		gb = mb * 1024
		tb = gb * 1024
	)
	switch {
	case n == 0:
		return "0 B"
	case n >= tb:
		return fmt.Sprintf("%.1f TB", float64(n)/float64(tb))
	case n >= gb:
		return fmt.Sprintf("%.1f GB", float64(n)/float64(gb))
	case n >= mb:
		return fmt.Sprintf("%.1f MB", float64(n)/float64(mb))
	case n >= kb:
		return fmt.Sprintf("%.1f KB", float64(n)/float64(kb))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// generateTrayIcon produces a minimal 22x22 PNG icon rendered entirely in Go.
// It draws a green monitor outline with a grey stand so it is recognisable at
// small sizes without needing an external asset file.
func generateTrayIcon() []byte {
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

	var buf bytes.Buffer
	// png.Encode only returns an error for nil images; the buffer write path
	// is always healthy, so the error is not actionable here.
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}
