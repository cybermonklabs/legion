import { afterEach, describe, expect, jest, spyOn, test } from "bun:test";
import { focusManager, QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, type RenderHookResult, renderHook } from "@testing-library/react";
import { createElement, type ReactNode } from "react";

import { jump, realClock, tick } from "../../../__tests__/fake-clock";
import { ApiError, api, NetworkError } from "../../../api/client";
import { routeUrl } from "../../../api/paths";
import { queryRetry, queryRetryDelay } from "../../../api/query-client";
import type { AskRead, CommentRead, MessageRead } from "../../../api/types";
import { askQuery, useReferenceQuery } from "../../refs/reference-target";
import askBody from "./fixtures/ask.json";
import ask404Body from "./fixtures/ask-404.json";
import comment404Body from "./fixtures/comment-404.json";
import commentHeadBody from "./fixtures/comment-head.json";
import commentQuoteReplyBody from "./fixtures/comment-quote-reply.json";
import commentReplyToBody from "./fixtures/comment-reply-to.json";
import message404Body from "./fixtures/message-404.json";
import messageRootBody from "./fixtures/message-root.json";
import { type ThreadLookup, useThreadLookup } from "./thread-lookup";

// The answers the server gave at the stack base (fixtures/README.md), and the ids it read them
// by.
const KEY = "PDFD720CB-1";
const SECOND_KEY = "PDFD720CB-2";
const ASK = askBody as unknown as AskRead;
const HEAD = commentHeadBody as unknown as CommentRead;
const QUOTE_REPLY = commentQuoteReplyBody as unknown as CommentRead;
const REPLY_TO = commentReplyToBody as unknown as CommentRead;
const ROOT = messageRootBody as unknown as MessageRead;
const ASK_ID = ASK.ask.id;
const HEAD_ID = HEAD.comment.id;
const QUOTE_REPLY_ID = QUOTE_REPLY.comment.id;
const REPLY_TO_ID = QUOTE_REPLY.comment.reply_to ?? "";
const ROOT_ID = ROOT.message.id;

const ASK_404 = () => new ApiError(404, ask404Body);
const COMMENT_404 = () => new ApiError(404, comment404Body);
const MESSAGE_404 = () => new ApiError(404, message404Body);

type Route = "ask" | "comment" | "message";

/** The three routes' reads, each answered when the row says, and the ids each was asked. */
interface Routes {
  readonly asked: Readonly<Record<Route, string[]>>;
  /** Has `route` answer with `body`, or refuse with `error`, then lets the page take it. */
  readonly answer: (route: Route, outcome: { body?: unknown; error?: unknown }) => Promise<void>;
  /** Holds `route`'s next read until the row answers it again. */
  readonly again: (route: Route) => void;
  readonly restore: () => void;
}

/** One route's read, held until the row answers it. */
interface Held {
  readonly promise: Promise<unknown>;
  readonly resolve: (body: unknown) => void;
  readonly reject: (error: unknown) => void;
}

function routes(): Routes {
  const held: Record<Route, Held> = {
    ask: Promise.withResolvers(),
    comment: Promise.withResolvers(),
    message: Promise.withResolvers(),
  };
  const asked: Record<Route, string[]> = { ask: [], comment: [], message: [] };
  const getAsk = spyOn(api, "getAsk").mockImplementation(((id: string) => {
    asked.ask.push(id);
    return held.ask.promise;
  }) as unknown as typeof api.getAsk);
  const getComment = spyOn(api, "getComment").mockImplementation(((id: string) => {
    asked.comment.push(id);
    return id === REPLY_TO_ID ? Promise.resolve(REPLY_TO) : held.comment.promise;
  }) as unknown as typeof api.getComment);
  const getMessageThread = spyOn(api, "getMessageThread").mockImplementation(((id: string) => {
    asked.message.push(id);
    return held.message.promise;
  }) as unknown as typeof api.getMessageThread);
  return {
    again: (route) => {
      held[route] = Promise.withResolvers();
    },
    answer: async (route, outcome) => {
      if (outcome.error === undefined) held[route].resolve(outcome.body);
      else held[route].reject(outcome.error);
      await jump(0);
    },
    asked,
    restore: () => {
      getAsk.mockRestore();
      getComment.mockRestore();
      getMessageThread.mockRestore();
    },
  };
}

/** Whether a page the view holds has the id, as the view tells the lookup: undefined while a
 *  page that may hold it is out. */
interface Pages {
  readonly held: boolean | undefined;
}

/** The lookup of `id` on `key`, over a query client with the app's own retry rules. A case
 *  draws it again with what the pages hold then (`rerender`). `beside` is drawn in the same
 *  render, before the lookup: another reader on the page, such as a reference to the record. */
function lookUp(
  key: string,
  id: string,
  pages: Pages = { held: false },
  beside: () => unknown = () => undefined
): RenderHookResult<ThreadLookup, Pages> {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: queryRetry, retryDelay: queryRetryDelay } },
  });
  drawnClient = client;
  return renderHook<ThreadLookup, Pages>(
    (now) => {
      beside();
      return useThreadLookup(key, id, now.held);
    },
    {
      initialProps: pages,
      wrapper: ({ children }: { children: ReactNode }) =>
        createElement(QueryClientProvider, { client }, children),
    }
  );
}

function recordOf(lookup: ThreadLookup): string | undefined {
  return lookup.kind === "found" ? lookup.record.id : undefined;
}

/** Has `route` fail with a 5xx past the app's two retries, its 404's body under the status 503,
 *  while the other two routes answer 404, so the lookup draws the failed read. */
async function failOn(lookups: Routes, route: Route): Promise<void> {
  const bodies = { ask: ask404Body, comment: comment404Body, message: message404Body };
  for (const other of ["ask", "comment", "message"] as const) {
    if (other !== route) await lookups.answer(other, { error: new ApiError(404, bodies[other]) });
  }
  await lookups.answer(route, { error: new ApiError(503, bodies[route]) });
  await tick(2_000);
}

let reads: Routes | undefined;
let view: RenderHookResult<ThreadLookup, Pages> | undefined;
/** The query client of the lookup drawn last, which a case refreshes as the app does. */
let drawnClient: QueryClient | undefined;

afterEach(() => {
  view?.unmount();
  view = undefined;
  reads?.restore();
  reads = undefined;
  realClock();
});

test("an id that is not a uuid is refused before any read", async () => {
  jest.useFakeTimers();
  reads = routes();
  view = lookUp(KEY, "not-a-record");
  await jump(0);
  expect(view.result.current.kind).toBe("missing");
  expect(reads.asked).toEqual({ ask: [], comment: [], message: [] });
});

test("an id that a page the view holds has is read by no lookup", async () => {
  jest.useFakeTimers();
  reads = routes();
  view = lookUp(KEY, ASK_ID, { held: true });
  await jump(0);
  expect(view.result.current.kind).toBe("held");
  expect(reads.asked).toEqual({ ask: [], comment: [], message: [] });
});

test("while a page that may hold the id is out no lookup is sent, and the three go once the pages answered without it", async () => {
  jest.useFakeTimers();
  reads = routes();
  view = lookUp(KEY, ASK_ID, { held: undefined });
  await jump(0);
  expect(view.result.current.kind).toBe("reading");
  expect(reads.asked).toEqual({ ask: [], comment: [], message: [] });
  view.rerender({ held: false });
  await jump(0);
  expect(reads.asked).toEqual({ ask: [ASK_ID], comment: [ASK_ID], message: [ASK_ID] });
  await reads.answer("ask", { body: ASK });
  expect(recordOf(view.result.current)).toBe(ASK_ID);
});

test("an id that is not a uuid draws the line at once, while the pages are still out", async () => {
  jest.useFakeTimers();
  reads = routes();
  view = lookUp(KEY, "not-a-record", { held: undefined });
  await jump(0);
  expect(view.result.current.kind).toBe("missing");
  expect(reads.asked).toEqual({ ask: [], comment: [], message: [] });
});

describe("one route answers with a record of this issue and the other two answer 404", () => {
  const answers: Record<Route, { id: string; body: unknown }> = {
    ask: { body: ASK, id: ASK_ID },
    comment: { body: HEAD, id: HEAD_ID },
    message: { body: ROOT, id: ROOT_ID },
  };
  const refusals: Record<Route, () => ApiError> = {
    ask: ASK_404,
    comment: COMMENT_404,
    message: MESSAGE_404,
  };
  const cases = (["ask", "comment", "message"] as const).flatMap((route) =>
    [0, 1, 2].map((at): [string, Route, number] => [
      `the ${route} route answers ${["first", "second", "last"][at]}`,
      route,
      at,
    ])
  );
  test.each(cases)("%s", async (_, route, at) => {
    jest.useFakeTimers();
    reads = routes();
    const { id, body } = answers[route];
    view = lookUp(KEY, id);
    await jump(0);
    // Each route is read once, all three at once, before any answers.
    expect(reads.asked).toEqual({ ask: [id], comment: [id], message: [id] });
    const others = (["ask", "comment", "message"] as const).filter((one) => one !== route);
    const order = [...others];
    order.splice(at, 0, route);
    // The lookup draws the record once its route answers, whatever is still out, and until then
    // it is reading, the 404s that came before it included.
    for (const [step, next] of order.entries()) {
      expect(recordOf(view.result.current)).toBe(step > at ? id : undefined);
      if (step <= at) expect(view.result.current.kind).toBe("reading");
      await reads.answer(next, next === route ? { body } : { error: refusals[next]() });
    }
    expect(recordOf(view.result.current)).toBe(id);
    await tick(5_000);
    expect(reads.asked).toEqual({ ask: [id], comment: [id], message: [id] });
  });
});

test("a comment reply with a quote is read once more by its reply_to, whose thread gives its record", async () => {
  jest.useFakeTimers();
  reads = routes();
  view = lookUp(KEY, QUOTE_REPLY_ID);
  await jump(0);
  await reads.answer("ask", { error: ASK_404() });
  await reads.answer("message", { error: MESSAGE_404() });
  await reads.answer("comment", { body: QUOTE_REPLY });
  await jump(0);
  const lookup = view.result.current;
  expect(recordOf(lookup)).toBe(QUOTE_REPLY_ID);
  expect(
    lookup.kind === "found" ? lookup.record.replies.map((reply) => reply.id.slice(0, 8)) : []
  ).toEqual(["fa70efe5"]);
  expect(reads.asked.comment).toEqual([QUOTE_REPLY_ID, REPLY_TO_ID]);
});

test("a quoting reply on an ask is read once more by its ask_id, whose thread gives its record", async () => {
  // No captured fixture holds a quoting reply on an ask, so this case builds one from
  // `fixtures/ask.json` and `fixtures/comment-quote-reply.json`.
  const askQuoteId = "a5c00000-0000-4000-8000-000000000001";
  const followUpId = "a5c00000-0000-4000-8000-000000000002";
  const quotedOnAsk = {
    ...QUOTE_REPLY.comment,
    answers: null,
    ask_id: ASK_ID,
    created_at: "2026-11-26T11:38:07.000000Z",
    id: askQuoteId,
    reply_to: null,
  };
  const followUp = {
    ...(ASK.replies?.[0] ?? QUOTE_REPLY.comment),
    answers: askQuoteId,
    ask_id: ASK_ID,
    created_at: "2026-11-26T11:38:08.000000Z",
    id: followUpId,
    reply_to: null,
  };
  const askWithQuote: AskRead = {
    ...ASK,
    replies: [...(ASK.replies ?? []), quotedOnAsk, followUp],
  };
  const commentRead: CommentRead = { comment: quotedOnAsk, replies: [] };

  jest.useFakeTimers();
  reads = routes();
  view = lookUp(KEY, askQuoteId);
  await jump(0);
  await reads.answer("ask", { error: ASK_404() });
  reads.again("ask");
  await reads.answer("message", { error: MESSAGE_404() });
  await reads.answer("comment", { body: commentRead });
  await jump(0);
  expect(view.result.current.kind).toBe("reading");
  expect(reads.asked.ask).toEqual([askQuoteId, ASK_ID]);
  await reads.answer("ask", { body: askWithQuote });
  const lookup = view.result.current;
  expect(recordOf(lookup)).toBe(askQuoteId);
  expect(lookup.kind === "found" ? lookup.record.thread : undefined).toBe(ASK_ID);
  expect(lookup.kind === "found" ? lookup.record.replies.map((reply) => reply.id) : []).toEqual([
    followUpId,
  ]);
  await tick(5_000);
  expect(reads.asked).toEqual({
    ask: [askQuoteId, ASK_ID],
    comment: [askQuoteId],
    message: [askQuoteId],
  });
});

test("a record whose issue_key is another issue's is no record of this page", async () => {
  jest.useFakeTimers();
  reads = routes();
  view = lookUp(SECOND_KEY, ASK_ID);
  await jump(0);
  await reads.answer("ask", { body: ASK });
  await reads.answer("comment", { error: COMMENT_404() });
  expect(view.result.current.kind).toBe("reading");
  await reads.answer("message", { error: MESSAGE_404() });
  expect(view.result.current.kind).toBe("missing");
});

test("three 404s draw the line for an address that names no record, and no route is read again", async () => {
  jest.useFakeTimers();
  reads = routes();
  view = lookUp(KEY, ASK_ID);
  await jump(0);
  await reads.answer("ask", { error: ASK_404() });
  await reads.answer("comment", { error: COMMENT_404() });
  await reads.answer("message", { error: MESSAGE_404() });
  expect(view.result.current.kind).toBe("missing");
  await tick(5_000);
  expect(reads.asked).toEqual({ ask: [ASK_ID], comment: [ASK_ID], message: [ASK_ID] });
});

test("three 404s keep the line while a page that may hold the id is read again, and no route is read again once it answers", async () => {
  jest.useFakeTimers();
  reads = routes();
  view = lookUp(KEY, ASK_ID);
  await jump(0);
  await reads.answer("ask", { error: ASK_404() });
  await reads.answer("comment", { error: COMMENT_404() });
  await reads.answer("message", { error: MESSAGE_404() });
  expect(view.result.current.kind).toBe("missing");
  // Each event of the issue reads the list's pages again, and one of them may hold the id.
  view.rerender({ held: undefined });
  await jump(0);
  expect(view.result.current.kind, "the line went while a page was read again").toBe("missing");
  view.rerender({ held: false });
  await jump(0);
  expect(view.result.current.kind, "the line went once the page answered").toBe("missing");
  await tick(5_000);
  expect(reads.asked).toEqual({ ask: [ASK_ID], comment: [ASK_ID], message: [ASK_ID] });
});

test("neither a window focus nor a refresh of every read reads a route that answered 404 again", async () => {
  jest.useFakeTimers();
  reads = routes();
  view = lookUp(KEY, ASK_ID);
  await jump(0);
  await reads.answer("ask", { error: ASK_404() });
  await reads.answer("comment", { error: COMMENT_404() });
  await reads.answer("message", { error: MESSAGE_404() });
  try {
    // The reader leaves the window and comes back to it, as a tab opened beside the page does.
    act(() => {
      focusManager.setFocused(false);
      focusManager.setFocused(true);
    });
    await jump(0);
    // The event stream's reconnect refreshes every read the client holds (`refreshQueries`).
    await act(async () => {
      await drawnClient?.invalidateQueries();
    });
    await jump(0);
    expect(view.result.current.kind).toBe("missing");
    expect(reads.asked).toEqual({ ask: [ASK_ID], comment: [ASK_ID], message: [ASK_ID] });
  } finally {
    focusManager.setFocused(undefined);
  }
});

test("a 403 is not read again either", async () => {
  jest.useFakeTimers();
  reads = routes();
  view = lookUp(KEY, ASK_ID);
  await jump(0);
  // The message route's refusal is its 404's body under the status 403.
  await reads.answer("message", { error: new ApiError(403, message404Body) });
  await reads.answer("ask", { error: ASK_404() });
  await reads.answer("comment", { error: COMMENT_404() });
  expect(view.result.current.kind).toBe("missing");
  await tick(5_000);
  expect(reads.asked.message).toEqual([ASK_ID]);
});

test("a route that fails with a 5xx draws the failed read with Retry, and Retry reads that route alone", async () => {
  jest.useFakeTimers();
  reads = routes();
  view = lookUp(KEY, ASK_ID);
  await jump(0);
  await reads.answer("comment", { error: COMMENT_404() });
  await reads.answer("message", { error: MESSAGE_404() });
  // The ask route's refusal is its 404's body under the status 503, which the app asks again
  // twice before the read stands failed.
  await reads.answer("ask", { error: new ApiError(503, ask404Body) });
  await tick(2_000);
  const failed = view.result.current;
  expect(failed.kind).toBe("failed");
  expect(reads.asked).toEqual({
    ask: [ASK_ID, ASK_ID, ASK_ID],
    comment: [ASK_ID],
    message: [ASK_ID],
  });
  reads.again("ask");
  await act(async () => {
    if (failed.kind === "failed") failed.retry();
  });
  await jump(0);
  await reads.answer("ask", { body: ASK });
  expect(recordOf(view.result.current)).toBe(ASK_ID);
  expect(reads.asked.comment).toEqual([ASK_ID]);
  expect(reads.asked.message).toEqual([ASK_ID]);
});

test("a failed read keeps its line and Retry while a page that may hold the id is read again, and is not read again once that page answers", async () => {
  jest.useFakeTimers();
  reads = routes();
  view = lookUp(KEY, ASK_ID);
  await jump(0);
  await failOn(reads, "ask");
  expect(view.result.current.kind).toBe("failed");
  // Each event of the issue reads the list's pages again, and one of them may hold the id.
  view.rerender({ held: undefined });
  await jump(0);
  expect(view.result.current.kind, "the failed read went while a page was read again").toBe(
    "failed"
  );
  view.rerender({ held: false });
  await jump(0);
  expect(view.result.current.kind, "the failed read went once the page answered").toBe("failed");
  await tick(5_000);
  expect(view.result.current.kind).toBe("failed");
  expect(reads.asked).toEqual({
    ask: [ASK_ID, ASK_ID, ASK_ID],
    comment: [ASK_ID],
    message: [ASK_ID],
  });
});

test("a message event's refresh reads a failed message lookup no more, and its line stays while the pages are read again", async () => {
  jest.useFakeTimers();
  reads = routes();
  view = lookUp(KEY, ASK_ID);
  await jump(0);
  await failOn(reads, "message");
  expect(view.result.current.kind).toBe("failed");
  // A message event of the issue refreshes every message read under the issue's key, the
  // lookup's among them (`api/event-refresh.ts`), and then the list's pages are read again.
  act(() => {
    void drawnClient?.invalidateQueries({ queryKey: ["issue-message", KEY] });
  });
  await jump(0);
  expect(
    drawnClient?.getQueryState(["issue-message", KEY, "thread", ASK_ID])?.isInvalidated,
    "the refresh did not reach the message lookup"
  ).toBe(true);
  expect(view.result.current.kind, "the failed read went as the event refreshed it").toBe("failed");
  view.rerender({ held: undefined });
  await jump(0);
  expect(view.result.current.kind, "the failed read went while a page was read again").toBe(
    "failed"
  );
  view.rerender({ held: false });
  await jump(0);
  expect(view.result.current.kind, "the failed read went once the page answered").toBe("failed");
  await tick(5_000);
  expect(reads.asked.message).toEqual([ASK_ID, ASK_ID, ASK_ID]);
});

test("Retry while a page that may hold the id is out reads the failed route again: reading while it is out, failed again if it fails, and the record once it answers", async () => {
  jest.useFakeTimers();
  reads = routes();
  view = lookUp(KEY, ASK_ID);
  await jump(0);
  await failOn(reads, "ask");
  view.rerender({ held: undefined });
  await jump(0);
  const failed = view.result.current;
  expect(failed.kind, "the failed read went while a page was read again").toBe("failed");
  reads.again("ask");
  await act(async () => {
    if (failed.kind === "failed") failed.retry();
  });
  await jump(0);
  expect(view.result.current.kind, "the failed read stayed while Retry read the route").toBe(
    "reading"
  );
  await reads.answer("ask", { error: new ApiError(503, ask404Body) });
  expect(view.result.current.kind, "the failed read came back before its retries").toBe("reading");
  await tick(2_000);
  const again = view.result.current;
  expect(again.kind).toBe("failed");
  expect(reads.asked.ask).toEqual(Array(6).fill(ASK_ID));
  reads.again("ask");
  await act(async () => {
    if (again.kind === "failed") again.retry();
  });
  await jump(0);
  expect(view.result.current.kind).toBe("reading");
  await reads.answer("ask", { body: ASK });
  expect(recordOf(view.result.current)).toBe(ASK_ID);
  expect(reads.asked.comment).toEqual([ASK_ID]);
  expect(reads.asked.message).toEqual([ASK_ID]);
});

test("a failed read is forgotten once a page holds the id: reading while a write moves the record off the pages, and read again once they answer without it", async () => {
  jest.useFakeTimers();
  reads = routes();
  view = lookUp(KEY, ROOT_ID);
  await jump(0);
  await failOn(reads, "message");
  expect(view.result.current.kind).toBe("failed");
  // A page the view reads holds the record, then a write moves it off that page while the page
  // it joins is out (R14).
  view.rerender({ held: true });
  await jump(0);
  expect(view.result.current.kind).toBe("held");
  view.rerender({ held: undefined });
  await jump(0);
  expect(
    view.result.current.kind,
    "the failed read came back while the page it joins was out"
  ).toBe("reading");
  expect(reads.asked.message, "the failed route was read while a page held the id").toEqual(
    Array(3).fill(ROOT_ID)
  );
  reads.again("message");
  view.rerender({ held: false });
  await jump(0);
  expect(
    reads.asked.message,
    "the failed route was not read again once the pages answered without the id"
  ).toEqual(Array(4).fill(ROOT_ID));
  await reads.answer("message", { body: ROOT });
  expect(recordOf(view.result.current)).toBe(ROOT_ID);
  expect(reads.asked.ask).toEqual([ROOT_ID]);
  expect(reads.asked.comment).toEqual([ROOT_ID]);
});

test("a failed read is forgotten again each time a page comes to hold the id, so a second move off the pages keeps the record open too", async () => {
  jest.useFakeTimers();
  reads = routes();
  view = lookUp(KEY, ROOT_ID);
  await jump(0);
  await failOn(reads, "message");
  expect(view.result.current.kind).toBe("failed");
  // A page holds the record, then the pages answer without it, and the route sent again fails.
  view.rerender({ held: true });
  await jump(0);
  reads.again("message");
  view.rerender({ held: false });
  await jump(0);
  await reads.answer("message", { error: new ApiError(503, message404Body) });
  await tick(2_000);
  expect(view.result.current.kind).toBe("failed");
  expect(reads.asked.message).toEqual(Array(6).fill(ROOT_ID));
  // A page holds the record again, then a write moves it off that page while the page it joins
  // is out (R14).
  view.rerender({ held: true });
  await jump(0);
  expect(view.result.current.kind).toBe("held");
  view.rerender({ held: undefined });
  await jump(0);
  expect(
    view.result.current.kind,
    "the failed read of the second time the pages answered without the id came back while the page it joins was out"
  ).toBe("reading");
  expect(reads.asked.message, "the failed route was read while a page held the id").toEqual(
    Array(6).fill(ROOT_ID)
  );
});

test("a failed read is forgotten once while a page holds the id, so a reference to the record reads a route that fails every time once more, and not over and over", async () => {
  jest.useFakeTimers();
  reads = routes();
  // A reference to the ask drawn on the page (`RefLink`) reads it under the lookup's own key, and
  // its read stays enabled once a page holds the id, where the lookup's own is not.
  view = lookUp(KEY, ASK_ID, { held: false }, () => useReferenceQuery(askQuery(ASK_ID)));
  await jump(0);
  // The ask route's read stays refused with its 503 from here on, so every read of it fails.
  await failOn(reads, "ask");
  expect(view.result.current.kind).toBe("failed");
  expect(reads.asked.ask).toEqual(Array(3).fill(ASK_ID));
  view.rerender({ held: true });
  await jump(0);
  expect(view.result.current.kind).toBe("held");
  // A round of reads, a try and the app's two retries 500 ms apart, ends within 1 s, so ten
  // rounds would pass here.
  await tick(10_000);
  // Three reads before a page held the id, a try and its two retries, and three more as the
  // reference reads the forgotten failure again, once. That read's failure came after a page held
  // the id, so it is not forgotten, and no read follows it.
  expect(
    reads.asked.ask,
    "the failing route was read again and again while a page held the id"
  ).toEqual(Array(6).fill(ASK_ID));
  expect(view.result.current.kind).toBe("held");
});

test("a route that loses its connection draws the failed read with Retry", async () => {
  jest.useFakeTimers();
  reads = routes();
  view = lookUp(KEY, ASK_ID);
  await jump(0);
  await reads.answer("ask", { error: ASK_404() });
  await reads.answer("comment", { error: COMMENT_404() });
  await reads.answer("message", {
    error: new NetworkError(
      routeUrl("/api/v1/messages/{id}", { id: ASK_ID }),
      new TypeError("lost")
    ),
  });
  await tick(2_000);
  expect(view.result.current.kind).toBe("failed");
});
