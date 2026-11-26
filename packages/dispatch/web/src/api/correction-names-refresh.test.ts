import { expect, test } from "bun:test";
import { QueryCache, QueryClient } from "@tanstack/react-query";

import { eventRefreshTargets } from "./event-refresh";
import type { RefreshTarget } from "./query-refresh";
import type { DispatchEvent } from "./types";

// R2 and R3 of dispatch://INFRA-1523, and the architect's decision of 2026-11-19 15:31Z on the
// names reads: an edit or a delete of a post refreshes every names read (`["reference-targets",
// <addresses>]`) that holds a member naming that post, or the chip and the published name keep its
// old words. GET /api/v1/reference-targets names a comment or a message by its id, whatever owner
// its address is written under (the server reads `dispatch://<KEY>/comment/<id>` by the id alone),
// so a page on CORE-2 whose text cites a CORE-1 comment as `dispatch://CORE-2/comment/<id>` holds
// that comment's words under a CORE-2 address. So the refresh (`namesReadsNaming`) matches such an
// address by the post's whole id, whatever owner it is written under, as the server reads it; a
// rule keyed by the event's own owner alone would never read that names read again.

const commentID = "11111111-2222-4333-8444-555555555555";
const messageID = "66666666-7777-4888-9999-aaaaaaaaaaaa";

/** A correction event of the post `id` of kind `kind` on CORE-1. */
function correction(type: string, id: string): DispatchEvent {
  const event = {
    actor: { id: "alice", kind: "user" },
    artifact_id: null,
    created_at: "2026-11-19T00:00:00Z",
    id: 1,
    issue_key: "CORE-1",
    notify: true,
    payload: { id, issue_key: "CORE-1" },
    project: "CORE",
    seq: 1,
    type,
  };
  // The feed's event union is wider than this test needs; this is the shape its reader reads.
  return event as unknown as DispatchEvent;
}

/** Whether `event` refreshes the names read keyed by `addresses`, as the page's cache matches. */
function refreshesNamesRead(event: DispatchEvent, addresses: string[]): boolean {
  const client = new QueryClient({ queryCache: new QueryCache() });
  const key = ["reference-targets", addresses];
  client.setQueryData(key, { targets: [] });
  const query = client.getQueryCache().find({ exact: true, queryKey: key });
  if (query === undefined) throw new Error("the names read was not cached");
  return eventRefreshTargets(event, "bob").some((target: RefreshTarget) => {
    const prefix = target.queryKey;
    const matches = prefix.every(
      (part, index) => JSON.stringify(part) === JSON.stringify(key[index])
    );
    return matches && (target.when === undefined || target.when(query.state.data, query));
  });
}

// The positive control: the same post cited under its own owner's address is read again, so each
// row below fails on the owner the address is written under and on nothing else.
for (const [type, id, kind] of [
  ["comment.edited", commentID, "comment"],
  ["comment.deleted", commentID, "comment"],
  ["message.edited", messageID, "message"],
  ["message.deleted", messageID, "message"],
] as const) {
  test(`${type} refreshes a names read citing the post under its own issue`, () => {
    expect(refreshesNamesRead(correction(type, id), [`dispatch://CORE-1/${kind}/${id}`])).toBe(
      true
    );
  });

  test(`${type} refreshes a names read citing the post under another issue's address`, () => {
    expect(refreshesNamesRead(correction(type, id), [`dispatch://CORE-2/${kind}/${id}`])).toBe(
      true
    );
  });
}
