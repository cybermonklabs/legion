// The Evals tab's browser rows' fixture (dispatch://INFRA-2918/artifact/plan-md, T28): the projects
// the checks k1 to k24 read, each written as intake, reconcile and the Legion sampler would have
// written it, and the reads those rows share: the live socket's frames, the tab's group links and
// the widths its tables draw at. The tab reads only stored facts, so every row seeds what it reads
// and opens the tab on it.
//
// The projects:
//
// - EVERY_SOURCE (ACME, the delivery fixtures' project): delivery on with its deploy source, the
//   measures fixture's pull requests, deploys and failures with its unowned P0 issue, the Pipeline
//   fixture's PR checks and deploy jobs, a Legion daemon with seven issues Legion worked and closed,
//   and one logged eval that declares a chart;
// - NO_DAEMON (EVALB): delivery on, no Legion daemon;
// - DELIVERY_OFF (EVALC): a Legion daemon, and delivery settings with delivery off;
// - NO_SOURCE (EVALD): no Legion daemon and no delivery settings row;
// - DEPLOY_RECORD (EVALE): a deploy source whose ten deploys each have a deploy row, and the failure
//   source's check failure blaming one, over the last ten days (k17, and k20's input that starts
//   ten days before the window's end);
// - DEFAULT_REPO (EVALF): a default repository and two pull requests of no issue on the second of
//   two days (k18);
// - PAGED (EVALG): more merges than one page of the merge timeline (k22).
//
// acme/widgets-shaped placeholders throughout (AGENTS.md: "This repository is public").
import { randomUUID } from "node:crypto";

import type {
  DeliveryLiveDelta,
  DeliveryLiveSnapshot,
  EvalItemsResponse,
  EvalListItem,
  EvalListResponse,
  LiveClientFrame,
  LiveServerFrame,
  LiveView,
  ProjectTimelineResponse,
} from "../web/src/api/types";
import { baseUrl, createIssue, createProject, registerEval, userHeaders } from "./api";
import { seedMeasuresFixture, seedPipelineRows } from "./delivery-seed";
import { DELIVERY_PROJECT, evalsTabPath, expectEvalsTab, paintedSection } from "./evals-tab";
import { seedDeliveryReconcileGithub } from "./fake-github-helpers";
import { seedLegionDaemon } from "./legion-seed";
import { sql } from "./psql";
import { sqlLiteral as lit } from "./seed";
import { expect, type Locator, type Page } from "./test";

// ---------------------------------------------------------------------------------------------
// The projects and their windows.

/** Every source live (k1, k2, k3, k5 to k16, k24): the delivery fixtures' project. */
export const EVERY_SOURCE = DELIVERY_PROJECT;
/** Delivery on, no Legion daemon (k1, k5, k11). */
export const NO_DAEMON = "EVALB";
/** A Legion daemon, delivery off (k1, k5). */
export const DELIVERY_OFF = "EVALC";
/** No Legion daemon and no delivery settings row (k1, k5). */
export const NO_SOURCE = "EVALD";
/** The deploy record's project (k17) and the input that starts ten days back (k20). */
export const DEPLOY_RECORD = "EVALE";
/** The project whose default repository brings two pull requests of no issue (k18). */
export const DEFAULT_REPO = "EVALF";
/** The project of more merges than one page of the merge timeline holds (k22). */
export const PAGED = "EVALG";

/** A whole UTC day in milliseconds. */
const DAY_MS = 24 * 60 * 60 * 1000;

/** `from` and `to` as a tab's address names a custom window, without the leading "?". */
export function windowQuery(window: { readonly from: string; readonly to: string }): string {
  return `from=${encodeURIComponent(window.from)}&to=${encodeURIComponent(window.to)}`;
}

/** EVERY_SOURCE's window: both delivery fixtures, the measures fixture's (2026-08-30 to 09-27)
 *  and the Pipeline fixture's (to 2026-10-02), and the logged eval below. */
export const TAB_WINDOW = { from: "2026-08-30T00:00:00Z", to: "2026-10-03T00:00:00Z" } as const;
export const TAB_QUERY = windowQuery(TAB_WINDOW);

/** The address the Legion daemon of a seeded project is named at: nothing answers it, so the
 *  sampler records a failed read and the header says so. */
const UNREACHABLE_DAEMON = "http://127.0.0.1:9/legion/state";

const SEED_ACTOR = `'{"kind":"system","id":"e2e-evals-seed"}'`;

/** A moment `hours` after midnight of the UTC day `day` (YYYY-MM-DD), as ISO 8601. */
function at(day: string, hours: number): string {
  return new Date(Date.parse(`${day}T00:00:00Z`) + hours * 3_600_000).toISOString();
}

/** The UTC day `days` after `day`, as YYYY-MM-DD. */
function dayAfter(day: string, days: number): string {
  return new Date(Date.parse(`${day}T00:00:00Z`) + days * DAY_MS).toISOString().slice(0, 10);
}

// ---------------------------------------------------------------------------------------------
// The statements a project's sources are written with.

/** Gives `project` delivery on, reading `repo` as its default repository and its one deploy
 *  source's repository (the source names none), with octocat as its population, the production
 *  job `release`, and the PR-checks workflow. With `failuresSince`, its failure source is the
 *  failed checks (`check_results`), measured from that day, which turns its three failure
 *  measures on (`failureProjectOf`, internal/dispatch/evals/failure_days.go). */
function deliveryOnSql(
  project: string,
  repo: string,
  failuresSince: string | null = null
): string[] {
  const failures =
    failuresSince === null
      ? "ARRAY[]::text[], null"
      : `ARRAY['check_results'], ${lit(failuresSince)}`;
  return [
    `UPDATE projects SET default_repo = ${lit(repo)} WHERE key = ${lit(project)}`,
    `INSERT INTO project_delivery_settings (
       project_key, delivery, population_authors, excluded_repos, updated_by, failure_sources,
       failures_measured_since
     ) VALUES (${lit(project)}, true, ARRAY['octocat'], ARRAY[]::text[], ${SEED_ACTOR}, ${failures})`,
    `INSERT INTO project_deploy_sources (
       project_key, position, kind, repo, deploy_workflow_path, production_job_names,
       pr_checks_workflow_path
     ) VALUES (
       ${lit(project)}, 0, 'github_workflow', null, '.github/workflows/deploy.yml',
       ARRAY['release'], '.github/workflows/pr-checks.yml'
     )`,
  ];
}

/** Names `project`'s Legion daemon at an address nothing answers, and wakes the facts loop, as
 *  the Settings page's save does (legion.StateChannel). */
function daemonSql(project: string): string[] {
  return [
    `INSERT INTO legion_settings (project, state_url, created_by, updated_at)
       VALUES (${lit(project)}, ${lit(UNREACHABLE_DAEMON)}, ${SEED_ACTOR}, now())`,
    `SELECT pg_notify('dispatch_legion_state', ${lit(project)})`,
  ];
}

/** One pull request of `repo`, by octocat, merged at `merged` after opening two hours and taking
 *  its first commit three hours before, naming `issue` (null for none). */
interface SeededPull {
  readonly repo: string;
  readonly number: number;
  readonly merged: string;
  readonly issue: string | null;
  readonly rework?: boolean;
}

function pullsSql(pulls: readonly SeededPull[]): string {
  const row = (pull: SeededPull) => {
    const merged = Date.parse(pull.merged);
    const opened = new Date(merged - 2 * 3_600_000).toISOString();
    const first = new Date(merged - 3 * 3_600_000).toISOString();
    return `(${lit(pull.repo)}, ${pull.number}, ${lit(`feat: widget change ${pull.number}`)},
      ${lit(`https://github.com/${pull.repo}/pull/${pull.number}`)}, 'octocat', ${lit(opened)},
      ${lit(pull.merged)}, ${lit(first)}, 10, 2, ${pull.rework === true}, ${
        pull.issue === null ? "null" : lit(pull.issue)
      }, ARRAY[]::text[], false)`;
  };
  return `INSERT INTO delivery_pull_requests (
     repo, number, title, url, author, created_at, merged_at, first_commit_at,
     additions, deletions, rework, issue_key, sessions, partial
   ) VALUES ${pulls.map(row).join(",\n")}`;
}

/** One deploy run of `repo` on main whose head commit lands five minutes after `after`: its build
 *  job, then its production job `release`, which succeeds sixteen minutes after `after`. */
interface SeededRun {
  readonly repo: string;
  readonly id: number;
  readonly after: string;
}

/** The times of `run`: its head commit, its start, its production job's end and its end. */
function runTimes(run: SeededRun): {
  head: string;
  started: string;
  built: string;
  released: string;
  completed: string;
} {
  const base = Date.parse(run.after);
  const minutes = (count: number) => new Date(base + count * 60_000).toISOString();
  return {
    built: minutes(10),
    completed: minutes(18),
    head: minutes(5),
    released: minutes(16),
    started: minutes(6),
  };
}

function runsSql(runs: readonly SeededRun[]): string[] {
  const run = (seeded: SeededRun) => {
    const times = runTimes(seeded);
    return `(${lit(seeded.repo)}, ${seeded.id}, 'deploy', ${lit(`sha${seeded.id}`)},
      ${lit(times.head)}, ${lit(times.started)}, ${lit(times.started)}, ${lit(times.completed)},
      'success', ${lit(`https://github.com/${seeded.repo}/actions/runs/${seeded.id}`)}, 'main', 'push')`;
  };
  const jobs = (seeded: SeededRun) => {
    const times = runTimes(seeded);
    return [
      `(${lit(seeded.repo)}, ${seeded.id}, 'build', ${lit(times.started)}, ${lit(times.built)}, 'success')`,
      `(${lit(seeded.repo)}, ${seeded.id}, 'release', ${lit(times.built)}, ${lit(times.released)}, 'success')`,
    ];
  };
  return [
    `INSERT INTO delivery_runs (
       repo, run_id, kind, head_sha, head_commit_at, started_at, created_at, completed_at,
       conclusion, url, head_branch, event
     ) VALUES ${runs.map(run).join(",\n")}`,
    `INSERT INTO delivery_run_jobs (repo, run_id, name, started_at, completed_at, conclusion)
       VALUES ${runs.flatMap(jobs).join(",\n")}`,
  ];
}

/** Opens an issue of `project` in one statement, as the seeds of the delivery fixtures do. */
function issueSql(
  project: string,
  number: number,
  title: string,
  extra: { status?: string; priority?: number; created?: string; closed?: string } = {}
): string {
  const created = extra.created === undefined ? "now()" : lit(extra.created);
  const closed = extra.closed === undefined ? "null" : lit(extra.closed);
  return `INSERT INTO issues (
     key, project_key, number, title, status, priority, created_by, rank, created_at, closed_at
   ) VALUES (
     ${lit(`${project}-${number}`)}, ${lit(project)}, ${number}, ${lit(title)},
     ${lit(extra.status ?? "todo")}, ${extra.priority ?? 2}, ${SEED_ACTOR}, ${lit(`U${number}`)},
     ${created}, ${closed}
   )`;
}

// ---------------------------------------------------------------------------------------------
// EVERY_SOURCE's Legion work.

/** The roles that work each of LEGION_WORK's issues, each one's first event this many hours after
 *  08:00 of the issue's day: the event starts the role's phase (legion/metrics' `rolePhase`), so
 *  each issue holds hours on the planner's, the implementer's, the tester's and the reviewer's
 *  bars, and the reviewer's phase runs to the close. */
const LEGION_ROLES = [
  { role: "planner", hours: 0 },
  { role: "implementer", hours: 2 },
  { role: "tester", hours: 5 },
  { role: "reviewer", hours: 7 },
] as const;

/** How many days before today the first issue of LEGION_WORK closed: before the first day of the
 *  tab's default window (28 days, `DEFAULT_WINDOW_PRESET`). A Legion number reads "no data before
 *  <day>" over a window that starts before its input began (spec R18): with every issue inside
 *  the window, its headlines and its phase bars would read no value, and draw no group link. */
const FIRST_LEGION_WORK_DAYS_AGO = 35;

/** The issues Legion worked on EVERY_SOURCE and closed: 200, FIRST_LEGION_WORK_DAYS_AGO days before
 *  today, where the Legion evals' input begins, and then one on each of the six days before today,
 *  so a window that ends today holds those six, beside the unowned P0 card, whose issue is
 *  today's, and each day's row of a Legion chart links to its own (k8); each closed nine hours
 *  after its planner began. */
export const LEGION_WORK = [
  {
    day: dayAfter(new Date().toISOString().slice(0, 10), -FIRST_LEGION_WORK_DAYS_AGO),
    number: 200,
  },
  ...[201, 202, 203, 204, 205, 206].map((number, index) => ({
    day: dayAfter(new Date().toISOString().slice(0, 10), index - 6),
    number,
  })),
];

/** The handoff each role writes when its phase ends (legion/metrics' `handoffPhase`). */
const HANDOFF_WORDS: Readonly<Record<(typeof LEGION_ROLES)[number]["role"], string>> = {
  implementer: "implement",
  planner: "plan",
  reviewer: "review",
  tester: "test",
};

/** The issue rows and the role sessions' events of LEGION_WORK: a session titled
 *  `Legion <role> · <key>` (legion.ParseSessionTitle), whose event carries the title, so the
 *  events trigger records it (session_titles) and the facts loop reads the issue as one with Legion
 *  phase work. */
function legionWorkSql(project: string): string[] {
  const issues = LEGION_WORK.map(({ day, number }) =>
    issueSql(project, number, `Legion widget work ${number}`, {
      closed: at(day, 17),
      created: at(day, 7),
      status: "done",
    })
  );
  const events = LEGION_WORK.flatMap(({ day, number }) =>
    LEGION_ROLES.map(({ hours, role }, index) => {
      const key = `${project}-${number}`;
      const actor = JSON.stringify({
        id: `e2e-legion-${role}-${number}`,
        kind: "session",
        origin: { session_title: `Legion ${role} \u00b7 ${key}` },
      });
      return `(${lit(key)}, ${index + 1}, 'message.created', ${lit(actor)}, '{}', false, ${lit(at(day, 8 + hours))})`;
    })
  );
  // Each role hands its phase on when the next role starts, and the reviewer half an hour
  // before the close: a handoff is phase work of its own (legion/metrics' hasPhaseWork).
  const handoffs = LEGION_WORK.flatMap(({ day, number }) =>
    LEGION_ROLES.map(({ role }, index) => {
      const author = JSON.stringify({ id: `e2e-legion-${role}-${number}`, kind: "session" });
      const word = HANDOFF_WORDS[role];
      const hours = LEGION_ROLES[index + 1]?.hours ?? 8.5;
      return `(${lit(`${project}-${number}`)}, ${lit(word)}, 1, '{}', ${lit(author)}, ${lit(at(day, 8 + hours))})`;
    })
  );
  return [
    ...issues,
    `UPDATE issues SET last_seq = ${LEGION_ROLES.length}
      WHERE key = ANY(ARRAY[${LEGION_WORK.map(({ number }) => lit(`${project}-${number}`)).join(", ")}])`,
    `INSERT INTO events (issue_key, seq, type, actor, payload, notify, created_at)
       VALUES ${events.join(",\n")}`,
    `INSERT INTO handoffs (issue_key, phase, version, body, author, created_at)
       VALUES ${handoffs.join(",\n")}`,
  ];
}

// ---------------------------------------------------------------------------------------------
// The evals the rows write through the routes.

/** One reading as the readings write takes it. */
export interface ReadingInput {
  readonly series: string;
  readonly row_kind: string;
  readonly measured_at: string;
  readonly value?: number | null;
  readonly value_absent_reason?: string;
  readonly row_key?: string;
  readonly col?: string;
  readonly unit?: string;
  readonly n?: number | null;
  readonly n_unit?: string;
  readonly ci_low?: number | null;
  readonly ci_high?: number | null;
  readonly ci_level?: number | null;
  readonly attributes?: Readonly<Record<string, unknown>> | null;
  readonly links?: readonly {
    readonly href: string;
    readonly label?: string;
    readonly rel: string;
  }[];
  readonly note?: string;
  readonly source?: string;
}

/** The address of one of `project`'s eval routes under `slug`. */
function evalPath(project: string, slug: string, rest = ""): URL {
  return new URL(
    `/api/v1/projects/${encodeURIComponent(project)}/evals/${encodeURIComponent(slug)}${rest}`,
    baseUrl
  );
}

/** `POST /api/v1/projects/{key}/evals/{slug}/readings` as alice, under a key of its own (the
 *  write requires one, 400 IDEMPOTENCY_KEY_REQUIRED): the answer's status and body. A slug no eval
 *  holds registers its eval (spec R19). */
export async function postReadings(
  project: string,
  slug: string,
  readings: readonly ReadingInput[],
  headers: Readonly<Record<string, string>> = {}
): Promise<{ status: number; body: string }> {
  const written = await fetch(evalPath(project, slug, "/readings"), {
    body: JSON.stringify({ readings }),
    headers: {
      "Content-Type": "application/json",
      "Idempotency-Key": randomUUID(),
      ...(Object.keys(headers).length === 0 ? await userHeaders("alice") : headers),
    },
    method: "POST",
  });
  return { body: await written.text(), status: written.status };
}

/** `postReadings`, which must be answered 201. */
export async function logReadings(
  project: string,
  slug: string,
  readings: readonly ReadingInput[]
): Promise<void> {
  const written = await postReadings(project, slug, readings);
  expect(written.status, `the readings of ${project}/${slug}: ${written.body}`).toBe(201);
}

/** `GET /api/v1/projects/{key}/evals/{slug}` as alice: the eval's versions, newest first. */
export async function readEvalDetail(
  project: string,
  slug: string,
  limit = 10
): Promise<{ status: number; body: unknown }> {
  const read = await fetch(evalPath(project, slug, `?limit=${limit}`), {
    headers: await userHeaders("alice"),
  });
  return { body: read.status === 200 ? await read.json() : await read.text(), status: read.status };
}

/** A GET of one of the routes the tab mirrors, as alice, which must answer 200: its body. */
export async function getJson<T>(path: string): Promise<T> {
  const read = await fetch(new URL(path, baseUrl), { headers: await userHeaders("alice") });
  const text = await read.text();
  expect(read.status, `GET ${path}: ${text}`).toBe(200);
  return JSON.parse(text) as T;
}

/** `project`'s evals over `window` (`GET /api/v1/projects/{key}/evals`), every kind, one page. */
export function listEvals(
  project: string,
  window: { readonly from: string; readonly to: string },
  extra = ""
): Promise<EvalListResponse> {
  return getJson<EvalListResponse>(
    `/api/v1/projects/${encodeURIComponent(project)}/evals?${windowQuery(window)}&limit=200${extra}`
  );
}

/** Holds, before a row compares a number of `project`'s tab, that the stored number `statistic`
 *  of `series` of the eval `slug` (of `kind`) has a value over `window`: the refresher's walk has
 *  built every day of the window before today (a window holding a day the walk has not built reads
 *  no value, evals.Built), and the window starts on or after the series' first day (spec R18). A
 *  failure names the series' first day (`data_since`) and how far each walk serves the rows. */
export async function expectNumberStands(
  project: string,
  window: { readonly from: string; readonly to: string },
  kind: string,
  slug: string,
  series: string,
  statistic: string
): Promise<void> {
  const read = async () => {
    const listed = await listEvals(project, window, `&kind=${kind}`);
    const item = (listed.items ?? []).find((candidate) => candidate.definition.slug === slug);
    const values = item?.series?.find((one) => one.series === series);
    return { since: values?.data_since ?? null, value: values?.window?.[statistic] ?? null };
  };
  try {
    await expect
      .poll(async () => (await read()).value, {
        message: `${project}'s ${slug} ${series} ${statistic} over ${window.from}..${window.to} has a value`,
        timeout: 60_000,
      })
      .not.toBeNull();
  } catch (error) {
    const { since } = await read();
    const serving = await sql(
      `SELECT coalesce(string_agg(family || ' ' || project_key || ' ' || next_day, '; '), 'none') FROM eval_days_serving`
    );
    throw new Error(
      `${String(error)}\n${slug} ${series}: data_since ${since ?? "none"}; the walks serve ${serving}`
    );
  }
}

/** One page of `slug`'s readings over `window` (the items route, `source=readings`). */
export function listReadings(
  project: string,
  slug: string,
  window: { readonly from: string; readonly to: string },
  query: string
): Promise<EvalItemsResponse> {
  return getJson<EvalItemsResponse>(
    `/api/v1/projects/${encodeURIComponent(project)}/evals/${encodeURIComponent(slug)}/items?${windowQuery(window)}&source=readings&${query}`
  );
}

/** The project timeline route's answer over `window` (`GET /api/v1/projects/{key}/timeline`). */
export function readTimeline(
  project: string,
  window: { readonly from: string; readonly to: string },
  limit: number
): Promise<ProjectTimelineResponse> {
  return getJson<ProjectTimelineResponse>(
    `/api/v1/projects/${encodeURIComponent(project)}/timeline?${windowQuery(window)}&limit=${limit}`
  );
}

/** EVERY_SOURCE's logged eval that declares one chart (k2): an `xy` of its readings over time,
 *  with an answer the cross-project page draws beside its number (k11). */
export const DECLARED_EVAL = {
  answer: "Widget latency stays under 150 ms at the median.",
  chart: "latency",
  name: "Widget latency",
  series: "latency",
  slug: "e2e-widget-latency",
} as const;

/** The declared eval's readings, one a day in TAB_WINDOW. */
export const DECLARED_READINGS: readonly ReadingInput[] = [120, 135, 110, 140, 125].map(
  (value, index) => ({
    measured_at: at(dayAfter("2026-09-10", index), 12),
    row_kind: "point",
    series: DECLARED_EVAL.series,
    unit: "ms",
    value,
  })
);

/** The declared eval: its readings, whose write registers it (spec R19), then the register write
 *  that names it, its answer, its series and its one chart. */
async function seedDeclaredEval(project: string): Promise<void> {
  await logReadings(project, DECLARED_EVAL.slug, DECLARED_READINGS);
  await registerEval(project, {
    answer: DECLARED_EVAL.answer,
    charts: [
      {
        id: DECLARED_EVAL.chart,
        kind: "xy",
        title: "Widget latency by day",
        y: { series: DECLARED_EVAL.series },
      },
    ],
    name: DECLARED_EVAL.name,
    series: [{ is_primary: true, listed: true, series: DECLARED_EVAL.series, unit: "ms" }],
    slug: DECLARED_EVAL.slug,
  });
}

/** The logged eval the relation rows read their links from (k10, and the relation inventory). */
export const RELATIONS_EVAL = { name: "Widget relations", slug: "e2e-widget-relations" } as const;

/** The four relations the tab draws apart from a plain link (spec R10), and two it draws plain. */
export const SPECIAL_RELATIONS = ["via", "describedby", "enclosure", "source-rule"] as const;
export const PLAIN_RELATIONS = ["related", "alternate"] as const;

/** A link whose target is gone: the tab draws it as any other, since nothing fetches a target
 *  (plan D12). `.invalid` is reserved (RFC 2606), so no host ever answers it. */
export const GONE_LINK = {
  href: "https://results.example.invalid/gone",
  label: "a result that is gone",
  rel: "via",
} as const;

/** A link of every relation, each to a result page of its own under `where`. */
function linksOfEveryRelation(where: string) {
  return [...SPECIAL_RELATIONS, ...PLAIN_RELATIONS].map((rel) => ({
    href: `https://results.example.com/${where}/${rel}`,
    label: `${where} ${rel}`,
    rel,
  }));
}

/** The relation eval: a logged eval whose own links, its primary series' links and one reading's
 *  links each hold every relation, and whose reading holds the gone link too. The readings write
 *  registers it (spec R19), and the register write then names its links. */
export async function seedRelations(project: string): Promise<void> {
  await logReadings(project, RELATIONS_EVAL.slug, [
    {
      links: [...linksOfEveryRelation("reading"), GONE_LINK],
      measured_at: "2026-09-10T12:00:00Z",
      row_key: "widget-1",
      row_kind: "row",
      series: "widgets",
      value: 3,
    },
  ]);
  await registerEval(project, {
    links: linksOfEveryRelation("eval"),
    name: RELATIONS_EVAL.name,
    series: [
      { is_primary: true, links: linksOfEveryRelation("series"), listed: true, series: "widgets" },
    ],
    slug: RELATIONS_EVAL.slug,
  });
}

/** Every relation drawn in `place`: the four of R10 each as its own kind, and the two others as a
 *  plain link. */
export async function expectEveryRelation(place: Locator, where: string): Promise<void> {
  for (const rel of SPECIAL_RELATIONS) {
    const link = place.locator(`a[data-rel="${rel}"][data-link-kind="${rel}"]`).first();
    await expect(link, `${where}: ${rel}`).toBeVisible();
  }
  for (const rel of PLAIN_RELATIONS) {
    const link = place.locator(`a[data-rel="${rel}"][data-link-kind="plain"]`).first();
    await expect(link, `${where}: ${rel}`).toBeVisible();
  }
}

// ---------------------------------------------------------------------------------------------
// The seeds.

/** EVERY_SOURCE with every source live (k1, k2, k3, k5 to k16, k24): made through the API, so its
 *  evals are seeded in the create's own transaction, then the delivery fixtures, the Legion daemon
 *  and the issues Legion worked, and the logged eval that declares a chart. */
export async function seedEverySource(): Promise<void> {
  await createProject({ key: EVERY_SOURCE, name: "Acme" });
  await seedMeasuresFixture();
  await seedPipelineRows();
  await seedLegionDaemon(UNREACHABLE_DAEMON);
  // The reconcile passes the stack's server runs succeed and leave the freshness line Live
  // (delivery-live.e2e.ts does the same).
  await seedDeliveryReconcileGithub();
  await sql(
    `UPDATE project_deploy_sources SET last_event_at = now(), last_reconcile_at = now(), last_error = null
      WHERE project_key = ${lit(EVERY_SOURCE)}`
  );
  await sql(
    "BEGIN",
    ...legionWorkSql(EVERY_SOURCE),
    // The facts loop reads a project whole where it holds no record of having read it with its
    // build (issue_facts_computed, legionfacts' legionProjects), whatever its events cursor says:
    // a cursor a pass wrote across the scenario's reset can stand above the event ids the reset
    // restarted, which no page then reads.
    `DELETE FROM issue_facts_computed WHERE project_key = ${lit(EVERY_SOURCE)}`,
    `SELECT pg_notify('dispatch_legion_state', ${lit(EVERY_SOURCE)})`,
    "COMMIT"
  );
  // An event written through the API after them wakes the loop's next pass, which reads the
  // project whole on its first page.
  await createIssue({ project: EVERY_SOURCE, title: "Horizon marker" });
  await seedDeclaredEval(EVERY_SOURCE);
}

/** NO_DAEMON: delivery on, reading a repository no fixture writes to, and no Legion daemon. */
export async function seedNoDaemon(): Promise<void> {
  await createProject({ key: NO_DAEMON, name: "Evals without a daemon" });
  await sql("BEGIN", ...deliveryOnSql(NO_DAEMON, "acme/evalb"), "COMMIT");
}

/** DELIVERY_OFF: a Legion daemon, and a delivery settings row with delivery off and no source. */
export async function seedDeliveryOff(): Promise<void> {
  await createProject({ key: DELIVERY_OFF, name: "Evals with delivery off" });
  await sql(
    "BEGIN",
    `INSERT INTO project_delivery_settings (
       project_key, delivery, population_authors, excluded_repos, updated_by
     ) VALUES (${lit(DELIVERY_OFF)}, false, ARRAY['octocat'], ARRAY[]::text[], ${SEED_ACTOR})`,
    ...daemonSql(DELIVERY_OFF),
    "COMMIT"
  );
}

/** Takes DELIVERY_OFF's delivery settings row away, so it has a daemon and no settings at all. */
export async function dropDeliverySettings(project: string): Promise<void> {
  await sql(
    "BEGIN",
    `DELETE FROM project_deploy_sources WHERE project_key = ${lit(project)}`,
    `DELETE FROM project_delivery_settings WHERE project_key = ${lit(project)}`,
    "COMMIT"
  );
}

/** NO_SOURCE: the project alone. */
export async function seedNoSource(): Promise<void> {
  await createProject({ key: NO_SOURCE, name: "Evals with no source" });
}

/** The four projects of k1 and k5: every source, no daemon, delivery off, no source. */
export async function seedFourProjects(): Promise<void> {
  await seedEverySource();
  await seedNoDaemon();
  await seedDeliveryOff();
  await seedNoSource();
}

/** What `seedDeployRecord` wrote: the first day of its input, and the deploy runs whose
 *  drill-downs k17 opens. */
export interface DeployRecordFixture {
  /** The first UTC day with input, ten days before today (YYYY-MM-DD). */
  readonly since: string;
  /** The deploy run k17 flags, which no failure blames yet. */
  readonly flagRun: number;
  /** The deploy run the failure source's check failure blames, which k17 rejects. */
  readonly failedRun: number;
}

/** DEPLOY_RECORD: delivery on from `acme/evale`, and on each of the ten days ending yesterday one
 *  pull request of EVALE-1 merged at 09:00 and shipped by that day's deploy run (701 to 710), each
 *  run with the deploy row the GitHub adapter writes for it (production_events, source github).
 *  With `failure`, the failure source's failed check (signal check_fail) blames run 708's deploy.
 *  The input begins ten days before the end of a window that ends today (k20). */
export async function seedDeployRecord(failure: boolean): Promise<DeployRecordFixture> {
  const since = dayAfter(new Date().toISOString().slice(0, 10), -10);
  const days = Array.from({ length: 10 }, (_, index) => dayAfter(since, index));
  const repo = "acme/evale";
  const runs = days.map((day, index) => ({ after: at(day, 9), id: 701 + index, repo }));
  const deployRows = runs.map((run, index) => {
    const times = runTimes(run);
    return `(${lit(DEPLOY_RECORD)}, 'deploy', 'succeeded', 'github', ${lit(`github:${run.id}`)},
      ${lit(times.started)}, ${lit(times.released)}, ${lit(repo)}, ARRAY[${lit(`sha${run.id}`)}],
      ARRAY[${lit(`${repo}#${index + 1}`)}], ${lit(`https://github.com/${repo}/actions/runs/${run.id}`)},
      '{"kind":"system","id":"github"}')`;
  });
  await createProject({ key: DEPLOY_RECORD, name: "Evals deploy record" });
  await sql(
    "BEGIN",
    ...deliveryOnSql(DEPLOY_RECORD, repo, since),
    issueSql(DEPLOY_RECORD, 1, "Ship the evaluated widgets"),
    pullsSql(
      days.map((day, index) => ({
        issue: `${DEPLOY_RECORD}-1`,
        merged: at(day, 9),
        number: index + 1,
        repo,
      }))
    ),
    ...runsSql(runs),
    `INSERT INTO production_events (
       project_key, kind, status, source, source_ref, started_at, finished_at, repo, commits,
       pull_requests, url, actor
     ) VALUES ${deployRows.join(",\n")}`,
    ...(failure
      ? [
          `INSERT INTO production_events (
             project_key, kind, source, source_ref, deploy_id, signal, detected_at, pull_requests,
             actor
           ) SELECT ${lit(DEPLOY_RECORD)}, 'failure', 'check_results', 'check:e2e-708', id,
             'check_fail', finished_at + interval '2 hours', ARRAY[${lit(`${repo}#8`)}],
             '{"kind":"system","id":"check-results"}'
             FROM production_events
            WHERE project_key = ${lit(DEPLOY_RECORD)} AND source_ref = 'github:708'`,
        ]
      : []),
    "COMMIT"
  );
  return { failedRun: 708, flagRun: 707, since };
}

/** DEFAULT_REPO's two days (k18): the first holds one pull request of an issue, the second the
 *  two pull requests of no issue that reach the project through its default repository. */
export const DEFAULT_REPO_DAYS = ["2026-10-20", "2026-10-21"] as const;

/** DEFAULT_REPO: `acme/evalf` its default repository and its deploy source's, EVALF-1's pull
 *  request merged on the first day and shipped by run 801, and two pull requests of no issue
 *  merged on the second and shipped by run 802. */
export async function seedDefaultRepository(): Promise<void> {
  const [first, second] = DEFAULT_REPO_DAYS;
  const repo = "acme/evalf";
  await createProject({ key: DEFAULT_REPO, name: "Evals default repository" });
  await sql(
    "BEGIN",
    ...deliveryOnSql(DEFAULT_REPO, repo),
    issueSql(DEFAULT_REPO, 1, "Ship the defaulted widgets"),
    pullsSql([
      { issue: `${DEFAULT_REPO}-1`, merged: at(first, 8), number: 1, repo },
      { issue: null, merged: at(second, 8), number: 2, repo },
      { issue: null, merged: at(second, 8.5), number: 3, repo, rework: true },
    ]),
    ...runsSql([
      { after: at(first, 9), id: 801, repo },
      { after: at(second, 9), id: 802, repo },
    ]),
    "COMMIT"
  );
}

/** PAGED's window and its merges (k22): more than two pages of the merge timeline, one every
 *  eighty minutes from the window's start. */
export const PAGED_WINDOW = { from: "2026-10-01T00:00:00Z", to: "2026-10-29T00:00:00Z" } as const;
export const PAGED_MERGES = 450;
const PAGED_EVERY_MINUTES = 80;

/** When PAGED's merge `number` (1 to PAGED_MERGES) merged. */
export function pagedMergedAt(number: number): string {
  return new Date(
    Date.parse(PAGED_WINDOW.from) + number * PAGED_EVERY_MINUTES * 60_000
  ).toISOString();
}

/** PAGED: delivery on from `acme/evalg`, and PAGED_MERGES pull requests of no issue merged in
 *  PAGED_WINDOW, written in one statement. */
export async function seedPagedTimeline(): Promise<void> {
  const repo = "acme/evalg";
  const start = lit(PAGED_WINDOW.from);
  await createProject({ key: PAGED, name: "Evals paged timeline" });
  await sql(
    "BEGIN",
    ...deliveryOnSql(PAGED, repo),
    `INSERT INTO delivery_pull_requests (
       repo, number, title, url, author, created_at, merged_at, first_commit_at,
       additions, deletions, rework, issue_key, sessions, partial
     ) SELECT ${lit(repo)}, n, 'feat: paged widget ' || n,
         'https://github.com/acme/evalg/pull/' || n, 'octocat',
         timestamptz ${start} + n * interval '${PAGED_EVERY_MINUTES} minutes' - interval '1 hour',
         timestamptz ${start} + n * interval '${PAGED_EVERY_MINUTES} minutes',
         timestamptz ${start} + n * interval '${PAGED_EVERY_MINUTES} minutes' - interval '2 hours',
         1, 1, false, null, ARRAY[]::text[], false
       FROM generate_series(1, ${PAGED_MERGES}) AS n`,
    "COMMIT"
  );
}

// ---------------------------------------------------------------------------------------------
// Opening the tab.

/** Opens `project`'s Evals tab at `query` and waits for its header to draw. */
export async function openTab(page: Page, project: string, query = ""): Promise<void> {
  await page.goto(evalsTabPath(query, project));
  await expectEvalsTab(page);
  await expect(page.getByRole("group", { name: "Window and filters" })).toBeVisible();
}

/** A chart figure of the tab (`EvalChart`): never one of its drawn values or table cells, which
 *  carry `data-chart` too. */
export function chartFigure(scope: Page | Locator, name: string): Locator {
  return scope.locator(`figure[data-chart="${name}"]`);
}

/** Turns `figure` to its table, or back to its graph. */
export async function showChart(figure: Locator, mode: "Graph" | "Table"): Promise<void> {
  await figure.getByRole("button", { exact: true, name: mode }).click();
  await expect(figure.getByRole("button", { exact: true, name: mode })).toHaveAttribute(
    "aria-pressed",
    "true"
  );
}

// ---------------------------------------------------------------------------------------------
// The live socket.

/** One frame of the live socket, read at the test boundary as the type the contract both ends
 *  hold gives it (packages/contracts, dispatch-api-live.ts): the page and Dispatch write no other
 *  frame on it. A frame with no `type` fails the row here rather than in an assertion after. */
export function liveFrame<T extends { readonly type: string }>(payload: string | Buffer): T {
  const text = typeof payload === "string" ? payload : payload.toString("utf8");
  const frame = JSON.parse(text) as T | null;
  if (typeof frame?.type !== "string") {
    throw new Error(`the live socket carried a frame with no type: ${text}`);
  }
  return frame;
}

/** What the page sent and was sent on its live socket, and every request it made, each stamped
 *  with when the row saw it. */
export interface LiveRecord {
  readonly sent: { readonly at: number; readonly frame: LiveClientFrame }[];
  readonly received: { readonly at: number; readonly frame: LiveServerFrame }[];
  readonly requests: { readonly at: number; readonly method: string; readonly url: URL }[];
}

/** Watches `page`'s live socket and its requests from before its first navigation. */
export function recordLive(page: Page): LiveRecord {
  const record: LiveRecord = { received: [], requests: [], sent: [] };
  page.on("request", (request) => {
    record.requests.push({ at: Date.now(), method: request.method(), url: new URL(request.url()) });
  });
  page.on("websocket", (socket) => {
    if (new URL(socket.url()).pathname !== "/ws/live") return;
    socket.on("framesent", (event) => {
      record.sent.push({ at: Date.now(), frame: liveFrame<LiveClientFrame>(event.payload) });
    });
    socket.on("framereceived", (event) => {
      record.received.push({ at: Date.now(), frame: liveFrame<LiveServerFrame>(event.payload) });
    });
  });
  return record;
}

/** One view the page subscribed: its subscription, its kind, the project and eval it names, its
 *  query and when the page sent it. A growth of a view sends its `sub` again. */
export interface SubscribedView {
  readonly at: number;
  readonly sub: string;
  readonly kind: LiveView["kind"];
  readonly project: string;
  readonly slug: string;
  readonly query: URLSearchParams;
}

/** Every subscribe frame `frames` holds, in order. */
export function subscribedViews(
  frames: readonly { readonly at: number; readonly frame: LiveClientFrame }[]
): SubscribedView[] {
  return frames.flatMap(({ at: sentAt, frame }) => {
    if (frame.type !== "subscribe") return [];
    const view = frame.view;
    return [
      {
        at: sentAt,
        kind: view.kind,
        project: "project" in view ? view.project : "",
        query: new URLSearchParams("query" in view ? view.query : ""),
        slug: "slug" in view ? view.slug : "",
        sub: frame.sub,
      },
    ];
  });
}

/** The evals views of `project` the page subscribed, each naming the kind of eval it reads in its
 *  query's `kind`. */
export function evalsViews(record: LiveRecord, project: string): SubscribedView[] {
  return subscribedViews(record.sent).filter(
    (view) => view.kind === "evals" && view.project === project
  );
}

/** The snapshots `frames` holds for `sub`, in order. */
export function snapshotsOf(
  frames: readonly { readonly at: number; readonly frame: LiveServerFrame }[],
  sub: string
): { at: number; frame: DeliveryLiveSnapshot }[] {
  return frames.flatMap(({ at: receivedAt, frame }) =>
    frame.type === "snapshot" && frame.sub === sub ? [{ at: receivedAt, frame }] : []
  );
}

/** The deltas `frames` holds for `sub`, in order. */
export function deltasOf(
  frames: readonly { readonly at: number; readonly frame: LiveServerFrame }[],
  sub: string
): { at: number; frame: DeliveryLiveDelta }[] {
  return frames.flatMap(({ at: receivedAt, frame }) =>
    frame.type === "delta" && frame.sub === sub ? [{ at: receivedAt, frame }] : []
  );
}

/** The evals an evals view of `sub` holds now, by id, as its snapshot and its deltas since have
 *  left them: a delta's `items.items` upserts and removes evals by id (DeliveryLiveDelta). An evals
 *  view's frames carry the list route's body (`DeliveryLiveBodies.evals`, web/src/api/queries.ts),
 *  so its lists are evals. Empty before its first snapshot. */
export function evalsViewItems(record: LiveRecord, sub: string): Map<string, EvalListItem> {
  const items = new Map<string, EvalListItem>();
  for (const { frame } of record.received) {
    if (frame.type === "snapshot" && frame.sub === sub) {
      items.clear();
      // An evals view's snapshot body is the list route's (`DeliveryLiveBodies.evals`).
      const body = frame.body as EvalListResponse;
      for (const item of body.items ?? []) items.set(item.id, item);
    } else if (frame.type === "delta" && frame.sub === sub) {
      const changes = frame.items.items;
      for (const item of (changes?.upsert ?? []) as EvalListItem[]) items.set(item.id, item);
      for (const id of changes?.remove ?? []) items.delete(id);
    }
  }
  return items;
}

/** The window figure `statistic` of `series` of the eval `slug` among `items`
 *  (`evalsViewItems`): null where they hold no such figure. */
export function windowFigure(
  items: ReadonlyMap<string, EvalListItem>,
  slug: string,
  series: string,
  statistic: string
): number | null {
  const item = [...items.values()].find((candidate) => candidate.definition.slug === slug);
  return item?.series?.find((values) => values.series === series)?.window?.[statistic] ?? null;
}

// ---------------------------------------------------------------------------------------------
// The group links (plan D13; spec R8).

/** The Legion charts whose table rows link to the issues each row counts: their tables are drawn
 *  only once the reader turns each chart to its table. */
const LINKED_LEGION_CHARTS = ["legion-work-time", "legion-rework", "legion-phase-time"] as const;

/** EVERY_SOURCE's tab over its default window with every group link but the P0 card's drawn: the
 *  Legion headlines, and the per-day and phase rows of the linked Legion charts, each turned to its
 *  table. The P0 card draws its link over a window of its own (`showP0Link`). */
export async function showGroupLinks(page: Page): Promise<void> {
  await openTab(page, EVERY_SOURCE);
  await paintedSection(page, "cards");
  const legion = await paintedSection(page, "legion");
  for (const slug of LINKED_LEGION_CHARTS) {
    await showChart(legion.locator(`[data-eval="${slug}"] figure[data-chart="${slug}"]`), "Table");
  }
}

/** One group link of the tab: the locator that finds it again on a fresh tab, and the count of
 *  issues it names. */
export interface GroupLink {
  readonly selector: string;
  readonly count: number;
}

/** Holds, before a row reads a group link, that Dispatch counted the Legion work the seed wrote:
 *  the facts loop stored each issue's facts (issue_facts), the refresher wrote the Legion evals'
 *  rows of their close days (eval_days), and the phase bars' issue counts stand over the tab's
 *  default window (expectNumberStands), so a link that is missing names which part of the seed's
 *  precondition failed. */
export async function expectLegionWork(): Promise<void> {
  try {
    // A whole read that began before the seed committed records the project as read with none
    // of its issues, so each reading here takes back a record of no row and wakes the loop
    // again, until the facts stand; a record of the rows, once written, is left as it is.
    await expect
      .poll(
        () =>
          sql(
            `DELETE FROM issue_facts_computed WHERE project_key = ${lit(EVERY_SOURCE)} AND computed_rows = 0`,
            `SELECT pg_notify('dispatch_legion_state', ${lit(EVERY_SOURCE)})`,
            `SELECT count(*) FROM issue_facts WHERE project_key = ${lit(EVERY_SOURCE)} AND closed_at IS NOT NULL`
          ),
        {
          message: `the facts loop stored the facts of the ${LEGION_WORK.length} issues Legion worked`,
          timeout: 30_000,
        }
      )
      .toMatch(new RegExp(`(^|\\n)${LEGION_WORK.length}$`));
  } catch (error) {
    // What the facts loop had to read and where it stands, so the failure says which input it
    // did not take: the seeded sessions' titles, events and handoffs, its cursors and its record
    // of the projects it read whole, and the event log's safe horizon.
    const state = await sql(
      `SELECT concat_ws(' | ',
         (SELECT count(*) FROM session_titles WHERE session_id LIKE 'e2e-legion-%') || ' titles',
         (SELECT count(*) FROM events WHERE actor ->> 'id' LIKE 'e2e-legion-%') || ' events',
         (SELECT count(*) FROM handoffs WHERE issue_key LIKE ${lit(`${EVERY_SOURCE}-2%`)}) || ' handoffs',
         'cursor ' || coalesce((SELECT string_agg(row_to_json(c)::text, '; ') FROM issue_facts_cursor c), 'none'),
         'computed ' || coalesce((SELECT string_agg(row_to_json(c)::text, '; ') FROM issue_facts_computed c), 'none'),
         'horizon ' || coalesce((SELECT string_agg(row_to_json(h)::text, '; ') FROM event_horizon h), 'none'),
         'newest event ' || (SELECT coalesce(max(id), 0) FROM events))`
    );
    throw new Error(`${String(error)}\nthe facts loop's inputs and state: ${state}`);
  }
  await expect
    .poll(
      () =>
        sql(
          `SELECT count(DISTINCT day) FROM eval_days
            WHERE eval_id = ${lit(`${EVERY_SOURCE}/legion-work-time`)} AND n > 0`
        ),
      {
        message: "the refresher wrote legion-work-time's rows of every close day",
        timeout: 30_000,
      }
    )
    .toBe(String(new Set(LEGION_WORK.map(({ day }) => day)).size));
  // The tab's default window, the 28 UTC days ending today, which showGroupLinks opens: each phase
  // bar's issue count stands there once the Legion walk has built its days.
  const today = new Date().toISOString().slice(0, 10);
  await expectNumberStands(
    EVERY_SOURCE,
    { from: `${dayAfter(today, -27)}T00:00:00Z`, to: `${dayAfter(today, 1)}T00:00:00Z` },
    "legion",
    "legion-phase-time",
    "implementer",
    "issues"
  );
}

/** Every group link `showGroupLinks` draws, once at least `minimum` are drawn, each found by the
 *  box it stands in (a card's `data-card`, a chart's `data-eval`) and the table row it stands in,
 *  or none for a headline's. */
export async function groupLinks(page: Page, minimum: number): Promise<GroupLink[]> {
  const drawnLinks = () =>
    page.locator("main button[data-group-count]").evaluateAll((buttons) =>
      buttons.map((button) => {
        const card = button.closest("[data-card]")?.getAttribute("data-card");
        const box = button.closest("[data-eval]")?.getAttribute("data-eval");
        const row = button.closest("tr[data-row]")?.getAttribute("data-row");
        const owner = card != null ? `[data-card="${card}"]` : `[data-eval="${box ?? ""}"]`;
        return {
          count: Number(button.getAttribute("data-group-count")),
          selector:
            row != null
              ? `${owner} tr[data-row="${row}"] button[data-group-count]`
              : `${owner} > div > button[data-group-count]`,
        };
      })
    );
  try {
    await expect.poll(async () => (await drawnLinks()).length).toBeGreaterThanOrEqual(minimum);
  } catch (error) {
    throw new Error(`${String(error)}\nthe links drawn: ${JSON.stringify(await drawnLinks())}`);
  }
  return drawnLinks();
}

/** Presses `link` on the tab `showGroupLinks` (or, for the P0 card's, `showP0Link`) drew, and holds
 *  that the issue table it opens lists exactly the issues the link counted (`only=`, plan D13). */
export async function openGroupLink(page: Page, link: GroupLink): Promise<void> {
  await page.locator(link.selector).click();
  await expect(page).toHaveURL(/\/issues\?(.*&)?only=/);
  await expect(page.locator("[data-issue-row]"), link.selector).toHaveCount(link.count);
}

/** The P0 card's group link, drawn on EVERY_SOURCE's tab over the days from unowned-p0's first day
 *  to today, once that tab shows exactly one. unowned-p0 counts the P0 issues open with no owner
 *  now, so the refresher writes its row for the day it runs, and on a freshly reset database its
 *  input begins today: over the tab's default window, which starts 27 days before, the card reads
 *  "no data before <that day>" and draws no link, as spec R18 says it must. The first day is the
 *  one the evals answer names (`data_since`), read once the refresher has written it. */
export async function showP0Link(page: Page): Promise<GroupLink> {
  const today = new Date().toISOString().slice(0, 10);
  const to = `${dayAfter(today, 1)}T00:00:00Z`;
  const firstDay = async () => {
    const listed = await listEvals(
      EVERY_SOURCE,
      { from: `${dayAfter(today, -27)}T00:00:00Z`, to },
      "&kind=delivery"
    );
    const item = (listed.items ?? []).find((one) => one.definition.slug === "unowned-p0");
    return item?.series?.find((one) => one.series === "issues")?.data_since ?? null;
  };
  await expect
    .poll(firstDay, { message: "the refresher wrote unowned-p0's first day", timeout: 30_000 })
    .not.toBeNull();
  const since = await firstDay();
  await openTab(page, EVERY_SOURCE, windowQuery({ from: `${since}T00:00:00Z`, to }));
  const cards = await paintedSection(page, "cards");
  await expect(cards.locator('[data-card="unowned-p0"] button[data-group-count]')).toHaveCount(1);
  // Read after the P0 card's link is drawn above, so groupLinks finds it.
  const link = (await groupLinks(page, 1)).find((one) =>
    one.selector.startsWith('[data-card="unowned-p0"]')
  );
  if (link === undefined) throw new Error(`the P0 card drew no group link from ${since}`);
  return link;
}

// ---------------------------------------------------------------------------------------------
// The widths a table's headers draw at (plan D5; ledger item 5).

/** The headers of `table` that draw their label, their filter button or their sort mark cut: the
 *  header cell's content wider than the cell, or the label's button wider than the room it is
 *  given. One reading, each named by its text. */
export function headersCut(table: Locator): Promise<string[]> {
  return table.locator("th").evaluateAll((headers) =>
    headers
      .filter((header) => {
        const content = header.firstElementChild;
        const label = header.querySelector("button");
        return (
          (content !== null && content.scrollWidth > header.clientWidth) ||
          (label !== null && label.scrollWidth > label.clientWidth)
        );
      })
      .map((header) => header.textContent ?? "")
  );
}

/** The share of a desktop box the issue table's pinned columns stay within
 *  (`ISSUE_TABLE_COLUMNS.pins.wide.share`, web/src/features/project/issues-table-model.ts). */
export const PINNED_SHARE = 0.6;

/** Each table of the page whose pinned columns (a header the layout pins draws at a `left` of its
 *  own) are more than one and cover more than PINNED_SHARE of the box they scroll in, measured as
 *  drawn, after the headers' natural widths are applied (ledger item 5): the table's label and the
 *  widths. One reading. */
export function pinnedOverShare(page: Page): Promise<string[]> {
  return page.locator("main table").evaluateAll(
    (tables, share) =>
      tables.flatMap((table) => {
        const pinned = [...table.querySelectorAll("thead th")].filter(
          (header): header is HTMLElement =>
            header instanceof HTMLElement && header.style.left !== ""
        );
        const box = table.closest("[data-testid]");
        if (pinned.length < 2 || !(box instanceof HTMLElement)) return [];
        const covered = pinned.reduce(
          (sum, header) => sum + header.getBoundingClientRect().width,
          0
        );
        return covered > box.clientWidth * share + 1
          ? [`${table.getAttribute("aria-label") ?? ""}: ${covered} of ${box.clientWidth}`]
          : [];
      }),
    PINNED_SHARE
  );
}
