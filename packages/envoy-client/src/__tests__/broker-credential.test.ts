// packages/envoy-client/src/__tests__/broker-credential.test.ts
//
// The credential a machine makes out of the identities it holds: when a session uses it (only
// when no bearer is configured at all, or when DISPATCH_TOKEN_BROKER tells it to hold its
// credential at the broker), what it puts on a request, that it renews mid-session with nothing
// restarted, that each session of one process trades under its own name and a session the host
// has left keeps nothing, and that a pool machine has the same route. A person signed in on the
// machine is broker-credential-signins.test.ts's; a pod session whose registration has not landed
// is broker-credential-registration.test.ts's.

import { expect, test } from "bun:test";
import { writeFileSync } from "node:fs";
import * as path from "node:path";
import {
  signedIn,
  stubIssuer,
  thumbprintOf,
  writeSignIn,
} from "@legion/cipher-client/test/sign-ins";
import {
  FORM_AS_OF,
  FORM_LAUNCHER_CREDENTIAL,
  FORM_SCOPE,
  SCOPE_READ,
  SUBJECT_TOKEN_TYPE_PERSON,
} from "@legion/cipher-client/wire";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { consumerDelivery } from "../../test/subscribe-answer";
import { brokerCredential, brokerTokenFor, forgetBrokerSession } from "../broker-credential";
import { type DispatchEnvironment, dispatchSettings } from "../dispatch-config";
import { DispatchClient } from "../dispatch-http";
import { readEnvoyConfig } from "../envoy-config";
import { envoySettings } from "../envoy-settings";
import { createEnvoyClient } from "../transport";
import { brokerStandIns } from "./broker-stand-ins";

const { noFile, podEnvironment, projectedToken, servers, stubBroker, stubDispatch, tempDir } =
  brokerStandIns("broker-credential");

/** The Dispatch token a person holds on the machine, which a session told to trade never uses. */
const PERSON_TOKEN = fixtureSecret("dsp");

test("a configured bearer is still the credential, whatever this machine could trade", () => {
  const broker = stubBroker(600);
  const env = { ...podEnvironment(broker.url, "http://dispatch.test"), DISPATCH_TOKEN: "written" };
  const resolution = dispatchSettings(env, noFile(env));
  if (!resolution.enabled) throw new Error(`Dispatch is off: ${resolution.error}`);
  expect(resolution.credential.scheme).toBe("Bearer");
  expect(resolution.credential.secret()).toBe("written");
});

/** An envoy.json of a person's own, holding the Dispatch token that person writes Dispatch with:
 * what a box's operator has in `~/.config/legion/envoy.json`, and what a session launched on that
 * box reads as its chosen file. */
function personalFile(env: DispatchEnvironment, serverUrl: string, token: string) {
  const file = path.join(tempDir(), "envoy.json");
  writeFileSync(file, JSON.stringify({ dispatch: { serverUrl, token } }));
  return readEnvoyConfig(file, env, { cwd: tempDir() });
}

test("told to hold its credential at the broker, a session reads no person's token", () => {
  // `legion controller start` under `dispatch_token_broker` launches the controller in the
  // operator's own HOME, where an envoy.json holds that person's Dispatch token for the very
  // server the controller writes. Unset, the variable leaves that token the credential, which is
  // the controller writing Dispatch as the operator; set, the session trades for its own.
  const broker = stubBroker(600);
  const env = podEnvironment(broker.url, "http://dispatch.test");
  const config = personalFile(env, "http://dispatch.test", PERSON_TOKEN);

  const shadowed = dispatchSettings(env, config, { session: "legion-demo-controller" });
  if (!shadowed.enabled) throw new Error(`Dispatch is off: ${shadowed.error}`);
  expect(shadowed.credential.secret()).toBe(PERSON_TOKEN);

  const forced = dispatchSettings({ ...env, DISPATCH_TOKEN_BROKER: "1" }, config, {
    session: "legion-demo-controller",
  });
  if (!forced.enabled) throw new Error(`Dispatch is off: ${forced.error}`);
  expect(forced.credential.scheme).toBe("DPoP");
  expect(forced.url).toBe("http://dispatch.test");
});

test("told to hold its credential at the broker, a session reads no bearer variable either", () => {
  const broker = stubBroker(600);
  const env = {
    ...podEnvironment(broker.url, "http://dispatch.test"),
    DISPATCH_TOKEN: PERSON_TOKEN,
    DISPATCH_TOKEN_BROKER: "1",
  };
  const resolution = dispatchSettings(env, noFile(env), { session: "legion-demo-controller" });
  if (!resolution.enabled) throw new Error(`Dispatch is off: ${resolution.error}`);
  expect(resolution.credential.scheme).toBe("DPoP");
});

test("told to hold a credential it cannot trade for, a session writes Dispatch with nothing", () => {
  // The refusal is the point: a machine whose broker or identity has gone is one that writes
  // nothing, never one that falls back to the bearer the box happens to hold.
  const broker = stubBroker(600);
  const env = {
    ...podEnvironment(broker.url, "http://dispatch.test"),
    DISPATCH_TOKEN: PERSON_TOKEN,
    DISPATCH_TOKEN_BROKER: "1",
  };

  const noBroker = dispatchSettings({ ...env, AGENT_SECRETS_URL: "" }, noFile(env));
  expect(noBroker.enabled).toBe(false);
  expect(noBroker.enabled === false && noBroker.error).toContain(
    "AGENT_SECRETS_URL names no secrets broker"
  );

  const noIdentity = dispatchSettings({ ...env, AGENT_SECRETS_MACHINE_IDENTITY: "" }, noFile(env));
  expect(noIdentity.enabled === false && noIdentity.error).toContain(
    "AGENT_SECRETS_MACHINE_IDENTITY names no identity this machine proves"
  );

  // A pool machine trades here too, so a pool named without its own variables says which one is
  // missing, as every other identity does, rather than reading as a machine with no route.
  const pool = dispatchSettings({ ...env, AGENT_SECRETS_MACHINE_IDENTITY: "pool" }, noFile(env));
  expect(pool.enabled === false && pool.error).toContain(
    "AGENT_SECRETS_MACHINE_AUDIENCE is required by AGENT_SECRETS_MACHINE_IDENTITY=pool"
  );

  // A named identity missing its own variables still says which variable.
  const halfNamed = dispatchSettings({ ...env, AGENT_SECRETS_MACHINE_TOKEN_FILE: "" }, noFile(env));
  expect(halfNamed.enabled === false && halfNamed.error).toContain(
    "AGENT_SECRETS_MACHINE_TOKEN_FILE is required"
  );
});

test("a DISPATCH_TOKEN_BROKER that is neither 1 nor unset is refused", () => {
  // Read as on, a typo would turn a person's session into one that writes nothing; read as off,
  // it would put a deployment that meant the broker route back on the operator's bearer.
  const broker = stubBroker(600);
  const env = {
    ...podEnvironment(broker.url, "http://dispatch.test"),
    DISPATCH_TOKEN_BROKER: "true",
  };
  const resolution = dispatchSettings(env, noFile(env));
  expect(resolution.enabled).toBe(false);
  expect(resolution.enabled === false && resolution.error).toContain(
    "DISPATCH_TOKEN_BROKER takes 1"
  );
  // Its value is not quoted, which a token pasted into the wrong variable would be.
  expect(resolution.enabled === false && resolution.error).not.toContain('"true"');
});

/** The Dispatch `env` resolves for a caller acting as `session`, which must be on. */
function dispatchOf(env: DispatchEnvironment, session: string): DispatchClient {
  const resolution = dispatchSettings(env, noFile(env), { session });
  if (!resolution.enabled) throw new Error(`Dispatch is off: ${resolution.error}`);
  expect(resolution.credential.scheme).toBe("DPoP");
  return new DispatchClient(resolution.url, resolution.credential);
}

/** The error `env` turns Dispatch off with; Dispatch on fails the test. */
function offError(env: DispatchEnvironment): string {
  const resolution = dispatchSettings(env, noFile(env), { session: "session-one" });
  if (resolution.enabled) throw new Error("Dispatch is on");
  return resolution.error ?? `no error, the notice: ${resolution.notice}`;
}

test("DISPATCH_TOKEN_BROKER_READ=1 makes the Dispatch trade ask for scope read", async () => {
  const broker = stubBroker(600);
  const dispatch = stubDispatch();
  const env = { ...podEnvironment(broker.url, dispatch.url), DISPATCH_TOKEN_BROKER_READ: "1" };

  await dispatchOf(env, "example-reader-session").listAllIssues({});

  expect(broker.trades).toHaveLength(1);
  expect(broker.trades[0]?.form.get(FORM_SCOPE)).toBe(SCOPE_READ);
  expect(broker.trades[0]?.form.has(FORM_AS_OF)).toBe(false);
  expect(broker.trades[0]?.form.get("session")).toBe("example-reader-session");
  // The pod proves a key this process made, so its read trade sends no holder proof.
  expect(broker.trades[0]?.holder).toBe("");
  expect(dispatch.asked.map((one) => one.authorization)).toEqual([
    `DPoP ${broker.trades[0]?.token}`,
  ]);
});

test("DISPATCH_TOKEN_BROKER_AS_OF beside it asks for that cut", async () => {
  const broker = stubBroker(600);
  const dispatch = stubDispatch();
  const env = podEnvironment(broker.url, dispatch.url);
  const cuts = ["2026-11-19T08:30:00Z", "2026-11-19T09:30:00.123456+01:00"];
  for (const [at, cut] of cuts.entries()) {
    await dispatchOf(
      { ...env, DISPATCH_TOKEN_BROKER_READ: "1", DISPATCH_TOKEN_BROKER_AS_OF: ` ${cut} ` },
      `session-${at}`
    ).listAllIssues({});
  }
  // Sent as written, trimmed: the broker reads the time, and its clock alone says it has passed.
  expect(
    broker.trades.map((trade) => [trade.form.get(FORM_SCOPE), trade.form.get(FORM_AS_OF)])
  ).toEqual(cuts.map((cut) => [SCOPE_READ, cut]));
});

test("the read settings take the broker route and read no DISPATCH_TOKEN", async () => {
  // The shell holds a writing bearer too, in a variable and in the person's own envoy.json for
  // the very server it reads: under the read settings neither is ever sent.
  const broker = stubBroker(600);
  const dispatch = stubDispatch();
  const env = {
    ...podEnvironment(broker.url, dispatch.url),
    DISPATCH_TOKEN: PERSON_TOKEN,
    DISPATCH_TOKEN_BROKER_READ: "1",
  };
  const config = personalFile(env, dispatch.url, PERSON_TOKEN);
  const resolution = dispatchSettings(env, config, { session: "session-one" });
  if (!resolution.enabled) throw new Error(`Dispatch is off: ${resolution.error}`);
  expect(resolution.credential.scheme).toBe("DPoP");
  await new DispatchClient(resolution.url, resolution.credential).listAllIssues({});
  expect(broker.trades.map((trade) => trade.form.get(FORM_SCOPE))).toEqual([SCOPE_READ]);
  expect(dispatch.asked.map((one) => one.authorization)).toEqual([
    `DPoP ${broker.trades[0]?.token}`,
  ]);
  expect(JSON.stringify(dispatch.asked)).not.toContain(PERSON_TOKEN);

  // With nothing to trade, Dispatch is off rather than written with the bearer the shell holds.
  const noBroker = dispatchSettings({ ...env, AGENT_SECRETS_URL: "" }, config);
  expect(noBroker.enabled).toBe(false);
  expect(noBroker.enabled === false && noBroker.error).toContain(
    "DISPATCH_TOKEN_BROKER_READ is set, so this session's Dispatch credential is traded at the secrets broker"
  );
  expect(noBroker.enabled === false && noBroker.error).toContain(
    "AGENT_SECRETS_URL names no secrets broker"
  );
});

test("a cut without DISPATCH_TOKEN_BROKER_READ=1 turns Dispatch off naming both settings", () => {
  // A bearer that would write: the cut is a mistake, never a reason to fall back on it.
  const env = {
    ...podEnvironment("http://broker.test", "http://dispatch.test"),
    DISPATCH_TOKEN: PERSON_TOKEN,
    DISPATCH_TOKEN_BROKER_AS_OF: "2026-11-19T08:30:00Z",
  };
  for (const broken of [env, { ...env, DISPATCH_TOKEN_BROKER: "1" }]) {
    const error = offError(broken);
    expect(error).toContain("DISPATCH_TOKEN_BROKER_AS_OF");
    expect(error).toContain("DISPATCH_TOKEN_BROKER_READ=1");
  }
});

test("DISPATCH_TOKEN_BROKER_READ other than 1 turns Dispatch off naming it", () => {
  const env = {
    ...podEnvironment("http://broker.test", "http://dispatch.test"),
    DISPATCH_TOKEN: "written",
  };
  for (const value of ["true", "0", "yes", "read"]) {
    const error = offError({ ...env, DISPATCH_TOKEN_BROKER_READ: value });
    expect(error).toContain("DISPATCH_TOKEN_BROKER_READ takes 1");
    // Its value is not quoted, which a token pasted into the wrong variable would be.
    expect(error).not.toContain(`"${value}"`);
    expect(error).not.toContain(`'${value}'`);
  }
  // Unset or blank, it reads as off, and the bearer is the credential.
  const blank = dispatchSettings({ ...env, DISPATCH_TOKEN_BROKER_READ: "  " }, noFile(env));
  expect(blank.enabled && blank.credential.scheme).toBe("Bearer");
});

test("a cut that is not RFC 3339, or is the zero time, turns Dispatch off naming it", () => {
  const env = {
    ...podEnvironment("http://broker.test", "http://dispatch.test"),
    DISPATCH_TOKEN_BROKER_READ: "1",
  };
  for (const cut of [
    "yesterday",
    "2026-11-19",
    "2026-11-19T08:30:00",
    "2026-11-19 08:30:00Z",
    "2026-11-19t08:30:00z",
    "2026-11-19T08:30:00.1234567Z",
    "2026-11-19T08:30Z",
    "2026-02-30T08:30:00Z",
    "2026-13-01T08:30:00Z",
    "2026-11-19T24:00:00Z",
    "2026-11-19T08:60:00Z",
    "2026-11-19T08:30:00+0100",
    "1763541000",
    // The zero time, which the broker takes for no cut at all, so it refuses it, under any offset.
    "0001-01-01T00:00:00Z",
    "0001-01-01T00:00:00.000000Z",
    "0001-01-01T01:00:00+01:00",
  ]) {
    const error = offError({ ...env, DISPATCH_TOKEN_BROKER_AS_OF: cut });
    expect(error, cut).toContain("DISPATCH_TOKEN_BROKER_AS_OF is not an RFC 3339 time");
    expect(error, cut).not.toContain(cut);
  }
  // A second after it is a cut, as the broker takes it.
  const second = dispatchSettings(
    { ...env, DISPATCH_TOKEN_BROKER_AS_OF: "0001-01-01T00:00:01Z" },
    noFile(env),
    { session: "session-one" }
  );
  expect(second.enabled).toBe(true);
});

test("the Envoy listener's and NATS's trades ask for no scope under the read settings", async () => {
  // Both refuse a read-only token, and a session's inbound messages must keep working, so they
  // keep the session's shared client while Dispatch's credential is a client of its own.
  const broker = stubBroker(600);
  const dispatch = stubDispatch();
  const cut = "2026-11-19T08:30:00Z";
  const env = {
    ...podEnvironment(broker.url, dispatch.url),
    DISPATCH_TOKEN_BROKER_READ: "1",
    DISPATCH_TOKEN_BROKER_AS_OF: cut,
  };
  await dispatchOf(env, "session-one").listAllIssues({});
  const envoy = envoySettings(env, noFile(env), { nats: false, session: "session-one" });
  if (!envoy.enabled) throw new Error(`Envoy is off: ${envoy.error ?? envoy.notice}`);
  const listener = await envoy.credential?.headers("GET", `${dispatch.url}/v1/interests/a`);
  const nats = await brokerTokenFor(env, "session-one")();
  // Dispatch's again: its own client answers out of the token it holds.
  await dispatchOf(env, "session-one").listAllIssues({});

  expect(
    broker.trades.map((trade) => [
      trade.form.get("session"),
      trade.form.get(FORM_SCOPE),
      trade.form.get(FORM_AS_OF),
    ])
  ).toEqual([
    ["session-one", SCOPE_READ, cut],
    ["session-one", null, null],
  ]);
  expect(listener?.Authorization).toBe(`DPoP ${broker.trades[1]?.token}`);
  expect(nats.token.accessToken).toBe(broker.trades[1]?.token);
  expect(dispatch.asked.map((one) => one.authorization)).toEqual([
    `DPoP ${broker.trades[0]?.token}`,
    `DPoP ${broker.trades[0]?.token}`,
  ]);
});

test("a person's trade under the read setting sends no launcher_credential", async () => {
  const broker = stubBroker(600);
  const dispatch = stubDispatch();
  const clientId = "example-device-client";
  const issuer = await stubIssuer({ clientId, person: "ada@example.com" });
  servers.push(issuer);
  const record = await signedIn("ada@example.com", { client_id: clientId });
  const signins = path.join(tempDir(), "signins");
  await writeSignIn(signins, record);
  const secret = path.join(tempDir(), "client-secret");
  writeFileSync(secret, `secret-${crypto.randomUUID()}\n`, { mode: 0o600 });
  const env = {
    ...podEnvironment(broker.url, dispatch.url),
    AGENT_SECRETS_SIGNIN_DIR: signins,
    AGENT_SECRETS_SIGNIN_ISSUER: issuer.url,
    AGENT_SECRETS_SIGNIN_CLIENT_ID: clientId,
    AGENT_SECRETS_SIGNIN_CLIENT_SECRET_FILE: secret,
    DISPATCH_TOKEN_BROKER_READ: "1",
  };

  await dispatchOf(env, "session-one").listAllIssues({});

  const [trade] = broker.trades;
  expect(broker.trades).toHaveLength(1);
  expect(trade?.form.get("subject_token_type")).toBe(SUBJECT_TOKEN_TYPE_PERSON);
  expect(trade?.form.get(FORM_SCOPE)).toBe(SCOPE_READ);
  // Absent, a read trade's launcher_credential reads as declined, and a person's trade may not
  // send `false`.
  expect(trade?.form.has(FORM_LAUNCHER_CREDENTIAL)).toBe(false);
  // The token is bound to the holder's key, and Dispatch's proof is signed with it, never with
  // the box key the sign-in is proved by.
  const [holderJwk, dispatchJwk] = [trade?.holder, dispatch.asked[0]?.proof].map(
    (proof) =>
      JSON.parse(Buffer.from((proof ?? "").split(".")[0] ?? "", "base64url").toString("utf8"))
        .jwk as JsonWebKey
  );
  if (dispatchJwk === undefined) throw new Error("Dispatch was sent no proof");
  expect(dispatchJwk).toEqual(holderJwk);
  expect(await thumbprintOf(dispatchJwk)).not.toBe(record.thumbprint);
});

test("with no bearer at all, the session's credential is its own identity traded at the broker", async () => {
  const broker = stubBroker(600);
  const dispatch = stubDispatch();
  const env = podEnvironment(broker.url, dispatch.url);
  const resolution = dispatchSettings(env, noFile(env), { session: "example-worker-session" });
  if (!resolution.enabled) throw new Error(`Dispatch is off: ${resolution.error}`);
  expect(resolution.credential.scheme).toBe("DPoP");

  await new DispatchClient(resolution.url, resolution.credential).listAllIssues({});

  // One trade of the pod's own token, under the session this caller named: nothing in the
  // environment does.
  expect(broker.trades.length).toBe(1);
  expect(broker.trades[0]?.form.get("subject_token")).toBe(projectedToken);
  expect(broker.trades[0]?.form.get("session")).toBe("example-worker-session");
  // The access token is presented under the DPoP scheme, with a proof naming this request and
  // hashing the token it is presented beside: a bearer copy of it buys nothing.
  const asked = dispatch.asked[0];
  expect(asked?.authorization).toBe(`DPoP ${broker.trades[0]?.token}`);
  const claims = JSON.parse(
    Buffer.from(asked?.proof.split(".")[1] ?? "", "base64url").toString("utf8")
  );
  expect(claims.htm).toBe("GET");
  expect(claims.htu).toBe(`${dispatch.url}/api/v1/issues`);
  const digest = await crypto.subtle.digest(
    "SHA-256",
    new TextEncoder().encode(broker.trades[0]?.token ?? "")
  );
  expect(claims.ath).toBe(Buffer.from(digest).toString("base64url"));
});

test("a session past its token's life trades again, with nothing restarted", async () => {
  // One second of life is the broker's own minimum (BROKER_ACCESS_TOKEN_SECONDS), so every
  // request after the first is inside the renewal window.
  const broker = stubBroker(1);
  const dispatch = stubDispatch();
  const env = podEnvironment(broker.url, dispatch.url);
  const resolution = dispatchSettings(env, noFile(env), { session: "example-worker-session" });
  if (!resolution.enabled) throw new Error(`Dispatch is off: ${resolution.error}`);
  const client = new DispatchClient(resolution.url, resolution.credential);

  await client.listAllIssues({});
  await client.listAllIssues({});

  expect(broker.trades.length).toBe(2);
  expect(dispatch.asked.map((one) => one.authorization)).toEqual([
    `DPoP ${broker.trades[0]?.token}`,
    `DPoP ${broker.trades[1]?.token}`,
  ]);
});

test("each session of one process trades under its own name, read at every trade", async () => {
  // One Oh My Pi process holds several sessions over its life, and Claude Code mints a new id on
  // /clear. A token names one session, and the listener answers 403 to a session presenting
  // another's, so the name is read when the trade happens rather than fixed at resolution.
  const broker = stubBroker(600);
  const dispatch = stubDispatch();
  const env = podEnvironment(broker.url, dispatch.url);
  let current = "session-one";
  const resolution = dispatchSettings(env, noFile(env), { session: () => current });
  if (!resolution.enabled) throw new Error(`Dispatch is off: ${resolution.error}`);

  await new DispatchClient(resolution.url, resolution.credential).listAllIssues({});
  current = "session-two";
  await new DispatchClient(resolution.url, resolution.credential).listAllIssues({});
  // Back to the first: its client is the one this process already made, so no third trade.
  current = "session-one";
  await new DispatchClient(resolution.url, resolution.credential).listAllIssues({});

  expect(broker.trades.map((trade) => trade.form.get("session"))).toEqual([
    "session-one",
    "session-two",
  ]);
  expect(dispatch.asked.map((one) => one.authorization)).toEqual([
    `DPoP ${broker.trades[0]?.token}`,
    `DPoP ${broker.trades[1]?.token}`,
    `DPoP ${broker.trades[0]?.token}`,
  ]);
});

test("the listener takes the same credential, and a configured bearer still wins", () => {
  const broker = stubBroker(600);
  const env = podEnvironment(broker.url, "http://listener.test");
  const brokered = envoySettings(env, noFile(env), { nats: false, session: "session-one" });
  if (!brokered.enabled) throw new Error("Envoy is off");
  expect(brokered.credential?.scheme).toBe("DPoP");

  const listenerToken = fixtureSecret("listener");
  const written = envoySettings({ ...env, ENVOY_TOKEN: listenerToken }, noFile(env), {
    nats: false,
  });
  if (!written.enabled) throw new Error("Envoy is off");
  expect(written.credential?.scheme).toBe("Bearer");
  expect(written.credential?.secret()).toBe(listenerToken);
});

/** A broker that mints a random token for each trade and records it against the session the
 *  trade asked to write under, as the real one does (the listener reads that session back at the
 *  introspection route): `sessionOf` maps each token minted to its session. */
function sessionBroker(): { url: string; sessions: string[]; sessionOf: Map<string, string> } {
  const sessions: string[] = [];
  const sessionOf = new Map<string, string>();
  const server = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch: async (request) => {
      const session = new URLSearchParams(await request.text()).get("session") ?? "";
      sessions.push(session);
      const token = fixtureSecret("broker");
      sessionOf.set(token, session);
      return Response.json({
        access_token: token,
        issued_token_type: "urn:ietf:params:oauth:token-type:access_token",
        token_type: "DPoP",
        expires_in: 600,
      });
    },
  });
  servers.push(server);
  return { url: `http://127.0.0.1:${server.port}`, sessions, sessionOf };
}

/** One call the stub listener was sent: the session its body acts as, and the token it
 *  presented. */
interface Acted {
  readonly path: string;
  readonly body: string;
  readonly token: string;
}

/** A listener that binds the session a broker token was minted for (`sessionOf`, a
 *  {@link sessionBroker}'s), as the real one does (`sessionNotBound`,
 *  packages/envoy/cmd/listener/brokerauth.go): a body naming another session is 403, and nothing
 *  else about the call is judged. */
function sessionBoundListener(sessionOf: ReadonlyMap<string, string>): {
  url: string;
  acted: Acted[];
} {
  const acted: Acted[] = [];
  const server = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch: async (request) => {
      const token = (request.headers.get("Authorization") ?? "").replace(/^DPoP /, "");
      const sent: unknown = JSON.parse(await request.text());
      const named =
        typeof sent === "object" && sent !== null && "session_id" in sent ? sent.session_id : "";
      const body = typeof named === "string" ? named : "";
      acted.push({ path: new URL(request.url).pathname, body, token });
      if (sessionOf.get(token) !== body) {
        return Response.json(
          { error: `this request acts as session ${body}, and its credential names another` },
          { status: 403 }
        );
      }
      return Response.json({
        session_id: body,
        machine_id: "box",
        dir: "/work",
        topics: [],
        delivery: consumerDelivery(body),
      });
    },
  });
  servers.push(server);
  return { url: `http://127.0.0.1:${server.port}`, acted };
}

test("a role handed back for the session this process left presents that session's credential", async () => {
  // One Oh My Pi process switches sessions, and the session it leaves may still hold a role at
  // the listener. The hand-back acts as that old session, so it must present the old session's
  // own token: under the token of the session running now the listener answers 403, the
  // hand-back fails, and the role stays with a session whose subject this process has closed
  // until the registration's TTL lapses.
  const broker = sessionBroker();
  const listener = sessionBoundListener(broker.sessionOf);
  const env = podEnvironment(broker.url, listener.url);
  let current = "session-one";
  const resolution = envoySettings(env, noFile(env), { nats: false, session: () => current });
  if (!resolution.enabled)
    throw new Error(`Envoy is off: ${resolution.error ?? resolution.notice}`);
  const client = createEnvoyClient({
    baseUrl: resolution.url,
    fetch,
    ...(resolution.credential === undefined ? {} : { credential: resolution.credential }),
  });

  await client.setRole({ sessionID: "session-one", role: "controller" });
  // The switch: `bindDispatchSession` has moved this process to the new session, and the pass
  // that follows hands the old session's role back.
  current = "session-two";
  await client.unsubscribe({
    sessionID: "session-one",
    topics: ["notifications.role.controller"],
  });
  await client.subscribe({
    sessionID: "session-two",
    directory: "/work",
    topics: [],
    port: 0,
    title: "",
    driving: false,
    plugin: { name: "@sjawhar/pi-envoy", version: "0.0.0" },
    delivery: { consumer: true, epoch: "epoch-1" },
  });

  // Each call presented a token the broker minted for the session its body acts as.
  expect(
    listener.acted.map((one) => [one.path, one.body, broker.sessionOf.get(one.token)])
  ).toEqual([
    ["/v1/roles/set", "session-one", "session-one"],
    ["/v1/interests/unsubscribe", "session-one", "session-one"],
    ["/v1/interests/subscribe", "session-two", "session-two"],
  ]);
  // One trade per session, and the session this process left answered out of the token it
  // already held rather than trading again.
  expect(broker.sessions).toEqual(["session-one", "session-two"]);
});

test("a session the host has left keeps no key and no renewing token", async () => {
  // One Oh My Pi or Claude Code process binds a new session id on `/clear` and on every switch,
  // and each one it traded under is a key pair and a token that trades again at every renewal.
  // The host says when it has made its last call for a session it left — Oh My Pi's pass once
  // that session's role is handed back, Claude Code's handoff once the old id is deregistered —
  // and that session's client goes, while the session running now keeps the token it holds.
  const broker = sessionBroker();
  const listener = sessionBoundListener(broker.sessionOf);
  const env = podEnvironment(broker.url, listener.url);
  let current = "session-one";
  const resolution = envoySettings(env, noFile(env), { nats: false, session: () => current });
  if (!resolution.enabled)
    throw new Error(`Envoy is off: ${resolution.error ?? resolution.notice}`);
  const client = createEnvoyClient({
    baseUrl: resolution.url,
    fetch,
    ...(resolution.credential === undefined ? {} : { credential: resolution.credential }),
  });
  const subscribeAs = (sessionID: string) =>
    client.subscribe({
      sessionID,
      directory: "/work",
      topics: [],
      port: 0,
      title: "",
      driving: false,
      plugin: { name: "@sjawhar/pi-envoy", version: "0.0.0" },
      delivery: { consumer: true, epoch: "epoch-1" },
    });

  await client.setRole({ sessionID: "session-one", role: "controller" });
  current = "session-two";
  await subscribeAs("session-two");
  expect(broker.sessions).toEqual(["session-one", "session-two"]);

  forgetBrokerSession("session-one");
  // The session left has nothing to present any more, so a call for it trades afresh, as one
  // this process had never traded for does.
  await client.unsubscribe({
    sessionID: "session-one",
    topics: ["notifications.role.controller"],
  });
  // The session running now was untouched by the drop: it still answers out of its own token.
  await subscribeAs("session-two");
  expect(broker.sessions).toEqual(["session-one", "session-two", "session-one"]);
});

test("a pool machine has a route: Dispatch and the listener are given its broker credential", () => {
  // This client federates at a workload identity pool as the Go client does
  // (@legion/cipher-client/pool-identity), so a pool machine with no bearer trades at the broker
  // like any other, rather than reading as a machine with nothing to present.
  const broker = stubBroker(600);
  const env = {
    ...podEnvironment(broker.url, "http://listener.test"),
    AGENT_SECRETS_MACHINE_IDENTITY: "pool",
    AGENT_SECRETS_MACHINE_AUDIENCE: "example-machine-client-id",
    AGENT_SECRETS_POOL_AUDIENCE:
      "//iam.googleapis.com/projects/000000000000/locations/global/workloadIdentityPools/example-pool/providers/example-provider",
    AGENT_SECRETS_POOL_ACCOUNT: "example-fleet@example-project.iam.gserviceaccount.com",
  };
  const envoy = envoySettings(env, noFile(env), { nats: false, session: "session-one" });
  if (!envoy.enabled) throw new Error(`Envoy is off: ${envoy.error ?? envoy.notice}`);
  expect(envoy.credential?.scheme).toBe("DPoP");
  const dispatch = dispatchSettings(env, noFile(env), { session: "session-one" });
  if (!dispatch.enabled) throw new Error(`Dispatch is off: ${dispatch.error}`);
  expect(dispatch.credential.scheme).toBe("DPoP");

  // An identity this client cannot prove at all is still a mistake in the deployment.
  const unknown = envoySettings(
    { ...env, AGENT_SECRETS_MACHINE_IDENTITY: "nonsense" },
    noFile(env),
    { nats: false }
  );
  expect(unknown.enabled).toBe(false);
});

test("stale() has the session's client drop its token, so its next headers() trades afresh", async () => {
  // What a caller does when a server says the token it presented is not live: Dispatch's
  // `401 BROKER_TOKEN_NOT_ACTIVE` for a token a revoke ended before it lapsed.
  const broker = stubBroker(600);
  const env = podEnvironment(broker.url, "http://dispatch.test");
  const credential = brokerCredential(env, "session-one");
  const url = "http://dispatch.test/api/v1/issues";

  await credential.headers("GET", url);
  await credential.headers("GET", url);
  expect(broker.trades).toHaveLength(1);
  credential.stale?.();
  expect((await credential.headers("GET", url)).Authorization).toBe(
    `DPoP ${broker.trades[1]?.token}`
  );

  // A session a call names is told apart as `headers` tells it apart: only its client drops.
  await credential.headers("POST", url, "session-two");
  credential.stale?.("session-two");
  expect((await credential.headers("POST", url, "session-two")).Authorization).toBe(
    `DPoP ${broker.trades[3]?.token}`
  );
  expect((await credential.headers("GET", url)).Authorization).toBe(
    `DPoP ${broker.trades[1]?.token}`
  );
  expect(broker.trades.map((trade) => trade.form.get("session"))).toEqual([
    "session-one",
    "session-one",
    "session-two",
    "session-two",
  ]);
});

// Red team 3 (the review of 4dad905e68): a caller told that the token it presented is not live
// names that presentation, and the credential drops that token alone. A host that moved to
// another session between the presentation and the answer, or a client that has traded a fresh
// token since, keeps the token it holds: dropping a still-valid token costs a trade for nothing.
test("stale() for a presentation drops that presentation's token alone, never the current session's or a fresher one", async () => {
  const broker = stubBroker(600);
  const env = podEnvironment(broker.url, "http://dispatch.test");
  let session = "session-one";
  const credential = brokerCredential(env, () => session);
  const url = "http://dispatch.test/api/v1/issues";

  const deadOne = await credential.headers("GET", url);
  // The host moves to another session while Dispatch answers the first presentation.
  session = "session-two";
  const two = await credential.headers("GET", url);
  expect(broker.trades).toHaveLength(2);

  credential.stale?.(undefined, deadOne);
  // The session the host is on now keeps its token: no trade.
  expect((await credential.headers("GET", url)).Authorization).toBe(two.Authorization);
  expect(broker.trades).toHaveLength(2);
  // The session whose token was dead trades afresh when it is next presented.
  session = "session-one";
  expect((await credential.headers("GET", url)).Authorization).toBe(
    `DPoP ${broker.trades[2]?.token}`
  );
  expect(broker.trades).toHaveLength(3);

  // A presentation told stale after its client traded a fresher token leaves the fresher one.
  const fresher = await credential.headers("GET", url);
  credential.stale?.(undefined, deadOne);
  expect((await credential.headers("GET", url)).Authorization).toBe(fresher.Authorization);
  expect(broker.trades).toHaveLength(3);
});
