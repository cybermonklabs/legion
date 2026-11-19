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
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { consumerDelivery } from "../../test/subscribe-answer";
import { brokerCredential, forgetBrokerSession } from "../broker-credential";
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
