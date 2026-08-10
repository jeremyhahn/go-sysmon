//go:build !desktop

package main

import "github.com/jeremyhahn/go-sysmon/pkg/monitor"

// startSystray is a no-op on non-desktop builds.
// It exists so gui_desktop.go compiles against a single signature.
func startSystray(_ *monitor.Monitor, _ func(), _ func()) {} //nolint:unused

// stopSystray is a no-op on non-desktop builds.
// It exists so gui_desktop.go compiles against a single signature.
func stopSystray() {} //nolint:unused
