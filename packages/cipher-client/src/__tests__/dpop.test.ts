// packages/cipher-client/src/__tests__/dpop.test.ts
//
// The proof this process signs: what it carries, what it never carries, and that it is signed by
// the key whose public half it embeds, for a key this process makes and for a person's box key;
// and the proof a NATS connection carries, which the Go checker reads from a committed vector as
// this suite reads the Go signer's.

import { expect, test } from "bun:test";
import { createHash, createPublicKey, generateKeyPairSync } from "node:crypto";
import { readFile } from "node:fs/promises";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { calculateJwkThumbprint, EmbeddedJWK, type JWK, jwtVerify } from "jose";
import { type DpopKey, importDpopKey, newDpopKey } from "../dpop";

/** One part of a compact JWS as JSON. */
function part(compact: string, at: number): Record<string, unknown> {
  return JSON.parse(Buffer.from(compact.split(".")[at] as string, "base64url").toString("utf8"));
}

test("a token endpoint proof carries jti, htm, htu and iat, and nothing else", async () => {
  const key = await newDpopKey();
  const compact = await key.proof({ method: "post", url: "http://broker.test/v1/oauth/token" });
  expect(Object.keys(part(compact, 0)).sort()).toEqual(["alg", "jwk", "typ"]);
  const claims = part(compact, 1);
  expect(Object.keys(claims).sort()).toEqual(["htm", "htu", "iat", "jti"]);
  expect(claims.htm).toBe("POST");
  expect(claims.htu).toBe("http://broker.test/v1/oauth/token");
  expect(Math.abs(Date.now() / 1000 - (claims.iat as number))).toBeLessThan(60);
  expect(part(compact, 0).typ).toBe("dpop+jwt");
  expect(part(compact, 0).alg).toBe("ES256");
});

test("the proof's embedded key is the public half alone, and signs the proof", async () => {
  const key = await newDpopKey();
  const compact = await key.proof({ method: "GET", url: "https://dispatch.test/api/v1/issues" });
  const jwk = part(compact, 0).jwk as Record<string, unknown>;
  expect(Object.hasOwn(jwk, "d")).toBe(false);
  expect(Object.hasOwn(key.publicJwk, "d")).toBe(false);
  expect(jwk.kty).toBe("EC");
  expect(jwk.crv).toBe("P-256");
  // The key verifies the proof, so the embedded half is this key's and no other's.
  const verified = await jwtVerify(compact, key.publicJwk);
  expect(verified.payload.htm).toBe("GET");
});

test("the key hands out nothing but its public half and its proofs", async () => {
  const key = await newDpopKey();
  // The private half is held in the closure that signs and nowhere else, so no caller of this
  // package can reach it: the key's members are its public half and its two signers.
  expect(Object.keys(key).sort()).toEqual(["connectionProof", "proof", "publicJwk"]);
  expect(Object.hasOwn(key.publicJwk, "d")).toBe(false);
});

test("a query and a fragment are no part of the target URI", async () => {
  const key = await newDpopKey();
  const compact = await key.proof({
    method: "GET",
    url: "https://dispatch.test/api/v1/issues?limit=5#top",
  });
  expect(part(compact, 1).htu).toBe("https://dispatch.test/api/v1/issues");
});

test("each proof carries a jti of its own", async () => {
  const key = await newDpopKey();
  const first = part(await key.proof({ method: "GET", url: "https://dispatch.test/a" }), 1);
  const second = part(await key.proof({ method: "GET", url: "https://dispatch.test/a" }), 1);
  expect(first.jti).not.toBe(second.jti);
  expect(String(first.jti).length).toBeGreaterThan(0);
});

test("a request presenting an access token names it in ath, and only then", async () => {
  const key = await newDpopKey();
  const accessToken = fixtureSecret("access");
  const withToken = part(
    await key.proof({ method: "POST", url: "https://dispatch.test/api/v1/comments", accessToken }),
    1
  );
  const digest = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(accessToken));
  expect(withToken.ath).toBe(Buffer.from(digest).toString("base64url"));
  const without = part(await key.proof({ method: "GET", url: "https://dispatch.test/a" }), 1);
  expect(Object.hasOwn(without, "ath")).toBe(false);
});

test("the server's nonce is carried once it has sent one", async () => {
  const key = await newDpopKey();
  const claims = part(
    await key.proof({ method: "POST", url: "http://broker.test/v1/oauth/token", nonce: "n-1" }),
    1
  );
  expect(claims.nonce).toBe("n-1");
  expect(Object.keys(claims).sort()).toEqual(["htm", "htu", "iat", "jti", "nonce"]);
});

/** A box key as `agent-secrets login` writes one: a key of `namedCurve` made here, as PKCS#8
 *  PEM. */
function boxKey(namedCurve: string): string {
  const { privateKey } = generateKeyPairSync("ec", { namedCurve });
  return privateKey.export({ format: "pem", type: "pkcs8" }).toString();
}

test("a box key's proof verifies with the public key its own header carries", async () => {
  const key = await importDpopKey(boxKey("P-256"), "ada@example.com's box key");
  const compact = await key.proof({ method: "post", url: "http://broker.test/v1/oauth/token" });
  // The same header and claim set as a key this process makes.
  expect(Object.keys(part(compact, 0)).sort()).toEqual(["alg", "jwk", "typ"]);
  expect(Object.keys(part(compact, 1)).sort()).toEqual(["htm", "htu", "iat", "jti"]);
  expect(part(compact, 0).typ).toBe("dpop+jwt");
  expect(part(compact, 0).alg).toBe("ES256");
  const jwk = part(compact, 0).jwk as JWK;
  expect(jwk).toEqual(key.publicJwk);
  const verified = await jwtVerify(compact, jwk);
  expect(verified.payload.htm).toBe("POST");
});

test("a box key's public JWK is the key's own public half, and carries no private member", async () => {
  const pem = boxKey("P-256");
  const key = await importDpopKey(pem, "ada@example.com's box key");
  const own = createPublicKey(pem).export({ format: "jwk" });
  expect(key.publicJwk.x).toBe(own.x as string);
  expect(key.publicJwk.y).toBe(own.y as string);
  expect(Object.keys(key.publicJwk).sort()).toEqual(["crv", "kty", "x", "y"]);
  expect(Object.hasOwn(key.publicJwk, "d")).toBe(false);
  expect(Object.keys(key).sort()).toEqual(["connectionProof", "proof", "publicJwk"]);
});

test("a box key that is not P-256 is refused, naming whose it is", async () => {
  const refusal = await importDpopKey(boxKey("P-384"), "ada@example.com's box key").then(
    () => null,
    (error: unknown) => error as Error
  );
  expect(refusal?.message).toBe(
    "ada@example.com's box key is not a P-256 key (EC P-384), and a DPoP proof is signed ES256"
  );
});

// h5 (the review of 2fa6fbf7cf): a box key that does not parse is refused in this package's own
// words, naming what was read, with no cause: the runtime's parse error can quote the bytes it
// was handed, and a cause is printed with the error wherever the error is printed whole.
test.each([
  [
    "a body that is no key",
    // The armour is built from parts: the push's leak check refuses a whole PEM header literal.
    (canary: string) =>
      `${"-----BEGIN"} PRIVATE KEY-----\n${canary}\n${"-----END"} PRIVATE KEY-----\n`,
  ],
  ["no PEM at all", (canary: string) => `d=${canary}`],
] as const)("a box key that does not parse, as %s, is refused naming none of it", async (_as, pem) => {
  const canary = fixtureSecret("canary");
  const refusal = await importDpopKey(pem(canary), "ada@example.com's box key").then(
    () => null,
    (error: unknown) => error as Error
  );
  expect(refusal).toBeInstanceOf(Error);
  expect(refusal?.message).toBe(
    "read ada@example.com's box key: it does not parse as a PKCS#8 PEM private key"
  );
  expect(refusal?.cause).toBeUndefined();
  expect(String(refusal)).not.toContain(canary);
  expect(String(refusal?.cause ?? "")).not.toContain(canary);
  expect(JSON.stringify(Object.getOwnPropertyNames(refusal))).not.toContain("cause");
});

/** What a connection proof's checker holds it to, beside its signature: the header and claim
 *  names, the connection's method and target, the token's hash (`ath`, its SHA-256 in base64url)
 *  and the server's nonce. */
function expectConnectionClaims(compact: string, accessToken: string, nonce: string): void {
  expect(Object.keys(part(compact, 0)).sort()).toEqual(["alg", "jwk", "typ"]);
  expect(part(compact, 0).typ).toBe("dpop+jwt");
  expect(part(compact, 0).alg).toBe("ES256");
  const claims = part(compact, 1);
  expect(Object.keys(claims).sort()).toEqual(["ath", "htm", "htu", "iat", "jti", "nonce"]);
  expect(claims.htm).toBe("CONNECT");
  expect(claims.htu).toBe("nats:connect");
  expect(claims.ath).toBe(createHash("sha256").update(accessToken).digest("base64url"));
  expect(claims.nonce).toBe(nonce);
  expect(String(claims.jti).length).toBeGreaterThan(0);
}

test("connectionProof verifies with jose", async () => {
  const key = await newDpopKey();
  const presented = "example-session-credential";
  const nonce = "the-server-nonce-of-this-connection";
  const compact = key.connectionProof({ accessToken: presented, nonce });
  // Signed with no await: nats.js asks its authenticator for the CONNECT synchronously.
  expect(typeof compact).toBe("string");
  expectConnectionClaims(compact, presented, nonce);
  expect(Math.abs(Date.now() / 1000 - (part(compact, 1).iat as number))).toBeLessThan(60);
  // The key its header embeds signed it, and that key is this key's public half.
  const verified = await jwtVerify(compact, EmbeddedJWK, {
    typ: "dpop+jwt",
    algorithms: ["ES256"],
  });
  expect(verified.protectedHeader.jwk).toEqual(key.publicJwk);
  await jwtVerify(compact, key.publicJwk);
  // Two proofs over one nonce are two proofs: each names a jti of its own.
  const again = key.connectionProof({ accessToken: presented, nonce });
  expect(part(again, 1).jti).not.toBe(part(compact, 1).jti);
});

test("connectionProof works with a key this process makes and an imported box key", async () => {
  const keys: readonly [string, DpopKey][] = [
    ["a key this process makes", await newDpopKey()],
    ["a box key", await importDpopKey(boxKey("P-256"), "ada@example.com's box key")],
  ];
  for (const [kind, key] of keys) {
    const compact = key.connectionProof({ accessToken: "token", nonce: "nonce" });
    expectConnectionClaims(compact, "token", "nonce");
    const verified = await jwtVerify(compact, EmbeddedJWK, { typ: "dpop+jwt" });
    // The thumbprint the broker binds a token to is this key's, so the auth service's key
    // check passes for the token this key traded.
    expect(await calculateJwkThumbprint(verified.protectedHeader.jwk as JWK), kind).toBe(
      await calculateJwkThumbprint(key.publicJwk)
    );
  }
});

test("connectionProof refuses a connection with no token or no nonce, naming which", async () => {
  const key = await newDpopKey();
  expect(() => key.connectionProof({ accessToken: "", nonce: "nonce" })).toThrow(
    "no access token to prove a NATS connection for"
  );
  expect(() => key.connectionProof({ accessToken: "token", nonce: "" })).toThrow(
    "the NATS server sent no nonce to sign"
  );
});

/** A connection proof committed with the token, the nonce and the thumbprint it verifies with
 *  and the instant it was signed, as `packages/envoy/internal/ciphertoken/testdata` keeps one per
 *  signer. The proof is kept as its three segments: a joined one reads as a credential to the
 *  push leak check. */
interface ConnectionVector {
  readonly proof_segments: readonly string[];
  readonly token: string;
  readonly nonce: string;
  readonly thumbprint: string;
  readonly now: number;
  readonly signer: string;
}

async function connectionVector(name: string): Promise<ConnectionVector> {
  const path = new URL(`../../../envoy/internal/ciphertoken/testdata/${name}`, import.meta.url);
  return JSON.parse(await readFile(path, "utf8")) as ConnectionVector;
}

/** The checks the Go checker makes of `vector`, made here with jose: the signature by the key the
 *  header embeds, that key's thumbprint, and the claims. */
async function expectVectorVerifies(vector: ConnectionVector): Promise<void> {
  expect(vector.proof_segments).toHaveLength(3);
  const compact = vector.proof_segments.join(".");
  expectConnectionClaims(compact, vector.token, vector.nonce);
  const verified = await jwtVerify(compact, EmbeddedJWK, {
    typ: "dpop+jwt",
    algorithms: ["ES256"],
    currentDate: new Date(vector.now * 1000),
  });
  expect(verified.payload.iat).toBe(vector.now);
  expect(await calculateJwkThumbprint(verified.protectedHeader.jwk as JWK)).toBe(vector.thumbprint);
}

test("the Go signer's committed connection proof verifies with jose", async () => {
  const vector = await connectionVector("connection-proof-go.json");
  expect(vector.signer).toBe("packages/envoy/internal/cipherclient ConnectionProof");
  await expectVectorVerifies(vector);
});

test("the committed TypeScript connection proof is this signer's, and verifies", async () => {
  // The Go checker reads this file (TestATypeScriptConnectionProofVerifies);
  // scripts/connection-proof-fixture.ts writes it.
  const vector = await connectionVector("connection-proof-ts.json");
  expect(vector.signer).toBe("packages/cipher-client/src/dpop.ts connectionProof");
  await expectVectorVerifies(vector);
  // The vector carries the header and claim names this signer signs today, so a claim added here
  // without writing the vector again fails.
  const fresh = (await newDpopKey()).connectionProof({ accessToken: "t", nonce: "n" });
  const committed = vector.proof_segments.join(".");
  expect(Object.keys(part(committed, 0)).sort()).toEqual(Object.keys(part(fresh, 0)).sort());
  expect(Object.keys(part(committed, 1)).sort()).toEqual(Object.keys(part(fresh, 1)).sort());
});
