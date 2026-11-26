import { expect, test } from "bun:test";

import {
  buildDispatchReference,
  buildIssuePath,
  type IssueRoute,
  type IssueTab,
  issueTabForRoute,
  parseDispatchReference,
  parseIssuePath,
  referenceRouteFromHref,
} from "./routes";

// Threads' addresses (dispatch://INFRA-5704/spec#b-r-addresses, R12): `/issues/<KEY>/threads` is
// the list, and `/issues/<KEY>/threads/<id>` a record of it. Both are pages of the new issue page
// alone, which no reader of text reads as a reference, and no tab of today's page.

/** A record's id as the server writes one. */
const RECORD = "0b6f3c1e-58a4-4d6e-9c41-7f2d8e1a9b30";

test("/issues/<KEY>/threads is the Threads route, and names no record", () => {
  expect(parseIssuePath("/issues/ACME-1/threads")).toEqual({ key: "ACME-1", kind: "threads" });
});

test("/issues/<KEY>/threads/<id> is the Threads route of that record, as the address writes it", () => {
  expect(parseIssuePath(`/issues/ACME-1/threads/${RECORD}`)).toEqual({
    key: "ACME-1",
    kind: "threads",
    record: RECORD,
  });
  // The id is the address's own, decoded; the view says whether it names a record (R12).
  expect(parseIssuePath("/issues/ACME-1/threads/not%20a%20uuid")).toEqual({
    key: "ACME-1",
    kind: "threads",
    record: "not a uuid",
  });
  for (const path of ["/issues/ACME-1/threads/", `/issues/ACME-1/threads/${RECORD}/more`]) {
    expect(parseIssuePath(path), `${path} names a route`).toBeUndefined();
  }
});

test("the Threads route builds its own path back, with and without a record", () => {
  for (const path of ["/issues/ACME-1/threads", `/issues/ACME-1/threads/${RECORD}`]) {
    const route = parseIssuePath(path);
    if (route === undefined) throw new Error(`${path} names no route`);
    expect(buildIssuePath(route)).toBe(path);
  }
  expect(buildIssuePath({ key: "ACME-1", kind: "threads", record: "a/b" })).toBe(
    "/issues/ACME-1/threads/a%2Fb"
  );
});

test("dispatch://<KEY>/threads and Threads' pages read in text as no reference", () => {
  // The server's `text` indexes neither form, so neither cites anything here, as Dashboard's
  // page cites nothing.
  for (const reference of ["dispatch://ACME-1/threads", `dispatch://ACME-1/threads/${RECORD}`]) {
    expect(parseDispatchReference(reference), reference).toBeUndefined();
  }
  for (const path of ["/issues/ACME-1/threads", `/issues/ACME-1/threads/${RECORD}`]) {
    expect(
      referenceRouteFromHref(`https://dispatch.test${path}`, "https://dispatch.test"),
      path
    ).toBeUndefined();
  }
});

test("the Threads route's own reference is the issue", () => {
  expect(buildDispatchReference({ key: "ACME-1", kind: "threads" })).toBe("dispatch://ACME-1");
  expect(buildDispatchReference({ key: "ACME-1", kind: "threads", record: RECORD })).toBe(
    "dispatch://ACME-1"
  );
});

// R18: each address today's page answers keeps the tab it opened, so the threads route changes
// nothing that today's page draws for them.
test("each address of today's page opens the tab it opened before the Threads route", () => {
  const tabs: readonly (readonly [string, IssueTab])[] = [
    ["/issues/ACME-1", "spec"],
    ["/issues/ACME-1/spec", "spec"],
    ["/issues/ACME-1/verification", "verification"],
    ["/issues/ACME-1/plan", "plan"],
    ["/issues/ACME-1/steps", "steps"],
    ["/issues/ACME-1/activity", "activity"],
    ["/issues/ACME-1/children", "children"],
    ["/issues/ACME-1/artifacts", "artifacts"],
    ["/issues/ACME-1/collaborators", "collaborators"],
    ["/issues/ACME-1/conversation", "conversation"],
    ["/issues/ACME-1/decisions", "decisions"],
    ["/issues/ACME-1/dashboard", "dashboard"],
    ["/issues/ACME-1/artifacts/notes-md", "artifacts"],
    ["/issues/ACME-1/asks/ask-1", "conversation"],
    ["/issues/ACME-1/comments/comment-1", "conversation"],
    ["/issues/ACME-1/messages/message-1", "conversation"],
  ];
  for (const [path, tab] of tabs) {
    const route: IssueRoute | undefined = parseIssuePath(path);
    if (route === undefined) throw new Error(`${path} names no route`);
    expect(issueTabForRoute(route, undefined), path).toBe(tab);
  }
});
