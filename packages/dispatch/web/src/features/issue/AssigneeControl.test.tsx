import { beforeAll, expect, jest, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
// biome-ignore lint/style/noRestrictedImports: this file keeps fireEvent for the focus that arms the lazy read of who has signed in, which user-event has no form for
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
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import {
  highlightedRow,
  listOf,
  openList,
  pick,
  pickNamed,
  rows,
  textBoxes,
} from "../../__tests__/list-picks";
import { answerIssuePatches } from "../../__tests__/send-answers";
import { stopTheLateWrite } from "../../__tests__/stop-late-write";
import { userOnFakeClock } from "../../__tests__/user";
import { ApiError, api, NetworkError } from "../../api/client";
import { projectDefaultAssigneesQuery, usersQuery } from "../../api/queries";
import type { InboxRow, Issue, IssueDetails, IssueSummary } from "../../api/types";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { SendStoppedError, stoppedWriteNote } from "../writes/keyed-write";
import { AssigneeControl } from "./AssigneeControl";

// The rows draw through the shared renderer, whose Markdown engine loads lazily, and they run in
// random order, so the file loads the engine before its first row, whichever row that is.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

const issue: Issue = {
  advances: [],
  advances_revision: 0,
  assignee: "alice",
  claim: null,
  components: { mode: "inherit", ids: [], unknown: [], reason: null, inherited_from: null },
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
  assignee: issue.assignee,
  claim: null,
  components: { mode: "inherit", ids: [], unknown: [], reason: null, inherited_from: null },
  key: issue.key,
  labels: [],
  last_seq: 1,
  open_asks: 0,
  progress: { tasks: null, children: null },
  parent: null,
  blocked_by: [],
  primary_artifact_id: "artifact-1",
  priority: null,
  rank: "U",
  status: issue.status,
  title: issue.title,
  updated_at: issue.updated_at,
  closed_at: issue.closed_at,
  created_by: issue.created_by,
  created_at: issue.created_at,
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
  last_reply: null,
  created_at: "2026-09-09T00:00:00Z",
  edited_at: null,
  id: "ask-1",
  issue: { assignee: issue.assignee, key: issue.key, title: issue.title },
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
  priority: null,
  snoozed_until: null,
  question: "Which layout?",
  state: "open",
  urgency: "med",
};

const detailKey = ["issue", issue.key] as const;
const listKey = ["issues", "project", issue.project] as const;
const inboxKey = ["inbox"] as const;

/** The control with the assignee its host reads from the cache, each commit of it recorded with
 *  that assignee, held while `held`. */
function CachedControl({
  commits,
  held,
}: {
  commits: Commits<string | null>;
  held: boolean;
}): ReactNode {
  const cached = useQuery<IssueDetails>({
    queryKey: detailKey,
    queryFn: () => Promise.reject(new Error("the test seeds this query")),
    staleTime: Number.POSITIVE_INFINITY,
  });
  if (cached.data === undefined) {
    return null;
  }
  return (
    <commits.Recorded host={cached.data.assignee}>
      <AssigneeControl assignee={cached.data.assignee} held={held} issueKey={cached.data.key} />
    </commits.Recorded>
  );
}

function renderControl({ held = false }: { held?: boolean } = {}) {
  const listUsers = spyOn(api, "listUsers").mockResolvedValue([
    { login: "alice" },
    { login: "bob" },
  ]);
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  queryClient.setQueryData(detailKey, details);
  queryClient.setQueryData(listKey, [summary]);
  queryClient.setQueryData(inboxKey, [inboxRow]);
  const user = userEvent.setup();
  // What the trigger's name says after its label - the person it holds, as a `<select>` says its
  // value - and the text of its badge.
  const commits = recordCommits<string | null>(() => drawnPick(trigger()));
  const tree = (heldNow: boolean) => (
    <QueryClientProvider client={queryClient}>
      <CachedControl commits={commits} held={heldNow} />
    </QueryClientProvider>
  );
  const view = render(tree(held));
  return {
    commits,
    /** Draws the control again, held while `heldNow`: the host's other write starting or
     *  ending. */
    hold: (heldNow: boolean) => view.rerender(tree(heldNow)),
    user,
    listUsers,
    queryClient,
    unmount: () => {
      view.unmount();
      listUsers.mockRestore();
    },
  };
}

/** The control's label: what its trigger's name starts with, before the badge's words, and its
 *  listbox's name with ` options` after it. */
const ASSIGNEE = "Assignee of CORE-1";

function trigger(): HTMLButtonElement {
  return screen.getByRole("button", { name: pickNamed(ASSIGNEE) }) as HTMLButtonElement;
}

/** Resolves once who has signed in has been read. */
async function peopleRead(queryClient: QueryClient): Promise<void> {
  await waitFor(() =>
    expect(queryClient.getQueryState(usersQuery().queryKey)?.status).toBe("success")
  );
}

test("AssigneeControl reads who has signed in only once the reader reaches for it, then lists Unassigned first", async () => {
  const { listUsers, unmount, user } = renderControl();
  try {
    // Mounted with the badge showing, nothing has been requested: a fresh issue load costs no
    // read of who has signed in.
    expect(screen.getByText("alice", { selector: "span" })).toBeTruthy();
    expect(trigger()).toHaveAccessibleName(`${ASSIGNEE} alice`);
    expect(listUsers).not.toHaveBeenCalled();

    // A focus with no press before it: user-event has no form for a bare focus.
    fireEvent.focus(trigger());
    await waitFor(() => expect(listUsers).toHaveBeenCalledTimes(1));
    const list = await openList(user, ASSIGNEE);
    await waitFor(() => expect(rows(list)).toEqual(["Unassigned", "✓alice", "bob"]));
    expect(listUsers).toHaveBeenCalledTimes(1);
  } finally {
    unmount();
  }
});

test("AssigneeControl reassigns optimistically on every surface and merges the server's answer", async () => {
  const save = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches().mockImplementation(() => save.promise);
  const { queryClient, unmount, user } = renderControl();
  try {
    await user.hover(trigger());
    await pick(user, ASSIGNEE, "bob");
    await waitFor(() => expect(trigger()).toHaveAccessibleName(`${ASSIGNEE} bob`));
    expect(patchIssue).toHaveBeenCalledWith("CORE-1", { assignee: "bob" }, keyedAttempt);
    expect(queryClient.getQueryData<IssueSummary[]>(listKey)?.[0]?.assignee).toBe("bob");
    expect(queryClient.getQueryData<InboxRow[]>(inboxKey)?.[0]?.issue?.assignee).toBe("bob");

    act(() => {
      save.resolve({ ...issue, assignee: "bob", last_seq: 2 });
    });
    await waitFor(() =>
      expect(queryClient.getQueryData<IssueDetails>(detailKey)?.last_seq).toBe(2)
    );
  } finally {
    unmount();
    patchIssue.mockRestore();
  }
});

// A pick shows from its press on, in every commit. The pick reaches the issue's cache in a
// microtask of the press, and the control's host only when TanStack Query tells the cache's
// observers, in a browser a timer later, which the row holds back. The list closes on the pick,
// and its trigger draws the person the control holds, so a commit before that timer that drew the
// assignee the pick replaces is a frame of it on screen (INFRA-3904).
test("AssigneeControl draws the pick in every commit from the pick on, while its host still passes the assignee it replaces", async () => {
  const save = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches().mockImplementation(() => save.promise);
  const { commits, queryClient, unmount, user } = renderControl();
  let notifications: HeldNotifications | undefined;
  try {
    // A focus with no press before it: user-event has no form for a bare focus.
    fireEvent.focus(trigger());
    const list = await openList(user, ASSIGNEE);
    await waitFor(() => expect(rows(list)).toHaveLength(3));
    const bob = within(list).getByRole("option", { name: "bob" });
    const fromPick = commits.markAt(bob, "click");
    notifications = holdNotifications();
    await user.click(bob);
    await waitFor(() =>
      expect(patchIssue).toHaveBeenCalledWith("CORE-1", { assignee: "bob" }, keyedAttempt)
    );
    // The cache draws the pick, and its host has not been told.
    expect(queryClient.getQueryData<IssueDetails>(detailKey)?.assignee).toBe("bob");
    expect(new Set(fromPick().map(({ host }) => host))).toEqual(new Set(["alice"]));
    notifications.tell();
    expect(fromPick().at(-1)?.host).toBe("bob");
    await act(async () => {
      save.resolve({ ...issue, assignee: "bob", last_seq: 2 });
      await save.promise;
    });
    await waitFor(() =>
      expect(queryClient.getQueryData<IssueDetails>(detailKey)?.last_seq).toBe(2)
    );
    expect(fromPick().map(({ drawn }) => drawn)).toEqual(fromPick().map(() => "bob bob"));
  } finally {
    notifications?.tell();
    unmount();
    patchIssue.mockRestore();
  }
});

test("AssigneeControl sends null for Unassigned", async () => {
  const patchIssue = answerIssuePatches().mockResolvedValue({ ...issue, assignee: null });
  const { unmount, user } = renderControl();
  try {
    // A focus with no press before it: user-event has no form for a bare focus.
    fireEvent.focus(trigger());
    await pick(user, ASSIGNEE, "Unassigned");
    await waitFor(() =>
      expect(patchIssue).toHaveBeenCalledWith("CORE-1", { assignee: null }, keyedAttempt)
    );
    await waitFor(() => expect(trigger()).toHaveAccessibleName(`${ASSIGNEE} Unassigned`));
    expect(screen.getByText("Unassigned", { selector: "span" })).toBeTruthy();
  } finally {
    unmount();
    patchIssue.mockRestore();
  }
});

// A question on an issue nobody holds reaches the project's default assignee, so the unassign's
// optimistic write must leave the issue's rows that nobody handed on waiting on that person: a
// row that named nobody would drop out of the default person's own (`assignee=me`) list until
// the PATCH settled. Where the client holds no defaults read, the row keeps the person it waited
// on rather than naming nobody.
test.each([
  ["names the project's default where the defaults read holds it", true, "carol"],
  ["keeps the person it waited on where the client has no defaults read", false, "alice"],
])("AssigneeControl's unassign leaves an inbox row nobody handed on waiting on a person: it %s", async (_name, defaultsRead, person) => {
  const save = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches().mockImplementation(() => save.promise);
  const { queryClient, unmount, user } = renderControl();
  const handedOn: InboxRow = {
    ...inboxRow,
    asked_of: "bob",
    asked_of_at: "2026-09-09T01:00:00Z",
    asked_of_by: "alice",
    id: "ask-2",
    waiting_on_person: "bob",
  };
  queryClient.setQueryData(inboxKey, [{ ...inboxRow, waiting_on_person: "alice" }, handedOn]);
  if (defaultsRead) {
    queryClient.setQueryData(projectDefaultAssigneesQuery().queryKey, [
      { project: "OPS", assignee: "dana" },
      { project: "CORE", assignee: "carol" },
    ]);
  }
  try {
    // A focus with no press before it: user-event has no form for a bare focus.
    fireEvent.focus(trigger());
    await pick(user, ASSIGNEE, "Unassigned");
    await waitFor(() =>
      expect(patchIssue).toHaveBeenCalledWith("CORE-1", { assignee: null }, keyedAttempt)
    );

    // The save is still in flight: what the rows hold is the optimistic write alone.
    const rows = queryClient.getQueryData<InboxRow[]>(inboxKey) ?? [];
    expect(rows.map((row) => [row.id, row.issue?.assignee, row.waiting_on_person])).toEqual([
      ["ask-1", null, person],
      ["ask-2", null, "bob"],
    ]);

    await act(async () => {
      save.resolve({ ...issue, assignee: null });
      await save.promise;
    });
    await waitFor(() =>
      expect(queryClient.getQueryData<IssueDetails>(detailKey)?.assignee).toBeNull()
    );
  } finally {
    unmount();
    patchIssue.mockRestore();
  }
});

test("AssigneeControl rolls back and shows the server's refusal when the person has not signed in", async () => {
  const patchIssue = answerIssuePatches().mockRejectedValue(
    new ApiError(400, {
      code: "ASSIGNEE_NOT_ALLOWED",
      error: '"bob" has not signed in to Dispatch',
    })
  );
  const { queryClient, unmount, user } = renderControl();
  try {
    // A focus with no press before it: user-event has no form for a bare focus.
    fireEvent.focus(trigger());
    await pick(user, ASSIGNEE, "bob");
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent('"bob" has not signed in to Dispatch');
    expect(trigger()).toHaveAccessibleName(`${ASSIGNEE} alice`);
    expect(queryClient.getQueryData<IssueSummary[]>(listKey)).toEqual([summary]);
    expect(queryClient.getQueryData<InboxRow[]>(inboxKey)).toEqual([inboxRow]);
  } finally {
    unmount();
    patchIssue.mockRestore();
  }
});

test("AssigneeControl keeps focus while a save is in flight and saves a pick made meanwhile after it", async () => {
  const first = Promise.withResolvers<Issue>();
  const second = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches()
    .mockImplementationOnce(() => first.promise)
    .mockImplementationOnce(() => second.promise);
  const { queryClient, unmount, user } = renderControl();
  try {
    act(() => trigger().focus());
    await peopleRead(queryClient);
    // From the keyboard: Enter opens the list on alice, ArrowDown highlights bob, Enter picks him.
    await user.keyboard("{Enter}{ArrowDown}{Enter}");
    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { assignee: "bob" }, keyedAttempt)
    );
    // A disabled control drops focus and leaves the tab order for the length of the save.
    expect(trigger()).not.toBeDisabled();
    expect(trigger()).not.toHaveAttribute("aria-disabled");
    expect(trigger()).toHaveFocus();

    // Home on the closed badge opens the list on Unassigned, and Enter picks it.
    await user.keyboard("{Home}{Enter}");
    expect(trigger()).toHaveAccessibleName(`${ASSIGNEE} Unassigned`);
    expect(patchIssue).toHaveBeenCalledTimes(1);
    await act(async () => {
      first.resolve({ ...issue, assignee: "bob" });
      await first.promise;
    });
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(2));
    expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { assignee: null }, keyedAttempt);
    expect(trigger()).toHaveAccessibleName(`${ASSIGNEE} Unassigned`);
    await act(async () => {
      second.resolve({ ...issue, assignee: null });
      await second.promise;
    });
    await waitFor(() =>
      expect(queryClient.getQueryData<IssueDetails>(detailKey)?.assignee).toBeNull()
    );
    expect(trigger()).toHaveFocus();
  } finally {
    unmount();
    patchIssue.mockRestore();
  }
});

// INFRA-3135, R3: "Every list of options a reader picks from holds a text box that narrows the
// options." The issue header's Assignee is such a list: its options are the people who have
// signed in, which grow with the team. Opened, it holds a text box of its own, a word typed into
// it leaves only the people who match, and a pick reassigns the issue and puts the focus back on
// the control. Opened again it stands on the person it holds, and Enter there writes nothing.
test("the issue header's Assignee list holds a text box that narrows its people, and a pick reassigns the issue", async () => {
  const patchIssue = answerIssuePatches().mockResolvedValue({ ...issue, assignee: "bob" });
  const { unmount, user } = renderControl();
  try {
    const before = new Set(textBoxes());
    await user.click(screen.getByRole("button", { name: pickNamed("Assignee of CORE-1") }));
    // The people are read once the reader reaches for the control.
    await waitFor(() =>
      expect(screen.queryAllByRole("option", { name: /^bob\b/ }).length).toBeGreaterThan(0)
    );
    const own = textBoxes().filter((box) => !before.has(box));
    expect(own, "the Assignee list holds no text box of its own").toHaveLength(1);
    const list = listOf(own[0] as HTMLElement);
    expect(within(list).getByRole("option", { name: /^alice\b/ })).toHaveAttribute(
      "aria-selected",
      "true"
    );
    await user.type(own[0] as HTMLElement, "bo");
    await waitFor(() =>
      expect(
        within(list).queryAllByRole("option", { name: /^alice\b/ }),
        `"bo" leaves alice in the Assignee list`
      ).toHaveLength(0)
    );
    expect(
      within(list).queryAllByRole("option", { name: /^Unassigned\b/ }),
      `"bo" leaves Unassigned in the Assignee list`
    ).toHaveLength(0);
    await user.click(within(list).getByRole("option", { name: /^bob\b/ }));
    await waitFor(() =>
      expect(patchIssue).toHaveBeenCalledWith("CORE-1", { assignee: "bob" }, keyedAttempt)
    );
    expect(screen.queryByRole("listbox", { name: "Assignee of CORE-1 options" })).toBeNull();
    expect(trigger()).toHaveFocus();
    await waitFor(() => expect(trigger()).toHaveAccessibleName(`${ASSIGNEE} bob`));

    await user.keyboard("{Enter}");
    await screen.findByRole("listbox", { name: "Assignee of CORE-1 options" });
    await user.keyboard("{Enter}");
    expect(screen.queryByRole("listbox", { name: "Assignee of CORE-1 options" })).toBeNull();
    await act(async () => {});
    expect(patchIssue).toHaveBeenCalledTimes(1);
  } finally {
    unmount();
    patchIssue.mockRestore();
  }
});

// INFRA-3135, round 12: a list whose read failed says so. Until the people answer, the list holds
// Unassigned and the person the issue names; when the read fails it still holds them, and says
// the rest could not be loaded, as the ask card's hand-on list does.
test("the issue header's Assignee list says the people could not be loaded when their read fails", async () => {
  const { listUsers, unmount, user } = renderControl();
  listUsers.mockRejectedValue(new Error("users unreachable"));
  try {
    const list = await openList(user, ASSIGNEE);
    const panel = list.parentElement as HTMLElement;
    expect(await within(panel).findByText("Could not load the people list.")).toBeTruthy();
    expect(rows(list)).toEqual(["Unassigned", "✓alice"]);
  } finally {
    unmount();
  }
});

// dispatch://INFRA-2160, R6: a key on the closed badge only moves the list's highlight - a
// native select would write the next person on ArrowDown - and Escape and Tab write nothing and
// leave the badge on the issue's assignee. Enter writes the person highlighted.
test("AssigneeControl's keys on its closed badge only move the list's highlight, and only Enter writes", async () => {
  const patchIssue = answerIssuePatches().mockResolvedValue({ ...issue, assignee: "bob" });
  const { queryClient, unmount, user } = renderControl();
  try {
    act(() => trigger().focus());
    await peopleRead(queryClient);
    for (const [keys, to] of [
      ["{ArrowDown}", "bob"],
      ["{ArrowUp}", "Unassigned"],
      ["{Home}", "Unassigned"],
      ["{End}", "bob"],
      ["{PageDown}", "bob"],
      ["{PageUp}", "Unassigned"],
      ["b", "bob"],
    ] as const) {
      await user.keyboard(keys);
      expect(
        highlightedRow(screen.getByRole("combobox", { name: "Search people" })),
        `${keys} highlights ${to}`
      ).toBe(to);
      await user.keyboard("{Escape}");
      expect(screen.queryByRole("listbox")).not.toBeInTheDocument();
      expect(trigger()).toHaveFocus();
      expect(trigger()).toHaveAccessibleName(`${ASSIGNEE} alice`);
    }
    await user.keyboard("{ArrowDown}");
    await user.tab();
    expect(screen.queryByRole("listbox")).not.toBeInTheDocument();
    expect(trigger()).toHaveFocus();
    expect(trigger()).toHaveAccessibleName(`${ASSIGNEE} alice`);
    expect(patchIssue).not.toHaveBeenCalled();

    await user.keyboard("{ArrowDown}{Enter}");
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(1));
    expect(patchIssue).toHaveBeenCalledWith("CORE-1", { assignee: "bob" }, keyedAttempt);
    // A pick of the person it already names writes nothing, as a `<select>` set to its own value.
    await waitFor(() => expect(trigger()).toHaveAccessibleName(`${ASSIGNEE} bob`));
    await user.keyboard("{Enter}{Enter}");
    expect(patchIssue).toHaveBeenCalledTimes(1);
  } finally {
    unmount();
    patchIssue.mockRestore();
  }
});

// `held` is the issue page's other field writes: the badge keeps the focus and its Tab stop and
// says it is busy, and neither a press nor a key on it opens the list while the hold lasts, so the
// reader never picks a person the hold would drop with no word. A list the reader opened before
// the hold began drops a pick made in it, rather than queueing it.
test("a held AssigneeControl keeps its focus and Tab stop, opens no list, and drops a pick from a list opened before the hold", async () => {
  const patchIssue = answerIssuePatches().mockResolvedValue({ ...issue, assignee: "bob" });
  const { hold, queryClient, unmount, user } = renderControl({ held: true });
  try {
    act(() => trigger().focus());
    await peopleRead(queryClient);
    expect(trigger()).toHaveAttribute("aria-disabled", "true");
    expect(trigger()).not.toBeDisabled();
    await user.click(trigger());
    expect(screen.queryByRole("listbox"), "a press opened the list").not.toBeInTheDocument();
    for (const keys of [
      "{ArrowDown}",
      "{ArrowUp}",
      "{Home}",
      "{End}",
      "{PageDown}",
      "{PageUp}",
      "b",
      "{Enter}",
      " ",
    ]) {
      await user.keyboard(keys);
      expect(screen.queryByRole("listbox"), `${keys} opened the list`).not.toBeInTheDocument();
      expect(trigger()).toHaveFocus();
    }
    expect(trigger()).toHaveAccessibleName(`${ASSIGNEE} alice`);

    hold(false);
    const list = await openList(user, ASSIGNEE);
    hold(true);
    await user.click(within(list).getByRole("option", { name: "bob" }));
    expect(trigger()).toHaveFocus();
    expect(trigger()).toHaveAccessibleName(`${ASSIGNEE} alice`);
    await act(async () => {});
    expect(patchIssue).not.toHaveBeenCalled();
  } finally {
    unmount();
    patchIssue.mockRestore();
  }
});

// A save the reader stopped may have been made: the control says so in the field's own words
// (`useIssueFieldWrite`), and that its Retry asks again under the save's key, rather than the
// stopped write's own words, which are a composer's, of a message.
test("an assignee the reader stopped says Retry asks again under its key, not a message's words", async () => {
  const patchIssue = answerIssuePatches().mockRejectedValue(
    new NetworkError("/api/v1/issues/CORE-1", new TypeError("Failed to fetch"))
  );
  const { queryClient, unmount, user } = renderControl();
  try {
    // The list is open, with everyone who has signed in, before the clock is held.
    const list = await openList(user, ASSIGNEE);
    await waitFor(() => expect(rows(list)).toEqual(["Unassigned", "✓alice", "bob"]));
    jest.useFakeTimers();
    await userOnFakeClock().click(within(list).getByRole("option", { name: /^bob\b/ }));
    await stopTheLateWrite(queryClient);
    // The alert's paragraph, without its Retry button's name.
    expect(screen.getByRole("alert").querySelector("p")?.textContent).toBe(
      stoppedWriteNote("the assignee of CORE-1", "saved", {
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
