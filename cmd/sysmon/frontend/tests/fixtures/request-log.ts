import { expect, type Page } from "@playwright/test";

/** One logged request: its URL, and whether the page has finished with it. */
export interface LoggedRequest {
  url: string;
  settled: boolean;
}

/**
 * Logs, in the page and as they are made, every fetch whose URL matches
 * pattern.
 *
 * A route handler sees a request only once it has crossed to the test process,
 * so "nothing was requested" read from there is only "nothing has arrived
 * yet". The page-side log is written synchronously by the fetch call itself.
 * Each entry is also marked settled -- in a task queued once its body has been
 * read, or once it failed or was aborted -- so everything the page did with the
 * answer, and any request that answer prompted, has already happened by then:
 * all of that is microtasks, and they run before any task.
 *
 * A MessageChannel rather than setTimeout for that task, because a spec that
 * installs page.clock has replaced setTimeout with timers it controls.
 *
 * One pattern per page: install it before the page loads.
 */
export async function logRequests(page: Page, pattern: RegExp) {
  await page.addInitScript(
    ({ source, flags }) => {
      const matches = new RegExp(source, flags);
      const log: Array<{ url: string; settled: boolean }> = [];
      (window as unknown as { __requestLog: typeof log }).__requestLog = log;

      const nextTask = (fn: () => void) => {
        const channel = new MessageChannel();
        channel.port1.onmessage = () => fn();
        channel.port2.postMessage(null);
      };
      const realFetch = window.fetch.bind(window);
      window.fetch = ((input: RequestInfo | URL, init?: RequestInit) => {
        // Resolved against the page, as fetch does: the dashboard asks for
        // relative URLs ("api/fleet"), so the raw argument has no leading
        // slash to match on.
        const url = new URL(
          typeof input === "string"
            ? input
            : input instanceof URL
              ? input.href
              : input.url,
          document.baseURI,
        ).href;
        const call = realFetch(input, init);
        if (!matches.test(url)) return call;

        const entry = { url, settled: false };
        log.push(entry);
        const settle = () => nextTask(() => (entry.settled = true));
        return call.then(
          (response) => {
            const json = response.json.bind(response);
            const text = response.text.bind(response);
            response.json = () => json().finally(settle);
            response.text = () => text().finally(settle);
            return response;
          },
          (error: unknown) => {
            settle();
            throw error;
          },
        );
      }) as typeof window.fetch;
    },
    { source: pattern.source, flags: pattern.flags },
  );
}

/** Every logged request so far, as the page sees them now. */
export async function loggedRequests(page: Page): Promise<LoggedRequest[]> {
  return page.evaluate(() =>
    (window as unknown as { __requestLog: LoggedRequest[] }).__requestLog.map(
      (e) => ({ ...e }),
    ),
  );
}

/**
 * The query parameters of every logged request once all of those matching
 * which have settled.
 *
 * Settled is the event a "was not requested" check has to wait for: a request
 * a response would prompt is made while that response is handled, so once
 * every logged request has settled nothing further can follow without another
 * change to the page.
 */
export async function settledRequests(
  page: Page,
  which: (url: URL) => boolean = () => true,
): Promise<URLSearchParams[]> {
  const parse = (e: LoggedRequest) => new URL(e.url, "http://page");
  await expect
    .poll(
      async () =>
        (await loggedRequests(page))
          .filter((e) => which(parse(e)))
          .every((e) => e.settled),
      {
        message: "a logged request never settled",
      },
    )
    .toBe(true);
  return (await loggedRequests(page)).map((e) => parse(e).searchParams);
}
