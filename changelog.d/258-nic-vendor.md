### Added

- **NIC manufacturer and model.** Each physical interface in `NetworkInfo`
  now carries its `bus` (`pci` or `usb`), `vendor_id` and `device_id`, a PCI
  board's `subsystem_vendor_id` and `subsystem_device_id`, and the `vendor`,
  `model` and `subsystem_vendor` names from `pci.ids` or `usb.ids` (a USB
  adapter's own strings when `usb.ids` is absent). The network panel shows
  them on one line per NIC; virtual interfaces report none (#258).
