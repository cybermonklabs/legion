/** `watchApiRequests` (e2e/traffic.ts) counts a request in flight until the document that sent it
 *  is gone. Chromium reports no end to a write its document let finish as it went (`keepalive`)
 *  once that document is gone, so a load after such a write never went quiet under the rule that
 *  waited for `requestfinished` or `requestfailed` alone (CI run 45987603643,
 *  `issue-page-g.e2e.ts:120`, part L's read-mark `PUT`). The row needs a real network and no
 *  Dispatch: its own server holds the write and answers the new document's read late, which the
 *  stub layer, answering each request from a recording through `page.route`, cannot do. The load
 *  goes quiet only once the old document's write stops counting and the new document's read is
 *  answered. */

import { createServer } from "node:http";
import type { AddressInfo, Socket } from "node:net";
import { expect, test } from "@playwright/test";

import { expectLoadSettled, watchApiRequests } from "./traffic";

/** The write the document the row leaves sends, as `web/src/api/client.ts` sends every `PUT`: one
 *  the page lets finish as it goes (`keepalive`), here to a route that never answers. */
const HELD_WRITE = "/api/v1/me/issues/QUIET-1/state";

/** The read the document the row opens sends as it loads, which answers after `READ_MS`. */
const SLOW_READ = "/api/v1/issues/QUIET-1";

/** Longer than the quiet window `expectLoadSettled` waits out (1.5 s), so a load counted done
 *  before the new document's read answers is counted done early. */
const READ_MS = 3_000;

const LEAVING = `<!doctype html><title>Leaving</title><script>
fetch(${JSON.stringify(HELD_WRITE)}, { body: "{}", keepalive: true, method: "PUT" });
</script>`;

const ARRIVING = `<!doctype html><title>Arriving</title><script>
fetch(${JSON.stringify(SLOW_READ)});
</script>`;

// SLOW(>=2s): two loads from the row's own server and the 1.5 s quiet window after a 3 s read.
test("a write its document let finish as it went holds no later load open, and the new document's read does", async ({
  page,
}) => {
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
    response.setHeader("content-type", "text/html");
    response.end(request.url === "/leaving" ? LEAVING : ARRIVING);
  });
  server.on("connection", (socket) => sockets.add(socket));
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const origin = `http://127.0.0.1:${(server.address() as AddressInfo).port}`;
  try {
    const traffic = watchApiRequests(page);
    await page.goto(`${origin}/leaving`);
    await expect.poll(() => writesHeld, { message: "the leaving document sent no write" }).toBe(1);
    await page.goto(`${origin}/arriving`);
    await expectLoadSettled(traffic);
    expect(
      readAnsweredAt,
      "the load went quiet before the new document's read was answered"
    ).toBeDefined();
    expect(traffic.requests.map((sent) => `${sent.method} ${sent.path}`)).toEqual([
      `PUT ${HELD_WRITE}`,
      `GET ${SLOW_READ}`,
    ]);
  } finally {
    for (const socket of sockets) socket.destroy();
    await new Promise<void>((resolve) => server.close(() => resolve()));
  }
});
