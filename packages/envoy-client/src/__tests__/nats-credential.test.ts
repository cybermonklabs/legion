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
import { RETRY_FIRST_WAIT_MS, RETRY_MAX_WAIT_MS } from "@legion/cipher-client/token-client";
import {
  recordTimers,
  type TimerOnTheWay,
  timersBetween,
} from "@legion/test-fixtures/timer-record";
import { until } from "@legion/test-fixtures/wait";
import type { Subprocess } from "bun";
import { connect, Events, type NatsConnection } from "nats";
import {
  awaitReady,
  buildGoBinaries,
  envoyStackEnabled,
  stopProcess,
  useEnvoyStackDirectory,
} from "../../test/envoy-stack";
import { brokerCredential } from "../broker-credential";
import { readEnvoyConfig } from "../envoy-config";
import { type EnvoyEnvironment, envoySettings } from "../envoy-settings";
import {
  BROKER_REDIAL_CAP_MS,
  BROKER_REDIAL_POLL_MS,
  brokerRedialWaitBounds,
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
  if ("off" in settings.inbound) return settings.inbound;
  if (!("natsCredential" in settings.inbound)) throw new Error("inbound is the relay socket");
  return settings.inbound.natsCredential;
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
    expect(broker.state.trades).toBe(0);
    // The control: once the name reads a session, its token is traded. That trade is the later
    // action whose effect is ordered after anything the calls above started: the broker counts
    // each trade as it reaches it, so a trade they made would be counted beside this one.
    session = "ses_named";
    await credential.ready();
    expect(broker.state.trades).toBe(1);
    expect(broker.state.minted.map((one) => one.session)).toEqual(["ses_named"]);
  });
});

describe("the broker token's authenticator", () => {
  test("authenticator signs the server's nonce", async () => {
    const broker = stubBroker(600);
    const credential = brokerNatsCredential(podEnvironment(broker.url), "ses_a");
    // Before the token is in hand it presents nothing, and never throws: nats.js closes a
    // connection for good when its authenticator throws.
    expect(credential.authenticator("nonce-0")).toBeUndefined();
    await credential.ready();
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
    // When the token in hand lapses: the answer's expires_in, read when it arrived.
    const lapses = credential.expiresAt().getTime();
    expect(Math.abs(lapses - (minted?.expiresAt ?? 0))).toBeLessThan(2_000);
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

/** The guard of the describe's one Go build of natsd and calloutd: a hook whose work is one
 *  measured process run with no shared wait in it keeps a guard of its own, at least twice its
 *  slowest reading (docs/testing.md), which the `"//"` note of this package's package.json gives. */
const BUILD_GUARD_MS = 300_000;

/** Waits through the shared wait until `probe` holds; past the row's deadline it fails naming
 *  `what`, followed by `dump()`, what the client and the hub had logged by then. */
async function untilOrDump(what: string, probe: () => boolean, dump: () => string) {
  try {
    await until(what, probe);
  } catch (error) {
    throw new Error(`${error instanceof Error ? error.message : String(error)}\n${dump()}`, {
      cause: error,
    });
  }
}

/** The roots of the program's own code the e2e rows record the timers of: this package's sources
 *  and the token client's (`@legion/cipher-client`), whose trades the credential waits on. */
const programRoots = [
  path.join(import.meta.dir, ".."),
  path.dirname(Bun.resolveSync("@legion/cipher-client/token-client", import.meta.dir)),
];

/** A row's own step in the record it shares with the program's timers. */
type RowMark = { readonly mark: string };

/** The index of the first step of `steps` that is the row's mark `mark`; -1 where there is none. */
function markAt(steps: readonly unknown[], mark: string): number {
  return steps.findIndex(
    (step) => typeof step === "object" && step !== null && "mark" in step && step.mark === mark
  );
}

/**
 * The timers of `steps` armed before the step at `at`, by code whose line `from` names, that fired
 * between it and the step at `before`: what was already pending when the row acted, with the
 * delay the program armed each with.
 */
function pendingTimers(
  steps: readonly unknown[],
  at: number,
  before: number,
  from: string
): TimerOnTheWay[] {
  const fired = new Set<unknown>();
  for (const step of steps.slice(at + 1, before)) {
    if (typeof step === "object" && step !== null && "fired" in step) fired.add(step.fired);
  }
  return steps
    .slice(0, at)
    .flatMap((step) =>
      typeof step === "object" &&
      step !== null &&
      "timer" in step &&
      "delayMs" in step &&
      "from" in step &&
      typeof step.delayMs === "number" &&
      typeof step.from === "string" &&
      fired.has(step.timer) &&
      step.from.includes(from)
        ? [{ delayMs: step.delayMs, from: step.from }]
        : []
    );
}

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

/** Whether the natsd that logs to `log` has bound its ports and answers its health check. */
async function natsdHealthy(log: string): Promise<boolean> {
  const ports = natsdPorts(log);
  if (ports === undefined) return false;
  return (await fetch(`http://127.0.0.1:${ports.monitorPort}/healthz`)).ok;
}

/** Starts a {@link CalloutHub} in its own directory under the stack directory, from the binaries
 *  the describe's `beforeAll` built. */
async function startCalloutHub(name: string, brokerUrl: string): Promise<CalloutHub> {
  const work = path.join(stackDirectory, name);
  const bin = path.join(stackDirectory, "bin");
  const keys = path.join(work, "callout-keys");
  mkdirSync(keys, { recursive: true });
  const { PATH = "" } = process.env;
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
    await awaitReady("natsd", natsd, natsLog, () => natsdHealthy(natsLog));
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
    await awaitReady("calloutd", calloutd, calloutLog, async () =>
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
    }, BUILD_GUARD_MS);

    test("reconnects after two refusals once a token is live again", async () => {
      // Tokens live three seconds, so the user the service admits a connection as lapses inside
      // the test, as a 15-minute one does inside a session; the server expires it on its own
      // clock, so this runs on the wall clock.
      const broker = stubBroker(3);
      const hub = await startCalloutHub("reconnect", broker.url);
      let connection: NatsConnection | undefined;
      let stopKeeping = () => {};
      // What the client did, for a failure's dump: every status nats.js reports (a redial's start
      // is its `reconnecting`, its end a `reconnect`, an `error` or the next `reconnecting`), and
      // each CONNECT the authenticator answered, with what it presented. A dial that hangs shows
      // as a `reconnecting` with nothing after it; a connection admitted and kept, as a
      // `reconnect` with no `disconnect` after it.
      const started = Date.now();
      const trace: string[] = [];
      const note = (what: string) => trace.push(`${Date.now() - started} ms: ${what}`);
      // The program's own record of its way, in one order with the row's marks: each timer and
      // deadline this package's code and the token client's armed and fired
      // (`@legion/test-fixtures/timer-record`), so each wait below is held to the delays the
      // program chose, not to a clock.
      const steps: unknown[] = [];
      const stopRecording = recordTimers(programRoots, (step) => steps.push(step));
      const mark = (what: string) => steps.push({ mark: what } satisfies RowMark);
      try {
        const credential = brokerNatsCredential(podEnvironment(broker.url), "ses_e2e");
        await credential.ready();
        note(`token in hand, lapsing at ${credential.expiresAt().getTime() - started} ms`);
        const options = natsConnectionOptions({
          servers: [hub.natsUrl],
          name: "nats-credential-e2e",
          credential: { kind: "broker", broker: credential },
        });
        const authenticate = options.authenticator as (nonce?: string) => unknown;
        const opened = await connect({
          ...options,
          authenticator: ((nonce?: string) => {
            const auth = authenticate(nonce) as { auth_token?: string } | undefined;
            note(
              `CONNECT ${auth?.auth_token === undefined ? "presents nothing" : "presents the token"}`
            );
            return auth;
          }) as typeof options.authenticator,
        });
        connection = opened;
        // How many times the connection came back, each a `reconnect` nats.js reports.
        let reconnects = 0;
        void (async () => {
          for await (const status of opened.status()) {
            note(`status ${status.type} ${JSON.stringify(status.data)}`);
            if (status.type === Events.Reconnect) {
              reconnects += 1;
              mark("reconnected");
            }
          }
        })();
        // Its own delivery channel is what the service grants a session.
        opened.subscribe("notifications.envoy.deliver.ses_e2e");
        await opened.flush();
        const warnings: string[] = [];
        // What the keeper says of a renewal that failed, which a refusal's warning does not say.
        const renewalFailed =
          "; it is tried again, and the connection reconnects once a token is live";
        mark("keeping");
        stopKeeping = keepNatsCredential(opened, credential, (text) => {
          note(`keeper: ${text}`);
          warnings.push(text);
          if (text.includes(renewalFailed)) mark("warned");
        });

        // The broker goes away: the token lapses with no other to take its place, the server
        // drops the user it admitted, and each reconnect is refused.
        note("the broker goes away");
        broker.state.down = true;
        const dump = () =>
          [
            `natsd refused ${hub.refusals()} connection(s) (the auth service logged ${hub.serviceRefusals()} refusal(s)); the connection is ${opened.isClosed() ? "closed" : "open"}`,
            "the client, by milliseconds since the row began:",
            ...trace,
            hub.logs(),
          ].join("\n");
        await untilOrDump("natsd's second refusal of a reconnect", () => hub.refusals() >= 2, dump);
        expect(opened.isClosed()).toBe(false);
        // The keeper warns once its renewal fails, and natsd refuses once the token lapses on the
        // server's clock: two clocks, so no order between the two is promised, and the row waits
        // for the warning itself.
        await untilOrDump(
          "the keeper's warning of its failed renewal",
          () => warnings.some((text) => text.includes(renewalFailed)),
          dump
        );
        // Main's bound, TRADE_DEADLINE_MS + 2 s from the broker going away, on the program's own
        // delays: what the keeper and the token client waited on from the keeper's start to its
        // warning - its renewal's schedule, a trade's deadline (`AbortSignal.timeout`), a retry -
        // stays inside it, however long the machine took.
        const warnedOn = timersBetween(steps, markAt(steps, "keeping"), markAt(steps, "warned"));
        expect(
          warnedOn.reduce((sum, wait) => sum + wait.delayMs, 0),
          `the program's own timers before the keeper's warning: ${JSON.stringify(warnedOn)}`
        ).toBeLessThan(TRADE_DEADLINE_MS + 2_000);
        for (const one of broker.state.minted) {
          expect(warnings.join("\n")).not.toContain(one.value);
        }

        // The broker is back: a token is traded and the connection is admitted again. It mints
        // tokens of a session's usual life again, so the keeper does not renew within the row:
        // with 3-second tokens it renews about every 1.5 s and reconnects to present each new
        // token, and the round trip below raced that reconnect (NatsError DISCONNECT).
        // The connection comes back on the token client's schedule: only a token landing starts
        // it over (`BrokerTokenClient.#run`), so the next trade is due at most RETRY_FIRST_WAIT_MS
        // doubled once for each trade that failed while the broker was away, capped at
        // RETRY_MAX_WAIT_MS, after the last of them. The credential holds the traded token only
        // once something takes it: unless the keeper's retry takes it first, the next dial due, at
        // most BROKER_REDIAL_CAP_MS on, finds nothing live to present, is refused, and starts the
        // trade that takes it; the token then goes out the next time the redial loop asks
        // (`brokerRedialDelays`), within BROKER_REDIAL_POLL_MS. The row waits for that reconnect.
        const failedTrades = Math.max(broker.state.trades - broker.state.minted.length, 1);
        const nextTradeMs = Math.min(
          RETRY_FIRST_WAIT_MS * 2 ** (failedTrades - 1),
          RETRY_MAX_WAIT_MS
        );
        const reconnectsBefore = reconnects;
        mark("back");
        broker.state.seconds = 900;
        broker.state.down = false;
        await untilOrDump(
          "the connection's reconnect once the broker is back",
          () => reconnects > reconnectsBefore,
          dump
        );
        // Main's bound, `nextTradeMs + BROKER_REDIAL_CAP_MS + BROKER_REDIAL_POLL_MS + 5_000` from
        // the broker's return, on the program's own delays: no wait of the token client pending at
        // the return was armed past that schedule's slot, and the client armed no more than the
        // redial's, the redial loop's ask and the handshake's time of its own once the broker was
        // back.
        const back = markAt(steps, "back");
        const reconnected = steps.findIndex(
          (step, index) =>
            index > back &&
            typeof step === "object" &&
            step !== null &&
            "mark" in step &&
            step.mark === "reconnected"
        );
        for (const pending of pendingTimers(steps, back, reconnected, "/token-client.ts:")) {
          expect(
            pending.delayMs,
            `the token client's wait pending at the broker's return (${pending.from}), against its schedule's slot after ${failedTrades} failed trade(s)`
          ).toBeLessThanOrEqual(nextTradeMs);
        }
        const afterBack = timersBetween(steps, back, reconnected).filter((wait) =>
          wait.from.includes("/token-client.ts:")
        );
        expect(
          afterBack.reduce((sum, wait) => sum + wait.delayMs, 0),
          `the token client's own timers between the broker's return and the reconnect: ${JSON.stringify(afterBack)}`
        ).toBeLessThanOrEqual(BROKER_REDIAL_CAP_MS + BROKER_REDIAL_POLL_MS + 5_000);
        expect(opened.isClosed()).toBe(false);
        expect(await opened.rtt()).toBeGreaterThanOrEqual(0);
      } finally {
        stopRecording();
        stopKeeping();
        await connection?.close().catch(() => {});
        await hub.stop();
      }
    });

    test("a token revoked before it lapses is dropped at the second refusal, and the connection comes back", async () => {
      // The broker revokes the session's token: the auth service, which asks it about each
      // token, refuses every CONNECT that presents it, and nats.js redials on its backoff
      // (`brokerRedialDelays`). Tokens live ten minutes, so only the keeper's drop gets the
      // connection a token it admits.
      const broker = stubBroker(600);
      const hub = await startCalloutHub("revoked", broker.url);
      let connection: NatsConnection | undefined;
      let stopKeeping = () => {};
      const started = Date.now();
      const trace: string[] = [];
      const note = (what: string) => trace.push(`${Date.now() - started} ms: ${what}`);
      // The program's own record of its way back, in one order with the row's marks
      // (`@legion/test-fixtures/timer-record`): the reconnect is held to the delays it chose.
      const steps: unknown[] = [];
      const stopRecording = recordTimers(programRoots, (step) => steps.push(step));
      const mark = (what: string) => steps.push({ mark: what } satisfies RowMark);
      try {
        const credential = brokerNatsCredential(podEnvironment(broker.url), "ses_revoked");
        await credential.ready();
        const opened = await connect(
          natsConnectionOptions({
            servers: [hub.natsUrl],
            name: "nats-credential-revoked",
            credential: { kind: "broker", broker: credential },
          })
        );
        connection = opened;
        // How many times the connection came back, each a `reconnect` nats.js reports.
        let reconnects = 0;
        void (async () => {
          for await (const status of opened.status()) {
            note(`status ${status.type} ${JSON.stringify(status.data)}`);
            if (status.type === Events.Reconnect) {
              reconnects += 1;
              mark("reconnected");
            }
          }
        })();
        const warnings: string[] = [];
        stopKeeping = keepNatsCredential(opened, credential, (text) => {
          note(`keeper: ${text}`);
          warnings.push(text);
        });
        await opened.flush();

        note("the broker revokes the token, and the connection redials");
        const revoked = broker.state.minted[0]?.value ?? "";
        mark("revoked");
        broker.state.revoked.add(revoked);
        const reconnectsBefore = reconnects;
        await opened.reconnect();
        // Two refusals on the backoff, then the token traded at the second, which goes out the
        // next time the redial loop asks: the connection is under a minute old, so it keeps its
        // place in the backoff, and the two refusals are its first two redials. The row waits for
        // that reconnect itself.
        const dump = () =>
          [
            `natsd refused ${hub.refusals()} connection(s), and the broker minted ${broker.state.minted.length} token(s)`,
            "the client, by milliseconds since the row began:",
            ...trace,
            hub.logs(),
          ].join("\n");
        await untilOrDump(
          "the connection's reconnect after the revoke",
          () => reconnects > reconnectsBefore,
          dump
        );
        // Main's bound from the revoke, on the program's own delays: the first redial due at most
        // `brokerRedialWaitBounds(1)` after the connection opened and the second at most
        // `brokerRedialWaitBounds(2)` after that, then BROKER_REDIAL_POLL_MS, and 10 s for the
        // trade and the handshakes. What the keeper and the token client waited on between the
        // revoke and the reconnect - the drop's renewal, its trade - stays inside it.
        const revokedAt = markAt(steps, "revoked");
        const cameBack = steps.findIndex(
          (step, index) =>
            index > revokedAt &&
            typeof step === "object" &&
            step !== null &&
            "mark" in step &&
            step.mark === "reconnected"
        );
        const waited = timersBetween(steps, revokedAt, cameBack);
        expect(
          waited.reduce((sum, wait) => sum + wait.delayMs, 0),
          `the program's own timers between the revoke and the reconnect: ${JSON.stringify(waited)}\n${dump()}`
        ).toBeLessThan(
          brokerRedialWaitBounds(1).highestMs +
            brokerRedialWaitBounds(2).highestMs +
            BROKER_REDIAL_POLL_MS +
            10_000
        );
        expect(hub.refusals()).toBeGreaterThanOrEqual(2);
        expect(broker.state.minted.length).toBe(2);
        expect(warnings.join("\n")).toContain(
          "refused the connection's live broker token (AUTHORIZATION_VIOLATION)"
        );
        for (const one of broker.state.minted) expect(warnings.join("\n")).not.toContain(one.value);
        expect(opened.isClosed()).toBe(false);
        expect(await opened.rtt()).toBeGreaterThanOrEqual(0);
      } finally {
        stopRecording();
        stopKeeping();
        await connection?.close().catch(() => {});
        await hub.stop();
      }
    });

    test("a session switch subscribes the new session only once the connection is admitted as it", async () => {
      const broker = stubBroker(600);
      const hub = await startCalloutHub("switch", broker.url);
      let connection: NatsConnection | undefined;
      try {
        // The session name a plugin hands its credential: read at each trade, so a switch moves
        // the credential to the new session's token.
        let session = "ses_before";
        const credential = brokerNatsCredential(podEnvironment(broker.url), () => session);
        await credential.ready();
        const opened = await connect(
          natsConnectionOptions({
            servers: [hub.natsUrl],
            name: "nats-credential-switch",
            credential: { kind: "broker", broker: credential },
          })
        );
        connection = opened;
        const before = opened.subscribe("notifications.envoy.deliver.ses_before");
        await opened.flush();
        expect(before.isClosed()).toBe(false);

        // The host switches: the old session's subject goes, and the new one's opens once the
        // connection presents the new session's token.
        before.unsubscribe();
        session = "ses_after";
        await followNatsCredential(opened, credential);
        // A subscribe the server refuses closes the subscription (nats.js's permission error).
        const after = opened.subscribe("notifications.envoy.deliver.ses_after");
        await opened.flush();
        expect(after.isClosed()).toBe(false);
        expect(broker.state.minted.map((one) => one.session)).toEqual(["ses_before", "ses_after"]);
        // Nothing to move when the session has not changed.
        await followNatsCredential(opened, credential);
        expect(broker.state.minted.length).toBe(2);
      } finally {
        await connection?.close().catch(() => {});
        await hub.stop();
      }
    });

    test("NATS_AUTH=broker against a hub with no auth service fails naming it", async () => {
      // natsd with no callout and no users admits any client and sends no nonce, so a broker
      // token has nothing to be checked by: the dial fails, naming that and the remedy, rather
      // than connecting with no credential while the session believes it presents its token.
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
        await awaitReady("natsd", natsd, natsLog, () => natsdHealthy(natsLog));
        const ports = natsdPorts(natsLog);
        if (ports === undefined) throw new Error(`natsd named no port in ${natsLog}`);
        const credential = brokerNatsCredential(podEnvironment(broker.url), "ses_no_auth");
        await credential.ready();
        const dialled = connect(
          natsConnectionOptions({
            servers: [`nats://127.0.0.1:${ports.natsPort}`],
            name: "nats-credential-no-auth",
            credential: { kind: "broker", broker: credential },
          })
        );
        await expect(dialled).rejects.toThrow(
          "NATS_AUTH is broker, but the NATS server sent no nonce to sign: it runs no auth service, so unset NATS_AUTH for this hub"
        );
      } finally {
        await stopProcess(natsd, "SIGTERM");
      }
    });
  }
);
