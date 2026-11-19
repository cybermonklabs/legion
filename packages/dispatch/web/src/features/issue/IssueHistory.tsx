import {
  replaceEqualDeep,
  useInfiniteQuery,
  useQuery,
  useQueryClient,
} from "@tanstack/react-query";
import { type ReactNode, useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import { Link } from "react-router-dom";

import { api } from "../../api/client";
import { type EventPages, mergeEventPages } from "../../api/event-feed";
import type { IssueDetails, UserState } from "../../api/types";
import { PinButton } from "../../components/PinButton";
import {
  dangerText,
  linkText,
  newDividerLine,
  pageInkText,
  pageMutedText,
  pageQuietText,
  pageRowAct,
} from "../../theme/classes";
import { ActivityLine } from "../activity/ActivityLine";
import { dateKey } from "../activity/activity-model";
import { useMarginDrawer } from "../margin/margin-slot";
import { AgentName } from "../refs/AgentName";
import { MarkdownBody } from "../refs/MarkdownBody";
import { buildIssuePath } from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";
import { textSourceOf } from "../refs/text-references";
import { PHONE_VIEWPORT_QUERY, useMediaQuery } from "../shell/useDialog";
import { ViewportAnchor } from "../shell/ViewportAnchor";
import { HistoryHandoff } from "./HistoryHandoff";
import {
  buildHistory,
  type HistoryDay,
  type HistoryEntry,
  type HistoryTurn,
} from "./history-model";
import { useOutlineWriter } from "./outline-writer";
import { authorText, pageChrome } from "./page-structure";
import { CONTROL_LABELS } from "./page-words";
import { writePin } from "./pin-write";
import { eventItemId, isPinnedEvent, stateForIssue } from "./pins";

/** How many events a page of the log holds, as the server answers it. */
const PAGE = 200;

/**
 * The issue's history, a list in the panel (dispatch://INFRA-1196 R15, `history-model.ts`):
 * newest first, each day before today folded to one line with its count, which opens it. An
 * event is one line with its pin; one author's uploads within a minute are one entry that opens
 * each file; a thread agents wrote among themselves is drawn whole, each turn's words with its
 * Reply, which sets the line at the foot of the outline to reply to it - and once a person has
 * replied in a thread, the outline draws it and this list no longer does (R1).
 *
 * `/activity`, `t v` and the address of a comment or message of such a thread open Details here
 * (R20); `item` is the entry such an address names, which is marked and brought into view, its
 * day opened. It writes no read mark: the outline's end is the issue's one writer of it
 * (`IssueReadMark.tsx`), so the watermark this draws has one author - and the line that says
 * what is new holds for the visit, where the reader's mark stood as the history first drew, so
 * the outline's own write, a second after the page opens on a short outline, does not take the
 * line away under the reader who came to read it.
 *
 * On a phone Details is a sheet over the page, so a link of the history that leaves it - a
 * file, a document, a row of the outline - closes it, as a turn's Reply does.
 */
export function IssueHistory({
  issue,
  item,
  state,
}: {
  issue: IssueDetails;
  /** The id of the comment or message the address names, which the list marks. */
  item: string | undefined;
  state: UserState | undefined;
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
  // The outline's own reads, under its keys, so the threads cost no request of their own.
  const comments = useQuery({
    queryFn: () => api.listComments(issueKey),
    queryKey: ["comments", issueKey],
  });
  const messages = useQuery({
    queryFn: () => api.listIssueMessages(issueKey),
    queryKey: ["issue-messages", issueKey],
  });
  const issueState = stateForIssue(state, issueKey);
  // The reader's read mark for this visit: where it stood when the history first drew for this
  // issue, and lower once it has been moved back elsewhere, never higher - the page's own write
  // of it is forwards.
  const visitMark = useRef<{ readonly issue: string; readonly seq: number } | undefined>(undefined);
  if (
    state !== undefined &&
    (visitMark.current?.issue !== issueKey || issueState.last_read_seq < visitMark.current.seq)
  ) {
    visitMark.current = { issue: issueKey, seq: issueState.last_read_seq };
  }
  const lastReadSeq = visitMark.current?.seq ?? issueState.last_read_seq;
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
        comments: comments.data,
        events,
        lastReadSeq,
        ...(loadedFrom === undefined ? {} : { loadedFrom }),
        messages: messages.data,
        today,
      }),
    [comments.data, events, issue.artifacts, lastReadSeq, loadedFrom, messages.data, today]
  );
  // The days before today the reader opened; the day the address's entry is in is opened as
  // the address names it, and the reader's own press closes it again like any other.
  const [opened, setOpened] = useState<ReadonlySet<string>>(() => new Set());
  const marked = item === undefined ? undefined : dayOf(days, item);
  useEffect(() => {
    if (marked === undefined) return;
    setOpened((held) => (held.has(marked) ? held : new Set(held).add(marked)));
  }, [marked]);
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
  const drawer = useMarginDrawer();
  const phone = useMediaQuery(PHONE_VIEWPORT_QUERY);
  // The history is newest first, so every event the feed prepends lands above a reader who is
  // down in it: the anchor holds the entry they are on where it was, in the box it scrolls in -
  // Details' own, or the document outside the shell. It follows each entry's own id
  // (`data-history-entry`), not its `data-event-seq`: a thread whose first turn's event is on a
  // page of the log not yet read carries seq 0 until that page lands, beside every other such
  // thread, so its seq names neither one entry nor the same entry before and after `Load older`.
  // That box is found once the list is drawn; moving the panel to another host remounts it.
  const anchorRoot = useRef<HTMLElement | null>(null);
  const scroller = useRef<HTMLElement | null>(null);
  const listDrawn = log.isSuccess;
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

  return (
    <section
      aria-label={CONTROL_LABELS.history}
      className="flex min-w-0 flex-col gap-1 outline-none"
      data-testid="issue-history"
      // On a phone Details covers the page, so a link that leaves the history closes it for the
      // reader to see where the link went. A turn's own time is that turn's address, which
      // opens Details here, so it stays.
      onClickCapture={(event) => {
        if (!phone || !(event.target instanceof Element)) return;
        const link = event.target.closest("a[href]");
        if (link !== null && !link.hasAttribute("data-history-time")) drawer?.setOpen(false);
      }}
      tabIndex={-1}
    >
      <h2 className={`text-sm font-semibold ${pageInkText}`} {...pageChrome}>
        {CONTROL_LABELS.history}
      </h2>
      {log.isPending ? (
        <p className={`text-xs ${pageQuietText}`} {...authorText}>
          Loading activity…
        </p>
      ) : log.isError ? (
        <p className={`text-xs ${dangerText}`} {...authorText}>
          Could not load the activity.
        </p>
      ) : (
        <ViewportAnchor
          enabled={holdsPlace}
          item="data-history-entry"
          rootRef={anchorRoot}
          scroller={scroller}
        >
          <ol aria-label="Issue activity" className="flex min-w-0 flex-col gap-1" {...authorText}>
            {days.map((day) => {
              const open = day.today || opened.has(day.date);
              const count = day.entries.filter((entry) => entry.kind !== "unread").length;
              return (
                <li
                  className="flex min-w-0 flex-col gap-1"
                  data-history-day={day.date}
                  key={day.date}
                >
                  {day.today ? (
                    <p
                      className={`mt-1 text-xs font-semibold tracking-wide uppercase ${pageQuietText}`}
                    >
                      {day.label}
                    </p>
                  ) : (
                    <button
                      aria-expanded={open}
                      className={`dispatch-inline-link mt-1 self-start text-xs font-semibold tracking-wide uppercase ${pageQuietText} ${pageRowAct}`}
                      onClick={() =>
                        setOpened((held) => {
                          const next = new Set(held);
                          if (!next.delete(day.date)) next.add(day.date);
                          return next;
                        })
                      }
                      type="button"
                    >
                      {day.label} · {count}
                    </button>
                  )}
                  {open ? (
                    <ol className="flex min-w-0 flex-col gap-1">
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
      {log.hasNextPage ? (
        <button
          className={`dispatch-inline-link self-start text-xs font-medium underline ${pageMutedText}`}
          disabled={log.isFetchingNextPage}
          onClick={() => void log.fetchNextPage()}
          type="button"
          {...authorText}
        >
          {log.isFetchingNextPage ? "Loading…" : "Load older"}
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
        aria-label="New since you last read"
        className={`my-1 flex items-center gap-3 text-xs font-semibold tracking-wide uppercase ${linkText}`}
      >
        <span className={`h-px flex-1 ${newDividerLine}`} />
        New since you last read
        <span className={`h-px flex-1 ${newDividerLine}`} />
      </li>
    );
  }
  const head = (
    <span className="flex max-w-full min-w-0 shrink-0 items-baseline gap-x-2 whitespace-nowrap">
      <span className="min-w-0 truncate font-medium">
        <AgentName actor={entry.author} inline />
      </span>
      <span aria-hidden="true">·</span>
      <Timestamp at={entry.at} />
    </span>
  );
  if (entry.kind === "event") {
    const isPinned = pinned(entry.event.id);
    return (
      <li
        className={`flex min-w-0 flex-wrap items-baseline gap-x-2 text-xs ${pageMutedText}`}
        data-event-seq={entry.seq}
        data-history-entry={entry.id}
      >
        {head}
        <span className="min-w-40 flex-1 break-words">
          <ActivityLine description={entry.description} event={entry.event} issueKey={issueKey} />
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
  if (entry.kind === "uploads") {
    return (
      <li
        className={`flex min-w-0 flex-wrap items-baseline gap-x-2 text-xs ${pageMutedText}`}
        data-event-seq={entry.seq}
        data-history-entry={entry.id}
        data-history-uploads={entry.files.length}
      >
        {head}
        <span className="min-w-40 flex-1 break-words">
          uploaded{" "}
          {entry.files.map((file, index) => (
            <span key={file.id}>
              {index === 0 ? null : ", "}
              {file.slug === undefined ? (
                file.name
              ) : (
                <Link
                  className={`dispatch-inline-link underline ${linkText}`}
                  to={buildIssuePath({ key: issueKey, kind: "artifact", slug: file.slug })}
                >
                  {file.name}
                </Link>
              )}
            </span>
          ))}
        </span>
      </li>
    );
  }
  return (
    <li
      className="flex min-w-0 flex-col gap-1"
      data-event-seq={entry.seq}
      data-history-entry={entry.id}
    >
      <Turn issueKey={issueKey} marked={item === entry.root.id} turn={entry.root} />
      {entry.replies.length === 0 ? null : (
        <ol className="ml-3 flex min-w-0 flex-col gap-1 border-l border-page-line pl-3">
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
      className={`flex min-w-0 flex-col rounded-md text-sm ${marked ? "bg-page-mark" : ""}`}
      data-history-item={turn.id}
      data-history-kind={turn.kind}
    >
      <p className={`flex min-w-0 items-baseline gap-x-2 text-xs ${pageMutedText}`}>
        <span className="min-w-0 truncate font-medium">
          <AgentName actor={turn.author} inline />
        </span>
        <span aria-hidden="true">·</span>
        <Link
          className={`dispatch-inline-link ${pageMutedText}`}
          data-history-time=""
          to={buildIssuePath({ id: turn.id, key: issueKey, kind: turn.kind })}
        >
          <Timestamp at={turn.at} />
        </Link>
      </p>
      <div className={`min-w-0 break-words ${pageInkText}`}>
        <MarkdownBody
          markdown={turn.body}
          source={textSourceOf(turn.kind, turn.id)}
          tree={turn.bodyTree}
        />
      </div>
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
