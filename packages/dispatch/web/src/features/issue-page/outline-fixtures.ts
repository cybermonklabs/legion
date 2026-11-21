import { jest } from "bun:test";

import { commentDeliveryFields } from "../../__tests__/comment-fixture";
import { onFakeClock } from "../../__tests__/fake-clock";
import type { Comment } from "../../api/types";
import { KEY } from "./decisions-filter/decisions-fixtures";

/**
 * What the outline's unit tests share (`IssueTree.test.tsx`, `outline-reads.test.tsx`): the
 * comments the comments' read answers for the fixtures' issue (`issueDetails`), and the fake clock
 * a row runs the product's zero-delay timers on.
 */

/**
 * Runs `body` on the fake clock (`onFakeClock` of `web/src/__tests__/fake-clock.ts`), for a row
 * whose next step waits on a zero-delay timer of the product's: a press is over a task after its
 * click (`press-focus.ts`'s `trackPress`), the row being edited is looked at a task after the
 * focus left it and reads a task after its editor drew the node's block whether it was handed one
 * (`useNodeEditing.ts`), and React Query hands an answer to what observes it a task after the
 * answer lands. `body` sets that timer, runs it with `tick(0)`, and reads what it did or did not
 * do once it has run.
 *
 * happy-dom runs every zero-delay timer set while one of them waits in that one's task
 * (`BrowserWindow.setTimeout`), and the fake clock holds a timer of the real clock still until the
 * real clock is back. So a timer of the product's set under the fake clock while one of the real
 * clock waited would ride that one: no `tick` would run it, and a row reading what it did not do
 * would pass whatever the product did. This refuses to run `body` then.
 */
export function zeroDelaysOnFakeClock(body: () => Promise<void>): Promise<void> {
  return onFakeClock(async () => {
    let held = false;
    window.setTimeout(() => {
      held = true;
    }, 0);
    jest.advanceTimersByTime(0);
    if (!held) {
      throw new Error(
        "a zero-delay timer of the real clock waited as the fake clock came, and would carry the product's own past it"
      );
    }
    await body();
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
