package collector

import (
	"path/filepath"
	"testing"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
	psnet "github.com/shirou/gopsutil/v4/net"
)

// testdata/proc/net/route holds its addresses as the kernel prints them on a
// little-endian host (amd64, arm64), which is what CI runs.
const routeFixtureRoot = "testdata/proc"

// TestReadRouteTables_Testdata reads a table with competing default routes,
// non-default gatewayed routes, a down route, a reject route and malformed
// lines; the malformed ones would win on metric if they were parsed.
func TestReadRouteTables_Testdata(t *testing.T) {
	withProcRoot(t, routeFixtureRoot)
	tables := readRouteTables(nil)

	if !tables.available {
		t.Fatal("available = false, want true")
	}
	if tables.v4.defaultIface != "eth0" {
		t.Errorf("IPv4 default interface = %q, want eth0 (metric 100 beats wlan0's 600; eth2's is down)", tables.v4.defaultIface)
	}
	if tables.v6.defaultIface != "eth0" {
		t.Errorf("IPv6 default interface = %q, want eth0 (metric 0x64 beats wlan0's 0x400)", tables.v6.defaultIface)
	}

	wantV4 := map[string]string{
		"wlan0": "192.168.1.1",
		"eth0":  "10.0.0.1",
		"eth1":  "192.168.1.254", // the metric-0 route, not the metric-5 one
	}
	assertGateways(t, "IPv4", tables.v4, wantV4)
	assertGateways(t, "IPv6", tables.v6, map[string]string{"wlan0": "fe80::1", "eth0": "fe80::2"})
}

func assertGateways(t *testing.T, family string, got familyRoutes, want map[string]string) {
	t.Helper()
	if len(got.gateways) != len(want) {
		t.Errorf("%s gateways = %v, want %v", family, got.gateways, want)
	}
	for iface, gw := range want {
		if g := got.gateways[iface]; g.String() != gw {
			t.Errorf("%s gateway of %s = %v, want %s", family, iface, g, gw)
		}
	}
}

func TestReadRouteTables_Absent(t *testing.T) {
	withProcRoot(t, t.TempDir())
	tables := readRouteTables(nil)

	if tables.available {
		t.Error("available = true, want false without /proc/net/route")
	}
	if len(tables.v4.gateways) != 0 || tables.v4.defaultIface != "" || tables.v6.defaultIface != "" {
		t.Errorf("tables = %+v, want no routes", tables)
	}
}

// With IPv6 disabled the kernel has no ipv6_route; the IPv4 table still
// counts as available.
func TestReadRouteTables_NoIPv6Table(t *testing.T) {
	root := t.TempDir()
	writeSysfsFile(t, filepath.Join(root, "net", "route"),
		"Iface\tDestination\tGateway\tFlags\tRefCnt\tUse\tMetric\tMask\tMTU\tWindow\tIRTT\n"+
			"eth0\t00000000\t0100000A\t0003\t0\t0\t100\t00000000\t0\t0\t0\n")
	withProcRoot(t, root)

	tables := readRouteTables(nil)
	if !tables.available || tables.v4.defaultIface != "eth0" {
		t.Errorf("v4 = %+v, available = %v; want eth0 as the default", tables.v4, tables.available)
	}
	if tables.v6.defaultIface != "" || len(tables.v6.gateways) != 0 {
		t.Errorf("v6 = %+v, want empty", tables.v6)
	}
}

func TestParseIPv4Routes_Malformed(t *testing.T) {
	tests := map[string]string{
		"empty":           "",
		"header only":     "Iface\tDestination\tGateway\n",
		"bad flags":       "h\neth0\t00000000\t0100000A\tzz\t0\t0\t1\t00000000\n",
		"bad metric":      "h\neth0\t00000000\t0100000A\t0003\t0\t0\t-1\t00000000\n",
		"bad mask":        "h\neth0\t00000000\t0100000A\t0003\t0\t0\t1\t1FFFFFFFF\n",
		"too few fields":  "h\neth0\t00000000\t0100000A\t0003\n",
		"bad destination": "h\neth0\tnothex\t0100000A\t0003\t0\t0\t1\t00000000\n",
		"reject route":    "h\neth0\t00000000\t0100000A\t0203\t0\t0\t1\t00000000\n",
	}
	for name, in := range tests {
		t.Run(name, func(t *testing.T) {
			if got := parseIPv4Routes([]byte(in)); len(got) != 0 {
				t.Errorf("parseIPv4Routes(%q) = %+v, want none", in, got)
			}
		})
	}
}

func TestParseIPv6Routes_Malformed(t *testing.T) {
	const zero = "00000000000000000000000000000000"
	tests := map[string]string{
		"empty":              "",
		"too few fields":     zero + " 00 " + zero + "\n",
		"short address":      "00 00 " + zero + " 00 " + zero + " 00000001 0 0 00000003 eth0\n",
		"bad prefix":         zero + " zz " + zero + " 00 " + zero + " 00000001 0 0 00000003 eth0\n",
		"bad metric":         zero + " 00 " + zero + " 00 " + zero + " -1 0 0 00000003 eth0\n",
		"bad flags":          zero + " 00 " + zero + " 00 " + zero + " 00000001 0 0 zz eth0\n",
		"route that is down": zero + " 00 " + zero + " 00 " + zero + " 00000001 0 0 00000002 eth0\n",
	}
	for name, in := range tests {
		t.Run(name, func(t *testing.T) {
			if got := parseIPv6Routes([]byte(in)); len(got) != 0 {
				t.Errorf("parseIPv6Routes(%q) = %+v, want none", in, got)
			}
		})
	}
}

// A default route with no next hop (a point-to-point link) still makes its
// interface the default, but gives it no gateway.
func TestSummarizeRoutes_OnLinkDefault(t *testing.T) {
	routes := parseIPv4Routes([]byte("h\nppp0\t00000000\t00000000\t0001\t0\t0\t0\t00000000\n"))
	summary := summarizeRoutes(routes)

	if summary.defaultIface != "ppp0" {
		t.Errorf("defaultIface = %q, want ppp0", summary.defaultIface)
	}
	if _, ok := summary.gateways["ppp0"]; ok {
		t.Errorf("gateways = %v, want none for an on-link default", summary.gateways)
	}
}

func TestApplyRoutes(t *testing.T) {
	withProcRoot(t, routeFixtureRoot)
	tables := readRouteTables(nil)

	eth0 := types.NetworkInfo{Name: "eth0"}
	applyRoutes(&eth0, tables)
	if eth0.GatewayIPv4 != "10.0.0.1" || eth0.GatewayIPv6 != "fe80::2" {
		t.Errorf("eth0 gateways = %q, %q; want 10.0.0.1, fe80::2", eth0.GatewayIPv4, eth0.GatewayIPv6)
	}
	if !eth0.DefaultRouteIPv4 || !eth0.DefaultRouteIPv6 || !eth0.RoutesAvailable {
		t.Errorf("eth0 = %+v, want both default flags and RoutesAvailable", eth0)
	}

	wlan0 := types.NetworkInfo{Name: "wlan0"}
	applyRoutes(&wlan0, tables)
	if wlan0.GatewayIPv4 != "192.168.1.1" || wlan0.DefaultRouteIPv4 || wlan0.DefaultRouteIPv6 {
		t.Errorf("wlan0 = %+v, want its own gateway but not the default", wlan0)
	}

	docker0 := types.NetworkInfo{Name: "docker0"}
	applyRoutes(&docker0, tables)
	if docker0.GatewayIPv4 != "" || docker0.GatewayIPv6 != "" {
		t.Errorf("docker0 gateways = %q, %q; want none for on-link routes", docker0.GatewayIPv4, docker0.GatewayIPv6)
	}
}

// An unnamed interface must not match an empty default interface.
func TestApplyRoutes_UnavailableAndUnnamed(t *testing.T) {
	info := types.NetworkInfo{}
	applyRoutes(&info, routeTables{})

	if info.RoutesAvailable || info.DefaultRouteIPv4 || info.DefaultRouteIPv6 || info.GatewayIPv4 != "" {
		t.Errorf("info = %+v, want no routing state", info)
	}
}

func TestParseAddress(t *testing.T) {
	tests := map[string]types.AddressInfo{
		"192.168.1.100/24": {Address: "192.168.1.100", PrefixLength: 24, Netmask: "255.255.255.0", Family: "ipv4"},
		"10.1.2.3/8":       {Address: "10.1.2.3", PrefixLength: 8, Netmask: "255.0.0.0", Family: "ipv4"},
		"100.64.0.1/32":    {Address: "100.64.0.1", PrefixLength: 32, Netmask: "255.255.255.255", Family: "ipv4"},
		"fe80::1/64":       {Address: "fe80::1", PrefixLength: 64, Family: "ipv6"},
		"2001:db8::5/128":  {Address: "2001:db8::5", PrefixLength: 128, Family: "ipv6"},
	}
	for in, want := range tests {
		got, ok := parseAddress(in)
		if !ok || got != want {
			t.Errorf("parseAddress(%q) = %+v, %v; want %+v", in, got, ok, want)
		}
	}
}

func TestParseAddress_Invalid(t *testing.T) {
	for _, in := range []string{"", "garbage", "10.0.0.1", "10.0.0.1/33", "fe80::1%eth0/64"} {
		if got, ok := parseAddress(in); ok {
			t.Errorf("parseAddress(%q) = %+v, want rejected", in, got)
		}
	}
}

// buildNetworkInfo keeps every address string but adds only the ones that
// parse to AddressDetails.
func TestBuildNetworkInfo_AddressDetails(t *testing.T) {
	iface := psnet.InterfaceStat{
		Name:  "sysmon-test0", // absent from sysfs, so nothing is read from it
		Addrs: psnet.InterfaceAddrList{{Addr: "192.168.1.5/24"}, {Addr: "bogus"}, {Addr: "fe80::1/64"}},
	}
	info := buildNetworkInfo(iface, nil, nil, 0)

	if len(info.Addresses) != 3 {
		t.Errorf("Addresses = %v, want all three strings kept", info.Addresses)
	}
	if len(info.AddressDetails) != 2 ||
		info.AddressDetails[0].Netmask != "255.255.255.0" ||
		info.AddressDetails[1].Family != "ipv6" {
		t.Errorf("AddressDetails = %+v, want the IPv4 and IPv6 entries in order", info.AddressDetails)
	}
}
