// packages/envoy-client/src/__tests__/sign-ins.ts
//
// A person's sign-in on a machine as `agent-secrets login` leaves it, and the OpenID provider it
// is renewed at, for the suites that drive a broker credential through a person's trade. Every
// key is made at run time, so the repository holds no private key.

import { createHash, generateKeyPairSync, type KeyObject, sign } from "node:crypto";
import { chmod, mkdir, writeFile } from "node:fs/promises";
import { join } from "node:path";
import type { SignInRecord } from "@legion/broker-client/signins";

/** The members of a public JWK an RFC 7638 thumbprint of an EC key is taken over. */
interface EcPublicJwk {
  readonly crv?: unknown;
  readonly kty?: unknown;
  readonly x?: unknown;
  readonly y?: unknown;
}

/** The RFC 7638 thumbprint of an EC public key: the SHA-256 of its required members, in
 *  lexicographic order with no whitespace, base64url. */
export function thumbprintOf(jwk: EcPublicJwk): string {
  const required = JSON.stringify({ crv: jwk.crv, kty: jwk.kty, x: jwk.x, y: jwk.y });
  return createHash("sha256").update(required).digest("base64url");
}

/**
 * The record `agent-secrets login` keeps for `person`, with a box key of its own: a P-256 key
 * written as PKCS#8 PEM, as Go's `x509.MarshalPKCS8PrivateKey` writes one, and that key's
 * thumbprint (Go's `SignInRecord`). `overrides` replaces any member.
 */
export function signedIn(person: string, overrides: Partial<SignInRecord> = {}): SignInRecord {
  const { privateKey, publicKey } = generateKeyPairSync("ec", { namedCurve: "P-256" });
  return {
    person,
    issuer: "https://accounts.example",
    client_id: "example-device-client",
    refresh_token: `refresh-token-of-${person}`,
    box_key: privateKey.export({ format: "pem", type: "pkcs8" }).toString(),
    thumbprint: thumbprintOf(publicKey.export({ format: "jwk" })),
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
 * is empty. A second record for the same person replaces the first, as a new
 * `agent-secrets login` does.
 */
export async function writeSignIn(dir: string, record: SignInRecord): Promise<void> {
  await mkdir(dir, { recursive: true, mode: 0o700 });
  await chmod(dir, 0o700);
  const { enrollment_id, ...rest } = record;
  const written = enrollment_id === "" ? rest : record;
  await writeFile(signInPath(dir, record.person), `${JSON.stringify(written, null, 2)}\n`, {
    mode: 0o600,
  });
}

/** One request the stub issuer was sent. */
export interface IssuerRequest {
  readonly path: string;
  readonly form: URLSearchParams;
}

/** `value` as one base64url JWT segment. */
function segment(value: Record<string, unknown>): string {
  return Buffer.from(JSON.stringify(value)).toString("base64url");
}

/** An ES256 JWT of `claims`, signed with `key`. */
function signedJwt(claims: Record<string, unknown>, key: KeyObject): string {
  const input = `${segment({ alg: "ES256", typ: "JWT" })}.${segment(claims)}`;
  const signature = sign("sha256", Buffer.from(input), { key, dsaEncoding: "ieee-p1363" });
  return `${input}.${signature.toString("base64url")}`;
}

/**
 * An OpenID provider serving its discovery document and its token endpoint, as Google and the
 * dev stack's issuer do. Each refresh is answered `answer(count)`, counted from 1, where that
 * answers something, and otherwise as Google answers one: an access token and a fresh ID token
 * for `person`, minted for `clientId` by this issuer.
 */
export function stubIssuer(options: {
  readonly clientId: string;
  readonly person: string;
  readonly answer?: (count: number) => Response | undefined;
}): { readonly url: string; readonly requests: IssuerRequest[]; stop(): void } {
  const { privateKey } = generateKeyPairSync("ec", { namedCurve: "P-256" });
  const requests: IssuerRequest[] = [];
  let refreshes = 0;
  const server = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch: async (request) => {
      // The issuer is the address it was asked at, which is the one its clients were told.
      const { origin: issuer, pathname: path } = new URL(request.url);
      requests.push({ path, form: new URLSearchParams(await request.text()) });
      if (path === "/.well-known/openid-configuration") {
        return Response.json({
          issuer,
          token_endpoint: `${issuer}/token`,
          jwks_uri: `${issuer}/keys`,
          id_token_signing_alg_values_supported: ["ES256"],
          grant_types_supported: ["refresh_token", "urn:ietf:params:oauth:grant-type:device_code"],
        });
      }
      if (path !== "/token") return new Response("not found", { status: 404 });
      refreshes += 1;
      const given = options.answer?.(refreshes);
      if (given !== undefined) return given;
      const now = Math.floor(Date.now() / 1000);
      const idToken = signedJwt(
        {
          iss: issuer,
          sub: `subject-of-${options.person}`,
          aud: options.clientId,
          iat: now,
          exp: now + 3600,
          email: options.person,
          email_verified: true,
        },
        privateKey
      );
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
    stop: () => server.stop(true),
  };
}
