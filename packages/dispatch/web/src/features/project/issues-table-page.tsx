import type { Table } from "@tanstack/react-table";
import { type ReactNode, type RefObject, useState } from "react";
import type { IssueSummary } from "../../api/types";
import { Chip } from "../../components/Chip";
import { ColumnsMenu } from "../../components/ColumnsMenu";
import { ControlRow } from "../../components/ControlRow";
import { DisplayMenu } from "../../components/DisplayMenu";
import { PanelTrigger } from "../../components/PanelTrigger";
import { SearchBox } from "../../components/SearchBox";
import { SelectionBar } from "../../components/SelectionBar";
import { useAddressUpdate } from "../../lib/page-address";
import {
  borderDefault,
  card,
  issueStatusScale,
  surfaceBg,
  surfaceMutedBg,
  surfaceMutedHoverBg,
  textPrimaryOnCanvas,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { issueStatuses, statusLabel } from "./board-model";
import { IssueFlagChips, UnclaimedUnchecked } from "./IssueFlags";
import { useIssueFilters } from "./issue-filters";
import {
  defaultView,
  type Item,
  readUrl,
  runningIssueStatuses,
  writeUrl,
} from "./issues-table-model";

/** The panel the row's one filter trigger opens: a column of chips, portalled over the page so
 *  opening it moves no row of the table beneath (INFRA-3135, R1). */
const FLAGS_PANEL_CLASS = `z-50 flex max-h-[min(20rem,calc(100dvh-16px))] w-64 max-w-[calc(100vw-16px)] flex-col items-start gap-2 overflow-y-auto rounded-lg border p-2 text-sm font-normal shadow-lg ${card}`;

/** The project List's own small button: the steps a selection is acted on by. */
export function Action({
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

export function IssueTableStatus({
  table,
  items,
  epics,
}: {
  table: Table<Item>;
  items: readonly Item[];
  epics: readonly IssueSummary[];
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
      <fieldset
        aria-label="Issues by status"
        className={`flex min-h-11 min-w-0 overflow-x-auto rounded-md border-0 p-0 md:min-h-7 ${surfaceMutedBg}`}
      >
        {issueStatuses.map((status) => {
          const count = statusCounts.get(status) ?? 0;
          if (count === 0) return null;
          const on = statusPicks.length === 0 || statusPicks.includes(status);
          return (
            <button
              className={`min-h-11 shrink-0 px-3 py-2 text-left text-xs font-medium whitespace-nowrap md:min-h-7 md:py-1 ${issueStatusScale[status]} ${on ? "" : "opacity-30"}`}
              key={status}
              onClick={() =>
                statusColumn?.setFilterValue(
                  statusPicks.length === 1 && statusPicks[0] === status ? undefined : [status]
                )
              }
              title={`${statusLabel(status)}: ${count}`}
              // Each status takes width in proportion to its count, so the bar spans the box
              // and reads as shares; a phone keeps 44 px targets and scrolls sideways instead.
              style={{ flexGrow: count }}
              type="button"
            >
              {statusLabel(status)} {count}
            </button>
          );
        })}
      </fieldset>
    </div>
  );
}

/** Which of the row's own panels stands open, so the List keeps one at a time rather than a
 *  row of them: the filter panel behind the trigger, or the display menu at the right end. */
type ToolbarPanel = "flags" | "view";

/**
 * The one row the List's controls sit in: the search box, the trigger over the three flags that
 * narrow no column of the table, the chips of the flags that are on, **Clear filters** (which
 * the row draws), the columns menu, and the display menu carrying the grouping (INFRA-3135, R1
 * and R3). Every narrowing that is a column's own stays on that column's header, where
 * `ColumnFilter` draws it.
 */
export function IssueTableToolbar({
  searchRef,
  table,
  unclaimedUnchecked,
}: {
  /** The search box, which the selection bar hands the focus to when no row is left for it. */
  searchRef: RefObject<HTMLInputElement | null>;
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
  // The three narrowings that are no column of this table, and so the ones the row's own
  // trigger holds. Unclaimed is the Claimed by column's "Nobody or lapsed" as `?unclaimed=1`
  // spells it (`issues-table-model.ts`), which is the parameter the Board's strip writes too.
  const flagged = [
    { label: "Needs you", off: () => flags.setNeedsYou(false), on: flags.needsYou },
    { label: "Unread", off: () => flags.setUnread(false), on: flags.unread },
    { label: "Unclaimed", off: () => flags.setUnclaimed(false), on: flags.unclaimed },
  ].filter(({ on }) => on);
  // Exactly the scope it has always had: the table's own filters and search, and the two flags
  // the write below takes out of the address.
  const narrowed = filters.length > 0 || search !== "" || flags.needsYou || flags.unread;
  const chips =
    flagged.length === 0
      ? undefined
      : flagged.map(({ label, off }) => (
          <Chip
            aria-label={`Remove ${label} filter`}
            key={label}
            onClick={off}
            removable
            selected
            size="compact"
            title={label}
          >
            {label}
          </Chip>
        ));
  const clearFilters = () =>
    update((current) => {
      const next = writeUrl(current, {
        ...readUrl(current),
        // Back to the view the table opens with, not to no filter at all: an empty status pick
        // asks the server for every closed issue in the project.
        filters: defaultView.filters,
        search: "",
        page: 0,
      });
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
            size="compact"
          />
        }
        onClear={narrowed ? clearFilters : undefined}
        search={
          <SearchBox
            className="w-full"
            label="Search issues"
            onChange={(value) => setSearch(value)}
            placeholder="Search issues: key, title, epic, assignee, author, labels"
            ref={searchRef}
            size="compact"
            value={search}
          />
        }
        size="compact"
        trigger={
          <PanelTrigger
            count={flagged.length}
            label="Filters"
            onOpenChange={(open) => setOpenPanel(open ? "flags" : undefined)}
            open={openPanel === "flags"}
            panelClassName={FLAGS_PANEL_CLASS}
            size="compact"
          >
            {() => (
              <>
                <IssueFlagChips />
                <Chip
                  onClick={() => flags.setUnclaimed(!flags.unclaimed)}
                  selected={flags.unclaimed}
                >
                  Unclaimed
                </Chip>
              </>
            )}
          </PanelTrigger>
        }
      >
        <ColumnsMenu columns={table.getAllLeafColumns()} size="compact" />
      </ControlRow>
      <UnclaimedUnchecked reason={unclaimedUnchecked} />
    </>
  );
}

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
  newEpic: (keys: readonly string[]) => void;
}): ReactNode {
  const selectedKeys = table.getSelectedRowModel().flatRows.map((row) => row.original.issue.key);
  const leafCount = table.getFilteredRowModel().rows.length;
  const setSelection = table.setRowSelection;
  /** The epic the reader has picked to move the selection into; no write has been sent yet. */
  const [epic, setEpic] = useState("");
  return (
    // Over the list, not in it: selecting a row moves no row (INFRA-3135, R4).
    <SelectionBar count={selectedKeys.length} label="Selected issues" returnFocus={returnFocus}>
      {selectedKeys.length < leafCount ? (
        <Action disabled={busy} onClick={() => table.toggleAllRowsSelected(true)}>
          Select all {leafCount} matching
        </Action>
      ) : null}
      {/* A selection the reader cannot see all of — "Select all N matching" — is asked
          about first: Start next writes once per issue. */}
      <Action
        disabled={busy}
        onClick={() =>
          startNext(selectedKeys, selectedKeys.length > table.getRowModel().rows.length)
        }
      >
        Start next
      </Action>
      {/* Two steps: the select stages an epic and Move commits it. A closed `<select>`
          commits on an arrow key in Chrome on Linux and Windows, and this one reparents
          every selected issue. */}
      <select
        aria-label="Move to epic"
        className={`rounded-lg border px-2 py-1 ${borderDefault} ${surfaceBg}`}
        disabled={busy}
        onChange={(event) => setEpic(event.target.value)}
        value={epic}
      >
        <option value="">Move to epic…</option>
        {epics.map((option) => (
          <option key={option.key} value={option.key}>
            {option.title}
          </option>
        ))}
      </select>
      <Action
        disabled={busy || epic === ""}
        onClick={() => {
          const into = epic;
          setEpic("");
          void moveTo(selectedKeys, into);
        }}
      >
        Move
      </Action>
      <Action disabled={busy} onClick={() => void moveTo(selectedKeys, null)}>
        Take out of epic
      </Action>
      <Action
        disabled={busy || unansweredEpic !== undefined}
        onClick={() => void newEpic(selectedKeys)}
      >
        New epic from these
      </Action>
      <Action disabled={busy} onClick={() => setSelection({})}>
        Clear selection
      </Action>
    </SelectionBar>
  );
}
