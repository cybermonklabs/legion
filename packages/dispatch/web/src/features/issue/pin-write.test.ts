import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryObserver } from "@tanstack/react-query";

import { settle } from "../../__tests__/fake-clock";
import { api, NetworkError } from "../../api/client";
import { userStateQuery } from "../../api/queries";
import { DispatchQueryClient } from "../../api/query-client";
import type { UserIssueState, UserState } from "../../api/types";
import { writesOf } from "../writes/write-store";
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
  const getMyState = spyOn(api, "getMyState").mockResolvedValue({ "CORE-1": fresh });
  const alicesAnswer: UserIssueState = { ...fresh, dismissed: [pinnedItemMarker("ask:a")], seq: 1 };
  const put = spyOn(api, "putIssueState")
    .mockResolvedValueOnce(alicesAnswer)
    .mockRejectedValueOnce(new NetworkError("/api/v1/me/issues/CORE-1/state", new TypeError()));
  try {
    writePin(client, "CORE-1", { id: "ask:a", op: "pin" });
    writePin(client, "CORE-1", { id: "ask:b", op: "pin" });
    await settle();
    expect(put).toHaveBeenCalledTimes(2);
    end(client);
    client.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "bob" });
    put.mockResolvedValue({ ...fresh, seq: 1 });
    writePin(client, "CORE-1", { id: "ask:c", op: "pin" });
    await settle();
    return put.mock.calls.at(-1)?.[1];
  } finally {
    writesOf(client).clear();
    getMyState.mockRestore();
    put.mockRestore();
  }
}

// W36. The issue state a pin's answer left is what the next pin of that issue writes over, so it
// belongs to the person signed in: once someone else signs in, or the person signs out, it goes,
// and so does a pin of theirs dropped unmade. The next pin - bob's - reads his own state afresh
// rather than writing over alice's.
test("another sign-in lets go of the last pin's answer", async () => {
  let rolledBack = false;
  const sent = await nextPinAfter((client) => {
    const invalidate = spyOn(client, "invalidateQueries");
    writesOf(client).signedIn("bob");
    // The dropped pin is undone as a refused one is: the page reads its state again.
    rolledBack = invalidate.mock.calls.some(
      ([filters]) => JSON.stringify(filters?.queryKey) === '["user-state"]'
    );
    invalidate.mockRestore();
  });
  expect(rolledBack).toBe(true);
  expect(sent).toEqual({ dismissed: [pinnedItemMarker("ask:c")], seq: 1 });
});

test("a sign-out lets go of the last pin's answer", async () => {
  const sent = await nextPinAfter((client) => writesOf(client).clear());
  expect(sent).toEqual({ dismissed: [pinnedItemMarker("ask:c")], seq: 1 });
});

/** Dispatch's answers to the reads of the viewer's issue states, each held until the test gives
 *  it: `asked[n]` is the read made n-th, the page's and a pin's own alike. */
function heldStateReads() {
  const asked: ReturnType<typeof Promise.withResolvers<UserState>>[] = [];
  const getMyState = spyOn(api, "getMyState").mockImplementation(() => {
    const read = Promise.withResolvers<UserState>();
    asked.push(read);
    return read.promise;
  });
  return { asked, getMyState };
}

/** The page's read of the viewer's issue states, mounted on `client` until the answer is called. */
function readStates(client: QueryClient): () => void {
  return new QueryObserver(client, userStateQuery()).subscribe(() => {});
}

/** Asks the viewer's issue states again, as an event's refresh or another surface does. */
function readStatesAgain(client: QueryClient): void {
  void client.invalidateQueries({ queryKey: userStateQuery().queryKey });
}

/** CORE-1's state as the page shows it. */
function shown(client: QueryClient): UserIssueState | undefined {
  return client.getQueryData<UserState>(userStateQuery().queryKey)?.["CORE-1"];
}

/** Dispatch's answer to alice's pin of `ask:a` on CORE-1. */
const pinAnswer: UserIssueState = { ...fresh, dismissed: [pinnedItemMarker("ask:a")], seq: 1 };

// A read of the issue states asked while a pin was out reached Dispatch before the pin did. When
// it lands after the pin's answer went in, the item the reader pinned must not show unpinned
// again.
test("a read asked before a pin's answer does not unpin the item when it answers", async () => {
  const client = signedIn("alice");
  const reads = heldStateReads();
  const write = Promise.withResolvers<UserIssueState>();
  const put = spyOn(api, "putIssueState").mockReturnValue(write.promise);
  const stopReading = readStates(client);
  try {
    await settle();
    reads.asked[0]?.resolve({ "CORE-1": fresh });
    await settle();

    writePin(client, "CORE-1", { id: "ask:a", op: "pin" });
    await settle();
    // The pin's own read of the state it writes over.
    reads.asked[1]?.resolve({ "CORE-1": fresh });
    await settle();
    expect(put).toHaveBeenCalledTimes(1);

    readStatesAgain(client);
    await settle();
    expect(reads.asked).toHaveLength(3);

    write.resolve(pinAnswer);
    await settle();
    expect(shown(client)).toEqual(pinAnswer);
    reads.asked[2]?.resolve({ "CORE-1": fresh });
    await settle();
    expect(shown(client)).toEqual(pinAnswer);
  } finally {
    stopReading();
    writesOf(client).clear();
    reads.getMyState.mockRestore();
    put.mockRestore();
  }
});

// A read of the issue states that was out when the reader pinned an item was answered from the
// states before the pin. When it lands after the press, the item must stay pinned on the page
// while the pin is out.
test("a read asked before a pin press does not unpin the item when it answers", async () => {
  const client = signedIn("alice");
  const reads = heldStateReads();
  const write = Promise.withResolvers<UserIssueState>();
  const put = spyOn(api, "putIssueState").mockReturnValue(write.promise);
  const stopReading = readStates(client);
  try {
    await settle();
    reads.asked[0]?.resolve({ "CORE-1": fresh });
    await settle();
    readStatesAgain(client);
    await settle();
    expect(reads.asked).toHaveLength(2);

    writePin(client, "CORE-1", { id: "ask:a", op: "pin" });
    expect(shown(client)?.dismissed).toEqual([pinnedItemMarker("ask:a")]);
    await settle();
    // The pin reads the state for itself, last; a read the press gave up and asked again comes
    // before it, and is left out here.
    reads.asked.at(-1)?.resolve({ "CORE-1": fresh });
    await settle();
    expect(put).toHaveBeenCalledTimes(1);

    // The read asked before the press answers the state from before it.
    reads.asked[1]?.resolve({ "CORE-1": fresh });
    await settle();
    expect(shown(client)?.dismissed).toEqual([pinnedItemMarker("ask:a")]);

    write.resolve(pinAnswer);
    await settle();
    expect(shown(client)).toEqual(pinAnswer);
  } finally {
    stopReading();
    writesOf(client).clear();
    reads.getMyState.mockRestore();
    put.mockRestore();
  }
});

// A read of the issue states asked while a pin is out could reach Dispatch before the pin does,
// and its answer, the item unpinned, would unpin it on the page while the pin is still out. The
// read waits until the pin is decided (`holdBackReads`), and then reads what Dispatch holds after
// the pin.
test("a read asked while a pin is out waits until the pin is decided", async () => {
  const client = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  const reads = heldStateReads();
  const write = Promise.withResolvers<UserIssueState>();
  const put = spyOn(api, "putIssueState").mockReturnValue(write.promise);
  const stopReading = readStates(client);
  try {
    await settle();
    reads.asked[0]?.resolve({ "CORE-1": fresh });
    await settle();

    writePin(client, "CORE-1", { id: "ask:a", op: "pin" });
    await settle();
    // The pin's own read of the state it writes over, which no hold keeps back.
    reads.asked[1]?.resolve({ "CORE-1": fresh });
    await settle();
    expect(put).toHaveBeenCalledTimes(1);

    readStatesAgain(client);
    await settle();
    expect(reads.getMyState).toHaveBeenCalledTimes(2);
    expect(shown(client)?.dismissed).toEqual([pinnedItemMarker("ask:a")]);

    write.resolve(pinAnswer);
    await settle();
    expect(shown(client)).toEqual(pinAnswer);

    // The read goes out once the pin is decided, and what Dispatch holds now - another issue read
    // in another browser as well - is what the page shows.
    expect(reads.getMyState).toHaveBeenCalledTimes(3);
    const after: UserState = { "CORE-1": pinAnswer, "CORE-2": { ...fresh, last_read_seq: 4 } };
    reads.asked[2]?.resolve(after);
    await settle();
    expect(client.getQueryData<UserState>(userStateQuery().queryKey)).toEqual(after);
  } finally {
    stopReading();
    writesOf(client).clear();
    reads.getMyState.mockRestore();
    put.mockRestore();
  }
});
