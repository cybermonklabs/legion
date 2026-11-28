// The rows on the broker trade a session's NATS dial presents: a broker that answers no trade,
// answers 503 or grants late, a broker token the hub refuses, the token's renewal for the
// connection's life, and the token a session switch moves to, with what a viewer of a session is
// replayed after a switch, a rollback or a resume. They have a file of their own so that
// `nats-start.test.ts` stays under 1,000 lines. Their `describe` is "envoy OMP extension", the one
// `envoy.test.ts` uses, so a row's full name is the same in every file. The files share one
// harness (`../test/extension-harness`).
import { describe, expect, spyOn, test } from "bun:test";
import { writeFileSync } from "node:fs";
import { join } from "node:path";
import { AGENT_STREAM_LIMITS } from "@legion/contracts";
import { NATS_DIAL_TOKEN_DROPPED } from "@legion/envoy-client/nats-connection";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { holdDeadlines } from "@legion/test-fixtures/held-deadlines";
import { settled, span, until } from "@legion/test-fixtures/wait";
import { logger } from "@oh-my-pi/pi-utils";
import { ErrorCode, NatsError } from "nats";
import { z } from "zod";
import { followBoundMs, startTradeBoundMs } from "../src/start-bound";
import type { EnvoyModule } from "../test/envoy-module";
import {
  configDirectory,
  createPi,
  sessionContext,
  setNatsConnectForTests,
  settle,
  useExtensionHarness,
  withEnvoyTimings,
} from "../test/extension-harness";
import {
  natsStandIn,
  natsState,
  responseWithRegistration,
  standInFetch,
} from "../test/listener-stand-in";
import type { SessionContext } from "../test/stand-in-host";

useExtensionHarness();

/** A pod's projected service-account token, which this machine trades at a stub broker. */
const MACHINE_IDENTITY_TOKEN = fixtureSecret("machine");

/** The request a stand-in `fetch` was handed, from whichever form its first argument took. */
const requestOf = (input: string | URL | Request, init?: RequestInit): Request =>
  input instanceof Request ? new Request(input, init) : new Request(input.toString(), init);

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
      globalThis.fetch = standInFetch(async (input, init) => {
        const request = requestOf(input, init);
        if (new URL(request.url).host !== "broker.test")
          return responseWithRegistration(input, init, []);
        await request.text();
        return new Response("the broker is away", { status: 503 });
      });
      const notifications: string[] = [];
      const intervals: { callback: () => void; intervalMs: number }[] = [];
      const context: SessionContext = {
        ...sessionContext("ses_broker_away"),
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
      // there and says so: a heartbeat that says nothing by the row's deadline has hung, and is
      // named.
      await until(
        "the heartbeat's notice of what its pass came to",
        () => notifications.length > before
      );
      expect(notifications.slice(before)).toEqual([
        expect.stringContaining("envoy: registry heartbeat failed"),
      ]);
      expect(notifications.some((message) => message.includes("did not settle within"))).toBe(
        false
      );
      await fixture.handlers.get("session_shutdown")?.({}, context);
    });
  });

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
    globalThis.fetch = standInFetch(async (input, init) => {
      const request = requestOf(input, init);
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
    });
    return { minted };
  }

  /** A session's context whose notices are kept in the order they were made. */
  function noticeContext(sessionID: string) {
    const notifications: string[] = [];
    const context: SessionContext = {
      ...sessionContext(sessionID),
      ui: { ...sessionContext().ui, notify: (message) => notifications.push(message) },
    };
    return { context, notifications };
  }

  /** Keeps what `console.warn` is handed, where the token client's retry loop says each trade it
   *  makes again (`trading again in <n> s`); `restore` puts the real one back. */
  function keptWarnings() {
    const said: string[] = [];
    const spy = spyOn(console, "warn").mockImplementation((...line: unknown[]) => {
      said.push(line.map(String).join(" "));
    });
    return { said, restore: () => spy.mockRestore() };
  }

  test("a broker that never grants: the start fails at its 20 s bound, naming the 503, inside its pass", async () => {
    // The start waits for ready within its bound (`TRADE_DEADLINE_MS`, 20 s), through the retry
    // loop's retries, and then fails naming what the last trade came to, before the pass's
    // deadline: a pass abandoned at that deadline names no cause at all. The deadline is its floor
    // here, 30 s (`MIN_PASS_DEADLINE_MS`, the heartbeat shortened to it), so the start's bound is
    // the pass less the rest the pass needs (`startTradeBoundMs`), the 20 s. No clock decides it:
    // the row holds that bound (`holdDeadlines`) and ends it once the retry loop has said it
    // trades again after the first 503, by which time a start that did not wait for ready has been
    // told the 503 (`BrokerTokenClient` tells a fail-fast caller before it says so). Until the
    // bound ends the start names nothing; at the bound it names the 503, and no pass is abandoned.
    // The start's own notice (`this session is not registered`) is then the one notice that names
    // the trade, once: the outage notice its refused registration owes (`inbound messages are
    // unavailable`) waits out the start race's bound (`INBOUND_FIRST_OPEN_BOUND_MS`), which has
    // not passed when the start's pass ends, so it is not given inside that pass.
    const passMs = 30_000;
    await withEnvoyTimings({ heartbeatMs: passMs, passDeadlineMs: passMs }, async () => {
      brokerPod(() => "503");
      const bound = holdDeadlines(startTradeBoundMs(passMs));
      const warnings = keptWarnings();
      try {
        const { default: envoyExtension }: EnvoyModule = await import(
          "./envoy.ts?broker-answers-503"
        );
        const fixture = createPi();
        await envoyExtension(fixture.pi);
        const { context, notifications } = noticeContext("ses_broker_503");
        let ended = false;
        const start = Promise.resolve(fixture.handlers.get("session_start")?.({}, context)).then(
          () => {
            ended = true;
          }
        );
        await until(
          "the start's bound armed, and the retry loop's word that it trades again",
          () =>
            bound.held.length > 0 && warnings.said.some((line) => line.includes("trading again"))
        );
        await settle();
        const named = () =>
          notifications.filter((message) => message.includes("the trade for the session's token"));
        expect(ended, "the start ended before its bound").toBe(false);
        expect(named(), "a notice named the trade before the start's bound").toEqual([]);
        bound.end();
        await start;
        const told = notifications.join(" | ");
        const startNotices = notifications.filter((message) =>
          message.startsWith("envoy: this session is not registered (")
        );
        expect(startNotices, `the start's notice was not given once: ${told}`).toHaveLength(1);
        expect(startNotices[0], "the start's notice does not name the trade").toContain(
          "the trade for the session's token"
        );
        expect(startNotices[0]).toContain("HTTP 503");
        expect(named(), "a notice other than the start's named the trade").toEqual(startNotices);
        expect(notifications.some((message) => message.includes("did not settle within"))).toBe(
          false
        );
        await fixture.handlers.get("session_shutdown")?.({}, context);
      } finally {
        warnings.restore();
        bound.restore();
      }
    });
  });

  test("a first trade that answers 503 and then grants: the start presents its token in its first pass", async () => {
    // The start has nothing to do until it holds a token, so it waits for ready: the retry
    // loop's next trade, 15 s at most later, grants, and the session's first pass dials and
    // registers with it, rather than waiting for the next heartbeat, 90 s on.
    const { minted } = brokerPod((trade) => (trade === 1 ? "503" : "grant"));
    const { context, notifications } = noticeContext("ses_broker_once_away");
    const { default: envoyExtension }: EnvoyModule = await import("./envoy.ts?broker-away-once");
    const fixture = createPi();
    await envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, context);
    expect(
      natsState.presented.map((token) => minted.get(token)),
      `the start presented no token: ${notifications.join(" | ")}`
    ).toEqual(["ses_broker_once_away"]);
    expect(natsState.controls.get("notifications.agent.ses_broker_once_away")?.active()).toBe(true);
    expect(
      notifications.some((message) => message.includes("the trade for the session's token"))
    ).toBe(false);
    await fixture.handlers.get("session_shutdown")?.({}, context);
  });

  test("a first NATS dial whose broker token the hub refuses twice drops it, starts another trade, and says so", async () => {
    // The pass's dials share the keeper's count of the token's refusals (`connectNats`'
    // `hooks.credential`), so the dial drops a token the hub refused twice, as the keeper does on
    // an open connection. The waits between the dials are recorded, not slept.
    const { minted } = brokerPod(() => "grant");
    natsState.failConnects = 2;
    natsState.failError = () =>
      new NatsError("'Authorization Violation'", ErrorCode.AuthorizationViolation);
    setNatsConnectForTests(
      natsStandIn,
      { firstDelayMs: 5, maxDelayMs: 5, budgetMs: 30_000 },
      { delay: async () => {}, random: () => 0 }
    );
    const warnings: string[] = [];
    const stopSink = logger.registerLogSink((entry) => {
      if (entry.level === "warn") warnings.push(entry.message);
    });
    try {
      const { context, notifications } = noticeContext("ses_first_dial_refused");
      const { default: envoyExtension }: EnvoyModule = await import(
        "./envoy.ts?first-dial-refused-twice"
      );
      const fixture = createPi();
      await envoyExtension(fixture.pi);
      await fixture.handlers.get("session_start")?.({}, context);
      expect(natsState.failConnects, notifications.join(" | ")).toBe(0);
      // The start's token, and the trade the drop started.
      await until("the trade the drop started", () => minted.size === 2);
      expect([...minted.values()]).toEqual(["ses_first_dial_refused", "ses_first_dial_refused"]);
      expect(warnings.filter((message) => message.includes(NATS_DIAL_TOKEN_DROPPED))).toHaveLength(
        1
      );
      await fixture.handlers.get("session_shutdown")?.({}, context);
    } finally {
      stopSink();
      setNatsConnectForTests(natsStandIn);
    }
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
    globalThis.fetch = standInFetch(async (input, init) => {
      const request = requestOf(input, init);
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
    });
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
    const { context, notifications } = noticeContext("ses_keeper_notice");
    const { default: envoyExtension }: EnvoyModule = await import("./envoy.ts?keeper-notice");
    const fixture = createPi();
    await envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, context);
    expect(natsState.presented.map((token) => minted.get(token))).toEqual(["ses_keeper_notice"]);
    const refused = () =>
      notifications.filter((message) =>
        message.includes("refused the connection's live broker token")
      );
    for (const read of natsState.statusReaders) {
      read({ type: "error", data: "AUTHORIZATION_VIOLATION" });
    }
    // The reader's warning is this process's own work, done within turns of the push.
    await until("the keeper's notice", () => refused().length > 0);
    expect(refused()).toEqual([
      "envoy: the NATS server refused the connection's live broker token (AUTHORIZATION_VIOLATION): the auth service is away, or the token was revoked; the connection keeps redialling, and at the second refusal the token is dropped and another traded",
    ]);
    await fixture.handlers.get("session_shutdown")?.({}, context);
  });

  test("a switch whose new token never comes fails inside a short pass, naming the trade", async () => {
    // ENVOY_HEARTBEAT_MS=10000 ENVOY_PASS_DEADLINE_MS=10000: the follow takes at most 8 s of the
    // pass (`followBoundMs`), its trade 4 s of that, so the pass names what the trade came to
    // rather than being abandoned at its deadline with no word of the cause (the review of
    // 2fa6fbf7cf, m3: unbounded, the trade alone waited 20 s). No clock decides it: the row holds
    // the follow's trade share, half of its bound (`holdDeadlines`), and ends it once the broker
    // has refused the new session's trade and the retry loop has said it trades again. Until then
    // nothing names the follow's trade; at the share's end the notice names the 503, and no pass
    // is abandoned. A follow whose trade took the whole pass would arm no such share, and the row
    // would then wait for a notice that never comes and fail at its deadline.
    const passMs = 10_000;
    await withEnvoyTimings({ heartbeatMs: passMs, passDeadlineMs: passMs }, async () => {
      brokerPod((trade) => (trade === 1 ? "grant" : "503"));
      const share = holdDeadlines(followBoundMs(passMs) / 2);
      const warnings = keptWarnings();
      try {
        const before = noticeContext("ses_follow_from");
        const { default: envoyExtension }: EnvoyModule = await import("./envoy.ts?follow-bound");
        const fixture = createPi();
        await envoyExtension(fixture.pi);
        await fixture.handlers.get("session_start")?.({}, before.context);
        const after = noticeContext("ses_follow_to");
        // The start's own listener calls arm shares of the same length, which their credential
        // answers at once: the follow's is the one the switch arms after them.
        const startShares = share.held.length;
        // The switch's pass runs the follow, so the row ends the share while the switch is under
        // way, and only then waits for the switch.
        const switching = Promise.resolve(
          fixture.handlers.get("session_switch")?.({}, after.context)
        );
        const all = () => [...before.notifications, ...after.notifications];
        const named = () =>
          all().filter((message) => message.includes("could not take the new session's token"));
        await until(
          "the follow's trade share armed, and the retry loop's word that it trades again",
          () =>
            share.held.length > startShares &&
            warnings.said.some((line) => line.includes("trading again"))
        );
        await settle();
        expect(named(), "a notice named the follow's trade before its share ended").toEqual([]);
        // Let go rather than ended alone: the pass's listener calls after the follow arm shares of
        // the same length, which run as they would.
        share.letGo();
        await settled("the switch once the follow's trade share ended", switching);
        await until("a notice naming the follow's trade", () => named().length > 0);
        expect(named()[0]).toContain("HTTP 503");
        expect(all().some((message) => message.includes("did not settle within"))).toBe(false);
        await fixture.handlers.get("session_shutdown")?.({}, after.context);
      } finally {
        warnings.restore();
        share.restore();
      }
    });
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
    globalThis.fetch = standInFetch(async (input, init) => {
      const request = requestOf(input, init);
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
    });
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
    // The switch's pass closes the old session's control subject before the switch returns (the
    // close runs inside the pass the switch awaits): a close put off past it fails here.
    expect(natsState.controls.get(oldControl)?.active()).toBe(false);
    expect(natsState.controls.has("agentstream.ses_failed_to.control")).toBe(false);
    const marker = "a turn of the session switched to";
    await fixture.handlers.get("message_end")?.(
      { message: { content: [{ text: marker, type: "text" }], role: "assistant", timestamp: 30 } },
      next
    );

    // A viewer of the old session asks for its history right after the new session's turn: a
    // control subject still open then would answer the replay with it.
    const control = natsState.controls.get(oldControl);
    control?.push(JSON.stringify({ type: "watch", v: 1 }));
    control?.push(JSON.stringify({ type: "replay", v: 1 }), "inbox-old-viewer");
    await settle();
    const replayed = natsState.published
      .filter((each) => each.subject === "inbox-old-viewer")
      .map((each) => new TextDecoder().decode(each.data));
    expect(replayed.join("\n")).not.toContain(marker);
    expect(natsState.controls.get(oldControl)?.active()).toBe(false);
    await fixture.handlers.get("session_shutdown")?.({}, next);
  });

  test("after a rollback with no event, a viewer of the session left is replayed nothing of the one returned to", async () => {
    // Two sessions of one host: it switches from A to B, B's viewer attaches, and the host then
    // returns to A with no event. Until the next pass runs, A's control subject is closed and B's
    // is still open; what the host records then is A's, and B's viewer must be replayed only B's.
    globalThis.fetch = standInFetch(async (input, init) =>
      responseWithRegistration(input, init, [])
    );
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
    // A replay is one answer, every frame of it in one message (`AgentStreamReplay`).
    const answer = await until("the replay's answer to B's viewer", () =>
      natsState.published.find((each) => each.subject === "inbox-viewer-of-b")
    );
    const replayed = new TextDecoder().decode(answer.data);
    // The replay can carry a turn at all: B's own is in it.
    expect(replayed).toContain(ownTurn);
    expect(replayed).not.toContain(returnedTurn);
    await fixture.handlers.get("session_shutdown")?.({}, first);
  });

  /** The frames a viewer that attaches to `session` is replayed, in the order the answer gives
   *  them. */
  const replayFrames = async (session: string, inbox: string) => {
    const control = natsState.controls.get(`agentstream.${session}.control`);
    expect(control?.active()).toBe(true);
    control?.push(JSON.stringify({ type: "watch", v: 1 }));
    control?.push(JSON.stringify({ type: "replay", v: 1 }), inbox);
    const answered = () => natsState.published.filter((each) => each.subject === inbox);
    await until(`the replay answer on ${inbox}`, () => answered().length > 0);
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
    globalThis.fetch = standInFetch(async (input, init) => {
      const request = requestOf(input, init);
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
    });
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
    await until("the keeper's reconnect with a renewed token", () =>
      natsState.timeline.includes("reconnect")
    );
    expect(natsState.presented.length).toBeGreaterThan(before);
    expect(natsState.presented.at(-1)).not.toBe(natsState.presented[before - 1]);
    await fixture.handlers.get("session_shutdown")?.({}, sessionContext("ses_kept"));
    // Stopped, it trades and reconnects no more.
    const reconnects = natsState.timeline.filter((entry) => entry === "reconnect").length;
    const traded = minted.length;
    await span(
      2_500,
      "the keeper's renewal timer is its own and clearing it leaves no state to read, and no " +
        "later action's effect is ordered after its next tick: one renewal of a 2 s token runs in it"
    );
    expect(natsState.timeline.filter((entry) => entry === "reconnect").length).toBe(reconnects);
    expect(minted).toHaveLength(traded);
  });
});
