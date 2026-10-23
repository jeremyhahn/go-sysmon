# Monitor — using it

> **Status: NORMATIVE** · Scope: polling behaviour as seen by users · Last verified: 2026-10-09
> Conflicts with this document are defects. Fix them here first.

You do not run the monitor directly; it is the poller inside `sysmon serve` and
the desktop app. What it means for you:

- **One poll serves everyone.** Five browser tabs cost one collection per
  interval, not five.
- **The refresh rate is shared.** Changing the interval in the dashboard changes
  it for every connected viewer, and takes effect on the next frame.
- **Choosing a rate.** The dashboard dropdown offers 500 ms, 1 s, 5 s,
  10 s, 15 s, 30 s and 60 s. The starting rate is `--interval` (default 1000 ms)
  or `server.interval_ms` in the config file, which a reload applies live.
- **A slow client misses frames, not the server.** If a viewer falls behind it
  skips to the newest reading.

## Troubleshooting

- **The dashboard looks stale.** The page shows the last reading the monitor
  collected. A long interval (60 s) or a stuck collector means an old number;
  check the interval first.
- **A rename or threshold change does not show immediately.** Reloading the
  config re-labels the current reading at once; new measurements arrive on the
  next poll.
