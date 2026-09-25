//go:build integration

// Package harness is what the integration suites share: it builds the real
// go-sysmon binaries and runs them, the way an operator would.
//
// The suites under test/integration run the binaries end to end: every CLI
// subcommand, the server and its streams, fleets of real processes, the
// Prometheus-enabled server build, and the development tools, each against
// in-process stand-ins for the external services they talk to.
//
// It runs in the devcontainer (make integration-test), never on the host.
// TestMain builds every binary once. When SYSMON_COVERDIR is set
// (scripts/pkgtest.sh integration), each is built with every package of the
// module instrumented and writes its coverage there.
package harness

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
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/internal/await"
)

// module is the import path every instrumented binary covers.
const module = "github.com/jeremyhahn/go-sysmon"

// Build shapes of the sysmon binary. Neither has the desktop tags, so no Wails
// runtime is needed. CGO stays enabled: pkg/collector imports NVIDIA go-nvml.
const (
	// ShapeDefault is the binary an embedder of pkg/* gets.
	ShapeDefault = ""
	// ShapeServer is sysmon-server, with remote write and date-range history.
	ShapeServer = "prometheus,nvml"
)

// Bins holds the path of every binary TestMain built.
var Bins struct {
	Sysmon      string
	Server      string
	Checklatest string
	Checkpins   string
	Buildtags   string
	Tsgen       string
}

// RepoRoot is the module root, which the tool commands are run against.
var RepoRoot string

// Main builds the binaries, runs the tests, and removes the build
// directory, returning the exit status.
func Main(m *testing.M) int {
	dir, err := os.MkdirTemp("", "sysmon-integration-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create the build directory: %v\n", err)
		return 1
	}
	code := buildAndRun(m, dir)
	// A build directory that cannot be removed is leaked into the container,
	// which fails the run even when every test passed.
	if err := os.RemoveAll(dir); err != nil {
		fmt.Fprintf(os.Stderr, "remove build dir %s: %v\n", dir, err)
		if code == 0 {
			code = 1
		}
	}
	return code
}

func buildAndRun(m *testing.M, dir string) int {
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolve the module root: %v\n", err)
		return 1
	}
	RepoRoot = root

	// SYSMON_COVERDIR is passed on as GOCOVERDIR to every binary the suite
	// starts. It is not GOCOVERDIR itself because `go test -cover` points the
	// test process's GOCOVERDIR at a directory of its own, removed when the
	// run ends.
	if dir := os.Getenv("SYSMON_COVERDIR"); dir != "" {
		if err := os.Setenv("GOCOVERDIR", dir); err != nil {
			fmt.Fprintf(os.Stderr, "set GOCOVERDIR: %v\n", err)
			return 1
		}
	}

	builds := []struct {
		out  *string
		name string
		pkg  string
		tags string
	}{
		{&Bins.Sysmon, "sysmon", "./cmd/sysmon", ShapeDefault},
		{&Bins.Server, "sysmon-server", "./cmd/sysmon", ShapeServer},
		{&Bins.Checklatest, "checklatest", "./cmd/checklatest", ""},
		{&Bins.Checkpins, "checkpins", "./cmd/checkpins", ""},
		{&Bins.Buildtags, "buildtags", "./cmd/buildtags", ""},
		{&Bins.Tsgen, "tsgen", "./cmd/tsgen", ""},
	}
	for _, b := range builds {
		path, err := build(dir, b.name, b.pkg, b.tags)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		*b.out = path
	}
	return m.Run()
}

// build compiles pkg with tags into dir/name, instrumented when coverage is
// being collected.
func build(dir, name, pkg, tags string) (string, error) {
	out := filepath.Join(dir, name)
	args := []string{"build", "-o", out, "-tags", tags}
	if os.Getenv("SYSMON_COVERDIR") != "" {
		args = append(args, "-cover", "-coverpkg="+module+"/...")
	}
	// #nosec G204 -- the program is the fixed "go", and every argument comes
	// from the fixed build table in buildAndRun: a package path, a tag set and
	// a directory this function created. Nothing reaches it from outside.
	cmd := exec.Command("go", append(args, pkg)...)
	cmd.Dir = RepoRoot
	cmd.Env = append(os.Environ(), "CGO_ENABLED=1")
	if output, err := cmd.CombinedOutput(); err != nil {
		return "", &BuildError{Name: name, Output: string(output), Err: err}
	}
	return out, nil
}

// BuildError reports a binary the suite could not build, with the compiler's
// output.
type BuildError struct {
	Name   string
	Output string
	Err    error
}

func (e *BuildError) Error() string {
	return "harness: build " + e.Name + ": " + e.Err.Error() + "\n" + e.Output
}

func (e *BuildError) Unwrap() error { return e.Err }

// ── Running a command ────────────────────────────────────────────────────────

// systemTools are the programs from the system a suite may run beside the
// binaries it built: the shell and unshare, for putting a process in a
// namespace or a cgroup of its own.
var systemTools = map[string]bool{"/bin/sh": true, "unshare": true}

// allowedProgram returns bin if a suite may run it, and fails the test
// otherwise. A suite runs the binaries TestMain built, programs a test wrote
// into its own temporary directory (a namespace wrapper, a stand-in guest),
// and the fixed systemTools -- nothing named by anything but the test itself.
func allowedProgram(t *testing.T, bin string) string {
	t.Helper()
	switch bin {
	case Bins.Sysmon, Bins.Server, Bins.Checklatest, Bins.Checkpins, Bins.Buildtags, Bins.Tsgen:
		return bin
	}
	if systemTools[bin] {
		return bin
	}
	clean := filepath.Clean(bin)
	if rel, err := filepath.Rel(os.TempDir(), clean); err == nil && filepath.IsAbs(clean) &&
		rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return clean
	}
	t.Fatalf("harness: %q is not a program the suites run: a built binary, a file under %s, or one of %v",
		bin, os.TempDir(), systemTools)
	return ""
}

// Result is how one run of a binary ended.
type Result struct {
	Stdout string
	Stderr string
	Code   int
}

// Output is stdout and stderr together, for assertions on what a user sees.
func (r Result) Output() string { return r.Stdout + r.Stderr }

// Run runs bin with args to completion. env adds to the test's
// environment; the display variables are always cleared so no command takes
// the GUI path.
func Run(t *testing.T, bin string, env []string, args ...string) Result {
	t.Helper()
	program := allowedProgram(t, bin)
	// #nosec G204 -- program has passed allowedProgram: a binary this suite
	// built, a file a test wrote into its own temporary directory, or one of
	// the fixed system tools. The arguments are the calling test's own.
	cmd := exec.Command(program, args...)
	cmd.Env = append(append(os.Environ(), "DISPLAY=", "WAYLAND_DISPLAY="), env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	code := 0
	if err := cmd.Run(); err != nil {
		var exited *exec.ExitError
		if !errors.As(err, &exited) {
			t.Fatalf("run %s %v: %v", filepath.Base(bin), args, err)
		}
		code = exited.ExitCode()
	}
	return Result{Stdout: stdout.String(), Stderr: stderr.String(), Code: code}
}

// RunSysmon runs the default sysmon binary, returning stdout and stderr together
// and the exit code.
func RunSysmon(t *testing.T, args ...string) (string, int) {
	t.Helper()
	r := Run(t, Bins.Sysmon, nil, args...)
	return r.Output(), r.Code
}

// RunStdout is RunSysmon returning stdout alone. Use it for --json
// assertions: diagnostics such as "NVML init failed" go to stderr, and merging
// the streams would corrupt otherwise valid JSON.
func RunStdout(t *testing.T, args ...string) (string, int) {
	t.Helper()
	r := Run(t, Bins.Sysmon, nil, args...)
	return r.Stdout, r.Code
}

// MustParseJSON unmarshals src into dst, failing the test immediately if
// parsing fails.
func MustParseJSON(t *testing.T, src string, dst any) {
	t.Helper()
	if err := json.Unmarshal([]byte(src), dst); err != nil {
		t.Fatalf("JSON parse error: %v\nraw output:\n%s", err, src)
	}
}

// CloseOnCleanup closes c when the test ends, failing the test if the close
// does. what names c in the failure.
func CloseOnCleanup(t *testing.T, what string, c io.Closer) {
	t.Helper()
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Errorf("closing %s: %v", what, err)
		}
	})
}

// WriteFile writes body to name in a fresh temporary directory and returns
// its path.
func WriteFile(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

// ── A running server ─────────────────────────────────────────────────────────

// ListeningMsg is the log message the serve command emits once its
// listener is bound, with the address it got.
const ListeningMsg = "server listening"

// LogRecord is one line of the server's JSON log.
type LogRecord map[string]any

// Msg is the record's message.
func (r LogRecord) Msg() string {
	s, _ := r["msg"].(string)
	return s
}

// Level is the record's level: DEBUG, INFO, WARN or ERROR.
func (r LogRecord) Level() string {
	s, _ := r["level"].(string)
	return s
}

// LogTail collects a process's JSON log as it is written. Waiters are woken
// by closing changed, which is replaced on every record, so a wait needs no
// polling and no timer beyond await's bound.
type LogTail struct {
	mu      sync.Mutex
	records []LogRecord
	changed chan struct{}
	ended   bool
}

func newLogTail() *LogTail { return &LogTail{changed: make(chan struct{})} }

// read consumes r to its end, recording every JSON line and ignoring any
// other text. It returns how reading ended: nil at end of stream.
func (l *LogTail) read(r io.Reader) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		var rec LogRecord
		if err := json.Unmarshal(scanner.Bytes(), &rec); err != nil {
			continue
		}
		l.mu.Lock()
		l.records = append(l.records, rec)
		close(l.changed)
		l.changed = make(chan struct{})
		l.mu.Unlock()
	}
	l.mu.Lock()
	l.ended = true
	close(l.changed)
	l.changed = make(chan struct{})
	l.mu.Unlock()
	return scanner.Err()
}

// find returns the first record from index from on that match accepts, its
// index, and the channel closed when another record arrives.
func (l *LogTail) find(from int, match func(LogRecord) bool) (LogRecord, int, bool, <-chan struct{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := from; i < len(l.records); i++ {
		if match(l.records[i]) {
			return l.records[i], i, true, nil
		}
	}
	if l.ended {
		return nil, len(l.records), false, nil
	}
	return nil, len(l.records), false, l.changed
}

// Await returns the first record match accepts, waiting for it to be logged.
// It fails the test if the log ends, or await's bound passes, first.
//
// The bound covers the whole wait, not each record: a process that keeps
// logging other lines must not keep a wait for this one alive.
func (l *LogTail) Await(t *testing.T, what string, match func(LogRecord) bool) LogRecord {
	t.Helper()
	bound := time.NewTimer(await.Timeout)
	defer bound.Stop()
	from := 0
	for {
		rec, next, ok, changed := l.find(from, match)
		if ok {
			return rec
		}
		if changed == nil {
			t.Fatalf("the log ended without %s", what)
			return nil
		}
		from = next
		select {
		case <-changed:
		case <-bound.C:
			t.Fatalf("await: %s did not happen within %v", what, await.Timeout)
			return nil
		}
	}
}

// AwaitMsg returns the first record whose message is msg.
func (l *LogTail) AwaitMsg(t *testing.T, msg string) LogRecord {
	t.Helper()
	return l.Await(t, fmt.Sprintf("the log line %q", msg), func(r LogRecord) bool { return r.Msg() == msg })
}

// Has reports whether a record with message msg has been logged so far.
func (l *LogTail) Has(msg string) bool {
	_, _, ok, _ := l.find(0, func(r LogRecord) bool { return r.Msg() == msg })
	return ok
}

// Server is a running `sysmon serve`.
type Server struct {
	// Addr is the address the listener bound.
	Addr string
	// Logs is the process's JSON log.
	Logs *LogTail
	cmd  *exec.Cmd
	// done is closed once the process has been reaped; exitErr is how it
	// ended.
	done    chan struct{}
	exitErr error
}

// ServeOptions say how to start a server.
type ServeOptions struct {
	// Bin is the binary; the default build when empty.
	Bin string
	// Config, when set, is written to a file passed with --config.
	Config string
	// ConfigPath is passed with --config as it is, for a test that edits the
	// file while the server runs. It takes precedence over Config.
	ConfigPath string
	// Args are further arguments to `sysmon serve`.
	Args []string
	// Env adds to the environment.
	Env []string
	// NoSnapshot skips waiting for the first collection.
	NoSnapshot bool
	// Client, when set, is the HTTPS client readiness is probed with: the
	// server serves TLS, and the client trusts its certificate.
	Client *http.Client
}

// StartServer starts the default build on an ephemeral loopback port and
// returns its address, ready to serve.
func StartServer(t *testing.T) string {
	t.Helper()
	return StartServe(t)
}

// StartServe is StartServer with extra arguments to `sysmon serve`.
func StartServe(t *testing.T, extra ...string) string {
	t.Helper()
	return Serve(t, ServeOptions{Args: extra}).Addr
}

// Serve starts `sysmon serve` on an ephemeral loopback port.
//
// The port is 0 rather than a constant on purpose. A fixed port made two CI
// jobs on one runner collide by construction -- whichever bound second failed,
// on a machine where nothing was wrong (issue #62). Asking the kernel for a
// port cannot collide, and the server logs the port it got.
//
// The server is stopped the way a service manager stops it, with SIGTERM, and
// must exit cleanly: that is the graceful shutdown path under test, and a
// process that exits normally is also one that writes its coverage data. Only
// a server that does not stop within the await bound is killed.
func Serve(t *testing.T, o ServeOptions) *Server {
	t.Helper()
	bin := o.Bin
	if bin == "" {
		bin = Bins.Sysmon
	}
	args := []string{"serve", "--addr", "127.0.0.1:0", "--interval", "200", "--log-format", "json"}
	switch {
	case o.ConfigPath != "":
		args = append(args, "--config", o.ConfigPath)
	case o.Config != "":
		args = append(args, "--config", WriteFile(t, "sysmon.yaml", o.Config))
	default:
		// An absent file is an empty configuration; the host's own
		// /etc/sysmon is never read.
		args = append(args, "--config", filepath.Join(t.TempDir(), "absent.yaml"))
	}
	args = append(args, o.Args...)

	s := Start(t, bin, o.Env, args...)
	s.Addr = s.Logs.Await(t, fmt.Sprintf("sysmon serve to report %q", ListeningMsg), func(r LogRecord) bool {
		addr, _ := r["addr"].(string)
		return r.Msg() == ListeningMsg && addr != ""
	})["addr"].(string)
	WaitForPort(t, s.Addr)
	switch {
	case o.NoSnapshot:
	case o.Client != nil:
		WaitForSnapshotWith(t, o.Client, "https://"+s.Addr+"/api/snapshot")
	default:
		WaitForSnapshot(t, "http://"+s.Addr+"/api/snapshot")
	}
	return s
}

// Start runs bin with args in the background, collecting its JSON log from
// stderr, and stops it with SIGTERM when the test ends.
func Start(t *testing.T, bin string, env []string, args ...string) *Server {
	t.Helper()
	program := allowedProgram(t, bin)
	// #nosec G204 -- program has passed allowedProgram, as in Run.
	cmd := exec.Command(program, args...)
	cmd.Env = append(append(os.Environ(), "DISPLAY=", "WAYLAND_DISPLAY="), env...)
	logs, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("capture the logs of %s: %v", filepath.Base(bin), err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", filepath.Base(bin), err)
	}

	s := &Server{Logs: newLogTail(), cmd: cmd, done: make(chan struct{})}
	drained := make(chan error, 1)
	go func() { drained <- s.Logs.read(logs) }()
	go func() {
		// The log reader finishes before Wait, which closes the pipe: reading
		// from it after that is the misuse os/exec documents.
		readErr := <-drained
		s.exitErr = errors.Join(cmd.Wait(), readErr)
		close(s.done)
	}()

	t.Cleanup(func() {
		select {
		case <-s.done:
			return
		default:
		}
		if err := s.Stop(t); err != nil {
			t.Errorf("%s on SIGTERM: %v, want a clean exit", filepath.Base(bin), err)
		}
	})
	return s
}

// Signal sends sig to the server.
func (s *Server) Signal(t *testing.T, sig os.Signal) {
	t.Helper()
	if err := s.cmd.Process.Signal(sig); err != nil {
		t.Fatalf("send %v: %v", sig, err)
	}
}

// Stop sends SIGTERM and returns how the process ended. A process that does
// not end within the await bound is killed, and the test fails.
func (s *Server) Stop(t *testing.T) error {
	t.Helper()
	if err := s.cmd.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return s.Wait(t)
}

// Wait returns how the process ended, once it has.
func (s *Server) Wait(t *testing.T) error {
	t.Helper()
	await.Recv(t, "the process to exit", s.done)
	return s.exitErr
}

// URL returns the server's http URL for path.
func (s *Server) URL(path string) string { return "http://" + s.Addr + path }

// ── HTTP ─────────────────────────────────────────────────────────────────────

// Do sends req and returns the status and body, failing the test if the
// request cannot be made.
func Do(t *testing.T, client *http.Client, req *http.Request) (int, http.Header, []byte) {
	t.Helper()
	if !isLoopback(req.URL.Hostname()) {
		t.Fatalf("harness: %s is not on loopback; the suites talk only to servers they started", req.URL)
	}
	// #nosec G704 -- the request goes to a loopback address, checked above:
	// a server or stand-in the calling test started. Nothing is fetched from
	// anywhere else.
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", req.Method, req.URL, err)
	}
	body, err := io.ReadAll(resp.Body)
	if cerr := resp.Body.Close(); cerr != nil {
		t.Errorf("close %s %s: %v", req.Method, req.URL, cerr)
	}
	if err != nil {
		t.Fatalf("read %s %s: %v", req.Method, req.URL, err)
	}
	return resp.StatusCode, resp.Header, body
}

// isLoopback reports whether host is a loopback address or localhost.
func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Get fetches url with the default client.
func Get(t *testing.T, url string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build GET %s: %v", url, err)
	}
	status, _, body := Do(t, http.DefaultClient, req)
	return status, body
}

// GetJSON fetches url, requires 200, and decodes the body into dst.
func GetJSON(t *testing.T, url string, dst any) {
	t.Helper()
	status, body := Get(t, url)
	if status != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200: %s", url, status, body)
	}
	MustParseJSON(t, string(body), dst)
}

// Post sends body to url as JSON and returns the status and response body.
func Post(t *testing.T, url, body string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build POST %s: %v", url, err)
	}
	req.Header.Set("Content-Type", "application/json")
	status, _, respBody := Do(t, http.DefaultClient, req)
	return status, respBody
}

// WaitForPort polls addr until a TCP connection succeeds, failing the test if
// it has not within await.Timeout. The listener belongs to another process,
// so polling it is the only way to observe it.
func WaitForPort(t *testing.T, addr string) {
	t.Helper()
	var dialer net.Dialer
	await.Until(t, "a listener to accept connections on "+addr, func(ctx context.Context) bool {
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

// WaitForSnapshot polls GET url until the server answers 200, meaning the
// monitor has completed its first collection, and fails the test if it has
// not within await.Timeout.
func WaitForSnapshot(t *testing.T, url string) {
	t.Helper()
	WaitForSnapshotWith(t, http.DefaultClient, url)
}

// WaitForSnapshotWith is WaitForSnapshot through client.
func WaitForSnapshotWith(t *testing.T, client *http.Client, url string) {
	t.Helper()
	await.Until(t, "GET "+url+" to answer 200", func(ctx context.Context) bool {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			t.Fatalf("building the request for %s: %v", url, err)
		}
		resp, err := client.Do(req)
		if err != nil {
			return false
		}
		if err := resp.Body.Close(); err != nil {
			t.Errorf("closing the %s response: %v", url, err)
		}
		return resp.StatusCode == http.StatusOK
	})
}
