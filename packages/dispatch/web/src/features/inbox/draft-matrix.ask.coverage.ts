/**
 * The draft matrix's coverage list, ask card (INFRA-1503): each composer census item that
 * `draft-matrix.ask.test.tsx` answers for, held to one of its tests or to one of the numbered
 * reasons below, as `__tests__/composer-census/coverage.ts` says.
 */
import { answers, type CoverageEntry, cells } from "../../__tests__/composer-census/coverage";

/** The hosts as the matrix file's titles name them. */
const ANSWER = "Ask answer";
const REPLY = "Ask reply";
const COLLAPSED = "Ask reply, collapsed";

/** Each cell writes its draft typed (the file's header). */
const TYPED = ["typed"];

/** The numbered reasons, said here alone, which the matrix file's header points to. */
const REASONS = {
  1: "1. Which end of the card the answer field and the reply composer sit at (`answerFirst`, `composerFirst`) is the reader's thread order (`useThreadOrder`), which the card reads when it is drawn and again only when who is signed in changes (`useUserPreference`), and never sets, so it never moves a field that is on screen; the shell draws no card before it knows who is signed in, and another person's sign-in forgets every ask draft (`DraftStore.signedIn`).",
  2: '2. Each host draws its cards in one variant and with one thread, literals of its own (`variant="compact"` and `thread="collapsed"` in the Conversation and a decision block, `variant="compact"` in the margin, `variant="full"`, `thread="inline"` or the defaults elsewhere), so no card switches between its full and its compact form, or moves its reply composer between its inline thread and its collapsed one (the card draws `AskThreadDisclosure` or `AskThread` by its `thread` alone); both composers hold the ask\'s one draft (`useReplyDraft(ask.id)`).',
  3: "3. A compact card draws its answer field only while the answer needs it or the reader has opened the note disclosure (`ownWordsShown`: Other ticked, a reason the choice demands, words in the draft, or `ownWordsOpen`); a full card always draws it. Folding the note takes the field off the screen while the card stays drawn, and the draft is the ask's (`useAnswerDraft(ask.id)` in `useAskAnswerForm`), as the upload behind it is the card's rather than the field's (`AskAnswerArea`), so the field the disclosure opens again shows the draft, as the field of a card ended and drawn again does.",
  4: "4. The card draws the ask's record in place of its form once the ask is settled (`completed`). An answer the server records for the reader ends the draft with the form, since it is no longer unsent (`answer-drafts.ts`); a question settled otherwise draws the reader's text under the record, read-only (`unsentAnswer`; `AskCard.test.tsx`, \"a question closed under the reader keeps their unsent answer, out of the form\"), and the draft stays the ask's, so a form drawn for the ask again holds it. A change of the reader's recorded answer (`changing`) draws the form over that answer's own text, which is the form's and no draft (`changeAnswer` in `useAskAnswerForm.ts`).",
  5: '5. An open ask takes no reply composer (`takesReply`): its card\'s one composer answers it. A reply the reader had not sent under a settled ask that opens again is drawn read-only in the composer\'s place, in either thread (`useUnsentReply`; `AskCard.test.tsx`, "a reply typed under a retracted ask that opens again stays on screen in the inline thread" and "… in the collapsed thread"), and the draft stays the ask\'s, so the composer drawn once the ask is settled again holds it. The collapsed thread draws nothing only for an open ask with no replies and no such reply (`replyCount === 0 && !takesReply(ask) && unsentReply === ""`), where there is no composer and no draft to show.',
  6: "6. The thread is its host's own read of its ask's thread (`useAskThreadRead`: the card's `threadQuery`, the approval request's `thread` in `ApprovalExchange`), of the same ask as the host. It decides which replies the thread shows and folds (`threadLayout`), and, in the collapsed thread, whether the disclosure draws a retry control in place of the replies and the composer while the read fails (`thread.isError`): that takes the composer off the screen as closing Write a reply does, and the composer drawn when the read lands again holds the ask's draft, as the composer Write a reply opens again does.",
  7: "7. A card draws its thread under its record while the ask is settled, and under its form while the ask is open or the reader is changing their recorded answer (`completed`). An open ask's thread holds no composer (5), so a thread holding one moves between the two only at the reader's own Change answer and its end, which draws the thread afresh, its composer with it, for the same ask: the composer holds the ask's draft, as the composer of a card ended and drawn again does.",
  8: "8. Which replies the reader has not read (`unread`) decides only which replies the thread folds away (`threadLayout`): it never draws, ends, moves or keys a composer, whose place is the order's alone (`composerFirst(order)`, (1)).",
  9: "9. `AskThread` draws no composer where it is passed `showComposer={false}`, which only the collapsed thread passes, drawing `AskThread` for its replies alone: the collapsed thread's composer is the disclosure's own, the `Ask reply, collapsed` rows', and every other `AskThread` takes the default, `true`. So nothing that draws, ends or moves the collapsed thread's `AskThread` (its replies being there, its read, its order, its ask) draws, ends or moves a composer.",
  10: "10. The Inbox reading pane's approval request draws its ask's inline thread alone (`AskThread`), with no card (`ApprovalExchange` in `ReadingPane.tsx`): its reply composer standing under the open ask too (`repliesWhileOpen`, a literal of its own, which no card passes), from its own read of the ask's thread (6), in the reader's order (1). Its column keys it by its request's ask (`<ApprovalExchange key={ask.id}>` in `ApprovalBody`), so it is never drawn for another ask, and the ask the thread stands under (`current`) is that ask, as the read reads it once settled: its composer holds the ask's one draft (`useReplyDraft(ask.id)`), as every other composer of the ask does, and the census follows it no further up (`APPROVAL_EXCHANGE` in `__tests__/composer-census/trees.ts`), as it follows a card no further.",
} as const;

/** The answer field's mount, and the reply composer's in each thread. */
const ANSWER_FIELD = "AskCard | AskCard <MentionField askAnswer={true}>";
const INLINE_COMPOSER = "AskThread | AskThread <AskReplyComposer>";
const COLLAPSED_COMPOSER = "AskThreadDisclosure | AskThreadDisclosure <AskReplyComposer>";

export const coverage: readonly CoverageEntry[] = [
  // The answer field, in its card.
  ...answers(
    `${ANSWER_FIELD} | address prop | useAskAnswerForm({ ask })`,
    cells(`${ANSWER} · the card drawn for another ask, then back`, TYPED)
  ),
  ...answers(
    `${ANSWER_FIELD} | address prop | {...props}`,
    cells(`${ANSWER} · the card drawn for another ask, then back`, TYPED)
  ),
  ...answers(
    `${ANSWER_FIELD} | conditional | (ownWordsShown) ? …`,
    { reason: REASONS[3] },
    cells(`${ANSWER} · the card ended, then drawn again`, TYPED)
  ),
  ...answers(
    `${ANSWER_FIELD} | conditional | (isCompact) ? …`,
    { reason: REASONS[2] },
    { reason: REASONS[3] }
  ),
  ...answers(`${ANSWER_FIELD} | conditional | !(isCompact) ? …`, { reason: REASONS[2] }),
  ...answers(`${ANSWER_FIELD} | early return | if (completed !== null) return …`, {
    reason: REASONS[4],
  }),
  ...answers(
    `${ANSWER_FIELD} | lifetime | <MentionField askAnswer={true}>`,
    cells(`${ANSWER} · the page reloaded, then back`, TYPED)
  ),

  // The reply composer, in the card's inline thread.
  ...answers(
    `${INLINE_COMPOSER} | address prop | ask={ask}`,
    cells(`${REPLY} · the card drawn for another ask, then back`, TYPED)
  ),
  ...answers(
    `${INLINE_COMPOSER} | conditional | (repliesWhileOpen || takesReply(ask)) ? …`,
    { reason: REASONS[5] },
    { reason: REASONS[10] }
  ),
  ...answers(`${INLINE_COMPOSER} | conditional | !(!showComposer) ? …`, { reason: REASONS[9] }),
  ...answers(`${INLINE_COMPOSER} | conditional | (composerFirst) ? …`, { reason: REASONS[1] }),
  ...answers(`${INLINE_COMPOSER} | conditional | !(composerFirst) ? …`, { reason: REASONS[1] }),
  ...answers(
    `${INLINE_COMPOSER} | lifetime | <AskReplyComposer>`,
    cells(`${REPLY} · the page reloaded, then back`, TYPED)
  ),

  // The reply composer, behind the collapsed thread's Write a reply.
  ...answers(
    `${COLLAPSED_COMPOSER} | address prop | ask={ask}`,
    cells(`${COLLAPSED} · the card drawn for another ask, then back`, TYPED)
  ),
  ...answers(
    `${COLLAPSED_COMPOSER} | conditional | (composerOpen) ? …`,
    cells(`${COLLAPSED} · Write a reply closed, then opened`, TYPED)
  ),
  ...answers(`${COLLAPSED_COMPOSER} | conditional | !(!takesReply(ask)) ? …`, {
    reason: REASONS[5],
  }),
  ...answers(`${COLLAPSED_COMPOSER} | conditional | (composerFirst(order)) ? …`, {
    reason: REASONS[1],
  }),
  ...answers(`${COLLAPSED_COMPOSER} | conditional | !(composerFirst(order)) ? …`, {
    reason: REASONS[1],
  }),
  ...answers(
    `${COLLAPSED_COMPOSER} | early return | if (thread.isError) return …`,
    { reason: REASONS[6] },
    cells(`${COLLAPSED} · Write a reply closed, then opened`, TYPED)
  ),
  ...answers(
    `${COLLAPSED_COMPOSER} | early return | if (replyCount === 0 && !takesReply(ask) && unsentReply === "") return …`,
    { reason: REASONS[5] }
  ),
  ...answers(
    `${COLLAPSED_COMPOSER} | lifetime | <AskReplyComposer>`,
    cells(`${COLLAPSED} · the page reloaded, then back`, TYPED)
  ),

  // Where the card draws its inline thread.
  ...answers('AskCard | AskThread | conditional | !(thread === "collapsed") ? …', {
    reason: REASONS[2],
  }),
  ...answers(
    "AskCard | AskThread | conditional | if (completed !== null)",
    { reason: REASONS[7] },
    cells(`${REPLY} · the card ended, then drawn again`, TYPED)
  ),
  ...answers(
    "AskCard | AskThread | early return | if (completed !== null) return …",
    { reason: REASONS[7] },
    cells(`${REPLY} · the card ended, then drawn again`, TYPED)
  ),
  ...answers("AskCard | AskThread | conditional | (answerFirst) ? …", { reason: REASONS[1] }),
  ...answers("AskCard | AskThread | conditional | !(answerFirst) ? …", { reason: REASONS[1] }),
  ...answers(
    "AskCard | AskThread | address prop | ask={currentAsk}",
    cells(`${REPLY} · the card drawn for another ask, then back`, TYPED)
  ),
  ...answers("AskCard | AskThread | address prop | order={threadOrder}", { reason: REASONS[1] }),
  ...answers("AskCard | AskThread | address prop | thread={threadQuery}", {
    reason: REASONS[6],
  }),
  ...answers("AskCard | AskThread | address prop | unread={unreadReplies}", {
    reason: REASONS[8],
  }),

  // Where the card draws its collapsed thread.
  ...answers('AskCard | AskThreadDisclosure | conditional | (thread === "collapsed") ? …', {
    reason: REASONS[2],
  }),
  ...answers(
    "AskCard | AskThreadDisclosure | conditional | if (completed !== null)",
    { reason: REASONS[7] },
    cells(`${COLLAPSED} · the card ended, then drawn again`, TYPED)
  ),
  ...answers(
    "AskCard | AskThreadDisclosure | early return | if (completed !== null) return …",
    { reason: REASONS[7] },
    cells(`${COLLAPSED} · the card ended, then drawn again`, TYPED)
  ),
  ...answers("AskCard | AskThreadDisclosure | conditional | (answerFirst) ? …", {
    reason: REASONS[1],
  }),
  ...answers("AskCard | AskThreadDisclosure | conditional | !(answerFirst) ? …", {
    reason: REASONS[1],
  }),
  ...answers(
    "AskCard | AskThreadDisclosure | address prop | ask={currentAsk}",
    cells(`${COLLAPSED} · the card drawn for another ask, then back`, TYPED)
  ),
  ...answers("AskCard | AskThreadDisclosure | address prop | order={threadOrder}", {
    reason: REASONS[1],
  }),
  ...answers("AskCard | AskThreadDisclosure | address prop | thread={threadQuery}", {
    reason: REASONS[6],
  }),
  ...answers("AskCard | AskThreadDisclosure | address prop | unread={unreadReplies}", {
    reason: REASONS[8],
  }),

  // Where the reading pane's approval request draws the inline thread, with no card.
  ...answers("ApprovalExchange | AskThread | address prop | ask={current}", {
    reason: REASONS[10],
  }),
  ...answers(
    "ApprovalExchange | AskThread | address prop | order={order}",
    { reason: REASONS[1] },
    { reason: REASONS[10] }
  ),
  ...answers("ApprovalExchange | AskThread | address prop | repliesWhileOpen", {
    reason: REASONS[10],
  }),
  ...answers(
    "ApprovalExchange | AskThread | address prop | thread={thread}",
    { reason: REASONS[6] },
    { reason: REASONS[10] }
  ),

  // Where the collapsed thread draws `AskThread` for its replies alone.
  ...answers("AskThreadDisclosure | AskThread | conditional | !(replyCount === 0) ? …", {
    reason: REASONS[9],
  }),
  ...answers("AskThreadDisclosure | AskThread | conditional | (composerFirst(order)) ? …", {
    reason: REASONS[9],
  }),
  ...answers("AskThreadDisclosure | AskThread | conditional | !(composerFirst(order)) ? …", {
    reason: REASONS[9],
  }),
  ...answers("AskThreadDisclosure | AskThread | early return | if (thread.isError) return …", {
    reason: REASONS[9],
  }),
  ...answers(
    'AskThreadDisclosure | AskThread | early return | if (replyCount === 0 && !takesReply(ask) && unsentReply === "") return …',
    { reason: REASONS[9] }
  ),
  ...answers("AskThreadDisclosure | AskThread | address prop | ask={ask}", { reason: REASONS[9] }),
  ...answers("AskThreadDisclosure | AskThread | address prop | order={order}", {
    reason: REASONS[9],
  }),
  ...answers("AskThreadDisclosure | AskThread | address prop | showComposer={false}", {
    reason: REASONS[9],
  }),
  ...answers("AskThreadDisclosure | AskThread | address prop | thread={thread}", {
    reason: REASONS[9],
  }),
  ...answers("AskThreadDisclosure | AskThread | address prop | unread={unread}", {
    reason: REASONS[9],
  }),
];
