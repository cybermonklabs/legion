// The rows on the broker trade a session's NATS dial presents: a broker that answers no trade,
// answers 503 or grants late, a broker token the hub refuses, the token's renewal for the
// connection's life, and the token a session switch moves to, with what a viewer of the session
// left is replayed after a switch or a rollback. They have a file of their own so that
// `nats-start.test.ts` stays under 1,000 lines. Their `describe` is "envoy OMP extension", the one
// `envoy.test.ts` uses, so a row's full name is the same in every file. The files share one
// harness (`../test/extension-harness`).
import { describe, expect, test } from "bun:test";
import { writeFileSync } from "node:fs";
import { join } from "node:path";
import { NATS_DIAL_TOKEN_DROPPED } from "@legion/envoy-client/nats-connection";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { logger } from "@oh-my-pi/pi-utils";
import { ErrorCode, NatsError } from "nats";
import type { EnvoyModule } from "../test/envoy-module";
import {
  configDirectory,
  createPi,
  sessionContext,
  setNatsConnectForTests,
  topLevelSession,
  until,
  useExtensionHarness,
  withEnvoyTimings,
} from "../test/extension-harness";
import {
  natsStandInConnect,
  natsState,
  responseWithRegistration,
  rowFetch,
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
    // with the heartbeat shortened to it, since a deadline is never shorter than the heartbeat. The
    // queue's own timer is then the row's bound, on the same loop as the pass: a pass that
    // outlasts it is abandoned, and the session is told so.
    const passMs = 30_000;
    await withEnvoyTimings({ heartbeatMs: passMs, passDeadlineMs: passMs }, async () => {
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
        await request.text();
        return new Response("the broker is away", { status: 503 });
      };
      const notifications: string[] = [];
      const intervals: { callback: () => void; intervalMs: number }[] = [];
      const context: SessionContext = {
        cwd: "/tmp/envoy-omp-test",
        sessionManager: { ...topLevelSession, getSessionId: () => "ses_broker_away" },
        setInterval: (callback, intervalMs) => intervals.push({ callback, intervalMs }),
        // The one extra call a failed heartbeat arms is not run: the row reads the heartbeat.
        setTimeout: () => undefined,
        ui: { ...sessionContext().ui, notify: (message) => notifications.push(message) },
      };
      const { default: envoyExtension }: EnvoyModule = await import(
        "./envoy.ts?broker-answers-no-trade"
      );
      const fixture = createPi();
      await envoyExtension(fixture.pi);
      await fixture.handlers.get("session_start")?.({}, context);
      expect(
        notifications.some((message) => message.includes("the trade for the session's token"))
      ).toBe(true);
      // The next heartbeat pass, while the broker is still away, as a pod's heartbeat runs it: the
      // heartbeat's own interval, whose tick says what its pass came to, a failure or a pass the
      // queue abandoned at its deadline. The 15 s retry the start's failure arms says neither, so a
      // row that ran it saw nothing of its pass.
      const heartbeat = intervals.find(({ intervalMs }) => intervalMs === passMs);
      if (heartbeat === undefined) throw new Error("the heartbeat interval was not registered");
      const before = notifications.length;
      heartbeat.callback();
      // Its pass ends inside the queue's 30 s deadline, the trade failing at 20 s, or is abandoned
      // there and says so: a heartbeat that says nothing a minute on has hung, and is named.
      await until(
        () => notifications.length > before,
        "the heartbeat's notice of what its pass came to",
        60_000
      );
      expect(notifications.slice(before)).toEqual([
        expect.stringContaining("envoy: registry heartbeat failed"),
      ]);
      expect(notifications.some((message) => message.includes("did not settle within"))).toBe(
        false
      );
      await fixture.handlers.get("session_shutdown")?.({}, context);
    });
  }, 90_000);

  /** A pod whose NATS connection presents its broker token, at a broker that answers each trade
   *  as `answer(n)` says for its n-th: a 503, or a token it mints for the trade's session. */
  function brokerPod(answer: (trade: number) => "503" | "grant"): { minted: Map<string, string> } {
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
    return { minted };
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

  test("a broker that never grants: the start fails at its 20 s bound, naming the 503, inside its pass", async () => {
    // The start waits for ready within its bound (`TRADE_DEADLINE_MS`, 20 s), through the retry
    // loop's retries, and then fails naming what the last trade came to, before the pass's
    // deadline: a pass abandoned at that deadline names no cause at all. The deadline is its floor
    // here, 30 s (`MIN_PASS_DEADLINE_MS`, the heartbeat shortened to it), and the queue's own
    // timer is the row's upper bound, on the same loop as the start's: a start that outlasts it is
    // abandoned, and the session is told that instead of the trade.
    const passMs = 30_000;
    await withEnvoyTimings({ heartbeatMs: passMs, passDeadlineMs: passMs }, async () => {
      brokerPod(() => "503");
      const { default: envoyExtension }: EnvoyModule = await import(
        "./envoy.ts?broker-answers-503"
      );
      const fixture = createPi();
      await envoyExtension(fixture.pi);
      // Timed from the start itself, not from before the module loads: a module slow to load on a
      // loaded machine is no slower start.
      const { context, notifications } = timedContext("ses_broker_503", Date.now());
      await fixture.handlers.get("session_start")?.({}, context);
      const notice = notifications.find(({ message }) =>
        message.includes("the trade for the session's token")
      );
      expect(
        notice,
        `no notice named the trade: ${notifications.map(({ message }) => message).join(" | ")}`
      ).toBeDefined();
      expect(notice?.message).toContain("HTTP 503");
      // A timer fires no sooner than it is due, so the start waited out its bound however loaded
      // the machine.
      expect(notice?.at).toBeGreaterThanOrEqual(19_000);
      expect(notifications.some(({ message }) => message.includes("did not settle within"))).toBe(
        false
      );
      await fixture.handlers.get("session_shutdown")?.({}, context);
    });
  }, 60_000);

  test("a first trade that answers 503 and then grants: the start presents its token in its first pass", async () => {
    // The start has nothing to do until it holds a token, so it waits for ready: the retry
    // loop's next trade, 15 s at most later, grants, and the session's first pass dials and
    // registers with it, rather than waiting for the next heartbeat, 90 s on.
    const { minted } = brokerPod((trade) => (trade === 1 ? "503" : "grant"));
    const started = Date.now();
    const { context, notifications } = timedContext("ses_broker_once_away", started);
    const { default: envoyExtension }: EnvoyModule = await import("./envoy.ts?broker-away-once");
    const fixture = createPi();
    await envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, context);
    expect(
      natsState.presented.map((token) => minted.get(token)),
      `the start presented no token: ${notifications.map(({ message }) => message).join(" | ")}`
    ).toEqual(["ses_broker_once_away"]);
    expect(natsState.controls.get("notifications.agent.ses_broker_once_away")?.active()).toBe(true);
    expect(
      notifications.some(({ message }) => message.includes("the trade for the session's token"))
    ).toBe(false);
    await fixture.handlers.get("session_shutdown")?.({}, context);
  }, 60_000);

  test("a first NATS dial whose broker token the hub refuses twice drops it, starts another trade, and says so", async () => {
    // The pass's dials share the keeper's count of the token's refusals (`connectNats`'
    // `hooks.credential`), so the dial drops a token the hub refused twice, as the keeper does on
    // an open connection. The waits between the dials are recorded, not slept.
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
    const stopSink = logger.registerLogSink((entry) => {
      if (entry.level === "warn") warnings.push(entry.message);
    });
    try {
      const { context, notifications } = timedContext("ses_first_dial_refused", Date.now());
      const { default: envoyExtension }: EnvoyModule = await import(
        "./envoy.ts?first-dial-refused-twice"
      );
      const fixture = createPi();
      await envoyExtension(fixture.pi);
      await fixture.handlers.get("session_start")?.({}, context);
      expect(natsState.failConnects, notifications.map(({ message }) => message).join(" | ")).toBe(
        0
      );
      // The start's token, and the trade the drop started.
      await until(() => minted.size === 2, "the trade the drop started");
      expect([...minted.values()]).toEqual(["ses_first_dial_refused", "ses_first_dial_refused"]);
      expect(warnings.filter((message) => message.includes(NATS_DIAL_TOKEN_DROPPED))).toHaveLength(
        1
      );
      await fixture.handlers.get("session_shutdown")?.({}, context);
    } finally {
      stopSink();
      setNatsConnectForTests(natsStandInConnect);
    }
  }, 30_000);

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
    await fixture.handlers.get("session_start")?.({}, sessionContext("ses_switch_from"));
    // Each CONNECT presented a token the broker minted for the session it names here.
    expect(natsState.presented.map((token) => minted.get(token))).toEqual(["ses_switch_from"]);
    expect(natsState.controls.get("notifications.agent.ses_switch_from")?.active()).toBe(true);

    natsState.timeline.length = 0;
    await fixture.handlers.get("session_switch")?.({}, sessionContext("ses_switch_to"));
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
    await fixture.handlers.get("session_shutdown")?.({}, sessionContext("ses_switch_to"));
  });

  test("the keeper's warning of a refused token reaches the session as a notice, once", async () => {
    // A token the server refuses though it is live is a session that stops receiving, which the
    // session is told of as every other inbound outage is (the review of 2fa6fbf7cf, M2).
    const { minted } = brokerPod(() => "grant");
    const { context, notifications } = timedContext("ses_keeper_notice", Date.now());
    const { default: envoyExtension }: EnvoyModule = await import("./envoy.ts?keeper-notice");
    const fixture = createPi();
    await envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, context);
    expect(natsState.presented.map((token) => minted.get(token))).toEqual(["ses_keeper_notice"]);
    const refused = () =>
      notifications.filter(({ message }) =>
        message.includes("refused the connection's live broker token")
      );
    for (const read of natsState.statusReaders) {
      read({ type: "error", data: "AUTHORIZATION_VIOLATION" });
    }
    // The reader's warning is this process's own work, done within turns of the push; a bound of
    // two-thirds of the row's time names a hang without failing a machine that stalls.
    await until(() => refused().length > 0, "the keeper's notice", 20_000);
    expect(refused().map(({ message }) => message)).toEqual([
      "envoy: the NATS server refused the connection's live broker token (AUTHORIZATION_VIOLATION): the auth service is away, or the token was revoked; the connection keeps redialling, and at the second refusal the token is dropped and another traded",
    ]);
    await fixture.handlers.get("session_shutdown")?.({}, context);
  }, 30_000);

  test("a switch whose new token never comes fails inside a short pass, naming the trade", async () => {
    // ENVOY_HEARTBEAT_MS=10000 ENVOY_PASS_DEADLINE_MS=10000: the follow takes at most 8 s of the
    // pass (`followBoundMs`), its trade 4 s of that, so the pass names what the trade came to
    // rather than being abandoned at its deadline with no word of the cause (the review of
    // 2fa6fbf7cf, m3: unbounded, the trade alone waited 20 s).
    await withEnvoyTimings({ heartbeatMs: 10_000, passDeadlineMs: 10_000 }, async () => {
      brokerPod((trade) => (trade === 1 ? "grant" : "503"));
      const started = Date.now();
      // When the follow's trade for the new session reached the broker: the follow starts its
      // clock just before it, which the switch's own work before the pass can put well after the
      // row's switch. The pod's fetch is `rowFetch()`: `globalThis.fetch` reads the file's
      // recording wrapper, which calls whatever a row installed, so a wrapper that called it would
      // call itself forever.
      const pod = rowFetch();
      let followTradeAt: number | undefined;
      globalThis.fetch = async (input, init) => {
        const request = new Request(input, init);
        if (new URL(request.url).host === "broker.test") {
          const session = new URLSearchParams(await request.clone().text()).get("session");
          if (session === "ses_follow_to") followTradeAt ??= Date.now() - started;
        }
        return await pod(input, init);
      };
      const before = timedContext("ses_follow_from", started);
      const { default: envoyExtension }: EnvoyModule = await import("./envoy.ts?follow-bound");
      const fixture = createPi();
      await envoyExtension(fixture.pi);
      await fixture.handlers.get("session_start")?.({}, before.context);
      const after = timedContext("ses_follow_to", started);
      const switchedAt = Date.now() - started;
      await fixture.handlers.get("session_switch")?.({}, after.context);
      const all = () => [...before.notifications, ...after.notifications];
      const named = () =>
        all().find(({ message }) => message.includes("could not take the new session's token"));
      await until(() => named() !== undefined, "a notice naming the follow's trade", 45_000);
      expect(named()?.message).toContain("HTTP 503");
      const at = named()?.at ?? Number.POSITIVE_INFINITY;
      // Not before the trade's 4 s share, which no timer ends early: counted from the switch,
      // before the follow started its clock.
      expect(at - switchedAt).toBeGreaterThanOrEqual(3_900);
      // Inside the follow's own 8 s bound (`followBoundMs`), counted from its trade: the 4 s share
      // and as much again for a loaded machine to say so, the share the follow would leave the
      // connection. The pass's own 10 s deadline is the bound the queue keeps: a follow that
      // outlasted it would be abandoned, which says so below.
      expect(followTradeAt).toBeDefined();
      expect(at - (followTradeAt ?? Number.NEGATIVE_INFINITY)).toBeLessThan(8_000);
      expect(all().some(({ message }) => message.includes("did not settle within"))).toBe(false);
      await fixture.handlers.get("session_shutdown")?.({}, after.context);
    });
  }, 60_000);

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
    await fixture.handlers.get("session_start")?.({}, sessionContext("ses_failed_from"));
    const oldControl = "agentstream.ses_failed_from.control";
    expect(natsState.controls.get(oldControl)?.active()).toBe(true);

    const next = sessionContext("ses_failed_to");
    await fixture.handlers.get("session_switch")?.({}, next);
    expect(natsState.controls.has("agentstream.ses_failed_to.control")).toBe(false);
    const marker = "a turn of the session switched to";
    await fixture.handlers.get("message_end")?.(
      { message: { content: [{ text: marker, type: "text" }], role: "assistant", timestamp: 30 } },
      next
    );

    // A viewer of the old session asks for its history.
    const control = natsState.controls.get(oldControl);
    control?.push(JSON.stringify({ type: "watch", v: 1 }));
    control?.push(JSON.stringify({ type: "replay", v: 1 }), "inbox-old-viewer");
    for (let index = 0; index < 20; index += 1) await Promise.resolve();
    await Bun.sleep(50);
    const replayed = natsState.published
      .filter((each) => each.subject === "inbox-old-viewer")
      .map((each) => new TextDecoder().decode(each.data));
    expect(replayed.join("\n")).not.toContain(marker);
    expect(natsState.controls.get(oldControl)?.active()).toBe(false);
    await fixture.handlers.get("session_shutdown")?.({}, next);
  }, 30_000);

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
    for (let index = 0; index < 20; index += 1) await Promise.resolve();
    await Bun.sleep(50);
    const replayed = natsState.published
      .filter((each) => each.subject === "inbox-viewer-of-b")
      .map((each) => new TextDecoder().decode(each.data))
      .join("\n");
    // The replay can carry a turn at all: B's own is in it.
    expect(replayed).toContain(ownTurn);
    expect(replayed).not.toContain(returnedTurn);
    await fixture.handlers.get("session_shutdown")?.({}, first);
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
    await fixture.handlers.get("session_start")?.({}, sessionContext("ses_kept"));
    const [first] = minted;
    expect(first).toBeString();
    expect(natsState.presented[0]).toBe(first);
    // What the start did is done; from here a reconnect is the keeper's.
    natsState.timeline.length = 0;
    const before = natsState.presented.length;
    const deadline = Date.now() + 5_000;
    while (!natsState.timeline.includes("reconnect") && Date.now() < deadline) await Bun.sleep(50);
    expect(natsState.timeline).toContain("reconnect");
    expect(natsState.presented.length).toBeGreaterThan(before);
    expect(natsState.presented.at(-1)).not.toBe(natsState.presented[before - 1]);
    await fixture.handlers.get("session_shutdown")?.({}, sessionContext("ses_kept"));
    // Stopped, it trades and reconnects no more.
    const reconnects = natsState.timeline.filter((entry) => entry === "reconnect").length;
    const traded = minted.length;
    await Bun.sleep(2_500);
    expect(natsState.timeline.filter((entry) => entry === "reconnect").length).toBe(reconnects);
    expect(minted).toHaveLength(traded);
  }, 15_000);
});
