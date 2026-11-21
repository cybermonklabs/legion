// packages/envoy-client/src/__tests__/nats-credential.test.ts
//
// A session's broker NATS credential on the wire: what its authenticator puts on a CONNECT, that
// it is the token and key the session's HTTP calls present, that nats.js is told never to give up
// over a refused one, how the keeper moves the connection to a renewed token before the old one
// lapses, and, on a real nats-server behind the real auth service, that a connection whose token
// lapsed while the broker was away is admitted again once a token is live, that a revoked token
// is dropped and the connection comes back, that a session switch subscribes the new session only
// once the connection is admitted as it, and that a hub with no auth service fails the dial naming
// it; and which credential `envoySettings` resolves for a session: a seed, the session's broker
// token, or none, and the reason inbound messages are off where the settings conflict or leave a
// gap. The keeper and the follow against a stand-in connection are
// nats-credential-keeper.test.ts's; how long its trades wait, and `renew` and
// `followNatsCredential`'s deadlines, are nats-credential-deadlines.test.ts's.

import { beforeAll, describe, expect, test } from "bun:test";
import { createHash, createPublicKey, verify } from "node:crypto";
import { closeSync, mkdirSync, openSync, readFileSync, writeFileSync } from "node:fs";
import { createRequire } from "node:module";
import * as path from "node:path";
import { thumbprintOf } from "@legion/cipher-client/test/sign-ins";
import { RETRY_FIRST_WAIT_MS } from "@legion/cipher-client/token-client";
import type { Subprocess } from "bun";
import { connect, Events, type NatsConnection } from "nats";
import {
  awaitReady,
  buildGoBinaries,
  envoyStackEnabled,
  STOP_GRACE_MS,
  stopProcess,
  useEnvoyStackDirectory,
} from "../../test/envoy-stack";
import { brokerCredential } from "../broker-credential";
import { readEnvoyConfig } from "../envoy-config";
import { type EnvoyEnvironment, envoySettings } from "../envoy-settings";
import {
  BROKER_REDIAL_CAP_MS,
  BROKER_REDIAL_POLL_MS,
  natsConnectionOptions,
} from "../nats-connection";
import {
  brokerNatsCredential,
  followNatsCredential,
  keepNatsCredential,
  type NatsCredential,
  TRADE_DEADLINE_MS,
} from "../nats-credential";
import { natsBrokerStandIns } from "./broker-stand-ins";

const { podEnvironment, stubBroker, tempDir } = natsBrokerStandIns("nats-credential");

/**
 * The proof a CONNECT's signature field carries, verified with the key its own header embeds:
 * nats.js sends what the authenticator answered, and the auth service reads it base64url-encoded,
 * as nats.go sends one. Throws when the signature does not verify.
 */
function verifiedProof(sig: string): {
  readonly header: { readonly typ: string; readonly alg: string; readonly jwk: JsonWebKey };
  readonly claims: Record<string, unknown>;
} {
  const [header = "", claims = "", signature = ""] = Buffer.from(sig, "base64url")
    .toString("utf8")
    .split(".");
  const decoded = (part: string) => JSON.parse(Buffer.from(part, "base64url").toString("utf8"));
  const read = { header: decoded(header), claims: decoded(claims) };
  const key = createPublicKey({ key: read.header.jwk, format: "jwk" });
  const signed = Buffer.from(`${header}.${claims}`);
  const raw = Buffer.from(signature, "base64url");
  if (!verify("sha256", signed, { key, dsaEncoding: "ieee-p1363" }, raw)) {
    throw new Error("the connection proof's signature does not verify with its own key");
  }
  return read;
}

/** The credential `envoySettings` resolves for `env` and an envoy.json holding `file` (none
 *  when undefined), or the reason inbound messages are off. */
function resolved(
  env: EnvoyEnvironment,
  file?: unknown
): NatsCredential | { readonly off: string } {
  const config = path.join(tempDir(), "envoy.json");
  if (file !== undefined) writeFileSync(config, JSON.stringify(file));
  const settings = envoySettings(env, readEnvoyConfig(config, env, { cwd: tempDir() }), {
    session: "ses_a",
  });
  if (!settings.enabled) throw new Error(`Envoy is off: ${settings.error ?? settings.notice}`);
  return "off" in settings.inbound ? settings.inbound : settings.inbound.natsCredential;
}

describe("which credential a session connects to NATS with", () => {
  test("a broker route, no seed and no NATS_AUTH resolves to none", () => {
    // The machine's broker route says nothing about the hub: dev-stack.sh without --callout, for
    // one, exports a broker route beside a hub that runs no auth service, where a broker token on
    // the CONNECT would lose the session its inbound messages.
    expect(resolved(podEnvironment("http://broker.test"))).toEqual({ kind: "none" });
    expect(resolved({ ...podEnvironment("http://broker.test"), NATS_AUTH: "" })).toEqual({
      kind: "none",
    });
  });

  test("a seed and no NATS_AUTH resolves to the seed, as it does today", () => {
    expect(resolved({ ...podEnvironment("http://broker.test"), NATS_NKEY_SEED: "SUSEED" })).toEqual(
      { kind: "seed", seed: { value: "SUSEED", source: "NATS_NKEY_SEED" } }
    );
  });

  test("NATS_AUTH=broker with a broker route resolves to the session's broker token", () => {
    const credential = resolved({ ...podEnvironment("http://broker.test"), NATS_AUTH: "broker" });
    expect("kind" in credential && credential.kind).toBe("broker");
  });

  test("envoy.natsAuth broker resolves to it too, and NATS_AUTH overrides it", () => {
    const env = podEnvironment("http://broker.test");
    const file = { envoy: { natsUrls: ["nats://nats.internal.example:4222"], natsAuth: "broker" } };
    const fromFile = resolved(env, file);
    expect("kind" in fromFile && fromFile.kind).toBe("broker");
    expect(resolved({ ...env, NATS_AUTH: "none" }, file)).toEqual({ kind: "none" });
  });

  test("a hub with no auth service is refused naming the setting that chose the broker token", () => {
    // The remedy is the one that undoes the choice: unsetting NATS_AUTH does nothing for a
    // session whose envoy.json chose the broker token.
    const env = podEnvironment("http://broker.test");
    const fromFile = resolved(env, {
      envoy: { natsUrls: ["nats://nats.internal.example:4222"], natsAuth: "broker" },
    });
    if (!("kind" in fromFile) || fromFile.kind !== "broker")
      throw new Error("no broker credential");
    expect(() => fromFile.broker.authenticator(undefined)).toThrow(
      /^envoy\.natsAuth in \/\S+\/envoy\.json is broker, but the NATS server sent no nonce to sign: it runs no auth service, so take natsAuth out of that file for this hub$/
    );
    const fromVariable = resolved({ ...env, NATS_AUTH: "broker" });
    if (!("kind" in fromVariable) || fromVariable.kind !== "broker")
      throw new Error("no broker credential");
    expect(() => fromVariable.broker.authenticator(undefined)).toThrow(
      "NATS_AUTH is broker, but the NATS server sent no nonce to sign: it runs no auth service, so unset NATS_AUTH for this hub"
    );
  });

  test("envoy.natsAuth broker goes only to the servers written beside it", () => {
    // As a file's seed does: a session's token and the proof it signs would otherwise go to any
    // server ENVOY_NATS_URL names, which could relay that proof to the hub it was meant for.
    const env = podEnvironment("http://broker.test");
    for (const natsUrls of [["nats://nats.elsewhere.example:4222"], undefined]) {
      const fromFile = resolved(env, { envoy: { natsUrls, natsAuth: "broker" } });
      expect("off" in fromFile && fromFile.off).toMatch(
        /^ENVOY_NATS_URL names other servers than \/\S+\/envoy\.json's envoy\.natsUrls; set NATS_AUTH=broker for them$/
      );
    }
    // The variable is the environment's own choice, for whatever servers it names.
    const fromVariable = resolved(
      { ...env, NATS_AUTH: "broker" },
      { envoy: { natsUrls: ["nats://nats.elsewhere.example:4222"] } }
    );
    expect("kind" in fromVariable && fromVariable.kind).toBe("broker");
  });

  test("NATS_AUTH=none resolves to none", () => {
    expect(resolved({ ...podEnvironment("http://broker.test"), NATS_AUTH: "none" })).toEqual({
      kind: "none",
    });
  });

  test("NATS_AUTH=broker with a key dir and no machine identity resolves inbound off", () => {
    // Every enrolled Legion pod's environment before it is given an identity to trade: a key
    // directory, a broker, and nothing this client can prove.
    const env: EnvoyEnvironment = {
      HOME: tempDir(),
      ENVOY_NATS_URL: "nats://nats.internal.example:4222",
      AGENT_SECRETS_URL: "http://broker.test",
      AGENT_SECRETS_KEY_DIR: tempDir(),
      NATS_AUTH: "broker",
    };
    expect(resolved(env)).toEqual({
      off: "NATS_AUTH is broker, but AGENT_SECRETS_MACHINE_IDENTITY names no identity this machine proves",
    });
  });

  test("NATS_AUTH=broker with no broker resolves inbound off naming the gap", () => {
    const env = {
      ...podEnvironment("http://broker.test"),
      AGENT_SECRETS_URL: "",
      NATS_AUTH: "broker",
    };
    expect(resolved(env)).toEqual({
      off: "NATS_AUTH is broker, but AGENT_SECRETS_URL names no secrets broker to trade it at",
    });
  });

  test("NATS_AUTH=broker beside a seed resolves inbound off naming both", () => {
    const env = {
      ...podEnvironment("http://broker.test"),
      NATS_AUTH: "broker",
      NATS_NKEY_SEED: "SUSEED",
    };
    expect(resolved(env)).toEqual({
      off: "NATS_AUTH is broker, and NATS_NKEY_SEED names an nkey seed: set one of the two",
    });
  });

  test("NATS_AUTH=none beside a seed resolves to the seed", () => {
    // A seed is the credential wherever one is set (the plan's table); only `broker` beside it
    // names two credentials at once.
    const env = {
      ...podEnvironment("http://broker.test"),
      NATS_AUTH: "none",
      NATS_NKEY_SEED: "SUSEED",
    };
    expect(resolved(env)).toEqual({
      kind: "seed",
      seed: { value: "SUSEED", source: "NATS_NKEY_SEED" },
    });
  });

  test("another value resolves inbound off naming the setting and its two values, not the value", () => {
    // A value is never echoed: a seed or token pasted into the wrong setting is a credential.
    const env = podEnvironment("http://broker.test");
    expect(resolved({ ...env, NATS_AUTH: "SUAPASTEDSEED" })).toEqual({
      off: "NATS_AUTH is neither broker nor none",
    });
    const fromFile = resolved(env, { envoy: { natsAuth: "Broker" } });
    expect("off" in fromFile && fromFile.off).toMatch(
      /^envoy\.natsAuth in \/\S+\/envoy\.json is neither broker nor none$/
    );
  });

  test("a caller that opens no NATS connection resolves no credential", () => {
    const env = { ...podEnvironment("http://broker.test"), NATS_AUTH: "broker" };
    const config = readEnvoyConfig(path.join(tempDir(), "envoy.json"), env, { cwd: tempDir() });
    const settings = envoySettings(env, config, { nats: false });
    expect(settings.enabled && settings.inbound).toEqual({
      off: "this session opens no NATS connection",
    });
  });

  test("NATS_AUTH=broker for a caller that names no session resolves inbound off naming it", () => {
    // A trade under no session is answered the machine's token, under AGENT_SECRETS_SESSION, or
    // the signed-in person's: neither is the session the connection delivers to (the review of
    // 2fa6fbf7cf, h4).
    const env = { ...podEnvironment("http://broker.test"), NATS_AUTH: "broker" };
    const config = readEnvoyConfig(path.join(tempDir(), "envoy.json"), env, { cwd: tempDir() });
    const settings = envoySettings(env, config, {});
    // The reason is typed too (`noSessionNamed`), for a caller with a remedy of its own
    // (`operatorNatsSeed`), which branches on it rather than on the text. Every other reason
    // stays untyped: the rows above hold each one with `toEqual({ off })`.
    expect(settings.enabled && settings.inbound).toEqual({
      off: "NATS_AUTH is broker, but this process names no session for its NATS connection's token",
      noSessionNamed: true,
    });
  });

  test("a broker token whose session name reads none trades nothing and says so", async () => {
    const broker = stubBroker(600);
    let session = "";
    const credential = brokerNatsCredential(podEnvironment(broker.url), () => session);
    const refusal =
      "NATS_AUTH is broker, but this process names no session for its NATS connection's token";
    await expect(credential.ready({ waitForReady: true })).rejects.toThrow(refusal);
    await expect(credential.renew()).rejects.toThrow(refusal);
    expect(credential.authenticator("nonce-1")).toBeUndefined();
    await Bun.sleep(50);
    expect(broker.state.trades).toBe(0);
    // The control: once the name reads a session, its token is traded.
    session = "ses_named";
    await credential.ready();
    expect(broker.state.minted.map((one) => one.session)).toEqual(["ses_named"]);
  });
});

describe("the broker token's authenticator", () => {
  test("authenticator signs the server's nonce", async () => {
    const broker = stubBroker(600);
    const credential = brokerNatsCredential(podEnvironment(broker.url), "ses_a");
    // Before the token is in hand it presents nothing, and never throws: nats.js closes a
    // connection for good when its authenticator throws.
    // The trade starts here: an authenticator with no token in hand asks for one.
    const asked = Date.now();
    expect(credential.authenticator("nonce-0")).toBeUndefined();
    await credential.ready();
    const answered = Date.now();
    const auth = credential.authenticator("nonce-1") as { auth_token: string; sig: string };
    expect(Object.keys(auth).sort()).toEqual(["auth_token", "sig"]);
    const [minted] = broker.state.minted;
    expect(auth.auth_token).toBe(minted?.value ?? "no token minted");
    expect(minted?.session).toBe("ses_a");
    const proof = verifiedProof(auth.sig);
    expect(proof.header.typ).toBe("dpop+jwt");
    expect(proof.header.alg).toBe("ES256");
    expect(proof.claims.nonce).toBe("nonce-1");
    expect(proof.claims.htm).toBe("CONNECT");
    expect(proof.claims.htu).toBe("nats:connect");
    expect(proof.claims.ath).toBe(createHash("sha256").update(auth.auth_token).digest("base64url"));
    // Signed by the key the token was traded with, which the broker bound it to.
    expect(await thumbprintOf(proof.header.jwk)).toBe(minted?.jkt ?? "no key");
    // Another connection's nonce is another proof of the same token.
    const again = credential.authenticator("nonce-2") as { auth_token: string; sig: string };
    expect(again.auth_token).toBe(auth.auth_token);
    expect(verifiedProof(again.sig).claims.nonce).toBe("nonce-2");
    // A server that sends no nonce runs no auth service, so nothing there admits a broker token:
    // the CONNECT fails naming that, rather than going out with no credential.
    for (const nonce of [undefined, ""]) {
      expect(() => credential.authenticator(nonce)).toThrow(
        "NATS_AUTH is broker, but the NATS server sent no nonce to sign: it runs no auth service, so unset NATS_AUTH for this hub"
      );
    }
    // When the token in hand lapses: the answer's expires_in, read when it arrived. The broker
    // mints it after the row asked, and the token client reads the answer after that and before
    // `ready` returns, so the expiry it holds is the broker's, later by at most the time the row
    // measured around the trade, however long the machine held either side.
    const late = credential.expiresAt().getTime() - (minted?.expiresAt ?? 0);
    expect(late).toBeGreaterThanOrEqual(0);
    expect(late).toBeLessThanOrEqual(answered - asked);
  });
});

describe("one token per session for HTTP and NATS", () => {
  test("the NATS credential presents the token and key the listener calls present", async () => {
    const broker = stubBroker(600);
    const env = podEnvironment(broker.url);
    const headers = await brokerCredential(env, "ses_shared").headers(
      "POST",
      "http://listener.test/v1/messages/send"
    );
    const credential = brokerNatsCredential(env, "ses_shared");
    await credential.ready();
    const auth = credential.authenticator("nonce-1") as { auth_token: string; sig: string };
    expect(broker.state.minted).toHaveLength(1);
    expect(headers.Authorization).toBe(`DPoP ${auth.auth_token}`);
    const httpKey = JSON.parse(
      Buffer.from((headers.DPoP ?? "").split(".")[0] ?? "", "base64url").toString("utf8")
    ) as { readonly jwk: JsonWebKey };
    expect(await thumbprintOf(verifiedProof(auth.sig).header.jwk)).toBe(
      await thumbprintOf(httpKey.jwk)
    );
  });
});

describe("natsConnectionOptions with the broker token", () => {
  test("never gives up over a refused credential", async () => {
    // The auth service refuses a session while it is away or while the session's token lapses
    // with the broker away; nats.js closes for good on a repeated refusal unless this is set. A
    // seed or no credential keeps nats.js's default (nats-connection-r12.test.ts).
    const broker = stubBroker(600);
    const credential = brokerNatsCredential(podEnvironment(broker.url), "ses_a");
    const options = natsConnectionOptions({
      servers: ["nats://127.0.0.1:4222"],
      name: "test",
      credential: { kind: "broker", broker: credential },
    });
    expect(options.ignoreAuthErrorAbort).toBe(true);
    // It redials on its own backoff, and sets no `reconnectTimeWait`, which nats.js reads only in
    // the default handler this one replaces.
    expect(options.reconnectDelayHandler).toBeFunction();
    expect(options.reconnectTimeWait).toBeUndefined();
  });

  test("the option it sets is the one nats.js reads", () => {
    // The control for the row above: nats.js takes an option it does not know without a word, so
    // this reads the installed nats.js for the default it carries and the line that acts on it.
    const nats = createRequire(import.meta.url).resolve("nats");
    const base = path.join(
      path.dirname(nats),
      nats.includes("/lib/") ? "" : "lib",
      "nats-base-client"
    );
    expect(readFileSync(path.join(base, "options.js"), "utf8")).toContain(
      "ignoreAuthErrorAbort: false"
    );
    expect(readFileSync(path.join(base, "protocol.js"), "utf8")).toContain(
      "this.options.ignoreAuthErrorAbort === false"
    );
  });
});

const stackDirectory = useEnvoyStackDirectory();

/** A natsd behind the real auth service, which asks `brokerUrl`'s introspection route about each
 *  session token. `refusals` counts the clients natsd itself has refused so far, one
 *  `authentication error` line of its log each (nats-server `authViolation`): every refusal,
 *  whether the service refused the connection or natsd gave up waiting for the service's answer,
 *  where the service logs nothing; and one count whatever the service's own log does with
 *  repeated refusals. `serviceRefusals` is the service's own lines, for a dump. `logs` is the last
 *  lines of both processes' logs. */
interface CalloutHub {
  readonly natsUrl: string;
  refusals(): number;
  serviceRefusals(): number;
  logs(): string;
  stop(): Promise<void>;
}

/** The ports natsd bound, read from its log; undefined until it has logged both. natsd is started
 *  with `-p -1 -m -1`, so the kernel picks each port as natsd binds it: a port chosen beforehand
 *  and let go can be taken by another process before natsd binds it, and natsd then exits with
 *  "address already in use" (seen on this repository's dev box under load). */
function natsdPorts(
  log: string
): { readonly natsPort: number; readonly monitorPort: number } | undefined {
  const text = readFileSync(log, "utf8");
  const client = /Listening for client connections on 127\.0\.0\.1:(\d+)/.exec(text);
  const monitor = /Starting http monitor on 127\.0\.0\.1:(\d+)/.exec(text);
  if (client === null || monitor === null) return undefined;
  return { natsPort: Number(client[1]), monitorPort: Number(monitor[1]) };
}

/** Whether the natsd that logs to `log` has bound its ports and answers its health check, which
 *  ends when `signal` aborts. */
async function natsdHealthy(log: string, signal: AbortSignal): Promise<boolean> {
  const ports = natsdPorts(log);
  if (ports === undefined) return false;
  return (await fetch(`http://127.0.0.1:${ports.monitorPort}/healthz`, { signal })).ok;
}

/** Starts a {@link CalloutHub} in its own directory under the stack directory, from the binaries
 *  the describe's `beforeAll` built. Its waits for natsd and then calloutd to come up end at
 *  `deadline` (a `Date.now()` reading), the row's own: a slow start spends the row's bound, and
 *  one that runs out fails naming the process that was not up. */
async function startCalloutHub(
  name: string,
  brokerUrl: string,
  deadline: number
): Promise<CalloutHub> {
  const work = path.join(stackDirectory, name);
  const bin = path.join(stackDirectory, "bin");
  const keys = path.join(work, "callout-keys");
  mkdirSync(keys, { recursive: true });
  const { PATH = "" } = process.env;
  const budget = () => ({ timeoutMs: deadline - Date.now(), tailLines: 10 });
  const processes: Subprocess[] = [];
  const stop = async () => {
    for (const child of processes.reverse()) await stopProcess(child, "SIGTERM");
  };
  try {
    const natsLog = path.join(work, "natsd.log");
    const natsOut = openSync(natsLog, "a");
    const natsd = Bun.spawn(
      [
        path.join(bin, "natsd"),
        ...["-p", "-1", "-m", "-1"],
        ...["-store_dir", path.join(work, "nats-store"), "-callout-keys", keys],
      ],
      { env: { PATH, HOME: work }, stdout: natsOut, stderr: natsOut }
    );
    closeSync(natsOut);
    processes.push(natsd);
    await awaitReady("natsd", natsd, natsLog, budget(), (signal) => natsdHealthy(natsLog, signal));
    const ports = natsdPorts(natsLog);
    if (ports === undefined) throw new Error(`natsd named no port in ${natsLog}`);
    const natsUrl = `nats://127.0.0.1:${ports.natsPort}`;
    const calloutLog = path.join(work, "calloutd.log");
    const calloutOut = openSync(calloutLog, "a");
    const calloutd = Bun.spawn(
      [
        path.join(bin, "calloutd"),
        ...["-keys", keys, "-nats", natsUrl],
        ...["-introspect", `${brokerUrl}/v1/oauth/introspect`],
      ],
      { env: { PATH, HOME: work }, stdout: calloutOut, stderr: calloutOut }
    );
    closeSync(calloutOut);
    processes.push(calloutd);
    await awaitReady("calloutd", calloutd, calloutLog, budget(), async () =>
      readFileSync(calloutLog, "utf8").includes("calloutd: ready")
    );
    const count = (file: string, text: string) =>
      readFileSync(file, "utf8")
        .split("\n")
        .filter((line) => line.includes(text)).length;
    const refusals = () => count(natsLog, "authentication error");
    const serviceRefusals = () => count(calloutLog, "refused a NATS connection");
    const tail = (file: string) => readFileSync(file, "utf8").split("\n").slice(-40).join("\n");
    const logs = () =>
      `calloutd.log, its last 40 lines:\n${tail(calloutLog)}\nnatsd.log, its last 40 lines:\n${tail(natsLog)}`;
    return { natsUrl, refusals, serviceRefusals, logs, stop };
  } catch (error) {
    await stop();
    throw error;
  }
}

/** How long a row's teardown gives its connection's close; a close past it is left behind. */
const CLOSE_MS = 5_000;

/** The room each row below keeps at the end of its bound. Every wait of the row, its hub's start
 *  included, ends this long before the row's timeout, measured from the row's own start, so one
 *  that gives up fails naming what it waited for, after the row's teardown, rather than as the
 *  timeout, however long the row's setup took. The teardown closes the connection, within
 *  `CLOSE_MS`, and stops calloutd and then natsd, each given `STOP_GRACE_MS` before it is killed;
 *  and 5 s more. */
const ROW_ROOM_MS = CLOSE_MS + 2 * STOP_GRACE_MS + 5_000;

/**
 * What `step` settles to, unless `deadline` (a `Date.now()` reading) passes first: then it fails
 * naming `what`, and `abandoned`, when given, takes what `step` settles to later (a connection to
 * close). Each of a row's waits on an answer from natsd or the auth service goes through this, or
 * through a bound of the product's own that the row hands its deadline, so an answer that never
 * comes fails the row by name before its timeout.
 */
async function byDeadline<T>(
  step: Promise<T>,
  deadline: number,
  what: string,
  abandoned?: (value: T) => void
): Promise<T> {
  const passed = Promise.withResolvers<never>();
  const timer = setTimeout(
    () => passed.reject(new Error(`${what} had not come by the row's deadline`)),
    Math.max(0, deadline - Date.now())
  );
  try {
    return await Promise.race([step, passed.promise]);
  } catch (error) {
    if (abandoned !== undefined) void step.then(abandoned, () => {});
    throw error;
  } finally {
    clearTimeout(timer);
  }
}

/** Closes `connection`, when there is one, within `CLOSE_MS`; a teardown's, so it never throws. */
async function closeWithin(connection: NatsConnection | undefined): Promise<void> {
  if (connection === undefined) return;
  await byDeadline(connection.close(), Date.now() + CLOSE_MS, "the connection's close").catch(
    () => {}
  );
}

describe.skipIf(!envoyStackEnabled)(
  "a session's connection to a hub behind the auth service",
  () => {
    beforeAll(async () => {
      await buildGoBinaries({
        bin: path.join(stackDirectory, "bin"),
        packages: ["./internal/testnats/cmd/natsd", "./internal/testnats/cmd/calloutd"],
        stack: "auth callout",
        binaries: "natsd and calloutd",
      });
    }, 300_000);

    /** The bound of the row below, passed to it; its first wait takes its share of it. */
    const RECONNECT_ROW_MS = 180_000;

    test(
      "reconnects after two refusals once a token is live again",
      async () => {
        // The row's clock starts before its setup, so the hub's start spends the row's bound and
        // leaves the waits below the rest of it, never more.
        const started = Date.now();
        const rowDeadline = started + RECONNECT_ROW_MS - ROW_ROOM_MS;
        // What the client did, for a failure's dump: every status nats.js reports (a redial's start
        // is its `reconnecting`, its end a `reconnect`, an `error` or the next `reconnecting`), and
        // each CONNECT the authenticator answered, with what it presented. A dial that hangs shows
        // as a `reconnecting` with nothing after it; a connection admitted and kept, as a
        // `reconnect` with no `disconnect` after it.
        const trace: string[] = [];
        const note = (what: string) => trace.push(`${Date.now() - started} ms: ${what}`);
        // Tokens live three seconds, so the user the service admits a connection as lapses inside
        // the test, as a 15-minute one does inside a session; the server expires it on its own
        // clock, so this runs on the wall clock.
        const broker = stubBroker(3);
        const hub = await startCalloutHub("reconnect", broker.url, rowDeadline);
        note("the hub is up");
        let connection: NatsConnection | undefined;
        let stopKeeping = () => {};
        try {
          const credential = brokerNatsCredential(podEnvironment(broker.url), "ses_e2e");
          await credential.ready({ boundMs: rowDeadline - Date.now() });
          note(`token in hand, lapsing at ${credential.expiresAt().getTime() - started} ms`);
          const options = natsConnectionOptions({
            servers: [hub.natsUrl],
            name: "nats-credential-e2e",
            credential: { kind: "broker", broker: credential },
          });
          const authenticate = options.authenticator as (nonce?: string) => unknown;
          const opened = await byDeadline(
            connect({
              ...options,
              authenticator: ((nonce?: string) => {
                const auth = authenticate(nonce) as { auth_token?: string } | undefined;
                note(
                  `CONNECT ${auth?.auth_token === undefined ? "presents nothing" : "presents the token"}`
                );
                return auth;
              }) as typeof options.authenticator,
            }),
            rowDeadline,
            "natsd's answer to the first connect",
            (late) => void late.close()
          );
          connection = opened;
          const reconnected: number[] = [];
          void (async () => {
            for await (const status of opened.status()) {
              note(`status ${status.type} ${JSON.stringify(status.data)}`);
              if (status.type === Events.Reconnect) reconnected.push(Date.now());
            }
          })();
          // Its own delivery channel is what the service grants a session.
          opened.subscribe("notifications.envoy.deliver.ses_e2e");
          await byDeadline(opened.flush(), rowDeadline, "natsd's answer to the flush");
          const warnings: string[] = [];
          stopKeeping = keepNatsCredential(opened, credential, (text) => {
            note(`keeper: ${text}`);
            warnings.push(text);
          });

          // The broker goes away: the token lapses with no other to take its place, the server
          // drops the user it admitted, and each reconnect is refused.
          note("the broker goes away");
          broker.state.down = true;
          // Each phase below waits for its fact, bounded only by the row's deadline. An earlier
          // phase keeps back the time the later ones take on the product's own schedule, so a slow
          // phase cannot leave the next none: the keeper's warning, at most TRADE_DEADLINE_MS after
          // its renewal starts, and the reconnect once the broker is back, at most
          // RETRY_FIRST_WAIT_MS after a failed trade, the next dial at most BROKER_REDIAL_CAP_MS
          // on, and the redial loop's BROKER_REDIAL_POLL_MS. The schedules themselves are held to
          // the millisecond by the keeper's and the redial handler's rows on the fake clock
          // (nats-credential-keeper, nats-redial); on the wall clock and a real natsd, this row
          // holds the facts.
          const reconnectMs = RETRY_FIRST_WAIT_MS + BROKER_REDIAL_CAP_MS + BROKER_REDIAL_POLL_MS;
          const until = async (holds: () => boolean, by: number) => {
            while (!holds() && Date.now() < by) await Bun.sleep(100);
            return holds();
          };
          const into = (at: number) => `${Math.round((at - started) / 1000)} s into the row`;
          const failed = (what: string) =>
            new Error(
              [
                `${what}; natsd refused ${hub.refusals()} connection(s) (the auth service logged ${hub.serviceRefusals()} refusal(s)), and the connection is ${opened.isClosed() ? "closed" : "open"}`,
                "the client, by milliseconds since the row began:",
                ...trace,
                hub.logs(),
              ].join("\n")
            );
          const refusedBy = rowDeadline - reconnectMs - TRADE_DEADLINE_MS;
          if (!(await until(() => hub.refusals() >= 2, refusedBy)))
            throw failed(`natsd had not refused 2 connections by ${into(refusedBy)}`);
          expect(opened.isClosed()).toBe(false);
          // The keeper warns once its renewal fails, and natsd refuses once the token lapses on the
          // server's clock: two clocks, so no order between the two is promised.
          const warnedBy = rowDeadline - reconnectMs;
          if (!(await until(() => warnings.length > 0, warnedBy)))
            throw failed(`the keeper had not warned by ${into(warnedBy)}`);
          for (const one of broker.state.minted) {
            expect(warnings.join("\n")).not.toContain(one.value);
          }

          // The broker is back: a token is traded and the connection is admitted again. It mints
          // tokens of a session's usual life again, so the keeper does not renew within the row:
          // with 3-second tokens it renews about every 1.5 s and reconnects to present each new
          // token, and the round trip below raced that reconnect (NatsError DISCONNECT).
          note("the broker is back");
          const back = Date.now();
          broker.state.seconds = 900;
          broker.state.down = false;
          if (!(await until(() => reconnected.some((at) => at > back), rowDeadline)))
            throw failed(`the connection had not come back by ${into(rowDeadline)}`);
          expect(opened.isClosed()).toBe(false);
          expect(
            await byDeadline(opened.rtt(), rowDeadline, "natsd's answer to the round trip")
          ).toBeGreaterThanOrEqual(0);
        } finally {
          stopKeeping();
          await closeWithin(connection);
          await hub.stop();
        }
      },
      RECONNECT_ROW_MS
    );

    /** The bound of the row below, passed to it; its wait for the reconnect is bounded by it. */
    const REVOKED_ROW_MS = 120_000;

    test(
      "a token revoked before it lapses is dropped at the second refusal, and the connection comes back",
      async () => {
        // The broker revokes the session's token: the auth service, which asks it about each
        // token, refuses every CONNECT that presents it, and nats.js redials on its backoff
        // (`brokerRedialDelays`). Tokens live ten minutes, so only the keeper's drop gets the
        // connection a token it admits. The row's clock starts before its setup, as the row
        // above's does.
        const started = Date.now();
        const rowDeadline = started + REVOKED_ROW_MS - ROW_ROOM_MS;
        const trace: string[] = [];
        const note = (what: string) => trace.push(`${Date.now() - started} ms: ${what}`);
        const broker = stubBroker(600);
        const hub = await startCalloutHub("revoked", broker.url, rowDeadline);
        note("the hub is up");
        let connection: NatsConnection | undefined;
        let stopKeeping = () => {};
        try {
          const credential = brokerNatsCredential(podEnvironment(broker.url), "ses_revoked");
          await credential.ready({ boundMs: rowDeadline - Date.now() });
          const opened = await byDeadline(
            connect(
              natsConnectionOptions({
                servers: [hub.natsUrl],
                name: "nats-credential-revoked",
                credential: { kind: "broker", broker: credential },
              })
            ),
            rowDeadline,
            "natsd's answer to the first connect",
            (late) => void late.close()
          );
          connection = opened;
          const reconnected: number[] = [];
          void (async () => {
            for await (const status of opened.status()) {
              note(`status ${status.type} ${JSON.stringify(status.data)}`);
              if (status.type === Events.Reconnect) reconnected.push(Date.now());
            }
          })();
          const warnings: string[] = [];
          stopKeeping = keepNatsCredential(opened, credential, (text) => {
            note(`keeper: ${text}`);
            warnings.push(text);
          });
          await byDeadline(opened.flush(), rowDeadline, "natsd's answer to the flush");

          note("the broker revokes the token, and the connection redials");
          const revoked = broker.state.minted[0]?.value ?? "";
          broker.state.revoked.add(revoked);
          const at = Date.now();
          await byDeadline(opened.reconnect(), rowDeadline, "the reconnect nats.js was asked for");
          // Two refusals on the backoff, then the token traded at the second, which goes out the
          // next time the redial loop asks. When each redial goes out is the redial handler's
          // schedule, held to the millisecond by its rows on the fake clock (nats-redial); here the
          // wait is for the fact, bounded only by the row's deadline.
          while (!reconnected.some((when) => when > at) && Date.now() < rowDeadline)
            await Bun.sleep(100);
          if (!reconnected.some((when) => when > at))
            throw new Error(
              [
                `the connection had not come back by ${Math.round((rowDeadline - started) / 1000)} s into the row; natsd refused ${hub.refusals()} connection(s), and the broker minted ${broker.state.minted.length} token(s)`,
                "the client, by milliseconds since the row began:",
                ...trace,
                hub.logs(),
              ].join("\n")
            );
          expect(hub.refusals()).toBeGreaterThanOrEqual(2);
          expect(broker.state.minted.length).toBe(2);
          expect(warnings.join("\n")).toContain(
            "refused the connection's live broker token (AUTHORIZATION_VIOLATION)"
          );
          for (const one of broker.state.minted)
            expect(warnings.join("\n")).not.toContain(one.value);
          expect(opened.isClosed()).toBe(false);
          expect(
            await byDeadline(opened.rtt(), rowDeadline, "natsd's answer to the round trip")
          ).toBeGreaterThanOrEqual(0);
        } finally {
          stopKeeping();
          await closeWithin(connection);
          await hub.stop();
        }
      },
      REVOKED_ROW_MS
    );

    /** The bound of the row below, passed to it; its hub's start is bounded by it. */
    const SWITCH_ROW_MS = 120_000;

    test(
      "a session switch subscribes the new session only once the connection is admitted as it",
      async () => {
        const rowDeadline = Date.now() + SWITCH_ROW_MS - ROW_ROOM_MS;
        const broker = stubBroker(600);
        const hub = await startCalloutHub("switch", broker.url, rowDeadline);
        let connection: NatsConnection | undefined;
        try {
          // The session name a plugin hands its credential: read at each trade, so a switch moves
          // the credential to the new session's token.
          let session = "ses_before";
          const credential = brokerNatsCredential(podEnvironment(broker.url), () => session);
          await credential.ready({ boundMs: rowDeadline - Date.now() });
          const opened = await byDeadline(
            connect(
              natsConnectionOptions({
                servers: [hub.natsUrl],
                name: "nats-credential-switch",
                credential: { kind: "broker", broker: credential },
              })
            ),
            rowDeadline,
            "natsd's answer to the first connect",
            (late) => void late.close()
          );
          connection = opened;
          const before = opened.subscribe("notifications.envoy.deliver.ses_before");
          await byDeadline(opened.flush(), rowDeadline, "natsd's answer to the flush");
          expect(before.isClosed()).toBe(false);

          // The host switches: the old session's subject goes, and the new one's opens once the
          // connection presents the new session's token.
          before.unsubscribe();
          session = "ses_after";
          // The follow's own bound, which the row's deadline shortens when it is nearer.
          await followNatsCredential(opened, credential, rowDeadline - Date.now());
          // A subscribe the server refuses closes the subscription (nats.js's permission error).
          const after = opened.subscribe("notifications.envoy.deliver.ses_after");
          await byDeadline(opened.flush(), rowDeadline, "natsd's answer to the flush");
          expect(after.isClosed()).toBe(false);
          expect(broker.state.minted.map((one) => one.session)).toEqual([
            "ses_before",
            "ses_after",
          ]);
          // Nothing to move when the session has not changed.
          await followNatsCredential(opened, credential, rowDeadline - Date.now());
          expect(broker.state.minted.length).toBe(2);
        } finally {
          await closeWithin(connection);
          await hub.stop();
        }
      },
      SWITCH_ROW_MS
    );

    /** The bound of the row below, passed to it; its natsd's start is bounded by it. */
    const NO_AUTH_ROW_MS = 120_000;

    test(
      "NATS_AUTH=broker against a hub with no auth service fails naming it",
      async () => {
        // natsd with no callout and no users admits any client and sends no nonce, so a broker
        // token has nothing to be checked by: the dial fails, naming that and the remedy, rather
        // than connecting with no credential while the session believes it presents its token.
        const rowDeadline = Date.now() + NO_AUTH_ROW_MS - ROW_ROOM_MS;
        const broker = stubBroker(600);
        const work = path.join(stackDirectory, "no-auth");
        mkdirSync(work, { recursive: true });
        const natsLog = path.join(work, "natsd.log");
        const natsOut = openSync(natsLog, "a");
        const { PATH = "" } = process.env;
        const natsd = Bun.spawn(
          [
            path.join(stackDirectory, "bin", "natsd"),
            ...["-p", "-1", "-m", "-1"],
            ...["-store_dir", path.join(work, "nats-store")],
          ],
          { env: { PATH, HOME: work }, stdout: natsOut, stderr: natsOut }
        );
        closeSync(natsOut);
        try {
          await awaitReady(
            "natsd",
            natsd,
            natsLog,
            { timeoutMs: rowDeadline - Date.now(), tailLines: 10 },
            (signal) => natsdHealthy(natsLog, signal)
          );
          const ports = natsdPorts(natsLog);
          if (ports === undefined) throw new Error(`natsd named no port in ${natsLog}`);
          const credential = brokerNatsCredential(podEnvironment(broker.url), "ses_no_auth");
          await credential.ready({ boundMs: rowDeadline - Date.now() });
          const dialled = connect(
            natsConnectionOptions({
              servers: [`nats://127.0.0.1:${ports.natsPort}`],
              name: "nats-credential-no-auth",
              credential: { kind: "broker", broker: credential },
            })
          );
          await expect(
            byDeadline(
              dialled,
              rowDeadline,
              "natsd's answer to the dial",
              (late) => void late.close()
            )
          ).rejects.toThrow(
            "NATS_AUTH is broker, but the NATS server sent no nonce to sign: it runs no auth service, so unset NATS_AUTH for this hub"
          );
        } finally {
          await stopProcess(natsd, "SIGTERM");
        }
      },
      NO_AUTH_ROW_MS
    );
  }
);
