package collector

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// ---- daemon info parsing --------------------------------------------------

func TestApplyDaemonInfo(t *testing.T) {
	t.Parallel()
	d := dockerInfo{
		ServerVersion:     "29.1.3",
		Driver:            "overlay2",
		DockerRootDir:     "/data/docker",
		Images:            83,
		Containers:        9,
		ContainersRunning: 6,
		ContainersStopped: 3,
		DriverStatus:      [][]string{{"Backing Filesystem", "extfs"}, {"Supports d_type", "true"}},
	}

	var info types.RuntimeInfo
	applyDaemonInfo(&info, d)

	if info.Version != "29.1.3" || info.StorageDriver != "overlay2" {
		t.Errorf("engine identity = %+v", info)
	}
	// The storage root is routinely relocated, which is exactly why it is
	// reported rather than assumed.
	if info.RootDir != "/data/docker" {
		t.Errorf("RootDir = %q, want /data/docker", info.RootDir)
	}
	if info.BackingFilesystem != "extfs" {
		t.Errorf("BackingFilesystem = %q, want extfs", info.BackingFilesystem)
	}
	if info.ContainersRunning != 6 || info.ContainersStopped != 3 {
		t.Errorf("container counts = %+v", info)
	}
}

func TestApplyDaemonInfo_MissingDriverStatus(t *testing.T) {
	t.Parallel()
	var info types.RuntimeInfo
	applyDaemonInfo(&info, dockerInfo{DriverStatus: [][]string{{"only-one-field"}}})
	if info.BackingFilesystem != "" {
		t.Errorf("BackingFilesystem = %q, want empty", info.BackingFilesystem)
	}
}

// ---- disk usage parsing ---------------------------------------------------

// diskUsageFixture mirrors a real /system/df response, including a dangling
// image and an unreferenced volume.
func diskUsageFixture() dockerDiskUsage {
	var df dockerDiskUsage
	df.LayersSize = 1000

	df.Images = append(df.Images, struct {
		ID          string   `json:"Id"`
		RepoTags    []string `json:"RepoTags"`
		Created     int64    `json:"Created"`
		Size        int64    `json:"Size"`
		SharedSize  int64    `json:"SharedSize"`
		Containers  int      `json:"Containers"`
		RepoDigests []string `json:"RepoDigests"`
	}{ID: "sha256:aaaa1111bbbb2222", RepoTags: []string{"app:latest"}, Size: 500, SharedSize: 100, Containers: 2})

	df.Images = append(df.Images, struct {
		ID          string   `json:"Id"`
		RepoTags    []string `json:"RepoTags"`
		Created     int64    `json:"Created"`
		Size        int64    `json:"Size"`
		SharedSize  int64    `json:"SharedSize"`
		Containers  int      `json:"Containers"`
		RepoDigests []string `json:"RepoDigests"`
	}{ID: "sha256:cccc3333dddd4444", RepoTags: []string{"<none>:<none>"}, Size: 300, Containers: 0})

	df.Volumes = append(df.Volumes, struct {
		Name      string `json:"Name"`
		UsageData struct {
			Size     int64 `json:"Size"`
			RefCount int64 `json:"RefCount"`
		} `json:"UsageData"`
	}{Name: "used"})
	df.Volumes[0].UsageData.Size = 200
	df.Volumes[0].UsageData.RefCount = 1

	df.Volumes = append(df.Volumes, struct {
		Name      string `json:"Name"`
		UsageData struct {
			Size     int64 `json:"Size"`
			RefCount int64 `json:"RefCount"`
		} `json:"UsageData"`
	}{Name: "orphan"})
	df.Volumes[1].UsageData.Size = 400
	df.Volumes[1].UsageData.RefCount = 0

	df.BuildCache = append(df.BuildCache, struct {
		Size   int64 `json:"Size"`
		InUse  bool  `json:"InUse"`
		Shared bool  `json:"Shared"`
	}{Size: 700, InUse: false})

	return df
}

func TestApplyDiskUsage_ClassifiesImages(t *testing.T) {
	t.Parallel()
	var info types.RuntimeInfo
	applyDiskUsage(&info, diskUsageFixture())

	if len(info.Images) != 2 {
		t.Fatalf("got %d images, want 2", len(info.Images))
	}
	// Largest first.
	if info.Images[0].SizeBytes != 500 {
		t.Errorf("images not sorted largest first: %+v", info.Images)
	}
	if info.Images[0].ShortID != "aaaa1111bbbb" {
		t.Errorf("ShortID = %q, want the sha256 prefix stripped", info.Images[0].ShortID)
	}
	if !info.Images[0].InUse || info.Images[0].Containers != 2 {
		t.Errorf("in-use image misclassified: %+v", info.Images[0])
	}

	dangling := info.Images[1]
	if !dangling.Dangling {
		t.Error("an image tagged <none>:<none> must be reported as dangling")
	}
	if len(dangling.Tags) != 0 {
		t.Errorf("dangling image kept a placeholder tag: %v", dangling.Tags)
	}
	if info.DanglingImages != 1 || info.UnusedImages != 1 {
		t.Errorf("counts = dangling %d unused %d, want 1 and 1", info.DanglingImages, info.UnusedImages)
	}
}

// TestApplyDiskUsage_ReclaimableIsClamped is the regression test for a
// reclaimable figure larger than the total on disk: per-image sizes include
// shared layers, so summing them over-counts.
func TestApplyDiskUsage_ReclaimableIsClamped(t *testing.T) {
	t.Parallel()
	df := diskUsageFixture()
	df.LayersSize = 100 // smaller than the 300-byte unused image

	var info types.RuntimeInfo
	applyDiskUsage(&info, df)

	if info.ReclaimableBytes > info.LayersBytes {
		t.Errorf("ReclaimableBytes = %d exceeds LayersBytes = %d",
			info.ReclaimableBytes, info.LayersBytes)
	}
}

func TestApplyDiskUsage_VolumesAndBuildCache(t *testing.T) {
	t.Parallel()
	var info types.RuntimeInfo
	applyDiskUsage(&info, diskUsageFixture())

	if info.VolumesCount != 2 || info.VolumesUnused != 1 {
		t.Errorf("volumes = %d total %d unused, want 2 and 1", info.VolumesCount, info.VolumesUnused)
	}
	if info.VolumesBytes != 600 || info.VolumesReclaimableBytes != 400 {
		t.Errorf("volume bytes = %d total %d reclaimable, want 600 and 400",
			info.VolumesBytes, info.VolumesReclaimableBytes)
	}
	if info.BuildCacheEntries != 1 || info.BuildCacheBytes != 700 {
		t.Errorf("build cache = %d entries %d bytes", info.BuildCacheEntries, info.BuildCacheBytes)
	}
	// An unused cache entry is entirely reclaimable.
	if info.BuildCacheReclaimableBytes != 700 {
		t.Errorf("BuildCacheReclaimableBytes = %d, want 700", info.BuildCacheReclaimableBytes)
	}
}

func TestApplyDiskUsage_TruncatesImageList(t *testing.T) {
	t.Parallel()
	var df dockerDiskUsage
	for i := 0; i < maxImages+5; i++ {
		df.Images = append(df.Images, struct {
			ID          string   `json:"Id"`
			RepoTags    []string `json:"RepoTags"`
			Created     int64    `json:"Created"`
			Size        int64    `json:"Size"`
			SharedSize  int64    `json:"SharedSize"`
			Containers  int      `json:"Containers"`
			RepoDigests []string `json:"RepoDigests"`
		}{ID: "sha256:" + string(rune('a'+i%26)) + "00000000000", Size: int64(i), Containers: 1})
	}

	var info types.RuntimeInfo
	applyDiskUsage(&info, df)

	if len(info.Images) != maxImages {
		t.Errorf("got %d images, want the list capped at %d", len(info.Images), maxImages)
	}
	if info.ImagesTruncated != 5 {
		t.Errorf("ImagesTruncated = %d, want 5", info.ImagesTruncated)
	}
}

func TestShortImageID(t *testing.T) {
	t.Parallel()
	if got := shortImageID("sha256:004c327801b2b84b8fcb"); got != "004c327801b2" {
		t.Errorf("shortImageID() = %q", got)
	}
	if got := shortImageID("abc"); got != "abc" {
		t.Errorf("short input must pass through, got %q", got)
	}
}

// ---- collector lifecycle --------------------------------------------------

// TestRuntimeCollector_DisabledDoesNoIO verifies the default is inert: the
// runtime socket is root-equivalent access, so nothing may touch it unasked.
func TestRuntimeCollector_DisabledDoesNoIO(t *testing.T) {
	t.Parallel()
	c := NewRuntimeCollector(slog.Default())

	if c.Enabled() {
		t.Error("a new RuntimeCollector must start disabled")
	}
	if err := c.Collect(); err != nil {
		t.Fatalf("Collect() error = %v, want nil", err)
	}
	if info := c.Info(); info.Available {
		t.Errorf("a disabled collector reported data: %+v", info)
	}
}

func TestRuntimeCollector_EnableToggles(t *testing.T) {
	t.Parallel()
	c := NewRuntimeCollector(nil)

	c.Enable(true)
	if !c.Enabled() {
		t.Error("Enable(true) did not take effect")
	}
	c.Enable(false)
	if c.Enabled() {
		t.Error("Enable(false) did not take effect")
	}
}

// TestRuntimeCollector_UnreachableSocket verifies an absent runtime is not an
// error and yields no data.
func TestRuntimeCollector_UnreachableSocket(t *testing.T) {
	original := runtimeSocketPaths
	runtimeSocketPaths = []struct {
		path   string
		engine string
	}{{filepath.Join(t.TempDir(), "absent.sock"), "docker"}}
	t.Cleanup(func() { runtimeSocketPaths = original })

	c := NewRuntimeCollector(slog.Default())
	c.Enable(true)

	if err := c.Collect(); err != nil {
		t.Fatalf("Collect() error = %v, want nil for an absent socket", err)
	}
	if c.Info().Available {
		t.Error("an absent socket must report Available false")
	}
}

// TestRuntimeCollector_AgainstFakeDaemon drives the whole path against a unix
// socket serving canned Docker API responses.
func TestRuntimeCollector_AgainstFakeDaemon(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "docker.sock")

	mux := http.NewServeMux()
	mux.HandleFunc("/info", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(dockerInfo{
			ServerVersion: "29.1.3", Driver: "overlay2", DockerRootDir: "/data/docker",
			Images: 2, Containers: 1, ContainersRunning: 1,
			DriverStatus: [][]string{{"Backing Filesystem", "extfs"}},
		})
	})
	mux.HandleFunc("/system/df", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(diskUsageFixture())
	})

	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: mux} //nolint:gosec
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		_ = os.Remove(sock)
	})

	original := runtimeSocketPaths
	runtimeSocketPaths = []struct {
		path   string
		engine string
	}{{sock, "docker"}}
	t.Cleanup(func() { runtimeSocketPaths = original })

	c := NewRuntimeCollector(slog.Default())
	c.Enable(true)

	if err := c.Collect(); err != nil {
		t.Fatalf("Collect() error = %v", err)
	}

	// Engine details arrive synchronously.
	info := c.Info()
	if !info.Available || info.RootDir != "/data/docker" {
		t.Fatalf("engine details missing: %+v", info)
	}

	// Disk usage is collected in the background; wait for it.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c.WaitForDiskUsage(ctx)

	if err := c.Collect(); err != nil {
		t.Fatalf("second Collect() error = %v", err)
	}
	info = c.Info()
	if len(info.Images) != 2 {
		t.Errorf("got %d images after the async query, want 2", len(info.Images))
	}
	if info.VolumesBytes != 600 {
		t.Errorf("VolumesBytes = %d, want 600", info.VolumesBytes)
	}
}

// TestWaitForDiskUsage_ReturnsWhenDisabled verifies the wait cannot hang when
// the collector is off.
func TestWaitForDiskUsage_ReturnsWhenDisabled(t *testing.T) {
	t.Parallel()
	c := NewRuntimeCollector(nil)

	done := make(chan struct{})
	go func() {
		c.WaitForDiskUsage(context.Background())
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Error("WaitForDiskUsage hung on a disabled collector")
	}
}
