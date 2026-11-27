/**
 * The draft matrix's coverage list, Inbox thread box (INFRA-1503): each composer census item that
 * `draft-matrix.thread-box.test.tsx` answers for, held to one of its tests or to one of the
 * numbered reasons below, as `__tests__/composer-census/coverage.ts` says.
 */
import { answers, type CoverageEntry, cells } from "../../__tests__/composer-census/coverage";

/** The host as the matrix file's titles name it. */
const HOST = "Inbox thread box";

/** The numbered reasons, said here alone, which the matrix file's header points to. */
const REASONS = {
  1: "1. The box draws no Back of its own; Escape from the reading column takes focus back to its row (`Inbox`'s `back`), and below the column's width there is no column to leave (13).",
  2: "2. Elsewhere needs a held refusal; the typed version is the unmount row.",
  3: "3. The column draws no collapse of the thread; leaving it is the move to another row.",
  4: "4. A standing box's reply is its own address, so it draws no Cancel reply (asserted absent).",
  5: "5. The box has no anchor, so it draws no Close (asserted absent below).",
  6: "6. An Agent row's box always answers a message of its thread (`agentReplyTo`), and its `AgentMessageComposer` draws the issue picker only while it answers none, so it draws no issue picker (asserted absent below).",
  7: "7. `CommentThreadBody` and `AgentThreadBody` draw a loading line or an error in place of the thread while its read is out or failed, which unmounts the box as the move to another row does, and the box drawn when the read lands again is the one that row draws.",
  8: "8. Which end of the thread the box sits at (`composerFirst`, from `useThreadOrder`) is read when the thread is drawn and again only when who is signed in changes (`useUserPreference`), so it never moves a box that is on screen; the shell draws no thread before it knows who is signed in, and another person is the sign-in file's row.",
  9: "9. An Agent row's box is drawn only while its session is in the live list; a session that leaves it and comes back unmounts and draws the box again at the same thread, as the move to another row and back does.",
  10: "10. A thread in a document is drawn inside the document's page where its block is found and beside it where the document fails or the block is gone (`ThreadPane`, `DocumentPane`, `ProofDocument`, keyed by the document); moving between the two draws the box afresh at the same thread, as the move to another row and back does, and a version or an admission that hides the document's page keeps the box mounted at its thread.",
  11: "11. A box answers its thread's root; a read that no longer holds the root draws no box, since there is no message left to answer, and a read that holds it again draws the box at the same thread, as the move to another row and back does.",
  12: "12. The Inbox page draws a thread's box in one place for the row the reader is on: in the reading column beside the list from the column's width (`columns`), and below it in the reading view that stands in the list's place (`readingView`), both the same `ReadingPane`; the drawer draws it in each thread row's card (`renderRow`'s `if (mode !== \"cards\")`), in a drawer that closing takes away (`InboxDrawer`); and the page is drawn on its own route (`app.tsx`'s `/` and `/credentials/*`). A resize across that width, opening a row or going back to the list, closing the drawer, leaving the page, a column with no row selected or an empty list, and the Inbox's loading line or error unmount the box as the move to another row does, and the box drawn on return is the one that row draws, at the same thread's send name. The list stays mounted, hidden, under the reading view (`<div hidden={readingView}>`), and a card of the drawer is never under it.",
  13: "13. Below the reading column's width the Inbox page lists short rows, which draw no box, and the one row the reader opens is read in place of the list in the column's own `ReadingPane` (`readingView`), so its box is the column's and what ends it is (12)'s. The drawer (`pane={false}`) draws every thread row as a card that draws its thread's own box at every width (`ThreadRowCard`), so no move takes a box from one row to another there, and each box stays at its thread while its row is listed; what ends a card's box is (12)'s: closing the drawer the compact header's Peek opens (`inboxDrawerOpen`), the row leaving the list. The box's own moves are the same `ThreadBody`'s as the rows above. The layout's other branches in `Inbox` (its `mode`), its keys (`useInboxKeymap`), `usePaneSelection` and `useReadingViewLanding` pick, focus and scroll the row, find the control a key presses in the item in hand (`atItem`, `approvalControl`, `cardControl`), open and close the reading view through the address and its history entry (`inboxOverList`, `inboxDefaultPick`), hand the reader on after their own action (`landHandOff`), draw the list beside the column, and end the line that says the open row left (`rowLeft`), and move no box; the app shell's forms are the sign-in file's.",
  14: "14. A root its author deleted takes no reply (`409 POST_DELETED`), so no box is drawn under it, as the margin's card draws no reply composer there, and the box leaves the page as a read without the root takes it (11): a reply the reader kept for the thread - typed in the box, or a send refused or stopped - is drawn itself in its place, saying the comment was deleted, with Discard my draft and no Send (`KeptReplyToDeleted`); `ThreadBody.test.tsx`'s \"a comment thread whose root is deleted draws no box, and keeps the reply typed there\" holds it.",
} as const;

export const coverage: readonly CoverageEntry[] = [
  ...answers(
    "AgentMessageComposer | AgentMessageComposer <MentionComposer mutationKey={sendKey}> | address prop | standing={standing}",
    { reason: REASONS[4] }
  ),
  ...answers(
    // biome-ignore lint/suspicious/noTemplateCurlyInString: the key holds the expression's own source text, whose template literal this is.
    "CommentThreadBody | CommentThreadBody <MentionComposer mutationKey={threadSendKey(thread.thread_id)}> | key | key={`composer:${thread.thread_id}`}",
    cells(`${HOST} · the column moves to another row, then back`)
  ),
  ...answers(
    "CommentThreadBody | CommentThreadBody <MentionComposer mutationKey={threadSendKey(thread.thread_id)}> | address prop | mutationKey={threadSendKey(thread.thread_id)}",
    cells(`${HOST} · the column moves to another row, then back`),
    { row: `${HOST} · Close, then Discard draft? · cannot make: the box draws no Close` },
    { reason: REASONS[1] },
    { reason: REASONS[3] },
    { reason: REASONS[5] }
  ),
  ...answers(
    "CommentThreadBody | CommentThreadBody <MentionComposer mutationKey={threadSendKey(thread.thread_id)}> | address prop | owner={owner}",
    cells(`${HOST} · the column moves to another row, then back`)
  ),
  ...answers(
    'CommentThreadBody | CommentThreadBody <MentionComposer mutationKey={threadSendKey(thread.thread_id)}> | address prop | replyTo={{ actor: root.author, excerpt: "", id: root.id, parentKind: "comment" }}',
    cells(`${HOST} · Escape in the field`),
    { row: `${HOST} · Cancel reply · cannot make: a standing box draws no Cancel reply` },
    { reason: REASONS[4] },
    { reason: REASONS[11] }
  ),
  ...answers(
    "CommentThreadBody | CommentThreadBody <MentionComposer mutationKey={threadSendKey(thread.thread_id)}> | conditional | !(root === undefined) ? …",
    { reason: REASONS[11] }
  ),
  ...answers(
    "CommentThreadBody | CommentThreadBody <MentionComposer mutationKey={threadSendKey(thread.thread_id)}> | conditional | !(postDeletion(root) !== null) ? …",
    { reason: REASONS[14] }
  ),
  ...answers(
    "ThreadTurns | CommentThreadBody <MentionComposer mutationKey={threadSendKey(thread.thread_id)}> | conditional | (composerFirst) ? …",
    { reason: REASONS[8] }
  ),
  ...answers(
    "ThreadTurns | CommentThreadBody <MentionComposer mutationKey={threadSendKey(thread.thread_id)}> | conditional | !(composerFirst) ? …",
    { reason: REASONS[8] }
  ),
  ...answers(
    "CommentThreadBody | CommentThreadBody <MentionComposer mutationKey={threadSendKey(thread.thread_id)}> | early return | if (comments.isPending) return …",
    { reason: REASONS[7] }
  ),
  ...answers(
    "CommentThreadBody | CommentThreadBody <MentionComposer mutationKey={threadSendKey(thread.thread_id)}> | early return | if (comments.isError) return …",
    { reason: REASONS[7] }
  ),
  ...answers(
    // biome-ignore lint/suspicious/noTemplateCurlyInString: the key holds the expression's own source text, whose template literal this is.
    "AgentThreadBody | AgentMessageComposer | key | <AgentMessageComposer key={`composer:${thread.thread_id}`}>",
    cells(`${HOST} · the column moves to another row, then back · an Agent row's box`)
  ),
  ...answers(
    "AgentThreadBody | AgentMessageComposer | conditional | !(agent === undefined || read === undefined || newest === undefined) ? …",
    { reason: REASONS[9] }
  ),
  ...answers("ThreadTurns | AgentMessageComposer | conditional | (composerFirst) ? …", {
    reason: REASONS[8],
  }),
  ...answers("ThreadTurns | AgentMessageComposer | conditional | !(composerFirst) ? …", {
    reason: REASONS[8],
  }),
  ...answers(
    "AgentThreadBody | AgentMessageComposer | early return | if (messages.isPending) return …",
    { reason: REASONS[7] }
  ),
  ...answers(
    "AgentThreadBody | AgentMessageComposer | early return | if (messages.isError) return …",
    { reason: REASONS[7] }
  ),
  ...answers("AgentThreadBody | AgentMessageComposer | address prop | agent={agent}", {
    reason: REASONS[9],
  }),
  ...answers(
    "AgentThreadBody | AgentMessageComposer | address prop | replyTo={agentReplyTo(agent, read, newest)}",
    cells(`${HOST} · host moves it over a refusal (elsewhere)`, ["refused", "stopped"]),
    {
      row: `${HOST} · typed while a refusal is held elsewhere, then the composer unmounts · refused`,
    },
    { row: `${HOST} · Agents pick · cannot make: an Agent row's box draws no issue picker` },
    { reason: REASONS[2] },
    { reason: REASONS[6] }
  ),
  ...answers(
    "AgentThreadBody | AgentMessageComposer | address prop | sendKey={sendKey}",
    cells(`${HOST} · the column moves to another row, then back · an Agent row's box`)
  ),
  ...answers("AgentThreadBody | AgentMessageComposer | address prop | standing", {
    reason: REASONS[4],
  }),
  ...answers(
    'ThreadBody | AgentThreadBody | conditional | if (row.kind === "agent")',
    cells(`${HOST} · the column moves to another row, then back · an Agent row's box`)
  ),
  ...answers(
    "ThreadBody | AgentThreadBody | address prop | thread={row}",
    cells(`${HOST} · the column moves to another row, then back · an Agent row's box`)
  ),
  ...answers(
    "ThreadBodyContent | ThreadBody | address prop | thread={thread}",
    cells(`${HOST} · the column moves to another row, then back`)
  ),
  ...answers(
    "ProofDocument | ThreadBody | conditional | !(anchoredThread === undefined || anchoredHost === undefined) ? …",
    { reason: REASONS[10] }
  ),
  ...answers(
    "ProofDocument | ThreadBody | conditional | <article hidden={version !== undefined || admissionHidesEditor}>",
    { reason: REASONS[10] }
  ),
  ...answers("PaneDocument | ThreadBody | key | <ProofDocument key={artifact.id}>", {
    reason: REASONS[10],
  }),
  ...answers(
    "DocumentPane | ThreadBody | conditional | !(!document.ready || artifact === undefined || user === undefined) ? …",
    { reason: REASONS[10] }
  ),
  ...answers("DocumentPane | ThreadBody | conditional | (paneBody === undefined) ? …", {
    reason: REASONS[10],
  }),
  ...answers(
    "DocumentPane | ThreadBody | early return | if (failed || blockFound === false) return …",
    { reason: REASONS[10] }
  ),
  ...answers("DocumentPane | ThreadBody | conditional | !(paneBody === undefined) ? …", {
    reason: REASONS[10],
  }),
  ...answers(
    'ThreadPane | ThreadBody | early return | if (row.kind === "agent") return …',
    cells(`${HOST} · the column moves to another row, then back`)
  ),
  ...answers(
    'ThreadPane | ThreadBody | early return | if (row.kind === "ask") return …',
    cells(`${HOST} · the column moves to another row, then back`)
  ),
  ...answers(
    "ThreadPane | ThreadBody | conditional | if (inDocument && anchor !== undefined && !anchor.orphaned && blockId !== null && blockId !== undefined)",
    { reason: REASONS[10] }
  ),
  ...answers(
    "ThreadPane | ThreadBody | address prop | thread={thread}",
    cells(`${HOST} · the column moves to another row, then back`)
  ),
  ...answers("ThreadRowCard | ThreadBody | address prop | thread={thread}", {
    reason: REASONS[13],
  }),
  ...answers(
    "ThreadBodyPane | ThreadBodyContent | address prop | <ThreadBodyContent {...pane}>",
    cells(`${HOST} · the column moves to another row, then back`)
  ),
  ...answers(
    "DocumentPane | ThreadBodyContent | conditional | if (failed || blockFound === false)",
    { reason: REASONS[10] }
  ),
  ...answers("ThreadDocumentBody | ThreadBodyContent | address prop | thread={thread}", {
    reason: REASONS[10],
  }),
  ...answers(
    'ReadingPane | ThreadPane | conditional | if (row.kind === "thread")',
    cells(`${HOST} · the column moves to another row, then back`)
  ),
  ...answers(
    "ReadingPane | ThreadPane | address prop | thread={row.thread}",
    cells(`${HOST} · the column moves to another row, then back`)
  ),
  ...answers("ThreadCardRow | ThreadRowCard | address prop | thread={thread}", {
    reason: REASONS[13],
  }),
  ...answers(
    "AskThreadPane | ThreadBodyPane | early return | if (ask !== undefined && isDecisionAsk(ask)) return …",
    { reason: REASONS[10] }
  ),
  ...answers(
    'ThreadPane | ThreadBodyPane | conditional | if (row.kind === "agent")',
    cells(`${HOST} · the column moves to another row, then back · an Agent row's box`)
  ),
  ...answers(
    'ThreadPane | ThreadBodyPane | early return | if (row.kind === "agent") return …',
    cells(`${HOST} · the column moves to another row, then back`)
  ),
  ...answers(
    'ThreadPane | ThreadBodyPane | early return | if (row.kind === "ask") return …',
    cells(`${HOST} · the column moves to another row, then back`)
  ),
  ...answers(
    "ThreadPane | ThreadBodyPane | early return | if (inDocument && anchor !== undefined && !anchor.orphaned && blockId !== null && blockId !== undefined) return …",
    { reason: REASONS[10] }
  ),
  ...answers(
    "AskThreadPane | ThreadDocumentBody | conditional | if (ask !== undefined && isDecisionAsk(ask))",
    { reason: REASONS[10] }
  ),
  ...answers("AskThreadPane | ThreadDocumentBody | address prop | thread={thread}", {
    reason: REASONS[10],
  }),
  ...answers(
    'ThreadPane | ThreadDocumentBody | early return | if (row.kind === "agent") return …',
    cells(`${HOST} · the column moves to another row, then back`)
  ),
  ...answers(
    'ThreadPane | ThreadDocumentBody | early return | if (row.kind === "ask") return …',
    cells(`${HOST} · the column moves to another row, then back`)
  ),
  ...answers(
    "ThreadPane | ThreadDocumentBody | conditional | if (inDocument && anchor !== undefined && !anchor.orphaned && blockId !== null && blockId !== undefined)",
    { reason: REASONS[10] }
  ),
  ...answers("ThreadPane | ThreadDocumentBody | address prop | thread={row}", {
    reason: REASONS[10],
  }),
  ...answers("InboxBody | ReadingPane | conditional | !(subject === undefined) ? …", {
    reason: REASONS[12],
  }),
  ...answers('InboxBody | ReadingPane | conditional | (mode === "columns") ? …', {
    reason: REASONS[12],
  }),
  ...answers('InboxBody | ReadingPane | conditional | !(mode === "columns") ? …', {
    reason: REASONS[12],
  }),
  ...answers("InboxBody | ReadingPane | conditional | (readingView) ? …", { reason: REASONS[12] }),
  ...answers(
    "InboxBody | ReadingPane | early return | if (inbox.isPending || whoAmI.isPending) return …",
    { reason: REASONS[12] }
  ),
  ...answers(
    "InboxBody | ReadingPane | early return | if (inbox.isError || viewer === undefined || place === undefined) return …",
    { reason: REASONS[12] }
  ),
  ...answers(
    "InboxBody | ReadingPane | conditional | if (shown.length === 0 && held.length === 0)",
    {
      reason: REASONS[12],
    }
  ),
  ...answers(
    "InboxBody | ReadingPane | early return | if (shown.length === 0 && held.length === 0) return …",
    { reason: REASONS[12] }
  ),
  ...answers(
    "InboxBody | ReadingPane | address prop | row={subject}",
    cells(`${HOST} · the column moves to another row, then back`)
  ),
  ...answers("InboxBody | ThreadCardRow | key | <ThreadCardRow key={id}>", { reason: REASONS[13] }),
  ...answers("InboxBody | ThreadCardRow | address prop | <ThreadCardRow {...shared}>", {
    reason: REASONS[13],
  }),
  ...answers('InboxBody | ThreadCardRow | conditional | !(row.kind === "ask") ? …', {
    reason: REASONS[13],
  }),
  ...answers('InboxBody | ThreadCardRow | early return | if (mode !== "cards") return …', {
    reason: REASONS[12],
  }),
  ...answers("InboxBody | ThreadCardRow | conditional | drawn by drawn.flatMap(…)", {
    reason: REASONS[13],
  }),
  ...answers('InboxBody | ThreadCardRow | conditional | (mode === "columns") ? …', {
    reason: REASONS[12],
  }),
  ...answers("InboxBody | ThreadCardRow | conditional | <div hidden={readingView}>", {
    reason: REASONS[12],
  }),
  ...answers(
    "InboxBody | ThreadCardRow | early return | if (inbox.isPending || whoAmI.isPending) return …",
    { reason: REASONS[12] }
  ),
  ...answers(
    "InboxBody | ThreadCardRow | early return | if (inbox.isError || viewer === undefined || place === undefined) return …",
    { reason: REASONS[12] }
  ),
  ...answers(
    "InboxBody | ThreadCardRow | conditional | if (shown.length === 0 && held.length === 0)",
    {
      reason: REASONS[12],
    }
  ),
  ...answers(
    "InboxBody | ThreadCardRow | early return | if (shown.length === 0 && held.length === 0) return …",
    { reason: REASONS[12] }
  ),
  ...answers('InboxBody | ThreadCardRow | conditional | !(mode === "columns") ? …', {
    reason: REASONS[12],
  }),
  ...answers("InboxBody | ThreadCardRow | address prop | thread={row.thread}", {
    reason: REASONS[13],
  }),
  ...answers(
    'ThreadPane | AskThreadPane | early return | if (row.kind === "agent") return …',
    cells(`${HOST} · the column moves to another row, then back`)
  ),
  ...answers(
    'ThreadPane | AskThreadPane | conditional | if (row.kind === "ask")',
    cells(`${HOST} · the column moves to another row, then back`)
  ),
  ...answers(
    "ThreadPane | AskThreadPane | address prop | thread={row}",
    cells(`${HOST} · the column moves to another row, then back`)
  ),
  ...answers(
    'ThreadBody | CommentThreadBody | early return | if (row.kind === "agent") return …',
    cells(`${HOST} · the column moves to another row, then back`)
  ),
  ...answers(
    'ThreadBody | CommentThreadBody | early return | if (row.kind === "ask") return …',
    cells(`${HOST} · the column moves to another row, then back`)
  ),
  ...answers(
    'ThreadBody | CommentThreadBody | early return | if (row.kind === "message") return …',
    cells(`${HOST} · the column moves to another row, then back`)
  ),
  ...answers(
    "ThreadBody | CommentThreadBody | conditional | if (issueKey !== undefined)",
    cells(`${HOST} · the column moves to another row, then back`)
  ),
  ...answers(
    'ThreadBody | CommentThreadBody | address prop | owner={{ issueKey, kind: "issue" }}',
    cells(`${HOST} · the column moves to another row, then back`)
  ),
  ...answers(
    "ThreadBody | CommentThreadBody | address prop | thread={row}",
    cells(`${HOST} · the column moves to another row, then back`)
  ),
  ...answers(
    "ThreadBody | CommentThreadBody | early return | if (issueKey !== undefined) return …",
    cells(`${HOST} · the column moves to another row, then back`)
  ),
  ...answers(
    "ThreadBody | CommentThreadBody | early return | if (document === undefined || artifactId === undefined) return …",
    cells(`${HOST} · the column moves to another row, then back`)
  ),
  ...answers(
    'ThreadBody | CommentThreadBody | address prop | owner={{ artifactId, kind: "artifact", project: document.project }}',
    cells(`${HOST} · the column moves to another row, then back`)
  ),
  ...answers("InboxDrawer | Inbox | early return | if (!open) return …", { reason: REASONS[12] }),
  ...answers("InboxDrawer | Inbox | address prop | filter={filter}", { reason: REASONS[12] }),
  ...answers("InboxDrawer | Inbox | address prop | pane={false}", { reason: REASONS[12] }),
  ...answers("Inbox | InboxBody | address prop | filter={filter}", { reason: REASONS[12] }),
  ...answers("Inbox | InboxBody | address prop | pane={pane}", { reason: REASONS[12] }),
  ...answers('AppShell | InboxPage | route | <Route path="/">', { reason: REASONS[12] }),
  ...answers('AppShell | InboxPage | route | <Route path="/credentials/*">', {
    reason: REASONS[12],
  }),
  ...answers("AppShell | InboxDrawer | address prop | open={inboxDrawerOpen}", {
    reason: REASONS[12],
  }),
  ...answers('InboxBody | ReadingPane | form | (mode === "columns") ? …', { reason: REASONS[13] }),
  ...answers("InboxBody | ReadingPane | form | (readingView) ? …", { reason: REASONS[13] }),
  ...answers("InboxBody | ReadingPane | form | <div className>", { reason: REASONS[13] }),
  ...answers('InboxBody | ThreadCardRow | form | (mode === "columns") ? …', {
    reason: REASONS[13],
  }),
  ...answers('InboxBody | ThreadCardRow | form | if (mode !== "cards") return …', {
    reason: REASONS[13],
  }),
  ...answers("InboxBody | ThreadCardRow | form | <div hidden={readingView}>", {
    reason: REASONS[13],
  }),
  ...answers("InboxBody | ThreadCardRow | form | <div className>", { reason: REASONS[13] }),
  ...answers("InboxBody | ThreadCardRow | form | <section className>", { reason: REASONS[13] }),
  ...answers("InboxPage | Inbox | form | <section className>", { reason: REASONS[13] }),
  ...answers("AppShell | InboxDrawer | form | open={inboxDrawerOpen}", { reason: REASONS[13] }),
  ...answers("InboxBody | - | form | (columns) ? …", { reason: REASONS[13] }),
  ...answers('InboxBody | - | form | if (mode === "columns" && named.id !== paneSelection.id)', {
    reason: REASONS[13],
  }),
  ...answers('InboxBody | - | form | (mode === "reading") ? …', { reason: REASONS[13] }),
  // The selection bar's hand-off reads the row open in the reading view below the column's width
  // (`barOpenRow`) to choose where its landing goes, as the reading mode's branch above does: it
  // draws nothing, so no box moves with it.
  ...answers("InboxBody | - | form | (barOpenRow === undefined) ? …", { reason: REASONS[13] }),
  ...answers(
    "InboxBody | - | form | (id === barOpenRow && view?.dataset.inboxReadingRow === id) ? …",
    { reason: REASONS[13] }
  ),
  ...answers("useReadingViewLanding | - | form | if (!columns)", { reason: REASONS[13] }),
  ...answers("useInboxKeymap | - | form | (keysOnShownRow) ? …", { reason: REASONS[13] }),
  ...answers('useInboxKeymap | - | form | if (mode === "columns" && focusInPane())', {
    reason: REASONS[13],
  }),
  ...answers("useInboxKeymap | - | form | (id === undefined) ? …", { reason: REASONS[13] }),
  ...answers('useInboxKeymap | - | form | (mode === "list" && focusedRow() !== null) ? …', {
    reason: REASONS[13],
  }),
  ...answers('useInboxKeymap | - | form | (row?.kind === "ask") ? …', { reason: REASONS[13] }),
  ...answers(
    "useInboxKeymap | - | form | if (stop === null && lost && keysOnShownRow && presented.current.length > 0) return …",
    { reason: REASONS[13] }
  ),
  ...answers(
    "useInboxKeymap | - | form | if (keysOnShownRow && active === paneScroller()) return …",
    {
      reason: REASONS[13],
    }
  ),
  ...answers("useInboxKeymap | - | form | if (keysOnShownRow && !outsideAskCard()) return …", {
    reason: REASONS[13],
  }),
  ...answers("useInboxKeymap | - | form | if (!keysOnShownRow) return …", { reason: REASONS[13] }),
  ...answers('useInboxKeymap | - | form | if (mode === "list") return …', { reason: REASONS[13] }),
  ...answers("useInboxKeymap | - | form | if (box === null && keysOnShownRow) return …", {
    reason: REASONS[13],
  }),
  ...answers('useInboxKeymap | - | form | (mode === "list") ? …', { reason: REASONS[13] }),
  ...answers(
    'useInboxKeymap | - | form | if (mode === "list" && handRow()?.kind === "thread") return …',
    { reason: REASONS[13] }
  ),
  ...answers(
    'useInboxKeymap | - | form | if (mode === "list" && inHand(ITEM_COPY_REF_SELECTOR) === null) return …',
    { reason: REASONS[13] }
  ),
  ...answers('useInboxKeymap | - | form | if (mode === "columns" && focusInPane()) return …', {
    reason: REASONS[13],
  }),
  ...answers('useInboxKeymap | - | form | (mode === "reading") ? …', { reason: REASONS[13] }),
  ...answers('useInboxKeymap | - | form | if (mode === "reading" && focusInPane()) return …', {
    reason: REASONS[13],
  }),
  ...answers("usePaneSelection | - | form | (began === undefined && shownRow && !columns) ? …", {
    reason: REASONS[13],
  }),
  ...answers("usePaneSelection | - | form | if (read !== undefined && !acted.has(read)) return …", {
    reason: REASONS[13],
  }),
  ...answers("InboxBody | - | form | if (!rowLeft) return …", { reason: REASONS[13] }),
  ...answers("InboxBody | - | form | if (columns) return …", { reason: REASONS[13] }),
  ...answers("usePaneSelection | - | form | (columns || entry.inboxDefaultPick !== true) ? …", {
    reason: REASONS[13],
  }),
  ...answers(
    "usePaneSelection | - | form | if (!columns || selectedId === undefined || selectedId === handed) return …",
    { reason: REASONS[13] }
  ),
  ...answers(
    "usePaneSelection | - | form | if (columns || (pane && handOff.selected !== undefined)) return …",
    { reason: REASONS[13] }
  ),
  ...answers("usePaneSelection | - | form | if (!pane || openId === undefined) return …", {
    reason: REASONS[13],
  }),
  ...answers("usePaneSelection | - | form | (columns) ? …", { reason: REASONS[13] }),
  ...answers("usePaneSelection | - | form | (id === undefined && !columns) ? …", {
    reason: REASONS[13],
  }),
  ...answers("usePaneSelection | - | form | if (columns)", { reason: REASONS[13] }),
  ...answers(
    "usePaneSelection | - | form | if (!pane || columns || left === undefined || selectedId !== undefined) return …",
    { reason: REASONS[13] }
  ),
  ...answers(
    "usePaneSelection | - | form | if (subject === undefined || rowId(subject.row) !== openId) return …",
    { reason: REASONS[13] }
  ),
  ...answers("usePaneSelection | - | form | (kept) ? …", { reason: REASONS[13] }),
  ...answers(
    "AgentMessageComposer | AgentMessageComposer <MentionComposer mutationKey={sendKey}> | lifetime | <MentionComposer mutationKey={sendKey}>",
    cells(`${HOST} · the page reloaded, then back · an Agent row's box`)
  ),
  ...answers(
    "CommentThreadBody | CommentThreadBody <MentionComposer mutationKey={threadSendKey(thread.thread_id)}> | lifetime | <MentionComposer mutationKey={threadSendKey(thread.thread_id)}>",
    cells(`${HOST} · the page reloaded, then back`)
  ),
];
