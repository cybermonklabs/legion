// packages/envoy-client/src/__tests__/broker-credential.test.ts
//
// The credential a machine makes out of the identities it holds: when a session uses it (only
// when no bearer is configured at all), what it puts on a request, that it renews mid-session
// with nothing restarted, and, where a person signed in on the machine, that it trades that
// person under the right session name, remembers a sign-in refused for good, and never trades
// the machine in its place.

import { afterEach, expect, test } from "bun:test";
import { writeFileSync } from "node:fs";
import { rm } from "node:fs/promises";
import * as path from "node:path";
import { type SignInRecord, signInSessionName } from "@legion/broker-client/signins";
import {
  type IssuerAnswer,
  signedIn,
  signInPath,
  stubIssuer,
  thumbprintOf,
  writeSignIn,
} from "@legion/broker-client/test/sign-ins";
import { SUBJECT_TOKEN_TYPE_PERSON } from "@legion/broker-client/wire";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { consumerDelivery } from "../../test/subscribe-answer";
import { brokerCredential, forgetBrokerClients, forgetBrokerSession } from "../broker-credential";
import { type DispatchEnvironment, dispatchSettings } from "../dispatch-config";
import { DispatchClient } from "../dispatch-http";
import { readEnvoyConfig } from "../envoy-config";
import { envoySettings } from "../envoy-settings";
import { createEnvoyClient } from "../transport";

afterEach(forgetBrokerClients);

const servers: { stop: (closeActive?: boolean) => void }[] = [];
afterEach(() => {
  for (const server of servers.splice(0)) server.stop(true);
});

const tempDir = temporaryDirectories("broker-credential");

/** The config a caller with no envoy.json reads: the neutral file, which does not exist. */
function noFile(env: DispatchEnvironment) {
  return readEnvoyConfig(path.join(tempDir(), "envoy.json"), env, { cwd: tempDir() });
}

/** One trade the stub broker was asked. */
interface Trade {
  readonly form: URLSearchParams;
  readonly proof: Record<string, unknown>;
}

/** A broker that mints an access token good for `seconds`, numbering each one. */
function stubBroker(seconds: number): { url: string; trades: Trade[] } {
  const trades: Trade[] = [];
  const server = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch: async (request) => {
      const parts = (request.headers.get("DPoP") ?? "").split(".");
      trades.push({
        form: new URLSearchParams(await request.text()),
        proof: JSON.parse(Buffer.from(parts[1] ?? "", "base64url").toString("utf8")),
      });
      return Response.json({
        access_token: `broker-access-token-${trades.length}`,
        issued_token_type: "urn:ietf:params:oauth:token-type:access_token",
        token_type: "DPoP",
        expires_in: seconds,
        credential_id: "cred-1",
      });
    },
  });
  servers.push(server);
  return { url: `http://127.0.0.1:${server.port}`, trades };
}

/** One request the stub Dispatch was sent. */
interface Asked {
  readonly authorization: string;
  readonly proof: string;
}

/** A Dispatch that answers an empty issue list and keeps what it was sent. */
function stubDispatch(): { url: string; asked: Asked[] } {
  const asked: Asked[] = [];
  const server = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch: (request) => {
      asked.push({
        authorization: request.headers.get("Authorization") ?? "",
        proof: request.headers.get("DPoP") ?? "",
      });
      return Response.json([]);
    },
  });
  servers.push(server);
  return { url: `http://127.0.0.1:${server.port}`, asked };
}

/** A pod's projected token file. */
function projectedFile(token: string): string {
  const file = path.join(tempDir(), "token");
  writeFileSync(file, `${token}\n`);
  return file;
}

/** The environment of a pod that has an identity to trade and no Dispatch bearer at all. It
 * names no `AGENT_SECRETS_SESSION`: nothing in a plugin's environment sets one, and the session
 * a trade writes under is the one the caller passes. */
function podEnvironment(brokerUrl: string, dispatchUrl: string): DispatchEnvironment {
  return {
    HOME: tempDir(),
    DISPATCH_URL: dispatchUrl,
    ENVOY_URL: dispatchUrl,
    AGENT_SECRETS_URL: brokerUrl,
    AGENT_SECRETS_MACHINE_IDENTITY: "kubernetes",
    AGENT_SECRETS_MACHINE_TOKEN_FILE: projectedFile("projected.service.account.token"),
  };
}

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
  const config = personalFile(env, "http://dispatch.test", "a-person-s-dispatch-token");

  const shadowed = dispatchSettings(env, config, { session: "legion-demo-controller" });
  if (!shadowed.enabled) throw new Error(`Dispatch is off: ${shadowed.error}`);
  expect(shadowed.credential.secret()).toBe("a-person-s-dispatch-token");

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
    DISPATCH_TOKEN: "a-person-s-dispatch-token",
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
    DISPATCH_TOKEN: "a-person-s-dispatch-token",
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
    'DISPATCH_TOKEN_BROKER is "true" and takes 1'
  );
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
  expect(broker.trades[0]?.form.get("subject_token")).toBe("projected.service.account.token");
  expect(broker.trades[0]?.form.get("session")).toBe("example-worker-session");
  // The access token is presented under the DPoP scheme, with a proof naming this request and
  // hashing the token it is presented beside: a bearer copy of it buys nothing.
  const asked = dispatch.asked[0];
  expect(asked?.authorization).toBe("DPoP broker-access-token-1");
  const claims = JSON.parse(
    Buffer.from(asked?.proof.split(".")[1] ?? "", "base64url").toString("utf8")
  );
  expect(claims.htm).toBe("GET");
  expect(claims.htu).toBe(`${dispatch.url}/api/v1/issues`);
  const digest = await crypto.subtle.digest(
    "SHA-256",
    new TextEncoder().encode("broker-access-token-1")
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
    "DPoP broker-access-token-1",
    "DPoP broker-access-token-2",
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
    "DPoP broker-access-token-1",
    "DPoP broker-access-token-2",
    "DPoP broker-access-token-1",
  ]);
});

test("the listener takes the same credential, and a configured bearer still wins", () => {
  const broker = stubBroker(600);
  const env = podEnvironment(broker.url, "http://listener.test");
  const brokered = envoySettings(env, noFile(env), { nats: false, session: "session-one" });
  if (!brokered.enabled) throw new Error("Envoy is off");
  expect(brokered.credential?.scheme).toBe("DPoP");

  const written = envoySettings({ ...env, ENVOY_TOKEN: "listener-token" }, noFile(env), {
    nats: false,
  });
  if (!written.enabled) throw new Error("Envoy is off");
  expect(written.credential?.scheme).toBe("Bearer");
  expect(written.credential?.secret()).toBe("listener-token");
});

/** A broker whose tokens name the session the trade asked to write under, as the real one's do
 *  (the broker records the minted token against that session and the listener reads it back at
 *  the introspection route). */
function sessionBroker(): { url: string; sessions: string[] } {
  const sessions: string[] = [];
  const server = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch: async (request) => {
      const session = new URLSearchParams(await request.text()).get("session") ?? "";
      sessions.push(session);
      return Response.json({
        access_token: `token-for-${session}`,
        issued_token_type: "urn:ietf:params:oauth:token-type:access_token",
        token_type: "DPoP",
        expires_in: 600,
      });
    },
  });
  servers.push(server);
  return { url: `http://127.0.0.1:${server.port}`, sessions };
}

/** One call the stub listener was sent: the session its body acts as, and the session the token
 *  it presented names. */
interface Acted {
  readonly path: string;
  readonly body: string;
  readonly token: string;
}

/** A listener that binds the session a broker token names, as the real one does
 *  (`sessionNotBound`, packages/envoy/cmd/listener/brokerauth.go): a body naming another session
 *  is 403, and nothing else about the call is judged. */
function sessionBoundListener(): { url: string; acted: Acted[] } {
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
      if (token !== `token-for-${body}`) {
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
  const listener = sessionBoundListener();
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

  expect(listener.acted.map((one) => [one.path, one.body, one.token])).toEqual([
    ["/v1/roles/set", "session-one", "token-for-session-one"],
    ["/v1/interests/unsubscribe", "session-one", "token-for-session-one"],
    ["/v1/interests/subscribe", "session-two", "token-for-session-two"],
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
  const listener = sessionBoundListener();
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
  // (@legion/broker-client/pool-identity), so a pool machine with no bearer trades at the broker
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
  expect((await credential.headers("GET", url)).Authorization).toBe("DPoP broker-access-token-2");

  // A session a call names is told apart as `headers` tells it apart: only its client drops.
  await credential.headers("POST", url, "session-two");
  credential.stale?.("session-two");
  expect((await credential.headers("POST", url, "session-two")).Authorization).toBe(
    "DPoP broker-access-token-4"
  );
  expect((await credential.headers("GET", url)).Authorization).toBe("DPoP broker-access-token-2");
  expect(broker.trades.map((trade) => trade.form.get("session"))).toEqual([
    "session-one",
    "session-one",
    "session-two",
    "session-two",
  ]);
});

test("stale() for a session this process never traded for trades nothing, drops nothing, and that session's first call trades", async () => {
  // A server may say a token is not live for a session whose calls never reached this process's
  // broker credential. Telling it so asks nothing of the broker and takes no other session's
  // token, and the session it names trades afresh at its first call, once.
  const broker = stubBroker(600);
  const env = podEnvironment(broker.url, "http://dispatch.test");
  const credential = brokerCredential(env, "session-one");
  const url = "http://dispatch.test/api/v1/issues";

  credential.stale?.("session-never");
  await credential.headers("GET", url);
  credential.stale?.("session-never");
  expect(broker.trades).toHaveLength(1);
  expect((await credential.headers("GET", url)).Authorization).toBe("DPoP broker-access-token-1");

  forgetBrokerSession("session-never");
  expect((await credential.headers("POST", url, "session-never")).Authorization).toBe(
    "DPoP broker-access-token-2"
  );
  expect((await credential.headers("POST", url, "session-never")).Authorization).toBe(
    "DPoP broker-access-token-2"
  );
  expect(broker.trades.map((trade) => trade.form.get("session"))).toEqual([
    "session-one",
    "session-never",
  ]);
});

const PERSON = "ada@example.com";
const CLIENT_ID = "example-device-client";
/** The name this machine trades its own identity under, as a daemon's environment sets it. */
const MACHINE_SESSION = "legion-demo-controller";
/** What the broker answers a trade proved with the box key of a sign-in its person revoked. */
const REVOKED = `${PERSON} revoked this machine's sign-in, so the key this proof was signed with trades nothing. Sign in again with \`agent-secrets login\`.`;

/** One trade the stub broker was asked, and the thumbprint of the key its proof was signed with. */
interface ProvedTrade {
  readonly form: URLSearchParams;
  readonly thumbprint: string;
}

/**
 * A broker that mints a token good for ten minutes for every trade, numbering each, except a
 * trade proved with a key whose thumbprint `revoked` holds, which it refuses as it refuses a
 * sign-in its person revoked.
 */
function signInBroker(): { url: string; trades: ProvedTrade[]; revoked: Set<string> } {
  const trades: ProvedTrade[] = [];
  const revoked = new Set<string>();
  const server = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch: async (request) => {
      const header = (request.headers.get("DPoP") ?? "").split(".")[0] ?? "";
      const { jwk } = JSON.parse(Buffer.from(header, "base64url").toString("utf8"));
      const thumbprint = await thumbprintOf(jwk);
      trades.push({ form: new URLSearchParams(await request.text()), thumbprint });
      if (revoked.has(thumbprint)) {
        return Response.json(
          { error: "invalid_grant", error_description: REVOKED },
          { status: 400 }
        );
      }
      return Response.json({
        access_token: `broker-token-${trades.length}`,
        issued_token_type: "urn:ietf:params:oauth:token-type:access_token",
        token_type: "DPoP",
        expires_in: 600,
      });
    },
  });
  servers.push(server);
  return { url: `http://127.0.0.1:${server.port}`, trades, revoked };
}

/** The issuer {@link PERSON}'s sign-in is renewed at, stopped after the test; `answer` replaces
 *  a refresh's answer where it gives one. */
async function issuerFor(answer?: (count: number) => IssuerAnswer | undefined) {
  const issuer = await stubIssuer({
    clientId: CLIENT_ID,
    person: PERSON,
    ...(answer === undefined ? {} : { answer }),
  });
  servers.push(issuer);
  return issuer;
}

/**
 * The environment of a pod whose sign-in directory (`signins`) holds `records`, with the sign-in
 * client at `issuerUrl` configured and `AGENT_SECRETS_SESSION` naming the machine's own session,
 * as a daemon's environment sets it. The pod's projected token is the actor: which kind of
 * machine a person signs in on changes nothing this file reads.
 */
async function signedInPod(
  brokerUrl: string,
  dispatchUrl: string,
  issuerUrl: string,
  records: readonly SignInRecord[]
): Promise<{ env: DispatchEnvironment; signins: string }> {
  const signins = path.join(tempDir(), "signins");
  for (const record of records) await writeSignIn(signins, record);
  const secret = path.join(tempDir(), "client-secret");
  writeFileSync(secret, "example-device-client-secret\n", { mode: 0o600 });
  return {
    env: {
      ...podEnvironment(brokerUrl, dispatchUrl),
      AGENT_SECRETS_SESSION: MACHINE_SESSION,
      AGENT_SECRETS_SIGNIN_DIR: signins,
      AGENT_SECRETS_SIGNIN_ISSUER: issuerUrl,
      AGENT_SECRETS_SIGNIN_CLIENT_ID: CLIENT_ID,
      AGENT_SECRETS_SIGNIN_CLIENT_SECRET_FILE: secret,
    },
    signins,
  };
}

/** A Dispatch client of `env` for a caller acting as `session`, or as none. */
function dispatchOf(env: DispatchEnvironment, session?: string): DispatchClient {
  const resolution = dispatchSettings(env, noFile(env), session === undefined ? {} : { session });
  if (!resolution.enabled) throw new Error(`Dispatch is off: ${resolution.error}`);
  return new DispatchClient(resolution.url, resolution.credential);
}

/** The words `call` failed with; a call that succeeds fails the test. */
async function refusalOf(call: Promise<unknown>): Promise<string> {
  return await call.then(
    () => {
      throw new Error("expected the call to be refused");
    },
    (error: unknown) => (error instanceof Error ? error.message : String(error))
  );
}

test("a person's trade asks under the caller's own session, else the sign-in's name, never AGENT_SECRETS_SESSION", async () => {
  // The machine trades under AGENT_SECRETS_SESSION, and the broker holds a name for one identity,
  // so the person asking under it would be refused.
  const broker = signInBroker();
  const dispatch = stubDispatch();
  const issuer = await issuerFor();
  const record = await signedIn(PERSON, { client_id: CLIENT_ID });
  const { env } = await signedInPod(broker.url, dispatch.url, issuer.url, [record]);

  await dispatchOf(env).listAllIssues({});
  await dispatchOf(env, "ses_01a9").listAllIssues({});

  expect(
    broker.trades.map((trade) => [
      trade.form.get("subject_token_type"),
      trade.form.get("session"),
      trade.thumbprint,
    ])
  ).toEqual([
    [SUBJECT_TOKEN_TYPE_PERSON, signInSessionName(record), record.thumbprint],
    [SUBJECT_TOKEN_TYPE_PERSON, "ses_01a9", record.thumbprint],
  ]);
  expect(dispatch.asked.map((one) => one.authorization)).toEqual([
    "DPoP broker-token-1",
    "DPoP broker-token-2",
  ]);
});

test("a caller acting as no session and one acting as AGENT_SECRETS_SESSION's session trade apart, whichever calls first", async () => {
  // With a sign-in, a caller acting as no session trades under the sign-in's own name and one
  // acting as the session AGENT_SECRETS_SESSION names trades under that session. One token for
  // both would name one of them wrongly, whichever called first, and the listener refuses every
  // write of a session whose token names another (`sessionNotBound`).
  const broker = signInBroker();
  const dispatch = stubDispatch();
  const issuer = await issuerFor();
  const record = await signedIn(PERSON, { client_id: CLIENT_ID });
  const { env } = await signedInPod(broker.url, dispatch.url, issuer.url, [record]);
  const asNone = () => dispatchOf(env).listAllIssues({});
  const asMachineSession = () => dispatchOf(env, MACHINE_SESSION).listAllIssues({});

  await asNone();
  await asMachineSession();
  await asNone();
  await asMachineSession();
  // A process that has traded nothing yet, whose first caller is the other one.
  forgetBrokerClients();
  await asMachineSession();
  await asNone();
  await asMachineSession();
  await asNone();

  expect(broker.trades.map((trade) => trade.form.get("session"))).toEqual([
    signInSessionName(record),
    MACHINE_SESSION,
    MACHINE_SESSION,
    signInSessionName(record),
  ]);
  // Every call presents the token its own caller's trade was answered.
  expect(dispatch.asked.map((one) => one.authorization)).toEqual([
    "DPoP broker-token-1",
    "DPoP broker-token-2",
    "DPoP broker-token-1",
    "DPoP broker-token-2",
    "DPoP broker-token-3",
    "DPoP broker-token-4",
    "DPoP broker-token-3",
    "DPoP broker-token-4",
  ]);
});

test("with nobody signed in, a caller acting as no session and one acting as AGENT_SECRETS_SESSION's session both trade the machine under it", async () => {
  const broker = stubBroker(600);
  const dispatch = stubDispatch();
  const env = {
    ...podEnvironment(broker.url, dispatch.url),
    AGENT_SECRETS_SESSION: MACHINE_SESSION,
  };

  await dispatchOf(env).listAllIssues({});
  await dispatchOf(env, MACHINE_SESSION).listAllIssues({});
  forgetBrokerClients();
  await dispatchOf(env, MACHINE_SESSION).listAllIssues({});
  await dispatchOf(env).listAllIssues({});

  // Two clients in each process, one per caller, as `CLIENTS` keeps them: the cost of keeping a
  // caller acting as no session apart from one acting as the session the variable names.
  const machine = ["projected.service.account.token", MACHINE_SESSION];
  expect(
    broker.trades.map((trade) => [trade.form.get("subject_token"), trade.form.get("session")])
  ).toEqual([machine, machine, machine, machine]);
  expect(dispatch.asked.map((one) => one.authorization)).toEqual([
    "DPoP broker-access-token-1",
    "DPoP broker-access-token-2",
    "DPoP broker-access-token-3",
    "DPoP broker-access-token-4",
  ]);
});

test("a client the factory failed to make is not kept: a store the person corrected is read with no restart", async () => {
  const broker = signInBroker();
  const dispatch = stubDispatch();
  const issuer = await issuerFor();
  const ada = await signedIn(PERSON, { client_id: CLIENT_ID });
  const grace = await signedIn("grace@example.com", { client_id: CLIENT_ID });
  const { env, signins } = await signedInPod(broker.url, dispatch.url, issuer.url, [ada, grace]);
  const client = dispatchOf(env, "ses_01a9");

  expect(await refusalOf(client.listAllIssues({}))).toBe(
    "2 people are signed in on this machine (ada@example.com, grace@example.com), so this session must say which one it acts for: set AGENT_SECRETS_PERSON to one of them"
  );
  expect(broker.trades).toHaveLength(0);

  await rm(signInPath(signins, grace.person));
  await client.listAllIssues({});
  expect(broker.trades.map((trade) => trade.thumbprint)).toEqual([ada.thumbprint]);
  expect(dispatch.asked.map((one) => one.authorization)).toEqual(["DPoP broker-token-1"]);
});

test("a sign-in the broker refused for good fails every later call in its words, asking nothing and never trading the machine", async () => {
  const broker = signInBroker();
  const dispatch = stubDispatch();
  const issuer = await issuerFor();
  const record = await signedIn(PERSON, { client_id: CLIENT_ID });
  const { env } = await signedInPod(broker.url, dispatch.url, issuer.url, [record]);
  broker.revoked.add(record.thumbprint);

  const refused = await refusalOf(dispatchOf(env, "ses_01a9").listAllIssues({}));
  expect(refused).toBe(
    `the secrets broker refused ${PERSON}'s sign-in on this machine: HTTP 400 invalid_grant: ${REVOKED}`
  );
  const asked = issuer.requests.length;

  // The same session's later calls, Dispatch's and the listener's alike, are refused in the same
  // words with no refresh at the issuer, no trade at the broker and no request to Dispatch.
  expect(await refusalOf(dispatchOf(env, "ses_01a9").listAllIssues({}))).toBe(refused);
  const listener = envoySettings(env, noFile(env), { nats: false, session: "ses_01a9" });
  if (!listener.enabled) throw new Error("Envoy is off");
  expect(
    await refusalOf(
      Promise.resolve(listener.credential?.headers("POST", `${dispatch.url}/v1/roles/set`))
    )
  ).toBe(refused);
  expect(issuer.requests).toHaveLength(asked);
  expect(dispatch.asked).toHaveLength(0);
  // The one trade made is the person's: the machine is never traded in the person's place.
  expect(broker.trades.map((trade) => trade.form.get("subject_token_type"))).toEqual([
    SUBJECT_TOKEN_TYPE_PERSON,
  ]);
});

test("after a refusal, a new sign-in with a new box key gets a client of its own, which trades with that key", async () => {
  const broker = signInBroker();
  const dispatch = stubDispatch();
  const issuer = await issuerFor();
  const record = await signedIn(PERSON, { client_id: CLIENT_ID });
  const { env, signins } = await signedInPod(broker.url, dispatch.url, issuer.url, [record]);
  broker.revoked.add(record.thumbprint);
  const client = dispatchOf(env, "ses_01a9");
  expect(await refusalOf(client.listAllIssues({}))).toContain("agent-secrets login");

  // `agent-secrets login` again: a new box key, written over the person's record.
  const renewed = await signedIn(PERSON, { client_id: CLIENT_ID });
  await writeSignIn(signins, renewed);
  await client.listAllIssues({});

  expect(broker.trades.map((trade) => trade.thumbprint)).toEqual([
    record.thumbprint,
    renewed.thumbprint,
  ]);
  expect(dispatch.asked.map((one) => one.authorization)).toEqual(["DPoP broker-token-2"]);
});

test("a sign-in the issuer has ended is remembered too: no trade at the broker, and no second refresh", async () => {
  const broker = signInBroker();
  const dispatch = stubDispatch();
  const issuer = await issuerFor(() => ({
    status: 400,
    body: { error: "invalid_grant", error_description: "Token has been expired or revoked." },
  }));
  const record = await signedIn(PERSON, { client_id: CLIENT_ID });
  const { env } = await signedInPod(broker.url, dispatch.url, issuer.url, [record]);

  const ended = await refusalOf(dispatchOf(env, "ses_01a9").listAllIssues({}));
  expect(ended).toBe(
    `${issuer.url} no longer honours ${PERSON}'s sign-in on this machine; run: agent-secrets login`
  );
  const refreshes = issuer.requests.filter((one) => one.path === "/token").length;
  expect(refreshes).toBe(1);

  expect(await refusalOf(dispatchOf(env, "ses_01a9").listAllIssues({}))).toBe(ended);
  expect(issuer.requests.filter((one) => one.path === "/token")).toHaveLength(refreshes);
  expect(broker.trades).toHaveLength(0);
  expect(dispatch.asked).toHaveLength(0);
});
