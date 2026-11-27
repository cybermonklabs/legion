/** What `watchApiRequests` holds in flight across a navigation.
 *
 *  A request of the document a `page.goto` tears down never fires `requestfinished` or
 *  `requestfailed`. Before the watcher dropped such requests at the next main-frame document,
 *  issue-page-q.e2e.ts's "an address the view draws no row for draws the outline's line for it in
 *  the view's place", which loads eleven addresses under one watcher, waited out its 30 s quiet
 *  wait about half its runs (INFRA-1109). The rows drive the watcher with a stand-in page whose
 *  events they fire themselves, so no row opens a browser. */
import { expect, test } from "bun:test";
import { EventEmitter } from "node:events";
import type { Frame, Page, Request } from "@playwright/test";

import { watchApiRequests } from "../traffic";

const ORIGIN = "http://127.0.0.1:5173";

/** A page with one main frame: the only parts of a page `watchApiRequests` touches are its
 *  events and `mainFrame()`. */
class StandInPage extends EventEmitter {
  readonly main = {} as Frame;

  mainFrame(): Frame {
    return this.main;
  }

  watch() {
    return watchApiRequests(this as unknown as Page);
  }
}

/** A request of `path` from the frame `frame` returns, as much of one as the watcher reads. */
function request(path: string, frame: () => Frame, navigation = false): Request {
  return {
    frame,
    isNavigationRequest: () => navigation,
    method: () => "GET",
    url: () => `${ORIGIN}${path}`,
  } as unknown as Request;
}

test("a main-frame navigation drops what the document it replaces left in flight, and counts the new document's requests", () => {
  const page = new StandInPage();
  const traffic = page.watch();

  const stranded = request("/api/v1/issues/CORE-1", () => page.main);
  page.emit("request", stranded);
  expect(traffic.quietFor(0), "a read in flight leaves the page quiet").toBe(false);

  // The next address's document: its own request is in flight until it finishes, and the read the
  // old document left never finishes.
  const document = request("/issues/CORE-1?comment=0190e7a1", () => page.main, true);
  page.emit("request", document);
  expect(traffic.quietFor(0), "the new document's own request was dropped").toBe(false);
  page.emit("requestfinished", document);
  expect(traffic.quietFor(0), "the old document's read still keeps the page busy").toBe(true);

  const read = request("/api/v1/issues/CORE-1/comments", () => page.main);
  page.emit("request", read);
  expect(traffic.quietFor(0), "a read the new document sent was not counted in flight").toBe(false);
  expect(traffic.requests.map((sent) => sent.path)).toEqual([
    "/api/v1/issues/CORE-1",
    "/api/v1/issues/CORE-1/comments",
  ]);
  page.emit("requestfinished", read);
  expect(traffic.quietFor(0)).toBe(true);
});

test("a sub-frame's navigation, a frame not yet created and a request that is no navigation drop nothing", () => {
  const page = new StandInPage();
  const traffic = page.watch();
  const pending = request("/api/v1/issues/CORE-1", () => page.main);
  page.emit("request", pending);

  const subFrame = {} as Frame;
  for (const other of [
    request("/embedded", () => subFrame, true),
    // Playwright throws when a navigation request's frame does not exist yet.
    request(
      "/new-frame",
      () => {
        throw new Error("Frame for this navigation request is not available");
      },
      true
    ),
    request("/assets/index.js", () => page.main),
  ]) {
    page.emit("request", other);
    page.emit("requestfinished", other);
  }
  expect(traffic.quietFor(0), "a request that loads no main-frame document dropped one").toBe(
    false
  );

  page.emit("requestfinished", pending);
  expect(traffic.quietFor(0)).toBe(true);
});
