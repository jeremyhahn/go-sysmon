# Drive slots

> **Status: REFERENCE** · Last verified: 2026-10-27
> As-built description of the drive-slot collector (#289).

The drive-slot collector reads each drive bay's health from the node's BMC:
presence, health, the fault LED, a predicted failure, a rebuild in progress and,
where the BMC reports it, the media life left. It exists because a bay can be
faulted while every block device the kernel sees reads healthy: the drive sits
behind a RAID controller, and the BMC is the one component that sees the bay.

It polls once a minute at any snapshot interval, and a whole poll is bounded
at two minutes. A one-shot CLI command waits for its poll to finish before it
takes the reading it prints. The result
is the snapshot's `drive_slots` field, the **Drive bays** card on the Storage
tab, the `drive_slot_fault` alert and the `sysmon_drive_slot_*` series.

## Sources

1. **Redfish**, when `bmc.redfish_url` is set. The collector walks
   `/redfish/v1/Systems/*/Storage/*` for their `Drives`. When that finds none,
   or the service does not implement `/redfish/v1/Systems` and answers it 404,
   it walks `/redfish/v1/Chassis/*`, using the chassis `Links.Drives` and its
   `Drives` collection. Any other error on Systems ends the poll. It then reads each drive. Each collection is read
   across all its pages: the walk follows `Members@odata.nextLink`, and a link
   back to a page it has already read ends the walk with an error.
2. **In-band IPMI**, through `/dev/ipmi0`, when Redfish is not configured or
   fails. A reading taken over IPMI after a Redfish failure still carries the
   Redfish error in `reason`, so a broken configuration is not hidden behind a
   working fallback. This source is available on linux/amd64 and linux/arm64
   only.

With neither source the collector is unavailable. `reason` then says
`no BMC configured (bmc.redfish_url) and no IPMI device at /dev/ipmi0`. That
is a fact about the host, not a failure.

## Configuration

```yaml
bmc:
  redfish_url: https://10.0.0.5          # https only; scheme and host are used
  username: monitor
  password_file: /etc/go-sysmon/bmc-password
  ca_file: /etc/go-sysmon/bmc-ca.pem     # optional: pin the BMC's CA
  insecure_skip_verify: false            # optional, explicit opt-out
  timeout_ms: 10000                      # per request; default 10000
```

| Key | Meaning |
|---|---|
| `bmc.redfish_url` | The BMC's Redfish endpoint. It must be `https`. The URL must not contain credentials, and a URL that does is rejected without being quoted. Only the scheme and host are used: every request goes under `/redfish/v1`. Empty means Redfish is off. |
| `bmc.username` | The Redfish account. Required with `redfish_url`. A read-only BMC role is enough. |
| `bmc.password_file` | The file that holds the password. Required with `redfish_url`. |
| `bmc.ca_file` | A PEM bundle of the CAs allowed to have signed the BMC's certificate, instead of the system roots. |
| `bmc.insecure_skip_verify` | Turns certificate verification off. Default `false`. |
| `bmc.timeout_ms` | The timeout for one Redfish request. A value of zero or less is replaced by the default. |

Every key is **boot-only**: a change takes effect on restart. The settings view
withholds `redfish_url`, `username`, `password_file` and `ca_file`, reporting
only whether each is set.

### The password file

The password is read from `password_file` **on every poll** and is never held
in the configuration. A rotated password is picked up on the next poll, with no
restart. A trailing newline is ignored.

**The file must not be readable by other users.** A file with any of the mode's
other-user bits set is refused: run `chmod o-rwx` on it. Make it `0600`, owned
by the service's user, or `0640 root:<service group>`. An empty file is refused
too. The password never appears in a log line, an error, a metric label or the
snapshot.

### TLS

Verification is **on by default**, against the system roots, with TLS 1.2 as
the minimum. A BMC usually ships a self-signed certificate. In order of
preference:

1. **Pin it with `ca_file`.** Export the BMC's certificate, or the CA that
   signed it, as PEM, and point `ca_file` at it. Verification stays on, and only
   that certificate is trusted.
2. **Set `insecure_skip_verify: true`.** This turns verification off: the
   connection is still encrypted, but nothing authenticates the BMC, and the
   password is sent to whoever answers. It is never inferred. Someone has to
   write it in the config.

## What goes on the wire

**Redfish: GET only.** Each request carries `Authorization: Basic`, `Accept:
application/json` and `OData-Version: 4.0`. The collector has no write path to
the BMC. It also refuses:

- a link that leaves the configured host, or points outside `/redfish/v1`;
- a response larger than 4 MiB;
- more than 512 requests in one walk.

**IPMI: three read commands, and nothing else.** Each request goes through one
function that refuses any command not on this list before it reaches the device:

| NetFn | Command | Name |
|---|---|---|
| `0x0A` Storage | `0x22` | Reserve SDR Repository |
| `0x0A` Storage | `0x23` | Get SDR |
| `0x04` Sensor | `0x2D` | Get Sensor Reading |

The collector walks the SDR repository for sensors the BMC owns of sensor type
`0x0D`, Drive Slot. It reads each one's state, and stops after 1024 records.
A sensor whose reading is unavailable, or whose scanning the BMC has disabled,
is reported with no health and raises no alert: its state bits may be stale.
Its `state` says which (`reading unavailable` or `sensor scanning disabled`).

## IPMI device access

The service needs **read access to `/dev/ipmi0`**, which it opens read-only.
`deploy/go-sysmon.service` grants this with the same lines that give SMART its
device access ([Privileges](../deployment/architecture.md#privileges)):

- `PrivateDevices=no`: the host's `/dev`, so the node is visible.
- `AmbientCapabilities=CAP_SYS_ADMIN CAP_DAC_OVERRIDE`, with the same
  `CapabilityBoundingSet=`. `CAP_DAC_OVERRIDE` is what opens the
  `root:root 0600` node. The IPMI ioctls need no further capability.
- `SupplementaryGroups=disk` is in the unit for the block devices. It does not
  reach `/dev/ipmi0`.

The host still has to load the `ipmi_devintf` and `ipmi_si` modules. List them
in `/etc/modules-load.d/` so they stay loaded.

A host where Redfish answers needs none of this.

## Fields

`drive_slots` carries `available`, `source` (`redfish` or `ipmi`), `reason` and
`slots`. Each slot has these fields:

| Field | Meaning |
|---|---|
| `id` | The bay's stable identity. For Redfish it is the drive's canonical `@odata.id` path, such as `/redfish/v1/Systems/1/Storage/SAS/Drives/0`, or the link it was read from when the BMC omits `@odata.id`. It is not the Drive `Id`, which is unique only within its collection, so two controllers can each have a drive `0`. For IPMI it is the sensor's name. It is the UI row key, the metric label and the alert resource. |
| `slot` | The label printed on the chassis: Redfish `PhysicalLocation.PartLocation.ServiceLabel`, then the drive's `Name`, then its `Id`, then the last segment of `id`. |
| `present` | A drive is in the bay. In Redfish this is any `Status.State` other than `Absent`. |
| `health` | `ok`, `warning`, `critical`, or empty when unknown. An empty bay and an unreadable sensor have no health. |
| `state` | The source's own words. For Redfish this is `Status.State`. For IPMI it is the asserted offsets, comma-separated (`Drive Presence, Drive Fault`). |
| `fault` | The fault LED. In Redfish this is Health `Critical`. In IPMI it is the Drive Fault or In Failed Array offset. |
| `failure_predicted` | The drive predicts its own failure, and its health is at least `warning`. |
| `rebuilding` | A rebuild or remap is in progress. |
| `media_life_left_percent`, `media_life_reported` | Redfish `PredictedMediaLifeLeftPercent`. The percentage means something only when `media_life_reported` is true. IPMI has no such reading. |
| `manufacturer`, `model`, `serial_number`, `media_type`, `protocol`, `capacity_bytes` | Redfish inventory. IPMI leaves them empty. |

## Alert and metrics

`drive_slot_fault` (Storage tab, always on) fires for each bay that has a fault
or `critical` health (severity critical), and for each bay that predicts its
failure or has `warning` health (severity warning). Its resource is the bay's
`id`. See [alerts](../server/alerting-fleet-config.md#alerts).

The metrics are `sysmon_drive_slots_available`, plus these series labelled by
`slot` (the `id`):

- `sysmon_drive_slot_present`
- `sysmon_drive_slot_fault`
- `sysmon_drive_slot_failure_predicted`
- `sysmon_drive_slot_rebuilding`
- `sysmon_drive_slot_health` (0 ok, 1 warning, 2 critical): only when the health is known.
- `sysmon_drive_slot_media_life_left_percent`: only when the BMC reports it.

`reason` is never exported.
