package config

import (
	"fmt"
	"sort"
	"strconv"

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
		"alerts.swap_pct",
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
		"server.addr":               c.Server.Addr,
		"server.interval_ms":        c.Server.IntervalMs,
		"server.tls_cert":           c.Server.TLSCert,
		"server.tls_key":            c.Server.TLSKey,
		"server.watch":              c.Server.Watch,
		"alerts.disk_full_pct":      c.Alerts.DiskFullPct,
		"alerts.disk_full_crit_pct": c.Alerts.DiskFullCritPct,
		"alerts.disk_util_pct":      c.Alerts.DiskUtilPct,
		"alerts.disk_queue_len":     c.Alerts.DiskQueueLen,
		"alerts.temp_warn_c":        c.Alerts.TempWarnC,
		"alerts.temp_crit_c":        c.Alerts.TempCritC,
		"alerts.nvme_wear_pct":      c.Alerts.NVMeWearPct,
		"alerts.nvme_spare_pct":     c.Alerts.NVMeSparePct,
		"alerts.cpu_pct":            c.Alerts.CPUPct,
		"alerts.memory_pct":         c.Alerts.MemoryPct,
		"alerts.swap_pct":           c.Alerts.SwapPct,
		"alerts.psi_full_avg10":     c.Alerts.PSIFullAvg10,
		"alerts.load_per_core":      c.Alerts.LoadPerCore,
		"alerts.iface_err_rate":     c.Alerts.IfaceErrRate,
		"alerts.net_util_pct":       c.Alerts.NetUtilPct,
		"alerts.gpu_temp_warn_c":    c.Alerts.GPUTempWarnC,
		"alerts.gpu_temp_crit_c":    c.Alerts.GPUTempCritC,
		"alerts.gpu_memory_pct":     c.Alerts.GPUMemoryPct,
		"alerts.gpu_power_pct":      c.Alerts.GPUPowerPct,
		"alerts.container_mem_pct":  c.Alerts.ContainerMemPct,
		"alerts.container_cpu_pct":  c.Alerts.ContainerCPUPct,
		"fleet.local_name":          c.Fleet.LocalName,
		"fleet.poll_interval_ms":    c.Fleet.PollIntervalMs,
		"fleet.timeout_ms":          c.Fleet.TimeoutMs,

		"prometheus.remote_write_url":   c.Prometheus.RemoteWriteURL,
		"prometheus.query_url":          c.Prometheus.QueryURL,
		"prometheus.sample_interval_ms": c.Prometheus.SampleIntervalMs,
		"prometheus.flush_interval_ms":  c.Prometheus.FlushIntervalMs,
		"prometheus.batch_size":         c.Prometheus.BatchSize,
		"prometheus.timeout_ms":         c.Prometheus.TimeoutMs,
	}
}

// value renders a key's effective value for display.
func (c *Config) value(key string) string {
	switch key {
	case "server.addr":
		return c.Server.Addr
	case "server.interval_ms":
		return strconv.Itoa(c.Server.IntervalMs)
	case "server.tls_cert":
		return redactPath(c.Server.TLSCert)
	case "server.tls_key":
		return redactPath(c.Server.TLSKey)
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
	case "alerts.swap_pct":
		return num(c.Alerts.SwapPct)
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
	// The two URLs are reported as configured-or-not rather than verbatim, the
	// same way TLS paths are. This endpoint has no authentication of its own,
	// and the address of the fleet's time-series database is reconnaissance
	// that anyone able to reach the dashboard should not be handed. Whether
	// history is switched on is the part an operator actually needs here.
	case "prometheus.remote_write_url":
		return redactPath(c.Prometheus.RemoteWriteURL)
	case "prometheus.query_url":
		return redactPath(c.Prometheus.QueryURL)
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

// ValueConfigured and ValueNotSet stand in for a filesystem path in the
// settings view, so an operator can still tell whether TLS is set up.
const (
	ValueConfigured = "(configured)"
	ValueNotSet     = "(not set)"
)

// redactPath reports whether a path is configured without disclosing it. The
// settings endpoint carries no authentication of its own -- it is designed to
// sit behind an authenticating proxy -- and the location of a certificate and
// its private key is reconnaissance that anyone reaching the endpoint should
// not be handed for free.
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

// View builds the read-only settings payload: every effective value with the
// source that supplied it, plus the configured peers.
func (m *Manager) View() types.ConfigView {
	// One load, not Current() plus Sources(): the whole point of this payload is
	// that each value sits beside the layer that supplied THAT value, so both
	// halves have to come from the same published generation.
	live := m.live.Load()
	if live == nil {
		return types.ConfigView{ConfigFile: m.Path()}
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
		entries = append(entries, types.ConfigEntry{
			Section: "fleet",
			Key:     fmt.Sprintf("fleet.peers.%s", p.Name),
			Value:   p.URL,
			Source:  peerSource,
		})
	}

	return types.ConfigView{ConfigFile: m.Path(), Entries: entries}
}
