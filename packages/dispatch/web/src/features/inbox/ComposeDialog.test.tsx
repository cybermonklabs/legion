import { afterEach, beforeAll, expect, jest, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

import { commentDeliveryFields } from "../../__tests__/comment-fixture";
import { jump, realClock, tick } from "../../__tests__/fake-clock";
import { afterFrameAndTasks } from "../../__tests__/frames";
import { issue } from "../../__tests__/issue-picks";
import { pickNamed } from "../../__tests__/list-picks";
import { userOnFakeClock } from "../../__tests__/user";
import { ApiError, api } from "../../api/client";
import type {
  Agent,
  BroadcastCreated,
  BroadcastRead,
  Comment,
  Message,
  MessageDelivery,
  MessageRead,
} from "../../api/types";
import { KEYED_SEND_LATE_TEXT } from "../conversation/send-request";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { KeymapProvider } from "../shell/KeymapProvider";
import { SendStoppedError } from "../writes/keyed-write";
import { resetWriteStorage } from "../writes/write-storage";
import { ComposeDialog } from "./ComposeDialog";

/**
 * The Inbox's compose dialog, the rows of its state table no browser row reads: a refused send,
 * the focus on Done after a broadcast's answer, the follow-up read redrawing an outcome, a
 * follow-up read that fails, and the write deadline. And what it keeps of the message: across a
 * change of recipients over a refused send, while a send is out, under a broadcast's key, and as
 * the dialog closes. Everything here is made up: no row names a real person, host or session.
 */

beforeAll(async () => {
  await warmMarkdownRenderer();
});

afterEach(() => {
  resetWriteStorage();
});

const planner = {
  capabilities: ["aside", "btw", "steer"],
  dir: "/workspaces/planner",
  host: "build-host",
  last_activity: new Date().toISOString(),
  last_seen: Date.now(),
  machine_id: "build-host",
  open_asks: 0,
  roles: [],
  session_id: "planner-session",
  state: "running",
  title: "Planner",
} satisfies Agent;

const reviewer = {
  ...planner,
  dir: "/workspaces/reviewer",
  session_id: "reviewer-session",
  title: "Reviewer",
} satisfies Agent;

/** One delivery attempt of a message to `session`. */
function attempt(
  session: string,
  state: MessageDelivery["state"],
  error: string | null = null
): MessageDelivery {
  return {
    accepted_as: null,
    accepted_at: null,
    attempt: 1,
    created_at: new Date().toISOString(),
    delivery: "steer",
    duplicate: false,
    envelope_id: state === "pending" ? null : "envelope-1",
    error,
    message_id: `message-${session}`,
    reply_id: null,
    requested_by: null,
    session_id: session,
    state,
  };
}

/** A message to `session`, with its attempts. */
function message(session: string, body: string, deliveries: MessageDelivery[]): Message {
  return {
    author: { id: "alice", kind: "user" },
    body,
    broadcast_id: null,
    created_at: new Date().toISOString(),
    deleted_at: null,
    deleted_by: null,
    deliveries,
    edited_at: null,
    id: `message-${session}`,
    in_reply_to: null,
    issue_key: null,
    reply_block: null,
    target: `session:${session}`,
  };
}

/** A broadcast to `sessions`, each copy with the attempts `deliveries` gives it. */
function broadcast(
  body: string,
  sessions: readonly string[],
  deliveries: (session: string) => MessageDelivery[]
): BroadcastCreated {
  return {
    author: { id: "alice", kind: "user" },
    body,
    created_at: new Date().toISOString(),
    delivery: "btw",
    excluded: [],
    id: "broadcast-1",
    recipients: sessions.map((session) => ({
      message: message(session, body, deliveries(session)),
      replies: [],
      session_id: session,
    })),
  };
}

/** The comment on CORE-1 that mentions `session`, with its delivery to that session. */
function comment(session: string, body: string, state: string): Comment {
  return {
    ...commentDeliveryFields(),
    anchor: null,
    ask_id: null,
    author: { id: "alice", kind: "user" },
    body,
    created_at: new Date().toISOString(),
    deliveries: [
      {
        attempt: 1,
        comment_id: "comment-1",
        created_at: new Date().toISOString(),
        delivery: "steer",
        duplicate: false,
        envelope_id: "envelope-1",
        error: null,
        reply_id: null,
        resolve_error: null,
        session_id: session,
        state,
        target: `session:${session}`,
      },
    ],
    edited_at: null,
    id: "comment-1",
    issue_key: "CORE-1",
    mentions: [{ delivery: "steer", session_id: session, target: `session:${session}` }],
    reply_to: null,
    resolved: false,
    resolved_at: null,
    resolved_by: null,
    suggestion: null,
    turn: null,
  };
}

const builder = {
  ...planner,
  dir: "/workspaces/builder",
  session_id: "builder-session",
  title: "Builder",
} satisfies Agent;

/** The dialog over a registry of the Planner, the Reviewer and the Builder, with the app's keys
 *  bound to the window. */
function renderDialog() {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const onClose = jest.fn();
  const listAgents = spyOn(api, "listAgents").mockResolvedValue([planner, reviewer, builder]);
  const view = render(
    <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
      <QueryClientProvider client={queryClient}>
        <KeymapProvider>
          <ComposeDialog onClose={onClose} />
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  return {
    onClose,
    restore: () => {
      view.unmount();
      listAgents.mockRestore();
    },
  };
}

/** Ticks `names` in the Recipients list, in that order, and closes it. */
async function pickRecipients(user: UserEvent, names: readonly string[]): Promise<void> {
  await user.click(await screen.findByRole("button", { name: pickNamed("Recipients") }));
  const list = await screen.findByRole("listbox", { name: "Recipients options" });
  for (const name of names) {
    await user.click(await within(list).findByRole("option", { name }));
  }
  await user.keyboard("{Escape}");
}

/** Writes `body` into the message box named `label`. */
async function write(user: UserEvent, label: string, body: string): Promise<void> {
  const box = screen.getByRole("textbox", { name: label });
  await user.click(box);
  await user.paste(body);
}

/** The outcome the dialog draws for `session`. */
function outcomeOf(session: string): string {
  const row = document.querySelector(`[data-compose-outcome="${session}"]`);
  return row?.textContent ?? "(no outcome drawn)";
}

// A send the server refuses gives Send back enabled with its error, and the form stays
// with what the person wrote, for a broadcast and for a send to one.
test("a refused send gives Send back with its error", async () => {
  const user = userEvent.setup();
  const dialog = renderDialog();
  const refusedBroadcast = "no selected session can receive this message";
  const createBroadcast = spyOn(api, "createBroadcast").mockRejectedValue(
    new ApiError(400, { code: "BROADCAST_EMPTY", error: refusedBroadcast })
  );
  const refusedMessage = "the session refuses this message";
  const createAgentMessage = spyOn(api, "createAgentMessage").mockRejectedValue(
    new ApiError(400, { code: "MESSAGE_REFUSED", error: refusedMessage })
  );
  try {
    await pickRecipients(user, ["Planner", "Reviewer"]);
    await write(user, "Broadcast message", "Status, please.");
    await user.click(screen.getByRole("button", { name: "Send to 2" }));
    await screen.findByText(`Couldn't send — ${refusedBroadcast}`);
    const sendToTwo = screen.getByRole("button", { name: "Send to 2" });
    expect(sendToTwo.getAttribute("aria-disabled")).toBeNull();
    expect(screen.getByRole("textbox", { name: "Broadcast message" })).toHaveValue(
      "Status, please."
    );
    expect(screen.queryByRole("region", { name: "Where your message went" })).toBeNull();

    // To one recipient the composer is the Agents page's, which says its refusal itself.
    await user.click(screen.getByTitle("Remove Reviewer from this broadcast"));
    const field = screen.getByRole("textbox", { name: "Comment" });
    expect(field).toHaveValue("Status, please.");
    await user.click(screen.getByRole("button", { name: "Send" }));
    await screen.findByText(`Couldn't send — ${refusedMessage}`);
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Send" }).getAttribute("aria-disabled")).toBeNull()
    );
    expect(field).toHaveValue("Status, please.");
    expect(screen.queryByRole("region", { name: "Where your message went" })).toBeNull();
    expect(createAgentMessage).toHaveBeenCalledTimes(1);
    expect(createBroadcast).toHaveBeenCalledTimes(1);
  } finally {
    createBroadcast.mockRestore();
    createAgentMessage.mockRestore();
    dialog.restore();
  }
});

// Once a broadcast's answer lands, the outcomes take the form's place with Done first, and the
// dialog's own hand-off puts the focus on Done. The dialog holds its Send through the answer
// (`useHeldThroughAnswer`), so the press is still live as Send goes with the form.
test("the focus goes to Done once a broadcast's answer lands", async () => {
  const user = userEvent.setup();
  const answer = Promise.withResolvers<BroadcastCreated>();
  const createBroadcast = spyOn(api, "createBroadcast").mockReturnValue(answer.promise);
  const getBroadcast = spyOn(api, "getBroadcast").mockReturnValue(new Promise(() => {}));
  const dialog = renderDialog();
  try {
    await pickRecipients(user, ["Planner", "Reviewer"]);
    await write(user, "Broadcast message", "Status, please.");
    await user.click(screen.getByRole("button", { name: "Send to 2" }));
    const held = await screen.findByRole("button", { name: "Sending…" });
    expect(held.getAttribute("aria-disabled")).toBe("true");
    expect(held).toHaveFocus();
    answer.resolve(broadcast("Status, please.", ["planner-session", "reviewer-session"], () => []));
    await screen.findByRole("region", { name: "Where your message went" });
    await afterFrameAndTasks();
    expect(screen.getByRole("button", { name: "Done" })).toHaveFocus();
  } finally {
    createBroadcast.mockRestore();
    getBroadcast.mockRestore();
    dialog.restore();
  }
});

// The outcomes are drawn from the answer first, and redrawn from the send's own read
// once it answers a state the answer did not: a broadcast's from GET /api/v1/broadcasts/{id},
// where a copy taken reads sent, one stored reads stored for later, and one refused reads failed
// with its error; and a message's to one session from GET /api/v1/messages/{id}, where a session
// the listener does not know reads failed with the listener's words and never left out.
test("an outcome follows the delivery read after the answer", async () => {
  const user = userEvent.setup();
  const dialog = renderDialog();
  const body = "Ship it when green.";
  const refusedCopy = "the session refused the copy";
  const createBroadcast = spyOn(api, "createBroadcast").mockImplementation(async (input) =>
    broadcast(input.body, input.session_ids ?? [], () => [])
  );
  const broadcastRead = Promise.withResolvers<BroadcastRead>();
  const getBroadcast = spyOn(api, "getBroadcast").mockReturnValue(broadcastRead.promise);
  try {
    await pickRecipients(user, ["Planner", "Reviewer", "Builder"]);
    await write(user, "Broadcast message", body);
    await user.click(screen.getByRole("button", { name: "Send to 3" }));
    await screen.findByRole("region", { name: "Where your message went" });
    // The answer: each copy made, no attempt settled yet.
    expect(outcomeOf("planner-session")).toContain("On its way");
    expect(outcomeOf("reviewer-session")).toContain("On its way");
    expect(outcomeOf("builder-session")).toContain("On its way");
    broadcastRead.resolve(
      broadcast(body, ["planner-session", "reviewer-session", "builder-session"], (session) => [
        session === "planner-session"
          ? attempt(session, "sent")
          : session === "reviewer-session"
            ? attempt(session, "queued")
            : attempt(session, "failed", refusedCopy),
      ])
    );
    await waitFor(() => expect(outcomeOf("planner-session")).toContain("Sent"));
    expect(outcomeOf("reviewer-session")).toContain(
      "Stored for later: the session is stopped; it gets the message if it runs again within 72 hours"
    );
    expect(outcomeOf("builder-session")).toContain(`Failed: ${refusedCopy}`);
    expect(getBroadcast).toHaveBeenCalledWith("broadcast-1");
  } finally {
    createBroadcast.mockRestore();
    getBroadcast.mockRestore();
    dialog.restore();
  }

  const single = renderDialog();
  const failure = "no live session planner-session";
  const createAgentMessage = spyOn(api, "createAgentMessage").mockImplementation(
    async (session, input) => message(session, input.body, [attempt(session, "queued")])
  );
  const messageRead = Promise.withResolvers<MessageRead>();
  const getMessageThread = spyOn(api, "getMessageThread").mockReturnValue(messageRead.promise);
  try {
    await pickRecipients(user, ["Planner"]);
    await write(user, "Comment", body);
    await user.click(screen.getByRole("button", { name: "Send" }));
    await screen.findByRole("region", { name: "Where your message went" });
    expect(outcomeOf("planner-session")).toContain(
      "Stored for later: the session is stopped; it gets the message if it runs again within 72 hours"
    );
    messageRead.resolve({
      message: message("planner-session", body, [attempt("planner-session", "failed", failure)]),
      replies: [],
    });
    await waitFor(() => expect(outcomeOf("planner-session")).toContain(`Failed: ${failure}`));
    expect(outcomeOf("planner-session")).not.toContain("Left out");
    expect(outcomeOf("planner-session")).toContain("planner-session");
    expect(getMessageThread).toHaveBeenCalledWith("message-planner-session");
  } finally {
    createAgentMessage.mockRestore();
    getMessageThread.mockRestore();
    single.restore();
  }
});

// A follow-up read that fails keeps the outcomes the answer gave, with the read's
// error beside them, and Done still closes the dialog: the message's read, the broadcast's read,
// and the read of the comment that mentions the session where an issue was picked.
test("a failed delivery read keeps the answer's outcomes", async () => {
  const user = userEvent.setup();
  const broke = new ApiError(500, { code: "INTERNAL", error: "the read broke" });
  const dialog = renderDialog();
  const createAgentMessage = spyOn(api, "createAgentMessage").mockImplementation(
    async (session, input) => message(session, input.body, [attempt(session, "queued")])
  );
  const getMessageThread = spyOn(api, "getMessageThread").mockRejectedValue(broke);
  try {
    await pickRecipients(user, ["Planner"]);
    await write(user, "Comment", "Back in ten.");
    await user.click(screen.getByRole("button", { name: "Send" }));
    await screen.findByRole("region", { name: "Where your message went" });
    await screen.findByText(/Could not read where it went since: the read broke\./);
    expect(outcomeOf("planner-session")).toContain(
      "Stored for later: the session is stopped; it gets the message if it runs again within 72 hours"
    );
    await user.click(screen.getByRole("button", { name: "Done" }));
    expect(dialog.onClose).toHaveBeenCalledTimes(1);
  } finally {
    createAgentMessage.mockRestore();
    getMessageThread.mockRestore();
    dialog.restore();
  }

  const many = renderDialog();
  const createBroadcast = spyOn(api, "createBroadcast").mockImplementation(async (input) =>
    broadcast(input.body, input.session_ids ?? [], (session) => [attempt(session, "queued")])
  );
  const getBroadcast = spyOn(api, "getBroadcast").mockRejectedValue(broke);
  try {
    await pickRecipients(user, ["Planner", "Reviewer"]);
    await write(user, "Broadcast message", "Back in ten.");
    await user.click(screen.getByRole("button", { name: "Send to 2" }));
    await screen.findByRole("region", { name: "Where your message went" });
    await screen.findByText(/Could not read where it went since: the read broke\./);
    expect(getBroadcast).toHaveBeenCalledWith("broadcast-1");
    for (const session of ["planner-session", "reviewer-session"]) {
      expect(outcomeOf(session)).toContain(
        "Stored for later: the session is stopped; it gets the message if it runs again within 72 hours"
      );
    }
    await user.click(screen.getByRole("button", { name: "Done" }));
    expect(many.onClose).toHaveBeenCalledTimes(1);
  } finally {
    createBroadcast.mockRestore();
    getBroadcast.mockRestore();
    many.restore();
  }

  const commented = renderDialog();
  const listIssues = spyOn(api, "listIssues").mockResolvedValue([
    {
      ...issue,
      open_asks: 0,
      progress: { children: null, tasks: null },
      route_holder: null,
      route_status: null,
      route_status_reason: null,
    },
  ]);
  const createComment = spyOn(api, "createComment").mockImplementation(async (_key, input) =>
    comment("planner-session", input.body ?? "", "sent")
  );
  const getComment = spyOn(api, "getComment").mockRejectedValue(broke);
  try {
    await pickRecipients(user, ["Planner"]);
    await user.click(screen.getByRole("button", { name: pickNamed("Choose issue") }));
    const issues = await screen.findByRole("listbox", { name: "Issue options" });
    await user.click(
      await within(issues).findByRole("option", { name: "CORE-1 · Review the spec" })
    );
    await write(user, "Comment", " Back in ten.");
    await user.click(screen.getByRole("button", { name: "Send" }));
    await screen.findByRole("region", { name: "Where your message went" });
    await screen.findByText(/Could not read where it went since: the read broke\./);
    expect(createComment).toHaveBeenCalledTimes(1);
    expect(getComment).toHaveBeenCalledWith("comment-1");
    expect(outcomeOf("planner-session")).toContain("Sent");
    await user.click(screen.getByRole("button", { name: "Done" }));
    expect(commented.onClose).toHaveBeenCalledTimes(1);
  } finally {
    listIssues.mockRestore();
    createComment.mockRestore();
    getComment.mockRestore();
    commented.restore();
  }
});

/** The tries a write makes inside the write deadline, each one the server answers 503, and the
 *  wait before each next try: past the last the write is late. */
async function failToTheDeadline(): Promise<void> {
  for (const wait of [1_000, 2_000, 4_000, 8_000]) await jump(wait);
  expect(screen.queryByRole("button", { name: "Stop trying" })).not.toBeInTheDocument();
  await jump(15_000);
}

const stoppedText = new SendStoppedError().message;

// A send still out at the write deadline is late. The press's hold ends there, the
// dialog says the send is still being tried, and Stop trying gives Send back enabled with what
// stopped it, for a send to one and for a broadcast.
test("Send's hold ends at the write deadline", async () => {
  const unavailable = new ApiError(503, { code: "UNAVAILABLE", error: "the server is down" });
  const createAgentMessage = spyOn(api, "createAgentMessage").mockRejectedValue(unavailable);
  const createBroadcast = spyOn(api, "createBroadcast").mockRejectedValue(unavailable);
  const dialog = renderDialog();
  jest.useFakeTimers();
  const user = userOnFakeClock();
  try {
    await pickRecipients(user, ["Planner"]);
    await write(user, "Comment", "Status please");
    await user.click(screen.getByRole("button", { name: "Send" }));
    await tick(0);
    expect(screen.getByRole("button", { name: "Sending…" }).getAttribute("aria-disabled")).toBe(
      "true"
    );
    await failToTheDeadline();
    expect(screen.getByText(KEYED_SEND_LATE_TEXT)).toBeTruthy();
    await user.click(screen.getByRole("button", { name: "Stop trying" }));
    await tick(0);
    expect(screen.getByText(stoppedText)).toBeTruthy();
    expect(screen.getByRole("button", { name: "Send" }).getAttribute("aria-disabled")).toBeNull();
    expect(screen.getByRole("textbox", { name: "Comment" })).toHaveValue("Status please");

    // The broadcast: the same deadline, and its own Stop trying.
    await pickRecipients(user, ["Reviewer"]);
    await user.click(screen.getByRole("button", { name: "Send to 2" }));
    await tick(0);
    expect(screen.getByRole("button", { name: "Sending…" }).getAttribute("aria-disabled")).toBe(
      "true"
    );
    await failToTheDeadline();
    expect(screen.getByText(KEYED_SEND_LATE_TEXT)).toBeTruthy();
    await user.click(screen.getByRole("button", { name: "Stop trying" }));
    await tick(0);
    expect(screen.getByText(`Couldn't send — ${stoppedText}`)).toBeTruthy();
    expect(
      screen.getByRole("button", { name: "Send to 2" }).getAttribute("aria-disabled")
    ).toBeNull();
  } finally {
    realClock();
    createAgentMessage.mockRestore();
    createBroadcast.mockRestore();
    dialog.restore();
  }
});

// The message stays across one recipient, two and back, even where a send to that one session is
// held under the dialog's send key: refused here. The composer mounted again for that session
// takes the dialog's message, not the refused send's draft, and Send sends what the field holds.
test("the message stays over a refused send to one as the recipients change", async () => {
  const user = userEvent.setup();
  const refusedMessage = "the session refuses this message";
  const createAgentMessage = spyOn(api, "createAgentMessage")
    .mockRejectedValueOnce(new ApiError(400, { code: "MESSAGE_REFUSED", error: refusedMessage }))
    .mockImplementation(async (session, input) =>
      message(session, input.body, [attempt(session, "sent")])
    );
  const getMessageThread = spyOn(api, "getMessageThread").mockReturnValue(new Promise(() => {}));
  const dialog = renderDialog();
  try {
    await pickRecipients(user, ["Planner"]);
    await write(user, "Comment", "Status, please.");
    await user.click(screen.getByRole("button", { name: "Send" }));
    await screen.findByText(`Couldn't send — ${refusedMessage}`);

    await pickRecipients(user, ["Reviewer"]);
    const broadcastBox = screen.getByRole("textbox", { name: "Broadcast message" });
    expect(broadcastBox).toHaveValue("Status, please.");
    await write(user, "Broadcast message", " Edited.");
    const edited = (broadcastBox as HTMLTextAreaElement).value;
    expect(edited).not.toBe("Status, please.");
    await user.click(screen.getByTitle("Remove Reviewer from this broadcast"));

    expect(screen.getByRole("textbox", { name: "Comment" })).toHaveValue(edited);
    await user.click(screen.getByRole("button", { name: "Send" }));
    await screen.findByRole("region", { name: "Where your message went" });
    expect(createAgentMessage).toHaveBeenCalledTimes(2);
    expect(createAgentMessage.mock.calls[1]?.[1].body).toBe(edited);
  } finally {
    createAgentMessage.mockRestore();
    getMessageThread.mockRestore();
    dialog.restore();
  }
});

// While a send to one is out, the recipients are held as a broadcast's are: a change of them
// would take the composer whose Send is out off the page, and its outcome with it.
test("the recipients are held while a send to one is out", async () => {
  const user = userEvent.setup();
  const answer = Promise.withResolvers<Message>();
  const createAgentMessage = spyOn(api, "createAgentMessage").mockReturnValue(answer.promise);
  const getMessageThread = spyOn(api, "getMessageThread").mockReturnValue(new Promise(() => {}));
  const dialog = renderDialog();
  try {
    await pickRecipients(user, ["Planner"]);
    const recipients = screen.getByRole("button", { name: pickNamed("Recipients") });
    expect(recipients).toBeEnabled();
    await write(user, "Comment", "Status, please.");
    await user.click(screen.getByRole("button", { name: "Send" }));
    await waitFor(() => expect(recipients).toBeDisabled());
    answer.resolve(
      message("planner-session", "Status, please.", [attempt("planner-session", "sent")])
    );
    await screen.findByRole("region", { name: "Where your message went" });
    expect(outcomeOf("planner-session")).toContain("Sent");
  } finally {
    createAgentMessage.mockRestore();
    getMessageThread.mockRestore();
    dialog.restore();
  }
});

// The Agents page's rule for a broadcast's key (`useBroadcastComposition`): a new key at every
// press, and the old key only for a refused request sent again unchanged. After Stop trying, the
// request sent again as it was goes under its own key, so Dispatch answers a repeat with what the
// first made. An edit makes a new request, which goes under a new key, so Dispatch never refuses
// it as a key reused for another request.
test("a broadcast sent again after Stop trying keeps its key only while it is unchanged", async () => {
  const unavailable = new ApiError(503, { code: "UNAVAILABLE", error: "the server is down" });
  const keys: string[] = [];
  const createBroadcast = spyOn(api, "createBroadcast").mockImplementation(
    async (input, bounds) => {
      keys.push(bounds.key);
      if (input.body === "Status please.") throw unavailable;
      return broadcast(input.body, input.session_ids ?? [], () => []);
    }
  );
  const getBroadcast = spyOn(api, "getBroadcast").mockReturnValue(new Promise(() => {}));
  const dialog = renderDialog();
  jest.useFakeTimers();
  const user = userOnFakeClock();
  try {
    await pickRecipients(user, ["Planner", "Reviewer"]);
    await write(user, "Broadcast message", "Status please.");
    await user.click(screen.getByRole("button", { name: "Send to 2" }));
    await tick(0);
    await failToTheDeadline();
    await user.click(screen.getByRole("button", { name: "Stop trying" }));
    await tick(0);
    const first = keys[0];
    expect(new Set(keys)).toEqual(new Set([first]));

    // Sent again as it was: under its own key.
    keys.length = 0;
    await user.click(screen.getByRole("button", { name: "Send to 2" }));
    await tick(0);
    expect(keys).toEqual([first]);
    await failToTheDeadline();
    await user.click(screen.getByRole("button", { name: "Stop trying" }));
    await tick(0);

    // Edited and sent again: a new request, under a new key, which Dispatch takes.
    keys.length = 0;
    await write(user, "Broadcast message", " Now.");
    await user.click(screen.getByRole("button", { name: "Send to 2" }));
    await tick(0);
    expect(keys).toHaveLength(1);
    expect(keys[0]).not.toBe(first);
    expect(screen.getByRole("region", { name: "Where your message went" })).toBeTruthy();
  } finally {
    realClock();
    createBroadcast.mockRestore();
    getBroadcast.mockRestore();
    dialog.restore();
  }
});

// Escape, Cancel and a click on the backdrop ask before they discard a message, in the words
// and with the controls of the one-recipient composer's own ask, and Keep leaves the message as
// it was. With no message they close the dialog at once.
test("closing the dialog over a message asks before it discards it", async () => {
  const user = userEvent.setup();
  const dialog = renderDialog();
  try {
    // No message: Escape closes at once.
    await user.keyboard("{Escape}");
    expect(dialog.onClose).toHaveBeenCalledTimes(1);
    await pickRecipients(user, ["Planner", "Reviewer"]);
    expect(dialog.onClose).toHaveBeenCalledTimes(1);

    // Escape in the broadcast's message box asks, and Keep leaves the message and the focus.
    await write(user, "Broadcast message", "Status, please.");
    const box = screen.getByRole("textbox", { name: "Broadcast message" });
    await user.keyboard("{Escape}");
    const ask = await screen.findByRole("alert");
    expect(ask).toHaveTextContent("Discard draft?");
    expect(dialog.onClose).toHaveBeenCalledTimes(1);
    await user.click(within(ask).getByRole("button", { name: "Keep" }));
    expect(screen.queryByText("Discard draft?")).toBeNull();
    expect(box).toHaveValue("Status, please.");
    expect(box).toHaveFocus();

    // A click on the backdrop asks too.
    const backdrop = screen.getByTestId("compose-dialog").parentElement?.previousElementSibling;
    if (!(backdrop instanceof HTMLElement)) throw new Error("the dialog draws no backdrop");
    await user.click(backdrop);
    await user.click(
      within(await screen.findByRole("alert")).getByRole("button", { name: "Keep" })
    );
    expect(dialog.onClose).toHaveBeenCalledTimes(1);
    expect(box).toHaveValue("Status, please.");

    // And so does Cancel, whose Discard closes the dialog.
    await user.click(screen.getByRole("button", { name: "Cancel" }));
    await user.click(
      within(await screen.findByRole("alert")).getByRole("button", { name: "Discard" })
    );
    expect(dialog.onClose).toHaveBeenCalledTimes(2);
  } finally {
    dialog.restore();
  }
});
