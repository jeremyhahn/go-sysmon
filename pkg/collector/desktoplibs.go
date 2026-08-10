package collector

import (
	"os"
	"path/filepath"
	"strings"
)

// DesktopLib describes a shared library the desktop build links against, and
// the package that provides it on each supported distribution family.
type DesktopLib struct {
	// SOName is the runtime name the dynamic linker looks for.
	SOName string
	// Purpose is a short description of what needs it.
	Purpose string
	// Debian, Fedora and Arch name the package providing the library.
	Debian string
	Fedora string
	Arch   string
	// Found is set by CheckDesktopLibs; Path is where it was located.
	Found bool
	Path  string
}

// desktopLibs are the libraries whose absence stops the desktop binary from
// starting. The dynamic linker resolves these before main() runs, so a binary
// linked against a missing one dies with a loader error and no Go code of ours
// ever executes — which is precisely why this check lives in a build that does
// not link them.
var desktopLibs = []DesktopLib{
	{
		SOName:  "libgtk-3.so.0",
		Purpose: "window toolkit",
		Debian:  "libgtk-3-0", Fedora: "gtk3", Arch: "gtk3",
	},
	{
		SOName:  "libwebkit2gtk-4.1.so.0",
		Purpose: "embedded browser engine that renders the UI",
		Debian:  "libwebkit2gtk-4.1-0", Fedora: "webkit2gtk4.1", Arch: "webkit2gtk-4.1",
	},
	{
		SOName:  "libsoup-3.0.so.0",
		Purpose: "HTTP stack used by the browser engine",
		Debian:  "libsoup-3.0-0", Fedora: "libsoup3", Arch: "libsoup3",
	},
	{
		SOName:  "libglib-2.0.so.0",
		Purpose: "core object system underneath GTK",
		Debian:  "libglib2.0-0", Fedora: "glib2", Arch: "glib2",
	},
}

// libSearchDirs returns the directories the dynamic linker searches, being the
// standard multiarch locations plus anything configured in ld.so.conf.d.
func libSearchDirs() []string {
	dirs := []string{
		"/lib/x86_64-linux-gnu",
		"/usr/lib/x86_64-linux-gnu",
		"/usr/local/lib/x86_64-linux-gnu",
		"/usr/lib64",
		"/usr/lib",
		"/lib",
		"/usr/local/lib",
	}

	// Additional paths configured by the distribution or by the administrator.
	confs, err := filepath.Glob("/etc/ld.so.conf.d/*.conf")
	if err != nil {
		return dirs
	}
	for _, conf := range confs {
		raw, readErr := os.ReadFile(conf)
		if readErr != nil {
			continue
		}
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "include") {
				continue
			}
			dirs = append(dirs, line)
		}
	}
	return dirs
}

// CheckDesktopLibs reports which desktop libraries are present. It is safe to
// call from any build: it only inspects the filesystem and never loads
// anything.
func CheckDesktopLibs() []DesktopLib {
	dirs := libSearchDirs()

	result := make([]DesktopLib, len(desktopLibs))
	copy(result, desktopLibs)

	for i := range result {
		for _, dir := range dirs {
			candidate := filepath.Join(dir, result[i].SOName)
			if _, err := os.Stat(candidate); err == nil {
				result[i].Found = true
				result[i].Path = candidate
				break
			}
		}
	}
	return result
}

// MissingDesktopLibs returns only the libraries that could not be found.
func MissingDesktopLibs() []DesktopLib {
	var missing []DesktopLib
	for _, lib := range CheckDesktopLibs() {
		if !lib.Found {
			missing = append(missing, lib)
		}
	}
	return missing
}

// InstallHint returns the command that installs the given libraries on the
// distribution family detected from /etc/os-release.
func InstallHint(libs []DesktopLib) string {
	if len(libs) == 0 {
		return ""
	}

	family := distroFamily()

	pkgs := make([]string, 0, len(libs))
	for _, lib := range libs {
		switch family {
		case "fedora":
			pkgs = append(pkgs, lib.Fedora)
		case "arch":
			pkgs = append(pkgs, lib.Arch)
		default:
			pkgs = append(pkgs, lib.Debian)
		}
	}
	joined := strings.Join(pkgs, " ")

	switch family {
	case "fedora":
		return "sudo dnf install " + joined
	case "arch":
		return "sudo pacman -S " + joined
	default:
		return "sudo apt install " + joined
	}
}

// osReleasePath identifies the running distribution. It is a variable so tests
// can exercise every packaging family, not just the host's.
var osReleasePath = "/etc/os-release"

// distroFamily identifies the packaging family from /etc/os-release, defaulting
// to Debian because that covers the most common targets.
func distroFamily() string {
	raw, err := os.ReadFile(osReleasePath)
	if err != nil {
		return "debian"
	}

	text := strings.ToLower(string(raw))
	switch {
	case strings.Contains(text, "fedora"), strings.Contains(text, "rhel"),
		strings.Contains(text, "centos"), strings.Contains(text, "rocky"):
		return "fedora"
	case strings.Contains(text, "arch"), strings.Contains(text, "manjaro"):
		return "arch"
	default:
		return "debian"
	}
}
