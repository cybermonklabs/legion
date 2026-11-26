// The Evals tab's first paint as its first reader after a restart (dispatch://INFRA-2918, T27,
// checks k4 and k19): run by evalbench/run.sh's `tab` command, once per case, each on a server
// the script has just started. One Chromium page of a signed-in reader opens the tab and is timed
// from its navigation to each first-paint section's `data-state="painted"` (lib.ts's INSTRUMENT);
// its live frames give the first payload, the four evals snapshots and the CI section's two items
// snapshots, compressed; and pg_stat_statements, reset before the navigation, gives the paint's
// database time, less the statements a quiet window before it shows the server's background
// loops run (measure paint's catalogue).
//
//   bun paint.ts --url <server> --database <url> --project INFRA --case <84d-compare|28d|84d-author> \
//     [--rate stated|ten] [--quiet 30] [--catalog 65] [--timeout 120] [--author <login>] \
//     [--sections cards,legion,ci,deploys,logged] [--login alice]
//
// The cases: 84d-compare, 84 days with the compare range the header defaults to (the 84 days
// before); 28d, the default 28 days with the compare range off; 84d-author, 84 days with a compare
// range from an address that sets the author facet, whose first paint is the unnarrowed one
// (plan D7a) and whose narrowed cards draw after it, the delay this case records beside the paint.
// Budgets (spec R4, R17): every section painted within 400 ms of the navigation, the first payload
// at most 300 KB compressed, and at most 150 ms of database time at 84 days with a compare range
// and 60 ms at 28 days with none. Each line is a RESULT, each missed budget a FAIL, and the step
// exits 1 when any FAIL was printed.
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
  readerContext,
  readInstrument,
  resetStatements,
  result,
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

const BUDGET_PAINT_MS = 400;
const BUDGET_GZIP = 300 * 1000;
const cases: Record<string, { query: string; budgetDbMs: number; narrowed: boolean }> = {
  "84d-compare": { query: "window=84d", budgetDbMs: 150, narrowed: false },
  "28d": { query: "compare=off", budgetDbMs: 60, narrowed: false },
  "84d-author": { query: "window=84d", budgetDbMs: 150, narrowed: true },
};
const chosen = cases[which];
if (chosen === undefined) throw new Error(`--case is one of ${Object.keys(cases).join(", ")}, not ${which}`);

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
  if (author === undefined) throw new Error("the window holds no pull request, so no author facet can be set");
  query += `&author=${encodeURIComponent(author)}`;
}
const path = `/projects/${encodeURIComponent(project)}/evals?${query}`;

const browser = await launchChromium();
mark(`paint case=${which}: Chromium ${browser.version()}, ${base}${path}`);
const context = await readerContext(browser, base, login);
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

// The paint: pg_stat_statements reset, the navigation, and every first-paint section painted.
await resetStatements(db);
const called = Date.now();
await page.goto(`${base}${path}`, { waitUntil: "commit", timeout: timeoutMs });
const expected = JSON.stringify(sections);
const painted = (want: string) =>
  `(() => { const r = window.__evalbench; if (!r) return false;
     const want = ${want};
     if (!want.every((id) => r.sections[id] && r.sections[id].painted !== undefined)) return false;
     const lists = Object.values(r.jobLists);
     return !want.includes("ci") || (lists.length >= 2 && lists.every((one) => one.painted !== undefined)); })()`;
let paintedAt = 0;
try {
  await page.waitForFunction(painted(expected), undefined, { polling: 25, timeout: timeoutMs });
  paintedAt = Date.now();
} catch (error) {
  fail(`tab paint case=${which} rate=${rate}: the sections did not all paint within ${timeoutMs} ms: ${String(error).slice(0, 200)}`);
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
    fail(`tab paint case=${which} rate=${rate}: the narrowed cards did not draw within ${timeoutMs} ms of the first paint: ${String(error).slice(0, 200)}`);
  }
}
const record: Instrumented = await readInstrument(page);
const timelineState = await page.evaluate<string | null>(
  `(() => { const e = document.querySelector('[data-section="timeline"]'); return e ? e.getAttribute("data-state") : null; })()`
);

// Times from the navigation's start (performance.timeOrigin) to each presentation.
const since = (at: number | undefined) => (at === undefined ? null : at - record.start);
const sectionTimes = sections.map((id) => [id, since(record.sections[id]?.painted)] as const);
const listTimes = Object.entries(record.jobLists).map(([id, one]) => [id, since(one.painted), one.failed] as const);
const times = [...sectionTimes.map(([, at]) => at), ...listTimes.map(([, at]) => at)];
const firstPaint = times.some((at) => at === null) ? null : Math.max(...(times as number[]));
const failedSections = Object.entries(record.sections).filter(([, one]) => one.state === "failed").map(([id]) => id);

// The first payload: the first snapshot of each of the tab's unnarrowed evals views and of the
// CI section's two items views (pr-ci's job lists), heard before the paint was complete. A
// snapshot of another view heard by then (the narrowed delivery view, a declared eval's items
// view) is counted beside it, outside the budget.
const FACET_KEYS = ["parent_agent", "session", "issue", "priority", "component", "author", "rework", "deployed", "q"];
const classOf = (view: { kind: string; [field: string]: unknown }): string => {
  const viewQuery = new URLSearchParams(typeof view.query === "string" ? view.query : "");
  if (view.kind === "evals") return FACET_KEYS.some((key) => viewQuery.has(key)) ? "narrowed" : "evals";
  if (view.kind === "items") return view.slug === "pr-ci" ? "items" : "other-items";
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
const budgeted = [payload.evals, payload.items];
const gzip = budgeted.reduce((n, one) => n + (one?.gzip ?? 0), 0);
const raw = budgeted.reduce((n, one) => n + (one?.raw ?? 0), 0);

const own = (rows: Counted[]) => rows.filter((one) => !catalog.has(one.queryid));
const total = (rows: Counted[]) => rows.reduce((n, one) => n + one.ms, 0);
const dbMs = total(own(atPaint));
const apiRequests = requests.filter((one) => paintedAt > 0 && one.at <= paintedAt && new URL(one.url).pathname.startsWith("/api/"));
const detailReads = apiRequests.filter((one) =>
  new RegExp(`^/api/v1/projects/${project}/evals/[^/]+$`).test(new URL(one.url).pathname)
);
result(
  `tab paint case=${which} rate=${rate} path=${path} first_paint_ms=${ms(firstPaint)} budget_ms=${BUDGET_PAINT_MS} ` +
    `navigation_called_before_start_ms=${ms(record.start - called)} ` +
    `sections=[${sectionTimes.map(([id, at]) => `${id}=${ms(at)}`).join(" ")}] ` +
    `job_lists=[${listTimes.map(([id, at, bad]) => `${id}=${ms(at)}${bad ? "(failed)" : ""}`).join(" ")}] timeline=${timelineState}`
);
result(
  `tab paint case=${which} rate=${rate} payload_gzip_bytes=${gzip} payload_raw_bytes=${raw} budget_gzip_bytes=${BUDGET_GZIP} ` +
    `payload_frames=[${Object.entries(payload).map(([kind, one]) => `${kind}:${one.frames}:${one.gzip}`).join(" ")}] ` +
    `live_frames_before_paint=${live.frames.filter((frame) => paintedAt > 0 && frame.heard <= paintedAt).length}`
);
result(
  `tab paint case=${which} rate=${rate} db_ms=${ms(dbMs)} budget_db_ms=${chosen.budgetDbMs} db_ms_everything_in_flight=${ms(total(atPaint))} ` +
    `statements=${own(atPaint).length} catalogued_statements_set_aside=${atPaint.length - own(atPaint).length} ` +
    `api_requests=${apiRequests.length} eval_detail_reads=${detailReads.length}`
);
for (const one of own(atPaint).sort((a, b) => b.ms - a.ms).slice(0, 12)) {
  mark(`paint case=${which} statement ms=${ms(one.ms)} calls=${one.calls} ${one.query.replace(/\s+/g, " ").slice(0, 220)}`);
}
if (chosen.narrowed) {
  const narrowedAt = since(record.narrowing.narrowed?.painted);
  const cardsAt = since(record.sections.cards?.painted);
  const after = (from: number | null) => (narrowedAt === null || from === null ? null : narrowedAt - from);
  // A negative delay is a narrowed view that landed before that paint: the cards then drew their
  // narrowed values before the last section of the first paint.
  result(
    `tab paint case=${which} rate=${rate} narrowed_cards_ms=${ms(narrowedAt)} narrowed_after_first_paint_ms=${ms(after(firstPaint))} ` +
      `narrowed_after_cards_painted_ms=${ms(after(cardsAt))} loading_mark_seen=${record.narrowing.loading !== undefined} ` +
      `db_ms_through_narrowed=${ms(atNarrowed === null ? null : total(own(atNarrowed)))} ` +
      "(no budget of its own: the narrowed cards draw after the first paint, plan D7a; reported to the architect)"
  );
}

if (firstPaint === null || firstPaint > BUDGET_PAINT_MS) {
  fail(`tab paint case=${which} rate=${rate}: the first paint took ${ms(firstPaint)} ms, over its ${BUDGET_PAINT_MS} ms`);
}
if (gzip > BUDGET_GZIP || gzip === 0) {
  fail(`tab paint case=${which} rate=${rate}: the first payload is ${gzip} bytes compressed, over its ${BUDGET_GZIP} (or none was heard)`);
}
if (dbMs > chosen.budgetDbMs) {
  fail(`tab paint case=${which} rate=${rate}: the first paint took ${ms(dbMs)} ms of database time, over its ${chosen.budgetDbMs} ms`);
}
if ((payload.evals?.frames ?? 0) < 4 || (payload.items?.frames ?? 0) < 2) {
  fail(`tab paint case=${which} rate=${rate}: the first payload holds ${payload.evals?.frames ?? 0} evals and ${payload.items?.frames ?? 0} items snapshots, not the four and the two`);
}
if (failedSections.length > 0) fail(`tab paint case=${which} rate=${rate}: sections refused: ${failedSections.join(", ")}`);
if (timelineState !== "closed") fail(`tab paint case=${which} rate=${rate}: the merge timeline is ${timelineState}, not closed, on the first paint`);

await browser.close();
await db.close();
process.exit(failed() > 0 ? 1 : 0);
