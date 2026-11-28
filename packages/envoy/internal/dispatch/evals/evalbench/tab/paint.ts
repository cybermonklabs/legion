// The Evals tab's first paint in a browser that has loaded the app's JavaScript, against a server
// just restarted (dispatch://INFRA-2918, T27, checks k4 and k19 of the Verification document's
// v23, measured as INFRA-979's ruling on the first screen and the machine says,
// dispatch://INFRA-2918/comment/75af800e-9db9-47d9-9c4d-427b0cff36c0, with the restart of the
// ruling of ledger item 62): run by evalbench/run.sh's `tab` command, once per case. The script
// first measures the host's Lighthouse BenchmarkIndex in its own Chromium (lib.ts's
// benchmarkIndex), which sets how each time of the case is judged (lib.ts's header and speedOf):
// above 1500 every page of the case runs with its CPU throttled at index ÷ 1500 and each time is
// judged as measured; at or below 1500 the pages run unthrottled, the timed navigation is traced
// (lib.ts's LIGHT_TRACE_CATEGORIES in a plain run, the split's trace in a split run), and each time
// is judged as its waits plus its main-thread time × index ÷ 1500 (lib.ts's judge). A signed-in
// reader's Chromium context, every page 1440×900 at device scale 1 and never scrolled, then opens
// the tab once on the server the script has just started, which loads the app's JavaScript into
// the context's cache: that navigation's first paint and its time to every section drawn (the
// cold-browser times), the bundle's size and its load time are recorded. The script then restarts
// the server (the handshake below), and a new page of the same context, its JavaScript cached, is
// timed from its navigation to its first paint, to every section drawn and, in the author case, to
// the narrowed cards drawn. The first paint ends at the first screen (lib.ts's screenPaint): once
// each section with any part in the viewport has drawn its numbers and charts (the app's mark
// `dispatch:evals-section:<id>`, with the CI section's job lists drawn) and each other section
// shows its header and its box (the mark `dispatch:evals-section-box:<id>`, or the drawing's where
// the app sets none), each time the browser presented it (lib.ts's instrument); which sections lie
// in the first screen is read once every section is drawn, when no box moves any more. Each
// section's box is held to one size from when it is first shown to when its section is drawn
// (lib.ts's boxChange, over the sizes the page's ResizeObserver reported). The page's live frames
// give the first payload, the four evals snapshots, the CI section's two items snapshots and the
// header's legion-read snapshot, compressed; and pg_stat_statements, reset before the navigation
// and read once every section is drawn, gives the paint's database time, less the statements a
// quiet window before it shows the server's background loops run (measure paint's catalogue). The
// page's boot, from its navigation to its first subscribe, is one `RESULT tab paint boot` line a
// case, read from the browser's own navigation and resource timings once the paint is done, which
// cost the paint nothing: when the document's response ended, when the document was interactive
// and its DOMContentLoaded handlers had run, when each script the page loaded ended loading, the
// route's chunk (ProjectPage's) among them, and each API request it made before its first frame
// with when it began and ended.
//
// Each case's `RESULT tab paint machine` line, and every line that states a judged time, carries
// the index, the throttling rate (index ÷ 1500, above 1500) or the scale (index ÷ 1500, at or
// below 1500), each in full, the node (--node) and its node.kubernetes.io/instance-type
// (--instance-type, `unread` where the run read none: its reader records the type from outside).
// Each moment is stated as `<moment>_raw_ms`, `<moment>_judged_ms` and, unthrottled, the two parts
// it is judged from, `<moment>_waits_ms` and `<moment>_main_ms`, each rounded to 0.1 ms.
//
// A split run (`--split 1`, run.sh's EVALBENCH_SPLIT=1) explains the numbers a plain run judges,
// and runs instruments that cost the page and the server time of their own (lib.ts's instrument,
// the server's debug lines): its numbers compare one part with another, never with a bound. Its
// boot line adds when the page made its live socket and when the socket opened, when it sent its
// first subscribe and its first evals view's, and how long the page's long tasks ran before that
// subscribe. Its `RESULT tab paint split` line (item 1 of the architect's ruling on T33's misses,
// dispatch://INFRA-2918/comment/0d0aa027-83af-4a1b-9042-69f5d161ec32) gives, for each view of the
// first payload (`evals.<kind>`, `items.<sort>`, `legion-read`), each time from the navigation,
// when the page sent its subscribe, when the server's recompute of it began and ended and how long
// its steps took (the change position, the read, within it an evals view's snapshot of reads,
// their fold and the delivery freshness, and the encode of the body), how long the socket took to
// encode its snapshot frame and to compress and write it and when the write ended, when the frame
// arrived at the page's handler and when its parse ended, how long the page's long tasks ran
// between the frame's write and that arrival (`busy_before_arrival_ms`), and its size; for each
// section and CI job list, when its drawing committed and when the browser presented it, and for
// each section when its box was presented; and every long task of the page from the navigation to
// the paint, its start and its length. The server's times are its debug lines (lib.ts's LOGGED) in
// the log --server-log names.
//
// A split run also traces Chromium from the timed navigation until every section is drawn (and the
// narrowed cards, in the author case), with the split's categories, and its `RESULT tab paint main`
// line splits the page's main thread from the navigation to the first paint, every task of it
// however short, where the long tasks show none under 50 ms; it tests whether the page's handling
// and drawing of each frame, each under 50 ms, holds the next frame's message event back (T33): how
// long the thread was busy and how long idle, by part (script, style and layout, paint, garbage
// collection, parse, other);
// every React commit's moment; for each frame of the first payload, in the order they arrived, how
// long the page's own handlers of it ran, how long the thread was busy, by part, between its write
// and its arrival, and, from its arrival to the next frame's (the paint's, for the last), the time
// busy by part and the commits; and every task of 2 ms or more, its start, its length and what it
// was (`frame:<view>`, the task whose handler read that frame; `react`, one that ended a commit;
// else what started it, lib.ts's mainThreadTasks).
//
//   bun paint.ts --url <server> --database <url> --project INFRA --case <84d-compare|28d|84d-author> \
//     [--warmed <file> --restarted <file>] [--split 1 --server-log <file>] [--rate stated|ten] \
//     [--node <node>] [--instance-type <machine type>] \
//     [--quiet 30] [--catalog 65] [--timeout 120] [--author <login>] \
//     [--sections cards,legion,ci,deploys,logged] [--login alice]
//
// The handshake: with --warmed, once the cold navigation is done the step writes that file and
// waits for the one --restarted names, which run.sh writes once the restarted server serves, then
// signs in to it again; with neither, the timed navigation follows on the same server.
//
// The cases: 84d-compare, 84 days with the compare range the header defaults to (the 84 days
// before); 28d, the default 28 days with the compare range off; 84d-author, 84 days with a compare
// range from an address that sets the author facet, whose first paint is the unnarrowed one
// (plan D7a), the page subscribing the narrowed delivery view only once the unnarrowed one has
// landed (D25 step 2), so its narrowed cards draw after the paint under a loading mark, the delay
// this case records beside the paint with the database time the narrowed read adds. Its database
// line splits db_ms into the first paint's reads and the narrowed read's (the ruling's item 4),
// statement by statement: the narrowed read's statements are those a read of it alone runs, read
// again through the list route, which answers the narrowed view's body through the same read
// (readProjectEvalList), once the narrowed cards have drawn; where the narrowed read ran within
// the paint's reading, wholly or in part (`narrowed_read_in_paint`, from when the page sent its
// subscribe and when its snapshot reached the browser), each statement of the paint's reading
// gives up as many of its calls as the read alone made of it, at that statement's own time a call
// in the paint, and the first paint's reads are what is left (`first_paint_reads_db_ms`), which
// the case's 150 ms judges (the ruling's item 4). Where only part of the narrowed read ran within
// the paint, a statement it had not reached yet gives up calls the paint's own reads made, so the
// paint's part is at least what that line states. A split run's narrowed line also gives, from the
// navigation, when the page subscribed the narrowed view (and how long after the unnarrowed
// delivery view's snapshot arrived and was parsed), the server's time for its read and its
// snapshot's (its statements and the server's own work between them, from the snapshot's begin to
// its commit), and when its snapshot arrived and was parsed (the ruling's item 3).
// Budgets (spec R4, R17; k4 and k19), each time the judged one: the first paint within 400 ms of
// the navigation; every section drawn within 2 s of it, with no scroll; the narrowed cards drawn
// within 2 s of it; no section's box a size other than the one it was first shown at by the time
// its section is drawn; the first payload at most 300 KB compressed; and, at the stated rate, at
// most 150 ms of database time at 84 days with a compare range and 60 ms at 28 days with none, as
// measured. At ten times the stated rate (--rate ten) the database time counts inside the judged
// 0.4 s and has no bound of its own: over 150 or 60 ms it prints a RECORD line, which fails
// nothing. A case fails besides when it has no index, a page refused its throttling rate, a moment
// has no judged time, or the page was not 1440×900 at device scale 1 at its top; an unread machine
// type fails nothing. A bundle load alone over 400 ms fails nothing: its line goes to the architect
// with its numbers (ledger item 62). Each line is a RESULT, each missed budget a FAIL, and the step
// exits 1 when any FAIL was printed; the boot's, the split's and the narrowed read's fields judge
// nothing.
import { existsSync, writeFileSync } from "node:fs";

import {
  benchmarkIndex,
  boxChange,
  busyIn,
  busyWithin,
  type CDPSession,
  type Counted,
  counted,
  database,
  type Frame,
  fail,
  failed,
  firstMark,
  flags,
  frameKey,
  gzipBytes,
  type Instrumented,
  inFirstScreen,
  judge,
  judgedFields,
  LIGHT_TRACE_CATEGORIES,
  LiveLog,
  LOGGED,
  type Logged,
  launchChromium,
  logSize,
  mainThreadTasks,
  mark,
  ms,
  need,
  numberOf,
  OWN,
  type Page,
  REFERENCE_INDEX,
  type Rect,
  readerContext,
  readInstrument,
  recomputes,
  recordLine,
  resetStatements,
  result,
  type SectionPaint,
  type Size,
  screenPaint,
  serverLines,
  signIn,
  sleep,
  speedFields,
  speedOf,
  splitOf,
  TASK_PARTS,
  type Task,
  type TaskPart,
  TRACE_CATEGORIES,
  throttle,
  VIEWPORT,
  type View,
} from "./lib";

const given = flags(process.argv.slice(2));
const base = need(given, "url").replace(/\/$/, "");
const project = given.get("project") ?? "INFRA";
const which = need(given, "case");
const rate = given.get("rate") ?? "stated";
const quietMs = numberOf(given, "quiet", 30) * 1000;
const catalogMs = numberOf(given, "catalog", 65) * 1000;
const timeoutMs = numberOf(given, "timeout", 120) * 1000;
const sections = (given.get("sections") ?? "cards,legion,ci,deploys,logged").split(",");
const login = given.get("login") ?? "alice";
const warmedFile = given.get("warmed");
const restartedFile = given.get("restarted");
const split = splitOf(given);
const serverLog = given.get("server-log");
const node = given.get("node") ?? "";
const instanceType = given.get("instance-type") ?? "";
if ((warmedFile === undefined) !== (restartedFile === undefined))
  throw new Error(
    "--warmed and --restarted name the handshake's two files, together or not at all"
  );
if (serverLog !== undefined && !split)
  throw new Error(
    "--server-log is read by a split run alone (--split 1), whose server logs at debug"
  );

const BUDGET_PAINT_MS = 400;
const BUDGET_EVERY_SECTION_MS = 2000;
const BUDGET_GZIP = 300 * 1000;
const BUDGET_NARROWED_MS = 2000;
/** How long the step waits for run.sh to restart the server: its start_server waits up to 900 s. */
const RESTART_WAIT_MS = 960 * 1000;
const cases: Record<string, { query: string; budgetDbMs: number; narrowed: boolean }> = {
  "84d-compare": { query: "window=84d", budgetDbMs: 150, narrowed: false },
  "28d": { query: "compare=off", budgetDbMs: 60, narrowed: false },
  "84d-author": { query: "window=84d", budgetDbMs: 150, narrowed: true },
};
const chosen = cases[which];
if (chosen === undefined)
  throw new Error(`--case is one of ${Object.keys(cases).join(", ")}, not ${which}`);

const db = database(need(given, "database"), 2);
await db.unsafe(`${OWN} create extension if not exists pg_stat_statements`);
let query = chosen.query;
if (chosen.narrowed) {
  // The author with the most pull requests merged in the window, unless one is named.
  const named = given.get("author");
  const rows = (await db.unsafe(
    `${OWN} select author from delivery_pull_requests where merged_at >= now() - interval '84 days'
     group by author order by count(*) desc, author limit 1`
  )) as { author: string }[];
  const author = named ?? rows[0]?.author;
  if (author === undefined)
    throw new Error("the window holds no pull request, so no author facet can be set");
  query += `&author=${encodeURIComponent(author)}`;
}
const path = `/projects/${encodeURIComponent(project)}/evals?${query}`;

// Every section drawn (its drawing marked and presented) or refused, and with the CI section the
// two job lists drawn.
const drawnAll = `(() => { const r = window.__evalbench; if (!r) return false;
     const want = ${JSON.stringify(sections)};
     const done = (s) => s !== undefined && ((s.drawn !== undefined && s.drawn.painted !== undefined)
       || (s.state === "failed" && s.painted !== undefined));
     if (!want.every((id) => done(r.sections[id]))) return false;
     const lists = Object.values(r.jobLists);
     return !want.includes("ci") || (lists.length >= 2 && lists.every((one) => one.painted !== undefined)); })()`;

/** The page's viewport, device scale and scroll, and where each section lies in the viewport,
 *  read once every section is drawn, after the moments the case judges, so the read costs them
 *  nothing. */
interface Screen {
  width: number;
  height: number;
  scale: number;
  scrollX: number;
  scrollY: number;
  rects: Record<string, Rect>;
}

function screenOf(page: Page): Promise<Screen> {
  return page.evaluate<Screen>(
    `(() => ({ width: innerWidth, height: innerHeight, scale: devicePixelRatio, scrollX, scrollY,
       rects: Object.fromEntries([...document.querySelectorAll("[data-section]")].map((element) => {
         const r = element.getBoundingClientRect();
         return [element.getAttribute("data-section"), { top: r.top, bottom: r.bottom, left: r.left, right: r.right }];
       })) }))()`
  );
}

/** The times of a page's paint, each from the navigation's start (performance.timeOrigin) to its
 *  presentation: each section's box and drawing, its CI job lists' drawing counted in its own, and
 *  whether it lies in the first screen (`perSection`); each CI job list's (`listTimes`); and the
 *  first paint and the moment every section was drawn (lib.ts's screenPaint), null while one of
 *  them has not been presented. */
function paintTimes(record: Instrumented, screen: Screen | null) {
  const since = (at: number | undefined) => (at === undefined ? null : at - record.start);
  const listTimes = Object.entries(record.jobLists).map(
    ([id, one]) => [id, since(one.painted), one.failed] as const
  );
  const perSection: SectionPaint[] = sections.map((id) => {
    const one = record.sections[id];
    const lists = Object.values(record.jobLists)
      .filter((list) => list.section === id)
      .map((list) => since(list.painted));
    const own = since(one?.drawn?.painted);
    const drawn =
      own === null || lists.some((at) => at === null)
        ? null
        : Math.max(own, ...(lists as number[]));
    const rect = screen?.rects[id];
    return {
      id,
      shown: since(one?.box?.painted),
      drawn,
      inView: rect !== undefined && inFirstScreen(rect),
    };
  });
  return { listTimes, perSection, since, ...screenPaint(perSection) };
}

/** The app's JavaScript a page loaded, from its resource timings: how many files, their bytes
 *  decoded, as sent (encoded) and as transferred (0 for a file the browser's cache answered), and
 *  when the last of them had loaded, in milliseconds from the navigation's start. */
interface Bundle {
  files: number;
  decoded: number;
  encoded: number;
  transfer: number;
  loadedMs: number | null;
}

function bundleOf(page: Page): Promise<Bundle> {
  return page.evaluate<Bundle>(
    `(() => { const scripts = performance.getEntriesByType("resource").filter((entry) =>
       entry.initiatorType === "script" || entry.name.split("?")[0].endsWith(".js"));
     const sum = (field) => scripts.reduce((n, entry) => n + (entry[field] || 0), 0);
     return { files: scripts.length, decoded: sum("decodedBodySize"), encoded: sum("encodedBodySize"),
       transfer: sum("transferSize"),
       loadedMs: scripts.length === 0 ? null : Math.max(...scripts.map((entry) => entry.responseEnd)) }; })()`
  );
}

/** The page's boot from its own timings, each in milliseconds from the navigation's start: when
 *  the document's response ended, when it was interactive and when its DOMContentLoaded handlers
 *  had run; each script it loaded with when its request began and its response ended; and each
 *  request of the app's API (`/api/`) with the same two moments. */
interface Boot {
  document: number | null;
  interactive: number | null;
  loaded: number | null;
  scripts: { name: string; at: number; end: number }[];
  api: { name: string; at: number; end: number }[];
}

function bootOf(page: Page): Promise<Boot> {
  return page.evaluate<Boot>(
    `(() => { const [nav] = performance.getEntriesByType("navigation");
     const resources = performance.getEntriesByType("resource");
     const one = (entry) => ({ name: new URL(entry.name).pathname, at: entry.startTime, end: entry.responseEnd });
     return { document: nav ? nav.responseEnd : null, interactive: nav ? nav.domInteractive : null,
       loaded: nav ? nav.domContentLoadedEventEnd : null,
       scripts: resources.filter((entry) => entry.initiatorType === "script" || entry.name.split("?")[0].endsWith(".js")).map(one),
       api: resources.filter((entry) => new URL(entry.name).pathname.startsWith("/api/")).map(one) }; })()`
  );
}

const browser = await launchChromium();
// The host's speed, measured in this Chromium before the case (the ruling's B4), which sets how
// every time of the case is judged (the header).
const speed = speedOf(await benchmarkIndex(browser));
const machine = speedFields(speed, node, instanceType);
const traceCategories = split
  ? TRACE_CATEGORIES
  : speed !== null && speed.scale !== null
    ? LIGHT_TRACE_CATEGORIES
    : null;
mark(
  `paint case=${which}: Chromium ${browser.version()}, ${base}${path}${split ? " (a split run)" : ""}, ${machine}`
);
result(
  `tab paint machine case=${which} rate=${rate} ${machine} ` +
    `cpu=${speed === null ? "none" : speed.throttle !== null ? "throttled" : "unthrottled"} ` +
    `main_thread_from=${speed === null || speed.scale === null ? "none" : split ? "split_trace" : "light_trace"}`
);
if (speed === null)
  fail(
    `tab paint case=${which} rate=${rate}: the host's BenchmarkIndex was not measured, so no time of the case is judged`
  );
const context = await readerContext(browser, base, login, split);
// The DevTools sessions that hold each page's CPU throttling, kept until the pages close.
const sessions: CDPSession[] = [];

/** Throttles `one`'s CPU at index ÷ 1500 where the host is above the reference: whether its
 *  times are judged as measured, false (and a FAIL) where Chromium refused the rate. */
async function throttled(one: Page, what: string): Promise<boolean> {
  if (speed === null || speed.throttle === null) return true;
  const session = await throttle(context, one, speed.throttle);
  if (session !== null) {
    sessions.push(session);
    return true;
  }
  fail(
    `tab paint case=${which} rate=${rate}: Chromium refused the CPU throttling rate ${speed.throttle} (index ${speed.index} ÷ ${REFERENCE_INDEX}) on the ${what} page, so its times are not judged`
  );
  return false;
}

// The cold navigation: the tab opened once in the new context, which loads the app's JavaScript
// into the context's cache, its first paint and its time to every section drawn, the bundle's size
// and its load time recorded (ledger item 62), throttled as the timed page is.
const cold = await context.newPage();
await throttled(cold, "cold");
await cold.goto(`${base}${path}`, { waitUntil: "commit", timeout: timeoutMs });
let coldPaint: { firstPaint: number | null; everySection: number | null } = {
  firstPaint: null,
  everySection: null,
};
try {
  await cold.waitForFunction(drawnAll, undefined, { polling: 25, timeout: timeoutMs });
  coldPaint = paintTimes(await readInstrument(cold), await screenOf(cold));
} catch (error) {
  fail(
    `tab paint case=${which} rate=${rate}: the cold browser's sections did not all draw within ${timeoutMs} ms: ${String(error).slice(0, 200)}`
  );
}
const coldBundle = await bundleOf(cold);
await cold.close();
const bundleOver = coldBundle.loadedMs !== null && coldBundle.loadedMs > BUDGET_PAINT_MS;
result(
  `tab paint case=${which} rate=${rate} cold_first_paint_raw_ms=${ms(coldPaint.firstPaint)} ` +
    `cold_every_section_raw_ms=${ms(coldPaint.everySection)} bundle_files=${coldBundle.files} ` +
    `bundle_bytes=${coldBundle.decoded} bundle_encoded_bytes=${coldBundle.encoded} bundle_transfer_bytes=${coldBundle.transfer} ` +
    `bundle_load_ms=${ms(coldBundle.loadedMs)} bundle_load_over_${BUDGET_PAINT_MS}_ms=${bundleOver} server_restarted_after=${warmedFile !== undefined} ${machine}`
);
if (bundleOver)
  mark(
    `paint case=${which}: the bundle alone loads in ${ms(coldBundle.loadedMs)} ms, over ${BUDGET_PAINT_MS} ms: this goes to the architect with its numbers (ledger item 62)`
  );

// The handshake: run.sh restarts the server, and the reader signs in to the new process.
if (warmedFile !== undefined && restartedFile !== undefined) {
  writeFileSync(warmedFile, `${new Date().toISOString()}\n`);
  const deadline = Date.now() + RESTART_WAIT_MS;
  while (!existsSync(restartedFile)) {
    if (Date.now() > deadline)
      throw new Error(`run.sh wrote no ${restartedFile} within ${RESTART_WAIT_MS} ms`);
    await sleep(200);
  }
  await signIn(context, base, login);
}

const page = await context.newPage();
// A page above the reference whose throttling Chromium refused has no judged time.
const judgedSpeed = (await throttled(page, "timed")) ? speed : null;
const live = new LiveLog(true);
live.listen(page);
const requests: { url: string; at: number }[] = [];
page.on("request", (request) => requests.push({ url: request.url(), at: Date.now() }));

// The catalogue: every statement the database ran in a window with no request in flight, after a
// quiet spell for the server's start to settle.
await sleep(quietMs);
await resetStatements(db);
await sleep(catalogMs);
const background = await counted(db);
const catalog = new Set(background.map((one) => one.queryid));
result(
  `tab paint case=${which} rate=${rate} catalog_statements=${catalog.size} catalog_calls=${background.reduce((n, one) => n + one.calls, 0)} catalog_db_ms=${ms(background.reduce((n, one) => n + one.ms, 0))}`
);

// The paint: pg_stat_statements reset, the navigation in the warm browser, every section drawn,
// and pg_stat_statements read then; the server's log is read from here on. Where the page runs
// unthrottled, or in a split run, Chromium is traced (traceCategories) from just before the
// navigation until every section, and in the author case the narrowed cards, have been drawn.
const logFrom = serverLog === undefined ? 0 : logSize(serverLog);
await resetStatements(db);
if (traceCategories !== null)
  await browser.startTracing(page, { categories: traceCategories, screenshots: false });
const called = Date.now();
await page.goto(`${base}${path}`, { waitUntil: "commit", timeout: timeoutMs });
// When the step saw every section drawn: the first payload and the paint's database time are cut
// there.
let paintedAt = 0;
try {
  await page.waitForFunction(drawnAll, undefined, { polling: 25, timeout: timeoutMs });
  paintedAt = Date.now();
} catch (error) {
  fail(
    `tab paint case=${which} rate=${rate}: the sections did not all draw within ${timeoutMs} ms: ${String(error).slice(0, 200)}`
  );
}
const paintReadAt = Date.now();
const atPaint = await counted(db);
const screen = await screenOf(page);
let atNarrowed: Counted[] | null = null;
if (chosen.narrowed && paintedAt > 0) {
  try {
    await page.waitForFunction(
      "(() => { const n = window.__evalbench.narrowing; return n.narrowed !== undefined && n.narrowed.painted !== undefined; })()",
      undefined,
      { polling: 25, timeout: timeoutMs }
    );
    atNarrowed = await counted(db);
  } catch (error) {
    fail(
      `tab paint case=${which} rate=${rate}: the narrowed cards did not draw within ${timeoutMs} ms of every section's drawing: ${String(error).slice(0, 200)}`
    );
  }
}
const trace = traceCategories === null ? null : await browser.stopTracing();
const record: Instrumented = await readInstrument(page);
const warmBundle = await bundleOf(page);
const boot = await bootOf(page);
const timelineState = await page.evaluate<string | null>(
  `(() => { const e = document.querySelector('[data-section="timeline"]'); return e ? e.getAttribute("data-state") : null; })()`
);

const { firstPaint, everySection, listTimes, perSection, since } = paintTimes(record, screen);
// The page's main thread from the navigation, from the trace, and each moment judged by it
// (lib.ts's judge): its busy time before the moment is the moment's main-thread time.
const main = trace === null ? null : mainThreadTasks(JSON.parse(trace.toString("utf8")), path);
const judgedAt = (moment: number | null) =>
  judge(
    moment,
    moment === null || main === null ? null : busyIn(main.tasks, 0, moment).ms,
    judgedSpeed
  );
const firstPaintJudged = judgedAt(firstPaint);
const everySectionJudged = judgedAt(everySection);
// Each section's box, from when it was first shown (its drawing, where the app set no box mark)
// to when its section was drawn, in the page's own moments (lib.ts's boxChange).
const boxes = perSection.map((one) => {
  const marked = record.sections[one.id];
  return {
    id: one.id,
    marked: marked?.box !== undefined,
    ...boxChange(
      marked?.sizes ?? [],
      marked?.box?.painted ?? marked?.drawn?.painted,
      one.drawn === null ? undefined : record.start + one.drawn
    ),
  };
});
const failedSections = Object.entries(record.sections)
  .filter(([, one]) => one.state === "failed")
  .map(([id]) => id);

// The first payload: the first snapshot of each of the tab's unnarrowed evals views, of the CI
// section's two items views (pr-ci's job lists) and of the header's legion-read view (plan D23),
// heard before every section was drawn. A snapshot of another view heard by then (the narrowed
// delivery view, a declared eval's items view) is counted beside it, outside the budget.
const FACET_KEYS = [
  "parent_agent",
  "session",
  "issue",
  "priority",
  "component",
  "author",
  "rework",
  "deployed",
  "q",
];
const classOf = (view: { kind: string; [field: string]: unknown }): string => {
  const viewQuery = new URLSearchParams(typeof view.query === "string" ? view.query : "");
  if (view.kind === "evals")
    return FACET_KEYS.some((key) => viewQuery.has(key)) ? "narrowed" : "evals";
  if (view.kind === "items") return view.slug === "pr-ci" ? "items" : "other-items";
  if (view.kind === "legion-read") return "legion-read";
  return "other";
};
const payload: Record<string, { frames: number; gzip: number; raw: number }> = {};
const firstSnapshot = new Set<string>();
for (const frame of live.frames) {
  if (paintedAt === 0 || frame.heard > paintedAt) break;
  const parsed = frame.parsed;
  if (parsed === null || parsed.type !== "snapshot" || typeof parsed.sub !== "string") continue;
  const view = live.views.get(parsed.sub);
  if (view === undefined || firstSnapshot.has(parsed.sub)) continue;
  firstSnapshot.add(parsed.sub);
  const kind = classOf(view);
  const one = payload[kind] ?? { frames: 0, gzip: 0, raw: 0 };
  payload[kind] = one;
  one.frames++;
  one.gzip += gzipBytes(frame.text);
  one.raw += frame.text.length;
}
const budgeted = [payload.evals, payload.items, payload["legion-read"]];
// A project with a Legion daemon, whose Legion section the paint waits for, opens its header's
// legion-read view with the evals views.
const daemonRead = sections.includes("legion");
const gzip = budgeted.reduce((n, one) => n + (one?.gzip ?? 0), 0);
const raw = budgeted.reduce((n, one) => n + (one?.raw ?? 0), 0);

const own = (rows: Counted[]) => rows.filter((one) => !catalog.has(one.queryid));
const total = (rows: Counted[]) => rows.reduce((n, one) => n + one.ms, 0);
const dbMs = total(own(atPaint));

// The split's views: the first snapshot the page heard of each subscription it holds, with the view
// it answers and its name in the split (`evals.<kind>`, `narrowed.<kind>`, `items.<sort>`,
// `legion-read`, an evals view of a page past the first with `@<offset>` after it), none for any
// other view; and the server's debug lines since the navigation.
const queryOf = (view: View) =>
  new URLSearchParams(typeof view.query === "string" ? view.query : "");
const nameOf = (view: View): string | null => {
  const kind = classOf(view);
  const viewQuery = queryOf(view);
  if (kind === "evals" || kind === "narrowed") {
    const offset = viewQuery.get("offset");
    return `${kind}.${viewQuery.get("kind") ?? "all"}${offset === null ? "" : `@${offset}`}`;
  }
  if (kind === "items") return `items.${viewQuery.get("sort") ?? "rows"}`;
  return kind === "legion-read" ? kind : null;
};
type Snapshot = { name: string; sub: string; view: View; frame: Frame };
const snapshots: Snapshot[] = [];
for (const frame of live.frames) {
  const parsed = frame.parsed;
  if (parsed === null || parsed.type !== "snapshot" || typeof parsed.sub !== "string") continue;
  const view = live.views.get(parsed.sub);
  const name = view === undefined ? null : nameOf(view);
  if (view === undefined || name === null || snapshots.some((one) => one.sub === parsed.sub))
    continue;
  snapshots.push({ name, sub: parsed.sub, view, frame });
}
const logged = serverLog === undefined ? [] : serverLines(serverLog, logFrom);
const arrivals = new Map(record.frames.map((one) => [frameKey(one.socket, one.index), one.at]));
const fromStart = (at: number | null | undefined) =>
  at === null || at === undefined ? null : at - record.start;
/** The server's recompute of a view for its first subscriber (LOGGED.recompute, cause
 *  subscribe). */
const recomputeOf = (view: View): Logged | undefined =>
  logged.find(
    (line) =>
      line.text("cause") === "subscribe" && recomputes(line, view, classOf(view) === "narrowed")
  );
/** Each time of a snapshot's way to the page, from the navigation, and each of the server's steps
 *  in milliseconds, by its name in the split. */
const timesOf = (one: Snapshot): [string, number | null][] => {
  const recompute = recomputeOf(one.view);
  const wrote = logged.find(
    (line) =>
      line.msg === LOGGED.frame && line.text("type") === "snapshot" && line.text("sub") === one.sub
  );
  const sent = record.sends.find(
    (send) => send.socket === one.frame.socket && send.sub === one.sub
  );
  return [
    ["subscribed_ms", fromStart(sent?.at)],
    ["server_began_ms", fromStart(recompute?.at("began_unix_us"))],
    ["head_ms", recompute?.number("head_ms") ?? null],
    ["read_ms", recompute?.number("read_ms") ?? null],
    ["snapshot_ms", recompute?.number("snapshot_ms") ?? null],
    ["fold_ms", recompute?.number("fold_ms") ?? null],
    ["freshness_ms", recompute?.number("freshness_ms") ?? null],
    ["encode_ms", recompute?.number("encode_ms") ?? null],
    ["server_ended_ms", fromStart(recompute?.at("ended_unix_us"))],
    ["frame_encode_ms", wrote?.number("encode_ms") ?? null],
    ["frame_write_ms", wrote?.number("write_ms") ?? null],
    ["written_ms", fromStart(wrote?.at("wrote_unix_us"))],
    ["arrived_ms", fromStart(arrivals.get(frameKey(one.frame.socket, one.frame.index)))],
    ["parsed_ms", fromStart(firstMark(record, `dispatch:live-snapshot:${one.sub}`))],
  ];
};
const timesByName = new Map(snapshots.map((one) => [one.name, new Map(timesOf(one))]));
const timeOf = (name: string, key: string) => timesByName.get(name)?.get(key) ?? null;

// The narrowed read alone (the ruling's item 4), once the narrowed cards have drawn: the list
// route's read of the narrowed delivery view's query and window, which answers the view's body
// through the same read (readProjectEvalList), with pg_stat_statements reset before it, so the
// statements it counts are the read's own. Whether the narrowed read ran within the paint's
// reading of pg_stat_statements (none, whole or part of it): the page's subscribe and the view's
// snapshot as the browser's record of its socket heard them, against the moment of that reading.
// The paint's reading then gives up, statement by statement, as many calls as the read alone
// made of that statement, at the statement's own time a call in the paint (the header).
const narrowed = snapshots.find((one) => one.name === "narrowed.delivery");
let alone: Counted[] | null = null;
let narrowedInPaint = "none";
if (chosen.narrowed) {
  const narrowedView =
    narrowed?.view ?? [...live.views.values()].find((one) => classOf(one) === "narrowed");
  if (narrowedView !== undefined && atNarrowed !== null) {
    const range = (narrowedView.window ?? {}) as { preset?: number; from?: string; to?: string };
    const to = range.to ?? new Date().toISOString();
    const from =
      range.from ?? new Date(Date.parse(to) - (range.preset ?? 84) * 86_400_000).toISOString();
    const routeQuery = queryOf(narrowedView);
    routeQuery.set("from", from);
    routeQuery.set("to", to);
    await resetStatements(db);
    const answer = await context.request.get(
      `${base}/api/v1/projects/${encodeURIComponent(project)}/evals?${routeQuery}`,
      {}
    );
    const read = await counted(db);
    if (answer.status() === 200) alone = own(read);
    else
      mark(
        `paint case=${which}: the narrowed read again answered ${answer.status()}, so its database time is none`
      );
  }
  const sub =
    narrowed?.sub ?? [...live.views].find(([, view]) => classOf(view) === "narrowed")?.[0];
  const sentAt = sub === undefined ? undefined : live.subscribed.get(sub);
  narrowedInPaint =
    paintedAt === 0
      ? "unknown"
      : sentAt === undefined || sentAt >= paintReadAt
        ? "none"
        : narrowed !== undefined && narrowed.frame.heard <= paintReadAt
          ? "whole"
          : "part";
}
const narrowedReadDbMs = alone === null ? null : total(alone);
let narrowedInPaintDbMs: number | null = 0;
if (chosen.narrowed && narrowedInPaint !== "none") {
  const aloneCalls = new Map((alone ?? []).map((one) => [one.queryid, one.calls]));
  narrowedInPaintDbMs = alone === null || narrowedInPaint === "unknown" ? null : 0;
  for (const one of own(atPaint)) {
    const calls = Math.min(one.calls, aloneCalls.get(one.queryid) ?? 0);
    if (narrowedInPaintDbMs !== null && calls > 0)
      narrowedInPaintDbMs += (one.ms * calls) / one.calls;
  }
}
const firstPaintReadsDbMs = narrowedInPaintDbMs === null ? null : dbMs - narrowedInPaintDbMs;

const apiRequests = requests.filter(
  (one) => paintedAt > 0 && one.at <= paintedAt && new URL(one.url).pathname.startsWith("/api/")
);
const detailPrefix = `/api/v1/projects/${project}/evals/`;
const detailReads = apiRequests.filter((one) => {
  const p = new URL(one.url).pathname;
  return (
    p.startsWith(detailPrefix) &&
    p.length > detailPrefix.length &&
    !p.slice(detailPrefix.length).includes("/")
  );
});
const sizeText = (size: Size | null) => (size === null ? "none" : `${size.width}x${size.height}`);
result(
  `tab paint case=${which} rate=${rate} path=${path} ${judgedFields("first_paint", firstPaint, firstPaintJudged)} ` +
    `budget_ms=${BUDGET_PAINT_MS} ${judgedFields("every_section", everySection, everySectionJudged)} ` +
    `budget_every_section_ms=${BUDGET_EVERY_SECTION_MS} ${machine} ` +
    `first_screen=[${perSection
      .filter((one) => one.inView)
      .map((one) => one.id)
      .join(" ")}] viewport=${screen.width}x${screen.height} device_scale=${screen.scale} ` +
    `scroll=${screen.scrollX},${screen.scrollY} navigation_called_before_start_ms=${ms(record.start - called)} ` +
    `sections=[${perSection.map((one) => `${one.id}=${ms(one.shown)}/${ms(one.drawn)}${one.inView ? "(first screen)" : ""}`).join(" ")}] ` +
    `job_lists=[${listTimes.map(([id, at, bad]) => `${id}=${ms(at)}${bad ? "(failed)" : ""}`).join(" ")}] timeline=${timelineState} ` +
    `warm_bundle_transfer_bytes=${warmBundle.transfer} warm_bundle_load_ms=${ms(warmBundle.loadedMs)} ` +
    `cold_first_paint_raw_ms=${ms(coldPaint.firstPaint)} cold_every_section_raw_ms=${ms(coldPaint.everySection)}`
);
result(
  `tab paint boxes case=${which} rate=${rate} box_marks=${boxes.filter((one) => one.marked).length} ` +
    `resized=${boxes.filter((one) => one.changed === true).length} unread=${boxes.filter((one) => one.changed === null).length} ` +
    `sections=[${boxes.map((one) => `${one.id}:${sizeText(one.shown)}->${sizeText(one.drawn)}${one.marked ? "" : "(no box mark)"}`).join(" ")}]`
);
result(
  `tab paint case=${which} rate=${rate} payload_gzip_bytes=${gzip} payload_raw_bytes=${raw} budget_gzip_bytes=${BUDGET_GZIP} ` +
    `payload_frames=[${Object.entries(payload)
      .map(([kind, one]) => `${kind}:${one.frames}:${one.gzip}`)
      .join(" ")}] ` +
    `live_frames_before_paint=${live.frames.filter((frame) => paintedAt > 0 && frame.heard <= paintedAt).length}`
);
result(
  `tab paint case=${which} rate=${rate} db_ms=${ms(dbMs)} budget_db_ms=${chosen.budgetDbMs} db_ms_everything_in_flight=${ms(total(atPaint))} ` +
    `first_paint_reads_db_ms=${ms(firstPaintReadsDbMs)} narrowed_read_db_ms_in_paint=${ms(narrowedInPaintDbMs)} ` +
    `narrowed_read_db_ms=${ms(narrowedReadDbMs)} narrowed_read_in_paint=${narrowedInPaint} ` +
    `statements=${own(atPaint).length} catalogued_statements_set_aside=${atPaint.length - own(atPaint).length} ` +
    `api_requests=${apiRequests.length} eval_detail_reads=${detailReads.length}`
);
for (const one of own(atPaint)
  .sort((a, b) => b.ms - a.ms)
  .slice(0, 12)) {
  mark(
    `paint case=${which} statement ms=${ms(one.ms)} calls=${one.calls} ${one.query.replace(/\s+/g, " ").slice(0, 220)}`
  );
}

// The boot (the architect's point A of the ruling on the split): the page's navigation and
// resource timings, and in a split run its socket's and its subscribes' moments and its long tasks
// before the first subscribe; each from the navigation.
const firstOf = (times: Iterable<number>) => {
  let first: number | null = null;
  for (const at of times) if (first === null || at < first) first = at;
  return first;
};
const routeChunk = boot.scripts.find((one) => one.name.includes("ProjectPage"));
const subscribeHeard = firstOf(live.subscribed.values());
const firstSend = firstOf(record.sends.map((one) => one.at));
const evalsSend = firstOf(
  record.sends.filter((one) => live.views.get(one.sub)?.kind === "evals").map((one) => one.at)
);
const bootEnd = subscribeHeard ?? (paintedAt > 0 ? paintedAt : Number.POSITIVE_INFINITY);
result(
  `tab paint boot case=${which} rate=${rate} split=${split} document_ms=${ms(boot.document)} interactive_ms=${ms(boot.interactive)} ` +
    `dom_content_loaded_ms=${ms(boot.loaded)} scripts=${boot.scripts.length} ` +
    `scripts_loaded_ms=${ms(boot.scripts.length === 0 ? null : Math.max(...boot.scripts.map((one) => one.end)))} ` +
    `route_chunk=${routeChunk?.name ?? "none"} route_chunk_requested_ms=${ms(routeChunk?.at)} route_chunk_loaded_ms=${ms(routeChunk?.end)} ` +
    `api_before_subscribe=[${boot.api
      .filter((one) => record.start + one.at < bootEnd)
      .map((one) => `${one.name}@${ms(one.at)}-${ms(one.end)}`)
      .join(" ")}] ` +
    `subscribe_heard_ms=${ms(fromStart(subscribeHeard))} socket_made_ms=${ms(fromStart(record.opened[0]?.made))} ` +
    `socket_open_ms=${ms(fromStart(record.opened[0]?.open))} first_subscribe_ms=${ms(fromStart(firstSend))} ` +
    `evals_subscribe_ms=${ms(fromStart(evalsSend))} ` +
    `busy_before_subscribe_ms=${split && firstSend !== null ? ms(busyWithin(record, record.start, firstSend)) : "none"}`
);

// The split (the ruling's item 1), a split run's alone: each view of the first payload, then each
// section and CI job list, then the page's long tasks to the paint.
if (split) {
  const fields = [
    `first_paint_raw_ms=${ms(firstPaint)}`,
    `every_section_raw_ms=${ms(everySection)}`,
    `server_log=${serverLog !== undefined}`,
    `server_lines=${logged.length}`,
  ];
  for (const one of snapshots) {
    if (one.name.startsWith("narrowed.")) continue;
    for (const [key, value] of timesByName.get(one.name) ?? [])
      fields.push(`${one.name}.${key}=${ms(value)}`);
    const written = timeOf(one.name, "written_ms");
    const arrived = timeOf(one.name, "arrived_ms");
    fields.push(
      `${one.name}.busy_before_arrival_ms=${written === null || arrived === null ? "none" : ms(busyWithin(record, record.start + written, record.start + arrived))}`,
      `${one.name}.raw_bytes=${one.frame.text.length}`,
      `${one.name}.gzip_bytes=${gzipBytes(one.frame.text)}`
    );
  }
  for (const one of perSection) {
    const committed = fromStart(firstMark(record, `dispatch:evals-section:${one.id}`));
    fields.push(
      `section.${one.id}.committed_ms=${ms(committed)}`,
      `section.${one.id}.box_presented_ms=${ms(one.shown)}`,
      `section.${one.id}.presented_ms=${ms(one.drawn)}`
    );
  }
  for (const [id, presented] of listTimes) {
    const committed = fromStart(firstMark(record, `dispatch:evals-job-list:${id}`));
    fields.push(
      `job_list.${id}.committed_ms=${ms(committed)}`,
      `job_list.${id}.presented_ms=${ms(presented)}`
    );
  }
  const tasks = record.longTasks.filter((task) => paintedAt === 0 || task.at <= paintedAt);
  fields.push(
    `long_tasks_ms=${ms(tasks.reduce((n, task) => n + task.ms, 0))}`,
    `long_tasks=[${tasks.map((task) => `${ms(fromStart(task.at))}+${ms(task.ms)}`).join(" ")}]`
  );
  result(`tab paint split case=${which} rate=${rate} ${fields.join(" ")}`);

  // The page's main thread to the first paint, from the trace (the header): each frame of the
  // first payload in the order it arrived, then every task of 2 ms or more, named by the frame whose
  // handler it ran or the commit it ended, else by what started it.
  const mainFields = [
    `trace_events=${main?.events ?? 0}`,
    `main_thread=${main === null ? "none" : "found"}`,
  ];
  if (main !== null && firstPaint !== null) {
    const partsText = (parts: Record<TaskPart, number>) =>
      TASK_PARTS.map((part) => `${part}:${ms(parts[part])}`).join(",");
    const tasks = main.tasks.filter((task) => task.at < firstPaint);
    const commits = record.commits.map((at) => at - record.start).filter((at) => at <= firstPaint);
    const frames = snapshots.flatMap((one) => {
      const arrived = timeOf(one.name, "arrived_ms");
      if (one.name.startsWith("narrowed.") || arrived === null) return [];
      const entry = record.frames.find(
        (frame) => frame.socket === one.frame.socket && frame.index === one.frame.index
      );
      return [
        {
          arrived,
          handled: entry?.handled ?? null,
          name: one.name,
          written: timeOf(one.name, "written_ms"),
        },
      ];
    });
    frames.sort((a, b) => a.arrived - b.arrived);
    const kindOf = (task: Task) => {
      const end = task.at + task.ms;
      const frame = frames.find((one) => one.arrived >= task.at && one.arrived < end);
      if (frame !== undefined) return `frame:${frame.name}`;
      return commits.some((at) => at >= task.at && at <= end) ? "react" : task.started;
    };
    const whole = busyIn(tasks, 0, firstPaint);
    mainFields.push(
      `until_ms=${ms(firstPaint)}`,
      `busy_ms=${ms(whole.ms)}`,
      `idle_ms=${ms(firstPaint - whole.ms)}`,
      `busy_parts=[${partsText(whole.parts)}]`,
      `tasks=${tasks.length}`,
      `tasks_over_50_ms=${tasks.filter((task) => task.ms >= 50).length}`,
      `commits=[${commits.map((at) => ms(at)).join(" ")}]`
    );
    frames.forEach((one, place) => {
      const next = frames[place + 1]?.arrived ?? firstPaint;
      const before = one.written === null ? null : busyIn(tasks, one.written, one.arrived);
      const after = busyIn(tasks, one.arrived, next);
      mainFields.push(
        `${one.name}.arrived_ms=${ms(one.arrived)}`,
        `${one.name}.handler_ms=${ms(one.handled)}`,
        `${one.name}.busy_before_arrival_ms=${ms(before?.ms)}`,
        `${one.name}.busy_before_arrival_parts=[${before === null ? "none" : partsText(before.parts)}]`,
        `${one.name}.until_next_ms=${ms(next - one.arrived)}`,
        `${one.name}.until_next_busy_ms=${ms(after.ms)}`,
        `${one.name}.until_next_parts=[${partsText(after.parts)}]`,
        `${one.name}.until_next_commits=${commits.filter((at) => at >= one.arrived && at < next).length}`
      );
    });
    mainFields.push(
      `task_list=[${tasks
        .filter((task) => task.ms >= 2)
        .map((task) => `${ms(task.at)}+${ms(task.ms)}:${kindOf(task)}`)
        .join(" ")}]`
    );
  }
  result(`tab paint main case=${which} rate=${rate} ${mainFields.join(" ")}`);
}

const narrowedAt = chosen.narrowed ? since(record.narrowing.narrowed?.painted) : null;
const narrowedJudged = judgedAt(narrowedAt);
if (chosen.narrowed) {
  const cardsAt = since(record.sections.cards?.drawn?.painted);
  const minus = (a: number | null, b: number | null) => (a === null || b === null ? null : a - b);
  const throughNarrowed = atNarrowed === null ? null : total(own(atNarrowed));
  const judged =
    `tab paint case=${which} rate=${rate} ${judgedFields("narrowed_cards", narrowedAt, narrowedJudged)} ` +
    `budget_narrowed_ms=${BUDGET_NARROWED_MS} ${machine} ` +
    `narrowed_after_first_paint_ms=${ms(minus(narrowedAt, firstPaint))} narrowed_after_cards_painted_ms=${ms(minus(narrowedAt, cardsAt))} ` +
    `loading_mark_seen=${record.narrowing.loading !== undefined} db_ms_through_narrowed=${ms(throughNarrowed)} ` +
    `db_ms_after_first_paint_through_narrowed=${ms(throughNarrowed === null ? null : throughNarrowed - dbMs)} ` +
    `narrowed_read_db_ms=${ms(narrowedReadDbMs)} narrowed_read_in_paint=${narrowedInPaint}`;
  // A negative delay is a narrowed view that landed before that paint: the cards then drew their
  // narrowed values before the last section of the first paint. The server's snapshot of the
  // narrowed read is its statements and the server's own work between them, from the snapshot's
  // begin to its commit; narrowed_read_db_ms is the statements' execution alone.
  const name = "narrowed.delivery";
  const subscribed = timeOf(name, "subscribed_ms");
  const serverSnapshot = ["head_ms", "snapshot_ms", "freshness_ms"]
    .map((key) => timeOf(name, key))
    .reduce<number | null>((sum, one) => (sum === null || one === null ? null : sum + one), 0);
  result(
    !split
      ? judged
      : `${judged} narrowed_subscribed_ms=${ms(subscribed)} ` +
          `narrowed_subscribed_after_delivery_arrived_ms=${ms(minus(subscribed, timeOf("evals.delivery", "arrived_ms")))} ` +
          `narrowed_subscribed_after_delivery_parsed_ms=${ms(minus(subscribed, timeOf("evals.delivery", "parsed_ms")))} ` +
          `narrowed_server_began_ms=${ms(timeOf(name, "server_began_ms"))} ` +
          `narrowed_server_ms=${ms(minus(timeOf(name, "server_ended_ms"), timeOf(name, "server_began_ms")))} ` +
          `narrowed_server_snapshot_ms=${ms(serverSnapshot)} narrowed_fold_ms=${ms(timeOf(name, "fold_ms"))} ` +
          `narrowed_encode_ms=${ms(timeOf(name, "encode_ms"))} narrowed_arrived_ms=${ms(timeOf(name, "arrived_ms"))} ` +
          `narrowed_parsed_ms=${ms(timeOf(name, "parsed_ms"))}`
  );
}

/** Fails a judged moment that is unread or over its budget. */
function judgedBound(
  what: string,
  raw: number | null,
  moment: { judged: number } | null,
  budget: number
) {
  if (moment === null)
    fail(
      `tab paint case=${which} rate=${rate}: ${what} has no judged time (raw ${ms(raw)} ms; ${machine})`
    );
  else if (moment.judged > budget)
    fail(
      `tab paint case=${which} rate=${rate}: ${what} took ${ms(moment.judged)} ms judged (${ms(raw)} ms raw), over its ${budget} ms`
    );
}
judgedBound("the first paint", firstPaint, firstPaintJudged, BUDGET_PAINT_MS);
judgedBound("every section's drawing", everySection, everySectionJudged, BUDGET_EVERY_SECTION_MS);
if (chosen.narrowed)
  judgedBound("the narrowed cards' drawing", narrowedAt, narrowedJudged, BUDGET_NARROWED_MS);
for (const one of boxes) {
  if (one.changed === null)
    fail(
      `tab paint case=${which} rate=${rate}: the box of section ${one.id} has no size read when it was first shown and when its section was drawn`
    );
  else if (one.changed)
    fail(
      `tab paint case=${which} rate=${rate}: the box of section ${one.id} changed its size when its section drew (${sizeText(one.shown)} shown, ${sizeText(one.drawn)} drawn, or another between)`
    );
}
if (
  screen.width !== VIEWPORT.width ||
  screen.height !== VIEWPORT.height ||
  screen.scale !== 1 ||
  screen.scrollX !== 0 ||
  screen.scrollY !== 0
)
  fail(
    `tab paint case=${which} rate=${rate}: the page was ${screen.width}x${screen.height} at device scale ${screen.scale}, scrolled to ${screen.scrollX},${screen.scrollY}, not ${VIEWPORT.width}x${VIEWPORT.height} at device scale 1 at its top`
  );
if (gzip > BUDGET_GZIP || gzip === 0) {
  fail(
    `tab paint case=${which} rate=${rate}: the first payload is ${gzip} bytes compressed, over its ${BUDGET_GZIP} (or none was heard)`
  );
}
// The author case's 150 ms judges the first paint's own reads, the narrowed read's set apart (the
// ruling's item 4); every other case's, every statement of the paint. The bound holds at the stated
// rate; at ten times the database time counts inside the judged 0.4 s, and a time over 150 or 60 ms
// is a RECORD line.
const judgedDbMs = chosen.narrowed ? firstPaintReadsDbMs : dbMs;
if (judgedDbMs === null || judgedDbMs > chosen.budgetDbMs) {
  (rate === "ten" ? recordLine : fail)(
    `tab paint case=${which} rate=${rate}: the first paint's reads took ${ms(judgedDbMs)} ms of database time, over its ${chosen.budgetDbMs} ms`
  );
}
if (
  (payload.evals?.frames ?? 0) < 4 ||
  (payload.items?.frames ?? 0) < 2 ||
  (daemonRead && (payload["legion-read"]?.frames ?? 0) < 1)
) {
  fail(
    `tab paint case=${which} rate=${rate}: the first payload holds ${payload.evals?.frames ?? 0} evals, ${payload.items?.frames ?? 0} items ` +
      `and ${payload["legion-read"]?.frames ?? 0} legion-read snapshots, not the four, the two and ${daemonRead ? "the one" : "none"}`
  );
}
if (failedSections.length > 0)
  fail(`tab paint case=${which} rate=${rate}: sections refused: ${failedSections.join(", ")}`);
if (timelineState !== "closed")
  fail(
    `tab paint case=${which} rate=${rate}: the merge timeline is ${timelineState}, not closed, on the first paint`
  );

await browser.close();
await db.close();
process.exit(failed() > 0 ? 1 : 0);
