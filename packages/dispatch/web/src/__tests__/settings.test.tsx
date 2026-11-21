import { expect, spyOn, test } from "bun:test";
import { PROJECT_NAME_MAX } from "@legion/contracts";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
// biome-ignore lint/style/noRestrictedImports: the focus site below reaches the picker's people list without a press, which user-event has no form for
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import type { UserEvent } from "@testing-library/user-event";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

import { ApiError, api } from "../api/client";
import type { CreatedProject, Project, ProjectDefaultAssignee, RepoProject } from "../api/types";
import { App } from "../app";
// The Settings page is a lazy route of the app. Its module's first compile is not what a wait is
// for, so the module is loaded here, with the test file, and no wait measures that load.
import "../features/settings/SettingsPage";
import { keyedAttempt } from "./keyed-attempt";
import { panelOf, pickNamed } from "./list-picks";

/** The mapping routes' server state as the Settings page reaches it: a read answers what the writes
 *  have left, and a write follows the server's default rules (the body's default, else the
 *  mapping's own while its project is unchanged; at most one default per project). */
function mappingServer(initial: RepoProject[]) {
  let rows = initial.map((row) => ({ ...row }));
  // `default` as the route table carries it: absent, or null for "leave it as it is".
  const put = (repo: string, input: { project: string; default?: boolean | null }): RepoProject => {
    const existing = rows.find((row) => row.repo === repo);
    const mapping: RepoProject = {
      created_at: "2026-09-10T00:00:00Z",
      created_by: { id: "alice", kind: "user" },
      default: input.default ?? (existing?.project === input.project ? existing.default : false),
      project: input.project,
      // The write names no rename, as the page's never does, so the stored one stands.
      renamed_to: existing?.renamed_to ?? null,
      repo,
    };
    rows = [
      ...rows
        .filter((row) => row.repo !== repo)
        .map((row) =>
          mapping.default && row.project === mapping.project ? { ...row, default: false } : row
        ),
      mapping,
    ];
    return { ...mapping };
  };
  return {
    list: async (): Promise<RepoProject[]> => rows.map((row) => ({ ...row })),
    put,
    remove: async (repo: string): Promise<void> => {
      rows = rows.filter((row) => row.repo !== repo);
    },
  };
}

function stubMatchMedia(): () => void {
  const originalMatchMedia = window.matchMedia;
  window.matchMedia = (() =>
    ({
      addEventListener: () => {},
      addListener: () => {},
      dispatchEvent: () => true,
      matches: false,
      media: "",
      onchange: null,
      removeEventListener: () => {},
      removeListener: () => {},
    }) as MediaQueryList) as typeof window.matchMedia;
  return () => {
    window.matchMedia = originalMatchMedia;
  };
}

test("human users manage repository project mappings from the settings route", async () => {
  const user = userEvent.setup();
  const restoreMatchMedia = stubMatchMedia();
  const mapping: RepoProject = {
    created_at: "2026-09-10T00:00:00Z",
    created_by: { id: "alice", kind: "user" },
    default: false,
    project: "CORE",
    renamed_to: null,
    repo: "owner/repo",
  };
  const other: RepoProject = { ...mapping, default: true, repo: "owner/other" };
  const server = mappingServer([mapping, other]);
  const whoAmI = spyOn(api, "whoAmI").mockResolvedValue({
    event_head: "0",
    kind: "user",
    login: "alice",
  });
  const listIssues = spyOn(api, "listIssues").mockResolvedValue([]);
  const getMyState = spyOn(api, "getMyState").mockResolvedValue({});
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([]);
  const listRepoProjects = spyOn(api, "listRepoProjects").mockImplementation(server.list);
  const listAgentTokens = spyOn(api, "listAgentTokens").mockResolvedValue([]);
  const getCredentialPending = spyOn(api, "getCredentialPending").mockResolvedValue(null);
  const listArchitectureSources = spyOn(api, "listArchitectureSources").mockResolvedValue([]);
  const getLegionDaemon = spyOn(api, "getLegionDaemon").mockResolvedValue(null);
  const listDefaults = spyOn(api, "listProjectDefaultAssignees").mockResolvedValue([]);
  const getDeliverySettings = spyOn(api, "getDeliverySettings").mockResolvedValue(null);
  const listProjects = spyOn(api, "listProjects").mockResolvedValue([
    {
      created_at: "2026-09-01T00:00:00Z",
      key: "CORE",
      name: "Core",
      open_asks: 0,
      gated_steps: [],
    },
  ]);
  const putRepoProject = spyOn(api, "putRepoProject").mockImplementation(async (repo, input) =>
    server.put(repo, input)
  );
  const deleteRepoProject = spyOn(api, "deleteRepoProject").mockImplementation(server.remove);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });

  try {
    render(
      <MemoryRouter initialEntries={["/settings"]}>
        <QueryClientProvider client={queryClient}>
          <App />
        </QueryClientProvider>
      </MemoryRouter>
    );

    expect(await screen.findByRole("heading", { name: "Repositories → Projects" })).toBeDefined();
    // The sidebar draws its links only once its inbox, pinned issues and projects have answered
    // (`Sidebar.tsx`), which the page's heading above does not wait for, so the link is waited for.
    expect(await screen.findByRole("link", { name: "Settings" })).toHaveAttribute(
      "href",
      "/settings"
    );
    // The heading above is the section's, which renders before its mapping list: the rows arrive
    // with a read of their own, and reading them synchronously finds the table still saying
    // "Loading…" on a pod whose cores are busy.
    expect(await screen.findByRole("cell", { name: "owner/repo" })).toBeDefined();
    expect(screen.getByRole("columnheader", { name: "Default" })).toBeDefined();
    const radio = (repo: string) =>
      screen.getByRole("radio", {
        name: `Default repository of CORE: ${repo}`,
      }) as HTMLInputElement;
    expect(radio("owner/other").checked).toBe(true);
    expect(radio("owner/repo").checked).toBe(false);
    await user.click(radio("owner/repo"));
    await waitFor(() =>
      expect(putRepoProject).toHaveBeenLastCalledWith(
        "owner/repo",
        {
          project: "CORE",
          default: true,
        },
        keyedAttempt
      )
    );
    // The pick moves the project's default at once: one default per project.
    expect(radio("owner/repo").checked).toBe(true);
    expect(radio("owner/other").checked).toBe(false);

    await user.click(screen.getByLabelText("Repository"));
    await user.paste("Owner/Repo.git");
    // By role: the page's Keyboard shortcuts section is a region named Project too.
    const project = screen.getByRole("button", { name: pickNamed("Project") });
    await user.click(project);
    await user.click(within(panelOf(project)).getByRole("option", { name: "CORE · Core" }));
    await user.click(screen.getByRole("button", { name: "Add mapping" }));
    await waitFor(() =>
      expect(putRepoProject).toHaveBeenLastCalledWith(
        "owner/repo",
        { project: "CORE" },
        keyedAttempt
      )
    );
    expect(screen.getAllByRole("cell", { name: "owner/repo" })).toHaveLength(1);
    expect(screen.queryByRole("cell", { name: "Owner/Repo.git" })).not.toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Delete mapping for owner/repo" }));
    await waitFor(() =>
      expect(deleteRepoProject).toHaveBeenLastCalledWith("owner/repo", keyedAttempt)
    );
    await waitFor(() =>
      expect(screen.queryByRole("cell", { name: "owner/repo" })).not.toBeInTheDocument()
    );
  } finally {
    cleanup();
    queryClient.clear();
    whoAmI.mockRestore();
    listIssues.mockRestore();
    getMyState.mockRestore();
    getInbox.mockRestore();
    listRepoProjects.mockRestore();
    listAgentTokens.mockRestore();
    getCredentialPending.mockRestore();
    listArchitectureSources.mockRestore();
    getLegionDaemon.mockRestore();
    listDefaults.mockRestore();
    getDeliverySettings.mockRestore();
    listProjects.mockRestore();
    putRepoProject.mockRestore();
    deleteRepoProject.mockRestore();
    restoreMatchMedia();
  }
});

// A mapping write's response describes one row, not the project's mappings as they stand when it
// arrives: the page reads them again once the write settles, so a response that arrives after the
// page already read a newer default does not put the older one back.
test("a delayed mapping response does not undo a newer default the page has read", async () => {
  const user = userEvent.setup();
  const restoreMatchMedia = stubMatchMedia();
  const row = (repo: string, isDefault: boolean): RepoProject => ({
    created_at: "2026-09-10T00:00:00Z",
    created_by: { id: "alice", kind: "user" },
    default: isDefault,
    project: "CORE",
    renamed_to: null,
    repo,
  });
  const server = mappingServer([row("owner/repo", false), row("owner/other", true)]);
  const whoAmI = spyOn(api, "whoAmI").mockResolvedValue({
    event_head: "0",
    kind: "user",
    login: "alice",
  });
  const listIssues = spyOn(api, "listIssues").mockResolvedValue([]);
  const getMyState = spyOn(api, "getMyState").mockResolvedValue({});
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([]);
  const listRepoProjects = spyOn(api, "listRepoProjects").mockImplementation(server.list);
  const listAgentTokens = spyOn(api, "listAgentTokens").mockResolvedValue([]);
  const getCredentialPending = spyOn(api, "getCredentialPending").mockResolvedValue(null);
  const listArchitectureSources = spyOn(api, "listArchitectureSources").mockResolvedValue([]);
  const getLegionDaemon = spyOn(api, "getLegionDaemon").mockResolvedValue(null);
  const listDefaults = spyOn(api, "listProjectDefaultAssignees").mockResolvedValue([]);
  const getDeliverySettings = spyOn(api, "getDeliverySettings").mockResolvedValue(null);
  const listProjects = spyOn(api, "listProjects").mockResolvedValue([
    {
      created_at: "2026-09-01T00:00:00Z",
      key: "CORE",
      name: "Core",
      open_asks: 0,
      gated_steps: [],
    },
  ]);
  // The server saves the pick at once; its answer reaches the page only when the test says.
  let answer: (() => void) | undefined;
  const putRepoProject = spyOn(api, "putRepoProject").mockImplementation((repo, input) => {
    const saved = server.put(repo, input);
    return new Promise<RepoProject>((resolve) => {
      answer = () => resolve(saved);
    });
  });
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const radio = (repo: string) =>
    screen.getByRole("radio", { name: `Default repository of CORE: ${repo}` }) as HTMLInputElement;

  try {
    render(
      <MemoryRouter initialEntries={["/settings"]}>
        <QueryClientProvider client={queryClient}>
          <App />
        </QueryClientProvider>
      </MemoryRouter>
    );
    await screen.findByRole("radio", { name: "Default repository of CORE: owner/repo" });
    await user.click(radio("owner/repo"));
    await waitFor(() => expect(putRepoProject).toHaveBeenCalledTimes(1));
    // Another browser makes owner/other the default again, and this page reads it (as a mapping
    // event would have it do) while its own write's answer is still on the way.
    server.put("owner/other", { default: true, project: "CORE" });
    await act(() => queryClient.invalidateQueries({ queryKey: ["repo-projects"] }));
    await waitFor(() => expect(radio("owner/other").checked).toBe(true));
    const reads = listRepoProjects.mock.calls.length;
    act(() => answer?.());
    await waitFor(() => expect(listRepoProjects.mock.calls.length).toBeGreaterThan(reads));
    await waitFor(() => expect(radio("owner/other").checked).toBe(true));
    expect(radio("owner/repo").checked).toBe(false);
  } finally {
    cleanup();
    queryClient.clear();
    whoAmI.mockRestore();
    listIssues.mockRestore();
    getMyState.mockRestore();
    getInbox.mockRestore();
    listRepoProjects.mockRestore();
    listAgentTokens.mockRestore();
    getCredentialPending.mockRestore();
    listArchitectureSources.mockRestore();
    getLegionDaemon.mockRestore();
    listDefaults.mockRestore();
    getDeliverySettings.mockRestore();
    listProjects.mockRestore();
    putRepoProject.mockRestore();
    restoreMatchMedia();
  }
});

test("repository settings retries a failed mapping query", async () => {
  const user = userEvent.setup();
  const whoAmI = spyOn(api, "whoAmI").mockResolvedValue({
    event_head: "0",
    kind: "user",
    login: "alice",
  });
  const listIssues = spyOn(api, "listIssues").mockResolvedValue([]);
  const getMyState = spyOn(api, "getMyState").mockResolvedValue({});
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([]);
  const listRepoProjects = spyOn(api, "listRepoProjects")
    .mockRejectedValueOnce(new Error("network"))
    .mockResolvedValueOnce([]);
  const listAgentTokens = spyOn(api, "listAgentTokens").mockResolvedValue([]);
  const getCredentialPending = spyOn(api, "getCredentialPending").mockResolvedValue(null);
  const listArchitectureSources = spyOn(api, "listArchitectureSources").mockResolvedValue([]);
  const getLegionDaemon = spyOn(api, "getLegionDaemon").mockResolvedValue(null);
  const listDefaults = spyOn(api, "listProjectDefaultAssignees").mockResolvedValue([]);
  const getDeliverySettings = spyOn(api, "getDeliverySettings").mockResolvedValue(null);
  const listProjects = spyOn(api, "listProjects").mockResolvedValue([
    {
      created_at: "2026-09-01T00:00:00Z",
      key: "CORE",
      name: "Core",
      open_asks: 0,
      gated_steps: [],
    },
  ]);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });

  try {
    render(
      <MemoryRouter initialEntries={["/settings"]}>
        <QueryClientProvider client={queryClient}>
          <App />
        </QueryClientProvider>
      </MemoryRouter>
    );

    expect(await screen.findByRole("alert")).toHaveTextContent(
      "Couldn't load repository mappings."
    );
    await user.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(listRepoProjects).toHaveBeenCalledTimes(2));
  } finally {
    cleanup();
    queryClient.clear();
    whoAmI.mockRestore();
    listIssues.mockRestore();
    getMyState.mockRestore();
    getInbox.mockRestore();
    listRepoProjects.mockRestore();
    listAgentTokens.mockRestore();
    getCredentialPending.mockRestore();
    listArchitectureSources.mockRestore();
    getLegionDaemon.mockRestore();
    listDefaults.mockRestore();
    getDeliverySettings.mockRestore();
    listProjects.mockRestore();
  }
});

test("a human creates a project from Settings and it appears in the mappings selector", async () => {
  const user = userEvent.setup();
  const core: Project = {
    created_at: "2026-09-01T00:00:00Z",
    key: "CORE",
    name: "Core",
    open_asks: 0,
    gated_steps: [],
  };
  const listed: Project = {
    created_at: "2026-09-10T00:00:00Z",
    key: "QA",
    name: "Quality",
    open_asks: 0,
    gated_steps: [],
  };
  // The create answers the project with its default; the project list carries none.
  const created: CreatedProject = { ...listed, default_assignee: "alice" };
  const whoAmI = spyOn(api, "whoAmI").mockResolvedValue({
    event_head: "0",
    kind: "user",
    login: "alice",
  });
  const listIssues = spyOn(api, "listIssues").mockResolvedValue([]);
  const getMyState = spyOn(api, "getMyState").mockResolvedValue({});
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([]);
  const listRepoProjects = spyOn(api, "listRepoProjects").mockResolvedValue([]);
  const listAgentTokens = spyOn(api, "listAgentTokens").mockResolvedValue([]);
  const getCredentialPending = spyOn(api, "getCredentialPending").mockResolvedValue(null);
  const listArchitectureSources = spyOn(api, "listArchitectureSources").mockResolvedValue([]);
  const getLegionDaemon = spyOn(api, "getLegionDaemon").mockResolvedValue(null);
  // The defaults read is the one place a project's default comes from: the page's first read
  // knows CORE alone, and the one after the create knows QA too.
  const listDefaults = spyOn(api, "listProjectDefaultAssignees")
    .mockResolvedValueOnce([{ assignee: "alice", project: "CORE" }])
    .mockResolvedValue([
      { assignee: "alice", project: "CORE" },
      { assignee: "alice", project: "QA" },
    ]);
  const getDeliverySettings = spyOn(api, "getDeliverySettings").mockResolvedValue(null);
  const listProjects = spyOn(api, "listProjects")
    .mockResolvedValueOnce([core])
    .mockResolvedValueOnce([core, listed]);
  const createProject = spyOn(api, "createProject").mockResolvedValue(created);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });

  try {
    render(
      <MemoryRouter initialEntries={["/settings"]}>
        <QueryClientProvider client={queryClient}>
          <App />
        </QueryClientProvider>
      </MemoryRouter>
    );

    expect(await screen.findByRole("cell", { name: "CORE" })).toBeDefined();
    await user.click(screen.getByLabelText("Key"));
    await user.paste("qa");
    await user.click(screen.getByLabelText("Name"));
    await user.paste("Quality");
    expect(screen.getByLabelText("Name")).toHaveProperty("maxLength", PROJECT_NAME_MAX);
    // The form starts on the person the server would take anyway - the one creating it - and
    // sends them, so the new project names somebody from the moment it exists.
    expect(screen.getByLabelText("Default assignee").textContent).toBe("alice");
    await user.click(screen.getByRole("button", { name: "New project" }));

    await waitFor(() =>
      expect(createProject).toHaveBeenLastCalledWith(
        { default_assignee: "alice", key: "QA", name: "Quality" },
        keyedAttempt
      )
    );
    await waitFor(() => expect(listProjects).toHaveBeenCalledTimes(2));
    expect(await screen.findByRole("cell", { name: "QA" })).toBeDefined();
    const qa = await screen.findByLabelText("Default assignee of QA");
    await waitFor(() => expect(qa.textContent).toBe("alice"));
    expect(listDefaults).toHaveBeenCalledTimes(2);
    // By role: the page's Keyboard shortcuts section is a region named Project too.
    const project = screen.getByRole("button", { name: pickNamed("Project") });
    await user.click(project);
    expect(
      within(panelOf(project)).getByRole("option", {
        name: "QA · Quality",
      })
    ).toBeDefined();
    expect(screen.getByLabelText("Key")).toHaveProperty("value", "");
  } finally {
    cleanup();
    queryClient.clear();
    whoAmI.mockRestore();
    listIssues.mockRestore();
    getInbox.mockRestore();
    getMyState.mockRestore();
    listRepoProjects.mockRestore();
    listAgentTokens.mockRestore();
    getCredentialPending.mockRestore();
    listArchitectureSources.mockRestore();
    getLegionDaemon.mockRestore();
    listDefaults.mockRestore();
    getDeliverySettings.mockRestore();
    listProjects.mockRestore();
    createProject.mockRestore();
  }
});

test("creating a project with a taken key shows the server's error inline", async () => {
  const user = userEvent.setup();
  const core: Project = {
    created_at: "2026-09-01T00:00:00Z",
    key: "CORE",
    name: "Core",
    open_asks: 0,
    gated_steps: [],
  };
  const whoAmI = spyOn(api, "whoAmI").mockResolvedValue({
    event_head: "0",
    kind: "user",
    login: "alice",
  });
  const listIssues = spyOn(api, "listIssues").mockResolvedValue([]);
  const getMyState = spyOn(api, "getMyState").mockResolvedValue({});
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([]);
  const listRepoProjects = spyOn(api, "listRepoProjects").mockResolvedValue([]);
  const listAgentTokens = spyOn(api, "listAgentTokens").mockResolvedValue([]);
  const getCredentialPending = spyOn(api, "getCredentialPending").mockResolvedValue(null);
  const listArchitectureSources = spyOn(api, "listArchitectureSources").mockResolvedValue([]);
  const getLegionDaemon = spyOn(api, "getLegionDaemon").mockResolvedValue(null);
  const listDefaults = spyOn(api, "listProjectDefaultAssignees").mockResolvedValue([]);
  const getDeliverySettings = spyOn(api, "getDeliverySettings").mockResolvedValue(null);
  const listProjects = spyOn(api, "listProjects").mockResolvedValue([core]);
  const createProject = spyOn(api, "createProject").mockRejectedValue(
    new ApiError(409, { code: "PROJECT_EXISTS", error: "a project with this key already exists" })
  );
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });

  try {
    render(
      <MemoryRouter initialEntries={["/settings"]}>
        <QueryClientProvider client={queryClient}>
          <App />
        </QueryClientProvider>
      </MemoryRouter>
    );

    expect(await screen.findByRole("cell", { name: "CORE" })).toBeDefined();
    await user.click(screen.getByLabelText("Key"));
    await user.paste("CORE");
    await user.click(screen.getByLabelText("Name"));
    await user.paste("Core Again");
    await user.click(screen.getByRole("button", { name: "New project" }));

    expect(await screen.findByRole("alert")).toHaveTextContent(
      "a project with this key already exists"
    );
  } finally {
    cleanup();
    queryClient.clear();
    whoAmI.mockRestore();
    listIssues.mockRestore();
    getInbox.mockRestore();
    getMyState.mockRestore();
    listRepoProjects.mockRestore();
    listAgentTokens.mockRestore();
    getCredentialPending.mockRestore();
    listArchitectureSources.mockRestore();
    getLegionDaemon.mockRestore();
    listDefaults.mockRestore();
    getDeliverySettings.mockRestore();
    listProjects.mockRestore();
    createProject.mockRestore();
  }
});

/** The default-assignee routes' server state as the Settings page reaches it: a read answers
 *  what the writes have left, and `sent` records each write's assignee as it arrives. Every
 *  project names a person and no write clears one, so an assignee is always a login. While
 *  `hold()`'s release has not been called, a write that arrives waits before it lands. */
function defaultAssigneeServer(initial: ProjectDefaultAssignee[]) {
  let rows = initial.map((row) => ({ ...row }));
  const sent: string[] = [];
  let gate: Promise<void> | undefined;
  return {
    hold: (): (() => void) => {
      const { promise, resolve } = Promise.withResolvers<void>();
      gate = promise;
      return () => {
        gate = undefined;
        resolve();
      };
    },
    list: async (): Promise<ProjectDefaultAssignee[]> => rows.map((row) => ({ ...row })),
    put: async (
      project: string,
      { assignee }: { assignee: string }
    ): Promise<ProjectDefaultAssignee> => {
      sent.push(assignee);
      await gate;
      rows = rows.filter((row) => row.project !== project);
      rows.push({ assignee, project });
      return { assignee, project };
    },
    rows: () => rows,
    sent,
  };
}

/** Renders the Settings route with every read but the default assignees and the people list
 *  stubbed, listing `projects`. `leave` unmounts the page and `reopen` renders it again on the
 *  same QueryClient, as leaving Settings and coming back does; `done` unmounts it and restores
 *  the stubs. */
function renderSettings(projects: string[]): {
  done: () => void;
  leave: () => void;
  reopen: () => void;
} {
  const stubs = [
    spyOn(api, "whoAmI").mockResolvedValue({ event_head: "0", kind: "user", login: "alice" }),
    spyOn(api, "listIssues").mockResolvedValue([]),
    spyOn(api, "getMyState").mockResolvedValue({}),
    spyOn(api, "getInbox").mockResolvedValue([]),
    spyOn(api, "listRepoProjects").mockResolvedValue([]),
    spyOn(api, "listAgentTokens").mockResolvedValue([]),
    spyOn(api, "getCredentialPending").mockResolvedValue(null),
    spyOn(api, "listArchitectureSources").mockResolvedValue([]),
    spyOn(api, "getLegionDaemon").mockResolvedValue(null),
    spyOn(api, "getDeliverySettings").mockResolvedValue(null),
    spyOn(api, "listProjects").mockResolvedValue(
      projects.map((key) => ({
        created_at: "2026-09-01T00:00:00Z",
        key,
        name: key,
        open_asks: 0,
        gated_steps: [],
      }))
    ),
  ];
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const page = (
    <MemoryRouter initialEntries={["/settings"]}>
      <QueryClientProvider client={queryClient}>
        <App />
      </QueryClientProvider>
    </MemoryRouter>
  );
  render(page);
  return {
    done: () => {
      cleanup();
      queryClient.clear();
      for (const stub of stubs) {
        stub.mockRestore();
      }
    },
    leave: cleanup,
    reopen: () => {
      render(page);
    },
  };
}

/** A project's default-assignee list's trigger, found by its label: its name may go on to the
 *  person it shows. What it shows is its value, as a select's `value` was. */
const defaultAssigneeControl = (project: string): Promise<HTMLButtonElement> =>
  screen.findByLabelText<HTMLButtonElement>(`Default assignee of ${project}`);

/** Opens `control`'s list, unless it stands open, and answers it once it offers `login`: the
 *  people are read only once a reader reaches for the control. The list is portalled out of the
 *  page, so it is found through its trigger. */
async function listOffering(
  user: UserEvent,
  control: HTMLElement,
  login: string
): Promise<HTMLElement> {
  if (control.getAttribute("aria-expanded") !== "true") await user.click(control);
  const list = within(panelOf(control)).getByRole("listbox");
  await within(list).findByRole("option", { name: login });
  return list;
}

/** Picks `login` from `control`'s list, as `selectOptions` picked it from the select. */
async function pickPerson(user: UserEvent, control: HTMLElement, login: string): Promise<void> {
  const list = await listOffering(user, control, login);
  await user.click(within(list).getByRole("option", { name: login }));
}

/** The people `list` offers, each as its option's name: the row's words without the tick the
 *  picked one carries, which is hidden from its name. */
function offered(list: HTMLElement): string[] {
  return within(list)
    .getAllByRole("option")
    .map((option) =>
      [...option.children]
        .filter((part) => part.getAttribute("aria-hidden") !== "true")
        .map((part) => part.textContent)
        .join("")
    );
}

test("default assignee picks made while one is saving show, and only the newest follows the held save", async () => {
  const user = userEvent.setup();
  const server = defaultAssigneeServer([{ assignee: "alice@example.com", project: "CORE" }]);
  const listDefaults = spyOn(api, "listProjectDefaultAssignees").mockImplementation(server.list);
  const putDefault = spyOn(api, "putProjectDefaultAssignee").mockImplementation(server.put);
  const listUsers = spyOn(api, "listUsers").mockResolvedValue([
    { login: "alice@example.com" },
    { login: "bob@example.com" },
    { login: "carol@example.com" },
  ]);
  const { done } = renderSettings(["CORE"]);

  try {
    const section = within(await screen.findByRole("region", { name: "Projects" }));
    // A default set on the project an unmapped repository falls back to is what a reader has to
    // weigh while setting one, so the consequence sits beside the control. The region is drawn
    // at once, saying `Loading projects…`, so the line is waited for: it comes with the projects.
    expect(await section.findByText(/no project mapping/)).toHaveTextContent("default project");

    const control = await defaultAssigneeControl("CORE");
    await waitFor(() => expect(control.textContent).toBe("alice@example.com"));
    expect(control.disabled).toBe(false);
    // Nobody is listed until a reader reaches for the picker.
    expect(listUsers).not.toHaveBeenCalled();

    // Reaching the control without pressing it: user-event has no form for focus alone. The
    // focus alone starts the people read, before any press.
    fireEvent.focus(control);
    await waitFor(() => expect(listUsers).toHaveBeenCalledTimes(1));
    const list = await listOffering(user, control, "carol@example.com");
    expect(offered(list)).toEqual(["alice@example.com", "bob@example.com", "carol@example.com"]);

    const release = server.hold();
    await pickPerson(user, control, "bob@example.com");
    await waitFor(() => expect(control.textContent).toBe("bob@example.com"));
    await waitFor(() => expect(server.sent).toEqual(["bob@example.com"]));

    // Two more picks while bob's save is held: each shows, neither is sent yet.
    await pickPerson(user, control, "alice@example.com");
    await waitFor(() => expect(control.textContent).toBe("alice@example.com"));
    await pickPerson(user, control, "carol@example.com");
    await waitFor(() => expect(control.textContent).toBe("carol@example.com"));
    await act(async () => {});
    expect(putDefault).toHaveBeenCalledTimes(1);

    await act(async () => release());
    // Only the newest pick follows the held save: alice's was superseded before it was sent.
    await waitFor(() => expect(server.sent).toEqual(["bob@example.com", "carol@example.com"]));
    await waitFor(() =>
      expect(server.rows()).toEqual([{ assignee: "carol@example.com", project: "CORE" }])
    );
    expect(putDefault).toHaveBeenCalledTimes(2);
    await waitFor(() => expect(control.textContent).toBe("carol@example.com"));

    // Nothing clears a default, so the picker holds no option that names nobody: every pick it
    // can make sends a person, and CORE is left naming one.
    expect(offered(await listOffering(user, control, "carol@example.com"))).toEqual([
      "alice@example.com",
      "bob@example.com",
      "carol@example.com",
    ]);
    expect(putDefault.mock.calls.map(([, input]) => input)).toEqual([
      { assignee: "bob@example.com" },
      { assignee: "carol@example.com" },
    ]);
  } finally {
    done();
    listDefaults.mockRestore();
    putDefault.mockRestore();
    listUsers.mockRestore();
  }
});

// The saves still out when Settings unmounts belong to the QueryClient, not to the row: a pick on
// the remounted row waits behind the held save and supersedes the pick queued before leaving, so
// only the newest pick follows the held save.
test("a default assignee picked after Settings is remounted supersedes the pick queued before, and follows the held save", async () => {
  const user = userEvent.setup();
  const server = defaultAssigneeServer([{ assignee: "alice@example.com", project: "CORE" }]);
  const listDefaults = spyOn(api, "listProjectDefaultAssignees").mockImplementation(server.list);
  const putDefault = spyOn(api, "putProjectDefaultAssignee").mockImplementation(server.put);
  const listUsers = spyOn(api, "listUsers").mockResolvedValue([
    { login: "bob@example.com" },
    { login: "carol@example.com" },
    { login: "dave@example.com" },
  ]);
  const { done, leave, reopen } = renderSettings(["CORE"]);

  try {
    let control = await defaultAssigneeControl("CORE");
    await waitFor(() => expect(control.disabled).toBe(false));
    await listOffering(user, control, "dave@example.com");

    const release = server.hold();
    await pickPerson(user, control, "bob@example.com");
    await waitFor(() => expect(server.sent).toEqual(["bob@example.com"]));
    await pickPerson(user, control, "carol@example.com");
    await waitFor(() => expect(control.textContent).toBe("carol@example.com"));

    leave();
    reopen();
    control = await defaultAssigneeControl("CORE");
    // The pick still waiting behind bob's held save is what the returning row shows.
    await waitFor(() => expect(control.textContent).toBe("carol@example.com"));

    await pickPerson(user, control, "dave@example.com");
    await waitFor(() => expect(control.textContent).toBe("dave@example.com"));
    await act(async () => {});
    // Still one at a time: nothing follows bob's save onto the wire while it is held.
    expect(server.sent).toEqual(["bob@example.com"]);

    await act(async () => release());
    await waitFor(() => expect(server.sent).toEqual(["bob@example.com", "dave@example.com"]));
    await waitFor(() =>
      expect(server.rows()).toEqual([{ assignee: "dave@example.com", project: "CORE" }])
    );
    expect(putDefault).toHaveBeenCalledTimes(2);
    await waitFor(() => expect(control.textContent).toBe("dave@example.com"));
  } finally {
    done();
    listDefaults.mockRestore();
    putDefault.mockRestore();
    listUsers.mockRestore();
  }
});

// A save refused while Settings is unmounted is still the reader's to hear about: the reopened
// row says so, beside the stored value it fell back to, and Retry resends it.
test("a default assignee save refused while Settings is unmounted shows its reason when Settings is reopened", async () => {
  const user = userEvent.setup();
  const server = defaultAssigneeServer([{ assignee: "alice@example.com", project: "CORE" }]);
  const listDefaults = spyOn(api, "listProjectDefaultAssignees").mockImplementation(server.list);
  const refusal = Promise.withResolvers<ProjectDefaultAssignee>();
  const putDefault = spyOn(api, "putProjectDefaultAssignee")
    .mockReturnValueOnce(refusal.promise)
    .mockImplementation(server.put);
  const listUsers = spyOn(api, "listUsers").mockResolvedValue([
    { login: "alice@example.com" },
    { login: "bob@example.com" },
  ]);
  const { done, leave, reopen } = renderSettings(["CORE"]);

  try {
    let core = await defaultAssigneeControl("CORE");
    await waitFor(() => expect(core.textContent).toBe("alice@example.com"));
    await pickPerson(user, core, "bob@example.com");
    await waitFor(() => expect(putDefault).toHaveBeenCalledTimes(1));

    leave();
    // Dispatch's verdict, a 4xx: a 5xx is tried again under the write's key, not refused.
    await act(async () =>
      refusal.reject(
        new ApiError(400, {
          code: "ASSIGNEE_NOT_ALLOWED",
          error: "the default assignee save was refused",
        })
      )
    );
    reopen();

    core = await defaultAssigneeControl("CORE");
    const coreCell = within(core.closest("td") as HTMLElement);
    await waitFor(() =>
      expect(coreCell.getByRole("alert")).toHaveTextContent("the default assignee save was refused")
    );
    expect(core.textContent).toBe("alice@example.com");

    await user.click(coreCell.getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(coreCell.queryByRole("alert")).not.toBeInTheDocument());
    await waitFor(() =>
      expect(server.rows()).toEqual([{ assignee: "bob@example.com", project: "CORE" }])
    );
    expect(putDefault.mock.calls).toEqual([
      ["CORE", { assignee: "bob@example.com" }, keyedAttempt],
      ["CORE", { assignee: "bob@example.com" }, keyedAttempt],
    ]);
    await waitFor(() => expect(core.textContent).toBe("bob@example.com"));
  } finally {
    done();
    listDefaults.mockRestore();
    putDefault.mockRestore();
    listUsers.mockRestore();
  }
});

test("a refused default assignee puts the row back, shows the server's reason in that row, and Retry resends it", async () => {
  const user = userEvent.setup();
  const server = defaultAssigneeServer([
    { assignee: "alice@example.com", project: "CORE" },
    { assignee: "alice@example.com", project: "OPS" },
  ]);
  const listDefaults = spyOn(api, "listProjectDefaultAssignees").mockImplementation(server.list);
  const putDefault = spyOn(api, "putProjectDefaultAssignee")
    .mockRejectedValueOnce(
      new ApiError(400, {
        code: "ASSIGNEE_NOT_ALLOWED",
        error: "bob@example.com has never signed in",
      })
    )
    .mockImplementation(server.put);
  const listUsers = spyOn(api, "listUsers").mockResolvedValue([
    { login: "alice@example.com" },
    { login: "bob@example.com" },
  ]);
  const { done } = renderSettings(["CORE", "OPS"]);

  try {
    const core = await defaultAssigneeControl("CORE");
    const ops = await defaultAssigneeControl("OPS");
    const coreCell = within(core.closest("td") as HTMLElement);
    const opsCell = within(ops.closest("td") as HTMLElement);
    await waitFor(() => expect(core.disabled).toBe(false));

    await pickPerson(user, core, "bob@example.com");
    await waitFor(() =>
      expect(coreCell.getByRole("alert")).toHaveTextContent("bob@example.com has never signed in")
    );
    // The refused pick is not left on screen: the row reads the person the server holds.
    expect(core.textContent).toBe("alice@example.com");

    // A save on another row neither clears nor takes over this row's error.
    await pickPerson(user, ops, "bob@example.com");
    await waitFor(() =>
      expect(server.rows()).toEqual([
        { assignee: "alice@example.com", project: "CORE" },
        { assignee: "bob@example.com", project: "OPS" },
      ])
    );
    expect(ops.textContent).toBe("bob@example.com");
    expect(opsCell.queryByRole("alert")).not.toBeInTheDocument();
    expect(coreCell.getByRole("alert")).toHaveTextContent("bob@example.com has never signed in");

    await user.click(coreCell.getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(coreCell.queryByRole("alert")).not.toBeInTheDocument());
    expect(putDefault.mock.calls).toEqual([
      ["CORE", { assignee: "bob@example.com" }, keyedAttempt],
      ["OPS", { assignee: "bob@example.com" }, keyedAttempt],
      ["CORE", { assignee: "bob@example.com" }, keyedAttempt],
    ]);
    await waitFor(() => expect(core.textContent).toBe("bob@example.com"));
    expect(server.rows()).toContainEqual({ assignee: "bob@example.com", project: "CORE" });
  } finally {
    done();
    listDefaults.mockRestore();
    putDefault.mockRestore();
    listUsers.mockRestore();
  }
});

test("a default assignee the page has not read is disabled and names nobody", async () => {
  const user = userEvent.setup();
  const read = Promise.withResolvers<ProjectDefaultAssignee[]>();
  const listDefaults = spyOn(api, "listProjectDefaultAssignees").mockReturnValueOnce(read.promise);
  const listUsers = spyOn(api, "listUsers").mockResolvedValue([{ login: "alice@example.com" }]);
  const { done } = renderSettings(["CORE"]);

  try {
    const section = within(await screen.findByRole("region", { name: "Projects" }));
    const control = await defaultAssigneeControl("CORE");
    // Still reading: nobody knows yet which person CORE names. Disabled, the control opens no
    // list, and the one value it holds is what it shows.
    expect(control.disabled).toBe(true);
    expect(control.textContent).toBe("Loading…");

    await act(async () => read.reject(new ApiError(500, { code: "INTERNAL", error: "boom" })));
    await waitFor(() =>
      expect(section.getByRole("alert")).toHaveTextContent("Couldn't load the default assignees.")
    );
    expect(control.disabled).toBe(true);
    expect(control.textContent).toBe("Unavailable");

    listDefaults.mockResolvedValue([{ assignee: "alice@example.com", project: "CORE" }]);
    await user.click(section.getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(control.disabled).toBe(false));
    expect(control.textContent).toBe("alice@example.com");
    expect(section.queryByRole("alert")).not.toBeInTheDocument();
  } finally {
    done();
    listDefaults.mockRestore();
    listUsers.mockRestore();
  }
});
