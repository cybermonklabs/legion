import { INBOX_THREAD_KINDS } from "@legion/contracts";

import {
  infiniteQueryOptions,
  keepPreviousData,
  partialMatchKey,
  type QueryFunctionContext,
  type QueryKey,
  queryOptions,
  replaceEqualDeep,
} from "@tanstack/react-query";

import { beginInboxFetch, finishInboxFetch } from "../features/inbox/ask-thread-freshness";
import { projectIssuesQueryKey } from "../features/project/issue-filters";
import {
  type ItemsWindow,
  LEGION_WORK_TIME,
  readLegionItems,
} from "../features/project/issues-legion";
import type { AdvancesListing } from "../features/project/issues-table-data";
import { type ClosedWindow, closedRange, holdsNoMoment, hourStart } from "../lib/closed-window";
import { compareTimestamps, isTimestamp } from "../lib/timestamps";

import { api, REFERENCE_TARGETS_MAX, REFERENCE_TARGETS_MAX_QUERY_CHARS } from "./client";
import { liveSocketFor } from "./live-socket";
import type {
  DeliveryLiveView,
  DeliveryMeasuresResponse,
  DeliveryPipelineResponse,
  DeliveryTimelineWithMeasures,
  EvalListResponse,
  InboxRow,
  IssueDetails,
  IssueSummary,
  IssueSummaryPage,
  LegionBoardResponse,
  MyRepliesResponse,
  ReferenceTarget,
} from "./types";

/** The signed-in principal (`/auth/whoami`). One key shared by every surface that reads it; it
 *  names the person by lowercase email, the form issue assignees carry. While it answers
 *  `EVENT_HEAD_UNAVAILABLE` it is asked again for as long as that lasts (`queryRetry`), so the
 *  shell shows its skeleton, never the error page. News that who is signed in may have changed
 *  asks it again through `askSignInAgain` (`query-client.ts`), never a bare invalidation. */
export const whoAmIQuery = () =>
  queryOptions({
    queryKey: ["whoami"],
    queryFn: () => api.whoAmI(),
  });

/** Every ask the viewer can see, as the Inbox lists them. */
export const inboxQuery = () =>
  queryOptions({
    queryKey: ["inbox"],
    queryFn: async ({ client }) => {
      const fetch = beginInboxFetch(client);
      let asks: InboxRow[] | undefined;
      try {
        asks = await api.getInbox();
        return asks;
      } finally {
        finishInboxFetch(client, fetch, asks);
      }
    },
  });

/** The viewer's per-issue read/pin/dismiss state. */
export const userStateQuery = () =>
  queryOptions({
    queryKey: ["user-state"],
    queryFn: () => api.getMyState(),
  });

/** Whether `next`, an issue put in the cache, is older than `cached`, the issue there: its
 *  `updated_at` names an earlier instant. */
function olderIssue(next: unknown, cached: unknown): boolean {
  const at = (issue: unknown) =>
    typeof issue === "object" && issue !== null ? Reflect.get(issue, "updated_at") : undefined;
  const nextAt = at(next);
  const cachedAt = at(cached);
  return (
    typeof nextAt === "string" &&
    typeof cachedAt === "string" &&
    isTimestamp(nextAt) &&
    isTimestamp(cachedAt) &&
    compareTimestamps(nextAt, cachedAt) < 0
  );
}

/**
 * One issue with its details (`GET /api/v1/issues/{key}`), the one query every view of an issue
 * reads. Dispatch answers a read with the issue as it was when the read reached it, so a read
 * already out when a write changed the issue - a slow GET started before a PATCH - can be answered
 * after the write's own answer was put in its place (`mergeIssue`). Such an answer is older than
 * the cached issue, and is dropped: the cache never takes an issue whose `updated_at` is earlier
 * than the one it holds, as the event log never lets a fetched page take back the events above
 * its head (`mergeEventPages`). The rule is the query's own, so it holds for every writer of the
 * key: a read from any of its views, a PATCH's answer, an optimistic pick and its rollback.
 */
export const issueQuery = (key: string | undefined) =>
  queryOptions({
    queryKey: ["issue", key],
    queryFn: (): Promise<IssueDetails> => {
      if (key === undefined) throw new Error("An issue read needs the issue's key.");
      return api.getIssue(key);
    },
    structuralSharing: (cached: unknown, next: unknown) =>
      olderIssue(next, cached) ? cached : replaceEqualDeep(cached, next),
  });

/** What each requirement of the issue's spec lacks (`GET /api/v1/issues/{key}/coverage`), which
 *  the Spec tab draws beside the requirement. It hangs under the issue's own key for the
 *  reader's sake, not for its invalidation: every issue target in `event-refresh` is `exact`,
 *  so the events that move it — a document version, a recorded result — name this key
 *  themselves. */
export const issueCoverageQuery = (key: string) =>
  queryOptions({
    queryKey: ["issue", key, "coverage"],
    queryFn: () => api.getIssueCoverage(key),
  });

/** The checks of the issue's Verification document and the results recorded against them
 *  (`GET /api/v1/issues/{key}/results`), which the Verification tab draws. Under the issue's key
 *  for the same reason as the coverage. */
export const issueResultsQuery = (key: string) =>
  queryOptions({
    queryKey: ["issue", key, "results"],
    queryFn: () => api.getIssueResults(key),
  });

/** Everyone who worked one issue, running or not (`GET /api/v1/issues/{key}/collaborators`).
 *  The Collaborators tab and the composer's "Not running" options read this one entry, so a tab
 *  read within the shell's `staleTime` opens the picker with no request of its own, and what
 *  refreshes one refreshes both. The event feed refreshes `["issue", key]` exactly, not as a
 *  prefix, so this key has a rule of its own there (`changesCollaborators`); the write paths that
 *  do invalidate the whole `["issue", key]` prefix reach it as well. The key takes `undefined`
 *  for the composer, whose own `enabled` holds the read until the picker opens on an issue
 *  owner. */
export const issueCollaboratorsQuery = (key: string | undefined) =>
  queryOptions({
    queryKey: ["issue", key, "collaborators"],
    queryFn: () => {
      if (key === undefined) throw new Error("A collaborators read needs the issue's key.");
      return api.getIssueCollaborators(key);
    },
  });

/** The step records Legion wrote of one issue (`GET /api/v1/issues/{key}/steps`), which the Steps
 *  tab draws. A step write appends no event of its own, so the event feed refreshes this key on
 *  the issue's events that come with a phase moving (`changesSteps` in `event-refresh.ts`), each
 *  naming it exactly as it names every issue target. The tab reads it through its catch-up,
 *  which every refresh that reaches the key starts, and through Retry
 *  (`features/steps/StepsTab.tsx`). */
export const issueStepsQuery = (key: string) =>
  queryOptions({
    queryKey: ["issue", key, "steps"],
    queryFn: () => api.listSteps(key),
  });

/** The live blocks of one document (`GET /api/v1/artifacts/{id}/blocks`): the document's own
 *  reference panel and the Plan tab's task list read the same key, so a tab opened after the
 *  document finds it answered. */
export const artifactBlocksQuery = (id: string) =>
  queryOptions({
    queryKey: ["artifact", id, "blocks"],
    queryFn: () => api.getArtifactBlocks(id),
  });

/** The batches `referenceTargetsQuery` reads `named` in, in its order: each at most
 *  `REFERENCE_TARGETS_MAX` references and at most `REFERENCE_TARGETS_MAX_QUERY_CHARS` characters
 *  of query string, written as the client writes it - `ref=<encoded>` for each reference, joined
 *  by `&` - except a batch of one address longer than that budget, which goes alone and passes
 *  it. Every reference is in exactly one batch. */
function referenceTargetBatches(named: readonly string[]): string[][] {
  const batches: string[][] = [];
  let batch: string[] = [];
  let written = 0;
  for (const ref of named) {
    // What this address writes into the query string: `ref=<encoded>`, after the `&` that
    // separates it from the address before it when the batch already holds one. Only such a
    // batch can be full, so the check below counts that `&`.
    const length = new URLSearchParams({ ref }).toString().length;
    const full =
      batch.length === REFERENCE_TARGETS_MAX ||
      written + 1 + length > REFERENCE_TARGETS_MAX_QUERY_CHARS;
    // An address longer than the whole budget still goes out, alone: a batch of none names
    // nothing at all, which is what this read is here to avoid.
    if (full && batch.length > 0) {
      batches.push(batch);
      batch = [];
      written = 0;
    }
    written += (batch.length > 0 ? 1 : 0) + length;
    batch.push(ref);
  }
  if (batch.length > 0) batches.push(batch);
  return batches;
}

/** What a names read (`referenceTargetsQuery`) holds: one member per address it names. */
export interface ReferenceTargetsAnswer {
  readonly targets: readonly ReferenceTarget[];
}

/** What the `dispatch://` references a page names point at (`GET /api/v1/reference-targets`): a
 *  page that names many references reads their names with this query, in as few requests as
 *  both bounds of one request allow. The key holds the references themselves, sorted, so two
 *  surfaces naming the same set share one answer and a set that grows by one is a read of its
 *  own rather than a stale hit. An empty set is never read: `enabled` is off, because a read of
 *  nothing still costs a request.
 *
 *  A set past either bound of one request is read in batches (`referenceTargetBatches`), every
 *  address named in exactly one of them, so a page naming more references than one request
 *  carries draws every row with its words. The bounds are the server's cap
 *  (`REFERENCE_TARGETS_MAX`, which it refuses with `400 CAP_EXCEEDED`) and the client's budget
 *  for the request line (`REFERENCE_TARGETS_MAX_QUERY_CHARS`), which a full batch of comment or
 *  message addresses passes. A set inside both bounds is one request. The batches are built
 *  when the query runs, not when its options are made.
 */
export const referenceTargetsQuery = (refs: readonly string[]) => {
  const named = [...new Set(refs)].sort();
  return queryOptions({
    enabled: named.length > 0,
    queryKey: ["reference-targets", named],
    queryFn: async (): Promise<ReferenceTargetsAnswer> => {
      const answers = await Promise.all(
        referenceTargetBatches(named).map((batch) => api.getReferenceTargets(batch))
      );
      return { targets: answers.flatMap((answer) => answer.targets ?? []) };
    },
  });
};

/** The viewer's per-agent conversation state: each session's Clear, read mark, count of unread
 *  replies to the viewer's direct messages, and `pinned_at` where the viewer pinned it
 *  (`features/agents/pins.ts`). The navigation, the Agents page, and the live view all read this
 *  one key, and `user_agent_state.updated` refreshes it, so a pin or a read mark another browser
 *  of the same person made shows here without a reload. A session the viewer has only pinned is
 *  in the answer too. */
export const userAgentStateQuery = () =>
  queryOptions({
    queryKey: ["user-agent-state"],
    queryFn: () => api.getMyAgentState(),
  });

/** The prefix every thread query shares: the Inbox's own rows, the unread count, and each
 *  owner's unread set. One invalidation of it refreshes them all. */
export const myRepliesKey = ["me", "replies"] as const;

/** One Inbox thread's ask, with its replies and its answer: the row's body in the list and in
 *  the reading pane read the one copy under this key. It never goes stale on its own - an event
 *  refresh is what moves it (`api/event-refresh.ts`). */
export const askThreadQuery = (threadId: string) =>
  queryOptions({
    queryKey: ["ask-thread", threadId],
    queryFn: () => api.getAsk(threadId),
    staleTime: Number.POSITIVE_INFINITY,
  });

/** The viewer's count of unread threads, from the one-row page's `total`: the sidebar's and the
 *  compact header's one Inbox number counts it beside the work waiting on them. One request per
 *  page load, never the unpaged read below. */
export const myRepliesCountQuery = () =>
  queryOptions({
    queryKey: [...myRepliesKey, "count"],
    queryFn: async () => (await api.listMyReplies({ kind: INBOX_THREAD_KINDS, limit: 1 })).total,
  });

/** The prefix both of the Inbox's thread reads share - its own list and what the reader has
 *  dismissed out of it: what a refresh rule tests the listed rows under, whichever view the page
 *  is on (`api/event-refresh.ts`). */
export const inboxThreadsKey = [...myRepliesKey, "inbox"] as const;

/** Every thread row in the reader's Inbox, in one unpaged read: the Reply and Agent rows of the
 *  list, read or not, less what they have dismissed. A thread stays here until it is dismissed,
 *  so reading one leaves its row where it is. The Inbox page and its drawer read this and
 *  nothing else does - a surface that wants one owner's unread set reads `myRepliesOwnerQuery`
 *  below. */
export const inboxThreadsQuery = () =>
  queryOptions({
    queryKey: [...inboxThreadsKey, "inbox"],
    queryFn: () => api.listMyReplies({ kind: INBOX_THREAD_KINDS, state: "inbox" }),
  });

/** What the reader has dismissed out of their Inbox, newest dismissal first, which the View
 *  menu's **Dismissed** draws: `INBOX_DISMISSED_PAGE` rows a page, each next page read from the
 *  last one's `next_cursor` by the list's own **Show more**. */
export const dismissedThreadsQuery = () =>
  infiniteQueryOptions({
    queryKey: [...inboxThreadsKey, "dismissed"],
    queryFn: ({ pageParam }) =>
      api.listMyReplies({
        kind: INBOX_THREAD_KINDS,
        state: "dismissed",
        ...(pageParam === undefined ? {} : { cursor: pageParam }),
      }),
    initialPageParam: undefined as string | undefined,
    getNextPageParam: (last: MyRepliesResponse) => last.next_cursor,
  });

/** One owner's unread threads, which every thread mounted on that owner's page reads to learn
 *  whether it is unread: one request however many threads the page shows. */
export const myRepliesOwnerQuery = (
  owner: { readonly issue: string } | { readonly artifact: string }
) =>
  queryOptions({
    queryKey: [...myRepliesKey, owner],
    queryFn: () => api.listMyReplies({ ...owner, limit: 200 }),
  });

/** One session's stored conversations: the direct messages and issue messages sent to it, with
 *  their replies. The Agents page row and the live view read this one key. */
export const agentMessagesQuery = (sessionId: string) =>
  queryOptions({
    queryKey: ["agents", sessionId, "messages"],
    queryFn: () => api.listAgentMessages(sessionId),
  });

/** Every native project, shared by the shell, project routes, and settings. */
export const projectsQuery = () =>
  queryOptions({
    queryKey: ["projects"],
    queryFn: () => api.listProjects(),
  });

/** The two settings the bar reads before it offers a person's typed words as a new issue: the
 *  label that issue carries and the session it is routed to. One read for the session: the
 *  deployment changes it only by restarting. */
export const newIssueSettingsQuery = () =>
  queryOptions({
    queryKey: ["new-issue-settings"],
    queryFn: () => api.getNewIssueSettings(),
  });

/** Everyone who has signed in (`GET /api/v1/users`), sorted by email and kept for the session:
 *  the issue header's assignee picker and the Inbox's assignee filter choose from this one list.
 *  Each call site keeps its own `enabled` - the list is read only once a reader reaches for a
 *  control that needs it - so no page fetches `/api/v1/users` just by rendering. */
export const usersQuery = () =>
  queryOptions({
    queryKey: ["users"],
    queryFn: () => api.listUsers(),
    staleTime: Number.POSITIVE_INFINITY,
  });

/** Every configured architecture source, shared by its settings table and invalidations. */
export const architectureSourcesQuery = () =>
  queryOptions({
    queryKey: ["architecture-sources"],
    queryFn: () => api.listArchitectureSources(),
  });

/** One project's architecture sources (`[]` when it has none), read by the project page's
 *  open-on rule and the issue components line, which shares it. Its key sits under
 *  `architectureSourcesQuery`'s, so refreshing that list refreshes every project's too. */
export const projectArchitectureSourcesQuery = (project: string) =>
  queryOptions({
    queryKey: ["architecture-sources", project],
    queryFn: () => api.listProjectArchitectureSources(project),
  });

/** Every project with the person it defaults to, one row per project: every project names one,
 *  and this read is the one place the page learns who, since the project list carries no default.
 *  Read by the Settings page's Projects table, and refreshed by the event a default write appends
 *  and by a project's creation. A project absent from the answer is one created since the read. */
export const projectDefaultAssigneesQuery = () =>
  queryOptions({
    queryKey: ["project-default-assignees"],
    queryFn: () => api.listProjectDefaultAssignees(),
  });

/** The mutation key every board move on `project` carries; `projectIssuesQuery` waits on it. */
export const boardMoveMutationKey = (project: string): readonly unknown[] => [
  "board-move",
  project,
];

/**
 * A listing's structural sharing, by issue key rather than by place. TanStack's own
 * (`replaceEqualDeep`) pairs a new read's issues with the cached ones by index, so one issue that
 * moves up a listing hands every issue it passed a new object, though none of them changed.
 * Paired by key, each issue the read did not change keeps the object the page already holds, so
 * a memoised Board card of it draws nothing again; a read that changed nothing keeps the cached
 * list itself.
 */
function shareIssuesByKey(cached: unknown, next: unknown): unknown {
  // A listing query holds the issues `listIssues` answers, and nothing before its first answer.
  const held = cached as readonly IssueSummary[] | undefined;
  const read = next as readonly IssueSummary[];
  if (held === undefined) return read;
  const byKey = new Map(held.map((issue) => [issue.key, issue]));
  let same = held.length === read.length;
  const shared = read.map((issue, index) => {
    const before = byKey.get(issue.key);
    const kept = before === undefined ? issue : replaceEqualDeep(before, issue);
    if (kept !== held[index]) same = false;
    return kept;
  });
  return same ? held : shared;
}

/** `shareIssuesByKey` for a page of a listing (`listIssuePage`): its `issues` paired by key, and
 *  the cached page itself where its issues and its numbers (`total`, `limit`, `offset`) are the
 *  same. The issues are not handed to `replaceEqualDeep` again, which would pair them by place. */
function sharePageByKey(cached: unknown, next: unknown): unknown {
  // A page query holds the pages `listIssuePage` answers, and nothing before its first answer.
  const held = cached as IssueSummaryPage | undefined;
  const read = next as IssueSummaryPage;
  if (held === undefined) return read;
  const issues =
    read.issues === null
      ? null
      : (shareIssuesByKey(held.issues ?? undefined, read.issues) as IssueSummary[]);
  const same =
    issues === held.issues &&
    read.total === held.total &&
    read.limit === held.limit &&
    read.offset === held.offset;
  return same ? held : { ...read, issues };
}

/**
 * A project's issue list, plain or open only (`open`): the Issues tab reads it open only, for its
 * table and its Board alike, and the issue header's label picker reads it plain. Every observer
 * sets the query's function, so each one takes it from here. A fetch that starts while a board
 * move on the project is unanswered waits for the answer: until then the server may not have
 * applied the move, and its list would put the card back where it came from.
 */
export const projectIssuesQuery = (project: string, open = false) =>
  queryOptions({
    structuralSharing: shareIssuesByKey,
    queryKey: [...projectIssuesQueryKey(project), ...(open ? ["open"] : [])],
    queryFn: async (context) => {
      await boardMovesSettled(context, project);
      return api.listIssues({ project, ...(open ? { open: true } : {}) });
    },
  });

/** Every done issue of a project that a Closed window holds, or every done issue where the view
 *  names no window: what a view that lists done rows reads when the browser sorts, narrows and
 *  pages them. A rolling window is read from the start of the hour of its first moment
 *  (`hourStart`). A window that holds no moment holds no done issue, so the route, which refuses
 *  it, is not asked (`holdsNoMoment`). Under the project listing's prefix, so the events that read
 *  the listing again read this again too, and a board move waits as it does there. */
export const projectDoneIssuesQuery = (project: string, window: ClosedWindow | undefined) => {
  const range = closedRange(hourStart(window));
  return queryOptions({
    structuralSharing: shareIssuesByKey,
    queryKey: [
      ...projectIssuesQueryKey(project),
      "done",
      range.closed_since ?? "",
      range.closed_before ?? "",
    ],
    queryFn: async (context) => {
      if (holdsNoMoment(window)) return [];
      await boardMovesSettled(context, project);
      return api.listIssues({ project, status: ["done"], ...range });
    },
  });
};

/** The order Done's page read asks the list route for, which its answer must name. */
const DONE_PAGE_SORT = "-closed_at";

/** The failure of a page read of Done whose answer does not name `DONE_PAGE_SORT` as its `sort`.
 *  A server older than `sort` ignores it and answers the route's own order, so its page holds the
 *  first done issues by rank, not the newest closes. No rule on the rows can tell such a page from
 *  a sorted one, since that server can write `closed_at` too. */
export class UnsortedPageError extends Error {
  constructor() {
    super("The list route answered a page of closed issues without the sort it was sent.");
    this.name = "UnsortedPageError";
  }
}

/** One page of a project's done issues, newest close first, with the count of every done issue
 *  the window holds (`total`): what Done reads while it is sorted by Closed and narrowed by
 *  nothing but a Closed window. `size` is the page's rows and `offset` the rows before it. The
 *  read sends the window's own first moment, so its rows and its `total` are the window's. For a
 *  rolling window that moment moves with the clock the caller reads, and each new moment is a new
 *  read. A window that holds no moment is an empty page that asks the route nothing
 *  (`holdsNoMoment`). An answer that does not name the sort the read sent fails the read
 *  (`UnsortedPageError`), so the query never holds such a page and no count is taken from it.
 *  The shared retry policy reads it again twice, as any failed read (`queryRetry`). Under the
 *  project listing's prefix, as `projectDoneIssuesQuery`. */
export const projectDonePageQuery = (
  project: string,
  page: { readonly offset: number; readonly size: number; readonly window?: ClosedWindow }
) => {
  const range = closedRange(page.window);
  return queryOptions({
    structuralSharing: sharePageByKey,
    queryKey: [
      ...projectIssuesQueryKey(project),
      "done-page",
      range.closed_since ?? "",
      range.closed_before ?? "",
      page.size,
      page.offset,
    ],
    queryFn: async (context) => {
      if (holdsNoMoment(page.window))
        return { issues: [], limit: page.size, offset: page.offset, total: 0 };
      await boardMovesSettled(context, project);
      const answer = await api.listIssuePage({
        project,
        status: ["done"],
        sort: DONE_PAGE_SORT,
        limit: page.size,
        offset: page.offset,
        ...range,
      });
      if (answer.sort !== DONE_PAGE_SORT) throw new UnsortedPageError();
      return answer;
    },
  });
};

/** Whether two keys of `projectDonePageQuery` read one window of one project's done issues: they
 *  differ in no part but the page's size and offset, the key's last two. */
export const sameDoneWindow = (key: QueryKey, other: QueryKey): boolean =>
  partialMatchKey(key, other.slice(0, -2));

/** How many open issues a project has: the `total` of a one-row page of its open listing, which
 *  Open's tab counts by on a view that reads no open listing (Done). Under the project listing's
 *  prefix, so a live event on an issue reads it again. */
export const projectOpenCountQuery = (project: string) =>
  queryOptions({
    queryKey: [...projectIssuesQueryKey(project), "open-count"],
    queryFn: async (context) => {
      await boardMovesSettled(context, project);
      return (await api.listIssuePage({ project, open: true, limit: 1 })).total;
    },
  });

/**
 * What a query under a project's listing prefix (`projectIssuesQueryKey(project)`) holds where it
 * holds rows, by the query that writes it: a listing's issues (`projectIssuesQuery`,
 * `projectDoneIssuesQuery`, and an advances listing's `advancesIssuesQuery` too), or a page of a
 * listing with its rows in `issues` (`projectDonePageQuery`). Open's count
 * (`projectOpenCountQuery`), a number under the same prefix, is no shape of it.
 */
export type IssueListData =
  | { readonly kind: "listing"; readonly issues: readonly IssueSummary[] }
  | {
      readonly kind: "page";
      readonly issues: readonly IssueSummary[];
      readonly page: IssueSummaryPage;
    };

/** `data`, what a query under a project's listing prefix holds, read by its shape. Undefined for
 *  no answer yet, for Open's count and for any other value; a page whose `issues` is null holds
 *  no row, and reads as no shape. */
export function readIssueList(data: unknown): IssueListData | undefined {
  if (Array.isArray(data)) return { kind: "listing", issues: data as IssueSummary[] };
  if (typeof data !== "object" || data === null || !("issues" in data)) return undefined;
  return Array.isArray(data.issues)
    ? { kind: "page", issues: data.issues as IssueSummary[], page: data as IssueSummaryPage }
    : undefined;
}

/** The rows `data` holds where it is a listing or a page of one (`readIssueList`), else
 *  undefined. */
export function issueListRows(data: unknown): readonly IssueSummary[] | undefined {
  return readIssueList(data)?.issues;
}

/** Resolves once no board move on `project` is pending; rejects if the fetch is cancelled first. */
function boardMovesSettled(context: QueryFunctionContext, project: string): Promise<void> {
  const { client } = context;
  const filters = { mutationKey: boardMoveMutationKey(project) };
  if (client.isMutating(filters) === 0) {
    return Promise.resolve();
  }
  // Read only here: once a fetch reads its signal, query-core cancels it when the last observer
  // unmounts, which a fetch that is not waiting has no reason to invite.
  const { signal } = context;
  const settled = Promise.withResolvers<void>();
  const stop = () => {
    unsubscribe();
    signal.removeEventListener("abort", abort);
  };
  const abort = () => {
    stop();
    settled.reject(signal.reason);
  };
  const unsubscribe = client.getMutationCache().subscribe(() => {
    if (client.isMutating(filters) === 0) {
      stop();
      settled.resolve();
    }
  });
  signal.addEventListener("abort", abort);
  return settled.promise;
}

/** What `GET /api/v1/issues` is asked for one advances listing: the issues advancing it, only
 *  those whose own row names it where it asks for that (`own`), in the statuses it names, with
 *  their move into backlog or icebox where it asks for that. `open` leaves the closed issues
 *  out, as the project listing's own flag does. */
export const advancesIssuesParams = (listing: AdvancesListing, open = false) => ({
  advances: [listing.advances],
  ...(listing.own ? { own: true } : {}),
  ...(listing.statuses === undefined ? {} : { status: [...listing.statuses] }),
  ...(listing.moved ? { moved: true } : {}),
  ...(open ? { open: true } : {}),
});

/** Every issue an advances listing holds, as the server narrows the listing itself
 *  (`GET /api/v1/issues?advances=<id>`, `none` or `any`). A priority belongs to no project, so
 *  the read names none: the work under it is the whole deployment's. No board move gates it - a
 *  move is one project's, and this listing is of no project. Every key sits under
 *  `["issues", "advances"]`, the prefix the event feed refreshes when an `advances`, a status
 *  or a parent moves; a deleted priority refreshes the listing under it and the work under none,
 *  and every listing whose rows advance it (`priorityWriteTargets`, `api/event-refresh.ts`). */
export const advancesIssuesQuery = (listing: AdvancesListing, open = false) =>
  queryOptions({
    structuralSharing: shareIssuesByKey,
    // Typed as widely as the project listing's key (`projectIssuesQueryKey`), so the table that
    // reads one or the other sees one options type rather than two that differ by their key.
    queryKey: [
      "issues",
      "advances",
      listing.advances,
      ...(listing.own ? ["own"] : []),
      ...(listing.statuses === undefined ? [] : ["status", ...listing.statuses]),
      ...(listing.moved ? ["moved"] : []),
      ...(open ? ["open"] : []),
    ] as unknown[],
    queryFn: () => api.listIssues(advancesIssuesParams(listing, open)),
  });

/** Every live priority with the work under each and the two faults beside them
 *  (`GET /api/v1/priorities`), in rank order. `projects` narrows the counts to the work of those
 *  projects and nothing else: the rows are the deployment's whichever projects are named. Its
 *  key sits under the summary's prefix, so one refresh moves both reads (`api/event-refresh.ts`). */
export const prioritiesQuery = (projects: readonly string[] = []) =>
  queryOptions({
    queryKey: ["priorities", "full", ...projects],
    queryFn: () => api.listPriorities(projects.length ? { project: [...projects] } : {}),
  });

/** The two fault counts alone (`GET /api/v1/priorities?summary=true`): the work under no
 *  priority and the work parked under one, which the navigation rail shows without reading a
 *  row or an issue of either list. */
export const prioritySummaryQuery = (projects: readonly string[] = []) =>
  queryOptions({
    queryKey: ["priorities", "summary", ...projects],
    queryFn: () =>
      api.listPriorities({ summary: true, ...(projects.length ? { project: [...projects] } : {}) }),
  });

/** One priority's own history, newest first (`GET /api/v1/priorities/{id}/history`): each write
 *  of its title, description, owner and order, and each `advances` relation an issue asserted or
 *  retracted on it, which the priority page's margin lists. Its key sits under the same
 *  `priorities` prefix as the rows, so the one refresh rule for `advances`, `status` and
 *  `parent`, and the one for `priority.updated`, read it again with them (`api/event-refresh.ts`). */
export const priorityHistoryQuery = (id: string) =>
  queryOptions({
    queryKey: ["priorities", "history", id],
    queryFn: () => api.getPriorityHistory(id),
  });

/** What every live-owned query shares (LEGION-690): the delivery timeline, measures and pipeline
 *  (`deliveryTimelineQuery`, `deliveryMeasuresQuery`, `deliveryPipelineQuery`) and the Legion
 *  board (`legionBoardQuery`). Its first load subscribes its view over the page's one live socket
 *  (`api/live-socket.ts`), which writes the snapshot and every later change into the cache, so
 *  nothing here reads again. `meta.live` keeps the event feed's refresh paths away from it
 *  (`api/query-refresh.ts`); `gcTime` 0 drops the subscription with the last observer. The
 *  timeline and the measures keep the previous answer on screen while a new view's snapshot is
 *  out (`keepPreviousData`), as a filter change in the prototype they port never blanks it. */
const LIVE = {
  gcTime: 0,
  meta: { live: true },
  refetchOnReconnect: false,
  refetchOnWindowFocus: false,
  staleTime: Number.POSITIVE_INFINITY,
} as const;

/** The body each live view's kind is answered with: the GET route's it mirrors. */
interface DeliveryLiveBodies {
  readonly timeline: DeliveryTimelineWithMeasures;
  readonly measures: DeliveryMeasuresResponse;
  readonly pipeline: DeliveryPipelineResponse;
  readonly legion: LegionBoardResponse;
  readonly evals: EvalListResponse;
}

/** The live view's answer, which is the body of the GET route its kind mirrors, for every kind
 *  the socket serves. */
function subscribeLive<K extends keyof DeliveryLiveBodies>(
  context: QueryFunctionContext,
  view: DeliveryLiveView & { readonly kind: K }
): Promise<DeliveryLiveBodies[K]> {
  // The socket answers each view with its own route's body (packages/contracts DeliveryLiveView);
  // the frame carries the union, and the view's kind names the member.
  return liveSocketFor(context.client).subscribe(context.queryKey, view) as Promise<
    DeliveryLiveBodies[K]
  >;
}

/** The delivery timeline, live: the window, the search and every facet, applied by Dispatch
 *  (LEGION-567's plan, "API"), keyed by the view so a filter change subscribes its own entry. */
export const deliveryTimelineQuery = (view: DeliveryLiveView & { kind: "timeline" }) =>
  queryOptions({
    ...LIVE,
    placeholderData: keepPreviousData,
    queryKey: ["delivery", "timeline", view],
    queryFn: (context) => subscribeLive(context, view),
  });

/** The measures panel's own read for a brushed window, live, under `["delivery"]` beside the
 *  timeline and by the same search and facets. */
export const deliveryMeasuresQuery = (view: DeliveryLiveView & { kind: "measures" }) =>
  queryOptions({
    ...LIVE,
    placeholderData: keepPreviousData,
    queryKey: ["delivery", "measures", view],
    queryFn: (context) => subscribeLive(context, view),
  });

/** One deploy-repository run with every job it ran, read by GET. A live delta that changes the
 *  run invalidates this key, so an open drill-down reads it again. */
export const deliveryRunQuery = (id: number) =>
  queryOptions({
    queryKey: ["delivery", "run", id],
    queryFn: () => api.getDeliveryRun(id),
  });

/** The Pipeline page, live, by `at` so a pinned day is its own subscription. */
export const deliveryPipelineQuery = (view: DeliveryLiveView & { kind: "pipeline" }) =>
  queryOptions({
    ...LIVE,
    queryKey: ["delivery", "pipeline", view],
    queryFn: (context) => subscribeLive(context, view),
  });

/** The delivery timeline's configuration record, or `null` until someone sets it: the Settings
 *  section's form reads it, while the Delivery page's setup form starts from the `null` the
 *  timeline's `DELIVERY_NOT_CONFIGURED` already implies and reads nothing. */
export const deliverySettingsQuery = () =>
  queryOptions({
    queryKey: ["delivery-settings"],
    queryFn: () => api.getDeliverySettings(),
  });

/** Where the next page of an offset-paged list starts, for an infinite query's
 *  `getNextPageParam`: after the rows this page answered, and nowhere once those reach the
 *  total the answer carries. `GET /api/v1/me/answers` and `GET /api/v1/people/{login}/nodes`
 *  both answer that shape (api.myAnswersQuery), so both pages read their next page from here. */
export const nextOffsetPage = (last: {
  offset: number;
  rows?: readonly unknown[] | null;
  total: number;
}): number | undefined => {
  const read = last.offset + (last.rows ?? []).length;
  return read < last.total ? read : undefined;
};

/** One project's Legion daemon, or `null` for a project that names none — not a failure. Each
 *  project is its own entry: the Settings page's Legion section reads one entry per project
 *  (`useLegionDaemons`), since no route lists them. */
export const legionDaemonQuery = (project: string) =>
  queryOptions({
    queryKey: ["legion-daemon", project],
    queryFn: () => api.getLegionDaemon(project),
  });

/** A project's Legion board, live (`GET /api/v1/projects/{key}/legion/board`): one subscription
 *  per project, since the board has no window and the project is all it reads. Its snapshot and
 *  each delta carry every column of cards in the board's order, with the Queue column in the
 *  daemon's own order, the header (capacity, the queue's length, the last read and whether it
 *  failed), and the `issues` map of stored facts by issue key, so a daemon read or a facts write
 *  of an issue the board shows reaches the page at once. TanStack Query's own sharing keeps what
 *  a frame did not change. The generated types let `columns`, each column's `cards` and `issues`
 *  be null, so every reader of them reads them null-safe. The Issues tab subscribes it only for a
 *  project whose daemon setting names a daemon (`legionDaemonQuery`). */
export const legionBoardQuery = (project: string) => {
  const view = { kind: "legion", project } as const;
  return queryOptions({
    ...LIVE,
    queryKey: ["legion", "board", view],
    queryFn: (context) => subscribeLive(context, view),
  });
};

/** The Legion facts of a project's closed issues that closed in `windows`, by issue key: every
 *  issue row of `legion-work-time`'s items route over those windows, read page by page
 *  (`readLegionItems`). A closed row the board's `issues` map does not hold takes its facts from
 *  here (`legionFactsOf`). The read keeps the app's shared retry rule (`queryRetry`), and a
 *  refusal, `404 EVAL_NOT_FOUND` among them, is a failed read, never facts of no Legion work. */
export const legionItemsQuery = (project: string, windows: readonly ItemsWindow[]) =>
  queryOptions({
    queryKey: ["legion", "items", project, windows],
    queryFn: () =>
      readLegionItems(windows, (query) => api.listEvalItems(project, LEGION_WORK_TIME, query)),
  });
