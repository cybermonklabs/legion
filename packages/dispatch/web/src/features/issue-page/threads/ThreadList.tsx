import {
  type InfiniteData,
  type UseInfiniteQueryResult,
  useInfiniteQuery,
  useQueries,
  useQuery,
} from "@tanstack/react-query";
import {
  type ReactNode,
  useCallback,
  useEffect,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
} from "react";

import { userStateQuery, whoAmIQuery } from "../../../api/queries";
import type { IssueDetails, MyRepliesResponse, MyReplyRow } from "../../../api/types";
import { focusVisibleRing, pageLineBorder, pageMutedText } from "../../../theme/classes";
import { threadQuery } from "../../dashboard/ThreadItem";
import { dashboardRepliesQuery, isDaemon } from "../../dashboard/useWaitingItems";
import { useAgentText } from "../../refs/AgentName";
import { EVENT_FRESH } from "../../refs/reference-target";
import { buildIssuePath } from "../../refs/routes";
import { agentRoleWord } from "../agent-role";
import { pageChrome } from "../page-structure";
import { ANSWER_LINES, EMPTY_STATE_LINES, THREADS_WORDS } from "../page-words";
import { FailedReadLine } from "../slots/failed-read-line";
import { ShowNext } from "./ShowNext";
import { FIRST_CHOICE, type ThreadChoice, ThreadChoices } from "./ThreadChoices";
import { ThreadRow } from "./ThreadRow";
import { type MarkReads, recordMark } from "./thread-marks";
import { mergeThreads, type ThreadPage } from "./thread-merge";
import {
  askThreadsQuery,
  commentThreadsQuery,
  isOlderBuild,
  messageThreadsQuery,
  THREADS_PAGE,
  threadEventsQuery,
} from "./thread-reads";
import { type ThreadRecord, type ThreadSource, threadRecords } from "./thread-records";
import { type RowNames, threadRow } from "./thread-row";

/**
 * How a row of Threads names an author (R7, root R9), as the outline's row does (`NodeRow.tsx`): a
 * person by the page's name of them, and an agent by its role's word, out of the title the page
 * names its session by (`agentRoleWord`). `threadRow` names the Legion daemon itself.
 */
export function useRowNames(project: string): RowNames {
  const text = useAgentText();
  return useMemo(
    () => ({
      author: (actor) =>
        actor.kind === "session" ? agentRoleWord(text.author(actor)) : text.author(actor),
      project,
    }),
    [project, text]
  );
}

/**
 * The rows of Waiting on me (dispatch://INFRA-5704/spec#b-r-waiting, R6): every row of the
 * reader's unread `me/replies` of the issue, less each row whose root the Legion daemon of the
 * issue's project wrote (`isDaemon`, Dashboard's own rule), in that route's order, the newest reply
 * first. Undefined while that read has not answered. The Threads control counts these rows (R1),
 * never the read's `total`, so the count and the choice read one rule.
 */
function waitingRows(
  replies: Pick<MyRepliesResponse, "rows"> | undefined,
  project: string
): readonly MyReplyRow[] | undefined {
  return replies === undefined
    ? undefined
    : (replies.rows ?? []).filter((row) => !isDaemon(row.root_author, project));
}

/**
 * The number of the rows of Waiting on me over the read that Dashboard's threads and the header's
 * count send (`dashboardRepliesQuery`, with no `limit`), so the page sends it once for all three
 * (R1, R6, R15). The view row reads it for the Threads control (`slots/view-row.tsx`), and selects
 * the count alone so a change of rows with the same count does not re-render the row.
 */
export function useWaitingCount(issueKey: string, project: string): number | undefined {
  return useQuery({
    ...dashboardRepliesQuery(issueKey),
    ...EVENT_FRESH,
    select: (replies) => waitingRows(replies, project)?.length,
  }).data;
}

/** One read that failed, which the list says with the page's failed-read line and its Retry
 *  (`FailedReadLine`). */
interface FailedRead {
  /** Which read it is, which keys its line. */
  readonly id: string;
  /** The read's own count of failures, which keys the line's alert. */
  readonly failure: number;
  /** Asks that read alone again. */
  readonly retry: () => void;
  readonly retrying: boolean;
}

/** What one choice draws. */
export interface ChoiceView {
  /** The rows, in the choice's order, or undefined while the choice draws none yet: one of the
   *  reads its first rows come from is out or failed. */
  readonly rows: readonly ThreadRecord[] | undefined;
  /** The number the control under the rows says is left (R2). */
  readonly left: number;
  /** Whether the rows a press of that control asked for wait on a read. */
  readonly busy: boolean;
  /** The reads that failed, in the order the choice gives its reads. */
  readonly failed: readonly FailedRead[];
  /** Whether a build from before the thread pages answered a route's bare list
   *  (`isOlderBuild`), which is not read again. */
  readonly olderBuild: boolean;
}

/** One route's pages as a choice reads them, from its infinite query. */
interface RouteRead {
  readonly id: string;
  /** The pages read so far, first to last, or undefined while the first has not answered. */
  readonly pages: readonly ThreadPage[] | undefined;
  /** The read's error, null while none stands. */
  readonly error: unknown;
  readonly failure: number;
  readonly fetching: boolean;
  /** Whether the last page read names a next one (`before`). */
  readonly hasNext: boolean;
  /** Whether the read of the next page failed. */
  readonly nextFailed: boolean;
  /** Whether a read of the pages again failed, a refresh's after a write (R14): the pages it
   *  would have replaced stay drawn. */
  readonly refetchFailed: boolean;
  /** Reads the next page, or joins the read of it that is out. */
  readonly next: () => void;
  /** Reads the route again from its first page. */
  readonly refetch: () => void;
}

function routeRead<P extends ThreadPage>(
  id: string,
  query: UseInfiniteQueryResult<InfiniteData<P, unknown>, unknown>
): RouteRead {
  return {
    error: query.error,
    failure: query.errorUpdateCount,
    fetching: query.isFetching,
    hasNext: query.hasNextPage,
    id,
    // A press that finds the next page out joins that read rather than sending a second one.
    next: () => void query.fetchNextPage({ cancelRefetch: false }),
    nextFailed: query.isFetchNextPageError,
    pages: query.data?.pages,
    refetch: () => void query.refetch(),
    refetchFailed: query.isRefetchError,
  };
}

/** The line a route's failure draws: a failed first page, or a failed read of the pages again
 *  after a write (R14), asks the route again from its start, a failed next page asks that page
 *  again, and an older build's answer draws no such line. */
function failedRoute(route: RouteRead): FailedRead[] {
  if (route.error === null || isOlderBuild(route.error)) return [];
  if (route.pages === undefined || route.refetchFailed) {
    return [
      { failure: route.failure, id: route.id, retry: route.refetch, retrying: route.fetching },
    ];
  }
  return route.nextFailed
    ? [{ failure: route.failure, id: route.id, retry: route.next, retrying: route.fetching }]
    : [];
}

/**
 * A choice that merges routes - All and Mine over the three routes, Agents over its one
 * (R3 to R5) - by `mergeThreads`, `shown` rows of it asked for. Once a press asks for rows past
 * the pages drawn, the view reads the next page of each route the merge names (`refill`), and
 * the rows wait for those reads, so the next 20 are drawn whole (R3). A read that is out, or whose
 * next page failed, is not sent again here: its line's Retry sends it.
 */
function useMergedChoice({
  mine,
  on,
  routes,
  shown,
}: {
  /** The reader's login, for Mine (R4). */
  readonly mine?: string;
  /** Whether the reader has turned the choice on, so its reads go (R15). */
  readonly on: boolean;
  readonly routes: readonly RouteRead[];
  readonly shown: number;
}): ChoiceView {
  const merged = mergeThreads({ mine, routes: routes.map((route) => route.pages ?? []), shown });
  useEffect(() => {
    if (!on) return;
    for (const at of merged.refill) {
      const route = routes[at];
      if (route?.hasNext === true && !route.fetching && !route.nextFailed) route.next();
    }
  });
  const drawn = on && routes.every((route) => route.pages !== undefined);
  return {
    busy: merged.refill.length > 0,
    failed: on ? routes.flatMap(failedRoute) : [],
    left: drawn ? merged.left : 0,
    olderBuild: on && routes.some((route) => isOlderBuild(route.error)),
    rows: drawn ? merged.rows : undefined,
  };
}

/** The records of each thread the pages hold, by the thread's id: All's first, then Mine's. */
function heldThreads(routes: readonly RouteRead[]): ReadonlyMap<string, readonly ThreadRecord[]> {
  const held = new Map<string, readonly ThreadRecord[]>();
  for (const route of routes) {
    for (const page of route.pages ?? []) {
      for (const source of page.threads ?? []) {
        const records = threadRecords(source);
        const thread = records[0]?.thread;
        if (thread !== undefined && !held.has(thread)) held.set(thread, records);
      }
    }
  }
  return held;
}

/** The pages Waiting on me draws: its records, and the number the control under them says is
 *  left. */
interface DrawnPages {
  readonly rows: readonly ThreadRecord[];
  readonly left: number;
}

/**
 * Waiting on me (R6): `rows`, 20 of them a page, each drawn from its thread, with the records of
 * one thread together at its row's place. A row whose thread a page of All or
 * Mine holds (`held`) is drawn from it. Every other row of a page drawn is read through its
 * thread's own route, as Dashboard's thread item reads it (`threadQuery`): at most 20 such reads
 * for each page, none for a row not drawn, and none before All's first pages have settled, which
 * may hold it. A page waiting on such a read draws none of its rows, so each page is drawn whole.
 *
 * Once the pages were drawn whole, a row that waits on its thread's read keeps them as they were
 * drawn (R14): a refresh after a write can put a thread no page holds among the rows, and until
 * that thread's read answers the choice draws the rows it drew, then the new pages whole, so no
 * row drawn goes while a later read answers.
 */
function useWaitingChoice({
  held,
  heldSettled,
  issueKey,
  on,
  rows,
  shown,
}: {
  readonly held: ReadonlyMap<string, readonly ThreadRecord[]>;
  /** Whether each of All's first pages has answered or failed. */
  readonly heldSettled: boolean;
  readonly issueKey: string;
  readonly on: boolean;
  readonly rows: readonly MyReplyRow[] | undefined;
  readonly shown: number;
}): ChoiceView & { readonly read: readonly ThreadRecord[] } {
  const asked = (rows ?? []).slice(0, shown);
  const unheld = on ? asked.filter((row) => !held.has(row.thread_id)) : [];
  // Each read is Dashboard's thread read, under its key, so the page sends one read for both
  // (R6). Dashboard's `select` answers only the thread's texts, and Threads draws the whole read,
  // so the read is taken without it: a `select` is the observer's own, and the key stays shared.
  const reads = useQueries({
    queries: on
      ? unheld.map((row) => {
          const { select: _texts, ...read } = threadQuery(issueKey, row);
          return { ...read, enabled: heldSettled };
        })
      : [],
  });
  // Each unheld row's records: from its own read once that answered, an empty list where that
  // read failed, and absent while it is out.
  const fetched = new Map<string, readonly ThreadRecord[]>();
  const failed: FailedRead[] = [];
  for (const [at, row] of unheld.entries()) {
    const read = reads[at];
    if (read?.data !== undefined) {
      fetched.set(row.thread_id, threadRecords(read.data as ThreadSource));
    } else if (read?.isError === true) {
      fetched.set(row.thread_id, []);
      failed.push({
        failure: read.errorUpdateCount,
        id: `thread-${row.thread_id}`,
        retry: () => void read.refetch(),
        retrying: read.isFetching,
      });
    }
  }
  let ready = 0;
  while (ready < asked.length) {
    const page = asked.slice(ready, ready + THREADS_PAGE);
    if (page.some((row) => !held.has(row.thread_id) && !fetched.has(row.thread_id))) break;
    ready += page.length;
  }
  const drawn = on && rows !== undefined && (asked.length === 0 || ready > 0);
  const now: DrawnPages | undefined = drawn
    ? {
        left: (rows?.length ?? 0) - ready,
        rows: asked
          .slice(0, ready)
          .flatMap((row) => held.get(row.thread_id) ?? fetched.get(row.thread_id) ?? []),
      }
    : undefined;
  // The pages as the choice drew them last with every row it asked for read.
  const lastWhole = useRef<DrawnPages | undefined>(undefined);
  const whole = on && rows !== undefined && ready === asked.length;
  useLayoutEffect(() => {
    if (whole) lastWhole.current = now;
  });
  const pages = whole || !on ? now : (lastWhole.current ?? now);
  return {
    busy: ready < asked.length,
    failed: on ? failed : [],
    left: pages?.left ?? 0,
    olderBuild: false,
    read: unheld.flatMap((row) => fetched.get(row.thread_id) ?? []),
    rows: pages?.rows,
  };
}

/** How many rows each choice draws when Threads opens: its first 20 (R2). */
const FIRST_SHOWN: Readonly<Record<ThreadChoice, number>> = {
  agents: THREADS_PAGE,
  all: THREADS_PAGE,
  mine: THREADS_PAGE,
  waiting: THREADS_PAGE,
};

/** What the list draws, as `useThreadLists` answers it. */
export interface ThreadLists {
  /** The choice that is on. */
  readonly choice: ThreadChoice;
  /** Turns a choice on, which sends its reads the first time (R15). */
  readonly choose: (next: ThreadChoice) => void;
  /** What the choice that is on draws. */
  readonly view: ChoiceView;
  /** Asks the choice that is on for its next 20 rows. */
  readonly showNext: () => void;
  /** Each record a page or a thread's read that the view holds gives, by the record's id. */
  readonly records: ReadonlyMap<string, ThreadRecord>;
  /** What each row's mark is read from (R7), once every one of those reads has answered. */
  readonly marks: MarkReads | undefined;
  /** Whether each of All's first pages has answered or failed: a record no page holds by then is
   *  one R12's lookups read (R15). All's pages are read whatever choice is on, and none of them
   *  is ever out again with no answer. */
  readonly settled: boolean;
  /** Whether a read of a route's pages is out: a first page, a next page, or the pages read
   *  again after a write (R14). A write can move a thread from one route to another, as a
   *  person's reply takes a thread between agents out of Agents and into All, and the route it
   *  leaves can answer before the one it joins: until that read answers, no page holds the
   *  thread's records, and none of them is gone. */
  readonly reading: boolean;
}

/**
 * The reads and the state of the list (dispatch://INFRA-5704/spec#b-r-view to #b-r-row): the
 * choice that is on, All when Threads opens (R2), and how many rows each choice has shown, which
 * the view holds apart from the pages, so a choice turned on again draws the rows it drew. A
 * choice reads its own pages when the reader turns it on and not before, and keeps them (R15).
 *
 * Every choice draws its rows only once R7's reads have answered as well - the reader's login,
 * the reader's unread threads, the reader's state and the issue's events - so each row is drawn
 * with its mark, and none gains one later as a read lands.
 */
export function useThreadLists(issue: IssueDetails): ThreadLists {
  const { key, project } = issue;
  const [choice, setChoice] = useState<ThreadChoice>(FIRST_CHOICE);
  const [opened, setOpened] = useState<ReadonlySet<ThreadChoice>>(() => new Set([FIRST_CHOICE]));
  const [shown, setShown] = useState(FIRST_SHOWN);

  const viewer = useQuery({ ...whoAmIQuery(), ...EVENT_FRESH });
  const login = viewer.data?.login;
  const replies = useQuery({ ...dashboardRepliesQuery(key), ...EVENT_FRESH });
  const state = useQuery({ ...userStateQuery(), ...EVENT_FRESH });
  const events = useInfiniteQuery({ ...threadEventsQuery(key), ...EVENT_FRESH });

  const all = [
    routeRead("comments", useInfiniteQuery(commentThreadsQuery(key))),
    routeRead("messages", useInfiniteQuery(messageThreadsQuery(key, "in"))),
    routeRead("asks", useInfiniteQuery(askThreadsQuery(key))),
  ];
  // Mine's keys name the reader's login, and with none they would be All's own, so its reads
  // stand off until the login has answered.
  const mineOn = opened.has("mine") && login !== undefined;
  const mine = [
    routeRead(
      "mine-comments",
      useInfiniteQuery({ ...commentThreadsQuery(key, login), enabled: mineOn })
    ),
    routeRead(
      "mine-messages",
      useInfiniteQuery({ ...messageThreadsQuery(key, "in", login), enabled: mineOn })
    ),
    routeRead("mine-asks", useInfiniteQuery({ ...askThreadsQuery(key, login), enabled: mineOn })),
  ];
  const agentsOn = opened.has("agents");
  const agents = [
    routeRead(
      "agents",
      useInfiniteQuery({ ...messageThreadsQuery(key, "none"), enabled: agentsOn })
    ),
  ];

  const allView = useMergedChoice({ on: true, routes: all, shown: shown.all });
  const mineView = useMergedChoice({
    mine: login,
    on: mineOn,
    routes: mine,
    shown: shown.mine,
  });
  const agentsView = useMergedChoice({ on: agentsOn, routes: agents, shown: shown.agents });
  const held = heldThreads(mineOn ? [...all, ...mine] : all);
  const settled = all.every((route) => route.pages !== undefined || route.error !== null);
  const waitingView = useWaitingChoice({
    held,
    heldSettled: settled,
    issueKey: key,
    on: opened.has("waiting"),
    rows: waitingRows(replies.data, project),
    shown: shown.waiting,
  });

  const marks: MarkReads | undefined =
    login === undefined ||
    replies.data === undefined ||
    state.data === undefined ||
    events.data === undefined
      ? undefined
      : {
          events: events.data,
          issueKey: key,
          issueOpen: issue.closed_at === null,
          reader: login,
          replies: replies.data,
          state: state.data,
        };
  const markFailures: FailedRead[] = [
    { id: "viewer", query: viewer },
    { id: "replies", query: replies },
    { id: "state", query: state },
    { id: "events", query: events },
  ].flatMap(({ id, query }) =>
    query.data === undefined && query.isError
      ? [
          {
            failure: query.errorUpdateCount,
            id,
            retry: () => void query.refetch(),
            retrying: query.isFetching,
          },
        ]
      : []
  );

  const choose = useCallback((next: ThreadChoice) => {
    setChoice(next);
    setOpened((before) => (before.has(next) ? before : new Set([...before, next])));
  }, []);
  const showNext = useCallback(
    () => setShown((before) => ({ ...before, [choice]: before[choice] + THREADS_PAGE })),
    [choice]
  );

  const views: Readonly<Record<ThreadChoice, ChoiceView>> = {
    agents: agentsView,
    all: allView,
    mine: mineView,
    waiting: waitingView,
  };
  const on = views[choice];
  const records = new Map<string, ThreadRecord>();
  for (const record of [
    ...[...held.values()].flat(),
    ...waitingView.read,
    ...(agentsView.rows ?? []),
  ]) {
    if (!records.has(record.id)) records.set(record.id, record);
  }
  return {
    choice,
    choose,
    marks,
    records,
    reading: [...all, ...mine, ...agents].some((route) => route.fetching),
    settled,
    showNext,
    view: {
      ...on,
      failed: [...markFailures, ...on.failed],
      rows: marks === undefined ? undefined : on.rows,
    },
  };
}

/** The line each choice draws where it holds no row. */
const EMPTY_LINES: Readonly<Record<ThreadChoice, string>> = {
  agents: EMPTY_STATE_LINES.threads_agents,
  all: EMPTY_STATE_LINES.threads,
  mine: EMPTY_STATE_LINES.threads_mine,
  waiting: EMPTY_STATE_LINES.threads_waiting,
};

/**
 * The list column (dispatch://INFRA-5704/spec#b-r-view, R2): the four choices, then the rows of
 * the choice that is on, then the control that shows the next 20 while rows are left, then New
 * thread. A failed read draws the page's failed-read line with its Retry above the rows, an
 * older build's answer its one sentence, and a choice with no row its empty line.
 *
 * Its groups, top to bottom: the choices; the lines of a read; the rows; the control under them;
 * New thread.
 */
export function ThreadList({
  issue,
  lists,
  onNewThread,
}: {
  readonly issue: IssueDetails;
  readonly lists: ThreadLists;
  /** Opens New thread in the main column (R10). */
  readonly onNewThread: () => void;
}): ReactNode {
  const { choice, choose, marks, showNext, view } = lists;
  const names = useRowNames(issue.project);
  return (
    <div className="flex min-w-0 flex-col gap-3" data-thread-list="">
      <ThreadChoices choice={choice} onChoose={choose} />
      {view.failed.map((read) => (
        <FailedReadLine
          failure={read.failure}
          key={read.id}
          message={ANSWER_LINES.threads_unreadable}
          onRetry={read.retry}
          retrying={read.retrying}
        />
      ))}
      {view.olderBuild ? (
        <p className={`text-sm ${pageMutedText}`} role="status" {...pageChrome}>
          {ANSWER_LINES.threads_older_build}
        </p>
      ) : null}
      {view.rows === undefined || marks === undefined ? null : view.rows.length === 0 ? (
        view.failed.length === 0 ? (
          <p className={`text-sm ${pageMutedText}`} {...pageChrome}>
            {EMPTY_LINES[choice]}
          </p>
        ) : null
      ) : (
        <ol className="min-w-0">
          {view.rows.map((record) => (
            <ThreadRow
              href={buildIssuePath({ key: issue.key, kind: "threads", record: record.id })}
              issueKey={issue.key}
              key={record.id}
              mark={recordMark(record, marks)}
              record={record}
              text={threadRow(record, names)}
            />
          ))}
        </ol>
      )}
      <ShowNext busy={view.busy} left={view.left} onShow={showNext} />
      <button
        className={`self-start rounded-lg border px-3 py-1 text-sm font-medium ${focusVisibleRing} ${pageLineBorder} ${pageMutedText}`}
        onClick={onNewThread}
        type="button"
        {...pageChrome}
      >
        {THREADS_WORDS.new_thread}
      </button>
    </div>
  );
}
