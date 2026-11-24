// packages/envoy-client/src/__tests__/credential-deadline.test.ts
//
// A request's credential waits on the request's time less a reserve for the request itself,
// `min(1 s, deadlineMs / 2)` (`credentialDeadline`): a broker credential that hands over its live
// token when its wait ends leaves the request that reserve to be sent and answered, and the
// credential's wait ends no later than the request's.
//
// The clock is Bun's fake one, which `AbortSignal.timeout` keeps time by too: each row moves it to
// the millisecond before the credential's wait ends and then onto it, so the row reads the wait's
// length to the millisecond however loaded the machine is.

import { afterEach, beforeEach, expect, jest, test } from "bun:test";
import { credentialDeadline } from "../bearer";

beforeEach(() => {
  jest.useFakeTimers();
});

// A fake clock left behind stops the timers of every file the same worker runs after this one.
afterEach(() => {
  jest.useRealTimers();
});

test("a request of 3 s leaves itself a 1 s reserve: the credential's wait ends at 2 s", () => {
  const deadline = AbortSignal.timeout(3_000);
  const waited = credentialDeadline(deadline, 3_000);
  jest.advanceTimersByTime(1_999);
  expect(waited.aborted).toBe(false);
  jest.advanceTimersByTime(1);
  expect(waited.aborted).toBe(true);
  expect(deadline.aborted).toBe(false);
});

test("a request shorter than 2 s leaves itself half: the credential's wait ends at 200 ms of 400", () => {
  const deadline = AbortSignal.timeout(400);
  const waited = credentialDeadline(deadline, 400);
  jest.advanceTimersByTime(199);
  expect(waited.aborted).toBe(false);
  jest.advanceTimersByTime(1);
  expect(waited.aborted).toBe(true);
  expect(deadline.aborted).toBe(false);
});

test("the credential's wait ends at once where the request's has", () => {
  const ended = new AbortController();
  ended.abort(new Error("the request's own deadline"));
  const waited = credentialDeadline(ended.signal, 60_000);
  expect(waited.aborted).toBe(true);
  expect((waited.reason as Error).message).toBe("the request's own deadline");
});
