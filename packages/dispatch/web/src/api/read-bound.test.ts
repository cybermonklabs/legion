import { expect, jest, test } from "bun:test";
import { onlineManager, QueryObserver } from "@tanstack/react-query";
import { act, renderHook } from "@testing-library/react";

import { onFakeClock, realClock, settle, tick } from "../__tests__/fake-clock";
import { REQUEST_DEADLINE_MS } from "./client";
import { DispatchQueryClient, waitingForPausedReads } from "./query-client";
import { readWithinBound, useListReadBound, useReadBound } from "./read-bound";

/** What `answer` holds so far: its value once it has settled, else `"waiting"`. */
function watch<T>(answer: Promise<T>): () => T | "waiting" {
  let value: T | "waiting" = "waiting";
  void answer.then((settled) => {
    value = settled;
  });
  return () => value;
}

// A read that settles before the bound is all the hold waits for: it answers at once, with no
// reason, and the clock need not move.
test("a read that settles before the bound ends the wait at once with no reason", async () => {
  await onFakeClock(async () => {
    const read = Promise.withResolvers<void>();
    const answer = watch(readWithinBound(read.promise));
    await settle();
    expect(answer()).toBe("waiting");

    read.resolve();
    await settle();
    expect(answer()).toBeUndefined();
  });
});

// A read that fails settles the wait too: the page shows that failure its own way.
test("a read that fails ends the wait at once with no reason", async () => {
  await onFakeClock(async () => {
    const read = Promise.withResolvers<void>();
    const answer = watch(readWithinBound(read.promise));
    read.reject(new Error("refused"));
    await settle();
    expect(answer()).toBeUndefined();
  });
});

/** Runs `body` on the fake clock with a client of its own whose issues list an observer reads,
 *  once that read has settled and the browser has gone offline. The client is cleared before the
 *  browser comes back online, so a read still paused goes with it and starts no request after the
 *  test. */
async function onOfflineIssues(
  body: (queryClient: DispatchQueryClient) => Promise<void>
): Promise<void> {
  const queryClient = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  queryClient.mount();
  const observer = new QueryObserver(queryClient, {
    queryFn: () => Promise.resolve("listed"),
    queryKey: ["issues"],
  });
  const unsubscribe = observer.subscribe(() => {});
  try {
    await onFakeClock(async () => {
      await settle();
      onlineManager.setOnline(false);
      await body(queryClient);
    });
  } finally {
    unsubscribe();
    queryClient.clear();
    queryClient.unmount();
    onlineManager.setOnline(true);
  }
}

/** Asks for the issues list again while the browser is offline, as the live feed does before a
 *  write's own read, and waits until the network has paused that fetch. */
async function pauseIssuesRead(queryClient: DispatchQueryClient): Promise<void> {
  void queryClient.invalidateQueries({ queryKey: ["issues"] });
  await settle();
  expect(queryClient.getQueryState(["issues"])?.fetchStatus).toBe("paused");
}

// `DispatchQueryClient` answers an invalidation of an idle query with the query's own fetch, which
// TanStack pauses while the browser is offline: that fetch does not settle until the network
// returns, and no request deadline runs while it is paused. The bound is the wait's own timer, so
// the wait ends at the request deadline (`REQUEST_DEADLINE_MS`) all the same, and says the network
// is why.
test("an invalidation paused offline is let go at the request deadline, saying Dispatch could not be reached", async () => {
  await onOfflineIssues(async (queryClient) => {
    const invalidation = queryClient.invalidateQueries({ queryKey: ["issues"] });
    const invalidated = watch(invalidation);
    const answer = watch(readWithinBound(invalidation));
    await tick(REQUEST_DEADLINE_MS - 2);
    expect({
      answer: answer(),
      fetch: queryClient.getQueryState(["issues"])?.fetchStatus,
      invalidation: invalidated(),
    }).toEqual({ answer: "waiting", fetch: "paused", invalidation: "waiting" });

    await tick(1);
    expect({ answer: answer(), invalidation: invalidated() }).toEqual({
      answer: "Dispatch could not be reached",
      invalidation: "waiting",
    });
  });
});

// A query whose own fetch the network has already paused is read again no sooner for a caller
// that waits on it (`waitingForPausedReads`): the invalidation after a write is answered when
// that fetch is, as for a query that was idle, so a hold through it waits out its bound rather
// than ending at once. The project List's offline Take out of epic ended its hold about 0.3 s
// after the press where the list's read was already paused, and the bar went with nothing said
// (INFRA-3135, round 24).
test("a read waiting on a paused fetch is let go at the request deadline, not at once", async () => {
  await onOfflineIssues(async (queryClient) => {
    await pauseIssuesRead(queryClient);
    // The action's own read of the list, asked while that fetch is still paused.
    const invalidation = waitingForPausedReads(queryClient, () =>
      queryClient.invalidateQueries({ queryKey: ["issues"] })
    );
    const answer = watch(readWithinBound(invalidation));
    await tick(REQUEST_DEADLINE_MS - 2);
    expect(answer()).toBe("waiting");

    await tick(1);
    expect(answer()).toBe("Dispatch could not be reached");
  });
});

// Every other caller keeps TanStack's own answer for a query whose fetch is already paused: the
// invalidation resolves at once, so a hold with no bound of its own does not wait on that fetch.
// Only the call that asks waits (`waitingForPausedReads`).
test("an invalidation that does not ask to wait resolves at once for a query whose fetch is paused", async () => {
  await onOfflineIssues(async (queryClient) => {
    await pauseIssuesRead(queryClient);
    const invalidated = watch(queryClient.invalidateQueries({ queryKey: ["issues"] }));
    await settle();
    expect(invalidated()).toBeUndefined();
  });
});

/** Why a read still out online at the bound has not settled, as `readWithinBound` says it. */
const NOT_ANSWERED = `Dispatch did not answer within ${REQUEST_DEADLINE_MS / 1000} seconds`;

// Online, a read can still be out at the bound - a Dispatch slow to answer, a read retried - and
// the wait ends there too, in the words of a read that timed out.
test("a read still out online at the bound is let go, saying Dispatch did not answer in time", async () => {
  await onFakeClock(async () => {
    const answer = watch(readWithinBound(Promise.withResolvers<void>().promise));
    await tick(REQUEST_DEADLINE_MS);
    expect(answer()).toBe(NOT_ANSWERED);
  });
});

// The hook keeps why the read of a hold that ran out has not settled, for the page to show, and
// lets it go once that read settles. A hold whose read settles in time leaves nothing to show.
test("useReadBound shows why a hold's read has not settled until that read settles", async () => {
  const online = onlineManager.isOnline();
  jest.useFakeTimers();
  const hook = renderHook(() => useReadBound());
  try {
    onlineManager.setOnline(false);
    const read = Promise.withResolvers<void>();
    let held = true;
    act(() => {
      void hook.result.current[1](read.promise).then(() => {
        held = false;
      });
    });
    await tick(REQUEST_DEADLINE_MS - 2);
    expect({ held, unsettled: hook.result.current[0] }).toEqual({
      held: true,
      unsettled: undefined,
    });

    await tick(1);
    expect({ held, unsettled: hook.result.current[0] }).toEqual({
      held: false,
      unsettled: "Dispatch could not be reached",
    });

    await act(async () => read.resolve());
    expect(hook.result.current[0]).toBeUndefined();

    let quick = true;
    act(() => {
      void hook.result.current[1](Promise.resolve()).then(() => {
        quick = false;
      });
    });
    await settle();
    expect({ quick, unsettled: hook.result.current[0] }).toEqual({
      quick: false,
      unsettled: undefined,
    });
  } finally {
    onlineManager.setOnline(online);
    hook.unmount();
    realClock();
  }
});

// The page shows why the latest hold's read has not settled, and nothing of an earlier hold's: a
// later hold forgets the earlier reason as it begins, though the earlier read never settles, and
// its own read answering in time leaves nothing to show.
test("useReadBound forgets an earlier hold's reason as a later hold begins", async () => {
  jest.useFakeTimers();
  const hook = renderHook(() => useReadBound());
  try {
    act(() => {
      void hook.result.current[1](Promise.withResolvers<void>().promise);
    });
    await tick(REQUEST_DEADLINE_MS);
    expect(hook.result.current[0]).toBe(NOT_ANSWERED);

    const later = Promise.withResolvers<void>();
    let held = true;
    act(() => {
      void hook.result.current[1](later.promise).then(() => {
        held = false;
      });
    });
    expect(
      { held, unsettled: hook.result.current[0] },
      "the earlier hold's reason outlived the start of a later hold"
    ).toEqual({ held: true, unsettled: undefined });

    await act(async () => later.resolve());
    await settle();
    expect({ held, unsettled: hook.result.current[0] }).toEqual({
      held: false,
      unsettled: undefined,
    });
  } finally {
    hook.unmount();
    realClock();
  }
});

// An earlier hold whose bound ends after a later hold began puts no reason back: what is shown is
// the later hold's alone - nothing while its read is still within its own bound, why it has not
// settled once that bound ends, which the earlier read settling late leaves in place, and nothing
// once its own read settles.
test("useReadBound keeps the later hold's state when an earlier hold's bound ends after it began", async () => {
  jest.useFakeTimers();
  const hook = renderHook(() => useReadBound());
  try {
    const earlier = Promise.withResolvers<void>();
    act(() => {
      void hook.result.current[1](earlier.promise);
    });
    await tick(REQUEST_DEADLINE_MS / 2);
    const later = Promise.withResolvers<void>();
    act(() => {
      void hook.result.current[1](later.promise);
    });

    await tick(REQUEST_DEADLINE_MS / 2);
    expect(
      hook.result.current[0],
      "the earlier hold's bound put its reason over the later hold"
    ).toBeUndefined();

    await tick(REQUEST_DEADLINE_MS / 2);
    expect(hook.result.current[0]).toBe(NOT_ANSWERED);
    await act(async () => earlier.resolve());
    expect(hook.result.current[0]).toBe(NOT_ANSWERED);

    await act(async () => later.resolve());
    expect(hook.result.current[0]).toBeUndefined();
  } finally {
    hook.unmount();
    realClock();
  }
});

// An earlier hold whose bound ended before a later hold began still waits for its read to settle
// (`useUnsettledRead`): that read settling once the later hold's bound has ended takes nothing
// away, and the later hold's reason goes only once the later read settles (`latest`).
test("useReadBound keeps the later hold's reason when an earlier hold's read settles after it", async () => {
  jest.useFakeTimers();
  const hook = renderHook(() => useReadBound());
  try {
    const earlier = Promise.withResolvers<void>();
    act(() => {
      void hook.result.current[1](earlier.promise);
    });
    await tick(REQUEST_DEADLINE_MS);
    const later = Promise.withResolvers<void>();
    act(() => {
      void hook.result.current[1](later.promise);
    });
    await tick(REQUEST_DEADLINE_MS);
    expect(hook.result.current[0]).toBe(NOT_ANSWERED);

    await act(async () => earlier.resolve());
    expect(hook.result.current[0], "an earlier read settling took the later hold's reason").toBe(
      NOT_ANSWERED
    );

    await act(async () => later.resolve());
    expect(hook.result.current[0]).toBeUndefined();
  } finally {
    hook.unmount();
    realClock();
  }
});

// A list's one hold (`useListReadBound`) says why the latest read has not settled for the row
// whose press made it and for no other: another row's press forgets it as its hold begins, though
// that earlier read never settles, and the reason of its own read is said for that row alone.
test("useListReadBound says why the latest read has not settled for the row whose press made it alone", async () => {
  jest.useFakeTimers();
  const hook = renderHook(() => useListReadBound());
  const shown = () => ({
    first: hook.result.current[0]("first"),
    second: hook.result.current[0]("second"),
  });
  try {
    act(() => {
      void hook.result.current[1]("first", Promise.withResolvers<void>().promise);
    });
    await tick(REQUEST_DEADLINE_MS);
    expect(shown()).toEqual({ first: NOT_ANSWERED, second: undefined });

    const later = Promise.withResolvers<void>();
    act(() => {
      void hook.result.current[1]("second", later.promise);
    });
    expect(shown(), "the first row's reason outlived the second row's hold").toEqual({
      first: undefined,
      second: undefined,
    });

    await tick(REQUEST_DEADLINE_MS);
    expect(shown()).toEqual({ first: undefined, second: NOT_ANSWERED });
    await act(async () => later.resolve());
    expect(shown()).toEqual({ first: undefined, second: undefined });
  } finally {
    hook.unmount();
    realClock();
  }
});
