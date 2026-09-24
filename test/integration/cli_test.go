//go:build integration

// Package integration contains end-to-end CLI integration tests for the sysmon
// binary. Tests build the binary once via TestMain and exercise every subcommand,
// asserting on both the human-readable text output and the --json output.
package integration

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/internal/await"
)

// binaryPath is the path to the sysmon binary built once in TestMain.
var binaryPath string

// TestMain builds the sysmon binary into a temporary directory before running
// any tests, and removes it when the suite finishes.
func TestMain(m *testing.M) {
	tmpDir, err := os.MkdirTemp("", "sysmon-integration-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create temp dir: %v\n", err)
		os.Exit(1)
	}
	binaryPath = filepath.Join(tmpDir, "sysmon")

	// Build without the desktop tag so the binary uses the stub GUI path and no
	// Wails runtime is required. CGO must stay enabled: pkg/collector imports
	// NVIDIA go-nvml, which does not compile with CGO_ENABLED=0.
	cmd := exec.Command("go", "build", "-o", binaryPath, "../../cmd/sysmon")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build failed: %v\n%s\n", err, out)
		os.RemoveAll(tmpDir)
		os.Exit(1)
	}

	code := m.Run()
	os.RemoveAll(tmpDir)
	os.Exit(code)
}

// runCmd executes the sysmon binary with the given args, suppressing any
// display-server environment variables so the binary always takes the CLI path.
// It returns the combined stdout+stderr output and the process exit code.
func runCmd(t *testing.T, args ...string) (string, int) {
	t.Helper()
	stdout, stderr, code := runCmdStreams(t, args...)
	return stdout + stderr, code
}

// runCmdStdout is like runCmd but returns only stdout. Use it for --json
// assertions: diagnostics such as "NVML init failed" are written to stderr, and
// merging the streams would corrupt otherwise valid JSON.
func runCmdStdout(t *testing.T, args ...string) (string, int) {
	t.Helper()
	stdout, _, code := runCmdStreams(t, args...)
	return stdout, code
}

// runCmdStreams executes the sysmon binary with stdout and stderr captured
// separately, returning both along with the process exit code.
func runCmdStreams(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(binaryPath, args...)
	// Clear display vars to prevent the root command from attempting to launch
	// the GUI and ensure the overview/subcommand path is exercised.
	cmd.Env = append(os.Environ(), "DISPLAY=", "WAYLAND_DISPLAY=")

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	exitCode := 0
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		}
	}
	return stdout.String(), stderr.String(), exitCode
}

// mustParseJSON unmarshals src into dst, failing the test immediately if
// parsing fails.
func mustParseJSON(t *testing.T, src string, dst any) {
	t.Helper()
	if err := json.Unmarshal([]byte(src), dst); err != nil {
		t.Fatalf("JSON parse error: %v\nraw output:\n%s", err, src)
	}
}

// closeOnCleanup closes c when the test ends, failing the test if the close
// does. what names c in the failure.
func closeOnCleanup(t *testing.T, what string, c io.Closer) {
	t.Helper()
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Errorf("closing %s: %v", what, err)
		}
	})
}

// waitForPort polls addr (e.g. "localhost:18080") until a TCP connection
// succeeds, failing the test if it has not within await.Timeout. The listener
// belongs to another process, so polling it is the only way to observe it.
func waitForPort(t *testing.T, addr string) {
	t.Helper()
	var dialer net.Dialer
	await.Until(t, "sysmon serve to accept connections on "+addr, func(ctx context.Context) bool {
		conn, err := dialer.DialContext(ctx, "tcp", addr)
		if err != nil {
			return false
		}
		if err := conn.Close(); err != nil {
			t.Errorf("closing the probe connection to %s: %v", addr, err)
		}
		return true
	})
}

// waitForSnapshot polls GET /api/snapshot until the server responds with HTTP
// 200, indicating the monitor has completed its first collection cycle, and
// fails the test if it has not within await.Timeout.
func waitForSnapshot(t *testing.T, url string) {
	t.Helper()
	await.Until(t, "GET "+url+" to answer 200", func(ctx context.Context) bool {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			t.Fatalf("building the request for %s: %v", url, err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return false
		}
		if err := resp.Body.Close(); err != nil {
			t.Errorf("closing the %s response: %v", url, err)
		}
		return resp.StatusCode == http.StatusOK
	})
}

// ── Overview ─────────────────────────────────────────────────────────────────

func TestCLI_Overview(t *testing.T) {
	out, code := runCmd(t, "overview")

	if code != 0 {
		t.Fatalf("expected exit 0, got %d; output:\n%s", code, out)
	}
	if !strings.Contains(out, "System Monitor") {
		t.Errorf("output missing 'System Monitor' header; got:\n%s", out)
	}
	// The hostname line must be non-empty.
	if !strings.Contains(out, "Host:") {
		t.Errorf("output missing 'Host:' line; got:\n%s", out)
	}
}

func TestCLI_Overview_ContainsAllSections(t *testing.T) {
	out, code := runCmd(t, "overview")
	if code != 0 {
		t.Fatalf("expected exit 0, got %d; output:\n%s", code, out)
	}

	sections := []string{"CPU:", "Memory:", "Storage:", "Network:", "Processes:"}
	for _, sec := range sections {
		if !strings.Contains(out, sec) {
			t.Errorf("overview output missing section %q; got:\n%s", sec, out)
		}
	}
}

func TestCLI_Overview_JSON(t *testing.T) {
	out, code := runCmdStdout(t, "overview", "--json")
	if code != 0 {
		t.Fatalf("expected exit 0, got %d; output:\n%s", code, out)
	}

	var snap struct {
		Timestamp time.Time `json:"timestamp"`
		Host      struct {
			Hostname string `json:"hostname"`
		} `json:"host"`
		CPUSummary struct {
			TotalCores int `json:"total_cores"`
		} `json:"cpu_summary"`
		Memory struct {
			TotalBytes uint64 `json:"total_bytes"`
		} `json:"memory"`
		CPUs     []json.RawMessage `json:"cpus"`
		Networks []json.RawMessage `json:"networks"`
	}
	mustParseJSON(t, out, &snap)

	if snap.Timestamp.IsZero() {
		t.Error("overview JSON: timestamp is zero")
	}
	if snap.Host.Hostname == "" {
		t.Error("overview JSON: hostname is empty")
	}
	if snap.CPUSummary.TotalCores <= 0 {
		t.Errorf("overview JSON: total_cores = %d, want > 0", snap.CPUSummary.TotalCores)
	}
	if snap.Memory.TotalBytes == 0 {
		t.Error("overview JSON: memory.total_bytes is 0")
	}
}

// ── Host ──────────────────────────────────────────────────────────────────────

func TestCLI_Host(t *testing.T) {
	out, code := runCmd(t, "host")
	if code != 0 {
		t.Fatalf("expected exit 0, got %d; output:\n%s", code, out)
	}

	for _, want := range []string{"Host Information", "Hostname:", "Kernel:", "Uptime:"} {
		if !strings.Contains(out, want) {
			t.Errorf("host output missing %q; got:\n%s", want, out)
		}
	}
}

func TestCLI_Host_JSON(t *testing.T) {
	out, code := runCmdStdout(t, "host", "--json")
	if code != 0 {
		t.Fatalf("expected exit 0, got %d; output:\n%s", code, out)
	}

	var host struct {
		Hostname      string `json:"hostname"`
		KernelVersion string `json:"kernel_version"`
		Uptime        uint64 `json:"uptime"`
		OS            string `json:"os"`
	}
	mustParseJSON(t, out, &host)

	if host.Hostname == "" {
		t.Error("host JSON: hostname is empty")
	}
	if host.KernelVersion == "" {
		t.Error("host JSON: kernel_version is empty")
	}
	if host.Uptime == 0 {
		t.Error("host JSON: uptime is 0")
	}
}

// ── CPU ───────────────────────────────────────────────────────────────────────

func TestCLI_CPU(t *testing.T) {
	out, code := runCmd(t, "cpu")
	if code != 0 {
		t.Fatalf("expected exit 0, got %d; output:\n%s", code, out)
	}

	for _, want := range []string{"CPU Information", "Model:", "Vendor:", "Per-Core Usage:", "Core  0:"} {
		if !strings.Contains(out, want) {
			t.Errorf("cpu output missing %q; got:\n%s", want, out)
		}
	}
}

func TestCLI_CPU_ContainsCoreCount(t *testing.T) {
	out, code := runCmd(t, "cpu")
	if code != 0 {
		t.Fatalf("expected exit 0, got %d; output:\n%s", code, out)
	}

	// The "Total:" line shows "<N> Cores / <M> Threads".
	if !strings.Contains(out, "Cores") {
		t.Errorf("cpu output missing 'Cores' count; got:\n%s", out)
	}
	if !strings.Contains(out, "Threads") {
		t.Errorf("cpu output missing 'Threads' count; got:\n%s", out)
	}
}

func TestCLI_CPU_JSON(t *testing.T) {
	out, code := runCmdStdout(t, "cpu", "--json")
	if code != 0 {
		t.Fatalf("expected exit 0, got %d; output:\n%s", code, out)
	}

	var resp struct {
		Summary struct {
			TotalCores int `json:"total_cores"`
		} `json:"cpu_summary"`
		CPUs []json.RawMessage `json:"cpus"`
	}
	mustParseJSON(t, out, &resp)

	if resp.Summary.TotalCores <= 0 {
		t.Errorf("cpu JSON: cpu_summary.total_cores = %d, want > 0", resp.Summary.TotalCores)
	}
	if len(resp.CPUs) == 0 {
		t.Error("cpu JSON: cpus array is empty")
	}
}

// ── Memory ───────────────────────────────────────────────────────────────────

func TestCLI_Memory(t *testing.T) {
	out, code := runCmd(t, "memory")
	if code != 0 {
		t.Fatalf("expected exit 0, got %d; output:\n%s", code, out)
	}

	for _, want := range []string{"Memory Information", "RAM:", "Available:"} {
		if !strings.Contains(out, want) {
			t.Errorf("memory output missing %q; got:\n%s", want, out)
		}
	}
}

func TestCLI_Memory_DIMMs(t *testing.T) {
	out, code := runCmd(t, "memory")
	if code != 0 {
		t.Fatalf("expected exit 0, got %d; output:\n%s", code, out)
	}

	// Either DIMM detail or at least the section heading should appear.
	// Some CI environments lack dmidecode access, so we accept either case.
	hasDIMMSection := strings.Contains(out, "Memory Modules")
	hasDIMMData := strings.Contains(out, "Manufacturer") || strings.Contains(out, "MT/s")
	if !hasDIMMSection && !hasDIMMData {
		// Not fatal: dmidecode may be unavailable; assert at least no crash.
		t.Logf("memory output contains no DIMM info (dmidecode may be unavailable); output:\n%s", out)
	}
}

func TestCLI_Memory_JSON(t *testing.T) {
	out, code := runCmdStdout(t, "memory", "--json")
	if code != 0 {
		t.Fatalf("expected exit 0, got %d; output:\n%s", code, out)
	}

	var mem struct {
		TotalBytes     uint64  `json:"total_bytes"`
		UsedBytes      uint64  `json:"used_bytes"`
		AvailableBytes uint64  `json:"available_bytes"`
		UsedPercent    float64 `json:"used_percent"`
	}
	mustParseJSON(t, out, &mem)

	if mem.TotalBytes == 0 {
		t.Error("memory JSON: total_bytes is 0")
	}
	if mem.AvailableBytes == 0 {
		t.Error("memory JSON: available_bytes is 0")
	}
}

// ── Storage ───────────────────────────────────────────────────────────────────

func TestCLI_Storage(t *testing.T) {
	out, code := runCmd(t, "storage")
	if code != 0 {
		t.Fatalf("expected exit 0, got %d; output:\n%s", code, out)
	}

	if !strings.Contains(out, "Storage Information") {
		t.Errorf("storage output missing 'Storage Information'; got:\n%s", out)
	}
	if !strings.Contains(out, "/dev/") {
		t.Errorf("storage output missing '/dev/' device path; got:\n%s", out)
	}
}

func TestCLI_Storage_SMART(t *testing.T) {
	out, code := runCmd(t, "storage")
	if code != 0 {
		t.Fatalf("expected exit 0, got %d; output:\n%s", code, out)
	}

	// SMART output appears as "Healthy ✓" or is absent when smartmontools is
	// unavailable. Either state is valid; the test ensures the command exits
	// cleanly and does not produce garbage output.
	hasHealthy := strings.Contains(out, "Healthy")
	hasNA := strings.Contains(out, "SMART N/A") || strings.Contains(out, "SMART:")
	if !hasHealthy && !hasNA {
		t.Logf("storage SMART field not found in output (smartmontools may be unavailable); output:\n%s", out)
	}
}

func TestCLI_Storage_NVMeHealth(t *testing.T) {
	out, code := runCmd(t, "storage")
	if code != 0 {
		t.Fatalf("expected exit 0, got %d; output:\n%s", code, out)
	}

	// NVMe drives will show Wear Level / Spare / Firmware fields.
	// Non-NVMe or environments without smartctl will show firmware or model.
	// Assert at minimum that the output is non-trivial (more than the header).
	if len(strings.TrimSpace(out)) < len("Storage Information") {
		t.Errorf("storage output appears truncated; got:\n%s", out)
	}
}

func TestCLI_Storage_JSON(t *testing.T) {
	out, code := runCmdStdout(t, "storage", "--json")
	if code != 0 {
		t.Fatalf("expected exit 0, got %d; output:\n%s", code, out)
	}

	var disks []struct {
		Name      string `json:"name"`
		SizeBytes uint64 `json:"size_bytes"`
	}
	mustParseJSON(t, out, &disks)

	if len(disks) == 0 {
		t.Fatal("storage JSON: disks array is empty")
	}
	for i, d := range disks {
		if d.Name == "" {
			t.Errorf("storage JSON: disks[%d].name is empty", i)
		}
		if d.SizeBytes == 0 {
			t.Errorf("storage JSON: disks[%d].size_bytes is 0", i)
		}
	}
}

// ── Network ───────────────────────────────────────────────────────────────────

func TestCLI_Network(t *testing.T) {
	out, code := runCmd(t, "network")
	if code != 0 {
		t.Fatalf("expected exit 0, got %d; output:\n%s", code, out)
	}

	if !strings.Contains(out, "Network Interfaces") {
		t.Errorf("network output missing 'Network Interfaces'; got:\n%s", out)
	}
	// At least one interface name must appear after the header.
	lines := strings.Split(out, "\n")
	hasInterface := false
	for _, l := range lines[2:] {
		l = strings.TrimSpace(l)
		if l != "" && strings.Contains(l, "[") {
			hasInterface = true
			break
		}
	}
	if !hasInterface {
		t.Errorf("network output contains no interface entries; got:\n%s", out)
	}
}

func TestCLI_Network_Loopback(t *testing.T) {
	out, code := runCmd(t, "network")
	if code != 0 {
		t.Fatalf("expected exit 0, got %d; output:\n%s", code, out)
	}

	if !strings.Contains(out, "lo ") {
		t.Errorf("network output missing loopback interface 'lo'; got:\n%s", out)
	}
	// The interface kind now comes from sysfs, so the loopback device is
	// labelled "loopback" rather than the old physical/virtual split.
	if !strings.Contains(out, "loopback") {
		t.Errorf("network output missing 'loopback' kind label; got:\n%s", out)
	}
}

func TestCLI_Network_JSON(t *testing.T) {
	out, code := runCmdStdout(t, "network", "--json")
	if code != 0 {
		t.Fatalf("expected exit 0, got %d; output:\n%s", code, out)
	}

	var networks []struct {
		Name       string `json:"name"`
		IsLoopback bool   `json:"is_loopback"`
	}
	mustParseJSON(t, out, &networks)

	if len(networks) == 0 {
		t.Fatal("network JSON: networks array is empty")
	}

	hasLoopback := false
	for _, n := range networks {
		if n.Name == "" {
			t.Error("network JSON: interface with empty name")
		}
		if n.IsLoopback {
			hasLoopback = true
		}
	}
	if !hasLoopback {
		t.Error("network JSON: no loopback interface found")
	}
}

// ── Version ───────────────────────────────────────────────────────────────────

func TestCLI_Version(t *testing.T) {
	out, code := runCmd(t, "version")
	if code != 0 {
		t.Fatalf("expected exit 0, got %d; output:\n%s", code, out)
	}

	if !strings.Contains(out, "sysmon") {
		t.Errorf("version output missing 'sysmon'; got:\n%s", out)
	}
}

func TestCLI_Version_ExitCode(t *testing.T) {
	_, code := runCmd(t, "version")
	if code != 0 {
		t.Errorf("version: expected exit 0, got %d", code)
	}
}

func TestCLI_Version_JSON(t *testing.T) {
	out, code := runCmdStdout(t, "version", "--json")
	if code != 0 {
		t.Fatalf("expected exit 0, got %d; output:\n%s", code, out)
	}

	var v struct {
		Version   string `json:"version"`
		GitCommit string `json:"gitCommit"`
		BuildDate string `json:"buildDate"`
	}
	mustParseJSON(t, out, &v)

	if v.Version == "" {
		t.Error("version JSON: version field is empty")
	}
	if v.GitCommit == "" {
		t.Error("version JSON: gitCommit field is empty")
	}
}

// ── Serve ─────────────────────────────────────────────────────────────────────

// serverListeningMsg is the log message the serve command emits once its
// listener is bound. It carries the bound address, which is the only way to
// learn the port when the server was started on port 0.
const serverListeningMsg = "server listening"

// startServer launches `sysmon serve` on a kernel-assigned port and returns the
// address it bound, ready to serve. The process is killed by a t.Cleanup.
//
// The port is 0 rather than a constant on purpose. A fixed port made two CI
// jobs on one runner collide by construction -- whichever bound second failed,
// on a machine where nothing was wrong (issue #62). Asking the kernel for a
// port cannot collide, and asking the server which port it got is exact,
// where guessing a free one in advance is a race with every other process on
// the host.
func startServer(t *testing.T) string {
	t.Helper()
	cmd := exec.Command(binaryPath, "serve", "--addr", "127.0.0.1:0", "--interval", "200", "--log-format", "json")
	cmd.Env = append(os.Environ(), "DISPLAY=", "WAYLAND_DISPLAY=")

	logs, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("failed to capture sysmon serve logs: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start sysmon serve: %v", err)
	}
	found, drained := watchLogs(logs)
	t.Cleanup(func() {
		if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Errorf("killing sysmon serve: %v", err)
		}
		// The log reader finishes before Wait, which closes the pipe: reading
		// from it after that is the misuse os/exec documents. The process is
		// dead, so its end of the pipe is closed and the reader meets EOF,
		// which io.Copy reports as success -- any error here is a real one.
		if err := await.Recv(t, "the sysmon serve log reader to finish", drained); err != nil {
			t.Errorf("reading the sysmon serve logs: %v", err)
		}
		// Killed, it exits on the signal, which Wait reports as an ExitError;
		// anything else is a failure to reap it.
		var exited *exec.ExitError
		if err := cmd.Wait(); err != nil && !errors.As(err, &exited) {
			t.Errorf("reaping sysmon serve: %v", err)
		}
	})

	addr := await.Recv(t, fmt.Sprintf("sysmon serve to report %q", serverListeningMsg), found)
	if addr == "" {
		t.Fatalf("sysmon serve's log ended without reporting %q", serverListeningMsg)
	}
	waitForPort(t, addr)
	// Wait for the monitor to complete its first collection cycle so that
	// /api/snapshot returns 200 rather than 503.
	waitForSnapshot(t, "http://"+addr+"/api/snapshot")
	return addr
}

// watchLogs reads the server's JSON log stream in the background. found
// delivers the address its listener bound, once reported, and is closed empty
// if the stream ends without it. The stream is then
// drained to its end, so a server that logs steadily is never blocked by a
// full pipe, and drained delivers how reading it ended: nil at end of stream,
// otherwise the error that stopped it.
func watchLogs(logs io.Reader) (found <-chan string, drained <-chan error) {
	addr := make(chan string, 1)
	done := make(chan error, 1)

	go func() {
		scanner := bufio.NewScanner(logs)
		for scanner.Scan() {
			var entry struct {
				Msg  string `json:"msg"`
				Addr string `json:"addr"`
			}
			if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
				continue
			}
			if entry.Msg == serverListeningMsg && entry.Addr != "" {
				addr <- entry.Addr
				_, err := io.Copy(io.Discard, logs)
				done <- err
				return
			}
		}
		// The stream ended without the address: closing found says so at
		// once, rather than leaving the test to wait out its bound.
		close(addr)
		done <- scanner.Err()
	}()

	return addr, done
}

func TestCLI_Serve_StartsAndStops(t *testing.T) {
	addr := startServer(t)

	resp, err := http.Get("http://" + addr + "/api/snapshot")
	if err != nil {
		t.Fatalf("GET /api/snapshot: %v", err)
	}
	closeOnCleanup(t, "the response body", resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /api/snapshot: expected 200, got %d", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "application/json") {
		t.Errorf("GET /api/snapshot: expected Content-Type application/json, got %q", ct)
	}
}

func TestCLI_Serve_APIReturnsSnapshot(t *testing.T) {
	addr := startServer(t)

	resp, err := http.Get("http://" + addr + "/api/snapshot")
	if err != nil {
		t.Fatalf("GET /api/snapshot: %v", err)
	}
	closeOnCleanup(t, "the response body", resp.Body)

	var snap struct {
		Timestamp time.Time `json:"timestamp"`
		Host      struct {
			Hostname string `json:"hostname"`
		} `json:"host"`
		CPUSummary struct {
			TotalCores int `json:"total_cores"`
		} `json:"cpu_summary"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}

	if snap.Timestamp.IsZero() {
		t.Error("serve /api/snapshot: timestamp is zero")
	}
	if snap.Host.Hostname == "" {
		t.Error("serve /api/snapshot: hostname is empty")
	}
	if snap.CPUSummary.TotalCores <= 0 {
		t.Errorf("serve /api/snapshot: total_cores = %d, want > 0", snap.CPUSummary.TotalCores)
	}
}

func TestCLI_Serve_EventStream(t *testing.T) {
	addr := startServer(t)

	stream := dialEventStream(t, "http://"+addr+"/api/events", false)

	if ct := stream.contentType; ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}

	ev := stream.nextNamed(t, "snapshot")

	var snap struct {
		Timestamp time.Time `json:"timestamp"`
		Host      struct {
			Hostname string `json:"hostname"`
		} `json:"host"`
	}
	if err := json.Unmarshal([]byte(ev.data), &snap); err != nil {
		t.Fatalf("parse snapshot event: %v\nraw: %s", err, ev.data)
	}

	if snap.Timestamp.IsZero() {
		t.Error("event stream snapshot: timestamp is zero")
	}
	if snap.Host.Hostname == "" {
		t.Error("event stream snapshot: hostname is empty")
	}
}

// TestCLI_Serve_EventStream_Gzip verifies the real binary compresses the
// stream for a client that offers it. Consecutive snapshots barely differ, so
// this is where the bandwidth saving lives.
func TestCLI_Serve_EventStream_Gzip(t *testing.T) {
	addr := startServer(t)

	stream := dialEventStream(t, "http://"+addr+"/api/events", true)

	if stream.encoding != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", stream.encoding)
	}

	ev := stream.nextNamed(t, "snapshot")

	var snap map[string]any
	if err := json.Unmarshal([]byte(ev.data), &snap); err != nil {
		t.Fatalf("parse compressed snapshot event: %v", err)
	}
	if _, ok := snap["timestamp"]; !ok {
		t.Error("compressed snapshot missing timestamp field")
	}
}

// ── GPU ───────────────────────────────────────────────────────────────────────

func TestCLI_GPU(t *testing.T) {
	out, code := runCmd(t, "gpu")
	if code != 0 {
		t.Fatalf("expected exit 0, got %d; output:\n%s", code, out)
	}
	if !strings.Contains(out, "GPU Information") {
		t.Errorf("output missing 'GPU Information' header; got:\n%s", out)
	}
}

func TestCLI_GPU_JSON(t *testing.T) {
	out, code := runCmdStdout(t, "gpu", "--json")
	if code != 0 {
		t.Fatalf("expected exit 0, got %d; output:\n%s", code, out)
	}
	var result struct {
		GPUs []json.RawMessage `json:"gpus"`
	}
	mustParseJSON(t, out, &result)
	// gpus must be a valid JSON array (may be empty if no GPUs on host)
	if result.GPUs == nil {
		t.Error("gpus field is nil; expected a JSON array")
	}
}

func TestCLI_GPU_JSON_Structure(t *testing.T) {
	out, code := runCmdStdout(t, "gpu", "--json")
	if code != 0 {
		t.Fatalf("expected exit 0, got %d; output:\n%s", code, out)
	}
	var result struct {
		GPUs []map[string]interface{} `json:"gpus"`
	}
	mustParseJSON(t, out, &result)
	if len(result.GPUs) == 0 {
		t.Skip("no GPUs detected on this host")
	}

	gpu := result.GPUs[0]
	requiredFields := []string{
		"index", "name", "driver_version",
		"memory_total_mib", "memory_used_mib", "memory_free_mib",
		"gpu_util_percent", "temperature_gpu",
		"ecc_enabled", "pci_bus_id",
	}
	for _, field := range requiredFields {
		if _, ok := gpu[field]; !ok {
			t.Errorf("GPU JSON missing required field %q", field)
		}
	}
}

func TestCLI_GPU_ContainsGPUDetails(t *testing.T) {
	out, code := runCmd(t, "gpu")
	if code != 0 {
		t.Fatalf("expected exit 0, got %d; output:\n%s", code, out)
	}
	if strings.Contains(out, "No GPUs detected") {
		t.Skip("no GPUs detected on this host")
	}
	// runCmd returns stdout+stderr, so the collector's NVML diagnostic lands
	// here. When NVML cannot initialize (no driver library in the container, or
	// a transient init failure on a shared GPU host) the Utilization/Memory/ECC
	// sections below cannot be rendered, and asserting them makes the test pass
	// or fail depending on which runner and GPU state happened to run it. The
	// renderer is covered by pkg/cli unit tests with a synthetic snapshot; this
	// integration case only makes sense when live NVML telemetry is present.
	if strings.Contains(out, "NVML init failed") {
		t.Skip("NVML telemetry unavailable in this environment; cannot assert GPU sections")
	}

	// Separately from whether NVML works at all: the index comes from driver
	// enumeration and is not guaranteed to start at zero -- a host with onboard
	// graphics plus a discrete card can report GPU 1 and GPU 2 and nothing else.
	// Asserting a literal "GPU 0:" made this test pass or fail depending on
	// which machine ran it. What the renderer promises is a numbered section
	// per GPU, so that is what is asserted.
	if !gpuSectionRE.MatchString(out) {
		t.Errorf("gpu output has no numbered GPU section; got:\n%s", out)
	}
	for _, sec := range []string{"Utilization:", "Memory:", "ECC:"} {
		if !strings.Contains(out, sec) {
			t.Errorf("gpu output missing section %q; got:\n%s", sec, out)
		}
	}
}

// gpuSectionRE matches the per-GPU heading the renderer emits, for any index.
var gpuSectionRE = regexp.MustCompile(`GPU \d+:`)

// ── Rate control over REST ────────────────────────────────────────────────────

func TestServer_SetInterval(t *testing.T) {
	addr := startServer(t)

	// A stream opened first must keep working across the rate change.
	stream := dialEventStream(t, "http://"+addr+"/api/events", false)
	stream.nextNamed(t, "snapshot")

	resp, err := http.Post("http://"+addr+"/api/interval", "application/json",
		strings.NewReader(`{"interval_ms":1000}`)) //nolint:noctx
	if err != nil {
		t.Fatalf("POST /api/interval: %v", err)
	}
	closeOnCleanup(t, "the response body", resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/interval status = %d, want 200", resp.StatusCode)
	}

	var got struct {
		IntervalMS int `json:"interval_ms"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode interval response: %v", err)
	}
	if got.IntervalMS != 1000 {
		t.Errorf("interval_ms = %d, want 1000", got.IntervalMS)
	}

	// The already-open stream must still deliver after the change.
	ev := stream.nextNamed(t, "snapshot")
	var snap map[string]any
	if err := json.Unmarshal([]byte(ev.data), &snap); err != nil {
		t.Fatalf("parse snapshot after interval change: %v", err)
	}
	if _, ok := snap["timestamp"]; !ok {
		t.Error("snapshot missing timestamp field after interval change")
	}
}

func TestServer_SetInterval_Disallowed(t *testing.T) {
	addr := startServer(t)

	resp, err := http.Post("http://"+addr+"/api/interval", "application/json",
		strings.NewReader(`{"interval_ms":999}`)) //nolint:noctx
	if err != nil {
		t.Fatalf("POST /api/interval: %v", err)
	}
	closeOnCleanup(t, "the response body", resp.Body)

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for a disallowed interval", resp.StatusCode)
	}

	// The server must still be serving normally afterwards.
	stream := dialEventStream(t, "http://"+addr+"/api/events", false)
	stream.nextNamed(t, "snapshot")
}

func TestServer_GetInterval(t *testing.T) {
	addr := startServer(t)

	resp, err := http.Get("http://" + addr + "/api/interval") //nolint:noctx
	if err != nil {
		t.Fatalf("GET /api/interval: %v", err)
	}
	closeOnCleanup(t, "the response body", resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var got struct {
		IntervalMS int `json:"interval_ms"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.IntervalMS <= 0 {
		t.Errorf("interval_ms = %d, want a positive interval", got.IntervalMS)
	}
}

// TestServer_WebSocketRouteRetired is the migration regression test: the old
// endpoint must not stream, so a stale client fails loudly.
func TestServer_WebSocketRouteRetired(t *testing.T) {
	addr := startServer(t)

	resp, err := http.Get("http://" + addr + "/ws") //nolint:noctx
	if err != nil {
		t.Fatalf("GET /ws: %v", err)
	}
	closeOnCleanup(t, "the response body", resp.Body)

	if resp.StatusCode == http.StatusSwitchingProtocols {
		t.Error("GET /ws upgraded a connection; the WebSocket route should be gone")
	}
	if ct := resp.Header.Get("Content-Type"); strings.Contains(ct, "event-stream") {
		t.Errorf("GET /ws Content-Type = %q, want no stream on the retired route", ct)
	}
}

// ── Error cases ───────────────────────────────────────────────────────────────

func TestCLI_UnknownCommand(t *testing.T) {
	_, code := runCmd(t, "foobar")
	if code == 0 {
		t.Error("unknown command 'foobar': expected non-zero exit, got 0")
	}
}

func TestCLI_NoDisplay_NoGUI(t *testing.T) {
	// With DISPLAY and WAYLAND_DISPLAY both empty the root command must fall
	// through to the overview path, not attempt to launch a GUI.
	out, code := runCmd(t) // no subcommand, no display
	if code != 0 {
		t.Fatalf("expected exit 0 when DISPLAY is unset, got %d; output:\n%s", code, out)
	}
	if !strings.Contains(out, "System Monitor") {
		t.Errorf("expected overview output, got:\n%s", out)
	}
}

// ── Virtualization ───────────────────────────────────────────────────────────

func TestCLI_Containers(t *testing.T) {
	out, code := runCmd(t, "containers")
	if code != 0 {
		t.Fatalf("expected exit 0, got %d; output:\n%s", code, out)
	}
	if !strings.Contains(out, "Containers") {
		t.Errorf("containers output missing header; got:\n%s", out)
	}
}

func TestCLI_Containers_JSON(t *testing.T) {
	out, code := runCmdStdout(t, "containers", "--json")
	if code != 0 {
		t.Fatalf("expected exit 0, got %d; output:\n%s", code, out)
	}

	var payload struct {
		CgroupVersion string            `json:"cgroup_version"`
		Containers    []json.RawMessage `json:"containers"`
	}
	mustParseJSON(t, out, &payload)

	// Inside the test container the cgroup tree is present, so a version must
	// be reported even when no nested containers are visible.
	if payload.CgroupVersion == "" {
		t.Error("containers JSON: cgroup_version is empty")
	}
}

func TestCLI_Containers_IndexOutOfRange(t *testing.T) {
	out, code := runCmd(t, "containers", "--index", "9999")
	if code == 0 {
		t.Errorf("expected a non-zero exit for an out-of-range index; output:\n%s", out)
	}
	if !strings.Contains(out, "not found") {
		t.Errorf("expected a not-found error; got:\n%s", out)
	}
}

func TestCLI_VMs(t *testing.T) {
	out, code := runCmd(t, "vms")
	if code != 0 {
		t.Fatalf("expected exit 0, got %d; output:\n%s", code, out)
	}
	if !strings.Contains(out, "Virtual Machines") {
		t.Errorf("vms output missing header; got:\n%s", out)
	}
}

func TestCLI_VMs_JSON(t *testing.T) {
	out, code := runCmdStdout(t, "vms", "--json")
	if code != 0 {
		t.Fatalf("expected exit 0, got %d; output:\n%s", code, out)
	}

	var payload struct {
		VMs []json.RawMessage `json:"vms"`
	}
	mustParseJSON(t, out, &payload)
}

func TestCLI_VMs_IndexOutOfRange(t *testing.T) {
	out, code := runCmd(t, "vms", "--index", "9999")
	if code == 0 {
		t.Errorf("expected a non-zero exit for an out-of-range index; output:\n%s", out)
	}
}

// TestCLI_Overview_IncludesVirtualization verifies the snapshot carries the
// virtualization section so the GUI and API clients can rely on it.
func TestCLI_Overview_IncludesVirtualization(t *testing.T) {
	out, code := runCmdStdout(t, "overview", "--json")
	if code != 0 {
		t.Fatalf("expected exit 0, got %d; output:\n%s", code, out)
	}

	var snap struct {
		Virt *struct {
			CgroupVersion string            `json:"cgroup_version"`
			Containers    []json.RawMessage `json:"containers"`
			VMs           []json.RawMessage `json:"vms"`
		} `json:"virtualization"`
	}
	mustParseJSON(t, out, &snap)

	if snap.Virt == nil {
		t.Fatal("overview JSON is missing the virtualization section")
	}
}
