//go:build integration

package fleet

import (
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/internal/deadpeer"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
	"github.com/jeremyhahn/go-sysmon/test/integration/harness"
)

// A flat fleet: one aggregator polling leaves directly. One real leaf, and a
// stand-in for every way a peer can fail, all polled by one aggregator in one
// cycle.

// misbehaving is every stand-in peer the flat-fleet tests poll, by name.
type misbehaving struct {
	peers []peer
	// secure is an https peer the aggregator trusts; old serves no summary.
	secure, old, mtls, redirected *standIn
	// elsewhere is the host an off-host redirect points at.
	elsewhere *standIn
	caPath    string
	client    issued
}

// oldSnapshot is the whole snapshot a peer too old to serve a summary sends:
// a critical alert, a degraded array, SMART counts and a measured network.
func oldSnapshot() types.Snapshot {
	return types.Snapshot{
		Timestamp: time.Unix(1_790_000_000, 0).UTC(),
		NodeName:  "old",
		Version:   "v0.9.0",
		GitCommit: "0123456789abcdef",
		Host:      types.HostInfo{Hostname: "old.example", Uptime: 7200, BootTime: 1_789_990_000},
		Memory:    types.MemoryInfo{UsedPercent: 71, SwapCountersAvailable: true, PageSizeBytes: 4096, SwapInPages: 10, SwapOutPages: 20},
		Disks: []types.DiskInfo{
			{Name: "md0", TotalBytes: 1 << 40, UsedPercent: 88, RAID: &types.RAIDInfo{Level: "raid1", State: "degraded", Degraded: true}},
			{Name: "sda", TotalBytes: 1 << 39, UsedPercent: 12, SMARTEnabled: true, SMARTHealthy: false, Temperature: 51,
				Members: []types.DiskMember{
					{Serial: "S1", SMARTEnabled: true, SMARTHealthy: true, Temperature: 38},
					{Serial: "S1", SMARTEnabled: true, SMARTHealthy: true},
					{Serial: "", SMARTEnabled: true, SMARTHealthy: false},
					{SMARTEnabled: false},
				}},
		},
		Networks: []types.NetworkInfo{
			{Name: "lo", IsLoopback: true},
			{Name: "eth0", RatesAvailable: true, BytesRecvRate: 1000, BytesSentRate: 500},
			{Name: "br0", IsVirtual: true},
		},
		Alerts: []types.Alert{
			alertOf(types.SeverityCritical, "raid_degraded", "md0"),
			alertOf(types.SeverityWarning, "disk_usage", "md0"),
			alertOf(types.SeverityInfo, "note", "sda"),
		},
	}
}

// startMisbehaving starts the stand-ins and says how each is polled.
func startMisbehaving(t *testing.T) *misbehaving {
	t.Helper()
	ca := newAuthority(t, "fleet")
	rogue := newAuthority(t, "rogue")
	clientCA := newAuthority(t, "clients")
	m := &misbehaving{caPath: ca.pemPath, client: clientCA.issue(t, "aggregator", leafOptions{client: true})}

	// Dead: the port is held, and nothing ever accepts on it.
	dead := deadpeer.URL(t)

	// Slow: answers nothing until the aggregator gives up.
	slow := newStandIn(t)
	slow.handle("/api/summary", func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })

	broken := newStandIn(t)
	broken.handle("/api/summary", rawAnswer(http.StatusInternalServerError, "text/plain", "boom"))

	// Garbled: a 200 that is not JSON on every path -- an older build's
	// catch-all page, and then not a snapshot either.
	garbled := newStandIn(t)
	garbled.handle("/api/summary", rawAnswer(http.StatusOK, "text/html", "<html>go-sysmon is running</html>"))
	garbled.handle("/api/snapshot", rawAnswer(http.StatusOK, "text/html", "<html>go-sysmon is running</html>"))

	// Truncated: promises more than it sends, then hangs up.
	truncated := newStandIn(t)
	truncated.handle("/api/summary", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "4096")
		if _, err := w.Write([]byte(`{"hostname":"trunc`)); err != nil {
			return
		}
	})

	// Huge: a summary far past the bound; it is cut off, and what is left
	// is not JSON.
	huge := newStandIn(t)
	huge.handle("/api/summary", rawAnswer(http.StatusOK, "application/json",
		`{"hostname":"`+strings.Repeat("h", 2<<20)+`"}`))

	m.old = newStandIn(t)
	m.old.handle("/api/snapshot", jsonAnswer(oldSnapshot()))

	m.redirected = newStandIn(t)
	m.redirected.handle("/api/summary", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/v2/summary", http.StatusTemporaryRedirect)
	})
	m.redirected.handle("/v2/summary", jsonAnswer(leafSummary("redirected")))

	m.elsewhere = newStandIn(t)
	m.elsewhere.handle("/api/summary", jsonAnswer(leafSummary("offhost")))
	offhost := newStandIn(t)
	offhost.handle("/api/summary", func(w http.ResponseWriter, r *http.Request) {
		_, port, err := net.SplitHostPort(hostPort(t, m.elsewhere.URL))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, "http://localhost:"+port+"/api/summary", http.StatusFound)
	})

	scheme := newStandIn(t)
	scheme.handle("/api/summary", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "ftp://127.0.0.1/api/summary", http.StatusFound)
	})

	loop := newStandIn(t)
	loop.handle("/api/summary", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/api/summary", http.StatusFound)
	})

	// https at an address that answers in the clear.
	plain := newStandIn(t)

	m.secure = newTLSStandIn(t, ca.issue(t, "secure", leafOptions{}).pair, nil)
	m.secure.handle("/api/summary", jsonAnswer(leafSummary("secure")))
	untrusted := newTLSStandIn(t, rogue.issue(t, "untrusted", leafOptions{}).pair, nil)
	untrusted.handle("/api/summary", jsonAnswer(leafSummary("untrusted")))
	wrongHost := newTLSStandIn(t, ca.issue(t, "wronghost", leafOptions{dnsNames: []string{"elsewhere.example"}}).pair, nil)
	wrongHost.handle("/api/summary", jsonAnswer(leafSummary("wronghost")))
	expired := newTLSStandIn(t, ca.issue(t, "expired", leafOptions{expired: true}).pair, nil)
	expired.handle("/api/summary", jsonAnswer(leafSummary("expired")))
	m.mtls = newTLSStandIn(t, ca.issue(t, "mtls", leafOptions{}).pair, clientCA.pool)
	m.mtls.handle("/api/summary", jsonAnswer(leafSummary("mtls")))
	// An https peer redirecting to plain http on the same host and port.
	downgrade := newTLSStandIn(t, ca.issue(t, "downgrade", leafOptions{}).pair, nil)
	downgrade.handle("/api/summary", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://"+r.Host+"/api/summary", http.StatusFound)
	})

	m.peers = []peer{
		{"broken", broken.URL}, {"dead", dead}, {"downgrade", downgrade.URL}, {"expired", expired.URL}, {"garbled", garbled.URL},
		{"huge", huge.URL}, {"loop", loop.URL}, {"mtls", m.mtls.URL}, {"offhost", offhost.URL},
		{"old", m.old.URL}, {"plain", "https://" + hostPort(t, plain.URL)}, {"redirected", m.redirected.URL},
		{"scheme", scheme.URL}, {"secure", m.secure.URL}, {"slow", slow.URL}, {"truncated", truncated.URL},
		{"untrusted", untrusted.URL}, {"wronghost", wrongHost.URL},
	}
	return m
}

// tlsExtra is the fleet.tls block trusting ca and presenting client.
func tlsExtra(caPath string, client issued) string {
	return "  tls:\n    ca_file: " + caPath + "\n    cert_file: " + client.certPath + "\n    key_file: " + client.keyPath + "\n"
}

// TestFlat_EveryPeerFailureIsNamedWithoutItsAddress polls every kind of
// failing peer at once and checks what the fleet view says of each: why it is
// unreachable, in words that carry no address (#120), and what the roll-up
// makes of the whole fleet.
func TestFlat_EveryPeerFailureIsNamedWithoutItsAddress(t *testing.T) {
	m := startMisbehaving(t)
	leaf := harness.Serve(t, harness.ServeOptions{Config: "fleet:\n  local_name: live\n"})
	peers := append([]peer{{"live", "http://" + leaf.Addr}}, m.peers...)
	agg := harness.Serve(t, harness.ServeOptions{Config: fleetYAML("hub", onePoll, 1500, peers, tlsExtra(m.caPath, m.client)),
		Args: []string{"--log-level", "debug"}})

	v := awaitFleet(t, agg.Addr, "the one poll of every peer", polled)

	want := map[string]string{
		"broken":    "peer broken returned HTTP 500",
		"dead":      "fleet: request to peer dead failed: connection refused",
		"downgrade": "fleet: request to peer downgrade failed: request failed; see this node's log",
		"expired":   "fleet: request to peer expired failed: certificate has expired or is not yet valid",
		"garbled":   "fleet: request to peer garbled failed: malformed response: invalid character '<' looking for beginning of value",
		"huge":      "peer huge returned HTTP 404",
		"loop":      "fleet: request to peer loop failed: request failed; see this node's log",
		"offhost":   "fleet: request to peer offhost failed: request failed; see this node's log",
		"plain":     "fleet: request to peer plain failed: peer did not answer with TLS",
		"scheme":    "fleet: request to peer scheme failed: request failed; see this node's log",
		"slow":      "fleet: request to peer slow failed: timed out",
		"truncated": "fleet: request to peer truncated failed: connection closed before a complete response",
		"untrusted": "fleet: request to peer untrusted failed: certificate signed by an unknown authority",
		"wronghost": "fleet: request to peer wronghost failed: certificate is not valid for the peer's host name",
	}
	for name, msg := range want {
		n := node(t, v, name)
		if n.Reachable || n.Error != msg || n.Status != types.NodeStatusUnreachable {
			t.Errorf("%s = reachable %v, status %s, error %q; want unreachable with %q", name, n.Reachable, n.Status, n.Error, msg)
		}
		if n.AgeSeconds != 0 {
			t.Errorf("%s never answered, and has an age of %v; want 0", name, n.AgeSeconds)
		}
	}
	for _, n := range v.Nodes {
		if strings.Contains(n.Error, "127.0.0.1") || strings.Contains(n.Error, "localhost") {
			t.Errorf("%s's error names an address: %q", n.Name, n.Error)
		}
	}
	if got := m.elsewhere.hitsOf("/api/summary"); got != 0 {
		t.Errorf("a redirect off the configured host was followed %d times; want never", got)
	}
	// What the served error leaves out, this node's own log keeps: the whole
	// chain, for the operator.
	for peer, reason := range map[string]string{
		"offhost":   "redirect leaves the configured host",
		"scheme":    `scheme "ftp" is not http or https`,
		"loop":      "more than 5 redirects",
		"downgrade": "refuses to downgrade https to http",
	} {
		rec := agg.Logs.Await(t, "the debug log of "+peer+"'s failure", func(r harness.LogRecord) bool {
			return r.Msg() == "fleet: peer unreachable" && r["peer"] == peer
		})
		if msg, _ := rec["cause"].(string); !strings.Contains(msg, reason) {
			t.Errorf("%s's failure in this node's log = %q; want the refused redirect named: %q", peer, msg, reason)
		}
	}

	// The peers that did answer, and how each was read.
	for name, source := range map[string]types.SummarySource{
		"live": types.SummarySourceSummary, "secure": types.SummarySourceSummary, "mtls": types.SummarySourceSummary,
		"redirected": types.SummarySourceSummary, "old": types.SummarySourceSnapshot,
	} {
		if n := node(t, v, name); !n.Reachable || n.Source != source || n.Error != "" {
			t.Errorf("%s = reachable %v via %q (%q); want reachable via %q", name, n.Reachable, n.Source, n.Error, source)
		}
	}
	if got := m.mtls.headerOf("/api/summary", "X-Sysmon-Fleet-Ancestry"); len(got) != 26 {
		t.Errorf("a poll carried ancestry %q; want this aggregator's 26-character instance ID", got)
	}

	// The old peer, summarised here from its whole snapshot.
	old := node(t, v, "old")
	switch {
	case old.Status != types.NodeStatusCritical || old.CriticalCount != 1 || old.WarningCount != 1 || len(old.Alerts) != 2:
		t.Errorf("old = status %s, %d critical, %d warning, alerts %v; want critical, 1 and 1, the info alert dropped",
			old.Status, old.CriticalCount, old.WarningCount, old.Alerts)
	case old.RAIDArrays != 1 || old.RAIDUnhealthy != 1 || !old.RAIDDegraded:
		t.Errorf("old RAID = %d arrays, %d unhealthy, degraded %v; want 1, 1, true", old.RAIDArrays, old.RAIDUnhealthy, old.RAIDDegraded)
	case old.SMARTCapable != 3 || old.SMARTUnhealthy != 2:
		// sda itself, S1 once, and the member with no serial.
		t.Errorf("old SMART = %d capable, %d unhealthy; want 3 and 2 (S1 counted once)", old.SMARTCapable, old.SMARTUnhealthy)
	case !old.NetReported || old.NetRxRate != 1000 || old.NetTxRate != 500:
		t.Errorf("old network = reported %v, %v/%v; want eth0's 1000/500 alone", old.NetReported, old.NetRxRate, old.NetTxRate)
	case old.MaxTempC != 51 || old.MaxTempSource != types.TempDrive:
		t.Errorf("old hottest = %v from %s; want sda's 51 from a drive", old.MaxTempC, old.MaxTempSource)
	case old.WorstDiskPercent != 88:
		t.Errorf("old fullest disk = %v; want 88", old.WorstDiskPercent)
	case old.BuildMatch != types.BuildUnknown || old.NameMatch != types.NameSame:
		t.Errorf("old build %s, name %s; want unknown (this build states no commit) and same", old.BuildMatch, old.NameMatch)
	}

	r := v.Rollup
	if r == nil {
		t.Fatal("no roll-up")
	}
	// The hub and the live leaf both run on this host, so their alerts are its
	// hardware's business: a runner at 80°C makes both critical. The fixture
	// peers are the ones this test set, so the roll-up is checked against them
	// plus whatever the two real nodes say of themselves -- not against a host
	// assumed to be quiet.
	critical := 1
	for _, name := range []string{"hub", "live"} {
		if node(t, v, name).Status == types.NodeStatusCritical {
			critical++
		}
	}
	unreachable := len(want)
	if r.Nodes != len(peers)+1 || r.Unreachable != unreachable || r.NeverAnswered != unreachable || r.Critical != critical {
		t.Errorf("roll-up = %d nodes, %d unreachable, %d never answered, %d critical; want %d, %d, %d, %d",
			r.Nodes, r.Unreachable, r.NeverAnswered, r.Critical, len(peers)+1, unreachable, unreachable, critical)
	}
	oldNeedsAttention := false
	for _, a := range r.Attention {
		if a.Status != types.NodeStatusCritical {
			// Critical nodes come first; nothing after them outranks old.
			break
		}
		oldNeedsAttention = oldNeedsAttention || a.Name == "old"
	}
	if !oldNeedsAttention {
		t.Errorf("attention = %+v; want old among the critical nodes, which come first", r.Attention)
	}
	if r.RAID == nil || r.RAID.Unhealthy != 1 || r.SMART == nil || r.SMART.Unhealthy != 2 {
		t.Errorf("roll-up devices: RAID %+v, SMART %+v; want the old peer's 1 and 2 unhealthy", r.RAID, r.SMART)
	}
}

// TestFlat_ARedirectOnTheSameHostIsFollowed checks the one redirect a poll
// takes: same scheme, same host.
func TestFlat_ARedirectOnTheSameHostIsFollowed(t *testing.T) {
	m := startMisbehaving(t)
	agg := harness.Serve(t, harness.ServeOptions{Config: fleetYAML("hub", onePoll, 1500,
		[]peer{{"redirected", m.redirected.URL}}, "")})

	v := awaitFleet(t, agg.Addr, "the redirected peer to be polled", polled)
	if n := node(t, v, "redirected"); !n.Reachable || n.Hostname != "redirected.example" {
		t.Errorf("redirected = %+v; want its summary, fetched from where it moved to", n)
	}
	if got := m.redirected.hitsOf("/v2/summary"); got != 1 {
		t.Errorf("the redirect's target was asked %d times; want once", got)
	}
}

// TestFlat_ClientCertificateIsRequired checks the other half of mTLS: an
// aggregator with no client certificate is refused by a peer that requires
// one, and says so.
func TestFlat_ClientCertificateIsRequired(t *testing.T) {
	m := startMisbehaving(t)
	agg := harness.Serve(t, harness.ServeOptions{Config: fleetYAML("hub", onePoll, 1500,
		[]peer{{"mtls", m.mtls.URL}, {"secure", m.secure.URL}}, "  tls:\n    ca_file: "+m.caPath+"\n")})

	v := awaitFleet(t, agg.Addr, "both https peers to be polled", polled)
	if n := node(t, v, "mtls"); n.Reachable || !strings.HasPrefix(n.Error, "fleet: request to peer mtls failed: peer refused the TLS handshake: ") {
		t.Errorf("mtls without a client certificate = %+v; want the refused handshake named", n)
	}
	if n := node(t, v, "secure"); !n.Reachable {
		t.Errorf("secure = %+v; want reachable: it needs no client certificate", n)
	}
}

// TestFlat_InsecureSkipVerifyIsAnnounced checks that turning verification off
// is said once, loudly, at startup, and does what it says.
func TestFlat_InsecureSkipVerifyIsAnnounced(t *testing.T) {
	rogue := newAuthority(t, "rogue")
	untrusted := newTLSStandIn(t, rogue.issue(t, "untrusted", leafOptions{}).pair, nil)
	untrusted.handle("/api/summary", jsonAnswer(leafSummary("untrusted")))
	agg := harness.Serve(t, harness.ServeOptions{Config: fleetYAML("hub", onePoll, 1500,
		[]peer{{"untrusted", untrusted.URL}}, "  tls:\n    insecure_skip_verify: true\n")})

	warn := agg.Logs.Await(t, "the startup warning", func(r harness.LogRecord) bool {
		return strings.HasPrefix(r.Msg(), "fleet: peer certificate verification is DISABLED")
	})
	if warn.Level() != "WARN" {
		t.Errorf("verification off logged at %s; want WARN", warn.Level())
	}
	v := awaitFleet(t, agg.Addr, "the untrusted peer to be polled", polled)
	if n := node(t, v, "untrusted"); !n.Reachable {
		t.Errorf("untrusted with verification off = %+v; want reachable", n)
	}
}

// TestFlat_RequireTLSRefusesAPlaintextPeer checks require_tls: an http peer is
// never spoken to, and the fix is named.
func TestFlat_RequireTLSRefusesAPlaintextPeer(t *testing.T) {
	plain := newStandIn(t)
	plain.handle("/api/summary", jsonAnswer(leafSummary("plain")))
	agg := harness.Serve(t, harness.ServeOptions{Config: fleetYAML("hub", onePoll, 1500,
		[]peer{{"plain", plain.URL}}, "  tls:\n    require_tls: true\n")})

	v := awaitFleet(t, agg.Addr, "the plaintext peer to be judged", polled)
	n := node(t, v, "plain")
	if n.Reachable || !strings.Contains(n.Error, "fleet.tls.require_tls is set") {
		t.Errorf("plain under require_tls = %+v; want refused, naming the setting", n)
	}
	if got := plain.hitsOf("/api/summary"); got != 0 {
		t.Errorf("a plaintext peer was spoken to %d times under require_tls; want never", got)
	}
}

// TestFlat_BadPeerTLSStopsTheServiceAtStartup checks that a fleet TLS setting
// that cannot be used stops the service before it binds, naming the file,
// rather than showing up later as every peer being unreachable.
func TestFlat_BadPeerTLSStopsTheServiceAtStartup(t *testing.T) {
	notPEM := harness.WriteFile(t, "ca.pem", "not a certificate\n")
	ca := newAuthority(t, "fleet")
	pair := ca.issue(t, "aggregator", leafOptions{client: true})
	cases := map[string]struct {
		tls  string
		want string
	}{
		"missing CA":        {"    ca_file: /nonexistent/ca.pem\n", "read ca_file /nonexistent/ca.pem"},
		"CA not PEM":        {"    ca_file: " + notPEM + "\n", "contains no PEM certificate"},
		"key pair unusable": {"    cert_file: " + pair.certPath + "\n    key_file: " + notPEM + "\n", "load key pair " + pair.certPath},
		"cert without key":  {"    cert_file: " + pair.certPath + "\n", "cert_file and key_file must be set together"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := harness.WriteFile(t, "sysmon.yaml", fleetYAML("hub", onePoll, 1500,
				[]peer{{"secure", "https://127.0.0.1:1"}}, "  tls:\n"+tc.tls))
			r := harness.Run(t, harness.Bins.Sysmon, nil, "serve", "--addr", "127.0.0.1:0", "--config", cfg)
			if r.Code == 0 || !strings.Contains(r.Output(), tc.want) {
				t.Errorf("serve with %s exited %d:\n%s\nwant a failure naming %q", name, r.Code, r.Output(), tc.want)
			}
			if strings.Contains(r.Output(), "server listening") {
				t.Errorf("serve with %s bound its listener before failing", name)
			}
		})
	}
}

// TestFlat_DrillInsAndSettingsThroughTheAggregator checks what the aggregator
// serves for one node: its snapshot and its settings, fetched on demand,
// reused within the poll interval, and each failure mapped to its status.
func TestFlat_DrillInsAndSettingsThroughTheAggregator(t *testing.T) {
	m := startMisbehaving(t)
	m.secure.handle("/api/snapshot", jsonAnswer(types.Snapshot{NodeName: "secure", Host: types.HostInfo{Hostname: "secure.example"}}))
	m.secure.handle("/api/config", jsonAnswer(types.ConfigView{ConfigFile: "/etc/go-sysmon/sysmon.yaml", Entries: []types.ConfigEntry{
		{Section: "server", Key: "server.addr", Value: "10.1.2.3:8081", Source: types.ConfigSourceFile},
		{Section: "server", Key: "server.interval_ms", Value: "1000", Source: types.ConfigSourceDefault},
	}}))
	dead := deadpeer.URL(t)
	agg := harness.Serve(t, harness.ServeOptions{Config: fleetYAML("hub", onePoll, 1500,
		[]peer{{"secure", m.secure.URL}, {"old", m.old.URL}, {"dead", dead}}, tlsExtra(m.caPath, m.client))})
	awaitFleet(t, agg.Addr, "the one poll", polled)

	var ps types.PeerSnapshot
	for range 2 {
		ps = types.PeerSnapshot{}
		harness.GetJSON(t, "http://"+agg.Addr+"/api/fleet/secure/snapshot", &ps)
	}
	if !ps.Reachable || ps.Snapshot == nil || ps.Snapshot.NodeName != "secure" {
		t.Errorf("secure's drill-in = %+v; want its snapshot, reachable", ps)
	}
	if got := m.secure.hitsOf("/api/snapshot"); got != 1 {
		t.Errorf("two drill-ins within the poll interval fetched the snapshot %d times; want once", got)
	}

	var self types.PeerSnapshot
	harness.GetJSON(t, "http://"+agg.Addr+"/api/fleet/hub/snapshot", &self)
	if !self.Reachable || self.Snapshot == nil || self.Snapshot.NodeName != "hub" {
		t.Errorf("the aggregator's own drill-in = reachable %v, node %v; want its own snapshot", self.Reachable, self.Snapshot)
	}

	for path, status := range map[string]int{
		"/api/fleet/dead/snapshot":   http.StatusBadGateway,
		"/api/fleet/nobody/snapshot": http.StatusNotFound,
		"/api/fleet/nobody/config":   http.StatusNotFound,
		"/api/fleet/dead/config":     http.StatusBadGateway,
		"/api/fleet/old/config":      http.StatusBadGateway,
	} {
		if got, _, body := request(t, agg.Addr, path, nil); got != status {
			t.Errorf("GET %s = %d (%s); want %d", path, got, body, status)
		}
	}
	if _, _, body := request(t, agg.Addr, "/api/fleet/old/config", nil); !strings.Contains(string(body), "does not serve /api/config (HTTP 404)") {
		t.Errorf("an old peer's settings = %q; want it named as a version older than the endpoint", body)
	}

	var cfg types.ConfigView
	harness.GetJSON(t, "http://"+agg.Addr+"/api/fleet/secure/config", &cfg)
	if cfg.ConfigFile != "(configured)" {
		t.Errorf("relayed config_file = %q; want the path withheld", cfg.ConfigFile)
	}
	for _, e := range cfg.Entries {
		if e.Key == "server.addr" && e.Value != "(configured)" {
			t.Errorf("relayed server.addr = %q; want the peer's bind address withheld", e.Value)
		}
	}
	var own types.ConfigView
	harness.GetJSON(t, "http://"+agg.Addr+"/api/fleet/hub/config", &own)
	if len(own.Entries) == 0 {
		t.Error("the aggregator's own settings through the fleet route are empty")
	}
}

// TestFlat_QueriesPagesAndTheRollup checks what GET /api/fleet selects: search,
// pages and anchors in name order, each malformed parameter refused, and the
// roll-up whole whatever the page.
func TestFlat_QueriesPagesAndTheRollup(t *testing.T) {
	var peers []peer
	for _, name := range []string{"sql-01", "sql-02", "web-01", "web-02", "Cache"} {
		s := newStandIn(t)
		summary := leafSummary(name)
		if name == "web-02" {
			summary.Alerts = []types.Alert{alertOf(types.SeverityWarning, "load", "cpu")}
			summary.WarningCount = 1
		}
		s.handle("/api/summary", jsonAnswer(summary))
		peers = append(peers, peer{name, s.URL})
	}
	agg := harness.Serve(t, harness.ServeOptions{Config: fleetYAML("hub", onePoll, 1500, peers, "")})
	awaitFleet(t, agg.Addr, "the one poll", polled)

	whole := fleetOf(t, agg.Addr, "")
	if got := strings.Join(names(whole), ","); got != "Cache,hub,sql-01,sql-02,web-01,web-02" {
		t.Errorf("order = %s; want names in case-folded order", got)
	}
	if whole.Page == nil || whole.Page.Total != 6 || whole.Self == nil || whole.Self.Name != "hub" || whole.Self.Aggregates != 5 {
		t.Errorf("page %+v, self %+v; want 6 in total and hub aggregating 5", whole.Page, whole.Self)
	}

	// "hub" is this host, and a search also matches its hostname, which in a
	// container is a hex ID: the searches here use letters no hex ID holds,
	// so the result is the same whatever the host is called.
	for query, want := range map[string]string{
		"?q=SQL":                 "sql-01,sql-02",
		"?q=sql&offset=1":        "sql-02",
		"?limit=2&offset=2":      "sql-01,sql-02",
		"?limit=2&anchor=web-02": "web-01,web-02",
		"?q=.example&limit=1":    "Cache",
		"?q=nothing":             "",
		"?limit=2&anchor=nobody": "Cache,hub",
		"?q=%20web-01%20":        "web-01",
	} {
		v := fleetOf(t, agg.Addr, query)
		if got := strings.Join(names(v), ","); got != want {
			t.Errorf("GET /api/fleet%s = %s; want %s", query, got, want)
		}
		// hub is this host, whose own alerts are the host's business; the
		// roll-up is compared with the whole fleet's rather than with a count.
		if v.Rollup == nil || v.Rollup.Nodes != 6 || v.Rollup.Warning != whole.Rollup.Warning || v.Rollup.Healthy != whole.Rollup.Healthy {
			t.Errorf("GET /api/fleet%s roll-up = %+v; want the whole fleet's, whatever the page", query, v.Rollup)
		}
	}
	if p := fleetOf(t, agg.Addr, "?q=sql&limit=1").Page; p.Matched != 2 || p.Limit != 1 || p.Query != "sql" {
		t.Errorf("page = %+v; want 2 matched, limit 1, the query echoed", p)
	}

	for _, query := range []string{"?limit=0", "?limit=x", "?offset=-1", "?q=a&q=b", "?q=%ff", "?anchor=%ff", "?limit=%zz", "?limit=25;offset=0"} {
		if status, _, body := request(t, agg.Addr, "/api/fleet"+query, nil); status != http.StatusBadRequest {
			t.Errorf("GET /api/fleet%s = %d (%s); want 400", query, status, body)
		}
	}

	var r types.FleetRollup
	harness.GetJSON(t, "http://"+agg.Addr+"/api/fleet/rollup", &r)
	named := false
	for _, a := range r.Attention {
		named = named || (a.Name == "web-02" && a.Status == types.NodeStatusWarning)
	}
	if r.Nodes != 6 || r.Warning != whole.Rollup.Warning || !named {
		t.Errorf("GET /api/fleet/rollup = %+v; want 6 nodes, web-02 needing attention", r)
	}

	var sub types.FleetSubtree
	harness.GetJSON(t, "http://"+agg.Addr+"/api/fleet/subtree", &sub)
	if len(sub.Nodes) != 6 || sub.Nodes[1].Name != "hub" || sub.Nodes[1].Depth != 0 || len(sub.Aggregators) != 1 {
		t.Errorf("GET /api/fleet/subtree = %+v; want six entries, hub at depth 0, one aggregator", sub)
	}

	var got types.FleetNodes
	harness.GetJSON(t, "http://"+agg.Addr+"/api/fleet/nodes?name=sql-02&name=web-02&name=nobody", &got)
	if len(got.Nodes) != 2 || got.Nodes[0].Name != "sql-02" || got.Nodes[1].Status != types.NodeStatusWarning {
		t.Errorf("GET /api/fleet/nodes = %+v; want sql-02 and web-02, warning", got.Nodes)
	}
	many := strings.Repeat("name=a&", 1001)
	for _, query := range []string{"", "?name=", "?name=bad%20name", "?" + many, "?name=%zz"} {
		if status, _, body := request(t, agg.Addr, "/api/fleet/nodes"+query, nil); status != http.StatusBadRequest {
			t.Errorf("GET /api/fleet/nodes%s = %d (%.80s); want 400", query[:min(len(query), 40)], status, body)
		}
	}
}

// TestFlat_AncestryIsChecked checks what an aggregator makes of the chain a
// request says it has passed through: a malformed one is refused, one that has
// already been here is a loop, and one past the depth limit is too deep.
func TestFlat_AncestryIsChecked(t *testing.T) {
	s := newStandIn(t)
	s.handle("/api/summary", jsonAnswer(leafSummary("leaf")))
	agg := harness.Serve(t, harness.ServeOptions{Config: fleetYAML("hub", onePoll, 1500, []peer{{"leaf", s.URL}}, "")})
	awaitFleet(t, agg.Addr, "the one poll", polled)

	var self types.NodeSummary
	harness.GetJSON(t, "http://"+agg.Addr+"/api/summary", &self)
	if len(self.InstanceID) != 26 || self.Aggregates != 1 {
		t.Fatalf("the aggregator's summary = instance %q, aggregates %d; want its ID and 1", self.InstanceID, self.Aggregates)
	}
	other := strings.Repeat("A", 26)
	for name, tc := range map[string]struct {
		chain   string
		status  int
		refusal string
	}{
		"malformed":       {"not-an-id", http.StatusBadRequest, ""},
		"a loop":          {other + "," + self.InstanceID, http.StatusLoopDetected, "loop"},
		"too deep":        {strings.TrimSuffix(strings.Repeat(other+",", 6), ","), http.StatusLoopDetected, "depth"},
		"at the limit":    {strings.TrimSuffix(strings.Repeat(other+",", 5), ","), http.StatusLoopDetected, "depth"},
		"from its parent": {other, http.StatusOK, ""},
	} {
		for _, path := range []string{"/api/fleet", "/api/fleet/leaf/snapshot", "/api/fleet/nodes?name=leaf", "/api/fleet/subtree", "/api/fleet/leaf/config"} {
			if name == "from its parent" && strings.HasSuffix(path, "/snapshot") {
				continue // the stand-in has no snapshot to give
			}
			if name == "from its parent" && strings.HasSuffix(path, "/config") {
				continue // nor settings
			}
			status, header, body := request(t, agg.Addr, path, map[string]string{
				"X-Sysmon-Fleet-Ancestry": tc.chain, "X-Sysmon-Fleet-Budget-Ms": "900",
			})
			if status != tc.status || header.Get("X-Sysmon-Fleet-Refusal") != tc.refusal {
				t.Errorf("%s on %s = %d, refusal %q (%.100s); want %d, %q", name, path, status,
					header.Get("X-Sysmon-Fleet-Refusal"), body, tc.status, tc.refusal)
			}
		}
	}
}

// TestFlat_TheFleetCadenceIsSetAndFloored checks POST /api/interval with the
// fleet scope: it drives the peer poller, is floored at a second, and is
// reported as what runs, not what was asked.
func TestFlat_TheFleetCadenceIsSetAndFloored(t *testing.T) {
	s := newStandIn(t)
	s.handle("/api/summary", jsonAnswer(leafSummary("leaf")))
	agg := harness.Serve(t, harness.ServeOptions{Config: fleetYAML("hub", 5000, 1500, []peer{{"leaf", s.URL}}, "")})
	awaitFleet(t, agg.Addr, "the first poll", polled)

	status, body := harness.Post(t, "http://"+agg.Addr+"/api/interval", `{"interval_ms":500,"scope":"fleet"}`)
	var got struct {
		IntervalMS      int `json:"interval_ms"`
		FleetIntervalMS int `json:"fleet_interval_ms"`
	}
	harness.MustParseJSON(t, string(body), &got)
	if status != http.StatusOK || got.FleetIntervalMS < 1000 || got.FleetIntervalMS >= 5000 {
		t.Errorf("fleet interval 500 = %d %+v; want at least the one-second floor and no longer the configured 5s", status, got)
	}
	if status, _ := harness.Post(t, "http://"+agg.Addr+"/api/interval", `{"interval_ms":1000,"scope":"elsewhere"}`); status != http.StatusBadRequest {
		t.Errorf("an unknown scope = %d; want 400", status)
	}
}

// TestFlat_AStaleDrillInIsMarkedStale checks that a node that stops answering
// keeps its last reading on the card and in its drill-in, marked as not
// current, with the time since it was read -- never presented as live.
func TestFlat_AStaleDrillInIsMarkedStale(t *testing.T) {
	leaf := harness.Serve(t, harness.ServeOptions{Config: "fleet:\n  local_name: leaf\n"})
	agg := harness.Serve(t, harness.ServeOptions{Config: fleetYAML("hub", 1000, 1500, []peer{{"leaf", "http://" + leaf.Addr}}, "")})
	awaitFleet(t, agg.Addr, "the leaf to be read", func(v types.Fleet) bool { return polled(v) && node(t, v, "leaf").Reachable })
	var live types.PeerSnapshot
	harness.GetJSON(t, "http://"+agg.Addr+"/api/fleet/leaf/snapshot", &live)
	if !live.Reachable {
		t.Fatalf("leaf's first drill-in = %+v; want reachable", live)
	}

	if err := leaf.Stop(t); err != nil {
		t.Fatalf("stop the leaf: %v", err)
	}
	v := awaitFleet(t, agg.Addr, "the leaf's absence to be noticed", func(v types.Fleet) bool { return !node(t, v, "leaf").Reachable })
	n := node(t, v, "leaf")
	if n.Status != types.NodeStatusUnreachable || n.Hostname == "" || !strings.Contains(n.Error, "connection refused") {
		t.Errorf("the stopped leaf = %+v; want unreachable, its last summary kept, the refusal named", n)
	}
	var stale types.PeerSnapshot
	harness.GetJSON(t, "http://"+agg.Addr+"/api/fleet/leaf/snapshot", &stale)
	if stale.Reachable || stale.Snapshot == nil || stale.Error == "" || stale.AgeSeconds <= 0 {
		t.Errorf("the stopped leaf's drill-in = reachable %v, error %q, age %v; want its last snapshot, marked stale", stale.Reachable, stale.Error, stale.AgeSeconds)
	}
	if v.Rollup.NeverAnswered != 0 || v.Rollup.LongestAbsenceSeconds <= 0 {
		t.Errorf("roll-up = %+v; want an absence, not a node that never answered", v.Rollup)
	}
}
