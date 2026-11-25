import type { Ask, Comment, MessageRead } from "../../api/types";

/** What the rule reads of a comment. */
type CommentTurn = Pick<Comment, "anchor" | "ask_id" | "author" | "id" | "reply_to">;

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
 * a comment under an ask. `asks` - the asks read - is what tells a question from an approval
 * request; a caller that has not read them passes `undefined`, and every ask is then taken for a
 * question. It is required, so a caller cannot leave it out unseen and draw an agent's reply
 * under an approval request in the outline.
 *
 * Pure, and the one place the rule is written: the outline, the history and the address of one
 * of these items read it alike.
 */
export function agentsOnlyIds(
  comments: readonly CommentTurn[] | undefined,
  messages: readonly MessageRead[] | undefined,
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
