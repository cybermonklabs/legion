import { expect, test } from "bun:test";

import {
  buildDispatchReference,
  buildIssuePath,
  issueTabForRoute,
  parseDispatchReference,
  parseIssuePath,
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

test("dispatch://<KEY>/dashboard is a reference to the tab, which names the issue itself", () => {
  const route = parseDispatchReference("dispatch://ACME-1/dashboard");
  expect(route).toEqual({ key: "ACME-1", kind: "dashboard" });
  if (route === undefined) throw new Error("the Dashboard reference names nothing");
  // A tab of the page is the issue, as the server's graph stores it (`text.issueTabs`).
  expect(buildDispatchReference(route)).toBe("dispatch://ACME-1");
});
