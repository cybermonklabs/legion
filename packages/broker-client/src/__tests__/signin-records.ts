// packages/broker-client/src/__tests__/signin-records.ts
//
// Sign-in records of the shape `agent-secrets login` keeps
// (`packages/envoy/internal/brokerclient/signinstore.go`, `SignInStore.Write`), each made from a
// box key the test makes at run time, so the repository holds no private key.

import { createHash, generateKeyPairSync } from "node:crypto";
import { chmod, mkdir, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { calculateJwkThumbprint, type JWK } from "jose";
import type { SignInRecord } from "../signins";

/**
 * The record `agent-secrets login` keeps for `person`, with a box key of its own: a P-256 key
 * written as PKCS#8 PEM, as Go's `x509.MarshalPKCS8PrivateKey` writes one, and that key's RFC
 * 7638 thumbprint. `overrides` replaces any member.
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
    thumbprint: await calculateJwkThumbprint(publicKey.export({ format: "jwk" }) as JWK),
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
 * is empty (`omitempty`). Answers the file written.
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
