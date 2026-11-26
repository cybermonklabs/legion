import { describe, expect, spyOn, test } from "bun:test";
import type { CommentThreadPage } from "@legion/contracts";
import { QueryClient } from "@tanstack/react-query";
import { act, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http } from "msw";
import type { ReactNode } from "react";
import { MemoryRouter, useLocation } from "react-router-dom";

import { answerRequests } from "../../../__tests__/network";
import { api } from "../../../api/client";
import { dashboardRepliesQuery } from "../../dashboard/useWaitingItems";
import { parseIssuePath } from "../../refs/routes";
import { issueDetails } from "../decisions-filter/decisions-fixtures";
import { NoTooltips } from "../NoTooltips";
import { ANSWER_LINES, CONTROL_LABELS, EMPTY_STATE_LINES, THREADS_WORDS } from "../page-words";
import { AppProviders } from "../slots/stage-fixtures";
import asksThreadBody from "./fixtures/asks-thread.json";
import commentsBareBody from "./fixtures/comments-bare.json";
import commentsInBody from "./fixtures/comments-in.json";
import commentsInBeforeBody from "./fixtures/comments-in-before.json";
import eventsBody from "./fixtures/events.json";
import meStateBody from "./fixtures/me-state.json";
import messagesInBody from "./fixtures/messages-in.json";
import messagesNoneAuthorBody from "./fixtures/messages-none-author.json";
import repliesUnreadBody from "./fixtures/replies-unread.json";
import { ThreadsView } from "./ThreadsView";
import { commentThreadsQuery, messageThreadsQuery, threadEventsQuery } from "./thread-reads";

/**
 * The Threads view over the server's answers captured at the stack base (`fixtures/README.md`),
 * with a query client, as plan list 3 tables F (the widths, R2) and G (load, failure, an older
 * build and empty, plan choice 7) give its states.
 */

/** The seeded issue the fixtures were read from, and its project. */
const KEY = "PDFD720CB-1";
const ISSUE = { ...issueDetails(), key: KEY, project: "PDFD720CB" };

/** The newest thread of All's first pages, the reader's comment: a record a page holds. */
const FIRST = (commentsInBody as unknown as CommentThreadPage).threads?.[0]?.comment.id ?? "";

/** The widths the cases read: a desktop's, past R2's 1024 px, and a phone's, below it. */
const DESKTOP_WIDTH = 1440;
const PHONE_WIDTH = 390;

/** Whether a window `width` px wide matches `query`, one `(min-width: …)` or `(max-width: …)`
 *  clause in `px` or `rem`. Any other query throws, naming it, since a guessed answer would let a
 *  case pass on a layout no screen draws. */
function widthMatches(width: number, query: string): boolean {
  const clause = /^\((min|max)-width:\s*([\d.]+)(px|rem)\)$/.exec(query.trim());
  if (clause === null) throw new Error(`The window's stub answers no media query "${query}".`);
  const px = Number(clause[2]) * (clause[3] === "rem" ? 16 : 1);
  return clause[1] === "min" ? width >= px : width <= px;
}

/** A window `width` px wide that a case can narrow and widen: each media query answers by its
 *  width, and each query's listeners hear a change of it, as a resize makes them. */
function stubWindow(width: number) {
  const original = window.matchMedia;
  let current = width;
  const heard = new Set<() => void>();
  window.matchMedia = ((query: string) => ({
    addEventListener: (_type: string, listener: () => void) => heard.add(listener),
    addListener: () => {},
    dispatchEvent: () => true,
    get matches() {
      return widthMatches(current, query);
    },
    media: query,
    onchange: null,
    removeEventListener: (_type: string, listener: () => void) => heard.delete(listener),
    removeListener: () => {},
  })) as unknown as typeof window.matchMedia;
  return {
    resize(next: number): void {
      current = next;
      act(() => {
        for (const listener of heard) listener();
      });
    },
    restore(): void {
      window.matchMedia = original;
    },
  };
}

/** What a case answers a route with in place of its captured page. */
type RouteAnswer = () => Response | Promise<Response>;

/**
 * Answers every read Threads sends with the server's captured answers: All's three first pages,
 * the comments' next page from its `before`, Agents' page, the events read, the reader's unread
 * threads and the reader's state. A case answers a route of All its own way through `over`. The
 * answer counts the reads of each route.
 */
function answerThreads(over: Partial<Record<"asks" | "comments" | "messages", RouteAnswer>> = {}) {
  const sent = { agents: 0, asks: 0, comments: 0, messages: 0 };
  answerRequests(
    http.get("/api/v1/issues/:key/comments", ({ request }) => {
      sent.comments += 1;
      if (over.comments !== undefined) return over.comments();
      const next = new URL(request.url).searchParams.has("before");
      return Response.json(next ? commentsInBeforeBody : commentsInBody);
    }),
    http.get("/api/v1/issues/:key/messages", ({ request }) => {
      if (new URL(request.url).searchParams.get("person") === "none") {
        sent.agents += 1;
        // The server's answer for a route that holds no thread (`person=none&author=alice`).
        return Response.json(messagesNoneAuthorBody);
      }
      sent.messages += 1;
      return over.messages?.() ?? Response.json(messagesInBody);
    }),
    http.get("/api/v1/issues/:key/asks", () => {
      sent.asks += 1;
      return over.asks?.() ?? Response.json(asksThreadBody);
    }),
    http.get("/api/v1/issues/:key/events", () => Response.json(eventsBody)),
    http.get("/api/v1/me/replies", () => Response.json(repliesUnreadBody)),
    http.get("/api/v1/me/state", () => Response.json(meStateBody))
  );
  return sent;
}

/** Threads at the address the router holds, as the stage draws it, and that address. */
function AtAddress(): ReactNode {
  const { pathname, search } = useLocation();
  const route = parseIssuePath(pathname, search);
  return (
    <>
      <output data-testid="address">{pathname}</output>
      {route === undefined ? null : <ThreadsView issue={ISSUE} route={route} />}
    </>
  );
}

/** Threads opened at `path`, under what the app and the page provide, signed in as the reader
 *  the fixtures were read for. */
function drawThreads(path: string) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, retryDelay: 0 } } });
  const who = spyOn(api, "whoAmI").mockResolvedValue({ kind: "user", login: "alice" } as never);
  const view = render(
    <MemoryRouter initialEntries={[path]}>
      <AppProviders client={client}>
        <NoTooltips>
          <AtAddress />
        </NoTooltips>
      </AppProviders>
    </MemoryRouter>
  );
  return {
    client,
    view,
    done(): void {
      view.unmount();
      who.mockRestore();
    },
  };
}

const listPath = `/issues/${KEY}/threads`;
const recordPath = `${listPath}/${FIRST}`;

const list = () => document.querySelector("[data-thread-list]");
const record = () => document.querySelector("[data-thread-record]");
const rows = () => document.querySelectorAll("[data-thread-row]");
const backToList = () => screen.queryByRole("button", { name: THREADS_WORDS.back_to_list });

describe("table F: the widths (R2)", () => {
  test("at 1440 px the list and the opened record stand side by side", async () => {
    const window = stubWindow(DESKTOP_WIDTH);
    answerThreads();
    const drawn = drawThreads(recordPath);
    try {
      await waitFor(() => expect(record(), "no record opened at 1440 px").not.toBeNull());
      expect(record()).toHaveAttribute("data-thread-record", FIRST);
      expect(list(), "the list leaves when a record opens at 1440 px").not.toBeNull();
      expect(rows().length, "the list draws no row beside the record").toBeGreaterThan(0);
      expect(
        document.querySelector("[data-thread-main]")?.contains(record() ?? null),
        "the record is not in the main column"
      ).toBe(true);
      expect(backToList(), "the control back to the list stands at 1440 px").toBeNull();
    } finally {
      drawn.done();
      window.restore();
    }
  });

  test("at 390 px with no record the list stands alone", async () => {
    const window = stubWindow(PHONE_WIDTH);
    answerThreads();
    const drawn = drawThreads(listPath);
    try {
      await waitFor(() => expect(rows().length, "the list draws no row at 390 px").toBe(20));
      expect(
        document.querySelector("[data-thread-main]"),
        "the main column stands beside the list at 390 px"
      ).toBeNull();
      expect(
        screen.queryByText(EMPTY_STATE_LINES.threads_none_open),
        "the main column's line stands at 390 px"
      ).toBeNull();
    } finally {
      drawn.done();
      window.restore();
    }
  });

  test("at 390 px an opened record stands alone with the control back to the list, which returns to it", async () => {
    const window = stubWindow(PHONE_WIDTH);
    answerThreads();
    const drawn = drawThreads(recordPath);
    try {
      await waitFor(() => expect(record(), "no record opened at 390 px").not.toBeNull());
      expect(list(), "the list and the record both stand at 390 px").toBeNull();
      const back = backToList();
      expect(back, "an opened record draws no control back to the list").not.toBeNull();
      if (back !== null) await userEvent.setup().click(back);
      await waitFor(() => expect(rows().length, "the list did not return").toBe(20));
      expect(screen.getByTestId("address")).toHaveTextContent(listPath);
      expect(record(), "the record stays open once the list returns").toBeNull();
    } finally {
      drawn.done();
      window.restore();
    }
  });

  test("a record open at 1440 px stands alone once the window narrows to 390 px, and beside the list once it widens again", async () => {
    const window = stubWindow(DESKTOP_WIDTH);
    answerThreads();
    const drawn = drawThreads(recordPath);
    try {
      await waitFor(() => expect(record(), "no record opened at 1440 px").not.toBeNull());
      expect(list()).not.toBeNull();
      window.resize(PHONE_WIDTH);
      expect(record(), "the record leaves as the window narrows").not.toBeNull();
      expect(list(), "the list stands beside the record at 390 px").toBeNull();
      expect(backToList(), "no control back to the list at 390 px").not.toBeNull();
      window.resize(DESKTOP_WIDTH);
      expect(record(), "the record leaves as the window widens").not.toBeNull();
      expect(list(), "the list does not return beside the record at 1440 px").not.toBeNull();
      expect(rows().length).toBe(20);
      expect(backToList(), "the control back to the list stays at 1440 px").toBeNull();
    } finally {
      drawn.done();
      window.restore();
    }
  });
});

describe("table G: load, failure, an older build and empty (plan choice 7)", () => {
  test("while one of All's three first pages is out All draws no row, and its 20 rows once it answers", async () => {
    const window = stubWindow(DESKTOP_WIDTH);
    const asks = Promise.withResolvers<void>();
    answerThreads({
      asks: async () => {
        await asks.promise;
        return Response.json(asksThreadBody);
      },
    });
    const drawn = drawThreads(listPath);
    try {
      const { client } = drawn;
      await waitFor(() => {
        for (const queryKey of [
          commentThreadsQuery(KEY).queryKey,
          messageThreadsQuery(KEY, "in").queryKey,
          threadEventsQuery(KEY).queryKey,
          dashboardRepliesQuery(KEY).queryKey,
          ["user-state"],
        ]) {
          expect(client.getQueryData(queryKey), `${JSON.stringify(queryKey)} is out`).toBeDefined();
        }
      });
      expect(rows().length, "All drew rows with the asks' page out").toBe(0);
      await act(async () => asks.resolve());
      await waitFor(() => expect(rows().length, "All drew no row once the asks answered").toBe(20));
    } finally {
      asks.resolve();
      drawn.done();
      window.restore();
    }
  });

  test("a route that fails draws the page's failed-read line with Retry, which reads that route again", async () => {
    const window = stubWindow(DESKTOP_WIDTH);
    let lost = true;
    const sent = answerThreads({
      messages: () => (lost ? Response.error() : Response.json(messagesInBody)),
    });
    const drawn = drawThreads(listPath);
    try {
      await screen.findByText(ANSWER_LINES.threads_unreadable);
      await waitFor(() => expect(drawn.client.isFetching(), "a read is still out").toBe(0));
      expect(rows().length, "All drew rows with the messages' page failed").toBe(0);
      expect(
        screen.queryByText(EMPTY_STATE_LINES.threads),
        "All drew its empty line with a read failed"
      ).toBeNull();
      const before = sent.messages;
      lost = false;
      await userEvent.setup().click(screen.getByRole("button", { name: CONTROL_LABELS.retry }));
      await waitFor(() => expect(rows().length, "Retry drew no row").toBe(20));
      expect(sent.messages, "Retry did not read the messages' page once more").toBe(before + 1);
      expect(screen.queryByText(ANSWER_LINES.threads_unreadable)).toBeNull();
    } finally {
      drawn.done();
      window.restore();
    }
  });

  test("a build that answers a route's bare list draws the older-build sentence, and that route is read once", async () => {
    const window = stubWindow(DESKTOP_WIDTH);
    const sent = answerThreads({ comments: () => Response.json(commentsBareBody) });
    const drawn = drawThreads(listPath);
    try {
      await screen.findByText(ANSWER_LINES.threads_older_build);
      await waitFor(() => expect(drawn.client.isFetching(), "a read is still out").toBe(0));
      expect(rows().length, "All drew rows with an older build's answer").toBe(0);
      expect(
        screen.queryByRole("button", { name: CONTROL_LABELS.retry }),
        "the older build's answer draws a Retry"
      ).toBeNull();
      expect(screen.queryByText(ANSWER_LINES.threads_unreadable)).toBeNull();
      expect(sent.comments, "the older build's route was read again").toBe(1);
    } finally {
      drawn.done();
      window.restore();
    }
  });

  test("a choice with no row draws its empty line, and reads its route only once the reader turns it on", async () => {
    const window = stubWindow(DESKTOP_WIDTH);
    const sent = answerThreads();
    const drawn = drawThreads(listPath);
    try {
      await waitFor(() => expect(rows().length, "All drew no row").toBe(20));
      expect(sent.agents, "Agents read its route before the reader turned it on").toBe(0);
      const agents = screen.getByRole("button", { name: THREADS_WORDS.choices.agents });
      await userEvent.setup().click(agents);
      expect(agents).toHaveAttribute("aria-pressed", "true");
      await screen.findByText(EMPTY_STATE_LINES.threads_agents);
      expect(rows().length, "Agents drew a row of an empty page").toBe(0);
      expect(sent.agents).toBe(1);
    } finally {
      drawn.done();
      window.restore();
    }
  });
});
