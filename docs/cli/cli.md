# CLI — verbs and flags

> **Status: NORMATIVE** · Scope: `cmd/sysmon` · Last verified: 2026-10-09
> Conflicts with this document are defects. Fix them here first.

The binary is `sysmon` (desktop build) or `sysmon-server` (portable build);
both carry every subcommand. Each verb's behaviour, output and examples are in
[reference.md](reference.md); this page is the index of what exists.

All commands work without a server: they collect locally. `serve` is the only
verb that starts one.

## Global flags

| Flag | Type | Default | Description |
|---|---|---|---|
| `--json` | bool | `false` | JSON instead of text |
| `--refresh` | float64 | `0` | Seconds between frames; `0` is one-shot |
| `--no-tray` | bool | `false` | No tray icon (desktop build) |
| `--log-file` | string | *(stderr)* | Append logs to this path; parent directories are created |
| `--log-level` | string | `info` | `debug`, `info`, `warn`, `error` |
| `--log-format` | string | `text` | `text` or `json` |

## Verbs

| Verb | Purpose | Own flags |
|---|---|---|
| *(none)* | GUI if a display is present and the build has it, else `overview` | |
| `overview` | Whole-system summary | |
| `host` | Hostname, OS, platform, kernel, uptime, boot time | |
| `cpu` | Topology, per-core usage, temperatures, power | `--index` |
| `memory` | RAM, swap, DIMM table | `--index` (DIMM) |
| `storage` | Disks, SMART/NVMe health, partitions | `--index` |
| `network` | Interfaces, bridges, bonds, wifi | `--index` |
| `gpu` | NVIDIA, AMD and Intel GPUs | `--index` |
| `npu` | Compute accelerators: identity, residency, utilisation, per-client use | `--index` |
| `containers` | Cgroup-derived container accounting | `--index` |
| `vms` | Guests seen from the hypervisor process | `--index` |
| `images` | Container image inventory (queries the runtime API) | `--sort`, `--filter`, `--unused`, `--limit` |
| `doctor` | What this build can do on this machine, and fixes | |
| `serve` | HTTP server and dashboard | `--addr`, `--interval`, `--tls-cert`, `--tls-key`, `--trusted-proxies`, `--config` |
| `version` | Version, commit, build date | |

`--index` is an `int`, default `-1` (show all), 0-based.

## `serve`

| Flag | Type | Default | Description |
|---|---|---|---|
| `--addr` | string | `:8080` | Listen address |
| `--interval` | int | `1000` | Polling interval in milliseconds; must be greater than zero |
| `--tls-cert` | string | *(none)* | PEM certificate; enables HTTPS |
| `--tls-key` | string | *(none)* | PEM key matching `--tls-cert` |
| `--trusted-proxies` | strings | *(none)* | CIDR prefixes of reverse proxies whose forwarding headers are believed; see [deployment](../server/deployment.md#trusted-proxies) |
| `--config` | string | `/etc/go-sysmon/sysmon.yaml` | YAML configuration; a missing file is not an error |

The defaults come from the configuration layer, so the file and `SYSMON_*`
environment variables can set the same values; a flag given on the command line
wins. See [server/cli.md](../server/cli.md) for signals and reload.

```bash
sysmon serve --addr :8443 --tls-cert /etc/ssl/sysmon.crt --tls-key /etc/ssl/sysmon.key
```

## Exit codes

`0` on success, `1` on any error (printed as `Error: <message>` on stderr).

## Examples

```bash
sysmon cpu --index 0 --json | jq '.cpus[0]'
sysmon --refresh 2 network
sysmon images --unused --sort size --limit 10
sysmon --log-file /var/log/sysmon/run.log --log-level debug doctor
```
