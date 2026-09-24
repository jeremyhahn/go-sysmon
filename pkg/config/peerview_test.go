package config_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jeremyhahn/go-sysmon/pkg/config"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// TestRedactPeerView_WithholdsTheBindAddress covers the part of #120 that the
// peer's own view does not do: server.addr is served verbatim by the node
// itself -- the caller already connected to it -- but an aggregator relaying it
// hands another machine's listening port to anyone who can name that machine.
//
// It is built from a real current-build view, so it also proves the relay
// changes nothing else such a view already withholds.
func TestRedactPeerView_WithholdsTheBindAddress(t *testing.T) {
	path := writeConfig(t, "server:\n  addr: 10.0.0.7:9443\n"+
		"alerts:\n  temp_crit_c: 88\n"+
		"fleet:\n  peers:\n    - name: fs\n      url: https://10.0.0.1:8081\n")
	mgr, err := config.NewManager(l(path))
	if err != nil {
		t.Fatalf("NewManager(): %v", err)
	}
	own := mgr.View()
	if v := entryValue(t, own, "server.addr"); v != "10.0.0.7:9443" {
		t.Fatalf("the node's own view shows server.addr = %q; the precondition is that it is served verbatim", v)
	}

	relayed := config.RedactPeerView(own)

	if v := entryValue(t, relayed, "server.addr"); v != config.ValueConfigured {
		t.Errorf("relayed server.addr = %q, want %q", v, config.ValueConfigured)
	}
	if len(relayed.Entries) != len(own.Entries) {
		t.Fatalf("relayed %d entries, want the peer's %d", len(relayed.Entries), len(own.Entries))
	}
	for i, e := range own.Entries {
		if e.Key == "server.addr" {
			continue
		}
		if relayed.Entries[i] != e {
			t.Errorf("relayed entry %+v, want the peer's own %+v unchanged", relayed.Entries[i], e)
		}
	}
	if relayed.ConfigFile != own.ConfigFile {
		t.Errorf("relayed config_file = %q, want the peer's own %q", relayed.ConfigFile, own.ConfigFile)
	}
	assertNoneOf(t, relayed, "10.0.0.7", "9443", "10.0.0.1", "8081")
}

// TestRedactPeerView_WithholdsWhatAnOlderPeerSendsVerbatim covers a peer on a
// build older than #16 or #37, which serves its topology in the clear. The
// aggregator's response is its own, so it withholds on its own authority
// rather than trusting the peer to have done it.
func TestRedactPeerView_WithholdsWhatAnOlderPeerSendsVerbatim(t *testing.T) {
	old := types.ConfigView{
		ConfigFile: "/etc/go-sysmon/sysmon.yaml",
		Entries: []types.ConfigEntry{
			{Section: "server", Key: "server.addr", Value: ":8081", Source: types.ConfigSourceFile},
			{Section: "server", Key: "server.tls_cert", Value: "/etc/go-sysmon/tls/node.pem", Source: types.ConfigSourceFile},
			{Section: "server", Key: "server.tls_key", Value: "", Source: types.ConfigSourceDefault},
			{Section: "alerts", Key: "alerts.cpu_pct", Value: "90", Source: types.ConfigSourceDefault},
			{Section: "fleet", Key: "fleet.tls.key_file", Value: "/etc/go-sysmon/tls/client-key.pem", Source: types.ConfigSourceFile},
			{Section: "fleet", Key: "fleet.tls.insecure_skip_verify", Value: "true", Source: types.ConfigSourceFile},
			{Section: "prometheus", Key: "prometheus.query_url", Value: "http://10.0.0.9:9090", Source: types.ConfigSourceFile},
			{Section: "prometheus", Key: "prometheus.remote_write_url", Value: config.ValueNotSet, Source: types.ConfigSourceDefault},
			{Section: "fleet", Key: "fleet.peers.db", Value: "http://10.0.0.2:8081", Source: types.ConfigSourceFile},
			{Section: "fleet", Key: "fleet.peers.http://10.0.0.3:8081", Value: "http://10.0.0.3:8081", Source: types.ConfigSourceFile},
			{Section: "fleet", Key: "fleet.peers.https://10.0.0.4:8443/", Value: "https://10.0.0.4:8443/", Source: types.ConfigSourceFile},
		},
	}

	got := config.RedactPeerView(old)

	want := map[string]string{
		"server.addr":                    config.ValueConfigured,
		"server.tls_cert":                config.ValueConfigured,
		"server.tls_key":                 config.ValueNotSet,
		"alerts.cpu_pct":                 "90",
		"fleet.tls.key_file":             config.ValueConfigured,
		"fleet.tls.insecure_skip_verify": "true",
		"prometheus.query_url":           config.ValueConfigured,
		"prometheus.remote_write_url":    config.ValueNotSet,
		"fleet.peers.db":                 config.ValueConfigured,
		"fleet.peers.(unnamed 1)":        config.ValueConfigured,
		"fleet.peers.(unnamed 2)":        config.ValueConfigured,
	}
	if len(got.Entries) != len(want) {
		t.Fatalf("relayed %d entries, want %d: %+v", len(got.Entries), len(want), got.Entries)
	}
	for key, value := range want {
		if v := entryValue(t, got, key); v != value {
			t.Errorf("relayed %s = %q, want %q", key, v, value)
		}
	}
	if got.ConfigFile != config.ValueConfigured {
		t.Errorf("relayed config_file = %q, want %q", got.ConfigFile, config.ValueConfigured)
	}
	assertNoneOf(t, got, "10.0.0.", ":8081", "8443", "/etc/go-sysmon")

	// The relay must not reach back into the peer's decoded view.
	if old.Entries[0].Value != ":8081" {
		t.Errorf("RedactPeerView modified its argument: server.addr is now %q", old.Entries[0].Value)
	}
}

// TestRedactPeerView_Empty covers a peer with nothing to report, which must
// come back as nothing rather than as a view claiming a file is configured.
func TestRedactPeerView_Empty(t *testing.T) {
	got := config.RedactPeerView(types.ConfigView{})
	if got.ConfigFile != config.ValueNotSet {
		t.Errorf("config_file = %q, want %q", got.ConfigFile, config.ValueNotSet)
	}
	if got.Entries == nil || len(got.Entries) != 0 {
		t.Errorf("entries = %#v, want an empty, non-nil list", got.Entries)
	}
}

// entryValue returns key's value in view, failing when it is absent.
func entryValue(t *testing.T, view types.ConfigView, key string) string {
	t.Helper()
	for _, e := range view.Entries {
		if e.Key == key {
			return e.Value
		}
	}
	t.Fatalf("view has no %s entry", key)
	return ""
}

// assertNoneOf fails when the view's JSON carries any of the given fragments.
func assertNoneOf(t *testing.T, view types.ConfigView, fragments ...string) {
	t.Helper()
	body, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, f := range fragments {
		if strings.Contains(string(body), f) {
			t.Errorf("relayed view carries %q: %s", f, body)
		}
	}
}
