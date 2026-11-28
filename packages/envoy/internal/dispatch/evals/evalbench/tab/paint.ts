// The Evals tab's first paint in a browser that has loaded the app's JavaScript, against a server
// just restarted (dispatch://INFRA-2918, T27, checks k4 and k19, measured as the ruling of ledger
// item 62 says): run by evalbench/run.sh's `tab` command, once per case. A signed-in reader's
// Chromium context first opens the tab once on the server the script has just started, which loads
// the app's JavaScript into the context's cache: that navigation's time to every section painted
// (the cold-browser time), the bundle's size and its load time are recorded. The script then
// restarts the server (the handshake below), and a new page of the same context, its JavaScript
// cached, is timed from its navigation to each first-paint section's `data-state="painted"`
// (lib.ts's instrument); its live frames give the first payload, the four evals snapshots, the CI
// section's two items snapshots and the header's legion-read snapshot, compressed; and
// pg_stat_statements, reset before the navigation and read once every section has painted, gives
// the paint's database time, less the statements a quiet window before it shows the server's
// background loops run (measure paint's catalogue). The page's boot, from its navigation to its
// first subscribe, is one `RESULT tab paint boot` line a case, read from the browser's own
// navigation and resource timings once the paint is done, which cost the paint nothing: when the
// document's response ended, when the document was interactive and its DOMContentLoaded handlers
// had run, when each script the page loaded ended loading, the route's chunk (ProjectPage's) among
// them, and each API request it made before its first frame with when it began and ended.
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
// section and CI job list, when its drawing committed and when the browser presented it; and every
// long task of the page from the navigation to the paint, its start and its length. The server's
// times are its debug lines (lib.ts's LOGGED) in the log --server-log names.
//
// A split run also traces Chromium from the timed navigation to the paint (to the narrowed cards'
// drawing, in the author case), and its `RESULT tab paint main` line splits the page's main
// thread from the navigation to the paint, every task of it however short, where the long tasks
// show none under 50 ms; it tests whether the page's handling and drawing of each frame, each
// under 50 ms, holds the next frame's message event back (T33): how long the thread was busy and
// how long idle, by part (script, style and layout, paint, garbage collection, parse, other);
// every React commit's moment; for each frame of the first payload, in the order they arrived, how
// long the page's own handlers of it ran, how long the thread was busy, by part, between its write
// and its arrival, and, from its arrival to the next frame's (the paint's, for the last), the time
// busy by part and the commits; and every task of 2 ms or more, its start, its length and what it
// was (`frame:<view>`, the task whose handler read that frame; `react`, one that ended a commit;
// else what started it, lib.ts's mainThreadTasks).
//
//   bun paint.ts --url <server> --database <url> --project INFRA --case <84d-compare|28d|84d-author> \
//     [--warmed <file> --restarted <file>] [--split 1 --server-log <file>] [--rate stated|ten] \
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
// Budgets (spec R4, R17): every section painted within 400 ms of the navigation, the first payload
// at most 300 KB compressed, at most 150 ms of database time at 84 days with a compare range and
// 60 ms at 28 days with none, and the narrowed cards drawn within 2 s of the navigation. A bundle
// load alone over 400 ms fails nothing: its line goes to the architect with its numbers (ledger
// item 62). Each line is a RESULT, each missed budget a FAIL, and the step exits 1 when any FAIL
// was printed; the boot's, the split's and the narrowed read's fields judge nothing.
import { existsSync, writeFileSync } from "node:fs";

import {
  busyIn,
  busyWithin,
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
  readerContext,
  readInstrument,
  recomputes,
  resetStatements,
  result,
  serverLines,
  signIn,
  sleep,
  splitOf,
  TASK_PARTS,
  type Task,
  type TaskPart,
  TRACE_CATEGORIES,
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
if ((warmedFile === undefined) !== (restartedFile === undefined))
  throw new Error(
    "--warmed and --restarted name the handshake's two files, together or not at all"
  );
if (serverLog !== undefined && !split)
  throw new Error(
    "--server-log is read by a split run alone (--split 1), whose server logs at debug"
  );

const BUDGET_PAINT_MS = 400;
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

const expected = JSON.stringify(sections);
const painted = (want: string) =>
  `(() => { const r = window.__evalbench; if (!r) return false;
     const want = ${want};
     if (!want.every((id) => r.sections[id] && r.sections[id].painted !== undefined)) return false;
     const lists = Object.values(r.jobLists);
     return !want.includes("ci") || (lists.length >= 2 && lists.every((one) => one.painted !== undefined)); })()`;

/** The times of a page's first paint, each from the navigation's start (performance.timeOrigin)
 *  to its presentation: each section's, each CI job list's, and the paint's, the latest of them,
 *  null while one of them has not painted. */
function paintTimes(record: Instrumented) {
  const since = (at: number | undefined) => (at === undefined ? null : at - record.start);
  const sectionTimes = sections.map((id) => [id, since(record.sections[id]?.painted)] as const);
  const listTimes = Object.entries(record.jobLists).map(
    ([id, one]) => [id, since(one.painted), one.failed] as const
  );
  const times = [...sectionTimes.map(([, at]) => at), ...listTimes.map(([, at]) => at)];
  const firstPaint = times.some((at) => at === null) ? null : Math.max(...(times as number[]));
  return { firstPaint, listTimes, sectionTimes, since };
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
mark(
  `paint case=${which}: Chromium ${browser.version()}, ${base}${path}${split ? " (a split run)" : ""}`
);
const context = await readerContext(browser, base, login, split);

// The cold navigation: the tab opened once in the new context, which loads the app's JavaScript
// into the context's cache, its time to every section painted, the bundle's size and its load
// time recorded (ledger item 62).
const cold = await context.newPage();
await cold.goto(`${base}${path}`, { waitUntil: "commit", timeout: timeoutMs });
let coldPaint: number | null = null;
try {
  await cold.waitForFunction(painted(expected), undefined, { polling: 25, timeout: timeoutMs });
  coldPaint = paintTimes(await readInstrument(cold)).firstPaint;
} catch (error) {
  fail(
    `tab paint case=${which} rate=${rate}: the cold browser's sections did not all paint within ${timeoutMs} ms: ${String(error).slice(0, 200)}`
  );
}
const coldBundle = await bundleOf(cold);
await cold.close();
const bundleOver = coldBundle.loadedMs !== null && coldBundle.loadedMs > BUDGET_PAINT_MS;
result(
  `tab paint case=${which} rate=${rate} cold_first_paint_ms=${ms(coldPaint)} bundle_files=${coldBundle.files} ` +
    `bundle_bytes=${coldBundle.decoded} bundle_encoded_bytes=${coldBundle.encoded} bundle_transfer_bytes=${coldBundle.transfer} ` +
    `bundle_load_ms=${ms(coldBundle.loadedMs)} bundle_load_over_${BUDGET_PAINT_MS}_ms=${bundleOver} server_restarted_after=${warmedFile !== undefined}`
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

// The paint: pg_stat_statements reset, the navigation in the warm browser, every first-paint
// section painted, and pg_stat_statements read then; the server's log is read from here on. A
// split run traces Chromium from just before the navigation (TRACE_CATEGORIES) until the paint, or
// the narrowed cards' drawing in the author case, has been read.
const logFrom = serverLog === undefined ? 0 : logSize(serverLog);
await resetStatements(db);
if (split) await browser.startTracing(page, { categories: TRACE_CATEGORIES, screenshots: false });
const called = Date.now();
await page.goto(`${base}${path}`, { waitUntil: "commit", timeout: timeoutMs });
let paintedAt = 0;
try {
  await page.waitForFunction(painted(expected), undefined, { polling: 25, timeout: timeoutMs });
  paintedAt = Date.now();
} catch (error) {
  fail(
    `tab paint case=${which} rate=${rate}: the sections did not all paint within ${timeoutMs} ms: ${String(error).slice(0, 200)}`
  );
}
const paintReadAt = Date.now();
const atPaint = await counted(db);
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
      `tab paint case=${which} rate=${rate}: the narrowed cards did not draw within ${timeoutMs} ms of the first paint: ${String(error).slice(0, 200)}`
    );
  }
}
const trace = split ? await browser.stopTracing() : null;
const record: Instrumented = await readInstrument(page);
const warmBundle = await bundleOf(page);
const boot = await bootOf(page);
const timelineState = await page.evaluate<string | null>(
  `(() => { const e = document.querySelector('[data-section="timeline"]'); return e ? e.getAttribute("data-state") : null; })()`
);

const { firstPaint, listTimes, sectionTimes, since } = paintTimes(record);
const failedSections = Object.entries(record.sections)
  .filter(([, one]) => one.state === "failed")
  .map(([id]) => id);

// The first payload: the first snapshot of each of the tab's unnarrowed evals views, of the CI
// section's two items views (pr-ci's job lists) and of the header's legion-read view (plan D23),
// heard before the paint was complete. A snapshot of another view heard by then (the narrowed
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
result(
  `tab paint case=${which} rate=${rate} path=${path} first_paint_ms=${ms(firstPaint)} budget_ms=${BUDGET_PAINT_MS} ` +
    `navigation_called_before_start_ms=${ms(record.start - called)} ` +
    `sections=[${sectionTimes.map(([id, at]) => `${id}=${ms(at)}`).join(" ")}] ` +
    `job_lists=[${listTimes.map(([id, at, bad]) => `${id}=${ms(at)}${bad ? "(failed)" : ""}`).join(" ")}] timeline=${timelineState} ` +
    `warm_bundle_transfer_bytes=${warmBundle.transfer} warm_bundle_load_ms=${ms(warmBundle.loadedMs)} cold_first_paint_ms=${ms(coldPaint)}`
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
    `first_paint_ms=${ms(firstPaint)}`,
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
  for (const [id, presented] of sectionTimes) {
    const committed = fromStart(firstMark(record, `dispatch:evals-section:${id}`));
    fields.push(
      `section.${id}.committed_ms=${ms(committed)}`,
      `section.${id}.presented_ms=${ms(presented)}`
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

  // The page's main thread to the paint, from the trace (the header): each frame of the first
  // payload in the order it arrived, then every task of 2 ms or more, named by the frame whose
  // handler it ran or the commit it ended, else by what started it.
  const main = trace === null ? null : mainThreadTasks(JSON.parse(trace.toString("utf8")), path);
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

let narrowedAt: number | null = null;
if (chosen.narrowed) {
  narrowedAt = since(record.narrowing.narrowed?.painted);
  const cardsAt = since(record.sections.cards?.painted);
  const minus = (a: number | null, b: number | null) => (a === null || b === null ? null : a - b);
  const throughNarrowed = atNarrowed === null ? null : total(own(atNarrowed));
  const judged =
    `tab paint case=${which} rate=${rate} narrowed_cards_ms=${ms(narrowedAt)} budget_narrowed_ms=${BUDGET_NARROWED_MS} ` +
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

if (firstPaint === null || firstPaint > BUDGET_PAINT_MS) {
  fail(
    `tab paint case=${which} rate=${rate}: the first paint took ${ms(firstPaint)} ms, over its ${BUDGET_PAINT_MS} ms`
  );
}
if (gzip > BUDGET_GZIP || gzip === 0) {
  fail(
    `tab paint case=${which} rate=${rate}: the first payload is ${gzip} bytes compressed, over its ${BUDGET_GZIP} (or none was heard)`
  );
}
// The author case's 150 ms judges the first paint's own reads, the narrowed read's set apart (the
// ruling's item 4); every other case's, every statement of the paint.
const judgedDbMs = chosen.narrowed ? firstPaintReadsDbMs : dbMs;
if (judgedDbMs === null || judgedDbMs > chosen.budgetDbMs) {
  fail(
    `tab paint case=${which} rate=${rate}: the first paint's reads took ${ms(judgedDbMs)} ms of database time, over its ${chosen.budgetDbMs} ms`
  );
}
if (chosen.narrowed && (narrowedAt === null || narrowedAt > BUDGET_NARROWED_MS)) {
  fail(
    `tab paint case=${which} rate=${rate}: the narrowed cards drew ${ms(narrowedAt)} ms after the navigation, over their ${BUDGET_NARROWED_MS} ms`
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
