/**
 * The Inbox page's reads of the one read against a Dispatch process of the release before
 * (`inboxOfTheReleaseBefore`). Such a process has no route for `GET /api/v1/me/inbox`, and while
 * one serves, during a roll or after a rollback, each of the page's reads of it - its own rows, its
 * unread count, what the reader dismissed and an owner's unread set, each of which its query names
 * as the read it is (`getMeInboxPage`) - is answered from the reads that release's page made, so
 * the Inbox still shows its sections. The one read is asked once, any other failure of it is the
 * read's own, and a read of the route through `getMeInbox`, whatever its query, is asked as every
 * read is.
 *
 * This file answers that release's reads as its server answers them, so the acceptance driver's
 * search for callers of the reads this release replaced spares it by its path
 * (`e2e/acceptance/one-inbox-callers.sh`, `callers_spared`), and the root's pull request deletes it
 * with the fallback (`root-deletes.txt`, `inbox-page-fallback`).
 */

import { expect, test } from "bun:test";
import {
  meInboxAsks,
  meInboxThreads,
  NO_ROUTE_RETRY_DELAYS_MS,
  NO_ROUTE_ROLLING_RELEASE_MESSAGE,
} from "@legion/contracts";
import { QueryClient } from "@tanstack/react-query";
import { http } from "msw";

import { answerRequests } from "../__tests__/network";
import { issueAsk } from "../features/inbox/inbox-fixture";
import { agentConversation, meInboxListRows } from "../features/inbox/one-read-rows";
import {
  AGENT_CONVERSATION,
  agentRow,
  replyRow,
  withConversation,
} from "../features/threads/replies-fixture";
import { ApiError, createApiClient } from "./client";
import {
  meInboxCountQuery,
  meInboxDismissedQuery,
  meInboxOwnerQuery,
  meInboxQuery,
} from "./queries";
import type { MyReplyRow, OpenAskRow } from "./types";

/** The `as_of` the release before's read of the reader's threads answers. */
const AS_OF = "2026-10-12T02:00:00Z";

/** A request's path and query, as a route answering it is named. */
function pathOf(request: Request): string {
  const url = new URL(request.url);
  return `${url.pathname}${url.search}`;
}

/** A fresh client for the page's reads, which asks nothing again on a failure. */
function inboxClient(): QueryClient {
  return new QueryClient({ defaultOptions: { queries: { retry: false } } });
}

/** `GET /api/v1/me/replies`'s answer listing `rows`, as the release before answers it: `limit` is
 *  the page asked for (0 for an unpaged read) and `total` every row of the state, read or not. */
function repliesAnswer(
  rows: readonly MyReplyRow[],
  page: { readonly limit?: number; readonly next_cursor?: string; readonly total?: number } = {}
) {
  return {
    as_of: AS_OF,
    limit: page.limit ?? 0,
    ...(page.next_cursor === undefined ? {} : { next_cursor: page.next_cursor }),
    offset: 0,
    rows,
    total: page.total ?? rows.length,
  };
}

/**
 * Answers the page as a Dispatch process of the release before does: `GET /api/v1/me/inbox` with
 * what `oneRead` makes of the request, by default no route, in the words a router with none
 * answers (`isNoRoute`); `GET /api/v1/inbox` with `asks`; `GET /api/v1/me/replies` with what
 * `replies` makes of the query; and `GET /api/v1/messages/{id}` with the conversation of
 * `agentRow("message-1")`. It answers the requests it is sent, each by its path and query, in the
 * order they came.
 */
function serveTheReleaseBefore(
  replies: (query: URLSearchParams) => unknown,
  asks: readonly OpenAskRow[] = [],
  oneRead: (request: Request) => Response = (request) =>
    Response.json(
      { code: "NOT_FOUND", error: `no route for GET ${new URL(request.url).pathname}` },
      { status: 404 }
    )
): string[] {
  const asked: string[] = [];
  answerRequests(
    http.get("/api/v1/me/inbox", ({ request }) => {
      asked.push(pathOf(request));
      return oneRead(request);
    }),
    http.get("/api/v1/inbox", ({ request }) => {
      asked.push(pathOf(request));
      return Response.json(asks);
    }),
    http.get("/api/v1/me/replies", ({ request }) => {
      asked.push(pathOf(request));
      return Response.json(replies(new URL(request.url).searchParams));
    }),
    http.get("/api/v1/messages/:id", ({ params, request }) => {
      asked.push(pathOf(request));
      return params.id === AGENT_CONVERSATION.message.id
        ? Response.json(AGENT_CONVERSATION)
        : Response.json({ code: "NOT_FOUND", error: "message not found" }, { status: 404 });
    })
  );
  return asked;
}

test("the reader's own rows are read from the two reads of the release before, the one read asked once", async () => {
  const waiting = issueAsk({ id: "ask-a" });
  const replied = issueAsk({
    id: "ask-b",
    last_reply: { author: { id: "bob", kind: "user" }, created_at: "2026-10-12T01:30:00Z" },
  });
  const reply = replyRow("c1");
  const agent = agentRow("message-1");
  const asked = serveTheReleaseBefore(() => repliesAnswer([reply, agent]), [waiting, replied]);

  const answer = await inboxClient().fetchQuery(meInboxQuery());

  // Each row under the node and the moment the one read gives it: an ask at its newest reply, or
  // where it has none at its own writing, and a thread at its newest reply.
  expect(answer).toEqual({
    as_of: AS_OF,
    entries: [
      {
        ask: waiting,
        moment: "2026-09-11T00:00:00Z",
        node: { id: "ask-a", kind: "ask" },
        relations: [],
      },
      {
        ask: replied,
        moment: "2026-10-12T01:30:00Z",
        node: { id: "ask-b", kind: "ask" },
        relations: [],
      },
      {
        moment: "2026-10-12T00:00:00Z",
        node: { id: "c1", kind: "comment" },
        relations: [],
        thread: reply,
      },
      {
        agent: withConversation(agent, AGENT_CONVERSATION),
        moment: "2026-10-12T00:00:00Z",
        node: { id: "message-1", kind: "message" },
        relations: [],
      },
    ],
    horizon: 0,
    limit: 0,
    offset: 0,
    person: "",
    total: 4,
  });
  // The page draws an ask row of each ask and a thread row of each thread, and the Agent row's
  // reading pane draws the conversation it carries.
  expect(meInboxAsks(answer)).toEqual([waiting, replied]);
  expect(meInboxThreads(answer)).toEqual([reply, withConversation(agent, AGENT_CONVERSATION)]);
  const agentListed = meInboxListRows(answer.entries).at(-1);
  expect(
    agentListed?.kind === "thread" ? agentConversation(agentListed.thread) : undefined
  ).toEqual(AGENT_CONVERSATION);
  expect([...asked].sort()).toEqual([
    "/api/v1/inbox?assignee=me",
    "/api/v1/me/inbox?state=inbox",
    "/api/v1/me/replies?kind=reply%2Cagent&state=inbox",
    "/api/v1/messages/message-1",
  ]);
  expect(asked[0]).toBe("/api/v1/me/inbox?state=inbox");
});

test("the unread count is the total of the release before's one-row page of unread threads", async () => {
  const asked = serveTheReleaseBefore(() =>
    repliesAnswer([replyRow("c1")], { limit: 1, total: 4 })
  );

  expect(await inboxClient().fetchQuery(meInboxCountQuery())).toBe(4);
  expect(asked).toEqual([
    "/api/v1/me/inbox?limit=1&state=unread",
    "/api/v1/me/replies?kind=reply%2Cagent&limit=1&state=unread",
  ]);
});

// The count is read for its `total` alone, so no entry is made of its one-row page, and an Agent
// row there has no conversation read.
test("the unread count reads no Agent row's conversation", async () => {
  const asked = serveTheReleaseBefore(() =>
    repliesAnswer([agentRow("message-1")], { limit: 1, total: 4 })
  );

  expect(await inboxClient().fetchQuery(meInboxCountQuery())).toBe(4);
  expect(asked).toEqual([
    "/api/v1/me/inbox?limit=1&state=unread",
    "/api/v1/me/replies?kind=reply%2Cagent&limit=1&state=unread",
  ]);
});

test("what the reader dismissed is read a page at a time from the release before, its cursor carried on", async () => {
  const newest = replyRow("c1", { dismissed_at: "2026-10-12T01:00:00Z" });
  const older = agentRow("message-1", { dismissed_at: "2026-10-11T01:00:00Z" });
  const cursor = "2026-10-12T01:00:00Z,c1";
  const asked = serveTheReleaseBefore((query) =>
    query.get("cursor") === cursor
      ? repliesAnswer([older], { limit: 200 })
      : repliesAnswer([newest], { limit: 200, next_cursor: cursor })
  );

  const dismissed = await inboxClient().fetchInfiniteQuery({
    ...meInboxDismissedQuery(),
    pages: 2,
  });

  // Each row at its dismissal, the first page naming the cursor the second was read from, and the
  // second, the last, naming none.
  expect(dismissed.pageParams).toEqual([undefined, cursor]);
  expect(dismissed.pages.map((page) => [page.entries, page.limit, page.next_cursor])).toEqual([
    [
      [
        {
          moment: "2026-10-12T01:00:00Z",
          node: { id: "c1", kind: "comment" },
          relations: [],
          thread: newest,
        },
      ],
      200,
      cursor,
    ],
    [
      [
        {
          agent: withConversation(older, AGENT_CONVERSATION),
          moment: "2026-10-11T01:00:00Z",
          node: { id: "message-1", kind: "message" },
          relations: [],
        },
      ],
      200,
      undefined,
    ],
  ]);
  // The release before pages that state with its cursor alone and refuses a limit there.
  const next = new URLSearchParams({ cursor }).toString();
  expect(asked).toEqual([
    "/api/v1/me/inbox?limit=200&state=dismissed",
    "/api/v1/me/replies?kind=reply%2Cagent&state=dismissed",
    `/api/v1/me/inbox?limit=200&state=dismissed&${next}`,
    `/api/v1/me/replies?kind=reply%2Cagent&state=dismissed&${next}`,
    "/api/v1/messages/message-1",
  ]);
});

test("an owner's unread set is the release before's unread threads of that issue or document", async () => {
  const onIssue = replyRow("c1");
  const onDocument = replyRow("c2");
  const asked = serveTheReleaseBefore((query) =>
    repliesAnswer([query.get("issue") === "CORE-1" ? onIssue : onDocument], { limit: 200 })
  );
  const client = inboxClient();

  const issue = await client.fetchQuery(meInboxOwnerQuery({ issue: "CORE-1" }));
  const document = await client.fetchQuery(meInboxOwnerQuery({ artifact: "artifact-1" }));

  expect(meInboxThreads(issue)).toEqual([onIssue]);
  expect(issue).toMatchObject({ limit: 200, total: 1 });
  expect(meInboxThreads(document)).toEqual([onDocument]);
  expect(asked).toEqual([
    "/api/v1/me/inbox?issue=CORE-1&limit=200&state=unread",
    "/api/v1/me/replies?kind=reply%2Cagent&issue=CORE-1&limit=200&state=unread",
    "/api/v1/me/inbox?artifact=artifact-1&limit=200&state=unread",
    "/api/v1/me/replies?kind=reply%2Cagent&artifact=artifact-1&limit=200&state=unread",
  ]);
});

// A read that draws its rows draws whatever row it answers: an Agent row there is the Agent entry
// the one read answers, with the conversation its reading pane draws, never a thread entry.
test("an Agent row of an owner's unread set is an Agent entry with its conversation, never a thread entry", async () => {
  const agent = agentRow("message-1");
  const asked = serveTheReleaseBefore(() => repliesAnswer([agent], { limit: 200 }));

  const answer = await inboxClient().fetchQuery(meInboxOwnerQuery({ issue: "CORE-1" }));

  expect(answer.entries).toEqual([
    {
      agent: withConversation(agent, AGENT_CONVERSATION),
      moment: "2026-10-12T00:00:00Z",
      node: { id: "message-1", kind: "message" },
      relations: [],
    },
  ]);
  expect(asked).toEqual([
    "/api/v1/me/inbox?issue=CORE-1&limit=200&state=unread",
    "/api/v1/me/replies?kind=reply%2Cagent&issue=CORE-1&limit=200&state=unread",
    "/api/v1/messages/message-1",
  ]);
});

test.each([
  [
    "a 404 for something the server does not know, which is no answer of no route",
    404,
    { code: "NOT_FOUND", error: "person alice not found" },
  ],
  [
    "a refusal of a credential without the person scope",
    403,
    { code: "OAUTH_SCOPE_REQUIRED", error: "this route requires the person scope" },
  ],
  ["a server failure", 500, { code: "INTERNAL", error: "the database went away" }],
])("%s is the one read's own failure, and nothing else is read", async (_what, status, refusal) => {
  const asked = serveTheReleaseBefore(
    () => repliesAnswer([]),
    [],
    () => Response.json(refusal, { status })
  );

  const thrown = await inboxClient()
    .fetchQuery(meInboxQuery())
    .catch((failure: unknown) => failure);
  expect(thrown).toBeInstanceOf(ApiError);
  expect(thrown).toMatchObject({ code: refusal.code, message: refusal.error, status });
  expect(asked).toEqual(["/api/v1/me/inbox?state=inbox"]);
});

// A read the page does not make - one with no state, which the one read answers with every node
// tied to the reader - is one no read of the release before answers, so it is asked again over a
// roll's waits, as every other read is, and the reads of that release are never made.
test("a read of the route that is none of the page's is asked again over a roll's waits, and reads nothing of the release before", async () => {
  const asked = serveTheReleaseBefore(() => repliesAnswer([]));
  const client = createApiClient(undefined, undefined, async () => {});

  const thrown = await client.getMeInbox({ limit: 50 }).catch((failure: unknown) => failure);

  expect(thrown).toBeInstanceOf(ApiError);
  expect(thrown).toMatchObject({ message: NO_ROUTE_ROLLING_RELEASE_MESSAGE, status: 404 });
  expect(asked).toEqual(
    Array.from({ length: NO_ROUTE_RETRY_DELAYS_MS.length + 1 }, () => "/api/v1/me/inbox?limit=50")
  );
});

// Only the page's own reads, each of which names itself as the read it is (`getMeInboxPage`), fall
// back: a read through `getMeInbox` is asked as every read is whatever its query, one in the shape
// of the page's own rows included.
test("a read through getMeInbox in the shape of the page's own rows is asked again over a roll's waits, and reads nothing of the release before", async () => {
  const asked = serveTheReleaseBefore(() => repliesAnswer([]));
  const client = createApiClient(undefined, undefined, async () => {});

  const thrown = await client.getMeInbox({ state: "inbox" }).catch((failure: unknown) => failure);

  expect(thrown).toBeInstanceOf(ApiError);
  expect(thrown).toMatchObject({ message: NO_ROUTE_ROLLING_RELEASE_MESSAGE, status: 404 });
  const sent = "/api/v1/me/inbox?state=inbox";
  expect(asked).toEqual(Array.from({ length: NO_ROUTE_RETRY_DELAYS_MS.length + 1 }, () => sent));
});
