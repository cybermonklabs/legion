import { describe, expect, test } from "bun:test";

import {
  aside,
  type CardWakeHarness,
  cardWakeHarness,
  followUp,
  wakeMessage,
} from "../test/card-wake-host";
import { STARTUP_TURN_CAP_MS, STARTUP_TURN_POLL_MS } from "./card-wake";

// The Legion controller's start message is its session's first turn however slowly the pod
// starts: no clock ends an unsent start-turn mark's hold, only the mark going does, and the Legion
// entry clears it on a fact (`src/card-wake.ts`, "The start turn"). One row per fact, each after
// the unsent mark held for twice the 30 s that main held it for from its first read, so a cap of
// that length put back on the unsent mark sends its wake ahead of the start in every row. The one
// clearing a bound makes, the daemon's registration deadline for its own controller whose claim
// never answers, ends the process and the hold with it: no card is left to wake
// (`packages/daemon/internal/supervise/table.go`, `registrationDeadline`). A sent mark keeps main's
// cap, `STARTUP_TURN_CAP_MS` from the send, until the daemon retries a start it failed to deliver
// (that cap's own doc says why): the rows that send the start hold the sent mark short of its cap,
// and one holds a start the daemon never delivered to that cap and no further.

/** Twice the 30 s main capped an unsent mark's hold at, from the wake's first read of it. */
const TWO_OLD_CAPS_MS = 60_000;

const oneCard = "1 Envoy message above arrived with no turn to read it: read it and act on it.";
const twoCards =
  "2 Envoy messages above arrived with no turn to read them: read them and act on them.";

/** Runs the wake's checks for `ms` of the fake clock, one poll at a time: each sends no wake, and
 *  arms the next read of the mark one poll later. */
async function holdsFor(host: CardWakeHarness, ms: number): Promise<void> {
  for (let held = 0; held < ms; held += STARTUP_TURN_POLL_MS) {
    host.advance(STARTUP_TURN_POLL_MS);
    await host.fire();
    expect(host.sent).toEqual([]);
    expect(host.delays.at(-1)).toBe(STARTUP_TURN_POLL_MS);
  }
}

/** The operator's controller in a terminal: the mark unsent from its `session_start`, and one
 *  card that found the session idle, sent as a follow-up that starts no turn, and seen. */
async function heldTerminal(label: string): Promise<CardWakeHarness> {
  const host = cardWakeHarness({ mode: "tui" });
  host.markUnsent();
  host.setIdle(true);
  const card = host.card(label);
  expect(card.delivery).toMatchObject(followUp);
  host.message(card.message);
  await host.fire();
  expect(host.sent).toEqual([]);
  return host;
}

/** The phase is over once the wake went: the wake armed no further check, and an idle card is an
 *  aside that starts its own turn. */
async function phaseOver(host: CardWakeHarness, sent: number): Promise<void> {
  const armed = host.delays.length;
  await host.fire();
  expect(host.delays).toHaveLength(armed);
  expect(host.card("after-the-wake").delivery).toMatchObject(aside);
  expect(host.sent).toHaveLength(sent);
}

describe("the start-turn mark holds the startup wake until a fact clears it, or a sent mark's cap", () => {
  test("the start sent and taken: a start held 60 s unsent, and sent 29 s before its run begins, is still the session's first turn, and its agent_end sends the one wake", async () => {
    const host = await heldTerminal("before-the-send");
    await holdsFor(host, TWO_OLD_CAPS_MS);
    // The phase holds however long the start takes: a card now is a startup card too.
    const later = host.card("still-held");
    expect(later.delivery).toMatchObject(followUp);
    host.message(later.message);

    // `/legion-controller-start` sends the start, and its run is slow to begin: it begins a poll
    // short of the sent mark's cap.
    host.markSent(host.clock());
    await holdsFor(host, STARTUP_TURN_CAP_MS - STARTUP_TURN_POLL_MS);

    host.startTurn();
    await host.fire();
    expect(host.sent).toEqual([wakeMessage(twoCards)]);
    await phaseOver(host, 1);
  });

  // The daemon's controller in RPC mode is marked sent right before its ready, and a start the
  // daemon fails to deliver at that ready is only logged there: no fact clears the mark, and its
  // cap is what ends the hold (`STARTUP_TURN_CAP_MS`).
  test("the daemon's start never delivered: the sent mark holds the wake to its cap, 30 s from the send, and no further", async () => {
    const host = cardWakeHarness();
    host.markSent(host.clock());
    host.setIdle(true);
    const card = host.card("start-never-delivered");
    expect(card.delivery).toMatchObject(followUp);
    host.message(card.message);
    await host.fire();
    expect(host.sent).toEqual([]);
    await holdsFor(host, STARTUP_TURN_CAP_MS - STARTUP_TURN_POLL_MS);

    // The read at the cap ends the hold with no fact behind it, and sends the one wake.
    host.advance(STARTUP_TURN_POLL_MS);
    await host.fire();
    expect(host.sent).toEqual([wakeMessage(oneCard)]);
    await phaseOver(host, 1);
  });

  test("the claim refused before the send: the unsent mark cleared with no host event releases the wake at the next poll", async () => {
    const host = await heldTerminal("refused-claim");
    await holdsFor(host, TWO_OLD_CAPS_MS);

    host.clearMark();
    expect(host.sent).toEqual([]);
    host.advance(STARTUP_TURN_POLL_MS);
    await host.fire();
    expect(host.sent).toEqual([wakeMessage(oneCard)]);
    await phaseOver(host, 1);
  });

  test("the start's send failing: the sent mark cleared with no run releases the wake at the next poll, before its cap", async () => {
    const host = await heldTerminal("failed-send");
    await holdsFor(host, TWO_OLD_CAPS_MS);
    host.markSent(host.clock());
    await holdsFor(host, STARTUP_TURN_CAP_MS / 2);

    // The host rejected the send, so no run starts: the Legion entry clears the mark on that.
    host.clearMark();
    host.advance(STARTUP_TURN_POLL_MS);
    await host.fire();
    expect(host.sent).toEqual([wakeMessage(oneCard)]);
    await phaseOver(host, 1);
  });

  // A start claim that never answers, or a start command that never runs, leaves the mark unsent:
  // the operator at the terminal is the one who acts, and their first turn is what clears it.
  test("the operator's first turn: its agent_start clears the unsent mark, and its agent_end sends the one wake", async () => {
    const host = await heldTerminal("operator-first");
    await holdsFor(host, TWO_OLD_CAPS_MS);

    host.setIdle(false);
    host.run();
    host.clearMark();
    await host.fire();
    expect(host.sent).toEqual([]);
    host.message({ role: "user", content: "typed first" });
    host.message(host.reply());
    host.setIdle(true);
    host.end();
    await host.fire();
    expect(host.sent).toEqual([wakeMessage(oneCard)]);
    await phaseOver(host, 1);
  });

  test("the session's shutdown: the wake drops the cards it held and sends nothing", async () => {
    const host = await heldTerminal("shutdown");
    await holdsFor(host, TWO_OLD_CAPS_MS);

    // pi-envoy's `session_shutdown` cancels the wake; the Legion entry's clears the mark.
    host.wake.cancel();
    host.clearMark();
    host.advance(STARTUP_TURN_POLL_MS);
    await host.fire();
    expect(host.sent).toEqual([]);
  });
});
