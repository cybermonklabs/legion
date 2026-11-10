import { afterEach, expect, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../api/client";
import type { InboxRow, IssueDetails } from "../api/types";
import { AuthGate } from "../app";
import type { MarginProps } from "../features/margin/Margin";
import { ReservedMarginColumn } from "../features/margin/MarginColumn";
import { DEFAULT_MARGIN_WIDTH } from "../features/margin/margin-width";

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
      ref_key: "CORE-1/spec",
      session_id: null,
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
  decisions_artifact_id: "",
  plan_artifact_id: "",
  primary_artifact_id: "artifact-1",
  verification_artifact_id: "",
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
  getAsk: api.getAsk,
  getInbox: api.getInbox,
  getIssue: api.getIssue,
  getIssueSubscribers: api.getIssueSubscribers,
  getMyAgentState: api.getMyAgentState,
  getMyState: api.getMyState,
  listComments: api.listComments,
  listIssueAsks: api.listIssueAsks,
  listIssues: api.listIssues,
  listMyReplies: api.listMyReplies,
  listProjectArchitectureSources: api.listProjectArchitectureSources,
  listProjects: api.listProjects,
  listRepoProjects: api.listRepoProjects,
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
  route = "/issues/CORE-1/children"
): void {
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
      labels: [],
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
    {
      created_at: "2026-09-10T00:00:00Z",
      key: "CORE",
      name: "Core",
      open_asks: 0,
      gated_steps: [],
    },
  ];
  api.listIssueAsks = async () => [];
  api.listComments = async () => [];
  // The issue page under the shell reads these of its own; no test here asserts them.
  api.getIssueSubscribers = async () => [];
  api.listRepoProjects = async () => [];
  api.listProjectArchitectureSources = async () => [];
  // Each ask card refreshes its own thread; the row the Inbox listed carries the one Dispatch
  // would answer with.
  api.getAsk = async (id) => {
    const row = inbox.find((ask) => ask.id === id);
    if (row === undefined) {
      throw new Error(`the Inbox lists no ask ${id}`);
    }
    const { thread, ...ask } = row;
    return { ...thread, ask };
  };

  render(
    <QueryClientProvider
      client={
        new QueryClient({
          defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
        })
      }
    >
      <MemoryRouter initialEntries={[route]}>
        <AuthGate />
      </MemoryRouter>
    </QueryClientProvider>
  );
}

afterEach(() => {
  cleanup();
  Object.assign(api, originalApi);
  window.matchMedia = originalMatchMedia;
});

test("tablet uses the drawer and bottom-sheet controls instead of hidden desktop landmarks", async () => {
  renderShellAt(800);

  // The margin's own code is fetched after the first paint
  // (`features/margin/MarginColumn.tsx`), so both its controls arrive a module load after the
  // shell's own first commit - longer than `waitFor`'s default second under a loaded machine.
  await waitFor(
    () => {
      expect(screen.queryByRole("button", { name: "Open navigation" })).not.toBeNull();
      expect(screen.queryByRole("button", { name: /Open review panel/ })).not.toBeNull();
    },
    { timeout: 5_000 }
  );
  expect(screen.queryByRole("navigation", { name: "Issues" })).not.toBeInTheDocument();
  expect(screen.queryByRole("complementary", { name: "Review margin" })).not.toBeInTheDocument();
});

test("desktop exposes the persistent sidebar navigation and review margin", async () => {
  renderShellAt(1280);

  await waitFor(
    () => {
      expect(screen.queryByRole("navigation", { name: "Navigation" })).not.toBeNull();
      expect(screen.queryByRole("complementary", { name: "Review margin" })).not.toBeNull();
    },
    { timeout: 5_000 }
  );
  expect(screen.queryByRole("button", { name: "Open navigation" })).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: /Open review panel/ })).not.toBeInTheDocument();
});

/** Renders what the shell holds the margin's column open with while the margin's own chunk is
 *  on its way (`features/margin/MarginColumn.tsx`).
 *
 *  Rendered directly rather than through the shell, because React's `lazy` resolves once per
 *  process: a shell render shows that fallback only when it is the first in its file to reach
 *  the margin, and two rows that pass or fail on where they sit in a file are worth nothing.
 *  What is checked here is the reservation's own rule - the column's real width wherever the
 *  margin will draw one, and nothing anywhere else - which is the whole of what the shift was.
 */
function renderReservation(route: string, props: MarginProps = {}): void {
  render(
    <MemoryRouter initialEntries={[route]}>
      <ReservedMarginColumn {...props} />
    </MemoryRouter>
  );
}

// The margin is 384 px of a 1,280 px viewport, and its code is fetched after the first paint:
// a column that was not held open left the document laid out 912 px wide and re-wrapped every
// line of it under the reader a third of a second later, when the margin landed beside it.
test("the margin's column is reserved at the width the margin itself takes", () => {
  setViewport(1280);
  renderReservation("/issues/CORE-1/children");

  const reservation = screen.getByTestId("margin-reservation");
  expect(reservation.style.width).toBe(`${DEFAULT_MARGIN_WIDTH}px`);
  // A reserved column is a page still settling, so the review and the accessibility layers
  // wait it out and read the real margin rather than an empty box (`e2e/review-routes.ts`).
  expect(reservation.hasAttribute("data-margin-pending")).toBe(true);
});

// The reader's own width, not the default: a margin dragged to 440 px reserves 440 px, or the
// document would be laid out at one width and then settle at another all the same.
test("a reader who widened their margin has that width reserved", () => {
  setViewport(1280);
  renderReservation("/projects/CORE/documents/design-language", { width: 440 });

  expect(screen.getByTestId("margin-reservation").style.width).toBe("440px");
});

// And nothing is reserved where the margin will draw no column, or the shift would simply run
// backwards: 384 px taken from the document at first paint and handed back once the margin
// arrives with nothing to put there. The three places are the margin's own
// (`features/margin/Margin.tsx`): a desktop route that has none, a margin collapsed to its
// rail, where the column is hidden and the shell pads the gutter instead, and a compact
// viewport, where the margin is a sheet over the document rather than a column beside it.
test("nothing is reserved where the margin will draw no column", () => {
  setViewport(1280);
  renderReservation("/");
  expect(screen.queryByTestId("margin-reservation")).toBeNull();

  cleanup();
  renderReservation("/issues/CORE-1", { collapsed: true });
  expect(screen.queryByTestId("margin-reservation")).toBeNull();

  cleanup();
  setViewport(800);
  renderReservation("/issues/CORE-1");
  expect(screen.queryByTestId("margin-reservation")).toBeNull();
});

test("compact top bar shows only asks waiting on the viewer", async () => {
  renderShellAt(800, [
    {
      anchor: null,
      answer: null,
      artifact_id: null,
      author: { id: "session-1", kind: "session" },
      block_id: null,
      created_at: "2026-09-10T00:00:00Z",
      edited_at: null,
      id: "ask-waiting",
      // The badge counts the reader's own: the shell signs in as `alice`.
      issue: { assignee: "alice", key: "CORE-1", title: "Responsive shell" },
      issue_key: "CORE-1",
      last_reply: null,
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
      artifact_id: null,
      author: { id: "session-2", kind: "session" },
      block_id: null,
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
  expect(inbox).toHaveAttribute("href", "/");
  const agents = screen.getByRole("link", { name: "New reply 1" });
  expect(inbox.nextElementSibling).toBe(agents);
});

test("the compact header's Inbox link is plain while nothing waits and nothing is unread", async () => {
  renderShellAt(800, [], 0);

  await waitFor(() =>
    expect(screen.queryByRole("button", { name: "Open navigation" })).not.toBeNull()
  );
  expect(screen.getByRole("link", { name: "Inbox" })).toHaveAttribute("href", "/");
  expect(screen.queryByRole("link", { name: /^Replies/ })).not.toBeInTheDocument();
});
