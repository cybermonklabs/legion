import { describe, expect, test } from "bun:test";

import { type KnownLayoutBreach, layoutBreaches } from "../layout-breaches";
import { LAYOUT_WIDTHS } from "../layout-measure";
import { layoutRoutes, layoutUnread } from "../layout-routes";
import { reviewRoutes } from "../review-routes";

/** The entries that name a place the layout rows never read: a route the list does not hold, a
 *  state that route does not have, or a width outside the five the rows read at. No reading can
 *  match such an entry, so nothing would ever report it either. */
function unreadPlaces(entries: readonly KnownLayoutBreach[]): KnownLayoutBreach[] {
  const read = new Map(layoutRoutes.map((route) => [route.name, route]));
  const widths: readonly number[] = LAYOUT_WIDTHS;
  return entries.filter((entry) => {
    const route = read.get(entry.route);
    if (route === undefined || !widths.includes(entry.width)) return true;
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

  test("names a read route, one of its states and one of the five widths in every breach list entry, once", () => {
    expect(unreadPlaces(layoutBreaches)).toEqual([]);
    const keys = layoutBreaches.map(
      (entry) => `${entry.route}#${entry.state ?? ""} ${entry.width} ${entry.kind} ${entry.element}`
    );
    expect(keys.filter((key, index) => keys.indexOf(key) !== index)).toEqual([]);
  });

  test("refuses an entry whose route, state or width the layout rows never read", () => {
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
      { ...read, width: 1024 as KnownLayoutBreach["width"] },
    ];
    expect(unreadPlaces(invented)).toEqual(invented);
  });
});
