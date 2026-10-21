package collector

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// withUSBIDs points the USB identifier lookup at a synthetic database and
// resets the one-time parse, which would otherwise carry between cases.
func withUSBIDs(t *testing.T, paths ...string) {
	t.Helper()
	origPaths, origDB := usbIDsPaths, usbIDs
	usbIDsPaths, usbIDsOnce = paths, sync.Once{}
	t.Cleanup(func() {
		usbIDsPaths, usbIDsOnce, usbIDs = origPaths, sync.Once{}, origDB
	})
}

// writeIDsFile writes an identifier database body to a file named name.
func writeIDsFile(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

// newNICSysfs builds an empty synthetic sysfs (class/net, devices and one
// directory per bus) and points the network classifier at its class/net.
func newNICSysfs(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{"class/net", "devices", "bus/pci", "bus/usb", "bus/virtio"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	withSysClassNet(t, filepath.Join(root, "class/net"))
	return root
}

// mkSysfsDevice creates devices/<rel> with the given attribute files and, when
// bus is set, a subsystem link to bus/<bus> as the kernel does.
func mkSysfsDevice(t *testing.T, root, rel, bus string, files map[string]string) {
	t.Helper()
	dir := filepath.Join(root, "devices", rel)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if bus != "" {
		if err := os.Symlink(filepath.Join(root, "bus", bus), filepath.Join(dir, "subsystem")); err != nil {
			t.Fatalf("symlink subsystem: %v", err)
		}
	}
	for name, content := range files {
		writeSysfsFile(t, filepath.Join(dir, name), content)
	}
}

// linkNIC creates class/net/<name> with its device link pointing at
// devices/<rel>.
func linkNIC(t *testing.T, root, name, rel string) {
	t.Helper()
	dir := filepath.Join(root, "class/net", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.Symlink(filepath.Join(root, "devices", rel), filepath.Join(dir, "device")); err != nil {
		t.Fatalf("symlink device: %v", err)
	}
}

const testPCIIDs = "# pci.ids\n" +
	"1028  Dell\n" +
	"1af4  Red Hat, Inc.\n" +
	"\t1041  Virtio 1.0 network device\n" +
	"8086  Intel Corporation\n" +
	"\t15f3  Ethernet Controller I225-V\n" +
	"\t\t1028 0a1b  Ethernet Controller I225-V (Dell)\n" +
	"C 02  Network controller\n" +
	"\t00  Ethernet controller\n"

const testUSBIDs = "# usb.ids\n" +
	"0bda  Realtek Semiconductor Corp.\n" +
	"\t8153  RTL8153 Gigabit Ethernet Adapter\n" +
	"\t\t00  Interface\n" +
	"C 02  Communications\n" +
	"\t06  Ethernet Networking\n"

// pciNIC lays out an Intel I225-V on a Dell board at 0000:03:00.0.
func pciNIC(t *testing.T, root string) {
	t.Helper()
	mkSysfsDevice(t, root, "pci0000:00/0000:03:00.0", "pci", map[string]string{
		"vendor":           "0x8086\n",
		"device":           "0x15F3\n",
		"subsystem_vendor": "0x1028\n",
		"subsystem_device": "0x0a1b\n",
	})
	linkNIC(t, root, "enp3s0", "pci0000:00/0000:03:00.0")
}

// usbNIC lays out a Realtek RTL8153 adapter on bus 2 of an Intel xHCI host
// controller: PCI function, USB device, then the USB interface the netdev
// hangs off.
func usbNIC(t *testing.T, root string) {
	t.Helper()
	controller := "pci0000:00/0000:00:14.0"
	mkSysfsDevice(t, root, controller, "pci", map[string]string{
		"vendor": "0x8086\n",
		"device": "0xa0ed\n",
	})
	mkSysfsDevice(t, root, controller+"/usb2/2-1", "usb", map[string]string{
		"idVendor":     "0bda\n",
		"idProduct":    "8153\n",
		"manufacturer": "Realtek\n",
		"product":      "USB 10/100/1000 LAN\n",
	})
	mkSysfsDevice(t, root, controller+"/usb2/2-1/2-1:1.0", "usb", nil)
	linkNIC(t, root, "enx001122334455", controller+"/usb2/2-1/2-1:1.0")
}

func TestApplyNICHardware_PCI(t *testing.T) {
	root := newNICSysfs(t)
	pciNIC(t, root)
	withPCIIDs(t, writeIDsFile(t, "pci.ids", testPCIIDs))

	info := types.NetworkInfo{Name: "enp3s0"}
	applyNICHardware(&info)

	want := types.NetworkInfo{
		Name:              "enp3s0",
		Bus:               "pci",
		VendorID:          "8086",
		DeviceID:          "15f3",
		Vendor:            "Intel Corporation",
		Model:             "Ethernet Controller I225-V",
		SubsystemVendorID: "1028",
		SubsystemDeviceID: "0a1b",
		SubsystemVendor:   "Dell",
	}
	assertNICHardware(t, info, want)
}

func TestApplyNICHardware_USBResolvesTheAdapterNotTheHostController(t *testing.T) {
	root := newNICSysfs(t)
	usbNIC(t, root)
	withPCIIDs(t, writeIDsFile(t, "pci.ids", testPCIIDs))
	withUSBIDs(t, writeIDsFile(t, "usb.ids", testUSBIDs))

	info := types.NetworkInfo{Name: "enx001122334455"}
	applyNICHardware(&info)

	want := types.NetworkInfo{
		Name:     "enx001122334455",
		Bus:      "usb",
		VendorID: "0bda",
		DeviceID: "8153",
		Vendor:   "Realtek Semiconductor Corp.",
		Model:    "RTL8153 Gigabit Ethernet Adapter",
	}
	assertNICHardware(t, info, want)
}

// A virtio NIC's device is the virtio device, whose bus is neither PCI nor
// USB; its identity is the virtio PCI function above it.
func TestApplyNICHardware_VirtioReportsItsPCIFunction(t *testing.T) {
	root := newNICSysfs(t)
	mkSysfsDevice(t, root, "pci0000:00/0000:00:03.0", "pci", map[string]string{
		"vendor": "0x1af4\n",
		"device": "0x1041\n",
	})
	mkSysfsDevice(t, root, "pci0000:00/0000:00:03.0/virtio0", "virtio", map[string]string{
		"vendor": "0x1af4\n",
		"device": "0x0001\n",
	})
	linkNIC(t, root, "eth0", "pci0000:00/0000:00:03.0/virtio0")
	withPCIIDs(t, writeIDsFile(t, "pci.ids", testPCIIDs))

	info := types.NetworkInfo{Name: "eth0"}
	applyNICHardware(&info)

	want := types.NetworkInfo{
		Name:     "eth0",
		Bus:      "pci",
		VendorID: "1af4",
		DeviceID: "1041",
		Vendor:   "Red Hat, Inc.",
		Model:    "Virtio 1.0 network device",
	}
	assertNICHardware(t, info, want)
}

func TestApplyNICHardware_VirtualInterfaceReportsNone(t *testing.T) {
	root := newNICSysfs(t)
	mkIface(t, filepath.Join(root, "class/net"), "br0", nil, "bridge")

	info := types.NetworkInfo{Name: "br0"}
	applyNICHardware(&info)
	assertNICHardware(t, info, types.NetworkInfo{Name: "br0"})

	// An interface sysfs does not list at all is equally left empty.
	missing := types.NetworkInfo{Name: "gone0"}
	applyNICHardware(&missing)
	assertNICHardware(t, missing, types.NetworkInfo{Name: "gone0"})
}

// A device whose ancestry reaches neither a PCI function nor a USB device --
// a platform NIC on an SoC -- is physical but has no identifiers to report.
func TestApplyNICHardware_UnknownBusReportsNone(t *testing.T) {
	root := newNICSysfs(t)
	mkSysfsDevice(t, root, "platform/soc/ff3f0000.ethernet", "", nil)
	linkNIC(t, root, "end0", "platform/soc/ff3f0000.ethernet")

	info := types.NetworkInfo{Name: "end0"}
	applyNICHardware(&info)
	assertNICHardware(t, info, types.NetworkInfo{Name: "end0"})
}

// With no identifier database installed the identifiers are still reported; a
// PCI NIC then has no names, and a USB NIC falls back to its own strings.
func TestApplyNICHardware_IDsFileAbsent(t *testing.T) {
	root := newNICSysfs(t)
	pciNIC(t, root)
	usbNIC(t, root)
	absent := filepath.Join(t.TempDir(), "absent")
	withPCIIDs(t, absent)
	withUSBIDs(t, absent)

	pci := types.NetworkInfo{Name: "enp3s0"}
	applyNICHardware(&pci)
	assertNICHardware(t, pci, types.NetworkInfo{
		Name:              "enp3s0",
		Bus:               "pci",
		VendorID:          "8086",
		DeviceID:          "15f3",
		SubsystemVendorID: "1028",
		SubsystemDeviceID: "0a1b",
	})

	usb := types.NetworkInfo{Name: "enx001122334455"}
	applyNICHardware(&usb)
	assertNICHardware(t, usb, types.NetworkInfo{
		Name:     "enx001122334455",
		Bus:      "usb",
		VendorID: "0bda",
		DeviceID: "8153",
		Vendor:   "Realtek",
		Model:    "USB 10/100/1000 LAN",
	})
}

// A malformed database neither panics nor attributes devices to the wrong
// vendor: an unnamed vendor line, a device line before any vendor, and
// usb.ids' non-vendor sections all end the current vendor.
func TestApplyNICHardware_MalformedIDsFile(t *testing.T) {
	root := newNICSysfs(t)
	pciNIC(t, root)
	usbNIC(t, root)
	withPCIIDs(t, writeIDsFile(t, "pci.ids",
		"\t15f3  Orphan device before any vendor\n"+
			"1028  Dell\n"+
			"8086\n"+ // vendor line without a name
			"\t15f3  Claimed by the previous vendor\n"+
			"zzzz  Not a hex identifier\n"))
	withUSBIDs(t, writeIDsFile(t, "usb.ids",
		"0bda  Realtek Semiconductor Corp.\n"+
			"HID 22  Report\n"+
			"\t8153  Report item, not a Realtek product\n"))

	pci := types.NetworkInfo{Name: "enp3s0"}
	applyNICHardware(&pci)
	assertNICHardware(t, pci, types.NetworkInfo{
		Name:              "enp3s0",
		Bus:               "pci",
		VendorID:          "8086",
		DeviceID:          "15f3",
		SubsystemVendorID: "1028",
		SubsystemDeviceID: "0a1b",
		SubsystemVendor:   "Dell",
	})

	usb := types.NetworkInfo{Name: "enx001122334455"}
	applyNICHardware(&usb)
	assertNICHardware(t, usb, types.NetworkInfo{
		Name:     "enx001122334455",
		Bus:      "usb",
		VendorID: "0bda",
		DeviceID: "8153",
		Vendor:   "Realtek Semiconductor Corp.",
		Model:    "USB 10/100/1000 LAN",
	})
}

// applyInterfaceDetail is the collector's per-interface enrichment; the
// identity must arrive through it.
func TestApplyInterfaceDetail_CarriesNICHardware(t *testing.T) {
	root := newNICSysfs(t)
	pciNIC(t, root)
	withPCIIDs(t, writeIDsFile(t, "pci.ids", testPCIIDs))
	original := systemdResolveNetif
	systemdResolveNetif = filepath.Join(t.TempDir(), "absent")
	t.Cleanup(func() { systemdResolveNetif = original })

	info := types.NetworkInfo{Name: "enp3s0"}
	applyInterfaceDetail(&info, 2, nil, nil)

	if info.Vendor != "Intel Corporation" || info.Model != "Ethernet Controller I225-V" {
		t.Errorf("Vendor/Model = %q/%q, want the pci.ids names", info.Vendor, info.Model)
	}
}

func assertNICHardware(t *testing.T, got, want types.NetworkInfo) {
	t.Helper()
	fields := []struct {
		name      string
		got, want string
	}{
		{"Bus", got.Bus, want.Bus},
		{"VendorID", got.VendorID, want.VendorID},
		{"DeviceID", got.DeviceID, want.DeviceID},
		{"Vendor", got.Vendor, want.Vendor},
		{"Model", got.Model, want.Model},
		{"SubsystemVendorID", got.SubsystemVendorID, want.SubsystemVendorID},
		{"SubsystemDeviceID", got.SubsystemDeviceID, want.SubsystemDeviceID},
		{"SubsystemVendor", got.SubsystemVendor, want.SubsystemVendor},
	}
	for _, f := range fields {
		if f.got != f.want {
			t.Errorf("%s: %s = %q, want %q", got.Name, f.name, f.got, f.want)
		}
	}
}
