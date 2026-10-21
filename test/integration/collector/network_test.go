//go:build integration

package collector

import (
	"strconv"
	"strings"
	"testing"

	"github.com/jeremyhahn/go-sysmon/test/integration/harness"
)

// netInfo is one interface of `sysmon network --json`.
type netInfo struct {
	Name         string   `json:"name"`
	Kind         string   `json:"kind"`
	HardwareAddr string   `json:"hardware_addr"`
	Addresses    []string `json:"addresses"`
	MTU          int      `json:"mtu"`
	IsUp         bool     `json:"is_up"`
	IsVirtual    bool     `json:"is_virtual"`
	OperState    string   `json:"oper_state"`
	DNSServers   []string `json:"dns_servers"`
	DNSSearch    []string `json:"dns_search"`
	BridgePorts  []string `json:"bridge_ports"`
	BondMode     string   `json:"bond_mode"`
	BondSlaves   []string `json:"bond_slaves"`
	BondMaster   string   `json:"bond_master"`
	Bus          string   `json:"bus"`
	VendorID     string   `json:"vendor_id"`
	Vendor       string   `json:"vendor"`
	Model        string   `json:"model"`
}

// topology builds, in the process's own network namespace, a host's worth of
// software networking: a bridge with two veth ports, an active-backup bond of
// two links, a VLAN, a tap and a plain dummy link. br0 has addresses and its
// own resolver settings from systemd-resolved; every other link resolves
// through the system's.
var topology = []string{
	"mount -t tmpfs tmpfs /run",
	"ip link add br0 type bridge",
	"ip link add veth-a type veth peer name veth-b",
	"ip link add veth-c type veth peer name veth-d",
	"ip link set veth-a master br0",
	"ip link set veth-c master br0",
	"ip link add bond0 type bond mode active-backup",
	"ip link add uplink1 type dummy",
	"ip link add uplink2 type dummy",
	"ip link set uplink1 master bond0",
	"ip link set uplink2 master bond0",
	"ip link add trunk type dummy",
	"ip link add link trunk name trunk.100 type vlan id 100",
	"ip tuntap add tap0 mode tap",
	"ip link add dum0 type dummy",
	"ip link set dum0 mtu 9000",
	"for l in br0 veth-a veth-b veth-c veth-d bond0 trunk trunk.100 dum0; do ip link set $l up; done",
	"ip addr add 192.0.2.10/24 dev br0",
	"ip -6 addr add 2001:db8::10/64 dev br0 nodad",
	"mkdir -p /run/systemd/resolve/netif",
	`printf 'nameserver 127.0.0.53\nnameserver 192.0.2.1\nsearch corp.example .\n' >/run/systemd/resolve/resolv.conf`,
	`printf '# This is private data. Do not parse.\nDNS=192.0.2.53 192.0.2.54\nDOMAINS=lab.example\n' >"/run/systemd/resolve/netif/$(cat /sys/class/net/br0/ifindex)"`,
}

// networks runs `sysmon network --json` in a namespace built by topology.
func networks(t *testing.T) map[string]netInfo {
	t.Helper()
	requireDevcontainer(t)
	bin := sandbox{net: true, setup: topology}.wrap(t, harness.Bins.Sysmon)
	r := harness.Run(t, bin, nil, "network", "--json")
	if r.Code != 0 {
		t.Fatalf("sysmon network --json exited %d:\n%s", r.Code, r.Output())
	}
	var list []netInfo
	harness.MustParseJSON(t, r.Stdout, &list)
	byName := make(map[string]netInfo, len(list))
	for _, n := range list {
		byName[n.Name] = n
	}
	return byName
}

// TestNetwork_SoftwareTopologyIsClassified verifies every kind of software
// link is told apart and its topology read: which ports a bridge has, a bond's
// mode and members and each member's master, and a VLAN, a tap and a veth for
// what they are.
func TestNetwork_SoftwareTopologyIsClassified(t *testing.T) {
	nets := networks(t)

	kinds := map[string]string{
		"lo": "loopback", "br0": "bridge", "bond0": "bond", "trunk.100": "vlan",
		"tap0": "tun", "veth-a": "veth", "dum0": "virtual",
	}
	for name, want := range kinds {
		if got := nets[name].Kind; got != want {
			t.Errorf("%s kind = %q, want %q", name, got, want)
		}
	}
	if got := strings.Join(nets["br0"].BridgePorts, ","); got != "veth-a,veth-c" {
		t.Errorf("br0 ports = %s, want veth-a,veth-c", got)
	}
	bond := nets["bond0"]
	if bond.BondMode != "active-backup" || strings.Join(bond.BondSlaves, ",") != "uplink1,uplink2" {
		t.Errorf("bond0 = %q over %q, want active-backup over uplink1,uplink2", bond.BondMode, bond.BondSlaves)
	}
	if nets["uplink1"].BondMaster != "bond0" || nets["veth-a"].BondMaster != "br0" {
		t.Errorf("masters = uplink1 %q, veth-a %q; want bond0 and br0", nets["uplink1"].BondMaster, nets["veth-a"].BondMaster)
	}
	if nets["dum0"].MTU != 9000 || !nets["dum0"].IsUp || !nets["dum0"].IsVirtual {
		t.Errorf("dum0 = mtu %d, up %v, virtual %v; want 9000, up, virtual", nets["dum0"].MTU, nets["dum0"].IsUp, nets["dum0"].IsVirtual)
	}
}

// TestNetwork_SoftwareLinksHaveNoHardwareIdentity verifies that a link with no
// device behind it reports no bus, identifiers, manufacturer or model, rather
// than borrowing those of some other device.
func TestNetwork_SoftwareLinksHaveNoHardwareIdentity(t *testing.T) {
	nets := networks(t)

	for _, name := range []string{"lo", "br0", "bond0", "trunk.100", "tap0", "veth-a", "dum0"} {
		n := nets[name]
		if n.Bus != "" || n.VendorID != "" || n.Vendor != "" || n.Model != "" {
			t.Errorf("%s = bus %q, id %q, %q %q; want no hardware identity", name, n.Bus, n.VendorID, n.Vendor, n.Model)
		}
	}
}

// TestNetwork_AddressesAndResolvers verifies a link's addresses, and that a
// link with systemd-resolved state of its own reports its own resolvers while
// every other link reports the system's, without the stub and root domain.
func TestNetwork_AddressesAndResolvers(t *testing.T) {
	nets := networks(t)

	br := nets["br0"]
	addrs := strings.Join(br.Addresses, ",")
	if !strings.Contains(addrs, "192.0.2.10/24") || !strings.Contains(addrs, "2001:db8::10/64") {
		t.Errorf("br0 addresses = %s, want 192.0.2.10/24 and 2001:db8::10/64", addrs)
	}
	if strings.Join(br.DNSServers, ",") != "192.0.2.53,192.0.2.54" || strings.Join(br.DNSSearch, ",") != "lab.example" {
		t.Errorf("br0 resolvers = %q search %q, want its own 192.0.2.53,192.0.2.54 search lab.example", br.DNSServers, br.DNSSearch)
	}
	dum := nets["dum0"]
	if strings.Join(dum.DNSServers, ",") != "127.0.0.53,192.0.2.1" || strings.Join(dum.DNSSearch, ",") != "corp.example" {
		t.Errorf("dum0 resolvers = %q search %q, want the system's 127.0.0.53,192.0.2.1 search corp.example", dum.DNSServers, dum.DNSSearch)
	}
}

// TestNetwork_TextAndIndex verifies the text report shows the topology and
// that --index selects one link and refuses one that does not exist.
func TestNetwork_TextAndIndex(t *testing.T) {
	requireDevcontainer(t)
	bin := sandbox{net: true, setup: topology}.wrap(t, harness.Bins.Sysmon)

	text := harness.Run(t, bin, nil, "network")
	if text.Code != 0 {
		t.Fatalf("sysmon network exited %d:\n%s", text.Code, text.Output())
	}
	for _, want := range []string{"br0", "bond0", "active-backup", "trunk.100", "192.0.2.10"} {
		if !strings.Contains(text.Stdout, want) {
			t.Errorf("sysmon network does not show %q:\n%s", want, text.Stdout)
		}
	}

	var all []netInfo
	harness.MustParseJSON(t, harness.Run(t, bin, nil, "network", "--json").Stdout, &all)
	one := harness.Run(t, bin, nil, "network", "--json", "--index", "0")
	var picked []netInfo
	harness.MustParseJSON(t, one.Stdout, &picked)
	if one.Code != 0 || len(picked) != 1 || picked[0].Name != all[0].Name {
		t.Errorf("sysmon network --index 0 = %+v (exit %d), want only %s", picked, one.Code, all[0].Name)
	}
	missing := harness.Run(t, bin, nil, "network", "--index", strconv.Itoa(len(all)+3))
	if missing.Code == 0 || !strings.Contains(missing.Output(), "not found") {
		t.Errorf("sysmon network --index past the end exited %d:\n%s", missing.Code, missing.Output())
	}
}
