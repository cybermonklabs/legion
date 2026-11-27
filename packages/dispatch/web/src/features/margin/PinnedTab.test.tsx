import { beforeAll, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { correction, correctionFeed } from "../../__tests__/correction-feed";
import { messageCorrectionFields } from "../../__tests__/message-fixture";
import { api } from "../../api/client";
import { useEventFeed } from "../../api/event-feed";
import { whoAmIQuery } from "../../api/queries";
import type { Comment, DispatchEvent, MessageWithTrees } from "../../api/types";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { comment as commentFixture } from "./margin-fixture";
import { PinnedTab } from "./PinnedTab";
import { type MarginOwner, type MarkPlacement, useMarginItems } from "./useMarginItems";

// The rows draw through the shared renderer, whose Markdown engine loads lazily, and they run in
// random order, so the file loads the engine before its first row, whichever row that is.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

const messageEvent: DispatchEvent = {
  actor: { id: "alice", kind: "user" },
  created_at: "2026-09-10T00:00:00Z",
  id: 1,
  issue_key: "CORE-1",
  notify: false,
  payload: {
    author: { id: "alice", kind: "user" },
    body: "Pinned message\n\n- item",
    broadcast_id: null,
    reply_block: null,
    created_at: "2026-09-10T00:00:00Z",
    deliveries: [],
    id: "message-1",
    in_reply_to: null,
    issue_key: "CORE-1",
    target: null,
  },
  seq: 1,
  type: "message.created",
};

test("PinnedTab renders pinned event bodies as Markdown", async () => {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  const view = render(
    <QueryClientProvider client={queryClient}>
      <PinnedTab
        conversations={[]}
        events={[messageEvent]}
        onUnpin={() => {}}
        onUnpinConversation={() => {}}
        pinnedIds={["1"]}
      />
    </QueryClientProvider>
  );
  try {
    expect(await screen.findByRole("list")).not.toBeNull();
    expect(screen.getByRole("listitem").textContent).toBe("item");
  } finally {
    view.unmount();
  }
});

test("a pinned lifecycle event remains removable", async () => {
  const user = userEvent.setup();
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  const lifecycleEvent: Extract<DispatchEvent, { type: "comment.resolved" }> = {
    actor: { id: "alice", kind: "user" },
    created_at: "2026-09-10T00:01:00Z",
    id: 2,
    issue_key: "CORE-1",
    notify: false,
    payload: {
      anchor: null,
      artifact_name: "spec",
      ask_id: null,
      author: { id: "alice", kind: "user" },
      answers: null,
      artifact_id: null,
      body: "Pinned lifecycle comment",
      reply_block: null,
      created_at: "2026-09-10T00:00:00Z",
      deliveries: [],
      edited_at: null,
      id: "comment-1",
      issue_key: "CORE-1",
      mentions: [],
      reply_to: null,
      resolved: true,
      resolved_at: "2026-09-10T00:01:00Z",
      resolved_by: { id: "alice", kind: "user" },
      suggestion: null,
      turn: null,
    },
    seq: 2,
    type: "comment.resolved",
  };
  const removed: number[] = [];
  const view = render(
    <QueryClientProvider client={queryClient}>
      <PinnedTab
        conversations={[]}
        events={[lifecycleEvent]}
        onUnpin={(eventId) => removed.push(eventId)}
        onUnpinConversation={() => {}}
        pinnedIds={["2"]}
      />
    </QueryClientProvider>
  );

  try {
    await user.click(await screen.findByRole("button", { name: "Unpin" }));
    expect(removed).toEqual([2]);
  } finally {
    view.unmount();
  }
});

// A pinned card's byline names its writer through `AgentName`: an agent's name links to its
// live conversation, on a pinned conversation and on a pinned event alike.
test("a pinned card's agent byline links to its live conversation", () => {
  const listAgents = spyOn(api, "listAgents").mockResolvedValue([]);
  const agent = {
    id: "session-one",
    kind: "session" as const,
    origin: { session_title: "Implementer" },
  };
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <PinnedTab
          conversations={[
            {
              at: "2026-09-10T00:00:00Z",
              author: agent,
              body: "Pinned question",
              deleted: null,
              edited: null,
              id: "ask:ask-1",
              issue: "CORE-1",
              kind: "ask",
              recordId: "ask-1",
            },
          ]}
          events={[{ ...messageEvent, actor: agent }]}
          onUnpin={() => {}}
          onUnpinConversation={() => {}}
          pinnedIds={["ask:ask-1", "1"]}
        />
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    const names = screen.getAllByRole("link", { name: "Implementer" });
    expect(names.map((name) => name.getAttribute("href"))).toEqual([
      "/agents/session-one/live",
      "/agents/session-one/live",
    ]);
  } finally {
    view.unmount();
    listAgents.mockRestore();
  }
});

test("a pinned conversation whose post was deleted says so in place of its body", () => {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <PinnedTab
          conversations={[
            {
              at: "2026-09-10T00:00:00Z",
              author: { id: "alice", kind: "user" },
              body: "",
              deleted: { at: "2026-09-10T00:05:00Z", by: { id: "alice", kind: "user" } },
              edited: null,
              id: "comment:comment-1",
              issue: "CORE-1",
              kind: "comment",
              recordId: "comment-1",
            },
          ]}
          events={[]}
          onUnpin={() => {}}
          onUnpinConversation={() => {}}
          pinnedIds={["comment:comment-1"]}
        />
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    expect(screen.getByText(/Deleted by/)).not.toBeNull();
  } finally {
    view.unmount();
  }
});

// A pinned event prints its post's body as the event recorded it, and the read of the event gives
// it the post's own edit time (`attachPostMarks` in `api/events.go`): the card of a message its
// author edited since says so in its byline, as a pinned conversation's card does, and the card
// of a message nobody edited does not.
test("a pinned message event its author edited marks its byline edited, and one nobody edited does not", () => {
  const edited: DispatchEvent = {
    actor: { id: "alice", kind: "user" },
    created_at: "2026-09-10T00:00:00Z",
    id: 2,
    issue_key: "CORE-1",
    notify: false,
    payload: {
      author: { id: "alice", kind: "user" },
      body: "The message as first written",
      broadcast_id: null,
      reply_block: null,
      created_at: "2026-09-10T00:00:00Z",
      deliveries: [],
      edited_at: "2026-09-10T00:05:00Z",
      id: "message-2",
      in_reply_to: null,
      issue_key: "CORE-1",
      target: null,
    },
    seq: 2,
    type: "message.created",
  };
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <PinnedTab
          conversations={[]}
          events={[messageEvent, edited]}
          onUnpin={() => {}}
          onUnpinConversation={() => {}}
          pinnedIds={["1", "2"]}
        />
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    const [standing, corrected] = view.container.querySelectorAll("article");
    expect(corrected?.textContent).toContain("alice · edited");
    expect(standing?.textContent).toContain("alice");
    expect(standing?.textContent).not.toContain("edited");
  } finally {
    view.unmount();
  }
});

// The read of an event of a post its author edited and then deleted gives it both marks and no
// body (`attachPostMarks` in `api/events.go`). The card must not depend on that: this event still
// holds the body it was written with, and the deleted line takes its place (`PostBody`), so no
// part of that body prints. The byline carries no ` · edited`: the deleted line already says
// what became of the text.
test("a pinned message event of a post edited and then deleted reads as deleted, and its byline is not marked edited", async () => {
  const editedThenDeleted: DispatchEvent = {
    ...messageEvent,
    payload: {
      author: { id: "alice", kind: "user" },
      body: "Pinned message\n\n- item",
      broadcast_id: null,
      reply_block: null,
      created_at: "2026-09-10T00:00:00Z",
      deleted_at: "2026-09-10T00:06:00Z",
      deleted_by: { id: "alice", kind: "user" },
      deliveries: [],
      edited_at: "2026-09-10T00:05:00Z",
      id: "message-1",
      in_reply_to: null,
      issue_key: "CORE-1",
      target: null,
    },
  };
  // A standing post beside it, whose own body is what the assertions below wait for: the
  // markdown of this render is asynchronous, so without that wait they run while no body has
  // drawn at all, and the one that says the deleted card holds no body is green whatever the
  // card does (dispatch://INFRA-1523).
  const standing: DispatchEvent = {
    ...messageEvent,
    id: 2,
    payload: {
      ...editedThenDeleted.payload,
      body: "Standing message\n\n- item",
      deleted_at: null,
      deleted_by: null,
      edited_at: null,
      id: "message-2",
    },
  };
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <PinnedTab
          conversations={[]}
          events={[editedThenDeleted, standing]}
          onUnpin={() => {}}
          onUnpinConversation={() => {}}
          pinnedIds={["1"]}
        />
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    expect(await screen.findByText("Standing message")).not.toBeNull();
    const [card] = view.container.querySelectorAll("article");
    expect(card?.textContent).not.toContain("Pinned message");
    expect(card?.textContent).toContain("Deleted by alice");
    expect(card?.textContent).not.toContain("edited");
  } finally {
    view.unmount();
  }
});

// R2 and R3 of dispatch://INFRA-1523, and the architect's ruling of 2026-11-20 00:23Z. The Pinned
// tab draws its cards from the margin's own reads - a pinned comment from the issue's comments, a
// pinned message from its own read, a pinned event from the issue's events - and says nothing
// when a refetch of one fails: each keeps what it held, so a post's correction writes its own
// facts into every read the cache holds (`markCorrectedPost`). After refetches that all fail,
// each card of a corrected post reads as edited, then as deleted, with no word of its body.

const PINNED_COMMENT = "2c3d4e5f-6a7b-4c8d-9e0f-1a2b3c4d5e6f";
const PINNED_MESSAGE = "3d4e5f6a-7b8c-4d9e-8f1a-2b3c4d5e6f7a";
const MARGIN_OWNER: MarginOwner = { key: "CORE-1", kind: "issue" };
const NO_PLACEMENTS: ReadonlyMap<string, MarkPlacement> = new Map();
const ALICE = { id: "alice", kind: "user" } as const;

const pinnedComment: Comment = {
  ...commentFixture,
  anchor: null,
  body: "Hold the deploy.",
  id: PINNED_COMMENT,
};
const pinnedMessage: MessageWithTrees = {
  author: ALICE,
  body: "Ship on Friday.",
  broadcast_id: null,
  created_at: "2026-09-10T00:00:00Z",
  deliveries: [],
  id: PINNED_MESSAGE,
  in_reply_to: null,
  issue_key: "CORE-1",
  reply_block: null,
  target: null,
  ...messageCorrectionFields(),
};

/** The margin's Pinned tab on CORE-1 with the page's event feed beside it, holding `pins` (each
 *  `<kind>:<id>`, as the reader's state records a pin) and answering `events` for the pinned
 *  events; each card is an `article`. Every read the tab draws from fails once `fail` is called. */
function openPinnedTab(pins: readonly string[], events: readonly DispatchEvent[]) {
  let failing = false;
  const refusing =
    <T,>(answer: T) =>
    async (): Promise<T> => {
      if (failing) throw new Error("the margin's read failed");
      return answer;
    };
  const stubs = [
    spyOn(api, "getInbox").mockResolvedValue([]),
    spyOn(api, "getMyState").mockResolvedValue({
      "CORE-1": {
        dismissed: pins.map((pin) => `pinned_items:${pin}`),
        last_read_seq: 0,
        pinned: false,
        seq: 1,
      },
    }),
    spyOn(api, "listIssueAsks").mockResolvedValue([]),
    spyOn(api, "listComments").mockImplementation(refusing([pinnedComment])),
    spyOn(api, "getMessage").mockImplementation(refusing({ message: pinnedMessage, replies: [] })),
    spyOn(api, "getIssueEvents").mockImplementation(refusing([...events])),
  ];
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  client.setQueryData(whoAmIQuery().queryKey, { event_head: "0", kind: "user", login: "alice" });
  const feed = correctionFeed(client);
  function Page(): ReactNode {
    useEventFeed();
    const items = useMarginItems(
      MARGIN_OWNER,
      "pinned",
      undefined,
      NO_PLACEMENTS,
      NO_PLACEMENTS,
      undefined
    );
    return (
      <PinnedTab
        conversations={items.pinnedConversations}
        events={items.pinned}
        onUnpin={() => {}}
        onUnpinConversation={() => {}}
        pinnedIds={items.pinnedIds}
      />
    );
  }
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <Page />
      </QueryClientProvider>
    </MemoryRouter>
  );
  return {
    /** The text of each card, in the tab's order. */
    cards: () => [...view.container.querySelectorAll("article")].map((card) => card.textContent),
    deliver: feed.deliver,
    fail: () => {
      feed.start();
      failing = true;
    },
    restore: () => {
      view.unmount();
      feed.restore();
      client.clear();
      for (const stub of stubs) stub.mockRestore();
    },
  };
}

test("a pinned comment's and message's cards whose refetch fails read as edited, then deleted", async () => {
  const tab = openPinnedTab([`comment:${PINNED_COMMENT}`, `message:${PINNED_MESSAGE}`], []);
  try {
    await waitFor(() =>
      expect(tab.cards()).toEqual([
        expect.stringContaining("Hold the deploy."),
        expect.stringContaining("Ship on Friday."),
      ])
    );
    tab.fail();

    await tab.deliver(correction("comment", PINNED_COMMENT, true), [["comments"]]);
    await tab.deliver(correction("message", PINNED_MESSAGE, true), [["issue-messages"]]);
    expect(tab.cards()).toEqual([
      expect.stringMatching(/^Hold the deploy\.alice · .+ · edited$/),
      expect.stringMatching(/^Ship on Friday\.alice · .+ · edited$/),
    ]);

    await tab.deliver(correction("comment", PINNED_COMMENT, false), [["comments"]]);
    await tab.deliver(correction("message", PINNED_MESSAGE, false), [["issue-messages"]]);
    await waitFor(() =>
      expect(tab.cards()).toEqual([
        expect.stringMatching(/^Deleted by alice.*alice · [^·]+$/),
        expect.stringMatching(/^Deleted by alice.*alice · [^·]+$/),
      ])
    );
  } finally {
    tab.restore();
  }
});

test("a pinned comment's and message's events whose refetch fails read as edited, then deleted", async () => {
  const commentCreated: Extract<DispatchEvent, { type: "comment.created" }> = {
    actor: ALICE,
    created_at: "2026-09-10T00:00:00Z",
    id: 11,
    issue_key: "CORE-1",
    notify: false,
    payload: {
      anchor: null,
      answers: null,
      artifact_id: null,
      artifact_name: "",
      ask_id: null,
      author: ALICE,
      body: "Hold the deploy.",
      created_at: "2026-09-10T00:00:00Z",
      deleted_at: null,
      deleted_by: null,
      deliveries: [],
      edited_at: null,
      id: PINNED_COMMENT,
      issue_key: "CORE-1",
      mentions: [],
      reply_block: null,
      reply_to: null,
      resolved: false,
      resolved_at: null,
      resolved_by: null,
      suggestion: null,
      turn: null,
    },
    seq: 11,
    type: "comment.created",
  };
  const messageCreated: Extract<DispatchEvent, { type: "message.created" }> = {
    actor: ALICE,
    created_at: "2026-09-10T00:01:00Z",
    id: 12,
    issue_key: "CORE-1",
    notify: false,
    payload: {
      author: ALICE,
      body: "Ship on Friday.",
      broadcast_id: null,
      created_at: "2026-09-10T00:01:00Z",
      deliveries: [],
      id: PINNED_MESSAGE,
      in_reply_to: null,
      issue_key: "CORE-1",
      reply_block: null,
      target: null,
    },
    seq: 12,
    type: "message.created",
  };
  const tab = openPinnedTab(["event:11", "event:12"], [commentCreated, messageCreated]);
  try {
    await waitFor(() =>
      expect(tab.cards()).toEqual(["Hold the deploy.alice", "Ship on Friday.alice"])
    );
    tab.fail();

    await tab.deliver(correction("comment", PINNED_COMMENT, true), [["events"]]);
    await tab.deliver(correction("message", PINNED_MESSAGE, true), [["events"]]);
    expect(tab.cards()).toEqual([
      "Hold the deploy.alice · edited",
      "Ship on Friday.alice · edited",
    ]);

    await tab.deliver(correction("comment", PINNED_COMMENT, false), [["events"]]);
    await tab.deliver(correction("message", PINNED_MESSAGE, false), [["events"]]);
    await waitFor(() =>
      expect(tab.cards()).toEqual([
        expect.stringMatching(/^Deleted by alice.*alice$/),
        expect.stringMatching(/^Deleted by alice.*alice$/),
      ])
    );
  } finally {
    tab.restore();
  }
});
