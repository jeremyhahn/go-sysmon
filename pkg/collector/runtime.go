package collector

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// runtimeSocketPaths are probed in order for a container runtime API.
//
// Unlike every other collector, this one talks to a daemon. Image inventory,
// layer sizes and reclaimable space exist only in the runtime's own database:
// its storage directory is root-owned, so there is no unprivileged filesystem
// route to them. Because the socket is effectively root-equivalent access, the
// collector is opt-in and disabled by default.
var runtimeSocketPaths = []struct {
	path   string
	engine string
}{
	{"/var/run/docker.sock", "docker"},
	{"/run/docker.sock", "docker"},
	{"/run/podman/podman.sock", "podman"},
}

// runtimeTimeout bounds the fast /info request, which a snapshot waits on.
const runtimeTimeout = 2 * time.Second

// diskUsageTimeout bounds the /system/df request. That call walks every layer,
// volume and build cache entry: on a host with a large image library it takes
// the better part of a minute, so it needs a generous ceiling.
const diskUsageTimeout = 60 * time.Second

// diskUsageInterval is the minimum gap between disk usage refreshes. The query
// is expensive for the daemon as well as for us, so it is deliberately rare.
const diskUsageInterval = 5 * time.Minute

// maxImages caps how many images are reported so a host with thousands does not
// produce an unbounded snapshot: every image is serialised into every snapshot
// the event streams, so the list is a per-second cost, not a one-off.
//
// The UI paginates, so this is a transport bound rather than a display one and
// can be generous. Anything dropped is reported in ImagesTruncated and shown
// in the table footer, and the aggregate counts and sizes always cover every
// image regardless.
const maxImages = 500

// RuntimeCollector reports container image inventory and storage usage from a
// container runtime's API socket.
type RuntimeCollector struct {
	info    atomic.Pointer[types.RuntimeInfo]
	logger  *slog.Logger
	enabled atomic.Bool

	// Disk usage is refreshed on a background goroutine: /system/df routinely
	// takes many seconds, and a snapshot must never block on it. Collect
	// serves the last completed result and triggers a refresh when it is due.
	diskUsage   atomic.Pointer[types.RuntimeInfo]
	dfInFlight  atomic.Bool
	dfLastStart atomic.Int64
}

// NewRuntimeCollector returns a collector that is disabled until Enable is
// called. A disabled collector performs no I/O and reports Available false.
func NewRuntimeCollector(logger *slog.Logger) *RuntimeCollector {
	if logger == nil {
		logger = slog.Default()
	}
	c := &RuntimeCollector{logger: logger}
	c.info.Store(&types.RuntimeInfo{})
	return c
}

// Enable turns daemon queries on or off.
func (c *RuntimeCollector) Enable(on bool) {
	c.enabled.Store(on)
}

// Enabled reports whether daemon queries are turned on.
func (c *RuntimeCollector) Enabled() bool {
	return c.enabled.Load()
}

// Info returns the most recently collected runtime data.
func (c *RuntimeCollector) Info() types.RuntimeInfo {
	return *c.info.Load()
}

// Collect queries the runtime for engine details and disk usage. A missing or
// unreadable socket is not an error: it means the runtime is absent or this
// user cannot reach it, and the rest of the snapshot is still valid.
func (c *RuntimeCollector) Collect() error {
	if !c.enabled.Load() {
		c.info.Store(&types.RuntimeInfo{})
		return nil
	}

	socket, engine := findRuntimeSocket()
	if socket == "" {
		c.info.Store(&types.RuntimeInfo{})
		return nil
	}

	client := unixHTTPClient(socket, runtimeTimeout)
	ctx, cancel := context.WithTimeout(context.Background(), runtimeTimeout)
	defer cancel()

	info := types.RuntimeInfo{Available: true, Engine: engine, SocketPath: socket}

	var daemon dockerInfo
	if err := getJSON(ctx, client, "/info", &daemon); err != nil {
		c.logger.Debug("runtime: /info query failed", "socket", socket, "error", err)
		c.info.Store(&types.RuntimeInfo{})
		return nil
	}
	applyDaemonInfo(&info, daemon)

	// Merge the most recent completed disk usage, then trigger a refresh if
	// one is due. The first snapshot after enabling therefore reports engine
	// details immediately and sizes once the background query lands.
	if cached := c.diskUsage.Load(); cached != nil {
		mergeDiskUsage(&info, cached)
	}
	c.refreshDiskUsage(socket)

	c.info.Store(&info)
	return nil
}

// refreshDiskUsage starts a background /system/df query when one is not
// already running and the previous result is older than diskUsageInterval.
func (c *RuntimeCollector) refreshDiskUsage(socket string) {
	now := time.Now()
	last := c.dfLastStart.Load()
	if last != 0 && now.Sub(time.UnixMilli(last)) < diskUsageInterval {
		return
	}
	if !c.dfInFlight.CompareAndSwap(false, true) {
		return
	}
	c.dfLastStart.Store(now.UnixMilli())

	go func() {
		defer c.dfInFlight.Store(false)

		ctx, cancel := context.WithTimeout(context.Background(), diskUsageTimeout)
		defer cancel()

		var df dockerDiskUsage
		if err := getJSON(ctx, unixHTTPClient(socket, diskUsageTimeout), "/system/df", &df); err != nil {
			c.logger.Debug("runtime: /system/df query failed", "socket", socket, "error", err)
			return
		}

		var usage types.RuntimeInfo
		applyDiskUsage(&usage, df)
		c.diskUsage.Store(&usage)
	}()
}

// mergeDiskUsage copies the asynchronously collected size fields into the
// snapshot being assembled.
func mergeDiskUsage(dst *types.RuntimeInfo, src *types.RuntimeInfo) {
	dst.LayersBytes = src.LayersBytes
	dst.ReclaimableBytes = src.ReclaimableBytes
	dst.DanglingImages = src.DanglingImages
	dst.UnusedImages = src.UnusedImages
	dst.VolumesCount = src.VolumesCount
	dst.VolumesUnused = src.VolumesUnused
	dst.VolumesBytes = src.VolumesBytes
	dst.VolumesReclaimableBytes = src.VolumesReclaimableBytes
	dst.BuildCacheEntries = src.BuildCacheEntries
	dst.BuildCacheBytes = src.BuildCacheBytes
	dst.BuildCacheReclaimableBytes = src.BuildCacheReclaimableBytes
	dst.Images = src.Images
	dst.ImagesTruncated = src.ImagesTruncated
}

// WaitForDiskUsage blocks until a disk usage result is available or the
// context is done. One-shot commands use it so that "sysmon images" prints
// sizes rather than an empty table on first run.
func (c *RuntimeCollector) WaitForDiskUsage(ctx context.Context) {
	for c.diskUsage.Load() == nil {
		if !c.enabled.Load() {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// findRuntimeSocket returns the first reachable runtime socket and its engine
// name, or empty strings when none is available to this user.
func findRuntimeSocket() (string, string) {
	for _, candidate := range runtimeSocketPaths {
		conn, err := net.DialTimeout("unix", candidate.path, 200*time.Millisecond)
		if err != nil {
			continue
		}
		if closeErr := conn.Close(); closeErr != nil {
			continue
		}
		return candidate.path, candidate.engine
	}
	return "", ""
}

// unixHTTPClient returns an HTTP client that speaks to a unix socket. The
// Docker and Podman APIs are HTTP over that socket, so no client library is
// required.
func unixHTTPClient(socket string, timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socket)
			},
		},
	}
}

// getJSON performs a GET against the runtime API and decodes the response.
func getJSON(ctx context.Context, client *http.Client, path string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost"+path, nil)
	if err != nil {
		return err
	}

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return &types.CollectorError{
			Collector: "runtime",
			Cause:     &runtimeStatusError{Path: path, Status: resp.StatusCode},
		}
	}
	return json.NewDecoder(resp.Body).Decode(dst)
}

// runtimeStatusError reports a non-200 response from the runtime API.
type runtimeStatusError struct {
	Path   string
	Status int
}

func (e *runtimeStatusError) Error() string {
	return "runtime API " + e.Path + " returned status " + strconv.Itoa(e.Status)
}

// dockerInfo is the subset of the /info response this collector uses.
type dockerInfo struct {
	ServerVersion     string     `json:"ServerVersion"`
	Driver            string     `json:"Driver"`
	DriverStatus      [][]string `json:"DriverStatus"`
	DockerRootDir     string     `json:"DockerRootDir"`
	Images            int        `json:"Images"`
	Containers        int        `json:"Containers"`
	ContainersRunning int        `json:"ContainersRunning"`
	ContainersStopped int        `json:"ContainersStopped"`
	ContainersPaused  int        `json:"ContainersPaused"`
}

// dockerDiskUsage is the subset of the /system/df response this collector uses.
type dockerDiskUsage struct {
	LayersSize int64 `json:"LayersSize"`
	Images     []struct {
		ID          string   `json:"Id"`
		RepoTags    []string `json:"RepoTags"`
		Created     int64    `json:"Created"`
		Size        int64    `json:"Size"`
		SharedSize  int64    `json:"SharedSize"`
		Containers  int      `json:"Containers"`
		RepoDigests []string `json:"RepoDigests"`
	} `json:"Images"`
	Volumes []struct {
		Name      string `json:"Name"`
		UsageData struct {
			Size     int64 `json:"Size"`
			RefCount int64 `json:"RefCount"`
		} `json:"UsageData"`
	} `json:"Volumes"`
	BuildCache []struct {
		Size   int64 `json:"Size"`
		InUse  bool  `json:"InUse"`
		Shared bool  `json:"Shared"`
	} `json:"BuildCache"`
}

// applyDaemonInfo copies engine identity and storage layout into info.
func applyDaemonInfo(info *types.RuntimeInfo, d dockerInfo) {
	info.Version = d.ServerVersion
	info.StorageDriver = d.Driver
	info.RootDir = d.DockerRootDir
	info.ImagesTotal = d.Images
	info.ContainersTotal = d.Containers
	info.ContainersRunning = d.ContainersRunning
	info.ContainersStopped = d.ContainersStopped
	info.ContainersPaused = d.ContainersPaused

	// DriverStatus is an array of [key, value] pairs.
	for _, pair := range d.DriverStatus {
		if len(pair) == 2 && pair[0] == "Backing Filesystem" {
			info.BackingFilesystem = pair[1]
		}
	}
}

// applyDiskUsage summarises image, volume and build cache consumption, and
// builds the per-image list.
func applyDiskUsage(info *types.RuntimeInfo, df dockerDiskUsage) {
	info.LayersBytes = uint64(max64(df.LayersSize, 0))

	images := make([]types.ImageInfo, 0, len(df.Images))
	for _, img := range df.Images {
		tags := img.RepoTags
		dangling := len(tags) == 0 || (len(tags) == 1 && tags[0] == "<none>:<none>")
		if dangling {
			tags = nil
		}

		size := uint64(max64(img.Size, 0))
		inUse := img.Containers > 0

		if dangling {
			info.DanglingImages++
		}
		if !inUse {
			info.UnusedImages++
			// Unused images are what "docker image prune -a" would remove.
			info.ReclaimableBytes += size
		}

		images = append(images, types.ImageInfo{
			ID:              img.ID,
			ShortID:         shortImageID(img.ID),
			Tags:            tags,
			SizeBytes:       size,
			SharedSizeBytes: uint64(max64(img.SharedSize, 0)),
			Containers:      img.Containers,
			CreatedUnix:     img.Created,
			InUse:           inUse,
			Dangling:        dangling,
		})
	}

	// Per-image sizes include layers shared with other images, so summing them
	// over-counts. Clamp the estimate to the deduplicated total on disk, which
	// is the most that pruning could possibly return.
	if info.LayersBytes > 0 && info.ReclaimableBytes > info.LayersBytes {
		info.ReclaimableBytes = info.LayersBytes
	}

	// Largest first: an operator reclaiming space wants the big ones.
	sort.Slice(images, func(i, j int) bool {
		if images[i].SizeBytes != images[j].SizeBytes {
			return images[i].SizeBytes > images[j].SizeBytes
		}
		return images[i].ID < images[j].ID
	})
	if len(images) > maxImages {
		info.ImagesTruncated = len(images) - maxImages
		images = images[:maxImages]
	}
	for i := range images {
		images[i].Index = i
	}
	info.Images = images

	for _, v := range df.Volumes {
		info.VolumesCount++
		info.VolumesBytes += uint64(max64(v.UsageData.Size, 0))
		if v.UsageData.RefCount == 0 {
			info.VolumesUnused++
			info.VolumesReclaimableBytes += uint64(max64(v.UsageData.Size, 0))
		}
	}

	for _, b := range df.BuildCache {
		info.BuildCacheEntries++
		info.BuildCacheBytes += uint64(max64(b.Size, 0))
		if !b.InUse {
			info.BuildCacheReclaimableBytes += uint64(max64(b.Size, 0))
		}
	}
}

// shortImageID trims the "sha256:" prefix and truncates to the 12 characters
// container tooling displays.
func shortImageID(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > shortIDLen {
		return id[:shortIDLen]
	}
	return id
}

// max64 clamps a signed API value to a non-negative number.
func max64(v, floor int64) int64 {
	if v < floor {
		return floor
	}
	return v
}
