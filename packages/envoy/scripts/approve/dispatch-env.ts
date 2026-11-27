// packages/envoy/scripts/approve/dispatch-env.ts
//
// The Dispatch that approve-check.sh's session (session.ts) and its scan (scan.ts) both reach, and
// the settings and the reading of an answer they share, kept once so a change to which Dispatch
// the harness talks to reaches the session's steps and the scan's diagnostic alike.
//
// Dispatch serves under https://<the approve host> (DISPATCH_URL), the public URL a request is
// signed for. On the local stack Dispatch also listens on a loopback address
// (APPROVE_CHECK_DISPATCH_LOCAL), where a direct request goes; with it unset the run is on a box,
// and a request goes to the public URL itself.
//
// It imports nothing from envoy-client, so the scan, which loads it, still loads no part of
// envoy-client, and the envoy-go-checks gate keeps its narrow read.

import { basename } from "node:path";

export const env = process.env as Record<string, string | undefined>;

/** The name a usage error starts with: `approve-session` for session.ts, `approve-scan` for
 *  scan.ts, from the file Bun runs. */
const program = `approve-${basename(process.argv[1] ?? "approve", ".ts")}`;

/** A usage error: the message on stderr after the program's name, and exit 2. */
export function fail(message: string): never {
  process.stderr.write(`${program}: ${message}\n`);
  process.exit(2);
}

/** The setting `name`, trimmed, or a usage error where it is unset or blank. */
export function setting(name: string): string {
  const value = (env[name] ?? "").trim();
  if (value === "") fail(`${name} is unset`);
  return value;
}

/** Dispatch's public URL, with no trailing slash. */
export const PUBLIC = setting("DISPATCH_URL").replace(/\/+$/, "");
/** Dispatch's loopback address on the local stack, with no trailing slash; "" on a box. */
export const LOCAL = (env["APPROVE_CHECK_DISPATCH_LOCAL"] ?? "").trim().replace(/\/+$/, "");

/** Where a direct request of `url` goes: a URL under the public one goes to the loopback address
 *  on the local stack, and every other URL, and every URL on a box, as it is. */
export function directURL(url: string): string {
  return LOCAL === "" || !url.startsWith(PUBLIC) ? url : LOCAL + url.slice(PUBLIC.length);
}

/** One answer's body: its text, and its members where it is a JSON object (none otherwise). */
export async function answerBody(
  answer: Response
): Promise<{ text: string; record: Record<string, unknown> }> {
  const text = await answer.text();
  let body: unknown;
  try {
    body = JSON.parse(text);
  } catch {
    body = undefined;
  }
  return {
    text,
    record: (typeof body === "object" && body !== null ? body : {}) as Record<string, unknown>,
  };
}
