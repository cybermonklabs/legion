import { expect, spyOn, test } from "bun:test";
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
import { issueQuery } from "../../api/queries";
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
  function CachedAssignee(): ReactNode {
    const cached = useQuery<IssueDetails>({
      queryFn: () => Promise.reject(new Error("the test seeds this query")),
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
    await user.selectOptions(select(), "carol");
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent('"carol" has not signed in to Dispatch');
    expect(within(alert).getByRole("button", { name: "Retry" })).toBeTruthy();
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

test("a refused priority pick with no newer live pick puts back the confirmed priority and shows its fixed line with Retry", async () => {
  const patchIssue = answerIssuePatches().mockRejectedValueOnce(
    new ApiError(409, { code: "ISSUE_CLOSED", error: "CORE-1 is closed" })
  );
  const client = signedIn();
  const view = renderIn(client, <CachedPriority reads={dispatchReads()} />);
  const user = userEvent.setup();
  try {
    await user.selectOptions(prioritySelects()[0] as HTMLSelectElement, "0");
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("Could not update the priority of CORE-1.");
    expect(within(alert).getByRole("button", { name: "Retry" })).toBeTruthy();
    expect(cachedPriorities(client)).toEqual({ detail: 1, inbox: 1, list: 1 });
    expect((prioritySelects()[0] as HTMLSelectElement).value).toBe("1");
  } finally {
    view.unmount();
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
  const client = signedIn();
  client.setQueryData(detailKey, { ...details, repo, resolved_repo: resolved });
  client.setQueryData(listKey, [{ ...summary, repo, resolved_repo: resolved }]);
  function CachedRepo(): ReactNode {
    const cached = useQuery<IssueDetails>({
      queryFn: () => Promise.reject(new Error("the test seeds this query")),
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
  const { client, described, select, unmount } = renderRepo(null, "acme/widgets");
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
