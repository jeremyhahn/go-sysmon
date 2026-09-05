import { defineConfig } from '@playwright/test';
import { e2ePort } from './e2e-port';

// One port per CI job rather than one for the whole fleet: see e2e-port.ts for
// why it is derived and not probed, and issue #62 for what a constant cost us.
const port = e2ePort();

export default defineConfig({
  testDir: './tests',
  timeout: 30000,
  retries: 1,
  // Single worker: interval tests mutate the shared server's polling rate,
  // which can starve parallel tests of snapshot data.
  workers: 1,
  use: {
    baseURL: `http://127.0.0.1:${port}`,
    headless: true,
    screenshot: 'only-on-failure',
  },
  webServer: {
    // CGO must stay enabled: pkg/collector imports NVIDIA go-nvml, which does
    // not compile with CGO_ENABLED=0.
    //
    // -tags prometheus builds the shape that SHIPS. Without it, since the tag
    // landed (#100), this suite ran against a server with no history support at
    // all and still passed -- every history spec mocks /api/history* through
    // page.route, so nothing noticed. "The browser tests pass" has to keep
    // meaning something about the built server: the mocks stay, because they
    // make the specs deterministic, but a spec that ever routes through to a
    // real endpoint must reach the real endpoint rather than an absence shaped
    // like one.
    // The frontend is rebuilt here as well: the Go binary embeds
    // frontend/dist, so building only the binary would serve whatever bundle
    // happened to be on disk and silently test a stale UI.
    command: `npm run build && cd ../../../ && CGO_ENABLED=1 go build -tags prometheus -o bin/sysmon-server ./cmd/sysmon && ./bin/sysmon-server serve --addr 127.0.0.1:${port} --interval 500`,
    // A dedicated high port: :8080 is a common default and reuseExistingServer
    // would otherwise run the whole suite against an unrelated service.
    port,
    timeout: 30000,
    reuseExistingServer: !process.env.CI,
  },
});
