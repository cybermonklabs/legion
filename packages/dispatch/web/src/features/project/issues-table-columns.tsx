import type { ColumnDef, FilterFn, RowData } from "@tanstack/react-table";
import { type ReactNode, useMemo, useState } from "react";
import { Link } from "react-router-dom";
import type { UserState } from "../../api/types";
import { AttentionBadge } from "../../components/Badge";
import { InfoHint } from "../../components/InfoHint";
import { LabelPill, StatusPill } from "../../components/Pill";
import { compareTimestamps } from "../../lib/timestamps";
import {
  borderDefault,
  checkboxAccent,
  linkHoverText,
  linkText,
  textMutedOnCanvas,
  textPrimaryOnCanvas,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { ClaimChip } from "../issue/ClaimChip";
import { issuePriority } from "../issue/issue-priority";
import { PriorityControl } from "../issue/PriorityControl";
import { ProgressChips } from "../issue/ProgressChips";
import { UnreachableRouteMarker } from "../issue/RouteReach";
import { AgentName } from "../refs/AgentName";
import { referenceTriggerProps } from "../refs/RefPreview";
import { buildIssuePath } from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";
import { type IssueStatus, issueStatuses, statusLabel } from "./board-model";
import {
  COLUMN_MIN,
  COLUMNS,
  COMPACT_COLUMN_MIN,
  type ColumnId,
  formatPhaseTime,
  type Item,
  legionPhaseLabel,
  legionSlotText,
  legionSlotValue,
  matchesFilters,
  NO_PHASE,
  stageOf,
} from "./issues-table-model";
import { PhaseHoursBar, phaseHoursTotal } from "./PhaseHoursBar";
import { issueIsUnread, UnreadDot } from "./UnreadDot";

declare module "@tanstack/react-table" {
  interface ColumnMeta<TData extends RowData, TValue> {
    /** A facet value as the reader sees it. */
    show?: (value: string) => string;
    /** The facet values in the order the filter lists them; by count when absent. */
    order?: readonly string[];
    /** The values the filter offers whatever the loaded rows hold. */
    always?: readonly string[];
    /** Filter by typed text instead of picking values: for a column of free text. */
    text?: boolean;
    /** The header holds a control of its own — a hint, a switch — so it is drawn beside the
     *  sort button rather than inside it: a press on that control sorts nothing. */
    control?: boolean;
    /** The column's own name in words, for a header that draws more than its name: the column
     *  picker and the Group by list read it rather than the header node. */
    label?: string;
    /** Whether closed rows are loaded, for a Status facet that otherwise says it can load them. */
    doneLoaded?: boolean;
    /** The types for the declaration's own parameters, which it does not otherwise use. */
    value?: [TData, TValue];
  }
}
const matchesColumn: FilterFn<Item> = (row, id, value) =>
  matchesFilters(row.original, [{ id, value }]);

/** A header that says what the column is, with what it means behind an (i). */
function HeaderWithHint({ children, hint }: { children: string; hint: string }): ReactNode {
  return (
    <span className="flex items-center gap-1">
      {children}
      <InfoHint label={`About ${children}`}>{hint}</InfoHint>
    </span>
  );
}

/** Each column's name in words: the column picker, the Group by list and a header's hint read
 *  it, so a column whose header draws a control still has one name. */
export const COLUMN_LABELS: Record<ColumnId, string> = {
  asks: "Open asks",
  assignee: "Assignee",
  author: "Created by",
  ci: "CI",
  claim: "Claimed by",
  closed: "Closed",
  created: "Created",
  epic: "Epic",
  key: "Key",
  labels: "Labels",
  legion: "Tree",
  phase: "Phase",
  phase_hours: "Time per phase",
  priority: "Priority",
  queue: "Place",
  review_rounds: "Rounds",
  roles: "Roles",
  select: "Select",
  slot: "Slot",
  slot_use: "Slot use",
  stage: "Stage",
  status: "Status",
  time_in_phase: "In phase",
  title: "Title",
  updated: "Updated",
  waited: "Waited",
  waiting_reason: "Waiting",
  work_hours: "Work time",
};
export const columnLabel = <Row,>(column: { id: string; columnDef: ColumnDef<Row> }): string =>
  column.columnDef.meta?.label ?? String(column.columnDef.header ?? column.id);

/** How each column renders and sorts, for the reader `userState` describes. Keyed by the
 *  registry's own ids, so a typo or a column with no rendering fails to compile. Everything
 *  else about a column — whether it filters, what a row files under, what it groups by, how
 *  wide it starts and how narrow it may go — comes from `COLUMNS` in the model, so the two
 *  never disagree. */
const renderings = (
  userState: UserState | undefined,
  phaseMode: "hours" | "share",
  setPhaseMode: (mode: "hours" | "share") => void,
  phaseHoursMax: number | undefined
): Record<ColumnId, Partial<ColumnDef<Item>>> => ({
  select: {
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
    enableHiding: false,
  },
  priority: {
    header: COLUMN_LABELS.priority,
    accessorFn: (item) => item.issue.priority ?? undefined,
    sortUndefined: "last",
    sortDescFirst: false,
    cell: ({ row }) => (
      <PriorityControl
        disabled={row.original.issue.status === "done"}
        issueKey={row.original.issue.key}
        priority={issuePriority(row.original.issue.priority)}
      />
    ),
  },
  queue: {
    header: COLUMN_LABELS.queue,
    accessorFn: (item) => item.queue,
    sortUndefined: "last",
    sortDescFirst: false,
    cell: ({ getValue }) => {
      const place = getValue<number | undefined>();
      return place === undefined ? null : (
        <span className="font-semibold tabular-nums" title="Legion starts issues in this order">
          #{place}
        </span>
      );
    },
  },
  key: {
    header: COLUMN_LABELS.key,
    accessorFn: (item) => item.issue.key,
    sortingFn: (a, b) =>
      Number(a.original.issue.key.split("-")[1]) - Number(b.original.issue.key.split("-")[1]),
    cell: ({ row }) => (
      <span className="flex items-center gap-1 font-semibold whitespace-nowrap">
        <Link
          className={`${linkText} ${linkHoverText}`}
          to={buildIssuePath({ key: row.original.issue.key, kind: "issue" })}
          {...referenceTriggerProps({ key: row.original.issue.key, kind: "issue" })}
        >
          {row.original.issue.key}
        </Link>
        {issueIsUnread(
          row.original.issue,
          userState?.[row.original.issue.key]?.last_read_seq ?? 0
        ) ? (
          <UnreadDot />
        ) : null}
      </span>
    ),
  },
  title: {
    header: COLUMN_LABELS.title,
    accessorFn: (item) => item.issue.title,
    cell: ({ row }) => (
      <div className="min-w-0 space-y-1">
        <Link
          aria-label={`${row.original.issue.key} ${row.original.issue.title}`}
          data-issue-link
          className={`flex w-full min-w-0 ${linkText} ${linkHoverText}`}
          to={buildIssuePath({ key: row.original.issue.key, kind: "issue" })}
          {...referenceTriggerProps({ key: row.original.issue.key, kind: "issue" })}
        >
          <span className={`line-clamp-2 min-w-0 flex-1 break-words ${textPrimaryOnCanvas}`}>
            {row.original.issue.title}
          </span>
        </Link>
        <span className="flex flex-wrap items-center gap-1">
          {row.original.issue.open_asks > 0 ? (
            <AttentionBadge count={row.original.issue.open_asks} />
          ) : null}
          <ProgressChips progress={row.original.issue.progress} />
          <UnreachableRouteMarker issue={row.original.issue} />
          {row.original.issue.parent ? (
            <Link
              className={`text-xs ${linkText} ${linkHoverText}`}
              to={buildIssuePath({ key: row.original.issue.parent, kind: "issue" })}
              {...referenceTriggerProps({ key: row.original.issue.parent, kind: "issue" })}
            >
              {row.original.issue.parent}
            </Link>
          ) : null}
        </span>
      </div>
    ),
  },
  status: {
    header: COLUMN_LABELS.status,
    accessorFn: (item) => item.issue.status,
    sortingFn: (a, b) =>
      issueStatuses.indexOf(a.original.issue.status as IssueStatus) -
      issueStatuses.indexOf(b.original.issue.status as IssueStatus),
    cell: ({ getValue }) => <StatusPill>{statusLabel(getValue<IssueStatus>())}</StatusPill>,
  },
  phase: {
    header: () => (
      <HeaderWithHint hint="The board column the daemon holds the issue in, and how long it has stood there.">
        Phase
      </HeaderWithHint>
    ),
    accessorFn: (item) => item.legion?.id ?? NO_PHASE,
    sortingFn: (a, b) =>
      (a.original.legion?.order ?? Number.MAX_SAFE_INTEGER) -
        (b.original.legion?.order ?? Number.MAX_SAFE_INTEGER) ||
      (a.original.queue ?? Number.MAX_SAFE_INTEGER) - (b.original.queue ?? Number.MAX_SAFE_INTEGER),
    sortDescFirst: false,
    meta: { control: true },
    cell: ({ row }) =>
      row.original.legion ? (
        <LegionPhaseCell phase={row.original.legion} />
      ) : (
        <span className={textMutedOnCanvas}>{NO_PHASE}</span>
      ),
  },
  slot: {
    header: () => (
      <HeaderWithHint hint="The worker slot the issue holds. A child holds its tree root's slot.">
        Slot
      </HeaderWithHint>
    ),
    // A child holds its root's slot, so it sorts and reads by that slot too.
    accessorFn: (item) =>
      item.legion ? (item.legion.card.slot ?? item.legion.card.tree_slot ?? undefined) : undefined,
    sortUndefined: "last",
    sortDescFirst: false,
    meta: { control: true },
    cell: ({ row }) => (
      <span
        className="tabular-nums"
        data-legion-fact="slot"
        // A child works in its tree root's slot, which the number alone does not say.
        title={row.original.legion ? legionSlotText(row.original.legion.card) : undefined}
      >
        {legionSlotValue(row.original)}
      </span>
    ),
  },
  roles: {
    header: () => (
      <HeaderWithHint hint="The roles that are in a turn on this issue now. None reads as between turns.">
        Roles
      </HeaderWithHint>
    ),
    accessorFn: (item) =>
      item.legion === undefined
        ? NO_PHASE
        : item.legion.card.roles.length === 0
          ? "between turns"
          : item.legion.card.roles.join(", "),
    meta: { control: true },
    cell: ({ getValue }) => <span data-legion-fact="roles">{getValue<string>()}</span>,
  },
  ci: {
    header: () => (
      <HeaderWithHint hint="What the issue's pull request checks said when the daemon last read them.">
        CI
      </HeaderWithHint>
    ),
    accessorFn: (item) => item.legion?.card.checks_verdict ?? "No verdict",
    meta: { control: true },
    cell: ({ getValue }) => <span data-legion-fact="checks">{getValue<string>()}</span>,
  },
  phase_hours: {
    header: () => (
      <span className="flex items-center gap-1">
        Time per phase
        <InfoHint label="About time per phase">
          The hours Legion spent on the issue, by phase. Hours draws every bar against the longest
          on the page. Share draws every bar full length, each block its own share.
        </InfoHint>
        <button
          aria-label={`Show ${phaseMode === "hours" ? "shares" : "hours"}`}
          className={`rounded border px-1 text-[0.625rem] ${borderDefault}`}
          onClick={(event) => {
            event.stopPropagation();
            setPhaseMode(phaseMode === "hours" ? "share" : "hours");
          }}
          type="button"
        >
          {phaseMode === "hours" ? "Hours" : "Share"}
        </button>
      </span>
    ),
    accessorFn: (item) => (item.phaseHours ? phaseHoursTotal(item.phaseHours) : undefined),
    sortUndefined: "last",
    sortDescFirst: true,
    enableColumnFilter: false,
    meta: { control: true },
    cell: ({ row }) =>
      row.original.phaseHours === undefined ? null : (
        <PhaseHoursBar hours={row.original.phaseHours} max={phaseHoursMax} mode={phaseMode} />
      ),
  },
  closed: {
    header: COLUMN_LABELS.closed,
    accessorFn: (item) => item.closedAt ?? undefined,
  },
  work_hours: {
    header: COLUMN_LABELS.work_hours,
    accessorFn: (item) => item.facts?.work_hours,
  },
  review_rounds: {
    header: COLUMN_LABELS.review_rounds,
    accessorFn: (item) => item.facts?.review_rounds,
  },
  slot_use: {
    header: COLUMN_LABELS.slot_use,
    accessorFn: (item) => item.legion?.card.slot_use ?? undefined,
  },
  waited: {
    header: COLUMN_LABELS.waited,
    accessorFn: (item) => {
      const since = item.legion?.card.waiting?.since;
      return since === null || since === undefined ? undefined : Date.now() - Date.parse(since);
    },
  },
  waiting_reason: {
    header: COLUMN_LABELS.waiting_reason,
    accessorFn: (item) => item.legion?.card.waiting?.reason ?? undefined,
  },
  time_in_phase: {
    header: COLUMN_LABELS.time_in_phase,
    accessorFn: (item) => {
      const since = item.legion?.card.phase_since;
      return since === null || since === undefined ? undefined : Date.now() - Date.parse(since);
    },
  },
  legion: {
    header: COLUMN_LABELS.legion,
    accessorFn: (item) => item.facts?.tree,
  },
  stage: {
    header: COLUMN_LABELS.stage,
    accessorFn: (item) => stageOf(item),
  },
  epic: {
    header: COLUMN_LABELS.epic,
    accessorFn: (item) => item.epic,
    cell: ({ getValue }) => (
      <span className={`line-clamp-2 ${textSecondaryOnCanvas}`}>{getValue<string>()}</span>
    ),
  },
  assignee: {
    header: COLUMN_LABELS.assignee,
    accessorFn: (item) => item.issue.assignee ?? "Unassigned",
    cell: ({ getValue }) => (
      <span className={`whitespace-nowrap ${textSecondaryOnCanvas}`}>{getValue<string>()}</span>
    ),
  },
  author: {
    header: COLUMN_LABELS.author,
    accessorFn: (item) => item.author,
    cell: ({ row }) =>
      row.original.issue.created_by === null ? (
        <span className={textSecondaryOnCanvas}>Unknown</span>
      ) : (
        <AgentName actor={row.original.issue.created_by} />
      ),
  },
  labels: {
    header: COLUMN_LABELS.labels,
    accessorFn: (item) => (item.issue.labels ?? []).join(", "),
    cell: ({ row }) => (
      <span className="flex flex-wrap gap-1">
        {(row.original.issue.labels ?? []).map((label) => (
          <LabelPill key={label}>{label}</LabelPill>
        ))}
      </span>
    ),
  },
  claim: {
    header: COLUMN_LABELS.claim,
    accessorFn: (item) => item.claimedBy,
    cell: ({ row }) => <ClaimChip claim={row.original.issue.claim} />,
  },
  asks: {
    header: COLUMN_LABELS.asks,
    accessorFn: (item) => item.issue.open_asks,
    sortDescFirst: true,
    cell: ({ getValue }) =>
      getValue<number>() === 0 ? null : <AttentionBadge count={getValue<number>()} />,
  },
  created: {
    header: COLUMN_LABELS.created,
    accessorFn: (item) => item.issue.created_at,
    sortingFn: (a, b) =>
      compareTimestamps(a.original.issue.created_at, b.original.issue.created_at),
    cell: ({ getValue }) => (
      <Timestamp
        at={getValue<string>()}
        className={`whitespace-nowrap text-xs ${textMutedOnCanvas}`}
      />
    ),
  },
  updated: {
    header: COLUMN_LABELS.updated,
    accessorFn: (item) => item.issue.updated_at,
    sortingFn: (a, b) =>
      compareTimestamps(a.original.issue.updated_at, b.original.issue.updated_at),
    cell: ({ getValue }) => (
      <Timestamp
        at={getValue<string>()}
        className={`whitespace-nowrap text-xs ${textMutedOnCanvas}`}
      />
    ),
  },
});

/** The columns only a project a Legion daemon serves has. */
export const LEGION_COLUMNS: readonly ColumnId[] = [
  "phase",
  "slot",
  "roles",
  "ci",
  "work_hours",
  "review_rounds",
  "slot_use",
  "waited",
  "waiting_reason",
  "time_in_phase",
  "legion",
  "phase_hours",
];

export function useIssueTableColumns(
  userState: UserState | undefined,
  compact: boolean,
  hasLegion = false,
  /** The largest total of the rows on the page, so every bar of the Time per phase column is
   *  drawn against one scale. */
  phaseHoursMax?: number
): ColumnDef<Item>[] {
  const [phaseMode, setPhaseMode] = useState<"hours" | "share">("hours");
  return useMemo<ColumnDef<Item>[]>(() => {
    const rendering = renderings(userState, phaseMode, setPhaseMode, phaseHoursMax);
    const shown = hasLegion ? COLUMNS : COLUMNS.filter((c) => !LEGION_COLUMNS.includes(c.id));
    return shown.map((column) => {
      const picks = column.filter?.kind === "picks" ? column.filter : undefined;
      return {
        ...rendering[column.id],
        id: column.id,
        size: column.size,
        minSize: (compact ? COMPACT_COLUMN_MIN[column.id] : undefined) ?? COLUMN_MIN[column.id],
        enableResizing: column.id !== "select",
        enableSorting: column.sortable !== false,
        enableColumnFilter: column.filter !== undefined,
        enableGrouping: picks?.group !== undefined,
        ...(picks?.group === undefined ? {} : { getGroupingValue: picks.group }),
        ...(column.filter === undefined
          ? {}
          : { filterFn: column.filter.kind === "text" ? "includesString" : matchesColumn }),
        meta: {
          label: COLUMN_LABELS[column.id],
          ...rendering[column.id]?.meta,
          ...(column.filter?.kind === "text" ? { text: true } : {}),
          ...(picks?.order === undefined ? {} : { order: picks.order }),
          ...(picks?.show === undefined ? {} : { show: picks.show }),
          ...(picks?.always === undefined ? {} : { always: picks.always }),
        },
      };
    });
  }, [userState, compact, hasLegion, phaseMode, phaseHoursMax]);
}

/** A Legion phase as the issues table writes it: the board column's label, the time in phase,
 *  what blocks it and what it waits on. The table's Phase column, the project board's cards and
 *  the Legion tab all draw a phase through this one cell. */
export function LegionPhaseCell({ phase }: { phase: NonNullable<Item["legion"]> }): ReactNode {
  return (
    <span className="flex flex-col gap-1" data-legion-phase={phase.id}>
      <span>{legionPhaseLabel(phase.label, phase.card.phase)}</span>
      <span
        className={`text-xs ${textMutedOnCanvas}`}
        data-legion-fact="phase-time"
        title={phase.card.phase_since ?? "Phase entry time unknown"}
      >
        {phase.card.phase_since === null
          ? "—"
          : `${formatPhaseTime(phase.card.phase_since, Date.now())} in phase`}
      </span>
      {phase.card.blocked_by.length > 0 ? (
        <span className="text-xs" data-legion-fact="blocked-by">
          Blocked by {phase.card.blocked_by.join(", ")}
        </span>
      ) : null}
      {phase.card.waiting_on ? (
        <span className="text-xs" data-legion-fact="waiting-on">
          Waiting on {phase.card.waiting_on.kind}
          {phase.card.waiting_on.answered_at ? "; answered" : ""}
        </span>
      ) : null}
    </span>
  );
}
