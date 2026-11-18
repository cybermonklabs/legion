import { expect, jest, test } from "bun:test";
import { act, waitFor } from "@testing-library/react";

import { realClock, tick } from "../../__tests__/fake-clock";
import { renderOnQueue } from "../../__tests__/query-provider";
import { answerIssuePatches } from "../../__tests__/send-answers";
import { stopTheLateWrite } from "../../__tests__/stop-late-write";
import { NetworkError } from "../../api/client";
import type { Issue, IssueDetails } from "../../api/types";
import { SendStoppedError, stoppedWriteNote } from "../writes/keyed-write";
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
    const { retry } = hook.result.current;
    expect(retry).toBeDefined();
    act(() => retry?.());
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

// A Retry runs the newest render's request with the press's own values, so every byte it sends
// rides in the press, the issue among them. Rendered for another issue by then, the hook's Retry
// is still of the stopped save: it goes to that save's issue under its key, and the words beside
// it name that issue. Sent to the issue the hook is rendered for by then, it would be other
// bytes under the stopped save's key.
test("a Retry after the control moved to another issue is of the stopped save's issue", async () => {
  jest.useFakeTimers();
  const { held, patchIssue } = dispatchHoldingAssignee();
  let issueKey = "CORE-1";
  const { client, hook } = renderOnQueue(() => useIssueAssignee(issueKey));

  try {
    act(() => hook.result.current.submit("alice"));
    await stopTheLateWrite(client);
    const stoppedKey = patchIssue.mock.calls[0]?.[2]?.key;
    const stopped = patchIssue.mock.calls.length;

    issueKey = "CORE-2";
    hook.rerender();
    const shown = hook.result.current.error;
    held.answering = true;
    const { retry } = hook.result.current;
    expect(retry).toBeDefined();
    act(() => retry?.());
    await tick(1_000);

    expect(patchIssue).toHaveBeenCalledTimes(stopped + 1);
    const again = patchIssue.mock.calls.at(-1);
    expect(again?.[0]).toBe("CORE-1");
    expect(again?.[1]).toEqual({ assignee: "alice" });
    expect(again?.[2]?.key).toBe(stoppedKey);
    expect(shown).toBe(
      stoppedWriteNote("the assignee of CORE-1", "saved", {
        method: "PATCH",
        stopped: new SendStoppedError(),
      })
    );
  } finally {
    hook.unmount();
    patchIssue.mockRestore();
    realClock();
  }
});

// A pick made during a save waits behind it in its issue's line (`issue:<KEY>`). A pick on
// another issue goes in that issue's own line rather than in place of the one already waiting,
// so that one is still saved, not left on screen with no save behind it. Each line sends its
// picks in the order they were made; the two lines are not ordered against each other.
test("a pick waiting behind a save is still saved when a pick on another issue waits too", async () => {
  const first = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches()
    .mockImplementationOnce(() => first.promise)
    .mockImplementation(async (key, input) => ({ assignee: input.assignee ?? null, key }) as Issue);
  let issueKey = "CORE-1";
  const { hook } = renderOnQueue(() => useIssueAssignee(issueKey));

  try {
    act(() => hook.result.current.submit("alice"));
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(1));
    act(() => hook.result.current.submit("bob"));
    issueKey = "CORE-2";
    hook.rerender();
    act(() => hook.result.current.submit("carol"));

    await act(async () => {
      first.resolve({ assignee: "alice", key: "CORE-1" } as Issue);
      await first.promise;
    });
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(3));
    const sent = patchIssue.mock.calls.map(([key, input]) => [key, input]);
    expect(sent.filter(([key]) => key === "CORE-1")).toEqual([
      ["CORE-1", { assignee: "alice" }],
      ["CORE-1", { assignee: "bob" }],
    ]);
    expect(sent.filter(([key]) => key === "CORE-2")).toEqual([["CORE-2", { assignee: "carol" }]]);
  } finally {
    hook.unmount();
    patchIssue.mockRestore();
  }
});

// A refused save rolls its issue's detail back to what that issue held, and leaves a pick of
// another issue out at the same time showing on that issue: refused in turn, that pick rolls its
// issue back to that issue's own detail, never to the detail of the issue the first save was of.
test("a refused pick of one issue after a refused save of another rolls back to its own detail", async () => {
  const first = Promise.withResolvers<Issue>();
  const second = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches()
    .mockImplementationOnce(() => first.promise)
    .mockImplementationOnce(() => second.promise);
  let issueKey = "CORE-1";
  const { client, hook } = renderOnQueue(() => useIssueAssignee(issueKey));
  const core1 = { assignee: null, key: "CORE-1" } as IssueDetails;
  const core2 = { assignee: "dave", key: "CORE-2" } as IssueDetails;
  client.setQueryData(["issue", "CORE-1"], core1);
  client.setQueryData(["issue", "CORE-2"], core2);

  try {
    act(() => hook.result.current.submit("alice"));
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(1));
    issueKey = "CORE-2";
    hook.rerender();
    // The pick joins its line in a microtask of the press, and shows once it has.
    await act(async () => hook.result.current.submit("carol"));
    expect(client.getQueryData<IssueDetails>(["issue", "CORE-2"])).toEqual({
      ...core2,
      assignee: "carol",
    });
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(2));

    await act(async () => {
      first.reject(new Error("refused"));
    });
    await waitFor(() =>
      expect(client.getQueryData<IssueDetails>(["issue", "CORE-1"])).toEqual(core1)
    );
    expect(client.getQueryData<IssueDetails>(["issue", "CORE-2"])).toEqual({
      ...core2,
      assignee: "carol",
    });
    await act(async () => {
      second.reject(new Error("refused"));
    });

    await waitFor(() =>
      expect(client.getQueryData<IssueDetails>(["issue", "CORE-2"])).toEqual(core2)
    );
    expect(client.getQueryData<IssueDetails>(["issue", "CORE-1"])).toEqual(core1);
  } finally {
    hook.unmount();
    patchIssue.mockRestore();
  }
});
