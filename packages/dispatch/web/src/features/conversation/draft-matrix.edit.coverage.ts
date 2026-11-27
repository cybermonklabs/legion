/**
 * The draft matrix's coverage list, comment edit (INFRA-1503): each composer census item that
 * `draft-matrix.edit.test.tsx` answers for, held to one of its tests or to one of the numbered
 * reasons below, as `__tests__/composer-census/coverage.ts` says.
 */
import {
  type Answer,
  answers,
  type CoverageEntry,
  cells,
} from "../../__tests__/composer-census/coverage";

/** The hosts of the edit file's rows, as its titles name them: the host and its move off the
 *  screen and back. */
const CONVERSATION_CARD = {
  move: "Collapse thread, then Expand thread",
  name: "Conversation card",
};
const CONVERSATION_PHONE = { move: "Back, then back", name: "Conversation phone thread" };
const MARGIN_CARD = { move: "another thread opened, then back", name: "margin card" };
const MARGIN_PHONE = { move: "Back, then back", name: "margin phone thread" };

type Host = typeof CONVERSATION_CARD;

const HELD = ["typed", "refused", "stopped"] as const;

/** The first table's cells of a host: each state, over the base unchanged and changed. */
function away(host: Host): Answer[] {
  return HELD.flatMap((held) =>
    ["unchanged", "changed"].map((base) => ({
      row: `${host.name} edit · ${host.move} · ${held} · base ${base}`,
    }))
  );
}

/** The second table's cells of a host: each state, the comment changed to newer text or to the
 *  edit's own while the editor is on screen. */
function onScreen(host: Host): Answer[] {
  return HELD.flatMap((held) =>
    ["newer text", "the edit's own text"].map((change) => ({
      row: `${host.name} edit · comment changed on screen to ${change}, then ${host.move} · ${held}`,
    }))
  );
}

/** The third table's cells of a host. */
function replyEdit(host: Host): Answer[] {
  return cells(
    `${host.name} edit · Edit on the reader's reply in the thread, then on the comment again`
  );
}

/** The fourth table's cells of a host. */
function reloaded(host: Host): Answer[] {
  return cells(`${host.name} edit · the page reloaded, then back`);
}

/** The numbered reasons, said here alone, which the matrix file's header points to. */
const REASONS = {
  1: "1. The editor's address is its comment (`edit`, under the send name `[\"comment-edit\", id]`): another comment's editor is another address under a name of its own, so no move of the host's gives an editor another comment.",
  2: "2. A comment that leaves its thread's list, and a thread that leaves the Conversation's or the margin's list (its resolution, the Conversation's All, Open and Closed filter, another document's list), unmount the editor with its row or its card, as Collapse thread and another thread's opening do in the rows above.",
  3: "3. The Conversation's and the margin's loading or error lines in place of the list, leaving the issue or the document, a page with no margin, and the margin's Pinned tab, phone thread view and rail (`MarginSheet` draws the list only on the Comments tab, uncovered) unmount the editor with its card as the rows above do.",
  4: "4. Which end of a thread its replies are drawn from (`useThreadOrder`) is the reader's thread order, which a pick in the Inbox's display menu changes for every reader on the page at once, and another tab's pick for this tab's readers (`useUserPreference`). A pick reverses the replies of an open card on screen; each reply's row is keyed by its comment (`<li key={reply.id}>`), so an editor open in one moves with its row and stays mounted at its comment, and the card's reply moving to its other end moves no editor.",
  5: "5. On a phone a comment's Expand thread and a margin card's tap open the thread full-screen in place of opening it in the list (`CommentTurn`'s `isPhone`, `useMarginSheet`'s `onToggleThread` and `selectMarginItem`); the editor there is the phone rows' above, and Back is the way off. A comment link opens the Conversation's view as Expand thread does (`ConversationTab`'s focus effect), a decision landing on the comment closes it as Back does (`phone.hide`), and a comment's Reply on a phone answers in the view's own composer and leaves the editor open in its card.",
  6: "6. Each view's card is drawn only while its thread is open there (`phoneThread`, and `MarginSheet`'s `sheet.thread` at a phone width) and keyed by its thread: widening the page past the phone layout closes the Conversation's view as Back does unless a send from it is out (`usePhoneThread`'s `outlived`), and takes the margin's off the page, either ending the editor's mount as Back does; the editor drawn for its comment next is the one the phone rows' return draws.",
  7: "7. The margin's phone panel open or closed, its rail, where it sits, and the focus and scrolling the layout decides move no editor (the margin file's reasons 9 to 12), and neither does the read mark's ref on the Conversation's newest card (`register`). The app shell's forms are the sign-in file's.",
} as const;

export const coverage: readonly CoverageEntry[] = [
  ...answers(
    "ThreadCard | ThreadCard <MentionComposer mutationKey={[...COMMENT_EDIT_SEND_KEY, comment.id]}> | address prop | edit={{ body: comment.body, id: comment.id }}",
    away(CONVERSATION_CARD),
    away(CONVERSATION_PHONE),
    away(MARGIN_CARD),
    away(MARGIN_PHONE),
    onScreen(CONVERSATION_CARD),
    onScreen(CONVERSATION_PHONE),
    onScreen(MARGIN_CARD),
    onScreen(MARGIN_PHONE),
    { reason: REASONS[1] }
  ),
  ...answers(
    "ThreadCard | ThreadCard <MentionComposer mutationKey={[...COMMENT_EDIT_SEND_KEY, comment.id]}> | address prop | mutationKey={[...COMMENT_EDIT_SEND_KEY, comment.id]}",
    { reason: REASONS[1] }
  ),
  ...answers(
    "ThreadCard | ThreadCard <MentionComposer mutationKey={[...COMMENT_EDIT_SEND_KEY, comment.id]}> | address prop | owner={composerOwner(owner)}",
    { reason: REASONS[1] }
  ),
  ...answers(
    "ThreadCard | ThreadCard <MentionComposer mutationKey={[...COMMENT_EDIT_SEND_KEY, comment.id]}> | conditional | (editing(root)) ? …",
    replyEdit(CONVERSATION_CARD),
    replyEdit(CONVERSATION_PHONE),
    replyEdit(MARGIN_CARD),
    replyEdit(MARGIN_PHONE)
  ),
  ...answers(
    "ThreadCard | ThreadCard <MentionComposer mutationKey={[...COMMENT_EDIT_SEND_KEY, comment.id]}> | conditional | (expanded) ? …",
    away(CONVERSATION_CARD),
    away(MARGIN_CARD)
  ),
  ...answers(
    "ThreadCard | ThreadCard <MentionComposer mutationKey={[...COMMENT_EDIT_SEND_KEY, comment.id]}> | conditional | (editing(reply)) ? …",
    replyEdit(CONVERSATION_CARD),
    replyEdit(CONVERSATION_PHONE),
    replyEdit(MARGIN_CARD),
    replyEdit(MARGIN_PHONE)
  ),
  ...answers(
    "ThreadCard | ThreadCard <MentionComposer mutationKey={[...COMMENT_EDIT_SEND_KEY, comment.id]}> | key | <li key={reply.id}>",
    { reason: REASONS[2] }
  ),
  ...answers(
    "ThreadCard | ThreadCard <MentionComposer mutationKey={[...COMMENT_EDIT_SEND_KEY, comment.id]}> | conditional | drawn by shown.map(…)",
    { reason: REASONS[2] }
  ),
  ...answers(
    "ThreadCard | ThreadCard <MentionComposer mutationKey={[...COMMENT_EDIT_SEND_KEY, comment.id]}> | conditional | !(shown.length === 0) ? …",
    { reason: REASONS[2] }
  ),
  ...answers(
    "ThreadCard | ThreadCard <MentionComposer mutationKey={replyMutationKey}> | conditional | (composerFirst) ? …",
    { reason: REASONS[4] }
  ),
  ...answers(
    "ThreadCard | ThreadCard <MentionComposer mutationKey={replyMutationKey}> | conditional | !(composerFirst) ? …",
    { reason: REASONS[4] }
  ),
  ...answers(
    "CommentTurn | ThreadCard | address prop | expanded={currentExpanded}",
    away(CONVERSATION_CARD),
    onScreen(CONVERSATION_CARD)
  ),
  ...answers(
    "MarginSheet | ThreadCard | conditional | (isPhoneViewport && phoneThread !== undefined && owner !== undefined && visibleArtifact !== undefined) ? …",
    away(MARGIN_PHONE),
    onScreen(MARGIN_PHONE)
  ),
  ...answers(
    "ThreadList | ThreadCard | address prop | expanded={expandedThreadKey === thread.key}",
    away(MARGIN_CARD),
    onScreen(MARGIN_CARD)
  ),
  ...answers("ConversationTab | CommentTurn | key | <CommentTurn key={item.id}>", {
    reason: REASONS[2],
  }),
  ...answers("ConversationTab | CommentTurn | early return | if (loading) return …", {
    reason: REASONS[3],
  }),
  ...answers("ConversationTab | CommentTurn | early return | if (failed) return …", {
    reason: REASONS[3],
  }),
  ...answers(
    "ConversationTab | CommentTurn | conditional | <section hidden={phoneThread === undefined}>",
    away(CONVERSATION_PHONE),
    onScreen(CONVERSATION_PHONE)
  ),
  ...answers("CommentsTab | ThreadList | conditional | !(commentsError) ? …", {
    reason: REASONS[3],
  }),
  ...answers(
    'MarginSheet | CommentsTab | conditional | (tab.value === "comments" && !covered && owner !== undefined && visibleArtifact !== undefined) ? …',
    { reason: REASONS[3] }
  ),
  ...answers(
    "MarginSheet | ThreadCard | form | (isPhoneViewport && phoneThread !== undefined && owner !== undefined && visibleArtifact !== undefined) ? …",
    { reason: REASONS[6] }
  ),
  ...answers("ConversationTab | CommentTurn | form | isPhone={isPhoneViewport}", {
    reason: REASONS[5],
  }),
  ...answers("ConversationTab | CommentTurn | form | register={register}", { reason: REASONS[7] }),
  ...answers("ConversationTab | CommentTurn | form | !(phoneThread === undefined) ? …", {
    reason: REASONS[6],
  }),
  ...answers("ConversationTab | CommentTurn | form | key={phoneThread.id}", { reason: REASONS[6] }),
  ...answers("ConversationTab | - | form | if (isPhoneViewport)", { reason: REASONS[5] }),
  ...answers("MarginSheet | - | form | (covered) ? …", { reason: REASONS[7] }),
  ...answers("CommentTurn | - | form | if (forceExpanded || (isPhone && !expanded)) return …", {
    reason: REASONS[5],
  }),
  ...answers("usePhoneThread | - | form | if (isPhoneViewport || outlived) return …", {
    reason: REASONS[6],
  }),
  ...answers(
    "useMarginSheet | - | form | if (ownerId !== undefined && window.matchMedia(PHONE_VIEWPORT_QUERY).matches) return …",
    { reason: REASONS[5] }
  ),
  ...answers(
    "ThreadCard | ThreadCard <MentionComposer mutationKey={[...COMMENT_EDIT_SEND_KEY, comment.id]}> | lifetime | <MentionComposer mutationKey={[...COMMENT_EDIT_SEND_KEY, comment.id]}>",
    reloaded(CONVERSATION_CARD),
    reloaded(CONVERSATION_PHONE),
    reloaded(MARGIN_CARD),
    reloaded(MARGIN_PHONE)
  ),
];
