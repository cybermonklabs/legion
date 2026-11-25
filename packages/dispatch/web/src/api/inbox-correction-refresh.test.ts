import { expect, test } from "bun:test";

import { eventRefreshTargets } from "./event-refresh";
import { meInboxKey } from "./queries";
import type { DispatchEvent } from "./types";

// R2 and R3 of dispatch://INFRA-1523: every page that prints an edited post marks it, and every
// page that prints a deleted post says so and gives no part of its body. The Inbox prints each
// thread's root (`thread.subject`, ThreadTitle and ThreadPane) from the viewer's `["me",
// "inbox"]` rows, the one read's, so an edit or a delete of a post in a thread of the viewer's has
// to refetch those rows, as a new post in that thread does. The tester found an open Inbox still
// printing a root its author had deleted, with no deleted line, until a reload (test round at
// 5bd5c11d).

/** An event of `type` on a comment or a message of CORE-1 whose thread alice is in. */
function threadEvent(type: string): DispatchEvent {
  const event = {
    actor: { id: "bob", kind: "user" },
    created_at: "2026-11-18T00:00:00Z",
    id: 1,
    issue_key: "CORE-1",
    notify: true,
    payload: {
      id: "11111111-2222-4333-8444-555555555555",
      issue_key: "CORE-1",
      thread_logins: ["alice", "bob"],
    },
    project: "CORE",
    seq: 1,
    type,
  };
  // The feed's event union is wider than this test needs; this is the shape its reader reads.
  return event as unknown as DispatchEvent;
}

function refetchesInbox(type: string): boolean {
  return eventRefreshTargets(threadEvent(type), "alice").some(
    (target) => JSON.stringify(target.queryKey) === JSON.stringify(meInboxKey)
  );
}

// The positive control: a new post in the thread refetches the rows, so the rows below fail on
// the correction events alone.
test("a new post in a thread of the viewer's refetches the Inbox rows", () => {
  expect(refetchesInbox("comment.created")).toBe(true);
  expect(refetchesInbox("message.created")).toBe(true);
});

for (const type of ["comment.edited", "comment.deleted", "message.edited", "message.deleted"]) {
  test(`${type} in a thread of the viewer's refetches the Inbox rows`, () => {
    expect(refetchesInbox(type)).toBe(true);
  });
}
