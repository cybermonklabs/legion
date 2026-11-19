// packages/envoy-client/src/__tests__/nats-credential.test.ts
//
// A session's broker NATS credential on the wire: what its authenticator puts on a CONNECT, that
// it is the token and key the session's HTTP calls present, that nats.js is told never to give up
// over a refused one, how the keeper moves the connection to a renewed token before the old one
// lapses, and, on a real nats-server behind the real auth service, that a connection whose token
// lapsed while the broker was away is admitted again once a token is live, that a session switch
// subscribes the new session only once the connection is admitted as it, and that a hub with no
// auth service fails the dial naming it; and which credential `envoySettings` resolves for a
// session: a seed, the session's broker token, or none, and the reason inbound messages are off
// where the settings conflict or leave a gap. How long its trades wait, and `renew` and
// `followNatsCredential` against stand-in connections, are nats-credential-deadlines.test.ts's.

import { afterEach, beforeAll, beforeEach, describe, expect, jest, spyOn, test } from "bun:test";
import { createHash, createPublicKey, verify } from "node:crypto";
import { closeSync, mkdirSync, openSync, readFileSync, writeFileSync } from "node:fs";
import { createRequire } from "node:module";
import * as path from "node:path";
import { thumbprintOf } from "@legion/broker-client/test/sign-ins";
import { RETRY_FIRST_WAIT_MS, RETRY_MAX_WAIT_MS } from "@legion/broker-client/token-client";
import { turn, turnsUntil } from "@legion/test-fixtures/fake-clock";
import type { Subprocess } from "bun";
import { connect, Events, type NatsConnection, type Status } from "nats";
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
import { natsConnectionOptions } from "../nats-connection";
import {
  type BrokerNatsCredential,
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
    expect(settings.enabled && settings.inbound).toEqual({
      off: "NATS_AUTH is broker, but this process names no session for its NATS connection's token",
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
    // presents it, and nats.js redials every 2 s.
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
    // token, and nats.js redials every 2 s. 20 refusals, 40 s of it: one drop and one trade, not one
    // every second refusal.
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
  const budget = { timeoutMs: 60_000, tailLines: 10 };
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
    await awaitReady("natsd", natsd, natsLog, budget, () => natsdHealthy(natsLog));
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
    await awaitReady("calloutd", calloutd, calloutLog, budget, async () =>
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
    }, 300_000);

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
        const reconnected: number[] = [];
        void (async () => {
          for await (const status of opened.status()) {
            note(`status ${status.type} ${JSON.stringify(status.data)}`);
            if (status.type === Events.Reconnect) reconnected.push(Date.now());
          }
        })();
        // Its own delivery channel is what the service grants a session.
        opened.subscribe("notifications.envoy.deliver.ses_e2e");
        await opened.flush();
        const warnings: string[] = [];
        stopKeeping = keepNatsCredential(opened, credential, (text) => {
          note(`keeper: ${text}`);
          warnings.push(text);
        });

        // The broker goes away: the token lapses with no other to take its place, the server
        // drops the user it admitted, and each reconnect is refused.
        note("the broker goes away");
        broker.state.down = true;
        const deadline = Date.now() + 20_000;
        while (hub.refusals() < 2 && Date.now() < deadline) await Bun.sleep(100);
        if (hub.refusals() < 2)
          throw new Error(
            [
              `natsd refused ${hub.refusals()} connection(s) in 20 s, not 2 or more (the auth service logged ${hub.serviceRefusals()} refusal(s)); the connection is ${opened.isClosed() ? "closed" : "open"}`,
              "the client, by milliseconds since the row began:",
              ...trace,
              hub.logs(),
            ].join("\n")
          );
        expect(opened.isClosed()).toBe(false);
        // The keeper warns once its renewal fails, and natsd refuses once the token lapses on the
        // server's clock: two clocks, so no order between the two is promised. The keeper's first
        // renewal comes halfway to the token's expiry (`renewDelay`), and waits for its trade at
        // most TRADE_DEADLINE_MS before it warns (`keepNatsCredential`'s tick), so the warning is
        // promised within that bound of the broker going away, not by natsd's second refusal.
        const warnedBy = Date.now() + TRADE_DEADLINE_MS + 2_000;
        while (warnings.length === 0 && Date.now() < warnedBy) await Bun.sleep(100);
        expect(warnings.length).toBeGreaterThan(0);
        for (const one of broker.state.minted) {
          expect(warnings.join("\n")).not.toContain(one.value);
        }

        // The broker is back: a token is traded and the connection is admitted again. It mints
        // tokens of a session's usual life again, so the keeper does not renew within the row:
        // with 3-second tokens it renews about every 1.5 s and reconnects to present each new
        // token, and the round trip below raced that reconnect (NatsError DISCONNECT).
        const back = Date.now();
        // How long the connection may take to come back, from the token client's schedule: only a
        // token landing starts it over (`BrokerTokenClient.#run`), so the next trade is due at most
        // RETRY_FIRST_WAIT_MS doubled once for each trade that failed while the broker was away,
        // capped at RETRY_MAX_WAIT_MS, after the last of them. The connection then presents the
        // token it took at its next reconnect but one (the authenticator's renewal lands it for the
        // next CONNECT), each after `reconnectTimeWait` (2 s, nats-connection.ts), and 5 s more for
        // the handshake on a loaded machine.
        const failedTrades = Math.max(broker.state.trades - broker.state.minted.length, 1);
        const nextTradeMs = Math.min(
          RETRY_FIRST_WAIT_MS * 2 ** (failedTrades - 1),
          RETRY_MAX_WAIT_MS
        );
        const backBy = back + nextTradeMs + 2 * 2_000 + 5_000;
        broker.state.seconds = 900;
        broker.state.down = false;
        while (!reconnected.some((at) => at > back) && Date.now() < backBy) await Bun.sleep(100);
        expect(reconnected.some((at) => at > back)).toBe(true);
        expect(opened.isClosed()).toBe(false);
        expect(await opened.rtt()).toBeGreaterThanOrEqual(0);
      } finally {
        stopKeeping();
        await connection?.close().catch(() => {});
        await hub.stop();
      }
    }, 180_000);

    test("a token revoked before it lapses is dropped at the second refusal, and the connection comes back", async () => {
      // The broker revokes the session's token: the auth service, which asks it about each
      // token, refuses every CONNECT that presents it, and nats.js redials every 2 s. Tokens
      // live ten minutes, so only the keeper's drop gets the connection a token it admits.
      const broker = stubBroker(600);
      const hub = await startCalloutHub("revoked", broker.url);
      let connection: NatsConnection | undefined;
      let stopKeeping = () => {};
      const started = Date.now();
      const trace: string[] = [];
      const note = (what: string) => trace.push(`${Date.now() - started} ms: ${what}`);
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
        await opened.flush();

        note("the broker revokes the token, and the connection redials");
        const revoked = broker.state.minted[0]?.value ?? "";
        broker.state.revoked.add(revoked);
        const at = Date.now();
        await opened.reconnect();
        // Two refusals 2 s apart, the trade, and the redial after it, with 10 s for the
        // handshakes on a loaded machine.
        const backBy = at + 3 * 2_000 + 10_000;
        while (!reconnected.some((when) => when > at) && Date.now() < backBy) await Bun.sleep(100);
        if (!reconnected.some((when) => when > at))
          throw new Error(
            [
              `the connection did not come back within ${(backBy - at) / 1000} s of the revoke; natsd refused ${hub.refusals()} connection(s), and the broker minted ${broker.state.minted.length} token(s)`,
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
        for (const one of broker.state.minted) expect(warnings.join("\n")).not.toContain(one.value);
        expect(opened.isClosed()).toBe(false);
        expect(await opened.rtt()).toBeGreaterThanOrEqual(0);
      } finally {
        stopKeeping();
        await connection?.close().catch(() => {});
        await hub.stop();
      }
    }, 120_000);

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
    }, 120_000);

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
        await awaitReady("natsd", natsd, natsLog, { timeoutMs: 60_000, tailLines: 10 }, () =>
          natsdHealthy(natsLog)
        );
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
    }, 120_000);
  }
);
