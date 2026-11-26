import { spyOn } from "bun:test";
import { act } from "@testing-library/react";

import { commentDeliveryFields } from "../../__tests__/comment-fixture";
import type { Comment } from "../../api/types";
import { KEY } from "./decisions-filter/decisions-fixtures";

/**
 * What the issue page's unit tests share. Frames run by hand (`framesByHand`), for each row that
 * holds the frames the page waits on before it moves the focus: the outline's
 * (`IssueTree.test.tsx`, `view-decisions-slot.test.tsx`), the outline's line
 * (`outline-writer.test.tsx`), Details' (`details-rows.test.tsx`, `IssueMargin.test.tsx`) and a
 * view chord's (`useIssueKeymap.test.tsx`). The comments the comments' read answers for the
 * fixtures' issue (`readComment`, `TWO_THREADS`), which the outline's and the stage's tests read.
 * And a wait inside `act` (`waitInAct`), which the outline's and the Decisions view's tests use.
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

/** Holds every frame asked for from here on until `run` runs them - the ones named, or every one
 *  still asked for - as the next frame would; a frame cancelled first is not run. `asked` names
 *  the frames asked for and not run yet, and `restore` gives the frames back to happy-dom. */
export function framesByHand(): {
  readonly asked: () => number[];
  readonly restore: () => void;
  readonly run: (frames?: readonly number[]) => Promise<void>;
} {
  const asked = new Map<number, FrameRequestCallback>();
  let last = 0;
  const request = spyOn(window, "requestAnimationFrame").mockImplementation((callback) => {
    last += 1;
    asked.set(last, callback);
    return last;
  });
  const cancel = spyOn(window, "cancelAnimationFrame").mockImplementation((frame) => {
    asked.delete(frame);
  });
  return {
    asked: () => [...asked.keys()],
    restore: () => {
      request.mockRestore();
      cancel.mockRestore();
    },
    run: (frames = [...asked.keys()]) =>
      act(async () => {
        for (const frame of frames) {
          const callback = asked.get(frame);
          asked.delete(frame);
          callback?.(performance.now());
        }
      }),
  };
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
