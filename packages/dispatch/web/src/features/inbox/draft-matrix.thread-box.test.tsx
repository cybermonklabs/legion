/**
 * The draft matrix, Inbox thread box row (INFRA-1503).
 *
 * Every composer host is held to one rule (`.dispatch/architecture/dispatch-web.md`, "A composer's
 * draft - typed, refused or stopped - goes where the person takes it, and stays where the host
 * moved away from"): the reader's own move with the composer's own control takes the draft with
 * it, and a move of the host's carries nothing: the text stays with the address it was written
 * for, and is there when the reader comes back to it. A refused or stopped send's note
 * stays at its own address. Each cell writes a draft in one state, typed and never sent, refused
 * by Dispatch, or stopped by the reader, makes one move, and asserts where the text ends.
 *
 * Host: the Inbox reading column's thread box (`ThreadBody`), a standing box: the thread's own
 * reply, which nothing ends, drawn afresh for each row the column moves to (`ReadingPane`), and,
 * for an Agent row, moved by a newer message of the session onto that message. The rows are the
 * comment thread's box (`CommentThreadBody`) unless they name the Agent row's
 * (`AgentThreadBody`); the issue message thread's box has a file of its own
 * (`draft-matrix.message-box.test.tsx`).
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
 * | The same, an Agent row's box (off)               | cell   | cell    | cell    |
 * | Host moves it over a refusal (elsewhere) (on)    | no (2) | cell    | cell    |
 * | Thread collapsed by the host                     | no (3) | no (3)  | no (3)  |
 * | Cancel reply                                     | no (4) | no (4)  | no (4)  |
 * | Escape in the field                              | cell   | cell    | cell    |
 * | Close, then Discard draft?                       | no (5) | no (5)  | no (5)  |
 * | Agents pick                                      | no (6) | no (6)  | no (6)  |
 * | Typed while elsewhere, then unmounted            | -      | cell    | -       |
 * | The thread's read fails or waits, then recovers  | no (7) | no (7)  | no (7)  |
 * | The thread's replies are ordered the other way   | no (8) | no (8)  | no (8)  |
 * | Its session leaves the live list, then returns   | no (9) | no (9)  | no (9)  |
 * | The thread is drawn in its document, or beside   | no (10)| no (10) | no (10) |
 * | The thread's root is no longer read              | no (11)| no (11) | no (11) |
 * | The Inbox resized, closed or left, then back     | no (12)| no (12) | no (12) |
 * | The page reloaded, then back (off)               | cell   | cell    | cell    |
 * | The same, an Agent row's box (off)               | cell   | cell    | cell    |
 *
 * (off) marks a host move that takes the box off the screen: the column draws another thread's
 * box in its place. (on) marks one that keeps the same box on screen at another address; the
 * refused or stopped draft stays in its field there, with Send saying it was written for an
 * earlier message, so no line is needed. The reload rows end the whole page, as any reload does
 * (`endAsAReload`), and load it again over the same tab: a new query client and write store over
 * the tab's `sessionStorage`, with the reader signed in as before. They are the boxes' lifetimes
 * the census finds (`lifetime.ts`).
 *
 * The numbered reasons - the moves this host cannot make, and the moves and forms the composer
 * census found that the rows above do not make - are `REASONS` in
 * `draft-matrix.thread-box.coverage.ts`, which holds each census item this file answers for to a
 * test above or to one of them.
 */
import { expect, type Mock, spyOn, test } from "bun:test";
import type { MutationKey, QueryClient } from "@tanstack/react-query";
import { act, screen, within } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";

import {
  BODY,
  drawInboxColumn,
  HELD,
  type Held,
  holdInInboxBox,
  inboxBoxField,
  inboxNote,
  inboxPane,
  newQueryClient,
  REFUSAL,
  signedInAsReader,
} from "../../__tests__/draft-matrix-fixture";
import { sentRequest, stopSend } from "../../__tests__/stop-late-write";
import { api } from "../../api/client";
import type {
  Agent,
  Comment,
  IssueDetails,
  Message,
  MessageRead,
  MyReplyRow,
  MyReplyThreadRow,
} from "../../api/types";
import type { SentRequest } from "../conversation/send-request";
import { plannerAgentRow } from "../threads/replies-fixture";
import { installInboxApiMocks } from "./inbox-fixture";
import { threadSendKey } from "./ThreadBody";

installInboxApiMocks();

const HOST = "Inbox thread box";

/** What an Agent row's box says once a newer message has moved it off its refused reply. */
const MOVED_ON =
  "This message was written as a reply to an earlier message, and this box now replies to a " +
  "newer one. Discard it to write a reply here.";

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

/** A reply of `BODY` from the first comment thread's box, and from the Planner thread's box. */
const commentReply = sentRequest(BODY, {
  actor: bob,
  excerpt: "",
  id: "comment-1",
  parentKind: "comment",
});
const agentReply = sentRequest(
  BODY,
  {
    actor: { id: "planner", kind: "session" },
    excerpt: "Answer reply-1",
    id: "reply-1",
    parentKind: "message",
    thread: { delivery: "steer", target: "session:planner" },
  },
  { owner: { kind: "session", sessionId: "planner" } }
);

interface Column {
  readonly client: QueryClient;
  readonly createAgentMessage: Mock<typeof api.createAgentMessage>;
  readonly createComment: Mock<typeof api.createComment>;
  readonly listAgentMessages: Mock<typeof api.listAgentMessages>;
  /** The column moved to another row, as the Inbox moves it: the same column, drawn again. */
  readonly move: (row: MyReplyRow) => void;
  /** Ends the page as a reload does and loads it again over the same tab on `row`, signed in as
   *  the same reader, with a new query client (`endAsAReload`, `signedInAsReader`). */
  readonly reload: (row: MyReplyRow) => void;
  readonly restore: () => void;
  readonly user: UserEvent;
}

/** A query client signed in as the reader, holding the two comment threads' roots. */
function commentsClient(): QueryClient {
  const client = signedInAsReader(newQueryClient());
  client.setQueryData(["comments", "CORE-1"], [rootComment("comment-1", "CORE-1")]);
  client.setQueryData(["comments", "CORE-2"], [rootComment("comment-9", "CORE-2")]);
  return client;
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
  const client = commentsClient();
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
    await stopSend(client, stopped.key, stopped.request, [createComment, createAgentMessage]);
  }
  createComment.mockRejectedValue(REFUSAL);
  createAgentMessage.mockRejectedValue(REFUSAL);
  const column = drawInboxColumn(client, row, commentsClient);
  return {
    get client() {
      return column.client;
    },
    createAgentMessage,
    createComment,
    listAgentMessages,
    move: column.move,
    reload: column.reload,
    restore: () => {
      column.unmount();
      createComment.mockRestore();
      createAgentMessage.mockRestore();
      listAgentMessages.mockRestore();
      for (const read of reads) read.mockRestore();
    },
    user,
  };
}

const stoppedComment = (held: Held) =>
  held === "stopped" ? { key: threadSendKey("comment-1"), request: commentReply } : undefined;

// The column moving to another row is the host's move, and it takes the box off the screen: the
// box's draft waits under its send name, and the box drawn when the column comes back shows it.
for (const held of HELD) {
  test(`${HOST} · the column moves to another row, then back · ${held}`, async () => {
    const column = await openColumn(firstRow, stoppedComment(held));
    try {
      await holdInInboxBox(column.user, held);
      column.move(otherRow);
      const other = inboxPane().querySelector('[data-inbox-thread="comment-9"]') !== null;
      column.move(firstRow);
      expect({ back: { note: inboxNote(), text: inboxBoxField("Reply").value }, other }).toEqual({
        back: { note: held === "typed" ? "none" : held, text: BODY },
        other: true,
      });
    } finally {
      column.restore();
    }
  });
}

const stoppedAgent = (held: Held) =>
  held === "stopped" ? { key: threadSendKey("message-1"), request: agentReply } : undefined;

// An Agent row's box is drawn afresh for each row too, keyed by its thread, so the column moving
// to a comment thread and back is the same host's move: the box's draft waits under its send name,
// and the box drawn when the column comes back shows it, and no other thread's box does.
for (const held of HELD) {
  test(`${HOST} · the column moves to another row, then back · an Agent row's box · ${held}`, async () => {
    const column = await openColumn(plannerAgentRow(), stoppedAgent(held));
    try {
      await holdInInboxBox(column.user, held, "Comment");
      column.move(firstRow);
      const other = (
        await within(inboxPane()).findByRole<HTMLTextAreaElement>("textbox", { name: "Reply" })
      ).value;
      column.move(plannerAgentRow());
      const field = await within(inboxPane()).findByRole<HTMLTextAreaElement>("textbox", {
        name: "Comment",
      });
      expect({ back: { note: inboxNote(), text: field.value }, other }).toEqual({
        back: { note: held === "typed" ? "none" : held, text: BODY },
        other: "",
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
  await within(inboxPane()).findByText(MOVED_ON);
}

// A newer message moves an Agent row's box onto it: the host's move, which carries nothing. The
// refused or stopped reply stays held for the message it answered, with its note and no Retry
// from the newer address; the box says what it can do there.
for (const held of ["refused", "stopped"] as const) {
  test(`${HOST} · host moves it over a refusal (elsewhere) · ${held}`, async () => {
    const column = await openColumn(
      plannerAgentRow(),
      held === "stopped" ? { key: threadSendKey("message-1"), request: agentReply } : undefined
    );
    try {
      await holdInInboxBox(column.user, held, "Comment");
      await newerAnswer(column);
      expect({
        note: inboxNote(),
        retry: within(inboxPane()).queryAllByRole("button", { name: "Retry" }).length,
        text: inboxBoxField("Comment").value,
      }).toEqual({ note: held, retry: 0, text: BODY });
    } finally {
      column.restore();
    }
  });
}

// While a newer message has the box away from its refused reply, nothing typed is written into
// the refusal's draft: what the reader types there is the newer message's, and it is kept under
// the box's send name as it is typed (`unsentName` in `useKeptDrafts`). The move takes the box off
// the screen, so the cell checks the return alone: coming back to the thread finds it, and the
// refusal comes back with the box as well.
test(`${HOST} · typed while a refusal is held elsewhere, then the composer unmounts · refused`, async () => {
  const column = await openColumn(plannerAgentRow());
  const typed = "Reply meant for the newer answer";
  try {
    await holdInInboxBox(column.user, "refused", "Comment");
    await newerAnswer(column);
    await column.user.click(inboxBoxField("Comment"));
    await column.user.keyboard("{Control>}a{/Control}");
    await column.user.paste(typed);
    column.move(firstRow);
    await within(inboxPane()).findByRole("textbox", { name: "Reply" });
    column.move(plannerAgentRow());
    await within(inboxPane()).findByRole("textbox", { name: "Comment" });
    const back = screen
      .getAllByRole<HTMLTextAreaElement>("textbox")
      .some((box) => box.value.includes(typed));
    expect({ back, refusal: inboxNote() }).toEqual({ back: true, refusal: "refused" });
  } finally {
    column.restore();
  }
});

// A standing box's Escape ends nothing: the draft, and a note with it, stay in the box.
for (const held of HELD) {
  test(`${HOST} · Escape in the field · ${held}`, async () => {
    const column = await openColumn(firstRow, stoppedComment(held));
    try {
      await holdInInboxBox(column.user, held);
      await column.user.click(inboxBoxField("Reply"));
      await column.user.keyboard("{Escape}");
      expect({ note: inboxNote(), text: inboxBoxField("Reply").value }).toEqual({
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
    await holdInInboxBox(column.user, "typed");
    expect(within(inboxPane()).queryAllByRole("button", { name: "Cancel reply" }).length).toBe(0);
  } finally {
    column.restore();
  }
});

test(`${HOST} · Close, then Discard draft? · cannot make: the box draws no Close`, async () => {
  const column = await openColumn(firstRow);
  try {
    await holdInInboxBox(column.user, "typed");
    expect(within(inboxPane()).queryAllByRole("button", { name: "Close" }).length).toBe(0);
  } finally {
    column.restore();
  }
});

test(`${HOST} · Agents pick · cannot make: an Agent row's box draws no issue picker`, async () => {
  const column = await openColumn(plannerAgentRow());
  try {
    await holdInInboxBox(column.user, "typed", "Comment");
    expect(within(inboxPane()).queryAllByRole("button", { name: "Choose issue" }).length).toBe(0);
  } finally {
    column.restore();
  }
});

// A reload ends the whole page and loads it again over the same tab (`endAsAReload`), with the
// reader signed in as before. The column drawn on the thread again, the way back to the box,
// shows the reply the reader wrote there, with its note: in a comment thread's box, and in an
// Agent row's.
for (const held of HELD) {
  test(`${HOST} · the page reloaded, then back · ${held}`, async () => {
    const column = await openColumn(firstRow, stoppedComment(held));
    try {
      await holdInInboxBox(column.user, held);
      column.reload(firstRow);
      const field = await within(
        await screen.findByTestId("inbox-pane")
      ).findByRole<HTMLTextAreaElement>("textbox", { name: "Reply" });
      expect({ note: inboxNote(), text: field.value }).toEqual({
        note: held === "typed" ? "none" : held,
        text: BODY,
      });
    } finally {
      column.restore();
    }
  });
}

for (const held of HELD) {
  test(`${HOST} · the page reloaded, then back · an Agent row's box · ${held}`, async () => {
    const column = await openColumn(plannerAgentRow(), stoppedAgent(held));
    try {
      await holdInInboxBox(column.user, held, "Comment");
      column.reload(plannerAgentRow());
      const field = await within(
        await screen.findByTestId("inbox-pane")
      ).findByRole<HTMLTextAreaElement>("textbox", { name: "Comment" });
      expect({ note: inboxNote(), text: field.value }).toEqual({
        note: held === "typed" ? "none" : held,
        text: BODY,
      });
    } finally {
      column.restore();
    }
  });
}
