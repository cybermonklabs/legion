/**
 * What the Inbox draws under the cleared assignee filter (`?assignee=everyone`), worked out from
 * what the server holds rather than known in advance.
 *
 * The cleared filter lists every open question of the stack, so a real-stack row reading it
 * reads every earlier row's questions beside its own: what it may assert about the list is what
 * the page draws from the server's answer, not the count it drew over an emptied database. That
 * drawing is the client's: the Inbox reads the open questions (`GET /api/v1/inbox`) and the
 * reader's own threads (`GET /api/v1/me/replies`, the Inbox's kinds, `state=inbox`), sorts them
 * newest first, bands them by priority, folds **Later**, and draws at most `GROUP_PAGE` rows of
 * each band with a **Show more** for the rest (`Inbox.tsx`). This module makes the same two reads
 * through e2e/api.ts and applies the product's own rules, imported from
 * `web/src/features/inbox/`, never copied, so the rows a row expects are the rows the page's own
 * code draws from that answer.
 *
 * It reads; it writes nothing. A row calls it once its seeds are written and before it reads
 * the page, and rows of one worker run one at a time on that worker's own stack, so nothing else
 * writes between the read here and the page's.
 */
import { INBOX_THREAD_KINDS } from "@legion/contracts";
import { expect, type Page } from "@playwright/test";

import { DEFAULT_FOLDED_GROUPS, group, sortRows } from "../web/src/features/inbox/grouping";
import { GROUP_PAGE } from "../web/src/features/inbox/InboxGroups";
import { type InboxListRow, rowId } from "../web/src/features/inbox/list-row";
import { sectionTitles } from "../web/src/features/inbox/sections";
import { getInbox, listMyReplies } from "./api";

/** The cleared Inbox as `viewer`'s page draws it. */
export interface DrawnInbox {
  /** Every row the page draws, as its `data-inbox-row` names it, top to bottom. */
  readonly rows: readonly string[];
  /** The text of each **Show more** the page draws, top to bottom: one for each unfolded band
   *  holding more than `GROUP_PAGE` rows. */
  readonly more: readonly string[];
}

/**
 * The rows `viewer`'s Inbox draws under `?assignee=everyone`, opened in a browser that has
 * folded nothing itself: the open questions the server answers `viewer`, and `viewer`'s own
 * threads, in the order and the bands the Inbox's default sort (`newest`) and grouping
 * (`priority`) put them, each band drawing its first `GROUP_PAGE` rows and **Later**, folded by
 * default (`DEFAULT_FOLDED_GROUPS`), drawing none.
 */
export async function clearedInbox(viewer: string): Promise<DrawnInbox> {
  const [asks, threads] = await Promise.all([
    getInbox({ login: viewer }),
    listMyReplies({ kind: INBOX_THREAD_KINDS, state: "inbox" }, { login: viewer }),
  ]);
  const candidates: InboxListRow[] = [
    ...asks.map((ask): InboxListRow => ({ ask, kind: "ask" })),
    ...(threads.rows ?? []).map((thread): InboxListRow => ({ kind: "thread", thread })),
  ];
  // The cleared filter is the empty selection (`parseInboxSearch`), whose bands speak in the
  // third person (`sectionTitles`).
  const bands = group(sortRows(candidates, "newest", viewer), "priority", {
    now: Date.now(),
    titles: sectionTitles([], viewer),
    viewer,
  });
  const rows: string[] = [];
  const more: string[] = [];
  for (const band of bands) {
    if (DEFAULT_FOLDED_GROUPS.includes(band.key)) continue;
    const drawn = band.rows.slice(0, GROUP_PAGE);
    rows.push(...drawn.map(rowId));
    const held = band.rows.length - drawn.length;
    if (held > 0) {
      more.push(
        `Show ${Math.min(held, GROUP_PAGE)} more${band.label === undefined ? "" : ` in ${band.label}`}`
      );
    }
  }
  return { more, rows };
}

/** The Inbox on `page` draws as many rows as `drawn` holds, and the **Show more** buttons it
 *  holds, in order: the count a row read over an emptied database, now the one the page's own
 *  rules draw from the server's answer. */
export async function expectDrawn(page: Page, drawn: DrawnInbox): Promise<void> {
  await expect(page.locator("[data-inbox-row]")).toHaveCount(drawn.rows.length);
  await expect(page.locator("[data-inbox-show-more]")).toHaveText([...drawn.more]);
}
