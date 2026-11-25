import { expect, test } from "bun:test";

import { eventRefreshTargets } from "./event-refresh";
import type { DispatchEvent } from "./types";

// R2 and R3 of dispatch://INFRA-1523: a person's page (`["people", <login>, "nodes"]`, main's
// #3178) prints the first line of each comment's words beside its marks. The cached correction
// writes the mark at once but not the words, so an edit or a delete of a comment must read every
// open person's page again, or an edited comment keeps its old first line until the page's own
// refetch.

/** A comment event of type `type`, on an issue or on a project document. */
function commentEvent(type: string, onDocument: boolean): DispatchEvent {
  const event = {
    actor: { id: "alice", kind: "user" },
    artifact_id: onDocument ? "33333333-4444-4555-8666-777777777777" : null,
    created_at: "2026-11-23T00:00:00Z",
    id: 1,
    issue_key: onDocument ? null : "CORE-1",
    notify: true,
    payload: {
      id: "11111111-2222-4333-8444-555555555555",
      issue_key: onDocument ? null : "CORE-1",
    },
    project: "CORE",
    seq: 1,
    type,
  };
  // The feed's event union is wider than this test needs; this is the shape its reader reads.
  return event as unknown as DispatchEvent;
}

/** Whether `event` reads alice's person page again. */
function refreshesPersonPage(event: DispatchEvent): boolean {
  const key = ["people", "alice", "nodes"];
  return eventRefreshTargets(event, "bob").some((target) =>
    target.queryKey.every((part, index) => JSON.stringify(part) === JSON.stringify(key[index]))
  );
}

for (const type of ["comment.edited", "comment.deleted"]) {
  for (const onDocument of [false, true]) {
    test(`a ${type} of a comment on ${onDocument ? "a project document" : "an issue"} reads every person's page again`, () => {
      expect(refreshesPersonPage(commentEvent(type, onDocument))).toBe(true);
    });
  }
}

// The negative control: an event that changes no comment's words or marks leaves the page alone,
// so the rule above is not given to every event.
test("a comment.resolved leaves every person's page alone", () => {
  expect(refreshesPersonPage(commentEvent("comment.resolved", false))).toBe(false);
});
