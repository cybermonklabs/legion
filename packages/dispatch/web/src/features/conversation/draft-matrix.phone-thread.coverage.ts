/**
 * The draft matrix's coverage list, phone thread (INFRA-1503): each composer census item that
 * `draft-matrix.phone-thread.test.tsx` answers for, held to one of its tests or to one of the
 * numbered reasons of its header, as `__tests__/composer-census/coverage.ts` says.
 */
import { answers, type CoverageEntry, cells } from "../../__tests__/composer-census/coverage";

/** The host as the matrix file's titles name it. */
const HOST = "phone thread";

/** The numbered reasons of the matrix file's header. */
const REASONS = {
  1: "1. Every Reply the thread composer answers is redirected onto the comment its refusal answers (`redirectedReplyTarget`), so the host never moves it while a refusal is held.",
  2: "2. The thread view draws its comment forced open with no Collapse thread (`forceExpanded`), so Back, the first row, is its one way out.",
  3: "3. The thread composer has no anchor, so it draws no Close (asserted absent below).",
  4: "4. The issue picker is an Agents row's (`AgentMessageComposer`), and the thread view draws no Agents row's composer.",
  5: "5. `draft-matrix.issue-page.test.tsx` makes these cells, with the issue page drawn around the Conversation: another issue's address draws the page afresh (`IssueDetail` keyed by `route.key`), and another tab hides the Conversation's panel and keeps it drawn. The composer's `owner` and send name follow `issueKey`, which only that remount changes.",
  6: "6. Another comment's thread opened with Expand thread, while the composer answers Comment A, hides the composer (`hidden={!phoneReplyTargetsThread}`) and keeps it mounted at A's address, so nothing it holds moves; A's Reply shows it again, as the first row's return does.",
  7: "7. The Conversation draws a loading line or an error in place of the list while its reads are out or failed (`loading`, `failed`), which unmounts the thread composer as leaving the issue does, and the composer drawn when they land again is the one the issue-left row draws on return.",
  8: "8. A comment link opens the thread view as it lands (`ConversationTab`'s focus effect), as the comment's Reply opens it here, and a decision landing on the comment closes the view as Back does (`phone.hide`), the first row's move; the composer stays mounted while its reply is set (`phone.replyTo`).",
  9: "9. Widening the page past the phone layout closes the view as Back does, unless a send from it is out, which keeps the view open at any width until Back (`usePhoneThread`'s `outlived`); the composer stays mounted while its reply is set, whatever the width. The Jump to latest pill and the read mark's ref on the newest card move no composer, and the app shell's forms are the sign-in file's.",
} as const;

export const coverage: readonly CoverageEntry[] = [
  ...answers(
    "ConversationTab | ConversationTab <MentionComposer mutationKey={sendKey}> | address prop | intake={phone.dockedIntake}",
    cells(`${HOST} · Cancel reply`)
  ),
  ...answers(
    'ConversationTab | ConversationTab <MentionComposer mutationKey={phone.composerKey}> | address prop | owner={{ issueKey, kind: "issue" }}',
    { reason: REASONS[5] }
  ),
  ...answers(
    "ConversationTab | ConversationTab <MentionComposer mutationKey={phone.composerKey}> | early return | if (loading) return …",
    { reason: REASONS[7] }
  ),
  ...answers(
    "ConversationTab | ConversationTab <MentionComposer mutationKey={phone.composerKey}> | early return | if (failed) return …",
    { reason: REASONS[7] }
  ),
  ...answers(
    "ConversationTab | ConversationTab <MentionComposer mutationKey={phone.composerKey}> | address prop | mutationKey={phone.composerKey}",
    {
      row: `${HOST} · Close, then Discard draft? · cannot make: the thread composer draws no Close`,
    },
    { reason: REASONS[3] },
    { reason: REASONS[4] },
    { reason: REASONS[5] }
  ),
  ...answers(
    "ConversationTab | ConversationTab <MentionComposer mutationKey={phone.composerKey}> | address prop | replyTo={phone.replyTo}",
    cells(`${HOST} · Back, then back`),
    cells(`${HOST} · Reply on another comment, then back`),
    cells(`${HOST} · Cancel reply`),
    cells(`${HOST} · Escape in the field`),
    { reason: REASONS[1] }
  ),
  ...answers(
    "ConversationTab | ConversationTab <MentionComposer mutationKey={phone.composerKey}> | conditional | <div hidden={!phoneReplyTargetsThread}>",
    { reason: REASONS[6] }
  ),
  ...answers(
    "ConversationTab | ConversationTab <MentionComposer mutationKey={phone.composerKey}> | conditional | !(phone.replyTo === null) ? …",
    cells(`${HOST} · Cancel reply`),
    { reason: REASONS[8] }
  ),
  ...answers(
    "ConversationTab | ConversationTab <MentionComposer mutationKey={phone.composerKey}> | conditional | <section hidden={phoneThread === undefined}>",
    cells(`${HOST} · Back, then back`)
  ),
  ...answers(
    "ConversationTab | ConversationTab <MentionComposer mutationKey={phone.composerKey}> | conditional | !(phoneThread === undefined && phone.replyTo === null) ? …",
    cells(`${HOST} · Back, then back`),
    { reason: REASONS[9] }
  ),
  ...answers("ConversationTab | CommentTurn | address prop | forceExpanded", {
    reason: REASONS[2],
  }),
  ...answers(
    "ConversationTab | ConversationTab <MentionComposer mutationKey={phone.composerKey}> | form | <div hidden={!phoneReplyTargetsThread}>",
    { reason: REASONS[6] }
  ),
  ...answers(
    "ConversationTab | ConversationTab <MentionComposer mutationKey={phone.composerKey}> | form | <section ref>",
    cells(`${HOST} · Back, then back`)
  ),
  ...answers(
    "ConversationTab | ConversationTab <MentionComposer mutationKey={phone.composerKey}> | form | <section hidden={phoneThread === undefined}>",
    cells(`${HOST} · Back, then back`)
  ),
  ...answers(
    "ConversationTab | ConversationTab <MentionComposer mutationKey={phone.composerKey}> | form | !(phoneThread === undefined && phone.replyTo === null) ? …",
    { reason: REASONS[9] }
  ),
  ...answers(
    "ConversationTab | CommentTurn | form | <section ref>",
    cells(`${HOST} · Back, then back`)
  ),
  ...answers(
    "ConversationTab | CommentTurn | form | <section hidden={phoneThread === undefined}>",
    cells(`${HOST} · Back, then back`)
  ),
  ...answers(
    "ConversationTab | CommentTurn | form | !(phoneThread === undefined && phone.replyTo === null) ? …",
    { reason: REASONS[9] }
  ),
  ...answers("ConversationTab | - | form | (phone.id === undefined) ? …", { reason: REASONS[9] }),
  ...answers("ConversationTab | - | form | if (phone.id === id)", { reason: REASONS[8] }),
  ...answers("ConversationTab | - | form | if (isPhoneViewport)", { reason: REASONS[8] }),
  ...answers(
    "ConversationTab | - | form | (visible && phoneThread === undefined && !follow.atTop && shown.length > 0) ? …",
    { reason: REASONS[9] }
  ),
  ...answers("usePhoneThread | - | form | if (isPhoneViewport || outlived) return …", {
    reason: REASONS[9],
  }),
  ...answers(
    "usePhoneThread | - | form | (isPhoneViewport || outlived || cardReplySending || replySending) ? …",
    { reason: REASONS[9] }
  ),
  ...answers(
    "ConversationTab | ConversationTab <MentionComposer mutationKey={phone.composerKey}> | lifetime | <MentionComposer mutationKey={phone.composerKey}>",
    cells(`${HOST} · the page reloaded, then back`)
  ),
];
