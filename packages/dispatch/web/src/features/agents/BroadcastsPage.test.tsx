import { beforeAll, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import type { Agent, BroadcastSummary } from "../../api/types";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { BroadcastsPage } from "./BroadcastsPage";

// The rows draw through the shared renderer, whose Markdown engine loads lazily, and they run in
// random order, so the file loads the engine before its first row, whichever row that is.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

const planner: Agent = {
  capabilities: ["steer"],
  dir: "/workspaces/planner",
  host: "build-host",
  last_activity: null,
  last_seen: Date.now(),
  machine_id: "build-host",
  open_asks: 0,
  roles: [],
  session_id: "planner-session",
  state: "live",
  title: "Planner",
};

// INFRA-440. A row is one link to its broadcast, and a session sender is drawn by `AgentName`,
// whose name links to its live conversation: the sender sits beside the row's link, never inside
// it, so neither link holds the other and the row link keeps its broadcast destination.
test("a broadcast row draws its session sender outside the row's link", async () => {
  const sent: BroadcastSummary = {
    author: { id: "planner-session", kind: "session" },
    body: "Report status.",
    created_at: new Date().toISOString(),
    delivery: "steer",
    id: "broadcast-1",
    recipients: 2,
    replies: 1,
  };
  const listBroadcasts = spyOn(api, "listBroadcasts").mockResolvedValue([sent]);
  const listAgents = spyOn(api, "listAgents").mockResolvedValue([planner]);
  const whoAmI = spyOn(api, "whoAmI").mockResolvedValue({
    event_head: "0",
    kind: "user",
    login: "alice",
  });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider
        client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}
      >
        <BroadcastsPage />
      </QueryClientProvider>
    </MemoryRouter>
  );
  try {
    const row = await screen.findByRole("listitem");
    const rowLink = within(row).getByRole("link", { name: /1 of 2 answered/ });
    expect(rowLink.getAttribute("href")).toBe("/agents/broadcasts/broadcast-1");
    const sender = await within(row).findByRole("link", { name: "Planner" });
    expect(sender.getAttribute("href")).toBe("/agents/planner-session/live");
    expect(rowLink.contains(sender)).toBe(false);
    expect(sender.contains(rowLink)).toBe(false);
    const agent = row.querySelector('[data-agent-name="planner-session"]');
    await waitFor(() => expect(agent?.getAttribute("data-agent-state")).toBe("running"));
  } finally {
    view.unmount();
    listAgents.mockRestore();
    listBroadcasts.mockRestore();
    whoAmI.mockRestore();
  }
});

// The list is read under the `person` scope: an agent viewer whose credential lacks it asks for
// none, and the page says why it has no list rather than drawing one that is still loading.
test("an agent viewer without the person scope is told why the broadcasts page has no list", async () => {
  const listBroadcasts = spyOn(api, "listBroadcasts").mockResolvedValue([]);
  const whoAmI = spyOn(api, "whoAmI").mockResolvedValue({
    event_head: "0",
    kind: "agent",
    login: "agent:planner-session",
    session: "planner-session",
  });
  const whoAmIScopes = spyOn(api, "whoAmIScopes").mockResolvedValue([]);
  const view = render(
    <MemoryRouter>
      <QueryClientProvider
        client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}
      >
        <BroadcastsPage />
      </QueryClientProvider>
    </MemoryRouter>
  );
  try {
    await screen.findByText(/Could not load broadcasts: .*person scope/);
    expect(screen.queryByLabelText("Loading broadcasts")).toBeNull();
    expect(listBroadcasts).not.toHaveBeenCalled();
  } finally {
    view.unmount();
    listBroadcasts.mockRestore();
    whoAmI.mockRestore();
    whoAmIScopes.mockRestore();
  }
});
