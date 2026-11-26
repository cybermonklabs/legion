/** the rows profile the longest main-thread task of the new issue page's loads and Enter in
 *  Chromium, its processor slowed, on an issue the server seeds at the largest issue's shape,
 *  which the server's real answers, live socket and document room decide */

/**
 * Every test in this file skips unless `ISSUE_PAGE_LOAD_LONGEST=1`, so routine CI legs of
 * `chromium-load` and `iphone-load` do not run it. When enabled, for each view the load rows read,
 * it makes three cold loads of the seed the clock rows use
 * (`seedLargestShape`, through `test` of `e2e/issue-page-load.ts`), at 1440 by 900 with the
 * processor slowed by 2 in `chromium-load` and at 390 px (Playwright's iPhone 13, in Chromium)
 * slowed by 4 in `iphone-load`. Each load opens as the load rows open theirs (`oneLoad`): a new
 * context signed in as the reader with the new page on, the instruments of `e2e/load-instruments.ts`
 * installed as Row 1 installs them (every box recorded), the slowing sent through a CDP session on
 * the page before the navigation; and beside those, through the same session, V8's sampling
 * profiler (`Profiler`, one sample each 100 µs), started before the navigation and stopped once the
 * load has ended. No Playwright trace is recorded: the load projects turn tracing off, and a trace's
 * snapshots of the document are work in the page that the profile would hold.
 *
 * Each load prints:
 *
 * - `LONGEST <view> <width> <run> start=<ms> duration=<ms>`: the longest task after the first
 *   drawing of the view row, the one the bound reads (`longestTaskAfterViewRow`), in the page's
 *   performance time;
 * - `LONGEST-FN <view> <width> <run> <selfMs> <function> <file>:<line>
 *   min=<name>@<chunk>:<line>:<column>`: the 25 functions with the most self time among the
 *   profile's samples inside that task's window, most first;
 * - `LONGEST-TOTAL <view> <width> <run> <totalMs> <function> <file>:<line>`: the 25 functions with
 *   the most time on the stack in that window, their callees' included, each counted once in a
 *   sample however often it stands in that sample's stack, so a component or a hook shows what it
 *   costs with its children;
 * - `LONGEST-TIME <view> <width> <run> gc=<ms> program=<ms> idle=<ms> js=<ms> sampled=<ms>`: the
 *   window's samples by kind. A CPU profile tells only the garbage collector apart from the rest of
 *   the engine's own work: style, layout and paint are inside `(program)`, with every other native
 *   task.
 *
 * At each width, `the longest task of Enter on a row` makes three more cold loads of Conversation,
 * the box recorder off as in the act rows, and once each has ended, `Enter` as Row 5 makes it on
 * the first row of the outline that is a document's block: a press on the row's words, then the
 * key once the page has gone quiet again. The profiler runs from just before the key to the frame
 * that shows the caret. Each prints `ENTER <width> <run> result=<ms> read-sent=<ms>
 * read-drawn=<ms>` (`actReading`: `n/a` where the result needed no read, `none` where it did not
 * show), the longest task in the window from the key's `timeStamp` to that frame as the lines above
 * print it with the view `enter`, and `ENTER-REQUEST <width> <run> <path> duration=<ms>` for each
 * request sent in that window, `open` where it had not ended.
 *
 * Each frame is mapped to its original file, line and name through the build's source maps, with
 * `source-map-js`, which the repository holds in bun's store alone and so is required by its path
 * there (`SOURCE_MAP_JS`). The Job builds the page with `--sourcemap` and moves the maps out of
 * `web/dist`, whose server refuses an asset over 8 MiB, into `web/sourcemaps/assets/<chunk>.map`;
 * the chunk's own code stays in `web/dist/assets`. A frame's file and line are its function's start
 * mapped; its name is the map's name for the minified name's token nearest that start, or the
 * minified name where the map gives none. A frame no map holds prints `(unmapped)` beside its
 * minified place, and a load whose maps or package are missing says so once in a `LONGEST-NOTE`.
 * The minified place counts the line and the column from 1, as a stack trace does.
 *
 * No line counts the `useQuery` observers made in the window: a sampling profile counts no calls,
 * and CDP's precise coverage, which does, counts over the whole time between two reads of it and
 * slows the page while it is on, so it would read the window under other conditions than the rows.
 *
 * The page's clock and the profile's are lined up by an anchor: once the load has ended, the page
 * runs a named function (`loadProbeAnchor`) for 30 ms from a time it reads, and the profile's first
 * sample inside that function stands for that time. The lining is off by at most one sample's
 * interval and the call's own start.
 */

import { existsSync, readFileSync } from "node:fs";
import { createRequire } from "node:module";
import { basename } from "node:path";
import { fileURLToPath } from "node:url";
import type { Browser, Page } from "@playwright/test";

import { presetPath } from "../web/src/features/issue-page/issue-address";
import type { Preset } from "../web/src/features/issue-page/page-words";
import {
  AFTER_LOAD_ENTERS,
  HEADER_SELECTORS,
  LOAD_ROW_SELECTOR,
  LOAD_VIEWS,
  loadCpuRate,
  loadWidth,
  makeSteps,
  READER,
  test,
  VIEW_ROW_SELECTOR,
} from "./issue-page-load";
import { withIssuePage } from "./issue-page-option";
import { owedUnless } from "./issue-page-t-tree";
import {
  type ActReading,
  actReading,
  firstDrawing,
  installInstruments,
  type LoadRecord,
  type RequestSpan,
  recordOf,
  type TaskEntry,
  waitForLoadEnd,
  watchLoadRequests,
} from "./load-instruments";
import { refuseASharedRun } from "./main-thread-hold";
import { asUser } from "./users";

/** How many cold loads each view is profiled over. */
const RUNS = 3;

/** The profiler's sampling interval, in µs. */
const SAMPLING_US = 100;

/** How many functions each load prints, by their self time and by their total time in the task. */
const TOP_FUNCTIONS = 25;

/** How long the anchor runs, in ms. */
const ANCHOR_MS = 30;

/** How long one view's three loads may take. */
const PROBE_MS = 600_000;

/** The projects the probe runs in: the clock rows' own. */
const LOAD_PROJECTS = ["chromium-load", "iphone-load"];

/** `source-map-js` in bun's store, from the repository's root: no package of the repository
 *  depends on it itself, so it is in no `node_modules` a module name resolves in. */
const SOURCE_MAP_JS = fileURLToPath(
  new URL(
    "../../../node_modules/.bun/source-map-js@1.2.1/node_modules/source-map-js",
    import.meta.url
  )
);

/** Where the Job puts the build's maps, and where the chunks they map stay. */
const MAPS = fileURLToPath(new URL("../web/sourcemaps/assets/", import.meta.url));
const CHUNKS = fileURLToPath(new URL("../web/dist/assets/", import.meta.url));

type SourceMaps =
  typeof import("../../../node_modules/.bun/source-map-js@1.2.1/node_modules/source-map-js");
type Consumer = InstanceType<SourceMaps["SourceMapConsumer"]>;

/** `source-map-js`, required once; null where it is not at `SOURCE_MAP_JS`. */
let sourceMaps: SourceMaps | null | undefined;
function sourceMapsOf(): SourceMaps | null {
  if (sourceMaps === undefined) {
    sourceMaps = existsSync(SOURCE_MAP_JS)
      ? (createRequire(import.meta.url)(SOURCE_MAP_JS) as SourceMaps)
      : null;
    if (sourceMaps === null) {
      console.log(`LONGEST-NOTE source-map-js is not at ${SOURCE_MAP_JS}: no frame is mapped`);
    }
  }
  return sourceMaps;
}

/** A chunk's map and its code by lines, read once each; null where either is missing. */
const chunks = new Map<
  string,
  { readonly code: readonly string[]; readonly map: Consumer } | null
>();
function chunkOf(url: string) {
  if (!URL.canParse(url)) return null;
  const path = new URL(url).pathname;
  if (!path.startsWith("/assets/")) return null;
  const chunk = basename(path);
  const held = chunks.get(chunk);
  if (held !== undefined) return held;
  const maps = sourceMapsOf();
  let read: { readonly code: readonly string[]; readonly map: Consumer } | null = null;
  if (maps !== null) {
    try {
      read = {
        code: readFileSync(`${CHUNKS}${chunk}`, "utf8").split("\n"),
        map: new maps.SourceMapConsumer(JSON.parse(readFileSync(`${MAPS}${chunk}.map`, "utf8"))),
      };
    } catch (error) {
      console.log(`LONGEST-NOTE no map of ${chunk} is read: ${String(error)}`);
    }
  }
  chunks.set(chunk, read);
  return read;
}

/** A map's source as a path of the repository: under its package's `web/src`, or the package of
 *  `node_modules` it is in. */
function shortSource(source: string): string {
  const repository = source.replace(/^(\.\.\/)+/, "");
  const modules = repository.match(/node_modules\/(?:\.bun\/[^/]+\/node_modules\/)?(.*)$/);
  if (modules !== null) return `node_modules/${modules[1] ?? ""}`;
  return repository.startsWith("src/") ? `packages/dispatch/web/${repository}` : repository;
}

/** A frame as a line prints it: its original name and place, and its minified ones. */
interface Mapped {
  /** The function's original name, or its minified one where the map names none. */
  readonly name: string;
  /** Its original `file:line`, `(native)` for a frame of no script, `(unmapped)` where no map holds
   *  it. */
  readonly place: string;
  /** Its minified name and place in the bundle, the line and the column counted from 1. */
  readonly minified: string;
}

const mappedFrames = new Map<string, Mapped>();

/** `frame` mapped through its chunk's map, once for each frame. */
function mapFrame(frame: ProfileNode["callFrame"]): Mapped {
  const key = keyOf(frame);
  const held = mappedFrames.get(key);
  if (held !== undefined) return held;
  const minifiedName = frame.functionName === "" ? "(anonymous)" : frame.functionName;
  let mapped: Mapped;
  if (frame.url === "") {
    mapped = { minified: `${minifiedName}@(native)`, name: minifiedName, place: "(native)" };
  } else {
    const chunk = chunkOf(frame.url);
    const minified = `${minifiedName}@${basename(new URL(frame.url).pathname)}:${
      frame.lineNumber + 1
    }:${frame.columnNumber + 1}`;
    const start = chunk?.map.originalPositionFor({
      column: frame.columnNumber,
      line: frame.lineNumber + 1,
    });
    const place =
      start?.source == null ? "(unmapped)" : `${shortSource(start.source)}:${start.line}`;
    // The original name: the map's name of the minified name's token nearest the function's
    // start, on the start's line of the chunk.
    let name = minifiedName;
    if (chunk !== null && frame.functionName !== "") {
      const text = chunk.code[frame.lineNumber] ?? "";
      const from = Math.max(0, frame.columnNumber - 400);
      const token = new RegExp(
        `(?<![\\w$])${frame.functionName.replace(/[$]/g, "\\$")}(?![\\w$])`,
        "g"
      );
      let nearest: number | undefined;
      for (const found of text.slice(from, frame.columnNumber + 400).matchAll(token)) {
        const at = from + (found.index ?? 0);
        if (
          nearest === undefined ||
          Math.abs(at - frame.columnNumber) < Math.abs(nearest - frame.columnNumber)
        ) {
          nearest = at;
        }
      }
      const named =
        nearest === undefined
          ? null
          : chunk.map.originalPositionFor({ column: nearest, line: frame.lineNumber + 1 }).name;
      if (named !== null && named !== undefined && named !== "") name = named;
    }
    mapped = { minified, name, place };
  }
  mappedFrames.set(key, mapped);
  return mapped;
}

/** A frame's identity: one function's start in one script. */
function keyOf(frame: ProfileNode["callFrame"]): string {
  return `${frame.functionName} ${frame.url}:${frame.lineNumber}:${frame.columnNumber}`;
}

/** Whether `frame` is one of the engine's own nodes - the garbage collector, idle, the rest of its
 *  work, the root - which are no function. */
function engineFrame(frame: ProfileNode["callFrame"]): boolean {
  return frame.url === "" && frame.functionName.startsWith("(");
}

/** The part of a V8 CPU profile this probe reads (CDP's `Profiler.Profile`). */
interface ProfileNode {
  readonly callFrame: {
    readonly columnNumber: number;
    readonly functionName: string;
    readonly lineNumber: number;
    readonly url: string;
  };
  readonly children?: readonly number[];
  readonly id: number;
}

interface CpuProfile {
  readonly nodes: readonly ProfileNode[];
  readonly samples?: readonly number[];
  readonly startTime: number;
  readonly timeDeltas?: readonly number[];
}

/** One sample of a profile: the node on top of the stack, from when it was taken, in the profile's
 *  µs, for as long as until the next sample. */
interface Sample {
  readonly at: number;
  readonly lasts: number;
  readonly node: ProfileNode;
}

/** One load, profiled. */
interface ProfiledLoad {
  /** The page's performance time at which the anchor started. */
  readonly anchoredAt: number;
  readonly profile: CpuProfile;
  readonly record: LoadRecord;
}

/** Runs the anchor the profile's clock is lined up by (`profileClock`): a named function that
 *  spins for `ANCHOR_MS`; answers the page's performance time at its start. */
function anchor(page: Page): Promise<number> {
  return page.evaluate((ms) => {
    const from = performance.now();
    // Named, so the profile's samples of it are found by its name.
    function loadProbeAnchor(until: number): number {
      let spins = 0;
      while (performance.now() < until) spins += 1;
      return spins;
    }
    loadProbeAnchor(from + ms);
    return from;
  }, ANCHOR_MS);
}

/** One cold load of `address`, opened as `oneLoad` of `e2e/issue-page-load.ts` opens it with the
 *  instruments `boxes` asks for, and the CDP session it is slowed through, with the profiler
 *  enabled at its sampling interval and not yet started. */
async function openLoad(browser: Browser, rate: number, address: string, boxes: "all" | "none") {
  const context = await asUser(browser, READER);
  try {
    await withIssuePage(context, READER);
    const page = await context.newPage();
    await installInstruments(page, {
      boxes,
      header: HEADER_SELECTORS,
      named: null,
      rows: LOAD_ROW_SELECTOR,
      viewRows: VIEW_ROW_SELECTOR,
    });
    const cdp = await context.newCDPSession(page);
    await cdp.send("Emulation.setCPUThrottlingRate", { rate });
    await cdp.send("Profiler.enable");
    await cdp.send("Profiler.setSamplingInterval", { interval: SAMPLING_US });
    const requests = watchLoadRequests(page);
    return {
      cdp,
      context,
      goto: () => page.goto(address, { waitUntil: "commit" }),
      page,
      requests,
    };
  } catch (error) {
    await context.close();
    throw error;
  }
}

/** One cold load of `address`, as the load rows of the views open theirs (every box recorded),
 *  with the profiler running from before the navigation to the load's end and the anchor run
 *  after it. */
async function profiledLoad(
  browser: Browser,
  rate: number,
  address: string
): Promise<ProfiledLoad> {
  const load = await openLoad(browser, rate, address, "all");
  try {
    await load.cdp.send("Profiler.start");
    await load.goto();
    await waitForLoadEnd(load.page, load.requests);
    const record = await recordOf(load.page);
    const anchoredAt = await anchor(load.page);
    const { profile } = await load.cdp.send("Profiler.stop");
    return { anchoredAt, profile, record };
  } finally {
    await load.context.close();
  }
}

/** One `Enter`, profiled. */
interface ProfiledEnter {
  /** The index the `Enter` was armed under. */
  readonly index: number;
  readonly load: ProfiledLoad;
  /** Every request of the load, in its page's performance time. */
  readonly spans: readonly RequestSpan[];
}

/** `Enter` on the first row of Conversation's outline that is a document's block, once a cold load
 *  of `address` has ended, made as Row 5 makes `Enter in Conversation on a row of the first page`:
 *  a press on the row's words, which puts the focus on it, then, once the page has gone quiet
 *  again, the key, with the box recorder off as in the act rows. The profiler runs from just before
 *  the key until the frame that showed the caret has been read. */
async function profiledEnter(
  browser: Browser,
  rate: number,
  address: string
): Promise<ProfiledEnter> {
  const act = AFTER_LOAD_ENTERS.find(
    (each) => each.on === "conversation" && each.paging === undefined
  );
  const [focus, enter] = act?.steps ?? [];
  if (focus === undefined || enter === undefined) {
    throw new Error("Row 5 makes no Enter in Conversation on a row of the first page");
  }
  const load = await openLoad(browser, rate, address, "none");
  try {
    await load.goto();
    await waitForLoadEnd(load.page, load.requests);
    const focused = await makeSteps(load.page, 0, [focus], "now");
    await waitForLoadEnd(load.page, load.requests);
    await load.cdp.send("Profiler.start");
    const made = await makeSteps(load.page, focused.next, [enter], "now");
    const anchoredAt = await anchor(load.page);
    const { profile } = await load.cdp.send("Profiler.stop");
    const record = await recordOf(load.page);
    return {
      index: made.last,
      load: { anchoredAt, profile, record },
      spans: load.requests.spans(record.timeOrigin),
    };
  } finally {
    await load.context.close();
  }
}

/** Every sample of `profile`, in the order taken. */
function samplesOf(profile: CpuProfile): Sample[] {
  const nodes = new Map(profile.nodes.map((node) => [node.id, node]));
  const ids = profile.samples ?? [];
  const deltas = profile.timeDeltas ?? [];
  const times: number[] = [];
  let at = profile.startTime;
  for (const delta of deltas.slice(0, ids.length)) {
    at += delta;
    times.push(at);
  }
  return ids.flatMap((id, index) => {
    const node = nodes.get(id);
    const taken = times[index];
    if (node === undefined || taken === undefined) return [];
    return [{ at: taken, lasts: (times[index + 1] ?? taken) - taken, node }];
  });
}

/** Each node's parent in `profile`'s tree of calls, by id. */
function parentsOf(profile: CpuProfile): Map<number, number> {
  const parents = new Map<number, number>();
  for (const node of profile.nodes) {
    for (const child of node.children ?? []) parents.set(child, node.id);
  }
  return parents;
}

/** Where the page's performance time `ms` stands on the profile's clock, in µs: lined up by the
 *  first sample whose stack holds the anchor. Null where the profile holds no sample of it. */
function profileClock(
  load: ProfiledLoad,
  samples: readonly Sample[],
  parents: ReadonlyMap<number, number>
): ((ms: number) => number) | null {
  const anchors = new Set(
    load.profile.nodes
      .filter((node) => node.callFrame.functionName === "loadProbeAnchor")
      .map((node) => node.id)
  );
  const inAnchor = (id: number): boolean => {
    for (let at: number | undefined = id; at !== undefined; at = parents.get(at)) {
      if (anchors.has(at)) return true;
    }
    return false;
  };
  const first = samples.find((sample) => inAnchor(sample.node.id));
  if (first === undefined) return null;
  return (ms) => first.at + (ms - load.anchoredAt) * 1000;
}

/** The longest task after the first drawing of the view row, the one R2's task bound reads, null
 *  where the load drew no view row or no task came after it. */
function longestTask(record: LoadRecord): TaskEntry | null {
  const viewRow = firstDrawing(record.frames).viewRow;
  if (viewRow === null) return null;
  let longest: TaskEntry | null = null;
  for (const task of record.longtask) {
    if (task.startTime > viewRow.at && task.duration > (longest?.duration ?? 0)) longest = task;
  }
  return longest;
}

/** A ms reading as a line prints it: to a tenth. */
function shown(ms: number): string {
  return (Math.round(ms * 10) / 10).toFixed(1);
}

/** The engine's own nodes, which a profile names in parentheses. */
const ENGINE = {
  gc: "(garbage collector)",
  idle: "(idle)",
  program: "(program)",
} as const;

/** Prints what `task` of one load ran, by its samples in the profile. */
function printLongest(
  view: string,
  width: number,
  run: number,
  load: ProfiledLoad,
  task: TaskEntry | null
): void {
  const label = `${view} ${width} ${run}`;
  if (task === null) {
    console.log(`LONGEST ${label} start=none duration=none`);
    return;
  }
  console.log(`LONGEST ${label} start=${shown(task.startTime)} duration=${shown(task.duration)}`);
  const samples = samplesOf(load.profile);
  const parents = parentsOf(load.profile);
  const clock = profileClock(load, samples, parents);
  if (clock === null) {
    console.log(
      `LONGEST-ERROR ${label} the profile holds no sample of the anchor, so the profiler did not ` +
        "follow the page through its navigation and no window is read"
    );
    return;
  }
  const from = clock(task.startTime);
  const to = clock(task.startTime + task.duration);
  const inside = samples.filter((sample) => sample.at >= from && sample.at < to);
  const nodes = new Map(load.profile.nodes.map((node) => [node.id, node]));
  // Each node's stack as the functions on it, each once: a function that recurses stands in a
  // sample's stack once, so its total is the time it was anywhere on the stack.
  const stacks = new Map<number, readonly ProfileNode["callFrame"][]>();
  const callStackOf = (leaf: ProfileNode): readonly ProfileNode["callFrame"][] => {
    const held = stacks.get(leaf.id);
    if (held !== undefined) return held;
    const seen = new Map<string, ProfileNode["callFrame"]>();
    for (let at: number | undefined = leaf.id; at !== undefined; at = parents.get(at)) {
      const frame = nodes.get(at)?.callFrame;
      if (frame !== undefined && !engineFrame(frame)) seen.set(keyOf(frame), frame);
    }
    const stack = [...seen.values()];
    stacks.set(leaf.id, stack);
    return stack;
  };
  const self = new Map<string, { ms: number; readonly frame: ProfileNode["callFrame"] }>();
  const total = new Map<string, { ms: number; readonly frame: ProfileNode["callFrame"] }>();
  const add = (
    into: Map<string, { ms: number; readonly frame: ProfileNode["callFrame"] }>,
    frame: ProfileNode["callFrame"],
    ms: number
  ) => {
    const key = keyOf(frame);
    const held = into.get(key);
    if (held === undefined) into.set(key, { frame, ms });
    else held.ms += ms;
  };
  const kinds = { gc: 0, idle: 0, js: 0, program: 0 };
  for (const sample of inside) {
    const ms = sample.lasts / 1000;
    const frame = sample.node.callFrame;
    if (frame.functionName === ENGINE.gc) kinds.gc += ms;
    else if (frame.functionName === ENGINE.idle) kinds.idle += ms;
    else if (frame.functionName === ENGINE.program) kinds.program += ms;
    else kinds.js += ms;
    for (const held of callStackOf(sample.node)) add(total, held, ms);
    // The engine's own nodes are no function: `LONGEST-TIME` reads them.
    if (!engineFrame(frame)) add(self, frame, ms);
  }
  const most = (of: Map<string, { ms: number; readonly frame: ProfileNode["callFrame"] }>) =>
    [...of.values()].sort((a, b) => b.ms - a.ms).slice(0, TOP_FUNCTIONS);
  for (const entry of most(self)) {
    const mapped = mapFrame(entry.frame);
    console.log(
      `LONGEST-FN ${label} ${shown(entry.ms)} ${mapped.name} ${mapped.place} min=${mapped.minified}`
    );
  }
  for (const entry of most(total)) {
    const mapped = mapFrame(entry.frame);
    console.log(`LONGEST-TOTAL ${label} ${shown(entry.ms)} ${mapped.name} ${mapped.place}`);
  }
  const sampled = kinds.gc + kinds.idle + kinds.js + kinds.program;
  console.log(
    `LONGEST-TIME ${label} gc=${shown(kinds.gc)} program=${shown(kinds.program)} ` +
      `idle=${shown(kinds.idle)} js=${shown(kinds.js)} sampled=${shown(sampled)}`
  );
}

for (const view of LOAD_VIEWS) {
  // SLOW(>=2s): three cold loads of the seeded issue at the slowed rate, each profiled to its end.
  test(`the longest task of a load of ${view.name}'s first screen`, async ({
    browser,
    loadSeeds,
  }, testInfo) => {
    test.skip(
      process.env.ISSUE_PAGE_LOAD_LONGEST !== "1",
      "the longest-task probes run only where ISSUE_PAGE_LOAD_LONGEST=1, so CI load legs stay fast"
    );
    test.skip(
      !LOAD_PROJECTS.includes(testInfo.project.name),
      "the probe profiles the clock rows' loads, which only chromium-load and iphone-load make"
    );
    refuseASharedRun();
    owedUnless(view.gate);
    test.setTimeout(PROBE_MS);
    const rate = loadCpuRate(testInfo.project.name);
    const width = loadWidth(testInfo);
    const seeded = await loadSeeds.shape();
    const address = presetPath(seeded.key, view.view as Preset);
    for (let run = 1; run <= RUNS; run += 1) {
      const load = await profiledLoad(browser, rate, address);
      printLongest(view.view, width, run, load, longestTask(load.record));
    }
  });
}

/** A reading of an act's part as a line prints it: `n/a` where the result needed no read, `none`
 *  where it did not show. */
function part(ms: number | null | undefined): string {
  if (ms === undefined) return "n/a";
  return ms === null ? "none" : shown(ms);
}

/** The longest task that ran in the window from `from` to `to`, in the page's performance time:
 *  one that started before its end and ended after its start. */
function longestIn(record: LoadRecord, from: number, to: number): TaskEntry | null {
  let longest: TaskEntry | null = null;
  for (const task of record.longtask) {
    const inside = task.startTime < to && task.startTime + task.duration > from;
    if (inside && task.duration > (longest?.duration ?? 0)) longest = task;
  }
  return longest;
}

/** Prints one `Enter`'s reading (`actReading`), the longest task in its window - from the key's
 *  `timeStamp` to the frame that showed the caret was shown - as `printLongest` prints it, and
 *  every request sent in that window, by its path and how long it took. */
function printEnter(width: number, run: number, made: ProfiledEnter): void {
  const { record } = made.load;
  const reading: ActReading = actReading(record, made.index);
  const read = reading.read;
  console.log(
    `ENTER ${width} ${run} result=${part(reading.shown)} ` +
      `read-sent=${part(read?.sent)} read-drawn=${part(read?.drawn)}`
  );
  const from = record.acts[made.index]?.event?.timeStamp;
  if (from === undefined) return;
  // Where no frame showed the caret, the window runs to the last frame the record holds.
  const to = reading.shown === null ? (record.frames.at(-1)?.at ?? from) : from + reading.shown;
  printLongest("enter", width, run, made.load, longestIn(record, from, to));
  for (const span of made.spans) {
    if (span.start < from || span.start >= to) continue;
    const path = URL.canParse(span.url)
      ? new URL(span.url).pathname + new URL(span.url).search
      : span.url;
    const lasted = span.end === null ? "open" : shown(span.end - span.start);
    console.log(`ENTER-REQUEST ${width} ${run} ${path} duration=${lasted}`);
  }
}

// SLOW(>=2s): three cold loads of the seeded issue at the slowed rate, each waited out, with Enter
// on a row made and profiled in it.
test("the longest task of Enter on a row", async ({ browser, loadSeeds }, testInfo) => {
  test.skip(
    process.env.ISSUE_PAGE_LOAD_LONGEST !== "1",
    "the longest-task probes run only where ISSUE_PAGE_LOAD_LONGEST=1, so CI load legs stay fast"
  );
  test.skip(
    !LOAD_PROJECTS.includes(testInfo.project.name),
    "the probe profiles the clock rows' loads, which only chromium-load and iphone-load make"
  );
  refuseASharedRun();
  test.setTimeout(PROBE_MS);
  const rate = loadCpuRate(testInfo.project.name);
  const width = loadWidth(testInfo);
  const seeded = await loadSeeds.shape();
  const address = presetPath(seeded.key, "conversation");
  for (let run = 1; run <= RUNS; run += 1) {
    printEnter(width, run, await profiledEnter(browser, rate, address));
  }
});

// ---------------------------------------------------------------------------------------------
// Decision 544: the room's Yjs sync apply on the first Enter, and the block editor's build.
// Runs only in iphone-load (390 px, CPU rate 4), five cold loads of Conversation.
// ---------------------------------------------------------------------------------------------

interface M544Sync {
  readonly keyTimeStamp: number;
  readonly syncedAt: number;
  readonly applyMs: number;
  readonly applyCount: number;
  readonly transportImportMs: number;
}

interface M544Build {
  readonly importMs: number;
  readonly buildMs: number;
  readonly warmBuildMs: number;
  readonly blockIdKept: boolean;
  readonly blockId: string | null;
  readonly rootType: string | null;
  readonly rootBlockId: string | null;
  readonly textAfterTyping: string;
  readonly textAfterUndo: string;
}

interface M544Run {
  readonly run: number;
  readonly sync: M544Sync;
  readonly syncLongest: TaskEntry | null;
  readonly build: M544Build;
  readonly buildLongest: TaskEntry | null;
  readonly syncLoad: ProfiledLoad;
  readonly buildLoad: ProfiledLoad;
  readonly requests: readonly string[];
}

/** The longest task that started inside `[from, to)`, in the page's performance time. */
function longestStartingIn(record: LoadRecord, from: number, to: number): TaskEntry | null {
  let longest: TaskEntry | null = null;
  for (const task of record.longtask) {
    if (task.startTime >= from && task.startTime < to && task.duration > (longest?.duration ?? 0)) {
      longest = task;
    }
  }
  return longest;
}

/** One cold load of Conversation, with `window.__measure544` from the app patch. */
async function measure544Run(
  browser: Browser,
  rate: number,
  address: string,
  run: number
): Promise<M544Run> {
  const load = await openLoad(browser, rate, address, "none");
  try {
    await load.goto();
    await waitForLoadEnd(load.page, load.requests);

    await load.cdp.send("Profiler.start");
    const syncPromise = load.page.evaluate(async () => {
      const measure = window.__measure544;
      if (measure === undefined)
        throw new Error("window.__measure544 is missing (apply the app patch)");
      return await measure.armSyncOnEnter();
    });
    await load.page.evaluate(() => {});
    await load.page.keyboard.press("Enter");
    const sync = await syncPromise;
    const syncRecord = await recordOf(load.page);
    const syncAnchoredAt = await anchor(load.page);
    const { profile: syncProfile } = await load.cdp.send("Profiler.stop");
    const syncLoad: ProfiledLoad = {
      anchoredAt: syncAnchoredAt,
      profile: syncProfile,
      record: syncRecord,
    };

    await load.cdp.send("Profiler.start");
    const build = await load.page.evaluate(async () => {
      const measure = window.__measure544;
      if (measure === undefined)
        throw new Error("window.__measure544 is missing (apply the app patch)");
      return await measure.measureBlockEditor();
    });
    const buildRecord = await recordOf(load.page);
    const buildAnchoredAt = await anchor(load.page);
    const { profile: buildProfile } = await load.cdp.send("Profiler.stop");
    const buildLoad: ProfiledLoad = {
      anchoredAt: buildAnchoredAt,
      profile: buildProfile,
      record: buildRecord,
    };

    const requests = load.requests
      .spans(syncRecord.timeOrigin)
      .filter((span) => span.start >= sync.keyTimeStamp && span.kind === "request")
      .map((span) => span.url);

    return {
      build,
      buildLoad,
      buildLongest: longestStartingIn(buildRecord, sync.syncedAt, Number.POSITIVE_INFINITY),
      requests,
      run,
      sync,
      syncLoad,
      syncLongest: longestStartingIn(syncRecord, sync.keyTimeStamp, sync.syncedAt),
    };
  } finally {
    await load.context.close();
  }
}

function fourthSmallest(readings: readonly number[]): number {
  const sorted = [...readings].sort((a, b) => a - b);
  return sorted[3] ?? Number.NaN;
}

// SLOW(>=2s): five cold loads of Conversation at 390 px with the processor slowed by 4, each
// measuring the room's Yjs sync apply on the first Enter and the block editor's build.
test("measure decision 544: Yjs sync apply and block editor build", async ({
  browser,
  loadSeeds,
}, testInfo) => {
  test.skip(
    process.env.ISSUE_PAGE_LOAD_LONGEST !== "1",
    "the longest-task probes run only where ISSUE_PAGE_LOAD_LONGEST=1, so CI load legs stay fast"
  );
  test.skip(
    testInfo.project.name !== "iphone-load",
    "decision 544 measures at a slowdown of 4, which only iphone-load sets"
  );
  refuseASharedRun();
  test.setTimeout(900_000);
  const rate = loadCpuRate(testInfo.project.name);
  const width = loadWidth(testInfo);
  const seeded = await loadSeeds.shape();
  const address = presetPath(seeded.key, "conversation");

  const runs: M544Run[] = [];
  for (let run = 1; run <= 5; run += 1) {
    const measured = await measure544Run(browser, rate, address, run);
    runs.push(measured);
    const { sync, build } = measured;
    console.log(
      `M544-SYNC run=${run} total=${shown(sync.syncedAt - sync.keyTimeStamp)} ` +
        `apply=${shown(sync.applyMs)} longest-task=${shown(measured.syncLongest?.duration ?? 0)}`
    );
    console.log(
      `M544-BUILD run=${run} import=${shown(build.importMs)} build=${shown(build.buildMs)} ` +
        `warm-build=${shown(build.warmBuildMs)} longest-task=${shown(measured.buildLongest?.duration ?? 0)} ` +
        `blockId-kept=${build.blockIdKept}`
    );
    if (measured.requests.length > 0) {
      console.log(`M544-REQUESTS run=${run} ${JSON.stringify(measured.requests)}`);
    }
  }

  const syncTotals = runs.map((run) => run.sync.syncedAt - run.sync.keyTimeStamp);
  const syncApplies = runs.map((run) => run.sync.applyMs);
  const buildTimes = runs.map((run) => run.build.buildMs);
  const warmTimes = runs.map((run) => run.build.warmBuildMs);
  const importTimes = runs.map((run) => run.build.importMs);
  console.log(
    `M544-SUMMARY sync-total fourth=${shown(fourthSmallest(syncTotals))} ` +
      `apply fourth=${shown(fourthSmallest(syncApplies))} ` +
      `build fourth=${shown(fourthSmallest(buildTimes))} ` +
      `warm-build fourth=${shown(fourthSmallest(warmTimes))} ` +
      `import fourth=${shown(fourthSmallest(importTimes))}`
  );

  const slowestSync = runs.reduce((a, b) =>
    (a.syncLongest?.duration ?? 0) >= (b.syncLongest?.duration ?? 0) ? a : b
  );
  const slowestBuild = runs.reduce((a, b) =>
    (a.buildLongest?.duration ?? 0) >= (b.buildLongest?.duration ?? 0) ? a : b
  );
  printLongest("m544-sync", width, slowestSync.run, slowestSync.syncLoad, slowestSync.syncLongest);
  printLongest(
    "m544-build",
    width,
    slowestBuild.run,
    slowestBuild.buildLoad,
    slowestBuild.buildLongest
  );
});

declare global {
  interface Window {
    __measure544?: {
      armSyncOnEnter(): Promise<M544Sync>;
      measureBlockEditor(): Promise<M544Build>;
    };
  }
}
