/**
 * The Dispatch layer the ask census and the check runs census share: Dispatch resolved as every
 * command resolves it, one read that goes around `DispatchClient`, every event of an issue, the
 * pool that reads issues `ISSUE_CONCURRENCY` at a time, and the read of a time.
 */

import type { DispatchEvent } from "../../packages/contracts/src/dispatch-api";
import type { ActiveDispatchConfig } from "../../packages/envoy-client/src/dispatch-config";
import {
  DISPATCH_TOOL_DEADLINE_MS,
  type DispatchClient,
} from "../../packages/envoy-client/src/dispatch-http";
import {
  sessionConfigFile,
  sessionDispatchSettings,
} from "../../packages/envoy-client/src/session-settings";

export type DispatchConfig = Pick<ActiveDispatchConfig, "url" | "credential">;

/** Issues read at once: a few requests' latency each, well inside one request's or one client's
 * deadline, while the run as a whole has no deadline at all. */
export const ISSUE_CONCURRENCY = 8;

const EVENT_PAGE_SIZE = 200;

/** How much of a refused read's body its error names: enough for Dispatch's own error, and one
 * line of a report for a page of HTML. */
const ERROR_BODY_CHARACTERS = 200;

/**
 * Dispatch as every command resolves it: `sessionDispatchSettings` over the envoy.json the harness
 * `command` reads. A checkout's OpenCode-era file, which is not read, is named on stderr on every
 * run while it is there. Dispatch that is off throws `Dispatch is not configured: <notice>`, or
 * `dispatch config: <error>`.
 */
export async function dispatchConfig(): Promise<DispatchConfig> {
  const { dispatch: resolution, warnings } = await sessionDispatchSettings(
    sessionConfigFile("command", process.env),
    process.env,
    { cwd: process.cwd() }
  );
  for (const warning of warnings) console.error(`dispatch: ${warning}`);
  if (!resolution.enabled) {
    throw new Error(
      resolution.error === null
        ? `Dispatch is not configured: ${resolution.notice}`
        : `dispatch config: ${resolution.error}`
    );
  }
  return resolution;
}

/** One read of Dispatch that goes around DispatchClient, as JSON. An answer that is not 2xx throws
 * with its status and the first `ERROR_BODY_CHARACTERS` of its body. */
export async function get<T>(config: DispatchConfig, path: string): Promise<T> {
  const url = `${config.url}${path}`;
  // The bearer, or the access token this machine traded its own identity for, with the proof
  // that goes with it: this read goes around DispatchClient, so it presents the credential itself,
  // under the bound DispatchClient puts on each of its own requests.
  const deadline = AbortSignal.timeout(DISPATCH_TOOL_DEADLINE_MS);
  const response = await fetch(url, {
    headers: await config.credential.headers("GET", url, undefined, deadline),
    signal: deadline,
  });
  if (!response.ok) {
    const body = (await response.text()).slice(0, ERROR_BODY_CHARACTERS);
    throw new Error(`GET ${path}: ${response.status} ${body}`);
  }
  return (await response.json()) as T;
}

/** Every event of an issue, a page at a time. The client types an event by today's contract, and
 * the history a census reads predates parts of it, so each event is read as the caller's own
 * shape, whose fields it checks before use. */
export async function fetchIssueEvents<T extends Pick<DispatchEvent, "seq">>(
  client: DispatchClient,
  issueKey: string
): Promise<T[]> {
  const events: T[] = [];
  let after = 0;
  while (true) {
    const page = (await client.getIssueEvents(issueKey, after, EVENT_PAGE_SIZE)) as unknown as T[];
    events.push(...page);
    if (page.length < EVENT_PAGE_SIZE) return events;
    const last = page.at(-1);
    if (last === undefined || last.seq <= after) {
      throw new Error(`GET /api/v1/issues/${issueKey}/events returned no seq after ${after}`);
    }
    after = last.seq;
  }
}

/** Reads every key, `ISSUE_CONCURRENCY` at a time, in the order given, and answers each read at
 * its key's place. */
export async function eachIssue<T>(
  keys: readonly string[],
  read: (key: string) => Promise<T>
): Promise<T[]> {
  const answers: T[] = [];
  let next = 0;
  const reader = async (): Promise<void> => {
    while (next < keys.length) {
      const index = next++;
      answers[index] = await read(keys[index] as string);
    }
  };
  await Promise.all(Array.from({ length: Math.min(ISSUE_CONCURRENCY, keys.length) }, reader));
  return answers;
}

/** A time, in milliseconds. A text that is no time throws, naming what held it. */
export function date(value: string, name: string): number {
  const parsed = Date.parse(value);
  if (Number.isNaN(parsed)) throw new Error(`${name} must be an ISO timestamp: ${value}`);
  return parsed;
}
