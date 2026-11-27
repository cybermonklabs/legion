import { useQuery } from "@tanstack/react-query";
import { useCallback, useEffect, useMemo, useState } from "react";
import {
  projectDoneIssuesQuery,
  projectDonePageQuery,
  projectOpenCountQuery,
} from "../../api/queries";
import type { UserState } from "../../api/types";
import { copyAddress } from "../../lib/page-address";
import { ownNarrowings } from "./issues-table-chips";
import { type DoneRead, listingsOf } from "./issues-table-data";
import {
  type Item,
  isBoardFedColumn,
  matchesFilters,
  matchesSearch,
  type View,
} from "./issues-table-model";
import { type Narrowings, passesNarrowings, picksOf } from "./issues-table-state";
import { type ActiveView, builtInAddress, type SavedView } from "./issues-views";

/** What Open's own address asks of its rows, which Open's tab counts by on another view. */
const OPEN_ADDRESS = builtInAddress("open");

/** Whether the board decides which rows a view lists, in a project with a Legion daemon: by a
 *  Queue filter, by a filter of a column the board feeds, or by the trees it leaves out
 *  (`excluded`), which a row's facts name as its `root_key`. */
export function narrowsByBoard(filters: View["filters"], excluded: readonly string[]): boolean {
  return excluded.length > 0 || filters.some(({ id }) => id === "queue" || isBoardFedColumn(id));
}

/** What the page holds that the view tabs count from. */
interface TabReads {
  /** The view the tabs have open, whose count `tabCounts` leaves to the rows the table lists. */
  readonly active: ActiveView;
  /** Every row the page holds, with its facts. */
  readonly items: readonly Item[];
  /** The open listing has answered, on a view that reads it. */
  readonly openIn: boolean;
  /** Open's count by the `total` of `open=true&limit=1`, on a view that reads no open row. */
  readonly openTotal: number | undefined;
  /** Done's count by its own read, kept once Done has been opened in the page. */
  readonly doneKept: number | undefined;
  /** The board has answered, or the project has no Legion daemon and so no board to wait for. */
  readonly boardIn: boolean;
  /** The reader's read state, which Unread narrows by. */
  readonly userState: UserState | undefined;
  readonly saved: readonly SavedView[];
}

/** Each view tab's count, by the built-in view's id or the saved view's name. */
export interface TabCounts {
  readonly built: ReadonlyMap<string, number | undefined>;
  readonly saved: ReadonlyMap<string, number | undefined>;
}

/**
 * The count of the view at `query` from the open listing: the rows it lists by every narrowing
 * it holds, its filters, its search, the issues a link names, the Needs you and Unread flags and
 * the trees it leaves out. None where the open listing cannot give it: a view that lists closed
 * issues, which only its own read holds; the open listing not read, or still out; a narrowing
 * the board decides (`narrowsByBoard`) before the board has answered; and Unread before the
 * reader's read state has.
 */
function openListingCount(query: URLSearchParams, reads: TabReads): number | undefined {
  if (!reads.openIn) return undefined;
  const own = ownNarrowings(query);
  if (listingsOf(picksOf(own.filters, "status")).done) return undefined;
  if (!reads.boardIn && narrowsByBoard(own.filters, own.excluded)) return undefined;
  if (own.unread && reads.userState === undefined) return undefined;
  const narrowings: Narrowings = { ...own, userState: reads.userState };
  return reads.items.filter(
    (item) =>
      matchesFilters(item, own.filters) &&
      matchesSearch(item, own.search) &&
      passesNarrowings(item, narrowings)
  ).length;
}

/**
 * The count each view tab but the open one shows, or none where the page cannot count that view
 * now (the spec's Design, "The views"). Open counts its own view from the open listing, and on a
 * view that reads none by the `total` of `open=true&limit=1`. Done counts by its own read, once
 * it has been opened in the page. A saved view counts from the open listing
 * (`openListingCount`). The open tab counts the rows the table lists, which its caller sets, so
 * its count is not worked out here.
 */
function tabCounts(reads: TabReads): TabCounts {
  const { active } = reads;
  let open: number | undefined;
  if (active.kind !== "built-in" || active.id !== "open")
    open = reads.openIn ? openListingCount(OPEN_ADDRESS, reads) : reads.openTotal;
  const built = new Map<string, number | undefined>([
    ["open", open],
    ["done", reads.doneKept],
  ]);
  const saved = new Map<string, number | undefined>(
    reads.saved.map((view) => [
      view.name,
      active.kind === "saved" && active.name === view.name
        ? undefined
        : openListingCount(copyAddress(view.query), reads),
    ])
  );
  return { built, saved };
}

/** Done's own read with no Closed window: one page of it, in the table in its own sort, or every
 *  done issue, on the Board, at the All rows page size or under a narrowing the browser applies.
 *  Either one counts Done's own view, which lists every done issue. */
export type HeldDoneRead =
  | { readonly kind: "page"; readonly offset: number; readonly size: number }
  | { readonly kind: "all" };

/** Done's own read once Done has been opened in the page, held while another view is open, and
 *  the way to hold another. The Issues tab holds it above its views, so it outlives a change of
 *  view. */
export function useHeldDone(): readonly [HeldDoneRead | undefined, (read: HeldDoneRead) => void] {
  const [heldDone, setHeldDone] = useState<HeldDoneRead | undefined>(undefined);
  const holdDone = useCallback(
    (read: HeldDoneRead) =>
      setHeldDone((held) => (JSON.stringify(held) === JSON.stringify(read) ? held : read)),
    []
  );
  return [heldDone, holdDone];
}

/**
 * Each view tab's count (`tabCounts`), with the open tab's from `listedCount`, the rows the table
 * lists now, the reader's own changes included. Open's count on a view that reads no open row is
 * the `total` of `open=true&limit=1`. Done's own read is kept once Done has been opened in the
 * page, in either layout: the read Done made (`done`, held by `holdDone` into `heldDone`), read
 * again while another view is open only when a live event does.
 */
export function useTabCounts({
  active,
  boardIn,
  done,
  heldDone,
  holdDone,
  items,
  listedCount,
  openIn,
  project,
  readsOpen,
  saved,
  userState,
}: {
  active: ActiveView;
  boardIn: boolean;
  /** The done issues the open view reads. */
  done: DoneRead | undefined;
  heldDone: HeldDoneRead | undefined;
  holdDone: (read: HeldDoneRead) => void;
  items: readonly Item[];
  listedCount: number | undefined;
  openIn: boolean;
  project: string;
  /** The open view reads the open listing. */
  readsOpen: boolean;
  saved: readonly SavedView[];
  userState: UserState | undefined;
}): TabCounts {
  const onDone = active.kind === "built-in" && active.id === "done";
  const openCount = useQuery({ ...projectOpenCountQuery(project), enabled: !readsOpen });
  useEffect(() => {
    if (onDone && done !== undefined && done.window === undefined)
      holdDone(
        done.kind === "page"
          ? { kind: "page", offset: done.offset, size: done.size }
          : { kind: "all" }
      );
  }, [onDone, done, holdDone]);
  // Done's own read, held: leaving Done reads it no more until a live event does. Its count is
  // read off the page here, not by a `select`: TanStack hands what `select` gives to the query's
  // `structuralSharing` too, and the page's (`sharePageByKey`) keeps the first number it is given.
  const donePage = useQuery({
    ...projectDonePageQuery(project, {
      offset: heldDone?.kind === "page" ? heldDone.offset : 0,
      size: heldDone?.kind === "page" ? heldDone.size : 0,
    }),
    refetchOnMount: false,
    enabled: heldDone?.kind === "page" && !onDone,
  });
  const doneAllCount = useQuery({
    ...projectDoneIssuesQuery(project, undefined),
    refetchOnMount: false,
    enabled: heldDone?.kind === "all" && !onDone,
  });
  let doneKept: number | undefined;
  if (heldDone?.kind === "page") doneKept = donePage.data?.total;
  else if (heldDone?.kind === "all") doneKept = doneAllCount.data?.length;
  const openTotal = readsOpen ? undefined : openCount.data;
  // Every tab's count but the open one's, worked out again only when what they count from
  // changes: not at each change of the rows the table lists, which the open tab alone counts.
  const otherCounts = useMemo(
    () => tabCounts({ active, items, openIn, openTotal, doneKept, boardIn, userState, saved }),
    [active, items, boardIn, doneKept, openIn, openTotal, saved, userState]
  );
  // The open tab counts the rows the table lists now, the reader's own changes included.
  return useMemo<TabCounts>(
    () =>
      active.kind === "built-in"
        ? {
            built: new Map(otherCounts.built).set(active.id, listedCount),
            saved: otherCounts.saved,
          }
        : {
            built: otherCounts.built,
            saved: new Map(otherCounts.saved).set(active.name, listedCount),
          },
    [active, listedCount, otherCounts]
  );
}
