import { api, type WriteBounds } from "../../api/client";
import type { Ask, Comment, CreateCommentInput } from "../../api/types";

/**
 * The comment an ask sends, and where it goes. One place, because the open card's Ask back
 * (`useAskAnswerForm.ts`) and the answered ask's Reply (`AskThread.tsx`) are the same write: the
 * same wire shape, the same fallback to the ask's document, and the same contract error where
 * that document has no artifact. Written twice, a change to either would have to land twice.
 */

/** A document ask's artifact; the server always sets it, so its absence is a contract error. */
export function askArtifactId(ask: Pick<Ask, "artifact_id">): string {
  if (ask.artifact_id === null || ask.artifact_id === undefined) {
    throw new Error("document ask is missing its artifact id");
  }
  return ask.artifact_id;
}

/** Posts the words `text` as a comment on `ask` where it lives: its issue, through
 *  `createReply`, or the document it was asked on. A party the words mention is a reference
 *  written in them, which the server reads deliveries from, so the comment carries no list of
 *  mentions beside its body. */
export function postAskComment(
  ask: Pick<Ask, "artifact_id" | "id" | "issue_key">,
  text: string,
  bounds: WriteBounds,
  createReply: (
    issueKey: string,
    input: CreateCommentInput,
    bounds: WriteBounds
  ) => Promise<Comment>
): Promise<Comment> {
  const input: CreateCommentInput = { ask_id: ask.id, body: text };
  if (ask.issue_key === null) {
    return api.createArtifactComment(askArtifactId(ask), input, bounds);
  }
  return createReply(ask.issue_key, input, bounds);
}
