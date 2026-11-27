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
//   bun paint.ts --url <server> --database <url> --project INFRA --case <84d-compare|28d|84d-author> \
//     [--warmed <file> --restarted <file>] [--rate stated|ten] [--quiet 30] [--catalog 65] \
//     [--timeout 120] [--author <login>] [--sections cards,legion,ci,deploys,logged] [--login alice]
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
// this case records beside the paint with the database time the narrowed read adds.
// Budgets (spec R4, R17): every section painted within 400 ms of the navigation, the first payload
// at most 300 KB compressed, at most 150 ms of database time at 84 days with a compare range and
// 60 ms at 28 days with none, and the narrowed cards drawn within 2 s of the navigation. A bundle
// load alone over 400 ms fails nothing: its line goes to the architect with its numbers (ledger
// item 62). Each line is a RESULT, each missed budget a FAIL, and the step exits 1 when any FAIL
// was printed.
import { existsSync, writeFileSync } from "node:fs";

import {
  type Counted,
  counted,
  database,
  fail,
  failed,
  flags,
  gzipBytes,
  type Instrumented,
  LiveLog,
  launchChromium,
  mark,
  ms,
  need,
  numberOf,
  OWN,
  type Page,
  readerContext,
  readInstrument,
  resetStatements,
  result,
  signIn,
  sleep,
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
// section painted, and pg_stat_statements read then.
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
let narrowedAt: number | null = null;
if (chosen.narrowed) {
  narrowedAt = since(record.narrowing.narrowed?.painted);
  const cardsAt = since(record.sections.cards?.painted);
  const after = (from: number | null) =>
    narrowedAt === null || from === null ? null : narrowedAt - from;
  const throughNarrowed = atNarrowed === null ? null : total(own(atNarrowed));
  // A negative delay is a narrowed view that landed before that paint: the cards then drew their
  // narrowed values before the last section of the first paint.
  result(
    `tab paint case=${which} rate=${rate} narrowed_cards_ms=${ms(narrowedAt)} budget_narrowed_ms=${BUDGET_NARROWED_MS} ` +
      `narrowed_after_first_paint_ms=${ms(after(firstPaint))} narrowed_after_cards_painted_ms=${ms(after(cardsAt))} ` +
      `loading_mark_seen=${record.narrowing.loading !== undefined} db_ms_through_narrowed=${ms(throughNarrowed)} ` +
      `db_ms_after_first_paint_through_narrowed=${ms(throughNarrowed === null ? null : throughNarrowed - dbMs)}`
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
