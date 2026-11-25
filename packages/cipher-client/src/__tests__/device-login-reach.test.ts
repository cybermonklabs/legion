// packages/cipher-client/src/__tests__/device-login-reach.test.ts
//
// The poll's interval and wait, and the refusal of a code no poll reaches (`device-login-reach.ts`),
// through the calls that read them, `startDeviceLogin` and `finishDeviceLogin`, against the
// stand-in broker. The device login's other rows live in device-login.test.ts.

import { afterEach, expect, spyOn, test } from "bun:test";
import { join } from "node:path";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import {
  advancingWaits,
  STAND_IN_USER_CODE,
  stubDeviceLoginBroker,
} from "../../test/device-logins";
import { finishDeviceLogin, startDeviceLogin } from "../device-login";
import { wholeMillisecondsOf } from "../device-login-reach";

const PERSON = "ada@example.com";
const HOST = "adas-laptop.local";

const makeDirectory = temporaryDirectories("device-login-reach");

const stops: (() => void)[] = [];
afterEach(() => {
  for (const stop of stops.splice(0)) stop();
});

/** A stand-in broker stopped after the test. */
function broker(options: Parameters<typeof stubDeviceLoginBroker>[0] = {}) {
  const stub = stubDeviceLoginBroker(options);
  stops.push(stub.stop);
  return stub;
}

/** A device login file's path in a directory of this test's own. */
function loginPath(): string {
  return join(makeDirectory(), "legion", "device-login.json");
}

test("a code is polled at the broker's interval, once a second at least, every 5 s where it names none", async () => {
  for (const [interval, wait] of [
    [0.001, 1_000],
    [null, 5_000],
    // RFC 8628 Section 3.5: the poll waits at least the interval the broker names.
    [90, 90_000],
  ] as const) {
    const stub = broker({ interval });
    const started = await startDeviceLogin({ brokerUrl: stub.url, person: PERSON, host: HOST });
    const waits: number[] = [];
    await finishDeviceLogin(started, {
      path: loginPath(),
      sleep: async (ms) => {
        waits.push(ms);
      },
    });
    expect(waits, String(interval)).toEqual([wait]);
  }
});

// RFC 8628 Section 3.5 has the poll wait at least the broker's interval, so a code that lapses no
// later than its first poll could never be polled. `startDeviceLogin` refuses such an answer before
// its code is shown, since an approval of it would mint a launcher credential bound to a key no
// process holds. The refusal names the broker's interval and what ends the code first, its own
// lifetime or the hour this machine polls a code at most, each number in all its digits however
// large or small, and what must change at the broker, worded from what ends the code: asking again
// would meet the same answer. A code that had already lapsed is not said to lapse before its poll.
test("a broker's code no poll could reach is refused before it is shown, naming the cause", async () => {
  const lapsed = "; no device login works at that broker until";
  const lapses = `: the code would lapse before its first poll${lapsed}`;
  const apart = `, under two milliseconds apart: the code would lapse before its first poll, as this machine counts a poll's wait up and a code's end down to a whole millisecond${lapsed}`;
  // Each limit is named by the count the poll's test reads: a lifetime a millisecond or more past
  // the wait, the interval counted up, and an interval a millisecond or more short of the end, the
  // lifetime counted down, each printed where it is not the broker's own digits.
  const shorter = "its poll interval is a millisecond or more shorter than";
  const upTo = "its poll interval counted up to a whole millisecond";
  const downTo = "its codes' lifetime counted down to a whole millisecond";
  const noInterval =
    "its codes live a millisecond or more longer than 5 s, or a millisecond or more longer than both 1 s and a poll interval it names, counted up to a whole millisecond";
  const pastHour = `${shorter} an hour and its codes live a millisecond or more longer than both 1 s and that interval, counted up to a whole millisecond`;
  for (const [options, says] of [
    [
      { interval: 1_800 },
      `a device code that lives 600 s, with a poll interval of 1800 s${lapses} ${shorter} its codes' lifetime`,
    ],
    [
      { interval: 600 },
      `a device code that lives 600 s, with a poll interval of 600 s${lapses} ${shorter} its codes' lifetime`,
    ],
    // A lifetime with digits past the millisecond is named by the end it holds.
    [
      { interval: 1_800, expiresIn: 600.0005 },
      `a device code that lives 600.0005 s, with a poll interval of 1800 s${lapses} ${shorter} 600 s, ${downTo}`,
    ],
    [
      { interval: 5_000, expiresIn: 7_200 },
      `a poll interval of 5000 s, and this machine polls a device code for an hour at most${lapses} ${shorter} an hour`,
    ],
    [
      { interval: 1e21 },
      `a device code that lives 600 s, with a poll interval of 1000000000000000000000 s${lapses} ${shorter} its codes' lifetime`,
    ],
    [
      { expiresIn: 0 },
      `a device code that had already lapsed, with a poll interval of 1 s${lapsed} its codes live a millisecond or more longer than 1 s, the least poll interval this machine keeps`,
    ],
    [
      { interval: null, expiresIn: 0 },
      `a device code that had already lapsed, with no poll interval, so one of 5 s${lapsed} ${noInterval}`,
    ],
    [
      { interval: 5_000, expiresIn: 0 },
      `a device code that had already lapsed, with a poll interval of 5000 s${lapsed} ${pastHour}`,
    ],
    [
      { interval: null, expiresIn: 3 },
      `a device code that lives 3 s, with no poll interval, so one of 5 s${lapses} it names a poll interval a millisecond or more shorter than its codes' lifetime, or its codes live a millisecond or more longer than 5 s`,
    ],
    // The broker's interval is already shorter than its code: only a longer lifetime helps.
    [
      { interval: 0.25, expiresIn: 1 },
      `a device code that lives 1 s, with a poll interval of 0.25 s, so one of 1 s${lapses} its codes live a millisecond or more longer than 1 s, the least poll interval this machine keeps`,
    ],
    [
      { interval: 0.0004, expiresIn: 1 },
      `a device code that lives 1 s, with a poll interval of 0.0004 s, so one of 1 s${lapses} its codes live a millisecond or more longer than 1 s, the least poll interval this machine keeps`,
    ],
    // An interval whose digits go past the millisecond is no floor: it waits the millisecond up
    // (1.0005 s waits 1001 ms), which a code holding only 1 s does not outlast, so the cure names
    // that wait: a code of 1.0015 s, a millisecond past the interval's digits, holds 1001 ms too.
    [
      { interval: 1.0005, expiresIn: 1 },
      `a device code that lives 1 s, with a poll interval of 1.0005 s${lapses} its codes live a millisecond or more longer than 1.001 s, ${upTo}`,
    ],
    [
      { interval: 2, expiresIn: 1e-9 },
      `a device code that lives 0.000000001 s, with a poll interval of 2 s${lapses} its codes live a millisecond or more longer than its poll interval`,
    ],
    // A code of 1.0005 s ends 1000 ms on, which no interval reaches, the least being 1 s: only a
    // longer lifetime helps, whatever interval the broker names, or none.
    [
      { interval: 2, expiresIn: 1.0005 },
      `a device code that lives 1.0005 s, with a poll interval of 2 s${lapses} its codes live a millisecond or more longer than its poll interval`,
    ],
    [
      { interval: null, expiresIn: 1.0005 },
      `a device code that lives 1.0005 s, with no poll interval, so one of 5 s${lapses} ${noInterval}`,
    ],
    // A code of 1 s, no interval named: its second limit counts the interval it names up, since a
    // broker that then names 1.0005 s and lengthens its codes to 1.0015 s is refused again.
    [
      { interval: null, expiresIn: 1 },
      `a device code that lives 1 s, with no poll interval, so one of 5 s${lapses} ${noInterval}`,
    ],
    // The same at an interval with digits past the millisecond, which waits it up, and at an
    // interval of an hour or more, where a shorter interval is needed too.
    [
      { interval: 1.0005, expiresIn: 1.0005 },
      `a device code that lives 1.0005 s, with a poll interval of 1.0005 s${lapses} its codes live a millisecond or more longer than 1.001 s, ${upTo}`,
    ],
    [
      { interval: 1.0009, expiresIn: 1.0002 },
      `a device code that lives 1.0002 s, with a poll interval of 1.0009 s${lapses} its codes live a millisecond or more longer than 1.001 s, ${upTo}`,
    ],
    // Exactly 1.001 s is past the floor: the broker's digits name 1001 ms, though 1.001 * 1000 is
    // 1000.9999999999999 in floating point, so the cure names that interval, not the floor.
    [
      { interval: 1.001, expiresIn: 1.0005 },
      `a device code that lives 1.0005 s, with a poll interval of 1.001 s${lapses} its codes live a millisecond or more longer than its poll interval`,
    ],
    [
      { interval: 5_000, expiresIn: 1.0005 },
      `a device code that lives 1.0005 s, with a poll interval of 5000 s${lapses} ${pastHour}`,
    ],
    // An interval under the hour whose wait, counted up, is the hour is one too.
    [
      { interval: 3_599.9995, expiresIn: 0.5 },
      `a device code that lives 0.5 s, with a poll interval of 3599.9995 s${lapses} ${pastHour}`,
    ],
    // Shorter than the code by less than two milliseconds, which the poll's wait, rounded up, and
    // the code's end, a `Date` cut down, do not separate: the poll would find the code lapsed at its
    // first poll, so it is refused here, before its code is shown. A code that ends a millisecond
    // past the wait polls (4.9997 s against 5.001 s).
    [
      { interval: 5, expiresIn: 5.0005 },
      `a device code that lives 5.0005 s, with a poll interval of 5 s${apart} ${shorter} 5 s, ${downTo}`,
    ],
    [
      { interval: 4.9997, expiresIn: 5 },
      `a device code that lives 5 s, with a poll interval of 4.9997 s${apart} ${shorter} its codes' lifetime`,
    ],
    [
      { interval: 4.9997, expiresIn: 5.0005 },
      `a device code that lives 5.0005 s, with a poll interval of 4.9997 s${apart} ${shorter} 5 s, ${downTo}`,
    ],
    [
      { interval: 4.9995, expiresIn: 5 },
      `a device code that lives 5 s, with a poll interval of 4.9995 s${apart} ${shorter} its codes' lifetime`,
    ],
    [
      { interval: 1.0005, expiresIn: 1.001 },
      `a device code that lives 1.001 s, with a poll interval of 1.0005 s${apart} ${shorter} its codes' lifetime`,
    ],
    [
      { interval: 1.0005, expiresIn: 1.0015 },
      `a device code that lives 1.0015 s, with a poll interval of 1.0005 s${apart} ${shorter} 1.001 s, ${downTo}`,
    ],
    [
      { interval: 2.0005, expiresIn: 2.001 },
      `a device code that lives 2.001 s, with a poll interval of 2.0005 s${apart} ${shorter} its codes' lifetime`,
    ],
    // The hour, where it ends the code, is the end an interval just under it is refused by.
    [
      { interval: 3_599.9995, expiresIn: 7_200 },
      `a poll interval of 3599.9995 s, and this machine polls a device code for an hour at most${apart} ${shorter} an hour`,
    ],
    // The same at the floor, where the interval used is 1 s, whether the broker named less or 1 s
    // itself; at 1.0002 s, which waits 1001 ms, so is no floor and the cure names that wait; and
    // at the 5 s of a broker that names none.
    [
      { interval: 0.5, expiresIn: 1.0005 },
      `a device code that lives 1.0005 s, with a poll interval of 0.5 s, so one of 1 s${apart} its codes live a millisecond or more longer than 1 s, the least poll interval this machine keeps`,
    ],
    [
      { interval: 1, expiresIn: 1.0005 },
      `a device code that lives 1.0005 s, with a poll interval of 1 s${apart} its codes live a millisecond or more longer than 1 s, the least poll interval this machine keeps`,
    ],
    [
      { interval: 1.0002, expiresIn: 1.0005 },
      `a device code that lives 1.0005 s, with a poll interval of 1.0002 s${apart} its codes live a millisecond or more longer than 1.001 s, ${upTo}`,
    ],
    [
      { interval: null, expiresIn: 5.0005 },
      `a device code that lives 5.0005 s, with no poll interval, so one of 5 s${apart} it names a poll interval a millisecond or more shorter than 5 s, ${downTo}, or its codes live a millisecond or more longer than 5 s`,
    ],
  ] as const) {
    const stub = broker(options);
    const refusal = await startDeviceLogin({
      brokerUrl: stub.url,
      person: PERSON,
      host: HOST,
    }).then(
      () => null,
      (error: unknown) => error as Error
    );
    expect(refusal?.message, says).toStartWith("the secrets broker at ");
    expect(refusal?.message, says).toEndWith(`answered ${says}`);
    expect(refusal?.message, says).not.toContain(STAND_IN_USER_CODE);
    expect(stub.polls, says).toHaveLength(0);
  }
  // Control: an interval just inside the code's life is a code to show and poll.
  const inside = broker({ interval: 599 });
  const started = await startDeviceLogin({ brokerUrl: inside.url, person: PERSON, host: HOST });
  expect(started.userCode).toBe(STAND_IN_USER_CODE);
});

// A refusal's cure names each limit by the count the poll's test reads, so a broker that meets it
// to the millisecond is shown its code, and one that meets only the broker's own digits is refused
// again: an interval that waits a millisecond past its digits, a lifetime whose end holds less
// than its digits, an interval shorter than the end by less than its wait counts up, and a new
// interval under 1 s, which this machine polls at 1 s. The floor's cure is the control: its words
// were already the count.
test("a broker that meets the limit a refusal names is shown its code, and one short of it is not", async () => {
  const until = "; no device login works at that broker until ";
  const pastHour =
    "its poll interval is a millisecond or more shorter than an hour and its codes live a millisecond or more longer than both 1 s and that interval, counted up to a whole millisecond";
  const noIntervalShort =
    "it names a poll interval a millisecond or more shorter than its codes' lifetime, or its codes live a millisecond or more longer than 5 s";
  const answer = async (options: Parameters<typeof broker>[0]): Promise<string> =>
    startDeviceLogin({ brokerUrl: broker(options).url, person: PERSON, host: HOST }).then(
      () => "shown",
      (error: unknown) => (error as Error).message
    );
  for (const [refused, cure, meets, short] of [
    [
      { interval: 1.0005, expiresIn: 1 },
      "its codes live a millisecond or more longer than 1.001 s, its poll interval counted up to a whole millisecond",
      { interval: 1.0005, expiresIn: 1.002 },
      { interval: 1.0005, expiresIn: 1.0015 },
    ],
    [
      { interval: null, expiresIn: 1 },
      "its codes live a millisecond or more longer than 5 s, or a millisecond or more longer than both 1 s and a poll interval it names, counted up to a whole millisecond",
      { interval: 1.0005, expiresIn: 1.002 },
      { interval: 1.0005, expiresIn: 1.0015 },
    ],
    [
      { interval: 2, expiresIn: 0.5 },
      "its codes live a millisecond or more longer than its poll interval",
      { interval: 2, expiresIn: 2.001 },
      { interval: 2, expiresIn: 2.0005 },
    ],
    [
      { interval: 1_800 },
      "its poll interval is a millisecond or more shorter than its codes' lifetime",
      { interval: 599.999 },
      { interval: 599.9995 },
    ],
    [
      { interval: 1_800, expiresIn: 600.0005 },
      "its poll interval is a millisecond or more shorter than 600 s, its codes' lifetime counted down to a whole millisecond",
      { interval: 599.999, expiresIn: 600.0005 },
      { interval: 600.0002, expiresIn: 600.0005 },
    ],
    [
      { interval: null, expiresIn: 3 },
      noIntervalShort,
      { interval: 2.999, expiresIn: 3 },
      { interval: 2.9995, expiresIn: 3 },
    ],
    [
      { interval: null, expiresIn: 3 },
      noIntervalShort,
      { interval: null, expiresIn: 5.001 },
      { interval: null, expiresIn: 5.0005 },
    ],
    [
      { interval: 5_000, expiresIn: 7_200 },
      "its poll interval is a millisecond or more shorter than an hour",
      { interval: 3_599.999, expiresIn: 7_200 },
      { interval: 3_599.9995, expiresIn: 7_200 },
    ],
    [
      { interval: 5_000, expiresIn: 0.5 },
      pastHour,
      { interval: 0.5, expiresIn: 1.001 },
      { interval: 0.5, expiresIn: 0.8 },
    ],
    [
      { interval: 3_599.9995, expiresIn: 0.5 },
      pastHour,
      { interval: 3_599.999, expiresIn: 3_600 },
      { interval: 3_599.9995, expiresIn: 3_600.0005 },
    ],
    [
      { interval: 1, expiresIn: 1 },
      "its codes live a millisecond or more longer than 1 s, the least poll interval this machine keeps",
      { interval: 1, expiresIn: 1.001 },
      { interval: 1, expiresIn: 1.0005 },
    ],
  ] as const) {
    const row = JSON.stringify(refused);
    expect(await answer(refused), row).toEndWith(`${until}${cure}`);
    expect(await answer(meets), `${row}, met by ${JSON.stringify(meets)}`).toBe("shown");
    expect(await answer(short), `${row}, short at ${JSON.stringify(short)}`).toContain(until);
  }
});

// `startDeviceLogin` and `finishDeviceLogin` read a code by one test: a poll fits only where the
// wait it will take, the interval rounded up to a whole millisecond, is fewer milliseconds than
// the code's end holds. So at one moment a code the one shows the other polls, and a code the one
// refuses the other refuses: none is printed to be refused by the poll, or shown and then found
// lapsed at its first poll. The wall clock stands still, so no time passes between the two, and
// the poll's clock moves by each wait, so a code shown is polled once and its login kept, and one
// its wait would outlast is refused at once, with no wait and no poll.
test("the start and the poll agree on which code a poll reaches, to the millisecond", async () => {
  const base = await startDeviceLogin({ brokerUrl: broker().url, person: PERSON, host: HOST });
  const now = Date.now();
  const still = spyOn(Date, "now").mockReturnValue(now);
  try {
    for (const [interval, expiresIn, reached] of [
      [5, 5, false],
      [5, 5.0005, false],
      [5, 5.002, true],
      [1.0002, 1.0005, false],
      [2, 1.0005, false],
      // An interval as long as its code, each the 1001 ms the broker's digits name.
      [1.001, 1.001, false],
      [0.25, 1, false],
      [599, 600, true],
      // An interval whose digits go past the millisecond waits the millisecond up: 4.9997 s waits
      // 5000 ms, so a code whose end holds 5000 ms is refused (a code shown with a 5000 ms wait
      // would be found lapsed at its first poll) and one that holds 5001 is polled, as with
      // 1.0005 s (1001 ms) and 2.0005 s (2001 ms).
      [4.9997, 5, false],
      [4.9997, 5.0005, false],
      [4.9995, 5, false],
      [4.9997, 5.001, true],
      [1.0005, 1.001, false],
      [1.0005, 1.002, true],
      [2.0005, 2.001, false],
      // The digits cut where a shifted string would round: 1.0999999999999999 s waits 1100 ms,
      // so a 1.1 s code is refused and a 1.101 s one polled, and 1.0009999999999997 s holds 1000 ms,
      // as a 1 s interval's.
      [1.0999999999999999, 1.1, false],
      [1.0999999999999999, 1.101, true],
      [1.0999999999999999, 1.0999999999999999, false],
      [1, 1.0009999999999997, false],
      // A lifetime past 2^64 nanoseconds, held to the hour here, where the Go helper's OAuth
      // library wraps it to about 900 s, so that helper refuses a 1000 s interval.
      [1000, 18446744974, true],
    ] as const) {
      const row = `${interval} s against ${expiresIn} s`;
      const stub = broker({ interval, expiresIn });
      const shown = await startDeviceLogin({
        brokerUrl: stub.url,
        person: PERSON,
        host: HOST,
      }).then(
        (started) => started,
        () => null
      );
      // A code the start refused is handed to the poll as the start would have handed it: at the
      // row's own broker, so a poll of it would reach the stand-in this row counts, and its end the
      // whole milliseconds the start reads, from the one function it reads them by.
      const handed = shown ?? {
        ...base,
        brokerUrl: stub.url,
        intervalSeconds: Math.max(interval, 1),
        expiresAt: new Date(now + wholeMillisecondsOf(expiresIn)),
      };
      const { waits, sleep, clock } = advancingWaits();
      const polled = await finishDeviceLogin(handed, { path: loginPath(), sleep, clock }).then(
        () => "kept",
        (error: unknown) => (error as Error).message
      );
      expect(shown === null, row).toBe(!reached);
      expect(waits, row).toHaveLength(reached ? 1 : 0);
      // A code shown reaches its poll, which the stand-in approves; one refused is refused at once.
      expect(polled, row).toEqual(
        reached ? "kept" : expect.stringContaining("no poll could reach it")
      );
      expect(stub.polls, row).toHaveLength(reached ? 1 : 0);
    }
  } finally {
    still.mockRestore();
  }
});

// The poll waits at least the interval the broker named (RFC 8628 Section 3.5), in whole
// milliseconds rounded up: 4.9997 s waits 5000 ms, not the 4999 its digits cut to, and 1.001 s
// 1001 ms, where the product is 1000.9999999999999, and 2.007 s 2007 ms, where it is
// 2007.0000000000002. An interval of whole seconds waits exactly that. The code's test reads this
// same count (the agreement test above).
test("the poll waits at least the interval, to a whole millisecond", async () => {
  const base = await startDeviceLogin({ brokerUrl: broker().url, person: PERSON, host: HOST });
  const now = Date.now();
  const still = spyOn(Date, "now").mockReturnValue(now);
  try {
    for (const [intervalSeconds, wait] of [
      [5, 5_000],
      [4.9997, 5_000],
      [1.001, 1_001],
      [1.0005, 1_001],
      [2.007, 2_007],
      [1.0999999999999999, 1_100],
    ] as const) {
      const waits: number[] = [];
      await finishDeviceLogin(
        { ...base, intervalSeconds, expiresAt: new Date(now + 600_000) },
        {
          path: loginPath(),
          sleep: async (ms) => {
            waits.push(ms);
            throw new Error("the first wait");
          },
        }
      ).catch(() => undefined);
      expect(waits, `${intervalSeconds} s`).toEqual([wait]);
    }
  } finally {
    still.mockRestore();
  }
});
