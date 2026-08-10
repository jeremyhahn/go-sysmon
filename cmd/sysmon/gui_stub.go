//go:build !desktop

package main

import (
	"fmt"
	"io"
	"os"

	"github.com/jeremyhahn/go-sysmon/pkg/collector"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// hasDesktopGUI reports whether this build has a GUI compiled in.
func hasDesktopGUI() bool { return false }

// launchGUI returns a GUIUnavailableError because this binary was not compiled
// with the "desktop" build tag and therefore has no Wails runtime linked in.
//
// Before falling back to the CLI, it explains the situation: a user who ran
// this expecting a window otherwise gets a wall of metrics with no indication
// that a GUI was ever possible.
func launchGUI() error {
	explainNoGUI(os.Stderr)
	return &types.GUIUnavailableError{}
}

// explainNoGUI writes the reason the GUI did not open, and the ways to get one.
func explainNoGUI(w io.Writer) {
	fmt.Fprintln(w, "No GUI in this build: it is the server binary, which omits the")
	fmt.Fprintln(w, "GTK and WebKit dependencies so it can run on headless machines.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  For the same dashboard in a browser:  sysmon serve --addr :8080")

	// Only recommend the desktop build when its libraries are actually here;
	// otherwise say what is missing so the advice is actionable.
	missing := missingDesktopLibs()
	if len(missing) == 0 {
		fmt.Fprintln(w, "  For a native window:                  build the desktop binary")
		fmt.Fprintln(w, "                                        (make build-desktop)")
	} else {
		names := make([]string, 0, len(missing))
		for _, lib := range missing {
			names = append(names, lib.SOName)
		}
		fmt.Fprintf(w, "  A native window also needs: %v\n", names)
		if hint := collector.InstallHint(missing); hint != "" {
			fmt.Fprintf(w, "    %s\n", hint)
		}
	}
	fmt.Fprintln(w)
}
