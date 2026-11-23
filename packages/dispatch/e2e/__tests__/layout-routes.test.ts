import { describe, expect, test } from "bun:test";

import { layoutBreaches } from "../layout-breaches";
import { layoutRoutes, layoutUnread } from "../layout-routes";
import { reviewRoutes } from "../review-routes";

// e2e/layout.e2e.ts reads the pages e2e/layout-routes.ts lists against the layout rule. The review
// table already holds every route of the router (e2e/__tests__/review-routes.test.ts); this holds
// the layout list to that table, so a page the table reviews or excludes is read here or left out
// with a reason, never silently skipped.
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

  // An entry of e2e/layout-breaches.ts whose `route` names no route of the list is inert: no
  // reading can match it, and the stale check in e2e/layout.e2e.ts judges only the routes a worker
  // measured, so nothing reports it either. A typo or a renamed route would accept a breach for
  // ever, in silence.
  test("names a read route, and one of its states, in every breach list entry, once", () => {
    const read = new Map(layoutRoutes.map((route) => [route.name, route]));
    const orphans = layoutBreaches.filter((entry) => {
      const route = read.get(entry.route);
      if (route === undefined) return true;
      return (
        entry.state !== undefined && !route.states?.some((state) => state.name === entry.state)
      );
    });
    expect(orphans).toEqual([]);
    const keys = layoutBreaches.map(
      (entry) => `${entry.route}#${entry.state ?? ""} ${entry.width} ${entry.kind} ${entry.element}`
    );
    expect(keys.filter((key, index) => keys.indexOf(key) !== index)).toEqual([]);
  });
});
