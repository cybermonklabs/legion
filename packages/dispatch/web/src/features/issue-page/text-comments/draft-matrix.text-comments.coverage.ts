/**
 * The draft matrix's coverage list, comments on text (INFRA-1503): each composer census item that
 * `draft-matrix.text-comments.test.tsx` answers for, held to one of its tests or to one of the
 * numbered reasons below, as `__tests__/composer-census/coverage.ts` says.
 */
import { answers, type CoverageEntry, cells } from "../../../__tests__/composer-census/coverage";

/** The hosts as the matrix file's titles name them. */
const COLUMN = "selection composer";
const SHEET = "selection composer, below 1024 px";
const REPLY = "text thread reply";
const REPLY_SHEET = "text thread reply, below 1024 px";

/** The moves as the matrix file's titles name them. */
const ESCAPED = "Escape, then opened again";
const OTHER_WORDS = "opened on other words, then back";
const NARROWED = "the window narrows below 1024 px, then widens";
const VIEW = "another view, then back";
const ISSUE = "issue left, then back";
const RELOADED = "the page reloaded, then back";

/** The tests of one row of the matrix file: `host` making `move`, in each state. */
const row = (host: string, move: string) => cells(`${host} · ${move}`);

/** The selection composer's mount, as the census names it. */
const MOUNT =
  'SelectionComposer <MentionComposer mutationKey={["text-comment", issue.key, wordsKeyOf(words)]}>';

/** The numbered reasons, said here alone, which the matrix file's header points to. */
const REASONS = {
  1: "1. `IssueOutline` draws a failed or a loading line in place of its slots while the read of the issue fails or is pending (`issue.isError`, `issue.data === undefined`), and a sheet's row reads no issue then (`useStageIssue`), which ends the column, the sheet and every composer of this part as leaving the issue does; what the page draws again on recovery is what the issue-left rows draw again on return.",
  2: "2. `IssuePage` draws today's page in place of the outline when the reader's stored page choice says so (`useIssuePageChoice`), which ends every composer of this part as leaving the issue does; choosing the outline again draws the page afresh, as the issue-left rows' returns do.",
  3: "3. An address that parses as no issue draws the not-found page in place of `IssueOutline` (`route === undefined`), so no composer of this part is drawn there, and going there from an issue is leaving the issue, the issue-left rows.",
  4: "4. `onLand` is what the composer's own send (`useSelectionSend`) calls once Dispatch takes it: it closes the composer, its draft sent, and holds where the page lands. At this base the composer's Send is `MentionComposer`'s own path, which never calls it (the composer takes `useSelectionSend` at gate G5), and the function changes only with the issue and the composer's state, so it moves no address and ends no mount.",
  5: "5. The composer's owner is the issue of the node its words are in (`useSelectionSend`'s `write`, else the page's issue): the node is drawn when its words are selected, the composer reads it then and keeps it while the view no longer draws the node (`last`), and a node's issue never changes, so the owner never moves while the composer is mounted; other words are another composer, the other-words rows.",
  6: "6. The address's search names the item an address of R8 lands on (`?node=`), which the column reads only to give that item the focus and mark it in its thread's card (`outlineItemRoute`, `currentReply`); it moves no composer's address and ends no mount.",
  7: "7. A sheet's `node` and `parent` are the node of the row it stands in and the node that row is drawn under, which the row site hands it once for its row: a node drawn under several parents has its threads and its composer in its stored parent's row alone, and another node or another parent is another row with a sheet of its own, so neither moves a composer within one. No row site mounts the sheet at this base (gate G6).",
  8: "8. The reply an address names is marked in its thread's card (`currentReplyId`, `aria-current`) and takes the focus; that moves no address and ends no mount.",
  9: "9. A thread on text's card is always expanded (`expanded`, with a toggle that does nothing, R4), so its reply is drawn for as long as its card is.",
  10: "10. A card's thread is the thread its card is keyed by: a read again gives it a new reply, a resolution or an edit, never another root, and its reply always answers its thread's root under its thread's reply send name (`marginReplySendKey`), so it moves no address.",
  11: "11. A thread's card is drawn once per thread of the nodes the view draws, keyed by its thread's root, which is the thread's own name and never changes; a thread that leaves the list unmounts its card, which the another-view rows make, and its reply is held under its thread's reply send name.",
  12: "12. `host` holds what every card of one column or sheet shares (`useThreadCardHost`): the issue's writes on its comments, the edit in progress and the reader. Its owner is the issue, which changes only with the issue, the issue-left rows; the rest moves no address and ends no mount.",
} as const;

export const coverage: readonly CoverageEntry[] = [
  // The selection composer's own lines.
  ...answers(
    `SelectionComposer | ${MOUNT} | address prop | mutationKey={["text-comment", issue.key, wordsKeyOf(words)]}`,
    row(COLUMN, OTHER_WORDS),
    row(SHEET, OTHER_WORDS),
    row(COLUMN, ISSUE),
    row(SHEET, ISSUE)
  ),
  ...answers(
    `SelectionComposer | ${MOUNT} | address prop | owner={{ issueKey: write?.issue ?? issue.key, kind: "issue" }}`,
    { reason: REASONS[5] }
  ),
  ...answers(
    `SelectionComposer | ${MOUNT} | lifetime | <MentionComposer mutationKey={["text-comment", issue.key, wordsKeyOf(words)]}>`,
    row(COLUMN, RELOADED)
  ),
  // The column draws the composer.
  ...answers(
    "Column | SelectionComposer | conditional | !(request === undefined) ? …",
    row(COLUMN, ESCAPED)
  ),
  ...answers(
    "Column | SelectionComposer | key | <SelectionComposer key={wordsKeyOf(request)}>",
    row(COLUMN, OTHER_WORDS)
  ),
  ...answers("Column | SelectionComposer | address prop | issue={issue}", row(COLUMN, ISSUE)),
  ...answers("Column | SelectionComposer | address prop | onLand={composer.land}", {
    reason: REASONS[4],
  }),
  ...answers("Column | SelectionComposer | address prop | view={view}", row(COLUMN, VIEW)),
  ...answers(
    "Column | SelectionComposer | address prop | words={request}",
    row(COLUMN, OTHER_WORDS)
  ),
  // The sheet draws the composer.
  ...answers(
    "SheetBody | SelectionComposer | conditional | !(request === undefined) ? …",
    row(SHEET, ESCAPED)
  ),
  ...answers(
    "SheetBody | SelectionComposer | key | <SelectionComposer key={wordsKeyOf(request)}>",
    row(SHEET, OTHER_WORDS)
  ),
  ...answers("SheetBody | SelectionComposer | address prop | issue={issue}", row(SHEET, ISSUE)),
  ...answers("SheetBody | SelectionComposer | address prop | onLand={composer.land}", {
    reason: REASONS[4],
  }),
  ...answers("SheetBody | SelectionComposer | address prop | view={view}", row(SHEET, VIEW)),
  ...answers(
    "SheetBody | SelectionComposer | address prop | words={request}",
    row(SHEET, OTHER_WORDS)
  ),
  // The cards of the threads, whose reply is `ThreadCard`'s.
  ...answers(
    "TextThreadCard | ThreadCard | address prop | <ThreadCard {...(currentReplyId === undefined ? {} : { currentReplyId })}>",
    { reason: REASONS[8] }
  ),
  ...answers("TextThreadCard | ThreadCard | address prop | expanded", { reason: REASONS[9] }),
  ...answers("TextThreadCard | ThreadCard | address prop | owner={host.owner}", row(REPLY, ISSUE)),
  ...answers(
    "TextThreadCard | ThreadCard | address prop | replyMutationKey={marginReplySendKey(card.key)}",
    row(REPLY, NARROWED),
    row(REPLY, VIEW),
    row(REPLY_SHEET, VIEW)
  ),
  ...answers("TextThreadCard | ThreadCard | address prop | thread={card}", { reason: REASONS[10] }),
  ...answers(
    "Column | TextThreadCard | address prop | <TextThreadCard {...(currentReply?.thread === thread.thread.key ? { currentReplyId: currentReply.id } : {})}>",
    { reason: REASONS[8] }
  ),
  ...answers("Column | TextThreadCard | key | <div key={thread.thread.key}>", {
    reason: REASONS[11],
  }),
  ...answers("Column | TextThreadCard | conditional | drawn by placed.map(…)", row(REPLY, VIEW)),
  ...answers("Column | TextThreadCard | address prop | host={host}", { reason: REASONS[12] }),
  ...answers("Column | TextThreadCard | address prop | thread={thread}", { reason: REASONS[10] }),
  ...answers("SheetBody | TextThreadCard | key | <TextThreadCard key={thread.thread.key}>", {
    reason: REASONS[11],
  }),
  ...answers(
    "SheetBody | TextThreadCard | conditional | drawn by threads.map(…)",
    row(REPLY_SHEET, VIEW)
  ),
  ...answers("SheetBody | TextThreadCard | address prop | host={host}", { reason: REASONS[12] }),
  ...answers("SheetBody | TextThreadCard | address prop | thread={thread}", {
    reason: REASONS[10],
  }),
  // The column, from 1024 px.
  ...answers(
    "TextMargin | Column | early return | if (!wide || (view.threads.size === 0 && composer.request === undefined)) return …",
    row(COLUMN, NARROWED),
    row(COLUMN, ESCAPED),
    row(REPLY, NARROWED)
  ),
  ...answers(
    "TextMargin | Column | form | if (!wide || (view.threads.size === 0 && composer.request === undefined)) return …",
    row(COLUMN, NARROWED),
    row(REPLY, NARROWED)
  ),
  ...answers(
    "TextMargin | Column | address prop | composer={composer}",
    row(COLUMN, ESCAPED),
    row(COLUMN, OTHER_WORDS)
  ),
  ...answers(
    "TextMargin | Column | address prop | issue={issue}",
    row(COLUMN, ISSUE),
    row(REPLY, ISSUE)
  ),
  ...answers(
    "TextMargin | Column | address prop | view={view}",
    row(COLUMN, VIEW),
    row(REPLY, VIEW)
  ),
  ...answers(
    "TextMargin | Column | address prop | address={address}",
    row(COLUMN, VIEW),
    row(REPLY, VIEW)
  ),
  ...answers(
    "TextMargin | Column | address prop | route={route}",
    row(COLUMN, VIEW),
    row(REPLY, VIEW),
    row(COLUMN, ISSUE),
    row(REPLY, ISSUE)
  ),
  ...answers("TextMargin | Column | address prop | search={search}", { reason: REASONS[6] }),
  // The sheet, below 1024 px.
  ...answers(
    "Sheet | SheetBody | early return | if (threads.length === 0 && request === undefined) return …",
    row(SHEET, ESCAPED),
    row(REPLY_SHEET, VIEW)
  ),
  ...answers(
    "Sheet | SheetBody | address prop | composer={composer}",
    row(SHEET, ESCAPED),
    row(SHEET, OTHER_WORDS)
  ),
  ...answers("Sheet | SheetBody | address prop | issue={issue}", row(SHEET, ISSUE)),
  ...answers(
    "Sheet | SheetBody | address prop | request={request}",
    row(SHEET, ESCAPED),
    row(SHEET, OTHER_WORDS)
  ),
  ...answers(
    "Sheet | SheetBody | address prop | view={view}",
    row(SHEET, VIEW),
    row(REPLY_SHEET, VIEW)
  ),
  ...answers("Sheet | SheetBody | address prop | threads={threads}", row(REPLY_SHEET, VIEW)),
  ...answers(
    "NarrowSheet | Sheet | early return | if (stage === undefined) return …",
    row(SHEET, ISSUE),
    { reason: REASONS[1] }
  ),
  ...answers("NarrowSheet | Sheet | address prop | issue={stage.issue}", row(SHEET, ISSUE)),
  ...answers(
    "NarrowSheet | Sheet | address prop | preset={stage.preset}",
    row(SHEET, VIEW),
    row(REPLY_SHEET, VIEW)
  ),
  ...answers("NarrowSheet | Sheet | address prop | node={node}", { reason: REASONS[7] }),
  ...answers(
    "NarrowSheet | Sheet | address prop | <Sheet {...(parent === undefined ? {} : { parent })}>",
    { reason: REASONS[7] }
  ),
  ...answers(
    "NodeThreads | NarrowSheet | early return | if (wide) return …",
    row(COLUMN, NARROWED),
    row(REPLY, NARROWED)
  ),
  ...answers(
    "NodeThreads | NarrowSheet | form | if (wide) return …",
    row(COLUMN, NARROWED),
    row(REPLY, NARROWED)
  ),
  ...answers("NodeThreads | NarrowSheet | address prop | node={node}", { reason: REASONS[7] }),
  ...answers(
    "NodeThreads | NarrowSheet | address prop | <NarrowSheet {...(parent === undefined ? {} : { parent })}>",
    { reason: REASONS[7] }
  ),
  // The page above the column, up to its root.
  ...answers(
    "IssueOutline | TextMargin | address prop | <TextMargin {...props}>",
    row(COLUMN, VIEW),
    row(COLUMN, ISSUE),
    row(REPLY, VIEW),
    row(REPLY, ISSUE)
  ),
  ...answers("IssueOutline | TextMargin | early return | if (issue.isError) return …", {
    reason: REASONS[1],
  }),
  ...answers("IssueOutline | TextMargin | early return | if (issue.data === undefined) return …", {
    reason: REASONS[1],
  }),
  ...answers(
    "ProvidersSlot | TextMargin | key | <FocusedNodeProvider key={issue.key}>",
    row(COLUMN, ISSUE),
    row(REPLY, ISSUE)
  ),
  ...answers(
    "IssueOutlinePage | IssueOutline | key | <IssueOutline key={route.key}>",
    row(COLUMN, ISSUE),
    row(SHEET, ISSUE),
    row(REPLY, ISSUE)
  ),
  ...answers(
    "IssueOutlinePage | IssueOutline | address prop | route={route}",
    row(COLUMN, VIEW),
    row(COLUMN, ISSUE),
    row(REPLY, VIEW),
    row(REPLY, ISSUE)
  ),
  ...answers("IssueOutlinePage | IssueOutline | conditional | !(route === undefined) ? …", {
    reason: REASONS[3],
  }),
  ...answers(
    'IssuePage | IssueOutlinePage | conditional | (useIssuePageChoice()[0] === "outline") ? …',
    { reason: REASONS[2] }
  ),
];
