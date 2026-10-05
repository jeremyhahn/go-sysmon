package collector

import (
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"path/filepath"
	"slices"
	"syscall"
	"testing"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// fakeFS is a statfs seam over a fixed set of filesystems: each path present
// answers with its sizes, an error answers with the error, and any other path
// does not exist. It records the paths asked, in order.
type fakeFS struct {
	sizes  map[string][2]uint64
	errs   map[string]error
	called []string
}

func (f *fakeFS) statfs(path string) (total, free uint64, err error) {
	f.called = append(f.called, path)
	if err := f.errs[path]; err != nil {
		return 0, 0, err
	}
	s, ok := f.sizes[path]
	if !ok {
		return 0, 0, &fs.PathError{Op: "statfs", Path: path, Err: syscall.ENOENT}
	}
	return s[0], s[1], nil
}

// newFakeRuntimeDisk is a collector over fake whose runtime reports root ("":
// its API did not answer).
func newFakeRuntimeDisk(root string, fake *fakeFS) *RuntimeDiskCollector {
	c := NewRuntimeDiskCollector(func() string { return root })
	c.statfs = fake.statfs
	return c
}

// TestRuntimeDisk_MeasuresTheFirstCandidateThatExists covers the order
// candidates are tried in: the runtime's own data root when its API answered,
// then Docker's default, then Podman's, then /. A missing path is passed over.
func TestRuntimeDisk_MeasuresTheFirstCandidateThatExists(t *testing.T) {
	everything := map[string][2]uint64{
		"/srv/docker":                 {1000, 400},
		"/var/lib/docker":             {2000, 900},
		"/var/lib/containers/storage": {3000, 1200},
		"/":                           {4000, 3000},
	}
	without := func(paths ...string) map[string][2]uint64 {
		m := map[string][2]uint64{}
		for p, s := range everything {
			if !slices.Contains(paths, p) {
				m[p] = s
			}
		}
		return m
	}
	tests := []struct {
		name string
		root string
		fs   map[string][2]uint64
		want types.RuntimeDisk
	}{
		{"the runtime's relocated root", "/srv/docker", everything,
			types.RuntimeDisk{Path: "/srv/docker", TotalBytes: 1000, FreeBytes: 400}},
		// ci-node-2: sysmon is not in the docker group, the API does not
		// answer, and /var/lib/docker is a bind mount the disk list cannot
		// name. statfs on the path measures it all the same.
		{"API unreachable: docker's default", "", everything,
			types.RuntimeDisk{Path: "/var/lib/docker", TotalBytes: 2000, FreeBytes: 900}},
		{"the runtime's root is missing: falls back", "/srv/docker", without("/srv/docker"),
			types.RuntimeDisk{Path: "/var/lib/docker", TotalBytes: 2000, FreeBytes: 900}},
		{"podman", "", without("/var/lib/docker"),
			types.RuntimeDisk{Path: "/var/lib/containers/storage", TotalBytes: 3000, FreeBytes: 1200}},
		{"no runtime at all: the root filesystem", "", without("/var/lib/docker", "/var/lib/containers/storage"),
			types.RuntimeDisk{Path: "/", TotalBytes: 4000, FreeBytes: 3000}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newFakeRuntimeDisk(tc.root, &fakeFS{sizes: tc.fs})
			if err := c.Collect(); err != nil {
				t.Fatalf("Collect() = %v", err)
			}
			if got := c.Info(); got != tc.want {
				t.Errorf("Info() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestRuntimeDisk_AFailedStatfsIsUnknownNotAnotherDisk: a candidate that
// exists but cannot be measured ends the collection with nothing measured.
// Moving on would report / under the data root's name -- another filesystem's
// free space, to a scheduler that asked about this one.
func TestRuntimeDisk_AFailedStatfsIsUnknownNotAnotherDisk(t *testing.T) {
	fake := &fakeFS{
		sizes: map[string][2]uint64{"/": {4000, 3000}},
		errs:  map[string]error{"/var/lib/docker": syscall.EACCES},
	}
	c := newFakeRuntimeDisk("", fake)
	err := c.Collect()

	var statErr *RuntimeDiskStatfsError
	if !errors.As(err, &statErr) || statErr.Path != "/var/lib/docker" || !errors.Is(err, syscall.EACCES) {
		t.Fatalf("Collect() = %v, want a RuntimeDiskStatfsError for /var/lib/docker wrapping EACCES", err)
	}
	var collErr *types.CollectorError
	if !errors.As(err, &collErr) || collErr.Collector != runtimeDiskName {
		t.Errorf("Collect() = %v, want it attributed to the %s collector", err, runtimeDiskName)
	}
	if got := err.Error(); got != "collector runtime-disk: statfs /var/lib/docker: permission denied" {
		t.Errorf("Error() = %q; it names the path that could not be measured and why", got)
	}
	if got := c.Info(); got != (types.RuntimeDisk{}) {
		t.Errorf("Info() = %+v, want nothing measured", got)
	}
	if slices.Contains(fake.called, "/") {
		t.Errorf("statfs asked %v; nothing after the failing path may be measured", fake.called)
	}
}

// TestRuntimeDisk_AFailureReplacesAnEarlierMeasurement: a reading that can no
// longer be taken is not left standing as current.
func TestRuntimeDisk_AFailureReplacesAnEarlierMeasurement(t *testing.T) {
	fake := &fakeFS{sizes: map[string][2]uint64{"/var/lib/docker": {2000, 900}}}
	c := newFakeRuntimeDisk("", fake)
	if err := c.Collect(); err != nil {
		t.Fatalf("first Collect() = %v", err)
	}
	fake.errs = map[string]error{"/var/lib/docker": syscall.EIO}
	if err := c.Collect(); err == nil {
		t.Fatal("second Collect() = nil, want the statfs error")
	}
	if got := c.Info(); got.Path != "" {
		t.Errorf("Info() = %+v after a failed collection, want nothing measured", got)
	}
}

// TestRuntimeDisk_NothingExists: with no candidate present -- only possible
// on a root that cannot be seen at all -- the error says what was tried.
func TestRuntimeDisk_NothingExists(t *testing.T) {
	c := newFakeRuntimeDisk("/srv/docker", &fakeFS{})
	err := c.Collect()
	var missing *RuntimeDiskMissingError
	if !errors.As(err, &missing) {
		t.Fatalf("Collect() = %v, want a RuntimeDiskMissingError", err)
	}
	want := []string{"/srv/docker", "/var/lib/docker", "/var/lib/containers/storage", "/"}
	if !slices.Equal(missing.Paths, want) {
		t.Errorf("Paths = %v, want %v", missing.Paths, want)
	}
	if got := err.Error(); got != "collector runtime-disk: none of /srv/docker, /var/lib/docker, /var/lib/containers/storage, / exists" {
		t.Errorf("Error() = %q", got)
	}
}

// TestRuntimeDisk_UnmeasuredBeforeTheFirstCollection: a node that has not
// collected yet reports no path, which is how the summary says unknown.
func TestRuntimeDisk_UnmeasuredBeforeTheFirstCollection(t *testing.T) {
	if got := NewRuntimeDiskCollector(nil).Info(); got != (types.RuntimeDisk{}) {
		t.Errorf("Info() before Collect = %+v, want nothing measured", got)
	}
}

// TestRuntimeDisk_NilRootDirAsksNoRuntime: a collector built without a
// runtime goes straight to the defaults.
func TestRuntimeDisk_NilRootDirAsksNoRuntime(t *testing.T) {
	fake := &fakeFS{sizes: map[string][2]uint64{"/": {10, 5}}}
	c := NewRuntimeDiskCollector(nil)
	c.statfs = fake.statfs
	if err := c.Collect(); err != nil {
		t.Fatalf("Collect() = %v", err)
	}
	if want := []string{"/var/lib/docker", "/var/lib/containers/storage", "/"}; !slices.Equal(fake.called, want) {
		t.Errorf("statfs asked %v, want %v", fake.called, want)
	}
}

// TestStatfs_MeasuresADirectoryAndReportsAMissingOne exercises the real
// statfs, read-only, against the test's own temporary directory: sizes that
// are a filesystem's (free within total), and a missing path reported as
// not found, which is what lets Collect fall back past it.
func TestStatfs_MeasuresADirectoryAndReportsAMissingOne(t *testing.T) {
	dir := t.TempDir()
	total, free, err := statfs(dir)
	if err != nil {
		t.Fatalf("statfs(%s) = %v", dir, err)
	}
	if total == 0 || free > total {
		t.Errorf("statfs(%s) = total %d, free %d; want a non-empty filesystem with free within it", dir, total, free)
	}

	_, _, err = statfs(filepath.Join(dir, "absent"))
	if !isNotFound(err) {
		t.Errorf("statfs of a missing path = %v, want not found", err)
	}
}

// TestAssemble_CarriesTheRuntimeDisk: the measurement reaches the snapshot.
func TestAssemble_CarriesTheRuntimeDisk(t *testing.T) {
	sc := NewSystemCollector(slog.New(slog.NewTextHandler(io.Discard, nil)))
	sc.runtimeDisk.statfs = (&fakeFS{sizes: map[string][2]uint64{"/var/lib/docker": {2000, 900}}}).statfs
	if err := sc.runtimeDisk.Collect(); err != nil {
		t.Fatalf("Collect() = %v", err)
	}
	var snap types.Snapshot
	sc.assemble(&snap)
	if want := (types.RuntimeDisk{Path: "/var/lib/docker", TotalBytes: 2000, FreeBytes: 900}); snap.Virt.RuntimeDisk != want {
		t.Errorf("Virt.RuntimeDisk = %+v, want %+v", snap.Virt.RuntimeDisk, want)
	}
	// The runtime collector has not answered, so the system collector names
	// no root of its own.
	if root := sc.runtimeRootDir(); root != "" {
		t.Errorf("runtimeRootDir() = %q with no runtime answering, want empty", root)
	}
}

// TestRuntimeRootDir_OnlyWhenTheAPIAnswered: the runtime's data root is asked
// for only from a runtime that answered; a stale or absent answer names none,
// so the defaults are probed instead.
func TestRuntimeRootDir_OnlyWhenTheAPIAnswered(t *testing.T) {
	sc := NewSystemCollector(slog.New(slog.NewTextHandler(io.Discard, nil)))
	sc.runtime.info.Store(&types.RuntimeInfo{Available: true, RootDir: "/srv/docker"})
	if got := sc.runtimeRootDir(); got != "/srv/docker" {
		t.Errorf("runtimeRootDir() = %q, want the API's /srv/docker", got)
	}
	sc.runtime.info.Store(&types.RuntimeInfo{Available: false, RootDir: "/srv/docker"})
	if got := sc.runtimeRootDir(); got != "" {
		t.Errorf("runtimeRootDir() = %q from a runtime that did not answer, want empty", got)
	}
}
