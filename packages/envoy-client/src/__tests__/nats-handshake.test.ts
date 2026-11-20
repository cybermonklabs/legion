import { afterEach, describe, expect, test } from "bun:test";
import { type ConnectionOptions, connect, Events, type NatsConnection } from "nats";
import { NatsConnectionImpl } from "nats/lib/nats-base-client/nats";
import { setTransportFactory } from "nats/lib/nats-base-client/transport";
import { NodeTransport } from "nats/lib/src/node_transport";
import { LoggableError } from "../errors";
import {
  connectNats,
  NATS_DIAL_TIMEOUT_MS,
  NATS_MIN_REDIAL_BOUND_MS,
  natsConnectionOptions,
} from "../nats-connection";
import {
  type FakeNatsBehaviour,
  type FakeNatsServer,
  LATE_INFO_MS,
  startFakeNats,
} from "./fake-nats-server";

// Real nats.js against a local server that speaks only the handshake, so each row reads what
// nats.js 2.29.3, as this repository patches it, does when a server closes or refuses a dial. The
// rows run on the real clock, since fake timers would not drive nats.js's own timers and sockets,
// but no row holds a dial or a close to a short upper limit, which a busy machine breaks with the
// code right. A dial that fails at once is one that ends with the server's error, not nats.js's
// timer's; a dial past its bound ends with the bound's own error, whose timer starts before
// nats.js's, which waits for the name lookup. A wait for what must happen is bounded by the row's
// own limit or by a deadline of 5 s or more, far past what it takes; a row that says nothing more
// happens waits out a short, named stretch, which a busy machine can only make longer.

const servers: FakeNatsServer[] = [];
const connections: NatsConnection[] = [];
afterEach(async () => {
  for (const nc of connections.splice(0)) await nc.close().catch(() => undefined);
  for (const server of servers.splice(0)) server.stop();
});

function fake(behaviour: (n: number) => FakeNatsBehaviour, lateInfoMs?: number): FakeNatsServer {
  const server = startFakeNats(behaviour, lateInfoMs);
  servers.push(server);
  return server;
}

function options(urls: readonly string[]) {
  return {
    ...natsConnectionOptions({
      servers: urls,
      name: "handshake-test",
      credential: { kind: "none" },
    }),
    noRandomize: true,
  };
}

/** How long `promise` took to settle, and how it settled. */
async function timed<T>(
  promise: Promise<T>
): Promise<{ ms: number; value?: T; error?: { code?: unknown; message: string } }> {
  const started = performance.now();
  try {
    const value = await promise;
    return { ms: performance.now() - started, value };
  } catch (error) {
    const { code, message } = error as { code?: unknown; message: string };
    return { ms: performance.now() - started, error: { code, message } };
  }
}

/**
 * A dial timeout longer than any row's own limit: a handshake that waits for nats.js's timer,
 * instead of ending on the server's close or on the row's, does not end within the row.
 */
const UNREACHED_TIMEOUT_MS = 120_000;

/** Resolves once `predicate` holds, or rejects after `ms`. */
async function until(predicate: () => boolean, ms: number, what: string): Promise<void> {
  const deadline = performance.now() + ms;
  while (!predicate()) {
    if (performance.now() > deadline) throw new Error(`${what} did not happen within ${ms} ms`);
    const tick = Promise.withResolvers<void>();
    setTimeout(tick.resolve, 10);
    await tick.promise;
  }
}

/** Resolves once `predicate` holds: only the row's own limit ends a wait for what never comes. */
async function eventually(predicate: () => boolean): Promise<void> {
  while (!predicate()) await pause(10);
}

/** Counts each status of `type` on `nc`. */
function countStatus(nc: NatsConnection, type: string): { count: () => number } {
  let count = 0;
  void (async () => {
    for await (const status of nc.status()) if (status.type === type) count += 1;
  })();
  return { count: () => count };
}

/**
 * nats.js's `connect`, with the name of each server resolved by `lookup` in place of the system
 * resolver: nats.js's own `connect` sets its transport (and so its resolver) at every call, so a
 * row that needs a name with two addresses, or a slow lookup, opens its connection through the
 * transport factory itself, as that `connect` does.
 */
function connectResolving(lookup: () => Promise<string[]>) {
  return (opts: ConnectionOptions): Promise<NatsConnection> => {
    setTransportFactory({ factory: () => new NodeTransport(), dnsResolveFn: lookup });
    return NatsConnectionImpl.connect(opts);
  };
}

/** `ms` of real time. */
async function pause(ms: number): Promise<void> {
  const done = Promise.withResolvers<void>();
  setTimeout(done.resolve, ms);
  await done.promise;
}

describe("a NATS dial that the server closes during the handshake", () => {
  // A dial the close does not fail waits for nats.js's own 10 s timer and ends with its TIMEOUT,
  // so the server's error, or the socket's, is what says the dial failed at once.
  test("fails at once with the server's authentication timeout", async () => {
    const server = fake(() => "refuse");
    const dial = await timed(connect(options([server.url])));
    expect(dial.error?.code).toBe("AUTHENTICATION_TIMEOUT");
  }, 30_000);

  // The client writes its CONNECT into the closing socket, so the socket's own error (EPIPE or
  // ECONNRESET) is usually what the close carries; DISCONNECT when it carries none.
  const CLOSE_CODES = ["DISCONNECT", "EPIPE", "ECONNRESET"];

  test("fails at once with the socket's error, or DISCONNECT, when the server closes with no -ERR", async () => {
    const server = fake(() => "close");
    const dial = await timed(connect(options([server.url])));
    expect(CLOSE_CODES).toContain(String(dial.error?.code));
  }, 30_000);

  test("fails at once with the last server's failure, not an earlier server's refusal", async () => {
    const refusing = fake(() => "refuse");
    const closing = fake(() => "close");
    const dial = await timed(connect(options([refusing.url, closing.url])));
    expect(closing.connections()).toBe(1);
    expect(CLOSE_CODES).toContain(String(dial.error?.code));
  }, 60_000);

  // The connection's timeout is one no row reaches, so a refused handshake that waited for
  // nats.js's timer instead of failing at once would not reconnect within the wait below.
  test("fails at once on a reconnect: one refused handshake, then exactly one reconnect", async () => {
    const server = fake((n) => (n === 1 ? "refuse" : "serve"));
    const nc = await connect({
      ...options([server.url]),
      timeout: UNREACHED_TIMEOUT_MS,
      reconnectTimeWait: 200,
      reconnectJitter: 0,
    });
    connections.push(nc);
    const reconnects = countStatus(nc, Events.Reconnect);
    server.dropAll();
    await until(() => reconnects.count() >= 1, 25_000, "a reconnect");
    expect(server.connections()).toBe(3);
    // A second report of the same reconnect would come at once; none does.
    const settle = Promise.withResolvers<void>();
    setTimeout(settle.resolve, 500);
    await settle.promise;
    expect(reconnects.count()).toBe(1);
  }, 60_000);

  // The reconnect's handshake has a timeout no row reaches, so a close that waited on it would not
  // end before the row's own limit.
  test("keeps a close during a reconnect's handshake final", async () => {
    const server = fake((n) => (n === 0 ? "serve" : "hold"));
    const nc = await connect({
      ...options([server.url]),
      timeout: UNREACHED_TIMEOUT_MS,
      reconnectTimeWait: 200,
      reconnectJitter: 0,
    });
    server.dropAll();
    await until(() => server.connections() === 2, 10_000, "the reconnect's dial");
    const closing = nc.close();
    await nc.closed();
    await closing;
    const after = Promise.withResolvers<void>();
    setTimeout(after.resolve, 3_000);
    await after.promise;
    expect(server.connections()).toBe(2);
  }, 30_000);

  test("keeps the authorization rule: refused reconnects end the connection", async () => {
    const server = fake((n) => (n === 0 ? "serve" : "refuse-authorization"));
    const nc = await connect({
      ...options([server.url]),
      reconnectTimeWait: 200,
      reconnectJitter: 0,
    });
    server.dropAll();
    const closed = await timed(
      Promise.race([
        nc.closed(),
        new Promise((_, reject) =>
          setTimeout(() => reject(new Error("still reconnecting")), 10_000)
        ),
      ])
    );
    expect(closed.error).toBeUndefined();
    expect(server.connections()).toBeLessThanOrEqual(3);
  }, 30_000);

  // A close() that lands after the reconnect's PONG resolved and before the connection is marked
  // up: the row calls close() inside nats.js's own PONG handling, which is that moment.
  test("keeps a close between a reconnect's PONG and its connection final", async () => {
    const server = fake(() => "serve");
    const nc = await connect({
      ...options([server.url]),
      reconnectTimeWait: 100,
      reconnectJitter: 0,
    });
    const handler = (
      nc as unknown as {
        protocol: {
          connected: boolean;
          processPong: () => void;
          heartbeats: { timer?: unknown };
        };
      }
    ).protocol;
    const processPong = handler.processPong.bind(handler);
    let armed = true;
    handler.processPong = () => {
      processPong();
      if (!armed) return;
      armed = false;
      void nc.close();
    };
    server.dropAll();
    await until(() => !armed, 10_000, "the reconnect's PONG");
    await nc.closed();
    await pause(300);
    expect(handler.connected).toBe(false);
    expect(handler.heartbeats.timer).toBeUndefined();
  }, 30_000);

  test("keeps a close during a reconnect's name lookup final", async () => {
    const server = fake((n) => (n === 0 ? "serve" : "hold"));
    const port = new URL(server.url).port;
    let lookups = 0;
    const inReconnectLookup = Promise.withResolvers<void>();
    const reconnectLookupDone = Promise.withResolvers<void>();
    const nc = await connectResolving(async () => {
      lookups += 1;
      if (lookups > 1) {
        // The reconnect's lookup: the row closes while it is held here, then lets it answer.
        inReconnectLookup.resolve();
        await pause(1_000);
        reconnectLookupDone.resolve();
      }
      return ["127.0.0.1"];
    })({ ...options([`nats://fake.test:${port}`]), reconnectTimeWait: 200, reconnectJitter: 0 });
    server.dropAll();
    await inReconnectLookup.promise;
    await nc.close();
    await reconnectLookupDone.promise;
    // An absence: a dial the close did not end would open its connection within this window.
    await pause(1_000);
    expect(server.connections()).toBe(1);
  }, 30_000);

  test("keeps a close during the first of a name's two addresses final", async () => {
    const server = fake((n) => (n === 0 ? "serve" : "hold"));
    const port = new URL(server.url).port;
    const nc = await connectResolving(async () => ["127.0.0.1", "127.0.0.1"])({
      ...options([`nats://fake.test:${port}`]),
      reconnectTimeWait: 200,
      reconnectJitter: 0,
    });
    server.dropAll();
    await until(() => server.connections() === 2, 10_000, "the reconnect's first address");
    await nc.close();
    await pause(2_000);
    expect(server.connections()).toBe(2);
  }, 30_000);
});

describe("a NATS dial's bound", () => {
  test("natsConnectionOptions bounds each dial at NATS_DIAL_TIMEOUT_MS, 10 s", () => {
    expect(NATS_DIAL_TIMEOUT_MS).toBe(10_000);
    expect(options(["nats://127.0.0.1:4222"]).timeout).toBe(10_000);
  });

  // Load cannot make a timer fire early, so the lower limit says the dial waited out its bound.
  test("a server that never answers fails the dial at its 10 s bound, not before 9.5 s", async () => {
    const server = fake(() => "silent");
    const once = { firstDelayMs: 0, maxDelayMs: 0, budgetMs: 0 };
    const dial = await timed(connectNats(connect, options([server.url]), once));
    expect(dial.error?.message).toBe(
      `the NATS dial did not complete within ${NATS_DIAL_TIMEOUT_MS} ms`
    );
    expect(dial.ms).toBeGreaterThanOrEqual(9_500);
  }, 60_000);

  // Each server gets half the bound as nats.js's own timeout; the bound around the whole dial is
  // what ends it, before nats.js would give up on the second server.
  test("two servers that never answer fail the dial at the bound", async () => {
    const first = fake(() => "silent");
    const second = fake(() => "silent");
    const once = { firstDelayMs: 0, maxDelayMs: 0, budgetMs: 0 };
    const dial = await timed(connectNats(connect, options([first.url, second.url]), once));
    expect(dial.error?.message).toBe(
      `the NATS dial did not complete within ${NATS_DIAL_TIMEOUT_MS} ms`
    );
  }, 60_000);

  // nats.js gives each address of a name a `timeout` of its own, so only the bound around the
  // whole `connect()` ends a dial of two silent addresses at 1 s.
  test("is whole: a name with two addresses that never answer fails the dial at the bound", async () => {
    const server = fake(() => "silent");
    const port = new URL(server.url).port;
    const once = { firstDelayMs: 0, maxDelayMs: 0, budgetMs: 0 };
    const dial = await timed(
      connectNats(
        connectResolving(async () => ["127.0.0.1", "127.0.0.1"]),
        { ...options([`nats://fake.test:${port}`]), timeout: 1_000 },
        once
      )
    );
    expect(dial.error?.message).toBe("the NATS dial did not complete within 1000 ms");
  }, 30_000);

  // The attempt nats.js is still making when the bound expires is abandoned with it: it dials no
  // further address, so a redial does not overlap dials whose result would be closed unread.
  test("is whole: a dial past its bound dials no further address", async () => {
    const server = fake(() => "silent");
    const port = new URL(server.url).port;
    const once = { firstDelayMs: 0, maxDelayMs: 0, budgetMs: 0 };
    const dial = await timed(
      connectNats(
        connectResolving(async () => ["127.0.0.1", "127.0.0.1", "127.0.0.1"]),
        { ...options([`nats://fake.test:${port}`]), timeout: 600 },
        once
      )
    );
    expect(dial.error?.message).toBe("the NATS dial did not complete within 600 ms");
    // Each of the other two addresses would have opened a connection within 1 200 ms more.
    await pause(2_000);
    expect(server.connections()).toBe(1);
  }, 30_000);

  // nats.js tries the next server when one fails, and looks its name up first: a dial past its
  // bound looks no further name up, so a lookup that never answers cannot keep it pending. The
  // first name has two silent addresses, so the bound expires while nats.js is still dialing
  // the first server, which nats.js leaves only after the second address has timed out too.
  test("is whole: a dial past its bound looks no further server's name up", async () => {
    const server = fake(() => "silent");
    const port = new URL(server.url).port;
    const looked: string[] = [];
    const once = { firstDelayMs: 0, maxDelayMs: 0, budgetMs: 0 };
    const lookup = async (host: string): Promise<string[]> => {
      looked.push(host);
      return host === "first.test" ? ["127.0.0.1", "127.0.0.1"] : ["127.0.0.1"];
    };
    const dial = await timed(
      connectNats(
        connectResolving(lookup as () => Promise<string[]>),
        { ...options([`nats://first.test:${port}`, `nats://second.test:${port}`]), timeout: 600 },
        once
      )
    );
    expect(dial.error?.message).toBe("the NATS dial did not complete within 600 ms");
    // The first server's two addresses: the bound expired inside the second.
    await until(() => server.connections() === 2, 5_000, "the first server's second address");
    // An absence: the abandoned connect() moves to the next server once that address times out,
    // about 600 ms after it opened, and would look its name up then.
    await pause(1_500);
    expect(looked).toEqual(["first.test"]);
  }, 30_000);

  // nats.js looks a name up before its timer starts, so a lookup that never answers would hold
  // the dial for good: without the bound, the dial would not end before the row's own limit.
  test("is whole: a name lookup that never answers fails the dial at the bound", async () => {
    const once = { firstDelayMs: 0, maxDelayMs: 0, budgetMs: 0 };
    const dial = await timed(
      connectNats(
        connectResolving(() => new Promise<string[]>(() => {})),
        { ...options(["nats://fake.test:4222"]), timeout: 1_000 },
        once
      )
    );
    expect(dial.error?.message).toBe("the NATS dial did not complete within 1000 ms");
  }, 30_000);

  test("is whole: a connect that settles late fails at the bound with the plugin's own error, and its connection is closed", async () => {
    let closed = false;
    const late = Promise.withResolvers<NatsConnection>();
    setTimeout(
      () =>
        late.resolve({
          close: async () => {
            closed = true;
          },
        } as unknown as NatsConnection),
      800
    );
    const once = { firstDelayMs: 0, maxDelayMs: 0, budgetMs: 0 };
    const dial = await timed(connectNats(() => late.promise, { timeout: 300 }, once));
    expect(dial.error?.message).toBe("the NATS dial did not complete within 300 ms");
    await eventually(() => closed);
  }, 30_000);

  // The row reads each server's share of the bound from the `timeout` the dial gives nats.js:
  // whether the second server's handshake then ends inside the bound is a race a busy machine
  // decides. The second server is dialed once the first one's share runs out, before the bound, so
  // a dial that gave the first server the whole bound would never reach it.
  test("shares the bound among the servers, so a dial reaches a second server when the first never answers", async () => {
    const silent = fake(() => "silent");
    const serving = fake(() => "serve");
    const once = { firstDelayMs: 0, maxDelayMs: 0, budgetMs: 0 };
    const shares: (number | undefined)[] = [];
    const dial = await timed(
      connectNats(
        (dialOptions) => {
          shares.push(dialOptions.timeout);
          return connect(dialOptions);
        },
        { ...options([silent.url, serving.url]), timeout: 1_000 },
        once
      )
    );
    if (dial.value !== undefined) connections.push(dial.value);
    expect(shares).toEqual([1_000 / 2]);
    await eventually(() => serving.connections() === 1);
  }, 30_000);

  // Each of four servers gets a quarter of a 1 200 ms bound, 300 ms, below the 500 ms floor, so the
  // floor is what nats.js is given for each server: time for the second server, which sends its
  // INFO 400 ms after it accepts, to answer. The row reads the share from the `timeout` the dial
  // gives nats.js; whether that INFO then beats nats.js's own timer is a 100 ms race, which a busy
  // machine decides.
  test("shares the bound among the servers, and gives each one at least 500 ms", async () => {
    const boundMs = 1_200;
    const infoMs = 400;
    const servers = [
      fake(() => "silent"),
      fake(() => "late-serve", infoMs),
      fake(() => "silent"),
      fake(() => "silent"),
    ];
    // The row tests the floor only while a share without it misses the INFO and one with it does
    // not; a change of the floor or of these numbers that breaks that fails here, not silently.
    expect(boundMs / servers.length).toBeLessThan(infoMs);
    expect(infoMs).toBeLessThan(NATS_MIN_REDIAL_BOUND_MS);
    const once = { firstDelayMs: 0, maxDelayMs: 0, budgetMs: 0 };
    const shares: (number | undefined)[] = [];
    const dial = await timed(
      connectNats(
        (dialOptions) => {
          shares.push(dialOptions.timeout);
          return connect(dialOptions);
        },
        { ...options(servers.map((server) => server.url)), timeout: boundMs },
        once
      )
    );
    if (dial.value !== undefined) connections.push(dial.value);
    expect(shares).toEqual([NATS_MIN_REDIAL_BOUND_MS]);
    await eventually(() => servers[1]?.connections() === 1);
  }, 30_000);

  test("gives a connection that a short bound opened the whole timeout for its reconnects", async () => {
    const server = fake((n) => (n === 0 ? "serve" : "late-serve"));
    const once = { firstDelayMs: 0, maxDelayMs: 0, budgetMs: 0 };
    const nc = await connectNats(
      connect,
      { ...options([server.url]), reconnectTimeWait: 100, reconnectJitter: 0 },
      once,
      { firstBoundMs: LATE_INFO_MS / 2 }
    );
    connections.push(nc);
    const reconnects = countStatus(nc, Events.Reconnect);
    server.dropAll();
    // A reconnect timed at the dial's 300 ms would give up before the server's 600 ms INFO every
    // time, so it would never come, and only the row's own limit would end the wait.
    await eventually(() => reconnects.count() >= 1);
  }, 30_000);

  test("is whole: an error past the bound is the plugin's own, which a log prints whole", async () => {
    const once = { firstDelayMs: 0, maxDelayMs: 0, budgetMs: 0 };
    // A dial with no bound never settles, and only the row's own limit ends it.
    const never = Promise.withResolvers<NatsConnection>();
    const error = await connectNats(() => never.promise, { timeout: 50 }, once).catch((e) => e);
    expect(error).toBeInstanceOf(LoggableError);
  }, 30_000);
});
