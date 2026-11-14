import { expect, jest, spyOn, test } from "bun:test";
import { onlineManager } from "@tanstack/react-query";
import { act } from "@testing-library/react";

import { realClock, tick } from "../../__tests__/fake-clock";
import { keptTab } from "../../__tests__/kept-sends-harness";
import { renderOnQueue } from "../../__tests__/query-provider";
import { stopTheLateWrite } from "../../__tests__/stop-late-write";
import { ApiError, api, NetworkError } from "../../api/client";
import { SEND_DEADLINE_MS, SendStoppedError } from "./keyed-write";
import { useKeyedWrite } from "./useKeyedWrite";
import { writesOf } from "./write-store";

const refusal = new ApiError(503, { code: "UNAVAILABLE", error: "the server is down" });
const verdict = new ApiError(422, { code: "KEY_REUSED", error: "the key names another write" });

/** A surface's write of an answer, as `AskCard`'s form makes it, over one app's queue. */
function renderAnswerWrite() {
  return renderOnQueue(() =>
    useKeyedWrite({
      inline: true,
      label: () => "Answer to “Ship?”",
      method: "POST",
      write: (input: { selected: string[] }, bounds) =>
        api.answerAsk("ask-1", { expected_edited_at: null, ...input }, bounds),
    })
  );
}

/** A surface's write whose variables are a string - a document's corrected title, as
 *  `CorrectTitle` makes it - over one app's queue. */
function renderRenameWrite() {
  return renderOnQueue(() =>
    useKeyedWrite({
      inline: true,
      label: (name: string) => `Rename to ${name}`,
      method: "PATCH",
      write: (name: string, bounds) => api.renameArtifact("artifact-1", { name }, bounds),
    })
  );
}

// Decision 3: a write the reader stopped may have reached Dispatch, so its Retry - the surface's
// own Retry of that very press (`retry`) - goes under its own key, and Dispatch answers it with
// what the first did. After Dispatch's own refusal the Retry is a new press under a new key, since
// Dispatch would answer the old one with that refusal for a day.
test("a surface's Retry keeps the key after Stop trying, and takes a new one after Dispatch refused", async () => {
  jest.useFakeTimers();
  const tab = keptTab();
  const answerAsk = spyOn(api, "answerAsk").mockRejectedValue(refusal);
  const { client, hook } = renderAnswerWrite();

  try {
    act(() => hook.result.current.mutate({ selected: ["Ship"] }));
    await stopTheLateWrite(client);
    expect(hook.result.current.error).toBeInstanceOf(SendStoppedError);
    const stoppedKey = answerAsk.mock.calls[0]?.[2]?.key;

    // Retry after the stop: the same press, under the same key.
    answerAsk.mockRejectedValueOnce(verdict);
    act(() => hook.result.current.retry());
    await tick(0);
    expect(answerAsk.mock.calls.at(-1)?.[2]?.key).toBe(stoppedKey);
    expect(hook.result.current.error).toBe(verdict);

    // Retry after Dispatch's refusal: a press of its own.
    answerAsk.mockResolvedValueOnce({ id: "ask-1" } as never);
    act(() => hook.result.current.retry());
    await tick(0);
    expect(answerAsk.mock.calls.at(-1)?.[2]?.key).not.toBe(stoppedKey);
  } finally {
    hook.unmount();
    answerAsk.mockRestore();
    tab.close();
    realClock();
  }
});

// A press the reader stopped belongs to the press that made it, not to the values it carried:
// only the surface's own Retry reaches it. So an action the reader takes later - a form's submit,
// a chip's Approve - is a press of its own under a key of its own, however alike the two read,
// and Dispatch decides it rather than answering with what the stopped press did. A press between
// the two does not come into it: the stopped press is held until its Retry makes it again.
test("a later press built afresh from equal values takes a key of its own", async () => {
  jest.useFakeTimers();
  const tab = keptTab();
  const answerAsk = spyOn(api, "answerAsk").mockRejectedValue(refusal);
  const { client, hook } = renderAnswerWrite();

  try {
    act(() => hook.result.current.mutate({ selected: ["Ship"] }));
    await stopTheLateWrite(client);
    expect(hook.result.current.error).toBeInstanceOf(SendStoppedError);
    const stoppedKey = answerAsk.mock.calls[0]?.[2]?.key;

    // Another answer of this surface, which lands.
    answerAsk.mockResolvedValueOnce({ id: "ask-1" } as never);
    act(() => hook.result.current.mutate({ selected: ["Hold"] }));
    await tick(0);
    expect(hook.result.current.isSuccess).toBe(true);

    // "Ship" again, as the form builds it: the same values, a new press.
    answerAsk.mockResolvedValueOnce({ id: "ask-1" } as never);
    act(() => hook.result.current.mutate({ selected: ["Ship"] }));
    await tick(0);
    const key = answerAsk.mock.calls.at(-1)?.[2]?.key;
    expect(key).not.toBe(stoppedKey);
    expect(key).not.toBe(answerAsk.mock.calls.at(-2)?.[2]?.key);
  } finally {
    hook.unmount();
    answerAsk.mockRestore();
    tab.close();
    realClock();
  }
});

// The other half of that rule: a surface with more than one press out names the stopped press
// its refusal is about (`stopped`) and makes that press again by the handle it was given
// (`again`), and that press goes out under the key its attempts went out under - Dispatch may
// have taken it, and answers the key with what it did there.
test("the press `stopped` handed back, made again, goes under its own key", async () => {
  jest.useFakeTimers();
  const tab = keptTab();
  const answerAsk = spyOn(api, "answerAsk").mockRejectedValue(refusal);
  const { client, hook } = renderAnswerWrite();

  try {
    act(() => hook.result.current.mutate({ selected: ["Ship"] }));
    await stopTheLateWrite(client);
    const stoppedKey = answerAsk.mock.calls[0]?.[2]?.key;

    const stopped = hook.result.current.stopped((variables) => variables.selected[0] === "Ship");
    if (stopped === undefined) {
      throw new Error("the hook held no stopped press to make again");
    }
    answerAsk.mockRejectedValueOnce(verdict);
    act(() => hook.result.current.again(stopped));
    await tick(0);

    expect(answerAsk.mock.calls.at(-1)?.[2]?.key).toBe(stoppedKey);
    expect(hook.result.current.error).toBe(verdict);
  } finally {
    hook.unmount();
    answerAsk.mockRestore();
    tab.close();
    realClock();
  }
});

// A key belongs to the press that made it, not to what the press carries, and a string carries
// nothing but its text: a title typed again is the very string a stopped press of that title was
// made with. Here the stopped rename to "X" had reached Dispatch, a rename to "Y" landed after it,
// and the person renames to "X" again. Under the stopped press's key Dispatch would answer with
// what that press did, its handler not run, and the title would stay "Y" while the surface said
// it was saved. So the press made anew goes under a key of its own.
test("a string pressed anew after its stopped press and another press's landing takes a key of its own", async () => {
  jest.useFakeTimers();
  const tab = keptTab();
  const renameArtifact = spyOn(api, "renameArtifact").mockRejectedValue(refusal);
  const { client, hook } = renderRenameWrite();

  try {
    act(() => hook.result.current.mutate("X"));
    await stopTheLateWrite(client);
    expect(hook.result.current.error).toBeInstanceOf(SendStoppedError);
    const stoppedKey = renameArtifact.mock.calls[0]?.[2]?.key;

    renameArtifact.mockResolvedValueOnce({ id: "artifact-1", name: "Y" } as never);
    act(() => hook.result.current.mutate("Y"));
    await tick(0);
    expect(hook.result.current.isSuccess).toBe(true);
    const landedKey = renameArtifact.mock.calls.at(-1)?.[2]?.key;

    renameArtifact.mockResolvedValueOnce({ id: "artifact-1", name: "X" } as never);
    act(() => hook.result.current.mutate("X"));
    await tick(0);
    const again = renameArtifact.mock.calls.at(-1);
    expect(again?.[1]).toEqual({ name: "X" });
    expect(again?.[2]?.key).not.toBe(stoppedKey);
    expect(again?.[2]?.key).not.toBe(landedKey);
  } finally {
    hook.unmount();
    renameArtifact.mockRestore();
    tab.close();
    realClock();
  }
});

// The same string pressed anew right after its press was stopped is a press of its own too: the
// hook never matches a press by what it carries. The stopped presses stay held, each its own,
// and only a Retry reaches one: `retry` the newest, here the second, and the handle `stopped`
// gives the one it names, here the first, each under the key its attempts went out under.
test("a string stopped and pressed anew takes a key of its own, and only a Retry reaches each stopped press", async () => {
  jest.useFakeTimers();
  const tab = keptTab();
  const renameArtifact = spyOn(api, "renameArtifact").mockRejectedValue(refusal);
  const { client, hook } = renderRenameWrite();

  try {
    act(() => hook.result.current.mutate("X"));
    await stopTheLateWrite(client);
    const firstKey = renameArtifact.mock.calls[0]?.[2]?.key;

    act(() => hook.result.current.mutate("X"));
    await tick(0);
    const secondKey = renameArtifact.mock.calls.at(-1)?.[2]?.key;
    expect(secondKey).not.toBe(firstKey);
    await stopTheLateWrite(client);
    expect(hook.result.current.error).toBeInstanceOf(SendStoppedError);

    // Dispatch's own refusal ends each Retry, so neither press is held again after it.
    renameArtifact.mockRejectedValueOnce(verdict);
    act(() => hook.result.current.retry());
    await tick(0);
    expect(renameArtifact.mock.calls.at(-1)?.[2]?.key).toBe(secondKey);
    expect(hook.result.current.error).toBe(verdict);

    const first = hook.result.current.stopped((name) => name === "X");
    if (first === undefined) {
      throw new Error("the hook held no stopped press to make again");
    }
    renameArtifact.mockRejectedValueOnce(verdict);
    act(() => hook.result.current.again(first));
    await tick(0);
    expect(renameArtifact.mock.calls.at(-1)?.[2]?.key).toBe(firstKey);
    expect(hook.result.current.stopped((name) => name === "X")).toBeUndefined();
  } finally {
    hook.unmount();
    renameArtifact.mockRestore();
    tab.close();
    realClock();
  }
});

// W27. A write that landed with no answer to give - Dispatch answered its key `409 KEY_COMMITTED` -
// is a success with no data: the surface's `onSuccess` runs with `null`, nothing is rolled back,
// nothing shows as failed, and `mutateAsync` resolves.
test("a write answered KEY_COMMITTED succeeds with null: no error and no rollback", async () => {
  const tab = keptTab();
  const answerAsk = spyOn(api, "answerAsk").mockRejectedValue(
    new ApiError(409, { code: "KEY_COMMITTED", error: "already made" })
  );
  let rolledBack = 0;
  const succeeded: unknown[] = [];
  const { client, hook } = renderOnQueue(() =>
    useKeyedWrite({
      inline: true,
      label: () => "Answer to “Ship?”",
      onError: () => {
        rolledBack += 1;
      },
      onSuccess: (data) => succeeded.push(data),
      method: "POST",
      write: (input: { selected: string[] }, bounds) =>
        api.answerAsk("ask-1", { expected_edited_at: null, ...input }, bounds),
    })
  );

  try {
    let resolved: unknown = "unset";
    await act(async () => {
      resolved = await hook.result.current.mutateAsync({ selected: ["Ship"] });
    });
    expect(resolved).toBeNull();
    expect(answerAsk).toHaveBeenCalledTimes(1);
    expect(hook.result.current.isSuccess).toBe(true);
    expect(hook.result.current.data).toBeNull();
    expect(rolledBack).toBe(0);
    expect(succeeded).toEqual([null]);
    expect(writesOf(client).rows()).toEqual([]);
  } finally {
    hook.unmount();
    answerAsk.mockRestore();
    tab.close();
  }
});

// A write pressed while the browser says it is offline is attempted, not paused: its own retries
// wait out the network, so it goes late at its deadline and can be stopped (`networkMode:
// "always"`).
test("a write pressed offline is attempted and goes late", async () => {
  jest.useFakeTimers();
  const online = onlineManager.isOnline();
  const answerAsk = spyOn(api, "answerAsk").mockRejectedValue(
    new NetworkError("/api/v1/asks/ask-1/answer", new TypeError("Failed to fetch"))
  );
  const { client, hook } = renderAnswerWrite();
  try {
    onlineManager.setOnline(false);
    act(() => hook.result.current.mutate({ selected: ["Ship"] }));
    await tick(0);
    expect(answerAsk).toHaveBeenCalledTimes(1);
    await tick(SEND_DEADLINE_MS + 1_000);
    expect(writesOf(client).rows()[0]?.status).toBe("late");
  } finally {
    onlineManager.setOnline(online);
    hook.unmount();
    answerAsk.mockRestore();
    realClock();
  }
});
