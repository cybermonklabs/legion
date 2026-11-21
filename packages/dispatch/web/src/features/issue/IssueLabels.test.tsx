import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { api } from "../../api/client";
import type { IssueSummary } from "../../api/types";
import { IssueLabels } from "./IssueLabels";

function summary(key: string, labels: string[]): IssueSummary {
  return {
    advances: [],
    advances_revision: 0,
    route: null,
    repo: null,
    resolved_repo: null,
    route_status: null,
    route_holder: null,
    route_status_reason: null,
    key,
    labels,
    last_seq: 1,
    open_asks: 0,
    progress: { tasks: null, children: null },
    parent: null,
    blocked_by: [],
    primary_artifact_id: "artifact-1",
    assignee: null,
    claim: null,
    components: { mode: "inherit", ids: [], unknown: [], reason: null, inherited_from: null },
    priority: null,
    rank: "U",
    status: "todo",
    title: key,
    updated_at: "2026-09-09T00:00:00Z",
    closed_at: null,
    created_by: null,
    created_at: "2026-09-09T00:00:00Z",
  };
}

test("the label cloud reads the project's loaded issue list instead of fetching its own copy", async () => {
  const user = userEvent.setup();
  const listIssues = spyOn(api, "listIssues").mockResolvedValue([]);
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: 30_000 } },
  });
  queryClient.setQueryData(
    ["issues", "project", "CORE"],
    [summary("CORE-1", ["bug"]), summary("CORE-2", ["frontend", "docs"])]
  );
  const view = render(
    <QueryClientProvider client={queryClient}>
      <IssueLabels
        disabled={false}
        externalLinks={[]}
        labels={["bug"]}
        onSave={() => Promise.resolve()}
        project="CORE"
        saving={false}
      />
    </QueryClientProvider>
  );

  try {
    await user.click(screen.getByRole("button", { name: "Edit labels" }));
    await screen.findByRole("combobox", { name: "Search or create label" });

    expect(await screen.findByRole("option", { name: "frontend" })).toBeDefined();
    expect(screen.getByRole("option", { name: "docs" })).toBeDefined();
    expect(listIssues).not.toHaveBeenCalled();
  } finally {
    view.unmount();
    listIssues.mockRestore();
  }
});

test("a rejected create shows its error until the next keystroke clears it", async () => {
  const user = userEvent.setup();
  const listIssues = spyOn(api, "listIssues").mockResolvedValue([]);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <QueryClientProvider client={queryClient}>
      <IssueLabels
        disabled={false}
        externalLinks={[]}
        labels={[]}
        onSave={() => Promise.resolve()}
        project="CORE"
        saving={false}
      />
    </QueryClientProvider>
  );

  try {
    await user.click(screen.getByRole("button", { name: "Edit labels" }));
    const search = await screen.findByRole("combobox", { name: "Search or create label" });
    const tooLong = "x".repeat(41);
    await user.click(search);
    await user.paste(tooLong);
    await user.click(screen.getByRole("option", { name: `Create "${tooLong}"` }));
    expect(screen.getByText("Labels must be 1–40 characters.")).toBeTruthy();
    expect(screen.queryByRole("option", { name: tooLong })).not.toBeInTheDocument();

    await user.clear(search);
    await user.click(search);
    await user.paste("x");
    expect(screen.queryByText("Labels must be 1–40 characters.")).not.toBeInTheDocument();
  } finally {
    view.unmount();
    listIssues.mockRestore();
  }
});

/** A repository name of 36 characters, which gives the 43-character `repo:` label Dispatch writes
 *  when it imports one of the repository's issues through the default project. */
const ownRepo = `o/${"r".repeat(36)}`;
const ownRepoLabel = `repo:${ownRepo}`;
/** The GitHub issue link that import records: the reference the issue's own `repo:` label names. */
const ownRepoLinks = [{ kind: "github_issue", url: `https://github.com/${ownRepo}/issues/1` }];
/** Twenty labels beside the `repo:` one, the most a caller can give an issue. */
const twentyLabels = Array.from({ length: 20 }, (_, index) => `area-${index + 1}`);

/** The editor of CORE-1, imported with `ownRepoLabel` and holding `twentyLabels` too, with
 *  `projectIssues` as the project's loaded issue list (kept fresh, so opening the popover
 *  fetches nothing). `saves` collects what each close sends. */
function renderImportedIssue(projectIssues: IssueSummary[]) {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: 30_000 } },
  });
  queryClient.setQueryData(["issues", "project", "CORE"], projectIssues);
  const saves: string[][] = [];
  const view = render(
    <QueryClientProvider client={queryClient}>
      <IssueLabels
        disabled={false}
        externalLinks={ownRepoLinks}
        labels={[ownRepoLabel, ...twentyLabels]}
        onSave={(labels) => {
          saves.push(labels);
          return Promise.resolve();
        }}
        project="CORE"
        saving={false}
      />
    </QueryClientProvider>
  );
  return { saves, view };
}

test("an issue's own long repo: label counts toward neither limit and is saved back unchanged", async () => {
  const user = userEvent.setup();
  const listIssues = spyOn(api, "listIssues").mockResolvedValue([]);
  const { saves, view } = renderImportedIssue([summary("CORE-1", [ownRepoLabel, ...twentyLabels])]);

  try {
    await user.click(screen.getByRole("button", { name: "Edit labels" }));
    const search = await screen.findByRole("combobox", { name: "Search or create label" });
    // Offered although it is over 40 characters, so a person can still take it off.
    expect(screen.getByRole("option", { name: ownRepoLabel })).toHaveAttribute(
      "aria-selected",
      "true"
    );

    await user.click(screen.getByRole("option", { name: "area-20" }));
    await user.click(search);
    await user.paste("triaged");
    await user.click(screen.getByRole("option", { name: 'Create "triaged"' }));
    expect(screen.queryByText("An issue can have at most 20 labels.")).not.toBeInTheDocument();
    expect(screen.getByRole("option", { name: "triaged" })).toHaveAttribute(
      "aria-selected",
      "true"
    );

    await user.click(search);
    await user.paste("one-too-many");
    await user.click(screen.getByRole("option", { name: 'Create "one-too-many"' }));
    expect(screen.getByText("An issue can have at most 20 labels.")).toBeTruthy();
    expect(screen.queryByRole("option", { name: "one-too-many" })).not.toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Edit labels" }));
    expect(saves).toEqual([[ownRepoLabel, ...twentyLabels.slice(0, 19), "triaged"]]);
  } finally {
    view.unmount();
    listIssues.mockRestore();
  }
});

test("another issue's long repo: label is not offered, and typed it is refused as too long", async () => {
  const user = userEvent.setup();
  const listIssues = spyOn(api, "listIssues").mockResolvedValue([]);
  const otherRepoLabel = `repo:o/${"s".repeat(36)}`;
  const { view } = renderImportedIssue([
    summary("CORE-1", [ownRepoLabel, ...twentyLabels]),
    summary("CORE-2", [otherRepoLabel, "frontend"]),
  ]);

  try {
    await user.click(screen.getByRole("button", { name: "Edit labels" }));
    const search = await screen.findByRole("combobox", { name: "Search or create label" });
    expect(screen.getByRole("option", { name: "frontend" })).toBeDefined();
    expect(screen.queryByRole("option", { name: otherRepoLabel })).not.toBeInTheDocument();

    await user.click(search);
    await user.paste(otherRepoLabel);
    await user.click(screen.getByRole("option", { name: `Create "${otherRepoLabel}"` }));
    expect(screen.getByText("Labels must be 1–40 characters.")).toBeTruthy();
    expect(screen.queryByRole("option", { name: otherRepoLabel })).not.toBeInTheDocument();
    expect(listIssues).not.toHaveBeenCalled();
  } finally {
    view.unmount();
    listIssues.mockRestore();
  }
});

test("another issue's short repo: label picked beside the 20 others is refused as a 21st label", async () => {
  const user = userEvent.setup();
  const listIssues = spyOn(api, "listIssues").mockResolvedValue([]);
  const { saves, view } = renderImportedIssue([
    summary("CORE-1", [ownRepoLabel, ...twentyLabels]),
    summary("CORE-2", ["repo:o/short"]),
  ]);

  try {
    await user.click(screen.getByRole("button", { name: "Edit labels" }));
    await screen.findByRole("combobox", { name: "Search or create label" });
    await user.click(screen.getByRole("option", { name: "repo:o/short" }));
    expect(screen.getByText("An issue can have at most 20 labels.")).toBeTruthy();
    expect(screen.getByRole("option", { name: "repo:o/short" })).toHaveAttribute(
      "aria-selected",
      "false"
    );

    await user.click(screen.getByRole("button", { name: "Edit labels" }));
    expect(saves).toEqual([]);
  } finally {
    view.unmount();
    listIssues.mockRestore();
  }
});
