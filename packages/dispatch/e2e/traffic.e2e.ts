/** `watchApiRequests` (e2e/traffic.ts) counts a request in flight until the frame that sent it
 *  commits a new document. Chromium reports no end to a write its document let finish as it went
 *  (`keepalive`) once that document is gone, so a load after such a write never went quiet under
 *  the rule that waited for `requestfinished` or `requestfailed` alone (CI run 45987603643,
 *  `issue-page-g.e2e.ts:120`, part L's read-mark `PUT`). The rows need a real network and no
 *  Dispatch: their own server holds the write, answers a read late and answers a navigation 204,
 *  which the stub layer, answering each request from a recording through `page.route`, cannot
 *  do. A load goes quiet only once the old document's write stops counting and the live
 *  document's read is answered. */

import { createServer } from "node:http";
import type { AddressInfo, Socket } from "node:net";
import { expect, test } from "@playwright/test";

import { expectLoadSettled, LOAD_QUIET_MS, watchApiRequests } from "./traffic";

/** The write the document the row leaves sends: a small `PUT`, which `send` in
 *  `web/src/api/client.ts` lets finish as the page goes (`keepalive`), here to a route that never
 *  answers. */
const HELD_WRITE = "/api/v1/me/issues/QUIET-1/state";

/** The read the document the row opens sends as it loads, which answers after `READ_MS`. */
const SLOW_READ = "/api/v1/issues/QUIET-1";

/** Longer than the quiet window `expectLoadSettled` waits out, so a rule that leaves the new
 *  document's read out of the count goes quiet before the read answers, and the row fails. */
const READ_MS = 2 * LOAD_QUIET_MS;

const LEAVING = `<!doctype html><title>Leaving</title><script>
fetch(${JSON.stringify(HELD_WRITE)}, { body: "{}", keepalive: true, method: "PUT" });
</script>`;

const ARRIVING = `<!doctype html><title>Arriving</title><script>
fetch(${JSON.stringify(SLOW_READ)});
</script>`;

/** A document that sends nothing, and the address a navigation from it is answered 204 at, so no
 *  new document is committed. */
const STAYING = `<!doctype html><title>Staying</title>`;
const NO_CONTENT = "/no-content";

/** The row's own server: it holds `HELD_WRITE` unanswered, answers `SLOW_READ` after `READ_MS`,
 *  answers `NO_CONTENT` with 204, and serves the three documents. */
async function serveQuietPages(): Promise<{
  readonly close: () => Promise<void>;
  readonly origin: string;
  readonly readAnsweredAt: () => number | undefined;
  readonly writesHeld: () => number;
}> {
  const sockets = new Set<Socket>();
  let writesHeld = 0;
  let readAnsweredAt: number | undefined;
  const server = createServer((request, response) => {
    if (request.url === HELD_WRITE) {
      writesHeld += 1;
      return;
    }
    if (request.url === SLOW_READ) {
      // The read is answered late on purpose: the row reads whether the quiet window waits for it.
      setTimeout(() => {
        readAnsweredAt = Date.now();
        response.end("{}");
      }, READ_MS);
      return;
    }
    if (request.url === NO_CONTENT) {
      response.statusCode = 204;
      response.end();
      return;
    }
    response.setHeader("content-type", "text/html");
    response.end(
      request.url === "/leaving" ? LEAVING : request.url === "/staying" ? STAYING : ARRIVING
    );
  });
  server.on("connection", (socket) => sockets.add(socket));
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  return {
    close: async () => {
      for (const socket of sockets) socket.destroy();
      await new Promise<void>((resolve) => server.close(() => resolve()));
    },
    origin: `http://127.0.0.1:${(server.address() as AddressInfo).port}`,
    readAnsweredAt: () => readAnsweredAt,
    writesHeld: () => writesHeld,
  };
}

// SLOW(>=2s): two loads from the row's own server, a read held twice the quiet window, and the
// quiet window after it.
test("a write its document let finish as it went holds no later load open, and the new document's read does", async ({
  page,
}) => {
  const served = await serveQuietPages();
  try {
    const traffic = watchApiRequests(page);
    await page.goto(`${served.origin}/leaving`);
    await expect
      .poll(() => served.writesHeld(), { message: "the leaving document sent no write" })
      .toBe(1);
    await page.goto(`${served.origin}/arriving`);
    await expectLoadSettled(traffic);
    expect(
      served.readAnsweredAt(),
      "the load went quiet before the new document's read was answered"
    ).toBeDefined();
    expect(traffic.requests.map((sent) => `${sent.method} ${sent.path}`)).toEqual([
      `PUT ${HELD_WRITE}`,
      `GET ${SLOW_READ}`,
    ]);
  } finally {
    await served.close();
  }
});

// A navigation answered 204 commits no document, and Playwright reports a `pushState` as a
// `framenavigated` too: a rule that kept the 204 navigation as the frame's would read the
// `pushState` to its URL as that navigation's commit and stop counting the live document's read.
// SLOW(>=2s): one load from the row's own server, a read held twice the quiet window, and the
// quiet window after it.
test("a navigation that commits nothing leaves a later pushState to its URL ending no count", async ({
  page,
}) => {
  const served = await serveQuietPages();
  try {
    const traffic = watchApiRequests(page);
    await page.goto(`${served.origin}/staying`);
    await page.evaluate(() => {
      document.documentElement.dataset.staying = "yes";
    });
    const noContent = (request: { url(): string }) => request.url().endsWith(NO_CONTENT);
    const ended = Promise.race([
      page.waitForEvent("requestfinished", noContent),
      page.waitForEvent("requestfailed", noContent),
    ]);
    await page.evaluate((to) => {
      window.location.href = to;
    }, NO_CONTENT);
    await ended;
    expect(page.url(), "a 204 changed the frame's URL").toBe(`${served.origin}/staying`);
    expect(
      await page.evaluate(() => document.documentElement.dataset.staying),
      "a 204 replaced the document"
    ).toBe("yes");
    const readSent = page.waitForEvent("request", (request) => request.url().endsWith(SLOW_READ));
    await page.evaluate((read) => {
      void fetch(read);
    }, SLOW_READ);
    await readSent;
    await page.evaluate((to) => {
      history.pushState(null, "", to);
    }, NO_CONTENT);
    await expectLoadSettled(traffic);
    expect(
      served.readAnsweredAt(),
      "the pushState ended the count of the live document's read"
    ).toBeDefined();
  } finally {
    await served.close();
  }
});
