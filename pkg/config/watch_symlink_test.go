package config_test

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/jeremyhahn/go-sysmon/internal/await"
)

// thirdConfig is a third distinct configuration, for tests that reload twice.
const thirdConfig = "alerts:\n  cpu_pct: 50\n  memory_pct: 51\n"

// configMapMount lays a directory out exactly as the kubelet projects a
// ConfigMap volume:
//
//	sysmon.yaml -> ..data/sysmon.yaml
//	..data      -> ..2026_09_24_12_00_00.000000001
//	..2026_09_24_12_00_00.000000001/sysmon.yaml
//
// The file the service is pointed at never changes; an update writes a new
// timestamped directory, swaps ..data onto it and removes the old one.
type configMapMount struct {
	dir     string
	current string
	serial  int
}

func newConfigMapMount(t *testing.T, body string) *configMapMount {
	t.Helper()
	m := &configMapMount{dir: t.TempDir()}
	m.update(t, body)
	if err := os.Symlink(filepath.Join("..data", "sysmon.yaml"), m.path()); err != nil {
		t.Fatal(err)
	}
	return m
}

// path is the config path the service is given: the mount's own entry.
func (m *configMapMount) path() string { return filepath.Join(m.dir, "sysmon.yaml") }

// update replays the kubelet's atomic update: the new contents are written in
// full into a fresh timestamped directory, ..data_tmp is pointed at it and
// renamed over ..data, and the previous directory is removed.
func (m *configMapMount) update(t *testing.T, body string) {
	t.Helper()
	m.serial++
	next := fmt.Sprintf("..2026_09_24_12_00_00.%09d", m.serial)
	if err := os.Mkdir(filepath.Join(m.dir, next), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(m.dir, next, "sysmon.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(next, filepath.Join(m.dir, "..data_tmp")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(m.dir, "..data_tmp"), filepath.Join(m.dir, "..data")); err != nil {
		t.Fatal(err)
	}
	if m.current != "" {
		if err := os.RemoveAll(filepath.Join(m.dir, m.current)); err != nil {
			t.Fatal(err)
		}
	}
	m.current = next
}

// awaitReload waits for the next published configuration and checks it.
func (w *watched) awaitReload(t *testing.T, what string, want alertValues) {
	t.Helper()
	got := await.Recv(t, what, w.reloads)
	w.received++
	if got != want {
		t.Fatalf("%s: published %v, want %v", what, got, want)
	}
}

// finish stops the watcher and fails the test if anything was published, or
// any warning logged, beyond the reloads the test already received.
func (w *watched) finish(t *testing.T) {
	t.Helper()
	for _, r := range w.stop(t) {
		if r.Level > slog.LevelInfo || (r.Level == slog.LevelInfo && r.Message != msgReloaded) {
			t.Errorf("the watcher logged %s %q, want only the reloads", r.Level, r.Message)
		}
	}
	close(w.reloads)
	for extra := range w.reloads {
		t.Errorf("an unexpected further reload published %v", extra)
	}
}

// TestWatch_ConfigMapUpdateReloads is the #227 case: a service pointed at a
// mounted ConfigMap. The update replaces ..data, not the file the service
// names, so a watcher that follows only that name never reloads. Two updates
// in a row prove the watch moved to each new directory, and that removing the
// old one is not itself mistaken for a change.
func TestWatch_ConfigMapUpdateReloads(t *testing.T) {
	m := newConfigMapMount(t, startingConfig)
	w := startWatchAt(t, m.path())

	m.update(t, fullConfig)
	w.awaitReload(t, "the reload after the first ConfigMap update", alertValues{70, 71})

	m.update(t, thirdConfig)
	w.awaitReload(t, "the reload after the second ConfigMap update", alertValues{50, 51})

	w.finish(t)
}

// TestWatch_ConfigMapTargetRemovedThenRestored covers the directory ..data
// points at disappearing without a swap. The config file is then gone, which
// runs on defaults exactly as a removed file does, and the next update is
// followed as usual.
func TestWatch_ConfigMapTargetRemovedThenRestored(t *testing.T) {
	m := newConfigMapMount(t, startingConfig)
	w := startWatchAt(t, m.path())

	if err := os.RemoveAll(filepath.Join(m.dir, m.current)); err != nil {
		t.Fatal(err)
	}
	w.awaitReload(t, "the reload after the target directory was removed", alertValues{90, 90})

	m.update(t, fullConfig)
	w.awaitReload(t, "the reload once ..data points somewhere real again", alertValues{70, 71})

	w.finish(t)
}

// TestWatch_SymlinkedDirectorySwapReloads covers one symlink on the path: a
// release directory reached through current -> v1, switched by renaming a new
// link over current. After the switch, writes to the file in the new directory
// are followed with the same close-write rule as any other file.
func TestWatch_SymlinkedDirectorySwapReloads(t *testing.T) {
	dir := t.TempDir()
	for version, body := range map[string]string{"v1": startingConfig, "v2": fullConfig} {
		if err := os.Mkdir(filepath.Join(dir, version), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, version, "sysmon.yaml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("v1", filepath.Join(dir, "current")); err != nil {
		t.Fatal(err)
	}
	w := startWatchAt(t, filepath.Join(dir, "current", "sysmon.yaml"))

	if err := os.Symlink("v2", filepath.Join(dir, "next")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, "next"), filepath.Join(dir, "current")); err != nil {
		t.Fatal(err)
	}
	w.awaitReload(t, "the reload after current was switched to v2", alertValues{70, 71})

	f := openInPlace(t, filepath.Join(dir, "v2", "sysmon.yaml"), os.O_TRUNC)
	writeString(t, f, "alerts:\n  cpu_pct: 50\n")
	w.awaitHeldAfterDecisions(t)
	writeString(t, f, "  memory_pct: 51\n")
	closeFile(t, f)
	w.awaitReload(t, "the reload of the file written in place in v2", alertValues{50, 51})

	w.finish(t)
}

// TestWatch_SymlinkToFileInAnotherDirectory covers a config path that is a
// symlink to a file kept elsewhere. Edits to that file happen in its own
// directory, where a watch on the link's directory sees nothing. They must be
// followed -- held while written, reloaded on close -- and so must the link
// being pointed at a different file.
func TestWatch_SymlinkToFileInAnotherDirectory(t *testing.T) {
	store := t.TempDir()
	real := filepath.Join(store, "real.yaml")
	if err := os.WriteFile(real, []byte(startingConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "sysmon.yaml")
	if err := os.Symlink(real, path); err != nil {
		t.Fatal(err)
	}
	w := startWatchAt(t, path)

	f := openInPlace(t, real, os.O_TRUNC)
	writeString(t, f, firstLine)
	w.awaitHeld(t)
	if got := w.live(); got != (alertValues{42, 43}) {
		t.Fatalf("while the target is half written Current() = %v, want the previous {42 43}", got)
	}
	writeString(t, f, restOfConfig)
	closeFile(t, f)
	w.awaitReload(t, "the reload of the edited target", alertValues{70, 71})

	other := filepath.Join(store, "other.yaml")
	if err := os.WriteFile(other, []byte(thirdConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(filepath.Dir(path), ".sysmon.yaml.new")
	if err := os.Symlink(other, link); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(link, path); err != nil {
		t.Fatal(err)
	}
	w.awaitReload(t, "the reload after the link was pointed at another file", alertValues{50, 51})

	w.finish(t)
}
