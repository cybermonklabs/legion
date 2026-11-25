import type { Table } from "@tanstack/react-table";
import { type ReactNode, useLayoutEffect, useRef, useState } from "react";
import { flushSync } from "react-dom";
import type { IssueSummary } from "../../api/types";
import { Chip } from "../../components/Chip";
import { ColumnsMenu } from "../../components/ColumnsMenu";
import { ControlRow } from "../../components/ControlRow";
import { DisplayMenu } from "../../components/DisplayMenu";
import {
  columnFilterChip,
  columnFilterPicks,
  type FilterChip,
  filterChips,
} from "../../components/filter-chips";
import { MultiSelect } from "../../components/MultiSelect";
import { PanelTrigger } from "../../components/PanelTrigger";
import { SearchBox } from "../../components/SearchBox";
import { SelectionBar } from "../../components/SelectionBar";
import { StepButton } from "../../components/StepButton";
import { useAddressUpdate } from "../../lib/page-address";
import {
  borderDefault,
  issueStatusScale,
  surfaceBg,
  surfaceMutedBg,
  textPrimaryOnCanvas,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { issueStatuses, statusLabel } from "./board-model";
import { IssueFlagChips, UnclaimedUnchecked } from "./IssueFlags";
import { useIssueFilters } from "./issue-filters";
import {
  defaultView,
  type Item,
  isDefaultStatus,
  readUrl,
  runningIssueStatuses,
  UNCLAIMED_FACET,
  writeUrl,
} from "./issues-table-model";

/** The type of a status segment, with its name or with its count alone. */
const SEGMENT_TYPE = "text-xs font-medium whitespace-nowrap";

/** What sets a status segment's width with its name, which the bar's hidden copy of its segments
 *  draws them with too (`IssueTableStatus`), so the copy is as wide as the segments it stands
 *  for. */
const SEGMENT = `px-3 ${SEGMENT_TYPE}`;

/** The padding and type of a segment that draws its count alone (`IssueTableStatus`'s
 *  `legend`): on a phone the padding its count needs, and from `md` up a named segment's. */
const COUNT_SEGMENT = `px-2 ${SEGMENT_TYPE} md:px-3`;

/** How narrow a segment that draws its count alone may be: 24 px, WCAG 2.5.8's least target
 *  width, in place of the 44 px floor `styles.css` gives every button below 1280 px, so the eight
 *  segments of a priority's work fit its 316 px bar at 390 px and none scrolls past its edge. Its
 *  height keeps that floor. Set as an inline style, since the floor stands in no cascade layer
 *  and outranks every utility class; from `md` up each count with its padding is wider anyway. */
const COUNT_SEGMENT_MIN = 24;

export function IssueTableStatus({
  table,
  items,
  epics,
  legend = false,
}: {
  table: Table<Item>;
  items: readonly Item[];
  epics: readonly IssueSummary[];
  /** Whether the bar draws each segment's count alone where the segments with their names are
   *  wider than it, and the names in a legend under it (`IssuesTable`'s `statusLegend`). Unset,
   *  each segment draws its name and its count, and the bar scrolls sideways where they are
   *  wider than it, as a project's does on a phone. */
  legend?: boolean;
}): ReactNode {
  const statusColumn = table.getColumn("status");
  const statusPicks = (statusColumn?.getFilterValue() as string[] | undefined) ?? [];
  const statusCounts = new Map<string, number>();
  let queuedCount = 0;
  let runningCount = 0;
  for (const item of items) {
    statusCounts.set(item.issue.status, (statusCounts.get(item.issue.status) ?? 0) + 1);
    if (item.queue !== undefined) queuedCount += 1;
    if ((runningIssueStatuses as readonly string[]).includes(item.issue.status)) runningCount += 1;
  }
  const shown = issueStatuses.filter((status) => (statusCounts.get(status) ?? 0) > 0);
  const barRef = useRef<HTMLFieldSetElement>(null);
  const namesRef = useRef<HTMLDivElement>(null);
  const [namesOverflow, setNamesOverflow] = useState(false);
  // Whether the segments with their names are wider than the bar, read off a copy of them that
  // is never drawn rather than off the bar itself: the copy is the same whichever way the bar
  // draws its segments, so what the switch does to the bar cannot switch it back. Read before
  // the first paint, and again in the frame the bar's width or the copy's (a count that
  // changed) does, before that frame is painted.
  useLayoutEffect(() => {
    const bar = barRef.current;
    const names = namesRef.current;
    if (!legend || bar === null || names === null) return;
    const measure = () =>
      setNamesOverflow(names.getBoundingClientRect().width > bar.getBoundingClientRect().width);
    measure();
    if (typeof ResizeObserver === "undefined") return;
    const sizes = new ResizeObserver(() => flushSync(measure));
    sizes.observe(bar);
    sizes.observe(names);
    return () => sizes.disconnect();
  }, [legend]);
  const countsOnly = legend && namesOverflow;
  return (
    <div className={`rounded-xl border p-3 ${borderDefault}`}>
      <div
        className={`mb-2 flex flex-wrap items-baseline gap-x-5 gap-y-1 text-sm ${textSecondaryOnCanvas}`}
      >
        <span>
          <b className={`text-lg tabular-nums ${textPrimaryOnCanvas}`}>{queuedCount}</b> in Legion's
          queue
        </span>
        <span>
          <b className={`text-lg tabular-nums ${textPrimaryOnCanvas}`}>{runningCount}</b> running
        </span>
        <span>
          <b className={`text-lg tabular-nums ${textPrimaryOnCanvas}`}>{epics.length}</b>{" "}
          {epics.length === 1 ? "epic" : "epics"}
        </span>
      </div>
      {legend ? (
        // The copy the bar is measured against: the segments with their names, each at its
        // natural width, in a strip that takes no height and clips them, so nothing of it is
        // drawn and the page grows no wider. It stands before the bar, so nothing a reader
        // reads under the bar holds it.
        <div aria-hidden className="h-0 overflow-hidden">
          <div className="invisible flex w-max" ref={namesRef}>
            {shown.map((status) => (
              <span className={SEGMENT} key={status}>
                {statusLabel(status)} {statusCounts.get(status)}
              </span>
            ))}
          </div>
        </div>
      ) : null}
      <fieldset
        aria-label="Issues by status"
        className={`flex min-h-11 min-w-0 overflow-x-auto rounded-md border-0 p-0 md:min-h-7 ${surfaceMutedBg}`}
        ref={barRef}
      >
        {shown.map((status) => {
          const count = statusCounts.get(status) ?? 0;
          const on = statusPicks.length === 0 || statusPicks.includes(status);
          return (
            <button
              // A count alone is named by its status, as its segment is with the name drawn.
              aria-label={countsOnly ? `${statusLabel(status)} ${count}` : undefined}
              className={`min-h-11 shrink-0 py-2 text-left md:min-h-7 md:py-1 ${countsOnly ? COUNT_SEGMENT : SEGMENT} ${issueStatusScale[status]} ${on ? "" : "opacity-30"}`}
              key={status}
              onClick={() =>
                statusColumn?.setFilterValue(
                  statusPicks.length === 1 && statusPicks[0] === status ? undefined : [status]
                )
              }
              title={`${statusLabel(status)}: ${count}`}
              // Each status takes width in proportion to its count, so the bar spans the box
              // and reads as shares; a phone keeps 44 px targets and scrolls sideways instead,
              // unless the table draws the counts alone with their names in a legend (`legend`),
              // each segment as narrow as its count and 24 px (`COUNT_SEGMENT_MIN`).
              style={{ flexGrow: count, minWidth: countsOnly ? COUNT_SEGMENT_MIN : undefined }}
              type="button"
            >
              {countsOnly ? (
                count
              ) : (
                <>
                  {statusLabel(status)} {count}
                </>
              )}
            </button>
          );
        })}
      </fieldset>
      {countsOnly ? (
        // The names the segments no longer draw, in the bar's order, each beside a swatch of its
        // status's colour, wrapping onto as many lines as the box needs. Each segment keeps its
        // name as its accessible name and its tooltip, so the legend is for the eye alone.
        <ul
          aria-hidden
          className={`mt-2 flex flex-wrap gap-x-4 gap-y-1 text-xs ${textSecondaryOnCanvas}`}
        >
          {shown.map((status) => (
            <li className="flex items-center gap-1.5" key={status}>
              <span
                className={`size-3 shrink-0 rounded-sm border ${borderDefault} ${issueStatusScale[status]}`}
              />
              {statusLabel(status)}
            </li>
          ))}
        </ul>
      ) : null}
    </div>
  );
}

/** Which of the row's own panels stands open, so the List keeps one at a time rather than a
 *  row of them: the filter panel behind the trigger, or the display menu at the right end. */
type ToolbarPanel = "flags" | "view";

/**
 * The one row the List's controls sit in: the search box, the trigger over the three flags that
 * narrow no column of the table, the chips of every narrowing that is on - the issues a link
 * names (`only=`), the flags, and every column's filter - **Clear filters** (which the row
 * draws), the columns menu, and the display menu carrying the grouping.
 * A column's filter is set on that column's header, where `ColumnFilter` draws it, and its chip
 * is drawn here as well, so a column the reader hid keeps its narrowing in sight.
 */
export function IssueTableToolbar({
  keyScope,
  selection,
  stateKey,
  table,
  unclaimedUnchecked,
}: {
  /** Where the row's Shift+F and Shift+V are registered (`ControlRow`): the project scope for
   *  the page's one table, or its main one, and none for a table drawn beside it. */
  keyScope?: "project";
  /** The chip of the issues a link names, which the row draws first among its chips. */
  selection?: FilterChip;
  /** The prefix this table's view is written under (`useTableView`), which **Clear filters**
   *  rewrites; unset is the bare one the project page publishes. */
  stateKey?: string;
  table: Table<Item>;
  /** Why the Claimed by filter cannot tell which claims have lapsed, while it is asked to. */
  unclaimedUnchecked: "refilling" | "unreachable" | undefined;
}): ReactNode {
  const flags = useIssueFilters();
  const update = useAddressUpdate();
  const [openPanel, setOpenPanel] = useState<ToolbarPanel | undefined>(undefined);
  const { globalFilter: search, columnFilters: filters, grouping } = table.getState();
  const setSearch = table.setGlobalFilter;
  const setGrouping = table.setGrouping;
  const setExpanded = table.setExpanded;
  // Unclaimed is the Claimed by column's "Nobody or lapsed", which this table writes as
  // `?unclaimed=1` under its own prefix (`issues-table-model.ts`) - the parameter the Board's
  // control row writes too - so the flag is read from and written to that column, and a table drawn
  // under a prefix narrows itself rather than the page's bare address.
  const claim = table.getColumn("claim");
  const claimPicks = (claim?.getFilterValue() as string[] | undefined) ?? [];
  const unclaimed = claimPicks.includes(UNCLAIMED_FACET);
  const setUnclaimed = (on: boolean) => {
    const rest = claimPicks.filter((value) => value !== UNCLAIMED_FACET);
    const next = on ? [...rest, UNCLAIMED_FACET] : rest;
    claim?.setFilterValue(next.length === 0 ? undefined : next);
  };
  // The three narrowings that are no column of this table, and so the ones the row's own
  // trigger holds.
  const flagged = [
    { label: "Needs you", on: flags.needsYou, remove: () => flags.setNeedsYou(false) },
    { label: "Unread", on: flags.unread, remove: () => flags.setUnread(false) },
    { label: "Unclaimed", on: unclaimed, remove: () => setUnclaimed(false) },
  ].filter(({ on }) => on);
  // Every column filter that is on, a chip of the row whether its column is shown or hidden:
  // a column the reader hid with the columns menu keeps its filter, and its chip is then the one
  // place it shows. Three columns are the List's own. Status opens on the open statuses, which a
  // link leaves out and Clear filters goes back to, so that pick is no chip, and taking a Status
  // chip off goes back to it rather than asking for every closed issue in the project. Claimed
  // by's "Nobody or lapsed" is the Unclaimed flag's chip already, so the column's chip names its
  // other picks, and taking it off leaves that one on. Labels is the project's own `?label=`,
  // which the Board reads too (`useIssueFilters`), so one of its values is one chip under the
  // name both views give it, `Label: <value>`, rather than a second chip in other words for the
  // narrowing the Board already names.
  const columned = table.getAllLeafColumns().flatMap((column) => {
    const picks = columnFilterPicks(column.getFilterValue());
    if (column.id === "status") {
      return picks.length === 0 || isDefaultStatus(picks)
        ? []
        : [
            columnFilterChip(column, picks, () =>
              column.setFilterValue(defaultView.filters[0]?.value)
            ),
          ];
    }
    if (column.id === "claim") {
      const named = picks.filter((value) => value !== UNCLAIMED_FACET);
      return named.length === 0
        ? []
        : [
            columnFilterChip(column, named, () =>
              column.setFilterValue(unclaimed ? [UNCLAIMED_FACET] : undefined)
            ),
          ];
    }
    if (column.id === "labels") {
      return picks.map((value) => ({
        key: `label:${value}`,
        label: `Label: ${value}`,
        remove: () => {
          const kept = picks.filter((one) => one !== value);
          column.setFilterValue(kept.length === 0 ? undefined : kept);
        },
      }));
    }
    return picks.length === 0 ? [] : [columnFilterChip(column, picks)];
  });
  // Exactly the scope it has always had: the table's own filters and search, and the two flags
  // the write below takes out of the address.
  const narrowed = filters.length > 0 || search !== "" || flags.needsYou || flags.unread;
  const chips = filterChips(
    [...(selection === undefined ? [] : [selection]), ...flagged, ...columned],
    "toolbar"
  );
  const clearFilters = () =>
    update((current) => {
      const next = writeUrl(
        current,
        {
          ...readUrl(current, stateKey),
          // Back to the view the table opens with, not to no filter at all: an empty status pick
          // asks the server for every closed issue in the project.
          filters: defaultView.filters,
          search: "",
          page: 0,
        },
        stateKey
      );
      for (const flag of ["needs-you", "unread"]) next.delete(flag);
      return next;
    });
  return (
    <>
      <ControlRow
        chips={chips}
        display={
          <DisplayMenu
            label="View"
            onOpenChange={(open) => setOpenPanel(open ? "view" : undefined)}
            open={openPanel === "view"}
            sections={[
              {
                id: "group",
                label: "Group by",
                onChange: (next) => {
                  setGrouping(next ? [next] : []);
                  setExpanded(true);
                },
                options: [
                  { label: "Nothing", value: "" },
                  ...table
                    .getAllLeafColumns()
                    .filter((column) => column.getCanGroup())
                    .map((column) => ({
                      label: String(column.columnDef.header),
                      value: column.id,
                    })),
                ],
                value: grouping[0] ?? "",
              },
            ]}
            size="toolbar"
          />
        }
        keyScope={keyScope}
        onClear={narrowed ? clearFilters : undefined}
        search={
          <SearchBox
            className="w-full"
            label="Search issues"
            onChange={(value) => setSearch(value)}
            placeholder="Search issues: key, title, epic, assignee, author, labels"
            size="toolbar"
            value={search}
          />
        }
        searchLine
        size="toolbar"
        trigger={
          <PanelTrigger
            count={flagged.length + columned.length}
            label="Filters"
            onOpenChange={(open) => setOpenPanel(open ? "flags" : undefined)}
            open={openPanel === "flags"}
            size="toolbar"
            stacked
          >
            {() => (
              <>
                <IssueFlagChips size="compact" />
                <Chip onClick={() => setUnclaimed(!unclaimed)} selected={unclaimed} size="compact">
                  Unclaimed
                </Chip>
              </>
            )}
          </PanelTrigger>
        }
      >
        <ColumnsMenu columns={table.getAllLeafColumns()} size="toolbar" />
      </ControlRow>
      <UnclaimedUnchecked reason={unclaimedUnchecked} />
    </>
  );
}

/**
 * The bar over the List's marked rows. While a table action is out (`busy`, which holds until
 * the list read again after its writes has drawn what they changed, and lets go while every write
 * the action waits on is past its deadline) every action is held rather than disabled - each
 * `StepButton`, and the Move to epic list through its `ariaDisabled` - so the press that makes a
 * control inert keeps the focus on it, and a confirm or a name dialog
 * hands the focus back to an opener that can take it. A held Move to epic opens no list and
 * stages no pick; a pick stages the epic, and Move, held until one is staged, writes it. The bar
 * and its steps take the List's compact size, as its control row does.
 */
export function IssueTableSelection({
  returnFocus,
  table,
  epics,
  busy,
  unansweredEpic,
  startNext,
  moveTo,
  newEpic,
}: {
  /** Where the focus goes when the bar goes with it (`SelectionBar`), which the List names. */
  returnFocus: () => HTMLElement | null;
  table: Table<Item>;
  epics: readonly IssueSummary[];
  busy: boolean;
  unansweredEpic: string | undefined;
  startNext: (keys: readonly string[], confirmFirst?: boolean) => void;
  moveTo: (keys: readonly string[], epic: string | null) => void;
  /** How an epic is made of the selected issues; absent on a table that stands on no project,
   *  since an epic is created in one and a cross-project selection names none. */
  newEpic?: (keys: readonly string[]) => void;
}): ReactNode {
  const selectedKeys = table.getSelectedRowModel().flatRows.map((row) => row.original.issue.key);
  const leafCount = table.getFilteredRowModel().rows.length;
  const setSelection = table.setRowSelection;
  /** The epic the reader has picked to move the selection into; no write has been sent yet. */
  const [epic, setEpic] = useState("");
  const [picking, setPicking] = useState(false);
  const epicTitles = new Map(epics.map((option) => [option.key, option.title]));
  return (
    // Over the list, not in it: selecting a row moves no row.
    <SelectionBar
      count={selectedKeys.length}
      label="Selected issues"
      returnFocus={returnFocus}
      size="compact"
    >
      {selectedKeys.length < leafCount ? (
        <StepButton
          ariaDisabled={busy}
          onClick={() => table.toggleAllRowsSelected(true)}
          size="compact"
        >
          Select all {leafCount} matching
        </StepButton>
      ) : null}
      {/* A selection the reader cannot see all of — "Select all N matching" — is asked
          about first: Start next writes once per issue. */}
      <StepButton
        ariaDisabled={busy}
        onClick={() =>
          startNext(selectedKeys, selectedKeys.length > table.getRowModel().rows.length)
        }
        size="compact"
      >
        Start next
      </StepButton>
      {/* Two steps, since a Move reparents every selected issue: the list stages an epic and
          Move commits it, so no press or key in the list writes anything. While a press is out
          the list is held as a `StepButton` is, and for the same reason: it opens on nothing,
          and a pick stages nothing. */}
      <MultiSelect
        ariaDisabled={busy}
        emptyMessage={epics.length === 0 ? "No epics yet." : "No epic matches that."}
        label="Move to epic"
        mode="single"
        onOpenChange={(next) => setPicking(next && !busy)}
        onPick={(picked) => {
          if (!busy) setEpic(picked);
        }}
        open={picking}
        optionLabel={(key) => epicTitles.get(key)}
        options={epics.map((option) => option.key)}
        searchLabel="Search epics"
        size="compact"
        triggerClassName={`max-w-60 rounded-lg border text-left refused:opacity-40 ${borderDefault} ${surfaceBg}`}
        value={epic === "" ? undefined : epic}
      />
      <StepButton
        ariaDisabled={busy || epic === ""}
        onClick={() => {
          const into = epic;
          setEpic("");
          void moveTo(selectedKeys, into);
        }}
        size="compact"
      >
        Move
      </StepButton>
      <StepButton
        ariaDisabled={busy}
        onClick={() => void moveTo(selectedKeys, null)}
        size="compact"
      >
        Take out of epic
      </StepButton>
      {newEpic === undefined ? null : (
        <StepButton
          ariaDisabled={busy || unansweredEpic !== undefined}
          onClick={() => void newEpic(selectedKeys)}
          size="compact"
        >
          New epic from these
        </StepButton>
      )}
      <StepButton ariaDisabled={busy} onClick={() => setSelection({})} size="compact">
        Clear selection
      </StepButton>
    </SelectionBar>
  );
}
