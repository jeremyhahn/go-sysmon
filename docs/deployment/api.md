# Remote deployment — API surface

> **Status: NORMATIVE** · Scope: what a deployed instance exposes on the network · Last verified: 2026-08-19
> Conflicts with this document are defects. Fix them here first.

**The deployment feature defines no API of its own.** It is a shell script and a
systemd unit; it is driven over SSH and has no endpoints, no RPCs and no
frames. There is nothing to call.

What it does determine is **what the deployed instance exposes**, which is the
part an operator has to reason about. The routes themselves belong to the server
and are specified in [../server/api.md](../server/api.md); this page records the
deployed surface, so that anyone auditing a host can see the whole exposure in
one place without reading the server docs.

## Ports bound by a deployed instance

**One TCP listener. That is the entire network surface.**

| Port | Transport | Default | Set by | Purpose |
|---|---|---|---|---|
| 8081 | TCP (HTTP, or HTTPS with `--tls-cert`) | `:8081` — every interface | `SYSMON_ADDR` in `sysmon.env` | Dashboard and its API |

Nothing else is opened: no metrics port, no admin port, no Unix socket, no
health port, no outbound connections. The service does not dial out — the one
socket it opens toward anything is the `AF_UNIX` probe of well-known container
runtime socket paths, which finds nothing on a host with no runtime and is not a
network operation.

The binary's own default is `:8080`. **The deployed default is `:8081`**,
because 8080 is the port other services on a shared host are most likely to
want. Check before choosing:

```bash
ss -ltnp
```

A collision is a straight bind failure at boot, and the loser is whichever
service starts second.

## Routes on that port

Reproduced from [../server/api.md](../server/api.md) so that the deployed
exposure is legible here. That document is authoritative if the two disagree.

| Route | Method | Returns |
|---|---|---|
| `/` | GET | The dashboard (embedded single-page app) |
| `/api/snapshot` | GET | One JSON document: the complete current host state |
| `/api/events` | GET | `text/event-stream`, one snapshot per poll interval |
| `/api/interval` | GET, POST | Read or change the poll interval at runtime |

`sysmon` is a single-binary program with an embedded HTTP server. There is no
gRPC service, no QUIC listener, no MCP tool surface and no Unix-socket
transport — those transports do not exist in this project, so there is no
capability row to record as missing against them.

## Authentication

**There is none, on any route.** This is a property of the program, not of the
deployment.

`/api/snapshot` returns the hostname, board and BIOS identifiers, every mounted
filesystem, disk models and serial numbers, every network interface with
addresses and DNS servers, and the full process list including command lines.
Command lines routinely carry information that is not meant to be public.
Anything that can route to the port reads all of it.

`/api/interval` accepts POST without authentication, so anything that can reach
the port can also raise the poll rate and with it the service's CPU cost.

**Nothing bounds that cost any more.** The unit used to carry `CPUQuota=50%`,
which capped it; that quota was removed on 2026-09-03 because a hard CFS stop
wedged the process (issue #65), and `CPUWeight` — what replaced it — is
work-conserving and imposes no ceiling at all. The damage is bounded only by
arithmetic: `allowedIntervals` floors the poll at 500 ms, so the worst an
unauthenticated caller can do is multiply the shipped 2 s cost by **4**. On a
big host that is a nuisance; on the 4-thread reference target, where a tick
already costs a tenth of a core, it is most of the machine.

The browser same-origin policy stops a random web page from reading the event
stream cross-origin. That is not authentication: anything that is not a browser
ignores it entirely.

Two controls, and they are the whole story:

1. **Bind one interface.** `SYSMON_ADDR=10.0.0.5:8081` in `sysmon.env` takes the
   listener off every other interface.
2. **Put an authenticating proxy in front.** Bind loopback
   (`SYSMON_ADDR=127.0.0.1:8081`) and let the proxy be the only thing that
   reaches it.

TLS via `--tls-cert` / `--tls-key` encrypts the transport. It does not
authenticate the client, and an eavesdropper is not the threat here — an
unauthenticated reader is.

## Versioning

The routes are unversioned. The JSON documents are additive in practice: fields
are added as collectors gain data, and a client that ignores unknown fields
keeps working. Nothing here is a compatibility promise — see
[../server/api.md](../server/api.md) for the server's own position.
