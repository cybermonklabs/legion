import { beforeAll, expect, jest, test } from "bun:test";
import { QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
// biome-ignore lint/style/noRestrictedImports: this file keeps fireEvent for the bare mouse, pointer and touch presses a draggable parent must not see, which user-event has no form for
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import {
  type Commits,
  drawnPick,
  type HeldNotifications,
  holdNotifications,
  recordCommits,
} from "../../__tests__/commits";
import { realClock } from "../../__tests__/fake-clock";
import { heldNotDisabled } from "../../__tests__/held-press";
import { bodies } from "../../__tests__/issue-picks";
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { listOf, openList, pick, pickNamed, rows, textBoxes } from "../../__tests__/list-picks";
import { answerIssuePatches } from "../../__tests__/send-answers";
import { stopTheLateWrite } from "../../__tests__/stop-late-write";
import { userOnFakeClock } from "../../__tests__/user";
import { NetworkError } from "../../api/client";
import type { InboxRow, Issue, IssueDetails, IssuePriority, IssueSummary } from "../../api/types";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { SendStoppedError, stoppedWriteNote } from "../writes/keyed-write";
import { issuePriority } from "./issue-priority";
import { PriorityControl, PriorityEditor } from "./PriorityControl";
import { useIssuePriority } from "./useIssuePriority";

// The rows draw through the shared renderer, whose Markdown engine loads lazily, and they run in
// random order, so the file loads the engine before its first row, whichever row that is.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

const issue: Issue = {
  advances: [],
  advances_revision: 0,
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
  advances: [],
  advances_revision: 0,
  route: null,
  repo: null,
  resolved_repo: null,
  route_status: null,
  route_holder: null,
  route_status_reason: null,
  key: issue.key,
  project: issue.project,
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

/** The issue as the case seeds it in the cache, which no read replaces. */
function useCachedIssue(): IssueDetails | undefined {
  return useQuery<IssueDetails>({
    queryKey: detailKey,
    queryFn: () => Promise.reject(new Error("the test seeds this query")),
    staleTime: Number.POSITIVE_INFINITY,
  }).data;
}

/** Renders the control the way every surface does: with the priority it reads from the cache,
 *  each commit of it recorded with that priority. */
function CachedControl({ commits }: { commits: Commits<IssuePriority | null> }): ReactNode {
  const cached = useCachedIssue();
  if (cached === undefined) {
    return null;
  }
  const host = issuePriority(cached.priority);
  return (
    <commits.Recorded host={host}>
      <PriorityControl issueKey={cached.key} priority={host} />
    </commits.Recorded>
  );
}

/** The priority as the issue page draws it: the page reads the issue and makes the write in one
 *  component, and the header's editor draws the write (`IssuePage`, `IssueHeader`), each commit
 *  of it recorded with the priority the page read. */
function PageControl({ commits }: { commits: Commits<IssuePriority | null> }): ReactNode {
  const host = issuePriority(useCachedIssue()?.priority);
  const write = useIssuePriority(issue.key, host);
  return (
    <commits.Recorded host={host}>
      <PriorityEditor issueKey={issue.key} write={write} />
    </commits.Recorded>
  );
}

function renderControl({
  page = false,
  priority = issuePriority(details.priority),
}: {
  page?: boolean;
  priority?: IssuePriority | null;
} = {}) {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  queryClient.setQueryData(detailKey, { ...details, priority });
  queryClient.setQueryData(listKey, [summary, otherSummary]);
  queryClient.setQueryData(inboxKey, [inboxRow]);
  const user = userEvent.setup();
  const commits = recordCommits<IssuePriority | null>(() => drawnPick(trigger()));
  const view = render(
    <QueryClientProvider client={queryClient}>
      {page ? <PageControl commits={commits} /> : <CachedControl commits={commits} />}
    </QueryClientProvider>
  );
  return { commits, queryClient, unmount: view.unmount, user };
}

/** The control's label: what its trigger's name starts with, before the badge's words, and its
 *  listbox's name with ` options` after it. */
const PRIORITY = "Priority of CORE-1";

function trigger(): HTMLButtonElement {
  return screen.getByRole("button", { name: pickNamed(PRIORITY) }) as HTMLButtonElement;
}

/** The open priority list, or `null` while it is shut. */
function openPriorityList(): HTMLElement | null {
  return screen.queryByRole("listbox", { name: `${PRIORITY} options` });
}

test("PriorityControl shows the issue's priority and offers Unset and P0-P3", async () => {
  const { unmount, user } = renderControl();
  try {
    expect(screen.getByText("P1", { selector: "span" })).toBeTruthy();
    expect(trigger()).toHaveAccessibleName(`${PRIORITY} P1`);
    const list = await openList(user, PRIORITY);
    expect(rows(list)).toEqual(["Unset", "P0", "✓P1", "P2", "P3"]);
  } finally {
    unmount();
  }
});

// INFRA-3135, R3: "Every list of options a reader picks from holds a text box that narrows the
// options." The priority is such a list. Opened, it holds a text box of its own, `P1` typed into
// it leaves only P1, and a pick writes that priority and puts the focus back on the control.
test("the priority list holds a text box that narrows its options to the typed P1, and a pick writes P1", async () => {
  const patchIssue = answerIssuePatches().mockResolvedValue({ ...issue, priority: 1 });
  const { unmount, user } = renderControl({ priority: 3 });
  try {
    const before = new Set(textBoxes());
    await user.click(trigger());
    const own = textBoxes().filter((box) => !before.has(box));
    expect(own, "the priority list holds no text box of its own").toHaveLength(1);
    const list = listOf(own[0] as HTMLElement);
    expect(rows(list)).toEqual(["Unset", "P0", "P1", "P2", "✓P3"]);
    await user.type(own[0] as HTMLElement, "P1");
    expect(rows(list), `"P1" leaves other options than P1 in the priority list`).toEqual(["P1"]);
    await user.click(within(list).getByRole("option", { name: "P1" }));
    await waitFor(() =>
      expect(patchIssue).toHaveBeenCalledWith("CORE-1", { priority: 1 }, keyedAttempt)
    );
    expect(bodies(patchIssue)).toEqual([{ priority: 1 }]);
    expect(openPriorityList()).toBeNull();
    expect(trigger()).toHaveFocus();
    expect(trigger()).toHaveAccessibleName(`${PRIORITY} P1`);
  } finally {
    unmount();
    patchIssue.mockRestore();
  }
});

test("PriorityControl saves the picked priority, shows it before the server answers, and keeps focus", async () => {
  const save = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches().mockImplementation(() => save.promise);
  const { queryClient, unmount, user } = renderControl();
  try {
    act(() => trigger().focus());
    await pick(user, PRIORITY, "P2");
    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { priority: 2 }, keyedAttempt)
    );
    expect(trigger()).toHaveAccessibleName(`${PRIORITY} P2`);
    expect(screen.getByText("P2", { selector: "span" })).toBeTruthy();
    // A disabled control drops focus and leaves the tab order for the length of the save, so
    // the trigger takes a press while the request is in flight.
    expect(trigger().disabled).toBe(false);
    expect(trigger()).not.toHaveAttribute("aria-disabled");
    expect(trigger()).toHaveFocus();
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
    expect(trigger()).toHaveFocus();
  } finally {
    unmount();
    patchIssue.mockRestore();
  }
});

// A pick shows from its press on, in every commit. The pick reaches the issue's cache in a
// microtask of the press, and the control's host only when TanStack Query tells the cache's
// observers, in a browser a timer later, which the row holds back. The list closes on the pick,
// and its trigger draws the priority the control holds, so a commit before that timer that drew
// the priority the pick replaces is a frame of it on screen (INFRA-3904).
test("PriorityControl draws the pick in every commit from the pick on, while its host still passes the priority it replaces", async () => {
  const save = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches().mockImplementation(() => save.promise);
  const { commits, queryClient, unmount, user } = renderControl();
  let notifications: HeldNotifications | undefined;
  try {
    const list = await openList(user, PRIORITY);
    const p2 = within(list).getByRole("option", { name: "P2" });
    const fromPick = commits.markAt(p2, "click");
    notifications = holdNotifications();
    await user.click(p2);
    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { priority: 2 }, keyedAttempt)
    );
    // The cache draws the pick, and its host has not been told.
    expect(queryClient.getQueryData<IssueDetails>(detailKey)?.priority).toBe(2);
    expect(new Set(fromPick().map(({ host }) => host))).toEqual(new Set([1]));
    notifications.tell();
    expect(fromPick().at(-1)?.host).toBe(2);
    await act(async () => {
      save.resolve({ ...issue, priority: 2, last_seq: 2 });
      await save.promise;
    });
    await waitFor(() =>
      expect(queryClient.getQueryData<IssueDetails>(detailKey)?.last_seq).toBe(2)
    );
    expect(fromPick().map(({ drawn }) => drawn)).toEqual(fromPick().map(() => "P2 P2"));
  } finally {
    notifications?.tell();
    unmount();
    patchIssue.mockRestore();
  }
});

// On the issue page the page reads the issue and makes the write in one component, so each
// render of it reads the cache as it is then, though TanStack Query has not told it of the cache
// yet. A second pick made before that telling is drawn from its press, as the first was, and
// never the first pick it replaces; each pick is drawn through both saves.
test("the issue page's priority editor draws each of two picks made before its host is told, from each press on", async () => {
  const first = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches()
    .mockImplementationOnce(() => first.promise)
    .mockResolvedValue({ ...issue, priority: 3, last_seq: 3 });
  const { commits, queryClient, unmount, user } = renderControl({ page: true });
  let notifications: HeldNotifications | undefined;
  try {
    const p2 = within(await openList(user, PRIORITY)).getByRole("option", { name: "P2" });
    const fromFirst = commits.markAt(p2, "click");
    notifications = holdNotifications();
    await user.click(p2);
    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { priority: 2 }, keyedAttempt)
    );
    const p3 = within(await openList(user, PRIORITY)).getByRole("option", { name: "P3" });
    const fromSecond = commits.markAt(p3, "click");
    await user.click(p3);
    // The second pick waits behind the first, and the cache draws it.
    await waitFor(() =>
      expect(queryClient.getQueryData<IssueDetails>(detailKey)?.priority).toBe(3)
    );
    notifications.tell();
    await act(async () => {
      first.resolve({ ...issue, priority: 2, last_seq: 2 });
      await first.promise;
    });
    await waitFor(() =>
      expect(queryClient.getQueryData<IssueDetails>(detailKey)?.last_seq).toBe(3)
    );
    expect(bodies(patchIssue)).toEqual([{ priority: 2 }, { priority: 3 }]);
    const betweenPresses = fromFirst().slice(0, fromFirst().length - fromSecond().length);
    expect(betweenPresses.length).toBeGreaterThan(0);
    expect(betweenPresses.map(({ drawn }) => drawn)).toEqual(betweenPresses.map(() => "P2 P2"));
    expect(fromSecond().map(({ drawn }) => drawn)).toEqual(fromSecond().map(() => "P3 P3"));
  } finally {
    notifications?.tell();
    unmount();
    patchIssue.mockRestore();
  }
});

test("PriorityControl sends null for Unset and shows the muted tag", async () => {
  const patchIssue = answerIssuePatches().mockResolvedValue({ ...issue, priority: null });
  const { unmount, user } = renderControl();
  try {
    await pick(user, PRIORITY, "Unset");
    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { priority: null }, keyedAttempt)
    );
    expect(trigger()).toHaveAccessibleName(`${PRIORITY} Priority`);
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
    await pick(user, PRIORITY, "P0");
    await waitFor(() => expect(trigger()).toHaveAccessibleName(`${PRIORITY} P0`));

    act(() => {
      save.reject(new Error("offline"));
    });
    await screen.findByRole("alert");
    expect(trigger()).toHaveAccessibleName(`${PRIORITY} P1`);
    expect(trigger().disabled).toBe(false);
    expect(queryClient.getQueryData<IssueSummary[]>(listKey)).toEqual([summary, otherSummary]);
    expect(queryClient.getQueryData<InboxRow[]>(inboxKey)).toEqual([inboxRow]);

    patchIssue.mockResolvedValue({ ...issue, priority: 0 });
    await user.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(2));
    expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { priority: 0 }, keyedAttempt);
    await waitFor(() => expect(screen.queryByRole("alert")).not.toBeInTheDocument());
    expect(trigger()).toHaveAccessibleName(`${PRIORITY} P0`);
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
    await pick(user, PRIORITY, "P0");
    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { priority: 0 }, keyedAttempt)
    );
    await pick(user, PRIORITY, "P3");
    await pick(user, PRIORITY, "P2");
    // One request at a time: the later picks wait for the first answer, but the badge already
    // shows the newest one and never snaps back to the value still being saved.
    expect(trigger()).toHaveAccessibleName(`${PRIORITY} P2`);
    expect(screen.getByText("P2", { selector: "span" })).toBeTruthy();
    expect(patchIssue).toHaveBeenCalledTimes(1);

    await act(async () => {
      first.resolve({ ...issue, priority: 0 });
      await first.promise;
    });
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(2));
    expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { priority: 2 }, keyedAttempt);
    expect(trigger()).toHaveAccessibleName(`${PRIORITY} P2`);

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

test("a queued pick that fails rolls back to the priority the server last confirmed, not the pick", async () => {
  const first = Promise.withResolvers<Issue>();
  const second = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches()
    .mockImplementationOnce(() => first.promise)
    .mockImplementationOnce(() => second.promise);
  const { queryClient, unmount, user } = renderControl();
  try {
    await pick(user, PRIORITY, "P0");
    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { priority: 0 }, keyedAttempt)
    );
    await pick(user, PRIORITY, "P2");
    expect(trigger()).toHaveAccessibleName(`${PRIORITY} P2`);

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
    expect(trigger()).toHaveAccessibleName(`${PRIORITY} P0`);
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

// A board card is a drag activator: a press that reached it would lift the card. A press on the
// badge opens the list instead, and neither that press nor one in the list it opens (portalled,
// but a React child of the control) reaches the card.
test("PriorityControl keeps mouse, pointer and touch presses on its trigger and in its list out of a draggable parent, and a press opens the list", async () => {
  const patchIssue = answerIssuePatches().mockResolvedValue({ ...issue, priority: 2 });
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const user = userEvent.setup();
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
    fireEvent.mouseDown(trigger());
    fireEvent.pointerDown(trigger());
    fireEvent.touchStart(trigger());
    expect(reached, "a bare press on the trigger reached the card").toEqual([]);
    expect(openPriorityList(), "a bare press opened the list").toBeNull();

    const list = await openList(user, PRIORITY);
    expect(reached, "the press that opened the list reached the card").toEqual([]);
    await user.click(within(list).getByRole("option", { name: "P2" }));
    expect(reached, "the press in the list reached the card").toEqual([]);
    await waitFor(() =>
      expect(patchIssue).toHaveBeenCalledWith("CORE-1", { priority: 2 }, keyedAttempt)
    );
  } finally {
    view.unmount();
    patchIssue.mockRestore();
  }
});

// While its host's `disabled` holds - a done card, a closed issue - the trigger is
// `aria-disabled`, never `disabled`: a focused control that disables itself drops the focus onto
// the page's body. It keeps the reader's focus, and no press, key or pick of it writes; a list
// that stood open as the hold began writes nothing either.
test("the host's disabled holds the priority's trigger by aria-disabled: it keeps the focus, opens no list and writes nothing", async () => {
  const patchIssue = answerIssuePatches().mockResolvedValue({ ...issue, priority: 2 });
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const user = userEvent.setup();
  const control = (disabled: boolean) => (
    <QueryClientProvider client={queryClient}>
      <PriorityControl disabled={disabled} issueKey="CORE-1" priority={1} />
    </QueryClientProvider>
  );
  const view = render(control(false));
  try {
    const list = await openList(user, PRIORITY);
    view.rerender(control(true));
    await user.click(within(list).getByRole("option", { name: "P2" }));
    expect(trigger()).toHaveFocus();

    expect(heldNotDisabled(trigger(), "the priority's trigger")).toBe(true);
    await user.keyboard("{Enter}");
    await user.keyboard("1");
    await user.click(trigger());
    expect(openPriorityList(), "a held trigger opened its list").toBeNull();
    expect(trigger()).toHaveFocus();
    expect(trigger()).toHaveAccessibleName(`${PRIORITY} P1`);
    await act(async () => {});
    expect(patchIssue).not.toHaveBeenCalled();

    view.rerender(control(false));
    expect(trigger()).not.toHaveAttribute("aria-disabled");
    expect(trigger()).toHaveFocus();
    await user.keyboard("{Enter}");
    expect(await screen.findByRole("listbox", { name: `${PRIORITY} options` })).toBeTruthy();
  } finally {
    view.unmount();
    patchIssue.mockRestore();
  }
});

// A pick the reader stopped may have been saved: the control says so, and that its Retry asks
// again under the save's key (`retryLast`), rather than a failure it cannot know of.
test("a priority the reader stopped says Retry asks again under its key, not that it failed", async () => {
  const patchIssue = answerIssuePatches().mockRejectedValue(
    new NetworkError("/api/v1/issues/CORE-1", new TypeError("Failed to fetch"))
  );
  const { queryClient, unmount, user } = renderControl();
  try {
    const list = await openList(user, PRIORITY);
    jest.useFakeTimers();
    await userOnFakeClock().click(within(list).getByRole("option", { name: "P0" }));
    await stopTheLateWrite(queryClient);
    // The alert's paragraph, without its Retry button's name.
    expect(screen.getByRole("alert").querySelector("p")?.textContent).toBe(
      stoppedWriteNote("the priority of CORE-1", "saved", {
        method: "PATCH",
        stopped: new SendStoppedError(),
      })
    );
  } finally {
    realClock();
    unmount();
    queryClient.clear();
    patchIssue.mockRestore();
  }
});
