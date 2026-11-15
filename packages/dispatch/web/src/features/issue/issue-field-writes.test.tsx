import { expect, mock, spyOn, test } from "bun:test";
import { QueryClientProvider, useQuery } from "@tanstack/react-query";
import {
  act,
  type RenderResult,
  render,
  renderHook,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";

import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { answerIssuePatches } from "../../__tests__/send-answers";
import { ApiError, api, issuePatchRequest } from "../../api/client";
import { mergeIssue } from "../../api/issue-cache";
import { issueQuery, projectIssuesQuery } from "../../api/queries";
import { DispatchQueryClient } from "../../api/query-client";
import type {
  InboxRow,
  Issue,
  IssueDetails,
  IssuePriority,
  IssueSummary,
  RepoProject,
  UpdateIssueInput,
} from "../../api/types";
import { useBoardMoves } from "../project/board-moves";
import { PendingWritesIndicator } from "../writes/PendingWritesIndicator";
import { KEPT_SEND_RESUME_MS, resetWriteStorage } from "../writes/write-storage";
import { plainWrite, writesOf } from "../writes/write-store";
import { AssigneeControl } from "./AssigneeControl";
import { issuePriority } from "./issue-priority";
import { PriorityControl } from "./PriorityControl";
import { RepoControl } from "./RepoControl";
import { useIssueFieldWrite } from "./useIssueFieldWrite";

// The rows of dispatch://INFRA-299: every pick of a priority, an assignee or a repository joins
// the page's write store as it is made, on its issue's line, and the pick's write - not the
// picker - keeps it on screen over every answer until Dispatch has stored it.

const issue: Issue = {
  advances: [],
  advances_revision: 0,
  assignee: "alice",
  blocked_by: [],
  claim: null,
  closed_at: null,
  components: { mode: "inherit", ids: [], unknown: [], reason: null, inherited_from: null },
  created_at: "2026-09-09T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  decisions_artifact_id: "",
  external_links: [],
  key: "CORE-1",
  labels: [],
  last_seq: 1,
  number: 1,
  parent: null,
  plan_artifact_id: "",
  primary_artifact_id: "artifact-1",
  priority: 1,
  project: "CORE",
  rank: "U",
  repo: null,
  resolved_repo: null,
  route: null,
  status: "todo",
  title: "Review the spec",
  updated_at: "2026-09-09T00:00:00Z",
  verification_artifact_id: "",
};
const details: IssueDetails = {
  holder_state: null,
  last_seen: null,
  route_holder: null,
  route_status: null,
  route_status_reason: null,
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
  advances: [],
  advances_revision: 0,
  assignee: issue.assignee,
  blocked_by: [],
  claim: null,
  closed_at: issue.closed_at,
  components: { mode: "inherit", ids: [], unknown: [], reason: null, inherited_from: null },
  created_at: issue.created_at,
  created_by: issue.created_by,
  key: issue.key,
  labels: [],
  last_seq: 1,
  open_asks: 0,
  parent: null,
  primary_artifact_id: "artifact-1",
  priority: issue.priority,
  progress: { tasks: null, children: null },
  rank: "U",
  repo: null,
  resolved_repo: null,
  route: null,
  route_holder: null,
  route_status: null,
  route_status_reason: null,
  status: issue.status,
  title: issue.title,
  updated_at: issue.updated_at,
};
const inboxRow: InboxRow = {
  anchor: null,
  answer: null,
  artifact_id: null,
  asked_of: null,
  asked_of_at: null,
  asked_of_by: null,
  author: { id: "session-1", kind: "session" },
  block_id: null,
  created_at: "2026-09-09T00:00:00Z",
  edited_at: null,
  id: "ask-1",
  issue: { assignee: issue.assignee, key: issue.key, title: issue.title },
  issue_key: issue.key,
  kind: "question",
  last_reply: null,
  multiple: false,
  opened_event_id: 1,
  options: [],
  priority: issue.priority,
  question: "Which layout?",
  snoozed_until: null,
  state: "open",
  thread: {
    answers: [],
    claimant: null,
    edits: [],
    followers: [],
    replies: [],
    route: null,
    route_holder: null,
    route_status: null,
    route_status_reason: null,
  },
  urgency: "med",
  waiting_on_person: issue.assignee ?? undefined,
};

const detailKey = issueQuery(issue.key).queryKey;
const listKey = ["issues", "project", issue.project] as const;
const inboxKey = ["inbox"] as const;

/** Dispatch's side of the three reads a pick shows on: each read is composed as it reaches
 *  Dispatch, from the priority Dispatch stores then, and answers once the row lets the reads it
 *  holds go (`hold`). */
interface Reads {
  readonly detail: () => Promise<IssueDetails>;
  readonly inbox: () => Promise<InboxRow[]>;
  readonly list: () => Promise<IssueSummary[]>;
  /** Holds every read that starts from now on until the returned function lets them answer. */
  readonly hold: () => () => void;
  /** What Dispatch stores from now on: a save landed. */
  readonly store: (priority: IssuePriority | null) => void;
}

function dispatchReads(): Reads {
  let stored = issuePriority(issue.priority);
  let gate: Promise<void> | undefined;
  const read =
    <T,>(compose: (priority: IssuePriority | null) => T) =>
    async (): Promise<T> => {
      const answer = compose(stored);
      await gate;
      return answer;
    };
  return {
    detail: read((priority): IssueDetails => ({ ...details, priority })),
    inbox: read((priority): InboxRow[] => [{ ...inboxRow, priority }]),
    list: read((priority): IssueSummary[] => [{ ...summary, priority }]),
    hold: () => {
      const held = Promise.withResolvers<void>();
      gate = held.promise;
      return () => held.resolve();
    },
    store: (priority) => {
      stored = priority;
    },
  };
}

/** The page's query client, signed in, holding CORE-1 as Dispatch last answered it on its detail,
 *  its project's list and the Inbox. */
function signedIn(): DispatchQueryClient {
  const client = new DispatchQueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  client.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  client.setQueryData(detailKey, details);
  client.setQueryData(listKey, [summary]);
  client.setQueryData(inboxKey, [inboxRow]);
  return client;
}

/** A priority picker, as every surface renders one: with the priority it reads from the issue. */
function CachedPriority({ reads }: { reads: Reads }): ReactNode {
  const cached = useQuery({
    ...issueQuery(issue.key),
    queryFn: reads.detail,
    staleTime: Number.POSITIVE_INFINITY,
  });
  if (cached.data === undefined) return null;
  return (
    <PriorityControl issueKey={cached.data.key} priority={issuePriority(cached.data.priority)} />
  );
}

/** The three surfaces a priority shows on beside its picker: the issue, a project list's row and
 *  an Inbox row, each reading its own query. */
function Surfaces({ reads }: { reads: Reads }): ReactNode {
  const forever = { staleTime: Number.POSITIVE_INFINITY };
  const detail = useQuery({ ...issueQuery(issue.key), ...forever, queryFn: reads.detail });
  const list = useQuery({ ...forever, queryFn: reads.list, queryKey: listKey });
  const inbox = useQuery({ ...forever, queryFn: reads.inbox, queryKey: inboxKey });
  return (
    <ul>
      <li data-testid="issue-priority">P{detail.data?.priority}</li>
      <li data-testid="list-priority">P{list.data?.[0]?.priority}</li>
      <li data-testid="inbox-priority">P{inbox.data?.[0]?.priority}</li>
    </ul>
  );
}

/** Renders `children` in the app `client` is the query client of. */
function renderIn(client: DispatchQueryClient, children: ReactNode): RenderResult {
  return render(<QueryClientProvider client={client}>{children}</QueryClientProvider>);
}

/** The priority each cache holds for CORE-1 now. */
function cachedPriorities(client: DispatchQueryClient) {
  return {
    detail: client.getQueryData<IssueDetails>(detailKey)?.priority,
    inbox: client.getQueryData<InboxRow[]>(inboxKey)?.[0]?.priority,
    list: client.getQueryData<IssueSummary[]>(listKey)?.[0]?.priority,
  };
}

/** The priority each surface shows for CORE-1 now. */
function shownPriorities() {
  return {
    detail: screen.getByTestId("issue-priority").textContent,
    inbox: screen.getByTestId("inbox-priority").textContent,
    list: screen.getByTestId("list-priority").textContent,
  };
}

function prioritySelects(): HTMLSelectElement[] {
  return screen.getAllByRole("combobox", { name: "Priority of CORE-1" }) as HTMLSelectElement[];
}

/** A save Dispatch answers by storing the change: its answer is the issue with it. */
const stores = (_key: string, input: UpdateIssueInput): Promise<Issue> =>
  Promise.resolve({ ...issue, ...input } as Issue);

/** Every save the server received, by what it carried. */
function bodies(patchIssue: { mock: { calls: unknown[][] } }): unknown[] {
  return patchIssue.mock.calls.map((call) => call[1]);
}

test("each pick joins the issue's line in the write store when it is made", async () => {
  const patchIssue = answerIssuePatches().mockImplementation(
    () => Promise.withResolvers<Issue>().promise
  );
  const client = signedIn();
  const view = renderIn(client, <CachedPriority reads={dispatchReads()} />);
  const user = userEvent.setup();
  try {
    await user.selectOptions(prioritySelects()[0] as HTMLSelectElement, "0");
    await user.selectOptions(prioritySelects()[0] as HTMLSelectElement, "3");
    // The second pick is the store's from its press, behind the first on `issue:CORE-1`, so it
    // is kept for a reload and goes out after the first whatever becomes of the picker.
    expect(
      writesOf(client)
        .rows()
        .map((row) => [row.label, row.target, row.status])
    ).toEqual([
      ["CORE-1 set to P0", "issue:CORE-1", "sending"],
      ["CORE-1 set to P3", "issue:CORE-1", "queued"],
    ]);
    expect(bodies(patchIssue)).toEqual([{ priority: 0 }]);
  } finally {
    view.unmount();
    patchIssue.mockRestore();
  }
});

test("a pick behind a waiting pick of the same field takes it out: one request carries the newest, and no error shows", async () => {
  const first = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches()
    .mockImplementationOnce(() => first.promise)
    .mockImplementation(stores);
  const client = signedIn();
  const reads = dispatchReads();
  const view = renderIn(client, <CachedPriority reads={reads} />);
  const user = userEvent.setup();
  try {
    const select = () => prioritySelects()[0] as HTMLSelectElement;
    await user.selectOptions(select(), "0");
    await user.selectOptions(select(), "3");
    await user.selectOptions(select(), "2");
    expect(
      writesOf(client)
        .rows()
        .map((row) => [row.label, row.status])
    ).toEqual([
      ["CORE-1 set to P0", "sending"],
      ["CORE-1 set to P2", "queued"],
    ]);
    // The pick taken out puts nothing back: every cache shows the newest.
    expect(select().value).toBe("2");
    expect(cachedPriorities(client)).toEqual({ detail: 2, inbox: 2, list: 2 });
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();

    reads.store(2);
    await act(async () => {
      first.resolve({ ...issue, priority: 0 });
      await first.promise;
    });
    await waitFor(() => expect(writesOf(client).rows()).toEqual([]));
    expect(bodies(patchIssue)).toEqual([{ priority: 0 }, { priority: 2 }]);
    expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { priority: 2 }, keyedAttempt);
    expect(select().value).toBe("2");
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  } finally {
    view.unmount();
    patchIssue.mockRestore();
  }
});

// An Inbox row and its reading pane each draw a picker of the issue's priority: a pick in one
// takes out a waiting pick of the other, as two picks in one picker do.
test("two pickers of one field share one line: a pick in either takes out the other's waiting pick", async () => {
  const first = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches()
    .mockImplementationOnce(() => first.promise)
    .mockImplementation(stores);
  const client = signedIn();
  const reads = dispatchReads();
  const view = renderIn(
    client,
    <>
      <section aria-label="Inbox row">
        <CachedPriority reads={reads} />
      </section>
      <section aria-label="Reading pane">
        <CachedPriority reads={reads} />
      </section>
    </>
  );
  const user = userEvent.setup();
  try {
    const [row, pane] = prioritySelects() as [HTMLSelectElement, HTMLSelectElement];
    await user.selectOptions(row, "0");
    await user.selectOptions(pane, "3");
    await user.selectOptions(row, "2");
    expect(
      writesOf(client)
        .rows()
        .map((write) => write.label)
    ).toEqual(["CORE-1 set to P0", "CORE-1 set to P2"]);

    reads.store(2);
    await act(async () => {
      first.resolve({ ...issue, priority: 0 });
      await first.promise;
    });
    await waitFor(() => expect(writesOf(client).rows()).toEqual([]));
    expect(bodies(patchIssue)).toEqual([{ priority: 0 }, { priority: 2 }]);
    expect(prioritySelects().map((select) => select.value)).toEqual(["2", "2"]);
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  } finally {
    view.unmount();
    patchIssue.mockRestore();
  }
});

// A pick takes out only the waiting picks of its own field: every other write of the issue that
// waits in its line - its status, its labels, a board move, a pick of another field - is sent.
test("a pick takes out no waiting write of the issue but a pick of its own field", async () => {
  const first = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches()
    .mockImplementationOnce(() => first.promise)
    .mockImplementation(stores);
  const client = signedIn();
  const reads = dispatchReads();
  const view = renderIn(client, <CachedPriority reads={reads} />);
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={client}>{children}</QueryClientProvider>
  );
  const board = renderHook(() => useBoardMoves(issue.project), { wrapper });
  const assignee = renderHook(() => useIssueFieldWrite(issue.key, "assignee"), { wrapper });
  const plain = (label: string, input: UpdateIssueInput) =>
    act(() => {
      void writesOf(client)
        .run({
          detail: null,
          kind: plainWrite,
          label,
          presentation: "indicator",
          request: issuePatchRequest(issue.key, input),
          target: `issue:${issue.key}`,
        })
        .catch(() => undefined);
    });
  const user = userEvent.setup();
  try {
    const select = () => prioritySelects()[0] as HTMLSelectElement;
    await user.selectOptions(select(), "0");
    await user.selectOptions(select(), "3");
    plain("CORE-1 set to done", { status: "done" });
    plain("Labels of CORE-1", { labels: ["ui"] });
    act(() => {
      void board.result.current.moveCardToLane(issue.key, "todo", 2, 0).catch(() => undefined);
    });
    await act(async () => {
      assignee.result.current.submit("bob");
      await Promise.resolve();
    });
    await waitFor(() => expect(writesOf(client).rows()).toHaveLength(6));
    await user.selectOptions(select(), "1");
    expect(
      writesOf(client)
        .rows()
        .map((row) => [row.label, row.status])
    ).toEqual([
      ["CORE-1 set to P0", "sending"],
      ["CORE-1 set to done", "queued"],
      ["Labels of CORE-1", "queued"],
      ["CORE-1 moved on the board", "queued"],
      ["CORE-1 assigned to bob", "queued"],
      ["CORE-1 set to P1", "queued"],
    ]);

    await act(async () => {
      first.resolve({ ...issue, priority: 0 });
      await first.promise;
    });
    await waitFor(() => expect(writesOf(client).rows()).toEqual([]));
    expect(bodies(patchIssue)).toEqual([
      { priority: 0 },
      { status: "done" },
      { labels: ["ui"] },
      expect.objectContaining({ priority: 2 }),
      { assignee: "bob" },
      { priority: 1 },
    ]);
    expect(select().value).toBe("1");
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  } finally {
    assignee.unmount();
    board.unmount();
    view.unmount();
    patchIssue.mockRestore();
  }
});

test("a refused pick shows Dispatch's reason with Retry and puts back the assignee Dispatch last confirmed, after an earlier pick landed", async () => {
  const listUsers = spyOn(api, "listUsers").mockResolvedValue([
    { login: "alice" },
    { login: "bob" },
    { login: "carol" },
  ]);
  const patchIssue = answerIssuePatches()
    .mockResolvedValueOnce({ ...issue, assignee: "bob", last_seq: 2 })
    .mockRejectedValueOnce(
      new ApiError(400, {
        code: "ASSIGNEE_NOT_ALLOWED",
        error: '"carol" has not signed in to Dispatch',
      })
    )
    .mockResolvedValueOnce({ ...issue, assignee: "carol", last_seq: 3 });
  const client = signedIn();
  // The issue's read, which the test seeds: the one asked of it is the issue read again.
  const detailRead = mock(() =>
    Promise.reject<IssueDetails>(new Error("the test seeds this query"))
  );
  function CachedAssignee(): ReactNode {
    const cached = useQuery<IssueDetails>({
      queryFn: detailRead,
      queryKey: detailKey,
      staleTime: Number.POSITIVE_INFINITY,
    });
    if (cached.data === undefined) return null;
    return <AssigneeControl assignee={cached.data.assignee} issueKey={cached.data.key} />;
  }
  const view = renderIn(client, <CachedAssignee />);
  const user = userEvent.setup();
  const select = () =>
    screen.getByRole("combobox", { name: "Assignee of CORE-1" }) as HTMLSelectElement;
  try {
    await user.hover(select());
    await waitFor(() => expect(select().options.length).toBe(4));
    await user.selectOptions(select(), "bob");
    await waitFor(() => expect(client.getQueryData<IssueDetails>(detailKey)?.last_seq).toBe(2));
    // The landing has the issue read again, as Dispatch now holds it.
    await waitFor(() => expect(detailRead).toHaveBeenCalledTimes(1));
    await user.selectOptions(select(), "carol");
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent('"carol" has not signed in to Dispatch');
    expect(within(alert).getByRole("button", { name: "Retry" })).toBeTruthy();
    // So does the refusal.
    await waitFor(() => expect(detailRead).toHaveBeenCalledTimes(2));
    // Back on the assignee Dispatch stored last, which the landing answered: not the one the
    // issue had before the first pick.
    expect(select().value).toBe("bob");
    expect(client.getQueryData<IssueSummary[]>(listKey)?.[0]?.assignee).toBe("bob");
    const [row] = client.getQueryData<InboxRow[]>(inboxKey) ?? [];
    expect([row?.issue?.assignee, row?.waiting_on_person]).toEqual(["bob", "bob"]);

    await user.click(within(alert).getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(3));
    expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { assignee: "carol" }, keyedAttempt);
    await waitFor(() => expect(screen.queryByRole("alert")).not.toBeInTheDocument());
    expect(select().value).toBe("carol");
  } finally {
    view.unmount();
    patchIssue.mockRestore();
    listUsers.mockRestore();
  }
});

// The screen shows the newer pick, which Dispatch will store last, so the older one's refusal is
// reported with no Retry: a Retry would join the back of the line and be stored last.
test("an older pick refused while a newer one is live keeps the newer one on screen and reports the refusal with no Retry until the newer one lands", async () => {
  const first = Promise.withResolvers<Issue>();
  const second = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches()
    .mockImplementationOnce(() => first.promise)
    .mockImplementationOnce(() => second.promise);
  const client = signedIn();
  const reads = dispatchReads();
  const view = renderIn(client, <CachedPriority reads={reads} />);
  const user = userEvent.setup();
  try {
    const select = () => prioritySelects()[0] as HTMLSelectElement;
    await user.selectOptions(select(), "0");
    await user.selectOptions(select(), "2");
    await act(async () => {
      first.reject(new ApiError(409, { code: "ISSUE_CLOSED", error: "CORE-1 is closed" }));
      await first.promise.catch(() => undefined);
    });
    await waitFor(() => expect(bodies(patchIssue)).toEqual([{ priority: 0 }, { priority: 2 }]));
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("Could not update the priority of CORE-1.");
    expect(within(alert).queryByRole("button", { name: "Retry" })).not.toBeInTheDocument();
    expect(select().value).toBe("2");
    expect(cachedPriorities(client)).toEqual({ detail: 2, inbox: 2, list: 2 });

    reads.store(2);
    await act(async () => {
      second.resolve({ ...issue, priority: 2 });
      await second.promise;
    });
    await waitFor(() => expect(writesOf(client).rows()).toEqual([]));
    expect(bodies(patchIssue)).toEqual([{ priority: 0 }, { priority: 2 }]);
    // Dispatch holds the newer pick now: the refusal has nothing left to report.
    await waitFor(() => expect(screen.queryByRole("alert")).not.toBeInTheDocument());
    expect(select().value).toBe("2");
  } finally {
    view.unmount();
    patchIssue.mockRestore();
  }
});

// The other way the report ends: the person picks again while the newer pick is still out. The
// new pick is the one the screen shows and Dispatch stores last, so the older refusal has nothing
// left to report.
test("an older pick refused while a newer one is live reports the refusal with no Retry until the person picks again", async () => {
  const first = Promise.withResolvers<Issue>();
  const second = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches()
    .mockImplementationOnce(() => first.promise)
    .mockImplementationOnce(() => second.promise)
    .mockImplementation(stores);
  const client = signedIn();
  const reads = dispatchReads();
  const view = renderIn(client, <CachedPriority reads={reads} />);
  const user = userEvent.setup();
  try {
    const select = () => prioritySelects()[0] as HTMLSelectElement;
    await user.selectOptions(select(), "0");
    await user.selectOptions(select(), "2");
    await act(async () => {
      first.reject(new ApiError(409, { code: "ISSUE_CLOSED", error: "CORE-1 is closed" }));
      await first.promise.catch(() => undefined);
    });
    await waitFor(() => expect(bodies(patchIssue)).toEqual([{ priority: 0 }, { priority: 2 }]));
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("Could not update the priority of CORE-1.");
    expect(within(alert).queryByRole("button", { name: "Retry" })).not.toBeInTheDocument();

    // P2 is still out when the person picks P3: the report clears with the press.
    await user.selectOptions(select(), "3");
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    expect(select().value).toBe("3");
    expect(cachedPriorities(client)).toEqual({ detail: 3, inbox: 3, list: 3 });

    reads.store(3);
    await act(async () => {
      second.resolve({ ...issue, priority: 2 });
      await second.promise;
    });
    await waitFor(() => expect(writesOf(client).rows()).toEqual([]));
    expect(bodies(patchIssue)).toEqual([{ priority: 0 }, { priority: 2 }, { priority: 3 }]);
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    expect(select().value).toBe("3");
  } finally {
    view.unmount();
    patchIssue.mockRestore();
  }
});

/** Has the page's store make a save of CORE-1's status, from the issue page's header. */
function saveStatus(client: DispatchQueryClient): void {
  act(() => {
    void writesOf(client)
      .run({
        detail: null,
        kind: plainWrite,
        label: "CORE-1 set to done",
        presentation: "indicator",
        request: issuePatchRequest(issue.key, { status: "done" }),
        target: `issue:${issue.key}`,
      })
      .catch(() => undefined);
  });
}

const closed = () => new ApiError(409, { code: "ISSUE_CLOSED", error: "CORE-1 is closed" });

// The report stands for the newer pick of its field, not for any write of the issue: a save of
// another field that lands meanwhile - the issue's status - leaves it as it is.
test("an older pick's refusal reported with no Retry stays while a save of another field of the issue lands, until the newer pick lands", async () => {
  const first = Promise.withResolvers<Issue>();
  const status = Promise.withResolvers<Issue>();
  const newer = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches()
    .mockImplementationOnce(() => first.promise)
    .mockImplementationOnce(() => status.promise)
    .mockImplementationOnce(() => newer.promise);
  const client = signedIn();
  const reads = dispatchReads();
  const view = renderIn(client, <CachedPriority reads={reads} />);
  const user = userEvent.setup();
  try {
    const select = () => prioritySelects()[0] as HTMLSelectElement;
    await user.selectOptions(select(), "0");
    saveStatus(client);
    await user.selectOptions(select(), "2");
    await act(async () => {
      first.reject(closed());
      await first.promise.catch(() => undefined);
    });
    await waitFor(() => expect(bodies(patchIssue)).toEqual([{ priority: 0 }, { status: "done" }]));
    const alert = await screen.findByRole("alert");
    expect(within(alert).queryByRole("button", { name: "Retry" })).not.toBeInTheDocument();

    await act(async () => {
      status.resolve({ ...issue, status: "done" });
      await status.promise;
    });
    await waitFor(() => expect(bodies(patchIssue)).toHaveLength(3));
    // The status landed and P2 is out: the refusal still stands for the newer pick.
    expect(screen.getByRole("alert")).toHaveTextContent("Could not update the priority of CORE-1.");
    expect(
      within(screen.getByRole("alert")).queryByRole("button", { name: "Retry" })
    ).not.toBeInTheDocument();
    expect(select().value).toBe("2");

    reads.store(2);
    await act(async () => {
      newer.resolve({ ...issue, priority: 2, status: "done" });
      await newer.promise;
    });
    await waitFor(() => expect(screen.queryByRole("alert")).not.toBeInTheDocument());
    expect(select().value).toBe("2");
  } finally {
    view.unmount();
    patchIssue.mockRestore();
  }
});

/** Two pickers of CORE-1's priority, on the issue page and in the Inbox drawer opened over it,
 *  each with its own report. */
function renderTwoPickers(client: DispatchQueryClient, reads: Reads): RenderResult {
  return renderIn(
    client,
    <>
      <section aria-label="Issue page">
        <CachedPriority reads={reads} />
      </section>
      <section aria-label="Inbox drawer">
        <CachedPriority reads={reads} />
        <PendingWritesIndicator />
      </section>
    </>
  );
}

const onPage = () => within(screen.getByRole("region", { name: "Issue page" }));
const inDrawer = () => within(screen.getByRole("region", { name: "Inbox drawer" }));

// The newer pick ends without landing and no other newer write of the field is left: the older
// refusal is then a refused pick with no newer live pick of its field, which R3 reports with Retry.
test("an older pick's refusal reported with no Retry offers Retry once the newer pick, made in another picker, is refused", async () => {
  const first = Promise.withResolvers<Issue>();
  const second = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches()
    .mockImplementationOnce(() => first.promise)
    .mockImplementationOnce(() => second.promise)
    .mockImplementation(stores);
  const client = signedIn();
  const view = renderTwoPickers(client, dispatchReads());
  const user = userEvent.setup();
  try {
    const [page, drawer] = prioritySelects() as [HTMLSelectElement, HTMLSelectElement];
    await user.selectOptions(page, "0");
    await user.selectOptions(drawer, "2");
    await act(async () => {
      first.reject(closed());
      await first.promise.catch(() => undefined);
    });
    const reported = await onPage().findByRole("alert");
    expect(within(reported).queryByRole("button", { name: "Retry" })).not.toBeInTheDocument();

    await act(async () => {
      second.reject(closed());
      await second.promise.catch(() => undefined);
    });
    expect(await inDrawer().findByRole("button", { name: "Retry" })).toBeInTheDocument();
    await waitFor(() =>
      expect(
        within(onPage().getByRole("alert")).getByRole("button", { name: "Retry" })
      ).toBeTruthy()
    );
    expect(cachedPriorities(client)).toEqual({ detail: 1, inbox: 1, list: 1 });

    await user.click(within(onPage().getByRole("alert")).getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(writesOf(client).rows()).toEqual([]));
    expect(bodies(patchIssue)).toEqual([{ priority: 0 }, { priority: 2 }, { priority: 0 }]);
    expect(onPage().queryByRole("alert")).not.toBeInTheDocument();
  } finally {
    view.unmount();
    patchIssue.mockRestore();
  }
});

test("an older pick's refusal reported with no Retry offers Retry once the newer pick, made in another picker, is discarded", async () => {
  const first = Promise.withResolvers<Issue>();
  const status = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches()
    .mockImplementationOnce(() => first.promise)
    .mockImplementationOnce(() => status.promise);
  const client = signedIn();
  const view = renderTwoPickers(client, dispatchReads());
  const user = userEvent.setup();
  try {
    const [page, drawer] = prioritySelects() as [HTMLSelectElement, HTMLSelectElement];
    await user.selectOptions(page, "0");
    saveStatus(client);
    await user.selectOptions(drawer, "2");
    await act(async () => {
      first.reject(closed());
      await first.promise.catch(() => undefined);
    });
    await waitFor(() => expect(bodies(patchIssue)).toEqual([{ priority: 0 }, { status: "done" }]));
    const reported = await onPage().findByRole("alert");
    expect(within(reported).queryByRole("button", { name: "Retry" })).not.toBeInTheDocument();

    // P2 still waits behind the status: the person discards it from the Pending changes list.
    const pending = inDrawer().getByTestId("pending-writes");
    expect(pending).toHaveTextContent("CORE-1 set to P2 — Waiting to send");
    await user.click(within(pending).getByRole("button", { name: "Discard" }));
    await waitFor(() =>
      expect(
        within(onPage().getByRole("alert")).getByRole("button", { name: "Retry" })
      ).toBeTruthy()
    );
    expect(inDrawer().queryByRole("alert")).not.toBeInTheDocument();
    expect(cachedPriorities(client)).toEqual({ detail: 1, inbox: 1, list: 1 });
    expect(bodies(patchIssue)).toEqual([{ priority: 0 }, { status: "done" }]);
  } finally {
    view.unmount();
    patchIssue.mockRestore();
  }
});

/** Every write this tab keeps in its sessionStorage, by name. */
function keptWrites(): [string, string][] {
  const storage = window.sessionStorage;
  return Array.from({ length: storage.length }, (_, index) => storage.key(index))
    .filter((name): name is string => name?.startsWith("dispatch.write:") === true)
    .map((name) => [name, storage.getItem(name) ?? ""]);
}

/** Makes two picks in a page whose first save never answers, and reloads: the page that made
 *  them goes (its kept writes with it), and the next one finds what the tab kept, as pressed
 *  `pressedAgo` before the reload, and takes it up signed in as the same person. */
async function pickTwiceAndReload(pressedAgo: number) {
  const client = signedIn();
  const view = renderIn(client, <CachedPriority reads={dispatchReads()} />);
  const user = userEvent.setup();
  await user.selectOptions(prioritySelects()[0] as HTMLSelectElement, "0");
  await user.selectOptions(prioritySelects()[0] as HTMLSelectElement, "3");
  view.unmount();
  const kept = keptWrites();
  resetWriteStorage();
  for (const [name, value] of kept) {
    const write = JSON.parse(value) as { pressedAt: number };
    const pressedAt = write.pressedAt - pressedAgo;
    window.sessionStorage.setItem(name, JSON.stringify({ ...write, pressedAt }));
  }
  const after = new DispatchQueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  after.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  writesOf(after).resume("alice");
  return after;
}

test("picks kept across a reload are sent again within five minutes, in the order they were made, and show from the first answer after it", async () => {
  const resent = Promise.withResolvers<void>();
  const patchIssue = answerIssuePatches()
    .mockImplementationOnce(() => Promise.withResolvers<Issue>().promise)
    .mockImplementation(async (key, input) => {
      await resent.promise;
      return stores(key, input);
    });
  try {
    const after = await pickTwiceAndReload(0);
    // The page after the reload holds nothing yet: the first read of the issue to answer, which
    // Dispatch composed before it had either pick, has the newest pick laid over it.
    const reads = dispatchReads();
    await act(() => after.fetchQuery({ ...issueQuery(issue.key), queryFn: reads.detail }));
    expect(after.getQueryData<IssueDetails>(detailKey)?.priority).toBe(3);

    await act(async () => resent.resolve());
    await waitFor(() => expect(writesOf(after).rows()).toEqual([]));
    // The first page sent the first pick; the next sends it again under its key, then the second.
    expect(bodies(patchIssue)).toEqual([{ priority: 0 }, { priority: 0 }, { priority: 3 }]);
    const keys = patchIssue.mock.calls.map((call) => call[2].key);
    expect(keys[1]).toBe(keys[0] as string);
    expect(keys[2]).not.toBe(keys[0] as string);
  } finally {
    patchIssue.mockRestore();
  }
});

test("picks kept across a reload for more than five minutes are not sent again, and show with Retry and Discard", async () => {
  const patchIssue = answerIssuePatches()
    .mockImplementationOnce(() => Promise.withResolvers<Issue>().promise)
    .mockImplementation(stores);
  let indicator: RenderResult | undefined;
  try {
    const after = await pickTwiceAndReload(KEPT_SEND_RESUME_MS + 1_000);
    indicator = renderIn(after, <PendingWritesIndicator />);
    const shown = await screen.findByTestId("pending-writes");
    expect(shown).toHaveTextContent("CORE-1 set to P0");
    expect(shown).toHaveTextContent("CORE-1 set to P3");
    expect(within(shown).getAllByRole("button", { name: "Retry" })).toHaveLength(2);
    expect(within(shown).getAllByRole("button", { name: "Discard" })).toHaveLength(2);
    expect(
      writesOf(after)
        .rows()
        .map((row) => [row.label, row.status])
    ).toEqual([
      ["CORE-1 set to P0", "refused"],
      ["CORE-1 set to P3", "refused"],
    ]);
    expect(bodies(patchIssue)).toEqual([{ priority: 0 }]);
  } finally {
    indicator?.unmount();
    patchIssue.mockRestore();
  }
});

/** Has `client` read CORE-1's detail, its project's list and the Inbox, as Dispatch answers
 *  them through `reads`. */
function readAll(client: DispatchQueryClient, reads: Reads) {
  return act(() =>
    Promise.all([
      client.fetchQuery({ ...issueQuery(issue.key), queryFn: reads.detail }),
      client.fetchQuery({ queryFn: reads.list, queryKey: listKey }),
      client.fetchQuery({ queryFn: reads.inbox, queryKey: inboxKey }),
    ])
  );
}

// Case 3 of the roll-back row: a page after a reload holds nothing when its kept picks start
// again, so the value Dispatch last confirmed is the field in its newest answer since the reload -
// not its first, and never one from before it - read before the newest pick is shown over it.
test("picks kept across a reload and refused put back the priority of Dispatch's newest answer since it, on every surface, before the issue is read again", async () => {
  const refuse = Promise.withResolvers<void>();
  const patchIssue = answerIssuePatches()
    .mockImplementationOnce(() => Promise.withResolvers<Issue>().promise)
    .mockImplementation(async () => {
      await refuse.promise;
      throw closed();
    });
  let surfaces: RenderResult | undefined;
  try {
    const after = await pickTwiceAndReload(0);
    // The page after the reload draws the issue, its project's list and the Inbox: its first
    // answers carry P1, the priority Dispatch held before either pick.
    const reads = dispatchReads();
    surfaces = renderIn(after, <Surfaces reads={reads} />);
    await waitFor(() =>
      expect(shownPriorities()).toEqual({ detail: "P3", inbox: "P3", list: "P3" })
    );
    // Another person sets P2, and the next answers carry it.
    reads.store(2);
    await readAll(after, reads);
    expect(shownPriorities()).toEqual({ detail: "P3", inbox: "P3", list: "P3" });
    // The reads the refusal starts are held until what it put back is checked.
    const answerReads = reads.hold();

    await act(async () => refuse.resolve());
    await waitFor(() =>
      expect(
        writesOf(after)
          .rows()
          .map((row) => row.status)
      ).toEqual(["refused", "refused"])
    );
    expect(cachedPriorities(after)).toEqual({ detail: 2, inbox: 2, list: 2 });
    expect(shownPriorities()).toEqual({ detail: "P2", inbox: "P2", list: "P2" });
    await act(async () => answerReads());
    expect(shownPriorities()).toEqual({ detail: "P2", inbox: "P2", list: "P2" });
  } finally {
    surfaces?.unmount();
    patchIssue.mockRestore();
  }
});

// Case 4 of the roll-back row: a lapse lets go of every value confirmed (`reset`), and the app
// empties its cache (`AuthGate`) and draws the page again once the same person signs in, while
// the pick is still out: the value put back is the field in Dispatch's newest answer since that
// sign-in.
test("a pick refused after a lapse and the same person's sign-in puts back the priority of Dispatch's newest answer since the sign-in, on every surface, before the issue is read again", async () => {
  const save = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches().mockImplementationOnce(() => save.promise);
  const client = signedIn();
  const writes = writesOf(client);
  act(() => writes.signedIn("alice"));
  const picker = renderHook(() => useIssueFieldWrite(issue.key, "priority"), {
    wrapper: ({ children }) => (
      <QueryClientProvider client={client}>{children}</QueryClientProvider>
    ),
  });
  let surfaces: RenderResult | undefined;
  try {
    await act(async () => {
      picker.result.current.submit(0);
      await Promise.resolve();
    });
    await waitFor(() => expect(bodies(patchIssue)).toEqual([{ priority: 0 }]));
    act(() => {
      writes.signedIn(undefined);
      client.removeQueries({ predicate: (query) => query.queryKey[0] !== "whoami" });
    });
    act(() => writes.signedIn("alice"));
    // The page drawn after the sign-in: its first answers carry P1, the priority Dispatch held
    // before the pick.
    const reads = dispatchReads();
    surfaces = renderIn(client, <Surfaces reads={reads} />);
    await waitFor(() =>
      expect(shownPriorities()).toEqual({ detail: "P0", inbox: "P0", list: "P0" })
    );
    // Another person sets P2, and the next answers carry it.
    reads.store(2);
    await readAll(client, reads);
    expect(shownPriorities()).toEqual({ detail: "P0", inbox: "P0", list: "P0" });
    // The reads the refusal starts are held until what it put back is checked.
    const answerReads = reads.hold();

    await act(async () => {
      save.reject(closed());
      await save.promise.catch(() => undefined);
    });
    await waitFor(() => expect(writes.rows()).toEqual([]));
    expect(picker.result.current.retry).toBeDefined();
    expect(cachedPriorities(client)).toEqual({ detail: 2, inbox: 2, list: 2 });
    expect(shownPriorities()).toEqual({ detail: "P2", inbox: "P2", list: "P2" });
    await act(async () => answerReads());
    expect(shownPriorities()).toEqual({ detail: "P2", inbox: "P2", list: "P2" });
  } finally {
    surfaces?.unmount();
    picker.unmount();
    patchIssue.mockRestore();
  }
});

test("an answer that lands while a pick is unsaved keeps the pick on the issue, a project list and an Inbox row, with the picker gone", async () => {
  const save = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches().mockImplementation(() => save.promise);
  const client = signedIn();
  const reads = dispatchReads();
  const surfaces = renderIn(client, <Surfaces reads={reads} />);
  const picker = renderIn(client, <CachedPriority reads={reads} />);
  const user = userEvent.setup();
  try {
    await user.selectOptions(prioritySelects()[0] as HTMLSelectElement, "2");
    await waitFor(() => expect(bodies(patchIssue)).toEqual([{ priority: 2 }]));
    picker.unmount();

    // Reads composed before Dispatch has the pick, answered while it is out, and the answer of
    // another write to the issue, merged: each holds the old priority.
    await act(() =>
      Promise.all(
        [detailKey, listKey, inboxKey].map((queryKey) => client.refetchQueries({ queryKey }))
      )
    );
    act(() => mergeIssue(client, { ...issue, priority: 1, title: "Review the spec again" }));
    expect(cachedPriorities(client)).toEqual({ detail: 2, inbox: 2, list: 2 });
    expect(shownPriorities()).toEqual({ detail: "P2", inbox: "P2", list: "P2" });
    // The answer is taken whole but for the pick: the title the merge carried is shown.
    expect(client.getQueryData<IssueDetails>(detailKey)?.title).toBe("Review the spec again");

    reads.store(2);
    await act(async () => {
      save.resolve({ ...issue, priority: 2, title: "Review the spec again" });
      await save.promise;
    });
    await waitFor(() => expect(writesOf(client).rows()).toEqual([]));
    expect(shownPriorities()).toEqual({ detail: "P2", inbox: "P2", list: "P2" });
  } finally {
    surfaces.unmount();
    picker.unmount();
    patchIssue.mockRestore();
  }
});

test("a read that answers after the pick landed with the value from before it does not show that value", async () => {
  const save = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches().mockImplementation(() => save.promise);
  const client = signedIn();
  const reads = dispatchReads();
  const surfaces = renderIn(client, <Surfaces reads={reads} />);
  const picker = renderIn(client, <CachedPriority reads={reads} />);
  const user = userEvent.setup();
  try {
    await user.selectOptions(prioritySelects()[0] as HTMLSelectElement, "2");
    await waitFor(() => expect(bodies(patchIssue)).toEqual([{ priority: 2 }]));
    // Three reads reach Dispatch before the save does, and are still out when it lands.
    const answerEarlyReads = reads.hold();
    const early = Promise.all(
      [detailKey, listKey, inboxKey].map((queryKey) => client.refetchQueries({ queryKey }))
    );
    reads.store(2);
    await act(async () => {
      save.resolve({ ...issue, priority: 2 });
      await save.promise;
    });
    await waitFor(() => expect(writesOf(client).rows()).toEqual([]));

    // Their answers land after the landing, each carrying the priority from before the pick; the
    // reads the landing asks for are still out.
    const answerLateReads = reads.hold();
    await act(async () => {
      answerEarlyReads();
      await early;
    });
    expect(cachedPriorities(client)).toEqual({ detail: 2, inbox: 2, list: 2 });
    expect(shownPriorities()).toEqual({ detail: "P2", inbox: "P2", list: "P2" });

    await act(async () => {
      answerLateReads();
      await client.refetchQueries({ queryKey: listKey });
    });
    expect(shownPriorities()).toEqual({ detail: "P2", inbox: "P2", list: "P2" });
  } finally {
    surfaces.unmount();
    picker.unmount();
    patchIssue.mockRestore();
  }
});

// A read sent before a write of the field landed is older than that write's answer, whenever it
// arrives: P2 lands while P0 waits behind it and three reads sent before the landing are out;
// their answers, carrying P1 from before P2, land after it, and P0 is then refused. What it puts
// back is P2, Dispatch's answer to the landing, not P1.
test("a read sent before a pick landed that answers after it is older than the landing: a refusal after it puts back the landed value", async () => {
  const save = Promise.withResolvers<Issue>();
  const refusal = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches()
    .mockImplementationOnce(() => save.promise)
    .mockImplementationOnce(() => refusal.promise);
  const client = signedIn();
  const reads = dispatchReads();
  const surfaces = renderIn(client, <Surfaces reads={reads} />);
  const picker = renderIn(client, <CachedPriority reads={reads} />);
  const user = userEvent.setup();
  try {
    await user.selectOptions(prioritySelects()[0] as HTMLSelectElement, "2");
    await waitFor(() => expect(bodies(patchIssue)).toEqual([{ priority: 2 }]));
    await user.selectOptions(prioritySelects()[0] as HTMLSelectElement, "0");
    const answerEarlyReads = reads.hold();
    const early = Promise.all(
      [detailKey, listKey, inboxKey].map((queryKey) => client.refetchQueries({ queryKey }))
    );
    reads.store(2);
    await act(async () => {
      save.resolve({ ...issue, priority: 2 });
      await save.promise;
    });
    await waitFor(() => expect(bodies(patchIssue)).toEqual([{ priority: 2 }, { priority: 0 }]));
    await act(async () => {
      answerEarlyReads();
      await early;
    });
    expect(shownPriorities()).toEqual({ detail: "P0", inbox: "P0", list: "P0" });
    // The reads the refusal starts are held until what it put back is checked.
    const answerReads = reads.hold();

    await act(async () => {
      refusal.reject(closed());
      await refusal.promise.catch(() => undefined);
    });
    await waitFor(() => expect(writesOf(client).rows()).toEqual([]));
    expect(cachedPriorities(client)).toEqual({ detail: 2, inbox: 2, list: 2 });
    expect(shownPriorities()).toEqual({ detail: "P2", inbox: "P2", list: "P2" });
    await act(async () => answerReads());
    expect(shownPriorities()).toEqual({ detail: "P2", inbox: "P2", list: "P2" });
  } finally {
    surfaces.unmount();
    picker.unmount();
    patchIssue.mockRestore();
  }
});

test("a refused priority pick with no newer live pick puts back the confirmed priority and shows its fixed line with Retry", async () => {
  const patchIssue = answerIssuePatches().mockRejectedValueOnce(
    new ApiError(409, { code: "ISSUE_CLOSED", error: "CORE-1 is closed" })
  );
  const client = signedIn();
  const reads = dispatchReads();
  const detailRead = mock(reads.detail);
  const view = renderIn(client, <CachedPriority reads={{ ...reads, detail: detailRead }} />);
  const user = userEvent.setup();
  try {
    await user.selectOptions(prioritySelects()[0] as HTMLSelectElement, "0");
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("Could not update the priority of CORE-1.");
    expect(within(alert).getByRole("button", { name: "Retry" })).toBeTruthy();
    expect(cachedPriorities(client)).toEqual({ detail: 1, inbox: 1, list: 1 });
    expect((prioritySelects()[0] as HTMLSelectElement).value).toBe("1");
    // The issue is read again, as Dispatch holds it.
    await waitFor(() => expect(detailRead).toHaveBeenCalledTimes(1));
    expect((prioritySelects()[0] as HTMLSelectElement).value).toBe("1");
  } finally {
    view.unmount();
    patchIssue.mockRestore();
  }
});

// Case 1 of the roll-back row, the sequence that tells the value Dispatch last confirmed from the
// value before a pick: P1 goes out, P2 waits behind it, P1 is refused while P2 is live, then P2 is
// refused. The value on screen before the pick of P2 was P1, which Dispatch never stored; what it
// last confirmed is the P3 the issue showed at the first pick, which shows before the issue's
// read again answers.
test("a refused pick whose older pick was refused before it puts back the priority shown at the first pick, not the older pick, with the fixed line and Retry, and reads the issue again", async () => {
  const first = Promise.withResolvers<Issue>();
  const second = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches()
    .mockImplementationOnce(() => first.promise)
    .mockImplementationOnce(() => second.promise);
  const client = signedIn();
  client.setQueryData(detailKey, { ...details, priority: 3 });
  client.setQueryData(listKey, [{ ...summary, priority: 3 }]);
  client.setQueryData(inboxKey, [{ ...inboxRow, priority: 3 }]);
  const reads = dispatchReads();
  reads.store(3);
  const detailRead = mock(reads.detail);
  const view = renderIn(client, <CachedPriority reads={{ ...reads, detail: detailRead }} />);
  const user = userEvent.setup();
  try {
    const select = () => prioritySelects()[0] as HTMLSelectElement;
    await user.selectOptions(select(), "1");
    await user.selectOptions(select(), "2");
    expect(
      writesOf(client)
        .rows()
        .map((row) => [row.label, row.status])
    ).toEqual([
      ["CORE-1 set to P1", "sending"],
      ["CORE-1 set to P2", "queued"],
    ]);

    // P1 is refused with P2 live: P2 stays on screen, and the refusal is reported with no Retry.
    await act(async () => {
      first.reject(new ApiError(409, { code: "ISSUE_CLOSED", error: "CORE-1 is closed" }));
      await first.promise.catch(() => undefined);
    });
    await waitFor(() => expect(bodies(patchIssue)).toEqual([{ priority: 1 }, { priority: 2 }]));
    const reported = await screen.findByRole("alert");
    expect(reported).toHaveTextContent("Could not update the priority of CORE-1.");
    expect(within(reported).queryByRole("button", { name: "Retry" })).not.toBeInTheDocument();
    expect(select().value).toBe("2");
    expect(cachedPriorities(client)).toEqual({ detail: 2, inbox: 2, list: 2 });
    const readsBefore = detailRead.mock.calls.length;
    // The issue's read again is held until the roll-back is checked, so what shows is the
    // roll-back's alone.
    const answerReads = reads.hold();

    // P2 is refused with no newer live pick: back on P3, the value Dispatch last confirmed.
    await act(async () => {
      second.reject(new ApiError(409, { code: "ISSUE_CLOSED", error: "CORE-1 is closed" }));
      await second.promise.catch(() => undefined);
    });
    const alert = await screen.findByRole("alert");
    await waitFor(() =>
      expect(within(alert).getByRole("button", { name: "Retry" })).toBeInTheDocument()
    );
    expect(alert).toHaveTextContent("Could not update the priority of CORE-1.");
    expect(select().value).toBe("3");
    expect(cachedPriorities(client)).toEqual({ detail: 3, inbox: 3, list: 3 });
    // The issue is read again, as Dispatch holds it.
    await waitFor(() => expect(detailRead.mock.calls.length).toBeGreaterThan(readsBefore));
    await act(async () => answerReads());
    expect(select().value).toBe("3");
    expect(bodies(patchIssue)).toEqual([{ priority: 1 }, { priority: 2 }]);
  } finally {
    view.unmount();
    patchIssue.mockRestore();
  }
});

// Case 2 of the roll-back row: the value Dispatch last confirmed is the field in its newest
// answer, kept apart from the pick shown over it. P1 is out when a read of the issue answers with
// P0, which another person set; P1 is then refused, and from the refusal until the app's read of
// the issue answers, the page shows P0, not the P3 from before the pick. That read is the app's
// one read of the issue after the refusal: Dispatch answers it with P2, which another person set
// meanwhile, and the page then shows P2.
test("a refused pick puts back the priority of Dispatch's newest answer, another person's P0 read while it was out, until the app's one read of the issue after it answers with another", async () => {
  const save = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches().mockImplementationOnce(() => save.promise);
  const client = signedIn();
  client.setQueryData(detailKey, { ...details, priority: 3 });
  client.setQueryData(listKey, [{ ...summary, priority: 3 }]);
  client.setQueryData(inboxKey, [{ ...inboxRow, priority: 3 }]);
  const reads = dispatchReads();
  reads.store(3);
  const detailRead = mock(reads.detail);
  const view = renderIn(client, <CachedPriority reads={{ ...reads, detail: detailRead }} />);
  const user = userEvent.setup();
  try {
    const select = () => prioritySelects()[0] as HTMLSelectElement;
    await user.selectOptions(select(), "1");
    await waitFor(() => expect(bodies(patchIssue)).toEqual([{ priority: 1 }]));
    // Another person sets P0, and a read of the issue answers with it while P1 is out: P1 stays
    // shown over it.
    reads.store(0);
    await act(() => client.refetchQueries({ queryKey: detailKey }));
    expect(select().value).toBe("1");
    expect(cachedPriorities(client)).toEqual({ detail: 1, inbox: 1, list: 1 });
    // Another person sets P2 before the refusal. The read the refusal starts is held until what
    // the refusal put back is checked.
    reads.store(2);
    const answerReads = reads.hold();
    const readsBefore = detailRead.mock.calls.length;

    await act(async () => {
      save.reject(closed());
      await save.promise.catch(() => undefined);
    });
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("Could not update the priority of CORE-1.");
    expect(within(alert).getByRole("button", { name: "Retry" })).toBeInTheDocument();
    expect(select().value).toBe("0");
    expect(cachedPriorities(client)).toEqual({ detail: 0, inbox: 0, list: 0 });
    await waitFor(() => expect(detailRead.mock.calls.length).toBe(readsBefore + 1));
    expect(select().value).toBe("0");

    await act(async () => answerReads());
    await waitFor(() => expect(select().value).toBe("2"));
    expect(client.getQueryData<IssueDetails>(detailKey)?.priority).toBe(2);
    expect(detailRead.mock.calls.length).toBe(readsBefore + 1);
  } finally {
    view.unmount();
    patchIssue.mockRestore();
  }
});

// R3: a read is as new as its request, whether a pick was live when it went out or not. P1
// lands, and the app reads the issue again; another person sets P0, and a read of the issue goes
// out while no pick is live. The person picks P2 while that read is out, the read answers with
// P0, and P2 is refused: what the refusal puts back is P0, Dispatch's newest answer, sent after
// P1 landed - not P1.
test("a read sent while no pick is live is as new as its request: a refusal after its answer puts back its value, not an older landing's", async () => {
  const first = Promise.withResolvers<Issue>();
  const second = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches()
    .mockImplementationOnce(() => first.promise)
    .mockImplementationOnce(() => second.promise);
  const client = signedIn();
  client.setQueryData(detailKey, { ...details, priority: 3 });
  client.setQueryData(listKey, [{ ...summary, priority: 3 }]);
  client.setQueryData(inboxKey, [{ ...inboxRow, priority: 3 }]);
  const reads = dispatchReads();
  reads.store(3);
  const surfaces = renderIn(client, <Surfaces reads={reads} />);
  const picker = renderIn(client, <CachedPriority reads={reads} />);
  const user = userEvent.setup();
  const seen: string[] = [];
  const see = (step: string) => {
    const { detail, inbox, list } = shownPriorities();
    seen.push(`${step}: detail ${detail} list ${list} inbox ${inbox}`);
  };
  try {
    await user.selectOptions(prioritySelects()[0] as HTMLSelectElement, "1");
    reads.store(1);
    await act(async () => {
      first.resolve({ ...issue, priority: 1 });
      await first.promise;
    });
    await waitFor(() => expect(writesOf(client).rows()).toEqual([]));
    await waitFor(() => expect(client.isFetching()).toBe(0));
    see("P1 lands and is read again");
    // Another person sets P0, and a read of the issue goes out while no pick is live.
    reads.store(0);
    const answerRead = reads.hold();
    const read = client.refetchQueries({ queryKey: detailKey });
    await user.selectOptions(prioritySelects()[0] as HTMLSelectElement, "2");
    await waitFor(() => expect(bodies(patchIssue)).toEqual([{ priority: 1 }, { priority: 2 }]));
    see("P2 picked while that read is out");
    await act(async () => {
      answerRead();
      await read;
    });
    see("the read answers P0");
    // The reads the refusal starts are held until what it put back is checked.
    const answerReads = reads.hold();

    await act(async () => {
      second.reject(closed());
      await second.promise.catch(() => undefined);
    });
    await waitFor(() => expect(writesOf(client).rows()).toEqual([]));
    see("P2 refused, before the re-read answers");
    await act(async () => answerReads());
    await waitFor(() => expect(client.isFetching()).toBe(0));
    see("the re-read answers");

    expect(seen).toEqual([
      "P1 lands and is read again: detail P1 list P1 inbox P1",
      "P2 picked while that read is out: detail P2 list P2 inbox P2",
      "the read answers P0: detail P2 list P2 inbox P2",
      "P2 refused, before the re-read answers: detail P0 list P0 inbox P0",
      "the re-read answers: detail P0 list P0 inbox P0",
    ]);
  } finally {
    surfaces.unmount();
    picker.unmount();
    patchIssue.mockRestore();
  }
});

const mappings: RepoProject[] = ["acme/widgets", "acme/gadgets"].map((repo) => ({
  created_at: "2026-10-01T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  default: repo === "acme/widgets",
  project: "CORE",
  renamed_to: null,
  repo,
}));

/** A repository picker on CORE-1, whose own repository is `repo` and which resolves to
 *  `resolved`, with the project's mappings read. */
function renderRepo(repo: string | null, resolved: string | null) {
  const listRepoProjects = spyOn(api, "listRepoProjects").mockResolvedValue(mappings);
  // The issue's read, which the test seeds: the one asked of it is the issue read again.
  const detailRead = mock(() =>
    Promise.reject<IssueDetails>(new Error("the test seeds this query"))
  );
  const client = signedIn();
  client.setQueryData(detailKey, { ...details, repo, resolved_repo: resolved });
  client.setQueryData(listKey, [{ ...summary, repo, resolved_repo: resolved }]);
  function CachedRepo(): ReactNode {
    const cached = useQuery<IssueDetails>({
      queryFn: detailRead,
      queryKey: detailKey,
      staleTime: Number.POSITIVE_INFINITY,
    });
    if (cached.data === undefined) return null;
    return (
      <RepoControl
        issueKey={cached.data.key}
        project={cached.data.project}
        repo={cached.data.repo}
        resolvedRepo={cached.data.resolved_repo}
      />
    );
  }
  const view = renderIn(client, <CachedRepo />);
  const select = () =>
    screen.getByRole("combobox", { name: "Repository of CORE-1" }) as HTMLSelectElement;
  /** What the select says it shows, which names a repository that is not the issue's own. */
  const described = () =>
    document.getElementById(select().getAttribute("aria-describedby") ?? "")?.textContent;
  return {
    client,
    described,
    detailRead,
    select,
    unmount: () => {
      view.unmount();
      listRepoProjects.mockRestore();
    },
  };
}

test("a refused repository pick puts back the repository Dispatch last confirmed and shows its reason with Retry", async () => {
  const patchIssue = answerIssuePatches().mockRejectedValueOnce(
    new ApiError(400, { code: "REPO_NOT_MAPPED", error: "acme/gadgets is not mapped to CORE" })
  );
  const { client, described, detailRead, select, unmount } = renderRepo(null, "acme/widgets");
  const user = userEvent.setup();
  try {
    await user.hover(select());
    await waitFor(() => expect(select().options.length).toBe(3));
    await user.selectOptions(select(), "acme/gadgets");
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("acme/gadgets is not mapped to CORE");
    expect(within(alert).getByRole("button", { name: "Retry" })).toBeTruthy();
    expect(select().value).toBe("");
    expect(described()).toBe("acme/widgets, inherited or the project's default");
    const [row] = client.getQueryData<IssueSummary[]>(listKey) ?? [];
    expect([row?.repo, row?.resolved_repo]).toEqual([null, "acme/widgets"]);
    // The issue is read again, as Dispatch holds it.
    await waitFor(() => expect(detailRead).toHaveBeenCalledTimes(1));
  } finally {
    unmount();
    patchIssue.mockRestore();
  }
});

test("an Inherit pick of the repository shows the repository the issue showed before, not as its own, until the answer names the inherited one", async () => {
  const save = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches().mockImplementation(() => save.promise);
  const { described, select, unmount } = renderRepo("acme/gadgets", "acme/gadgets");
  const user = userEvent.setup();
  try {
    expect(described()).toBe("acme/gadgets");
    await user.hover(select());
    await waitFor(() => expect(select().options.length).toBe(3));
    await user.selectOptions(select(), "");
    await waitFor(() => expect(bodies(patchIssue)).toEqual([{ repo: null }]));
    expect(described()).toBe("acme/gadgets, inherited or the project's default");

    await act(async () => {
      save.resolve({ ...issue, repo: null, resolved_repo: "acme/widgets" });
      await save.promise;
    });
    await waitFor(() =>
      expect(described()).toBe("acme/widgets, inherited or the project's default")
    );
  } finally {
    unmount();
    patchIssue.mockRestore();
  }
});

test("a second pick waits in the Pending changes list as Waiting to send, and its Discard puts back the newest live pick", async () => {
  const first = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches()
    .mockImplementationOnce(() => first.promise)
    .mockImplementation(stores);
  const client = signedIn();
  const reads = dispatchReads();
  const view = renderIn(
    client,
    <>
      <CachedPriority reads={reads} />
      <PendingWritesIndicator />
    </>
  );
  const user = userEvent.setup();
  try {
    await user.selectOptions(prioritySelects()[0] as HTMLSelectElement, "0");
    await user.selectOptions(prioritySelects()[0] as HTMLSelectElement, "3");
    const pending = screen.getByTestId("pending-writes");
    expect(pending).toHaveTextContent("CORE-1 set to P3 — Waiting to send");
    await user.click(within(pending).getByRole("button", { name: "Discard" }));
    // The pick still out is the newest live one: it shows, and nothing is refused.
    expect((prioritySelects()[0] as HTMLSelectElement).value).toBe("0");
    expect(cachedPriorities(client)).toEqual({ detail: 0, inbox: 0, list: 0 });
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();

    reads.store(0);
    await act(async () => {
      first.resolve({ ...issue, priority: 0 });
      await first.promise;
    });
    await waitFor(() => expect(writesOf(client).rows()).toEqual([]));
    expect(bodies(patchIssue)).toEqual([{ priority: 0 }]);
  } finally {
    view.unmount();
    patchIssue.mockRestore();
  }
});

test("a pick discarded from the Pending changes list with no other live pick puts back the confirmed priority", async () => {
  const status = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches().mockImplementationOnce(() => status.promise);
  const client = signedIn();
  const view = renderIn(
    client,
    <>
      <CachedPriority reads={dispatchReads()} />
      <PendingWritesIndicator />
    </>
  );
  const user = userEvent.setup();
  try {
    // Another write of the issue is out ahead of the pick: its status, from the issue page.
    act(() => {
      void writesOf(client)
        .run({
          detail: null,
          kind: plainWrite,
          label: "CORE-1 set to done",
          presentation: "indicator",
          request: issuePatchRequest(issue.key, { status: "done" }),
          target: `issue:${issue.key}`,
        })
        .catch(() => undefined);
    });
    await user.selectOptions(prioritySelects()[0] as HTMLSelectElement, "0");
    const pending = screen.getByTestId("pending-writes");
    expect(pending).toHaveTextContent("CORE-1 set to P0 — Waiting to send");
    expect(cachedPriorities(client)).toEqual({ detail: 0, inbox: 0, list: 0 });
    await user.click(within(pending).getByRole("button", { name: "Discard" }));
    expect(cachedPriorities(client)).toEqual({ detail: 1, inbox: 1, list: 1 });
    expect((prioritySelects()[0] as HTMLSelectElement).value).toBe("1");
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    expect(bodies(patchIssue)).toEqual([{ status: "done" }]);
  } finally {
    view.unmount();
    patchIssue.mockRestore();
  }
});

test("a pick shows on the issue tables of the Priorities view, over their answers, until it lands", async () => {
  const save = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches().mockImplementation(() => save.promise);
  const client = signedIn();
  const advancesKey = ["issues", "advances", "priority-1"] as const;
  client.setQueryData(advancesKey, [summary]);
  const shown = () => client.getQueryData<IssueSummary[]>(advancesKey)?.[0]?.priority;
  const view = renderIn(client, <CachedPriority reads={dispatchReads()} />);
  const user = userEvent.setup();
  try {
    await user.selectOptions(prioritySelects()[0] as HTMLSelectElement, "2");
    expect(shown()).toBe(2);
    // The table's read, composed before Dispatch has the pick, answers while it is out.
    await act(() => client.fetchQuery({ queryFn: async () => [summary], queryKey: advancesKey }));
    expect(shown()).toBe(2);

    await act(async () => {
      save.resolve({ ...issue, priority: 2 });
      await save.promise;
    });
    await waitFor(() => expect(writesOf(client).rows()).toEqual([]));
    expect(shown()).toBe(2);
    // Landed, the pick is laid over no answer: a read the table makes afterwards shows what
    // Dispatch holds then, here a priority somebody set since.
    await act(() =>
      client.fetchQuery({
        queryFn: async () => [{ ...summary, priority: 0 }],
        queryKey: advancesKey,
      })
    );
    expect(shown()).toBe(0);
  } finally {
    view.unmount();
    patchIssue.mockRestore();
  }
});

test("a board move to another priority lane made over a live pick is the priority every surface shows", async () => {
  const pick = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches()
    .mockImplementationOnce(() => pick.promise)
    .mockImplementation(stores);
  const client = signedIn();
  const reads = dispatchReads();
  const view = renderIn(
    client,
    <>
      <CachedPriority reads={reads} />
      <Surfaces reads={reads} />
    </>
  );
  const board = renderHook(() => useBoardMoves(issue.project), {
    wrapper: ({ children }) => (
      <QueryClientProvider client={client}>{children}</QueryClientProvider>
    ),
  });
  const user = userEvent.setup();
  try {
    await user.selectOptions(prioritySelects()[0] as HTMLSelectElement, "1");
    expect(cachedPriorities(client)).toEqual({ detail: 1, inbox: 1, list: 1 });
    // The pick is still out when the card is dragged into the P3 lane: the move joins the issue's
    // line behind it, and Dispatch will store the move's priority last.
    let moved: Promise<void> | undefined;
    act(() => {
      moved = board.result.current.moveCardToLane(issue.key, "todo", 3, 0);
    });
    await waitFor(() => expect(writesOf(client).rows()).toHaveLength(2));
    expect(cachedPriorities(client)).toEqual({ detail: 3, inbox: 3, list: 3 });
    expect(shownPriorities()).toEqual({ detail: "P3", inbox: "P3", list: "P3" });

    reads.store(3);
    await act(async () => {
      pick.resolve({ ...issue, priority: 1 });
      await moved;
    });
    await waitFor(() => expect(writesOf(client).rows()).toEqual([]));
    expect(shownPriorities()).toEqual({ detail: "P3", inbox: "P3", list: "P3" });
    expect(bodies(patchIssue).map((body) => (body as UpdateIssueInput).priority)).toEqual([1, 3]);
  } finally {
    board.unmount();
    view.unmount();
    patchIssue.mockRestore();
  }
});

/** The priority Dispatch stores for CORE-1 once it has applied every save sent so far, in the
 *  order they were sent. */
function storedPriority(patchIssue: { mock: { calls: unknown[][] } }): IssuePriority | null {
  let stored = issuePriority(issue.priority);
  for (const [key, input] of patchIssue.mock.calls) {
    const { priority } = input as UpdateIssueInput;
    if (key === issue.key && priority !== undefined) stored = issuePriority(priority);
  }
  return stored;
}

/** The project's board as the Board reads it (`projectIssuesQuery`, whose read waits until every
 *  board move on the project is answered), showing the priority of CORE-1's card, and, with
 *  `picker`, the card's own priority picker, as each card of the board draws one. */
function BoardCard({ picker = false }: { picker?: boolean }): ReactNode {
  const board = useQuery({
    ...projectIssuesQuery(issue.project, []),
    staleTime: Number.POSITIVE_INFINITY,
  });
  const card = board.data?.find((row) => row.key === issue.key);
  return (
    <>
      <p data-testid="board-priority">P{card?.priority}</p>
      {picker && card !== undefined ? (
        <PriorityControl issueKey={card.key} priority={issuePriority(card.priority)} />
      ) : null}
    </>
  );
}

/** A board on CORE beside the issue's own picker - or, `onCard`, with the picker on CORE-1's card
 *  alone, as the board page draws it - with `others` on it besides CORE-1, whose list and detail
 *  Dispatch answers with the priority it stores as each read reaches it (`stored`), once the row
 *  lets the reads it holds go (`hold`). */
function renderBoard(
  client: DispatchQueryClient,
  stored: () => IssuePriority | null,
  { onCard = false, others = [] }: { onCard?: boolean; others?: readonly IssueSummary[] } = {}
) {
  client.setQueryData(listKey, [summary, ...others]);
  let gate: Promise<void> | undefined;
  const read =
    <T,>(compose: () => T) =>
    async (): Promise<T> => {
      const answer = compose();
      await gate;
      return answer;
    };
  const listIssues = spyOn(api, "listIssues").mockImplementation(
    read(() => [{ ...summary, priority: stored() }, ...others])
  );
  const reads: Reads = {
    ...dispatchReads(),
    detail: read(() => ({ ...details, priority: stored() })),
  };
  const view = renderIn(
    client,
    onCard ? (
      <BoardCard picker />
    ) : (
      <>
        <CachedPriority reads={reads} />
        <BoardCard />
      </>
    )
  );
  const board = renderHook(() => useBoardMoves(issue.project), {
    wrapper: ({ children }) => (
      <QueryClientProvider client={client}>{children}</QueryClientProvider>
    ),
  });
  return {
    /** Holds every read of the board and the issue that starts from now on until the returned
     *  function lets them answer. */
    hold: () => {
      const held = Promise.withResolvers<void>();
      gate = held.promise;
      return () => held.resolve();
    },
    moves: () => board.result.current,
    shown: () => screen.getByTestId("board-priority").textContent,
    unmount: () => {
      board.unmount();
      view.unmount();
      listIssues.mockRestore();
    },
  };
}

// R2: a drag to another priority lane made after a pick is newer than it, so the board shows the
// card in that drag's lane; a second drag is newer again, from the moment it is made, although
// it waits in its board's scope, out of the store, until the first is answered.
test("two drags of a card over its live pick show the card in the newest drag's lane from that drag on", async () => {
  const pick = Promise.withResolvers<Issue>();
  const first = Promise.withResolvers<Issue>();
  const second = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches()
    .mockImplementationOnce(() => pick.promise)
    .mockImplementationOnce(() => first.promise)
    .mockImplementationOnce(() => second.promise);
  const client = signedIn();
  const board = renderBoard(client, () => storedPriority(patchIssue));
  const user = userEvent.setup();
  const seen: string[] = [];
  const see = (step: string) => seen.push(`${step}: ${board.shown()}`);
  try {
    await user.selectOptions(prioritySelects()[0] as HTMLSelectElement, "2");
    await waitFor(() => expect(bodies(patchIssue)).toHaveLength(1));
    see("pick P2 out");
    act(() => {
      void board.moves().moveCardToLane(issue.key, "todo", 3, 0);
    });
    await waitFor(() => expect(writesOf(client).rows()).toHaveLength(2));
    see("drag 1 to P3");
    act(() => {
      void board.moves().moveCardToLane(issue.key, "todo", 0, 0);
    });
    see("drag 2 to P0");

    await act(async () => {
      pick.resolve({ ...issue, priority: 2 });
      await pick.promise;
    });
    await waitFor(() => expect(bodies(patchIssue)).toHaveLength(2));
    see("pick lands, drag 1 out");
    await act(async () => {
      first.resolve({ ...issue, priority: 3 });
      await first.promise;
    });
    await waitFor(() => expect(bodies(patchIssue)).toHaveLength(3));
    see("drag 1 lands, drag 2 out");
    await act(async () => {
      second.resolve({ ...issue, priority: 0 });
      await second.promise;
    });
    await waitFor(() => expect(writesOf(client).rows()).toEqual([]));
    await waitFor(() => expect(client.isFetching()).toBe(0));
    see("drag 2 lands");

    expect(seen).toEqual([
      "pick P2 out: P2",
      "drag 1 to P3: P3",
      "drag 2 to P0: P0",
      "pick lands, drag 1 out: P0",
      "drag 1 lands, drag 2 out: P0",
      "drag 2 lands: P0",
    ]);
    expect(bodies(patchIssue).map((body) => (body as UpdateIssueInput).priority)).toEqual([
      2, 3, 0,
    ]);
  } finally {
    board.unmount();
    patchIssue.mockRestore();
  }
});

// R2: a drag that lands as the last live write of the priority is what Dispatch stores, so a read
// of the issue that reached Dispatch before it, while Dispatch held the pick the drag took over,
// and answers after it does not put that pick back: over its answer the drag's lane shows again,
// on the issue as on the board.
test("a read composed before a drag of the card landed as the last write of its priority answers with the drag's lane, not the pick before it", async () => {
  const pick = Promise.withResolvers<Issue>();
  const drag = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches()
    .mockImplementationOnce(() => pick.promise)
    .mockImplementationOnce(() => drag.promise);
  const client = signedIn();
  let stored: IssuePriority | null = 1;
  const board = renderBoard(client, () => stored);
  const user = userEvent.setup();
  const seen: string[] = [];
  const see = (step: string) =>
    seen.push(`${step}: detail P${cachedPriorities(client).detail} board ${board.shown()}`);
  try {
    await user.selectOptions(prioritySelects()[0] as HTMLSelectElement, "2");
    act(() => {
      void board.moves().moveCardToLane(issue.key, "todo", 0, 0);
    });
    await waitFor(() => expect(writesOf(client).rows()).toHaveLength(2));
    see("pick P2 out, drag to P0");
    stored = 2;
    await act(async () => {
      pick.resolve({ ...issue, priority: 2 });
      await pick.promise;
    });
    await waitFor(() => expect(bodies(patchIssue)).toHaveLength(2));
    see("pick lands, drag out");
    // A read of the issue reaches Dispatch while it holds P2, and answers once the drag landed.
    const answerReads = board.hold();
    const read = client.refetchQueries({ queryKey: detailKey });
    stored = 0;
    await act(async () => {
      drag.resolve({ ...issue, priority: 0 });
      await drag.promise;
    });
    await waitFor(() => expect(writesOf(client).rows()).toEqual([]));
    see("drag lands");
    await act(async () => {
      answerReads();
      await read;
    });
    await waitFor(() => expect(client.isFetching()).toBe(0));
    see("the read composed before the drag landed answers");

    expect(seen).toEqual([
      "pick P2 out, drag to P0: detail P0 board P0",
      "pick lands, drag out: detail P0 board P0",
      "drag lands: detail P0 board P0",
      "the read composed before the drag landed answers: detail P0 board P0",
    ]);
    expect(bodies(patchIssue).map((body) => (body as UpdateIssueInput).priority)).toEqual([2, 0]);
  } finally {
    board.unmount();
    patchIssue.mockRestore();
  }
});

// The drag is refused once the pick has landed: Dispatch holds the pick, which every surface the
// pick's write showed the drag on - the board, the issue, the Inbox - shows again.
test("a refused drag of a card over its pick puts the pick back on every surface", async () => {
  const pick = Promise.withResolvers<Issue>();
  const drag = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches()
    .mockImplementationOnce(() => pick.promise)
    .mockImplementationOnce(() => drag.promise);
  const client = signedIn();
  let stored: IssuePriority | null = 1;
  const board = renderBoard(client, () => stored);
  const user = userEvent.setup();
  try {
    await user.selectOptions(prioritySelects()[0] as HTMLSelectElement, "2");
    act(() => {
      void board.moves().moveCardToLane(issue.key, "todo", 3, 0);
    });
    await waitFor(() => expect(writesOf(client).rows()).toHaveLength(2));
    expect(board.shown()).toBe("P3");
    stored = 2;
    await act(async () => {
      pick.resolve({ ...issue, priority: 2 });
      await pick.promise;
    });
    await waitFor(() => expect(bodies(patchIssue)).toHaveLength(2));
    expect(board.shown()).toBe("P3");

    await act(async () => {
      drag.reject(
        new ApiError(400, { code: "RANK_INPUT", error: "rank neighbors are not ordered" })
      );
      await drag.promise.catch(() => undefined);
    });
    await waitFor(() => expect(writesOf(client).rows()).toEqual([]));
    await waitFor(() => expect(client.isFetching()).toBe(0));
    expect(board.shown()).toBe("P2");
    expect(cachedPriorities(client)).toEqual({ detail: 2, inbox: 2, list: 2 });
  } finally {
    board.unmount();
    patchIssue.mockRestore();
  }
});

// R3: what Dispatch last confirmed is never a drag's lane it has not stored. P2 lands while a drag
// of the card to P0 is out behind it, and the person picks P1 behind the drag: the issue shows P0
// then, which the drag put there, so what Dispatch last confirmed stays P2, the landing's answer.
// Someone closes the issue, and Dispatch refuses the drag, then P1: from P1's refusal until the
// reads it starts answer, every surface shows P2, not P0.
test("a pick made behind a live drag of its card and refused after the drag puts back the value Dispatch last confirmed, not the drag's lane", async () => {
  const pick = Promise.withResolvers<Issue>();
  const drag = Promise.withResolvers<Issue>();
  const again = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches()
    .mockImplementationOnce(() => pick.promise)
    .mockImplementationOnce(() => drag.promise)
    .mockImplementationOnce(() => again.promise);
  const client = signedIn();
  let stored: IssuePriority | null = 1;
  const board = renderBoard(client, () => stored);
  const user = userEvent.setup();
  const seen: string[] = [];
  const see = (step: string) => {
    const { detail, inbox } = cachedPriorities(client);
    seen.push(`${step}: detail P${detail} inbox P${inbox} board ${board.shown()}`);
  };
  try {
    await user.selectOptions(prioritySelects()[0] as HTMLSelectElement, "2");
    act(() => {
      void board.moves().moveCardToLane(issue.key, "todo", 0, 0);
    });
    await waitFor(() => expect(writesOf(client).rows()).toHaveLength(2));
    stored = 2;
    await act(async () => {
      pick.resolve({ ...issue, priority: 2 });
      await pick.promise;
    });
    await waitFor(() => expect(bodies(patchIssue)).toHaveLength(2));
    see("P2 lands, drag to P0 out");
    await user.selectOptions(prioritySelects()[0] as HTMLSelectElement, "1");
    see("P1 picked behind the drag");
    // The reads the refusals start are held until what the refusals put back is checked.
    const answerReads = board.hold();

    await act(async () => {
      drag.reject(closed());
      await drag.promise.catch(() => undefined);
    });
    await waitFor(() => expect(bodies(patchIssue)).toHaveLength(3));
    see("drag refused, P1 out");
    await act(async () => {
      again.reject(closed());
      await again.promise.catch(() => undefined);
    });
    await waitFor(() => expect(writesOf(client).rows()).toEqual([]));
    see("P1 refused, before the reads answer");
    await act(async () => answerReads());
    await waitFor(() => expect(client.isFetching()).toBe(0));
    see("the reads answer");

    expect(seen).toEqual([
      "P2 lands, drag to P0 out: detail P0 inbox P0 board P0",
      "P1 picked behind the drag: detail P1 inbox P1 board P1",
      "drag refused, P1 out: detail P1 inbox P1 board P1",
      "P1 refused, before the reads answer: detail P2 inbox P2 board P2",
      "the reads answer: detail P2 inbox P2 board P2",
    ]);
  } finally {
    board.unmount();
    patchIssue.mockRestore();
  }
});

// R3 on the board page alone, which holds no read of the issue: the card is dragged to P0 while no
// pick is live, and the person picks P2 on the card while the drag is out. The board shows the
// drag's lane then, which Dispatch has not stored, so nothing is confirmed until a read answers.
// Someone closes the issue, and Dispatch refuses the drag, then the pick, before the board's read
// answers: the card never shows P0 again, and shows P1, the priority Dispatch holds, once the
// reads answer.
test("a pick made on a card while its drag is out and refused after the drag never puts back the drag's lane", async () => {
  const drag = Promise.withResolvers<Issue>();
  const pick = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches()
    .mockImplementationOnce(() => drag.promise)
    .mockImplementationOnce(() => pick.promise);
  const client = signedIn();
  client.removeQueries({ queryKey: detailKey });
  client.removeQueries({ queryKey: inboxKey });
  const board = renderBoard(client, () => 1, { onCard: true });
  const user = userEvent.setup();
  const seen: string[] = [];
  const see = (step: string) => seen.push(`${step}: ${board.shown()}`);
  try {
    act(() => {
      void board.moves().moveCardToLane(issue.key, "todo", 0, 0);
    });
    await waitFor(() => expect(bodies(patchIssue)).toHaveLength(1));
    see("drag to P0 out");
    await user.selectOptions(prioritySelects()[0] as HTMLSelectElement, "2");
    see("P2 picked on the card behind it");
    // The reads the refusals start are held until what the refusals put back is checked.
    const answerReads = board.hold();

    await act(async () => {
      drag.reject(closed());
      await drag.promise.catch(() => undefined);
    });
    await waitFor(() => expect(bodies(patchIssue)).toHaveLength(2));
    see("drag refused, P2 out");
    await act(async () => {
      pick.reject(closed());
      await pick.promise.catch(() => undefined);
    });
    await waitFor(() => expect(writesOf(client).rows()).toEqual([]));
    see("P2 refused, before the reads answer");
    await act(async () => answerReads());
    await waitFor(() => expect(client.isFetching()).toBe(0));
    see("the reads answer");

    expect(seen).toEqual([
      "drag to P0 out: P0",
      "P2 picked on the card behind it: P2",
      "drag refused, P2 out: P2",
      "P2 refused, before the reads answer: P2",
      "the reads answer: P1",
    ]);
  } finally {
    board.unmount();
    patchIssue.mockRestore();
  }
});

const neighbour: IssueSummary = { ...summary, key: "CORE-2", rank: "V", title: "Plan the work" };

// A drag of the card made after a pick, which still waits its turn behind another card's move, is
// a newer live write of the priority than the pick: the pick's refusal keeps the drag on screen,
// and is reported with no Retry.
test("a pick refused while a later drag of its card still waits behind another card's move reports no Retry and keeps the card in the drag's lane", async () => {
  const pick = Promise.withResolvers<Issue>();
  const other = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches()
    .mockImplementationOnce(() => pick.promise)
    .mockImplementationOnce(() => other.promise)
    .mockImplementation(stores);
  const client = signedIn();
  const board = renderBoard(client, () => 1, { others: [neighbour] });
  const user = userEvent.setup();
  try {
    await user.selectOptions(prioritySelects()[0] as HTMLSelectElement, "0");
    await waitFor(() => expect(bodies(patchIssue)).toHaveLength(1));
    act(() => {
      void board.moves().moveCardToLane(neighbour.key, "todo", 2, 0);
    });
    await waitFor(() => expect(bodies(patchIssue)).toHaveLength(2));
    act(() => {
      void board.moves().moveCardToLane(issue.key, "todo", 3, 0);
    });
    // The drag waits in the board's scope, so it is not in the store.
    expect(
      writesOf(client)
        .rows()
        .map((row) => row.label)
    ).toEqual(["CORE-1 set to P0", "CORE-2 moved on the board"]);
    expect(board.shown()).toBe("P3");

    await act(async () => {
      pick.reject(closed());
      await pick.promise.catch(() => undefined);
    });
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("Could not update the priority of CORE-1.");
    expect(within(alert).queryByRole("button", { name: "Retry" })).not.toBeInTheDocument();
    expect(board.shown()).toBe("P3");
    expect(cachedPriorities(client)).toMatchObject({ detail: 3, inbox: 3 });

    // The other card's move lands, and the drag goes out and lands: the report ends with it.
    await act(async () => {
      other.resolve({ ...issue, key: neighbour.key });
      await other.promise;
    });
    await waitFor(() => expect(writesOf(client).rows()).toEqual([]));
    await waitFor(() => expect(screen.queryByRole("alert")).not.toBeInTheDocument());
    expect(bodies(patchIssue).map((body) => (body as UpdateIssueInput).priority)).toEqual([
      0, 2, 3,
    ]);
  } finally {
    board.unmount();
    patchIssue.mockRestore();
  }
});
