/**
 * PROTOTYPE (dispatch://INFRA-1508): the project's issues as one table on TanStack Table.
 * Every column sorts and filters in its own header, the groupable ones group, one fuzzy search
 * covers every row, and the rows come in pages. A bar over the table counts the issues by status
 * and filters on a click. Legion's view of the work is three columns: the queue position (Legion
 * starts `todo` issues handed to it in board order), the Legion phase (from a daemon-state
 * snapshot until Dispatch reads the daemon itself, dispatch://INFRA-1077) and the epic.
 * Every write is an ordinary issue write, the same an agent or the command bar makes.
 */
import { rankItem, rankings } from "@tanstack/match-sorter-utils";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import {
  type Column,
  type ColumnDef,
  type ColumnFiltersState,
  type ExpandedState,
  type FilterFn,
  flexRender,
  type GroupingState,
  getCoreRowModel,
  getExpandedRowModel,
  getFacetedRowModel,
  getFacetedUniqueValues,
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
import { type ReactNode, useEffect, useMemo, useRef, useState } from "react";
import { Link } from "react-router-dom";

import { api } from "../../api/client";
import { projectIssuesQuery, whoAmIQuery } from "../../api/queries";
import type { IssueSummary, UpdateIssueInput } from "../../api/types";
import { AttentionBadge } from "../../components/Badge";
import { LoadingSkeleton } from "../../components/LoadingSkeleton";
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
import { referenceTriggerProps } from "../refs/RefPreview";
import { buildIssuePath } from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";
import { type IssueStatus, issueStatuses, statusLabel } from "./board-model";
import { projectIssuesQueryKey } from "./issue-filters";

declare module "@tanstack/react-table" {
  interface ColumnMeta<TData extends RowData, TValue> {
    /** How the header filters this column: a pick list of its values, or typed text. */
    filter?: "facet" | "text";
    /** A facet value as the reader sees it. */
    show?: (value: string) => string;
  }
}

/** One table row: an issue and what the table derives for it. */
interface Item {
  readonly issue: IssueSummary;
  readonly queue?: number;
  readonly epic: string;
  readonly epicKey?: string;
  readonly phase: string;
  readonly search: string;
}

const NO_EPIC = "No epic";
const NO_PHASE = "Not in Legion";
const RUNNING: ReadonlySet<string> = new Set(["in_progress", "testing", "needs_review", "retro"]);
const isQueued = (issue: IssueSummary) =>
  issue.status === "todo" && (issue.labels ?? []).includes("legion");
const isEpic = (issue: IssueSummary) =>
  issue.parent === null && (issue.labels ?? []).includes("epic") && issue.status !== "done";
const priorityName = (value: string) => (value === "" ? "No priority" : `P${value}`);

/** A row passes a pick-list filter when nothing is picked or its value is one of the picks; a
 *  list-valued cell (labels) passes when any of its values is. */
const inPicks: FilterFn<Item> = (row, id, picks: string[]) => {
  if (picks.length === 0) return true;
  const value = row.getValue<unknown>(id);
  return Array.isArray(value)
    ? value.some((entry) => picks.includes(String(entry)))
    : picks.includes(String(value ?? ""));
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
const fuzzy: FilterFn<Item> = (row, _id, query: string) => {
  const haystack = row.original.search;
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
};

const statusTone: Record<string, string> = {
  triage: "bg-slate-300 dark:bg-slate-600",
  icebox: "bg-slate-300 dark:bg-slate-600",
  backlog: "bg-slate-400 dark:bg-slate-500",
  todo: "bg-sky-200 dark:bg-sky-800",
  in_progress: "bg-sky-500",
  testing: "bg-sky-600",
  needs_review: "bg-sky-700 dark:bg-sky-400",
  retro: "bg-sky-800 dark:bg-sky-300",
  done: "bg-emerald-600 dark:bg-emerald-500",
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
  visibility: { claim: false, asks: false },
  pageSize: 50,
};
function loadSaved(project: string): Saved {
  try {
    return { ...defaults, ...JSON.parse(localStorage.getItem(`issues-table.${project}`) ?? "{}") };
  } catch {
    return defaults;
  }
}

export function IssuesTable({ project }: { project: string }): ReactNode {
  const queryClient = useQueryClient();
  const login = useQuery(whoAmIQuery()).data?.login;
  const issues = useQuery(projectIssuesQuery(project, []));
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
  const saved = useMemo(() => loadSaved(project), [project]);
  const [sorting, setSorting] = useState<SortingState>(saved.sorting);
  const [grouping, setGrouping] = useState<GroupingState>(saved.grouping);
  const [filters, setFilters] = useState<ColumnFiltersState>(saved.filters);
  const [visibility, setVisibility] = useState<VisibilityState>(saved.visibility);
  const [pagination, setPagination] = useState<PaginationState>({
    pageIndex: 0,
    pageSize: saved.pageSize,
  });
  const [search, setSearch] = useState("");
  const [expanded, setExpanded] = useState<ExpandedState>(true);
  const [selection, setSelection] = useState<RowSelectionState>({});
  const [moved, setMoved] = useState<ReadonlySet<string>>(new Set());
  const [notice, setNotice] = useState<string>();
  const [busy, setBusy] = useState(false);
  useEffect(() => {
    localStorage.setItem(
      `issues-table.${project}`,
      JSON.stringify({ sorting, grouping, filters, visibility, pageSize: pagination.pageSize })
    );
  }, [project, sorting, grouping, filters, visibility, pagination.pageSize]);

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
        return {
          issue,
          queue: position.get(issue.key),
          epic: epicTitle,
          epicKey: epic?.key,
          phase: phases[issue.key] ?? NO_PHASE,
          search: [
            issue.key,
            issue.title,
            epicTitle,
            issue.assignee ?? "",
            (issue.labels ?? []).join(" "),
          ].join(" "),
        };
      });
  }, [all, legion.data]);

  const columns = useMemo<ColumnDef<Item>[]>(
    () => [
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
        id: "queue",
        header: "Queue",
        accessorFn: (item) => item.queue,
        sortUndefined: "last",
        sortDescFirst: false,
        aggregationFn: "min",
        filterFn: inPicks,
        meta: { filter: "facet", show: (value) => (value === "" ? "Not queued" : `#${value}`) },
        enableGrouping: false,
        cell: ({ getValue }) => {
          const place = getValue<number | undefined>();
          return place === undefined ? null : (
            <span className="font-semibold tabular-nums" title="Legion starts issues in this order">
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
          Number(a.original.issue.key.split("-")[1]) - Number(b.original.issue.key.split("-")[1]),
        filterFn: "includesString",
        meta: { filter: "text" },
        enableGrouping: false,
        cell: ({ row }) => (
          <span className="font-semibold whitespace-nowrap">{row.original.issue.key}</span>
        ),
      },
      {
        id: "title",
        header: "Title",
        accessorFn: (item) => item.issue.title,
        filterFn: "includesString",
        meta: { filter: "text" },
        enableGrouping: false,
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
        meta: { filter: "facet", show: (value) => statusLabel(value as IssueStatus) },
        cell: ({ getValue }) => <StatusPill>{statusLabel(getValue<IssueStatus>())}</StatusPill>,
      },
      {
        id: "phase",
        header: "Legion phase",
        accessorFn: (item) => item.phase,
        filterFn: inPicks,
        meta: { filter: "facet" },
        cell: ({ getValue }) => {
          const phase = getValue<string>();
          return phase === NO_PHASE ? null : (
            <span className={`whitespace-nowrap ${textSecondaryOnCanvas}`}>{phase}</span>
          );
        },
      },
      {
        id: "priority",
        header: "Priority",
        accessorFn: (item) => item.issue.priority ?? undefined,
        sortUndefined: "last",
        sortDescFirst: false,
        filterFn: inPicks,
        meta: { filter: "facet", show: priorityName },
        cell: ({ row }) => (
          <PriorityControl
            disabled={row.original.issue.status === "done"}
            issueKey={row.original.issue.key}
            priority={row.original.issue.priority}
          />
        ),
      },
      {
        id: "epic",
        header: "Epic",
        accessorFn: (item) => item.epic,
        filterFn: inPicks,
        meta: { filter: "facet" },
        cell: ({ getValue }) => (
          <span className={`line-clamp-2 ${textSecondaryOnCanvas}`}>{getValue<string>()}</span>
        ),
      },
      {
        id: "assignee",
        header: "Assignee",
        accessorFn: (item) => item.issue.assignee ?? "Unassigned",
        filterFn: inPicks,
        meta: { filter: "facet" },
        cell: ({ getValue }) => (
          <span className={`whitespace-nowrap ${textSecondaryOnCanvas}`}>{getValue<string>()}</span>
        ),
      },
      {
        id: "labels",
        header: "Labels",
        accessorFn: (item) => item.issue.labels ?? [],
        sortingFn: (a, b) =>
          (a.original.issue.labels ?? [])
            .join(",")
            .localeCompare((b.original.issue.labels ?? []).join(",")),
        filterFn: inPicks,
        meta: { filter: "facet" },
        enableGrouping: false,
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
        header: "Working on it",
        accessorFn: (item) =>
          item.issue.claim === null ? "Nobody" : JSON.stringify(item.issue.claim).slice(0, 80),
        filterFn: "includesString",
        meta: { filter: "text" },
        enableGrouping: false,
        cell: ({ row }) => <ClaimChip claim={row.original.issue.claim} />,
      },
      {
        id: "asks",
        header: "Open asks",
        accessorFn: (item) => item.issue.open_asks,
        enableColumnFilter: false,
        enableGrouping: false,
        cell: ({ getValue }) =>
          getValue<number>() === 0 ? null : <AttentionBadge count={getValue<number>()} />,
      },
      {
        id: "updated",
        header: "Updated",
        accessorFn: (item) => item.issue.updated_at,
        enableColumnFilter: false,
        enableGrouping: false,
        cell: ({ getValue }) => (
          <span className={`whitespace-nowrap text-xs ${textMutedOnCanvas}`}>
            <Timestamp at={getValue<string>()} />
          </span>
        ),
      },
    ],
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
    },
    getRowId: (item) => item.issue.key,
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
    getFacetedUniqueValues: getFacetedUniqueValues(),
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

  if (issues.isPending) return <LoadingSkeleton label="Loading issues" />;
  if (issues.isError) return <p className={dangerText}>Could not load issues.</p>;

  const statusColumn = table.getColumn("status");
  const statusPicks = (statusColumn?.getFilterValue() as string[] | undefined) ?? [];
  const statusCounts = statusColumn?.getFacetedUniqueValues() ?? new Map<string, number>();
  const statusTotal = issueStatuses.reduce(
    (sum, status) => sum + (statusCounts.get(status) ?? 0),
    0
  );
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
    <section aria-label="Project issues" className="space-y-3 pb-20">
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
                className={`min-w-0 overflow-hidden px-1 text-left text-[11px] font-medium whitespace-nowrap text-white ${statusTone[status]} ${on ? "" : "opacity-30"}`}
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
        </div>
      </div>

      {/* The table's own controls: always shown, never behind a button. */}
      <div className="flex flex-wrap items-center gap-2">
        <input
          aria-label="Search every issue"
          className={`min-w-64 flex-1 rounded-lg border px-3 py-1.5 text-sm ${borderDefault} ${surfaceBg} ${textPrimaryOnCanvas}`}
          onChange={(event) => {
            setSearch(event.target.value);
            setPagination((page) => ({ ...page, pageIndex: 0 }));
          }}
          placeholder="Search issues: key, title, epic, assignee, labels"
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

      {selectedKeys.length === 0 ? null : (
        <div
          className={`sticky top-0 z-20 flex flex-wrap items-center gap-2 rounded-xl border px-3 py-2 text-sm shadow ${card}`}
          role="toolbar"
        >
          <span className="mr-1 font-semibold">{selectedKeys.length} selected</span>
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

      <div className={`overflow-x-auto rounded-xl border ${borderDefault}`}>
        <table className="w-full border-collapse text-sm">
          <thead className={`sticky top-0 z-10 ${surfaceBg}`}>
            {table.getHeaderGroups().map((group) => (
              <tr className={`border-b ${borderDefault}`} key={group.id}>
                {group.headers.map((header) => (
                  <th
                    className={`px-2 pt-2 pb-1 text-left align-top text-xs font-semibold ${textSecondaryOnCanvas} ${header.column.id === "title" ? "min-w-80" : ""}`}
                    key={header.id}
                  >
                    {header.column.getCanSort() ? (
                      <button
                        className="flex items-center gap-1 whitespace-nowrap"
                        onClick={header.column.getToggleSortingHandler()}
                        title="Sort (shift-click to add a second sort)"
                        type="button"
                      >
                        {flexRender(header.column.columnDef.header, header.getContext())}
                        <span className={textMutedOnCanvas}>
                          {{ asc: "▲", desc: "▼" }[header.column.getIsSorted() as string] ?? "↕"}
                        </span>
                      </button>
                    ) : (
                      flexRender(header.column.columnDef.header, header.getContext())
                    )}
                    {header.column.getCanFilter() ? <HeaderFilter column={header.column} /> : null}
                  </th>
                ))}
                <th className="w-24" />
              </tr>
            ))}
          </thead>
          <tbody>
            {table.getRowModel().rows.map((row) =>
              row.getIsGrouped() ? (
                <GroupRow
                  columns={visibleColumns}
                  epics={epics}
                  grouping={grouping[0] ?? ""}
                  key={row.id}
                  onMerge={mergeEpic}
                  onRename={renameEpic}
                  onStartAll={(keys) => startNext(keys)}
                  row={row}
                />
              ) : (
                <tr
                  className={`group border-b align-top ${borderDefault} ${moved.has(row.original.issue.key) ? selectedCardBg : surfaceMutedHoverBg}`}
                  key={row.id}
                >
                  {row.getVisibleCells().map((cell) => (
                    <td
                      className={`px-2 py-2 ${row.depth > 0 && cell.column.id === "select" ? "pl-6" : ""}`}
                      key={cell.id}
                    >
                      {flexRender(cell.column.columnDef.cell, cell.getContext())}
                    </td>
                  ))}
                  <td className="px-2 py-2 text-right">
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
              )
            )}
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
          Rows {rowsOnPages === 0 ? 0 : pageIndex * pageSize + 1}–
          {Math.min((pageIndex + 1) * pageSize, rowsOnPages)} of {rowsOnPages}
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
          {[25, 50, 100, 250].map((size) => (
            <option key={size} value={size}>
              {size} per page
            </option>
          ))}
        </select>
      </div>
    </section>
  );
}

/** A group's header row: its value, how many issues, and for an epic its issues by status and
 *  its actions. */
function GroupRow({
  row,
  grouping,
  columns,
  epics,
  onRename,
  onMerge,
  onStartAll,
}: {
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
        ? priorityName(value === "undefined" ? "" : value)
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
    <tr className={`border-b ${borderDefault} ${surfaceMutedBg}`}>
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
  children,
  align = "left",
}: {
  label: ReactNode;
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
        className={`rounded-md border px-2 py-1 text-xs font-normal whitespace-nowrap ${borderDefault} ${surfaceBg} ${textSecondaryOnCanvas}`}
        onClick={() => setOpen(!open)}
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

/** The filter under a column's name: typed text, or a pick list of the column's values with how
 *  many rows each would show. */
function HeaderFilter({ column }: { column: Column<Item, unknown> }): ReactNode {
  const meta = column.columnDef.meta;
  if (meta?.filter === "text") {
    return (
      <input
        aria-label={`Filter ${String(column.columnDef.header)}`}
        className={`mt-1 block w-full min-w-16 rounded-md border px-1.5 py-0.5 text-xs font-normal ${borderDefault} ${surfaceBg}`}
        onChange={(event) => column.setFilterValue(event.target.value || undefined)}
        placeholder="Filter"
        value={(column.getFilterValue() as string | undefined) ?? ""}
      />
    );
  }
  const picks = (column.getFilterValue() as string[] | undefined) ?? [];
  const counts = new Map<string, number>();
  for (const row of column.getFacetedRowModel().flatRows) {
    if (row.getIsGrouped()) continue;
    const value = row.getValue<unknown>(column.id);
    for (const entry of Array.isArray(value) ? value : [value]) {
      const key = String(entry ?? "");
      counts.set(key, (counts.get(key) ?? 0) + 1);
    }
  }
  const values = [...counts.entries()].sort((a, b) =>
    column.id === "status"
      ? issueStatuses.indexOf(a[0] as IssueStatus) - issueStatuses.indexOf(b[0] as IssueStatus)
      : column.id === "queue"
        ? (Number(a[0]) || 1e9) - (Number(b[0]) || 1e9)
        : b[1] - a[1]
  );
  const show = meta?.show ?? ((value: string) => value);
  const toggle = (value: string) => {
    const next = picks.includes(value) ? picks.filter((pick) => pick !== value) : [...picks, value];
    column.setFilterValue(next.length === 0 ? undefined : next);
  };
  return (
    <span className="mt-1 block">
      <Popover
        label={
          picks.length === 0
            ? "All"
            : picks.length === 1
              ? show(picks[0] ?? "")
              : column.id === "status" &&
                  picks.length === issueStatuses.length - 1 &&
                  !picks.includes("done")
                ? "Not done"
                : `${picks.length} picked`
        }
      >
        {() => (
          <>
            {column.id === "queue" ? (
              <button
                className={`rounded px-2 py-1 text-left ${surfaceMutedHoverBg}`}
                onClick={() =>
                  column.setFilterValue(
                    values.map(([value]) => value).filter((value) => value !== "")
                  )
                }
                type="button"
              >
                Only queued issues
              </button>
            ) : null}
            <button
              className={`rounded px-2 py-1 text-left ${surfaceMutedHoverBg} ${textMutedOnCanvas}`}
              onClick={() => column.setFilterValue(undefined)}
              type="button"
            >
              Show all
            </button>
            {values.map(([value, count]) => (
              <label
                className={`flex items-center gap-2 rounded px-2 py-1 ${surfaceMutedHoverBg}`}
                key={value}
              >
                <input
                  checked={picks.includes(value)}
                  className={checkboxAccent}
                  onChange={() => toggle(value)}
                  type="checkbox"
                />
                <span className="min-w-0 flex-1 truncate">{show(value)}</span>
                <span className={`tabular-nums ${textMutedOnCanvas}`}>{count}</span>
              </label>
            ))}
          </>
        )}
      </Popover>
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
