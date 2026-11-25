import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import {
  act,
  render,
  screen,
  waitFor,
  waitForElementToBeRemoved,
  within,
} from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http } from "msw";
import type { ReactNode } from "react";
import { MemoryRouter, useLocation, useNavigate } from "react-router-dom";

import { answerRequests } from "../../__tests__/network";
import { answerIssuePatches } from "../../__tests__/send-answers";
import { ApiError, api } from "../../api/client";
import { projectArchitectureSourcesQuery } from "../../api/queries";
import type { ArchitectureSource, ArchitectureTree, InboxRow, IssueSummary } from "../../api/types";
import { withWaitingOnPerson } from "../inbox/inbox-fixture";
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

/** The reader going back: the browser's Back button, or anything that calls `navigate(-1)`. */
function GoBack(): ReactNode {
  const navigate = useNavigate();
  return (
    <button onClick={() => navigate(-1)} type="button">
      Go back
    </button>
  );
}

/** One open ask. The person it waits on is resolved as the server resolves it
 *  (`coalesce(a.asked_of, i.assignee, p.default_assignee)`, through `withWaitingOnPerson`), so
 *  an override that moves the issue's assignee, or hands the ask to somebody, moves that person
 *  with it. */
function inboxRow(overrides: Partial<InboxRow> = {}): InboxRow {
  const row: InboxRow = {
    anchor: null,
    answer: null,
    artifact_id: null,
    asked_of: null,
    asked_of_at: null,
    asked_of_by: null,
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
  return withWaitingOnPerson(row);
}

/** `path` is the address the page opens at, or the reader's history, oldest first, ending at that
 *  address: a page with history behind it draws a Go back button beside it. `sources` is what
 *  `GET /projects/CORE/architecture-sources` answers: the project's sources (a non-empty list
 *  opens it on Architecture; `[]`, the default, on Issues, as every render here expects unless it
 *  says otherwise), an `ApiError` for a failure, or `"pending"` for a lookup that never
 *  settles. `architecture` is what `GET /projects/{key}/architecture` answers for every project;
 *  left out, that read never settles. */
function renderPage(
  path: string | readonly string[],
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
  architecture?: ArchitectureTree
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
  // The Architecture pane's own tree fetch is not under test here unless the caller gives a tree;
  // else it never settles.
  const getArchitecture = spyOn(api, "getArchitecture");
  if (architecture === undefined) {
    getArchitecture.mockImplementation(() => Promise.withResolvers<never>().promise);
  } else {
    getArchitecture.mockResolvedValue(architecture);
  }
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const entries = typeof path === "string" ? [path] : [...path];
  const view = render(
    <MemoryRouter initialEntries={entries} initialIndex={entries.length - 1}>
      <QueryClientProvider client={queryClient}>
        <KeymapProvider>
          <ProjectPage />
          <LocationProbe />
          {entries.length > 1 ? <GoBack /> : null}
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
    listProjectArtifacts.mockRestore();
    listProjects.mockRestore();
    whoAmI.mockRestore();
  };
  return {
    getArchitecture,
    listProjectArchitectureSources,
    getInbox,
    getMyState,
    listIssues,
    listProjectArtifacts,
    listProjects,
    queryClient,
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

// Who may see a project is its own tab, beside Issues and Documents, at /access: the tab reads the
// project's access and the groups its picker offers, and is the owner's to change.
test("the Access tab, at /access, shows the project's owner and the people and groups its list names", async () => {
  const user = userEvent.setup();
  const getProjectAccess = spyOn(api, "getProjectAccess").mockResolvedValue({
    groups: ["crew"],
    owner: "alice",
    people: ["carol@example.com"],
    project: "CORE",
    not_signed_in: [],
    slack_channels: [],
    slack_channels_changed_at: null,
    visibility: "public",
  });
  const listAccessGroups = spyOn(api, "listAccessGroups").mockResolvedValue([
    { members: [], name: "crew", not_signed_in: [], owner: "alice" },
  ]);
  const page = renderPage("/projects/CORE/issues");
  try {
    await screen.findByRole("heading", { name: "Core" });
    expect(screen.queryByRole("tabpanel", { name: "Access" })).not.toBeInTheDocument();
    expect(getProjectAccess).not.toHaveBeenCalled();

    await user.click(screen.getByRole("tab", { name: "Access" }));
    expect(screen.getByTestId("location").textContent).toBe("/projects/CORE/access");
    expect(screen.getByRole("tab", { name: "Access" })).toHaveAttribute("aria-selected", "true");
    const panel = screen.getByRole("tabpanel", { name: "Access" });
    expect(await within(panel).findByTestId("access-owner")).toHaveTextContent("alice (you)");
    expect(within(panel).getByRole("list", { name: "People" })).toHaveTextContent(
      "carol@example.com"
    );
    expect(getProjectAccess).toHaveBeenCalledWith("CORE");
    // The List / Board control belongs to Issues only.
    expect(screen.queryByRole("button", { name: "Board" })).not.toBeInTheDocument();
  } finally {
    page.restore();
    getProjectAccess.mockRestore();
    listAccessGroups.mockRestore();
  }
});

// A draft typed on one project's Access tab belongs to that project: going to another project's
// Access tab draws its own stored values, and its Save sends none of the first project's draft.
// The panel is keyed by the project, as the issue list and the board are. Control: unkeyed, the
// panel keeps ALPHA's channel draft on BETA and Save writes it there.
test("a draft on one project's Access tab is not saved to the project the reader goes to next", async () => {
  const user = userEvent.setup();
  const stored = { ALPHA: ["C0ALPHA0001"], BETA: ["C0BETA00001"] } as Record<string, string[]>;
  const getProjectAccess = spyOn(api, "getProjectAccess").mockImplementation(async (key) => ({
    groups: [],
    owner: "alice",
    people: [],
    project: key,
    not_signed_in: [],
    slack_channels: stored[key] ?? [],
    slack_channels_changed_at: null,
    visibility: "public",
  }));
  const listAccessGroups = spyOn(api, "listAccessGroups").mockResolvedValue([]);
  const patchProjectAccess = spyOn(api, "patchProjectAccess").mockImplementation(
    async (key, body) => ({
      groups: [],
      owner: "alice",
      people: [],
      project: key,
      not_signed_in: [],
      slack_channels: body.slack_channels ?? stored[key] ?? [],
      slack_channels_changed_at: null,
      visibility: "public",
    })
  );
  const project = (key: string, name: string) => ({
    created_at: "2026-09-10T00:00:00Z",
    key,
    name,
    open_asks: 0,
    gated_steps: [],
  });
  const page = renderPage(
    ["/projects/BETA/access", "/projects/ALPHA/access"],
    [project("ALPHA", "Alpha"), project("BETA", "Beta")]
  );
  try {
    const alpha = await screen.findByLabelText<HTMLInputElement>("Channel ids");
    await waitFor(() => expect(alpha.value).toBe("C0ALPHA0001"));
    await user.clear(alpha);
    await user.type(alpha, "C0ALPHADRAFT");

    await user.click(screen.getByRole("button", { name: "Go back" }));
    await screen.findByRole("heading", { name: "Beta" });
    const beta = await screen.findByLabelText<HTMLInputElement>("Channel ids");
    await waitFor(() => expect(beta.value).toBe("C0BETA00001"));
    await user.click(screen.getByRole("button", { name: "Save channels" }));
    await waitFor(() => expect(patchProjectAccess).toHaveBeenCalled());
    expect(patchProjectAccess).not.toHaveBeenCalledWith(
      "BETA",
      { slack_channels: ["C0ALPHADRAFT"] },
      expect.anything()
    );
    expect(patchProjectAccess).toHaveBeenLastCalledWith(
      "BETA",
      { slack_channels: ["C0BETA00001"] },
      expect.anything()
    );
  } finally {
    page.restore();
    getProjectAccess.mockRestore();
    listAccessGroups.mockRestore();
    patchProjectAccess.mockRestore();
  }
});

// A Refresh belongs to the project it was pressed on: going to another project's Architecture
// tab while ALPHA's Refresh is still out draws BETA's same-named source ready to refresh, and
// ALPHA's Refresh settling reloads ALPHA's architecture, not BETA's. The pane is keyed by the
// project, as the issue list and the board are. Control: unkeyed, BETA's source says
// "Refreshing…" and the settle reads BETA's architecture again.
test("a Refresh still out on one project's Architecture tab is not the next project's", async () => {
  const user = userEvent.setup();
  const tree: ArchitectureTree = {
    components: [],
    not_architectural: [],
    retired_links: [],
    sources: [
      {
        branch: "main",
        last_commit: null,
        last_error: null,
        last_sync_at: null,
        repo: "legion/legion",
      },
    ],
    totals: {
      components_without_work: 0,
      issues_done: 0,
      issues_total: 0,
      not_architectural: 0,
      retired_links: 0,
      unassigned: 0,
    },
    unassigned: [],
  };
  const project = (key: string, name: string) => ({
    created_at: "2026-09-10T00:00:00Z",
    key,
    name,
    open_asks: 0,
    gated_steps: [],
  });
  let settle: (() => void) | undefined;
  const syncArchitectureSource = spyOn(api, "syncArchitectureSource").mockImplementation(() => {
    const { promise, resolve } = Promise.withResolvers<never>();
    settle = () => resolve(undefined as never);
    return promise;
  });
  const page = renderPage(
    ["/projects/BETA/architecture", "/projects/ALPHA/architecture"],
    [project("ALPHA", "Alpha"), project("BETA", "Beta")],
    "alice",
    [],
    [coreSource],
    tree
  );
  const readsOf = (key: string) =>
    page.getArchitecture.mock.calls.filter(([asked]) => asked === key).length;
  try {
    await user.click(await screen.findByRole("button", { name: "Refresh legion/legion" }));
    await screen.findByRole("button", { name: "Refreshing legion/legion…" });
    expect(syncArchitectureSource).toHaveBeenCalledWith(
      "ALPHA",
      "legion/legion",
      expect.anything()
    );

    // BETA's sources are held as a visit to BETA earlier leaves them, so the tab strip and the
    // pane stay drawn across the move rather than wait on BETA's read.
    page.queryClient.setQueryData(projectArchitectureSourcesQuery("BETA").queryKey, [coreSource]);
    await user.click(screen.getByRole("button", { name: "Go back" }));
    await screen.findByRole("heading", { name: "Beta" });
    await screen.findByTestId("architecture-source");
    expect(screen.queryByRole("button", { name: "Refreshing legion/legion…" })).toBeNull();
    expect(screen.getByRole("button", { name: "Refresh legion/legion" })).not.toBeDisabled();
    const betaReads = readsOf("BETA");

    // ALPHA's Refresh settles; a pane that took it as BETA's would read BETA's architecture again.
    await act(async () => {
      settle?.();
      await new Promise((resolve) => setTimeout(resolve, 200));
    });
    expect(readsOf("BETA")).toBe(betaReads);
  } finally {
    page.restore();
    syncArchitectureSource.mockRestore();
  }
});

test("keeps the issue view preference separate for each signed-in user", async () => {
  const aliceStorageKey = "dispatch.project.issue-view:alice";
  const bobStorageKey = "dispatch.project.issue-view:bob";
  window.localStorage.setItem(aliceStorageKey, "board");
  window.localStorage.setItem(bobStorageKey, "list");

  const alice = renderPage("/projects/CORE", undefined, "alice");
  try {
    await screen.findByRole("heading", { name: "Core" });
    expect(screen.getByRole("button", { name: "Board" })).toHaveAttribute("aria-pressed", "true");
  } finally {
    alice.restore();
  }

  const bob = renderPage("/projects/CORE", undefined, "bob");
  try {
    await screen.findByRole("heading", { name: "Core" });
    expect(screen.getByRole("button", { name: "List" })).toHaveAttribute("aria-pressed", "true");
  } finally {
    bob.restore();
    window.localStorage.removeItem(aliceStorageKey);
    window.localStorage.removeItem(bobStorageKey);
  }
});

test("the Icebox & Done toggle lives in Board view only and persists per signed-in user", async () => {
  const user = userEvent.setup();
  const viewKey = "dispatch.project.issue-view:alice";
  const edgesKey = "dispatch.project.board-edges:alice";
  window.localStorage.setItem(viewKey, "board");

  const first = renderPage("/projects/CORE", undefined, "alice");
  try {
    await screen.findByRole("heading", { name: "Core" });
    const toggle = await screen.findByRole("button", { name: "Show Icebox & Done" });
    expect(toggle).toHaveAttribute("aria-pressed", "false");
    expect(toggle).toHaveClass("min-h-11");
    await user.click(toggle);
    expect(screen.getByRole("button", { name: "Hide Icebox & Done" })).toHaveAttribute(
      "aria-pressed",
      "true"
    );
    expect(window.localStorage.getItem(edgesKey)).toBe("shown");

    await user.click(screen.getByRole("button", { name: "List" }));
    expect(screen.queryByRole("button", { name: /Icebox & Done/ })).not.toBeInTheDocument();
  } finally {
    first.restore();
  }

  window.localStorage.setItem(viewKey, "board");
  const second = renderPage("/projects/CORE", undefined, "alice");
  try {
    await screen.findByRole("heading", { name: "Core" });
    expect(await screen.findByRole("button", { name: "Hide Icebox & Done" })).toHaveAttribute(
      "aria-pressed",
      "true"
    );
  } finally {
    second.restore();
  }

  const bob = renderPage("/projects/CORE", undefined, "bob");
  try {
    window.localStorage.setItem("dispatch.project.issue-view:bob", "board");
    await screen.findByRole("heading", { name: "Core" });
    await user.click(screen.getByRole("button", { name: "Board" }));
    expect(await screen.findByRole("button", { name: "Show Icebox & Done" })).toHaveAttribute(
      "aria-pressed",
      "false"
    );
  } finally {
    bob.restore();
    window.localStorage.removeItem(viewKey);
    window.localStorage.removeItem(edgesKey);
    window.localStorage.removeItem("dispatch.project.issue-view:bob");
    window.localStorage.removeItem("dispatch.project.board-edges:bob");
  }
});

test("v toggles List and Board on the issues tab and writes the preference; it is inert on Documents", async () => {
  const user = userEvent.setup();
  const viewKey = "dispatch.project.issue-view:alice";
  const page = renderPage("/projects/CORE");
  try {
    await screen.findByRole("heading", { name: "Core" });
    expect(screen.getByRole("button", { name: "List" })).toHaveAttribute("aria-pressed", "true");

    await user.keyboard("v");
    expect(screen.getByRole("button", { name: "Board" })).toHaveAttribute("aria-pressed", "true");
    expect(window.localStorage.getItem(viewKey)).toBe("board");

    await user.keyboard("v");
    expect(screen.getByRole("button", { name: "List" })).toHaveAttribute("aria-pressed", "true");
    expect(window.localStorage.getItem(viewKey)).toBe("list");
  } finally {
    page.restore();
    window.localStorage.removeItem(viewKey);
  }

  const documents = renderPage("/projects/CORE/documents");
  try {
    await screen.findByRole("heading", { name: "Core" });
    await user.keyboard("v");
    expect(screen.queryByRole("button", { name: "Board" })).not.toBeInTheDocument();
    expect(window.localStorage.getItem(viewKey)).toBeNull();
  } finally {
    documents.restore();
  }
});

test("keeps an unpersisted List or Board choice while identity is pending", async () => {
  const user = userEvent.setup();
  const pendingKey = "dispatch.project.issue-view:undefined";
  window.localStorage.removeItem(pendingKey);
  const page = renderPage("/projects/CORE", undefined, null);

  try {
    await screen.findByRole("heading", { name: "Core" });
    await user.click(screen.getByRole("button", { name: "Board" }));
    expect(screen.getByRole("button", { name: "Board" })).toHaveAttribute("aria-pressed", "true");
    expect(window.localStorage.getItem(pendingKey)).toBeNull();
  } finally {
    page.restore();
    window.localStorage.removeItem(pendingKey);
  }
});

test("List table and Board strip share the URL's status, labels and search", async () => {
  const user = userEvent.setup();
  const viewKey = "dispatch.project.issue-view:alice";
  const page = renderPage("/projects/CORE?label=frontend&q=core&status=todo");
  try {
    await screen.findByRole("table", { name: "Project issues table" });
    expect(screen.getByRole("button", { name: "Filter Status: 1 picked" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Filter Labels: 1 picked" })).toBeTruthy();
    expect(screen.getByRole("searchbox", { name: "Search issues" })).toHaveValue("core");
    await user.click(screen.getByRole("button", { name: "Board" }));
    expect(await screen.findByRole("button", { name: "Filters · 2 active" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Remove Label: frontend filter" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Remove Search: core filter" })).toBeTruthy();
    await user.click(screen.getByRole("button", { name: "List" }));
    expect(await screen.findByRole("button", { name: "Filter Status: 1 picked" })).toBeTruthy();
    expect(screen.getByTestId("location").textContent).toContain("status=todo");
  } finally {
    page.restore();
    window.localStorage.removeItem(viewKey);
    window.localStorage.removeItem("dispatch.project.issue-filters:alice");
  }
});

test("typing v in the strip's search input never toggles the view", async () => {
  const user = userEvent.setup();
  const viewKey = "dispatch.project.issue-view:alice";
  const page = renderPage("/projects/CORE?label=frontend");
  try {
    await screen.findByRole("heading", { name: "Core" });
    const search = await screen.findByRole("searchbox", { name: "Search issues" });
    // The key reaches the field as a typed character, which is what a reader pressing it does.
    await user.click(search);
    await user.keyboard("v");
    expect(screen.getByRole("button", { name: "List" })).toHaveAttribute("aria-pressed", "true");
    expect(window.localStorage.getItem(viewKey)).toBeNull();
    // Outside an editable target the same key still toggles.
    await user.click(document.body);
    await user.keyboard("v");
    expect(screen.getByRole("button", { name: "Board" })).toHaveAttribute("aria-pressed", "true");
  } finally {
    page.restore();
    window.localStorage.removeItem(viewKey);
  }
});

test("the Lanes toggle lives in Board view only and persists per signed-in user", async () => {
  const user = userEvent.setup();
  const viewKey = "dispatch.project.issue-view:alice";
  const lanesKey = "dispatch.project.board-lanes:alice";
  window.localStorage.setItem(viewKey, "board");

  const first = renderPage("/projects/CORE", undefined, "alice");
  try {
    await screen.findByRole("heading", { name: "Core" });
    const toggle = await screen.findByRole("button", { name: "Show lanes" });
    expect(toggle).toHaveAttribute("aria-pressed", "false");
    expect(toggle).toHaveClass("min-h-11");
    await user.click(toggle);
    expect(screen.getByRole("button", { name: "Hide lanes" })).toHaveAttribute(
      "aria-pressed",
      "true"
    );
    expect(window.localStorage.getItem(lanesKey)).toBe("shown");

    await user.click(screen.getByRole("button", { name: "List" }));
    expect(screen.queryByRole("button", { name: /lanes/i })).not.toBeInTheDocument();
  } finally {
    first.restore();
  }

  window.localStorage.setItem(viewKey, "board");
  const second = renderPage("/projects/CORE", undefined, "alice");
  try {
    await screen.findByRole("heading", { name: "Core" });
    expect(await screen.findByRole("button", { name: "Hide lanes" })).toHaveAttribute(
      "aria-pressed",
      "true"
    );
  } finally {
    second.restore();
  }

  const bob = renderPage("/projects/CORE", undefined, "bob");
  try {
    window.localStorage.setItem("dispatch.project.issue-view:bob", "board");
    await screen.findByRole("heading", { name: "Core" });
    await user.click(screen.getByRole("button", { name: "Board" }));
    expect(await screen.findByRole("button", { name: "Show lanes" })).toHaveAttribute(
      "aria-pressed",
      "false"
    );
  } finally {
    bob.restore();
    window.localStorage.removeItem(viewKey);
    window.localStorage.removeItem(lanesKey);
    window.localStorage.removeItem("dispatch.project.issue-view:bob");
    window.localStorage.removeItem("dispatch.project.board-lanes:bob");
  }
});

/** An issue in Todo, as a project's issue list answers it. */
function todo(key: string, title: string): IssueSummary {
  return {
    advances: [],
    advances_revision: 0,
    assignee: null,
    blocked_by: [],
    claim: null,
    closed_at: null,
    components: { mode: "inherit", ids: [], unknown: [], reason: null, inherited_from: null },
    created_at: "2026-09-13T00:00:00Z",
    created_by: null,
    key,
    labels: [],
    last_seq: 1,
    open_asks: 0,
    parent: null,
    primary_artifact_id: "artifact-1",
    priority: null,
    progress: { tasks: null, children: null },
    rank: "a",
    repo: null,
    resolved_repo: null,
    route: null,
    route_holder: null,
    route_status: null,
    route_status_reason: null,
    status: "todo",
    title,
    updated_at: "2026-09-13T00:00:00Z",
  };
}

// Back from one project's board to another's draws the same page for the other project, and a
// move belongs to the board it was made on: its refusal names that project's issue. The other
// project's board is drawn with none of it.
test("a move refused on one project's board leaves no refusal above the board the reader goes back to", async () => {
  const user = userEvent.setup();
  const viewKey = "dispatch.project.issue-view:alice";
  window.localStorage.setItem(viewKey, "board");
  const page = renderPage(
    ["/projects/OTHER/issues", "/projects/CORE/issues"],
    [
      {
        created_at: "2026-09-10T00:00:00Z",
        key: "CORE",
        name: "Core",
        open_asks: 0,
        gated_steps: [],
      },
      {
        created_at: "2026-09-10T00:00:00Z",
        key: "OTHER",
        name: "Other",
        open_asks: 0,
        gated_steps: [],
      },
    ]
  );
  page.listIssues.mockImplementation(async (options = {}) =>
    options.project === "OTHER" ? [todo("OTHER-1", "Bravo")] : [todo("CORE-1", "Alpha")]
  );
  const patchIssue = answerIssuePatches().mockRejectedValue(
    new ApiError(409, { code: "CONFLICT", error: "CORE-1 is blocked by CORE-2" })
  );

  try {
    const alpha = await screen.findByRole("article", { name: "CORE-1 Alpha" });
    act(() => alpha.focus());
    await user.keyboard("{Shift>}L{/Shift}");
    expect(await screen.findByRole("alert")).toHaveTextContent("CORE-1 is blocked by CORE-2");

    await user.click(screen.getByRole("button", { name: "Go back" }));
    await screen.findByRole("article", { name: "OTHER-1 Bravo" });
    expect(screen.queryByRole("alert")).toBeNull();
  } finally {
    patchIssue.mockRestore();
    page.restore();
    window.localStorage.removeItem(viewKey);
  }
});
