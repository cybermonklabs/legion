/** What `watchApiRequests` (e2e/traffic.ts) holds in flight when the page moves, read on a page
 *  the test drives by hand.
 *
 *  `expectLoadSettled` waits for `quietFor(1500)`, which is false while any request the page sent
 *  has neither finished nor failed. The row
 *  `issue-page-i.e2e.ts [iphone] the children are one group of rows of the outline @phone` timed
 *  out in that wait in merge runs 45607339493 and 45852270285: the page it was leaving sent a read
 *  within 1 ms of the navigation's document request, and Playwright reported neither that read's
 *  end nor its failure. The tracker now drops such reads when the main frame commits the new
 *  document, and only then: a move inside the page, or a navigation that failed, commits nothing.
 *
 *  Each row fires the events as Playwright would and moves the clock with `setSystemTime`, which
 *  holds `Date.now()` where it is set, so no row waits on the real clock.
 */

import { afterEach, expect, setSystemTime, test } from "bun:test";
import type { Page } from "@playwright/test";

import { watchApiRequests } from "../traffic";

/** The window `expectLoadSettled` waits for. */
const QUIET_MS = 1_500;

/** The tracker compares a frame only by identity, so a frame is any object. */
type StandInFrame = object;

/** What the tracker reads of a request. */
interface StandInRequest {
  url(): string;
  method(): string;
  isNavigationRequest(): boolean;
  frame(): StandInFrame;
}

type PageEvent = "request" | "requestfinished" | "requestfailed" | "framenavigated";
type Listener = (argument: StandInRequest | StandInFrame) => void;

/** A page whose events the test fires: it records each listener the tracker adds. */
function standInPage() {
  const listeners = new Map<PageEvent, Listener[]>();
  const mainFrame: StandInFrame = {};
  const page = {
    on(event: PageEvent, listener: Listener) {
      listeners.set(event, [...(listeners.get(event) ?? []), listener]);
    },
    mainFrame: () => mainFrame,
  };
  const request = (path: string, isNavigation: boolean): StandInRequest => ({
    url: () => `http://dispatch.test${path}`,
    method: () => "GET",
    isNavigationRequest: () => isNavigation,
    frame: () => mainFrame,
  });
  return {
    // The tracker reads no more of a page than `on` and `mainFrame`, and every request and frame
    // reaches it through a listener, so this is the one cast to Playwright's types.
    traffic: watchApiRequests(page as unknown as Page),
    mainFrame,
    fire: (event: PageEvent, argument: StandInRequest | StandInFrame) => {
      for (const listener of listeners.get(event) ?? []) listener(argument);
    },
    /** A read the page sends from its main frame. */
    read: (path: string) => request(path, false),
    /** The main frame's request for its next document. */
    navigation: (path: string) => request(path, true),
  };
}

const START = Date.parse("2026-11-26T10:00:00Z");

/** Holds the clock `ms` after the row's start. */
const at = (ms: number) => setSystemTime(START + ms);

// A clock left held would hold `Date.now()` for every file the same worker runs after this one.
afterEach(() => {
  setSystemTime();
});

test("a read the left document sent at a navigation, never ended, holds it only to the commit", () => {
  at(0);
  const { traffic, mainFrame, fire, read, navigation } = standInPage();
  fire("request", read("/api/v1/issues/LAY-3/children"));
  const next = navigation("/issues/LAY-4");
  fire("request", next);
  // The document request ends before the commit, so a quiet counted from its end would read
  // true less than 1,500 ms after the commit.
  at(50);
  fire("requestfinished", next);
  at(100);
  fire("framenavigated", mainFrame);

  at(100 + QUIET_MS - 1);
  expect(traffic.quietFor(QUIET_MS)).toBe(false);
  at(100 + QUIET_MS);
  expect(traffic.quietFor(QUIET_MS)).toBe(true);
});

test("a move inside the page drops nothing: its read holds the page until it finishes", () => {
  at(0);
  const { traffic, mainFrame, fire, read } = standInPage();
  const children = read("/api/v1/issues/LAY-3/children");
  fire("request", children);
  // `history.pushState` moves the main frame without a navigation request.
  at(100);
  fire("framenavigated", mainFrame);

  at(100 + QUIET_MS);
  expect(traffic.quietFor(QUIET_MS)).toBe(false);
  at(600_000);
  expect(traffic.quietFor(QUIET_MS)).toBe(false);
  fire("requestfinished", children);
  at(600_000 + QUIET_MS - 1);
  expect(traffic.quietFor(QUIET_MS)).toBe(false);
  at(600_000 + QUIET_MS);
  expect(traffic.quietFor(QUIET_MS)).toBe(true);
});

test("a navigation request that fails leaves no commit for a later move inside the page", () => {
  at(0);
  const { traffic, mainFrame, fire, read, navigation } = standInPage();
  const refused = navigation("/issues/LAY-4");
  fire("request", refused);
  const children = read("/api/v1/issues/LAY-3/children");
  fire("request", children);
  at(50);
  fire("requestfailed", refused);
  // A move inside the page, with no document request after the failed one.
  at(100);
  fire("framenavigated", mainFrame);

  at(600_000);
  expect(traffic.quietFor(QUIET_MS)).toBe(false);
  // The read, not the failed document request, is what held the page.
  fire("requestfinished", children);
  at(600_000 + QUIET_MS);
  expect(traffic.quietFor(QUIET_MS)).toBe(true);
});
