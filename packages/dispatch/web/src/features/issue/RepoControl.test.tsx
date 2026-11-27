import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";
import type { ReactNode } from "react";
import {
  type Commits,
  type HeldNotifications,
  holdNotifications,
  recordCommits,
} from "../../__tests__/commits";
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { answerIssuePatches } from "../../__tests__/send-answers";
import { ApiError, api } from "../../api/client";
import { whoAmIQuery } from "../../api/queries";
import type { Issue, IssueDetails, IssueSummary, Project } from "../../api/types";
import { NoTooltips } from "../issue-page/NoTooltips";
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
 *  each commit of it recorded with that repository. */
function CachedControl({ commits }: { commits: Commits<string | null> }): ReactNode {
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
  listed: Project[] = projects
) {
  const listProjects = spyOn(api, "listProjects").mockResolvedValue(listed);
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  // A full member, read as the shell already read them: the control asks whether its reader is a
  // restricted member (`useRestrictedMember`), and a read of the network here would be one the
  // control should not make.
  queryClient.setQueryDefaults(whoAmIQuery().queryKey, { staleTime: Number.POSITIVE_INFINITY });
  queryClient.setQueryData(whoAmIQuery().queryKey, {
    event_head: "0",
    kind: "user",
    login: "alice",
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
  // What the box holds, the repository its placeholder names, and its description.
  const commits = recordCommits<string | null>(() => {
    const drawn = box();
    const description = document.getElementById(drawn.getAttribute("aria-describedby") ?? "");
    return `${drawn.value} | ${drawn.placeholder} | ${description?.textContent}`;
  });
  const view = render(
    <QueryClientProvider client={queryClient}>
      <CachedControl commits={commits} />
    </QueryClientProvider>
  );
  return {
    commits,
    user,
    listProjects,
    queryClient,
    unmount: () => {
      view.unmount();
      listProjects.mockRestore();
    },
  };
}

function box(): HTMLInputElement {
  return screen.getByLabelText("Repository of CORE-1") as HTMLInputElement;
}

/** What the box suggests: its `<datalist>`, which no role query reaches. */
function suggestions(): string[] {
  const list = document.getElementById(box().getAttribute("list") ?? "");
  return Array.from(list?.querySelectorAll("option") ?? []).map((option) => option.value);
}

/** Puts `repo` in the box and leaves it, which is what writes it; `""` empties the box. */
async function write(user: UserEvent, repo: string): Promise<void> {
  await user.clear(box());
  if (repo !== "") {
    await user.click(box());
    await user.paste(repo);
  }
  await user.tab();
}

// The suggestion comes from the project list, the read the sidebar and the issue header share:
// the control reads no route of its own.
test("RepoControl suggests the project's default repository, shows the resolved one, and patches repo", async () => {
  // The one read is the project list, which `renderControl` answers at the api client, and the
  // write is answered by `answerIssuePatches`, so a request that reaches the network is one the
  // control should not make.
  const network = spyOn(globalThis, "fetch").mockRejectedValue(
    new Error("RepoControl sent a request")
  );
  const patchIssue = answerIssuePatches().mockResolvedValue({
    ...issue,
    repo: "acme/gadgets",
    resolved_repo: "acme/gadgets",
    last_seq: 2,
  });
  const { listProjects, queryClient, unmount, user } = renderControl({
    repo: null,
    resolvedRepo: "acme/widgets",
  });
  try {
    // The box holds the issue's own repository, which it has none of, so the one it resolves to
    // is its placeholder.
    expect(box().value).toBe("");
    expect(box().placeholder).toBe("acme/widgets");
    // CORE's default alone; OTHER's is absent.
    await waitFor(() => expect(suggestions()).toEqual(["acme/widgets"]));
    expect(listProjects).toHaveBeenCalledTimes(1);
    await write(user, "acme/gadgets");
    await waitFor(() =>
      expect(patchIssue).toHaveBeenCalledWith("CORE-1", { repo: "acme/gadgets" }, keyedAttempt)
    );
    await waitFor(() =>
      expect(queryClient.getQueryData<IssueDetails>(detailKey)?.last_seq).toBe(2)
    );
    expect(box().value).toBe("acme/gadgets");
    expect(queryClient.getQueryData<IssueSummary[]>(listKey)?.[0]?.repo).toBe("acme/gadgets");
    expect(network).not.toHaveBeenCalled();
  } finally {
    unmount();
    patchIssue.mockRestore();
    network.mockRestore();
  }
});

// Any `owner/name` is a repository the server takes, so the box is not held to the project's
// default: what the reader types is what is written.
test("RepoControl writes an owner/name that is not the project's default", async () => {
  const patchIssue = answerIssuePatches().mockResolvedValue({
    ...issue,
    repo: "zed/elsewhere",
    resolved_repo: "zed/elsewhere",
  });
  const { queryClient, unmount, user } = renderControl({
    repo: null,
    resolvedRepo: "acme/widgets",
  });
  try {
    await write(user, "zed/elsewhere");
    await waitFor(() =>
      expect(patchIssue).toHaveBeenCalledWith("CORE-1", { repo: "zed/elsewhere" }, keyedAttempt)
    );
    await waitFor(() =>
      expect(queryClient.getQueryData<IssueDetails>(detailKey)?.repo).toBe("zed/elsewhere")
    );
    expect(box().value).toBe("zed/elsewhere");
  } finally {
    unmount();
    patchIssue.mockRestore();
  }
});

test("RepoControl sends null for the clear control", async () => {
  const patchIssue = answerIssuePatches().mockResolvedValue({
    ...issue,
    repo: null,
    resolved_repo: "acme/widgets",
  });
  const { unmount, user } = renderControl({ repo: "acme/gadgets", resolvedRepo: "acme/gadgets" });
  try {
    // The issue's own repository is in the box before the project list has loaded.
    expect(box().value).toBe("acme/gadgets");
    await user.click(screen.getByRole("button", { name: "Clear the repository of CORE-1" }));
    await waitFor(() =>
      expect(patchIssue).toHaveBeenCalledWith("CORE-1", { repo: null }, keyedAttempt)
    );
    // Cleared, the issue inherits again: the box is empty, its placeholder names what it
    // resolves to, and there is nothing left to clear.
    await waitFor(() => expect(box().placeholder).toBe("acme/widgets"));
    expect(box().value).toBe("");
    expect(
      screen.queryByRole("button", { name: "Clear the repository of CORE-1" })
    ).not.toBeInTheDocument();
  } finally {
    unmount();
    patchIssue.mockRestore();
  }
});

// An emptied box is the same clearing: there is no repository in a blank box to write.
test("RepoControl sends null for an emptied box", async () => {
  const patchIssue = answerIssuePatches().mockResolvedValue({
    ...issue,
    repo: null,
    resolved_repo: "acme/widgets",
  });
  const { unmount, user } = renderControl({ repo: "acme/gadgets", resolvedRepo: "acme/gadgets" });
  try {
    await write(user, "");
    await waitFor(() =>
      expect(patchIssue).toHaveBeenCalledWith("CORE-1", { repo: null }, keyedAttempt)
    );
  } finally {
    unmount();
    patchIssue.mockRestore();
  }
});

// The box writes what was typed when it is left, so a press on the clear control must not take
// focus from it: the press would write the typed repository, which the reader is throwing away,
// and the click after it would write the clear. One action is one write.
test("RepoControl sends the clear alone when a repository was typed first", async () => {
  const patchIssue = answerIssuePatches().mockResolvedValue({
    ...issue,
    repo: null,
    resolved_repo: "acme/widgets",
  });
  const { unmount, user } = renderControl({ repo: "acme/gadgets", resolvedRepo: "acme/gadgets" });
  try {
    await user.clear(box());
    await user.click(box());
    await user.paste("zed/other");
    expect(box().value).toBe("zed/other");
    await user.click(screen.getByRole("button", { name: "Clear the repository of CORE-1" }));
    await waitFor(() =>
      expect(patchIssue).toHaveBeenCalledWith("CORE-1", { repo: null }, keyedAttempt)
    );
    // Cleared, the box is empty and inherits, with what was typed gone from it.
    await waitFor(() => expect(box().placeholder).toBe("acme/widgets"));
    expect(box().value).toBe("");
    expect(patchIssue.mock.calls.map(([, patch]) => patch)).toEqual([{ repo: null }]);
  } finally {
    unmount();
    patchIssue.mockRestore();
  }
});

// A browser compiles the box's `pattern` as `^(?:…)$` with the `v` flag, and under that flag a
// bare hyphen in a character class is a syntax error, which makes the browser ignore the
// attribute and log the error. So the attribute is compiled here as a browser compiles it, and
// held to the names the box's own check takes and refuses.
test("RepoControl's pattern compiles as a browser compiles it", () => {
  const { unmount } = renderControl({ repo: null, resolvedRepo: "acme/widgets" });
  try {
    const compiled = new RegExp(`^(?:${box().getAttribute("pattern") ?? ""})$`, "v");
    // Names GitHub holds, which the pattern must take, and names it could hold no repository at,
    // which the pattern must refuse: each list keeps the names the pattern got wrong.
    const held = [
      "acme/widgets",
      "Sjawhar/.knives-ledger",
      "mona-cat_octo/hello--world",
      "artofcode-/x",
    ];
    const notHeld = [
      "acme gadgets",
      "x/..",
      "x/.",
      "-bad/x",
      "own%er/x",
      `owner/${"a".repeat(101)}`,
    ];
    expect(held.filter((name) => !compiled.test(name))).toEqual([]);
    expect(notHeld.filter((name) => compiled.test(name))).toEqual([]);
  } finally {
    unmount();
  }
});

// A typo belongs at the box, not in a 400 a round trip later: the control reads `owner/name`
// itself, writes nothing, and keeps what was typed for the reader to correct.
test("RepoControl refuses a value that is not owner/name without writing it", async () => {
  const patchIssue = answerIssuePatches();
  const { unmount, user } = renderControl({ repo: null, resolvedRepo: "acme/widgets" });
  try {
    await write(user, "acme gadgets");
    expect(await screen.findByRole("alert")).toHaveTextContent(
      "Write the repository as owner/name."
    );
    expect(patchIssue).not.toHaveBeenCalled();
    expect(box().value).toBe("acme gadgets");
    expect(box()).toHaveAttribute("aria-invalid", "true");
    // Corrected, it takes the refusal off and writes.
    patchIssue.mockResolvedValue({ ...issue, repo: "acme/gadgets", resolved_repo: "acme/gadgets" });
    await write(user, "acme/gadgets");
    await waitFor(() =>
      expect(patchIssue).toHaveBeenCalledWith("CORE-1", { repo: "acme/gadgets" }, keyedAttempt)
    );
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  } finally {
    unmount();
    patchIssue.mockRestore();
  }
});

// The box holds what is typed to GitHub's own grammar, which is what the server holds it to:
// a name GitHub could hold no repository at reaches the box's refusal rather than a round trip,
// and a name GitHub does hold is written whatever its case, its leading dot or its `.git`.
test("RepoControl refuses a name GitHub could hold no repository at", async () => {
  const patchIssue = answerIssuePatches();
  const { unmount, user } = renderControl({ repo: null, resolvedRepo: "acme/widgets" });
  try {
    for (const typed of ["x/..", "x/.", "-bad/x", "own%er/x", `owner/${"a".repeat(101)}`]) {
      await write(user, typed);
      expect(await screen.findByRole("alert")).toHaveTextContent(
        "Write the repository as owner/name."
      );
      expect(box().value).toBe(typed);
      expect(box()).toHaveAttribute("aria-invalid", "true");
    }
    expect(patchIssue).not.toHaveBeenCalled();
    patchIssue.mockResolvedValue({
      ...issue,
      repo: "sjawhar/.knives-ledger",
      resolved_repo: "sjawhar/.knives-ledger",
    });
    await write(user, "Sjawhar/.knives-ledger.git");
    await waitFor(() =>
      expect(patchIssue).toHaveBeenCalledWith(
        "CORE-1",
        { repo: "Sjawhar/.knives-ledger.git" },
        keyedAttempt
      )
    );
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    // An owner GitHub serves that its sign-up form would refuse a new account - a doubled
    // hyphen, a hyphen at the end, a managed user's `_<shortcode>` - names a repository the
    // server takes, so the box takes it too.
    patchIssue.mockResolvedValue({
      ...issue,
      repo: "mona-cat_octo/hello--world",
      resolved_repo: "mona-cat_octo/hello--world",
    });
    await write(user, "mona-cat_octo/hello--world");
    await waitFor(() =>
      expect(patchIssue).toHaveBeenCalledWith(
        "CORE-1",
        { repo: "mona-cat_octo/hello--world" },
        keyedAttempt
      )
    );
  } finally {
    unmount();
    patchIssue.mockRestore();
  }
});

test("RepoControl restores the issue's repository on Escape", async () => {
  const patchIssue = answerIssuePatches();
  const { unmount, user } = renderControl({ repo: "acme/gadgets", resolvedRepo: "acme/gadgets" });
  try {
    await user.click(box());
    await user.paste("-typo");
    expect(box().value).toBe("acme/gadgets-typo");
    await user.keyboard("{Escape}");
    expect(box().value).toBe("acme/gadgets");
    // Escape restored it, so leaving the box writes nothing.
    await user.tab();
    expect(patchIssue).not.toHaveBeenCalled();
  } finally {
    unmount();
    patchIssue.mockRestore();
  }
});

// The placeholder is for sighted readers; the box's description says the same in words, so a
// screen reader hears the target and whether it is the issue's own.
test("RepoControl describes the repository the issue resolves to", async () => {
  const description = () =>
    document.getElementById(box().getAttribute("aria-describedby") ?? "")?.textContent ?? null;
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
      await screen.findByLabelText("Repository of CORE-1");
      expect(description()).toBe(want);
    } finally {
      unmount();
    }
  }
});

// With nothing resolved there is no repository for the placeholder, so it says what the box
// takes, and the description says there is none.
test("RepoControl says No repository when nothing resolves", async () => {
  const { unmount } = renderControl({ repo: null, resolvedRepo: null }, noCoreDefault);
  try {
    expect(await screen.findByText("Repository:")).toBeTruthy();
    expect(box().placeholder).toBe("owner/name");
    expect(box()).toHaveAccessibleDescription("No repository");
  } finally {
    unmount();
  }
});

// A project with no default repository has nothing to suggest, and its issues take a repository
// all the same, so the rail carries the item with an empty suggestion list.
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
    // The project list is read at once; once it is in and React has rendered it, the control has
    // decided.
    await waitFor(() =>
      expect(queryClient.getQueryData<Project[]>(["projects"])).toEqual(noCoreDefault)
    );
    await act(async () => {});
    expect(screen.getByText("Repository:")).toBeTruthy();
    expect(suggestions()).toEqual([]);
    await write(user, "zed/elsewhere");
    await waitFor(() =>
      expect(patchIssue).toHaveBeenCalledWith("CORE-1", { repo: "zed/elsewhere" }, keyedAttempt)
    );
  } finally {
    unmount();
    patchIssue.mockRestore();
  }
});

// What the box lets through, the server decides on: its refusal shows beside the box, and the
// optimistic write is rolled back.
test("RepoControl rolls back and shows the server's refusal", async () => {
  const patchIssue = answerIssuePatches().mockRejectedValue(
    new ApiError(400, {
      code: "INVALID_ISSUE",
      error: 'repo must be "owner/name" (got "acme/gadgets.git/extra")',
    })
  );
  const { unmount, user } = renderControl({ repo: null, resolvedRepo: "acme/widgets" });
  try {
    await write(user, "acme/gadgets");
    expect(
      await screen.findByText('repo must be "owner/name" (got "acme/gadgets.git/extra")')
    ).toBeTruthy();
    expect(box().value).toBe("");
    expect(box().placeholder).toBe("acme/widgets");
  } finally {
    unmount();
    patchIssue.mockRestore();
  }
});

// Enter writes what the box holds, and from that press on the control draws it in every commit,
// with the repository the issue then resolves to: the box, its placeholder and its description.
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
    await user.click(box());
    await user.paste("acme/gadgets");
    const fromPress = commits.markAt(box(), "keydown");
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

// A pick can land before TanStack Query has told the box's host of it: a background tab holds
// that timer back while a request's answer still lands. Dispatch drops a `.git` suffix before it
// stores a repository (canonicalRepo), so a pick of `acme/widgets.git` on an issue that holds
// `acme/widgets` lands as the repository the host passed at the press, and passes still. The box
// draws what Dispatch stored once the host is told, and keeps no pick Dispatch did not store.
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
    await user.clear(box());
    await user.click(box());
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
    expect(box().value).toBe("acme/widgets");
  } finally {
    notifications?.tell();
    unmount();
    patchIssue.mockRestore();
  }
});

/** The control alone, signed in as `who`: the shell holds `/auth/whoami`'s answer before any page
 *  draws (`AuthGate`), so it is seeded here as the shell holds it, and held fresh, so the control
 *  asks it nothing. `inRow` draws it as the value of a row of the new issue page's Details, inside
 *  `NoTooltips` as that page draws it. */
function renderSignedIn(
  who: { login: string; member_kind?: "restricted" },
  own: { repo: string | null; resolvedRepo: string | null },
  { inRow = false }: { inRow?: boolean } = {}
) {
  const listProjects = spyOn(api, "listProjects").mockResolvedValue(projects);
  const queryClient = new QueryClient({
    defaultOptions: {
      mutations: { retry: false },
      queries: { retry: false, staleTime: Number.POSITIVE_INFINITY },
    },
  });
  queryClient.setQueryData(whoAmIQuery().queryKey, { event_head: "0", kind: "user", ...who });
  const control = (
    <RepoControl
      inRow={inRow}
      issueKey="CORE-1"
      project="CORE"
      repo={own.repo}
      resolvedRepo={own.resolvedRepo}
    />
  );
  const view = render(
    <QueryClientProvider client={queryClient}>
      {inRow ? <NoTooltips>{control}</NoTooltips> : control}
    </QueryClientProvider>
  );
  return {
    container: view.container,
    q: within(view.container),
    queryClient,
    unmount: () => {
      view.unmount();
      listProjects.mockRestore();
    },
  };
}

// Dispatch refuses a restricted member every write that names an issue's repository (403
// RESTRICTED_MEMBER), so their control draws the repository the issue resolves to as text, in the
// words the box's description says, and offers no box, no clear control and no suggestions, even
// for an issue nothing resolves for. In a row of the new issue page's Details the same text is the
// row's value, "None" while none resolves, and no button opens a box in its place.
test("a restricted member's RepoControl draws the repository as text, with no box and no clear control", async () => {
  const patchIssue = answerIssuePatches();
  try {
    for (const [own, drawn] of [
      [{ repo: "acme/gadgets", resolvedRepo: "acme/gadgets" }, "acme/gadgets"],
      [
        { repo: null, resolvedRepo: "acme/widgets" },
        "acme/widgets, inherited or the project's default",
      ],
      [{ repo: null, resolvedRepo: null }, "No repository"],
    ] as const) {
      const { q, unmount } = renderSignedIn(
        { login: "bob@outside.example", member_kind: "restricted" },
        own
      );
      try {
        expect(q.queryByRole("combobox", { name: "Repository of CORE-1" })).toBeNull();
        expect(q.queryByRole("textbox")).toBeNull();
        expect(q.queryByRole("button")).toBeNull();
        expect(q.getByText("Repository:").parentElement?.textContent).toBe(`Repository:${drawn}`);
        if (own.resolvedRepo !== null) {
          // The repository in the box's font, the words around it in the line's.
          expect(q.getByText(own.resolvedRepo)).toHaveClass("font-mono");
        }
      } finally {
        unmount();
      }
    }
    for (const [own, drawn] of [
      [{ repo: "acme/gadgets", resolvedRepo: "acme/gadgets" }, "acme/gadgets"],
      [
        { repo: null, resolvedRepo: "acme/widgets" },
        "acme/widgets, inherited or the project's default",
      ],
      [{ repo: null, resolvedRepo: null }, "None"],
    ] as const) {
      const { container, q, unmount } = renderSignedIn(
        { login: "bob@outside.example", member_kind: "restricted" },
        own,
        { inRow: true }
      );
      try {
        expect(q.queryByRole("combobox")).toBeNull();
        expect(q.queryByRole("button")).toBeNull();
        expect(container.textContent).toBe(drawn);
      } finally {
        unmount();
      }
    }
    expect(patchIssue).not.toHaveBeenCalled();
  } finally {
    patchIssue.mockRestore();
  }
});

// A full member's control is the one it always was (R9): the box holds the issue's own
// repository, or names the inherited one as its placeholder and its description, the project's
// default repository from the project list is its suggestion, and the clear control writes null.
test("a full member's RepoControl still draws the box, its suggestions and its clear control", async () => {
  const patchIssue = answerIssuePatches().mockResolvedValue({
    ...issue,
    repo: null,
    resolved_repo: "acme/widgets",
    last_seq: 2,
  });
  try {
    const inherited = renderSignedIn(
      { login: "alice" },
      { repo: null, resolvedRepo: "acme/widgets" }
    );
    try {
      const field = inherited.q.getByRole("combobox", { name: "Repository of CORE-1" });
      expect((field as HTMLInputElement).value).toBe("");
      expect((field as HTMLInputElement).placeholder).toBe("acme/widgets");
      expect(field).toHaveAccessibleDescription("acme/widgets, inherited or the project's default");
      expect(
        inherited.q.queryByRole("button", { name: "Clear the repository of CORE-1" })
      ).toBeNull();
      await waitFor(() =>
        expect(
          Array.from(
            document.getElementById(field.getAttribute("list") ?? "")?.querySelectorAll("option") ??
              []
          ).map((option) => option.value)
        ).toEqual(["acme/widgets"])
      );
    } finally {
      inherited.unmount();
    }

    const user = userEvent.setup();
    const own = renderSignedIn(
      { login: "alice" },
      { repo: "acme/gadgets", resolvedRepo: "acme/gadgets" }
    );
    try {
      const field = own.q.getByRole("combobox", { name: "Repository of CORE-1" });
      expect((field as HTMLInputElement).value).toBe("acme/gadgets");
      expect(field).toHaveAccessibleDescription("acme/gadgets");
      await user.click(own.q.getByRole("button", { name: "Clear the repository of CORE-1" }));
      await waitFor(() =>
        expect(patchIssue).toHaveBeenCalledWith("CORE-1", { repo: null }, keyedAttempt)
      );
      await waitFor(() => expect(own.queryClient.isMutating()).toBe(0));
    } finally {
      own.unmount();
    }
  } finally {
    patchIssue.mockRestore();
  }
});
