/** `bun e2e/acceptance/broker-session.ts`: the credential graph-store.sh writes with as a session
 *  the secrets broker proved a person's. Dispatch records such a session as that person's (an
 *  alias_of tuple, graphstore's aliases) only for an access token the broker scoped to the person,
 *  presented under RFC 9449's DPoP scheme with a fresh ES256 proof on every request
 *  (packages/envoy/internal/brokertoken). The harness's broker is e2e/fake-broker.ts, which calls
 *  a token minted by e2e/fake-broker-token.ts live with the answer it carries, so this mints one
 *  bound to a key of its own and signs each request's proof with that key, as
 *  packages/broker-client/src/dpop.ts signs a real session's.
 *
 *    mint KEY SESSION PERSON PROJECT
 *        makes a P-256 key and writes it to the file KEY, readable by its owner alone, and prints
 *        a token naming SESSION, acting for PERSON with PROJECT in its list, bound to that key.
 *    send KEY TOKEN METHOD URL OUT [JSON]
 *        sends METHOD URL with JSON as its body, presenting the token in the file TOKEN with a
 *        proof the key in KEY signed for this one request, writes the answer's body to the file
 *        OUT and prints its status.
 *
 *  Neither prints the key or, beyond what mint answers on standard output, the token. */
import { randomUUID } from "node:crypto";
import { readFileSync, writeFileSync } from "node:fs";

import { fakeBrokerToken } from "../fake-broker-token";

/** The one curve and signature every proof is made with: ES256, the only one Dispatch accepts. */
const curve = { name: "ECDSA", namedCurve: "P-256" } as const;

/** The client every token issued for acting as a session is issued to (oauthwire's
 *  ExchangeClientID): Dispatch refuses a token issued to any other. */
const exchangeClientID = "dispatch-token-exchange";

function base64url(bytes: ArrayBuffer | Uint8Array | string): string {
  return Buffer.from(typeof bytes === "string" ? bytes : new Uint8Array(bytes)).toString(
    "base64url"
  );
}

async function sha256(text: string): Promise<string> {
  return base64url(await crypto.subtle.digest("SHA-256", new TextEncoder().encode(text)));
}

/** The public half of a key: RFC 7638's required members of a P-256 key, in its lexicographic
 *  order, which is both what a proof's header carries and what the key's thumbprint hashes. */
function publicHalf(jwk: JsonWebKey): JsonWebKey {
  return { crv: jwk.crv, kty: jwk.kty, x: jwk.x, y: jwk.y };
}

async function mint(keyFile: string, session: string, person: string, project: string) {
  const { privateKey } = await crypto.subtle.generateKey(curve, true, ["sign", "verify"]);
  const key = await crypto.subtle.exportKey("jwk", privateKey);
  writeFileSync(keyFile, JSON.stringify(key), { mode: 0o600 });
  const token = fakeBrokerToken({
    active: true,
    client_id: exchangeClientID,
    exp: Math.floor(Date.now() / 1000) + 3600,
    // RFC 7638's thumbprint, which is what a token's `jkt` binds it to.
    jkt: await sha256(JSON.stringify(publicHalf(key))),
    person,
    project_scope: "person",
    projects: [project],
    session,
  });
  process.stdout.write(token);
}

/** One proof for `method url` beside `token`: RFC 9449 Section 4.2's header and claims, the
 *  target URI without its query, and `ath` the token's own hash, so the proof is good for this
 *  one request under this one token. */
async function proof(key: JsonWebKey, method: string, url: string, token: string) {
  const target = new URL(url);
  target.search = "";
  target.hash = "";
  const header = { alg: "ES256", jwk: publicHalf(key), typ: "dpop+jwt" };
  const claims = {
    ath: await sha256(token),
    htm: method.toUpperCase(),
    htu: target.toString(),
    iat: Math.floor(Date.now() / 1000),
    jti: randomUUID(),
  };
  const signingInput = `${base64url(JSON.stringify(header))}.${base64url(JSON.stringify(claims))}`;
  const signer = await crypto.subtle.importKey("jwk", key, curve, false, ["sign"]);
  const signature = await crypto.subtle.sign(
    { hash: "SHA-256", name: "ECDSA" },
    signer,
    new TextEncoder().encode(signingInput)
  );
  return `${signingInput}.${base64url(signature)}`;
}

async function send(
  keyFile: string,
  tokenFile: string,
  method: string,
  url: string,
  out: string,
  body: string | undefined
) {
  // The file is mint's own export of the key, and importKey refuses any JWK that is not one.
  const key = JSON.parse(readFileSync(keyFile, "utf8")) as JsonWebKey;
  const token = readFileSync(tokenFile, "utf8").trim();
  const response = await fetch(url, {
    body,
    headers: {
      Authorization: `DPoP ${token}`,
      DPoP: await proof(key, method, url, token),
      ...(body === undefined ? {} : { "Content-Type": "application/json" }),
    },
    method,
  });
  writeFileSync(out, new Uint8Array(await response.arrayBuffer()));
  process.stdout.write(String(response.status));
}

const [command, ...args] = process.argv.slice(2);
if (command === "mint" && args.length === 4) {
  await mint(args[0], args[1], args[2], args[3]);
} else if (command === "send" && (args.length === 5 || args.length === 6)) {
  await send(args[0], args[1], args[2], args[3], args[4], args[5]);
} else {
  process.stderr.write(
    "usage: broker-session.ts mint KEY SESSION PERSON PROJECT\n" +
      "       broker-session.ts send KEY TOKEN METHOD URL OUT [JSON]\n"
  );
  process.exit(2);
}
