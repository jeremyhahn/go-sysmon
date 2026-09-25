//go:build integration

package collector

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jeremyhahn/go-sysmon/test/integration/harness"
)

// engine stands in for the Docker Engine API on a unix socket: the daemon an
// operator's `sysmon images` and the server's image inventory talk to.
type engine struct {
	socket string

	mu       sync.Mutex
	info     int    // status /info answers with
	df       int    // status /system/df answers with
	infoBody string // what /info answers with
	dfBody   string // what /system/df answers with
	requests []string
}

// engineInfo is a daemon's /info, as Docker 27 answers it.
const engineInfo = `{
	"ServerVersion": "27.3.1",
	"Driver": "overlay2",
	"DriverStatus": [["Backing Filesystem", "extfs"], ["Supports d_type", "true"]],
	"DockerRootDir": "/var/lib/docker",
	"Images": 4,
	"Containers": 3,
	"ContainersRunning": 2,
	"ContainersStopped": 1,
	"ContainersPaused": 0
}`

// engineDiskUsage is /system/df: four images, one of them dangling and two in
// use, two volumes, and build cache.
const engineDiskUsage = `{
	"LayersSize": 3000000000,
	"Images": [
		{"Id": "sha256:aaaa1111bbbb2222cccc3333dddd4444eeee5555ffff6666aaaa7777bbbb8888", "RepoTags": ["nginx:1.27"], "Created": 1726000000, "Size": 190000000, "SharedSize": 80000000, "Containers": 2},
		{"Id": "sha256:bbbb1111cccc2222dddd3333eeee4444ffff5555aaaa6666bbbb7777cccc8888", "RepoTags": ["postgres:16", "db:latest"], "Created": 1725000000, "Size": 450000000, "SharedSize": 80000000, "Containers": 1},
		{"Id": "sha256:cccc1111dddd2222eeee3333ffff4444aaaa5555bbbb6666cccc7777dddd8888", "RepoTags": ["<none>:<none>"], "Created": 1720000000, "Size": 120000000, "SharedSize": 0, "Containers": 0},
		{"Id": "sha256:dddd1111eeee2222ffff3333aaaa4444bbbb5555cccc6666dddd7777eeee8888", "RepoTags": ["busybox:1.36"], "Created": 1727000000, "Size": 4000000, "SharedSize": 0, "Containers": 0}
	],
	"Volumes": [
		{"Name": "pgdata", "UsageData": {"Size": 800000000, "RefCount": 1}},
		{"Name": "orphan", "UsageData": {"Size": 50000000, "RefCount": 0}}
	],
	"BuildCache": [
		{"Size": 70000000, "InUse": false, "Shared": false},
		{"Size": 30000000, "InUse": true, "Shared": false}
	]
}`

// startEngine serves the stand-in on a socket in a fresh directory.
func startEngine(t *testing.T) *engine {
	t.Helper()
	// A short directory: a unix socket path is limited to 108 bytes, which a
	// test's own temporary directory can exceed.
	dir, err := os.MkdirTemp("", "engine")
	if err != nil {
		t.Fatalf("create the socket directory: %v", err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("remove %s: %v", dir, err)
		}
	})
	e := &engine{socket: filepath.Join(dir, "docker.sock"), info: http.StatusOK, df: http.StatusOK,
		infoBody: engineInfo, dfBody: engineDiskUsage}

	ln, err := net.Listen("unix", e.socket)
	if err != nil {
		t.Fatalf("listen on %s: %v", e.socket, err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /info", func(w http.ResponseWriter, r *http.Request) {
		status, body := e.reply(func() (int, string) { return e.info, e.infoBody })
		e.answer(w, r, status, body)
	})
	mux.HandleFunc("GET /system/df", func(w http.ResponseWriter, r *http.Request) {
		status, body := e.reply(func() (int, string) { return e.df, e.dfBody })
		e.answer(w, r, status, body)
	})
	srv := &http.Server{Handler: mux}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	t.Cleanup(func() {
		if err := srv.Close(); err != nil {
			t.Errorf("stop the engine stand-in: %v", err)
		}
		if err := <-served; err != nil && err != http.ErrServerClosed {
			t.Errorf("engine stand-in: %v", err)
		}
	})
	return e
}

// reply reads what an endpoint is set to answer, under the lock.
func (e *engine) reply(read func() (int, string)) (int, string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return read()
}

// answer records the request and replies with status, and body when it is 200.
func (e *engine) answer(w http.ResponseWriter, r *http.Request, status int, body string) {
	e.mu.Lock()
	e.requests = append(e.requests, r.URL.Path)
	e.mu.Unlock()
	if status != http.StatusOK {
		http.Error(w, `{"message":"daemon is shutting down"}`, status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write([]byte(body)); err != nil {
		return
	}
}

// asked reports whether the stand-in has been sent a request for path.
func (e *engine) asked(path string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, p := range e.requests {
		if p == path {
			return true
		}
	}
	return false
}

// sysmonWithEngine is the sysmon binary run with e as its /run/docker.sock.
func sysmonWithEngine(t *testing.T, bin string, e *engine) string {
	t.Helper()
	requireDevcontainer(t)
	return sandbox{setup: withRuntimeSocket(e.socket)}.wrap(t, bin)
}

// imagesPayload is `sysmon images --json`.
type imagesPayload struct {
	Available         bool   `json:"available"`
	Engine            string `json:"engine"`
	Version           string `json:"version"`
	SocketPath        string `json:"socket_path"`
	RootDir           string `json:"root_dir"`
	StorageDriver     string `json:"storage_driver"`
	BackingFilesystem string `json:"backing_filesystem"`
	ImagesTotal       int    `json:"images_total"`
	ContainersRunning int    `json:"containers_running"`
	LayersBytes       uint64 `json:"layers_bytes"`
	ReclaimableBytes  uint64 `json:"reclaimable_bytes"`
	DanglingImages    int    `json:"dangling_images"`
	UnusedImages      int    `json:"unused_images"`
	VolumesCount      int    `json:"volumes_count"`
	VolumesUnused     int    `json:"volumes_unused"`
	VolumesBytes      uint64 `json:"volumes_bytes"`
	BuildCacheEntries int    `json:"build_cache_entries"`
	BuildCacheBytes   uint64 `json:"build_cache_bytes"`
	ImagesFiltered    int    `json:"images_filtered"`
	Images            []struct {
		ShortID   string   `json:"short_id"`
		Tags      []string `json:"tags"`
		SizeBytes uint64   `json:"size_bytes"`
		InUse     bool     `json:"in_use"`
		Dangling  bool     `json:"dangling"`
	} `json:"images"`
	ContainerRuntime struct {
		State string `json:"state"`
	} `json:"container_runtime"`
}

// images runs `sysmon images` against e with args, requiring success.
func images(t *testing.T, e *engine, args ...string) imagesPayload {
	t.Helper()
	r := harness.Run(t, sysmonWithEngine(t, harness.Bins.Sysmon, e), nil, append([]string{"images", "--json"}, args...)...)
	if r.Code != 0 {
		t.Fatalf("sysmon images --json %v exited %d:\n%s", args, r.Code, r.Output())
	}
	var p imagesPayload
	harness.MustParseJSON(t, r.Stdout, &p)
	return p
}

// imageTags lists the first tag of each image, "<none>" for a dangling one.
func imageTags(p imagesPayload) []string {
	tags := make([]string, 0, len(p.Images))
	for _, img := range p.Images {
		if len(img.Tags) == 0 {
			tags = append(tags, "<none>")
			continue
		}
		tags = append(tags, img.Tags[0])
	}
	return tags
}

// TestImages_ReportsTheEnginesInventory verifies `sysmon images` reads the
// daemon's identity and storage from /info and the inventory from
// /system/df, and says what a prune could reclaim.
func TestImages_ReportsTheEnginesInventory(t *testing.T) {
	e := startEngine(t)

	p := images(t, e)

	// /var/run/docker.sock is the first place looked, and /var/run is /run.
	if !p.Available || p.Engine != "docker" || p.Version != "27.3.1" || p.SocketPath != "/var/run/docker.sock" {
		t.Errorf("engine = available %v, %q %q at %q; want docker 27.3.1 at /var/run/docker.sock", p.Available, p.Engine, p.Version, p.SocketPath)
	}
	if p.StorageDriver != "overlay2" || p.BackingFilesystem != "extfs" || p.RootDir != "/var/lib/docker" {
		t.Errorf("storage = %q on %q at %q, want overlay2 on extfs at /var/lib/docker", p.StorageDriver, p.BackingFilesystem, p.RootDir)
	}
	if p.ImagesTotal != 4 || p.ContainersRunning != 2 {
		t.Errorf("counts = %d images, %d running; want 4 and 2", p.ImagesTotal, p.ContainersRunning)
	}
	// Largest first.
	if got, want := strings.Join(imageTags(p), ","), "postgres:16,nginx:1.27,<none>,busybox:1.36"; got != want {
		t.Errorf("images = %s, want %s", got, want)
	}
	if p.DanglingImages != 1 || p.UnusedImages != 2 || p.ReclaimableBytes != 124000000 {
		t.Errorf("reclaim = %d dangling, %d unused, %d bytes; want 1, 2, 124000000", p.DanglingImages, p.UnusedImages, p.ReclaimableBytes)
	}
	if p.VolumesCount != 2 || p.VolumesUnused != 1 || p.VolumesBytes != 850000000 {
		t.Errorf("volumes = %d (%d unused, %d bytes), want 2 (1, 850000000)", p.VolumesCount, p.VolumesUnused, p.VolumesBytes)
	}
	if p.BuildCacheEntries != 2 || p.BuildCacheBytes != 100000000 {
		t.Errorf("build cache = %d entries, %d bytes; want 2 and 100000000", p.BuildCacheEntries, p.BuildCacheBytes)
	}
	if p.Images[2].ShortID != "cccc1111dddd" || !p.Images[2].Dangling || p.Images[2].InUse {
		t.Errorf("the dangling image = %+v, want short ID cccc1111dddd, dangling, unused", p.Images[2])
	}
}

// TestImages_FilterSortAndLimit verifies the flags narrow and order the list,
// and that a shortened list says how many it left out.
func TestImages_FilterSortAndLimit(t *testing.T) {
	e := startEngine(t)

	tests := []struct {
		args     []string
		want     string
		filtered int
	}{
		{[]string{"--unused"}, "<none>,busybox:1.36", 2},
		{[]string{"--filter", "POSTGRES"}, "postgres:16", 3},
		{[]string{"--filter", "dddd1111"}, "busybox:1.36", 3},
		{[]string{"--sort", "tag"}, "busybox:1.36,nginx:1.27,postgres:16,<none>", 0},
		{[]string{"--sort", "created"}, "busybox:1.36,nginx:1.27,postgres:16,<none>", 0},
		{[]string{"--sort", "used"}, "nginx:1.27,postgres:16,<none>,busybox:1.36", 0},
		{[]string{"--sort", "shared"}, "postgres:16,nginx:1.27,<none>,busybox:1.36", 0},
		{[]string{"--sort", "id"}, "nginx:1.27,postgres:16,<none>,busybox:1.36", 0},
		{[]string{"--limit", "1"}, "postgres:16", 3},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			p := images(t, e, tt.args...)
			if got := strings.Join(imageTags(p), ","); got != tt.want {
				t.Errorf("images = %s, want %s", got, tt.want)
			}
			if p.ImagesFiltered != tt.filtered {
				t.Errorf("images_filtered = %d, want %d", p.ImagesFiltered, tt.filtered)
			}
		})
	}
}

// TestImages_RejectsAnUnknownSort verifies a sort key the command does not
// know fails with the valid keys named, instead of printing some order.
func TestImages_RejectsAnUnknownSort(t *testing.T) {
	e := startEngine(t)

	r := harness.Run(t, sysmonWithEngine(t, harness.Bins.Sysmon, e), nil, "images", "--sort", "age")

	if r.Code == 0 || !strings.Contains(r.Output(), "size, shared, tag, id, created, used") {
		t.Errorf("sysmon images --sort age exited %d:\n%s\nwant a failure naming the valid keys", r.Code, r.Output())
	}
}

// TestImages_TextTable verifies the human-readable report: the engine, the
// table of images and the reclaimable space.
func TestImages_TextTable(t *testing.T) {
	e := startEngine(t)

	r := harness.Run(t, sysmonWithEngine(t, harness.Bins.Sysmon, e), nil, "images", "--limit", "2")

	if r.Code != 0 {
		t.Fatalf("sysmon images exited %d:\n%s", r.Code, r.Output())
	}
	for _, want := range []string{"docker", "27.3.1", "overlay2", "postgres:16", "nginx:1.27"} {
		if !strings.Contains(r.Stdout, want) {
			t.Errorf("sysmon images does not show %q:\n%s", want, r.Stdout)
		}
	}
	if strings.Contains(r.Stdout, "busybox") {
		t.Errorf("--limit 2 still listed busybox:\n%s", r.Stdout)
	}
}

// TestImages_ReportsAFailedInventory verifies a daemon that answers /info but
// fails /system/df makes the command fail, rather than print an inventory of
// no images with zero sizes as if it were true.
func TestImages_ReportsAFailedInventory(t *testing.T) {
	e := startEngine(t)
	e.mu.Lock()
	e.df = http.StatusInternalServerError
	e.mu.Unlock()

	r := harness.Run(t, sysmonWithEngine(t, harness.Bins.Sysmon, e), nil, "images")

	if r.Code == 0 {
		t.Fatalf("sysmon images exited 0 with /system/df failing:\n%s", r.Output())
	}
	if !strings.Contains(r.Output(), "/system/df returned status 500") {
		t.Errorf("the failure does not name the query and status:\n%s", r.Output())
	}
}

// TestImages_DaemonThatRefusesInfoIsUnavailable verifies a socket whose daemon
// answers /info with an error is reported as no runtime, and no disk usage is
// asked for.
func TestImages_DaemonThatRefusesInfoIsUnavailable(t *testing.T) {
	e := startEngine(t)
	e.mu.Lock()
	e.info = http.StatusServiceUnavailable
	e.mu.Unlock()

	p := images(t, e)

	if p.Available || len(p.Images) != 0 {
		t.Errorf("runtime = available %v with %d images, want unavailable and none", p.Available, len(p.Images))
	}
	if e.asked("/system/df") {
		t.Error("disk usage was queried from a daemon whose /info failed")
	}
}

// TestImages_NoRuntimeSocket verifies a host with no runtime socket reports
// the runtime unavailable and says why, rather than failing.
func TestImages_NoRuntimeSocket(t *testing.T) {
	requireDevcontainer(t)
	bin := sandbox{setup: []string{"mount -t tmpfs tmpfs /run"}}.wrap(t, harness.Bins.Sysmon)

	r := harness.Run(t, bin, nil, "images")

	if r.Code != 0 {
		t.Fatalf("sysmon images with no runtime exited %d:\n%s", r.Code, r.Output())
	}
	if !strings.Contains(r.Stdout, "No container runtime API reachable.") {
		t.Errorf("sysmon images does not say no runtime answered:\n%s", r.Output())
	}
}

// TestServe_InventoryInTheSnapshot verifies the server, which queries the
// runtime on its slow tier, carries the engine and, once the background disk
// usage query lands, the image inventory in every snapshot.
func TestServe_InventoryInTheSnapshot(t *testing.T) {
	e := startEngine(t)
	s := harness.Serve(t, harness.ServeOptions{Bin: sysmonWithEngine(t, harness.Bins.Sysmon, e)})

	var snap struct {
		Virt struct {
			Runtime struct {
				Available bool `json:"available"`
				Images    []struct {
					Tags []string `json:"tags"`
				} `json:"images"`
			} `json:"runtime"`
		} `json:"virtualization"`
	}
	harness.GetJSON(t, s.URL("/api/snapshot"), &snap)
	if !snap.Virt.Runtime.Available {
		t.Fatal("the server's snapshot has no runtime although its socket answers")
	}
	awaitSnapshot(t, s, "the image inventory to reach the snapshot", func(raw []byte) bool {
		if err := json.Unmarshal(raw, &snap); err != nil {
			t.Fatalf("decode the snapshot: %v", err)
		}
		return len(snap.Virt.Runtime.Images) == 4
	})
}

// TestImages_UnknownSizesAndOverlappingLayers verifies the two ways a
// daemon's own numbers can mislead: an image whose shared size the daemon
// has not computed (-1) is reported as 0 rather than as a huge unsigned
// number, and reclaimable space -- a sum of per-image sizes that share layers
// -- is capped at the deduplicated total the layers actually take.
func TestImages_UnknownSizesAndOverlappingLayers(t *testing.T) {
	e := startEngine(t)
	e.mu.Lock()
	e.dfBody = `{"LayersSize": 150000000, "Images": [
		{"Id": "sha256:1111aaaa2222bbbb3333cccc4444dddd5555eeee6666ffff7777aaaa8888bbbb", "RepoTags": ["app:1"], "Size": 120000000, "SharedSize": -1, "Containers": 0},
		{"Id": "sha256:2222aaaa3333bbbb4444cccc5555dddd6666eeee7777ffff8888aaaa9999bbbb", "RepoTags": ["app:2"], "Size": 110000000, "SharedSize": -1, "Containers": 0}
	]}`
	e.mu.Unlock()

	r := harness.Run(t, sysmonWithEngine(t, harness.Bins.Sysmon, e), nil, "images", "--json")
	if r.Code != 0 {
		t.Fatalf("sysmon images --json exited %d:\n%s", r.Code, r.Output())
	}
	var p struct {
		ReclaimableBytes uint64 `json:"reclaimable_bytes"`
		Images           []struct {
			SharedSizeBytes uint64 `json:"shared_size_bytes"`
		} `json:"images"`
	}
	harness.MustParseJSON(t, r.Stdout, &p)
	if p.ReclaimableBytes != 150000000 {
		t.Errorf("reclaimable = %d, want the layers' own 150000000", p.ReclaimableBytes)
	}
	for i, img := range p.Images {
		if img.SharedSizeBytes != 0 {
			t.Errorf("image %d shared size = %d, want 0 for a size the daemon did not compute", i, img.SharedSizeBytes)
		}
	}
}

// TestImages_ADaemonAnsweringNonsense verifies a socket whose daemon answers
// /info with something that is not its JSON is treated as no runtime.
func TestImages_ADaemonAnsweringNonsense(t *testing.T) {
	e := startEngine(t)
	e.mu.Lock()
	e.infoBody = "<html>proxy error</html>"
	e.mu.Unlock()

	p := images(t, e)

	if p.Available {
		t.Error("a daemon whose /info is not JSON was reported as an available runtime")
	}
}
