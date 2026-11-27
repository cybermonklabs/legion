import { expect, test } from "bun:test";
import {
  IDEMPOTENCY_KEY_COMMITTED,
  IDEMPOTENCY_KEY_IN_FLIGHT,
  retriesUnderKey,
} from "./dispatch-api-constants";
import { IDEMPOTENCY_KEY_REUSED } from "./dispatch-api-limits";
import { mayHaveReachedServer, outage, retryAfterMs } from "./http-status";

/** Every status from 100 to 599. */
const STATUSES = Array.from({ length: 500 }, (_, index) => 100 + index);

/** Every 5xx, written out rather than read through the rule under test. */
const SERVER_ERRORS = Array.from({ length: 100 }, (_, index) => 500 + index);

test("an outage is a 408, a 429 and every 5xx, and no other status from 100 to 599", () => {
  expect(STATUSES.filter(outage)).toEqual([408, 429, ...SERVER_ERRORS]);
});

// A failed request may have reached the server and taken effect after a 5xx or a 408: a hop can
// send a 408 once it forwarded the whole request. So of the statuses from 100 to 599 the rule holds
// of exactly those, each of them an outage, and the one outage it does not hold of is the 429,
// which turns the request away before it runs.
test("a failed request may have reached the server after every 5xx and a 408, and after no other status from 100 to 599", () => {
  expect(STATUSES.filter(mayHaveReachedServer)).toEqual([408, ...SERVER_ERRORS]);
  expect(STATUSES.filter((status) => mayHaveReachedServer(status) && !outage(status))).toEqual([]);
  expect(STATUSES.filter((status) => outage(status) && !mayHaveReachedServer(status))).toEqual([
    429,
  ]);
});

// A keyed write asks again after exactly the outages, and after `409 KEY_IN_FLIGHT` besides,
// which is Dispatch's keyed-write layer rather than an outage: so that answer under that code is
// the one place, at any status and any code, where the two rules part.
test("retriesUnderKey reads every status from 100 to 599 as outage does, but a 409 KEY_IN_FLIGHT", () => {
  const codes = [
    undefined,
    IDEMPOTENCY_KEY_IN_FLIGHT,
    IDEMPOTENCY_KEY_COMMITTED,
    IDEMPOTENCY_KEY_REUSED,
    "UNAVAILABLE",
  ];
  const parts = STATUSES.flatMap((status) =>
    codes
      .filter((code) => retriesUnderKey(status, code) !== outage(status))
      .map((code) => `${status} ${code}`)
  );
  expect(parts).toEqual([`409 ${IDEMPOTENCY_KEY_IN_FLIGHT}`]);
});

// `Retry-After` is delay-seconds or an HTTP-date (RFC 9110 section 10.2.3); anything else, or no
// header, asks for no wait.
test("Retry-After is read as delay-seconds or as the time left until its date", () => {
  const now = Date.parse("Mon, 23 Nov 2026 18:00:00 GMT");
  const asked = (value?: string) =>
    retryAfterMs(new Headers(value === undefined ? {} : { "Retry-After": value }), now);
  expect(asked("30")).toBe(30_000);
  expect(asked(" 7 ")).toBe(7_000);
  expect(asked("Mon, 23 Nov 2026 18:00:45 GMT")).toBe(45_000);
  expect(asked("Mon, 23 Nov 2026 17:59:00 GMT")).toBe(0);
  expect(asked("soon")).toBe(0);
  expect(asked("-5")).toBe(0);
  expect(asked()).toBe(0);
});
