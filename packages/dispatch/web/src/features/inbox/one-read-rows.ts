/**
 * The rows the Inbox draws of the one read, `GET /api/v1/me/inbox`: each entry whose node is a
 * row of the page carries that row (`inboxEntryRow`), so it becomes the `InboxListRow` the list
 * already draws - an ask row of the ask, a thread row of a Reply or an Agent row - and the list's
 * merge, its narrowing and its row shape stay the ones it has. An entry the page draws no row for
 * (a mention of the reader, a write of theirs nobody answered) is no row here either. A row of an
 * issue carries its entry's approval gate (`InboxRowGate`).
 *
 * Nothing here is a hook, so the refresh rules read it too (`api/event-refresh.ts`).
 */

import { type ApprovalGateLine, inboxEntryRow } from "@legion/contracts";

import type {
  MeInboxAgentRow,
  MeInboxEntry,
  MeInboxResponse,
  MessageRead,
  MyReplyRow,
} from "../../api/types";
import type { InboxListRow } from "./list-row";

/** The rows `entries` carries, in the read's own order, each with its entry's gate. An Agent row
 *  keeps its conversation (`MeInboxAgentRow.conversation`), which its reading pane draws
 *  (`agentConversation`). */
export function meInboxListRows(
  entries: readonly MeInboxEntry[] | null | undefined
): InboxListRow[] {
  const rows: InboxListRow[] = [];
  for (const entry of entries ?? []) {
    const row = inboxEntryRow(entry);
    if (row.kind === "ask") rows.push({ ask: row.ask, kind: "ask" });
    else if (row.kind === "thread") rows.push({ kind: "thread", thread: row.thread });
    else if (row.kind === "agent") rows.push({ kind: "thread", thread: row.agent });
  }
  return rows;
}

/** The rows a cached read of the one read holds, whichever of its reads it is: one answer, or
 *  the pages of the paged one (`meInboxDismissedQuery`). Anything else - a count, no data - holds
 *  none. */
export function meInboxCachedRows(data: unknown): InboxListRow[] {
  const read = data as { readonly entries?: unknown; readonly pages?: unknown } | undefined;
  if (Array.isArray(read?.pages)) {
    return (read.pages as readonly MeInboxResponse[]).flatMap((page) =>
      meInboxListRows(page.entries)
    );
  }
  return Array.isArray(read?.entries) ? meInboxListRows(read.entries as MeInboxEntry[]) : [];
}

/** The conversation an Agent row of the one read carries, which its reading pane draws in place
 *  of reading the session's conversations; undefined for a row of another read, which carries
 *  none. */
export function agentConversation(thread: MyReplyRow): MessageRead | undefined {
  return "conversation" in thread ? (thread as MeInboxAgentRow).conversation : undefined;
}

/** The approval gate of the issue a list row belongs to, which the row's mark draws: the
 *  `approval_gate` its ask or its thread carries (`InboxRowGate`), undefined where it carries
 *  none. */
export function rowApprovalGate(row: InboxListRow): readonly ApprovalGateLine[] | undefined {
  return (row.kind === "ask" ? row.ask : row.thread).approval_gate;
}
