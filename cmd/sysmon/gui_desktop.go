//go:build desktop

package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/jeremyhahn/go-sysmon/pkg/collector"
	"github.com/jeremyhahn/go-sysmon/pkg/monitor"
	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// hasDesktopGUI reports whether this build has a GUI compiled in.
func hasDesktopGUI() bool { return true }

// launchGUI creates a Wails desktop application, wires up the monitor and
// bindings, and runs the event loop. It blocks until the window is closed.
func launchGUI() error {
	sc := collector.NewSystemCollector(slog.Default())
	snap := newMonitorSnapshotter(sc)

	mon, err := monitor.New(snap, defaultInterval)
	if err != nil {
		slog.Error("monitor create", "error", err)
		os.Exit(1)
	}

	binding := NewMonitorBinding(mon, sc)

	// wailsCtx is stored once the Wails runtime calls OnStartup so that the
	// tray callbacks can reference it.
	var wailsCtx context.Context

	app := &options.App{
		Title:  "System Monitor",
		Width:  1400,
		Height: 900,
		AssetServer: &assetserver.Options{
			Assets: frontendAssets,
		},
		OnStartup: func(ctx context.Context) {
			wailsCtx = ctx
			binding.startup(ctx)

			if !noTray {
				startSystray(
					mon,
					func() { wailsruntime.WindowShow(wailsCtx) },
					func() { wailsruntime.Quit(wailsCtx) },
				)
			}
		},
		OnShutdown: func(ctx context.Context) {
			stopSystray()
			binding.shutdown(ctx)
		},
		// When the tray is active, closing the window hides it instead of
		// terminating the application so that the user can reopen it from
		// the tray.
		OnBeforeClose: func(ctx context.Context) (prevent bool) {
			if !noTray {
				wailsruntime.WindowHide(ctx)
				return true
			}
			return false
		},
		Bind: []interface{}{
			binding,
		},
	}

	return wails.Run(app)
}
