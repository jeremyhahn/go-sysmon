### Added

- **Trusted reverse proxies.** `server.trusted_proxies` (`--trusted-proxies`,
  `SYSMON_SERVER_TRUSTED_PROXIES`) lists the CIDR prefixes of the proxies in
  front of a node. For a request from one of them, go-sysmon reads `Forwarded`,
  else `X-Forwarded-For` and `X-Forwarded-Proto`, and uses the client's scheme
  and address to send HSTS and to log the real client. Other peers' forwarding
  headers are ignored, malformed headers are ignored rather than refused, and an
  invalid prefix stops startup. Empty by default, so nothing changes unless it
  is set (#262).
