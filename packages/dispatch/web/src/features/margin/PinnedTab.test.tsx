import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { type ReactNode, useState } from "react";
import { MemoryRouter } from "react-router-dom";

import { afterRescue, RescuedMain } from "../../__tests__/focus-rescue";
import { api } from "../../api/client";
import type { DispatchEvent } from "../../api/types";
import { PinnedTab } from "./PinnedTab";
import type { PinnedConversationCard } from "./useMarginItems";

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
              id: "ask:ask-1",
              kind: "ask",
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

// A pinned card is an `<article>`, a row of the Pinned tab by its element alone (`row-marker.ts`'s
// `ROW_SELECTOR`). The reader's own Unpin takes its card off the tab in the render its press makes,
// as the margin applies an unpin to the page's pins at once (`writePin`): a loss the reader caused.
// So the app shell's focus rescue, mounted here on the page's main content as the shell mounts it,
// hands the focus to the same control in the card that moved up into the place - the next card's
// Unpin - and the reader goes on unpinning. Counted by place alone, it would land on that card's
// first control, the link its byline draws.
test("the reader's Unpin hands the focus to the Unpin of the card that moves up into its place", async () => {
  const user = userEvent.setup();
  const listAgents = spyOn(api, "listAgents").mockResolvedValue([]);
  const author = {
    id: "session-one",
    kind: "session" as const,
    origin: { session_title: "Implementer" },
  };
  const pinned: PinnedConversationCard[] = ["first", "second", "third"].map((name) => ({
    at: "2026-09-10T00:00:00Z",
    author,
    body: `The ${name} pinned question`,
    id: `ask:${name}`,
    kind: "ask",
  }));
  /** The tab on the page's pins, which Unpin takes a card out of. */
  function Pins(): ReactNode {
    const [cards, setCards] = useState(pinned);
    return (
      <PinnedTab
        conversations={cards}
        events={[]}
        onUnpin={() => {}}
        onUnpinConversation={(id) =>
          setCards((current) => current.filter((card) => card.id !== id))
        }
        pinnedIds={cards.map((card) => card.id)}
      />
    );
  }
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <RescuedMain>
          <Pins />
        </RescuedMain>
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    const [, second, third] = screen.getAllByRole("button", { name: "Unpin" });
    if (second === undefined || third === undefined) throw new Error("three Unpins missing");
    await user.click(second);
    await waitFor(() => expect(second).not.toBeInTheDocument());
    await afterRescue();

    expect(screen.getAllByRole("article")).toHaveLength(2);
    // Named rather than compared as nodes, so a focus elsewhere reports what holds it, not the
    // whole tree a failed comparison of two elements prints.
    const focused = document.activeElement;
    expect([focused?.tagName, focused?.getAttribute("aria-label") ?? focused?.textContent]).toEqual(
      ["BUTTON", "Unpin"]
    );
    expect(focused === third).toBe(true);
  } finally {
    view.unmount();
    listAgents.mockRestore();
  }
});
