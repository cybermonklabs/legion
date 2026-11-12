import { rankItem, rankings } from "@tanstack/match-sorter-utils";
import type { ColumnFiltersState, GroupingState, SortingState } from "@tanstack/react-table";
import type { Actor, IssueSummary, UpdateIssueInput } from "../../api/types";
import { capSearchQuery } from "../../lib/query-param";
import { compareTimestamps } from "../../lib/timestamps";
import { type IssueStatus, issueStatuses, openIssueStatuses, statusText } from "./board-model";
import type { LayoutColumns } from "./issues-table-layout";

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

/** How many of one action's writes are in flight at once. Declared in a module of its own, so
 *  the write kind the shell registers at boot does not pull this model in with it
 *  (`issues-table-limits.ts`), and re-exported here, where its readers look for it. */
export { WRITES_IN_FLIGHT } from "./issues-table-limits";

/** How a column filters, and everything a filter of its kind needs: a column that filters by
 *  picking values cannot exist without the values a row files under, nor one that filters by
 *  typed text without the text it reads. Matching, the URL parameter and the options a filter
 *  always offers live here too, so a column is one entry rather than a branch in three files. */
export type ColumnFilterSpec =
  | {
      readonly kind: "picks";
      /** Every value this row files under here: its filter options and its facet counts. */
      readonly values: (item: Item) => string[];
      /** The heading this row groups under; absent when the column cannot group. */
      readonly group?: (item: Item) => string;
      /** The order the filter lists its values in; by count when absent. */
      readonly order?: readonly string[];
      /** A value as the reader sees it, in an option and in a group's heading. */
      readonly show?: (value: string) => string;
      /** A row must carry every picked value, not just one of them, compared case-insensitively. */
      readonly match?: "all";
      /** The URL parameter the picks travel in; `f.<id>` when absent. */
      readonly param?: string;
      /** Values the filter offers whatever the loaded rows hold. */
      readonly always?: readonly string[];
      /** A value out of the URL as this column keeps it. */
      readonly normalize?: (value: string) => string;
    }
  | { readonly kind: "text"; readonly value: (item: Item) => string };

/** Every column the table has. Narrow enough that a rendering, a width or a minimum keyed by
 *  an id no column carries fails to compile. */
export type ColumnId =
  | "select"
  | "priority"
  | "queue"
  | "key"
  | "title"
  | "status"
  | "epic"
  | "assignee"
  | "author"
  | "labels"
  | "claim"
  | "asks"
  | "created"
  | "updated";

/** One column's identity: everything about it that is not how its cell renders. The column
 *  definitions, `columnIds`, `groupable`, `facets`, the default widths and the minimums are all
 *  derived from this list, so a new column is one entry here plus its cell. */
export interface ColumnSpec {
  readonly id: ColumnId;
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
    filter: { kind: "text", value: (item) => item.issue.key },
  },
  {
    id: "title",
    size: 380,
    min: 116,
    compactMin: 120,
    pinned: true,
    filter: { kind: "text", value: (item) => item.issue.title },
  },
  {
    id: "status",
    size: 128,
    min: 132,
    filter: {
      kind: "picks",
      values: (item) => [item.issue.status],
      group: (item) => item.issue.status,
      order: issueStatuses,
      // A `?status=` this bundle does not know stays under its own text: the filter applies
      // and matches nothing, which is what the Board has always done (`statusText`).
      show: statusText,
      // Shared with the Board, and every status is offered even before its rows are loaded.
      param: "status",
      always: issueStatuses,
    },
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
      // The project's own spelling of each label, which is the Board's too: one `?label=` is
      // one cache key and one ticked option in both views. Matching is case-insensitive, so a
      // link in another casing still picks the same rows.
      values: (item) => item.issue.labels ?? [],
      match: "all",
      param: "label",
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
const columnSpec: ReadonlyMap<string, ColumnSpec> = new Map(
  COLUMNS.map((column) => [column.id, column])
);
export const columnFilterSpec = (id: string): ColumnFilterSpec | undefined =>
  columnSpec.get(id)?.filter;
/** The columns a URL may name in `sort=` or a filter parameter: every one a reader controls. */
export const columnIds: readonly string[] = COLUMNS.filter(
  (column) => column.sortable !== false || column.filter !== undefined
).map((column) => column.id);
const groupable: readonly string[] = COLUMNS.filter(
  (column) => column.filter?.kind === "picks" && column.filter.group !== undefined
).map((column) => column.id);
export const canGroupBy = (id: string) => groupable.includes(id);
/** Each registry column's minimum and its phone minimum, by id. Looked up by a column id, so a
 *  lookup can miss: a column the registry gives no such width. */
export const COLUMN_MIN: Readonly<Partial<Record<string, number>>> = Object.fromEntries(
  COLUMNS.map((column) => [column.id, column.min])
);
export const COMPACT_COLUMN_MIN: Readonly<Partial<Record<string, number>>> = Object.fromEntries(
  COLUMNS.flatMap((column) =>
    column.compactMin === undefined ? [] : [[column.id, column.compactMin]]
  )
);
/** A column's narrowest on a phone: its phone minimum where the registry gives one. */
const phoneMin = (id: ColumnId): number => COMPACT_COLUMN_MIN[id] ?? COLUMN_MIN[id] ?? 0;

/**
 * The List's layout, for `useTableLayout`: every registry column, at the registry's starting
 * width and never below its minimum, with Updated hidden until a reader shows it. Select,
 * Priority, Key and Title stay pinned within 60% of a box 640 px wide or wider, Title narrowing
 * first; on a phone, where the key and the title are what identify a row, Select, Key and Title
 * stay pinned within 85% of the box, Key at its phone width and Title taking what is left down
 * to its phone minimum, so an edge of the next column shows the table scrolls sideways. Every
 * saved `dispatch.project-table.<project>` record was written for these columns.
 */
export const ISSUE_TABLE_COLUMNS: LayoutColumns = {
  ids: COLUMNS.map((column) => column.id),
  hiddenByDefault: { updated: false } satisfies Partial<Record<ColumnId, boolean>>,
  sizes: Object.fromEntries(COLUMNS.map((column) => [column.id, column.size])),
  minimums: COLUMN_MIN,
  pins: {
    wide: { columns: COLUMNS.filter((column) => column.pinned).map(({ id }) => id), share: 0.6 },
    phone: {
      columns: ["select", "key", "title"] satisfies ColumnId[],
      share: 0.85,
      widths: { key: phoneMin("key") },
      flexibleMin: phoneMin("title"),
    },
    flexible: "title" satisfies ColumnId,
  },
};

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
  // Nothing typed matches everything, and the two hot callers — the epic fold-in and the
  // facet counts — ask on every render, so this answers before any work is done.
  if (query.trim() === "") return true;
  let words: string[] | undefined;
  const searchWords = () => {
    if (words === undefined) words = item.search.toLowerCase().split(/[^\p{L}\p{N}]+/u);
    return words;
  };
  return query
    .trim()
    .toLowerCase()
    .split(/\s+/)
    .every(
      (word) =>
        rankItem(item.search, word, { threshold: rankings.CONTAINS }).passed ||
        (word.length >= 5 && searchWords().some((candidate) => withinOneEdit(candidate, word)))
    );
}
export function facets(item: Item, id: string): string[] {
  const filter = columnFilterSpec(id);
  return filter?.kind === "picks" ? filter.values(item) : [];
}
export function matchesFilters(item: Item, filters: ColumnFiltersState, except?: string): boolean {
  return filters.every(({ id, value }) => {
    if (id === except) return true;
    const filter = columnFilterSpec(id);
    if (filter?.kind === "text")
      return filter.value(item).toLowerCase().includes(String(value).toLowerCase());
    if (!Array.isArray(value) || value.length === 0) return true;
    return filter?.kind === "picks" && filter.match === "all"
      ? value.every((picked: string) =>
          facets(item, id).some((entry) => entry.toLowerCase() === picked.toLowerCase())
        )
      : facets(item, id).some((entry) => value.includes(entry));
  });
}
/** The headings that stand for "none of them", wherever a column has one: they read last, after
 *  every named group. */
const EMPTY_GROUPS: readonly string[] = ["No epic", "Unknown epic", "Nobody", "Unassigned"];
/** Which of two of column `id`'s groups comes first: the order its filter lists its values in,
 *  and by name where the registry gives none — numbers inside a name counted as numbers, so
 *  `CORE-2` reads before `CORE-10`, and the empty group last. A group's place is its column's,
 *  not the row sort's, so the headings read P0, P1, P2, P3 however the rows are sorted. */
export function compareGroups(id: string, a: string, b: string): number {
  const filter = columnFilterSpec(id);
  if (filter?.kind !== "picks" || filter.group === undefined) return 0;
  const order = filter.order;
  if (order === undefined) {
    const empty = Number(EMPTY_GROUPS.includes(a)) - Number(EMPTY_GROUPS.includes(b));
    return empty !== 0 ? empty : a.localeCompare(b, undefined, { numeric: true });
  }
  const place = (value: string) => {
    const index = order.indexOf(value);
    return index === -1 ? order.length : index;
  };
  return place(a) - place(b);
}
/** The heading `item` groups under in column `id`, which is what `compareGroups` orders. */
export function groupValue(id: string, item: Item): string {
  const filter = columnFilterSpec(id);
  return filter?.kind === "picks" && filter.group !== undefined ? filter.group(item) : "";
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
const paramFor = (id: string) => {
  const filter = columnFilterSpec(id);
  return filter?.kind === "picks" && filter.param !== undefined ? filter.param : `f.${id}`;
};
/** The column a filter parameter in a URL belongs to. */
const columnForParam: ReadonlyMap<string, string> = new Map(
  COLUMNS.map((column) => [paramFor(column.id), column.id])
);
const defaultSort = defaultView.sorting.map(({ id, desc }) => `${desc ? "-" : ""}${id}`).join(",");
export function readUrl(params: URLSearchParams): View {
  const sorting = (params.get("sort") ?? "")
    .split(",")
    .map((part) => ({ id: part.replace(/^-/, ""), desc: part.startsWith("-") }))
    .filter(({ id }) => columnIds.includes(id));
  const grouping = params.get("group") ?? "";
  const filters: ColumnFiltersState = [...new Set(params.keys())].flatMap((key) => {
    const id = columnForParam.get(key) ?? "";
    const filter = columnFilterSpec(id);
    return columnIds.includes(id)
      ? [
          {
            id,
            value:
              filter?.kind === "text"
                ? params.get(key)
                : params
                    .getAll(key)
                    .map((value) =>
                      filter?.kind === "picks" && filter.normalize ? filter.normalize(value) : value
                    ),
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
/** Whether a status pick is exactly the set the table opens with. */
function isDefaultStatus(value: unknown): boolean {
  const picked = Array.isArray(value) ? value.map(String) : [];
  const fallback = (defaultView.filters[0]?.value ?? []) as string[];
  return picked.length === fallback.length && fallback.every((status) => picked.includes(status));
}
export function writeUrl(params: URLSearchParams, state: View): URLSearchParams {
  const next = new URLSearchParams(params);
  for (const key of [...next.keys()])
    if (
      key.startsWith("f.") ||
      columnForParam.has(key) ||
      ["sort", "group", "q", "page", "size", "nofilter", "unclaimed"].includes(key)
    )
      next.delete(key);
  // A control left at its default says nothing, so a shared link carries only what was changed.
  const sort = state.sorting.map(({ id, desc }) => `${desc ? "-" : ""}${id}`).join(",");
  if (sort !== defaultSort) next.set("sort", sort);
  if (state.grouping[0]) next.set("group", state.grouping[0]);
  if (!state.filters.some(({ id }) => id === "status")) next.set("nofilter", "1");
  for (const filter of state.filters) {
    // The open statuses are what the table shows when nothing says otherwise, so a link that
    // carries them says nothing; `readUrl` puts them back, and `nofilter=1` is how an empty
    // selection stays expressible.
    if (filter.id === "status" && isDefaultStatus(filter.value)) continue;
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

/** The column order a drag leaves behind: `dragged` taken out of `current` and put back
 *  immediately before `before`. A drop on itself, or on an id the order does not hold, leaves
 *  the order as it was. */
export function reorder(current: readonly string[], dragged: string, before: string): string[] {
  if (dragged === before || !current.includes(dragged) || !current.includes(before))
    return [...current];
  const next = current.filter((id) => id !== dragged);
  next.splice(next.indexOf(before), 0, dragged);
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
