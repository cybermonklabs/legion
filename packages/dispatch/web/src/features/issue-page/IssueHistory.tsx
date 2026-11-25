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
import { issueResultsQuery } from "../../api/queries";
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
import { EditedMark } from "../refs/PostBody";
import { EVENT_FRESH, standsFailed } from "../refs/reference-target";
import { buildIssuePath, buildReferencePath, documentRoute } from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";
import { textSourceOf } from "../refs/text-references";
import { PHONE_VIEWPORT_QUERY, useMediaQuery } from "../shell/useDialog";
import { ViewportAnchor } from "../shell/ViewportAnchor";
import { DetailsErrorLine, retryRead } from "./details-error";
import { ENTRY_CONTROLS, SHOW_MORE_CONTROL, ShowMoreWords } from "./details-leaves";
import { HistoryHandoff } from "./HistoryHandoff";
import {
  buildHistory,
  comparePlaces,
  HISTORY_PAGE,
  type HistoryDay,
  type HistoryEntry,
  type HistoryPlace,
  type HistoryTurn,
  historyBlocks,
  historyDownTo,
  type NamedBlock,
  placeUnder,
  turnPlace,
} from "./history-model";
import { useMarginDrawer } from "./margin-slot";
import { DeletedLine } from "./NodeRow";
import { useOutlineWriter } from "./outline-writer";
import { authorText, pageChrome } from "./page-structure";
import { ANSWER_LINES, CONTROL_LABELS } from "./page-words";
import { ROW_LINK } from "./row-references";
import { scrollingAncestor } from "./scroll-box";

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
  // (`details-leaves.tsx`'s `useListPages`), whether the control stays or goes: a control of
  // that entry the reader can see; else one the press added that they can see, one under the
  // entries drawn before the press before one above them, as the next entries are drawn where
  // the control stood; else that entry's control out of view; else any the press added; else the
  // control again, where entries or pages are left; else the last control drawn before the press,
  // which stood just above it, for a page that adds no entry (its events are all events the
  // outline draws); and only then the heading, above the whole list. A press that reads the
  // log's next page moves it once that page has landed and its first entries are drawn. A
  // control of the
  // history that goes away under the reader's focus so leaves the focus in the history, never on
  // the page, as Details' Unpin does (`IssuePins.tsx`). The Retry of a failed next page, which
  // stands where the control stood, hands it on the same way; the Retry of a failed read of the
  // log, gone once the read lands, hands it to the heading. None of them is disabled while it
  // reads, since Chromium drops the focus of a focused button that turns disabled to the page,
  // so the focus stays on the control pressed while its read is out, and a press of it then joins
  // that read rather than ending it and sending another (`readOlderPage` for a page, `retryRead`
  // for the log). Each press, made with the focus on its control, is held until what it asked
  // for is drawn, and a press of the control already held keeps the first press's hold, the
  // controls drawn before it with it, so the press that joins changes nothing of the hand-on;
  // then a focus left on the page moves, and so does one still on that control once what it
  // asked for is drawn, while a read that failed leaves it there, beside the line that says so. A
  // reader who has put the focus elsewhere by then keeps it there. No such move scrolls Details,
  // whose reader keeps their place: from the press until its reads settle, Details keeps the
  // place of the entry next to the pressed control (`keepEntryBeside`), so the entries drawn
  // under it leave that entry where the reader saw it, and a control the page pushed out of view
  // - under files it added to a run above - takes the focus only when none is in view. The entry
  // the focus moved to is then the place Details holds as the entries above it change height
  // (`ViewportAnchor` records the place at every focus moved into its rows).
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
  useLayoutEffect(() => {
    const press = pressed.current;
    const reads = press?.kind === "retry" || press?.reads === true;
    if (press === undefined || (reads && log.isFetching)) return;
    pressed.current = undefined;
    const active = document.activeElement;
    const onPage = active === null || active === document.body;
    const onPressedAfterRead = active === press.control && !(reads && log.isError);
    if (!onPage && !onPressedAfterRead) return;
    if (press.kind === "retry") {
      (failure.current?.querySelector("button") ?? heading.current)?.focus({ preventScroll: true });
      return;
    }
    // The entries it drew stand under every control drawn before it, where Show more stood. They
    // can push one another out of Details' view: files a page added to a run above move the
    // entries under that run down, so a new control is in view or not by where it is drawn once
    // the entry next to the pressed control is back where it stood: the place kept from the
    // press (`keepEntryBeside`), which `ViewportAnchor` restored in this commit before this runs.
    const { controls, entries } = press.before;
    const lastBefore = [...controls].filter((control) => control.isConnected).at(-1);
    const added = controlsOfList().filter((control) => !controls.has(control));
    const isBelow = (element: HTMLElement) =>
      lastBefore === undefined ||
      (lastBefore.compareDocumentPosition(element) & Node.DOCUMENT_POSITION_FOLLOWING) !== 0;
    const view = scroller.current?.getBoundingClientRect() ?? {
      bottom: window.innerHeight,
      top: 0,
    };
    const inView = (control: HTMLElement) => {
      const at = control.getBoundingClientRect();
      return at.height > 0 && at.top >= view.top && at.bottom <= view.bottom;
    };
    const first =
      entriesOfList()
        .find((entry) => !entries.has(entry) && isBelow(entry))
        ?.querySelector<HTMLElement>(ENTRY_CONTROLS) ?? undefined;
    const target =
      (first !== undefined && inView(first) ? first : undefined) ??
      added.find((control) => isBelow(control) && inView(control)) ??
      added.find(inView) ??
      first ??
      added.find(isBelow) ??
      added[0] ??
      more.current ??
      lastBefore;
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
              Details draws in no line: those two lines are the history's own. So is a line that
              names a block by its node's words, where `main`'s names it by its id. */}
          {entry.event.type === "artifact.created" || entry.event.type === "artifact.renamed" ? (
            <DocumentLine artifact={entry.event.payload.artifact} description={entry.description} />
          ) : entry.block === undefined ? (
            <ActivityLine description={entry.description} event={entry.event} issueKey={issueKey} />
          ) : (
            <BlockLine block={entry.block} description={entry.description} />
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

/** The line of a block repaired or found invalid that names the block by its node's words
 *  (`HistoryEntry.block`), as `DocumentLine` draws a document's: its words, with the block's -
 *  the part of the line `block.part` names - a link to its node in the page's ink and its rows'
 *  underline, where its address reads as one. */
function BlockLine({
  block,
  description,
}: {
  block: NamedBlock;
  description: readonly AgentPart[];
}): ReactNode {
  const { path } = block.block;
  return (
    <span>
      <AgentParts
        parts={description}
        text={(part, index) =>
          index !== block.part || path === undefined ? (
            part
          ) : (
            <Link
              className={`dispatch-inline-link ${ROW_LINK} ${pageInkText}`}
              to={path}
              {...authorText}
            >
              {part}
            </Link>
          )
        }
      />
    </span>
  );
}

/**
 * One turn of a thread the history holds: when, who, and ` · edited` after the author where
 * the author edited it (`EditedMark`); its words whole at any length, or, where its author
 * deleted it, the deleted line in their place, as the outline's rows draw it (`DeletedLine`);
 * and Reply, which a deleted turn has not, as Dispatch takes no reply to it.
 */
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
        <EditedMark at={turn.editedAt} deleted={turn.deleted} marks={pageChrome} />
      </p>
      {/* Marked as a node's words, so `styles.css` keeps a wide table or line of code scrolling
          sideways inside itself and gives the words the rhythm, links, chips and pictures of a
          node's words elsewhere on the page. */}
      <div className={WORDS} data-node-words="">
        {turn.deleted === null ? (
          <MarkdownBody
            markdown={turn.body}
            source={textSourceOf(turn.kind, turn.id)}
            tree={turn.bodyTree}
          />
        ) : (
          <DeletedLine deleted={turn.deleted} />
        )}
      </div>
      {writer === undefined || turn.deleted !== null ? null : (
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
