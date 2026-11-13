import { expect, jest, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
import {
  act,
  // biome-ignore lint/style/noRestrictedImports: this file keeps fireEvent for the mouse and pointer presses that drive dnd-kit, the key inside one of those drags, the clicks whose boolean return it reads, and the bare focus that has a control read its options
  fireEvent,
  render,
  renderHook,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";
import type { ReactNode } from "react";
import { MemoryRouter, useLocation } from "react-router-dom";

import { realClock, settle, tick } from "../../__tests__/fake-clock";
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { answerIssuePatches, answerIssueStates } from "../../__tests__/send-answers";
import { userOnFakeClock } from "../../__tests__/user";
import { ApiError, api, issuePatchRequest, issueStateRequest } from "../../api/client";
import { inboxQuery, issueQuery, projectIssuesQuery } from "../../api/queries";
import type {
  InboxRow,
  Issue,
  IssueDetails,
  IssueSummary,
  RepoProject,
  UserIssueState,
  UserState,
} from "../../api/types";
import { issueAsk } from "../inbox/inbox-fixture";
import { AssigneeControl } from "../issue/AssigneeControl";
import { issuePriority } from "../issue/issue-priority";
import { PriorityControl } from "../issue/PriorityControl";
import { RepoControl } from "../issue/RepoControl";
import { KeymapProvider } from "../shell/KeymapProvider";
import { SEND_DEADLINE_MS } from "../writes/keyed-write";
import { plainWrite, writesOf } from "../writes/write-store";
import { issueStatuses } from "./board-model";
import { staleBoardMessage, useBoardMoves } from "./board-moves";
import { IssueBoard } from "./IssueBoard";

function CurrentRoute(): ReactNode {
  const location = useLocation();
  return <output data-testid="current-route">{location.pathname}</output>;
}

function issue(overrides: Partial<IssueSummary> = {}): IssueSummary {
  return {
    route: null,
    repo: null,
    resolved_repo: null,
    route_status: null,
    route_holder: null,
    route_status_reason: null,
    key: "CORE-1",
    labels: [],
    last_seq: 1,
    open_asks: 0,
    progress: { tasks: null, children: null },
    parent: null,
    blocked_by: [],
    primary_artifact_id: "artifact-1",
    assignee: null,
    claim: null,
    components: { mode: "inherit", ids: [], unknown: [], reason: null, inherited_from: null },
    rank: "U",
    priority: null,
    status: "triage",
    title: "Plan the work",
    updated_at: "2026-09-13T00:00:00Z",
    closed_at: overrides.status === "done" ? "2026-09-13T00:00:00Z" : null,
    created_by: null,
    created_at: "2026-09-13T00:00:00Z",
    ...overrides,
  };
}

const issues: IssueSummary[] = [
  issue(),
  issue({ key: "CORE-2", last_seq: 3, rank: "k", status: "todo", title: "Ship the work" }),
  issue({ key: "CORE-3", rank: "p", status: "done", title: "Shipped" }),
];

function renderBoard(
  state: UserState = {},
  showEdges?: boolean,
  list: IssueSummary[] = issues,
  initialEntry = "/projects/CORE",
  lanes?: boolean
) {
  const listIssues = spyOn(api, "listIssues").mockImplementation(async (options = {}) => {
    const labels = options.label ?? [];
    return labels.length === 0
      ? list
      : list.filter((entry) => labels.every((label) => (entry.labels ?? []).includes(label)));
  });
  const getMyState = spyOn(api, "getMyState").mockResolvedValue(state);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter initialEntries={[initialEntry]}>
      <QueryClientProvider client={queryClient}>
        <KeymapProvider>
          <CurrentRoute />
          <IssueBoard lanes={lanes} project="CORE" showEdges={showEdges} />
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  return {
    listIssues,
    queryClient,
    cleanup() {
      view.unmount();
      listIssues.mockRestore();
      getMyState.mockRestore();
    },
  };
}

test("board collapses Icebox and Done into droppable rails by default", async () => {
  const { cleanup } = renderBoard();
  try {
    const board = await screen.findByRole("region", { name: "Project board" });
    const headers = within(board).getAllByTestId("board-column-header");
    expect(headers).toHaveLength(issueStatuses.length - 2);
    expect(headers.every((header) => header.classList.contains("min-h-[52px]"))).toBe(true);
    const rails = within(board).getAllByTestId("board-column-rail");
    expect(rails).toHaveLength(2);
    const done = within(board).getByRole("region", { name: "Done (collapsed)" });
    expect(done).toHaveTextContent("Done");
    expect(done).toHaveTextContent("1");
    expect(within(done).queryByRole("article")).not.toBeInTheDocument();
    expect(within(board).getByRole("region", { name: "Icebox (collapsed)" })).toBeDefined();
  } finally {
    cleanup();
  }
});

test("board shows all nine columns when the edges are expanded", async () => {
  const { cleanup } = renderBoard({}, true);
  try {
    const board = await screen.findByRole("region", { name: "Project board" });
    expect(within(board).getAllByTestId("board-column-header")).toHaveLength(issueStatuses.length);
    expect(within(board).queryAllByTestId("board-column-rail")).toHaveLength(0);
    expect(
      within(within(board).getByRole("region", { name: "Done" })).getByRole("article", {
        name: "CORE-3 Shipped",
      })
    ).toBeDefined();
  } finally {
    cleanup();
  }
});

test("the whole card is the drag activator: no Reorder button, no status pill, still an article", async () => {
  const { cleanup } = renderBoard();
  try {
    const board = await screen.findByRole("region", { name: "Project board" });
    const card = within(board).getByRole("article", { name: "CORE-1 Plan the work" });
    expect(card.tagName).toBe("ARTICLE");
    expect(card).toHaveAttribute("tabindex", "-1");
    expect(within(board).queryAllByRole("button", { name: /^Reorder/ })).toHaveLength(0);
    // The column header names the status; the card does not repeat it.
    expect(within(card).queryByText("Triage")).not.toBeInTheDocument();
    expect(within(card).getByRole("link", { name: /CORE-1/ })).toBeDefined();
    expect(within(card).getByRole("combobox", { name: "Priority of CORE-1" })).toBeDefined();
  } finally {
    cleanup();
  }
});

test("board cards show the unread dot when events are newer than the viewer's last read", async () => {
  const { cleanup } = renderBoard({
    "CORE-1": { dismissed: [], last_read_seq: 1, pinned: false, seq: 0 },
    "CORE-2": { dismissed: [], last_read_seq: 2, pinned: false, seq: 0 },
  });
  try {
    const board = await screen.findByRole("region", { name: "Project board" });
    const unread = within(board).getByRole("article", { name: "CORE-2 Ship the work" });
    await waitFor(() => {
      expect(unread.querySelector('[title="Unread"]')).not.toBeNull();
    });
    const read = within(board).getByRole("article", { name: "CORE-1 Plan the work" });
    expect(read.querySelector('[title="Unread"]')).toBeNull();
  } finally {
    cleanup();
  }
});

test("board shows a shared empty state when the project has no issues", async () => {
  const listIssues = spyOn(api, "listIssues").mockResolvedValue([]);
  const getMyState = spyOn(api, "getMyState").mockResolvedValue({});
  const view = render(
    <MemoryRouter>
      <QueryClientProvider
        client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}
      >
        <IssueBoard project="CORE" />
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    const board = await screen.findByRole("region", { name: "Project board" });
    expect(
      within(board).getByRole("region", { name: "Empty project board" }).textContent
    ).toContain("No issues in this project.");
  } finally {
    view.unmount();
    listIssues.mockRestore();
    getMyState.mockRestore();
  }
});

const patched: Issue = {
  closed_at: null,
  created_at: "2026-09-13T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  external_links: [],
  key: "CORE-1",
  labels: [],
  last_seq: 1,
  number: 1,
  parent: null,
  blocked_by: [],
  assignee: null,
  claim: null,
  components: { mode: "inherit", ids: [], unknown: [], reason: null, inherited_from: null },
  plan_artifact_id: "",
  decisions_artifact_id: "",
  primary_artifact_id: "artifact-1",
  verification_artifact_id: "",
  priority: null,
  project: "CORE",
  rank: "U",
  route: null,
  repo: null,
  resolved_repo: null,
  status: "todo",
  title: "Plan the work",
  updated_at: "2026-09-13T00:00:00Z",
};

function renderMoves(
  queryClient: QueryClient,
  labels: readonly string[] = [],
  isVisible?: (candidate: IssueSummary) => boolean
) {
  return renderHook(() => useBoardMoves("CORE", labels, isVisible), {
    wrapper: ({ children }: { children: ReactNode }) => (
      <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
    ),
  });
}

test("a failed move rolls the board back, refetches it and tells the truth about a stale rank", async () => {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const queryKey = ["issues", "project", "CORE"] as const;
  queryClient.setQueryData(queryKey, issues);
  const patchIssue = answerIssuePatches().mockRejectedValue(
    new ApiError(400, { code: "RANK_INPUT", error: "rank neighbors are not ordered" })
  );
  const hook = renderMoves(queryClient);
  try {
    await act(() => hook.result.current.moveCard("CORE-1", "todo", 1));
    expect(patchIssue).toHaveBeenCalledWith(
      "CORE-1",
      {
        rank: { after: "CORE-2" },
        status: "todo",
      },
      keyedAttempt
    );
    expect(hook.result.current.error).toBe(staleBoardMessage);
    expect(queryClient.getQueryData<IssueSummary[]>(queryKey)).toEqual(issues);
    expect(queryClient.getQueryState(queryKey)?.isInvalidated).toBe(true);
  } finally {
    hook.unmount();
    patchIssue.mockRestore();
  }
});

test("any other failure shows the server's message and still refetches", async () => {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const queryKey = ["issues", "project", "CORE"] as const;
  queryClient.setQueryData(queryKey, issues);
  const patchIssue = answerIssuePatches().mockRejectedValue(
    new ApiError(403, { code: "FORBIDDEN", error: "not your project" })
  );
  const hook = renderMoves(queryClient);
  try {
    await act(() => hook.result.current.moveCard("CORE-1", "todo", 1));
    expect(hook.result.current.error).toBe("not your project");
    expect(queryClient.getQueryState(queryKey)?.isInvalidated).toBe(true);
  } finally {
    hook.unmount();
    patchIssue.mockRestore();
  }
});

test("two rapid moves on one board run one after the other, the second from the optimistic state", async () => {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const queryKey = ["issues", "project", "CORE"] as const;
  queryClient.setQueryData(queryKey, issues);
  const settle: Array<(issue: Issue) => void> = [];
  const patchIssue = answerIssuePatches().mockImplementation(
    () =>
      new Promise<Issue>((resolve) => {
        settle.push(resolve);
      })
  );
  const hook = renderMoves(queryClient);
  try {
    let first: Promise<void> = Promise.resolve();
    let second: Promise<void> = Promise.resolve();
    act(() => {
      first = hook.result.current.moveCard("CORE-1", "todo", 1);
      second = hook.result.current.moveCard("CORE-3", "todo", 0);
    });
    // Both optimistic placements are on the board before either request is answered...
    expect(
      (queryClient.getQueryData(queryKey) as IssueSummary[])
        .filter((item) => item.status === "todo")
        .map((item) => item.key)
    ).toEqual(["CORE-3", "CORE-2", "CORE-1"]);
    // ...but only the first PATCH is in flight.
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(1));
    expect(patchIssue.mock.calls[0]).toEqual([
      "CORE-1",
      { rank: { after: "CORE-2" }, status: "todo" },
      keyedAttempt,
    ]);
    await act(async () => {
      settle[0]?.(patched);
      await first;
    });
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(2));
    // The second move's neighbours come from the board as the first move left it.
    expect(patchIssue.mock.calls[1]).toEqual([
      "CORE-3",
      { rank: { before: "CORE-2" }, status: "todo" },
      keyedAttempt,
    ]);
    await act(async () => {
      settle[1]?.({ ...patched, key: "CORE-3" });
      await second;
    });
    expect(hook.result.current.error).toBeUndefined();
  } finally {
    hook.unmount();
    patchIssue.mockRestore();
  }
});

// R7 of dispatch://INFRA-299. A drop's placement is written in the press's tick as the store's
// own writing (`Writes.placeAhead`), placed on the board as every earlier drop left it: a drop
// made while an earlier one is unanswered keeps its own place, and the earlier drop's rule is not
// run again over it.
test("a second drop while the first is unanswered leaves both cards where they were dropped", async () => {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const queryKey = ["issues", "project", "CORE"] as const;
  queryClient.setQueryData(queryKey, [
    issue({ key: "CORE-1", rank: "a", status: "todo", title: "Alpha" }),
    issue({ key: "CORE-2", rank: "b", status: "todo", title: "Bravo" }),
    issue({ key: "CORE-3", rank: "c", status: "todo", title: "Charlie" }),
    issue({ key: "CORE-4", rank: "d", status: "in_progress", title: "Delta" }),
    issue({ key: "CORE-5", rank: "e", status: "in_progress", title: "Echo" }),
  ]);
  const todoKeys = () =>
    (queryClient.getQueryData(queryKey) as IssueSummary[])
      .filter((item) => item.status === "todo")
      .map((item) => item.key);
  const answers: ((answer: Issue) => void)[] = [];
  const patchIssue = answerIssuePatches().mockImplementation(() => {
    const { promise, resolve } = Promise.withResolvers<Issue>();
    answers.push(resolve);
    return promise;
  });
  const hook = renderMoves(queryClient);
  let first: Promise<void> = Promise.resolve();
  let second: Promise<void> = Promise.resolve();
  try {
    act(() => {
      first = hook.result.current.moveCard("CORE-4", "todo", 2);
    });
    // The first drop's write is out, and live.
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(1));
    expect(todoKeys()).toEqual(["CORE-1", "CORE-2", "CORE-4", "CORE-3"]);

    act(() => {
      second = hook.result.current.moveCard("CORE-5", "todo", 1);
    });
    expect(todoKeys()).toEqual(["CORE-1", "CORE-5", "CORE-2", "CORE-4", "CORE-3"]);

    await act(async () => {
      answers[0]?.({ ...patched, key: "CORE-4" });
      await first;
    });
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(2));
    await act(async () => {
      answers[1]?.({ ...patched, key: "CORE-5" });
      await second;
    });
  } finally {
    hook.unmount();
    patchIssue.mockRestore();
  }
});

// Todo: Alpha, Bravo, Charlie; In progress: Delta - the keyboard rows' seed.
const keyboardIssues: IssueSummary[] = [
  issue({ key: "CORE-1", rank: "a", status: "todo", title: "Alpha" }),
  issue({ key: "CORE-2", rank: "b", status: "todo", title: "Bravo" }),
  issue({ key: "CORE-3", rank: "c", status: "todo", title: "Charlie" }),
  issue({ key: "CORE-4", rank: "d", status: "in_progress", title: "Delta" }),
];

/** A PATCH that never answers: the board stays as the move placed it, so a test reads the
 *  optimistic placement rather than the seed the mocked refetch would hand back. */
function patchInFlight() {
  return answerIssuePatches().mockImplementation(() => new Promise<Issue>(() => {}));
}

test("Shift+J moves the focused card down through moveCard, announces the new position and keeps focus on it; Shift+K at the top does nothing", async () => {
  const user = userEvent.setup();
  const patchIssue = patchInFlight();
  const { cleanup } = renderBoard({}, undefined, keyboardIssues);
  try {
    const board = await screen.findByRole("region", { name: "Project board" });
    const alpha = within(board).getByRole("article", { name: "CORE-1 Alpha" });
    const status = within(board).getByRole("status", { name: "Board announcements" });
    expect(status).toHaveClass("sr-only");
    act(() => alpha.focus());
    expect(alpha).toHaveFocus();

    await user.keyboard("{Shift>}K{/Shift}");
    expect(patchIssue).not.toHaveBeenCalled();
    expect(status.textContent).toBe("");

    await user.keyboard("{Shift>}J{/Shift}");
    expect(patchIssue).toHaveBeenCalledWith(
      "CORE-1",
      {
        rank: { after: "CORE-2", before: "CORE-3" },
      },
      keyedAttempt
    );
    expect(status.textContent).toBe("CORE-1 → Todo, position 2 of 3");
    const todo = within(board).getByRole("region", { name: "Todo" });
    await waitFor(() =>
      expect(
        within(todo)
          .getAllByRole("article")
          .map((node) => node.textContent)
      ).toEqual([
        expect.stringContaining("Bravo"),
        expect.stringContaining("Alpha"),
        expect.stringContaining("Charlie"),
      ])
    );
    expect(within(board).getByRole("article", { name: "CORE-1 Alpha" })).toHaveFocus();
  } finally {
    cleanup();
    patchIssue.mockRestore();
  }
});

test("Shift+L moves the focused card to the top of the next status and focus follows the remounted card", async () => {
  const user = userEvent.setup();
  const patchIssue = patchInFlight();
  const { cleanup } = renderBoard({}, undefined, keyboardIssues);
  try {
    const board = await screen.findByRole("region", { name: "Project board" });
    const bravo = within(board).getByRole("article", { name: "CORE-2 Bravo" });
    act(() => bravo.focus());
    await user.keyboard("{Shift>}L{/Shift}");
    expect(patchIssue).toHaveBeenCalledWith(
      "CORE-2",
      {
        rank: { before: "CORE-4" },
        status: "in_progress",
      },
      keyedAttempt
    );
    expect(within(board).getByRole("status", { name: "Board announcements" }).textContent).toBe(
      "CORE-2 → In progress, position 1 of 2"
    );
    const inProgress = within(board).getByRole("region", { name: "In progress" });
    await waitFor(() =>
      expect(
        within(inProgress)
          .getAllByRole("article")
          .map((node) => node.textContent)
      ).toEqual([expect.stringContaining("Bravo"), expect.stringContaining("Delta")])
    );
    expect(within(inProgress).getByRole("article", { name: "CORE-2 Bravo" })).toHaveFocus();
  } finally {
    cleanup();
    patchIssue.mockRestore();
  }
});

const laneIssues: IssueSummary[] = [
  issue({ key: "CORE-1", priority: 0, rank: "a", status: "todo", title: "P0 card" }),
  issue({ key: "CORE-2", priority: 3, rank: "b", status: "todo", title: "P3 card" }),
  issue({
    key: "CORE-3",
    priority: null,
    rank: "c",
    status: "in_progress",
    title: "No priority card",
  }),
];

test("lanes group the board into priority bands, each with its own set of columns", async () => {
  const { cleanup } = renderBoard({}, undefined, laneIssues, "/projects/CORE", true);
  try {
    const p0Lane = await screen.findByRole("region", { name: "P0 lane" });
    expect(within(p0Lane).getAllByTestId("board-column-header")).toHaveLength(7);
    expect(within(p0Lane).getAllByTestId("board-column-rail")).toHaveLength(2);
    expect(
      within(within(p0Lane).getByRole("region", { name: "Todo" })).getByRole("article", {
        name: "CORE-1 P0 card",
      })
    ).toBeDefined();

    const p3Lane = screen.getByRole("region", { name: "P3 lane" });
    expect(
      within(within(p3Lane).getByRole("region", { name: "Todo" })).getByRole("article", {
        name: "CORE-2 P3 card",
      })
    ).toBeDefined();
    // The P0 lane's Todo column holds only the P0 card, not the P3 one.
    expect(within(p0Lane).getByRole("region", { name: "Todo" }).textContent).not.toContain(
      "P3 card"
    );

    const noPriorityLane = screen.getByRole("region", { name: "No priority lane" });
    expect(
      within(within(noPriorityLane).getByRole("region", { name: "In progress" })).getByRole(
        "article",
        { name: "CORE-3 No priority card" }
      )
    ).toBeDefined();
  } finally {
    cleanup();
  }
});

test("without the lanes prop the board renders the flat columns, no lane headings", async () => {
  const { cleanup } = renderBoard({}, undefined, laneIssues);
  try {
    const board = await screen.findByRole("region", { name: "Project board" });
    expect(within(board).queryByRole("region", { name: "P0 lane" })).not.toBeInTheDocument();
    expect(
      within(within(board).getByRole("region", { name: "Todo" })).getAllByRole("article")
    ).toHaveLength(2);
  } finally {
    cleanup();
  }
});

test("within a lane, Shift+L moves a card to the next status in the same lane without touching priority", async () => {
  const user = userEvent.setup();
  const patchIssue = patchInFlight();
  const { cleanup } = renderBoard({}, undefined, laneIssues, "/projects/CORE", true);
  try {
    const p0Lane = await screen.findByRole("region", { name: "P0 lane" });
    const p0Card = within(p0Lane).getByRole("article", { name: "CORE-1 P0 card" });
    act(() => p0Card.focus());
    await user.keyboard("{Shift>}L{/Shift}");
    // Same lane, so only status and rank move - priority is not part of this PATCH.
    expect(patchIssue).toHaveBeenCalledWith(
      "CORE-1",
      { rank: {}, status: "in_progress" },
      keyedAttempt
    );
    const movedInProgress = within(p0Lane).getByRole("region", { name: "In progress" });
    await waitFor(() =>
      expect(within(movedInProgress).getByRole("article", { name: "CORE-1 P0 card" })).toBeDefined()
    );
  } finally {
    cleanup();
    patchIssue.mockRestore();
  }
});

test("with lanes on, the first roving key before anything is focused lands in the first lane's first cell, not flat cross-lane order", async () => {
  const user = userEvent.setup();
  // CORE-1 (P3) sorts first by rank in the merged Todo column a flat board would use; the fix
  // lands in the first swimlane (P0) instead, on CORE-2.
  const firstLaneIssues: IssueSummary[] = [
    issue({ key: "CORE-1", priority: 3, rank: "a", status: "todo", title: "P3 card" }),
    issue({ key: "CORE-2", priority: 0, rank: "b", status: "todo", title: "P0 card" }),
  ];
  const { cleanup } = renderBoard({}, undefined, firstLaneIssues, "/projects/CORE", true);
  try {
    const p0Lane = await screen.findByRole("region", { name: "P0 lane" });
    const p0Card = within(p0Lane).getByRole("article", { name: "CORE-2 P0 card" });
    await user.keyboard("j");
    expect(p0Card).toHaveFocus();
  } finally {
    cleanup();
  }
});

function articleLabels(region: HTMLElement): (string | null)[] {
  return within(region)
    .queryAllByRole("article")
    .map((node) => node.getAttribute("aria-label"));
}

const boardListKey = ["issues", "project", "CORE"] as const;

test("a board refetch already on the wire when a card moves is dropped: the card stays where it moved and keeps focus", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const patchIssue = patchInFlight();
  const { cleanup, listIssues, queryClient } = renderBoard({}, undefined, keyboardIssues);
  try {
    const board = await screen.findByRole("region", { name: "Project board" });
    const bravo = within(board).getByRole("article", { name: "CORE-2 Bravo" });
    // An event-stream refetch goes out before the move, and its answer - the board as it was -
    // arrives after the move has placed the card.
    const inFlight = Promise.withResolvers<IssueSummary[]>();
    listIssues.mockImplementationOnce(() => inFlight.promise);
    await act(async () => {
      void queryClient.invalidateQueries({ queryKey: ["issues"] });
    });
    await waitFor(() => expect(listIssues).toHaveBeenCalledTimes(2));
    act(() => bravo.focus());
    await user.keyboard("{Shift>}L{/Shift}");
    const inProgress = within(board).getByRole("region", { name: "In progress" });
    await waitFor(() =>
      expect(articleLabels(inProgress)).toEqual(["CORE-2 Bravo", "CORE-4 Delta"])
    );

    await act(async () => {
      inFlight.resolve(keyboardIssues);
    });
    await waitFor(() => expect(queryClient.getQueryState(boardListKey)?.fetchStatus).toBe("idle"));
    // The stale answer, had it landed, has notified the board by now: query-core notifies on a
    // zero-delay timer queued when the data is written, ahead of this one.
    await tick(0);
    expect(articleLabels(inProgress)).toEqual(["CORE-2 Bravo", "CORE-4 Delta"]);
    expect(within(inProgress).getByRole("article", { name: "CORE-2 Bravo" })).toHaveFocus();
  } finally {
    realClock();
    cleanup();
    patchIssue.mockRestore();
  }
});

// R7. No reader is held: a refetch asked for while a move is unanswered goes out at once and
// its answer is read. That the moved card keeps its placement over such an answer is the move
// write's own to show again (`features/project/board-moves.ts`, whose write shows the drop as a
// change to the list as it stands); this row reads the reader's side of it - the fetch, and the
// board once the server has applied the move.
test("a board refetch asked for while a move is unanswered goes out at once, and the answer is read", async () => {
  const user = userEvent.setup();
  const answer = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches().mockImplementation(() => answer.promise);
  const { cleanup, listIssues, queryClient } = renderBoard({}, undefined, keyboardIssues);
  try {
    const board = await screen.findByRole("region", { name: "Project board" });
    const bravo = within(board).getByRole("article", { name: "CORE-2 Bravo" });
    act(() => bravo.focus());
    await user.keyboard("{Shift>}L{/Shift}");
    const inProgress = within(board).getByRole("region", { name: "In progress" });
    await waitFor(() =>
      expect(articleLabels(inProgress)).toEqual(["CORE-2 Bravo", "CORE-4 Delta"])
    );
    expect(listIssues).toHaveBeenCalledTimes(1);

    // An event arrives while the PATCH is unanswered: the read goes out, nothing holds it.
    listIssues.mockResolvedValue(
      keyboardIssues.map((entry) =>
        entry.key === "CORE-2" ? { ...entry, status: "in_progress" } : entry
      )
    );
    await act(async () => {
      void queryClient.invalidateQueries({ queryKey: ["issues"] });
      await settle();
    });
    await waitFor(() => expect(listIssues).toHaveBeenCalledTimes(2));

    // The server applies the move and answers; the board reads the server's list.
    await act(async () => {
      answer.resolve({ ...patched, key: "CORE-2", status: "in_progress" });
      await settle();
    });
    await waitFor(() => expect(queryClient.getQueryState(boardListKey)?.fetchStatus).toBe("idle"));
    expect(articleLabels(inProgress)).toEqual(["CORE-2 Bravo", "CORE-4 Delta"]);
  } finally {
    cleanup();
    patchIssue.mockRestore();
  }
});

// The rows from here to the end of this block measure R7 of dispatch://INFRA-299: a change a
// person has made and not saved survives every answer that lands under it, and no reader is
// held. The three field controls - priority, assignee and repository - show their pick through
// the store (`features/issue/useIssueFieldWrite.ts`, `WriteSpec.shows`), which shows it again
// over each answer that lands on a cache the write wrote, until the write settles. So each row
// here lets the answer land - the server's older value for the issue written, and a change of
// another row the same answer carries - and reads that the pick is still on screen and that the
// other row's change arrived.

/** The project list as the server has it: CORE-1 at P1, nobody, and no repository of its own. */
const fieldIssues: IssueSummary[] = [
  issue({ key: "CORE-1", priority: 1, rank: "a", status: "todo", title: "Alpha" }),
  issue({ key: "CORE-2", priority: 1, rank: "b", status: "todo", title: "Bravo" }),
];

/** The same list once the server has taken a pick of CORE-1's, for the read that follows it. */
function fieldIssuesWith(fields: Partial<IssueSummary>): IssueSummary[] {
  return fieldIssues.map((entry) => (entry.key === "CORE-1" ? { ...entry, ...fields } : entry));
}

/** The list as another browser has left it while this one holds an unsaved pick: CORE-1 as the
 *  server still has it, and CORE-2 moved to P3 by somebody else. That second row is what every
 *  row below reads to see that the whole answer arrived. */
const listSinceOtherBrowser: IssueSummary[] = fieldIssues.map((entry) =>
  entry.key === "CORE-2" ? { ...entry, priority: 3 } : entry
);

/** The Inbox as the server has it: one row on CORE-1, one on an issue of another project. An
 *  Inbox row carries its issue's priority and the person it waits on, and no repository. */
const raceInbox: InboxRow[] = [
  issueAsk({
    id: "ask-core-1",
    issue: { assignee: null, key: "CORE-1", title: "Alpha" },
    issue_key: "CORE-1",
    priority: 1,
  }),
  issueAsk({
    id: "ask-other-1",
    issue: { assignee: null, key: "OTHER-1", title: "Another project's" },
    issue_key: "OTHER-1",
    priority: 1,
  }),
];

/** The Inbox as another browser has left it: the other project's row moved to P3, which is the
 *  row an answer landing under an unsaved pick must still bring through. */
const inboxSinceOtherBrowser: InboxRow[] = raceInbox.map((row) =>
  row.issue_key === "OTHER-1" ? { ...row, priority: 3 } : row
);

const repoMappings: RepoProject[] = [
  {
    created_at: "2026-09-13T00:00:00Z",
    created_by: { id: "alice", kind: "user" },
    default: true,
    project: "CORE",
    renamed_to: null,
    repo: "acme/widgets",
  },
];

const inboxKey = inboxQuery().queryKey;

/** The issue as the server has it for the issue's own read (`["issue", "CORE-1"]`), the cache
 *  every field write writes beside the lists. */
const raceDetails: IssueDetails = {
  ...patched,
  artifacts: [],
  children: [],
  design_gates: [],
  holder_state: null,
  last_seen: null,
  open_asks: [],
  priority: 1,
  progress: { tasks: null, children: null },
  referenced_by_count: 0,
  route_holder: null,
  route_status: null,
  route_status_reason: null,
  takeable_at: null,
  title: "Alpha",
};

/** The issue's own read, as the header draws it from the issue rather than from a list: the
 *  priority a write of it shows its change on, and the title the same answer carries. */
function IssueDetail(): ReactNode {
  const details = useQuery(issueQuery("CORE-1"));
  if (details.data === undefined) {
    return null;
  }
  return (
    <div data-testid="issue-detail">
      <output data-testid="detail-priority">{String(details.data.priority)}</output>
      <output data-testid="detail-title">{details.data.title}</output>
    </div>
  );
}

/** The issue's assignee and repository controls, drawn from the project list the board reads, as
 *  the issue header draws them from the issue: what the reader sees while a write of either
 *  field is out, and what a list answer that lands over it would change. */
function ListFields(): ReactNode {
  const list = useQuery(projectIssuesQuery("CORE", []));
  const row = (list.data ?? []).find((entry) => entry.key === "CORE-1");
  if (row === undefined) {
    return null;
  }
  return (
    <div data-testid="issue-fields">
      <AssigneeControl assignee={row.assignee} issueKey="CORE-1" />
      <RepoControl
        issueKey="CORE-1"
        project="CORE"
        repo={row.repo}
        resolvedRepo={row.resolved_repo}
      />
    </div>
  );
}

/** The Inbox's own read, each row with the priority control the Inbox draws on it and the person
 *  it waits on, which is its issue's assignee until somebody hands the ask on. */
function InboxRows(): ReactNode {
  const inbox = useQuery(inboxQuery());
  return (
    <ul aria-label="Inbox rows">
      {(inbox.data ?? []).map((row) => (
        <li data-testid={`inbox-row-${row.issue_key}`} key={row.id}>
          {row.issue_key === null ? null : (
            <PriorityControl issueKey={row.issue_key} priority={issuePriority(row.priority)} />
          )}
          <output data-testid={`inbox-waits-${row.issue_key}`}>
            {row.waiting_on_person ?? "nobody"}
          </output>
        </li>
      ))}
    </ul>
  );
}

/** The page these rows read: the board, CORE-1's assignee and repository controls over the same
 *  project list, the issue's own read, and the Inbox's own read. One query client, so one write
 *  store (`writesOf`) and one copy of each cache. */
function renderRace(issues: IssueSummary[] = fieldIssues) {
  const listIssues = spyOn(api, "listIssues").mockResolvedValue(issues);
  const getMyState = spyOn(api, "getMyState").mockResolvedValue({});
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(raceInbox);
  const listUsers = spyOn(api, "listUsers").mockResolvedValue([
    { login: "alice" },
    { login: "bob" },
  ]);
  const listRepoProjects = spyOn(api, "listRepoProjects").mockResolvedValue(repoMappings);
  const getIssue = spyOn(api, "getIssue").mockResolvedValue(raceDetails);
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const view = render(
    <MemoryRouter initialEntries={["/projects/CORE"]}>
      <QueryClientProvider client={queryClient}>
        <KeymapProvider>
          <IssueBoard project="CORE" />
          <ListFields />
          <IssueDetail />
          <InboxRows />
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  return {
    getInbox,
    getIssue,
    listIssues,
    queryClient,
    cleanup() {
      view.unmount();
      for (const spy of [listIssues, getMyState, getInbox, getIssue, listUsers, listRepoProjects]) {
        spy.mockRestore();
      }
    },
  };
}

/** The page as the reader finds it: the board, the issue's own controls, and the Inbox's rows. */
async function raceReady(): Promise<HTMLElement> {
  const board = await screen.findByRole("region", { name: "Project board" });
  await screen.findByTestId("issue-fields");
  await screen.findByRole("combobox", { name: "Repository of CORE-1" });
  await screen.findByTestId("issue-detail");
  await screen.findByTestId("inbox-row-CORE-1");
  return board;
}

/** The priority select of `key` inside `scope`: the board's card carries one, as the Inbox's row
 *  does, so each row says which it reads. */
function priorityIn(scope: HTMLElement, key: string): HTMLSelectElement {
  return within(scope).getByRole("combobox", { name: `Priority of ${key}` }) as HTMLSelectElement;
}

function assigneeSelect(): HTMLSelectElement {
  return screen.getByRole("combobox", { name: "Assignee of CORE-1" }) as HTMLSelectElement;
}

function repoSelect(): HTMLSelectElement {
  return screen.getByRole("combobox", { name: "Repository of CORE-1" }) as HTMLSelectElement;
}

/** An event refresh asks `queryKey` again, and everything its answer could do runs: the row then
 *  reads whether the fetch went out or is waiting. */
async function askAgain(queryClient: QueryClient, queryKey: readonly unknown[]): Promise<void> {
  await act(async () => {
    void queryClient.invalidateQueries({ queryKey });
    await settle();
  });
}

/** The Inbox's row for `key`'s ask. */
function inboxRow(key: string): HTMLElement {
  return screen.getByTestId(`inbox-row-${key}`);
}

/** Picks `login` in the assignee control, which reads the people as the reader reaches for it:
 *  a bare focus, which user-event has no form for, then the pick. */
async function pickAssignee(user: UserEvent, login: string): Promise<void> {
  fireEvent.focus(assigneeSelect());
  await waitFor(() =>
    expect([...assigneeSelect().options].map((option) => option.value)).toContain(login)
  );
  await user.selectOptions(assigneeSelect(), login);
}

test("a project list answer landing while a priority write is out keeps the pick and brings every other row", async () => {
  const user = userEvent.setup();
  const answer = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches().mockImplementation(() => answer.promise);
  const race = renderRace();
  try {
    const board = await raceReady();
    await user.selectOptions(priorityIn(board, "CORE-1"), "0");
    await waitFor(() =>
      expect(patchIssue).toHaveBeenCalledWith("CORE-1", { priority: 0 }, keyedAttempt)
    );

    // An event arrives while the PATCH is out. The server still holds P1 for CORE-1, and
    // another browser has moved CORE-2 to P3 since this page read the list.
    race.listIssues.mockResolvedValue(listSinceOtherBrowser);
    await askAgain(race.queryClient, ["issues"]);

    // The answer landed whole - nothing was held - and the write showed its pick again over it.
    await waitFor(() => expect(race.listIssues).toHaveBeenCalledTimes(2));
    expect(race.queryClient.getQueryState(boardListKey)?.fetchStatus).toBe("idle");
    expect(priorityIn(board, "CORE-1").value).toBe("0");
    expect(priorityIn(board, "CORE-2").value).toBe("3");

    // The server takes the pick; its own answer is what the page reads from then on.
    race.listIssues.mockResolvedValue(fieldIssuesWith({ priority: 0 }));
    await act(async () => {
      answer.resolve({ ...patched, key: "CORE-1", priority: 0 });
      await settle();
    });
    await waitFor(() => expect(race.listIssues.mock.calls.length).toBeGreaterThan(2));
    expect(priorityIn(board, "CORE-1").value).toBe("0");
  } finally {
    race.cleanup();
    patchIssue.mockRestore();
  }
});

test("a project list answer landing while a priority write waits its turn keeps the pick", async () => {
  const user = userEvent.setup();
  const first = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches().mockImplementation(() => first.promise);
  const race = renderRace();
  try {
    const board = await raceReady();
    const writes = writesOf(race.queryClient);
    // A write of CORE-1 that never answers, so the pick behind it waits its turn.
    await act(async () => {
      void writes
        .run({
          detail: null,
          kind: plainWrite,
          label: "CORE-1 set to done",
          presentation: "indicator",
          request: issuePatchRequest("CORE-1", { status: "done" }),
          target: "issue:CORE-1",
        })
        .catch(() => undefined);
      await settle();
    });
    await user.selectOptions(priorityIn(board, "CORE-1"), "0");
    await waitFor(() =>
      expect(writes.rows().map((write) => write.status)).toEqual(["sending", "queued"])
    );

    race.listIssues.mockResolvedValue(listSinceOtherBrowser);
    await askAgain(race.queryClient, ["issues"]);

    await waitFor(() => expect(race.listIssues).toHaveBeenCalledTimes(2));
    expect(priorityIn(board, "CORE-1").value).toBe("0");
    expect(priorityIn(board, "CORE-2").value).toBe("3");
  } finally {
    race.cleanup();
    patchIssue.mockRestore();
  }
});

test("a project list answer landing while an assignee write is out keeps the person and brings every other row", async () => {
  const user = userEvent.setup();
  const answer = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches().mockImplementation(() => answer.promise);
  const race = renderRace();
  try {
    const board = await raceReady();
    await pickAssignee(user, "bob");
    await waitFor(() =>
      expect(patchIssue).toHaveBeenCalledWith("CORE-1", { assignee: "bob" }, keyedAttempt)
    );

    // The server still holds nobody: its answer alone would unassign the issue on screen.
    race.listIssues.mockResolvedValue(listSinceOtherBrowser);
    await askAgain(race.queryClient, ["issues"]);

    await waitFor(() => expect(race.listIssues).toHaveBeenCalledTimes(2));
    expect(assigneeSelect().value).toBe("bob");
    expect(priorityIn(board, "CORE-2").value).toBe("3");

    race.listIssues.mockResolvedValue(fieldIssuesWith({ assignee: "bob" }));
    await act(async () => {
      answer.resolve({ ...patched, assignee: "bob", key: "CORE-1" });
      await settle();
    });
    await waitFor(() => expect(race.listIssues.mock.calls.length).toBeGreaterThan(2));
    expect(assigneeSelect().value).toBe("bob");
  } finally {
    race.cleanup();
    patchIssue.mockRestore();
  }
});

test("a project list answer landing while a repository write is out keeps the repository and brings every other row", async () => {
  const user = userEvent.setup();
  const answer = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches().mockImplementation(() => answer.promise);
  const race = renderRace();
  try {
    const board = await raceReady();
    await user.selectOptions(repoSelect(), "acme/widgets");
    await waitFor(() =>
      expect(patchIssue).toHaveBeenCalledWith("CORE-1", { repo: "acme/widgets" }, keyedAttempt)
    );

    // The server still holds no repository of its own: its answer alone would clear the pick.
    race.listIssues.mockResolvedValue(listSinceOtherBrowser);
    await askAgain(race.queryClient, ["issues"]);

    await waitFor(() => expect(race.listIssues).toHaveBeenCalledTimes(2));
    expect(repoSelect().value).toBe("acme/widgets");
    expect(priorityIn(board, "CORE-2").value).toBe("3");

    race.listIssues.mockResolvedValue(
      fieldIssuesWith({ repo: "acme/widgets", resolved_repo: "acme/widgets" })
    );
    await act(async () => {
      answer.resolve({
        ...patched,
        key: "CORE-1",
        repo: "acme/widgets",
        resolved_repo: "acme/widgets",
      });
      await settle();
    });
    await waitFor(() => expect(race.listIssues.mock.calls.length).toBeGreaterThan(2));
    expect(repoSelect().value).toBe("acme/widgets");
  } finally {
    race.cleanup();
    patchIssue.mockRestore();
  }
});

test("an Inbox answer landing while a priority write is out keeps the pick and brings every other row", async () => {
  const user = userEvent.setup();
  const answer = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches().mockImplementation(() => answer.promise);
  const race = renderRace();
  try {
    await raceReady();
    await user.selectOptions(priorityIn(inboxRow("CORE-1"), "CORE-1"), "0");
    await waitFor(() =>
      expect(patchIssue).toHaveBeenCalledWith("CORE-1", { priority: 0 }, keyedAttempt)
    );

    race.getInbox.mockResolvedValue(inboxSinceOtherBrowser);
    await askAgain(race.queryClient, ["inbox"]);

    await waitFor(() => expect(race.getInbox).toHaveBeenCalledTimes(2));
    expect(race.queryClient.getQueryState(inboxKey)?.fetchStatus).toBe("idle");
    expect(priorityIn(inboxRow("CORE-1"), "CORE-1").value).toBe("0");
    expect(priorityIn(inboxRow("OTHER-1"), "OTHER-1").value).toBe("3");

    race.getInbox.mockResolvedValue(
      raceInbox.map((row) => (row.issue_key === "CORE-1" ? { ...row, priority: 0 } : row))
    );
    await act(async () => {
      answer.resolve({ ...patched, key: "CORE-1", priority: 0 });
      await settle();
    });
    await waitFor(() => expect(race.getInbox.mock.calls.length).toBeGreaterThan(2));
    expect(priorityIn(inboxRow("CORE-1"), "CORE-1").value).toBe("0");
  } finally {
    race.cleanup();
    patchIssue.mockRestore();
  }
});

test("an Inbox answer landing while an assignee write is out keeps the person and brings every other row", async () => {
  const user = userEvent.setup();
  const answer = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches().mockImplementation(() => answer.promise);
  const race = renderRace();
  try {
    await raceReady();
    await pickAssignee(user, "bob");
    await waitFor(() =>
      expect(patchIssue).toHaveBeenCalledWith("CORE-1", { assignee: "bob" }, keyedAttempt)
    );
    expect(screen.getByTestId("inbox-waits-CORE-1").textContent).toBe("bob");

    // An Inbox row waits on the person its issue is assigned to until somebody hands the ask
    // on, so the server's answer alone would put "nobody" back on this row.
    race.getInbox.mockResolvedValue(inboxSinceOtherBrowser);
    await askAgain(race.queryClient, ["inbox"]);

    await waitFor(() => expect(race.getInbox).toHaveBeenCalledTimes(2));
    expect(screen.getByTestId("inbox-waits-CORE-1").textContent).toBe("bob");
    expect(priorityIn(inboxRow("OTHER-1"), "OTHER-1").value).toBe("3");
  } finally {
    race.cleanup();
    patchIssue.mockRestore();
  }
});

test("a list answer landing while a write is late and still being tried keeps the pick", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const answer = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches().mockImplementation(() => answer.promise);
  const race = renderRace();
  try {
    const board = await raceReady();
    await user.selectOptions(priorityIn(board, "CORE-1"), "0");
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(1));
    // Past the send deadline the write is late: it is still out, still being tried, and its
    // answer is still to come, so it is live and shows its pick again.
    await tick(SEND_DEADLINE_MS + 1_000);
    expect(
      writesOf(race.queryClient)
        .rows()
        .map((write) => write.status)
    ).toEqual(["late"]);

    race.listIssues.mockResolvedValue(listSinceOtherBrowser);
    await askAgain(race.queryClient, ["issues"]);

    await waitFor(() => expect(race.listIssues).toHaveBeenCalledTimes(2));
    expect(priorityIn(board, "CORE-1").value).toBe("0");
    expect(priorityIn(board, "CORE-2").value).toBe("3");
  } finally {
    realClock();
    race.cleanup();
    patchIssue.mockRestore();
  }
});

test("a refused write shows nothing again: the list answer's own value stands", async () => {
  const user = userEvent.setup();
  const patchIssue = answerIssuePatches().mockRejectedValue(
    new ApiError(422, { error: "Dispatch refused the priority" })
  );
  const race = renderRace();
  try {
    const board = await raceReady();
    await user.selectOptions(priorityIn(board, "CORE-1"), "0");
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(1));
    // The engine shows its own refusals, so the store lets the write go the moment Dispatch
    // refuses it, and the pick it showed was rolled back with it.
    await waitFor(() => expect(writesOf(race.queryClient).rows()).toEqual([]));

    race.listIssues.mockResolvedValue(listSinceOtherBrowser);
    await askAgain(race.queryClient, ["issues"]);

    await waitFor(() => expect(race.listIssues.mock.calls.length).toBeGreaterThan(1));
    await waitFor(() => expect(priorityIn(board, "CORE-2").value).toBe("3"));
    expect(priorityIn(board, "CORE-1").value).toBe("1");
  } finally {
    race.cleanup();
    patchIssue.mockRestore();
  }
});

test("a discarded write shows nothing again: the reader took it back", async () => {
  const answer = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches().mockImplementation(() => answer.promise);
  const race = renderRace();
  try {
    const board = await raceReady();
    const writes = writesOf(race.queryClient);
    // A write of CORE-1 that holds the line, and behind it one that shows a pick and waits its
    // turn: a write waiting its turn, unsent, is the reader's to discard.
    await act(async () => {
      void writes
        .run({
          detail: null,
          kind: plainWrite,
          label: "CORE-1 set to done",
          presentation: "indicator",
          request: issuePatchRequest("CORE-1", { status: "done" }),
          target: "issue:CORE-1",
        })
        .catch(() => undefined);
      await settle();
    });
    const key = "discarded-pick";
    await act(async () => {
      void writes
        .run({
          detail: null,
          key,
          kind: plainWrite,
          label: "CORE-1 set to P0",
          presentation: "indicator",
          request: issuePatchRequest("CORE-1", { priority: 0 }),
          shows: (shown) => {
            shown.under<IssueSummary[]>(["issues", "project"], (rows) =>
              rows.map((row) => (row.key === "CORE-1" ? { ...row, priority: 0 } : row))
            );
          },
          target: "issue:CORE-1",
        })
        .catch(() => undefined);
      await settle();
    });
    expect(priorityIn(board, "CORE-1").value).toBe("0");
    act(() => {
      expect(writes.discard(writes.idOf(key) ?? 0)).toBe(true);
    });
    // Its press is refused with no failure to show, so the store reads again the lists its
    // change wrote (R10), and the server's answer stands.
    await waitFor(() => expect(race.listIssues).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(priorityIn(board, "CORE-1").value).toBe("1"));

    race.listIssues.mockResolvedValue(listSinceOtherBrowser);
    await askAgain(race.queryClient, ["issues"]);

    await waitFor(() => expect(race.listIssues).toHaveBeenCalledTimes(3));
    await waitFor(() => expect(priorityIn(board, "CORE-1").value).toBe("1"));
    expect(priorityIn(board, "CORE-2").value).toBe("3");
  } finally {
    race.cleanup();
    patchIssue.mockRestore();
  }
});

test("a settled write shows nothing again: what the server answers is the page's from then on", async () => {
  const user = userEvent.setup();
  const answer = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches().mockImplementation(() => answer.promise);
  const race = renderRace();
  try {
    const board = await raceReady();
    await user.selectOptions(priorityIn(board, "CORE-1"), "0");
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(1));
    race.listIssues.mockResolvedValue(fieldIssuesWith({ priority: 0 }));
    await act(async () => {
      answer.resolve({ ...patched, key: "CORE-1", priority: 0 });
      await settle();
    });
    await waitFor(() => expect(writesOf(race.queryClient).rows()).toEqual([]));

    // Another browser moves CORE-1 back to P1 and CORE-2 to P3. The write is made, so nothing
    // is shown over this answer and the server's value stands on both rows.
    race.listIssues.mockResolvedValue(listSinceOtherBrowser);
    await askAgain(race.queryClient, ["issues"]);

    await waitFor(() => expect(priorityIn(board, "CORE-1").value).toBe("1"));
    expect(priorityIn(board, "CORE-2").value).toBe("3");
  } finally {
    race.cleanup();
    patchIssue.mockRestore();
  }
});

test("a read mark shows nothing, and so shows nothing again", async () => {
  const mark = Promise.withResolvers<UserIssueState>();
  const putIssueState = answerIssueStates().mockImplementation(() => mark.promise);
  const race = renderRace();
  try {
    const board = await raceReady();
    const writes = writesOf(race.queryClient);
    // The read mark every conversation sends (`useSendMarkOnce`): one write of the store, on the
    // reader's own line for the issue (`read:issue:<KEY>`), which shows no change of any cache.
    await act(async () => {
      void writes
        .run({
          detail: null,
          kind: plainWrite,
          label: "Read mark",
          presentation: "quiet",
          request: issueStateRequest("CORE-1", { last_read_seq: 2 }),
          target: "read:issue:CORE-1",
        })
        .catch(() => undefined);
      await settle();
    });
    expect(writes.rows().map((write) => [write.target, write.status])).toEqual([
      ["read:issue:CORE-1", "sending"],
    ]);

    race.listIssues.mockResolvedValue(listSinceOtherBrowser);
    await askAgain(race.queryClient, ["issues"]);
    await askAgain(race.queryClient, ["inbox"]);

    await waitFor(() => expect(race.listIssues).toHaveBeenCalledTimes(2));
    expect(race.getInbox).toHaveBeenCalledTimes(2);
    await waitFor(() => expect(priorityIn(board, "CORE-2").value).toBe("3"));
    expect(priorityIn(board, "CORE-1").value).toBe("1");

    await act(async () => {
      mark.resolve({ dismissed: [], last_read_seq: 2, pinned: false, seq: 2 });
      await settle();
    });
  } finally {
    race.cleanup();
    putIssueState.mockRestore();
  }
});

test("a write the reader stopped keeps showing its change until the attempt already out answers", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const answer = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches().mockImplementation(() => answer.promise);
  const race = renderRace();
  try {
    const board = await raceReady();
    await user.selectOptions(priorityIn(board, "CORE-1"), "0");
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(1));
    await tick(SEND_DEADLINE_MS + 1_000);
    const writes = writesOf(race.queryClient);
    const late = writes.rows()[0];
    // Stop trying ends the retries, not the attempt in flight: that one may still land, so the
    // write is live and its pick is shown again.
    act(() => {
      expect(writes.stop(late?.id ?? 0)).toBe(true);
    });
    expect(writes.rows().map((write) => write.status === "late" && write.stopping)).toEqual([true]);

    race.listIssues.mockResolvedValue(listSinceOtherBrowser);
    await askAgain(race.queryClient, ["issues"]);

    await waitFor(() => expect(race.listIssues).toHaveBeenCalledTimes(2));
    expect(priorityIn(board, "CORE-1").value).toBe("0");
    expect(priorityIn(board, "CORE-2").value).toBe("3");

    // The attempt answers: the write is made and shows nothing more.
    await act(async () => {
      answer.resolve({ ...patched, key: "CORE-1", priority: 0 });
      await settle();
    });
    await waitFor(() => expect(writes.rows()).toEqual([]));
    await askAgain(race.queryClient, ["issues"]);
    await waitFor(() => expect(priorityIn(board, "CORE-1").value).toBe("1"));
  } finally {
    realClock();
    race.cleanup();
    patchIssue.mockRestore();
  }
});

test("a write of another project's issue leaves this project's list alone and keeps its own Inbox row", async () => {
  const user = userEvent.setup();
  const answer = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches().mockImplementation(() => answer.promise);
  const race = renderRace();
  try {
    const board = await raceReady();
    await user.selectOptions(priorityIn(inboxRow("OTHER-1"), "OTHER-1"), "0");
    await waitFor(() =>
      expect(patchIssue).toHaveBeenCalledWith("OTHER-1", { priority: 0 }, keyedAttempt)
    );

    // CORE's list holds no row of OTHER-1, so this write changes nothing there: its answer is
    // the server's whole.
    race.listIssues.mockResolvedValue(listSinceOtherBrowser);
    await askAgain(race.queryClient, ["issues"]);
    await waitFor(() => expect(race.listIssues).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(priorityIn(board, "CORE-2").value).toBe("3"));
    expect(priorityIn(board, "CORE-1").value).toBe("1");

    // The Inbox lists every project's issues, so the pick is shown again over its answer.
    await askAgain(race.queryClient, ["inbox"]);
    await waitFor(() => expect(race.getInbox).toHaveBeenCalledTimes(2));
    expect(priorityIn(inboxRow("OTHER-1"), "OTHER-1").value).toBe("0");
    expect(priorityIn(inboxRow("CORE-1"), "CORE-1").value).toBe("1");
  } finally {
    race.cleanup();
    patchIssue.mockRestore();
  }
});

test("two live writes of one list show their changes again in the order the store holds them", async () => {
  const answer = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches().mockImplementation(() => answer.promise);
  const race = renderRace();
  try {
    const board = await raceReady();
    const writes = writesOf(race.queryClient);
    const pick = (priority: number, key: string) =>
      writes
        .run({
          detail: null,
          key,
          kind: plainWrite,
          label: `CORE-1 set to P${priority}`,
          presentation: "indicator",
          request: issuePatchRequest("CORE-1", { priority }),
          shows: (shown) => {
            shown.under<IssueSummary[]>(["issues", "project"], (rows) =>
              rows.map((row) => (row.key === "CORE-1" ? { ...row, priority } : row))
            );
          },
          target: "issue:CORE-1",
        })
        .catch(() => undefined);
    await act(async () => {
      void pick(0, "first-pick");
      void pick(2, "second-pick");
      await settle();
    });
    expect(priorityIn(board, "CORE-1").value).toBe("2");

    race.listIssues.mockResolvedValue(listSinceOtherBrowser);
    await askAgain(race.queryClient, ["issues"]);

    // Both are live, so both are shown again, in the order the store holds them: the last
    // pressed is the last seen.
    await waitFor(() => expect(race.listIssues).toHaveBeenCalledTimes(2));
    expect(priorityIn(board, "CORE-1").value).toBe("2");
    expect(priorityIn(board, "CORE-2").value).toBe("3");
  } finally {
    race.cleanup();
    patchIssue.mockRestore();
  }
});

test("a cache emptied before the answer lands, as a reload leaves it, carries the pending change too", async () => {
  const user = userEvent.setup();
  const answer = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches().mockImplementation(() => answer.promise);
  const race = renderRace();
  try {
    const board = await raceReady();
    await user.selectOptions(priorityIn(board, "CORE-1"), "0");
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(1));

    // The list's cache is emptied under the write, as a reload leaves it: there is no row there
    // to hold the pick, and the answer fills the cache for the first time.
    race.listIssues.mockResolvedValue(listSinceOtherBrowser);
    act(() => {
      race.queryClient.resetQueries({ queryKey: boardListKey });
    });
    await askAgain(race.queryClient, ["issues"]);

    await waitFor(() => expect(race.listIssues.mock.calls.length).toBeGreaterThan(1));
    await waitFor(() =>
      expect(
        (race.queryClient.getQueryData<IssueSummary[]>(boardListKey) ?? []).map((row) => [
          row.key,
          row.priority,
        ])
      ).toEqual([
        ["CORE-1", 0],
        ["CORE-2", 3],
      ])
    );
    expect(priorityIn(board, "CORE-1").value).toBe("0");
  } finally {
    race.cleanup();
    patchIssue.mockRestore();
  }
});

// A board move is the write's own change too: the placement rule run against the list as it
// stands, never a list computed once against the rows the drop read. So an answer that lands
// while the move is unanswered leaves the card where the person dropped it and brings every
// other card's move with it.
test("a list answer landing under an unanswered move keeps the card where it was dropped", async () => {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const answer = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches().mockImplementation(() => answer.promise);
  /** The server's board: Todo holds Bravo then Charlie, and Alpha waits in Triage. */
  const board: IssueSummary[] = [
    issue({ key: "CORE-1", rank: "a", status: "triage", title: "Alpha" }),
    issue({ key: "CORE-2", rank: "b", status: "todo", title: "Bravo" }),
    issue({ key: "CORE-3", rank: "c", status: "todo", title: "Charlie" }),
  ];
  const listIssues = spyOn(api, "listIssues").mockResolvedValue(board);
  const hook = renderMoves(queryClient);
  const todoKeys = () =>
    (queryClient.getQueryData<IssueSummary[]>(boardListKey) ?? [])
      .filter((row) => row.status === "todo")
      .map((row) => row.key);
  try {
    await act(async () => {
      await queryClient.fetchQuery(projectIssuesQuery("CORE", []));
    });
    let moved: Promise<void> = Promise.resolve();
    act(() => {
      moved = hook.result.current.moveCard("CORE-1", "todo", 1);
    });
    // Alpha was dropped as the column's second card, between Bravo and Charlie.
    expect(todoKeys()).toEqual(["CORE-2", "CORE-1", "CORE-3"]);
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(1));
    expect(patchIssue.mock.calls[0]).toEqual([
      "CORE-1",
      { rank: { after: "CORE-2", before: "CORE-3" }, status: "todo" },
      keyedAttempt,
    ]);

    // Another browser moves Charlie to the head of the column, and this page reads the list
    // again while its own move is still unanswered. The server lists a project's issues by
    // status then rank, which is the order each column is drawn in.
    listIssues.mockResolvedValue([
      issue({ key: "CORE-1", rank: "a", status: "triage", title: "Alpha" }),
      issue({ key: "CORE-3", rank: "a0", status: "todo", title: "Charlie" }),
      issue({ key: "CORE-2", rank: "b", status: "todo", title: "Bravo" }),
    ]);
    await act(async () => {
      await queryClient.refetchQueries({ queryKey: boardListKey });
      await settle();
    });

    // The answer landed whole - Charlie's move came with it - and the move showed its own
    // placement again over it: Alpha is still the column's second card, now after Charlie.
    expect(listIssues).toHaveBeenCalledTimes(2);
    expect(todoKeys()).toEqual(["CORE-3", "CORE-1", "CORE-2"]);

    // The server takes the move, and what it answers is the board's from then on.
    listIssues.mockResolvedValue([
      issue({ key: "CORE-3", rank: "a0", status: "todo", title: "Charlie" }),
      issue({ key: "CORE-1", rank: "ab", status: "todo", title: "Alpha" }),
      issue({ key: "CORE-2", rank: "b", status: "todo", title: "Bravo" }),
    ]);
    await act(async () => {
      answer.resolve({ ...patched, key: "CORE-1", status: "todo" });
      await moved;
    });
    expect(hook.result.current.error).toBeUndefined();
  } finally {
    hook.unmount();
    listIssues.mockRestore();
    patchIssue.mockRestore();
  }
});

test("a project list answer landing while an assignee write waits its turn keeps the person", async () => {
  const user = userEvent.setup();
  const first = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches().mockImplementation(() => first.promise);
  const race = renderRace();
  try {
    const board = await raceReady();
    const writes = writesOf(race.queryClient);
    // A write of CORE-1 that never answers, so the pick behind it waits its turn.
    await act(async () => {
      void writes
        .run({
          detail: null,
          kind: plainWrite,
          label: "CORE-1 set to done",
          presentation: "indicator",
          request: issuePatchRequest("CORE-1", { status: "done" }),
          target: "issue:CORE-1",
        })
        .catch(() => undefined);
      await settle();
    });
    await pickAssignee(user, "bob");
    await waitFor(() =>
      expect(writes.rows().map((write) => write.status)).toEqual(["sending", "queued"])
    );

    race.listIssues.mockResolvedValue(listSinceOtherBrowser);
    await askAgain(race.queryClient, ["issues"]);

    await waitFor(() => expect(race.listIssues).toHaveBeenCalledTimes(2));
    expect(assigneeSelect().value).toBe("bob");
    expect(priorityIn(board, "CORE-2").value).toBe("3");
  } finally {
    race.cleanup();
    patchIssue.mockRestore();
  }
});

test("a list answer landing while an assignee write is late and still being tried keeps the person", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const answer = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches().mockImplementation(() => answer.promise);
  const race = renderRace();
  try {
    const board = await raceReady();
    await pickAssignee(user, "bob");
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(1));
    await tick(SEND_DEADLINE_MS + 1_000);
    expect(
      writesOf(race.queryClient)
        .rows()
        .map((write) => write.status)
    ).toEqual(["late"]);

    race.listIssues.mockResolvedValue(listSinceOtherBrowser);
    await askAgain(race.queryClient, ["issues"]);

    await waitFor(() => expect(race.listIssues).toHaveBeenCalledTimes(2));
    expect(assigneeSelect().value).toBe("bob");
    expect(priorityIn(board, "CORE-2").value).toBe("3");
  } finally {
    realClock();
    race.cleanup();
    patchIssue.mockRestore();
  }
});

test("a project list answer landing while a repository write waits its turn keeps the repository", async () => {
  const user = userEvent.setup();
  const first = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches().mockImplementation(() => first.promise);
  const race = renderRace();
  try {
    const board = await raceReady();
    const writes = writesOf(race.queryClient);
    await act(async () => {
      void writes
        .run({
          detail: null,
          kind: plainWrite,
          label: "CORE-1 set to done",
          presentation: "indicator",
          request: issuePatchRequest("CORE-1", { status: "done" }),
          target: "issue:CORE-1",
        })
        .catch(() => undefined);
      await settle();
    });
    await user.selectOptions(repoSelect(), "acme/widgets");
    await waitFor(() =>
      expect(writes.rows().map((write) => write.status)).toEqual(["sending", "queued"])
    );

    race.listIssues.mockResolvedValue(listSinceOtherBrowser);
    await askAgain(race.queryClient, ["issues"]);

    await waitFor(() => expect(race.listIssues).toHaveBeenCalledTimes(2));
    expect(repoSelect().value).toBe("acme/widgets");
    expect(priorityIn(board, "CORE-2").value).toBe("3");
  } finally {
    race.cleanup();
    patchIssue.mockRestore();
  }
});

test("a list answer landing while a repository write is late and still being tried keeps it", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const answer = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches().mockImplementation(() => answer.promise);
  const race = renderRace();
  try {
    const board = await raceReady();
    await user.selectOptions(repoSelect(), "acme/widgets");
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(1));
    await tick(SEND_DEADLINE_MS + 1_000);
    expect(
      writesOf(race.queryClient)
        .rows()
        .map((write) => write.status)
    ).toEqual(["late"]);

    race.listIssues.mockResolvedValue(listSinceOtherBrowser);
    await askAgain(race.queryClient, ["issues"]);

    await waitFor(() => expect(race.listIssues).toHaveBeenCalledTimes(2));
    expect(repoSelect().value).toBe("acme/widgets");
    expect(priorityIn(board, "CORE-2").value).toBe("3");
  } finally {
    realClock();
    race.cleanup();
    patchIssue.mockRestore();
  }
});

test("an issue read answer landing while a priority write is out keeps the pick and brings the rest of the issue", async () => {
  const user = userEvent.setup();
  const answer = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches().mockImplementation(() => answer.promise);
  const race = renderRace();
  try {
    const board = await raceReady();
    expect(screen.getByTestId("detail-priority").textContent).toBe("1");
    await user.selectOptions(priorityIn(board, "CORE-1"), "0");
    await waitFor(() =>
      expect(patchIssue).toHaveBeenCalledWith("CORE-1", { priority: 0 }, keyedAttempt)
    );
    expect(screen.getByTestId("detail-priority").textContent).toBe("0");

    // The issue's own read answers while the PATCH is out: the server still holds P1, and the
    // title another person wrote comes with that answer.
    race.getIssue.mockResolvedValue({
      ...raceDetails,
      title: "Alpha, retitled",
      updated_at: "2026-09-14T00:00:00Z",
    });
    await askAgain(race.queryClient, ["issue", "CORE-1"]);

    await waitFor(() => expect(race.getIssue).toHaveBeenCalledTimes(2));
    expect(screen.getByTestId("detail-title").textContent).toBe("Alpha, retitled");
    expect(screen.getByTestId("detail-priority").textContent).toBe("0");
  } finally {
    race.cleanup();
    patchIssue.mockRestore();
  }
});

test("j/k/h/l rove real focus over cards, columns and rails", async () => {
  const user = userEvent.setup();
  const { cleanup } = renderBoard({}, undefined, keyboardIssues);
  try {
    const board = await screen.findByRole("region", { name: "Project board" });
    const alpha = within(board).getByRole("article", { name: "CORE-1 Alpha" });
    const bravo = within(board).getByRole("article", { name: "CORE-2 Bravo" });
    const delta = within(board).getByRole("article", { name: "CORE-4 Delta" });
    await user.keyboard("j");
    expect(alpha).toHaveFocus();
    await user.keyboard("j");
    expect(bravo).toHaveFocus();
    await user.keyboard("{ArrowUp}");
    expect(alpha).toHaveFocus();
    await user.keyboard("l");
    expect(delta).toHaveFocus();
    await user.keyboard("l");
    expect(within(board).getByRole("region", { name: "Testing" })).toHaveFocus();
    await user.keyboard("h");
    expect(delta).toHaveFocus();
    for (const _ of ["testing", "needs_review", "retro", "done"]) {
      await user.keyboard("l");
    }
    const doneRail = within(board).getByRole("region", { name: "Done (collapsed)" });
    expect(doneRail).toHaveFocus();
    await user.keyboard("l");
    expect(doneRail).toHaveFocus();
    await user.keyboard("{Escape}");
    expect(document.body).toHaveFocus();
    await user.keyboard("k");
    expect(delta).toHaveFocus();
  } finally {
    cleanup();
  }
});

test("Enter and o open the focused card once, p focuses its priority select, Escape returns to the card and then out", async () => {
  const user = userEvent.setup();
  const { cleanup } = renderBoard({}, undefined, keyboardIssues);
  try {
    const board = await screen.findByRole("region", { name: "Project board" });
    const alpha = within(board).getByRole("article", { name: "CORE-1 Alpha" });
    const link = within(alpha).getByRole("link");
    let clicks = 0;
    link.addEventListener("click", () => (clicks += 1));
    act(() => alpha.focus());

    await user.keyboard("p");
    const select = within(alpha).getByRole("combobox", { name: "Priority of CORE-1" });
    expect(select).toHaveFocus();
    // Single letters never fire on an editable target.
    await user.keyboard("j");
    expect(select).toHaveFocus();
    await user.keyboard("{Escape}");
    expect(alpha).toHaveFocus();
    await user.keyboard("{Escape}");
    expect(document.body).toHaveFocus();

    act(() => alpha.focus());
    await user.keyboard("{Enter}");
    expect(clicks).toBe(1);
    expect(screen.getByTestId("current-route").textContent).toBe("/issues/CORE-1");
  } finally {
    cleanup();
  }
});

test("Space on a focused card lifts nothing, and the card's instructions name the registry keys, not dnd-kit's grammar", async () => {
  const user = userEvent.setup();
  const patchIssue = answerIssuePatches().mockResolvedValue(patched);
  const { cleanup } = renderBoard({}, undefined, keyboardIssues);
  try {
    const board = await screen.findByRole("region", { name: "Project board" });
    const alpha = within(board).getByRole("article", { name: "CORE-1 Alpha" });
    act(() => alpha.focus());
    await user.keyboard("[Space]");
    await user.keyboard("{ArrowDown}");
    await user.keyboard("[Space]");
    await act(async () => {});
    expect(patchIssue).not.toHaveBeenCalled();
    expect(board.querySelector(".opacity-50")).toBeNull();
    const describedBy = alpha.getAttribute("aria-describedby");
    expect(describedBy).not.toBeNull();
    const instructions = document.getElementById(describedBy as string)?.textContent ?? "";
    expect(instructions).toContain("Shift with J, K, H or L");
    expect(instructions).not.toContain("space bar");
  } finally {
    cleanup();
    patchIssue.mockRestore();
  }
});

// The click the browser fires after a drop is the drag's, however late the main thread delivers
// it: a busy tab can take far longer than any window of milliseconds between the drop and the
// click, and a drop that navigates to the issue page loses the board the reader was arranging.
/** Press a card and move far enough to lift it, then release it where it was picked up. */
async function dropInPlace(card: HTMLElement): Promise<void> {
  await act(async () => {
    // dnd-kit's pointer sensor is driven by this mouse press; user-event's pointer API sends a
    // fixed sequence that never lifts the card.
    fireEvent.mouseDown(card, { button: 0, clientX: 10, clientY: 10 });
  });
  await act(async () => {
    // The move that lifts the card: user-event sends no mousemove of its own.
    fireEvent.mouseMove(document, { clientX: 10, clientY: 40 });
  });
  expect(card.className).toContain("opacity-50");
  await act(async () => {
    // The release that drops it, the other half of a gesture user-event cannot drive.
    fireEvent.mouseUp(document, { clientX: 10, clientY: 40 });
  });
}

/** The card as the drop re-rendered it: the drag replaces the node it began on. */
async function droppedCard(): Promise<HTMLElement> {
  return within(await screen.findByRole("region", { name: "Project board" })).getByRole("article", {
    name: "CORE-1 Alpha",
  });
}

/** Wait until a click reaches React again: dnd-kit stops click propagation for 50 ms after a
 * drop, so a route is no evidence of what a click did until that listener is gone. The fake
 * clock the test installed moves past the 50 ms, so that timer has fired when this resolves. */
async function pastDndKitClickGuard(): Promise<void> {
  await tick(80);
}

test("the click that trails a drop never follows the card's link, however late it arrives", async () => {
  const patchIssue = answerIssuePatches().mockResolvedValue(patched);
  const { cleanup } = renderBoard({}, undefined, keyboardIssues);
  try {
    const board = await screen.findByRole("region", { name: "Project board" });
    await dropInPlace(within(board).getByRole("article", { name: "CORE-1 Alpha" }));
    // The click reaches the page a second later, as a stalled main thread delivers it. What
    // stops the browser following the anchor is the board cancelling the click
    // (`defaultPrevented`); a rendered route would say nothing, since dnd-kit's own
    // `stopPropagation` keeps this click from React for 50 ms after the drop either way.
    const link = within(await droppedCard()).getByRole("link");
    const now = spyOn(performance, "now").mockReturnValue(performance.now() + 1000);
    let trailingFollowed = true;
    try {
      await act(async () => {
        // The assertion reads fireEvent's boolean: whether the click was cancelled.
        trailingFollowed = fireEvent.click(link, { button: 0 });
      });
    } finally {
      now.mockRestore();
    }
    expect(trailingFollowed).toBe(false);
    expect(screen.getByTestId("current-route").textContent).toBe("/projects/CORE");
  } finally {
    cleanup();
    patchIssue.mockRestore();
  }
});

// Only the drag's own click is the drag's. A drop draws no click of its own on every surface -
// a touch drag makes no tap, and a cancelled drag releases outside the page - so the mark a
// drop leaves must not outlive the gesture that follows it, by pointer or by key.
test("a drop that draws no click leaves the next press on the board working", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const patchIssue = answerIssuePatches().mockResolvedValue(patched);
  const { cleanup } = renderBoard({}, undefined, keyboardIssues);
  try {
    const board = await screen.findByRole("region", { name: "Project board" });
    await dropInPlace(within(board).getByRole("article", { name: "CORE-1 Alpha" }));
    const card = await droppedCard();
    await pastDndKitClickGuard();
    act(() => card.focus());
    await user.keyboard("{Enter}");
    expect(screen.getByTestId("current-route").textContent).toBe("/issues/CORE-1");
  } finally {
    realClock();
    cleanup();
    patchIssue.mockRestore();
  }
});

// Escape cancels a drag, and that key drops the mark like any other press, so the cancel has to
// put it back: the button is still down, and its release draws the same trailing click a drop
// does (`onDragCancel` on the board's `DndContext`).
test("a drag cancelled with Escape keeps its own release off the card's link", async () => {
  const patchIssue = answerIssuePatches().mockResolvedValue(patched);
  const { cleanup } = renderBoard({}, undefined, keyboardIssues);
  try {
    const board = await screen.findByRole("region", { name: "Project board" });
    const alpha = within(board).getByRole("article", { name: "CORE-1 Alpha" });
    await act(async () => {
      // The press that starts a dnd-kit drag, as in `dropInPlace` above.
      fireEvent.mouseDown(alpha, { button: 0, clientX: 10, clientY: 10 });
    });
    await act(async () => {
      // The move that lifts the card: user-event sends no mousemove of its own.
      fireEvent.mouseMove(document, { clientX: 10, clientY: 40 });
    });
    await act(async () => {
      // dnd-kit's own cancel reads `code`, not `key` (`core.esm.js` `handleKeydown`), and this
      // key is one step of the mouse drag above, which user-event cannot drive.
      fireEvent.keyDown(document, { code: "Escape", key: "Escape" });
    });
    await act(async () => {
      // The release of that same drag, with the button still down.
      fireEvent.mouseUp(document, { clientX: 10, clientY: 40 });
    });
    // The cancel landed: the lifted card is no longer the one dnd-kit is dragging.
    expect(board.querySelector(".opacity-50")).toBeNull();
    let followed = true;
    await act(async () => {
      // The assertion reads fireEvent's boolean: whether the click was cancelled.
      followed = fireEvent.click(within(await droppedCard()).getByRole("link"), { button: 0 });
    });
    expect(followed).toBe(false);
    expect(screen.getByTestId("current-route").textContent).toBe("/projects/CORE");
  } finally {
    cleanup();
    patchIssue.mockRestore();
  }
});

// The press of a pointer drops the mark as a key press does. `fireEvent.mouseDown` emits no
// `pointerdown`, so the drag above cannot reach that listener; this row presses one itself.
test("a pointer press on a card after a drop that drew no click follows its link", async () => {
  jest.useFakeTimers();
  const patchIssue = answerIssuePatches().mockResolvedValue(patched);
  const { cleanup } = renderBoard({}, undefined, keyboardIssues);
  try {
    const board = await screen.findByRole("region", { name: "Project board" });
    await dropInPlace(within(board).getByRole("article", { name: "CORE-1 Alpha" }));
    const link = within(await droppedCard()).getByRole("link");
    await pastDndKitClickGuard();
    await act(async () => {
      // The press and the click it draws are one gesture in one task, which user-event's own
      // pointer sequence would not reproduce.
      fireEvent.pointerDown(link, { button: 0 });
      // Its trailing click, in the same task: user-event's click would re-send the press.
      fireEvent.click(link, { button: 0 });
    });
    expect(screen.getByTestId("current-route").textContent).toBe("/issues/CORE-1");
  } finally {
    realClock();
    cleanup();
    patchIssue.mockRestore();
  }
});

// The mark is the board's own: a link anywhere else on the page follows itself while one is
// pending, which is the whole point of the containment test on the click's target.
test("a drop's mark never cancels a click outside the board", async () => {
  const patchIssue = answerIssuePatches().mockResolvedValue(patched);
  const { cleanup } = renderBoard({}, undefined, keyboardIssues);
  const elsewhere = document.createElement("a");
  elsewhere.href = "/issues/CORE-2";
  document.body.append(elsewhere);
  try {
    const board = await screen.findByRole("region", { name: "Project board" });
    await dropInPlace(within(board).getByRole("article", { name: "CORE-1 Alpha" }));
    let followed = false;
    await act(async () => {
      // The assertion reads fireEvent's boolean: whether the click was cancelled.
      followed = fireEvent.click(elsewhere, { button: 0 });
    });
    expect(followed).toBe(true);
  } finally {
    elsewhere.remove();
    cleanup();
    patchIssue.mockRestore();
  }
});

// The strip's filters on the board: Todo holds Alpha, Bravo, Charlie; only Alpha and Charlie
// contain an "h", so `?q=h` hides Bravo (and In progress's Delta).
test("the strip's search hides board cards, counts the visible column, and drags name visible neighbours", async () => {
  const user = userEvent.setup();
  const patchIssue = patchInFlight();
  const { cleanup } = renderBoard({}, undefined, keyboardIssues, "/projects/CORE?q=h");
  try {
    const board = await screen.findByRole("region", { name: "Project board" });
    const todo = within(board).getByRole("region", { name: "Todo" });
    await waitFor(() =>
      expect(
        within(todo)
          .getAllByRole("article")
          .map((node) => node.getAttribute("aria-label"))
      ).toEqual(["CORE-1 Alpha", "CORE-3 Charlie"])
    );
    expect(within(board).queryByRole("article", { name: "CORE-2 Bravo" })).not.toBeInTheDocument();
    // The column header counts what is on screen, not the whole column.
    expect(within(todo).getByTestId("board-column-header")).toHaveTextContent("2");
    const inProgress = within(board).getByRole("region", { name: "In progress" });
    expect(within(inProgress).queryAllByRole("article")).toHaveLength(0);
    expect(within(inProgress).getByTestId("board-column-header")).toHaveTextContent("0");

    // A keyboard move computes against the visible column: Alpha lands after Charlie, the
    // hidden Bravo is not a neighbour, and the announcement counts 2 visible cards.
    const alpha = within(board).getByRole("article", { name: "CORE-1 Alpha" });
    act(() => alpha.focus());
    await user.keyboard("{Shift>}J{/Shift}");
    expect(patchIssue).toHaveBeenCalledWith("CORE-1", { rank: { after: "CORE-3" } }, keyedAttempt);
    expect(within(board).getByRole("status", { name: "Board announcements" }).textContent).toBe(
      "CORE-1 → Todo, position 2 of 2"
    );
    await waitFor(() =>
      expect(
        within(todo)
          .getAllByRole("article")
          .map((node) => node.getAttribute("aria-label"))
      ).toEqual(["CORE-3 Charlie", "CORE-1 Alpha"])
    );
    expect(within(board).queryByRole("article", { name: "CORE-2 Bravo" })).not.toBeInTheDocument();
  } finally {
    cleanup();
    patchIssue.mockRestore();
  }
});

test("Needs you and Unread from the URL keep only matching cards on the board", async () => {
  const { cleanup } = renderBoard(
    {
      "CORE-1": { dismissed: [], last_read_seq: 1, pinned: false, seq: 0 },
      "CORE-2": { dismissed: [], last_read_seq: 1, pinned: false, seq: 0 },
      "CORE-3": { dismissed: [], last_read_seq: 1, pinned: false, seq: 0 },
    },
    undefined,
    [
      issue({ key: "CORE-1", last_seq: 4, open_asks: 1, status: "todo", title: "Wanted" }),
      issue({ key: "CORE-2", last_seq: 4, open_asks: 0, status: "todo", title: "No asks" }),
      issue({ key: "CORE-3", last_seq: 1, open_asks: 2, status: "todo", title: "Already read" }),
    ],
    "/projects/CORE?needs-you=1&unread=1"
  );
  try {
    const board = await screen.findByRole("region", { name: "Project board" });
    const todo = within(board).getByRole("region", { name: "Todo" });
    await waitFor(() =>
      expect(
        within(todo)
          .getAllByRole("article")
          .map((node) => node.getAttribute("aria-label"))
      ).toEqual(["CORE-1 Wanted"])
    );
    expect(within(todo).getByTestId("board-column-header")).toHaveTextContent("1");
  } finally {
    cleanup();
  }
});

test("a labelled move writes the rendered list, names its neighbours, and refetches both lists", async () => {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const labels = ["keep"] as const;
  const labelledKey = ["issues", "project", "CORE", "labels", labels] as const;
  const baseKey = ["issues", "project", "CORE"] as const;
  const alpha = issue({ key: "CORE-1", labels: ["keep"], rank: "a", status: "todo" });
  const bravo = issue({ key: "CORE-2", rank: "b", status: "todo" });
  const charlie = issue({ key: "CORE-3", labels: ["keep"], rank: "c", status: "todo" });
  const delta = issue({ key: "CORE-4", labels: ["keep"], rank: "d", status: "todo" });
  // The server answers the labelled query without the unlabelled card.
  queryClient.setQueryData(labelledKey, [alpha, charlie, delta]);
  queryClient.setQueryData(baseKey, [alpha, bravo, charlie, delta]);
  const patchIssue = answerIssuePatches().mockResolvedValue(patched);
  const hook = renderMoves(queryClient, labels);
  try {
    await act(() => hook.result.current.moveCard("CORE-4", "todo", 1));
    expect(patchIssue).toHaveBeenCalledWith(
      "CORE-4",
      {
        rank: { after: "CORE-1", before: "CORE-3" },
      },
      keyedAttempt
    );
    expect(
      queryClient.getQueryData<IssueSummary[]>(labelledKey)?.map((entry) => entry.key)
    ).toEqual(["CORE-1", "CORE-4", "CORE-3"]);
    // The unfiltered list's ranks changed too: both queries refetch.
    expect(queryClient.getQueryState(labelledKey)?.isInvalidated).toBe(true);
    expect(queryClient.getQueryState(baseKey)?.isInvalidated).toBe(true);
  } finally {
    hook.unmount();
    patchIssue.mockRestore();
  }
});
