import { afterEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { createElement } from "react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../api/client";
import { whoAmIQuery } from "../api/queries";
import type {
  CredentialPendingRow,
  IssueSummary,
  MyReplyRow,
  OpenAskRow,
  Priorities,
  Project,
} from "../api/types";
import { oneReadReply } from "../features/inbox/inbox-fixture";
import { Sidebar } from "../features/sidebar/Sidebar";
import { replyRow } from "../features/threads/replies-fixture";

/** The summary call the rail reads (`?summary=true`): the two fault counts, and `empty` where
 *  the deployment has written no priority at all. Neither answer carries a priority row. */
function summary(parked: number, unplaced: number, written = true): Priorities {
  return {
    ...(written
      ? {}
      : {
          empty:
            "No priority is live now, so the list is empty. Write one, then say which work advances it.",
        }),
    priorities: [],
    priority_work_parked: { by_priority: {}, count: parked, issues: [] },
    work_under_no_priority: { by_project: {}, count: unplaced, issues: [] },
  };
}

/** The spies a test of this file makes through `renderSidebar` that its own `finally` does not
 *  put back: a spy on `api` outlives its test, so the rail's own read is let go here. */
const railSpies: { mockRestore: () => void }[] = [];

afterEach(() => {
  for (const spy of railSpies.splice(0)) spy.mockRestore();
});

function issue(overrides: Partial<IssueSummary> = {}): IssueSummary {
  return {
    advances: [],
    advances_revision: 0,
    route: null,
    repo: null,
    resolved_repo: null,
    route_status: null,
    route_holder: null,
    route_status_reason: null,
    key: "CORE-1",
    labels: [],
    last_seq: 0,
    open_asks: 1,
    progress: { tasks: null, children: null },
    parent: null,
    blocked_by: [],
    primary_artifact_id: "artifact-1",
    assignee: null,
    claim: null,
    components: { mode: "inherit", ids: [], unknown: [], reason: null, inherited_from: null },
    status: "todo",
    priority: null,
    rank: "U",
    title: "Pinned work",
    updated_at: "2026-09-10T00:00:00Z",
    closed_at: null,
    created_by: null,
    created_at: "2026-09-10T00:00:00Z",
    ...overrides,
  };
}

function project(overrides: Partial<Project> = {}): Project {
  return {
    created_at: "2026-09-10T00:00:00Z",
    key: "CORE",
    name: "Core",
    open_asks: 3,
    gated_steps: [],
    proven_sessions: false,
    ...overrides,
  };
}

/** `pending` is the broker's list of credential requests waiting on the viewer; without it this
 *  Dispatch has no broker (the list answers `null`), and `"unreadable"` is a list whose first
 *  read failed. `priorities` is what the rail's summary call answers; by default this
 *  deployment has written none. `threads` are the reader's Reply rows. The reader's asks and
 *  threads reach the Inbox count through the one read, which answers every ask below - one of
 *  them waiting on an agent - so the count is the rail's own narrowing to the asks that wait on
 *  the reader. `unreadTotal` is the unread count's answer for a test that holds that read out
 *  itself: the one read's other reads answer at once. */
function renderSidebar(
  pathname = "/",
  {
    pending,
    priorities = summary(0, 0, false),
    threads = [],
    unreadTotal,
  }: {
    pending?: CredentialPendingRow[] | "unreadable";
    priorities?: Priorities;
    threads?: readonly MyReplyRow[];
    unreadTotal?: Promise<number>;
  } = {}
) {
  const listPriorities = spyOn(api, "listPriorities").mockResolvedValue(priorities);
  railSpies.push(listPriorities);
  const asks: OpenAskRow[] = [
    {
      anchor: null,
      answer: null,
      artifact_id: null,
      asked_of: null,
      asked_of_at: null,
      asked_of_by: null,
      author: { id: "alice", kind: "user" },
      block_id: null,
      created_at: "2026-09-10T00:00:00Z",
      edited_at: null,
      id: "ask-1",
      // Needs you counts the questions waiting on the reader, which is what the server answers
      // in `waiting_on_person`: nobody has handed these on, so it is their issue's assignee.
      issue: { assignee: "alice", key: "CORE-1", title: "Pinned work" },
      issue_key: "CORE-1",
      last_reply: null,
      waiting_on_person: "alice",
      priority: null,
      snoozed_until: null,
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
      question: "Ship?",
      state: "open",
      waiting_on: "human",
      urgency: "med",
    },
    {
      anchor: null,
      answer: null,
      artifact_id: null,
      asked_of: null,
      asked_of_at: null,
      asked_of_by: null,
      author: { id: "alice", kind: "user" },
      block_id: null,
      created_at: "2026-09-10T00:00:00Z",
      edited_at: null,
      id: "ask-2",
      issue_key: "OPS-1",
      priority: null,
      snoozed_until: null,
      kind: "question",
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
      question: "Review?",
      state: "open",
      waiting_on: "agent",
      last_reply: { author: { id: "alice", kind: "user" }, created_at: "2026-09-10T01:00:00Z" },
      urgency: "med",
    },
    {
      anchor: null,
      answer: null,
      artifact_id: null,
      asked_of: null,
      asked_of_at: null,
      asked_of_by: null,
      author: { id: "agent-2", kind: "session" },
      block_id: null,
      created_at: "2026-09-10T00:00:00Z",
      edited_at: null,
      id: "ask-3",
      issue: { assignee: "alice", key: "OPS-2", title: "Operations work" },
      issue_key: "OPS-2",
      waiting_on_person: "alice",
      priority: null,
      snoozed_until: null,
      kind: "question",
      multiple: false,
      opened_event_id: 3,
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
      question: "Confirm?",
      state: "open",
      waiting_on: "human",
      last_reply: {
        author: { id: "agent-3", kind: "session" },
        created_at: "2026-09-10T01:00:00Z",
      },
      urgency: "med",
    },
  ];
  // The rail's one Inbox number adds the threads the reader has not read to the work waiting on
  // them, and draws no number at all until every read it adds up has answered, so the one read
  // is answered here for every test.
  railSpies.push(
    spyOn(api, "getMeInbox").mockImplementation(async (query = {}) => {
      const answer = oneReadReply(query, { asks, threads });
      return query.state === "unread" && unreadTotal !== undefined
        ? { ...answer, total: await unreadTotal }
        : answer;
    })
  );
  const getCredentialPending = spyOn(api, "getCredentialPending");
  if (pending === "unreadable") {
    getCredentialPending.mockRejectedValue(new Error("the broker is unreachable"));
  } else {
    getCredentialPending.mockResolvedValue(pending === undefined ? null : { pending });
  }
  const getIssue = spyOn(api, "getIssue").mockResolvedValue(undefined as never);
  const listIssues = spyOn(api, "listIssues").mockResolvedValue([issue()]);
  const listProjects = spyOn(api, "listProjects").mockResolvedValue([
    project(),
    project({ key: "OPS", name: "Operations", open_asks: 0 }),
  ]);
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  // Needs you counts the reader's own questions, so the badge waits on `/auth/whoami`; the
  // sidebar is handed the same person below.
  queryClient.setQueryData(whoAmIQuery().queryKey, {
    event_head: "0",
    kind: "user",
    login: "alice",
  });
  const view = render(
    createElement(
      MemoryRouter,
      { initialEntries: [pathname] },
      createElement(
        QueryClientProvider,
        { client: queryClient },
        createElement(Sidebar, { user: { event_head: "0", kind: "user", login: "alice" } })
      )
    )
  );
  return {
    getCredentialPending,
    getIssue,
    listIssues,
    listPriorities,
    listProjects,
    view,
  };
}

test("sidebar lists Inbox, Pinned, Projects, and Settings with a truthful Inbox count", async () => {
  const sidebar = renderSidebar();

  try {
    await screen.findByRole("link", { name: /CORE.*Core/ });
    expect(screen.getByRole("link", { name: "Inbox 2" })).toBeTruthy();
    expect(screen.getByRole("heading", { name: "Pinned" })).toBeTruthy();
    expect(screen.getByRole("link", { name: /CORE-1.*Pinned work/ })).toHaveTextContent("1");
    expect(screen.getByRole("heading", { name: "Projects" })).toBeTruthy();
    expect(screen.getByRole("link", { name: /CORE.*Core/ })).toHaveTextContent("3");
    expect(screen.getByRole("link", { name: /OPS.*Operations/ })).toBeTruthy();
    expect(screen.getByRole("link", { name: "Settings" })).toBeTruthy();
  } finally {
    sidebar.view.unmount();
    sidebar.getCredentialPending.mockRestore();
    sidebar.getIssue.mockRestore();
    sidebar.listIssues.mockRestore();
    sidebar.listProjects.mockRestore();
  }
});

test("sidebar counts a pending credential request in the Inbox count, beside the asks waiting on you", async () => {
  const sidebar = renderSidebar("/", {
    pending: [
      {
        identifiers: ["DEMO_API_KEY"],
        kind: "agent_secret",
        record_id: "record-1",
        requested_at: "2026-09-10T00:00:00Z",
        session: { enrollment: null, request: null },
      },
    ],
  });

  try {
    expect(await screen.findByRole("link", { name: "Inbox 3" })).toBeTruthy();
  } finally {
    sidebar.view.unmount();
    sidebar.getCredentialPending.mockRestore();
    sidebar.getIssue.mockRestore();
    sidebar.listIssues.mockRestore();
    sidebar.listProjects.mockRestore();
  }
});

// INFRA-3921: an expired credential request stays listed until somebody dismisses it, but nobody
// can decide it any more, so the rail's count leaves it out: only the pending request is counted
// beside the asks.
test("sidebar leaves an expired credential request out of the Inbox count", async () => {
  const sidebar = renderSidebar("/", {
    pending: [
      {
        identifiers: ["DEMO_API_KEY"],
        kind: "agent_secret",
        record_id: "record-1",
        requested_at: "2026-09-10T00:00:00Z",
        session: { enrollment: null, request: null },
        state: "pending",
      },
      {
        expires_at: "2026-09-10T01:00:00Z",
        identifiers: ["DEEL_API_KEY"],
        kind: "agent_secret",
        record_id: "record-2",
        requested_at: "2026-09-09T00:00:00Z",
        session: { enrollment: null, request: null },
        state: "expired",
      },
    ],
  });

  try {
    expect(await screen.findByRole("link", { name: "Inbox 3" })).toBeTruthy();
    expect(screen.queryByRole("link", { name: "Inbox 4" })).not.toBeInTheDocument();
  } finally {
    sidebar.view.unmount();
    sidebar.getCredentialPending.mockRestore();
    sidebar.getIssue.mockRestore();
    sidebar.listIssues.mockRestore();
    sidebar.listProjects.mockRestore();
  }
});

test("sidebar marks the current project", async () => {
  const sidebar = renderSidebar("/projects/CORE/documents");

  try {
    const core = await screen.findByRole("link", { name: /CORE.*Core/ });
    expect(core).toHaveAttribute("aria-current", "page");
  } finally {
    sidebar.view.unmount();
    sidebar.getCredentialPending.mockRestore();
    sidebar.getIssue.mockRestore();
    sidebar.listIssues.mockRestore();
    sidebar.listProjects.mockRestore();
  }
});

test("sidebar never requests the full issue list", async () => {
  const sidebar = renderSidebar();

  try {
    await screen.findByRole("link", { name: /CORE.*Core/ });
    expect(sidebar.listIssues).toHaveBeenCalledTimes(1);
    expect(sidebar.listIssues).toHaveBeenCalledWith({ pinned: true });
    expect(sidebar.getIssue).not.toHaveBeenCalled();
  } finally {
    sidebar.view.unmount();
    sidebar.getCredentialPending.mockRestore();
    sidebar.getIssue.mockRestore();
    sidebar.listIssues.mockRestore();
    sidebar.listProjects.mockRestore();
  }
});

// The Inbox lists the threads beside the asks now, so the rail has one row and one number for
// both: the work waiting on the reader plus the threads they have not read.
for (const [unread, name] of [
  [2, "Inbox4"],
  [0, "Inbox2"],
] as const) {
  test(`sidebar adds ${unread} unread threads to the Inbox count`, async () => {
    const whoAmI = spyOn(api, "whoAmI").mockResolvedValue({
      event_head: "0",
      kind: "user",
      login: "alice",
    });
    const sidebar = renderSidebar("/", {
      threads: Array.from({ length: unread }, (_, index) => replyRow(`c${index}`)),
    });

    try {
      await waitFor(() =>
        expect(api.getMeInbox).toHaveBeenCalledWith({ limit: 1, state: "unread" })
      );
      await waitFor(() =>
        expect(screen.getByRole("link", { name: /^Inbox/ }).textContent).toBe(name)
      );
      expect(screen.queryByRole("link", { name: /^Replies/ })).not.toBeInTheDocument();
    } finally {
      sidebar.view.unmount();
      sidebar.getCredentialPending.mockRestore();
      sidebar.getIssue.mockRestore();
      sidebar.listIssues.mockRestore();
      sidebar.listProjects.mockRestore();
      whoAmI.mockRestore();
    }
  });
}

// F2 item 3: the rail's number summed three reads and counted each as empty while it was out or
// after it failed, so it showed a partial sum as the reader's. It is drawn only once every read
// it adds up has answered.
test("the rail draws no Inbox number while a read it adds up is out, and the sum once it lands", async () => {
  const whoAmI = spyOn(api, "whoAmI").mockResolvedValue({
    event_head: "0",
    kind: "user",
    login: "alice",
  });
  const counted = Promise.withResolvers<number>();
  const sidebar = renderSidebar("/", { unreadTotal: counted.promise });

  try {
    // Two asks wait on alice, so the old sum would have read `Inbox 2` here.
    const inbox = await screen.findByRole("link", { name: "Inbox" });
    expect(inbox.textContent).toBe("Inbox");
    counted.resolve(3);
    await waitFor(() =>
      expect(screen.getByRole("link", { name: /^Inbox/ }).textContent).toBe("Inbox5")
    );
  } finally {
    sidebar.view.unmount();
    sidebar.getCredentialPending.mockRestore();
    sidebar.getIssue.mockRestore();
    sidebar.listIssues.mockRestore();
    sidebar.listProjects.mockRestore();
    whoAmI.mockRestore();
  }
});

test("the rail draws no Inbox number after the credential list's first read failed", async () => {
  const whoAmI = spyOn(api, "whoAmI").mockResolvedValue({
    event_head: "0",
    kind: "user",
    login: "alice",
  });
  const sidebar = renderSidebar("/", { pending: "unreadable" });

  try {
    const inbox = await screen.findByRole("link", { name: "Inbox" });
    expect(inbox.textContent).toBe("Inbox");
  } finally {
    sidebar.view.unmount();
    sidebar.getCredentialPending.mockRestore();
    sidebar.getIssue.mockRestore();
    sidebar.listIssues.mockRestore();
    sidebar.listProjects.mockRestore();
    whoAmI.mockRestore();
  }
});

test("sidebar keeps its hide control available while navigation loads", async () => {
  const user = userEvent.setup();
  const getMeInbox = spyOn(api, "getMeInbox").mockImplementation(
    () => new Promise<never>(() => {})
  );
  const getCredentialPending = spyOn(api, "getCredentialPending").mockImplementation(
    () => new Promise<never>(() => {})
  );
  const listIssues = spyOn(api, "listIssues").mockResolvedValue([]);
  const listProjects = spyOn(api, "listProjects").mockResolvedValue([]);
  let hideCount = 0;
  const view = render(
    createElement(
      MemoryRouter,
      { initialEntries: ["/"] },
      createElement(
        QueryClientProvider,
        { client: new QueryClient({ defaultOptions: { queries: { retry: false } } }) },
        createElement(Sidebar, {
          onHide: () => {
            hideCount += 1;
          },
          user: { event_head: "0", kind: "user", login: "alice" },
        })
      )
    )
  );

  try {
    expect(screen.getByText("Loading navigation…")).toBeTruthy();
    await user.click(screen.getByRole("button", { name: "Hide sidebar" }));
    expect(hideCount).toBe(1);
  } finally {
    view.unmount();
    getMeInbox.mockRestore();
    getCredentialPending.mockRestore();
    listIssues.mockRestore();
    listProjects.mockRestore();
  }
});

// The rail's Priorities entry (INFRA-2947, R8): the way to the page from every other page, with
// the number of faults now beside it, which is the two fault counts added.
test("the rail's Priorities entry opens the page and counts both faults as one number", async () => {
  const sidebar = renderSidebar("/", { priorities: summary(5, 7) });

  try {
    const entry = await screen.findByRole("link", { name: /^Priorities/ });
    expect(entry).toHaveAttribute("href", "/priorities");
    await waitFor(() => expect(entry).toHaveTextContent("12"));
  } finally {
    sidebar.view.unmount();
    sidebar.getCredentialPending.mockRestore();
    sidebar.getIssue.mockRestore();
    sidebar.listIssues.mockRestore();
    sidebar.listProjects.mockRestore();
  }
});

// Where no priority has been written there is nothing for work to be off, so the entry carries
// no number rather than a zero the reader would read as a state of the board.
test("the rail's Priorities entry carries no number where no priority exists", async () => {
  const sidebar = renderSidebar();

  try {
    const entry = await screen.findByRole("link", { name: /^Priorities/ });
    await waitFor(() => expect(sidebar.listPriorities).toHaveBeenCalledWith({ summary: true }));
    expect(entry.textContent).toBe("PrioritiesPriorities and the work under them");
  } finally {
    sidebar.view.unmount();
    sidebar.getCredentialPending.mockRestore();
    sidebar.getIssue.mockRestore();
    sidebar.listIssues.mockRestore();
    sidebar.listProjects.mockRestore();
  }
});
