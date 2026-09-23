package config

import (
	"sort"
	"strconv"
	"strings"

	"github.com/jeremyhahn/go-sysmon/pkg/nodename"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// scalarKeys lists every simple setting, in the order the settings view shows
// them. Peers are a list and are reported separately.
func scalarKeys() []string {
	return []string{
		"server.addr",
		"server.interval_ms",
		"server.tls_cert",
		"server.tls_key",
		"server.watch",
		"alerts.disk_full_pct",
		"alerts.disk_full_crit_pct",
		"alerts.disk_util_pct",
		"alerts.disk_queue_len",
		"alerts.temp_warn_c",
		"alerts.temp_crit_c",
		"alerts.nvme_wear_pct",
		"alerts.nvme_spare_pct",
		"alerts.cpu_pct",
		"alerts.memory_pct",
		"alerts.swap_out_pages_per_sec",
		"alerts.psi_full_avg10",
		"alerts.load_per_core",
		"alerts.iface_err_rate",
		"alerts.net_util_pct",
		"alerts.gpu_temp_warn_c",
		"alerts.gpu_temp_crit_c",
		"alerts.gpu_memory_pct",
		"alerts.gpu_power_pct",
		"alerts.container_mem_pct",
		"alerts.container_cpu_pct",
		"fleet.local_name",
		"fleet.poll_interval_ms",
		"fleet.timeout_ms",
		"fleet.tls.ca_file",
		"fleet.tls.cert_file",
		"fleet.tls.key_file",
		"fleet.tls.insecure_skip_verify",
		"fleet.tls.require_tls",
		"prometheus.remote_write_url",
		"prometheus.query_url",
		"prometheus.sample_interval_ms",
		"prometheus.flush_interval_ms",
		"prometheus.batch_size",
		"prometheus.timeout_ms",
	}
}

// defaultMap flattens a Config into the dotted keys viper seeds as defaults.
func defaultMap(c Config) map[string]any {
	return map[string]any{
		"server.addr":                   c.Server.Addr,
		"server.interval_ms":            c.Server.IntervalMs,
		"server.tls_cert":               c.Server.TLSCert,
		"server.tls_key":                c.Server.TLSKey,
		"server.watch":                  c.Server.Watch,
		"alerts.disk_full_pct":          c.Alerts.DiskFullPct,
		"alerts.disk_full_crit_pct":     c.Alerts.DiskFullCritPct,
		"alerts.disk_util_pct":          c.Alerts.DiskUtilPct,
		"alerts.disk_queue_len":         c.Alerts.DiskQueueLen,
		"alerts.temp_warn_c":            c.Alerts.TempWarnC,
		"alerts.temp_crit_c":            c.Alerts.TempCritC,
		"alerts.nvme_wear_pct":          c.Alerts.NVMeWearPct,
		"alerts.nvme_spare_pct":         c.Alerts.NVMeSparePct,
		"alerts.cpu_pct":                c.Alerts.CPUPct,
		"alerts.memory_pct":             c.Alerts.MemoryPct,
		"alerts.swap_out_pages_per_sec": c.Alerts.SwapOutPagesPerSec,
		"alerts.psi_full_avg10":         c.Alerts.PSIFullAvg10,
		"alerts.load_per_core":          c.Alerts.LoadPerCore,
		"alerts.iface_err_rate":         c.Alerts.IfaceErrRate,
		"alerts.net_util_pct":           c.Alerts.NetUtilPct,
		"alerts.gpu_temp_warn_c":        c.Alerts.GPUTempWarnC,
		"alerts.gpu_temp_crit_c":        c.Alerts.GPUTempCritC,
		"alerts.gpu_memory_pct":         c.Alerts.GPUMemoryPct,
		"alerts.gpu_power_pct":          c.Alerts.GPUPowerPct,
		"alerts.container_mem_pct":      c.Alerts.ContainerMemPct,
		"alerts.container_cpu_pct":      c.Alerts.ContainerCPUPct,
		"fleet.local_name":              c.Fleet.LocalName,
		"fleet.poll_interval_ms":        c.Fleet.PollIntervalMs,
		"fleet.timeout_ms":              c.Fleet.TimeoutMs,

		"prometheus.remote_write_url":   c.Prometheus.RemoteWriteURL,
		"prometheus.query_url":          c.Prometheus.QueryURL,
		"prometheus.sample_interval_ms": c.Prometheus.SampleIntervalMs,
		"prometheus.flush_interval_ms":  c.Prometheus.FlushIntervalMs,
		"prometheus.batch_size":         c.Prometheus.BatchSize,
		"prometheus.timeout_ms":         c.Prometheus.TimeoutMs,

		"fleet.tls.ca_file":              c.Fleet.TLS.CAFile,
		"fleet.tls.cert_file":            c.Fleet.TLS.CertFile,
		"fleet.tls.key_file":             c.Fleet.TLS.KeyFile,
		"fleet.tls.insecure_skip_verify": c.Fleet.TLS.InsecureSkipVerify,
		"fleet.tls.require_tls":          c.Fleet.TLS.RequireTLS,
	}
}

// withheld are the settings reported as configured-or-not rather than
// verbatim, by redactPath. It is the one list of them: this node's own view and
// the view it relays for a peer (RedactPeerView) both read it, so a setting
// cannot be withheld on one path and served on the other.
//
// The paths are the certificate and key locations, the node's and the fleet's:
// where a node keeps its private key is not something an unauthenticated
// dashboard should hand out. The URLs are the Prometheus endpoints: the address
// of the fleet's time-series database is reconnaissance anyone able to reach
// the dashboard should not be handed, and whether history is switched on is
// the part an operator needs. fleet.tls's two booleans are NOT here -- their
// whole purpose is to be auditable at a glance, and a fleet running with
// verification off should say so.
var withheld = map[string]bool{
	"server.tls_cert":             true,
	"server.tls_key":              true,
	"fleet.tls.ca_file":           true,
	"fleet.tls.cert_file":         true,
	"fleet.tls.key_file":          true,
	"prometheus.remote_write_url": true,
	"prometheus.query_url":        true,
}

// value renders a key's effective value for display, withholding the settings
// in withheld.
func (c *Config) value(key string) string {
	v := c.raw(key)
	if withheld[key] {
		return redactPath(v)
	}
	return v
}

// raw renders a key's effective value verbatim. Only value may call it: raw is
// the configuration as held, not as it may be served.
func (c *Config) raw(key string) string {
	switch key {
	case "server.addr":
		return c.Server.Addr
	case "server.interval_ms":
		return strconv.Itoa(c.Server.IntervalMs)
	case "server.tls_cert":
		return c.Server.TLSCert
	case "server.tls_key":
		return c.Server.TLSKey
	case "server.watch":
		return strconv.FormatBool(c.Server.Watch)
	case "alerts.disk_full_pct":
		return num(c.Alerts.DiskFullPct)
	case "alerts.disk_full_crit_pct":
		return num(c.Alerts.DiskFullCritPct)
	case "alerts.disk_util_pct":
		return num(c.Alerts.DiskUtilPct)
	case "alerts.disk_queue_len":
		return num(c.Alerts.DiskQueueLen)
	case "alerts.temp_warn_c":
		return num(c.Alerts.TempWarnC)
	case "alerts.temp_crit_c":
		return num(c.Alerts.TempCritC)
	case "alerts.nvme_wear_pct":
		return num(c.Alerts.NVMeWearPct)
	case "alerts.nvme_spare_pct":
		return num(c.Alerts.NVMeSparePct)
	case "alerts.cpu_pct":
		return num(c.Alerts.CPUPct)
	case "alerts.memory_pct":
		return num(c.Alerts.MemoryPct)
	case "alerts.swap_out_pages_per_sec":
		return num(c.Alerts.SwapOutPagesPerSec)
	case "alerts.psi_full_avg10":
		return num(c.Alerts.PSIFullAvg10)
	case "alerts.load_per_core":
		return num(c.Alerts.LoadPerCore)
	case "alerts.iface_err_rate":
		return num(c.Alerts.IfaceErrRate)
	case "alerts.net_util_pct":
		return num(c.Alerts.NetUtilPct)
	case "alerts.gpu_temp_warn_c":
		return num(c.Alerts.GPUTempWarnC)
	case "alerts.gpu_temp_crit_c":
		return num(c.Alerts.GPUTempCritC)
	case "alerts.gpu_memory_pct":
		return num(c.Alerts.GPUMemoryPct)
	case "alerts.gpu_power_pct":
		return num(c.Alerts.GPUPowerPct)
	case "alerts.container_mem_pct":
		return num(c.Alerts.ContainerMemPct)
	case "alerts.container_cpu_pct":
		return num(c.Alerts.ContainerCPUPct)
	case "fleet.local_name":
		return c.Fleet.LocalName
	case "fleet.poll_interval_ms":
		return strconv.Itoa(c.Fleet.PollIntervalMs)
	case "fleet.timeout_ms":
		return strconv.Itoa(c.Fleet.TimeoutMs)
	case "fleet.tls.ca_file":
		return c.Fleet.TLS.CAFile
	case "fleet.tls.cert_file":
		return c.Fleet.TLS.CertFile
	case "fleet.tls.key_file":
		return c.Fleet.TLS.KeyFile
	case "fleet.tls.insecure_skip_verify":
		return strconv.FormatBool(c.Fleet.TLS.InsecureSkipVerify)
	case "fleet.tls.require_tls":
		return strconv.FormatBool(c.Fleet.TLS.RequireTLS)
	case "prometheus.remote_write_url":
		return c.Prometheus.RemoteWriteURL
	case "prometheus.query_url":
		return c.Prometheus.QueryURL
	case "prometheus.sample_interval_ms":
		return strconv.Itoa(c.Prometheus.SampleIntervalMs)
	case "prometheus.flush_interval_ms":
		return strconv.Itoa(c.Prometheus.FlushIntervalMs)
	case "prometheus.batch_size":
		return strconv.Itoa(c.Prometheus.BatchSize)
	case "prometheus.timeout_ms":
		return strconv.Itoa(c.Prometheus.TimeoutMs)
	}
	return ""
}

// ValueConfigured and ValueNotSet stand in for a withheld value in the settings
// view, so an operator can still tell whether TLS, a peer or the Prometheus
// integration is set up.
const (
	ValueConfigured = "(configured)"
	ValueNotSet     = "(not set)"
)

// redactPath reports whether a filesystem path or a URL is configured without
// disclosing it. The settings endpoint carries no authentication of its own --
// it is designed to sit behind an authenticating proxy -- and the location of a
// certificate and its private key, the address of the fleet's time-series
// database and the URL of another node are reconnaissance that anyone reaching
// the endpoint should not be handed for free.
func redactPath(p string) string {
	if p == "" {
		return ValueNotSet
	}
	return ValueConfigured
}

// num formats a threshold without trailing zeros, so 90 reads as "90".
func num(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// section returns the settings group a key belongs to.
func section(key string) string {
	for i := 0; i < len(key); i++ {
		if key[i] == '.' {
			return key[:i]
		}
	}
	return key
}

// configFileState reports whether a configuration file is in use without
// disclosing where it is, for the same reason redactPath exists: the settings
// endpoint carries no authentication of its own. An absolute path is a map of
// the host's filesystem layout handed to anyone who can reach the port, and
// "am I running on a file or on defaults" is the part an operator standing in
// front of this dialog actually needs. Where the file lives is answered by the
// unit and by --config, both of which require being on the machine.
func configFileState(loaded bool) string {
	if loaded {
		return ValueConfigured
	}
	return ValueNotSet
}

// View builds the read-only settings payload: every effective value with the
// source that supplied it, plus the configured peers by name.
//
// It is deliberately not a copy of the configuration. GET /api/config has no
// authentication -- go-sysmon is designed to sit behind an authenticating proxy
// -- so the values that describe the internal topology rather than this node's
// behaviour are reported as configured-or-not: the config file's path, the peer
// URLs, the TLS key and certificate paths and the Prometheus addresses. Between
// them they gave one reachable listener the fleet's hostnames, ports, transport
// choices and filesystem layout (#16). What survives is what the dialog is for:
// which settings are in force, and which layer supplied each of them.
func (m *Manager) View() types.ConfigView {
	// One load, not Current() plus Sources(): the whole point of this payload is
	// that each value sits beside the layer that supplied THAT value, so both
	// halves have to come from the same published generation.
	live := m.live.Load()
	if live == nil {
		return types.ConfigView{ConfigFile: configFileState(false)}
	}
	cfg, src := live.cfg, live.src

	entries := make([]types.ConfigEntry, 0, len(scalarKeys())+len(cfg.Fleet.Peers))
	for _, key := range scalarKeys() {
		s := src[key]
		if s == "" {
			s = SourceDefault
		}
		entries = append(entries, types.ConfigEntry{
			Section: section(key),
			Key:     key,
			Value:   cfg.value(key),
			Source:  s,
		})
	}

	peerSource := src["fleet.peers"]
	if peerSource == "" {
		peerSource = SourceDefault
	}
	peers := make([]Peer, len(cfg.Fleet.Peers))
	copy(peers, cfg.Fleet.Peers)
	sort.Slice(peers, func(i, j int) bool { return peers[i].Name < peers[j].Name })
	for _, p := range peers {
		// A peer is keyed by its name, kept deliberately: it is how the
		// dashboard, the fleet proxy and the history queries all address a node,
		// and it is the peer's identity everywhere an operator meets it. The URL
		// behind it is the scheme, host and port of another machine on the
		// internal network, and nothing on this page needs it -- the browser
		// reaches a peer through /api/fleet/{name}/... on this node, never by
		// dialling it.
		//
		// Every name can be shown. A name is held to nodename.Rule at load, which
		// admits no scheme and no path, so no name can carry a URL back onto the
		// wire. An unnamed peer is named after its URL's host (#37), and that
		// host is then its fleet identity -- the same string GET /api/fleet
		// titles its card with and routes it by -- so withholding it here would
		// hide nothing.
		entries = append(entries, types.ConfigEntry{
			Section: "fleet",
			Key:     peerKeyPrefix + p.Name,
			Value:   redactPath(p.URL),
			Source:  peerSource,
		})
	}

	return types.ConfigView{ConfigFile: configFileState(live.fileLoaded), Entries: entries}
}

// peerKeyPrefix begins every configured peer's entry in the settings view; the
// rest of the key is the peer's name.
const peerKeyPrefix = "fleet.peers."

// relayWithheld are the settings withheld only when one node relays another's
// view, on top of withheld.
//
// server.addr is the node's bind address. Served by the node itself it tells
// the caller nothing it did not need to connect; relayed by an aggregator
// through /api/fleet/{name}/config it is another machine's listening port,
// handed to anyone who can resolve the peer's name -- part of the topology
// GET /api/fleet withholds (#120).
var relayWithheld = map[string]bool{
	"server.addr": true,
}

// RedactPeerView withholds a peer's topology from its settings view before an
// aggregator relays it, as View withholds this node's own.
//
// The view is the peer's own GET /api/config, so a current build has already
// withheld everything in withheld and this changes nothing there. It is applied
// anyway, because the aggregator cannot know what the peer runs: a build older
// than #16 sends its certificate paths, its Prometheus addresses, its config
// file's path and its peers' URLs verbatim, and one older than #37 keys an
// unnamed peer by its whole URL. What the aggregator relays is its own
// response, and it withholds on its own authority.
//
// A peer entry whose name is not a usable node name can only be a URL carried
// in the key; it is re-keyed positionally, "fleet.peers.(unnamed 1)", which
// keeps the count of peers and the fact that each is configured.
func RedactPeerView(view types.ConfigView) types.ConfigView {
	out := types.ConfigView{
		ConfigFile: redactRelayed(view.ConfigFile),
		Entries:    make([]types.ConfigEntry, 0, len(view.Entries)),
	}
	unnamed := 0
	for _, e := range view.Entries {
		if name, ok := strings.CutPrefix(e.Key, peerKeyPrefix); ok {
			if nodename.Validate(name) != nil {
				unnamed++
				e.Key = peerKeyPrefix + "(unnamed " + strconv.Itoa(unnamed) + ")"
			}
			e.Value = redactRelayed(e.Value)
		} else if withheld[e.Key] || relayWithheld[e.Key] {
			e.Value = redactRelayed(e.Value)
		}
		out.Entries = append(out.Entries, e)
	}
	return out
}

// redactRelayed is redactPath for a value that may already have been through
// it: a relayed "(not set)" stays not set rather than reading as a value that
// is configured.
func redactRelayed(v string) string {
	if v == ValueNotSet {
		return v
	}
	return redactPath(v)
}
