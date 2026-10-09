# Collector — using it

> **Status: NORMATIVE** · Scope: what the collector can and cannot read on your machine · Last verified: 2026-10-09
> Conflicts with this document are defects. Fix them here first.

The collector is what reads your machine for `sysmon`, the dashboard and the
desktop app. You never run it directly, but what it can see depends on how you
run them.

## Check what it can see

```bash
sysmon doctor
```

It reports, per capability, `ok`, `degraded` or `unavailable`, and a fix where
one exists. Nothing is changed on the machine.

## What affects the readings

| You want | It needs |
|---|---|
| Disk health (SMART, NVMe wear) | Root or the `disk` group; some controllers also need `CAP_SYS_RAWIO` |
| DIMM temperature | The `spd5118` or `jc42` driver bound to the SMBus ([sensors](sensors.md#dimm-temperatures)); go-sysmon never loads it |
| NVIDIA GPU detail | The NVML library and the `nvml` build; AMD and Intel report less |
| Containers and VMs | Readable `/proc` and cgroups; no runtime socket needed |
| Image inventory | Access to the runtime's API socket; only `sysmon images` uses it |
| Processes of other users | `/proc` not mounted `hidepid` |

## How fresh the numbers are

Fast-changing values (CPU, memory, network, load) update every poll. GPU,
processes, sensors and containers update every fifth poll, and disks and
accelerators every thirtieth, so at a 1 s interval a disk's health can be up to
30 s old. The `--refresh` mode on the CLI re-reads everything each frame.

## Troubleshooting

- **A column is blank.** It is unavailable, not zero; run `sysmon doctor`.
- **Disk shows no SMART status.** See [disk and network detail](disk-and-network.md);
  the reason is shown in the storage view.
- **A container or VM tab is missing.** The node was found to have no runtime
  or hypervisor, or that could not be determined; see
  [virtualization](virtualization.md#is-there-a-runtime-or-a-hypervisor-at-all).
- **Rates show `-`.** A rate needs two readings; wait one interval.
