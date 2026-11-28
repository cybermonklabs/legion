import { expect, test } from "bun:test";
import { IDEMPOTENCY_KEY_COMMITTED, IDEMPOTENCY_KEY_IN_FLIGHT } from "@legion/contracts";
import type { Page, Route } from "@playwright/test";

import { MESSAGE_REFUSED, refusePosts, WRITE_REFUSED } from "../sends";

/** A page whose routes the test reads back: the only part of a page `refusePosts` touches. */
function routedPage(): {
  page: Page;
  patterns: string[];
  handlers: ((route: Route) => Promise<void>)[];
} {
  const patterns: string[] = [];
  const handlers: ((route: Route) => Promise<void>)[] = [];
  const page = {
    route: async (pattern: string, handler: (route: Route) => Promise<void>) => {
      patterns.push(pattern);
      handlers.push(handler);
    },
  } as unknown as Page;
  return { page, patterns, handlers };
}

// Every write the page makes tries an outage (a 5xx, a 408 or a 429), a `409 KEY_IN_FLIGHT` or a
// lost answer again under its key (`retriesUnderKey`), so a row that refused any route with one
// would wait on it for good. The helper refuses the row instead, naming the refusal that does end
// a write: Dispatch's own verdict.
test("refusePosts refuses an outage or a 409 KEY_IN_FLIGHT for every write route, naming WRITE_REFUSED", async () => {
  for (const pattern of [
    "**/api/v1/agents/*/messages",
    "**/api/v1/agents/01a0e0c1-0000-7000-8000-00000000a002/messages",
    "**/api/v1/issues/CORE-1/comments",
    "**/api/v1/issues/*/asks",
  ]) {
    for (const refusal of [
      ...[408, 429, 500, 501, 503].map((status) => ({
        code: "UNAVAILABLE",
        error: "down",
        status,
      })),
      {
        code: IDEMPOTENCY_KEY_IN_FLIGHT,
        error: "an earlier request is still running",
        status: 409,
      },
    ]) {
      const { page, patterns } = routedPage();
      await expect(refusePosts(page, pattern, refusal)).rejects.toThrow(/WRITE_REFUSED/);
      expect(patterns).toEqual([]);
    }
  }
});

// A 409 under any other code is Dispatch's verdict on the key, which the page does not try again.
test("refusePosts routes a 409 that is not KEY_IN_FLIGHT", async () => {
  const { page, patterns } = routedPage();
  await refusePosts(page, "**/api/v1/issues/*/comments", {
    code: IDEMPOTENCY_KEY_COMMITTED,
    error: "the write already landed",
    status: 409,
  });
  expect(patterns).toEqual(["**/api/v1/issues/*/comments"]);
});

test("refusePosts routes Dispatch's verdict, a 400 unless a row names its own", async () => {
  const { page, patterns, handlers } = routedPage();
  await refusePosts(page, "**/api/v1/agents/*/messages", MESSAGE_REFUSED);
  const refuse = await refusePosts(page, "**/api/v1/issues/CORE-1/comments");
  await refusePosts(page, "**/api/v1/issues/*/messages");
  expect(patterns).toEqual([
    "**/api/v1/agents/*/messages",
    "**/api/v1/issues/CORE-1/comments",
    "**/api/v1/issues/*/messages",
  ]);

  const fulfilled: { status?: number; body?: string }[] = [];
  const route = {
    fulfill: async (answer: { status?: number; body?: string }) => {
      fulfilled.push(answer);
    },
    request: () => ({ method: () => "POST" }),
  } as unknown as Route;
  refuse();
  await handlers[1]?.(route);
  expect(WRITE_REFUSED.status).toBe(400);
  expect(fulfilled).toMatchObject([
    {
      body: JSON.stringify({ code: WRITE_REFUSED.code, error: WRITE_REFUSED.error }),
      status: 400,
    },
  ]);
});
