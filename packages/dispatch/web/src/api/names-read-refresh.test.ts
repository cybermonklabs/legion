import { expect, test } from "bun:test";
import { QueryClient } from "@tanstack/react-query";

import { eventRefreshTargets } from "./event-refresh";
import { referenceTargetsQuery } from "./queries";
import { refreshQueries } from "./query-refresh";
import type { DispatchEvent } from "./types";

/**
 * A names read (`referenceTargetsQuery`) answers an issue's own address with the issue's title and
 * its status beside it, which the status mark beside a reference to the issue draws (INFRA-4009
 * R2). So a write that moves the status - a status change, a close, a reopen - costs the reads a
 * title change costs: it refreshes the names reads whose key holds the issue's own address, and
 * no other (`eventRefreshTargets`, R8). A write that moves neither leaves every names read alone.
 */

/** A write of CORE-1 as the feed carries it, `changed` naming what it moved. The feed's event
 *  union is wider than this test needs; this is the shape its reader reads. */
function issueWrite(
  type: "issue.closed" | "issue.updated",
  changed: readonly string[] | undefined
): DispatchEvent {
  return {
    actor: { id: "alice", kind: "user" },
    created_at: "2026-11-21T00:00:00Z",
    id: 1,
    issue_key: "CORE-1",
    notify: true,
    payload: changed === undefined ? { key: "CORE-1" } : { changed, key: "CORE-1" },
    project: "CORE",
    seq: 1,
    type,
  } as unknown as DispatchEvent;
}

/** The address sets among `held` whose names read `event` refreshes: each set is cached under the
 *  key a page reads it by, and the event's rules run over the cache as the feed runs them. */
function refreshedNames(event: DispatchEvent, held: readonly (readonly string[])[]): string[][] {
  const client = new QueryClient();
  for (const refs of held) {
    client.setQueryData(referenceTargetsQuery(refs).queryKey, { targets: [] });
  }
  refreshQueries(client, eventRefreshTargets(event, "alice"));
  return held
    .filter(
      (refs) => client.getQueryState(referenceTargetsQuery(refs).queryKey)?.isInvalidated === true
    )
    .map((refs) => [...refs]);
}

/** A names read whose key holds CORE-1's own address, beside another issue's. */
const NAMING_ISSUE = ["dispatch://CORE-1", "dispatch://CORE-2"];

/** Names reads whose key holds no address of CORE-1 itself: another issue's alone, CORE-12's -
 *  whose address begins with CORE-1's letters - and an address under CORE-1, which none of its
 *  status or title names. */
const NAMING_OTHERS = [
  ["dispatch://CORE-2"],
  ["dispatch://CORE-12", "dispatch://CORE-12/comment/comment-1"],
  ["dispatch://CORE-1/spec#b-s1"],
];

test("a status change, a close and a reopen refresh the names reads holding the issue's address, and no other", () => {
  const moves: [string, DispatchEvent][] = [
    ["a close", issueWrite("issue.closed", ["status", "closed_at"])],
    ["a close an older server sent", issueWrite("issue.closed", undefined)],
    ["a status change", issueWrite("issue.updated", ["status"])],
    ["a reopen", issueWrite("issue.updated", ["closed_at"])],
  ];
  for (const [name, event] of moves) {
    expect(refreshedNames(event, [NAMING_ISSUE, ...NAMING_OTHERS]), name).toEqual([NAMING_ISSUE]);
  }
});

test("an issue write that moves neither its title nor its status refreshes no names read", () => {
  expect(
    refreshedNames(issueWrite("issue.updated", ["priority"]), [NAMING_ISSUE, ...NAMING_OTHERS])
  ).toEqual([]);
});
