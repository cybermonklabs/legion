import type { Row } from "@tanstack/react-table";
import { type ReactNode, useState } from "react";
import { Link } from "react-router-dom";
import type { IssueSummary } from "../../api/types";
import { DataTableCells, DataTableGroupRow } from "../../components/DataTable";
import { heldPress } from "../../components/held-press";
import { MultiSelect } from "../../components/MultiSelect";
import { PanelTrigger } from "../../components/PanelTrigger";
import { useNestedPanels } from "../../components/useNestedPanels";
import {
  borderDefault,
  focusVisibleRing,
  issueStatusScale,
  linkHoverText,
  linkText,
  selectedCardBg,
  surfaceBg,
  surfaceMutedBg,
  surfaceMutedHoverBg,
  textMutedOnCanvas,
  textMutedOnSurfaceMuted,
  textPrimaryOnCanvas,
} from "../../theme/classes";
import { buildIssuePath } from "../refs/routes";
import { issueStatuses, statusLabel } from "./board-model";
import { columnFilterSpec, type Item } from "./issues-table-model";

/** One issue's row: its cells, and the Start next the reader reaches by hovering it, which is
 *  held while a table action is out (`busy`, `heldPress`) as a `StepButton` is, and for the same
 *  reason: from its own press until the list read again moves its issue up the queue and takes
 *  it away, so it keeps the focus while its write is out and its going is the press's doing. */
export function IssueRow({
  busy,
  index,
  measure,
  moved,
  onStartNext,
  row,
}: {
  busy: boolean;
  index: number;
  measure: (element: Element | null) => void;
  /** The issues this action has just changed, which the row shows until the next one. */
  moved: ReadonlySet<string>;
  /** Starts the row's issue next: absent on a table that draws no row's actions - its listing
   *  holds no issue in Legion's queue (`listsQueued`), or its layout leaves them out
   *  (`rowActions`, `IssuesTable`) - which draws no cell for it. */
  onStartNext: ((key: string) => void) | undefined;
  row: Row<Item>;
}): ReactNode {
  const { issue, queue } = row.original;
  const changed = moved.has(issue.key);
  return (
    <tr
      aria-label={`${issue.key} ${issue.title}`}
      className={`group align-top outline-none focus-visible:ring-2 ${focusVisibleRing} ${changed ? selectedCardBg : ""}`}
      data-index={index}
      data-issue-group={row.getParentRow()?.id ?? "ungrouped"}
      data-issue-row={issue.key}
      data-moved={changed || undefined}
      ref={measure}
      tabIndex={-1}
    >
      <DataTableCells
        cellClass={(cell) => (row.depth > 0 && cell.column.id === "select" ? "pl-6" : "")}
        highlight={changed ? selectedCardBg : undefined}
        row={row}
        // A table that offers no Start next (`onStartNext`) draws no cell after its columns.
        trailing={
          onStartNext === undefined ? undefined : queue !== undefined && queue > 1 ? (
            <button
              className={`rounded-md border px-2 py-0.5 text-xs whitespace-nowrap opacity-0 group-hover:opacity-100 focus:opacity-100 ${borderDefault} ${surfaceBg}`}
              {...heldPress(busy, () => onStartNext(issue.key))}
              type="button"
            >
              Start next
            </button>
          ) : null
        }
      />
    </tr>
  );
}

/** A group's header row: its value, how many issues, its issues by status, and for an epic its
 *  actions, whose trigger is held while a table action is out (`busy`). */
export function GroupRow({
  busy,
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
  shown,
  viewportWidth,
}: {
  busy: boolean;
  /** A repeat of the header of a group the page opens in the middle of. */
  continued?: boolean;
  /** How many of the group's rows this page holds, when that is fewer than the group has. */
  shown?: number;
  index: number;
  measure: (element: Element | null) => void;
  row: Row<Item>;
  grouping: string;
  columns: number;
  viewportWidth: number;
  epics: readonly IssueSummary[];
  onRename: (key: string) => void;
  /** How an epic is merged into another; absent on a table that stands on no project, where
   *  the issues of a group come from several and no epic can take them all. */
  onMerge?: (from: string, into: string) => void;
  onStartAll: (epic: string) => void;
}): ReactNode {
  const leaves = row.getLeafRows().filter((leaf) => !leaf.getIsGrouped());
  const value = String(row.groupingValue ?? "");
  // A heading reads as the column's filter does: the registry holds that text once.
  const groupedBy = columnFilterSpec(grouping);
  const heading = groupedBy?.kind === "picks" && groupedBy.show ? groupedBy.show(value) : value;
  // What the reader can see under this heading, which a page boundary can make fewer than the
  // group holds: "3 of 5 issues" rather than a count of rows that are not on this page.
  const here = shown ?? leaves.length;
  const count =
    here === leaves.length
      ? `${leaves.length} ${leaves.length === 1 ? "issue" : "issues"}`
      : `${here} of ${leaves.length} issues`;
  const epicKey = grouping === "epic" ? leaves[0]?.original.epicKey : undefined;
  const leafCounts = new Map<string, number>();
  let next = Number.POSITIVE_INFINITY;
  for (const leaf of leaves) {
    const { issue, queue } = leaf.original;
    leafCounts.set(issue.status, (leafCounts.get(issue.status) ?? 0) + 1);
    if (queue !== undefined && queue < next) next = queue;
  }
  const byStatus = issueStatuses
    .map((status) => [status, leafCounts.get(status) ?? 0] as const)
    .filter(([, count]) => count > 0);
  return (
    <DataTableGroupRow
      columns={columns}
      expanded={row.getIsExpanded()}
      heading={
        <>
          {epicKey === undefined ? heading : null}
          {continued ? (
            <span className={`font-normal ${textMutedOnSurfaceMuted}`}>(continued)</span>
          ) : null}
          <span className={`font-normal ${textMutedOnSurfaceMuted}`}>
            {count}
            {Number.isFinite(next) ? ` · next in queue #${next}` : ""}
          </span>
        </>
      }
      index={index}
      label={`${row.getIsExpanded() ? "Collapse" : "Expand"} ${heading}, ${count}${continued ? ", continued" : ""}`}
      measure={measure}
      onToggle={row.getToggleExpandedHandler()}
      slot={
        <>
          {epicKey === undefined ? null : (
            <Link
              className={`font-semibold ${linkText} ${linkHoverText}`}
              to={buildIssuePath({ key: epicKey, kind: "issue" })}
            >
              {heading}
            </Link>
          )}
          <span className="flex items-center gap-2">
            <span
              aria-hidden
              className={`flex h-2 w-32 overflow-hidden rounded-full ${surfaceMutedBg}`}
            >
              {byStatus.map(([status, count]) => (
                <span className={issueStatusScale[status]} key={status} style={{ flex: count }} />
              ))}
            </span>
            <span className={`text-xs ${textMutedOnSurfaceMuted}`}>
              {byStatus
                .map(([status, count]) => `${count} ${statusLabel(status).toLowerCase()}`)
                .join(" · ")}
            </span>
          </span>
          {epicKey === undefined ? null : (
            <EpicActions
              busy={busy}
              epicKey={epicKey}
              epics={epics}
              onMerge={onMerge === undefined ? undefined : (into) => onMerge(epicKey, into)}
              onRename={() => onRename(epicKey)}
              onStartAll={() => onStartAll(epicKey)}
            />
          )}
        </>
      }
      width={Math.max(180, viewportWidth - 20)}
    />
  );
}

/** An epic's actions, behind one trigger. While a table action is out (`busy`) the trigger is
 *  held (`PanelTrigger`'s `ariaDisabled`): it keeps the focus the closed panel or the merge's
 *  confirmation handed back to it until the group row goes with it - the merge moving the epic's
 *  last issue out - and the action's hold keeps the reader's press live through that
 *  (`holdHandOff`), so the row's going reads as the reader's own (`causedByReader`). */
function EpicActions({
  busy,
  epicKey,
  epics,
  onRename,
  onMerge,
  onStartAll,
}: {
  busy: boolean;
  epicKey: string;
  epics: readonly IssueSummary[];
  onRename: () => void;
  onMerge?: (into: string) => void;
  onStartAll: () => void;
}): ReactNode {
  /** The epic the reader has picked to merge into, which no write has been sent for yet. */
  const [target, setTarget] = useState("");
  const [open, setOpen] = useState(false);
  const innerPanel = useNestedPanels<"merge">(open);
  return (
    <span className="ml-auto">
      <PanelTrigger
        align="right"
        ariaDisabled={busy}
        label="Epic actions ▾"
        onOpenChange={setOpen}
        open={open}
        size="compact"
      >
        {(close) => {
          const others = epics.filter((epic) => epic.key !== epicKey);
          const titles = new Map(others.map((epic) => [epic.key, epic.title]));
          return (
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
              {onMerge === undefined ? null : (
                <>
                  {/* Two steps, because a merge closes an issue: the list stages a target, and
                      Merge… asks before anything is written, so no press or key in the list
                      closes an epic. The list is a panel of this one (`useNestedPanels`): a
                      press in it closes neither, and Escape in it closes it alone. The words
                      over it are its trigger's name, which a screen reader hears from the
                      trigger itself. */}
                  <div className={`px-2 py-1 text-xs ${textMutedOnCanvas}`}>
                    <span aria-hidden="true">Merge into another epic</span>
                    <MultiSelect
                      {...innerPanel("merge")}
                      emptyMessage={
                        others.length === 0
                          ? "No other epic to merge into."
                          : "No epic matches that."
                      }
                      label="Merge into another epic"
                      mode="single"
                      onPick={setTarget}
                      optionLabel={(key) => titles.get(key)}
                      options={others.map((epic) => epic.key)}
                      searchLabel="Search epics"
                      triggerClassName={`mt-1 block w-full rounded-md border px-1 py-1 text-left text-sm ${borderDefault} ${surfaceBg} ${textPrimaryOnCanvas}`}
                      value={target === "" ? undefined : target}
                    >
                      {target === "" ? "Choose an epic…" : undefined}
                    </MultiSelect>
                  </div>
                  <button
                    className={`mx-2 my-1 rounded border px-2 py-1 text-sm disabled:opacity-40 ${borderDefault} ${surfaceMutedHoverBg}`}
                    disabled={target === ""}
                    onClick={() => {
                      close();
                      onMerge(target);
                    }}
                    type="button"
                  >
                    Merge…
                  </button>
                  <span className={`px-2 py-1 text-xs ${textMutedOnCanvas}`}>
                    To split it, tick some of its issues and choose “New epic from these”.
                  </span>
                </>
              )}
            </>
          );
        }}
      </PanelTrigger>
    </span>
  );
}
