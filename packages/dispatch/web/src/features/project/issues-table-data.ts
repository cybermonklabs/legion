import { useQuery } from "@tanstack/react-query";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  advancesIssuesQuery,
  legionBoardQuery,
  legionItemsQuery,
  projectDoneIssuesQuery,
  projectDonePageQuery,
  projectIssuesQuery,
  sameDoneWindow,
} from "../../api/queries";
import type { IssueSummary, LegionBoardResponse } from "../../api/types";
import type { ClosedWindow } from "../../lib/closed-window";
import { claimIsTakeable } from "../issue/ClaimChip";
import { useAgentText } from "../refs/AgentName";
import type { AgentRegistry } from "../refs/agent-registry";
import { openIssueStatuses } from "./board-model";
import { itemsWindows, type LegionFacts, listedDoneRows } from "./issues-legion";
import { buildItems, type LegionReads, type ReadStatus } from "./issues-table-model";

/**
 * What a table lists. A project's table, its Issues tab, lists that project's issues; a scoped
 * table lists the issues that advance one priority (`AdvancesListing`). A priority belongs to no
 * project, so a scoped table lists the work of every project and stands on none of them: it can
 * make no write that names a project, and it offers none (`issues-table-actions.ts`).
 */
export type IssueListing = { readonly project: string } | AdvancesListing;

/**
 * The issues of every project the server narrows by what they advance: one priority's id, or
 * `none` for the work that advances no live priority and `any` for the work that advances one.
 * The server narrows the listing by it (`GET /api/v1/issues?advances=<id>`). `statuses`, where
 * it is set, is the whole of what the listing holds - the issues standing in one of those
 * statuses, which is how the priorities page asks for each of its two faults (see
 * `answeredWhole` for when its open read is every row of it). `own` keeps the issues whose own
 * row names what they advance, and leaves out those that inherit it from an issue above them.
 * `moved` asks for the time each row standing in backlog or icebox was moved there and who moved
 * it.
 */
export type AdvancesListing = {
  readonly advances: string;
  readonly own?: boolean;
  readonly statuses?: readonly string[];
  readonly moved?: boolean;
};

/**
 * Whether a listing's one open read (`open: true`) answers every row its table can show, so the
 * table never reads the closed issues beside it: a listing that names its statuses, every one of
 * them open. A listing whose statuses include `done` holds closed issues the open read leaves
 * out, and a listing that names no status holds every closed issue it has: its table reads its
 * whole listing once the reader lets closed issues through.
 */
export function answeredWhole(listing: AdvancesListing): boolean {
  return (
    listing.statuses?.every((status) =>
      (openIssueStatuses as readonly string[]).includes(status)
    ) === true
  );
}

/**
 * Whether a listing can hold an issue in Legion's queue, the one row a row's Start next acts on:
 * an issue is queued only while it stands in `todo` (`isQueued`), so a listing that names its
 * statuses without `todo` - each fault list of the priorities page - never holds one, and its
 * table draws no column for that action, which would stand empty on every row.
 */
export function listsQueued(listing: IssueListing): boolean {
  return (
    !("advances" in listing) || listing.statuses === undefined || listing.statuses.includes("todo")
  );
}

/** The name what a table keeps of its listing is held under: the action a reload interrupted.
 *  Two listings of different rows never share one. */
export const listingName = (listing: IssueListing): string =>
  "project" in listing
    ? listing.project
    : [
        `advances:${listing.advances}`,
        ...(listing.own ? ["own"] : []),
        ...(listing.statuses ?? []),
      ].join(":");

/** Which listings a view reads by the picks of its Status filter (the spec's Loading section):
 *  the open listing unless it picks Done alone, and the done issues where it picks Done or no
 *  status. */
export function listingsOf(statusPicks: readonly string[]): {
  readonly open: boolean;
  readonly done: boolean;
} {
  return {
    open: statusPicks.length === 0 || statusPicks.some((status) => status !== "done"),
    done: statusPicks.length === 0 || statusPicks.includes("done"),
  };
}

/**
 * Which done issues a view of the Issues tab reads (the spec's Loading section): one page of
 * them in their close order, newest first (`page`), while the view is sorted by Closed and
 * narrowed by nothing but a Closed window; else every done issue the window holds (`all`), which
 * the browser sorts, narrows and pages. `window` is the view's Closed window, none for every done
 * issue. `nowMs` is the clock the caller holds, which the items of a read of every done row run
 * to.
 */
export type DoneRead =
  | {
      readonly kind: "page";
      readonly window: ClosedWindow | undefined;
      readonly offset: number;
      readonly size: number;
    }
  | { readonly kind: "all"; readonly window: ClosedWindow | undefined; readonly nowMs: number };

/** What a view of the Issues tab reads beside the project's open issues. */
export interface ProjectReads {
  /** The view lists open issues: the open listing is read. A view that lists only done issues
   *  reads no open row. */
  readonly open: boolean;
  /** The done issues the view lists, or undefined for a view that lists none. The caller holds
   *  one object per read (a memo), since the rows and their facts are derived from it. */
  readonly done: DoneRead | undefined;
  /** The project has a Legion daemon: its board is read, and the items of the done rows the
   *  board's map does not hold once the board has answered. */
  readonly legion: boolean;
}

const NO_ISSUES: readonly IssueSummary[] = [];
const NO_FACTS: Readonly<Record<string, LegionFacts>> = {};

/** How long a close waits before the items are read once more, where the read the close started
 *  held no row for it: a stored facts row is never more than 60 s behind the event that changed
 *  it (dispatch://INFRA-2916's R21). */
export const CLOSE_ITEMS_AGAIN_MS = 60_000;

/** The rows of `all` as a table draws them (`buildItems`), with the names the agents go by. A row
 *  is free where nobody holds its claim or the server would hand the claim over
 *  (`claimIsTakeable`, against `registry`). `legion` is a project's Legion reads, where it has a
 *  daemon. */
function useBuiltItems(
  all: readonly IssueSummary[],
  registry: AgentRegistry,
  viewer: string | undefined,
  legion?: LegionReads
) {
  const text = useAgentText();
  return useMemo(
    () =>
      buildItems(
        all,
        text.label,
        (issue) => issue.claim === null || claimIsTakeable(issue.claim, registry),
        { legion, viewer }
      ),
    [all, text, registry, legion, viewer]
  );
}

/**
 * A project's issues as a view of its Issues tab shows them: the open listing once, unless the
 * view lists only done issues, and the done issues the view lists (`DoneRead`), never the full
 * listing. The open rows come first, then the done ones, as the listing's own order has them. A
 * Done read with a window none of whose rows carries `closed_at` lists none of them
 * (`listedDoneRows`), and a page read whose answer does not name its sort fails
 * (`UnsortedPageError`). In a project with a Legion daemon the board is read, and the facts of
 * the listed done rows its map does not hold come from the items of `legion-work-time` over
 * their close days, read once the board has answered; items of rows the view does not list are
 * dropped.
 */
export function useProjectTableItems(
  project: string,
  {
    reads,
    registry,
    viewer,
  }: { reads: ProjectReads; registry: AgentRegistry; viewer?: string | undefined }
) {
  const done = reads.done;
  // A view that reads its done rows a page at a time.
  const paged = done?.kind === "page";
  const everyDone = useQuery({
    ...projectDoneIssuesQuery(project, done?.window),
    enabled: done?.kind === "all",
  });
  const pageRead = projectDonePageQuery(project, {
    offset: done?.kind === "page" ? done.offset : 0,
    size: done?.kind === "page" ? done.size : 0,
    window: done?.window,
  });
  const donePage = useQuery({
    ...pageRead,
    // A step of the pager keeps the page on screen until the next one answers: a read of the
    // same window (`sameDoneWindow`). A read of another window keeps nothing of the last one,
    // whose rows and `total` are not this window's: the view shows its loading state until it
    // answers.
    placeholderData: (previous, previousQuery) =>
      previousQuery !== undefined && sameDoneWindow(previousQuery.queryKey, pageRead.queryKey)
        ? previous
        : undefined,
    enabled: paged,
  });
  const doneRead = paged ? donePage : everyDone;
  const doneLoaded = done !== undefined && doneRead.data !== undefined;
  const openIssues = useQuery({ ...projectIssuesQuery(project, true), enabled: reads.open });
  const board = useQuery({ ...legionBoardQuery(project), enabled: reads.legion });

  const openRows = reads.open ? openIssues.data : NO_ISSUES;
  const readRows = paged
    ? donePage.data === undefined
      ? undefined
      : (donePage.data.issues ?? NO_ISSUES)
    : everyDone.data;
  const doneRows = useMemo(
    () =>
      done === undefined || readRows === undefined
        ? NO_ISSUES
        : listedDoneRows(readRows, done.window !== undefined),
    [done, readRows]
  );
  const all = useMemo(() => {
    const open = openRows ?? NO_ISSUES;
    if (doneRows.length === 0) return open;
    const keys = new Set(open.map((issue) => issue.key));
    return [...open, ...doneRows.filter((issue) => !keys.has(issue.key))];
  }, [openRows, doneRows]);

  // The items: the close days of the listed done rows the board's map does not hold, from the
  // oldest such close to the newest on a page, and to now for a read of every done row.
  const windows = useMemo(
    () =>
      !reads.legion || done === undefined || board.data === undefined
        ? []
        : itemsWindows(doneRows, board.data, done.kind === "all" ? done.nowMs : undefined),
    [reads.legion, done, board.data, doneRows]
  );
  const itemsRead = reads.legion && board.data !== undefined && windows.length > 0;
  const items = useQuery({ ...legionItemsQuery(project, windows), enabled: itemsRead });
  const listedItems = useMemo(() => {
    if (items.data === undefined) return undefined;
    const listed = new Set(doneRows.map((issue) => issue.key));
    const kept = Object.entries(items.data).filter(([key]) => listed.has(key));
    return kept.length === 0 ? NO_FACTS : Object.fromEntries(kept);
  }, [items.data, doneRows]);
  // A read the view does not make is answered: only a read that is out, or failed, holds a done
  // row's facts back. A later failed read keeps the facts it had.
  let itemsStatus: ReadStatus = "success";
  if (itemsRead) {
    if (items.data === undefined) itemsStatus = items.status;
  } else if (board.data === undefined && reads.legion && done !== undefined) {
    itemsStatus = board.status === "error" ? "error" : "pending";
  }
  const legionReads = useMemo<LegionReads | undefined>(
    () =>
      reads.legion
        ? { board: board.data, boardStatus: board.status, items: listedItems, itemsStatus }
        : undefined,
    [reads.legion, board.data, board.status, listedItems, itemsStatus]
  );
  useDoneFactsCurrent({
    board: board.data,
    doneAt: doneRead.dataUpdatedAt,
    doneKey: done === undefined ? "" : JSON.stringify(done),
    doneRows,
    items: itemsRead ? items : undefined,
  });

  // Each build is of its own moment (`Item.at`): the board's facts that grow are closed at it,
  // and the rows' times count to it. A board frame builds the rows again, and the board's header
  // moves with each read of the daemon, once a minute (`legionBoardView` in `api/live_frames.go`).
  const allItems = useBuiltItems(all, registry, viewer, legionReads);
  const listingError =
    done !== undefined && doneRead.isError
      ? doneRead
      : reads.open && openIssues.isError
        ? openIssues
        : undefined;
  return {
    allItems,
    board,
    doneLoaded,
    doneRead,
    /** Every done issue the paged read's window holds, where the view reads a page. */
    doneTotal: paged ? donePage.data?.total : undefined,
    issues: openIssues,
    items,
    itemsStatus,
    listingError,
  };
}

/**
 * The issues of a listing that advances a priority, as its table shows them: its open read, and
 * its whole listing once the reader lets `closed` issues through. Two reads, never three: the
 * open read stops once the whole one has answered, so a live event refetches one listing. A
 * listing its open read answers whole never asks for the whole one (`answeredWhole`, which its
 * table reads). A priority belongs to no project, so the listing is the work of every project.
 */
export function useAdvancesTableItems(
  listing: AdvancesListing,
  {
    closed,
    registry,
    viewer,
  }: { closed: boolean; registry: AgentRegistry; viewer?: string | undefined }
) {
  const whole = useQuery({ ...advancesIssuesQuery(listing), enabled: closed });
  const wholeIn = closed && whole.data !== undefined;
  const open = useQuery({ ...advancesIssuesQuery(listing, true), enabled: !wholeIn });
  // The whole listing once it has answered; else the open read.
  const issues = wholeIn ? whole : open;
  const all = issues.data ?? NO_ISSUES;
  const allItems = useBuiltItems(all, registry, viewer);
  const listingError = closed && whole.isError ? whole : open.isError ? open : undefined;
  return { allItems, issues, whole, wholeIn, listingError };
}

/**
 * The three rules that keep the facts of the done rows a view lists current (the spec's Loading
 * section). First, a live event reads the view's listing again, and its items are read again
 * with it. Second, a close whose items answer holds no row for the closed issue reads the items
 * once more 60 s later. Third, a board push that changes or removes the key of a listed done row
 * in the board's map reads the items again. `items` is the items read, where the view makes one.
 */
function useDoneFactsCurrent({
  board,
  doneAt,
  doneKey,
  doneRows,
  items,
}: {
  board: LegionBoardResponse | undefined;
  doneAt: number;
  doneKey: string;
  doneRows: readonly IssueSummary[];
  items:
    | {
        readonly data: Readonly<Record<string, LegionFacts>> | undefined;
        readonly dataUpdatedAt: number;
        readonly refetch: (options: { cancelRefetch: boolean }) => Promise<unknown>;
      }
    | undefined;
}) {
  // The latest of what the rules read, so each effect runs on its own trigger alone.
  const latest = useRef({ doneRows, items });
  latest.current = { doneRows, items };
  // A read that is out already answers for this one too.
  const readAgain = useCallback(
    () => void latest.current.items?.refetch({ cancelRefetch: false }),
    []
  );
  const seen = useRef<{ key: string; at: number; keys: ReadonlySet<string> } | undefined>(
    undefined
  );
  const closing = useRef<{ keys: readonly string[]; itemsAt: number } | undefined>(undefined);
  // The timers of the second rule that have not fired yet, cleared as the view goes.
  const [timers] = useState(() => new Set<ReturnType<typeof setTimeout>>());
  useEffect(
    () => () => {
      for (const timer of timers) clearTimeout(timer);
      timers.clear();
    },
    [timers]
  );

  // The first rule, and the close the second waits on: the same done read answered again.
  useEffect(() => {
    const keys = new Set(latest.current.doneRows.map((issue) => issue.key));
    const before = seen.current;
    seen.current = { key: doneKey, at: doneAt, keys };
    if (doneKey === "" || doneAt === 0) return;
    if (before === undefined || before.key !== doneKey || before.at === 0 || before.at === doneAt)
      return;
    const closed = [...keys].filter((key) => !before.keys.has(key));
    if (closed.length > 0)
      closing.current = { keys: closed, itemsAt: latest.current.items?.dataUpdatedAt ?? 0 };
    readAgain();
  }, [doneKey, doneAt, readAgain]);

  // The second rule: the items answer a close started, which holds no row for a closed issue.
  const itemsAt = items?.dataUpdatedAt ?? 0;
  useEffect(() => {
    const wait = closing.current;
    const answered = latest.current.items?.data;
    if (wait === undefined || answered === undefined || itemsAt === wait.itemsAt) return;
    closing.current = undefined;
    if (wait.keys.every((key) => Object.hasOwn(answered, key))) return;
    const timer = setTimeout(() => {
      timers.delete(timer);
      readAgain();
    }, CLOSE_ITEMS_AGAIN_MS);
    timers.add(timer);
  }, [itemsAt, readAgain, timers]);

  // The third rule: a board push that changes or removes a listed done row's key in its map.
  const boardIssues = board?.issues;
  const held = useRef(boardIssues);
  useEffect(() => {
    const before = held.current;
    held.current = boardIssues;
    if (before === undefined || before === null || before === boardIssues) return;
    const moved = latest.current.doneRows.some(
      ({ key }) => Object.hasOwn(before, key) && boardIssues?.[key] !== before[key]
    );
    if (moved) readAgain();
  }, [boardIssues, readAgain]);
}
