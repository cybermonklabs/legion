import { expect, jest, spyOn, test } from "bun:test";
import { onlineManager, QueryClientProvider, QueryObserver } from "@tanstack/react-query";
import { act, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";

import { type HeldNotifications, holdNotifications } from "../../__tests__/commits";
import { realClock, settle, tick } from "../../__tests__/fake-clock";
import { keptTab } from "../../__tests__/kept-sends-harness";
import { renderOnQueue } from "../../__tests__/query-provider";
import { stopTheLateWrite } from "../../__tests__/stop-late-write";
import { ApiError, api, NetworkError } from "../../api/client";
import { DispatchQueryClient } from "../../api/query-client";
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

/** The same write of an answer, but one the shell's pending writes show, with their Retry, once
 *  the reader stopped it, as they show a header's pin: its surface shows no refusal itself. */
function renderShownAnswerWrite() {
  return renderOnQueue(() =>
    useKeyedWrite({
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
// only a Retry of that press reaches it, the surface's own or the shell's. So an action the reader
// takes later - a form's submit, a chip's Approve - is a press of its own under a key of its own,
// however alike the two read, and Dispatch decides it rather than answering with what the stopped
// press did. A press between the two does not come into it: the stopped press is held until a
// Retry makes it again.
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

// The shell's Retry makes a stopped press again under its key, outside the surface's hook
// (`Writes.retry`), for a write the shell shows: one its surface does not show the refusal of
// itself, as a header's pin. Once it lands, the hook holds that press no longer and its newest
// press no longer ends stopped: the surface's note that Dispatch has not confirmed it goes, and
// its own Retry has nothing to make again.
test("a stopped press the store's Retry lands is the hook's stopped press no longer", async () => {
  jest.useFakeTimers();
  const tab = keptTab();
  const answerAsk = spyOn(api, "answerAsk").mockRejectedValue(refusal);
  const { client, hook } = renderShownAnswerWrite();

  try {
    act(() => hook.result.current.mutate({ selected: ["Ship"] }));
    await stopTheLateWrite(client);
    expect(hook.result.current.error).toBeInstanceOf(SendStoppedError);
    const stoppedKey = answerAsk.mock.calls[0]?.[2]?.key;
    const tries = answerAsk.mock.calls.length;
    const [row, ...more] = writesOf(client).rows();
    expect(row?.key, "the shell shows no stopped write to retry").toBe(stoppedKey);
    expect(more).toEqual([]);

    answerAsk.mockResolvedValueOnce({ id: "ask-1" } as never);
    act(() => writesOf(client).retry(row?.id ?? 0));
    await tick(0);

    expect(answerAsk.mock.calls.length).toBe(tries + 1);
    expect(answerAsk.mock.calls.at(-1)?.[2]?.key).toBe(stoppedKey);
    expect(writesOf(client).rows(), "the store's Retry did not land").toEqual([]);
    expect(hook.result.current.error, "the landed press still ends stopped").toBeNull();
    expect(hook.result.current.method).toBeUndefined();
    expect(
      hook.result.current.stopped(() => true),
      "the hook still holds the landed press"
    ).toBe(undefined);
  } finally {
    hook.unmount();
    answerAsk.mockRestore();
    tab.close();
    realClock();
  }
});

// The order review 11 traced: the reader makes a new press, and the shell's Retry of the stopped
// press lands before React draws that new press. TanStack tells React of a press on a timer of its
// own (`holdNotifications`), and here the press and the landing fall in one task, so React draws
// neither before both are done. The landing lets the stopped press go and nothing more: the new
// press stays pending, and Dispatch's refusal of it is the hook's error, which a surface says. A
// surface makes a press by `mutate` or by `mutateAsync` (a label's or an agent pin's save), and
// each must count as the press the hook made last.
for (const made of ["mutate", "mutateAsync"] as const) {
  test(`a stopped press the store's Retry lands before a newer press is drawn leaves that press its state (${made})`, async () => {
    jest.useFakeTimers();
    const tab = keptTab();
    const answerAsk = spyOn(api, "answerAsk").mockRejectedValue(refusal);
    const { client, hook } = renderShownAnswerWrite();
    let notifications: HeldNotifications | undefined;

    try {
      act(() => hook.result.current.mutate({ selected: ["Ship"] }));
      await stopTheLateWrite(client);
      const [row] = writesOf(client).rows();
      const stoppedKey = row?.key;
      const retried = Promise.withResolvers<{ id: string }>();
      const newer = Promise.withResolvers<never>();
      answerAsk
        .mockImplementationOnce(() => retried.promise as never)
        .mockImplementationOnce(() => newer.promise);
      act(() => writesOf(client).retry(row?.id ?? 0));
      await tick(0);
      expect(answerAsk.mock.calls.at(-1)?.[2]?.key, "the shell's Retry did not go out").toBe(
        stoppedKey
      );

      notifications = holdNotifications();
      await act(async () => {
        if (made === "mutate") hook.result.current.mutate({ selected: ["Hold"] });
        else void hook.result.current.mutateAsync({ selected: ["Hold"] }).catch(() => undefined);
        await settle();
        retried.resolve({ id: "ask-1" });
        await settle();
      });
      expect(answerAsk.mock.calls.at(-1)?.[1]?.selected, "the newer press did not go out").toEqual([
        "Hold",
      ]);
      expect(
        writesOf(client)
          .rows()
          .map((write) => write.key),
        "the shell's Retry did not land"
      ).not.toContain(stoppedKey);
      expect(
        notifications.held(),
        "React was told of the newer press before the landing, so the order is not held"
      ).toBeGreaterThan(0);
      notifications.tell();

      expect(hook.result.current.status, "the stopped press's landing reset the newer press").toBe(
        "pending"
      );
      expect(hook.result.current.variables).toEqual({ selected: ["Hold"] });
      newer.reject(verdict);
      await tick(0);
      expect(hook.result.current.error, "the newer press's refusal left the hook").toBe(verdict);
    } finally {
      notifications?.tell();
      hook.unmount();
      answerAsk.mockRestore();
      tab.close();
      realClock();
    }
  });
}

// A surface with two stopped presses makes the older one again by its handle (`again`), and
// React draws that press; then the shell's Retry of the newer one lands. The press the hook made
// last is the one `again` made, so the landing lets the newer stopped press go and leaves the
// older press's state: it is pending, and Dispatch's refusal of it is the hook's error.
test("a stopped press the store's Retry lands after `again` made an older one leaves that one its state", async () => {
  jest.useFakeTimers();
  const tab = keptTab();
  const answerAsk = spyOn(api, "answerAsk").mockRejectedValue(refusal);
  const { client, hook } = renderShownAnswerWrite();
  const stopLate = async () => {
    await tick(SEND_DEADLINE_MS + 1_000);
    const late = writesOf(client)
      .rows()
      .find((write) => write.status === "late");
    expect(late, "no write went late to stop").toBeDefined();
    act(() => {
      writesOf(client).stop(late?.id ?? 0);
    });
    await tick(60_000);
  };

  try {
    act(() => hook.result.current.mutate({ selected: ["Ship"] }));
    await stopLate();
    act(() => hook.result.current.mutate({ selected: ["Hold"] }));
    await stopLate();
    const older = hook.result.current.stopped((variables) => variables.selected[0] === "Ship");
    if (older === undefined) throw new Error("the hook held no stopped press of Ship");
    const newerRow = writesOf(client)
      .rows()
      .find((write) => write.key !== answerAsk.mock.calls[0]?.[2]?.key);
    expect(newerRow, "the shell shows no stopped press of Hold").toBeDefined();

    const again = Promise.withResolvers<never>();
    const retried = Promise.withResolvers<{ id: string }>();
    answerAsk
      .mockImplementationOnce(() => again.promise)
      .mockImplementationOnce(() => retried.promise as never);
    act(() => hook.result.current.again(older));
    await tick(0);
    expect(hook.result.current.variables, "the older press was not drawn").toEqual({
      selected: ["Ship"],
    });
    act(() => writesOf(client).retry(newerRow?.id ?? 0));
    await tick(0);
    retried.resolve({ id: "ask-1" });
    await tick(0);

    expect(hook.result.current.status, "the newer press's landing reset the older press").toBe(
      "pending"
    );
    again.reject(verdict);
    await tick(0);
    expect(hook.result.current.error, "the older press's refusal left the hook").toBe(verdict);
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

/** A write whose optimistic value stands in `["things"]` (`holds`), under Dispatch's own client,
 *  with a read of that query mounted: what each request of the read answered, in order, and the
 *  answer the test gives the write. */
function renderHeldWrite() {
  const client = new DispatchQueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  client.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  const server = { value: "before" };
  const reads: string[] = [];
  const stopReading = new QueryObserver(client, {
    queryFn: () => {
      reads.push(server.value);
      return Promise.resolve(server.value);
    },
    queryKey: ["things"],
  }).subscribe(() => {});
  const answer = Promise.withResolvers<string>();
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={client}>{children}</QueryClientProvider>
  );
  const hook = renderHook(
    () =>
      useKeyedWrite<string, void, { previous: string | undefined }>({
        holds: () => ({ keys: [["things"]] }),
        inline: true,
        label: () => "A thing",
        method: "POST",
        onError: (_error, _variables, context) => {
          client.setQueryData(["things"], context?.previous);
        },
        onMutate: () => {
          const previous = client.getQueryData<string>(["things"]);
          client.setQueryData(["things"], "optimistic");
          return { previous };
        },
        onSuccess: (data) => {
          client.setQueryData(["things"], data);
        },
        // TanStack waits for this before the write stops being pending: a hold kept until then
        // would keep this read waiting for the write, and the write for this read.
        onSettled: () => client.invalidateQueries({ queryKey: ["things"] }),
        write: () => answer.promise,
      }),
    { wrapper }
  );
  return { answer, client, hook, reads, server, stopReading };
}

/** Waits long enough for a read that is not held to have asked. */
function aMoment(): Promise<void> {
  const { promise, resolve } = Promise.withResolvers<void>();
  setTimeout(resolve, 20);
  return promise;
}

// A read asked while an optimistic write is out could be answered before Dispatch takes the
// write, and would put back the value the press replaced. It waits for the write's answer, which
// it then reads; the write still settles though its `onSettled` reads the query again.
test("a write that holds its query keeps a read asked while it is out until its answer is in", async () => {
  const tab = keptTab();
  const { answer, client, hook, reads, server, stopReading } = renderHeldWrite();
  try {
    await waitFor(() => expect(reads).toEqual(["before"]));
    act(() => hook.result.current.mutate());
    await waitFor(() => expect(client.getQueryData<string>(["things"])).toBe("optimistic"));
    await act(async () => {
      void client.invalidateQueries({ queryKey: ["things"] });
      await aMoment();
    });
    expect(reads).toEqual(["before"]);
    expect(client.getQueryData<string>(["things"])).toBe("optimistic");

    server.value = "after";
    await act(async () => {
      answer.resolve("after");
      await aMoment();
    });
    await waitFor(() => expect(hook.result.current.isSuccess).toBe(true));
    expect(reads.length).toBeGreaterThan(1);
    expect(reads.slice(1).every((read) => read === "after")).toBe(true);
    expect(client.getQueryData<string>(["things"])).toBe("after");
  } finally {
    hook.unmount();
    stopReading();
    tab.close();
  }
});

// A refused write puts the value from before the press back, and the reads it held then go.
test("a write that holds its query lets the reads it held go once its refusal is rolled back", async () => {
  const tab = keptTab();
  const { answer, client, hook, reads, stopReading } = renderHeldWrite();
  try {
    await waitFor(() => expect(reads).toEqual(["before"]));
    act(() => hook.result.current.mutate());
    await act(async () => {
      void client.invalidateQueries({ queryKey: ["things"] });
      await aMoment();
    });
    expect(reads).toEqual(["before"]);

    await act(async () => {
      answer.reject(verdict);
      await aMoment();
    });
    await waitFor(() => expect(hook.result.current.isError).toBe(true));
    expect(reads.length).toBeGreaterThan(1);
    expect(client.getQueryData<string>(["things"])).toBe("before");
  } finally {
    hook.unmount();
    stopReading();
    tab.close();
  }
});

// A held read keeps back whatever anybody else changed in its query too, so a write whose answer
// does not come holds it no longer than its deadline: a read asked while it is out waits until
// `SEND_DEADLINE_MS` after the press, and then asks, though the write is still out, and late.
test("a write that holds its query lets a read asked while it is out go at its deadline, with no answer in", async () => {
  jest.useFakeTimers();
  const tab = keptTab();
  const { client, hook, reads, server, stopReading } = renderHeldWrite();
  try {
    await tick(0);
    expect(reads).toEqual(["before"]);
    act(() => hook.result.current.mutate());
    await tick(0);
    expect(client.getQueryData<string>(["things"])).toBe("optimistic");
    await act(async () => {
      void client.invalidateQueries({ queryKey: ["things"] });
    });
    // Another person's change, which the held read would bring.
    server.value = "elsewhere";

    await tick(SEND_DEADLINE_MS - 1_000);
    expect(reads).toEqual(["before"]);
    expect(client.getQueryData<string>(["things"])).toBe("optimistic");

    await tick(1_000);
    expect(reads).toEqual(["before", "elsewhere"]);
    expect(client.getQueryData<string>(["things"])).toBe("elsewhere");
    expect(hook.result.current.isPending).toBe(true);
    expect(writesOf(client).rows()[0]?.status).toBe("late");
  } finally {
    hook.unmount();
    stopReading();
    tab.close();
    realClock();
  }
});
