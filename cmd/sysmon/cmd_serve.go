package main

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/jeremyhahn/go-sysmon/pkg/collector"
	"github.com/jeremyhahn/go-sysmon/pkg/config"
	"github.com/jeremyhahn/go-sysmon/pkg/fleet"
	"github.com/jeremyhahn/go-sysmon/pkg/memlimit"
	"github.com/jeremyhahn/go-sysmon/pkg/monitor"
	"github.com/jeremyhahn/go-sysmon/pkg/outbound"
	"github.com/jeremyhahn/go-sysmon/pkg/sdnotify"
	"github.com/jeremyhahn/go-sysmon/pkg/server"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
	"github.com/jeremyhahn/go-sysmon/pkg/watchdog"
)

// shutdownTimeout bounds how long the server waits for in-flight HTTP requests
// to finish after a shutdown signal before closing them.
const shutdownTimeout = 10 * time.Second

var (
	serveAddr       string
	serveIntervalMs int
	serveTLSCert    string
	serveTLSKey     string
	serveConfigPath string
)

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Start the web server",
	Long: "Start the HTTP server that streams live system metrics to connected browsers " +
		"as server-sent events.",
	RunE: runServeCmd,
}

func init() {
	// Every flag default is read from the defaults layer rather than written
	// out again here. A second copy of ":8080" would be a listen address that
	// the config file and the environment cannot reach: it sits in the flag
	// layer, which outranks both, and the two copies would drift the first time
	// only one of them was edited.
	d := config.Defaults()

	serveCmd.Flags().StringVar(&serveAddr, "addr", d.Server.Addr, "HTTP listen address")
	serveCmd.Flags().IntVar(&serveIntervalMs, "interval", d.Server.IntervalMs, "Polling interval in milliseconds")
	serveCmd.Flags().StringVar(&serveTLSCert, "tls-cert", d.Server.TLSCert, "Path to a PEM certificate; enables HTTPS")
	serveCmd.Flags().StringVar(&serveTLSKey, "tls-key", d.Server.TLSKey, "Path to the PEM private key matching --tls-cert")
	serveCmd.Flags().StringVar(&serveConfigPath, "config", config.DefaultPath,
		"Path to the YAML configuration file; missing is not an error")
	rootCmd.AddCommand(serveCmd)
}

// serveTLSConfig builds the server TLS settings from the command flags,
// returning nil when neither flag was supplied.
func serveTLSConfig(cfg *config.Config) *server.TLS {
	if cfg.Server.TLSCert == "" && cfg.Server.TLSKey == "" {
		return nil
	}
	return &server.TLS{CertFile: cfg.Server.TLSCert, KeyFile: cfg.Server.TLSKey}
}

// localNodeName is how this node identifies itself in the fleet view. The
// hostname is the name an operator already knows the machine by, but a
// configured name wins: a repurposed machine can carry a hostname that no
// longer describes its role, and relabelling a dashboard is far cheaper than
// renaming a running host.
func localNodeName(mgr *config.Manager) string {
	if mgr == nil {
		d := config.Defaults()
		return d.LocalName()
	}
	// Resolved by the config itself, so the name published to the fleet is the
	// same one peers are validated against at load.
	return mgr.Current().LocalName()
}

// listenHook is called once the listener is bound, before anything serves. It
// does nothing outside tests; see afterListen in runServe.
var listenHook = func() {}

// serverListeningMsg is logged once the listener is bound, with the address it
// bound to. Named rather than inlined because the integration suite reads this
// line to discover the port the kernel assigned it.
const serverListeningMsg = "server listening"

func runServeCmd(cmd *cobra.Command, _ []string) error {
	if serveIntervalMs <= 0 {
		return &types.InvalidIntervalError{Message: "interval must be greater than zero"}
	}

	// The signal handler is installed FIRST, before anything is started and
	// well before the socket is bound.
	//
	// It used to be installed inside serveUntilSignal, which runs after
	// srv.Listen() -- so between the bind and the handler the process was
	// reachable on its port with SIGTERM still at its default disposition, and
	// a stop arriving in that window killed it outright: no clean shutdown, no
	// monitor stop, no drained connections. `systemctl stop` during startup is
	// a real ordering on a rolling deploy, not a theoretical one (#106).
	//
	// Installed before the monitor starts as well, so a stop during collector
	// startup -- which takes longer than the bind -- is handled too.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfgMgr, err := loadServeConfig(cmd)
	if err != nil {
		return err
	}
	cfg := cfgMgr.Current()
	interval := time.Duration(cfg.Server.IntervalMs) * time.Millisecond

	slog.Info("starting sysmon web server",
		"addr", cfg.Server.Addr,
		"interval", interval,
		"config", cfgMgr.Path(),
		"peers", len(cfg.Fleet.Peers),
		"version", version,
		"commit", gitCommit,
		"built", buildDate,
	)

	// Report environment problems once, up front. The dashboard cannot show
	// what the process is not permitted to read, and a silent gap looks like
	// an idle machine rather than a missing permission.
	runStartupChecks()

	// Tell the Go runtime about the cgroup it is running in. Without this it
	// grows the heap toward twice the live set and the kernel throttles the
	// allocation instead -- silently, with the unit still active and nothing
	// restarting (issue #65).
	applyMemoryLimit()

	sc := collector.NewSystemCollector(slog.Default())
	// The build, the node's name and the alerts are attached by the server's
	// decorator on this monitor, the same for every path that serves a
	// snapshot; see server.NewWithConfig.
	mon, err := monitor.New(newMonitorSnapshotter(sc), interval)
	if err != nil {
		return err
	}

	// The polling interval is consumed once, when the monitor is built. Without
	// this applier a reload would change what GET /api/config reports while the
	// collection loop kept running at its original cadence -- the settings view
	// would be describing a service that does not exist.
	cfgMgr.OnReload(func(c *config.Config) error {
		ms := c.Server.IntervalMs
		if ms <= 0 {
			return &types.InvalidIntervalError{Message: "interval_ms must be greater than zero"}
		}
		return mon.SetInterval(time.Duration(ms) * time.Millisecond)
	})

	// done bounds the reload watchers and the fleet poller to the lifetime of
	// this command.
	done := make(chan struct{})
	defer close(done)

	watchReloadSignals(cfgMgr, done, slog.Default())
	if cfg.Server.Watch {
		if err := cfgMgr.Watch(done, slog.Default(), func(deferred []string) {
			if len(deferred) > 0 {
				slog.Warn("config: these settings need a restart to take effect", "settings", deferred)
			}
		}); err != nil {
			slog.Warn("config: cannot watch the config file", "error", err)
		}
	}

	// The fleet poller reads its peer list from the config manager on every
	// cycle, so peers can be added or removed by reloading rather than
	// restarting -- which would reset Prometheus counters and drop every
	// connected browser.
	fleetPoller := fleet.New(
		func() config.Fleet { return cfgMgr.Current().Fleet },
		func() (*types.Snapshot, *types.Snapshot, time.Duration) {
			prev, cur, err := mon.Consecutive()
			if err != nil {
				return nil, nil, 0
			}
			return prev, cur, mon.Interval()
		},
		func() string { return localNodeName(cfgMgr) },
		slog.Default(),
	)

	// The peer transport is resolved once, here, because a CA file that cannot
	// be read is a startup mistake and should read as one. Deferring it to the
	// poll loop would report it as every peer being unreachable, which is the
	// same symptom as a switch being down.
	//
	// An unconfigured fleet.tls yields exactly the client the poller builds for
	// itself, so a plaintext fleet takes this path and changes nothing.
	fleetTLS := cfgMgr.Current().Fleet.TLS
	peerClient, err := outbound.NewTLSClient(0, outbound.TLS{
		CAFile:             fleetTLS.CAFile,
		CertFile:           fleetTLS.CertFile,
		KeyFile:            fleetTLS.KeyFile,
		InsecureSkipVerify: fleetTLS.InsecureSkipVerify,
	})
	if err != nil {
		return err
	}
	fleetPoller.SetClient(peerClient)

	if fleetTLS.InsecureSkipVerify {
		// Said once, loudly, at startup. A fleet running with peer verification
		// off looks identical to one running with it on, right up until it
		// matters, so this log is the only place it is visible.
		slog.Default().Warn("fleet: peer certificate verification is DISABLED " +
			"(fleet.tls.insecure_skip_verify); https peers are encrypted but not authenticated")
	}

	// Nil in a build without prometheus support; the server has always
	// treated that as "this node keeps no history" (#100).
	exporter := startExporter(cfgMgr, mon, done)

	// Topology A (owner ruling 2026-09-04): the aggregator holds every peer's
	// reading already, so it is the one node that can persist the fleet's
	// history -- and the peers keep no prometheus and no storage of their own.
	//
	// Registered before Run so no reading can arrive unobserved, and wired
	// unconditionally: whether anything is written is decided per submission by
	// the exporter reading the current config, so turning remote write on stays
	// a reload rather than a restart. In a build without prometheus support
	// this observes nothing, because there is nothing to write to.
	observePeers(cfgMgr, fleetPoller, exporter)
	go fleetPoller.Run(done)

	webAssets, err := fs.Sub(frontendAssets, "frontend/dist")
	if err != nil {
		return err
	}

	srv, err := server.NewWithConfig(server.Config{
		Monitor:   mon,
		Addr:      cfg.Server.Addr,
		Assets:    webAssets,
		TLS:       serveTLSConfig(cfg),
		Fleet:     fleetPoller,
		Config:    cfgMgr,
		Exporter:  exporter,
		History:   newHistoryQuerier(cfgMgr),
		Version:   version,
		GitCommit: gitCommit,
		BuildDate: buildDate,
	})
	if err != nil {
		return err
	}

	// Started only once the server has installed its decorator on it, so no
	// collection -- which the exporter and the fleet poller, already running,
	// read at once -- is published without the build, the name or the alerts.
	if err := mon.Start(); err != nil {
		return err
	}

	// afterListen is called with the socket bound and nothing serving yet.
	//
	// It exists so a test can occupy that instant deterministically: the window
	// this fix closes is a few microseconds wide, and a test that waits for the
	// port and then signals wins the race almost always -- which is how the
	// defect survived (#106). With the hook, "a SIGTERM arriving between the
	// bind and the first request" is a case that runs the same way every time.
	afterListen := listenHook

	// Bind before serving so a bad address fails here rather than inside the
	// goroutine below, and so the watchdog has a real address to dial.
	bound, err := srv.Listen()
	if err != nil {
		stopMonitor(mon)
		return err
	}

	// The configured address is frequently not the one in use: with a port of 0
	// the kernel chooses, and ":8081" names no interface. The line above logs
	// what was asked for; this one logs what was bound, which is the only place
	// that address exists for anything outside this process -- an operator
	// reading the journal, or a test harness that asked for port 0 precisely so
	// that no two of them could collide.
	slog.Info(serverListeningMsg, "addr", bound.String())
	afterListen()

	return serveUntilSignal(ctx, srv, mon)
}

// watchdogFor builds the systemd watchdog for a running server, or a Watchdog
// that does nothing when the manager did not ask for one.
//
// Both signals are required, and each covers what the other cannot. The probe
// makes a real request over the socket the server actually bound: accept is
// what failed on vm-host-2, where 2929 connections queued against a listener
// nothing was draining while every handler was parked, and a check that does
// not cross the listener cannot see that. Collection freshness covers the
// opposite case, a server answering promptly with readings from before its
// poll loop stalled -- to the probe that looks perfect.
func watchdogFor(srv *server.Server, mon *monitor.Monitor, n *sdnotify.Notifier) *watchdog.Watchdog {
	interval := n.WatchdogInterval()
	if interval <= 0 {
		return watchdog.New(nil, 0, nil, nil, slog.Default())
	}

	return watchdog.New(n, interval, selfRequest(srv), collectionIsCurrent(mon), slog.Default())
}

// selfRequest returns a probe that fetches /api/version from the server's own
// listener.
//
// The address comes from the listener rather than from configuration because
// the configured string is frequently not dialable -- ":8081" has no host, and
// a specific bind address is not loopback. The endpoint is the cheapest one
// that still crosses accept, the mux and a response write; it deliberately does
// not collect anything, because collection is the other signal's job.
func selfRequest(srv *server.Server) watchdog.Probe {
	client := &http.Client{
		Transport: &http.Transport{
			// Talking to our own socket, at the address our own listener is
			// bound to, which nothing else can bind while we hold it. The
			// question this probe asks is whether this process answers, not
			// who it is -- and the deployed certificate is routinely
			// self-signed or issued for a name that is not the bind address,
			// so verification would fail a liveness check for reasons that
			// have nothing to do with liveness.
			//
			// Annotated for gosec rather than suppressed for the linter alone:
			// the standalone scanner does not read //nolint, so a nolint here
			// silences `make lint` and leaves CI failing, which is the worst of
			// both -- a suppression that hides the finding from the developer
			// and not from the gate.
			// #nosec G402 -- self-dial to our own listener; identity is not the question
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			// One connection, never pooled: a pooled connection would let a
			// later probe succeed on a socket accepted before the stall began.
			DisableKeepAlives: true,
		},
	}

	return func(ctx context.Context) error {
		addr := srv.Addr()
		if addr == "" {
			return &types.ServerStartError{Cause: errors.New("no listener bound")}
		}

		scheme := "http"
		if srv.UsesTLS() {
			scheme = "https"
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, scheme+"://"+addr+"/api/version", nil)
		if err != nil {
			return err
		}

		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close() //nolint:errcheck
		// Read the body rather than only the status: a response whose headers
		// arrived before the stall and whose body never followed is a served
		// request only if nobody looks.
		if _, err := io.Copy(io.Discard, resp.Body); err != nil {
			return err
		}
		if resp.StatusCode != http.StatusOK {
			return &types.SelfRequestError{Addr: addr, Status: resp.StatusCode}
		}
		return nil
	}
}

// collectionIsCurrent returns a check that fails when the monitor's latest
// snapshot is older than three polling intervals.
//
// Three rather than one: a single missed tick is ordinary on a loaded host, and
// restarting for it would make the watchdog a source of outages. The interval
// is read on every check because it is settable at runtime -- a threshold
// computed once at startup would restart the service the moment somebody
// changed the poll rate.
func collectionIsCurrent(mon *monitor.Monitor) watchdog.Collecting {
	return func() error {
		snap, err := mon.Snapshot()
		if err != nil {
			return err
		}
		if age, limit := time.Since(snap.Timestamp), 3*mon.Interval(); age > limit {
			return &types.StaleCollectionError{Age: age, Limit: limit}
		}
		return nil
	}
}

// serveUntilSignal runs srv until it fails or the process receives SIGINT or
// SIGTERM, then shuts both srv and mon down cleanly. It returns the error that
// ended the server, or nil when the shutdown was signal-initiated and clean.
func serveUntilSignal(ctx context.Context, srv *server.Server, mon *monitor.Monitor) error {
	// A signal that arrived during startup is already in ctx, and the server is
	// stopped without ever being started rather than serving one request and
	// then being told to stop.
	if ctx.Err() != nil {
		slog.Info("shutdown signal received before serving started")
		stopMonitor(mon)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		// The listener is already bound -- runServe binds before this is
		// called -- so it is closed here rather than left open on a process
		// that is exiting.
		return srv.Stop(shutdownCtx)
	}

	// Start blocks, so run it alongside the signal wait. The buffered channel
	// keeps the goroutine from leaking if a signal wins the race.
	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Start()
	}()

	// Notification is set up after the listener is bound, so READY=1 is not a
	// claim the server cannot yet honour, and torn down before the shutdown
	// path so no ping outlives the decision to stop.
	notifier, err := sdnotify.New(os.Getenv, os.Getpid())
	if err != nil {
		// A socket the manager named and we cannot open is worth saying out
		// loud, but it is not a reason to refuse to serve: the service is
		// useful unwatched, and failing here would turn a notification problem
		// into an outage.
		slog.Warn("watchdog: notification socket unusable, running unwatched", "err", err)
	}
	defer func() {
		if err := notifier.Close(); err != nil {
			slog.Warn("watchdog: closing notification socket", "err", err)
		}
	}()

	// Nothing this process starts should be able to answer for its liveness.
	if err := sdnotify.Unsetenv(); err != nil {
		slog.Warn("watchdog: clearing notification variables", "err", err)
	}

	wd := watchdogFor(srv, mon, notifier)
	wdCtx, stopWatchdog := context.WithCancel(context.Background())
	defer stopWatchdog()
	go wd.Run(wdCtx)

	if err := notifier.Ready(); err != nil {
		slog.Warn("watchdog: READY notification failed", "err", err)
	}

	select {
	case err := <-errCh:
		// The listener failed (for example, the address is already in use).
		// There is nothing to shut down beyond the monitor.
		stopMonitor(mon)
		return err

	case <-ctx.Done():
		slog.Info("shutdown signal received, stopping server")
	}

	// Stop pinging before anything is torn down. A ping sent while the server
	// is draining reports a service that is deliberately on its way out as
	// healthy, and systemd is entitled to act on it.
	stopWatchdog()
	if err := notifier.Status("shutting down"); err != nil {
		slog.Debug("watchdog: status notification failed", "err", err)
	}

	// Stop the monitor first: closing each subscriber's Done channel lets the
	// event-stream handlers emit a final "bye" event and return. Without it,
	// Shutdown would block on those handlers until the timeout expires, since
	// a stream that is working as intended never finishes on its own.
	stopMonitor(mon)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	if err := srv.Stop(shutdownCtx); err != nil {
		slog.Error("server shutdown", "err", err)
		return err
	}

	slog.Info("server stopped cleanly")
	return nil
}

// stopMonitor stops mon, logging rather than propagating a failure: it is only
// ever called on a path that is already terminating.
func stopMonitor(mon *monitor.Monitor) {
	if err := mon.Stop(); err != nil {
		slog.Warn("monitor stop", "err", err)
	}
}

// applyMemoryLimit derives GOMEMLIMIT from this process's cgroup ceiling.
//
// Failure to read the cgroup is logged and not fatal: a host with no cgroups,
// or one that hides them, is a legitimate target and the runtime default is a
// working configuration. What is not acceptable is being silent about it, so
// every outcome including "no ceiling found" is logged once at startup.
func applyMemoryLimit() {
	l, err := memlimit.Detect("", os.Getenv)
	if err != nil {
		slog.Warn("memory limit: cgroup unreadable, leaving the Go default in place", "err", err)
		return
	}

	if l.Incoherent {
		// The watermark cannot be reached, so memory.events "high" will stay at
		// zero however much memory is used and the process will be OOM-killed
		// at memory.max instead of throttled. Reported loudly because the
		// configuration reads as more generous than a correct one and its
		// counter agrees with it.
		slog.Warn("memory limit: MemoryHigh is at or above MemoryMax, so the throttle watermark is unreachable and the cgroup will be OOM-killed instead",
			"high_bytes", l.High, "max_bytes", l.Max)
	}

	memlimit.Apply(l)

	switch {
	case l.Source == "":
		slog.Info("memory limit: no cgroup ceiling found; the Go runtime default applies")
	case l.Applied == 0:
		slog.Info("memory limit: set by the environment", "source", l.Source)
	default:
		slog.Info("memory limit: derived from the cgroup",
			"source", l.Source, "ceiling_bytes", l.Ceiling(), "gomemlimit_bytes", l.Applied)
	}
}
