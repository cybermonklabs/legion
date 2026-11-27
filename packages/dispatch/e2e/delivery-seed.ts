// The delivery pages' e2e fixture, written directly in Postgres: the timeline, the measures and
// the Pipeline page read only stored facts, so these are the rows intake and reconcile would have
// written, without a live GitHub App. One module so the timeline rows, the measures rows, the
// Pipeline rows and the docs screenshots read the same data. acme/widgets-shaped placeholders
// throughout (AGENTS.md: "This repository is public").
//
// Each fixture is written in a place (`DeliveryPlace`). Called with none, it is written as it
// always was, the delivery settings with it, for a caller that has emptied the database first:
// the review corpus (e2e/review-seed.ts) and the docs media harness (docs/site/media/harness.ts),
// whose recordings and screenshots were made under these numbers and days. A real-stack row
// writes it in the place the `delivery` fixture of e2e/test.ts hands it, on the settings that
// fixture wrote.
//
// The timeline's, the session groups' and the measures' fixtures each insert their place's project
// with no `ON CONFLICT`, so a place takes one of them, and a second insert of its project fails
// the row rather than being skipped: a row's place is under its own key (`rowKey`, e2e/test.ts),
// and the corpus's is written once into each database its caller emptied.
import { fixedKey, type RowKey } from "./api";
import { sql } from "./psql";

const SEED_ACTOR = `'{"kind":"system","id":"e2e-seed"}'`;

/** Every column of the delivery settings row a write here sets: all but `singleton`, which is
 *  always true, and `updated_at`, which the write stamps. */
const SETTINGS_COLUMNS = [
  "deploy_repo",
  "deploy_workflow_path",
  "production_job_names",
  "production_apply_step_name",
  "pr_checks_workflow_path",
  "population_authors",
  "excluded_repos",
  "staging_e2e_job_name",
  "staging_apply_job_name",
  "pr_checks_rollup_job_name",
  "last_event_at",
  "last_reconcile_at",
  "last_error",
  "updated_by",
] as const;

/** The settings row `source` names - a `VALUES` or a `SELECT` of `singleton` and every column of
 *  `SETTINGS_COLUMNS` - written whole, whether a settings row stands or not: `delivery_settings`
 *  holds one row (`singleton`), so a write is an upsert that sets every column, and a column the
 *  source leaves null goes back to null. */
function upsertSettings(source: string): string {
  return `INSERT INTO delivery_settings (singleton, ${SETTINGS_COLUMNS.join(", ")})
   ${source}
   ON CONFLICT (singleton) DO UPDATE SET
     ${SETTINGS_COLUMNS.map((column) => `${column} = excluded.${column}`).join(", ")},
     updated_at = now()`;
}

/** The delivery settings every fixture here is read under: acme/widgets' deploy and PR-checks
 *  workflows, its one production job and octocat's pull requests, with no staging job, no
 *  freshness and no error. The review corpus and the docs harness write them onto the database
 *  they emptied, and the `delivery` fixture of e2e/test.ts writes them for the delivery rows. */
const SETTINGS = upsertSettings(`VALUES (
     true, 'acme/widgets', '.github/workflows/deploy.yml', ARRAY['widgets-release / widgets-release'], null,
     '.github/workflows/pr-checks.yml', ARRAY['octocat'], ARRAY[]::text[], null,
     null, null, null, null,
     null, ${SEED_ACTOR}
   )`);

/** Writes `SETTINGS` over whatever settings row stands, or none: what the `delivery` fixture of
 *  e2e/test.ts writes for the delivery rows, and puts back after each of them. */
export async function writeDeliverySettings(): Promise<void> {
  await sql(SETTINGS);
}

/** The settings row as it stands, as JSON, or "" when there is none: what
 *  `putDeliverySettings` writes back. */
export function readDeliverySettings(): Promise<string> {
  return sql("SELECT row_to_json(settings) FROM delivery_settings AS settings");
}

/** Writes back, whole, a settings row `readDeliverySettings` read. */
export async function putDeliverySettings(saved: string): Promise<void> {
  await sql(
    upsertSettings(
      `SELECT true, ${SETTINGS_COLUMNS.join(", ")}
       FROM json_populate_record(null::delivery_settings, $saved$${saved}$saved$::json)`
    )
  );
}

/** The pull requests, runs and session groups the delivery tables hold, each by its key: what a
 *  row holds at its start (`deliveryRowsHeld`), so the ones it holds at its end and not here are
 *  the ones it wrote (`deleteDeliveryRowsBut`). */
export interface DeliveryRows {
  readonly groups: readonly string[];
  readonly pulls: readonly string[];
  readonly runs: readonly string[];
}

/** The key of every pull request (`repo#number`), run (`repo#run_id`) and session group (`id`)
 *  the delivery tables hold. */
export async function deliveryRowsHeld(): Promise<DeliveryRows> {
  return JSON.parse(
    await sql(`SELECT json_build_object(
      'groups', coalesce((SELECT json_agg(id::text) FROM delivery_session_groups), '[]'),
      'pulls', coalesce((SELECT json_agg(repo || '#' || number) FROM delivery_pull_requests), '[]'),
      'runs', coalesce((SELECT json_agg(repo || '#' || run_id) FROM delivery_runs), '[]'))`)
  ) as DeliveryRows;
}

/** Deletes every pull request, run and session group the delivery tables hold but `held`, and
 *  every issue of `project` with its events and read state, in one statement: what a row's
 *  delivery fixtures wrote, so that the delivery tables hold again what the row started on.
 *
 *  A run's jobs go with it and a group's members with it (both `ON DELETE CASCADE`). The issues
 *  are the ones the fixtures wrote into the row's own project, which the measures read beside
 *  every other project's (`ListUnownedP0`, "P0 issues with no owner"), so a row's P0 would
 *  otherwise stand in the next row's list. An issue's events and read state reference it with no
 *  action (migration 0001), which is checked at the end of the statement, as the reset's own
 *  delete relies on (`resetDatabaseOnce`, e2e/seed.ts); a pull request's issue is set null, and
 *  the pull request is deleted beside it anyway. `held` travels as one dollar-quoted JSON
 *  literal: its keys are the database's own, and none holds that quote. */
export async function deleteDeliveryRowsBut(held: DeliveryRows, project: string): Promise<void> {
  await sql(`WITH held AS (
  SELECT $held$${JSON.stringify(held)}$held$::jsonb AS rows
), pulls AS (
  DELETE FROM delivery_pull_requests
  WHERE repo || '#' || number NOT IN (SELECT jsonb_array_elements_text(rows -> 'pulls') FROM held)
), runs AS (
  DELETE FROM delivery_runs
  WHERE repo || '#' || run_id NOT IN (SELECT jsonb_array_elements_text(rows -> 'runs') FROM held)
), groups AS (
  DELETE FROM delivery_session_groups
  WHERE id::text NOT IN (SELECT jsonb_array_elements_text(rows -> 'groups') FROM held)
), written AS (
  SELECT key FROM issues WHERE project_key = '${project}'
), issue_events AS (
  DELETE FROM events WHERE issue_key IN (SELECT key FROM written)
), reads AS (
  DELETE FROM user_issue_state WHERE issue_key IN (SELECT key FROM written)
)
DELETE FROM issues WHERE key IN (SELECT key FROM written)`);
}

/** The timeline fixture's day, as `TIMELINE_FIXTURE_QUERY` spans it. */
const TIMELINE_FROM = "2024-06-01T00:00:00Z";
const TIMELINE_TO = "2024-06-02T00:00:00Z";

/** The Pipeline page's pinned "today" (`?at=`): the e2e rows and the docs screenshot show the
 *  seven days ending on its day, 2026-09-26 to 2026-10-02 in the review corpus's place. */
export const PIPELINE_AT = "2026-10-02T12:00:00Z";

/** How many days before the review corpus's a row's place writes the timeline's and the
 *  Pipeline's fixtures: 52 weeks, so a row's timeline day (2023-06-03) and Pipeline week (Sep 27
 *  to Oct 3, 2025) are days the corpus writes nothing on, a whole number of weeks from the
 *  corpus's own, and far from the last 120 days the rows that open `/delivery` on its default
 *  window read. */
export const ROW_DAYS_EARLIER = 364;

/** The measures fixture's window (packages/envoy/internal/dispatch/delivery/measures/testdata/
 *  dora-fixture.json, the prototype's own web/fixtures/dataset.json with its names replaced), in
 *  the corpus's place. */
export const FIXTURE_WINDOW = { from: "2026-08-30T00:00:00Z", to: "2026-09-27T20:00:00Z" } as const;

/** How many days after the corpus's place a row's place writes the measures fixture: five weeks,
 *  so its window (2026-10-04 to 2026-11-01) starts after the last deploy the review corpus writes,
 *  its Pipeline's on 2026-10-02 (`seedPipelineRows`), and each of its days is the weekday it is in
 *  the corpus's place. An earlier place, as the timeline's is, would not keep the corpus out: the
 *  measures look for a pull request's deploy among every deploy from the window's start on, with
 *  no end (`readDeliveryRows`, packages/envoy/internal/dispatch/api/delivery.go), so a deploy the
 *  corpus wrote after the window ships the fixture's last pull request, which none of the
 *  fixture's own deploys ships, and moves the window's merge-to-production median. */
export const MEASURES_DAYS_LATER = 35;

/** Where a fixture is written: the project its issues are in, the number it gives each of its
 *  pull requests and runs, and the moment it gives each row of the timeline, the Pipeline and the
 *  measures. A caller that names no place writes each fixture as this module spells it, in
 *  project `ACME`; a row's place (the `delivery` fixture of e2e/test.ts) writes the row's own
 *  project, numbers neither another row nor the review corpus writes, the timeline and the
 *  Pipeline `ROW_DAYS_EARLIER` days earlier and the measures `MEASURES_DAYS_LATER` days later, so
 *  the window a row reads holds its own rows and none of the corpus's, which a worker keeps
 *  beside them (the `reviewSeed` fixture). */
export interface DeliveryPlace {
  /** The key of the project the fixture's issues are in. */
  readonly project: RowKey;
  /** The number the place writes for the pull request or run the fixture numbers `fixed` (#1,
   *  run 500). Every number this module and its callers write is below 10,000, so a row's place
   *  adds a multiple of 10,000 and keeps the fixture's own number in the last four digits. */
  readonly number: (fixed: number) => number;
  /** The moment the place writes for the timeline's or the Pipeline's moment `iso`, spelled the
   *  same way (`YYYY-MM-DDTHH:MM:SSZ`). */
  readonly at: (iso: string) => string;
  /** The moment the place writes for the measures fixture's moment `iso`, spelled the same
   *  way. */
  readonly measuresAt: (iso: string) => string;
  /** The window `/delivery` is opened on to see the place's timeline fixture: the place's day,
   *  as `TIMELINE_FIXTURE_QUERY` spans the corpus's. */
  readonly timelineQuery: string;
  /** The window `/delivery` and its measures are read in to see the place's measures fixture:
   *  `FIXTURE_WINDOW` at the place's moments for it, as a query string without the leading
   *  "?". */
  readonly measuresQuery: string;
  /** The Pipeline page's pinned "today" (`?at=`) for the place's Pipeline rows: `PIPELINE_AT`
   *  at the place's moment for it. */
  readonly pipelineAt: string;
}

/** `iso` moved `days` days on, or back where `days` is below zero, spelled as `iso` is
 *  (`YYYY-MM-DDTHH:MM:SSZ`). */
function daysOn(iso: string, days: number): string {
  return new Date(Date.parse(iso) + days * 24 * 60 * 60 * 1000).toISOString().replace(".000Z", "Z");
}

/** The place in `project` that adds `numbers` to each pull request's and run's number, writes
 *  the timeline and the Pipeline `daysEarlier` days before the review corpus does, and the
 *  measures `measuresDaysLater` days after. */
export function deliveryPlace(
  project: RowKey,
  numbers: number,
  daysEarlier: number,
  measuresDaysLater: number
): DeliveryPlace {
  const at = (iso: string) => daysOn(iso, -daysEarlier);
  const measuresAt = (iso: string) => daysOn(iso, measuresDaysLater);
  return {
    at,
    measuresAt,
    measuresQuery: `from=${encodeURIComponent(measuresAt(FIXTURE_WINDOW.from))}&to=${encodeURIComponent(measuresAt(FIXTURE_WINDOW.to))}`,
    number: (fixed) => numbers + fixed,
    pipelineAt: at(PIPELINE_AT),
    project,
    timelineQuery: `from=${encodeURIComponent(at(TIMELINE_FROM))}&to=${encodeURIComponent(at(TIMELINE_TO))}`,
  };
}

/** The place a fixture is written in when its caller names none. */
const corpusPlace = deliveryPlace(
  fixedKey("ACME", "the review corpus and the docs pictures write it into a database they emptied"),
  0,
  0,
  0
);

/** The slice 1 timeline's fixture, on the place's day (2024-06-01 in the corpus's): two
 *  population pull requests (#1 shipped by deploy run 500 and attributed to `<project>-1`, a P0
 *  issue; #2 waiting, a rework, with no issue), the deploy run with its jobs, and a failed run
 *  (501). With no place it is the review corpus's (e2e/review-seed.ts), in project `ACME`, and
 *  writes the delivery settings first; a real-stack row passes its own place. */
export async function seedDeliveryFixture(place?: DeliveryPlace): Promise<void> {
  const { at, number, project } = place ?? corpusPlace;
  await sql(
    ...(place === undefined ? [SETTINGS] : []),
    // Every project names a default assignee, so the column takes none of these rows without
    // one; alice is the person every row of this fixture is read as.
    `INSERT INTO projects (key, name, default_assignee) VALUES ('${project}', 'Acme', 'alice')`,
    `INSERT INTO issues (key, project_key, number, title, status, priority, created_by, rank)
     VALUES ('${project}-1', '${project}', 1, 'Ship the widgets', 'todo', 0, ${SEED_ACTOR}, 'U')`,
    `INSERT INTO delivery_pull_requests (
       repo, number, title, url, author, created_at, merged_at, additions, deletions, rework, issue_key, sessions, partial
     ) VALUES
       ('acme/widgets', ${number(1)}, 'feat: a shipped widget', 'https://github.com/acme/widgets/pull/${number(1)}', 'octocat',
        '${at("2024-06-01T00:00:00Z")}', '${at("2024-06-01T01:00:00Z")}', 12, 3, false, '${project}-1', ARRAY[]::text[], false),
       ('acme/widgets', ${number(2)}, 'fix: a waiting widget', 'https://github.com/acme/widgets/pull/${number(2)}', 'octocat',
        '${at("2024-06-01T02:00:00Z")}', '${at("2024-06-01T03:00:00Z")}', 4, 1, true, null, ARRAY[]::text[], false)`,
    `INSERT INTO delivery_runs (
       repo, run_id, kind, head_sha, head_commit_at, started_at, created_at, completed_at, conclusion, url, head_branch, event
     ) VALUES
       ('acme/widgets', ${number(500)}, 'deploy', 'deadbeef', '${at("2024-06-01T01:00:00Z")}',
        '${at("2024-06-01T01:30:00Z")}', '${at("2024-06-01T01:30:00Z")}', '${at("2024-06-01T01:40:00Z")}', 'success', 'https://github.com/acme/widgets/actions/runs/${number(500)}', 'main', 'push'),
       ('acme/widgets', ${number(501)}, 'deploy', 'cafef00d', '${at("2024-06-01T03:00:00Z")}',
        '${at("2024-06-01T12:00:00Z")}', '${at("2024-06-01T12:00:00Z")}', '${at("2024-06-01T12:20:00Z")}', 'failure', 'https://github.com/acme/widgets/actions/runs/${number(501)}', 'main', 'push')`,
    `INSERT INTO delivery_run_jobs (repo, run_id, name, started_at, completed_at, conclusion) VALUES
       ('acme/widgets', ${number(500)}, 'build', '${at("2024-06-01T01:30:00Z")}', '${at("2024-06-01T01:32:00Z")}', 'success'),
       ('acme/widgets', ${number(500)}, 'widgets-release / widgets-release', '${at("2024-06-01T01:32:00Z")}', '${at("2024-06-01T01:40:00Z")}', 'success'),
       ('acme/widgets', ${number(501)}, 'build', '${at("2024-06-01T12:00:00Z")}', '${at("2024-06-01T12:05:00Z")}', 'success'),
       ('acme/widgets', ${number(501)}, 'integration tests', '${at("2024-06-01T12:05:00Z")}', '${at("2024-06-01T12:20:00Z")}', 'failure'),
       ('acme/widgets', ${number(501)}, 'widgets-release / widgets-release', null, null, 'skipped')`
  );
}

/** The window `/delivery` is opened on to see `seedDeliveryFixture()`'s rows, as a query string
 *  without the leading "?": it spans the day that fixture writes its pull requests and runs in.
 *  It is read from here by `e2e/review-seed.ts`, which hands it to the review layers, as a row
 *  reads its own place's (`DeliveryPlace.timelineQuery`), rather than either writing it out,
 *  because a window that no longer spans the rows fails nothing: `/delivery` still draws its
 *  heading, so a readiness locator passes and the baseline silently becomes an empty chart. */
export const TIMELINE_FIXTURE_QUERY = corpusPlace.timelineQuery;

/** The session-groups fixture, relative to now so the window control's presets see it: two
 *  population pull requests merged in the last seven days, first written by sessions
 *  `e2e-planner-1` and `e2e-planner-2`, and one merged 20 days ago, first written by
 *  `e2e-reviewer`. Each session carries the title Dispatch learns it by: an event the session
 *  wrote with its origin's `session_title` (delivery/agents.go's ResolveSessionTitles). The
 *  events are on issue 7 of the place's project, and the pull requests take the place's numbers
 *  for #11 to #13. Only a real-stack row writes it, in its own place. */
export async function seedSessionGroupsFixture({ number, project }: DeliveryPlace): Promise<void> {
  await sql(
    `INSERT INTO projects (key, name, default_assignee) VALUES ('${project}', 'Acme', 'alice')`,
    `INSERT INTO issues (key, project_key, number, title, status, priority, created_by, rank)
     VALUES ('${project}-7', '${project}', 7, 'Group the planners', 'todo', 1, ${SEED_ACTOR}, 'U')`,
    `INSERT INTO delivery_pull_requests (
       repo, number, title, url, author, created_at, merged_at, additions, deletions, rework, issue_key, sessions, partial
     ) VALUES
       ('acme/widgets', ${number(11)}, 'feat: plan the widgets', 'https://github.com/acme/widgets/pull/${number(11)}', 'octocat',
        now() - interval '2 days 1 hour', now() - interval '2 days', 10, 2, false, null, ARRAY['e2e-planner-1'], false),
       ('acme/widgets', ${number(12)}, 'feat: plan the gadgets', 'https://github.com/acme/widgets/pull/${number(12)}', 'octocat',
        now() - interval '3 days 1 hour', now() - interval '3 days', 8, 1, false, null, ARRAY['e2e-planner-2'], false),
       ('acme/widgets', ${number(13)}, 'feat: review the widgets', 'https://github.com/acme/widgets/pull/${number(13)}', 'octocat',
        now() - interval '20 days 1 hour', now() - interval '20 days', 5, 5, false, null, ARRAY['e2e-reviewer'], false)`,
    `INSERT INTO events (issue_key, seq, type, actor, payload, notify, created_at) VALUES
       ('${project}-7', 1, 'message.created',
        '{"kind":"session","id":"e2e-planner-1","origin":{"session_title":"Planner, first run"}}', '{}', false, now() - interval '4 days'),
       ('${project}-7', 2, 'message.created',
        '{"kind":"session","id":"e2e-planner-2","origin":{"session_title":"Planner, second run"}}', '{}', false, now() - interval '4 days'),
       ('${project}-7', 3, 'message.created',
        '{"kind":"session","id":"e2e-reviewer","origin":{"session_title":"Reviewer"}}', '{}', false, now() - interval '21 days')`
  );
}

/** The measures fixture: the prototype fixture's 14 population pull requests and 8 deploy runs,
 *  each run's production job (`widgets-release / widgets-release`) and failed jobs, and the four
 *  issues of the place's project, issue 103 the one open P0 with neither a claim nor a route.
 *  With no place it is the docs media harness's (docs/site/media/harness.ts), in project `ACME`
 *  and in `FIXTURE_WINDOW`, and writes the delivery settings first; a real-stack row passes its
 *  own place, whose numbers its pull requests and runs take and whose moments for the measures
 *  (`measuresAt`, `MEASURES_DAYS_LATER`) they are written at. Every run is a push on main. Its
 *  expected measures are the prototype's own computeDora with each deploy derived by its own
 *  containment (LEGION-567 slice 2 plan, "How the expected values are produced"). */
export async function seedMeasuresFixture(place?: DeliveryPlace): Promise<void> {
  const { measuresAt: at, number, project } = place ?? corpusPlace;
  await sql(
    ...(place === undefined ? [SETTINGS] : []),
    `INSERT INTO projects (key, name, default_assignee) VALUES ('${project}', 'Acme', 'alice')`,
    `INSERT INTO issues (key, project_key, number, title, status, priority, route, claimed_by, claimed_at, created_by, rank) VALUES
       ('${project}-100', '${project}', 100, 'Build timeline UI', 'in_progress', 1, null,
        '{"kind":"session","id":"01a1-e2e-session"}', now(), ${SEED_ACTOR}, 'U100'),
       ('${project}-101', '${project}', 101, 'DORA metrics module', 'in_progress', 2, 'role:e2e', null, null, ${SEED_ACTOR}, 'U101'),
       ('${project}-102', '${project}', 102, 'Collector schema', 'done', 1, null, null, null, ${SEED_ACTOR}, 'U102'),
       ('${project}-103', '${project}', 103, 'Production deploy gate flakes', 'triage', 0, null, null, null, ${SEED_ACTOR}, 'U103')`,
    `INSERT INTO delivery_pull_requests (
       repo, number, title, url, author, created_at, merged_at, first_commit_at,
       additions, deletions, rework, issue_key, sessions, partial
     ) VALUES
       ('acme/widgets', ${number(101)}, 'Add timeline zoom brush', 'https://github.com/acme/widgets/pull/${number(101)}', 'octocat',
        '${at("2026-09-04T18:00:00Z")}', '${at("2026-09-05T12:00:00Z")}', '${at("2026-09-04T15:00:00Z")}', 210, 12, false, '${project}-100', ARRAY[]::text[], false),
       ('acme/widgets', ${number(102)}, 'fix: timeline brush jumps on resize', 'https://github.com/acme/widgets/pull/${number(102)}', 'octocat',
        '${at("2026-09-06T09:00:00Z")}', '${at("2026-09-06T13:00:00Z")}', '${at("2026-09-06T08:30:00Z")}', 18, 4, true, '${project}-100', ARRAY[]::text[], false),
       ('acme/widgets', ${number(103)}, 'Add DORA lead-time calc', 'https://github.com/acme/widgets/pull/${number(103)}', 'octocat',
        '${at("2026-09-07T10:00:00Z")}', '${at("2026-09-07T16:00:00Z")}', '${at("2026-09-07T08:00:00Z")}', 140, 6, false, '${project}-101', ARRAY[]::text[], false),
       ('acme/widgets', ${number(104)}, 'Add facet filtering module', 'https://github.com/acme/widgets/pull/${number(104)}', 'octocat',
        '${at("2026-09-08T09:00:00Z")}', '${at("2026-09-08T15:00:00Z")}', '${at("2026-09-08T07:00:00Z")}', 95, 3, false, '${project}-100', ARRAY[]::text[], false),
       ('acme/widgets', ${number(105)}, 'revert: Add facet filtering module', 'https://github.com/acme/widgets/pull/${number(105)}', 'octocat',
        '${at("2026-09-09T08:00:00Z")}', '${at("2026-09-09T09:00:00Z")}', '${at("2026-09-09T07:45:00Z")}', 3, 95, true, '${project}-100', ARRAY[]::text[], false),
       ('acme/widgets', ${number(106)}, 'Add drill-down panel', 'https://github.com/acme/widgets/pull/${number(106)}', 'octocat',
        '${at("2026-09-10T09:00:00Z")}', '${at("2026-09-10T17:00:00Z")}', '${at("2026-09-10T08:00:00Z")}', 180, 20, false, '${project}-100', ARRAY[]::text[], false),
       ('acme/widgets', ${number(107)}, 'hotfix: drill-down panel crash on null issue', 'https://github.com/acme/widgets/pull/${number(107)}', 'octocat',
        '${at("2026-09-11T10:00:00Z")}', '${at("2026-09-11T11:00:00Z")}', '${at("2026-09-11T09:45:00Z")}', 9, 2, true, '${project}-100', ARRAY[]::text[], false),
       ('acme/gadgets', ${number(20)}, 'Add dispatch API client', 'https://github.com/acme/gadgets/pull/${number(20)}', 'octocat',
        '${at("2026-09-12T09:00:00Z")}', '${at("2026-09-12T14:00:00Z")}', '${at("2026-09-12T08:00:00Z")}', 60, 0, false, '${project}-102', ARRAY[]::text[], false),
       ('acme/widgets', ${number(108)}, 'Wire SSE events endpoint', 'https://github.com/acme/widgets/pull/${number(108)}', 'octocat',
        '${at("2026-09-13T07:00:00Z")}', '${at("2026-09-13T08:00:00Z")}', '${at("2026-09-13T06:00:00Z")}', 75, 5, false, '${project}-100', ARRAY[]::text[], false),
       ('acme/widgets', ${number(109)}, 'Add priority facet counts', 'https://github.com/acme/widgets/pull/${number(109)}', 'octocat',
        '${at("2026-09-13T08:00:00Z")}', '${at("2026-09-13T08:30:00Z")}', '${at("2026-09-13T07:00:00Z")}', 40, 2, false, '${project}-101', ARRAY[]::text[], false),
       ('acme/widgets', ${number(110)}, 'Improve waiting-to-deploy step line', 'https://github.com/acme/widgets/pull/${number(110)}', 'octocat',
        '${at("2026-09-15T09:00:00Z")}', '${at("2026-09-15T15:00:00Z")}', '${at("2026-09-15T08:00:00Z")}', 30, 8, false, '${project}-100', ARRAY[]::text[], false),
       ('acme/widgets', ${number(111)}, 'Add component hierarchy filter', 'https://github.com/acme/widgets/pull/${number(111)}', 'octocat',
        '${at("2026-09-16T09:00:00Z")}', '${at("2026-09-16T16:00:00Z")}', '${at("2026-09-16T08:00:00Z")}', 120, 10, false, null, ARRAY[]::text[], false),
       ('acme/widgets', ${number(112)}, 'fix: component hierarchy includes grandchildren', 'https://github.com/acme/widgets/pull/${number(112)}', 'octocat',
        '${at("2026-09-17T07:00:00Z")}', '${at("2026-09-17T07:30:00Z")}', '${at("2026-09-17T06:45:00Z")}', 14, 3, true, null, ARRAY[]::text[], false),
       ('acme/widgets', ${number(113)}, 'Add rework share metric', 'https://github.com/acme/widgets/pull/${number(113)}', 'octocat',
        '${at("2026-09-18T09:00:00Z")}', '${at("2026-09-18T15:00:00Z")}', '${at("2026-09-18T08:00:00Z")}', 50, 4, false, '${project}-101', ARRAY[]::text[], false)`,
    `INSERT INTO delivery_runs (
       repo, run_id, kind, head_sha, head_commit_at, started_at, created_at, completed_at, conclusion, url, head_branch, event
     ) VALUES
      ('acme/widgets', ${number(501)}, 'deploy', 'aaa101', '${at("2026-09-05T12:30:00Z")}', '${at("2026-09-05T12:01:00Z")}', '${at("2026-09-05T12:01:00Z")}',
        '${at("2026-09-05T13:05:00Z")}', 'success', 'https://github.com/acme/widgets/actions/runs/${number(501)}', 'main', 'push'),
       ('acme/widgets', ${number(502)}, 'deploy', 'aaa102', '${at("2026-09-06T13:30:00Z")}', '${at("2026-09-06T13:35:00Z")}', '${at("2026-09-06T13:35:00Z")}',
        '${at("2026-09-06T14:05:00Z")}', 'success', 'https://github.com/acme/widgets/actions/runs/${number(502)}', 'main', 'push'),
       ('acme/widgets', ${number(503)}, 'deploy', 'aaa103', '${at("2026-09-09T09:30:00Z")}', '${at("2026-09-09T09:35:00Z")}', '${at("2026-09-09T09:35:00Z")}',
        '${at("2026-09-09T10:05:00Z")}', 'success', 'https://github.com/acme/widgets/actions/runs/${number(503)}', 'main', 'push'),
       ('acme/widgets', ${number(504)}, 'deploy', 'aaa104', '${at("2026-09-13T08:45:00Z")}', '${at("2026-09-13T08:50:00Z")}', '${at("2026-09-13T08:50:00Z")}',
        '${at("2026-09-13T09:05:00Z")}', 'success', 'https://github.com/acme/widgets/actions/runs/${number(504)}', 'main', 'push'),
       ('acme/widgets', ${number(505)}, 'deploy', 'aaa105', '${at("2026-09-17T07:45:00Z")}', '${at("2026-09-17T07:50:00Z")}', '${at("2026-09-17T07:50:00Z")}',
        '${at("2026-09-17T08:05:00Z")}', 'success', 'https://github.com/acme/widgets/actions/runs/${number(505)}', 'main', 'push'),
       ('acme/widgets', ${number(506)}, 'deploy', 'aaa106', '${at("2026-09-11T08:00:00Z")}', '${at("2026-09-11T08:30:00Z")}', '${at("2026-09-11T08:30:00Z")}',
        '${at("2026-09-11T09:05:00Z")}', 'failure', 'https://github.com/acme/widgets/actions/runs/${number(506)}', 'main', 'push'),
       ('acme/widgets', ${number(507)}, 'deploy', 'aaa107', '${at("2026-09-14T08:30:00Z")}', '${at("2026-09-14T08:35:00Z")}', '${at("2026-09-14T08:35:00Z")}',
        '${at("2026-09-14T09:05:00Z")}', 'failure', 'https://github.com/acme/widgets/actions/runs/${number(507)}', 'main', 'push'),
       ('acme/widgets', ${number(508)}, 'deploy', 'aaa108', '${at("2026-09-19T09:50:00Z")}', '${at("2026-09-19T10:00:00Z")}', '${at("2026-09-19T10:00:00Z")}',
        null, 'cancelled', 'https://github.com/acme/widgets/actions/runs/${number(508)}', 'main', 'push')`,
    `INSERT INTO delivery_run_jobs (repo, run_id, name, completed_at, conclusion) VALUES
       ('acme/widgets', ${number(501)}, 'widgets-release / widgets-release', '${at("2026-09-05T13:00:00Z")}', 'success'),
       ('acme/widgets', ${number(502)}, 'widgets-release / widgets-release', '${at("2026-09-06T14:00:00Z")}', 'success'),
       ('acme/widgets', ${number(503)}, 'widgets-release / widgets-release', '${at("2026-09-09T10:00:00Z")}', 'success'),
       ('acme/widgets', ${number(504)}, 'widgets-release / widgets-release', '${at("2026-09-13T09:00:00Z")}', 'success'),
       ('acme/widgets', ${number(505)}, 'widgets-release / widgets-release', '${at("2026-09-17T08:00:00Z")}', 'success'),
       ('acme/widgets', ${number(506)}, 'integration-test', '${at("2026-09-11T09:00:00Z")}', 'failure'),
       ('acme/widgets', ${number(507)}, 'widgets-release / widgets-release', '${at("2026-09-14T09:00:00Z")}', 'failure')`
  );
}

interface PipelineJob {
  name: string;
  minutes: number;
  conclusion: string | null;
}

/** One stored run and its jobs as VALUES rows, under the place's number for the run: each job
 *  starts at `jobStart` and lasts its minutes; a null conclusion is a job still running. */
function pipelineRun(
  place: DeliveryPlace,
  run: {
    id: number;
    kind: "pr_checks" | "deploy";
    started: string;
    completed: string;
    conclusion: string;
    branch: string | null;
    event: string;
    jobStart: string;
    jobs: PipelineJob[];
  }
): { run: string; jobs: string[] } {
  const quote = (value: string | null) => (value === null ? "null" : `'${value}'`);
  const prNumber = run.kind === "pr_checks" ? "7" : "null";
  const started = Date.parse(run.jobStart);
  const id = place.number(run.id);
  return {
    run: `('acme/widgets', ${id}, '${run.kind}', ${prNumber}, 's${id}', '${run.started}', '${run.started}', '${run.started}', '${run.completed}', '${run.conclusion}', 'https://github.com/acme/widgets/actions/runs/${id}', ${quote(run.branch)}, '${run.event}')`,
    jobs: run.jobs.map(
      (job) =>
        `('acme/widgets', ${id}, '${job.name}', '${run.jobStart}', '${new Date(started + job.minutes * 60_000).toISOString()}', ${quote(job.conclusion)})`
    ),
  };
}

/** The Pipeline page's rows: the ones the prototype's collector/tests/test_pipeline.py builds in
 *  code, with placeholder job names (the API test `api/delivery_pipeline_test.go` seeds the same
 *  rows), dated so the place's `?at=` (`PIPELINE_AT` in the corpus's) shows them, and the three
 *  job names set on the stored settings row. PR checks: the failing-jobs runs (today, two
 *  failures and a cancelled run; yesterday, a success and a failure before the 24 hours) and 50
 *  build / unit results to rank among the slowest jobs, every one a `pull_request` run, plus a
 *  `merge_group` run no card counts. Deploys (run ids 601-606, clear of the measures fixture's
 *  501-508, so the docs harness seeds both): four push runs on main naming the three settings'
 *  jobs, plus a workflow_dispatch run and a push on feature/x no card counts. The settings name
 *  `staging-e2e / run`, `staging-release / apply` and the roll-up `checks-complete`. Two pull
 *  requests merged today: one shipped by run 601 (created 08:00, after its 07:10 merge), one
 *  still waiting. Each run id and pull request number is the place's for it. With no place
 *  they are the corpus's, on the settings the caller wrote. */
export async function seedPipelineRows(place: DeliveryPlace = corpusPlace): Promise<void> {
  /** A time on 2026-09-<day> (a day past 30 rolls into October), as the prototype's
   *  collector/tests/test_pipeline.py `_ts` writes it, at the place's moment for it. */
  const fixtureTime = (day: number, hour: number, minute = 0) =>
    place.at(
      new Date(Date.UTC(2026, 8, day, hour, 0, 0) + minute * 60_000)
        .toISOString()
        .replace(".000Z", "Z")
    );
  const job = (name: string, minutes: number, conclusion: string | null = "success") => ({
    conclusion,
    minutes,
    name,
  });
  const check = (
    id: number,
    day: number,
    hour: number,
    minutes: number,
    conclusion: string,
    jobs: PipelineJob[],
    event = "pull_request"
  ) =>
    pipelineRun(place, {
      branch: null,
      completed: fixtureTime(day, hour, minutes),
      conclusion,
      event,
      id,
      jobStart: fixtureTime(day, hour),
      jobs,
      kind: "pr_checks",
      started: fixtureTime(day, hour),
    });
  const deploy = (id: number, branch: string, event: string, jobs: PipelineJob[]) =>
    pipelineRun(place, {
      branch,
      completed: fixtureTime(32, 12),
      conclusion: "success",
      event,
      id,
      jobStart: fixtureTime(32, 9),
      jobs: [job("staging-e2e / inventory", 0, "skipped"), ...jobs],
      kind: "deploy",
      started: fixtureTime(32, 8),
    });
  const runs = [
    check(1, 32, 8, 30, "failure", [
      job("build / unit", 29, "failure"),
      job("checks-complete", 0.5, "failure"),
      job("lint", 5),
    ]),
    check(2, 32, 10, 30, "failure", [
      job("build / unit", 29, "failure"),
      job("lint", 5, "failure"),
    ]),
    check(3, 31, 13, 30, "success", [
      job("build / unit", 29),
      job("lint", 5),
      job("docs", 0, "skipped"),
    ]),
    check(4, 31, 6, 30, "failure", [job("typecheck", 4, "failure")]),
    check(5, 32, 11, 10, "cancelled", [job("build / unit", 9, "failure")]),
    ...Array.from({ length: 50 }, (_, i) =>
      check(100 + i, 27 + (i % 5), 8, 95, "success", [job("build / unit", 90 + i)])
    ),
    check(900, 32, 9, 20, "success", [job("build / unit", 19)], "merge_group"),
    deploy(601, "main", "push", [
      job("staging-release / apply", 7),
      job("staging-e2e / run", 40),
      job("widgets-release / widgets-release", 9),
    ]),
    deploy(602, "main", "push", [
      job("staging-release / apply", 8),
      job("staging-e2e / run", 70, "failure"),
      job("widgets-release / widgets-release", 0, "skipped"),
    ]),
    deploy(603, "main", "push", [
      job("staging-release / apply", 9),
      job("staging-e2e / run", 3, "cancelled"),
    ]),
    deploy(604, "main", "push", [job("staging-release / apply", 0, null)]),
    deploy(605, "main", "workflow_dispatch", [
      job("staging-release / apply", 99, "failure"),
      job("staging-e2e / run", 99, "failure"),
    ]),
    deploy(606, "feature/x", "push", [
      job("staging-e2e / run", 30),
      job("widgets-release / widgets-release", 9),
    ]),
  ];
  const { number } = place;
  await sql(
    `UPDATE delivery_settings SET staging_e2e_job_name = 'staging-e2e / run',
       staging_apply_job_name = 'staging-release / apply', pr_checks_rollup_job_name = 'checks-complete'
     WHERE singleton`,
    `INSERT INTO delivery_pull_requests (
       repo, number, title, url, author, created_at, merged_at, additions, deletions, rework, issue_key, sessions, partial
     ) VALUES
       ('acme/widgets', ${number(11)}, 'feat: a piped widget', 'https://github.com/acme/widgets/pull/${number(11)}', 'octocat',
        '${fixtureTime(32, 7)}', '${fixtureTime(32, 7, 10)}', 3, 1, false, null, ARRAY[]::text[], false),
       ('acme/widgets', ${number(12)}, 'feat: a waiting widget', 'https://github.com/acme/widgets/pull/${number(12)}', 'octocat',
        '${fixtureTime(32, 9)}', '${fixtureTime(32, 10)}', 2, 0, false, null, ARRAY[]::text[], false)`,
    `INSERT INTO delivery_runs (
       repo, run_id, kind, pr_number, head_sha, head_commit_at, started_at, created_at, completed_at, conclusion, url, head_branch, event
     ) VALUES ${runs.map((r) => r.run).join(",\n")}`,
    `INSERT INTO delivery_run_jobs (repo, run_id, name, started_at, completed_at, conclusion) VALUES
       ${runs.flatMap((r) => r.jobs).join(",\n")}`
  );
}
