import { existsSync, readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { errors, expect, type Page, type Request } from "@playwright/test";
import type * as WebVitals from "web-vitals";

/**
 * The instruments of a load of the new issue page, shared by the load rows
 * (`e2e/issue-page-load.e2e.ts`) and the production script, so both read each bound with the same
 * code: the page's init script, which collects, and the readers, which reduce.
 *
 * **What the init script collects**, installed before the page's own scripts
 * (`installInstruments`): Chromium's `PerformanceObserver` entries of `element` (Element Timing),
 * `largest-contentful-paint`, `longtask`, `event` (Event Timing, `durationThreshold: 16`) and
 * `resource`; `web-vitals`' `onCLS` with `reportAllChanges: true`, loaded from the installed
 * package's IIFE build; each `loadingdone` of `document.fonts`; and a `requestAnimationFrame` loop
 * that stamps each frame and, where the load asks for it, records the boxes of the view row's
 * controls, the header's controls and band, the rows in the viewport and the row the load names
 * (`BOX_GROUPS`). A box is recorded at the frame it is first drawn in and at each frame it
 * differs, rounded to whole pixels. The same loop watches the act a row armed (`armStep`): the
 * event's `timeStamp` comes from a capturing listener of the first trusted `pointerdown` or
 * `keydown` after the arming, and the act's result is read in each frame after it.
 *
 * **What the readers take**: plain arrays of those entries, in the shapes the observers give
 * them, serialised (`LoadRecord`), so a load's capture (`LoadCapture`) can be read back from a
 * fixture and every rule tested without a browser. Time is the page's own performance time,
 * which starts at the navigation's start.
 */

// ---------------------------------------------------------------------------------------------
// The entries, as a load's record serialises them.
// ---------------------------------------------------------------------------------------------

/** A task the Long Tasks API reported. */
export interface TaskEntry {
  readonly duration: number;
  readonly name?: string;
  readonly startTime: number;
}

/** An Element Timing entry, tied to the row whose first line its element is: by the row's
 *  identity in this load (`row`) and its words for a failure (`label`), or null for an element
 *  that stands in no row, or that was gone from the document by the time the entry was read. */
export interface ElementEntry {
  readonly identifier: string;
  readonly label: string | null;
  readonly loadTime: number;
  readonly name: string;
  readonly renderTime: number;
  readonly row: number | null;
  readonly startTime: number;
}

/** A candidate of the largest contentful paint. */
export interface LcpEntry {
  readonly element: string | null;
  readonly loadTime: number;
  readonly renderTime: number;
  readonly size: number;
  readonly startTime: number;
  readonly url: string;
}

/** An Event Timing entry, one of at least 16 ms. */
export interface EventEntry {
  readonly duration: number;
  readonly interactionId: number;
  readonly name: string;
  readonly processingEnd: number;
  readonly processingStart: number;
  readonly startTime: number;
}

/** A Resource Timing entry: a request the page made, once its answer has ended. */
export interface ResourceEntry {
  readonly duration: number;
  readonly initiatorType: string;
  /** The request's whole address. */
  readonly name: string;
  readonly requestStart: number;
  readonly responseEnd: number;
  readonly responseStart: number;
  readonly startTime: number;
}

/** One report of `web-vitals`' `onCLS`: the layout shift of the load so far, its largest session
 *  window, and the shifts of that window. */
export interface ClsReport {
  readonly delta: number;
  readonly shifts: readonly {
    readonly hadRecentInput: boolean;
    readonly startTime: number;
    readonly value: number;
  }[];
  readonly value: number;
}

/** One frame of the page's `requestAnimationFrame` loop. */
export interface FrameEntry {
  /** `performance.now()` in the frame's callback, which is inside the task that renders the
   *  frame: a task that starts after it is a task after the frame. */
  readonly at: number;
  /** The foot of the header, in px from the viewport's top: what a row stands under. */
  readonly foot: number;
  /** The viewport's height, in px. */
  readonly height: number;
  /** Whether the document holds a row of a view. */
  readonly rows: boolean;
  /** The time `requestAnimationFrame` gives the frame: the frame that shows what the document
   *  holds at its callback. */
  readonly time: number;
  /** Whether the document holds the view row's controls. */
  readonly viewRow: boolean;
}

/** A box, in px from the viewport's top left, rounded to whole pixels. */
export interface Box {
  readonly height: number;
  readonly width: number;
  readonly x: number;
  readonly y: number;
}

/** One element's box at one frame: the frame it was first drawn in, or one where it differed. */
export interface BoxEntry {
  readonly box: Box;
  /** The frame's index in the record's `frames`. */
  readonly frame: number;
  readonly group: string;
  /** The element's identity in this load, which the same element keeps across frames. */
  readonly id: number;
  readonly label: string;
}

/** One step of an act the page was armed for (`armStep`), by the index it was armed under. */
export interface ActRecord {
  /** The first trusted `pointerdown` or `keydown` after the arming; null while none came. */
  readonly event: { readonly timeStamp: number; readonly type: string } | null;
  /** The first frame after the event in which the control reads as current and the view's place
   *  shows that it loads, or the result shows (an act that is no view press: its result's). */
  readonly loading: number | null;
  readonly measured: boolean;
  readonly name: string;
  /** The first frame after the event whose document holds the result. */
  readonly result: number | null;
}

/** A row of the first screen as the load ends: its identity, its words for a failure, and the
 *  earliest Element Timing paint of its first line, null where none came. */
export interface FirstScreenRow {
  readonly id: number;
  readonly label: string;
  readonly paint: number | null;
}

/** What the init script collected over one load. */
export interface LoadRecord {
  readonly acts: readonly ActRecord[];
  readonly boxes: readonly BoxEntry[];
  readonly cls: readonly ClsReport[];
  readonly element: readonly ElementEntry[];
  readonly event: readonly EventEntry[];
  /** `performance.now()` at each `loadingdone` of `document.fonts`. */
  readonly fontsDone: readonly number[];
  readonly frames: readonly FrameEntry[];
  readonly lcp: readonly LcpEntry[];
  readonly longtask: readonly TaskEntry[];
  readonly resource: readonly ResourceEntry[];
  /** `performance.timeOrigin`: the navigation's start, in ms since the epoch. */
  readonly timeOrigin: number;
  /** The entry types this browser has no observer of. */
  readonly unobserved: readonly string[];
  /** Whether `web-vitals` was in the page for the init script to call. */
  readonly vitals: boolean;
}

/** A request of the load from its start to its end, in the page's performance time, `end` null
 *  while it is in flight. A WebSocket is `websocket`: Playwright reports a socket apart from the
 *  requests, and the page's live socket (`GET /ws/live`) stays open for the page's life. */
export interface RequestSpan {
  readonly end: number | null;
  readonly kind: "request" | "websocket";
  readonly start: number;
  readonly url: string;
}

/** One load's raw readings, as the capture row prints them and a fixture holds them. */
export interface LoadCapture {
  readonly end: number;
  readonly firstScreen: readonly FirstScreenRow[];
  readonly rate: number;
  readonly record: LoadRecord;
  readonly requests: readonly RequestSpan[];
}

// ---------------------------------------------------------------------------------------------
// The rules.
// ---------------------------------------------------------------------------------------------

/** A task longer than this is a long task, which holds a load open (the spec's "no task longer
 *  than 50 ms"). */
export const LONG_TASK_MS = 50;

/** How long the page must have no request in flight and no long task for the load to have
 *  ended. */
export const LOAD_QUIET_MS = 1_000;

/** The boxes R3 holds after their first drawing: the view row's controls (its toggles, their
 *  counts' labels, and below 1280 px the one control), the header's controls and the header's
 *  band. A group that matches no element at a row's width fails that row (`groupsUndrawn`). */
export const BOX_GROUPS = {
  "view row's controls": [
    '[data-testid="issue-presets"] button',
    '[data-testid="issue-presets"] label',
    '[data-filter-row] button[aria-haspopup="menu"]',
  ],
  "header's controls": ["[data-identity-block] button", '[data-testid="issue-state-actions"] > *'],
  "header's band": ["[data-identity-block]"],
} as const satisfies Readonly<Record<string, readonly string[]>>;

/** The group of the rows in the viewport, which the frame recorder keeps beside `BOX_GROUPS`. */
export const ROWS_GROUP = "rows in the viewport";

/** The group of the row a load names. */
export const NAMED_GROUP = "the named row";

/** When the load ended: one second after the later of the end of the last task longer than
 *  50 ms and the end of the last request, or null where a request is in flight or that second
 *  has not passed by `now`. A WebSocket is no request in flight. With neither, the load ends one
 *  second after the navigation started. */
export function loadEnd(
  tasks: readonly TaskEntry[],
  requests: readonly RequestSpan[],
  now: number
): number | null {
  let last = 0;
  for (const task of tasks) {
    if (task.duration > LONG_TASK_MS) last = Math.max(last, task.startTime + task.duration);
  }
  for (const request of requests) {
    if (request.kind === "websocket") continue;
    if (request.end === null) return null;
    last = Math.max(last, request.end);
  }
  const end = last + LOAD_QUIET_MS;
  return now >= end ? end : null;
}

/** The fourth smallest of five readings, which a bound holds when it meets: one load in five may
 *  read over it. A load that gave no reading (null) counts as larger than any, so two of them
 *  leave no fourth smallest. */
export function fourthSmallest(readings: readonly (number | null)[]): number | null {
  if (readings.length !== 5) {
    throw new Error(`a bound is read over five readings, and this one has ${readings.length}`);
  }
  const sorted = [...readings].sort((a, b) =>
    a === null ? (b === null ? 0 : 1) : b === null ? -1 : a - b
  );
  return sorted[3] ?? null;
}

/** One bound of one row: its name, a single word, the processor's slowing, the five loads'
 *  readings and its limit. */
export interface BoundReading {
  readonly bound: string;
  readonly limit: number;
  readonly rate: number;
  readonly readings: readonly (number | null)[];
}

/** Whether the bound holds: the fourth smallest of its five readings is at most its limit. */
export function boundHolds(reading: BoundReading): boolean {
  const fourth = fourthSmallest(reading.readings);
  return fourth !== null && fourth <= reading.limit;
}

/** A reading, as a line prints it: to three decimals at most, `none` for a load that gave none. */
function printed(value: number | null): string {
  return value === null ? "none" : String(Math.round(value * 1000) / 1000);
}

/** The line a row prints for one bound:
 *  `<bound> rate=<r> readings=<five> limit=<l> fourth=<v> <pass or fail>`. */
export function readingLine(reading: BoundReading): string {
  if (!/^\S+$/.test(reading.bound)) {
    throw new Error(
      `a bound is named in one word, so its line splits on spaces: "${reading.bound}"`
    );
  }
  const fourth = fourthSmallest(reading.readings);
  return [
    reading.bound,
    `rate=${printed(reading.rate)}`,
    `readings=${reading.readings.map(printed).join(",")}`,
    `limit=${printed(reading.limit)}`,
    `fourth=${printed(fourth)}`,
    boundHolds(reading) ? "pass" : "fail",
  ].join(" ");
}

/** The first drawing of the view row, the first frame whose document holds its controls, and the
 *  first drawing of the rows, the first frame whose document holds a row of the view. */
export function firstDrawing(frames: readonly FrameEntry[]): {
  readonly rows: FrameEntry | null;
  readonly viewRow: FrameEntry | null;
} {
  return {
    rows: frames.find((frame) => frame.rows) ?? null,
    viewRow: frames.find((frame) => frame.viewRow) ?? null,
  };
}

/** The paint of the first screen: the latest Element Timing paint among its rows, and each of its
 *  rows that has no paint, which fails the bound whatever the others read. */
export function firstScreenPaint(rows: readonly FirstScreenRow[]): {
  readonly reading: number | null;
  readonly unpainted: readonly string[];
} {
  const painted = rows.flatMap((row) => (row.paint === null ? [] : [row.paint]));
  return {
    reading: painted.length === 0 ? null : Math.max(...painted),
    unpainted: rows.flatMap((row) => (row.paint === null ? [row.label] : [])),
  };
}

/** The largest contentful paint as the load ended: the last candidate reported by then. */
export function largestContentfulPaint(lcp: readonly LcpEntry[], until: number): number | null {
  let last: number | null = null;
  for (const candidate of lcp) {
    if (candidate.startTime <= until) last = candidate.startTime;
  }
  return last;
}

/** The longest task that started after `after`, 0 where none did: the Long Tasks API reports none
 *  shorter than 50 ms. */
export function longestTaskAfter(tasks: readonly TaskEntry[], after: number): number {
  let longest = 0;
  for (const task of tasks) {
    if (task.startTime > after) longest = Math.max(longest, task.duration);
  }
  return longest;
}

/** R2's task reading of a load: its longest task after the first drawing of the view row, a task
 *  that started after the frame that drew it, which is in a task of its own; null where the load
 *  drew no view row. */
export function longestTaskAfterViewRow(
  record: Pick<LoadRecord, "frames" | "longtask">
): number | null {
  const viewRow = firstDrawing(record.frames).viewRow;
  return viewRow === null ? null : longestTaskAfter(record.longtask, viewRow.at);
}

/** The longest Event Timing entry, 0 where none reached the 16 ms the observer reads from. */
export function longestEvent(events: readonly EventEntry[]): number {
  let longest = 0;
  for (const entry of events) longest = Math.max(longest, entry.duration);
  return longest;
}

/** The cumulative layout shift `web-vitals` last reported, null where it reported none. */
export function layoutShift(cls: readonly ClsReport[]): number | null {
  return cls.at(-1)?.value ?? null;
}

/** The path and query of a request's address, which is how a failure names a read. */
function pathOf(address: string): string {
  const url = new URL(address);
  return `${url.pathname}${url.search}`;
}

/** A box that changed after its first drawing, at the frame it changed in. */
export interface BoxMove {
  readonly after: Box;
  /** The frame's `at`. */
  readonly at: number;
  readonly before: Box;
  readonly frame: number;
  readonly group: string;
  readonly label: string;
  /** The requests whose answer ended since the frame before, by path. */
  readonly reads: readonly string[];
}

/** Whether frame `index` laid out the typeface's swap: a `loadingdone` came after the frame
 *  before it and by the frame after it. The event is dispatched after the layout that used the
 *  face, which may be the layout this frame's own recorder made, so a change in the frame just
 *  before the event is the swap as much as one in the frame just after it. */
function swapFrame(
  frames: readonly FrameEntry[],
  fontsDone: readonly number[],
  index: number
): boolean {
  const after = frames[index - 1]?.at ?? Number.NEGATIVE_INFINITY;
  const by = frames[index + 1]?.at ?? Number.POSITIVE_INFINITY;
  return fontsDone.some((time) => time > after && time <= by);
}

/** Every box that changed after its first drawing and up to `until`, the load's end, but a change
 *  in a frame of the typeface's swap (`swapFrame`), which the row of the held typeface bounds.
 *  Each move names the element, its box before and after, and the reads that answered since the
 *  frame before. */
export function boxesMoved(
  record: Pick<LoadRecord, "boxes" | "fontsDone" | "frames" | "resource">,
  until: number
): BoxMove[] {
  const last = new Map<number, Box>();
  const moves: BoxMove[] = [];
  for (const entry of record.boxes) {
    const before = last.get(entry.id);
    last.set(entry.id, entry.box);
    const frame = record.frames[entry.frame];
    if (before === undefined || frame === undefined || frame.at > until) continue;
    if (swapFrame(record.frames, record.fontsDone, entry.frame)) continue;
    const since = record.frames[entry.frame - 1]?.at ?? Number.NEGATIVE_INFINITY;
    moves.push({
      after: entry.box,
      at: frame.at,
      before,
      frame: entry.frame,
      group: entry.group,
      label: entry.label,
      reads: record.resource
        .filter((read) => read.responseEnd > since && read.responseEnd <= frame.at)
        .map((read) => pathOf(read.name)),
    });
  }
  return moves;
}

/** A move in words, for a row's failure. */
export function moveLine(move: BoxMove): string {
  const [before, after] = [move.before, move.after].map(
    (box) => `${box.width}x${box.height} at (${box.x}, ${box.y})`
  );
  return (
    `${move.group} "${move.label}" changed its box at ${Math.round(move.at)} ms, from ` +
    `${before} to ${after}; reads answered since the frame before: ` +
    (move.reads.length === 0 ? "none" : move.reads.join(", "))
  );
}

/** Each group of `BOX_GROUPS` the load drew no element of. */
export function groupsUndrawn(boxes: readonly Pick<BoxEntry, "group">[]): string[] {
  const drawn = new Set(boxes.map((entry) => entry.group));
  return Object.keys(BOX_GROUPS).filter((group) => !drawn.has(group));
}

/** The named row's box as frame `index` was drawn, null where it was not drawn by then. */
export function namedRowAt(record: Pick<LoadRecord, "boxes">, index: number): Box | null {
  let box: Box | null = null;
  for (const entry of record.boxes) {
    if (entry.group === NAMED_GROUP && entry.frame <= index) box = entry.box;
  }
  return box;
}

/** Whether a box stands in the viewport of `frame` under its header: its top is on screen and
 *  below the header's foot. */
export function underHeader(box: Box, frame: Pick<FrameEntry, "foot" | "height">): boolean {
  return box.height > 0 && box.y >= frame.foot - 1 && box.y < frame.height;
}

/** One act's reading. */
export interface ActReading {
  /** The step's name, null where the page was never armed for it. */
  readonly act: string | null;
  /** From the event's `timeStamp` to the frame in which the control reads as current and the
   *  view's place shows that it loads, or the result shows. */
  readonly loading: number | null;
  /** The read the act sent after its event, where it sent one: from the event to the read's
   *  start (`sent`), and from the read's answer to the frame that drew the result (`drawn`). */
  readonly read: {
    readonly drawn: number | null;
    readonly sent: number;
    readonly url: string;
  } | null;
  /** From the event's `timeStamp` to the `requestAnimationFrame` time of the first frame whose
   *  document holds the result; null where no frame did. */
  readonly shown: number | null;
}

/** The reading of the step armed under `index`: from its event to the first frame that shows its
 *  result, and where Resource Timing shows a read the page's code sent (`fetch` or
 *  `xmlhttprequest`) after the event and by that frame, the three parts R2 bounds where a result
 *  needs a read. The read's answer is the last that came before the result's frame. */
export function actReading(
  record: Pick<LoadRecord, "acts" | "frames" | "resource">,
  index: number
): ActReading {
  const act = record.acts[index];
  const event = act?.event ?? null;
  if (act === undefined || event === null) {
    return { act: act?.name ?? null, loading: null, read: null, shown: null };
  }
  const shownAt = act.result === null ? null : (record.frames[act.result]?.time ?? null);
  const loadingAt = act.loading === null ? null : (record.frames[act.loading]?.time ?? null);
  const reads = record.resource
    .filter(
      (entry) =>
        (entry.initiatorType === "fetch" || entry.initiatorType === "xmlhttprequest") &&
        entry.startTime >= event.timeStamp &&
        (shownAt === null || entry.startTime <= shownAt)
    )
    .sort((a, b) => a.startTime - b.startTime);
  const first = reads[0];
  const answered = reads
    .filter((entry) => shownAt === null || entry.responseEnd <= shownAt)
    .reduce((latest, entry) => Math.max(latest, entry.responseEnd), Number.NEGATIVE_INFINITY);
  return {
    act: act.name,
    loading: loadingAt === null ? null : loadingAt - event.timeStamp,
    read:
      first === undefined
        ? null
        : {
            drawn:
              shownAt === null || answered === Number.NEGATIVE_INFINITY ? null : shownAt - answered,
            sent: first.startTime - event.timeStamp,
            url: pathOf(first.name),
          },
    shown: shownAt === null ? null : shownAt - event.timeStamp,
  };
}

// ---------------------------------------------------------------------------------------------
// Acts: what a row presses, and what shows that it was done.
// ---------------------------------------------------------------------------------------------

/** A control a step presses: the first element `selector` finds whose own words, or the words of
 *  the element `within` finds inside it, are `text`; or the first it finds where no label is
 *  given. */
export interface ControlTarget {
  readonly kind: "control";
  readonly label?: { readonly text: string; readonly within: string | null };
  readonly selector: string;
}

/** What a press presses: a control, the fold control of the first row in the viewport under the
 *  header that `row` finds (an open row holding children), or the fold control of the row the
 *  step before folded. */
export type ActTarget =
  | ControlTarget
  | { readonly control: string; readonly kind: "fold"; readonly row: string }
  | { readonly control: string; readonly kind: "folded" };

/** How a step is made: the mouse at its target's box, or a key. */
export type ActInput =
  | { readonly kind: "press"; readonly target: ActTarget }
  | { readonly key: string; readonly kind: "key" };

/** What shows a step was done. `view`: the control `current` finds reads as current and the
 *  document holds a row of a view that was not in it at the event, and the view's place shows it
 *  loads where `busy` is in the document. `expanded`: the row the step pressed the fold control of
 *  reads `aria-expanded` as `value`. `ring`: the focus is on the visible row after (1) or before
 *  (-1) the one it was on at the event, in the viewport under the header. */
export type ActResult =
  | { readonly kind: "absent"; readonly selector: string }
  | { readonly kind: "present"; readonly selector: string }
  | { readonly busy: string; readonly current: ControlTarget; readonly kind: "view" }
  | { readonly kind: "expanded"; readonly value: "false" | "true" }
  | { readonly kind: "ring"; readonly step: -1 | 1 };

/** One step of an act. */
export interface ActStep {
  readonly input: ActInput;
  readonly measured: boolean;
  readonly name: string;
  readonly result: ActResult;
}

/** When a step can be made: at the first drawing of the view row, at the first drawing of the
 *  rows, or now. */
export type ArmWhen = "now" | "rows" | "view row";

// ---------------------------------------------------------------------------------------------
// The browser side.
// ---------------------------------------------------------------------------------------------

/** What a load's instruments are given. */
export interface InstrumentConfig {
  /** The boxes the frame recorder keeps: `BOX_GROUPS` and the rows in the viewport, the named row
   *  alone, or none. */
  readonly boxes: "all" | "named" | "none";
  /** What the viewport's top stands under: the header's band and the view row. */
  readonly header: readonly string[];
  /** The row a load names, by the selector that finds it and words it holds, where it names
   *  one. */
  readonly named: { readonly selector: string; readonly text: string | null } | null;
  /** Every row whose first line Element Timing reads: each view's and the children's
   *  section's. */
  readonly rows: string;
  /** A row of a view. */
  readonly viewRows: string;
}

/** The config the init script reads: the caller's, with the groups it records. */
interface PageConfig extends InstrumentConfig {
  readonly groups: Readonly<Record<string, readonly string[]>>;
  readonly namedGroup: string;
  readonly rowsGroup: string;
  /** The view row's controls, whose first frame is the first drawing of the view row. */
  readonly viewRow: string;
}

/** What the init script puts on the page. */
interface LoadInstruments {
  /** Arms the page for step `index` once `when` has come and its target is drawn: answers the
   *  target's centre, `{ x: -1, y: -1 }` for a key, and null until then. */
  arm(
    index: number,
    step: ActStep,
    when: ArmWhen
  ): { readonly x: number; readonly y: number } | null;
  /** The rows in the viewport under the header now, with their paints. */
  firstScreen(): FirstScreenRow[];
  /** The time now and the long tasks so far, which say whether the load has ended. */
  probe(): { readonly now: number; readonly tasks: readonly TaskEntry[] };
  /** What the init script has collected. */
  record(): LoadRecord;
  /** Whether the step armed under `index` has shown its result. */
  shown(index: number): boolean;
}

declare global {
  interface Window {
    __loadInstruments?: LoadInstruments;
    webVitals?: typeof WebVitals;
  }
}

/** The IIFE build of the installed `web-vitals`, which defines `webVitals` on the page. */
export const WEB_VITALS_IIFE = fileURLToPath(
  new URL("../node_modules/web-vitals/dist/web-vitals.iife.js", import.meta.url)
);

/**
 * The init script. It is serialised into the page, so it reads nothing outside itself but its
 * config; it runs in the top frame alone.
 */
function instrumentPage(config: PageConfig): void {
  if (window.top !== window) return;

  // Element Timing's entry, which no lib of this tree declares; `web-vitals`' types declare the
  // largest contentful paint's and Event Timing's `interactionId`.
  interface ElementTiming extends PerformanceEntry {
    readonly element: Element | null;
    readonly identifier: string;
    readonly loadTime: number;
    readonly renderTime: number;
  }
  interface OpenAct {
    event: { readonly timeStamp: number; readonly type: string } | null;
    loading: number | null;
    readonly measured: boolean;
    readonly name: string;
    result: number | null;
  }
  interface Armed {
    readonly act: OpenAct;
    before: WeakSet<Element>;
    focused: { readonly label: string; readonly row: Element } | null;
    readonly index: number;
    readonly point: { readonly x: number; readonly y: number };
    readonly row: { readonly label: string; readonly row: Element } | null;
    readonly step: ActStep;
  }

  const acts: OpenAct[] = [];
  const boxes: BoxEntry[] = [];
  const cls: ClsReport[] = [];
  const element: ElementEntry[] = [];
  const event: EventEntry[] = [];
  const fontsDone: number[] = [];
  const frames: FrameEntry[] = [];
  const lcp: LcpEntry[] = [];
  const longtask: TaskEntry[] = [];
  const resource: ResourceEntry[] = [];
  const unobserved: string[] = [];

  const ids = new WeakMap<Element, number>();
  let lastId = 0;
  const idOf = (of: Element): number => {
    const known = ids.get(of);
    if (known !== undefined) return known;
    lastId += 1;
    ids.set(of, lastId);
    return lastId;
  };
  const words = (of: Element): string => (of.textContent ?? "").replace(/\s+/g, " ").trim();
  const rowLabel = (row: Element): string => {
    const key =
      row.getAttribute("data-row-key") ??
      row.getAttribute("data-node-id") ??
      row.getAttribute("data-child-entry") ??
      String(idOf(row));
    if (row.hasAttribute("data-child-entry")) return `child ${key}`;
    return `${row.getAttribute("data-node-type") ?? "row"} ${key}`;
  };

  // The earliest paint of each row's first line, by the row: Element Timing's element is the one
  // that owns the first line's text, inside the row.
  const rowPaints = new WeakMap<Element, number>();
  const observe = (
    type: string,
    options: PerformanceObserverInit,
    take: (entry: PerformanceEntry) => void
  ) => {
    if (!PerformanceObserver.supportedEntryTypes.includes(type)) {
      unobserved.push(type);
      return;
    }
    new PerformanceObserver((list) => {
      for (const entry of list.getEntries()) take(entry);
    }).observe({ ...options, buffered: true, type });
  };
  observe("element", {}, (entry) => {
    const timing = entry as ElementTiming;
    const row = timing.element?.closest(config.rows) ?? null;
    const paint = timing.renderTime || timing.loadTime || timing.startTime;
    if (row !== null && !rowPaints.has(row)) rowPaints.set(row, paint);
    element.push({
      identifier: timing.identifier,
      label: row === null ? null : rowLabel(row),
      loadTime: timing.loadTime,
      name: timing.name,
      renderTime: timing.renderTime,
      row: row === null ? null : idOf(row),
      startTime: timing.startTime,
    });
  });
  observe("largest-contentful-paint", {}, (entry) => {
    const paint = entry as LargestContentfulPaint;
    lcp.push({
      element: paint.element?.tagName.toLowerCase() ?? null,
      loadTime: paint.loadTime,
      renderTime: paint.renderTime,
      size: paint.size,
      startTime: paint.startTime,
      url: paint.url,
    });
  });
  observe("longtask", {}, (entry) => {
    longtask.push({ duration: entry.duration, name: entry.name, startTime: entry.startTime });
  });
  observe("event", { durationThreshold: 16 }, (entry) => {
    const timing = entry as PerformanceEventTiming;
    event.push({
      duration: timing.duration,
      interactionId: timing.interactionId,
      name: timing.name,
      processingEnd: timing.processingEnd,
      processingStart: timing.processingStart,
      startTime: timing.startTime,
    });
  });
  observe("resource", {}, (entry) => {
    const timing = entry as PerformanceResourceTiming;
    resource.push({
      duration: timing.duration,
      initiatorType: timing.initiatorType,
      name: timing.name,
      requestStart: timing.requestStart,
      responseEnd: timing.responseEnd,
      responseStart: timing.responseStart,
      startTime: timing.startTime,
    });
  });
  window.webVitals?.onCLS(
    (metric) => {
      cls.push({
        delta: metric.delta,
        shifts: metric.entries.map((shift) => ({
          hadRecentInput: shift.hadRecentInput,
          startTime: shift.startTime,
          value: shift.value,
        })),
        value: metric.value,
      });
    },
    { reportAllChanges: true }
  );
  document.fonts.addEventListener("loadingdone", () => {
    fontsDone.push(performance.now());
  });

  /** The header's foot: the lowest bottom of what the viewport's top stands under. */
  const foot = (): number => {
    let bottom = 0;
    for (const selector of config.header) {
      for (const part of document.querySelectorAll(selector)) {
        const box = part.getBoundingClientRect();
        if (box.height > 0) bottom = Math.max(bottom, box.bottom);
      }
    }
    return bottom;
  };
  const inViewport = (box: DOMRect, top: number): boolean =>
    box.width > 0 && box.height > 0 && box.bottom > top && box.top < window.innerHeight;

  // The frame recorder's boxes: each element's last box, and the rows it has seen in the viewport.
  const lastBoxes = new WeakMap<Element, string>();
  const watchedRows = new WeakSet<Element>();
  let named: Element | null = null;
  const keep = (index: number, kept: Element, group: string, label: () => string) => {
    const rect = kept.getBoundingClientRect();
    if (rect.width === 0 && rect.height === 0) return;
    const box = {
      height: Math.round(rect.height),
      width: Math.round(rect.width),
      x: Math.round(rect.x),
      y: Math.round(rect.y),
    };
    const shape = `${box.x} ${box.y} ${box.width} ${box.height}`;
    if (lastBoxes.get(kept) === shape) return;
    lastBoxes.set(kept, shape);
    boxes.push({ box, frame: index, group, id: idOf(kept), label: label() });
  };
  const findNamed = (): Element | null => {
    const wanted = config.named;
    if (wanted === null) return null;
    if (named?.isConnected === true) return named;
    named =
      [...document.querySelectorAll(wanted.selector)].find(
        (row) => wanted.text === null || words(row).includes(wanted.text)
      ) ?? null;
    return named;
  };
  const recordBoxes = (index: number, top: number) => {
    if (config.boxes === "all") {
      for (const [group, selectors] of Object.entries(config.groups)) {
        for (const control of document.querySelectorAll(selectors.join(", "))) {
          keep(
            index,
            control,
            group,
            () =>
              control.getAttribute("aria-label") ??
              (words(control).slice(0, 48) || control.tagName.toLowerCase())
          );
        }
      }
      for (const row of document.querySelectorAll(config.rows)) {
        if (!watchedRows.has(row)) {
          if (!inViewport(row.getBoundingClientRect(), top)) continue;
          watchedRows.add(row);
        }
        keep(index, row, config.rowsGroup, () => rowLabel(row));
      }
    }
    const row = findNamed();
    if (row !== null) keep(index, row, config.namedGroup, () => rowLabel(row));
  };

  // The act armed, and what a step that folds leaves for the step after it.
  let armed: Armed | null = null;
  let folded: { readonly label: string; readonly row: Element } | null = null;
  const resolve = (held: { readonly label: string; readonly row: Element }): Element | null =>
    held.row.isConnected
      ? held.row
      : ([...document.querySelectorAll(config.viewRows)].find(
          (row) => rowLabel(row) === held.label
        ) ?? null);
  const control = (target: ControlTarget): Element | null => {
    for (const found of document.querySelectorAll(target.selector)) {
      const label = target.label;
      if (label === undefined) return found;
      const holder = label.within === null ? found : found.querySelector(label.within);
      if (holder !== null && words(holder) === label.text) return found;
    }
    return null;
  };
  const foldControl = (row: Element, selector: string): Element | null =>
    [...row.querySelectorAll(selector)].find((found) => found.closest(config.viewRows) === row) ??
    null;
  const target = (
    aim: ActTarget,
    top: number
  ): { readonly control: Element; readonly row: Element | null } | null => {
    if (aim.kind === "control") {
      const found = control(aim);
      return found === null ? null : { control: found, row: null };
    }
    if (aim.kind === "folded") {
      const row = folded === null ? null : resolve(folded);
      const found = row === null ? null : foldControl(row, aim.control);
      return row === null || found === null ? null : { control: found, row };
    }
    for (const row of document.querySelectorAll(aim.row)) {
      if (!inViewport(row.getBoundingClientRect(), top)) continue;
      const found = foldControl(row, aim.control);
      if (found !== null) return { control: found, row };
    }
    return null;
  };
  const ringHolds = (on: Armed, step: -1 | 1, top: number): boolean => {
    const now = document.activeElement?.closest(config.viewRows) ?? null;
    if (now === null) return false;
    const rows = [...document.querySelectorAll(config.viewRows)].filter(
      (row) => row.getClientRects().length > 0
    );
    const from = on.focused === null ? null : resolve(on.focused);
    const want =
      from === null ? (step === 1 ? rows[0] : undefined) : rows[rows.indexOf(from) + step];
    const box = now.getBoundingClientRect();
    return want === now && box.height > 0 && box.top >= top - 1 && box.top < window.innerHeight;
  };
  const resultHolds = (on: Armed, top: number): boolean => {
    const result = on.step.result;
    switch (result.kind) {
      case "present":
        return document.querySelector(result.selector) !== null;
      case "absent":
        return document.querySelector(result.selector) === null;
      case "view":
        return (
          control(result.current) !== null &&
          [...document.querySelectorAll(config.viewRows)].some((row) => !on.before.has(row))
        );
      case "expanded": {
        const row = on.row === null ? null : resolve(on.row);
        return row?.getAttribute("aria-expanded") === result.value;
      }
      case "ring":
        return ringHolds(on, result.step, top);
    }
  };
  const loadsHolds = (on: Armed): boolean => {
    const result = on.step.result;
    return (
      result.kind === "view" &&
      control(result.current) !== null &&
      document.querySelector(result.busy) !== null
    );
  };
  const input = (happened: Event) => {
    if (!happened.isTrusted || armed === null || armed.act.event !== null) return;
    armed.act.event = { timeStamp: happened.timeStamp, type: happened.type };
    const row = document.activeElement?.closest(config.viewRows) ?? null;
    armed.focused = row === null ? null : { label: rowLabel(row), row };
    armed.before = new WeakSet(document.querySelectorAll(config.viewRows));
  };
  window.addEventListener("pointerdown", input, { capture: true });
  window.addEventListener("keydown", input, { capture: true });

  // The frame loop: each frame stamped, its marks read, its boxes kept and the armed act watched.
  let drawnViewRow = false;
  let drawnRows = false;
  const tick = (time: number) => {
    requestAnimationFrame(tick);
    const at = performance.now();
    const index = frames.length;
    const top = foot();
    const viewRow = document.querySelector(config.viewRow) !== null;
    const rows = document.querySelector(config.viewRows) !== null;
    drawnViewRow ||= viewRow;
    drawnRows ||= rows;
    frames.push({
      at,
      foot: Math.round(top),
      height: window.innerHeight,
      rows,
      time,
      viewRow,
    });
    if (config.boxes !== "none") recordBoxes(index, top);
    const on = armed;
    if (on === null || on.act.event === null || on.act.result !== null) return;
    const shown = resultHolds(on, top);
    if (on.act.loading === null && (shown || loadsHolds(on))) on.act.loading = index;
    if (shown) on.act.result = index;
  };
  requestAnimationFrame(tick);

  window.__loadInstruments = {
    arm: (index, step, when) => {
      if (armed?.index === index) return armed.point;
      if (when === "view row" && !drawnViewRow) return null;
      if (when === "rows" && !drawnRows) return null;
      let point = { x: -1, y: -1 };
      let row: { readonly label: string; readonly row: Element } | null = null;
      if (step.input.kind === "press") {
        const found = target(step.input.target, foot());
        if (found === null) return null;
        const box = found.control.getBoundingClientRect();
        if (box.width === 0 || box.height === 0) return null;
        point = { x: box.x + box.width / 2, y: box.y + box.height / 2 };
        row = found.row === null ? null : { label: rowLabel(found.row), row: found.row };
      }
      if (row !== null && step.result.kind === "expanded" && step.result.value === "false") {
        folded = row;
      }
      const act: OpenAct = {
        event: null,
        loading: null,
        measured: step.measured,
        name: step.name,
        result: null,
      };
      acts[index] = act;
      armed = { act, before: new WeakSet(), focused: null, index, point, row, step };
      return point;
    },
    firstScreen: () => {
      const top = foot();
      return [...document.querySelectorAll(config.rows)].flatMap((row) =>
        inViewport(row.getBoundingClientRect(), top)
          ? [{ id: idOf(row), label: rowLabel(row), paint: rowPaints.get(row) ?? null }]
          : []
      );
    },
    probe: () => ({ now: performance.now(), tasks: longtask }),
    record: () => ({
      acts,
      boxes,
      cls,
      element,
      event,
      fontsDone,
      frames,
      lcp,
      longtask,
      resource,
      timeOrigin: performance.timeOrigin,
      unobserved,
      vitals: window.webVitals !== undefined,
    }),
    shown: (index) => acts[index]?.result != null,
  };
}

/** Installs the instruments on `page`, before its first navigation: `web-vitals`' IIFE build,
 *  then the init script that calls it. */
export async function installInstruments(page: Page, config: InstrumentConfig): Promise<void> {
  if (!existsSync(WEB_VITALS_IIFE)) {
    throw new Error(
      `${WEB_VITALS_IIFE} is missing: \`bun install\` at the repository's root installs ` +
        "web-vitals, a devDependency of packages/dispatch"
    );
  }
  // Playwright runs each init script inside a function of its own (`(() => { ... })();`,
  // `InitScript` in playwright-core), so the build's top-level `var webVitals` would stay local to
  // that function: the script names it on the window, where the init script below reads it.
  await page.addInitScript({
    content: `${readFileSync(WEB_VITALS_IIFE, "utf8")}\nwindow.webVitals = webVitals;`,
  });
  await page.addInitScript(instrumentPage, {
    ...config,
    groups: BOX_GROUPS,
    namedGroup: NAMED_GROUP,
    rowsGroup: ROWS_GROUP,
    viewRow: BOX_GROUPS["view row's controls"].join(", "),
  } satisfies PageConfig);
}

/** The requests of one load, which say whether one is in flight. */
export interface LoadRequests {
  /** Every request so far, in the performance time of the page whose time origin is `origin`. */
  spans(origin: number): RequestSpan[];
}

/** Watches every request `page` makes from now on, and every WebSocket it opens, from the
 *  browser's own timing of each where it has one. Start it before the navigation. */
export function watchLoadRequests(page: Page): LoadRequests {
  const open = new Map<Request, { readonly start: number; readonly url: string }>();
  const settled: { readonly end: number; readonly start: number; readonly url: string }[] = [];
  const sockets: { end: number | null; readonly start: number; readonly url: string }[] = [];
  page.on("request", (request) => {
    open.set(request, { start: Date.now(), url: request.url() });
  });
  const settle = (request: Request) => {
    const opened = open.get(request);
    if (opened === undefined) return;
    open.delete(request);
    const timing = request.timing();
    const timed = timing.startTime > 0;
    settled.push({
      end: timed && timing.responseEnd >= 0 ? timing.startTime + timing.responseEnd : Date.now(),
      start: timed ? timing.startTime : opened.start,
      url: opened.url,
    });
  };
  page.on("requestfinished", settle);
  page.on("requestfailed", settle);
  page.on("websocket", (socket) => {
    const span: { end: number | null; start: number; url: string } = {
      end: null,
      start: Date.now(),
      url: socket.url(),
    };
    sockets.push(span);
    socket.on("close", () => {
      span.end = Date.now();
    });
  });
  return {
    spans: (origin) => [
      ...settled.map((span) => ({
        end: span.end - origin,
        kind: "request" as const,
        start: span.start - origin,
        url: span.url,
      })),
      ...[...open.values()].map((span) => ({
        end: null,
        kind: "request" as const,
        start: span.start - origin,
        url: span.url,
      })),
      ...sockets.map((span) => ({
        end: span.end === null ? null : span.end - origin,
        kind: "websocket" as const,
        start: span.start - origin,
        url: span.url,
      })),
    ],
  };
}

/** How long a load may take to end before the row fails on it. */
const LOAD_END_MS = 90_000;

/** How often the end of a load is asked for. */
const LOAD_POLL_MS = 100;

/** The instruments `page` holds, or a failure that says they were installed too late. */
async function instrumented(page: Page): Promise<void> {
  const held = await page.evaluate(() => window.__loadInstruments !== undefined);
  if (!held) {
    throw new Error(
      "the page holds no load instruments: installInstruments runs before the page's first goto"
    );
  }
}

/** Waits until the load has ended (`loadEnd`) and answers when it did, in the page's time. A load
 *  that does not end fails on what holds it open. */
export async function waitForLoadEnd(page: Page, requests: LoadRequests): Promise<number> {
  await instrumented(page);
  const origin = await page.evaluate(() => performance.timeOrigin);
  const found: { end: number | null } = { end: null };
  await expect
    .poll(
      async () => {
        const probe = await page.evaluate(
          () => window.__loadInstruments?.probe() ?? { now: 0, tasks: [] }
        );
        const spans = requests.spans(origin);
        found.end = loadEnd(probe.tasks, spans, probe.now);
        if (found.end !== null) return "ended";
        const flying = spans.filter((span) => span.kind === "request" && span.end === null);
        return flying.length > 0
          ? `in flight: ${flying.map((span) => span.url).join(", ")}`
          : `a request or a task over ${LONG_TASK_MS} ms ended less than ${LOAD_QUIET_MS} ms ago`;
      },
      {
        intervals: [LOAD_POLL_MS],
        message: `the load never had ${LOAD_QUIET_MS} ms with no request in flight and no long task`,
        timeout: LOAD_END_MS,
      }
    )
    .toBe("ended");
  if (found.end === null) throw new Error("the load's end was read and then lost");
  return found.end;
}

/** What the init script collected on `page`. A page whose `web-vitals` did not load, or whose
 *  browser observes no entry type the readers read, fails here rather than reading zeros. */
export async function recordOf(page: Page): Promise<LoadRecord> {
  await instrumented(page);
  const record = await page.evaluate(() => window.__loadInstruments?.record());
  if (record === undefined) throw new Error("the load instruments left the page");
  if (!record.vitals) {
    throw new Error(`the page had no webVitals for the init script to call: ${WEB_VITALS_IIFE}`);
  }
  if (record.unobserved.length > 0) {
    throw new Error(`this browser observes no ${record.unobserved.join(", ")} entries`);
  }
  return record;
}

/** The rows in the viewport under the header now, with their paints: the first screen, read as
 *  the load ends. */
export async function firstScreenOf(page: Page): Promise<FirstScreenRow[]> {
  await instrumented(page);
  return await page.evaluate(() => window.__loadInstruments?.firstScreen() ?? []);
}

/** The shape of what the page drew: the rows (`[data-node-row]`), every element of the document,
 *  and the UTF-8 bytes of the text of `[data-issue-page]`. */
export function pageShape(
  page: Page
): Promise<{ readonly bytes: number; readonly elements: number; readonly rows: number }> {
  return page.evaluate(() => ({
    bytes: new TextEncoder().encode(document.querySelector("[data-issue-page]")?.textContent ?? "")
      .length,
    elements: document.getElementsByTagName("*").length,
    rows: document.querySelectorAll("[data-node-row]").length,
  }));
}

/** How long a step's target may take to be drawn. */
const ARM_MS = 60_000;

/** Arms `page` for `step` under `index` once `when` has come and its target is drawn, polled at
 *  each frame so a step made at a first drawing is made at the frame that drew it. Answers where
 *  a press lands: the target's centre, as that frame drew it. */
export async function armStep(
  page: Page,
  index: number,
  step: ActStep,
  when: ArmWhen
): Promise<{ readonly x: number; readonly y: number }> {
  try {
    const point = await page.waitForFunction(
      (arming) => window.__loadInstruments?.arm(arming.index, arming.step, arming.when) ?? null,
      { index, step, when },
      { polling: "raf", timeout: ARM_MS }
    );
    return (await point.jsonValue()) ?? { x: -1, y: -1 };
  } catch (error) {
    if (!(error instanceof errors.TimeoutError)) throw error;
    throw new Error(
      `${step.name} could not be made within ${ARM_MS} ms: ${
        step.input.kind === "press"
          ? `nothing drew its target, ${JSON.stringify(step.input.target)}`
          : `the page never drew ${when === "rows" ? "a row" : "the view row"}`
      }`
    );
  }
}

/** Makes `step`'s input: the mouse at `point`, where its target was drawn, or its key. */
export async function makeInput(
  page: Page,
  step: ActStep,
  point: { readonly x: number; readonly y: number }
): Promise<void> {
  if (step.input.kind === "key") {
    await page.keyboard.press(step.input.key);
  } else {
    await page.mouse.click(point.x, point.y);
  }
}

/** How often a step's result is asked for. */
const SHOWN_POLL_MS = 50;

/** Whether the step armed under `index` shows its result within `timeout`. */
export async function resultShown(page: Page, index: number, timeout: number): Promise<boolean> {
  try {
    await page.waitForFunction(
      (armedAt) => window.__loadInstruments?.shown(armedAt) === true,
      index,
      {
        polling: SHOWN_POLL_MS,
        timeout,
      }
    );
    return true;
  } catch (error) {
    if (error instanceof errors.TimeoutError) return false;
    throw error;
  }
}
