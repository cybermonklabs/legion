import { readFile } from "node:fs/promises";
import { setTimeout as sleep } from "node:timers/promises";

/**
 * The read-only Dispatch client the approval-eval scripts share.
 *
 * Every call here is a GET: the client sends no other method, so no script built on it can write
 * to Dispatch. The server is the `DISPATCH_URL` of the moment of the call and the bearer is the
 * trimmed contents of the file `DISPATCH_TOKEN_FILE` names, read at that same moment, so neither
 * a host nor a credential is ever written into this repository and a token rolled between two
 * calls is picked up by the second.
 *
 * One retry policy covers every read: a transport error or a 5xx is tried again, four times, with
 * a growing wait, and a 4xx is thrown at once, since the route or the item is not there and
 * another try would not help.
 */

/** The growing waits between the tries of one read: four retries after the first try. */
const RETRY_WAITS_MS: readonly number[] = [1_000, 2_000, 4_000, 8_000];
/** How many reads `readMany` runs at once when its caller names no other bound. */
const DEFAULT_CONCURRENCY = 8;
/** The page the event log is asked for, which is that route's own maximum. */
const EVENT_PAGE_SIZE = 1000;
/** How much of a refused read's body is kept: enough to name the refusal, never a whole page. */
const REFUSAL_BODY_CHARS = 500;

/** The server and the bearer, as one call read them from the environment. */
export interface DispatchConfig {
  baseUrl: string;
  token: string;
}

/** The environment does not name the server or the token: nothing can be read. */
export class DispatchConfigError extends Error {
  override readonly name = "DispatchConfigError";
}

/** A 4xx: the route or the item is not there, and another try would not help. */
export class DispatchRefusedError extends Error {
  override readonly name = "DispatchRefusedError";
  readonly status: number;
  readonly body: string;

  constructor(path: string, status: number, body: string) {
    super(`GET ${path}: ${status} ${body}`);
    this.status = status;
    this.body = body;
  }
}

/** Every try of one read failed on the transport or on a 5xx. */
export class DispatchUnreachableError extends Error {
  override readonly name = "DispatchUnreachableError";

  constructor(path: string, tries: number, lastFailure: string) {
    super(`GET ${path}: ${tries} tries, last failure ${lastFailure}`);
  }
}

/** Who an event names as its own author. A person's answer is an actor whose kind is `user`. */
export interface DispatchActor {
  kind: string;
  id: string;
  owner?: string | null;
}

/** One row of `/api/v1/events/log`, whose payload differs by `type` and is read by the caller. */
export interface DispatchEvent {
  id: number;
  issue_key: string | null;
  project: string | null;
  seq: number;
  type: string;
  actor: DispatchActor | null;
  created_at: string;
  payload: Record<string, unknown>;
}

/** What `pageEventLog` is asked to walk, and what it hands each page to. */
export interface PageEventLogOptions {
  /**
   * The last event id of the walk. The walk ends with it, and an event above it, with everything
   * after it, is dropped. With none, the walk runs to the end of the log.
   */
  through?: number;
  /** Called with each page, oldest event first; the walk waits for it. */
  onPage: (events: readonly DispatchEvent[]) => void | Promise<void>;
}

/** What one walk of the event log saw. */
export interface EventLogWalk {
  /** The id of the last event handed to `onPage`, or 0 when the walk saw none. */
  lastEventId: number;
  /** How many events were handed to `onPage`. */
  eventsRead: number;
  /** How many pages carried at least one of them. */
  pages: number;
}

/**
 * The server and the bearer, read now. Throws `DispatchConfigError` when the environment names
 * neither, or when the file it names holds nothing.
 */
export async function dispatchConfig(): Promise<DispatchConfig> {
  const baseUrl = process.env.DISPATCH_URL?.trim();
  if (!baseUrl) {
    throw new DispatchConfigError("DISPATCH_URL names the Dispatch server, and it is not set");
  }
  const tokenFile = process.env.DISPATCH_TOKEN_FILE?.trim();
  if (!tokenFile) {
    throw new DispatchConfigError(
      "DISPATCH_TOKEN_FILE names the file holding the bearer token, and it is not set"
    );
  }
  let token: string;
  try {
    token = (await readFile(tokenFile, "utf8")).trim();
  } catch {
    throw new DispatchConfigError("the file DISPATCH_TOKEN_FILE names could not be read");
  }
  if (token === "") {
    throw new DispatchConfigError("the file DISPATCH_TOKEN_FILE names holds no token");
  }
  return { baseUrl: baseUrl.replace(/\/+$/, ""), token };
}

/**
 * GET one path of the Dispatch API and decode its JSON. `config` is read from the environment
 * when the caller hands none, which is what a single read does.
 */
export async function readJson<T>(path: string, config?: DispatchConfig): Promise<T> {
  const resolved = config ?? (await dispatchConfig());
  const url = `${resolved.baseUrl}${path}`;
  let lastFailure = "no try was made";
  for (let attempt = 0; ; attempt += 1) {
    try {
      const answer = await fetch(url, {
        method: "GET",
        headers: { authorization: `Bearer ${resolved.token}` },
      });
      if (answer.status >= 400 && answer.status < 500) {
        const body = (await answer.text()).slice(0, REFUSAL_BODY_CHARS);
        throw new DispatchRefusedError(path, answer.status, body);
      }
      if (answer.ok) return (await answer.json()) as T;
      lastFailure = `${answer.status} ${(await answer.text()).slice(0, REFUSAL_BODY_CHARS)}`;
    } catch (error) {
      if (error instanceof DispatchRefusedError) throw error;
      lastFailure = error instanceof Error ? error.message : String(error);
    }
    const wait = RETRY_WAITS_MS[attempt];
    if (wait === undefined) {
      throw new DispatchUnreachableError(path, RETRY_WAITS_MS.length + 1, lastFailure);
    }
    await sleep(wait);
  }
}

/**
 * Read several paths, at most `concurrency` of them at once, and hand the answers back in the
 * order of the paths. One refusal or one unreachable path ends the whole read.
 */
export async function readMany<T>(
  paths: readonly string[],
  concurrency: number = DEFAULT_CONCURRENCY
): Promise<T[]> {
  if (paths.length === 0) return [];
  const config = await dispatchConfig();
  const answers = new Array<T>(paths.length);
  const bound = Math.min(Math.max(1, Math.trunc(concurrency)), paths.length);
  let next = 0;
  const worker = async (): Promise<void> => {
    for (let index = next++; index < paths.length; index = next++) {
      const path = paths[index];
      if (path === undefined) return;
      answers[index] = await readJson<T>(path, config);
    }
  };
  await Promise.all(Array.from({ length: bound }, worker));
  return answers;
}

/**
 * Walk `/api/v1/events/log` from the start, a page of `EVENT_PAGE_SIZE` at a time, handing each
 * page to `onPage` oldest event first. The walk ends at the first empty page, or at the first
 * event whose id is above `through`: that event, and everything after it, is dropped, so two
 * walks pinned to the same id read the same events however much the log grew between them.
 */
export async function pageEventLog(options: PageEventLogOptions): Promise<EventLogWalk> {
  const { through, onPage } = options;
  const config = await dispatchConfig();
  let after = 0;
  let eventsRead = 0;
  let pages = 0;
  let lastEventId = 0;
  let more = true;
  while (more) {
    const path = `/api/v1/events/log?after=${after}&limit=${EVENT_PAGE_SIZE}`;
    const page = await readJson<DispatchEvent[]>(path, config);
    if (page.length === 0) break;
    let kept = page;
    if (through !== undefined) {
      const above = page.findIndex((event) => event.id > through);
      if (above >= 0) {
        kept = page.slice(0, above);
        more = false;
      }
    }
    const last = kept[kept.length - 1];
    if (last !== undefined) {
      pages += 1;
      eventsRead += kept.length;
      lastEventId = last.id;
      after = last.id;
      await onPage(kept);
    }
    if (through !== undefined && after >= through) more = false;
  }
  return { lastEventId, eventsRead, pages };
}
