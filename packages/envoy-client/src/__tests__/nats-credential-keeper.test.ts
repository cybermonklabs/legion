// packages/envoy-client/src/__tests__/nats-credential-keeper.test.ts
//
// A session's broker NATS credential against a stand-in connection whose status a row pushes:
// how `keepNatsCredential` renews the token ahead of its expiry and reconnects with it, how it
// warns of, counts and drops a live token the server refuses (once per session until the
// connection is back), and how `followNatsCredential` keeps within its bound. The credential's
// wire, `envoySettings`' resolution and the real nats-server rows are nats-credential.test.ts's.

import { afterEach, beforeEach, describe, expect, jest, spyOn, test } from "bun:test";
import { turn, turnsUntil } from "@legion/test-fixtures/fake-clock";
import type { Status } from "nats";
import { brokerCredential } from "../broker-credential";
import {
  type BrokerNatsCredential,
  brokerNatsCredential,
  followNatsCredential,
  keepNatsCredential,
} from "../nats-credential";
import { natsBrokerStandIns } from "./broker-stand-ins";

const { podEnvironment, stubBroker } = natsBrokerStandIns("nats-credential-keeper");

/** A connection as the keeper sees it: each reconnect is a new CONNECT, which asks the
 *  credential's authenticator again, as nats.js does, and its status is what a row pushes, read
 *  until the reader is stopped. `drained` says the reader has taken every status pushed and waits
 *  for the next: it reads each one to the end before it asks for another, so what it did of them
 *  is done. `closeQuietly` marks it closed without ending `closed()`: the moment between the two
 *  in nats.js. */
function fakeConnection(onConnect: () => void) {
  let closed = false;
  const ended = Promise.withResolvers<undefined>();
  const reconnects: number[] = [];
  const queued: Status[] = [];
  let wake: (() => void) | undefined;
  let readerStopped = false;
  let waiting = false;
  const statuses = {
    stop: () => {
      readerStopped = true;
      wake?.();
    },
    async *[Symbol.asyncIterator](): AsyncGenerator<Status> {
      for (;;) {
        const next = queued.shift();
        if (next !== undefined) {
          yield next;
          continue;
        }
        if (readerStopped) return;
        waiting = true;
        await new Promise<void>((resolve) => {
          wake = resolve;
        });
        waiting = false;
        wake = undefined;
      }
    },
  };
  return {
    reconnects,
    readerStopped: () => readerStopped,
    drained: () => queued.length === 0 && (waiting || readerStopped),
    /** Pushes `type` with `data` onto the status the keeper reads. */
    push: (type: string, data = "") => {
      queued.push({ type, data } as Status);
      wake?.();
    },
    connection: {
      reconnect: async () => {
        reconnects.push(Date.now());
        onConnect();
      },
      isClosed: () => closed,
      closed: () => ended.promise,
      status: () => statuses,
    },
    close: () => {
      closed = true;
      ended.resolve(undefined);
    },
    closeQuietly: () => {
      closed = true;
    },
  };
}

/** Turns until the keeper's status reader has done what it does with every status pushed. */
function readerAtRest(fake: { readonly drained: () => boolean }): Promise<void> {
  return turnsUntil(fake.drained, "the keeper's reader at rest");
}

/** `credential` with the time of each renewal the keeper asks of it kept in `asked`, and of each
 *  drop in `dropped`: what the keeper decides, apart from how long the trade and the reconnect
 *  after it take. Each is kept as the keeper calls it, so once a timer of the keeper's has run,
 *  what it asked is there. */
function askedOf(credential: BrokerNatsCredential): {
  readonly credential: BrokerNatsCredential;
  readonly asked: number[];
  readonly dropped: number[];
} {
  const asked: number[] = [];
  const dropped: number[] = [];
  return {
    asked,
    dropped,
    credential: {
      ...credential,
      renew: (signal, waitForReady) => {
        asked.push(Date.now());
        return credential.renew(signal, waitForReady);
      },
      drop: () => {
        dropped.push(Date.now());
        credential.drop();
      },
    },
  };
}

// These run on Bun's fake clock, which the keeper's timer, the token client's expiry, the stub
// broker's lapses and every `AbortSignal.timeout` keep time by alike (`Date.now` and timers both),
// while each trade is a real HTTP trade at the stub broker, which runs on turns of the event loop.
// A row moves the clock itself, so what it reads of when the keeper asked for a renewal
// (`askedOf`) is the keeper's own decision, to the millisecond; and a renewal that would come
// after the keeper was stopped shows, since moving the clock past it runs its timer.
describe("keepNatsCredential", () => {
  beforeEach(() => {
    jest.useFakeTimers();
  });
  afterEach(() => {
    jest.useRealTimers();
  });

  test("keepNatsCredential renews and reconnects before expiry", async () => {
    const broker = stubBroker(2);
    const { credential, asked } = askedOf(
      brokerNatsCredential(podEnvironment(broker.url), "ses_a")
    );
    await credential.ready();
    const first = credential.authenticator("nonce-1") as { auth_token: string };
    const lapses = credential.expiresAt().getTime();
    const fake = fakeConnection(() => credential.authenticator("nonce-again"));
    const warnings: string[] = [];
    const stop = keepNatsCredential(fake.connection, credential, (text) => warnings.push(text));
    try {
      // Asked for the next token at half the two-second life of the one the connection presented,
      // and not a millisecond before.
      jest.advanceTimersByTime(999);
      expect(asked).toEqual([]);
      jest.advanceTimersByTime(1);
      expect(asked).toEqual([lapses - 1_000]);
      await turnsUntil(() => fake.reconnects.length > 0, "reconnected with the renewed token");
      const next = credential.authenticator("nonce-3") as { auth_token: string };
      expect(next.auth_token).not.toBe(first.auth_token);
      expect(broker.state.minted.length).toBe(2);
      expect(warnings).toEqual([]);
    } finally {
      stop();
    }
    // Stopped, it asks for nothing at the next renewal's time nor a minute on, and its status
    // reader is stopped.
    expect(fake.readerStopped()).toBe(true);
    jest.advanceTimersByTime(60_000);
    await turn();
    expect(asked.length).toBe(1);
    expect(fake.reconnects.length).toBe(1);
  }, 15_000);

  test("a token that lives one second is renewed before it lapses", async () => {
    // The broker allows one-second tokens (BROKER_ACCESS_TOKEN_SECONDS=1): no floor on the wait
    // may push the renewal past the token's own expiry.
    const broker = stubBroker(1);
    const { credential, asked } = askedOf(
      brokerNatsCredential(podEnvironment(broker.url), "ses_a")
    );
    await credential.ready();
    credential.authenticator("nonce-1");
    const lapses = credential.expiresAt().getTime();
    const fake = fakeConnection(() => credential.authenticator("nonce-again"));
    const stop = keepNatsCredential(fake.connection, credential, () => {});
    try {
      jest.advanceTimersByTime(499);
      expect(asked).toEqual([]);
      jest.advanceTimersByTime(1);
      expect(asked).toEqual([lapses - 500]);
    } finally {
      stop();
    }
  }, 10_000);

  test("a renewal that lands no new token is warned about, never naming the token", async () => {
    // The token client answers the token in hand through a transient exchange failure, so the
    // connection keeps it, and the session is told its renewals are not landing.
    const broker = stubBroker(2);
    const credential = brokerNatsCredential(podEnvironment(broker.url), "ses_a");
    await credential.ready();
    credential.authenticator("nonce-1");
    broker.state.down = true;
    const fake = fakeConnection(() => credential.authenticator("nonce-again"));
    const warnings: string[] = [];
    const stop = keepNatsCredential(fake.connection, credential, (text) => warnings.push(text));
    try {
      // The renewal due at half the token's two-second life.
      jest.advanceTimersByTime(1_000);
      await turnsUntil(() => warnings.length > 0, "warned of the renewal");
      expect(warnings.join("\n")).toContain("landed no new token");
      for (const one of broker.state.minted) expect(warnings.join("\n")).not.toContain(one.value);
      expect(fake.reconnects).toEqual([]);
    } finally {
      stop();
    }
  }, 10_000);

  test("a closed connection ends the keeper, its status reader too", async () => {
    const broker = stubBroker(2);
    const { credential, asked } = askedOf(
      brokerNatsCredential(podEnvironment(broker.url), "ses_a")
    );
    await credential.ready();
    credential.authenticator("nonce-1");
    const fake = fakeConnection(() => credential.authenticator("nonce-again"));
    keepNatsCredential(fake.connection, credential, () => {});
    fake.close();
    // Stopped by `closed()` alone: nothing else stops the status reader, nor the renewal due at
    // one second.
    await turnsUntil(() => fake.readerStopped(), "the keeper's status reader stopped");
    jest.advanceTimersByTime(60_000);
    await turn();
    expect(asked).toEqual([]);
    expect(fake.reconnects).toEqual([]);
    expect(broker.state.minted.length).toBe(1);
  }, 15_000);

  test("a connection marked closed before closed() ends trades nothing at the renewal due", async () => {
    // nats.js marks a connection closed before its closed() promise settles: a renewal falling
    // due between the two would renew for, and reconnect, a connection that is gone.
    const broker = stubBroker(2);
    const { credential, asked } = askedOf(
      brokerNatsCredential(podEnvironment(broker.url), "ses_a")
    );
    await credential.ready();
    credential.authenticator("nonce-1");
    const fake = fakeConnection(() => credential.authenticator("nonce-again"));
    const stop = keepNatsCredential(fake.connection, credential, () => {});
    try {
      fake.closeQuietly();
      // Past the renewal due at half the token's two-second life, and long after.
      jest.advanceTimersByTime(60_000);
      await turn();
      expect(asked).toEqual([]);
      expect(broker.state.trades).toBe(1);
      expect(fake.reconnects).toEqual([]);
    } finally {
      stop();
    }
  }, 15_000);
});

// On the fake clock too: a refusal is read as the status reader takes it (`readerAtRest`), and
// the renewal a drop asks for runs on a timer of the keeper's set for now, which a row runs by
// moving the clock the one millisecond Bun's clock moves for it. Tokens live ten minutes, so no
// renewal falls due inside a row.
describe("keepNatsCredential with a refused live token", () => {
  beforeEach(() => {
    jest.useFakeTimers();
  });
  afterEach(() => {
    jest.useRealTimers();
  });

  test("two refusals are warned of once, drop the token here and in the token client, and trade another", async () => {
    // A token the broker revoked before it lapsed: the auth service refuses every CONNECT that
    // presents it, and nats.js redials on its backoff (`brokerRedialDelays`).
    const broker = stubBroker(600);
    const env = podEnvironment(broker.url);
    const { credential, asked, dropped } = askedOf(brokerNatsCredential(env, "ses_a"));
    await credential.ready();
    const first = (credential.authenticator("nonce-1") as { auth_token: string }).auth_token;
    const fake = fakeConnection(() => credential.authenticator("nonce-again"));
    const warnings: string[] = [];
    const stop = keepNatsCredential(fake.connection, credential, (text) => warnings.push(text));
    try {
      fake.push("error", "AUTHORIZATION_VIOLATION");
      await readerAtRest(fake);
      expect(warnings).toEqual([
        "the NATS server refused the connection's live broker token (AUTHORIZATION_VIOLATION): the auth service is away, or the token was revoked; the connection keeps redialling, and at the second refusal the token is dropped and another traded",
      ]);
      // One refusal can be the auth service's own outage: nothing is dropped, and nothing renewed.
      jest.advanceTimersByTime(1);
      expect(dropped).toEqual([]);
      expect(asked).toEqual([]);

      fake.push("error", "AUTHORIZATION_VIOLATION");
      await readerAtRest(fake);
      expect(dropped.length).toBe(1);
      jest.advanceTimersByTime(1);
      expect(asked.length).toBe(1);
      await turnsUntil(
        () => fake.reconnects.length > 0,
        "reconnected with the token traded after the drop"
      );
      expect(broker.state.trades).toBe(2);
      const next = (credential.authenticator("nonce-3") as { auth_token: string }).auth_token;
      expect(next).not.toBe(first);
      // The token client dropped it too, so the session's HTTP calls present the new token.
      const headers = await brokerCredential(env, "ses_a").headers("GET", "http://listener.test/");
      expect(headers["Authorization"]).toBe(`DPoP ${next}`);
      // Rate-limited: still the one warning, and it names no token.
      expect(warnings.length).toBe(1);
      for (const one of broker.state.minted) expect(warnings.join("\n")).not.toContain(one.value);

      // Refused again before the connection is back: the new token does not cure an auth service
      // that is away, and this session was dropped for this outage, so nothing more is dropped,
      // and no renewal is asked for.
      fake.push("error", "AUTHORIZATION_VIOLATION");
      fake.push("error", "AUTHORIZATION_VIOLATION");
      await readerAtRest(fake);
      jest.advanceTimersByTime(1);
      expect(dropped.length).toBe(1);
      expect(asked.length).toBe(1);

      // Back, and then refused twice: a new outage, warned of and cured as the first was.
      fake.push("reconnect");
      fake.push("error", "AUTHENTICATION_EXPIRED");
      fake.push("error", "AUTHENTICATION_EXPIRED");
      await readerAtRest(fake);
      expect(dropped.length).toBe(2);
      jest.advanceTimersByTime(1);
      await turnsUntil(() => broker.state.trades === 3, "traded after the second outage's drop", {
        state: () => `${broker.state.trades} trades`,
      });
      expect(warnings.length).toBe(2);
      expect(warnings[1]).toContain("(AUTHENTICATION_EXPIRED)");
    } finally {
      stop();
    }
  }, 20_000);

  test("the warning repeated after a drop says no token is dropped again until the connection is back", async () => {
    // The deep review of dfa2ea1f59, m1: the repeat, 30 minutes on, still promised a drop the
    // per-session limit no longer makes.
    let generation = 0;
    let expiresAt = Date.now() + 10 * 3_600_000;
    const drops: number[] = [];
    const credential: BrokerNatsCredential = {
      ready: async () => {},
      renew: async () => {
        generation += 1;
        expiresAt = Date.now() + 10 * 3_600_000 + generation;
      },
      authenticator: () => undefined,
      expiresAt: () => new Date(expiresAt),
      needsReconnect: () => false,
      drop: () => {
        drops.push(Date.now());
      },
      tokenGeneration: () => generation,
      tokenSession: () => "ses_stand_in",
    };
    const fake = fakeConnection(() => {});
    const warnings: string[] = [];
    const stop = keepNatsCredential(fake.connection, credential, (text) => warnings.push(text));
    try {
      fake.push("error", "AUTHORIZATION_VIOLATION");
      fake.push("error", "AUTHORIZATION_VIOLATION");
      await readerAtRest(fake);
      jest.advanceTimersByTime(1);
      await turn();
      expect(drops.length).toBe(1);
      expect(warnings).toHaveLength(1);
      expect(warnings[0]).toEndWith(
        "and at the second refusal the token is dropped and another traded"
      );
      // 30 minutes on, the outage lasting, the warning is said again, of what it does now.
      jest.advanceTimersByTime(30 * 60_000);
      fake.push("error", "AUTHORIZATION_VIOLATION");
      fake.push("error", "AUTHORIZATION_VIOLATION");
      await readerAtRest(fake);
      expect(drops.length).toBe(1);
      expect(warnings).toHaveLength(2);
      expect(warnings[1]).toBe(
        "the NATS server refused the connection's live broker token (AUTHORIZATION_VIOLATION): the auth service is away, or the token was revoked; the connection keeps redialling, and this session's token was dropped and another traded once already, so none is dropped again until the connection is back"
      );
    } finally {
      stop();
    }
  }, 10_000);

  test("a refusal of a lapsed token, of one the connection is moving off, or of another kind drops nothing", async () => {
    // A stand-in credential, so each condition is set alone.
    let expiresAt = Date.now() + 600_000;
    let moving = false;
    const drops: number[] = [];
    const renewals: number[] = [];
    const credential: BrokerNatsCredential = {
      ready: async () => {},
      renew: async () => {
        renewals.push(Date.now());
      },
      authenticator: () => undefined,
      expiresAt: () => new Date(expiresAt),
      needsReconnect: () => moving,
      drop: () => {
        drops.push(Date.now());
      },
      tokenGeneration: () => drops.length,
      tokenSession: () => "ses_stand_in",
    };
    const fake = fakeConnection(() => {});
    const warnings: string[] = [];
    const stop = keepNatsCredential(fake.connection, credential, (text) => warnings.push(text));
    try {
      // Not a refusal of the credential: a subscribe the server denied, an error with no code. Each
      // condition is set once the reader has taken the statuses before it.
      fake.push("error", "PERMISSIONS_VIOLATION");
      fake.push("error", "PERMISSIONS_VIOLATION");
      fake.push("error", "");
      fake.push("disconnect", "AUTHORIZATION_VIOLATION");
      await readerAtRest(fake);
      // The connection is about to present the token in hand, not the one refused.
      moving = true;
      fake.push("error", "AUTHORIZATION_VIOLATION");
      fake.push("error", "AUTHORIZATION_VIOLATION");
      await readerAtRest(fake);
      // A token that lapsed is the renewal's to replace.
      moving = false;
      expiresAt = Date.now() - 1;
      fake.push("error", "AUTHENTICATION_EXPIRED");
      fake.push("error", "AUTHENTICATION_EXPIRED");
      await readerAtRest(fake);
      expect(drops).toEqual([]);
      expect(warnings).toEqual([]);
      // The control: the same two refusals of a live token in hand drop it, and the renewal the
      // drop asks for runs.
      expiresAt = Date.now() + 600_000;
      fake.push("error", "AUTHORIZATION_VIOLATION");
      fake.push("error", "AUTHORIZATION_VIOLATION");
      await readerAtRest(fake);
      expect(drops.length).toBe(1);
      jest.advanceTimersByTime(1);
      expect(renewals.length).toBe(1);
    } finally {
      stop();
    }
  }, 10_000);

  /** A stand-in credential whose renewals the row lands by hand, each with a token that lives
   *  ten minutes; `expiresAt` is when the token in hand lapses. */
  function heldRenewals(expiresAt: number) {
    const state = { expiresAt, renewals: 0, drops: 0, generation: 0 };
    const pending: (() => void)[] = [];
    const credential: BrokerNatsCredential = {
      ready: async () => {},
      renew: () => {
        state.renewals += 1;
        const landed = Promise.withResolvers<void>();
        pending.push(() => {
          state.expiresAt = Date.now() + 600_000;
          state.generation += 1;
          landed.resolve();
        });
        return landed.promise;
      },
      authenticator: () => undefined,
      expiresAt: () => new Date(state.expiresAt),
      needsReconnect: () => false,
      drop: () => {
        state.drops += 1;
      },
      tokenGeneration: () => state.generation,
      tokenSession: () => "ses_stand_in",
    };
    return { state, credential, land: () => pending.shift()?.() };
  }

  test("a drop while a renewal is in flight renews again the moment that one lands", async () => {
    const { state, credential, land } = heldRenewals(Date.now() + 600_000);
    const fake = fakeConnection(() => {});
    const stop = keepNatsCredential(fake.connection, credential, () => {});
    try {
      fake.push("error", "AUTHORIZATION_VIOLATION");
      fake.push("error", "AUTHORIZATION_VIOLATION");
      await readerAtRest(fake);
      jest.advanceTimersByTime(1);
      expect(state.renewals).toBe(1);
      // Back, and refused twice again while that renewal is still in flight.
      fake.push("reconnect");
      fake.push("error", "AUTHORIZATION_VIOLATION");
      fake.push("error", "AUTHORIZATION_VIOLATION");
      await readerAtRest(fake);
      expect(state.drops).toBe(2);
      jest.advanceTimersByTime(1);
      expect(state.renewals).toBe(1);
      // It lands a token that lives ten minutes, so only the drop's ask renews again now, at
      // once: what the landing sets off settles within a turn, and the renewal it asks for runs
      // on the keeper's timer for now.
      land();
      await turn();
      jest.advanceTimersByTime(1);
      expect(state.renewals).toBe(2);
    } finally {
      land();
      stop();
    }
  }, 10_000);

  test("a drop's renewal replaces the renewal that was due, rather than adding one", async () => {
    // The token in hand lapses in 2 s, so a renewal is due at 1 s; the drop renews at once and
    // lands a token that lives ten minutes, after which nothing is due for minutes.
    const { state, credential, land } = heldRenewals(Date.now() + 2_000);
    const fake = fakeConnection(() => {});
    const stop = keepNatsCredential(fake.connection, credential, () => {});
    try {
      fake.push("error", "AUTHORIZATION_VIOLATION");
      fake.push("error", "AUTHORIZATION_VIOLATION");
      await readerAtRest(fake);
      jest.advanceTimersByTime(1);
      expect(state.renewals).toBe(1);
      land();
      await turn();
      // Past the renewal that was due at 1 s.
      jest.advanceTimersByTime(1_500);
      expect(state.renewals).toBe(1);
    } finally {
      land();
      stop();
    }
  }, 10_000);

  test("a token dropped while the broker is away is presented no more", async () => {
    // The renewal after the drop fails, so the connection holds no token: each CONNECT presents
    // nothing and trades for one, rather than present the token the server refused.
    const broker = stubBroker(600);
    const credential = brokerNatsCredential(podEnvironment(broker.url), "ses_a");
    await credential.ready();
    credential.authenticator("nonce-1");
    const fake = fakeConnection(() => {});
    const stop = keepNatsCredential(fake.connection, credential, () => {});
    try {
      broker.state.down = true;
      fake.push("error", "AUTHORIZATION_VIOLATION");
      fake.push("error", "AUTHORIZATION_VIOLATION");
      await readerAtRest(fake);
      jest.advanceTimersByTime(1);
      await turnsUntil(() => broker.state.trades > 1, "traded after the drop");
      expect(credential.authenticator("nonce-2")).toBeUndefined();
      expect(credential.expiresAt().getTime()).toBe(0);
    } finally {
      stop();
    }
  }, 15_000);

  // The review of c19c0722bf, R1: a count kept across tokens, and a drop allowed once until the
  // connection was back whatever its session, left a revoked token of a session switched to after
  // an earlier drop presented until its renewal, and let one refusal each of two tokens drop the
  // second.
  test("a token traded after a drop, a switch's included, is dropped at its own second refusal", async () => {
    const broker = stubBroker(600);
    let session = "ses_a";
    const env = podEnvironment(broker.url);
    const { credential, dropped } = askedOf(brokerNatsCredential(env, () => session));
    await credential.ready();
    credential.authenticator("nonce-1");
    const fake = fakeConnection(() => credential.authenticator("nonce-again"));
    const stop = keepNatsCredential(fake.connection, credential, () => {});
    try {
      // Session A's token is revoked: dropped at its second refusal, with no reconnect after.
      fake.push("error", "AUTHORIZATION_VIOLATION");
      fake.push("error", "AUTHORIZATION_VIOLATION");
      await readerAtRest(fake);
      expect(dropped.length).toBe(1);
      jest.advanceTimersByTime(1);
      await turnsUntil(() => broker.state.trades === 2, "traded after A's drop");
      // The host moves to session B, whose token is revoked too, before the connection is back.
      session = "ses_b";
      await credential.renew(undefined, true);
      expect(broker.state.minted.at(-1)?.session).toBe("ses_b");
      credential.authenticator("nonce-b");
      fake.push("error", "AUTHORIZATION_VIOLATION");
      await readerAtRest(fake);
      // B's first refusal is not B's second, whatever A's count was.
      expect(dropped.length).toBe(1);
      fake.push("error", "AUTHORIZATION_VIOLATION");
      await readerAtRest(fake);
      expect(dropped.length).toBe(2);
      jest.advanceTimersByTime(1);
      await turnsUntil(() => broker.state.trades === 4, "traded after B's drop", {
        state: () => `${broker.state.trades} trades`,
      });
    } finally {
      stop();
    }
  }, 15_000);

  test("one refusal of a token and one of the token after it drop neither", async () => {
    const { state, credential, land } = heldRenewals(Date.now() + 600_000);
    const fake = fakeConnection(() => {});
    const stop = keepNatsCredential(fake.connection, credential, () => {});
    try {
      fake.push("error", "AUTHORIZATION_VIOLATION");
      await readerAtRest(fake);
      // A renewal lands another token, which the server refuses once.
      void credential.renew();
      land();
      await turn();
      fake.push("error", "AUTHORIZATION_VIOLATION");
      await readerAtRest(fake);
      expect(state.drops).toBe(0);
      // The control: that token's own second refusal drops it.
      fake.push("error", "AUTHORIZATION_VIOLATION");
      await readerAtRest(fake);
      expect(state.drops).toBe(1);
    } finally {
      land();
      stop();
    }
  }, 10_000);

  test("an auth service that refuses every token costs each session one trade for the outage", async () => {
    // The hub's auth service is away and the broker is up: every CONNECT is refused whatever its
    // token, and nats.js redials, here every 2 s, the shortest wait of its backoff
    // (`brokerRedialDelays`). 20 refusals, 40 s of it: one drop and one trade, not one every second
    // refusal.
    const broker = stubBroker(600);
    const env = podEnvironment(broker.url);
    const { credential, dropped } = askedOf(brokerNatsCredential(env, "ses_storm"));
    await credential.ready();
    credential.authenticator("nonce-1");
    const fake = fakeConnection(() => credential.authenticator("nonce-again"));
    const stop = keepNatsCredential(fake.connection, credential, () => {});
    try {
      for (let refusal = 0; refusal < 20; refusal += 1) {
        fake.push("error", "AUTHORIZATION_VIOLATION");
        await readerAtRest(fake);
        // The renewal a drop asks for runs at once on the keeper's timer, and its trade lands.
        jest.advanceTimersByTime(1);
        await turnsUntil(() => credential.expiresAt().getTime() > Date.now(), "a token in hand", {
          state: () => `${broker.state.trades} trades`,
        });
        jest.advanceTimersByTime(2_000 - 1);
      }
      expect(dropped.length).toBe(1);
      expect(broker.state.trades).toBe(2);
      // The control: back, and refused twice again, is a new outage, with its one drop.
      fake.push("reconnect");
      fake.push("error", "AUTHORIZATION_VIOLATION");
      fake.push("error", "AUTHORIZATION_VIOLATION");
      await readerAtRest(fake);
      expect(dropped.length).toBe(2);
    } finally {
      stop();
    }
  }, 30_000);
});

// The review of 2fa6fbf7cf, m3: a session switch's follow inside a pass whose deadline is shorter
// than the default is bounded by that pass (`followNatsCredential`'s `boundMs`); at the default it
// is the 20 s trade and the 8 s reconnect it always was (nats-credential-deadlines.test.ts). On the
// fake clock, which the follow's bounds keep time by: once only its timers are left, a row moves
// the clock to a millisecond before the bound and then onto it, so the bound is read as the
// follow's own, however loaded the machine.
describe("followNatsCredential within a bound", () => {
  beforeEach(() => {
    jest.useFakeTimers();
  });
  afterEach(() => {
    jest.useRealTimers();
  });

  /** The follow's outcome once it ends: the error it rejects with, or "followed". */
  function ending(following: Promise<void>): { readonly outcome: () => unknown } {
    let outcome: unknown;
    following.then(
      () => {
        outcome = "followed";
      },
      (error: unknown) => {
        outcome = error;
      }
    );
    return { outcome: () => outcome };
  }

  test("a new session's token that never comes fails at the trade's share of the bound", async () => {
    const broker = stubBroker(600);
    let session = "ses_from";
    const credential = brokerNatsCredential(podEnvironment(broker.url), () => session);
    await credential.ready();
    credential.authenticator("nonce-1");
    broker.state.down = true;
    session = "ses_to";
    const fake = fakeConnection(() => credential.authenticator("nonce-again"));
    // The token client says what a trade came to at the wait after it.
    let refused = false;
    const warn = spyOn(console, "warn").mockImplementation((line: unknown) => {
      if (String(line).includes("trading again")) refused = true;
    });
    try {
      // 4 s: the connection's share is half of it, so the trade waits 2 s, then names what the
      // last trade came to.
      const follow = ending(followNatsCredential(fake.connection, credential, 4_000));
      // The new session's first trade answered 503: from here only the follow's timers are left,
      // and the token client's next trade, 15 s on.
      await turnsUntil(() => refused, "the new session's first trade answered");
      jest.advanceTimersByTime(1_999);
      await turn();
      expect(follow.outcome()).toBeUndefined();
      jest.advanceTimersByTime(1);
      await turnsUntil(() => follow.outcome() !== undefined, "the follow's end at 2 s");
      expect(String(follow.outcome())).toContain(
        "the NATS connection could not take the new session's token: "
      );
      expect(String(follow.outcome())).toContain("HTTP 503");
      expect(fake.reconnects).toEqual([]);
    } finally {
      warn.mockRestore();
    }
  }, 15_000);

  test("a connection that never comes back fails at what is left of the bound", async () => {
    const broker = stubBroker(600);
    let session = "ses_from";
    const credential = brokerNatsCredential(podEnvironment(broker.url), () => session);
    await credential.ready();
    credential.authenticator("nonce-1");
    session = "ses_to";
    // The connection is asked to reconnect, and no status ever says it is back.
    const fake = fakeConnection(() => {});
    const follow = ending(followNatsCredential(fake.connection, credential, 4_000));
    // The token landed with the clock where the follow started it, so all 4 s of the bound are
    // left for the connection, which the follow asks to reconnect.
    await turnsUntil(() => fake.reconnects.length === 1, "asked to reconnect");
    jest.advanceTimersByTime(3_999);
    await turn();
    expect(follow.outcome()).toBeUndefined();
    expect(fake.readerStopped()).toBe(false);
    jest.advanceTimersByTime(1);
    await turnsUntil(() => follow.outcome() !== undefined, "the follow's end at 4 s");
    expect(String(follow.outcome())).toContain(
      "the NATS connection did not come back as the new session within 4 s"
    );
    expect(fake.reconnects.length).toBe(1);
    expect(fake.readerStopped()).toBe(true);
  }, 15_000);
});
