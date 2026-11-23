### Changed

- **Node.js 26.13.1 LTS.** `.node-version` and the Dockerfile's frontend stage
  move from Node.js 24.21.0 to 26.13.1, the current LTS line (Lithium). The
  `golang:1.27.4-trixie` and `debian:trixie-slim` image digests move to the
  current builds of the same tags.
- **Analysis tools at their latest releases.** golangci-lint v2.14.0 -> v2.15.0
  and govulncheck v1.9.0 -> v1.10.0.
- **Go modules at their latest releases.** go-nvml v0.13.4-1, ghw v0.27.0,
  gopsutil v4.26.10, Wails v2.17.1, `go.yaml.in/yaml/v3` v3.0.6,
  `golang.org/x/mod` v0.43.0, `golang.org/x/net` v0.63.0, `golang.org/x/sys`
  v0.50.0, and every indirect module. No call site changed.
- **Dashboard dependencies at their latest releases.** @playwright/test 1.65.1,
  svelte 5.59.1, svelte-check 4.7.8, @sveltejs/vite-plugin-svelte 7.3.2,
  tailwindcss and @tailwindcss/vite 4.3.5, daisyui 5.8.2 and @types/node 26.6.6.
  The postcss override moves from 8.5.26 to 8.5.33, because 8.5.26 sat below
  vite 8.3.8's own `^8.5.32` requirement. TypeScript stays at 6.0.3:
  svelte-check 4.7.8 rejects TypeScript 7 unless TypeScript 6 is installed
  beside it and its experimental `--tsgo` flag is used.
