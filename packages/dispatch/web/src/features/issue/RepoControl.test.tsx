import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
// biome-ignore lint/style/noRestrictedImports: this file keeps fireEvent for the focus that arms the lazy read of the project's mappings, which user-event has no form for
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
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
  return screen.getByRole("combobox", { name: "Repository of CORE-1" }) as HTMLInputElement;
}

/** The repositories the box's `<datalist>` offers, in its order. */
function offered(): string[] {
  const list = document.getElementById(box().getAttribute("list") ?? "");
  return Array.from(list?.querySelectorAll("option") ?? []).map((option) => option.value);
}

/** The visible badge; the box's description repeats its text in a span of its own. */
const badge = 'span[aria-hidden="true"]';

test("RepoControl offers the project's mapped repositories, shows the resolved one, and patches repo", async () => {
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
    // The badge names the repository the issue resolves to; no mapping is read until the reader
    // reaches for the control.
    expect(screen.getByText("acme/widgets", { selector: badge })).toBeTruthy();
    expect(listRepoProjects).not.toHaveBeenCalled();
    // A focus with no press before it: user-event has no form for a bare focus.
    fireEvent.focus(box());
    await waitFor(() => expect(listRepoProjects).toHaveBeenCalled());
    // CORE's mappings sorted; OTHER's absent.
    await waitFor(() => expect(offered()).toEqual(["acme/gadgets", "acme/widgets"]));
    expect(box().value).toBe("");
    await user.type(box(), "acme/gadgets{Enter}");
    await waitFor(() =>
      expect(patchIssue).toHaveBeenCalledWith("CORE-1", { repo: "acme/gadgets" }, keyedAttempt)
    );
    await waitFor(() =>
      expect(queryClient.getQueryData<IssueDetails>(detailKey)?.last_seq).toBe(2)
    );
    expect(box().value).toBe("acme/gadgets");
    expect(screen.getByText("acme/gadgets", { selector: badge })).toBeTruthy();
    expect(queryClient.getQueryData<IssueSummary[]>(listKey)?.[0]?.repo).toBe("acme/gadgets");
  } finally {
    unmount();
    patchIssue.mockRestore();
  }
});

// Any owner/name goes, whether the project maps it or not: the mappings are an offer, not the
// set of values the box takes.
test("RepoControl writes an owner/name the project does not map", async () => {
  const patchIssue = answerIssuePatches().mockResolvedValue({
    ...issue,
    repo: "other/thing",
    resolved_repo: "other/thing",
  });
  const { unmount, user } = renderControl({ repo: null, resolvedRepo: "acme/widgets" });
  try {
    await user.type(box(), "other/thing{Enter}");
    await waitFor(() =>
      expect(patchIssue).toHaveBeenCalledWith("CORE-1", { repo: "other/thing" }, keyedAttempt)
    );
    expect(offered()).not.toContain("other/thing");
    await waitFor(() => expect(screen.getByText("other/thing", { selector: badge })).toBeTruthy());
  } finally {
    unmount();
    patchIssue.mockRestore();
  }
});

test("RepoControl sends null for Inherit", async () => {
  const patchIssue = answerIssuePatches().mockResolvedValue({
    ...issue,
    repo: null,
    resolved_repo: "acme/widgets",
  });
  const { unmount, user } = renderControl({ repo: "acme/gadgets", resolvedRepo: "acme/gadgets" });
  try {
    // The issue's own repository is in the box before the mapping list has loaded.
    expect(box().value).toBe("acme/gadgets");
    await user.click(screen.getByRole("button", { name: "Inherit" }));
    await waitFor(() =>
      expect(patchIssue).toHaveBeenCalledWith("CORE-1", { repo: null }, keyedAttempt)
    );
    expect(box().value).toBe("");
    await waitFor(() => expect(screen.getByText("acme/widgets", { selector: badge })).toBeTruthy());
  } finally {
    unmount();
    patchIssue.mockRestore();
  }
});

// The badge is decoration for sighted readers (aria-hidden); the box names the repository the
// issue resolves to in its description, so a screen reader hears the target as well as the value.
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
      await screen.findByRole("combobox", { name: "Repository of CORE-1" });
      expect(description()).toBe(want);
    } finally {
      unmount();
    }
  }
});

// With nothing resolved the control cannot tell from the issue whether its project maps any
// repository, so it reads the mappings at once: the badge says nothing resolves either way.
test("RepoControl says No repository when nothing resolves in a project that maps one", async () => {
  const { listRepoProjects, unmount } = renderControl({ repo: null, resolvedRepo: null });
  try {
    expect(await screen.findByText("No repository", { selector: badge })).toBeTruthy();
    expect(screen.getByText("Repository:")).toBeTruthy();
    expect(listRepoProjects).toHaveBeenCalledTimes(1);
  } finally {
    unmount();
  }
});

// A project that maps no repository has nothing to offer, and the issue can still carry a
// repository, so the item is there with an empty list.
test("RepoControl takes a repository in a project that maps none", async () => {
  const others = mappings.filter((mapping) => mapping.project !== "CORE");
  const patchIssue = answerIssuePatches().mockResolvedValue({
    ...issue,
    repo: "other/thing",
    resolved_repo: "other/thing",
  });
  const { queryClient, unmount, user } = renderControl({ repo: null, resolvedRepo: null }, others);
  try {
    await waitFor(() =>
      expect(queryClient.getQueryData<RepoProject[]>(["repo-projects"])).toEqual(others)
    );
    await act(async () => {});
    expect(screen.getByText("Repository:")).toBeTruthy();
    expect(offered()).toEqual([]);
    await user.type(box(), "other/thing{Enter}");
    await waitFor(() =>
      expect(patchIssue).toHaveBeenCalledWith("CORE-1", { repo: "other/thing" }, keyedAttempt)
    );
  } finally {
    unmount();
    patchIssue.mockRestore();
  }
});

// A failed mappings read takes nothing away: the box still writes, and the failure says so and
// offers the read again.
test("RepoControl shows a failed mappings read with a retry beside the box", async () => {
  const { listRepoProjects, unmount, user } = renderControl(
    { repo: null, resolvedRepo: null },
    mappings,
    { failFirstRead: true }
  );
  try {
    expect(await screen.findByText("Could not load the repositories mapped to CORE.")).toBeTruthy();
    expect(screen.getByText("Repository:")).toBeTruthy();
    expect(box()).toBeTruthy();
    await user.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(listRepoProjects).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(offered()).toEqual(["acme/gadgets", "acme/widgets"]));
    expect(
      screen.queryByText("Could not load the repositories mapped to CORE.")
    ).not.toBeInTheDocument();
    expect(screen.getByText("No repository", { selector: badge })).toBeTruthy();
  } finally {
    unmount();
  }
});

// With a repository resolved the box still shows it, and a failed read says so beside it.
test("RepoControl shows a failed mappings read beside a resolved repository", async () => {
  const { unmount } = renderControl({ repo: null, resolvedRepo: "acme/widgets" }, mappings, {
    failFirstRead: true,
  });
  try {
    // A focus with no press before it: user-event has no form for a bare focus.
    fireEvent.focus(box());
    expect(await screen.findByText("Could not load the repositories mapped to CORE.")).toBeTruthy();
    expect(screen.getByText("acme/widgets", { selector: badge })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Retry" })).toBeTruthy();
  } finally {
    unmount();
  }
});

test("RepoControl rolls back and shows the server's refusal", async () => {
  const patchIssue = answerIssuePatches().mockRejectedValue(
    new ApiError(400, {
      code: "INVALID_ISSUE",
      error: "repo must be owner/name",
    })
  );
  const { unmount, user } = renderControl({ repo: null, resolvedRepo: "acme/widgets" });
  try {
    await user.type(box(), "acme gadgets{Enter}");
    expect(await screen.findByText("repo must be owner/name")).toBeTruthy();
    expect(box().value).toBe("");
    expect(screen.getByText("acme/widgets", { selector: badge })).toBeTruthy();
  } finally {
    unmount();
    patchIssue.mockRestore();
  }
});
