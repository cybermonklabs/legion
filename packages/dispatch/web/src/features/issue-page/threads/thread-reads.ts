/**
 * The reads of Threads (dispatch://INFRA-5704/spec#b-r-all to #b-r-row): the three pages of All and
 * Mine, Agents' page, and the events read the marks count from (R3 to R7).
 *
 * Each read stands under a key whose prefix a rule of `api/event-refresh.ts` already refreshes,
 * so a comment, message, ask or issue event of the issue reads it again and neither that file nor
 * `api/queries.ts` changes (R14, R18, plan choice 3): a comment event refreshes
 * `["comments", key]`, a message event `["issue-messages", key]`, an ask event `["asks", key]`, and
 * every issue event `["events", key]`. No key here meets the Conversation tab's
 * `["asks", key, "thread"]` or a document's `["comments", key, { artifact }]`.
 *
 * The other two reads R6 and R7 name are the page's own and are read where they are defined: the
 * reader's state is `userStateQuery()` (`api/queries.ts`), and the reader's unread threads are
 * `dashboardRepliesQuery` (`features/dashboard/useWaitingItems.ts`), which the header's count
 * sends, so the page sends each once.
 */
import { NO_ROUTE_ROLLING_RELEASE_MESSAGE } from "@legion/contracts";
import { infiniteQueryOptions, replaceEqualDeep } from "@tanstack/react-query";

import { ApiError, api } from "../../../api/client";
import { type EventPages, mergeEventPages } from "../../../api/event-feed";
import { queryRetry } from "../../../api/query-client";
import { EVENT_FRESH } from "../../refs/reference-target";

/** How many threads a page of a route holds, and how many rows a page of a choice draws (R2,
 *  R3's `limit=20`). */
export const THREADS_PAGE = 20;

/** How many events the first page of the events read holds, as the server answers it at most
 *  (R7's newest 200). */
export const EVENTS_PAGE = 200;

/** Whether `error` is what a page read rejects with where the build that answered serves no page:
 *  one from before the pages answers the route's bare list while a release rolls out
 *  (`threadPageNotOffered` in `api/client.ts`), and a read again would reach the same build as
 *  likely as not. Threads draws its older-build sentence for it (plan choice 7). */
export function isOlderBuild(error: unknown): boolean {
  return (
    error instanceof ApiError &&
    error.status === 404 &&
    error.code === "NOT_FOUND" &&
    error.message === NO_ROUTE_ROLLING_RELEASE_MESSAGE
  );
}

/** A page read is retried as every read of the page is (`queryRetry`), but an older build's bare
 *  list, which `queryRetry` would read twice more as a `404` (plan choice 7). */
function pageRetry(failureCount: number, error: unknown): boolean {
  return !isOlderBuild(error) && queryRetry(failureCount, error);
}

/** The comment pages of All, or of Mine with `author` the reader's login (R3, R4): each page's
 *  parameter is the `before` of the page above it. */
export const commentThreadsQuery = (key: string, author?: string) =>
  infiniteQueryOptions({
    ...EVENT_FRESH,
    initialPageParam: null as string | null,
    queryKey: ["comments", key, "threads", { author, person: "in" }],
    queryFn: ({ pageParam }) =>
      api.listCommentThreads(key, {
        author,
        before: pageParam ?? undefined,
        limit: THREADS_PAGE,
        person: "in",
      }),
    getNextPageParam: (page) => page.before ?? undefined,
    retry: pageRetry,
  });

/** The message pages of All and Mine (`person=in`), or Agents' one route (`person=none`, R5),
 *  with `author` the reader's login for Mine (R3, R4). */
export const messageThreadsQuery = (key: string, person: "in" | "none", author?: string) =>
  infiniteQueryOptions({
    ...EVENT_FRESH,
    initialPageParam: null as string | null,
    queryKey: ["issue-messages", key, "threads", { author, person }],
    queryFn: ({ pageParam }) =>
      api.listMessageThreads(key, {
        author,
        before: pageParam ?? undefined,
        limit: THREADS_PAGE,
        person,
      }),
    getNextPageParam: (page) => page.before ?? undefined,
    retry: pageRetry,
  });

/** The ask pages of All, or of Mine with `author` the reader's login, each row with its thread
 *  (`thread=1`, R3, R4). No ask read carries `person` (R5). */
export const askThreadsQuery = (key: string, author?: string) =>
  infiniteQueryOptions({
    ...EVENT_FRESH,
    initialPageParam: null as string | null,
    queryKey: ["asks", key, "threads", { author }],
    queryFn: ({ pageParam }) =>
      api.listAskThreads(key, {
        author,
        before: pageParam ?? undefined,
        limit: THREADS_PAGE,
        thread: "1",
      }),
    getNextPageParam: (page) => page.before ?? undefined,
    retry: pageRetry,
  });

/**
 * The events read the marks count from (R7): the `["events", key]` infinite query in the shape
 * that today's Activity tab (`features/activity/ActivityTab.tsx`) and the new page's history
 * (`IssueHistory.tsx` on dispatch://INFRA-3948) send, a first page of the newest 200, so the page
 * holds one copy and sends one read (plan choice 6). The feed (`api/event-feed.ts`,
 * `prependEventToLog`) prepends each event as it is published, and a refetch whose read predates
 * one of them must not take it back, so the merge is the same `mergeEventPages`.
 */
export const threadEventsQuery = (key: string) =>
  infiniteQueryOptions({
    initialPageParam: null as number | null,
    queryKey: ["events", key],
    queryFn: ({ pageParam }) =>
      api.getIssueEvents(
        key,
        pageParam === null
          ? { limit: EVENTS_PAGE, order: "desc" }
          : { before: pageParam, limit: EVENTS_PAGE }
      ),
    // A page can hold more than the server's 200 once streamed events sit above a full response
    // (mergeEventPages, prependEventToLog). It is still a full page with older events beneath it.
    getNextPageParam: (page) => (page.length >= EVENTS_PAGE ? page.at(-1)?.seq : undefined),
    structuralSharing: (current, incoming) =>
      replaceEqualDeep(
        current,
        mergeEventPages(current as EventPages | undefined, incoming as EventPages)
      ),
  });
