// packages/cipher-client/src/dpop.ts
//
// The one RFC 9449 proof signer of this package, mirroring the Go signer in
// `packages/envoy/internal/cipherclient/dpop.go`. A key is one of two kinds: one a session makes
// in its own process (`newDpopKey`), or one read from PKCS#8 PEM (`importDpopKey`), which is the
// box key a person's sign-in on this machine keeps, which the broker bound the sign-in to, or a
// pod role's own agent-secrets key, which the role's registration names (`brokerTokenClient`,
// ./environment.ts). Either way only its public half ever leaves the process: a proof carries the
// public JWK in its header, and the private half is held in the closure that signs, which no
// caller of this package can reach.
//
// The private half is a `node:crypto` `KeyObject` rather than a WebCrypto `CryptoKey`, because a
// NATS connection's proof is signed synchronously (`connectionProof`): nats.js asks its
// authenticator for the CONNECT inside its own synchronous handshake, WebCrypto signs only
// asynchronously, and the runtime warns of a non-extractable `CryptoKey` turned into a
// `KeyObject` (`KeyObject.from`, DEP0204). One signer serves both proofs, as Go's `signProof`
// does, so the two cannot drift.

import {
  createHash,
  createPrivateKey,
  createPublicKey,
  generateKeyPairSync,
  type KeyObject,
  randomUUID,
  sign,
} from "node:crypto";
import type { JWK } from "jose";

/** The signature algorithm every proof carries: ES256 over a P-256 key, the one the broker's
 *  exchange and its resource servers accept. */
const ALGORITHM = "ES256";

/**
 * The `htm` and `htu` of a NATS connection's proof, Go's `oauthwire.ConnectionProofMethod` and
 * `oauthwire.ConnectionProofTarget`: a connection is no HTTP request, so the pair names the
 * connection rather than a method and a URL, and a proof signed for an HTTP request can never be
 * presented on a connection, nor the other way round.
 */
export const CONNECTION_PROOF_METHOD = "CONNECT";
export const CONNECTION_PROOF_TARGET = "nats:connect";

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

/** A NATS connection's proof's subject: the token its CONNECT presents and the nonce the server
 *  sent that one connection. */
export interface ConnectionProofRequest {
  readonly accessToken: string;
  readonly nonce: string;
}

/** A DPoP key: the proofs it signs, and the public half the broker records. */
export interface DpopKey {
  /** The public half, as it is embedded in every proof's header. It carries no `d` member. */
  readonly publicJwk: JWK;
  /** One compact proof for `request`. */
  proof(request: ProofRequest): Promise<string>;
  /**
   * The proof a NATS connection presenting `accessToken` carries in its CONNECT's signature
   * field: the claims {@link proof} signs, naming {@link CONNECTION_PROOF_METHOD},
   * {@link CONNECTION_PROOF_TARGET}, the token's hash and the nonce the server sent this
   * connection, which the auth service checks (Go's `ciphertoken.VerifyConnectionProof`). It is
   * synchronous, because nats.js calls its authenticator synchronously. A token or nonce that is
   * empty is refused, as Go's `cipherclient.ConnectionProof` refuses one.
   */
  connectionProof(request: ConnectionProofRequest): string;
}

/** The `htu` claim for `url`: the target URI with no query and no fragment. */
function targetUri(url: string): string {
  const parsed = new URL(url);
  parsed.search = "";
  parsed.hash = "";
  return parsed.toString();
}

/**
 * The key that signs with `privateKey` and embeds `publicJwk`, whichever kind it is: `typ:
 * dpop+jwt`, `alg: ES256` and the public JWK in the header, and `jti`, `htm`, `htu` and `iat` in
 * the claims, with `nonce` and `ath` added only where the request has them. The claim set is the
 * whole of what a proof carries, as `testdata/wire.json` names it, and both kinds of key and both
 * kinds of proof share this one signer so none can drift from it. The signature is ECDSA over
 * SHA-256 in the IEEE P1363 form (`r || s`) a JWS carries (RFC 7518 Section 3.4).
 */
function dpopKey(privateKey: KeyObject, publicJwk: JWK): DpopKey {
  const header = Buffer.from(
    JSON.stringify({ typ: "dpop+jwt", alg: ALGORITHM, jwk: publicJwk })
  ).toString("base64url");
  const signed = (htm: string, htu: string, nonce?: string, accessToken?: string): string => {
    const claims: Record<string, string | number> = {
      jti: randomUUID(),
      htm,
      htu,
      iat: Math.floor(Date.now() / 1000),
    };
    if (nonce !== undefined && nonce !== "") claims["nonce"] = nonce;
    if (accessToken !== undefined && accessToken !== "") {
      claims["ath"] = createHash("sha256").update(accessToken).digest("base64url");
    }
    const signingInput = `${header}.${Buffer.from(JSON.stringify(claims)).toString("base64url")}`;
    const signature = sign("sha256", Buffer.from(signingInput), {
      key: privateKey,
      dsaEncoding: "ieee-p1363",
    });
    return `${signingInput}.${signature.toString("base64url")}`;
  };
  return {
    publicJwk,
    async proof(request: ProofRequest): Promise<string> {
      return signed(
        request.method.toUpperCase(),
        targetUri(request.url),
        request.nonce,
        request.accessToken
      );
    },
    connectionProof({ accessToken, nonce }: ConnectionProofRequest): string {
      if (accessToken === "") throw new Error("no access token to prove a NATS connection for");
      if (nonce === "") throw new Error("the NATS server sent no nonce to sign");
      return signed(CONNECTION_PROOF_METHOD, CONNECTION_PROOF_TARGET, nonce, accessToken);
    },
  };
}

/**
 * The public JWK of `privateKey`, kept to `kty`, `crv`, `x` and `y`, so a proof of either kind of
 * key carries one header shape; or the refusal of a key that is not P-256, since ES256 is the one
 * algorithm the broker accepts. `name` says whose key it is.
 */
function publicJwkOf(privateKey: KeyObject, name: string): JWK {
  const { kty, crv, x, y } = createPublicKey(privateKey).export({ format: "jwk" });
  if (kty !== "EC" || crv !== "P-256" || typeof x !== "string" || typeof y !== "string") {
    throw new Error(
      `${name} is not a P-256 key (${kty} ${crv ?? "with no curve"}), and a DPoP proof is signed ${ALGORITHM}`
    );
  }
  return { kty, crv, x, y };
}

/** A key of this process's own, made here and held nowhere else. */
export async function newDpopKey(): Promise<DpopKey> {
  const { privateKey } = generateKeyPairSync("ec", { namedCurve: "P-256" });
  return dpopKey(privateKey, publicJwkOf(privateKey, "this process's key"));
}

/**
 * The key `pkcs8Pem` holds, as a DPoP key: a person's box key, which `agent-secrets login`
 * wrote as PKCS#8 PEM and the broker bound their sign-in to, so every trade of that sign-in
 * proves this key and no other; or a pod role's own agent-secrets key, which `agent-secrets
 * keygen` wrote and the role's registration names. `name` says whose key it is, for a refusal to
 * name. A key that is not P-256 is refused. A key that does not parse is refused in this
 * package's own words, naming what was read, with none of the runtime's text and no cause: a
 * parse error can quote the bytes it was handed, which here are key material, and a cause is
 * printed with the error wherever the error is printed whole.
 */
export async function importDpopKey(pkcs8Pem: string, name: string): Promise<DpopKey> {
  let privateKey: KeyObject;
  try {
    privateKey = createPrivateKey(pkcs8Pem);
  } catch {
    throw new Error(`read ${name}: it does not parse as a PKCS#8 PEM private key`);
  }
  return dpopKey(privateKey, publicJwkOf(privateKey, name));
}
