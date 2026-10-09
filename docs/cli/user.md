# CLI — using it

> **Status: NORMATIVE** · Scope: `sysmon` command line · Last verified: 2026-10-09
> Conflicts with this document are defects. Fix them here first.

## Before you start

Build or install the binary ([server/deployment.md](../server/deployment.md)).
No server, config file or daemon is needed: every command reads the machine it
runs on. Some readings need privilege: SMART needs root or the `disk` group.

## First run

```bash
sysmon version      # confirm which build you have
sysmon doctor       # what this build can do here, and how to fix what it can't
sysmon overview     # one-shot summary
```

Run `sysmon` with no arguments on a desktop with the desktop build and the
window opens; anywhere else you get `overview`.

## Common tasks

| I want to… | Run |
|---|---|
| Watch a subsystem live | `sysmon --refresh 1 cpu` (decimals allowed; Ctrl+C stops) |
| Look at one disk, NIC, GPU or core | `sysmon storage --index 0` |
| Pipe to a script | `sysmon --json memory \| jq .` |
| See what the containers are doing | `sysmon containers` |
| Find reclaimable image space | `sysmon images --unused --sort size` |
| See guests on a hypervisor | `sysmon vms` |
| Keep a log | `sysmon --log-file ~/sysmon.log --log-level debug cpu` |
| Run the dashboard | `sysmon serve --addr :8080` ([server/user.md](../server/user.md)) |

Every verb and flag is in [cli.md](cli.md); what each prints is explained in
[reference.md](reference.md).

## Troubleshooting

- **A value shows `-`.** It is a rate the second reading could not measure, not
  zero. With `--refresh`, the first frame always shows `-` for CPU usage.
- **Health columns on `storage` are empty.** SMART was refused. `sysmon doctor`
  says whether it is file permissions or a missing capability, and what fixes it.
- **`containers` or `vms` is empty.** The first line says whether the node has
  a runtime or hypervisor at all, or why that is unknown.
- **`images` is slow.** It walks every layer through the runtime's API; several
  seconds on a large library is normal.
- **`Error: …` and exit 1.** The message is the reason; add `--log-level debug`
  for detail on stderr.
- **`sysmon` fails to start with a missing shared library.** You are running the
  desktop build on a host without GTK/WebKit; use `sysmon-server`.
- **Output cut by `| head`.** The command exits non-zero on the closed pipe;
  that is expected.
