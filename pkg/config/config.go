// Package config loads go-sysmon's runtime settings and makes them reloadable
// without restarting the process.
//
// Precedence is the conventional one, lowest to highest: built-in defaults, the
// config file, environment variables, then command-line flags. Every effective
// value records where it came from so the dashboard can show an operator why a
// setting has the value it does.
//
// Reload matters here for a specific reason: adding a fleet peer or retuning a
// threshold by restarting the service would reset the Prometheus counters, drop
// every connected browser and empty the history buffers. Instead the settings
// live behind an atomic pointer that Reload swaps, and long-lived components
// read the current pointer each cycle.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/spf13/viper"
)

// Source describes where an effective value came from.
const (
	SourceDefault = "default"
	SourceFile    = "config file"
	SourceEnv     = "env"
	SourceFlag    = "flag"
	// SourceRuntime marks a value changed on the running service rather than in
	// any configured layer -- today only the polling interval, which the
	// dashboard's refresh-rate selector sets through POST /api/interval. It
	// outranks every other source for as long as it lasts, because it is what
	// the service is actually doing, and a reload replaces it with the
	// configured value again.
	SourceRuntime = "runtime"
)

// EnvPrefix is the prefix for environment overrides: alerts.disk_full_pct is
// SYSMON_ALERTS_DISK_FULL_PCT.
const EnvPrefix = "SYSMON"

// DefaultPath is the config file consulted when no explicit path is given.
const DefaultPath = "/etc/go-sysmon/sysmon.yaml"

// Peer is one fleet member polled by this node.
type Peer struct {
	Name string `mapstructure:"name"`
	URL  string `mapstructure:"url"`
}

// Fleet holds the peers this node aggregates. With no peers the node behaves
// exactly as a standalone node: no fleet endpoint, no fleet view.
type Fleet struct {
	Peers []Peer `mapstructure:"peers"`
	// LocalName is how this node names itself in the fleet view. It defaults to
	// the hostname, which is usually right -- but a machine repurposed from an
	// earlier role can carry a hostname that no longer describes it, and
	// renaming a running host is a heavier change than relabelling a dashboard.
	LocalName string `mapstructure:"local_name"`
	// PollIntervalMs is how often peers are refreshed.
	PollIntervalMs int `mapstructure:"poll_interval_ms"`
	// TimeoutMs bounds a single peer request so one dead node cannot stall the
	// whole fleet refresh.
	TimeoutMs int `mapstructure:"timeout_ms"`
}

// PollInterval returns the peer refresh period.
func (f Fleet) PollInterval() time.Duration {
	return time.Duration(f.PollIntervalMs) * time.Millisecond
}

// Timeout returns the per-peer request timeout.
func (f Fleet) Timeout() time.Duration {
	return time.Duration(f.TimeoutMs) * time.Millisecond
}

// Alerts holds the threshold rules. A threshold of zero disables its rule;
// rules with no sensible "off" (a degraded array, a failing disk) are not
// configurable and always evaluate.
type Alerts struct {
	// Storage
	DiskFullPct     float64 `mapstructure:"disk_full_pct"`
	DiskFullCritPct float64 `mapstructure:"disk_full_crit_pct"`
	DiskUtilPct     float64 `mapstructure:"disk_util_pct"`
	DiskQueueLen    float64 `mapstructure:"disk_queue_len"`
	TempWarnC       float64 `mapstructure:"temp_warn_c"`
	TempCritC       float64 `mapstructure:"temp_crit_c"`
	NVMeWearPct     float64 `mapstructure:"nvme_wear_pct"`
	NVMeSparePct    float64 `mapstructure:"nvme_spare_pct"`
	// CPU and memory
	CPUPct       float64 `mapstructure:"cpu_pct"`
	MemoryPct    float64 `mapstructure:"memory_pct"`
	SwapPct      float64 `mapstructure:"swap_pct"`
	PSIFullAvg10 float64 `mapstructure:"psi_full_avg10"`
	LoadPerCore  float64 `mapstructure:"load_per_core"`
	// Network
	IfaceErrRate float64 `mapstructure:"iface_err_rate"`
	NetUtilPct   float64 `mapstructure:"net_util_pct"`
	// GPU. Accelerators are designed to run hot and near their power limit, so
	// these are deliberately looser than the CPU/disk equivalents.
	GPUTempWarnC float64 `mapstructure:"gpu_temp_warn_c"`
	GPUTempCritC float64 `mapstructure:"gpu_temp_crit_c"`
	GPUMemoryPct float64 `mapstructure:"gpu_memory_pct"`
	GPUPowerPct  float64 `mapstructure:"gpu_power_pct"`
	// Containers and virtualisation
	ContainerMemPct float64 `mapstructure:"container_mem_pct"`
	ContainerCPUPct float64 `mapstructure:"container_cpu_pct"`
	// Overrides tune thresholds for one resource, e.g. a filesystem that is
	// expected to run full. Keys match the rule fields above.
	Overrides []Override `mapstructure:"overrides"`
}

// Override is a per-resource threshold adjustment.
type Override struct {
	// Resource matches the alert's resource, e.g. "md2" or "/data".
	Resource        string  `mapstructure:"resource"`
	DiskFullPct     float64 `mapstructure:"disk_full_pct"`
	DiskFullCritPct float64 `mapstructure:"disk_full_crit_pct"`
	DiskUtilPct     float64 `mapstructure:"disk_util_pct"`
	DiskQueueLen    float64 `mapstructure:"disk_queue_len"`
	TempWarnC       float64 `mapstructure:"temp_warn_c"`
	TempCritC       float64 `mapstructure:"temp_crit_c"`
}

// Server holds settings for the HTTP listener. Addr and TLS are read at bind
// time and are therefore NOT reloadable; changing them needs a restart, and
// Reload reports that rather than pretending to apply them.
type Server struct {
	Addr       string `mapstructure:"addr"`
	IntervalMs int    `mapstructure:"interval_ms"`
	TLSCert    string `mapstructure:"tls_cert"`
	TLSKey     string `mapstructure:"tls_key"`
	// Watch re-reads the config file when it changes on disk.
	Watch bool `mapstructure:"watch"`
}

// Config is the complete effective configuration.
type Config struct {
	Server Server `mapstructure:"server"`
	Alerts Alerts `mapstructure:"alerts"`
	Fleet  Fleet  `mapstructure:"fleet"`
}

// Defaults returns the built-in configuration. These are chosen to be useful
// with no config file at all.
func Defaults() Config {
	return Config{
		Server: Server{
			Addr:       ":8080",
			IntervalMs: 1000,
		},
		Alerts: Alerts{
			DiskFullPct:     90,
			DiskFullCritPct: 95,
			DiskUtilPct:     90,
			DiskQueueLen:    8,
			TempWarnC:       70,
			TempCritC:       85,
			NVMeWearPct:     80,
			NVMeSparePct:    10,
			CPUPct:          90,
			MemoryPct:       90,
			SwapPct:         50,
			PSIFullAvg10:    10,
			LoadPerCore:     2.0,
			IfaceErrRate:    1,
			NetUtilPct:      80,
			GPUTempWarnC:    85,
			GPUTempCritC:    95,
			GPUMemoryPct:    90,
			GPUPowerPct:     95,
			ContainerMemPct: 90,
			ContainerCPUPct: 90,
		},
		Fleet: Fleet{
			PollIntervalMs: 10000,
			TimeoutMs:      3000,
		},
	}
}

// Loader reads configuration and tracks the origin of each value.
type Loader struct {
	path  string
	flags map[string]string
}

// NewLoader returns a Loader for the given config file path. An empty path
// means DefaultPath, and a missing file is not an error: defaults plus
// environment still produce a usable configuration.
func NewLoader(path string) *Loader {
	if path == "" {
		path = DefaultPath
	}
	return &Loader{path: path, flags: make(map[string]string)}
}

// SetFlag records a value supplied on the command line, which outranks every
// other source. Callers pass only flags the user actually set.
func (l *Loader) SetFlag(key, value string) {
	l.flags[key] = value
}

// Load produces the effective configuration and a per-key source map.
func (l *Loader) Load() (*Config, map[string]string, error) {
	v := viper.New()
	v.SetConfigFile(l.path)
	v.SetConfigType("yaml")

	def := Defaults()
	for key, val := range defaultMap(def) {
		v.SetDefault(key, val)
	}

	sources := make(map[string]string)

	// Only "the file is not there" is benign; that host is meant to run on
	// defaults. Every other stat failure -- a path component that is not a
	// directory, a directory the process may not traverse -- means the
	// operator's settings exist and cannot be read. Starting on defaults there
	// would silently discard the TLS, listener, fleet and alert configuration
	// while looking like a healthy boot, which is failing open on the file that
	// configures the security-relevant behaviour.
	fileLoaded := false
	switch _, err := os.Stat(l.path); {
	case err == nil:
		if err := v.ReadInConfig(); err != nil {
			return nil, nil, &ErrConfigParse{Path: l.path, Err: err}
		}
		fileLoaded = true
	case !errors.Is(err, fs.ErrNotExist):
		return nil, nil, &ErrConfigUnreadable{Path: l.path, Err: err}
	}

	v.SetEnvPrefix(EnvPrefix)
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	for key, val := range l.flags {
		v.Set(key, val)
	}

	// Decode strictly. A plain Unmarshal accepts keys it cannot map, so
	// `fleet: {peer: [...]}` -- one character wrong -- loads without complaint
	// and leaves Fleet.Peers empty: the daemon starts clean and monitors none
	// of the hosts the operator listed. The same applies to a misspelled alert
	// threshold, which silently keeps the default while the file says
	// otherwise. Every key in this file was typed on purpose, so one that maps
	// to nothing is a mistake worth stopping for, not a comment.
	var cfg Config
	if err := v.UnmarshalExact(&cfg); err != nil {
		return nil, nil, &ErrConfigParse{Path: l.path, Err: err}
	}

	// Attribute each scalar key. Viper cannot report provenance, so the sources
	// are derived the same way precedence is applied: flag, then env, then
	// file, else default.
	for _, key := range scalarKeys() {
		switch {
		case l.flags[key] != "":
			sources[key] = SourceFlag
		case os.Getenv(envKey(key)) != "":
			sources[key] = SourceEnv
		case fileLoaded && v.InConfig(key):
			sources[key] = SourceFile
		default:
			sources[key] = SourceDefault
		}
	}
	if len(cfg.Fleet.Peers) > 0 {
		sources["fleet.peers"] = SourceFile
		if os.Getenv(envKey("fleet.peers")) != "" {
			sources["fleet.peers"] = SourceEnv
		}
	}

	if err := cfg.normalize(sources); err != nil {
		return nil, nil, err
	}
	return &cfg, sources, nil
}

// Path returns the config file this loader reads.
func (l *Loader) Path() string { return l.path }

// normalize repairs values that would otherwise disable a subsystem by
// accident, keeping a partial config file usable. It fails only on a peer list
// that cannot be made coherent -- see the uniqueness check below.
//
// Each repair rewrites the key's source as well. A value the file asked for and
// did not get must not be attributed to the file: the effective value came from
// the built-in defaults, and that is what the settings view has to say.
func (c *Config) normalize(sources map[string]string) error {
	d := Defaults()
	if c.Server.IntervalMs <= 0 {
		c.Server.IntervalMs = d.Server.IntervalMs
		repairedToDefault(sources, "server.interval_ms")
	}
	if c.Server.Addr == "" {
		c.Server.Addr = d.Server.Addr
		repairedToDefault(sources, "server.addr")
	}
	if c.Fleet.PollIntervalMs <= 0 {
		c.Fleet.PollIntervalMs = d.Fleet.PollIntervalMs
		repairedToDefault(sources, "fleet.poll_interval_ms")
	}
	if c.Fleet.TimeoutMs <= 0 {
		c.Fleet.TimeoutMs = d.Fleet.TimeoutMs
		repairedToDefault(sources, "fleet.timeout_ms")
	}

	// A peer's name is its identity everywhere downstream: the poller stores
	// results under it, the fleet view renders one card per name and the
	// per-peer config lookup selects by it. Two peers sharing a name therefore
	// produce a card whose snapshot came from whichever host answered last and
	// whose settings came from the first one listed -- a single displayed node
	// mixing two real machines.
	//
	// This is rejected rather than de-duplicated. Silently dropping one peer
	// would hide a genuine mistake in the operator's file and leave a host
	// unmonitored while the dashboard looked complete.
	peers := c.Fleet.Peers[:0]
	seen := make(map[string]string, len(c.Fleet.Peers))
	for _, p := range c.Fleet.Peers {
		// An entry with no URL is rejected, not skipped. Skipping it was the
		// one unusable-peer case that failed open: `- name: db-prod` with the
		// url line missing or misindented started the service cleanly, and the
		// operator got a smaller fleet that looked complete. Every other
		// unusable peer below returns an error, and the reasoning in the
		// comment above -- a dropped peer hides a real mistake and leaves a
		// host unmonitored -- applies to this case most of all, because a
		// missing url is the likeliest of these mistakes to be a typo.
		if p.URL == "" {
			return &ErrPeerURL{Name: peerLabel(p.Name, "(unnamed)"), Reason: "no url; " + peerURLGuidance}
		}

		// The URL is validated before anything copies it. An unnamed peer takes
		// its name from its URL and ErrDuplicatePeer quotes URLs, so checking
		// credentials first is what keeps a secret out of a peer name, out of
		// an error message and out of the settings view.
		u, err := url.Parse(p.URL)
		if err != nil {
			return &ErrPeerURL{Name: peerLabel(p.Name, "(unnamed)"), Reason: urlParseReason(err)}
		}
		if u.User != nil {
			return &ErrPeerCredentials{Name: peerLabel(p.Name, u.Host), Host: u.Host}
		}
		// Parsing only proves the string is well-formed. "localhost:8081" parses
		// happily as scheme "localhost", and the poller could never fetch it --
		// the node would be permanently unreachable while the service reported a
		// clean start, which is the failure the operator is least likely to
		// notice.
		if reason := peerURLReason(u); reason != "" {
			return &ErrPeerURL{Name: peerLabel(p.Name, u.Host), Reason: reason}
		}

		// Default the name after validation, so two unnamed peers are compared
		// by the URL that will actually identify them.
		if p.Name == "" {
			p.Name = p.URL
		}
		if first, dup := seen[p.Name]; dup {
			return &ErrDuplicatePeer{Name: p.Name, URLs: []string{first, p.URL}}
		}
		seen[p.Name] = p.URL
		peers = append(peers, p)
	}
	c.Fleet.Peers = peers

	return nil
}

// ValidateAgainstLocalName rejects a peer carrying the name this node publishes
// as its own. Such a peer is resolved as the local machine by the fleet
// handlers, which consult the local name before the peer cache, so the
// configured host is never polled while the dashboard shows a card that looks
// like it.
//
// It is deliberately separate from normalize and takes the name as an argument
// rather than calling LocalName itself. Loading configuration must not depend
// on the machine it runs on: folding this into the parse made the same file
// valid on one host and invalid on another, which is a property no one expects
// of a config loader and which duly broke a test whose fixture happened to
// share a build agent's hostname. The caller resolves the name once, and the
// fleet publishes that same name.
func (c *Config) ValidateAgainstLocalName(local string) error {
	if local == "" {
		return nil
	}
	for _, p := range c.Fleet.Peers {
		if p.Name == local {
			return &ErrPeerIsLocalName{Name: p.Name}
		}
	}
	return nil
}

// LocalName is how this node identifies itself in the fleet view: the
// configured name, else the hostname, else a constant so the node is never
// nameless.
//
// It lives here rather than in the command so that the name peers are validated
// against and the name the fleet actually publishes cannot drift apart -- a
// collision check against a different name than the one in use would be worse
// than none.
func (c *Config) LocalName() string {
	if c.Fleet.LocalName != "" {
		return c.Fleet.LocalName
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "local"
}

// peerURLReason reports why a syntactically valid URL still cannot be polled,
// or "" when it is usable. The poller hands these to net/http, so an absolute
// http or https URL with a host is the whole requirement.
//
// Only the scheme is ever quoted. RFC 3986 limits a scheme to letters, digits
// and "+-.", so it cannot carry userinfo -- naming it cannot leak a credential
// the way echoing the URL would.
func peerURLReason(u *url.URL) string {
	switch {
	case u.Scheme == "":
		return "no scheme; " + peerURLGuidance
	case u.Scheme != "http" && u.Scheme != "https":
		return fmt.Sprintf("scheme %q is not supported; use http or https", u.Scheme)
	case u.Host == "":
		return "no host; " + peerURLGuidance
	}
	return ""
}

// peerURLGuidance tells the operator what a usable peer URL looks like. It is
// shared by every rejection reason so the advice cannot drift between them.
const peerURLGuidance = "use an absolute url such as http://host:8081"

// peerLabel names a peer in an error message. A peer with no name of its own is
// identified by fallback rather than by its URL, which may be the very thing
// that must not be quoted.
func peerLabel(name, fallback string) string {
	if name == "" {
		return fallback
	}
	return name
}

// urlParseReason extracts the parser's explanation from a url.Parse failure.
// The *url.Error wrapper formats the offending URL into its message, so only
// the inner cause is safe to report.
func urlParseReason(err error) string {
	var uerr *url.Error
	if errors.As(err, &uerr) && uerr.Err != nil {
		return uerr.Err.Error()
	}
	return "invalid url"
}

// repairedToDefault records that a key's effective value came from the built-in
// defaults after normalize rejected what a higher-precedence layer supplied.
func repairedToDefault(sources map[string]string, key string) {
	if sources != nil {
		sources[key] = SourceDefault
	}
}

// envKey maps a config key to its environment variable name.
func envKey(key string) string {
	return EnvPrefix + "_" + strings.ToUpper(strings.ReplaceAll(key, ".", "_"))
}

// Applier updates a runtime component from a newly loaded configuration. It is
// how a reload actually changes behaviour rather than merely changing what the
// settings view reports.
type Applier func(*Config) error

// Validator rejects a configuration that parsed correctly but cannot be used by
// this process. It runs BEFORE the new configuration is published, so a failure
// leaves the previous one live -- the same guarantee a malformed file gets.
//
// This is the seam for checks that depend on the running machine and therefore
// cannot live in parsing: loading must produce the same result wherever it runs.
type Validator func(*Config) error

// snapshot is one published generation of the configuration: the effective
// values and the provenance of those exact values. Nothing mutates a snapshot
// after it is stored, so readers need no lock.
type snapshot struct {
	cfg *Config
	src map[string]string
}

// Manager holds the current configuration and swaps it atomically on reload, so
// readers never block and never see a half-applied change.
type Manager struct {
	loader *Loader
	// reloadMu serialises a whole reload: load, publish and apply. The atomic
	// pointer keeps readers lock-free, but it only makes each individual store
	// safe -- it does not stop two reloads from interleaving. Without this,
	// reload A could publish A, reload B publish and apply B, and A then apply
	// A, leaving Current() reporting B while the monitor runs A's interval.
	reloadMu sync.Mutex
	// live holds the configuration and its provenance together. They are
	// published as one pointer because they are read as one fact: the settings
	// view puts a value and the layer that supplied it side by side, and two
	// separate pointers let a reader land between the two stores and pair a
	// value from one generation with the source that explains another.
	live atomic.Pointer[snapshot]
	// boot records the settings that are consumed once, when the process starts
	// and the listener binds. They cannot change in place, so they are pinned
	// back into every reloaded Config: reporting a value that is not in effect
	// would make the settings view lie about the running service.
	boot Server
	// bootSources records where each boot-only value came from. Provenance is
	// pinned with the value it describes: reporting the boot value under the
	// new file's source would produce a value/source pair that contradicts
	// itself, pointing an operator at a file that says something else.
	bootSources map[string]string
	// appliers push reloadable settings into the components that own them.
	appliers []Applier
	// validators reject an unusable configuration before it is published.
	validators []Validator
}

// bootOnlyKeys are the settings consumed at startup. They are reported as
// needing a restart and their boot values are retained as the effective ones.
func bootOnlyKeys() []string {
	return []string{
		"server.addr",
		"server.tls_cert",
		"server.tls_key",
		"server.watch",
	}
}

// NewManager loads the configuration once and returns a Manager holding it.
func NewManager(l *Loader) (*Manager, error) {
	cfg, src, err := l.Load()
	if err != nil {
		return nil, err
	}
	m := &Manager{loader: l}
	m.live.Store(&snapshot{cfg: cfg, src: src})
	m.boot = cfg.Server
	m.bootSources = make(map[string]string, len(bootOnlyKeys()))
	for _, key := range bootOnlyKeys() {
		m.bootSources[key] = src[key]
	}
	return m, nil
}

// OnReload registers a function that applies reloadable settings to a runtime
// component. Appliers run after the swap, on every reload path, so SIGHUP, the
// file watcher and the HTTP endpoint cannot diverge in what they actually
// change.
//
// Registration takes the same lock a reload holds, so an applier added while a
// reload is in flight either runs from the next one or not at all -- never
// against a half-published configuration.
func (m *Manager) OnReload(a Applier) {
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()
	m.appliers = append(m.appliers, a)
}

// OnValidate registers a check that runs against a freshly loaded configuration
// before it is published. Returning an error aborts the reload with the running
// configuration untouched.
func (m *Manager) OnValidate(v Validator) {
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()
	m.validators = append(m.validators, v)
}

// Validate runs the registered validators against cfg. NewManager callers use it
// to apply the same checks to the initial load that a reload would get.
func (m *Manager) Validate(cfg *Config) error {
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()
	for _, v := range m.validators {
		if err := v(cfg); err != nil {
			return err
		}
	}
	return nil
}

// Current returns the active configuration. It is safe for concurrent use and
// must be called per cycle rather than cached, or a reload will not be seen.
//
// A caller that needs the value AND its source must use View rather than
// pairing Current with Sources: two calls can straddle a reload and describe
// different generations.
func (m *Manager) Current() *Config {
	if s := m.live.Load(); s != nil {
		return s.cfg
	}
	return nil
}

// Sources returns the per-key origin map for the active configuration. The
// returned map is a copy: the published one is shared by every reader and must
// stay immutable for them to remain lock-free.
func (m *Manager) Sources() map[string]string {
	if s := m.live.Load(); s != nil {
		return maps.Clone(s.src)
	}
	return nil
}

// Path returns the config file backing this manager.
func (m *Manager) Path() string { return m.loader.Path() }

// Reload re-reads the configuration and swaps it in. The returned slice names
// settings that changed but cannot take effect until the process restarts. A
// failed read leaves the running configuration untouched.
//
// Caveat: appliers run AFTER the swap, so a setting an applier rejects is
// already reported as effective by Current and the settings view while the
// component that rejected it keeps its previous value. The returned *ErrApply
// is the only signal that this happened. That is tolerable today because the
// one registered applier cannot fail -- normalize guarantees a positive
// interval -- but an applier that genuinely can fail would need its rejected
// keys marked in the view, not just reported to the caller.
//
// A reload also re-asserts settings that were changed on the running service,
// which today means the polling interval set through POST /api/interval: the
// interval applier pushes the configured value back, discarding a refresh rate
// chosen in the dashboard. That is deliberate. A reload is an operator saying
// "make the file true", and a transient UI preference is not something the file
// knows about or should lose to.
func (m *Manager) Reload() ([]string, error) {
	// Held across load, publish and apply so that what Current() reports and
	// what the running components were configured with always describe the same
	// reload. SIGHUP, the file watcher and the HTTP endpoint can all land here
	// at once.
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()

	cfg, src, err := m.loader.Load()
	if err != nil {
		return nil, err
	}

	// Boot-only settings are reported as deferred and then pinned back to the
	// values actually in force, so Current() and the settings view describe the
	// running service rather than the file on disk.
	var deferred []string
	if cfg.Server.Addr != m.boot.Addr {
		deferred = append(deferred, "server.addr")
	}
	// Reported as the two separate keys they are, so deferred entries and
	// BootOnly() speak one vocabulary and a caller can match either list
	// against the settings view by key.
	if cfg.Server.TLSCert != m.boot.TLSCert {
		deferred = append(deferred, "server.tls_cert")
	}
	if cfg.Server.TLSKey != m.boot.TLSKey {
		deferred = append(deferred, "server.tls_key")
	}
	if cfg.Server.Watch != m.boot.Watch {
		deferred = append(deferred, "server.watch")
	}

	// Validate before publishing: a configuration this process cannot use must
	// leave the running one in place, exactly as a malformed file does.
	for _, v := range m.validators {
		if err := v(cfg); err != nil {
			return nil, err
		}
	}

	cfg.Server.Addr = m.boot.Addr
	cfg.Server.TLSCert = m.boot.TLSCert
	cfg.Server.TLSKey = m.boot.TLSKey
	cfg.Server.Watch = m.boot.Watch

	// Restore each pinned value's provenance along with the value itself. The
	// settings view is where an operator goes to resolve exactly this kind of
	// confusion, so a pair saying ":8080, from the config file" -- when the
	// file says :9090 -- is worse there than either half would be alone.
	for _, key := range bootOnlyKeys() {
		src[key] = m.bootSources[key]
	}

	// Values and provenance are published in one store, so no reader can
	// observe a value from this generation beside a source from the last.
	m.live.Store(&snapshot{cfg: cfg, src: src})

	// Apply the settings that genuinely can change at runtime. A failure here
	// is reported but does not roll back: the configuration is already live for
	// every component that read it successfully.
	var errs []string
	for _, a := range m.appliers {
		if err := a(cfg); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		return deferred, &ErrApply{Reasons: errs}
	}
	return deferred, nil
}

// BootOnly reports the settings that cannot change without a restart, for
// documentation and for the settings view.
func (m *Manager) BootOnly() []string { return bootOnlyKeys() }

// configFileDir is used by the file watcher to watch the directory rather than
// the file, so an editor that replaces the file still triggers a reload.
func configFileDir(path string) string { return filepath.Dir(path) }
