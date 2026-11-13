import { expect, test } from "bun:test";
import { QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
// biome-ignore lint/style/noRestrictedImports: this file keeps fireEvent for the bare mouse, pointer and touch presses a draggable parent must not see, which user-event has no form for
import { act, fireEvent, render, renderHook, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { answerIssuePatches } from "../../__tests__/send-answers";
import type { InboxRow, Issue, IssueDetails, IssueSummary } from "../../api/types";
import { issuePriority } from "./issue-priority";
import { PriorityControl } from "./PriorityControl";
import { useIssueAssignee } from "./useIssueAssignee";
import { useIssuePriority } from "./useIssuePriority";

const issue: Issue = {
  closed_at: null,
  created_at: "2026-09-09T00:00:00Z",
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
  priority: 1,
  project: "CORE",
  rank: "U",
  route: null,
  repo: null,
  resolved_repo: null,
  status: "todo",
  title: "Review the spec",
  updated_at: "2026-09-09T00:00:00Z",
};
const details: IssueDetails = {
  route_status: null,
  route_holder: null,
  route_status_reason: null,
  holder_state: null,
  last_seen: null,
  takeable_at: null,
  ...issue,
  artifacts: [],
  children: [],
  design_gates: [],
  open_asks: [],
  progress: { tasks: null, children: null },
  referenced_by_count: 0,
};
const summary: IssueSummary = {
  route: null,
  repo: null,
  resolved_repo: null,
  route_status: null,
  route_holder: null,
  route_status_reason: null,
  key: issue.key,
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
  priority: issue.priority,
  rank: "U",
  status: issue.status,
  title: issue.title,
  updated_at: issue.updated_at,
  closed_at: issue.closed_at,
  created_by: issue.created_by,
  created_at: issue.created_at,
};
const otherSummary: IssueSummary = { ...summary, key: "CORE-2", priority: 3 };
const inboxRow: InboxRow = {
  anchor: null,
  answer: null,
  artifact_id: null,
  asked_of: null,
  asked_of_at: null,
  asked_of_by: null,
  author: { id: "session-1", kind: "session" },
  block_id: null,
  last_reply: null,
  created_at: "2026-09-09T00:00:00Z",
  edited_at: null,
  id: "ask-1",
  issue_key: issue.key,
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
  priority: issue.priority,
  snoozed_until: null,
  question: "Which layout?",
  state: "open",
  urgency: "med",
};

const detailKey = ["issue", issue.key] as const;
const listKey = ["issues", "project", issue.project] as const;
const inboxKey = ["inbox"] as const;

/** Renders the control the way every surface does: with the priority it reads from the cache. */
function CachedControl(): ReactNode {
  const cached = useQuery<IssueDetails>({
    queryKey: detailKey,
    queryFn: () => Promise.reject(new Error("the test seeds this query")),
    staleTime: Number.POSITIVE_INFINITY,
  });
  if (cached.data === undefined) {
    return null;
  }
  return (
    <PriorityControl issueKey={cached.data.key} priority={issuePriority(cached.data.priority)} />
  );
}

function renderControl() {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  queryClient.setQueryData(detailKey, details);
  queryClient.setQueryData(listKey, [summary, otherSummary]);
  queryClient.setQueryData(inboxKey, [inboxRow]);
  const user = userEvent.setup();
  const view = render(
    <QueryClientProvider client={queryClient}>
      <CachedControl />
    </QueryClientProvider>
  );
  return { queryClient, unmount: view.unmount, user };
}

function select(): HTMLSelectElement {
  return screen.getByRole("combobox", { name: "Priority of CORE-1" }) as HTMLSelectElement;
}

test("PriorityControl shows the issue's priority and offers Unset and P0-P3", () => {
  const { unmount } = renderControl();
  try {
    expect(screen.getByText("P1", { selector: "span" })).toBeTruthy();
    expect(select().value).toBe("1");
    expect([...select().options].map((option) => option.value)).toEqual(["", "0", "1", "2", "3"]);
  } finally {
    unmount();
  }
});

test("PriorityControl saves the picked priority, shows it before the server answers, and keeps focus", async () => {
  const save = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches().mockImplementation(() => save.promise);
  const { queryClient, unmount, user } = renderControl();
  try {
    select().focus();
    await user.selectOptions(select(), "2");
    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { priority: 2 }, keyedAttempt)
    );
    expect(select().value).toBe("2");
    expect(screen.getByText("P2", { selector: "span" })).toBeTruthy();
    // A disabled control drops focus and leaves the tab order for the length of the save, so
    // the picker stays enabled while the request is in flight.
    expect(select().disabled).toBe(false);
    expect(select()).toHaveFocus();
    expect(queryClient.getQueryData<IssueSummary[]>(listKey)).toEqual([
      { ...summary, priority: 2 },
      otherSummary,
    ]);
    expect(queryClient.getQueryData<InboxRow[]>(inboxKey)).toEqual([{ ...inboxRow, priority: 2 }]);

    await act(async () => {
      save.resolve({ ...issue, priority: 2 });
      await save.promise;
    });
    await waitFor(() =>
      expect(queryClient.getQueryData<IssueDetails>(detailKey)?.priority).toBe(2)
    );
    expect(select()).toHaveFocus();
  } finally {
    unmount();
    patchIssue.mockRestore();
  }
});

test("PriorityControl sends null for Unset and shows the muted tag", async () => {
  const patchIssue = answerIssuePatches().mockResolvedValue({ ...issue, priority: null });
  const { unmount, user } = renderControl();
  try {
    await user.selectOptions(select(), "");
    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { priority: null }, keyedAttempt)
    );
    expect(select().value).toBe("");
    expect(screen.getByText("Priority", { selector: "span" })).toBeTruthy();
    expect(screen.queryByText("P1", { selector: "span" })).not.toBeInTheDocument();
  } finally {
    unmount();
    patchIssue.mockRestore();
  }
});

test("PriorityControl rolls every surface back and offers Retry when the save fails", async () => {
  const save = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches().mockImplementation(() => save.promise);
  const { queryClient, unmount, user } = renderControl();
  try {
    await user.selectOptions(select(), "0");
    await waitFor(() => expect(select().value).toBe("0"));

    act(() => {
      save.reject(new Error("offline"));
    });
    await screen.findByRole("alert");
    expect(select().value).toBe("1");
    expect(select().disabled).toBe(false);
    expect(queryClient.getQueryData<IssueSummary[]>(listKey)).toEqual([summary, otherSummary]);
    expect(queryClient.getQueryData<InboxRow[]>(inboxKey)).toEqual([inboxRow]);

    patchIssue.mockResolvedValue({ ...issue, priority: 0 });
    await user.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(2));
    expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { priority: 0 }, keyedAttempt);
    await waitFor(() => expect(screen.queryByRole("alert")).not.toBeInTheDocument());
    expect(select().value).toBe("0");
  } finally {
    unmount();
    patchIssue.mockRestore();
  }
});

test("PriorityControl saves a pick made during the first save once that save settles, newest first shown", async () => {
  const first = Promise.withResolvers<Issue>();
  const second = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches()
    .mockImplementationOnce(() => first.promise)
    .mockImplementationOnce(() => second.promise);
  const { queryClient, unmount, user } = renderControl();
  try {
    await user.selectOptions(select(), "0");
    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { priority: 0 }, keyedAttempt)
    );
    await user.selectOptions(select(), "3");
    await user.selectOptions(select(), "2");
    // One request at a time: the later picks wait for the first answer, but the badge already
    // shows the newest one and never snaps back to the value still being saved.
    expect(select().value).toBe("2");
    expect(screen.getByText("P2", { selector: "span" })).toBeTruthy();
    expect(patchIssue).toHaveBeenCalledTimes(1);

    await act(async () => {
      first.resolve({ ...issue, priority: 0 });
      await first.promise;
    });
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(2));
    expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { priority: 2 }, keyedAttempt);
    expect(select().value).toBe("2");

    await act(async () => {
      second.resolve({ ...issue, priority: 2 });
      await second.promise;
    });
    await waitFor(() =>
      expect(queryClient.getQueryData<IssueDetails>(detailKey)?.priority).toBe(2)
    );
    expect(patchIssue).toHaveBeenCalledTimes(2);
  } finally {
    unmount();
    patchIssue.mockRestore();
  }
});

test("a queued pick that fails leaves the priority the server last confirmed, not the pick", async () => {
  const first = Promise.withResolvers<Issue>();
  const second = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches()
    .mockImplementationOnce(() => first.promise)
    .mockImplementationOnce(() => second.promise);
  const { queryClient, unmount, user } = renderControl();
  try {
    await user.selectOptions(select(), "0");
    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { priority: 0 }, keyedAttempt)
    );
    await user.selectOptions(select(), "2");
    expect(select().value).toBe("2");

    await act(async () => {
      first.resolve({ ...issue, priority: 0 });
      await first.promise;
    });
    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { priority: 2 }, keyedAttempt)
    );
    act(() => {
      second.reject(new Error("offline"));
    });
    await screen.findByRole("alert");
    expect(select().value).toBe("0");
    expect(queryClient.getQueryData<IssueSummary[]>(listKey)).toEqual([
      { ...summary, priority: 0 },
      otherSummary,
    ]);
    expect(queryClient.getQueryData<InboxRow[]>(inboxKey)).toEqual([{ ...inboxRow, priority: 0 }]);
  } finally {
    unmount();
    patchIssue.mockRestore();
  }
});

// R6 of dispatch://INFRA-299. A pick replaces a pick of the same field that is still waiting its
// turn: the two picks that have not gone out are one request, the newest, and the engine shows
// the person nothing for the one it took out - no error, no Retry, and no roll-back of the
// control or the lists, since the engine, not the server, is what dropped it.
test("a pick replaces a pick of the same priority control still waiting its turn, quietly", async () => {
  const first = Promise.withResolvers<Issue>();
  const second = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches()
    .mockImplementationOnce(() => first.promise)
    .mockImplementationOnce(() => second.promise);
  const { queryClient, unmount, user } = renderControl();
  try {
    await user.selectOptions(select(), "0");
    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { priority: 0 }, keyedAttempt)
    );
    await user.selectOptions(select(), "3");
    await user.selectOptions(select(), "2");

    // P3 never went out and never will: the person sees their newest pick and nothing of the
    // one it replaced.
    expect(select().value).toBe("2");
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Retry" })).not.toBeInTheDocument();
    expect(queryClient.getQueryData<IssueSummary[]>(listKey)).toEqual([
      { ...summary, priority: 2 },
      otherSummary,
    ]);
    expect(queryClient.getQueryData<InboxRow[]>(inboxKey)).toEqual([{ ...inboxRow, priority: 2 }]);

    await act(async () => {
      first.resolve({ ...issue, priority: 0 });
      await first.promise;
    });
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(2));
    // Two picks that had not gone out, one request: the newest.
    expect(patchIssue.mock.calls.map(([, update]) => update.priority)).toEqual([0, 2]);
    expect(select().value).toBe("2");
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Retry" })).not.toBeInTheDocument();
  } finally {
    await act(async () => {
      second.resolve({ ...issue, priority: 2 });
      await second.promise;
    });
    unmount();
    patchIssue.mockRestore();
  }
});

// R6 of dispatch://INFRA-299. The server refuses an older pick while a newer one stands in the
// line: the screen keeps the newer value, the control reports the refusal, and no Retry is
// offered, because a Retry joins the back of the line and would store the older pick last.
test("a refused pick is reported with no Retry while a newer pick stands in the line", async () => {
  const first = Promise.withResolvers<Issue>();
  const second = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches()
    .mockImplementationOnce(() => first.promise)
    .mockImplementationOnce(() => second.promise);
  const { queryClient, unmount, user } = renderControl();
  try {
    await user.selectOptions(select(), "0");
    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { priority: 0 }, keyedAttempt)
    );
    await user.selectOptions(select(), "2");
    expect(select().value).toBe("2");

    act(() => {
      first.reject(new Error("offline"));
    });
    expect(await screen.findByRole("alert")).toHaveTextContent(
      "Could not update the priority of CORE-1."
    );
    expect(screen.queryByRole("button", { name: "Retry" })).not.toBeInTheDocument();
    expect(select().value).toBe("2");
    expect(queryClient.getQueryData<IssueSummary[]>(listKey)).toEqual([
      { ...summary, priority: 2 },
      otherSummary,
    ]);
  } finally {
    await act(async () => {
      second.resolve({ ...issue, priority: 2 });
      await second.promise;
    });
    unmount();
    patchIssue.mockRestore();
  }
});

// R10 of dispatch://INFRA-299. A refused save stops being drawn, so the screen shows the value
// the server last confirmed, which is not the pick before it when the server refused that one
// too: the first refusal changes nothing on screen, because a newer pick stands in the line and
// is drawn over it, so the second refusal is what puts the screen right, and the screen it
// leaves reaches past the pick the server never took.
test("a refused pick after another refused pick leaves the server's value, not the first pick", async () => {
  const first = Promise.withResolvers<Issue>();
  const second = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches()
    .mockImplementationOnce(() => first.promise)
    .mockImplementationOnce(() => second.promise);
  const { queryClient, unmount, user } = renderControl();
  try {
    await user.selectOptions(select(), "0");
    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { priority: 0 }, keyedAttempt)
    );
    await user.selectOptions(select(), "2");
    expect(select().value).toBe("2");

    act(() => {
      first.reject(new Error("offline"));
    });
    await screen.findByRole("alert");
    expect(select().value).toBe("2");

    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { priority: 2 }, keyedAttempt)
    );
    act(() => {
      second.reject(new Error("offline"));
    });
    await waitFor(() => expect(select().value).toBe(String(issue.priority)));
    expect(queryClient.getQueryData<IssueSummary[]>(listKey)).toEqual([summary, otherSummary]);
    expect(queryClient.getQueryData<InboxRow[]>(inboxKey)).toEqual([inboxRow]);
  } finally {
    unmount();
    patchIssue.mockRestore();
  }
});

// R10 of dispatch://INFRA-299. A pick that replaces one still waiting its turn, once refused,
// leaves the screen on what it held before the pick it replaced was shown: that pick was never
// sent, so the server never had it, and the screen must not come back to it.
test("a refused pick that replaced a waiting pick leaves the screen past the pick it replaced", async () => {
  const first = Promise.withResolvers<Issue>();
  const second = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches()
    .mockImplementationOnce(() => first.promise)
    .mockImplementationOnce(() => second.promise);
  const { queryClient, unmount, user } = renderControl();
  try {
    await user.selectOptions(select(), "0");
    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { priority: 0 }, keyedAttempt)
    );
    await user.selectOptions(select(), "3");
    await user.selectOptions(select(), "2");
    expect(select().value).toBe("2");

    await act(async () => {
      first.resolve({ ...issue, priority: 0 });
      await first.promise;
    });
    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { priority: 2 }, keyedAttempt)
    );
    act(() => {
      second.reject(new Error("offline"));
    });
    await screen.findByRole("alert");
    // The server holds P0, the pick it took; P3 never went out.
    expect(select().value).toBe("0");
    expect(queryClient.getQueryData<IssueSummary[]>(listKey)).toEqual([
      { ...summary, priority: 0 },
      otherSummary,
    ]);
    expect(queryClient.getQueryData<InboxRow[]>(inboxKey)).toEqual([{ ...inboxRow, priority: 0 }]);
    expect(queryClient.getQueryData<IssueDetails>(detailKey)?.priority).toBe(0);
  } finally {
    unmount();
    patchIssue.mockRestore();
  }
});

// R7 of dispatch://INFRA-299. One issue's writes go out one at a time, so the answer of an
// earlier save of another field always predates a pick waiting behind it: the engine merges
// that answer into the caches, and the store shows the waiting pick again over the merge, so
// the person's pick stays on screen while it is out.
test("a pick waiting behind another field's save stays shown over that save's merged answer", async () => {
  const assigned = Promise.withResolvers<Issue>();
  const prioritised = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches()
    .mockImplementationOnce(() => assigned.promise)
    .mockImplementationOnce(() => prioritised.promise);
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  queryClient.setQueryData(detailKey, details);
  queryClient.setQueryData(listKey, [summary, otherSummary]);
  queryClient.setQueryData(inboxKey, [inboxRow]);
  const { result, unmount } = renderHook(
    () => ({ assignee: useIssueAssignee(issue.key), priority: useIssuePriority(issue.key) }),
    {
      wrapper: ({ children }: { children: ReactNode }) => (
        <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
      ),
    }
  );
  try {
    act(() => result.current.assignee.submit("bob"));
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(1));
    act(() => result.current.priority.submit(2));
    expect(queryClient.getQueryData<IssueDetails>(detailKey)?.priority).toBe(2);

    await act(async () => {
      assigned.resolve({ ...issue, assignee: "bob" });
      await assigned.promise;
    });
    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { priority: 2 }, keyedAttempt)
    );
    // The assignee's answer is merged, and the priority pick, still out, is shown over it.
    expect(queryClient.getQueryData<IssueDetails>(detailKey)).toMatchObject({
      assignee: "bob",
      priority: 2,
    });
  } finally {
    await act(async () => {
      prioritised.resolve({ ...issue, assignee: "bob", priority: 2 });
      await prioritised.promise;
    });
    unmount();
    patchIssue.mockRestore();
  }
});

test("PriorityControl keeps mouse, pointer and touch presses on the select out of a draggable parent", () => {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const reached: string[] = [];
  const view = render(
    <QueryClientProvider client={queryClient}>
      <article
        aria-label="CORE-1 Review the spec"
        onMouseDown={() => reached.push("mousedown")}
        onPointerDown={() => reached.push("pointerdown")}
        onTouchStart={() => reached.push("touchstart")}
      >
        <PriorityControl issueKey="CORE-1" priority={1} />
      </article>
    </QueryClientProvider>
  );
  try {
    // Bare mouse, pointer and touch presses: user-event sends a whole press sequence and cannot
    // deliver one of these events on its own.
    fireEvent.mouseDown(select());
    fireEvent.pointerDown(select());
    fireEvent.touchStart(select());
    expect(reached).toEqual([]);
  } finally {
    view.unmount();
  }
});
