import type {
  ColumnDef,
  FilterFn,
  FilterFnOption,
  RowData,
  RowModel,
  SortingFn,
  SortingState,
  Table,
} from "@tanstack/react-table";
import { type ReactNode, useMemo, useRef } from "react";
import { Link } from "react-router-dom";
import type { IssueSummary, UserState } from "../../api/types";
import { AttentionBadge } from "../../components/Badge";
import { LabelPill, StatusPill } from "../../components/Pill";
import { rangeWords } from "../../components/RangeFilter";
import { Tooltip, useTooltip } from "../../components/Tooltip";
import type { LayoutColumns } from "../../components/table-layout";
import { type ClosedWindow, inWindow, readWindow } from "../../lib/closed-window";
import { issueProject } from "../../lib/issue-key";
import { compareTimestamps } from "../../lib/timestamps";
import {
  checkboxAccent,
  dangerText,
  focusVisibleRing,
  linkHoverText,
  linkText,
  skeletonBg,
  textMutedOnCanvas,
  textPrimaryOnCanvas,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { ClaimChip } from "../issue/ClaimChip";
import { issuePriority } from "../issue/issue-priority";
import { PriorityControl } from "../issue/PriorityControl";
import { ProgressChips } from "../issue/ProgressChips";
import { UnreachableRouteMarker } from "../issue/RouteReach";
import { PriorityChip } from "../priorities/PriorityChip";
import { AgentName, useAgentText } from "../refs/AgentName";
import { referenceTriggerProps } from "../refs/RefPreview";
import { buildIssuePath } from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";
import { type IssueStatus, issueStatuses, statusLabel } from "./board-model";
import { ROLE_PICKS, WAITING_REASON_PICKS, WORKER_STATE_PICKS } from "./issues-filter-picks";
import {
  formatPhaseTime,
  formatWorkTime,
  LEGION_PHASES,
  type LegionFacts,
  type LegionPhaseColumn,
  sinceMs,
} from "./issues-legion";
import {
  PhaseHoursCell,
  type PhaseHoursMode,
  PhaseHoursModeControl,
  usePhaseHoursMode,
} from "./issues-phase-hours";
import type { AdvancesListing } from "./issues-table-data";
import {
  type ColumnFilterSpec,
  type ColumnId,
  columnFilterSpec,
  columnSpec,
  createdAt,
  createdBy,
  embeddedPhoneMin,
  ISSUE_TABLE_COLUMNS,
  type Item,
  isBoardFedColumn,
  legionPhaseFilter,
  MEASURE_METHODS,
  matchesFilters,
  phaseHoursTotal,
  phoneMin,
  waitingReasonWords,
  workerName,
  workerStateWords,
  workHoursOf,
} from "./issues-table-model";
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
    /** Filter by a window of time (`RangeFilter`): for a column of moments. */
    range?: boolean;
    /** How the column's measure is taken, in one or two sentences (`MEASURE_METHODS`), which
     *  its header's tooltip says before the sort hint and the Columns menu shows under its
     *  name. */
    describe?: string;
    /** A control of the column's own that its header draws beside the filter: Time per
     *  phase's Share and Hours. */
    headerControl?: ReactNode;
    /** A column of counts: its header's words stand at its right edge, over its numbers, and
     *  wrap rather than be cut (`DataTable`'s header). */
    numeric?: boolean;
    /** A header whose words wrap onto a second line rather than be cut, in a column narrower
     *  than its words on one line. */
    wrap?: boolean;
    /** The types for the declaration's own parameters, which it does not otherwise use. */
    value?: [TData, TValue];
  }
}
const inPicks: FilterFn<Item> = (row, id, value: string[] | string) =>
  matchesFilters(row.original, [{ id, value }]);

/** A window column's filter (Closed): the window its value names, read once for each pass of the
 *  filter against one clock (`resolveFilterValue`), and each row's moment tested against it. The
 *  window travels in an object, since TanStack hands the filter the raw value where the resolved
 *  one is undefined, and undefined is no window. */
const inRange: FilterFn<Item> = (
  row,
  id,
  resolved: { readonly window: ClosedWindow | undefined }
) => {
  const filter = columnFilterSpec(id);
  return filter?.kind === "range" && inWindow(filter.value(row.original), resolved.window);
};
inRange.resolveFilterValue = (value: unknown) => ({
  window: readWindow(String(value ?? ""), Date.now()),
});

/** The filter of each kind of column filter. */
const FILTER_FNS = {
  text: "includesString",
  picks: inPicks,
  range: inRange,
} as const satisfies Record<ColumnFilterSpec["kind"], FilterFnOption<Item>>;

/** Where `value` stands in `order`, a value this build does not know after the known ones, or
 *  undefined with no value: a column of named states sorts in their order, its empty cells last
 *  (`emptyLast`). */
function placeIn(order: readonly string[], value: string | null | undefined): number | undefined {
  if (value === null || value === undefined || value === "") return undefined;
  const index = order.indexOf(value);
  return index === -1 ? order.length : index;
}

/** How a column whose cells can be empty sorts: its sorting function, and no answer of TanStack's
 *  own for an empty cell before that function is asked (`emptyLast`). */
interface EmptyLastSort {
  readonly sortingFn: SortingFn<Item>;
  readonly sortUndefined: false;
}

/** Whether the table sorts the column `id` descending now, by its sorting state. */
type SortsDescending = (id: string) => boolean;

/**
 * The sort of a column whose cells can be empty (an undefined accessor value): two values by
 * `compare`, the empty cells last whichever way the column is sorted, and two empty cells tied,
 * so that the next sort key orders them and, after the last, the listing's own order. TanStack
 * answers for a pair with an empty cell itself unless `sortUndefined` is false: `"last"` keeps
 * the empty cells last but answers for two of them too, before the next key is asked, and its
 * default puts them first in a descending sort. The table turns a sorting function's answer round
 * for a descending sort, so the answer for an empty cell is turned round here first, by the
 * direction the table sorts the column in (`descending`).
 */
function emptyLast<V>(
  compare: (left: V, right: V) => number,
  descending: SortsDescending
): EmptyLastSort {
  return {
    sortUndefined: false,
    sortingFn: (a, b, id) => {
      const left = a.getValue<V | undefined>(id);
      const right = b.getValue<V | undefined>(id);
      if (left !== undefined && right !== undefined) return compare(left, right);
      if (left === right) return 0;
      const last = left === undefined ? 1 : -1;
      return descending(id) ? -last : last;
    },
  };
}

/** Two numbers, the smaller first, and the empty cells last. */
const smallestFirst = (descending: SortsDescending) =>
  emptyLast((left: number, right: number) => left - right, descending);

/** Two runs of a card's time by the moment each started, as lengths: the later start is the
 *  shorter time, so it sorts first, and the empty cells last. */
const shortestFirst = (descending: SortsDescending) =>
  emptyLast((left: number, right: number) => right - left, descending);

/** What a facts cell draws while the read of its facts is out, or after it failed: a loading
 *  mark or an error mark, and never the empty cell of an issue with no Legion work. */
function FactsReadMark({ read }: { read: "pending" | "error" }): ReactNode {
  return read === "pending" ? (
    <span data-facts-read="pending">
      <span
        aria-hidden="true"
        className={`inline-block h-3 w-10 animate-pulse rounded align-middle ${skeletonBg}`}
      />
      <span className="sr-only">Loading</span>
    </span>
  ) : (
    <span className={`text-xs font-semibold ${dangerText}`} data-facts-read="error">
      <span aria-hidden="true">!</span>
      <span className="sr-only">Could not load</span>
    </span>
  );
}

/** A facts cell: its read's mark while that read is out or failed, nothing for an issue with no
 *  Legion work, else what `draw` makes of the facts. */
function factsCell(item: Item, draw: (facts: LegionFacts) => ReactNode): ReactNode {
  if (item.factsRead !== "success") return <FactsReadMark read={item.factsRead} />;
  return item.facts === undefined ? null : draw(item.facts);
}

/** A card's time, from `since` to the row's clock (`Item.at`), in the words of the Board's card;
 *  nothing with no time. */
function CardTime({
  since,
  nowMs,
}: {
  since: string | null | undefined;
  nowMs: number;
}): ReactNode {
  return since === null || since === undefined ? null : (
    <span className={`text-xs tabular-nums ${textSecondaryOnCanvas}`}>
      {formatPhaseTime(since, nowMs)}
    </span>
  );
}

/**
 * The Queue cell: the issue's number in the Legion queue, or "In Legion queue" for a card of the
 * board's Queue column that the daemon's line gives no number. With no daemon the number is
 * today's: the Todo issues labelled `legion`, in rank order. The number says what it is in the
 * shared tooltip, on hover and on keyboard focus, so it is a Tab stop; it carries no native
 * title.
 */
function QueuePlace({ place, queued }: { place: number | undefined; queued: boolean }): ReactNode {
  const anchorRef = useRef<HTMLSpanElement>(null);
  const tooltip = useTooltip();
  if (place !== undefined)
    return (
      <span
        {...tooltip.props}
        className={`rounded-sm font-semibold tabular-nums outline-none focus-visible:ring-2 ${focusVisibleRing}`}
        data-queue-place={place}
        ref={anchorRef}
        // biome-ignore lint/a11y/noNoninteractiveTabindex: the Tab stop opens the tooltip that says what the number is.
        tabIndex={0}
      >
        #{place}
        <Tooltip anchorRef={anchorRef} trigger={tooltip}>
          Legion starts issues in this order
        </Tooltip>
      </span>
    );
  return queued ? <span className={`text-xs ${textMutedOnCanvas}`}>In Legion queue</span> : null;
}

/** The longest Time per phase total among the rows of a page, which Hours draws each bar
 *  against. Counted once per page: TanStack hands back the same page model until it changes. */
const PAGE_LONGEST = new WeakMap<RowModel<Item>, number>();
function pageLongest(table: Table<Item>): number {
  const page = table.getRowModel();
  const known = PAGE_LONGEST.get(page);
  if (known !== undefined) return known;
  let longest = 0;
  for (const row of page.rows)
    if (!row.getIsGrouped()) longest = Math.max(longest, phaseHoursTotal(row.original.facts));
  PAGE_LONGEST.set(page, longest);
  return longest;
}

/** Who created the issue. Where its table clamps (`clamp`), the cell takes two lines at most, and
 *  three for a session whose name holds a "·" ("Legion architect · INFRA-3320"): the key after
 *  the dot names the issue its creator worked on, and the name draws each "·" on the line of the
 *  word before it (`AgentName`), so a name whose dot ends the second line puts that key on a third
 *  line, which a clamp at two would cut with no tooltip to give it back. A person, a session whose
 *  name has no "·" and the unknown creator keep two. A server of the release before the one that
 *  added `created_by` sends none, so the cell asks `createdBy`: an absent creator reads as the
 *  unknown one rather than reaching `AgentName` as undefined, which would throw and take the
 *  whole table with it. Without it, `IssuesTable.test.tsx`'s "a row a server sends without
 *  created_by or created_at renders, with no author and no time" fails in `AgentName`. */
function Author({ issue, clamp }: { issue: IssueSummary; clamp: boolean }): ReactNode {
  const text = useAgentText();
  const actor = createdBy(issue);
  if (actor === null) return <span className={textSecondaryOnCanvas}>Unknown</span>;
  if (!clamp) return <AgentName actor={actor} />;
  // Whole class names, which Tailwind reads as written. `label` spells the name the cell draws,
  // all but the no-break space `AgentName` draws before each "·".
  const lines =
    actor.kind === "session" && text.label(actor).includes("·") ? "line-clamp-3" : "line-clamp-2";
  return (
    <span className={`${lines} break-words`}>
      <AgentName actor={actor} />
    </span>
  );
}

/** How each column renders and sorts on the Issues tab, for the reader `userState` describes,
 *  with each title in eight lines at most where the table wraps its titles (`wrapTitles`),
 *  ending in '…' with the whole of it in its tooltip where it runs longer, and in two lines that
 *  end in '…' otherwise, and each claim whole there too, its holder's name wrapping rather than
 *  cut (`ClaimChip`'s `wraps`). Keyed by the registry's own ids, so a typo or a column with no
 *  rendering fails to compile. Everything else about a column — whether it filters, what a row
 *  files under, what it groups by, how wide it starts and how narrow it may go — comes from
 *  `COLUMNS` in the model, so the two never disagree. A row's own checkbox tells `onMark` the
 *  row it marks. A column whose rows can have no value sorts those rows last whichever way the
 *  table sorts it (`descending`), and by the next sort key among themselves (`emptyLast`).
 *  Time per phase draws in the reader's mode (`usePhaseHoursMode`), which its header's control
 *  switches. Legion phase sorts in the order of `phaseOrder`, the board's columns.
 */
const renderings = ({
  userState,
  onMark,
  wrapTitles,
  phaseMode,
  setPhaseMode,
  descending,
  phaseOrder,
}: {
  userState: UserState | undefined;
  onMark: (key: string) => void;
  wrapTitles: boolean;
  phaseMode: PhaseHoursMode;
  setPhaseMode: (mode: PhaseHoursMode) => void;
  descending: SortsDescending;
  phaseOrder: readonly string[];
}): Record<ColumnId, Partial<ColumnDef<Item>>> => ({
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
        onChange={(event) => {
          if (event.target.checked) onMark(row.id);
          row.toggleSelected(event.target.checked);
        }}
        type="checkbox"
      />
    ),
    enableHiding: false,
  },
  priority: {
    header: "Priority",
    accessorFn: (item) => item.issue.priority ?? undefined,
    ...smallestFirst(descending),
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
    header: "Queue",
    accessorFn: (item) => item.place,
    sortDescFirst: false,
    sortUndefined: false,
    // By the place, then the rows with no place by rank, which is the order Legion starts the
    // issues it admits in: so a Queue card with no place follows the numbered ones.
    sortingFn: (a, b) => {
      const left = a.original.place;
      const right = b.original.place;
      if (left !== undefined && right !== undefined) return left - right;
      if (left !== undefined) return -1;
      if (right !== undefined) return 1;
      const rankA = a.original.issue.rank;
      const rankB = b.original.issue.rank;
      return rankA < rankB ? -1 : rankA > rankB ? 1 : 0;
    },
    cell: ({ row }) => <QueuePlace place={row.original.place} queued={row.original.queued} />,
  },
  key: {
    header: "Key",
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
    header: "Title",
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
          <span
            className={`${wrapTitles ? "line-clamp-8" : "line-clamp-2"} min-w-0 flex-1 break-words ${textPrimaryOnCanvas}`}
            title={wrapTitles ? row.original.issue.title : undefined}
          >
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
    header: "Status",
    accessorFn: (item) => item.issue.status,
    sortingFn: (a, b) =>
      issueStatuses.indexOf(a.original.issue.status as IssueStatus) -
      issueStatuses.indexOf(b.original.issue.status as IssueStatus),
    cell: ({ getValue }) => <StatusPill>{statusLabel(getValue<IssueStatus>())}</StatusPill>,
  },
  legion_phase: {
    header: "Legion phase",
    // The board's order, the rows with no card last. The cell reads the board's own label of the
    // card's column, so a column a newer server adds reads as that server names it.
    accessorFn: (item) => placeIn(phaseOrder, item.card?.column),
    ...smallestFirst(descending),
    sortDescFirst: false,
    cell: ({ row }) =>
      row.original.card === undefined ? null : (
        <span className={`text-xs whitespace-nowrap ${textSecondaryOnCanvas}`}>
          {row.original.card.label}
        </span>
      ),
  },
  assignee: {
    header: "Assignee",
    accessorFn: (item) => item.issue.assignee ?? "Unassigned",
    // A narrow column ends a long address with an ellipsis.
    cell: ({ getValue }) => (
      <span className={`block truncate ${textSecondaryOnCanvas}`}>{getValue<string>()}</span>
    ),
  },
  author: {
    header: "Created by",
    accessorFn: (item) => item.author,
    // A session's long name ends on its second line, as the title does, so the row stays as
    // tall as its title; one whose name holds a "·" ends on its third, so the key after the dot
    // is drawn whole (`Author`).
    cell: ({ row }) => <Author clamp issue={row.original.issue} />,
  },
  labels: {
    header: "Labels",
    accessorFn: (item) => (item.issue.labels ?? []).join(", "),
    // A label wider than the column wraps inside its own pill rather than run past the cell,
    // which would cut it with no ellipsis: a label is a value, read whole.
    cell: ({ row }) => (
      <span className="flex flex-wrap gap-1">
        {(row.original.issue.labels ?? []).map((label) => (
          <LabelPill key={label} wraps>
            {label}
          </LabelPill>
        ))}
      </span>
    ),
  },
  claim: {
    header: "Claimed by",
    accessorFn: (item) => item.claimedBy,
    cell: ({ row }) => <ClaimChip claim={row.original.issue.claim} wraps={wrapTitles} />,
  },
  asks: {
    header: "Open asks",
    accessorFn: (item) => item.issue.open_asks,
    sortDescFirst: true,
    cell: ({ getValue }) =>
      getValue<number>() === 0 ? null : <AttentionBadge count={getValue<number>()} />,
  },
  created: {
    header: "Created",
    accessorFn: (item) => createdAt(item.issue),
    // A server of the release before the one that added `created_at` sends none, and the table
    // sorts such a row last (`emptyLast`), so what `compareTimestamps` compares is always a time
    // it can parse. `IssuesTable.test.tsx` sorts a row without a time in both directions.
    ...emptyLast(compareTimestamps, descending),
    cell: ({ row }) => {
      // The cell is empty for a row that server sent, which carries no time to show, which the
      // same row of `IssuesTable.test.tsx` reads.
      const at = createdAt(row.original.issue);
      return at === undefined ? null : (
        <Timestamp at={at} className={`whitespace-nowrap text-xs ${textMutedOnCanvas}`} />
      );
    },
  },
  updated: {
    header: "Updated",
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
  closed: {
    header: "Closed",
    accessorFn: (item) => item.issue.closed_at ?? undefined,
    ...emptyLast(compareTimestamps, descending),
    cell: ({ row }) =>
      row.original.issue.closed_at === null ? null : (
        <Timestamp
          at={row.original.issue.closed_at}
          className={`whitespace-nowrap text-xs ${textMutedOnCanvas}`}
        />
      ),
  },
  roles: {
    header: "Workers",
    // In `LEGION_ROLES`' order, by the first role and then the next.
    accessorFn: (item) => {
      const places = (item.card?.card.roles ?? []).flatMap(
        (role) => placeIn(ROLE_PICKS, role) ?? []
      );
      return places.length === 0 ? undefined : places.sort((a, b) => a - b);
    },
    ...emptyLast((left: number[], right: number[]) => {
      for (let index = 0; index < Math.min(left.length, right.length); index++) {
        const step = (left[index] ?? 0) - (right[index] ?? 0);
        if (step !== 0) return step;
      }
      return left.length - right.length;
    }, descending),
    sortDescFirst: false,
    cell: ({ row }) => (
      <span className={`text-xs ${textSecondaryOnCanvas}`}>
        {[...(row.original.card?.card.roles ?? [])]
          .sort((a, b) => (placeIn(ROLE_PICKS, a) ?? 0) - (placeIn(ROLE_PICKS, b) ?? 0))
          .map(workerName)
          .join(", ")}
      </span>
    ),
  },
  activity: {
    header: "Worker state",
    accessorFn: (item) => placeIn(WORKER_STATE_PICKS, item.card?.card.slot_use),
    ...smallestFirst(descending),
    sortDescFirst: false,
    cell: ({ row }) => {
      const use = row.original.card?.card.slot_use;
      return use === null || use === undefined || use === "" ? null : (
        <span className={`text-xs ${textSecondaryOnCanvas}`}>{workerStateWords(use)}</span>
      );
    },
  },
  waited: {
    header: "Time waiting",
    accessorFn: (item) => sinceMs(item.card?.card.waiting?.since),
    ...shortestFirst(descending),
    sortDescFirst: true,
    cell: ({ row }) => (
      <CardTime nowMs={row.original.at} since={row.original.card?.card.waiting?.since} />
    ),
  },
  waiting_reason: {
    header: "Why it waits",
    accessorFn: (item) => placeIn(WAITING_REASON_PICKS, item.card?.card.waiting?.reason),
    ...smallestFirst(descending),
    sortDescFirst: false,
    cell: ({ row }) => {
      const waiting = row.original.card?.card.waiting;
      if (waiting === null || waiting === undefined || waiting.reason === "") return null;
      // After A blocker, the issues that block it.
      const blockers = waiting.reason === "blocked" ? (waiting.blocked_by ?? []) : [];
      return (
        <span className={`flex flex-wrap items-center gap-x-1 text-xs ${textSecondaryOnCanvas}`}>
          {waitingReasonWords(waiting.reason)}
          {blockers.map((key) => (
            <Link
              className={`${linkText} ${linkHoverText}`}
              key={key}
              to={buildIssuePath({ key, kind: "issue" })}
              {...referenceTriggerProps({ key, kind: "issue" })}
            >
              {key}
            </Link>
          ))}
        </span>
      );
    },
  },
  time_in_phase: {
    header: "Time in Legion phase",
    accessorFn: (item) => sinceMs(item.card?.card.phase_since),
    ...shortestFirst(descending),
    sortDescFirst: true,
    cell: ({ row }) => (
      <CardTime nowMs={row.original.at} since={row.original.card?.card.phase_since} />
    ),
  },
  work_hours: {
    header: "Work time",
    accessorFn: workHoursOf,
    ...smallestFirst(descending),
    sortDescFirst: true,
    cell: ({ row }) =>
      factsCell(row.original, () => {
        const hours = workHoursOf(row.original);
        return hours === undefined ? null : (
          <span className={`text-xs tabular-nums ${textSecondaryOnCanvas}`}>
            {formatWorkTime(hours)}
          </span>
        );
      }),
  },
  review_rounds: {
    header: "Review rounds",
    accessorFn: (item) => item.facts?.review_rounds,
    ...smallestFirst(descending),
    sortDescFirst: true,
    // A counted zero reads 0: Legion worked the issue and no review round reached it.
    cell: ({ row }) =>
      factsCell(row.original, (facts) => (
        <span className="tabular-nums">{facts.review_rounds}</span>
      )),
  },
  legion: {
    header: "Legion tree",
    // The roots first, then the children by their root.
    accessorFn: (item) =>
      item.facts === undefined ? undefined : item.facts.tree_depth === 0 ? "" : item.facts.root_key,
    ...emptyLast(
      (left: string, right: string) => left.localeCompare(right, undefined, { numeric: true }),
      descending
    ),
    sortDescFirst: false,
    cell: ({ row }) =>
      factsCell(row.original, (facts) =>
        facts.tree_depth === 0 ? (
          <span className="text-xs">Root</span>
        ) : (
          <Link
            className={`text-xs ${linkText} ${linkHoverText}`}
            to={buildIssuePath({ key: facts.root_key, kind: "issue" })}
            {...referenceTriggerProps({ key: facts.root_key, kind: "issue" })}
          >
            {facts.root_key}
          </Link>
        )
      ),
  },
  phase_hours: {
    header: "Time per phase",
    // By the total, where an empty cell counts 0.
    accessorFn: (item) => phaseHoursTotal(item.facts),
    sortingFn: "basic",
    sortDescFirst: true,
    cell: ({ row, table }) =>
      factsCell(row.original, (facts) => (
        <PhaseHoursCell
          facts={facts}
          mode={phaseMode}
          pageMax={phaseMode === "hours" ? pageLongest(table) : 0}
        />
      )),
    meta: {
      headerControl: <PhaseHoursModeControl mode={phaseMode} onChange={setPhaseMode} />,
    },
  },
});

/**
 * The cells a table another page draws (the priority pages') draws as today, where the Issues
 * tab's differ: Queue, Start next's number with the rows that have none after the rest in the
 * listing's order (where the Issues tab's is the daemon's own number), Assignee whole on one
 * line, and Created by at every line its name takes. Queue sorts its empty cells last by
 * `descending`, as the Issues tab's columns do.
 */
const embeddedRenderings = (
  descending: SortsDescending
): Readonly<Partial<Record<ColumnId, Partial<ColumnDef<Item>>>>> => ({
  queue: {
    header: "Queue",
    accessorFn: (item) => item.queue,
    ...smallestFirst(descending),
    sortDescFirst: false,
    cell: ({ row }) => <QueuePlace place={row.original.queue} queued={false} />,
  },
  assignee: {
    header: "Assignee",
    accessorFn: (item) => item.issue.assignee ?? "Unassigned",
    cell: ({ getValue }) => (
      <span className={`whitespace-nowrap ${textSecondaryOnCanvas}`}>{getValue<string>()}</span>
    ),
  },
  author: {
    header: "Created by",
    accessorFn: (item) => item.author,
    cell: ({ row }) => <Author clamp={false} issue={row.original.issue} />,
  },
});

/** The columns only some listings have a use for: which project a row is in, which priorities
 *  it advances, and when it was moved into backlog or icebox and by whom. A table has each only
 *  where its listing gives the cell something true to say (`listingColumns`). */
export type ListingColumnId = "project" | "advances" | "moved";

/**
 * The listing columns a table of `listing`, the work of every project that advances a priority,
 * has, `titled` when its caller gives each priority's title:
 *
 * - Project, always: the listing stands on no project, and a row names the one it is in. The
 *   Issues tab, one project's own table, has none of these columns (`viewLayoutColumns`).
 * - Advances, where the titles are given: a title is the only name a reader knows a priority by,
 *   and without them the column could name each priority only by its id.
 * - Moved there, on a listing that asks for the move (`moved`): no other listing is answered with
 *   one, so its every cell would be empty, whatever the record holds.
 */
export function listingColumns(listing: AdvancesListing, titled: boolean): ListingColumnId[] {
  return [
    "project",
    ...(titled ? (["advances"] as const) : []),
    ...(listing.moved === true ? (["moved"] as const) : []),
  ];
}

/** What the Advances column knows of one priority by its id: its place in the one order and its
 *  title, each absent for a priority the priorities read does not hold (`PriorityChip`). */
type PriorityOf = (id: string) => { place: number | undefined; title: string | undefined };

/**
 * The three listing columns. They name what R6 asks each fault row to name, and none of them
 * sorts, filters or groups - deliberately: the table's view in the address reads only the
 * columns `features/project/issues-table-model.ts` registers (`readUrl` drops any other id), and
 * these are the cells' own, kept out of that registry. Every registered column still sorts,
 * filters and groups on every table that shows these beside it. A table has only the ones its
 * listing gives a use (`listingColumns`).
 */
const LISTING_COLUMNS: readonly {
  readonly id: ListingColumnId;
  readonly size: number;
  readonly min: number;
  readonly rendering: (priorityOf: PriorityOf) => Partial<ColumnDef<Item>>;
}[] = [
  {
    id: "project",
    size: 120,
    min: 96,
    rendering: () => ({
      header: "Project",
      accessorFn: (item) => issueProject(item.issue.key),
      cell: ({ getValue }) => (
        <span className={`whitespace-nowrap ${textSecondaryOnCanvas}`}>{getValue<string>()}</span>
      ),
    }),
  },
  {
    id: "advances",
    // Each priority is its chip and its title after it, wrapped whole: the cell is where a fault
    // row says which priority it stands under, and a phone has no tooltip to read a title in. A
    // layout names its own width where its other columns leave this one more or less room.
    size: 200,
    min: 160,
    rendering: (priorityOf) => ({
      header: "Advances",
      accessorFn: (item) =>
        (item.issue.advances ?? []).map(({ id }) => priorityOf(id).title ?? id).join(", "),
      cell: ({ row }) => (
        <span className="flex flex-col gap-1">
          {(row.original.issue.advances ?? []).map(({ id, inherited }) => (
            <PriorityChip id={id} inherited={inherited} key={id} titled {...priorityOf(id)} />
          ))}
        </span>
      ),
    }),
  },
  {
    id: "moved",
    size: 220,
    min: 160,
    rendering: () => ({
      header: "Moved there",
      accessorFn: (item) => item.issue.moved_at ?? "",
      // A row the listing answered with no move - it stands outside backlog and icebox, or the
      // issue's events do not reach back to the move - draws an empty cell: absent is absent,
      // and no cell claims a record is missing.
      cell: ({ row }) => {
        const { moved_at: at, moved_by: by } = row.original.issue;
        return at === undefined ? null : (
          <span className="flex flex-wrap items-center gap-1">
            <Timestamp at={at} className={`whitespace-nowrap text-xs ${textMutedOnCanvas}`} />
            {by === undefined ? null : <AgentName actor={by} />}
          </span>
        );
      },
    }),
  },
];

/** A column an issues table can open with shown, or give a width of its own: one of the
 *  registry's (`ColumnId`) or a listing column (`ListingColumnId`). */
export type IssueTableColumnId = ColumnId | ListingColumnId;

/** A column's width where a table's layout names one: where it starts, and the narrowest the
 *  reader may drag it (`size`, `min`); or what it holds (`fit`): as wide as its header or its
 *  widest cell on the page needs on one line, measured as the table draws them, and where `most`
 *  is given no wider than that, past which its cells wrap (`LayoutColumns.fits`) - until it is
 *  measured it stands at `most`, else its own width. Either, given `leaves`, gives Title, the
 *  column that fills the box, room in a desktop's box: where the others would leave Title less
 *  than that, it narrows - toward `min`, or toward the narrowest what it holds needs - to leave
 *  it that much (`LayoutColumns.yields`), each such column in turn in the order the layout names
 *  them. A column of set width that does so draws its header's words wrapped beside its filter
 *  once it is narrower than they are on one line (`DataTable`'s `wrap`), and is never narrower
 *  than its header needs as drawn, sorted or not (`LayoutColumns.wrappedHeaders`). */
export type IssueTableColumnWidth =
  | { readonly size: number; readonly min: number; readonly leaves?: number }
  | { readonly fit: true; readonly most?: number; readonly leaves?: number };

/** The widths a table's layout names, by column: each over the registry's or the listing
 *  column's own. */
export type IssueTableWidths = Readonly<Partial<Record<IssueTableColumnId, IssueTableColumnWidth>>>;

/** The width an issues table draws after its columns, for a row's actions, where its listing can
 *  hold a row they act on (`listsQueued`) and its layout does not leave them out (`rowActions`,
 *  `ListingTable`). The Issues tab always draws them. */
export const ROW_ACTIONS_WIDTH = 104;

/** The one column no reader can hide or show (`enableHiding: false`): the row's checkbox, which
 *  `x` selects with. A layout that names what it opens with keeps it whatever it names. */
const ALWAYS_SHOWN: ColumnId = "select";

/** Each layout `issueTableLayout` built, by its listing columns, the columns it opens with (or
 *  none, for today's opening set), the widths it names, the row's actions' width and the
 *  columns it pins. */
const layouts = new Map<string, LayoutColumns>();

/**
 * The layout of an issues table another page draws, with the listing columns `columns`
 * (`listingColumns`): today's (`ISSUE_TABLE_COLUMNS`), with those listing columns at their
 * widths. With no `opens` it opens as such a table always has: today's opening set, Updated and
 * Closed hidden, and every listing column hidden until the reader shows it. With `opens`, it
 * opens with exactly those columns shown, registry and listing columns alike, and every other
 * one hidden until the reader shows it from the column control; the row's checkbox stays
 * (`ALWAYS_SHOWN`), and Title fills a desktop box (`fills`, `useTableLayout`): it takes the
 * width the other shown columns and the row's actions (`actions`, none where the table draws
 * none) leave.
 * `widths` puts a column's starting width and minimum in place of the registry's or the listing
 * column's, with the room it leaves Title where it names one (`yields`), or makes the column as
 * wide as what it holds (`fits`); and `pins` the columns pinned at the left of a desktop's box in
 * place of the registry's (`LayoutPins.wide`), where the registry's would count a column the
 * table opens hidden. A table that names none of them opens as it always has; each fault list of
 * the priorities page and a priority's work table name their own. Built once for each set and
 * kept, since the stored layout is read again whenever this object changes (`useTableLayout`).
 */
export function issueTableLayout(
  columns: readonly ListingColumnId[],
  opens?: readonly IssueTableColumnId[],
  widths: IssueTableWidths = {},
  actions = ROW_ACTIONS_WIDTH,
  pins?: readonly IssueTableColumnId[]
): LayoutColumns {
  const name = `${columns.join(" ")}|${opens === undefined ? "*" : opens.join(" ")}|${JSON.stringify(widths)}|${actions}|${pins === undefined ? "*" : pins.join(" ")}`;
  const built = layouts.get(name);
  if (built !== undefined) return built;
  const listing = LISTING_COLUMNS.filter(({ id }) => columns.includes(id));
  const ids = [...ISSUE_TABLE_COLUMNS.ids, ...listing.map(({ id }) => id)];
  const shown = new Set<string>(opens);
  const named = Object.entries(widths).flatMap(([id, width]) =>
    width === undefined ? [] : [[id, width] as const]
  );
  const set = named.flatMap(([id, width]) => ("fit" in width ? [] : [[id, width] as const]));
  const fitted = named.flatMap(([id, width]) => ("fit" in width ? [[id, width] as const] : []));
  const yielding = named.flatMap(([id, { leaves }]) =>
    leaves === undefined ? [] : [[id, leaves] as const]
  );
  // A column of set width that gives way can narrow below its header's words on one line, so its
  // header's words wrap there, and the column is never narrower than its header needs as drawn.
  const wrapped = set.flatMap(([id, { leaves }]) => (leaves === undefined ? [] : [id]));
  const registryPins = ISSUE_TABLE_COLUMNS.pins;
  // A column as wide as what it holds is never narrower than that (`useTableLayout`), and no
  // minimum of its own holds it wider.
  const minimums = {
    ...ISSUE_TABLE_COLUMNS.minimums,
    ...Object.fromEntries(listing.map(({ id, min }) => [id, min])),
    ...Object.fromEntries(set.map(([id, { min }]) => [id, min])),
    ...Object.fromEntries(fitted.map(([id]) => [id, 0])),
  };
  const layout: LayoutColumns = {
    ...ISSUE_TABLE_COLUMNS,
    ids,
    hiddenByDefault:
      opens === undefined
        ? {
            ...ISSUE_TABLE_COLUMNS.hiddenByDefault,
            ...Object.fromEntries(listing.map(({ id }) => [id, false])),
          }
        : Object.fromEntries(
            ids.filter((id) => id !== ALWAYS_SHOWN && !shown.has(id)).map((id) => [id, false])
          ),
    sizes: {
      ...ISSUE_TABLE_COLUMNS.sizes,
      ...Object.fromEntries(listing.map(({ id, size }) => [id, size])),
      ...Object.fromEntries(set.map(([id, { size }]) => [id, size])),
      ...Object.fromEntries(
        fitted.flatMap(([id, { most }]) => (most === undefined ? [] : [[id, most]]))
      ),
    },
    minimums,
    // A sort widens no column of these tables, as on today's (R13): each minimum holds its header
    // with the sort mark already (`ISSUE_TABLE_COLUMNS`), and a column as wide as what it holds
    // (`fits`) or whose header's words wrap (`wrappedHeaders`) is measured with the mark it draws.
    // So each column's sorted minimum is its minimum here, the one this layout names in place of
    // the registry's too, and Title keeps the floor the others leave it (`yields`) in every sort.
    sortedMinimums: minimums,
    ...(fitted.length === 0 ? {} : { fits: Object.fromEntries(fitted) }),
    ...(yielding.length === 0 ? {} : { yields: Object.fromEntries(yielding) }),
    ...(wrapped.length === 0 ? {} : { wrappedHeaders: wrapped }),
    ...(pins === undefined || registryPins === undefined
      ? {}
      : { pins: { ...registryPins, wide: { ...registryPins.wide, columns: pins } } }),
    ...(opens === undefined ? {} : { fills: "title", trailing: actions }),
  };
  layouts.set(name, layout);
  return layout;
}

/** Which table the columns are drawn for: a table another page draws over a listing of its own
 *  (`embedded`), which draws today's cells at today's widths, or the Issues tab's own (`tab`),
 *  whose board-fed columns are neither in the Columns menu nor in Group by in a project with no
 *  Legion daemon (`hasLegion`), and whose Legion phase column offers, sorts and groups by the
 *  board's columns (`phases`, `legionPhasesOf`). */
export type IssueTableMode =
  | {
      readonly kind: "embedded";
      /** Each priority's title by its id, in the order the priorities read answers them, which
       *  is the one order: the Advances column names each priority by its place in this map
       *  (`#3`) with its title. One this map does not hold, a priority deleted since it was
       *  read, is named by its id. */
      readonly priorityTitles?: ReadonlyMap<string, string>;
      /** Whether each title wraps onto up to eight lines, with the whole of it in its tooltip,
       *  rather than two (`ListingTable`'s `wrapTitles`). */
      readonly wrapTitles?: boolean;
    }
  | {
      readonly kind: "tab";
      readonly hasLegion: boolean;
      readonly phases: readonly LegionPhaseColumn[];
    };

/**
 * The columns of `layout`, in its order: the registry's (`viewLayoutColumns` on the Issues tab,
 * `issueTableLayout` elsewhere), and a scoped table's listing columns after Title. `onMark` is
 * told the row a row's own checkbox marks; it is a dependency of the columns, so a caller hands a
 * stable one, as it does `layout` and the embedded mode's `priorityTitles`. A column starts at
 * the layout's width and goes no narrower than its minimum there (`compact`: a phone's). `mode`
 * says which table they are drawn for. Time per phase's mode is the reader's
 * (`usePhaseHoursMode`), and a switch of it draws the columns anew, as a change of the board's
 * phases does. A column whose cells can be empty reads the direction it is sorted in from
 * `sorting` (`emptyLast`), through a ref set at each draw, so a change of sort draws no column
 * anew.
 */
export function useIssueTableColumns(
  userState: UserState | undefined,
  compact: boolean,
  onMark: (key: string) => void,
  /** The table's layout (`viewLayoutColumns`, `issueTableLayout`): which columns it has, their
   *  widths, and the narrowest each column may be drawn at, a layout's `widths` included. */
  layout: LayoutColumns,
  /** The sorting state the table is given, which the table sorts its rows by. */
  sorting: SortingState,
  mode: IssueTableMode
): ColumnDef<Item>[] {
  const [phaseMode, setPhaseMode] = usePhaseHoursMode();
  const sorted = useRef(sorting);
  sorted.current = sorting;
  const embedded = mode.kind === "embedded";
  const priorityTitles = mode.kind === "embedded" ? mode.priorityTitles : undefined;
  const wrapTitles = mode.kind === "embedded" && mode.wrapTitles === true;
  // A board-fed column of a project with no Legion daemon has nothing to draw.
  const noLegion = mode.kind === "tab" && !mode.hasLegion;
  // A table another page draws has no Legion phase column: today's phases stand in.
  const phases = mode.kind === "tab" ? mode.phases : LEGION_PHASES;
  return useMemo<ColumnDef<Item>[]>(() => {
    const descending = (id: string) => sorted.current.find((sort) => sort.id === id)?.desc === true;
    const phaseFilter = legionPhaseFilter(phases);
    const rendering = renderings({
      userState,
      onMark,
      wrapTitles,
      phaseMode,
      setPhaseMode,
      descending,
      phaseOrder: phaseFilter.order,
    });
    const places = new Map([...(priorityTitles?.keys() ?? [])].map((id, at) => [id, at + 1]));
    const priorityOf: PriorityOf = (id) => ({
      place: places.get(id),
      title: priorityTitles?.get(id),
    });
    const embeddedRendering = embedded ? embeddedRenderings(descending) : undefined;
    const registered = layout.ids.flatMap((id): ColumnDef<Item>[] => {
      const column = columnSpec.get(id);
      if (column === undefined) return [];
      // Legion phase offers the board's columns, in its order and under its labels.
      const filter = column.id === "legion_phase" ? phaseFilter : column.filter;
      const picks = filter?.kind === "picks" ? filter : undefined;
      const unfed = noLegion && isBoardFedColumn(column.id);
      // Today's Queue filter names its values as they are, Queued and Not queued.
      const show =
        filter?.kind === "range"
          ? rangeWords
          : embedded && column.id === "queue"
            ? undefined
            : picks?.show;
      const describe = MEASURE_METHODS[column.id];
      // On a phone, a column of a table another page draws keeps the width its registry names
      // for a phone, else the minimum its layout names, else today's.
      const minSize = compact
        ? embedded
          ? embeddedPhoneMin(column.id, layout.minimums?.[column.id])
          : phoneMin(column.id)
        : (layout.minimums?.[column.id] ?? column.min);
      const drawn = embeddedRendering?.[column.id] ?? rendering[column.id];
      return [
        {
          ...drawn,
          id: column.id,
          size: layout.sizes?.[column.id] ?? column.size,
          minSize,
          enableResizing: column.id !== "select",
          enableSorting: column.sortable !== false,
          enableColumnFilter: filter !== undefined,
          enableGrouping: picks?.group !== undefined && !unfed,
          ...(unfed ? { enableHiding: false } : {}),
          ...(picks?.group === undefined ? {} : { getGroupingValue: picks.group }),
          ...(filter === undefined ? {} : { filterFn: FILTER_FNS[filter.kind] }),
          meta: {
            ...drawn.meta,
            ...(filter?.kind === "text" ? { text: true } : {}),
            ...(filter?.kind === "range" ? { range: true } : {}),
            ...(picks?.order === undefined ? {} : { order: picks.order }),
            ...(show === undefined ? {} : { show }),
            ...(picks?.always === undefined ? {} : { always: picks.always }),
            ...(describe === undefined ? {} : { describe }),
            // A column of set width that gives Title its room may narrow below its header's words
            // on one line, so they wrap there beside the filter rather than be cut, the column no
            // narrower than they need (`wrappedHeaders`); where the column is as wide as one line,
            // they stand on one.
            ...(layout.wrappedHeaders?.includes(column.id) === true ? { wrap: true } : {}),
          },
        },
      ];
    });
    const listing = LISTING_COLUMNS.filter(({ id }) => layout.ids.includes(id)).map(
      (column): ColumnDef<Item> => ({
        ...column.rendering(priorityOf),
        id: column.id,
        size: column.size,
        minSize: layout.minimums?.[column.id] ?? column.min,
        enableResizing: true,
        enableSorting: false,
        enableColumnFilter: false,
        enableGrouping: false,
      })
    );
    if (listing.length === 0) return registered;
    // Beside the title, which is what a reader reads a row of every project's issues by.
    const after = registered.findIndex(({ id }) => id === "title") + 1;
    return [...registered.slice(0, after), ...listing, ...registered.slice(after)];
  }, [
    userState,
    compact,
    onMark,
    layout,
    priorityTitles,
    wrapTitles,
    embedded,
    noLegion,
    phases,
    phaseMode,
    setPhaseMode,
  ]);
}
