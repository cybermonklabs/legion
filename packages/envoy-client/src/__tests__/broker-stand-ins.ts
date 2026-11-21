// packages/envoy-client/src/__tests__/broker-stand-ins.ts
//
// The stand-ins the broker credential's tests trade against, with the cleanup they need: a
// secrets broker, a Dispatch, and the environment of a pod that has an identity to trade.
// `brokerStandIns` is for the files that read the credential Dispatch and the listener are given
// (broker-credential*.test.ts), and `natsBrokerStandIns` for the files that read a session's NATS
// credential (nats-credential*.test.ts).
//
// This module's body registers no hook: Bun evaluates an imported module once per process, so a
// hook here would land in the scope of the first test file that imported it, and every later file
// would leave its clients, servers and directories behind. Each factory registers the cleanup of
// the file that calls it, so a test file calls one once, at its module scope.

import { afterEach } from "bun:test";
import { writeFileSync } from "node:fs";
import * as path from "node:path";
import {
  type DeviceLoginBrokerOptions,
  stubDeviceLoginBroker,
  writeDeviceLoginFile,
} from "@legion/cipher-client/test/device-logins";
import { thumbprintOf } from "@legion/cipher-client/test/sign-ins";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { forgetBrokerClients } from "../broker-credential";
import type { DispatchEnvironment } from "../dispatch-config";
import { readEnvoyConfig } from "../envoy-config";
import type { EnvoyEnvironment } from "../envoy-settings";

/** A server a test starts, which the calling file's `afterEach` stops. */
interface Stoppable {
  stop: (closeActive?: boolean) => void;
}

/**
 * Registers, where it is called, the `afterEach` that forgets every broker client this process
 * made, the one that stops every server a test pushed onto `servers`, and the removal of every
 * directory `tempDir` makes, in that order; and returns `servers` and `tempDir`.
 */
function standInScope(prefix: string) {
  afterEach(forgetBrokerClients);
  const servers: Stoppable[] = [];
  afterEach(() => {
    for (const server of servers.splice(0)) server.stop(true);
  });
  const tempDir = temporaryDirectories(prefix);
  return { servers, tempDir };
}

/** One trade the stub broker was asked, and the access token it answered. */
export interface Trade {
  readonly form: URLSearchParams;
  readonly proof: Record<string, unknown>;
  readonly token: string;
}

/** One request the stub Dispatch was sent. */
export interface Asked {
  readonly authorization: string;
  readonly proof: string;
}

/**
 * The stand-ins of a file that reads the credential Dispatch and the listener are given. Call it
 * once, at the test file's module scope, with the prefix of the file's temporary directories: it
 * registers that file's cleanup ({@link standInScope}). A test pushes any other server it starts
 * onto `servers`.
 */
export function brokerStandIns(prefix: string) {
  const { servers, tempDir } = standInScope(prefix);

  /** The config a caller with no envoy.json reads: the neutral file, which does not exist. */
  function noFile(env: DispatchEnvironment) {
    return readEnvoyConfig(path.join(tempDir(), "envoy.json"), env, { cwd: tempDir() });
  }

  /** A broker that mints a random access token good for `seconds`, kept with its trade. */
  function stubBroker(seconds: number): { url: string; trades: Trade[] } {
    const trades: Trade[] = [];
    const server = Bun.serve({
      port: 0,
      hostname: "127.0.0.1",
      fetch: async (request) => {
        const parts = (request.headers.get("DPoP") ?? "").split(".");
        const token = fixtureSecret("broker");
        trades.push({
          form: new URLSearchParams(await request.text()),
          proof: JSON.parse(Buffer.from(parts[1] ?? "", "base64url").toString("utf8")),
          token,
        });
        return Response.json({
          access_token: token,
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

  /** The pod's projected token, random as a real one is: the client holds what it trades, and a
   *  token spelled from words would take those words out of its lines. */
  const projectedToken = fixtureSecret("proj");

  /** A pod's projected token file. */
  function projectedFile(): string {
    const file = path.join(tempDir(), "token");
    writeFileSync(file, `${projectedToken}\n`);
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
      AGENT_SECRETS_MACHINE_TOKEN_FILE: projectedFile(),
    };
  }

  return { servers, tempDir, noFile, stubBroker, stubDispatch, podEnvironment, projectedToken };
}

/** One token the stub broker minted: what it handed out, the session and key it was traded
 *  for, and when it lapses. */
export interface Minted {
  readonly value: string;
  readonly session: string;
  readonly jkt: string;
  readonly expiresAt: number;
}

/**
 * The stand-ins of a file that reads a session's NATS credential. Call it once, at the test
 * file's module scope, with the prefix of the file's temporary directories: it registers that
 * file's cleanup ({@link standInScope}). A test pushes any other server it starts onto `servers`.
 */
export function natsBrokerStandIns(prefix: string) {
  const { servers, tempDir } = standInScope(prefix);

  /**
   * A secrets broker: its token exchange mints a random token good for `seconds`, kept in
   * `minted` with the session and key it was traded for, and its introspection route answers each
   * token it minted live until it lapses, as a session's (`ProjectScopePerson`). `trades` counts
   * every trade it was sent, answered or not. While `down` is set the exchange answers 503, as a
   * broker that is away; while `unregistered` is set it refuses each trade as the broker refuses a
   * pod session no registration names yet (`registeredPodSession`,
   * packages/envoy/internal/cipher/oauth/exchange.go), in its own words. `state.seconds`, at first
   * `lifeSeconds`, is how long the next token lives: a row may change it, as a broker that is back mints tokens of their
   * usual life. A token in `revoked` introspects as not active, as one a revoke ended before it
   * lapsed. A trade whose DPoP proof is missing or whose header is no JSON is answered as the
   * broker answers it, `invalid_dpop_proof` (RFC 9449 Section 7.1), and counted in `badProofs`,
   * rather than thrown: a throw in the handler fails the row running and skips its cleanup.
   */
  function stubBroker(lifeSeconds: number) {
    const minted: Minted[] = [];
    const state = {
      down: false,
      unregistered: false,
      minted,
      revoked: new Set<string>(),
      trades: 0,
      seconds: lifeSeconds,
      badProofs: 0,
    };
    const server = Bun.serve({
      port: 0,
      hostname: "127.0.0.1",
      fetch: async (request) => {
        const form = new URLSearchParams(await request.text());
        if (new URL(request.url).pathname === "/v1/oauth/introspect") {
          const held = minted.find((one) => one.value === form.get("token"));
          if (held === undefined || held.expiresAt <= Date.now() || state.revoked.has(held.value))
            return Response.json({ active: false });
          return Response.json({
            active: true,
            session: held.session,
            person: "ada@example.com",
            client_id: "dispatch-token-exchange",
            project_scope: "person",
            jkt: held.jkt,
            exp: Math.floor(held.expiresAt / 1000),
          });
        }
        state.trades += 1;
        if (state.down) return new Response("the broker is away", { status: 503 });
        if (state.unregistered) {
          const session = form.get("session") ?? "";
          return Response.json(
            {
              error: "invalid_grant",
              error_description: `pod_session_not_registered: No registration says this pod may write under the session name '${session}'. A pod writes under a session name its deployment registered for it.`,
            },
            { status: 400 }
          );
        }
        let header: { readonly jwk: JsonWebKey };
        try {
          header = JSON.parse(
            Buffer.from(
              (request.headers.get("DPoP") ?? "").split(".")[0] ?? "",
              "base64url"
            ).toString()
          ) as { readonly jwk: JsonWebKey };
        } catch {
          state.badProofs += 1;
          return Response.json(
            { error: "invalid_dpop_proof", error_description: "the request carries no DPoP proof" },
            { status: 400 }
          );
        }
        // Random, as the broker's are: the client holds each one, and two trades in flight at once
        // must not be answered the same token.
        const value = fixtureSecret("access");
        minted.push({
          value,
          session: form.get("session") ?? "",
          jkt: await thumbprintOf(header.jwk),
          expiresAt: Date.now() + state.seconds * 1000,
        });
        return Response.json({
          access_token: value,
          issued_token_type: "urn:ietf:params:oauth:token-type:access_token",
          token_type: "DPoP",
          expires_in: state.seconds,
        });
      },
    });
    servers.push(server);
    return { url: `http://127.0.0.1:${server.port}`, state };
  }

  /** A pod's projected token file. */
  function projectedFile(): string {
    const file = path.join(tempDir(), "token");
    writeFileSync(file, `${fixtureSecret("proj")}\n`);
    return file;
  }

  /** The environment of a pod that has an identity to trade at `brokerUrl` and holds no NATS
   *  seed: a broker-capable environment, whatever its hub runs. */
  function podEnvironment(brokerUrl: string): EnvoyEnvironment {
    return {
      HOME: tempDir(),
      ENVOY_NATS_URL: "nats://nats.internal.example:4222",
      AGENT_SECRETS_URL: brokerUrl,
      AGENT_SECRETS_MACHINE_IDENTITY: "kubernetes",
      AGENT_SECRETS_MACHINE_TOKEN_FILE: projectedFile(),
    };
  }

  return { servers, tempDir, stubBroker, podEnvironment };
}

/**
 * The stand-ins of a file that drives a device login (`AGENT_SECRETS_MACHINE_IDENTITY=device-login`,
 * `dispatch login --device`). Call it once, at the test file's module scope, with the prefix of the
 * file's temporary directories: it registers that file's cleanup ({@link standInScope}).
 */
export function deviceLoginStandIns(prefix: string) {
  const { servers, tempDir } = standInScope(prefix);

  /** A secrets broker serving the device grant, the refresh and the exchange
   *  (`stubDeviceLoginBroker`), stopped after the test. */
  function broker(options: DeviceLoginBrokerOptions = {}) {
    const stub = stubDeviceLoginBroker(options);
    servers.push(stub);
    return stub;
  }

  /** A device login's file, in a directory of this test's own, made at `brokerUrl`. */
  function loginFile(brokerUrl: string, fields: Record<string, unknown> = {}) {
    return writeDeviceLoginFile(
      path.join(tempDir(), "legion", "device-login.json"),
      brokerUrl,
      fields
    );
  }

  /** The environment of a laptop whose identity is the device login at `file`, at `brokerUrl`,
   *  with no Dispatch bearer at all. */
  function laptopEnvironment(
    brokerUrl: string,
    dispatchUrl: string,
    file: string
  ): DispatchEnvironment {
    return {
      HOME: tempDir(),
      DISPATCH_URL: dispatchUrl,
      ENVOY_URL: dispatchUrl,
      AGENT_SECRETS_URL: brokerUrl,
      AGENT_SECRETS_MACHINE_IDENTITY: "device-login",
      AGENT_SECRETS_DEVICE_LOGIN_FILE: file,
    };
  }

  return { servers, tempDir, broker, loginFile, laptopEnvironment };
}
