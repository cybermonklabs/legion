import type { Ask, Comment, MessageRead } from "../../api/types";

/** What the rule reads of a comment. */
type CommentTurn = Pick<Comment, "anchor" | "ask_id" | "author" | "id" | "reply_to">;

/** What the rule reads of a message thread: its root's author and id, and each reply's id. */
interface MessageThread {
  readonly message: Pick<MessageRead["message"], "author" | "id">;
  readonly replies: readonly Pick<MessageRead["message"], "id">[] | null;
}

/** How the session id of the Legion daemon begins: the daemon of a project writes as the session
 *  `legion-daemon:<PROJECT>`, which `shortSessionId` of `@legion/contracts` reads by the same
 *  prefix. */
const DAEMON_SESSION = "legion-daemon:";

/**
 * The comments and messages of the threads the history in the panel draws, each thread whole:
 * its text, its replies and its reply control. The outline draws none of them, and it draws
 * every comment and message not in the set as it draws a thread a person wrote in.
 *
 * A message thread is a message and its replies, as the server lists them: a root message with
 * every message under it. It is in the set, with every message of it, when the Legion daemon
 * wrote its root message: the author is the session `legion-daemon:<PROJECT>`. Whoever wrote the
 * replies, a person included, the thread stays in the set. No other message thread is in it: not
 * an agent's note to the issue's people, not a thread between agents, not a thread a person
 * began.
 *
 * A comment thread is a comment and every reply under its head (`reply_to`, which the server
 * climbs to the head as it stores, `comment_create.go`'s `threadHeadOf`). It is in the set when
 * its head comment is tied to no text (its `anchor` is null), no person wrote in it, and no turn
 * of it is under a question. A person is a user. A thread whose head has an anchor is never in
 * the set, whoever wrote in it. A thread whose head comment is not among the comments read is not
 * in the set either: whether that head has an anchor is not known, and the history draws a
 * comment thread from its head, so the outline draws its replies.
 *
 * A comment under an ask names the ask (`ask_id`), where that climb stops, so an ask's thread is
 * every comment naming it. The outline draws each question as a node and no approval request
 * (`tree-drafts.ts`'s `addAsks`): a comment under a question is in that node's thread, so it is
 * never in the set, and neither is a reply whose thread opens under one. The comments under an
 * approval request are a thread of their own, whose head is the ask and no comment, and it is in
 * the set when no person wrote in it: no anchor decides it, since the server stores no anchor on
 * a comment under an ask. `asks` - the issue's open asks and the asks read (`issueAsks`) - is
 * what tells a question from an approval request; a caller that has read none of them passes
 * `undefined`, and every ask is then taken for a question. It is required, so a caller cannot
 * leave it out unseen and draw an agent's reply under an approval request in the outline.
 *
 * Pure, and the one place the rule is written: the outline, the history, the address of one of
 * these items and the landing of a write from the line at the foot read it alike.
 */
export function agentsOnlyIds(
  comments: readonly CommentTurn[] | undefined,
  messages: readonly MessageThread[] | undefined,
  asks: readonly Pick<Ask, "id" | "kind">[] | undefined
): ReadonlySet<string> {
  const questions =
    asks === undefined
      ? undefined
      : new Set(asks.filter((ask) => ask.kind === "question").map((ask) => ask.id));
  const underQuestion = (comment: CommentTurn): boolean =>
    comment.ask_id !== null && (questions === undefined || questions.has(comment.ask_id));
  const ids = new Set<string>();
  // A thread by the comment at its head, and one under an ask the outline draws no node for by
  // that ask: an ask's id and a comment's are ids of two tables, so each has a map of its own.
  const threads = new Map<string, CommentTurn[]>();
  const askThreads = new Map<string, CommentTurn[]>();
  for (const comment of comments ?? []) {
    const [byHead, head] =
      comment.ask_id === null || underQuestion(comment)
        ? ([threads, comment.reply_to ?? comment.id] as const)
        : ([askThreads, comment.ask_id] as const);
    const thread = byHead.get(head);
    if (thread === undefined) byHead.set(head, [comment]);
    else thread.push(comment);
  }
  // The threads tied to no text: each whose head comment the read holds with no anchor, and
  // each under an approval request, whose head is the ask.
  const untied = [
    ...[...threads].flatMap(([head, thread]) =>
      thread.find((one) => one.id === head)?.anchor === null ? [thread] : []
    ),
    ...askThreads.values(),
  ];
  for (const thread of untied) {
    if (thread.every((one) => !underQuestion(one) && one.author.kind !== "user")) {
      for (const one of thread) ids.add(one.id);
    }
  }
  for (const thread of messages ?? []) {
    const { author } = thread.message;
    if (author.kind !== "session" || !author.id.startsWith(DAEMON_SESSION)) continue;
    for (const one of [thread.message, ...(thread.replies ?? [])]) ids.add(one.id);
  }
  return ids;
}

/** The id a person's reply is read under before the server has stored it: no id the server
 *  gives, which is a uuid, so it names no turn the reads hold. */
const NEW_TURN = "";

/** The author of that reply: a person, whose kind is all the rule reads of an author. */
const PERSON = { id: "", kind: "user" } as const;

/**
 * Whether the history draws a person's reply to `under`, a comment or a message of the issue by
 * its id and kind: whether `under` is in the set above over the reads with that reply added, as
 * the server stores it, which climbs a reply to the head of its thread - the ask the thread hangs
 * under, or its root comment (`comment_create.go`'s `threadHeadOf`). A person's turn takes a
 * comment thread out of the set, so the outline then draws that thread whole, the reply under the
 * comment it answers; a message thread the Legion daemon began stays in it, whoever replies. An
 * ask and a statement are never in the set, so a reply to one is never the history's. The line at
 * the foot asks it as its reply lands, before the read that brings the reply has answered
 * (`outline-writer.tsx`'s `land`), so the page takes the reader to the part that draws the reply.
 */
export function historyHoldsReply(
  under: { readonly id: string; readonly kind: string },
  comments: readonly CommentTurn[] | undefined,
  messages: readonly MessageThread[] | undefined,
  asks: readonly Pick<Ask, "id" | "kind">[] | undefined
): boolean {
  const answered =
    under.kind === "comment" ? comments?.find((one) => one.id === under.id) : undefined;
  const withComment =
    answered === undefined
      ? comments
      : [
          ...(comments ?? []),
          {
            anchor: null,
            ask_id: answered.ask_id,
            author: PERSON,
            id: NEW_TURN,
            reply_to: answered.ask_id === null ? (answered.reply_to ?? answered.id) : null,
          },
        ];
  const withMessage =
    under.kind === "message"
      ? messages?.map((thread) =>
          [thread.message, ...(thread.replies ?? [])].some((one) => one.id === under.id)
            ? { ...thread, replies: [...(thread.replies ?? []), { id: NEW_TURN }] }
            : thread
        )
      : messages;
  return agentsOnlyIds(withComment, withMessage, asks).has(under.id);
}

/**
 * The asks of an issue that tell a question from an approval request: `openAsks`, the asks the
 * issue read holds open (`IssueDetails.open_asks`), then `asks`, the asks read, `undefined` while
 * it has not answered. The rule above reads this one list wherever it is read (`agentsOnlyIds`'
 * `asks`) - the outline (`tree-drafts.ts`), which draws its questions from it too, the history
 * (`IssueHistory.tsx`), the address of one of these items (`address-target.ts`) and the landing
 * of a write from the line at the foot (`outline-writer.tsx`) - so no turn is in both the outline
 * and the history or in neither, and no landing is left by both. While the asks read is out or
 * has failed, the list is the open asks alone, and every reader of it takes the same asks for
 * questions: the questions the issue holds open, and no other ask.
 */
export function issueAsks<Open, Read>(
  openAsks: readonly Open[] | null,
  asks: readonly Read[] | undefined
): (Open | Read)[] {
  return [...(openAsks ?? []), ...(asks ?? [])];
}
