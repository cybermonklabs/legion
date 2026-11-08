import { rankItem, rankings } from "@tanstack/match-sorter-utils";
import type { ColumnFiltersState, GroupingState, SortingState } from "@tanstack/react-table";
import type { Actor, IssueSummary, UpdateIssueInput } from "../../api/types";
import { capSearchQuery } from "../../lib/query-param";
import { compareTimestamps } from "../../lib/timestamps";
import { issueStatuses } from "./board-model";

export interface Item {
  readonly issue: IssueSummary;
  readonly queue?: number;
  readonly epic: string;
  readonly epicKey?: string;
  readonly author: string;
  readonly claimedBy: string;
  readonly search: string;
}
export const AGES = ["Today", "This week", "This month", "Older"] as const;
export const ALL_ROWS = Number.MAX_SAFE_INTEGER;
export const columnIds = [
  "priority",
  "queue",
  "key",
  "title",
  "status",
  "epic",
  "assignee",
  "author",
  "labels",
  "claim",
  "asks",
  "created",
  "updated",
];
const groupable = [
  "priority",
  "status",
  "epic",
  "assignee",
  "author",
  "claim",
  "created",
  "updated",
];
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
  filters: [{ id: "status", value: issueStatuses.filter((status) => status !== "done") }],
  search: "",
  page: 0,
  pageSize: 50,
};
export const isQueued = (issue: IssueSummary) =>
  issue.status === "todo" && (issue.labels ?? []).includes("legion");
export const isEpic = (issue: Pick<IssueSummary, "parent" | "labels" | "status">) =>
  issue.parent === null && (issue.labels ?? []).includes("epic") && issue.status !== "done";
export const priorityName = (value: string) => (value === "" ? "No priority" : `P${value}`);
export function ageOf(iso: string): string {
  const days = (Date.now() - new Date(iso).getTime()) / 86_400_000;
  return days < 1 ? "Today" : days < 7 ? "This week" : days < 30 ? "This month" : "Older";
}
export type EpicAncestor = Pick<IssueSummary, "key" | "parent" | "status" | "labels" | "title">;
export function buildItems(
  issues: readonly IssueSummary[],
  label: (actor: Actor) => string,
  ancestors: readonly EpicAncestor[] = []
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
  return issues
    .filter((issue) => !isEpic(issue))
    .map((issue) => {
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
  switch (id) {
    case "priority":
      return [String(item.issue.priority ?? "")];
    case "queue":
      return [item.queue === undefined ? "Not queued" : "Queued"];
    case "key":
      return [item.issue.key];
    case "status":
      return [item.issue.status];
    case "epic":
      return [item.epic];
    case "assignee":
      return [item.issue.assignee ?? "Unassigned"];
    case "author":
      return [item.author];
    case "claim":
      return [item.claimedBy];
    case "labels":
      return (item.issue.labels ?? []).map((label) => label.toLowerCase());
    case "asks":
      return [String(item.issue.open_asks)];
    case "created":
      return [ageOf(item.issue.created_at)];
    case "updated":
      return [ageOf(item.issue.updated_at)];
    default:
      return [];
  }
}
export function matchesFilters(item: Item, filters: ColumnFiltersState, except?: string): boolean {
  return filters.every(
    ({ id, value }) =>
      id === except ||
      (id === "title"
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
              id === "title"
                ? params.get(key)
                : params
                    .getAll(key)
                    .map((value) => (id === "labels" ? value.toLowerCase() : value)),
          },
        ]
      : [];
  });
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
    grouping: groupable.includes(grouping) ? [grouping] : [],
    filters,
    search: capSearchQuery(params.get("q") ?? ""),
    page: Number.isSafeInteger(page) && page > 0 ? page - 1 : 0,
    pageSize: [25, 50, 100, 250, ALL_ROWS].includes(size) ? size : defaultView.pageSize,
  };
}
export function writeUrl(params: URLSearchParams, state: View): URLSearchParams {
  const next = new URLSearchParams(params);
  for (const key of [...next.keys()])
    if (
      key.startsWith("f.") ||
      ["status", "label", "sort", "group", "q", "page", "size", "nofilter"].includes(key)
    )
      next.delete(key);
  next.set("sort", state.sorting.map(({ id, desc }) => `${desc ? "-" : ""}${id}`).join(","));
  next.set("group", state.grouping[0] ?? "");
  if (!state.filters.some(({ id }) => id === "status")) next.set("nofilter", "1");
  for (const filter of state.filters)
    for (const value of Array.isArray(filter.value) ? filter.value : [filter.value])
      next.append(paramFor(filter.id), String(value));
  if (state.search) next.set("q", state.search);
  if (state.page) next.set("page", String(state.page + 1));
  next.set("size", String(state.pageSize));
  return next;
}
export function startNextWrites(
  items: readonly Item[],
  keys: readonly string[]
): { key: string; input: UpdateIssueInput }[] {
  const queue = items
    .filter((item): item is Item & { queue: number } => item.queue !== undefined)
    .sort((a, b) => a.queue - b.queue);
  const selected = queue.filter((item) => keys.includes(item.issue.key));
  const head = queue.find((item) => !keys.includes(item.issue.key));
  if (!head) return [];
  const writes: { key: string; input: UpdateIssueInput }[] = [];
  let before = head.issue.key;
  for (const item of selected.reverse()) {
    writes.push({ key: item.issue.key, input: { rank: { before } } });
    before = item.issue.key;
  }
  return writes;
}

export function remainingStartNextWrites(items: readonly Item[], keys: readonly string[]) {
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
