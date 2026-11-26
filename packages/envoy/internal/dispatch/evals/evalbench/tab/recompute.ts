// The Evals tab's recompute under INFRA-2916's write drive (dispatch://INFRA-2918, T27, checks k4
// and k19): run by evalbench/run.sh's `tab` command on a server it has started, the tab open on
// two pages of signed-in readers, 84 days with the header's default compare range, with the merge
// timeline open on each. The drive is INFRA-2916's phases A to D (its round-15 latency drive,
// lat_r16.py, the architect's rulings of 2026-11-22 08:20Z and 18:21Z), each write one commit of
// one of two families:
//
//   - a job write: the production job of a deploy run of the deploy repository, in the 28 days
//     before today and finished on the day it began, flipped from success to failure by SQL, as
//     the intake writes a job;
//   - a facts write: an INFRA issue finished in those days PATCHed back to in_progress by a Legion
//     session, through the issues route.
//
//   A  Poisson arrivals for --a-seconds at --a-rate writes a second (0: ten times the run rate,
//      the jobs of every run completed in the last 7 days a second, times ten), --a-facts-share
//      of them facts writes;
//   B  a kick, then --b-writes job writes each U(0, --b-max-ms) ms after a pass of the eval-days
//      refresher starts (its lease seen held in pg_locks, polled every --poll-ms ms) and
//      --b-controls at U(0, --b-control-ms) ms, interleaved;
//   C  a burst of --c-rate job writes a second for --c-seconds (fewer where the window holds
//      fewer runs);
//   D  --d-rounds rounds --d-gap s apart: the deploy source's staging end-to-end job renamed and
//      back in turn (the settings write, by SQL, since a server with no GitHub App refuses the
//      settings route), a job write U(0, 50) ms after the eval-days pass it wakes starts, then a
//      facts write.
//
// Each phase's writes are undone together, unmeasured, once it has settled. A write reaches a page
// at the first presentation of the page (lib.ts's INSTRUMENT: two animation frames after the
// frame arrived) whose views all show it: a view shows the k-th write of a family on a day once
// each of that family's count leaves of that day (a number under the day's entry that moved by one
// whole step at each of two writes on one day, read before the phases, lat_r16.py's calibration)
// stands k steps from its value at the phase's start, and each of its own leaves (a leaf under the
// written item's own entry, a run's row on the timeline) differs from it. The delay is that
// presentation less the write's commit (pg_xact_commit_timestamp, track_commit_timestamp on). A
// leaf that does not end the phase at its start value plus its writes' steps is dropped and named,
// and a view with no leaf for a write is not judged for it. Budget (spec R4, R17, R21): every
// judged write reaches each page within 2 s; a write that never reaches one fails.
//
//   bun recompute.ts --url <server> --database <url> --project INFRA [--rate stated|ten] \
//     [--token evalbench-token] [--phases A,B,C,D] [lat_r16.py's phase flags]
import { createHash } from "node:crypto";
import {
  type Browser,
  type BrowserContext,
  database,
  type Frame,
  fail,
  failed,
  flags,
  flatten,
  frameKey,
  LiveLog,
  launchChromium,
  leafAt,
  mark,
  ms,
  need,
  numberOf,
  OWN,
  type Page,
  pathOf,
  pathText,
  Replay,
  rank,
  readerContext,
  readInstrument,
  result,
  shown,
  type Step,
  sleep,
  type View,
  type ViewState,
} from "./lib";

const given = flags(process.argv.slice(2));
const base = need(given, "url").replace(/\/$/, "");
const project = given.get("project") ?? "INFRA";
const rate = given.get("rate") ?? "stated";
const token = given.get("token") ?? "evalbench-token";
const login = given.get("login") ?? "alice";
const phases = (given.get("phases") ?? "A,B,C,D").split(",");
const o = {
  aSeconds: numberOf(given, "a-seconds", 300),
  aRate: numberOf(given, "a-rate", 0),
  aFactsShare: numberOf(given, "a-facts-share", 0.2),
  bWrites: numberOf(given, "b-writes", 100),
  bControls: numberOf(given, "b-controls", 20),
  bMaxMs: numberOf(given, "b-max-ms", 50),
  bControlMs: numberOf(given, "b-control-ms", 5),
  bTimeoutMs: numberOf(given, "b-timeout-ms", 5000),
  cSeconds: numberOf(given, "c-seconds", 60),
  cRate: numberOf(given, "c-rate", 20),
  dRounds: numberOf(given, "d-rounds", 10),
  dGap: numberOf(given, "d-gap", 4),
  phaseSettle: numberOf(given, "phase-settle", 20),
  settle: numberOf(given, "settle", 15),
  quiet: numberOf(given, "quiet", 11),
  cap: numberOf(given, "cap", 75),
  pollMs: numberOf(given, "poll-ms", 2),
  seed: numberOf(given, "seed", 16),
  timeoutMs: numberOf(given, "timeout", 180) * 1000,
};
const BOUND_MS = 2000;
const sections = (given.get("sections") ?? "cards,legion,ci,deploys,logged").split(",");

const db = database(need(given, "database"), 2);
const poller = database(need(given, "database"), 1);
const writer = database(need(given, "database"), 1);

// ---------------------------------------------------------------------------------------------
// Small helpers

let seed = o.seed >>> 0;
/** The drive's one random source (mulberry32), seeded so a run's draws repeat. */
function random(): number {
  seed = (seed + 0x6d2b79f5) >>> 0;
  let t = seed;
  t = Math.imul(t ^ (t >>> 15), t | 1);
  t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
  return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
}

function shuffle<T>(xs: T[]): T[] {
  for (let i = xs.length - 1; i > 0; i--) {
    const j = Math.floor(random() * (i + 1));
    [xs[i], xs[j]] = [xs[j] as T, xs[i] as T];
  }
  return xs;
}

/** A string as an SQL literal. */
function lit(text: string): string {
  return `'${text.replaceAll("'", "''")}'`;
}

async function rows<T>(text: string): Promise<T[]> {
  return (await db.unsafe(`${OWN} ${text}`)) as T[];
}

/** A Legion session acting on an issue, as lat_r16.py's facts writes name one: a session id
 *  derived from the issue's key (a name-based UUID), titled as a merger of it. */
function legionActor(key: string) {
  const hash = createHash("sha1").update(`evalbench-tab-legion-${key}`).digest("hex");
  const id = `${hash.slice(0, 8)}-${hash.slice(8, 12)}-5${hash.slice(13, 16)}-a${hash.slice(17, 20)}-${hash.slice(20, 32)}`;
  return { kind: "session", id, origin: { session_title: `Legion merger · ${key}` } };
}

/** How long since either consumer's cursor last moved, in seconds: lat_r15.py's gate. */
async function cursorsAge(): Promise<number> {
  try {
    const [row] = await rows<{ age: number | null }>(
      `select extract(epoch from now() - greatest((select max(updated_at) from eval_days_cursor),
         (select max(updated_at) from issue_facts_cursor)))::float8 as age`
    );
    return Number(row?.age ?? 1e9);
  } catch {
    return 1e9;
  }
}

/** Waits until both consumers' cursors have stood still `quiet` s, at most `cap` s. */
async function gate(): Promise<number> {
  const started = Date.now();
  let age = await cursorsAge();
  while (age < o.quiet && Date.now() - started < o.cap * 1000) {
    await sleep(500);
    age = await cursorsAge();
  }
  return age;
}

/** After a write at `from`: at least `least` s, then the cursors still `quiet` s, at most `cap`. */
async function settle(from: number, least = o.settle, cap = o.cap): Promise<number> {
  for (;;) {
    await sleep(1000);
    const elapsed = (Date.now() - from) / 1000;
    if (elapsed >= cap || (elapsed >= least && (await cursorsAge()) >= o.quiet)) return elapsed;
  }
}

// ---------------------------------------------------------------------------------------------
// The pages

/** One page of the tab: its live socket's views replayed, and what a phase watches of them. */
interface TabPage {
  name: string;
  context: BrowserContext;
  page: Page;
  log: LiveLog;
  replay: Replay;
  watch: Watch | null;
}

/** What a phase reads of a page's views: for each view, the leaves of its writes, their values
 *  at the phase's start and now, and each frame that moved one. */
interface Watch {
  paths: Map<string, Map<string, Step[]>>;
  start: Map<string, Map<string, unknown>>;
  now: Map<string, Map<string, unknown>>;
  stream: Map<string, { key: string; heard: number; changed: [string, unknown][] }[]>;
}

/** The key one view of a page is known by across its subscriptions: its kind, window and query. */
function viewKey(view: View): string {
  return [view.kind, view.slug ?? "", JSON.stringify(view.window ?? null), view.query ?? ""].join(" ");
}

/** The page's subs by the key of the view each holds, the newest sub of a key last. */
function subsByView(tab: TabPage): Map<string, string> {
  const out = new Map<string, string>();
  for (const [sub, view] of tab.log.views) if (tab.replay.states.has(sub)) out.set(viewKey(view), sub);
  return out;
}

function readLeaves(state: ViewState | undefined, paths: Map<string, Step[]>): Map<string, unknown> {
  const out = new Map<string, unknown>();
  for (const [text, path] of paths) out.set(text, state === undefined ? undefined : leafAt(state, path));
  return out;
}

function onFrame(tab: TabPage, frame: Frame): void {
  const moved = tab.replay.apply(frame);
  const watch = tab.watch;
  if (moved === null || watch === null) return;
  const view = tab.log.views.get(moved.sub);
  if (view === undefined) return;
  const key = viewKey(view);
  const paths = watch.paths.get(key);
  if (paths === undefined) return;
  const now = watch.now.get(key) ?? new Map<string, unknown>();
  watch.now.set(key, now);
  const values = readLeaves(tab.replay.states.get(moved.sub), paths);
  const changed: [string, unknown][] = [];
  for (const [text, value] of values) {
    if (now.get(text) !== value) {
      changed.push([text, value]);
      now.set(text, value);
    }
  }
  if (changed.length > 0) {
    const stream = watch.stream.get(key) ?? [];
    watch.stream.set(key, stream);
    stream.push({ key: frameKey(frame.socket, frame.index), heard: frame.heard, changed });
  }
}

/** Every leaf of every view of the page, by view. */
function snapshot(tab: TabPage): Map<string, Map<string, unknown>> {
  const out = new Map<string, Map<string, unknown>>();
  for (const [key, sub] of subsByView(tab)) {
    const state = tab.replay.states.get(sub);
    if (state !== undefined) out.set(key, flatten(state));
  }
  return out;
}

const paintedCheck = `(() => { const r = window.__evalbench; if (!r) return false;
  const want = ${JSON.stringify(sections)};
  if (!want.every((id) => r.sections[id] && r.sections[id].painted !== undefined)) return false;
  const lists = Object.values(r.jobLists);
  return !want.includes("ci") || (lists.length >= 2 && lists.every((one) => one.painted !== undefined)); })()`;

async function openTab(name: string, browser: Browser): Promise<TabPage> {
  const context = await readerContext(browser, base, login);
  const page = await context.newPage();
  const log = new LiveLog(false);
  const tab: TabPage = { name, context, page, log, replay: new Replay(log), watch: null };
  log.onFrame = (frame) => onFrame(tab, frame);
  log.listen(page);
  await page.goto(`${base}/projects/${encodeURIComponent(project)}/evals?window=84d`, {
    waitUntil: "commit",
    timeout: o.timeoutMs,
  });
  await page.waitForFunction(paintedCheck, undefined, { polling: 50, timeout: o.timeoutMs });
  await page.click('[data-section="timeline"] button[aria-expanded="false"]', { timeout: o.timeoutMs });
  await page.waitForFunction(
    `(() => { const t = window.__evalbench.sections.timeline; return t !== undefined && t.painted !== undefined; })()`,
    undefined,
    { polling: 50, timeout: o.timeoutMs }
  );
  return tab;
}

// ---------------------------------------------------------------------------------------------
// The writes

/** One write of the drive: what it runs and when, and, once run, what it answered. */
interface Write {
  id: string;
  family: "job" | "facts" | "settings";
  item: string;
  day: string | null;
  sql?: string;
  http?: { method: string; path: string; body: unknown };
  xidSql?: string;
  atMs?: number;
  waitPass?: boolean;
  delayMs?: number;
  timeoutMs?: number;
  undoStatus?: string;
  // the answer
  sent?: number;
  returned?: number;
  xid?: string | null;
  status?: string | number;
  error?: string;
  keyed?: number | null;
  commit?: number;
}

const passStarts: number[] = [];
let polling = false;

/** Polls pg_locks for the eval-days refresher's lease (evals.RefresherLease, a duty lease: the
 *  advisory lock of classid 0x4C4F4F50 and objid hashtext('eval-days'), objsubid 2), noting each
 *  pass's start at the query_start of the holder's take, as lat-pass.r16.ts does. */
async function pollPasses(): Promise<void> {
  const [ids] = (await poller.unsafe(
    `${OWN} select hashtext('eval-days')::oid::bigint::text as e, (select oid from pg_database where datname = current_database())::bigint::text as d`
  )) as { e: string; d: string }[];
  if (ids === undefined) return;
  const query = `${OWN} select l.pid, (extract(epoch from a.query_start) * 1000)::float8 as qs,
    (a.query like '%pg_try_advisory_lock%') as taking from pg_locks l join pg_stat_activity a on a.pid = l.pid
    where l.locktype = 'advisory' and l.granted and l.objsubid = 2 and l.database = ${ids.d}
      and l.classid = 1280266064 and l.objid = ${ids.e}`;
  let held: { pid: number; qs: number } | null = null;
  while (polling) {
    const before = Date.now();
    try {
      const [row] = (await poller.unsafe(query)) as { pid: number; qs: number; taking: boolean }[];
      if (row !== undefined) {
        const fresh = row.taking && (held === null || held.pid !== Number(row.pid) || Math.abs(held.qs - row.qs) > 0.001);
        if (held === null || fresh) passStarts.push(row.taking ? Number(row.qs) : before);
        held = { pid: Number(row.pid), qs: Number(row.qs) };
      } else held = null;
    } catch {
      await sleep(50);
    }
    const wait = o.pollMs - (Date.now() - before);
    if (wait > 0) await sleep(wait);
  }
}

/** Runs one write, at its time and after its pass where it names them, and records its answer. */
async function runWrite(write: Write, since: number): Promise<void> {
  if (write.atMs !== undefined) {
    const wait = write.atMs - Date.now();
    if (wait > 0) await sleep(wait);
  }
  write.keyed = null;
  if (write.waitPass) {
    const deadline = Date.now() + (write.timeoutMs ?? 5000);
    while (Date.now() < deadline) {
      const start = passStarts.find((at) => at >= since);
      if (start !== undefined) {
        write.keyed = start;
        break;
      }
      await sleep(1);
    }
    if (write.keyed !== null && write.delayMs !== undefined) {
      const wait = write.keyed + write.delayMs - Date.now();
      if (wait > 0) await sleep(wait);
    }
  }
  write.sent = Date.now();
  try {
    if (write.sql !== undefined) {
      const statement = write.sql;
      write.xid = await writer.begin(async (tx) => {
        await tx.unsafe(statement);
        const [row] = (await tx.unsafe(`${OWN} select pg_current_xact_id()::text as x`)) as { x: string }[];
        return row?.x ?? null;
      });
      write.returned = Date.now();
      write.status = "sql";
    } else if (write.http !== undefined) {
      const answer = await fetch(base + write.http.path, {
        method: write.http.method,
        headers: {
          Authorization: `Bearer ${token}`,
          "Content-Type": "application/json",
          "Idempotency-Key": crypto.randomUUID(),
        },
        body: JSON.stringify(write.http.body),
      });
      await answer.text();
      write.returned = Date.now();
      write.status = answer.status;
      if (write.xidSql !== undefined) {
        const [row] = (await writer.unsafe(write.xidSql)) as { x: string }[];
        write.xid = row?.x ?? null;
      }
    }
  } catch (error) {
    write.returned ??= Date.now();
    write.error = String(error).slice(0, 300);
  }
}

/** Each write's commit time, from its transaction id where track_commit_timestamp is on, else its
 *  answer's. */
async function commitTimes(writes: Write[], tracked: boolean): Promise<void> {
  const xids = [...new Set(writes.map((one) => one.xid).filter((x): x is string => typeof x === "string" && /^\d+$/.test(x)))];
  const times = new Map<string, number>();
  if (tracked) {
    for (let at = 0; at < xids.length; at += 500) {
      const chunk = xids.slice(at, at + 500);
      const got = await rows<{ x: string; ms: number | null }>(
        `select x, (extract(epoch from pg_xact_commit_timestamp(x::xid8::xid)) * 1000)::float8 as ms from unnest('{${chunk.join(",")}}'::text[]) x`
      );
      for (const row of got) if (row.ms !== null) times.set(row.x, Number(row.ms));
    }
  }
  for (const write of writes) write.commit = (write.xid ? times.get(write.xid) : undefined) ?? write.returned;
}

const ok = (write: Write) => write.error === undefined && (write.status === "sql" || write.status === 200 || write.status === 204);

// ---------------------------------------------------------------------------------------------
// Setup

const tracked =
  (await rows<{ setting: string }>("select current_setting('track_commit_timestamp') as setting"))[0]?.setting === "on";
const [source] = await rows<{ repo: string; prod: string; e2e: string | null; apply: string | null; id: string }>(
  `select coalesce(s.repo, p.default_repo) as repo, s.production_job_names[1] as prod, s.staging_e2e_job_name as e2e,
     s.staging_apply_job_name as apply, s.id::text as id
   from project_deploy_sources s join projects p on p.key = s.project_key
   where s.project_key = ${lit(project)} and s.kind = 'github_workflow' order by s.position limit 1`
);
if (source === undefined) throw new Error(`${project} has no github_workflow deploy source`);
const jobsSeen = await rows<{ name: string }>(
  `select j.name from delivery_run_jobs j join delivery_runs r using (repo, run_id)
   where r.kind = 'deploy' and r.repo = ${lit(source.repo)} group by 1 order by count(*) desc`
);
const otherE2E = ["migrate / check", ...jobsSeen.map((one) => one.name)].find(
  (name) => name !== source.prod && name !== source.e2e && name !== source.apply
);
const today = new Date(new Date().toISOString().slice(0, 10));
const day = (at: Date) => at.toISOString().slice(0, 10);
const from28 = day(new Date(today.getTime() - 28 * 86400000));
const runRate =
  Number(
    (
      await rows<{ n: number }>(
        "select count(*)::float8 as n from delivery_run_jobs where completed_at >= now() - interval '7 days'"
      )
    )[0]?.n ?? 0
  ) /
  (7 * 86400);

const jobSql = (run: string, failure: boolean) =>
  `${OWN} update delivery_run_jobs set conclusion = '${failure ? "failure" : "success"}' where repo = ${lit(source.repo)} and run_id = ${run} and name = ${lit(source.prod)}`;
const settingsSql = (name: string | null) =>
  `${OWN} update project_deploy_sources set staging_e2e_job_name = ${name === null ? "null" : lit(name)} where id = ${lit(source.id)}`;
const factsWrite = (key: string, status: string) => ({
  http: { method: "PATCH", path: `/api/v1/issues/${encodeURIComponent(key)}`, body: { status, actor: legionActor(key) } },
  xidSql: `${OWN} select xmin::text as x from issues where key = ${lit(key)}`,
});

type Item = { item: string; day: string; status?: string };
const jobItems: Item[] = (
  await rows<{ run: string; day: string }>(
    `select r.run_id::text as run, to_char((r.created_at at time zone 'UTC')::date, 'YYYY-MM-DD') as day
     from delivery_runs r join delivery_run_jobs j using (repo, run_id)
     where r.repo = ${lit(source.repo)} and r.kind = 'deploy' and j.name = ${lit(source.prod)} and j.conclusion = 'success'
       and r.created_at >= '${from28}' and r.created_at < '${day(today)}'
       and (r.completed_at at time zone 'UTC')::date = (r.created_at at time zone 'UTC')::date
       and (j.completed_at at time zone 'UTC')::date = (r.created_at at time zone 'UTC')::date
     order by r.created_at`
  )
).map((one) => ({ item: one.run, day: one.day }));
const factsItems: Item[] = (
  await rows<{ key: string; day: string; status: string }>(
    `select f.issue_key as key, to_char((f.closed_at at time zone 'UTC')::date, 'YYYY-MM-DD') as day, i.status
     from issue_facts f join issues i on i.key = f.issue_key
     where f.project_key = ${lit(project)} and i.status = 'done' and f.closed_at >= '${from28}' and f.closed_at < '${day(today)}'
     order by f.closed_at`
  )
).map((one) => ({ item: one.key, day: one.day, status: one.status }));
result(
  `tab recompute setup rate=${rate} project=${project} deploy_repo=${source.repo} production_job=${JSON.stringify(source.prod)} ` +
    `staging_e2e=${JSON.stringify(source.e2e)} renamed_to=${JSON.stringify(otherE2E)} job_items=${jobItems.length} facts_items=${factsItems.length} ` +
    `run_rate_per_s=${runRate.toFixed(4)} track_commit_timestamp=${tracked} window_from=${from28}`
);

const browser = await launchChromium();
mark(`recompute: Chromium ${browser.version()}`);
const tabs = [await openTab("page1", browser), await openTab("page2", browser)];
for (const tab of tabs) {
  result(`tab recompute page=${tab.name} views=[${[...subsByView(tab).keys()].map((key) => key.split(" ")[0]).join(" ")}]`);
}

// ---------------------------------------------------------------------------------------------
// Calibration: lat_r16.py's, read off each page's own views

/** A family's leaves on one page: for each view, its count leaves (a template with the day as
 *  `d:*`, and the step one write moves it by) and its own leaves (a template with the item as
 *  `k:*`). */
type Leaves = Map<string, { count: [string, number][]; own: string[] }>;
const families = new Map<string, { items: Item[]; leaves: Map<string, Leaves> }>();

const isNumber = (value: unknown): value is number => typeof value === "number" && Number.isFinite(value);
const fill = (template: string, values: { day?: string; item?: string }) =>
  template.replace("d:*", `d:${values.day ?? "*"}`).replace("k:*", `k:${values.item ?? "*"}`);

async function calibrate(
  family: string,
  items: Item[],
  run: (item: Item) => Promise<number>,
  undo: (item: Item) => Promise<number>,
  restores: boolean
): Promise<Set<string>> {
  const byDay = new Map<string, Item[]>();
  for (const item of items) byDay.set(item.day, [...(byDay.get(item.day) ?? []), item]);
  const days = [...byDay].filter(([, xs]) => xs.length >= 2).map(([one]) => one).sort().reverse();
  const d0 = days[0];
  if (d0 === undefined) {
    mark(`calibrate ${family}: no day holds two candidates, so its writes are judged by no leaf`);
    return new Set();
  }
  const chosen = byDay.get(d0) ?? [];
  const a = chosen[chosen.length - 1] as Item;
  const b = chosen[chosen.length - 2] as Item;
  await gate();
  const s0a = tabs.map(snapshot);
  await sleep(1500);
  const s0 = tabs.map(snapshot);
  const snaps = [s0];
  for (const [item, step] of [
    [a, run],
    [b, run],
    [b, undo],
    [a, undo],
  ] as const) {
    await gate();
    await settle(await step(item));
    snaps.push(tabs.map(snapshot));
  }
  const [, s1, s2, , s4] = snaps;
  const perPage = new Map<string, Leaves>();
  for (const [page, tab] of tabs.entries()) {
    const leaves: Leaves = new Map();
    for (const [key, f0] of s0[page] ?? new Map()) {
      const f0a = s0a[page]?.get(key) ?? new Map<string, unknown>();
      const f1 = s1?.[page]?.get(key) ?? new Map<string, unknown>();
      const f2 = s2?.[page]?.get(key) ?? new Map<string, unknown>();
      const f4 = s4?.[page]?.get(key) ?? new Map<string, unknown>();
      const count: [string, number][] = [];
      const own: string[] = [];
      for (const text of new Set([...f0.keys(), ...f1.keys()])) {
        const v0 = f0.get(text);
        if (f0a.get(text) !== v0 || v0 === f1.get(text) || (restores && f4.get(text) !== v0)) continue;
        const path = pathOf(text);
        const mine = path.findIndex(([kind, name]) => kind === "k" && name === a.item);
        if (mine >= 0) {
          const template = pathText(path.map((step, index) => (index === mine ? (["k", "*"] as const) : step)));
          const atB = fill(template, { item: b.item });
          if (f1.get(atB) !== f2.get(atB) && !own.includes(template)) own.push(template);
          continue;
        }
        const onDay = path.findIndex(([kind, name]) => kind === "d" && name === d0);
        if (onDay < 0 || path.some(([kind, name]) => kind === "k" && name === b.item)) continue;
        const v1 = f1.get(text);
        const v2 = f2.get(text);
        if (!isNumber(v0) || !isNumber(v1) || !isNumber(v2)) continue;
        const d1 = v1 - v0;
        const d2 = v2 - v1;
        if (Math.abs(d1 - d2) < 1e-9 && Math.abs(d1 - Math.round(d1)) < 1e-9 && Math.round(d1) !== 0) {
          count.push([pathText(path.map((step, index) => (index === onDay ? (["d", "*"] as const) : step))), Math.round(d1)]);
        }
      }
      if (count.length > 0 || own.length > 0) leaves.set(key, { count, own });
      result(
        `tab recompute calibrate family=${family} page=${tab.name} view=${JSON.stringify(key)} count_leaves=${count.length} own_leaves=${own.length} ` +
          `first=${JSON.stringify([...count.slice(0, 3).map(([t, s]) => `${shown(t)} ${s > 0 ? "+" : ""}${s}`), ...own.slice(0, 2).map(shown)])}`
      );
    }
    perPage.set(tab.name, leaves);
  }
  result(`tab recompute calibrate family=${family} day=${d0} items=${a.item},${b.item}`);
  families.set(family, { items: items.filter((item) => item !== a && item !== b), leaves: perPage });
  return new Set([a.item, b.item]);
}

const sqlWrite = async (statement: string): Promise<number> => {
  await writer.unsafe(statement);
  return Date.now();
};
const patch = async (key: string, status: string): Promise<number> => {
  const write = factsWrite(key, status);
  const answer = await fetch(base + write.http.path, {
    method: "PATCH",
    headers: { Authorization: `Bearer ${token}`, "Content-Type": "application/json", "Idempotency-Key": crypto.randomUUID() },
    body: JSON.stringify(write.http.body),
  });
  await answer.text();
  if (answer.status !== 200 && answer.status !== 204) mark(`facts write ${key} -> ${status} answered ${answer.status}`);
  return Date.now();
};

await calibrate(
  "job",
  jobItems,
  (item) => sqlWrite(jobSql(item.item, true)),
  (item) => sqlWrite(jobSql(item.item, false)),
  true
);
await calibrate(
  "facts",
  factsItems,
  (item) => patch(item.item, "in_progress"),
  (item) => patch(item.item, item.status ?? "done"),
  false
);

/** n items of a family, drawn at random; a facts item only once in a run, since its undo closes the
 *  issue again today and its day rows are not what they were. */
function take(family: string, n: number): Item[] {
  const pool = families.get(family)?.items ?? [];
  const got = shuffle([...pool]).slice(0, Math.max(0, n));
  if (family === "facts") {
    const used = new Set(got);
    const kept = pool.filter((item) => !used.has(item));
    pool.splice(0, pool.length, ...kept);
  }
  return got;
}

const jobWrite = (item: Item, extra: Partial<Write> = {}): Write => ({
  id: `job:${item.item}`,
  family: "job",
  item: item.item,
  day: item.day,
  sql: jobSql(item.item, true),
  ...extra,
});
const factsWriteOf = (item: Item, extra: Partial<Write> = {}): Write => ({
  id: `facts:${item.item}`,
  family: "facts",
  item: item.item,
  day: item.day,
  undoStatus: item.status ?? "done",
  ...factsWrite(item.item, "in_progress"),
  ...extra,
});

// ---------------------------------------------------------------------------------------------
// A phase: its writes, its settle, its judgement on each page, and its undo

let worstOfAll = 0;

async function runPhase(name: string, writes: Write[], note: string): Promise<void> {
  const quietBefore = await gate();
  // What each page watches: each family's leaves for the days and items of the phase's writes.
  for (const tab of tabs) {
    const paths = new Map<string, Map<string, Step[]>>();
    for (const write of writes) {
      const leaves = families.get(write.family)?.leaves.get(tab.name);
      if (leaves === undefined || write.day === null) continue;
      for (const [key, { count, own }] of leaves) {
        const held = paths.get(key) ?? new Map<string, Step[]>();
        paths.set(key, held);
        for (const [template] of count) {
          const text = fill(template, { day: write.day });
          held.set(text, pathOf(text));
        }
        for (const template of own) {
          const text = fill(template, { item: write.item });
          held.set(text, pathOf(text));
        }
      }
    }
    const start = new Map<string, Map<string, unknown>>();
    const byView = subsByView(tab);
    for (const [key, held] of paths) start.set(key, readLeaves(tab.replay.states.get(byView.get(key) ?? ""), held));
    tab.watch = { paths, start, now: new Map([...start].map(([key, values]) => [key, new Map(values)])), stream: new Map() };
  }
  const needsPasses = writes.some((write) => write.waitPass);
  let pollTask: Promise<void> = Promise.resolve();
  if (needsPasses) {
    polling = true;
    passStarts.length = 0;
    pollTask = pollPasses();
  }
  const started = Date.now();
  const base0 = started + 300;
  for (const write of writes) if (write.atMs !== undefined) write.atMs += base0;
  let since = started;
  for (const write of writes) {
    await runWrite(write, since);
    since = write.sent ?? Date.now();
  }
  const last = Date.now();
  const settled = await settle(last, o.phaseSettle, Math.max(o.cap, 180));
  polling = false;
  await pollTask;
  const watches = tabs.map((tab) => tab.watch);
  for (const tab of tabs) tab.watch = null;
  await commitTimes(writes, tracked);
  const refused = writes.filter((write) => !ok(write));
  mark(
    `phase ${name}: ${writes.length} writes in ${((last - started) / 1000).toFixed(1)} s (${note}); settled ${settled.toFixed(0)} s after the last; ` +
      `quiet before ${quietBefore.toFixed(1)} s; refused or failed ${refused.length}${refused[0] ? ` first ${refused[0].status} ${refused[0].error ?? ""}` : ""}; ` +
      `pass starts seen ${passStarts.length}; keyed ${writes.filter((write) => write.keyed !== null && write.keyed !== undefined).length}`
  );
  // The undo, unmeasured: the job writes in one statement, each facts write one PATCH, the settings
  // as they were.
  const jobs = writes.filter((write) => write.family === "job" && ok(write)).map((write) => write.item);
  if (jobs.length > 0) {
    await writer.unsafe(
      `${OWN} update delivery_run_jobs set conclusion = 'success' where repo = ${lit(source.repo)} and name = ${lit(source.prod)} and run_id in (${jobs.join(",")})`
    );
  }
  for (const write of writes) if (write.family === "facts") await patch(write.item, write.undoStatus ?? "done");
  if (writes.some((write) => write.family === "settings")) await writer.unsafe(settingsSql(source.e2e));
  const undone = Date.now();
  await settle(undone, o.settle, Math.max(o.cap, 180));
  mark(`phase ${name}: undone and settled in ${((Date.now() - undone) / 1000).toFixed(0)} s`);

  let phaseWorst = 0;
  let phaseUnreflected = 0;
  for (const [index, tab] of tabs.entries()) {
    const watch = watches[index];
    if (watch === null || watch === undefined) continue;
    const judged = await judge(tab, watch, writes);
    const delays = judged.delays;
    const worst = delays.length > 0 ? Math.max(...delays) : null;
    phaseWorst = Math.max(phaseWorst, worst ?? 0);
    phaseUnreflected += judged.unreflected.length;
    result(
      `tab recompute phase=${name} rate=${rate} page=${tab.name} writes=${writes.length} judged=${judged.judged} ` +
        `median_ms=${ms(rank(delays, 0.5))} p90_ms=${ms(rank(delays, 0.9))} max_ms=${ms(worst)} over_2s=${delays.filter((x) => x > BOUND_MS).length} ` +
        `unreflected=${judged.unreflected.length} reflected_at_commit=${judged.early} leaves_dropped=${judged.dropped.length} ` +
        `worst_view=${JSON.stringify(judged.worstView)} budget_ms=${BOUND_MS}`
    );
    for (const line of judged.dropped.slice(0, 5)) mark(`phase ${name} page=${tab.name} dropped leaf ${shown(line)}`);
    for (const write of judged.unreflected.slice(0, 5)) mark(`phase ${name} page=${tab.name} unreflected ${write}`);
    if (judged.judged === 0) fail(`tab recompute phase=${name} rate=${rate} page=${tab.name}: no write was judged, so the page measured nothing`);
  }
  worstOfAll = Math.max(worstOfAll, phaseWorst);
  result(`tab recompute phase=${name} rate=${rate} pages=${tabs.length} worst_ms=${ms(phaseWorst)} unreflected=${phaseUnreflected} budget_ms=${BOUND_MS}`);
  if (phaseWorst > BOUND_MS || phaseUnreflected > 0) {
    fail(
      `tab recompute phase=${name} rate=${rate}: the slowest write reached a page ${ms(phaseWorst)} ms after its commit, and ${phaseUnreflected} never did, over the ${BOUND_MS} ms`
    );
  }
}

/** Each judged write's delay to the page (the header): its slowest view's presentation after
 *  the commit, lat_r16.py's attribution over the page's own frames. */
async function judge(
  tab: TabPage,
  watch: Watch,
  writes: Write[]
): Promise<{ judged: number; delays: number[]; unreflected: string[]; early: number; dropped: string[]; worstView: string | null }> {
  const timings = new Map<string, { at?: number; painted?: number }>();
  for (const frame of (await readInstrument(tab.page)).frames) timings.set(frameKey(frame.socket, frame.index), frame);
  const leaves = (family: string) => families.get(family)?.leaves.get(tab.name);
  const usable = writes
    .filter((write) => ok(write) && write.day !== null && leaves(write.family) !== undefined)
    .sort((x, y) => (x.commit ?? 0) - (y.commit ?? 0));
  const perDay = new Map<string, number>();
  const rankOf = new Map<Write, number>();
  for (const write of usable) {
    const key = `${write.family} ${write.day}`;
    perDay.set(key, (perDay.get(key) ?? 0) + 1);
    rankOf.set(write, perDay.get(key) ?? 0);
  }
  const final = watch.now;
  const bad = new Set<string>();
  for (const [key, n] of perDay) {
    const [family, onDay] = key.split(" ");
    for (const [view, { count }] of leaves(family ?? "") ?? new Map()) {
      for (const [template, step] of count) {
        const text = fill(template, { day: onDay });
        const b0 = watch.start.get(view)?.get(text);
        const f1 = final.get(view)?.get(text);
        if (isNumber(b0) && !(isNumber(f1) && Math.abs(f1 - (b0 + n * step)) < 1e-9)) bad.add(`${view} ${text}`);
      }
    }
  }
  type Use = { count: [string, number][]; own: string[] };
  const uses = new Map<Write, Map<string, Use>>();
  for (const write of usable) {
    const use = new Map<string, Use>();
    for (const [view, { count, own }] of leaves(write.family) ?? new Map()) {
      const start = watch.start.get(view) ?? new Map<string, unknown>();
      const end = final.get(view) ?? new Map<string, unknown>();
      const c = count
        .map(([template, step]) => [fill(template, { day: write.day ?? "" }), step] as [string, number])
        .filter(([text]) => isNumber(start.get(text)) && !bad.has(`${view} ${text}`));
      const w = own.map((template) => fill(template, { item: write.item })).filter((text) => end.get(text) !== start.get(text));
      if (c.length > 0 || w.length > 0) use.set(view, { count: c, own: w });
    }
    uses.set(write, use);
  }
  const latency = new Map<Write, Map<string, number | null>>();
  let early = 0;
  for (const view of watch.paths.keys()) {
    const judged = usable.filter((write) => uses.get(write)?.has(view));
    if (judged.length === 0) continue;
    const start = watch.start.get(view) ?? new Map<string, unknown>();
    const values = new Map(start);
    const shows = (write: Write) => {
      const use = uses.get(write)?.get(view);
      if (use === undefined) return false;
      for (const [text, step] of use.count) {
        const now = values.get(text);
        const was = start.get(text);
        if (!isNumber(now) || !isNumber(was) || (now - was) / step < (rankOf.get(write) ?? 1) - 1e-9) return false;
      }
      return use.own.every((text) => values.get(text) !== start.get(text));
    };
    const set = (write: Write, value: number | null) => {
      const held = latency.get(write) ?? new Map<string, number | null>();
      latency.set(write, held);
      held.set(view, value);
    };
    let next = 0;
    let pending: Write[] = [];
    for (const entry of watch.stream.get(view) ?? []) {
      const timing = timings.get(entry.key);
      const arrived = timing?.at ?? entry.heard;
      const presented = timing?.painted ?? arrived;
      while (next < judged.length && ((judged[next] as Write).commit ?? 0) <= arrived) {
        const write = judged[next++] as Write;
        if (shows(write)) {
          set(write, 0);
          early++;
        } else pending.push(write);
      }
      for (const [text, value] of entry.changed) values.set(text, value);
      pending = pending.filter((write) => {
        if (!shows(write)) return true;
        set(write, Math.round((presented - (write.commit ?? presented)) * 10) / 10);
        return false;
      });
    }
    for (const write of [...pending, ...judged.slice(next)]) set(write, null);
  }
  const delays: number[] = [];
  const unreflected: string[] = [];
  let worst = -1;
  let worstView: string | null = null;
  for (const write of usable) {
    const held = latency.get(write);
    if (held === undefined || held.size === 0) continue;
    if ([...held.values()].some((value) => value === null)) {
      unreflected.push(`${write.id} day=${write.day} views=${[...held].filter(([, value]) => value === null).map(([view]) => view.split(" ")[0]).join(",")}`);
      continue;
    }
    const slowest = Math.max(...([...held.values()] as number[]));
    delays.push(slowest);
    if (slowest > worst) {
      worst = slowest;
      worstView = [...held].find(([, value]) => value === slowest)?.[0] ?? null;
    }
  }
  return { judged: delays.length + unreflected.length, delays, unreflected, early, dropped: [...bad], worstView };
}

// ---------------------------------------------------------------------------------------------
// The phases

if (phases.includes("A")) {
  const writesPerSecond = o.aRate || 10 * runRate;
  const n = Math.max(1, Math.round(writesPerSecond * o.aSeconds));
  const nFacts = families.has("facts") ? Math.round(n * o.aFactsShare) : 0;
  const writes = shuffle([...take("job", n - nFacts).map((item) => jobWrite(item)), ...take("facts", nFacts).map((item) => factsWriteOf(item))]);
  let at = 0;
  for (const write of writes) {
    at += (-Math.log(1 - random()) / writesPerSecond) * 1000;
    write.atMs = at;
  }
  await runPhase("A", writes, `Poisson at ${writesPerSecond.toFixed(3)}/s for ${o.aSeconds} s (ten times the run rate unless --a-rate), ${writes.length} of ${n} writes`);
}
if (phases.includes("B")) {
  const items = take("job", o.bWrites + o.bControls + 1);
  const kinds = shuffle([...Array(o.bWrites).fill("write"), ...Array(o.bControls).fill("control")] as string[]);
  const writes = items.map((item, index) =>
    index === 0
      ? jobWrite(item)
      : jobWrite(item, {
          waitPass: true,
          delayMs: Math.round(random() * (kinds[index - 1] === "write" ? o.bMaxMs : o.bControlMs) * 10) / 10,
          timeoutMs: o.bTimeoutMs,
        })
  );
  await runPhase("B", writes, `a kick, then ${o.bWrites} writes at U(0, ${o.bMaxMs}) ms and ${o.bControls} at U(0, ${o.bControlMs}) ms after an eval-days pass start`);
}
if (phases.includes("C")) {
  const items = take("job", Math.floor(o.cRate * o.cSeconds));
  const writes = items.map((item, index) => jobWrite(item, { atMs: (index * 1000) / o.cRate }));
  await runPhase("C", writes, `a burst at ${o.cRate}/s for ${(writes.length / o.cRate).toFixed(0)} s`);
}
if (phases.includes("D")) {
  const jobs = take("job", o.dRounds);
  const facts = take("facts", o.dRounds);
  const writes: Write[] = [];
  for (let round = 0; round < o.dRounds; round++) {
    writes.push({
      id: `settings:${round}`,
      family: "settings",
      item: `settings:${round}`,
      day: null,
      sql: settingsSql(round % 2 === 0 ? (otherE2E ?? null) : source.e2e),
      atMs: round * o.dGap * 1000,
    });
    const job = jobs[round];
    if (job !== undefined) writes.push(jobWrite(job, { waitPass: true, delayMs: Math.round(random() * 500) / 10, timeoutMs: 5000 }));
    const fact = facts[round];
    if (fact !== undefined) writes.push(factsWriteOf(fact));
  }
  await runPhase("D", writes, `${o.dRounds} rounds ${o.dGap} s apart: the staging end-to-end job renamed and back in turn, a job write U(0, 50) ms after the eval-days pass it wakes starts, then a facts write`);
}

result(`tab recompute rate=${rate} phases=${phases.join(",")} pages=${tabs.length} worst_ms=${ms(worstOfAll)} budget_ms=${BOUND_MS}`);
await browser.close();
await Promise.all([db.close(), poller.close(), writer.close()]);
process.exit(failed() > 0 ? 1 : 0);
