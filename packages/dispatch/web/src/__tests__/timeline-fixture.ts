// What the Evals tab's merge timeline tests read: a project-timeline view's rows, its snapshots and
// the deltas a growth answers, as Dispatch writes them (packages/contracts fixtures/live
// project_timeline_*), with a revision for every row so a resume names what the page holds.
import type {
  DeliveryLiveDelta,
  DeliveryLiveSnapshot,
  LiveSubscribe,
  ProjectTimelineMergeRow,
  ProjectTimelineResponse,
  ProjectTimelineRow,
  ProjectTimelineRunRow,
} from "../api/types";

/** The window every row of `timelineRows` falls in: a 28-day preset as Dispatch resolved it. */
export const TIMELINE_WINDOW = { from: "2026-10-26T00:00:00Z", to: "2026-11-24T00:00:00Z" };

/** A merge of pull request `number` at `at`. */
export function mergeRow(
  number: number,
  at: string,
  overrides: Partial<ProjectTimelineMergeRow> = {}
): ProjectTimelineMergeRow {
  return {
    additions: 1,
    author: "octocat",
    components: [],
    created_at: at,
    deletions: 1,
    deploy_run: null,
    deployed_at: null,
    deployed_status: "waiting",
    first_commit_at: null,
    id: `acme/widgets#${number}`,
    issue: null,
    issue_title: null,
    kind: "merge",
    merged_at: at,
    number,
    parent_agent: null,
    partial: false,
    priority: null,
    repo: "acme/widgets",
    rework: false,
    sessions: [],
    title: `feat: widget ${number}`,
    unfetchable_reason: null,
    url: `https://github.com/acme/widgets/pull/${number}`,
    ...overrides,
  };
}

/** Deploy run `id`, created at `at`: a deploy that shipped production, or a failed run. */
export function runRow(
  id: number,
  at: string,
  kind: ProjectTimelineRunRow["kind"] = "deploy"
): ProjectTimelineRunRow {
  return {
    completed_at: at,
    conclusion: kind === "deploy" ? "success" : "failure",
    created_at: at,
    failed_jobs: kind === "deploy" ? [] : [{ completed_at: at, name: "e2e" }],
    head_at: at,
    head_sha: `sha${id}`,
    id,
    kind,
    production: { completed_at: at, conclusion: kind === "deploy" ? "success" : null },
    prs: [],
    repo: "acme/deploys",
    root_failing_job: kind === "deploy" ? null : { completed_at: at, name: "e2e" },
    started_at: at,
    url: `https://github.com/acme/deploys/actions/runs/${id}`,
  };
}

/** The rows `[from, to)` of a window of rows newest first, a minute apart from the window's end:
 *  every tenth a deploy, every other a merge. */
export function timelineRows(from: number, to: number): ProjectTimelineRow[] {
  return Array.from({ length: to - from }, (_, offset) => {
    const index = from + offset;
    const at = new Date(Date.parse(TIMELINE_WINDOW.to) - (index + 1) * 60_000).toISOString();
    return index % 10 === 9 ? runRow(10_000 + index, at) : mergeRow(index + 1, at);
  });
}

/** A project-timeline view's body holding `items`, its `limit` newest of `total` rows. */
export function timelineAnswer(
  items: ProjectTimelineRow[],
  total: number,
  limit = 200
): ProjectTimelineResponse {
  return {
    color_counts: { author: { octocat: total }, repo: { "acme/widgets": total } },
    components: {},
    facet_counts: {
      author: { octocat: total },
      repo: { "acme/gadgets": 1, "acme/widgets": total },
    },
    freshness: {
      last_error: null,
      last_event_at: null,
      last_reconcile_at: null,
      unfetchable_count: 0,
    },
    issue_titles: {},
    items,
    limit,
    offset: 0,
    session_groups: [],
    session_pr_counts: {},
    session_titles: {},
    total,
    waiting: limit >= total ? [] : null,
    window: TIMELINE_WINDOW,
  };
}

/** Each row's revision, as Dispatch names what a page holds. */
function rowRevisions(rows: readonly ProjectTimelineRow[]): Record<string, string> {
  return Object.fromEntries(
    rows.map((row) => [String(row.id), String(row.id).padStart(16, "0").slice(-16)])
  );
}

/** Dispatch's first answer to subscription `sub`: `body`, as of change `seq`, naming each of its
 *  rows' revisions. */
export function timelineSnapshot(
  sub: string,
  body: ProjectTimelineResponse,
  seq = "1"
): DeliveryLiveSnapshot {
  return {
    body,
    changed_at: null,
    revs: {
      counts: {},
      fields: { limit: "00000000000000aa" },
      items: { items: rowRevisions(body.items ?? []) },
      maps: {},
    },
    seq,
    sub,
    type: "snapshot",
  };
}

/** Dispatch's answer to a growth of subscription `sub` to `limit`: only the rows it lacked. */
export function timelineGrowth(
  sub: string,
  rows: ProjectTimelineRow[],
  limit: number,
  seq: string
): DeliveryLiveDelta {
  return {
    changed_at: null,
    counts: {},
    items: { items: { remove: [], upsert: rows } },
    maps: {},
    revs: {
      fields: { limit: limit.toString(16).padStart(16, "0") },
      items: { items: rowRevisions(rows) },
    },
    seq,
    set: { limit },
    sub,
    type: "delta",
  };
}

/** The limit a project-timeline subscribe frame asks for. */
export function limitOf(frame: LiveSubscribe): number {
  return frame.view.kind === "project-timeline"
    ? Number(new URLSearchParams(frame.view.query).get("limit"))
    : Number.NaN;
}
