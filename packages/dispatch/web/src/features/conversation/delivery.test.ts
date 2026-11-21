import { expect, test } from "bun:test";
import {
  DELIVERY_DUPLICATE_WINDOW_MS,
  RECEIPT_TIMEOUT_CAUSE,
  STORE_UNCONFIRMED_CAUSE,
} from "@legion/contracts/dispatch-constants";

import {
  broadcastDelivery,
  type DeliveryOutcome,
  deliveryModesOf,
  deliveryOutcomeState,
  duplicateText,
  isSafeRetry,
  noDeliveryLine,
  plainDelivery,
  queuedParts,
  rowAnsweredWithError,
  safeRetryGuidance,
  strandedRetryGuidance,
  withGuidance,
} from "./delivery";

const now = Date.parse("2026-09-25T12:00:00Z");

function attempt(overrides: Partial<DeliveryOutcome> = {}): DeliveryOutcome {
  return { state: "failed", createdAt: new Date(now - 60_000).toISOString(), ...overrides };
}

// LEGION-271. The promise "Retry won't deliver it twice" is only true while a repeat is still
// recognised (`DELIVERY_DUPLICATE_WINDOW_MS`). Past that window the retry is a second delivery —
// the defect this issue exists to close, reached by acting on a promise that outlived its truth.
test("a fresh failed attempt can still be promised a safe retry", () => {
  expect(isSafeRetry(attempt(), now)).toBe(true);
});

test("a failed attempt older than the duplicate window cannot", () => {
  const stale = attempt({
    createdAt: new Date(now - DELIVERY_DUPLICATE_WINDOW_MS - 1000).toISOString(),
  });
  expect(isSafeRetry(stale, now)).toBe(false);
});

test("the boundary itself is outside the promise", () => {
  const exactly = attempt({
    createdAt: new Date(now - DELIVERY_DUPLICATE_WINDOW_MS).toISOString(),
  });
  expect(isSafeRetry(exactly, now)).toBe(false);
  const justInside = attempt({
    createdAt: new Date(now - DELIVERY_DUPLICATE_WINDOW_MS + 1000).toISOString(),
  });
  expect(isSafeRetry(justInside, now)).toBe(true);
});

test("a delivered attempt has no failure to retry", () => {
  expect(isSafeRetry(attempt({ state: "sent" }), now)).toBe(false);
  expect(isSafeRetry(attempt({ state: "pending" }), now)).toBe(false);
});

// INFRA-626. The listener took a queued attempt and stored it for a session that is not running,
// so there is nothing to retry: a repeat would store a second copy for the same session to read
// when it comes back.
test("a queued attempt is never offered a retry, and reads as stored for a stopped session", () => {
  expect(isSafeRetry(attempt({ state: "queued" }), now)).toBe(false);
  expect(queuedParts({ sessionId: "s1", storedTitle: undefined })).toEqual([
    "Queued: ",
    { sessionId: "s1", storedTitle: undefined },
    " is stopped; it gets the message if it runs again within 72 hours",
  ]);
});

// The route table carries a message attempt's state as a plain string, and the Agents card
// (`attempts.ts`), the conversation model, the Sends strip and the search bar's send read it
// through this one narrowing: a queued attempt must come through it queued, or they draw it as
// sent. A word this build does not know still reads as sent.
test("an attempt's state is read as one of the four, a queued one as queued", () => {
  expect(["pending", "sent", "queued", "failed"].map(deliveryOutcomeState)).toEqual([
    "pending",
    "sent",
    "queued",
    "failed",
  ]);
  expect(deliveryOutcomeState("held")).toBe("sent");
});

test("an attempt the stream already had is not a safe retry either", () => {
  expect(isSafeRetry(attempt({ state: "sent", duplicate: true }), now)).toBe(false);
  expect(isSafeRetry(attempt({ duplicate: true }), now)).toBe(false);
});

// A same-mode Retry repeats the key the stream stored the answered frame under, so a session the
// listener pushes to from the stream would never be handed it. A promise that cannot be kept is
// not made, however young the attempt.
test("an attempt the session answered with an error is not offered a same-mode retry", () => {
  expect(isSafeRetry(attempt({ answeredWithError: true }), now)).toBe(false);
  expect(isSafeRetry(attempt({ answeredWithError: false }), now)).toBe(true);
});

// Every failure Dispatch records itself leaves the envelope id null; only the session's own error
// reply fails a row whose send the listener accepted.
test("a row reads as answered with an error only when a send that reached the listener failed", () => {
  expect(rowAnsweredWithError({ state: "failed", envelope_id: "envelope-1" })).toBe(true);
  expect(rowAnsweredWithError({ state: "failed", envelope_id: null })).toBe(false);
  expect(rowAnsweredWithError({ state: "sent", envelope_id: "envelope-1" })).toBe(false);
});

// The browser's clock is not the server's. `createdAt` is Postgres-stamped and `now` defaults to
// Date.now(), so a client running behind the server sees a NEGATIVE age on an attempt that just
// failed. Since this predicate gates whether the same-mode Retry renders at all, treating that
// as "outside the window" hides the safe action on the primary case - and leaves the
// mode-change buttons, which are a genuine second delivery, as the only thing on offer.
test("a client whose clock trails the server still gets the safe retry", () => {
  const stamped = new Date(now).toISOString();
  for (const skew of [2_000, 30_000, 300_000]) {
    expect(isSafeRetry(attempt({ createdAt: stamped }), now - skew)).toBe(true);
  }
});

test("a createdAt after now is the youngest attempt there is, so it is safest", () => {
  const future = attempt({ createdAt: new Date(now + 1000).toISOString() });
  expect(isSafeRetry(future, now)).toBe(true);
});

test("an unparseable timestamp is not promised anything", () => {
  expect(isSafeRetry(attempt({ createdAt: "not a date" }), now)).toBe(false);
});

// Only the receipt-timeout cause ends in a sentence, so joining with a bare space ran every
// other cause into the guidance: "Failed: no live session Retry won't deliver it twice."
test("the guidance is separated from a cause that has no trailing punctuation", () => {
  expect(withGuidance("Failed: no live session", "Retry won't deliver it twice.")).toBe(
    "Failed: no live session. Retry won't deliver it twice."
  );
  expect(withGuidance(`Failed: ${RECEIPT_TIMEOUT_CAUSE}`, "Retry won't deliver it twice.")).toBe(
    `Failed: ${RECEIPT_TIMEOUT_CAUSE} Retry won't deliver it twice.`
  );
});

// "Sending in a different mode delivers it again" is only true of a send that may already have
// reached the recipient. For a failure that never got there, nothing was delivered to deliver
// "again"; and the mention surface has no mode-change button to name at all.
test("the mode-change clause is offered only for a send that may have landed, and never on a mention", () => {
  const modeChange = "sending in a different mode delivers it again";
  expect(safeRetryGuidance("card", RECEIPT_TIMEOUT_CAUSE)).toContain(modeChange);
  expect(safeRetryGuidance("card", STORE_UNCONFIRMED_CAUSE)).toContain(modeChange);
  expect(safeRetryGuidance("card", "no live session s1")).not.toContain(modeChange);
  expect(safeRetryGuidance("card", null)).not.toContain(modeChange);
  expect(safeRetryGuidance("mention", RECEIPT_TIMEOUT_CAUSE)).not.toContain(modeChange);
  expect(safeRetryGuidance("mention", STORE_UNCONFIRMED_CAUSE)).not.toContain(modeChange);
});

// A host holds the keys it was handed in memory, so a session that restarted after the first
// send (`claude --resume`, a resumed pod) is handed the repeat again. No sentence that promises a
// single delivery may leave that case out (`DELIVERY_DUPLICATE_WINDOW_MS`).
test("every single-delivery promise names the restart it does not survive", () => {
  const inWindow = new Date(now - 60_000).toISOString();
  for (const promise of [
    safeRetryGuidance("card", RECEIPT_TIMEOUT_CAUSE),
    safeRetryGuidance("mention", null),
    duplicateText,
    strandedRetryGuidance("aside", inWindow, now),
    strandedRetryGuidance("aside", undefined, now),
  ]) {
    expect(promise).toContain("unless the session restarted since it was sent");
  }
});

test("a plain message is a Send unless every addressed session takes Aside and one refuses Send", () => {
  const omp = ["aside", "btw", "steer"];
  const claude = ["aside"];
  expect(plainDelivery([])).toBe("steer");
  expect(plainDelivery([omp])).toBe("steer");
  expect(plainDelivery([claude])).toBe("aside");
  expect(plainDelivery([omp, claude])).toBe("aside");
  // BTW is never picked for the reader, and a session with no mode takes none, so it decides
  // nothing for the rest; alone, it leaves Send to be warned about.
  expect(plainDelivery([["btw"]])).toBe("steer");
  expect(plainDelivery([claude, []])).toBe("aside");
  expect(plainDelivery([[]])).toBe("steer");
});

test("a broadcast is a BTW unless every selected session takes Aside and one refuses BTW", () => {
  const omp = ["aside", "btw", "steer"];
  const claude = ["aside"];
  expect(broadcastDelivery([])).toBe("btw");
  expect(broadcastDelivery([omp])).toBe("btw");
  expect(broadcastDelivery([claude])).toBe("aside");
  expect(broadcastDelivery([omp, claude])).toBe("aside");
  // A session with no mode decides nothing for the rest; one that takes neither leaves BTW, which
  // the composer names as leaving it out.
  expect(broadcastDelivery([claude, []])).toBe("aside");
  expect(broadcastDelivery([[]])).toBe("btw");
  expect(broadcastDelivery([claude, ["steer"]])).toBe("btw");
});

// A list can hold capabilities that are no delivery mode this Dispatch knows, so an empty list is
// not the only one that takes no message: every surface asks which of its capabilities are modes.
test("a session's delivery modes are the listed capabilities that are modes, in list order, once", () => {
  expect(deliveryModesOf([])).toEqual([]);
  expect(deliveryModesOf(["future-capability"])).toEqual([]);
  expect(deliveryModesOf(["steer", "x"])).toEqual(["steer"]);
  expect(deliveryModesOf(["btw", "future-capability", "aside"])).toEqual(["btw", "aside"]);
  expect(deliveryModesOf(["aside", "steer", "aside"])).toEqual(["aside", "steer"]);
});

test("the no-delivery-mode line says why an empty list and a list of unknown capabilities take none", () => {
  expect(noDeliveryLine([])).toBe(
    "This session advertises no delivery mode, so a message sent here records a failed attempt. A session advertises none while its plugin cannot reach its conversation."
  );
  expect(noDeliveryLine(["future-capability"])).toBe(
    "This session advertises no delivery mode, so a message sent here records a failed attempt. None of the capabilities it lists is a delivery mode this Dispatch knows."
  );
});

// The line is said of a session that takes no message, so a list holding a mode, beside unknown
// capabilities or not, gives none rather than a sentence that is false of it.
test("a list that holds a delivery mode gives no no-delivery-mode line", () => {
  expect(noDeliveryLine(["aside"])).toBeUndefined();
  expect(noDeliveryLine(["future-capability", "steer"])).toBeUndefined();
});

// A session listing only capabilities that are no mode takes none, as one listing nothing does, so
// it decides neither a plain message's mode nor a broadcast's for the rest.
test("a list with no delivery mode does not decide a plain message's or a broadcast's mode", () => {
  const omp = ["aside", "btw", "steer"];
  const claude = ["aside"];
  const unknown = ["future-capability"];
  expect(plainDelivery([claude, unknown])).toBe(plainDelivery([claude]));
  expect(plainDelivery([omp, unknown])).toBe(plainDelivery([omp]));
  expect(plainDelivery([unknown])).toBe(plainDelivery([]));
  expect(broadcastDelivery([claude, unknown])).toBe(broadcastDelivery([claude]));
  expect(broadcastDelivery([omp, unknown])).toBe(broadcastDelivery([omp]));
  expect(broadcastDelivery([unknown])).toBe(broadcastDelivery([]));
});
