// packages/envoy-client/src/__tests__/nats-credential-deadlines.test.ts
//
// How long a session's broker NATS credential waits, and on what: `ready` fails at its deadline,
// named, for a broker that never answers and for a pod session with no registration yet; `renew`
// takes the new session's token across a switch; a CONNECT with no live token trades on its own
// and keeps the broker's back-off while the registration is missing; and `followNatsCredential`
// moves a connection to the new session's token after a switch, ending at its own deadline when
// the connection closes, never comes back, or the new session has no registration yet.
//
// The rows that read a wait's length or a schedule's slot run on Bun's fake clock, which the
// token client's waits and every `AbortSignal.timeout` keep time by, while the broker answers
// over a real socket. Such a row moves the clock only once nothing but a timer is left to happen,
// so what it reads is the code's own time, however loaded the machine.

import { afterEach, describe, expect, jest, spyOn, test } from "bun:test";
import { turn, turnsUntil } from "@legion/test-fixtures/fake-clock";
import {
  type BrokerNatsCredential,
  brokerNatsCredential,
  followNatsCredential,
  TRADE_DEADLINE_MS,
} from "../nats-credential";
import { natsBrokerStandIns } from "./broker-stand-ins";

const { podEnvironment, servers, stubBroker } = natsBrokerStandIns("nats-credential-deadlines");

/** The bound of the row whose authenticator trades on its own, passed to it: the package's
 *  `--timeout 30000`, written so its waits can give up inside it. */
const SELF_TRADE_ROW_MS = 30_000;

// A fake clock left behind stops the timers of every file the same worker runs after this one.
afterEach(() => {
  jest.useRealTimers();
});

/** The bound, in real time, of each wait of the `ready` rows below on real I/O or the code's
 *  reaction to it: a hang guard, the fake-clock fixture's own 30 s, never a measure of the code.
 *  The rows read the deadline itself on the fake clock. A row's bound is its two waits and 5 s. */
const READY_WAIT_MS = 30_000;
const READY_ROW_MS = 2 * READY_WAIT_MS + 5_000;

describe("ready", () => {
  /** Starts `ready` and returns what it has come to so far: undefined until it settles, then
   *  "ready" or the message it failed with. */
  function readying(
    credential: BrokerNatsCredential,
    options?: Parameters<BrokerNatsCredential["ready"]>[0]
  ): () => string | undefined {
    let outcome: string | undefined;
    void credential.ready(options).then(
      () => {
        outcome = "ready";
      },
      (error: unknown) => {
        outcome = error instanceof Error ? error.message : String(error);
      }
    );
    return () => outcome;
  }

  /** Moves the fake clock, which has not moved since `ready` started, to 1 ms short of
   *  `deadlineMs`, where `ready` must not have settled, and then to it, where it must fail: so
   *  the failure is the deadline's, and comes when its timer fires, read on the code's own clock
   *  however loaded the machine. Real time bounds only the wait for the failure, to name a hang:
   *  a deadline that never fires. */
  async function atDeadline(
    outcome: () => string | undefined,
    deadlineMs: number
  ): Promise<string | undefined> {
    jest.advanceTimersByTime(deadlineMs - 1);
    await turn();
    expect(outcome()).toBeUndefined();
    jest.advanceTimersByTime(1);
    await turnsUntil(
      () => outcome() !== undefined,
      `ready's failure at its ${deadlineMs} ms deadline`,
      { boundMs: READY_WAIT_MS }
    );
    return outcome();
  }

  /** A broker that takes the trade's request and never answers it; `asked` once it has. */
  function silentBroker(): { readonly url: string; asked(): boolean } {
    let asked = false;
    const server = Bun.serve({
      port: 0,
      hostname: "127.0.0.1",
      fetch: () => {
        asked = true;
        return new Promise<Response>(() => undefined);
      },
    });
    servers.push(server);
    return { url: `http://127.0.0.1:${server.port}`, asked: () => asked };
  }

  test(
    "a broker that never answers the trade fails it, named, at the deadline",
    async () => {
      // A broker that takes the request and never answers: with no deadline a start waited on it
      // in silence (64 s and counting, at 579d9b45f6), where a hub that never answers fails at
      // nats.js's own dial timeout, naming it.
      const broker = silentBroker();
      const credential = brokerNatsCredential(
        podEnvironment(broker.url),
        "ses_hung",
        "NATS_AUTH",
        300
      );
      jest.useFakeTimers();
      const outcome = readying(credential);
      await turnsUntil(broker.asked, "the broker taking the trade", { boundMs: READY_WAIT_MS });
      expect(await atDeadline(outcome, 300)).toBe(
        "NATS_AUTH is broker, but the secrets broker did not answer the trade for the session's token within 0.3 s"
      );
    },
    READY_ROW_MS
  );

  test(
    "a startup's bound shorter than the trade deadline ends its wait first, naming the broker's silence",
    async () => {
      // A start whose pass deadline is shorter than the default bounds its wait by that deadline
      // less what the rest of its pass needs (`ReadyOptions.boundMs`), so its pass is not
      // abandoned with no cause named.
      const broker = silentBroker();
      const credential = brokerNatsCredential(
        podEnvironment(broker.url),
        "ses_short_pass",
        "NATS_AUTH",
        10_000
      );
      jest.useFakeTimers();
      const outcome = readying(credential, { waitForReady: true, boundMs: 400 });
      await turnsUntil(broker.asked, "the broker taking the trade", { boundMs: READY_WAIT_MS });
      expect(await atDeadline(outcome, 400)).toBe(
        "NATS_AUTH is broker, but the secrets broker did not answer the trade for the session's token within 0.4 s"
      );
    },
    READY_ROW_MS
  );

  test(
    "a pod session the broker holds no registration for fails it at the deadline, naming the session",
    async () => {
      // The token client waits out a missing registration for as long as its caller waits
      // (#4153), so a start that handed it no deadline waited until the registration landed, and
      // then said the broker had not answered, which it had.
      const broker = stubBroker(600);
      broker.state.unregistered = true;
      const credential = brokerNatsCredential(
        podEnvironment(broker.url),
        "ses_pending",
        "NATS_AUTH",
        300
      );
      jest.useFakeTimers();
      let refused = false;
      const warn = spyOn(console, "warn").mockImplementation((line: unknown) => {
        if (String(line).includes("session ses_pending yet")) refused = true;
      });
      try {
        const outcome = readying(credential);
        // The broker's refusal is in before the deadline, so the failure names it.
        await turnsUntil(() => refused, "the broker refusing the trade", {
          boundMs: READY_WAIT_MS,
        });
        expect(await atDeadline(outcome, 300)).toBe(
          "NATS_AUTH is broker, and the session's token waits on its registration: the secrets broker has no registration for this pod's session ses_pending yet, and the trade is being retried in the background until it lands (pod_session_not_registered: No registration says this pod may write under the session name 'ses_pending'. A pod writes under a session name its deployment registered for it.)"
        );
      } finally {
        warn.mockRestore();
      }
    },
    READY_ROW_MS
  );
});

describe("renew", () => {
  test("a switch while a renewal is in flight takes the new session's token", async () => {
    // A trade reads the session's name as it starts, so the keeper's renewal of the session
    // before can be in flight when the host switches. The switch's renewal must not be handed
    // that one: the connection would come back as the session before, and the new session's
    // subscribes be refused. The switch ends the wait for the session before, which a missing
    // registration could hold for minutes. Two-second tokens, so each renewal is a real trade.
    const broker = stubBroker(2);
    let session = "ses_before";
    const credential = brokerNatsCredential(podEnvironment(broker.url), () => session);
    await credential.ready();
    credential.authenticator("nonce-1");
    const keeperRenewal = credential.renew().then(
      () => "renewed",
      (error: unknown) => (error instanceof Error ? error.message : String(error))
    );
    session = "ses_after";
    await credential.renew();
    expect(await keeperRenewal).toBe(
      "the host moved from the session ses_before to ses_after while ses_before's token was traded"
    );
    expect(credential.needsReconnect()).toBe(true);
    const auth = credential.authenticator("nonce-2") as { auth_token: string };
    const presented = broker.state.minted.find((one) => one.value === auth.auth_token);
    expect(presented?.session).toBe("ses_after");
  });

  test("CONNECTs with no live token while the registration is missing keep the broker's back-off", async () => {
    // The token client's retry loop trades for a pod session with no registration on its own
    // schedule, 2 s doubling to 30 s, each wait at least half its length, and no caller cuts a
    // wait short. nats.js asks the authenticator at every attempt, for a connection the server
    // refuses at once after a refusal when a token waits to be presented, else as often as every
    // 2 s (`brokerRedialDelays`), so an authenticator that traded at each one would trade at that
    // pace. Inside the schedule's first slot, a CONNECT every 50 ms makes no trade of its own.
    //
    // On the fake clock the slot lasts as long as the row: no trade of the loop's own follows the
    // first, so every trade past it is one a CONNECT made. A round trip to the broker after each
    // CONNECT gives a trade one of them started the time to reach it.
    jest.useFakeTimers();
    const refused = Promise.withResolvers<void>();
    const warn = spyOn(console, "warn").mockImplementation((line: unknown) => {
      if (String(line).includes("session ses_unregistered yet")) refused.resolve();
    });
    try {
      const broker = stubBroker(600);
      broker.state.unregistered = true;
      const credential = brokerNatsCredential(podEnvironment(broker.url), "ses_unregistered");
      expect(credential.authenticator("nonce")).toBeUndefined();
      // The first trade is refused, and the loop waits out its first slot, at least 1 s.
      await refused.promise;
      for (let connect = 2; connect <= 18; connect++) {
        jest.advanceTimersByTime(50);
        expect(credential.authenticator("nonce")).toBeUndefined();
        await fetch(`${broker.url}/v1/oauth/introspect`, {
          method: "POST",
          body: new URLSearchParams({ token: "none" }),
        });
      }
      expect(broker.state.trades).toBe(1);
    } finally {
      warn.mockRestore();
    }
  }, 15_000);

  test(
    "a CONNECT with no live token presents nothing, trades, and presents the token next",
    async () => {
      // No ready(), renew() or keeper: the authenticator's own trade is what the next reconnect
      // presents, so a connection whose token lapsed comes back on its own. The trade is a real HTTP
      // trade at the stub broker, so its waits are on the facts, bounded only by the row's own bound
      // less 5 s, so that one that gives up names what it waited for.
      const deadline = performance.now() + SELF_TRADE_ROW_MS - 5_000;
      const until = async (holds: () => boolean, what: string) => {
        while (!holds()) {
          if (performance.now() > deadline) {
            throw new Error(
              `timed out waiting for ${what}, ${SELF_TRADE_ROW_MS / 1000 - 5} s into the row`
            );
          }
          await Bun.sleep(20);
        }
      };
      const broker = stubBroker(600);
      const credential = brokerNatsCredential(podEnvironment(broker.url), "ses_self");
      expect(credential.authenticator("nonce-1")).toBeUndefined();
      await until(
        () => broker.state.minted.length > 0,
        "the stub broker to mint the authenticator's trade"
      );
      // The trade has landed once the authenticator presents its token.
      let auth: { auth_token?: string } | undefined;
      await until(() => {
        auth = credential.authenticator("nonce-2") as { auth_token?: string } | undefined;
        return auth !== undefined;
      }, "the authenticator to present the traded token");
      expect(auth?.auth_token).toBe(broker.state.minted[0]?.value ?? "no token minted");
      expect(broker.state.minted.map((one) => one.session)).toEqual(["ses_self"]);
    },
    SELF_TRADE_ROW_MS
  );
});

describe("followNatsCredential", () => {
  /** A connection whose reconnect asks the authenticator again and reports the statuses the
   *  caller pushes, and which the caller closes. */
  function controlledConnection(credential: BrokerNatsCredential) {
    const pushed: { type: string }[] = [];
    let wake = () => {};
    let stopped = false;
    const ended = Promise.withResolvers<void>();
    const statuses = {
      stop: () => {
        stopped = true;
        wake();
      },
      async *[Symbol.asyncIterator]() {
        while (!stopped) {
          const next = pushed.shift();
          if (next !== undefined) {
            yield next;
            continue;
          }
          await new Promise<void>((resolve) => {
            wake = resolve;
          });
        }
      },
    };
    return {
      push: (type: string) => {
        pushed.push({ type });
        wake();
      },
      close: () => ended.resolve(),
      stoppedReader: () => stopped,
      connection: {
        reconnect: async () => {
          credential.authenticator("nonce-again");
        },
        status: () => statuses,
        closed: () => ended.promise,
      } as unknown as Parameters<typeof followNatsCredential>[0],
    };
  }

  async function switched(): Promise<BrokerNatsCredential> {
    const broker = stubBroker(600);
    let session = "ses_from";
    const credential = brokerNatsCredential(podEnvironment(broker.url), () => session);
    await credential.ready();
    credential.authenticator("nonce-1");
    session = "ses_to";
    return credential;
  }

  test("a connection that closes before it is back rejects at once, and stops its reader", async () => {
    const credential = await switched();
    const rig = controlledConnection(credential);
    const following = followNatsCredential(rig.connection, credential);
    // A status that is not the reconnect ends nothing.
    rig.push("disconnect");
    await Bun.sleep(50);
    rig.close();
    await expect(following).rejects.toThrow(
      "the NATS connection closed before it came back as the new session"
    );
    expect(rig.stoppedReader()).toBe(true);
  });

  test("a connection that never comes back rejects 8 s after the token lands, and stops its reader", async () => {
    const credential = await switched();
    const rig = controlledConnection(credential);
    // The follow's 8 s (`FOLLOW_RECONNECT_MS`) run from when it asks the connection to come back
    // with the new session's token, which lands with the fake clock where the follow started it.
    jest.useFakeTimers();
    let reconnecting = false;
    (rig.connection as { reconnect: () => Promise<void> }).reconnect = async () => {
      reconnecting = true;
      credential.authenticator("nonce-again");
    };
    let outcome: unknown;
    void followNatsCredential(rig.connection, credential).then(
      () => {
        outcome = "followed";
      },
      (error: unknown) => {
        outcome = error;
      }
    );
    await turnsUntil(() => reconnecting, "asked to reconnect with the new session's token");
    jest.advanceTimersByTime(7_999);
    await turn();
    expect(outcome).toBeUndefined();
    expect(rig.stoppedReader()).toBe(false);
    jest.advanceTimersByTime(1);
    await turnsUntil(() => outcome !== undefined, "the follow's end at 8 s");
    expect(String(outcome)).toContain(
      "the NATS connection did not come back as the new session within 8 s"
    );
    expect(rig.stoppedReader()).toBe(true);
  }, 30_000);

  test("a switch to a session with no registration yet stops waiting for its token at 20 s, naming why", async () => {
    // The case the token client's wait exists for: a pod's host moves to a session whose
    // registration has not landed. The follow must end at its own bound, naming why, so the pass
    // behind it (a registration, a role claim) runs again rather than stalling.
    const broker = stubBroker(600);
    let session = "ses_from";
    const credential = brokerNatsCredential(podEnvironment(broker.url), () => session);
    await credential.ready();
    credential.authenticator("nonce-1");
    broker.state.unregistered = true;
    session = "ses_to";
    const rig = controlledConnection(credential);
    // The follow's bound is the clock's: once the first trade for ses_to is refused, only timers
    // are left to happen, and the follow ends at its bound to the millisecond.
    jest.useFakeTimers();
    const refused = Promise.withResolvers<void>();
    const warn = spyOn(console, "warn").mockImplementation((line: unknown) => {
      if (String(line).includes("session ses_to yet")) refused.resolve();
    });
    try {
      let ended: string | undefined;
      void followNatsCredential(rig.connection, credential).then(
        () => {
          ended = "followed";
        },
        (error: unknown) => {
          ended = error instanceof Error ? error.message : String(error);
        }
      );
      await refused.promise;
      jest.advanceTimersByTime(TRADE_DEADLINE_MS - 1);
      await turn();
      expect(ended).toBeUndefined();
      jest.advanceTimersByTime(1);
      await turn();
      expect(ended ?? "the follow had not ended").toContain(
        "the secrets broker has no registration for this pod's session ses_to yet"
      );
    } finally {
      warn.mockRestore();
    }
  }, 40_000);

  test("a switch whose first trade answers 503 waits for ready and takes the token the next trade grants", async () => {
    // A switch has nothing to do until it holds the new session's token, so its trade waits for
    // ready within its bound: the retry loop's next trade, 15 s at most later, grants, and the
    // follow goes on to its reconnect. Failing fast, the switch would end at the 503 and the new
    // session would wait for the next heartbeat.
    const broker = stubBroker(600);
    let session = "ses_from";
    const credential = brokerNatsCredential(podEnvironment(broker.url), () => session);
    await credential.ready();
    credential.authenticator("nonce-1");
    broker.state.down = true;
    session = "ses_to";
    const rig = controlledConnection(credential);
    const before = broker.state.trades;
    const following = followNatsCredential(rig.connection, credential);
    while (broker.state.trades === before) await Bun.sleep(20);
    // The first trade for the new session was answered 503; the broker is back for the next.
    broker.state.down = false;
    while (broker.state.minted.length < 2) await Bun.sleep(50);
    await Bun.sleep(50);
    rig.push("reconnect");
    await following;
    expect(credential.needsReconnect()).toBe(false);
    expect(broker.state.minted.at(-1)?.session).toBe("ses_to");
  }, 40_000);

  test("a reconnect returns once its CONNECT presented the new session's token", async () => {
    const credential = await switched();
    const rig = controlledConnection(credential);
    const following = followNatsCredential(rig.connection, credential);
    await Bun.sleep(50);
    rig.push("reconnect");
    await following;
    expect(credential.needsReconnect()).toBe(false);
    expect(rig.stoppedReader()).toBe(true);
  });

  test("a redial already in flight with the old token is followed by another reconnect", async () => {
    // The connection is redialling when the switch comes: nats.js's reconnect() does nothing
    // while it is not connected, and the redial's CONNECT presented the old token, so its
    // reconnect leaves the move undone. The follow asks again, and the next CONNECT presents the
    // new session's token.
    const credential = await switched();
    const rig = controlledConnection(credential);
    const asked: string[] = [];
    const redialling = Promise.withResolvers<void>();
    (rig.connection as { reconnect: () => Promise<void> }).reconnect = async () => {
      if (asked.push("reconnect") === 1) {
        redialling.resolve();
        return;
      }
      credential.authenticator("nonce-again");
      rig.push("reconnect");
    };
    // The clock stands still from here: the follow asks again on the status alone, within one
    // turn of the event loop, where a follow that waited on a timer to ask again would not.
    jest.useFakeTimers();
    const following = followNatsCredential(rig.connection, credential);
    await redialling.promise;
    // The in-flight redial comes back, admitted as the old session.
    rig.push("reconnect");
    await turn();
    expect(asked).toEqual(["reconnect", "reconnect"]);
    await following;
    expect(credential.needsReconnect()).toBe(false);
    expect(rig.stoppedReader()).toBe(true);
  }, 30_000);
});
