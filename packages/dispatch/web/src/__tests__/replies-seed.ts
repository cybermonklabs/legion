import type { Cache } from "../api/cache/cache";
import { answeredByDispatch } from "../api/cache/test-client";
import { myRepliesOwnerQuery, whoAmIQuery } from "../api/queries";
import { replyRow } from "../features/threads/replies-fixture";
import type { RepliesOwner } from "../features/threads/useMarkThreadRead";

/** Seeds the signed-in viewer (alice) and `owner`'s unread threads, as Dispatch would answer
 *  them, so every thread mounted on that owner reads them from the cache rather than a request. */
export function seedUnread(cache: Cache, owner: RepliesOwner, threadIds: readonly string[]): void {
  cache.setDispatched(
    whoAmIQuery().queryKey,
    answeredByDispatch({ event_head: "0", kind: "user" as const, login: "alice" })
  );
  cache.setDispatched(
    myRepliesOwnerQuery(owner).queryKey,
    answeredByDispatch({
      as_of: "2026-10-12T00:00:00Z",
      limit: 200,
      offset: 0,
      rows: threadIds.map((id) => replyRow(id)),
      total: threadIds.length,
    })
  );
}
