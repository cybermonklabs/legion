import type { Comment, MessageRead } from "../../api/types";

/**
 * Which comments and messages an agent wrote outside every thread a person wrote in
 * (dispatch://INFRA-1196 R1, R15, R20): the outline draws none of them, and the history in the
 * panel draws each, with its text, its replies and its reply control. Once a person writes in
 * such a thread it is the conversation's, and the outline draws the whole of it.
 *
 * A comment thread is a comment and every reply under its head (`reply_to`, which the server
 * climbs to the head as it stores, `comment_create.go`'s `threadHeadOf`); a message thread is a
 * message and its replies. A person is a user. A comment under an ask is in the ask's thread, and
 * every ask is a node of the outline, so no such comment is one of these, and neither is a reply
 * whose thread opens under an ask.
 *
 * Pure, and the one place the rule is written: the outline, the history and the address of one
 * of these items read it alike.
 */
export function agentsOnlyIds(
  comments: readonly Pick<Comment, "ask_id" | "author" | "id" | "reply_to">[] | undefined,
  messages: readonly MessageRead[] | undefined
): ReadonlySet<string> {
  const ids = new Set<string>();
  const threads = new Map<string, Pick<Comment, "ask_id" | "author" | "id" | "reply_to">[]>();
  for (const comment of comments ?? []) {
    const head = comment.reply_to ?? comment.id;
    const thread = threads.get(head);
    if (thread === undefined) threads.set(head, [comment]);
    else thread.push(comment);
  }
  for (const thread of threads.values()) {
    if (thread.every((one) => one.ask_id === null && one.author.kind !== "user")) {
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
