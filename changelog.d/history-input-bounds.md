### Fixed

- **A history request whose `start`, `end` or `step` is too large to convert is
  refused.** A millisecond count whose nanoseconds do not fit a 64-bit integer
  wrapped instead: a `step` of 288230376151712744 was served as a one-second
  step, and a range past the year 2262 was asked of the database as one in
  1677 and answered as the range requested. Each is now a 400 naming the
  parameter, as is a range so early that what a counter reads before it would
  wrap.
