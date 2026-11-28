// What the Evals tab's browser measurements share (dispatch://INFRA-2918, T27, D16): Playwright's
// Chromium, resolved from packages/dispatch, which locks it; a signed-in reader's page of the tab
// with the instrument that times it; the page's own live frames, replayed into what each of its
// views holds; the server's debug lines, which time the server's steps (its DISPATCH_LOG_LEVEL is
// debug under run.sh's `tab` in a split run, EVALBENCH_SPLIT=1); and the database the bench's
// Postgres serves. paint.ts and recompute.ts are the two steps that read it, run by
// evalbench/run.sh's `tab` command with bun.
//
// The split's instruments (each step's `--split 1`) explain a run's numbers and cost the page and
// the server time of their own: a plain run, which judges every bound, runs none of them (the
// architect's ruling on T33's split, dispatch://INFRA-2918/comment/b286546b-9d3c-4361-a987-dc1b3fdc4018).
import { readFileSync, statSync } from "node:fs";
import { createRequire } from "node:module";
import { join, resolve } from "node:path";
import { gzipSync } from "node:zlib";
import { SQL } from "bun";

/** The flags of a command line, `--name value` each. */
export function flags(argv: readonly string[]): Map<string, string> {
  const out = new Map<string, string>();
  for (let index = 0; index < argv.length; index += 2) {
    const name = argv[index] ?? "";
    if (!name.startsWith("--") || index + 1 >= argv.length) {
      throw new Error(`flags come as --name value pairs, and ${JSON.stringify(name)} is none`);
    }
    out.set(name.slice(2), argv[index + 1] ?? "");
  }
  return out;
}

/** The value of a flag that must be named. */
export function need(given: Map<string, string>, name: string): string {
  const value = given.get(name);
  if (value === undefined || value === "") throw new Error(`name --${name}`);
  return value;
}

/** A number a flag names, or the default where it names none. */
export function numberOf(given: Map<string, string>, name: string, fallback: number): number {
  const value = given.get(name);
  if (value === undefined) return fallback;
  const parsed = Number(value);
  if (!Number.isFinite(parsed))
    throw new Error(`--${name} is a number, not ${JSON.stringify(value)}`);
  return parsed;
}

/** Whether the step runs the split's instruments (`--split 1`; `--split 0` or none runs none): the
 *  page instrument's recording of the subscribes, the app's marks, the long tasks and the socket's
 *  opening (instrument), and the reads of the server's debug lines, which only a server at debug
 *  writes. */
export function splitOf(given: Map<string, string>): boolean {
  const value = given.get("split") ?? "0";
  if (value !== "0" && value !== "1")
    throw new Error(`--split is 1 or 0, not ${JSON.stringify(value)}`);
  return value === "1";
}

let failures = 0;

/** Prints one measurement, as every evalbench step prints its own. */
export function result(line: string): void {
  console.log(`RESULT ${line}`);
}

/** Prints one measurement that missed its budget or its rule, and marks the step failed. */
export function fail(line: string): void {
  failures++;
  console.log(`FAIL ${line}`);
}

/** Prints what the step did, for the reader of the log. */
export function mark(line: string): void {
  console.log(`MARK ${new Date().toISOString()} ${line}`);
}

/** How many FAIL lines the step printed. */
export function failed(): number {
  return failures;
}

/** Waits ms. */
export function sleep(ms: number): Promise<void> {
  const { promise, resolve } = Promise.withResolvers<void>();
  setTimeout(resolve, ms);
  return promise;
}

/** How many bytes text takes compressed, at gzip's default level, as the bench's other reads
 *  measure a body (measure's gzipSize). */
export function gzipBytes(text: string): number {
  return gzipSync(Buffer.from(text)).length;
}

/** The median and the other ranks of xs, null where it holds none. */
export function rank(xs: readonly number[], q: number): number | null {
  if (xs.length === 0) return null;
  const sorted = [...xs].sort((a, b) => a - b);
  return sorted[Math.min(sorted.length - 1, Math.floor(q * sorted.length))] ?? null;
}

/** A number for a RESULT line: one decimal, or "none". */
export function ms(value: number | null | undefined): string {
  return value === null || value === undefined || !Number.isFinite(value)
    ? "none"
    : value.toFixed(1);
}

// ---------------------------------------------------------------------------------------------
// Chromium

interface Playwright {
  chromium: {
    launch(options: Record<string, unknown>): Promise<Browser>;
  };
}

export interface Browser {
  newContext(options: Record<string, unknown>): Promise<BrowserContext>;
  close(): Promise<void>;
  version(): string;
}

export interface BrowserContext {
  addInitScript(script: { content: string }): Promise<void>;
  newPage(): Promise<Page>;
  request: {
    get(url: string, options: Record<string, unknown>): Promise<{ status(): number }>;
  };
  close(): Promise<void>;
}

export interface WebSocketHandle {
  url(): string;
  on(
    event: "framereceived" | "framesent",
    listener: (frame: { payload: string | Buffer }) => void
  ): void;
  on(event: "close", listener: () => void): void;
}

export interface Page {
  goto(url: string, options: Record<string, unknown>): Promise<unknown>;
  on(event: "websocket", listener: (socket: WebSocketHandle) => void): void;
  on(event: "request", listener: (request: { url(): string; method(): string }) => void): void;
  evaluate<T>(script: string): Promise<T>;
  waitForFunction(script: string, arg: unknown, options: Record<string, unknown>): Promise<unknown>;
  click(selector: string, options?: Record<string, unknown>): Promise<void>;
  close(): Promise<void>;
}

/** The directory of packages/dispatch, whose lock names the Playwright the bench drives. */
function dispatchDir(): string {
  return (
    process.env.EVALBENCH_DISPATCH_DIR ?? resolve(import.meta.dir, "../../../../../..", "dispatch")
  );
}

/** Launches Chromium, headless, through the Playwright packages/dispatch locks: the build
 *  Playwright installed (`bunx playwright install chromium`), or the one EVALBENCH_CHROMIUM names. */
export async function launchChromium(): Promise<Browser> {
  const requireFromDispatch = createRequire(join(dispatchDir(), "package.json"));
  // Imported by the path packages/dispatch resolves: Bun's isolated linker installs Playwright
  // under that package alone, so no static import from this directory finds it.
  const playwright: Playwright = await import(requireFromDispatch.resolve("@playwright/test"));
  const executablePath = process.env.EVALBENCH_CHROMIUM;
  return playwright.chromium.launch({
    headless: true,
    ...(executablePath ? { executablePath } : {}),
    args: ["--disable-dev-shm-usage"],
  });
}

/** The instrument each page of the tab runs before any of the app's own scripts. A plain run's
 *  records what the bounds are judged by: when each section of the first paint (`[data-section]`,
 *  the timeline's left out) first reads `data-state="painted"`, when each of the CI section's job
 *  lists (`[data-job-list]`) first holds no loading line, when the narrowed cards' loading mark
 *  (`[data-narrowing="loading"]`) shows and when it gives way to the narrowed values, and for every
 *  frame of the page's live socket (`/ws/live`), when it arrived and when the page had painted
 *  after it. A split run's (`split`) records besides: when the page made each live socket and when
 *  it opened (`opened`); when the page sent each subscribe frame, by its sub (`sends`); every
 *  performance mark the app sets that is named `dispatch:…` (`marks`): a snapshot's parse ended
 *  (`dispatch:live-snapshot:<sub>`, api/live-socket.ts), and a section or a CI job list committed
 *  its drawing (`dispatch:evals-section:<id>`, `dispatch:evals-job-list:<id>`, useDrawnMark.ts);
 *  and every long task of the page's main thread, a task of 50 ms or more, with its start and
 *  length (`longTasks`), which says whether the thread was busy between two frames. Every time of
 *  a DOM change is read twice: `at`, the change, and `painted`, two animation frames later, once
 *  the browser has presented it. Times are milliseconds since the epoch, so they compare with the
 *  database's commit timestamps and the server's log; the navigation's own start is `start`. */
export function instrument(split: boolean): string {
  const splitParts = !split
    ? ""
    : `
  record.opened = []; record.sends = []; record.marks = []; record.longTasks = [];
  const SUBSCRIBE = '{"type":"subscribe","sub":"';
  const send = Measured.prototype.send;
  Measured.prototype.send = function (data) {
    if (this.__evalbenchSocket !== undefined && typeof data === "string" && data.startsWith(SUBSCRIBE)) {
      const end = data.indexOf('"', SUBSCRIBE.length);
      if (end > 0) record.sends.push({ socket: this.__evalbenchSocket, sub: data.slice(SUBSCRIBE.length, end), at: now() });
    }
    return send.call(this, data);
  };
  opened = (socket, ws) => {
    const entry = { socket, made: now() };
    record.opened.push(entry);
    ws.addEventListener("open", () => { entry.open = now(); });
  };
  new PerformanceObserver((list) => {
    for (const entry of list.getEntries())
      if (entry.name.startsWith("dispatch:")) record.marks.push({ name: entry.name, at: performance.timeOrigin + entry.startTime });
  }).observe({ type: "mark", buffered: true });
  new PerformanceObserver((list) => {
    for (const entry of list.getEntries())
      record.longTasks.push({ at: performance.timeOrigin + entry.startTime, ms: entry.duration });
  }).observe({ type: "longtask", buffered: true });`;
  return `(() => {
  const record = { start: performance.timeOrigin, sections: {}, jobLists: {}, narrowing: {}, frames: [], sockets: 0 };
  window.__evalbench = record;
  const now = () => performance.timeOrigin + performance.now();
  const stamp = (entry) => {
    entry.at = now();
    requestAnimationFrame(() => requestAnimationFrame(() => { entry.painted = now(); }));
    return entry;
  };
  let opened = null;
  const Native = window.WebSocket;
  class Measured extends Native {
    constructor(url, protocols) {
      super(url, protocols);
      if (!String(url).includes("/ws/live")) return;
      const socket = ++record.sockets;
      this.__evalbenchSocket = socket;
      if (opened !== null) opened(socket, this);
      let index = 0;
      this.addEventListener("message", () => {
        const entry = { socket, index: index++ };
        record.frames.push(entry);
        stamp(entry);
      });
    }
  }
  window.WebSocket = Measured;${splitParts}
  const check = () => {
    for (const element of document.querySelectorAll("[data-section]")) {
      const id = element.getAttribute("data-section");
      const state = element.getAttribute("data-state");
      const seen = record.sections[id];
      if (seen === undefined) record.sections[id] = { state };
      else seen.state = state;
      if ((state === "painted" || state === "failed") && record.sections[id].at === undefined) stamp(record.sections[id]);
    }
    for (const element of document.querySelectorAll("[data-job-list]")) {
      const id = element.getAttribute("data-job-list");
      if (record.jobLists[id] !== undefined) continue;
      if (element.querySelector('[role="status"]') !== null) continue;
      record.jobLists[id] = stamp({ failed: (element.textContent || "").includes("could not load") });
    }
    const loading = document.querySelector('[data-narrowing="loading"]') !== null;
    if (loading && record.narrowing.loading === undefined) record.narrowing.loading = stamp({});
    if (!loading && record.narrowing.loading !== undefined && record.narrowing.narrowed === undefined
      && document.querySelector('[data-narrowing="narrowed"]') !== null) record.narrowing.narrowed = stamp({});
  };
  new MutationObserver(check).observe(document, { subtree: true, childList: true, attributes: true,
    attributeFilter: ["data-state", "data-narrowing", "role"] });
})();`;
}

/** One time the instrument read: the DOM change and the presentation after it. */
export interface Stamp {
  at?: number;
  painted?: number;
}

/** One `dispatch:` performance mark the app set, at its moment in milliseconds since the epoch. */
export interface Mark {
  name: string;
  at: number;
}

/** What the instrument holds of one page; the split's records are empty in a plain run. */
export interface Instrumented {
  start: number;
  sections: Record<string, Stamp & { state: string }>;
  jobLists: Record<string, Stamp & { failed: boolean }>;
  narrowing: { loading?: Stamp; narrowed?: Stamp };
  frames: (Stamp & { socket: number; index: number })[];
  /** Each live socket the page made, when it made it and when it opened (none before). */
  opened: { socket: number; made: number; open?: number }[];
  /** Each subscribe frame the page sent, by its socket and sub, when it sent it. */
  sends: { socket: number; sub: string; at: number }[];
  /** Every `dispatch:` mark set, the ones the app has cleared since among them, each once. */
  marks: Mark[];
  /** Every long task of the page's main thread: when it began and how long it ran. */
  longTasks: { at: number; ms: number }[];
}

/** Reads the instrument's record off the page, its frames from `fromFrame` on: in a split run, the
 *  marks its observer has been handed so far and every mark the page's timeline still holds, which
 *  an observer not yet called has not been handed. */
export async function readInstrument(page: Page, fromFrame = 0): Promise<Instrumented> {
  const record = await page.evaluate<Instrumented>(
    `(() => { const r = window.__evalbench; const split = r.marks !== undefined;
       const held = !split ? [] : performance.getEntriesByType("mark").filter((entry) => entry.name.startsWith("dispatch:"))
         .map((entry) => ({ name: entry.name, at: performance.timeOrigin + entry.startTime }));
       return { start: r.start, sections: r.sections, jobLists: r.jobLists, narrowing: r.narrowing,
         frames: r.frames.slice(${fromFrame}), opened: r.opened || [], sends: r.sends || [],
         marks: (r.marks || []).concat(held), longTasks: r.longTasks || [] }; })()`
  );
  const seen = new Set<string>();
  record.marks = record.marks.filter((one) => {
    const key = `${one.name} ${one.at}`;
    if (seen.has(key)) return false;
    seen.add(key);
    return true;
  });
  return record;
}

/** The first moment the mark `name` was set on a page, in milliseconds since the epoch. */
export function firstMark(record: Instrumented, name: string): number | undefined {
  let first: number | undefined;
  for (const one of record.marks)
    if (one.name === name && (first === undefined || one.at < first)) first = one.at;
  return first;
}

/** How long the page's long tasks ran within [from, to), milliseconds since the epoch: the main
 *  thread's time busy in tasks of 50 ms or more, which no frame's handler could run beside. */
export function busyWithin(record: Instrumented, from: number, to: number): number {
  let busy = 0;
  for (const task of record.longTasks)
    busy += Math.max(0, Math.min(to, task.at + task.ms) - Math.max(from, task.at));
  return busy;
}

/** A signed-in reader's browser context: the instrument on every page it opens, a split run's where
 *  `split`, and the session cookie of the server's dev sign-in (signIn), so no page loads before
 *  the tab. */
export async function readerContext(
  browser: Browser,
  base: string,
  login: string,
  split: boolean
): Promise<BrowserContext> {
  const context = await browser.newContext({
    baseURL: base,
    viewport: { width: 1440, height: 1000 },
  });
  await context.addInitScript({ content: instrument(split) });
  await signIn(context, base, login);
  return context;
}

/** Signs `login` in to the server at `base` in `context`: the session cookie of its dev sign-in
 *  (DISPATCH_DEV_SIGNIN=1), read through the context's own request client. A restarted server is
 *  signed in to again, since the cookie its last process signed need not be one it reads. */
export async function signIn(context: BrowserContext, base: string, login: string): Promise<void> {
  const answer = await context.request.get(
    `${base}/auth/_dev/signin?login=${encodeURIComponent(login)}`,
    {
      maxRedirects: 0,
    }
  );
  if (answer.status() !== 302 && answer.status() !== 303) {
    throw new Error(
      `the dev sign-in answered ${answer.status()}, not a redirect with the session cookie`
    );
  }
}

// ---------------------------------------------------------------------------------------------
// The page's live frames

/** A view a page subscribed, as its subscribe frame named it. */
export interface View {
  kind: string;
  [field: string]: unknown;
}

/** One frame of a page's live socket, as the page received it. */
export interface Frame {
  socket: number;
  index: number;
  /** When the bench's process heard it, milliseconds since the epoch. */
  heard: number;
  text: string;
  parsed: Record<string, unknown> | null;
}

/** Everything a page's live socket carried: the views it subscribed, by sub, when the bench's
 *  process heard the page send each one's first subscribe (milliseconds since the epoch, as a
 *  frame's `heard`, both through the browser's own record of its socket, which costs the page
 *  nothing), and, where `keep`, each frame it received, in order (a long drive keeps none, since
 *  its frames run to gigabytes). `onFrame` sees each frame as it is heard. */
export class LiveLog {
  readonly views = new Map<string, View>();
  readonly subscribed = new Map<string, number>();
  readonly frames: Frame[] = [];
  onFrame: ((frame: Frame) => void) | null = null;
  private sockets = 0;

  constructor(private readonly keep: boolean) {}

  /** Starts listening to the page's sockets; call before its first navigation. */
  listen(page: Page): void {
    page.on("websocket", (socket) => {
      if (!socket.url().includes("/ws/live")) return;
      const number = ++this.sockets;
      let index = 0;
      socket.on("framesent", ({ payload }) => {
        const frame = parse(String(payload));
        if (frame === null || typeof frame.sub !== "string") return;
        if (frame.type === "subscribe" && typeof frame.view === "object" && frame.view !== null) {
          this.views.set(frame.sub, frame.view as View);
          if (!this.subscribed.has(frame.sub)) this.subscribed.set(frame.sub, Date.now());
        }
      });
      socket.on("framereceived", ({ payload }) => {
        const text = String(payload);
        const frame: Frame = {
          socket: number,
          index: index++,
          heard: Date.now(),
          text,
          parsed: parse(text),
        };
        if (this.keep) this.frames.push(frame);
        this.onFrame?.(frame);
      });
    });
  }
}

function parse(text: string): Record<string, unknown> | null {
  try {
    const value: unknown = JSON.parse(text);
    return typeof value === "object" && value !== null ? (value as Record<string, unknown>) : null;
  } catch {
    return null;
  }
}

/** The key a frame's socket and place give it, which the instrument's frames carry too. */
export function frameKey(socket: number, index: number): string {
  return `${socket}:${index}`;
}

// ---------------------------------------------------------------------------------------------
// Replaying a view's frames

/** What one view of a page holds: its body's members, each list as its items by id. */
export type ViewState = Map<string, unknown>;

/** An item's id as the hub keys a list item: a string id as it is, any other in decimal. */
function itemId(item: unknown): string {
  if (typeof item === "object" && item !== null && "id" in item) {
    return typeof item.id === "string" ? item.id : JSON.stringify(item.id);
  }
  return JSON.stringify(item);
}

/** A list member of a view's body: every array whose items carry an id. */
function isList(value: unknown): value is unknown[] {
  return (
    Array.isArray(value) &&
    value.every((item) => typeof item === "object" && item !== null && "id" in item)
  );
}

/** The state a snapshot's body is. */
export function stateOf(body: Record<string, unknown>): ViewState {
  const state: ViewState = new Map();
  for (const [name, value] of Object.entries(body)) {
    state.set(name, isList(value) ? new Map(value.map((item) => [itemId(item), item])) : value);
  }
  return state;
}

/** Applies a delta to a view's state (the hub's deliveryLiveDelta: whole fields set, list items
 *  upserted and removed by id, map entries and counts set, null removing), and answers the
 *  members it touched, each as `name` or `name/key`. */
export function applyDelta(state: ViewState, delta: Record<string, unknown>): Set<string> {
  const touched = new Set<string>();
  for (const [name, value] of Object.entries(asRecord(delta.set))) {
    const held = state.get(name);
    state.set(
      name,
      held instanceof Map && isList(value)
        ? new Map(value.map((item) => [itemId(item), item]))
        : value
    );
    touched.add(name);
  }
  for (const [name, change] of Object.entries(asRecord(delta.items))) {
    const held = state.get(name);
    const items = held instanceof Map ? held : new Map<unknown, unknown>();
    state.set(name, items);
    const { upsert, remove } = asRecord(change);
    for (const id of Array.isArray(remove) ? remove : []) {
      const key = typeof id === "string" ? id : JSON.stringify(id);
      items.delete(key);
      touched.add(`${name}/${key}`);
    }
    for (const item of Array.isArray(upsert) ? upsert : []) {
      items.set(itemId(item), item);
      touched.add(`${name}/${itemId(item)}`);
    }
  }
  for (const member of ["maps", "counts"]) {
    for (const [name, change] of Object.entries(asRecord(delta[member]))) {
      const held = asRecord(state.get(name));
      const next: Record<string, unknown> = { ...held };
      for (const [key, value] of Object.entries(asRecord(change))) {
        if (member === "counts") {
          const facet: Record<string, unknown> = { ...asRecord(next[key]) };
          for (const [entry, count] of Object.entries(asRecord(value))) {
            if (count === null) delete facet[entry];
            else facet[entry] = count;
          }
          next[key] = facet;
        } else if (value === null) delete next[key];
        else next[key] = value;
        touched.add(`${name}/${key}`);
      }
      state.set(name, next);
    }
  }
  return touched;
}

function asRecord(value: unknown): Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : {};
}

/** One step of a leaf's path: a member of an object (`f`), a list item by id (`k`), a series by
 *  its name (`s`), a day by its date (`d`), or an array element by place (`i`). */
export type Step = readonly ["f" | "k" | "s" | "d" | "i", string];

/** What separates the steps of a path's text: a character no id, series, day or member name
 *  holds, where an eval's id ("INFRA/pr-ci") and a job's ("build / unit") hold a slash. */
const SEPARATOR = "\u001f";

/** A leaf's path as text, which keys a map of leaves. */
export function pathText(path: readonly Step[]): string {
  return path.map(([kind, name]) => `${kind}:${name}`).join(SEPARATOR);
}

/** The path a pathText names. */
export function pathOf(text: string): Step[] {
  return text.split(SEPARATOR).map((step) => {
    const colon = step.indexOf(":");
    const kind = step.slice(0, colon);
    if (kind !== "f" && kind !== "k" && kind !== "s" && kind !== "d" && kind !== "i") {
      throw new Error(`a path step is f, k, s, d or i, not ${JSON.stringify(step)}`);
    }
    return [kind, step.slice(colon + 1)] as const;
  });
}

/** A path's text as a log line shows it. */
export function shown(text: string): string {
  return text.replaceAll(SEPARATOR, " / ");
}

/** The step an array element of `parent` takes: a series by its name, a day by its date, an item
 *  by its id, else its place. */
function elementStep(element: unknown, place: number): Step {
  const record = asRecord(element);
  if (typeof record.series === "string") return ["s", record.series];
  if (typeof record.day === "string") return ["d", record.day];
  if ("id" in record) return ["k", itemId(record)];
  return ["i", String(place)];
}

/** Every leaf of a view's state, by its path's text, with its value. */
export function flatten(state: ViewState): Map<string, unknown> {
  const out = new Map<string, unknown>();
  const walk = (value: unknown, path: Step[]) => {
    if (value instanceof Map) {
      for (const [id, item] of value) walk(item, [...path, ["k", id]]);
    } else if (Array.isArray(value)) {
      for (const [place, element] of value.entries())
        walk(element, [...path, elementStep(element, place)]);
    } else if (typeof value === "object" && value !== null) {
      for (const [name, member] of Object.entries(value)) walk(member, [...path, ["f", name]]);
    } else out.set(pathText(path), value);
  };
  for (const [name, value] of state) walk(value, [["f", name]]);
  return out;
}

/** The value at path in a view's state, undefined where it holds none. */
export function leafAt(state: ViewState, path: readonly Step[]): unknown {
  let value: unknown = state;
  for (const [kind, name] of path) {
    if (value instanceof Map) value = value.get(name);
    else if (Array.isArray(value)) {
      value =
        kind === "i"
          ? value[Number(name)]
          : value.find((element, place) => {
              const [k, n] = elementStep(element, place);
              return k === kind && n === name;
            });
    } else if (typeof value === "object" && value !== null) {
      value = asRecord(value)[name];
    } else return undefined;
    if (value === undefined) return undefined;
  }
  return value;
}

/** Every view of one page as its frames left it, kept current as each frame is heard. */
export class Replay {
  readonly states = new Map<string, ViewState>();

  constructor(private readonly log: LiveLog) {}

  /** Applies one frame; answers the sub it moved and the members it touched, null for a frame
   *  that moved no view (a heartbeat, a freshness frame, an events frame). */
  apply(frame: Frame): { sub: string; touched: Set<string> | "all" } | null {
    const parsed = frame.parsed;
    if (parsed === null || typeof parsed.sub !== "string") return null;
    if (!this.log.views.has(parsed.sub)) return null;
    if (parsed.type === "snapshot") {
      this.states.set(parsed.sub, stateOf(asRecord(parsed.body)));
      return { sub: parsed.sub, touched: "all" };
    }
    if (parsed.type === "delta") {
      const state = this.states.get(parsed.sub);
      if (state === undefined) return null;
      return { sub: parsed.sub, touched: applyDelta(state, parsed) };
    }
    return null;
  }
}

// ---------------------------------------------------------------------------------------------
// The server's debug lines

/** The messages of the server's debug lines the steps read (cmd/dispatch/README.md, Logging): a
 *  page of the eval day-row refresher that read changes, a read of the change log by the live
 *  hub, a recompute of the hub, and a snapshot or delta a live socket wrote. */
export const LOGGED = {
  page: "dispatch: the eval day-row refresher read a page of changes",
  changes: "dispatch: delivery live hub read changes",
  recompute: "dispatch: delivery live hub timed a recompute",
  frame: "dispatch: live socket wrote a frame",
} as const;

/** One of slog's attributes in a line of its text form, `key=value` or `key="quoted value"`. */
const ATTRIBUTE = /(?:^|\s)([A-Za-z_][\w.]*)=("(?:[^"\\]|\\.)*"|\S*)/g;

/** One line of the server's log in slog's text form (`time=… level=… msg="…" key=value …`): its
 *  message and its attributes, a quoted value unquoted. */
export class Logged {
  constructor(
    readonly msg: string,
    private readonly attrs: ReadonlyMap<string, string>
  ) {}

  /** The attribute `key` as text, "" where the line holds none. */
  text(key: string): string {
    return this.attrs.get(key) ?? "";
  }

  /** The attribute `key` as a number, null where the line holds none or no number. */
  number(key: string): number | null {
    const value = this.attrs.get(key);
    if (value === undefined || value === "") return null;
    const parsed = Number(value);
    return Number.isFinite(parsed) ? parsed : null;
  }

  /** The moment the attribute `key` states in Unix microseconds, in milliseconds since the epoch
   *  as the page's and the database's times are, null where it states none (0). */
  at(key: string): number | null {
    const micros = this.number(key);
    return micros === null || micros === 0 ? null : micros / 1000;
  }
}

/** How many bytes the server's log at path holds now, 0 where it does not exist yet: where a read
 *  of what the server logs from now on starts (serverLines). */
export function logSize(path: string): number {
  try {
    return statSync(path).size;
  } catch {
    return 0;
  }
}

/** Every line of the server's log at path from byte `from` on whose message is one of LOGGED's,
 *  in the order the server logged them; none where the log cannot be read. */
export function serverLines(path: string, from: number): Logged[] {
  let text: string;
  try {
    text = readFileSync(path).subarray(from).toString("utf8");
  } catch {
    return [];
  }
  const wanted = Object.values(LOGGED).map((msg) => [msg, `msg=${JSON.stringify(msg)}`] as const);
  const out: Logged[] = [];
  for (const line of text.split("\n")) {
    const msg = wanted.find(([, quoted]) => line.includes(quoted))?.[0];
    if (msg === undefined) continue;
    const attrs = new Map<string, string>();
    for (const [, key, value] of line.matchAll(ATTRIBUTE)) {
      if (key === undefined || value === undefined) continue;
      attrs.set(key, value.startsWith('"') ? unquote(value) : value);
    }
    out.push(new Logged(msg, attrs));
  }
  return out;
}

/** A value slog quoted (strconv.Quote), unquoted. */
function unquote(value: string): string {
  try {
    return String(JSON.parse(value));
  } catch {
    return value.slice(1, -1);
  }
}

/** Whether a recompute's line (LOGGED.recompute) is one of view's, a view a page subscribed that
 *  `narrowed` says whether a facet narrows: the line's view kind, kind of eval, page offset,
 *  narrowing, eval and order are the view's. */
export function recomputes(line: Logged, view: View, narrowed: boolean): boolean {
  const viewQuery = new URLSearchParams(typeof view.query === "string" ? view.query : "");
  return (
    line.msg === LOGGED.recompute &&
    line.text("view") === view.kind &&
    line.text("evals_kind") === (viewQuery.get("kind") ?? "") &&
    line.text("offset") === (viewQuery.get("offset") ?? "") &&
    line.text("narrowed") === String(narrowed) &&
    line.text("slug") === (typeof view.slug === "string" ? view.slug : "") &&
    line.text("sort") === (viewQuery.get("sort") ?? "")
  );
}

// ---------------------------------------------------------------------------------------------
// The database

/** A connection to the bench's database, `max` at once. */
export function database(url: string, max = 1): SQL {
  return new SQL(url, { max });
}

/** The bench's own statements carry this, so a reading of pg_stat_statements leaves them out. */
export const OWN = "/* evalbench-tab */";

/** One statement pg_stat_statements counted since its last reset. */
export interface Counted {
  queryid: string;
  query: string;
  calls: number;
  ms: number;
}

/** Every statement the database ran since pg_stat_statements' last reset, the bench's own and
 *  pg_stat_statements' readers left out. */
export async function counted(db: SQL): Promise<Counted[]> {
  const rows = (await db.unsafe(
    `${OWN} select queryid::text as queryid, query, calls::float8 as calls, total_exec_time::float8 as ms
     from pg_stat_statements
     where dbid = (select oid from pg_database where datname = current_database())
       and calls > 0 and query not like '%pg_stat_statements%' and query not like '%evalbench-tab%'`
  )) as Counted[];
  return rows.map((row) => ({ ...row, calls: Number(row.calls), ms: Number(row.ms) }));
}

/** Empties pg_stat_statements. */
export async function resetStatements(db: SQL): Promise<void> {
  await db.unsafe(`${OWN} select pg_stat_statements_reset()`);
}
