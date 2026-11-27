import { INBOX_DISMISSED_PAGE } from "@legion/contracts";
import type { Response } from "@playwright/test";

import type { MeInboxResponse } from "../web/src/api/types";
import { dismissInbox, getMeInbox } from "./api";
import { replyRowId, seedBulkReplyThreads } from "./inbox-threads";

/**
 * The reader's dismissed threads at a person's scale: thousands of them, seeded in one batch.
 *
 * The threads are written as every bulk seed of Reply threads writes them: one statement into
 * `comments`, whose triggers write the reader's `user_threads` rows (`seedBulkReplyThreads`).
 * They are dismissed by the server's own writer, in one request: the Inbox's Dismiss under the
 * list's own narrowing, one issue and a mark (`POST /api/v1/me/inbox/dismiss`), which is what
 * **Select all** and Dismiss send. So each row carries the dismissal and the read mark that route
 * writes, and the store holds the dismissed and read tuples it states, as a person's own
 * dismissals leave them. A request per thread would be thousands of writes through the API, which
 * the suite's one worker cannot pay for.
 */

/** Seeds `count` threads on `issueKey` for `login` and dismisses all of them in one request.
 *  `olderThan` and `prefix` are `seedBulkReplyThreads`' own. Fails unless the route dismissed
 *  exactly `count` threads, so the issue must hold no other thread in the reader's Inbox. */
export async function seedDismissedThreads({
  count,
  issueKey,
  login = "alice",
  olderThan = 0,
  prefix,
}: {
  count: number;
  issueKey: string;
  login?: string;
  olderThan?: number;
  prefix: string;
}): Promise<void> {
  await seedBulkReplyThreads({ count, issueKey, login, olderThan, prefix });
  const { dismissed } = await dismissInbox(
    { dismiss_through: new Date().toISOString(), issue: issueKey },
    { login }
  );
  if (dismissed !== count) {
    throw new Error(`the dismissal of ${issueKey}'s ${count} threads changed ${dismissed}`);
  }
}

/** How many threads `login` has dismissed, counted through the API: the Dismissed view's own read,
 *  `INBOX_DISMISSED_PAGE` entries a page, followed from each page's `next_cursor` to the last. */
export async function countDismissedThreads(login = "alice"): Promise<number> {
  let counted = 0;
  let cursor: string | undefined;
  do {
    const page = await getMeInbox(
      {
        limit: INBOX_DISMISSED_PAGE,
        state: "dismissed",
        ...(cursor === undefined ? {} : { cursor }),
      },
      { login }
    );
    counted += page.entries?.length ?? 0;
    cursor = page.next_cursor ?? undefined;
  } while (cursor !== undefined);
  return counted;
}

/** Whether `response` answers a read of the Dismissed view's pages: the first, or with `cursor`
 *  the page that cursor names. */
export function isDismissedPage(response: Response, cursor?: string): boolean {
  const url = new URL(response.url());
  return (
    url.pathname === "/api/v1/me/inbox" &&
    url.searchParams.get("state") === "dismissed" &&
    url.searchParams.get("cursor") === (cursor ?? null)
  );
}

/** The row ids one page of the Dismissed view's read answers, in its order. */
export function pageIds(answer: MeInboxResponse): string[] {
  return (answer.entries ?? []).flatMap((entry) =>
    entry.thread === undefined || entry.thread === null ? [] : [replyRowId(entry.thread.thread_id)]
  );
}
