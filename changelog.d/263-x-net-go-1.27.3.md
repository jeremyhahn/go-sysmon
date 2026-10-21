### Security

- **`golang.org/x/net` v0.60.0 and Go 1.27.3.** x/net moves from v0.57.0 to
  v0.60.0 for CVE-2026-78669, and the Go toolchain from 1.27.2 to 1.27.3 for
  GO-2026-6809 (`crypto/x509`). `golang.org/x/crypto`, `golang.org/x/sys` and
  `golang.org/x/text` move with x/net (#263).
