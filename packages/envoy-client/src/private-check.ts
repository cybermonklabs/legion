import { createHash, randomUUID } from "node:crypto";
import { mkdir, readFile, rename, rm, writeFile } from "node:fs/promises";
import { dirname, isAbsolute, join } from "node:path";
import { setTimeout as pauseFor } from "node:timers/promises";

/**
 * The check that keeps a private project's answers from a bearer that names one, wherever this
 * process sends a request to Dispatch: every request `DispatchClient` sends, when it is made by
 * `checkedDispatchClient` (dispatch-http.ts), and one request a caller sends itself through
 * `checkedDispatchFetch`.
 *
 * Every response Dispatch's handlers write says whether any project may be private, in
 * `Dispatch-Private-Projects: 0|1`. A response without it is no answer of Dispatch's: a proxy's
 * 502, a hop's 401, a malformed request Go's server refused before any handler ran.
 *
 * Only a response that carries the header changes the shared state, which is kept per Dispatch URL
 * in memory and in `${XDG_CACHE_HOME:-$HOME/.cache}/agent-private/state/<sha256 of the URL>`, so
 * every process of the user reads it. A `1` holds with no expiry until Dispatch's own `0` has held
 * for longer than `ZERO_HOLDS_MS`, so a stale replica or a revert cannot turn the check off under
 * a private project. A `0` is trusted for `ZERO_TRUSTED_MS` after the last response that carried
 * it; after that, and before any response, the state is unknown.
 *
 * Each response is judged by its own header first. `0` is returned as it came. `1` holds it while
 * the check runs. A response with no header is judged by the state: a trusted `0` returns it, and
 * under `1` or unknown a 2xx is held while the check runs and a non-2xx, which holds no object of
 * Dispatch's, is returned as it came. A write's response is held exactly as a read's is.
 *
 * The check asks `GET /api/v1/whoami` with the request's own bearer, a personal `dsp_` token, and
 * remembers the answer per bearer, by its SHA-256, in this process alone: a refusal for
 * `REFUSED_FOR_MS`, an admission for `ADMITTED_FOR_MS`. A whoami naming a project in
 * `private_projects` refuses the bearer (`PrivateProjectRefused`) and the held response is dropped
 * unread. A whoami answered 401 or 403 is the request's own answer. A whoami that gets no answer
 * through the retry policy the check was given fails closed (`PrivateDispatchUnanswered`). A
 * broker credential (`DPoP`) is admitted without whoami, since the broker mints a token naming a
 * private project only for that project's own pods, and a request with no credential makes no
 * whoami. No variable and no local file admits a bearer.
 *
 * Before sending, with the state `1`, a write with a bearer waits for the check, so nothing is
 * written with a refused bearer, and a read runs the check beside its request. With the state `0`
 * or unknown nothing is sent first: with no project private, the requests are today's.
 *
 * It imports Node's built-ins alone, so a file that loads it gains this one file: the approval
 * eval's pinned decision (`RESERVE_ASK_CLOSURE`, scripts/approval-eval/s10-decision.ts) names
 * every file it loads by its sha256.
 */

/** The header every response Dispatch's handlers write carries: `1` when the deployment names a
 *  project that may be private or the database holds a private project, `0` otherwise
 *  (`access.PrivateProjectsHeader`, packages/envoy/internal/dispatch/access/header.go). */
export const PRIVATE_PROJECTS_HEADER = "Dispatch-Private-Projects";

const MINUTE_MS = 60_000;

/** How long Dispatch's own `0` must hold after a `1` before the state reads `0` again. */
export const ZERO_HOLDS_MS = 6 * MINUTE_MS;

/** How long a `0` is trusted after the last response that carried it. */
export const ZERO_TRUSTED_MS = 5 * MINUTE_MS;

/** How long a whoami that names a private project refuses its bearer without asking again. */
export const REFUSED_FOR_MS = 24 * 60 * MINUTE_MS;

/** How long a whoami that names no private project admits its bearer without asking again. */
export const ADMITTED_FOR_MS = 5 * MINUTE_MS;

/** The least advance of a trusted `0` that is written to the shared file on its own, so a busy
 *  process does not write the file once per response. A transition is written at once. */
const ZERO_WRITTEN_EVERY_MS = MINUTE_MS;

/** The longest a whoami waits for a caller that gives the check no signal of its own: the
 *  Dispatch client's own deadline (`DISPATCH_TOOL_DEADLINE_MS`, dispatch-http.ts). */
const WHOAMI_DEADLINE_MS = MINUTE_MS;

/** A bearer naming a private project, refused before anything Dispatch answered reaches the
 *  caller. The `dispatch` command exits 2 on it, and a tool's call fails with it. */
export class PrivateProjectRefused extends Error {
  override readonly name = "PrivateProjectRefused";
  readonly code = "private_project_refused";

  constructor(readonly keys: readonly string[]) {
    super(
      keys.length === 1
        ? `private_project_refused: ${keys[0]} is private; its sessions run only in its own cluster namespace`
        : `private_project_refused: ${keys.join(", ")} are private; their sessions run only in their own cluster namespaces`
    );
  }
}

/** A check whose whoami got no answer it could read, through the retry policy it was given: the
 *  held response is dropped, and asking again later may get through. */
export class PrivateDispatchUnanswered extends Error {
  override readonly name = "PrivateDispatchUnanswered";
  readonly code = "private_dispatch_unanswered";

  constructor(why: string) {
    super(`Dispatch did not answer; retry. ${why}`);
  }
}

/** Whether `error` is the check's own: a refusal, or a whoami with no answer. Either ends the
 *  request it was run for, whatever retry policy that request has. */
export function isPrivateCheckFailure(
  error: unknown
): error is PrivateProjectRefused | PrivateDispatchUnanswered {
  return error instanceof PrivateProjectRefused || error instanceof PrivateDispatchUnanswered;
}

/** The one call shape the check makes and `checkedDispatchFetch` answers: a URL and its request.
 *  The global `fetch` is one. */
export type DispatchFetch = (input: string, init: RequestInit) => Promise<Response>;

/** How a whoami that got no answer is asked again: after each of `pausesMs` in turn, none
 *  starting more than `withinMs` after the first ask. `DispatchClient` gives the check its own
 *  write policy (`DispatchWriteRetry`); `pause` and `now` are the timer and the monotonic clock,
 *  which a test replaces. */
export interface PrivateCheckRetry {
  readonly pausesMs: readonly number[];
  readonly withinMs: number;
  readonly pause?: ((ms: number, signal: AbortSignal) => Promise<void>) | undefined;
  readonly now?: (() => number) | undefined;
}

/** One ask and no retry: a caller that sends its own request has its own retry around it. */
const ASK_ONCE: PrivateCheckRetry = { pausesMs: [], withinMs: 0 };

/** What a check is made with beside its Dispatch URL and its fetch. */
export interface PrivateCheckSettings {
  /** Where the shared state's directory is read from (`XDG_CACHE_HOME`, else `HOME`):
   *  `process.env` where absent. With neither set, the state is this process's alone. */
  readonly env?: Readonly<Record<string, string | undefined>> | undefined;
  /** The wall clock the state's times are kept on, shared by the user's processes. */
  readonly now?: (() => number) | undefined;
  readonly retry?: PrivateCheckRetry | undefined;
  /** Where the one warning per process for a Dispatch with no whoami goes. */
  readonly warn?: ((line: string) => void) | undefined;
}

/** What one Dispatch URL's responses have said, on the wall clock: when the latest `1` came,
 *  when the first `0` after it came, and when the latest `0` came. */
interface Seen {
  oneAt?: number | undefined;
  zeroSince?: number | undefined;
  zeroAt?: number | undefined;
}

type State = "1" | "0" | "unknown";

/** A bearer's whoami as remembered: the private keys it names (none for an admission), until
 *  when, and whether it was a Dispatch's 404 to a whoami it does not serve, which admits only
 *  while the state is not `1`. */
interface Verdict {
  readonly until: number;
  readonly refused: readonly string[] | undefined;
  readonly noWhoami: boolean;
}

/** Whoami's own 401 or 403, which is the request's answer: Dispatch does not take the bearer. */
interface WhoamiRefusal {
  readonly answer: Response;
}

/** The outcome of a check that did not throw: the bearer is admitted, or whoami refused it. */
type Outcome = { readonly admitted: true } | WhoamiRefusal;

const ADMITTED: Outcome = { admitted: true };

/** Every Dispatch URL's state this process holds, by its store (`stateFile`). */
const SEEN = new Map<string, Seen>();
/** The latest `zeroAt` each store read from its file or wrote to it. */
const ZERO_IN_FILE = new Map<string, number | undefined>();
/** Every remembered bearer, by its store, then by the SHA-256 of the bearer. */
const VERDICTS = new Map<string, Map<string, Verdict>>();
/** The whoamis in flight, by store and bearer, so concurrent requests ask once. */
const ASKING = new Map<string, Promise<Verdict | WhoamiRefusal>>();
/** The Dispatch URLs this process has warned about. */
const WARNED = new Set<string>();

function later(a: number | undefined, b: number | undefined): number | undefined {
  if (a === undefined) return b;
  if (b === undefined) return a;
  return Math.max(a, b);
}

/** Whether Dispatch's own `0` has held for longer than `ZERO_HOLDS_MS` since the latest `1`. */
function zeroHeld(seen: Seen): boolean {
  return (
    seen.oneAt !== undefined &&
    seen.zeroSince !== undefined &&
    seen.zeroAt !== undefined &&
    seen.zeroSince > seen.oneAt &&
    seen.zeroAt - seen.zeroSince > ZERO_HOLDS_MS
  );
}

function stateOf(seen: Seen, now: number): State {
  if (seen.oneAt !== undefined && !zeroHeld(seen)) return "1";
  if (seen.zeroAt !== undefined && now - seen.zeroAt <= ZERO_TRUSTED_MS) return "0";
  return "unknown";
}

/** `into` and `other` as one: the latest `1` and the latest `0`, and the first `0` either saw
 *  after that latest `1`. A `0` seen before a `1` the other saw starts no run. */
function merge(into: Seen, other: Seen): void {
  const oneAt = later(into.oneAt, other.oneAt);
  let zeroSince: number | undefined;
  if (oneAt !== undefined) {
    for (const since of [into.zeroSince, other.zeroSince]) {
      if (since !== undefined && since > oneAt && (zeroSince === undefined || since < zeroSince))
        zeroSince = since;
    }
  }
  into.oneAt = oneAt;
  into.zeroSince = zeroSince;
  into.zeroAt = later(into.zeroAt, other.zeroAt);
}

/** `header`, a response's `Dispatch-Private-Projects`, taken into `seen` at `now`. */
function take(seen: Seen, header: "0" | "1", now: number): void {
  if (header === "1") {
    seen.oneAt = later(seen.oneAt, now);
    if (seen.zeroSince !== undefined && seen.oneAt !== undefined && seen.zeroSince <= seen.oneAt)
      seen.zeroSince = undefined;
    return;
  }
  seen.zeroAt = later(seen.zeroAt, now);
  if (seen.oneAt !== undefined && (seen.zeroSince === undefined || seen.zeroSince <= seen.oneAt)) {
    seen.zeroSince = now;
  }
}

function finite(value: unknown): number | undefined {
  return typeof value === "number" && Number.isFinite(value) ? value : undefined;
}

/** The shared file's text as a state; anything unreadable is no state at all. */
function parsedSeen(text: string): Seen {
  let value: unknown;
  try {
    value = JSON.parse(text);
  } catch {
    return {};
  }
  if (typeof value !== "object" || value === null) return {};
  const record = value as Record<string, unknown>;
  return {
    oneAt: finite(record["one_at"]),
    zeroSince: finite(record["zero_since"]),
    zeroAt: finite(record["zero_at"]),
  };
}

/** The shared file of the Dispatch at `url`, or undefined where the environment names no cache
 *  directory, which keeps the state in this process alone. */
function stateFile(
  url: string,
  env: Readonly<Record<string, string | undefined>>
): string | undefined {
  const xdg = env["XDG_CACHE_HOME"];
  const home = env["HOME"];
  const root =
    xdg !== undefined && xdg !== "" && isAbsolute(xdg)
      ? xdg
      : home !== undefined && home !== "" && isAbsolute(home)
        ? join(home, ".cache")
        : undefined;
  return root === undefined
    ? undefined
    : join(root, "agent-private", "state", createHash("sha256").update(url).digest("hex"));
}

/** The personal token a request presents (`Authorization: Bearer dsp_…`), or undefined for a
 *  request with no credential, a broker credential (`DPoP`) or any other bearer, none of which
 *  can list a project a person made private. */
function personalBearer(headers: RequestInit["headers"]): string | undefined {
  if (headers === undefined) return undefined;
  const value = new Headers(headers).get("Authorization")?.trim();
  if (value === undefined) return undefined;
  const match = /^bearer\s+(dsp_\S+)$/i.exec(value);
  return match?.[1];
}

/** A held response dropped unread. */
async function drop(response: Response): Promise<void> {
  try {
    await response.body?.cancel();
  } catch {
    // Already read or locked: nothing of it reaches the caller either way.
  }
}

/** The private keys a whoami's JSON names, an empty list for none, or undefined for a body that
 *  is no whoami answer. `private_projects` is left out where none is private (KF4). */
function privateKeysOf(value: unknown): readonly string[] | undefined {
  if (typeof value !== "object" || value === null || Array.isArray(value)) return undefined;
  const keys = (value as Record<string, unknown>)["private_projects"];
  if (keys === undefined || keys === null) return [];
  if (!Array.isArray(keys) || !keys.every((key) => typeof key === "string" && key !== ""))
    return undefined;
  return keys as string[];
}

/**
 * The check for the Dispatch at one URL. Its state and its bearers' answers are shared by every
 * check of that URL in this process, and its state by the user's other processes too.
 */
export class PrivateCheck {
  readonly #url: string;
  readonly #shown: string;
  readonly #file: string | undefined;
  readonly #store: string;
  readonly #fetch: DispatchFetch;
  readonly #now: () => number;
  readonly #retry: PrivateCheckRetry;
  readonly #warn: (line: string) => void;

  constructor(baseUrl: string, fetchImpl: DispatchFetch, settings: PrivateCheckSettings = {}) {
    this.#url = baseUrl.replace(/\/+$/, "");
    // The URL as a line may name it: its origin and path, never its user info or query.
    let shown = "the configured Dispatch URL";
    try {
      const parsed = new URL(this.#url);
      shown = `${parsed.origin}${parsed.pathname.replace(/\/+$/, "")}`;
    } catch {
      // Not a URL: named by the setting alone.
    }
    this.#shown = shown;
    this.#file = stateFile(this.#url, settings.env ?? process.env);
    this.#store = this.#file ?? `memory:${this.#url}`;
    this.#fetch = fetchImpl;
    this.#now = settings.now ?? Date.now;
    this.#retry = settings.retry ?? ASK_ONCE;
    // A line holds no secret: the URL as `#shown` names it, and a status or an error's class.
    this.#warn = settings.warn ?? ((line) => console.warn(line));
  }

  /**
   * One request: `send` sends it as `method` with `headers`, and its answer is returned once it
   * has been judged, or a refusal is thrown in its place. `signal` bounds the whoami, as it
   * bounds the request. An error `send` throws passes on as it came.
   */
  async request(
    method: string,
    headers: RequestInit["headers"],
    signal: AbortSignal,
    send: () => Promise<Response>
  ): Promise<Response> {
    const bearer = personalBearer(headers);
    let beside: Promise<Outcome> | undefined;
    if (bearer !== undefined && (await this.state()) === "1") {
      if (!["GET", "HEAD", "OPTIONS"].includes(method.toUpperCase())) {
        const outcome = await this.#check(bearer, signal);
        if ("answer" in outcome) return outcome.answer;
        const response = await send();
        await this.note(response);
        return response;
      }
      beside = this.#check(bearer, signal);
      // Read once the answer is in; a request that fails first leaves it unread.
      beside.catch(() => undefined);
    }
    const response = await send();
    const header = await this.note(response);
    if (beside !== undefined) return await this.#settle(response, beside);
    if (header === "0" || bearer === undefined) return response;
    if (header === undefined) {
      if (response.status < 200 || response.status > 299) return response;
      if ((await this.state()) === "0") return response;
    }
    return await this.#settle(response, this.#check(bearer, signal));
  }

  /**
   * The check of the personal token `headers` present, asked now whatever the state, for a
   * request whose answer this process cannot judge: a WebSocket's upgrade carries the header, and
   * Bun's WebSocket shows none of it. It throws the refusal, answers whoami's own 401 or 403, or
   * undefined once the token is admitted. Any other credential, and none, is admitted unasked.
   */
  async before(
    headers: RequestInit["headers"],
    signal: AbortSignal
  ): Promise<Response | undefined> {
    const bearer = personalBearer(headers);
    if (bearer === undefined) return undefined;
    const outcome = await this.#check(bearer, signal);
    return "answer" in outcome ? outcome.answer : undefined;
  }

  /** The shared state now: this process's, with the user's other processes' merged in. */
  async state(): Promise<State> {
    return stateOf(await this.#read(), this.#now());
  }

  /** Takes `response`'s header into the shared state and answers it, or undefined for a response
   *  without one (any value but `0` and `1` included), which changes nothing. */
  async note(response: Response): Promise<"0" | "1" | undefined> {
    const value = response.headers.get(PRIVATE_PROJECTS_HEADER)?.trim();
    if (value !== "0" && value !== "1") return undefined;
    const seen = await this.#read();
    const before = { ...seen };
    take(seen, value, this.#now());
    const moved = seen.oneAt !== before.oneAt || seen.zeroSince !== before.zeroSince;
    const inFile = ZERO_IN_FILE.get(this.#store);
    const zeroMoved =
      seen.zeroAt !== undefined &&
      (inFile === undefined || seen.zeroAt - inFile >= ZERO_WRITTEN_EVERY_MS);
    if (moved || zeroMoved) await this.#write(seen);
    return value;
  }

  /** The held `response`, or the check's refusal in its place, with `response` dropped. */
  async #settle(response: Response, check: Promise<Outcome>): Promise<Response> {
    let outcome: Outcome;
    try {
      outcome = await check;
    } catch (error) {
      await drop(response);
      throw error;
    }
    if ("admitted" in outcome) return response;
    await drop(response);
    return outcome.answer;
  }

  /** The check of `bearer`: its remembered answer where one is live, else one whoami at a time
   *  per bearer, whose answer every request waiting on it shares. */
  async #check(bearer: string, signal: AbortSignal): Promise<Outcome> {
    const digest = createHash("sha256").update(bearer).digest("hex");
    const remembered = await this.#remembered(digest);
    if (remembered !== undefined) return remembered;
    const key = `${this.#store}\n${digest}`;
    let asking = ASKING.get(key);
    if (asking === undefined) {
      asking = this.#ask(bearer, signal).finally(() => ASKING.delete(key));
      ASKING.set(key, asking);
    }
    const answer = await asking;
    // A whoami's own 401 or 403, which every request that waited on it answers with a copy.
    if (!("until" in answer)) return { answer: answer.answer.clone() as Response };
    let verdicts = VERDICTS.get(this.#store);
    if (verdicts === undefined) {
      verdicts = new Map();
      VERDICTS.set(this.#store, verdicts);
    }
    verdicts.set(digest, answer);
    return this.#outcomeOf(answer);
  }

  /** The live remembered answer for the bearer whose SHA-256 is `digest`, or undefined. */
  async #remembered(digest: string): Promise<Outcome | undefined> {
    const verdict = VERDICTS.get(this.#store)?.get(digest);
    if (verdict === undefined || verdict.until <= this.#now()) return undefined;
    if (verdict.noWhoami && (await this.state()) === "1") return undefined;
    return this.#outcomeOf(verdict);
  }

  #outcomeOf(verdict: Verdict): Outcome {
    if (verdict.refused !== undefined) throw new PrivateProjectRefused(verdict.refused);
    return ADMITTED;
  }

  /** `GET /api/v1/whoami` with `bearer`, asked again on the retry policy while it gets no
   *  answer. It answers a verdict to remember, or whoami's own 401 or 403. */
  async #ask(bearer: string, signal: AbortSignal): Promise<Verdict | WhoamiRefusal> {
    const { pausesMs, withinMs } = this.#retry;
    const pause =
      this.#retry.pause ??
      ((ms: number, stop: AbortSignal) =>
        pauseFor(ms, undefined, { signal: stop }).catch(() => {
          throw stop.reason;
        }));
    const clock = this.#retry.now ?? (() => performance.now());
    const started = clock();
    const request = `GET ${this.#shown}/api/v1/whoami`;
    for (let asked = 1; ; asked += 1) {
      let failure: string;
      try {
        const response = await this.#fetch(`${this.#url}/api/v1/whoami`, {
          method: "GET",
          headers: { Accept: "application/json", Authorization: `Bearer ${bearer}` },
          redirect: "manual",
          signal,
        });
        await this.note(response);
        const { status } = response;
        if (status === 401 || status === 403) return { answer: response };
        if (status === 404) {
          await drop(response);
          return await this.#noWhoami(request);
        }
        if (status >= 200 && status <= 299) {
          const text = await response.text();
          let body: unknown;
          try {
            body = JSON.parse(text);
          } catch {
            body = undefined;
          }
          if (body !== undefined) {
            const keys = privateKeysOf(body);
            if (keys === undefined) {
              throw new PrivateDispatchUnanswered(
                `${request} answered ${status} with no list of private projects it could read.`
              );
            }
            const now = this.#now();
            return keys.length === 0
              ? { until: now + ADMITTED_FOR_MS, refused: undefined, noWhoami: false }
              : { until: now + REFUSED_FOR_MS, refused: keys, noWhoami: false };
          }
          failure = `answered ${status} with a body that is not Dispatch's JSON`;
        } else if (status >= 500 || status === 408 || status === 429) {
          // A 5xx, a timeout or a rate limit can clear on asking again (`transientStatus`,
          // dispatch-http.ts); any other status answers the same ask the same way.
          await drop(response);
          failure = `answered ${status}`;
        } else {
          await drop(response);
          throw new PrivateDispatchUnanswered(`${request} answered ${status}.`);
        }
      } catch (error) {
        if (signal.aborted) throw signal.reason;
        if (isPrivateCheckFailure(error)) throw error;
        // The client's own transport failure names the URL alone; any other error by its class
        // and code, never its message, which can repeat the host or the bearer.
        const code =
          error instanceof Error && "code" in error && typeof error.code === "string"
            ? ` ${error.code}`
            : "";
        failure =
          error instanceof Error && error.name === "DispatchUnreachableError"
            ? `failed: ${error.message}`
            : `failed: ${error instanceof Error ? error.name : typeof error}${code}`;
      }
      const wait = pausesMs[asked - 1];
      if (wait === undefined || clock() + wait - started > withinMs) {
        throw new PrivateDispatchUnanswered(
          `${request}, which says whether this bearer names a private project, ${failure}${asked === 1 ? "." : ` on each of ${asked} asks.`}`
        );
      }
      await pause(wait, signal);
    }
  }

  /** A whoami answered 404: a Dispatch older than whoami's project lists. Under the state `1`
   *  that Dispatch has them, so the 404 is no answer; otherwise the held answer is returned as it
   *  came, with one warning per process naming the URL. */
  async #noWhoami(request: string): Promise<Verdict> {
    if ((await this.state()) === "1") {
      throw new PrivateDispatchUnanswered(
        `${request} answered 404, and that Dispatch answered ${PRIVATE_PROJECTS_HEADER}: 1, which only a Dispatch that serves it answers.`
      );
    }
    if (!WARNED.has(this.#url)) {
      WARNED.add(this.#url);
      this.#warn(
        `dispatch: warning: ${request} answered 404, so whether a bearer names a private project cannot be told there; its answers are returned as they come until it answers ${PRIVATE_PROJECTS_HEADER}.`
      );
    }
    return { until: this.#now() + ADMITTED_FOR_MS, refused: undefined, noWhoami: true };
  }

  /** This process's state, with the shared file's merged in. */
  async #read(): Promise<Seen> {
    let seen = SEEN.get(this.#store);
    if (seen === undefined) {
      seen = {};
      SEEN.set(this.#store, seen);
    }
    if (this.#file === undefined) return seen;
    let text: string;
    try {
      text = await readFile(this.#file, "utf8");
    } catch {
      return seen;
    }
    const filed = parsedSeen(text);
    merge(seen, filed);
    ZERO_IN_FILE.set(this.#store, later(ZERO_IN_FILE.get(this.#store), filed.zeroAt));
    return seen;
  }

  /** `seen` written to the shared file whole, through a file of its own renamed over it. A
   *  directory this process cannot write keeps the state in this process alone. */
  async #write(seen: Seen): Promise<void> {
    if (this.#file === undefined) return;
    const temporary = `${this.#file}.${process.pid}.${randomUUID()}`;
    try {
      await mkdir(dirname(this.#file), { recursive: true, mode: 0o700 });
      await writeFile(
        temporary,
        `${JSON.stringify({ one_at: seen.oneAt, zero_since: seen.zeroSince, zero_at: seen.zeroAt })}\n`,
        { mode: 0o600 }
      );
      await rename(temporary, this.#file);
      ZERO_IN_FILE.set(this.#store, seen.zeroAt);
    } catch {
      await rm(temporary, { force: true }).catch(() => undefined);
    }
  }
}

/**
 * `fetchImpl` for requests a caller sends to the Dispatch at `baseUrl` itself, each one judged by
 * the check `DispatchClient` runs (`PrivateCheck.request`). A request names its method in
 * `init.method` and its credential in `init.headers`. The whoami is asked once per request unless
 * `settings.retry` says otherwise: the caller's own retry around its request is the request's
 * retry policy, and a `PrivateDispatchUnanswered` is a failure it may ask again on. A redirect is
 * never followed by the whoami; the caller's request is sent as the caller wrote it.
 */
export function checkedDispatchFetch(
  baseUrl: string,
  fetchImpl: DispatchFetch = fetch,
  settings: PrivateCheckSettings = {}
): DispatchFetch {
  const check = new PrivateCheck(baseUrl, fetchImpl, settings);
  return (input, init) =>
    check.request(
      init.method ?? "GET",
      init.headers,
      init.signal ?? AbortSignal.timeout(WHOAMI_DEADLINE_MS),
      () => fetchImpl(input, init)
    );
}

/**
 * The check of a socket to the Dispatch at `baseUrl` (`/ws/live`, `/ws/doc/`) that a caller opens
 * itself with `headers`, run before it opens (`PrivateCheck.before`): a personal token naming a
 * private project throws `PrivateProjectRefused` and the socket is never opened; whoami's own 401
 * or 403 is answered for the caller to refuse by; undefined admits the socket. It asks one whoami
 * for a personal token, whatever Dispatch answered before, since no answer of the socket's can
 * be read; any other credential asks nothing.
 */
export async function checkDispatchSocket(
  baseUrl: string,
  headers: RequestInit["headers"],
  fetchImpl: DispatchFetch = fetch,
  settings: PrivateCheckSettings = {}
): Promise<Response | undefined> {
  return await new PrivateCheck(baseUrl, fetchImpl, settings).before(
    headers,
    AbortSignal.timeout(WHOAMI_DEADLINE_MS)
  );
}
