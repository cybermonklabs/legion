#!/usr/bin/env bun
/**
 * bus/token-projects.ts <session> trades this pod's identity for one of the broker's access tokens
 * naming <session>, as every session client does (envoy-client's brokerTokenFor, under the
 * AGENT_SECRETS_* variables the caller sets: AGENT_SECRETS_URL, AGENT_SECRETS_MACHINE_IDENTITY and
 * AGENT_SECRETS_MACHINE_TOKEN_FILE), and asks the broker what it knows of that token at its RFC
 * 7662 introspection route, as the listener asks it, with the platform token in
 * BUS_INTROSPECT_TOKEN_FILE (an account BROKER_API_ACCOUNTS lists as `listener`). It prints the
 * answer's session and projects as one JSON object, {"session": "...", "projects": [...]}, and
 * exits 1, naming why, when the trade or the introspection fails or the token is not active. No
 * token is printed.
 */
import { readFileSync } from "node:fs";
import { z } from "zod";
import { brokerTokenFor } from "../../../../envoy-client/src/broker-credential";

const Introspection = z.looseObject({
  active: z.boolean(),
  session: z.string().optional(),
  projects: z.array(z.string()).optional(),
});

function refuse(message: string): never {
  console.error(`token-projects: ${message}`);
  process.exit(1);
}

const session = process.argv[2];
if (session === undefined || process.argv.length !== 3) {
  console.error("usage: bun token-projects.ts <session>");
  process.exit(2);
}
const broker = (process.env.AGENT_SECRETS_URL ?? "").replace(/\/+$/, "");
const introspector = process.env.BUS_INTROSPECT_TOKEN_FILE ?? "";
if (broker === "" || introspector === "") {
  refuse("AGENT_SECRETS_URL and BUS_INTROSPECT_TOKEN_FILE are required");
}

let accessToken: string;
try {
  const traded = await brokerTokenFor(process.env, session)(AbortSignal.timeout(20_000));
  accessToken = traded.token.accessToken;
} catch (error) {
  refuse(
    `the trade for ${session} failed: ${error instanceof Error ? error.message : String(error)}`
  );
}

const read = await fetch(`${broker}/v1/oauth/introspect`, {
  method: "POST",
  headers: {
    authorization: `Bearer ${readFileSync(introspector, "utf8").trim()}`,
    "content-type": "application/x-www-form-urlencoded",
  },
  body: new URLSearchParams({ token: accessToken }),
  signal: AbortSignal.timeout(10_000),
});
const text = await read.text();
if (read.status !== 200) refuse(`the introspection answered ${read.status}: ${text.slice(0, 300)}`);
const parsed = Introspection.safeParse(JSON.parse(text));
if (!parsed.success) refuse(`the introspection answered no introspection: ${text.slice(0, 300)}`);
if (!parsed.data.active) refuse(`the broker reads ${session}'s token as not active`);
console.log(
  JSON.stringify({ session: parsed.data.session ?? "", projects: parsed.data.projects ?? [] })
);
process.exit(0);
