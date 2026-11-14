import { afterEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import { issueQuery } from "../../api/queries";
import type {
  Agent,
  Comment,
  IssueDetails,
  Message,
  MessageRead,
  MyReplyRow,
  MyReplyThreadRow,
} from "../../api/types";
import { thePageLeftItThere } from "../doc/readers-place";
import { plannerAgentRow } from "../threads/replies-fixture";
import { ThreadBody } from "./ThreadBody";
import { ThreadRowCard } from "./ThreadRowCard";

/** Every API mock a test installed, restored after it. */
const restores: { mockRestore: () => void }[] = [];

afterEach(() => {
  cleanup();
  for (const mock of restores.splice(0)) {
    mock.mockRestore();
  }
  window.localStorage.clear();
});

function spy<K extends keyof typeof api>(name: K, value: unknown): void {
  restores.push(spyOn(api, name).mockResolvedValue(value as never));
}

const session: Agent = {
  capabilities: ["steer", "btw"],
  dir: "/work",
  host: "box",
  last_activity: "2026-10-12T00:00:00Z",
  last_seen: Date.now(),
  machine_id: "box",
  open_asks: 0,
  roles: [],
  session_id: "planner",
  state: "live",
  title: "Planner",
};

function message(id: string, body: string, overrides: Partial<Message> = {}): Message {
  return {
    author: { id: "alice", kind: "user" },
    body,
    broadcast_id: null,
    created_at: "2026-10-12T00:00:00Z",
    deliveries: [],
    id,
    in_reply_to: null,
    issue_key: null,
    reply_block: null,
    target: "session:planner",
    ...overrides,
  };
}

function commentRow(overrides: Partial<MyReplyThreadRow> = {}): MyReplyThreadRow {
  return {
    issue: { key: "CORE-1", title: "Fix the thing" },
    kind: "comment",
    read_through: "2026-10-12T00:30:00Z",
    ref: "/issues/CORE-1?comment=comment-1",
    reply: {
      author: { id: "bob", kind: "user" },
      created_at: "2026-10-12T01:00:00Z",
      first_line: "Agreed.",
      id: "comment-2",
      kind: "reply",
    },
    subject: "The retry budget",
    thread_id: "comment-1",
    unread: true,
    unread_count: 1,
    version: 1,
    ...overrides,
  };
}

function comment(id: string, body: string, overrides: Partial<Comment> = {}): Comment {
  return {
    anchor: null,
    answers: null,
    artifact_id: null,
    ask_id: null,
    author: { id: "alice", kind: "user" },
    body,
    created_at: "2026-10-12T00:00:00Z",
    deliveries: [],
    edited_at: null,
    id,
    issue_key: "CORE-1",
    mentions: [],
    reply_block: null,
    reply_to: null,
    resolved: false,
    resolved_at: null,
    resolved_by: null,
    suggestion: null,
    turn: null,
    ...overrides,
  };
}

function renderCard(thread: MyReplyRow): void {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  const tree: ReactNode = (
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <ThreadRowCard thread={thread} />
      </QueryClientProvider>
    </MemoryRouter>
  );
  render(tree);
}

/** The reading column a thread is drawn in, and where its first unread message is drawn down
 *  it: happy-dom lays nothing out, computes no overflow and scrolls nothing, so what
 *  `whatScrolls` reads to find the box and the rectangles the placement measures are stated
 *  here (`features/doc/readers-place.ts`). */
const COLUMN_TOP = 40;
const COLUMN_HEIGHT = 600;
const UNREAD_BELOW = 500;

function readingColumn(): HTMLElement {
  let scrolled = 0;
  const column = document.createElement("div");
  column.style.overflowY = "auto";
  Object.defineProperty(column, "clientHeight", { configurable: true, get: () => COLUMN_HEIGHT });
  Object.defineProperty(column, "scrollHeight", { configurable: true, get: () => 4_000 });
  Object.defineProperty(column, "scrollTop", {
    configurable: true,
    get: () => scrolled,
    set: (next: number) => {
      scrolled = next;
    },
  });
  document.body.append(column);
  return column;
}

test("a thread opened at its first unread message is the page's own move", async () => {
  spy("getIssue", { closed_at: null } as unknown as IssueDetails);
  spy("listComments", [
    comment("comment-1", "The retry budget is too small."),
    comment("comment-2", "Agreed.", {
      author: { id: "bob", kind: "user" },
      created_at: "2026-10-12T01:00:00Z",
      reply_to: "comment-1",
    }),
  ]);
  const column = readingColumn();
  const rect = spyOn(Element.prototype, "getBoundingClientRect").mockImplementation(function (
    this: Element
  ) {
    if (this === column) {
      return {
        bottom: COLUMN_TOP + COLUMN_HEIGHT,
        height: COLUMN_HEIGHT,
        top: COLUMN_TOP,
      } as DOMRect;
    }
    // The first unread message is drawn `UNREAD_BELOW` down the column, and so is each wrapper
    // the thread draws around it - one of which is what the placement measures.
    const holdsUnread = this.querySelector("[data-inbox-thread-first-unread]") !== null;
    const top = COLUMN_TOP + (holdsUnread ? UNREAD_BELOW : 0) - column.scrollTop;
    return { bottom: top + 100, height: 100, top } as DOMRect;
  });
  restores.push(rect);
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  // The reads the Inbox already holds when it opens a thread in the reading pane: the body
  // draws its messages in the commit it mounts in, which is the commit whose effect places the
  // reader at the first unread one.
  client.setQueryData(issueQuery("CORE-1").queryKey, {
    closed_at: null,
  } as unknown as IssueDetails);
  client.setQueryData(
    ["comments", "CORE-1"],
    [
      comment("comment-1", "The retry budget is too small."),
      comment("comment-2", "Agreed.", {
        author: { id: "bob", kind: "user" },
        created_at: "2026-10-12T01:00:00Z",
        reply_to: "comment-1",
      }),
    ]
  );

  try {
    render(
      <MemoryRouter>
        <QueryClientProvider client={client}>
          <ThreadBody openAtFirstUnread thread={commentRow()} />
        </QueryClientProvider>
      </MemoryRouter>,
      { container: column }
    );
    await screen.findByTestId("inbox-thread-comment");

    // The thread opening where the reader left off is the page placing what it draws, written
    // through the one call that records it (`placeInView`, `features/doc/readers-place.ts`): it
    // measures what the browser's own `scrollIntoView({ block: "start" })` would do and then
    // writes it through the recorded call, so a landing correction watching the same column
    // goes on watching (`refocus-when-settled.ts`).
    await waitFor(() => expect(column.scrollTop).toBe(UNREAD_BELOW));
    expect(thePageLeftItThere(column)).toBe(true);

    // The reader scrolls the column themselves, and the correction stops.
    column.scrollTop = 120;
    expect(thePageLeftItThere(column)).toBe(false);
  } finally {
    cleanup();
    column.remove();
  }
});

test("an Agent row's card draws the message, the session's reply and the composer", async () => {
  spy("listAgents", [session]);
  spy("listAgentMessages", [
    {
      message: message("message-1", "Have a look at the build"),
      replies: [
        message("reply-1", "Looked at it.", {
          author: { id: "planner", kind: "session" },
          created_at: "2026-10-12T01:00:00Z",
          in_reply_to: "message-1",
        }),
      ],
    } satisfies MessageRead,
  ]);
  renderCard(plannerAgentRow());

  const body = await screen.findByTestId("inbox-thread-agent");
  // Every message is drawn whole, by the one markdown renderer, which warms asynchronously.
  await waitFor(() => expect(body.textContent).toContain("Have a look at the build"));
  expect(body.textContent).toContain("Looked at it.");
  // The row names the unread reply, so only that message is marked New.
  const turns = [...body.querySelectorAll("[data-inbox-thread-turn]")];
  expect(turns.map((turn) => turn.getAttribute("data-inbox-thread-turn"))).toEqual([
    "message-1",
    "reply-1",
  ]);
  expect(turns[0]?.hasAttribute("data-inbox-thread-unread")).toBe(false);
  expect(turns[1]?.hasAttribute("data-inbox-thread-unread")).toBe(true);
  expect(within(turns[1] as HTMLElement).getByText("New")).toBeTruthy();
  await waitFor(() => expect(body.querySelector("[data-inbox-agent-composer]")).not.toBeNull());
});

test("a reply the session took as a user turn says so and links the live view", async () => {
  spy("listAgents", [session]);
  spy("listAgentMessages", [
    {
      message: message("message-1", "Have a look at the build"),
      replies: [
        message("reply-1", "Running it now.", {
          created_at: "2026-10-12T01:00:00Z",
          deliveries: [
            {
              accepted_as: "user_turn",
              accepted_at: "2026-10-12T01:00:05Z",
              attempt: 1,
              created_at: "2026-10-12T01:00:01Z",
              delivery: "steer",
              duplicate: false,
              envelope_id: "envelope-1",
              error: null,
              message_id: "reply-1",
              reply_id: null,
              requested_by: null,
              session_id: "planner",
              state: "sent",
            },
          ],
          in_reply_to: "message-1",
        }),
      ],
    } satisfies MessageRead,
  ]);
  renderCard(plannerAgentRow({ unread_reply_ids: [] }));

  const note = await screen.findByTestId("agent-reply-accepted");
  expect(note.textContent).toBe("Taken as a turn of the live conversation.");
  const live = screen.getByRole("link", { name: "Live view" });
  expect(live.getAttribute("href")).toBe("/agents/planner/live");
});

test("a session the live list no longer holds says so and offers no composer", async () => {
  spy("listAgents", []);
  spy("listAgentMessages", [
    {
      message: message("message-1", "Have a look at the build"),
      replies: [
        message("reply-1", "Looked at it.", {
          author: { id: "planner", kind: "session" },
          in_reply_to: "message-1",
        }),
      ],
    } satisfies MessageRead,
  ]);
  renderCard(plannerAgentRow());

  const ended = await screen.findByTestId("agent-session-ended");
  expect(ended.textContent).toBe("Not in the live session list — it may have ended.");
  expect(document.querySelector("[data-inbox-agent-composer]")).toBeNull();
  // The thread is still there to read, whatever became of the session.
  expect(screen.getByTestId("inbox-thread-agent").textContent).toContain("Looked at it.");
});

test("a listed session that advertises no delivery mode warns above its composer", async () => {
  spy("listAgents", [{ ...session, capabilities: [] }]);
  spy("listAgentMessages", [
    { message: message("message-1", "Have a look"), replies: [] } satisfies MessageRead,
  ]);
  renderCard(plannerAgentRow({ unread_reply_ids: [] }));

  await screen.findByTestId("inbox-thread-agent");
  await waitFor(() => expect(screen.getByText("Takes no messages")).toBeTruthy());
  expect(screen.getByTestId("agent-takes-no-messages").textContent).toContain(
    "records a failed attempt"
  );
});

test("a comment thread's card draws the quote it is anchored to and the thread whole", async () => {
  spy("getIssue", { closed_at: null } as unknown as IssueDetails);
  spy("listComments", [
    comment("comment-1", "The retry budget is too small."),
    comment("comment-2", "Agreed.", {
      author: { id: "bob", kind: "user" },
      created_at: "2026-10-12T01:00:00Z",
      reply_to: "comment-1",
    }),
  ]);
  renderCard(
    commentRow({
      anchor: {
        artifact_id: "artifact-1",
        block_id: "b-1",
        mark_id: "mark-1",
        orphaned: false,
        quote: "three attempts",
        version: 2,
      },
    })
  );

  const body = await screen.findByTestId("inbox-thread-comment");
  expect(body.querySelector("[data-inbox-thread-quote]")?.textContent).toBe("three attempts");
  expect(body.textContent).toContain("The retry budget is too small.");
  expect(body.textContent).toContain("Agreed.");
  // Written after the row's read mark, so only the reply is new.
  const turns = [...body.querySelectorAll("[data-inbox-thread-turn]")];
  expect(turns[0]?.hasAttribute("data-inbox-thread-unread")).toBe(false);
  expect(turns[1]?.hasAttribute("data-inbox-thread-unread")).toBe(true);
});
