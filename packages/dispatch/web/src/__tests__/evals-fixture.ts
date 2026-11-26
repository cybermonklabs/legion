// What the Evals tab's section tests read: evals as the list read and the evals view answer them,
// and pages of an eval's rows as the items route answers them.
import type { EvalKind } from "../api/queries";
import type { EvalItemRow, EvalItemsResponse, EvalListItem, EvalListResponse } from "../api/types";
import type { EvalRange, EvalSeriesValues } from "../features/evals/chart-model";
import type { EvalsViewState } from "../features/evals/useEvalsViews";

/** Three whole UTC days, long past, so no day of them is partial. */
export const WINDOW: EvalRange = {
  days: 3,
  from: "2026-11-01T00:00:00Z",
  to: "2026-11-04T00:00:00Z",
};
/** The three days before `WINDOW`. */
export const COMPARE: EvalRange = {
  days: 3,
  from: "2026-10-29T00:00:00Z",
  to: "2026-11-01T00:00:00Z",
};

/** A listed series as the list read answers it: its input began before the window, and it holds
 *  no day row of the window unless `overrides` gives one. */
export function evalSeries(
  name: string,
  unit: string,
  overrides: Partial<EvalSeriesValues> = {}
): EvalSeriesValues {
  return {
    agg: "quantile",
    change: null,
    compare: null,
    compare_days: null,
    created_at: "2026-10-01T00:00:00Z",
    data_since: "2026-09-01",
    days: [],
    eval_id: "ACME/eval",
    is_primary: false,
    links: [],
    listed: true,
    min_n: null,
    narrowed: null,
    role: "headline",
    series: name,
    unit,
    version: 1,
    via_repository: 0,
    window: null,
    ...overrides,
  };
}

/** The computed eval `slug` of `kind` at `version`, holding `series`. */
export function evalItem(
  slug: string,
  kind: EvalKind,
  series: EvalSeriesValues[],
  version = 1
): EvalListItem {
  const written = { id: "alice", kind: "user" } as const;
  return {
    definition: {
      answer: "",
      created_at: "2026-10-01T00:00:00Z",
      decision: "",
      direction: "lower_better",
      doc_block: "",
      eval_id: `ACME/${slug}`,
      failure_modes: [],
      issue_key: "",
      kind,
      links: [],
      name: slug,
      number_rule: "",
      owner_role: "",
      person: "alice",
      project_key: "ACME",
      question: "",
      replaced_by: "",
      slug,
      source: "computed",
      state: "active",
      target: null,
      target_inclusive: false,
      target_judges: [],
      unit: series[0]?.unit ?? "count",
      version,
      written_by: written,
      written_via: "seed",
    },
    id: `ACME/${slug}`,
    owner: { person: "alice", role: "", written_by: written },
    series,
    verdict: null,
  };
}

/** An evals view's answer over `window` and `compare`, holding `items`. */
export function evalsAnswer(
  items: EvalListItem[],
  window: EvalRange = WINDOW,
  compare: EvalRange | null = COMPARE
): EvalListResponse {
  return {
    compare,
    delivery_freshness: null,
    freshness: { refresher_age_seconds: null, rows_updated_at: null },
    items,
    limit: 200,
    offset: 0,
    total: items.length,
    window,
  };
}

/** A painted view of `kind` that answered `items`. */
export function evalsView(
  kind: EvalKind,
  items: EvalListItem[],
  compare: EvalRange | null = COMPARE
): EvalsViewState {
  return {
    answer: evalsAnswer(items, WINDOW, compare),
    items,
    kind,
    retry: () => undefined,
    status: "painted",
  };
}

/** One row of an eval's rows, keyed `key`, of kind `kind`, holding `cols`. */
export function itemRow(key: string, kind: string, cols: Record<string, unknown>): EvalItemRow {
  return {
    cols,
    href: kind === "issue" ? `/issues/${key}` : "",
    links: [],
    measured_at: "2026-11-02T10:00:00Z",
    range: "window",
    row_key: key,
    row_kind: kind,
    source: "test",
  };
}

/** One page of an eval's rows, `rows` of `total` from `offset` on, over `WINDOW`. */
export function itemsPage(
  rows: EvalItemRow[],
  total = rows.length,
  offset = 0,
  limit = 200
): EvalItemsResponse {
  return { compare: null, items: rows, limit, offset, total, window: WINDOW };
}
