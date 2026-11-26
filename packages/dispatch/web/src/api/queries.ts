import { INBOX_THREAD_KINDS } from "@legion/contracts";

import {
  infiniteQueryOptions,
  keepPreviousData,
  type QueryClient,
  type QueryFunctionContext,
  queryOptions,
  replaceEqualDeep,
} from "@tanstack/react-query";

import { beginInboxFetch, finishInboxFetch } from "../features/inbox/ask-thread-freshness";
import { projectIssuesQueryKey } from "../features/project/issue-filters";
import type { AdvancesListing } from "../features/project/issues-table-data";
import { compareTimestamps, isTimestamp } from "../lib/timestamps";

import { api, REFERENCE_TARGETS_MAX, REFERENCE_TARGETS_MAX_QUERY_CHARS } from "./client";
import { liveSocketFor } from "./live-socket";
import type {
  DeliveryLiveView,
  DeliveryLiveWindow,
  EvalItemsResponse,
  EvalListResponse,
  InboxRow,
  IssueDetails,
  MyRepliesResponse,
  ProjectTimelineResponse,
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
 * A project's issue list - plain, or server-filtered by labels - shared by the Board, the List,
 * the filter strip and the issue header's label picker. Every observer sets the query's function,
 * so each one takes it from here. A fetch that starts while a board move on the project is
 * unanswered waits for the answer: until then the server may not have applied the move, and its
 * list would put the card back where it came from.
 */
export const projectIssuesQuery = (project: string, labels: readonly string[], open = false) =>
  queryOptions({
    queryKey: [...projectIssuesQueryKey(project, labels), ...(open ? ["open"] : [])],
    queryFn: async (context) => {
      await boardMovesSettled(context, project);
      return api.listIssues({
        project,
        ...(labels.length ? { label: [...labels] } : {}),
        ...(open ? { open: true } : {}),
      });
    },
  });

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

/** What every live-owned query of the Evals tab shares (LEGION-690): its first load subscribes
 *  its view over the page's one live socket (`api/live-socket.ts`), which writes the snapshot and
 *  every later change into the cache, so nothing here reads again. `meta.live` keeps the event
 *  feed's refresh paths away from it (`api/query-refresh.ts`); `gcTime` 0 drops the subscription
 *  with the last observer; the page keeps the previous answer on screen while a new view's
 *  snapshot is out, as a filter change in the prototype it ports never blanks it. */
const LIVE = {
  gcTime: 0,
  meta: { live: true },
  refetchOnReconnect: false,
  refetchOnWindowFocus: false,
  staleTime: Number.POSITIVE_INFINITY,
} as const;

/** The body each live view's kind is answered with: the GET route's it mirrors. */
interface DeliveryLiveBodies {
  readonly evals: EvalListResponse;
  readonly items: EvalItemsResponse;
  readonly "project-timeline": ProjectTimelineResponse;
}

/** The live view's answer, which is the body of the GET route its kind mirrors. Only the views
 *  the Evals tab subscribes - evals, items and project-timeline - are keyed here; the socket
 *  serves other kinds no page reads. */
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

/** One run of a project's deploy source with every job it ran and its record (deploy row and
 *  failures), read by GET from the repository its timeline row names (dispatch://INFRA-2918,
 *  D14). A project-timeline delta that changes the run's row invalidates this key, so an open
 *  drill-down reads it again, as each write of its record does. */
export const projectDeployRunQuery = (project: string, repo: string, id: number) =>
  queryOptions({
    queryKey: ["project-deploy-run", project, repo, id],
    queryFn: () => api.getProjectDeployRun(project, id, repo),
  });

/** The kinds of eval, each read by a view of its own on the Evals tab. */
export type EvalKind = "delivery" | "legion" | "ci" | "experiment";

/** How many evals one evals view answers: the eval list's most (`evalPageMax`, api/evals.go). A
 *  kind with more is read by a second view at the next offset. */
export const EVALS_VIEW_LIMIT = 200;

/** One evals view of a project's Evals tab: its window, the compare range (null for none), the
 *  repositories whose day rows every number folds (none: the project's own), the merge timeline's
 *  facets but the repository and its search (`narrowing`), the kind it keeps, and the offset of its
 *  page. */
export interface EvalsViewParams {
  readonly project: string;
  readonly window: DeliveryLiveWindow;
  readonly compare: { readonly from: string; readonly to: string } | null;
  readonly repos: readonly string[];
  /** The facets but the repository and the search that narrow the evals counting pull requests
   *  (dispatch://INFRA-2918/artifact/plan-md, D7a), as the timeline's query names them: `q`, then
   *  each facet's key once per value. Empty or absent: none, and every number reads the stored
   *  day rows. */
  readonly narrowing?: string;
  readonly kind: EvalKind;
  readonly offset: number;
}

/** The prefix of every read of one project's evals on its Evals tab (its evals, items and
 *  project-timeline views and its evals' detail reads), which a write of the project's eval rows
 *  invalidates: a delivery settings save writes each target it changes as a new version of that
 *  project's eval. */
export const evalsProjectKey = (project: string) => ["evals", project] as const;

/** One project's evals of one kind, live (dispatch://INFRA-2918/artifact/plan-md, D3): the body of
 *  `GET /api/v1/projects/{key}/evals` for the window, sent as the view's window, and the rest of
 *  its query, sent as the view's query, a page of EVALS_VIEW_LIMIT at `offset`. Keyed under the
 *  project's evals (`evalsProjectKey`) by the view, so a change of the window, the compare
 *  range, the repositories or the narrowing subscribes its own entry. Only a delivery view carries
 *  the narrowing: the evals it narrows are delivery evals (D7a), so a view of another kind answers
 *  the same with or without it, and a facet change subscribes no such view again. */
export const evalsViewQuery = (params: EvalsViewParams) => {
  const query = new URLSearchParams();
  if (params.compare !== null) {
    query.set("compare_from", params.compare.from);
    query.set("compare_to", params.compare.to);
  }
  for (const repo of params.repos) query.append("repo", repo);
  if (params.kind === "delivery") {
    for (const [key, value] of new URLSearchParams(params.narrowing)) query.append(key, value);
  }
  query.set("kind", params.kind);
  query.set("limit", String(EVALS_VIEW_LIMIT));
  if (params.offset > 0) query.set("offset", String(params.offset));
  const view = {
    kind: "evals",
    project: params.project,
    window: params.window,
    query: query.toString(),
  } as const;
  return queryOptions({
    ...LIVE,
    queryKey: [...evalsProjectKey(params.project), "view", view],
    queryFn: (context) => subscribeLive(context, view),
  });
};

/** One items view of a project's Evals tab: the eval whose rows it shows (`slug`), its window and
 *  compare range (null for none), the repositories, and what of the rows it keeps: one series,
 *  one row kind, one row, pr-ci's jobs in one order (`sort`), and the page, `limit` rows at
 *  `offset`. */
export interface ItemsViewParams {
  readonly project: string;
  readonly slug: string;
  readonly window: DeliveryLiveWindow;
  readonly compare: { readonly from: string; readonly to: string } | null;
  readonly repos: readonly string[];
  readonly series?: string;
  readonly rowKind?: string;
  readonly row?: string;
  readonly sort?: "median" | "failures";
  readonly limit: number;
  readonly offset?: number;
}

/** The rows behind one eval's numbers, live (dispatch://INFRA-2918/artifact/plan-md, D9): the body
 *  of `GET /api/v1/projects/{key}/evals/{slug}/items` for the window, sent as the view's window, and
 *  the rest of its query, sent as the view's query. Keyed under the project's evals by the view,
 *  as `evalsViewQuery` is, so a change of any of them subscribes its own entry. */
export const itemsViewQuery = (params: ItemsViewParams) => {
  const query = new URLSearchParams();
  if (params.compare !== null) {
    query.set("compare_from", params.compare.from);
    query.set("compare_to", params.compare.to);
  }
  if (params.series !== undefined) query.set("series", params.series);
  if (params.rowKind !== undefined) query.set("row_kind", params.rowKind);
  if (params.row !== undefined) query.set("row", params.row);
  for (const repo of params.repos) query.append("repo", repo);
  if (params.sort !== undefined) query.set("sort", params.sort);
  query.set("limit", String(params.limit));
  if (params.offset !== undefined && params.offset > 0) query.set("offset", String(params.offset));
  const view = {
    kind: "items",
    project: params.project,
    slug: params.slug,
    window: params.window,
    query: query.toString(),
  } as const;
  return queryOptions({
    ...LIVE,
    queryKey: [...evalsProjectKey(params.project), "items", view],
    queryFn: (context) => subscribeLive(context, view),
  });
};

/** How many rows a project-timeline view holds when it is opened, and how many more each growth
 *  asks for (dispatch://INFRA-2918/artifact/plan-md, D8). */
export const PROJECT_TIMELINE_PAGE = 200;

/** One project-timeline view of a project's Evals tab: its window, and its facets and search as
 *  `GET /api/v1/projects/{key}/timeline` takes them (`q` and each facet's key), without the
 *  window and the limit, which the view names apart. */
export interface ProjectTimelineViewParams {
  readonly project: string;
  readonly window: DeliveryLiveWindow;
  readonly query: string;
}

/** The project-timeline view of `params` that holds its `limit` newest rows. */
function projectTimelineView(params: ProjectTimelineViewParams, limit: number) {
  const query = new URLSearchParams(params.query);
  query.set("limit", String(limit));
  return {
    kind: "project-timeline",
    project: params.project,
    window: params.window,
    query: query.toString(),
  } as const;
}

/** A project's merge timeline, live (plan D8): the body of `GET /api/v1/projects/{key}/timeline`
 *  for the window, its facets and search, holding the PROJECT_TIMELINE_PAGE newest rows when it
 *  opens. Keyed under the project's evals by the view without its limit, so a growth
 *  (`growProjectTimelineView`) keeps the one entry and its rows, and a change of the window, a
 *  facet or the search subscribes its own; the rows on screen stay while its snapshot is out, as
 *  the Delivery page's timeline kept them. */
export const projectTimelineViewQuery = (params: ProjectTimelineViewParams) =>
  queryOptions({
    ...LIVE,
    placeholderData: keepPreviousData,
    queryKey: [...evalsProjectKey(params.project), "timeline", params],
    queryFn: (context) =>
      subscribeLive(context, projectTimelineView(params, PROJECT_TIMELINE_PAGE)),
  });

/** Grows the project-timeline view of `params` to its `limit` newest rows: its one subscription
 *  sent again with that limit and a resume of what it holds, so Dispatch sends only the rows it
 *  lacks (`LiveSocket.grow`). False where the view has not answered yet. */
export function growProjectTimelineView(
  client: QueryClient,
  params: ProjectTimelineViewParams,
  limit: number
): boolean {
  return liveSocketFor(client).grow(
    projectTimelineViewQuery(params).queryKey,
    projectTimelineView(params, limit)
  );
}

/** One eval's detail read at its `version` (`GET /api/v1/projects/{key}/evals/{slug}`, the newest
 *  version alone): its method (`doc_text`), its declared charts and its answer. Keyed under the
 *  project's evals by the version the evals view answers, so a register write that writes a new
 *  version, which the view's delta carries, reads it again (plan D11a). */
export const evalDetailQuery = (project: string, slug: string, version: number) =>
  queryOptions({
    queryKey: [...evalsProjectKey(project), "detail", slug, version],
    queryFn: () => api.getEval(project, slug, { limit: 1 }),
  });

/** One page of the cross-project read: its window and compare range (null for none) as whole UTC
 *  days, the repositories, and the offset of its page. */
export interface EvalsAcrossProjectsParams {
  readonly window: { readonly from: string; readonly to: string };
  readonly compare: { readonly from: string; readonly to: string } | null;
  readonly repos: readonly string[];
  readonly offset: number;
}

/** The prefix of every page of the cross-project read, which a delivery settings save invalidates
 *  beside the project's own evals (`evalsProjectKey`), since the targets it writes are evals that
 *  read lists. No project key is lowercase, so no project's prefix holds it. */
export const evalsAcrossProjectsQueryKey = ["evals", "across"] as const;

/** Every project's evals (dispatch://INFRA-2918/artifact/plan-md, D11; spec R11): `GET
 *  /api/v1/evals` with no project named, so one request reads every project the reader may read,
 *  in the item shape a project's evals view answers, a page of EVALS_VIEW_LIMIT at `offset`. */
export const evalsAcrossProjectsQuery = (params: EvalsAcrossProjectsParams) =>
  queryOptions({
    queryKey: [...evalsAcrossProjectsQueryKey, params],
    queryFn: () =>
      api.listEvals({
        compare_from: params.compare?.from,
        compare_to: params.compare?.to,
        from: params.window.from,
        limit: EVALS_VIEW_LIMIT,
        offset: params.offset > 0 ? params.offset : undefined,
        repo: [...params.repos],
        to: params.window.to,
      }),
  });

/** One project's delivery settings, or `null` for a project that has none: its settings tab's
 *  delivery section reads and edits them (`DeliverySettingsSection`), the Evals tab reads them to
 *  decide its delivery and CI sections, and `/delivery` its default project. */
export const projectDeliverySettingsQuery = (project: string) =>
  queryOptions({
    queryKey: ["project-delivery-settings", project],
    queryFn: () => api.getProjectDeliverySettings(project),
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
