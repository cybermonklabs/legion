// The rows on the broker trade a session's NATS dial presents: a broker that answers no trade,
// answers 503 or grants late, a broker token the hub refuses, the token's renewal for the
// connection's life, and the token a session switch moves to, with what a viewer of a session is
// replayed after a switch, a rollback or a resume. They have a file of their own so that
// `nats-start.test.ts` stays under 1,000 lines. Their `describe` is "envoy OMP extension", the one
// `envoy.test.ts` uses, so a row's full name is the same in every file. The files share one
// harness (`../test/extension-harness`).
import { describe, expect } from "bun:test";
import { writeFileSync } from "node:fs";
import { join } from "node:path";
import { AGENT_STREAM_LIMITS } from "@legion/contracts";
import { NATS_DIAL_TOKEN_DROPPED } from "@legion/envoy-client/nats-connection";
import { TRADE_DEADLINE_MS } from "@legion/envoy-client/nats-credential";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { until } from "@legion/test-fixtures/wait";
import { ErrorCode, NatsError } from "nats";
import { z } from "zod";
import type { EnvoyModule } from "../test/envoy-module";
import {
  configDirectory,
  createPi,
  onPassClock,
  type PassClock,
  rowLogSink,
  rowProbe,
  rowSessionShutdown,
  sessionContext,
  setNatsConnectForTests,
  topLevelSession,
  useExtensionHarness,
  withEnvoyTimings,
} from "../test/extension-harness";
import {
  natsStandInConnect,
  natsState,
  responseWithRegistration,
  rowCheck,
  rowFetch,
  stillOn,
  test,
} from "../test/listener-stand-in";
import type { SessionContext } from "../test/stand-in-host";

useExtensionHarness();

/** A pod's projected service-account token, which this machine trades at a stub broker. */
const MACHINE_IDENTITY_TOKEN = fixtureSecret("machine");

describe("envoy OMP extension", () => {
  // The acceptance's row for #4140 (AcceptE2-2): a broker that answers no trade. Run it alone with
  // `bun test extensions/nats-broker-trade.test.ts -t 'a broker that answers no trade'` in
  // packages/pi-envoy.
  // The broker trade is one retry loop per session: a broker that answers 503 is traded again at
  // 15 s doubling to 5 minutes, and each caller waits on that loop within its own deadline. A pass
  // of the session's registration holds several such callers: the listener calls (the pod's
  // broker credential, since it holds no Envoy bearer) and the NATS dial's `ready` (20 s).
  // Together they could outlast the pass's 30 s deadline. The queue would then abandon the task,
  // and the notice would say "a queued task did not settle within 30000ms and was abandoned",
  // with no word of the trade. The row requires that the pass ends inside its deadline and that
  // its notice names the failed trade. The acceptance's leak matrix runs the same case on the real
  // stack, for each trade that cannot succeed: a broker that refuses connections, a broker that
  // answers 500 or a malformed token, and a machine token that cannot be read.
  test("a broker that answers no trade: the session's notice names the trade, and no pass is abandoned", async () => {
    // The pass deadline the story above names, 30 s, the deadline's floor (`MIN_PASS_DEADLINE_MS`),
    // with the heartbeat shortened to it, since a deadline is never shorter than the heartbeat.
    // Each pass's dial waits for ready within its bound (`startTradeBoundMs`, 20 s of the 30 s),
    // and that bound and the queue's deadline are this process's timers, so the row reads both
    // passes on the test's clock (`onPassClock`) rather than leaving their order to how fast the
    // host runs a pass's work: a pass that outlasts its deadline is abandoned, and the session is
    // told so instead of the trade.
    const passMs = 30_000;
    const startBoundMs = 20_000;
    await withEnvoyTimings({ heartbeatMs: passMs, passDeadlineMs: passMs }, async () => {
      const tokenFile = join(configDirectory, "projected-token");
      writeFileSync(tokenFile, `${MACHINE_IDENTITY_TOKEN}\n`);
      process.env.NATS_AUTH = "broker";
      process.env.AGENT_SECRETS_URL = "http://broker.test";
      process.env.AGENT_SECRETS_MACHINE_IDENTITY = "kubernetes";
      process.env.AGENT_SECRETS_MACHINE_TOKEN_FILE = tokenFile;
      let answered = 0;
      globalThis.fetch = async (input, init) => {
        const request = new Request(input, init);
        if (new URL(request.url).host !== "broker.test")
          return responseWithRegistration(input, init, []);
        await request.text();
        answered += 1;
        return new Response("the broker is away", { status: 503 });
      };
      const notifications: { readonly message: string }[] = [];
      const intervals: { callback: () => void; intervalMs: number }[] = [];
      const context: SessionContext = {
        cwd: "/tmp/envoy-omp-test",
        sessionManager: { ...topLevelSession, getSessionId: () => "ses_broker_away" },
        setInterval: (callback, intervalMs) => intervals.push({ callback, intervalMs }),
        // The one extra call a failed heartbeat arms is not run: the row reads the heartbeat.
        setTimeout: () => undefined,
        ui: {
          ...sessionContext().ui,
          notify: (message) => notifications.push({ message }),
        },
      };
      const { default: envoyExtension }: EnvoyModule = await import(
        "./envoy.ts?broker-answers-no-trade"
      );
      const fixture = createPi();
      await envoyExtension(fixture.pi);
      const stop = rowSessionShutdown(fixture, context);
      const trade = (from: number) =>
        notifications
          .slice(from)
          .find(({ message }) => message.includes("the trade for the session's token"));
      const heartbeatFailed = (from: number) =>
        notifications
          .slice(from)
          .find(({ message }) => message.includes("envoy: registry heartbeat failed"));
      const { start, from, heartbeat } = await onPassClock(async (clock) => {
        const start = await readPass(clock, {
          begin: () => fixture.handlers.get("session_start")?.({}, context),
          boundMs: startBoundMs,
          answered: () => answered,
          said: () => trade(0) !== undefined,
        });
        // The next heartbeat pass, while the broker is still away, as a pod's heartbeat runs it:
        // the heartbeat's own interval, whose tick says what its pass came to, a failure or a pass
        // the queue abandoned at its deadline. The 15 s retry the start's failure arms says
        // neither, so a row that ran it saw nothing of its pass.
        const tick = intervals.find(({ intervalMs }) => intervalMs === passMs);
        if (tick === undefined) throw new Error("the heartbeat interval was not registered");
        const from = notifications.length;
        // It trades no sooner than the retry loop the start's failure left running, whose next
        // trade waits on the clock, so the clock is held only until the pass arms its bound.
        const heartbeat = await readPass(clock, {
          begin: () => tick.callback(),
          boundMs: startBoundMs,
          said: () => heartbeatFailed(from) !== undefined,
        });
        return { start, from, heartbeat };
      });
      // Each pass says what its trade came to as its bound ends, with the test's clock standing
      // at it, and not a step before, and then settles inside its deadline: a ready with no bound
      // outlives the pass, and a bound grown into the 10 s the pass keeps after it
      // (`PASS_REST_MS`) waits longer, so neither says so here.
      for (const read of [start, heartbeat]) {
        expect(read.early).toBe(false);
        expect(read.saidMs).toBe(startBoundMs);
        expect(read.armedMs).toBe(0);
        expect(read.settled).toBe(true);
      }
      expect(trade(0)?.message).toContain("HTTP 503");
      expect(notifications.slice(from).map(({ message }) => message)).toEqual([
        expect.stringContaining("envoy: registry heartbeat failed"),
      ]);
      expect(notifications.some(({ message }) => message.includes("did not settle within"))).toBe(
        false
      );
      await stop();
    });
    // What can hang is `readPass`'s wait for each pass's work up to its bound (`clock.wait`),
    // which has no bound of turns, because the start's trade reads the pod's token from disk, real
    // I/O.
  });

  /** A pod whose NATS connection presents its broker token, at a broker that answers each trade
   *  as `answer(n)` says for its n-th: a 503, or a token it mints for the trade's session.
   *  Answers the tokens minted, each to its trade's session, and how many trades have reached
   *  the broker. */
  function brokerPod(answer: (trade: number) => "503" | "grant"): {
    minted: Map<string, string>;
    trades: () => number;
  } {
    stillOn(rowCheck(), "standing in the broker pod");
    const tokenFile = join(configDirectory, "projected-token");
    writeFileSync(tokenFile, `${MACHINE_IDENTITY_TOKEN}\n`);
    process.env.NATS_AUTH = "broker";
    process.env.AGENT_SECRETS_URL = "http://broker.test";
    process.env.AGENT_SECRETS_MACHINE_IDENTITY = "kubernetes";
    process.env.AGENT_SECRETS_MACHINE_TOKEN_FILE = tokenFile;
    const minted = new Map<string, string>();
    let trades = 0;
    globalThis.fetch = async (input, init) => {
      const request = new Request(input, init);
      if (new URL(request.url).host !== "broker.test")
        return responseWithRegistration(input, init, []);
      const session = new URLSearchParams(await request.text()).get("session") ?? "";
      trades += 1;
      if (answer(trades) === "503") return new Response("the broker is away", { status: 503 });
      const token = fixtureSecret("nats");
      minted.set(token, session);
      return Response.json({
        access_token: token,
        issued_token_type: "urn:ietf:params:oauth:token-type:access_token",
        token_type: "DPoP",
        expires_in: 900,
      });
    };
    return { minted, trades: () => trades };
  }

  /** A session's context whose notices are kept with how long after `started` each was made: the
   *  start itself can hold the turn, so the time is taken as the notice is made. */
  function timedContext(sessionID: string, started: number) {
    const notifications: { readonly message: string; readonly at: number }[] = [];
    const context: SessionContext = {
      cwd: "/tmp/envoy-omp-test",
      sessionManager: { ...topLevelSession, getSessionId: () => sessionID },
      setInterval: () => {},
      ui: {
        ...sessionContext().ui,
        notify: (message) => notifications.push({ message, at: Date.now() - started }),
      },
    };
    return { context, notifications };
  }

  /**
   * Reads one pass on the test's clock (`onPassClock`): `begin` starts it, and its own work up to
   * the `AbortSignal.timeout` of `boundMs` it arms, and the broker's next answer where the pass
   * trades at once (`answered`), run with the clock held. That work includes the trade's read of
   * the pod's token from disk (`projectedToken`'s `readFile`), real I/O that no count of turns
   * measures, so the clock is held for it with no bound of turns (`wait`). The clock then moves in
   * 100 ms steps, which let each retry the loop makes be answered on real turns: one step short of
   * the bound the pass must have said nothing, and at the bound, held, it says what its trade came
   * to (`said`), which the code makes in process from the last answer it holds. Then it goes on to
   * its end inside its deadline. Answers, each from `begin` on the test's clock, when the bound was
   * armed and when the pass said so, whether it had said so a step early, and whether it settled.
   */
  async function readPass(
    clock: PassClock,
    options: {
      readonly begin: () => unknown;
      readonly boundMs: number;
      /** How many trades the broker has answered, for a pass that trades as it starts. */
      readonly answered?: () => number;
      readonly said: () => boolean;
    }
  ): Promise<{ armedMs?: number; early: boolean; saidMs?: number; settled: boolean }> {
    const { begin, boundMs, answered, said } = options;
    const armedBefore = clock.armed.filter((ms) => ms === boundMs).length;
    const answeredBefore = answered?.() ?? 0;
    const started = clock.now();
    let settled = false;
    void Promise.resolve(begin()).finally(() => {
      settled = true;
    });
    const armed = () => clock.armed.filter((ms) => ms === boundMs).length > armedBefore;
    await clock.wait(
      "the pass's work up to its bound",
      () => armed() && (answered === undefined || answered() > answeredBefore)
    );
    const armedMs = armed() ? clock.now() - started : undefined;
    for (let step = 0; step < boundMs / 100 - 1; step += 1) await clock.advance(100);
    const early = said();
    await clock.advance(100);
    await clock.turns(
      10_000,
      "the pass's word on what its trade came to, which the row reads itself",
      said
    );
    const saidMs = said() ? clock.now() - started : undefined;
    for (let step = 0; step < 150 && !settled; step += 1) await clock.advance(100);
    return { armedMs, early, saidMs, settled };
  }

  test("a broker that never grants: the start fails at its 20 s bound, naming the 503, inside its pass", async () => {
    // The start waits for ready within its bound (`startTradeBoundMs`: the pass less the 10 s the
    // rest of it needs, `PASS_REST_MS`, which for a 30 s pass is `TRADE_DEADLINE_MS`, 20 s),
    // through the retry loop's retries, and then fails naming what the last trade came to, before
    // the pass's deadline: a pass abandoned at that deadline names no cause at all. The deadline is
    // its floor here, 30 s (`MIN_PASS_DEADLINE_MS`, the heartbeat shortened to it). Both are this
    // process's timers, so the row reads them on the test's clock (`onPassClock`), from the start
    // on, rather than leaving their order to how fast the host runs the pass's work.
    const passMs = 30_000;
    const startBoundMs = 20_000;
    await withEnvoyTimings({ heartbeatMs: passMs, passDeadlineMs: passMs }, async () => {
      brokerPod(() => "503");
      // How many trades the broker has answered. The pod's fetch is `rowFetch()`, as in the
      // switch-follow row.
      const pod = rowFetch();
      let answered = 0;
      globalThis.fetch = async (input, init) => {
        const answer = await pod(input, init);
        if (new URL(new Request(input, init).url).host === "broker.test") answered += 1;
        return answer;
      };
      const { default: envoyExtension }: EnvoyModule = await import(
        "./envoy.ts?broker-answers-503"
      );
      const fixture = createPi();
      await envoyExtension(fixture.pi);
      const { context, notifications } = timedContext("ses_broker_503", Date.now());
      const stop = rowSessionShutdown(fixture, context);
      const notice = () =>
        notifications.find(({ message }) => message.includes("the trade for the session's token"));
      const read = await onPassClock((clock) =>
        readPass(clock, {
          begin: () => fixture.handlers.get("session_start")?.({}, context),
          boundMs: startBoundMs,
          answered: () => answered,
          said: () => notice() !== undefined,
        })
      );
      // The start names the trade as its bound ends, with the test's clock standing at it, and
      // not a step before: a start whose ready waits with no bound outlives the pass, and one
      // whose bound grows into the 10 s the pass keeps after it (`PASS_REST_MS`) waits longer, so
      // neither names it here.
      expect(read.early).toBe(false);
      expect(
        read.saidMs,
        `no notice named the trade: ${notifications.map(({ message }) => message).join(" | ")}`
      ).toBe(startBoundMs);
      // The bound was armed with the clock held, where the code arms it.
      expect(read.armedMs).toBe(0);
      expect(notice()?.message).toMatch(/the trade for the session's token.*HTTP 503/);
      expect(read.settled).toBe(true);
      expect(notifications.some(({ message }) => message.includes("did not settle within"))).toBe(
        false
      );
      await stop();
    });
    // What can hang is `readPass`'s wait for the start's work up to its bound (`clock.wait`),
    // which has no bound of turns, because its trade reads the pod's token from disk, real I/O.
  });

  test("a first trade that answers 503 and then grants: the start presents its token in its first pass", async () => {
    // The start has nothing to do until it holds a token, so it waits for ready: the retry
    // loop's next trade, 15 s at most later, grants, and the session's first pass dials and
    // registers with it, rather than waiting for the next heartbeat, 90 s on.
    const { minted, trades } = brokerPod((trade) => (trade === 1 ? "503" : "grant"));
    const started = Date.now();
    const { context, notifications } = timedContext("ses_broker_once_away", started);
    const { default: envoyExtension }: EnvoyModule = await import("./envoy.ts?broker-away-once");
    const fixture = createPi();
    await envoyExtension(fixture.pi);
    const stop = rowSessionShutdown(fixture, context);
    // The retry loop's next trade comes 7.5 to 15 s after the first's 503 (its jittered delay,
    // at most `RETRY_FIRST_WAIT_MS`) and the start's ready waits for it within its 20 s bound
    // (`TRADE_DEADLINE_MS`): both are this process's timers, so the start runs on the test's
    // clock (`onPassClock`). Each trade first reads the pod's token from disk (`projectedToken`'s
    // `readFile`), real I/O that runs off the event loop; its DPoP proof is signed synchronously.
    // A clock moved while a read is in flight races the 20 s as the real clock did on a slow
    // host, so the clock never moves then. It is held until the first trade has reached the
    // broker, its read done (`wait`, which no count of turns bounds). The broker answers 503, and
    // all the start waits on is then the retry's delay: the clock moves through it 100 ms at a
    // time until the retry has fired, which the second trade shows by arming its own 30 s bound
    // (`TRADE_ATTEMPT_MS`) in the turn it starts its read, and is held from that step on, through
    // the read, the grant and the start's dial and registration. The delay is set as the code
    // reads the 503, in process, a step at most after the broker answers, so the retry fires
    // before the 20 s; the steps stop at the 20 s in any case, which the clock never passes, and
    // a retry that has not begun by then, with the start unsettled, fails the row there.
    /** How long one trade of the retry loop may take: the token client's `TRADE_ATTEMPT_MS`
     *  (`packages/cipher-client/src/token-client.ts`), which pi-envoy does not import. */
    const TRADE_ATTEMPT_MS = 30_000;
    await onPassClock(async (clock) => {
      let settled = false;
      const starting = Promise.resolve(
        fixture.handlers.get("session_start")?.({}, context)
      ).finally(() => {
        settled = true;
      });
      /** The trades begun so far, each by the `TRADE_ATTEMPT_MS` bound it arms before its read.
       *  Between the first trade and the retry's, no other bound of that length arms: the start
       *  waits for its token within its own bound, armed before the first trade; the retry loop
       *  sleeps out its delay on a `setTimeout`, which `armed` does not hold; the start's dial and
       *  registration wait for the token; and this context's `setInterval` runs no heartbeat. */
      const begun = () => clock.armed.filter((ms) => ms === TRADE_ATTEMPT_MS).length;
      await clock.wait("the first trade", () => trades() > 0 || settled);
      const first = begun();
      for (let step = 0; step < 200 && begun() === first && !settled; step += 1) {
        await clock.advance(100);
      }
      if (begun() === first && !settled) {
        throw new Error("the retry's trade did not begin in 20 s, and the start did not settle");
      }
      await clock.wait("the second trade", () => trades() > 1 || settled);
      await clock.waitOn("the start", starting);
    });
    expect(
      natsState.presented.map((token) => minted.get(token)),
      `the start presented no token: ${notifications.map(({ message }) => message).join(" | ")}`
    ).toEqual(["ses_broker_once_away"]);
    expect(natsState.controls.get("notifications.agent.ses_broker_once_away")?.active()).toBe(true);
    expect(
      notifications.some(({ message }) => message.includes("the trade for the session's token"))
    ).toBe(false);
    await stop();
    // What can hang is a wait for a trade's read of the pod's token from disk or for the start's
    // dial (`clock.wait`, `clock.waitOn`), which has no bound of turns: the read is real I/O.
  });

  test("a first NATS dial whose broker token the hub refuses twice drops it, starts another trade, and says so", async () => {
    // The pass's dials share the keeper's count of the token's refusals (`connectNats`'
    // `hooks.credential`), so the dial drops a token the hub refused twice, as the keeper does on
    // an open connection. The waits between the dials are recorded, not slept.
    const ownRow = rowCheck();
    const { minted } = brokerPod(() => "grant");
    natsState.failConnects = 2;
    natsState.failError = () =>
      new NatsError("'Authorization Violation'", ErrorCode.AuthorizationViolation);
    setNatsConnectForTests(
      natsStandInConnect,
      { firstDelayMs: 5, maxDelayMs: 5, budgetMs: 30_000 },
      { delay: async () => {}, random: () => 0 }
    );
    const warnings: string[] = [];
    const stopSink = rowLogSink((entry) => {
      if (entry.level === "warn") warnings.push(entry.message);
    });
    try {
      const { context, notifications } = timedContext("ses_first_dial_refused", Date.now());
      const { default: envoyExtension }: EnvoyModule = await import(
        "./envoy.ts?first-dial-refused-twice"
      );
      const fixture = createPi();
      await envoyExtension(fixture.pi);
      const stop = rowSessionShutdown(fixture, context);
      // The start's work, two trades that each read the pod's token from disk and three dials,
      // runs inside this process's own timers: the start's trade bound (`TRADE_DEADLINE_MS`) and
      // its pass's deadline. On the real clock the row's outcome depended on the host finishing
      // that work inside them, so the start runs on the test's clock (`onPassClock`), held while
      // the work runs, which no count of turns measures (`waitOn`): the token's read is real I/O.
      await onPassClock((clock) =>
        clock.waitOn("the start", fixture.handlers.get("session_start")?.({}, context))
      );
      expect(natsState.failConnects, notifications.map(({ message }) => message).join(" | ")).toBe(
        0
      );
      // The start's token, and the trade the drop started.
      await until(
        "the trade the drop started",
        rowProbe(() => minted.size === 2)
      );
      expect([...minted.values()]).toEqual(["ses_first_dial_refused", "ses_first_dial_refused"]);
      expect(warnings.filter((message) => message.includes(NATS_DIAL_TOKEN_DROPPED))).toHaveLength(
        1
      );
      await stop();
    } finally {
      stopSink();
      if (ownRow()) setNatsConnectForTests(natsStandInConnect);
    }
    // What can hang is the wait for the start (`clock.waitOn`), which has no bound of turns,
    // because each of its trades reads the pod's token from disk, real I/O.
  });

  test("natsCredential follows a session switch", async () => {
    // A session that presents its broker token (NATS_AUTH=broker) is admitted to its own subjects
    // alone, so after a switch the connection presents the new session's token, and is back,
    // before anything of the new session's is subscribed.
    const tokenFile = join(configDirectory, "projected-token");
    writeFileSync(tokenFile, `${MACHINE_IDENTITY_TOKEN}\n`);
    process.env.NATS_AUTH = "broker";
    process.env.AGENT_SECRETS_URL = "http://broker.test";
    process.env.AGENT_SECRETS_MACHINE_IDENTITY = "kubernetes";
    process.env.AGENT_SECRETS_MACHINE_TOKEN_FILE = tokenFile;
    const traded: string[] = [];
    /** Each token the stub broker minted, to the session its trade wrote under. */
    const minted = new Map<string, string>();
    globalThis.fetch = async (input, init) => {
      const request = new Request(input, init);
      const url = new URL(request.url);
      if (url.host !== "broker.test") return responseWithRegistration(input, init, []);
      const session = new URLSearchParams(await request.text()).get("session") ?? "";
      traded.push(session);
      const token = fixtureSecret("nats");
      minted.set(token, session);
      return Response.json({
        access_token: token,
        issued_token_type: "urn:ietf:params:oauth:token-type:access_token",
        token_type: "DPoP",
        expires_in: 900,
      });
    };
    const { default: envoyExtension }: EnvoyModule = await import(
      "./envoy.ts?nats-credential-switch"
    );
    const fixture = createPi();
    await envoyExtension(fixture.pi);
    const stop = rowSessionShutdown(fixture, sessionContext("ses_switch_to"));
    // The start waits for its token within its trade bound (`startTradeBoundMs`, 20 s of the 30 s
    // pass), and the switch's follow takes the new session's token within its share of the
    // follow's bound (20 s of `followBoundMs`'s 28 s), then waits for the connection to come back
    // within the rest (`FOLLOW_RECONNECT_MS`, 8 s); each trade is bounded by its own attempt too
    // (`TRADE_ATTEMPT_MS`, 30 s). Those are timers of this process, which each trade must beat,
    // and a trade first reads the pod's token from disk, real I/O that runs off the event loop,
    // which no count of turns measures. So the row runs on the test's clock (`onPassClock`) from
    // the start on, where none of those timers moves, held while the start and the follow's trade
    // run (`waitOn`, `wait`). The clock moves once, 1 ms, which ends none of those bounds, after
    // the follow has asked the connection to come back, its trade done: the stand-in server
    // answers that on a 0 ms timer, which only a move of the clock runs. On the real clock a host
    // slow enough to stretch a trade past its bound would fail the row.
    await onPassClock(async (clock) => {
      await clock.waitOn(
        "the start",
        fixture.handlers.get("session_start")?.({}, sessionContext("ses_switch_from"))
      );
      // Each CONNECT presented a token the broker minted for the session it names here.
      expect(natsState.presented.map((token) => minted.get(token))).toEqual(["ses_switch_from"]);
      expect(natsState.controls.get("notifications.agent.ses_switch_from")?.active()).toBe(true);

      natsState.timeline.length = 0;
      let switched = false;
      const switching = Promise.resolve(
        fixture.handlers.get("session_switch")?.({}, sessionContext("ses_switch_to"))
      ).finally(() => {
        switched = true;
      });
      await clock.wait(
        "the follow's reconnect",
        () => natsState.timeline.includes("reconnect") || switched
      );
      await clock.advance(1);
      await clock.waitOn("the switch", switching);
      expect(natsState.presented.map((token) => minted.get(token))).toEqual([
        "ses_switch_from",
        "ses_switch_to",
      ]);
      // The reconnect comes before every subscribe of the new session's subjects.
      const moved = natsState.timeline.filter(
        (entry) => entry === "reconnect" || entry.includes("ses_switch_to")
      );
      expect(moved[0]).toBe("reconnect");
      expect(moved.length).toBeGreaterThan(1);
      expect(natsState.controls.get("notifications.agent.ses_switch_to")?.active()).toBe(true);
      expect(traded).toContain("ses_switch_to");
    });
    await stop();
    // What can hang is the wait for the start, the follow's reconnect or the switch
    // (`clock.waitOn`, `clock.wait`), which has no bound of turns, because the start's and the
    // follow's trades read the pod's token from disk, real I/O.
  });

  test("the keeper's warning of a refused token reaches the session as a notice, once", async () => {
    // A token the server refuses though it is live is a session that stops receiving, which the
    // session is told of as every other inbound outage is (the review of 2fa6fbf7cf, M2).
    const { minted } = brokerPod(() => "grant");
    const { context, notifications } = timedContext("ses_keeper_notice", Date.now());
    const { default: envoyExtension }: EnvoyModule = await import("./envoy.ts?keeper-notice");
    const fixture = createPi();
    await envoyExtension(fixture.pi);
    const stop = rowSessionShutdown(fixture, context);
    const refused = () =>
      notifications.filter(({ message }) =>
        message.includes("refused the connection's live broker token")
      );
    // The start's trade and dial wait within its 20 s bound (`TRADE_DEADLINE_MS`), this process's
    // own timer, so the row runs on the test's clock (`onPassClock`), which the start's work does
    // not move: on the real clock a host slow enough to stretch the start past that bound failed
    // the row. The start's trade first reads the pod's token from disk (`projectedToken`'s
    // `readFile`), real I/O that no count of turns measures, so the clock is held for the start
    // with no bound of turns (`waitOn`). The reader's warning is this process's own work, done
    // within turns of the push.
    await onPassClock(async (clock) => {
      await clock.waitOn("the start", fixture.handlers.get("session_start")?.({}, context));
      expect(natsState.presented.map((token) => minted.get(token))).toEqual(["ses_keeper_notice"]);
      for (const read of natsState.statusReaders) {
        read({ type: "error", data: "AUTHORIZATION_VIOLATION" });
      }
      await clock.hold("the notice of the refused token", () => refused().length > 0);
    });
    expect(refused().map(({ message }) => message)).toEqual([
      "envoy: the NATS server refused the connection's live broker token (AUTHORIZATION_VIOLATION): the auth service is away, or the token was revoked; the connection keeps redialling, and at the second refusal the token is dropped and another traded",
    ]);
    await stop();
    // What can hang is the wait for the start (`clock.waitOn`), which has no bound of turns,
    // because its trade reads the pod's token from disk, real I/O.
  });

  test("a switch whose new token never comes fails inside a short pass, naming the trade", async () => {
    // ENVOY_HEARTBEAT_MS=10000 ENVOY_PASS_DEADLINE_MS=10000: the follow takes at most 8 s of the
    // pass (`followBoundMs`), its trade half of that, 4 s (`followNatsCredential`), so the pass
    // names what the trade came to rather than being abandoned at its deadline with no word of
    // the cause (the review of 2fa6fbf7cf, m3: unbounded, the trade alone waited 20 s).
    await withEnvoyTimings({ heartbeatMs: 10_000, passDeadlineMs: 10_000 }, async () => {
      brokerPod((trade) => (trade === 1 ? "grant" : "503"));
      // Whether the follow traded for the new session at the broker. The pod's fetch is
      // `rowFetch()`: `globalThis.fetch` reads the file's recording wrapper, which calls whatever
      // a row installed, so a wrapper that called it would call itself forever.
      const pod = rowFetch();
      let followTraded = false;
      globalThis.fetch = async (input, init) => {
        const request = new Request(input, init);
        if (new URL(request.url).host === "broker.test") {
          const session = new URLSearchParams(await request.clone().text()).get("session");
          if (session === "ses_follow_to") followTraded = true;
        }
        return await pod(input, init);
      };
      const before = timedContext("ses_follow_from", Date.now());
      const { default: envoyExtension }: EnvoyModule = await import("./envoy.ts?follow-bound");
      const fixture = createPi();
      await envoyExtension(fixture.pi);
      const after = timedContext("ses_follow_to", Date.now());
      const stop = rowSessionShutdown(fixture, after.context);
      const all = () => [...before.notifications, ...after.notifications];
      const named = () =>
        all().find(({ message }) => message.includes("could not take the new session's token"));
      // From the boot on, the test's clock (`onPassClock`): the passes' deadlines, the follow's
      // bound and its trade's share are all this process's timers, so on it they come at the
      // times the code gives them, whatever the host's speed. The work between those times (the
      // start's trade and dial, the pass's restores, the follow's trade and the report of how it
      // ended) comes on real turns, which move no clock, so the clock is held while it runs:
      // through the boot, until the follow has traded, and again at the trade's share until the
      // notice is made. Each trade first reads the pod's token from disk (`projectedToken`'s
      // `readFile`), real I/O that no count of turns measures, so the boot and the follow's trade
      // are held for with no bound of turns (`waitOn`, `wait`); the rest is in-process here. A
      // boot on the real clock would race its own 10 s pass deadline, and a boot past it puts
      // "did not settle within" into the notices this row reads.
      const shareMs = 4_000;
      const { earlyNotice, noticeMs, settled, switching } = await onPassClock(async (clock) => {
        const bootedAt = clock.now();
        await clock.waitOn("the boot", fixture.handlers.get("session_start")?.({}, before.context));
        // The boot's own work moved no clock.
        expect(clock.now()).toBe(bootedAt);
        const switchedAt = clock.now();
        let settled = false;
        const switching = Promise.resolve(
          fixture.handlers.get("session_switch")?.({}, after.context)
        );
        void switching.finally(() => {
          settled = true;
        });
        await clock.wait("the follow's trade", () => followTraded);
        // The follow traded with the clock held, so nothing of the pass's own work before it
        // moved the clock.
        expect(followTraded).toBe(true);
        expect(clock.now()).toBe(switchedAt);
        // Not before the trade's share: one step short of it, the pass has said nothing.
        await clock.advance(shareMs - 50);
        const earlyNotice = named()?.message;
        // At the share, held until the pass says what the trade came to.
        await clock.advance(50);
        await clock.turns(
          10_000,
          "the pass's word on what the trade came to, which the row reads itself",
          () => named() !== undefined
        );
        const noticeMs = named() === undefined ? undefined : clock.now() - switchedAt;
        // The pass goes on after the follow (its subscribes and its registration), inside its
        // own deadline, which a pass that outlived it would say below.
        for (let step = 0; step < 120 && !settled; step += 1) await clock.advance(100);
        return { earlyNotice, noticeMs, settled, switching };
      });
      // The notice is made as the trade's share ends, half the follow's 8 s bound of the pass's
      // 10 s, with the test's clock standing at that share: a follow given no bound (m3) has 20 s
      // for its trade and is abandoned with the pass at 10 s, saying nothing, and a follow whose
      // bound grows into the 2 s the pass keeps after it (`FOLLOW_REST_MS`) gives its trade more
      // than 4 s, so neither names the trade here.
      expect(earlyNotice).toBeUndefined();
      expect(noticeMs).toBe(shareMs);
      expect(named()?.message).toContain("HTTP 503");
      expect(settled).toBe(true);
      expect(all().some(({ message }) => message.includes("did not settle within"))).toBe(false);
      await switching;
      await stop();
    });
    // What can hang is the wait for the boot or for the follow's trade (`clock.waitOn`,
    // `clock.wait`), which has no bound of turns, because each trade reads the pod's token from
    // disk, real I/O.
  });

  test("after a switch whose move failed, the old session's viewer is replayed nothing of the new one", async () => {
    // The move to the new session's token fails (its trade is refused outright, a refusal no
    // trade cures, so the move fails at once rather than at the follow's deadline), so the new
    // session's agent-stream subject never opens. What the host records next is the new session's,
    // and the old session's control subject must not answer a replay with it.
    const tokenFile = join(configDirectory, "projected-token");
    writeFileSync(tokenFile, `${MACHINE_IDENTITY_TOKEN}\n`);
    process.env.NATS_AUTH = "broker";
    process.env.AGENT_SECRETS_URL = "http://broker.test";
    process.env.AGENT_SECRETS_MACHINE_IDENTITY = "kubernetes";
    process.env.AGENT_SECRETS_MACHINE_TOKEN_FILE = tokenFile;
    globalThis.fetch = async (input, init) => {
      const request = new Request(input, init);
      if (new URL(request.url).host !== "broker.test")
        return responseWithRegistration(input, init, []);
      const session = new URLSearchParams(await request.text()).get("session") ?? "";
      if (session !== "ses_failed_from") {
        return Response.json({ error: "invalid_client" }, { status: 401 });
      }
      return Response.json({
        access_token: fixtureSecret("nats"),
        issued_token_type: "urn:ietf:params:oauth:token-type:access_token",
        token_type: "DPoP",
        expires_in: 900,
      });
    };
    const { default: envoyExtension }: EnvoyModule = await import(
      "./envoy.ts?nats-credential-failed-switch"
    );
    const fixture = createPi();
    await envoyExtension(fixture.pi);
    const oldControl = "agentstream.ses_failed_from.control";
    const next = sessionContext("ses_failed_to");
    const stop = rowSessionShutdown(fixture, next);
    const marker = "a turn of the session switched to";
    // The start's trade and the switch's follow each wait within a bound of this process's own
    // (`TRADE_DEADLINE_MS`, `followBoundMs`), so the row runs on the test's clock (`onPassClock`),
    // which their work does not move: on the real clock a host slow enough to stretch the start
    // past its bound failed the row before it read anything of the switch. Each of their trades
    // first reads the pod's token from disk (`projectedToken`'s `readFile`), real I/O that no
    // count of turns measures, so the clock is held for the start and the switch with no bound of
    // turns (`waitOn`). The turn's end is in-process.
    await onPassClock(async (clock) => {
      await clock.waitOn(
        "the start",
        fixture.handlers.get("session_start")?.({}, sessionContext("ses_failed_from"))
      );
      expect(natsState.controls.get(oldControl)?.active()).toBe(true);

      await clock.waitOn("the switch", fixture.handlers.get("session_switch")?.({}, next));
      expect(natsState.controls.has("agentstream.ses_failed_to.control")).toBe(false);
      await clock.run(
        "the turn's end",
        fixture.handlers.get("message_end")?.(
          {
            message: {
              content: [{ text: marker, type: "text" }],
              role: "assistant",
              timestamp: 30,
            },
          },
          next
        )
      );

      // A viewer of the old session asks for its history. What the replay would publish, it
      // publishes within turns of the request, held here: the old session's subject is closed,
      // so it publishes nothing, and the turns are what a replay that did answer would need.
      const control = natsState.controls.get(oldControl);
      control?.push(JSON.stringify({ type: "watch", v: 1 }));
      control?.push(JSON.stringify({ type: "replay", v: 1 }), "inbox-old-viewer");
      await clock.turns(200, "the turns a replay that did answer would need");
    });
    const replayed = natsState.published
      .filter((each) => each.subject === "inbox-old-viewer")
      .map((each) => new TextDecoder().decode(each.data));
    expect(replayed.join("\n")).not.toContain(marker);
    expect(natsState.controls.get(oldControl)?.active()).toBe(false);
    await stop();
    // What can hang is the wait for the start or the switch (`clock.waitOn`), which has no bound
    // of turns, because each of their trades reads the pod's token from disk, real I/O.
  });

  // Not decided by the host's speed: the row waits for the viewer's replay to hold B's own turn,
  // however late it lands, and reads what else it holds only then.
  test("after a rollback with no event, a viewer of the session left is replayed nothing of the one returned to", async () => {
    // Two sessions of one host: it switches from A to B, B's viewer attaches, and the host then
    // returns to A with no event. Until the next pass runs, A's control subject is closed and B's
    // is still open; what the host records then is A's, and B's viewer must be replayed only B's.
    globalThis.fetch = async (input, init) => responseWithRegistration(input, init, []);
    const { default: envoyExtension }: EnvoyModule = await import(
      "./envoy.ts?agent-stream-ring-per-session"
    );
    const fixture = createPi();
    await envoyExtension(fixture.pi);
    const first = sessionContext("ses_ring_a");
    const stop = rowSessionShutdown(fixture, first);
    await fixture.handlers.get("session_start")?.({}, first);
    const second = sessionContext("ses_ring_b");
    await fixture.handlers.get("session_switch")?.({}, second);
    const control = natsState.controls.get("agentstream.ses_ring_b.control");
    expect(control?.active()).toBe(true);
    const say = (text: string, timestamp: number) => ({
      message: { content: [{ text, type: "text" }], role: "assistant", timestamp },
    });
    const ownTurn = "a turn of the session switched to";
    await fixture.handlers.get("message_end")?.(say(ownTurn, 40), second);
    // The host returns to A with no event, and records A's next turn.
    const returnedTurn = "a turn of the session returned to";
    await fixture.handlers.get("message_end")?.(say(returnedTurn, 50), first);

    control?.push(JSON.stringify({ type: "watch", v: 1 }));
    control?.push(JSON.stringify({ type: "replay", v: 1 }), "inbox-viewer-of-b");
    const decoder = new TextDecoder();
    // The replay is one publish on the viewer's inbox (`pumpAgentStreamControl`), so once it holds
    // B's own turn it holds all it ever will. The replay can carry a turn at all: B's own is in it.
    const replayed = await until(
      "the replay on B's viewer's inbox to hold B's own turn",
      rowProbe(() => {
        const text = natsState.published
          .filter((each) => each.subject === "inbox-viewer-of-b")
          .map((each) => decoder.decode(each.data))
          .join("\n");
        return text.includes(ownTurn) && text;
      })
    );
    expect(replayed).not.toContain(returnedTurn);
    await stop();
    // What can hang is the session's start, its switch, a turn's end (`message_end`) or its
    // shutdown.
  });

  /** The frames a viewer that attaches to `session` is replayed, in the order the answer gives
   *  them. */
  const replayFrames = async (session: string, inbox: string) => {
    const control = natsState.controls.get(`agentstream.${session}.control`);
    expect(control?.active()).toBe(true);
    control?.push(JSON.stringify({ type: "watch", v: 1 }));
    control?.push(JSON.stringify({ type: "replay", v: 1 }), inbox);
    const answered = () => natsState.published.filter((each) => each.subject === inbox);
    await until(
      `the replay answer on ${inbox}`,
      rowProbe(() => answered().length > 0)
    );
    const answer = answered().at(-1);
    return z
      .object({ frames: z.array(z.record(z.string(), z.unknown())) })
      .parse(JSON.parse(new TextDecoder().decode(answer?.data))).frames;
  };

  /** What a viewer that attaches to `session` is replayed of its conversation: each message's id,
   *  role and Dispatch message, and each tool result's call, in the order the answer gives them. */
  const viewerReplay = async (session: string, inbox: string) =>
    (await replayFrames(session, inbox)).flatMap((frame) => {
      if (frame.kind === "tool-result") {
        return [{ call: z.object({ toolCallId: z.string() }).parse(frame.result).toolCallId }];
      }
      if (frame.kind !== "message") return [];
      const message = z
        .object({ dispatchMessageId: z.string().optional(), id: z.string(), role: z.string() })
        .parse(frame.message);
      return [{ dispatchMessageId: message.dispatchMessageId, id: message.id, role: message.role }];
    });

  /** A session's transcript as Oh My Pi's `getBranch` lists it: a person's Send the session took
   *  as its typed turn (`sent`, the attempt the session recorded handling, and the user message
   *  the host wrote with the message's id as its tag), a reply with a tool call, its result, an
   *  Envoy card the session showed and one it did not, another input's tagged user message, and
   *  the last reply, between entries that hold no message. */
  const resumedTranscript = (sent: string): unknown[] => [
    { id: "e0", modelId: "claude-opus-5", provider: "anthropic", type: "model_change" },
    {
      customType: "envoy-dispatch-handled-attempt",
      data: { attempt: 1, message_id: sent },
      id: "e1",
      type: "custom",
    },
    {
      id: "e2",
      message: {
        content: [{ text: "What changed?", type: "text" }],
        role: "user",
        tag: sent,
        timestamp: 10,
      },
      type: "message",
    },
    {
      id: "e3",
      message: {
        content: [
          { text: "Reading it.", type: "text" },
          { arguments: { path: "a.ts" }, id: "call-1", name: "read", type: "toolCall" },
        ],
        role: "assistant",
        timestamp: 20,
      },
      type: "message",
    },
    {
      id: "e4",
      message: {
        content: [{ text: "export {};", type: "text" }],
        isError: false,
        role: "toolResult",
        timestamp: 30,
        toolCallId: "call-1",
        toolName: "read",
      },
      type: "message",
    },
    {
      content: "A card the session showed.",
      customType: "envoy-message",
      details: { envoyCard: "card-1" },
      display: true,
      id: "e5",
      timestamp: new Date(40).toISOString(),
      type: "custom_message",
    },
    {
      content: "A card the session kept out of sight.",
      customType: "envoy-message",
      details: { envoyCard: "card-2" },
      display: false,
      id: "e6",
      timestamp: new Date(45).toISOString(),
      type: "custom_message",
    },
    // A compaction that kept every turn above from the Send on: the live context, which the
    // session's terminal draws on a resume, starts there.
    {
      firstKeptEntryId: "e2",
      id: "e7",
      summary: "The conversation so far.",
      type: "compaction",
    },
    {
      id: "e8",
      message: {
        content: [{ text: "A line another input sent.", type: "text" }],
        role: "user",
        tag: "other-input",
        timestamp: 50,
      },
      type: "message",
    },
    {
      id: "e9",
      message: { content: [{ text: "Done.", type: "text" }], role: "assistant", timestamp: 60 },
      type: "message",
    },
  ];

  /** What `viewerReplay` reads of `resumedTranscript(sent)`: the same frames the process that ran
   *  those turns recorded, the Send tagged with its Dispatch message as it was then. */
  const resumedTurns = (sent: string) => [
    { dispatchMessageId: sent, id: `u10:${sent}`, role: "user" },
    { dispatchMessageId: undefined, id: "a20", role: "assistant" },
    { call: "call-1" },
    { dispatchMessageId: undefined, id: "c40:card-1", role: "user" },
    { dispatchMessageId: undefined, id: "u50:other-input", role: "user" },
    { dispatchMessageId: undefined, id: "a60", role: "assistant" },
  ];

  // A process that resumes a session (a pod's restart, `omp --continue`) recorded none of its
  // turns, and a viewer who opened the session was replayed none of what its terminal still
  // showed (dispatch://INFRA-5990, R1).
  test("a session the host resumes at its start replays the turns its transcript holds", async () => {
    globalThis.fetch = async (input, init) => responseWithRegistration(input, init, []);
    const { default: envoyExtension }: EnvoyModule = await import(
      "./envoy.ts?agent-stream-resumed-start"
    );
    const fixture = createPi();
    await envoyExtension(fixture.pi);
    const sent = "22222222-2222-4222-8222-222222222222";
    const transcript = resumedTranscript(sent);
    const base = sessionContext("ses_resumed");
    const resumed: SessionContext = {
      ...base,
      sessionManager: {
        ...base.sessionManager,
        getBranch: () => transcript,
        getEntries: () => transcript,
      },
    };
    await fixture.handlers.get("session_start")?.({}, resumed);

    expect(await viewerReplay("ses_resumed", "inbox-resumed-viewer")).toEqual(resumedTurns(sent));
    await fixture.handlers.get("session_shutdown")?.({}, resumed);
  }, 30_000);

  // Oh My Pi keeps the transcript's message objects and rewrites them in place: a prune pass blanks
  // a stale tool result's content (`blankToolResult` in its `packages/agent/src/compaction/
  // pruning.ts`). The replay of a result the session was taken up with keeps the output it had
  // then, as a result this process recorded keeps the output it had at its `message_end`.
  test("a resumed session's tool result keeps its output in the replay after the host prunes it", async () => {
    globalThis.fetch = async (input, init) => responseWithRegistration(input, init, []);
    const { default: envoyExtension }: EnvoyModule = await import(
      "./envoy.ts?agent-stream-pruned-after-take-up"
    );
    const fixture = createPi();
    await envoyExtension(fixture.pi);
    const transcript = resumedTranscript("44444444-4444-4444-8444-444444444444");
    const base = sessionContext("ses_pruned_later");
    const resumed: SessionContext = {
      ...base,
      sessionManager: {
        ...base.sessionManager,
        getBranch: () => transcript,
        getEntries: () => transcript,
      },
    };
    await fixture.handlers.get("session_start")?.({}, resumed);
    // The host's prune pass after the take-up, on the object it holds: the tool result, e4.
    const pruned = Object(transcript[4]);
    expect(Reflect.get(pruned, "id")).toBe("e4");
    Reflect.set(Reflect.get(pruned, "message"), "content", [
      { text: "[Superseded by a newer read of this file]", type: "text" },
    ]);

    const outputs = (await replayFrames("ses_pruned_later", "inbox-pruned-later-viewer")).flatMap(
      (frame) =>
        frame.kind === "tool-result"
          ? [z.object({ output: z.string() }).parse(frame.result).output]
          : []
    );
    expect(outputs).toEqual(["export {};"]);
    await fixture.handlers.get("session_shutdown")?.({}, resumed);
  }, 30_000);

  // An in-process `/resume` takes up a session the same way: its viewer reads its transcript's
  // turns, and none of the session the host left.
  test("a session the host resumes by a switch replays its own transcript's turns alone", async () => {
    globalThis.fetch = async (input, init) => responseWithRegistration(input, init, []);
    const { default: envoyExtension }: EnvoyModule = await import(
      "./envoy.ts?agent-stream-resumed-switch"
    );
    const fixture = createPi();
    await envoyExtension(fixture.pi);
    const left = sessionContext("ses_left");
    await fixture.handlers.get("session_start")?.({}, left);
    const leftTurn = "a turn of the session the host left";
    await fixture.handlers.get("message_end")?.(
      {
        message: { content: [{ text: leftTurn, type: "text" }], role: "assistant", timestamp: 5 },
      },
      left
    );
    const sent = "33333333-3333-4333-8333-333333333333";
    const transcript = resumedTranscript(sent);
    const base = sessionContext("ses_switched_to");
    const switched: SessionContext = {
      ...base,
      sessionManager: {
        ...base.sessionManager,
        getBranch: () => transcript,
        getEntries: () => transcript,
      },
    };
    await fixture.handlers.get("session_switch")?.({ reason: "resume" }, switched);

    expect(await viewerReplay("ses_switched_to", "inbox-switched-viewer")).toEqual(
      resumedTurns(sent)
    );
    await fixture.handlers.get("session_shutdown")?.({}, switched);
  }, 30_000);

  // R2: a session with no earlier turns replays what it records, and nothing else.
  test("a session that starts fresh replays only the turns it records", async () => {
    globalThis.fetch = async (input, init) => responseWithRegistration(input, init, []);
    const { default: envoyExtension }: EnvoyModule = await import(
      "./envoy.ts?agent-stream-fresh-start"
    );
    const fixture = createPi();
    await envoyExtension(fixture.pi);
    const base = sessionContext("ses_fresh");
    const fresh: SessionContext = {
      ...base,
      sessionManager: { ...base.sessionManager, getBranch: () => [], getEntries: () => [] },
    };
    await fixture.handlers.get("session_start")?.({}, fresh);
    await fixture.handlers.get("message_end")?.(
      {
        message: {
          content: [{ text: "The first turn.", type: "text" }],
          role: "assistant",
          timestamp: 7,
        },
      },
      fresh
    );

    expect(await viewerReplay("ses_fresh", "inbox-fresh-viewer")).toEqual([
      { dispatchMessageId: undefined, id: "a7", role: "assistant" },
    ]);
    await fixture.handlers.get("session_shutdown")?.({}, fresh);
  }, 30_000);

  // A ring a session holds does not say its transcript was read back: the host can record a turn
  // under a session before a pass has taken that session up, and that ring then holds the one turn
  // and none before it. Here the host leaves A for B, records A's next turn with no event for the
  // move back, and only then does a pass take A up again.
  test("a session whose ring holds a turn recorded before its pass still replays its earlier turns", async () => {
    globalThis.fetch = async (input, init) => responseWithRegistration(input, init, []);
    const { default: envoyExtension }: EnvoyModule = await import(
      "./envoy.ts?agent-stream-ring-before-the-pass"
    );
    const fixture = createPi();
    await envoyExtension(fixture.pi);
    const earlier = [
      {
        id: "e0",
        message: {
          content: [{ text: "A turn of its own, before the move.", type: "text" }],
          role: "assistant",
          timestamp: 10,
        },
        type: "message",
      },
    ];
    const base = sessionContext("ses_rolled_back");
    const rolledBack: SessionContext = {
      ...base,
      sessionManager: {
        ...base.sessionManager,
        getBranch: () => earlier,
        getEntries: () => earlier,
      },
    };
    await fixture.handlers.get("session_start")?.({}, rolledBack);
    // The host moves to another session, and this one's ring is dropped: nobody watches it.
    await fixture.handlers.get("session_switch")?.({ reason: "new" }, sessionContext("ses_other"));
    // It is back, with no event, and records a turn: that turn makes the ring again.
    await fixture.handlers.get("message_end")?.(
      {
        message: {
          content: [{ text: "A turn after the move back.", type: "text" }],
          role: "assistant",
          timestamp: 20,
        },
      },
      rolledBack
    );
    // The pass that learns of the move takes the session up.
    await fixture.handlers.get("session_switch")?.({ reason: "new" }, rolledBack);

    expect(await viewerReplay("ses_rolled_back", "inbox-rolled-back-viewer")).toEqual([
      { dispatchMessageId: undefined, id: "a10", role: "assistant" },
      { dispatchMessageId: undefined, id: "a20", role: "assistant" },
    ]);
    await fixture.handlers.get("session_shutdown")?.({}, rolledBack);
  }, 30_000);

  // A move away and back leaves the view where it was: a viewer's watch keeps the ring, the
  // session is filled once per ring (`restoredAgentStreams`), and a fill into a ring that already
  // holds the session's turns keeps each of them where it is (`AgentStreamPublisher.restore`). So
  // a turn the bound dropped stays dropped, and the turn this process recorded is still the
  // newest.
  test("a session taken up again while its ring is watched replays the turns the ring holds", async () => {
    globalThis.fetch = async (input, init) => responseWithRegistration(input, init, []);
    const { default: envoyExtension }: EnvoyModule = await import(
      "./envoy.ts?agent-stream-filled-once"
    );
    const fixture = createPi();
    await envoyExtension(fixture.pi);
    const { historyMessages } = AGENT_STREAM_LIMITS;
    const say = (at: number) => ({
      content: [{ text: `turn ${at}`, type: "text" }],
      role: "assistant",
      timestamp: at,
    });
    const transcript = Array.from({ length: historyMessages }, (_, index) => ({
      id: `e${index}`,
      message: say(index + 1),
      type: "message",
    }));
    const base = sessionContext("ses_filled_once");
    const watched: SessionContext = {
      ...base,
      sessionManager: {
        ...base.sessionManager,
        getBranch: () => transcript,
        getEntries: () => transcript,
      },
    };
    await fixture.handlers.get("session_start")?.({}, watched);
    expect(await viewerReplay("ses_filled_once", "inbox-filled-first")).toHaveLength(
      historyMessages
    );
    // One more turn: the ring drops the first.
    await fixture.handlers.get("message_end")?.({ message: say(historyMessages + 1) }, watched);
    // The host leaves and comes back; the viewer's watch keeps the ring.
    await fixture.handlers.get("session_switch")?.({ reason: "new" }, sessionContext("ses_away"));
    await fixture.handlers.get("session_switch")?.({ reason: "resume" }, watched);

    const replayed = await viewerReplay("ses_filled_once", "inbox-filled-again");
    expect(replayed).toHaveLength(historyMessages);
    expect(replayed.slice(0, 2).map((row) => ("id" in row ? row.id : row.call))).toEqual([
      "a2",
      "a3",
    ]);
    expect(replayed.at(-1)).toEqual({
      dispatchMessageId: undefined,
      id: `a${historyMessages + 1}`,
      role: "assistant",
    });
    await fixture.handlers.get("session_shutdown")?.({}, watched);
  }, 30_000);

  test("natsCredential is kept renewed for the connection's life, and the keeper stops at shutdown", async () => {
    // Two-second tokens, so the keeper renews within the test: each renewal lands a new token,
    // and the connection reconnects to present it before the old one lapses. Shutdown stops it.
    const tokenFile = join(configDirectory, "projected-token");
    writeFileSync(tokenFile, `${MACHINE_IDENTITY_TOKEN}\n`);
    process.env.NATS_AUTH = "broker";
    process.env.AGENT_SECRETS_URL = "http://broker.test";
    process.env.AGENT_SECRETS_MACHINE_IDENTITY = "kubernetes";
    process.env.AGENT_SECRETS_MACHINE_TOKEN_FILE = tokenFile;
    const minted: string[] = [];
    globalThis.fetch = async (input, init) => {
      const request = new Request(input, init);
      if (new URL(request.url).host !== "broker.test")
        return responseWithRegistration(input, init, []);
      const token = fixtureSecret("nats");
      minted.push(token);
      return Response.json({
        access_token: token,
        issued_token_type: "urn:ietf:params:oauth:token-type:access_token",
        token_type: "DPoP",
        expires_in: 2,
      });
    };
    const { default: envoyExtension }: EnvoyModule = await import(
      "./envoy.ts?nats-credential-keeper"
    );
    const fixture = createPi();
    await envoyExtension(fixture.pi);
    // The keeper renews on a timer of this process, a second after each two-second token lands
    // (`renewDelay`), and each trade first reads the pod's token from disk (`projectedToken`'s
    // `readFile`), real I/O that runs off the event loop. On the real clock a trade could outlast
    // the shutdown and reach the broker after it: a renewal in flight as the keeper stopped, or
    // the deregistration's own trade, which its credential waits for 500 ms of the shutdown's 1 s
    // (`SHUTDOWN_DEADLINE_MS`) before the DELETE goes without it. So the host's speed decided the
    // row, and it runs on the test's clock (`onPassClock`). The clock is held through the start
    // (`wait`, `waitOn`, which no count of turns bounds) but for one move of 1 ms: the start's
    // pass follows the session (`followNatsCredential`), which with two-second tokens trades and
    // reconnects, and the stand-in server answers the reconnect on a 0 ms timer, which only a move
    // of the clock runs, made once the follow's trade is done. All the keeper then waits on is its
    // timer: the clock moves 100 ms at a time until the keeper's renewal arms its bound, and is
    // held from that step on, through the renewal's read and the reconnect. The session shuts
    // down there, the clock still held: the keeper's next renewal is a second away, so none is in
    // flight, and the deregistration's trade reaches the broker before the shutdown ends. The
    // clock then moves 10 s, five token lifetimes, 100 ms at a time, each step held on real
    // turns: a keeper that had not stopped would renew about once a second there. No other timer
    // matters: the bounds of the start, its follow, each trade, each listener call and the
    // shutdown end only work that is done by then, and this context's `setInterval` runs no
    // heartbeat.
    await onPassClock(async (clock) => {
      /** The renewals the keeper has begun, each by the bound it arms as its timer ends
       *  (`TRADE_DEADLINE_MS`). The start's ready and its follow arm theirs before the start ends;
       *  after it nothing else arms one: the keeper's reconnect presents a live token, so its
       *  CONNECT starts no trade of its own. */
      const renewals = () => clock.armed.filter((ms) => ms === TRADE_DEADLINE_MS).length;
      let started = false;
      const starting = Promise.resolve(
        fixture.handlers.get("session_start")?.({}, sessionContext("ses_kept"))
      ).finally(() => {
        started = true;
      });
      await clock.wait(
        "the start's follow",
        () => natsState.timeline.includes("reconnect") || started
      );
      await clock.advance(1);
      await clock.waitOn("the start", starting);
      const [first] = minted;
      expect(first).toBeString();
      expect(natsState.presented[0]).toBe(first);
      // What the start did is done; from here a reconnect is the keeper's.
      natsState.timeline.length = 0;
      const before = natsState.presented.length;
      // The keeper renews before the two-second token lapses, and the connection reconnects to
      // present the new one.
      const begun = renewals();
      for (let step = 0; step < 20 && renewals() === begun; step += 1) await clock.advance(100);
      if (renewals() === begun) throw new Error("the keeper began no renewal in the token's 2 s");
      await clock.wait("the keeper's reconnect", () => natsState.timeline.includes("reconnect"));
      expect(natsState.timeline).toContain("reconnect");
      expect(natsState.presented.length).toBeGreaterThan(before);
      expect(natsState.presented.at(-1)).not.toBe(natsState.presented[before - 1]);
      await clock.waitOn(
        "the shutdown",
        fixture.handlers.get("session_shutdown")?.({}, sessionContext("ses_kept"))
      );
      // Stopped, it trades and reconnects no more.
      const reconnects = natsState.timeline.filter((entry) => entry === "reconnect").length;
      const traded = minted.length;
      await clock.steps(10_000, 100);
      expect(natsState.timeline.filter((entry) => entry === "reconnect").length).toBe(reconnects);
      expect(minted).toHaveLength(traded);
    });
    // What can hang is the wait for the start's follow, the start, the keeper's reconnect or the
    // shutdown (`clock.wait`, `clock.waitOn`), which has no bound of turns, because each waits on
    // a trade that reads the pod's token from disk, real I/O.
  });
});
