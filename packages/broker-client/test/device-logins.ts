// packages/broker-client/test/device-logins.ts
//
// A machine's device login as `dispatch login --device` leaves it (`src/device-login.ts`), and a
// secrets broker serving the three routes a device login meets: the device authorization, the
// token endpoint's device code, refresh and exchange grants, as `internal/broker/oauth` serves
// them. It is for every suite that drives a device login: this package's own, and envoy-client's
// broker credential and `dispatch login --device`. Every key is made at run time, so the
// repository holds no private key.

import { generateKeyPairSync, type KeyObject } from "node:crypto";
import { chmod, mkdir, writeFile } from "node:fs/promises";
import { dirname } from "node:path";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { calculateJwkThumbprint, type JWK } from "jose";

/** One request the stand-in was sent: its form, and the DPoP proof's header and claims where it
 *  carried one, with the thumbprint of the key that header embeds. */
export interface DeviceLoginRequest {
  readonly form: URLSearchParams;
  readonly proofHeader: Record<string, unknown> | null;
  readonly proofClaims: Record<string, unknown> | null;
  /** The RFC 7638 thumbprint of the proof's key, "" where the request carried no proof. */
  readonly proofThumbprint: string;
}

/** How the stand-in answers one poll of a device code. */
export type PollAnswer = "pending" | "slow_down" | "approve" | "deny" | "expired";

/** What the stand-in is told to do. */
export interface DeviceLoginBrokerOptions {
  /** The answer to each poll in turn; the last repeats. Default: approve at the first poll. */
  readonly polls?: readonly PollAnswer[];
  /**
   * Hold each refresh's answer until another refresh arrives, or this many milliseconds pass, so
   * two processes that refresh at once both reach the stand-in before either is answered. Where a
   * lock keeps the second process from refreshing, the second refresh never comes, and only the
   * time running out can show that: so this is a real wait, which a row that passes pays once.
   */
  readonly refreshOverlapMs?: number;
  /** How long an access token the stand-in mints lasts. Default 900 s, the broker's own. */
  readonly accessSeconds?: number;
  /** Answer every refresh `400 invalid_grant`, as the broker answers one whose login ended. */
  readonly refuseRefresh?: boolean;
}

/** The credential every device login of this stand-in holds. */
export const STAND_IN_CREDENTIAL = "cred-device-1";

/** The user code every device authorization of this stand-in answers. */
export const STAND_IN_USER_CODE = "WDJB-MJHT";

/** The stand-in: its URL, what it was sent, and the refresh and access tokens it minted. */
export interface DeviceLoginBroker {
  readonly url: string;
  readonly authorizations: DeviceLoginRequest[];
  readonly polls: DeviceLoginRequest[];
  readonly refreshes: DeviceLoginRequest[];
  readonly trades: DeviceLoginRequest[];
  /** Each access token the device code and refresh grants minted, in order. */
  readonly loginTokens: string[];
  /** Each access token the exchange minted, in order. */
  readonly tradeTokens: string[];
  stop(): void;
}

/** One part of a compact JWS as JSON, or null where there is no such part. */
function part(compact: string, at: number): Record<string, unknown> | null {
  const encoded = compact.split(".")[at];
  if (encoded === undefined || encoded === "") return null;
  try {
    return JSON.parse(Buffer.from(encoded, "base64url").toString("utf8")) as Record<
      string,
      unknown
    >;
  } catch {
    return null;
  }
}

/** The request as the stand-in records it. */
async function recorded(request: Request): Promise<DeviceLoginRequest> {
  const proof = request.headers.get("DPoP") ?? "";
  const proofHeader = proof === "" ? null : part(proof, 0);
  const jwk = proofHeader?.jwk as JWK | undefined;
  return {
    form: new URLSearchParams(await request.text()),
    proofHeader,
    proofClaims: proof === "" ? null : part(proof, 1),
    proofThumbprint: jwk === undefined ? "" : await calculateJwkThumbprint(jwk),
  };
}

/** An OAuth error answer, as the broker writes one. */
function oauthError(error: string, status = 400): Response {
  return Response.json({ error, error_description: `the stand-in answered ${error}` }, { status });
}

/**
 * A secrets broker serving a device login: `POST /v1/oauth/device_authorization` answers a device
 * code, {@link STAND_IN_USER_CODE} and a verification URI on its own origin, with no
 * `verification_uri_complete`, as the broker omits it; `POST /v1/oauth/token` answers a device
 * code as `polls` says, a refresh by minting a new refresh token and a new access token (or
 * `invalid_grant` under `refuseRefresh`), and an RFC 8693 exchange with a DPoP-bound access
 * token. The device code grant and a refresh carry the credential's id and expiry, as the
 * broker's token hooks add them (`machine.Service.TokenHooks`).
 */
export function stubDeviceLoginBroker(options: DeviceLoginBrokerOptions = {}): DeviceLoginBroker {
  const polls = options.polls ?? ["approve"];
  const accessSeconds = options.accessSeconds ?? 900;
  const authorizations: DeviceLoginRequest[] = [];
  const pollsSeen: DeviceLoginRequest[] = [];
  const refreshes: DeviceLoginRequest[] = [];
  const trades: DeviceLoginRequest[] = [];
  const loginTokens: string[] = [];
  const tradeTokens: string[] = [];
  const deviceCode = fixtureSecret("device");
  // The refreshes holding their answers for the next one to arrive (`refreshOverlapMs`).
  const holding: (() => void)[] = [];
  const loginAnswer = (): Response => {
    const access = fixtureSecret("login");
    loginTokens.push(access);
    return Response.json({
      access_token: access,
      token_type: "DPoP",
      expires_in: accessSeconds,
      refresh_token: fixtureSecret("refresh"),
      scope: "machine",
      credential_id: STAND_IN_CREDENTIAL,
      credential_expires_at: new Date(Date.now() + 30 * 86_400_000)
        .toISOString()
        .replace(/\.\d{3}Z$/, "Z"),
    });
  };
  const server = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch: async (request) => {
      const { origin, pathname } = new URL(request.url);
      const seen = await recorded(request);
      if (request.method === "POST" && pathname === "/v1/oauth/device_authorization") {
        authorizations.push(seen);
        return Response.json({
          device_code: deviceCode,
          user_code: STAND_IN_USER_CODE,
          verification_uri: `${origin}/credentials/machine`,
          expires_in: 600,
          interval: 1,
        });
      }
      if (request.method !== "POST" || pathname !== "/v1/oauth/token") {
        return new Response("not found", { status: 404 });
      }
      switch (seen.form.get("grant_type")) {
        case "urn:ietf:params:oauth:grant-type:device_code": {
          pollsSeen.push(seen);
          if (seen.form.get("device_code") !== deviceCode) return oauthError("invalid_grant");
          const answer = polls[Math.min(pollsSeen.length, polls.length) - 1];
          switch (answer) {
            case "pending":
              return oauthError("authorization_pending");
            case "slow_down":
              return oauthError("slow_down");
            case "deny":
              return oauthError("access_denied");
            case "expired":
              return oauthError("expired_token");
            default:
              return loginAnswer();
          }
        }
        case "refresh_token": {
          refreshes.push(seen);
          for (const release of holding.splice(0)) release();
          if (options.refreshOverlapMs !== undefined) {
            const { promise, resolve } = Promise.withResolvers<void>();
            holding.push(resolve);
            const timer = setTimeout(resolve, options.refreshOverlapMs);
            await promise;
            clearTimeout(timer);
          }
          if (options.refuseRefresh === true) return oauthError("invalid_grant");
          return loginAnswer();
        }
        case "urn:ietf:params:oauth:grant-type:token-exchange": {
          trades.push(seen);
          const token = fixtureSecret("traded");
          tradeTokens.push(token);
          return Response.json({
            access_token: token,
            issued_token_type: "urn:ietf:params:oauth:token-type:access_token",
            token_type: "DPoP",
            expires_in: accessSeconds,
          });
        }
        default:
          return oauthError("unsupported_grant_type");
      }
    },
  });
  return {
    url: `http://127.0.0.1:${server.port}`,
    authorizations,
    polls: pollsSeen,
    refreshes,
    trades,
    loginTokens,
    tradeTokens,
    stop: () => server.stop(true),
  };
}

/** A device login's file as `dispatch login --device` writes it, and the key it holds. */
export interface WrittenDeviceLogin {
  readonly path: string;
  readonly key: KeyObject;
  readonly thumbprint: string;
  readonly refreshToken: string;
}

/**
 * Writes a device login for `broker` at `path` as `dispatch login --device` leaves one: a fresh
 * P-256 key as base64 PKCS#8 DER, the stand-in's credential, a random refresh token, an expiry
 * 30 days on, and the broker, in a 0600 file whose directory is 0700. `fields` replaces or adds
 * any member of the JSON (an `access_token` the login holds, a `broker` of another).
 */
export async function writeDeviceLoginFile(
  path: string,
  broker: string,
  fields: Record<string, unknown> = {},
  mode = 0o600
): Promise<WrittenDeviceLogin> {
  const { privateKey, publicKey } = generateKeyPairSync("ec", { namedCurve: "P-256" });
  const refreshToken = fixtureSecret("refresh");
  await mkdir(dirname(path), { recursive: true, mode: 0o700 });
  const json = {
    key: privateKey.export({ format: "der", type: "pkcs8" }).toString("base64"),
    credential_id: STAND_IN_CREDENTIAL,
    refresh_token: refreshToken,
    expires_at: new Date(Date.now() + 30 * 86_400_000).toISOString(),
    broker,
    ...fields,
  };
  await writeFile(path, JSON.stringify(json), { mode });
  await chmod(path, mode);
  return {
    path,
    key: privateKey,
    thumbprint: await calculateJwkThumbprint(publicKey.export({ format: "jwk" }) as JWK),
    refreshToken,
  };
}
