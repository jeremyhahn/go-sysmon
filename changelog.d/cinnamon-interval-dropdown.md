### Fixed

- **The Cinnamon applet keeps a 500 ms or 1 minute update interval when it is
  updated.** The interval was a spinbutton bounded at 500 and 60000 ms.
  Cinnamon's settings upgrade keeps a spinbutton value only when it lies
  strictly between those bounds, so an applet update reset both values to
  1000 ms. The setting is now a dropdown of exactly the intervals the server
  accepts, and Cinnamon keeps a dropdown value that is still one of the
  options. The desktop test now checks both the upgrade path and a saved
  settings file.
