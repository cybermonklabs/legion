import { beforeAll, expect, jest, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { realClock, tick } from "../../__tests__/fake-clock";
import { afterFrameAndTasks } from "../../__tests__/frames";
import { heldAnswers } from "../../__tests__/held-answers";
import { pressHeld } from "../../__tests__/held-press";
import { heldUntilLate } from "../../__tests__/held-until-late";
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { listOf, panelOf, pickFromLabelled, textBoxes } from "../../__tests__/list-picks";
import { stopTheLateWrite } from "../../__tests__/stop-late-write";
import { userOnFakeClock } from "../../__tests__/user";
import { ApiError, api, NetworkError, REQUEST_DEADLINE_MS } from "../../api/client";
import { DispatchQueryClient } from "../../api/query-client";
import type { ArchitectureSource, Project } from "../../api/types";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { SendStoppedError, stoppedWriteNote } from "../writes/keyed-write";
import { writesOf } from "../writes/write-store";
import { ArchitectureSourcesSection } from "./ArchitectureSourcesSection";

// The rows draw through the shared renderer, whose Markdown engine loads lazily, and they run in
// random order, so the file loads the engine before its first row, whichever row that is.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

const architectureSource: ArchitectureSource = {
  branch: "main",
  components: 0,
  created_at: "2026-09-10T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  enabled: true,
  last_commit: null,
  last_error: null,
  last_sync_at: null,
  project: "CORE",
  repo: "legion/arch",
};

const core: Project = {
  created_at: "2026-09-01T00:00:00Z",
  gated_steps: [],
  key: "CORE",
  name: "Core",
  open_asks: 0,
  proven_sessions: false,
};

/** The architecture source routes' server state as the Settings page reaches it: one row per
 *  project and repository, listed in the order they were added; a save of a repository the
 *  project already has changes its branch and resets its bookkeeping, as the server does. */
function sourceServer(initial: ArchitectureSource[]) {
  let rows = initial.map((row) => ({ ...row }));
  const same = (row: ArchitectureSource, project: string, repo: string) =>
    row.project === project && row.repo === repo;
  return {
    list: async (): Promise<ArchitectureSource[]> => rows.map((row) => ({ ...row })),
    put: async (
      project: string,
      repo: string,
      input: { branch: string }
    ): Promise<ArchitectureSource> => {
      const saved: ArchitectureSource = {
        ...architectureSource,
        branch: input.branch,
        components: rows.find((row) => same(row, project, repo))?.components ?? 0,
        project,
        repo,
      };
      rows = rows.some((row) => same(row, project, repo))
        ? rows.map((row) => (same(row, project, repo) ? saved : row))
        : [...rows, saved];
      return { ...saved };
    },
    remove: async (project: string, repo: string): Promise<void> => {
      rows = rows.filter((row) => !same(row, project, repo));
    },
    sync: async (project: string, repo: string): Promise<ArchitectureSource> => {
      const row = rows.find((candidate) => same(candidate, project, repo));
      if (row === undefined) {
        throw new ApiError(404, { code: "SOURCE_NOT_FOUND" });
      }
      return { ...row };
    },
  };
}

test("a human adds an architecture source and the verified row appears", async () => {
  const user = userEvent.setup();
  const listArchitectureSources = spyOn(api, "listArchitectureSources")
    .mockResolvedValueOnce([])
    .mockResolvedValue([architectureSource]);
  const listProjects = spyOn(api, "listProjects").mockResolvedValue([
    {
      created_at: "2026-09-01T00:00:00Z",
      key: "CORE",
      name: "Core",
      open_asks: 0,
      gated_steps: [],
      proven_sessions: false,
    },
  ]);
  const putArchitectureSource = spyOn(api, "putArchitectureSource").mockResolvedValue(
    architectureSource
  );
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });

  try {
    render(
      <QueryClientProvider client={queryClient}>
        <ArchitectureSourcesSection />
      </QueryClientProvider>
    );

    const section = within(await screen.findByRole("region", { name: "Architecture sources" }));
    expect(await section.findByText("No architecture sources yet.")).toBeDefined();

    await pickFromLabelled(user, "Source project", "CORE · Core");
    await user.click(section.getByLabelText("Source repository"));
    await user.paste("legion/arch");
    await user.click(section.getByRole("button", { name: "Add source" }));
    await waitFor(() =>
      expect(putArchitectureSource).toHaveBeenLastCalledWith(
        "CORE",
        "legion/arch",
        { branch: "main" },
        keyedAttempt
      )
    );
    await waitFor(() => expect(section.getByRole("cell", { name: "legion/arch" })).toBeDefined());
    expect(section.getByRole("cell", { name: "Access verified" })).toBeDefined();
  } finally {
    cleanup();
    queryClient.clear();
    listArchitectureSources.mockRestore();
    listProjects.mockRestore();
    putArchitectureSource.mockRestore();
  }
});

test("a failed architecture source access check shows the server's reason inline", async () => {
  const user = userEvent.setup();
  const listArchitectureSources = spyOn(api, "listArchitectureSources").mockResolvedValue([]);
  const listProjects = spyOn(api, "listProjects").mockResolvedValue([
    {
      created_at: "2026-09-01T00:00:00Z",
      key: "CORE",
      name: "Core",
      open_asks: 0,
      gated_steps: [],
      proven_sessions: false,
    },
  ]);
  const putArchitectureSource = spyOn(api, "putArchitectureSource").mockRejectedValue(
    new ApiError(409, {
      code: "SOURCE_ACCESS",
      error: "the GitHub App is not installed on legion/arch",
    })
  );
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });

  try {
    render(
      <QueryClientProvider client={queryClient}>
        <ArchitectureSourcesSection />
      </QueryClientProvider>
    );

    const section = within(await screen.findByRole("region", { name: "Architecture sources" }));
    await pickFromLabelled(user, "Source project", "CORE · Core");
    await user.click(section.getByLabelText("Source repository"));
    await user.paste("legion/arch");
    await user.click(section.getByRole("button", { name: "Add source" }));

    await waitFor(() =>
      expect(section.getByRole("alert")).toHaveTextContent(
        "the GitHub App is not installed on legion/arch"
      )
    );
  } finally {
    cleanup();
    queryClient.clear();
    listArchitectureSources.mockRestore();
    listProjects.mockRestore();
    putArchitectureSource.mockRestore();
  }
});

test("each architecture source of a project is its own row, refreshed and deleted on its own", async () => {
  const user = userEvent.setup();
  const server = sourceServer([
    {
      ...architectureSource,
      components: 36,
      last_commit: "3be61df5aa001122334455667788990011223344",
      last_sync_at: "2026-09-10T00:00:00Z",
    },
    {
      ...architectureSource,
      components: 1,
      last_commit: "9f1e2d3c4b5a69788796a5b4c3d2e1f0a9b8c7d6",
      last_error: 'component id "dispatch" is already a component of CORE from legion/arch',
      last_sync_at: "2026-09-10T00:00:00Z",
      repo: "acme/infra",
    },
  ]);
  const listArchitectureSources = spyOn(api, "listArchitectureSources").mockImplementation(
    server.list
  );
  const listProjects = spyOn(api, "listProjects").mockResolvedValue([
    {
      created_at: "2026-09-01T00:00:00Z",
      key: "CORE",
      name: "Core",
      open_asks: 0,
      gated_steps: [],
      proven_sessions: false,
    },
  ]);
  const putArchitectureSource = spyOn(api, "putArchitectureSource").mockImplementation(server.put);
  const syncArchitectureSource = spyOn(api, "syncArchitectureSource").mockImplementation(
    server.sync
  );
  const deleteArchitectureSource = spyOn(api, "deleteArchitectureSource").mockImplementation(
    server.remove
  );
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  try {
    render(
      <QueryClientProvider client={queryClient}>
        <ArchitectureSourcesSection />
      </QueryClientProvider>
    );

    const section = within(await screen.findByRole("region", { name: "Architecture sources" }));
    await section.findByRole("cell", { name: "legion/arch" });
    const rowOf = (repo: string) => section.getByRole("cell", { name: repo }).closest("tr");
    expect(section.getAllByRole("cell", { name: "CORE" })).toHaveLength(2);
    // Each row shows its own import: the healthy one its commit and component count, the
    // failed one its error and the commit and count it still supplies.
    expect(rowOf("legion/arch")?.textContent).toContain("Synced 3be61df5aa00 · 36 components");
    const failed = rowOf("acme/infra")?.textContent;
    expect(failed).toContain('component id "dispatch" is already a component of CORE');
    expect(failed).toContain("Previous model stays up at 9f1e2d3c4b5a · 1 component");
    expect(failed).not.toContain("1 components");

    await user.click(
      section.getByRole("button", { name: "Refresh architecture source acme/infra for CORE" })
    );
    await waitFor(() =>
      expect(syncArchitectureSource).toHaveBeenCalledWith("CORE", "acme/infra", keyedAttempt)
    );
    expect(syncArchitectureSource).toHaveBeenCalledTimes(1);
    await waitFor(() =>
      expect(
        section.getByRole("button", { name: "Delete architecture source legion/arch for CORE" })
      ).not.toHaveAttribute("aria-disabled")
    );

    await user.click(
      section.getByRole("button", { name: "Delete architecture source legion/arch for CORE" })
    );
    await waitFor(() =>
      expect(section.queryByRole("cell", { name: "legion/arch" })).not.toBeInTheDocument()
    );
    expect(deleteArchitectureSource).toHaveBeenCalledTimes(1);
    expect(deleteArchitectureSource).toHaveBeenCalledWith("CORE", "legion/arch", keyedAttempt);
    expect(section.getByRole("cell", { name: "acme/infra" })).toBeDefined();

    // Saving a repository the project already has changes that source's branch: still one row.
    await pickFromLabelled(user, "Source project", "CORE · Core");
    await user.click(section.getByLabelText("Source repository"));
    await user.paste("acme/infra");
    await user.clear(section.getByLabelText("Branch"));
    await user.click(section.getByLabelText("Branch"));
    await user.paste("release");
    await user.click(section.getByRole("button", { name: "Add source" }));
    await waitFor(() => expect(section.getByRole("cell", { name: "release" })).toBeDefined());
    expect(putArchitectureSource).toHaveBeenCalledWith(
      "CORE",
      "acme/infra",
      { branch: "release" },
      keyedAttempt
    );
    expect(section.getAllByRole("cell", { name: "acme/infra" })).toHaveLength(1);
    expect(section.queryByRole("cell", { name: "main" })).not.toBeInTheDocument();
  } finally {
    cleanup();
    queryClient.clear();
    listArchitectureSources.mockRestore();
    listProjects.mockRestore();
    putArchitectureSource.mockRestore();
    syncArchitectureSource.mockRestore();
    deleteArchitectureSource.mockRestore();
  }
});

// A source's Refresh and Delete, and the form's Add source, repository and branch, are each held
// while their write is out, by `aria-disabled`, never `disabled` (`pressHeld`): the control the
// reader pressed or submitted from keeps the focus, and a second press sends nothing.
test.each([
  { label: "Refresh architecture source legion/arch for CORE", role: "button" },
  { label: "Delete architecture source legion/arch for CORE", role: "button" },
  { label: "Add source", role: "button" },
  { label: "Source repository", role: "textbox" },
  { label: "Branch", role: "textbox" },
] as const)(
  "$label, held while its write is out, keeps the focus, and a second press sends nothing",
  async ({ label, role }) => {
    const user = userEvent.setup();
    const listArchitectureSources = spyOn(api, "listArchitectureSources").mockResolvedValue([
      architectureSource,
    ]);
    const listProjects = spyOn(api, "listProjects").mockResolvedValue([core]);
    // The writes' answer, held until the case refuses it inside `act`.
    const answer = Promise.withResolvers<never>();
    const writes = [
      spyOn(api, "syncArchitectureSource").mockReturnValue(answer.promise),
      spyOn(api, "deleteArchitectureSource").mockReturnValue(answer.promise),
      spyOn(api, "putArchitectureSource").mockReturnValue(answer.promise),
    ];
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });

    try {
      render(
        <QueryClientProvider client={queryClient}>
          <ArchitectureSourcesSection />
        </QueryClientProvider>
      );
      const section = within(await screen.findByRole("region", { name: "Architecture sources" }));
      await section.findByRole("cell", { name: "legion/arch" });
      await pickFromLabelled(user, "Source project", "CORE · Core");
      await user.click(section.getByLabelText("Source repository"));
      await user.paste("acme/infra");
      await pressHeld(user, section.getByRole(role, { name: label }), {
        name: label,
        sent: () => writes.reduce((count, write) => count + write.mock.calls.length, 0),
      });
    } finally {
      await act(async () => answer.reject(new ApiError(403, { code: "FORBIDDEN", error: "no" })));
      cleanup();
      queryClient.clear();
      listArchitectureSources.mockRestore();
      listProjects.mockRestore();
      for (const write of writes) write.mockRestore();
    }
  }
);

/** The section over CORE and OPS, with no source yet and every save answered as the server would
 *  answer it. */
function renderSourceForm() {
  const server = sourceServer([]);
  const stubs = {
    listArchitectureSources: spyOn(api, "listArchitectureSources").mockImplementation(server.list),
    listProjects: spyOn(api, "listProjects").mockResolvedValue([
      core,
      { ...core, key: "OPS", name: "Operations" },
    ]),
    putArchitectureSource: spyOn(api, "putArchitectureSource").mockImplementation(server.put),
  };
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={queryClient}>
      <ArchitectureSourcesSection />
    </QueryClientProvider>
  );
  return {
    putArchitectureSource: stubs.putArchitectureSource,
    done: () => {
      cleanup();
      queryClient.clear();
      for (const stub of Object.values(stubs)) stub.mockRestore();
    },
  };
}

// INFRA-3135, R3: "Every list of options a reader picks from holds a text box that narrows the
// options." The Source project list's options are every project, which grow with the deployment:
// opened from its trigger it holds a text box of its own, a word typed there leaves only the
// projects it matches, Escape closes it with nothing picked, and a pick puts the focus back on
// the trigger and is the project the source is saved for.
test("the Source project list holds a text box that narrows the projects, and a pick saves the source there", async () => {
  const user = userEvent.setup();
  const view = renderSourceForm();
  try {
    const section = within(await screen.findByRole("region", { name: "Architecture sources" }));
    const trigger = await section.findByLabelText("Source project");
    const before = new Set(textBoxes());
    await user.click(trigger);
    const own = textBoxes().filter((box) => !before.has(box));
    expect(own, "the Source project list holds no text box of its own").toHaveLength(1);
    const list = listOf(own[0] as HTMLElement);
    expect(within(list).getAllByRole("option")).toHaveLength(2);
    await user.type(own[0] as HTMLElement, "oper");
    await waitFor(() =>
      expect(
        within(list).queryAllByRole("option", { name: /^CORE\b/ }),
        `"oper" leaves CORE in the Source project list`
      ).toHaveLength(0)
    );
    expect(within(list).getByRole("option", { name: "OPS · Operations" })).toBeDefined();
    await user.keyboard("{Escape}");
    expect(screen.queryByRole("listbox", { name: "Source project options" })).toBeNull();
    expect(trigger).toHaveFocus();
    expect(trigger).toHaveTextContent("Choose a project");

    await user.click(trigger);
    await user.type(within(panelOf(trigger)).getByRole("combobox"), "oper");
    await user.keyboard("{Enter}");
    expect(trigger).toHaveFocus();
    expect(trigger).toHaveTextContent("OPS · Operations");

    await user.click(section.getByLabelText("Source repository"));
    await user.paste("legion/arch");
    await user.click(section.getByRole("button", { name: "Add source" }));
    await waitFor(() =>
      expect(view.putArchitectureSource).toHaveBeenCalledWith(
        "OPS",
        "legion/arch",
        { branch: "main" },
        keyedAttempt
      )
    );
  } finally {
    view.done();
  }
});

// The Source project list is a button, which no `required` holds as the select it replaced held
// the form: with no project picked the form files nothing and says to choose one, as the browser
// said for the select, until a project is picked.
test("with no project picked, Add source files nothing and says to choose a project", async () => {
  const user = userEvent.setup();
  const view = renderSourceForm();
  try {
    const section = within(await screen.findByRole("region", { name: "Architecture sources" }));
    const trigger = await section.findByLabelText("Source project");
    expect(trigger).toHaveTextContent("Choose a project");
    await user.click(section.getByLabelText("Source repository"));
    await user.paste("legion/arch");
    await user.click(section.getByRole("button", { name: "Add source" }));
    await act(async () => {});
    expect(view.putArchitectureSource, "a source was saved for no project").not.toHaveBeenCalled();
    expect(section.getByRole("alert")).toHaveTextContent("Choose a project.");

    await pickFromLabelled(user, "Source project", "CORE · Core");
    expect(section.queryByRole("alert")).toBeNull();
    await user.click(section.getByRole("button", { name: "Add source" }));
    await waitFor(() =>
      expect(view.putArchitectureSource).toHaveBeenCalledWith(
        "CORE",
        "legion/arch",
        { branch: "main" },
        keyedAttempt
      )
    );
    expect(view.putArchitectureSource).toHaveBeenCalledTimes(1);
  } finally {
    view.done();
  }
});

/** The section over the sources `list` answers, its save and its removal answered by `put` and
 *  `remove`. Answers the section once it has drawn. */
async function sourcesOf(
  list: () => Promise<ArchitectureSource[]>,
  put: typeof api.putArchitectureSource,
  remove: typeof api.deleteArchitectureSource
) {
  const mocks = [
    spyOn(api, "listArchitectureSources").mockImplementation(list),
    spyOn(api, "listProjects").mockResolvedValue([core]),
    spyOn(api, "putArchitectureSource").mockImplementation(put),
    spyOn(api, "deleteArchitectureSource").mockImplementation(remove),
  ];
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={queryClient}>
      <ArchitectureSourcesSection />
    </QueryClientProvider>
  );
  const section = within(await screen.findByRole("region", { name: "Architecture sources" }));
  return {
    queryClient,
    restore: () => {
      cleanup();
      queryClient.clear();
      for (const mock of mocks) mock.mockRestore();
    },
    section,
  };
}

/** A fetch that failed, which the write is tried again after: it goes late, for the reader to
 *  stop. */
const dropped = () =>
  Promise.reject(new NetworkError("/api/v1/projects/CORE", new TypeError("Failed to fetch")));

test.each([
  { name: "Add source", label: "CORE's architecture source legion/arch" },
  {
    name: "Delete architecture source legion/arch for CORE",
    label: "CORE's architecture source legion/arch removed",
  },
  {
    name: "Refresh architecture source legion/arch for CORE",
    label: "Refresh of CORE's architecture source legion/arch",
  },
] as const)("$name is released at its write's deadline", async ({ name, label }) => {
  const sync = spyOn(api, "syncArchitectureSource").mockImplementation(dropped);
  const page = await sourcesOf(async () => [architectureSource], dropped, dropped);
  try {
    const button = await page.section.findByRole<HTMLButtonElement>("button", { name });
    if (name === "Add source") {
      const user = userEvent.setup();
      await pickFromLabelled(user, "Source project", "CORE · Core");
      await user.click(page.section.getByLabelText("Source repository"));
      await user.paste("legion/arch");
    }
    jest.useFakeTimers();
    const user = userOnFakeClock();
    const sent = () => writesOf(page.queryClient).rows().length;
    await heldUntilLate(user, button, { label, name, queryClient: page.queryClient, sent });
    await user.keyboard("{Enter}");
    await waitFor(() => expect(sent()).toBe(2));
  } finally {
    realClock();
    page.restore();
    sync.mockRestore();
  }
});

test("Delete hands focus to the preceding source's Delete after the re-read removes its row", async () => {
  const user = userEvent.setup();
  const before = { ...architectureSource, repo: "acme/infra" };
  const updatedList = Promise.withResolvers<ArchitectureSource[]>();
  let removed = false;
  const page = await sourcesOf(
    () => (removed ? updatedList.promise : Promise.resolve([before, architectureSource])),
    dropped,
    async () => {
      removed = true;
    }
  );
  try {
    const pressed = await page.section.findByRole("button", {
      name: "Delete architecture source legion/arch for CORE",
    });
    const preceding = page.section.getByRole("button", {
      name: "Delete architecture source acme/infra for CORE",
    });
    act(() => pressed.focus());
    await user.keyboard("{Enter}");
    await waitFor(() => expect(removed).toBe(true));
    // The re-read remains out past the frame and task a press with no hold waits.
    await afterFrameAndTasks();
    expect(pressed).toHaveFocus();
    await act(async () => updatedList.resolve([before]));
    await waitFor(() => expect(pressed).not.toBeInTheDocument());
    expect(preceding).toHaveFocus();
  } finally {
    await act(async () => updatedList.resolve([before]));
    page.restore();
  }
});

// The line a removal's list read the bound ended draws is that removal's own: the next Delete
// takes it away as its removal lands, though the earlier read is still out, and the read after
// it, once answered, takes the row away. Dispatch holds the first removal's read until the row
// answers it, so the next removal's read, which waits for that one (`DispatchQueryClient`),
// answers after it.
test("the line a removal's list read the bound ended draws goes as the next Delete lands, whose read takes the row away", async () => {
  const listArchitectureSources = spyOn(api, "listArchitectureSources").mockResolvedValue([
    architectureSource,
  ]);
  const listProjects = spyOn(api, "listProjects").mockResolvedValue([core]);
  // Each removal's answer, held until the row gives it, so what it sets off runs in `act`.
  const { answers: removals, next } = heldAnswers<void>();
  const deleteArchitectureSource = spyOn(api, "deleteArchitectureSource").mockImplementation(next);
  const queryClient = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={queryClient}>
      <ArchitectureSourcesSection />
    </QueryClientProvider>
  );
  const name = "Delete architecture source legion/arch for CORE";
  const note = /^Couldn't load architecture sources\./;
  try {
    const section = within(await screen.findByRole("region", { name: "Architecture sources" }));
    const remove = await section.findByRole("button", { name });
    const reread = Promise.withResolvers<ArchitectureSource[]>();
    listArchitectureSources.mockResolvedValue([]);
    listArchitectureSources.mockReturnValueOnce(reread.promise);
    jest.useFakeTimers();
    const user = userOnFakeClock();
    await user.click(remove);
    await tick(0);
    await act(async () => removals[0]?.());
    await tick(0);
    expect(remove).toHaveAttribute("aria-disabled", "true");
    await tick(REQUEST_DEADLINE_MS);
    expect(remove).not.toHaveAttribute("aria-disabled");
    expect(section.getByText(note).textContent).toBe(
      "Couldn't load architecture sources. Dispatch did not answer within 30 seconds."
    );

    await user.click(remove);
    await tick(0);
    expect(deleteArchitectureSource).toHaveBeenCalledTimes(2);
    await act(async () => removals[1]?.());
    await tick(0);
    expect(remove).toHaveAttribute("aria-disabled", "true");
    expect(
      section.queryByText(note),
      "the earlier removal's read kept its line past the next removal"
    ).toBeNull();

    await act(async () => reread.resolve([]));
    await tick(0);
    expect(section.queryByRole("button", { name })).toBeNull();
    expect(section.queryByText(note)).toBeNull();
  } finally {
    cleanup();
    queryClient.clear();
    realClock();
    deleteArchitectureSource.mockRestore();
    listProjects.mockRestore();
    listArchitectureSources.mockRestore();
  }
});

// A save or a removal the reader stopped may have been made: the section says so, and that its
// Retry sends the same change again - a `PUT` or a `DELETE` sends no key - rather than that it
// failed.
test("a source save the reader stopped says Retry sends it again, not that it failed", async () => {
  const server = sourceServer([]);
  const page = await sourcesOf(server.list, dropped, server.remove);
  try {
    await pickFromLabelled(userEvent.setup(), "Source project", "CORE · Core");
    jest.useFakeTimers();
    const user = userOnFakeClock();
    await user.click(page.section.getByLabelText("Source repository"));
    await user.paste("legion/arch");
    await user.click(page.section.getByRole("button", { name: "Add source" }));
    await stopTheLateWrite(page.queryClient);
    // The alert's paragraph, without its Retry button's name.
    expect(page.section.getByRole("alert").querySelector("p")?.textContent).toBe(
      stoppedWriteNote("the architecture source", "saved", {
        method: "PUT",
        stopped: new SendStoppedError(),
      })
    );
  } finally {
    realClock();
    page.restore();
  }
});

test("a source removal the reader stopped says Retry sends it again, not that it failed", async () => {
  const server = sourceServer([architectureSource]);
  const page = await sourcesOf(server.list, server.put, dropped);
  try {
    const remove = await page.section.findByRole("button", {
      name: "Delete architecture source legion/arch for CORE",
    });
    jest.useFakeTimers();
    await userOnFakeClock().click(remove);
    await stopTheLateWrite(page.queryClient);
    expect(page.section.getByRole("alert").querySelector("p")?.textContent).toBe(
      stoppedWriteNote("the removal of the architecture source legion/arch for CORE", "made", {
        method: "DELETE",
        stopped: new SendStoppedError(),
      })
    );
  } finally {
    realClock();
    page.restore();
  }
});

// A refresh the reader stopped may have been made: the section says so, and that its Retry asks
// again under the refresh's key - a `POST` sends one - rather than that it failed.
test("a source refresh the reader stopped says Retry asks again under its key", async () => {
  const server = sourceServer([architectureSource]);
  const sync = spyOn(api, "syncArchitectureSource").mockImplementation(dropped);
  const page = await sourcesOf(server.list, server.put, server.remove);
  try {
    const refresh = await page.section.findByRole("button", {
      name: "Refresh architecture source legion/arch for CORE",
    });
    jest.useFakeTimers();
    await userOnFakeClock().click(refresh);
    await stopTheLateWrite(page.queryClient);
    expect(page.section.getByRole("alert").querySelector("p")?.textContent).toBe(
      stoppedWriteNote("the refresh of the architecture source legion/arch for CORE", "made", {
        method: "POST",
        stopped: new SendStoppedError(),
      })
    );
  } finally {
    realClock();
    page.restore();
    sync.mockRestore();
  }
});

// A `DELETE` sends no key, so one made again after a removal that landed finds no source and is
// answered `404 SOURCE_NOT_FOUND`: the removal done, which the section takes as it takes a
// `204`, reading its sources again, not as a refusal.
test("a source removal answered SOURCE_NOT_FOUND is the removal done, not a refusal", async () => {
  const server = sourceServer([architectureSource]);
  const page = await sourcesOf(server.list, server.put, async (project, repo) => {
    await server.remove(project, repo);
    throw new ApiError(404, { code: "SOURCE_NOT_FOUND", error: "no architecture source" });
  });
  try {
    await userEvent.setup().click(
      await page.section.findByRole("button", {
        name: "Delete architecture source legion/arch for CORE",
      })
    );
    await waitFor(() =>
      expect(page.section.queryByRole("cell", { name: "legion/arch" })).not.toBeInTheDocument()
    );
    expect(page.section.queryByRole("alert")).not.toBeInTheDocument();
  } finally {
    page.restore();
  }
});

// A removal's and a refresh's refusals each sit under the table, one line for every row: each
// names the source it was about, and goes once that source leaves the list - removed in another
// tab, or by another person - rather than stand under sources it was never about.
test("a refused removal or refresh names its source, and goes once that source leaves", async () => {
  const server = sourceServer([architectureSource, { ...architectureSource, repo: "acme/infra" }]);
  const sync = spyOn(api, "syncArchitectureSource").mockRejectedValue(
    new ApiError(409, {
      code: "SOURCE_UNREADABLE",
      error: "the Dispatch GitHub App cannot read acme/infra",
    })
  );
  const page = await sourcesOf(server.list, server.put, async () => {
    throw new ApiError(403, { code: "FORBIDDEN", error: "not yours to remove" });
  });
  try {
    const user = userEvent.setup();
    await user.click(
      await page.section.findByRole("button", {
        name: "Refresh architecture source acme/infra for CORE",
      })
    );
    const refreshRefusal =
      "Couldn't refresh the architecture source acme/infra for CORE — the Dispatch GitHub App cannot read acme/infra";
    expect(await page.section.findByText(refreshRefusal)).toBeDefined();
    await user.click(
      page.section.getByRole("button", { name: "Delete architecture source legion/arch for CORE" })
    );
    const deleteRefusal = "Couldn't delete the architecture source legion/arch for CORE.";
    expect(await page.section.findByText(deleteRefusal)).toBeDefined();

    await server.remove("CORE", "acme/infra");
    await act(() => page.queryClient.invalidateQueries());
    await waitFor(() =>
      expect(page.section.queryByRole("cell", { name: "acme/infra" })).not.toBeInTheDocument()
    );
    expect(page.section.queryByText(refreshRefusal)).not.toBeInTheDocument();
    expect(page.section.getByText(deleteRefusal)).toBeDefined();

    await server.remove("CORE", "legion/arch");
    await act(() => page.queryClient.invalidateQueries());
    expect(await page.section.findByText("No architecture sources yet.")).toBeDefined();
    expect(page.section.queryByRole("alert")).not.toBeInTheDocument();
  } finally {
    page.restore();
    sync.mockRestore();
  }
});
