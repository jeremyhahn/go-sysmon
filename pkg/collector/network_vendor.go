package collector

import (
	"path/filepath"
	"sync"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// Buses a physical NIC's identity is read from.
const (
	nicBusPCI = "pci"
	nicBusUSB = "usb"
)

// nicAncestorLimit bounds the walk from an interface's device directory up its
// sysfs ancestry. A USB NIC's identity is one level up (the interface's
// parent device), a virtio NIC's one level up (the virtio PCI function), and
// a PCI NIC's at the device itself; the limit only stops a walk that never
// finds either bus.
const nicAncestorLimit = 8

// usbIDsPaths are the locations of the hwdata USB identifier database, tried in
// order. Like pci.ids it may be absent; a USB device's own manufacturer and
// product strings then name it.
var usbIDsPaths = []string{
	"/usr/share/hwdata/usb.ids",
	"/usr/share/misc/usb.ids",
	"/var/lib/usbutils/usb.ids",
}

// usbIDsOnce guards the one-time parse of the USB identifier database.
var (
	usbIDsOnce sync.Once
	usbIDs     idDatabase
)

func loadUSBIDs() {
	usbIDs = loadIDDatabase(usbIDsPaths)
}

// applyNICHardware fills in the bus, identifiers, manufacturer and model of a
// physical interface from sysfs. It walks from /sys/class/net/<if>/device up
// the device's ancestry to the first PCI function or USB device, so a USB
// adapter is identified by the USB device and not by the host controller it
// hangs off. A virtual interface has no device link and is left untouched.
func applyNICHardware(info *types.NetworkInfo) {
	dir, err := filepath.EvalSymlinks(filepath.Join(sysClassNet, info.Name, "device"))
	if err != nil {
		return
	}
	for range nicAncestorLimit {
		// The subsystem link names the device's bus the same way the driver
		// link names its driver: by the base name of its target.
		switch readNICDriver(filepath.Join(dir, "subsystem")) {
		case nicBusPCI:
			applyPCIIdentity(info, dir)
			return
		case nicBusUSB:
			// A USB interface directory sits below the USB device and has no
			// idVendor of its own.
			if pathExists(filepath.Join(dir, "idVendor")) {
				applyUSBIdentity(info, dir)
				return
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return
		}
		dir = parent
	}
}

// applyPCIIdentity reads a PCI function's identifiers and resolves their names
// from pci.ids.
func applyPCIIdentity(info *types.NetworkInfo, dir string) {
	info.Bus = nicBusPCI
	info.VendorID = normalizeHexID(readSysfsString(filepath.Join(dir, "vendor")))
	info.DeviceID = normalizeHexID(readSysfsString(filepath.Join(dir, "device")))
	info.SubsystemVendorID = normalizeHexID(readSysfsString(filepath.Join(dir, "subsystem_vendor")))
	info.SubsystemDeviceID = normalizeHexID(readSysfsString(filepath.Join(dir, "subsystem_device")))
	info.Vendor, info.Model, _ = lookupPCINames(info.VendorID, info.DeviceID, "")
	info.SubsystemVendor, _, _ = lookupPCINames(info.SubsystemVendorID, "", "")
}

// applyUSBIdentity reads a USB device's identifiers and resolves their names
// from usb.ids, falling back to the strings the device reports about itself.
func applyUSBIdentity(info *types.NetworkInfo, dir string) {
	info.Bus = nicBusUSB
	info.VendorID = normalizeHexID(readSysfsString(filepath.Join(dir, "idVendor")))
	info.DeviceID = normalizeHexID(readSysfsString(filepath.Join(dir, "idProduct")))

	usbIDsOnce.Do(loadUSBIDs)
	info.Vendor = usbIDs.vendors[info.VendorID]
	if info.Vendor == "" {
		info.Vendor = readSysfsString(filepath.Join(dir, "manufacturer"))
	}
	info.Model = usbIDs.devices[info.VendorID+":"+info.DeviceID]
	if info.Model == "" {
		info.Model = readSysfsString(filepath.Join(dir, "product"))
	}
}
