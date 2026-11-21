import { afterEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { ApiError, api } from "../../api/client";
import type {
  Agent,
  Comment,
  IssueDetails,
  Message,
  MessageRead,
  MyReplyRow,
  MyReplyThreadRow,
} from "../../api/types";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { agentRow, plannerAgentRow } from "../threads/replies-fixture";
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

function renderCard(thread: MyReplyRow): QueryClient {
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
  return client;
}

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
  // The reply is read as soon as the ended note shows, and only a loaded engine has drawn it by
  // then (`MarkdownBody`): the first body of a file takes the asynchronous path.
  await warmMarkdownRenderer();
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

// A list that holds only capabilities that are no delivery mode takes no message either, and the
// warning says that is why, not that the plugin cannot reach the conversation.
test("a listed session whose capabilities hold no delivery mode warns the same way", async () => {
  spy("listAgents", [{ ...session, capabilities: ["future-capability"] }]);
  spy("listAgentMessages", [
    { message: message("message-1", "Have a look"), replies: [] } satisfies MessageRead,
  ]);
  renderCard(plannerAgentRow({ unread_reply_ids: [] }));

  await screen.findByTestId("inbox-thread-agent");
  await waitFor(() => expect(screen.getByText("Takes no messages")).toBeTruthy());
  expect(screen.getByTestId("agent-takes-no-messages").textContent).toBe(
    "This session advertises no delivery mode, so a message sent here records a failed attempt. None of the capabilities it lists is a delivery mode this Dispatch knows."
  );
});

test("a comment thread's card draws the quote it is anchored to and the thread whole", async () => {
  // The thread is read in the commit that mounts its card, which only a loaded engine draws
  // (`MarkdownBody`): the first body of a file takes the asynchronous path.
  await warmMarkdownRenderer();
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

// A comment the reviewer rewrote shows its original with its badge, or with `w` while it holds
// the focus, in the Inbox's thread view as on the issue's page.
test("w on a rewritten comment of a thread shows the agent's wording in place, and w again the rewrite", async () => {
  await warmMarkdownRenderer();
  const user = userEvent.setup();
  spy("getIssue", { closed_at: null } as unknown as IssueDetails);
  spy("listComments", [
    comment("comment-1", "The retry budget is too small.", {
      rewrite: {
        at: "2026-10-12T00:00:00Z",
        by: "reviewer",
        original: { body: "retry budg 2 small imo" },
        state: "rewritten",
      },
    }),
  ]);
  renderCard(commentRow());

  const body = await screen.findByTestId("inbox-thread-comment");
  const words = await within(body).findByText("The retry budget is too small.");
  expect(within(body).getByRole("button", { name: "Rewritten" })).toHaveAttribute(
    "title",
    "Show the original wording (w)"
  );
  await user.click(words);
  await user.keyboard("w");
  expect(await within(body).findByText("retry budg 2 small imo")).toBeTruthy();
  await user.keyboard("w");
  expect(await within(body).findByText("The retry budget is too small.")).toBeTruthy();
});

// A thread box in the Inbox stands for its thread: its reply is the box's own address, which
// nothing ends, so it draws no Cancel reply. Pressed, that control would do nothing and leave the
// draft in the field (MentionComposer's `standing`).
test("an Inbox thread box, comment or Agent, draws no Cancel reply", async () => {
  spy("getIssue", { closed_at: null } as unknown as IssueDetails);
  spy("listComments", [comment("comment-1", "The retry budget is too small.")]);
  renderCard(commentRow());
  const thread = await screen.findByTestId("inbox-thread-comment");
  await waitFor(() =>
    expect(within(thread).queryAllByRole("form", { name: "Comment composer" }).length).toBe(1)
  );
  expect(within(thread).queryAllByRole("button", { name: "Cancel reply" }).length).toBe(0);
  cleanup();

  spy("listAgents", [session]);
  spy("listAgentMessages", [
    {
      message: message("message-1", "Have a look at the build"),
      replies: [],
    } satisfies MessageRead,
  ]);
  renderCard(plannerAgentRow());
  const agent = await screen.findByTestId("inbox-thread-agent");
  await waitFor(() => expect(agent.querySelector("[data-inbox-agent-composer]")).not.toBeNull());
  expect(within(agent).queryAllByRole("button", { name: "Cancel reply" }).length).toBe(0);
});

/** A card for each of `threads`, as the Inbox lists them below 1280 px; the setter draws the list
 *  again with the threads it is handed, over the same client. */
function renderCards(threads: readonly MyReplyRow[]): (threads: readonly MyReplyRow[]) => void {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  const tree = (rows: readonly MyReplyRow[]): ReactNode => (
    <MemoryRouter>
      <QueryClientProvider client={client}>
        {rows.map((row) => (
          <ThreadRowCard key={row.thread_id} thread={row} />
        ))}
      </QueryClientProvider>
    </MemoryRouter>
  );
  const view = render(tree(threads));
  return (next) => view.rerender(tree(next));
}

/** The card drawing thread `id`. */
function cardOf(id: string): HTMLElement {
  const found = document
    .querySelector(`[data-inbox-thread="${id}"]`)
    ?.closest<HTMLElement>("[data-inbox-thread-card]");
  if (found === null || found === undefined) throw new Error(`no card for ${id}`);
  return found;
}

// Two cards can be two threads of one session, each with a reply box of its own. A reply Dispatch
// refused in one is that thread's: a card for the other thread mounts with none of it - no draft,
// no refusal, no pending turn, no Retry that would send it as a reply there - and the refused
// card's Retry replies under its own thread's newest message.
test("an Agent card shows nothing of a reply refused in another card of its session", async () => {
  spy("listAgents", [session]);
  spy(
    "listAgentMessages",
    ["1", "2"].map(
      (n) =>
        ({
          message: message(`message-${n}`, `Question ${n}`),
          replies: [
            message(`reply-${n}`, `Answer ${n}`, {
              author: { id: "planner", kind: "session" },
              created_at: "2026-10-12T01:00:00Z",
              in_reply_to: `message-${n}`,
            }),
          ],
        }) satisfies MessageRead
    )
  );
  const createAgentMessage = spyOn(api, "createAgentMessage")
    .mockRejectedValueOnce(
      new ApiError(400, { code: "MESSAGE_INPUT", error: "Dispatch refused the message" })
    )
    .mockResolvedValue({} as never);
  restores.push(createAgentMessage);
  const first = plannerAgentRow();
  const second = agentRow("message-2", {
    ...first,
    reply: { ...first.reply, id: "reply-2" },
    thread_id: "message-2",
    unread_reply_ids: ["reply-2"],
  });
  const show = renderCards([first]);
  const user = userEvent.setup();

  await waitFor(() => expect(cardOf("message-1").querySelector("textarea")).not.toBeNull());
  const refusedCard = within(cardOf("message-1"));
  await user.click(refusedCard.getByRole("textbox", { name: "Comment" }));
  await user.paste("Reply meant for the first");
  await user.click(refusedCard.getByRole("button", { name: "Send" }));
  await refusedCard.findByText("Couldn't send — Dispatch refused the message");

  show([first, second]);
  const other = within(cardOf("message-2"));
  expect(other.getByRole<HTMLTextAreaElement>("textbox", { name: "Comment" }).value).toBe("");
  expect(other.queryByRole("alert")).toBeNull();
  expect(other.queryByTestId("agent-send-held")).toBeNull();

  await user.click(refusedCard.getByRole("button", { name: "Retry" }));
  await waitFor(() => expect(createAgentMessage).toHaveBeenCalledTimes(2));
  expect(
    createAgentMessage.mock.calls.map(([sessionId, input]) => [sessionId, input.in_reply_to])
  ).toEqual([
    ["planner", "reply-1"],
    ["planner", "reply-1"],
  ]);
});

// An Agent card's box replies under its thread's newest message, which no press of the reader's
// moves. A message that comes while the box holds a refused reply moves the box onto it, so the
// refusal has no Retry there, and the box says what it can do: Discard, to write a reply to the
// newer message - never "reply to that message again", which it offers no way to do.
test("an Agent card whose thread moves on under a refused reply says what its box can do", async () => {
  spy("listAgents", [session]);
  const answer = (id: string, body: string, at: string) =>
    message(id, body, {
      author: { id: "planner", kind: "session" },
      created_at: at,
      in_reply_to: "message-1",
    });
  const thread = (replies: Message[]): MessageRead[] => [
    { message: message("message-1", "Have a look at the build"), replies },
  ];
  const first = answer("reply-1", "Looked at it.", "2026-10-12T01:00:00Z");
  const list = spyOn(api, "listAgentMessages").mockResolvedValue(thread([first]));
  restores.push(list);
  const createAgentMessage = spyOn(api, "createAgentMessage").mockRejectedValue(
    new ApiError(400, { code: "MESSAGE_INPUT", error: "Dispatch refused the message" })
  );
  restores.push(createAgentMessage);
  const client = renderCard(plannerAgentRow());
  const user = userEvent.setup();

  await user.click(await screen.findByRole("textbox", { name: "Comment" }));
  await user.paste("Reply to the first answer");
  await user.click(screen.getByRole("button", { name: "Send" }));
  await screen.findByText("Couldn't send — Dispatch refused the message");

  list.mockResolvedValue(
    thread([first, answer("reply-2", "And one more thing.", "2026-10-12T02:00:00Z")])
  );
  await act(() => client.invalidateQueries());
  expect(
    await screen.findByText(
      "This message was written as a reply to an earlier message, and this box now replies to a newer one. Discard it to write a reply here."
    )
  ).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Retry" })).toBeNull();
  expect(screen.getByRole("button", { name: "Discard draft" })).toBeTruthy();
  expect(createAgentMessage).toHaveBeenCalledTimes(1);
});
