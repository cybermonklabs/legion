// packages/cipher-client/src/device-login-reach.ts
//
// Whether a poll reaches a device login's code, and the refusal of a code no poll reaches
// (`device-login.ts`: `startDeviceLogin` and `finishDeviceLogin` read every code by it): the
// poll's interval bounds and a code's longest life, a number of seconds as a refusal prints it and
// in the whole milliseconds the poll waits and a code's end holds, the one test both read a code
// by, and the words of the refusal. Nothing here reads a clock: each takes the moment from its
// caller.

import type { DeviceAuthorizationResponse } from "openid-client";

/** How many seconds apart a code is polled where the broker names no interval (RFC 8628 Section
 *  3.2), and the least interval the poll starts at whatever interval the broker names, so no poll
 *  goes out with no wait. Each `slow_down` and each poll the broker did not answer or answered with
 *  an outage add 5 s from then on (RFC 8628 Section 3.5). */
export const DEFAULT_POLL_SECONDS = 5;
export const LEAST_POLL_SECONDS = 1;

/** The most a code or an access token the broker answers is taken to live, whatever lifetime it
 *  names: an hour, four times the broker's own access tokens' 900 s. A longer one would keep a
 *  poll, or a token the broker no longer honours, past any use. */
export const MOST_LIFETIME_SECONDS = 3600;

/**
 * `value`, a positive number of seconds, as a refusal prints it: the shortest digits that read
 * back as it (`Number.prototype.toString`), with no exponent, so a huge one shows every digit where
 * `toString` writes `1e+21`, as the Go helper prints one, and a tiny one is no `0` where it writes
 * `1e-9`. `toString` writes an exponent only from 1e21 up and below 1e-6, so the point falls past
 * every digit or before them all.
 */
export function seconds(value: number): string {
  const [mantissa = "", exponent] = String(value).split("e");
  if (exponent === undefined) return mantissa;
  const [whole = "", fraction = ""] = mantissa.split(".");
  const point = whole.length + Number(exponent);
  const digits = whole + fraction;
  return point <= 0 ? `0.${"0".repeat(-point)}${digits}` : digits.padEnd(point, "0");
}

/**
 * `value`, a number of seconds, 0 or more, in whole milliseconds as its digits name them: the
 * digits {@link seconds} prints, the point moved three places and what follows cut off. Nothing
 * parses and floors a product or a shifted string, since either rounds to the nearest double.
 * `value * 1000` in floating point can fall short of the milliseconds the broker's own digits
 * name, as 1.001 gives 1000.9999999999999; a shifted string can round up to the next millisecond,
 * as 1.0999999999999999 gives 1100 where its digits name 1099.9999999999999 ms. The end of a code
 * is counted so ({@link startDeviceLogin}), and a refusal reads the code's lifetime by that end
 * ({@link unpollableRemedy}'s `left`); the interval is counted by {@link waitMillisecondsOf}.
 * Exported for the tests, which hand a refused code to the poll the way the start would have.
 */
export function wholeMillisecondsOf(value: number): number {
  const [whole = "", fraction = ""] = seconds(value).split(".");
  return Number(`${whole}${fraction.slice(0, 3).padEnd(3, "0")}`);
}

/**
 * How long the poll waits an interval of `value` seconds, in milliseconds: its digits' whole
 * milliseconds, and one more where any digit past the third is not 0, so the wait is never shorter
 * than the interval (RFC 8628 Section 3.5: "at least the number of seconds specified by the
 * 'interval' parameter"): 4.9997 s waits 5000 ms, 1.001 s 1001 ms, 5 s 5000 ms. No product here
 * either: 2.007 gives 2007.0000000000002, which `Math.ceil` would wait a millisecond long.
 * Whether a poll fits in a code is decided by this count, the wait the poll will actually take
 * ({@link codeLeft}), and not by the interval cut down ({@link wholeMillisecondsOf}): a code whose
 * end holds a millisecond past the cut count, as 4.9997 s against a 5 s code, would be shown and
 * then found lapsed at its first poll.
 */
export function waitMillisecondsOf(value: number): number {
  return wholeMillisecondsOf(value) + (pastTheMillisecond(value) ? 1 : 0);
}

/** Whether a digit of `value` seconds past the millisecond, as {@link seconds} prints it, is not
 *  0: where it is, {@link waitMillisecondsOf} counts the value up and {@link wholeMillisecondsOf}
 *  cuts it down. */
export function pastTheMillisecond(value: number): boolean {
  return /\.\d{3}\d*[1-9]/.test(seconds(value));
}

/**
 * What is left, at `now`, of a code that lapses at `expiresAt`: in milliseconds as its end names
 * it (`named`) and {@link MOST_LIFETIME_SECONDS} at most (`left`), and whether a poll
 * `intervalSeconds` apart reaches it: a poll waits the interval first (RFC 8628 Section 3.5),
 * {@link waitMillisecondsOf} of it, and reaches the code only where the code's end, a `Date`'s
 * whole millisecond, holds more than that wait, so a code that lapses no later than the wait is
 * one no poll reaches. {@link startDeviceLogin} and {@link finishDeviceLogin} both read a code by
 * this test, and {@link finishDeviceLogin} waits by that same count, so a code the one shows the
 * other polls, but for the time between the two calls, and an interval of 1.001 s reaches no code
 * that ends 1001 ms on.
 */
export function codeLeft(
  expiresAt: Date,
  intervalSeconds: number,
  now: number
): { readonly named: number; readonly left: number; readonly reached: boolean } {
  const named = expiresAt.getTime() - now;
  const left = Math.min(named, MOST_LIFETIME_SECONDS * 1000);
  return { named, left, reached: waitMillisecondsOf(intervalSeconds) < left };
}

/**
 * What must change at the broker before a device code it answers can be polled, for a broker that
 * named the interval `named`, or none where it is undefined, that this machine polls at
 * `intervalSeconds`, and a code that lives `lifetimeSeconds` here, its end holding `left`
 * milliseconds ({@link codeLeft}), past no hour (the caller names the hour where it ends the code).
 * Each limit is named in the counts the poll's test reads, so a broker that meets it is shown its
 * code: an interval reaches a code only where its wait, the interval counted up to a whole
 * millisecond ({@link waitMillisecondsOf}), is fewer milliseconds than the code's end, its
 * lifetime counted down to one, holds. So a lifetime must live a millisecond or more longer than
 * the wait, and an interval be a millisecond or more shorter than the end: a code that outlives
 * the wait by less holds only the wait, and an interval shorter than the end by less waits the end
 * whole. Where the wait or the end is not the broker's own digits, the limit prints it: 1.0005 s
 * waits 1001 ms, so its limit reads "longer than 1.001 s, its poll interval counted up to a whole
 * millisecond", which a code of 1.0015 s, whose end holds 1001 ms, does not meet. This machine
 * polls at the interval named, 1 s at least, so where that waits 1 s, or the code's end holds 1 s
 * or less, no shorter interval alone helps: the code must live longer, and where the wait is an
 * hour or more, which no lifetime this machine keeps outlives, the interval must also be shorter
 * than an hour, and the lifetime longer than both 1 s and that interval, counted up.
 */
function unpollableRemedy(
  named: number | undefined,
  intervalSeconds: number,
  lifetimeSeconds: number,
  left: number
): string {
  const outlivesLeast = left > LEAST_POLL_SECONDS * 1000;
  const wait = waitMillisecondsOf(intervalSeconds);
  // What a lifetime must pass, and an interval fall short of, each by a millisecond or more.
  const pastWait = pastTheMillisecond(intervalSeconds)
    ? `${seconds(wait / 1000)} s, its poll interval counted up to a whole millisecond`
    : "its poll interval";
  const underEnd = pastTheMillisecond(lifetimeSeconds)
    ? `${seconds(left / 1000)} s, its codes' lifetime counted down to a whole millisecond`
    : "its codes' lifetime";
  if (named === undefined) {
    return outlivesLeast
      ? `it names a poll interval a millisecond or more shorter than ${underEnd}, or its codes live a millisecond or more longer than ${DEFAULT_POLL_SECONDS} s`
      : `its codes live a millisecond or more longer than ${DEFAULT_POLL_SECONDS} s, or a millisecond or more longer than both ${LEAST_POLL_SECONDS} s and a poll interval it names, counted up to a whole millisecond`;
  }
  if (wait === LEAST_POLL_SECONDS * 1000) {
    return `its codes live a millisecond or more longer than ${LEAST_POLL_SECONDS} s, the least poll interval this machine keeps`;
  }
  if (outlivesLeast) return `its poll interval is a millisecond or more shorter than ${underEnd}`;
  return wait >= MOST_LIFETIME_SECONDS * 1000
    ? `its poll interval is a millisecond or more shorter than an hour and its codes live a millisecond or more longer than both ${LEAST_POLL_SECONDS} s and that interval, counted up to a whole millisecond`
    : `its codes live a millisecond or more longer than ${pastWait}`;
}

/**
 * What {@link startDeviceLogin}'s refusal of `answered` says the broker answered, for a code no
 * poll `intervalSeconds` apart reaches that lives `lifetimeSeconds` here, its end holding `left`
 * milliseconds ({@link codeLeft}): the interval the broker named, or none, with the one used where
 * this machine's bounds set it; the code's lifetime, that it had already lapsed, or the hour this
 * machine polls a code at most where that ends it first; and what must change at the broker,
 * worded from what ends the code, since asking again would meet the same answer. An interval used
 * shorter than its code's life, which the test still refuses (the interval counted up to a
 * millisecond, the end down), is said to be so.
 */
export function unpollableAnswer(
  answered: DeviceAuthorizationResponse,
  intervalSeconds: number,
  lifetimeSeconds: number,
  left: number
): string {
  const named = answered.interval;
  const interval =
    named === undefined
      ? `no poll interval, so one of ${DEFAULT_POLL_SECONDS} s`
      : named < intervalSeconds
        ? `a poll interval of ${seconds(named)} s, so one of ${LEAST_POLL_SECONDS} s`
        : `a poll interval of ${seconds(named)} s`;
  const lapses = "the code would lapse before its first poll";
  const verdict = "no device login works at that broker until";
  const hour = lifetimeSeconds < answered.expires_in;
  // Where the hour ends the code, only a shorter interval helps, by the wait's count too.
  const remedy = hour
    ? "its poll interval is a millisecond or more shorter than an hour"
    : unpollableRemedy(named, intervalSeconds, lifetimeSeconds, left);
  if (lifetimeSeconds <= 0) {
    return `a device code that had already lapsed, with ${interval}; ${verdict} ${remedy}`;
  }
  const answer = hour
    ? `${interval}, and this machine polls a device code for an hour at most`
    : `a device code that lives ${seconds(lifetimeSeconds)} s, with ${interval}`;
  // The wait is less than a millisecond past the interval and the end less than one short of the
  // lifetime, or of the hour, so an interval shorter than either that is refused is under two
  // milliseconds shorter.
  if (intervalSeconds < lifetimeSeconds) {
    return `${answer}, under two milliseconds apart: ${lapses}, as this machine counts a poll's wait up and a code's end down to a whole millisecond; ${verdict} ${remedy}`;
  }
  return `${answer}: ${lapses}; ${verdict} ${remedy}`;
}
