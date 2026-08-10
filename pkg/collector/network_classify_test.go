package collector

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// withSysClassNet points the classifier at a synthetic /sys/class/net.
func withSysClassNet(t *testing.T, root string) {
	t.Helper()
	original := sysClassNet
	sysClassNet = root
	t.Cleanup(func() { sysClassNet = original })
}

// mkIface creates an interface directory with the given attribute files.
func mkIface(t *testing.T, root, name string, files map[string]string, dirs ...string) string {
	t.Helper()
	base := filepath.Join(root, name)
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", base, err)
	}
	for f, content := range files {
		writeSysfsFile(t, filepath.Join(base, f), content)
	}
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(base, d), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	return base
}

// ---- classification -------------------------------------------------------

func TestClassifyInterface_Kinds(t *testing.T) {
	root := t.TempDir()
	withSysClassNet(t, root)

	mkIface(t, root, "lo", nil)
	mkIface(t, root, "br0", nil, "bridge", "brif")
	mkIface(t, root, "bond0", nil, "bonding")
	mkIface(t, root, "wlan0", nil, "wireless")
	mkIface(t, root, "wlan1", map[string]string{"phy80211": "phy0\n"})
	mkIface(t, root, "tun0", map[string]string{"tun_flags": "0x1002\n"})
	mkIface(t, root, "veth123", nil)
	mkIface(t, root, "eth0", nil, "device")
	mkIface(t, root, "weird0", nil)

	tests := map[string]string{
		"lo":      kindLoopback,
		"br0":     kindBridge,
		"bond0":   kindBond,
		"wlan0":   kindWifi,
		"wlan1":   kindWifi,
		"tun0":    kindTun,
		"veth123": kindVeth,
		"eth0":    kindEthernet,
		"weird0":  kindVirtual,
	}
	for name, want := range tests {
		if got := classifyInterface(name); got != want {
			t.Errorf("classifyInterface(%q) = %q, want %q", name, got, want)
		}
	}
}

// TestClassifyInterface_BridgeBeatsDevice verifies precedence: a bridge that
// also has a device directory must still classify as a bridge.
func TestClassifyInterface_BridgeBeatsDevice(t *testing.T) {
	root := t.TempDir()
	withSysClassNet(t, root)
	mkIface(t, root, "br0", nil, "bridge", "device")

	if got := classifyInterface("br0"); got != kindBridge {
		t.Errorf("classifyInterface(br0) = %q, want %q", got, kindBridge)
	}
}

func TestClassifyInterface_MissingInterface(t *testing.T) {
	withSysClassNet(t, t.TempDir())
	if got := classifyInterface("does-not-exist"); got != kindVirtual {
		t.Errorf("classifyInterface(missing) = %q, want %q", got, kindVirtual)
	}
}

// ---- bridge and bond topology ---------------------------------------------

func TestReadBridgePorts_SortedAndEmpty(t *testing.T) {
	root := t.TempDir()
	withSysClassNet(t, root)

	base := mkIface(t, root, "br0", nil, "brif")
	for _, p := range []string{"vethC", "vethA", "vethB"} {
		if err := os.MkdirAll(filepath.Join(base, "brif", p), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}

	got := readBridgePorts("br0")
	want := []string{"vethA", "vethB", "vethC"}
	if len(got) != len(want) {
		t.Fatalf("readBridgePorts() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("port[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	mkIface(t, root, "eth0", nil)
	if ports := readBridgePorts("eth0"); ports != nil {
		t.Errorf("readBridgePorts(non-bridge) = %v, want nil", ports)
	}
}

func TestReadBondInfo(t *testing.T) {
	root := t.TempDir()
	withSysClassNet(t, root)

	base := mkIface(t, root, "bond0", nil, "bonding")
	writeSysfsFile(t, filepath.Join(base, "bonding", "mode"), "802.3ad 4\n")
	writeSysfsFile(t, filepath.Join(base, "bonding", "slaves"), "eth1 eth0\n")

	mode, slaves := readBondInfo("bond0")
	if mode != "802.3ad" {
		t.Errorf("mode = %q, want 802.3ad", mode)
	}
	if len(slaves) != 2 || slaves[0] != "eth0" || slaves[1] != "eth1" {
		t.Errorf("slaves = %v, want [eth0 eth1]", slaves)
	}
}

func TestReadBondInfo_NotABond(t *testing.T) {
	withSysClassNet(t, t.TempDir())
	mode, slaves := readBondInfo("eth0")
	if mode != "" || slaves != nil {
		t.Errorf("readBondInfo(non-bond) = (%q, %v), want empty mode and no slaves", mode, slaves)
	}
}

func TestReadMaster(t *testing.T) {
	root := t.TempDir()
	withSysClassNet(t, root)

	mkIface(t, root, "docker0", nil)
	base := mkIface(t, root, "veth123", nil)
	if err := os.Symlink(filepath.Join(root, "docker0"), filepath.Join(base, "master")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if got := readMaster("veth123"); got != "docker0" {
		t.Errorf("readMaster(veth123) = %q, want docker0", got)
	}
	if got := readMaster("docker0"); got != "" {
		t.Errorf("readMaster(unenslaved) = %q, want empty", got)
	}
}

func TestReadOperState(t *testing.T) {
	root := t.TempDir()
	withSysClassNet(t, root)
	mkIface(t, root, "eth0", map[string]string{"operstate": "down\n"})

	if got := readOperState("eth0"); got != "down" {
		t.Errorf("readOperState() = %q, want down", got)
	}
	if got := readOperState("absent"); got != "" {
		t.Errorf("readOperState(missing) = %q, want empty", got)
	}
}

// ---- wireless -------------------------------------------------------------

func TestReadWirelessInfo(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wireless")
	writeSysfsFile(t, path,
		"Inter-| sta-|   Quality        |   Discarded packets\n"+
			" face | tus | link level noise |  nwid  crypt   frag\n"+
			" wlan0: 0000   70.  -40.  -256        0      0      0\n")

	original := procNetWireless
	procNetWireless = path
	t.Cleanup(func() { procNetWireless = original })

	got := readWirelessInfo("wlan0")
	if got == nil {
		t.Fatal("readWirelessInfo(wlan0) = nil, want stats")
	}
	if got.LinkQuality != 70 {
		t.Errorf("LinkQuality = %f, want 70", got.LinkQuality)
	}
	if got.SignalLevelDBm != -40 {
		t.Errorf("SignalLevelDBm = %f, want -40", got.SignalLevelDBm)
	}
	if got.NoiseLevelDBm != -256 {
		t.Errorf("NoiseLevelDBm = %f, want -256", got.NoiseLevelDBm)
	}

	if other := readWirelessInfo("wlan9"); other != nil {
		t.Errorf("readWirelessInfo(absent) = %+v, want nil", other)
	}
}

func TestReadWirelessInfo_MissingFile(t *testing.T) {
	original := procNetWireless
	procNetWireless = filepath.Join(t.TempDir(), "nope")
	t.Cleanup(func() { procNetWireless = original })

	if got := readWirelessInfo("wlan0"); got != nil {
		t.Errorf("readWirelessInfo() = %+v, want nil when the file is absent", got)
	}
}

func TestParseWirelessValue(t *testing.T) {
	t.Parallel()
	tests := map[string]float64{"70.": 70, "-40.": -40, " -256 ": -256, "abc": 0, "": 0}
	for in, want := range tests {
		if got := parseWirelessValue(in); got != want {
			t.Errorf("parseWirelessValue(%q) = %f, want %f", in, got, want)
		}
	}
}

// ---- resolver -------------------------------------------------------------

func TestReadSystemDNS_PrefersSystemdResolved(t *testing.T) {
	dir := t.TempDir()
	systemd := filepath.Join(dir, "systemd-resolv.conf")
	etc := filepath.Join(dir, "etc-resolv.conf")
	writeSysfsFile(t, systemd, "nameserver 192.168.1.1\nsearch example.com\n")
	writeSysfsFile(t, etc, "nameserver 127.0.0.53\nsearch .\n")

	original := resolvConfPaths
	resolvConfPaths = []string{systemd, etc}
	t.Cleanup(func() { resolvConfPaths = original })

	servers, search := readSystemDNS()
	if len(servers) != 1 || servers[0] != "192.168.1.1" {
		t.Errorf("servers = %v, want the systemd upstream not the stub", servers)
	}
	if len(search) != 1 || search[0] != "example.com" {
		t.Errorf("search = %v, want [example.com]", search)
	}
}

// TestReadSystemDNS_SkipsRootSearchDomain verifies the bare "." systemd writes
// is not reported as a search suffix.
func TestReadSystemDNS_SkipsRootSearchDomain(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "resolv.conf")
	writeSysfsFile(t, path, "# a comment\nnameserver 10.0.0.1\nsearch .\n")

	original := resolvConfPaths
	resolvConfPaths = []string{path}
	t.Cleanup(func() { resolvConfPaths = original })

	servers, search := readSystemDNS()
	if len(servers) != 1 || servers[0] != "10.0.0.1" {
		t.Errorf("servers = %v, want [10.0.0.1]", servers)
	}
	if len(search) != 0 {
		t.Errorf("search = %v, want none for a bare root domain", search)
	}
}

func TestReadSystemDNS_NoFiles(t *testing.T) {
	original := resolvConfPaths
	resolvConfPaths = []string{filepath.Join(t.TempDir(), "absent")}
	t.Cleanup(func() { resolvConfPaths = original })

	servers, search := readSystemDNS()
	if servers != nil || search != nil {
		t.Errorf("readSystemDNS() = (%v, %v), want (nil, nil)", servers, search)
	}
}

func TestReadLinkDNS(t *testing.T) {
	dir := t.TempDir()
	original := systemdResolveNetif
	systemdResolveNetif = dir
	t.Cleanup(func() { systemdResolveNetif = original })

	writeSysfsFile(t, filepath.Join(dir, "3"), "DNS=10.1.1.1 10.1.1.2\nDOMAINS=corp.local\n")

	servers, search := readLinkDNS(3)
	if len(servers) != 2 || servers[0] != "10.1.1.1" {
		t.Errorf("servers = %v, want two link servers", servers)
	}
	if len(search) != 1 || search[0] != "corp.local" {
		t.Errorf("search = %v, want [corp.local]", search)
	}

	if s, _ := readLinkDNS(99); s != nil {
		t.Errorf("readLinkDNS(absent) = %v, want nil", s)
	}
}

// ---- applyInterfaceDetail -------------------------------------------------

// TestApplyInterfaceDetail_Bridge exercises the whole enrichment path for a
// bridge, including the fallback to system-wide resolvers.
func TestApplyInterfaceDetail_Bridge(t *testing.T) {
	root := t.TempDir()
	withSysClassNet(t, root)

	base := mkIface(t, root, "br0", map[string]string{"operstate": "up\n"}, "bridge", "brif")
	if err := os.MkdirAll(filepath.Join(base, "brif", "veth1"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	original := systemdResolveNetif
	systemdResolveNetif = filepath.Join(t.TempDir(), "absent")
	t.Cleanup(func() { systemdResolveNetif = original })

	info := types.NetworkInfo{Name: "br0", Index: 0}
	applyInterfaceDetail(&info, 7, []string{"9.9.9.9"}, []string{"lan"})

	if info.Kind != kindBridge {
		t.Errorf("Kind = %q, want bridge", info.Kind)
	}
	if info.OperState != "up" {
		t.Errorf("OperState = %q, want up", info.OperState)
	}
	if len(info.BridgePorts) != 1 || info.BridgePorts[0] != "veth1" {
		t.Errorf("BridgePorts = %v, want [veth1]", info.BridgePorts)
	}
	if len(info.DNSServers) != 1 || info.DNSServers[0] != "9.9.9.9" {
		t.Errorf("DNSServers = %v, want the system fallback", info.DNSServers)
	}
	if len(info.DNSSearch) != 1 || info.DNSSearch[0] != "lan" {
		t.Errorf("DNSSearch = %v, want [lan]", info.DNSSearch)
	}
}

// TestApplyInterfaceDetail_PerLinkDNSWins verifies systemd-resolved per-link
// configuration takes precedence over the system-wide fallback.
func TestApplyInterfaceDetail_PerLinkDNSWins(t *testing.T) {
	root := t.TempDir()
	withSysClassNet(t, root)
	mkIface(t, root, "eth0", nil, "device")

	netif := t.TempDir()
	original := systemdResolveNetif
	systemdResolveNetif = netif
	t.Cleanup(func() { systemdResolveNetif = original })
	writeSysfsFile(t, filepath.Join(netif, "5"), "DNS=172.16.0.1\n")

	info := types.NetworkInfo{Name: "eth0"}
	applyInterfaceDetail(&info, 5, []string{"9.9.9.9"}, nil)

	if len(info.DNSServers) != 1 || info.DNSServers[0] != "172.16.0.1" {
		t.Errorf("DNSServers = %v, want the per-link server to win", info.DNSServers)
	}
}
