import { test, expect, type Page } from '@playwright/test';
import { mockSnapshotFeed } from './fixtures/mock-sse';
import { serveFleet } from './fixtures/fleet';
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
 * settles only when the test releases it, which is precisely the window the
 * old code was blind to -- held open for as long as the test needs rather
 * than for a duration it hopes is long enough.
 */

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
    cpu_available: true,
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

  await serveFleet(page, {
    nodes: [fleetNode('alpha', 'alpha-host'), fleetNode('bravo', 'bravo-host')],
  });

  // Patch fetch so alpha's body parses only once the test releases it, and
  // bravo's at once. Each alpha body is counted as handled in a task queued
  // after it resolves: the page's handling of it -- the selection check, the
  // apply, the render -- is all microtasks, so it has finished by then.
  await page.addInitScript(
    ({ alphaSnap, bravoSnap }) => {
      const alpha = { parsing: 0, handled: 0, release: () => {} };
      const released = new Promise<void>((resolve) => {
        alpha.release = resolve;
      });
      (window as unknown as { __alpha: typeof alpha }).__alpha = alpha;

      const realFetch = window.fetch.bind(window);
      const fake = (payload: unknown, parsed: Promise<void>, onHandled: () => void) =>
        Promise.resolve({
          ok: true,
          status: 200,
          json: () =>
            parsed.then(() => {
              const channel = new MessageChannel();
              channel.port1.onmessage = onHandled;
              channel.port2.postMessage(null);
              return payload;
            }),
        } as unknown as Response);

      window.fetch = ((input: RequestInfo | URL, init?: RequestInit) => {
        // Resolved against the page, as fetch does: the dashboard asks for
        // relative URLs ("api/fleet"), so the raw argument has no leading
        // slash to match on.
        const url = new URL(
          typeof input === 'string'
            ? input
            : input instanceof URL
              ? input.href
              : input.url,
          document.baseURI,
        ).href;
        // The peer endpoint returns a freshness envelope, not a bare snapshot.
        // reachable must be true here or the store would decline to apply
        // either payload -- correctly, since a stale reading must not be folded
        // into the live series -- and this test would pass for the wrong
        // reason, never having exercised the race it exists to catch.
        if (url.includes('/api/fleet/alpha/snapshot')) {
          alpha.parsing++;
          return fake(
            { snapshot: alphaSnap, reachable: true, age_seconds: 0 },
            released,
            () => alpha.handled++
          );
        }
        if (url.includes('/api/fleet/bravo/snapshot')) {
          return fake(
            { snapshot: bravoSnap, reachable: true, age_seconds: 0 },
            Promise.resolve(),
            () => {}
          );
        }
        return realFetch(input, init);
      }) as typeof window.fetch;
    },
    {
      alphaSnap: alphaSnapshot,
      bravoSnap: bravoSnapshot,
    }
  );
}

interface AlphaBodies {
  parsing: number;
  handled: number;
}

function alphaBodies(page: Page): Promise<AlphaBodies> {
  return page.evaluate(() => {
    const { parsing, handled } = (window as unknown as { __alpha: AlphaBodies }).__alpha;
    return { parsing, handled };
  });
}

/** Lets every held alpha body parse, and returns once the page has handled them all. */
async function releaseAlpha(page: Page) {
  await page.evaluate(() => (window as unknown as { __alpha: { release: () => void } }).__alpha.release());
  await expect
    .poll(async () => {
      const { parsing, handled } = await alphaBodies(page);
      return handled === parsing;
    }, { message: "alpha's response was never handled" })
    .toBe(true);
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
 *
 * A MutationObserver sees every one: its callback runs after each batch of DOM
 * changes, before the page can paint, so a value that is on screen for any
 * frame is recorded. (A value written and replaced within one batch is never
 * painted, and no sampler could see it either.)
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
    new MutationObserver(sample).observe(document.body, {
      childList: true,
      subtree: true,
      characterData: true,
    });
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

    // Select alpha; its snapshot body will not parse until released.
    await selectNode(page, 'alpha');
    await expect
      .poll(async () => (await alphaBodies(page)).parsing, { message: 'alpha was never polled' })
      .toBeGreaterThan(0);

    // Switch to bravo while alpha's body is still parsing.
    await selectNode(page, 'bravo');

    // Bravo's data is what the panels must show.
    await openTab(page, 'Host');
    const hostname = page.locator('main h2').first();
    await expect(hostname).toHaveText('bravo-host');

    await recordHostnames(page);

    // Let alpha's stale response land, and wait until the page has handled
    // it: the assertion is that nothing happens when it does.
    await releaseAlpha(page);

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
