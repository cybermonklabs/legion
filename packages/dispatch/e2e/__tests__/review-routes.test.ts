import { describe, expect, test } from "bun:test";

import { phoneShotNames, reviewRoutes, scannedRoutes, shotRoutes } from "../review-routes";
import { walkRoutes } from "../two-builds-routes";

// The review layers (an accessibility pass and a containerised visual review) open exactly the
// pages e2e/review-routes.ts lists, so a page app.tsx adds without a row there is a page both
// layers would miss unseen. e2e/__tests__/two-builds-routes.test.ts holds `walkRoutes` to the
// router itself; this holds the review table to `walkRoutes`.

/** `/issues/:key/*` is one router route that nine entries stand for, so an entry is matched to
 *  the router by its `pattern` string and a router route needs one entry, not one each. */
const routerPaths = new Set<string>(walkRoutes.map((route) => route.path));

const scanned = scannedRoutes.length;
/** Each shot route is shot twice - the two themes - which is the "about 40" R2 counts. */
const snapshots = shotRoutes.length * 2;

describe("the review layers' route list", () => {
  test("names a route of the router in every entry", () => {
    const unknown = reviewRoutes
      .filter((route) => !routerPaths.has(route.pattern))
      .map((route) => `${route.name} at ${route.pattern}`);
    expect(unknown).toEqual([]);
  });

  test("covers every route of the router, reviewed or excluded with a reason", () => {
    const covered = new Set(reviewRoutes.map((route) => route.pattern));
    expect(
      walkRoutes.filter((route) => !covered.has(route.path)).map((route) => route.path)
    ).toEqual([]);
    const unexplained = reviewRoutes.filter(
      (route) => route.kind === "excluded" && route.reason.trim() === ""
    );
    expect(unexplained.map((route) => route.name)).toEqual([]);
  });

  test("scans at least 15 pages and shoots about 40 snapshots", () => {
    expect(scanned).toBeGreaterThanOrEqual(15);
    expect(snapshots).toBeGreaterThanOrEqual(36);
    expect(snapshots).toBeLessThanOrEqual(48);
  });

  test("gives every reviewed entry a layer, and every entry its own name", () => {
    const layerless = reviewRoutes.filter(
      (route) => route.kind === "reviewed" && route.layers.length === 0
    );
    expect(layerless.map((route) => route.name)).toEqual([]);

    const names = reviewRoutes.map((route) => route.name);
    expect(names.filter((name, index) => names.indexOf(name) !== index)).toEqual([]);

    const pairs = reviewRoutes.map((route) => `${route.pattern} ${route.name}`);
    expect(pairs.filter((pair, index) => pairs.indexOf(pair) !== index)).toEqual([]);
  });

  test("names only shot entries in the phone list", () => {
    const shot = new Set(shotRoutes.map((route) => route.name));
    expect(phoneShotNames.filter((name) => !shot.has(name))).toEqual([]);
  });
});
