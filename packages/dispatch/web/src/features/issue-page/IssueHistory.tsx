import {
  replaceEqualDeep,
  useInfiniteQuery,
  useQuery,
  useQueryClient,
} from "@tanstack/react-query";
import { type ReactNode, useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import { Link, useNavigationType } from "react-router-dom";

import { api } from "../../api/client";
import { type EventPages, mergeEventPages } from "../../api/event-feed";
import type { Artifact, IssueDetails, UserState } from "../../api/types";
import { PinButton } from "../../components/PinButton";
import {
  pageHairBg,
  pageInkText,
  pageKindTag,
  pageLineBorder,
  pageMarkBg,
  pageMutedText,
  pageQuietText,
  pageRowAct,
} from "../../theme/classes";
import { ActivityLine } from "../activity/ActivityLine";
import { dateKey } from "../activity/activity-model";
import { scrollElementToTop } from "../doc/scroll-block";
import { writePin } from "../issue/pin-write";
import { eventItemId, isPinnedEvent, stateForIssue } from "../issue/pins";
import { HISTORY_ENTRY_ATTRIBUTE } from "../margin/details-focus-keeper";
import { KEEPS_DETAILS_ATTRIBUTE } from "../margin/Margin";
import { AgentName, type AgentPart, AgentParts } from "../refs/AgentName";
import { MarkdownBody } from "../refs/MarkdownBody";
import { buildIssuePath, buildReferencePath, documentRoute } from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";
import { textSourceOf } from "../refs/text-references";
import { PHONE_VIEWPORT_QUERY, useMediaQuery } from "../shell/useDialog";
import { ViewportAnchor } from "../shell/ViewportAnchor";
import { CutWords } from "./CutWords";
import { DetailsErrorLine } from "./details-error";
import { HistoryHandoff } from "./HistoryHandoff";
import {
  buildHistory,
  type HistoryDay,
  type HistoryEntry,
  type HistoryTurn,
} from "./history-model";
import { useMarginDrawer } from "./margin-slot";
import { useOutlineWriter } from "./outline-writer";
import { authorText, pageChrome } from "./page-structure";
import { ANSWER_LINES, CONTROL_LABELS } from "./page-words";
import { ROW_LINK } from "./row-references";

/** How many events a page of the log holds, as the server answers it. */
const PAGE = 200;

/** The event the history's heading raises, bubbling, once an arrival at `/activity` has brought
 *  it to the top of Details' view. That scroll is the landing the arrival asked for, not a move
 *  that takes the reader from their place: the hold of the place `t v` lands the focus at keeps
 *  the focused history where this scroll leaves it (`details-more.tsx`). */
export const HISTORY_HEADING_LANDED_EVENT = "history-heading-landed";

/** A day's line, the prototype's `.hdl`: 12 px on 18 px at weight 600 in the muted grey, 5 px
 *  above and below, its words as written. Its count is at weight 500 in the quiet grey. */
const DAY_LINE = `py-[5px] text-left text-xs leading-[18px] font-semibold ${pageMutedText}`;

/** One entry, the prototype's `.hev`: 12 px on 18 px in the muted grey, 14 px from a 2 px rule
 *  at its left in the page's line, the rule 4 px from the column's edge, 4 px above and 6 px
 *  below. The prototype's rule is #EEEAE3, which no page token holds; `page-line` is #ECEAE7. */
const ENTRY = `ml-1 border-l-2 pt-1 pb-1.5 pl-3.5 text-xs leading-[18px] ${pageLineBorder} ${pageMutedText}`;

/** When an entry was made, which opens it (`.ht`), 6 px before what follows: the quiet grey, as
 *  the prototype's #A8A29E reads 2.52:1 on the paper, under the 4.5:1 text needs. */
const TIME = `mr-1.5 ${pageQuietText}`;

/** Who made the entry (`.hev b`). */
const AUTHOR = `font-semibold ${pageInkText}`;

/** An agent's words (`.hb`): 14 px on 20 px in the issue's own colour, the prototype's
 *  #44403C, 2 px under the entry's line. */
const WORDS = `mt-0.5 min-w-0 text-sm leading-5 [overflow-wrap:anywhere] ${pageKindTag.issue}`;

/**
 * The issue's history, a list in the panel (`history-model.ts`):
 * newest first, each day before today folded to one line with its count, which opens it - the
 * day the pages of the log read so far end in with no count, as its entries are not all read
 * yet (`HistoryDay.partial`). An event is one line with its pin; a run of uploads - one
 * author's newest upload and their uploads made at most a minute before it, with no other
 * author's upload between them - is one entry that opens each file, each with its own upload's
 * pin, drawn under one key as the run grows or a newer upload splits it (`runKeys`); a thread
 * agents wrote among themselves is drawn whole, each turn's words with its Reply, which sets the
 * line at the foot of the outline to reply to it - and once a person has replied in a thread,
 * the outline draws it and this list no longer does.
 *
 * The day that was today as midnight passes with the history drawn stays open, as if the reader
 * had opened it: a reader reading its entries then is still on them.
 *
 * `/activity`, `t v` and the address of a comment or message of such a thread open Details here:
 * at `/activity` the heading is brought into Details' view once for each arrival, except a
 * reader's coming back to the history they left drawn, who finds it where they left it;
 * `item` is the entry such an address names, which is marked and brought into view, its day
 * opened. It writes no read mark. The line that says what is new is drawn at `visitMark`,
 * where the reader's mark stood as the history first drew in this visit, which
 * `DetailsHistory` holds while Details is shut and this list is not drawn: so a later move of
 * the mark, from another page or tab, leaves the line where the reader found it.
 *
 * On a phone Details is a sheet over the page, and a link followed in it closes it
 * (`Margin.tsx`), so a link of the history that leaves it - a file, a document, a row of the
 * outline - closes it. A turn's own time is that turn's address, which opens Details here, so
 * it keeps Details open (`KEEPS_DETAILS_ATTRIBUTE`); a turn's Reply, a button, closes it itself.
 */
export function IssueHistory({
  activityAddress,
  issue,
  item,
  state,
  visitMark,
}: {
  /** The `/activity` address the reader is at, or undefined at any other: each arrival at it
   *  brings the history's heading into Details' view once. */
  activityAddress: string | undefined;
  issue: IssueDetails;
  /** The id of the comment or message the address names, which the list marks. */
  item: string | undefined;
  state: UserState | undefined;
  /** The reader's read mark for this visit (`DetailsHistory`), where the line that says what is
   *  new is drawn. */
  visitMark: number;
}): ReactNode {
  const issueKey = issue.key;
  const queryClient = useQueryClient();
  const log = useInfiniteQuery({
    initialPageParam: null as number | null,
    queryKey: ["events", issueKey],
    queryFn: ({ pageParam }) =>
      api.getIssueEvents(
        issueKey,
        pageParam === null ? { limit: PAGE, order: "desc" } : { before: pageParam, limit: PAGE }
      ),
    // A page can hold more than the server's 200 once streamed events sit above a full response
    // (mergeEventPages, prependEventToLog); it is still a full page with older events beneath it.
    getNextPageParam: (page) => (page.length >= PAGE ? page.at(-1)?.seq : undefined),
    // The feed (api/event-feed.ts) prepends events as they are published; a refetch whose read
    // predates one of them must not take it back.
    structuralSharing: (current, incoming) =>
      replaceEqualDeep(
        current,
        mergeEventPages(current as EventPages | undefined, incoming as EventPages)
      ),
  });
  // The outline's own reads, under its keys, so the threads cost no request of their own: never
  // stale here, since the outline's own reads and every invalidation keep them current, so an
  // open of Details reads the cache however long ago the outline read it. The asks tell a reply
  // under a question, the outline's, from one under an approval request.
  const comments = useQuery({
    queryFn: () => api.listComments(issueKey),
    queryKey: ["comments", issueKey],
    staleTime: Number.POSITIVE_INFINITY,
  });
  const messages = useQuery({
    queryFn: () => api.listIssueMessages(issueKey),
    queryKey: ["issue-messages", issueKey],
    staleTime: Number.POSITIVE_INFINITY,
  });
  const asks = useQuery({
    queryFn: () => api.listIssueAsks(issueKey, "all"),
    queryKey: ["asks", issueKey],
    staleTime: Number.POSITIVE_INFINITY,
  });
  const issueState = stateForIssue(state, issueKey);
  const events = useMemo(() => log.data?.pages.flat() ?? [], [log.data]);
  // When the oldest event read so far was made, while older pages remain: a thread that began
  // before it waits for its page (`HistoryInput.loadedFrom`).
  const older = log.hasNextPage;
  const loadedFrom = useMemo(
    () =>
      older
        ? events.reduce<(typeof events)[number] | undefined>(
            (oldest, event) => (oldest === undefined || event.seq < oldest.seq ? event : oldest),
            undefined
          )?.created_at
        : undefined,
    [events, older]
  );
  // The day the reader is in, read every render so a midnight rollover moves the `Today` label;
  // the model is rebuilt only when one of its reads changes.
  const today = dateKey(new Date().toISOString());
  const days = useMemo(
    () =>
      buildHistory({
        artifacts: issue.artifacts ?? [],
        // The outline's own ask input (`tree-drafts.ts`), so no turn is in both or neither.
        asks: [...(issue.open_asks ?? []), ...(asks.data ?? [])],
        comments: comments.data,
        events,
        lastReadSeq: visitMark,
        ...(loadedFrom === undefined ? {} : { loadedFrom }),
        messages: messages.data,
        today,
      }),
    [
      asks.data,
      comments.data,
      events,
      issue.artifacts,
      issue.open_asks,
      loadedFrom,
      messages.data,
      today,
      visitMark,
    ]
  );
  // The key each run of uploads is drawn under, held from one render to the next by its files'
  // uploads (`runKeys`), so a run a newer upload joins is drawn on, not drawn anew, and so is the
  // newer part of a run that upload splits.
  const runKeyOfUpload = useRef(new Map<number, string>());
  const drawnIds = useMemo(() => runKeys(days, runKeyOfUpload.current), [days]);
  // The days before today the reader opened; the day the address's entry is in is opened as
  // the address names it, and the day that was today as the date moves on as if the reader had
  // opened it; the reader's own press closes either again like any other.
  const [opened, setOpened] = useState<ReadonlySet<string>>(() => new Set());
  // The day the history last drew as today: the render that reads a new date opens it, and React
  // draws that render again with it opened before committing anything, so the day never folds
  // under the reader for a frame.
  const [drawnToday, setDrawnToday] = useState(today);
  if (drawnToday !== today) {
    setDrawnToday(today);
    setOpened(openDay(drawnToday));
  }
  const marked = item === undefined ? undefined : dayOf(days, item);
  useEffect(() => {
    if (marked === undefined) return;
    setOpened(openDay(marked));
  }, [marked]);
  // The address of a turn of a thread that began before the oldest event read so far names an
  // entry the history does not draw yet (`HistoryInput.loadedFrom`), so the log is read on, one
  // page at a time, until that thread is in or the log has ended, and the landing below then
  // marks the turn, opens its day and brings it on screen. It waits for the comments and
  // messages the threads are drawn from, before which no thread is drawn at all, and for every
  // read in flight, so each page is one request; a read that failed stops it, and its line says
  // so with its Retry.
  const seeksItem =
    item !== undefined &&
    marked === undefined &&
    comments.data !== undefined &&
    messages.data !== undefined &&
    log.hasNextPage &&
    !log.isFetching &&
    !log.isError;
  const { fetchNextPage } = log;
  useEffect(() => {
    if (seeksItem) void fetchNextPage();
  }, [fetchNextPage, seeksItem]);
  const togglePin = (eventId: number) =>
    writePin(queryClient, issueKey, (dismissed) => ({
      id: eventItemId({ id: eventId }),
      op: isPinnedEvent(dismissed, eventId) ? "unpin" : "pin",
    }));
  // The entry an address names is brought into view once, as the list first draws it: an entry
  // with no box yet - its day still folded, or Details still closed around the list, which the
  // page opens a commit after this one mounts - is not landed on, so the render that draws it
  // lands it. An entry taller than Details' view lands at the top of that view, so its head is
  // read first; a shorter one in the middle of it.
  const landed = useRef<string | undefined>(undefined);
  useEffect(() => {
    if (item === undefined || landed.current === item || marked === undefined) return;
    const entry = document.querySelector<HTMLElement>(`[data-history-item="${CSS.escape(item)}"]`);
    if (entry === null || entry.getClientRects().length === 0) return;
    landed.current = item;
    const box = scrollingAncestor(entry);
    const tall = box !== null && entry.getBoundingClientRect().height > box.clientHeight;
    entry.scrollIntoView({ block: tall ? "start" : "center" });
  });
  // `/activity` opens Details at the history: its heading is brought into Details' view
  // once for each arrival at that address, on the first render it has a box and the log under
  // it is drawn - Details still closed around it, or the panel not yet placed in the drawer,
  // gives it no box, and before the log is drawn Details has nothing to scroll the heading up
  // by - and leaving the address lets the next arrival bring it again. A reader who comes back
  // with the browser's Back or Forward to the history this list drew there before - Details left
  // open beside a view, or at a turn's address - finds Details where they left it, so that
  // arrival brings nothing. Only the box Details scrolls in moves, never the page under it; the
  // reader's own scroll after that is theirs. Once the heading has landed it says so
  // (`HISTORY_HEADING_LANDED_EVENT`).
  const navigation = useNavigationType();
  const heading = useRef<HTMLHeadingElement>(null);
  const headingLanded = useRef<string | undefined>(undefined);
  const landedBefore = useRef(false);
  useEffect(() => {
    if (activityAddress === undefined) {
      headingLanded.current = undefined;
      return;
    }
    const element = heading.current;
    if (
      headingLanded.current === activityAddress ||
      element === null ||
      element.getClientRects().length === 0 ||
      log.data === undefined
    ) {
      return;
    }
    headingLanded.current = activityAddress;
    const comingBack = navigation === "POP" && landedBefore.current;
    landedBefore.current = true;
    if (comingBack) return;
    const box = scrollingAncestor(element);
    if (box === null) return;
    scrollElementToTop(element, box);
    element.dispatchEvent(new CustomEvent(HISTORY_HEADING_LANDED_EVENT, { bubbles: true }));
  });
  // The history is newest first, so every event the feed prepends lands above a reader who is
  // down in it: the anchor holds the entry they are on where it was, in the box it scrolls in -
  // Details' own, or the document outside the shell. It follows each entry's own id
  // (`data-history-entry`), not its `data-event-seq`: a thread whose first turn's event is on a
  // page of the log not yet read carries seq 0 until that page lands, beside every other such
  // thread, so its seq names neither one entry nor the same entry before and after `Load older`.
  // That box is found once the list is drawn; moving the panel to another host remounts it.
  const anchorRoot = useRef<HTMLElement | null>(null);
  const scroller = useRef<HTMLElement | null>(null);
  const listDrawn = log.data !== undefined;
  useLayoutEffect(() => {
    if (listDrawn) scroller.current = scrollingAncestor(anchorRoot.current);
  }, [listDrawn]);
  // A reader whose focus is on another control of the box the history scrolls in - a chord
  // left it on the collaborators, a field of Details is being edited - is reading that control,
  // not the history: the history holds no place then, or holding one would scroll the box off
  // what the reader is on as the history fills in beneath it.
  const holdsPlace = () => {
    const active = document.activeElement;
    const box = scroller.current;
    return (
      box === null ||
      active === null ||
      !box.contains(active) ||
      anchorRoot.current?.closest("section")?.contains(active) === true
    );
  };
  // A control of the history that goes away under the reader's focus leaves the focus in the
  // history, never on the page, as Details' Unpin does (`IssuePins.tsx`). Load older, which the
  // log's end takes away, hands it on to a control the page it read added: first one the reader
  // can see, one under the list drawn before the press before one above it, as the page lands
  // where Load older stood; else one out of view under that list; else any the page added;
  // else Load older again, where pages are left; else the last control drawn before the press,
  // which stood just above Load older, for a last page that adds no control (its events go into
  // a day drawn folded, or are all events the outline draws); and only then the heading, above
  // the whole list. The Retry of a failed next page, which stands where Load older stood, hands
  // it on the same way; the Retry of a failed read of the log, gone once the read lands, hands
  // it to the heading. None of them is disabled while it reads, since Chromium drops the focus
  // of a focused button that turns disabled to the page, so the focus stays on the control
  // pressed while its read is out, and a press of it then joins that read rather than ending it
  // and sending another (`readOlderPage` for a page, `cancelRefetch: false` for the log). Each
  // press, made with the focus on its control, is held until the reads settle, and a press of
  // the control already held keeps the first press's hold, the controls drawn before it with it,
  // so the press that joins changes nothing of the hand-on; then a focus left on the page moves,
  // and so does one still on that control once its read has landed - Load older where pages are
  // left hands it on as if it had gone - while a read that failed leaves it there, beside the
  // line that says so. A reader who has put the focus elsewhere by then keeps it there. No such
  // move scrolls Details, whose reader keeps their place: from the press until its reads settle,
  // Details keeps the place of the entry next to the pressed control (`keepEntryBeside`), so the
  // page that lands, and a cut of the turns above it in the same commit, leave that entry where
  // the reader saw it, and a control the page pushed out of view - under files it added to a run
  // above - takes the focus only when none is in view. The entry the focus moved to is then the
  // place Details holds as the entries above it change height (`ViewportAnchor` records the
  // place at every focus moved into its rows).
  const pressed = useRef<
    | {
        readonly before: ReadonlySet<HTMLElement>;
        readonly control: Element;
        readonly kind: "older";
      }
    | { readonly control: Element; readonly kind: "retry" }
  >(undefined);
  const loadOlder = useRef<HTMLButtonElement>(null);
  const failure = useRef<HTMLDivElement>(null);
  const controlsOfList = () => [
    ...(anchorRoot.current?.querySelectorAll<HTMLElement>("a[href], button:not(:disabled)") ?? []),
  ];
  // The place Details keeps from a press of Load older or of either Retry that sends a read,
  // made with the focus on its control or not, until the reads it sent settle: that of the
  // entry next to the control, the list's last under Load older and the next page's Retry, its
  // first above the log's Retry. The control is outside the entries, so while the focus is on
  // it no act of the reader's names a place, and a page that lands in the same commit as a cut
  // of the turns above it would take the entries under that cut, the new ones with them, up out
  // of Details' view.
  const anchor = useRef<ViewportAnchor>(null);
  const keeping = useRef(false);
  const keepEntryBeside = (control: "above" | "under") => {
    const entries = anchorRoot.current?.querySelectorAll<HTMLElement>(
      `[${HISTORY_ENTRY_ATTRIBUTE}]`
    );
    keeping.current = true;
    anchor.current?.keepRow(
      entries === undefined ? null : entries.item(control === "under" ? entries.length - 1 : 0)
    );
  };
  // The reader's own read of the next page while it is out, from a press of Load older or of the
  // next page's Retry: a press made meanwhile joins it and sends nothing, where the render's
  // `isFetchingNextPage` can be older than a second press made before React draws the first
  // one's read. It is marked here, at the press, and not joined by `cancelRefetch: false`, which
  // joins whatever read of the log is out: a refetch a live event sent, common on a busy issue,
  // would take the press and read no page. A press with no read of its own out reads the next
  // page as `fetchNextPage` does by default, ending a refetch that is out.
  const olderRead = useRef<Promise<unknown> | null>(null);
  const readOlderPage = () => {
    if (olderRead.current !== null) return;
    olderRead.current = log.fetchNextPage().finally(() => {
      olderRead.current = null;
    });
  };
  useLayoutEffect(() => {
    const press = pressed.current;
    if (press === undefined || log.isFetching) return;
    pressed.current = undefined;
    const active = document.activeElement;
    const onPage = active === null || active === document.body;
    const onPressedAfterRead = active === press.control && !log.isError;
    if (!onPage && !onPressedAfterRead) return;
    // The page it read lands under every control drawn before it, where Load older stood. Its
    // entries can push one another out of Details' view: files the page added to a run above
    // move the entries under that run down, so a new control is in view or not by where it is
    // drawn once the entry next to the pressed control is back where it stood: the place kept
    // from the press (`keepEntryBeside`), which `ViewportAnchor` restored in this commit before
    // this runs.
    const before = press.kind === "older" ? press.before : new Set<HTMLElement>();
    const lastBefore = [...before].filter((control) => control.isConnected).at(-1);
    const added = controlsOfList().filter((control) => !before.has(control));
    const isBelow = (control: HTMLElement) =>
      lastBefore === undefined ||
      (lastBefore.compareDocumentPosition(control) & Node.DOCUMENT_POSITION_FOLLOWING) !== 0;
    const view = scroller.current?.getBoundingClientRect() ?? {
      bottom: window.innerHeight,
      top: 0,
    };
    const inView = (control: HTMLElement) => {
      const at = control.getBoundingClientRect();
      return at.height > 0 && at.top >= view.top && at.bottom <= view.bottom;
    };
    const target =
      press.kind === "older"
        ? (added.find((control) => isBelow(control) && inView(control)) ??
          added.find(inView) ??
          added.find(isBelow) ??
          added[0] ??
          loadOlder.current ??
          lastBefore)
        : failure.current?.querySelector("button");
    (target ?? heading.current)?.focus({ preventScroll: true });
  });
  // The place kept from the press goes once the reads it sent have settled, after the hand-on,
  // whatever that did: the focus it moved into the entries is the reader's place from then on,
  // which the anchor recorded as the focus moved and leaves alone here, and a focus it left
  // where the reader put it names no place in the history.
  useLayoutEffect(() => {
    if (!keeping.current || log.isFetching) return;
    keeping.current = false;
    anchor.current?.keepRow(null);
  });
  // The history draws the log it holds whatever the last read's outcome: a read
  // that fails once it holds the log - a refetch after an event, a later open of Details, the
  // next page - says so with its Retry beside the entries, never in their place, and the line
  // stands alone only while no page is held. A failed next page is said where Load older is,
  // and its Retry reads that page again; any other failure under the heading, where its Retry
  // reads the log again. The line stays while the read its Retry sent is out, even where no page
  // is held: a fetch of a log that holds none clears its failure and puts it back to pending as
  // it starts, which would take the line, and its Retry with the focus, away at the press. A
  // failure it counts (`errorUpdateCount`) is then one it still stands in. Each failure is drawn
  // afresh by that count, so a read that fails again is said again, in the same words.
  const logFailed = log.isError || (log.isPending && log.errorUpdateCount > 0);
  const failed = logFailed ? (
    <div ref={failure}>
      <DetailsErrorLine
        error={log.error}
        failure={log.errorUpdateCount}
        message={ANSWER_LINES.history_unreadable}
        onRetry={() => {
          // The next page's Retry stands where Load older stood, and hands the focus on as it
          // does; the log's Retry, under the heading, to the heading. Neither is disabled while
          // it reads, since Chromium drops the focus of a focused button that turns disabled
          // to the page, so a read that fails again leaves the focus on it; it reads busy then,
          // and a press while the log is read joins that read rather than ending it and sending
          // another: the next page's as `readOlderPage` says, the log's by `cancelRefetch: false`,
          // which the query decides as the press lands, as the render's `isFetching` can be older
          // than a second press made before React draws the first one's read.
          const active = document.activeElement;
          if (
            active !== null &&
            failure.current?.contains(active) === true &&
            pressed.current?.control !== active
          ) {
            pressed.current = log.isFetchNextPageError
              ? { before: new Set(controlsOfList()), control: active, kind: "older" }
              : { control: active, kind: "retry" };
          }
          keepEntryBeside(log.isFetchNextPageError ? "under" : "above");
          if (log.isFetchNextPageError) readOlderPage();
          else void log.refetch({ cancelRefetch: false });
        }}
        retrying={log.isFetching}
      />
    </div>
  ) : null;

  return (
    <section
      aria-label={CONTROL_LABELS.history}
      className="flex min-w-0 flex-col gap-1 outline-none"
      data-testid="issue-history"
      tabIndex={-1}
    >
      <h2
        className={`text-sm font-semibold outline-none ${pageInkText}`}
        ref={heading}
        tabIndex={-1}
        {...pageChrome}
      >
        {CONTROL_LABELS.history}
      </h2>
      {log.isFetchNextPageError ? null : failed}
      {log.isPending && !logFailed ? (
        <p className={`text-xs ${pageQuietText}`} {...pageChrome}>
          {ANSWER_LINES.loading_history}
        </p>
      ) : log.data === undefined ? null : (
        <ViewportAnchor
          enabled={holdsPlace}
          item={HISTORY_ENTRY_ATTRIBUTE}
          ownsScrollAnchoring
          ref={anchor}
          rootRef={anchorRoot}
          scroller={scroller}
        >
          <ol
            aria-label={CONTROL_LABELS.history_list}
            className="flex min-w-0 flex-col gap-1"
            {...authorText}
          >
            {days.map((day) => {
              const open = day.today || opened.has(day.date);
              const count = day.entries.filter((entry) => entry.kind !== "unread").length;
              return (
                <li className="flex min-w-0 flex-col" data-history-day={day.date} key={day.date}>
                  {day.today ? (
                    <p className={DAY_LINE}>{day.label}</p>
                  ) : (
                    <button
                      aria-expanded={open}
                      className={`dispatch-inline-link self-start ${DAY_LINE} ${pageRowAct}`}
                      onClick={() =>
                        setOpened((held) => {
                          const next = new Set(held);
                          if (!next.delete(day.date)) next.add(day.date);
                          return next;
                        })
                      }
                      type="button"
                    >
                      {day.label}
                      {/* The day the pages read so far end in holds entries not read yet
                          (`HistoryDay.partial`): its name alone, which still opens it. */}
                      {day.partial ? null : (
                        <>
                          {" "}
                          <span className={`font-medium ${pageQuietText}`}>· {count}</span>
                        </>
                      )}
                    </button>
                  )}
                  {open ? (
                    <ol className="flex min-w-0 flex-col">
                      {day.entries.map((entry) => {
                        const drawnId = drawnIds.get(entry.id) ?? entry.id;
                        return (
                          <Entry
                            drawnId={drawnId}
                            entry={entry}
                            issueKey={issueKey}
                            item={item}
                            key={drawnId}
                            onPin={togglePin}
                            pinned={(id) => isPinnedEvent(issueState.dismissed ?? [], id)}
                          />
                        );
                      })}
                    </ol>
                  ) : null}
                </li>
              );
            })}
          </ol>
        </ViewportAnchor>
      )}
      {log.isFetchNextPageError ? failed : null}
      {log.hasNextPage ? (
        <button
          aria-busy={log.isFetchingNextPage || undefined}
          className={`dispatch-inline-link self-start text-xs font-medium underline ${pageMutedText}`}
          onClick={(event) => {
            // Not disabled while its page is read, since Chromium drops the focus of a focused
            // button that turns disabled to the page: a press then joins the reader's own read of
            // that page (`readOlderPage`), and keeps the first press's hold.
            const control = event.currentTarget;
            if (document.activeElement === control && pressed.current?.control !== control) {
              pressed.current = { before: new Set(controlsOfList()), control, kind: "older" };
            }
            keepEntryBeside("under");
            readOlderPage();
          }}
          ref={loadOlder}
          type="button"
          {...pageChrome}
        >
          {log.isFetchingNextPage ? ANSWER_LINES.loading_older : CONTROL_LABELS.load_older}
        </button>
      ) : null}
    </section>
  );
}

/** The nearest box above `element` that scrolls its content, or null where the document does. */
function scrollingAncestor(element: HTMLElement | null): HTMLElement | null {
  for (
    let node = element?.parentElement;
    node && node !== document.body;
    node = node.parentElement
  ) {
    const overflow = getComputedStyle(node).overflowY;
    if (overflow === "auto" || overflow === "scroll") return node;
  }
  return null;
}

/** The day the entry `item` names is in: a thread's turn, root or reply. */
function dayOf(days: readonly HistoryDay[], item: string): string | undefined {
  return days.find((day) =>
    day.entries.some(
      (entry) =>
        entry.kind === "thread" &&
        (entry.root.id === item || entry.replies.some((reply) => reply.id === item))
    )
  )?.date;
}

/** The update of the days opened that opens `day` too. A day already open leaves the set it was
 *  handed as it is, so the history does not draw again for it. */
function openDay(day: string): (held: ReadonlySet<string>) => ReadonlySet<string> {
  return (held) => (held.has(day) ? held : new Set(held).add(day));
}

/**
 * The key each run of uploads among `days` is drawn under, by the run's id. The model names a
 * run by its newest upload (`HistoryEntry`), so a newer upload that joins a run renames it;
 * drawn under that name, the run would be mounted anew, and a focus on one of its files' links
 * or Pins would fall to the page. So a run keeps the key it was first drawn under: the key one
 * of its files was drawn under (`held`, by each file's upload) unless another run here took it,
 * else its own id, with a count where that is taken too. A newer upload can split a run drawn
 * before (`buildHistory`): the runs are read newest first, so the part that holds the newer
 * upload keeps the run's key, and the files split off, more than a minute older than that
 * upload, are drawn anew under their own run's id, which can be the very key the newer part
 * kept. Every file of the run is then held under its key.
 */
function runKeys(
  days: readonly HistoryDay[],
  held: Map<number, string>
): ReadonlyMap<string, string> {
  const keys = new Map<string, string>();
  const taken = new Set<string>();
  for (const day of days) {
    for (const entry of day.entries) {
      if (entry.kind !== "uploads") continue;
      let key = entry.id;
      for (const file of entry.files) {
        const kept = held.get(file.id);
        if (kept !== undefined && !taken.has(kept)) {
          key = kept;
          break;
        }
      }
      for (let count = 1; taken.has(key); count += 1) key = `${entry.id}:${count}`;
      taken.add(key);
      keys.set(entry.id, key);
      for (const file of entry.files) held.set(file.id, key);
    }
  }
  return keys;
}

/** One entry of the history (`HistoryEntry`). */
function Entry({
  drawnId,
  entry,
  issueKey,
  item,
  onPin,
  pinned,
}: {
  /** The id the entry is drawn under: its own, or a run's key (`runKeys`). */
  drawnId: string;
  entry: HistoryEntry;
  issueKey: string;
  item: string | undefined;
  onPin: (eventId: number) => void;
  pinned: (eventId: number) => boolean;
}): ReactNode {
  if (entry.kind === "unread") {
    return (
      <li
        aria-label={CONTROL_LABELS.history_unread}
        className={`my-1 flex items-center gap-3 text-xs leading-[18px] font-semibold ${pageInkText}`}
        {...pageChrome}
      >
        <span className={`h-px flex-1 ${pageHairBg}`} />
        {CONTROL_LABELS.history_unread}
        <span className={`h-px flex-1 ${pageHairBg}`} />
      </li>
    );
  }
  // What a dated entry opens with (`.hev`): when, then who, then what they did.
  const head = (
    <>
      <Timestamp at={entry.at} className={TIME} />
      <span className={AUTHOR}>
        <AgentName actor={entry.author} inline />
      </span>{" "}
    </>
  );
  if (entry.kind === "event") {
    const isPinned = pinned(entry.event.id);
    return (
      <li
        className={`${ENTRY} flex min-w-0 flex-wrap items-baseline gap-x-2`}
        data-event-seq={entry.seq}
        {...{ [HISTORY_ENTRY_ATTRIBUTE]: drawnId }}
      >
        <span className="min-w-40 flex-1 break-words">
          {head}
          {/* `main`'s line links a document's name in the application's link blue, which
              Details draws in no line: those two lines are the history's own. */}
          {entry.event.type === "artifact.created" || entry.event.type === "artifact.renamed" ? (
            <DocumentLine artifact={entry.event.payload.artifact} description={entry.description} />
          ) : (
            <ActivityLine description={entry.description} event={entry.event} issueKey={issueKey} />
          )}
        </span>
        <PinButton
          label={isPinned ? CONTROL_LABELS.unpin : CONTROL_LABELS.pin}
          onClick={() => onPin(entry.event.id)}
          pinned={isPinned}
          quiet
        />
        {entry.event.type === "handoff.recorded" ? (
          <HistoryHandoff handoffId={entry.event.payload.handoff_id} />
        ) : null}
      </li>
    );
  }
  // Each file of a run is one event's upload, so each has that event's Pin and Unpin, as every
  // line of the history has its event's: the run's line says who uploaded, and each file
  // stands under it on a line of its own, beside its pin, as an event's words stand beside theirs.
  // Each file is an entry the reader's place is held by (`ViewportAnchor`), so an older page
  // that adds files under the one they read leaves it where it was.
  if (entry.kind === "uploads") {
    return (
      <li
        className={`${ENTRY} flex min-w-0 flex-col`}
        data-event-seq={entry.seq}
        {...{ [HISTORY_ENTRY_ATTRIBUTE]: drawnId }}
        data-history-uploads={entry.files.length}
      >
        <span className="min-w-0 break-words">
          {head}
          <span {...pageChrome}>{CONTROL_LABELS.history_uploaded}</span>
        </span>
        <ul className="flex min-w-0 flex-col">
          {entry.files.map((file) => {
            const isPinned = pinned(file.id);
            return (
              <li
                className="flex min-w-0 items-center gap-x-2"
                {...{ [HISTORY_ENTRY_ATTRIBUTE]: `file:${file.id}` }}
                key={file.id}
              >
                <span className="min-w-0 flex-1 break-words">
                  {file.slug === undefined ? (
                    file.name
                  ) : (
                    <Link
                      className={`dispatch-inline-link ${ROW_LINK} ${pageInkText}`}
                      to={buildIssuePath({ key: issueKey, kind: "artifact", slug: file.slug })}
                    >
                      {file.name}
                    </Link>
                  )}
                </span>
                <PinButton
                  label={isPinned ? CONTROL_LABELS.unpin : CONTROL_LABELS.pin}
                  onClick={() => onPin(file.id)}
                  pinned={isPinned}
                  quiet
                />
              </li>
            );
          })}
        </ul>
      </li>
    );
  }
  return (
    <li
      className="flex min-w-0 flex-col"
      data-event-seq={entry.seq}
      {...{ [HISTORY_ENTRY_ATTRIBUTE]: drawnId }}
    >
      <Turn issueKey={issueKey} marked={item === entry.root.id} turn={entry.root} />
      {entry.replies.length === 0 ? null : (
        <ol className="ml-3.5 flex min-w-0 flex-col">
          {entry.replies.map((reply) => (
            <li key={reply.id}>
              <Turn issueKey={issueKey} marked={item === reply.id} turn={reply} />
            </li>
          ))}
        </ol>
      )}
    </li>
  );
}

/** The line of a document added or renamed, as `ActivityLine` draws it - its words, with the
 *  last reading of the document's name a link to its page, the new title of a rename, whose slug
 *  does not move - in the page's ink and its rows' underline, as an upload's file is. */
function DocumentLine({
  artifact,
  description,
}: {
  artifact: Pick<
    Artifact,
    "issue_key" | "kind" | "name" | "primary" | "project" | "session_id" | "slug"
  >;
  description: readonly AgentPart[];
}): ReactNode {
  const name = artifact.name;
  const linked = description.findIndex((part) => typeof part === "string" && part.includes(name));
  return (
    <span>
      <AgentParts
        parts={description}
        text={(part, index) => {
          if (index !== linked) return part;
          const at = part.lastIndexOf(name);
          return (
            <>
              {part.slice(0, at)}
              <Link
                className={`dispatch-inline-link ${ROW_LINK} ${pageInkText}`}
                to={buildReferencePath(documentRoute(artifact))}
              >
                {name}
              </Link>
              {part.slice(at + name.length)}
            </>
          );
        }}
      />
    </span>
  );
}

/** One turn of a thread agents wrote among themselves: who, when, the words, and Reply. */
function Turn({
  issueKey,
  marked,
  turn,
}: {
  issueKey: string;
  marked: boolean;
  turn: HistoryTurn;
}): ReactNode {
  const writer = useOutlineWriter();
  const drawer = useMarginDrawer();
  const phone = useMediaQuery(PHONE_VIEWPORT_QUERY);
  return (
    <article
      aria-current={marked ? "true" : undefined}
      className={`${ENTRY} flex min-w-0 flex-col ${marked ? `rounded-r-md ${pageMarkBg}` : ""}`}
      data-history-item={turn.id}
      data-history-kind={turn.kind}
    >
      <p className="min-w-0 break-words">
        <Link
          className={`dispatch-inline-link ${TIME} ${pageRowAct}`}
          data-history-time=""
          to={buildIssuePath({ id: turn.id, key: issueKey, kind: turn.kind })}
          {...{ [KEEPS_DETAILS_ATTRIBUTE]: "" }}
        >
          <Timestamp at={turn.at} />
        </Link>
        <span className={AUTHOR}>
          <AgentName actor={turn.author} inline />
        </span>
      </p>
      {/* Cut as the outline cuts an agent's words, and whole where the address
          names this turn. Details is reached by Tab, so its controls keep their tab stops. */}
      <CutWords className={WORDS} tabStops whole={marked}>
        <MarkdownBody
          markdown={turn.body}
          source={textSourceOf(turn.kind, turn.id)}
          tree={turn.bodyTree}
        />
      </CutWords>
      {writer === undefined ? null : (
        <p>
          <button
            className={`dispatch-inline-link text-xs font-medium ${pageMutedText} ${pageRowAct}`}
            data-history-reply={turn.id}
            onClick={() => {
              // On a phone Details covers the outline whose foot the line is at, so it closes
              // for the reader to write there.
              if (phone) drawer?.setOpen(false);
              writer.replyTo({
                author: turn.author,
                id: turn.id,
                kind: turn.kind,
                text: turn.body,
              });
            }}
            type="button"
            {...pageChrome}
          >
            {CONTROL_LABELS.reply}
          </button>
        </p>
      )}
    </article>
  );
}
