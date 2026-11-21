import { act } from "@testing-library/react";

import { commentDeliveryFields } from "../../__tests__/comment-fixture";
import type { Comment } from "../../api/types";
import { KEY } from "./decisions-filter/decisions-fixtures";

/**
 * What the outline's unit tests share (`IssueTree.test.tsx`, `outline-reads.test.tsx`): the
 * comments the comments' read answers for the fixtures' issue (`issueDetails`), and a wait inside
 * `act`.
 */

/** Lets `ms` pass inside `act`, after `first` where given, and React draw what changed by then. */
export function waitInAct(ms: number, first?: () => void): Promise<void> {
  return act(async () => {
    first?.();
    const waited = Promise.withResolvers<void>();
    setTimeout(waited.resolve, ms);
    await waited.promise;
  });
}

/** A comment as the comments' read answers it, `at` the minute of the hour it was written in:
 *  `reply_to` is the head of the thread it answers, and `author` alice where none is given. */
export function readComment(
  id: string,
  at: number,
  reply_to: string | null = null,
  author: Comment["author"] = { id: "alice", kind: "user" }
): Comment {
  return {
    ...commentDeliveryFields(),
    anchor: null,
    answers: reply_to,
    ask_id: null,
    author,
    body: `The words of ${id}.`,
    created_at: `2026-11-05T01:${String(at).padStart(2, "0")}:00Z`,
    edited_at: null,
    id,
    issue_key: KEY,
    reply_to,
    resolved: false,
    resolved_at: null,
    resolved_by: null,
    suggestion: null,
    turn: null,
  };
}

/** Two threads of the issue, each a head and one reply under it. */
export const TWO_THREADS: readonly Comment[] = [
  readComment("head-a", 1),
  readComment("reply-a", 2, "head-a"),
  readComment("head-b", 3),
  readComment("reply-b", 4, "head-b"),
];
