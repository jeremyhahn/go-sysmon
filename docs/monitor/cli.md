# Monitor — CLI

> **Status: NORMATIVE** · Scope: `pkg/monitor` · Last verified: 2026-10-09
> Conflicts with this document are defects. Fix them here first.

No CLI surface: the monitor is a library package with no command of its own.
It is driven by `sysmon serve` (polling interval: `--interval`, see
[cli/cli.md](../cli/cli.md)) and by `--refresh` on other verbs, which poll the
collector directly rather than through the monitor.
