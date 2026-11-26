import { afterEach, beforeAll, expect, jest, type Mock, spyOn, test } from "bun:test";
import { DELETED_POST_DELIVERY_ERROR } from "@legion/contracts";
import {
  type MutationKey,
  onlineManager,
  QueryClientProvider,
  useQuery,
} from "@tanstack/react-query";
// biome-ignore lint/style/noRestrictedImports: the two clicks of one same-tick double press, which user-event cannot fire inside a single task
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";
import { commentDeliveryFields } from "../../__tests__/comment-fixture";
import { correction, EDITED_AT } from "../../__tests__/correction-feed";
import { displayTree } from "../../__tests__/display-trees";
import { type Lists, newQueryClient, stubLists, tab } from "../../__tests__/draft-matrix-fixture";
import { jump, realClock, settle, tick } from "../../__tests__/fake-clock";
import { afterFrameAndTasks } from "../../__tests__/frames";
import { heldAnswers } from "../../__tests__/held-answers";
import { pressHeld } from "../../__tests__/held-press";
import { heldUntilLate, pressesOf } from "../../__tests__/held-until-late";
import {
  dwellOn,
  type IntersectionControl,
  installIntersectionObserver,
  type Placement,
} from "../../__tests__/intersection-observer";
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { messageCorrectionFields } from "../../__tests__/message-fixture";
import { ownReplies } from "../../__tests__/own-replies";
import { answerIssueStates, answerThreadReads } from "../../__tests__/send-answers";
import { heldDraftWords, stopReplySend } from "../../__tests__/stop-late-write";
import { userOnFakeClock } from "../../__tests__/user";
import { wholeText } from "../../__tests__/whole-text";
import type { WriteBounds } from "../../api/client";
import { ApiError, api, NetworkError, REQUEST_DEADLINE_MS } from "../../api/client";
import { markCorrectedPost } from "../../api/corrected-post";
import { userStateQuery } from "../../api/queries";
import { DispatchQueryClient, hearLiveEvent } from "../../api/query-client";
import type {
  Actor,
  Agent,
  Artifact,
  Ask,
  Comment,
  CommentDelivery,
  DispatchEvent,
  DisplayTree,
  Handoff,
  IssueAskRow,
  IssueAskThreadRow,
  Message,
  MessageDelivery,
  MessageRead,
  UserIssueState,
  UserState,
} from "../../api/types";
import { messageDeliveryFields } from "../agents/delivery-fixture";
import { PHONE_WIDTH, stubViewport } from "../margin/margin-fixture";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { KeymapProvider } from "../shell/KeymapProvider";
import { useKeymapScope } from "../shell/keymap";
import { userPreferenceStorageKey } from "../shell/userPreference";
import { seedUnread } from "../threads/replies-fixture";
import { SendStoppedError } from "../writes/keyed-write";
import { writesOf } from "../writes/write-store";
import { ConversationTab } from "./ConversationTab";
import { answeredWithErrorGuidance, safeRetryGuidance } from "./delivery";
import { threadReplySendKey } from "./usePhoneThread";

// The rows draw through the shared renderer, whose Markdown engine loads lazily, and they run in
// random order, so the file loads the engine before its first row, whichever row that is.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

const alice: Actor = { id: "alice", kind: "user" };

// Delivery attempts are dated relative to the run: the dashboard only offers a
// same-mode Retry while an attempt is inside the stream's duplicate window, so a
// fixture frozen at an absolute date would age out of every retry assertion.
const recentAttemptAt = new Date(Date.now() - 60_000).toISOString();

function issueState(dismissed: string[] = [], lastReadSeq = 0): UserIssueState {
  return { dismissed, last_read_seq: lastReadSeq, pinned: false, seq: 0 };
}

function comment(id: string, overrides: Partial<Comment> = {}): Comment {
  return {
    ...commentDeliveryFields(),
    anchor: null,
    ask_id: null,
    author: alice,
    body: `Body of ${id}`,
    created_at: "2026-09-20T00:00:00Z",
    edited_at: null,
    id,
    issue_key: "CORE-1",
    reply_to: null,
    resolved: false,
    resolved_at: null,
    resolved_by: null,
    suggestion: null,
    turn: null,
    ...overrides,
  };
}

function message(id: string, overrides: Partial<Message> = {}): Message {
  return {
    author: alice,
    broadcast_id: null,
    reply_block: null,
    body: `Body of ${id}`,
    created_at: "2026-09-09T00:00:00Z",
    deliveries: [],
    ...messageCorrectionFields(),
    id,
    in_reply_to: null,
    issue_key: "CORE-1",
    target: null,
    ...overrides,
  };
}

function thread(root: Message, replies: Message[] = []): MessageRead {
  return { message: root, replies };
}

function attempt(overrides: Partial<MessageDelivery> = {}): MessageDelivery {
  return {
    ...messageDeliveryFields(),
    attempt: 1,
    created_at: recentAttemptAt,
    delivery: "btw",
    envelope_id: null,
    error: null,
    message_id: "message-1",
    reply_id: null,
    session_id: "s1",
    state: "sent",
    ...overrides,
  };
}

function mentionAttempt(overrides: Partial<CommentDelivery> = {}): CommentDelivery {
  return {
    duplicate: false,
    attempt: 1,
    comment_id: "comment-1",
    created_at: recentAttemptAt,
    delivery: "steer",
    envelope_id: null,
    error: null,
    reply_id: null,
    resolve_error: null,
    session_id: "worker-session",
    state: "failed",
    target: "session:worker-session",
    ...overrides,
  };
}

const baseAsk: Ask = {
  anchor: null,
  artifact_id: null,
  asked_of: null,
  asked_of_at: null,
  asked_of_by: null,
  block_id: null,
  answer: null,
  author: { id: "s1", kind: "session" },
  created_at: "2026-09-10T09:00:00Z",
  edited_at: null,
  id: "ask-1",
  issue_key: "CORE-1",
  kind: "question",
  multiple: false,
  opened_event_id: 4,
  options: [{ label: "Ship" }, { label: "Hold" }],
  question: "Ship it?",
  state: "open",
  urgency: "med",
};

function askRow(overrides: Partial<Ask> = {}, replies: Comment[] = []): IssueAskThreadRow {
  return {
    ...baseAsk,
    ...overrides,
    last_reply: null,
    thread: {
      answers: [],
      claimant: null,
      edits: [],
      followers: [],
      replies,
      route: null,
      route_status: null,
      route_holder: null,
      route_status_reason: null,
    },
  };
}

function headEvent(seq: number, at = "2026-09-20T00:00:00Z"): DispatchEvent {
  return {
    actor: alice,
    created_at: at,
    id: seq,
    issue_key: "CORE-1",
    notify: false,
    payload: {},
    seq,
    type: "issue.created",
  } as DispatchEvent;
}

function agent(sessionId: string, title: string, capabilities: string[] = []): Agent {
  return {
    capabilities,
    dir: "/w",
    host: "m",
    last_activity: null,
    last_seen: 1,
    machine_id: "m",
    open_asks: 0,
    roles: [],
    state: "live",
    session_id: sessionId,
    title,
  };
}

const primaryDocument: Artifact = {
  created_at: "2026-09-09T00:00:00Z",
  created_by: alice,
  id: "artifact-primary",
  issue_key: "CORE-1",
  kind: "doc",
  name: "spec.md",
  primary: true,
  project: "CORE",
  ref_key: "CORE-1/spec",
  session_id: null,
  slug: "spec",
  versions: [],
};

const secondaryDocument: Artifact = {
  ...primaryDocument,
  id: "artifact-secondary",
  name: "supporting-document.md",
  primary: false,
  slug: "supporting-document",
};

// The sticky composer sits at document top 0 and the newest card below it. `useFollowLatest`
// reads their bounding rectangles only to place the newest card beneath the composer, so these
// tests fake that minimal layout rather than exercising a real browser layout engine.
const COMPOSER_HEIGHT = 140;
const NEWEST_TURN_DOC_TOP = 300;
const RESTING_SCROLL_Y = NEWEST_TURN_DOC_TOP - COMPOSER_HEIGHT;

function installScrollLayoutMocks(): { restore: () => void } {
  const rectSpy = spyOn(Element.prototype, "getBoundingClientRect").mockImplementation(function (
    this: Element
  ) {
    const isComposer = this.getAttribute("aria-label") === "Comment composer";
    const isTurn = this.hasAttribute("data-turn");
    const docTop = isComposer ? 0 : isTurn ? NEWEST_TURN_DOC_TOP : 0;
    const height = isComposer ? COMPOSER_HEIGHT : isTurn ? 50 : 0;
    const top = docTop - window.scrollY;
    return {
      bottom: top + height,
      height,
      left: 0,
      right: 100,
      toJSON: () => ({}),
      top,
      width: 100,
      x: 0,
      y: top,
    };
  });
  const originalScrollTo = window.scrollTo;
  window.scrollTo = (options?: ScrollToOptions | number, y?: number) => {
    const top = typeof options === "number" ? y : options?.top;
    if (top !== undefined) {
      Object.defineProperty(window, "scrollY", { configurable: true, value: top, writable: true });
    }
  };
  return {
    restore: () => {
      rectSpy.mockRestore();
      window.scrollTo = originalScrollTo;
    },
  };
}

function withScrollY(run: () => Promise<void>): () => Promise<void> {
  return async () => {
    const scrollY = Object.getOwnPropertyDescriptor(window, "scrollY");
    try {
      await run();
    } finally {
      if (scrollY === undefined) {
        Reflect.deleteProperty(window, "scrollY");
      } else {
        Object.defineProperty(window, "scrollY", scrollY);
      }
    }
  };
}

/** Stubs `scrollIntoView` on Element, the prototype that owns the method: patching HTMLElement
 *  would leave an own property behind that shadows every later patch on Element. */
function stubScrollIntoView() {
  return spyOn(Element.prototype, "scrollIntoView").mockImplementation(() => {});
}

afterEach(() => {
  jest.useRealTimers();
});

test("the read mark watches the newest conversation alone, and only while the tab is visible", async () => {
  const viewport = installIntersectionObserver();
  const stub = stubLists({
    head: [headEvent(7)],
    messages: [
      thread(message("message-1", { created_at: "2026-09-09T00:00:00Z" })),
      thread(message("message-2", { created_at: "2026-09-09T00:05:00Z" })),
    ],
  });
  const queryClient = newQueryClient();
  const view = render(tab({ "CORE-1": issueState() }, false, queryClient));

  try {
    await screen.findByText("Body of message-2");
    expect(viewport.observed().filter((element) => view.container.contains(element))).toHaveLength(
      0
    );

    view.rerender(tab({ "CORE-1": issueState() }, true, queryClient));
    const newest = view.container.querySelector('[data-turn="message:message-2"]');
    const older = view.container.querySelector('[data-turn="message:message-1"]');
    await waitFor(() => expect(viewport.observed()).toContain(newest as Element));
    expect(viewport.observed()).not.toContain(older as Element);
  } finally {
    view.unmount();
    stub.restore();
    viewport.restore();
  }
});

test("Conversation holds Reply while its composer sends", async () => {
  const user = userEvent.setup();
  const sent = Promise.withResolvers<Comment>();
  const createComment = spyOn(api, "createComment").mockReturnValueOnce(sent.promise);
  const stub = stubLists({ messages: [thread(message("message-1"))] });
  const queryClient = newQueryClient();
  const view = render(tab({ "CORE-1": issueState() }, true, queryClient));

  try {
    const field = await screen.findByLabelText<HTMLTextAreaElement>("Comment");
    await user.clear(field);
    await user.click(field);
    await user.paste("Status please");
    await user.click(screen.getByRole("button", { name: "Send" }));
    await waitFor(() =>
      expect(field.closest("fieldset")?.getAttribute("aria-disabled")).toBe("true")
    );
    // The held composer keeps the box focusable, and a key in it writes nothing.
    act(() => field.focus());
    await user.keyboard("x");
    expect(field.value).toBe("Status please");
    const reply = screen.getByRole("button", { name: "Reply" });
    expect(reply).toHaveAttribute("aria-disabled", "true");
    expect(reply.hasAttribute("disabled")).toBe(false);
    await user.click(reply);
    expect(screen.queryByRole("button", { name: "Cancel reply" })).not.toBeInTheDocument();

    sent.reject(new Error("the server is down"));
    await screen.findByText("Couldn't send — network error");
    expect(createComment.mock.calls[0]).toEqual([
      "CORE-1",
      { body: "Status please" },
      keyedAttempt,
    ]);
  } finally {
    view.unmount();
    createComment.mockRestore();
    stub.restore();
  }
});

test("a message reply that names a passage quotes it, and says so once the passage is removed", async () => {
  const passage = {
    block_id: "b2",
    node: "message/message-1#b2",
    ref: "dispatch://CORE-1/message/message-1#b-b2",
    removed: false,
    text: "Beta.",
  };
  const reply = (id: string, overrides: Partial<Message>) =>
    message(id, { created_at: "2026-09-09T00:01:00Z", in_reply_to: "message-1", ...overrides });
  const stub = stubLists({
    messages: [
      thread(message("message-1", { body: "Alpha.\n\nBeta." }), [
        reply("message-2", { body: "Agreed.", reply_block: passage }),
        reply("message-3", { body: "Still?", reply_block: { ...passage, removed: true } }),
        reply("message-4", { body: "All of it.", reply_block: null }),
      ]),
    ],
  });
  const view = render(tab({ "CORE-1": issueState() }, true, newQueryClient()));
  const turn = (id: string) => view.container.querySelector(`[data-turn="message:${id}"]`);

  try {
    await screen.findByText("Agreed.");
    await waitFor(() =>
      expect(turn("message-2")?.textContent).toContain("Replying to alice's passage — Beta.")
    );
    expect(turn("message-3")?.textContent).toContain(
      "Replying to alice's passage — This passage was removed."
    );
    expect(turn("message-3")?.textContent).not.toContain("Beta.");
    // A reply that names no block quotes the whole parent, as before.
    expect(turn("message-4")?.textContent).toContain("Replying to alice — Alpha.");
  } finally {
    view.unmount();
    stub.restore();
  }
});

// W22. Each pin is a write of the one store, in press order on its issue: the ones Dispatch refuses
// stay refused in the shell's indicator, the Pin buttons stay live, and each Retry re-sends its own
// pin over the state Dispatch holds then, so after both land the issue holds both.
test("each refused pin is retried on its own, over the state Dispatch holds", async () => {
  const user = userEvent.setup();
  const originalGetMyState = api.getMyState;
  const originalPutIssueState = api.putIssueState;
  const stub = stubLists({
    messages: [
      thread(message("message-1", { created_at: "2026-09-09T00:00:00Z" })),
      thread(message("message-2", { created_at: "2026-09-09T00:05:00Z" })),
    ],
  });
  const queryClient = newQueryClient();
  queryClient.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  let saved: string[] = [];
  let failures = 2;
  let unmount: (() => void) | undefined;

  try {
    api.getMyState = async () => ({ "CORE-1": issueState(saved) });
    api.putIssueState = async (_issueKey, input) => {
      if (failures > 0) {
        failures -= 1;
        throw new ApiError(409, { code: "PIN_REFUSED", error: "refused" });
      }
      saved = input.dismissed ?? [];
      return issueState(saved);
    };

    unmount = render(tab({ "CORE-1": issueState() }, true, queryClient)).unmount;
    await screen.findByText("Body of message-2");
    const pins = screen.getAllByRole("button", { name: "Pin" });
    expect(pins.map((button) => button.getAttribute("aria-pressed"))).toEqual(["false", "false"]);
    for (const button of pins) {
      await user.click(button);
    }

    const writes = writesOf(queryClient);
    await waitFor(() =>
      expect(writes.rows().map((write) => write.status)).toEqual(["refused", "refused"])
    );
    expect(
      screen
        .getAllByRole("button", { name: /Pin|Unpin/ })
        .every((button) => !button.hasAttribute("disabled"))
    ).toBe(true);
    for (const write of writes.rows()) act(() => writes.retry(write.id));
    await waitFor(() => expect(writes.rows()).toEqual([]));
    // A card built from `/messages` has no event id at all: it pins under its own id.
    expect([...saved].sort()).toEqual([
      "pinned_items:message:message-1",
      "pinned_items:message:message-2",
    ]);
  } finally {
    unmount?.();
    api.getMyState = originalGetMyState;
    api.putIssueState = originalPutIssueState;
    stub.restore();
  }
});

test("a scroll event measures the reader position in O(1) rect reads", async () => {
  const stub = stubLists({
    messages: Array.from({ length: 250 }, (_, index) =>
      thread(
        message(`message-${index}`, {
          body: `Message ${index}`,
          created_at: `2026-09-09T00:00:${String(index % 60).padStart(2, "0")}Z`,
        })
      )
    ),
  });
  const rectSpy = spyOn(Element.prototype, "getBoundingClientRect").mockReturnValue({
    bottom: 10,
    height: 10,
    left: 0,
    right: 10,
    toJSON: () => ({}),
    top: 0,
    width: 10,
    x: 0,
    y: 0,
  });
  const elementFromPointSpy = spyOn(document, "elementFromPoint").mockImplementation(() =>
    document.querySelector("[data-turn]")
  );
  const queryClient = newQueryClient();
  queryClient.setQueryData<UserState>(["user-state"], { "CORE-1": issueState() });
  let unmount: (() => void) | undefined;

  try {
    unmount = render(tab({ "CORE-1": issueState() }, true, queryClient)).unmount;
    await screen.findByText("Message 0");

    rectSpy.mockClear();
    // The scroll listener sets the follow state, as a reader's scroll does, so the event goes
    // inside `act` like every other scroll of this file.
    act(() => {
      window.dispatchEvent(new Event("scroll"));
    });

    // ViewportAnchor measures only in the commit phase, immediately before a content reflow; a
    // scroll alone must do no layout reads. This guards both against O(n) scans over every card
    // and unnecessary per-scroll layout work.
    expect(rectSpy.mock.calls.length).toBe(0);
  } finally {
    unmount?.();
    rectSpy.mockRestore();
    elementFromPointSpy.mockRestore();
    stub.restore();
  }
});

test("agent authors render the registry title and fall back to a short id", async () => {
  const stub = stubLists({
    agents: [agent("0123456789abcdef", "Planner")],
    messages: [
      thread(
        message("message-1", {
          author: { id: "0123456789abcdef", kind: "session" },
          created_at: "2026-09-09T00:00:00Z",
        })
      ),
      thread(
        message("message-2", {
          author: { id: "fedcba9876543210", kind: "session" },
          created_at: "2026-09-09T00:05:00Z",
        })
      ),
    ],
  });
  const queryClient = newQueryClient();
  const view = render(tab({ "CORE-1": issueState() }, true, queryClient));

  try {
    const turns = await screen.findByRole("list", { name: "Conversation turns" });
    await within(turns).findByText("Planner");
    expect(within(turns).getByText("session:fedcba98…")).toBeTruthy();
  } finally {
    view.unmount();
    stub.restore();
  }
});

// Each issue a status change's option moves or unlinks, and each issue R25's fix changes, gets a
// comment written as Dispatch's system actor: no person and no session. Its thread names it as
// `AgentName` names every actor that is not a session, by its id, and reads no registry for it.
test("a comment from Dispatch's system actor shows its author", async () => {
  const user = userEvent.setup();
  const body =
    "Dispatch moved this issue from `todo` to `done`, because alice's status change of its parent passed `include_children`.";
  const stub = stubLists({
    comments: [
      comment("comment-1", { author: { id: "parent-status-rule", kind: "system" }, body }),
    ],
  });
  const queryClient = newQueryClient();
  const view = render(tab({ "CORE-1": issueState() }, true, queryClient));

  try {
    const turns = await screen.findByRole("list", { name: "Conversation turns" });
    const turn = (await within(turns).findByText(/Dispatch moved this issue/)).closest<HTMLElement>(
      "[data-turn]"
    );
    if (turn === null) throw new Error("the comment is drawn in no turn");
    // A thread names its comments' authors once it is open.
    await user.click(within(turn).getByRole("button", { name: "Expand thread" }));
    expect(await within(turn).findByText(/^parent-status-rule ·/)).toBeInTheDocument();
  } finally {
    view.unmount();
    stub.restore();
  }
});

// A closed issue takes a message's retry and replies, so its card offers them as an open one does.
test("offers targeted-message retries and Reply on a closed issue", async () => {
  const stub = stubLists({
    messages: [
      thread(
        message("message-1", {
          deliveries: [attempt({ error: "no live session s1", state: "failed" })],
          target: "session:s1",
        })
      ),
    ],
  });
  const queryClient = newQueryClient();
  const view = render(tab({ "CORE-1": issueState() }, true, queryClient, true));

  try {
    await screen.findByText(/Failed: no live session s1/);
    expect(screen.getByRole("button", { name: "Retry" })).toBeTruthy();
    const turns = screen.getByRole("list", { name: "Conversation turns" });
    expect(within(turns).getByRole("button", { name: "Reply" })).toBeTruthy();
  } finally {
    view.unmount();
    stub.restore();
  }
});

// A failed attempt that carries an envelope id was failed by the session's own error reply
// after Dispatch recorded the send (`rowAnsweredWithError`); that attempt gets a mode change,
// not a same-mode Retry whose repeated key the stream has already stored.
test("a targeted message its session answered with an error offers a mode change, not Retry", async () => {
  const stub = stubLists({
    messages: [
      thread(
        message("message-1", {
          deliveries: [
            attempt({ envelope_id: "envelope-1", error: "side turn failed", state: "failed" }),
          ],
          target: "session:s1",
        })
      ),
    ],
  });
  const queryClient = newQueryClient();
  const view = render(tab({ "CORE-1": issueState() }, true, queryClient));

  try {
    await screen.findByText(`Failed: side turn failed. ${answeredWithErrorGuidance("card")}`);
    expect(screen.queryByRole("button", { name: "Retry" })).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Use Send instead" })).toBeTruthy();
  } finally {
    view.unmount();
    stub.restore();
  }
});

// A mention's row carries the same mark. The mention list has no mode-change action, so the
// attempt gets no Retry and is pointed at a new comment; the failure Dispatch recorded for the
// same attempt keeps its Retry.
test("a mention its session answered with an error is pointed at a new comment, not Retry", async () => {
  const mentioned = (id: string, session: string, deliveries: CommentDelivery[]): Comment =>
    comment(id, {
      body: `@${session} take a look`,
      deliveries,
      mentions: [{ delivery: "btw", session_id: session, target: `session:${session}` }],
    });
  const stub = stubLists({
    comments: [
      mentioned("comment-1", "worker", [
        mentionAttempt({
          comment_id: "comment-1",
          envelope_id: "envelope-1",
          error: "worker failed",
          session_id: "worker",
          target: "session:worker",
        }),
      ]),
      mentioned("comment-2", "reviewer", [
        mentionAttempt({
          comment_id: "comment-2",
          error: "reviewer failed",
          session_id: "reviewer",
          target: "session:reviewer",
        }),
      ]),
    ],
  });
  const queryClient = newQueryClient();
  let view = render(tab({ "CORE-1": issueState() }, true, queryClient));

  try {
    // The recipient is the session the attempt recorded, drawn by `AgentName` (an unlisted
    // session reads by its short id) with its live link.
    await screen.findByText(
      wholeText(`session:worker… · failed · worker failed. ${answeredWithErrorGuidance("mention")}`)
    );
    expect(document.querySelector('[data-agent-name="worker"] a')?.getAttribute("href")).toBe(
      "/agents/worker/live"
    );
    screen.getByText(
      wholeText(
        `session:reviewer… · failed · reviewer failed. ${safeRetryGuidance("mention", "reviewer failed")}`
      )
    );
    expect(screen.getAllByRole("button", { name: "Retry" })).toHaveLength(1);
    view.unmount();

    // A closed issue takes a retry and a new comment, so it says both sentences, with the Retry,
    // as an open one does.
    view = render(tab({ "CORE-1": issueState() }, true, queryClient, true));
    await screen.findByText(
      wholeText(`session:worker… · failed · worker failed. ${answeredWithErrorGuidance("mention")}`)
    );
    screen.getByText(
      wholeText(
        `session:reviewer… · failed · reviewer failed. ${safeRetryGuidance("mention", "reviewer failed")}`
      )
    );
    expect(screen.getAllByRole("button", { name: "Retry" })).toHaveLength(1);
  } finally {
    view.unmount();
    stub.restore();
  }
});

// A delete ends each send of its post still pending, with its own words, and Dispatch refuses
// every new delivery of a deleted post (409 POST_DELETED). So the attempt a comment's delete
// ended reads the delete's words alone, never that it failed, with no Retry, while a standing
// comment's failed mention still reads as failed and keeps its Retry; and a deleted targeted
// message's card says the delete ended it and offers neither Retry nor a mode change.
test("an attempt a post's delete ended offers no Retry or mode change, and a standing post's still does", async () => {
  const deletion = { deleted_at: "2026-09-20T00:05:00Z", deleted_by: alice };
  const stub = stubLists({
    comments: [
      comment("comment-1", {
        ...deletion,
        body: "",
        deliveries: [
          mentionAttempt({
            comment_id: "comment-1",
            error: DELETED_POST_DELIVERY_ERROR,
            session_id: "worker",
            target: "session:worker",
          }),
        ],
        mentions: [{ delivery: "btw", session_id: "worker", target: "session:worker" }],
      }),
      comment("comment-2", {
        body: "@reviewer take a look",
        deliveries: [
          mentionAttempt({
            comment_id: "comment-2",
            error: "reviewer failed",
            session_id: "reviewer",
            target: "session:reviewer",
          }),
        ],
        mentions: [{ delivery: "btw", session_id: "reviewer", target: "session:reviewer" }],
      }),
    ],
    messages: [
      thread(
        message("message-1", {
          ...deletion,
          body: "",
          deliveries: [attempt({ error: DELETED_POST_DELIVERY_ERROR, state: "failed" })],
          target: "session:s1",
        })
      ),
    ],
  });
  const queryClient = newQueryClient();
  const view = render(tab({ "CORE-1": issueState() }, true, queryClient));

  try {
    await screen.findByText(wholeText("session:worker… · Ended by the post's delete"));
    expect(
      screen.queryByText(wholeText(`session:worker… · failed · ${DELETED_POST_DELIVERY_ERROR}`))
    ).toBeNull();
    screen.getByText(
      wholeText(
        `session:reviewer… · failed · reviewer failed. ${safeRetryGuidance("mention", "reviewer failed")}`
      )
    );
    const card = (await screen.findByText("Ended by the post's delete")).closest(
      "[data-turn]"
    ) as HTMLElement;
    expect(within(card).queryByRole("button", { name: "Retry" })).not.toBeInTheDocument();
    expect(within(card).queryByRole("button", { name: /instead/ })).not.toBeInTheDocument();
    // The one Retry on the page is the standing comment's.
    expect(screen.getAllByRole("button", { name: "Retry" })).toHaveLength(1);
  } finally {
    view.unmount();
    stub.restore();
  }
});

test("a targeted message asked by a session names that session as the asker", async () => {
  const asker = { id: "s2", kind: "session", origin: { session_title: "reviewer" } } as const;
  const stub = stubLists({
    agents: [agent("s1", "planner")],
    messages: [
      thread(
        message("message-2", {
          author: asker,
          body: "Is the build green?",
          deliveries: [attempt({ message_id: "message-2" })],
          target: "session:s1",
        })
      ),
    ],
  });
  const queryClient = newQueryClient();
  const view = render(tab({ "CORE-1": issueState() }, true, queryClient));

  try {
    // The recipient is the session the attempt reached, by its registry title, and links to it.
    // The headline comes first; the retry row's sentences name the same session.
    const [recipient] = await screen.findAllByRole("link", { name: "planner" });
    expect(recipient?.getAttribute("href")).toBe("/agents/s1/live");
    expect(recipient?.closest("p")?.textContent?.startsWith("Asking planner (BTW) ·")).toBe(true);
    expect(screen.getByRole("link", { name: "reviewer" })).toHaveAttribute(
      "href",
      "/agents/s2/live"
    );
    expect(screen.queryByText("alice")).not.toBeInTheDocument();
  } finally {
    view.unmount();
    stub.restore();
  }
});

// A closed issue takes comments and message replies, so its Conversation keeps the composer.
test("renders the message composer on an open issue and a closed one alike", async () => {
  const stub = stubLists({ messages: [thread(message("message-1"))] });
  const queryClient = newQueryClient();
  const view = render(tab({ "CORE-1": issueState() }, true, queryClient));

  try {
    await screen.findByRole("form", { name: "Comment composer" });
    view.rerender(tab({ "CORE-1": issueState() }, true, queryClient, true));
    expect(screen.getByRole("form", { name: "Comment composer" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Send" })).toBeTruthy();
  } finally {
    view.unmount();
    stub.restore();
  }
});

test("renders an anchored comment with the owner controls in Conversation", async () => {
  const user = userEvent.setup();
  const stub = stubLists({
    comments: [
      comment("comment-1", {
        anchor: {
          artifact_id: "artifact-secondary",
          block_id: null,
          mark_id: "mark-1",
          orphaned: true,
          quote: "Anchored source",
          version: 1,
        },
        body: "Please revise this.",
      }),
    ],
  });
  const queryClient = newQueryClient();
  queryClient.setQueryData(["whoami"], { login: "alice" });
  const view = render(
    tab(
      { "CORE-1": issueState() },
      true,
      queryClient,
      false,
      new Map([["artifact-secondary", secondaryDocument]])
    )
  );

  try {
    await screen.findByText("Please revise this.");
    const card = screen.getByTestId("margin-comment-comment-1");
    expect(card).toHaveTextContent("Anchored source");
    await user.click(screen.getByRole("button", { name: "Expand thread" }));
    expect(screen.getByRole("button", { name: "Edit" })).toBeTruthy();
    expect(
      screen.getAllByRole("button", {
        name: "Copy reference dispatch://CORE-1/comment/comment-1",
      })
    ).toHaveLength(1);
    expect(screen.getByRole("link", { name: "View in document" })).toHaveAttribute(
      "href",
      "/issues/CORE-1/artifacts/supporting-document?comment=comment-1"
    );
    expect(screen.getByRole("link", { name: "View original text" })).toHaveAttribute(
      "href",
      "/issues/CORE-1/artifacts/supporting-document?v=1&comment=comment-1"
    );
  } finally {
    view.unmount();
    stub.restore();
  }
});

test("a comment on the primary document links to the Spec route, not its artifact route", async () => {
  // The link is built by `documentItemPath`, the one builder that knows a primary document is
  // the issue's Spec tab; the hand-assembled href it replaced always named the artifact route.
  const stub = stubLists({
    comments: [
      comment("comment-1", {
        anchor: {
          artifact_id: "artifact-primary",
          block_id: null,
          mark_id: "mark-1",
          orphaned: false,
          quote: "Anchored source",
          version: 1,
        },
        body: "Please revise this.",
      }),
    ],
  });
  const queryClient = newQueryClient();
  queryClient.setQueryData(["whoami"], { login: "alice" });
  const view = render(
    tab(
      { "CORE-1": issueState() },
      true,
      queryClient,
      false,
      new Map([["artifact-primary", primaryDocument]])
    )
  );

  try {
    await screen.findByText("Please revise this.");
    expect(screen.getByRole("link", { name: "View in document" })).toHaveAttribute(
      "href",
      "/issues/CORE-1/spec?comment=comment-1"
    );
  } finally {
    view.unmount();
    stub.restore();
  }
});

test("the filter opens on All, and Open and Closed partition the list", async () => {
  const user = userEvent.setup();
  const stub = stubLists({
    asks: [
      askRow({
        answer: { at: "2026-09-10T10:00:00Z", selected: ["Ship"], text: null, user: "bob" },
        state: "answered",
      }),
    ],
    comments: [comment("comment-1", { body: "An open thread" })],
  });
  const queryClient = newQueryClient();
  const view = render(tab({ "CORE-1": issueState() }, true, queryClient));

  try {
    await screen.findByText("An open thread");
    expect(screen.getByRole("button", { name: "All" })).toHaveAttribute("aria-pressed", "true");
    expect(screen.getByText("Ship it?")).toBeTruthy();

    await user.click(screen.getByRole("button", { name: "Open" }));
    expect(screen.queryByText("Ship it?")).not.toBeInTheDocument();
    expect(screen.getByText("An open thread")).toBeTruthy();

    await user.click(screen.getByRole("button", { name: "Closed" }));
    expect(screen.getByText("Ship it?")).toBeTruthy();
    expect(screen.queryByText("An open thread")).not.toBeInTheDocument();

    // Not persisted: a reader who filtered on an earlier visit meets the whole conversation.
    view.unmount();
    const revisit = render(tab({ "CORE-1": issueState() }, true, queryClient));
    try {
      await screen.findByText("An open thread");
      expect(screen.getByRole("button", { name: "All" })).toHaveAttribute("aria-pressed", "true");
      expect(screen.getByText("Ship it?")).toBeTruthy();
    } finally {
      revisit.unmount();
    }
  } finally {
    stub.restore();
  }
});

test("a retracted ask is listed, and landing on one resets a filter that hides it", async () => {
  const user = userEvent.setup();
  const retracted = askRow({
    id: "ask-retracted",
    resolution: {
      actor: { id: "s1", kind: "session" },
      at: "2026-09-10T09:10:00Z",
      kind: "retracted",
      reason: "Duplicate of the spec's own decision.",
    },
    state: "resolved",
  });
  const stub = stubLists({
    asks: [retracted],
    comments: [comment("comment-1", { body: "An open thread" })],
  });
  const queryClient = newQueryClient();
  const scrollIntoView = stubScrollIntoView();
  const view = render(tab({ "CORE-1": issueState() }, true, queryClient));

  try {
    await screen.findByText("Ship it?");
    await user.click(screen.getByRole("button", { name: "Open" }));
    expect(screen.queryByText("Ship it?")).not.toBeInTheDocument();

    view.rerender(
      tab({ "CORE-1": issueState() }, true, queryClient, false, new Map(), "ask-retracted")
    );
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "All" })).toHaveAttribute("aria-pressed", "true")
    );
    const card = view.container.querySelector('[data-turn="ask:ask-retracted"]');
    expect(card?.getAttribute("aria-current")).toBe("true");
  } finally {
    view.unmount();
    scrollIntoView.mockRestore();
    stub.restore();
  }
});

function handoffRecord(id: string, version: number, body: string, created_at: string): Handoff {
  return {
    author: { id: "fedcba9876543210", kind: "session", origin: { session_title: "Implementer" } },
    body,
    created_at,
    id,
    issue_key: "CORE-1",
    phase: "implement",
    version,
  };
}

test("each handoff version is a card naming its phase, version and author, and expanding one reads its fields", async () => {
  const user = userEvent.setup();
  const stub = stubLists({
    handoffs: [
      handoffRecord(
        "handoff-2",
        2,
        JSON.stringify({
          schemaVersion: 1,
          summary: "Second pass",
          files: ["a.ts"],
          proof: ["bun test passed", { command: "bun run typecheck", exit: 0 }],
        }),
        "2026-09-10T09:05:00Z"
      ),
      handoffRecord("handoff-1", 1, '{"summary":"First pass"}', "2026-09-10T09:00:00Z"),
    ],
  });
  const getHandoff = api.getHandoff as Mock<typeof api.getHandoff>;
  const queryClient = newQueryClient();
  const view = render(tab({ "CORE-1": issueState() }, true, queryClient));

  try {
    const turns = await screen.findByRole("list", { name: "Conversation turns" });
    await within(turns).findByText("implement handoff, version 2");
    expect(within(turns).getByText("implement handoff, version 1")).toBeTruthy();
    // The session's stamped title names it when the registry has no live one.
    expect(within(turns).getAllByText("Implementer")).toHaveLength(2);
    // The body is read only once the reader asks for it.
    expect(getHandoff).not.toHaveBeenCalled();

    const card = view.container.querySelector<HTMLElement>('[data-turn="handoff:handoff-2"]');
    if (card === null) throw new Error("expected the version 2 card");
    const toggle = within(card).getByRole("button", { name: "Show fields" });
    expect(toggle).toHaveAttribute("aria-expanded", "false");
    await user.click(toggle);

    await within(card).findByText("Second pass");
    expect(getHandoff.mock.calls).toEqual([["handoff-2"]]);
    expect(toggle).toHaveAttribute("aria-expanded", "true");
    const fields = document.getElementById(toggle.getAttribute("aria-controls") ?? "");
    expect(fields?.tagName).toBe("DL");
    const rows = [...card.querySelectorAll("[data-handoff-field]")].map((row) => [
      row.querySelector("dt")?.textContent,
      row.querySelector("dd")?.textContent,
    ]);
    expect(rows).toEqual([
      ["schemaVersion", "1"],
      ["summary", "Second pass"],
      ["files", '[\n  "a.ts"\n]'],
      ["proof 1", "bun test passed"],
      ["proof 2", '{\n  "command": "bun run typecheck",\n  "exit": 0\n}'],
    ]);
    expect(card.querySelectorAll('[data-handoff-field="proof"]')).toHaveLength(2);
    // Arrays and objects are indented JSON; strings and numbers are text.
    expect(card.querySelector('[data-handoff-field="files"] pre')).not.toBeNull();
    expect(card.querySelector('[data-handoff-field="summary"] pre')).toBeNull();

    await user.click(within(card).getByRole("button", { name: "Hide fields" }));
    expect(within(card).queryByText("Second pass")).not.toBeInTheDocument();
    expect(within(card).getByRole("button", { name: "Show fields" })).toBeTruthy();
  } finally {
    view.unmount();
    stub.restore();
  }
});

test("a handoff body that is not a JSON object is drawn whole by the renderer", async () => {
  const user = userEvent.setup();
  const stub = stubLists({
    handoffs: [handoffRecord("handoff-1", 1, "not { json", "2026-09-10T09:00:00Z")],
  });
  const queryClient = newQueryClient();
  const view = render(tab({ "CORE-1": issueState() }, true, queryClient));

  try {
    await user.click(await screen.findByRole("button", { name: "Show fields" }));
    const raw = await screen.findByText("not { json");
    expect(raw.closest("[data-handoff-raw]")).not.toBeNull();
    expect(view.container.querySelector("[data-handoff-field]")).toBeNull();
  } finally {
    view.unmount();
    stub.restore();
  }
});

// The words a handoff's session wrote are drawn by the renderer from the reading the server
// sends for each (`field_trees`, keyed by the text's JSON Pointer into the body - `/summary` for a
// field, `/proof/<n>` for a proof entry - and `` for a body that is no JSON object), so a
// reference in them reads as its target's name and the page parses none of them: the tree here
// reads `**Second** pass` where the field says `Second pass`. A field of JSON stays the JSON it
// is (`conversation-handoff-data`).
test("a handoff's words are drawn from the trees the server sends, its JSON as JSON", async () => {
  await warmMarkdownRenderer();
  const user = userEvent.setup();
  const record = {
    ...handoffRecord(
      "handoff-1",
      1,
      JSON.stringify({ files: ["a.ts"], proof: ["CI is green"], summary: "Second pass" }),
      "2026-09-10T09:00:00Z"
    ),
    field_trees: {
      "/proof/0": displayTree("CI is *green*") as DisplayTree,
      "/summary": displayTree("**Second** pass") as DisplayTree,
    },
  };
  const stub = stubLists({ handoffs: [record] });
  const queryClient = newQueryClient();
  const view = render(tab({ "CORE-1": issueState() }, true, queryClient));

  try {
    await user.click(await screen.findByRole("button", { name: "Show fields" }));
    const summary = await waitFor(() => {
      const found = view.container.querySelector('[data-handoff-field="summary"] strong');
      if (found === null) throw new Error("the summary is not drawn from its tree yet");
      return found;
    });
    expect(summary.textContent).toBe("Second");
    expect(view.container.querySelector('[data-handoff-field="proof"] em')?.textContent).toBe(
      "green"
    );
    expect(view.container.querySelector('[data-handoff-field="files"] pre')?.textContent).toBe(
      '[\n  "a.ts"\n]'
    );
  } finally {
    view.unmount();
    stub.restore();
  }
});

// A field named `proof.0` and the first entry of `proof` are two texts with two keys
// (`/proof.0`, `/proof/0`), so each draws its own tree: here a word in strong type for the one
// and in emphasis for the other, which their own words write neither of.
test("a handoff field named like a proof entry and the entry draw their own trees", async () => {
  await warmMarkdownRenderer();
  const user = userEvent.setup();
  const record = {
    ...handoffRecord(
      "handoff-1",
      1,
      JSON.stringify({ "proof.0": "x one", proof: ["y two"] }),
      "2026-09-10T09:00:00Z"
    ),
    field_trees: {
      "/proof.0": displayTree("**x** one") as DisplayTree,
      "/proof/0": displayTree("*y* two") as DisplayTree,
    },
  };
  const stub = stubLists({ handoffs: [record] });
  const queryClient = newQueryClient();
  const view = render(tab({ "CORE-1": issueState() }, true, queryClient));

  try {
    await user.click(await screen.findByRole("button", { name: "Show fields" }));
    const field = await waitFor(() => {
      const found = view.container.querySelector('[data-handoff-field="proof.0"] strong');
      if (found === null) throw new Error("the field is not drawn from its tree yet");
      return found;
    });
    expect(field.textContent).toBe("x");
    expect(view.container.querySelector('[data-handoff-field="proof.0"] em')).toBeNull();
    const entry = view.container.querySelector('[data-handoff-field="proof"] em');
    expect(entry?.textContent).toBe("y");
    expect(view.container.querySelector('[data-handoff-field="proof"] strong')).toBeNull();
  } finally {
    view.unmount();
    stub.restore();
  }
});

// A tree is looked up among the answer's own keys alone: a field named after a member every
// object inherits - `constructor`, `toString` - whose tree the answer does not hold (an empty
// string has none) reads no such member as its tree, and is drawn from its own words.
test("a handoff field named after an inherited member reads no member as its tree", async () => {
  await warmMarkdownRenderer();
  const user = userEvent.setup();
  const record = {
    ...handoffRecord(
      "handoff-1",
      1,
      JSON.stringify({ constructor: "**bold** words", toString: "" }),
      "2026-09-10T09:00:00Z"
    ),
    field_trees: {},
  };
  const stub = stubLists({ handoffs: [record] });
  const queryClient = newQueryClient();
  const view = render(tab({ "CORE-1": issueState() }, true, queryClient));

  try {
    await user.click(await screen.findByRole("button", { name: "Show fields" }));
    const words = await waitFor(() => {
      const found = view.container.querySelector('[data-handoff-field="constructor"] strong');
      if (found === null) throw new Error("the field is not drawn from its words yet");
      return found;
    });
    expect(words.textContent).toBe("bold");
    expect(view.container.querySelector('[data-handoff-field="toString"] dd')?.textContent).toBe(
      ""
    );
  } finally {
    view.unmount();
    stub.restore();
  }
});

test("a handoff is Closed, and landing on one resets a filter that hides it", async () => {
  const user = userEvent.setup();
  const stub = stubLists({
    comments: [comment("comment-1", { body: "An open thread" })],
    handoffs: [handoffRecord("handoff-1", 1, "{}", "2026-09-10T09:00:00Z")],
  });
  const queryClient = newQueryClient();
  const scrollIntoView = stubScrollIntoView();
  const view = render(tab({ "CORE-1": issueState() }, true, queryClient));

  try {
    await screen.findByText("implement handoff, version 1");
    await user.click(screen.getByRole("button", { name: "Closed" }));
    expect(screen.getByText("implement handoff, version 1")).toBeTruthy();
    expect(screen.queryByText("An open thread")).not.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Open" }));
    expect(screen.queryByText("implement handoff, version 1")).not.toBeInTheDocument();

    view.rerender(
      tab({ "CORE-1": issueState() }, true, queryClient, false, new Map(), "handoff-1")
    );
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "All" }).getAttribute("aria-pressed")).toBe("true")
    );
    const card = view.container.querySelector('[data-turn="handoff:handoff-1"]');
    expect(card?.getAttribute("aria-current")).toBe("true");
  } finally {
    view.unmount();
    scrollIntoView.mockRestore();
    stub.restore();
  }
});

test("landing on a reply under an ask marks that ask's card and the reply's own row", async () => {
  const replies = [
    comment("reply-1", {
      ask_id: "ask-1",
      body: "The oldest reply",
      created_at: "2026-09-10T09:01:00Z",
    }),
    comment("reply-2", {
      ask_id: "ask-1",
      body: "A later reply",
      created_at: "2026-09-10T09:02:00Z",
    }),
    comment("reply-3", {
      ask_id: "ask-1",
      body: "The newest reply",
      created_at: "2026-09-10T09:03:00Z",
    }),
  ];
  const getAsk = spyOn(api, "getAsk");
  const stub = stubLists({ asks: [askRow({}, replies)], comments: replies });
  const queryClient = newQueryClient();
  const scrollIntoView = stubScrollIntoView();
  const view = render(
    tab({ "CORE-1": issueState() }, true, queryClient, false, new Map(), "reply-1")
  );

  try {
    // The oldest reply sits behind the card's fold, which the landing opens.
    const marked = await screen.findByText("The oldest reply");
    const row = marked.closest("[data-reply]");
    expect(row?.getAttribute("aria-current")).toBe("true");
    expect(
      view.container.querySelector('[data-turn="ask:ask-1"]')?.getAttribute("aria-current")
    ).toBe("true");
    // The row's own thread seeds the card, so the page makes no per-ask thread request.
    expect(getAsk).not.toHaveBeenCalled();
  } finally {
    view.unmount();
    getAsk.mockRestore();
    scrollIntoView.mockRestore();
    stub.restore();
  }
});

// A thread the reader has open is in their hand: resolving its comment leaves the card, its open
// thread and the reply they are writing where they are, whatever the filter would do with it,
// until they close the thread.
test("under Open, a comment resolved while its thread is open stays until the thread closes", async () => {
  const user = userEvent.setup();
  const root = comment("root-comment", { body: "Root comment" });
  const stub = stubLists({ comments: [root] });
  const queryClient = newQueryClient();
  const view = render(tab({ "CORE-1": issueState() }, true, queryClient));

  try {
    await screen.findByText("Root comment");
    await user.click(screen.getByRole("button", { name: "Open" }));
    await user.click(screen.getByRole("button", { name: "Expand thread" }));
    const field = await screen.findByRole<HTMLTextAreaElement>("textbox", { name: "Reply" });
    await user.clear(field);
    await user.click(field);
    await user.paste("Half a reply");

    stub.lists.comments = [{ ...root, resolved: true }];
    await act(async () => {
      await queryClient.invalidateQueries({ queryKey: ["comments", "CORE-1"] });
    });
    expect(screen.getByText("Root comment")).toBeTruthy();
    expect(screen.getByRole<HTMLTextAreaElement>("textbox", { name: "Reply" })).toBe(field);
    expect(field.value).toBe("Half a reply");

    await user.click(screen.getByRole("button", { name: "Collapse thread" }));
    await waitFor(() => expect(screen.queryByText("Root comment")).not.toBeInTheDocument());
  } finally {
    view.unmount();
    stub.restore();
  }
});

// Resolve inside a thread closes it once the server takes it, never before: a refusal leaves the
// thread open with the reply the reader was writing, and the Retry that lands closes it.
test("a refused Resolve keeps the thread and its reply, and the one that lands closes it", async () => {
  const user = userEvent.setup();
  const root = comment("root-comment", { body: "Root comment" });
  const stub = stubLists({ comments: [root] });
  const queryClient = newQueryClient();
  let refuse = true;
  const resolves: string[] = [];
  const resolveComment = spyOn(api, "resolveComment").mockImplementation(async (id) => {
    resolves.push(id);
    if (refuse) throw new ApiError(409, { error: "the server is down" });
    return { ...root, resolved: true };
  });
  const view = render(tab({ "CORE-1": issueState() }, true, queryClient));

  try {
    await screen.findByText("Root comment");
    await user.click(screen.getByRole("button", { name: "Expand thread" }));
    const field = await screen.findByRole<HTMLTextAreaElement>("textbox", { name: "Reply" });
    await user.clear(field);
    await user.click(field);
    await user.paste("Half a reply");

    await user.click(screen.getByRole("button", { name: "Resolve" }));
    await screen.findByText("the server is down");
    expect(screen.getByRole<HTMLTextAreaElement>("textbox", { name: "Reply" })).toBe(field);
    expect(field.value).toBe("Half a reply");

    refuse = false;
    await user.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() =>
      expect(screen.queryByRole("textbox", { name: "Reply" })).not.toBeInTheDocument()
    );
    expect(resolves).toEqual(["root-comment", "root-comment"]);
  } finally {
    view.unmount();
    resolveComment.mockRestore();
    stub.restore();
  }
});

/** A tab whose one comment the reader resolves: `land` lets the server take the Resolve, after
 *  which the comments read lists the comment resolved, and `refuse` refuses the reply sent. */
function ownResolveOverAReply() {
  const root = comment("root-comment", { body: "Root comment" });
  const stub = stubLists({ comments: [root] });
  const resolution = Promise.withResolvers<void>();
  const reply = Promise.withResolvers<Comment>();
  const listComments = api.listComments as Mock<typeof api.listComments>;
  const spies = [
    spyOn(api, "createComment").mockImplementation(() => reply.promise),
    spyOn(api, "resolveComment").mockImplementation(async () => {
      await resolution.promise;
      stub.lists.comments = [{ ...root, resolved: true }];
      return { ...root, resolved: true };
    }),
  ];
  const queryClient = newQueryClient();
  const view = render(tab({ "CORE-1": issueState() }, true, queryClient));
  return {
    land: async () => {
      const reads = listComments.mock.calls.length;
      await act(async () => {
        resolution.resolve();
        await resolution.promise;
      });
      // The read after the Resolve lists the comment resolved, so only a hold keeps its thread.
      await waitFor(() => expect(listComments.mock.calls.length).toBeGreaterThan(reads));
    },
    refuse: async () => {
      await act(async () => {
        reply.reject(new ApiError(409, { code: "UNAVAILABLE", error: "the server is down" }));
        await reply.promise.catch(() => {});
      });
    },
    restore: () => {
      view.unmount();
      for (const spy of spies) spy.mockRestore();
      stub.restore();
    },
    turn: () => view.container.querySelector('[data-turn="comment:root-comment"]'),
  };
}

const refusedText = "Couldn't send — the server is down";

// A decision closes its thread once the server takes it, and the thread's reply stays the
// reader's: the refusal it holds, and the comment that reply answers, outlive the reader's own
// Resolve on a phone as they do at a desktop width.
test("on a phone, the reader's own Resolve keeps the thread composer's refused reply and its comment", async () => {
  const user = userEvent.setup();
  const restoreViewport = stubViewport(PHONE_WIDTH);
  const resolve = ownResolveOverAReply();

  try {
    await screen.findByText("Root comment");
    await user.click(screen.getByRole("button", { name: "Reply" }));
    const dialog = await screen.findByRole("dialog", { name: "Thread" });
    const field = within(dialog).getByRole<HTMLTextAreaElement>("textbox", { name: "Comment" });
    await user.clear(field);
    await user.click(field);
    await user.paste("Refused reply");
    await user.click(within(dialog).getByRole("button", { name: "Send" }));
    await resolve.refuse();
    await within(dialog).findByText(refusedText);

    await user.click(within(dialog).getByRole("button", { name: "Resolve" }));
    await resolve.land();
    expect(screen.queryByRole("dialog", { name: "Thread" })).not.toBeInTheDocument();
    const turn = resolve.turn();
    if (turn === null) throw new Error("expected the resolved comment to stay listed");

    await user.click(within(turn as HTMLElement).getByRole("button", { name: "Expand thread" }));
    const reopened = await screen.findByRole("dialog", { name: "Thread" });
    expect(within(reopened).getByText(refusedText)).toBeTruthy();
    expect(
      within(reopened).getByRole<HTMLTextAreaElement>("textbox", { name: "Comment" }).value
    ).toBe("Refused reply");
  } finally {
    resolve.restore();
    restoreViewport();
  }
});

// A phone's thread view scrolls its card in a list of its own, which Firefox makes a Tab stop of
// its own, with no name. The card holds its controls from its head to its foot, the Tab stops
// that bring each part into view, so the list takes no Tab stop.
test("on a phone, the thread view's list is no Tab stop of its own", async () => {
  const user = userEvent.setup();
  const restoreViewport = stubViewport(PHONE_WIDTH);
  const root = comment("root-comment", { body: "Root comment" });
  const reply = comment("reply-comment", { body: "Reply comment", reply_to: "root-comment" });
  const stub = stubLists({ comments: [root, reply] });
  const view = render(tab({ "CORE-1": issueState() }, true, newQueryClient()));

  try {
    await screen.findByText("Root comment");
    await user.click(screen.getByRole("button", { name: "Expand thread" }));
    const dialog = await screen.findByRole("dialog", { name: "Thread" });
    expect(within(dialog).getAllByRole("list")[0]).toHaveAttribute("tabindex", "-1");
  } finally {
    view.unmount();
    stub.restore();
    restoreViewport();
  }
});

// The same, with the reply refused while the Resolve is still out: the decision lands on a thread
// that already shows the refusal.
test("on a phone, a thread reply refused while the reader's Resolve is out outlives the Resolve", async () => {
  const user = userEvent.setup();
  const restoreViewport = stubViewport(PHONE_WIDTH);
  const resolve = ownResolveOverAReply();

  try {
    await screen.findByText("Root comment");
    await user.click(screen.getByRole("button", { name: "Reply" }));
    const dialog = await screen.findByRole("dialog", { name: "Thread" });
    const field = within(dialog).getByRole<HTMLTextAreaElement>("textbox", { name: "Comment" });
    await user.clear(field);
    await user.click(field);
    await user.paste("Refused reply");
    await user.click(within(dialog).getByRole("button", { name: "Send" }));
    await waitFor(() => expect(api.createComment).toHaveBeenCalledTimes(1));
    await user.click(within(dialog).getByRole("button", { name: "Resolve" }));
    await waitFor(() => expect(api.resolveComment).toHaveBeenCalledTimes(1));
    await resolve.refuse();
    await within(dialog).findByText(refusedText);

    await resolve.land();
    const turn = resolve.turn();
    if (turn === null) throw new Error("expected the resolved comment to stay listed");
    await user.click(within(turn as HTMLElement).getByRole("button", { name: "Expand thread" }));
    const reopened = await screen.findByRole("dialog", { name: "Thread" });
    expect(within(reopened).getByText(refusedText)).toBeTruthy();
    expect(
      within(reopened).getByRole<HTMLTextAreaElement>("textbox", { name: "Comment" }).value
    ).toBe("Refused reply");
  } finally {
    resolve.restore();
    restoreViewport();
  }
});

// The thread card's own reply, at a phone width and at a desktop one: the reader's own Resolve
// closes the thread and keeps the reply's refusal for the thread's return.
for (const width of ["phone", "desktop"] as const) {
  test(`at a ${width} width, the reader's own Resolve keeps the card's refused reply and its comment`, async () => {
    const user = userEvent.setup();
    const restoreViewport = width === "phone" ? stubViewport(PHONE_WIDTH) : () => {};
    const resolve = ownResolveOverAReply();

    try {
      await screen.findByText("Root comment");
      await user.click(screen.getByRole("button", { name: "Expand thread" }));
      const field = await screen.findByRole<HTMLTextAreaElement>("textbox", { name: "Reply" });
      const form = field.closest("form");
      if (form === null) throw new Error("expected the card's reply form");
      await user.clear(field);
      await user.click(field);
      await user.paste("Refused reply");
      await user.click(within(form).getByRole("button", { name: "Send" }));
      await resolve.refuse();
      await within(form).findByText(refusedText);

      await user.click(screen.getByRole("button", { name: "Resolve" }));
      await resolve.land();
      await waitFor(() =>
        expect(screen.queryByRole("textbox", { name: "Reply" })).not.toBeInTheDocument()
      );
      const turn = resolve.turn();
      if (turn === null) throw new Error("expected the resolved comment to stay listed");

      await user.click(within(turn as HTMLElement).getByRole("button", { name: "Expand thread" }));
      const reopened = await screen.findByRole<HTMLTextAreaElement>("textbox", { name: "Reply" });
      expect(reopened.value).toBe("Refused reply");
      expect(screen.getByText(refusedText)).toBeTruthy();
    } finally {
      resolve.restore();
      restoreViewport();
    }
  });
}

// The thread composer's refusal is its comment's. Left with Back while its send was out, it comes
// back in that comment's thread: another comment's Reply opens it there, rather than readdressing
// the composer, and its draft, to the other comment.
test("on a phone, another comment's Reply opens the thread whose reply was refused after Back", async () => {
  const user = userEvent.setup();
  const restoreViewport = stubViewport(PHONE_WIDTH);
  const stub = stubLists({
    comments: [
      comment("comment-a", { body: "Comment A", created_at: "2026-09-20T00:00:00Z" }),
      comment("comment-b", { body: "Comment B", created_at: "2026-09-20T00:01:00Z" }),
    ],
  });
  const reply = Promise.withResolvers<Comment>();
  const createComment = spyOn(api, "createComment").mockImplementation(() => reply.promise);
  const queryClient = newQueryClient();
  const view = render(tab({ "CORE-1": issueState() }, true, queryClient));
  const turn = (id: string) => {
    const element = view.container.querySelector<HTMLElement>(`[data-turn="comment:${id}"]`);
    if (element === null) throw new Error(`expected ${id}'s turn`);
    return element;
  };

  try {
    await screen.findByText("Comment A");
    await user.click(within(turn("comment-a")).getByRole("button", { name: "Reply" }));
    let dialog = await screen.findByRole("dialog", { name: "Thread" });
    const field = within(dialog).getByRole<HTMLTextAreaElement>("textbox", { name: "Comment" });
    await user.clear(field);
    await user.click(field);
    await user.paste("Reply meant for A");
    await user.click(within(dialog).getByRole("button", { name: "Send" }));
    await waitFor(() => expect(createComment).toHaveBeenCalledTimes(1));
    await user.click(within(dialog).getByRole("button", { name: "Back" }));
    await waitFor(() =>
      expect(screen.queryByRole("dialog", { name: "Thread" })).not.toBeInTheDocument()
    );
    await act(async () => {
      reply.reject(new ApiError(409, { code: "UNAVAILABLE", error: "the server is down" }));
      await reply.promise.catch(() => {});
    });

    await user.click(within(turn("comment-b")).getByRole("button", { name: "Reply" }));
    dialog = await screen.findByRole("dialog", { name: "Thread" });
    expect(dialog.querySelector('[data-turn="comment:comment-a"]')).not.toBeNull();
    expect(within(dialog).queryByText("Comment B")).not.toBeInTheDocument();
    expect(within(dialog).getByText(refusedText)).toBeTruthy();
    expect(
      within(dialog).getByRole<HTMLTextAreaElement>("textbox", { name: "Comment" }).value
    ).toBe("Reply meant for A");
    expect(createComment).toHaveBeenCalledTimes(1);
  } finally {
    view.unmount();
    createComment.mockRestore();
    stub.restore();
    restoreViewport();
  }
});

// At a desktop width a comment's Reply answers the same docked composer and send name the phone
// thread composer uses on a phone (`CommentTurn`'s `onReply` prop in `ConversationTab.tsx`), so
// the same hazard applies: a refusal the composer holds for one comment must redirect, not be
// silently overwritten, when the reader taps Reply on a different one.
test("at a desktop width, another comment's Reply redirects onto the one whose refusal it holds", async () => {
  const user = userEvent.setup();
  const stub = stubLists({
    comments: [
      comment("comment-a", { body: "Comment A", created_at: "2026-09-20T00:00:00Z" }),
      comment("comment-b", { body: "Comment B", created_at: "2026-09-20T00:01:00Z" }),
    ],
  });
  const reply = Promise.withResolvers<Comment>();
  const createComment = spyOn(api, "createComment").mockImplementation(() => reply.promise);
  const queryClient = newQueryClient();
  const view = render(tab({ "CORE-1": issueState() }, true, queryClient));
  const turn = (id: string) => {
    const element = view.container.querySelector<HTMLElement>(`[data-turn="comment:${id}"]`);
    if (element === null) throw new Error(`expected ${id}'s turn`);
    return element;
  };

  try {
    await screen.findByText("Comment A");
    await user.click(within(turn("comment-a")).getByRole("button", { name: "Reply" }));
    const composer = screen.getByRole("form", { name: "Comment composer" });
    const field = within(composer).getByRole<HTMLTextAreaElement>("textbox", { name: "Comment" });
    await user.clear(field);
    await user.click(field);
    await user.paste("Reply meant for A");
    await user.click(within(composer).getByRole("button", { name: "Send" }));
    await waitFor(() => expect(createComment).toHaveBeenCalledTimes(1));
    await act(async () => {
      reply.reject(new ApiError(409, { code: "UNAVAILABLE", error: "the server is down" }));
      await reply.promise.catch(() => {});
    });
    await within(composer).findByText(refusedText);

    await user.click(within(turn("comment-b")).getByRole("button", { name: "Reply" }));
    expect(within(composer).getByText(/Comment A/)).toBeTruthy();
    expect(within(composer).queryByText(/Comment B/)).not.toBeInTheDocument();
    expect(within(composer).getByText(refusedText)).toBeTruthy();
    expect(field.value).toBe("Reply meant for A");
    expect(createComment).toHaveBeenCalledTimes(1);
  } finally {
    view.unmount();
    createComment.mockRestore();
    stub.restore();
  }
});

// A comment's Reply prefills its mention as the mention picker names the session
// (`useAgentText().sessionText`): the link's words are its registry title, else the shortened
// `session:<id>` the picker shows, and its address names the session whole whatever the words
// read.
test("a comment's Reply names its session author's mention as the picker does", async () => {
  const user = userEvent.setup();
  const untitled = "01234567-89ab-7def-8000-000000000001";
  const stub = stubLists({
    agents: [agent("planner-session", "Planner")],
    comments: [
      comment("comment-a", {
        author: { id: untitled, kind: "session" },
        body: "Comment A",
        created_at: "2026-09-20T00:00:00Z",
      }),
      comment("comment-b", {
        author: {
          id: "planner-session",
          kind: "session",
          origin: { session_title: "Old title" },
        },
        body: "Comment B",
        created_at: "2026-09-20T00:01:00Z",
      }),
    ],
  });
  // The prefilled mention's pill asks once about the session the registry does not list.
  const known = spyOn(api, "getKnownSessions").mockResolvedValue({ sessions: [] });
  const queryClient = newQueryClient();
  const view = render(tab({ "CORE-1": issueState() }, true, queryClient));
  const turn = (id: string) => {
    const element = view.container.querySelector<HTMLElement>(`[data-turn="comment:${id}"]`);
    if (element === null) throw new Error(`expected ${id}'s turn`);
    return element;
  };

  try {
    await screen.findByText("Comment B");
    await waitFor(() => expect(queryClient.getQueryData(["agents"])).toBeDefined());
    const composer = screen.getByRole("form", { name: "Comment composer" });
    const field = within(composer).getByRole<HTMLTextAreaElement>("textbox", { name: "Comment" });
    await user.click(within(turn("comment-a")).getByRole("button", { name: "Reply" }));
    await waitFor(() =>
      expect(field.value).toBe(`[@session:01234567…](dispatch://agent/${untitled}) `)
    );
    await user.click(within(composer).getByRole("button", { name: "Cancel reply" }));
    await user.click(within(turn("comment-b")).getByRole("button", { name: "Reply" }));
    await waitFor(() => expect(field.value).toBe("[@Planner](dispatch://agent/planner-session) "));
  } finally {
    view.unmount();
    stub.restore();
    known.mockRestore();
  }
});

// A message's Reply answers the same docked composer at every width (`MessageTurn`'s `onReply`
// prop in `ConversationTab.tsx`, unconditional - not gated on phone viewport the way a
// comment's is), so the hazard, and the redirect that closes it, hold here too.
test("another message's Reply redirects onto the one whose refusal the docked composer holds", async () => {
  const user = userEvent.setup();
  const stub = stubLists({
    messages: [
      thread(message("message-1", { body: "Message A", created_at: "2026-09-09T00:00:00Z" })),
      thread(message("message-2", { body: "Message B", created_at: "2026-09-09T00:05:00Z" })),
    ],
  });
  const reply = Promise.withResolvers<Message>();
  const createMessage = spyOn(api, "createMessage").mockImplementation(() => reply.promise);
  const queryClient = newQueryClient();
  const view = render(tab({ "CORE-1": issueState() }, true, queryClient));
  const turn = (id: string) => {
    const element = view.container.querySelector<HTMLElement>(`[data-turn="message:${id}"]`);
    if (element === null) throw new Error(`expected ${id}'s turn`);
    return element;
  };

  try {
    await screen.findByText("Message A");
    await user.click(within(turn("message-1")).getByRole("button", { name: "Reply" }));
    const composer = screen.getByRole("form", { name: "Comment composer" });
    const field = within(composer).getByRole<HTMLTextAreaElement>("textbox", { name: "Comment" });
    await user.clear(field);
    await user.click(field);
    await user.paste("Reply meant for A");
    await user.click(within(composer).getByRole("button", { name: "Send" }));
    await waitFor(() => expect(createMessage).toHaveBeenCalledTimes(1));
    await act(async () => {
      reply.reject(new ApiError(409, { code: "UNAVAILABLE", error: "the server is down" }));
      await reply.promise.catch(() => {});
    });
    await within(composer).findByText(refusedText);

    await user.click(within(turn("message-2")).getByRole("button", { name: "Reply" }));
    expect(within(composer).getByText(/Message A/)).toBeTruthy();
    expect(within(composer).queryByText(/Message B/)).not.toBeInTheDocument();
    expect(within(composer).getByText(refusedText)).toBeTruthy();
    expect(field.value).toBe("Reply meant for A");
    expect(createMessage).toHaveBeenCalledTimes(1);
  } finally {
    view.unmount();
    createMessage.mockRestore();
    stub.restore();
  }
});

// The docked composer is one box for the issue, whose reply its Reply and Cancel reply move. Cancel
// reply is the reader's own move, so a reply Dispatch refused goes with it: the box shows the draft
// as a new comment, with its Retry at once, and that Retry posts it as one, a new write under a
// key of its own.
test("a reply the docked composer holds refused is posted as a new comment after Cancel reply", async () => {
  const user = userEvent.setup();
  const stub = stubLists({
    comments: [comment("comment-a", { body: "Comment A", created_at: "2026-09-20T00:00:00Z" })],
  });
  const createComment = spyOn(api, "createComment")
    .mockRejectedValueOnce(new ApiError(409, { code: "UNAVAILABLE", error: "the server is down" }))
    .mockResolvedValue(comment("comment-b"));
  const queryClient = newQueryClient();
  const view = render(tab({ "CORE-1": issueState() }, true, queryClient));
  const turn = () => {
    const element = view.container.querySelector<HTMLElement>('[data-turn="comment:comment-a"]');
    if (element === null) throw new Error("expected comment-a's turn");
    return element;
  };

  try {
    await screen.findByText("Comment A");
    await user.click(within(turn()).getByRole("button", { name: "Reply" }));
    const composer = screen.getByRole("form", { name: "Comment composer" });
    const field = within(composer).getByRole<HTMLTextAreaElement>("textbox", { name: "Comment" });
    await user.clear(field);
    await user.click(field);
    await user.paste("Reply meant for A");
    await user.click(within(composer).getByRole("button", { name: "Send" }));
    await within(composer).findByText(refusedText);

    await user.click(within(composer).getByRole("button", { name: "Cancel reply" }));
    expect(within(composer).queryByText(/Comment A/)).not.toBeInTheDocument();
    expect(field.value).toBe("Reply meant for A");
    expect(within(composer).queryByText(/This message was written/)).not.toBeInTheDocument();
    await user.click(within(composer).getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(createComment).toHaveBeenCalledTimes(2));
    expect(createComment.mock.calls.map(([, input]) => input)).toEqual([
      { body: "Reply meant for A", reply_to: "comment-a" },
      { body: "Reply meant for A" },
    ]);
    const [first, retried] = createComment.mock.calls.map(([, , bounds]) => bounds.key);
    expect(retried).not.toBe(first);
  } finally {
    view.unmount();
    createComment.mockRestore();
    stub.restore();
  }
});

/** The Conversation at a phone width over one comment, `Comment A`, whose comment writes
 *  `createComment` answers. */
function phoneOverCommentA(
  createComment: Mock<typeof api.createComment>,
  queryClient = newQueryClient()
) {
  const restoreViewport = stubViewport(PHONE_WIDTH);
  const stub = stubLists({
    comments: [comment("comment-a", { body: "Comment A", created_at: "2026-09-20T00:00:00Z" })],
  });
  const view = render(tab({ "CORE-1": issueState() }, true, queryClient));
  return {
    /** Answers the comments again without `Comment A`, which leaves the list. */
    dropCommentA: async () => {
      stub.lists.comments = [];
      await act(() => queryClient.invalidateQueries({ queryKey: ["comments", "CORE-1"] }));
      await waitFor(() =>
        expect(view.container.querySelector('[data-turn="comment:comment-a"]')).toBe(null)
      );
    },
    restore: () => {
      view.unmount();
      createComment.mockRestore();
      stub.restore();
      restoreViewport();
    },
    turn: () => {
      const element = view.container.querySelector<HTMLElement>('[data-turn="comment:comment-a"]');
      if (element === null) throw new Error("expected comment-a's turn");
      return element;
    },
  };
}

/** Opens the thread of `turn`'s comment with its Reply, and writes `body` in the thread
 *  composer. Answers the thread view. */
async function writeInThread(
  user: UserEvent,
  turn: HTMLElement,
  body: string
): Promise<HTMLElement> {
  await user.click(within(turn).getByRole("button", { name: "Reply" }));
  const dialog = await screen.findByRole("dialog", { name: "Thread" });
  const field = within(dialog).getByRole<HTMLTextAreaElement>("textbox", { name: "Comment" });
  await user.clear(field);
  await user.click(field);
  await user.paste(body);
  return dialog;
}

// On a phone a comment's Reply answers in its thread, by a composer of its own that Cancel reply
// ends. Cancel reply is the reader's own move to the issue's address all the same, so a reply
// Dispatch refused goes with it, as it goes with the docked composer's own Cancel reply: the
// docked composer the reader comes back to shows the text and the refusal, and its Retry posts a
// new comment under a key of its own.
test("on a phone, a thread reply refused is posted as a new comment from the docked composer after Cancel reply", async () => {
  const user = userEvent.setup();
  const createComment = spyOn(api, "createComment")
    .mockRejectedValueOnce(new ApiError(409, { code: "UNAVAILABLE", error: "the server is down" }))
    .mockResolvedValue(comment("comment-b"));
  const phone = phoneOverCommentA(createComment);

  try {
    await screen.findByText("Comment A");
    const dialog = await writeInThread(user, phone.turn(), "Reply meant for A");
    await user.click(within(dialog).getByRole("button", { name: "Send" }));
    await within(dialog).findByText(refusedText);

    await user.click(within(dialog).getByRole("button", { name: "Cancel reply" }));
    expect(within(dialog).queryByRole("textbox", { name: "Comment" })).not.toBeInTheDocument();
    await user.click(within(dialog).getByRole("button", { name: "Back" }));
    const composer = screen.getByRole("form", { name: "Comment composer" });
    const field = within(composer).getByRole<HTMLTextAreaElement>("textbox", { name: "Comment" });
    expect(field.value).toBe("Reply meant for A");
    expect(within(composer).getByText(refusedText)).toBeTruthy();
    expect(within(composer).queryByText(/Comment A/)).not.toBeInTheDocument();
    expect(within(composer).queryByText(/This message was written/)).not.toBeInTheDocument();
    expect(createComment).toHaveBeenCalledTimes(1);

    await user.click(within(composer).getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(createComment).toHaveBeenCalledTimes(2));
    expect(createComment.mock.calls.map(([, input]) => input)).toEqual([
      { body: "Reply meant for A", reply_to: "comment-a" },
      { body: "Reply meant for A" },
    ]);
    const [first, retried] = createComment.mock.calls.map(([, , bounds]) => bounds.key);
    expect(retried).not.toBe(first);
  } finally {
    phone.restore();
  }
});

// The draft goes with the reader's own move whether or not it was ever sent: a reply typed in the
// thread and cancelled is the docked composer's draft, and nothing goes until Send there.
test("on a phone, a thread reply never sent is the docked composer's draft after Cancel reply", async () => {
  const user = userEvent.setup();
  const createComment = spyOn(api, "createComment").mockResolvedValue(comment("comment-b"));
  const phone = phoneOverCommentA(createComment);

  try {
    await screen.findByText("Comment A");
    const dialog = await writeInThread(user, phone.turn(), "Reply never sent");

    await user.click(within(dialog).getByRole("button", { name: "Cancel reply" }));
    expect(within(dialog).queryByRole("textbox", { name: "Comment" })).not.toBeInTheDocument();
    await user.click(within(dialog).getByRole("button", { name: "Back" }));
    const composer = screen.getByRole("form", { name: "Comment composer" });
    const field = within(composer).getByRole<HTMLTextAreaElement>("textbox", { name: "Comment" });
    expect(field.value).toBe("Reply never sent");
    expect(within(composer).queryByText(/Comment A/)).not.toBeInTheDocument();
    expect(createComment).not.toHaveBeenCalled();

    await user.click(within(composer).getByRole("button", { name: "Send" }));
    await waitFor(() => expect(createComment).toHaveBeenCalledTimes(1));
    expect(createComment.mock.calls.map(([, input]) => input)).toEqual([
      { body: "Reply never sent" },
    ]);
  } finally {
    phone.restore();
  }
});

// A docked composer holding a draft of its own is never written over. The thread composer's
// Cancel reply then puts it away as it stands, its refusal with it: Back keeps both, the docked
// draft is as the reader left it, the docked composer says the reply is kept and how to go on with
// it, and the comment's Reply brings the thread's reply back and the line goes.
test("on a phone, Cancel reply over a docked draft of its own keeps both, and the comment's Reply brings the thread reply back", async () => {
  const user = userEvent.setup();
  const createComment = spyOn(api, "createComment").mockRejectedValueOnce(
    new ApiError(409, { code: "UNAVAILABLE", error: "the server is down" })
  );
  const phone = phoneOverCommentA(createComment);

  try {
    await screen.findByText("Comment A");
    const composer = screen.getByRole("form", { name: "Comment composer" });
    const docked = within(composer).getByRole<HTMLTextAreaElement>("textbox", { name: "Comment" });
    await user.click(docked);
    await user.paste("Docked draft");
    let dialog = await writeInThread(user, phone.turn(), "Reply meant for A");
    await user.click(within(dialog).getByRole("button", { name: "Send" }));
    await within(dialog).findByText(refusedText);

    await user.click(within(dialog).getByRole("button", { name: "Cancel reply" }));
    expect(within(dialog).queryByRole("textbox", { name: "Comment" })).not.toBeInTheDocument();
    await user.click(within(dialog).getByRole("button", { name: "Back" }));
    expect(screen.queryByRole("dialog", { name: "Thread" })).not.toBeInTheDocument();
    expect(docked.value).toBe("Docked draft");
    expect(within(composer).queryByText(refusedText)).not.toBeInTheDocument();
    // The thread covers the docked composer until Back, so this is the line's first time on
    // screen; a status the reader cannot see is no role the query finds.
    expect(within(composer).getByRole("status").textContent).toBe(
      "Your reply to the comment by alice is kept. Press Reply on that comment to go on with it."
    );

    await user.click(within(phone.turn()).getByRole("button", { name: "Reply" }));
    dialog = await screen.findByRole("dialog", { name: "Thread" });
    expect(
      within(dialog).getByRole<HTMLTextAreaElement>("textbox", { name: "Comment" }).value
    ).toBe("Reply meant for A");
    expect(within(dialog).getByText(refusedText)).toBeTruthy();
    expect(docked.value).toBe("Docked draft");
    expect(within(composer).queryByText(/is kept/)).not.toBeInTheDocument();
    expect(createComment).toHaveBeenCalledTimes(1);
  } finally {
    phone.restore();
  }
});

// Once the comment a reply put away answers leaves the list, the docked composer draws that reply
// itself, with Discard my draft (`parkedNotice`). The press takes the offer away, and with it the
// button that held the focus: the docked composer's text box takes the focus then, with no scroll
// and the caret at the end of the draft it holds, as a composer's field takes the focus its own
// offers leave (`handFocusOn`).
test("on a phone, Discard my draft on a reply put away whose comment left the list hands the focus to the docked composer's box, with no scroll and the caret at the end", async () => {
  const user = userEvent.setup();
  const createComment = spyOn(api, "createComment").mockResolvedValue(comment("comment-b"));
  const phone = phoneOverCommentA(createComment);

  try {
    await screen.findByText("Comment A");
    const composer = screen.getByRole("form", { name: "Comment composer" });
    const docked = within(composer).getByRole<HTMLTextAreaElement>("textbox", { name: "Comment" });
    await user.click(docked);
    await user.paste("Docked draft");
    const dialog = await writeInThread(user, phone.turn(), "Reply meant for A");
    await user.click(within(dialog).getByRole("button", { name: "Cancel reply" }));
    await user.click(within(dialog).getByRole("button", { name: "Back" }));
    await phone.dropCommentA();
    const discard = within(composer).getByRole("button", { name: "Discard my draft" });
    const focuses = spyOn(docked, "focus");

    try {
      await user.click(discard);
      expect({
        caret: [docked.selectionStart, docked.selectionEnd],
        focuses: focuses.mock.calls,
        inTheBox: document.activeElement === docked,
        offered: within(composer).queryByRole("button", { name: "Discard my draft" }) !== null,
        text: docked.value,
      }).toEqual({
        caret: ["Docked draft".length, "Docked draft".length],
        focuses: [[{ preventScroll: true }]],
        inTheBox: true,
        offered: false,
        text: "Docked draft",
      });
    } finally {
      focuses.mockRestore();
    }
  } finally {
    phone.restore();
  }
});

// On a phone the thread composer's Cancel reply and Escape end its reply and take the composer,
// with the focus it holds, out of the thread, which stays open. The focus goes to the turn whose
// Reply started the reply where the thread draws it: a Reply pressed in the list, which the
// thread covers, leaves no such turn in the thread, and the thread's Back takes the focus; the
// Reply of the thread's own turn leaves that turn (`giveBackFromPhoneReply`).
test("on a phone, the thread composer's Cancel reply and Escape give the focus to the turn whose Reply started the reply, else to the thread's Back", async () => {
  const user = userEvent.setup();
  const createComment = spyOn(api, "createComment").mockResolvedValue(comment("comment-b"));
  const phone = phoneOverCommentA(createComment);

  try {
    await screen.findByText("Comment A");
    const dialog = await writeInThread(user, phone.turn(), "Reply meant for A");
    await user.click(within(dialog).getByRole("button", { name: "Cancel reply" }));
    expect(within(dialog).queryByRole("textbox", { name: "Comment" })).not.toBeInTheDocument();
    expect(within(dialog).getByRole("button", { name: "Back" })).toHaveFocus();

    const turn = dialog.querySelector<HTMLElement>('[data-turn="comment:comment-a"]');
    if (turn === null) throw new Error("expected comment-a's turn in the thread");
    await user.click(within(turn).getByRole("button", { name: "Reply" }));
    const field = await within(dialog).findByRole("textbox", { name: "Comment" });
    await waitFor(() => expect(field).toHaveFocus());
    await user.keyboard("{Escape}");
    expect(within(dialog).queryByRole("textbox", { name: "Comment" })).not.toBeInTheDocument();
    expect(document.activeElement).toBe(turn);
    expect(createComment).not.toHaveBeenCalled();
  } finally {
    phone.restore();
  }
});

/** What a send the reader stopped says in its composer at its own address: Dispatch has not
 *  confirmed it, and its Retry asks again under its own key. */
const stoppedText = new SendStoppedError().message;

/** The phone thread composer's send name, and the one of `Comment A`'s thread card's own reply. */
const threadComposerKey: MutationKey = ["conversation-composer", "CORE-1", "phone-thread"];
const cardReplyKey = threadReplySendKey(["conversation-composer", "CORE-1"], "comment-a");

/** The reply to `Comment A` a stopped send answers (`stopReplySend`). */
const commentAReply = {
  actor: alice,
  excerpt: "Comment A",
  id: "comment-a",
  parentKind: "comment",
} as const;

// Back is a move of the host's, not one the reader makes with the composer's own controls, so it
// carries the thread composer's draft nowhere: the draft is held for the comment it answers, and
// that comment's Reply brings it back - its text, a refusal's note, and a stopped send's note
// that it may have been delivered. Nothing is sent meanwhile.
for (const held of ["typed", "refused", "stopped"] as const) {
  test(`on a phone, a thread reply ${heldDraftWords[held]} comes back with the comment's Reply after Back`, async () => {
    const user = userEvent.setup();
    const createComment = spyOn(api, "createComment").mockRejectedValue(
      new ApiError(409, { code: "UNAVAILABLE", error: "the server is down" })
    );
    const queryClient = newQueryClient();
    const body = `Reply ${heldDraftWords[held]}, then Back`;
    if (held === "stopped") {
      await stopReplySend(queryClient, createComment, threadComposerKey, body, commentAReply);
    }
    const phone = phoneOverCommentA(createComment, queryClient);
    const reply = async () => {
      await user.click(within(phone.turn()).getByRole("button", { name: "Reply" }));
      return screen.findByRole("dialog", { name: "Thread" });
    };

    try {
      await screen.findByText("Comment A");
      let dialog: HTMLElement;
      if (held === "stopped") {
        dialog = await reply();
        expect(within(dialog).getByText(stoppedText)).toBeTruthy();
      } else {
        dialog = await writeInThread(user, phone.turn(), body);
        if (held === "refused") {
          await user.click(within(dialog).getByRole("button", { name: "Send" }));
          await within(dialog).findByText(refusedText);
        }
      }
      const sent = createComment.mock.calls.length;
      await user.click(within(dialog).getByRole("button", { name: "Back" }));
      expect(screen.queryByRole("dialog", { name: "Thread" })).not.toBeInTheDocument();

      dialog = await reply();
      // The text and the note in one comparison, so a row that fails says which of them is gone.
      expect({
        refusal: within(dialog).queryByText(refusedText) !== null,
        stopped: within(dialog).queryByText(stoppedText) !== null,
        text: within(dialog).getByRole<HTMLTextAreaElement>("textbox", { name: "Comment" }).value,
      }).toEqual({ refusal: held === "refused", stopped: held === "stopped", text: body });
      expect(createComment).toHaveBeenCalledTimes(sent);
    } finally {
      phone.restore();
    }
  });
}

// The thread card's own reply lives in the thread view, which Back takes off the page: Back is the
// host's move all the same, so the card's draft is held for its thread, and opening the thread
// again brings it back, with a refusal's note or a stopped send's.
for (const held of ["typed", "refused", "stopped"] as const) {
  test(`on a phone, the thread card's own reply ${heldDraftWords[held]} comes back in its thread after Back`, async () => {
    const user = userEvent.setup();
    const createComment = spyOn(api, "createComment").mockRejectedValue(
      new ApiError(409, { code: "UNAVAILABLE", error: "the server is down" })
    );
    const queryClient = newQueryClient();
    const body = `Card reply ${heldDraftWords[held]}, then Back`;
    if (held === "stopped") {
      await stopReplySend(queryClient, createComment, cardReplyKey, body, commentAReply);
    }
    const phone = phoneOverCommentA(createComment, queryClient);
    const openThread = async () => {
      await user.click(within(phone.turn()).getByRole("button", { name: "Expand thread" }));
      return screen.findByRole("dialog", { name: "Thread" });
    };

    try {
      await screen.findByText("Comment A");
      let dialog = await openThread();
      const field = within(dialog).getByRole<HTMLTextAreaElement>("textbox", { name: "Reply" });
      if (held === "stopped") {
        expect(within(dialog).getByText(stoppedText)).toBeTruthy();
      } else {
        await user.click(field);
        await user.paste(body);
        if (held === "refused") {
          const form = field.closest("form");
          if (form === null) throw new Error("expected the card's reply form");
          await user.click(within(form).getByRole("button", { name: "Send" }));
          await within(dialog).findByText(refusedText);
        }
      }
      const sent = createComment.mock.calls.length;
      await user.click(within(dialog).getByRole("button", { name: "Back" }));
      expect(screen.queryByRole("dialog", { name: "Thread" })).not.toBeInTheDocument();

      dialog = await openThread();
      expect({
        refusal: within(dialog).queryByText(refusedText) !== null,
        stopped: within(dialog).queryByText(stoppedText) !== null,
        text: within(dialog).getByRole<HTMLTextAreaElement>("textbox", { name: "Reply" }).value,
      }).toEqual({ refusal: held === "refused", stopped: held === "stopped", text: body });
      expect(createComment).toHaveBeenCalledTimes(sent);
    } finally {
      phone.restore();
    }
  });
}

test(
  "shows Jump to latest until the reader returns to the top",
  withScrollY(async () => {
    const layout = installScrollLayoutMocks();
    const stub = stubLists({ messages: [thread(message("message-1", { body: "A message" }))] });
    const queryClient = newQueryClient();
    const view = render(tab({ "CORE-1": issueState() }, true, queryClient));

    try {
      await screen.findByText("A message");
      await waitFor(() => expect(window.scrollY).toBe(RESTING_SCROLL_Y));
      expect(screen.queryByTestId("jump-to-latest")).not.toBeInTheDocument();

      Object.defineProperty(window, "scrollY", {
        configurable: true,
        value: 4_500,
        writable: true,
      });
      act(() => {
        window.dispatchEvent(new Event("scroll"));
      });
      expect(screen.getByTestId("jump-to-latest")).toBeTruthy();

      Object.defineProperty(window, "scrollY", {
        configurable: true,
        value: RESTING_SCROLL_Y,
        writable: true,
      });
      act(() => {
        window.dispatchEvent(new Event("scroll"));
      });
      await waitFor(() => expect(screen.queryByTestId("jump-to-latest")).not.toBeInTheDocument());
    } finally {
      view.unmount();
      layout.restore();
      stub.restore();
    }
  })
);

test(
  "Jump to latest counts the unread conversations and returns to the newest",
  withScrollY(async () => {
    const user = userEvent.setup();
    const layout = installScrollLayoutMocks();
    const stub = stubLists({
      comments: [
        comment("comment-1", { body: "A read comment", created_at: "2026-09-20T00:00:00Z" }),
        comment("comment-2", { body: "An unread comment", created_at: "2026-09-20T00:05:00Z" }),
      ],
      unread: [headEvent(9, "2026-09-20T00:05:00Z")],
    });
    const queryClient = newQueryClient();
    const view = render(tab({ "CORE-1": issueState([], 3) }, true, queryClient));

    try {
      await screen.findByText("An unread comment");
      await waitFor(() => expect(window.scrollY).toBe(RESTING_SCROLL_Y));
      Object.defineProperty(window, "scrollY", {
        configurable: true,
        value: 4_500,
        writable: true,
      });
      act(() => {
        window.dispatchEvent(new Event("scroll"));
      });

      const pill = await screen.findByTestId("jump-to-latest");
      expect(pill.textContent).toBe("Jump to latest · 1 new");
      await user.click(pill);
      await waitFor(() => expect(window.scrollY).toBe(RESTING_SCROLL_Y));
    } finally {
      view.unmount();
      layout.restore();
      stub.restore();
    }
  })
);

// The pill scrolls to the first card on the page and labels itself with a count: both have to
// read the same list. With the filter on Open and the only unread conversation resolved, a count
// taken over the whole set would promise `· 1 new` and land the reader on a card carrying no
// `New` mark, with the conversation it counted not on the page at all.
test(
  "Jump to latest counts the unread conversations the filter leaves on the page",
  withScrollY(async () => {
    const user = userEvent.setup();
    const layout = installScrollLayoutMocks();
    const stub = stubLists({
      comments: [
        comment("comment-1", { body: "An open comment", created_at: "2026-09-20T00:00:00Z" }),
        comment("comment-2", {
          body: "A resolved comment",
          created_at: "2026-09-20T00:05:00Z",
          resolved: true,
          resolved_at: "2026-09-20T00:06:00Z",
          resolved_by: alice,
        }),
      ],
      unread: [headEvent(9, "2026-09-20T00:05:00Z")],
    });
    const queryClient = newQueryClient();
    const view = render(tab({ "CORE-1": issueState([], 3) }, true, queryClient));

    try {
      await screen.findByText("A resolved comment");
      await waitFor(() => expect(window.scrollY).toBe(RESTING_SCROLL_Y));
      Object.defineProperty(window, "scrollY", {
        configurable: true,
        value: 4_500,
        writable: true,
      });
      act(() => {
        window.dispatchEvent(new Event("scroll"));
      });
      expect((await screen.findByTestId("jump-to-latest")).textContent).toBe(
        "Jump to latest · 1 new"
      );

      const filters = screen.getByRole("group", { name: "Conversations" });
      await user.click(within(filters).getByRole("button", { name: "Open" }));
      await waitFor(() => expect(screen.queryByText("A resolved comment")).not.toBeInTheDocument());

      expect(screen.getByTestId("jump-to-latest").textContent).toBe("Jump to latest");
    } finally {
      view.unmount();
      layout.restore();
      stub.restore();
    }
  })
);

// The pill counts the conversations after the reader's read mark as it stands, while every `New`
// the visit drew stays drawn (`visitBoundary`): the two turns the reader was shown and then read
// keep their marks, and only the two tails that arrive after the read mark are counted.
test(
  "Jump to latest counts the turns after the read mark while the visit's New marks stay drawn",
  withScrollY(async () => {
    const layout = installScrollLayoutMocks();
    const stub = stubLists({
      comments: [
        comment("comment-1", { body: "An older comment", created_at: "2026-09-20T00:00:00Z" }),
        comment("comment-2", { body: "A newer comment", created_at: "2026-09-20T00:05:00Z" }),
      ],
      head: [headEvent(9, "2026-09-20T00:05:00Z")],
      unread: [headEvent(4, "2026-09-20T00:00:00Z")],
    });
    const queryClient = newQueryClient();
    const view = render(tab({ "CORE-1": issueState([], 3) }, true, queryClient));
    /** Whether each turn, in page order, draws a `New` mark. */
    const newMarks = () =>
      Array.from(view.container.querySelectorAll<HTMLElement>("[data-turn]")).map(
        (turn) => within(turn).queryByText("New", { exact: true }) !== null
      );
    const pillText = () => screen.getByTestId("jump-to-latest").textContent;
    /** Comment `id` arrives as event `seq`, as its live event refreshes the comments and the
     *  issue's event reads. */
    const arrive = async (id: string, body: string, seq: number, at: string) => {
      stub.lists.comments = [...(stub.lists.comments ?? []), comment(id, { body, created_at: at })];
      stub.lists.head = [headEvent(seq, at)];
      await act(async () => {
        await queryClient.invalidateQueries({ queryKey: ["comments", "CORE-1"] });
        await queryClient.invalidateQueries({ queryKey: ["events", "CORE-1"] });
      });
      await screen.findByText(body);
    };

    try {
      await screen.findByText("A newer comment");
      await waitFor(() => expect(window.scrollY).toBe(RESTING_SCROLL_Y));
      await waitFor(() => expect(newMarks()).toEqual([true, true]));

      // The read mark through the head: nothing is unread under the new key.
      stub.lists.unread = [];
      view.rerender(tab({ "CORE-1": issueState([], 9) }, true, queryClient));
      await waitFor(() =>
        expect(queryClient.getQueryState(["events", "CORE-1", "unread-boundary", 9])?.status).toBe(
          "success"
        )
      );
      Object.defineProperty(window, "scrollY", {
        configurable: true,
        value: 4_500,
        writable: true,
      });
      act(() => {
        window.dispatchEvent(new Event("scroll"));
      });
      await screen.findByTestId("jump-to-latest");
      expect(pillText()).toBe("Jump to latest");

      // Event 10, the first tail's, is the first the reader has not read, through both arrivals.
      stub.lists.unread = [headEvent(10, "2026-09-20T00:10:00Z")];
      await arrive("comment-3", "A tail", 10, "2026-09-20T00:10:00Z");
      await waitFor(() => expect(pillText()).toBe("Jump to latest · 1 new"));
      await arrive("comment-4", "Another tail", 11, "2026-09-20T00:15:00Z");
      await waitFor(() => expect(pillText()).toBe("Jump to latest · 2 new"));
      expect(newMarks()).toEqual([true, true, true, true]);
    } finally {
      view.unmount();
      layout.restore();
      stub.restore();
    }
  })
);

test(
  "a reader pinned to the top stays pinned when a conversation arrives",
  withScrollY(async () => {
    const layout = installScrollLayoutMocks();
    const stub = stubLists({
      messages: [thread(message("message-1", { body: "A message" }))],
    });
    const queryClient = newQueryClient();
    const view = render(tab({ "CORE-1": issueState() }, true, queryClient));

    try {
      Object.defineProperty(window, "scrollY", { configurable: true, value: 0, writable: true });
      await screen.findByText("A message");
      await waitFor(() => expect(window.scrollY).toBe(RESTING_SCROLL_Y));
      expect(screen.queryByTestId("jump-to-latest")).not.toBeInTheDocument();

      stub.lists.messages = [
        thread(message("message-1", { body: "A message" })),
        thread(
          message("message-2", { body: "Incoming message", created_at: "2026-09-09T00:05:00Z" })
        ),
      ];
      await act(async () => {
        await queryClient.invalidateQueries({ queryKey: ["issue-messages", "CORE-1"] });
      });

      await screen.findByText("Incoming message");
      expect(
        Array.from(view.container.querySelectorAll("[data-turn]")).map((turn) =>
          turn.getAttribute("data-turn")
        )
      ).toEqual(["message:message-2", "message:message-1"]);
      expect(screen.queryByTestId("jump-to-latest")).not.toBeInTheDocument();
      expect(window.scrollY).toBe(RESTING_SCROLL_Y);
    } finally {
      view.unmount();
      layout.restore();
      stub.restore();
    }
  })
);

test("a comment-delivery retry disables when the target no longer advertises the failed mode", async () => {
  const stub = stubLists({
    agents: [agent("worker-session", "Worker")],
    comments: [
      comment("comment-1", {
        body: "@Worker take a look",
        deliveries: [mentionAttempt({ error: "no live session worker" })],
        mentions: [
          { delivery: "steer", session_id: "worker-session", target: "session:worker-session" },
        ],
      }),
    ],
  });
  const queryClient = newQueryClient();
  const view = render(tab({ "CORE-1": issueState() }, true, queryClient));

  try {
    await screen.findByText(/no live session worker/);
    expect(screen.queryByRole("button", { name: "Retry" })).not.toBeInTheDocument();
    expect(screen.getByText(wholeText("Worker no longer supports Send."))).toBeTruthy();
  } finally {
    view.unmount();
    stub.restore();
  }
});

test("a comment-delivery retry to a role target checks the role's current live holder, not the old one", async () => {
  const holder = agent("new-reviewer-session", "New reviewer");
  const stub = stubLists({
    // The role's live holder changed since the failed attempt and does not advertise steer,
    // even though the delivery's own recorded session_id (the old holder) is a stale reference.
    agents: [{ ...holder, roles: ["reviewer"] }],
    comments: [
      comment("comment-1", {
        body: "@reviewer take a look",
        deliveries: [
          mentionAttempt({
            error: "no live session reviewer",
            session_id: "old-reviewer-session",
            target: "role:reviewer",
          }),
        ],
        mentions: [{ delivery: "steer", session_id: null, target: "role:reviewer" }],
      }),
    ],
  });
  const queryClient = newQueryClient();
  const view = render(tab({ "CORE-1": issueState() }, true, queryClient));

  try {
    await screen.findByText(/no live session reviewer/);
    expect(screen.queryByRole("button", { name: "Retry" })).not.toBeInTheDocument();
    expect(screen.getByText(/role:reviewer no longer supports Send\./)).toBeTruthy();
  } finally {
    view.unmount();
    stub.restore();
  }
});

test("a comment-delivery retry guards a same-tick double click to exactly one attempt", async () => {
  const stub = stubLists({
    agents: [agent("worker-session", "Worker", ["steer"])],
    comments: [
      comment("comment-1", {
        body: "@Worker take a look",
        deliveries: [mentionAttempt({ error: "no live session worker" })],
      }),
    ],
  });
  const queryClient = newQueryClient();
  let sends = 0;
  const { promise, resolve } = Promise.withResolvers<void>();
  const createCommentDelivery = spyOn(api, "createCommentDelivery").mockImplementation(
    async (id, target, delivery) => {
      sends += 1;
      await promise;
      return mentionAttempt({
        attempt: 2,
        comment_id: id,
        delivery,
        error: null,
        state: "sent",
        target: target ?? "session:worker-session",
      });
    }
  );
  const view = render(tab({ "CORE-1": issueState() }, true, queryClient));

  try {
    const retry = await screen.findByRole("button", { name: "Retry" });
    // Both clicks fire in the same task, before any re-render could hold the button - the window
    // a check of the held state (which lags on React state) cannot catch. user-event has no form
    // for this: each `await user.click` lets React re-render between the two presses.
    fireEvent.click(retry);
    fireEvent.click(retry);
    resolve();
    await waitFor(() => expect(sends).toBe(1));
  } finally {
    view.unmount();
    createCommentDelivery.mockRestore();
    stub.restore();
  }
});

// A mention's Retry is held while its retry is out, by `aria-disabled`, never `disabled`
// (`pressHeld`): the pressed Retry keeps the focus through the write, and a second press, by key or
// by pointer, sends nothing.
test("a mention's held Retry keeps the focus while its retry is out, and a second press sends nothing", async () => {
  const user = userEvent.setup();
  const stub = stubLists({
    agents: [agent("worker-session", "Worker", ["steer"])],
    comments: [
      comment("comment-1", {
        body: "@Worker take a look",
        deliveries: [mentionAttempt({ error: "no live session worker" })],
      }),
    ],
  });
  const queryClient = newQueryClient();
  // The retry's answer, held until the case gives it inside `act`.
  const answer = Promise.withResolvers<CommentDelivery>();
  const createCommentDelivery = spyOn(api, "createCommentDelivery").mockReturnValue(answer.promise);
  const view = render(tab({ "CORE-1": issueState() }, true, queryClient));

  try {
    await pressHeld(user, await screen.findByRole("button", { name: "Retry" }), {
      name: "Retry",
      sent: () => createCommentDelivery.mock.calls.length,
    });
  } finally {
    await act(async () =>
      answer.resolve(mentionAttempt({ attempt: 2, error: null, state: "sent" }))
    );
    view.unmount();
    createCommentDelivery.mockRestore();
    stub.restore();
  }
});

test.each([
  { turn: "comment", name: "Retry", mode: "Send" },
  { turn: "message", name: "Retry", mode: "Aside" },
  { turn: "message", name: "Use BTW instead", mode: "BTW" },
  { turn: "message", name: "Use Send instead", mode: "Send" },
  { turn: "reply", name: "Retry", mode: "Aside" },
  { turn: "reply", name: "Use BTW instead", mode: "BTW" },
  { turn: "reply", name: "Use Send instead", mode: "Send" },
] as const)("$turn: $name lets go at the delivery write deadline and sends again", async ({
  turn,
  name,
  mode,
}) => {
  const target = "session:worker-session";
  const failed = attempt({
    delivery: "aside",
    error: "no live session",
    session_id: "worker-session",
    state: "failed",
  });
  const lists =
    turn === "comment"
      ? {
          comments: [
            comment("comment-1", { deliveries: [mentionAttempt({ error: "no live session" })] }),
          ],
        }
      : {
          messages: [
            turn === "message"
              ? thread(message("message-1", { deliveries: [failed], target }))
              : thread(message("message-1", { target }), [
                  message("message-2", {
                    author: { id: "worker-session", kind: "session" },
                    in_reply_to: "message-1",
                  }),
                  message("message-3", {
                    deliveries: [{ ...failed, message_id: "message-3" }],
                    in_reply_to: "message-1",
                  }),
                ]),
          ],
        };
  const stub = stubLists({
    ...lists,
    agents: [agent("worker-session", "Worker", ["aside", "btw", "steer"])],
  });
  const dropped = new NetworkError("/api/v1/deliveries", new TypeError("Failed to fetch"));
  const retryComment = spyOn(api, "createCommentDelivery").mockRejectedValue(dropped);
  const retryMessage = spyOn(api, "createMessageDelivery").mockRejectedValue(dropped);
  const queryClient = newQueryClient();
  const view = render(tab({ "CORE-1": issueState() }, true, queryClient));
  try {
    const button = await screen.findByRole<HTMLButtonElement>("button", { name });
    jest.useFakeTimers();
    const user = userOnFakeClock();
    const sent = () => pressesOf([retryComment, retryMessage]);
    await heldUntilLate(user, button, {
      label: `Delivery ${turn === "comment" ? "of the comment " : ""}to Worker again, as ${mode}`,
      name,
      queryClient,
      sent,
    });
    await user.keyboard("{Enter}");
    await tick(1_000);
    expect(sent()).toBe(2);
  } finally {
    view.unmount();
    queryClient.clear();
    retryComment.mockRestore();
    retryMessage.mockRestore();
    stub.restore();
  }
});

test("a root targeted-message retry checks the role's current holder after a handoff, not the failed attempt's session", async () => {
  const stub = stubLists({
    // The role has since handed off; the live holder does not advertise steer.
    agents: [{ ...agent("new-reviewer-session", "New reviewer", ["btw"]), roles: ["reviewer"] }],
    messages: [
      thread(
        message("message-1", {
          // The failed attempt was recorded against the role's OLD holder.
          deliveries: [
            attempt({
              delivery: "steer",
              error: "no live session old-reviewer-session",
              session_id: "old-reviewer-session",
              state: "failed",
            }),
          ],
          target: "role:reviewer",
        })
      ),
    ],
  });
  const queryClient = newQueryClient();
  const view = render(tab({ "CORE-1": issueState() }, true, queryClient));

  try {
    // The new holder dropped steer, so the same-mode Retry of a steer attempt is refused.
    const sendNormally = await screen.findByRole("button", { name: "Retry" });
    expect(sendNormally.hasAttribute("disabled")).toBe(true);
    expect(screen.getByRole("button", { name: "Use BTW instead" })).not.toHaveAttribute("disabled");
  } finally {
    view.unmount();
    stub.restore();
  }
});

test("a reply's targeted-message retry checks the thread's current role holder after a handoff", async () => {
  const stub = stubLists({
    agents: [{ ...agent("new-reviewer-session", "New reviewer", ["btw"]), roles: ["reviewer"] }],
    messages: [
      thread(
        message("message-1", {
          deliveries: [attempt({ delivery: "steer", session_id: "old-reviewer-session" })],
          target: "role:reviewer",
        }),
        [
          // A session answer marks the root "answered," suppressing its own retry row so only
          // the reply's retry row is under test below.
          message("message-3", {
            author: { id: "old-reviewer-session", kind: "session" },
            body: "Looking into it.",
            created_at: "2026-09-09T00:00:45Z",
            in_reply_to: "message-1",
          }),
          message("message-2", {
            body: "Any update?",
            created_at: "2026-09-09T00:01:00Z",
            // The reply's own failed delivery is recorded against the role's OLD holder too.
            deliveries: [
              attempt({
                delivery: "steer",
                error: "no live session old-reviewer-session",
                message_id: "message-2",
                session_id: "old-reviewer-session",
                state: "failed",
              }),
            ],
            in_reply_to: "message-1",
          }),
        ]
      ),
    ],
  });
  const queryClient = newQueryClient();
  const view = render(tab({ "CORE-1": issueState() }, true, queryClient));

  try {
    await screen.findByText("Any update?");
    // The new holder dropped steer, so the same-mode Retry of a steer attempt is refused.
    const sendNormally = await screen.findByRole("button", { name: "Retry" });
    expect(sendNormally.hasAttribute("disabled")).toBe(true);
    expect(screen.getByRole("button", { name: "Use BTW instead" })).not.toHaveAttribute("disabled");
  } finally {
    view.unmount();
    stub.restore();
  }
});

test("Conversation hides reply editing while its owner saves a comment", async () => {
  const user = userEvent.setup();
  const root = comment("root-comment", { body: "Root comment" });
  const reply = comment("reply-comment", { body: "Reply comment", reply_to: "root-comment" });
  const stub = stubLists({ comments: [root, reply] });
  const save = Promise.withResolvers<Comment>();
  const editComment = spyOn(api, "editComment").mockImplementation(() => save.promise);
  const queryClient = newQueryClient();
  queryClient.setQueryData(["whoami"], { kind: "user", login: "alice" });
  const view = render(tab({ "CORE-1": issueState() }, true, queryClient));

  try {
    const card = await screen.findByTestId("margin-comment-root-comment");
    await user.click(screen.getByRole("button", { name: "Expand thread" }));
    await waitFor(() =>
      expect(within(card).getAllByRole("button", { name: "Edit" })).toHaveLength(2)
    );
    await user.click(within(card).getAllByRole("button", { name: "Edit" })[0] as HTMLElement);
    const editField = screen.getByLabelText("Edit comment");
    await user.clear(editField);
    await user.click(editField);
    await user.paste("Updated root");
    await user.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() =>
      expect(editComment).toHaveBeenCalledWith(
        "root-comment",
        { body: "Updated root" },
        keyedAttempt
      )
    );
    expect(within(card).queryAllByRole("button", { name: "Edit" })).toHaveLength(0);
    expect((screen.getByRole("textbox", { name: "Reply" }) as HTMLTextAreaElement).disabled).toBe(
      false
    );

    await act(async () => {
      save.resolve(root);
      await save.promise;
    });
    await waitFor(() =>
      expect(within(card).getAllByRole("button", { name: "Edit" })).toHaveLength(2)
    );
  } finally {
    save.resolve(root);
    view.unmount();
    editComment.mockRestore();
    stub.restore();
  }
});

test("Conversation restores edit controls and the draft when its owner save rejects", async () => {
  const user = userEvent.setup();
  const root = comment("root-comment", { body: "Root comment" });
  const reply = comment("reply-comment", { body: "Reply comment", reply_to: "root-comment" });
  const stub = stubLists({ comments: [root, reply] });
  const save = Promise.withResolvers<Comment>();
  const editComment = spyOn(api, "editComment").mockImplementation(() => save.promise);
  const queryClient = newQueryClient();
  queryClient.setQueryData(["whoami"], { kind: "user", login: "alice" });
  const view = render(tab({ "CORE-1": issueState() }, true, queryClient));

  try {
    const card = await screen.findByTestId("margin-comment-root-comment");
    await user.click(screen.getByRole("button", { name: "Expand thread" }));
    await waitFor(() =>
      expect(within(card).getAllByRole("button", { name: "Edit" })).toHaveLength(2)
    );
    await user.click(within(card).getAllByRole("button", { name: "Edit" })[0] as HTMLElement);
    const editField = screen.getByLabelText("Edit comment");
    await user.clear(editField);
    await user.click(editField);
    await user.paste("Draft survives");
    await user.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() =>
      expect(editComment).toHaveBeenCalledWith(
        "root-comment",
        { body: "Draft survives" },
        keyedAttempt
      )
    );
    expect(within(card).queryAllByRole("button", { name: "Edit" })).toHaveLength(0);

    await act(async () => {
      save.reject(new Error("offline"));
      await Promise.resolve();
    });
    await screen.findByRole("alert");
    await waitFor(() =>
      expect(within(card).getAllByRole("button", { name: "Edit" })).toHaveLength(2)
    );
    expect((screen.getByLabelText("Edit comment") as HTMLTextAreaElement).value).toBe(
      "Draft survives"
    );
  } finally {
    view.unmount();
    editComment.mockRestore();
    stub.restore();
  }
});

// A save that lands just as the browser goes offline: the comments it changed are asked again,
// and the network pauses that read until it returns, with no deadline of its own running: the
// app's client holds a read of an idle query until its paused fetch answers
// (`DispatchQueryClient`). The save holds the edit - its editor open and every Edit of the card
// hidden - through that read only up to the request deadline (`REQUEST_DEADLINE_MS`), and lets it
// go there, as the other holds through a read do (`readWithinBound`).
test("offline, a comment edit's save holds the edit through the read it made stale only until the request deadline", async () => {
  const user = userEvent.setup();
  const root = comment("root-comment", { body: "Root comment" });
  const reply = comment("reply-comment", { body: "Reply comment", reply_to: "root-comment" });
  const stub = stubLists({ comments: [root, reply] });
  const save = Promise.withResolvers<Comment>();
  const editComment = spyOn(api, "editComment").mockImplementation(() => save.promise);
  const queryClient = new DispatchQueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  queryClient.setQueryData(["whoami"], { kind: "user", login: "alice" });
  const view = render(tab({ "CORE-1": issueState() }, true, queryClient));
  const online = onlineManager.isOnline();

  try {
    const card = await screen.findByTestId("margin-comment-root-comment");
    await user.click(screen.getByRole("button", { name: "Expand thread" }));
    await waitFor(() =>
      expect(within(card).getAllByRole("button", { name: "Edit" })).toHaveLength(2)
    );
    await user.click(within(card).getAllByRole("button", { name: "Edit" })[0] as HTMLElement);
    const editField = screen.getByLabelText("Edit comment");
    await user.clear(editField);
    await user.click(editField);
    await user.paste("Updated root");
    jest.useFakeTimers();
    await userOnFakeClock().click(screen.getByRole("button", { name: "Save" }));
    await settle();
    expect(editComment).toHaveBeenCalledWith(
      "root-comment",
      { body: "Updated root" },
      keyedAttempt
    );

    // The save lands with the browser offline, and the comments' read it starts is paused.
    onlineManager.setOnline(false);
    await act(async () => {
      save.resolve({ ...root, body: "Updated root" });
      await save.promise;
    });

    // A second short of the request deadline the read is still paused, and the edit held.
    await tick(REQUEST_DEADLINE_MS - 1_000);
    expect(within(card).queryAllByRole("button", { name: "Edit" })).toHaveLength(0);
    expect(screen.getByLabelText("Edit comment")).toBeInTheDocument();

    // At it the save lets the edit go: its editor closes, and each Edit is back.
    await tick(1_000);
    expect(screen.queryByLabelText("Edit comment")).not.toBeInTheDocument();
    expect(within(card).getAllByRole("button", { name: "Edit" })).toHaveLength(2);
  } finally {
    save.resolve(root);
    view.unmount();
    queryClient.clear();
    onlineManager.setOnline(online);
    realClock();
    editComment.mockRestore();
    stub.restore();
  }
});

// A save that lands while the read of the comments it changed goes unanswered: the save holds the
// edit through that read up to the request deadline and lets it go there, and the comment says
// why beneath its card, in the words of the error such a read ends with. The line goes once that
// read settles.
test("a comment edit's save whose read has not answered by the request deadline lets the edit go there, and the comment says why until that read settles", async () => {
  const user = userEvent.setup();
  const root = comment("root-comment", { body: "Root comment" });
  const reply = comment("reply-comment", { body: "Reply comment", reply_to: "root-comment" });
  const stub = stubLists({ comments: [root, reply] });
  const listComments = api.listComments as Mock<typeof api.listComments>;
  const save = Promise.withResolvers<Comment>();
  const editComment = spyOn(api, "editComment").mockImplementation(() => save.promise);
  const reread = Promise.withResolvers<Comment[]>();
  const queryClient = newQueryClient();
  queryClient.setQueryData(["whoami"], { kind: "user", login: "alice" });
  const view = render(tab({ "CORE-1": issueState() }, true, queryClient));
  const why = "The edited comment is not read yet. Dispatch did not answer within 30 seconds.";

  try {
    const card = await screen.findByTestId("margin-comment-root-comment");
    const turn = card.closest("[data-turn]");
    if (!(turn instanceof HTMLElement)) throw new Error("the comment's card is drawn in no turn");
    await user.click(screen.getByRole("button", { name: "Expand thread" }));
    await waitFor(() =>
      expect(within(card).getAllByRole("button", { name: "Edit" })).toHaveLength(2)
    );
    await user.click(within(card).getAllByRole("button", { name: "Edit" })[0] as HTMLElement);
    const editField = screen.getByLabelText("Edit comment");
    await user.clear(editField);
    await user.click(editField);
    await user.paste("Updated root");
    listComments.mockImplementation(() => reread.promise);
    jest.useFakeTimers();
    await userOnFakeClock().click(screen.getByRole("button", { name: "Save" }));
    await settle();
    expect(editComment).toHaveBeenCalledWith(
      "root-comment",
      { body: "Updated root" },
      keyedAttempt
    );

    // The save lands, and the comments' read it starts goes unanswered.
    await act(async () => {
      save.resolve({ ...root, body: "Updated root" });
      await save.promise;
    });

    // A second short of the request deadline the read is still out, the edit held, and nothing
    // said.
    await tick(REQUEST_DEADLINE_MS - 1_000);
    expect(screen.getByLabelText("Edit comment")).toBeInTheDocument();
    expect(screen.queryByText(why)).toBeNull();

    // At it the save lets the edit go, and the comment's turn says why.
    await tick(1_000);
    expect(screen.queryByLabelText("Edit comment")).not.toBeInTheDocument();
    expect(within(turn).getByText(why)).toBeInTheDocument();

    // The read answers with the saved words, and the line goes.
    await act(async () => reread.resolve([{ ...root, body: "Updated root" }, reply]));
    await tick(0);
    expect(screen.queryByText(why)).toBeNull();
  } finally {
    save.resolve(root);
    reread.resolve([root, reply]);
    view.unmount();
    queryClient.clear();
    realClock();
    editComment.mockRestore();
    stub.restore();
  }
});

// The save's own event reaches the page while the read of the comments its landing started is
// out, as the live feed brings it: the feed writes the edit's time into the comments over that
// read, which it gives up and asks again (`markCorrectedPost`, `replaceQueryData`). The save holds
// the edit through the read asked again (`DispatchQueryClient`), so where that read goes
// unanswered the comment says why at the request deadline, until that read answers.
test("a comment edit's save whose read its own event asks again holds the edit through the read asked again, and the comment says why at the request deadline until it answers", async () => {
  const user = userEvent.setup();
  const root = comment("root-comment", { body: "Root comment" });
  const reply = comment("reply-comment", { body: "Reply comment", reply_to: "root-comment" });
  const stub = stubLists({ comments: [root, reply] });
  const listComments = api.listComments as Mock<typeof api.listComments>;
  const save = Promise.withResolvers<Comment>();
  const editComment = spyOn(api, "editComment").mockImplementation(() => save.promise);
  // Each read of the comments from the save on, held until the row answers it.
  const { answers: rereads, next: nextReread } = heldAnswers<Comment[]>();
  const queryClient = new DispatchQueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  queryClient.setQueryData(["whoami"], { kind: "user", login: "alice" });
  const view = render(tab({ "CORE-1": issueState() }, true, queryClient));
  const why = "The edited comment is not read yet. Dispatch did not answer within 30 seconds.";
  const edited = correction("comment", root.id, true);

  try {
    const card = await screen.findByTestId("margin-comment-root-comment");
    const turn = card.closest("[data-turn]");
    if (!(turn instanceof HTMLElement)) throw new Error("the comment's card is drawn in no turn");
    await user.click(screen.getByRole("button", { name: "Expand thread" }));
    await waitFor(() =>
      expect(within(card).getAllByRole("button", { name: "Edit" })).toHaveLength(2)
    );
    await user.click(within(card).getAllByRole("button", { name: "Edit" })[0] as HTMLElement);
    const editField = screen.getByLabelText("Edit comment");
    await user.clear(editField);
    await user.click(editField);
    await user.paste("Updated root");
    listComments.mockImplementation(nextReread);
    jest.useFakeTimers();
    await userOnFakeClock().click(screen.getByRole("button", { name: "Save" }));
    await settle();
    expect(editComment).toHaveBeenCalledWith(
      "root-comment",
      { body: "Updated root" },
      keyedAttempt
    );

    // The save lands, and the comments' read it starts goes unanswered.
    await act(async () => {
      save.resolve({ ...root, body: "Updated root" });
      await save.promise;
    });
    expect(rereads).toHaveLength(1);

    // The save's own event comes as the feed hears it (`hearLiveEvent`, then `markCorrectedPost`,
    // in `useEventFeed`): it gives that read up and asks the comments again, and that read goes
    // unanswered too.
    await act(async () => {
      hearLiveEvent(queryClient, edited, "alice");
      markCorrectedPost(queryClient, edited);
    });
    expect(rereads).toHaveLength(2);

    // A second short of the request deadline the edit is still held, and nothing said.
    await tick(REQUEST_DEADLINE_MS - 1_000);
    expect(screen.getByLabelText("Edit comment")).toBeInTheDocument();
    expect(screen.queryByText(why)).toBeNull();

    // At it the save lets the edit go, and the comment's turn says why.
    await tick(1_000);
    expect(screen.queryByLabelText("Edit comment")).not.toBeInTheDocument();
    expect(within(turn).getByText(why)).toBeInTheDocument();

    // The read asked again answers with the saved words, and the line goes.
    await act(async () =>
      rereads[1]?.([{ ...root, body: "Updated root", edited_at: EDITED_AT }, reply])
    );
    await tick(0);
    expect(screen.queryByText(why)).toBeNull();
  } finally {
    save.resolve(root);
    for (const answer of rereads) answer([root, reply]);
    view.unmount();
    queryClient.clear();
    realClock();
    editComment.mockRestore();
    stub.restore();
  }
});

// The line a save's read the bound ended draws is that save's own: the next save of an edit takes
// it away as it lands, though the earlier read is still out, and that save's read answering says
// nothing more. Dispatch holds each read of the comments until the row answers them, and the next
// save's read waits for the earlier one's fetch (`DispatchQueryClient`), so it answers after it.
test("the line a comment edit's read the bound ended draws goes as the next save lands, whose read answers", async () => {
  const user = userEvent.setup();
  const root = comment("root-comment", { body: "Root comment" });
  const reply = comment("reply-comment", { body: "Reply comment", reply_to: "root-comment" });
  const stub = stubLists({ comments: [root, reply] });
  const listComments = api.listComments as Mock<typeof api.listComments>;
  // Each save's answer and each read of the comments, held until the row gives them.
  const { answers: saves, next: nextSave } = heldAnswers<Comment>();
  const editComment = spyOn(api, "editComment").mockImplementation(nextSave);
  const { answers: rereads, next: nextReread } = heldAnswers<Comment[]>();
  const queryClient = new DispatchQueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  queryClient.setQueryData(["whoami"], { kind: "user", login: "alice" });
  const view = render(tab({ "CORE-1": issueState() }, true, queryClient));
  const why = "The edited comment is not read yet. Dispatch did not answer within 30 seconds.";

  try {
    const card = await screen.findByTestId("margin-comment-root-comment");
    const turn = card.closest("[data-turn]");
    if (!(turn instanceof HTMLElement)) throw new Error("the comment's card is drawn in no turn");
    await user.click(screen.getByRole("button", { name: "Expand thread" }));
    await waitFor(() =>
      expect(within(card).getAllByRole("button", { name: "Edit" })).toHaveLength(2)
    );
    listComments.mockImplementation(nextReread);
    jest.useFakeTimers();
    const onClock = userOnFakeClock();
    /** Saves `body` as the root comment's words through its editor, and lands the save. */
    const saveRoot = async (body: string) => {
      await onClock.click(within(card).getAllByRole("button", { name: "Edit" })[0] as HTMLElement);
      const editField = screen.getByLabelText("Edit comment");
      await onClock.clear(editField);
      await onClock.click(editField);
      await onClock.paste(body);
      await onClock.click(screen.getByRole("button", { name: "Save" }));
      await settle();
      await act(async () => saves.at(-1)?.({ ...root, body }));
      await tick(0);
    };

    await saveRoot("Updated root");
    await tick(REQUEST_DEADLINE_MS);
    expect(screen.queryByLabelText("Edit comment")).not.toBeInTheDocument();
    expect(within(turn).getByText(why)).toBeInTheDocument();

    await saveRoot("Updated again");
    expect(editComment).toHaveBeenCalledTimes(2);
    expect(screen.getByLabelText("Edit comment")).toBeInTheDocument();
    expect(
      within(turn).queryByText(why),
      "the earlier save's read kept its line past the next save"
    ).toBeNull();

    const saved = [{ ...root, body: "Updated again" }, reply];
    listComments.mockResolvedValue(saved);
    await act(async () => {
      for (const answer of rereads) answer(saved);
    });
    await tick(0);
    expect(screen.queryByLabelText("Edit comment")).not.toBeInTheDocument();
    expect(screen.queryByText(why)).toBeNull();
  } finally {
    view.unmount();
    queryClient.clear();
    realClock();
    editComment.mockRestore();
    stub.restore();
  }
});

// Every comment's edit reads the one list of the issue's comments again, so the line one comment's
// turn draws for a read the bound ended goes as another comment's save lands, though that read is
// still out: the turn list's one hold forgets it then (`useListReadBound`), and the second turn
// alone says why the read its own save made has not settled, once that read's bound ends.
test("the line one comment's edit leaves goes as another comment's save lands", async () => {
  const user = userEvent.setup();
  const first = comment("first-comment", { body: "First comment" });
  const second = comment("second-comment", { body: "Second comment" });
  const stub = stubLists({ comments: [first, second] });
  const listComments = api.listComments as Mock<typeof api.listComments>;
  // Each save's answer and each read of the comments, held until the row gives them.
  const { answers: saves, next: nextSave } = heldAnswers<Comment>();
  const editComment = spyOn(api, "editComment").mockImplementation(nextSave);
  const { answers: rereads, next: nextReread } = heldAnswers<Comment[]>();
  const queryClient = new DispatchQueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  queryClient.setQueryData(["whoami"], { kind: "user", login: "alice" });
  const view = render(tab({ "CORE-1": issueState() }, true, queryClient));
  const why = "The edited comment is not read yet. Dispatch did not answer within 30 seconds.";

  try {
    /** The comment's card, open on its thread, and the turn it is drawn in. */
    const opened = async (id: string) => {
      const card = await screen.findByTestId(`margin-comment-${id}`);
      const turn = card.closest("[data-turn]");
      if (!(turn instanceof HTMLElement)) throw new Error(`${id}'s card is drawn in no turn`);
      await user.click(within(turn).getByRole("button", { name: "Expand thread" }));
      await waitFor(() =>
        expect(within(card).getAllByRole("button", { name: "Edit" })).toHaveLength(1)
      );
      return { card, turn };
    };
    const firstTurn = await opened(first.id);
    const secondTurn = await opened(second.id);
    listComments.mockImplementation(nextReread);
    jest.useFakeTimers();
    const onClock = userOnFakeClock();
    /** Saves `body` as the words of the comment drawn in `card` through its editor, and lands
     *  the save. */
    const save = async (card: HTMLElement, saved: Comment, body: string) => {
      await onClock.click(within(card).getByRole("button", { name: "Edit" }));
      const editField = screen.getByLabelText("Edit comment");
      await onClock.clear(editField);
      await onClock.click(editField);
      await onClock.paste(body);
      await onClock.click(screen.getByRole("button", { name: "Save" }));
      await settle();
      await act(async () => saves.at(-1)?.({ ...saved, body }));
      await tick(0);
    };

    await save(firstTurn.card, first, "First, edited");
    await tick(REQUEST_DEADLINE_MS);
    expect(within(firstTurn.turn).getByText(why)).toBeInTheDocument();

    await save(secondTurn.card, second, "Second, edited");
    expect(editComment).toHaveBeenCalledTimes(2);
    expect(
      within(firstTurn.turn).queryByText(why),
      "the first comment's read kept its line on its turn past the second comment's save"
    ).toBeNull();

    await tick(REQUEST_DEADLINE_MS);
    expect(within(secondTurn.turn).getByText(why)).toBeInTheDocument();
    expect(within(firstTurn.turn).queryByText(why)).toBeNull();
  } finally {
    for (const answer of rereads) answer([first, second]);
    view.unmount();
    queryClient.clear();
    realClock();
    editComment.mockRestore();
    stub.restore();
  }
});

interface ThreadsPage {
  readonly cleanup: () => void;
  readonly mark: Mock<
    (id: string, body: { read_through: string }, bounds: WriteBounds) => Promise<unknown>
  >;
  readonly turn: (id: string) => HTMLElement;
  readonly viewport: IntersectionControl;
}

/**
 * Renders the Conversation on `lists` with `unread` thread ids unread for alice, the screen in
 * the test's hands, and every write it can make stubbed. `focusItemId` is the item a deep link
 * names.
 */
function renderThreads(lists: Lists, unread: readonly string[], focusItemId?: string): ThreadsPage {
  const viewport = installIntersectionObserver();
  const stub = stubLists(lists);
  const putIssueState = answerIssueStates().mockImplementation(async () => issueState());
  const mark = answerThreadReads().mockResolvedValue(undefined);
  const scrollIntoView = stubScrollIntoView();
  const queryClient = newQueryClient();
  seedUnread(queryClient, { issue: "CORE-1" }, unread);
  const view = render(
    tab({ "CORE-1": issueState() }, true, queryClient, false, new Map(), focusItemId)
  );
  return {
    cleanup: () => {
      view.unmount();
      putIssueState.mockRestore();
      mark.mockRestore();
      stub.restore();
      scrollIntoView.mockRestore();
      viewport.restore();
    },
    mark,
    turn: (id) => {
      const element = view.container.querySelector<HTMLElement>(`[data-turn="${id}"]`);
      if (element === null) throw new Error(`expected the ${id} turn`);
      return element;
    },
    viewport,
  };
}

/** The shared dwell with the fake clock it moves: this file's tests leave installing the clock to
 *  the dwell rather than installing it at their head. */
async function dwellOnFakeClock(
  viewport: IntersectionControl,
  element: Element,
  placement?: Placement
) {
  jest.useFakeTimers();
  await dwellOn(viewport, element, placement);
}

/** A session's reply in a message thread. */
function sessionReply(id: string, parent: string, at: string): Message {
  return message(id, {
    author: { id: "s1", kind: "session" },
    body: `Session reply ${id}`,
    created_at: at,
    in_reply_to: parent,
  });
}

// A link to a message (`/issues/K/messages/<root>`) shows its thread's replies, which a message
// thread always renders, and they mark the thread read like any other view of them.
test("landing on a message thread shows its replies and marks the thread read through the newest", async () => {
  const reply = sessionReply("message-2", "message-1", "2026-09-09T00:05:00.25Z");
  const page = renderThreads(
    { messages: [thread(message("message-1", { body: "Status?" }), [reply])] },
    ["message-1"],
    "message-1"
  );
  try {
    await screen.findByText("Session reply message-2");
    const newest = page.turn("message:message-2");
    expect(page.viewport.observed()).toContain(newest);
    await dwellOnFakeClock(page.viewport, newest);
    expect(page.mark.mock.calls).toEqual([
      ["message-1", { read_through: "2026-09-09T00:05:00.25Z" }, keyedAttempt],
    ]);
  } finally {
    page.cleanup();
  }
});

// The root card wraps every reply, so a long exchange can be taller than the screen and never
// three-quarters on it; the mark watches the newest reply instead.
test("a message thread taller than the screen is marked read from its newest reply alone", async () => {
  const first = sessionReply("message-2", "message-1", "2026-09-09T00:05:00Z");
  const second = sessionReply("message-3", "message-1", "2026-09-09T00:06:00Z");
  const page = renderThreads(
    { messages: [thread(message("message-1", { body: "Status?" }), [first, second])] },
    ["message-1"]
  );
  try {
    await screen.findByText("Session reply message-3");
    act(() => page.viewport.show(page.turn("message:message-1"), { height: 3_000, visible: 300 }));
    await dwellOnFakeClock(page.viewport, page.turn("message:message-3"));
    expect(page.mark.mock.calls).toEqual([
      ["message-1", { read_through: "2026-09-09T00:06:00Z" }, keyedAttempt],
    ]);
    // The older reply is not what the mark watches.
    expect(page.viewport.observed()).not.toContain(page.turn("message:message-2"));
  } finally {
    page.cleanup();
  }
});

test("a targeted message's thread is marked read once its newest reply is on screen", async () => {
  const reply = sessionReply("message-2", "message-1", "2026-09-09T00:07:00Z");
  const page = renderThreads(
    {
      messages: [
        thread(message("message-1", { body: "Please rebase", target: "session:s1" }), [reply]),
      ],
    },
    ["message-1"]
  );
  try {
    await screen.findByText("Session reply message-2");
    await dwellOnFakeClock(page.viewport, page.turn("message:message-2"));
    expect(page.mark.mock.calls).toEqual([
      ["message-1", { read_through: "2026-09-09T00:07:00Z" }, keyedAttempt],
    ]);
  } finally {
    page.cleanup();
  }
});

test("a message thread the viewer has read is never watched for the Replies mark", async () => {
  const reply = sessionReply("message-2", "message-1", "2026-09-09T00:05:00Z");
  const page = renderThreads(
    { messages: [thread(message("message-1", { body: "Status?" }), [reply])] },
    []
  );
  try {
    await screen.findByText("Session reply message-2");
    expect(page.viewport.observed()).not.toContain(page.turn("message:message-2"));
  } finally {
    page.cleanup();
  }
});

// Every comment link opens the comment's thread on landing - a Replies row's, and equally the
// Inbox's, search's or a copied reference's - so its replies are on screen to read.
test("at a desktop width, landing on a comment opens its thread inline and reading it marks it", async () => {
  const root = comment("root-comment", { body: "Root comment" });
  const reply = comment("reply-comment", {
    body: "Reply comment",
    created_at: "2026-09-20T00:01:00Z",
    reply_to: "root-comment",
  });
  const page = renderThreads({ comments: [root, reply] }, ["root-comment"], "root-comment");
  try {
    const replyItem = (await screen.findByText("Reply comment")).closest("li");
    if (replyItem === null) throw new Error("expected the reply's item");
    expect(
      page.turn("comment:root-comment").querySelector("article")?.getAttribute("aria-expanded")
    ).toBe("true");
    await dwellOnFakeClock(page.viewport, replyItem);
    expect(page.mark.mock.calls).toEqual([
      ["root-comment", { read_through: reply.created_at }, keyedAttempt],
    ]);
  } finally {
    page.cleanup();
  }
});

test("landing on a comment's reply opens the thread it belongs to and marks that reply", async () => {
  const root = comment("root-comment", { body: "Root comment" });
  const reply = comment("reply-comment", {
    body: "Reply comment",
    created_at: "2026-09-20T00:01:00Z",
    reply_to: "root-comment",
  });
  const page = renderThreads({ comments: [root, reply] }, [], "reply-comment");
  try {
    const marked = await screen.findByText("Reply comment");
    expect(marked.closest("[data-reply]")?.getAttribute("aria-current")).toBe("true");
    expect(page.turn("comment:root-comment")).toHaveAttribute("aria-current", "true");
  } finally {
    page.cleanup();
  }
});

test("on a phone, landing on a comment opens its thread dialog and reading it marks it", async () => {
  // The reply is read in the commit that mounts the dialog, which only a loaded engine draws
  // (`MarkdownBody`): the first body of a file takes the asynchronous path.
  await warmMarkdownRenderer();
  const restoreViewport = stubViewport(PHONE_WIDTH);
  const root = comment("root-comment", { body: "Root comment" });
  const reply = comment("reply-comment", {
    body: "Reply comment",
    created_at: "2026-09-20T00:01:00Z",
    reply_to: "root-comment",
  });
  const page = renderThreads({ comments: [root, reply] }, ["root-comment"], "root-comment");
  try {
    const dialog = await screen.findByRole("dialog", { name: "Thread" });
    const replyItem = within(dialog).getByText("Reply comment").closest("li");
    if (replyItem === null) throw new Error("expected the reply's item");
    await dwellOnFakeClock(page.viewport, replyItem);
    expect(page.mark.mock.calls).toEqual([
      ["root-comment", { read_through: reply.created_at }, keyedAttempt],
    ]);
  } finally {
    page.cleanup();
    restoreViewport();
  }
});

test("without a link, a comment's thread stays collapsed", async () => {
  const root = comment("root-comment", { body: "Root comment" });
  const reply = comment("reply-comment", { body: "Reply comment", reply_to: "root-comment" });
  const page = renderThreads({ comments: [root, reply] }, ["root-comment"]);
  try {
    await screen.findByText("Root comment");
    expect(screen.queryByText("Reply comment")).not.toBeInTheDocument();
  } finally {
    page.cleanup();
  }
});

// The issue's watermark is the Conversation's to write, and only from the newest conversation:
// a single mark cannot say "I read the third card only". W38: the mark is a quiet write of the one
// write store, so it goes as the person reading (`Dispatch-Login`), under one key on every attempt,
// and a write Dispatch answers 503 is asked again under that key.
test("the read mark writes the issue's head seq as the reader, and retries an unanswered write", async () => {
  const viewport = installIntersectionObserver();
  const stub = stubLists({
    comments: [comment("comment-1", { body: "Newest" })],
    head: [headEvent(7)],
  });
  const getMyState = spyOn(api, "getMyState").mockImplementation(async () => ({
    "CORE-1": issueState([], 1),
  }));
  let writes = 0;
  const putIssueState = answerIssueStates().mockImplementation(async (_key, input) => {
    writes += 1;
    if (writes === 1) throw new ApiError(503, { code: "UNAVAILABLE", error: "down" });
    return issueState([], input.last_read_seq ?? 0);
  });
  const queryClient = newQueryClient();
  queryClient.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  const view = render(tab({ "CORE-1": issueState([], 1) }, true, queryClient));

  try {
    await screen.findByText("Newest");
    const card = view.container.querySelector('[data-turn="comment:comment-1"]');
    if (card === null) throw new Error("expected the newest card");
    await dwellOnFakeClock(viewport, card);
    expect(putIssueState.mock.calls).toEqual([["CORE-1", { last_read_seq: 7 }, keyedAttempt]]);
    expect(putIssueState.mock.calls[0]?.[2]?.login).toBe("alice");
    await jump(1_000);
    expect(putIssueState.mock.calls.map(([key, input]) => [key, input])).toEqual([
      ["CORE-1", { last_read_seq: 7 }],
      ["CORE-1", { last_read_seq: 7 }],
    ]);
    const [first, second] = putIssueState.mock.calls;
    expect(second?.[2]?.key).toBe(first?.[2]?.key);
    await waitFor(() =>
      expect(queryClient.getQueryData<UserState>(["user-state"])?.["CORE-1"]?.last_read_seq).toBe(7)
    );
  } finally {
    view.unmount();
    getMyState.mockRestore();
    putIssueState.mockRestore();
    stub.restore();
    viewport.restore();
  }
});

const NO_ARTIFACTS: ReadonlyMap<string, Artifact> = new Map();

/** CORE-1's Conversation under the viewer's states as the issue page's read of them holds them
 *  (`IssuePage`), so a write into that read reaches the tab as it does on the page. */
function ConversationUnderReadStates(): ReactNode {
  const state = useQuery(userStateQuery()).data;
  return (
    <ConversationTab
      issueArtifacts={NO_ARTIFACTS}
      isClosed={false}
      issueKey="CORE-1"
      state={state}
      visible
    />
  );
}

// A read of the viewer's issue states that was out when the read mark answered was answered from
// the states before the mark. When it lands after the mark's answer, it must not put back the
// issue's old read mark, which would count the conversation the reader just read unread again.
test("a read asked before the read mark answered does not put back the mark it replaced", async () => {
  const viewport = installIntersectionObserver();
  const stub = stubLists({
    comments: [comment("comment-1", { body: "Newest" })],
    head: [headEvent(7)],
  });
  const asked: ReturnType<typeof Promise.withResolvers<UserState>>[] = [];
  const getMyState = spyOn(api, "getMyState").mockImplementation(() => {
    const read = Promise.withResolvers<UserState>();
    asked.push(read);
    return read.promise;
  });
  const putIssueState = answerIssueStates().mockImplementation(async (_key, input) =>
    issueState([], input.last_read_seq ?? 0)
  );
  const queryClient = newQueryClient();
  queryClient.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  const lastReadSeq = () =>
    queryClient.getQueryData<UserState>(userStateQuery().queryKey)?.["CORE-1"]?.last_read_seq;
  const view = render(
    <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
      <KeymapProvider>
        <QueryClientProvider client={queryClient}>
          <ConversationUnderReadStates />
        </QueryClientProvider>
      </KeymapProvider>
    </MemoryRouter>
  );

  try {
    await waitFor(() => expect(asked).toHaveLength(1));
    await act(async () => asked[0]?.resolve({ "CORE-1": issueState([], 1) }));
    await screen.findByText("Newest");
    const card = view.container.querySelector('[data-turn="comment:comment-1"]');
    if (card === null) throw new Error("expected the newest card");

    // A read of the states goes out, as an event's refresh asks one, and is still out at the mark.
    act(() => {
      void queryClient.invalidateQueries({ queryKey: userStateQuery().queryKey });
    });
    await waitFor(() => expect(asked).toHaveLength(2));
    await dwellOnFakeClock(viewport, card);
    expect(putIssueState).toHaveBeenCalledTimes(1);
    await waitFor(() => expect(lastReadSeq()).toBe(7));

    // The read asked before the mark answers the states from before it.
    await act(async () => {
      asked[1]?.resolve({ "CORE-1": issueState([], 1) });
      await settle();
    });
    expect(lastReadSeq()).toBe(7);
  } finally {
    view.unmount();
    getMyState.mockRestore();
    putIssueState.mockRestore();
    stub.restore();
    viewport.restore();
  }
});

// The Conversation folds nothing and carries no order control: every thread in it reads the way
// the reader set the Inbox's View menu.
for (const order of ["oldest", "newest"] as const) {
  test(`a thread's replies read ${order} first`, async () => {
    const key = userPreferenceStorageKey("alice", "inbox.thread-order");
    window.localStorage.setItem(key, order);
    const stub = stubLists({
      messages: [
        thread(message("message-1"), [
          message("reply-1", { created_at: "2026-09-09T00:01:00Z", in_reply_to: "message-1" }),
          message("reply-2", { created_at: "2026-09-09T00:02:00Z", in_reply_to: "message-1" }),
        ]),
      ],
    });
    const queryClient = newQueryClient();
    // The browser's order is the signed-in person's, so the card waits on who they are.
    seedUnread(queryClient, { issue: "CORE-1" }, []);
    const view = render(tab({ "CORE-1": issueState() }, true, queryClient));

    try {
      const replies = await screen.findByRole("list", { name: "Replies" });
      await waitFor(() =>
        expect(
          Array.from(replies.querySelectorAll("li"), (item) => item.getAttribute("data-turn"))
        ).toEqual(
          order === "oldest"
            ? ["message:reply-1", "message:reply-2"]
            : ["message:reply-2", "message:reply-1"]
        )
      );
    } finally {
      view.unmount();
      stub.restore();
      window.localStorage.removeItem(key);
    }
  });
}

// INFRA-869: a message that is nothing but a reference shows the reference's card under it, as
// the margin's comments do.
test("a message that is nothing but a reference shows its card", async () => {
  const getIssueSummary = spyOn(api, "getIssueSummary").mockResolvedValue({
    documents: [],
    key: "CORE-2",
    project: "CORE",
    status: "in_progress",
    title: "The other issue",
  });
  const stub = stubLists({
    messages: [thread(message("message-1", { body: "dispatch://CORE-2" }))],
  });
  const view = render(tab({ "CORE-1": issueState() }, true, newQueryClient()));

  try {
    const turn = await waitFor(() => {
      const found = view.container.querySelector<HTMLElement>('[data-turn="message:message-1"]');
      if (found === null) throw new Error("the turn has not rendered");
      return found;
    });
    const cards = await within(turn).findByRole("region", { name: "References" });
    expect(
      within(cards)
        .getByRole("link", { name: /The other issue/ })
        .getAttribute("href")
    ).toBe("/issues/CORE-2");
  } finally {
    view.unmount();
    stub.restore();
    getIssueSummary.mockRestore();
  }
});

// INFRA-869: a turn's text is its message's, so its `#id` resolves against the question map the
// server answers for that message (its issue's spec) - the one map the reference graph stores
// its edges from.
test("a Conversation turn links the `#q2` the question map of its message names", async () => {
  const question: IssueAskRow = {
    ...baseAsk,
    block_artifact: { id: "spec-1", primary: true, slug: "spec" },
    block_id: "q2",
    id: "ask-2",
    last_reply: null,
    options: [],
    question: "Which region?",
  };
  const getReferenceQuestions = spyOn(api, "getReferenceQuestions").mockResolvedValue({
    document: "spec-1",
    questions: [{ ask: "ask-2", block_id: "q2", reference: "dispatch://CORE-1/ask/ask-2" }],
  });
  const getAsk = spyOn(api, "getAsk").mockResolvedValue({
    answers: [],
    ask: question,
    claimant: null,
    edits: [],
    followers: [],
    replies: [],
    route: null,
    route_holder: null,
    route_status: null,
    route_status_reason: null,
  });
  const stub = stubLists({
    messages: [thread(message("message-1", { body: "See #q2 for the rollout." }))],
  });
  const view = render(tab({ "CORE-1": issueState() }, true, newQueryClient()));

  try {
    const turn = await waitFor(() => {
      const found = view.container.querySelector<HTMLElement>('[data-turn="message:message-1"]');
      if (found === null) throw new Error("the turn has not rendered");
      return found;
    });
    const link = await waitFor(() => {
      const found = turn.querySelector<HTMLElement>("a[data-dispatch-ref]");
      if (found === null) throw new Error("the `#q2` is still plain text");
      return found;
    });
    expect(link.getAttribute("data-dispatch-ref")).toBe("dispatch://CORE-1/ask/ask-2");
    expect(link.getAttribute("href")).toBe("/issues/CORE-1/asks/ask-2");
    expect(getReferenceQuestions).toHaveBeenCalledWith("message", "message-1");
  } finally {
    view.unmount();
    stub.restore();
    getAsk.mockRestore();
    getReferenceQuestions.mockRestore();
  }
});

// ---------------------------------------------------------------------------
// The keys of the turns (dispatch://INFRA-2160, R22): `j` and `k` walk the turns and the replies
// drawn in them; Enter, `o`, `r`, `y` and Shift+Y act on the turn the focus is on.
// ---------------------------------------------------------------------------

/** Pushes the issue page's scope, under which the tab registers its keys (`IssuePage`). */
function IssueScope(): null {
  useKeymapScope("issue");
  return null;
}

const bob: Actor = { id: "bob", kind: "user" };

/** One conversation of each kind a turn comes in, newest activity first: a message with a reply,
 *  a comment thread with two replies, and a handoff. */
function turnLists(): Lists {
  return {
    comments: [
      comment("comment-1", {
        body: "The ledger needs a lease.",
        created_at: "2026-09-20T00:00:00Z",
      }),
      comment("reply-a", {
        author: bob,
        body: "First answer",
        created_at: "2026-09-20T00:01:00Z",
        reply_to: "comment-1",
      }),
      comment("reply-b", {
        author: bob,
        body: "Second answer",
        created_at: "2026-09-20T00:02:00Z",
        reply_to: "comment-1",
      }),
    ],
    handoffs: [handoffRecord("handoff-1", 1, '{"summary":"First pass"}', "2026-09-10T09:00:00Z")],
    messages: [
      thread(
        message("message-1", {
          body: "Who holds the lease?",
          created_at: "2026-09-21T00:00:00Z",
        }),
        [
          message("message-2", {
            author: bob,
            body: "The mirror does.",
            created_at: "2026-09-21T00:01:00Z",
            in_reply_to: "message-1",
          }),
        ]
      ),
    ],
  };
}

/** The tab under the issue page's scope, once `first` - a turn's `data-turn` - is drawn. */
async function renderTurns(lists: Lists, first: string, visible = true) {
  const stub = stubLists(lists);
  const view = render(
    <>
      <IssueScope />
      {tab({ "CORE-1": issueState() }, visible, newQueryClient())}
    </>
  );
  const turn = (id: string) => {
    const found = view.container.querySelector<HTMLElement>(`[data-turn="${id}"]`);
    if (found === null) throw new Error(`expected the ${id} turn`);
    return found;
  };
  await waitFor(() => turn(first));
  const list = screen.getByRole("list", { name: "Conversation turns" });
  return {
    cleanup: () => {
      view.unmount();
      stub.restore();
    },
    /** Every turn and reply the list draws, top to bottom. */
    rows: () => [...list.querySelectorAll<HTMLElement>("[data-turn], [data-reply]")],
    turn,
  };
}

/** The reply box of the comment thread `root` holds, once it is drawn. */
function threadBox(root: HTMLElement): Promise<HTMLElement> {
  return waitFor(() => {
    const found = root.querySelector<HTMLElement>("[data-thread-reply] [data-reply-box]");
    if (found === null) throw new Error("the thread drew no reply box");
    return found;
  });
}

test("j and k walk the Conversation's turns and the replies drawn in them, each a turn Tab does not stop on", async () => {
  const user = userEvent.setup();
  const page = await renderTurns(turnLists(), "message:message-1");
  try {
    // A comment's thread is closed, so its replies are not drawn and not walked.
    expect(page.rows().map((row) => row.getAttribute("data-turn"))).toEqual([
      "message:message-1",
      "message:message-2",
      "comment:comment-1",
      "handoff:handoff-1",
    ]);
    for (const row of page.rows()) expect(row).toHaveAttribute("tabindex", "-1");

    await user.keyboard("j");
    expect(document.activeElement).toBe(page.turn("message:message-1"));
    await user.keyboard("j");
    expect(document.activeElement).toBe(page.turn("message:message-2"));
    await user.keyboard("jj");
    expect(document.activeElement).toBe(page.turn("handoff:handoff-1"));
    // The walk stops at either end.
    await user.keyboard("j");
    expect(document.activeElement).toBe(page.turn("handoff:handoff-1"));
    await user.keyboard("k");
    expect(document.activeElement).toBe(page.turn("comment:comment-1"));
  } finally {
    page.cleanup();
  }
});

test("Enter and o open and close a comment's thread with the focus kept on the turn and its reply box standing, and j then walks its replies", async () => {
  const user = userEvent.setup();
  const page = await renderTurns(turnLists(), "comment:comment-1");
  try {
    const root = page.turn("comment:comment-1");
    act(() => root.focus());
    await user.keyboard("{Enter}");
    expect(within(root).getByRole("button", { name: "Collapse thread" })).toBeTruthy();
    // The thread's box is drawn, and takes no focus: it is standing.
    await threadBox(root);
    expect(document.activeElement).toBe(root);

    await user.keyboard("j");
    const first = (await within(root).findByText("First answer")).closest("[data-reply]");
    expect(document.activeElement).toBe(first);
    expect(first).toHaveAttribute("data-turn-ref", "dispatch://CORE-1/comment/reply-a");
    // `o` on a reply closes its thread, which takes the reply away: the thread's turn has the
    // focus.
    await user.keyboard("o");
    expect(within(root).getByRole("button", { name: "Expand thread" })).toBeTruthy();
    expect(document.activeElement).toBe(root);
    await user.keyboard("o");
    expect(within(root).getByRole("button", { name: "Collapse thread" })).toBeTruthy();
    expect(document.activeElement).toBe(root);
    await user.keyboard("{Enter}");
    expect(within(root).getByRole("button", { name: "Expand thread" })).toBeTruthy();
    expect(document.activeElement).toBe(root);
  } finally {
    page.cleanup();
  }
});

test("r puts the caret in a comment thread's reply box, opening the thread, and Escape in the empty box gives the focus back to the turn and keeps the thread open", async () => {
  const user = userEvent.setup();
  const page = await renderTurns(turnLists(), "comment:comment-1");
  try {
    const root = page.turn("comment:comment-1");
    act(() => root.focus());
    await user.keyboard("r");
    const box = await threadBox(root);
    await waitFor(() => expect(box).toHaveFocus());
    await user.keyboard("{Escape}");
    expect(document.activeElement).toBe(root);
    expect(within(root).getByRole("button", { name: "Collapse thread" })).toBeTruthy();

    // From a reply of the open thread `r` reaches the same box, and Escape gives the focus back
    // to the reply.
    await user.keyboard("j");
    const reply = document.activeElement;
    expect(reply?.hasAttribute("data-reply")).toBe(true);
    await user.keyboard("r");
    expect(box).toHaveFocus();
    await user.keyboard("{Escape}");
    expect(document.activeElement).toBe(reply);
    expect(within(root).getByRole("button", { name: "Collapse thread" })).toBeTruthy();

    // A thread the pointer opens keeps its own Escape, which closes it.
    await user.keyboard("k");
    await user.keyboard("o");
    await user.click(within(root).getByRole("button", { name: "Expand thread" }));
    await waitFor(async () => expect(await threadBox(root)).toHaveFocus());
    await user.keyboard("{Escape}");
    expect(within(root).getByRole("button", { name: "Expand thread" })).toBeTruthy();
  } finally {
    page.cleanup();
  }
});

test("r on a message presses its Reply, and Escape in the composer gives the focus back to the turn", async () => {
  const user = userEvent.setup();
  const page = await renderTurns(turnLists(), "message:message-2");
  try {
    const reply = page.turn("message:message-2");
    act(() => reply.focus());
    await user.keyboard("r");
    const composer = screen.getByRole("form", { name: "Comment composer" });
    await waitFor(() => expect(composer.querySelector("[data-reply-box]")).toHaveFocus());
    expect(within(composer).getByRole("button", { name: "Cancel reply" })).toBeTruthy();
    await user.keyboard("{Escape}");
    expect(document.activeElement).toBe(reply);
    expect(within(composer).queryByRole("button", { name: "Cancel reply" })).toBeNull();
  } finally {
    page.cleanup();
  }
});

// A composer a key or a button inside it ends gives the focus back to the control that opened it,
// or keeps it in a composer that stays, never on the page's body (dispatch://INFRA-2160, E1).

// Escape over a typed draft in the docked composer asks Discard draft?, and the question's Discard
// takes itself away with the question while it holds the focus. The docked composer stays, so the
// caret goes back to its Comment box, which held it before the question (`MentionComposer`).
test("Discard over a typed draft in the Conversation's comment composer puts the caret back in its Comment box", async () => {
  const user = userEvent.setup();
  const stub = stubLists();
  const view = render(tab({ "CORE-1": issueState() }, true, newQueryClient()));
  try {
    const composer = await screen.findByRole("form", { name: "Comment composer" });
    // A string `name` matches the whole accessible name, so no box whose name only holds the
    // word "Comment" is taken for it.
    const field = within(composer).getByRole<HTMLTextAreaElement>("textbox", { name: "Comment" });
    await user.click(field);
    await user.keyboard("Half a thought about the clock");
    await user.keyboard("{Escape}");
    const question = within(composer).getByRole("alert");
    expect(question).toHaveTextContent("Discard draft?");
    await user.click(within(question).getByRole("button", { name: "Discard" }));
    expect(within(composer).queryByRole("alert")).toBeNull();
    expect(field.value).toBe("");
    expect(field).toHaveFocus();
  } finally {
    view.unmount();
    stub.restore();
  }
});

// A second Escape over Discard draft? in a comment's edit composer ends the edit unsaved and takes
// the editor away with the focus it holds: the focus goes back to the comment's Edit, which
// opened it (`ThreadCard`, `useFocusBack`).
test("a second Escape over Discard draft? in a Conversation comment's edit composer gives the focus back to the comment's Edit", async () => {
  const user = userEvent.setup();
  const stub = stubLists({
    comments: [comment("comment-1", { body: "Why is the clock shared?" })],
  });
  const queryClient = newQueryClient();
  queryClient.setQueryData(["whoami"], { kind: "user", login: "alice" });
  const view = render(tab({ "CORE-1": issueState() }, true, queryClient));
  try {
    const card = await screen.findByTestId("margin-comment-comment-1");
    await user.click(screen.getByRole("button", { name: "Expand thread" }));
    await user.click(await within(card).findByRole("button", { name: "Edit" }));
    const field = within(card).getByRole("textbox", { name: "Edit comment" });
    await user.click(field);
    await user.keyboard(" And since when?");
    await user.keyboard("{Escape}");
    expect(within(card).getByRole("alert")).toHaveTextContent("Discard draft?");
    expect(field).toHaveFocus();
    await user.keyboard("{Escape}");
    expect(within(card).queryByRole("textbox", { name: "Edit comment" })).toBeNull();
    expect(within(card).getByRole("button", { name: "Edit" })).toHaveFocus();
  } finally {
    view.unmount();
    stub.restore();
  }
});

// A thread Expand thread opened, not the tab's `r`: Escape in its reply box ends the reply and
// closes the thread, which takes the box away with the focus it holds. The focus goes to the
// turn's Expand thread, which opened the thread (`CommentTurn`, `useFocusBack`).
test("Escape in the reply box of a thread Expand thread opened closes the thread with the focus on its Expand thread", async () => {
  const user = userEvent.setup();
  const page = await renderTurns(turnLists(), "comment:comment-1");
  try {
    const root = page.turn("comment:comment-1");
    await user.click(within(root).getByRole("button", { name: "Expand thread" }));
    await user.click(await threadBox(root));
    await user.keyboard("{Escape}");
    expect(root.querySelector("[data-thread-reply]")).toBeNull();
    const toggle = within(root).getByRole("button", { name: "Expand thread" });
    expect(toggle).toHaveAttribute("aria-expanded", "false");
    expect(toggle).toHaveFocus();
  } finally {
    page.cleanup();
  }
});

// A thread `r` opened, whose reply Dispatch refused: the reply box's Discard draft drops the reply
// and ends it as its Cancel reply does there (`onReplyCancel`, `giveBackFromThread`), so the
// thread stays open, its box empty, and the focus goes back to the turn `r` was pressed on.
test("Discard draft over a refused reply in a thread r opened gives the focus back to the turn and keeps the thread open", async () => {
  const user = userEvent.setup();
  const createComment = spyOn(api, "createComment").mockRejectedValue(
    new ApiError(409, { code: "UNAVAILABLE", error: "the server is down" })
  );
  const page = await renderTurns(turnLists(), "comment:comment-1");
  try {
    const root = page.turn("comment:comment-1");
    act(() => root.focus());
    await user.keyboard("r");
    const box = await threadBox(root);
    await waitFor(() => expect(box).toHaveFocus());
    await user.paste("A reply Dispatch refuses");
    await user.keyboard("{Control>}{Enter}{/Control}");
    await user.click(await within(root).findByRole("button", { name: "Discard draft" }));
    expect(within(root).getByRole("button", { name: "Collapse thread" })).toBeTruthy();
    expect(((await threadBox(root)) as HTMLTextAreaElement).value).toBe("");
    expect(document.activeElement).toBe(root);
  } finally {
    page.cleanup();
    createComment.mockRestore();
  }
});

// A reply the docked composer takes from a turn's Reply, pressed with the pointer: Cancel reply
// ends it and takes itself away with the focus it holds, and the focus goes back to the Reply that
// started the reply, which the press hands on (`ReplyButton`, `beginDockedReply`). A comment's
// Reply and a message reply's Reply each start one.
for (const [kind, turnId] of [
  ["a comment's", "comment:comment-1"],
  ["a message reply's", "message:message-2"],
] as const) {
  test(`Cancel reply in the Conversation's docked composer gives the focus back to ${kind} Reply that started the reply`, async () => {
    const user = userEvent.setup();
    const page = await renderTurns(turnLists(), turnId);
    try {
      const composer = screen.getByRole("form", { name: "Comment composer" });
      await user.click(within(page.turn(turnId)).getByRole("button", { name: "Reply" }));
      const cancel = await within(composer).findByRole("button", { name: "Cancel reply" });
      await user.click(cancel);
      expect(within(composer).queryByRole("button", { name: "Cancel reply" })).toBeNull();
      expect(within(page.turn(turnId)).getByRole("button", { name: "Reply" })).toHaveFocus();
    } finally {
      page.cleanup();
    }
  });
}

// A reply the docked composer took from a message's Reply, whose message has left the list since:
// Cancel reply ends it and takes itself away with the focus it holds, and no Reply is left in the
// page to give the focus back to (`giveBackFromDockedReply`). The composer stays, so the caret is
// in its field, where Cancel reply put it first (`MentionComposer`'s `cancelReply`).
test("Cancel reply in the Conversation's docked composer, once the message whose Reply started the reply has left the list, puts the caret in its field", async () => {
  const user = userEvent.setup();
  const stub = stubLists(turnLists());
  const queryClient = newQueryClient();
  const view = render(tab({ "CORE-1": issueState() }, true, queryClient));
  try {
    const composer = await screen.findByRole("form", { name: "Comment composer" });
    const turn = await waitFor(() => {
      const found = view.container.querySelector<HTMLElement>('[data-turn="message:message-2"]');
      if (found === null) throw new Error("expected the message:message-2 turn");
      return found;
    });
    await user.click(within(turn).getByRole("button", { name: "Reply" }));
    await within(composer).findByRole("button", { name: "Cancel reply" });
    stub.lists.messages = [];
    await act(() => queryClient.invalidateQueries({ queryKey: ["issue-messages", "CORE-1"] }));
    await waitFor(() => expect(turn.isConnected).toBe(false));

    await user.click(within(composer).getByRole("button", { name: "Cancel reply" }));
    expect(within(composer).queryByRole("button", { name: "Cancel reply" })).toBeNull();
    expect(within(composer).getByRole("textbox", { name: "Comment" })).toHaveFocus();
  } finally {
    view.unmount();
    stub.restore();
  }
});

// A turn the focus is given back to may stand anywhere in the list, far above the docked
// composer or a long thread's box, and the browser's own scroll of a focused element centres one
// not wholly in view: each turn takes the focus with no scroll of that kind, and scrolls to its
// nearest edge only where the reader cannot see it whole (`focusIntoView`). Escape in a thread's
// box or in the docked composer gives a turn back, and so does `o` on a reply, which takes the
// reply away with the thread it closes.
test("a turn given back the focus, by Escape from a reply box or by o on a reply, takes it with no scroll of the browser's own", async () => {
  const user = userEvent.setup();
  /** Each focus that moved the focus to a turn or a reply since the last read: the turn's
   *  `data-turn`, else `reply`, and whether it was asked to scroll nothing. */
  let given: { readonly at: string; readonly still: boolean }[] = [];
  const page = await renderTurns(turnLists(), "comment:comment-1");
  const focus = HTMLElement.prototype.focus;
  const focuses = spyOn(HTMLElement.prototype, "focus").mockImplementation(function (
    this: HTMLElement,
    options?: FocusOptions
  ) {
    if (this.matches("[data-turn], [data-reply]") && document.activeElement !== this) {
      given.push({ at: this.dataset.turn ?? "reply", still: options?.preventScroll === true });
    }
    focus.call(this, options);
  });
  try {
    const root = page.turn("comment:comment-1");
    act(() => root.focus());
    await user.keyboard("r");
    await waitFor(async () => expect(await threadBox(root)).toHaveFocus());
    given = [];
    await user.keyboard("{Escape}");
    expect(document.activeElement).toBe(root);
    const fromThreadBox = given;
    given = [];

    await user.keyboard("j");
    expect(document.activeElement?.hasAttribute("data-reply")).toBe(true);
    given = [];
    await user.keyboard("o");
    expect(document.activeElement).toBe(root);
    const fromReply = given;
    given = [];

    const message = page.turn("message:message-2");
    act(() => message.focus());
    await user.keyboard("r");
    const composer = screen.getByRole("form", { name: "Comment composer" });
    await waitFor(() => expect(composer.querySelector("[data-reply-box]")).toHaveFocus());
    given = [];
    await user.keyboard("{Escape}");
    expect(document.activeElement).toBe(message);

    expect({ fromDockedComposer: given, fromReply, fromThreadBox }).toEqual({
      fromDockedComposer: [{ at: "message:message-2", still: true }],
      fromReply: [{ at: "comment:comment-1", still: true }],
      fromThreadBox: [{ at: "comment:comment-1", still: true }],
    });
  } finally {
    page.cleanup();
    focuses.mockRestore();
  }
});

// A post its author deleted takes no reply (409 POST_DELETED), so its turn draws no Reply and its
// thread no reply box: `r` on a deleted comment's turn has no box to put the caret in, so it opens
// no thread and the focus stays on the turn, as on a handoff, which takes no reply either.
test("r on a comment its author deleted opens no thread and leaves the focus on the turn", async () => {
  const user = userEvent.setup();
  const lists = turnLists();
  const page = await renderTurns(
    {
      ...lists,
      comments: (lists.comments ?? []).map((item) =>
        item.id === "comment-1"
          ? { ...item, body: "", deleted_at: "2026-09-20T00:05:00Z", deleted_by: alice }
          : item
      ),
    },
    "comment:comment-1"
  );
  try {
    const root = page.turn("comment:comment-1");
    act(() => root.focus());
    await user.keyboard("r");
    expect({
      expanded: within(root).queryByRole("button", { name: "Collapse thread" }) !== null,
      focused: document.activeElement === root,
    }).toEqual({ expanded: false, focused: true });
  } finally {
    page.cleanup();
  }
});

test("y and Shift+Y copy the focused turn's reference and its page address, and on a handoff, which has neither, copy nothing", async () => {
  const user = userEvent.setup();
  const originalClipboard = navigator.clipboard;
  const writeText = spyOn({ writeText: async () => undefined }, "writeText");
  Object.defineProperty(navigator, "clipboard", { configurable: true, value: { writeText } });
  const page = await renderTurns(turnLists(), "handoff:handoff-1");
  try {
    act(() => page.turn("comment:comment-1").focus());
    await user.keyboard("y");
    await waitFor(() =>
      expect(writeText).toHaveBeenLastCalledWith("dispatch://CORE-1/comment/comment-1")
    );
    await user.keyboard("{Shift>}Y{/Shift}");
    await waitFor(() =>
      expect(writeText).toHaveBeenLastCalledWith(
        `${window.location.origin}/issues/CORE-1/comments/comment-1`
      )
    );
    act(() => page.turn("message:message-2").focus());
    await user.keyboard("y");
    await waitFor(() =>
      expect(writeText).toHaveBeenLastCalledWith("dispatch://CORE-1/message/message-2")
    );

    const handoff = page.turn("handoff:handoff-1");
    act(() => handoff.focus());
    const copies = writeText.mock.calls.length;
    await user.keyboard("y{Shift>}Y{/Shift}");
    expect(writeText.mock.calls.length).toBe(copies);
    // Enter opens the handoff's other fold, its fields, and the focus stays on it.
    await user.keyboard("{Enter}");
    expect(within(handoff).getByRole("button", { name: "Hide fields" })).toBeTruthy();
    expect(document.activeElement).toBe(handoff);
  } finally {
    page.cleanup();
    Object.defineProperty(navigator, "clipboard", { configurable: true, value: originalClipboard });
  }
});

test("Enter on an ask's turn opens its earlier replies, and r on an answered ask puts the caret in its reply box", async () => {
  const user = userEvent.setup();
  const replies = ["First", "Second", "Third"].map((body, index) =>
    comment(`ask-reply-${index + 1}`, {
      ask_id: "ask-1",
      author: bob,
      body: `${body} reply`,
      created_at: `2026-09-10T09:0${index + 1}:00Z`,
    })
  );
  const answered = askRow(
    {
      answer: { at: "2026-09-10T09:00:30Z", selected: ["Ship"], text: null, user: "alice" },
      state: "answered",
    },
    replies
  );
  const page = await renderTurns({ asks: [answered], comments: replies }, "ask:ask-1");
  try {
    const ask = page.turn("ask:ask-1");
    expect(ask).toHaveAttribute("data-turn-ref", "dispatch://CORE-1/ask/ask-1");
    const fold = await within(ask).findByRole("button", { name: "Show 1 earlier reply" });
    act(() => ask.focus());
    await user.keyboard("{Enter}");
    expect(fold).toHaveAttribute("aria-expanded", "true");
    expect(document.activeElement).toBe(ask);
    // Each reply is a turn, with its own reference.
    const drawn = [...ask.querySelectorAll("[data-reply]")];
    expect(drawn.map((row) => row.getAttribute("data-turn-ref"))).toEqual(
      expect.arrayContaining(replies.map(({ id }) => `dispatch://CORE-1/comment/${id}`))
    );
    expect(drawn).toHaveLength(3);

    await user.keyboard("r");
    await waitFor(() => expect(document.activeElement?.matches("[data-reply-box]")).toBe(true));
    expect(ask.contains(document.activeElement)).toBe(true);
  } finally {
    page.cleanup();
  }
});

test("every comment, reply and message the Conversation draws has its own Copy reference, and a comment's own is drawn once", async () => {
  const user = userEvent.setup();
  const page = await renderTurns(turnLists(), "comment:comment-1");
  try {
    await user.click(
      within(page.turn("comment:comment-1")).getByRole("button", { name: "Expand thread" })
    );
    await screen.findByText("Second answer");
    for (const reference of [
      "message/message-1",
      "message/message-2",
      "comment/comment-1",
      "comment/reply-a",
      "comment/reply-b",
    ]) {
      expect(
        screen.getAllByRole("button", { name: `Copy reference dispatch://CORE-1/${reference}` })
      ).toHaveLength(1);
    }
    expect(page.turn("handoff:handoff-1").querySelector("[data-copy-ref]")).toBeNull();
    expect(page.turn("handoff:handoff-1").hasAttribute("data-turn-ref")).toBe(false);
  } finally {
    page.cleanup();
  }
});

test("the turns' keys stand down while another tab is shown", async () => {
  const user = userEvent.setup();
  const page = await renderTurns(turnLists(), "message:message-1", false);
  try {
    await user.keyboard("j");
    expect(document.activeElement).toBe(document.body);
    act(() => page.turn("comment:comment-1").focus());
    await user.keyboard("{Enter}");
    expect(
      within(page.turn("comment:comment-1")).getByRole("button", { name: "Expand thread" })
    ).toBeTruthy();
  } finally {
    page.cleanup();
  }
});

// A turn is a row of the tab's list: an `<li>`, a row by its element alone (`row-marker.ts`'s
// `ROW_SELECTOR`). A refetch that takes the turn holding the focus away is a loss the reader did
// not cause: no press of theirs armed a hand-off (`causedByReader`), so in the page's main content,
// as the shell draws it, the focus goes into no other turn: the reader's next `r` or `y` acts on
// no conversation they did not pick.
test("a turn a refetch takes away from under the focus hands it to no other turn", async () => {
  const stub = stubLists(turnLists());
  const queryClient = newQueryClient();
  const view = render(
    <main>
      <IssueScope />
      {tab({ "CORE-1": issueState() }, true, queryClient)}
    </main>
  );
  try {
    const turn = await waitFor(() => {
      const found = view.container.querySelector<HTMLElement>('[data-turn="comment:comment-1"]');
      if (found === null) throw new Error("expected the comment:comment-1 turn");
      return found;
    });
    const expand = within(turn).getByRole("button", { name: "Expand thread" });
    act(() => expand.focus());

    stub.lists.comments = [];
    await act(async () => {
      await queryClient.refetchQueries();
    });
    await waitFor(() => expect(turn).not.toBeInTheDocument());
    await afterFrameAndTasks();

    expect(document.activeElement?.closest("[data-turn], [data-reply]") ?? null).toBeNull();
    expect(view.container.querySelector('[data-turn="handoff:handoff-1"]')).not.toBeNull();
  } finally {
    view.unmount();
    stub.restore();
  }
});

test("the Conversation marks an edited message and reads a deleted one as deleted", async () => {
  const root = message("message-1", { edited_at: "2026-09-09T01:00:00Z" });
  // The deleted reply keeps its body, "Body of message-2", beside the marks, so only the
  // Conversation's own reading of the marks keeps it off the page.
  const deletedReply = message("message-2", {
    created_at: "2026-09-09T02:00:00Z",
    deleted_at: "2026-09-09T03:00:00Z",
    deleted_by: { id: "alice", kind: "user" },
    in_reply_to: "message-1",
  });
  const stub = stubLists({ messages: [thread(root, [deletedReply])] });
  const queryClient = newQueryClient();
  const view = render(tab({ "CORE-1": issueState() }, true, queryClient));

  try {
    const turn = await screen.findByText("Body of message-1");
    const card = turn.closest("[data-turn]") as HTMLElement;
    expect(card.textContent).toContain("edited");
    await waitFor(() => expect(card.textContent).toContain("Deleted by alice"));
    expect(card.textContent).not.toContain("Body of message-2");
  } finally {
    view.unmount();
    stub.restore();
  }
});

// An author who edits a reply and then deletes it leaves both marks on the record, and the
// reader meets only the deleted line: ` · edited` points at a body no read of the reply gives any more.
test("a deleted reply its author had edited reads as deleted, with no edited mark", async () => {
  const root = message("message-1");
  const deletedReply = message("message-2", {
    body: "",
    created_at: "2026-09-09T02:00:00Z",
    deleted_at: "2026-09-09T04:00:00Z",
    deleted_by: alice,
    edited_at: "2026-09-09T03:00:00Z",
    in_reply_to: "message-1",
  });
  const stub = stubLists({ messages: [thread(root, [deletedReply])] });
  const queryClient = newQueryClient();
  const view = render(tab({ "CORE-1": issueState() }, true, queryClient));

  try {
    const deletedLine = await waitFor(() => {
      const found = view.container.querySelector<HTMLElement>("[data-post-deleted]");
      if (found === null) throw new Error("the deleted reply is not on the page yet");
      return found;
    });
    const reply = deletedLine.closest("[data-reply]") as HTMLElement;
    expect(reply.textContent).toContain("Deleted by alice");
    expect(reply.textContent).not.toContain("edited");
  } finally {
    view.unmount();
    stub.restore();
  }
});

// The quote under a reply prints its parent, so a parent its author edited carries ` · edited`
// there too, as its own byline does; a parent nobody edited does not.
test("a reply's quote of an edited parent marks the parent as edited, and of a standing one does not", async () => {
  const reply = (id: string, parent: string) =>
    message(id, { body: `Answer ${id}.`, created_at: "2026-09-09T02:00:00Z", in_reply_to: parent });
  const stub = stubLists({
    messages: [
      thread(message("message-1", { body: "Alpha.", edited_at: "2026-09-09T01:00:00Z" }), [
        reply("message-2", "message-1"),
      ]),
      thread(message("message-3", { body: "Gamma." }), [reply("message-4", "message-3")]),
    ],
  });
  const view = render(tab({ "CORE-1": issueState() }, true, newQueryClient()));
  const turn = (id: string) => view.container.querySelector(`[data-turn="message:${id}"]`);

  try {
    await waitFor(() =>
      expect(turn("message-2")?.textContent).toContain("Replying to alice · edited — Alpha.")
    );
    await waitFor(() =>
      expect(turn("message-4")?.textContent).toContain("Replying to alice — Gamma.")
    );
  } finally {
    view.unmount();
    stub.restore();
  }
});

// Reply takes the message as it stands when it is pressed, and a draft can be open when its
// author deletes it. The composer's quote reads the message from the Conversation's own read, so
// once the delete lands it says the message is deleted and gives no part of the body the draft
// took; until then it quotes the standing message as ever.
test("the composer's quote of a message deleted while the draft is open reads as deleted, with no part of its body", async () => {
  const user = userEvent.setup();
  const stub = stubLists({ messages: [thread(message("message-1", { body: "Alpha." }))] });
  const queryClient = newQueryClient();
  const view = render(tab({ "CORE-1": issueState() }, true, queryClient));
  const composer = () => screen.getByRole("form", { name: "Comment composer" });

  try {
    const turn = (await screen.findByText("Alpha.")).closest("[data-turn]") as HTMLElement;
    await user.click(within(turn).getByRole("button", { name: "Reply" }));
    await waitFor(() => expect(composer().textContent).toContain("Replying to alice — Alpha."));

    stub.lists.messages = [
      thread(
        message("message-1", { body: "", deleted_at: "2026-09-09T03:00:00Z", deleted_by: alice })
      ),
    ];
    await act(() => queryClient.invalidateQueries({ queryKey: ["issue-messages", "CORE-1"] }));

    await waitFor(() => expect(composer().textContent).toContain("Deleted by alice"));
    expect(composer().textContent).toContain("Replying to alice");
    expect(composer().textContent).not.toContain("Alpha.");
  } finally {
    view.unmount();
    stub.restore();
  }
});

// A post its author deleted takes no reply (409 POST_DELETED), so the Conversation offers no
// Reply on one - a message, a reply to one, a comment - as the margin's card draws no reply
// composer under a deleted root; a standing post beside each keeps its Reply.
test("the Conversation offers no Reply on a deleted message, reply or comment, and a standing one keeps it", async () => {
  const deleted = { deleted_at: "2026-09-21T00:00:00Z", deleted_by: alice };
  const stub = stubLists({
    comments: [
      comment("comment-1", deleted),
      comment("comment-2", { created_at: "2026-09-20T01:00:00Z" }),
    ],
    messages: [
      thread(message("message-1", deleted), [
        message("message-2", { created_at: "2026-09-09T01:00:00Z", in_reply_to: "message-1" }),
      ]),
      thread(message("message-3", { created_at: "2026-09-09T02:00:00Z" }), [
        message("message-4", {
          ...deleted,
          created_at: "2026-09-09T03:00:00Z",
          in_reply_to: "message-3",
        }),
      ]),
    ],
  });
  const view = render(tab({ "CORE-1": issueState() }, true, newQueryClient()));
  const turn = (id: string) => view.container.querySelector(`[data-turn="${id}"]`);

  try {
    await screen.findByText("Body of message-2");
    await screen.findByText("Body of comment-2");
    expect({
      deletedComment: ownReplies(turn("comment:comment-1")),
      deletedMessage: ownReplies(turn("message:message-1")),
      deletedReply: ownReplies(turn("message:message-4")),
      standingComment: ownReplies(turn("comment:comment-2")),
      standingMessage: ownReplies(turn("message:message-3")),
      standingReply: ownReplies(turn("message:message-2")),
    }).toEqual({
      deletedComment: 0,
      deletedMessage: 0,
      deletedReply: 0,
      standingComment: 1,
      standingMessage: 1,
      standingReply: 1,
    });
  } finally {
    view.unmount();
    stub.restore();
  }
});

// A reply open when its message is deleted can no longer go: the message takes no reply (409
// POST_DELETED), so Send says so and sends nothing, and Cancel reply takes the words the reader
// wrote to the issue's own address, where they go as a message that answers none.
test("a reply open when its message is deleted sends nothing, and Cancel reply takes its words to the issue", async () => {
  const user = userEvent.setup();
  const stub = stubLists({ messages: [thread(message("message-1", { body: "Alpha." }))] });
  const queryClient = newQueryClient();
  const view = render(tab({ "CORE-1": issueState() }, true, queryClient));
  const composer = () => screen.getByRole("form", { name: "Comment composer" });
  const send = () => within(composer()).getByRole("button", { name: "Send" });
  const field = () =>
    within(composer()).getByRole<HTMLTextAreaElement>("textbox", { name: "Comment" });

  try {
    const turn = (await screen.findByText("Alpha.")).closest("[data-turn]") as HTMLElement;
    await user.click(within(turn).getByRole("button", { name: "Reply" }));
    await user.click(field());
    await user.keyboard("{Control>}a{/Control}");
    await user.paste("Ship it.");
    stub.lists.messages = [
      thread(
        message("message-1", { body: "", deleted_at: "2026-09-09T03:00:00Z", deleted_by: alice })
      ),
    ];
    await act(() => queryClient.invalidateQueries({ queryKey: ["issue-messages", "CORE-1"] }));
    await waitFor(() => expect(composer().textContent).toContain("Deleted by alice"));
    const deleted = { refusal: send().getAttribute("title"), text: field().value };
    await user.click(within(composer()).getByRole("button", { name: "Cancel reply" }));
    expect({
      cancelled: {
        quoted: composer().textContent?.includes("Replying to") ?? false,
        refusal: send().getAttribute("title"),
        text: field().value,
      },
      deleted,
    }).toEqual({
      cancelled: { quoted: false, refusal: null, text: "Ship it." },
      deleted: {
        refusal:
          "The message this reply answers was deleted, so it takes no reply. Press Cancel reply to send it as a message that answers none.",
        text: "Ship it.",
      },
    });
  } finally {
    view.unmount();
    stub.restore();
  }
});
