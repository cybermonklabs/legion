import { expect, test } from "bun:test";

import {
  buildDispatchReference,
  buildIssuePath,
  issueTabForRoute,
  parseDispatchReference,
  parseIssuePath,
  referenceRouteFromHref,
} from "../refs/routes";

// Dashboard's address on today's page (dispatch://INFRA-5702/spec#b-r-today-tab): its own path
// opens its tab, and the bare address still opens Spec.

test("/issues/<KEY>/dashboard is the Dashboard route, and the tab of its own name", () => {
  const route = parseIssuePath("/issues/ACME-1/dashboard");
  expect(route).toEqual({ key: "ACME-1", kind: "dashboard" });
  if (route === undefined) throw new Error("the Dashboard path names no route");
  expect(issueTabForRoute(route, undefined)).toBe("dashboard");
});

test("the Dashboard route builds its own path back", () => {
  expect(buildIssuePath({ key: "ACME-1", kind: "dashboard" })).toBe("/issues/ACME-1/dashboard");
});

test("the bare issue address still opens Spec", () => {
  const route = parseIssuePath("/issues/ACME-1");
  expect(route).toEqual({ key: "ACME-1", kind: "issue" });
  if (route === undefined) throw new Error("the bare address names no route");
  expect(issueTabForRoute(route, undefined)).toBe("spec");
});

test("dispatch://<KEY>/dashboard and the tab's page read in text as no reference", () => {
  // The tab is a page of the web alone: the server's `text` indexes neither form (`text.issueTabs`
  // holds no `dashboard`), so neither cites anything here.
  expect(parseDispatchReference("dispatch://ACME-1/dashboard")).toBeUndefined();
  expect(
    referenceRouteFromHref("https://dispatch.test/issues/ACME-1/dashboard", "https://dispatch.test")
  ).toBeUndefined();
});

test("the Dashboard route's own reference is the issue", () => {
  expect(buildDispatchReference({ key: "ACME-1", kind: "dashboard" })).toBe("dispatch://ACME-1");
});
