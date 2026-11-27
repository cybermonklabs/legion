// The NATS stand-in's own contract with the rows that use it (`./listener-stand-in`).
import { expect, test } from "bun:test";
import {
  currentRow,
  natsStandInConnect,
  natsState,
  resetNatsStandIn,
  rowScope,
  startRow,
} from "./listener-stand-in";

test("an ended row's late subscribe leaves the failure the next row armed to that row's own subscribe", async () => {
  startRow();
  resetNatsStandIn();
  try {
    // A session the row booted keeps that row's scope (`rowScope`) after the row has ended.
    const endedRow = currentRow;
    const ended = await natsStandInConnect({ name: "ended-row", servers: ["nats://stand-in"] });

    // The next row starts, and arms one failure for its own subscribe.
    startRow();
    natsState.failSubscribes = 1;

    // The ended row's session subscribes late.
    const lateOutcome = rowScope.run(endedRow, () => {
      try {
        ended.subscribe("ended.late");
        return "subscribed";
      } catch (error) {
        return error instanceof Error ? error.message : String(error);
      }
    });
    expect(lateOutcome).toBe("subscribed");

    const current = await natsStandInConnect({ name: "current-row", servers: ["nats://stand-in"] });
    expect(() => current.subscribe("current.own")).toThrow("SUBSCRIBE_FAILED");
    expect(natsState.failSubscribes).toBe(0);
    expect(natsState.timeline).toEqual(["subscribe current.own"]);
  } finally {
    resetNatsStandIn();
    startRow();
  }
});
