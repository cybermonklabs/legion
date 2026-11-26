import {
  replaceEqualDeep,
  useInfiniteQuery,
  useQuery,
  useQueryClient,
} from "@tanstack/react-query";
import { type ReactNode, useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import { useNavigationType } from "react-router-dom";

import { api } from "../../api/client";
import { type EventPages, mergeEventPages } from "../../api/event-feed";
import { issueResultsQuery } from "../../api/queries";
import type { IssueDetails, UserState } from "../../api/types";
import { pageInkText, pageMutedText, pageQuietText } from "../../theme/classes";
import { dateKey } from "../activity/activity-model";
import { scrollElementToTop } from "../doc/scroll-block";
import { writePin } from "../issue/pin-write";
import { eventItemId, isPinnedEvent, stateForIssue } from "../issue/pins";
import { HISTORY_ENTRY_ATTRIBUTE } from "../margin/details-focus-keeper";
import { EVENT_FRESH, standsFailed } from "../refs/reference-target";
import { overlaps, ViewportAnchor } from "../shell/ViewportAnchor";
import { DetailsErrorLine, retryRead } from "./details-error";
import { ENTRY_CONTROLS, SHOW_MORE_CONTROL, ShowMoreWords } from "./details-leaves";
import { Entry, runKeys } from "./history-entries";
import {
  buildHistory,
  comparePlaces,
  HISTORY_PAGE,
  type HistoryPlace,
  historyBlocks,
  historyDownTo,
  placeUnder,
  turnPlace,
} from "./history-model";
import { authorText, pageChrome } from "./page-structure";
import { ANSWER_LINES, CONTROL_LABELS } from "./page-words";
import { nearestScroll, scrollingAncestor } from "./scroll-box";
import { abortOnReaderAct } from "./useNodeEditing";

/** How many events a page of the log holds, as the server answers it. */
const PAGE = 200;

/** The event the history's heading raises, bubbling, once a following of `/activity`
 *  (`useFollowing`) has brought it to the top of Details' view. That scroll is the landing that
 *  following asked for, not a move that takes the reader from their place: the hold of the place
 *  `t v` lands the focus at keeps the focused history where this scroll leaves it
 *  (`details-more.tsx`). */
export const HISTORY_HEADING_LANDED_EVENT = "history-heading-landed";

/** A day's line, the prototype's `.hdl`: 12 px on 18 px at weight 600 in the muted grey, 5 px
 *  above and below, its words as written. */
const DAY_LINE = `py-[5px] text-xs leading-[18px] font-semibold ${pageMutedText}`;

/** A turn of a thread the history holds that an address names (`/issues/<key>/comments/<id>` or
 *  `/messages/<id>`), with the following of that address it was named at (`useFollowing`). */
export interface AddressedTurn {
  readonly following: string;
  readonly id: string;
}

/**
 * The issue's history, a list in the panel (`history-model.ts`): newest first, every day drawn
 * open under a line that names it, which is no control. It draws its first `HISTORY_PAGE`
 * entries, and one control under the last entry drawn draws the next ones (Show more). An event
 * is one line with its pin, and a handoff's line holds its fields, open until the reader folds
 * them (`HistoryHandoff`); a run of uploads - one author's newest upload and their uploads made at
 * most a minute before it, with no other author's upload between them - is one entry that opens
 * each file, each with its own upload's pin, drawn under one key as the run grows or a newer
 * upload splits it (`runKeys`); a thread the history holds (`agents-only.ts`: a thread the Legion
 * daemon began, whoever wrote in it, or an agents' post tied to no text) is one entry drawn
 * whole, each turn's words whole at any length, with the marks of a post its author corrected
 * and with its Reply, which sets the line at the foot of the outline to reply to it. Once a person
 * has written in an agents' post, the outline draws it and this list no longer does.
 *
 * `/activity`, `t v` and the address of a turn of such a thread open Details here:
 * at `/activity` the heading is brought into Details' view once for each following of that
 * address (`useFollowing`), `t v` pressed there among them, except a reader's coming back to the
 * history they left drawn, who finds it where they left it;
 * `item` is the turn such an address names, which is marked, drawn with every entry above its
 * thread, and brought into view once for each following of its address. It writes no read mark.
 * The line that says what is new is drawn at `visitMark`,
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
  activityFollowing,
  issue,
  item,
  state,
  visitMark,
}: {
  /** The following of `/activity` the reader is on (`useFollowing`), or undefined at any other
   *  address: each following of `/activity` brings the history's heading into Details' view
   *  once. */
  activityFollowing: string | undefined;
  issue: IssueDetails;
  /** The turn the address names, which the list marks, draws and brings into view once for each
   *  following of that address. */
  item: AddressedTurn | undefined;
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
  // Whether the log holds a refused check's line, the one line that names a check's block.
  const refusesCheck = events.some(
    (event) => event.type === "block.invalid" && event.payload.block_type === "check"
  );
  // The outline's own read of the issue's checks, under its key and with its options
  // (`useIssueTree.ts`), so the cache answers it wherever the outline has read it: a refused
  // check's line names the check's block by the check's words instead of its id
  // (`historyBlocks`). A history that holds no such line sends no read of it.
  const results = useQuery({
    ...issueResultsQuery(issueKey),
    ...EVENT_FRESH,
    enabled: refusesCheck,
  });
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
  const days = useMemo(() => {
    // The outline's own ask input (`tree-drafts.ts`), so no turn is in both or neither.
    const issueAsks = [...(issue.open_asks ?? []), ...(asks.data ?? [])];
    // The blocks are known once every read they come from has answered: the asks read, and the
    // results read where a line names a check's block. A read that failed before it answered
    // holds no answer, so until it does no line says that a block is gone.
    const answered = asks.data !== undefined && (!refusesCheck || results.data !== undefined);
    return buildHistory({
      artifacts: issue.artifacts ?? [],
      asks: issueAsks,
      blocks: answered ? historyBlocks(issueKey, issueAsks, results.data?.checks ?? []) : undefined,
      comments: comments.data,
      events,
      lastReadSeq: visitMark,
      ...(loadedFrom === undefined ? {} : { loadedFrom }),
      messages: messages.data,
      today,
    });
  }, [
    asks.data,
    comments.data,
    events,
    issue.artifacts,
    issue.open_asks,
    issueKey,
    loadedFrom,
    messages.data,
    refusesCheck,
    results.data,
    today,
    visitMark,
  ]);
  // The key each run of uploads is drawn under, held from one render to the next by its files'
  // uploads (`runKeys`), so a run a newer upload joins is drawn on, not drawn anew, and so is the
  // newer part of a run that upload splits.
  const runKeyOfUpload = useRef(new Map<number, string>());
  const drawnIds = useMemo(() => runKeys(days, runKeyOfUpload.current), [days]);
  // How far down the history is drawn (`historyDownTo`): to the place of the last entry drawn
  // (`HistoryPlace`), so an entry that arrives above it is drawn and takes no entry drawn out from
  // under the reader. Until that place is held, the first `HISTORY_PAGE` entries are drawn. It is
  // held from the render in which the log and every read the threads come from have answered, so
  // that a thread those reads bring stands among the first entries rather than above them.
  const [end, setEnd] = useState<HistoryPlace | undefined>(undefined);
  const firstEnd = useMemo(() => placeUnder(days, undefined, HISTORY_PAGE), [days]);
  const shownEnd = end ?? firstEnd;
  const settled =
    log.data !== undefined &&
    comments.status !== "pending" &&
    messages.status !== "pending" &&
    asks.status !== "pending";
  // How many pages of the log were held when a press of the control, or of the next page's
  // Retry in its place, asked for the next one: once that page lands, the next entries are drawn
  // from it.
  const pages = log.data?.pages.length ?? 0;
  const [asked, setAsked] = useState<number | undefined>(undefined);
  // The entry of the thread that holds the turn the address names, which is drawn however far
  // under the entries drawn it stands, with every entry above it.
  const itemId = item?.id;
  const itemPlace = useMemo(
    () => (itemId === undefined ? undefined : turnPlace(days, itemId)),
    [days, itemId]
  );
  // Each move of the place is drawn in the render that finds it, before React commits anything.
  let nextEnd = end;
  if (asked !== undefined && pages > asked) {
    setAsked(undefined);
    nextEnd = placeUnder(days, shownEnd, HISTORY_PAGE);
  } else if (
    itemPlace !== undefined &&
    (shownEnd === undefined || comparePlaces(itemPlace, shownEnd) < 0)
  ) {
    nextEnd = itemPlace;
  } else if (end === undefined && settled) {
    nextEnd = shownEnd;
  }
  if (nextEnd !== end) setEnd(nextEnd);
  const drawn = useMemo(() => historyDownTo(days, shownEnd), [days, shownEnd]);
  // The address of a turn of a thread that began before the oldest event read so far names an
  // entry the history does not hold yet (`HistoryInput.loadedFrom`), so the log is read on, one
  // page at a time, until that thread is in or the log has ended; the history is then drawn down
  // to that thread, and the landing below marks the turn and brings it on screen. It waits for
  // the comments and messages the threads are drawn from, before which no thread is drawn at all,
  // and for every read in flight, so each page is one request. A read that failed stops it: a
  // failed next page says so with its Retry, and a refetch that failed over the pages held draws
  // no line, which leaves the control under the entries to read on.
  const seeksItem =
    item !== undefined &&
    itemPlace === undefined &&
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
  // The turn an address names is brought into view once for each following of that address
  // (`AddressedTurn`), as the list first draws it there: the same address followed again, a
  // link to it pressed in the history among them, brings it into view again however far the
  // reader has read since. A turn with no box yet - not drawn yet, or Details still closed around
  // the list, which the page opens a commit after this one mounts - is not landed on, so the
  // render that draws it lands it. A turn taller than Details' view lands at the top of that
  // view, so its head is read first; a shorter one in the middle of it. The history's anchor
  // keeps the landed turn where this scroll left it, from the landing on: its own place, inside
  // the entry of the history it stands in (`keepRow`'s `element`). A change of height in the same
  // frame, whether the scroll's event is read before it or after it, then moves Details by as
  // much rather than moving the turn, until the reader's next act names their place. That holds
  // for a change inside the entry too, such as a turn above a reply whose words finish drawing
  // after the scroll, which leaves the entry's top where it was and moves the reply. The place
  // the reader's hand was on before the landing is often wholly outside the view after it, and
  // is no place they can see (`rowToKeep`).
  const landed = useRef<string | undefined>(undefined);
  // The history's anchor, which keeps the reader's place in Details (`ViewportAnchor` below).
  const anchor = useRef<ViewportAnchor>(null);
  useEffect(() => {
    if (item === undefined || landed.current === item.following) return;
    const entry = document.querySelector<HTMLElement>(
      `[data-history-item="${CSS.escape(item.id)}"]`
    );
    if (entry === null || entry.getClientRects().length === 0) return;
    landed.current = item.following;
    const box = scrollingAncestor(entry);
    const tall = box !== null && entry.getBoundingClientRect().height > box.clientHeight;
    entry.scrollIntoView({ block: tall ? "start" : "center" });
    anchor.current?.keepRow(entry.closest<HTMLElement>(`[${HISTORY_ENTRY_ATTRIBUTE}]`), entry);
  });
  // `/activity` opens Details at the history: its heading is brought into Details' view once for
  // each following of that address (`useFollowing`), on the first render it has a box and the log
  // under it is drawn - Details still closed around it, or the panel not yet placed in the drawer,
  // gives it no box, and before the log is drawn Details has nothing to scroll the heading up by.
  // Each entry the page makes at the address is a following of its own - `t v` pressed there, a
  // link to it followed there - so each brings the heading back to the top of Details' view,
  // however far down Details the reader has read. A following the browser steps to itself (`POP`)
  // once this list has landed a heading brings nothing: a reader who comes back with Back or
  // Forward to the history this list drew there before - Details left open beside a view, or at a
  // turn's address - finds Details where they left it, and so does one who types a fragment at
  // the address. Only the box Details scrolls in moves, never the page under it; the reader's own
  // scroll after that is theirs. Once the heading has landed it says so
  // (`HISTORY_HEADING_LANDED_EVENT`).
  const navigation = useNavigationType();
  const heading = useRef<HTMLHeadingElement>(null);
  const headingLanded = useRef<string | undefined>(undefined);
  useEffect(() => {
    const element = heading.current;
    if (
      activityFollowing === undefined ||
      headingLanded.current === activityFollowing ||
      element === null ||
      element.getClientRects().length === 0 ||
      log.data === undefined
    ) {
      return;
    }
    const comingBack = navigation === "POP" && headingLanded.current !== undefined;
    headingLanded.current = activityFollowing;
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
  // thread, so its seq names neither one entry nor the same entry before and after that page.
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
  // The control under the last entry drawn, Show more, draws the next entries and moves the
  // focus to the first entry it added, as Show more under every other list of Details does
  // (`details-leaves.tsx`'s `useListPages`), whether the control stays or goes: to that entry's
  // first control, wherever it stands. Only a press that adds no entry - a page whose events are
  // all events the outline draws - hands the focus on otherwise: to a control the press added
  // that the reader can see, one under the entries drawn before the press before one above them,
  // as the next entries are drawn where the control stood; else any the press added; else the
  // control again, where entries or pages are left; else the last control drawn before the press,
  // which stood just above it; and only then the heading, above the whole list. A press that
  // reads the log's next page moves it once that page has landed and its first entries are drawn.
  // A control of the history that goes away under the reader's focus so leaves the focus in the
  // history, never on the page, as Details' Unpin does (`IssuePins.tsx`). The Retry of a failed
  // next page, which stands where the control stood, hands it on the same way; the Retry of a
  // failed read of the log, gone once the read lands, hands it to the heading. None of them is
  // disabled while it reads, since Chromium drops the focus of a focused button that turns
  // disabled to the page, so the focus stays on the control pressed while its read is out, and a
  // press of it then joins that read rather than ending it and sending another (`readOlderPage`
  // for a page, `retryRead` for the log). Each press, made with the focus on its control, is held
  // until what it asked for is drawn, and a press of the control already held keeps the first
  // press's hold, the controls drawn before it with it, so the press that joins changes nothing of
  // the hand-on; then a focus left on the page moves, and so does one still on that control once
  // what it asked for is drawn, while a read that failed leaves it there, beside the line that
  // says so. A reader who has put the focus elsewhere by then keeps it there. From the press until
  // its reads settle, Details keeps the place of the entry next to the pressed control
  // (`keepEntryBeside`), so the entries drawn under it leave that entry where the reader saw it.
  // The one move of Details a hand-on makes is to bring the element it gives the focus to into
  // Details' view, by the least scroll (`handTo`), and keep it wholly there as it grows
  // (`keepFocusedInView`): the first entry a press draws can stand below the view, as under a new
  // day's line, where a keyboard reader would not see the focus. The entry the focus moved to is
  // then the place Details holds as the entries above it change height (`ViewportAnchor` records
  // the place at every focus moved into its rows).
  const pressed = useRef<
    | {
        /** The controls and the entries drawn as the press was made. */
        readonly before: HeldParts;
        readonly control: Element;
        readonly kind: "more";
        /** Whether the press reads the log's next page, rather than draws entries read. */
        readonly reads: boolean;
      }
    | { readonly control: Element; readonly kind: "retry" }
  >(undefined);
  const more = useRef<HTMLButtonElement>(null);
  const failure = useRef<HTMLDivElement>(null);
  const controlsOfList = () => [
    ...(anchorRoot.current?.querySelectorAll<HTMLElement>(ENTRY_CONTROLS) ?? []),
  ];
  const entriesOfList = () => [
    ...(anchorRoot.current?.querySelectorAll<HTMLElement>(
      `[data-history-day] > ol > [${HISTORY_ENTRY_ATTRIBUTE}]`
    ) ?? []),
  ];
  const heldParts = (): HeldParts => ({
    controls: new Set(controlsOfList()),
    entries: new Set(entriesOfList()),
  });
  // The place Details keeps from a press of Show more or of either Retry that sends a read,
  // made with the focus on its control or not, until the reads it sent settle: that of the
  // entry next to the control, the list's last over Show more and the next page's Retry, its
  // first under the log's Retry. The control is outside the entries, so while the focus is on
  // it no act of the reader's names a place, and entries that grow above it as the next ones are
  // drawn would take the entries under them, the new ones with them, out of Details' view. The
  // history's anchor keeps it (`keepRow`).
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
  // The reader's own read of the next page while it is out, from a press of Show more or of the
  // next page's Retry: a press made meanwhile joins it and sends nothing, where the render's
  // `isFetchingNextPage` can be older than a second press made before React draws the first
  // one's read. It is marked here, at the press, and not joined by `cancelRefetch: false`, which
  // joins whatever read of the log is out: a refetch a live event sent, common on a busy issue,
  // would take the press and read no page. A press with no read of its own out reads the next
  // page as `fetchNextPage` does by default, ending a refetch that is out. Where every entry read
  // is drawn as the press is made, the next entries are drawn from that page once it lands
  // (`asked`).
  const olderRead = useRef<Promise<unknown> | null>(null);
  const readOlderPage = () => {
    if (drawn.left === 0) setAsked((held) => held ?? pages);
    if (olderRead.current !== null) return;
    olderRead.current = log.fetchNextPage().finally(() => {
      olderRead.current = null;
    });
  };
  // The keeping of the element the last hand-on gave the focus to in Details' view
  // (`keepFocusedInView`): the next hand-on ends it, and so does the history going.
  const focusKept = useRef<AbortController | null>(null);
  useEffect(() => () => focusKept.current?.abort(), []);
  useLayoutEffect(() => {
    const press = pressed.current;
    const reads = press?.kind === "retry" || press?.reads === true;
    if (press === undefined || (reads && log.isFetching)) return;
    pressed.current = undefined;
    const active = document.activeElement;
    const onPage = active === null || active === document.body;
    const onPressedAfterRead = active === press.control && !(reads && log.isError);
    if (!onPage && !onPressedAfterRead) return;
    // Gives `element` the focus, brought first into Details' view by the least scroll, the one
    // `block: "nearest"` makes (`nearestScroll`). The scroll is of Details' box alone, never of
    // the page under it, and the history's anchor takes it for its own (`scrollBox`), so its
    // event records no place. It comes before the focus, which then scrolls nothing
    // (`preventScroll`), so that the focus moving into the entries, which the anchor records as
    // the reader's place, is recorded where the reader sees it. The element can grow after the
    // focus, as a reference in an entry's line does once its target's name is read, so it is kept
    // wholly in Details' view by the same least scroll until the reader acts
    // (`keepFocusedInView`).
    const handTo = (element: HTMLElement) => {
      focusKept.current?.abort();
      focusKept.current = null;
      const box = scroller.current;
      if (box !== null) {
        const at = element.getBoundingClientRect();
        const view = box.getBoundingClientRect();
        const by = nearestScroll(at, view);
        if (by !== 0) anchor.current?.scrollBox(by);
      }
      element.focus({ preventScroll: true });
      if (box !== null) {
        focusKept.current = keepFocusedInView(element, box, (by) => anchor.current?.scrollBox(by));
      }
    };
    if (press.kind === "retry") {
      const retried = failure.current?.querySelector("button") ?? heading.current;
      if (retried !== null) handTo(retried);
      return;
    }
    // The entries it drew stand under every control drawn before it, where Show more stood, once
    // the entry next to the pressed control is back where it stood: the place kept from the press
    // (`keepEntryBeside`), which `ViewportAnchor` restored in this commit before this runs.
    const { controls, entries } = press.before;
    const lastBefore = [...controls].filter((control) => control.isConnected).at(-1);
    const isBelow = (element: HTMLElement) =>
      lastBefore === undefined ||
      (lastBefore.compareDocumentPosition(element) & Node.DOCUMENT_POSITION_FOLLOWING) !== 0;
    const first = entriesOfList()
      .find((entry) => !entries.has(entry) && isBelow(entry))
      ?.querySelector<HTMLElement>(ENTRY_CONTROLS);
    if (first != null) {
      handTo(first);
      return;
    }
    const added = controlsOfList().filter((control) => !controls.has(control));
    const view = scroller.current?.getBoundingClientRect() ?? {
      bottom: window.innerHeight,
      top: 0,
    };
    const inView = (control: HTMLElement) => {
      const at = control.getBoundingClientRect();
      return at.height > 0 && at.top >= view.top && at.bottom <= view.bottom;
    };
    const target =
      added.find((control) => isBelow(control) && inView(control)) ??
      added.find(inView) ??
      added.find(isBelow) ??
      added[0] ??
      more.current ??
      lastBefore ??
      heading.current;
    if (target !== null) handTo(target);
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
  // The history draws the log it holds whatever the last read's outcome. The log's own line
  // stands under the heading, in place of the loading line, while the log stands failed
  // (`standsFailed`). A failed next page is said where Show more is, below the pages held, and
  // its Retry reads that page again; that line stays while the page is read again, since the log
  // keeps that failure meanwhile.
  const logFailed = standsFailed(log);
  const saysFailure = logFailed || log.isFetchNextPageError;
  const failed = saysFailure ? (
    <div ref={failure}>
      <DetailsErrorLine
        error={log.error}
        failure={log.errorUpdateCount}
        message={ANSWER_LINES.history_unreadable}
        onRetry={() => {
          // The next page's Retry stands where Show more stood, and does its work and hands the
          // focus on as it does; the log's Retry, under the heading, to the heading. Neither is
          // disabled while it reads, since Chromium drops the focus of a focused button that
          // turns disabled to the page, so a read that fails again leaves the focus on it; it
          // reads busy then, and a press while the log is read joins that read rather than ending
          // it and sending another: the next page's as `readOlderPage` says, the log's by
          // `retryRead`, which the query decides as the press lands, as the render's
          // `isFetching` can be older than a second press made before React draws the first
          // one's read.
          const active = document.activeElement;
          if (
            active !== null &&
            failure.current?.contains(active) === true &&
            pressed.current?.control !== active
          ) {
            pressed.current = log.isFetchNextPageError
              ? { before: heldParts(), control: active, kind: "more", reads: true }
              : { control: active, kind: "retry" };
          }
          keepEntryBeside(log.isFetchNextPageError ? "under" : "above");
          if (log.isFetchNextPageError) readOlderPage();
          else void retryRead(log)();
        }}
        retrying={log.isFetching}
      />
    </div>
  ) : null;
  // Show more reads busy while the page its press asked for is read, the entries read all drawn.
  const readsPage = drawn.left === 0 && log.isFetchingNextPage;

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
            {drawn.days.map((day) => (
              <li className="flex min-w-0 flex-col" data-history-day={day.date} key={day.date}>
                <p className={DAY_LINE}>{day.label}</p>
                <ol className="flex min-w-0 flex-col">
                  {day.entries.map((entry) => {
                    const drawnId = drawnIds.get(entry.id) ?? entry.id;
                    return (
                      <Entry
                        drawnId={drawnId}
                        entry={entry}
                        issueKey={issueKey}
                        item={item?.id}
                        key={drawnId}
                        onPin={togglePin}
                        pinned={(id) => isPinnedEvent(issueState.dismissed ?? [], id)}
                      />
                    );
                  })}
                </ol>
              </li>
            ))}
          </ol>
        </ViewportAnchor>
      )}
      {log.isFetchNextPageError ? failed : null}
      {log.data !== undefined && (drawn.left > 0 || log.hasNextPage) ? (
        <button
          aria-busy={readsPage || undefined}
          className={`${SHOW_MORE_CONTROL} self-start`}
          onClick={(event) => {
            // Not disabled while its page is read, since Chromium drops the focus of a focused
            // button that turns disabled to the page: a press then joins the reader's own read of
            // that page (`readOlderPage`), and keeps the first press's hold. It draws the entries
            // read and not drawn yet, and reads the next page only where it has drawn them all.
            const control = event.currentTarget;
            if (document.activeElement === control && pressed.current?.control !== control) {
              pressed.current = {
                before: heldParts(),
                control,
                kind: "more",
                reads: drawn.left === 0,
              };
            }
            keepEntryBeside("under");
            if (drawn.left > 0) setEnd((held) => placeUnder(days, held ?? shownEnd, HISTORY_PAGE));
            else readOlderPage();
          }}
          ref={more}
          type="button"
          {...pageChrome}
        >
          {readsPage ? (
            ANSWER_LINES.loading_older
          ) : (
            // The count is of the entries read and not drawn, with "+" while an older page of
            // the log is unread, and none once every entry read is drawn.
            <ShowMoreWords
              count={
                drawn.left === 0 ? undefined : (
                  <span data-history-left="" {...authorText}>
                    {log.hasNextPage ? `${drawn.left}+` : drawn.left}
                  </span>
                )
              }
            />
          )}
        </button>
      ) : null}
    </section>
  );
}

/** What a press of Show more found drawn: each control of the list, and each entry. */
interface HeldParts {
  readonly controls: ReadonlySet<HTMLElement>;
  readonly entries: ReadonlySet<HTMLElement>;
}

/**
 * Keeps `element`, which a hand-on of the history just gave the focus to, wholly in the view of
 * `box`, Details' own, through its own later changes of height, by the least scroll
 * (`nearestScroll`), which `scrollBy` makes. The element can grow after the hand-on: a reference
 * in an entry's line is drawn by its address until the names of the references are read, then by
 * its target's name, which can wrap over several lines and take the focused link's foot below the
 * view, where a keyboard reader no longer sees the whole focus. An element taller than the view,
 * or one that lies wholly outside it, is left where it is: the reader's place went elsewhere then.
 * Chromium's resize observer reports no change of an element drawn inline, as a link in a line of
 * words is, so the entry the element stands in is watched, else the element itself. A browser
 * calls resize observers back in the order they were made, so the history's anchor, whose
 * observer is made as the list mounts, first puts back the element's top where a growth moved it
 * (`ViewportAnchor`'s `contentResized`), and this then brings the element's foot into the view.
 * The keeping ends as the focus leaves the element and at the reader's first act, a key, a press,
 * a wheel or a touch (`abortOnReaderAct`; the press that made the hand-on came before it).
 * Returns the keeping, which aborting ends sooner, as the next hand-on does.
 */
function keepFocusedInView(
  element: HTMLElement,
  box: HTMLElement,
  scrollBy: (by: number) => void
): AbortController {
  const kept = abortOnReaderAct();
  const sizes = new ResizeObserver(() => {
    if (document.activeElement !== element) {
      kept.abort();
      return;
    }
    const at = element.getBoundingClientRect();
    const view = box.getBoundingClientRect();
    if (at.height > view.height || !overlaps(at, view)) return;
    const by = nearestScroll(at, view);
    if (by !== 0) scrollBy(by);
  });
  kept.signal.addEventListener("abort", () => sizes.disconnect());
  sizes.observe(element.closest(`[${HISTORY_ENTRY_ATTRIBUTE}]`) ?? element);
  element.addEventListener("focusout", () => kept.abort(), { signal: kept.signal });
  return kept;
}
