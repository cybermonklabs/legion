/**
 * The draft matrix's coverage list, inline reply (INFRA-1503): each composer census item that
 * `draft-matrix.inline-reply.test.tsx` answers for, held to one of its tests or to one of the
 * numbered reasons of its header, as `__tests__/composer-census/coverage.ts` says.
 */
import { answers, type CoverageEntry, cells } from "../../__tests__/composer-census/coverage";

/** The host as the matrix file's titles name it. */
const HOST = "inline reply";

/** The numbered reasons of the matrix file's header. */
const REASONS = {
  1: "1. The card's reply always answers its own thread's root on its own issue, so no move of the host's changes its address, and it is never elsewhere.",
  2: "2. An inline reply is compact, so it draws no Close (asserted absent below).",
  3: "3. The issue picker is an Agents row's (`AgentMessageComposer`), and a thread card draws no Agents row's composer.",
  4: "4. A card is drawn per item of the Conversation's list, keyed by it (`CommentTurn`): the list's All, Open and Closed filter, and a thread the list no longer holds, unmount the card and its reply as Collapse thread does, and the card drawn when it is listed again opens on the same thread's send name.",
  5: "5. The Conversation draws a loading line or an error in place of the list while its reads are out or failed (`loading`, `failed`), which unmounts the reply as Collapse thread does.",
  6: "6. Another issue's address draws the issue page afresh (`IssueDetail` keyed by `route.key`), unmounting the card and its reply as Collapse thread does, and another tab hides the Conversation's panel with the reply mounted (`draft-matrix.issue-page.test.tsx` makes both moves for the docked and phone thread composers).",
  7: "7. Which end of the thread the reply sits at (`composerFirst`, from `useThreadOrder`) is read when the card is drawn and again only when who is signed in changes (`useUserPreference`), so it never moves a reply that is on screen.",
  8: "8. On a phone the comment's Expand thread opens the full-screen thread view in place of opening the card in the list (`toggleThread`'s `isPhone`), and its Reply answers in the view's own composer (`onReply`, and Reply's `!isPhone`), so the first row and the phone Reply row are the phone's forms of the Collapse thread and Reply rows. A comment link opens the view as Expand thread does (`ConversationTab`'s focus effect), and a decision landing on the comment closes it as Back does (`phone.hide`).",
  9: "9. The view's card is drawn only while a thread is open (`phoneThread`) and keyed by its thread (`key={phoneThread.id}`): another thread is opened after Back, and widening the page past the phone layout closes the view as Back does unless a send from it is out (`usePhoneThread`'s `outlived`), the first row's move either way.",
  10: "10. The read mark's ref on the newest card (`register`) moves to the view's copy of it while a thread is open; it marks the issue read and moves no reply. The margin's thread reply is the margin file's, and the app shell's forms are the sign-in file's.",
} as const;

export const coverage: readonly CoverageEntry[] = [
  ...answers(
    "ThreadCard | ThreadCard <MentionComposer mutationKey={replyMutationKey}> | address prop | owner={composerOwner(owner)}",
    { reason: REASONS[1] }
  ),
  ...answers(
    "ThreadCard | ThreadCard <MentionComposer mutationKey={replyMutationKey}> | conditional | (expanded) ? …",
    cells(`${HOST} · thread collapsed by the host, then opened`)
  ),
  ...answers(
    "ThreadCard | ThreadCard <MentionComposer mutationKey={replyMutationKey}> | address prop | mutationKey={replyMutationKey}",
    { row: `${HOST} · Close, then Discard draft? · cannot make: the card's reply draws no Close` },
    { reason: REASONS[2] },
    { reason: REASONS[3] }
  ),
  ...answers(
    'ThreadCard | ThreadCard <MentionComposer mutationKey={replyMutationKey}> | address prop | replyTo={{ actor: root.author, excerpt: "", id: root.id, parentKind: "comment" }}',
    cells(`${HOST} · Cancel reply`),
    cells(`${HOST} · Escape in the field`),
    { reason: REASONS[1] }
  ),
  ...answers(
    "ThreadCard | ThreadCard <MentionComposer mutationKey={replyMutationKey}> | conditional | <div hidden={hideReplyComposer}>",
    cells(`${HOST} · Reply on its comment, then back`),
    cells(`${HOST} · phone · Reply on its comment, then back (phone thread view)`)
  ),
  ...answers(
    "ThreadCard | ThreadCard <MentionComposer mutationKey={replyMutationKey}> | conditional | (composerFirst) ? …",
    { reason: REASONS[7] }
  ),
  ...answers(
    "ThreadCard | ThreadCard <MentionComposer mutationKey={replyMutationKey}> | conditional | !(composerFirst) ? …",
    { reason: REASONS[7] }
  ),
  ...answers(
    "CommentTurn | ThreadCard | address prop | expanded={currentExpanded}",
    cells(`${HOST} · thread collapsed by the host, then opened`)
  ),
  ...answers('CommentTurn | ThreadCard | address prop | owner={{ key: issueKey, kind: "issue" }}', {
    reason: REASONS[6],
  }),
  ...answers("CommentTurn | ThreadCard | address prop | thread={thread}", { reason: REASONS[4] }),
  ...answers(
    "CommentTurn | ThreadCard | address prop | hideReplyComposer={hideReplyComposer}",
    cells(`${HOST} · Reply on its comment, then back`)
  ),
  ...answers(
    "CommentTurn | ThreadCard | address prop | replyMutationKey={replySendKey}",
    cells(`${HOST} · thread collapsed by the host, then opened`),
    { reason: REASONS[4] }
  ),
  ...answers("ConversationTab | CommentTurn | key | <CommentTurn key={item.id}>", {
    reason: REASONS[4],
  }),
  ...answers(
    'ConversationTab | CommentTurn | early return | if (item.kind === "targeted-message") return …',
    { reason: REASONS[4] }
  ),
  ...answers(
    'ConversationTab | CommentTurn | early return | if (item.kind === "message") return …',
    { reason: REASONS[4] }
  ),
  ...answers(
    'ConversationTab | CommentTurn | early return | if (item.kind === "handoff") return …',
    { reason: REASONS[4] }
  ),
  ...answers('ConversationTab | CommentTurn | conditional | if (item.kind === "comment")', {
    reason: REASONS[4],
  }),
  ...answers("ConversationTab | CommentTurn | conditional | drawn by shown.map(…)", {
    reason: REASONS[4],
  }),
  ...answers("ConversationTab | CommentTurn | early return | if (loading) return …", {
    reason: REASONS[5],
  }),
  ...answers("ConversationTab | CommentTurn | early return | if (failed) return …", {
    reason: REASONS[5],
  }),
  ...answers(
    "ConversationTab | CommentTurn | address prop | expanded={openThreads.has(item.root.id)}",
    cells(`${HOST} · thread collapsed by the host, then opened`)
  ),
  ...answers("ConversationTab | CommentTurn | address prop | issueKey={issueKey}", {
    reason: REASONS[6],
  }),
  ...answers("ConversationTab | CommentTurn | address prop | item={item}", { reason: REASONS[4] }),
  ...answers(
    'ConversationTab | CommentTurn | address prop | hideReplyComposer={replyTo?.parentKind === "comment" && replyTo.id === item.root.id}',
    cells(`${HOST} · Reply on its comment, then back`)
  ),
  ...answers("ConversationTab | CommentTurn | address prop | sendKey={sendKey}", {
    reason: REASONS[6],
  }),
  ...answers("ConversationTab | CommentTurn | key | <CommentTurn key={phoneThread.id}>", {
    reason: REASONS[9],
  }),
  ...answers(
    "ConversationTab | CommentTurn | conditional | !(phoneThread === undefined) ? …",
    cells(`${HOST} · Back, then back (phone thread view)`),
    { reason: REASONS[9] }
  ),
  ...answers(
    "ConversationTab | CommentTurn | conditional | <section hidden={phoneThread === undefined}>",
    cells(`${HOST} · Back, then back (phone thread view)`)
  ),
  ...answers(
    "ConversationTab | CommentTurn | conditional | !(phoneThread === undefined && phone.replyTo === null) ? …",
    cells(`${HOST} · Back, then back (phone thread view)`)
  ),
  ...answers(
    "ConversationTab | CommentTurn | address prop | expanded={openThreads.has(phoneThread.root.id)}",
    cells(`${HOST} · Back, then back (phone thread view)`)
  ),
  ...answers("ConversationTab | CommentTurn | address prop | item={phoneThread}", {
    reason: REASONS[9],
  }),
  ...answers(
    "ConversationTab | CommentTurn | address prop | hideReplyComposer={phoneReplyTargetsThread}",
    cells(`${HOST} · phone · Reply on its comment, then back (phone thread view)`)
  ),
  ...answers("ConversationTab | CommentTurn | form | isPhone={isPhoneViewport}", {
    reason: REASONS[8],
  }),
  ...answers(
    "ConversationTab | CommentTurn | form | onReply={isPhoneViewport ? phone.beginReply : beginReply}",
    { reason: REASONS[8] }
  ),
  ...answers("ConversationTab | CommentTurn | form | register={register}", { reason: REASONS[10] }),
  ...answers("ConversationTab | CommentTurn | form | !(phoneThread === undefined) ? …", {
    reason: REASONS[9],
  }),
  ...answers("ConversationTab | CommentTurn | form | key={phoneThread.id}", { reason: REASONS[9] }),
  ...answers("ConversationTab | - | form | if (phone.id === id)", { reason: REASONS[8] }),
  ...answers("ConversationTab | - | form | if (isPhoneViewport)", { reason: REASONS[8] }),
  ...answers("CommentTurn | - | form | if (forceExpanded || (isPhone && !expanded)) return …", {
    reason: REASONS[8],
  }),
  ...answers(
    "CommentTurn | - | form | if (onReply(commentReplyTarget(item.root, agentText, issueKey)) && !isPhone)",
    { reason: REASONS[8] }
  ),
  ...answers("usePhoneThread | - | form | if (isPhoneViewport || outlived) return …", {
    reason: REASONS[9],
  }),
  ...answers(
    "ThreadCard | ThreadCard <MentionComposer mutationKey={replyMutationKey}> | lifetime | <MentionComposer mutationKey={replyMutationKey}>",
    cells(`${HOST} · the page reloaded, then back`)
  ),
];
