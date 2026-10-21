### Fixed

- **systemd notifications can no longer block forever.** Each `READY`,
  `WATCHDOG` and `STATUS` write to `NOTIFY_SOCKET` now has a one-second
  deadline (`sdnotify.SendTimeout`); a manager that stops draining its socket
  produces a `*types.NotifySendError` instead of stalling the watchdog loop.
  The abstract-socket unit test binds a name unique per run, so concurrent
  test runs sharing a network namespace cannot collide (#265).
