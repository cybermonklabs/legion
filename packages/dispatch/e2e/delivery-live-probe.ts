/** `bun e2e/delivery-live-probe.ts`: an agent's client for `GET /ws/live`, the websocket the
 * Evals tab keeps itself current over. It subscribes one of the tab's views (an `evals`, `items`
 * or `project-timeline` view) under a bearer token and writes one JSON line per frame Dispatch
 * sends, applying each to the body it holds with the page's own `applyDelta` and `applyRevs`. It
 * reconnects as the page does, and each new connection resumes from the last change it applied.
 * What bounds each wait of the probe:
 *
 *  - Before the first open, `--open-within` seconds from the first dial; without that flag, the
 *    page's silence rule, `LIVE_SOCKET_SILENT_MS` (45 s, three heartbeats). A first dial that
 *    has not opened by then fails the watch, and a first connection that is refused or closed
 *    fails it at once. A later connection that is refused or never opens is dialled again.
 *  - After the first open, `--minutes` counted from that open, which ends the watch cleanly and
 *    writes an `end` line. The first `open` line and the `end` line read one clock, so the `end`
 *    line's `at` less the first `open` line's `at` is the watch's length and never less than
 *    `--minutes`. A reconnect's open does not count it again.
 *  - Between reconnects, the wait `reconnectWaitMs` computes: none after Dispatch's rotation
 *    (4001), which the probe answers at once. Any other close waits on the page's backoff, which
 *    Dispatch's restart close (1012) and every frame received start over: the first attempt
 *    draws from its spread, up to `RECONNECT_SPREAD_MS` (2 s) or what `--reconnect-spread-ms`
 *    sets, and each later attempt backs off, up to 30 s. `--minutes` ends the watch during that
 *    wait as well. Each close line carries that wait as `wait_ms`, and null on a close the probe
 *    answers with no timer: the rotation, a silent socket's replacement, and a refused first
 *    connection, which ends the watch.
 *  - A silent socket, after the first open: one that has carried no frame for more than
 *    `LIVE_SOCKET_SILENT_MS`, a reconnect's dial that never opens included, is replaced by a new
 *    dial at the next check, which runs each second, and `--minutes` still ends the watch.
 *  - Each `--compare` read, while Dispatch sends nothing: `--read-within` seconds; without that
 *    flag, the page's own deadline on a GET's silence, `REQUEST_DEADLINE_MS` (30 s). The time
 *    starts at the request and starts over as the headers arrive and as each piece of the body
 *    does, so a read that keeps sending goes on however slowly, and one that sends nothing for
 *    that long fails the probe. The reads run one after another: one for an evals or an items
 *    view, and one for each page of `PROJECT_TIMELINE_PAGE_MAX` rows a project-timeline view
 *    holds.
 *
 *  No flag takes a time longer than the longest delay a timer keeps, 2^31-1 ms (about 24.9 days).
 *  Reading `--token-file` and writing `--body-file` wait as long as the named file makes them, as
 *  a FIFO with no writer or no reader does; whoever named the file ends that wait. The probe takes
 *  such a file, since `--token-file <(...)` and `/dev/stdin` are uses it serves.
 *
 *  Ctrl-C or SIGTERM ends the watch at any time. Then `--body-file` writes the body it holds, and
 *  `--compare` reads the GET route the view mirrors and writes, for each read, whether what it
 *  answers equals what the probe holds: for a project-timeline view, the route
 *  `GET /api/v1/projects/{key}/timeline` over the last window the probe applied, a page at a time,
 *  each page's row ids and every field the page carries but the freshness; for an evals view, the
 *  list read `GET /api/v1/projects/{key}/evals`, and for an items view the eval's rows
 *  `GET /api/v1/projects/{key}/evals/{slug}/items`, each over the view's own window (a preset's
 *  days ending then) and each field by field (an evals view's every eval and every field but the
 *  refresher's age, which the route measures at the moment of its read). Dispatch refusing the
 *  view ends the watch at once and the probe exits 1 with neither: no body is written and no
 *  route is read.
 *
 *    bun e2e/delivery-live-probe.ts --url <dispatch> --token-file <file> \
 *      --view '{"kind":"project-timeline","project":"INFRA","window":{"preset":28},"query":"limit=200"}' \
 *      --minutes 10 --compare
 *
 *    bun e2e/delivery-live-probe.ts --url <dispatch> --token-file <file> \
 *      --view '{"kind":"evals","project":"INFRA","window":{"preset":28},"query":"kind=delivery"}' \
 *      --minutes 10 --compare
 *
 *    bun e2e/delivery-live-probe.ts --url <dispatch> --token-file <file> \
 *      --view '{"kind":"items","project":"INFRA","slug":"pr-ci","window":{"preset":28},"query":"sort=failures"}' \
 *      --minutes 10 --compare
 *
 *  It exits 0 when every comparison is equal or none was asked for; 1 when one is not, a read
 *  fails or receives nothing for `--read-within`, `--compare` was asked for and no snapshot
 *  arrived to compare, the first connection is refused or does not open within `--open-within`,
 *  Dispatch refuses the view, or a frame breaks the contract; 2 on a usage error. */
import { writeFileSync } from "node:fs";
import { parseArgs } from "node:util";
import { LIVE_SOCKET_MAX_AGE_CLOSE } from "@legion/contracts";

import { REQUEST_DEADLINE_MS } from "../web/src/api/client";
import {
  LIVE_SOCKET_SILENT_MS,
  RECONNECT_SPREAD_MS,
  reconnectDelayMs,
} from "../web/src/api/live-socket";
import type {
  DeliveryLiveBody,
  DeliveryLiveCounts,
  DeliveryLiveDelta,
  DeliveryLiveFreshness,
  DeliveryLiveMap,
  DeliveryLiveRevs,
  DeliveryLiveView,
  EvalItemsResponse,
  EvalListResponse,
  LiveServerFrame,
  LiveSubscribe,
  ProjectTimelineResponse,
} from "../web/src/api/types";
import { applyDelta, applyRevs } from "../web/src/features/delivery/lib/applyDelta";
import { DAY_MS } from "../web/src/features/delivery/lib/window";
import { readUntilSilent } from "./silence-read";

/** The frames `GET /ws/live` sends: a delivery view's own, the freshness verdict, a refusal and
 * the heartbeat. */
type DeliveryServerFrame = Extract<
  LiveServerFrame,
  { readonly type: "snapshot" | "delta" | "freshness" | "error" | "heartbeat" }
>;

/** The views the probe watches: the Evals tab's own. */
export type ProbeView = Extract<
  DeliveryLiveView,
  { readonly kind: "evals" | "items" | "project-timeline" }
>;

/** A delta's changes to one list: the ids of the items it upserts, whole, and of those that left. */
export interface ProbeListChanges {
  readonly upsert: readonly (string | number)[];
  readonly remove: readonly string[];
}

/** One frame Dispatch sent: when it arrived and its size on the wire in UTF-8 bytes. A snapshot
 *  counts what it holds: its lists' items, each map's keys, and each count map's values by facet.
 *  A delta names what it changes: each list's upserted and removed ids, each map's keys, each
 *  count map's values by facet, and the fields `set` replaces whole. A project-timeline view's
 *  lists are `items`, its rows by id, and `session_groups`; an evals view's one list is `items`,
 *  its evals by id; an items view's rows carry no id, so its body is counted by its rows and a
 *  delta sends them in `set`. A section the frame does not carry is null; an error frame adds its
 *  code and message, a freshness frame the verdict. */
export interface ProbeFrameLine {
  readonly at: string;
  readonly type: DeliveryServerFrame["type"];
  readonly bytes: number;
  readonly seq: string | null;
  readonly changed_at: string | null;
  readonly session_groups: number | ProbeListChanges | null;
  readonly items: number | ProbeListChanges | null;
  readonly maps: Readonly<Record<string, number | readonly string[]>> | null;
  readonly counts: Readonly<
    Record<string, Readonly<Record<string, number | readonly string[]>>>
  > | null;
  readonly set: readonly string[] | null;
  readonly code?: string;
  readonly message?: string;
  readonly freshness?: DeliveryLiveFreshness["freshness"];
  readonly stale?: readonly string[];
}

/** A connection opening, and the change it resumes from (null on the first subscribe). */
export interface ProbeOpenLine {
  readonly at: string;
  readonly type: "open";
  readonly resume: string | null;
}

/** A connection closing, by Dispatch or by the probe replacing a silent socket. `wait_ms` is the
 *  wait the probe computed before it reopens, null on a close it answers with no timer: the
 *  rotation it reopens at once after, a silent socket's replacement, and the refused first
 *  connection, which ends the watch. */
export interface ProbeCloseLine {
  readonly at: string;
  readonly type: "close";
  readonly code: number;
  readonly reason: string;
  readonly wait_ms: number | null;
}

/** The watch's end once `--minutes` has passed since the first open, by the clock that open's
 *  line reads. No other end of the watch writes it. */
export interface ProbeEndLine {
  readonly at: string;
  readonly type: "end";
}

/** The ids a GET answer holds that the probe's body lacks (`missing`), and the reverse (`extra`). */
export interface ProbeIdsComparison {
  readonly equal: boolean;
  readonly missing: readonly string[];
  readonly extra: readonly string[];
}

/** One GET route read, against the body the probe holds. `rows` compares a project timeline
 *  page's rows by id, and is null for a route that answers no rows by id; `fields` lists the JSON
 *  path of every other field compared whose value differs. A refused read carries its error. */
export interface ProbeCompareLine {
  readonly at: string;
  readonly type: "compare";
  readonly route: string;
  readonly status: number;
  readonly equal: boolean;
  readonly rows: ProbeIdsComparison | null;
  readonly fields: { readonly equal: boolean; readonly differ: readonly string[] } | null;
  readonly error?: { readonly code: string; readonly message: string };
}

export type ProbeLine =
  | ProbeFrameLine
  | ProbeOpenLine
  | ProbeCloseLine
  | ProbeEndLine
  | ProbeCompareLine;

/** What `--body-file` holds: the last change the probe applied, the body and its revisions. */
export interface ProbeHeld {
  readonly seq: string;
  readonly changed_at: string | null;
  readonly body: DeliveryLiveBody;
  readonly revs: DeliveryLiveRevs;
}

const USAGE =
  "usage: bun e2e/delivery-live-probe.ts --url <dispatch> --token-file <file> --view <json> --minutes <n> [--open-within <seconds>] [--read-within <seconds>] [--compare] [--body-file <file>] [--reconnect-spread-ms <n>]";

/** The probe's one subscription. */
const SUB = "probe";
/** The largest delay `setTimeout` keeps, 2^31-1 ms: a longer one does not fit its 32-bit signed
 *  field, and Bun sets it to 1 ms instead. So the probe refuses a duration flag (`--minutes`,
 *  `--open-within`, `--read-within`) longer than this, whose wait would then end at once, and a
 *  `--reconnect-spread-ms` above it, whose draws could pass it and make the probe reopen at once:
 *  the option honoured as its own opposite. */
export const MAX_TIMER_DELAY_MS = 2_147_483_647;
/** Dispatch closes every socket with this code when it shuts down; the page reconnects within the
 *  first attempt's spread (`reconnectDelayMs(0)`). */
const SERVICE_RESTART = 1012;
/** The code the page closes a silent socket with before it opens another (api/live-socket.ts). */
const REPLACED = 4000;
/** The error codes that refuse what the probe sent: its view or its frame. Waiting cannot change
 *  either answer, so the watch ends; any other code (DELIVERY_NOT_CONFIGURED) is answered again
 *  once Dispatch can, and the watch goes on. */
const REFUSALS: Readonly<Record<string, true>> = { INVALID_FRAME: true, INVALID_QUERY: true };

const MAPS = [
  "issue_titles",
  "session_titles",
  "session_pr_counts",
  "components",
] as const satisfies readonly DeliveryLiveMap[];
const COUNTS = ["facet_counts", "color_counts"] as const satisfies readonly DeliveryLiveCounts[];

type EvalsField = Exclude<keyof EvalListResponse, "freshness">;

/** The evals list's fields, every one but its freshness, whose refresher age the route measures at
 *  the moment of the read (`api/evals.go`); the freshness's other field, the age of the rows the
 *  read folded, is compared on its own. A record, so a field the response gains fails the type
 *  check until it is named here. */
const EVALS_FIELDS: Readonly<Record<EvalsField, true>> = {
  compare: true,
  delivery_freshness: true,
  items: true,
  limit: true,
  offset: true,
  total: true,
  window: true,
};
/** The items read's fields, every one: its rows carry no id, so they are compared whole, as a
 *  field. A record, so a field the response gains fails the type check until it is named here. */
const ITEMS_FIELDS: Readonly<Record<keyof EvalItemsResponse, true>> = {
  compare: true,
  items: true,
  limit: true,
  offset: true,
  total: true,
  window: true,
};

/** The project timeline route's largest page, `projectTimelinePageMax` (api/project_timeline.go).
 *  A view grows past it, so `--compare` reads the rows a view holds a page of this many at a time. */
export const PROJECT_TIMELINE_PAGE_MAX = 500;

type TimelineField = Exclude<
  keyof ProjectTimelineResponse,
  "items" | "limit" | "offset" | "freshness"
>;

/** Which page of the project timeline carries each field a comparison holds equal
 *  (`projectTimelinePage`, api/project_timeline.go): the window, the total and the facet and
 *  colour counts, which are the whole window's, every page; the titles, the window's sessions'
 *  counts, the components and the session groups the first page; and `waiting` the page that
 *  reaches the window's start, which of the pages a view's rows are read in is the last. The
 *  rows are compared by id, a page's limit and offset are the read's own, and the freshness,
 *  which a live body keeps as it was last sent, is not compared. A record, so a field the
 *  response gains fails the type check until it is named here. */
const TIMELINE_FIELDS: Readonly<Record<TimelineField, "every" | "first" | "last">> = {
  color_counts: "every",
  components: "first",
  facet_counts: "every",
  issue_titles: "first",
  session_groups: "first",
  session_pr_counts: "first",
  session_titles: "first",
  total: "every",
  waiting: "last",
  window: "every",
};

const SERVER_FRAME_TYPES: Readonly<Record<DeliveryServerFrame["type"], true>> = {
  delta: true,
  error: true,
  freshness: true,
  heartbeat: true,
  snapshot: true,
};

// Bun's WebSocket client accepts request headers; the repository's DOM WebSocket type does not.
const BunWebSocket = WebSocket as unknown as new (
  url: string,
  options: { readonly headers: Record<string, string> }
) => WebSocket;

class UsageError extends Error {}

interface Options {
  readonly url: URL;
  readonly tokenFile: string;
  readonly view: ProbeView;
  /** How long the watch runs from its first open, in milliseconds. */
  readonly minutesMs: number;
  /** How long the first dial has to open, in milliseconds. */
  readonly openWithinMs: number;
  /** How long each `--compare` read may receive nothing, in milliseconds. */
  readonly readWithinMs: number;
  readonly compare: boolean;
  readonly bodyFile: string | undefined;
  /** The window the first reconnect attempt's wait is drawn from, the page's own by default. */
  readonly reconnectSpreadMs: number;
}

type Sections = Pick<ProbeFrameLine, "session_groups" | "items" | "maps" | "counts" | "set">;

const now = () => new Date().toISOString();
const write = (line: ProbeLine) => console.log(JSON.stringify(line));
const message = (error: unknown) => (error instanceof Error ? error.message : String(error));

function isServerFrame(value: unknown): value is DeliveryServerFrame {
  return (
    typeof value === "object" &&
    value !== null &&
    "type" in value &&
    typeof value.type === "string" &&
    Object.hasOwn(SERVER_FRAME_TYPES, value.type)
  );
}

function mapValues<V, W>(
  record: Readonly<Record<string, V>>,
  fn: (value: V) => W
): Record<string, W> {
  return Object.fromEntries(Object.entries(record).map(([key, value]) => [key, fn(value)]));
}

/** The wait before reconnect attempt `attempt`, with the first attempt's spread set to
 *  `spreadMs`: `reconnectDelayMs` draws that attempt's wait as `random() × RECONNECT_SPREAD_MS`,
 *  so dividing the draw by that window makes the first attempt's spread `spreadMs` without this
 *  file restating the backoff's arithmetic. It is the first attempt's spread alone, as
 *  `--reconnect-spread-ms` says: a later attempt takes the function's own source, so its backoff
 *  and jitter stay the page's whatever the option is set to.
 *
 *  It enforces no bound of its own. `parseOptions` is what refuses a `spreadMs` above
 *  `MAX_TIMER_DELAY_MS`, because a `setTimeout` delay above that is clamped to 1 ms and the probe
 *  would reopen at once, the option honoured as its opposite; a caller that reaches this function
 *  another way gets that defect back with nothing between it and the timer. */
export function reconnectWaitMs(
  attempt: number,
  spreadMs: number,
  random: () => number = Math.random
): number {
  if (attempt !== 0) return reconnectDelayMs(attempt, random);
  return reconnectDelayMs(0, () => random() * (spreadMs / RECONNECT_SPREAD_MS));
}

function readArgs(argv: readonly string[]) {
  try {
    return parseArgs({
      args: [...argv],
      allowPositionals: false,
      options: {
        "body-file": { type: "string" },
        compare: { type: "boolean" },
        minutes: { type: "string" },
        "open-within": { type: "string" },
        "read-within": { type: "string" },
        "reconnect-spread-ms": { type: "string" },
        "token-file": { type: "string" },
        url: { type: "string" },
        view: { type: "string" },
      },
      strict: true,
    }).values;
  } catch (error) {
    throw new UsageError(message(error));
  }
}

function required(value: string | undefined, flag: string): string {
  if (value === undefined || value === "") throw new UsageError(`${flag} is required`);
  return value;
}

/** `raw`, a number of `unit`, in milliseconds: above 0 and no longer than `MAX_TIMER_DELAY_MS`. */
function duration(flag: string, raw: string, unit: "minutes" | "seconds"): number {
  const unitMs = unit === "minutes" ? 60_000 : 1_000;
  const ms = Number(raw) * unitMs;
  if (!Number.isFinite(ms) || ms <= 0) {
    throw new UsageError(`${flag} is a number of ${unit} above 0, not ${raw}`);
  }
  if (ms > MAX_TIMER_DELAY_MS) {
    // Rounded down to six decimals, so the value named is one the flag takes.
    const largest = Math.floor((MAX_TIMER_DELAY_MS / unitMs) * 1e6) / 1e6;
    throw new UsageError(
      `${flag} is at most ${largest} ${unit}, the longest delay a timer keeps, not ${raw}`
    );
  }
  return ms;
}

/** The view `--view` names. The probe checks what it reads itself (the kind, and each view's
 *  project, window and query, and an items view's slug); Dispatch judges the rest and answers a
 *  view it refuses with an error frame. */
function parseView(text: string): ProbeView {
  let value: unknown;
  try {
    value = JSON.parse(text);
  } catch (error) {
    throw new UsageError(`--view is not JSON: ${message(error)}`);
  }
  if (typeof value !== "object" || value === null || Array.isArray(value)) {
    throw new UsageError("--view is a JSON object");
  }
  const kind = "kind" in value ? value.kind : undefined;
  if (kind !== "evals" && kind !== "items" && kind !== "project-timeline") {
    throw new UsageError(
      `--view's "kind" is evals, items or project-timeline, not ${String(kind)}`
    );
  }
  const named = kind === "project-timeline" ? "a project-timeline view" : `an ${kind} view`;
  if (!("project" in value) || typeof value.project !== "string") {
    throw new UsageError(`${named}'s "project" is the key of the project it shows`);
  }
  if (kind === "items" && (!("slug" in value) || typeof value.slug !== "string")) {
    throw new UsageError(`an items view's "slug" is the slug of the eval whose rows it shows`);
  }
  const window = "window" in value ? value.window : undefined;
  if (typeof window !== "object" || window === null) {
    throw new UsageError(
      `${named}'s "window" is {"preset": 7 | 14 | 28 | 84} or {"from": ..., "to": ...}`
    );
  }
  if (!("query" in value) || typeof value.query !== "string") {
    throw new UsageError(
      `${named}'s "query" is the GET route's query string without from and to ("" for none)`
    );
  }
  // Checked above as far as the probe reads it; Dispatch refuses the rest of a malformed view.
  const view = value as ProbeView;
  return view;
}

function parseOptions(argv: readonly string[]): Options {
  const values = readArgs(argv);
  const rawUrl = required(values.url, "--url");
  let url: URL;
  try {
    url = new URL(rawUrl);
  } catch {
    throw new UsageError(`--url is not a URL: ${rawUrl}`);
  }
  if (url.protocol !== "http:" && url.protocol !== "https:") {
    throw new UsageError(`--url is Dispatch's http or https address, not ${rawUrl}`);
  }
  const rawOpenWithin = values["open-within"];
  const rawReadWithin = values["read-within"];
  const rawSpread = values["reconnect-spread-ms"];
  const reconnectSpreadMs = rawSpread === undefined ? RECONNECT_SPREAD_MS : Number(rawSpread);
  if (
    rawSpread?.trim() === "" ||
    !Number.isFinite(reconnectSpreadMs) ||
    reconnectSpreadMs < 0 ||
    reconnectSpreadMs > MAX_TIMER_DELAY_MS
  ) {
    throw new UsageError(
      "--reconnect-spread-ms is a number of milliseconds from 0 to " +
        `${MAX_TIMER_DELAY_MS}, not ${rawSpread}`
    );
  }
  return {
    bodyFile: values["body-file"],
    compare: values.compare === true,
    minutesMs: duration("--minutes", required(values.minutes, "--minutes"), "minutes"),
    openWithinMs:
      rawOpenWithin === undefined
        ? LIVE_SOCKET_SILENT_MS
        : duration("--open-within", rawOpenWithin, "seconds"),
    readWithinMs:
      rawReadWithin === undefined
        ? REQUEST_DEADLINE_MS
        : duration("--read-within", rawReadWithin, "seconds"),
    reconnectSpreadMs,
    tokenFile: required(values["token-file"], "--token-file"),
    url,
    view: parseView(required(values.view, "--view")),
  };
}

async function readToken(file: string): Promise<string> {
  let token: string;
  try {
    token = (await Bun.file(file).text()).trim();
  } catch (error) {
    throw new UsageError(`cannot read --token-file ${file}: ${message(error)}`);
  }
  if (token === "") throw new UsageError(`--token-file ${file} holds no token`);
  return token;
}

function listChanges(
  changes:
    | {
        readonly upsert: readonly { readonly id: string | number }[];
        readonly remove: readonly string[];
      }
    | undefined
): ProbeListChanges | null {
  if (changes === undefined) return null;
  return { remove: [...changes.remove], upsert: changes.upsert.map((item) => item.id) };
}

/** What a snapshot's body holds, counted, by the view it answers: a project-timeline body has two
 *  lists, its rows and its session groups, maps and counts; an evals body one list, its evals; and
 *  an items body its rows. Dispatch answers each view with its own route's body (packages/contracts
 *  DeliveryLiveView): the frame carries the union, and the view's kind names the member. */
function holds(view: ProbeView, body: DeliveryLiveBody): Sections {
  const none: Sections = { counts: null, items: null, maps: null, session_groups: null, set: null };
  switch (view.kind) {
    case "evals":
      return { ...none, items: ((body as EvalListResponse).items ?? []).length };
    case "items":
      return { ...none, items: ((body as EvalItemsResponse).items ?? []).length };
    case "project-timeline": {
      const timeline = body as ProjectTimelineResponse;
      return {
        ...none,
        counts: Object.fromEntries(
          COUNTS.map((name) => [
            name,
            mapValues(timeline[name] ?? {}, (values) => Object.keys(values).length),
          ])
        ),
        items: (timeline.items ?? []).length,
        maps: Object.fromEntries(
          MAPS.map((name) => [name, Object.keys(timeline[name] ?? {}).length])
        ),
        session_groups: (timeline.session_groups ?? []).length,
      };
    }
  }
}

/** What a delta changes, by id and key. */
function changes(delta: DeliveryLiveDelta): Sections {
  return {
    counts: mapValues(delta.counts, (facets) =>
      mapValues(facets ?? {}, (values) => Object.keys(values))
    ),
    maps: mapValues(delta.maps, (entries) => Object.keys(entries ?? {})),
    session_groups: listChanges(delta.items.session_groups),
    items: listChanges(delta.items.items),
    set: Object.keys(delta.set),
  };
}

function frameLine(view: ProbeView, frame: DeliveryServerFrame, bytes: number): ProbeFrameLine {
  const line: ProbeFrameLine = {
    at: now(),
    bytes,
    changed_at: null,
    counts: null,
    items: null,
    maps: null,
    seq: null,
    session_groups: null,
    set: null,
    type: frame.type,
  };
  switch (frame.type) {
    case "snapshot":
      return { ...line, ...holds(view, frame.body), changed_at: frame.changed_at, seq: frame.seq };
    case "delta":
      return { ...line, ...changes(frame), changed_at: frame.changed_at, seq: frame.seq };
    case "error":
      return { ...line, code: frame.code, message: frame.message };
    case "freshness":
      return { ...line, freshness: frame.freshness, stale: frame.stale };
    case "heartbeat":
      return line;
  }
}

/** Watches `options.view` until `--minutes` pass from its socket's first open, or a Ctrl-C or
 *  SIGTERM, and resolves with the body the probe holds then, or null when no snapshot arrived.
 *  Rejects when the first connection is refused or does not open within `--open-within`,
 *  Dispatch refuses the view, or a frame breaks the contract. */
async function watch(options: Options, token: string): Promise<ProbeHeld | null> {
  const liveUrl = new URL("/ws/live", options.url);
  liveUrl.protocol = options.url.protocol === "https:" ? "wss:" : "ws:";
  let held: ProbeHeld | null = null;
  let socket: WebSocket | null = null;
  let attempt = 0;
  let lastFrameAt = 0;
  let cancelReconnect: (() => void) | undefined;
  // Armed at the first dial for `--open-within`, which fails the watch; the first open arms it
  // again for `--minutes` from then, which ends the watch cleanly.
  let timer: Timer | undefined;
  // Whether the first open has come; the time `--minutes` counts from goes to `timeUp` as its
  // argument.
  let opened = false;
  // Settles once, with why the watch ended early, or with nothing once its time is up.
  const ended = Promise.withResolvers<Error | undefined>();
  const fail = (reason: string) => ended.resolve(new Error(reason));
  const stop = () => ended.resolve(undefined);
  // `--minutes` is up only once that much of this clock has passed since the first open, at
  // `from`; a timer that fires early is armed again for what is left.
  const timeUp = (from: number) => {
    const at = Date.now();
    const left = from + options.minutesMs - at;
    if (left > 0) {
      timer = setTimeout(timeUp, left, from);
      return;
    }
    write({ at: new Date(at).toISOString(), type: "end" });
    stop();
  };

  const subscribeFrame = (): LiveSubscribe =>
    held === null
      ? { sub: SUB, type: "subscribe", view: options.view }
      : {
          resume: { revs: held.revs, seq: held.seq },
          sub: SUB,
          type: "subscribe",
          view: options.view,
        };

  const apply = (frame: DeliveryServerFrame) => {
    if (frame.type === "error") {
      if (frame.sub === SUB && Object.hasOwn(REFUSALS, frame.code)) {
        fail(`Dispatch refused the view: ${frame.code}: ${frame.message}`);
      }
      return;
    }
    if (frame.type !== "snapshot" && frame.type !== "delta") return;
    if (frame.sub !== SUB) {
      fail(`a ${frame.type} for subscription ${frame.sub}, which the probe never made`);
      return;
    }
    if (frame.type === "snapshot") {
      held = { body: frame.body, changed_at: frame.changed_at, revs: frame.revs, seq: frame.seq };
      return;
    }
    if (held === null) {
      fail("a delta arrived before its subscription's snapshot");
      return;
    }
    held = {
      body: applyDelta(held.body, frame),
      changed_at: frame.changed_at,
      revs: applyRevs(held.revs, frame.revs),
      seq: frame.seq,
    };
  };

  const receive = (data: unknown) => {
    if (typeof data !== "string") {
      fail("Dispatch sent a binary frame; the live route sends text");
      return;
    }
    lastFrameAt = Date.now();
    attempt = 0;
    let frame: unknown;
    try {
      frame = JSON.parse(data);
    } catch (error) {
      fail(`a frame that is not JSON (${message(error)})`);
      return;
    }
    if (!isServerFrame(frame)) {
      fail(`a frame of no type the contract names: ${data.slice(0, 200)}`);
      return;
    }
    write(frameLine(options.view, frame, Buffer.byteLength(data)));
    apply(frame);
  };

  const open = () => {
    cancelReconnect = undefined;
    lastFrameAt = Date.now();
    const next = new BunWebSocket(liveUrl.href, { headers: { Authorization: `Bearer ${token}` } });
    socket = next;
    // Each listener ignores a socket the probe has already let go, whose events still arrive.
    next.addEventListener("open", () => {
      if (socket !== next) return;
      const at = Date.now();
      if (!opened) {
        clearTimeout(timer);
        opened = true;
        timer = setTimeout(timeUp, options.minutesMs, at);
      }
      lastFrameAt = at;
      write({ at: new Date(at).toISOString(), resume: held?.seq ?? null, type: "open" });
      next.send(JSON.stringify(subscribeFrame()));
    });
    next.addEventListener("message", (event) => {
      if (socket === next) receive(event.data);
    });
    next.addEventListener("close", (event) => {
      if (socket === next) lost(event.code, event.reason);
    });
  };

  const lost = (code: number, reason: string) => {
    socket = null;
    // The close line carries the wait the probe computed before it would open again, so a reader
    // sees what `--reconnect-spread-ms` and the backoff came to; a close answered with no timer
    // reports none.
    const closed = (waitMs: number | null) =>
      write({ at: now(), code, reason, type: "close", wait_ms: waitMs });
    if (!opened) {
      closed(null);
      fail(`Dispatch refused the connection to ${liveUrl.href}: ${reason || `close ${code}`}`);
      return;
    }
    if (code === LIVE_SOCKET_MAX_AGE_CLOSE) {
      closed(null);
      open();
      return;
    }
    // As the page does: Dispatch's restart close starts the backoff over, so the next attempt
    // waits within the first attempt's spread, which `--reconnect-spread-ms` sets.
    if (code === SERVICE_RESTART) attempt = 0;
    const delayMs = reconnectWaitMs(attempt, options.reconnectSpreadMs);
    closed(delayMs);
    const handle = setTimeout(open, delayMs);
    attempt += 1;
    cancelReconnect = () => clearTimeout(handle);
  };

  // After the first open, a socket that carried no frame for three heartbeats is dead without
  // knowing it: replace it. Before the first open, `--open-within` judges the dial.
  const silence = setInterval(() => {
    if (!opened || socket === null || Date.now() - lastFrameAt <= LIVE_SOCKET_SILENT_MS) {
      return;
    }
    const silent = socket;
    socket = null;
    const reason = `no frame for ${LIVE_SOCKET_SILENT_MS / 1000} s`;
    write({ at: now(), code: REPLACED, reason, type: "close", wait_ms: null });
    silent.close(REPLACED, reason);
    open();
  }, 1_000);
  process.once("SIGINT", stop);
  process.once("SIGTERM", stop);
  const shutDown = () => {
    clearTimeout(timer);
    clearInterval(silence);
    cancelReconnect?.();
    process.off("SIGINT", stop);
    process.off("SIGTERM", stop);
    const closing = socket;
    socket = null;
    closing?.close(1000);
  };

  timer = setTimeout(
    () =>
      fail(
        `the socket to ${liveUrl.href} did not open within ${options.openWithinMs / 1000} s (--open-within)`
      ),
    options.openWithinMs
  );
  open();
  const failure = await ended.promise;
  shutDown();
  if (failure !== undefined) throw failure;
  return held;
}

/** The comparison of one route's answer with the body the probe holds. */
type Checked = Pick<ProbeCompareLine, "rows" | "fields">;

interface Read {
  readonly route: string;
  readonly check: (answer: unknown) => Checked;
}

function read<A>(route: string, check: (answer: A) => Checked): Read {
  // The route answers its own body, as its contract types it; the check reads the fields it names.
  return { check: (answer) => check(answer as A), route };
}

function compareIds(
  held: readonly (string | number)[],
  answer: readonly (string | number)[]
): ProbeIdsComparison {
  const mine = new Set(held.map(String));
  const theirs = new Set(answer.map(String));
  const missing = [...theirs].filter((id) => !mine.has(id)).sort();
  const extra = [...mine].filter((id) => !theirs.has(id)).sort();
  return { equal: missing.length === 0 && extra.length === 0, extra, missing };
}

/** The JSON paths at or below `path` where `a` and `b` differ; an array whose length differs is
 *  one difference, at its own path. */
function differences(a: unknown, b: unknown, path: string): string[] {
  if (a === b) return [];
  if (Array.isArray(a) && Array.isArray(b)) {
    if (a.length !== b.length) return [path];
    return a.flatMap((item, index) => differences(item, b[index], `${path}[${index}]`));
  }
  if (
    typeof a === "object" &&
    a !== null &&
    !Array.isArray(a) &&
    typeof b === "object" &&
    b !== null &&
    !Array.isArray(b)
  ) {
    const left = new Map<string, unknown>(Object.entries(a));
    const right = new Map<string, unknown>(Object.entries(b));
    const keys = [...new Set([...left.keys(), ...right.keys()])].sort();
    return keys.flatMap((key) => differences(left.get(key), right.get(key), `${path}.${key}`));
  }
  return [path];
}

function compareFields<F extends string>(
  held: Readonly<Record<F, unknown>>,
  answer: Readonly<Record<F, unknown>>,
  names: readonly F[]
): NonNullable<Checked["fields"]> {
  const differ = names.flatMap((field) => differences(held[field], answer[field], field));
  return { differ, equal: differ.length === 0 };
}

/** The names a record of fields holds: exactly the keys its type names, since it is a literal. */
function namesOf<F extends string>(fields: Readonly<Record<F, unknown>>): F[] {
  return Object.keys(fields) as F[];
}

/** The evals list's comparison: its fields but the freshness, and the freshness's rows' age, which
 *  is reported under `freshness.` so a reader sees where the difference is. */
function compareEvals(
  held: EvalListResponse,
  answer: EvalListResponse
): NonNullable<Checked["fields"]> {
  const fields = compareFields(held, answer, namesOf(EVALS_FIELDS));
  const rows = differences(
    held.freshness.rows_updated_at,
    answer.freshness.rows_updated_at,
    "freshness.rows_updated_at"
  );
  const differ = [...fields.differ, ...rows];
  return { differ, equal: differ.length === 0 };
}

/** `base`, a view's query, with the window and the other keys `extra` names set. */
function query(
  base: string,
  window: { readonly from: string; readonly to: string },
  extra: Readonly<Record<string, string>> = {}
): string {
  const params = new URLSearchParams(base);
  params.set("from", window.from);
  params.set("to", window.to);
  for (const [key, value] of Object.entries(extra)) params.set(key, value);
  return params.toString();
}

/** An evals or items view's window as Dispatch read it, a preset's days ending now, and never the
 *  body's: those reads answer the whole UTC days a window touches, so a preset of 84 days, which
 *  touches 85, holds a window the route refuses to be sent. The route folds the same days, and the
 *  `window` it answers is compared with the rest. */
function ownWindow(view: ProbeView): { readonly from: string; readonly to: string } {
  if (!("preset" in view.window)) return view.window;
  const end = Date.now();
  return {
    from: new Date(end - view.window.preset * DAY_MS).toISOString(),
    to: new Date(end).toISOString(),
  };
}

/** The project timeline's pages that hold the rows `timeline` holds, newest first, each
 *  `PROJECT_TIMELINE_PAGE_MAX` rows at most and one page where it holds none, read over the window
 *  the body last applied with the view's own query: each page's rows by id against the rows the
 *  body holds at its offset, and each field that page carries (`TIMELINE_FIELDS`). */
function timelineReads(view: ProbeView, timeline: ProjectTimelineResponse): Read[] {
  const rows = timeline.items ?? [];
  const reads: Read[] = [];
  for (let offset = 0; offset === 0 || offset < rows.length; offset += PROJECT_TIMELINE_PAGE_MAX) {
    const limit = Math.min(PROJECT_TIMELINE_PAGE_MAX, Math.max(timeline.limit - offset, 1));
    const last = offset + PROJECT_TIMELINE_PAGE_MAX >= rows.length;
    const carried = namesOf(TIMELINE_FIELDS).filter((field) => {
      const page = TIMELINE_FIELDS[field];
      return page === "every" || (page === "first" ? offset === 0 : last);
    });
    const params = query(view.query, timeline.window, {
      limit: String(limit),
      offset: String(offset),
    });
    reads.push(
      read<ProjectTimelineResponse>(
        `/api/v1/projects/${encodeURIComponent(view.project)}/timeline?${params}`,
        (answer) => ({
          fields: compareFields(timeline, answer, carried),
          rows: compareIds(
            rows.slice(offset, offset + limit).map((row) => row.id),
            (answer.items ?? []).map((row) => row.id)
          ),
        })
      )
    );
  }
  return reads;
}

/** The GET routes that answer what `body` holds: the project timeline's pages for the window it
 *  last applied; the evals list and an eval's rows for the view's own window. Dispatch answers
 *  each view with its own route's body (packages/contracts DeliveryLiveView), which the view's
 *  kind names. */
function readsFor(view: ProbeView, body: DeliveryLiveBody): Read[] {
  const project = encodeURIComponent(view.project);
  switch (view.kind) {
    case "project-timeline":
      return timelineReads(view, body as ProjectTimelineResponse);
    case "evals":
      return [
        read<EvalListResponse>(
          `/api/v1/projects/${project}/evals?${query(view.query, ownWindow(view))}`,
          (answer) => ({ fields: compareEvals(body as EvalListResponse, answer), rows: null })
        ),
      ];
    case "items": {
      const slug = encodeURIComponent(view.slug);
      return [
        read<EvalItemsResponse>(
          `/api/v1/projects/${project}/evals/${slug}/items?${query(view.query, ownWindow(view))}`,
          (answer) => ({
            fields: compareFields(body as EvalItemsResponse, answer, namesOf(ITEMS_FIELDS)),
            rows: null,
          })
        ),
      ];
    }
  }
}

/** One route read and compared. Dispatch has `withinMs` to send something, as the page gives a
 *  GET: `readUntilSilent` starts the time over as the headers arrive and as each piece of the body
 *  does, so a body that keeps coming is read however slowly, and one that stops fails the read. */
async function compareRead(
  base: URL,
  token: string,
  route: Read,
  withinMs: number
): Promise<ProbeCompareLine> {
  const { response, text } = await readUntilSilent(
    route.route,
    (signal) =>
      fetch(new URL(route.route, base), {
        headers: { Authorization: `Bearer ${token}` },
        signal,
      }),
    withinMs
  );
  const refused = (code: string, reason: string): ProbeCompareLine => ({
    at: now(),
    equal: false,
    error: { code, message: reason },
    fields: null,
    route: route.route,
    rows: null,
    status: response.status,
    type: "compare",
  });
  let answer: unknown;
  try {
    answer = JSON.parse(text);
  } catch {
    return refused(response.ok ? "NOT_JSON" : `HTTP_${response.status}`, text.slice(0, 200));
  }
  if (!response.ok) {
    if (typeof answer !== "object" || answer === null) {
      return refused(`HTTP_${response.status}`, text.slice(0, 200));
    }
    return refused(
      "code" in answer && typeof answer.code === "string" ? answer.code : `HTTP_${response.status}`,
      "error" in answer && typeof answer.error === "string" ? answer.error : text.slice(0, 200)
    );
  }
  const checked = route.check(answer);
  const equal = [checked.rows, checked.fields].every((part) => part === null || part.equal);
  return {
    at: now(),
    equal,
    ...checked,
    route: route.route,
    status: response.status,
    type: "compare",
  };
}

/** Reads each GET route for `held`'s window and writes its comparison; true when all are equal. */
async function compare(options: Options, token: string, held: ProbeHeld): Promise<boolean> {
  let equal = true;
  for (const route of readsFor(options.view, held.body)) {
    const line = await compareRead(options.url, token, route, options.readWithinMs);
    write(line);
    equal &&= line.equal;
  }
  return equal;
}

async function main(argv: readonly string[]): Promise<number> {
  let options: Options;
  let token: string;
  try {
    options = parseOptions(argv);
    token = await readToken(options.tokenFile);
  } catch (error) {
    if (!(error instanceof UsageError)) throw error;
    console.error(`${error.message}\n${USAGE}`);
    return 2;
  }
  try {
    const held = await watch(options, token);
    if (options.bodyFile !== undefined) {
      writeFileSync(options.bodyFile, `${JSON.stringify(held)}\n`);
    }
    if (!options.compare) return 0;
    if (held === null) {
      console.error("delivery-live-probe: nothing to compare: no snapshot arrived");
      return 1;
    }
    return (await compare(options, token, held)) ? 0 : 1;
  } catch (error) {
    console.error(`delivery-live-probe: ${message(error)}`);
    return 1;
  }
}

// The test imports `reconnectWaitMs` and `MAX_TIMER_DELAY_MS` from this file: the command line
// runs only when bun runs the probe itself.
if (import.meta.main) {
  process.exit(await main(Bun.argv.slice(2)));
}
