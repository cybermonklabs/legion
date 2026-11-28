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
 *
 * With `ISSUE_PAGE_LOAD_TRACE=1`, `the acts made after the load, profiled` also records Chrome's
 * own trace through the same session (`Tracing`, the timeline's categories) around each fold and
 * unfold, from just before the act's steps to its result, and prints each task that dispatched
 * the act's press (`printTrace`): what a CPU profile reads as `(program)` split by the engine's
 * own events, style, layout and paint.
 */

import { existsSync, readFileSync } from "node:fs";
import { createRequire } from "node:module";
import { basename } from "node:path";
import { setTimeout as delay } from "node:timers/promises";
import { fileURLToPath } from "node:url";
import type { Browser, CDPSession, Page } from "@playwright/test";

import { presetPath } from "../web/src/features/issue-page/issue-address";
import type { Preset } from "../web/src/features/issue-page/page-words";
import { listIssueAsks } from "./api";
import { BLOCK_EDITOR_READY_MARK, blockEditorMeasures, waitForMark } from "./block-editor";
import {
  AFTER_LOAD_ENTERS,
  actAddress,
  afterLoadOrder,
  answeredQuestion,
  boundWord,
  HEADER_SELECTORS,
  LOAD_ACTS,
  LOAD_ROW_SELECTOR,
  LOAD_VIEWS,
  type LoadAct,
  loadCpuRate,
  loadWidth,
  makeSteps,
  namedRow,
  outlineRows,
  READER,
  test,
  VIEW_ROW_SELECTOR,
} from "./issue-page-load";
import { withIssuePage } from "./issue-page-option";
import { owedUnless } from "./issue-page-t-tree";
import { typefaceAddress } from "./issue-page-typeface";
import {
  type ActReading,
  actReading,
  boxesMoved,
  firstDrawing,
  installInstruments,
  type LoadRecord,
  moveLine,
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
  // With no sample of the anchor the window is the whole profile, which an act's profile spans
  // from just before its measured step to its result: its functions then are every task's there.
  const from = clock === null ? Number.NEGATIVE_INFINITY : clock(task.startTime);
  const to = clock === null ? Number.POSITIVE_INFINITY : clock(task.startTime + task.duration);
  if (clock === null) {
    console.log(
      `LONGEST-ERROR ${label} the profile holds no sample of the anchor: the window read is the ` +
        "whole profile"
    );
  }
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
    __commitProbe?: {
      arm(): void;
      disarm(): readonly CommitEntry[];
    };
  }
}

/** One React commit the commit probe read (`commitProbe`), at the page's performance time. */
interface CommitEntry {
  readonly at: number;
  /** Each component that rendered in the commit, by its name, with how often. */
  readonly rendered: Readonly<Record<string, number>>;
  /** The `NodeRow` fibers the committed tree holds, whatever number the build adds to the name
   *  (`ROW_COMPONENT`), and how many of them rendered. */
  readonly rows: number;
  readonly rowsRendered: number;
  /** Of the rows that rendered and had rendered before, those whose props were all the same
   *  objects as at their last render, and each prop that changed by identity, with how often. */
  readonly rowsSameProps: number;
  readonly propsChanged: Readonly<Record<string, number>>;
}

/**
 * The init script of the commit probe (`ISSUE_PAGE_LOAD_COMMITS=1`), installed before the app
 * loads: React's devtools hook, which React 19's production build injects itself into and calls at
 * each commit (`onCommitFiberRoot`). While armed (`window.__commitProbe.arm`), each commit is
 * read: every component fiber that rendered in it - a fiber the render reached whose
 * `PerformedWork` flag (1) is set, or one it mounted - by its type's name, `memo` and `forwardRef`
 * unwrapped; and the `NodeRow` fibers, with which of their props changed by identity. The walk
 * goes into a fiber's children only where the render reached them: a parent whose child is still
 * its last render's child bailed out of that subtree, whose old flags say nothing of this commit.
 */
function commitProbe(): void {
  interface Fiber {
    readonly alternate: Fiber | null;
    readonly child: Fiber | null;
    readonly flags: number;
    readonly memoizedProps: Readonly<Record<string, unknown>> | null;
    readonly sibling: Fiber | null;
    readonly tag: number;
    readonly type: unknown;
  }
  // Function, class, forwardRef and simple memo components; a memo component's own fiber (14)
  // renders nothing itself, and its child fiber is counted.
  const components = new Set([0, 1, 11, 15]);
  const performedWork = 1;
  // The row's component, `NodeRow`, by its name as the build gives it: a build that keeps names
  // calls the function `NodeRow2`, a number added so that it shadows no `const NodeRow` it is
  // bound to (`memo(function NodeRow ...)`). `ROW_COMPONENT` below is the same rule; this script
  // runs in the page, and reads nothing of this file's.
  const rowComponent = /^NodeRow\d*$/;
  const nameOf = (type: unknown): string => {
    let at: unknown = type;
    for (let depth = 0; depth < 4 && typeof at === "object" && at !== null; depth += 1) {
      at = Reflect.get(at, "type") ?? Reflect.get(at, "render");
    }
    if (typeof at !== "function") return "(none)";
    const shown: unknown = Reflect.get(at, "displayName");
    return typeof shown === "string" ? shown : at.name || "(anonymous)";
  };
  const commits: CommitEntry[] = [];
  let armed = false;
  const read = (root: Fiber): CommitEntry => {
    const rendered: Record<string, number> = {};
    const propsChanged: Record<string, number> = {};
    let rows = 0;
    let rowsRendered = 0;
    let rowsSameProps = 0;
    const count = (into: Record<string, number>, key: string): void => {
      into[key] = (into[key] ?? 0) + 1;
    };
    const reached = (fiber: Fiber): void => {
      const before = fiber.alternate;
      const did = before === null || (fiber.flags & performedWork) === performedWork;
      if (components.has(fiber.tag) && did) {
        const name = nameOf(fiber.type);
        count(rendered, name);
        if (rowComponent.test(name)) {
          rowsRendered += 1;
          if (before !== null) {
            const now = fiber.memoizedProps ?? {};
            const then = before.memoizedProps ?? {};
            const changed = [...new Set([...Object.keys(now), ...Object.keys(then)])].filter(
              (key) => !Object.is(now[key], then[key])
            );
            if (changed.length === 0) rowsSameProps += 1;
            for (const key of changed) count(propsChanged, key);
          }
        }
      }
      if (before !== null && fiber.child === before.child) return;
      for (let child = fiber.child; child !== null; child = child.sibling) reached(child);
    };
    const every = (fiber: Fiber): void => {
      if (fiber.tag === 15 && rowComponent.test(nameOf(fiber.type))) rows += 1;
      for (let child = fiber.child; child !== null; child = child.sibling) every(child);
    };
    reached(root);
    every(root);
    return { at: performance.now(), propsChanged, rendered, rows, rowsRendered, rowsSameProps };
  };
  Reflect.set(window, "__REACT_DEVTOOLS_GLOBAL_HOOK__", {
    inject: () => 1,
    isDisabled: false,
    onCommitFiberRoot: (_renderer: number, root: { readonly current: Fiber }) => {
      if (armed) commits.push(read(root.current));
    },
    onCommitFiberUnmount: () => undefined,
    onPostCommitFiberRoot: () => undefined,
    renderers: new Map(),
    supportsFiber: true,
  });
  window.__commitProbe = {
    arm: () => {
      commits.length = 0;
      armed = true;
    },
    disarm: () => {
      armed = false;
      return [...commits];
    },
  };
}

/** The acts the commit probe reads, after the load (`ISSUE_PAGE_LOAD_COMMITS=1`). */
const COMMIT_ACTS: Readonly<Partial<Record<LoadAct["kind"], true>>> = { fold: true, unfold: true };

/** How many components `printCommits` prints for each commit, most renders first. */
const COMMIT_COMPONENTS = 30;

/** The row's component, `NodeRow`, by a function's name as the build gives it: `NodeRow`, or with
 *  the number a build that keeps names adds so that the function shadows no `const NodeRow`
 *  (`NodeRow2`). The commit probe's init script holds the same rule (`commitProbe`). */
const ROW_COMPONENT = /^NodeRow\d*$/;

/**
 * Prints each commit the commit probe read in an act's window, from its event to its result
 * (`COMMIT`, its time from the event): each component that rendered in it with how often
 * (`COMMIT-RENDERED`), and its rows - the `NodeRow` fibers it holds, how many rendered, how many of
 * those had every prop the same object as before, and each prop that changed (`COMMIT-ROWS`,
 * `COMMIT-ROW-PROP`). Then, from the CPU profile over the window, the time on the stack of
 * `NodeRow` and of the page's functions it calls (`COMMIT-NODEROW`, `COMMIT-NODEROW-CALLS`), and
 * the part of that time the rows whose props were all the same take, by their share of the rows
 * that rendered (`COMMIT-SAME-ROWS`): the profile tells no row's render from another's.
 */
function printCommits(
  label: string,
  width: number,
  commits: readonly CommitEntry[],
  made: ProfiledAct
): void {
  const { record } = made.load;
  const event = record.acts[made.index]?.event ?? null;
  if (event === null) return;
  const from = event.timeStamp;
  const to = from + (actReading(record, made.index).shown ?? 3_000);
  const line = `${width} ${label} 1`;
  let rendered = 0;
  let same = 0;
  for (const commit of commits) {
    if (commit.at < from || commit.at > to) continue;
    const at = shown(commit.at - from);
    const names = Object.entries(commit.rendered).sort((a, b) => b[1] - a[1]);
    const renders = names.reduce((total, [, times]) => total + times, 0);
    console.log(`COMMIT ${line} at=${at} components=${names.length} renders=${renders}`);
    for (const [name, times] of names.slice(0, COMMIT_COMPONENTS)) {
      console.log(`COMMIT-RENDERED ${line} at=${at} ${times} ${name}`);
    }
    console.log(
      `COMMIT-ROWS ${line} at=${at} rows=${commit.rows} rendered=${commit.rowsRendered} ` +
        `same-props=${commit.rowsSameProps}`
    );
    const props = Object.entries(commit.propsChanged).sort((a, b) => b[1] - a[1]);
    for (const [prop, times] of props) {
      console.log(`COMMIT-ROW-PROP ${line} at=${at} ${times} ${prop}`);
    }
    rendered += commit.rowsRendered;
    same += commit.rowsSameProps;
  }
  const samples = samplesOf(made.load.profile);
  const parents = parentsOf(made.load.profile);
  const clock = profileClock(made.load, samples, parents);
  if (clock === null) return;
  const nodes = new Map(made.load.profile.nodes.map((node) => [node.id, node]));
  const start = clock(from);
  const end = clock(to);
  let rowMs = 0;
  const calls = new Map<string, { ms: number; readonly frame: ProfileNode["callFrame"] }>();
  for (const sample of samples) {
    if (sample.at < start || sample.at >= end) continue;
    // The stack from the sample's own frame up: the frames below the first `NodeRow` met going
    // up are the functions it calls.
    const below: ProfileNode["callFrame"][] = [];
    let inRow = false;
    for (let at: number | undefined = sample.node.id; at !== undefined; at = parents.get(at)) {
      const frame = nodes.get(at)?.callFrame;
      if (frame === undefined || engineFrame(frame)) continue;
      if (ROW_COMPONENT.test(mapFrame(frame).name)) {
        inRow = true;
        break;
      }
      below.push(frame);
    }
    if (!inRow) continue;
    const ms = sample.lasts / 1000;
    rowMs += ms;
    for (const frame of new Map(below.map((one) => [keyOf(one), one])).values()) {
      const held = calls.get(keyOf(frame));
      if (held === undefined) calls.set(keyOf(frame), { frame, ms });
      else held.ms += ms;
    }
  }
  console.log(`COMMIT-NODEROW ${line} total=${shown(rowMs)}`);
  const own = [...calls.values()]
    .map((entry) => ({ ...entry, mapped: mapFrame(entry.frame) }))
    .filter(({ mapped }) => !mapped.place.startsWith("node_modules/"))
    .sort((a, b) => b.ms - a.ms)
    .slice(0, TASK_FUNCTIONS);
  for (const entry of own) {
    console.log(
      `COMMIT-NODEROW-CALLS ${line} ${shown(entry.ms)} ${entry.mapped.name} ${entry.mapped.place}`
    );
  }
  const share = rendered === 0 ? 0 : same / rendered;
  console.log(
    `COMMIT-SAME-ROWS ${line} rendered=${rendered} same-props=${same} ` +
      `share=${share.toFixed(2)} time=${shown(rowMs * share)}`
  );
}

// ---------------------------------------------------------------------------------------------
// The trace probe (`ISSUE_PAGE_LOAD_TRACE=1`): Chrome's own events of the tasks that dispatched a
// fold's or an unfold's press after the load, so the engine's time a CPU profile reads as
// `(program)` is split by what the engine did in it.
// ---------------------------------------------------------------------------------------------

/** The acts the trace probe records, after the load. */
const TRACE_ACTS: Readonly<Partial<Record<LoadAct["kind"], true>>> = { fold: true, unfold: true };

/** The trace's categories: the timeline's events (style, layout, paint, script, an input's
 *  dispatch) and each task of the thread (`RunTask`). */
const TRACE_CATEGORIES = [
  "devtools.timeline",
  "disabled-by-default-devtools.timeline",
  "toplevel",
  "v8.execute",
];

/** The input events whose dispatch makes a task one of the act's input tasks: a press as
 *  Playwright makes it, a move to the point and then the button down and up. */
const TRACE_INPUTS: Readonly<Record<string, true>> = {
  click: true,
  mousedown: true,
  mousemove: true,
  mouseup: true,
  pointerdown: true,
  pointermove: true,
  pointerup: true,
};

/** The engine's own events the probe prints of an input task, in the order a frame makes them. */
const TRACE_RENDERING = [
  "UpdateLayoutTree",
  "Layout",
  "PrePaint",
  "Paint",
  "Layerize",
  "UpdateLayer",
  "CompositeLayers",
];

/** The events whose time is the page's script: a style or layout inside one is one the script
 *  forced. */
const TRACE_SCRIPT: Readonly<Record<string, true>> = {
  EvaluateScript: true,
  FunctionCall: true,
  RunMicrotasks: true,
  "V8.Execute": true,
  "v8.callFunction": true,
};

/** One event of a trace as a span of its thread's time, in µs, with its arguments: a complete
 *  event (`X`), or a begin and its end (`B`, `E`) with both their arguments. */
interface TraceSpan {
  readonly args: Readonly<Record<string, unknown>>;
  readonly end: number;
  readonly name: string;
  readonly pid: number;
  readonly start: number;
  readonly tid: number;
}

/** Starts Chrome's trace of `TRACE_CATEGORIES` on `cdp`; answers how to end it, which answers the
 *  trace's events as the session reported them. */
async function startTrace(cdp: CDPSession): Promise<() => Promise<readonly unknown[]>> {
  const events: unknown[] = [];
  const collect = (batch: { readonly value: readonly unknown[] }): void => {
    events.push(...batch.value);
  };
  cdp.on("Tracing.dataCollected", collect);
  await cdp.send("Tracing.start", {
    traceConfig: { includedCategories: TRACE_CATEGORIES, recordMode: "recordAsMuchAsPossible" },
    transferMode: "ReportEvents",
  });
  return async () => {
    // Every batch of the trace is reported before `tracingComplete`, which ends it.
    const complete = Promise.withResolvers<void>();
    cdp.once("Tracing.tracingComplete", () => complete.resolve());
    await cdp.send("Tracing.end");
    await complete.promise;
    cdp.off("Tracing.dataCollected", collect);
    return events;
  };
}

/** A record's object, or none. */
function objectOf(value: unknown): Readonly<Record<string, unknown>> {
  return typeof value === "object" && value !== null ? (value as Record<string, unknown>) : {};
}

/** The spans of a trace's events, by the trace event format: each complete event, and each begin
 *  matched with the next end of its name on its thread. Other phases are left out. */
function traceSpans(events: readonly unknown[]): TraceSpan[] {
  const spans: TraceSpan[] = [];
  const open = new Map<string, { readonly args: Record<string, unknown>; readonly ts: number }[]>();
  for (const raw of events) {
    const event = objectOf(raw);
    const { name, ph, pid, tid, ts } = event;
    if (typeof name !== "string" || typeof ts !== "number") continue;
    if (typeof pid !== "number" || typeof tid !== "number") continue;
    const args = objectOf(event.args);
    if (ph === "X" && typeof event.dur === "number") {
      spans.push({ args, end: ts + event.dur, name, pid, start: ts, tid });
      continue;
    }
    const key = `${pid} ${tid} ${name}`;
    if (ph === "B") {
      open.set(key, [...(open.get(key) ?? []), { args: { ...args }, ts }]);
      continue;
    }
    if (ph !== "E") continue;
    const begun = open.get(key)?.pop();
    if (begun !== undefined) {
      spans.push({ args: { ...begun.args, ...args }, end: ts, name, pid, start: begun.ts, tid });
    }
  }
  return spans.sort((a, b) => a.start - b.start);
}

/** How long the spans cover between them, each µs counted once however many cover it. */
function coveredUs(spans: readonly TraceSpan[]): number {
  let total = 0;
  let reached = Number.NEGATIVE_INFINITY;
  for (const span of [...spans].sort((a, b) => a.start - b.start)) {
    const from = Math.max(span.start, reached);
    if (span.end > from) total += span.end - from;
    reached = Math.max(reached, span.end);
  }
  return total;
}

/** A span's µs as a line prints ms. */
function traceMs(us: number): string {
  return shown(us / 1000);
}

/** What a line prints of a style or layout event's own counts: `elementCount` of a style
 *  recalculation, and a layout's `beginData` counts, each where the event holds it. */
function traceCounts(span: TraceSpan): string {
  const begin = objectOf(span.args.beginData);
  const counts: [string, unknown][] = [
    ["elementCount", span.args.elementCount ?? objectOf(span.args.data).elementCount],
    ["dirtyObjects", begin.dirtyObjects],
    ["totalObjects", begin.totalObjects],
    ["partialLayout", begin.partialLayout],
  ];
  return counts
    .filter(([, value]) => value !== undefined)
    .map(([name, value]) => ` ${name}=${String(value)}`)
    .join("");
}

/**
 * Prints each task of the trace's main thread that dispatched one of the act's press events
 * (`TRACE_INPUTS`): `TRACE-TASK` with its time from the first such dispatch, its duration, the
 * time its script covers (`js`) and the events it dispatched; `TRACE-EVENT` for each of the
 * engine's own events in it (`TRACE_RENDERING`), with its time from the task's start, its
 * duration, whether a script forced it (`in-js`), and the counts a style recalculation and a
 * layout give; and `TRACE-SUM` for each kind of them, with how many came, their total, and the
 * part of it that no script forced.
 */
function printTrace(label: string, width: number, events: readonly unknown[]): void {
  const line = `${width} ${label} 1`;
  const spans = traceSpans(events);
  const typeOf = (span: TraceSpan): unknown => objectOf(span.args.data).type;
  const inputs = spans.filter(
    (span) => span.name === "EventDispatch" && TRACE_INPUTS[String(typeOf(span))] === true
  );
  const first = inputs[0];
  if (first === undefined) {
    console.log(`TRACE ${line} no press event was dispatched in the trace (${spans.length} spans)`);
    return;
  }
  const thread = spans.filter((span) => span.pid === first.pid && span.tid === first.tid);
  const within = (inner: TraceSpan, outer: TraceSpan) =>
    inner.start >= outer.start && inner.end <= outer.end;
  const runs = thread.filter((span) => span.name === "RunTask");
  const tasks = runs.filter(
    (task) =>
      !runs.some((outer) => outer !== task && within(task, outer)) &&
      inputs.some(
        (input) => input.pid === task.pid && input.tid === task.tid && within(input, task)
      )
  );
  for (const [at, task] of tasks.entries()) {
    const inside = thread.filter((span) => within(span, task));
    const script = inside.filter((span) => TRACE_SCRIPT[span.name] === true);
    const dispatched = [
      ...new Set(inputs.filter((input) => within(input, task)).map((input) => typeOf(input))),
    ];
    console.log(
      `TRACE-TASK ${line} task=${at + 1} start=${traceMs(task.start - first.start)} ` +
        `duration=${traceMs(task.end - task.start)} js=${traceMs(coveredUs(script))} ` +
        `inputs=${dispatched.join(",")}`
    );
    for (const name of TRACE_RENDERING) {
      const own = inside.filter((span) => span.name === name);
      if (own.length === 0) continue;
      const forced = (span: TraceSpan) => script.some((outer) => within(span, outer));
      for (const span of own) {
        console.log(
          `TRACE-EVENT ${line} task=${at + 1} ${name} at=${traceMs(span.start - task.start)} ` +
            `dur=${traceMs(span.end - span.start)} in-js=${forced(span) ? "yes" : "no"}` +
            traceCounts(span)
        );
      }
      console.log(
        `TRACE-SUM ${line} task=${at + 1} ${name} count=${own.length} ` +
          `total=${traceMs(coveredUs(own))} ` +
          `native=${traceMs(coveredUs(own.filter((span) => !forced(span))))}`
      );
    }
  }
}

// ---------------------------------------------------------------------------------------------
// The acts, profiled: each act of the width made as Row 4 makes it, at its first moment during a
// cold load, and as Row 5 makes it, once in one load after its end; and the named rows' loads,
// with every layout shift's sources and every box that changed its size.
// ---------------------------------------------------------------------------------------------

/** How many cold loads each act made during the load is profiled over. */
const ACT_RUNS = 2;

/** One act, profiled. */
interface ProfiledAct {
  /** The index its measured step was armed under. */
  readonly index: number;
  readonly load: ProfiledLoad;
  /** Every request of the load, in its page's performance time. */
  readonly spans: readonly RequestSpan[];
}

/** `act` made at its first moment in a cold load of its address, as Row 4 makes it, with the
 *  profiler running from before the navigation to the load's end. */
async function profiledAct(
  browser: Browser,
  rate: number,
  address: string,
  act: LoadAct
): Promise<ProfiledAct> {
  const load = await openLoad(browser, rate, address, "none");
  try {
    await load.cdp.send("Profiler.start");
    await load.goto();
    const made = await makeSteps(load.page, 0, act.steps, act.when);
    await waitForLoadEnd(load.page, load.requests);
    const record = await recordOf(load.page);
    const anchoredAt = await anchor(load.page);
    const { profile } = await load.cdp.send("Profiler.stop");
    return {
      index: made.last,
      load: { anchoredAt, profile, record },
      spans: load.requests.spans(record.timeOrigin),
    };
  } finally {
    await load.context.close();
  }
}

/** Prints one act's reading as Rows 4 and 5 read it (`ACT`), the frames that showed its loading
 *  and its result, each request sent from a second before its event to its result (`ACT-REQUEST`),
 *  each long task in that window (`ACT-TASK`), each time from the event, and the longest task
 *  there as `printLongest` prints it, under the view `act-<word>`. */
function printAct(label: string, width: number, run: number, made: ProfiledAct): void {
  const { record } = made.load;
  const reading = actReading(record, made.index);
  const armed = record.acts[made.index];
  const event = armed?.event ?? null;
  console.log(
    `ACT ${width} ${label} ${run} result=${part(reading.shown)} loading=${part(reading.loading)} ` +
      `read-sent=${part(reading.read?.sent)} read-drawn=${part(reading.read?.drawn)} ` +
      `read=${reading.read?.url ?? "none"} landed=${event?.target ?? "none"}`
  );
  if (event === null) return;
  const from = event.timeStamp;
  const to = from + (reading.shown ?? 3_000);
  for (const [what, index] of [
    ["loading", armed?.loading ?? null],
    ["result", armed?.result ?? null],
  ] as const) {
    const frame = index === null ? undefined : record.frames[index];
    if (frame === undefined) continue;
    console.log(
      `ACT-FRAME ${width} ${label} ${run} ${what} at=${shown(frame.at - from)} ` +
        `shown=${frame.shown === null ? "none" : shown(frame.shown - from)}`
    );
  }
  for (const span of made.spans) {
    if (span.start < from - 1_000 || span.start >= to) continue;
    const path = URL.canParse(span.url)
      ? new URL(span.url).pathname + new URL(span.url).search
      : span.url;
    console.log(
      `ACT-REQUEST ${width} ${label} ${run} start=${shown(span.start - from)} ` +
        `end=${span.end === null ? "open" : shown(span.end - from)} ${path.slice(0, 160)}`
    );
  }
  for (const task of record.longtask) {
    if (task.startTime + task.duration < from - 300 || task.startTime >= to) continue;
    console.log(
      `ACT-TASK ${width} ${label} ${run} start=${shown(task.startTime - from)} ` +
        `duration=${shown(task.duration)}`
    );
  }
  printLongest(`act-${label}`, width, run, made.load, longestIn(record, from, to));
  if (label.startsWith("during-")) {
    const peak = longestTask(record);
    if (peak !== null && peak.duration > 200) {
      printLongest(`load-${label}`, width, run, made.load, peak);
    }
  }
  if (process.env.ISSUE_PAGE_LOAD_ACT_TASKS === "1") printActParts(label, width, run, made, to);
}

/** How many of the page's own functions `printActParts` prints for each task. */
const TASK_FUNCTIONS = 24;

/** The shortest stretch of work `printActParts` prints (`BUSY`), in ms. */
const BUSY_MIN_MS = 4;

/**
 * With `ISSUE_PAGE_LOAD_ACT_TASKS=1`, what each part of an act's window ran, so a reading spread
 * over several tasks shows each of them: each tree or decisions read from a second before the
 * event to the result by its resource timing (`ACT-RT`, start and `responseEnd` from the event);
 * each frame from the event to the result (`FRAME`, its animation callback's time and the time
 * it was shown); for each long task there (`TASK`) the page's own functions with the most time on
 * the stack in it, their callees' included and no module of `node_modules` (`TASK-TOTAL`); and
 * each stretch of the main thread's work there of `BUSY_MIN_MS` or more, long task or not, read
 * off the profile as a run of samples with no idle one among them (`BUSY`, with the engine's own
 * time in it), with the same functions (`BUSY-TOTAL`). Each time is from the event.
 */
function printActParts(
  label: string,
  width: number,
  run: number,
  made: ProfiledAct,
  to: number
): void {
  const { record } = made.load;
  const event = record.acts[made.index]?.event ?? null;
  if (event === null) return;
  const from = event.timeStamp;
  const line = `${width} ${label} ${run}`;
  for (const entry of record.resource) {
    if (!/\/api\/v1\/issues\/[^/]+\/(tree|decisions)/.test(entry.name)) continue;
    if (entry.responseEnd < from - 1_000 || entry.startTime >= to) continue;
    const path = new URL(entry.name).pathname + new URL(entry.name).search;
    console.log(
      `ACT-RT ${line} start=${shown(entry.startTime - from)} ` +
        `end=${shown(entry.responseEnd - from)} ${path.slice(0, 120)}`
    );
  }
  for (const frame of record.frames) {
    if (frame.at < from - 20 || frame.at > to) continue;
    console.log(
      `FRAME ${line} at=${shown(frame.at - from)} ` +
        `shown=${frame.shown === null ? "none" : shown(frame.shown - from)}`
    );
  }
  const samples = samplesOf(made.load.profile);
  const parents = parentsOf(made.load.profile);
  const clock = profileClock(made.load, samples, parents);
  if (clock === null) return;
  const nodes = new Map(made.load.profile.nodes.map((node) => [node.id, node]));
  // The page's functions on the stack in the profile's window from `start` to `end` (µs), most
  // time first, each printed under `head`.
  const printTotals = (head: string, start: number, end: number): void => {
    const totals = new Map<string, { ms: number; readonly frame: ProfileNode["callFrame"] }>();
    for (const sample of samples) {
      if (sample.at < start || sample.at >= end) continue;
      const seen = new Set<string>();
      for (let at: number | undefined = sample.node.id; at !== undefined; at = parents.get(at)) {
        const frame = nodes.get(at)?.callFrame;
        if (frame === undefined || engineFrame(frame)) continue;
        const key = keyOf(frame);
        if (seen.has(key)) continue;
        seen.add(key);
        const held = totals.get(key);
        if (held === undefined) totals.set(key, { frame, ms: sample.lasts / 1000 });
        else held.ms += sample.lasts / 1000;
      }
    }
    const own = [...totals.values()]
      .map((entry) => ({ ...entry, mapped: mapFrame(entry.frame) }))
      .filter(({ mapped }) => !mapped.place.startsWith("node_modules/"))
      .sort((a, b) => b.ms - a.ms)
      .slice(0, TASK_FUNCTIONS);
    for (const entry of own) {
      console.log(`${head} ${shown(entry.ms)} ${entry.mapped.name} ${entry.mapped.place}`);
    }
  };
  for (const task of record.longtask) {
    if (task.startTime + task.duration < from - 50 || task.startTime >= to) continue;
    const at = shown(task.startTime - from);
    console.log(`TASK ${line} start=${at} duration=${shown(task.duration)}`);
    printTotals(
      `TASK-TOTAL ${line} start=${at}`,
      clock(task.startTime),
      clock(task.startTime + task.duration)
    );
  }
  // The event's time on the profile's clock, which each stretch's start is read from.
  const origin = clock(from);
  const windowStart = clock(from - 50);
  const windowEnd = clock(to);
  let stretch: { start: number; end: number; gc: number; program: number } | undefined;
  const endStretch = (): void => {
    if (stretch === undefined) return;
    const ms = (stretch.end - stretch.start) / 1000;
    if (ms >= BUSY_MIN_MS) {
      const at = shown((stretch.start - origin) / 1000);
      console.log(
        `BUSY ${line} start=${at} duration=${shown(ms)} program=${shown(stretch.program)} ` +
          `gc=${shown(stretch.gc)}`
      );
      printTotals(`BUSY-TOTAL ${line} start=${at}`, stretch.start, stretch.end);
    }
    stretch = undefined;
  };
  for (const sample of samples) {
    if (sample.at < windowStart || sample.at >= windowEnd) continue;
    const name = sample.node.callFrame.functionName;
    if (name === ENGINE.idle) {
      endStretch();
      continue;
    }
    stretch ??= { end: sample.at, gc: 0, program: 0, start: sample.at };
    stretch.end = sample.at + sample.lasts;
    if (name === ENGINE.gc) stretch.gc += sample.lasts / 1000;
    if (name === ENGINE.program) stretch.program += sample.lasts / 1000;
  }
  endStretch();
}

// SLOW(>=2s): two cold loads for each act of the width, each act made at its first moment and the
// load profiled to its end.
test("the acts made during the load, profiled", async ({ browser, loadSeeds }, testInfo) => {
  test.skip(
    process.env.ISSUE_PAGE_LOAD_LONGEST !== "1",
    "the longest-task probes run only where ISSUE_PAGE_LOAD_LONGEST=1, so CI load legs stay fast"
  );
  test.skip(
    !LOAD_PROJECTS.includes(testInfo.project.name),
    "the probe profiles the clock rows' loads, which only chromium-load and iphone-load make"
  );
  refuseASharedRun();
  test.setTimeout(PROBE_MS * 4);
  const rate = loadCpuRate(testInfo.project.name);
  const width = loadWidth(testInfo);
  const seeded = await loadSeeds.shape();
  const only = process.env.ISSUE_PAGE_LOAD_ACTS;
  for (const act of LOAD_ACTS[width]) {
    const label = `during-${boundWord(act.name)}`;
    if (only !== undefined && !new RegExp(only).test(label)) continue;
    for (let run = 1; run <= ACT_RUNS; run += 1) {
      printAct(
        label,
        width,
        run,
        await profiledAct(browser, rate, actAddress(seeded.key, act), act)
      );
    }
  }
});

// SLOW(>=2s): one cold load waited out, then each act of the width made once in it as Row 5 makes
// it, each profiled from just before its measured step.
test("the acts made after the load, profiled", async ({ browser, loadSeeds }, testInfo) => {
  test.skip(
    process.env.ISSUE_PAGE_LOAD_LONGEST !== "1",
    "the longest-task probes run only where ISSUE_PAGE_LOAD_LONGEST=1, so CI load legs stay fast"
  );
  test.skip(
    !LOAD_PROJECTS.includes(testInfo.project.name),
    "the probe profiles the clock rows' loads, which only chromium-load and iphone-load make"
  );
  refuseASharedRun();
  test.setTimeout(PROBE_MS * 2);
  const rate = loadCpuRate(testInfo.project.name);
  const width = loadWidth(testInfo);
  const seeded = await loadSeeds.shape();
  const load = await openLoad(browser, rate, presetPath(seeded.key, "conversation"), "none");
  const readCommits = process.env.ISSUE_PAGE_LOAD_COMMITS === "1";
  const readTrace = process.env.ISSUE_PAGE_LOAD_TRACE === "1";
  try {
    if (readCommits) await load.page.addInitScript(commitProbe);
    await load.goto();
    await waitForLoadEnd(load.page, load.requests);
    let next = 0;
    for (const act of afterLoadOrder(LOAD_ACTS[width])) {
      const label = `after-${boundWord(act.name)}`;
      await waitForLoadEnd(load.page, load.requests);
      if (act.paging !== undefined) {
        // The load has just gone quiet again, so the rows it drew already say whether the
        // paging control is there: one read of that settled page is its answer.
        const pages = await load.page.evaluate(
          (selector) => document.querySelector(selector) !== null,
          act.paging
        );
        if (!pages) {
          console.log(`ACT ${width} ${label} 1 not made: no paging control drawn`);
          continue;
        }
      }
      const commits = readCommits && COMMIT_ACTS[act.kind] === true;
      if (commits) await load.page.evaluate(() => window.__commitProbe?.arm());
      await load.cdp.send("Profiler.start");
      const endTrace =
        readTrace && TRACE_ACTS[act.kind] === true ? await startTrace(load.cdp) : undefined;
      const made = await makeSteps(load.page, next, act.steps, "now");
      const traced = await endTrace?.();
      const anchoredAt = await anchor(load.page);
      const { profile } = await load.cdp.send("Profiler.stop");
      const record = await recordOf(load.page);
      const profiled = {
        index: made.last,
        load: { anchoredAt, profile, record },
        spans: load.requests.spans(record.timeOrigin),
      };
      printAct(label, width, 1, profiled);
      if (traced !== undefined) printTrace(label, width, traced);
      if (commits) {
        // The act has shown its result and the probe was armed for it alone, so this one read
        // holds every commit of the act's window; it prints them and decides nothing.
        const held = await load.page.evaluate(() => window.__commitProbe?.disarm() ?? []);
        printCommits(label, width, held, profiled);
      }
      next = (await makeSteps(load.page, made.next, act.after, "now")).next;
    }
  } finally {
    await load.context.close();
  }
});

/** The init script of the shift probe: each layout shift with its sources, each box of a row or
 *  of the page's frame that changed its size after its first report (a `ResizeObserver`), and
 *  each scroll of the window, kept on `window.__shiftProbe`. */
function shiftProbe(): void {
  const lines: string[] = [];
  const describe = (node: Node | null | undefined): string => {
    const element = node instanceof Element ? node : (node?.parentElement ?? null);
    if (element === null) return "none";
    const marks = [
      "data-row-key",
      "data-node-id",
      "data-node-type",
      "data-testid",
      "data-child-entry",
      "role",
      "aria-label",
      "data-spec-because",
      "data-spec-checks",
      "data-spec-cell",
      "data-node-trail",
    ]
      .map((name) => {
        // The line names the element as it stands when its shift or resize is reported, which is
        // the instant the probe logs.
        const value = element.getAttribute(name);
        return value === null ? "" : `${name}=${value.slice(0, 70)}`;
      })
      .filter((mark) => mark !== "")
      .join(" ");
    const row = element.parentElement?.closest("[data-node-row], [data-child-entry]");
    // The row's key as it stands when the shift is reported, which is the instant the probe
    // logs, so one read of it is the answer.
    const rowKey = row?.getAttribute("data-row-key") ?? row?.getAttribute("data-child-entry");
    const inRow = row === null || row === undefined ? "" : ` in-row=${rowKey ?? "?"}`;
    const words = (element.textContent ?? "").replace(/\s+/g, " ").slice(0, 40);
    const kind = `${element.tagName.toLowerCase()}.${String(element.className).split(" ").slice(0, 2).join(".")}`;
    return `${kind} ${marks} "${words}"${inRow}`;
  };
  const rect = (box: DOMRectReadOnly): string =>
    `${box.width.toFixed(0)}x${box.height.toFixed(0)}@${box.x.toFixed(0)},${box.y.toFixed(0)}`;
  new PerformanceObserver((list) => {
    for (const entry of list.getEntries()) {
      const shift = entry as PerformanceEntry & {
        readonly hadRecentInput: boolean;
        readonly sources?: readonly {
          readonly currentRect: DOMRectReadOnly;
          readonly node: Node | null;
          readonly previousRect: DOMRectReadOnly;
        }[];
        readonly value: number;
      };
      lines.push(
        `shift t=${shift.startTime.toFixed(0)} value=${shift.value.toFixed(4)} ` +
          `recent=${shift.hadRecentInput} scrollY=${window.scrollY}`
      );
      for (const source of shift.sources ?? []) {
        lines.push(
          `  source ${describe(source.node)} ${rect(source.previousRect)} -> ${rect(source.currentRect)}`
        );
      }
    }
  }).observe({ buffered: true, type: "layout-shift" });
  const sizes = new WeakMap<Element, string>();
  // Each row's parts as last seen - every child of its content and of its words, by tag, data
  // attributes and height - so a row that grew names the part that came, left or grew.
  const partsSeen = new WeakMap<Element, string>();
  const partsOf = (row: Element): string =>
    [
      ...row.querySelectorAll(
        "[data-node-content] > *, [data-node-words] > *, [data-spec-statement] > div > *"
      ),
    ]
      .map((part) => {
        const data = [...part.attributes]
          .filter((attribute) => attribute.name.startsWith("data-"))
          .map((attribute) => attribute.name.slice(5))
          .join(",");
        return `${part.tagName.toLowerCase()}[${data}]${part.getBoundingClientRect().height.toFixed(0)}`;
      })
      .join(" ");
  const resized = new ResizeObserver((entries) => {
    for (const entry of entries) {
      const size = `${entry.contentRect.width.toFixed(0)}x${entry.contentRect.height.toFixed(0)}`;
      const before = sizes.get(entry.target);
      sizes.set(entry.target, size);
      // A child's entry is one run of inline words: its words before and after tell what changed.
      const parts = entry.target.hasAttribute("data-node-row")
        ? partsOf(entry.target)
        : entry.target.hasAttribute("data-child-entry")
          ? `"${(entry.target.textContent ?? "").replace(/\s+/g, " ").slice(0, 200)}"`
          : "";
      const partsBefore = partsSeen.get(entry.target);
      if (parts !== "") partsSeen.set(entry.target, parts);
      if (before !== undefined && before !== size) {
        const top = entry.target.getBoundingClientRect().top.toFixed(0);
        lines.push(
          `resize t=${performance.now().toFixed(0)} ${before} -> ${size} top=${top} ` +
            `scrollY=${window.scrollY} ${describe(entry.target)}`
        );
        if (parts !== "" && partsBefore !== parts) {
          lines.push(`  parts ${partsBefore ?? "none"} => ${parts}`);
        }
        if (entry.target.hasAttribute("data-spec-statement")) {
          // Identify the growing part of the measured statement, not just the nearby read.
          const parts = entry.target.querySelectorAll(
            "[data-node-words], [data-node-trail], [data-spec-because], [data-spec-checks]"
          );
          for (const part of parts) {
            lines.push(`  part ${describe(part)} ${rect(part.getBoundingClientRect())}`);
          }
        }
      }
    }
  });
  const watched = [
    "[data-node-row]",
    "[data-node-body]",
    "[data-node-counts]",
    "[data-child-entry]",
    "[data-identity-block]",
    "[data-filter-row]",
    "[data-issue-body]",
    "[data-spec-statement]",
    "[data-spec-cell]",
    "[data-spec-because]",
    "[data-spec-checks]",
    "[data-spec-statement] [data-node-words]",
    "[data-spec-statement] [data-node-trail]",
    '[role="tree"]',
    // Every part of the page's column and header that stands above the rows: a part that grows or
    // shrinks there moves every row under it.
    '[data-testid="issue-column"] *:not([data-node-row] *):not([data-node-row])',
    "[data-identity-block] > *",
  ].join(", ");
  const seen = new WeakSet<Element>();
  const watch = (): void => {
    for (const element of document.querySelectorAll(watched)) {
      if (seen.has(element)) continue;
      seen.add(element);
      resized.observe(element);
    }
  };
  new MutationObserver(watch).observe(document, { childList: true, subtree: true });
  let scrolled = window.scrollY;
  addEventListener(
    "scroll",
    () => {
      if (window.scrollY === scrolled) return;
      lines.push(`scroll t=${performance.now().toFixed(0)} ${scrolled} -> ${window.scrollY}`);
      scrolled = window.scrollY;
    },
    { capture: true, passive: true }
  );
  Reflect.set(window, "__shiftProbe", lines);
}

/** One cold load of `address`, every box recorded, with the shift probe's init script; prints
 *  what the probe kept up to the load's end (`SHIFT`) and every box the recorder saw change
 *  (`SHIFT-MOVE`). */
async function shiftLoad(
  browser: Browser,
  rate: number,
  label: string,
  run: number,
  address: string,
  heldTypeface?: string
): Promise<void> {
  const load = await openLoad(browser, rate, address, "all");
  try {
    await load.page.addInitScript(shiftProbe);
    if (heldTypeface !== undefined) {
      await load.page.route(heldTypeface, async (route) => {
        const answer = await route.fetch();
        // Row 3 answers the typeface's file 3,000 ms late, and this load holds it as long, so the
        // shifts it logs as the face lands are that row's.
        await delay(3_000);
        await route.fulfill({ response: answer });
      });
    }
    await load.goto();
    const end = await waitForLoadEnd(load.page, load.requests);
    const record = await recordOf(load.page);
    // The load has ended, so the probe has written every line it logs: one read of them prints
    // the whole log.
    const lines = await load.page.evaluate(
      () => (Reflect.get(window, "__shiftProbe") as string[] | undefined) ?? []
    );
    console.log(
      `SHIFT ${label} ${run} end=${shown(end)} cls=${record.cls.at(-1)?.value ?? "none"}`
    );
    for (const line of lines.slice(0, 400)) console.log(`SHIFT ${label} ${run} ${line}`);
    for (const move of boxesMoved(record, end)) {
      console.log(`SHIFT-MOVE ${label} ${run} ${moveLine(move).slice(0, 400)}`);
    }
  } finally {
    await load.context.close();
  }
}

// SLOW(>=2s): a load to find Row 2's last row, then two cold loads of each of Row 2's first two
// addresses and of Spec's and Conversation's own.
test("the layout shifts of the named rows' loads", async ({ browser, loadSeeds }, testInfo) => {
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
  const seeded = await loadSeeds.shape();
  const conversation = presetPath(seeded.key, "conversation");
  const finder = await openLoad(browser, rate, conversation, "none");
  let lastRow: string;
  let font: string | undefined;
  try {
    await finder.goto();
    await waitForLoadEnd(finder.page, finder.requests);
    lastRow = namedRow(seeded.key, await outlineRows(finder.page)).address;
    font = await typefaceAddress(finder.page);
  } finally {
    await finder.context.close();
  }
  const question = answeredQuestion(seeded.key, await listIssueAsks(seeded.key));
  const addresses: (readonly [string, string])[] = [
    ["last-row", lastRow],
    ...(question === null ? [] : [["question", question.address] as const]),
    ["spec", presetPath(seeded.key, "spec")],
    ["conversation", conversation],
    ["typeface-held", conversation],
  ];
  const only = process.env.ISSUE_PAGE_SHIFT_LOADS;
  for (const [label, address] of addresses) {
    if (only !== undefined && !new RegExp(only).test(label)) continue;
    console.log(`SHIFT ${label} address=${address}`);
    for (let run = 1; run <= ACT_RUNS; run += 1) {
      await shiftLoad(
        browser,
        rate,
        label,
        run,
        address,
        label === "typeface-held" ? font : undefined
      );
    }
  }
});

// SLOW(>=2s): cold loads followed through the block editor's ready mark.
test("the longest task between load end and block editor readiness", async ({
  browser,
  loadSeeds,
}, testInfo) => {
  test.skip(process.env.ISSUE_PAGE_LOAD_LONGEST !== "1");
  refuseASharedRun();
  test.setTimeout(PROBE_MS);
  const width = loadWidth(testInfo);
  const rate = loadCpuRate(testInfo.project.name);
  const seeded = await loadSeeds.shape();
  const address = presetPath(seeded.key, "conversation");
  for (let run = 1; run <= RUNS; run += 1) {
    const load = await openLoad(browser, rate, address, "all");
    try {
      await load.cdp.send("Profiler.start");
      await load.goto();
      const end = await waitForLoadEnd(load.page, load.requests);
      const ready = await waitForMark(load.page, BLOCK_EDITOR_READY_MARK);
      const measures = await blockEditorMeasures(load.page, end, ready);
      const anchoredAt = await anchor(load.page);
      const { profile } = await load.cdp.send("Profiler.stop");
      const record = await recordOf(load.page);
      console.log(`READY ${width} ${run} end=${end} ready=${ready} ${JSON.stringify(measures)}`);
      printLongest(
        "editor-ready",
        width,
        run,
        { anchoredAt, profile, record },
        longestIn(record, end, ready)
      );
    } finally {
      await load.context.close();
    }
  }
});
