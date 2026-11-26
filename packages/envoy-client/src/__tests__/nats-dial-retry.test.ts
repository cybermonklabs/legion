import { afterEach, describe, expect, test } from "bun:test";
import { type ConnectionOptions, connect, type NatsConnection } from "nats";
import { LoggableError } from "../errors";
import {
  connectNats,
  NATS_DIAL_PLAN,
  NATS_MIN_REDIAL_BOUND_MS,
  type NatsDialHooks,
  type NatsDialPlan,
  natsConnectionOptions,
  natsDialBudget,
} from "../nats-connection";
import {
  type BrokerNatsCredential,
  type BrokerTokenRefusal,
  noAuthService,
} from "../nats-credential";
import { type FakeNatsBehaviour, type FakeNatsServer, startFakeNats } from "./fake-nats-server";

// The first describe runs real nats.js against a local server, on the real clock on purpose: each
// row holds how long `connectNats` waits between dials and how it bounds them, which only real
// time shows; the plans are scaled down so a row takes about a second. The broker-token rows at
// the end script each dial's failure, with no network and no timer.

const servers: FakeNatsServer[] = [];
const connections: NatsConnection[] = [];
afterEach(async () => {
  for (const nc of connections.splice(0)) await nc.close().catch(() => undefined);
  for (const server of servers.splice(0)) server.stop();
});

function fake(behaviour: (n: number) => FakeNatsBehaviour): FakeNatsServer {
  const server = startFakeNats(behaviour);
  servers.push(server);
  return server;
}

function options(url: string): ConnectionOptions {
  return natsConnectionOptions({
    servers: [url],
    name: "retry-test",
    credential: { kind: "none" },
  });
}

/** Runs `connectNats` with `plan`, recording each wait it announces and each dial's bound. */
async function dialWith(
  url: string,
  plan: NatsDialPlan,
  extra: {
    readonly timeout?: number;
    readonly stopAfterFirstFailure?: boolean;
    readonly stoppedFromStart?: boolean;
    /** The token of a broker credential is renewed between dials, and it sets this. */
    readonly ignoreAuthErrorAbort?: boolean;
    /** How long the event loop is blocked as the first wait begins. */
    readonly blockOnRetryMs?: number;
  } = {}
) {
  const delays: number[] = [];
  const bounds: number[] = [];
  // When each dial began, and when each wait began: the wait a dial really had is the gap from
  // the failure before it to its start, not the delay announced to `onRetry`.
  const dialStarts: number[] = [];
  const waitStarts: number[] = [];
  let stopped = extra.stoppedFromStart === true;
  const started = performance.now();
  const recording = (dialOptions: ConnectionOptions) => {
    bounds.push(dialOptions.timeout ?? Number.NaN);
    dialStarts.push(performance.now());
    return connect(dialOptions);
  };
  const waited = () => waitStarts.map((at, i) => (dialStarts[i + 1] ?? Number.NaN) - at);
  try {
    const nc = await connectNats(
      recording,
      {
        ...options(url),
        ...(extra.timeout === undefined ? {} : { timeout: extra.timeout }),
        ...(extra.ignoreAuthErrorAbort === true ? { ignoreAuthErrorAbort: true } : {}),
      },
      plan,
      {
        stopped: () => stopped,
        onRetry: (_error, delayMs) => {
          delays.push(delayMs);
          waitStarts.push(performance.now());
          if (extra.stopAfterFirstFailure === true) stopped = true;
          if (extra.blockOnRetryMs !== undefined && delays.length === 1) {
            const until = performance.now() + extra.blockOnRetryMs;
            while (performance.now() < until) {}
          }
        },
      }
    );
    connections.push(nc);
    return {
      ms: performance.now() - started,
      delays,
      waited: waited(),
      bounds,
      nc,
      error: undefined,
    };
  } catch (error) {
    return {
      ms: performance.now() - started,
      delays,
      waited: waited(),
      bounds,
      nc: undefined,
      error: error as { code?: unknown },
    };
  }
}

/** Fails unless each wait lasted at least its announced delay, less 5 ms of timer slack. */
function waitedAtLeast(waited: readonly number[], delays: readonly number[]): void {
  delays.forEach((delay, i) => {
    const real = waited[i] ?? Number.NaN;
    if (!(real >= delay - 5))
      throw new Error(`wait ${i + 1} lasted ${Math.round(real)} ms, not its ${delay} ms`);
  });
}

describe("connectNats", () => {
  test("dials again after 250 ms and 500 ms and answers the third dial's connection", async () => {
    const server = fake((n) => (n < 2 ? "refuse" : "serve"));
    const dial = await dialWith(server.url, NATS_DIAL_PLAN);
    expect(dial.error).toBeUndefined();
    expect(dial.delays).toEqual([250, 500]);
    waitedAtLeast(dial.waited, [250, 500]);
    expect(server.connections()).toBe(3);
  }, 30_000);

  test("doubles each wait up to the plan's cap", async () => {
    const server = fake(() => "refuse");
    const dial = await dialWith(server.url, { firstDelayMs: 20, maxDelayMs: 80, budgetMs: 1_000 });
    expect(dial.error?.code).toBe("AUTHENTICATION_TIMEOUT");
    expect(dial.delays.slice(0, 4)).toEqual([20, 40, 80, 80]);
    waitedAtLeast(dial.waited, [20, 40, 80, 80]);
    expect(Math.max(...dial.delays)).toBe(80);
  }, 30_000);

  // A refusal, a server that says nothing for the second dial's whole 1 000 ms, then a third dial
  // that starts at least 1 100 ms in (two 50 ms waits and the second dial's bound), so at most
  // 900 ms of the 2 000 ms budget are left for it, less than its 1 000 ms `timeout`. It still
  // starts while 500 ms are left as long as it starts within 1 500 ms: about 400 ms of slack.
  test("gives a later dial only the budget it has left", async () => {
    const server = fake((n) => (n === 0 ? "refuse" : "silent"));
    const dial = await dialWith(
      server.url,
      { firstDelayMs: 50, maxDelayMs: 50, budgetMs: 2_000 },
      { timeout: 1_000 }
    );
    expect(dial.error).toBeDefined();
    if (dial.ms >= 2_100)
      throw new Error(`the dials took ${Math.round(dial.ms)} ms, past the 2 000 ms budget`);
    expect(dial.bounds).toHaveLength(3);
    expect(dial.bounds.at(-1)).toBeGreaterThanOrEqual(NATS_MIN_REDIAL_BOUND_MS);
    expect(dial.bounds.at(-1)).toBeLessThanOrEqual(900);
  }, 30_000);

  // The client writes its CONNECT into the closing socket, so a plain close fails a dial with the
  // socket's own error, or DISCONNECT when it carries none.
  test("ends with the last dial's own error", async () => {
    const server = fake((n) => (n === 0 ? "refuse" : "close"));
    const dial = await dialWith(server.url, { firstDelayMs: 10, maxDelayMs: 10, budgetMs: 1_000 });
    expect(["DISCONNECT", "EPIPE", "ECONNRESET"]).toContain(String(dial.error?.code));
    expect(server.connections()).toBeGreaterThanOrEqual(2);
  }, 30_000);

  // A blocked event loop makes a wait run long: 700 ms blocked and the 100 ms wait leave under
  // 400 ms of the 1 200 ms budget, too little for a dial to answer in, so none starts.
  test("starts no dial when its wait ran past the budget", async () => {
    const server = fake(() => "refuse");
    const dial = await dialWith(
      server.url,
      { firstDelayMs: 100, maxDelayMs: 100, budgetMs: 1_200 },
      { blockOnRetryMs: 700 }
    );
    expect(dial.delays).toEqual([100]);
    expect(dial.error?.code).toBe("AUTHENTICATION_TIMEOUT");
    expect(server.connections()).toBe(1);
  }, 30_000);

  test("starts no dial once its caller stopped before the first", async () => {
    const server = fake(() => "serve");
    const dial = await dialWith(server.url, NATS_DIAL_PLAN, { stoppedFromStart: true });
    expect(dial.error).toBeInstanceOf(LoggableError);
    expect(server.connections()).toBe(0);
  }, 30_000);

  test("starts no dial once its caller stops during a wait", async () => {
    const server = fake(() => "refuse");
    const dial = await dialWith(
      server.url,
      { firstDelayMs: 200, maxDelayMs: 200, budgetMs: 5_000 },
      { stopAfterFirstFailure: true }
    );
    expect(dial.delays).toEqual([200]);
    expect(dial.error?.code).toBe("AUTHENTICATION_TIMEOUT");
    expect(server.connections()).toBe(1);
  }, 30_000);

  test("does not dial again after an authorization refusal", async () => {
    const server = fake(() => "refuse-authorization");
    const dial = await dialWith(server.url, NATS_DIAL_PLAN);
    expect(dial.error?.code).toBe("AUTHORIZATION_VIOLATION");
    expect(dial.delays).toEqual([]);
    expect(server.connections()).toBe(1);
  }, 30_000);

  // A broker token is renewed between dials, so the hub's refusal of an expired one is cured by
  // the next dial's new token, as nats.js's `ignoreAuthErrorAbort` says for its own reconnects. A
  // caller that names no broker credential (`hooks.credential`) keeps the plan's waits.
  test("dials again after an authorization refusal of a credential renewed between dials", async () => {
    const server = fake((n) => (n === 0 ? "refuse-authorization" : "serve"));
    const dial = await dialWith(
      server.url,
      { firstDelayMs: 20, maxDelayMs: 20, budgetMs: 2_000 },
      { ignoreAuthErrorAbort: true }
    );
    expect(dial.error).toBeUndefined();
    expect(dial.delays).toEqual([20]);
    expect(server.connections()).toBe(2);
  }, 30_000);

  test("does not dial again after a failure no redial cures", async () => {
    let dials = 0;
    const error = await connectNats(
      async () => {
        dials += 1;
        throw new LoggableError(noAuthService("NATS_AUTH"));
      },
      options("nats://127.0.0.1:4222"),
      { firstDelayMs: 5, maxDelayMs: 5, budgetMs: 2_000 }
    ).catch((e: unknown) => e);
    expect(error).toBeInstanceOf(LoggableError);
    expect(dials).toBe(1);
  }, 30_000);

  test("bounds the first dial by the caller's time", async () => {
    const server = fake(() => "silent");
    const started = performance.now();
    const error = await connectNats(
      connect,
      options(server.url),
      { ...NATS_DIAL_PLAN, budgetMs: 0 },
      {
        firstBoundMs: 300,
      }
    ).catch((e) => e);
    const ms = performance.now() - started;
    expect(error).toBeDefined();
    if (ms >= 600)
      throw new Error(`the first dial took ${Math.round(ms)} ms, past its 300 ms bound`);
  }, 30_000);
});

describe("natsDialBudget", () => {
  test("gives a pass's dial what the pass has left after the rest of it, at most the plan's budget, and none when nothing is left", () => {
    expect(natsDialBudget(90_000, 20_250, NATS_DIAL_PLAN)).toBe(25_000);
    expect(natsDialBudget(30_000, 20_250, NATS_DIAL_PLAN)).toBe(9_750);
    expect(natsDialBudget(7_500, 20_250, NATS_DIAL_PLAN)).toBe(0);
    expect(natsDialBudget(Number.POSITIVE_INFINITY, 20_250, NATS_DIAL_PLAN)).toBe(25_000);
  });
});

/**
 * A broker credential as `connectNats` reads it: each refusal of its token is answered in turn
 * from `drops` (whether that refusal dropped the token; none past the list), and it records each
 * refusal's code, each admission's dial start, and the trades a drop starts. Its own count of
 * refusals is the credential's (`brokerNatsCredential`), which `nats-credential-keeper.test.ts`
 * holds.
 */
function brokerCredential(drops: readonly boolean[] = []) {
  const seen = { refused: [] as string[], admitted: [] as (number | undefined)[], trades: 0 };
  const broker = {
    refused: (code: string): BrokerTokenRefusal => {
      seen.refused.push(code);
      return { droppedBefore: false, dropped: drops[seen.refused.length - 1] === true };
    },
    admitted: (dialedAt?: number) => {
      seen.admitted.push(dialedAt);
    },
    renew: async () => {
      seen.trades += 1;
    },
  } as unknown as BrokerNatsCredential;
  return { credential: { kind: "broker", broker } as const, seen };
}

/** A dial's failure with nats.js's code. */
function coded(code: string): Error {
  return Object.assign(new Error(code), { code });
}

/**
 * Runs `connectNats` over `codes`, no network and no timer: each dial fails with the next code
 * while one is left, and then opens. A code that no redial cures ends the run there, and so does
 * a wait past `budgetMs`. Records each wait announced and each dial whose refusal dropped the
 * token, and sleeps none of the waits.
 */
async function scriptedDials(
  codes: readonly string[],
  hooks: Pick<NatsDialHooks, "credential" | "random"> = {},
  budgetMs = Number.POSITIVE_INFINITY
) {
  const delays: number[] = [];
  const dropsAt: number[] = [];
  let dials = 0;
  const opened = await connectNats(
    async () => {
      const code = codes[dials];
      dials += 1;
      if (code !== undefined) throw coded(code);
      return { close: async () => {} } as unknown as NatsConnection;
    },
    { servers: ["nats://127.0.0.1:4222"], ignoreAuthErrorAbort: true },
    { firstDelayMs: 250, maxDelayMs: 4_000, budgetMs },
    {
      ...hooks,
      delay: async () => {},
      onRetry: (_error, delayMs) => delays.push(delayMs),
      onTokenDropped: (_error, attempt) => dropsAt.push(attempt),
    }
  ).then(
    () => true,
    () => false
  );
  return { opened, dials, delays, dropsAt };
}

const REFUSED = "AUTHORIZATION_VIOLATION";
const TIMED_OUT = "AUTHENTICATION_TIMEOUT";

describe("connectNats with a broker token", () => {
  test("waits 2-4 s, 4-8 s, 8-16 s, then 10-20 s after each refusal, its lowest at the lowest draw", async () => {
    const { credential } = brokerCredential();
    const run = await scriptedDials([REFUSED, REFUSED, REFUSED, REFUSED, REFUSED], {
      credential,
      random: () => 0,
    });
    expect(run.opened).toBe(true);
    expect(run.delays).toEqual([2_000, 4_000, 8_000, 10_000, 10_000]);
  });

  test("and its highest at the highest draw", async () => {
    const { credential } = brokerCredential();
    const run = await scriptedDials([REFUSED, REFUSED, REFUSED, REFUSED, REFUSED], {
      credential,
      random: () => 1 - 1e-12,
    });
    expect(run.delays).toEqual([4_000, 8_000, 16_000, 20_000, 20_000]);
  });

  test("leaves the plan's wait for another failure where it was, undoubled by a refusal", async () => {
    const { credential } = brokerCredential();
    const run = await scriptedDials([REFUSED, TIMED_OUT, REFUSED, TIMED_OUT, TIMED_OUT], {
      credential,
      random: () => 0,
    });
    expect(run.delays).toEqual([2_000, 250, 4_000, 500, 1_000]);
  });

  test("a caller that names no broker credential keeps the plan's waits for a refusal, and drops nothing", async () => {
    const run = await scriptedDials([REFUSED, REFUSED, REFUSED]);
    expect(run.delays).toEqual([250, 500, 1_000]);
    expect(run.dropsAt).toEqual([]);
  });

  test("a refusal that drops the token starts a trade and is told at its dial; an admitted dial is told to the credential with when it began", async () => {
    const { credential, seen } = brokerCredential([false, true]);
    const started = Date.now();
    const run = await scriptedDials([REFUSED, REFUSED, REFUSED], { credential, random: () => 0 });
    expect(run.opened).toBe(true);
    expect(run.dropsAt).toEqual([2]);
    expect(seen.refused).toEqual([REFUSED, REFUSED, REFUSED]);
    expect(seen.trades).toBe(1);
    // The fourth dial opened, and the credential is told when that dial began.
    expect(seen.admitted).toHaveLength(1);
    expect(seen.admitted[0]).toBeGreaterThanOrEqual(started);
    expect(seen.admitted[0]).toBeLessThanOrEqual(Date.now());
  });

  test("asks the credential only of an authorization refusal, and waits on the backoff for the account's lapse too", async () => {
    const { credential, seen } = brokerCredential();
    const run = await scriptedDials([TIMED_OUT, "ACCOUNT_EXPIRED", REFUSED], {
      credential,
      random: () => 0,
    });
    expect(seen.refused).toEqual(["ACCOUNT_EXPIRED", REFUSED]);
    expect(run.delays).toEqual([250, 2_000, 4_000]);
  });

  test("tells of a drop at the last dial its budget affords, which no redial follows", async () => {
    // A pass with 4 s to dial in, at the lowest draws: refused at once, again 2 s on, and the 4 s
    // wait after that second refusal is past the budget, so the call ends on the refusal that
    // dropped the token. The first wait fits while less than 1.5 s has passed before it.
    const { credential, seen } = brokerCredential([false, true]);
    const run = await scriptedDials(
      [REFUSED, REFUSED, REFUSED],
      { credential, random: () => 0 },
      4_000
    );
    expect(run.opened).toBe(false);
    expect(run.dials).toBe(2);
    expect(run.delays).toEqual([2_000]);
    expect(run.dropsAt).toEqual([2]);
    expect(seen.trades).toBe(1);
  });
});
