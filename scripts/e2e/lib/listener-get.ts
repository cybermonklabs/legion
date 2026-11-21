// scripts/e2e/lib/listener-get.ts
//
// One GET of an Envoy listener that takes no shared token, presented as every session's client
// presents itself: with this machine's own broker token under DPoP, a fresh proof of the token's
// key naming the request's method and URL (packages/envoy-client's brokerCredential, which trades
// the identity the AGENT_SECRETS_* environment names at the broker through packages/cipher-client's
// token client: the operator's `agent-secrets login` on a devbox, or a pod's projected token). A
// stage proof's own reads of a deployment's listener go through it, where curl, which can sign no
// proof, used to send the listener's shared bearer (stage4b-sandbox-tree.sh's interests sampler and
// its controller-role lookup).
//
//   bun scripts/e2e/lib/listener-get.ts --out FILE [--max-time SECONDS] URL
//
// It prints the listener's HTTP status on stdout, as `curl -w '%{http_code}'` does, writes the
// answer's body to FILE (0600), follows no redirect, and exits 0 for any answer. It exits 1, saying
// why on stderr, when no answer came or the broker token could not be made, and 2 when this
// machine has no broker route at all (brokerRouteConfigured's gap), sending nothing. A line it
// prints names the request's path, never its host, which names the operator's deployment, and
// never a token.

import { writeFileSync } from "node:fs";
import {
  brokerCredential,
  brokerRouteConfigured,
  forgetBrokerClients,
} from "../../../packages/envoy-client/src/broker-credential";
import { messageFor } from "../../../packages/envoy-client/src/errors";

function usage(why: string): never {
  process.stderr.write(
    `listener-get: ${why}\nusage: bun listener-get.ts --out FILE [--max-time SECONDS] URL\n`
  );
  process.exit(2);
}

let out = "";
let maxTime = 20;
let url = "";
const args = process.argv.slice(2);
while (args.length > 0) {
  const arg = args.shift() ?? "";
  if (arg === "--out") out = args.shift() ?? usage("--out needs a file");
  else if (arg === "--max-time") {
    maxTime = Number(args.shift());
    if (!Number.isInteger(maxTime) || maxTime < 1)
      usage("--max-time needs a whole number of seconds >= 1");
  } else if (url === "" && !arg.startsWith("--")) url = arg;
  else usage(`unexpected argument ${arg.startsWith("--") ? arg : "(a second URL)"}`);
}
if (out === "") usage("--out is required");
let target: URL;
try {
  target = new URL(url);
} catch {
  usage("the URL is not an absolute http(s) URL");
}
if (target.protocol !== "http:" && target.protocol !== "https:")
  usage("the URL is not an absolute http(s) URL");
const path = target.pathname;

const route = brokerRouteConfigured(process.env);
if (!route.traded) {
  process.stderr.write(
    `listener-get: GET ${path}: this machine has no broker token to present, since ${route.gap}\n`
  );
  process.exit(2);
}

/** Ends the run with code, dropping the broker client so no renewal holds the process open. */
function finish(code: number): never {
  forgetBrokerClients();
  process.exit(code);
}

const signal = AbortSignal.timeout(maxTime * 1000);
let headers: Readonly<Record<string, string>>;
try {
  const made = brokerCredential(process.env).headers("GET", url, undefined, signal);
  headers = made instanceof Promise ? await made : made;
} catch (error) {
  process.stderr.write(
    `listener-get: GET ${path}: the broker token could not be made: ${messageFor(error)}\n`
  );
  finish(1);
}
let response: Response;
let body: string;
try {
  response = await fetch(url, { headers, signal, redirect: "manual" });
  body = await response.text();
} catch (error) {
  // The runtime's own message repeats the host it dialed, so the failure is named by its class.
  const kind = error instanceof Error ? error.name : typeof error;
  process.stderr.write(`listener-get: GET ${path} got no answer: ${kind}\n`);
  finish(1);
}
writeFileSync(out, body, { mode: 0o600 });
process.stdout.write(`${response.status}\n`);
finish(0);
