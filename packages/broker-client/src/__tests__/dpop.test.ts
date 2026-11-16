// packages/broker-client/src/__tests__/dpop.test.ts
//
// The proof this process signs: what it carries, what it never carries, and that it is signed by
// the key whose public half it embeds, for a key this process makes and for a person's box key.

import { expect, test } from "bun:test";
import { createPublicKey, generateKeyPairSync } from "node:crypto";
import { type JWK, jwtVerify } from "jose";
import { importDpopKey, newDpopKey } from "../dpop";

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
  // The private half is held in the closure that signs, as a key `jose` made non-extractable, so
  // no caller of this package can reach it and `crypto.subtle` would refuse to export it.
  expect(Object.keys(key).sort()).toEqual(["proof", "publicJwk"]);
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
  const accessToken = "broker-access-token";
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
  expect(Object.keys(key).sort()).toEqual(["proof", "publicJwk"]);
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
