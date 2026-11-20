import { afterEach, describe, expect, test } from "bun:test";
import { type ConnectionOptions, connect, type NatsConnection } from "nats";
import { LoggableError } from "../errors";
import {
  connectNats,
  NATS_DIAL_PLAN,
  type NatsDialPlan,
  natsConnectionOptions,
  natsDialBudget,
} from "../nats-connection";
import { noAuthService } from "../nats-credential";
import { type FakeNatsBehaviour, type FakeNatsServer, startFakeNats } from "./fake-nats-server";

// Real nats.js against a local server. The waits between dials run on the real clock on purpose,
// since only real time shows them, and are held only from below, which a late timer cannot break.
// A dial's bound is read from the dial instead of timed: the `timeout` it hands nats.js, and the
// error the bound ends it with. The bound starts before nats.js's own timer for the server, which
// starts only once the server's name is looked up, so a dial the server leaves silent ends with
// the bound's error. A row whose plan has a short budget runs the budget on a clock of its own
// (`rowClock`), so a busy machine cannot spend it; the row on a blocked event loop keeps the real
// clock, since its block is what spends the budget. The plans are scaled down so a row takes about
// a second.

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
    readonly firstBoundMs?: number;
    readonly stopAfterFirstFailure?: boolean;
    readonly stoppedFromStart?: boolean;
    /** The token of a broker credential is renewed between dials, and it sets this. */
    readonly ignoreAuthErrorAbort?: boolean;
    /** How long the event loop is blocked as the first wait begins. */
    readonly blockOnRetryMs?: number;
    /**
     * Runs the budget on the row's own clock (`now`) instead of the real one, so a busy machine
     * cannot spend it. The clock starts at 0 and moves by each announced wait, and by the bound of
     * each dial `silent` names (from 0) as that dial starts, since only its bound ends a dial the
     * server leaves silent and the code reads no clock while a dial runs. Any other dial takes no
     * time on it. With one server, a dial's `timeout` is its bound.
     */
    readonly rowClock?: { readonly silent?: (n: number) => boolean };
  } = {}
) {
  const delays: number[] = [];
  const bounds: number[] = [];
  // The row's clock as each dial began, read when the budget runs on it (`rowClock`).
  const clockAtDial: number[] = [];
  let clock = 0;
  // When each dial began, and when each wait began: the wait a dial really had is the gap from
  // the failure before it to its start, not the delay announced to `onRetry`.
  const dialStarts: number[] = [];
  const waitStarts: number[] = [];
  let stopped = extra.stoppedFromStart === true;
  const recording = (dialOptions: ConnectionOptions) => {
    const bound = dialOptions.timeout ?? Number.NaN;
    clockAtDial.push(clock);
    if (extra.rowClock?.silent?.(bounds.length) === true) clock += bound;
    bounds.push(bound);
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
        ...(extra.firstBoundMs === undefined ? {} : { firstBoundMs: extra.firstBoundMs }),
        ...(extra.rowClock === undefined ? {} : { now: () => clock }),
        stopped: () => stopped,
        onRetry: (_error, delayMs) => {
          delays.push(delayMs);
          clock += delayMs;
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
      delays,
      waited: waited(),
      bounds,
      clockAtDial,
      nc,
      error: undefined,
    };
  } catch (error) {
    return {
      delays,
      waited: waited(),
      bounds,
      clockAtDial,
      nc: undefined,
      error: error as { code?: unknown; message?: unknown },
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
    const dial = await dialWith(
      server.url,
      { firstDelayMs: 20, maxDelayMs: 80, budgetMs: 1_000 },
      { rowClock: {} }
    );
    expect(dial.error?.code).toBe("AUTHENTICATION_TIMEOUT");
    expect(dial.delays.slice(0, 4)).toEqual([20, 40, 80, 80]);
    waitedAtLeast(dial.waited, [20, 40, 80, 80]);
    expect(Math.max(...dial.delays)).toBe(80);
  }, 30_000);

  // A refusal, a server that says nothing for the second dial's whole 1 000 ms, then a third dial
  // that starts 1 100 ms in (two 50 ms waits and the second dial's bound), so 900 ms of the
  // 2 000 ms budget are left for it, less than its 1 000 ms `timeout`, and its bound ends it there.
  test("gives a later dial only the budget it has left", async () => {
    const server = fake((n) => (n === 0 ? "refuse" : "silent"));
    const dial = await dialWith(
      server.url,
      { firstDelayMs: 50, maxDelayMs: 50, budgetMs: 2_000 },
      { timeout: 1_000, rowClock: { silent: (n) => n > 0 } }
    );
    expect(dial.clockAtDial).toEqual([0, 50, 1_100]);
    expect(dial.bounds).toEqual([1_000, 1_000, 900]);
    expect(dial.error?.message).toBe("the NATS dial did not complete within 900 ms");
  }, 30_000);

  // The client writes its CONNECT into the closing socket, so a plain close fails a dial with the
  // socket's own error, or DISCONNECT when it carries none.
  test("ends with the last dial's own error", async () => {
    const server = fake((n) => (n === 0 ? "refuse" : "close"));
    const dial = await dialWith(
      server.url,
      { firstDelayMs: 10, maxDelayMs: 10, budgetMs: 1_000 },
      { rowClock: {} }
    );
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
  // the next dial's new token, as nats.js's `ignoreAuthErrorAbort` says for its own reconnects.
  test("dials again after an authorization refusal of a credential renewed between dials", async () => {
    const server = fake((n) => (n === 0 ? "refuse-authorization" : "serve"));
    const dial = await dialWith(
      server.url,
      { firstDelayMs: 20, maxDelayMs: 20, budgetMs: 2_000 },
      { ignoreAuthErrorAbort: true, rowClock: {} }
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
    const dial = await dialWith(
      server.url,
      { ...NATS_DIAL_PLAN, budgetMs: 0 },
      { firstBoundMs: 300 }
    );
    expect(dial.bounds).toEqual([300]);
    expect(dial.error?.message).toBe("the NATS dial did not complete within 300 ms");
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
