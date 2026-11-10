import { rankItem, rankings } from "@tanstack/match-sorter-utils";
import type { ColumnFiltersState, GroupingState, SortingState } from "@tanstack/react-table";
import type { Actor, IssueSummary, UpdateIssueInput } from "../../api/types";
import { capSearchQuery } from "../../lib/query-param";
import { compareTimestamps } from "../../lib/timestamps";
import { type IssueStatus, issueStatuses, openIssueStatuses, statusLabel } from "./board-model";

export interface Item {
  readonly issue: IssueSummary;
  readonly queue?: number;
  readonly epic: string;
  readonly epicKey?: string;
  readonly author: string;
  readonly claimedBy: string;
  /** Whether an agent could pick this issue up now: nobody holds it, or the server would hand
   *  its claim to the next agent that asks. The Claimed by filter's "Nobody or lapsed". */
  readonly claimFree: boolean;
  readonly search: string;
}
export const AGES = ["Today", "This week", "This month", "Older"] as const;
export const ALL_ROWS = Number.MAX_SAFE_INTEGER;
export const PAGE_SIZES = [25, 50, 100, 250, ALL_ROWS];
/** The statuses an issue is being worked in, as the status bar counts them. */
export const runningIssueStatuses: readonly IssueStatus[] = [
  "in_progress",
  "testing",
  "needs_review",
  "retro",
];
/** The Claimed by value that picks the issues nobody is working: no claim at all, or a claim
 *  the server would hand over (`claimIsTakeable`). The `?unclaimed=1` link sets it. */
export const UNCLAIMED_FACET = "Nobody or lapsed";

/** How a column filters. `picks` carries the values a row files under, so a column that filters
 *  by picking values cannot exist without them; `text` filters by what the reader types. */
export type ColumnFilterSpec =
  | {
      readonly kind: "picks";
      /** Every value this row files under here: its filter options and its facet counts. */
      readonly values: (item: Item) => string[];
      /** The heading this row groups under; absent when the column cannot group. */
      readonly group?: (item: Item) => string;
      /** The order the filter lists its values in; by count when absent. */
      readonly order?: readonly string[];
      /** A value as the reader sees it. */
      readonly show?: (value: string) => string;
    }
  | { readonly kind: "text" };

/** One column's identity: everything about it that is not how its cell renders. The column
 *  definitions, `columnIds`, `groupable`, `facets`, the default widths and the minimums are all
 *  derived from this list, so a new column is one entry here plus its cell. */
export interface ColumnSpec {
  readonly id: string;
  /** Its width before the reader drags it. */
  readonly size: number;
  /** How narrow a drag may take it: label, sort mark, a 44 px filter target and padding. */
  readonly min: number;
  /** How narrow it may go on a phone, where the key and the title are the row. */
  readonly compactMin?: number;
  /** Stays in view while the table scrolls sideways. */
  readonly pinned?: boolean;
  /** Headers sort unless this says otherwise. */
  readonly sortable?: boolean;
  readonly filter?: ColumnFilterSpec;
}

const one = (
  value: (item: Item) => string,
  rest: { order?: readonly string[]; show?: (value: string) => string; groups?: boolean } = {}
): ColumnFilterSpec => ({
  kind: "picks",
  values: (item) => [value(item)],
  ...(rest.groups === false ? {} : { group: value }),
  ...(rest.order === undefined ? {} : { order: rest.order }),
  ...(rest.show === undefined ? {} : { show: rest.show }),
});

export const priorityName = (value: string) => (value === "" ? "No priority" : `P${value}`);
export function ageOf(iso: string): string {
  const days = (Date.now() - new Date(iso).getTime()) / 86_400_000;
  return days < 1 ? "Today" : days < 7 ? "This week" : days < 30 ? "This month" : "Older";
}

export const COLUMNS: readonly ColumnSpec[] = [
  { id: "select", size: 44, min: 44, pinned: true, sortable: false },
  {
    id: "priority",
    size: 140,
    min: 140,
    pinned: true,
    filter: one((item) => String(item.issue.priority ?? ""), {
      order: ["0", "1", "2", "3", ""],
      show: priorityName,
    }),
  },
  {
    id: "queue",
    size: 96,
    min: 124,
    filter: one((item) => (item.queue === undefined ? "Not queued" : "Queued"), {
      order: ["Queued", "Not queued"],
      groups: false,
    }),
  },
  {
    id: "key",
    size: 104,
    min: 108,
    compactMin: 88,
    pinned: true,
    filter: one((item) => item.issue.key, { groups: false }),
  },
  { id: "title", size: 380, min: 116, compactMin: 120, pinned: true, filter: { kind: "text" } },
  {
    id: "status",
    size: 128,
    min: 132,
    filter: one((item) => item.issue.status, {
      order: issueStatuses,
      show: (value) => statusLabel(value as IssueStatus),
    }),
  },
  { id: "epic", size: 220, min: 116, filter: one((item) => item.epic) },
  {
    id: "assignee",
    size: 180,
    min: 148,
    filter: one((item) => item.issue.assignee ?? "Unassigned"),
  },
  { id: "author", size: 200, min: 164, filter: one((item) => item.author) },
  {
    id: "labels",
    size: 200,
    min: 132,
    filter: {
      kind: "picks",
      values: (item) => (item.issue.labels ?? []).map((label) => label.toLowerCase()),
    },
  },
  {
    id: "claim",
    size: 220,
    min: 164,
    filter: {
      kind: "picks",
      values: (item) => (item.claimFree ? [item.claimedBy, UNCLAIMED_FACET] : [item.claimedBy]),
      group: (item) => item.claimedBy,
    },
  },
  {
    id: "asks",
    size: 96,
    min: 148,
    filter: one((item) => String(item.issue.open_asks), { groups: false }),
  },
  {
    id: "created",
    size: 120,
    min: 148,
    filter: one((item) => ageOf(item.issue.created_at), { order: AGES }),
  },
  {
    id: "updated",
    size: 120,
    min: 148,
    filter: one((item) => ageOf(item.issue.updated_at), { order: AGES }),
  },
];
const columnSpec = new Map(COLUMNS.map((column) => [column.id, column]));
export const columnFilterSpec = (id: string): ColumnFilterSpec | undefined =>
  columnSpec.get(id)?.filter;
/** The columns a URL may name in `sort=` or a filter parameter: every one a reader controls. */
export const columnIds = COLUMNS.filter(
  (column) => column.sortable !== false || column.filter !== undefined
).map((column) => column.id);
const groupable = COLUMNS.filter(
  (column) => column.filter?.kind === "picks" && column.filter.group !== undefined
).map((column) => column.id);
export const canGroupBy = (id: string) => groupable.includes(id);
export const COLUMN_SIZES: Readonly<Record<string, number>> = Object.fromEntries(
  COLUMNS.map((column) => [column.id, column.size])
);
export const COLUMN_MIN: Readonly<Record<string, number>> = Object.fromEntries(
  COLUMNS.map((column) => [column.id, column.min])
);
export const COMPACT_COLUMN_MIN: Readonly<Record<string, number>> = Object.fromEntries(
  COLUMNS.flatMap((column) =>
    column.compactMin === undefined ? [] : [[column.id, column.compactMin]]
  )
);
export const PINNED = COLUMNS.filter((column) => column.pinned).map((column) => column.id);
/** How much of the table's box the pinned columns may cover: on a phone (below 640 px) the key
 *  and title are what identify a row, so they take most of it. */
export const PINNED_SHARE = 0.6;
export const PHONE_PINNED_SHARE = 0.85;

export interface View {
  sorting: SortingState;
  grouping: GroupingState;
  filters: ColumnFiltersState;
  search: string;
  page: number;
  pageSize: number;
}
export const defaultView: View = {
  sorting: [{ id: "queue", desc: false }],
  grouping: [],
  filters: [{ id: "status", value: [...openIssueStatuses] }],
  search: "",
  page: 0,
  pageSize: 50,
};
const hasLabel = (issue: Pick<IssueSummary, "labels">, label: string) =>
  (issue.labels ?? []).some((value) => value.toLowerCase() === label);
/** The server matches a label on `lower(label)`, so the table does too: an issue labelled
 *  `Legion` is in Legion's queue, and one labelled `Epic` is an epic. */
export const isQueued = (issue: Pick<IssueSummary, "status" | "labels">) =>
  issue.status === "todo" && hasLabel(issue, "legion");
export const isEpic = (issue: Pick<IssueSummary, "parent" | "labels" | "status">) =>
  issue.parent === null && hasLabel(issue, "epic") && issue.status !== "done";
export type EpicAncestor = Pick<IssueSummary, "key" | "parent" | "status" | "labels" | "title">;
export function buildItems(
  issues: readonly IssueSummary[],
  label: (actor: Actor) => string,
  ancestors: readonly EpicAncestor[] = [],
  isFree: (issue: IssueSummary) => boolean = (issue) => issue.claim === null
): Item[] {
  const byKey = new Map<string, EpicAncestor>(
    [...ancestors, ...issues].map((issue) => [issue.key, issue])
  );
  // Rank's alphabet is byte-ordered, not locale-ordered.
  const queue = issues
    .filter((issue) => !isEpic(issue) && isQueued(issue))
    .sort((a, b) =>
      a.rank < b.rank
        ? -1
        : a.rank > b.rank
          ? 1
          : compareTimestamps(a.created_at, b.created_at) || a.key.localeCompare(b.key)
    );
  const positions = new Map(queue.map((issue, index) => [issue.key, index + 1]));
  return issues.map((issue) => {
    let root: EpicAncestor | undefined = issue;
    const seen = new Set<string>();
    while (root?.parent && !seen.has(root.key)) {
      seen.add(root.key);
      root = byKey.get(root.parent);
    }
    const epic = root && isEpic(root) ? root : undefined;
    const author = issue.created_by ? label(issue.created_by) : "Unknown";
    const claimedBy = issue.claim ? label(issue.claim.actor) : "Nobody";
    const epicName = epic
      ? `${epic.key} · ${epic.title}`
      : root === undefined || root.parent !== null
        ? "Unknown epic"
        : "No epic";
    return {
      issue,
      queue: positions.get(issue.key),
      epic: epicName,
      epicKey: epic?.key,
      author,
      claimedBy,
      claimFree: isFree(issue),
      search: [
        issue.key,
        issue.title,
        epicName,
        issue.assignee ?? "",
        author,
        claimedBy,
        ...(issue.labels ?? []),
      ].join(" "),
    };
  });
}

/** Move the first open issue on each path out of the source. Its open or closed descendants
 *  travel with it; only a path made entirely of closed intermediates needs another parent write. */
export function mergeMembers(issues: readonly IssueSummary[], source: string): string[] {
  const byKey = new Map(issues.map((issue) => [issue.key, issue]));
  return issues
    .filter((issue) => {
      if (issue.status === "done" || issue.key === source) return false;
      let parent = issue.parent;
      const seen = new Set<string>();
      while (parent !== null && parent !== source && !seen.has(parent)) {
        seen.add(parent);
        const intermediate = byKey.get(parent);
        if (!intermediate || intermediate.status !== "done") return false;
        parent = intermediate.parent;
      }
      return parent === source;
    })
    .map((issue) => issue.key);
}
function withinOneEdit(a: string, b: string): boolean {
  if (Math.abs(a.length - b.length) > 1) return false;
  let i = 0,
    j = 0,
    edits = 0;
  while (i < a.length && j < b.length) {
    if (a[i] === b[j]) {
      i++;
      j++;
      continue;
    }
    if (++edits > 1) return false;
    if (a.length > b.length) i++;
    else if (b.length > a.length) j++;
    else {
      i++;
      j++;
    }
  }
  return edits + a.length - i + b.length - j <= 1;
}
export function matchesSearch(item: Item, query: string): boolean {
  const words = item.search.toLowerCase().split(/[^\p{L}\p{N}]+/u);
  return query
    .trim()
    .toLowerCase()
    .split(/\s+/)
    .every(
      (word) =>
        rankItem(item.search, word, { threshold: rankings.CONTAINS }).passed ||
        (word.length >= 5 && words.some((candidate) => withinOneEdit(candidate, word)))
    );
}
export function facets(item: Item, id: string): string[] {
  const filter = columnFilterSpec(id);
  return filter?.kind === "picks" ? filter.values(item) : [];
}
export function matchesFilters(item: Item, filters: ColumnFiltersState, except?: string): boolean {
  return filters.every(
    ({ id, value }) =>
      id === except ||
      (columnFilterSpec(id)?.kind === "text"
        ? item.issue.title.toLowerCase().includes(String(value).toLowerCase())
        : !Array.isArray(value) ||
          value.length === 0 ||
          (id === "labels"
            ? value.every((picked: string) =>
                facets(item, id).some((entry) => entry.toLowerCase() === picked.toLowerCase())
              )
            : facets(item, id).some((entry) => value.includes(entry))))
  );
}
export function facetCounts(
  items: readonly Item[],
  id: string,
  filters: ColumnFiltersState,
  search: string
): Map<string, number> {
  const counts = new Map<string, number>();
  for (const item of items)
    if (matchesFilters(item, filters, id) && matchesSearch(item, search)) {
      for (const value of new Set(facets(item, id)))
        counts.set(value, (counts.get(value) ?? 0) + 1);
    }
  return counts;
}
const paramFor = (id: string) =>
  id === "status" ? "status" : id === "labels" ? "label" : `f.${id}`;
const defaultSort = defaultView.sorting.map(({ id, desc }) => `${desc ? "-" : ""}${id}`).join(",");
export function readUrl(params: URLSearchParams): View {
  const sorting = (params.get("sort") ?? "")
    .split(",")
    .map((part) => ({ id: part.replace(/^-/, ""), desc: part.startsWith("-") }))
    .filter(({ id }) => columnIds.includes(id));
  const grouping = params.get("group") ?? "";
  const filters: ColumnFiltersState = [...new Set(params.keys())].flatMap((key) => {
    const id =
      key === "label"
        ? "labels"
        : key === "status"
          ? "status"
          : key.startsWith("f.")
            ? key.slice(2)
            : "";
    return columnIds.includes(id)
      ? [
          {
            id,
            value:
              columnFilterSpec(id)?.kind === "text"
                ? params.get(key)
                : params
                    .getAll(key)
                    .map((value) => (id === "labels" ? value.toLowerCase() : value)),
          },
        ]
      : [];
  });
  // `?unclaimed=1` is the Board strip's own parameter and what shared List links carry: it picks
  // the Claimed by filter's "Nobody or lapsed", so both views read one URL.
  if (params.get("unclaimed") === "1") {
    const claim = filters.find((filter) => filter.id === "claim");
    if (claim === undefined) filters.push({ id: "claim", value: [UNCLAIMED_FACET] });
    else
      claim.value = [
        ...new Set([...(Array.isArray(claim.value) ? claim.value : []), UNCLAIMED_FACET]),
      ];
  }
  if (!params.has("status") && !params.has("nofilter")) filters.unshift(...defaultView.filters);
  const page = Number(params.get("page"));
  const size = Number(params.get("size"));
  return {
    sorting:
      params.has("sort") && params.get("sort") === ""
        ? []
        : sorting.length
          ? sorting
          : defaultView.sorting,
    grouping: canGroupBy(grouping) ? [grouping] : [],
    filters,
    search: capSearchQuery(params.get("q") ?? ""),
    page: Number.isSafeInteger(page) && page > 0 ? page - 1 : 0,
    pageSize: PAGE_SIZES.includes(size) ? size : defaultView.pageSize,
  };
}
export function writeUrl(params: URLSearchParams, state: View): URLSearchParams {
  const next = new URLSearchParams(params);
  for (const key of [...next.keys()])
    if (
      key.startsWith("f.") ||
      ["status", "label", "sort", "group", "q", "page", "size", "nofilter", "unclaimed"].includes(
        key
      )
    )
      next.delete(key);
  // A control left at its default says nothing, so a shared link carries only what was changed.
  const sort = state.sorting.map(({ id, desc }) => `${desc ? "-" : ""}${id}`).join(",");
  if (sort !== defaultSort) next.set("sort", sort);
  if (state.grouping[0]) next.set("group", state.grouping[0]);
  if (!state.filters.some(({ id }) => id === "status")) next.set("nofilter", "1");
  for (const filter of state.filters) {
    const values = Array.isArray(filter.value) ? filter.value : [filter.value];
    for (const value of values) {
      if (filter.id === "claim" && value === UNCLAIMED_FACET) next.set("unclaimed", "1");
      else next.append(paramFor(filter.id), String(value));
    }
  }
  if (state.search) next.set("q", state.search);
  if (state.page) next.set("page", String(state.page + 1));
  if (state.pageSize !== defaultView.pageSize) next.set("size", String(state.pageSize));
  return next;
}

/** The selected issues that are in Legion's queue, in the order the queue holds them. */
export function queuedInOrder(items: readonly Item[], keys: readonly string[]): string[] {
  return items
    .filter((item): item is Item & { queue: number } => item.queue !== undefined)
    .sort((a, b) => a.queue - b.queue)
    .filter((item) => keys.includes(item.issue.key))
    .map((item) => item.issue.key);
}

/**
 * The rank writes that move `keys` to the head of Legion's queue, each one placing its issue
 * before the one the previous write placed. Rows already ahead of everything unselected are
 * left alone, so a finished action writes nothing, and a resumed one writes only what is left:
 * the same function serves the first attempt and the one that picks it up again.
 */
export function startNextWrites(
  items: readonly Item[],
  keys: readonly string[]
): { key: string; input: UpdateIssueInput }[] {
  const queue = items
    .filter((item) => item.queue !== undefined)
    .sort((a, b) => (a.queue ?? 0) - (b.queue ?? 0))
    .map((item) => item.issue.key);
  const queued = new Set(queue);
  const selected = keys.filter((key) => queued.has(key));
  const selectedKeys = new Set(selected);
  let before = queue.find((key) => !selectedKeys.has(key));
  const operations: { key: string; input: UpdateIssueInput }[] = [];
  for (const key of selected.reverse()) {
    if (before && queue.indexOf(key) > queue.indexOf(before)) {
      operations.push({ key, input: { rank: { before } } });
      queue.splice(queue.indexOf(key), 1);
      queue.splice(queue.indexOf(before), 0, key);
    }
    before = key;
  }
  return operations;
}
