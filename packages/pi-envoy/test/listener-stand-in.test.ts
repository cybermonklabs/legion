// The NATS stand-in's own contract with the rows that use it (`./listener-stand-in`): a call that
// a session makes after its row has ended takes nothing the running row armed.
import { expect, test } from "bun:test";
import {
  currentRow,
  natsStandInConnect,
  natsState,
  resetNatsStandIn,
  rowScope,
  startRow,
} from "./listener-stand-in";

const servers = ["nats://stand-in"];

/** Runs `call` in the scope of a row that has ended, as a session that row booted does
 *  (`rowScope`), and answers "ok", or the message of the error it threw. */
async function lateCall(endedRow: symbol, call: () => unknown): Promise<string> {
  return rowScope.run(endedRow, async () => {
    try {
      await call();
      return "ok";
    } catch (error) {
      return error instanceof Error ? error.message : String(error);
    }
  });
}

test("an ended row's late subscribe leaves the failure the next row armed to that row's own subscribe", async () => {
  startRow();
  resetNatsStandIn();
  try {
    const endedRow = currentRow;
    const ended = await natsStandInConnect({ name: "ended-row", servers });

    // The next row starts, and arms one failure for its own subscribe.
    startRow();
    natsState.failSubscribes = 1;

    expect(await lateCall(endedRow, () => ended.subscribe("ended.late"))).toBe("ok");
    const current = await natsStandInConnect({ name: "current-row", servers });
    expect(() => current.subscribe("current.own")).toThrow("SUBSCRIBE_FAILED");
    expect(natsState.failSubscribes).toBe(0);
    expect(natsState.timeline).toEqual(["subscribe current.own"]);
  } finally {
    resetNatsStandIn();
    startRow();
  }
});

test("an ended row's late connect leaves the refusal the next row armed to that row's own connect", async () => {
  startRow();
  resetNatsStandIn();
  try {
    const endedRow = currentRow;

    // The next row starts, and arms one refusal for its own connect.
    startRow();
    natsState.failConnects = 1;

    expect(await lateCall(endedRow, () => natsStandInConnect({ name: "ended-row", servers }))).toBe(
      "ok"
    );
    await expect(natsStandInConnect({ name: "current-row", servers })).rejects.toThrow();
    expect(natsState.failConnects).toBe(0);
    expect(natsState.connectedNames).toEqual([]);
  } finally {
    resetNatsStandIn();
    startRow();
  }
});

test("an ended row's late flush leaves the failure the next row armed to that row's own flush", async () => {
  startRow();
  resetNatsStandIn();
  try {
    const endedRow = currentRow;
    const ended = await natsStandInConnect({ name: "ended-row", servers });

    // The next row starts, subscribes, and arms one failure for its own flush of that subject.
    startRow();
    const current = await natsStandInConnect({ name: "current-row", servers });
    current.subscribe("current.own");
    natsState.failFlushOnSubject = "current.own";

    expect(await lateCall(endedRow, () => ended.flush())).toBe("ok");
    await expect(current.flush()).rejects.toThrow("FLUSH_FAILED current.own");
    expect(natsState.failFlushOnSubject).toBeUndefined();
    expect(natsState.flushes).toEqual([["current.own"]]);
  } finally {
    resetNatsStandIn();
    startRow();
  }
});

test("a flush parked before its row ended leaves the failure the next row armed to that row's own flush", async () => {
  startRow();
  resetNatsStandIn();
  const parked = Promise.withResolvers<void>();
  try {
    const endedRow = currentRow;
    const ended = await natsStandInConnect({ name: "ended-row", servers });

    // The flush starts while its row is the current one, and parks inside the stand-in.
    natsState.heldFlush = parked.promise;
    const parkedFlush = rowScope.run(endedRow, () => ended.flush());

    // The next row starts, subscribes, and arms one failure for its own flush of that subject.
    startRow();
    natsState.heldFlush = undefined;
    const current = await natsStandInConnect({ name: "current-row", servers });
    current.subscribe("current.own");
    natsState.failFlushOnSubject = "current.own";

    // The parked flush comes back after the row that started it has ended.
    parked.resolve();
    await expect(parkedFlush).resolves.toBeUndefined();
    await expect(current.flush()).rejects.toThrow("FLUSH_FAILED current.own");
    expect(natsState.failFlushOnSubject).toBeUndefined();
  } finally {
    parked.resolve();
    resetNatsStandIn();
    startRow();
  }
});
