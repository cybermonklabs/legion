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
 * The issue's history, a list in the panel (dispatch://INFRA-1196 R15, `history-model.ts`):
 * newest first, each day before today folded to one line with its count, which opens it. An
 * event is one line with its pin; one author's uploads within a minute are one entry that opens
 * each file, each with its own upload's pin; a thread agents wrote among themselves is drawn
 * whole, each turn's words with its Reply, which sets the line at the foot of the outline to
 * reply to it - and once a person has replied in a thread, the outline draws it and this list no
 * longer does (R1).
 *
 * `/activity`, `t v` and the address of a comment or message of such a thread open Details here
 * (R20): at `/activity` the heading is brought into Details' view once for each arrival, except a
 * reader's coming back to the history they left drawn, who finds it where they left it (R3);
 * `item` is the entry such an address names, which is marked and brought into view, its day
 * opened. It writes no read mark. The line that says what is new is drawn at `visitMark`,
 * where the reader's mark stood as the history first drew in this visit, which
 * `DetailsHistory` holds while Details is shut and this list is not drawn: so a later move of
 * the mark, from another page or tab, leaves the line where the reader found it.
 *
 * On a phone Details is a sheet over the page, and a link followed in it closes it (part D's
 * `Margin.tsx`), so a link of the history that leaves it - a file, a document, a row of the
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
  // The days before today the reader opened; the day the address's entry is in is opened as
  // the address names it, and the reader's own press closes it again like any other.
  const [opened, setOpened] = useState<ReadonlySet<string>>(() => new Set());
  const marked = item === undefined ? undefined : dayOf(days, item);
  useEffect(() => {
    if (marked === undefined) return;
    setOpened((held) => (held.has(marked) ? held : new Set(held).add(marked)));
  }, [marked]);
  // The address of a turn of a thread that began before the oldest event read so far names an
  // entry the history does not draw yet (`HistoryInput.loadedFrom`), so the log is read on, one
  // page at a time, until that thread is in or the log has ended, and the landing below then
  // marks the turn, opens its day and brings it on screen (R4). It waits for the comments and
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
  // lands it.
  const landed = useRef<string | undefined>(undefined);
  useEffect(() => {
    if (item === undefined || landed.current === item || marked === undefined) return;
    const entry = document.querySelector(`[data-history-item="${CSS.escape(item)}"]`);
    if (entry === null || entry.getClientRects().length === 0) return;
    landed.current = item;
    entry.scrollIntoView({ block: "center" });
  });
  // `/activity` opens Details at the history (R4): its heading is brought into Details' view
  // once for each arrival at that address, on the first render it has a box - Details still
  // closed around it, or the panel not yet placed in the drawer, gives it none - and leaving the
  // address lets the next arrival bring it again. A reader who comes back with the browser's
  // Back or Forward to the history this list drew there before - Details left open beside a
  // view, or at a turn's address - finds Details where they left it (R3), so that arrival brings
  // nothing. Only the box Details scrolls in moves, never the page under it; the reader's own
  // scroll after that is theirs.
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
      element.getClientRects().length === 0
    ) {
      return;
    }
    headingLanded.current = activityAddress;
    const comingBack = navigation === "POP" && landedBefore.current;
    landedBefore.current = true;
    if (comingBack) return;
    const box = scrollingAncestor(element);
    if (box !== null) scrollElementToTop(element, box);
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
  // history, never on the page, as Details' Unpin does (`IssuePins.tsx`). Load older, which a
  // press disables while its page is read and the log's end takes away, hands it to the first
  // control the page it read added, where Load older stood, else to Load older again where
  // pages are left, else to the heading; the Retry of a failed read, gone once the read lands,
  // hands it to the heading. Each is held from its press, made with the focus on it, until the
  // reads settle, and only a focus left on the page moves: a reader who has put it elsewhere by
  // then keeps it there. No such move scrolls Details, whose reader keeps their place (R3).
  const pressed = useRef<
    { readonly kind: "older"; readonly before: ReadonlySet<Element> } | { readonly kind: "retry" }
  >(undefined);
  const loadOlder = useRef<HTMLButtonElement>(null);
  const failure = useRef<HTMLDivElement>(null);
  const controlsOfList = () => [
    ...(anchorRoot.current?.querySelectorAll<HTMLElement>("a[href], button:not(:disabled)") ?? []),
  ];
  useLayoutEffect(() => {
    const press = pressed.current;
    if (press === undefined || log.isFetching) return;
    pressed.current = undefined;
    const active = document.activeElement;
    if (active !== null && active !== document.body) return;
    const target =
      press.kind === "older"
        ? (controlsOfList().find((control) => !press.before.has(control)) ?? loadOlder.current)
        : failure.current?.querySelector("button");
    (target ?? heading.current)?.focus({ preventScroll: true });
  });
  // The history draws the log it holds whatever the last read's outcome (decision 98): a read
  // that fails once it holds the log - a refetch after an event, a later open of Details, the
  // next page - says so with its Retry beside the entries, never in their place, and the line
  // stands alone only while no page is held. A failed next page is said where Load older is,
  // and its Retry reads that page again; any other failure under the heading, where its Retry
  // reads the log again.
  const failed = log.isError ? (
    <div ref={failure}>
      <DetailsErrorLine
        error={log.error}
        message={ANSWER_LINES.history_unreadable}
        onRetry={() => {
          if (failure.current?.contains(document.activeElement) === true) {
            pressed.current = { kind: "retry" };
          }
          void (log.isFetchNextPageError ? log.fetchNextPage() : log.refetch());
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
      {log.isPending ? (
        <p className={`text-xs ${pageQuietText}`} {...pageChrome}>
          {ANSWER_LINES.loading_history}
        </p>
      ) : log.data === undefined ? null : (
        <ViewportAnchor
          enabled={holdsPlace}
          item="data-history-entry"
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
                      {day.label} <span className={`font-medium ${pageQuietText}`}>· {count}</span>
                    </button>
                  )}
                  {open ? (
                    <ol className="flex min-w-0 flex-col">
                      {day.entries.map((entry) => (
                        <Entry
                          entry={entry}
                          issueKey={issueKey}
                          item={item}
                          key={entry.id}
                          onPin={togglePin}
                          pinned={(id) => isPinnedEvent(issueState.dismissed ?? [], id)}
                        />
                      ))}
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
          className={`dispatch-inline-link self-start text-xs font-medium underline ${pageMutedText}`}
          disabled={log.isFetchingNextPage}
          onClick={(event) => {
            if (document.activeElement === event.currentTarget) {
              pressed.current = { before: new Set(controlsOfList()), kind: "older" };
            }
            void log.fetchNextPage();
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

/** One entry of the history (`HistoryEntry`). */
function Entry({
  entry,
  issueKey,
  item,
  onPin,
  pinned,
}: {
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
        data-history-entry={entry.id}
      >
        <span className="min-w-40 flex-1 break-words">
          {head}
          {/* `main`'s line links a document's name in the application's link blue, which
              Details draws in no line (decision 56): those two lines are the history's own. */}
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
  // line of the history has its event's (R2): the run's line says who uploaded, and each file
  // stands under it on a line of its own, beside its pin, as an event's words stand beside theirs.
  if (entry.kind === "uploads") {
    return (
      <li
        className={`${ENTRY} flex min-w-0 flex-col`}
        data-event-seq={entry.seq}
        data-history-entry={entry.id}
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
              <li className="flex min-w-0 items-center gap-x-2" key={file.id}>
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
    <li className="flex min-w-0 flex-col" data-event-seq={entry.seq} data-history-entry={entry.id}>
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
      {/* Cut as the outline cuts an agent's words (ruling 1), and whole where the address
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
