import type { Table } from "@tanstack/react-table";
import { type ReactNode, useLayoutEffect, useRef, useState } from "react";
import { flushSync } from "react-dom";
import { Chip } from "../../components/Chip";
import { ColumnsMenu } from "../../components/ColumnsMenu";
import { ControlRow } from "../../components/ControlRow";
import { DisplayMenu } from "../../components/DisplayMenu";
import { type FilterChip, filterChips } from "../../components/filter-chips";
import { PanelTrigger } from "../../components/PanelTrigger";
import { SearchBox } from "../../components/SearchBox";
import { SelectionBar } from "../../components/SelectionBar";
import { StepButton } from "../../components/StepButton";
import {
  borderDefault,
  issueStatusScale,
  surfaceMutedBg,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { statusLabel } from "./board-model";
import { IssueFlagChips, NarrowingList, UnclaimedUnchecked } from "./IssueFlags";
import { useIssueFilters } from "./issue-filters";
import { countStatuses, statusSegments } from "./issues-table-bar";
import { type OwnNarrowings, viewChips } from "./issues-table-chips";
import { COLUMNS, type Item, UNCLAIMED_FACET } from "./issues-table-model";

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

/**
 * A priority page's status strip: every status of the rows the table holds, each as wide as its
 * count, a press showing only that status and a second press every status. It draws no other
 * count (R13).
 */
export function IssueTableStatus({
  table,
  items,
  legend = false,
}: {
  table: Table<Item>;
  items: readonly Item[];
  /** Whether the bar draws each segment's count alone where the segments with their names are
   *  wider than it, and the names in a legend under it (`ListingTable`'s `statusLegend`). Unset,
   *  each segment draws its name and its count, and the bar scrolls sideways where they are
   *  wider than it, as a project's does on a phone. */
  legend?: boolean;
}): ReactNode {
  const statusColumn = table.getColumn("status");
  const statusPicks = (statusColumn?.getFilterValue() as string[] | undefined) ?? [];
  const segments = statusSegments(countStatuses(items.map((item) => item.issue.status)));
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
      {legend ? (
        // The copy the bar is measured against: the segments with their names, each at its
        // natural width, in a strip that takes no height and clips them, so nothing of it is
        // drawn and the page grows no wider. It stands before the bar, so nothing a reader
        // reads under the bar holds it.
        <div aria-hidden className="h-0 overflow-hidden">
          <div className="invisible flex w-max" ref={namesRef}>
            {segments.map(({ count, status }) => (
              <span className={SEGMENT} key={status}>
                {statusLabel(status)} {count}
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
        {segments.map(({ count, status }) => {
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
          {segments.map(({ status }) => (
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
 * narrow no column of the table, the chips of every narrowing that differs from the view's own -
 * the issues a link names (`only=`) first, then the chips of `viewChips`, which the Board draws
 * too - **Clear filters** where the page gives it (`onClear`), the page's own controls (the
 * columns menu where the page gives none), and the display menu, named Group by for the grouping
 * it holds. A column's filter is set on that column's header, where `ColumnFilter` draws it, and
 * its chip is drawn here as well, whether its column is shown or hidden, so a column the reader
 * hid keeps its narrowing in sight.
 */
export function IssueTableToolbar({
  controls,
  narrowings,
  onClear,
  own,
  selection,
  table,
  unclaimedUnchecked,
}: {
  /** The page's own controls, in the place of the columns menu the row draws by itself. */
  controls?: ReactNode;
  /** The narrowings an address carries that are no column and no flag (`exclude_tree`): each a
   *  chip of the row and a line of the Filters panel, with its remove. */
  narrowings: readonly FilterChip[];
  /** **Clear filters**, which the row draws while the page gives it: the page leaves it unset
   *  while nothing is narrowed. */
  onClear: (() => void) | undefined;
  /** What the open view narrows by of itself, which draws no chip and which a chip's press goes
   *  back to (`viewChips`). */
  own: OwnNarrowings;
  /** The chip of the issues a link names, which the row draws first among its chips. */
  selection?: FilterChip;
  table: Table<Item>;
  /** Why the Claimed by filter cannot tell which claims have lapsed, while it is asked to. */
  unclaimedUnchecked: "refilling" | "unreachable" | undefined;
}): ReactNode {
  const flags = useIssueFilters();
  const [openPanel, setOpenPanel] = useState<ToolbarPanel | undefined>(undefined);
  const { globalFilter: search, grouping } = table.getState();
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
  const viewed = viewChips({ columns: table.getAllLeafColumns(), flags, narrowings, own });
  const chips = filterChips(
    [...(selection === undefined ? [] : [selection]), ...viewed],
    "toolbar"
  );
  return (
    <>
      <ControlRow
        chips={chips}
        display={
          <DisplayMenu
            label="Group by"
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
                  // In the registry's order, whatever order the columns are drawn in.
                  ...table
                    .getAllLeafColumns()
                    .filter((column) => column.getCanGroup())
                    .map((column) => ({
                      label: String(column.columnDef.header),
                      place: COLUMNS.findIndex(({ id }) => id === column.id),
                      value: column.id,
                    }))
                    .sort((a, b) => a.place - b.place)
                    .map(({ label, value }) => ({ label, value })),
                ],
                value: grouping[0] ?? "",
              },
            ]}
            size="toolbar"
          />
        }
        onClear={onClear}
        search={
          <SearchBox
            className="w-full"
            label="Search issues"
            onChange={(value) => setSearch(value)}
            placeholder="Search issues: key, title, assignee, author, labels"
            size="toolbar"
            value={search}
          />
        }
        searchLine
        size="toolbar"
        trigger={
          <PanelTrigger
            count={viewed.length}
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
                <NarrowingList narrowings={narrowings} />
              </>
            )}
          </PanelTrigger>
        }
      >
        {controls ?? <ColumnsMenu columns={table.getAllLeafColumns()} size="toolbar" />}
      </ControlRow>
      <UnclaimedUnchecked reason={unclaimedUnchecked} />
    </>
  );
}

/**
 * The bar over the List's marked rows. While a table action is out (`busy`, which holds until
 * the list read again after its writes has drawn what they changed) every action is held rather
 * than disabled - each `StepButton` through its `ariaDisabled` - so the press that makes a
 * control inert keeps the focus on it, and Start next's confirm hands the focus back to an
 * opener that can take it. The bar and its steps take the List's compact size, as its control
 * row does.
 */
export function IssueTableSelection({
  returnFocus,
  table,
  busy,
  startNext,
}: {
  /** Where the focus goes when the bar goes with it (`SelectionBar`), which the List names. */
  returnFocus: () => HTMLElement | null;
  table: Table<Item>;
  busy: boolean;
  startNext: (keys: readonly string[], confirmFirst?: boolean) => void;
}): ReactNode {
  const selectedKeys = table.getSelectedRowModel().flatRows.map((row) => row.original.issue.key);
  const leafCount = table.getFilteredRowModel().rows.length;
  const setSelection = table.setRowSelection;
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
      <StepButton ariaDisabled={busy} onClick={() => setSelection({})} size="compact">
        Clear selection
      </StepButton>
    </SelectionBar>
  );
}
