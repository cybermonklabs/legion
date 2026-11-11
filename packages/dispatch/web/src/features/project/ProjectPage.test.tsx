import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, waitForElementToBeRemoved, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http } from "msw";
import { MemoryRouter, useLocation } from "react-router-dom";

import { installFakeLiveSockets } from "../../__tests__/live-socket-harness";

import { answerRequests } from "../../__tests__/network";
import { ApiError, api } from "../../api/client";
import type { ArchitectureSource, InboxRow } from "../../api/types";
import { KeymapProvider } from "../shell/KeymapProvider";
import { ProjectPage } from "./ProjectPage";

function LocationProbe() {
  const location = useLocation();
  return (
    <output data-testid="location">
      {location.pathname}
      {location.search}
    </output>
  );
}

function inboxRow(overrides: Partial<InboxRow> = {}): InboxRow {
  return {
    anchor: null,
    answer: null,
    artifact_id: null,
    author: { id: "session-1", kind: "session" },
    block_id: null,
    created_at: "2026-09-11T00:00:00Z",
    edited_at: null,
    id: "ask-1",
    // The pill counts the reader's own: `renderPage` signs in as `alice`.
    issue: { assignee: "alice", key: "CORE-1", title: "Fix the thing" },
    issue_key: "CORE-1",
    last_reply: null,
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
    question: "Which approach?",
    state: "open",
    waiting_on: "human",
    urgency: "med",
    ...overrides,
  };
}

/** `sources` is what `GET /projects/CORE/architecture-sources` answers: the project's sources (a
 *  non-empty list opens it on Architecture; `[]`, the default, on Issues, as every render here
 *  expects unless it says otherwise), an `ApiError` for a failure, or `"pending"` for a lookup
 *  that never settles. `legionProjects` names the projects `GET /api/v1/settings/legion` lists:
 *  only those have a Legion view. */
function renderPage(
  path: string,
  projects = [
    {
      created_at: "2026-09-10T00:00:00Z",
      key: "CORE",
      name: "Core",
      open_asks: 0,
      gated_steps: [],
    },
  ],
  login: string | null = "alice",
  inboxRows: InboxRow[] = [],
  sources: ArchitectureSource[] | ApiError | "pending" = [],
  legionProjects: readonly string[] = []
) {
  // The project's issue list is read through `api.listIssues`, which the spy below answers -
  // except for a fetch the last assertion of a test starts: its query function awaits the
  // board-move gate first, so the call lands a microtask later, after this render's spies are
  // restored. That one reaches the client, and this answers it with the same empty list.
  answerRequests(http.get("/api/v1/issues", () => Response.json([])));
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(inboxRows);
  const whoAmI = spyOn(api, "whoAmI");
  if (login === null) {
    whoAmI.mockImplementation(() => Promise.withResolvers<never>().promise);
  } else {
    whoAmI.mockResolvedValue({ event_head: "0", kind: "user", login });
  }

  const listProjects = spyOn(api, "listProjects").mockResolvedValue(projects);
  const listIssues = spyOn(api, "listIssues").mockResolvedValue([]);
  const getMyState = spyOn(api, "getMyState").mockResolvedValue({});
  const listProjectArtifacts = spyOn(api, "listProjectArtifacts").mockResolvedValue([]);
  const listProjectArchitectureSources = spyOn(api, "listProjectArchitectureSources");
  if (sources === "pending") {
    listProjectArchitectureSources.mockImplementation(() => Promise.withResolvers<never>().promise);
  } else if (sources instanceof ApiError) {
    listProjectArchitectureSources.mockRejectedValue(sources);
  } else {
    listProjectArchitectureSources.mockResolvedValue(sources);
  }
  // The Architecture pane's own tree fetch is not under test here; it never settles.
  const getArchitecture = spyOn(api, "getArchitecture").mockImplementation(
    () => Promise.withResolvers<never>().promise
  );
  const listLegionSettings = spyOn(api, "listLegionSettings").mockResolvedValue(
    legionProjects.map((project) => ({
      created_by: { id: "alice", kind: "user" as const },
      project,
      state_url: "http://legion-daemon.example:13370",
      updated_at: "2026-11-01T00:00:00Z",
    }))
  );
  const getLegionMetrics = spyOn(api, "getLegionMetrics").mockImplementation(
    () => Promise.withResolvers<never>().promise
  );
  const getDeliverySettings = spyOn(api, "getDeliverySettings").mockResolvedValue(null);
  const listRepoProjects = spyOn(api, "listRepoProjects").mockResolvedValue([]);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter initialEntries={[path]}>
      <QueryClientProvider client={queryClient}>
        <KeymapProvider>
          <ProjectPage />
          <LocationProbe />
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  const restore = () => {
    view.unmount();
    getArchitecture.mockRestore();
    listProjectArchitectureSources.mockRestore();
    getInbox.mockRestore();
    getMyState.mockRestore();
    listIssues.mockRestore();
    listLegionSettings.mockRestore();
    getLegionMetrics.mockRestore();
    getDeliverySettings.mockRestore();
    listRepoProjects.mockRestore();
    listProjectArtifacts.mockRestore();
    listProjects.mockRestore();
    whoAmI.mockRestore();
  };
  return {
    listProjectArchitectureSources,
    getInbox,
    getMyState,
    listIssues,
    listLegionSettings,
    listProjectArtifacts,
    listProjects,
    restore,
    view,
    whoAmI,
  };
}

const coreSource: ArchitectureSource = {
  branch: "main",
  components: 0,
  created_at: "2026-09-10T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  enabled: true,
  last_commit: null,
  last_error: null,
  last_sync_at: null,
  project: "CORE",
  repo: "legion/legion",
};

test("the bare project path opens on Architecture when the project has sources, forwarding nothing, from one read", async () => {
  const user = userEvent.setup();
  const page = renderPage(
    "/projects/CORE",
    undefined,
    "alice",
    [],
    [coreSource, { ...coreSource, repo: "acme/infra" }]
  );
  try {
    await screen.findByRole("heading", { name: "Core" });
    expect(screen.getByTestId("location").textContent).toBe("/projects/CORE/architecture");
    expect(screen.getByRole("tab", { name: "Architecture" })).toHaveAttribute(
      "aria-selected",
      "true"
    );
    expect(screen.getByRole("tab", { name: "Issues" })).toBeTruthy();
    // The List / Board control belongs to Issues only.
    expect(screen.queryByRole("button", { name: "Board" })).not.toBeInTheDocument();
    // `v` is inert here.
    await user.keyboard("v");
    expect(window.localStorage.getItem("dispatch.project.issue-view:alice")).toBeNull();
    // One read of the project's source list decides it; the Architecture pane reads the tree.
    expect(page.listProjectArchitectureSources).toHaveBeenCalledTimes(1);
    expect(page.listProjectArchitectureSources).toHaveBeenCalledWith("CORE");
  } finally {
    page.restore();
  }
});

test("the bare project path opens on Issues with the filter parameters forwarded when there is no source, and the tab is absent", async () => {
  const page = renderPage("/projects/CORE?label=frontend&q=core");
  try {
    await screen.findByRole("heading", { name: "Core" });
    await waitFor(() =>
      expect(screen.getByTestId("location").textContent).toBe(
        "/projects/CORE/issues?label=frontend&q=core"
      )
    );
    expect(screen.queryByRole("tab", { name: "Architecture" })).not.toBeInTheDocument();
    expect(screen.getByRole("tab", { name: "Issues" })).toHaveAttribute("aria-selected", "true");
    expect(await screen.findByRole("searchbox", { name: "Search issues" })).toHaveValue("core");
  } finally {
    page.restore();
  }
});

test("any other failure of the source lookup is shown, never silently turned into Issues", async () => {
  const page = renderPage("/projects/CORE", undefined, "alice", [], new ApiError(500, {}));
  try {
    expect(await screen.findByRole("alert")).toHaveProperty(
      "textContent",
      expect.stringContaining("Could not load this project's architecture sources.")
    );
    expect(screen.getByTestId("location").textContent).toBe("/projects/CORE");
    expect(screen.queryByRole("tab", { name: "Issues" })).not.toBeInTheDocument();
  } finally {
    page.restore();
  }
});

test("/architecture renders nothing while the source lookup is pending, like the bare path, so the tab strip never shows without its active tab", async () => {
  const page = renderPage("/projects/CORE/architecture", undefined, "alice", [], "pending");
  try {
    // Past the projects query (the `Loading project…` placeholder), with the source lookup
    // still open: no header, no tabs. Scoped to this render: bun runs every test file in one
    // document, and a sibling's leftover placeholder would never go away.
    await waitForElementToBeRemoved(() =>
      within(page.view.container).queryByText("Loading project…")
    );
    expect(page.listProjectArchitectureSources).toHaveBeenCalledTimes(1);
    expect(page.view.container.innerHTML).toBe(
      '<output data-testid="location">/projects/CORE/architecture</output>'
    );
  } finally {
    page.restore();
  }
});

test("renders header, tabs, and the Issues panel; /documents selects Documents", async () => {
  const user = userEvent.setup();
  const page = renderPage("/projects/CORE");

  try {
    await screen.findByRole("heading", { name: "Core" });
    expect(screen.getByText("CORE", { exact: true })).toBeTruthy();
    expect(screen.getByRole("tab", { name: "Issues" })).toHaveAttribute("aria-selected", "true");
    expect(screen.getByRole("tabpanel", { name: "Issues" })).toBeTruthy();

    await user.click(screen.getByRole("tab", { name: "Documents" }));
    expect(screen.getByRole("tab", { name: "Documents" })).toHaveAttribute("aria-selected", "true");
    expect(screen.getByRole("tabpanel", { name: "Documents" })).toBeTruthy();
  } finally {
    page.restore();
  }

  const documents = renderPage("/projects/CORE/documents");
  try {
    await screen.findByRole("heading", { name: "Core" });
    expect(screen.getByRole("tab", { name: "Documents" })).toHaveAttribute("aria-selected", "true");
  } finally {
    documents.restore();
  }
});

test("shows a compact blocker link only when asks await the viewer", async () => {
  const waiting = renderPage("/projects/CORE", undefined, "alice", [inboxRow()]);
  try {
    await screen.findByRole("heading", { name: "Core" });
    expect(screen.getByRole("link", { name: "Blocked on you · 1" })).toHaveAttribute("href", "/");
    expect(screen.queryByText(/oldest/)).not.toBeInTheDocument();
  } finally {
    waiting.restore();
  }

  const clear = renderPage("/projects/CORE", undefined, "alice", [
    inboxRow({
      last_reply: { author: { id: "alice", kind: "user" }, created_at: "2026-09-11T01:00:00Z" },

      waiting_on: "agent",
    }),
  ]);
  try {
    await screen.findByRole("heading", { name: "Core" });
    expect(screen.queryByRole("link", { name: /Blocked on you/ })).not.toBeInTheDocument();
  } finally {
    clear.restore();
  }
});

test("unknown project shows the not-found view", async () => {
  const page = renderPage("/projects/MISSING", []);

  try {
    expect(await screen.findByRole("heading", { name: "Page not found" })).toBeTruthy();
  } finally {
    page.restore();
  }
});

test("an explicit layout URL overrides the reader's saved choice", async () => {
  const user = userEvent.setup();
  const legacyKey = "dispatch.project.issue-view:alice";
  window.localStorage.setItem(legacyKey, "board");
  const page = renderPage("/projects/CORE?layout=list&label=frontend", undefined, "alice");
  try {
    await screen.findByRole("heading", { name: "Core" });
    expect(screen.getByRole("button", { name: "List" })).toHaveAttribute("aria-pressed", "true");

    await user.click(screen.getByRole("button", { name: "Board" }));
    expect(screen.getByRole("button", { name: "Board" })).toHaveAttribute("aria-pressed", "true");
    expect(await screen.findByRole("region", { name: "Project board" })).toBeTruthy();
    expect(screen.getByTestId("location").textContent).toContain("layout=board");
    expect(screen.getByTestId("location").textContent).toContain("label=frontend");

    await user.click(screen.getByRole("button", { name: "List" }));
    expect(screen.getByRole("button", { name: "List" })).toHaveAttribute("aria-pressed", "true");
    expect(screen.getByTestId("location").textContent).not.toContain("layout=");
  } finally {
    page.restore();
    window.localStorage.removeItem(legacyKey);
  }
});

test("choosing Board persists for a later bare project URL", async () => {
  const user = userEvent.setup();
  const preference = "dispatch.project.issue-view:alice";
  const first = renderPage("/projects/CORE", undefined, "alice");
  try {
    await screen.findByRole("heading", { name: "Core" });
    await user.click(screen.getByRole("button", { name: "Board" }));
    expect(window.localStorage.getItem(preference)).toBe("board");
  } finally {
    first.restore();
  }

  const second = renderPage("/projects/CORE", undefined, "alice");
  try {
    await screen.findByRole("heading", { name: "Core" });
    expect(screen.getByRole("button", { name: "Board" })).toHaveAttribute("aria-pressed", "true");
  } finally {
    second.restore();
    window.localStorage.removeItem(preference);
  }
});

test("v toggles the URL layout on Issues and is inert on Documents", async () => {
  const user = userEvent.setup();
  const page = renderPage("/projects/CORE");
  try {
    await screen.findByRole("heading", { name: "Core" });
    await user.keyboard("v");
    expect(screen.getByRole("button", { name: "Board" })).toHaveAttribute("aria-pressed", "true");
    expect(screen.getByTestId("location").textContent).toContain("layout=board");
    await user.keyboard("v");
    expect(screen.getByRole("button", { name: "List" })).toHaveAttribute("aria-pressed", "true");
    expect(screen.getByTestId("location").textContent).not.toContain("layout=");
  } finally {
    page.restore();
  }

  const documents = renderPage("/projects/CORE/documents");
  try {
    await screen.findByRole("heading", { name: "Core" });
    await user.keyboard("v");
    expect(screen.queryByRole("button", { name: "Board" })).not.toBeInTheDocument();
    expect(screen.getByTestId("location").textContent).toBe("/projects/CORE/documents");
  } finally {
    documents.restore();
  }
});

test("Board-only edge and lane controls persist for a signed-in reader", async () => {
  const user = userEvent.setup();
  const edgesKey = "dispatch.project.board-edges:alice";
  const lanesKey = "dispatch.project.board-lanes:alice";
  const first = renderPage("/projects/CORE?layout=board", undefined, "alice");
  try {
    await screen.findByRole("heading", { name: "Core" });
    await user.click(screen.getByRole("button", { name: "Show Icebox & Done" }));
    await user.click(screen.getByRole("button", { name: "Show lanes" }));
    expect(window.localStorage.getItem(edgesKey)).toBe("shown");
    expect(window.localStorage.getItem(lanesKey)).toBe("shown");
    await user.click(screen.getByRole("button", { name: "List" }));
    expect(screen.queryByRole("button", { name: /Icebox & Done/ })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /lanes/i })).not.toBeInTheDocument();
  } finally {
    first.restore();
  }

  const second = renderPage("/projects/CORE?layout=board", undefined, "alice");
  try {
    await screen.findByRole("heading", { name: "Core" });
    expect(screen.getByRole("button", { name: "Hide Icebox & Done" })).toHaveAttribute(
      "aria-pressed",
      "true"
    );
    expect(screen.getByRole("button", { name: "Hide lanes" })).toHaveAttribute(
      "aria-pressed",
      "true"
    );
  } finally {
    second.restore();
    window.localStorage.removeItem(edgesKey);
    window.localStorage.removeItem(lanesKey);
  }
});

test("a Board URL keeps the selected view's controls in its shared address", async () => {
  const page = renderPage(
    "/projects/CORE?view=triage&layout=board&status=triage&label=frontend&q=core"
  );
  try {
    await screen.findByRole("region", { name: "Project board" });
    expect(screen.getByTestId("location").textContent).toContain("view=triage");
    expect(screen.getByTestId("location").textContent).toContain("status=triage");
    expect(screen.getByTestId("location").textContent).toContain("label=frontend");
    expect(screen.getByTestId("location").textContent).toContain("q=core");
  } finally {
    page.restore();
  }
});

test("a Legion project keeps its Evals tab and uses the same URL Board switch", async () => {
  const user = userEvent.setup();
  const sockets = installFakeLiveSockets();
  const page = renderPage("/projects/CORE?layout=board", undefined, "alice", [], [], ["CORE"]);
  try {
    await screen.findByRole("heading", { name: "Core" });
    expect(screen.getByRole("tab", { name: "Evals" })).toBeVisible();
    expect(screen.getByRole("button", { name: "Board" })).toHaveAttribute("aria-pressed", "true");
    await user.keyboard("v");
    expect(screen.getByRole("button", { name: "List" })).toHaveAttribute("aria-pressed", "true");
    expect(screen.getByTestId("location").textContent).not.toContain("layout=");
  } finally {
    page.restore();
    sockets.restore();
  }
});

test("the former Legion route redirects to Evals and keeps its query", async () => {
  const page = renderPage("/projects/CORE/legion?x=1", undefined, "alice", [], [], ["CORE"]);
  try {
    await screen.findByRole("heading", { name: "Core" });
    await waitFor(() =>
      expect(screen.getByTestId("location").textContent).toBe("/projects/CORE/evals?x=1")
    );
    expect(screen.getByRole("tab", { name: "Evals" })).toHaveAttribute("aria-selected", "true");
  } finally {
    page.restore();
  }
});

test("the Evals tab remains active for a direct route", async () => {
  const page = renderPage("/projects/CORE/evals", undefined, "alice", [], [], ["CORE"]);
  try {
    await screen.findByRole("heading", { name: "Core" });
    expect(screen.getByRole("tab", { name: "Evals" })).toHaveAttribute("aria-selected", "true");
  } finally {
    page.restore();
  }
});
