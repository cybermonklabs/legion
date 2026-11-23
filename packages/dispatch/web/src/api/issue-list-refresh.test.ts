import { expect, test } from "bun:test";
import type { Query } from "@tanstack/react-query";

import { eventRefreshTargets } from "./event-refresh";
import type { DispatchEvent } from "./types";

/** An issue write of `key`; `changed` names the fields it moved, and an older server names none. */
function issueEvent(
  type: "issue.created" | "issue.updated" | "issue.closed",
  key: string,
  changed?: readonly string[]
): DispatchEvent {
  const event = {
    actor: { id: "alice", kind: "user" },
    created_at: "2026-11-22T00:00:00Z",
    id: 1,
    issue_key: key,
    notify: true,
    payload: { key, ancestors: [], ...(changed === undefined ? {} : { changed }) },
    project: key.slice(0, key.lastIndexOf("-")),
    seq: 1,
    type,
  };
  // The feed's event union is wider than this test needs; this is the shape its reader reads.
  return event as unknown as DispatchEvent;
}

/** Two lists a document holds, as `queryIssuesQuery` keys and caches them: one whose rows hold
 *  CORE-1, and one whose rows do not. */
const HOLDING = {
  key: ["issues", "query", ["CORE"], ["overnight"], [], []],
  rows: [{ key: "CORE-1" }],
};
const OTHER = { key: ["issues", "query", [], ["dispatch-ux"], [], []], rows: [{ key: "OSSV-9" }] };

/** The lists the event reads again: those a target's key is a prefix of, whose rows its `when`
 *  test, where it has one, accepts. */
function listsRead(event: DispatchEvent): string[] {
  const targets = eventRefreshTargets(event);
  return [
    ["holding", HOLDING],
    ["other", OTHER],
  ].flatMap(([name, list]) => {
    const { key, rows } = list as typeof HOLDING;
    const read = targets.some(
      (target) =>
        target.queryKey.every((part, index) => part === key[index]) &&
        !(target.exact === true && target.queryKey.length !== key.length) &&
        (target.when === undefined || target.when(rows, {} as Query))
    );
    return read ? [name as string] : [];
  });
}

// c-live at the unit level (dispatch://INFRA-4935/spec#b-r-live): a write that can carry an issue
// into a list's query or out of it reads every list again; any other write reads again only the
// lists whose rows hold the issue.
test("a list reads again when an issue can enter or leave it, or when one of its rows changes", () => {
  for (const changed of [["labels"], ["status", "closed_at"], ["priority"]]) {
    expect(listsRead(issueEvent("issue.updated", "CORE-1", changed))).toEqual(["holding", "other"]);
    expect(listsRead(issueEvent("issue.updated", "CORE-7", changed))).toEqual(["holding", "other"]);
  }
  expect(listsRead(issueEvent("issue.created", "CORE-7", []))).toEqual(["holding", "other"]);
  expect(listsRead(issueEvent("issue.closed", "CORE-7", ["status", "closed_at"]))).toEqual([
    "holding",
    "other",
  ]);
  // An older server names no field, so any of them may have moved.
  expect(listsRead(issueEvent("issue.updated", "CORE-7"))).toEqual(["holding", "other"]);
  // A title or an assignee moves a row a list holds, and no list that does not hold it.
  expect(listsRead(issueEvent("issue.updated", "CORE-1", ["title"]))).toEqual(["holding"]);
  expect(listsRead(issueEvent("issue.updated", "CORE-7", ["title"]))).toEqual([]);
  expect(listsRead(issueEvent("issue.updated", "CORE-7", ["assignee"]))).toEqual([]);
});
