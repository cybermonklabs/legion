import { describe, expect, test } from "bun:test";

import { accessibilityExclusions } from "../accessibility-exclusions";
import { reviewRoutes, scannedRoutes, shotRoutes } from "../review-routes";
import { walkRoutes } from "../two-builds-routes";

// The review layers (an accessibility pass and a containerised visual review) open exactly the
// pages e2e/review-routes.ts lists, so a page app.tsx adds without a row there is a page both
// layers would miss unseen. e2e/__tests__/two-builds-routes.test.ts holds `walkRoutes` to the
// router itself; this holds the review table to `walkRoutes`. The other direction - that an
// entry's `pattern` names a router route - is the type of that field (`WalkRoutePath`) and so a
// compile error, which is why no row below asserts it.

const scanned = scannedRoutes.length;
/** Each shot route is shot twice - the two themes - which is the "about 40" R2 counts. */
const snapshots = shotRoutes.length * 2;

describe("the review layers' route list", () => {
  // `/issues/:key/*` is one router route that nine entries stand for, so a router route needs
  // one entry and not one each, which is why this counts the patterns covered rather than the
  // entries written.
  test("covers every route of the router, reviewed or excluded with a reason", () => {
    const covered = new Set<string>(reviewRoutes.map((route) => route.pattern));
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
  });

  // An entry of e2e/accessibility-exclusions.ts whose `route` names no scanned entry is inert:
  // the scan matches a finding on `rule route selector`, so nothing can ever match it, and the
  // stale check in e2e/accessibility.e2e.ts judges only the routes a worker really scanned, so
  // nothing reports it either. A typo or a renamed route would therefore accept a violation for
  // ever, in silence.
  test("names a scanned route in every accessibility exclusion", () => {
    const scannedNames = new Set(scannedRoutes.map((route) => route.name));
    const orphans = accessibilityExclusions
      .filter((exclusion) => !scannedNames.has(exclusion.route))
      .map(
        (exclusion) =>
          `${exclusion.rule} at ${exclusion.selector} names route ${exclusion.route}, which is ` +
          "no scanned entry of e2e/review-routes.ts"
      );
    expect(orphans).toEqual([]);
  });
});
