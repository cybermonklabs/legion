// packages/broker-client/test/sign-ins.ts
//
// A person's sign-in on a machine as `agent-secrets login` leaves it
// (`packages/envoy/internal/brokerclient/signinstore.go`, `SignInStore.Write`), and the OpenID
// provider it is renewed at, for every suite that drives a person's trade: this package's own,
// and envoy-client's broker credential. Every key is made at run time, so the repository holds no
// private key.

import { createHash, generateKeyPairSync } from "node:crypto";
import { chmod, mkdir, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { calculateJwkThumbprint, generateKeyPair, type JWK, SignJWT } from "jose";
import type { SignInRecord } from "../src/signins";

/** The RFC 7638 thumbprint of `jwk`: the one `agent-secrets login` records for a box key, and
 *  the one a broker reads off a proof's header, for a suite whose package has no `jose`. */
export async function thumbprintOf(jwk: JWK): Promise<string> {
  return await calculateJwkThumbprint(jwk);
}

/**
 * The record `agent-secrets login` keeps for `person`, with a box key of its own: a P-256 key
 * written as PKCS#8 PEM, as Go's `x509.MarshalPKCS8PrivateKey` writes one, and that key's
 * thumbprint (Go's `SignInRecord`). `overrides` replaces any member.
 */
export async function signedIn(
  person: string,
  overrides: Partial<SignInRecord> = {}
): Promise<SignInRecord> {
  const { privateKey, publicKey } = generateKeyPairSync("ec", { namedCurve: "P-256" });
  return {
    person,
    issuer: "https://accounts.example",
    client_id: "example-device-client",
    refresh_token: `refresh-token-of-${person}`,
    box_key: privateKey.export({ format: "pem", type: "pkcs8" }).toString(),
    thumbprint: await thumbprintOf(publicKey.export({ format: "jwk" }) as JWK),
    enrollment_id: "",
    signed_in_at: new Date().toISOString(),
    ...overrides,
  };
}

/** The file Go keeps `person`'s record in under `dir`: the SHA-256 of the trimmed, lowercased
 *  address (`SignInStore.Path`). */
export function signInPath(dir: string, person: string): string {
  const digest = createHash("sha256").update(person.trim().toLowerCase()).digest("hex");
  return join(dir, `${digest}.json`);
}

/**
 * Writes `record` under `dir` as Go's `SignInStore.Write` does: the directory 0700, the file
 * 0600, the JSON indented by two spaces and ended by a newline, and no `enrollment_id` while it
 * is empty (`omitempty`). A second record for the same person replaces the first, as a new
 * `agent-secrets login` does. Answers the file written.
 */
export async function writeSignIn(dir: string, record: SignInRecord): Promise<string> {
  await mkdir(dir, { recursive: true, mode: 0o700 });
  await chmod(dir, 0o700);
  const { enrollment_id, ...rest } = record;
  const path = signInPath(dir, record.person);
  const written = enrollment_id === "" ? rest : record;
  await writeFile(path, `${JSON.stringify(written, null, 2)}\n`, { mode: 0o600 });
  return path;
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
