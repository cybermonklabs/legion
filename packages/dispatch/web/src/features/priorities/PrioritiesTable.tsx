import { type ReactNode, useCallback, useMemo, useState } from "react";
import type { PriorityRow } from "../../api/types";
import {
  DataTable,
  DataTableGroupRow,
  type DataTableNames,
  type RowNoun,
  useDataTable,
} from "../../components/DataTable";
import type { PagerOptions } from "../../components/Pager";
import { useTableLayout } from "../../components/table-layout";
import { useTableAddress } from "../../lib/page-address";
import { textMutedOnSurfaceMuted } from "../../theme/classes";
import { ALL_ROWS, PAGE_SIZES } from "../project/issues-table-model";
import { NOTHING_KEPT, usedOn } from "./field-arrival";
import { ChangedMeanwhile } from "./InPlaceField";
import { NewPriority } from "./NewPriority";
import { PRIORITY_COLUMN_DEFS, PriorityRowActionsContext } from "./priorities-columns";
import {
  buildPriorityItems,
  matchesPrioritySearch,
  PRIORITY_COLUMNS,
  PRIORITY_COLUMNS_BY_ID,
  PRIORITY_GROUP_ATTRIBUTE,
  PRIORITY_LAYOUT,
  PRIORITY_LINK_ATTRIBUTE,
  PRIORITY_PHONE_HIDDEN,
  PRIORITY_ROW_ATTRIBUTE,
  PRIORITY_SORTING,
  type PriorityItem,
  rankMove,
} from "./priorities-model";
import { TableToolbar } from "./priority-table-controls";
import { compareHeadings, facetCounts, headingOf } from "./priority-table-model";
import { TableBodyRow } from "./priority-table-rows";
import { priorityTableGrammar } from "./priority-table-view";
import { type EditedText, textWriteLabel, usePriorityWrites } from "./priority-writes";

const EMPTY_FACETS: ReadonlyMap<string, number> = new Map();
/** What a row of this table is, which its filters, its pager and its group headings count in. */
const NOUN: RowNoun = {
  group: { one: "group", many: "groups" },
  line: { one: "row", many: "rows" },
  many: "priorities",
  one: "priority",
};
/** The priorities table's own words, which the table core reads and never invents: its rows'
 *  attributes, and the keys that walk them under ids of their own, beside the issue tables'
 *  `list-*` keys of the page's other tabs. It is its tab's one table, so `j` and `k` walk it
 *  from anywhere the focus is in no other table. It selects nothing, so it names no select key. */
const NAMES: DataTableNames = {
  groupAttribute: PRIORITY_GROUP_ATTRIBUTE,
  keymap: {
    next: { id: "priorities-next", keys: "j", label: "Next priority" },
    open: { id: "priorities-open", keys: "o", label: "Open priority" },
    openOnEnter: { id: "priorities-open-enter", keys: "Enter", label: "Open the focused priority" },
    previous: { id: "priorities-previous", keys: "k", label: "Previous priority" },
    scope: "project",
  },
  linkAttribute: PRIORITY_LINK_ATTRIBUTE,
  rowAttribute: PRIORITY_ROW_ATTRIBUTE,
};
/** The line under the table: the page sizes the issue tables offer, and its own test id. */
const PAGER: PagerOptions = {
  allRows: ALL_ROWS,
  pageSizes: PAGE_SIZES,
  testId: "priorities-table-pager",
};
/** The whole key the priorities table keeps its layout under: one table, one deployment, one
 *  stored layout, under the key the page has always written it to. */
const LAYOUT_KEY = "dispatch.project-table.priorities";
/** The address prefix of this table's own view, so the fault lists keep their own. A
 *  module-level constant, since `useTableAddress` memoizes the view it reads on its identity. */
const VIEW_PREFIX = "p";
const VIEW_ADDRESS = priorityTableGrammar(VIEW_PREFIX, PRIORITY_COLUMNS, PRIORITY_SORTING);
/** The priorities a table draws while it draws none. */
const NONE_DRAWN: readonly string[] = [];
/** The one text of a priority a row writes: its description. A title is written on the
 *  priority's own page, so a refused write of one is that page's, or the shell's line's. */
const ROW_TEXTS: readonly EditedText[] = ["description"];

/**
 * The priorities themselves: one row per priority in the one order, with the counts that say
 * what is happening under each. A reader who may write a priority adds one, writes its
 * description and drags its order here, and renames it on its own page, which its title links
 * to; every reader opens a row, which shows the whole of that priority's description. The work
 * advancing a priority is listed on that priority's own page, so this tab draws one table and
 * the keys every table is walked with (`j`, `k`, `x`, `o`, Enter) are this table's alone. The
 * reader's own texts kept beside a priority's description (`ChangedMeanwhile`) stand beside its
 * row, or above the table while the view leaves the row out.
 */
export function PrioritiesTable({ rows }: { rows: readonly PriorityRow[] }): ReactNode {
  const { params, view, setFilters, setGrouping, setPagination, setSearch, setSorting } =
    useTableAddress(VIEW_ADDRESS);
  // On a desktop the title fills the box: it takes the width the other shown columns leave, so
  // the table fits the window and nothing scrolls sideways (`PRIORITY_LAYOUT`'s `fills`). A
  // phone's box draws the place and the title alone, whatever the reader shows, and the title's
  // cell names what the others hold (`PRIORITY_PHONE_HIDDEN`); what the reader shows stays
  // theirs, since a change under Columns writes the reader's own visibility, never this one.
  const stored = useTableLayout(LAYOUT_KEY, PRIORITY_LAYOUT);
  const { compact } = stored.responsive;
  const visibility = useMemo(
    () => (compact ? { ...stored.visibility, ...PRIORITY_PHONE_HIDDEN } : stored.visibility),
    [compact, stored.visibility]
  );
  const layout = { ...stored, visibility };
  const { attachAbove, boxWidth } = layout;
  const [openId, setOpenId] = useState<string>();
  const [dragging, setDragging] = useState<string>();
  const [openFilter, setOpenFilter] = useState<string>();
  const items = useMemo(() => buildPriorityItems(rows), [rows]);
  const order = useMemo(() => items.map((item) => item.priority.id), [items]);
  // A drag writes the place the reader can see. Under another sort, a grouping or a page of
  // rows that is not the whole order, where a row lands is not where it was dropped, so the
  // handle is not drawn at all.
  const canReorder =
    view.grouping.length === 0 &&
    view.sorting.length === 1 &&
    view.sorting[0]?.id === "order" &&
    view.sorting[0]?.desc === false;

  const onDragEnd = useCallback(() => setDragging(undefined), []);
  const onOpen = useCallback(
    (item: PriorityItem) =>
      setOpenId((was) => (was === item.priority.id ? undefined : item.priority.id)),
    []
  );
  const model = useDataTable<PriorityItem>({
    rows: items,
    columns: PRIORITY_COLUMN_DEFS,
    getRowId: (item) => item.priority.id,
    layout,
    names: NAMES,
    state: {
      sorting: view.sorting,
      grouping: view.grouping,
      columnFilters: view.filters,
      globalFilter: view.search,
      pagination: { pageIndex: view.page, pageSize: view.pageSize },
    },
    onSortingChange: setSorting,
    onGroupingChange: setGrouping,
    onColumnFiltersChange: setFilters,
    onGlobalFilterChange: setSearch,
    onPaginationChange: setPagination,
    viewKey: params.toString(),
    search: {
      column: "title",
      filterFn: (row, _id, search: string) => matchesPrioritySearch(row.original, search),
    },
    groupOrder: {
      compare: (id, a, b) => compareHeadings(PRIORITY_COLUMNS_BY_ID, id, a, b),
      heading: (id, item) => headingOf(PRIORITY_COLUMNS_BY_ID, id, item),
    },
  });
  const { table } = model;
  // The priorities the table draws now: the rows of the page and view the reader is on, drawn or
  // scrolled away, and not those a filter, another page or a closed group leaves out. Each
  // drawn one's refused descriptions are the table's to settle with the reader
  // (`usePriorityWrites`); one it does not draw stays on the shell's line, with the reader's
  // text to copy, and so does every refused title.
  const drawnKey = model.displayRows
    .filter(({ row }) => !row.getIsGrouped())
    .map(({ row }) => row.original.priority.id)
    .join("\n");
  const drawn = useMemo(() => (drawnKey === "" ? NONE_DRAWN : drawnKey.split("\n")), [drawnKey]);
  const { canWrite, describe, keep, keeping, kept, keptMadeOn, patch } = usePriorityWrites(
    drawn,
    ROW_TEXTS
  );

  const move = useCallback(
    (item: PriorityItem, onto: string) => {
      const rank = rankMove(order, item.priority.id, onto);
      if (rank === undefined) return;
      patch.mutate({
        id: item.priority.id,
        input: { rank },
        label: `Move the priority “${item.priority.title}” in the order`,
      });
    },
    [order, patch.mutate]
  );
  const onDescribe = useCallback(
    (item: PriorityItem, description: string, from: string, on: string) =>
      describe(
        item.priority.id,
        description,
        from,
        on,
        textWriteLabel("description", item.priority.title, description)
      ),
    [describe]
  );
  const onMove = useCallback(
    (item: PriorityItem, by: -1 | 1) => {
      const onto = order[order.indexOf(item.priority.id) + by];
      if (onto !== undefined) move(item, onto);
    },
    [move, order]
  );
  const keptDescription = useCallback(
    (id: string, held: string) => kept(id, "description", held),
    [kept]
  );
  const keepDescription = useCallback(
    (id: string, texts: readonly string[]) => keep(id, "description", texts),
    [keep]
  );
  const descriptionMadeOn = useCallback(
    (id: string, text: string) => keptMadeOn(id, "description", text),
    [keptMadeOn]
  );
  // One object while nothing a row can do changes: every cell reads it
  // (`PriorityRowActionsContext`), and is drawn again when it changes.
  const actions = useMemo(
    () => ({
      canReorder,
      canWrite,
      compact,
      dragging,
      kept: keptDescription,
      keptMadeOn: descriptionMadeOn,
      onDragEnd,
      onDragStart: setDragging,
      onDescribe,
      onKeep: keepDescription,
      onMove,
      onOpen,
      openId,
    }),
    [
      canReorder,
      canWrite,
      compact,
      descriptionMadeOn,
      dragging,
      keepDescription,
      keptDescription,
      onDescribe,
      onDragEnd,
      onMove,
      onOpen,
      openId,
    ]
  );
  // The reader's descriptions kept beside a priority the table no longer draws - one the other
  // person's change took out of the search, or onto another page - which no row shows, so they
  // are shown above the table, each as its editor would show it. One the reader uses there is
  // written over the description the page reads, which no editor stands over: they chose it. It
  // is made on what its own write was made on, as the page holds it, through each kept text the
  // click lets go (`usedOn`), and else on the description it goes over.
  const undrawn = keeping.flatMap(({ id }) => {
    const item = drawn.includes(id) ? undefined : items.find((one) => one.priority.id === id);
    if (item === undefined) return [];
    const mine = keptDescription(id, item.priority.description);
    return mine.length === 0 ? [] : [{ item, mine }];
  });
  const facets = useMemo(
    () =>
      openFilter === undefined
        ? EMPTY_FACETS
        : facetCounts(
            items,
            PRIORITY_COLUMNS_BY_ID,
            openFilter,
            view.filters,
            view.search,
            matchesPrioritySearch
          ),
    [items, openFilter, view.filters, view.search]
  );
  const visibleColumns = table.getVisibleLeafColumns().length;

  return (
    <section aria-label="Priorities" className="space-y-3">
      <div className="space-y-3" ref={attachAbove}>
        <TableToolbar
          compact={compact}
          searchLabel="Search priorities"
          searchPlaceholder="Search priorities: title, owner"
          table={table}
        >
          <NewPriority />
        </TableToolbar>
        {undrawn.map(({ item, mine }) => (
          <ChangedMeanwhile
            key={item.priority.id}
            label={`Description of ${item.order}`}
            mine={mine}
            onKeepTheirs={() => keepDescription(item.priority.id, NOTHING_KEPT)}
            onUse={(text) => {
              const { description, id } = item.priority;
              const known = usedOn(text, mine, (one) => descriptionMadeOn(id, one));
              keepDescription(id, NOTHING_KEPT);
              onDescribe(item, text, description, known ?? description);
            }}
          />
        ))}
      </div>

      <PriorityRowActionsContext.Provider value={actions}>
        <DataTable
          empty="No priorities match these filters."
          filterCounts={facets}
          fitRows
          label="Priorities table"
          model={model}
          noun={NOUN}
          pager={PAGER}
          onOpenFilterChange={setOpenFilter}
          openFilter={openFilter}
          renderGroup={({ entry: { continued, row, shown }, index, measure }) => {
            const heading = String(row.groupingValue ?? "");
            const leaves = row.getLeafRows().filter((leaf) => !leaf.getIsGrouped()).length;
            const counted = `${leaves} ${leaves === 1 ? NOUN.one : NOUN.many}`;
            // What the reader can see under this heading, which a page boundary can make fewer
            // than the group holds: "1 of 2 priorities" rather than a count of rows not here.
            const count = (shown ?? leaves) === leaves ? counted : `${shown} of ${counted}`;
            return (
              <DataTableGroupRow
                columns={visibleColumns}
                expanded={row.getIsExpanded()}
                heading={
                  <>
                    {heading}
                    {continued ? (
                      <span className={`font-normal ${textMutedOnSurfaceMuted}`}>(continued)</span>
                    ) : null}
                    <span className={`font-normal ${textMutedOnSurfaceMuted}`}>{count}</span>
                  </>
                }
                index={index}
                label={`${row.getIsExpanded() ? "Collapse" : "Expand"} ${heading}, ${count}${continued ? ", continued" : ""}`}
                measure={measure}
                onToggle={row.getToggleExpandedHandler()}
                width={Math.max(180, boxWidth - 20)}
              />
            );
          }}
          renderRow={({ row, index, measure }) => (
            <TableBodyRow
              dragging={dragging}
              index={index}
              measure={measure}
              onDrop={(onto) => {
                const item = items.find((candidate) => candidate.priority.id === dragging);
                setDragging(undefined);
                if (item !== undefined) move(item, onto);
              }}
              label={`${row.original.order} ${row.original.priority.title}`}
              row={row}
            />
          )}
          scrollTestId="priorities-table-scroll"
          size="compact"
          // A phone draws `#` beside the one cell that holds the rest of the row, too narrow for
          // a filter's touch target beside its sort's.
          sortOnlyOnPhone="order"
        />
      </PriorityRowActionsContext.Provider>
    </section>
  );
}
