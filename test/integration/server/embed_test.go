//go:build integration

package server

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/internal/await"
	"github.com/jeremyhahn/go-sysmon/pkg/collector"
	"github.com/jeremyhahn/go-sysmon/pkg/delta"
	"github.com/jeremyhahn/go-sysmon/pkg/monitor"
	"github.com/jeremyhahn/go-sysmon/pkg/server"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
	"github.com/jeremyhahn/go-sysmon/test/integration/harness"
)

// hostSnapshotter collects this host through pkg/collector, as an embedding
// application wires the library.
type hostSnapshotter struct{ sc *collector.SystemCollector }

func (h hostSnapshotter) Snapshot() (*types.Snapshot, error) {
	snap, err := h.sc.Snapshot(context.Background())
	if err != nil {
		return nil, err
	}
	return &snap, nil
}

// hostMonitor starts a monitor collecting this host, stopped when the test
// ends.
func hostMonitor(t *testing.T) *monitor.Monitor {
	t.Helper()
	mon, err := monitor.New(hostSnapshotter{sc: collector.NewSystemCollector(slog.New(slog.DiscardHandler))}, 200*time.Millisecond)
	if err != nil {
		t.Fatalf("monitor.New: %v", err)
	}
	if err := mon.Start(); err != nil {
		t.Fatalf("monitor.Start: %v", err)
	}
	t.Cleanup(func() {
		if err := mon.Stop(); err != nil {
			t.Errorf("monitor.Stop: %v", err)
		}
	})
	return mon
}

// run starts srv as an embedding application does -- Listen for the address,
// Start in the background -- and stops it gracefully when the test ends.
func run(t *testing.T, srv *server.Server) string {
	t.Helper()
	addr, err := srv.Listen()
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.Start() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), await.Timeout)
		defer cancel()
		if err := srv.Stop(ctx); err != nil {
			t.Errorf("Stop: %v", err)
		}
		if err := await.Recv(t, "Start to return after Stop", done); err != nil {
			t.Errorf("Start returned %v after a graceful Stop, want nil", err)
		}
	})
	return addr.String()
}

// TestEmbed_TLSPerHandshake embeds the server in an application that resolves
// TLS per handshake, as a multi-tenant proxy does: the hook sees the client's
// server name and supplies the certificate, and the API answers over it.
func TestEmbed_TLSPerHandshake(t *testing.T) {
	pki := newPKI(t)
	names := make(chan string, 16)
	srv, err := server.NewWithConfig(server.Config{
		Monitor: hostMonitor(t),
		Addr:    "127.0.0.1:0",
		TLS: &server.TLS{GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			select {
			case names <- hello.ServerName:
			default:
			}
			return &tls.Config{Certificates: []tls.Certificate{pki.leaf}, NextProtos: []string{"h2", "http/1.1"}, MinVersion: tls.VersionTLS12}, nil
		}},
	})
	if err != nil {
		t.Fatalf("NewWithConfig: %v", err)
	}
	if !srv.UsesTLS() {
		t.Fatal("a server configured with a handshake hook says it does not use TLS")
	}
	addr := run(t, srv)
	if srv.Addr() != addr {
		t.Errorf("Addr() = %q, want the bound %q", srv.Addr(), addr)
	}

	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs: pki.pool, ServerName: "localhost", MinVersion: tls.VersionTLS12,
	}}}
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	harness.WaitForSnapshotWith(t, client, "https://127.0.0.1:"+port+"/api/snapshot")
	if got := await.Recv(t, "the handshake hook to be consulted", names); got != "localhost" {
		t.Errorf("the hook saw server name %q, want localhost", got)
	}
}

// TestEmbed_ACallersBaseTLSConfigAndHeaders embeds the server with the
// application's own base TLS configuration and its own security-header
// policy: a replaced header is sent as the application set it, a removed one
// is not sent, and the rest keep their defaults.
func TestEmbed_ACallersBaseTLSConfigAndHeaders(t *testing.T) {
	pki := newPKI(t)
	base := &tls.Config{Certificates: []tls.Certificate{pki.leaf}, MinVersion: tls.VersionTLS13}
	srv, err := server.NewWithConfig(server.Config{
		Monitor: hostMonitor(t),
		Addr:    "127.0.0.1:0",
		TLS:     &server.TLS{Config: base},
		SecurityHeaders: http.Header{
			"Content-Security-Policy":      {"default-src 'self' https://cdn.example"},
			"Cross-Origin-Resource-Policy": {},
		},
		Version: "9.9.9",
	})
	if err != nil {
		t.Fatalf("NewWithConfig: %v", err)
	}
	if len(base.NextProtos) != 0 {
		t.Error("the caller's TLS config was modified")
	}
	addr := run(t, srv)
	client := pki.client()
	harness.WaitForSnapshotWith(t, client, "https://"+addr+"/api/snapshot")

	status, header, body := request(t, client, http.MethodGet, "https://"+addr+"/api/version", "")
	if status != http.StatusOK || !strings.Contains(body, `"9.9.9"`) {
		t.Errorf("GET /api/version = %d %s, want the embedding build's version", status, body)
	}
	if got := header.Get("Content-Security-Policy"); got != "default-src 'self' https://cdn.example" {
		t.Errorf("Content-Security-Policy = %q, want the application's", got)
	}
	if got := header.Get("Cross-Origin-Resource-Policy"); got != "" {
		t.Errorf("Cross-Origin-Resource-Policy = %q, want it removed", got)
	}
	if got := header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want the default kept", got)
	}
	// The embedded page, with no dashboard assets, says the service is up.
	if status, _, body := request(t, client, http.MethodGet, "https://"+addr+"/", ""); status != http.StatusOK || !strings.Contains(body, "running") {
		t.Errorf("GET / with no assets = %d %q, want the status page", status, body)
	}
}

// TestEmbed_RefusesWhatCannotWork verifies the constructor refuses, with a
// typed error, what would otherwise fail later and opaquely: TLS with no way
// to produce a certificate, a certificate without its key, a header the HTTP
// layer cannot send, and a second server describing one monitor differently.
func TestEmbed_RefusesWhatCannotWork(t *testing.T) {
	mon := hostMonitor(t)
	pki := newPKI(t)

	tlsCases := map[string]*server.TLS{
		"no certificate source":  {},
		"a certificate, no key":  {CertFile: pki.certFile},
		"a key file that is not": {CertFile: pki.certFile, KeyFile: pki.caFile},
	}
	for name, cfg := range tlsCases {
		_, err := server.NewWithConfig(server.Config{Monitor: mon, Addr: "127.0.0.1:0", TLS: cfg})
		var tlsErr *types.TLSConfigError
		if !errors.As(err, &tlsErr) || tlsErr.Error() == "" {
			t.Errorf("%s: NewWithConfig = %v, want a *types.TLSConfigError", name, err)
		}
	}

	for name, h := range map[string]http.Header{
		"a header name with a space": {"Bad Name": {"x"}},
		"a value with a newline":     {"X-Frame-Options": {"DENY\r\nX-Injected: 1"}},
		"one header named twice":     {"x-frame-options": {"DENY"}, "X-Frame-Options": {"SAMEORIGIN"}},
	} {
		_, err := server.NewWithConfig(server.Config{Monitor: mon, Addr: "127.0.0.1:0", SecurityHeaders: h})
		var headerErr *types.SecurityHeaderError
		if !errors.As(err, &headerErr) || headerErr.Error() == "" {
			t.Errorf("%s: NewWithConfig = %v, want a *types.SecurityHeaderError", name, err)
		}
	}

	if _, err := server.NewWithConfig(server.Config{Monitor: mon, Addr: "127.0.0.1:0", Version: "1.0.0"}); err != nil {
		t.Fatalf("the first server on the monitor: %v", err)
	}
	if _, err := server.NewWithConfig(server.Config{Monitor: mon, Addr: "127.0.0.1:0", Version: "1.0.0"}); err != nil {
		t.Errorf("a second server describing the monitor the same way: %v, want it to share the decoration", err)
	}
	_, err := server.NewWithConfig(server.Config{Monitor: mon, Addr: "127.0.0.1:0", Version: "2.0.0"})
	var installed *types.DecoratorInstalledError
	if !errors.As(err, &installed) || installed.Error() == "" {
		t.Errorf("a second server describing the monitor differently: %v, want a *types.DecoratorInstalledError", err)
	}
}

// TestEmbed_PlainServerMountedOnTheApplicationsRouter covers the simplest
// embedding: server.New for plain HTTP, mounted on the application's own mux
// under a prefix through ServeHTTP, while its own Start reports an address
// already in use as a start error.
func TestEmbed_PlainServerMountedOnTheApplicationsRouter(t *testing.T) {
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	harness.CloseOnCleanup(t, "the held port", held)

	srv := server.New(hostMonitor(t), held.Addr().String(), nil)
	if srv.UsesTLS() || srv.Addr() != "" {
		t.Errorf("a plain server before binding: UsesTLS %v Addr %q", srv.UsesTLS(), srv.Addr())
	}
	var startErr *types.ServerStartError
	if err := srv.Start(); !errors.As(err, &startErr) {
		t.Errorf("Start on a held address = %v, want a *types.ServerStartError", err)
	}

	mux := http.NewServeMux()
	mux.Handle("/sysmon/", http.StripPrefix("/sysmon", srv))
	app := &http.Server{Handler: mux, ReadHeaderTimeout: time.Second}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- app.Serve(ln) }()
	t.Cleanup(func() {
		if err := app.Shutdown(context.Background()); err != nil {
			t.Errorf("shut the application down: %v", err)
		}
		if err := await.Recv(t, "the application to stop serving", served); !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("application Serve = %v", err)
		}
	})

	harness.WaitForSnapshot(t, "http://"+ln.Addr().String()+"/sysmon/api/snapshot")
	var interval map[string]int
	harness.GetJSON(t, "http://"+ln.Addr().String()+"/sysmon/api/interval", &interval)
	if interval["interval_ms"] != 200 {
		t.Errorf("interval through the application's router = %v, want 200", interval)
	}
	// A server with no fleet refuses a fleet cadence rather than pretending.
	if status, body := harness.Post(t, "http://"+ln.Addr().String()+"/sysmon/api/interval", `{"interval_ms":1000,"scope":"fleet"}`); status != http.StatusBadRequest {
		t.Errorf("a fleet cadence on a server with no fleet = %d %s, want 400", status, body)
	}
}

// frame is one event DeltaStream emitted.
type frame struct {
	name    string
	payload []byte
}

// TestEmbed_DeltaStreamFeedsAnApplicationDirectly drives server.DeltaStream,
// which the desktop build uses to push frames without HTTP: the frames are
// the SSE protocol's, they apply in order into the snapshot the monitor
// holds, and the stream ends when the monitor stops.
func TestEmbed_DeltaStreamFeedsAnApplicationDirectly(t *testing.T) {
	mon, err := monitor.New(hostSnapshotter{sc: collector.NewSystemCollector(slog.New(slog.DiscardHandler))}, 200*time.Millisecond)
	if err != nil {
		t.Fatalf("monitor.New: %v", err)
	}
	if err := mon.Start(); err != nil {
		t.Fatalf("monitor.Start: %v", err)
	}
	ds := server.NewDeltaStream(mon, nil)
	frames := make(chan frame, 64)
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		ds.Run(context.Background(), func(name string, payload []byte) {
			frames <- frame{name: name, payload: append([]byte(nil), payload...)}
		})
	}()

	var tree delta.Tree
	var seq uint64
	for applied := 0; applied < 4; applied++ {
		f := await.Recv(t, "a frame", frames)
		var env envelope
		harness.MustParseJSON(t, string(f.payload), &env)
		switch f.name {
		case "full":
			if tree, err = delta.Decode(env.Snapshot); err != nil {
				t.Fatalf("full frame: %v", err)
			}
		case "delta":
			if tree == nil || env.Seq != seq+1 {
				t.Fatalf("delta frame %d after frame %d, with a base held: %v", env.Seq, seq, tree != nil)
			}
			if env.Patch != nil {
				if tree, err = delta.Apply(tree, env.Patch); err != nil {
					t.Fatalf("delta frame %d: %v", env.Seq, err)
				}
			}
		default:
			t.Fatalf("frame %q", f.name)
		}
		seq = env.Seq
	}
	if timestampOf(t, tree) == "" {
		t.Error("the reconstructed snapshot is undated")
	}

	if err := mon.Stop(); err != nil {
		t.Fatalf("monitor.Stop: %v", err)
	}
	await.Recv(t, "the delta stream to end with the monitor", ended)
	ds.Shutdown()

	// A stream with nothing to deliver to returns at once.
	server.NewDeltaStream(mon, nil).Run(context.Background(), nil)
}

// TestEmbed_ABareServerSaysWhatItDoesNotHave embeds the server with nothing
// but a monitor, as the simplest library use does: no dashboard assets, no
// fleet, no configuration. Each surface that needs one of those answers
// plainly that it is not there, rather than failing or pretending.
func TestEmbed_ABareServerSaysWhatItDoesNotHave(t *testing.T) {
	addr := run(t, server.New(hostMonitor(t), "127.0.0.1:0", nil))
	harness.WaitForSnapshot(t, "http://"+addr+"/api/snapshot")

	// With no assets the root is a status page, so a probe of / still learns
	// the service is up.
	status, body := harness.Get(t, "http://"+addr+"/")
	if status != http.StatusOK || string(body) != "go-sysmon is running\n" {
		t.Errorf("GET / = %d %q, want the status page", status, body)
	}

	// No configuration manager: the settings view is empty, not an error.
	var view types.ConfigView
	harness.GetJSON(t, "http://"+addr+"/api/config", &view)
	if len(view.Entries) != 0 {
		t.Errorf("GET /api/config on a server with no configuration = %+v, want no entries", view)
	}

	// No fleet: every fleet surface says so with a 404.
	for _, path := range []string{
		"/api/fleet/subtree",
		"/api/fleet/nodes?name=peer",
		"/api/fleet/peer/snapshot",
		"/api/fleet/peer/config",
	} {
		status, body := harness.Get(t, "http://"+addr+path)
		if status != http.StatusNotFound || !strings.Contains(string(body), "fleet is not configured") {
			t.Errorf("GET %s = %d %q, want 404, fleet is not configured", path, status, body)
		}
	}
}

// TestEmbed_AMonitorNeverStartedIsNamed covers an embedder that wires the
// server and forgets to start the monitor: the API says the monitor is not
// running, which is the fix, rather than timing out or serving nothing.
func TestEmbed_AMonitorNeverStartedIsNamed(t *testing.T) {
	mon, err := monitor.New(hostSnapshotter{sc: collector.NewSystemCollector(slog.New(slog.DiscardHandler))}, time.Second)
	if err != nil {
		t.Fatalf("monitor.New: %v", err)
	}
	addr := run(t, server.New(mon, "127.0.0.1:0", nil))

	status, body := harness.Get(t, "http://"+addr+"/api/snapshot")
	if status != http.StatusServiceUnavailable || !strings.Contains(string(body), "monitor is not running") {
		t.Errorf("GET /api/snapshot with the monitor never started = %d %q, want 503 naming the monitor", status, body)
	}
}

// opaqueWriter is an application middleware's ResponseWriter wrapper that
// hides the writer it wraps: it forwards writes but neither flushes nor
// unwraps, which is the mistake that breaks streaming behind it.
type opaqueWriter struct{ w http.ResponseWriter }

func (o opaqueWriter) Header() http.Header         { return o.w.Header() }
func (o opaqueWriter) Write(p []byte) (int, error) { return o.w.Write(p) }
func (o opaqueWriter) WriteHeader(status int)      { o.w.WriteHeader(status) }

// TestEmbed_AMiddlewareThatCannotFlushIsTold mounts the server behind such a
// middleware. The event stream cannot work there, and the client is told why
// with a 500 before anything is streamed -- not left holding a response that
// never delivers an event -- while the rest of the API still answers.
func TestEmbed_AMiddlewareThatCannotFlushIsTold(t *testing.T) {
	srv := server.New(hostMonitor(t), "127.0.0.1:0", nil)
	app := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			srv.ServeHTTP(opaqueWriter{w}, r)
		}),
		ReadHeaderTimeout: time.Second,
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- app.Serve(ln) }()
	t.Cleanup(func() {
		if err := app.Shutdown(context.Background()); err != nil {
			t.Errorf("shut the application down: %v", err)
		}
		if err := await.Recv(t, "the application to stop serving", served); !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("application Serve = %v", err)
		}
	})
	base := "http://" + ln.Addr().String()
	harness.WaitForSnapshot(t, base+"/api/snapshot")

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, base+"/api/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept-Encoding", "gzip")
	status, header, body := harness.Do(t, &http.Client{Transport: &http.Transport{DisableCompression: true}}, req)
	if status != http.StatusInternalServerError || !strings.Contains(string(body), "response writer does not support streaming") {
		t.Errorf("GET /api/events behind a non-flushing middleware = %d %q, want 500 saying why", status, body)
	}
	if enc := header.Get("Content-Encoding"); enc != "" {
		t.Errorf("the refusal carries Content-Encoding %q, but its body is not compressed", enc)
	}
}
