//go:build !desktop

package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/jeremyhahn/go-sysmon/pkg/collector"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// These test what gui_stub.go does, so they carry its constraint. In a desktop
// build launchGUI opens a window, and runRoot with a display would too.
// Everything the desktop and server builds share is tested in files with no
// constraint, which every shape compiles.

// TestExplainNoGUI_NamesMissingLibraries verifies the fallback notice tells a
// user exactly what to install rather than only that a GUI is unavailable.
func TestExplainNoGUI_NamesMissingLibraries(t *testing.T) {
	withMissingLibs(t, []collector.DesktopLib{
		{SOName: "libgtk-3.so.0", Debian: "libgtk-3-0", Fedora: "gtk3", Arch: "gtk3"},
	})

	var buf bytes.Buffer
	explainNoGUI(&buf)

	out := buf.String()
	for _, want := range []string{"No GUI in this build", "sysmon serve", "libgtk-3.so.0"} {
		if !strings.Contains(out, want) {
			t.Errorf("notice missing %q; got:\n%s", want, out)
		}
	}
}

// TestExplainNoGUI_LibsPresentRecommendsDesktopBuild covers the other branch:
// the libraries are there, so the answer is to build the desktop binary.
func TestExplainNoGUI_LibsPresentRecommendsDesktopBuild(t *testing.T) {
	withMissingLibs(t, nil)

	var buf bytes.Buffer
	explainNoGUI(&buf)

	out := buf.String()
	if !strings.Contains(out, "make build-desktop") {
		t.Errorf("notice does not point at the desktop build; got:\n%s", out)
	}
}

// TestLaunchGUI_ReportsUnavailable verifies the non-desktop build reports the
// typed error that runRoot uses to fall through to the CLI.
func TestLaunchGUI_ReportsUnavailable(t *testing.T) {
	err := launchGUI()
	if err == nil {
		t.Fatal("launchGUI() = nil, want a GUIUnavailableError in a non-desktop build")
	}
	var guiErr *types.GUIUnavailableError
	if !errors.As(err, &guiErr) {
		t.Errorf("launchGUI() error = %T (%v), want *types.GUIUnavailableError", err, err)
	}
}

// TestRunRoot_WithDisplayFallsBackWhenGUIUnavailable verifies that in a
// non-desktop build a detected display still falls through to the overview,
// because launchGUI reports GUIUnavailableError.
func TestRunRoot_WithDisplayFallsBackWhenGUIUnavailable(t *testing.T) {
	t.Setenv("DISPLAY", ":0")
	resetGlobals(t)
	withFixtureSnapshot(t, fixtureSnapshot(), nil)

	var buf bytes.Buffer
	cmd := newTestCmd(&buf)

	if err := runRoot(cmd, nil); err != nil {
		t.Fatalf("runRoot() error = %v, want nil", err)
	}
	if !bytes.Contains(buf.Bytes(), []byte("fixture-host")) {
		t.Errorf("runRoot() did not fall back to the overview; got:\n%s", buf.String())
	}
}
