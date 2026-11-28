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
//
// The rule the times are judged by is INFRA-979's ruling on the first screen and the machine
// (dispatch://INFRA-2918/comment/75af800e-9db9-47d9-9c4d-427b0cff36c0), as k4 and k19 of the
// Verification document (v23) state it. Every page is 1440×900 at device scale 1, at the top of
// the page (VIEWPORT), and the first paint ends at its first screen (screenPaint): each section
// with any part in the viewport has drawn its numbers and charts, each other section its header and
// its box. The browser's work is judged at the speed of a reader's computer, a Lighthouse
// BenchmarkIndex of 1500 (REFERENCE_INDEX), and the server's time as measured: each run measures
// the host's index in the same Chromium (benchmarkIndex); above 1500 it slows the page's CPU at
// index ÷ 1500 (throttle) and judges each time as measured, and at or below 1500 it runs
// unthrottled and judges each time as its waits plus its main-thread time × index ÷ 1500 (speedOf,
// judge), the main-thread time read from a trace of the paint (mainThreadTasks, busyIn).
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

/** Prints one measurement over a budget no check gates at the run's rate, as a RECORD line, which
 *  fails nothing (run.sh's header): at ten times the stated rate, the first paint's database time,
 *  which counts inside the judged 0.4 s there. */
export function recordLine(line: string): void {
  console.log(`RECORD ${line}`);
}

/** Prints what the step did, for the reader of the log. */
export function mark(line: string): void {
  console.log(`MARK ${new Date().toISOString()} ${line}`);
}

/** How many FAIL lines the step printed. */
export function failed(): number {
  return failures;
}

/** The exit status of a drive that ran to its end and printed FAIL lines (`failed`), so run.sh
 *  tells it from a drive that ended in an error (status 1, Bun's for an uncaught error), which
 *  measured nothing it can name. */
export const MISSED_EXIT = 3;

/** The exit status of a drive that ran to its end: MISSED_EXIT where it printed a FAIL line, else 0. */
export function exitStatus(): number {
  return failures > 0 ? MISSED_EXIT : 0;
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
  /** Chromium's tracing of every one of its processes (Playwright's `browser.startTracing`), the
   *  page's main thread among them, until stopTracing answers the trace as JSON. */
  startTracing(
    page: Page | undefined,
    options: { categories: readonly string[]; screenshots: boolean }
  ): Promise<void>;
  stopTracing(): Promise<Buffer>;
}

export interface BrowserContext {
  addInitScript(script: { content: string }): Promise<void>;
  newPage(): Promise<Page>;
  /** A Chrome DevTools Protocol session attached to one page of the context (Chromium alone). */
  newCDPSession(page: Page): Promise<CDPSession>;
  request: {
    get(url: string, options: Record<string, unknown>): Promise<{ status(): number }>;
  };
  close(): Promise<void>;
}

export interface CDPSession {
  send(method: string, params?: Record<string, unknown>): Promise<unknown>;
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

// ---------------------------------------------------------------------------------------------
// The machine (INFRA-979's ruling, part B: the header)

/** The Lighthouse BenchmarkIndex a reader's computer is taken to have: the low end of Lighthouse's
 *  "High-End Desktop" class (Lighthouse's docs/throttling.md), the ruling's reference. */
export const REFERENCE_INDEX = 1500;

/**
 * Lighthouse's computeBenchmarkIndex, verbatim but for its comments, from Lighthouse v13.6.0's
 * core/lib/page-functions.js
 * (https://github.com/GoogleChrome/lighthouse/blob/v13.6.0/core/lib/page-functions.js), which
 * Lighthouse evaluates in the page under test before it throttles it (getBenchmarkIndex of
 * core/gather/driver/environment.js): the mean of a GC-heavy index (a string of 10,000 characters
 * built in a loop for 500 ms) and an array copy index (100,000 integers copied from one array to
 * another for 500 ms), each the loops a second ÷ 10.
 */
const BENCHMARK_INDEX = `(function computeBenchmarkIndex() {
  function benchmarkIndexGC() {
    const start = Date.now();
    let iterations = 0;

    while (Date.now() - start < 500) {
      let s = '';
      for (let j = 0; j < 10000; j++) s += 'a';
      if (s.length === 1) throw new Error('will never happen, but prevents compiler optimizations');

      iterations++;
    }

    const durationInSeconds = (Date.now() - start) / 1000;
    return Math.round(iterations / 10 / durationInSeconds);
  }

  function benchmarkIndexNoGC() {
    const arrA = [];
    const arrB = [];
    for (let i = 0; i < 100000; i++) arrA[i] = arrB[i] = i;

    const start = Date.now();
    let iterations = 0;

    while (iterations % 10 !== 0 || Date.now() - start < 500) {
      const src = iterations % 2 === 0 ? arrA : arrB;
      const tgt = iterations % 2 === 0 ? arrB : arrA;

      for (let j = 0; j < src.length; j++) tgt[j] = src[j];

      iterations++;
    }

    const durationInSeconds = (Date.now() - start) / 1000;
    return Math.round(iterations / 10 / durationInSeconds);
  }

  return (benchmarkIndexGC() + benchmarkIndexNoGC()) / 2;
})()`;

/** The host's BenchmarkIndex, measured as Lighthouse measures it in a blank page of `browser`, in
 *  a context of its own so no instrument runs beside it; null where the page answered no positive
 *  number or the measurement failed. */
export async function benchmarkIndex(browser: Browser): Promise<number | null> {
  const context = await browser.newContext({});
  try {
    const page = await context.newPage();
    const index = await page.evaluate<number>(BENCHMARK_INDEX);
    return typeof index === "number" && Number.isFinite(index) && index > 0 ? index : null;
  } catch (error) {
    mark(`the BenchmarkIndex was not measured: ${String(error).slice(0, 200)}`);
    return null;
  } finally {
    await context.close();
  }
}

/** How a host's times are judged at the reference's speed (the ruling's B4): above
 *  REFERENCE_INDEX the page's CPU is throttled at `throttle`, index ÷ 1500, and each time is judged
 *  as measured (`scale` null); at or below it the page runs unthrottled (`throttle` null) and each
 *  time's main-thread part is judged at `scale`, index ÷ 1500. */
export interface Speed {
  index: number;
  throttle: number | null;
  scale: number | null;
}

/** The Speed of a host whose index is `index`; null where no index was read. */
export function speedOf(index: number | null): Speed | null {
  if (index === null || !Number.isFinite(index) || index <= 0) return null;
  const ratio = index / REFERENCE_INDEX;
  return index > REFERENCE_INDEX
    ? { index, throttle: ratio, scale: null }
    : { index, throttle: null, scale: ratio };
}

/** One moment's time from the navigation, in milliseconds: as measured (`raw`) and as judged, and
 *  where the page ran unthrottled the two parts it is judged from: its waits (the raw time less the
 *  main thread's busy time before the moment) and its main-thread time. */
export interface Judged {
  raw: number;
  judged: number;
  waits: number | null;
  main: number | null;
}

/** Judges a moment `raw` ms after the navigation whose main thread was busy `main` ms before it, on
 *  a host of `speed`: throttled, as measured; unthrottled, its waits plus its main-thread time ×
 *  index ÷ 1500. Null where the moment, the speed or, unthrottled, the main-thread time is unread. */
export function judge(raw: number | null, main: number | null, speed: Speed | null): Judged | null {
  if (raw === null || !Number.isFinite(raw) || speed === null) return null;
  if (speed.scale === null) return { raw, judged: raw, waits: null, main: null };
  if (main === null || !Number.isFinite(main)) return null;
  const waits = raw - main;
  return { raw, judged: waits + main * speed.scale, waits, main };
}

/** A moment's fields on a RESULT line, each named `<name>_<part>_ms`: its raw time, its judged
 *  time (`none` where it was not judged), and its waits and main-thread time (`none` where the page
 *  was throttled or the moment was not judged). */
export function judgedFields(name: string, raw: number | null, moment: Judged | null): string {
  return (
    `${name}_raw_ms=${ms(raw)} ${name}_judged_ms=${ms(moment?.judged)} ` +
    `${name}_waits_ms=${ms(moment?.waits)} ${name}_main_ms=${ms(moment?.main)}`
  );
}

/** The speed's fields on a RESULT line: the index, the throttling rate or the scale, each in full
 *  so index ÷ 1500 can be checked, and the node with its machine type (job.sh), `unread` where the
 *  run read none. */
export function speedFields(speed: Speed | null, node: string, instanceType: string): string {
  return (
    `index=${speed?.index ?? "none"} reference_index=${REFERENCE_INDEX} ` +
    `throttle_rate=${speed?.throttle ?? "none"} scale=${speed?.scale ?? "none"} ` +
    `node=${node === "" ? "unnamed" : node} instance_type=${instanceType === "" ? "unread" : instanceType}`
  );
}

/** Slows `page`'s CPU at `rate` before its navigation through a DevTools session, as Lighthouse's
 *  DevTools throttling does (core/lib/emulation.js of v13.6.0: `Emulation.setCPUThrottlingRate`).
 *  The session holds the throttling for as long as it is attached, so its caller keeps it until the
 *  page closes; null where Chromium refused it. */
export async function throttle(
  context: BrowserContext,
  page: Page,
  rate: number
): Promise<CDPSession | null> {
  try {
    const session = await context.newCDPSession(page);
    await session.send("Emulation.setCPUThrottlingRate", { rate });
    return session;
  } catch (error) {
    mark(`Emulation.setCPUThrottlingRate ${rate} was refused: ${String(error).slice(0, 200)}`);
    return null;
  }
}

// ---------------------------------------------------------------------------------------------
// The first screen (INFRA-979's ruling, part A: the header)

/** The reader's viewport: 1440×900 at device scale 1, the page at its top. */
export const VIEWPORT = { width: 1440, height: 900 } as const;

/** Where an element lies in the viewport, in CSS pixels from its top left corner. */
export interface Rect {
  top: number;
  bottom: number;
  left: number;
  right: number;
}

/** Whether any part of `rect` lies in the first screen. */
export function inFirstScreen(rect: Rect): boolean {
  return (
    rect.bottom > rect.top &&
    rect.right > rect.left &&
    rect.top < VIEWPORT.height &&
    rect.bottom > 0 &&
    rect.left < VIEWPORT.width &&
    rect.right > 0
  );
}

/** One section's moments, in milliseconds from the navigation: when its header and its box were
 *  first presented (`shown`, null where the page set no box mark) and when its numbers and charts
 *  were, its job lists' among them (`drawn`); and whether it lies in the first screen. */
export interface SectionPaint {
  id: string;
  shown: number | null;
  drawn: number | null;
  inView: boolean;
}

/** The first paint and the moment every section was drawn: the first paint ends once each section
 *  in the first screen is drawn and each other section shows its box (or, with no box mark, is
 *  drawn). Each is null while a section has not, or where there is no section. */
export function screenPaint(sections: readonly SectionPaint[]): {
  firstPaint: number | null;
  everySection: number | null;
} {
  const latest = (times: readonly (number | null)[]) =>
    times.length === 0 || times.some((at) => at === null) ? null : Math.max(...(times as number[]));
  return {
    firstPaint: latest(sections.map((one) => (one.inView ? one.drawn : (one.shown ?? one.drawn)))),
    everySection: latest(sections.map((one) => one.drawn)),
  };
}

/** One size the page's ResizeObserver reported of a section's element, its border box in CSS
 *  pixels, at its moment in milliseconds since the epoch. */
export interface Size {
  at: number;
  width: number;
  height: number;
}

/** A section box's size when first presented (`shown`) and when its drawing was (`drawn`), each
 *  the last size the page reported by then, and whether it changed between them: whether any size
 *  reported after `shown` and by `drawn` is not the shown one. A size reported in a frame comes
 *  before that frame's presentation (the instrument's `painted`), so the last size by a moment is
 *  the size the browser presented then. The sizes are null where the page reported none by a
 *  moment, and `changed` is null where either moment or either size is unread. */
export function boxChange(
  sizes: readonly Size[],
  shown: number | undefined,
  drawn: number | undefined
): { shown: Size | null; drawn: Size | null; changed: boolean | null } {
  const by = (moment: number | undefined) =>
    moment === undefined ? null : (sizes.filter((one) => one.at <= moment).at(-1) ?? null);
  const first = by(shown);
  const last = by(drawn);
  if (shown === undefined || drawn === undefined || first === null || last === null)
    return { shown: first, drawn: last, changed: null };
  const changed = sizes.some(
    (one) =>
      one.at > shown &&
      one.at <= drawn &&
      (one.width !== first.width || one.height !== first.height)
  );
  return { shown: first, drawn: last, changed };
}

/** The instrument each page of the tab runs before any of the app's own scripts. A plain run's
 *  records what the bounds are judged by. For each section (`[data-section]`): when it first reads
 *  `data-state="painted"` or `"failed"` (the section's own `at` and `painted`); when the app first
 *  marks its header and its box shown (`box`: the mark `dispatch:evals-section-box:<id>`) and its
 *  numbers and charts drawn (`drawn`: the mark `dispatch:evals-section:<id>`), each mark set in
 *  the commit that draws what it names (useDrawnMark.ts) and stamped as the app sets it, through
 *  `performance.mark`; and each size of its element's border box that a ResizeObserver reports
 *  (`sizes`), which the browser reports after its own layout of a frame, so the observer forces
 *  no layout. When each of the CI section's job lists (`[data-job-list]`) first holds no loading
 *  line, and the section it lies in; when the narrowed cards' loading mark
 *  (`[data-narrowing="loading"]`) shows and when it gives way to the narrowed values; and for every
 *  frame of the page's live socket (`/ws/live`), when it arrived and when the page had painted
 *  after it. A split run's (`split`) records besides: when the page made each live socket and when
 *  it opened (`opened`); when the page sent each subscribe frame, by its sub (`sends`); every
 *  performance mark the app sets that is named `dispatch:…` (`marks`): a snapshot's parse ended
 *  (`dispatch:live-snapshot:<sub>`, api/live-socket.ts), and a section or a CI job list committed
 *  its drawing (`dispatch:evals-section:<id>`, `dispatch:evals-job-list:<id>`, useDrawnMark.ts);
 *  and every long task of the page's main thread, a task of 50 ms or more, with its start and
 *  length (`longTasks`), which says whether the thread was busy between two frames; how long the
 *  page's own handlers of each live frame ran (each frame's `handled`, the app's `message`
 *  listeners of the socket, timed around each call); and the moment each React commit ended
 *  (`commits`), through the hook React's production build calls at the end of every commit, which
 *  React's developer tools install. Every time of a DOM change or a mark is read twice: `at`, the
 *  change, and `painted`, two animation frames later, once the browser has presented it. Times are
 *  milliseconds since the epoch, so they compare with the database's commit timestamps and the
 *  server's log; the navigation's own start is `start`. */
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
  }).observe({ type: "longtask", buffered: true });
  record.commits = [];
  window.__REACT_DEVTOOLS_GLOBAL_HOOK__ = { supportsFiber: true, isDisabled: false, renderers: new Map(),
    inject: () => 1, onCommitFiberRoot: () => { record.commits.push(now()); }, onPostCommitFiberRoot: () => {},
    onCommitFiberUnmount: () => {}, setStrictMode: () => {} };
  const timed = new WeakMap();
  Measured.prototype.addEventListener = function (type, listener, options) {
    if (type !== "message" || this.__evalbenchSocket === undefined || typeof listener !== "function")
      return listen.call(this, type, listener, options);
    let wrapped = timed.get(listener);
    if (wrapped === undefined) {
      wrapped = function (event) {
        const began = now();
        try {
          return listener.call(this, event);
        } finally {
          const frame = this.__evalbenchFrame;
          if (frame !== undefined) frame.handled = (frame.handled || 0) + now() - began;
        }
      };
      timed.set(listener, wrapped);
    }
    return listen.call(this, type, wrapped, options);
  };
  const unlisten = Native.prototype.removeEventListener;
  Measured.prototype.removeEventListener = function (type, listener, options) {
    return unlisten.call(this, type, (typeof listener === "function" && timed.get(listener)) || listener, options);
  };`;
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
  const listen = Native.prototype.addEventListener;
  class Measured extends Native {
    constructor(url, protocols) {
      super(url, protocols);
      if (!String(url).includes("/ws/live")) return;
      const socket = ++record.sockets;
      this.__evalbenchSocket = socket;
      if (opened !== null) opened(socket, this);
      let index = 0;
      listen.call(this, "message", () => {
        const entry = { socket, index: index++ };
        record.frames.push(entry);
        this.__evalbenchFrame = entry;
        stamp(entry);
      });
    }
  }
  window.WebSocket = Measured;${splitParts}
  const sectionOf = (id) => record.sections[id] || (record.sections[id] = { state: null, sizes: [] });
  const BOX = "dispatch:evals-section-box:";
  const DRAWN = "dispatch:evals-section:";
  const setMark = performance.mark;
  performance.mark = function (name, options) {
    const entry = setMark.call(this, name, options);
    if (typeof name === "string") {
      const box = name.startsWith(BOX);
      if (box || name.startsWith(DRAWN)) {
        const section = sectionOf(name.slice(box ? BOX.length : DRAWN.length));
        const key = box ? "box" : "drawn";
        if (section[key] === undefined) section[key] = stamp({});
      }
    }
    return entry;
  };
  const sized = new ResizeObserver((entries) => {
    const at = now();
    for (const entry of entries) {
      const id = entry.target.getAttribute("data-section");
      if (id === null) continue;
      const border = entry.borderBoxSize && entry.borderBoxSize[0];
      sectionOf(id).sizes.push({ at, width: border ? border.inlineSize : entry.contentRect.width,
        height: border ? border.blockSize : entry.contentRect.height });
    }
  });
  const watched = new WeakSet();
  const check = () => {
    for (const element of document.querySelectorAll("[data-section]")) {
      const section = sectionOf(element.getAttribute("data-section"));
      const state = element.getAttribute("data-state");
      section.state = state;
      if (!watched.has(element)) {
        watched.add(element);
        sized.observe(element);
      }
      if ((state === "painted" || state === "failed") && section.at === undefined) stamp(section);
    }
    for (const element of document.querySelectorAll("[data-job-list]")) {
      const id = element.getAttribute("data-job-list");
      if (record.jobLists[id] !== undefined) continue;
      if (element.querySelector('[role="status"]') !== null) continue;
      const owner = element.closest("[data-section]");
      record.jobLists[id] = stamp({ failed: (element.textContent || "").includes("could not load"),
        section: owner === null ? null : owner.getAttribute("data-section") });
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
  /** Each section: its state and when it first read painted or failed (the stamp), when its box
   *  and its drawing were first marked, and each size its element reported (instrument). */
  sections: Record<
    string,
    Stamp & { state: string | null; box?: Stamp; drawn?: Stamp; sizes: Size[] }
  >;
  /** Each CI job list: when it first held no loading line, whether it failed, and its section. */
  jobLists: Record<string, Stamp & { failed: boolean; section: string | null }>;
  narrowing: { loading?: Stamp; narrowed?: Stamp };
  /** Each live frame: its socket and place, and in a split run how long the page's own handlers of
   *  it ran (`handled`, ms). */
  frames: (Stamp & { socket: number; index: number; handled?: number })[];
  /** Each live socket the page made, when it made it and when it opened (none before). */
  opened: { socket: number; made: number; open?: number }[];
  /** Each subscribe frame the page sent, by its socket and sub, when it sent it. */
  sends: { socket: number; sub: string; at: number }[];
  /** Every `dispatch:` mark set, the ones the app has cleared since among them, each once. */
  marks: Mark[];
  /** Every long task of the page's main thread: when it began and how long it ran. */
  longTasks: { at: number; ms: number }[];
  /** When each React commit ended. */
  commits: number[];
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
         marks: (r.marks || []).concat(held), longTasks: r.longTasks || [], commits: r.commits || [] }; })()`
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

// ---------------------------------------------------------------------------------------------
// The page's main thread, from Chromium's trace

/** What a split run traces: every task each thread runs (`toplevel`), what each task ran, as
 *  DevTools' Performance panel draws it (`devtools.timeline` and its detail: script, style and
 *  layout, paint and garbage collection), and the user timing marks, among them the navigation's
 *  start. */
export const TRACE_CATEGORIES: readonly string[] = [
  "toplevel",
  "devtools.timeline",
  "disabled-by-default-devtools.timeline",
  "blink.user_timing",
];

/** What a plain run traces where its host's index is at or below 1500, so that each moment's
 *  main-thread time can be judged (judge): the tasks each thread runs (`toplevel`) and the user
 *  timing marks, among them the navigation's start, which is all mainThreadTasks and busyIn read.
 *  The DevTools `Performance` domain's task durations would cost less still, but they are a running
 *  total read when asked, which cannot tell the time before a moment from the time after it. This
 *  trace costs the page little beside the split's: Chromium writes the events of a task as the
 *  task begins and ends, into a buffer of the thread that runs it, and none of what the task ran
 *  (no event of script, style, layout or paint, none with arguments to serialise), so the split's
 *  per-call detail, its costly part, is left out. */
export const LIGHT_TRACE_CATEGORIES: readonly string[] = ["toplevel", "blink.user_timing"];

/** What a task of the main thread spent its time on: the page's script (event handlers, timers,
 *  animation frame callbacks, microtasks, compiles), style and layout, paint (pre-paint, paint,
 *  layerize and the commit to the compositor), garbage collection, the parse of HTML and CSS, and
 *  whatever else it ran. */
export type TaskPart = "script" | "style_layout" | "paint" | "gc" | "parse" | "other";

export const TASK_PARTS: readonly TaskPart[] = [
  "script",
  "style_layout",
  "paint",
  "gc",
  "parse",
  "other",
];

/** The part each trace event's time is, by its name; an event of no part named here counts toward
 *  the event of a part it runs inside, or `other` inside none. */
const PART_OF: Readonly<Record<string, TaskPart>> = {
  EventDispatch: "script",
  EvaluateScript: "script",
  FireAnimationFrame: "script",
  FireIdleCallback: "script",
  FunctionCall: "script",
  RunMicrotasks: "script",
  TimerFire: "script",
  "v8.compile": "script",
  "v8.compileModule": "script",
  "v8.evaluateModule": "script",
  XHRReadyStateChange: "script",
  HitTest: "style_layout",
  Layout: "style_layout",
  RecalculateStyles: "style_layout",
  UpdateLayoutTree: "style_layout",
  Commit: "paint",
  CompositeLayers: "paint",
  Layerize: "paint",
  Paint: "paint",
  PrePaint: "paint",
  UpdateLayer: "paint",
  MajorGC: "gc",
  MinorGC: "gc",
  ParseAuthorStyleSheet: "parse",
  ParseHTML: "parse",
};

/** The names Chromium gives a task a thread runs. */
const TASK_NAMES: Readonly<Record<string, true>> = {
  RunTask: true,
  "ThreadControllerImpl::RunTask": true,
};

/** One task of the page's main thread: when it began, in milliseconds from the navigation, how
 *  long it ran, its self time by part, and what started it: `timer`, `message` (a message event,
 *  a socket's frame or a MessagePort's, React's scheduler's among them), `animation-frame`,
 *  `render` (style, layout or paint and no script), else its largest part. */
export interface Task {
  at: number;
  ms: number;
  parts: Record<TaskPart, number>;
  started: string;
}

/** One event of a trace, as Chromium writes it in JSON. */
interface TraceEvent {
  name: string;
  ph: string;
  ts: number;
  dur?: number;
  pid: number;
  tid: number;
  args?: { data?: Record<string, unknown> } & Record<string, unknown>;
}

/** One span of one thread: a complete event, or a begin and its end. */
interface Span {
  name: string;
  ts: number;
  end: number;
  data: Record<string, unknown>;
}

function noParts(): Record<TaskPart, number> {
  return { script: 0, style_layout: 0, paint: 0, gc: 0, parse: 0, other: 0 };
}

/** The events of a trace in Chromium's JSON form: an array of them, or `{traceEvents: [...]}`. */
function traceEvents(trace: unknown): TraceEvent[] {
  const events = Array.isArray(trace) ? trace : asRecord(trace).traceEvents;
  return Array.isArray(events)
    ? events.filter(
        (event): event is TraceEvent =>
          typeof event === "object" &&
          event !== null &&
          typeof event.name === "string" &&
          typeof event.ts === "number"
      )
    : [];
}

/** The spans of one thread's events, in order of their start, the longer first of two that start
 *  together: each complete event (`X`), and each begin (`B`) with the end (`E`) that closes it. */
function spansOf(events: readonly TraceEvent[]): Span[] {
  const spans: Span[] = [];
  const open: TraceEvent[] = [];
  for (const event of [...events].sort((a, b) => a.ts - b.ts)) {
    if (event.ph === "X") {
      spans.push({
        name: event.name,
        ts: event.ts,
        end: event.ts + (event.dur ?? 0),
        data: event.args?.data ?? {},
      });
    } else if (event.ph === "B") {
      open.push(event);
    } else if (event.ph === "E") {
      const at = open.findLastIndex((begun) => event.name === "" || begun.name === event.name);
      const begun = at < 0 ? undefined : open.splice(at, 1)[0];
      if (begun !== undefined)
        spans.push({ name: begun.name, ts: begun.ts, end: event.ts, data: begun.args?.data ?? {} });
    }
  }
  return spans.sort((a, b) => a.ts - b.ts || b.end - a.end);
}

/** The self time of each part within one task (`inside`, the spans that start in it, in spansOf's
 *  order), in microseconds: each span of a part less the spans of a part inside it, and the task's
 *  time outside every span of a part as `other`. */
function partsWithin(task: Span, inside: readonly Span[]): Record<TaskPart, number> {
  const parts = noParts();
  const stack: { end: number; part: TaskPart; self: number }[] = [];
  let covered = 0;
  const close = (until: number) => {
    for (let top = stack.at(-1); top !== undefined && top.end <= until; top = stack.at(-1)) {
      stack.pop();
      parts[top.part] += Math.max(0, top.self);
    }
  };
  for (const span of inside) {
    const part = PART_OF[span.name];
    if (part === undefined) continue;
    close(span.ts);
    const end = Math.min(span.end, task.end);
    const top = stack.at(-1);
    if (top === undefined) covered += end - span.ts;
    else top.self -= Math.min(end, top.end) - span.ts;
    stack.push({ end, part, self: end - span.ts });
  }
  close(Number.POSITIVE_INFINITY);
  parts.other += Math.max(0, task.end - task.ts - covered);
  return parts;
}

/** What started a task, from the spans inside it and its parts (Task). */
function startedBy(inside: readonly Span[], parts: Record<TaskPart, number>): string {
  if (inside.some((span) => span.name === "TimerFire")) return "timer";
  if (inside.some((span) => span.name === "EventDispatch" && span.data.type === "message"))
    return "message";
  if (inside.some((span) => span.name === "FireAnimationFrame")) return "animation-frame";
  if (parts.script === 0 && parts.style_layout + parts.paint > 0) return "render";
  return TASK_PARTS.reduce((most, part) => (parts[part] > parts[most] ? part : most), "other");
}

/**
 * Every task of the page's main thread from the navigation to the trace's end, from a trace of
 * TRACE_CATEGORIES: the main thread is the one that marked the navigation's start
 * (`navigationStart`) of the last document whose address holds `path`, and each task is one the
 * thread ran from then on, outside any other (Chromium nests a task in a task where one runs
 * another), each time in milliseconds from that start, which is the page's
 * `performance.timeOrigin`. Null where the trace marks no such navigation. `events` counts the
 * trace's events.
 */
export function mainThreadTasks(
  trace: unknown,
  path: string
): { events: number; tasks: Task[] } | null {
  const events = traceEvents(trace);
  const origin = events
    .filter(
      (event) =>
        event.name === "navigationStart" &&
        String(event.args?.data?.documentLoaderURL ?? "").includes(path)
    )
    .sort((a, b) => a.ts - b.ts)
    .at(-1);
  if (origin === undefined) return null;
  const spans = spansOf(
    events.filter((event) => event.pid === origin.pid && event.tid === origin.tid)
  );
  const tasks: Task[] = [];
  let next = 0;
  let open: Span | undefined;
  for (const span of spans) {
    if (TASK_NAMES[span.name] !== true || span.ts < origin.ts) continue;
    if (open !== undefined && span.ts < open.end) continue;
    open = span;
    while (next < spans.length && (spans[next]?.ts ?? 0) < span.ts) next++;
    const inside: Span[] = [];
    for (let at = next; at < spans.length; at++) {
      const candidate = spans[at];
      if (candidate === undefined || candidate.ts >= span.end) break;
      if (TASK_NAMES[candidate.name] !== true) inside.push(candidate);
    }
    const parts = partsWithin(span, inside);
    tasks.push({
      at: (span.ts - origin.ts) / 1000,
      ms: (span.end - span.ts) / 1000,
      parts: Object.fromEntries(TASK_PARTS.map((part) => [part, parts[part] / 1000])) as Record<
        TaskPart,
        number
      >,
      started: startedBy(inside, parts),
    });
  }
  return { events: events.length, tasks };
}

/** How long `tasks` ran within [from, to), milliseconds from the navigation, and that time by
 *  part: each task's parts in the share of the task the span holds. */
export function busyIn(
  tasks: readonly Task[],
  from: number,
  to: number
): { ms: number; parts: Record<TaskPart, number> } {
  const parts = noParts();
  let busy = 0;
  for (const task of tasks) {
    const held = Math.max(0, Math.min(to, task.at + task.ms) - Math.max(from, task.at));
    if (held === 0) continue;
    busy += held;
    const share = task.ms === 0 ? 0 : held / task.ms;
    for (const part of TASK_PARTS) parts[part] += task.parts[part] * share;
  }
  return { ms: busy, parts };
}

/** A signed-in reader's browser context: pages of the reader's viewport (VIEWPORT, at device scale
 *  1), the instrument on every page it opens, a split run's where `split`, and the session cookie
 *  of the server's dev sign-in (signIn), so no page loads before the tab. */
export async function readerContext(
  browser: Browser,
  base: string,
  login: string,
  split: boolean
): Promise<BrowserContext> {
  const context = await browser.newContext({
    baseURL: base,
    viewport: { width: VIEWPORT.width, height: VIEWPORT.height },
    deviceScaleFactor: 1,
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
