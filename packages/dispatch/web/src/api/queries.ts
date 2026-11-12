import { INBOX_THREAD_KINDS } from "@legion/contracts/dispatch-api-constants";

import {
  infiniteQueryOptions,
  keepPreviousData,
  type QueryClient,
  type QueryFunctionContext,
  queryOptions,
  replaceEqualDeep,
} from "@tanstack/react-query";

import { credentialPendingQuery } from "../features/credentials/pending";
import { issueDecisionsQuery } from "../features/doc/spec-marks";
import { beginInboxFetch, finishInboxFetch } from "../features/inbox/ask-thread-freshness";
import { projectIssuesQueryKey } from "../features/project/issue-filters";
import { agentsQuery } from "../features/refs/agent-registry";
import { parseIssuePath, parseProjectPath } from "../features/refs/routes";
import { compareTimestamps, isTimestamp } from "../lib/timestamps";

import { api, isEventHeadReadFailed, isEventHeadUnavailable, isUnauthorized } from "./client";
import { liveSocketFor } from "./live-socket";
import type { Query } from "./paths";
import type {
  Artifact,
  ArtifactBlock,
  ArtifactText,
  AuthenticatedUser,
  DeliveryLiveView,
  DeliveryMeasuresResponse,
  DeliveryPipelineResponse,
  DeliveryTimelineWithMeasures,
  InboxRow,
  IssueDetails,
  LandingBoardPage,
  LandingDocumentPage,
  LandingIssuePage,
  LandingResponse,
  LandingShell,
  MyRepliesResponse,
} from "./types";

/** The signed-in principal, which every surface reads under this one key; it names the person by
 *  lowercase email, the form issue assignees carry. While it answers `EVENT_HEAD_UNAVAILABLE` it
 *  is asked again for as long as that lasts (`queryRetry`), so the shell shows its skeleton,
 *  never the error page.
 *
 *  The COLD read is the landing (at the end of this file): one request that answers the viewer,
 *  the shell and the page the browser is loading, and seeds each part under the key its own
 *  route already uses. `AuthGate` holds the tree behind this one pending query, so that read is
 *  the page's first wave and whatever the answer does not carry is its second. A Dispatch older
 *  than that route answers 404, and the read falls back to `/auth/whoami` and seeds nothing,
 *  which leaves every page reading as it read before.
 *
 *  A WARM read of this key is `/auth/whoami` and nothing more. This key is not only the cold
 *  read: `AuthGate` keeps an observer on it for the whole session, under `refetchOnWindowFocus`
 *  and a 30,000 ms `staleTime` (`main.tsx`), and it is the "am I still signed in?" probe that
 *  `api/live-socket.ts` invalidates on a refused or closed socket, that
 *  `features/writes/write-store.ts` invalidates twice - one of them awaited inline in a write's
 *  401 path - and that `app.tsx` invalidates on a cross-tab sign-in change. Each of those used
 *  to cost one `GET /auth/whoami`; while this key's `queryFn` was the landing, each cost a whole
 *  landing instead - about twenty server-side reads and a re-seed of some twenty cache keys - to
 *  answer a question the viewer's own route answers in one. The landing is the read a cold page
 *  makes, which is what the route's own head calls it, so it is made where the client holds no
 *  viewer yet and nowhere else.
 *
 *  What makes a read cold is this key holding no viewer, which is exactly the state a page load
 *  starts in and the state a sign-out leaves behind: the sign-out's `resetQueries` on this key
 *  drops its data and refetches at once (`app.tsx`), so the next person to sign in here lands on
 *  a cold read and gets their own page seeded. A refetch of a key that still holds a viewer
 *  keeps that data while it runs (TanStack Query leaves `data` in place during a refetch), so
 *  the two cases cannot be confused. */
export const whoAmIQuery = () =>
  queryOptions({
    queryKey: ["whoami"],
    queryFn: ({ client, queryKey }) =>
      client.getQueryData<AuthenticatedUser>(queryKey) === undefined
        ? readLanding(client)
        : api.whoAmI(),
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

/** The live blocks of one document (`GET /api/v1/artifacts/{id}/blocks`): the document's own
 *  reference panel and the Plan tab's task list read the same key, so a tab opened after the
 *  document finds it answered. */
export const artifactBlocksQuery = (id: string) =>
  queryOptions({
    queryKey: ["artifact", id, "blocks"],
    queryFn: () => api.getArtifactBlocks(id),
  });

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

/** Every project that names a default assignee, read by the Settings page's Projects table and
 *  refreshed by the event a write appends. A project absent from the answer names nobody. */
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

/** The live view's answer, which is the body of the GET route its kind mirrors. */
function subscribeLive<V extends DeliveryLiveView>(
  context: QueryFunctionContext,
  view: V
): Promise<DeliveryLiveBodies[V["kind"]]> {
  // The socket answers each view with its own route's body (packages/contracts DeliveryLiveView);
  // the frame carries the union, and the view's kind names the member.
  return liveSocketFor(context.client).subscribe(context.queryKey, view) as Promise<
    DeliveryLiveBodies[V["kind"]]
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

// ---------------------------------------------------------------------------------------------
// The landing: the one read a cold page makes, and the seed of every key above that it answers.
//
// Why it is here and not beside each query: the seed writes into the keys this file declares, so
// a reader who changes a key below sees, in the same file, what else writes it. It is a cache
// seed and never a second data path - each part is written under the key the route it stands in
// for already uses, and the client's default `staleTime` is 30,000 ms (`main.tsx`), so a
// component that mounts with its key seeded reads the cache and sends no request of its own. A
// part the answer leaves null is left unseeded, and its own query runs as it ran before.
// ---------------------------------------------------------------------------------------------

/** What the landing route is asked for: the page, and the names that page is about. */
export type LandingRead = Query<"/api/v1/landing", "get">;

/**
 * Which page the entry asks the landing for, read from the address the browser is loading.
 *
 * The shapes are the router's own, parsed by the functions that already parse them
 * (`features/refs/routes.ts`), so this cannot drift from what `app.tsx` draws at that address:
 * `/issues/<key>` and every tab, document and item under it are one issue page; a project's
 * `/documents/<slug>` is a document page; every other `/projects/<key>` address - the board and
 * its Issues, Documents and Architecture tabs - is a board.
 *
 * Any other address - the Inbox at `/`, and the Agents, Settings, Delivery and machine pages
 * too - asks for the Inbox. The route needs a page, and the Inbox is the only arm that needs no
 * name from the address, so it is the only one such an address can ask for at all. It costs
 * those pages nothing they cannot use: the threads it carries are the rows the Inbox drawer
 * draws, and that drawer is mounted on every page of the shell (`app.tsx`,
 * `features/inbox/InboxDrawer.tsx`), so a reader who opens it there has them already.
 */
export function landingReadOf(pathname: string): LandingRead {
  const issue = parseIssuePath(pathname);
  if (issue !== undefined) {
    return { page: "issue", key: issue.key };
  }
  const project = parseProjectPath(pathname);
  if (project !== undefined) {
    return project.kind === "document"
      ? { page: "document", project: project.project, slug: project.slug }
      : { page: "board", project: project.project };
  }
  return { page: "inbox" };
}

/**
 * The refusals that are the answer itself, which no fallback may swallow: the 401 `AuthGate`
 * draws its sign-in page from, and the two 503s about the event log's head, which the query
 * client asks again by their codes (`api/query-client.ts`) - the first ends on its own, the
 * second ends at the page's error and its Retry. A page drawn without a head has no position
 * for its event feed to resume from, so neither may be answered with a viewer read some other
 * way.
 *
 * Every other failure - the 404 of a Dispatch older than this route, which a rolling deploy puts
 * in front of a browser, a dropped connection, a 5xx - says only that this one read did not
 * happen, and the page falls back to reading as it read before.
 */
function refusalIsTheAnswer(error: unknown): boolean {
  return isUnauthorized(error) || isEventHeadUnavailable(error) || isEventHeadReadFailed(error);
}

/**
 * Reads the landing for the address the browser is loading, seeds every part of the answer, and
 * answers the viewer, which is what `["whoami"]` has always held.
 *
 * `pathname` is the browser's own, read before React renders anything: `AuthGate` holds the
 * whole tree - the router included - behind this one read, and the router reads the same address
 * (`BrowserRouter` in `main.tsx`).
 */
export async function readLanding(
  client: QueryClient,
  pathname: string = window.location.pathname
): Promise<AuthenticatedUser> {
  const read = landingReadOf(pathname);
  let answer: LandingResponse;
  try {
    answer = await api.getLanding(read);
  } catch (error) {
    if (refusalIsTheAnswer(error)) throw error;
    return api.whoAmI();
  }
  // The viewer, in the shape `GET /auth/whoami` answers: this route resolves its caller as that
  // one does, and answers a person or refuses, so the kind is the kind whoami names.
  //
  // The event feed resumes from this `event_head` (`api/event-feed.ts`), and that is sound only
  // while every read of the page holds the events at or below it. One answer carries the head
  // and the page's data together, so the data cannot be newer than the head it is seeded beside.
  const viewer: AuthenticatedUser = {
    event_head: answer.viewer.event_head,
    kind: "user",
    login: answer.viewer.login,
  };
  seedShell(client, answer.shell);
  const { board, document, inbox, issue } = answer.page;
  if (issue !== null && read.key !== undefined) seedIssuePage(client, read.key, issue);
  if (board !== null && read.project !== undefined) seedBoardPage(client, read.project, board);
  if (inbox !== null && inbox.threads !== null) {
    client.setQueryData(inboxThreadsQuery().queryKey, inbox.threads);
  }
  if (document !== null && read.project !== undefined && read.slug !== undefined) {
    seedDocumentPage(client, `${read.project}/${read.slug}`, document);
  }
  return viewer;
}

/** What every page draws, whatever its address. */
function seedShell(client: QueryClient, shell: LandingShell): void {
  // The asks the navigation counts and the Inbox lists (`inboxQuery`).
  if (shell.inbox !== null) client.setQueryData(inboxQuery().queryKey, shell.inbox);
  // The reader's pinned issues, under the key the sidebar reads them by
  // (`features/sidebar/Sidebar.tsx`).
  if (shell.pinned_issues !== null) client.setQueryData(["issues", "pinned"], shell.pinned_issues);
  if (shell.projects !== null) client.setQueryData(projectsQuery().queryKey, shell.projects);
  if (shell.agent_states !== null) {
    client.setQueryData(userAgentStateQuery().queryKey, shell.agent_states);
  }
  // The badge's count query keeps the `total` of its one-row page and throws the row away
  // (`myRepliesCountQuery`), so the number is what that key holds, and the number is what the
  // landing carries.
  if (shell.unread_replies !== null) {
    client.setQueryData(myRepliesCountQuery().queryKey, shell.unread_replies);
  }
  // A deployment with no secrets broker answers null here, as that route itself answers null
  // (`features/credentials/pending.ts`). It is left unseeded all the same: a part is null when
  // Dispatch could not read it as well as when there is nothing to read, and nothing here tells
  // the two apart, so the list is read from its own route - which does tell them apart - rather
  // than held at null on this answer's word.
  if (shell.credential_requests !== null) {
    client.setQueryData(credentialPendingQuery().queryKey, shell.credential_requests);
  }
  // The agent registry every surface shares (`features/refs/agent-registry.ts`); null where this
  // Dispatch has no Envoy listener to read it from.
  if (shell.agents !== null) client.setQueryData(agentsQuery().queryKey, shell.agents);
}

/** One issue's page, under the keys `features/issue` and the margin read it by. */
function seedIssuePage(client: QueryClient, key: string, page: LandingIssuePage): void {
  if (page.issue !== null) client.setQueryData(issueQuery(key).queryKey, page.issue);
  if (page.user_state !== null) client.setQueryData(userStateQuery().queryKey, page.user_state);
  // The issue's whole comment list, which is what the margin and the Conversation read
  // (`features/margin/useMarginItems.ts`, `features/conversation/ConversationTab.tsx`). The
  // document's `["comments", key, artifact]` is a narrower list - the comments anchored in that
  // one document - which this answer does not carry, so that key keeps its own read.
  if (page.comments !== null) client.setQueryData(["comments", key], page.comments);
  // The issue's asks in every state, as the document's own reader asks for them
  // (`features/doc/ProofDocument.tsx`).
  if (page.asks !== null) client.setQueryData(["asks", key], page.asks);
  // Who follows this issue (`features/issue/IssueSubscribers.tsx`).
  if (page.subscribers !== null) client.setQueryData(["subscribers", key], page.subscribers);
  if (page.decisions !== null) {
    client.setQueryData(issueDecisionsQuery(key, true).queryKey, page.decisions);
  }
  // What each requirement of the spec lacks, which the Spec tab draws beside the requirement
  // (`features/issue/SpecRequirements.tsx`).
  if (page.coverage !== null) {
    client.setQueryData(issueCoverageQuery(key).queryKey, page.coverage);
  }
  // Every repository mapped to a project, out of which the header's repository control picks
  // this project's (`features/issue/RepoControl.tsx`, which writes the key by hand).
  if (page.repo_projects !== null) {
    client.setQueryData(["repo-projects"], page.repo_projects);
  }
  // The sources of the issue's own project, which the components line reads
  // (`features/issue/IssueComponentsLine.tsx`). The project is the one the issue carries, and
  // the landing reads the sources of that project and of no other, so an answer that carries no
  // issue carries no sources to put under a project's key either.
  if (page.issue !== null && page.architecture_sources !== null) {
    client.setQueryData(
      projectArchitectureSourcesQuery(page.issue.project).queryKey,
      page.architecture_sources
    );
  }
  const document = page.document;
  if (document === null || document.artifact === null) return;
  const id = document.artifact.id;
  seedDocument(client, document.artifact, document.text, document.blocks);
  // Which version of this document the reader last read, which is what marks the blocks that
  // changed since (`features/doc/spec-marks.ts`).
  if (document.read !== null) client.setQueryData(["document-read", id], document.read);
}

/** A project's board and the tabs beside it. */
function seedBoardPage(client: QueryClient, project: string, page: LandingBoardPage): void {
  // The project's open issues, the read the board makes for itself (`projectIssuesQuery`, whose
  // key `features/project/issue-filters.ts` builds).
  if (page.issues !== null) {
    client.setQueryData(projectIssuesQuery(project, [], true).queryKey, page.issues);
  }
  // The asks of this project alone (`features/project/ProjectPage.tsx`).
  if (page.inbox !== null) client.setQueryData(["inbox", project], page.inbox);
  if (page.user_state !== null) client.setQueryData(userStateQuery().queryKey, page.user_state);
  // The project's architecture sources, which decide which tab the project page opens on
  // (`features/project/ProjectPage.tsx`).
  if (page.architecture_sources !== null) {
    client.setQueryData(
      projectArchitectureSourcesQuery(project).queryKey,
      page.architecture_sources
    );
  }
}

/** A project's document page, named by the `project/slug` its own read is keyed by. */
function seedDocumentPage(client: QueryClient, reference: string, page: LandingDocumentPage): void {
  const artifact = page.artifact;
  if (artifact === null) return;
  // The read the page gates on: it holds everything else until this one names the document's id
  // (`features/document/useProjectArtifact.ts`).
  client.setQueryData(["artifact-ref", reference], artifact);
  seedDocument(client, artifact, page.text, page.blocks);
  // The document's own asks (`features/doc/ProofDocument.tsx`), its subscribers
  // (`features/document/DocumentPage.tsx`) and its comments
  // (`features/margin/useMarginItems.ts`).
  if (page.asks !== null) client.setQueryData(["artifact", artifact.id, "asks"], page.asks);
  if (page.subscribers !== null) {
    client.setQueryData(["subscribers", artifact.id], page.subscribers);
  }
  if (page.comments !== null) {
    client.setQueryData(["artifact", artifact.id, "comments"], page.comments);
  }
}

/** One document under the three keys its readers share: the artifact itself (`ProofDocument` and
 *  `features/inbox/PaneDocument.tsx`), its live text (`ProofDocument`'s `liveTextQueryOptions`)
 *  and its blocks (`artifactBlocksQuery`). The live editor's admission read of that text is
 *  deliberately not started here: it carries `staleTime: 0`, so it reads again whatever this key
 *  holds and whatever a read started here had already answered, which measured as two requests
 *  for one document rather than one (`features/doc/ProofDocument.tsx`, `admit`). */
function seedDocument(
  client: QueryClient,
  artifact: Artifact,
  text: ArtifactText | null,
  blocks: ArtifactBlock[] | null
): void {
  const id = artifact.id;
  client.setQueryData(["artifact", id], artifact);
  if (text !== null) client.setQueryData(["artifact", id, "text"], text);
  if (blocks !== null) client.setQueryData(artifactBlocksQuery(id).queryKey, blocks);
}
