#!/usr/bin/env bun
/**
 * bus/callout-keys.ts <dir> makes the three keys a hub with an auth callout and its auth service
 * share, as testnats.MakeCalloutKeys makes them: the service's own NATS user (a user key), the
 * issuer that signs every answer (an account key) and the curve key the hub seals each request to.
 * It writes their seeds to service.seed, issuer.seed and xkey.seed in <dir>, each readable by its
 * owner alone (the names internal/testnats/cmd/natsd -callout-keys writes and the broker's
 * BROKER_NATS_*_SEED_FILE read), and prints the three public keys as one JSON object,
 * {"service": "U…", "issuer": "A…", "xkey": "X…"}, which the hub's authorization file names. No
 * seed is printed.
 */
import { chmodSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { nkeys } from "nats";

const dir = process.argv[2];
if (dir === undefined || process.argv.length !== 3) {
  console.error("usage: bun callout-keys.ts <dir>");
  process.exit(2);
}

const decoder = new TextDecoder();
const publics: Record<string, string> = {};
for (const [name, pair] of [
  ["service", nkeys.createUser()],
  ["issuer", nkeys.createAccount()],
  ["xkey", nkeys.createCurve()],
] as const) {
  const file = join(dir, `${name}.seed`);
  writeFileSync(file, decoder.decode(pair.getSeed()), { mode: 0o600 });
  chmodSync(file, 0o600);
  publics[name] = pair.getPublicKey();
}
console.log(JSON.stringify(publics));
