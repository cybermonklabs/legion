// packages/broker-client/src/__tests__/stub.ts
//
// The stub broker the renewal and environment suites trade against, and the stub issuer a
// person's sign-in is renewed at. The wire suite keeps a recorder of its own, since what it
// asserts is the raw shape of a request rather than the behaviour around it.

import { generateKeyPair, SignJWT } from "jose";

/** One trade as the stub saw it. */
export interface StubTrade {
  readonly form: URLSearchParams;
  readonly proofHeader: Record<string, unknown>;
  readonly proofClaims: Record<string, unknown>;
}

/** What the stub answers one trade with. */
export interface StubAnswer {
  readonly status?: number;
  readonly body: Record<string, unknown>;
  /** A `DPoP-Nonce` to send with the answer, which the next proof must carry. */
  readonly nonce?: string;
}

/** A broker that answers `answer` to each trade in turn, keeping every trade it was sent. */
export function stubBroker(answer: (trade: StubTrade, count: number) => StubAnswer): {
  readonly url: string;
  readonly trades: StubTrade[];
  stop(): void;
} {
  const trades: StubTrade[] = [];
  const server = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch: async (request) => {
      const parts = (request.headers.get("DPoP") ?? "").split(".");
      const read = (at: number): Record<string, unknown> =>
        parts.length < 3
          ? {}
          : (JSON.parse(Buffer.from(parts[at] as string, "base64url").toString("utf8")) as Record<
              string,
              unknown
            >);
      const trade: StubTrade = {
        form: new URLSearchParams(await request.text()),
        proofHeader: read(0),
        proofClaims: read(1),
      };
      trades.push(trade);
      const given = answer(trade, trades.length);
      return Response.json(given.body, {
        status: given.status ?? 200,
        ...(given.nonce === undefined ? {} : { headers: { "DPoP-Nonce": given.nonce } }),
      });
    },
  });
  return { url: `http://127.0.0.1:${server.port}`, trades, stop: () => server.stop(true) };
}

/** An accepted answer carrying `accessToken`, good for `seconds`. */
export function accepted(accessToken: string, seconds: number): Record<string, unknown> {
  return {
    access_token: accessToken,
    issued_token_type: "urn:ietf:params:oauth:token-type:access_token",
    token_type: "DPoP",
    expires_in: seconds,
    credential_id: "cred-1",
  };
}

/** One request the stub issuer was sent, as it saw it. */
export interface IssuerRequest {
  readonly path: string;
  /** The request's `Authorization` header, null where it carried none. */
  readonly authorization: string | null;
  readonly form: URLSearchParams;
}

/** What the stub issuer answers one refresh with in place of a good answer. A string body is
 *  sent as plain text, as an issuer's proxy answers a 503. */
export interface IssuerAnswer {
  readonly status?: number;
  readonly body: Record<string, unknown> | string;
}

/**
 * An OpenID provider serving its discovery document and its token endpoint, as Google and the
 * dev stack's issuer do. Each refresh is answered `answer(count)`, counted from 1, or, where that
 * answers nothing, as Google answers one: an access token and a fresh ID token for `person`,
 * minted for `clientId` by this issuer and kept in `idTokens`.
 */
export async function stubIssuer(options: {
  readonly clientId: string;
  readonly person: string;
  readonly answer?: (count: number) => IssuerAnswer | undefined;
}): Promise<{
  readonly url: string;
  readonly requests: IssuerRequest[];
  readonly idTokens: string[];
  stop(): void;
}> {
  const { privateKey } = await generateKeyPair("ES256");
  const requests: IssuerRequest[] = [];
  const idTokens: string[] = [];
  let refreshes = 0;
  const server = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch: async (request) => {
      // The issuer is the address it was asked at, which is the one its clients were told.
      const { origin: issuer, pathname: path } = new URL(request.url);
      requests.push({
        path,
        authorization: request.headers.get("Authorization"),
        form: new URLSearchParams(await request.text()),
      });
      if (path === "/.well-known/openid-configuration") {
        return Response.json({
          issuer,
          token_endpoint: `${issuer}/token`,
          device_authorization_endpoint: `${issuer}/device_authorization`,
          jwks_uri: `${issuer}/keys`,
          id_token_signing_alg_values_supported: ["ES256"],
          grant_types_supported: ["refresh_token", "urn:ietf:params:oauth:grant-type:device_code"],
        });
      }
      if (path !== "/token") return new Response("not found", { status: 404 });
      refreshes += 1;
      const given = options.answer?.(refreshes);
      if (given !== undefined) {
        const status = given.status ?? 200;
        return typeof given.body === "string"
          ? new Response(given.body, { status })
          : Response.json(given.body, { status });
      }
      const idToken = await new SignJWT({ email: options.person, email_verified: true })
        .setProtectedHeader({ alg: "ES256", typ: "JWT" })
        .setIssuer(issuer)
        .setSubject(`subject-of-${options.person}`)
        .setAudience(options.clientId)
        .setIssuedAt()
        .setExpirationTime("1h")
        .sign(privateKey);
      idTokens.push(idToken);
      return Response.json({
        access_token: `issuer-access-token-${refreshes}`,
        token_type: "Bearer",
        expires_in: 3600,
        scope: "openid email",
        id_token: idToken,
      });
    },
  });
  return {
    url: `http://127.0.0.1:${server.port}`,
    requests,
    idTokens,
    stop: () => server.stop(true),
  };
}
