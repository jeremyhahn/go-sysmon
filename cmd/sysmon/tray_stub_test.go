//go:build !desktop || !systray

package main

import "testing"

// TestSystrayStubs_NoOp verifies the tray stubs are safe no-ops, including
// with nil callbacks, and that a build without the tray never reports one
// running: the desktop window hides on close only when it does, and with no
// tray there would be no way to bring it back.
func TestSystrayStubs_NoOp(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("systray stub panicked: %v", r)
		}
	}()
	startSystray(nil, nil, nil)
	if trayRunning() {
		t.Error("trayRunning() = true after startSystray in a build without the tray")
	}
	stopSystray()
}
