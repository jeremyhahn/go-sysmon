//go:build !desktop || !systray

package main

import "github.com/jeremyhahn/go-sysmon/pkg/monitor"

// The tray is compiled only into a desktop build that carries the systray tag
// (make build-desktop, unless WITH_SYSTRAY=0). Every other build gets these.

// startSystray is a no-op without the tray. It exists so gui_desktop.go
// compiles against a single signature.
func startSystray(_ *monitor.Monitor, _ func(), _ func()) {} //nolint:unused

// stopSystray is a no-op without the tray, for the same reason.
func stopSystray() {} //nolint:unused

// trayRunning reports false: there is no tray to reopen the window from.
func trayRunning() bool { return false } //nolint:unused
