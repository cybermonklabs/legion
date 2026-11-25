import { INBOX_DISMISSED_PAGE, meInboxAsks } from "@legion/contracts";

import {
  infiniteQueryOptions,
  keepPreviousData,
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
  DeliveryMeasuresResponse,
  DeliveryPipelineResponse,
  DeliveryTimelineWithMeasures,
  IssueDetails,
  MeInboxResponse,
  OpenAskRow,
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

/** The prefix of every read of the open asks (`openAsksQuery`), each project's its own entry
 *  under it beside the read of every project: one invalidation of it refreshes them all, and every
 *  optimistic write over the open asks patches each read under it as a list of rows. */
export const openAsksKey = ["open-asks"] as const;

/** The open asks the viewer can see (`GET /api/v1/asks/open`), as their rows: every one of every
 *  project by default, which the Inbox reads for every person's asks but the reader's own, and the
 *  margin, the Agents page and the Machines page read too; one project's (`project`) for its page.
 *  An Inbox card mounts its thread from these rows, so the read's requests count in the
 *  generations an ask event in flight marks (`ask-thread-freshness.ts`). */
export const openAsksQuery = (project?: string) =>
  queryOptions({
    queryKey: [...openAsksKey, { project }],
    queryFn: async ({ client }) => {
      const fetch = beginInboxFetch(client, "open-asks");
      let asks: OpenAskRow[] | undefined;
      try {
        asks = (await api.listOpenAsks(project === undefined ? {} : { project })).asks ?? [];
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

/** The prefix of every read of the one read, `GET /api/v1/me/inbox`: the Inbox's own rows, its
 *  unread count, what the reader has dismissed and each owner's unread set. One invalidation of it
 *  refreshes them all; what refreshes the open asks or an Agent row's conversation reaches it
 *  through `event-refresh.ts`, since it holds the reader's half of those as well. */
export const meInboxKey = ["me", "inbox"] as const;

/** One Inbox thread's ask, with its replies and its answer: the row's body in the list and in
 *  the reading pane read the one copy under this key. It never goes stale on its own - an event
 *  refresh is what moves it (`api/event-refresh.ts`). */
export const askThreadQuery = (threadId: string) =>
  queryOptions({
    queryKey: ["ask-thread", threadId],
    queryFn: () => api.getAsk(threadId),
    staleTime: Number.POSITIVE_INFINITY,
  });

/** Every row of the reader's own Inbox from the one read, in one unpaged read of its `inbox`
 *  state: the open asks that wait on them, and their Reply and Agent rows, read or not, less what
 *  they have dismissed. The page, its drawer, the sidebar and the Needs-you count read this one
 *  key. The reader's own ask cards mount their threads from its asks, so its requests count in the
 *  generations an ask event in flight marks (`ask-thread-freshness.ts`), as the open asks' do. */
export const meInboxQuery = () =>
  queryOptions({
    queryKey: [...meInboxKey, "inbox"],
    queryFn: async ({ client }) => {
      const fetch = beginInboxFetch(client, "one-read");
      let answer: MeInboxResponse | undefined;
      try {
        answer = await api.getMeInbox({ state: "inbox" });
        return answer;
      } finally {
        finishInboxFetch(client, fetch, answer === undefined ? undefined : meInboxAsks(answer));
      }
    },
  });

/** The reader's count of unread threads, the `total` of the one read's `unread` state on a
 *  one-entry page: the sidebar's and the compact header's one Inbox number counts it beside the
 *  work waiting on them. A small read of its own rather than a count over the unpaged read above,
 *  which every page makes as well (`useNeedsYouCount`): the number does not wait on that read to
 *  show the unread threads. */
export const meInboxCountQuery = () =>
  queryOptions({
    queryKey: [...meInboxKey, "count"],
    queryFn: async () => (await api.getMeInbox({ limit: 1, state: "unread" })).total,
  });

/** What the reader has dismissed out of their Inbox, from the one read's `dismissed` state, newest
 *  dismissal first, which the View menu's **Dismissed** draws: `INBOX_DISMISSED_PAGE` entries a
 *  page, each next page read from the last one's `next_cursor` by the list's own **Show more**. */
export const meInboxDismissedQuery = () =>
  infiniteQueryOptions({
    queryKey: [...meInboxKey, "dismissed"],
    queryFn: ({ pageParam }) =>
      api.getMeInbox({
        limit: INBOX_DISMISSED_PAGE,
        state: "dismissed",
        ...(pageParam === undefined ? {} : { cursor: pageParam }),
      }),
    initialPageParam: undefined as string | undefined,
    getNextPageParam: (last: MeInboxResponse) => last.next_cursor,
  });

/** One owner's unread threads, from the one read's `unread` state narrowed to that issue or
 *  document, which every thread mounted on that owner's page reads to learn whether it is unread:
 *  one request however many threads the page shows. */
export const meInboxOwnerQuery = (
  owner: { readonly issue: string } | { readonly artifact: string }
) =>
  queryOptions({
    queryKey: [...meInboxKey, owner],
    queryFn: () => api.getMeInbox({ ...owner, limit: 200, state: "unread" }),
  });

/** One session's conversation for the reader (`GET /api/v1/agents/{session_id}/conversation`):
 *  the direct messages and issue messages that reach it, with their replies. The Agents page row,
 *  the live view and an Agent row's reading pane read this one key. */
export const agentConversationQuery = (sessionId: string) =>
  queryOptions({
    queryKey: ["agents", sessionId, "conversation"],
    queryFn: () => api.getAgentConversation(sessionId),
  });

/** One broadcast (`GET /api/v1/broadcasts/{id}`): what was sent, and where every recipient's copy
 *  of it got to. Keyed under ["broadcast"], the prefix every issue-less message event refreshes
 *  (`event-refresh`), so a recipient's delivery receipt or reply lands without a poll. The
 *  broadcast's page, the Agents page's sent broadcast and the compose dialog read this one key. */
export const broadcastQuery = (id: string) =>
  queryOptions({
    queryKey: ["broadcast", id],
    queryFn: () => api.getBroadcast(id),
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
 * A project's issue list - plain, or server-filtered by labels - shared by the Board, the List
 * and the issue header's label picker. Every observer sets the query's function, so each one
 * takes it from here. A fetch that starts while a board move on the project is unanswered waits
 * for the answer: until then the server may not have applied the move, and its list would put
 * the card back where it came from.
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

/** What every live-owned delivery query shares (LEGION-690): its first load subscribes its view
 *  over the page's one live socket (`api/live-socket.ts`), which writes the snapshot and every
 *  later change into the cache, so nothing here reads again. `meta.live` keeps the event feed's
 *  refresh paths away from it (`api/query-refresh.ts`); `gcTime` 0 drops the subscription with the
 *  last observer; the page keeps the previous answer on screen while a new view's snapshot is out,
 *  as a filter change in the prototype it ports never blanks it. */
const LIVE = {
  gcTime: 0,
  meta: { live: true },
  refetchOnReconnect: false,
  refetchOnWindowFocus: false,
  staleTime: Number.POSITIVE_INFINITY,
} as const;

/** The body each delivery view's kind is answered with: the GET route's it mirrors. */
interface DeliveryLiveBodies {
  readonly timeline: DeliveryTimelineWithMeasures;
  readonly measures: DeliveryMeasuresResponse;
  readonly pipeline: DeliveryPipelineResponse;
}

/** The live view's answer, which is the body of the GET route its kind mirrors. Only the views
 *  this page subscribes — timeline, measures and pipeline — are keyed here; the socket serves
 *  other kinds this page does not read. */
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
