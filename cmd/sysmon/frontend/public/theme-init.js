// Pre-paint theme bootstrap.
//
// The application's module script runs after first paint, so applying the
// theme from there alone would flash the wrong one on every load -- most
// visibly a white page for a user who chose dark. This is the one script that
// has to run before the body renders, so index.html loads it as a classic,
// render-blocking script in <head>, and it duplicates the storage key and the
// resolution rule from src/lib/theme.svelte.ts. Keep the two in step;
// tests/theme.spec.ts asserts the first frame.
//
// It is a file rather than inline in index.html because the server's
// Content-Security-Policy allows no inline script (script-src 'self'). It sits
// in public/ so Vite copies it verbatim instead of bundling it: a bundled
// script would be a deferred module, which runs after first paint -- the flash
// this exists to prevent.
//
// The try/catch wraps the storage read ALONE. localStorage throws outright
// in a sandboxed frame or with cookies blocked, and a catch around the
// whole block would answer that by forcing dark -- disagreeing with the
// application store, which treats the same failure as "no choice stored"
// and follows the OS. On a light-preferring machine that is precisely the
// flash this script exists to prevent, just in the other direction.
(function () {
  var stored = null;
  try {
    stored = localStorage.getItem("sysmon.theme");
  } catch (e) {
    // Storage unavailable: no stored choice, so follow the OS below.
  }
  // A stored choice wins. With none -- including the "system" value an
  // older build may have left behind -- the OS preference decides the
  // first load, and the first explicit choice replaces it for good.
  var dark =
    stored === "light" || stored === "dark"
      ? stored === "dark"
      : !window.matchMedia ||
        window.matchMedia("(prefers-color-scheme: dark)").matches;
  document.documentElement.setAttribute("data-theme", dark ? "dark" : "light");
})();
