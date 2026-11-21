import { expect, jest, mock, spyOn, test } from "bun:test";
import { focusManager, QueryClientProvider, useQuery } from "@tanstack/react-query";
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

import { type Commits, drawnPick, recordCommits } from "../../__tests__/commits";
import { jump, realClock, tick } from "../../__tests__/fake-clock";
import {
  bodies,
  details,
  issue,
  pick,
  pickerTriggers,
  pickIn,
  pick as pickPriority,
  pickRepo,
  repoSuggestions,
  shownBy,
  shownValue,
  signedInHolding,
} from "../../__tests__/issue-picks";
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { panelOf, pickNamed, rows } from "../../__tests__/list-picks";
import { choiceOf } from "../../__tests__/menu-picks";
import { answerIssuePatches } from "../../__tests__/send-answers";
import { userOnFakeClock } from "../../__tests__/user";
import { ApiError, api, issuePatchRequest } from "../../api/client";
import { mergeIssue } from "../../api/issue-cache";
import { issueQuery, projectIssuesQuery } from "../../api/queries";
import type { DispatchQueryClient } from "../../api/query-client";
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
import { MAX_BACKOFF_MS, SEND_DEADLINE_MS, stoppedWriteNote } from "../writes/keyed-write";
import { PendingWritesIndicator } from "../writes/PendingWritesIndicator";
import { KEPT_SEND_RESUME_MS, resetWriteStorage } from "../writes/write-storage";
import { plainWrite, type Writes, writesOf } from "../writes/write-store";
import { AssigneeControl } from "./AssigneeControl";
import { issuePriority } from "./issue-priority";
import { PriorityControl } from "./PriorityControl";
import { RepoControl } from "./RepoControl";
import { useIssueFieldWrite } from "./useIssueFieldWrite";

// The rows of dispatch://INFRA-299: every pick of a priority, an assignee or a repository joins
// the page's write store as it is made, on its issue's line, and the pick's write - not the
// picker - keeps it on screen over every answer until Dispatch has stored it.

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

/** What Dispatch stores of CORE-1 that a pick or a read of it carries. */
type Stored = Pick<Issue, "assignee" | "priority" | "updated_at">;

/** Dispatch's side of the three reads a pick shows on: each read is composed as it reaches
 *  Dispatch, from what Dispatch stores then, and answers once the row lets the reads it holds go
 *  (`hold`). */
interface Reads {
  readonly detail: () => Promise<IssueDetails>;
  readonly inbox: () => Promise<InboxRow[]>;
  readonly list: () => Promise<IssueSummary[]>;
  /** Holds every read that starts from now on until the returned function lets them answer. */
  readonly hold: () => () => void;
  /** What Dispatch stores from now on: a save landed - the priority, and the assignee and the
   *  moment of the change (`updated_at`) where `change` names them. */
  readonly store: (
    priority: IssuePriority | null,
    change?: Partial<Pick<Stored, "assignee" | "updated_at">>
  ) => void;
}

function dispatchReads(): Reads {
  let stored: Stored = {
    assignee: issue.assignee,
    priority: issuePriority(issue.priority),
    updated_at: issue.updated_at,
  };
  let gate: Promise<void> | undefined;
  const read =
    <T,>(compose: (now: Stored) => T) =>
    async (): Promise<T> => {
      const answer = compose(stored);
      await gate;
      return answer;
    };
  return {
    detail: read((now): IssueDetails => ({ ...details, ...now })),
    inbox: read((now): InboxRow[] => [
      {
        ...inboxRow,
        issue: { assignee: now.assignee, key: issue.key, title: issue.title },
        priority: now.priority,
        waiting_on_person: now.assignee ?? undefined,
      },
    ]),
    list: read((now): IssueSummary[] => [{ ...summary, ...now }]),
    hold: () => {
      const held = Promise.withResolvers<void>();
      gate = held.promise;
      return () => held.resolve();
    },
    store: (priority, change) => {
      stored = { ...stored, ...change, priority };
    },
  };
}

/** The page's query client, signed in, holding CORE-1 as Dispatch last answered it on its detail,
 *  its project's list and the Inbox. */
function signedIn(): DispatchQueryClient {
  return signedInHolding([detailKey, details], [listKey, [summary]], [inboxKey, [inboxRow]]);
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

/** Every trigger of CORE-1's priority menu on the screen, in its order. */
function priorityTriggers(): HTMLElement[] {
  return pickerTriggers("Priority of CORE-1");
}

/** A save Dispatch answers by storing the change: its answer is the issue with it. */
const stores = (_key: string, input: UpdateIssueInput): Promise<Issue> =>
  Promise.resolve({ ...issue, ...input } as Issue);

test("each pick joins the issue's line in the write store when it is made", async () => {
  const patchIssue = answerIssuePatches().mockImplementation(
    () => Promise.withResolvers<Issue>().promise
  );
  const client = signedIn();
  const view = renderIn(client, <CachedPriority reads={dispatchReads()} />);
  const user = userEvent.setup();
  try {
    await pick(user, priorityTriggers()[0] as HTMLElement, "0");
    await pick(user, priorityTriggers()[0] as HTMLElement, "3");
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
    const select = () => priorityTriggers()[0] as HTMLElement;
    await pick(user, select(), "0");
    await pick(user, select(), "3");
    await pick(user, select(), "2");
    expect(
      writesOf(client)
        .rows()
        .map((row) => [row.label, row.status])
    ).toEqual([
      ["CORE-1 set to P0", "sending"],
      ["CORE-1 set to P2", "queued"],
    ]);
    // The pick taken out puts nothing back: every cache shows the newest.
    expect(shownValue(select())).toBe("2");
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
    expect(shownValue(select())).toBe("2");
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
    const [row, pane] = priorityTriggers() as [HTMLElement, HTMLElement];
    await pick(user, row, "0");
    await pick(user, pane, "3");
    await pick(user, row, "2");
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
    expect(priorityTriggers().map(shownValue)).toEqual(["2", "2"]);
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
  const assignee = renderHook(() => useIssueFieldWrite(issue.key, "assignee", issue.assignee), {
    wrapper,
  });
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
    const select = () => priorityTriggers()[0] as HTMLElement;
    await pick(user, select(), "0");
    await pick(user, select(), "3");
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
    await pick(user, select(), "1");
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
    expect(shownValue(select())).toBe("1");
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
  const trigger = () => screen.getByRole("button", { name: pickNamed("Assignee of CORE-1") });
  try {
    // The people are read once the reader reaches for the control, and its list, opened, holds
    // them.
    await user.hover(trigger());
    await user.click(trigger());
    await waitFor(() => expect(rows(panelOf(trigger())).length).toBe(4));
    await pickIn(user, trigger, "bob");
    await waitFor(() => expect(client.getQueryData<IssueDetails>(detailKey)?.last_seq).toBe(2));
    // The landing has the issue read again, as Dispatch now holds it.
    await waitFor(() => expect(detailRead).toHaveBeenCalledTimes(1));
    await pickIn(user, trigger, "carol");
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent('"carol" has not signed in to Dispatch');
    expect(within(alert).getByRole("button", { name: "Retry" })).toBeTruthy();
    // So does the refusal.
    await waitFor(() => expect(detailRead).toHaveBeenCalledTimes(2));
    // Back on the assignee Dispatch stored last, which the landing answered: not the one the
    // issue had before the first pick.
    expect(shownBy(trigger())).toBe("bob");
    expect(client.getQueryData<IssueSummary[]>(listKey)?.[0]?.assignee).toBe("bob");
    const [row] = client.getQueryData<InboxRow[]>(inboxKey) ?? [];
    expect([row?.issue?.assignee, row?.waiting_on_person]).toEqual(["bob", "bob"]);

    await user.click(within(alert).getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(3));
    expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { assignee: "carol" }, keyedAttempt);
    await waitFor(() => expect(screen.queryByRole("alert")).not.toBeInTheDocument());
    expect(shownBy(trigger())).toBe("carol");
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
    const select = () => priorityTriggers()[0] as HTMLElement;
    await pick(user, select(), "0");
    await pick(user, select(), "2");
    await act(async () => {
      first.reject(new ApiError(409, { code: "ISSUE_CLOSED", error: "CORE-1 is closed" }));
      await first.promise.catch(() => undefined);
    });
    await waitFor(() => expect(bodies(patchIssue)).toEqual([{ priority: 0 }, { priority: 2 }]));
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("Could not update the priority of CORE-1.");
    expect(within(alert).queryByRole("button", { name: "Retry" })).not.toBeInTheDocument();
    expect(shownValue(select())).toBe("2");
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
    expect(shownValue(select())).toBe("2");
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
    const select = () => priorityTriggers()[0] as HTMLElement;
    await pick(user, select(), "0");
    await pick(user, select(), "2");
    await act(async () => {
      first.reject(new ApiError(409, { code: "ISSUE_CLOSED", error: "CORE-1 is closed" }));
      await first.promise.catch(() => undefined);
    });
    await waitFor(() => expect(bodies(patchIssue)).toEqual([{ priority: 0 }, { priority: 2 }]));
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("Could not update the priority of CORE-1.");
    expect(within(alert).queryByRole("button", { name: "Retry" })).not.toBeInTheDocument();

    // P2 is still out when the person picks P3: the report clears with the press.
    await pick(user, select(), "3");
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    expect(shownValue(select())).toBe("3");
    expect(cachedPriorities(client)).toEqual({ detail: 3, inbox: 3, list: 3 });

    reads.store(3);
    await act(async () => {
      second.resolve({ ...issue, priority: 2 });
      await second.promise;
    });
    await waitFor(() => expect(writesOf(client).rows()).toEqual([]));
    expect(bodies(patchIssue)).toEqual([{ priority: 0 }, { priority: 2 }, { priority: 3 }]);
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    expect(shownValue(select())).toBe("3");
  } finally {
    view.unmount();
    patchIssue.mockRestore();
  }
});

// "Until the person picks again" holds whatever picker they pick in: a pick of the field in
// another picker of it - the Inbox drawer's row over the issue page - ends the report too.
test("an older pick refused while a newer one is live reports the refusal with no Retry until the person picks again in another picker of the field", async () => {
  const first = Promise.withResolvers<Issue>();
  const second = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches()
    .mockImplementationOnce(() => first.promise)
    .mockImplementationOnce(() => second.promise)
    .mockImplementation(stores);
  const client = signedIn();
  const reads = dispatchReads();
  const view = renderIn(
    client,
    <>
      <section aria-label="Issue page">
        <CachedPriority reads={reads} />
      </section>
      <section aria-label="Inbox drawer">
        <CachedPriority reads={reads} />
      </section>
    </>
  );
  const user = userEvent.setup();
  try {
    const page = () => priorityTriggers()[0] as HTMLElement;
    const drawer = () => priorityTriggers()[1] as HTMLElement;
    await pick(user, page(), "0");
    await pick(user, page(), "2");
    await act(async () => {
      first.reject(closed());
      await first.promise.catch(() => undefined);
    });
    await waitFor(() => expect(bodies(patchIssue)).toEqual([{ priority: 0 }, { priority: 2 }]));
    const alert = await within(screen.getByRole("region", { name: "Issue page" })).findByRole(
      "alert"
    );
    expect(alert).toHaveTextContent("Could not update the priority of CORE-1.");
    expect(within(alert).queryByRole("button", { name: "Retry" })).not.toBeInTheDocument();

    // P2 is still out when the person picks P3 in the drawer's row: the issue page's report ends.
    await pick(user, drawer(), "3");
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    expect(priorityTriggers().map(shownValue)).toEqual(["3", "3"]);
    expect(cachedPriorities(client)).toEqual({ detail: 3, inbox: 3, list: 3 });

    reads.store(3);
    await act(async () => {
      second.resolve({ ...issue, priority: 2 });
      await second.promise;
    });
    await waitFor(() => expect(writesOf(client).rows()).toEqual([]));
    expect(bodies(patchIssue)).toEqual([{ priority: 0 }, { priority: 2 }, { priority: 3 }]);
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
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
    const select = () => priorityTriggers()[0] as HTMLElement;
    await pick(user, select(), "0");
    saveStatus(client);
    await pick(user, select(), "2");
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
    expect(shownValue(select())).toBe("2");

    reads.store(2);
    await act(async () => {
      newer.resolve({ ...issue, priority: 2, status: "done" });
      await newer.promise;
    });
    await waitFor(() => expect(screen.queryByRole("alert")).not.toBeInTheDocument());
    expect(shownValue(select())).toBe("2");
  } finally {
    view.unmount();
    patchIssue.mockRestore();
  }
});

// A refused save of the field of another kind - the priority set from the issues table - that the
// reader retries from the Pending changes list while an older pick is out joins the back of the
// line, behind the pick: from its Retry on it is a newer live write of the field. Its landing ends
// the pick's refusal reported with no Retry, as a newer pick's does.
test("an older pick's refusal reported with no Retry ends once a save of the field retried before that refusal lands", async () => {
  const table = Promise.withResolvers<Issue>();
  const first = Promise.withResolvers<Issue>();
  const again = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches()
    .mockImplementationOnce(() => table.promise)
    .mockImplementationOnce(() => first.promise)
    .mockImplementationOnce(() => again.promise);
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
    const select = () => priorityTriggers()[0] as HTMLElement;
    // P3 from the table goes out first, and the pick of P0 waits behind it.
    act(() => {
      void writesOf(client)
        .run({
          detail: null,
          kind: plainWrite,
          label: "CORE-1 set to P3 from the issues table",
          presentation: "indicator",
          request: issuePatchRequest(issue.key, { priority: 3 }),
          target: `issue:${issue.key}`,
        })
        .catch(() => undefined);
    });
    await pick(user, select(), "0");
    await act(async () => {
      table.reject(closed());
      await table.promise.catch(() => undefined);
    });
    await waitFor(() => expect(bodies(patchIssue)).toEqual([{ priority: 3 }, { priority: 0 }]));
    // P0 is out when the reader retries P3: it joins the line again, behind P0.
    const pending = within(screen.getByTestId("pending-writes"));
    await user.click(pending.getByRole("button", { name: "Retry" }));
    await act(async () => {
      first.reject(closed());
      await first.promise.catch(() => undefined);
    });
    await waitFor(() =>
      expect(bodies(patchIssue)).toEqual([{ priority: 3 }, { priority: 0 }, { priority: 3 }])
    );
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("Could not update the priority of CORE-1.");
    expect(within(alert).queryByRole("button", { name: "Retry" })).not.toBeInTheDocument();
    expect(shownValue(select())).toBe("3");

    reads.store(3);
    await act(async () => {
      again.resolve({ ...issue, priority: 3 });
      await again.promise;
    });
    await waitFor(() => expect(writesOf(client).rows()).toEqual([]));
    // Dispatch holds P3, saved after the refused pick: the refusal has nothing left to report.
    await waitFor(() => expect(screen.queryByRole("alert")).not.toBeInTheDocument());
    expect(shownValue(select())).toBe("3");
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
    const [page, drawer] = priorityTriggers() as [HTMLElement, HTMLElement];
    await pick(user, page, "0");
    await pick(user, drawer, "2");
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
    const [page, drawer] = priorityTriggers() as [HTMLElement, HTMLElement];
    await pick(user, page, "0");
    saveStatus(client);
    await pick(user, drawer, "2");
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

/** Makes each of `picks`, a priority, in a page whose first save never answers, and reloads: the
 *  page that made them goes (its kept writes with it), and the next one finds what the tab kept,
 *  as pressed `pressedAgo` before the reload, and takes it up signed in as the same person. */
async function pickAndReload(picks: readonly string[], pressedAgo: number) {
  const client = signedIn();
  const view = renderIn(client, <CachedPriority reads={dispatchReads()} />);
  const user = userEvent.setup();
  for (const value of picks) {
    await pick(user, priorityTriggers()[0] as HTMLElement, value);
  }
  view.unmount();
  const kept = keptWrites();
  resetWriteStorage();
  for (const [name, value] of kept) {
    const write = JSON.parse(value) as { pressedAt: number };
    const pressedAt = write.pressedAt - pressedAgo;
    window.sessionStorage.setItem(name, JSON.stringify({ ...write, pressedAt }));
  }
  const after = signedInHolding();
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
    const after = await pickAndReload(["0", "3"], 0);
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
    const after = await pickAndReload(["0", "3"], KEPT_SEND_RESUME_MS + 1_000);
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
    const after = await pickAndReload(["0", "3"], 0);
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

/** The row of the write labelled `label` in the shell's Pending changes list. */
function pendingRow(label: string) {
  const row = within(screen.getByTestId("pending-writes"))
    .getByText(label, { exact: false })
    .closest("li");
  if (row === null) throw new Error(`the Pending changes list names no ${label}`);
  return within(row);
}

// R3: a pick puts a value back only if the page showed it. A pick a reload kept for more than
// five minutes stands refused in the Pending changes list and never joined its line, so nothing
// showed it. A later pick of P2 lands, and another person's P0 is read. The reader then discards
// the kept pick: the page keeps P0, Dispatch's newest answer, and reads nothing again.
test("a pick a reload kept refused, which the page never showed, puts nothing back when it is discarded: the page keeps Dispatch's newest answer", async () => {
  const patchIssue = answerIssuePatches()
    .mockImplementationOnce(() => Promise.withResolvers<Issue>().promise)
    .mockImplementation(stores);
  let view: RenderResult | undefined;
  try {
    const after = await pickAndReload(["3"], KEPT_SEND_RESUME_MS + 1_000);
    const reads = dispatchReads();
    view = renderIn(
      after,
      <>
        <Surfaces reads={reads} />
        <CachedPriority reads={reads} />
        <PendingWritesIndicator />
      </>
    );
    const user = userEvent.setup();
    const line = () =>
      writesOf(after)
        .rows()
        .map((row) => [row.label, row.status]);
    await waitFor(() =>
      expect(shownPriorities()).toEqual({ detail: "P1", inbox: "P1", list: "P1" })
    );
    expect(line()).toEqual([["CORE-1 set to P3", "refused"]]);
    // A later pick of P2 lands, and the app reads the issue, the list and the Inbox again.
    reads.store(2);
    await pick(user, priorityTriggers()[0] as HTMLElement, "2");
    await waitFor(() => expect(bodies(patchIssue)).toEqual([{ priority: 3 }, { priority: 2 }]));
    await waitFor(() => expect(line()).toEqual([["CORE-1 set to P3", "refused"]]));
    await waitFor(() => expect(after.isFetching()).toBe(0));
    expect(shownPriorities()).toEqual({ detail: "P2", inbox: "P2", list: "P2" });
    // Another person sets P0, and every surface reads it.
    reads.store(0);
    await readAll(after, reads);
    expect(shownPriorities()).toEqual({ detail: "P0", inbox: "P0", list: "P0" });
    // A read the Discard started would be held, so what shows is what the Discard left.
    const answerReads = reads.hold();

    await user.click(pendingRow("CORE-1 set to P3").getByRole("button", { name: "Discard" }));
    await waitFor(() => expect(writesOf(after).rows()).toEqual([]));
    expect(cachedPriorities(after)).toEqual({ detail: 0, inbox: 0, list: 0 });
    expect(shownPriorities()).toEqual({ detail: "P0", inbox: "P0", list: "P0" });
    // Nothing was put back, so nothing is read again.
    expect(after.isFetching()).toBe(0);
    answerReads();
  } finally {
    view?.unmount();
    patchIssue.mockRestore();
  }
});

// R3: a refusal puts a value back once. A pick a reload kept refused is retried from the Pending
// changes list: it joins its line and shows, Dispatch refuses it, and the value Dispatch last
// confirmed is put back. Another person's P0 is then read. The pick still stands refused in the
// list, and the reader discards it: that puts nothing back a second time, so the page keeps P0
// and reads nothing again.
test("a pick a reload kept, retried and refused again, puts nothing back a second time when it is then discarded", async () => {
  const again = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches()
    .mockImplementationOnce(() => Promise.withResolvers<Issue>().promise)
    .mockImplementationOnce(() => again.promise);
  let view: RenderResult | undefined;
  try {
    const after = await pickAndReload(["3"], KEPT_SEND_RESUME_MS + 1_000);
    const reads = dispatchReads();
    view = renderIn(
      after,
      <>
        <Surfaces reads={reads} />
        <PendingWritesIndicator />
      </>
    );
    const user = userEvent.setup();
    await waitFor(() =>
      expect(shownPriorities()).toEqual({ detail: "P1", inbox: "P1", list: "P1" })
    );
    // Retried, the pick shows; refused, it puts back P1 and the page reads again.
    await user.click(pendingRow("CORE-1 set to P3").getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(bodies(patchIssue)).toEqual([{ priority: 3 }, { priority: 3 }]));
    expect(shownPriorities()).toEqual({ detail: "P3", inbox: "P3", list: "P3" });
    await act(async () => {
      again.reject(closed());
      await again.promise.catch(() => undefined);
    });
    await waitFor(() =>
      expect(
        writesOf(after)
          .rows()
          .map((row) => row.status)
      ).toEqual(["refused"])
    );
    await waitFor(() => expect(after.isFetching()).toBe(0));
    expect(shownPriorities()).toEqual({ detail: "P1", inbox: "P1", list: "P1" });
    // Another person sets P0, and every surface reads it.
    reads.store(0);
    await readAll(after, reads);
    expect(shownPriorities()).toEqual({ detail: "P0", inbox: "P0", list: "P0" });
    // A read the Discard started would be held, so what shows is what the Discard left.
    const answerReads = reads.hold();

    await user.click(pendingRow("CORE-1 set to P3").getByRole("button", { name: "Discard" }));
    await waitFor(() => expect(writesOf(after).rows()).toEqual([]));
    expect(cachedPriorities(after)).toEqual({ detail: 0, inbox: 0, list: 0 });
    expect(shownPriorities()).toEqual({ detail: "P0", inbox: "P0", list: "P0" });
    expect(after.isFetching()).toBe(0);
    answerReads();
  } finally {
    view?.unmount();
    patchIssue.mockRestore();
  }
});

// R3: a refusal reported with no Retry stands until a newer write of the field lands, and a
// write is as new as its last place in the line. A pick of P3 a reload kept stands refused in the
// Pending changes list. P0 is picked and refused while P2, picked after it, is live: reported,
// with no Retry. The reader retries the kept P3, which joins the back of the line, behind P2;
// P2 is refused, and P3 goes out, lands and is stored. P3 is the newest write of the priority and
// Dispatch holds it, so the report ends.
test("a refusal reported with no Retry ends once a kept pick the reader retried after it lands", async () => {
  const older = Promise.withResolvers<Issue>();
  const newer = Promise.withResolvers<Issue>();
  const retried = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches()
    .mockImplementationOnce(() => Promise.withResolvers<Issue>().promise)
    .mockImplementationOnce(() => older.promise)
    .mockImplementationOnce(() => newer.promise)
    .mockImplementationOnce(() => retried.promise);
  let view: RenderResult | undefined;
  try {
    const after = await pickAndReload(["3"], KEPT_SEND_RESUME_MS + 1_000);
    const reads = dispatchReads();
    view = renderIn(
      after,
      <>
        <CachedPriority reads={reads} />
        <PendingWritesIndicator />
      </>
    );
    const user = userEvent.setup();
    await waitFor(() => expect(priorityTriggers()).toHaveLength(1));
    const select = () => priorityTriggers()[0] as HTMLElement;
    await pick(user, select(), "0");
    await pick(user, select(), "2");
    await act(async () => {
      older.reject(closed());
      await older.promise.catch(() => undefined);
    });
    await waitFor(() => expect(bodies(patchIssue)).toHaveLength(3));
    const reported = await screen.findByRole("alert");
    expect(within(reported).queryByRole("button", { name: "Retry" })).not.toBeInTheDocument();

    // P2 is out when the reader retries the kept P3: it joins the line again, behind P2.
    await user.click(pendingRow("CORE-1 set to P3").getByRole("button", { name: "Retry" }));
    expect(
      writesOf(after)
        .rows()
        .map((row) => [row.label, row.status])
    ).toEqual([
      ["CORE-1 set to P3", "queued"],
      ["CORE-1 set to P2", "sending"],
    ]);
    expect(shownValue(select())).toBe("3");
    await act(async () => {
      newer.reject(closed());
      await newer.promise.catch(() => undefined);
    });
    await waitFor(() =>
      expect(bodies(patchIssue)).toEqual([
        { priority: 3 },
        { priority: 0 },
        { priority: 2 },
        { priority: 3 },
      ])
    );
    // P3 is live behind the refused P2: the refusal is still reported with no Retry.
    expect(
      within(screen.getByRole("alert")).queryByRole("button", { name: "Retry" })
    ).not.toBeInTheDocument();
    expect(shownValue(select())).toBe("3");

    reads.store(3);
    await act(async () => {
      retried.resolve({ ...issue, priority: 3 });
      await retried.promise;
    });
    await waitFor(() => expect(writesOf(after).rows()).toEqual([]));
    // Dispatch holds P3, the newest write of the priority: the refusal has nothing left to report.
    await waitFor(() => expect(screen.queryByRole("alert")).not.toBeInTheDocument());
    expect(shownValue(select())).toBe("3");
  } finally {
    view?.unmount();
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
  const picker = renderHook(
    () => useIssueFieldWrite(issue.key, "priority", issuePriority(issue.priority)),
    {
      wrapper: ({ children }) => (
        <QueryClientProvider client={client}>{children}</QueryClientProvider>
      ),
    }
  );
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

/** A pick of P0 out on a page that read CORE-1 at P1, whose writes `drop` then has the store
 *  drop, with every read sent from then on held: what the store holds, and what each cache and
 *  each surface shows, right after the drop. */
async function dropPick(drop: (writes: Writes) => void) {
  const patchIssue = answerIssuePatches().mockImplementation(
    () => Promise.withResolvers<Issue>().promise
  );
  const client = signedInHolding();
  const writes = writesOf(client);
  act(() => writes.signedIn("alice"));
  const reads = dispatchReads();
  const view = renderIn(
    client,
    <>
      <Surfaces reads={reads} />
      <CachedPriority reads={reads} />
    </>
  );
  const user = userEvent.setup();
  try {
    await waitFor(() =>
      expect(shownPriorities()).toEqual({ detail: "P1", inbox: "P1", list: "P1" })
    );
    await pick(user, priorityTriggers()[0] as HTMLElement, "0");
    await waitFor(() => expect(bodies(patchIssue)).toEqual([{ priority: 0 }]));
    expect(shownPriorities()).toEqual({ detail: "P0", inbox: "P0", list: "P0" });
    const answerReads = reads.hold();
    await act(async () => drop(writes));
    const left = {
      cached: cachedPriorities(client),
      held: writes.rows().length,
      shown: shownPriorities(),
    };
    answerReads();
    return left;
  } finally {
    view.unmount();
    patchIssue.mockRestore();
  }
}

/** What a dropped pick of P0 leaves on a page that read CORE-1 at P1, before any read answers. */
const putBackAtOnce = {
  cached: { detail: 1, inbox: 1, list: 1 },
  held: 0,
  shown: { detail: "P1", inbox: "P1", list: "P1" },
};

// R3: a dropped pick puts back the value Dispatch last confirmed, as a refused one does. Another
// person signs in on the page while P0 is out: the store drops the pick, and the page shows P1,
// which Dispatch answered the person before, from the drop on - not P0 until a read answers. The
// store lets go of what Dispatch answered that person only after it has dropped their writes.
test("a pick dropped as another person signs in on the page puts back the priority Dispatch last confirmed at once, before any read answers", async () => {
  expect(await dropPick((writes) => writes.signedIn("bob"))).toEqual(putBackAtOnce);
});

// The same where a lapse drops the pick: its person's sign-out in another tab held their writes
// here, and the lapse is that sign-out landing (`Writes.dropLogin`).
test("a pick dropped as its person's sign-out in another tab lands puts back the priority Dispatch last confirmed at once, before any read answers", async () => {
  const lapse = (writes: Writes) => {
    writes.holdFor("alice");
    writes.signedIn(undefined);
  };
  expect(await dropPick(lapse)).toEqual(putBackAtOnce);
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
    await pick(user, priorityTriggers()[0] as HTMLElement, "2");
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
    await pick(user, priorityTriggers()[0] as HTMLElement, "2");
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

// A read held at a landing that the page then cancels (`cancelQueries`, as a board's drop, the
// Inbox's writes and a components save do) is given up: its answer never lands, and the list
// keeps the landed value. The watch of the client's picks lives as long as the client does, so
// the list's next read, sent after the landing, answers for itself: with P3, which another person
// set since. No row sees the cancel end the read's hold: the next read ends it as it is sent, so
// the hold's end at the cancel (`givenUp` in `answered`) frees its memory and nothing more.
test("a read held at a landing that is then cancelled leaves the list at the landed value, and its next read answers for itself", async () => {
  const save = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches().mockImplementation(() => save.promise);
  const client = signedIn();
  const reads = dispatchReads();
  const view = renderIn(client, <CachedPriority reads={reads} />);
  const user = userEvent.setup();
  const answerReads = reads.hold();
  try {
    await pick(user, priorityTriggers()[0] as HTMLElement, "2");
    await waitFor(() => expect(bodies(patchIssue)).toEqual([{ priority: 2 }]));
    // A read of the project's list reaches Dispatch before the save does, and is out when it
    // lands: the landing holds it.
    void client.prefetchQuery({ queryFn: reads.list, queryKey: listKey });
    reads.store(2);
    await act(async () => {
      save.resolve({ ...issue, priority: 2 });
      await save.promise;
    });
    await waitFor(() => expect(writesOf(client).rows()).toEqual([]));

    await act(() => client.cancelQueries({ queryKey: listKey }));
    expect(client.getQueryState(listKey)?.fetchStatus).toBe("idle");
    expect(cachedPriorities(client).list).toBe(2);

    reads.store(3);
    answerReads();
    await act(() => client.fetchQuery({ queryFn: reads.list, queryKey: listKey }));
    expect(cachedPriorities(client).list).toBe(3);
  } finally {
    answerReads();
    view.unmount();
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
    await pick(user, priorityTriggers()[0] as HTMLElement, "2");
    await waitFor(() => expect(bodies(patchIssue)).toEqual([{ priority: 2 }]));
    await pick(user, priorityTriggers()[0] as HTMLElement, "0");
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

// R2 and R3: once the last live write of the field ends, every page shows the value Dispatch
// stored, and a read sent before a write of the field landed is older than that write's answer,
// whenever it arrives. P2 lands while P0, picked after it, is live, and reads of the issue, the
// project's list and the Inbox sent before the landing are out. P0 is refused before they answer,
// and P2 is put back. Their answers then land with P1, from before P2, and every surface keeps P2:
// the list and the Inbox, which no rule of their own keeps from an older answer, as the issue,
// whose query keeps the landing's newer answer (`issueQuery`).
test("a read sent before a pick landed that answers after a later pick's refusal does not show the value from before the landing", async () => {
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
  const landedAt = "2026-09-09T00:01:00Z";
  const seen: string[] = [];
  const see = (step: string) => {
    const { detail, inbox, list } = shownPriorities();
    seen.push(`${step}: detail ${detail} list ${list} inbox ${inbox}`);
  };
  try {
    await pick(user, priorityTriggers()[0] as HTMLElement, "2");
    await waitFor(() => expect(bodies(patchIssue)).toEqual([{ priority: 2 }]));
    await pick(user, priorityTriggers()[0] as HTMLElement, "0");
    // Three reads reach Dispatch before the save of P2 does.
    const answerEarlyReads = reads.hold();
    const early = Promise.all(
      [detailKey, listKey, inboxKey].map((queryKey) => client.refetchQueries({ queryKey }))
    );
    // Every read sent from now on - those the refusal starts - waits.
    const answerLaterReads = reads.hold();
    reads.store(2, { updated_at: landedAt });
    await act(async () => {
      save.resolve({ ...issue, priority: 2, updated_at: landedAt });
      await save.promise;
    });
    await waitFor(() => expect(bodies(patchIssue)).toEqual([{ priority: 2 }, { priority: 0 }]));
    see("P2 lands while P0 is live");

    await act(async () => {
      refusal.reject(closed());
      await refusal.promise.catch(() => undefined);
    });
    await waitFor(() => expect(writesOf(client).rows()).toEqual([]));
    see("P0 refused, before the reads answer");
    await act(async () => {
      answerEarlyReads();
      await early;
    });
    see("the reads sent before the landing answer P1");
    await act(async () => answerLaterReads());
    await waitFor(() => expect(client.isFetching()).toBe(0));
    see("the reads the refusal started answer");

    expect(seen).toEqual([
      "P2 lands while P0 is live: detail P0 list P0 inbox P0",
      "P0 refused, before the reads answer: detail P2 list P2 inbox P2",
      "the reads sent before the landing answer P1: detail P2 list P2 inbox P2",
      "the reads the refusal started answer: detail P2 list P2 inbox P2",
    ]);
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
    await pick(user, priorityTriggers()[0] as HTMLElement, "0");
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("Could not update the priority of CORE-1.");
    expect(within(alert).getByRole("button", { name: "Retry" })).toBeTruthy();
    expect(cachedPriorities(client)).toEqual({ detail: 1, inbox: 1, list: 1 });
    expect(shownValue(priorityTriggers()[0] as HTMLElement)).toBe("1");
    // The issue is read again, as Dispatch holds it.
    await waitFor(() => expect(detailRead).toHaveBeenCalledTimes(1));
    expect(shownValue(priorityTriggers()[0] as HTMLElement)).toBe("1");
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
    const select = () => priorityTriggers()[0] as HTMLElement;
    await pick(user, select(), "1");
    await pick(user, select(), "2");
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
    expect(shownValue(select())).toBe("2");
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
    expect(shownValue(select())).toBe("3");
    expect(cachedPriorities(client)).toEqual({ detail: 3, inbox: 3, list: 3 });
    // The issue is read again, as Dispatch holds it.
    await waitFor(() => expect(detailRead.mock.calls.length).toBeGreaterThan(readsBefore));
    await act(async () => answerReads());
    expect(shownValue(select())).toBe("3");
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
    const select = () => priorityTriggers()[0] as HTMLElement;
    await pick(user, select(), "1");
    await waitFor(() => expect(bodies(patchIssue)).toEqual([{ priority: 1 }]));
    // Another person sets P0, and a read of the issue answers with it while P1 is out: P1 stays
    // shown over it.
    reads.store(0);
    await act(() => client.refetchQueries({ queryKey: detailKey }));
    expect(shownValue(select())).toBe("1");
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
    expect(shownValue(select())).toBe("0");
    expect(cachedPriorities(client)).toEqual({ detail: 0, inbox: 0, list: 0 });
    await waitFor(() => expect(detailRead.mock.calls.length).toBe(readsBefore + 1));
    expect(shownValue(select())).toBe("0");

    await act(async () => answerReads());
    await waitFor(() => expect(shownValue(select())).toBe("2"));
    expect(client.getQueryData<IssueDetails>(detailKey)?.priority).toBe(2);
    expect(detailRead.mock.calls.length).toBe(readsBefore + 1);
  } finally {
    view.unmount();
    patchIssue.mockRestore();
  }
});

// R3: the put-back is the field in the newest answer Dispatch gave, and of two reads that each
// hold the issue the newer is the one whose request went out last - whichever cache it filled,
// and whichever of the two answered first since the page began keeping answers. Each order is a
// row: a put-back that takes the first answer it keeps, or that ranks one cache over another,
// fails one of the two.
for (const [older, newer] of [
  ["detail", "list"],
  ["list", "detail"],
] as const) {
  test(`a refused pick puts back the priority of the newer of two reads of the issue: the ${newer}'s, read again after the ${older}'s`, async () => {
    const save = Promise.withResolvers<Issue>();
    const patchIssue = answerIssuePatches().mockImplementationOnce(() => save.promise);
    const client = signedInHolding();
    const reads = dispatchReads();
    reads.store(3);
    const keys = { detail: detailKey, list: listKey } as const;
    const view = renderIn(
      client,
      <>
        <Surfaces reads={reads} />
        <CachedPriority reads={reads} />
      </>
    );
    const user = userEvent.setup();
    try {
      await waitFor(() =>
        expect(shownPriorities()).toEqual({ detail: "P3", inbox: "P3", list: "P3" })
      );
      // Both reads go out while the page keeps every answer: the older at P3, the newer at P1.
      await act(() => client.refetchQueries({ queryKey: keys[older] }));
      reads.store(1);
      await act(() => client.refetchQueries({ queryKey: keys[newer] }));
      await waitFor(() => expect(shownPriorities()[newer]).toBe("P1"));
      expect(shownPriorities()[older], `the ${older} read before Dispatch stored P1`).toBe("P3");

      await pick(user, priorityTriggers()[0] as HTMLElement, "0");
      await waitFor(() => expect(bodies(patchIssue)).toEqual([{ priority: 0 }]));
      // The reads the refusal starts are held until what it put back is checked.
      const answerReads = reads.hold();
      await act(async () => {
        save.reject(closed());
        await save.promise.catch(() => undefined);
      });
      await waitFor(() => expect(writesOf(client).rows()).toEqual([]));
      expect(shownPriorities(), "P0 refused, before the re-read answers").toEqual({
        detail: "P1",
        inbox: "P1",
        list: "P1",
      });
      await act(async () => answerReads());
      await waitFor(() => expect(client.isFetching()).toBe(0));
      expect(shownPriorities(), "the re-read answers").toEqual({
        detail: "P1",
        inbox: "P1",
        list: "P1",
      });
    } finally {
      view.unmount();
      patchIssue.mockRestore();
    }
  });
}

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
    await pick(user, priorityTriggers()[0] as HTMLElement, "1");
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
    await pick(user, priorityTriggers()[0] as HTMLElement, "2");
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

// R3: a landing is as new as its answer's arrival, and every read it starts goes out after it. A
// save of CORE-1's status from its header is refused and retried from the Pending changes list,
// where no press awaits it: its retry answers with P1, another person then sets P2, and the store
// reads every query again, which answers P2. P0 is picked and refused: from the refusal until the
// re-read answers, every surface shows P2, the newer answer of the reads the retry's landing
// started - never the P1 of the retry's own answer before them.
test("a refused pick puts back the priority the reads a retried save's landing started answered, not the older answer of that save", async () => {
  const retried = Promise.withResolvers<Issue>();
  const save = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches()
    .mockRejectedValueOnce(closed())
    .mockImplementationOnce(() => retried.promise)
    .mockImplementationOnce(() => save.promise);
  const client = signedInHolding();
  const reads = dispatchReads();
  reads.store(1);
  const view = renderIn(
    client,
    <>
      <Surfaces reads={reads} />
      <CachedPriority reads={reads} />
    </>
  );
  const user = userEvent.setup();
  const seen: string[] = [];
  const see = (step: string) => {
    const { detail, inbox, list } = shownPriorities();
    seen.push(`${step}: detail ${detail} list ${list} inbox ${inbox}`);
  };
  try {
    await waitFor(() =>
      expect(shownPriorities()).toEqual({ detail: "P1", inbox: "P1", list: "P1" })
    );
    saveStatus(client);
    await waitFor(() =>
      expect(
        writesOf(client)
          .rows()
          .map((row) => row.status)
      ).toEqual(["refused"])
    );
    const [refused] = writesOf(client).rows();
    act(() => writesOf(client).retry(refused?.id ?? 0));
    await waitFor(() => expect(bodies(patchIssue)).toHaveLength(2));
    // Dispatch answers the retry at P1, and another person sets P2 before the reads its landing
    // starts reach it.
    reads.store(2);
    await act(async () => {
      retried.resolve({ ...issue, priority: 1, status: "done" });
      await retried.promise;
    });
    await waitFor(() => expect(writesOf(client).rows()).toEqual([]));
    await waitFor(() => expect(client.isFetching()).toBe(0));
    see("the status save retried and answered at P1, the reads after its landing at P2");
    await pick(user, priorityTriggers()[0] as HTMLElement, "0");
    await waitFor(() => expect(bodies(patchIssue)).toHaveLength(3));
    see("P0 picked");
    // The reads the refusal starts are held until what it put back is checked.
    const answerReads = reads.hold();

    await act(async () => {
      save.reject(closed());
      await save.promise.catch(() => undefined);
    });
    await waitFor(() => expect(writesOf(client).rows()).toEqual([]));
    see("P0 refused, before the re-read answers");
    await act(async () => answerReads());
    await waitFor(() => expect(client.isFetching()).toBe(0));
    see("the re-read answers");

    expect(seen).toEqual([
      "the status save retried and answered at P1, the reads after its landing at P2: detail P2 list P2 inbox P2",
      "P0 picked: detail P0 list P0 inbox P0",
      "P0 refused, before the re-read answers: detail P2 list P2 inbox P2",
      "the re-read answers: detail P2 list P2 inbox P2",
    ]);
  } finally {
    view.unmount();
    patchIssue.mockRestore();
  }
});

// R3: the value Dispatch last confirmed is the field in Dispatch's newest answer, never the pick
// shown over it. An assignee save is out and a priority pick, P0, waits behind it. Another person
// sets P2, and a read of the issue reaches Dispatch before the save does and answers, with P2,
// after the save landed: that answer is older than the landing's (`updated_at`), so the issue's
// query keeps the cached issue, which shows P0 over the landing's answer (`issueQuery`). P0 is
// then refused before a newer read answers: what it puts back is P2, the priority that read
// answered - not P0, nor the P1 the issue showed at the pick.
test("a pick refused after a read older than the cached issue answered puts back the priority that read answered, not the refused pick", async () => {
  const assign = Promise.withResolvers<Issue>();
  const refusal = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches()
    .mockImplementationOnce(() => assign.promise)
    .mockImplementationOnce(() => refusal.promise);
  const client = signedIn();
  const reads = dispatchReads();
  const surfaces = renderIn(client, <Surfaces reads={reads} />);
  const picker = renderIn(client, <CachedPriority reads={reads} />);
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={client}>{children}</QueryClientProvider>
  );
  const assignee = renderHook(() => useIssueFieldWrite(issue.key, "assignee", issue.assignee), {
    wrapper,
  });
  const user = userEvent.setup();
  const landedAt = "2026-09-09T00:01:00Z";
  const seen: string[] = [];
  const see = (step: string) => {
    const { detail, inbox, list } = shownPriorities();
    seen.push(`${step}: detail ${detail} list ${list} inbox ${inbox}`);
  };
  try {
    await act(async () => {
      assignee.result.current.submit("bob");
      await Promise.resolve();
    });
    await waitFor(() => expect(bodies(patchIssue)).toEqual([{ assignee: "bob" }]));
    await pick(user, priorityTriggers()[0] as HTMLElement, "0");
    see("P0 picked behind the assignee save");
    // Another person sets P2, and a read of the issue reaches Dispatch before the assignee save.
    reads.store(2, { updated_at: "2026-09-09T00:00:30Z" });
    const answerEarlyRead = reads.hold();
    const early = client.refetchQueries({ queryKey: detailKey });
    // Every read sent from now on - those the landing and the refusal start - waits.
    const answerLaterReads = reads.hold();
    reads.store(2, { assignee: "bob", updated_at: landedAt });
    await act(async () => {
      assign.resolve({ ...issue, assignee: "bob", priority: 2, updated_at: landedAt });
      await assign.promise;
    });
    await waitFor(() => expect(bodies(patchIssue)).toEqual([{ assignee: "bob" }, { priority: 0 }]));
    await act(async () => {
      answerEarlyRead();
      await early;
    });
    // The read's answer is older than the landing's: the issue keeps the landing's, P0 over it.
    expect(client.getQueryData<IssueDetails>(detailKey)).toMatchObject({
      assignee: "bob",
      priority: 0,
      updated_at: landedAt,
    });
    see("the read sent before the landing answers P2");

    await act(async () => {
      refusal.reject(closed());
      await refusal.promise.catch(() => undefined);
    });
    await waitFor(() => expect(writesOf(client).rows()).toEqual([]));
    see("P0 refused, before a newer read answers");
    await act(async () => answerLaterReads());
    await waitFor(() => expect(client.isFetching()).toBe(0));
    see("the newer reads answer");

    expect(seen).toEqual([
      "P0 picked behind the assignee save: detail P0 list P0 inbox P0",
      "the read sent before the landing answers P2: detail P0 list P0 inbox P0",
      "P0 refused, before a newer read answers: detail P2 list P2 inbox P2",
      "the newer reads answer: detail P2 list P2 inbox P2",
    ]);
  } finally {
    assignee.unmount();
    surfaces.unmount();
    picker.unmount();
    patchIssue.mockRestore();
  }
});

/** Has the page's store save CORE-1's title, as the issue page's header does (`updateIssue` in
 *  `IssueHeader`): on the issue's line, its answer merged into the issue, and the lists read
 *  again. */
function saveTitle(client: DispatchQueryClient, title: string): void {
  act(() => {
    void writesOf(client)
      .run({
        detail: null,
        inline: true,
        kind: plainWrite,
        label: `CORE-1 renamed to ${title}`,
        presentation: "indicator",
        request: issuePatchRequest(issue.key, { title }),
        target: `issue:${issue.key}`,
      })
      .then((next) => {
        if (next === null) void client.invalidateQueries({ queryKey: detailKey });
        else mergeIssue(client, next);
        void client.invalidateQueries({ queryKey: ["issues"] });
      })
      .catch(() => undefined);
  });
}

// R3: the value Dispatch last confirmed is the field in Dispatch's newest answer for the issue,
// whatever write of it gave that answer. The issue, the project's list and the Inbox are read at
// P1; another person sets P2, and the reader saves the issue's title from its header, whose answer
// carries P2 and is merged into the issue. P0 is picked and refused before any read sent after
// the title save answers: until the reads answer, every surface shows P2, the title save's answer
// - never the P1 of the reads before it.
test("a refused pick puts back the priority a save of the issue's title answered, not the older reads' one", async () => {
  const title = Promise.withResolvers<Issue>();
  const save = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches()
    .mockImplementationOnce(() => title.promise)
    .mockImplementationOnce(() => save.promise);
  const client = signedInHolding();
  // The page's store is made with the app, before any read goes out, so each read is kept.
  writesOf(client);
  const reads = dispatchReads();
  const view = renderIn(
    client,
    <>
      <Surfaces reads={reads} />
      <CachedPriority reads={reads} />
    </>
  );
  const user = userEvent.setup();
  const seen: string[] = [];
  const see = (step: string) => {
    const { detail, inbox, list } = shownPriorities();
    seen.push(`${step}: detail ${detail} list ${list} inbox ${inbox}`);
  };
  try {
    await waitFor(() =>
      expect(shownPriorities()).toEqual({ detail: "P1", inbox: "P1", list: "P1" })
    );
    await waitFor(() => expect(client.isFetching()).toBe(0));
    see("the issue, the list and the Inbox read at P1");
    // Another person sets P2. Every read sent from now on - the lists' after the title save,
    // those the refusal starts - waits.
    reads.store(2);
    const answerReads = reads.hold();
    saveTitle(client, "Review the spec again");
    await waitFor(() => expect(bodies(patchIssue)).toEqual([{ title: "Review the spec again" }]));
    await act(async () => {
      title.resolve({ ...issue, priority: 2, title: "Review the spec again" });
      await title.promise;
    });
    await waitFor(() => expect(writesOf(client).rows()).toEqual([]));
    see("the title saved, its answer at P2 merged into the issue");
    await pick(user, priorityTriggers()[0] as HTMLElement, "0");
    await waitFor(() => expect(bodies(patchIssue)).toHaveLength(2));
    see("P0 picked");

    await act(async () => {
      save.reject(closed());
      await save.promise.catch(() => undefined);
    });
    await waitFor(() => expect(writesOf(client).rows()).toEqual([]));
    see("P0 refused, before any read sent after the title save answers");
    await act(async () => answerReads());
    await waitFor(() => expect(client.isFetching()).toBe(0));
    see("the reads answer");

    expect(seen).toEqual([
      "the issue, the list and the Inbox read at P1: detail P1 list P1 inbox P1",
      "the title saved, its answer at P2 merged into the issue: detail P2 list P1 inbox P1",
      "P0 picked: detail P0 list P0 inbox P0",
      "P0 refused, before any read sent after the title save answers: detail P2 list P2 inbox P2",
      "the reads answer: detail P2 list P2 inbox P2",
    ]);
  } finally {
    view.unmount();
    patchIssue.mockRestore();
  }
});

// R3 inside the kind: a pick's answer is the whole issue, so an assignee pick's answer carries its
// priority too. The issue, the project's list and the Inbox are read at P1; bob is picked, and P0
// waits behind him. Another person sets P2, and the assignee save lands with it; P0 then goes out
// and is refused before any read sent after that landing answers: until the reads answer, every
// surface shows P2, the assignee save's answer - never the P1 of the reads before it.
test("a refused priority pick puts back the priority an assignee pick that landed before it answered, not the older reads' one", async () => {
  const assign = Promise.withResolvers<Issue>();
  const refusal = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches()
    .mockImplementationOnce(() => assign.promise)
    .mockImplementationOnce(() => refusal.promise);
  const client = signedInHolding();
  const reads = dispatchReads();
  const surfaces = renderIn(client, <Surfaces reads={reads} />);
  const picker = renderIn(client, <CachedPriority reads={reads} />);
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={client}>{children}</QueryClientProvider>
  );
  const assignee = renderHook(() => useIssueFieldWrite(issue.key, "assignee", issue.assignee), {
    wrapper,
  });
  const user = userEvent.setup();
  const seen: string[] = [];
  const see = (step: string) => {
    const { detail, inbox, list } = shownPriorities();
    seen.push(`${step}: detail ${detail} list ${list} inbox ${inbox}`);
  };
  try {
    await waitFor(() =>
      expect(shownPriorities()).toEqual({ detail: "P1", inbox: "P1", list: "P1" })
    );
    await waitFor(() => expect(client.isFetching()).toBe(0));
    see("the issue, the list and the Inbox read at P1");
    await act(async () => {
      assignee.result.current.submit("bob");
      await Promise.resolve();
    });
    await waitFor(() => expect(bodies(patchIssue)).toEqual([{ assignee: "bob" }]));
    await pick(user, priorityTriggers()[0] as HTMLElement, "0");
    see("P0 picked behind the assignee save");
    // Another person sets P2 before the assignee save reaches Dispatch. Every read sent from now
    // on - those the landing and the refusal start - waits.
    reads.store(2, { assignee: "bob" });
    const answerReads = reads.hold();
    await act(async () => {
      assign.resolve({ ...issue, assignee: "bob", priority: 2 });
      await assign.promise;
    });
    await waitFor(() => expect(bodies(patchIssue)).toEqual([{ assignee: "bob" }, { priority: 0 }]));
    see("the assignee save lands at P2, and P0 goes out");

    await act(async () => {
      refusal.reject(closed());
      await refusal.promise.catch(() => undefined);
    });
    await waitFor(() => expect(writesOf(client).rows()).toEqual([]));
    see("P0 refused, before any read sent after the landing answers");
    await act(async () => answerReads());
    await waitFor(() => expect(client.isFetching()).toBe(0));
    see("the reads answer");

    expect(seen).toEqual([
      "the issue, the list and the Inbox read at P1: detail P1 list P1 inbox P1",
      "P0 picked behind the assignee save: detail P0 list P0 inbox P0",
      "the assignee save lands at P2, and P0 goes out: detail P0 list P0 inbox P0",
      "P0 refused, before any read sent after the landing answers: detail P2 list P2 inbox P2",
      "the reads answer: detail P2 list P2 inbox P2",
    ]);
  } finally {
    assignee.unmount();
    surfaces.unmount();
    picker.unmount();
    patchIssue.mockRestore();
  }
});

/** Has the page's store release CORE-1's claim, as the issue page's header does (`releaseClaim`
 *  in `IssueHeader`): off the issue's line, `answer` being Dispatch's, the issue, merged into
 *  the issue, and the lists read again. */
function releaseClaim(client: DispatchQueryClient, answer: Promise<Issue>): void {
  act(() => {
    void writesOf(client)
      .run({
        detail: null,
        inline: true,
        kind: plainWrite,
        label: `Claim on ${issue.key} released`,
        presentation: "indicator",
        method: "DELETE",
        write: (bounds) => {
          bounds.sent?.({ method: "DELETE", path: `/api/v1/issues/${issue.key}/claim` });
          return answer;
        },
      })
      .then((next) => {
        if (next !== null) mergeIssue(client, next);
        void client.invalidateQueries({ queryKey: ["issues"] });
      })
      .catch(() => undefined);
  });
}

// R3 with a write off the issue's line: a landing is stamped as its answer arrives, which is the
// order Dispatch stored the writes in only on the issue's line, which sends one at a time. The
// issue, the project's list and the Inbox are read at P1; P0 is picked, and the claim is released
// from the issue's header while it is out, off the issue's line. Dispatch stores the release
// first, so its answer carries P1, then P0, and P0's answer arrives first. P2 is then picked and
// refused before any read sent after the release's answer answers: until the reads answer, every
// surface shows P0, the newest value Dispatch stored - never the older P1 of the release's answer.
test("a refused pick puts back the priority a pick that landed answered, not the older one a claim release off the issue's line answered after it", async () => {
  const pick = Promise.withResolvers<Issue>();
  const refusal = Promise.withResolvers<Issue>();
  const release = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches()
    .mockImplementationOnce(() => pick.promise)
    .mockImplementationOnce(() => refusal.promise);
  const client = signedInHolding();
  // The page's store is made with the app, before any read goes out, so each read is kept.
  writesOf(client);
  const reads = dispatchReads();
  const view = renderIn(
    client,
    <>
      <Surfaces reads={reads} />
      <CachedPriority reads={reads} />
    </>
  );
  const user = userEvent.setup();
  const releasedAt = "2026-09-09T00:01:00Z";
  const pickedAt = "2026-09-09T00:02:00Z";
  const seen: string[] = [];
  const see = (step: string) => {
    const { detail, inbox, list } = shownPriorities();
    seen.push(`${step}: detail ${detail} list ${list} inbox ${inbox}`);
  };
  try {
    await waitFor(() =>
      expect(shownPriorities()).toEqual({ detail: "P1", inbox: "P1", list: "P1" })
    );
    await waitFor(() => expect(client.isFetching()).toBe(0));
    see("the issue, the list and the Inbox read at P1");
    await pickPriority(user, priorityTriggers()[0] as HTMLElement, "0");
    await waitFor(() => expect(bodies(patchIssue)).toEqual([{ priority: 0 }]));
    releaseClaim(client, release.promise);
    see("P0 picked and the claim released, both out");
    // Dispatch stores the release at P1, then P0. Every read sent from now on - those P0's
    // landing, the release and the refusal start - waits.
    reads.store(0, { updated_at: pickedAt });
    const answerReads = reads.hold();
    await act(async () => {
      pick.resolve({ ...issue, priority: 0, updated_at: pickedAt });
      await pick.promise;
    });
    await waitFor(() => expect(writesOf(client).rows()).toHaveLength(1));
    see("P0 lands");
    await act(async () => {
      release.resolve({ ...issue, priority: 1, updated_at: releasedAt });
      await release.promise;
    });
    await waitFor(() => expect(writesOf(client).rows()).toEqual([]));
    see("the release's answer, at P1 from before P0, arrives after P0's");
    await pickPriority(user, priorityTriggers()[0] as HTMLElement, "2");
    await waitFor(() => expect(bodies(patchIssue)).toEqual([{ priority: 0 }, { priority: 2 }]));
    see("P2 picked");

    await act(async () => {
      refusal.reject(closed());
      await refusal.promise.catch(() => undefined);
    });
    await waitFor(() => expect(writesOf(client).rows()).toEqual([]));
    see("P2 refused, before any read sent after the release's answer answers");
    await act(async () => answerReads());
    await waitFor(() => expect(client.isFetching()).toBe(0));
    see("the reads answer");

    expect(seen).toEqual([
      "the issue, the list and the Inbox read at P1: detail P1 list P1 inbox P1",
      "P0 picked and the claim released, both out: detail P0 list P0 inbox P0",
      "P0 lands: detail P0 list P0 inbox P0",
      "the release's answer, at P1 from before P0, arrives after P0's: detail P0 list P0 inbox P0",
      "P2 picked: detail P2 list P2 inbox P2",
      "P2 refused, before any read sent after the release's answer answers: detail P0 list P0 inbox P0",
      "the reads answer: detail P0 list P0 inbox P0",
    ]);
  } finally {
    view.unmount();
    patchIssue.mockRestore();
  }
});

// R3 on an Inbox row: Dispatch answers the person an ask nobody handed on waits on as its issue's
// assignee, else its project's default. CORE-1 is unassigned and its ask waits on dave, CORE's
// default; the Inbox is read, then the issue's detail, and the client holds no read of the
// defaults. carol is picked and refused: until the re-read answers, the row shows CORE-1
// unassigned and waiting on dave, as Dispatch's newest Inbox answer named him - never on carol,
// though the newest answer of the assignee is the detail's.
test("a refused assignee pick puts back the person an Inbox row waited on in Dispatch's newest Inbox answer, though the issue's detail was read after it", async () => {
  const save = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches().mockImplementationOnce(() => save.promise);
  const client = signedInHolding();
  let gate: Promise<void> | undefined;
  const read =
    <T,>(answer: T) =>
    async (): Promise<T> => {
      await gate;
      return answer;
    };
  const unassigned: InboxRow = {
    ...inboxRow,
    issue: { assignee: null, key: issue.key, title: issue.title },
    waiting_on_person: "dave",
  };
  const inboxRead = read([unassigned]);
  const detailRead = read<IssueDetails>({ ...details, assignee: null });
  const forever = { staleTime: Number.POSITIVE_INFINITY };
  function Inbox(): ReactNode {
    useQuery({ ...forever, queryFn: inboxRead, queryKey: inboxKey });
    return null;
  }
  function Detail(): ReactNode {
    useQuery({ ...issueQuery(issue.key), ...forever, queryFn: detailRead });
    return null;
  }
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={client}>{children}</QueryClientProvider>
  );
  // The page's write store is made before any read, as the app's shell makes it.
  const assignee = renderHook(() => useIssueFieldWrite(issue.key, "assignee", issue.assignee), {
    wrapper,
  });
  const inbox = renderIn(client, <Inbox />);
  let detail: RenderResult | undefined;
  const seen: string[] = [];
  const see = (step: string) => {
    const [row] = client.getQueryData<InboxRow[]>(inboxKey) ?? [];
    seen.push(`${step}: assignee ${row?.issue?.assignee} waiting on ${row?.waiting_on_person}`);
  };
  try {
    await waitFor(() => expect(client.getQueryData<InboxRow[]>(inboxKey)).toEqual([unassigned]));
    detail = renderIn(client, <Detail />);
    await waitFor(() => expect(client.getQueryData(detailKey)).toMatchObject({ assignee: null }));
    see("the Inbox read, then the issue");
    await act(async () => {
      assignee.result.current.submit("carol");
      await Promise.resolve();
    });
    await waitFor(() => expect(bodies(patchIssue)).toEqual([{ assignee: "carol" }]));
    see("carol picked");
    // The reads the refusal starts are held until what it put back is checked.
    const held = Promise.withResolvers<void>();
    gate = held.promise;

    await act(async () => {
      save.reject(
        new ApiError(400, {
          code: "ASSIGNEE_NOT_ALLOWED",
          error: '"carol" has not signed in to Dispatch',
        })
      );
      await save.promise.catch(() => undefined);
    });
    await waitFor(() => expect(writesOf(client).rows()).toEqual([]));
    see("carol refused, before the re-read answers");
    await act(async () => held.resolve());
    await waitFor(() => expect(client.isFetching()).toBe(0));
    see("the re-read answers");

    expect(seen).toEqual([
      "the Inbox read, then the issue: assignee null waiting on dave",
      "carol picked: assignee carol waiting on carol",
      "carol refused, before the re-read answers: assignee null waiting on dave",
      "the re-read answers: assignee null waiting on dave",
    ]);
  } finally {
    detail?.unmount();
    inbox.unmount();
    assignee.unmount();
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
 *  `resolved`, with the project's mappings read once the reader reaches for it. The picker is the
 *  shared list's single pick: its list suggests the mappings after Inherit, a pick of a row writes
 *  it, and the Inherit row is an Inherit pick: the issue inherits a repository again. */
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
  const trigger = () => screen.getByRole("button", { name: pickNamed("Repository of CORE-1") });
  /** The repositories the open list suggests, its options after Inherit. */
  const suggestions = () => repoSuggestions(panelOf(trigger()));
  /** What the trigger's name reads after its label, which names a repository that is not the
   *  issue's own. */
  const described = () => shownBy(trigger());
  return {
    client,
    described,
    detailRead,
    suggestions,
    trigger,
    unmount: () => {
      view.unmount();
      listRepoProjects.mockRestore();
    },
  };
}

test("a refused repository pick puts back the repository Dispatch last confirmed and shows its reason with Retry", async () => {
  const patchIssue = answerIssuePatches().mockRejectedValueOnce(
    new ApiError(400, {
      code: "INVALID_ISSUE",
      error: "acme/gadgets cannot be the repository of CORE-1",
    })
  );
  const { client, described, detailRead, suggestions, trigger, unmount } = renderRepo(
    null,
    "acme/widgets"
  );
  const user = userEvent.setup();
  try {
    await user.hover(trigger());
    await user.click(trigger());
    await waitFor(() => expect(suggestions()).toEqual(["acme/gadgets", "acme/widgets"]));
    await pickRepo(user, trigger, "acme/gadgets");
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("acme/gadgets cannot be the repository of CORE-1");
    expect(within(alert).getByRole("button", { name: "Retry" })).toBeTruthy();
    // The issue's own repository is the row its list marks picked: none, so Inherit.
    await user.click(trigger());
    expect(within(panelOf(trigger())).getByRole("option", { name: "Inherit" })).toHaveAttribute(
      "aria-selected",
      "true"
    );
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
  const { described, suggestions, trigger, unmount } = renderRepo("acme/gadgets", "acme/gadgets");
  const user = userEvent.setup();
  try {
    expect(described()).toBe("acme/gadgets");
    await user.hover(trigger());
    await user.click(trigger());
    await waitFor(() => expect(suggestions()).toEqual(["acme/gadgets", "acme/widgets"]));
    await pickRepo(user, trigger, "");
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
    await pick(user, priorityTriggers()[0] as HTMLElement, "0");
    await pick(user, priorityTriggers()[0] as HTMLElement, "3");
    const pending = screen.getByTestId("pending-writes");
    expect(pending).toHaveTextContent("CORE-1 set to P3 — Waiting to send");
    await user.click(within(pending).getByRole("button", { name: "Discard" }));
    // The pick still out is the newest live one: it shows, and nothing is refused.
    expect(shownValue(priorityTriggers()[0] as HTMLElement)).toBe("0");
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
    await pick(user, priorityTriggers()[0] as HTMLElement, "0");
    const pending = screen.getByTestId("pending-writes");
    expect(pending).toHaveTextContent("CORE-1 set to P0 — Waiting to send");
    expect(cachedPriorities(client)).toEqual({ detail: 0, inbox: 0, list: 0 });
    await user.click(within(pending).getByRole("button", { name: "Discard" }));
    expect(cachedPriorities(client)).toEqual({ detail: 1, inbox: 1, list: 1 });
    expect(shownValue(priorityTriggers()[0] as HTMLElement)).toBe("1");
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
    await pick(user, priorityTriggers()[0] as HTMLElement, "2");
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
    // P2, not the P1 the issue holds: this tree's menu writes nothing for the value it already
    // shows (`ChoiceMenu`), as a select set to its own value fires nothing in a browser.
    await pickPriority(user, priorityTriggers()[0] as HTMLElement, "2");
    expect(cachedPriorities(client)).toEqual({ detail: 2, inbox: 2, list: 2 });
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
      pick.resolve({ ...issue, priority: 2 });
      await moved;
    });
    await waitFor(() => expect(writesOf(client).rows()).toEqual([]));
    expect(shownPriorities()).toEqual({ detail: "P3", inbox: "P3", list: "P3" });
    expect(bodies(patchIssue).map((body) => (body as UpdateIssueInput).priority)).toEqual([2, 3]);
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
 *  board move on the project is answered), showing the priority of CORE-1's card, and, given
 *  `commits`, the card's own priority picker, as each card of the board draws one, each commit of
 *  it recorded there with the card's priority. */
function BoardCard({ commits }: { commits?: Commits<IssuePriority | null> }): ReactNode {
  const board = useQuery({
    ...projectIssuesQuery(issue.project, []),
    staleTime: Number.POSITIVE_INFINITY,
  });
  const card = board.data?.find((row) => row.key === issue.key);
  return (
    <>
      <p data-testid="board-priority">P{card?.priority}</p>
      {commits !== undefined && card !== undefined ? (
        <commits.Recorded host={issuePriority(card.priority)}>
          <PriorityControl issueKey={card.key} priority={issuePriority(card.priority)} />
        </commits.Recorded>
      ) : null}
    </>
  );
}

/** A board on CORE beside the issue's own picker - or, `onCard`, with the picker on CORE-1's card
 *  alone, as the board page draws it - with `others` on it besides CORE-1, whose list and detail
 *  Dispatch answers with the priority it stores as each read reaches it (`stored`), once the row
 *  lets the reads it holds go (`hold`). The client holds the list as Dispatch last answered it,
 *  or, `unread`, holds none yet, so the board reads it. `commits` records each commit of the
 *  card's picker, `onCard`: what its menu's trigger is named after its label and what its badge
 *  says (`drawnPick`). */
function renderBoard(
  client: DispatchQueryClient,
  stored: () => IssuePriority | null,
  {
    onCard = false,
    others = [],
    unread = false,
  }: { onCard?: boolean; others?: readonly IssueSummary[]; unread?: boolean } = {}
) {
  if (!unread) client.setQueryData(listKey, [summary, ...others]);
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
  const commits = recordCommits<IssuePriority | null>(() => {
    const trigger = document.querySelector<HTMLElement>("[data-priority-menu]");
    return trigger === null ? "(none)" : drawnPick(trigger);
  });
  const view = renderIn(
    client,
    onCard ? (
      <BoardCard commits={commits} />
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
    commits,
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
    await pickPriority(user, priorityTriggers()[0] as HTMLElement, "2");
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
    await pickPriority(user, priorityTriggers()[0] as HTMLElement, "2");
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
    await pickPriority(user, priorityTriggers()[0] as HTMLElement, "2");
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
    await pickPriority(user, priorityTriggers()[0] as HTMLElement, "2");
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
    await pickPriority(user, priorityTriggers()[0] as HTMLElement, "1");
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
    await pickPriority(user, priorityTriggers()[0] as HTMLElement, "2");
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
  const drag = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches()
    .mockImplementationOnce(() => pick.promise)
    .mockImplementationOnce(() => other.promise)
    .mockImplementationOnce(() => drag.promise)
    .mockImplementation(stores);
  const client = signedIn();
  const board = renderBoard(client, () => 1, { others: [neighbour] });
  const user = userEvent.setup();
  try {
    await pickPriority(user, priorityTriggers()[0] as HTMLElement, "0");
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

    // The other card's move lands, and the drag joins the line and goes out: a drag is no pick,
    // so the report stands while it is out.
    await act(async () => {
      other.resolve({ ...issue, key: neighbour.key });
      await other.promise;
    });
    await waitFor(() => expect(bodies(patchIssue)).toHaveLength(3));
    expect(
      writesOf(client)
        .rows()
        .map((row) => row.label)
    ).toEqual(["CORE-1 moved on the board"]);
    expect(screen.getByRole("alert")).toHaveTextContent("Could not update the priority of CORE-1.");
    expect(board.shown()).toBe("P3");

    // The drag lands: the report ends with it.
    await act(async () => {
      drag.resolve({ ...issue, priority: 3 });
      await drag.promise;
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

// A pick made on a card while a drag of that card still waits its turn behind another card's
// move is older than the drag (`fieldWritesOf`): the pick goes out first, Dispatch stores the
// drag's lane last, and the card shows that lane. The card's picker draws the card's own priority
// in every commit from the pick on - never a pick the card does not draw - and the drag's lane
// once every write has landed, whichever of the pick and the other card's move Dispatch answers
// first, with no picker drawn again. The row asserts that and not "never P2": when the other
// card's move lands first, the card itself draws P2 for one commit, while the drag has left its
// board's queue and not yet joined the issue's line, so `fieldWritesOf` counts it as neither and
// the pick is the newest write for those microtasks. That gap is the write engine's; the picker
// draws P2 there because its card does.
for (const first of ["the pick", "the other card's move"] as const) {
  test(`a pick made on a card while a drag of it waits behind another card's move draws the card's priority in every commit, when ${first} lands first`, async () => {
    const other = Promise.withResolvers<Issue>();
    const pick = Promise.withResolvers<Issue>();
    const drag = Promise.withResolvers<Issue>();
    const patchIssue = answerIssuePatches()
      .mockImplementationOnce(() => other.promise)
      .mockImplementationOnce(() => pick.promise)
      .mockImplementationOnce(() => drag.promise);
    const client = signedIn();
    const board = renderBoard(client, () => storedPriority(patchIssue), {
      onCard: true,
      others: [neighbour],
    });
    const user = userEvent.setup();
    const land = async (
      save: { readonly promise: Promise<Issue>; readonly resolve: (answer: Issue) => void },
      answer: Issue
    ) => {
      await act(async () => {
        save.resolve(answer);
        await save.promise;
      });
    };
    try {
      act(() => {
        void board.moves().moveCardToLane(neighbour.key, "todo", 2, 0);
      });
      await waitFor(() => expect(bodies(patchIssue)).toHaveLength(1));
      act(() => {
        void board.moves().moveCardToLane(issue.key, "todo", 3, 0);
      });
      // The drag waits in the board's scope, out of the store.
      expect(
        writesOf(client)
          .rows()
          .map((row) => row.label)
      ).toEqual(["CORE-2 moved on the board"]);
      expect(board.shown()).toBe("P3");
      const [picker] = priorityTriggers() as [HTMLElement];
      await user.click(picker);
      const p2 = choiceOf(picker, "P2");
      const fromPick = board.commits.markAt(p2, "click");
      await user.click(p2);
      await waitFor(() => expect(bodies(patchIssue)).toHaveLength(2));
      if (first === "the pick") {
        await land(pick, { ...issue, priority: 2 });
        await land(other, { ...issue, key: neighbour.key });
      } else {
        await land(other, { ...issue, key: neighbour.key });
        await land(pick, { ...issue, priority: 2 });
      }
      await waitFor(() => expect(bodies(patchIssue)).toHaveLength(3));
      await land(drag, { ...issue, priority: 3 });
      await waitFor(() => expect(writesOf(client).rows()).toEqual([]));
      await waitFor(() => expect(client.isFetching()).toBe(0));

      expect(bodies(patchIssue).map((body) => (body as UpdateIssueInput).priority)).toEqual([
        2, 2, 3,
      ]);
      expect(board.shown()).toBe("P3");
      expect(fromPick().map(({ drawn }) => drawn)).toEqual(
        fromPick().map(({ host }) => `P${host} P${host}`)
      );
      expect(fromPick().at(-1)?.drawn).toBe("P3 P3");
    } finally {
      board.unmount();
      patchIssue.mockRestore();
    }
  });
}

// R2, for drags of a card that still wait their turn on the board: a pick of P2 is out, and so
// is another card's move, so two drags of this card - to P3, then to P0 - both wait behind that
// move, out of the store. A waiting drag is newer than every write on the issue's line, and of
// two waiting drags the later made is newer: the board and the issue show P0 from the second
// drag on - once the other card's move is answered and the first drag joins the line while the
// second still waits, through the pick's landing and the first drag's - and P0 once all landed.
test("two drags of a card that wait behind another card's move, made over its live pick, show the newest drag's lane from that drag on", async () => {
  const pick = Promise.withResolvers<Issue>();
  const other = Promise.withResolvers<Issue>();
  const first = Promise.withResolvers<Issue>();
  const second = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches()
    .mockImplementationOnce(() => pick.promise)
    .mockImplementationOnce(() => other.promise)
    .mockImplementationOnce(() => first.promise)
    .mockImplementationOnce(() => second.promise);
  const client = signedIn();
  const board = renderBoard(client, () => storedPriority(patchIssue), { others: [neighbour] });
  const user = userEvent.setup();
  const seen: string[] = [];
  const see = (step: string) =>
    seen.push(`${step}: detail P${cachedPriorities(client).detail} board ${board.shown()}`);
  const line = () =>
    writesOf(client)
      .rows()
      .map((row) => row.label);
  try {
    await pickPriority(user, priorityTriggers()[0] as HTMLElement, "2");
    await waitFor(() => expect(bodies(patchIssue)).toHaveLength(1));
    act(() => {
      void board.moves().moveCardToLane(neighbour.key, "todo", 2, 0);
    });
    await waitFor(() => expect(bodies(patchIssue)).toHaveLength(2));
    see("pick P2 out, another card's move out");
    act(() => {
      void board.moves().moveCardToLane(issue.key, "todo", 3, 0);
    });
    see("drag 1 to P3 waits");
    act(() => {
      void board.moves().moveCardToLane(issue.key, "todo", 0, 0);
    });
    see("drag 2 to P0 waits");
    // Neither drag is in the store: both wait in the board's scope.
    expect(line()).toEqual(["CORE-1 set to P2", "CORE-2 moved on the board"]);

    await act(async () => {
      other.resolve({ ...issue, key: neighbour.key });
      await other.promise;
    });
    await waitFor(() => expect(line()).toEqual(["CORE-1 set to P2", "CORE-1 moved on the board"]));
    see("the other card's move lands, drag 1 joins the line");
    await act(async () => {
      pick.resolve({ ...issue, priority: 2 });
      await pick.promise;
    });
    await waitFor(() => expect(bodies(patchIssue)).toHaveLength(3));
    see("pick lands, drag 1 out");
    await act(async () => {
      first.resolve({ ...issue, priority: 3 });
      await first.promise;
    });
    await waitFor(() => expect(bodies(patchIssue)).toHaveLength(4));
    see("drag 1 lands, drag 2 out");
    await act(async () => {
      second.resolve({ ...issue, priority: 0 });
      await second.promise;
    });
    await waitFor(() => expect(writesOf(client).rows()).toEqual([]));
    await waitFor(() => expect(client.isFetching()).toBe(0));
    see("drag 2 lands");

    expect(seen).toEqual([
      "pick P2 out, another card's move out: detail P2 board P2",
      "drag 1 to P3 waits: detail P3 board P3",
      "drag 2 to P0 waits: detail P0 board P0",
      "the other card's move lands, drag 1 joins the line: detail P0 board P0",
      "pick lands, drag 1 out: detail P0 board P0",
      "drag 1 lands, drag 2 out: detail P0 board P0",
      "drag 2 lands: detail P0 board P0",
    ]);
    expect(bodies(patchIssue).map((body) => (body as UpdateIssueInput).priority)).toEqual([
      2, 2, 3, 0,
    ]);
  } finally {
    board.unmount();
    patchIssue.mockRestore();
  }
});

// R3 on a page that read the issue before its board: the issue's detail is read at P3 and stays
// in the cache. The card is then dragged to the P1 lane and the drag is saved, so the board
// reads its list again, at P1, and nothing reads the issue again. P0 is picked and refused:
// until the re-read answers, the board and the issue show P1, the board's newer answer - never
// the P3 the older read of the issue holds.
test("a refused pick puts back the priority of the newest read across the caches: the board's, read again after a drag of the card was saved", async () => {
  const drag = Promise.withResolvers<Issue>();
  const pick = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches()
    .mockImplementationOnce(() => drag.promise)
    .mockImplementationOnce(() => pick.promise);
  const client = signedInHolding();
  let stored: IssuePriority | null = 3;
  const board = renderBoard(client, () => stored, { unread: true });
  const user = userEvent.setup();
  const seen: string[] = [];
  const see = (step: string) =>
    seen.push(`${step}: detail P${cachedPriorities(client).detail} board ${board.shown()}`);
  try {
    await waitFor(() => expect(board.shown()).toBe("P3"));
    await waitFor(() => expect(priorityTriggers()).toHaveLength(1));
    act(() => {
      void board.moves().moveCardToLane(issue.key, "todo", 1, 0);
    });
    await waitFor(() => expect(bodies(patchIssue)).toHaveLength(1));
    stored = 1;
    await act(async () => {
      drag.resolve({ ...issue, priority: 1 });
      await drag.promise;
    });
    // The board reads its list again once the drag is saved.
    await waitFor(() => expect(api.listIssues).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(client.isFetching()).toBe(0));
    see("the issue read at P3, the card dragged to P1 and the board read again");
    await pickPriority(user, priorityTriggers()[0] as HTMLElement, "0");
    await waitFor(() => expect(bodies(patchIssue)).toHaveLength(2));
    see("P0 picked");
    // The reads the refusal starts are held until what it put back is checked.
    const answerReads = board.hold();

    await act(async () => {
      pick.reject(closed());
      await pick.promise.catch(() => undefined);
    });
    await waitFor(() => expect(writesOf(client).rows()).toEqual([]));
    see("P0 refused, before the re-read answers");
    await act(async () => answerReads());
    await waitFor(() => expect(client.isFetching()).toBe(0));
    see("the re-read answers");

    expect(seen).toEqual([
      "the issue read at P3, the card dragged to P1 and the board read again: detail P3 board P1",
      "P0 picked: detail P0 board P0",
      "P0 refused, before the re-read answers: detail P1 board P1",
      "the re-read answers: detail P1 board P1",
    ]);
  } finally {
    board.unmount();
    patchIssue.mockRestore();
  }
});

// R3 for a drag that lands while no pick is live: Dispatch's answer to it is its newest for the
// card's priority until the board's read after it answers. The issue and the board are read at
// P3; the card is dragged to the P1 lane and the drag is saved, and the board's read again is
// held. P0 is then picked and refused: until the reads answer, the issue and the board show P1,
// the drag's lane - never the P3 from before the drag.
test("a drag saved while no pick is live is the priority a later refused pick puts back, before the board's read after the drag answers", async () => {
  const drag = Promise.withResolvers<Issue>();
  const pick = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches()
    .mockImplementationOnce(() => drag.promise)
    .mockImplementationOnce(() => pick.promise);
  const client = signedInHolding();
  let stored: IssuePriority | null = 3;
  const board = renderBoard(client, () => stored, { unread: true });
  const user = userEvent.setup();
  const seen: string[] = [];
  const see = (step: string) =>
    seen.push(`${step}: detail P${cachedPriorities(client).detail} board ${board.shown()}`);
  try {
    await waitFor(() => expect(board.shown()).toBe("P3"));
    await waitFor(() => expect(priorityTriggers()).toHaveLength(1));
    await waitFor(() => expect(client.isFetching()).toBe(0));
    // Every read from now on - the board's after the drag, those the refusal starts - waits.
    const answerReads = board.hold();
    act(() => {
      void board.moves().moveCardToLane(issue.key, "todo", 1, 0);
    });
    await waitFor(() => expect(bodies(patchIssue)).toHaveLength(1));
    stored = 1;
    await act(async () => {
      drag.resolve({ ...issue, priority: 1 });
      await drag.promise;
    });
    await waitFor(() => expect(api.listIssues).toHaveBeenCalledTimes(2));
    see("the issue and the board read at P3, the card dragged to P1 and saved");
    await pickPriority(user, priorityTriggers()[0] as HTMLElement, "0");
    await waitFor(() => expect(bodies(patchIssue)).toHaveLength(2));
    see("P0 picked");

    await act(async () => {
      pick.reject(closed());
      await pick.promise.catch(() => undefined);
    });
    await waitFor(() => expect(writesOf(client).rows()).toEqual([]));
    see("P0 refused, before any read answers");
    await act(async () => answerReads());
    await waitFor(() => expect(client.isFetching()).toBe(0));
    see("the reads answer");

    expect(seen).toEqual([
      "the issue and the board read at P3, the card dragged to P1 and saved: detail P3 board P1",
      "P0 picked: detail P0 board P0",
      "P0 refused, before any read answers: detail P1 board P1",
      "the reads answer: detail P1 board P1",
    ]);
  } finally {
    board.unmount();
    patchIssue.mockRestore();
  }
});

// R3 for a drag that lands with no answer while no pick is live: Dispatch had stored it and kept
// no answer for it (`409 KEY_COMMITTED`), so the store's end of its write tells nothing of what it
// set, and the drag's lane is what Dispatch stored. The issue and the board are read at P3; the
// card is dragged to the P1 lane, and the reads its landing starts are held. P0 is then picked and
// refused: until the reads answer, the issue and the board show P1, the drag's lane - never the
// P3 from before the drag.
test("a drag that landed with no answer while no pick is live is the priority a later refused pick puts back, before the reads after it answer", async () => {
  const pick = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches()
    .mockRejectedValueOnce(new ApiError(409, { code: "KEY_COMMITTED", error: "already made" }))
    .mockImplementationOnce(() => pick.promise);
  const client = signedInHolding();
  let stored: IssuePriority | null = 3;
  const board = renderBoard(client, () => stored, { unread: true });
  const user = userEvent.setup();
  const seen: string[] = [];
  const see = (step: string) =>
    seen.push(`${step}: detail P${cachedPriorities(client).detail} board ${board.shown()}`);
  try {
    await waitFor(() => expect(board.shown()).toBe("P3"));
    await waitFor(() => expect(priorityTriggers()).toHaveLength(1));
    await waitFor(() => expect(client.isFetching()).toBe(0));
    // Every read from now on - those the drag's landing starts, those the refusal starts - waits.
    const answerReads = board.hold();
    stored = 1;
    act(() => {
      void board.moves().moveCardToLane(issue.key, "todo", 1, 0);
    });
    await waitFor(() => expect(bodies(patchIssue)).toHaveLength(1));
    await waitFor(() => expect(client.isMutating()).toBe(0));
    see("the issue and the board read at P3, the card dragged to P1 and stored with no answer");
    await pickPriority(user, priorityTriggers()[0] as HTMLElement, "0");
    await waitFor(() => expect(bodies(patchIssue)).toHaveLength(2));
    see("P0 picked");

    await act(async () => {
      pick.reject(closed());
      await pick.promise.catch(() => undefined);
    });
    await waitFor(() => expect(writesOf(client).rows()).toEqual([]));
    see("P0 refused, before any read answers");
    await act(async () => answerReads());
    await waitFor(() => expect(client.isFetching()).toBe(0));
    see("the reads answer");

    expect(seen).toEqual([
      "the issue and the board read at P3, the card dragged to P1 and stored with no answer: detail P3 board P1",
      "P0 picked: detail P0 board P0",
      "P0 refused, before any read answers: detail P1 board P1",
      "the reads answer: detail P1 board P1",
    ]);
  } finally {
    board.unmount();
    patchIssue.mockRestore();
  }
});

/** The Inbox as a page reads it, drawing nothing. */
function InboxRead({ reads }: { reads: Reads }): ReactNode {
  useQuery({ queryFn: reads.inbox, queryKey: inboxKey, staleTime: Number.POSITIVE_INFINITY });
  return null;
}

/** CORE-1's detail, with its priority picker, and an Inbox row of it, each reading its own query,
 *  beside a board move of it (`useBoardMoves`) made from the project's list the client holds, with
 *  CORE-1 at P3, which no read refreshes: no surface reads that list. */
function renderDetailAndInbox(client: DispatchQueryClient, reads: Reads) {
  client.setQueryData(listKey, [{ ...summary, priority: 3 }]);
  const view = renderIn(
    client,
    <>
      <CachedPriority reads={reads} />
      <InboxRead reads={reads} />
    </>
  );
  const board = renderHook(() => useBoardMoves(issue.project), {
    wrapper: ({ children }) => (
      <QueryClientProvider client={client}>{children}</QueryClientProvider>
    ),
  });
  return {
    moves: () => board.result.current,
    unmount: () => {
      board.unmount();
      view.unmount();
    },
  };
}

// R3 for a drag that lands with no answer (`409 KEY_COMMITTED`): its landing is as new as that
// answer's arrival, and the reads the store starts at it - every query, since nothing tells what
// the drag stored - go out after it, before the drag's move settles. The issue and its Inbox row
// are read at P3; the card is dragged to the P1 lane, another person then sets P2, and the
// drag's landing has the issue and the Inbox read again, at P2. P0 is picked and refused: until
// the re-read answers, the issue and the Inbox show P2, those reads' answer - never the drag's P1.
test("a refused pick puts back the priority the reads a drag's landing with no answer started answered, not the drag's older lane", async () => {
  const drag = Promise.withResolvers<Issue>();
  const pick = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches()
    .mockImplementationOnce(() => drag.promise)
    .mockImplementationOnce(() => pick.promise);
  const client = signedInHolding();
  const reads = dispatchReads();
  reads.store(3);
  const page = renderDetailAndInbox(client, reads);
  const user = userEvent.setup();
  const seen: string[] = [];
  const see = (step: string) => {
    const { detail, inbox } = cachedPriorities(client);
    seen.push(`${step}: detail P${detail} inbox P${inbox}`);
  };
  try {
    await waitFor(() => expect(priorityTriggers()).toHaveLength(1));
    await waitFor(() => expect(client.isFetching()).toBe(0));
    act(() => {
      void page.moves().moveCardToLane(issue.key, "todo", 1, 0);
    });
    await waitFor(() => expect(bodies(patchIssue)).toHaveLength(1));
    // Dispatch stores the drag's P1 and loses its answer; another person then sets P2.
    reads.store(2);
    await act(async () => {
      drag.reject(new ApiError(409, { code: "KEY_COMMITTED", error: "already made" }));
      await drag.promise.catch(() => undefined);
    });
    await waitFor(() => expect(client.isMutating()).toBe(0));
    await waitFor(() => expect(client.isFetching()).toBe(0));
    see("the card dragged to P1 and stored with no answer, the reads after its landing at P2");
    await pickPriority(user, priorityTriggers()[0] as HTMLElement, "0");
    await waitFor(() => expect(bodies(patchIssue)).toHaveLength(2));
    see("P0 picked");
    // The reads the refusal starts are held until what it put back is checked.
    const answerReads = reads.hold();

    await act(async () => {
      pick.reject(closed());
      await pick.promise.catch(() => undefined);
    });
    await waitFor(() => expect(writesOf(client).rows()).toEqual([]));
    see("P0 refused, before the re-read answers");
    await act(async () => answerReads());
    await waitFor(() => expect(client.isFetching()).toBe(0));
    see("the re-read answers");

    expect(seen).toEqual([
      "the card dragged to P1 and stored with no answer, the reads after its landing at P2: detail P2 inbox P2",
      "P0 picked: detail P0 inbox P0",
      "P0 refused, before the re-read answers: detail P2 inbox P2",
      "the re-read answers: detail P2 inbox P2",
    ]);
  } finally {
    page.unmount();
    patchIssue.mockRestore();
  }
});

// R3 for a read of the board that waits for a drag of its card (`projectIssuesQuery`): its
// request goes out once the drag's move is answered, after the drag landed, so it is newer than
// the drag's answer. The issue and the board are read at P3; the card is dragged to the P1 lane,
// and the board is read again while the drag is out, which waits. Dispatch answers the drag at
// P1, another person then sets P2, and the board's read goes out and answers P2. P0 is picked and
// refused: until the re-read answers, the issue and the board show P2, the board's answer - never
// the drag's P1.
test("a refused pick puts back the priority a read of the board that waited for a drag answered, not the drag's older answer", async () => {
  const drag = Promise.withResolvers<Issue>();
  const pick = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches()
    .mockImplementationOnce(() => drag.promise)
    .mockImplementationOnce(() => pick.promise);
  const client = signedInHolding();
  let stored: IssuePriority | null = 3;
  const board = renderBoard(client, () => stored, { unread: true });
  const user = userEvent.setup();
  const seen: string[] = [];
  const see = (step: string) =>
    seen.push(`${step}: detail P${cachedPriorities(client).detail} board ${board.shown()}`);
  try {
    await waitFor(() => expect(board.shown()).toBe("P3"));
    await waitFor(() => expect(priorityTriggers()).toHaveLength(1));
    await waitFor(() => expect(client.isFetching()).toBe(0));
    act(() => {
      void board.moves().moveCardToLane(issue.key, "todo", 1, 0);
    });
    await waitFor(() => expect(bodies(patchIssue)).toHaveLength(1));
    // The board is read again while the drag is out, as an event of the project has it: the read
    // waits for the drag's move, and its answer is held until the board's read after the drag
    // has gone out too.
    const answerWaiting = board.hold();
    act(() => {
      void client.invalidateQueries({ queryKey: listKey });
    });
    // Dispatch answers the drag at P1, and another person sets P2 before the waiting read goes
    // out.
    stored = 2;
    await act(async () => {
      drag.resolve({ ...issue, priority: 1 });
      await drag.promise;
    });
    await waitFor(() => expect(client.isMutating()).toBe(0));
    await waitFor(() => expect(api.listIssues).toHaveBeenCalledTimes(2));
    const answerReads = board.hold();
    await act(async () => answerWaiting());
    await waitFor(() => expect(board.shown()).toBe("P2"));
    see("the card dragged to P1, the board's read that waited for it at P2");
    await pickPriority(user, priorityTriggers()[0] as HTMLElement, "0");
    await waitFor(() => expect(bodies(patchIssue)).toHaveLength(2));
    see("P0 picked");

    await act(async () => {
      pick.reject(closed());
      await pick.promise.catch(() => undefined);
    });
    await waitFor(() => expect(writesOf(client).rows()).toEqual([]));
    see("P0 refused, before the re-read answers");
    await act(async () => answerReads());
    await waitFor(() => expect(client.isFetching()).toBe(0));
    see("the re-read answers");

    expect(seen).toEqual([
      "the card dragged to P1, the board's read that waited for it at P2: detail P3 board P2",
      "P0 picked: detail P0 board P0",
      "P0 refused, before the re-read answers: detail P2 board P2",
      "the re-read answers: detail P2 board P2",
    ]);
  } finally {
    board.unmount();
    patchIssue.mockRestore();
  }
});

const unavailable = () => new ApiError(503, { code: "UNAVAILABLE", error: "the server is down" });

// R3: a read is as new as its request, and each retry of a read sends a request of its own,
// which TanStack makes within the read's one fetch. CORE-1 shows P3. The issue is read again
// while a save of its status from its header is out, and Dispatch refuses the read with a 503
// while the page is hidden, so the read's retry waits until the page is shown (TanStack goes on
// with a retry only on a page that shows). Dispatch answers the save with P1, another person
// then sets P2, and the page is shown: the retry goes out and answers P2. P0 is picked and
// refused: until the re-read answers, every surface shows P2, the retry's answer - never the P1
// of the save, which landed before the retry went out.
test("a refused pick puts back the priority a read's retry answered, not the older answer of a save that landed before the retry went out", async () => {
  const save = Promise.withResolvers<Issue>();
  const pick = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches()
    .mockImplementationOnce(() => save.promise)
    .mockImplementationOnce(() => pick.promise);
  const client = signedInHolding(
    [detailKey, { ...details, priority: 3 }],
    [listKey, [{ ...summary, priority: 3 }]],
    [inboxKey, [{ ...inboxRow, priority: 3 }]]
  );
  client.setQueryDefaults(detailKey, { retry: 1, retryDelay: 0 });
  const reads = dispatchReads();
  reads.store(3);
  // The issue's first read from now on is refused; every one after it answers.
  const refusal = Promise.withResolvers<IssueDetails>();
  let detailReads = 0;
  const refusedOnce: Reads = {
    ...reads,
    detail: () => {
      detailReads += 1;
      return detailReads === 1 ? refusal.promise : reads.detail();
    },
  };
  const view = renderIn(
    client,
    <>
      <Surfaces reads={refusedOnce} />
      <CachedPriority reads={refusedOnce} />
    </>
  );
  const user = userEvent.setup();
  const seen: string[] = [];
  const see = (step: string) => {
    const { detail, inbox, list } = shownPriorities();
    seen.push(`${step}: detail ${detail} list ${list} inbox ${inbox}`);
  };
  try {
    saveStatus(client);
    await waitFor(() => expect(bodies(patchIssue)).toHaveLength(1));
    // The issue is read again, as an event of it has it, and the page is hidden.
    let read = Promise.resolve();
    act(() => {
      read = client.refetchQueries({ queryKey: detailKey });
    });
    await waitFor(() => expect(detailReads).toBe(1));
    focusManager.setFocused(false);
    await act(async () => {
      refusal.reject(unavailable());
      await refusal.promise.catch(() => undefined);
    });
    await waitFor(() => expect(client.getQueryState(detailKey)?.fetchStatus).toBe("paused"));
    // Dispatch answers the save at P1, and another person sets P2 before the retry goes out.
    await act(async () => {
      save.resolve({ ...issue, priority: 1, status: "done" });
      await save.promise;
    });
    await waitFor(() => expect(writesOf(client).rows()).toEqual([]));
    reads.store(2);
    await act(async () => {
      focusManager.setFocused(true);
      await read;
    });
    expect(detailReads).toBe(2);
    see("the status save landed at P1, then the read's retry went out and answered P2");
    await pickPriority(user, priorityTriggers()[0] as HTMLElement, "0");
    await waitFor(() => expect(bodies(patchIssue)).toHaveLength(2));
    see("P0 picked");
    // The reads the refusal starts are held until what it put back is checked.
    const answerReads = reads.hold();

    await act(async () => {
      pick.reject(closed());
      await pick.promise.catch(() => undefined);
    });
    await waitFor(() => expect(writesOf(client).rows()).toEqual([]));
    see("P0 refused, before the re-read answers");
    await act(async () => answerReads());
    await waitFor(() => expect(client.isFetching()).toBe(0));
    see("the re-read answers");

    expect(seen).toEqual([
      "the status save landed at P1, then the read's retry went out and answered P2: detail P2 list P3 inbox P3",
      "P0 picked: detail P0 list P0 inbox P0",
      "P0 refused, before the re-read answers: detail P2 list P2 inbox P2",
      "the re-read answers: detail P2 list P2 inbox P2",
    ]);
  } finally {
    focusManager.setFocused(undefined);
    view.unmount();
    patchIssue.mockRestore();
  }
});

/** The key of CORE's open issues, as the issues table reads them. */
const openKey = projectIssuesQuery(issue.project, [], true).queryKey;

/** CORE's open issues as the issues table reads them (`projectIssuesQuery` with `open`, whose
 *  read waits until every board move on the project is answered), showing CORE-1's priority. */
function OpenIssues(): ReactNode {
  const open = useQuery({
    ...projectIssuesQuery(issue.project, [], true),
    staleTime: Number.POSITIVE_INFINITY,
  });
  return (
    <p data-testid="open-priority">P{open.data?.find((row) => row.key === issue.key)?.priority}</p>
  );
}

// R3 for a read of a project's list that waits for the project's board moves and is retried:
// each attempt of it waits for the moves unanswered as it starts, and its request goes out once
// they are answered, so each attempt is as new as its own request. The issue and CORE's open
// issues are read at P3. The card is dragged to the P1 lane, and the open issues are read again
// while the drag is out, which waits; Dispatch answers the drag at P1, and the read goes out and
// is refused with a 503 while the page is hidden, so its retry waits until the page is shown. The
// card is dragged again meanwhile, to the P0 lane, and the retry, which starts as the page is
// shown, waits for that drag in turn. Dispatch answers it at P0, another person then sets P2, and
// the retry goes out and answers P2. P1 is picked and refused: until the re-read answers, the
// issue and the open issues show P2, the retry's answer - never the second drag's P0.
test("a refused pick puts back the priority a retried read of the open issues answered after it waited for a later drag, not that drag's older answer", async () => {
  const first = Promise.withResolvers<Issue>();
  const second = Promise.withResolvers<Issue>();
  const pick = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches()
    .mockImplementationOnce(() => first.promise)
    .mockImplementationOnce(() => second.promise)
    .mockImplementationOnce(() => pick.promise);
  const client = signedInHolding();
  // The board the card is dragged on, which no surface reads.
  client.setQueryData(listKey, [{ ...summary, priority: 3 }]);
  client.setQueryDefaults(openKey, { retry: 1, retryDelay: 0 });
  const reads = dispatchReads();
  reads.store(3);
  const listIssues = spyOn(api, "listIssues").mockImplementation(reads.list);
  const view = renderIn(
    client,
    <>
      <CachedPriority reads={reads} />
      <OpenIssues />
    </>
  );
  const board = renderHook(() => useBoardMoves(issue.project), {
    wrapper: ({ children }) => (
      <QueryClientProvider client={client}>{children}</QueryClientProvider>
    ),
  });
  const user = userEvent.setup();
  const seen: string[] = [];
  const see = (step: string) => {
    const open = screen.getByTestId("open-priority").textContent;
    seen.push(`${step}: detail P${cachedPriorities(client).detail} open ${open}`);
  };
  try {
    await waitFor(() => expect(screen.getByTestId("open-priority")).toHaveTextContent("P3"));
    await waitFor(() => expect(priorityTriggers()).toHaveLength(1));
    await waitFor(() => expect(client.isFetching()).toBe(0));
    act(() => {
      void board.result.current.moveCardToLane(issue.key, "todo", 1, 0);
    });
    await waitFor(() => expect(bodies(patchIssue)).toHaveLength(1));
    // The open issues are read again while the drag is out, as an event of the project has it:
    // the read waits for the drag, and Dispatch refuses its request.
    const refusal = Promise.withResolvers<IssueSummary[]>();
    listIssues.mockImplementationOnce(() => refusal.promise);
    act(() => {
      void client.invalidateQueries({ queryKey: openKey });
    });
    await act(async () => {
      first.resolve({ ...issue, priority: 1 });
      await first.promise;
    });
    await waitFor(() => expect(client.isMutating()).toBe(0));
    await waitFor(() => expect(listIssues).toHaveBeenCalledTimes(2));
    focusManager.setFocused(false);
    await act(async () => {
      refusal.reject(unavailable());
      await refusal.promise.catch(() => undefined);
    });
    await waitFor(() => expect(client.getQueryState(openKey)?.fetchStatus).toBe("paused"));
    act(() => {
      void board.result.current.moveCardToLane(issue.key, "todo", 0, 0);
    });
    await waitFor(() => expect(bodies(patchIssue)).toHaveLength(2));
    // The page is shown: the retry starts, and waits for the second drag.
    await act(async () => focusManager.setFocused(true));
    await waitFor(() => expect(client.getQueryState(openKey)?.fetchStatus).toBe("fetching"));
    expect(listIssues).toHaveBeenCalledTimes(2);
    // Dispatch answers the second drag at P0, and another person sets P2 before the retry goes
    // out; its answer is held until the read of the open issues after the drag has gone out too.
    const answerRetry = reads.hold();
    reads.store(2);
    await act(async () => {
      second.resolve({ ...issue, priority: 0 });
      await second.promise;
    });
    await waitFor(() => expect(client.isMutating()).toBe(0));
    await waitFor(() => expect(listIssues).toHaveBeenCalledTimes(3));
    const answerReads = reads.hold();
    await act(async () => answerRetry());
    await waitFor(() => expect(screen.getByTestId("open-priority")).toHaveTextContent("P2"));
    see(
      "the card dragged to P1, then to P0, the open issues' retry that waited for the second drag at P2"
    );
    await pickPriority(user, priorityTriggers()[0] as HTMLElement, "1");
    await waitFor(() => expect(bodies(patchIssue)).toHaveLength(3));
    see("P1 picked");

    await act(async () => {
      pick.reject(closed());
      await pick.promise.catch(() => undefined);
    });
    await waitFor(() => expect(writesOf(client).rows()).toEqual([]));
    see("P1 refused, before the re-read answers");
    await act(async () => answerReads());
    await waitFor(() => expect(client.isFetching()).toBe(0));
    see("the re-read answers");

    expect(seen).toEqual([
      "the card dragged to P1, then to P0, the open issues' retry that waited for the second drag at P2: detail P3 open P2",
      "P1 picked: detail P1 open P1",
      "P1 refused, before the re-read answers: detail P2 open P2",
      "the re-read answers: detail P2 open P2",
    ]);
  } finally {
    focusManager.setFocused(undefined);
    board.unmount();
    view.unmount();
    listIssues.mockRestore();
    patchIssue.mockRestore();
  }
});

// R2 and R3 for a pick past its deadline: a pick Dispatch has not answered within it is late and
// still being tried, so it is live. P0 is answered 503 at every try, and P2, picked after it,
// waits behind it and shows. The reader stops P0 from the Pending changes list: P2 stays on
// every surface, the picker reports the stopped pick with no Retry - one would join behind P2
// and be stored last - and P2 goes out and is stored, which ends the report.
test("Stop trying on a late pick with a newer pick live behind it keeps the newer pick on every surface, reports the stopped pick with no Retry, and the newer pick is stored", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const newer = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches().mockImplementation((_key, input) =>
    input.priority === 0 ? Promise.reject(unavailable()) : newer.promise
  );
  const client = signedIn();
  const reads = dispatchReads();
  const view = renderIn(
    client,
    <>
      <CachedPriority reads={reads} />
      <Surfaces reads={reads} />
      <PendingWritesIndicator />
    </>
  );
  const line = () =>
    writesOf(client)
      .rows()
      .map((row) => [row.label, row.status]);
  try {
    const select = () => priorityTriggers()[0] as HTMLElement;
    await pick(user, select(), "0");
    await pick(user, select(), "2");
    await jump(SEND_DEADLINE_MS);
    expect(line()).toEqual([
      ["CORE-1 set to P0", "late"],
      ["CORE-1 set to P2", "queued"],
    ]);
    expect(shownPriorities()).toEqual({ detail: "P2", inbox: "P2", list: "P2" });

    await user.click(pendingRow("CORE-1 set to P0").getByRole("button", { name: "Stop trying" }));
    await jump(1_000);
    const alert = screen.getByRole("alert");
    // A pick the reader stopped may have been saved: the picker says so rather than that it
    // failed, and, with no Retry offered, that the newer pick goes after it.
    expect(alert).toHaveTextContent(
      stoppedWriteNote("the priority of CORE-1", "saved", {
        anew: "The newer change shown goes to Dispatch after it, so that one is stored last.",
      })
    );
    expect(within(alert).queryByRole("button", { name: "Retry" })).not.toBeInTheDocument();
    expect(shownValue(select())).toBe("2");
    expect(cachedPriorities(client)).toEqual({ detail: 2, inbox: 2, list: 2 });
    expect(shownPriorities()).toEqual({ detail: "P2", inbox: "P2", list: "P2" });
    // The stopped pick has left the line, and P2 is out.
    expect(line()).toEqual([["CORE-1 set to P2", "sending"]]);
    expect(bodies(patchIssue).at(-1)).toEqual({ priority: 2 });

    reads.store(2);
    await act(async () => {
      newer.resolve({ ...issue, priority: 2 });
      await newer.promise;
    });
    await jump(1_000);
    expect(line()).toEqual([]);
    // Dispatch holds P2, picked after the stopped pick: the report has nothing left to say.
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    expect(shownPriorities()).toEqual({ detail: "P2", inbox: "P2", list: "P2" });
    const sent = bodies(patchIssue).map((body) => (body as UpdateIssueInput).priority);
    expect(sent.filter((priority) => priority !== 0)).toEqual([2]);
  } finally {
    view.unmount();
    patchIssue.mockRestore();
    realClock();
  }
});

// R2: a late pick is live with no newer pick behind it as well. P0 is answered 503 at every try
// and goes late; reads of the issue, the project's list and the Inbox answer then, with the P1
// Dispatch still holds, and P0 stays shown over each. Dispatch then answers a try and stores P0.
test("a pick that is late and still being tried stays shown over the reads that answer while it is late, until Dispatch stores it", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  let down = true;
  const patchIssue = answerIssuePatches().mockImplementation((key, input) =>
    down ? Promise.reject(unavailable()) : stores(key, input)
  );
  const client = signedIn();
  const reads = dispatchReads();
  const view = renderIn(
    client,
    <>
      <CachedPriority reads={reads} />
      <Surfaces reads={reads} />
    </>
  );
  try {
    await pick(user, priorityTriggers()[0] as HTMLElement, "0");
    await jump(SEND_DEADLINE_MS);
    expect(
      writesOf(client)
        .rows()
        .map((row) => [row.label, row.status])
    ).toEqual([["CORE-1 set to P0", "late"]]);
    await act(() =>
      Promise.all(
        [detailKey, listKey, inboxKey].map((queryKey) => client.refetchQueries({ queryKey }))
      )
    );
    expect(cachedPriorities(client)).toEqual({ detail: 0, inbox: 0, list: 0 });
    expect(shownPriorities()).toEqual({ detail: "P0", inbox: "P0", list: "P0" });

    down = false;
    reads.store(0);
    await tick(2 * MAX_BACKOFF_MS);
    expect(writesOf(client).rows()).toEqual([]);
    expect(bodies(patchIssue).at(-1)).toEqual({ priority: 0 });
    expect(shownPriorities()).toEqual({ detail: "P0", inbox: "P0", list: "P0" });
  } finally {
    view.unmount();
    patchIssue.mockRestore();
    realClock();
  }
});
