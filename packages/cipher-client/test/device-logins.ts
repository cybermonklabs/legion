// packages/cipher-client/test/device-logins.ts
//
// A machine's device login as `dispatch login --device` leaves it (`src/device-login.ts`), and a
// secrets broker serving the three routes a device login meets: the device authorization, the
// token endpoint's device code, refresh and exchange grants, as `internal/cipher/oauth` serves
// them. It is for every suite that drives a device login: this package's own, and envoy-client's
// broker credential and `dispatch login --device`. Every key is made at run time, so the
// repository holds no private key.

import { generateKeyPairSync, type KeyObject } from "node:crypto";
import { chmod, mkdir, writeFile } from "node:fs/promises";
import { dirname } from "node:path";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { calculateJwkThumbprint, type JWK } from "jose";
import { FORM_SCOPE, SCOPE_READ } from "../src/wire";

/** One request the stand-in was sent: its form, and the DPoP proof's header and claims where it
 *  carried one, with the thumbprint of the key that header embeds. */
export interface DeviceLoginRequest {
  readonly form: URLSearchParams;
  readonly proofHeader: Record<string, unknown> | null;
  readonly proofClaims: Record<string, unknown> | null;
  /** The RFC 7638 thumbprint of the proof's key, "" where the request carried no proof. */
  readonly proofThumbprint: string;
}

/** How the stand-in answers one poll of a device code: `invalid_client` is an OAuth refusal the
 *  poll does not expect, which the broker answers a client it does not know, and `unknown-code`
 *  one under a code no client knows; `invalid_grant` is the broker's answer to a code it no
 *  longer knows; `html-403` and `challenge-401` are 4xx answers with no OAuth error in them, as a
 *  proxy or a load balancer in front of the broker gives (an HTML page, a bare
 *  `WWW-Authenticate: DPoP` challenge); `busy-429` and `unavailable-503` are answers a later poll
 *  can get past. */
export type PollAnswer =
  | "pending"
  | "slow_down"
  | "approve"
  | "deny"
  | "expired"
  | "invalid_grant"
  | "invalid_client"
  | "unknown-code"
  | "html-403"
  | "challenge-401"
  | "busy-429"
  | "unavailable-503";

/** The OAuth error code the stand-in answers an `unknown-code` poll with: one no client knows,
 *  which a text must not show. */
export const STAND_IN_UNKNOWN_CODE = "stand_in_code_of_its_own";

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
  /**
   * Hold each exchange's answer until a refresh arrives, or this many milliseconds pass, so a
   * refresh made while a trade is still in flight lands before that trade is answered. A trade
   * whose subject token that refresh revoked is then refused, as the broker refuses it. Where the
   * lock covers the trade, no refresh comes, and only the time running out shows that: a real
   * wait, which a row that passes pays once per trade.
   */
  readonly tradeHoldMs?: number;
  /**
   * Mint each refresh's answer when it arrives, as the broker does, and hold it until the
   * stand-in has been sent this many refreshes in all, with no time limit. A control row sets it
   * in place of {@link refreshOverlapMs}: there the second refresh must come, so no clock decides
   * whether the first is answered before it. A row whose client gives up on the first sets it to
   * 2 to stand for an answer that never reached the client.
   */
  readonly refreshesBeforeAnswer?: number;
  /**
   * Hold each exchange's answer until the stand-in has been sent this many refreshes in all,
   * with no time limit. A control row sets it in place of {@link tradeHoldMs}: there the second
   * process's refresh must come while the first one's trade is out, so no clock decides it.
   */
  readonly refreshesBeforeTrade?: number;
  /** How long an access token the stand-in mints lasts, the broker's `BROKER_ACCESS_TOKEN_SECONDS`:
   *  {@link STAND_IN_ACCESS_SECONDS} unless a row names another. */
  readonly accessSeconds?: number;
  /** Answer every refresh `400 invalid_grant`, as the broker answers one whose login ended, or
   *  `401 invalid_client`, a refusal of the refresh that is no end of the login. */
  readonly refuseRefresh?: true | "invalid_client";
  /** Refuse the device authorization `400` with this OAuth error code. */
  readonly refuseAuthorization?: string;
  /** The device authorization's `interval`, in seconds; 1 by default, and none where null. */
  readonly interval?: number | null;
  /** The device authorization's `expires_in`, in seconds; 600 by default. */
  readonly expiresIn?: number;
  /** The device authorization's user code and pages, in place of the stand-in's own; it sends a
   *  `verification_uri_complete` only where one is given. */
  readonly userCode?: string;
  readonly verificationUri?: string;
  readonly verificationUriComplete?: string;
  /** The credential id a grant answers, in place of {@link STAND_IN_CREDENTIAL}. */
  readonly credentialId?: string;
}

/** The credential every device login of this stand-in holds. */
export const STAND_IN_CREDENTIAL = "cred-device-1";

/** How long an access token the stand-in mints lasts unless told otherwise: the broker's default
 *  `BROKER_ACCESS_TOKEN_SECONDS`. */
export const STAND_IN_ACCESS_SECONDS = 900;

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
  /** Each exchange refused because a refresh had revoked its subject token. */
  readonly revokedTrades: DeviceLoginRequest[];
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
  const jwk = proofHeader?.["jwk"] as JWK | undefined;
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
 * broker's token hooks add them (`machine.Service.TokenHooks`). A refresh revokes every access
 * token of the login but the one it answers, as the broker's `RotateRefreshToken` does, so an
 * exchange presenting any other after a refresh is refused `400 invalid_request`, with the
 * broker's own description of a token it no longer honours. Before the first refresh, any subject
 * token is taken, so a test may keep an access token of its own in the file.
 *
 * Every token lives {@link DeviceLoginBrokerOptions.accessSeconds} on the clock of the process,
 * which a test may move (`setSystemTime`). An exchange presenting an access token this stand-in
 * minted that has run out is refused as the broker refuses a token it no longer honours. A read
 * trade's token (`scope=read`) of such a token lives no longer than that token has left,
 * truncated to whole seconds, as the broker's `clampNarrowed` bounds it and writes `expires_in`;
 * a plain trade's is not bound by its subject's, as the broker's is not.
 */
export function stubDeviceLoginBroker(options: DeviceLoginBrokerOptions = {}): DeviceLoginBroker {
  const polls = options.polls ?? ["approve"];
  const accessSeconds = options.accessSeconds ?? STAND_IN_ACCESS_SECONDS;
  const authorizations: DeviceLoginRequest[] = [];
  const pollsSeen: DeviceLoginRequest[] = [];
  const refreshes: DeviceLoginRequest[] = [];
  const trades: DeviceLoginRequest[] = [];
  const loginTokens: string[] = [];
  const tradeTokens: string[] = [];
  const revokedTrades: DeviceLoginRequest[] = [];
  const deviceCode = fixtureSecret("device");
  // The refreshes holding their answers for the next one to arrive (`refreshOverlapMs`), and the
  // exchanges holding theirs for a refresh (`tradeHoldMs`).
  const holding: (() => void)[] = [];
  const trading: (() => void)[] = [];
  // The one access token the last refresh answered: every other is revoked from then on.
  let live: string | undefined;
  // When each access token the device code and refresh grants minted lapses.
  const lapses = new Map<string, number>();
  const hold = async (into: (() => void)[], ms: number): Promise<void> => {
    const { promise, resolve } = Promise.withResolvers<void>();
    into.push(resolve);
    const timer = setTimeout(resolve, ms);
    await promise;
    clearTimeout(timer);
  };
  // The refreshes and exchanges waiting, with no time limit, for the stand-in to have been sent a
  // number of refreshes in all (`refreshesBeforeAnswer`, `refreshesBeforeTrade`).
  const awaitingRefreshes: { count: number; resolve: () => void }[] = [];
  const awaitingRefreshedTrades: { count: number; resolve: () => void }[] = [];
  const untilRefreshes = async (
    into: { count: number; resolve: () => void }[],
    count: number
  ): Promise<void> => {
    if (refreshes.length >= count) return;
    const { promise, resolve } = Promise.withResolvers<void>();
    into.push({ count, resolve });
    await promise;
  };
  const releaseReached = (from: { count: number; resolve: () => void }[]): void => {
    for (const waiter of from.filter((one) => one.count <= refreshes.length)) {
      from.splice(from.indexOf(waiter), 1);
      waiter.resolve();
    }
  };
  const loginAnswer = (): Response => {
    const access = fixtureSecret("login");
    loginTokens.push(access);
    lapses.set(access, Date.now() + accessSeconds * 1000);
    return Response.json({
      access_token: access,
      token_type: "DPoP",
      expires_in: accessSeconds,
      refresh_token: fixtureSecret("refresh"),
      scope: "machine",
      credential_id: options.credentialId ?? STAND_IN_CREDENTIAL,
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
        if (options.refuseAuthorization !== undefined) {
          return oauthError(options.refuseAuthorization);
        }
        return Response.json({
          device_code: deviceCode,
          user_code: options.userCode ?? STAND_IN_USER_CODE,
          verification_uri: options.verificationUri ?? `${origin}/credentials/machine`,
          ...(options.verificationUriComplete === undefined
            ? {}
            : { verification_uri_complete: options.verificationUriComplete }),
          expires_in: options.expiresIn ?? 600,
          ...(options.interval === null ? {} : { interval: options.interval ?? 1 }),
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
            case "invalid_grant":
              return oauthError("invalid_grant");
            case "invalid_client":
              return oauthError("invalid_client", 401);
            case "unknown-code":
              return oauthError(STAND_IN_UNKNOWN_CODE);
            case "html-403":
              return new Response("<html><body><h1>403 Forbidden</h1></body></html>", {
                status: 403,
                headers: { "Content-Type": "text/html" },
              });
            case "challenge-401":
              return new Response(null, {
                status: 401,
                headers: { "WWW-Authenticate": 'DPoP algs="ES256"' },
              });
            case "busy-429":
              return new Response("too many requests", { status: 429 });
            case "unavailable-503":
              return new Response("<html><body>503 Service Unavailable</body></html>", {
                status: 503,
                headers: { "Content-Type": "text/html" },
              });
            default:
              return loginAnswer();
          }
        }
        case "refresh_token": {
          refreshes.push(seen);
          for (const release of holding.splice(0)) release();
          releaseReached(awaitingRefreshes);
          if (options.refreshOverlapMs !== undefined) await hold(holding, options.refreshOverlapMs);
          if (options.refuseRefresh === true) return oauthError("invalid_grant");
          if (options.refuseRefresh === "invalid_client") return oauthError("invalid_client", 401);
          const answer = loginAnswer();
          live = loginTokens.at(-1);
          for (const release of trading.splice(0)) release();
          releaseReached(awaitingRefreshedTrades);
          if (options.refreshesBeforeAnswer !== undefined) {
            await untilRefreshes(awaitingRefreshes, options.refreshesBeforeAnswer);
          }
          return answer;
        }
        case "urn:ietf:params:oauth:grant-type:token-exchange": {
          trades.push(seen);
          if (options.tradeHoldMs !== undefined) await hold(trading, options.tradeHoldMs);
          if (options.refreshesBeforeTrade !== undefined) {
            await untilRefreshes(awaitingRefreshedTrades, options.refreshesBeforeTrade);
          }
          const subject = seen.form.get("subject_token") ?? "";
          const notHonoured = (): Response =>
            Response.json(
              {
                error: "invalid_request",
                error_description:
                  "The request is missing a required parameter, includes an invalid parameter value, includes a parameter more than once, or is otherwise malformed. Token is not valid or has expired.",
              },
              { status: 400 }
            );
          if (live !== undefined && subject !== live) {
            revokedTrades.push(seen);
            return notHonoured();
          }
          const lapse = lapses.get(subject);
          const left =
            lapse === undefined ? accessSeconds : Math.floor((lapse - Date.now()) / 1000);
          if (left <= 0) return notHonoured();
          const read = seen.form.get(FORM_SCOPE) === SCOPE_READ;
          const token = fixtureSecret("traded");
          tradeTokens.push(token);
          return Response.json({
            access_token: token,
            issued_token_type: "urn:ietf:params:oauth:token-type:access_token",
            token_type: "DPoP",
            expires_in: read ? Math.min(accessSeconds, left) : accessSeconds,
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
    revokedTrades,
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
 * any member of the JSON (an `access_token` the login holds, a `broker` of another). Beside an
 * `access_token` it writes the life the stand-in gives one, `access_life_seconds`
 * ({@link STAND_IN_ACCESS_SECONDS}), as a login of this build keeps it; a row writing a file an
 * older build left gives that member as `undefined`, which leaves it out.
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
    ...("access_token" in fields ? { access_life_seconds: STAND_IN_ACCESS_SECONDS } : {}),
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
