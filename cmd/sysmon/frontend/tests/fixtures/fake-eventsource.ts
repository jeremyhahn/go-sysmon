import type { Page } from "@playwright/test";

/**
 * Replaces EventSource with one the test drives.
 *
 * The frames matter here, not the socket: what is under test is how the
 * transport reacts to a sequence, and Playwright's route interception cannot
 * hold a response open and push to it. The Go side covers the real socket
 * end to end.
 */
export async function installFakeEventSource(page: Page) {
  await page.addInitScript(() => {
    class FakeEventSource extends EventTarget {
      static readonly CONNECTING = 0;
      static readonly OPEN = 1;
      static readonly CLOSED = 2;
      static instances: FakeEventSource[] = [];

      readonly url: string;
      readyState = FakeEventSource.OPEN;
      /** Whether onopen has been delivered, so a test can wait for it. */
      opened = false;
      onopen: ((e: Event) => void) | null = null;
      onerror: ((e: Event) => void) | null = null;

      constructor(url: string) {
        super();
        this.url = url;
        FakeEventSource.instances.push(this);
        // Asynchronous, as the real one is: the caller has to have attached
        // its listeners before anything arrives.
        setTimeout(() => {
          this.onopen?.(new Event('open'));
          this.opened = true;
        }, 0);
      }

      close() {
        this.readyState = FakeEventSource.CLOSED;
      }
    }
    const w = window as unknown as { EventSource: unknown; __es: typeof FakeEventSource };
    w.EventSource = FakeEventSource;
    w.__es = FakeEventSource;
  });
}

interface FakeES {
  instances: Array<{ url: string; dispatchEvent(e: Event): boolean }>;
}

/** Pushes one frame onto the stream the page most recently opened. */
export async function send(page: Page, event: string, data: unknown) {
  await page.evaluate(
    ([name, payload]) => {
      const es = (window as unknown as { __es: FakeES }).__es.instances;
      const latest = es[es.length - 1];
      latest.dispatchEvent(new MessageEvent(name as string, { data: JSON.stringify(payload) }));
    },
    [event, data] as const
  );
}

/** How many times the page has opened a stream. */
export function connections(page: Page): Promise<number> {
  return page.evaluate(
    () => (window as unknown as { __es: FakeES }).__es.instances.length
  );
}

/** Whether the most recently opened stream has delivered its open event. */
export function latestOpened(page: Page): Promise<boolean> {
  return page.evaluate(() => {
    const es = (window as unknown as { __es: { instances: Array<{ opened: boolean }> } }).__es
      .instances;
    return es[es.length - 1]?.opened ?? false;
  });
}

/** The URL of the most recently opened stream. */
export function lastURL(page: Page): Promise<string> {
  return page.evaluate(() => {
    const es = (window as unknown as { __es: FakeES }).__es.instances;
    return es[es.length - 1]?.url ?? '';
  });
}
