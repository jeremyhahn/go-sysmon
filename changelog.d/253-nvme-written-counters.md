### Added

- **NVMe write and read volume on `/metrics`.** NVMe drives that report SMART
  data units export `sysmon_disk_data_units_written_total`,
  `sysmon_disk_data_units_read_total`, and the same volume in bytes as
  `sysmon_disk_written_bytes_total` and `sysmon_disk_read_bytes_total` (one
  data unit is 1000 × 512 = 512000 bytes). All four are counters, so
  `increase(...[7d])` gives a drive's weekly write volume (#253).
- **Drive serial on the disk health series.** `sysmon_disk_smart_healthy`,
  `sysmon_disk_power_on_hours`, `sysmon_disk_wear_percent`,
  `sysmon_disk_spare_percent` and the new volume counters carry a `serial`
  label beside `device`, so a drive's history can be followed across a boot
  that renames NVMe devices. A drive that reports no serial has no `serial`
  label. Adding the label starts new series for those metrics; queries that
  select on `device` keep working (#253).
