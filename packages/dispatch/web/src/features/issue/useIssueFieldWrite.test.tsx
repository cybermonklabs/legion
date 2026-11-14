import { expect, jest, test } from "bun:test";
import { act } from "@testing-library/react";

import { realClock, tick } from "../../__tests__/fake-clock";
import { renderOnQueue } from "../../__tests__/query-provider";
import { answerIssuePatches } from "../../__tests__/send-answers";
import { stopTheLateWrite } from "../../__tests__/stop-late-write";
import { NetworkError } from "../../api/client";
import type { Issue } from "../../api/types";
import { useIssueAssignee } from "./useIssueAssignee";

/** The assignee field of CORE-1 over one app's queue, with alice signed in: the field every
 *  single-field issue write is made through (`useIssueFieldWrite`). */
function renderAssignee() {
  return renderOnQueue(() => useIssueAssignee("CORE-1"));
}

/** CORE-1's assignee as Dispatch holds it, answering its patches as it keeps a key
 *  (`api/request_keys.go`): the first request under a key makes the change, and each later one
 *  under it is answered with that answer, nothing made again. While the line is down no answer
 *  comes back, though the change was made. */
function dispatchHoldingAssignee() {
  const held = { answering: false, assignee: null as string | null };
  const answers = new Map<string, Issue>();
  const patchIssue = answerIssuePatches().mockImplementation((key, input, bounds) => {
    let answer = answers.get(bounds.key);
    if (answer === undefined) {
      held.assignee = input.assignee ?? null;
      answer = { assignee: held.assignee, key } as Issue;
      answers.set(bounds.key, answer);
    }
    return held.answering
      ? Promise.resolve(answer)
      : Promise.reject(new NetworkError("/api/v1/issues/CORE-1", new TypeError("Failed to fetch")));
  });
  return { held, patchIssue };
}

// A pick the reader stopped belongs to the save that made it, never to the login it carried.
// Here the stopped save of alice had reached Dispatch, a save of bob landed after it, and the
// person picks alice again. Sent under the stopped save's key, that pick would be answered with
// what the stopped save did, the change not made, and the issue would stay bob's while the
// control said it was alice's. So the pick goes under a key of its own, and the issue ends at
// the person's last pick.
test("picking again the assignee a stopped save carried goes under its own key and lands", async () => {
  jest.useFakeTimers();
  const { held, patchIssue } = dispatchHoldingAssignee();
  const { client, hook } = renderAssignee();

  try {
    act(() => hook.result.current.submit("alice"));
    await stopTheLateWrite(client);
    expect(hook.result.current.failed).toBe(true);
    const stoppedKey = patchIssue.mock.calls[0]?.[2]?.key;

    held.answering = true;
    act(() => hook.result.current.submit("bob"));
    await tick(1_000);
    expect(held.assignee).toBe("bob");

    act(() => hook.result.current.submit("alice"));
    await tick(1_000);

    const back = patchIssue.mock.calls.at(-1);
    expect(back?.[1]).toEqual({ assignee: "alice" });
    expect(back?.[2]?.key).not.toBe(stoppedKey);
    expect(held.assignee).toBe("alice");
    expect(hook.result.current.failed).toBe(false);
  } finally {
    hook.unmount();
    patchIssue.mockRestore();
    realClock();
  }
});

// The control's Retry is the one way back to a stopped save: it may have reached Dispatch, so
// the Retry goes under its key, and Dispatch answers it with what that save did rather than
// making it a second time.
test("the control's Retry after a stopped save goes under that save's key", async () => {
  jest.useFakeTimers();
  const { held, patchIssue } = dispatchHoldingAssignee();
  const { client, hook } = renderAssignee();

  try {
    act(() => hook.result.current.submit("alice"));
    await stopTheLateWrite(client);
    const stoppedKey = patchIssue.mock.calls[0]?.[2]?.key;
    const stopped = patchIssue.mock.calls.length;

    held.answering = true;
    act(() => hook.result.current.retry());
    await tick(1_000);

    expect(patchIssue).toHaveBeenCalledTimes(stopped + 1);
    expect(patchIssue.mock.calls.at(-1)?.[2]?.key).toBe(stoppedKey);
    expect(held.assignee).toBe("alice");
    expect(hook.result.current.failed).toBe(false);
  } finally {
    hook.unmount();
    patchIssue.mockRestore();
    realClock();
  }
});
