// packages/broker-client/src/dpop.ts
//
// The one RFC 9449 proof signer of this package, mirroring the Go signer in
// `packages/envoy/internal/brokerclient/dpop.go`. A key is one of two kinds: one a session makes
// in its own process (`newDpopKey`), or the box key a person's sign-in on this machine keeps
// (`importDpopKey`), which the broker bound the sign-in to. Either way only its public half ever
// leaves the process: a proof carries the public JWK in its header, and nothing here can export
// the private half at all — `jose` makes it a non-extractable `CryptoKey`, so `exportJWK` on it
// fails rather than writing a `d` member.

import { createPrivateKey, createPublicKey } from "node:crypto";
import { exportJWK, generateKeyPair, importPKCS8, type JWK, SignJWT } from "jose";
import { messageOf } from "./error-text";

/** The signature algorithm every proof carries: ES256 over a P-256 key, the one the broker's
 *  exchange and its resource servers accept. */
const ALGORITHM = "ES256";

/** One proof's subject: the request it is signed for. */
export interface ProofRequest {
  /** The HTTP method, uppercased into the `htm` claim. */
  readonly method: string;
  /** The request's URL. Its query and fragment are dropped: `htu` is the target URI alone. */
  readonly url: string;
  /**
   * The access token this request presents under the DPoP scheme, which the proof names as `ath`,
   * the base64url SHA-256 of it (RFC 9449 Section 4.3 step 12), so a proof is good only for the
   * one token it was signed beside. A token endpoint request presents none — the token endpoint
   * is where one is asked for — and its proof carries no `ath`.
   */
  readonly accessToken?: string;
  /** The server's most recent `DPoP-Nonce`, carried once it has sent one. */
  readonly nonce?: string;
}

/** A DPoP key: the proofs it signs, and the public half the broker records. */
export interface DpopKey {
  /** The public half, as it is embedded in every proof's header. It carries no `d` member: the
   *  private half cannot be exported from the key at all. */
  readonly publicJwk: JWK;
  /** One compact proof for `request`. */
  proof(request: ProofRequest): Promise<string>;
}

/** The `htu` claim for `url`: the target URI with no query and no fragment. */
function targetUri(url: string): string {
  const parsed = new URL(url);
  parsed.search = "";
  parsed.hash = "";
  return parsed.toString();
}

/** The `ath` claim for `accessToken`: its SHA-256, base64url with no padding. */
async function accessTokenHash(accessToken: string): Promise<string> {
  const digest = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(accessToken));
  return Buffer.from(digest).toString("base64url");
}

/**
 * The key that signs with `privateKey` and embeds `publicJwk`, whichever kind it is: `typ:
 * dpop+jwt`, `alg: ES256` and the public JWK in the header, and `jti`, `htm`, `htu` and `iat` in
 * the claims, with `nonce` and `ath` added only where the request has them. The claim set is the
 * whole of what a proof carries, as `testdata/wire.json` names it, and both kinds of key share
 * this one signer so neither can drift from it.
 */
function dpopKey(privateKey: CryptoKey, publicJwk: JWK): DpopKey {
  return {
    publicJwk,
    async proof(request: ProofRequest): Promise<string> {
      const claims: Record<string, string | number> = {
        jti: crypto.randomUUID(),
        htm: request.method.toUpperCase(),
        htu: targetUri(request.url),
        iat: Math.floor(Date.now() / 1000),
      };
      if (request.nonce !== undefined && request.nonce !== "") claims["nonce"] = request.nonce;
      if (request.accessToken !== undefined && request.accessToken !== "") {
        claims["ath"] = await accessTokenHash(request.accessToken);
      }
      return await new SignJWT(claims)
        .setProtectedHeader({ typ: "dpop+jwt", alg: ALGORITHM, jwk: publicJwk })
        .sign(privateKey);
    },
  };
}

/** A key of this process's own, made here and held nowhere else. */
export async function newDpopKey(): Promise<DpopKey> {
  const { privateKey, publicKey } = await generateKeyPair(ALGORITHM, { extractable: false });
  return dpopKey(privateKey, await exportJWK(publicKey));
}

/**
 * The key `pkcs8Pem` holds, as a DPoP key: a person's box key, which `agent-secrets login`
 * wrote as PKCS#8 PEM and the broker bound their sign-in to, so every trade of that sign-in
 * proves this key and no other. `name` says whose key it is, for a refusal to name.
 *
 * The private half is imported non-extractable for ES256, as {@link newDpopKey}'s is made. The
 * public JWK is derived with `node:crypto` and kept to `kty`, `crv`, `x` and `y`, the members
 * `jose` exports for a key it made, so a proof of either kind carries one header shape. A key
 * that is not P-256 is refused, since ES256 is the one algorithm the broker accepts.
 */
export async function importDpopKey(pkcs8Pem: string, name: string): Promise<DpopKey> {
  /** The refusal of a key that could not be read, naming whose it is. */
  const unreadable = (error: unknown): Error =>
    new Error(`read ${name}: ${messageOf(error)}`, { cause: error });
  let exported: JWK;
  try {
    exported = createPublicKey(createPrivateKey(pkcs8Pem)).export({ format: "jwk" });
  } catch (error) {
    throw unreadable(error);
  }
  const { kty, crv, x, y } = exported;
  if (kty !== "EC" || crv !== "P-256" || typeof x !== "string" || typeof y !== "string") {
    throw new Error(
      `${name} is not a P-256 key (${kty} ${crv ?? "with no curve"}), and a DPoP proof is signed ${ALGORITHM}`
    );
  }
  let privateKey: CryptoKey;
  try {
    privateKey = await importPKCS8(pkcs8Pem, ALGORITHM, { extractable: false });
  } catch (error) {
    throw unreadable(error);
  }
  return dpopKey(privateKey, { kty, crv, x, y });
}
