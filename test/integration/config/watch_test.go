//go:build integration

package config

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
	"github.com/jeremyhahn/go-sysmon/test/integration/harness"
)

// watched starts a service that watches the config file at path, logging at
// debug so the watcher's own progress can be followed.
func watched(t *testing.T, path string) *harness.Server {
	t.Helper()
	return harness.Serve(t, harness.ServeOptions{ConfigPath: path, Args: []string{"--log-level", "debug"}})
}

// TestWatch_ReloadsAnEditedFile edits a watched file in place and by atomic
// rename: each change is live without a signal, and a boot-only change is
// named as needing a restart.
func TestWatch_ReloadsAnEditedFile(t *testing.T) {
	path := configDir(t, "server:\n  watch: true\nalerts:\n  memory_pct: 80\n")
	s := watched(t, path)

	rewrite(t, path, "server:\n  watch: true\nalerts:\n  memory_pct: 70\n")
	s.Logs.AwaitMsg(t, msgFileReloaded)
	viewOf(t, s).want(t, "alerts.memory_pct", "70", types.ConfigSourceFile)

	replace(t, path, "server:\n  watch: false\nalerts:\n  memory_pct: 60\n")
	restart := s.Logs.AwaitMsg(t, msgNeedsRestart)
	if got := settingsOf(restart); !slices.Equal(got, []string{"server.watch"}) {
		t.Errorf("settings needing a restart = %q, want [server.watch]", got)
	}
	view := viewOf(t, s)
	view.want(t, "alerts.memory_pct", "60", types.ConfigSourceFile)
	// Still watching, as it was started: the view reports what is running.
	view.want(t, "server.watch", "true", types.ConfigSourceFile)
}

// TestWatch_WaitsForTheWriterToFinish holds the file open mid-write: the
// half-written file is not loaded, and the complete one is, once closed.
func TestWatch_WaitsForTheWriterToFinish(t *testing.T) {
	path := configDir(t, "server:\n  watch: true\nalerts:\n  memory_pct: 80\n")
	s := watched(t, path)

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("server:\n  watch: true\nalerts:\n  memory_pct: "); err != nil {
		t.Fatal(err)
	}
	s.Logs.AwaitMsg(t, "config: file is being written, reload waits until it is closed")
	if s.Logs.Has(msgFileReloaded) || s.Logs.Has(msgFileFailed) {
		t.Fatal("the watcher read the file while it was still being written")
	}
	viewOf(t, s).want(t, "alerts.memory_pct", "80", types.ConfigSourceFile)

	if _, err := f.WriteString("55\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	s.Logs.AwaitMsg(t, msgFileReloaded)
	viewOf(t, s).want(t, "alerts.memory_pct", "55", types.ConfigSourceFile)
}

// TestWatch_RefusesAnEmptyOrBrokenFile verifies a file caught empty, or one
// that no longer parses, leaves the running settings in force.
func TestWatch_RefusesAnEmptyOrBrokenFile(t *testing.T) {
	path := configDir(t, "server:\n  watch: true\nalerts:\n  memory_pct: 80\n")
	s := watched(t, path)

	rewrite(t, path, "")
	if r := s.Logs.AwaitMsg(t, msgFileFailed); r.Level() != "WARN" {
		t.Errorf("a refused reload is logged at %s, want WARN", r.Level())
	}
	viewOf(t, s).want(t, "alerts.memory_pct", "80", types.ConfigSourceFile)
}

// TestWatch_RemovedFileMeansDefaults verifies deleting the watched file runs
// the service on defaults, as it would have started without one.
func TestWatch_RemovedFileMeansDefaults(t *testing.T) {
	path := configDir(t, "server:\n  watch: true\nalerts:\n  memory_pct: 80\n")
	s := watched(t, path)

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	s.Logs.AwaitMsg(t, msgFileReloaded)
	view := viewOf(t, s)
	if view.file != "(not set)" {
		t.Errorf("config_file = %q after the file was removed, want (not set)", view.file)
	}
	view.want(t, "alerts.memory_pct", "90", types.ConfigSourceDefault)
}

// TestWatch_FollowsAConfigMapUpdate mounts the config the way Kubernetes does
// -- sysmon.yaml -> ..data/sysmon.yaml, ..data -> a timestamped directory --
// and swaps ..data to a new directory, as the kubelet does on an update.
func TestWatch_FollowsAConfigMapUpdate(t *testing.T) {
	mount := t.TempDir()
	writeVersion := func(name, body string) {
		t.Helper()
		if err := os.Mkdir(filepath.Join(mount, name), 0o700); err != nil {
			t.Fatal(err)
		}
		rewrite(t, filepath.Join(mount, name, "sysmon.yaml"), body)
	}
	writeVersion("..2026_09_24_01", "server:\n  watch: true\nalerts:\n  memory_pct: 80\n")
	if err := os.Symlink("..2026_09_24_01", filepath.Join(mount, "..data")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..data", "sysmon.yaml"), filepath.Join(mount, "sysmon.yaml")); err != nil {
		t.Fatal(err)
	}
	s := watched(t, filepath.Join(mount, "sysmon.yaml"))

	writeVersion("..2026_09_24_02", "server:\n  watch: true\nalerts:\n  memory_pct: 66\n")
	if err := os.Symlink("..2026_09_24_02", filepath.Join(mount, "..data_tmp")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(mount, "..data_tmp"), filepath.Join(mount, "..data")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(mount, "..2026_09_24_01")); err != nil {
		t.Fatal(err)
	}

	s.Logs.AwaitMsg(t, msgFileReloaded)
	viewOf(t, s).want(t, "alerts.memory_pct", "66", types.ConfigSourceFile)
}

// TestWatch_StopsWhenTheDirectoryGoes verifies the watcher says, at warning
// level, that its directory is gone rather than looking alive while blind.
func TestWatch_StopsWhenTheDirectoryGoes(t *testing.T) {
	path := configDir(t, "server:\n  watch: true\n")
	s := watched(t, path)

	if err := os.RemoveAll(filepath.Dir(path)); err != nil {
		t.Fatal(err)
	}
	if r := s.Logs.AwaitMsg(t, msgDirGone); r.Level() != "WARN" {
		t.Errorf("directory gone logged at %s, want WARN", r.Level())
	}
	s.Logs.AwaitMsg(t, "config: stopped watching the config file")
}

// TestWatch_ADirectoryThatCannotBeWatchedIsReported verifies a watch that
// cannot start -- the config's directory does not exist -- is said at
// startup, and the service runs on regardless.
func TestWatch_ADirectoryThatCannotBeWatchedIsReported(t *testing.T) {
	s := harness.Serve(t, harness.ServeOptions{
		ConfigPath: filepath.Join(t.TempDir(), "absent", "sysmon.yaml"),
		Env:        []string{"SYSMON_SERVER_WATCH=true"},
	})
	s.Logs.AwaitMsg(t, "config: cannot watch the config file")
	viewOf(t, s).want(t, "server.watch", "true", types.ConfigSourceEnv)
}

// TestWatch_FollowsAPathThroughSymlinks watches a config path that resolves
// through an absolute symlink to a directory and then a relative one that
// climbs out through "..": an edit to the file it finally names is reloaded.
func TestWatch_FollowsAPathThroughSymlinks(t *testing.T) {
	base := t.TempDir()
	for _, dir := range []string{"real", "links", "entry"} {
		if err := os.Mkdir(filepath.Join(base, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	target := filepath.Join(base, "real", "sysmon.yaml")
	rewrite(t, target, "server:\n  watch: true\nalerts:\n  memory_pct: 80\n")
	// entry/etc -> <base>/links (absolute), links/sysmon.yaml ->
	// ../real/sysmon.yaml (relative, through "..").
	if err := os.Symlink(filepath.Join(base, "links"), filepath.Join(base, "entry", "etc")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../real/sysmon.yaml", filepath.Join(base, "links", "sysmon.yaml")); err != nil {
		t.Fatal(err)
	}
	s := watched(t, filepath.Join(base, "entry", "etc", "sysmon.yaml"))

	rewrite(t, target, "server:\n  watch: true\nalerts:\n  memory_pct: 64\n")
	s.Logs.AwaitMsg(t, msgFileReloaded)
	viewOf(t, s).want(t, "alerts.memory_pct", "64", types.ConfigSourceFile)
}

// TestWatch_ASymlinkLoopIsRefusedAndFollowedOut replaces the watched file with
// a symlink loop: the reload is refused and the running settings stay, and
// once the loop is replaced by a real file again that file is loaded.
func TestWatch_ASymlinkLoopIsRefusedAndFollowedOut(t *testing.T) {
	path := configDir(t, "server:\n  watch: true\nalerts:\n  memory_pct: 80\n")
	dir := filepath.Dir(path)
	s := watched(t, path)

	if err := os.Symlink("loop-b", filepath.Join(dir, "loop-a")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("loop-a", filepath.Join(dir, "loop-b")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("loop-a", filepath.Join(dir, "loop.tmp")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, "loop.tmp"), path); err != nil {
		t.Fatal(err)
	}
	s.Logs.AwaitMsg(t, msgFileFailed)
	viewOf(t, s).want(t, "alerts.memory_pct", "80", types.ConfigSourceFile)

	replace(t, path, "server:\n  watch: true\nalerts:\n  memory_pct: 61\n")
	s.Logs.AwaitMsg(t, msgFileReloaded)
	viewOf(t, s).want(t, "alerts.memory_pct", "61", types.ConfigSourceFile)
}
