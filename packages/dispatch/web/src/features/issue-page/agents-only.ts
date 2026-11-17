import type { Ask, Comment, MessageRead } from "../../api/types";

/** What the rule reads of a comment. */
type CommentTurn = Pick<Comment, "ask_id" | "author" | "id" | "reply_to">;

/**
 * Which comments and messages an agent wrote outside every thread a person wrote in: the
 * outline draws none of them, and the history in the panel draws each, with its text, its
 * replies and its reply control. Once a person writes in such a thread it is the
 * conversation's, and the outline draws the whole of it.
 *
 * A comment thread is a comment and every reply under its head (`reply_to`, which the server
 * climbs to the head as it stores, `comment_create.go`'s `threadHeadOf`); a message thread is a
 * message and its replies. A person is a user. A comment under an ask names the ask (`ask_id`),
 * where that climb stops, so an ask's thread is every comment naming it. The outline draws each
 * question as a node and no approval request (`tree-drafts.ts`'s `addAsks`): a comment under a
 * question is in that node's thread, so it is none of these, and neither is a reply whose thread
 * opens under one; the comments under an approval request are a thread of their own, and these
 * where no person wrote in it. `asks` - the asks read - is what tells the two apart; a caller
 * that has not read them passes `undefined`, and every ask is then taken for a question. It is
 * required, so a caller cannot leave it out unseen and draw an agent's reply under an approval
 * request in the outline.
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
  for (const thread of [...threads.values(), ...askThreads.values()]) {
    if (thread.every((one) => !underQuestion(one) && one.author.kind !== "user")) {
      for (const one of thread) ids.add(one.id);
    }
  }
  for (const thread of messages ?? []) {
    const turns = [thread.message, ...(thread.replies ?? [])];
    if (turns.every((one) => one.author.kind !== "user")) {
      for (const one of turns) ids.add(one.id);
    }
  }
  return ids;
}
