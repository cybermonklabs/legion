// packages/envoy-client/src/__tests__/listener-relay.test.ts
//
// A session's inbound through the listener's relay socket (`../listener-relay.ts`), against a
// stand-in that serves the socket's wire and the broker's token exchange on one origin
// (`../../test/relay-socket-stand-in.ts`): a frame the host answers `done` goes back on the frame's
// reply; each close code is reopened as the wire says, with a fresh trade where the credential was
// the cause; a subject the socket does not carry closes that subscription alone and is logged
// once; a refused trade is told as an inbound outage is; and which settings choose the socket.
//
// The reopen rows read the real clock: what they hold is how long the client waits on it.

import { afterEach, describe, expect, test } from "bun:test";
import { writeFileSync } from "node:fs";
import * as path from "node:path";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import {
  type RelaySocketStandIn,
  type StandInFrame,
  type StandInUpgrade,
  standInDeliverSubject,
  startRelaySocketStandIn,
} from "../../test/relay-socket-stand-in";
import { brokerCredential, forgetBrokerClients } from "../broker-credential";
import { createConsumerDelivery, inboundUnavailable } from "../consumer-delivery";
import { readEnvoyConfig } from "../envoy-config";
import { type EnvoyEnvironment, envoySettings } from "../envoy-settings";
import { connectListenerRelay, type ListenerRelay } from "../listener-relay";
import { sessionSettings } from "../session-settings";

const tempDir = temporaryDirectories("listener-relay");
/** What each row started, stopped after it in the reverse order. */
const started: (() => unknown)[] = [];
afterEach(async () => {
  for (const stop of started.splice(0).reverse()) await stop();
});
afterEach(forgetBrokerClients);

const encoder = new TextEncoder();

function standIn(): RelaySocketStandIn {
  const server = startRelaySocketStandIn();
  started.push(() => server.stop());
  return server;
}

/** A pod's environment whose broker and listener are both `server`. */
function podEnvironment(server: RelaySocketStandIn): EnvoyEnvironment {
  const dir = tempDir();
  const tokenFile = path.join(dir, "token");
  writeFileSync(tokenFile, `${fixtureSecret("proj")}\n`);
  return {
    HOME: dir,
    ENVOY_URL: server.url,
    AGENT_SECRETS_URL: server.url,
    AGENT_SECRETS_MACHINE_IDENTITY: "kubernetes",
    AGENT_SECRETS_MACHINE_TOKEN_FILE: tokenFile,
  };
}

/** A relay socket for `session` at `server`, open once the first socket opens or `boundMs` pass;
 *  closed after the row. `heard.asks` counts each time the relay asks the credential for the
 *  upgrade's headers, and `heard.restored` each time the outage was told it ended. */
async function opened(
  server: RelaySocketStandIn,
  session: string | (() => string),
  heard: {
    readonly logs?: string[];
    readonly told?: string[];
    readonly asks?: { count: number };
    readonly restored?: { count: number };
    readonly boundMs?: number;
  } = {}
): Promise<ListenerRelay> {
  const credential = brokerCredential(podEnvironment(server), session);
  const relay = await connectListenerRelay(
    {
      url: server.url,
      session,
      credential: {
        ...credential,
        headers: (...args) => {
          if (heard.asks !== undefined) heard.asks.count += 1;
          return credential.headers(...args);
        },
      },
      outage: inboundUnavailable(
        (why) => heard.told?.push(why),
        () => {
          if (heard.restored !== undefined) heard.restored.count += 1;
        }
      ),
      log: (line) => heard.logs?.push(line),
    },
    AbortSignal.timeout(heard.boundMs ?? 20_000)
  );
  started.push(() => relay.close());
  return relay;
}

/** Waits until `check` holds, failing with `what()` once `ms` pass. */
async function until(check: () => boolean, ms: number, what: () => string): Promise<void> {
  const ends = Date.now() + ms;
  while (!check()) {
    if (Date.now() > ends) throw new Error(what());
    await Bun.sleep(5);
  }
}

/** A relay frame on `session`'s own agent subject, stored now, as the listener relays one: each
 *  header a list of its values, as K-E2 carries a `nats.Header`. */
function frame(
  session: string,
  seq: number,
  extra: Readonly<Record<string, readonly string[]>> = {}
): StandInFrame {
  return {
    headers: {
      "Envoy-Subject": [`notifications.agent.${session}`],
      "Envoy-Stream-Seq": [String(seq)],
      "Envoy-Stream-Id": ["stream-1"],
      "Envoy-Delivered": ["1"],
      "Envoy-Stored-At": [new Date().toISOString()],
      ...extra,
    },
    data: JSON.stringify({ event_id: `evt-${seq}`, source: "agent" }),
  };
}

/** The upgrades `server` opened, in order. */
function opens(server: RelaySocketStandIn): StandInUpgrade[] {
  return server.upgrades.filter((upgrade) => upgrade.status === 101);
}

describe("a relay socket", () => {
  test("hands the host a frame, and the host's done goes back on the frame's reply", async () => {
    const server = standIn();
    const relay = await opened(server, "ses_a");
    const delivery = createConsumerDelivery({
      sessionID: "ses_a",
      host: { deliver: (handed) => handed.done(), gap: () => undefined, log: () => undefined },
      publish: (reply, body) => relay.publish(reply, encoder.encode(body)),
      ledger: { delivered: () => false, liveFailed: async () => true },
      persist: () => true,
    });
    const subscription = relay.subscribe(delivery.subject);
    void (async () => {
      for await (const message of subscription) delivery.receive(message);
    })();
    await relay.flush();
    const reply = server.deliver("ses_a", frame("ses_a", 7));
    if (reply === undefined) throw new Error("the stand-in holds no relay subscription of ses_a");
    await until(
      () => server.receipts.some((receipt) => receipt.reply === reply && receipt.body === "done"),
      5_000,
      () =>
        `frame 7 of stream-1 on notifications.agent.ses_a was never answered done on its reply; the stand-in read ${JSON.stringify(server.receipts)} and refused ${JSON.stringify(server.refusedPublishes)}`
    );
    expect(server.receipts.filter((receipt) => receipt.reply === reply).map((r) => r.body)).toEqual(
      ["received", "done"]
    );
    expect(server.refusedPublishes).toEqual([]);
    // Each `sub` and `pub` carried K-E2's members and nothing more.
    expect(server.malformed).toEqual([]);
  });

  test("closed 4401 reopens within 1 s with a fresh token, and takes its subject again", async () => {
    const server = standIn();
    const relay = await opened(server, "ses_a");
    relay.subscribe(standInDeliverSubject("ses_a"));
    await relay.flush();
    const closedAt = Date.now();
    server.close(4401, "credential_expired");
    await until(
      () => opens(server).length === 2,
      1_000,
      () => `no reopen within 1 s of a 4401 close; upgrades ${JSON.stringify(server.upgrades)}`
    );
    const [before, after] = opens(server);
    expect(after?.at ?? Number.POSITIVE_INFINITY).toBeLessThan(closedAt + 1_000);
    expect(after?.token).not.toBe(before?.token);
    expect(server.minted.map((one) => one.session)).toEqual(["ses_a", "ses_a"]);
    await until(
      () => server.subs.length === 2,
      1_000,
      () => `the reopened socket took no subject again; it took ${JSON.stringify(server.subs)}`
    );
  });

  test("closed 4403 reopens with a fresh token after 1 s, then 2 s", async () => {
    const server = standIn();
    await opened(server, "ses_a");
    const closes: number[] = [];
    for (const nth of [1, 2]) {
      closes.push(Date.now());
      server.close(4403, "credential_refused");
      await until(
        () => opens(server).length === nth + 1,
        10_000,
        () => `no reopen after 4403 close ${nth}; upgrades ${JSON.stringify(server.upgrades)}`
      );
    }
    const waited = opens(server)
      .slice(1)
      .map((upgrade, index) => upgrade.at - (closes[index] ?? 0));
    // The stand-in's count of reopens sooner than the backoff: none.
    const early = waited.filter((ms, index) => ms < 1_000 * 2 ** index);
    if (early.length > 0) {
      throw new Error(
        `the stand-in counted ${early.length} reopens sooner than 1 s doubling after a 4403 close: ${waited.join(" ms, ")} ms after each`
      );
    }
    // Each wait is the backoff's and no longer than it by more than the trade and the upgrade.
    expect(waited[0]).toBeLessThan(1_900);
    expect(waited[1]).toBeLessThan(2_900);
    const tokens = opens(server).map((upgrade) => upgrade.token);
    expect(new Set(tokens).size).toBe(3);
  });

  test("closed 1013 reopens after 5 s, then 10 s, on the token it holds", async () => {
    const server = standIn();
    await opened(server, "ses_a");
    const closes: number[] = [];
    for (const nth of [1, 2]) {
      closes.push(Date.now());
      server.close(1013, "broker_unavailable");
      await until(
        () => opens(server).length === nth + 1,
        15_000,
        () => `no reopen after 1013 close ${nth}; upgrades ${JSON.stringify(server.upgrades)}`
      );
    }
    const waited = opens(server)
      .slice(1)
      .map((upgrade, index) => upgrade.at - (closes[index] ?? 0));
    const early = waited.filter((ms, index) => ms < 5_000 * 2 ** index);
    if (early.length > 0) {
      throw new Error(
        `the stand-in counted ${early.length} reopens sooner than 5 s doubling after a 1013 close: ${waited.join(" ms, ")} ms after each`
      );
    }
    expect(waited[0]).toBeLessThan(5_900);
    expect(waited[1]).toBeLessThan(10_900);
    expect(new Set(opens(server).map((upgrade) => upgrade.token)).size).toBe(1);
  }, 40_000);

  test("closed 1012 reopens at once", async () => {
    const server = standIn();
    await opened(server, "ses_a");
    const closedAt = Date.now();
    server.close(1012, "service_restart");
    await until(
      () => opens(server).length === 2,
      1_000,
      () => `no reopen within 1 s of a 1012 close; upgrades ${JSON.stringify(server.upgrades)}`
    );
    expect(opens(server)[1]?.at ?? Number.POSITIVE_INFINITY).toBeLessThan(closedAt + 1_000);
  });

  test("a subject it does not carry closes that subscription alone, logged once", async () => {
    const server = standIn();
    const logs: string[] = [];
    const relay = await opened(server, "ses_a", { logs });
    const relayed = relay.subscribe(standInDeliverSubject("ses_a"));
    const refused = [
      relay.subscribe("agentstream.ses_a.control"),
      relay.subscribe("agentstream.ses_a.control"),
    ];
    // Each refused subscription ends, as a closed one does.
    for (const subscription of refused) {
      for await (const _ of subscription) throw new Error("a refused subscription yielded");
    }
    expect(server.refusedSubs).toEqual([
      { session: "ses_a", subject: "agentstream.ses_a.control" },
      { session: "ses_a", subject: "agentstream.ses_a.control" },
    ]);
    expect(logs.filter((line) => line.includes("agentstream.ses_a.control"))).toEqual([
      "the listener's relay socket for ses_a refused the subject agentstream.ses_a.control (subject_not_granted); that subscription is closed",
    ]);
    // The relay subscription goes on.
    server.deliver("ses_a", frame("ses_a", 1));
    const iterator = relayed[Symbol.asyncIterator]();
    const next = await iterator.next();
    expect(next.done).toBe(false);
    expect(next.value?.headers?.get("Envoy-Stream-Seq")).toBe("1");
  });

  test("a flush resolves once the listener has handled every sub sent before it", async () => {
    // The listener reads a socket's frames one at a time and handles a `sub` in full before the
    // next frame; a successful `sub` has no answer of its own. So the pong to a ping sent after
    // it is what says it is held.
    const server = standIn();
    const relay = await opened(server, "ses_a");
    const subject = standInDeliverSubject("ses_a");
    relay.subscribe(subject);
    await relay.flush();
    // Read at once, before the stand-in's event loop could run again.
    const handled = server.handled.filter((one) => one.session === "ses_a").map((one) => one.frame);
    if (!handled.includes(`sub 1 ${subject}`)) {
      throw new Error(
        `flush resolved before the listener handled sub 1 of ${subject}; it had handled ${JSON.stringify(handled)}`
      );
    }
    expect(handled).toEqual([`sub 1 ${subject}`, "ping 1"]);
    expect(server.deliver("ses_a", frame("ses_a", 1))).toBeDefined();
  });

  test("a repeated header's values reach the host in order, each name's case kept", async () => {
    const server = standIn();
    const relay = await opened(server, "ses_a");
    const subscription = relay.subscribe(standInDeliverSubject("ses_a"));
    await relay.flush();
    server.deliver(
      "ses_a",
      frame("ses_a", 3, { "X-Envoy-Trace": ["one", "two"], "x-envoy-trace": ["lower"] })
    );
    const next = await subscription[Symbol.asyncIterator]().next();
    if (next.done === true) throw new Error("the relay subscription ended with no frame");
    expect(next.value.headers.values("X-Envoy-Trace")).toEqual(["one", "two"]);
    expect(next.value.headers.get("X-Envoy-Trace")).toBe("one");
    expect(next.value.headers.values("x-envoy-trace")).toEqual(["lower"]);
    expect(next.value.headers.get("Envoy-Stream-Seq")).toBe("3");
    expect(next.value.headers.values("Envoy-Gap")).toEqual([]);
  });

  test("a refused trade is told once, and the socket opens once the broker trades", async () => {
    const server = standIn();
    server.trades = "refuse";
    const told: string[] = [];
    const asks = { count: 0 };
    const restored = { count: 0 };
    // The token client answers a refusal again with no trade until its own retry, 15 s or so
    // later (`RETRY_FIRST_WAIT_MS`, @legion/broker-client/token-client), and the relay asks it at
    // 1 s, 3 s, 7 s, 15 s and 31 s: the open comes on one of the last two.
    const opening = opened(server, "ses_a", { told, asks, restored, boundMs: 50_000 });
    await until(
      () => told.length > 0,
      5_000,
      () => "a refused trade was never told"
    );
    // Asked again after 1 s, then 2 s: still refused, and told no second time.
    await until(
      () => asks.count >= 3,
      5_000,
      () => `the relay asked its credential ${asks.count} times`
    );
    expect(told).toHaveLength(1);
    expect(told[0]).toContain(
      "the secrets broker refused the projected service-account token AGENT_SECRETS_MACHINE_TOKEN_FILE names: HTTP 400 invalid_grant"
    );
    server.trades = "mint";
    await opening;
    expect(opens(server)).toHaveLength(1);
    // The socket that opened ends the outage, and says so once.
    expect(restored.count).toBe(1);
  }, 60_000);

  test("follows the session its name reads now, with that session's own token", async () => {
    const server = standIn();
    let session = "ses_a";
    const relay = await opened(server, () => session);
    session = "ses_b";
    await relay.follow();
    expect(opens(server).map((upgrade) => upgrade.session)).toEqual(["ses_a", "ses_b"]);
    expect(server.minted.map((one) => one.session)).toEqual(["ses_a", "ses_b"]);
  });

  test("the stand-in asks a test's own routes first, and the trade and 404 take what they leave", async () => {
    // A driver registers and sends at the listener whose socket it opens, and serves its device
    // login's grants at the broker that trades its session's token, so its routes answer on the
    // one origin, the token endpoint included. Each is handed a copy of the request: one it reads
    // and leaves still reaches the trade whole. The relay upgrade is never handed to them.
    const asked: string[] = [];
    // Random, as a broker's token is: the client holds every token it is handed.
    const deviceToken = fixtureSecret("device");
    const server = startRelaySocketStandIn({
      routes: async (request) => {
        const { pathname } = new URL(request.url);
        const grant = new URLSearchParams(await request.text()).get("grant_type") ?? "";
        asked.push(`${request.method} ${pathname}${grant === "" ? "" : ` ${grant}`}`);
        if (pathname === "/v1/subscribe") return Response.json({ registered: true });
        if (grant === "urn:ietf:params:oauth:grant-type:device_code") {
          return Response.json({ access_token: deviceToken });
        }
        return undefined;
      },
    });
    started.push(() => server.stop());
    const registered = await fetch(`${server.url}/v1/subscribe`, { method: "POST" });
    expect(await registered.json()).toEqual({ registered: true });
    const device = await fetch(`${server.url}/v1/oauth/token`, {
      method: "POST",
      body: new URLSearchParams({ grant_type: "urn:ietf:params:oauth:grant-type:device_code" }),
    });
    expect(await device.json()).toEqual({ access_token: deviceToken });
    expect((await fetch(`${server.url}/v1/messages/send`, { method: "POST" })).status).toBe(404);
    // The session's own trade goes through the routes and on to the stand-in's trade.
    await opened(server, "ses_a");
    expect(asked).toEqual([
      "POST /v1/subscribe",
      "POST /v1/oauth/token urn:ietf:params:oauth:grant-type:device_code",
      "POST /v1/messages/send",
      "POST /v1/oauth/token urn:ietf:params:oauth:grant-type:token-exchange",
    ]);
    expect(server.minted.map((token) => token.session)).toEqual(["ses_a"]);
    expect(opens(server).map((upgrade) => upgrade.session)).toEqual(["ses_a"]);
  });
});

/** The settings `env` and an envoy.json holding `file` resolve, as a session of `ses_a` reads
 *  them, or, with `{}`, a caller that names no session. */
function settings(
  env: EnvoyEnvironment,
  file?: unknown,
  caller: { readonly session?: string } = { session: "ses_a" }
) {
  const config = path.join(tempDir(), "envoy.json");
  if (file !== undefined) writeFileSync(config, JSON.stringify(file));
  const resolved = envoySettings(env, readEnvoyConfig(config, env, { cwd: tempDir() }), caller);
  if (!resolved.enabled) throw new Error(`Envoy is off: ${resolved.error ?? resolved.notice}`);
  return resolved;
}

describe("which inbound a session takes", () => {
  const pod = (): EnvoyEnvironment => {
    const dir = tempDir();
    const tokenFile = path.join(dir, "token");
    writeFileSync(tokenFile, `${fixtureSecret("proj")}\n`);
    return {
      HOME: dir,
      ENVOY_URL: "https://envoy.example",
      AGENT_SECRETS_URL: "https://secrets.example",
      AGENT_SECRETS_MACHINE_IDENTITY: "kubernetes",
      AGENT_SECRETS_MACHINE_TOKEN_FILE: tokenFile,
    };
  };

  test("ENVOY_INBOUND=listener takes it through the relay socket on the broker credential", () => {
    const { inbound } = settings({ ...pod(), ENVOY_INBOUND: "listener" });
    if (!("relaySetting" in inbound))
      throw new Error(`no relay socket: ${JSON.stringify(inbound)}`);
    expect(inbound.relaySetting).toBe("ENVOY_INBOUND");
    expect(inbound.relayCredential.scheme).toBe("DPoP");
  });

  test("envoy.inbound listener in the file does too, and ENVOY_INBOUND=nats overrides it", () => {
    const fromFile = settings(pod(), { envoy: { inbound: "listener" } }).inbound;
    expect("relaySetting" in fromFile && fromFile.relaySetting).toMatch(
      /^envoy\.inbound in \/\S+\/envoy\.json$/
    );
    expect(
      settings({ ...pod(), ENVOY_INBOUND: "nats" }, { envoy: { inbound: "listener" } }).inbound
    ).toEqual({
      off: "Envoy has no NATS server (envoy.natsUrls, ENVOY_NATS_URL)",
    });
  });

  test("ENVOY_INBOUND=listener beside ENVOY_NATS_URL is refused naming both", () => {
    const env = {
      ...pod(),
      ENVOY_INBOUND: "listener",
      ENVOY_NATS_URL: "nats://nats.internal.example:4222",
    };
    expect(settings(env).inbound).toEqual({
      off: "ENVOY_INBOUND is listener, and ENVOY_NATS_URL names NATS servers: set one of the two",
    });
    const fromFile = settings(pod(), {
      envoy: { inbound: "listener", natsUrls: ["nats://nats.internal.example:4222"] },
    }).inbound;
    expect("off" in fromFile && fromFile.off).toMatch(
      /^envoy\.inbound in (\/\S+\/envoy\.json) is listener, and envoy\.natsUrls in \1 names NATS servers: set one of the two$/
    );
  });

  test("ENVOY_INBOUND=listener with a bearer or no broker route is refused naming why", () => {
    expect(
      settings({ ...pod(), ENVOY_INBOUND: "listener", ENVOY_TOKEN: "bearer-1" }).inbound
    ).toEqual({
      off: "ENVOY_INBOUND is listener, but the listener's credential is a bearer (ENVOY_TOKEN, ENVOY_TOKEN_FILE or envoy.token), and the relay socket admits only the session's broker token",
    });
    expect(
      settings({ ...pod(), ENVOY_INBOUND: "listener", AGENT_SECRETS_URL: "" }).inbound
    ).toEqual({
      off: "ENVOY_INBOUND is listener, but AGENT_SECRETS_URL names no secrets broker to trade it at",
    });
  });

  test("ENVOY_INBOUND=listener for a caller that names no session is refused naming it", () => {
    expect(settings({ ...pod(), ENVOY_INBOUND: "listener" }, undefined, {}).inbound).toEqual({
      off: "ENVOY_INBOUND is listener, but this process names no session for its relay socket's token",
      noSessionNamed: true,
    });
  });

  test("another value is refused naming the setting and its two values, not the value", () => {
    expect(settings({ ...pod(), ENVOY_INBOUND: "Listener" }).inbound).toEqual({
      off: "ENVOY_INBOUND is neither nats nor listener",
    });
  });

  test("ENVOY_INBOUND=nats is what an unset one is", () => {
    const env = { ...pod(), ENVOY_NATS_URL: "nats://nats.internal.example:4222" };
    expect(settings({ ...env, ENVOY_INBOUND: "nats" }).inbound).toEqual(settings(env).inbound);
    expect(settings({ ...env, ENVOY_INBOUND: "" }).inbound).toEqual(settings(env).inbound);
  });

  test("the start notice says the relay socket carries no live view", () => {
    const env = { ...pod(), ENVOY_INBOUND: "listener" };
    const file = path.join(tempDir(), "envoy.json");
    const { notice } = sessionSettings({ path: file, error: null }, env, {
      cwd: tempDir(),
      session: "ses_a",
    });
    expect(notice).toContain(
      "Envoy's inbound messages come through the listener's relay socket (ENVOY_INBOUND is listener), which carries no live view: the Agents page shows nothing of this session"
    );
  });
});
