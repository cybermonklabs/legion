import { expect, spyOn, test } from "bun:test";
import { SEARCH_QUERY_MAX } from "@legion/contracts/dispatch-tools";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, useLocation } from "react-router-dom";

import { api } from "../../api/client";
import type { IssueSummary, UserState } from "../../api/types";
import { userPreferenceStorageKey } from "../shell/userPreference";
import { IssueBoard } from "./IssueBoard";
import { IssueFilters } from "./IssueFilters";

function issue(overrides: Partial<IssueSummary> = {}): IssueSummary {
  return {
    route: null,
    repo: null,
    resolved_repo: null,
    route_status: null,
    route_holder: null,
    route_status_reason: null,
    key: "CORE-1",
    labels: [],
    last_seq: 0,
    open_asks: 0,
    progress: { tasks: null, children: null },
    parent: null,
    blocked_by: [],
    primary_artifact_id: "artifact-1",
    assignee: null,
    claim: null,
    components: { mode: "inherit", ids: [], unknown: [], reason: null, inherited_from: null },
    status: "todo",
    priority: null,
    rank: "U",
    title: "Core work",
    updated_at: "2026-09-10T00:00:00Z",
    closed_at: overrides.status === "done" ? "2026-09-10T00:00:00Z" : null,
    created_by: null,
    created_at: "2026-09-10T00:00:00Z",
    ...overrides,
  };
}

function LocationSearch() {
  return <output data-testid="location-search">{useLocation().search}</output>;
}

async function openFilters(): Promise<void> {
  const user = userEvent.setup();
  const disclosure = await screen.findByRole("button", { name: /Filters · \d+ active/ });
  if (disclosure.getAttribute("aria-expanded") === "false") {
    await user.click(disclosure);
  }
}

/** Opens the `Labels` picker if it is closed and toggles one option. */
async function pickOption(picker: "Labels", option: string): Promise<void> {
  const user = userEvent.setup();
  const trigger = screen.getByRole("button", { name: new RegExp(`^${picker}( · \\d+)?$`) });
  if (trigger.getAttribute("aria-expanded") === "false") {
    await user.click(trigger);
  }
  await user.click(screen.getByRole("option", { name: option }));
}

/** The Board and its filter strip share the URL. */
function StripAndList() {
  return (
    <>
      <IssueFilters project="CORE" />
      <IssueBoard project="CORE" showEdges />
    </>
  );
}

function renderStrip(
  issues: IssueSummary[],
  state: UserState = {},
  initialEntry = "/projects/CORE",
  login = "alice"
) {
  const listIssues = spyOn(api, "listIssues").mockImplementation(async (options = {}) => {
    const labels = options.label ?? [];
    return labels.length === 0
      ? issues
      : issues.filter((issue) => labels.every((label) => (issue.labels ?? []).includes(label)));
  });
  const getMyState = spyOn(api, "getMyState").mockResolvedValue(state);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  queryClient.setQueryData(["whoami"], { kind: "user", login });
  const tree = () => (
    <MemoryRouter initialEntries={[initialEntry]}>
      <QueryClientProvider client={queryClient}>
        <StripAndList />
        <LocationSearch />
      </QueryClientProvider>
    </MemoryRouter>
  );
  const view = render(tree());
  return { getMyState, listIssues, tree, view };
}

test("Needs you keeps only issues with open asks and lives in the URL", async () => {
  const user = userEvent.setup();
  const { getMyState, listIssues, view } = renderStrip([
    issue({ key: "CORE-1", open_asks: 1, title: "Answer me" }),
    issue({ key: "CORE-2", title: "Quiet" }),
  ]);

  try {
    await screen.findByText("Answer me");
    await openFilters();
    await user.click(screen.getByRole("button", { name: "Needs you" }));
    expect(screen.getByText("Answer me")).toBeTruthy();
    expect(screen.queryByText("Quiet")).not.toBeInTheDocument();
    expect(screen.getByTestId("location-search").textContent).toBe("?needs-you=1");
    await user.click(screen.getByRole("button", { name: "Remove Needs you filter" }));
    expect(screen.getByTestId("location-search").textContent).toBe("");
    expect(await screen.findByText("Quiet")).toBeTruthy();
  } finally {
    view.unmount();
    getMyState.mockRestore();
    listIssues.mockRestore();
  }
});

test("Unread keeps only issues with events past last_read_seq and lives in the URL", async () => {
  const user = userEvent.setup();
  const { getMyState, listIssues, view } = renderStrip(
    [
      issue({ key: "CORE-1", last_seq: 4, title: "Unread" }),
      issue({ key: "CORE-2", last_seq: 3, title: "Read" }),
    ],
    {
      "CORE-1": { dismissed: [], last_read_seq: 3, pinned: false, seq: 0 },
      "CORE-2": { dismissed: [], last_read_seq: 3, pinned: false, seq: 0 },
    }
  );

  try {
    await screen.findByRole("link", { name: /CORE-1.*Unread/ });
    await openFilters();
    await user.click(screen.getByRole("button", { name: "Unread" }));
    expect(screen.getByRole("link", { name: /CORE-1.*Unread/ })).toBeTruthy();
    expect(screen.queryByText("Read")).not.toBeInTheDocument();
    expect(screen.getByTestId("location-search").textContent).toBe("?unread=1");
  } finally {
    view.unmount();
    getMyState.mockRestore();
    listIssues.mockRestore();
  }
});

test("label chips send every selection to the API and retain only AND matches", async () => {
  const { getMyState, listIssues, view } = renderStrip([
    issue({ key: "CORE-1", labels: ["frontend", "api"], title: "Navigation" }),
    issue({ key: "CORE-2", labels: ["frontend", "docs"], title: "Guidance" }),
    issue({ key: "CORE-3", labels: ["backend", "docs"], title: "Endpoint" }),
  ]);

  try {
    await screen.findByText("Navigation");
    await openFilters();
    await pickOption("Labels", "frontend");
    await waitFor(() =>
      expect(listIssues).toHaveBeenCalledWith({ label: ["frontend"], project: "CORE" })
    );
    await pickOption("Labels", "docs");
    await waitFor(() =>
      expect(listIssues).toHaveBeenCalledWith({ label: ["frontend", "docs"], project: "CORE" })
    );
    expect(screen.getByText("Guidance")).toBeTruthy();
    expect(screen.queryByText("Navigation")).not.toBeInTheDocument();
    expect(screen.queryByText("Endpoint")).not.toBeInTheDocument();
  } finally {
    view.unmount();
    getMyState.mockRestore();
    listIssues.mockRestore();
  }
});

test("restores every filter from the URL and writes changes back to it", async () => {
  const user = userEvent.setup();
  const { getMyState, listIssues, view } = renderStrip(
    [
      issue({ key: "CORE-1", labels: ["docs"], title: "Guidance" }),
      issue({ key: "CORE-2", labels: ["frontend"], title: "Navigation" }),
    ],
    {},
    "/projects/CORE?label=docs"
  );

  try {
    await screen.findByText("Guidance");
    await openFilters();
    await waitFor(() =>
      expect(listIssues).toHaveBeenCalledWith({ label: ["docs"], project: "CORE" })
    );
    await user.click(screen.getByRole("button", { name: "Labels · 1" }));
    expect(screen.getByRole("option", { name: "docs" })).toHaveAttribute("aria-selected", "true");
    expect(screen.getByTestId("location-search").textContent).toBe("?label=docs");

    await pickOption("Labels", "frontend");
    await waitFor(() =>
      expect(listIssues).toHaveBeenCalledWith({ label: ["docs", "frontend"], project: "CORE" })
    );
    expect(screen.getByTestId("location-search").textContent).toBe("?label=docs&label=frontend");

    await user.click(screen.getByRole("searchbox", { name: "Search issues" }));
    await user.paste("guid");
    expect(screen.getByTestId("location-search").textContent).toBe(
      "?label=docs&label=frontend&q=guid"
    );
    await user.click(screen.getByRole("button", { name: "Remove Search: guid filter" }));

    // Clicking into the search field closed the label picker, as it does in a browser; Clear
    // labels is inside it.
    await user.click(screen.getByRole("button", { name: "Labels · 2" }));
    await user.click(screen.getByRole("button", { name: "Clear labels" }));
    expect(screen.getByTestId("location-search").textContent).toBe("");
  } finally {
    view.unmount();
    getMyState.mockRestore();
    listIssues.mockRestore();
  }
});

test("a search restored from ?q= narrows the list before anyone types", async () => {
  const { getMyState, listIssues, view } = renderStrip(
    [issue({ key: "CORE-1", title: "Guidance" }), issue({ key: "CORE-2", title: "Navigation" })],
    {},
    "/projects/CORE?q=guid"
  );

  try {
    await screen.findByText("Guidance");
    expect(screen.queryByText("Navigation")).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Remove Search: guid filter" })).toBeTruthy();
  } finally {
    view.unmount();
    getMyState.mockRestore();
    listIssues.mockRestore();
  }
});

test("the search filter writes at most SEARCH_QUERY_MAX characters into the page URL", async () => {
  const user = userEvent.setup();
  const { getMyState, listIssues, view } = renderStrip([
    issue({ key: "CORE-1", title: "Guidance" }),
  ]);

  try {
    await screen.findByText("Guidance");
    await openFilters();
    await user.click(screen.getByRole("searchbox", { name: "Search issues" }));
    await user.paste("x".repeat(SEARCH_QUERY_MAX + 500));
    const written = new URLSearchParams(screen.getByTestId("location-search").textContent ?? "");
    expect(written.get("q")).toBe("x".repeat(SEARCH_QUERY_MAX));
  } finally {
    view.unmount();
    getMyState.mockRestore();
    listIssues.mockRestore();
  }
});

test("a ?q= over the cap from a link is cut on a code point, never leaving half an emoji", async () => {
  const user = userEvent.setup();
  const long = `${"x".repeat(SEARCH_QUERY_MAX - 1)}😀`;
  const { getMyState, listIssues, view } = renderStrip(
    [issue({ key: "CORE-1", title: "Guidance" })],
    {},
    `/projects/CORE?q=${encodeURIComponent(long)}`
  );

  try {
    await openFilters();
    const search = screen.getByRole("searchbox", { name: "Search issues" });
    await user.clear(search);
    await user.click(search);
    await user.paste(`${long}y`);
    const written = new URLSearchParams(screen.getByTestId("location-search").textContent ?? "");
    expect(written.get("q")).toBe("x".repeat(SEARCH_QUERY_MAX - 1));
  } finally {
    view.unmount();
    getMyState.mockRestore();
    listIssues.mockRestore();
  }
});

test("collapses filter controls by default and retains the disclosure preference per login", async () => {
  const user = userEvent.setup();
  window.localStorage.clear();
  const { getMyState, listIssues, view } = renderStrip([issue()]);

  try {
    await screen.findByText("Core work");
    const disclosure = screen.getByRole("button", { name: "Filters · 0 active" });
    expect(disclosure).toHaveAttribute("aria-expanded", "false");
    expect(screen.queryByRole("button", { name: "Labels" })).not.toBeInTheDocument();

    await user.click(disclosure);
    await user.click(screen.getByRole("button", { name: "Needs you" }));
    expect(screen.getByRole("button", { name: "Filters · 1 active" })).toHaveAttribute(
      "aria-expanded",
      "true"
    );

    await user.click(screen.getByRole("button", { name: "Filters · 1 active" }));
    await user.click(screen.getByRole("button", { name: "Remove Needs you filter" }));
    expect(screen.getByRole("button", { name: "Filters · 0 active" })).toBeTruthy();
    expect(
      window.localStorage.getItem(userPreferenceStorageKey("alice", "project.issue-filters"))
    ).toBe("collapsed");
  } finally {
    view.unmount();
    getMyState.mockRestore();
    listIssues.mockRestore();
    window.localStorage.clear();
  }
});

test("collapses a saved filter disclosure when no filters are active", async () => {
  window.localStorage.setItem(
    userPreferenceStorageKey("alice", "project.issue-filters"),
    "expanded"
  );
  const { getMyState, listIssues, view } = renderStrip([issue()]);

  try {
    await screen.findByText("Core work");
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Filters · 0 active" })).toHaveAttribute(
        "aria-expanded",
        "false"
      )
    );
  } finally {
    view.unmount();
    getMyState.mockRestore();
    listIssues.mockRestore();
    window.localStorage.clear();
  }
});

test("a pending viewer keeps a URL-only filter disclosure collapsed without persisting", async () => {
  const pendingKey = "dispatch.project.issue-filters:undefined";
  window.localStorage.removeItem(pendingKey);
  const whoAmI = spyOn(api, "whoAmI").mockImplementation(
    () => Promise.withResolvers<never>().promise
  );
  const listIssues = spyOn(api, "listIssues").mockResolvedValue([issue()]);
  const getMyState = spyOn(api, "getMyState").mockResolvedValue({});
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter initialEntries={["/projects/CORE?q=guid"]}>
      <QueryClientProvider client={queryClient}>
        <StripAndList />
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    const disclosure = await screen.findByRole("button", { name: "Filters · 1 active" });
    expect(disclosure).toHaveAttribute("aria-expanded", "false");
    expect(window.localStorage.getItem(pendingKey)).toBeNull();
  } finally {
    view.unmount();
    getMyState.mockRestore();
    listIssues.mockRestore();
    whoAmI.mockRestore();
    window.localStorage.removeItem(pendingKey);
  }
});

test("a ?status= in the URL is neither counted nor chipped by the strip", async () => {
  const { getMyState, listIssues, view } = renderStrip(
    [issue()],
    {},
    "/projects/CORE?label=x&status=done"
  );

  try {
    await screen.findByRole("button", { name: "Filters · 1 active" });
    expect(screen.queryByRole("button", { name: "Status" })).not.toBeInTheDocument();
    expect(
      screen.queryByRole("button", { name: "Remove Status: Done filter" })
    ).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Remove Label: x filter" })).toBeTruthy();
  } finally {
    view.unmount();
    getMyState.mockRestore();
    listIssues.mockRestore();
  }
});
