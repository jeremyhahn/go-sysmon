import { defineConfig } from '@playwright/test';

export default defineConfig({
  testDir: './tests',
  timeout: 30000,
  retries: 1,
  // Single worker: interval tests mutate the shared server's polling rate,
  // which can starve parallel tests of snapshot data.
  workers: 1,
  use: {
    baseURL: 'http://127.0.0.1:18080',
    headless: true,
    screenshot: 'only-on-failure',
  },
  webServer: {
    // CGO must stay enabled: pkg/collector imports NVIDIA go-nvml, which does
    // not compile with CGO_ENABLED=0.
    // The frontend is rebuilt here as well: the Go binary embeds
    // frontend/dist, so building only the binary would serve whatever bundle
    // happened to be on disk and silently test a stale UI.
    command: 'npm run build && cd ../../../ && CGO_ENABLED=1 go build -o bin/sysmon-server ./cmd/sysmon && ./bin/sysmon-server serve --addr 127.0.0.1:18080 --interval 500',
    // A dedicated high port: :8080 is a common default and reuseExistingServer
    // would otherwise run the whole suite against an unrelated service.
    port: 18080,
    timeout: 30000,
    reuseExistingServer: !process.env.CI,
  },
});
