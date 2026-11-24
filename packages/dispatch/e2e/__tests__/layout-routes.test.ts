import { describe, expect, test } from "bun:test";

import { type KnownLayoutBreach, layoutBreaches } from "../layout-breaches";
import { layoutRoutes, layoutRowTimeoutMs, layoutUnread } from "../layout-routes";
import { reviewRoutes } from "../review-routes";

/** The entries that name a place the layout rows never read: a route the list does not hold, or
 *  a state that route does not have. No reading can match such an entry, so nothing would ever
 *  report it either. Its width is the type's to bound (`LayoutWidth`). */
function unreadPlaces(entries: readonly KnownLayoutBreach[]): KnownLayoutBreach[] {
  const read = new Map(layoutRoutes.map((route) => [route.name, route]));
  return entries.filter((entry) => {
    const route = read.get(entry.route);
    if (route === undefined) return true;
    return entry.state !== undefined && !route.states?.some((state) => state.name === entry.state);
  });
}

describe("the layout layer's route list", () => {
  test("reads every reviewed page, and every excluded one it does not leave out with a reason", () => {
    const read = new Set(layoutRoutes.map((route) => route.name));
    const missing = reviewRoutes
      .filter((route) => !read.has(route.name) && layoutUnread[route.name] === undefined)
      .map((route) => `${route.name} (${route.kind})`);
    expect(missing).toEqual([]);
    const reviewed = reviewRoutes.filter((route) => route.kind === "reviewed");
    expect(reviewed.filter((route) => layoutUnread[route.name] !== undefined)).toEqual([]);
    const leftOut = Object.entries(layoutUnread).filter(
      ([name, reason]) => read.has(name) || reason.trim() === ""
    );
    expect(leftOut).toEqual([]);
  });

  test("gives every route its own name", () => {
    const names = layoutRoutes.map((route) => route.name);
    expect(names.filter((name, index) => names.indexOf(name) !== index)).toEqual([]);
  });

  test("names a read route, and one of its states, in every breach list entry, once", () => {
    expect(unreadPlaces(layoutBreaches)).toEqual([]);
    const keys = layoutBreaches.map(
      (entry) => `${entry.route}#${entry.state ?? ""} ${entry.width} ${entry.kind} ${entry.element}`
    );
    expect(keys.filter((key, index) => keys.indexOf(key) !== index)).toEqual([]);
  });

  test("refuses an entry whose route or state the layout rows never read", () => {
    const read: KnownLayoutBreach = {
      element: '[data-testid="issue-metadata-rail"]',
      kind: "box-scroll",
      reason: "B issue page",
      route: "issue-spec",
      width: 360,
    };
    expect(unreadPlaces([read])).toEqual([]);
    const invented: KnownLayoutBreach[] = [
      { ...read, route: "issue-specs" },
      { ...read, route: "agents", state: "peek-drawers" },
    ];
    expect(unreadPlaces(invented)).toEqual(invented);
  });

  test("gives a route row a budget that follows its readings, 6 s each, and never under 60 s", () => {
    const state = { act: async () => undefined, name: "a state" };
    // Five widths and three states: 20 readings, 120 s.
    expect(layoutRowTimeoutMs({ states: [state, state, state] })).toBe(120_000);
    // Five widths and no state: 5 readings, which keep Playwright's 60 s.
    expect(layoutRowTimeoutMs({})).toBe(60_000);
  });
});
