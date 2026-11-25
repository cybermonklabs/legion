import { mkdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { actorActText, type SessionAnswer } from "@legion/contracts";
import { urlForText } from "@legion/contracts/log-text";
import { deletedMark, editedMark } from "./actor-text";
import { dispatchWordsText, QUESTION_HEAD, textHead } from "./ask-answer";
import { leftOutWords, type ReadTime } from "./dispatch-commands/as-of";
import type { DispatchConfigResolution } from "./dispatch-config";
import { AnswerCut, DispatchClient, DispatchNoRouteError, isNoRoute } from "./dispatch-http";
import { ownerTitle } from "./dispatch-owner";
import { sessionDirectory, writeAtomically } from "./dispatch-session-directory";
import { messageFor } from "./errors";
import { jsonLineRecord } from "./json-line-record";
import { sessionAnswerRelationPhrase } from "./session-answer-relations";

/**
 * People's answers on every model request: what people wrote in the last 72
 * hours that Dispatch delivered to a session (`GET /api/v1/agents/{session_id}/answers`), rendered
 * as one message under a cap, and read on each request within a deadline, carrying the last list
 * read when Dispatch does not answer in time. pi-envoy calls `readSessionAnswers` in process, its
 * subagents sharing one in-flight read and one cache; claude-envoy's hook calls it as a short-lived
 * process, where the disk cache is what carries a list across calls; `dispatch answers` prints the
 * whole list with `renderSessionAnswersList`.
 */

/** How far back the list reaches, at the read and again at each render. */
export const SESSION_ANSWERS_WINDOW_MS = 72 * 3_600_000;

/** The cap on the whole rendered message, in UTF-16 units, as Dispatch counts a text. */
export const SESSION_ANSWERS_MAX_CHARS = 8_000;

/** How long one model request waits on the read before it carries the last list read. */
export const SESSION_ANSWERS_DEADLINE_MS = 800;

/** How long a read may run in the background, past the deadline, to fill the cache. */
const READ_TIMEOUT_MS = 10_000;

/** How long a Dispatch that has no answers route is not asked again, process-wide. */
const NO_ROUTE_BACKOFF_MS = 5 * 60_000;

/** The rendered list's first line, and the marker a host checks to find a list it already added.
 *  A list carried from the cache adds its read time on the line after, so this line never varies. */
export const SESSION_ANSWERS_MARKER =
  "[Dispatch: what people wrote to this session in the last 72 hours, newest first, word for word. Reference, not a new request.]";

/** How the line standing in for a list that could not be read begins. */
const FAILURE_MARKER = "[Dispatch: people's answers from the last 72 hours could not be read (";

/** The reason a read gives when Dispatch has no answers route (a build older than the route). */
export const NO_ANSWERS_ROUTE_REASON = "this Dispatch build has no answers route";

/** Whether `text` is a message `renderSessionAnswers` or the failure line wrote: what a host checks
 *  so a second copy of its extension adds no second list. */
export function isSessionAnswersText(text: string): boolean {
  return text.startsWith(SESSION_ANSWERS_MARKER) || text.startsWith(FAILURE_MARKER);
}

/** Whether `error` is Dispatch's own answer that it has no answers route: the `404 NOT_FOUND`
 *  "no route for …" its router answers for a path it has no route for, as a build older than the
 *  route answers it. Both shapes of that answer count: the raw one (`isNoRoute`), as the read on
 *  the request path sees it from a client built with `retryNoRoute: false`, and
 *  `DispatchNoRouteError`, which a client that asks again (the `dispatch answers` command and the
 *  `dispatch_answers` tool) reports once every attempt met no route. A 404 that something in
 *  Dispatch's place wrote (a `DispatchGatewayError`: a proxy's page, an empty body) says nothing
 *  about Dispatch's routes. */
export function isNoAnswersRoute(error: unknown): boolean {
  return isNoRoute(error) || error instanceof DispatchNoRouteError;
}

/**
 * One item as both the per-request list and `dispatch answers` print it, in this order: the time
 * the person wrote it, the person (with the agent that wrote it for them, through
 * `actorActText`) and what it is to the session (with `replaces an earlier answer` on an answer
 * that replaced one, `edited <time>` on an edited comment or message, and the mark every read
 * prints for a post its author withdrew (`deletedMark`), whose words the server sends empty); its
 * `dispatch://` reference and its issue or document, or that it is a direct message; the ask's
 * question, cut to 200 characters, on an answer, on a decline and on a reply in an ask's thread;
 * on an answer, `Selected:` with the options chosen or `none`; on a decline, `Dispatch's words:`
 * with the sentences Dispatch wrote for the asking agent; then the person's words in full.
 *
 * The person's own words and Dispatch's stand on lines of their own, the label naming whose
 * each is, so nobody reads Dispatch's instruction as something the person wrote. The words print
 * wherever an item carries them rather than on the `declined` kind alone, so an item kind a
 * newer Dispatch adds keeps them.
 */
export function renderSessionAnswerItem(item: SessionAnswer): string {
  return renderSessionAnswerItemAt(item, undefined);
}

function renderSessionAnswerItemAt(item: SessionAnswer, asOf: string | undefined): string {
  const phrase = sessionAnswerRelationPhrase(item.relation);
  const relation = `${phrase}${item.replaced === true ? " (replaces an earlier answer)" : ""}`;
  const head = [item.at, actorActText(`user ${item.author.id}`, item.author.act), relation];
  if (item.edited_at !== undefined) head.push(editedMark(item.edited_at));
  // No read of a deleted post gives any part of its body: the server sends its text empty, so no
  // words line prints, and the head line ends with the one mark every read prints.
  if (item.deleted_at !== undefined) head.push(deletedMark(item.deleted_at, item.deleted_by));
  const owner = item.owner === null ? undefined : ownerTitle(item.owner);
  const where =
    item.ref === null
      ? `Direct message ${item.id}, on no issue`
      : owner === undefined
        ? item.ref
        : `${item.ref} · ${owner}`;
  const lines = [head.join(" · "), where];
  if (item.ask !== undefined) {
    // The question is the ask's, whoever asked it - the session, another session or a person -
    // and not what the person wrote in this item, which prints whole below; it is cut to the
    // length every surface quotes a question at (QUESTION_HEAD). An answer at a cut leaves out a
    // question the records at the cut cannot give, and it prints as left out at the cut, or by
    // Dispatch where the cut is not known (`leftOutWords`).
    const question: string | undefined = item.ask.question;
    const text = question === undefined ? leftOutWords(asOf) : textHead(question, QUESTION_HEAD);
    lines.push(`Question: ${text}`);
  }
  if (item.kind === "answer") {
    const selected = item.selected ?? [];
    lines.push(`Selected: ${selected.length === 0 ? "none" : selected.join(", ")}`);
  }
  const words = dispatchWordsText(item.dispatch_words);
  if (words !== undefined) lines.push(words);
  if (item.text !== "") lines.push(item.text);
  return lines.join("\n");
}

/** The items written inside the 72 hours before `now`, in the order given. */
function withinWindow(items: readonly SessionAnswer[], now: number): SessionAnswer[] {
  const start = now - SESSION_ANSWERS_WINDOW_MS;
  return items.filter((item) => Date.parse(item.at) >= start);
}

/** A read's `words` and `since` for a list rendered from `items` at `now`: every item Dispatch
 *  sent inside the window, shown or left out by the cap, and where the window starts. */
function windowOf(
  items: readonly SessionAnswer[],
  now: number
): { readonly words: string[]; readonly since: string } {
  return {
    words: sessionAnswersWords(withinWindow(items, now)),
    since: new Date(now - SESSION_ANSWERS_WINDOW_MS).toISOString(),
  };
}

/** The instant (epoch ms) a list counts its 72 hours back from: the cut its answer named
 *  (`SessionAnswersCache.as_of`), or `now()` for a list of the present. A cut that does not parse
 *  counts as none, so no read throws on it. */
function windowEnd(asOf: string | undefined, now: () => number): number {
  const cut = asOf === undefined ? Number.NaN : Date.parse(asOf);
  return Number.isNaN(cut) ? now() : cut;
}

/**
 * What the people wrote in each of `items`, and what each is to the session, one string per item:
 * its kind, id, author and times, the options chosen, the words and its relation.
 * `readSessionAnswers` gives it for every item Dispatch sent inside the read's 72-hour window,
 * the ones its list shows and the ones the character cap left out, so a host comparing two reads
 * (`sessionAnswersChanged`) tells a new item, an answer, comment or message a person changed, or
 * an item that became or stopped being the session's own, from a list that only lost items to the
 * window or changed what the session or Dispatch says around them (an issue's title, an ask's
 * question, a cached list's header). An item the cap leaves out counts too: a newest item longer
 * than the cap, or a comment an edit made too long to fit, is shown to nobody, and a host that
 * compared only the items shown would go on showing the person's old words as current. Dispatch
 * cuts the list itself (`max_chars`) after the first item that takes its count past the cap, so it
 * sends such an item; the items after its cut, which no list shows, reach the host only as
 * `total`.
 *
 * The author is the rendered act (`actorActText`), not the person's id alone, so the same words
 * from the person themselves and from an agent acting for them are two different items: a host
 * that compared ids would carry the second as if the person had written it. Dispatch gives the
 * relation at each read, from who asked the item's ask and who follows it at that moment: once
 * the session follows an ask, a reply on it that reached the session earlier reads as a reply on
 * its own ask, and a host that kept the old list would go on telling the session the ask is
 * another's. The fourth field is the item's time, which `sessionAnswersChanged` reads back.
 */
export function sessionAnswersWords(items: readonly SessionAnswer[]): string[] {
  return items.map((item) =>
    JSON.stringify([
      item.kind,
      item.id,
      actorActText(item.author.id, item.author.act),
      item.at,
      item.edited_at ?? null,
      item.deleted_at ?? null,
      item.selected ?? null,
      item.text,
      item.relation,
    ])
  );
}

/** The time (epoch ms) of the item a `sessionAnswersWords` string names, or NaN when `word` is no
 *  such string. */
function wordTime(word: string): number {
  try {
    const fields: unknown = JSON.parse(word);
    return Array.isArray(fields) && typeof fields[3] === "string"
      ? Date.parse(fields[3])
      : Number.NaN;
  } catch {
    return Number.NaN;
  }
}

/**
 * Whether a read whose `words` are `next` shows something that a list with the `words` `shown`
 * did not: an item new to it, an answer, comment or message a person changed, or an item whose
 * relation to the session changed (a string `shown` lacks); or an item `shown` held that is gone
 * from the read while its time is still inside the read's window, at or after `since`
 * (`SessionAnswersRead.since`), which a person retracted (an answer set back to none) or Dispatch
 * removed. An item gone because its time left the window is no change: the window moved, and
 * nothing a person wrote did. A string whose time cannot be read, or a read with no `since`,
 * counts as gone inside the window, so a host never goes on showing what may have been retracted.
 */
export function sessionAnswersChanged(
  shown: readonly string[],
  next: readonly string[],
  since: string | undefined
): boolean {
  const before = new Set(shown);
  if (next.some((word) => !before.has(word))) return true;
  const after = new Set(next);
  const start = since === undefined ? Number.NaN : Date.parse(since);
  return shown.some((word) => {
    if (after.has(word)) return false;
    const at = wordTime(word);
    return Number.isNaN(at) || Number.isNaN(start) || at >= start;
  });
}

/** `SESSION_ANSWERS_MAX_CHARS` as the left-out line prints it. */
const MAX_CHARS_TEXT = SESSION_ANSWERS_MAX_CHARS.toLocaleString("en-US");

/** The last line of a list that left `count` items out: it names `dispatch answers`, which reads
 *  every one, unless `namesCommand` is false because that command cannot read the Dispatch. */
function leftOutLine(count: number, namesCommand: boolean): string {
  const counted = `${count} older ${count === 1 ? "item" : "items"} left out to stay under ${MAX_CHARS_TEXT} characters`;
  return namesCommand
    ? `${counted}: run \`dispatch answers\` to read every one in full.`
    : `${counted}.`;
}

export interface RenderSessionAnswersOptions {
  /** The render's time: an item written more than 72 hours before it is not shown or counted. */
  readonly now: number | Date;
  /** How many items the window held when the list was read (`SessionAnswersResponse.total`),
   *  when the server cut the list short with `max_chars`, so the left-out count covers the items
   *  it did not send; the items' own count when unset. */
  readonly total?: number | undefined;
  /** Set when a cached list stands in for a read that failed: when that list was read and why
   *  this read failed, which the header names on its second line. */
  readonly cached?: { readonly readAt: string; readonly reason: string } | undefined;
  /** Set when `dispatch answers` cannot read the Dispatch the list came from, since it has no
   *  answers route: the left-out line then counts the items left out and names no command. */
  readonly withoutCommand?: boolean | undefined;
  /** The cut the list was read at, where its answer named one in `Dispatch-As-Of`: Dispatch
   *  answers a token that carries a cut at that cut, whatever the read sends. An item whose ask's
   *  question the answer left out prints `Question: left out at <cut>`, or `Question: left out by
   *  Dispatch` where this is unset. Nothing else of the list changes. */
  readonly asOf?: string | undefined;
}

/**
 * The per-request message: `SESSION_ANSWERS_MARKER` (and, for a cached list, `As read at <time>;
 * this read failed: <reason>.` on the next line), then each item newest first, blank-line
 * separated, as a strict prefix under the cap: the whole message, header and left-out line
 * included, is shorter than `SESSION_ANSWERS_MAX_CHARS` (at most 7,999 characters, as the
 * left-out line's "under 8,000" says); the first item that does not fit is left
 * out with every item after it, so an item over the cap on its own is left out whole, never cut.
 * When any item is left out, the last line counts them and names `dispatch answers` (no command
 * with `withoutCommand`). Items are
 * filtered by the 72-hour window at `now` first. Undefined when the window holds nothing.
 */
export function renderSessionAnswers(
  items: readonly SessionAnswer[],
  options: RenderSessionAnswersOptions
): string | undefined {
  const inWindow = withinWindow(items, Number(options.now));
  // A list the server cut short with max_chars left out only items older than every one it sent,
  // so once any item it sent has left the window, every one it left out has too.
  const unsent =
    inWindow.length === items.length
      ? Math.max(0, (options.total ?? items.length) - items.length)
      : 0;
  const count = inWindow.length + unsent;
  if (count === 0) return undefined;
  const header =
    options.cached === undefined
      ? SESSION_ANSWERS_MARKER
      : `${SESSION_ANSWERS_MARKER}\nAs read at ${options.cached.readAt}; this read failed: ${failureSentence(options.cached.reason)}`;
  const namesCommand = options.withoutCommand !== true;
  const blocks: string[] = [];
  let length = header.length;
  for (const item of inWindow) {
    const block = renderSessionAnswerItemAt(item, options.asOf);
    const after = count - blocks.length - 1;
    const needed =
      length + 2 + block.length + (after === 0 ? 0 : 2 + leftOutLine(after, namesCommand).length);
    if (needed >= SESSION_ANSWERS_MAX_CHARS) break;
    blocks.push(block);
    length += 2 + block.length;
  }
  const leftOut = count - blocks.length;
  const text = [
    header,
    ...blocks,
    ...(leftOut === 0 ? [] : [leftOutLine(leftOut, namesCommand)]),
  ].join("\n\n");
  return text;
}

/** What `dispatch answers` prints: every item of the 72 hours before `time`, newest first, in the
 *  per-request list's item format and with no cap, or a line saying there is none. A list read at
 *  a cut counts the 72 hours back from the instant its answer named (`at`), and says so in the
 *  words the read names the cut in (`asOf`). */
export function renderSessionAnswersList(items: readonly SessionAnswer[], time: ReadTime): string {
  const asOf = "asOf" in time ? time.asOf : undefined;
  const now = "asOf" in time ? time.at : time.now;
  const window = asOf === undefined ? "in the last 72 hours" : `in the 72 hours before ${asOf}`;
  const shown = withinWindow(items, now);
  if (shown.length === 0) return `No person wrote to this session ${window}.`;
  return [
    `${shown.length} ${shown.length === 1 ? "item" : "items"} people wrote to this session ${window}, newest first, word for word:`,
    ...shown.map((item) => renderSessionAnswerItemAt(item, asOf)),
  ].join("\n\n");
}

/** A read's failure reason as the end of a sentence: whitespace collapsed, and a full stop added
 *  only when the reason does not already end in `.`, `?` or `!`. */
function failureSentence(reason: string): string {
  const text = reason.replace(/\s+/g, " ").trim();
  return /[.?!]$/.test(text) ? text : `${text}.`;
}

/** The line a request carries in place of the list when no list it could carry renders. */
export function sessionAnswersFailureLine(reason: string): string {
  return `${FAILURE_MARKER}${reason.replace(/\s+/g, " ").trim()}); run \`dispatch answers\`.]`;
}

/** The last list read for a session from one Dispatch, as memory and `answers.json` keep it. */
export interface SessionAnswersCache {
  /** The Dispatch URL the list was read from: only a read of that URL carries it. */
  readonly dispatch_url: string;
  /** When the read answered (ISO 8601 UTC). */
  readonly read_at: string;
  /** `SessionAnswersResponse.total` of that read. */
  readonly total: number;
  /** `SessionAnswersResponse.items` of that read, newest first. */
  readonly items: readonly SessionAnswer[];
  /** The cut that read's answer named in `Dispatch-As-Of`, an answer under a token that carries
   *  a cut; absent for an answer of the present, and in a file an older build wrote. */
  readonly as_of?: string;
}

type ReadOutcome =
  | { readonly cache: SessionAnswersCache }
  | { readonly failure: string }
  /** Dispatch answered that it has no answers route (`isNoAnswersRoute`). */
  | { readonly noRoute: true };

/** One session's read state for one Dispatch URL. */
interface SessionEntry {
  /** The last list read from that URL, from a read in this process or `answers.json`. */
  cache: SessionAnswersCache | undefined;
  /** The read in flight to that URL, which every request of the session to it joins; it never
   *  rejects. */
  inFlight: Promise<ReadOutcome> | undefined;
  /** Whether a request already waited the whole deadline on `inFlight`, so no other waits on it. */
  overdue: boolean;
  /** Whether a read already gave the session the notice that the URL has no answers route, since
   *  the last read of it that answered with a list. */
  noRouteReported: boolean;
}

interface SessionAnswersState {
  /** Each session's read state, by session id and then by Dispatch URL. */
  readonly sessions: Map<string, Map<string, SessionEntry>>;
  /** Until when (epoch ms) each Dispatch URL that answered it has no answers route is not asked
   *  again. */
  readonly noRouteUntil: Map<string, number>;
}

/** The process-wide read state. It lives on `globalThis` under a registered symbol, as
 *  `envoy-config.ts`'s kept command outcomes do, because Oh My Pi loads each extension instance
 *  (a subagent's included) as a fresh module, and the parent and every subagent share one read
 *  and one cache per session and Dispatch URL. Its shape (`SessionAnswersState`) is its contract
 *  across bundles: a change to it takes a new key (`.v4`). */
const STATE_KEY: unique symbol = Symbol.for("legion.pi-envoy.session-answers.v3");
const processGlobal = globalThis as typeof globalThis & {
  [STATE_KEY]?: SessionAnswersState;
};
const state: SessionAnswersState = processGlobal[STATE_KEY] ?? {
  sessions: new Map(),
  noRouteUntil: new Map(),
};
processGlobal[STATE_KEY] = state;

const CACHE_FILE = "answers.json";

/** `answers.json` under `directory` when it holds a list read from `url`, else undefined: a
 *  missing or broken cache, or one another Dispatch answered, only means the request has no list
 *  to carry. */
function readCacheFile(directory: string, url: string): SessionAnswersCache | undefined {
  try {
    const value: unknown = JSON.parse(readFileSync(join(directory, CACHE_FILE), "utf-8"));
    if (
      typeof value === "object" &&
      value !== null &&
      "dispatch_url" in value &&
      typeof value.dispatch_url === "string" &&
      value.dispatch_url === url &&
      "read_at" in value &&
      typeof value.read_at === "string" &&
      "total" in value &&
      typeof value.total === "number" &&
      "items" in value &&
      Array.isArray(value.items)
    ) {
      // Only `readSessionAnswers` writes this file, owner-only in the session's private
      // directory, with the items a Dispatch read answered.
      return {
        dispatch_url: value.dispatch_url,
        read_at: value.read_at,
        total: value.total,
        items: value.items as SessionAnswer[],
        ...("as_of" in value && typeof value.as_of === "string" ? { as_of: value.as_of } : {}),
      };
    }
  } catch {
    // Missing, unreadable or not JSON: no cache.
  }
  return undefined;
}

export interface ReadSessionAnswersOptions {
  /** The session whose list is read: a subagent passes its parent's. */
  readonly session: string;
  /** Dispatch as `sessionDispatchSettings(…, { commands: "kept" })` resolves it, so no `!` token
   *  command runs on the request path. Off with a notice (nothing configures Dispatch) reads
   *  nothing; off with an error is a failed read with that error as its reason. */
  readonly dispatch: DispatchConfigResolution;
  /** The Dispatch URL the session is configured with (`sessionDispatchUrl`), for `dispatch` off
   *  with an error, which names none: only a list read from this URL stands in. Unset, such a read
   *  carries the failure line. Not read while `dispatch` is on: its own URL is the one. */
  readonly dispatchUrl?: string | undefined;
  /** The environment the session's state directory is worked out from
   *  (`sessionDirectory(env, session)`), which holds `answers.json`. A session id that is not one
   *  plain path segment keeps the cache in memory only. */
  readonly env: Readonly<Record<string, string | undefined>>;
  readonly fetchImpl?: typeof fetch | undefined;
  /** The request's own abort: it stops this call's wait, never the shared read. */
  readonly signal?: AbortSignal | undefined;
  /** The clock (epoch ms): the read time, the 404 back-off and the window of a list of the
   *  present read it; a list read at a cut counts its window back from the cut instead. */
  readonly now?: (() => number) | undefined;
  /** How long this call waits on the read (`SESSION_ANSWERS_DEADLINE_MS` by default). */
  readonly deadlineMs?: number | undefined;
}

export interface SessionAnswersRead {
  /** The message to carry: the list, the list as last read, or the failure line; undefined when
   *  there is nothing to show, Dispatch has no answers route, or Dispatch is not configured. */
  readonly text: string | undefined;
  /** `fresh`: this request's read answered. `cache`: it did not, and the last list read stands
   *  in. `failure`: it did not and no list read has an item inside the 72 hours, so `text` is the
   *  failure line. `unavailable`: Dispatch answered that it has no answers route
   *  (`isNoAnswersRoute`, or that answer's 5-minute back-off) and no list read has an item inside
   *  the 72 hours, so the request carries nothing: the failure line would send the agent to
   *  `dispatch answers`, which that Dispatch refuses too. `off`: nothing configures Dispatch. */
  readonly source: "fresh" | "cache" | "failure" | "unavailable" | "off";
  /** When the list shown was read (`fresh` and `cache`). */
  readonly readAt?: string;
  /** `sessionAnswersWords` of every item Dispatch sent inside the 72-hour window the list was
   *  rendered for (`fresh` and `cache`), the ones the character cap left out included: what the
   *  people wrote, and what each item is to the session. A `fresh` read whose window holds no item
   *  gives `[]` and no `text`, so a host still learns that the items of a list it shows left the
   *  read (`sessionAnswersChanged`). */
  readonly words?: readonly string[];
  /** The start of that window (ISO 8601 UTC), 72 hours before the render, or before the cut for
   *  a list read at one (`fresh` and `cache`): an item `words` no longer holds whose time is at or
   *  after it left the read inside the window (`sessionAnswersChanged`). */
  readonly since?: string;
  /** Why the read gave no list (`cache`, `failure`, `unavailable`), or the notice that turns
   *  Dispatch off. */
  readonly reason?: string;
  /** For the host to report once, never to the model: set on the first read for this session
   *  and Dispatch URL that found the URL has no answers route, and again only after a read of it
   *  answered with a list. It names the URL. */
  readonly notice?: string;
}

/** Tests only: the file each wait below appends `{ armedMs, endedBy }` to, one JSON line each, so
 *  a test across the process boundary reads the deadline a request's read was given and what
 *  ended it rather than timing the request. Unset, as every deployment runs, nothing is written. */
export const ANSWERS_WAIT_RECORD_FILE = "ENVOY_ANSWERS_WAIT_RECORD_FILE";

const recordWait = jsonLineRecord<{
  readonly armedMs: number;
  readonly endedBy: "deadline" | "aborted" | "answered";
}>(process.env, ANSWERS_WAIT_RECORD_FILE);

/** `promise`'s value, or which came first of `ms` passing and `signal` aborting. */
async function settleWithin<T>(
  promise: Promise<T>,
  ms: number,
  signal: AbortSignal | undefined
): Promise<T | "deadline" | "aborted"> {
  if (signal?.aborted === true) return "aborted";
  let timer: NodeJS.Timeout | undefined;
  let onAbort: (() => void) | undefined;
  const stop = new Promise<"deadline" | "aborted">((resolve) => {
    timer = setTimeout(() => resolve("deadline"), ms);
    onAbort = () => resolve("aborted");
    signal?.addEventListener("abort", onAbort, { once: true });
  });
  try {
    const outcome = await Promise.race([promise, stop]);
    recordWait?.({
      armedMs: ms,
      endedBy: outcome === "deadline" ? "deadline" : outcome === "aborted" ? "aborted" : "answered",
    });
    return outcome;
  } finally {
    clearTimeout(timer);
    if (onAbort !== undefined) signal?.removeEventListener("abort", onAbort);
  }
}

/**
 * The list for one model request, never throwing. Every request reads it; one read is in flight
 * per session and Dispatch URL for the whole process, and every request of the session (the
 * parent's and each subagent's) to that URL joins it. A request waits at most `deadlineMs`
 * (800 ms); past that it carries the last list read, and the read goes on in the background (for
 * at most 10 s) and fills the cache when it answers. While a read that a request already waited
 * out is still in flight, a new request does not wait on it: it carries the cache at once. The
 * cache is in memory, keyed by session and Dispatch URL, and in `answers.json` under the
 * session's state directory (`sessionDirectory(env, session)`) with the URL it was read from,
 * loaded when memory has none for that URL; a list read from another Dispatch is never carried.
 * A cached list is rendered with its read time and the reason this read failed, and filtered by
 * the 72 hours again. A read of the present asks Dispatch for the 72 hours by this host's clock
 * (`since`), the clock every render of its list filters by, so an item Dispatch would drop by its
 * own clock is still sent while the render's window holds it, and is not taken for one a person
 * retracted (`sessionAnswersChanged`). When no cached list renders (none was read, it was empty,
 * or every item is now past 72 hours), a failed read carries the failure line naming why.
 * Dispatch off with an error names no URL: only a list read from `dispatchUrl` stands in, and with
 * none given the failure line does. A Dispatch that answers it has no answers route
 * (`isNoAnswersRoute`) is not asked again for 5 minutes, process-wide; that answer, and a request
 * inside those 5 minutes, carry the list read before from that URL, its left-out line naming no
 * command, or else nothing: the failure line would send the agent to `dispatch answers`, which
 * that Dispatch refuses too. The first such read for a session and URL, until a read of it answers
 * with a list again, gives the host a `notice` to report. Every other failure, a gateway's 404 or
 * a 200 that is no answers list included, is a failed read as above.
 *
 * An answer under a token that carries a cut names the cut (`AnswerCut`), which the list keeps in
 * memory and in `answers.json` (`as_of`), so the fresh list and each list carried from it print an
 * ask's question the answer left out as left out at that cut; a list from a file with no cut
 * prints it as left out by Dispatch. Dispatch answers such a token at its cut whatever the read
 * sends, so a read under it counts the 72 hours back from the cut, not from this host's clock:
 * every render of its list, its `words` and its `since`. Where the list read before kept a cut,
 * the read sends no `since`, and Dispatch answers the 72 hours before the cut it reads at, the
 * token's, whatever cut the list kept. When the answer's having a cut differs from the list's
 * (the list kept none, as on a token's first read, or the session's token now reads the present),
 * the read asks once more, at once, as that answer was answered: with no `since` at a cut, by
 * this host's clock at none. That second answer is the read's own: it is the same read in flight,
 * under the same 10 s, and when it fails the read fails.
 */
export async function readSessionAnswers(
  options: ReadSessionAnswersOptions
): Promise<SessionAnswersRead> {
  const now = options.now ?? Date.now;
  const { dispatch, session } = options;
  if (!dispatch.enabled && dispatch.error === null) {
    return { text: undefined, source: "off", reason: dispatch.notice };
  }
  let directory: string | undefined;
  try {
    directory = sessionDirectory(options.env, session);
  } catch {
    // An id that is not one plain path segment keeps no answers.json; the read still runs.
  }
  const entries = state.sessions.get(session) ?? new Map<string, SessionEntry>();
  state.sessions.set(session, entries);
  const carry = (
    cache: SessionAnswersCache | undefined,
    reason: string,
    noRoute = false
  ): SessionAnswersRead => {
    const at = windowEnd(cache?.as_of, now);
    const text =
      cache === undefined
        ? undefined
        : renderSessionAnswers(cache.items, {
            now: at,
            total: cache.total,
            cached: { readAt: cache.read_at, reason },
            withoutCommand: noRoute,
            asOf: cache.as_of,
          });
    if (cache === undefined || text === undefined) {
      if (noRoute) return { text: undefined, source: "unavailable", reason };
      // A cache that renders nothing is no list to show: the failure line keeps a read that failed
      // from looking like no person wrote.
      return { text: sessionAnswersFailureLine(reason), source: "failure", reason };
    }
    return { text, source: "cache", readAt: cache.read_at, ...windowOf(cache.items, at), reason };
  };
  if (!dispatch.enabled) {
    const url = options.dispatchUrl;
    if (url === undefined) return carry(undefined, dispatch.error);
    const cache =
      entries.get(url)?.cache ??
      (directory === undefined ? undefined : readCacheFile(directory, url));
    return carry(cache, dispatch.error);
  }
  const { url } = dispatch;
  const current: SessionEntry = entries.get(url) ?? {
    cache: undefined,
    inFlight: undefined,
    overdue: false,
    noRouteReported: false,
  };
  entries.set(url, current);
  if (current.cache === undefined && directory !== undefined) {
    current.cache = readCacheFile(directory, url);
  }
  const noRoute = (): SessionAnswersRead => {
    const read = carry(current.cache, NO_ANSWERS_ROUTE_REASON, true);
    if (current.noRouteReported) return read;
    current.noRouteReported = true;
    return {
      ...read,
      notice: `Dispatch at ${urlForText(url, "the configured Dispatch URL")} has no answers route (its build is older than the route), so people's answers cannot be read from it`,
    };
  };
  if ((state.noRouteUntil.get(url) ?? 0) > now()) return noRoute();
  if (current.inFlight !== undefined && current.overdue) {
    return carry(current.cache, "an earlier read is still waiting on Dispatch");
  }
  let read = current.inFlight;
  if (read === undefined) {
    // The client runs the read once: an answer of no route is this read's own (`isNoAnswersRoute`,
    // `NO_ROUTE_BACKOFF_MS`), and that 5-minute back-off, not the client's waits, is what carries
    // the request past a release rolling out, within this call's deadline. The deadline is the
    // client's own, so its credential is given the read less the request's reserve.
    const client = new DispatchClient(url, dispatch.credential, options.fetchImpl ?? fetch, {
      retryNoRoute: false,
      deadlineMs: READ_TIMEOUT_MS,
    });
    // A replayed agent reads under a token that carries a cut, and Dispatch answers it at that
    // cut whatever the read sends, leaving out each question the records at the cut cannot give:
    // each request learns the cut its answer names, and the list keeps it, so such a question
    // prints as left out at it and the list's 72 hours count back from it. A read at a cut sends
    // no `since`: Dispatch counts the 72 hours back from the cut it answers at, which is the
    // token's, whatever cut the list kept (api/session_answers.go, parseSessionAnswersSince). A
    // read of the present sends the 72 hours by this host's clock.
    const ask = async (atCut: boolean) => {
      const cut = new AnswerCut();
      const response = await client.sessionAnswers(
        session,
        {
          ...(atCut ? {} : { since: new Date(now() - SESSION_ANSWERS_WINDOW_MS).toISOString() }),
          maxChars: SESSION_ANSWERS_MAX_CHARS,
        },
        cut
      );
      return { response, asOf: cut.cut, atCut };
    };
    // The read asks as the list read before was answered: at a cut where it kept one. An answer
    // whose having a cut differs (a token's first read, or a session whose token now reads the
    // present) answered the wrong window, so the read asks once more, at once, as that answer was
    // answered.
    const started = ask(current.cache?.as_of !== undefined)
      .then((answer) =>
        answer.atCut === (answer.asOf !== undefined) ? answer : ask(answer.asOf !== undefined)
      )
      .then(
        ({ response, asOf }): ReadOutcome => {
          // A 200 whose body is no answers list (a proxy's page, another service on the URL) is a
          // failed read, never a cache: rendering it would throw on the request path.
          if (!Array.isArray(response?.items) || typeof response.total !== "number") {
            return { failure: "Dispatch answered with something that is not an answers list" };
          }
          const cache: SessionAnswersCache = {
            dispatch_url: url,
            read_at: new Date(now()).toISOString(),
            total: response.total,
            items: response.items,
            ...(asOf === undefined ? {} : { as_of: asOf }),
          };
          current.cache = cache;
          current.noRouteReported = false;
          if (directory !== undefined) {
            try {
              mkdirSync(directory, { recursive: true, mode: 0o700 });
              writeAtomically(join(directory, CACHE_FILE), JSON.stringify(cache));
            } catch {
              // The memory cache still holds it; the next read writes the file again.
            }
          }
          return { cache };
        },
        (error: unknown): ReadOutcome => {
          if (isNoAnswersRoute(error)) {
            state.noRouteUntil.set(url, now() + NO_ROUTE_BACKOFF_MS);
            return { noRoute: true };
          }
          return { failure: messageFor(error) };
        }
      );
    read = started.finally(() => {
      if (current.inFlight === read) {
        current.inFlight = undefined;
        current.overdue = false;
      }
    });
    current.inFlight = read;
    current.overdue = false;
  }
  const deadlineMs = options.deadlineMs ?? SESSION_ANSWERS_DEADLINE_MS;
  const outcome = await settleWithin(read, deadlineMs, options.signal);
  if (outcome === "deadline") {
    if (current.inFlight === read) current.overdue = true;
    return carry(current.cache, `Dispatch did not answer within ${deadlineMs} ms`);
  }
  if (outcome === "aborted") {
    return carry(current.cache, "the request was cancelled before Dispatch answered");
  }
  if ("noRoute" in outcome) return noRoute();
  if ("failure" in outcome) return carry(current.cache, outcome.failure);
  const at = windowEnd(outcome.cache.as_of, now);
  return {
    text: renderSessionAnswers(outcome.cache.items, {
      now: at,
      total: outcome.cache.total,
      asOf: outcome.cache.as_of,
    }),
    source: "fresh",
    readAt: outcome.cache.read_at,
    ...windowOf(outcome.cache.items, at),
  };
}

/** Settles once every read in flight for `session`, to any Dispatch, has filled its cache or
 *  failed. A short-lived host (claude-envoy's hook) awaits it after printing, within a budget of
 *  its own, so a read that answered past the deadline still writes `answers.json` for the next
 *  process. */
export async function sessionAnswersSettled(session: string): Promise<void> {
  await Promise.all(
    Array.from(state.sessions.get(session)?.values() ?? [], (entry) => entry.inFlight)
  );
}
