/**
 * PROTOTYPE (dispatch://INFRA-1508): the project's issues as one table on TanStack Table.
 * Every column sorts from its header and filters from the filter button at the header's right,
 * which opens Dispatch's one searchable multi-select; the groupable columns group; one fuzzy
 * search covers every row; and the rows come in pages. A bar over the table counts the issues by
 * status and filters on a click. Legion's view of the work is three columns: the queue position
 * (Legion starts `todo` issues handed to it in board order), the Legion phase (from a daemon-state
 * snapshot until Dispatch reads the daemon itself, dispatch://INFRA-1077) and the epic.
 * Every write is an ordinary issue write, the same an agent or the command bar makes.
 */
import { rankItem, rankings } from "@tanstack/match-sorter-utils";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import {
  type Column,
  type ColumnDef,
  type ColumnFiltersState,
  type ColumnOrderState,
  type ColumnSizingState,
  type ExpandedState,
  type FilterFn,
  flexRender,
  type GroupingState,
  getCoreRowModel,
  getExpandedRowModel,
  getFacetedRowModel,
  getFilteredRowModel,
  getGroupedRowModel,
  getPaginationRowModel,
  getSortedRowModel,
  type PaginationState,
  type Row,
  type RowData,
  type RowSelectionState,
  type SortingState,
  useReactTable,
  type VisibilityState,
} from "@tanstack/react-table";
import { useVirtualizer } from "@tanstack/react-virtual";
import { type ReactNode, useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import { Link, useSearchParams } from "react-router-dom";

import { api } from "../../api/client";
import { projectIssuesQuery, whoAmIQuery } from "../../api/queries";
import type { IssueSummary, UpdateIssueInput } from "../../api/types";
import { AttentionBadge } from "../../components/Badge";
import { LoadingSkeleton } from "../../components/LoadingSkeleton";
import { MultiSelect } from "../../components/MultiSelect";
import { LabelPill, StatusPill } from "../../components/Pill";
import {
  borderDefault,
  card,
  checkboxAccent,
  dangerText,
  linkHoverText,
  linkText,
  selectedCardBg,
  surfaceBg,
  surfaceMutedBg,
  surfaceMutedHoverBg,
  textMutedOnCanvas,
  textPrimaryOnCanvas,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { ClaimChip } from "../issue/ClaimChip";
import { PriorityControl } from "../issue/PriorityControl";
import { useAgentText } from "../refs/AgentName";
import { referenceTriggerProps } from "../refs/RefPreview";
import { buildIssuePath } from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";
import { type IssueStatus, issueStatuses, statusLabel } from "./board-model";
import { projectIssuesQueryKey } from "./issue-filters";

declare module "@tanstack/react-table" {
  interface ColumnMeta<TData extends RowData, TValue> {
    /** The value or values a row files under in the column's filter (and its group). */
    facet?: (item: TData) => string | string[];
    /** A facet value as the reader sees it. */
    show?: (value: string) => string;
    /** The facet values in the order the filter lists them; by count when absent. */
    order?: readonly string[];
    /** Filter by typed text instead of picking values: for a column of free text. */
    text?: boolean;
    /** The type for `TValue`, so the declaration uses both parameters. */
    value?: TValue;
  }
}

/** One table row: an issue and what the table derives for it. */
interface Item {
  readonly issue: IssueSummary;
  readonly queue?: number;
  readonly epic: string;
  readonly epicKey?: string;
  readonly phase: string;
  readonly author: string;
  readonly claimedBy: string;
  readonly search: string;
}

const NO_EPIC = "No epic";
const NO_PHASE = "Not in Legion";
const NOBODY = "Nobody";
const RUNNING: ReadonlySet<string> = new Set(["in_progress", "testing", "needs_review", "retro"]);
const isQueued = (issue: IssueSummary) =>
  issue.status === "todo" && (issue.labels ?? []).includes("legion");
const isEpic = (issue: IssueSummary) =>
  issue.parent === null && (issue.labels ?? []).includes("epic") && issue.status !== "done";
const priorityName = (value: string) => (value === "" ? "No priority" : `P${value}`);

/** A date as the filter and the grouping file it: how long ago, in four bands. */
const AGES = ["Today", "This week", "This month", "Older"] as const;
function ageOf(iso: string): string {
  const days = (Date.now() - new Date(iso).getTime()) / 86_400_000;
  return days < 1 ? "Today" : days < 7 ? "This week" : days < 30 ? "This month" : "Older";
}

const facetsOf = (column: Column<Item, unknown>, item: Item): string[] => {
  const facet = column.columnDef.meta?.facet;
  const value = facet === undefined ? item.issue.key : facet(item);
  return Array.isArray(value) ? value : [value];
};

/** A row passes a pick filter when nothing is picked or one of its facet values is picked. */
const inPicks: FilterFn<Item> = (row, id, picks: string[]) => {
  if (picks.length === 0) return true;
  const column = row._getAllCellsByColumnId()[id]?.column;
  return column === undefined || facetsOf(column, row.original).some((v) => picks.includes(v));
};

/** Whether two words are at most one insertion, deletion or substitution apart. */
function withinOneEdit(a: string, b: string): boolean {
  if (Math.abs(a.length - b.length) > 1) return false;
  let i = 0;
  let j = 0;
  let edits = 0;
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
  return edits + (a.length - i) + (b.length - j) <= 1;
}

/** Every word of the query must appear in the row (match-sorter's contains ranking), or, for a
 *  word of five letters or more, a word of the row must be one typo away from it. */
function matchesSearch(item: Item, query: string): boolean {
  const haystack = item.search;
  const words = haystack.toLowerCase().split(/[^a-z0-9]+/);
  return query
    .trim()
    .toLowerCase()
    .split(/\s+/)
    .every(
      (word) =>
        rankItem(haystack, word, { threshold: rankings.CONTAINS }).passed ||
        (word.length >= 5 && words.some((candidate) => withinOneEdit(candidate, word)))
    );
}
const fuzzy: FilterFn<Item> = (row, _id, query: string) => matchesSearch(row.original, query);

/** One scale for the lifecycle: lightest at triage, darkest at done, so the bar reads left to
 *  right as work moving through. */
const statusTone: Record<string, string> = {
  triage: "bg-sky-100 text-sky-950",
  icebox: "bg-sky-200 text-sky-950",
  backlog: "bg-sky-300 text-sky-950",
  todo: "bg-sky-400 text-sky-950",
  in_progress: "bg-sky-500 text-white",
  testing: "bg-sky-600 text-white",
  needs_review: "bg-sky-700 text-white",
  retro: "bg-sky-800 text-white",
  done: "bg-sky-950 text-white",
};

interface Saved {
  sorting: SortingState;
  grouping: GroupingState;
  filters: ColumnFiltersState;
  visibility: VisibilityState;
  pageSize: number;
}
const defaults: Saved = {
  sorting: [{ id: "queue", desc: false }],
  grouping: [],
  filters: [{ id: "status", value: issueStatuses.filter((status) => status !== "done") }],
  visibility: { asks: false, updated: false },
  pageSize: 50,
};
/** The "All rows" page size: rows are virtualized, so one page of every row costs what 40 do. */
const ALL_ROWS = 100_000;
/** The unlabelled last column that holds a row's Start next button. */
const ACTION_WIDTH = 104;
/** Columns that stay put while the table scrolls sideways: what tells one row from another. */
const PINNED = ["select", "priority", "key", "title"];
/** Starting widths in pixels; a person's drag on a header edge overrides them. */
const COLUMN_SIZES: Record<string, number> = {
  select: 40,
  priority: 104,
  queue: 96,
  key: 104,
  title: 380,
  status: 128,
  phase: 140,
  epic: 220,
  assignee: 180,
  author: 200,
  labels: 200,
  claim: 220,
  asks: 96,
  created: 120,
  updated: 120,
};
const savedKey = (project: string) => `issues-table.v2.${project}`;
function loadSaved(project: string): Saved {
  try {
    return { ...defaults, ...JSON.parse(localStorage.getItem(savedKey(project)) ?? "{}") };
  } catch {
    return defaults;
  }
}

/** The view a link carries: search, sort, grouping, every column filter, page and page size, so
 *  a copied URL opens the same view and Back undoes a change. */
function readUrl(params: URLSearchParams): Partial<Saved> & { search?: string; page?: number } {
  const out: Partial<Saved> & { search?: string; page?: number } = {};
  const sort = params.get("sort");
  if (sort !== null) {
    out.sorting = sort
      .split(",")
      .filter(Boolean)
      .map((part) => ({ id: part.replace(/^-/, ""), desc: part.startsWith("-") }));
  }
  if (params.has("group")) out.grouping = params.get("group") ? [params.get("group") ?? ""] : [];
  const filterKeys = [...new Set([...params.keys()].filter((key) => key.startsWith("f.")))];
  if (filterKeys.length > 0 || params.has("nofilter")) {
    out.filters = filterKeys.map((key) => {
      const id = key.slice(2);
      return { id, value: id === "title" ? params.get(key) : params.getAll(key) };
    });
  }
  if (params.has("q")) out.search = params.get("q") ?? "";
  if (params.has("page")) out.page = Math.max(0, Number(params.get("page")) - 1);
  if (params.has("size")) out.pageSize = Number(params.get("size")) || defaults.pageSize;
  return out;
}

function writeUrl(
  params: URLSearchParams,
  state: {
    sorting: SortingState;
    grouping: GroupingState;
    filters: ColumnFiltersState;
    search: string;
    page: number;
    pageSize: number;
  }
): URLSearchParams {
  const next = new URLSearchParams(params);
  for (const key of [...next.keys()]) {
    if (key.startsWith("f.") || ["sort", "group", "q", "page", "size", "nofilter"].includes(key)) {
      next.delete(key);
    }
  }
  next.set("sort", state.sorting.map((sort) => `${sort.desc ? "-" : ""}${sort.id}`).join(","));
  next.set("group", state.grouping[0] ?? "");
  if (state.filters.length === 0) next.set("nofilter", "1");
  for (const filter of state.filters) {
    for (const value of Array.isArray(filter.value) ? filter.value : [filter.value]) {
      next.append(`f.${filter.id}`, String(value));
    }
  }
  if (state.search !== "") next.set("q", state.search);
  if (state.page > 0) next.set("page", String(state.page + 1));
  next.set("size", String(state.pageSize));
  return next;
}

interface Layout {
  sizing: ColumnSizingState;
  order: ColumnOrderState;
}
const layoutKey = (project: string) => `issues-table.layout.${project}`;
function loadLayout(project: string): Layout {
  try {
    return {
      sizing: {},
      order: [],
      ...JSON.parse(localStorage.getItem(layoutKey(project)) ?? "{}"),
    };
  } catch {
    return { sizing: {}, order: [] };
  }
}

export function IssuesTable({ project }: { project: string }): ReactNode {
  const queryClient = useQueryClient();
  const text = useAgentText();
  const login = useQuery(whoAmIQuery()).data?.login;
  const [params, setParams] = useSearchParams();
  const legion = useQuery({
    queryKey: ["prototype-legion-phases"],
    queryFn: async () => {
      const response = await fetch("/prototype/legion-phases.json");
      return response.ok
        ? ((await response.json()) as { snapshotAt: string; phases: Record<string, string> })
        : null;
    },
    staleTime: Number.POSITIVE_INFINITY,
  });
  // A link's view wins over the one this browser last left; either wins over the defaults. Read
  // once per project: the URL is written from the state after that, never read back.
  // biome-ignore lint/correctness/useExhaustiveDependencies: the URL seeds the state only once
  const initial = useMemo(() => ({ ...loadSaved(project), ...readUrl(params) }), [project]);
  const layout = useMemo(() => loadLayout(project), [project]);
  const [sorting, setSorting] = useState<SortingState>(initial.sorting);
  const [grouping, setGrouping] = useState<GroupingState>(initial.grouping);
  const [filters, setFilters] = useState<ColumnFiltersState>(initial.filters);
  const [visibility, setVisibility] = useState<VisibilityState>(initial.visibility);
  const [pagination, setPagination] = useState<PaginationState>({
    pageIndex: initial.page ?? 0,
    pageSize: initial.pageSize,
  });
  const [search, setSearch] = useState(initial.search ?? "");
  const [sizing, setSizing] = useState<ColumnSizingState>(layout.sizing);
  const [order, setOrder] = useState<ColumnOrderState>(layout.order);
  const [expanded, setExpanded] = useState<ExpandedState>(true);
  const [selection, setSelection] = useState<RowSelectionState>({});
  const [openFilter, setOpenFilter] = useState<string>();
  const [dragColumn, setDragColumn] = useState<string>();
  const [moved, setMoved] = useState<ReadonlySet<string>>(new Set());
  const [notice, setNotice] = useState<string>();
  const [busy, setBusy] = useState(false);
  useEffect(() => {
    localStorage.setItem(
      savedKey(project),
      JSON.stringify({ sorting, grouping, filters, visibility, pageSize: pagination.pageSize })
    );
    setParams(
      (current) =>
        writeUrl(current, {
          sorting,
          grouping,
          filters,
          search,
          page: pagination.pageIndex,
          pageSize: pagination.pageSize,
        }),
      { replace: true }
    );
  }, [project, sorting, grouping, filters, visibility, search, pagination, setParams]);
  useEffect(() => {
    localStorage.setItem(layoutKey(project), JSON.stringify({ sizing, order }));
  }, [project, sizing, order]);

  // Open issues load first and are what a person works on; closed ones (the bulk of a project
  // over time) load only once the Status filter lets Done through.
  const statusPicked = (filters.find((filter) => filter.id === "status")?.value ?? []) as string[];
  const wantsDone = statusPicked.length === 0 || statusPicked.includes("done");
  const openIssues = useQuery({
    queryKey: [...projectIssuesQueryKey(project, []), "open"],
    queryFn: () => api.listIssues({ project, open: true }),
  });
  const everyIssue = useQuery({ ...projectIssuesQuery(project, []), enabled: wantsDone });
  const issues = wantsDone && everyIssue.data !== undefined ? everyIssue : openIssues;
  const all = issues.data ?? [];
  const epics = useMemo(() => all.filter(isEpic), [all]);
  const items = useMemo<Item[]>(() => {
    const byKey = new Map(all.map((issue) => [issue.key, issue]));
    const queue = all
      .filter(isQueued)
      .sort((a, b) => (a.rank < b.rank ? -1 : a.rank > b.rank ? 1 : 0));
    const position = new Map(queue.map((issue, index) => [issue.key, index + 1]));
    const phases = legion.data?.phases ?? {};
    return all
      .filter((issue) => !isEpic(issue))
      .map((issue) => {
        let root: IssueSummary | undefined = issue;
        for (let depth = 0; root?.parent && depth < 30; depth++) root = byKey.get(root.parent);
        const epic = root !== undefined && root !== issue && isEpic(root) ? root : undefined;
        const epicTitle = epic?.title ?? NO_EPIC;
        const author = issue.created_by ? text.label(issue.created_by) : "Unknown";
        const claimedBy = issue.claim === null ? NOBODY : text.label(issue.claim.actor);
        return {
          issue,
          queue: position.get(issue.key),
          epic: epicTitle,
          epicKey: epic?.key,
          phase: phases[issue.key] ?? NO_PHASE,
          author,
          claimedBy,
          search: [
            issue.key,
            issue.title,
            epicTitle,
            issue.assignee ?? "",
            author,
            (issue.labels ?? []).join(" "),
          ].join(" "),
        };
      });
  }, [all, legion.data, text]);

  const columns = useMemo<ColumnDef<Item>[]>(
    () =>
      (
        [
          {
            id: "select",
            enableSorting: false,
            enableHiding: false,
            enableColumnFilter: false,
            enableGrouping: false,
            header: ({ table }) => (
              <input
                aria-label="Select every row on this page"
                checked={table.getIsAllPageRowsSelected()}
                className={checkboxAccent}
                onChange={table.getToggleAllPageRowsSelectedHandler()}
                type="checkbox"
              />
            ),
            cell: ({ row }) => (
              <input
                aria-label={`Select ${row.original.issue.key}`}
                checked={row.getIsSelected()}
                className={checkboxAccent}
                onChange={row.getToggleSelectedHandler()}
                type="checkbox"
              />
            ),
          },
          {
            id: "priority",
            header: "Priority",
            accessorFn: (item) => item.issue.priority ?? undefined,
            sortUndefined: "last",
            sortDescFirst: false,
            filterFn: inPicks,
            getGroupingValue: (item) => String(item.issue.priority ?? ""),
            meta: {
              facet: (item) => String(item.issue.priority ?? ""),
              show: priorityName,
              order: ["0", "1", "2", "3", ""],
            },
            cell: ({ row }) => (
              <PriorityControl
                disabled={row.original.issue.status === "done"}
                issueKey={row.original.issue.key}
                priority={row.original.issue.priority}
              />
            ),
          },
          {
            id: "queue",
            header: "Queue",
            accessorFn: (item) => item.queue,
            sortUndefined: "last",
            sortDescFirst: false,
            filterFn: inPicks,
            enableGrouping: false,
            meta: {
              facet: (item) => (item.queue === undefined ? "Not queued" : "Queued"),
              order: ["Queued", "Not queued"],
            },
            cell: ({ getValue }) => {
              const place = getValue<number | undefined>();
              return place === undefined ? null : (
                <span
                  className="font-semibold tabular-nums"
                  title="Legion starts issues in this order"
                >
                  #{place}
                </span>
              );
            },
          },
          {
            id: "key",
            header: "Key",
            accessorFn: (item) => item.issue.key,
            sortingFn: (a, b) =>
              Number(a.original.issue.key.split("-")[1]) -
              Number(b.original.issue.key.split("-")[1]),
            filterFn: inPicks,
            enableGrouping: false,
            meta: { facet: (item) => item.issue.key },
            cell: ({ row }) => (
              <span className="font-semibold whitespace-nowrap">{row.original.issue.key}</span>
            ),
          },
          {
            id: "title",
            header: "Title",
            accessorFn: (item) => item.issue.title,
            filterFn: "includesString",
            enableGrouping: false,
            meta: { text: true },
            cell: ({ row }) => (
              <Link
                className={`${linkText} ${linkHoverText}`}
                to={buildIssuePath({ key: row.original.issue.key, kind: "issue" })}
                {...referenceTriggerProps({ key: row.original.issue.key, kind: "issue" })}
              >
                <span className={textPrimaryOnCanvas}>{row.original.issue.title}</span>
              </Link>
            ),
          },
          {
            id: "status",
            header: "Status",
            accessorFn: (item) => item.issue.status,
            sortingFn: (a, b) =>
              issueStatuses.indexOf(a.original.issue.status as IssueStatus) -
              issueStatuses.indexOf(b.original.issue.status as IssueStatus),
            filterFn: inPicks,
            meta: {
              facet: (item) => item.issue.status,
              show: (value) => statusLabel(value as IssueStatus),
              order: issueStatuses,
            },
            cell: ({ getValue }) => <StatusPill>{statusLabel(getValue<IssueStatus>())}</StatusPill>,
          },
          {
            id: "phase",
            header: "Legion phase",
            accessorFn: (item) => item.phase,
            filterFn: inPicks,
            meta: { facet: (item) => item.phase },
            cell: ({ getValue }) => {
              const phase = getValue<string>();
              return phase === NO_PHASE ? null : (
                <span className={`whitespace-nowrap ${textSecondaryOnCanvas}`}>{phase}</span>
              );
            },
          },
          {
            id: "epic",
            header: "Epic",
            accessorFn: (item) => item.epic,
            filterFn: inPicks,
            meta: { facet: (item) => item.epic },
            cell: ({ getValue }) => (
              <span className={`line-clamp-2 ${textSecondaryOnCanvas}`}>{getValue<string>()}</span>
            ),
          },
          {
            id: "assignee",
            header: "Assignee",
            accessorFn: (item) => item.issue.assignee ?? "Unassigned",
            filterFn: inPicks,
            meta: { facet: (item) => item.issue.assignee ?? "Unassigned" },
            cell: ({ getValue }) => (
              <span className={`whitespace-nowrap ${textSecondaryOnCanvas}`}>
                {getValue<string>()}
              </span>
            ),
          },
          {
            id: "author",
            header: "Created by",
            accessorFn: (item) => item.author,
            filterFn: inPicks,
            meta: { facet: (item) => item.author },
            cell: ({ getValue }) => (
              <span className={`line-clamp-2 ${textSecondaryOnCanvas}`}>{getValue<string>()}</span>
            ),
          },
          {
            id: "labels",
            header: "Labels",
            accessorFn: (item) => (item.issue.labels ?? []).join(", "),
            filterFn: inPicks,
            enableGrouping: false,
            meta: { facet: (item) => item.issue.labels ?? [] },
            cell: ({ row }) => (
              <span className="flex flex-wrap gap-1">
                {(row.original.issue.labels ?? []).map((label) => (
                  <LabelPill key={label}>{label}</LabelPill>
                ))}
              </span>
            ),
          },
          {
            id: "claim",
            header: "Claimed by",
            accessorFn: (item) => item.claimedBy,
            filterFn: inPicks,
            meta: { facet: (item) => item.claimedBy },
            cell: ({ row }) => <ClaimChip claim={row.original.issue.claim} />,
          },
          {
            id: "asks",
            header: "Open asks",
            accessorFn: (item) => item.issue.open_asks,
            sortDescFirst: true,
            filterFn: inPicks,
            enableGrouping: false,
            meta: { facet: (item) => String(item.issue.open_asks) },
            cell: ({ getValue }) =>
              getValue<number>() === 0 ? null : <AttentionBadge count={getValue<number>()} />,
          },
          {
            id: "created",
            header: "Created",
            accessorFn: (item) => item.issue.created_at ?? item.issue.updated_at,
            filterFn: inPicks,
            getGroupingValue: (item) => ageOf(item.issue.created_at ?? item.issue.updated_at),
            meta: {
              facet: (item) => ageOf(item.issue.created_at ?? item.issue.updated_at),
              order: AGES,
            },
            cell: ({ getValue }) => (
              <span className={`whitespace-nowrap text-xs ${textMutedOnCanvas}`}>
                <Timestamp at={getValue<string>()} />
              </span>
            ),
          },
          {
            id: "updated",
            header: "Updated",
            accessorFn: (item) => item.issue.updated_at,
            filterFn: inPicks,
            getGroupingValue: (item) => ageOf(item.issue.updated_at),
            meta: { facet: (item) => ageOf(item.issue.updated_at), order: AGES },
            cell: ({ getValue }) => (
              <span className={`whitespace-nowrap text-xs ${textMutedOnCanvas}`}>
                <Timestamp at={getValue<string>()} />
              </span>
            ),
          },
        ] as ColumnDef<Item>[]
      ).map((column) => ({
        ...column,
        size: COLUMN_SIZES[column.id ?? ""] ?? 140,
        minSize: column.id === "select" ? 36 : 64,
        enableResizing: column.id !== "select",
      })),
    []
  );

  const table = useReactTable({
    data: items,
    columns,
    state: {
      sorting,
      grouping,
      columnFilters: filters,
      columnVisibility: visibility,
      pagination,
      globalFilter: search,
      expanded,
      rowSelection: selection,
      columnSizing: sizing,
      columnOrder: order,
      // What identifies a row stays in view while the rest scrolls sideways.
      columnPinning: { left: PINNED },
    },
    getRowId: (item) => item.issue.key,
    onColumnSizingChange: setSizing,
    onColumnOrderChange: setOrder,
    columnResizeMode: "onChange",
    onSortingChange: setSorting,
    onGroupingChange: setGrouping,
    onColumnFiltersChange: (update) => {
      setFilters(update);
      setPagination((page) => ({ ...page, pageIndex: 0 }));
    },
    onColumnVisibilityChange: setVisibility,
    onPaginationChange: setPagination,
    onGlobalFilterChange: setSearch,
    onExpandedChange: setExpanded,
    onRowSelectionChange: setSelection,
    globalFilterFn: fuzzy,
    getColumnCanGlobalFilter: (column) => column.id === "title",
    enableRowSelection: (row) => !row.getIsGrouped(),
    autoResetPageIndex: false,
    // A page holds leaf rows, so a big group spans pages instead of making one page huge.
    paginateExpandedRows: true,
    // A new grouping keeps every group open, and the grouped column stays where it was.
    autoResetExpanded: false,
    groupedColumnMode: false,
    getCoreRowModel: getCoreRowModel(),
    getFilteredRowModel: getFilteredRowModel(),
    getSortedRowModel: getSortedRowModel(),
    getGroupedRowModel: getGroupedRowModel(),
    getExpandedRowModel: getExpandedRowModel(),
    getPaginationRowModel: getPaginationRowModel(),
    getFacetedRowModel: getFacetedRowModel(),
  });

  /** Each action is one or more ordinary issue writes, then one reload of the list. */
  const run = async (
    said: string,
    writes: (() => Promise<unknown>)[],
    highlight: readonly string[] = []
  ) => {
    setBusy(true);
    try {
      for (const write of writes) await write();
      setNotice(said);
      setMoved(new Set(highlight));
      setSelection({});
    } catch (cause) {
      setNotice(cause instanceof Error ? cause.message : String(cause));
    } finally {
      setBusy(false);
      await queryClient.invalidateQueries({ queryKey: projectIssuesQueryKey(project, []) });
    }
  };
  const patch = (key: string, input: UpdateIssueInput) => () =>
    api.patchIssue(key, input, { key: crypto.randomUUID(), login });
  const titleOf = (key: string) => all.find((issue) => issue.key === key)?.title ?? key;
  const named = (keys: readonly string[]) =>
    keys.length === 1 ? `${keys[0]} “${titleOf(keys[0] ?? "")}”` : `${keys.length} issues`;
  const queueHead = items
    .filter((item) => item.queue !== undefined)
    .sort((a, b) => (a.queue ?? 0) - (b.queue ?? 0));
  const startNext = (keys: readonly string[]) => {
    const queued = keys.filter((key) => items.some((item) => item.issue.key === key && item.queue));
    const head = queueHead.find((item) => !queued.includes(item.issue.key));
    if (queued.length === 0 || head === undefined) return;
    void run(
      `${named(queued)} now ${queued.length === 1 ? "starts" : "start"} next.`,
      [...queued].reverse().map((key, index, reversed) =>
        patch(key, {
          rank: { before: index === 0 ? head.issue.key : (reversed[index - 1] ?? "") },
        })
      ),
      queued
    );
  };
  const moveTo = (keys: readonly string[], epic: string | null) =>
    run(
      epic === null
        ? `Took ${named(keys)} out of ${keys.length === 1 ? "its epic" : "their epics"}.`
        : `Moved ${named(keys)} to the epic “${titleOf(epic)}”.`,
      keys.map((key) => patch(key, { parent: epic })),
      keys
    );
  const newEpic = async (keys: readonly string[]) => {
    const title = window.prompt(`Name the new epic for ${named(keys)}`);
    if (!title) return;
    let created = "";
    await run(
      `Made the epic “${title}” from ${named(keys)}.`,
      [
        async () => {
          created = (
            await api.createIssue(
              { project, title, labels: ["epic"], force: true },
              { key: crypto.randomUUID(), login }
            )
          ).key;
        },
        ...keys.map((key) => async () => patch(key, { parent: created })()),
      ],
      keys
    );
  };
  const renameEpic = (key: string) => {
    const title = window.prompt("Rename the epic", titleOf(key));
    if (title && title !== titleOf(key)) {
      void run(`Renamed the epic to “${title}”.`, [patch(key, { title })]);
    }
  };
  const mergeEpic = (from: string, into: string) => {
    const members = items
      .filter((item) => item.issue.parent === from)
      .map((item) => item.issue.key);
    void run(
      `Merged “${titleOf(from)}” into “${titleOf(into)}”.`,
      [
        ...members.map((key) => patch(key, { parent: into })),
        // A close needs its reason posted first, as `dispatch issue-update --status done` does.
        () =>
          api.createMessage(
            from,
            { body: `Merged into ${into} (“${titleOf(into)}”) from the issues table.` },
            { key: crypto.randomUUID(), login }
          ),
        patch(from, { status: "done" }),
      ],
      members
    );
  };

  // The table scrolls in a box of its own that ends at the bottom of the window, so its header
  // sticks (a sticky header sticks to its nearest scrolling ancestor), its sideways scrollbar is
  // always on screen, and the pager under it never scrolls away.
  const scrollRef = useRef<HTMLDivElement>(null);
  const [boxTop, setBoxTop] = useState(0);
  useLayoutEffect(() => {
    const place = () =>
      setBoxTop((scrollRef.current?.getBoundingClientRect().top ?? 0) + window.scrollY);
    place();
    window.addEventListener("resize", place);
    return () => window.removeEventListener("resize", place);
  });
  // A page past the first that opens in the middle of a group repeats that group's header.
  const pageRows = table.getRowModel().rows;
  const continued =
    pageRows[0] !== undefined && !pageRows[0].getIsGrouped()
      ? pageRows[0].getParentRow()
      : undefined;
  const displayRows: { row: Row<Item>; continued?: boolean }[] = [
    ...(continued === undefined ? [] : [{ row: continued, continued: true }]),
    ...pageRows.map((row) => ({ row })),
  ];
  // Only the rows in view are in the page: 2,000 rows on one page cost what 40 do.
  const virtualizer = useVirtualizer({
    count: displayRows.length,
    getScrollElement: () => scrollRef.current,
    estimateSize: (index) => (displayRows[index]?.row.getIsGrouped() ? 48 : 64),
    overscan: 12,
  });
  /** The issues every filter but this column's lets through, with the search: a column's counts
   *  say what picking a value would show, as faceted search does. */
  const facetItems = (columnId: string) =>
    items.filter(
      (item) =>
        (search.trim() === "" || matchesSearch(item, search)) &&
        filters.every((filter) => {
          if (filter.id === columnId) return true;
          const column = table.getColumn(filter.id);
          if (column === undefined) return true;
          if (column.columnDef.meta?.text) {
            return item.issue.title.toLowerCase().includes(String(filter.value).toLowerCase());
          }
          const picks = filter.value as string[];
          return (
            picks.length === 0 || facetsOf(column, item).some((value) => picks.includes(value))
          );
        })
    );

  if (issues.isPending) return <LoadingSkeleton label="Loading issues" />;
  if (issues.isError) return <p className={dangerText}>Could not load issues.</p>;

  const statusColumn = table.getColumn("status");
  const statusPicks = (statusColumn?.getFilterValue() as string[] | undefined) ?? [];
  const statusCounts = new Map<string, number>();
  for (const item of items) {
    statusCounts.set(item.issue.status, (statusCounts.get(item.issue.status) ?? 0) + 1);
  }
  const statusTotal = items.length;
  const selectedKeys = table.getSelectedRowModel().flatRows.map((row) => row.original.issue.key);
  // Filtering runs before grouping, so the filtered rows are the issues and the grouped rows the
  // groups.
  const leafCount = table.getFilteredRowModel().rows.length;
  const groupCount = table.getGroupedRowModel().rows.length;
  const queuedCount = items.filter((item) => item.queue !== undefined).length;
  const runningCount = items.filter((item) => RUNNING.has(item.issue.status)).length;
  const { pageIndex, pageSize } = table.getState().pagination;
  const rowsOnPages = table.getPrePaginationRowModel().rows.length;
  const visibleColumns = table.getVisibleLeafColumns().length + 1;

  return (
    <section aria-label="Project issues" className="space-y-3 pb-24">
      {/* Issues by status: a click shows only that status, a second click every status. */}
      <div className={`rounded-xl border p-3 ${borderDefault}`}>
        <div
          className={`mb-2 flex flex-wrap items-baseline gap-x-5 gap-y-1 text-sm ${textSecondaryOnCanvas}`}
        >
          <span>
            <b className={`text-lg tabular-nums ${textPrimaryOnCanvas}`}>{queuedCount}</b> in
            Legion's queue
          </span>
          <span>
            <b className={`text-lg tabular-nums ${textPrimaryOnCanvas}`}>{runningCount}</b> running
          </span>
          <span>
            <b className={`text-lg tabular-nums ${textPrimaryOnCanvas}`}>{epics.length}</b> epics
          </span>
          <span className={`ml-auto text-xs ${textMutedOnCanvas}`}>
            {legion.data
              ? `Legion phases from the daemon's state at ${legion.data.snapshotAt.slice(0, 16).replace("T", " ")}Z`
              : "No Legion phase snapshot"}
          </span>
        </div>
        <div className={`flex h-6 overflow-hidden rounded-md ${surfaceMutedBg}`}>
          {issueStatuses.map((status) => {
            const count = statusCounts.get(status) ?? 0;
            if (count === 0) return null;
            const on = statusPicks.length === 0 || statusPicks.includes(status);
            return (
              <button
                className={`min-w-0 overflow-hidden px-1 text-left text-[11px] font-medium whitespace-nowrap ${statusTone[status]} ${on ? "" : "opacity-30"}`}
                key={status}
                onClick={() =>
                  statusColumn?.setFilterValue(
                    statusPicks.length === 1 && statusPicks[0] === status ? undefined : [status]
                  )
                }
                style={{ flex: count }}
                title={`${statusLabel(status)}: ${count}`}
                type="button"
              >
                {count / Math.max(statusTotal, 1) > 0.06
                  ? `${statusLabel(status)} ${count}`
                  : count}
              </button>
            );
          })}
          {statusCounts.has("done") ? null : (
            <button
              className={`shrink-0 px-2 text-[11px] font-medium ${statusTone.done}`}
              onClick={() => statusColumn?.setFilterValue(["done"])}
              title="Closed issues load when you show them"
              type="button"
            >
              Done: load
            </button>
          )}
        </div>
      </div>

      <div className="flex flex-wrap items-center gap-2">
        <input
          aria-label="Search every issue"
          className={`min-w-64 flex-1 rounded-lg border px-3 py-1.5 text-sm ${borderDefault} ${surfaceBg} ${textPrimaryOnCanvas}`}
          onChange={(event) => {
            setSearch(event.target.value);
            setPagination((page) => ({ ...page, pageIndex: 0 }));
          }}
          placeholder="Search issues: key, title, epic, assignee, author, labels"
          type="search"
          value={search}
        />
        <label className={`flex items-center gap-2 text-sm ${textSecondaryOnCanvas}`}>
          Group by
          <select
            className={`rounded-lg border px-2 py-1.5 ${borderDefault} ${surfaceBg}`}
            onChange={(event) => {
              setGrouping(event.target.value ? [event.target.value] : []);
              setExpanded(true);
            }}
            value={grouping[0] ?? ""}
          >
            <option value="">Nothing</option>
            {table
              .getAllLeafColumns()
              .filter((column) => column.getCanGroup())
              .map((column) => (
                <option key={column.id} value={column.id}>
                  {String(column.columnDef.header)}
                </option>
              ))}
          </select>
        </label>
        <ColumnsMenu columns={table.getAllLeafColumns()} />
        {filters.length > 0 || search !== "" ? (
          <button
            className={`rounded-lg border px-2 py-1.5 text-sm ${borderDefault} ${surfaceMutedHoverBg}`}
            onClick={() => {
              setFilters([]);
              setSearch("");
            }}
            type="button"
          >
            Clear filters
          </button>
        ) : null}
      </div>

      {notice === undefined ? null : (
        <p aria-live="polite" className={`text-sm ${textSecondaryOnCanvas}`}>
          {notice}
        </p>
      )}

      <div
        className={`overflow-auto rounded-xl border ${borderDefault}`}
        ref={scrollRef}
        style={{ maxHeight: `max(360px, calc(100dvh - ${Math.round(boxTop)}px - 64px))` }}
      >
        <table
          className="table-fixed border-separate border-spacing-0 text-sm"
          style={{ width: table.getTotalSize() + ACTION_WIDTH }}
        >
          <thead>
            {table.getHeaderGroups().map((group) => (
              <tr key={group.id}>
                {group.headers.map((header) => {
                  const pinned = header.column.getIsPinned() === "left";
                  return (
                    <th
                      className={`sticky top-0 border-b px-2 py-2 text-left align-middle text-xs font-semibold ${borderDefault} ${surfaceBg} ${textSecondaryOnCanvas} ${pinned ? "z-30" : "z-20"} ${pinned && header.column.getIsLastColumn("left") ? "border-r" : ""}`}
                      key={header.id}
                      onDragOver={(event) => {
                        if (dragColumn !== undefined && !pinned) event.preventDefault();
                      }}
                      onDrop={() => {
                        if (dragColumn === undefined || pinned || dragColumn === header.column.id) {
                          return;
                        }
                        const current = table.getAllLeafColumns().map((column) => column.id);
                        const next = current.filter((id) => id !== dragColumn);
                        next.splice(next.indexOf(header.column.id), 0, dragColumn);
                        setOrder(next);
                        setDragColumn(undefined);
                      }}
                      style={{
                        width: header.getSize(),
                        left: pinned ? header.column.getStart("left") : undefined,
                      }}
                    >
                      <span className="flex items-center justify-between gap-1">
                        {header.column.getCanSort() ? (
                          <button
                            className="flex min-w-0 items-center gap-1 truncate whitespace-nowrap"
                            draggable={!pinned}
                            onClick={header.column.getToggleSortingHandler()}
                            onDragEnd={() => setDragColumn(undefined)}
                            onDragStart={() => setDragColumn(header.column.id)}
                            title={
                              pinned
                                ? "Sort (shift-click to add a second sort)"
                                : "Sort (shift-click to add a second sort); drag to move the column"
                            }
                            type="button"
                          >
                            {flexRender(header.column.columnDef.header, header.getContext())}
                            <span className={textMutedOnCanvas}>
                              {{ asc: "▲", desc: "▼" }[header.column.getIsSorted() as string] ?? ""}
                            </span>
                          </button>
                        ) : (
                          flexRender(header.column.columnDef.header, header.getContext())
                        )}
                        {header.column.getCanFilter() ? (
                          <ColumnFilter
                            column={header.column}
                            items={
                              openFilter === header.column.id ? facetItems(header.column.id) : items
                            }
                            onOpenChange={(open) =>
                              setOpenFilter(open ? header.column.id : undefined)
                            }
                            open={openFilter === header.column.id}
                          />
                        ) : null}
                      </span>
                      {header.column.getCanResize() ? (
                        <span
                          aria-hidden
                          className={`absolute top-0 right-0 h-full w-1.5 cursor-col-resize touch-none select-none ${header.column.getIsResizing() ? "bg-sky-500" : "hover:bg-sky-300"}`}
                          onDoubleClick={() => header.column.resetSize()}
                          onMouseDown={header.getResizeHandler()}
                          onTouchStart={header.getResizeHandler()}
                        />
                      ) : null}
                    </th>
                  );
                })}
                <th
                  className={`sticky top-0 z-20 border-b ${borderDefault} ${surfaceBg}`}
                  style={{ width: ACTION_WIDTH }}
                />
              </tr>
            ))}
          </thead>
          <tbody>
            {virtualizer.getVirtualItems()[0] ? (
              <tr aria-hidden style={{ height: virtualizer.getVirtualItems()[0]?.start ?? 0 }} />
            ) : null}
            {virtualizer.getVirtualItems().map((virtual) => {
              const entry = displayRows[virtual.index];
              if (entry === undefined) return null;
              const { row } = entry;
              return row.getIsGrouped() ? (
                <GroupRow
                  columns={visibleColumns}
                  continued={entry.continued}
                  epics={epics}
                  grouping={grouping[0] ?? ""}
                  index={virtual.index}
                  key={`${entry.continued ? "continued-" : ""}${row.id}`}
                  measure={virtualizer.measureElement}
                  onMerge={mergeEpic}
                  onRename={renameEpic}
                  onStartAll={(keys) => startNext(keys)}
                  row={row}
                />
              ) : (
                <tr
                  className={`group align-top ${moved.has(row.original.issue.key) ? selectedCardBg : ""}`}
                  data-index={virtual.index}
                  key={row.id}
                  ref={virtualizer.measureElement}
                >
                  {row.getVisibleCells().map((cell) => {
                    const pinned = cell.column.getIsPinned() === "left";
                    return (
                      <td
                        className={`overflow-hidden border-b px-2 py-2 ${borderDefault} ${pinned ? `sticky z-10 ${moved.has(row.original.issue.key) ? selectedCardBg : surfaceBg}` : ""} ${pinned && cell.column.getIsLastColumn("left") ? "border-r" : ""} ${row.depth > 0 && cell.column.id === "select" ? "pl-6" : ""} ${cell.column.id === "title" ? "[&_a]:line-clamp-2" : ""}`}
                        key={cell.id}
                        style={{
                          width: cell.column.getSize(),
                          left: pinned ? cell.column.getStart("left") : undefined,
                        }}
                      >
                        {flexRender(cell.column.columnDef.cell, cell.getContext())}
                      </td>
                    );
                  })}
                  <td className={`border-b px-2 py-2 text-right ${borderDefault}`}>
                    {row.original.queue !== undefined && row.original.queue > 1 ? (
                      <button
                        className={`rounded-md border px-2 py-0.5 text-xs whitespace-nowrap opacity-0 group-hover:opacity-100 focus:opacity-100 ${borderDefault} ${surfaceBg}`}
                        disabled={busy}
                        onClick={() => startNext([row.original.issue.key])}
                        type="button"
                      >
                        Start next
                      </button>
                    ) : null}
                  </td>
                </tr>
              );
            })}
            {virtualizer.getVirtualItems().length > 0 ? (
              <tr
                aria-hidden
                style={{
                  height:
                    virtualizer.getTotalSize() - (virtualizer.getVirtualItems().at(-1)?.end ?? 0),
                }}
              />
            ) : null}
          </tbody>
        </table>
        {leafCount === 0 ? (
          <p className={`p-6 text-center text-sm ${textMutedOnCanvas}`}>No issues match.</p>
        ) : null}
      </div>

      <div className={`flex flex-wrap items-center gap-3 text-sm ${textSecondaryOnCanvas}`}>
        <span>
          {leafCount} of {items.length} issues
          {grouping.length > 0 ? ` in ${groupCount} groups` : ""}
        </span>
        <span className="ml-auto">
          {pageSize >= ALL_ROWS
            ? `All ${rowsOnPages} rows`
            : `Rows ${rowsOnPages === 0 ? 0 : pageIndex * pageSize + 1}–${Math.min((pageIndex + 1) * pageSize, rowsOnPages)} of ${rowsOnPages}`}
        </span>
        <Action disabled={!table.getCanPreviousPage()} onClick={() => table.previousPage()}>
          ‹ Previous
        </Action>
        <span>
          Page {pageIndex + 1} of {Math.max(table.getPageCount(), 1)}
        </span>
        <Action disabled={!table.getCanNextPage()} onClick={() => table.nextPage()}>
          Next ›
        </Action>
        <select
          aria-label="Rows per page"
          className={`rounded-lg border px-2 py-1 ${borderDefault} ${surfaceBg}`}
          onChange={(event) => table.setPageSize(Number(event.target.value))}
          value={pageSize}
        >
          {[25, 50, 100, 250, ALL_ROWS].map((size) => (
            <option key={size} value={size}>
              {size >= ALL_ROWS ? "All rows" : `${size} per page`}
            </option>
          ))}
        </select>
      </div>

      {/* Over the page, not in it: selecting a row moves nothing else. */}
      {selectedKeys.length === 0 ? null : (
        <div
          className={`fixed bottom-4 left-1/2 z-30 flex -translate-x-1/2 flex-wrap items-center gap-2 rounded-2xl border px-4 py-2 text-sm shadow-xl ${card} ${textPrimaryOnCanvas}`}
          role="toolbar"
        >
          <span className="mr-1 font-semibold">{selectedKeys.length} selected</span>
          {selectedKeys.length < leafCount ? (
            <Action disabled={busy} onClick={() => table.toggleAllRowsSelected(true)}>
              Select all {leafCount} matching
            </Action>
          ) : null}
          <Action disabled={busy} onClick={() => startNext(selectedKeys)}>
            Start next
          </Action>
          <select
            aria-label="Move to epic"
            className={`rounded-lg border px-2 py-1 ${borderDefault} ${surfaceBg}`}
            disabled={busy}
            onChange={(event) => {
              if (event.target.value) void moveTo(selectedKeys, event.target.value);
            }}
            value=""
          >
            <option value="">Move to epic…</option>
            {epics.map((epic) => (
              <option key={epic.key} value={epic.key}>
                {epic.title}
              </option>
            ))}
          </select>
          <Action disabled={busy} onClick={() => void moveTo(selectedKeys, null)}>
            Take out of epic
          </Action>
          <Action disabled={busy} onClick={() => void newEpic(selectedKeys)}>
            New epic from these
          </Action>
          <Action disabled={busy} onClick={() => setSelection({})}>
            Clear selection
          </Action>
        </div>
      )}
    </section>
  );
}

function FunnelIcon(): ReactNode {
  return (
    <svg aria-hidden="true" className="size-3.5" fill="currentColor" viewBox="0 0 16 16">
      <title>Filter</title>
      <path d="M1.5 2h13l-5 6.2V13l-3 1.5V8.2z" />
    </svg>
  );
}

const filterTrigger = (active: boolean) =>
  `rounded p-1 ${active ? "bg-sky-100 text-sky-700 dark:bg-sky-900 dark:text-sky-300" : `${textMutedOnCanvas} ${surfaceMutedHoverBg}`}`;

/** The filter button at a column header's right: Dispatch's searchable multi-select over the
 *  column's values with how many issues each holds, or typed text for a free-text column. */
function ColumnFilter({
  column,
  items,
  open,
  onOpenChange,
}: {
  column: Column<Item, unknown>;
  items: readonly Item[];
  open: boolean;
  onOpenChange: (open: boolean) => void;
}): ReactNode {
  const meta = column.columnDef.meta;
  const name = String(column.columnDef.header);
  if (meta?.text) {
    const value = (column.getFilterValue() as string | undefined) ?? "";
    return (
      <Popover
        align="right"
        label={<FunnelIcon />}
        labelClassName={filterTrigger(value !== "")}
        title={`Filter ${name}`}
      >
        {() => (
          <input
            aria-label={`Filter ${name}`}
            className={`m-1 rounded-md border px-2 py-1 text-sm ${borderDefault} ${surfaceBg}`}
            // biome-ignore lint/a11y/noAutofocus: the popover opens to type a filter
            autoFocus
            onChange={(event) => column.setFilterValue(event.target.value || undefined)}
            placeholder={`${name} contains…`}
            value={value}
          />
        )}
      </Popover>
    );
  }
  const picks = (column.getFilterValue() as string[] | undefined) ?? [];
  const counts = new Map<string, number>();
  for (const item of items) {
    for (const value of facetsOf(column, item)) counts.set(value, (counts.get(value) ?? 0) + 1);
  }
  const order = meta?.order;
  const options = [...counts.keys()].sort((a, b) =>
    order === undefined
      ? (counts.get(b) ?? 0) - (counts.get(a) ?? 0)
      : order.indexOf(a) - order.indexOf(b)
  );
  const show = meta?.show ?? ((value: string) => value);
  return (
    <MultiSelect
      emptyMessage="No matching value."
      label={name}
      onChange={(next) => column.setFilterValue(next.length === 0 ? undefined : next)}
      onOpenChange={onOpenChange}
      open={open}
      optionDetail={(value) => String(counts.get(value) ?? 0)}
      optionDetailLabel={(value) => `${counts.get(value) ?? 0} issues`}
      optionLabel={show}
      options={options}
      searchLabel={`Search ${name}`}
      selected={picks}
      triggerAriaLabel={
        picks.length === 0 ? `Filter ${name}` : `Filter ${name}: ${picks.length} picked`
      }
      triggerClassName={filterTrigger(picks.length > 0)}
    >
      <FunnelIcon />
    </MultiSelect>
  );
}

/** A group's header row: its value, how many issues, its issues by status, and for an epic its
 *  actions. */
function GroupRow({
  row,
  grouping,
  columns,
  epics,
  onRename,
  onMerge,
  onStartAll,
  continued,
  index,
  measure,
}: {
  /** A repeat of the header of a group the page opens in the middle of. */
  continued?: boolean;
  index: number;
  measure: (element: Element | null) => void;
  row: Row<Item>;
  grouping: string;
  columns: number;
  epics: readonly IssueSummary[];
  onRename: (key: string) => void;
  onMerge: (from: string, into: string) => void;
  onStartAll: (keys: string[]) => void;
}): ReactNode {
  const leaves = row.getLeafRows().filter((leaf) => !leaf.getIsGrouped());
  const value = String(row.groupingValue ?? "");
  const shown =
    grouping === "status"
      ? statusLabel(value as IssueStatus)
      : grouping === "priority"
        ? priorityName(value)
        : value;
  const epicKey = grouping === "epic" ? leaves[0]?.original.epicKey : undefined;
  const byStatus = issueStatuses
    .map(
      (status) =>
        [status, leaves.filter((leaf) => leaf.original.issue.status === status).length] as const
    )
    .filter(([, count]) => count > 0);
  const next = Math.min(...leaves.map((leaf) => leaf.original.queue ?? Number.POSITIVE_INFINITY));
  return (
    <tr className={surfaceMutedBg} data-index={index} ref={measure}>
      <td className="px-2 py-2" colSpan={columns}>
        <div className="flex flex-wrap items-center gap-x-4 gap-y-1">
          <button
            aria-expanded={row.getIsExpanded()}
            className={`flex min-w-0 items-center gap-2 text-left font-semibold ${textPrimaryOnCanvas}`}
            onClick={row.getToggleExpandedHandler()}
            type="button"
          >
            <span className={`w-4 ${textMutedOnCanvas}`}>{row.getIsExpanded() ? "▾" : "▸"}</span>
            {shown}
            {continued ? (
              <span className={`font-normal ${textMutedOnCanvas}`}>(continued)</span>
            ) : null}
            <span className={`font-normal ${textMutedOnCanvas}`}>
              {leaves.length} {leaves.length === 1 ? "issue" : "issues"}
              {Number.isFinite(next) ? ` · next in queue #${next}` : ""}
            </span>
          </button>
          <span className="flex items-center gap-2">
            <span
              aria-hidden
              className="flex h-2 w-32 overflow-hidden rounded-full bg-slate-200 dark:bg-slate-700"
            >
              {byStatus.map(([status, count]) => (
                <span className={statusTone[status]} key={status} style={{ flex: count }} />
              ))}
            </span>
            <span className={`text-xs ${textMutedOnCanvas}`}>
              {byStatus
                .map(([status, count]) => `${count} ${statusLabel(status).toLowerCase()}`)
                .join(" · ")}
            </span>
          </span>
          {epicKey === undefined ? null : (
            <EpicActions
              epicKey={epicKey}
              epics={epics}
              onMerge={(into) => onMerge(epicKey, into)}
              onRename={() => onRename(epicKey)}
              onStartAll={() =>
                onStartAll(
                  leaves
                    .filter((leaf) => leaf.original.queue)
                    .map((leaf) => leaf.original.issue.key)
                )
              }
            />
          )}
        </div>
      </td>
    </tr>
  );
}

/** A pop-over that closes on a click outside it. */
function Popover({
  label,
  labelClassName,
  title,
  children,
  align = "left",
}: {
  label: ReactNode;
  labelClassName?: string;
  title?: string;
  children: (close: () => void) => ReactNode;
  align?: "left" | "right";
}): ReactNode {
  const [open, setOpen] = useState(false);
  const box = useRef<HTMLSpanElement>(null);
  useEffect(() => {
    if (!open) return;
    const close = (event: MouseEvent) => {
      if (!box.current?.contains(event.target as Node)) setOpen(false);
    };
    document.addEventListener("mousedown", close);
    return () => document.removeEventListener("mousedown", close);
  }, [open]);
  return (
    <span className="relative inline-block" ref={box}>
      <button
        aria-expanded={open}
        aria-label={title}
        className={
          labelClassName ??
          `rounded-md border px-2 py-1 text-xs font-normal whitespace-nowrap ${borderDefault} ${surfaceBg} ${textSecondaryOnCanvas}`
        }
        onClick={() => setOpen(!open)}
        title={title}
        type="button"
      >
        {label}
      </button>
      {open ? (
        <span
          className={`absolute z-30 mt-1 flex max-h-80 w-64 flex-col overflow-y-auto rounded-lg border p-1 text-sm font-normal shadow-lg ${card} ${align === "right" ? "right-0" : "left-0"}`}
        >
          {children(() => setOpen(false))}
        </span>
      ) : null}
    </span>
  );
}

function ColumnsMenu({ columns }: { columns: Column<Item, unknown>[] }): ReactNode {
  return (
    <Popover align="right" label="Columns">
      {() =>
        columns
          .filter((column) => column.getCanHide())
          .map((column) => (
            <label
              className={`flex items-center gap-2 rounded px-2 py-1 ${surfaceMutedHoverBg}`}
              key={column.id}
            >
              <input
                checked={column.getIsVisible()}
                className={checkboxAccent}
                onChange={column.getToggleVisibilityHandler()}
                type="checkbox"
              />
              {String(column.columnDef.header)}
            </label>
          ))
      }
    </Popover>
  );
}

function EpicActions({
  epicKey,
  epics,
  onRename,
  onMerge,
  onStartAll,
}: {
  epicKey: string;
  epics: readonly IssueSummary[];
  onRename: () => void;
  onMerge: (into: string) => void;
  onStartAll: () => void;
}): ReactNode {
  return (
    <span className="ml-auto">
      <Popover align="right" label="Epic actions ▾">
        {(close) => (
          <>
            <button
              className={`rounded px-2 py-1 text-left ${surfaceMutedHoverBg}`}
              onClick={() => {
                close();
                onRename();
              }}
              type="button"
            >
              Rename
            </button>
            <button
              className={`rounded px-2 py-1 text-left ${surfaceMutedHoverBg}`}
              onClick={() => {
                close();
                onStartAll();
              }}
              type="button"
            >
              Start all its queued issues next
            </button>
            <label className={`px-2 py-1 text-xs ${textMutedOnCanvas}`}>
              Merge into another epic
              <select
                className={`mt-1 block w-full rounded-md border px-1 py-1 text-sm ${borderDefault} ${surfaceBg} ${textPrimaryOnCanvas}`}
                onChange={(event) => {
                  if (event.target.value) {
                    close();
                    onMerge(event.target.value);
                  }
                }}
                value=""
              >
                <option value="">Choose an epic…</option>
                {epics
                  .filter((epic) => epic.key !== epicKey)
                  .map((epic) => (
                    <option key={epic.key} value={epic.key}>
                      {epic.title}
                    </option>
                  ))}
              </select>
            </label>
            <span className={`px-2 py-1 text-xs ${textMutedOnCanvas}`}>
              To split it, tick some of its issues and choose “New epic from these”.
            </span>
          </>
        )}
      </Popover>
    </span>
  );
}

function Action({
  children,
  disabled,
  onClick,
}: {
  children: ReactNode;
  disabled?: boolean;
  onClick: () => void;
}): ReactNode {
  return (
    <button
      className={`rounded-lg border px-3 py-1 text-sm disabled:opacity-40 ${borderDefault} ${surfaceMutedHoverBg}`}
      disabled={disabled}
      onClick={onClick}
      type="button"
    >
      {children}
    </button>
  );
}
