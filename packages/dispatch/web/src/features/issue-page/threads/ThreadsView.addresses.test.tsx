import { beforeAll, describe, expect, spyOn, test } from "bun:test";
import type { CommentThreadPage, MessageThreadPage } from "@legion/contracts";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";
import { http } from "msw";

import { answerRequests } from "../../../__tests__/network";
import { api } from "../../../api/client";
import { warmMarkdownRenderer } from "../../refs/markdown-engine";
import { ANSWER_LINES, DASHBOARD_WORDS, THREADS_WORDS } from "../page-words";
import ask404Body from "./fixtures/ask-404.json";
import asksThreadBody from "./fixtures/asks-thread.json";
import comment404Body from "./fixtures/comment-404.json";
import commentsInBody from "./fixtures/comments-in.json";
import message404Body from "./fixtures/message-404.json";
import messagesInBody from "./fixtures/messages-in.json";
import messagesNoneBody from "./fixtures/messages-none.json";
import { answerThreads, drawThreads, stubWindow, threadsAt } from "./threads-view-fixture";

/**
 * The addresses of Threads (dispatch://INFRA-5704/spec#b-r-addresses, R12, R15) over the server's
 * answers captured at the stack base (`fixtures/README.md`), as plan list 3 table E gives their
 * states - a row pressed, Back, Escape with no caret and with one, and a cold load of
 * `/threads/<id>` - and as table B's lookups draw in the view: the record a lookup gives, under
 * its own choice, and the line for an address that names no record.
 */

/** The newest thread of All's first pages, the reader's comment: a record a page holds. */
const FIRST = (commentsInBody as unknown as CommentThreadPage).threads?.[0]?.comment.id ?? "";
/** The roots of All's message threads. */
const MESSAGE_ROOTS = new Set(
  ((messagesInBody as unknown as MessageThreadPage).threads ?? []).map((row) => row.message.id)
);
/** The thread between agents, Agents' one row, which no page of All holds. */
const AGENTS_ROW = (messagesNoneBody as unknown as MessageThreadPage).threads?.[0];
const AGENTS = AGENTS_ROW?.message.id ?? "";
/** An id that names nothing on the server. */
const NOBODY = "00000000-0000-4000-8000-000000000000";

const rows = () => [...document.querySelectorAll<HTMLElement>("[data-thread-row]")];
const rowIds = () => rows().map((row) => row.getAttribute("data-thread-row"));
const record = () => document.querySelector("[data-thread-record]");
const main = () => document.querySelector<HTMLElement>("[data-thread-main]");
const choice = (name: string) => screen.getByRole("button", { name });
const footField = () =>
  within(
    document.querySelector<HTMLElement>('[data-thread-box="foot"]') ?? document.body
  ).getByRole("textbox");

/** How many times each lookup was read, by the route. */
interface Lookups {
  asks: number;
  comments: number;
  messages: number;
}

/**
 * Answers R12's three lookups: the ask and the comment route each with its `404`, and the message
 * route with the thread between agents read by its root, as `GET /api/v1/messages/{id}` answers
 * a thread (its root and its replies), and its `404` for every other id. Agents' page is the
 * server's own, the thread between agents. Every other read is Threads' own (`answerThreads`).
 */
function answerLookups(): Lookups {
  const sent: Lookups = { asks: 0, comments: 0, messages: 0 };
  answerRequests(
    http.get("/api/v1/asks/:id", () => {
      sent.asks += 1;
      return Response.json(ask404Body, { status: 404 });
    }),
    http.get("/api/v1/comments/:id", () => {
      sent.comments += 1;
      return Response.json(comment404Body, { status: 404 });
    }),
    http.get("/api/v1/messages/:id", ({ params }) => {
      sent.messages += 1;
      return params.id === AGENTS && AGENTS_ROW !== undefined
        ? Response.json({ message: AGENTS_ROW.message, replies: AGENTS_ROW.replies })
        : Response.json(message404Body, { status: 404 });
    }),
    http.get("/api/v1/issues/:key/messages", ({ request }) =>
      new URL(request.url).searchParams.get("person") === "none"
        ? Response.json(messagesNoneBody)
        : undefined
    )
  );
  return sent;
}

/** Threads at 1440 px at `path`, every read answered. */
function drawAt(path: string, { lookups = false } = {}) {
  const window = stubWindow(1440);
  answerThreads();
  // Answered last, so each runs before Threads' own.
  const sent = lookups ? answerLookups() : undefined;
  const drawn = drawThreads(path);
  return {
    ...drawn,
    sent,
    done(): void {
      drawn.done();
      window.restore();
    },
  };
}

/** Presses the link of the row of `id`. */
async function pressRow(user: UserEvent, id: string): Promise<void> {
  const row = rows().find((one) => one.getAttribute("data-thread-row") === id);
  if (row === undefined) throw new Error(`The list draws no row of ${id}.`);
  await user.click(within(row).getByRole("link"));
  await waitFor(() => expect(record()).toHaveAttribute("data-thread-record", id));
}

beforeAll(async () => {
  await warmMarkdownRenderer();
});

describe("table E: open, Back and Escape (R12)", () => {
  test("a row pressed pushes its address, and Back returns to the list with the same choice and the rows it had shown", async () => {
    const drawn = drawAt(threadsAt());
    const user = userEvent.setup();
    try {
      await waitFor(() => expect(rows().length, "All drew no row").toBe(20));
      await user.click(
        screen.getByRole("button", { name: new RegExp(`^${DASHBOARD_WORDS.next_twenty}`) })
      );
      await waitFor(() => expect(rows().length, "the control drew no next rows").toBe(40));
      const shown = rowIds();
      const pressed = shown[30] ?? "";
      await pressRow(user, pressed);
      expect(drawn.address(), "the row did not open its record's address").toBe(threadsAt(pressed));
      drawn.back();
      await waitFor(() => expect(record(), "Back left the record open").toBeNull());
      expect(drawn.address()).toBe(threadsAt());
      expect(choice(THREADS_WORDS.choices.all)).toHaveAttribute("aria-pressed", "true");
      expect(rowIds(), "Back drew other rows than the list had shown").toEqual(shown);
    } finally {
      drawn.done();
    }
  });

  test("Escape with no caret in a box closes the record and returns to the list", async () => {
    const drawn = drawAt(threadsAt());
    const user = userEvent.setup();
    try {
      await waitFor(() => expect(rows().length).toBe(20));
      await pressRow(user, FIRST);
      await user.keyboard("{Escape}");
      await waitFor(() => expect(record(), "Escape left the record open").toBeNull());
      expect(drawn.address()).toBe(threadsAt());
      expect(rows().length, "the list left with the record").toBe(20);
    } finally {
      drawn.done();
    }
  });

  test("Escape with the caret in a comment's box or a message's box takes the caret out, and the record and the words stay", async () => {
    const drawn = drawAt(threadsAt());
    const user = userEvent.setup();
    try {
      await waitFor(() => expect(rows().length).toBe(20));
      const message = rowIds().find((id) => id !== null && MESSAGE_ROOTS.has(id));
      expect(message, "All's first rows hold no message thread").toBeDefined();
      for (const id of [FIRST, message ?? ""]) {
        await pressRow(user, id);
        await user.click(footField());
        await user.type(footField(), "Words kept.");
        expect(footField()).toHaveFocus();
        await user.keyboard("{Escape}");
        expect(footField(), `Escape in ${id}'s box kept the caret there`).not.toHaveFocus();
        expect(record(), `Escape in ${id}'s box closed the record`).toHaveAttribute(
          "data-thread-record",
          id
        );
        expect(footField(), `Escape in ${id}'s box took its words`).toHaveValue("Words kept.");
        expect(drawn.address()).toBe(threadsAt(id));
      }
    } finally {
      drawn.done();
    }
  });

  test("a cold load of /threads/<id> that All's first pages hold opens the record with its row drawn, and sends no lookup", async () => {
    const lookups = [
      spyOn(api, "getAsk"),
      spyOn(api, "getComment"),
      spyOn(api, "getMessageThread"),
    ];
    const drawn = drawAt(threadsAt(FIRST));
    try {
      await waitFor(() => expect(record()).toHaveAttribute("data-thread-record", FIRST));
      await waitFor(() => expect(rowIds(), "the list draws no row of the record").toContain(FIRST));
      await waitFor(() => expect(drawn.client.isFetching(), "a read is still out").toBe(0));
      expect(choice(THREADS_WORDS.choices.all)).toHaveAttribute("aria-pressed", "true");
      for (const lookup of lookups) {
        expect(lookup, "a record a page holds was looked up").not.toHaveBeenCalled();
      }
    } finally {
      for (const lookup of lookups) lookup.mockRestore();
      drawn.done();
    }
  });
});

describe("table B in the view: the lookups' record and line (R12, R15)", () => {
  test("a thread between agents that no page of All holds is looked up once All's first pages answered, once on each route, and opens under Agents with its row", async () => {
    const window = stubWindow(1440);
    const asks = Promise.withResolvers<void>();
    answerThreads();
    const sent = answerLookups();
    answerRequests(
      http.get("/api/v1/issues/:key/asks", async () => {
        await asks.promise;
        return Response.json(asksThreadBody);
      })
    );
    const drawn = drawThreads(threadsAt(AGENTS));
    try {
      await waitFor(() => expect(document.querySelector("[data-thread-list]")).not.toBeNull());
      await waitFor(() => expect(drawn.client.isFetching(), "a read but the asks' is out").toBe(1));
      expect(sent, "a lookup went before All's first pages answered").toEqual({
        asks: 0,
        comments: 0,
        messages: 0,
      });
      asks.resolve();
      await waitFor(() => expect(record()).toHaveAttribute("data-thread-record", AGENTS));
      expect(choice(THREADS_WORDS.choices.agents)).toHaveAttribute("aria-pressed", "true");
      await waitFor(() => expect(rowIds(), "Agents draws no row of the record").toEqual([AGENTS]));
      await waitFor(() => expect(drawn.client.isFetching(), "a read is still out").toBe(0));
      expect(sent, "a lookup was sent other than once").toEqual({
        asks: 1,
        comments: 1,
        messages: 1,
      });
    } finally {
      asks.resolve();
      drawn.done();
      window.restore();
    }
  });

  test("an id that names no record of the issue draws the line for an address it cannot find", async () => {
    const drawn = drawAt(threadsAt(NOBODY), { lookups: true });
    try {
      await waitFor(() =>
        expect(main(), "the main column draws no line for the address").toHaveTextContent(
          ANSWER_LINES.threads_not_found
        )
      );
      expect(record()).toBeNull();
      expect(drawn.sent).toEqual({ asks: 1, comments: 1, messages: 1 });
    } finally {
      drawn.done();
    }
  });

  test("an id that is no uuid draws the line at once and sends no lookup", async () => {
    const drawn = drawAt(threadsAt("not-a-thread"), { lookups: true });
    try {
      await waitFor(() => expect(main()).toHaveTextContent(ANSWER_LINES.threads_not_found));
      await waitFor(() => expect(rows().length).toBe(20));
      await waitFor(() => expect(drawn.client.isFetching(), "a read is still out").toBe(0));
      expect(drawn.sent).toEqual({ asks: 0, comments: 0, messages: 0 });
    } finally {
      drawn.done();
    }
  });
});
