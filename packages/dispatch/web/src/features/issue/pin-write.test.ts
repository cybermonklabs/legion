import { expect, spyOn, test } from "bun:test";
import { settle } from "../../__tests__/fake-clock";

import {
  answeredByDispatch,
  cacheOf,
  QueryClient,
  QueryObserver,
} from "../../api/cache/test-client";
import { ApiError, api, NetworkError } from "../../api/client";
import { userStateQuery } from "../../api/queries";
import type { UserIssueState, UserState } from "../../api/types";
import { writePin } from "./pin-write";
import { pinnedItemMarker } from "./pins";

/** A page's query client, signed in as `login`. */
function signedIn(login: string): QueryClient {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(["whoami"], { event_head: "0", kind: "user", login });
  return client;
}

const fresh: UserIssueState = { dismissed: [], last_read_seq: 0, pinned: false, seq: 0 };

/**
 * Alice's first pin of CORE-1 lands while her second is still out, so the state its answer gave
 * is what the second writes over; then `end` ends her session on the page. Answers with what the
 * next pin of CORE-1 sends.
 */
async function nextPinAfter(end: (client: QueryClient) => void) {
  const client = signedIn("alice");
  client.setQueryData<UserState>(["user-state"], { "CORE-1": fresh });
  const getMyState = spyOn(api, "getMyState").mockResolvedValue({ "CORE-1": fresh });
  const alicesAnswer: UserIssueState = { ...fresh, dismissed: [pinnedItemMarker("ask:a")], seq: 1 };
  const put = spyOn(api, "putIssueState")
    .mockResolvedValueOnce(answeredByDispatch(alicesAnswer))
    .mockRejectedValueOnce(new NetworkError("/api/v1/me/issues/CORE-1/state", new TypeError()));
  try {
    writePin(cacheOf(client), "CORE-1", { id: "ask:a", op: "pin" });
    writePin(cacheOf(client), "CORE-1", { id: "ask:b", op: "pin" });
    await settle();
    expect(put).toHaveBeenCalledTimes(2);
    end(client);
    client.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "bob" });
    put.mockResolvedValue(answeredByDispatch({ ...fresh, seq: 1 }));
    writePin(cacheOf(client), "CORE-1", { id: "ask:c", op: "pin" });
    await settle();
    return put.mock.calls.at(-1)?.[1];
  } finally {
    cacheOf(client).writes.clear();
    getMyState.mockRestore();
    put.mockRestore();
  }
}

// W36. The issue state a pin's answer left is what the next pin of that issue writes over, so it
// belongs to the person signed in: once someone else signs in, or the person signs out, it goes,
// and so does a pin of theirs dropped unmade. The next pin - bob's - reads his own state afresh
// rather than writing over alice's.
test("another sign-in lets go of the last pin's answer", async () => {
  let rolledBack:
    | { dismissed: string[] | null | undefined; readAgain: boolean | undefined }
    | undefined;
  const sent = await nextPinAfter((client) => {
    cacheOf(client).writes.signedIn("bob");
    // The dropped pin stops being composed as a refused one does: the page shows the state Dispatch
    // last confirmed - alice's first pin, which it made - and reads it again.
    rolledBack = {
      dismissed: cacheOf(client).shown<UserState>(["user-state"])?.["CORE-1"]?.dismissed,
      readAgain: client.getQueryState(["user-state"])?.isInvalidated,
    };
  });
  expect(rolledBack).toEqual({ dismissed: [pinnedItemMarker("ask:a")], readAgain: true });
  expect(sent).toEqual({ dismissed: [pinnedItemMarker("ask:c")], seq: 1 });
});

test("a sign-out lets go of the last pin's answer", async () => {
  const sent = await nextPinAfter((client) => cacheOf(client).writes.clear());
  expect(sent).toEqual({ dismissed: [pinnedItemMarker("ask:c")], seq: 1 });
});

// A pin's body depends on the answer before it: the issue's state as Dispatch last left it, with
// the item pinned or unpinned, under the next sequence. So the re-base stays whatever else the
// store orders (R8 of dispatch://INFRA-299): Dispatch answers a write another tab got in first
// with `STATE_STALE` and its current row, and the pin is written again over that row.
test("a pin Dispatch refuses as stale is written again over the row it answered with", async () => {
  const client = signedIn("alice");
  const getMyState = spyOn(api, "getMyState").mockResolvedValue({ "CORE-1": fresh });
  /** The row another tab left: its own pin, under a sequence this page never read. */
  const current: UserIssueState = { ...fresh, dismissed: [pinnedItemMarker("ask:x")], seq: 7 };
  const put = spyOn(api, "putIssueState")
    .mockRejectedValueOnce(
      new ApiError(409, {
        code: "STATE_STALE",
        error: "user state was updated by another write",
        state: current,
      })
    )
    .mockImplementation(async (_key, body) =>
      answeredByDispatch({ ...fresh, ...body } as UserIssueState)
    );
  try {
    writePin(cacheOf(client), "CORE-1", { id: "ask:a", op: "pin" });
    await settle();
    expect(put.mock.calls.map(([, body]) => body)).toEqual([
      { dismissed: [pinnedItemMarker("ask:a")], seq: 1 },
      { dismissed: [pinnedItemMarker("ask:x"), pinnedItemMarker("ask:a")], seq: 8 },
    ]);
  } finally {
    cacheOf(client).writes.clear();
    getMyState.mockRestore();
    put.mockRestore();
  }
});

// R7 of dispatch://INFRA-299: while the pin is live it shows its change again over each answer
// that lands on the viewer's state. The change is a change to that state as it stands - the
// item's marker in the issue's own `dismissed` list - so the answer's own reading of every other
// issue, and of every other field of this one, arrives whole.
test("a pin shows its change again over an answer to the viewer's state", async () => {
  const client = signedIn("alice");
  const getMyState = spyOn(api, "getMyState").mockResolvedValue({
    "CORE-1": fresh,
    "CORE-2": fresh,
  });
  // A pin the server never answers, so it stays live for the read below.
  const put = spyOn(api, "putIssueState").mockImplementation(() =>
    Promise.withResolvers<UserIssueState>().promise.then(answeredByDispatch)
  );
  const dismissedOf = (issueKey: string) =>
    cacheOf(client).shown<UserState>(["user-state"])?.[issueKey]?.dismissed;
  try {
    await client.fetchQuery(userStateQuery());
    writePin(cacheOf(client), "CORE-1", { id: "ask:a", op: "pin" });
    await settle();
    expect(dismissedOf("CORE-1")).toEqual([pinnedItemMarker("ask:a")]);

    // The page reads the state again while the pin is out: the server has not taken it yet, and
    // another device's read mark on CORE-2 comes with that answer.
    getMyState.mockResolvedValue({ "CORE-1": fresh, "CORE-2": { ...fresh, last_read_seq: 9 } });
    await client.refetchQueries({ queryKey: ["user-state"] });
    await settle();

    expect(dismissedOf("CORE-1")).toEqual([pinnedItemMarker("ask:a")]);
    expect(cacheOf(client).shown<UserState>(["user-state"])?.["CORE-2"]?.last_read_seq).toBe(9);
  } finally {
    cacheOf(client).writes.clear();
    getMyState.mockRestore();
    put.mockRestore();
  }
});

// R10 of dispatch://INFRA-299. Nothing awaits a pin's press to undo it: a refused pin stops being
// composed, so the page shows the viewer's state as Dispatch last gave it, before the state is read
// again. Where that read fails - offline, a 5xx - the page shows no pin Dispatch never took.
test("a refused pin leaves the state Dispatch last gave when the read again fails", async () => {
  const client = signedIn("alice");
  // The page's read, then the pin's own read of the state, then the read again, which fails.
  const getMyState = spyOn(api, "getMyState")
    .mockResolvedValueOnce({ "CORE-1": fresh })
    .mockResolvedValueOnce({ "CORE-1": fresh })
    .mockRejectedValue(new NetworkError("/api/v1/me/state", new TypeError("Failed to fetch")));
  const put = spyOn(api, "putIssueState").mockRejectedValue(
    new ApiError(422, { code: "INVALID_STATE", error: "not a state" })
  );
  // A surface drawing the viewer's state, so the read again goes out.
  const unsubscribe = new QueryObserver(client, userStateQuery()).subscribe(() => {});
  const dismissed = () => cacheOf(client).shown<UserState>(["user-state"])?.["CORE-1"]?.dismissed;
  try {
    await client.fetchQuery(userStateQuery());
    writePin(cacheOf(client), "CORE-1", { id: "ask:a", op: "pin" });
    expect(dismissed()).toEqual([pinnedItemMarker("ask:a")]);
    await settle();
    expect(put).toHaveBeenCalledTimes(1);
    expect(
      cacheOf(client)
        .writes.rows()
        .map((write) => write.status)
    ).toEqual(["refused"]);
    expect(getMyState).toHaveBeenCalledTimes(3);
    expect(client.getQueryState(["user-state"])?.status).toBe("error");
    expect(dismissed()).toEqual([]);
  } finally {
    unsubscribe();
    cacheOf(client).writes.clear();
    getMyState.mockRestore();
    put.mockRestore();
  }
});

// R10 of dispatch://INFRA-299. What a refused pin leaves on the page is the state as Dispatch
// last gave it. Another issue's pin landing while this one is out is merged into the viewer's
// state (`storeIssueState`), which holds Dispatch's answers alone, never this pin's
// change: the merge is Dispatch's answer for CORE-2 alone, not for CORE-1. So when this pin is
// refused and the read again fails, CORE-1 must show what Dispatch last gave it, not the pin
// Dispatch refused.
test("a refused pin does not leave another issue's pin answer carrying it while it was out", async () => {
  const client = signedIn("alice");
  let online = true;
  const getMyState = spyOn(api, "getMyState").mockImplementation(() =>
    online
      ? Promise.resolve({ "CORE-1": fresh, "CORE-2": fresh })
      : Promise.reject(new NetworkError("/api/v1/me/state", new TypeError("Failed to fetch")))
  );
  const refusal = Promise.withResolvers<UserIssueState>();
  const core2Answer: UserIssueState = { ...fresh, dismissed: [pinnedItemMarker("ask:b")], seq: 1 };
  const put = spyOn(api, "putIssueState").mockImplementation((key) =>
    key === "CORE-1"
      ? refusal.promise.then(answeredByDispatch)
      : Promise.resolve(answeredByDispatch(core2Answer))
  );
  // A surface drawing the viewer's state, so the read again goes out.
  const unsubscribe = new QueryObserver(client, userStateQuery()).subscribe(() => {});
  const dismissed = (issueKey: string) =>
    cacheOf(client).shown<UserState>(["user-state"])?.[issueKey]?.dismissed;
  try {
    await client.fetchQuery(userStateQuery());
    writePin(cacheOf(client), "CORE-1", { id: "ask:a", op: "pin" });
    await settle();
    expect(dismissed("CORE-1")).toEqual([pinnedItemMarker("ask:a")]);

    // Another issue's pin lands while CORE-1's is out, and is merged into the state.
    writePin(cacheOf(client), "CORE-2", { id: "ask:b", op: "pin" });
    await settle();
    expect(dismissed("CORE-2")).toEqual([pinnedItemMarker("ask:b")]);
    expect(dismissed("CORE-1")).toEqual([pinnedItemMarker("ask:a")]);

    // Dispatch refuses CORE-1's pin, and the read again fails.
    online = false;
    refusal.reject(new ApiError(422, { code: "INVALID_STATE", error: "not a state" }));
    await settle();
    expect(
      cacheOf(client)
        .writes.rows()
        .map((write) => write.status)
    ).toEqual(["refused"]);
    expect(client.getQueryState(["user-state"])?.status).toBe("error");
    expect(dismissed("CORE-1")).toEqual([]);
    expect(dismissed("CORE-2")).toEqual([pinnedItemMarker("ask:b")]);
  } finally {
    unsubscribe();
    cacheOf(client).writes.clear();
    getMyState.mockRestore();
    put.mockRestore();
  }
});
