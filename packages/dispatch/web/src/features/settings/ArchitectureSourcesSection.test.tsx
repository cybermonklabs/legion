import { expect, jest, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { realClock } from "../../__tests__/fake-clock";
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { stopTheLateWrite } from "../../__tests__/stop-late-write";
import { userOnFakeClock } from "../../__tests__/user";
import { ApiError, api, NetworkError } from "../../api/client";
import type { ArchitectureSource } from "../../api/types";
import { SendStoppedError, stoppedWriteNote } from "../writes/keyed-write";
import { ArchitectureSourcesSection } from "./ArchitectureSourcesSection";

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

    await user.selectOptions(section.getByLabelText("Source project"), "CORE");
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
    await user.selectOptions(await section.findByLabelText("Source project"), "CORE");
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
      ).not.toHaveAttribute("disabled")
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
    await user.selectOptions(section.getByLabelText("Source project"), "CORE");
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

/** The section over the sources `list` answers, its save and its removal answered by `put` and
 *  `remove`. Answers the section once it has drawn. */
async function sourcesOf(
  list: () => Promise<ArchitectureSource[]>,
  put: typeof api.putArchitectureSource,
  remove: typeof api.deleteArchitectureSource
) {
  const mocks = [
    spyOn(api, "listArchitectureSources").mockImplementation(list),
    spyOn(api, "listProjects").mockResolvedValue([
      {
        created_at: "2026-09-01T00:00:00Z",
        gated_steps: [],
        key: "CORE",
        name: "Core",
        open_asks: 0,
      },
    ]),
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

// A save or a removal the reader stopped may have been made: the section says so, and that its
// Retry sends the same change again - a `PUT` or a `DELETE` sends no key - rather than that it
// failed.
test("a source save the reader stopped says Retry sends it again, not that it failed", async () => {
  const server = sourceServer([]);
  const page = await sourcesOf(server.list, dropped, server.remove);
  try {
    await userEvent
      .setup()
      .selectOptions(await page.section.findByLabelText("Source project"), "CORE");
    jest.useFakeTimers();
    const user = userOnFakeClock();
    await user.click(page.section.getByLabelText("Source repository"));
    await user.paste("legion/arch");
    await user.click(page.section.getByRole("button", { name: "Add source" }));
    await stopTheLateWrite(page.queryClient);
    // The alert's paragraph, without its Retry button's name.
    expect(page.section.getByRole("alert").querySelector("p")?.textContent).toBe(
      stoppedWriteNote("the architecture source", "saved", {
        again: "Retry",
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
      stoppedWriteNote("the removal of the architecture source", "made", {
        again: "Retry",
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
      stoppedWriteNote("the refresh of the architecture source", "made", {
        again: "Retry",
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
