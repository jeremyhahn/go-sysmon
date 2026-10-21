### Added

- **Wifi link detail.** The Network panel shows a wifi interface's link
  quality, signal level and noise floor (dBm), which the collector already
  read from `/proc/net/wireless`. The collector now also reports the SSID and
  BSSID of the associated network (`wireless.ssid`, `wireless.bssid`), read
  unprivileged from nl80211, and the Network panel and the CLI show them.
  Wired interfaces are unchanged (#259).
