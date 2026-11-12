import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
// biome-ignore lint/style/noRestrictedImports: this file keeps fireEvent for the focus that arms the lazy read of the project's mappings, which user-event has no form for
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";
import type { ReactNode } from "react";
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { answerIssuePatches } from "../../__tests__/send-answers";
import { ApiError, api } from "../../api/client";
import type { Issue, IssueDetails, IssueSummary, RepoProject } from "../../api/types";
import { RepoControl } from "./RepoControl";

const issue: Issue = {
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
};
const mappings: RepoProject[] = [
  {
    repo: "acme/widgets",
    project: "CORE",
    default: true,
    renamed_to: null,
    created_by: { kind: "user", id: "alice" },
    created_at: "2026-10-01T00:00:00Z",
  },
  {
    repo: "acme/gadgets",
    project: "CORE",
    default: false,
    renamed_to: null,
    created_by: { kind: "user", id: "alice" },
    created_at: "2026-10-01T00:00:00Z",
  },
  {
    repo: "other/thing",
    project: "OTHER",
    default: true,
    renamed_to: null,
    created_by: { kind: "user", id: "alice" },
    created_at: "2026-10-01T00:00:00Z",
  },
];

const detailKey = ["issue", issue.key] as const;
const listKey = ["issues", "project", issue.project] as const;

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
    <RepoControl
      issueKey={cached.data.key}
      project={cached.data.project}
      repo={cached.data.repo}
      resolvedRepo={cached.data.resolved_repo}
    />
  );
}

function renderControl(
  own: { repo: string | null; resolvedRepo: string | null },
  mapped: RepoProject[] = mappings,
  { failFirstRead = false }: { failFirstRead?: boolean } = {}
) {
  const listRepoProjects = spyOn(api, "listRepoProjects").mockResolvedValue(mapped);
  if (failFirstRead) {
    listRepoProjects.mockRejectedValueOnce(new Error("the network dropped the read"));
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
    open_asks: [],
    progress: { tasks: null, children: null },
    referenced_by_count: 0,
  });
  queryClient.setQueryData(listKey, [
    { ...summary, repo: own.repo, resolved_repo: own.resolvedRepo },
  ]);
  const user = userEvent.setup();
  const view = render(
    <QueryClientProvider client={queryClient}>
      <CachedControl />
    </QueryClientProvider>
  );
  return {
    user,
    listRepoProjects,
    queryClient,
    unmount: () => {
      view.unmount();
      listRepoProjects.mockRestore();
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

test("RepoControl suggests the project's mapped repositories, shows the resolved one, and patches repo", async () => {
  const patchIssue = answerIssuePatches().mockResolvedValue({
    ...issue,
    repo: "acme/gadgets",
    resolved_repo: "acme/gadgets",
    last_seq: 2,
  });
  const { listRepoProjects, queryClient, unmount, user } = renderControl({
    repo: null,
    resolvedRepo: "acme/widgets",
  });
  try {
    // The box holds the issue's own repository, which it has none of, so the one it resolves to
    // is its placeholder; no mapping is read until the reader reaches for the control.
    expect(box().value).toBe("");
    expect(box().placeholder).toBe("acme/widgets");
    expect(listRepoProjects).not.toHaveBeenCalled();
    // A focus with no press before it: user-event has no form for a bare focus.
    fireEvent.focus(box());
    await waitFor(() => expect(listRepoProjects).toHaveBeenCalled());
    // CORE's mappings sorted; OTHER's absent.
    await waitFor(() => expect(suggestions()).toEqual(["acme/gadgets", "acme/widgets"]));
    await write(user, "acme/gadgets");
    await waitFor(() =>
      expect(patchIssue).toHaveBeenCalledWith("CORE-1", { repo: "acme/gadgets" }, keyedAttempt)
    );
    await waitFor(() =>
      expect(queryClient.getQueryData<IssueDetails>(detailKey)?.last_seq).toBe(2)
    );
    expect(box().value).toBe("acme/gadgets");
    expect(queryClient.getQueryData<IssueSummary[]>(listKey)?.[0]?.repo).toBe("acme/gadgets");
  } finally {
    unmount();
    patchIssue.mockRestore();
  }
});

// Any `owner/name` is a repository the server takes, so the box is not held to the project's
// mappings: what the reader types is what is written.
test("RepoControl writes an owner/name the project does not map", async () => {
  const patchIssue = answerIssuePatches().mockResolvedValue({
    ...issue,
    repo: "zed/unmapped",
    resolved_repo: "zed/unmapped",
  });
  const { queryClient, unmount, user } = renderControl({
    repo: null,
    resolvedRepo: "acme/widgets",
  });
  try {
    await write(user, "zed/unmapped");
    await waitFor(() =>
      expect(patchIssue).toHaveBeenCalledWith("CORE-1", { repo: "zed/unmapped" }, keyedAttempt)
    );
    await waitFor(() =>
      expect(queryClient.getQueryData<IssueDetails>(detailKey)?.repo).toBe("zed/unmapped")
    );
    expect(box().value).toBe("zed/unmapped");
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
    // The issue's own repository is in the box before the mapping list has loaded.
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
test("RepoControl says No repository when nothing resolves in a project that maps one", async () => {
  const { listRepoProjects, unmount } = renderControl({ repo: null, resolvedRepo: null });
  try {
    expect(await screen.findByText("Repository:")).toBeTruthy();
    expect(box().placeholder).toBe("owner/name");
    expect(box()).toHaveAccessibleDescription("No repository");
    expect(listRepoProjects).toHaveBeenCalledTimes(1);
  } finally {
    unmount();
  }
});

// A project that maps no repository has nothing to suggest, and its issues take a repository all
// the same, so the rail carries the item with an empty suggestion list.
test("RepoControl shows the item for an issue whose project maps no repository", async () => {
  const others = mappings.filter((mapping) => mapping.project !== "CORE");
  const patchIssue = answerIssuePatches().mockResolvedValue({
    ...issue,
    repo: "zed/unmapped",
    resolved_repo: "zed/unmapped",
  });
  const { queryClient, unmount, user } = renderControl({ repo: null, resolvedRepo: null }, others);
  try {
    // The mappings are read at once; once they are in and React has rendered them, the control
    // has decided.
    await waitFor(() =>
      expect(queryClient.getQueryData<RepoProject[]>(["repo-projects"])).toEqual(others)
    );
    await act(async () => {});
    expect(screen.getByText("Repository:")).toBeTruthy();
    expect(suggestions()).toEqual([]);
    await write(user, "zed/unmapped");
    await waitFor(() =>
      expect(patchIssue).toHaveBeenCalledWith("CORE-1", { repo: "zed/unmapped" }, keyedAttempt)
    );
  } finally {
    unmount();
    patchIssue.mockRestore();
  }
});

// A failed mappings read costs the reader the suggestions and nothing else: the box still takes
// a repository, and the read is offered again.
test("RepoControl shows a failed mappings read with a retry instead of hiding the item", async () => {
  const { listRepoProjects, unmount, user } = renderControl(
    { repo: null, resolvedRepo: null },
    mappings,
    { failFirstRead: true }
  );
  try {
    expect(await screen.findByText("Could not load the repositories mapped to CORE.")).toBeTruthy();
    expect(screen.getByText("Repository:")).toBeTruthy();
    await user.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(listRepoProjects).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(suggestions()).toEqual(["acme/gadgets", "acme/widgets"]));
    expect(
      screen.queryByText("Could not load the repositories mapped to CORE.")
    ).not.toBeInTheDocument();
    expect(box().placeholder).toBe("owner/name");
  } finally {
    unmount();
  }
});

// With a repository resolved the box still names it, and a failed read says so beside it.
test("RepoControl shows a failed mappings read beside a resolved repository", async () => {
  const { unmount } = renderControl({ repo: null, resolvedRepo: "acme/widgets" }, mappings, {
    failFirstRead: true,
  });
  try {
    // A focus with no press before it: user-event has no form for a bare focus.
    fireEvent.focus(box());
    expect(await screen.findByText("Could not load the repositories mapped to CORE.")).toBeTruthy();
    expect(box().placeholder).toBe("acme/widgets");
    expect(screen.getByRole("button", { name: "Retry" })).toBeTruthy();
  } finally {
    unmount();
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
