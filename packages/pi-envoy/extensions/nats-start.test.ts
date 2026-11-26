// The rows on a session's NATS connection at its start and after it: the first dial and its
// retries, the relay subject and the live view's control subject the session opens on it, and the
// registration that names why a relay is missing. They have a file of their own, which holds the
// rows of this subject and stays under 1,000 lines. Their `describe` is "envoy OMP extension",
// the one `envoy.test.ts` uses, so a row's full name is the same in every file. The rows on the
// broker trade a dial presents are in `nats-broker-trade.test.ts`. All three files share one
// harness (`../test/extension-harness`).
import { describe, expect, setSystemTime, test } from "bun:test";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { deliverSubject } from "@legion/envoy-client/consumer-delivery";
import { natsFailureReason } from "@legion/envoy-client/errors";
import { INBOUND_FIRST_OPEN_BOUND_MS } from "@legion/envoy-client/inbound-outage";
import { NATS_DIAL_PLAN } from "@legion/envoy-client/nats-connection";
import { logger } from "@oh-my-pi/pi-utils";
import {
  type ConnectionOptions,
  ErrorCode,
  NatsError,
  nkeys,
  type connect as realConnect,
} from "nats";
import { version as packageVersion } from "../package.json" with { type: "json" };
import type { EnvoyModule } from "../test/envoy-module";
import {
  createPi,
  forwardedRoleEnvelope,
  sessionContext,
  setNatsConnectForTests,
  settle,
  topLevelSession,
  until,
  useExtensionHarness,
  withEnvoyTimings,
} from "../test/extension-harness";
import { natsStandInConnect, natsState, responseWithRegistration } from "../test/listener-stand-in";
import type { SessionContext } from "../test/stand-in-host";

useExtensionHarness();

describe("envoy OMP extension", () => {
  test("retries NATS in the background when the initial connection fails", async () => {
    globalThis.fetch = async (input, init) => responseWithRegistration(input, init, {});
    natsState.failConnects = 1;
    const notifications: string[] = [];
    const intervals: { callback: () => void; intervalMs: number }[] = [];
    const context: SessionContext = {
      cwd: "/tmp/envoy-omp-test",
      sessionManager: { ...topLevelSession, getSessionId: () => "ses_retry" },
      setInterval: (callback, intervalMs) => intervals.push({ callback, intervalMs }),
      ui: { ...sessionContext().ui, notify: (message) => notifications.push(message) },
    };
    const { default: envoyExtension }: EnvoyModule = await import("./envoy.ts?retry");
    const fixture = createPi();

    await envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, context);

    expect(notifications.some((message) => message.includes("retrying in the background"))).toBe(
      true
    );
    // With no connection the session holds no relay subject, so the listener can give it no
    // consumer and its registration is refused until the retry connects. The relay may still
    // open: the start race is held for its bound (`INBOUND_FIRST_OPEN_BOUND_MS`), so the session
    // is not told that it receives nothing.
    expect(
      notifications.some((message) => message.includes("inbound messages are unavailable"))
    ).toBe(false);
    // The notice that the session is not registered names the dial's own failure, the cause a
    // person can act on (a hub with no auth service under NATS_AUTH=broker says so and what to
    // unset), not only the refusal it led to.
    expect(
      notifications.some((message) =>
        message.includes("because its relay subject could not be opened: CONNECTION_REFUSED")
      )
    ).toBe(true);
    expect(natsState.subscriptions.has("notifications.envoy.deliver.ses_retry")).toBe(false);
    const retry = intervals.at(-1);
    if (retry === undefined) throw new Error("retry interval was not registered");

    retry.callback();
    await new Promise((resolve) => setTimeout(resolve, 0));

    expect(natsState.subscriptions.has("notifications.envoy.deliver.ses_retry")).toBe(true);
    expect(notifications.some((message) => message.includes("this session is registered"))).toBe(
      true
    );
    // The relay opened inside the bound, so the session was told nothing, and it is told nothing
    // about receiving again either.
    expect(notifications.filter((message) => message.includes("inbound messages are"))).toEqual([]);
    const connectionsAfterRecovery = natsState.connectedNames.length;

    retry.callback();
    await new Promise((resolve) => setTimeout(resolve, 0));

    expect(natsState.connectedNames.length).toBe(connectionsAfterRecovery);
  });

  test("a NATS failure notice never names an address the log would not", async () => {
    // A throwaway nkey seed pasted as the NATS server: nats.js's resolver error names it as the
    // host it could not look up, lowercased, and the notice must carry neither form.
    const seed = new TextDecoder().decode(nkeys.createUser().getSeed());
    process.env.ENVOY_NATS_URL = seed;
    globalThis.fetch = async (input, init) => responseWithRegistration(input, init, {});
    natsState.failConnects = 1;
    natsState.failError = () =>
      Object.assign(new Error(`getaddrinfo EAI_AGAIN ${seed.toLowerCase()}`), {
        name: "DNSException",
        code: "EAI_AGAIN",
        syscall: "getaddrinfo",
      });
    const notifications: string[] = [];
    const context: SessionContext = {
      cwd: "/tmp/envoy-omp-test",
      sessionManager: { ...topLevelSession, getSessionId: () => "ses_seed_notice" },
      setInterval: () => {},
      ui: { ...sessionContext().ui, notify: (message) => notifications.push(message) },
    };
    const { default: envoyExtension }: EnvoyModule = await import("./envoy.ts?seed-notice");
    const fixture = createPi();
    await envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, context);
    expect(
      notifications.some((message) =>
        message.includes(
          "because its relay subject could not be opened: getaddrinfo EAI_AGAIN); retrying in the background"
        )
      )
    ).toBe(true);
    expect(notifications.join("\n").toLowerCase()).not.toContain(seed.toLowerCase());
  });

  // The hub closes a dial whose CONNECT came after its 2 s authentication timeout, which a busy
  // start of Oh My Pi causes, and answers the next one: the pass dials again rather than register
  // with no relay.
  test("a first NATS dial that fails once is dialed again, and the session_start pass registers with its relay", async () => {
    globalThis.fetch = async (input, init) => responseWithRegistration(input, init, {});
    natsState.failConnects = 1;
    natsState.failError = () =>
      new NatsError("'Authentication Timeout'", ErrorCode.AuthenticationTimeout);
    setNatsConnectForTests(natsStandInConnect, { firstDelayMs: 5, maxDelayMs: 5, budgetMs: 5_000 });
    try {
      const notifications: string[] = [];
      const context: SessionContext = {
        cwd: "/tmp/envoy-omp-test",
        sessionManager: { ...topLevelSession, getSessionId: () => "ses_redial" },
        setInterval: () => {},
        ui: { ...sessionContext().ui, notify: (message) => notifications.push(message) },
      };
      const { default: envoyExtension }: EnvoyModule = await import("./envoy.ts?redial-first-dial");
      const fixture = createPi();
      await envoyExtension(fixture.pi);
      await fixture.handlers.get("session_start")?.({}, context);

      expect(natsState.failConnects).toBe(0);
      expect(natsState.subscriptions.has("notifications.envoy.deliver.ses_redial")).toBe(true);
      expect(
        notifications.filter((message) => message.includes("inbound messages are unavailable"))
      ).toEqual([]);
      expect(notifications.filter((message) => message.includes("not registered"))).toEqual([]);
    } finally {
      setNatsConnectForTests(natsStandInConnect);
    }
  });

  // A Legion wake after a pod release is a new pod that resumes the session's transcript: its
  // start blocks the event loop as a fresh one does, and its pass also reads the interests the
  // session registered before. The dial the hub closes is dialed again there too.
  test("a session resumed in a new pod, whose first dial the hub closes, is dialed again and registers with its relay in its session_start pass", async () => {
    const registrations: { readonly consumer: boolean }[] = [];
    globalThis.fetch = async (input, init) => {
      const url = new URL(input.toString());
      if (url.pathname === "/v1/interests/subscribe") {
        const body = JSON.parse(init?.body?.toString() ?? "{}") as {
          readonly delivery?: { readonly consumer?: boolean };
        };
        registrations.push({ consumer: body.delivery?.consumer === true });
      }
      return responseWithRegistration(input, init, {});
    };
    natsState.failConnects = 1;
    natsState.failError = () =>
      new NatsError("'Authentication Timeout'", ErrorCode.AuthenticationTimeout);
    setNatsConnectForTests(natsStandInConnect, { firstDelayMs: 5, maxDelayMs: 5, budgetMs: 5_000 });
    try {
      const notifications: string[] = [];
      const earlier = [
        { type: "message", message: { role: "user", content: "the task", timestamp: 1 } },
        { type: "message", message: { role: "assistant", content: "working on it", timestamp: 2 } },
      ];
      const context: SessionContext = {
        cwd: "/tmp/envoy-omp-test",
        sessionManager: {
          ...topLevelSession,
          getSessionId: () => "ses_woken",
          getBranch: () => earlier,
          getEntries: () => earlier,
        },
        setInterval: () => {},
        ui: { ...sessionContext().ui, notify: (message) => notifications.push(message) },
      };
      const { default: envoyExtension }: EnvoyModule = await import(
        "./envoy.ts?redial-resumed-start"
      );
      const fixture = createPi();
      await envoyExtension(fixture.pi);
      await fixture.handlers.get("session_start")?.({}, context);

      expect(natsState.failConnects).toBe(0);
      expect(natsState.subscriptions.has("notifications.envoy.deliver.ses_woken")).toBe(true);
      expect(registrations.length).toBeGreaterThan(0);
      expect(registrations.every((one) => one.consumer)).toBe(true);
      expect(
        notifications.filter((message) => message.includes("inbound messages are unavailable"))
      ).toEqual([]);
      expect(notifications.filter((message) => message.includes("not registered"))).toEqual([]);
    } finally {
      setNatsConnectForTests(natsStandInConnect);
    }
  });

  // A pod whose first dial fails, and whose relay is still closed once the start race's bound
  // (`INBOUND_FIRST_OPEN_BOUND_MS`) has passed, is told that it receives nothing. A later pass
  // registers. Without a word that it receives again, the session reads the outage notice as its
  // state for its life.
  test("a session told it receives nothing is told once it receives again, in every part of the notice", async () => {
    globalThis.fetch = async (input, init) => responseWithRegistration(input, init, {});
    natsState.failConnects = 99;
    const infos: string[] = [];
    const stopSink = logger.registerLogSink((entry) => {
      if (entry.level === "info") infos.push(entry.message);
    });
    const start = Date.now();
    try {
      const notifications: string[] = [];
      const intervals: (() => void)[] = [];
      const context: SessionContext = {
        cwd: "/tmp/envoy-omp-test",
        sessionManager: { ...topLevelSession, getSessionId: () => "ses_restored" },
        setInterval: (callback) => intervals.push(callback),
        ui: { ...sessionContext().ui, notify: (message) => notifications.push(message) },
      };
      const { default: envoyExtension }: EnvoyModule = await import("./envoy.ts?inbound-restored");
      const fixture = createPi();
      await envoyExtension(fixture.pi);
      await fixture.handlers.get("session_start")?.({}, context);
      const transcript = () =>
        fixture.entries
          .map((entry) => ("data" in entry ? entry.data : undefined))
          .map((data) =>
            typeof data === "object" &&
            data !== null &&
            "content" in data &&
            typeof data.content === "string"
              ? data.content
              : ""
          );
      expect(transcript().filter((content) => content.includes("Envoy inbound restored"))).toEqual(
        []
      );

      // Past the bound, the background retry's pass finds NATS still closed, and the session is
      // told that it receives nothing.
      setSystemTime(new Date(start + INBOUND_FIRST_OPEN_BOUND_MS + 1_000));
      await until(() => {
        intervals.at(-1)?.();
        return notifications.some((m) => m.includes("inbound messages are unavailable"));
      }, "the outage notice");

      // A later retry's pass dials, opens the relay and registers.
      natsState.failConnects = 0;
      await until(() => {
        intervals.at(-1)?.();
        return notifications.some((m) => m.includes("inbound messages are back"));
      }, "the restored notice");
      expect(infos.filter((m) => m.includes("this session receives again"))).toHaveLength(1);
      expect(
        transcript().filter((content) => content.includes("Envoy inbound restored"))
      ).toHaveLength(1);
    } finally {
      setSystemTime();
      stopSink();
    }
  });

  test("a relay lost after the start names its failure in the refused registration, and the next heartbeat opens it again and says so once", async () => {
    // The relay subject opened at the start. Later its connection closes for good, as nats.js
    // closes one on an authorization error it does not ignore, and the hub refuses every dial
    // after that. The relay's own resubscribe fails once and then waits NATS_RETRY_INTERVAL_MS,
    // and the agent stream's gives up, so here the heartbeat's pass is what opens them again.
    process.env.ENVOY_RESUBSCRIBE_DELAY_MS = "10";
    globalThis.fetch = async (input, init) => responseWithRegistration(input, init, []);
    const id = "ses_relay_lost";
    const relay = deliverSubject(id);
    const control = `agentstream.${id}.control`;
    const reason = "because its relay subject could not be opened: CONNECTION_REFUSED";
    const warnings: string[] = [];
    const infos: string[] = [];
    const stopSink = logger.registerLogSink((entry) => {
      if (entry.level === "warn") warnings.push(entry.message);
      if (entry.level === "info") infos.push(entry.message);
    });
    await withEnvoyTimings({ heartbeatMs: 45_000 }, async () => {
      const notifications: string[] = [];
      const intervals: { readonly callback: () => void; readonly intervalMs: number }[] = [];
      const context: SessionContext = {
        ...sessionContext(id),
        setInterval: (callback, intervalMs) => intervals.push({ callback, intervalMs }),
        ui: { ...sessionContext().ui, notify: (message) => notifications.push(message) },
      };
      const { default: envoyExtension }: EnvoyModule = await import(
        "./envoy.ts?relay-lost-mid-session"
      );
      const fixture = createPi();
      await envoyExtension(fixture.pi);
      const heartbeat = () => {
        for (const { callback, intervalMs } of intervals) if (intervalMs === 45_000) callback();
      };
      const transcript = () =>
        fixture.entries
          .map((entry) => ("data" in entry ? entry.data : undefined))
          .map((data) =>
            typeof data === "object" &&
            data !== null &&
            "content" in data &&
            typeof data.content === "string"
              ? data.content
              : ""
          );
      try {
        await fixture.handlers.get("session_start")?.({}, context);
        expect(natsState.controls.get(relay)?.active()).toBe(true);
        expect(natsState.controls.get(control)?.active()).toBe(true);
        expect(natsState.connectedNames).toHaveLength(1);

        // The connection closes for good: nats.js ends every subscription on it, and the hub
        // refuses each dial from now on.
        natsState.failConnects = 99;
        await natsState.connections[0]?.close();
        natsState.controls.get(relay)?.end();
        natsState.controls.get(control)?.end();
        await until(() => natsState.failConnects < 99, "the relay's own redial");
        await settle();

        // A registration sent outside a pass while the subject is missing - here the one a new
        // follow sends - is refused naming why, and the session is told so.
        const told = () => ({
          logged: warnings.filter((m) => m.includes("this session receives nothing")),
          shown: notifications.filter((m) => m.includes("inbound messages are unavailable")),
          written: transcript().filter((content) => content.includes("Envoy inbound unavailable")),
        });
        const subscribe = fixture.tools.find((tool) => tool.name === "envoy_subscribe");
        if (subscribe === undefined) throw new Error("envoy_subscribe was not registered");
        const followed = await subscribe.execute("", { topics: ["team.relay-lost"] });
        expect(String(followed.details.registrationError)).toContain(reason);
        for (const part of Object.values(told())) {
          expect(part).toHaveLength(1);
          expect(part[0]).toContain(reason);
        }

        // The heartbeat's pass dials to open the relay again, is refused, and sends a
        // registration with no relay subject; that refusal tells the session nothing more.
        const beforeHeartbeat = natsState.failConnects;
        heartbeat();
        await until(
          () =>
            natsState.failConnects < beforeHeartbeat &&
            notifications.some((m) => m.includes("registry heartbeat failed")),
          "the heartbeat's dial and its failure"
        );
        await settle();
        for (const part of Object.values(told())) expect(part).toHaveLength(1);
        expect(
          transcript().filter((content) => content.includes("Envoy inbound restored"))
        ).toEqual([]);

        // The hub admits again. The next heartbeat dials a new connection, opens the relay and
        // the control subject on it, registers, and tells the session once that it receives
        // again.
        natsState.failConnects = 0;
        heartbeat();
        await until(
          () => notifications.some((m) => m.includes("inbound messages are back")),
          "the restored notice"
        );
        await settle();
        expect(natsState.connectedNames).toHaveLength(2);
        expect(natsState.controls.get(relay)?.active()).toBe(true);
        expect(natsState.controls.get(control)?.active()).toBe(true);
        expect(infos.filter((m) => m.includes("this session receives again"))).toHaveLength(1);
        expect(
          transcript().filter((content) => content.includes("Envoy inbound restored"))
        ).toHaveLength(1);

        natsState.controls
          .get(`notifications.agent.${id}`)
          ?.push(forwardedRoleEnvelope("legion-controller", "after the relay came back", "back"));
        await until(
          () => fixture.messages.some((m) => m.includes("after the relay came back")),
          "the message relayed on the new connection"
        );
      } finally {
        stopSink();
        await fixture.handlers.get("session_shutdown")?.({}, context);
      }
    });
  });

  test("a live view control subject whose own resubscribe failed is opened again by the next heartbeat", async () => {
    // The control subscription ends under a connection that stays up, and its one resubscribe,
    // one resubscribe delay later, is refused; nothing else would open it again.
    process.env.ENVOY_RESUBSCRIBE_DELAY_MS = "10";
    globalThis.fetch = async (input, init) => responseWithRegistration(input, init, []);
    const id = "ses_control_lost";
    const control = `agentstream.${id}.control`;
    await withEnvoyTimings({ heartbeatMs: 45_000 }, async () => {
      const intervals: { readonly callback: () => void; readonly intervalMs: number }[] = [];
      const context: SessionContext = {
        ...sessionContext(id),
        setInterval: (callback, intervalMs) => intervals.push({ callback, intervalMs }),
      };
      const { default: envoyExtension }: EnvoyModule = await import(
        "./envoy.ts?control-lost-mid-session"
      );
      const fixture = createPi();
      await envoyExtension(fixture.pi);
      try {
        await fixture.handlers.get("session_start")?.({}, context);
        const first = natsState.controls.get(control);
        expect(first?.active()).toBe(true);

        natsState.failSubscribes = 1;
        first?.end();
        await until(() => natsState.failSubscribes === 0, "the control subject's own resubscribe");
        await settle();
        expect(natsState.controls.get(control)).toBe(first);

        for (const { callback, intervalMs } of intervals) if (intervalMs === 45_000) callback();
        await until(
          () => natsState.controls.get(control) !== first,
          "the heartbeat's open of the control subject"
        );
        expect(natsState.controls.get(control)?.active()).toBe(true);
        expect(natsState.connectedNames).toHaveLength(1);
      } finally {
        await fixture.handlers.get("session_shutdown")?.({}, context);
      }
    });
  });

  /** A session for the relay-loss rows: its notices in all three places, and its heartbeat. */
  async function relayLossSession(id: string, query: string) {
    const warnings: string[] = [];
    const infos: string[] = [];
    const stopSink = logger.registerLogSink((entry) => {
      if (entry.level === "warn") warnings.push(entry.message);
      if (entry.level === "info") infos.push(entry.message);
    });
    const notifications: string[] = [];
    const intervals: { readonly callback: () => void; readonly intervalMs: number }[] = [];
    const context: SessionContext = {
      ...sessionContext(id),
      setInterval: (callback, intervalMs) => intervals.push({ callback, intervalMs }),
      ui: { ...sessionContext().ui, notify: (message) => notifications.push(message) },
    };
    const { default: envoyExtension }: EnvoyModule = await import(`./envoy.ts?${query}`);
    const fixture = createPi();
    await envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, context);
    const written = (word: string) =>
      fixture.entries
        .map((entry) => ("data" in entry ? entry.data : undefined))
        .map((data) =>
          typeof data === "object" &&
          data !== null &&
          "content" in data &&
          typeof data.content === "string"
            ? data.content
            : ""
        )
        .filter((content) => content.includes(word));
    return {
      fixture,
      /** The outage as told in the log, the UI and the transcript. */
      outage: () => ({
        logged: warnings.filter((m) => m.includes("this session receives nothing")),
        shown: notifications.filter((m) => m.includes("inbound messages are unavailable")),
        written: written("Envoy inbound unavailable"),
      }),
      restored: () => ({
        logged: infos.filter((m) => m.includes("this session receives again")),
        shown: notifications.filter((m) => m.includes("inbound messages are back")),
        written: written("Envoy inbound restored"),
      }),
      heartbeat: () => {
        for (const { callback, intervalMs } of intervals) if (intervalMs === 45_000) callback();
      },
      follow: async (topic: string) => {
        const subscribe = fixture.tools.find((tool) => tool.name === "envoy_subscribe");
        if (subscribe === undefined) throw new Error("envoy_subscribe was not registered");
        return String((await subscribe.execute("", { topics: [topic] })).details.registrationError);
      },
      stop: async () => {
        stopSink();
        await fixture.handlers.get("session_shutdown")?.({}, context);
      },
    };
  }

  test("a registration sent right after the connection closed names the close", async () => {
    // nats.js ends each subscription of a connection it closes with no error of its own, and the
    // relay's own redial has not failed yet: the close is the only reason there is.
    process.env.ENVOY_RESUBSCRIBE_DELAY_MS = "10";
    globalThis.fetch = async (input, init) => responseWithRegistration(input, init, []);
    const id = "ses_relay_closed";
    await withEnvoyTimings({ heartbeatMs: 45_000 }, async () => {
      const session = await relayLossSession(id, "relay-closed-reason");
      const parked = Promise.withResolvers<void>();
      try {
        expect(natsState.controls.get(deliverSubject(id))?.active()).toBe(true);
        // Every dial from now on waits, so the relay's own redial never ends during the row.
        natsState.heldConnect = parked.promise;
        await natsState.connections[0]?.close();
        natsState.controls.get(deliverSubject(id))?.end();
        await until(() => natsState.connectedNames.length === 2, "the relay's own redial");
        const reason = "because its relay subject could not be opened: the NATS connection closed";
        expect(await session.follow("team.relay-closed")).toContain(reason);
        for (const part of Object.values(session.outage())) {
          expect(part).toHaveLength(1);
          expect(part[0]).toContain(reason);
        }
      } finally {
        natsState.heldConnect = undefined;
        parked.resolve();
        await session.stop();
      }
    });
  });

  test("a registration sent after a start whose dial failed names the dial's failure, though no relay exists yet", async () => {
    globalThis.fetch = async (input, init) => responseWithRegistration(input, init, []);
    natsState.failConnects = 99;
    const id = "ses_start_dial_failed";
    await withEnvoyTimings({ heartbeatMs: 45_000 }, async () => {
      const session = await relayLossSession(id, "start-dial-failed-reason");
      try {
        // The start's dial failed before a relay existed; a follow outside any pass names it.
        expect(await session.follow("team.start-dial-failed")).toContain(
          "because its relay subject could not be opened: CONNECTION_REFUSED"
        );
      } finally {
        await session.stop();
      }
    });
  });

  test("a registration sent once the connection closed with an error of its own names that error", async () => {
    // nats.js settles `closed()` with the close's own error, which replaces the plugin's own words
    // for the close once it settles.
    process.env.ENVOY_RESUBSCRIBE_DELAY_MS = "10";
    globalThis.fetch = async (input, init) => responseWithRegistration(input, init, []);
    const id = "ses_relay_closed_error";
    const closeError = NatsError.errorForCode(ErrorCode.AuthenticationExpired);
    await withEnvoyTimings({ heartbeatMs: 45_000 }, async () => {
      const session = await relayLossSession(id, "relay-closed-error");
      const parked = Promise.withResolvers<void>();
      try {
        natsState.heldConnect = parked.promise;
        natsState.closeError = closeError;
        await natsState.connections[0]?.close();
        natsState.controls.get(deliverSubject(id))?.end();
        await until(() => natsState.connectedNames.length === 2, "the relay's own redial");
        const reason = `because its relay subject could not be opened: ${natsFailureReason(closeError)}`;
        expect(natsFailureReason(closeError)).not.toBe("the NATS connection closed");
        expect(await session.follow("team.relay-closed-error")).toContain(reason);
      } finally {
        natsState.heldConnect = undefined;
        parked.resolve();
        await session.stop();
      }
    });
  });

  test("a relay subscription that ends with its own error names that error, and the next heartbeat opens it again", async () => {
    process.env.ENVOY_RESUBSCRIBE_DELAY_MS = "10";
    globalThis.fetch = async (input, init) => responseWithRegistration(input, init, []);
    const id = "ses_relay_errored";
    const relay = deliverSubject(id);
    await withEnvoyTimings({ heartbeatMs: 45_000 }, async () => {
      const session = await relayLossSession(id, "relay-own-error");
      try {
        const first = natsState.controls.get(relay);
        // The connection stays up; the relay's own resubscribe is refused, and waits 15 s.
        natsState.failSubscribes = 1;
        first?.fail(NatsError.errorForCode(ErrorCode.Disconnect));
        await until(() => natsState.failSubscribes === 0, "the relay's own resubscribe");
        const reason = "because its relay subject could not be opened: DISCONNECT";
        expect(await session.follow("team.relay-errored")).toContain(reason);
        for (const part of Object.values(session.outage())) {
          expect(part).toHaveLength(1);
          expect(part[0]).toContain(reason);
        }
        session.heartbeat();
        await until(
          () =>
            natsState.controls.get(relay) !== first &&
            natsState.controls.get(relay)?.active() === true,
          "the heartbeat's open of the relay subject"
        );
        await until(() => session.restored().written.length === 1, "the restored notice");
        for (const part of Object.values(session.restored())) expect(part).toHaveLength(1);
        expect(natsState.connectedNames).toHaveLength(1);
      } finally {
        await session.stop();
      }
    });
  });

  test("two opens of the live view's control subject that race each other take it once", async () => {
    // The control subject's own resubscribe and the heartbeat's pass both dial a closed
    // connection; whichever lands second takes nothing.
    process.env.ENVOY_RESUBSCRIBE_DELAY_MS = "10";
    globalThis.fetch = async (input, init) => responseWithRegistration(input, init, []);
    const id = "ses_control_race";
    const control = `agentstream.${id}.control`;
    await withEnvoyTimings({ heartbeatMs: 45_000 }, async () => {
      const session = await relayLossSession(id, "control-open-race");
      const parked = Promise.withResolvers<void>();
      try {
        natsState.heldConnect = parked.promise;
        await natsState.connections[0]?.close();
        natsState.controls.get(control)?.end();
        await until(() => natsState.connectedNames.length === 2, "the control subject's own dial");
        session.heartbeat();
        await until(() => natsState.connectedNames.length === 3, "the heartbeat's dial");
        natsState.heldConnect = undefined;
        parked.resolve();
        await until(
          () => (natsState.controlsByTopic.get(control) ?? []).some((c, i) => i > 0 && c.active()),
          "the control subject opened again"
        );
        await settle();
        // The first subscription ended with its connection; the stand-in still calls an ended
        // subscription active, so only those taken after it are counted.
        const live = (natsState.controlsByTopic.get(control) ?? [])
          .slice(1)
          .filter((c) => c.active());
        expect(live).toHaveLength(1);
      } finally {
        natsState.heldConnect = undefined;
        parked.resolve();
        await session.stop();
      }
    });
  });

  test("the budget of the pass: a pass whose deadline is 7.5 s dials once, bounded by that deadline", async () => {
    await withEnvoyTimings({ heartbeatMs: 5_000, passDeadlineMs: 7_500 }, async () => {
      globalThis.fetch = async (input, init) => responseWithRegistration(input, init, {});
      const bounds: number[] = [];
      setNatsConnectForTests(
        (async (options: ConnectionOptions) => {
          bounds.push(options.timeout ?? Number.NaN);
          throw NatsError.errorForCode(ErrorCode.ConnectionRefused);
        }) as unknown as typeof realConnect,
        NATS_DIAL_PLAN
      );
      try {
        const context: SessionContext = {
          cwd: "/tmp/envoy-omp-test",
          sessionManager: { ...topLevelSession, getSessionId: () => "ses_pass_budget" },
          setInterval: () => {},
          ui: sessionContext().ui,
        };
        const { default: envoyExtension }: EnvoyModule = await import("./envoy.ts?pass-budget");
        const fixture = createPi();
        await envoyExtension(fixture.pi);
        await fixture.handlers.get("session_start")?.({}, context);
        // The rest of the pass needs 20.25 s, more than the 7.5 s it has, so no redial fits.
        expect(bounds).toHaveLength(1);
        expect(bounds[0]).toBeLessThanOrEqual(7_500);
        expect(bounds[0]).toBeGreaterThan(6_000);
      } finally {
        setNatsConnectForTests(natsStandInConnect);
      }
    });
  }, 30_000);

  // legacy-subjects-gate.sh reads each connection's build from the end of the name the hub's census
  // of connections lists: the image's build-commit stamp, else a packed build's release version,
  // else none.
  test("the connection name the server sees ends with the plugin's build", async () => {
    await withEnvoyTimings({ heartbeatMs: 5_000, passDeadlineMs: 7_500 }, async () => {
      globalThis.fetch = async (input, init) => responseWithRegistration(input, init, {});
      const names: string[] = [];
      setNatsConnectForTests(
        (async (options: ConnectionOptions) => {
          names.push(options.name ?? "");
          throw NatsError.errorForCode(ErrorCode.ConnectionRefused);
        }) as unknown as typeof realConnect,
        NATS_DIAL_PLAN
      );
      const stampDirectory = mkdtempSync(join(tmpdir(), "pi-envoy-build-"));
      try {
        // An instance of its own (the query), as each row of this file loads one: the build source
        // it is given is that instance's module state.
        const entry = await import("./envoy.ts?connection-build");
        // The query names no file, so `entry` is untyped; its factory is read through
        // `EnvoyModule`, so a call of it left without `await` fails Biome's noFloatingPromises.
        const { default: envoyExtension }: EnvoyModule = entry;
        const commit = "0123456789abcdef0123456789abcdef01234567";
        const rows = [
          { stamp: `${commit}\n`, packed: true, want: `omp-ses_build legion@${commit}` },
          {
            stamp: undefined,
            packed: true,
            want: `omp-ses_build @sjawhar/pi-envoy@${packageVersion}`,
          },
          { stamp: undefined, packed: false, want: "omp-ses_build" },
        ];
        for (const [index, row] of rows.entries()) {
          const stamp = join(stampDirectory, `build-commit-${index}`);
          if (row.stamp !== undefined) writeFileSync(stamp, row.stamp);
          entry.setBuildSourceForTests({ stamp, packed: row.packed });
          const context: SessionContext = {
            cwd: "/tmp/envoy-omp-test",
            sessionManager: { ...topLevelSession, getSessionId: () => "ses_build" },
            setInterval: () => {},
            ui: sessionContext().ui,
          };
          const fixture = createPi();
          await envoyExtension(fixture.pi);
          names.length = 0;
          await fixture.handlers.get("session_start")?.({}, context);
          await fixture.handlers.get("session_shutdown")?.({}, context);
          expect(names[0]).toBe(row.want);
        }
      } finally {
        rmSync(stampDirectory, { recursive: true, force: true });
        setNatsConnectForTests(natsStandInConnect);
      }
    });
  }, 30_000);

  for (const { row, error, shown, withheld } of [
    {
      row: "names the NATS failure in the refused registration and in every part of the notice",
      error: () => new NatsError("'Authentication Timeout'", ErrorCode.AuthenticationTimeout),
      shown: "Authentication Timeout",
      withheld: [] as string[],
    },
    {
      row: "withholds the text of a NATS failure that carries no code, in every part of the notice",
      error: () => new Error("bad INFO from 10.9.8.7:4222 MARKER-7f3a"),
      shown: "because its relay subject could not be opened:",
      withheld: ["10.9.8.7", "MARKER-7f3a"],
    },
  ]) {
    test(`a session whose dial fails ${row}`, async () => {
      globalThis.fetch = async (input, init) => responseWithRegistration(input, init, {});
      natsState.failConnects = 99;
      natsState.failError = error;
      const warnings: string[] = [];
      const stopSink = logger.registerLogSink((entry) => {
        if (entry.level === "warn") warnings.push(entry.message);
      });
      const start = Date.now();
      try {
        const notifications: string[] = [];
        const intervals: (() => void)[] = [];
        const context: SessionContext = {
          cwd: "/tmp/envoy-omp-test",
          sessionManager: { ...topLevelSession, getSessionId: () => "ses_named_failure" },
          setInterval: (callback) => intervals.push(callback),
          ui: { ...sessionContext().ui, notify: (message) => notifications.push(message) },
        };
        const { default: envoyExtension }: EnvoyModule = await import(
          `./envoy.ts?named-failure-${withheld.length}`
        );
        const fixture = createPi();
        await envoyExtension(fixture.pi);
        await fixture.handlers.get("session_start")?.({}, context);
        // The start race is held for its bound (`INBOUND_FIRST_OPEN_BOUND_MS`). Past it, the
        // background retry's pass, whose dial fails too, tells the session.
        setSystemTime(new Date(start + INBOUND_FIRST_OPEN_BOUND_MS + 1_000));
        await until(() => {
          intervals.at(-1)?.();
          return notifications.some((m) => m.includes("inbound messages are unavailable"));
        }, "the outage notice");

        const parts = {
          "the session_start notice": notifications.find((m) =>
            m.includes("this session is not registered")
          ),
          "the UI notice": notifications.find((m) =>
            m.includes("inbound messages are unavailable")
          ),
          // Held until the host is settled, and written to the transcript at once.
          "the transcript notice": fixture.entries
            .map((entry) => ("data" in entry ? entry.data : undefined))
            .map((data) =>
              typeof data === "object" &&
              data !== null &&
              "content" in data &&
              typeof data.content === "string"
                ? data.content
                : ""
            )
            .find((content) => content.includes("Envoy inbound unavailable")),
          "the log line": warnings.find((m) => m.includes("this session receives nothing")),
        };
        for (const [part, text] of Object.entries(parts)) {
          expect(text, `${part} was not given`).toBeDefined();
          expect(text, `${part} does not name the failure`).toContain(shown);
          // Named once: the pass's own failure text is not added to a refusal that names it.
          expect(
            text?.split("because its relay subject could not be opened").length,
            `${part} names the failure more than once`
          ).toBe(2);
          for (const secret of withheld)
            expect(text, `${part} printed ${secret}`).not.toContain(secret);
        }
      } finally {
        setSystemTime();
        stopSink();
      }
    });
  }
});
