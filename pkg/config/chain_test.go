package config

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("alerts: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustLink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

// realTempDir is a temp dir with any symlinks in its own path resolved, so
// expected plans name the directories resolvePlan will reach.
func realTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func assertPlan(t *testing.T, got, want watchPlan) {
	t.Helper()
	if !got.equal(want) {
		t.Errorf("resolvePlan() = %v, want %v", got, want)
	}
}

func TestResolvePlan_PlainFile(t *testing.T) {
	dir := realTempDir(t)
	mustWrite(t, filepath.Join(dir, "sysmon.yaml"))

	assertPlan(t, resolvePlan(filepath.Join(dir, "sysmon.yaml")), watchPlan{
		dir: {"sysmon.yaml": roleFile},
	})
}

// A missing file is watched for in its directory: it may be created later.
func TestResolvePlan_MissingFile(t *testing.T) {
	dir := realTempDir(t)
	assertPlan(t, resolvePlan(filepath.Join(dir, "sysmon.yaml")), watchPlan{
		dir: {"sysmon.yaml": roleFile},
	})
}

// The kubelet's layout: two links in the mount, the file in the timestamped
// directory ..data points at.
func TestResolvePlan_ConfigMapMount(t *testing.T) {
	dir := realTempDir(t)
	ts := "..2026_09_24_12_00_00.000000001"
	mustMkdir(t, filepath.Join(dir, ts))
	mustWrite(t, filepath.Join(dir, ts, "sysmon.yaml"))
	mustLink(t, ts, filepath.Join(dir, "..data"))
	mustLink(t, filepath.Join("..data", "sysmon.yaml"), filepath.Join(dir, "sysmon.yaml"))

	assertPlan(t, resolvePlan(filepath.Join(dir, "sysmon.yaml")), watchPlan{
		dir:                    {"sysmon.yaml": roleLink, "..data": roleLink},
		filepath.Join(dir, ts): {"sysmon.yaml": roleFile},
	})
}

// A link to a file in another directory, given as an absolute target.
func TestResolvePlan_AbsoluteLinkToAnotherDirectory(t *testing.T) {
	store, dir := realTempDir(t), realTempDir(t)
	mustWrite(t, filepath.Join(store, "real.yaml"))
	mustLink(t, filepath.Join(store, "real.yaml"), filepath.Join(dir, "sysmon.yaml"))

	assertPlan(t, resolvePlan(filepath.Join(dir, "sysmon.yaml")), watchPlan{
		dir:   {"sysmon.yaml": roleLink},
		store: {"real.yaml": roleFile},
	})
}

// A relative target with ".." is applied to the directory the link is in,
// after resolving it, not lexically to the path as written.
func TestResolvePlan_RelativeLinkWithParent(t *testing.T) {
	root := realTempDir(t)
	mustMkdir(t, filepath.Join(root, "etc"))
	mustMkdir(t, filepath.Join(root, "store"))
	mustWrite(t, filepath.Join(root, "store", "real.yaml"))
	mustLink(t, filepath.Join("..", "store", "real.yaml"), filepath.Join(root, "etc", "sysmon.yaml"))

	assertPlan(t, resolvePlan(filepath.Join(root, "etc", "sysmon.yaml")), watchPlan{
		filepath.Join(root, "etc"):   {"sysmon.yaml": roleLink},
		filepath.Join(root, "store"): {"real.yaml": roleFile},
	})
}

// A symlinked directory in the middle of the path is a link on it too.
func TestResolvePlan_SymlinkedDirectory(t *testing.T) {
	dir := realTempDir(t)
	mustMkdir(t, filepath.Join(dir, "v1"))
	mustWrite(t, filepath.Join(dir, "v1", "sysmon.yaml"))
	mustLink(t, "v1", filepath.Join(dir, "current"))

	assertPlan(t, resolvePlan(filepath.Join(dir, "current", "sysmon.yaml")), watchPlan{
		dir:                      {"current": roleLink},
		filepath.Join(dir, "v1"): {"sysmon.yaml": roleFile},
	})
}

// A dangling link ends the walk at the name it points to that is missing,
// watched as a name the path passes through.
func TestResolvePlan_DanglingLink(t *testing.T) {
	dir := realTempDir(t)
	mustLink(t, filepath.Join("..data", "sysmon.yaml"), filepath.Join(dir, "sysmon.yaml"))

	assertPlan(t, resolvePlan(filepath.Join(dir, "sysmon.yaml")), watchPlan{
		dir: {"sysmon.yaml": roleLink, "..data": roleLink},
	})
}

// A loop stops at the kernel's hop limit instead of spinning; the load then
// fails on ELOOP and the running configuration is kept.
func TestResolvePlan_LoopEnds(t *testing.T) {
	dir := realTempDir(t)
	mustLink(t, "b", filepath.Join(dir, "a"))
	mustLink(t, "a", filepath.Join(dir, "b"))

	assertPlan(t, resolvePlan(filepath.Join(dir, "a")), watchPlan{
		dir: {"a": roleLink, "b": roleLink},
	})
}

func TestSplitPath_DropsEmptyAndDotKeepsParent(t *testing.T) {
	got := splitPath("/a//./b/../c/")
	want := []string{"a", "b", "..", "c"}
	if len(got) != len(want) {
		t.Fatalf("splitPath() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("splitPath() = %v, want %v", got, want)
		}
	}
}

// refresh moves the watches with the path: after ..data is swapped, the old
// timestamped directory is no longer watched and the new one is, and an event
// still queued from the dropped watch is not read as a change.
func TestChainWatch_RefreshMovesWatches(t *testing.T) {
	dir := realTempDir(t)
	for _, ts := range []string{"..old", "..new"} {
		mustMkdir(t, filepath.Join(dir, ts))
		mustWrite(t, filepath.Join(dir, ts, "sysmon.yaml"))
	}
	mustLink(t, "..old", filepath.Join(dir, "..data"))
	mustLink(t, filepath.Join("..data", "sysmon.yaml"), filepath.Join(dir, "sysmon.yaml"))

	in, err := newInotify()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := in.close(); err != nil {
			t.Error(err)
		}
	}()
	c := newChainWatch(in, filepath.Join(dir, "sysmon.yaml"))
	if failed, err := c.refresh(); err != nil {
		t.Fatalf("refresh() = %v (%s)", err, failed)
	}
	oldWD, ok := c.wds[filepath.Join(dir, "..old")]
	if !ok {
		t.Fatalf("watched %v, want ..old among them", c.wds)
	}

	mustLink(t, "..new", filepath.Join(dir, "..data_tmp"))
	if err := os.Rename(filepath.Join(dir, "..data_tmp"), filepath.Join(dir, "..data")); err != nil {
		t.Fatal(err)
	}
	if failed, err := c.refresh(); err != nil {
		t.Fatalf("refresh() after the swap = %v (%s)", err, failed)
	}
	if _, ok := c.wds[filepath.Join(dir, "..old")]; ok {
		t.Errorf("..old is still watched after the swap")
	}
	if _, ok := c.wds[filepath.Join(dir, "..new")]; !ok {
		t.Errorf("..new is not watched after the swap: %v", c.wds)
	}
	if got := c.classify(rawEvent{wd: oldWD, mask: unix.IN_IGNORED}); got != eventNone {
		t.Errorf("IN_IGNORED from the dropped watch classified as %v, want eventNone", got)
	}
	if got := c.classify(rawEvent{wd: -1, mask: unix.IN_Q_OVERFLOW}); got != eventOverflow {
		t.Errorf("overflow classified as %v, want eventOverflow", got)
	}
	mountWD := c.wds[dir]
	if got := c.classify(rawEvent{wd: mountWD, mask: unix.IN_MOVED_TO, name: "..data"}); got != eventRelinked {
		t.Errorf("..data moved into place classified as %v, want eventRelinked", got)
	}
	if got := c.classify(rawEvent{wd: mountWD, mask: unix.IN_CREATE, name: "..data_tmp"}); got != eventNone {
		t.Errorf("..data_tmp created classified as %v, want eventNone: it is not on the path", got)
	}
	if got := c.classify(rawEvent{wd: mountWD, mask: unix.IN_DELETE_SELF}); got != eventRelinked {
		t.Errorf("a watched directory deleted classified as %v, want eventRelinked", got)
	}
}

// A directory on the path that cannot be watched is reported by name.
func TestChainWatch_RefreshReportsAnUnwatchableDirectory(t *testing.T) {
	in, err := newInotify()
	if err != nil {
		t.Fatal(err)
	}
	if err := in.close(); err != nil {
		t.Fatal(err)
	}
	dir := realTempDir(t)
	c := newChainWatch(in, filepath.Join(dir, "sysmon.yaml"))
	failed, err := c.refresh()
	if err == nil {
		t.Fatal("refresh() on a closed instance succeeded")
	}
	if failed != dir {
		t.Errorf("refresh() reported %q, want the directory %q", failed, dir)
	}
}

// TestChainWatch_FollowReportsADirectoryGoneMidRemoval is the interleaving
// that left the watcher blind (FIPS job 99288). `rm -r` deletes the file and
// then the directory; the file's IN_DELETE is handled while the directory is
// still there, and by the time the watches move it is gone. The refresh then
// swaps the directory's watch for one on its parent, so the directory's own
// IN_DELETE_SELF arrives on a dropped watch and says nothing. follow must see
// the directory gone at that point, not only before moving the watches.
func TestChainWatch_FollowReportsADirectoryGoneMidRemoval(t *testing.T) {
	parent := realTempDir(t)
	dir := filepath.Join(parent, "etc")
	mustMkdir(t, dir)
	mustWrite(t, filepath.Join(dir, "sysmon.yaml"))
	in, err := newInotify()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := in.close(); err != nil {
			t.Error(err)
		}
	}()
	c := newChainWatch(in, filepath.Join(dir, "sysmon.yaml"))
	if failed, err := c.refresh(); err != nil {
		t.Fatalf("refresh() = %v (%s)", err, failed)
	}
	dirWD := c.wds[dir]

	// The file's removal alone: the directory stays watched, for the file to
	// come back.
	if err := os.Remove(filepath.Join(dir, "sysmon.yaml")); err != nil {
		t.Fatal(err)
	}
	if gone, failed, err := c.follow(); gone || err != nil {
		t.Fatalf("follow() after the file went = %v, %v (%s), want the directory still followed", gone, err, failed)
	}
	if c.wds[dir] != dirWD {
		t.Fatalf("the directory's watch moved while the directory is still there: %v", c.wds)
	}

	// Then the directory, before the next event is handled.
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	gone, failed, err := c.follow()
	if !gone {
		t.Fatalf("follow() after the directory went = not gone (%v, %s): the watcher would watch %v for it forever", err, failed, c.wds)
	}
	if got := c.classify(rawEvent{wd: dirWD, mask: unix.IN_DELETE_SELF}); got != eventNone {
		t.Errorf("IN_DELETE_SELF on the dropped watch classified as %v: the reason follow has to decide", got)
	}
}
