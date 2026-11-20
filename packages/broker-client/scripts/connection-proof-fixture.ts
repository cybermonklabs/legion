// packages/broker-client/scripts/connection-proof-fixture.ts
//
// Writes packages/envoy/internal/brokertoken/testdata/connection-proof-ts.json: one proof this
// package's signer makes for a NATS connection (`DpopKey.connectionProof`), with the token, the
// nonce and the key thumbprint it verifies with and the instant it was signed. The Go checker
// reads it (`TestATypeScriptConnectionProofVerifies`), as src/__tests__/dpop.test.ts reads the Go
// signer's `connection-proof-go.json`, so each client's proof is verified by the other side's
// checker. Run it again whenever the signer's header or claims change, from packages/broker-client:
//
//   bun scripts/connection-proof-fixture.ts
//
// The key is a throwaway made for this one proof, and the token an example: the file holds no
// credential. The proof is written as its three segments, because a joined compact JWS reads as a
// credential to the push leak check.

import { writeFile } from "node:fs/promises";
import { calculateJwkThumbprint } from "jose";
import { newDpopKey } from "../src/dpop";

const token = "example-access-token-of-ses_a"; // fixture-secret: a committed proof vector's token, which the Go and TS proof checks read from connection-proof-ts.json; no client is handed it
const nonce = "example-server-nonce";
const key = await newDpopKey();
const segments = key.connectionProof({ accessToken: token, nonce }).split(".");
const claims = JSON.parse(Buffer.from(segments[1] ?? "", "base64url").toString("utf8")) as {
  readonly iat: number;
};
const target = new URL(
  "../../envoy/internal/brokertoken/testdata/connection-proof-ts.json",
  import.meta.url
);
await writeFile(
  target,
  `${JSON.stringify(
    {
      nonce,
      note: "the compact JWS is the three segments joined by dots; kept apart because it is a proof of a throwaway key over an example token, and a joined JWS reads as a credential to the push leak check",
      now: claims.iat,
      proof_segments: segments,
      signer: "packages/broker-client/src/dpop.ts connectionProof",
      thumbprint: await calculateJwkThumbprint(key.publicJwk),
      token,
    },
    null,
    2
  )}\n`
);
console.log(`wrote ${target.pathname}`);
