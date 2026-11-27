import { useMemo, useState } from "react";
import { UnsortedPageError } from "../../api/queries";
import { readWindow } from "../../lib/closed-window";
import type { TableAddress } from "../../lib/page-address";
import type { AgentRegistry } from "../refs/agent-registry";
import type { IssueFiltersState } from "./issue-filters";
import { narrowsByBoard } from "./issues-tab-counts";
import { type DoneRead, listingsOf, useProjectTableItems } from "./issues-table-data";
import { ALL_ROWS, FACTS_COLUMNS, isBoardFedColumn, type View } from "./issues-table-model";
import { picksOf } from "./issues-table-state";

/**
 * What a view of the Issues tab of `project` reads, and how far each read is (the spec's Loading
 * section). A view reads the open listing unless its Status filter picks Done alone, and the done
 * issues where it picks Done or no status: one page of them while the table shows it sorted by
 * Closed, newest first, and narrowed by nothing but a Closed window, and every done issue the
 * window holds otherwise (`DoneRead`). In a project with a Legion daemon (`hasLegion`) it reads
 * the board, and a view whose rows the board decides (`narrowsByBoard`), or with a Legion sort or
 * grouping, waits for it; one whose filter, sort or grouping reads facts waits for the facts of
 * its done rows too. It never lists rows, or a count, from a read that has not answered.
 */
export function useTabReads({
  address,
  excluded,
  flags,
  hasLegion,
  onBoard,
  project,
  registry,
  userState,
  viewer,
}: {
  address: TableAddress<View>;
  /** The trees the view leaves out (`exclude_tree`). */
  excluded: readonly string[];
  flags: IssueFiltersState;
  hasLegion: boolean;
  /** The view draws the Board, which reads every done issue rather than a page. */
  onBoard: boolean;
  project: string;
  registry: AgentRegistry;
  /** The read of the reader's read state, which every view waits for. */
  userState: { readonly isPending: boolean; readonly isError: boolean };
  viewer: string | undefined;
}) {
  const { params, view } = address;
  const { sorting, grouping, filters, search } = view;
  const statusPicks = picksOf(filters, "status");
  const listings = listingsOf(statusPicks);
  const readsOpen = listings.open;
  const closedValue = filters.find((filter) => filter.id === "closed")?.value;
  const closed = typeof closedValue === "string" ? closedValue : undefined;
  // The clock a window is read against, held while the Closed filter stays, so a rolling
  // window's page read keeps its key until the reader changes the window.
  const [clock, setClock] = useState(() => ({ closed, now: Date.now() }));
  if (clock.closed !== closed) setClock({ closed, now: Date.now() });
  const closedWindow = readWindow(closed, clock.closed === closed ? clock.now : Date.now());
  const firstSort = sorting[0];
  const pageRead =
    !onBoard &&
    statusPicks.length === 1 &&
    statusPicks[0] === "done" &&
    sorting.length === 1 &&
    firstSort?.id === "closed" &&
    firstSort.desc &&
    search === "" &&
    grouping.length === 0 &&
    view.pageSize !== ALL_ROWS &&
    !flags.needsYou &&
    !flags.unread &&
    view.selection === null &&
    filters.every((filter) => filter.id === "status" || filter.id === "closed") &&
    excluded.length === 0;
  const windowFrom = closedWindow?.from;
  const windowTo = closedWindow?.to;
  const windowRolling = closedWindow?.rolling;
  // The page on screen, for a view whose done rows are read a page at a time.
  const offset = pageRead ? view.page * view.pageSize : 0;
  const size = pageRead ? view.pageSize : 0;
  const done = useMemo<DoneRead | undefined>(() => {
    if (!listings.done) return undefined;
    const held =
      windowFrom === undefined || windowTo === undefined || windowRolling === undefined
        ? undefined
        : { from: windowFrom, to: windowTo, rolling: windowRolling };
    return pageRead
      ? { kind: "page", window: held, offset, size }
      : { kind: "all", window: held, nowMs: clock.now };
  }, [listings.done, pageRead, windowFrom, windowTo, windowRolling, offset, size, clock.now]);
  const rows = useProjectTableItems(project, {
    reads: { open: readsOpen, done, legion: hasLegion },
    registry,
    viewer,
  });
  const { board, doneRead, issues, itemsStatus, listingError } = rows;
  const reading = [...filters.map(({ id }) => id), ...sorting.map(({ id }) => id), ...grouping];
  const boardWaits =
    hasLegion && (narrowsByBoard(filters, excluded) || reading.some(isBoardFedColumn));
  const factsWait =
    hasLegion &&
    done !== undefined &&
    (reading.some((id) => FACTS_COLUMNS.includes(id)) || excluded.length > 0);
  const loading =
    (readsOpen && issues.isPending) ||
    userState.isPending ||
    (!readsOpen && done !== undefined && doneRead.isPending) ||
    (boardWaits && board.isPending) ||
    (factsWait && itemsStatus === "pending");
  const failed =
    !loading &&
    ((readsOpen && issues.isError && issues.data === undefined) ||
      (!readsOpen && done !== undefined && doneRead.isError && doneRead.data === undefined) ||
      userState.isError);
  const legionFailed =
    !loading &&
    !failed &&
    ((boardWaits && board.isError && board.data === undefined) ||
      (factsWait && itemsStatus === "error"));
  const loaded = !loading && !failed && !legionFailed;
  return {
    ...rows,
    statusPicks,
    readsOpen,
    done,
    boardWaits,
    loading,
    failed,
    legionFailed,
    loaded,
    /** The rows the table waits on are in, which the counts it draws count. */
    counted: loaded && listingError === undefined && (done === undefined || rows.doneLoaded),
    /** Every listing the view reads has answered. */
    listingIn:
      (!readsOpen || issues.data !== undefined) &&
      (done === undefined || doneRead.data !== undefined),
    /** Done's page read failed on an answer the server did not sort (`UnsortedPageError`). */
    unsorted: done !== undefined && doneRead.error instanceof UnsortedPageError,
    /** The view the reader picked, and whether its first draw is whole. In a project with a
     *  Legion daemon the board's first snapshot is part of it: Open sorts by Queue and draws its
     *  rows before the board answers, and the snapshot sorts them again. So the commit that
     *  draws the snapshot is the view's first draw too, as a change of view is: the table starts
     *  at its top and the anchor holds nothing for it, and Open starts with the Queue sort's
     *  first row (R11). */
    viewKey:
      hasLegion && board.data === undefined ? `${params.toString()}\nno board` : params.toString(),
  };
}
