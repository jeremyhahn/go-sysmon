import { test, expect, type Page } from '@playwright/test';
import { mockSnapshotFeed } from './fixtures/mock-sse';
import snapshot from './fixtures/snapshot.json' with { type: 'json' };
import { openTab } from './fixtures/tabs';

/**
 * Regression test for the stale peer-response race.
 *
 * The dashboard used to check which node was selected *before* awaiting the
 * response body. A switch to another peer during `res.json()` therefore could
 * not be seen: the check had already passed, and the apply-time guard only
 * asked whether *some* peer was selected, not which one. The result was one
 * host's snapshot and history rendered under another host's name.
 *
 * Reproducing it needs the delay between the response resolving and its body
 * parsing, which `route.fulfill` cannot express — it sends headers and body
 * together. So window.fetch is patched to hand back a response whose json()
 * settles late, which is precisely the window the old code was blind to.
 */

const PARSE_DELAY_MS = 2000;

const alphaSnapshot = {
  ...snapshot,
  host: { ...snapshot.host, hostname: 'alpha-host' },
};
const bravoSnapshot = {
  ...snapshot,
  host: { ...snapshot.host, hostname: 'bravo-host' },
};

function fleetNode(name: string, hostname: string) {
  return {
    name,
    self: false,
    reachable: true,
    age_seconds: 1,
    status: 'healthy',
    hostname,
    version: '1.0.0',
    uptime_seconds: 3600,
    critical_count: 0,
    warning_count: 0,
    cpu_percent: 5,
    memory_percent: 20,
    swap_percent: 0,
    load_per_core: 0.1,
    worst_disk_percent: 30,
    raid_degraded: false,
    smart_unhealthy: 0,
    max_temp_c: 40,
    power_w: 0,
    net_rx_rate: 0,
    net_tx_rate: 0,
  };
}

async function setupFleet(page: Page) {
  await mockSnapshotFeed(page);

  await page.route(/\/api\/fleet$/, async (route) => {
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({
        enabled: true,
        nodes: [
          fleetNode('alpha', 'alpha-host'),
          fleetNode('bravo', 'bravo-host'),
        ],
      }),
    });
  });

  // Patch fetch so alpha's body parses slowly and bravo's resolves at once.
  await page.addInitScript(
    ({ alphaSnap, bravoSnap, delayMs }) => {
      const realFetch = window.fetch.bind(window);
      const fake = (payload: unknown, delay: number) =>
        Promise.resolve({
          ok: true,
          status: 200,
          json: () =>
            new Promise((resolve) => {
              window.setTimeout(() => resolve(payload), delay);
            }),
        } as unknown as Response);

      window.fetch = ((input: RequestInfo | URL, init?: RequestInit) => {
        const url =
          typeof input === 'string'
            ? input
            : input instanceof URL
              ? input.href
              : input.url;
        // The peer endpoint returns a freshness envelope, not a bare snapshot.
        // reachable must be true here or the store would decline to apply
        // either payload -- correctly, since a stale reading must not be folded
        // into the live series -- and this test would pass for the wrong
        // reason, never having exercised the race it exists to catch.
        if (url.includes('/api/fleet/alpha/snapshot')) {
          return fake(
            { snapshot: alphaSnap, reachable: true, age_seconds: 0 },
            delayMs
          );
        }
        if (url.includes('/api/fleet/bravo/snapshot')) {
          return fake(
            { snapshot: bravoSnap, reachable: true, age_seconds: 0 },
            0
          );
        }
        return realFetch(input, init);
      }) as typeof window.fetch;
    },
    {
      alphaSnap: alphaSnapshot,
      bravoSnap: bravoSnapshot,
      delayMs: PARSE_DELAY_MS,
    }
  );
}

/** Clicks a node card on the fleet dashboard. */
async function selectNode(page: Page, name: string) {
  await openTab(page, 'Fleet');
  await page.getByRole('button', { name: new RegExp(name) }).first().click();
}

/**
 * Records every hostname the Host panel ever displays.
 *
 * Asserting the final value is not enough: the corruption is transient. The
 * selected peer's own next poll overwrites the stale snapshot a second later,
 * so a test that only looks at the end state sees the right answer and misses
 * the bug entirely. What matters is that the wrong hostname is never shown at
 * any instant, so every value the node takes is recorded and checked.
 */
async function recordHostnames(page: Page) {
  await page.evaluate(() => {
    const seen = new Set<string>();
    (window as unknown as { __seenHostnames: Set<string> }).__seenHostnames =
      seen;

    const sample = () => {
      const el = document.querySelector('main h2');
      const text = el?.textContent?.trim();
      if (text) seen.add(text);
    };

    sample();
    // Mutations catch the update the instant Svelte makes it; the sampler is a
    // backstop in case a value is written and replaced between mutations.
    new MutationObserver(sample).observe(document.body, {
      childList: true,
      subtree: true,
      characterData: true,
    });
    window.setInterval(sample, 50);
  });
}

async function seenHostnames(page: Page): Promise<string[]> {
  return page.evaluate(() => [
    ...(window as unknown as { __seenHostnames: Set<string> })
      .__seenHostnames,
  ]);
}

/**
 * Slows every poll to 60s so a peer's own refresh cannot mask a stale apply
 * during the measurement window.
 */
async function slowPolling(page: Page) {
  await page.getByLabel('Polling interval').selectOption('60000');
}

test.describe('Fleet peer selection race', () => {
  test('a peer response that resolves after a switch is never applied', async ({
    page,
  }) => {
    await setupFleet(page);
    await page.goto('/');
    await slowPolling(page);

    // Select alpha; its snapshot body will not parse for PARSE_DELAY_MS.
    await selectNode(page, 'alpha');

    // Switch to bravo well inside that window.
    await selectNode(page, 'bravo');

    // Bravo's data is what the panels must show.
    await openTab(page, 'Host');
    const hostname = page.locator('main h2').first();
    await expect(hostname).toHaveText('bravo-host');

    await recordHostnames(page);

    // Wait past alpha's parse delay. This is a deliberate fixed wait: the
    // event being waited for is the stale response landing, and the assertion
    // is that nothing happens when it does.
    await page.waitForTimeout(PARSE_DELAY_MS + 1500);

    const seen = await seenHostnames(page);
    expect(seen).toContain('bravo-host');
    expect(seen).not.toContain('alpha-host');
    await expect(hostname).toHaveText('bravo-host');
  });

  test('selecting a peer still shows that peer', async ({ page }) => {
    // Guards against "fixing" the race by dropping every peer response.
    await setupFleet(page);
    await page.goto('/');

    await selectNode(page, 'bravo');
    await openTab(page, 'Host');

    await expect(page.locator('main h2').first()).toHaveText('bravo-host');
  });
});
