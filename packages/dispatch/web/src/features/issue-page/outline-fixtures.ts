import { jest, spyOn } from "bun:test";
import { act } from "@testing-library/react";

import { commentDeliveryFields } from "../../__tests__/comment-fixture";
import { afterOnFakeClock, onFakeClock } from "../../__tests__/fake-clock";
import type { Comment } from "../../api/types";
import { KEY } from "../decision-card/decision-fixture";

/**
 * What the outline's unit tests share (`IssueTree.test.tsx`, `outline-reads.test.tsx`, and the
 * stage's tests): frames run by hand (`framesByHand`), the
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
 * would pass whatever the product did. This refuses to run `body` then, which it tells by a
 * zero-delay timer of its own on the fake clock (`afterOnFakeClock`, whose `setTimeout` is
 * happy-dom's window's): a move of 0 ms does not run that timer then.
 */
export function zeroDelaysOnFakeClock(body: () => Promise<void>): Promise<void> {
  return onFakeClock(async () => {
    let held = false;
    afterOnFakeClock(0, () => {
      held = true;
    });
    jest.advanceTimersByTime(0);
    if (!held) {
      throw new Error(
        "a zero-delay timer of the real clock waited as the fake clock came, and would carry the product's own past it"
      );
    }
    await body();
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
