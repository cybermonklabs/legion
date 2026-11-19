/**
 * The draft matrix's coverage list, Inbox message box (INFRA-1503): each composer census item that
 * `draft-matrix.message-box.test.tsx` answers for, held to one of its tests or to one of the
 * numbered reasons below, as `__tests__/composer-census/coverage.ts` says.
 */
import { answers, type CoverageEntry, cells } from "../../__tests__/composer-census/coverage";

/** The host as the matrix file's titles name it. */
const HOST = "Inbox message box";

/** The numbered reasons, said here alone, which the matrix file's header points to. */
const REASONS = {
  1: "1. The box draws no Back of its own; Escape from the reading column takes focus back to its row (`Inbox`'s `back`), and below the column's width there is no column to leave (11).",
  2: "2. The box always answers its thread's root message (`replyTo` names `thread.thread_id`'s message), so a newer message in the thread does not move it, and it is never elsewhere.",
  3: "3. The column draws no collapse of the thread; leaving it is the move to another row.",
  4: "4. A standing box's reply is its own address, so it draws no Cancel reply (asserted absent).",
  5: "5. The box has no anchor, so it draws no Close (asserted absent below).",
  6: "6. The issue picker is an Agents row's (`AgentMessageComposer`), and an issue message thread's box is a `MentionComposer` of its own, no Agents row's composer.",
  7: "7. `MessageThreadBody` draws a loading line or an error in place of the thread while the issue's messages are read or their read fails, which unmounts the box as the move to another row does, and the box drawn when the read lands again is the one that row draws.",
  8: "8. Which end of the thread the box sits at (`composerFirst`, from `useThreadOrder`) is read when the box's thread is drawn and again only when who is signed in changes (`useUserPreference`), so it never moves a box that is on screen; the shell draws no thread before it knows who is signed in, and another person is the sign-in file's row.",
  9: "9. The Inbox draws a thread's box in the reading column above the column's width and in the row's card below it (`paneShown`), in a drawer that closing takes away (`InboxDrawer`), and on its own page (`app.tsx`'s `/` and `/credentials/*`); a resize across that width, closing the drawer, leaving the page, a column with no row selected or an empty list, and the Inbox's loading line or error unmount the box as the move to another row does, and the box drawn on return is the one that row draws, at the same thread's send name.",
  10: "10. The box answers its thread's root message and is drawn only while the read holds it (`root === undefined` draws none), so a read without the root leaves no message to answer, and a read that holds it again draws the box at the same thread, as the move to another row and back does.",
  11: "11. Below the reading column's width, a phone's included, the Inbox draws no column: each thread row is a card that draws its thread's own box (`ThreadRowCard`, `renderRow`'s `paneShown`), so no move takes a box from one row to another there, and each box stays at its thread while its row is listed. What ends a card's box is (9)'s: leaving the page, closing the drawer the compact header's Peek opens (`inboxDrawerOpen`), the row leaving the list. The box's own moves are the same `MessageThreadBody`'s as the rows above. The layout's other branches in `Inbox` and `usePaneSelection` pick, focus and scroll the column's row and draw the list beside the column, and move no box; the app shell's forms are the sign-in file's.",
} as const;

export const coverage: readonly CoverageEntry[] = [
  ...answers(
    "ThreadTurns | MessageThreadBody <MentionComposer mutationKey={threadSendKey(thread.thread_id)}> | conditional | (composerFirst) ? …",
    { reason: REASONS[8] }
  ),
  ...answers(
    "ThreadTurns | MessageThreadBody <MentionComposer mutationKey={threadSendKey(thread.thread_id)}> | conditional | !(composerFirst) ? …",
    { reason: REASONS[8] }
  ),
  ...answers(
    // biome-ignore lint/suspicious/noTemplateCurlyInString: the key holds the expression's own source text, whose template literal this is.
    "MessageThreadBody | MessageThreadBody <MentionComposer mutationKey={threadSendKey(thread.thread_id)}> | key | key={`composer:${thread.thread_id}`}",
    cells(`${HOST} · the column moves to another row, then back`)
  ),
  ...answers(
    "MessageThreadBody | MessageThreadBody <MentionComposer mutationKey={threadSendKey(thread.thread_id)}> | address prop | mutationKey={threadSendKey(thread.thread_id)}",
    cells(`${HOST} · the column moves to another row, then back`),
    { row: `${HOST} · Close, then Discard draft? · cannot make: the box draws no Close` },
    { reason: REASONS[1] },
    { reason: REASONS[3] },
    { reason: REASONS[5] },
    { reason: REASONS[6] }
  ),
  ...answers(
    'MessageThreadBody | MessageThreadBody <MentionComposer mutationKey={threadSendKey(thread.thread_id)}> | address prop | owner={{ issueKey, kind: "issue" }}',
    cells(`${HOST} · the column moves to another row, then back`)
  ),
  ...answers(
    'MessageThreadBody | MessageThreadBody <MentionComposer mutationKey={threadSendKey(thread.thread_id)}> | address prop | replyTo={{ actor: root.author, excerpt: "", id: root.id, parentKind: "message", ...(target === null || target === undefined ? {} : { thread: { delivery: deliveryMode(last?.delivery ?? "") ?? "steer", target } }), }}',
    cells(`${HOST} · Escape in the field`),
    { row: `${HOST} · Cancel reply · cannot make: a standing box draws no Cancel reply` },
    { reason: REASONS[2] },
    { reason: REASONS[4] }
  ),
  ...answers(
    "MessageThreadBody | MessageThreadBody <MentionComposer mutationKey={threadSendKey(thread.thread_id)}> | conditional | !(root === undefined) ? …",
    { reason: REASONS[10] }
  ),
  ...answers(
    "MessageThreadBody | MessageThreadBody <MentionComposer mutationKey={threadSendKey(thread.thread_id)}> | early return | if (messages.isPending) return …",
    { reason: REASONS[7] }
  ),
  ...answers(
    "MessageThreadBody | MessageThreadBody <MentionComposer mutationKey={threadSendKey(thread.thread_id)}> | early return | if (messages.isError) return …",
    { reason: REASONS[7] }
  ),
  ...answers("Inbox | ReadingPane | conditional | (paneShown) ? …", { reason: REASONS[9] }),
  ...answers("InboxDrawer | Inbox | early return | if (!open) return …", { reason: REASONS[9] }),
  ...answers('AppShell | InboxPage | route | <Route path="/">', { reason: REASONS[9] }),
  ...answers('AppShell | InboxPage | route | <Route path="/credentials/*">', {
    reason: REASONS[9],
  }),
  ...answers(
    "ThreadBody | MessageThreadBody | conditional | !(issueKey === undefined) ? …",
    cells(`${HOST} · the column moves to another row, then back`)
  ),
  ...answers(
    'ThreadBody | MessageThreadBody | early return | if (row.kind === "agent") return …',
    cells(`${HOST} · the column moves to another row, then back`)
  ),
  ...answers(
    'ThreadBody | MessageThreadBody | early return | if (row.kind === "ask") return …',
    cells(`${HOST} · the column moves to another row, then back`)
  ),
  ...answers(
    'ThreadBody | MessageThreadBody | conditional | if (row.kind === "message")',
    cells(`${HOST} · the column moves to another row, then back`)
  ),
  ...answers(
    "ThreadBody | MessageThreadBody | address prop | issueKey={issueKey}",
    cells(`${HOST} · the column moves to another row, then back`)
  ),
  ...answers(
    "ThreadBody | MessageThreadBody | address prop | thread={row}",
    cells(`${HOST} · the column moves to another row, then back`)
  ),
  ...answers("Inbox | ThreadCardRow | form | if (paneShown) return …", { reason: REASONS[11] }),
  ...answers("AppShell | InboxDrawer | form | open={inboxDrawerOpen}", { reason: REASONS[11] }),
  ...answers(
    "MessageThreadBody | MessageThreadBody <MentionComposer mutationKey={threadSendKey(thread.thread_id)}> | lifetime | <MentionComposer mutationKey={threadSendKey(thread.thread_id)}>",
    cells(`${HOST} · the page reloaded, then back`)
  ),
];
