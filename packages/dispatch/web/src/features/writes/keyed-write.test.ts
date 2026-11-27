import { expect, jest, test } from "bun:test";
import { IDEMPOTENCY_KEY_TTL_MS } from "@legion/contracts";
import { settle } from "../../__tests__/fake-clock";
import { keyMinted } from "../../__tests__/kept-sends-harness";
import { ApiError, NetworkError, type WriteMethod } from "../../api/client";
import {
  keyExpired,
  keySpent,
  MAX_BACKOFF_MS,
  refusedByDispatch,
  retriesUnderItsKey,
  retryNote,
  SendExpiredError,
  SendStoppedError,
  stoppedWriteNote,
  writeUnderKey,
} from "./keyed-write";

/** A v7 key minted at `MINTED_AT`, as `newSendKey` mints one (`keyMinted`). */
const MINTED_AT = 1_700_000_000_000;
const V7_KEY = keyMinted(MINTED_AT);

test("a key is spent once Dispatch's whole key lifetime has passed since it was minted", () => {
  expect(keyExpired(V7_KEY, MINTED_AT + IDEMPOTENCY_KEY_TTL_MS)).toBe(true);
  expect(keyExpired(V7_KEY, MINTED_AT + IDEMPOTENCY_KEY_TTL_MS - 1)).toBe(false);
});

// A key minted before this release holds no mint time, and calling it spent would mint a new key
// for a write Dispatch may already have done.
test("a key with no mint time is never spent", () => {
  const key = "3f2a8b1c-5d4e-4a7b-9c2d-0e1f2a3b4c5d";
  expect(keyExpired(key, MINTED_AT + 10 * IDEMPOTENCY_KEY_TTL_MS)).toBe(false);
});

// One question, asked in one place, of what the key has already carried: the method that went
// out under it, or `null` where nothing did. `refusalFrom` asks it of what the write's record
// says went out (`KeptWrite.sent`), and the retry loop asks it of what the
// store says the key carried and of its own attempts, so the two cannot answer the same write
// differently.
test("a key is spent for what went out under it, never for a key that carried nothing", () => {
  const spent = keyMinted(MINTED_AT);
  const now = MINTED_AT + IDEMPOTENCY_KEY_TTL_MS;
  expect(keySpent("POST", spent, now)).toBe(true);
  expect(keySpent("PATCH", spent, now)).toBe(true);
  expect(keySpent("PUT", spent, now)).toBe(false);
  expect(keySpent("DELETE", spent, now)).toBe(false);
  expect(keySpent("POST", spent, now - 1)).toBe(false);
  // Nothing went out under it: Dispatch has never seen the key, so there is nothing under it
  // that a request now would write a second time.
  expect(keySpent(null, spent, now)).toBe(false);
});

// A key Dispatch has forgotten, which a `POST` already went out under, names nothing there any
// more: a request under it would be a second write of what the person pressed rather than a
// repeat of the first. The write makes no attempt at all and ends as a stopped write does, so
// every surface that branches on that class keeps offering Retry and Discard beside it.
test("a write whose key is older than the key's lifetime sends no attempt", async () => {
  let attempts = 0;
  const refusal = await writeUnderKey(
    () => {
      attempts += 1;
      return Promise.resolve({ key: "CORE-1" });
    },
    keyMinted(Date.now() - IDEMPOTENCY_KEY_TTL_MS),
    { carried: "POST" }
  ).catch((error: unknown) => error);
  expect(attempts).toBe(0);
  expect(refusal).toBeInstanceOf(SendExpiredError);
  expect(refusal).toBeInstanceOf(SendStoppedError);
});

// A write whose key has carried nothing sent nothing, whatever its age: a page that kept it
// across a reload without ever sending it, whose Retry comes a day after the press. Dispatch
// has never seen the key, so the attempt still to come is the first one and it goes out - it is
// not held back to be refused, and then sent only on a second Retry under a new key.
test("a write under a key that has carried nothing makes its attempt a day later", async () => {
  let attempts = 0;
  const answer = await writeUnderKey(
    () => {
      attempts += 1;
      return Promise.resolve({ key: "CORE-1" });
    },
    keyMinted(Date.now() - IDEMPOTENCY_KEY_TTL_MS),
    { carried: null }
  );
  expect(attempts).toBe(1);
  expect(answer).toEqual({ key: "CORE-1" });
});

// The key carries from the moment its first request goes out, which the loop is told as it goes:
// that request may have reached Dispatch even where the attempt ended without an answer, so the
// attempt after it is the one the spent key stops.
test("a key spent before the write began carries its first attempt and stops the second", async () => {
  let attempts = 0;
  const refusal = await writeUnderKey(
    (bounds) => {
      attempts += 1;
      bounds.sent?.({ body: { title: "A thing" }, method: "POST", path: "/api/v1/issues" });
      return Promise.reject(new ApiError(503, { code: "UNAVAILABLE", error: "down" }));
    },
    keyMinted(Date.now() - IDEMPOTENCY_KEY_TTL_MS),
    { carried: null }
  ).catch((error: unknown) => error);
  expect(attempts).toBe(1);
  expect(refusal).toBeInstanceOf(SendExpiredError);
});

/** Whether the error a write ends with is Dispatch's verdict that it made nothing
 *  (`refusedByDispatch`), where its attempts end with `failures` in order, each after the page's
 *  wait, for a person still signed in, so a second `401` is Dispatch's refusal. `carried` is what
 *  an earlier try of the write sent under its key (`KeyedWriteOptions.carried`). */
async function verdictAfter(
  failures: readonly Error[],
  carried: WriteMethod | null = null
): Promise<boolean> {
  jest.useFakeTimers();
  try {
    let attempts = 0;
    const ended = writeUnderKey(
      () => {
        const failure = failures[Math.min(attempts, failures.length - 1)];
        attempts += 1;
        return Promise.reject(failure);
      },
      keyMinted(Date.now()),
      { carried, unauthorized: async () => true }
    ).catch((error: unknown) => error);
    for (let wait = 1; wait < failures.length; wait += 1) {
      await settle();
      jest.advanceTimersByTime(MAX_BACKOFF_MS);
    }
    const error = await ended;
    expect(attempts).toBe(failures.length);
    expect(error).toBe(failures.at(-1));
    return refusedByDispatch(error);
  } finally {
    jest.useRealTimers();
  }
}

// A refusal says Dispatch made nothing only where no attempt went unanswered before it. An attempt
// the page had no answer to - the connection failed - or one answered with a 5xx or a 408 may have
// landed: a 5xx can come after Dispatch committed the write, and a proxy in front of Dispatch can
// answer 408 after it forwarded the request. A retry Dispatch then refuses says nothing of it: a
// snooze is a `PUT` and sends no key, so its retry runs Dispatch's handler again, which refuses a
// moment that has passed or an ask that has gone; and a second `401` refuses any write. That
// write's event may still come.
test("a refusal after an attempt the page had no answer to is no verdict that nothing was made", async () => {
  // Each attempt's error is an object of its own, as each answer is.
  const pastMoment = () =>
    new ApiError(400, { code: "SNOOZE_INPUT", error: "snoozed_until is in the past" });
  const lost = () =>
    new NetworkError("/api/v1/asks/ask-1/snooze", new TypeError("Failed to fetch"));
  const lapsed = () => new ApiError(401, { code: "UNAUTHORIZED", error: "sign in" });

  expect(await verdictAfter([pastMoment()])).toBe(true);
  expect(await verdictAfter([lapsed(), lapsed()])).toBe(true);
  expect(await verdictAfter([lost(), pastMoment()])).toBe(false);
  expect(
    await verdictAfter([
      new ApiError(503, { code: "UNAVAILABLE", error: "down" }),
      new ApiError(404, { code: "ASK_NOT_FOUND", error: "no such ask" }),
    ])
  ).toBe(false);
  expect(
    await verdictAfter([
      new ApiError(408, { error: "request timeout" }),
      new ApiError(404, { code: "ASK_NOT_FOUND", error: "no such ask" }),
    ])
  ).toBe(false);
  // A 429 turns the request away before it runs, so the refusal after it is still a verdict.
  expect(
    await verdictAfter([
      new ApiError(429, { code: "RATE_LIMITED", error: "slow down" }),
      new ApiError(404, { code: "ASK_NOT_FOUND", error: "no such ask" }),
    ])
  ).toBe(true);
  expect(await verdictAfter([lost(), lapsed(), lapsed()])).toBe(false);
});

// A write can be tried more than once: a lapse stops its try, and the page takes it up in a try
// of its own once the same person signs in again. An attempt of the earlier try may have landed
// with its answer lost, so a refusal in the later try says nothing of it either, whether it comes
// first in that try or as a second `401`.
test("a refusal in a try after an earlier try sent under the key is no verdict that nothing was made", async () => {
  const lapsed = () => new ApiError(401, { code: "UNAUTHORIZED", error: "sign in" });
  expect(
    await verdictAfter(
      [new ApiError(400, { code: "SNOOZE_INPUT", error: "snoozed_until is in the past" })],
      "PUT"
    )
  ).toBe(false);
  expect(await verdictAfter([lapsed(), lapsed()], "PUT")).toBe(false);
});

// The key stops only the write that sends it. A `PUT` or a `DELETE` sends none, so Dispatch
// keeps nothing under one for it and making it again is the same change: a tab that slept past
// the key's lifetime still makes it. A key a `POST` went out under is the write the guard is
// for, and the write is refused before any attempt.
test("a spent key stops a write that sends it and lets one that does not through", async () => {
  const spent = keyMinted(Date.now() - IDEMPOTENCY_KEY_TTL_MS);
  let attempts = 0;
  const answer = await writeUnderKey(
    () => {
      attempts += 1;
      return Promise.resolve({ seq: 2 });
    },
    spent,
    {
      carried: "PUT",
      planned: { body: { seq: 2 }, method: "PUT", path: "/api/v1/me/state/CORE-1" },
    }
  );
  expect(attempts).toBe(1);
  expect(answer).toEqual({ seq: 2 });

  const refusal = await writeUnderKey(() => Promise.resolve({}), spent, {
    carried: "POST",
    planned: { body: { body: "hello" }, method: "POST", path: "/api/v1/issues/CORE-1/comments" },
  }).catch((error: unknown) => error);
  expect(refusal).toBeInstanceOf(SendExpiredError);
});

// Retry of a write that ended without Dispatch's verdict asks again under its own key, which
// Dispatch answers with what the first attempt did. Once the key is spent there is nothing to ask
// again under, so the Retry is a new write under a new key, which the store and the three
// surfaces that re-press a write (`send-request.ts`, `broadcast-composition.ts`,
// `useKeyedWrite.ts`) all read from here.
test("a Retry keeps the write's key after a stop and takes a new one once the key is spent", () => {
  expect(retriesUnderItsKey(new SendStoppedError())).toBe(true);
  expect(retriesUnderItsKey(new SendExpiredError())).toBe(false);
});

// Every surface's case builds its expected sentence with `stoppedWriteNote` and pins what the
// surface hands it: the write it names, what it does once it lands, and its control. This case
// pins what each kind of sentence says, written out as the reader reads it, so a sentence that
// promises a Retry the write does not have fails here, whatever the surfaces' cases compute.
test("each kind of stopped write's sentence, and each Retry's promise, reads as written here", () => {
  const stopped = new SendStoppedError();
  expect({
    "a control that is a press of its own": stoppedWriteNote("the new issue", "made", {
      anew: "Pressing Make a new issue again sends it as a new create.",
    }),
    "a Retry of a POST": stoppedWriteNote("your reply", "posted", { method: "POST", stopped }),
    "a Retry of a PUT": stoppedWriteNote("the snooze", "saved", { method: "PUT", stopped }),
    "a Retry of a DELETE": stoppedWriteNote("the un-snooze", "saved", {
      method: "DELETE",
      stopped,
    }),
    "a named control": stoppedWriteNote("the upload", "stored", {
      again: "Upload",
      method: "POST",
      stopped,
    }),
    "a Retry under a key that ran out": stoppedWriteNote("the priority of CORE-1", "saved", {
      method: "PATCH",
      stopped: new SendExpiredError(),
    }),
  }).toEqual({
    "a control that is a press of its own":
      "Dispatch has not confirmed the new issue: it may have been made. Pressing Make a new " +
      "issue again sends it as a new create.",
    "a Retry of a POST":
      "Dispatch has not confirmed your reply: it may have been posted. Retry asks again under " +
      "the same key, so it is posted at most once if you retry within 24 hours of the first " +
      "try. After that Dispatch no longer knows the key: a Retry is a new write and may be " +
      "posted twice.",
    "a Retry of a PUT":
      "Dispatch has not confirmed the snooze: it may have been saved. A Retry sends the same " +
      "change again, which is the same however often it is made.",
    "a Retry of a DELETE":
      "Dispatch has not confirmed the un-snooze: it may have been saved. A Retry sends the same " +
      "change again, which is the same however often it is made.",
    "a named control":
      "Dispatch has not confirmed the upload: it may have been stored. Pressing Upload again " +
      "retries it. Retry asks again under the same key, so it is stored at most once if you " +
      "retry within 24 hours of the first try. After that Dispatch no longer knows the key: a " +
      "Retry is a new write and may be stored twice.",
    "a Retry under a key that ran out":
      "Dispatch has not confirmed the priority of CORE-1: it may have been saved. Its key is 24 " +
      "hours old or more, so Dispatch no longer holds it: a Retry is a new write, which can be " +
      "saved a second time if the first one was.",
  });
  // The shell's indicator ends a stopped write's line with the Retry's promise alone.
  expect({
    "a write under its key": retryNote(true, stopped, "posted"),
    "a write that sends no key": retryNote(false, stopped, "saved"),
    "a write whose key ran out": retryNote(true, new SendExpiredError(), "saved"),
  }).toEqual({
    "a write under its key":
      "Retry asks again under the same key, so it is posted at most once if you retry within " +
      "24 hours of the first try. After that Dispatch no longer knows the key: a Retry is a new " +
      "write and may be posted twice.",
    "a write that sends no key":
      "A Retry sends the same change again, which is the same however often it is made.",
    "a write whose key ran out":
      "Its key is 24 hours old or more, so Dispatch no longer holds it: a Retry is a new write, " +
      "which can be saved a second time if the first one was.",
  });
});
