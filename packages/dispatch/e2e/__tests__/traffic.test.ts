import { expect, test } from "bun:test";
import type { Page, Request } from "@playwright/test";

import { isLiveSocket, watchApiRequests } from "../traffic";

// Like e2e/__tests__/sends.test.ts, this file drives a helper of the suite through a stand-in for
// the part of a page the helper touches, so it starts no browser. Every row that drives a watcher
// reads `quietFor(0)`, which asks only whether a request is still in flight, so no row waits on
// the clock. Each such row reads every watcher before it asserts, so a failure names each watcher
// that failed it.

/** A page whose request events the row fires itself: the only part of a page the watchers touch. */
function eventPage(): { page: Page; emit(event: string, request: Request): void } {
  const handlers = new Map<string, ((request: Request) => void)[]>();
  const page = {
    on: (event: string, handler: (request: Request) => void) => {
      handlers.set(event, [...(handlers.get(event) ?? []), handler]);
    },
  } as unknown as Page;
  return {
    emit: (event, request) => {
      for (const handler of handlers.get(event) ?? []) handler(request);
    },
    page,
  };
}

/** A request as the watchers read it: its URL, its method, and that it loads no new document
 *  (`isNavigationRequest`), which `watchApiRequests` reads to settle a replaced document's reads. */
function request(url: string): Request {
  return {
    isNavigationRequest: () => false,
    method: () => "GET",
    url: () => url,
  } as unknown as Request;
}

/** The watchers this file drives, by name, as far as it reads them: whether the page has gone
 *  quiet. */
const watchers: readonly (readonly [string, (page: Page) => { quietFor(ms: number): boolean }])[] =
  [["watchApiRequests", watchApiRequests]];

/** The two events that end a request in flight: its answer, and its failure. */
const ends = ["requestfinished", "requestfailed"];

// Playwright can report the live socket's upgrade as a request that stays in flight for as long
// as the socket is open: in a CI trace a load's `GET /ws/live` (status 101) was open 29,998.5 ms
// though every read had answered within 0.5 s, so a watcher that held it in flight never let the
// page go quiet.
test("a live socket's upgrade that never finishes does not keep the page from going quiet", () => {
  const quiet = watchers.map(([watcher, watch]) => {
    const { emit, page } = eventPage();
    const traffic = watch(page);
    emit("request", request("http://127.0.0.1/ws/live"));
    const read = request("http://127.0.0.1/api/v1/issues/LAY-3");
    emit("request", read);
    emit("requestfinished", read);
    return { quiet: traffic.quietFor(0), watcher };
  });
  expect(quiet).toEqual(watchers.map(([watcher]) => ({ quiet: true, watcher })));
});

// The other direction, which also proves the stand-in's events reach each watcher: a read under
// `/api/` with no answer yet keeps the page from going quiet, and its end, answered or failed,
// lets it go quiet.
test("a read still in flight keeps the page from going quiet", () => {
  const quiet = watchers.flatMap(([watcher, watch]) =>
    ends.map((end) => {
      const { emit, page } = eventPage();
      const traffic = watch(page);
      const read = request("http://127.0.0.1/api/v1/issues/LAY-3");
      emit("request", read);
      const inFlight = traffic.quietFor(0);
      emit(end, read);
      return { end, inFlight, ended: traffic.quietFor(0), watcher };
    })
  );
  expect(quiet).toEqual(
    watchers.flatMap(([watcher]) =>
      ends.map((end) => ({ end, ended: true, inFlight: false, watcher }))
    )
  );
});

// `e2e/refetch-budget.e2e.ts` keeps a watcher of its own, which this file does not drive, and
// leaves the socket out through this same predicate, so the predicate is held here directly: a
// path under `/ws/` is the socket, and a path that holds `ws` anywhere else is a read like any
// other.
test("a path under /ws/ is the live socket, and no other path is", () => {
  const paths = ["/ws/live", "/api/v1/ws/x", "/wsx"];
  expect(
    paths.map((path) => ({ path, socket: isLiveSocket(new URL(path, "http://127.0.0.1")) }))
  ).toEqual([
    { path: "/ws/live", socket: true },
    { path: "/api/v1/ws/x", socket: false },
    { path: "/wsx", socket: false },
  ]);
});
