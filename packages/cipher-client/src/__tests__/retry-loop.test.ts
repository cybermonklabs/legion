// packages/cipher-client/src/__tests__/retry-loop.test.ts
//
// The token client's one retry loop per session (`BrokerTokenClient.token`), as one table: each
// row is what the broker answers, trade by trade, and what the client must do about it. A row
// asserts the caller's outcome, the trades the broker saw, the waits between them and the lines
// the client logged at each wait. The clock and the waits are the test's own: a wait the client
// asks for passes at once while the row's budget of waits lasts and then holds, so a schedule of
// minutes runs at the test's speed and a loop that trades forever stops where the row reads it.
// A trade's own timeout (`attemptMs`) is the client's, set on the fetch, on Bun's fake clock: the
// row's waits (`until`) move that clock a millisecond at each turn of the event loop, so a row
// whose broker leaves a trade unanswered sees it run out after `SILENT_ATTEMPT_MS` of turns, and
// every other row gives a trade the client's own 30 s, so no machine is slow enough to turn a
// trade the broker answered into one it did not. Real time bounds a wait only to name a hang.

import { expect, jest, test } from "bun:test";
import { turn, turnsUntil } from "@legion/test-fixtures/fake-clock";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { newDpopKey } from "../dpop";
import type { SubjectToken, TokenSource } from "../source";
import { type BrokerToken, BrokerTokenClient, TRADE_ATTEMPT_MS } from "../token-client";
import {
  BrokerRefusedError,
  RegistrationPendingError,
  TradeRetryingError,
  TradeUnansweredError,
} from "../token-errors";
import { SUBJECT_TOKEN_TYPE_KUBERNETES } from "../wire";
import { accepted } from "./stub";

/** The session name a pod of these rows trades under. */
const SESSION = "implementer-g1";

/** The broker's refusal of a pod whose session no registration names yet, in its own words
 *  (`registeredPodSession`, packages/envoy/internal/cipher/oauth/exchange.go). */
const UNREGISTERED = `pod_session_not_registered: No registration says this pod may write under the session name '${SESSION}'. A pod writes under a session name its deployment registered for it.`;

/** A pod's projected token, read afresh at each trade; random, as a real one is. */
const POD: TokenSource = {
  read: async (): Promise<SubjectToken> => ({
    token: fixtureSecret("pod"),
    tokenType: SUBJECT_TOKEN_TYPE_KUBERNETES,
  }),
  describe: () => "a pod's projected service-account token",
};

/** How long one trade may take before the client reads it as unanswered, in a row whose broker
 *  leaves a trade unanswered (`Row.attemptMs`). Every other row gives a trade the client's own
 *  `TRADE_ATTEMPT_MS`, which no row's test time reaches. */
const SILENT_ATTEMPT_MS = 25;

/** A caller that waits through the loop's retries, as the channel server's start does. */
const READY = { waitForReady: true } as const;

/** What the broker does with one trade. */
type Answer = "granted" | "pending" | "503" | "refused" | "connect refused" | "silent";

/** The broker's answer, as the client's fetch returns or throws it. */
async function respond(answer: Answer, signal: AbortSignal | null | undefined): Promise<Response> {
  switch (answer) {
    case "granted":
      return Response.json(accepted(fixtureSecret("granted"), 600));
    case "pending":
      return Response.json(
        { error: "invalid_grant", error_description: UNREGISTERED },
        { status: 400 }
      );
    case "503":
      return new Response("the broker is away", { status: 503 });
    case "refused":
      return Response.json({ error: "invalid_client" }, { status: 401 });
    case "connect refused":
      // A real refused connection, the runtime's own error: nothing listens on port 1.
      return await fetch("http://127.0.0.1:1/", signal === null ? {} : { signal });
    case "silent":
      return await new Promise<Response>((_resolve, reject) => {
        signal?.addEventListener("abort", () => reject(signal.reason), { once: true });
      });
  }
}

/** One row's client and what the row can read of it. */
interface Rig {
  readonly client: BrokerTokenClient;
  readonly clock: { now: number };
  readonly waits: number[];
  readonly lines: string[];
  trades(): number;
  /** Lets the first held wait pass, moving the clock by its length. */
  advance(): void;
}

/** A pod's client against a broker answering `answer(n, now)` to its n-th trade, each trade given
 *  `attemptMs`; its first `budget` waits pass at once, and the rest hold until the row advances
 *  them. */
async function rig(
  answer: (trade: number, now: number) => Answer,
  budget: number,
  attemptMs: number
): Promise<Rig> {
  const clock = { now: 1_700_000_000_000 };
  const waits: number[] = [];
  const lines: string[] = [];
  const held: (() => void)[] = [];
  let trades = 0;
  let left = budget;
  const client = new BrokerTokenClient({
    brokerUrl: "http://broker.test",
    key: await newDpopKey(),
    subject: POD,
    session: SESSION,
    now: () => clock.now,
    // The worst case of the jitter: every wait its whole length.
    random: () => 1,
    attemptMs,
    sleep: (ms: number, signal?: AbortSignal) => {
      waits.push(ms);
      if (left > 0) {
        left -= 1;
        clock.now += ms;
        return Promise.resolve();
      }
      return new Promise<void>((resolve) => {
        held.push(() => {
          clock.now += ms;
          resolve();
        });
        signal?.addEventListener("abort", () => resolve(), { once: true });
      });
    },
    log: (line: string) => lines.push(line),
    fetch: (async (_url: string | URL | Request, init?: RequestInit) => {
      trades += 1;
      return await respond(answer(trades, clock.now), init?.signal);
    }) as typeof fetch,
  });
  return {
    client,
    clock,
    waits,
    lines,
    trades: () => trades,
    advance: () => held.shift()?.(),
  };
}

/** Turns of the event loop until `ready()` holds, the fake clock moving a millisecond at each, so
 *  a trade's attempt runs out as the client set it; a wait that would not end names `what`. */
function until(ready: () => boolean, what: string): Promise<void> {
  return turnsUntil(ready, what, { boundMs: 10_000, each: () => jest.advanceTimersByTime(1) });
}

/** How `call` settled, once it has: its value, or what it rejected with. */
function settlement(call: Promise<unknown>): { outcome?: { readonly value: unknown } } {
  const box: { outcome?: { readonly value: unknown } } = {};
  call.then(
    (value) => {
      box.outcome = { value };
    },
    (error: unknown) => {
      box.outcome = { value: error };
    }
  );
  return box;
}

/** What `call` settles with, waited for as {@link until} waits: a call the client never answers
 *  fails the row naming `what`, not at the row's timeout. A call that rejects answers its error. */
async function settledWith(call: Promise<unknown>, what: string): Promise<unknown> {
  const box = settlement(call);
  await until(() => box.outcome !== undefined, what);
  return box.outcome?.value;
}

/** What `call` settled with by a few turns of the event loop, with no clock moved, or "pending". */
async function outcome(call: Promise<BrokerToken>): Promise<unknown> {
  const box = settlement(call);
  for (let turns = 0; turns < 3 && box.outcome === undefined; turns += 1) await turn();
  return box.outcome === undefined ? "pending" : box.outcome.value;
}

/** A row of the table: what the broker answers, the waits that pass at once, and the drive. A row
 *  whose broker leaves a trade unanswered gives each trade `SILENT_ATTEMPT_MS` and waits it out;
 *  every other row leaves `attemptMs` out, so each of its trades is given the client's own
 *  `TRADE_ATTEMPT_MS` and is read by what the broker answered, however slow the machine. */
interface Row {
  readonly answer: (trade: number, now: number) => Answer;
  readonly budget: number;
  readonly attemptMs?: number;
  readonly drive: (rig: Rig) => Promise<void>;
}

const ROWS: [string, Row][] = [
  [
    "pending, then granted: traded again on the short schedule, said once, and once more when the token lands",
    {
      answer: (n) => (n <= 2 ? "pending" : "granted"),
      budget: 10,
      drive: async ({ client, waits, lines, trades }) => {
        const token = await client.token();
        expect(token.accessToken).toStartWith("granted-");
        expect(trades()).toBe(3);
        expect(waits).toEqual([2_000, 4_000]);
        expect(lines).toHaveLength(2);
        expect(lines[0]).toContain(`no registration for this pod's session ${SESSION}`);
        expect(lines[1]).toContain(
          `answered this pod's session ${SESSION} once it was registered, 6 s and 2 failed trades after the first`
        );
      },
    },
  ],
  [
    "B1: pending, then a broker that never answers: each trade times out and is traded again, with a line at each wait",
    {
      answer: (n) => (n === 1 ? "pending" : "silent"),
      budget: 4,
      attemptMs: SILENT_ATTEMPT_MS,
      drive: async ({ client, waits, lines, trades }) => {
        const life = new AbortController();
        const call = client.token(life.signal, READY);
        await until(() => waits.length === 5, "at its fifth wait");
        expect(trades()).toBe(5);
        // The short schedule's first wait, then the long schedule's: a different failure starts
        // no schedule over.
        expect(waits).toEqual([2_000, 15_000, 30_000, 60_000, 120_000]);
        // The pending wait said once; each unanswered trade at its own wait.
        expect(lines).toHaveLength(5);
        for (const line of lines.slice(1)) {
          expect(line).toContain(
            `did not answer the trade of a pod's projected service-account token within ${SILENT_ATTEMPT_MS / 1000} s`
          );
        }
        expect(await outcome(call)).toBe("pending");
        life.abort();
        const ended = await outcome(call);
        expect(ended).toBeInstanceOf(TradeRetryingError);
        expect((ended as Error).message).toContain("did not answer");
        client.close();
      },
    },
  ],
  [
    "B2: pending, then a 503, then granted: the 503 is traded again, and the caller takes the token",
    {
      answer: (n) => (n === 1 ? "pending" : n === 2 ? "503" : "granted"),
      budget: 10,
      drive: async ({ client, waits, lines, trades }) => {
        const token = await client.token(new AbortController().signal, READY);
        expect(token.accessToken).toStartWith("granted-");
        expect(trades()).toBe(3);
        expect(waits).toEqual([2_000, 15_000]);
        expect(lines).toHaveLength(3);
        expect(lines[1]).toContain("HTTP 503");
        expect(lines[2]).toContain("once it was registered");
      },
    },
  ],
  [
    "refused: the loop ends at once, and a call before the next slot is told the same with no trade",
    {
      answer: () => "refused",
      budget: 10,
      drive: async ({ client, clock, lines, trades }) => {
        const first = await client.token().catch((error: unknown) => error);
        expect(first).toBeInstanceOf(BrokerRefusedError);
        expect((first as BrokerRefusedError).permanent).toBe(true);
        expect(trades()).toBe(1);
        expect(lines).toHaveLength(0);
        // A caller at a NATS reconnect's pace, 2 s on: answered at once, with no trade.
        clock.now += 2_000;
        expect(await client.token().catch((error: unknown) => error)).toBe(first);
        expect(trades()).toBe(1);
        // Past the slot, the next call trades again.
        clock.now += 15_000;
        expect(await client.token().catch((error: unknown) => error)).toBeInstanceOf(
          BrokerRefusedError
        );
        expect(trades()).toBe(2);
      },
    },
  ],
  [
    "connect refused twice, then granted: each refused connection is a trade, and the wait doubles",
    {
      answer: (n) => (n <= 2 ? "connect refused" : "granted"),
      budget: 10,
      drive: async ({ client, waits, lines, trades }) => {
        const token = await client.token(new AbortController().signal, READY);
        expect(token.accessToken).toStartWith("granted-");
        expect(trades()).toBe(3);
        expect(waits).toEqual([15_000, 30_000]);
        expect(lines).toHaveLength(2);
        for (const line of lines) expect(line).toContain("http://broker.test");
      },
    },
  ],
  [
    "never answers: the trade ends at its own timeout, and each caller is told what it waited for",
    {
      answer: () => "silent",
      budget: 0,
      attemptMs: SILENT_ATTEMPT_MS,
      drive: async ({ client, waits }) => {
        // A deadline before any trade has ended is told the trade still out has not been
        // answered, by the identity it trades: the caller's deadline is no answer about it.
        const early = new AbortController();
        const before = client.token(early.signal);
        early.abort(new Error("the caller's own deadline"));
        const unanswered = await outcome(before);
        expect(unanswered).toBeInstanceOf(TradeUnansweredError);
        expect((unanswered as Error).message).toBe(
          "the secrets broker has not yet answered the trade of a pod's projected service-account token, and the trade goes on in the background"
        );
        // A fail-fast caller with no deadline of its own is told when the trade's time runs out:
        // a trade that never returns ends, and so does the wait on it.
        const fast = client.token().catch((error: unknown) => error);
        // A caller that waits for ready waits on past the failure, and its deadline then names it.
        const late = new AbortController();
        const waiting = client.token(late.signal, READY).catch((error: unknown) => error);
        await until(() => waits.length === 1, "at its first wait");
        const told = await settledWith(fast, "the fail-fast caller told");
        expect(told).toBeInstanceOf(TradeRetryingError);
        expect((told as Error).message).toContain("did not answer");
        late.abort();
        const named = await settledWith(waiting, "the waiting caller told at its deadline");
        expect(named).toBeInstanceOf(TradeRetryingError);
        expect((named as Error).message).toContain("did not answer");
        client.close();
      },
    },
  ],
  [
    "B3: seven callers during a pending wait, a NATS reconnect's every 2 s, wake nothing",
    {
      answer: () => "pending",
      budget: 0,
      drive: async ({ client, clock, waits, trades, advance }) => {
        const first = client.token(new AbortController().signal).catch(() => undefined);
        await until(() => waits.length === 1, "at its first wait");
        for (let call = 0; call < 7; call++) {
          clock.now += 2_000;
          const deadline = new AbortController();
          const joined = client.token(deadline.signal);
          deadline.abort();
          expect(await outcome(joined)).toBeInstanceOf(RegistrationPendingError);
        }
        expect(trades()).toBe(1);
        // The schedule's own slot is what trades again.
        advance();
        await until(() => trades() === 2, "at its second trade");
        client.close();
        await settledWith(first, "the first caller told the client closed");
      },
    },
  ],
  [
    "a registration that lands just after a trade at the cap is traded for within one capped wait, 30 s",
    {
      // The registration lands 1 ms after the eighth pending trade, when the wait is at its cap.
      // The row's clock moves only in the loop's waits, so it reads the schedule alone: the time
      // from the landing to the trade that follows. That trade has its own 30 s, which the row's
      // immediate answer does not spend.
      answer: (n, now) => {
        if (n === 8) LANDS_AT.at = now + 1;
        return n <= 8 || now < LANDS_AT.at ? "pending" : "granted";
      },
      budget: 20,
      drive: async ({ client, clock, waits, trades }) => {
        const token = await client.token();
        expect(token.accessToken).toStartWith("granted-");
        expect(trades()).toBe(9);
        const untilNextTrade = clock.now - LANDS_AT.at;
        // The worst case of the schedule: the whole cap, with the jitter at its longest.
        expect(untilNextTrade).toBeLessThan(30_000);
        expect(waits).toEqual([2_000, 4_000, 8_000, 16_000, 30_000, 30_000, 30_000, 30_000]);
      },
    },
  ],
  [
    "inside the renewal window, a caller whose deadline ends before the loop's first trade answers takes the live token in hand",
    {
      answer: (n) => (n === 1 ? "granted" : "silent"),
      budget: 10,
      attemptMs: SILENT_ATTEMPT_MS,
      drive: async ({ client, clock, trades }) => {
        const first = await client.token();
        // Inside the 5-minute window ahead of the token's 600 s: the loop trades, and the broker
        // does not answer.
        clock.now += 400_000;
        const deadline = new AbortController();
        const call = client.token(deadline.signal);
        await until(() => trades() === 2, "at its second trade");
        deadline.abort();
        expect(await outcome(call)).toBe(first);
        client.close();
      },
    },
  ],
  [
    "after close, a waiting caller is told the client is closed, and nothing is traded again",
    {
      answer: () => "pending",
      budget: 0,
      drive: async ({ client, waits, trades }) => {
        const call = client.token();
        await until(() => waits.length === 1, "at its first wait");
        client.close();
        const told = await outcome(call);
        expect((told as Error).message).toBe(
          "the secrets broker client was closed, and trades no more"
        );
        expect(await outcome(client.token())).toBe(told);
        expect(trades()).toBe(1);
      },
    },
  ],
  [
    "after close, a fail-fast caller behind a failed trade is told the client is closed, not that it is retried",
    {
      answer: () => "503",
      budget: 0,
      drive: async ({ client, waits, trades }) => {
        const first = client.token().catch((error: unknown) => error);
        await until(() => waits.length === 1, "at its first wait");
        expect(await settledWith(first, "the fail-fast caller told")).toBeInstanceOf(
          TradeRetryingError
        );
        client.close();
        const told = await outcome(client.token());
        expect((told as Error).message).toBe(
          "the secrets broker client was closed, and trades no more"
        );
        expect(trades()).toBe(1);
      },
    },
  ],
  [
    "fail-fast: a caller behind a refused connection is told at once, and a wait-for-ready caller waits on",
    {
      answer: (n) => (n <= 1 ? "connect refused" : "granted"),
      budget: 0,
      drive: async ({ client, waits, trades, advance }) => {
        const ready = client.token(new AbortController().signal, READY);
        const fast = client.token(new AbortController().signal).catch((error: unknown) => error);
        await until(() => waits.length === 1, "at its first wait");
        const told = await settledWith(fast, "the fail-fast caller told");
        expect(told).toBeInstanceOf(TradeRetryingError);
        expect((told as Error).message).toContain("ECONNREFUSED");
        // A caller arriving now is told the same at once, with no trade of its own.
        expect(await outcome(client.token())).toBeInstanceOf(TradeRetryingError);
        expect(trades()).toBe(1);
        expect(await outcome(ready)).toBe("pending");
        advance();
        expect(await settledWith(ready, "the wait-for-ready caller's token")).toMatchObject({
          accessToken: expect.stringMatching(/^granted-/),
        });
      },
    },
  ],
  [
    "fail-fast: a caller behind a 503 is told at once, and a pending registration still waits within its deadline",
    {
      answer: (n) => (n === 1 ? "503" : "pending"),
      budget: 0,
      drive: async ({ client, waits, trades, advance }) => {
        const fast = client.token(new AbortController().signal).catch((error: unknown) => error);
        await until(() => waits.length === 1, "at its first wait");
        const told = await settledWith(fast, "the fail-fast caller told");
        expect(told).toBeInstanceOf(TradeRetryingError);
        expect((told as Error).message).toContain("HTTP 503");
        advance();
        await until(() => trades() === 2 && waits.length === 2, "at its second wait");
        // Pending: a fail-fast caller waits within its own deadline, as every caller does.
        const deadline = new AbortController();
        const pending = client.token(deadline.signal);
        expect(await outcome(pending)).toBe("pending");
        deadline.abort();
        expect(await outcome(pending)).toBeInstanceOf(RegistrationPendingError);
        client.close();
      },
    },
  ],
];

/** When the registration of the last row lands, on the row's clock. */
const LANDS_AT = { at: Number.POSITIVE_INFINITY };

test.each(ROWS)(
  "%s",
  async (_name, row) => {
    jest.useFakeTimers();
    const one = await rig(row.answer, row.budget, row.attemptMs ?? TRADE_ATTEMPT_MS);
    try {
      await row.drive(one);
    } finally {
      one.client.close();
      // A fake clock left behind stops the timers of every file the same worker runs after this.
      jest.useRealTimers();
    }
  },
  20_000
);
