import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
// biome-ignore lint/style/noRestrictedImports: this file keeps fireEvent for the focus that arms the lazy read of the project list, which user-event has no form for
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";
import type { ReactNode } from "react";
import {
  type Commits,
  type HeldNotifications,
  holdNotifications,
  recordCommits,
} from "../../__tests__/commits";
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
import { ApiError, api } from "../../api/client";
import type { Issue, IssueDetails, IssueSummary, Project } from "../../api/types";
import { RepoControl } from "./RepoControl";

const issue: Issue = {
  advances: [],
  advances_revision: 0,
  assignee: null,
  claim: null,
  components: { mode: "inherit", ids: [], unknown: [], reason: null, inherited_from: null },
  closed_at: null,
  created_at: "2026-10-01T00:00:00Z",
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
  repo: null,
  resolved_repo: "acme/widgets",
  route: null,
  status: "todo",
  title: "Repository work",
  updated_at: "2026-10-01T00:00:00Z",
};
const summary: IssueSummary = {
  advances: [],
  advances_revision: 0,
  assignee: null,
  blocked_by: [],
  claim: null,
  components: issue.components,
  key: issue.key,
  labels: [],
  last_seq: 1,
  open_asks: 0,
  parent: null,
  primary_artifact_id: "artifact-1",
  priority: null,
  progress: { tasks: null, children: null },
  rank: "U",
  repo: null,
  resolved_repo: "acme/widgets",
  route: null,
  route_holder: null,
  route_status: null,
  route_status_reason: null,
  status: "todo",
  title: issue.title,
  updated_at: issue.updated_at,
  closed_at: issue.closed_at,
  created_by: issue.created_by,
  created_at: issue.created_at,
};

/** CORE's default repository is acme/widgets, and OTHER's is other/thing. */
const projects: Project[] = [
  {
    created_at: "2026-10-01T00:00:00Z",
    default_repo: "acme/widgets",
    gated_steps: [],
    key: "CORE",
    name: "Core",
    open_asks: 0,
    proven_sessions: false,
  },
  {
    created_at: "2026-10-01T00:00:00Z",
    default_repo: "other/thing",
    gated_steps: [],
    key: "OTHER",
    name: "Other",
    open_asks: 0,
    proven_sessions: false,
  },
];

/** The projects with CORE holding no default repository: a project with none carries no
 *  `default_repo`. */
const noCoreDefault: Project[] = projects.map(({ default_repo, ...project }) =>
  project.key === "CORE" ? project : { ...project, default_repo }
);

const detailKey = ["issue", issue.key] as const;
const listKey = ["issues", "project", issue.project] as const;

/** The control as the issue page draws it: with the repository its host reads from the cache,
 *  each commit of it recorded with that repository, held while `held`. */
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
    <commits.Recorded host={cached.data.repo}>
      <RepoControl
        held={held}
        issueKey={cached.data.key}
        project={cached.data.project}
        repo={cached.data.repo}
        resolvedRepo={cached.data.resolved_repo}
      />
    </commits.Recorded>
  );
}

function renderControl(
  own: { repo: string | null; resolvedRepo: string | null },
  listed: Project[] = projects,
  { failFirstRead = false, held = false }: { failFirstRead?: boolean; held?: boolean } = {}
) {
  const listProjects = spyOn(api, "listProjects").mockResolvedValue(listed);
  if (failFirstRead) {
    listProjects.mockRejectedValueOnce(new Error("the network dropped the read"));
  }
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  queryClient.setQueryData<IssueDetails>(detailKey, {
    ...issue,
    repo: own.repo,
    resolved_repo: own.resolvedRepo,
    route_status: null,
    route_holder: null,
    route_status_reason: null,
    holder_state: null,
    last_seen: null,
    takeable_at: null,
    artifacts: [],
    children: [],
    design_gates: [],
    open_asks: [],
    progress: { tasks: null, children: null },
    referenced_by_count: 0,
  });
  queryClient.setQueryData(listKey, [
    { ...summary, repo: own.repo, resolved_repo: own.resolvedRepo },
  ]);
  const user = userEvent.setup();
  // What the trigger's tooltip names - the issue's own repository, where it holds one - the
  // repository its badge names, and the words that read the badge.
  const commits = recordCommits<string | null>(() => {
    const drawn = trigger();
    const title = drawn.querySelector(".truncate")?.getAttribute("title");
    const spoken = drawn.querySelector(".sr-only")?.textContent;
    return `${title} | ${drawn.querySelector(badge)?.textContent} | ${spoken}`;
  });
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
    listProjects,
    queryClient,
    unmount: () => {
      view.unmount();
      listProjects.mockRestore();
    },
  };
}

/** The control's label: what its trigger's name starts with, before the words that read the
 *  badge, and its listbox's name with ` options` after it. */
const REPOSITORY = "Repository of CORE-1";

function trigger(): HTMLButtonElement {
  return screen.getByRole("button", { name: pickNamed(REPOSITORY) }) as HTMLButtonElement;
}

/** The visible badge; the trigger's name repeats its text from a span of its own. */
const badge = 'span[aria-hidden="true"]';

/** What the list says under its options about a typed value that is not `owner/name`. */
const NOT_OWNER_NAME = "Write the repository as owner/name.";

/** The open list's text box. */
function searchBox(): HTMLInputElement {
  return screen.getByRole("combobox", { name: "Search repositories" }) as HTMLInputElement;
}

/** Resolves once the project list has been read. */
async function projectsRead(queryClient: QueryClient): Promise<void> {
  await waitFor(() => expect(queryClient.getQueryState(["projects"])?.status).toBe("success"));
}

/** Types `repo` into the open list's text box, in place of what it held, and chooses the row
 *  that takes the typed text as the repository. */
async function chooseTyped(user: UserEvent, repo: string): Promise<void> {
  await user.clear(searchBox());
  await user.click(searchBox());
  await user.paste(repo);
  await user.click(screen.getByRole("option", { name: `Use "${repo}"` }));
}

/** Opens the list and gives it `repo` as typed text, through the row that takes it. */
async function giveTyped(user: UserEvent, repo: string): Promise<void> {
  await openList(user, REPOSITORY);
  await chooseTyped(user, repo);
}

// The suggestion comes from the project list, the read the sidebar and the issue header share:
// the control reads no route of its own.
test("RepoControl lists the project's default repository, shows the resolved one, and patches repo", async () => {
  // The one read is the project list, which `renderControl` answers at the api client, and the
  // write is answered by `answerIssuePatches`, so a request that reaches the network is one the
  // control should not make.
  const network = spyOn(globalThis, "fetch").mockRejectedValue(
    new Error("RepoControl sent a request")
  );
  const patchIssue = answerIssuePatches().mockResolvedValue({
    ...issue,
    repo: "acme/widgets",
    resolved_repo: "acme/widgets",
    last_seq: 2,
  });
  const { listProjects, queryClient, unmount, user } = renderControl({
    repo: null,
    resolvedRepo: "acme/widgets",
  });
  try {
    // The badge names the repository the issue resolves to; the project list is not read until
    // the reader reaches for the control.
    expect(screen.getByText("acme/widgets", { selector: badge })).toBeTruthy();
    expect(listProjects).not.toHaveBeenCalled();
    // A focus with no press before it: user-event has no form for a bare focus.
    fireEvent.focus(trigger());
    await waitFor(() => expect(listProjects).toHaveBeenCalled());
    // Inherit first, then CORE's default; OTHER's is absent.
    const list = await openList(user, REPOSITORY);
    await waitFor(() => expect(rows(list)).toEqual(["✓Inherit", "acme/widgets"]));
    expect(listProjects).toHaveBeenCalledTimes(1);
    await user.click(within(list).getByRole("option", { name: "acme/widgets" }));
    await waitFor(() =>
      expect(patchIssue).toHaveBeenCalledWith("CORE-1", { repo: "acme/widgets" }, keyedAttempt)
    );
    await waitFor(() =>
      expect(queryClient.getQueryData<IssueDetails>(detailKey)?.last_seq).toBe(2)
    );
    // The default, picked, is the issue's own repository: no longer inherited.
    expect(trigger()).toHaveAccessibleName(`${REPOSITORY} acme/widgets`);
    expect(screen.getByText("acme/widgets", { selector: badge })).toBeTruthy();
    expect(queryClient.getQueryData<IssueSummary[]>(listKey)?.[0]?.repo).toBe("acme/widgets");
    expect(network).not.toHaveBeenCalled();
  } finally {
    unmount();
    patchIssue.mockRestore();
    network.mockRestore();
  }
});

// Any `owner/name` is a repository the server takes, so the list is not held to the project's
// default: another project's default, or a repository no project names, is typed and given by the
// row that takes it, and what the reader typed is what is written.
test("RepoControl writes an owner/name that is not the project's default", async () => {
  const patchIssue = answerIssuePatches()
    .mockResolvedValueOnce({ ...issue, repo: "other/thing", resolved_repo: "other/thing" })
    .mockResolvedValueOnce({ ...issue, repo: "zed/elsewhere", resolved_repo: "zed/elsewhere" });
  const { queryClient, unmount, user } = renderControl({
    repo: null,
    resolvedRepo: "acme/widgets",
  });
  try {
    // other/thing is OTHER's default, so CORE's list offers it only as typed text.
    const list = await openList(user, REPOSITORY);
    await waitFor(() => expect(rows(list)).toEqual(["✓Inherit", "acme/widgets"]));
    await chooseTyped(user, "other/thing");
    await waitFor(() =>
      expect(patchIssue).toHaveBeenCalledWith("CORE-1", { repo: "other/thing" }, keyedAttempt)
    );
    await waitFor(() => expect(trigger()).toHaveAccessibleName(`${REPOSITORY} other/thing`));
    await giveTyped(user, "zed/elsewhere");
    await waitFor(() =>
      expect(patchIssue).toHaveBeenCalledWith("CORE-1", { repo: "zed/elsewhere" }, keyedAttempt)
    );
    await waitFor(() =>
      expect(queryClient.getQueryData<IssueDetails>(detailKey)?.repo).toBe("zed/elsewhere")
    );
    expect(screen.getByText("zed/elsewhere", { selector: badge })).toBeTruthy();
    // The issue's own repository is in its list, after the project's default.
    expect(rows(await openList(user, REPOSITORY))).toEqual([
      "Inherit",
      "acme/widgets",
      "✓zed/elsewhere",
    ]);
  } finally {
    unmount();
    patchIssue.mockRestore();
  }
});

// Inherit clears the issue's own repository, and the issue inherits again: the badge and the
// trigger's name say what it resolves to now.
test("RepoControl sends null for Inherit", async () => {
  const patchIssue = answerIssuePatches().mockResolvedValue({
    ...issue,
    repo: null,
    resolved_repo: "acme/widgets",
  });
  const { unmount, user } = renderControl({ repo: "acme/gadgets", resolvedRepo: "acme/gadgets" });
  try {
    // The issue's own repository is selectable before the project list has loaded.
    expect(trigger()).toHaveAccessibleName(`${REPOSITORY} acme/gadgets`);
    await pick(user, REPOSITORY, "Inherit");
    await waitFor(() =>
      expect(patchIssue).toHaveBeenCalledWith("CORE-1", { repo: null }, keyedAttempt)
    );
    await waitFor(() => expect(screen.getByText("acme/widgets", { selector: badge })).toBeTruthy());
    expect(trigger()).toHaveAccessibleName(
      `${REPOSITORY} acme/widgets, inherited or the project's default`
    );
  } finally {
    unmount();
    patchIssue.mockRestore();
  }
});

// A typed text is written only by its row or Enter, and an emptied box offers nothing to write,
// so a reader who types a repository, empties the box and picks Inherit sends the clear alone:
// one action is one write.
test("RepoControl sends the clear alone when a repository was typed first", async () => {
  const patchIssue = answerIssuePatches().mockResolvedValue({
    ...issue,
    repo: null,
    resolved_repo: "acme/widgets",
  });
  const { unmount, user } = renderControl({ repo: "acme/gadgets", resolvedRepo: "acme/gadgets" });
  try {
    const list = await openList(user, REPOSITORY);
    await user.click(searchBox());
    await user.paste("zed/other");
    expect(within(list).getByRole("option", { name: 'Use "zed/other"' })).toBeTruthy();
    await user.clear(searchBox());
    expect(within(list).queryByRole("option", { name: /^Use / })).not.toBeInTheDocument();
    await user.click(within(list).getByRole("option", { name: "Inherit" }));
    await waitFor(() =>
      expect(patchIssue).toHaveBeenCalledWith("CORE-1", { repo: null }, keyedAttempt)
    );
    await waitFor(() => expect(screen.getByText("acme/widgets", { selector: badge })).toBeTruthy());
    expect(patchIssue.mock.calls.map(([, patch]) => patch)).toEqual([{ repo: null }]);
  } finally {
    unmount();
    patchIssue.mockRestore();
  }
});

// A typo belongs in the list, not in a 400 a round trip later: the control reads `owner/name`
// itself, writes nothing, and keeps the list open with what was typed for the reader to correct.
test("RepoControl refuses a value that is not owner/name without writing it", async () => {
  const patchIssue = answerIssuePatches();
  const { unmount, user } = renderControl({ repo: null, resolvedRepo: "acme/widgets" });
  try {
    const list = await openList(user, REPOSITORY);
    const panel = list.parentElement as HTMLElement;
    await chooseTyped(user, "acme gadgets");
    expect(within(panel).getByText(NOT_OWNER_NAME)).toBeTruthy();
    expect(patchIssue).not.toHaveBeenCalled();
    expect(searchBox().value).toBe("acme gadgets");
    expect(searchBox()).toHaveFocus();
    // Changing the text takes the refusal off; corrected, it writes.
    await user.clear(searchBox());
    expect(within(panel).queryByText(NOT_OWNER_NAME)).not.toBeInTheDocument();
    patchIssue.mockResolvedValue({ ...issue, repo: "zed/gadgets", resolved_repo: "zed/gadgets" });
    await chooseTyped(user, "zed/gadgets");
    await waitFor(() =>
      expect(patchIssue).toHaveBeenCalledWith("CORE-1", { repo: "zed/gadgets" }, keyedAttempt)
    );
    expect(screen.queryByRole("listbox", { name: `${REPOSITORY} options` })).toBeNull();
    expect(screen.queryByText(NOT_OWNER_NAME)).not.toBeInTheDocument();
  } finally {
    unmount();
    patchIssue.mockRestore();
  }
});

// Today's Repository line wraps, so the refusal it draws beside the badge - the server's, with
// its Retry - moves whole onto the next line under the badge rather than squeezing it, and the
// badge narrows to the line (dispatch://INFRA-5200, c-review F3). A name that is no `owner/name`
// is refused in the open list, under its options. `e2e/issue.e2e.ts` measures the line at 360 px
// in an open Details.
test("RepoControl's line wraps, and the server's refusal is an item of its own beside a badge that narrows to the line", async () => {
  const patchIssue = answerIssuePatches().mockRejectedValue(
    new ApiError(400, { code: "INVALID_ISSUE", error: "repo refused by the server" })
  );
  const { unmount, user } = renderControl({ repo: null, resolvedRepo: "acme/widgets" });
  try {
    await giveTyped(user, "acme/gadgets");
    const refusal = await screen.findByText("repo refused by the server");
    const place = trigger().parentElement;
    const line = place?.parentElement;
    if (place == null || line == null) throw new Error("expected the trigger in a line");
    expect(line.contains(refusal)).toBe(true);
    expect(line.classList.contains("flex-wrap")).toBe(true);
    expect(line.classList.contains("w-full")).toBe(true);
    expect(place.contains(refusal)).toBe(false);
    expect(place.classList.contains("max-w-full")).toBe(true);
    expect(place.classList.contains("min-w-0")).toBe(true);
  } finally {
    unmount();
    patchIssue.mockRestore();
  }
});

// The list holds what is typed to GitHub's own grammar, which is what the server holds it to:
// a name GitHub could hold no repository at reaches the list's refusal rather than a round trip,
// and a name GitHub does hold is written as typed, whatever its case, its leading dot or its
// `.git`.
test("RepoControl refuses a name GitHub could hold no repository at", async () => {
  const patchIssue = answerIssuePatches();
  const { unmount, user } = renderControl({ repo: null, resolvedRepo: "acme/widgets" });
  try {
    const list = await openList(user, REPOSITORY);
    const panel = list.parentElement as HTMLElement;
    for (const typed of ["x/..", "x/.", "-bad/x", "own%er/x", `owner/${"a".repeat(101)}`]) {
      await chooseTyped(user, typed);
      expect(within(panel).getByText(NOT_OWNER_NAME)).toBeTruthy();
      expect(searchBox().value).toBe(typed);
    }
    expect(patchIssue).not.toHaveBeenCalled();
    patchIssue.mockResolvedValue({
      ...issue,
      repo: "sjawhar/.knives-ledger",
      resolved_repo: "sjawhar/.knives-ledger",
    });
    await chooseTyped(user, "Sjawhar/.knives-ledger.git");
    await waitFor(() =>
      expect(patchIssue).toHaveBeenCalledWith(
        "CORE-1",
        { repo: "Sjawhar/.knives-ledger.git" },
        keyedAttempt
      )
    );
    expect(screen.queryByText(NOT_OWNER_NAME)).not.toBeInTheDocument();
    // An owner GitHub serves that its sign-up form would refuse a new account - a doubled
    // hyphen, a hyphen at the end, a managed user's `_<shortcode>` - names a repository the
    // server takes, so the list takes it too.
    for (const typed of ["mona-cat_octo/hello--world", "artofcode-/x"]) {
      patchIssue.mockResolvedValue({ ...issue, repo: typed, resolved_repo: typed });
      await giveTyped(user, typed);
      await waitFor(() =>
        expect(patchIssue).toHaveBeenCalledWith("CORE-1", { repo: typed }, keyedAttempt)
      );
    }
  } finally {
    unmount();
    patchIssue.mockRestore();
  }
});

// Leaving a list picks nothing, in this list as in every other: a typed repository is given by
// its row or Enter, so Escape or Tab with one typed writes nothing and keeps what the issue holds.
test("RepoControl writes nothing when the list is left by Escape or Tab with a repository typed", async () => {
  const patchIssue = answerIssuePatches();
  const { unmount, user } = renderControl({ repo: "acme/gadgets", resolvedRepo: "acme/gadgets" });
  try {
    for (const leave of [() => user.keyboard("{Escape}"), () => user.tab()]) {
      await openList(user, REPOSITORY);
      await user.click(searchBox());
      await user.paste("acme/gadgets-typo");
      await leave();
      expect(screen.queryByRole("listbox", { name: `${REPOSITORY} options` })).toBeNull();
      expect(trigger()).toHaveFocus();
      expect(trigger()).toHaveAccessibleName(`${REPOSITORY} acme/gadgets`);
    }
    // Opened again, the box starts empty: what was typed went with the list that held it.
    await openList(user, REPOSITORY);
    expect(searchBox().value).toBe("");
    await act(async () => {});
    expect(patchIssue).not.toHaveBeenCalled();
  } finally {
    unmount();
    patchIssue.mockRestore();
  }
});

// dispatch://INFRA-2160, R6: a key on the closed trigger only moves the list's highlight - a
// native select would write the next repository on ArrowDown - and Escape, Tab and a press
// outside write nothing and leave the badge on the issue's repository. Enter writes the row
// highlighted.
test("RepoControl's keys on its closed trigger only move the list's highlight, and only Enter writes", async () => {
  const patchIssue = answerIssuePatches().mockResolvedValue({
    ...issue,
    repo: "acme/widgets",
    resolved_repo: "acme/widgets",
  });
  const { queryClient, unmount, user } = renderControl({
    repo: "acme/gadgets",
    resolvedRepo: "acme/gadgets",
  });
  try {
    act(() => trigger().focus());
    await projectsRead(queryClient);
    // The rows: Inherit, CORE's default acme/widgets, and the issue's own acme/gadgets.
    for (const [keys, to] of [
      ["{ArrowDown}", "Inherit"],
      ["{ArrowUp}", "acme/widgets"],
      ["{End}", "acme/gadgets"],
      ["{Home}", "Inherit"],
      ["a", "acme/widgets"],
    ] as const) {
      await user.keyboard(keys);
      expect(highlightedRow(searchBox()), `${keys} highlights ${to}`).toBe(to);
      await user.keyboard("{Escape}");
      expect(screen.queryByRole("listbox", { name: `${REPOSITORY} options` })).toBeNull();
      expect(trigger()).toHaveFocus();
      expect(trigger()).toHaveAccessibleName(`${REPOSITORY} acme/gadgets`);
    }
    await user.keyboard("{ArrowDown}");
    await user.tab();
    expect(screen.queryByRole("listbox", { name: `${REPOSITORY} options` })).toBeNull();
    expect(trigger()).toHaveFocus();
    await user.keyboard("{ArrowDown}");
    await user.click(document.body);
    expect(screen.queryByRole("listbox", { name: `${REPOSITORY} options` })).toBeNull();
    expect(trigger()).toHaveAccessibleName(`${REPOSITORY} acme/gadgets`);
    await act(async () => {});
    expect(patchIssue).not.toHaveBeenCalled();

    act(() => trigger().focus());
    await user.keyboard("{ArrowUp}{Enter}");
    await waitFor(() =>
      expect(patchIssue).toHaveBeenCalledWith("CORE-1", { repo: "acme/widgets" }, keyedAttempt)
    );
    expect(patchIssue).toHaveBeenCalledTimes(1);
  } finally {
    unmount();
    patchIssue.mockRestore();
  }
});

// `held` is the issue page's other field writes: the trigger keeps the focus and its Tab stop and
// says it is busy, and neither a press nor a key on it opens the list while the hold lasts, so the
// reader never picks or types a repository the hold would refuse with no word. A list opened
// before the hold began writes nothing while it lasts: a pick is dropped, and a typed repository
// stays in the list's text box for the next Enter.
test("a held RepoControl keeps its focus and Tab stop, opens no list, and writes nothing from a list opened before the hold", async () => {
  const patchIssue = answerIssuePatches();
  const { hold, queryClient, unmount, user } = renderControl(
    { repo: "acme/gadgets", resolvedRepo: "acme/gadgets" },
    projects,
    { held: true }
  );
  try {
    act(() => trigger().focus());
    await projectsRead(queryClient);
    expect(trigger()).toHaveAttribute("aria-disabled", "true");
    expect(trigger()).not.toBeDisabled();
    await user.click(trigger());
    expect(screen.queryByRole("listbox"), "a press opened the list").toBeNull();
    for (const keys of [
      "{ArrowDown}",
      "{ArrowUp}",
      "{Home}",
      "{End}",
      "{PageDown}",
      "{PageUp}",
      "z",
      "{Enter}",
      " ",
    ]) {
      await user.keyboard(keys);
      expect(screen.queryByRole("listbox"), `${keys} opened the list`).toBeNull();
      expect(trigger()).toHaveFocus();
    }
    expect(trigger()).toHaveAccessibleName(`${REPOSITORY} acme/gadgets`);

    hold(false);
    const list = await openList(user, REPOSITORY);
    hold(true);
    await user.click(within(list).getByRole("option", { name: "acme/widgets" }));
    expect(trigger()).toHaveFocus();
    expect(trigger()).toHaveAccessibleName(`${REPOSITORY} acme/gadgets`);
    hold(false);
    await openList(user, REPOSITORY);
    hold(true);
    await user.click(searchBox());
    await user.paste("zed/other");
    await user.keyboard("{Enter}");
    expect(searchBox().value).toBe("zed/other");
    await act(async () => {});
    expect(patchIssue).not.toHaveBeenCalled();
  } finally {
    unmount();
    patchIssue.mockRestore();
  }
});

// A typed text the issue already holds names an option, so the list offers no row to use it,
// and Enter takes that option, which writes nothing.
test("RepoControl writes nothing for a typed repository the issue already holds", async () => {
  const patchIssue = answerIssuePatches();
  const { unmount, user } = renderControl({ repo: "zed/own", resolvedRepo: "zed/own" });
  try {
    const list = await openList(user, REPOSITORY);
    await user.click(searchBox());
    await user.paste("zed/own");
    expect(rows(list)).toEqual(["✓zed/own"]);
    await user.keyboard("{Enter}");
    expect(screen.queryByRole("listbox", { name: `${REPOSITORY} options` })).toBeNull();
    await act(async () => {});
    expect(patchIssue).not.toHaveBeenCalled();
  } finally {
    unmount();
    patchIssue.mockRestore();
  }
});

// The badge is decoration for sighted readers (aria-hidden); the trigger's name goes on to the
// repository the issue resolves to, so a screen reader hears the target as well as Inherit.
test("RepoControl's name goes on to the repository the issue resolves to", async () => {
  for (const [own, want] of [
    [
      { repo: null, resolvedRepo: "acme/widgets" },
      "acme/widgets, inherited or the project's default",
    ],
    [{ repo: "acme/gadgets", resolvedRepo: "acme/gadgets" }, "acme/gadgets"],
    [{ repo: null, resolvedRepo: null }, "No repository"],
  ] as const) {
    const { unmount } = renderControl({ ...own });
    try {
      expect(
        await screen.findByRole("button", { name: pickNamed("Repository of CORE-1") })
      ).toHaveAccessibleName(`Repository of CORE-1 ${want}`);
    } finally {
      unmount();
    }
  }
});

// With nothing resolved the badge says there is no repository, and the project list is read at
// once: the issue with no repository is the one most likely to be given one.
test("RepoControl says No repository when nothing resolves", async () => {
  const { listProjects, unmount } = renderControl(
    { repo: null, resolvedRepo: null },
    noCoreDefault
  );
  try {
    expect(await screen.findByText("No repository", { selector: badge })).toBeTruthy();
    // The control is the value of the Repository row of the header's Details, which names it.
    expect(screen.queryByText("Repository:")).toBeNull();
    expect(trigger()).toHaveAccessibleName(`${REPOSITORY} No repository`);
    await waitFor(() => expect(listProjects).toHaveBeenCalledTimes(1));
  } finally {
    unmount();
  }
});

// A project with no default repository has nothing to suggest, and its issues take a repository
// all the same, so Details carries the item, and its list offers Inherit and a typed repository.
test("RepoControl shows the item for an issue whose project has no default repository", async () => {
  const patchIssue = answerIssuePatches().mockResolvedValue({
    ...issue,
    repo: "zed/elsewhere",
    resolved_repo: "zed/elsewhere",
  });
  const { queryClient, unmount, user } = renderControl(
    { repo: null, resolvedRepo: null },
    noCoreDefault
  );
  try {
    // The project list is read at once; once it is in and React has rendered it, the list holds
    // no default.
    await waitFor(() =>
      expect(queryClient.getQueryData<Project[]>(["projects"])).toEqual(noCoreDefault)
    );
    await act(async () => {});
    // The control is the value of the Repository row of the header's Details, which names it.
    expect(screen.queryByText("Repository:")).toBeNull();
    expect(screen.getByText("No repository", { selector: badge })).toBeTruthy();
    expect(rows(await openList(user, REPOSITORY))).toEqual(["✓Inherit"]);
    await chooseTyped(user, "zed/elsewhere");
    await waitFor(() =>
      expect(patchIssue).toHaveBeenCalledWith("CORE-1", { repo: "zed/elsewhere" }, keyedAttempt)
    );
  } finally {
    unmount();
    patchIssue.mockRestore();
  }
});

// A failed read of the project list costs the reader the suggestion and nothing else: the item
// shows, with the failure and a Retry beside its trigger, and the read is offered again.
test("RepoControl shows a failed project list read with a retry instead of hiding the item", async () => {
  const { listProjects, unmount, user } = renderControl(
    { repo: null, resolvedRepo: null },
    projects,
    { failFirstRead: true }
  );
  try {
    expect(await screen.findByText("Could not load projects.")).toBeTruthy();
    expect(screen.getByText("No repository", { selector: badge })).toBeTruthy();
    await user.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(listProjects).toHaveBeenCalledTimes(2));
    await waitFor(() =>
      expect(screen.queryByText("Could not load projects.")).not.toBeInTheDocument()
    );
    expect(rows(await openList(user, REPOSITORY))).toEqual(["✓Inherit", "acme/widgets"]);
    expect(screen.getByText("No repository", { selector: badge })).toBeTruthy();
  } finally {
    unmount();
  }
});

// With a repository resolved the picker still shows it, and a failed read says so beside it.
test("RepoControl shows a failed project list read beside a resolved repository", async () => {
  const { unmount } = renderControl({ repo: null, resolvedRepo: "acme/widgets" }, projects, {
    failFirstRead: true,
  });
  try {
    // A focus with no press before it: user-event has no form for a bare focus.
    fireEvent.focus(trigger());
    expect(await screen.findByText("Could not load projects.")).toBeTruthy();
    expect(screen.getByText("acme/widgets", { selector: badge })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Retry" })).toBeTruthy();
  } finally {
    unmount();
  }
});

// What the list lets through, the server decides on: its refusal shows beside the trigger, and
// the optimistic write is rolled back.
test("RepoControl rolls back and shows the server's refusal", async () => {
  const patchIssue = answerIssuePatches().mockRejectedValue(
    new ApiError(400, {
      code: "INVALID_ISSUE",
      error: 'repo must be "owner/name" (got "zed/gadgets.git/extra")',
    })
  );
  const { unmount, user } = renderControl({ repo: null, resolvedRepo: "acme/widgets" });
  try {
    await giveTyped(user, "zed/gadgets");
    expect(
      await screen.findByText('repo must be "owner/name" (got "zed/gadgets.git/extra")')
    ).toBeTruthy();
    expect(trigger()).toHaveAccessibleName(
      `${REPOSITORY} acme/widgets, inherited or the project's default`
    );
    expect(screen.getByText("acme/widgets", { selector: badge })).toBeTruthy();
  } finally {
    unmount();
    patchIssue.mockRestore();
  }
});

// INFRA-3135, R3: "Every list of options a reader picks from holds a text box that narrows the
// options." The issue header's Repository is such a list: its options are Inherit, the project's
// default repository and the issue's own. Opened, it holds a text box of its own, a word typed
// into it leaves only the repositories that match, and a pick writes the issue's own repository
// and puts the focus back on the control. Opened again it stands on the value it holds, and
// Enter there writes nothing.
test("the issue header's Repository list holds a text box that narrows the repositories, and a pick writes the issue's own", async () => {
  const patchIssue = answerIssuePatches().mockResolvedValue({
    ...issue,
    repo: "acme/widgets",
    resolved_repo: "acme/widgets",
  });
  const { unmount, user } = renderControl({ repo: "acme/gadgets", resolvedRepo: "acme/gadgets" });
  try {
    const before = new Set(textBoxes());
    await user.click(screen.getByRole("button", { name: pickNamed("Repository of CORE-1") }));
    // The project list is read once the reader reaches for the control.
    await waitFor(() =>
      expect(screen.queryAllByRole("option", { name: "acme/widgets" }).length).toBeGreaterThan(0)
    );
    const own = textBoxes().filter((box) => !before.has(box));
    expect(own, "the Repository list holds no text box of its own").toHaveLength(1);
    const list = listOf(own[0] as HTMLElement);
    expect(within(list).getByRole("option", { name: "acme/gadgets" })).toHaveAttribute(
      "aria-selected",
      "true"
    );
    await user.type(own[0] as HTMLElement, "widg");
    await waitFor(() =>
      expect(
        within(list).queryAllByRole("option", { name: "acme/gadgets" }),
        `"widg" leaves acme/gadgets in the Repository list`
      ).toHaveLength(0)
    );
    expect(
      within(list).queryAllByRole("option", { name: "Inherit" }),
      `"widg" leaves Inherit in the Repository list`
    ).toHaveLength(0);
    await user.click(within(list).getByRole("option", { name: "acme/widgets" }));
    await waitFor(() =>
      expect(patchIssue).toHaveBeenCalledWith("CORE-1", { repo: "acme/widgets" }, keyedAttempt)
    );
    expect(screen.queryByRole("listbox", { name: "Repository of CORE-1 options" })).toBeNull();
    expect(trigger()).toHaveFocus();
    await waitFor(() => expect(trigger()).toHaveAccessibleName(`${REPOSITORY} acme/widgets`));

    await user.keyboard("{Enter}");
    await screen.findByRole("listbox", { name: "Repository of CORE-1 options" });
    await user.keyboard("{Enter}");
    expect(screen.queryByRole("listbox", { name: "Repository of CORE-1 options" })).toBeNull();
    await act(async () => {});
    expect(patchIssue).toHaveBeenCalledTimes(1);
  } finally {
    unmount();
    patchIssue.mockRestore();
  }
});

// Until the project list answers, the list says the repositories are loading where a typed word
// matches no option, and it takes the typed repository all the same.
test("the issue header's Repository list says the repositories are loading where a typed word matches none", async () => {
  const patchIssue = answerIssuePatches().mockResolvedValue({
    ...issue,
    repo: "zed/elsewhere",
    resolved_repo: "zed/elsewhere",
  });
  const { listProjects, unmount, user } = renderControl({
    repo: null,
    resolvedRepo: "acme/widgets",
  });
  listProjects.mockReturnValue(new Promise(() => {}));
  try {
    const list = await openList(user, REPOSITORY);
    await user.click(searchBox());
    await user.paste("zed/elsewhere");
    expect(within(list).getByText("Loading repositories…")).toBeTruthy();
    await user.click(within(list).getByRole("option", { name: 'Use "zed/elsewhere"' }));
    await waitFor(() =>
      expect(patchIssue).toHaveBeenCalledWith("CORE-1", { repo: "zed/elsewhere" }, keyedAttempt)
    );
  } finally {
    unmount();
    patchIssue.mockRestore();
  }
});

// INFRA-3135, round 12: a list whose read failed says so. Beside a resolved repository the line
// shows the failure with a Retry, and the list the reader opens says it too, over the Inherit it
// still offers. It still takes a typed repository, and the refusal of a typed text stands in the
// failure's place until the text changes.
test("the issue header's Repository list says the repositories could not be loaded when their read fails", async () => {
  const patchIssue = answerIssuePatches().mockResolvedValue({
    ...issue,
    repo: "zed/elsewhere",
    resolved_repo: "zed/elsewhere",
  });
  const { unmount, user } = renderControl({ repo: null, resolvedRepo: "acme/widgets" }, projects, {
    failFirstRead: true,
  });
  try {
    const failure = "Could not load projects.";
    const list = await openList(user, REPOSITORY);
    const panel = list.parentElement as HTMLElement;
    expect(await within(panel).findByText(failure)).toBeTruthy();
    expect(rows(list)).toEqual(["✓Inherit"]);
    await chooseTyped(user, "zed elsewhere");
    expect(within(panel).getByText(NOT_OWNER_NAME)).toBeTruthy();
    expect(within(panel).queryByText(failure)).not.toBeInTheDocument();
    await user.clear(searchBox());
    expect(within(panel).getByText(failure)).toBeTruthy();
    await chooseTyped(user, "zed/elsewhere");
    await waitFor(() =>
      expect(patchIssue).toHaveBeenCalledWith("CORE-1", { repo: "zed/elsewhere" }, keyedAttempt)
    );
  } finally {
    unmount();
    patchIssue.mockRestore();
  }
});

// INFRA-3135, round 13, the architect's ruling of 2026-11-18T10:54Z: a trigger that replaces a
// visible control shows the shared chevron, the sign a sighted reader reads that it opens a list.
// At the merge base (main's #3730) the Repository is a visible text box, so its trigger ends in
// the chevron every other single pick draws (`ChevronIcon`, `components/DisclosureToggle.tsx`),
// whether the issue holds a repository of its own or inherits one.
test("the issue header's Repository trigger ends in the shared chevron, as every list replacing a visible control does", async () => {
  for (const own of [
    { repo: "acme/gadgets", resolvedRepo: "acme/gadgets" },
    { repo: null, resolvedRepo: "acme/widgets" },
  ]) {
    const { unmount } = renderControl(own);
    try {
      const chevron = trigger().querySelector("svg");
      expect(chevron, `${own.repo ?? "inherited"}: the trigger draws no chevron`).not.toBeNull();
      expect(chevron?.querySelector("path")?.getAttribute("d")).toBe("M4 6l4 4 4-4");
      expect(chevron).toHaveAttribute("aria-hidden", "true");
    } finally {
      unmount();
    }
  }
});

// The badge's tooltip says what the badge means rather than the picked row's text, which is
// Inherit for every issue with no repository of its own: an inherited repository is inherited or
// the project's default, an issue's own repository is itself, and "No repository" says all there
// is, so it shows none.
test("the issue header's Repository badge's tooltip says what the badge means, never the Inherit row's text", async () => {
  for (const [own, want] of [
    [{ repo: null, resolvedRepo: "acme/widgets" }, "Inherited or the project's default"],
    [{ repo: "acme/gadgets", resolvedRepo: "acme/gadgets" }, "acme/gadgets"],
    [{ repo: null, resolvedRepo: null }, ""],
  ] as const) {
    const { unmount } = renderControl({ ...own });
    try {
      const shown = await screen.findByRole("button", { name: pickNamed(REPOSITORY) });
      expect(
        shown.querySelector(".truncate"),
        `${own.repo ?? (own.resolvedRepo === null ? "no repository" : "inherited")}: the trigger's text`
      ).toHaveAttribute("title", want);
    } finally {
      unmount();
    }
  }
});

// Enter picks the repository typed into the list's text box, and from that press on the control
// draws it in every commit, with the repository the issue then resolves to: the trigger's
// tooltip, its badge and the words that read the badge.
// The pick reaches the issue's cache in a microtask of the press, and the control's host only
// when TanStack Query tells the cache's observers, in a browser a timer later, which the row
// holds back: a commit before that timer that drew the repository the pick replaces is a frame
// of it on screen (INFRA-3904).
test("RepoControl draws the pick, and the repository it resolves to, in every commit from Enter on, while its host still passes the inherited repository it replaces", async () => {
  const save = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches().mockImplementation(() => save.promise);
  const { commits, queryClient, unmount, user } = renderControl({
    repo: null,
    resolvedRepo: "acme/widgets",
  });
  let notifications: HeldNotifications | undefined;
  try {
    await openList(user, REPOSITORY);
    await user.click(searchBox());
    await user.paste("acme/gadgets");
    const fromPress = commits.markAt(searchBox(), "keydown");
    notifications = holdNotifications();
    await user.keyboard("{Enter}");
    await waitFor(() =>
      expect(patchIssue).toHaveBeenCalledWith("CORE-1", { repo: "acme/gadgets" }, keyedAttempt)
    );
    // The cache draws the pick, and its host has not been told.
    expect(queryClient.getQueryData<IssueDetails>(detailKey)?.repo).toBe("acme/gadgets");
    expect(new Set(fromPress().map(({ host }) => host))).toEqual(new Set([null]));
    notifications.tell();
    expect(fromPress().at(-1)?.host).toBe("acme/gadgets");
    await act(async () => {
      save.resolve({ ...issue, repo: "acme/gadgets", resolved_repo: "acme/gadgets", last_seq: 2 });
      await save.promise;
    });
    await waitFor(() =>
      expect(queryClient.getQueryData<IssueDetails>(detailKey)?.last_seq).toBe(2)
    );
    expect(fromPress().map(({ drawn }) => drawn)).toEqual(
      fromPress().map(() => "acme/gadgets | acme/gadgets | acme/gadgets")
    );
  } finally {
    notifications?.tell();
    unmount();
    patchIssue.mockRestore();
  }
});

// A pick can land before TanStack Query has told the control's host of it: a background tab holds
// that timer back while a request's answer still lands. Dispatch drops a `.git` suffix before it
// stores a repository (canonicalRepo), so a pick of `acme/widgets.git` on an issue that holds
// `acme/widgets` lands as the repository the host passed at the press, and passes still. The
// control draws what Dispatch stored once the host is told, and keeps no pick Dispatch did not
// store.
test("RepoControl draws what Dispatch stored once its host is told, when the pick landed first as the repository its host passed at the press", async () => {
  const patchIssue = answerIssuePatches().mockResolvedValue({
    ...issue,
    repo: "acme/widgets",
    resolved_repo: "acme/widgets",
    last_seq: 2,
  });
  const { queryClient, unmount, user } = renderControl({
    repo: "acme/widgets",
    resolvedRepo: "acme/widgets",
  });
  let notifications: HeldNotifications | undefined;
  try {
    await openList(user, REPOSITORY);
    await user.clear(searchBox());
    await user.click(searchBox());
    await user.paste("acme/widgets.git");
    notifications = holdNotifications();
    await user.keyboard("{Enter}");
    await waitFor(() =>
      expect(queryClient.getQueryData<IssueDetails>(detailKey)?.last_seq).toBe(2)
    );
    expect(patchIssue).toHaveBeenCalledWith("CORE-1", { repo: "acme/widgets.git" }, keyedAttempt);
    // The pick landed, and its host has been told of nothing.
    expect(notifications.held()).toBeGreaterThan(0);
    notifications.tell();
    expect(trigger()).toHaveAccessibleName(`${REPOSITORY} acme/widgets`);
  } finally {
    notifications?.tell();
    unmount();
    patchIssue.mockRestore();
  }
});
