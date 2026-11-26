import { beforeAll, expect, spyOn, test } from "bun:test";
import { PROJECT_NAME_MAX } from "@legion/contracts";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
// biome-ignore lint/style/noRestrictedImports: the focus sites below reach the picker's people list without a press, which user-event has no form for
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

import { ApiError, api } from "../api/client";
import type { CreatedProject, Project, ProjectDefaultAssignee } from "../api/types";
import { App } from "../app";
// The Settings page is a lazy route of the app. Its module's first compile is not what a wait is
// for, so the module is loaded here, with the test file, and no wait measures that load.
import "../features/settings/SettingsPage";
import { warmMarkdownRenderer } from "../features/refs/markdown-engine";
import { keyedAttempt } from "./keyed-attempt";

// The rows draw through the shared renderer, whose Markdown engine loads lazily, and they run in
// random order, so the file loads the engine before its first row, whichever row that is.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

test("a human creates a project from Settings and it appears in the project pickers", async () => {
  const user = userEvent.setup();
  const core: Project = {
    created_at: "2026-09-01T00:00:00Z",
    key: "CORE",
    name: "Core",
    open_asks: 0,
    gated_steps: [],
    proven_sessions: false,
  };
  const listed: Project = {
    created_at: "2026-09-10T00:00:00Z",
    key: "QA",
    name: "Quality",
    open_asks: 0,
    gated_steps: [],
    proven_sessions: false,
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
    expect(screen.getByLabelText<HTMLSelectElement>("Default assignee").value).toBe("alice");
    await user.click(screen.getByRole("button", { name: "New project" }));

    await waitFor(() =>
      expect(createProject).toHaveBeenLastCalledWith(
        { default_assignee: "alice", key: "QA", name: "Quality" },
        keyedAttempt
      )
    );
    await waitFor(() => expect(listProjects).toHaveBeenCalledTimes(2));
    expect(await screen.findByRole("cell", { name: "QA" })).toBeDefined();
    const qa = await screen.findByRole("combobox", { name: "Default assignee of QA" });
    await waitFor(() => expect(qa).toHaveProperty("value", "alice"));
    expect(listDefaults).toHaveBeenCalledTimes(2);
    expect(
      within(screen.getByRole("combobox", { name: "Source project" })).getByRole("option", {
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
    listAgentTokens.mockRestore();
    getCredentialPending.mockRestore();
    listArchitectureSources.mockRestore();
    getLegionDaemon.mockRestore();
    listDefaults.mockRestore();
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
    proven_sessions: false,
  };
  const whoAmI = spyOn(api, "whoAmI").mockResolvedValue({
    event_head: "0",
    kind: "user",
    login: "alice",
  });
  const listIssues = spyOn(api, "listIssues").mockResolvedValue([]);
  const getMyState = spyOn(api, "getMyState").mockResolvedValue({});
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([]);
  const listAgentTokens = spyOn(api, "listAgentTokens").mockResolvedValue([]);
  const getCredentialPending = spyOn(api, "getCredentialPending").mockResolvedValue(null);
  const listArchitectureSources = spyOn(api, "listArchitectureSources").mockResolvedValue([]);
  const getLegionDaemon = spyOn(api, "getLegionDaemon").mockResolvedValue(null);
  const listDefaults = spyOn(api, "listProjectDefaultAssignees").mockResolvedValue([]);
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
    listAgentTokens.mockRestore();
    getCredentialPending.mockRestore();
    listArchitectureSources.mockRestore();
    getLegionDaemon.mockRestore();
    listDefaults.mockRestore();
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
    spyOn(api, "listAgentTokens").mockResolvedValue([]),
    spyOn(api, "getCredentialPending").mockResolvedValue(null),
    spyOn(api, "listArchitectureSources").mockResolvedValue([]),
    spyOn(api, "getLegionDaemon").mockResolvedValue(null),
    spyOn(api, "listProjects").mockResolvedValue(
      projects.map((key) => ({
        created_at: "2026-09-01T00:00:00Z",
        key,
        name: key,
        open_asks: 0,
        gated_steps: [],
        proven_sessions: false,
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

const defaultAssigneeControl = async (project: string): Promise<HTMLSelectElement> =>
  (await screen.findByRole("combobox", {
    name: `Default assignee of ${project}`,
  })) as HTMLSelectElement;

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
    const control = await defaultAssigneeControl("CORE");
    await waitFor(() => expect(control.value).toBe("alice@example.com"));
    expect(control.disabled).toBe(false);
    // Nobody is listed until a reader reaches for the picker.
    expect(listUsers).not.toHaveBeenCalled();

    // Reaching the control without pressing it: user-event has no form for focus alone.
    fireEvent.focus(control);
    await waitFor(() =>
      expect(within(control).getByRole("option", { name: "carol@example.com" })).toBeDefined()
    );
    expect(
      within(control)
        .getAllByRole("option")
        .map((option) => option.textContent)
    ).toEqual(["alice@example.com", "bob@example.com", "carol@example.com"]);

    const release = server.hold();
    await user.selectOptions(control, "bob@example.com");
    await waitFor(() => expect(control.value).toBe("bob@example.com"));
    await waitFor(() => expect(server.sent).toEqual(["bob@example.com"]));

    // Two more picks while bob's save is held: each shows, neither is sent yet.
    await user.selectOptions(control, "alice@example.com");
    await waitFor(() => expect(control.value).toBe("alice@example.com"));
    await user.selectOptions(control, "carol@example.com");
    await waitFor(() => expect(control.value).toBe("carol@example.com"));
    await act(async () => {});
    expect(putDefault).toHaveBeenCalledTimes(1);

    await act(async () => release());
    // Only the newest pick follows the held save: alice's was superseded before it was sent.
    await waitFor(() => expect(server.sent).toEqual(["bob@example.com", "carol@example.com"]));
    await waitFor(() =>
      expect(server.rows()).toEqual([{ assignee: "carol@example.com", project: "CORE" }])
    );
    expect(putDefault).toHaveBeenCalledTimes(2);
    await waitFor(() => expect(control.value).toBe("carol@example.com"));

    // Nothing clears a default, so the picker holds no option that names nobody: every pick it
    // can make sends a person, and CORE is left naming one.
    expect(
      within(control)
        .getAllByRole("option")
        .map((option) => option.getAttribute("value"))
    ).toEqual(["alice@example.com", "bob@example.com", "carol@example.com"]);
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
    // Reaching the control without pressing it: user-event has no form for focus alone.
    fireEvent.focus(control);
    await waitFor(() =>
      expect(within(control).getByRole("option", { name: "dave@example.com" })).toBeDefined()
    );

    const release = server.hold();
    await user.selectOptions(control, "bob@example.com");
    await waitFor(() => expect(server.sent).toEqual(["bob@example.com"]));
    await user.selectOptions(control, "carol@example.com");
    await waitFor(() => expect(control.value).toBe("carol@example.com"));

    leave();
    reopen();
    control = await defaultAssigneeControl("CORE");
    // The pick still waiting behind bob's held save is what the returning row shows.
    await waitFor(() => expect(control.value).toBe("carol@example.com"));
    // Reaching the control without pressing it: user-event has no form for focus alone.
    fireEvent.focus(control);
    await waitFor(() =>
      expect(within(control).getByRole("option", { name: "dave@example.com" })).toBeDefined()
    );

    await user.selectOptions(control, "dave@example.com");
    await waitFor(() => expect(control.value).toBe("dave@example.com"));
    await act(async () => {});
    // Still one at a time: nothing follows bob's save onto the wire while it is held.
    expect(server.sent).toEqual(["bob@example.com"]);

    await act(async () => release());
    await waitFor(() => expect(server.sent).toEqual(["bob@example.com", "dave@example.com"]));
    await waitFor(() =>
      expect(server.rows()).toEqual([{ assignee: "dave@example.com", project: "CORE" }])
    );
    expect(putDefault).toHaveBeenCalledTimes(2);
    await waitFor(() => expect(control.value).toBe("dave@example.com"));
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
    await waitFor(() => expect(core.value).toBe("alice@example.com"));
    // Reaching the control without pressing it: user-event has no form for focus alone.
    fireEvent.focus(core);
    await waitFor(() =>
      expect(within(core).getByRole("option", { name: "bob@example.com" })).toBeDefined()
    );
    await user.selectOptions(core, "bob@example.com");
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
    expect(core.value).toBe("alice@example.com");

    await user.click(coreCell.getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(coreCell.queryByRole("alert")).not.toBeInTheDocument());
    await waitFor(() =>
      expect(server.rows()).toEqual([{ assignee: "bob@example.com", project: "CORE" }])
    );
    expect(putDefault.mock.calls).toEqual([
      ["CORE", { assignee: "bob@example.com" }, keyedAttempt],
      ["CORE", { assignee: "bob@example.com" }, keyedAttempt],
    ]);
    await waitFor(() => expect(core.value).toBe("bob@example.com"));
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
    // Reaching the control without pressing it: user-event has no form for focus alone.
    fireEvent.focus(core);
    await waitFor(() =>
      expect(within(core).getByRole("option", { name: "bob@example.com" })).toBeDefined()
    );

    await user.selectOptions(core, "bob@example.com");
    await waitFor(() =>
      expect(coreCell.getByRole("alert")).toHaveTextContent("bob@example.com has never signed in")
    );
    // The refused pick is not left on screen: the row reads the person the server holds.
    expect(core.value).toBe("alice@example.com");

    // A save on another row neither clears nor takes over this row's error.
    await user.selectOptions(ops, "bob@example.com");
    await waitFor(() =>
      expect(server.rows()).toEqual([
        { assignee: "alice@example.com", project: "CORE" },
        { assignee: "bob@example.com", project: "OPS" },
      ])
    );
    expect(ops.value).toBe("bob@example.com");
    expect(opsCell.queryByRole("alert")).not.toBeInTheDocument();
    expect(coreCell.getByRole("alert")).toHaveTextContent("bob@example.com has never signed in");

    await user.click(coreCell.getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(coreCell.queryByRole("alert")).not.toBeInTheDocument());
    expect(putDefault.mock.calls).toEqual([
      ["CORE", { assignee: "bob@example.com" }, keyedAttempt],
      ["OPS", { assignee: "bob@example.com" }, keyedAttempt],
      ["CORE", { assignee: "bob@example.com" }, keyedAttempt],
    ]);
    await waitFor(() => expect(core.value).toBe("bob@example.com"));
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
  const optionNames = (control: HTMLSelectElement) =>
    within(control)
      .getAllByRole("option")
      .map((option) => option.textContent);

  try {
    const section = within(await screen.findByRole("region", { name: "Projects" }));
    const control = await defaultAssigneeControl("CORE");
    // Still reading: nobody knows yet which person CORE names.
    expect(control.disabled).toBe(true);
    expect(control.value).not.toBe("");
    expect(optionNames(control)).toEqual(["Loading…"]);

    await act(async () => read.reject(new ApiError(500, { code: "INTERNAL", error: "boom" })));
    await waitFor(() =>
      expect(section.getByRole("alert")).toHaveTextContent("Couldn't load the default assignees.")
    );
    expect(control.disabled).toBe(true);
    expect(control.value).not.toBe("");
    expect(optionNames(control)).toEqual(["Unavailable"]);

    listDefaults.mockResolvedValue([{ assignee: "alice@example.com", project: "CORE" }]);
    await user.click(section.getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(control.disabled).toBe(false));
    expect(control.value).toBe("alice@example.com");
    expect(section.queryByRole("alert")).not.toBeInTheDocument();
  } finally {
    done();
    listDefaults.mockRestore();
    listUsers.mockRestore();
  }
});
