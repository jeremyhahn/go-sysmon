package collector

import (
	"strings"
	"sync/atomic"

	"golang.org/x/sys/unix"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// defaultRuntimeRoots are the data roots probed, in order, when the container
// runtime's API did not say where its own is: Docker's, then Podman's (and
// every containers/storage engine's). The last resort, "/", is not a runtime
// root at all; it is there so a node with no runtime still reports the free
// disk a scheduler needs.
var defaultRuntimeRoots = []string{"/var/lib/docker", "/var/lib/containers/storage", "/"}

// statfsFunc measures the filesystem holding path: its size and the space an
// unprivileged writer may still use, in bytes. It is the seam the tests use in
// place of the host's filesystems.
type statfsFunc func(path string) (total, free uint64, err error)

// RuntimeDiskCollector measures the free space on the filesystem a container
// runtime writes its images and layers to (#239). See types.RuntimeDisk for
// why it is a statfs of the path and not a lookup in the disk list.
type RuntimeDiskCollector struct {
	disk atomic.Pointer[types.RuntimeDisk]
	// rootDir is the data root the runtime's API reported, or "" when the API
	// did not answer -- the usual case for a node whose user is not in the
	// docker group.
	rootDir func() string
	statfs  statfsFunc
	// defaults are the paths tried after rootDir, in order.
	defaults []string
}

// NewRuntimeDiskCollector returns a RuntimeDiskCollector that asks rootDir for
// the runtime's own data root on each collection. rootDir may be nil, for a
// node that never asks a runtime.
func NewRuntimeDiskCollector(rootDir func() string) *RuntimeDiskCollector {
	if rootDir == nil {
		rootDir = func() string { return "" }
	}
	c := &RuntimeDiskCollector{rootDir: rootDir, statfs: statfs, defaults: defaultRuntimeRoots}
	c.disk.Store(&types.RuntimeDisk{})
	return c
}

// Collect measures the first candidate path that exists: the runtime's own
// data root, then the defaults. A path that does not exist is passed over; any
// other failure ends the collection with nothing measured, because reporting
// the next candidate instead -- / where the data root was unreadable -- would
// be another filesystem's free space under a scheduler that asked for this
// one.
func (c *RuntimeDiskCollector) Collect() error {
	disk, err := c.measure()
	c.disk.Store(&disk)
	return err
}

// Info returns the most recent measurement. Its Path is empty when nothing
// has been measured.
func (c *RuntimeDiskCollector) Info() types.RuntimeDisk {
	return *c.disk.Load()
}

// measure statfs's the candidates in order; see Collect.
func (c *RuntimeDiskCollector) measure() (types.RuntimeDisk, error) {
	candidates := c.defaults
	if root := c.rootDir(); root != "" {
		candidates = append([]string{root}, c.defaults...)
	}
	for _, path := range candidates {
		total, free, err := c.statfs(path)
		switch {
		case err == nil:
			return types.RuntimeDisk{Path: path, TotalBytes: total, FreeBytes: free}, nil
		case isNotFound(err):
			continue
		default:
			return types.RuntimeDisk{}, &types.CollectorError{Collector: runtimeDiskName,
				Cause: &RuntimeDiskStatfsError{Path: path, Err: err}}
		}
	}
	// Only reachable when no candidate exists. "/" always does, so this is a
	// root filesystem that cannot be seen at all.
	return types.RuntimeDisk{}, &types.CollectorError{Collector: runtimeDiskName,
		Cause: &RuntimeDiskMissingError{Paths: candidates}}
}

// runtimeDiskName names the collector in its errors.
const runtimeDiskName = "runtime-disk"

// RuntimeDiskStatfsError reports a data-root candidate that exists but could
// not be measured: the collection reports nothing rather than moving on to a
// different filesystem.
type RuntimeDiskStatfsError struct {
	Path string
	Err  error
}

func (e *RuntimeDiskStatfsError) Error() string {
	return "statfs " + e.Path + ": " + e.Err.Error()
}

func (e *RuntimeDiskStatfsError) Unwrap() error { return e.Err }

// RuntimeDiskMissingError reports that none of the data-root candidates
// exists, so there was nothing to measure.
type RuntimeDiskMissingError struct {
	Paths []string
}

func (e *RuntimeDiskMissingError) Error() string {
	return "none of " + strings.Join(e.Paths, ", ") + " exists"
}

// statfs is statfsFunc over the host's filesystems. Sizes are in f_bsize
// units, as gopsutil's disk usage -- and so the disk list -- computes them,
// so the two never disagree about the same filesystem.
func statfs(path string) (total, free uint64, err error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	bsize := uint64(st.Bsize) //nolint:gosec // G115: a block size is positive.
	return st.Blocks * bsize, st.Bavail * bsize, nil
}
