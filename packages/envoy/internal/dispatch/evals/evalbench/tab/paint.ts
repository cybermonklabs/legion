// The Evals tab's first paint in a browser that has loaded the app's JavaScript, against a server
// just restarted (dispatch://INFRA-2918, T27, checks k4 and k19, measured as the ruling of ledger
// item 62 says): run by evalbench/run.sh's `tab` command, once per case. A signed-in reader's
// Chromium context first opens the tab once on the server the script has just started, which loads
// the app's JavaScript into the context's cache: that navigation's time to every section painted
// (the cold-browser time), the bundle's size and its load time are recorded. The script then
// restarts the server (the handshake below), and a new page of the same context, its JavaScript
// cached, is timed from its navigation to each first-paint section's `data-state="painted"`
// (lib.ts's INSTRUMENT); its live frames give the first payload, the four evals snapshots, the CI
// section's two items snapshots and the header's legion-read snapshot, compressed; and
// pg_stat_statements, reset before the navigation and read once every section has painted, gives
// the paint's database time, less the statements a quiet window before it shows the server's
// background loops run (measure paint's catalogue).
//
// The split of the first paint (item 1 of the architect's ruling on T33's misses,
// dispatch://INFRA-2918/comment/0d0aa027-83af-4a1b-9042-69f5d161ec32), one
// `RESULT tab paint split` line a case: for each view of the first payload (`evals.<kind>`,
// `items.<sort>`, `legion-read`), each time from the navigation, when the page sent its subscribe,
// when the server's recompute of it began and ended and how long its steps took (the change
// position, the read, within it an evals view's snapshot of reads, their fold and the delivery
// freshness, and the encode of the body), how long the socket took to encode its snapshot frame
// and to compress and write it and when the write ended, when the frame arrived and when its
// parse ended, and its size; and for each section and CI job list, when its drawing committed and
// when the browser presented it. The server's times are its debug lines (lib.ts's LOGGED) in the
// log --server-log names; the page's are the instrument's and the app's `dispatch:` marks. Without
// --server-log the server's times are none.
//
//   bun paint.ts --url <server> --database <url> --project INFRA --case <84d-compare|28d|84d-author> \
//     [--warmed <file> --restarted <file>] [--server-log <file>] [--rate stated|ten] [--quiet 30] \
//     [--catalog 65] [--timeout 120] [--author <login>] [--sections cards,legion,ci,deploys,logged] \
//     [--login alice]
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
// this case records beside the paint with the database time the narrowed read adds. Its narrowed
// line also gives, from the navigation, when the page subscribed the narrowed view (and how long
// after the unnarrowed delivery view's snapshot arrived and was parsed), the server's time and
// database time for its read, and when its snapshot arrived and was parsed (the ruling's item 3);
// and its database line splits db_ms into the first paint's reads and the narrowed read (item 4): the
// narrowed read's database time is its own, read again alone through the list route, which
// answers the narrowed view's body through the same read (readProjectEvalList), once the narrowed
// cards have drawn, and the first paint's reads are db_ms less that time where the server's log
// shows the narrowed read ran before the paint was read (`narrowed_read_in_paint`: whole, part or
// none).
// Budgets (spec R4, R17): every section painted within 400 ms of the navigation, the first payload
// at most 300 KB compressed, at most 150 ms of database time at 84 days with a compare range and
// 60 ms at 28 days with none, and the narrowed cards drawn within 2 s of the navigation. A bundle
// load alone over 400 ms fails nothing: its line goes to the architect with its numbers (ledger
// item 62). Each line is a RESULT, each missed budget a FAIL, and the step exits 1 when any FAIL
// was printed; the split's and the narrowed read's fields judge nothing.
import { existsSync, writeFileSync } from "node:fs";

import {
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
const serverLog = given.get("server-log");
if ((warmedFile === undefined) !== (restartedFile === undefined))
  throw new Error(
    "--warmed and --restarted name the handshake's two files, together or not at all"
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

const browser = await launchChromium();
mark(`paint case=${which}: Chromium ${browser.version()}, ${base}${path}`);
const context = await readerContext(browser, base, login);

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
// section painted, and pg_stat_statements read then; the server's log is read from here on.
const logFrom = serverLog === undefined ? 0 : logSize(serverLog);
await resetStatements(db);
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
const record: Instrumented = await readInstrument(page);
const warmBundle = await bundleOf(page);
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

// The narrowed read alone (the ruling's item 4), once the narrowed cards have drawn: the list route's read
// of the narrowed delivery view's query and window, which answers the view's body through the same
// read (readProjectEvalList), with pg_stat_statements reset before it, so the statements it counts
// are the read's own; and whether the server's log shows the hub's narrowed read running before
// the paint's database time was read (whole, part or none of it; unknown without its line).
const narrowed = snapshots.find((one) => one.name === "narrowed.delivery");
let narrowedReadDbMs: number | null = null;
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
    const alone = await counted(db);
    if (answer.status() === 200) narrowedReadDbMs = total(own(alone));
    else
      mark(
        `paint case=${which}: the narrowed read again answered ${answer.status()}, so its database time is none`
      );
  }
  const line = narrowed === undefined ? undefined : recomputeOf(narrowed.view);
  const began = line?.at("began_unix_us") ?? null;
  const ended = line?.at("ended_unix_us") ?? null;
  narrowedInPaint =
    began === null || ended === null || paintedAt === 0
      ? "unknown"
      : ended <= paintedAt
        ? "whole"
        : began >= paintedAt
          ? "none"
          : "part";
}
const firstPaintReadsDbMs =
  !chosen.narrowed || narrowedInPaint === "none"
    ? dbMs
    : narrowedReadDbMs === null || narrowedInPaint === "unknown"
      ? null
      : dbMs - narrowedReadDbMs;

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
    `first_paint_reads_db_ms=${ms(firstPaintReadsDbMs)} narrowed_read_db_ms=${ms(narrowedReadDbMs)} narrowed_read_in_paint=${narrowedInPaint} ` +
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

// The split (the ruling's item 1): each view of the first payload, then each section and CI job list.
const split = [
  `first_paint_ms=${ms(firstPaint)}`,
  `server_log=${serverLog !== undefined}`,
  `server_lines=${logged.length}`,
];
for (const one of snapshots) {
  if (one.name.startsWith("narrowed.")) continue;
  for (const [key, value] of timesByName.get(one.name) ?? [])
    split.push(`${one.name}.${key}=${ms(value)}`);
  split.push(
    `${one.name}.raw_bytes=${one.frame.text.length}`,
    `${one.name}.gzip_bytes=${gzipBytes(one.frame.text)}`
  );
}
for (const [id, presented] of sectionTimes) {
  const committed = fromStart(firstMark(record, `dispatch:evals-section:${id}`));
  split.push(
    `section.${id}.committed_ms=${ms(committed)}`,
    `section.${id}.presented_ms=${ms(presented)}`
  );
}
for (const [id, presented] of listTimes) {
  const committed = fromStart(firstMark(record, `dispatch:evals-job-list:${id}`));
  split.push(
    `job_list.${id}.committed_ms=${ms(committed)}`,
    `job_list.${id}.presented_ms=${ms(presented)}`
  );
}
result(`tab paint split case=${which} rate=${rate} ${split.join(" ")}`);

let narrowedAt: number | null = null;
if (chosen.narrowed) {
  narrowedAt = since(record.narrowing.narrowed?.painted);
  const cardsAt = since(record.sections.cards?.painted);
  const minus = (a: number | null, b: number | null) => (a === null || b === null ? null : a - b);
  const throughNarrowed = atNarrowed === null ? null : total(own(atNarrowed));
  const name = "narrowed.delivery";
  const subscribed = timeOf(name, "subscribed_ms");
  const serverDb = ["head_ms", "snapshot_ms", "freshness_ms"]
    .map((key) => timeOf(name, key))
    .reduce<number | null>((sum, one) => (sum === null || one === null ? null : sum + one), 0);
  // A negative delay is a narrowed view that landed before that paint: the cards then drew their
  // narrowed values before the last section of the first paint.
  result(
    `tab paint case=${which} rate=${rate} narrowed_cards_ms=${ms(narrowedAt)} budget_narrowed_ms=${BUDGET_NARROWED_MS} ` +
      `narrowed_after_first_paint_ms=${ms(minus(narrowedAt, firstPaint))} narrowed_after_cards_painted_ms=${ms(minus(narrowedAt, cardsAt))} ` +
      `loading_mark_seen=${record.narrowing.loading !== undefined} db_ms_through_narrowed=${ms(throughNarrowed)} ` +
      `db_ms_after_first_paint_through_narrowed=${ms(throughNarrowed === null ? null : throughNarrowed - dbMs)} ` +
      `narrowed_subscribed_ms=${ms(subscribed)} ` +
      `narrowed_subscribed_after_delivery_arrived_ms=${ms(minus(subscribed, timeOf("evals.delivery", "arrived_ms")))} ` +
      `narrowed_subscribed_after_delivery_parsed_ms=${ms(minus(subscribed, timeOf("evals.delivery", "parsed_ms")))} ` +
      `narrowed_server_began_ms=${ms(timeOf(name, "server_began_ms"))} ` +
      `narrowed_server_ms=${ms(minus(timeOf(name, "server_ended_ms"), timeOf(name, "server_began_ms")))} ` +
      `narrowed_server_db_ms=${ms(serverDb)} narrowed_fold_ms=${ms(timeOf(name, "fold_ms"))} ` +
      `narrowed_encode_ms=${ms(timeOf(name, "encode_ms"))} narrowed_arrived_ms=${ms(timeOf(name, "arrived_ms"))} ` +
      `narrowed_parsed_ms=${ms(timeOf(name, "parsed_ms"))} narrowed_read_db_ms=${ms(narrowedReadDbMs)} ` +
      `narrowed_read_in_paint=${narrowedInPaint}`
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
if (dbMs > chosen.budgetDbMs) {
  fail(
    `tab paint case=${which} rate=${rate}: the first paint took ${ms(dbMs)} ms of database time, over its ${chosen.budgetDbMs} ms`
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
