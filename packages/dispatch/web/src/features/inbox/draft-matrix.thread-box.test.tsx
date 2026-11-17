/**
 * The draft matrix, Inbox thread box row (INFRA-1503).
 *
 * Every composer host is held to one rule (packages/dispatch/AGENTS.md, "Who moved the
 * composer's address"): the reader's own move with the composer's own control takes the draft
 * with it, and a move of the host's carries nothing, so the text stays with the address it was
 * written for and is there when the reader comes back to it (AGENTS.md:116-119). A refused or
 * stopped send's note stays at its own address. Each cell writes a draft in one state, typed and
 * never sent, refused by Dispatch, or stopped by the reader, makes one move, and asserts where
 * the text ends. A cell that fails at the code it was written against is a hole in the rule, kept
 * red on purpose until the code is fixed.
 *
 * Host: the Inbox reading column's thread box (`ThreadBody`), a standing box: the thread's own
 * reply, which nothing ends, drawn afresh for each row the column moves to (`ReadingPane`), and,
 * for an Agent row, moved by a newer message of the session onto that message.
 *
 * A host move that takes the composer off the screen is checked on return alone, where the field
 * holds the draft with its refusal or stopped note; a host move that keeps the same composer on
 * screen at another address is checked on return too and, while away, for a line on screen that
 * says the earlier text is kept and names its message's author or words.
 *
 * | Move                                             | typed  | refused | stopped |
 * | ------------------------------------------------ | ------ | ------- | ------- |
 * | Back, then back                                  | no (1) | no (1)  | no (1)  |
 * | The column moves to another row, then back (off) | cell   | cell    | cell    |
 * | Host moves it over a refusal (elsewhere) (on)    | no (2) | cell    | cell    |
 * | Thread collapsed by the host                     | no (3) | no (3)  | no (3)  |
 * | Cancel reply                                     | no (4) | no (4)  | no (4)  |
 * | Escape in the field                              | cell   | cell    | cell    |
 * | Close, then Discard draft?                       | no (5) | no (5)  | no (5)  |
 * | Agents pick                                      | no (6) | no (6)  | no (6)  |
 * | Typed while elsewhere, then unmounted            | -      | cell    | -       |
 *
 * (off) marks a host move that takes the box off the screen: the column draws another thread's
 * box in its place. (on) marks one that keeps the same box on screen at another address; the
 * refused or stopped draft stays in its field there, with Send saying it was written for an
 * earlier message, so no line is needed.
 *
 * The moves this host cannot make:
 * 1. The box draws no Back of its own; the Inbox's phone Back closes the column, which unmounts
 *    the box as the move to another row does.
 * 2. Elsewhere needs a held refusal; the typed version is the unmount row.
 * 3. The column draws no collapse of the thread; leaving it is the move to another row.
 * 4. A standing box's reply is its own address, so it draws no Cancel reply (asserted absent).
 * 5. The box has no anchor, so it draws no Close (asserted absent below).
 * 6. An Agent row's box answers its thread, so it draws no issue picker (asserted absent below).
 */
import { expect, jest, type Mock, spyOn, test } from "bun:test";
import { type MutationKey, QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

import { fakeDocumentRuntime } from "../../__tests__/document-runtime";
import { realClock } from "../../__tests__/fake-clock";
import { stopTheLateWrite } from "../../__tests__/stop-late-write";
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
import { heldSends } from "../conversation/held-sends";
import type { SentRequest } from "../conversation/send-request";
import { DocumentRuntime } from "../doc/runtime";
import { MarginProvider } from "../margin/margin-context";
import { plannerAgentRow } from "../threads/replies-fixture";
import { SendStoppedError } from "../writes/keyed-write";
import { installInboxApiMocks } from "./inbox-fixture";
import { ReadingPane } from "./ReadingPane";

installInboxApiMocks();

const HOST = "Inbox thread box";

/** Dispatch refusing the send outright, as a 400 does: no attempt follows under its key. */
const REFUSAL = new ApiError(400, { code: "INPUT_REFUSED", error: "Dispatch refused the message" });

/** What each note says in the box: the refusal, and a stopped send at its own address. */
const REFUSED_NOTE = "Couldn't send — Dispatch refused the message";
const STOPPED_NOTE = new SendStoppedError().message;

/** What an Agent row's box says once a newer message has moved it off its refused reply. */
const MOVED_ON =
  "This message was written as a reply to an earlier message, and this box now replies to a " +
  "newer one. Discard it to write a reply here.";

const BODY = "Draft the reader wrote";

type Held = "typed" | "refused" | "stopped";
const HELD: readonly Held[] = ["typed", "refused", "stopped"];

const bob = { id: "bob", kind: "user" } as const;

function rootComment(id: string, issueKey: string): Comment {
  return {
    anchor: null,
    answers: null,
    artifact_id: null,
    ask_id: null,
    author: bob,
    body: "The retry budget is too small.",
    created_at: "2026-10-12T00:00:00Z",
    deliveries: [],
    edited_at: null,
    id,
    issue_key: issueKey,
    mentions: [],
    reply_block: null,
    reply_to: null,
    resolved: false,
    resolved_at: null,
    resolved_by: null,
    suggestion: null,
    turn: null,
  };
}

function commentRow(overrides: Partial<MyReplyThreadRow>): MyReplyThreadRow {
  return {
    issue: { key: "CORE-1", title: "Fix the thing" },
    kind: "comment",
    read_through: "2026-10-12T00:30:00Z",
    ref: "/issues/CORE-1?comment=comment-1",
    reply: {
      author: bob,
      created_at: "2026-10-12T01:00:00Z",
      first_line: "Agreed.",
      id: "comment-2",
      kind: "reply",
    },
    subject: "The retry budget",
    thread_id: "comment-1",
    unread: false,
    unread_count: 0,
    version: 1,
    ...overrides,
  };
}

/** The comment thread the cells write in, and the one the column moves to. */
const firstRow = commentRow({});
const otherRow = commentRow({
  issue: { key: "CORE-2", title: "Other issue" },
  ref: "/issues/CORE-2?comment=comment-9",
  thread_id: "comment-9",
});

/** The Planner session, live, which takes a Send. */
const planner: Agent = {
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

function plannerMessage(id: string, body: string, overrides: Partial<Message> = {}): Message {
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

/** The Planner's answers to the reader's message-1, newest last. */
function plannerThread(replies: readonly string[]): MessageRead[] {
  return [
    {
      message: plannerMessage("message-1", "Have a look at the build"),
      replies: replies.map((id, index) =>
        plannerMessage(id, `Answer ${id}`, {
          author: { id: "planner", kind: "session" },
          created_at: `2026-10-12T0${index + 1}:00:00Z`,
          in_reply_to: "message-1",
        })
      ),
    },
  ];
}

/** The send name of a thread's box (`threadSendKey` in `ThreadBody.tsx`). */
const boxKey = (threadId: string): MutationKey => ["inbox-thread-reply", threadId];

/** A reply of `BODY` from the first comment thread's box, and from the Planner thread's box. */
const commentReply: SentRequest = {
  anchor: undefined,
  ask: { multiple: false, options: [], urgency: "med" },
  draft: { body: BODY, mentions: [], replacement: "" },
  edit: undefined,
  kind: "comment",
  owner: { issueKey: "CORE-1", kind: "issue" },
  plainDelivery: "steer",
  replyTo: { actor: bob, excerpt: "", id: "comment-1", parentKind: "comment" },
  sendKey: "stopped-send",
};
const agentReply: SentRequest = {
  ...commentReply,
  owner: { kind: "session", sessionId: "planner" },
  replyTo: {
    actor: { id: "planner", kind: "session" },
    excerpt: "Answer reply-1",
    id: "reply-1",
    parentKind: "message",
    thread: { delivery: "steer", target: "session:planner" },
  },
};

interface Column {
  readonly client: QueryClient;
  readonly createAgentMessage: Mock<typeof api.createAgentMessage>;
  readonly createComment: Mock<typeof api.createComment>;
  readonly listAgentMessages: Mock<typeof api.listAgentMessages>;
  /** The column moved to another row, as the Inbox moves it: the same column, drawn again. */
  readonly move: (row: MyReplyRow) => void;
  readonly restore: () => void;
  readonly user: UserEvent;
}

/**
 * The Inbox reading column on `row`, over the two comment threads and the Planner's thread.
 * Every write is refused, so a send pressed here comes back refused; `stopped`, when given, is
 * sent through the held-send store under its name before the column is drawn and stopped once
 * late, as the reader's Stop trying does, so the box mounts showing it.
 */
async function openColumn(
  row: MyReplyRow,
  stopped?: { readonly key: MutationKey; readonly request: SentRequest }
): Promise<Column> {
  const user = userEvent.setup();
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  client.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  client.setQueryData(["comments", "CORE-1"], [rootComment("comment-1", "CORE-1")]);
  client.setQueryData(["comments", "CORE-2"], [rootComment("comment-9", "CORE-2")]);
  const reads = [
    spyOn(api, "getArtifact").mockRejectedValue(new Error("no document seeded")),
    spyOn(api, "getArtifactText").mockResolvedValue({ markdown: "", token: "", version: null }),
    // The column reads one field of the issue: whether it is closed.
    spyOn(api, "getIssue").mockResolvedValue({ closed_at: null } as unknown as IssueDetails),
    // A newer answer is fetched by invalidating every read, the comments too, which a box drawn
    // after it reads again.
    spyOn(api, "listComments").mockImplementation(async (issueKey) => [
      rootComment(issueKey === "CORE-1" ? "comment-1" : "comment-9", issueKey),
    ]),
    spyOn(api, "listAgents").mockResolvedValue([planner]),
  ];
  const listAgentMessages = spyOn(api, "listAgentMessages").mockResolvedValue(
    plannerThread(["reply-1"])
  );
  const createComment = spyOn(api, "createComment");
  const createAgentMessage = spyOn(api, "createAgentMessage");
  if (stopped !== undefined) {
    const down = new ApiError(503, { code: "UNAVAILABLE", error: "the server is down" });
    createComment.mockRejectedValue(down);
    createAgentMessage.mockRejectedValue(down);
    jest.useFakeTimers();
    try {
      act(() => {
        heldSends(client).send(stopped.key, stopped.request);
      });
      await stopTheLateWrite(client);
    } finally {
      realClock();
    }
  }
  createComment.mockRejectedValue(REFUSAL);
  createAgentMessage.mockRejectedValue(REFUSAL);
  const fake = fakeDocumentRuntime({ text: "The live document" });
  const tree = (shown: MyReplyRow) => (
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <DocumentRuntime.Provider value={fake.runtime}>
          <MarginProvider>
            <ReadingPane
              onAnswered={() => {}}
              onSnoozeLive={() => {}}
              row={{ kind: "thread", thread: shown }}
              viewer="alice"
            />
          </MarginProvider>
        </DocumentRuntime.Provider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  const view = render(tree(row));
  return {
    client,
    createAgentMessage,
    createComment,
    listAgentMessages,
    move: (shown) => view.rerender(tree(shown)),
    restore: () => {
      view.unmount();
      createComment.mockRestore();
      createAgentMessage.mockRestore();
      listAgentMessages.mockRestore();
      for (const read of reads) read.mockRestore();
    },
    user,
  };
}

const pane = () => screen.getByTestId("inbox-pane");

/** The box's field: `Reply` in a comment thread, `Comment` in an Agent row's thread. */
const boxField = (name: "Reply" | "Comment") =>
  within(pane()).getByRole<HTMLTextAreaElement>("textbox", { name });

/** Which note the column shows about a send its box holds. */
function note(): Held | "none" {
  const shown = within(pane());
  if (shown.queryByText(REFUSED_NOTE) !== null) return "refused";
  return shown.queryByText(STOPPED_NOTE) === null ? "none" : "stopped";
}

/** Puts `BODY` in the box of the box's field `name`, in the state `held` names; a stopped reply
 *  is the one `openColumn` seeded. */
async function holdInBox(column: Column, name: "Reply" | "Comment", held: Held): Promise<void> {
  const field = await within(
    await screen.findByTestId("inbox-pane")
  ).findByRole<HTMLTextAreaElement>("textbox", { name });
  if (held === "stopped") {
    await waitFor(() => expect(note()).toBe("stopped"));
    return;
  }
  await column.user.click(field);
  await column.user.keyboard("{Control>}a{/Control}");
  await column.user.paste(BODY);
  if (held === "refused") {
    await column.user.click(within(pane()).getByRole("button", { name: "Send" }));
    await within(pane()).findByText(REFUSED_NOTE);
  }
}

const stoppedComment = (held: Held) =>
  held === "stopped" ? { key: boxKey("comment-1"), request: commentReply } : undefined;

// The column moving to another row is the host's move, and it takes the box off the screen: the
// box's draft waits under its send name, and the box drawn when the column comes back shows it.
for (const held of HELD) {
  test(`${HOST} · the column moves to another row, then back · ${held}`, async () => {
    const column = await openColumn(firstRow, stoppedComment(held));
    try {
      await holdInBox(column, "Reply", held);
      column.move(otherRow);
      const other = pane().querySelector('[data-inbox-thread="comment-9"]') !== null;
      column.move(firstRow);
      expect({ back: { note: note(), text: boxField("Reply").value }, other }).toEqual({
        back: { note: held === "typed" ? "none" : held, text: BODY },
        other: true,
      });
    } finally {
      column.restore();
    }
  });
}

/** The Planner's thread moved on: a newer answer has come since the box's reply was refused. */
async function newerAnswer(column: Column): Promise<void> {
  column.listAgentMessages.mockResolvedValue(plannerThread(["reply-1", "reply-2"]));
  await act(() => column.client.invalidateQueries());
  await within(pane()).findByText(MOVED_ON);
}

// A newer message moves an Agent row's box onto it: the host's move, which carries nothing. The
// refused or stopped reply stays held for the message it answered, with its note and no Retry
// from the newer address; the box says what it can do there.
for (const held of ["refused", "stopped"] as const) {
  test(`${HOST} · host moves it over a refusal (elsewhere) · ${held}`, async () => {
    const column = await openColumn(
      plannerAgentRow(),
      held === "stopped" ? { key: boxKey("message-1"), request: agentReply } : undefined
    );
    try {
      await holdInBox(column, "Comment", held);
      await newerAnswer(column);
      expect({
        note: note(),
        retry: within(pane()).queryAllByRole("button", { name: "Retry" }).length,
        text: boxField("Comment").value,
      }).toEqual({ note: held, retry: 0, text: BODY });
    } finally {
      column.restore();
    }
  });
}

// Review hole (b): while the box is elsewhere nothing typed is written into the refusal's draft
// (`keepDraft` returns early), and the unsent draft is not written while a send holds the box, so
// what the reader types for the newer message is kept nowhere once the column moves on and the
// box unmounts. The move takes the box off the screen, so the cell checks the return alone:
// coming back to the thread should find it.
test(`${HOST} · typed while a refusal is held elsewhere, then the composer unmounts · refused · kept nowhere`, async () => {
  const column = await openColumn(plannerAgentRow());
  const typed = "Reply meant for the newer answer";
  try {
    await holdInBox(column, "Comment", "refused");
    await newerAnswer(column);
    await column.user.click(boxField("Comment"));
    await column.user.keyboard("{Control>}a{/Control}");
    await column.user.paste(typed);
    column.move(firstRow);
    await within(pane()).findByRole("textbox", { name: "Reply" });
    column.move(plannerAgentRow());
    await within(pane()).findByRole("textbox", { name: "Comment" });
    const back = screen
      .getAllByRole<HTMLTextAreaElement>("textbox")
      .some((box) => box.value.includes(typed));
    // The refusal comes back with the box, so what is missing is the typed text alone.
    expect({ back, refusal: note() }).toEqual({ back: true, refusal: "refused" });
  } finally {
    column.restore();
  }
});

// A standing box's Escape ends nothing: the draft, and a note with it, stay in the box.
for (const held of HELD) {
  test(`${HOST} · Escape in the field · ${held}`, async () => {
    const column = await openColumn(firstRow, stoppedComment(held));
    try {
      await holdInBox(column, "Reply", held);
      await column.user.click(boxField("Reply"));
      await column.user.keyboard("{Escape}");
      expect({ note: note(), text: boxField("Reply").value }).toEqual({
        note: held === "typed" ? "none" : held,
        text: BODY,
      });
    } finally {
      column.restore();
    }
  });
}

test(`${HOST} · Cancel reply · cannot make: a standing box draws no Cancel reply`, async () => {
  const column = await openColumn(firstRow);
  try {
    await holdInBox(column, "Reply", "typed");
    expect(within(pane()).queryAllByRole("button", { name: "Cancel reply" }).length).toBe(0);
  } finally {
    column.restore();
  }
});

test(`${HOST} · Close, then Discard draft? · cannot make: the box draws no Close`, async () => {
  const column = await openColumn(firstRow);
  try {
    await holdInBox(column, "Reply", "typed");
    expect(within(pane()).queryAllByRole("button", { name: "Close" }).length).toBe(0);
  } finally {
    column.restore();
  }
});

test(`${HOST} · Agents pick · cannot make: an Agent row's box draws no issue picker`, async () => {
  const column = await openColumn(plannerAgentRow());
  try {
    await holdInBox(column, "Comment", "typed");
    expect(within(pane()).queryAllByRole("button", { name: "Choose issue" }).length).toBe(0);
  } finally {
    column.restore();
  }
});
