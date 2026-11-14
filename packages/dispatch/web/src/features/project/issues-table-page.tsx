import type { Column, Table } from "@tanstack/react-table";
import { type ReactNode, type RefObject, useState } from "react";
import type { IssueSummary } from "../../api/types";
import { Chip } from "../../components/Chip";
import { ControlRow } from "../../components/ControlRow";
import { DisplayMenu } from "../../components/DisplayMenu";
import { PanelTrigger } from "../../components/PanelTrigger";
import { SearchBox } from "../../components/SearchBox";
import { SelectionBar } from "../../components/SelectionBar";
import { borderDefault, card, surfaceBg, surfaceMutedHoverBg } from "../../theme/classes";
import { IssueFlagChips, UnclaimedUnchecked } from "./IssueFlags";
import { useIssueFilters } from "./issue-filters";
import type { Item } from "./issues-table-model";

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

/** Which of the row's own panels stands open, so the List keeps one at a time rather than a
 *  row of them: the filter panel behind the trigger, or the display menu at the right end. */
type ToolbarPanel = "flags" | "display";

/** A narrowing an address carries that is no column's filter: a chip with a remove button. */
export interface ExtraChip {
  readonly label: string;
  readonly remove: () => void;
}

/**
 * The one row the view's controls sit in, for the table and the Board alike: the search box, the
 * trigger over the three flags that narrow no column, the chips of the flags and of the
 * narrowings an address carries (`exclude_tree`, `only`), **Clear filters** (which the row
 * draws), the page's own controls, and the display menu carrying the grouping (INFRA-3135, R1
 * and R3). Every narrowing that is a column's own stays on that column's header.
 */
export function IssueTableToolbar({
  columns,
  extraChips,
  onClear,
  searchRef,
  table,
  trailing,
  unclaimedUnchecked,
}: {
  /** The columns the Columns menu offers: those the project has. */
  columns: Column<Item>[];
  extraChips: readonly ExtraChip[];
  /** Takes every narrowing off, back to the view as it opens; absent while nothing narrows. */
  onClear: (() => void) | undefined;
  /** The search box, which the selection bar hands the focus to when no row is left for it. */
  searchRef: RefObject<HTMLInputElement | null>;
  table: Table<Item>;
  /** The page's own controls: the Columns menu for the table, Show Icebox and the lanes for the
   *  Board. */
  trailing: ReactNode;
  /** Why the Claimed by filter cannot tell which claims have lapsed, while it is asked to. */
  unclaimedUnchecked: "refilling" | "unreachable" | undefined;
}): ReactNode {
  const flags = useIssueFilters();
  const [openPanel, setOpenPanel] = useState<ToolbarPanel | undefined>(undefined);
  const { globalFilter: search, grouping } = table.getState();
  // The three narrowings that are no column of this table, and so the ones the row's own
  // trigger holds. Unclaimed is the Claimed by column's "Nobody or lapsed" as `?unclaimed=1`
  // spells it (`issues-table-model.ts`).
  const flagged = [
    { label: "Needs you", off: () => flags.setNeedsYou(false), on: flags.needsYou },
    { label: "Unread", off: () => flags.setUnread(false), on: flags.unread },
    { label: "Unclaimed", off: () => flags.setUnclaimed(false), on: flags.unclaimed },
  ].filter(({ on }) => on);
  const chipped = [...flagged.map(({ label, off }) => ({ label, remove: off })), ...extraChips];
  const chips =
    chipped.length === 0
      ? undefined
      : chipped.map(({ label, remove }) => (
          <Chip
            aria-label={`Remove ${label} filter`}
            key={label}
            onClick={remove}
            removable
            selected
            size="compact"
            title={label}
          >
            {label}
          </Chip>
        ));
  return (
    <>
      <ControlRow
        chips={chips}
        display={
          <DisplayMenu
            // "View" names the views in the tab row on this page, so the menu that holds the
            // grouping takes another word.
            label="Display"
            onOpenChange={(open) => setOpenPanel(open ? "display" : undefined)}
            open={openPanel === "display"}
            sections={[
              {
                id: "group",
                label: "Group by",
                onChange: (next) => {
                  table.setGrouping(next ? [next] : []);
                  table.setExpanded(true);
                },
                options: [
                  { label: "Nothing", value: "" },
                  ...columns
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
        onClear={onClear}
        search={
          <SearchBox
            className="w-full"
            label="Search issues"
            onChange={(value) => table.setGlobalFilter(value)}
            placeholder="Search: key, title, epic, people, labels"
            ref={searchRef}
            size="compact"
            value={search}
          />
        }
        size="compact"
        trigger={
          <PanelTrigger
            count={flagged.length === 0 ? undefined : flagged.length}
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
        {trailing}
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
