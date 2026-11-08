import { afterEach, expect, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { ApiError, api } from "../api/client";
import type { Agent, InboxRow, IssueDetails, UserAgentState } from "../api/types";
import { AuthGate } from "../app";
import { liveViewsOf } from "../features/agent-view/live-views";
import { answerAgentStates } from "./send-answers";

const issue: IssueDetails = {
  route_status: null,
  route_holder: null,
  route_status_reason: null,
  holder_state: null,
  last_seen: null,
  takeable_at: null,
  artifacts: [
    {
      created_at: "2026-09-10T00:00:00Z",
      created_by: { id: "alice", kind: "user" },
      id: "artifact-1",
      issue_key: "CORE-1",
      project: "CORE",
      kind: "doc",
      name: "Spec",
      primary: true,
      slug: "spec",
      versions: [],
    },
  ],
  children: [],
  closed_at: null,
  created_at: "2026-09-10T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  external_links: [],
  key: "CORE-1",
  labels: [],
  last_seq: 0,
  number: 1,
  open_asks: [],
  progress: { tasks: null, children: null },
  parent: null,
  blocked_by: [],
  assignee: null,
  claim: null,
  components: { mode: "inherit", ids: [], unknown: [], reason: null, inherited_from: null },
  primary_artifact_id: "artifact-1",
  project: "CORE",
  referenced_by_count: 0,
  route: null,
  repo: null,
  resolved_repo: null,
  status: "todo",
  priority: null,
  rank: "U",
  title: "Responsive shell",
  updated_at: "2026-09-10T00:00:00Z",
};

const originalApi = {
  getInbox: api.getInbox,
  getIssue: api.getIssue,
  getMyAgentState: api.getMyAgentState,
  getMyState: api.getMyState,
  listAgents: api.listAgents,
  listComments: api.listComments,
  listIssueAsks: api.listIssueAsks,
  listIssues: api.listIssues,
  listMyReplies: api.listMyReplies,
  listProjects: api.listProjects,
  whoAmI: api.whoAmI,
};
const originalMatchMedia = window.matchMedia;

function setViewport(width: number): void {
  window.matchMedia = ((query: string) =>
    ({
      addEventListener: () => {},
      addListener: () => {},
      dispatchEvent: () => true,
      matches: query === "(max-width: 1279px)" && width <= 1279,
      media: query,
      onchange: null,
      removeEventListener: () => {},
      removeListener: () => {},
    }) as MediaQueryList) as typeof window.matchMedia;
}

function renderShellAt(
  width: number,
  inbox: InboxRow[] = [],
  unreadThreads = 0,
  listedAgents: Agent[] = []
): QueryClient {
  setViewport(width);
  api.whoAmI = async () => ({ event_head: "0", kind: "user", login: "alice" });
  api.listMyReplies = async (query) => ({
    as_of: "2026-10-12T00:00:00Z",
    limit: query.limit ?? 0,
    offset: 0,
    rows: [],
    total: unreadThreads,
  });
  api.listIssues = async () => [
    {
      route: null,
      repo: null,
      resolved_repo: null,
      route_status: null,
      route_holder: null,
      route_status_reason: null,
      key: issue.key,
      open_asks: 0,
      progress: { tasks: null, children: null },
      last_seq: 0,
      parent: issue.parent,
      blocked_by: issue.blocked_by,
      primary_artifact_id: issue.primary_artifact_id,
      assignee: issue.assignee,
      claim: null,
      components: issue.components,
      status: issue.status,
      priority: issue.priority,
      rank: issue.rank,
      title: issue.title,
      updated_at: issue.updated_at,
    },
  ];
  api.getMyState = async () => ({
    "CORE-1": { dismissed: [], last_read_seq: 0, pinned: false, seq: 0 },
  });
  api.getIssue = async () => issue;
  api.getInbox = async () => inbox;
  api.listProjects = async () => [
    { created_at: "2026-09-10T00:00:00Z", key: "CORE", name: "Core", open_asks: 0 },
  ];
  api.listIssueAsks = async () => [];
  api.listComments = async () => [];
  api.listAgents = async () => listedAgents;

  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter initialEntries={["/issues/CORE-1/children"]}>
        <AuthGate />
      </MemoryRouter>
    </QueryClientProvider>
  );
  return queryClient;
}

afterEach(() => {
  cleanup();
  Object.assign(api, originalApi);
  window.matchMedia = originalMatchMedia;
});

test("tablet uses the drawer and bottom-sheet controls instead of hidden desktop landmarks", async () => {
  renderShellAt(800);

  await waitFor(() =>
    expect(screen.queryByRole("button", { name: "Open navigation" })).not.toBeNull()
  );
  // The margin's own code is fetched after the first paint (`features/margin/MarginColumn.tsx`),
  // so its control arrives on a later frame than the shell's.
  await waitFor(() =>
    expect(screen.queryByRole("button", { name: /Open review panel/ })).not.toBeNull()
  );
  expect(screen.queryByRole("navigation", { name: "Issues" })).toBeNull();
  expect(screen.queryByRole("complementary", { name: "Review margin" })).toBeNull();
});

test("desktop exposes the persistent sidebar navigation and review margin", async () => {
  renderShellAt(1280);

  await waitFor(() =>
    expect(screen.queryByRole("navigation", { name: "Navigation" })).not.toBeNull()
  );
  // As above: the margin lands once its chunk has.
  await waitFor(() =>
    expect(screen.queryByRole("complementary", { name: "Review margin" })).not.toBeNull()
  );
  expect(screen.queryByRole("button", { name: "Open navigation" })).toBeNull();
  expect(screen.queryByRole("button", { name: /Open review panel/ })).toBeNull();
});
test("compact top bar shows only asks waiting on the viewer", async () => {
  renderShellAt(800, [
    {
      anchor: null,
      answer: null,
      author: { id: "session-1", kind: "session" },
      created_at: "2026-09-10T00:00:00Z",
      edited_at: null,
      id: "ask-waiting",
      // The badge counts the reader's own: the shell signs in as `alice`.
      issue: { assignee: "alice", key: "CORE-1", title: "Responsive shell" },
      issue_key: "CORE-1",
      kind: "question",
      multiple: false,
      opened_event_id: 1,
      options: [],
      thread: {
        answers: [],
        claimant: null,
        edits: [],
        followers: [],
        replies: [],
        route: null,
        route_status: null,
        route_holder: null,
        route_status_reason: null,
      },
      priority: null,
      snoozed_until: null,
      question: "Choose a direction",
      state: "open",
      waiting_on: "human",
      urgency: "med",
    },
    {
      anchor: null,
      answer: null,
      author: { id: "session-2", kind: "session" },
      created_at: "2026-09-10T00:00:00Z",
      edited_at: null,
      id: "ask-answered",
      issue_key: "CORE-1",
      kind: "question",
      last_reply: { author: { id: "alice", kind: "user" }, created_at: "2026-09-10T01:00:00Z" },
      multiple: false,
      opened_event_id: 2,
      options: [],
      thread: {
        answers: [],
        claimant: null,
        edits: [],
        followers: [],
        replies: [],
        route: null,
        route_status: null,
        route_holder: null,
        route_status_reason: null,
      },
      priority: null,
      snoozed_until: null,
      question: "Clarify this",
      state: "open",
      waiting_on: "agent",
      urgency: "med",
    },
  ]);

  // One number now: the asks waiting on the reader and the threads they have not read.
  expect(await screen.findByText("Inbox 1")).not.toBeNull();
});

test("the compact header's one Inbox link counts the unread threads too and opens the Inbox", async () => {
  api.getMyAgentState = async () => ({ planner: { unread_replies: 1 } });
  renderShellAt(800, [], 2);

  const inbox = await screen.findByRole("link", { name: "Inbox 2" });
  expect(inbox.getAttribute("href")).toBe("/");
  const agents = screen.getByRole("link", { name: "New reply 1" });
  expect(inbox.nextElementSibling).toBe(agents);
});

test("the compact header's Inbox link is plain while nothing waits and nothing is unread", async () => {
  renderShellAt(800, [], 0);

  await waitFor(() =>
    expect(screen.queryByRole("button", { name: "Open navigation" })).not.toBeNull()
  );
  expect(screen.getByRole("link", { name: "Inbox" }).getAttribute("href")).toBe("/");
  expect(screen.queryByRole("link", { name: /^Replies/ })).toBeNull();
});

/** A session Dispatch lists, for the rail's pinned rows. */
function agent(sessionId: string, title: string): Agent {
  return {
    capabilities: ["aside"],
    dir: "/workspaces/legion",
    host: "build-host",
    last_activity: null,
    last_seen: Date.now(),
    machine_id: "build-host",
    open_asks: 0,
    roles: [],
    session_id: sessionId,
    title,
  };
}

const PINS_KEY = "dispatch.agents.pinned:alice";

// The pins were this browser's until INFRA-812 made them the person's. The shell writes each id
// the old key holds as a pin and removes the key, so the person's pins survive the change
// without being pinned again by hand, and each browser migrates only its own list.
//
// One write at a time, in the order the key kept them: Dispatch stamps each pin as it arrives
// and `pinned_at` is the order the rail draws. The key holds every id until the last of them
// has landed, so nothing in between - a reload, a build rolled back - loses a pin that was
// never written.
test("the shell writes the browser's old pins one at a time and forgets the key once they land", async () => {
  const planner = Promise.withResolvers<UserAgentState>();
  const putAgentState = answerAgentStates().mockImplementation(async (session) =>
    session === "planner-session" ? planner.promise : { pinned_at: "2026-09-14T12:00:01Z" }
  );
  window.localStorage.setItem(PINS_KEY, JSON.stringify(["planner-session", "reviewer-session"]));
  api.getMyAgentState = async () => ({});

  try {
    renderShellAt(1280);
    // The first id is out on its own, and the key still holds both: nothing has landed yet.
    await waitFor(() => expect(putAgentState).toHaveBeenCalledTimes(1));
    expect(putAgentState.mock.calls[0]?.[0]).toBe("planner-session");
    expect(window.localStorage.getItem(PINS_KEY)).not.toBeNull();

    await act(async () => {
      planner.resolve({ pinned_at: "2026-09-14T12:00:00Z" });
      await planner.promise;
    });
    await waitFor(() => expect(putAgentState).toHaveBeenCalledTimes(2));
    expect(putAgentState.mock.calls.map(([session, input]) => [session, input])).toEqual([
      ["planner-session", { pinned: true }],
      ["reviewer-session", { pinned: true }],
    ]);
    await waitFor(() => expect(window.localStorage.getItem(PINS_KEY)).toBeNull());

    // The next load of the same browser has nothing left to migrate, so it writes nothing.
    cleanup();
    renderShellAt(1280);
    await waitFor(() =>
      expect(screen.queryByRole("navigation", { name: "Navigation" })).not.toBeNull()
    );
    expect(putAgentState).toHaveBeenCalledTimes(2);
  } finally {
    window.localStorage.removeItem(PINS_KEY);
    putAgentState.mockRestore();
  }
});

// A write Dispatch refuses is the reader's to retry or discard in the pending-writes list, and
// what they discard was never made: the key keeps every id it held, including the ids after the
// refused one, which this load does not write at all. The next load writes the lot again, in
// the same order.
test("a pin write Dispatch refuses leaves the browser's old key as it was", async () => {
  const kept = JSON.stringify(["planner-session", "reviewer-session"]);
  const putAgentState = answerAgentStates().mockImplementation(async (session) => {
    if (session === "planner-session") {
      throw new ApiError(400, { code: "AGENT_STATE_INPUT", error: "Dispatch refused it" });
    }
    return { pinned_at: "2026-09-14T12:00:01Z" };
  });
  window.localStorage.setItem(PINS_KEY, kept);
  api.getMyAgentState = async () => ({});

  try {
    renderShellAt(1280);
    await waitFor(() => expect(putAgentState).toHaveBeenCalled());
    // Long enough for a second write to have gone out, had the refusal not stopped the run.
    await act(async () => {
      await Bun.sleep(50);
    });
    expect(putAgentState.mock.calls.map(([session]) => session)).toEqual(["planner-session"]);
    expect(window.localStorage.getItem(PINS_KEY)).toBe(kept);
  } finally {
    window.localStorage.removeItem(PINS_KEY);
    putAgentState.mockRestore();
  }
});

// Below 1280 pixels the rail is a sheet behind the Menu control, and the person's pinned agents
// are in that sheet (R1), not left behind on the desktop rail.
test("a phone finds the pinned agents in the sidebar sheet", async () => {
  api.getMyAgentState = async () => ({
    "planner-session": { pinned_at: "2026-09-14T00:00:01Z" },
  });
  renderShellAt(800, [], 0, [agent("planner-session", "Planner")]);

  fireEvent.click(await screen.findByRole("button", { name: "Open navigation" }));
  const sheet = await screen.findByRole("dialog", { name: "Navigation" });
  await within(sheet).findByRole("link", { name: "Planner" });
  expect(
    within(sheet).getByRole("button", { name: "Unpin Planner from the sidebar" })
  ).toBeTruthy();
});

// The registry keeps a sleeping pinned view and discards a sleeping unpinned one (`D8`), so the
// shell - not the rail, which is off the page at a phone width or a collapsed sidebar - tells it
// what the person has pinned. Without that, coming back to a pinned agent would lose where they
// were reading.
test("the shell tells the live-view registry which agents the person pinned", async () => {
  api.getMyAgentState = async () => ({
    "planner-session": { pinned_at: "2026-09-14T00:00:01Z" },
  });
  const queryClient = renderShellAt(1280, [], 0, [agent("planner-session", "Planner")]);
  await screen.findByRole("button", { name: "Unpin Planner from the sidebar" });

  const views = liveViewsOf(queryClient);
  act(() => {
    views.show("planner-session");
    views.setPlace("planner-session", { id: "dispatch:m-4", top: -120 });
    views.show("other-session");
    views.setPlace("other-session", { id: "dispatch:m-7", top: -40 });
    // A third view puts the first to sleep at once, where only a pin keeps its entry.
    views.show("third-session");
    views.show(null);
  });

  expect(views.place("planner-session")).toEqual({ id: "dispatch:m-4", top: -120 });
  expect(views.place("other-session")).toBeNull();
});
