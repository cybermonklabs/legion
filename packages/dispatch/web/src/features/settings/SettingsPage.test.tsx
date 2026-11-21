import { expect, jest, type Mock, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
// biome-ignore lint/style/noRestrictedImports: the in-the-step rows press and check in one synchronous step, which user-event's awaited press cannot give
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import type { UserEvent } from "@testing-library/user-event";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { realClock, settle } from "../../__tests__/fake-clock";
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { stopTheLateWrite } from "../../__tests__/stop-late-write";
import { userOnFakeClock } from "../../__tests__/user";
import { api, NetworkError } from "../../api/client";
import { DispatchQueryClient } from "../../api/query-client";
import type { RepoProject } from "../../api/types";
import { SendStoppedError, stoppedWriteNote } from "../writes/keyed-write";
import { SettingsPage } from "./SettingsPage";

/** Renders the page on `queryClient`, and hands that client back, which a case that stops a late
 *  write reaches as the shell's indicator does. */
function renderPage(
  queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
): QueryClient {
  render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <SettingsPage />
      </QueryClientProvider>
    </MemoryRouter>
  );
  return queryClient;
}

/** Every query `SettingsPage` and its child sections issue besides the credential pending and
 *  grants reads each test sets, resolved with empty fixtures so the page settles past its own
 *  loading states. */
function stubUnrelatedQueries() {
  return {
    getDeliverySettings: spyOn(api, "getDeliverySettings").mockResolvedValue(null),
    getLegionDaemon: spyOn(api, "getLegionDaemon").mockResolvedValue(null),
    getSignIns: spyOn(api, "getSignIns").mockResolvedValue({ signins: [] }),
    listAccessGroups: spyOn(api, "listAccessGroups").mockResolvedValue([]),
    listAgentTokens: spyOn(api, "listAgentTokens").mockResolvedValue([]),
    listArchitectureSources: spyOn(api, "listArchitectureSources").mockResolvedValue([]),
    listProjectDefaultAssignees: spyOn(api, "listProjectDefaultAssignees").mockResolvedValue([]),
    listProjects: spyOn(api, "listProjects").mockResolvedValue([]),
    listRepoProjects: spyOn(api, "listRepoProjects").mockResolvedValue([]),
    whoAmI: spyOn(api, "whoAmI").mockResolvedValue({
      event_head: "0",
      kind: "user",
      login: "alice",
    }),
  };
}

test("hides the Live grants section on a Dispatch with no secrets broker", async () => {
  const unrelated = stubUnrelatedQueries();
  const getCredentialPending = spyOn(api, "getCredentialPending").mockResolvedValue(null);

  try {
    renderPage();

    await waitFor(() => expect(getCredentialPending).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(screen.queryByText("Live grants")).not.toBeInTheDocument());
  } finally {
    cleanup();
    getCredentialPending.mockRestore();
    for (const spy of Object.values(unrelated)) {
      spy.mockRestore();
    }
  }
});

test("lists live grants once credential requests are configured, and Revoke is one plain POST", async () => {
  const user = userEvent.setup();
  const unrelated = stubUnrelatedQueries();
  const getCredentialPending = spyOn(api, "getCredentialPending").mockResolvedValue({
    pending: [],
  });
  const getCredentialGrants = spyOn(api, "getCredentialGrants").mockResolvedValue({
    grants: [
      {
        approver: "sami@example.com",
        created_at: "2026-09-30T00:00:00Z",
        decided_by: "sami@example.com",
        enrollment: {
          kind: "session",
          operator: "sami@example.com",
          runtime_id: "box-1",
          slot: null,
        },
        expires_at: "2026-09-30T01:00:00Z",
        grant_id: "grant-1",
        granted: "approval",
        names: ["DEEL_API_KEY"],
        record_id: "rec-1",
      },
    ],
  });
  const revokeCredentialGrant = spyOn(api, "revokeCredentialGrant").mockResolvedValue(undefined);

  try {
    renderPage();

    expect(await screen.findByText("Live grants")).toBeDefined();
    expect(await screen.findByText("DEEL_API_KEY")).toBeDefined();
    await user.click(screen.getByRole("button", { name: "Revoke" }));
    await waitFor(() => expect(revokeCredentialGrant).toHaveBeenCalledTimes(1));
    expect(revokeCredentialGrant).toHaveBeenCalledWith("grant-1", keyedAttempt);
  } finally {
    cleanup();
    getCredentialPending.mockRestore();
    getCredentialGrants.mockRestore();
    revokeCredentialGrant.mockRestore();
    for (const spy of Object.values(unrelated)) {
      spy.mockRestore();
    }
  }
});

// The list holds every grant of the viewer's sessions, so each row says how it was granted: by the
// policy without asking, or on the approval of a login it names, which can be another person's.
test("names how each grant was granted: automatically, or approved by whom", async () => {
  const unrelated = stubUnrelatedQueries();
  const getCredentialPending = spyOn(api, "getCredentialPending").mockResolvedValue({
    pending: [],
  });
  const enrollment = {
    kind: "session",
    operator: "sami@example.com",
    runtime_id: "example-host-devbox:4242:1",
    slot: null,
  };
  const getCredentialGrants = spyOn(api, "getCredentialGrants").mockResolvedValue({
    grants: [
      {
        approver: "mallory@example.com",
        created_at: "2026-09-30T00:00:00Z",
        decided_by: "mallory@example.com",
        enrollment,
        expires_at: "2026-09-30T01:00:00Z",
        grant_id: "grant-1",
        granted: "approval",
        names: ["DEEL_API_KEY"],
        record_id: "rec-1",
      },
      {
        approver: null,
        created_at: "2026-09-30T00:00:00Z",
        decided_by: null,
        enrollment,
        expires_at: "2026-09-30T01:00:00Z",
        grant_id: "grant-2",
        granted: "automatic",
        names: ["GWS_READ_TOKEN"],
        record_id: null,
      },
    ],
  });

  try {
    renderPage();

    expect(await screen.findByRole("columnheader", { name: "Granted" })).toBeDefined();
    expect(screen.getByRole("cell", { name: "Approved by mallory@example.com" })).toBeDefined();
    expect(screen.getByRole("cell", { name: "Automatically" })).toBeDefined();
    expect(screen.getAllByRole("button", { name: "Revoke" })).toHaveLength(2);
  } finally {
    cleanup();
    getCredentialPending.mockRestore();
    getCredentialGrants.mockRestore();
    for (const spy of Object.values(unrelated)) {
      spy.mockRestore();
    }
  }
});

// Two roles of one pod hold grants on one runtime id, so each row names its enrollment's slot; a
// grant on an enrollment with no slot names none.
test("two grants on one pod read apart by their slots", async () => {
  const unrelated = stubUnrelatedQueries();
  const getCredentialPending = spyOn(api, "getCredentialPending").mockResolvedValue({
    pending: [],
  });
  const grant = (grantId: string, slot: string | null) => ({
    approver: "sami@example.com",
    created_at: "2026-09-30T00:00:00Z",
    decided_by: "sami@example.com",
    enrollment: { kind: "pod", operator: "", runtime_id: "3f9c-pod-uid", slot },
    expires_at: "2026-09-30T01:00:00Z",
    grant_id: grantId,
    granted: "approval" as const,
    names: ["DEEL_API_KEY"],
    record_id: `rec-${grantId}`,
  });
  const getCredentialGrants = spyOn(api, "getCredentialGrants").mockResolvedValue({
    grants: [
      grant("grant-1", "implementer-g1"),
      grant("grant-2", "reviewer-g1"),
      grant("grant-3", null),
    ],
  });

  try {
    renderPage();

    expect(await screen.findByRole("cell", { name: /slot implementer-g1/ })).toBeDefined();
    expect(screen.getByRole("cell", { name: /slot reviewer-g1/ })).toBeDefined();
    const unslotted = screen.getAllByRole("cell", { name: "pod · 3f9c-pod-uid · —" });
    expect(unslotted).toHaveLength(1);
  } finally {
    cleanup();
    getCredentialPending.mockRestore();
    getCredentialGrants.mockRestore();
    for (const spy of Object.values(unrelated)) {
      spy.mockRestore();
    }
  }
});

// A grant that lasts until its session ends has no expiry, which the broker answers as null; its
// row says so rather than a time read from null (1970), and a grant with an expiry still shows it.
test("a grant with no expiry says it ends when its session ends", async () => {
  const unrelated = stubUnrelatedQueries();
  const getCredentialPending = spyOn(api, "getCredentialPending").mockResolvedValue({
    pending: [],
  });
  const grant = (grantId: string, expiresAt: string | null) => ({
    approver: null,
    created_at: "2026-09-30T00:00:00Z",
    decided_by: null,
    enrollment: { kind: "session", operator: "sami@example.com", runtime_id: "box-1", slot: null },
    expires_at: expiresAt,
    grant_id: grantId,
    granted: "automatic" as const,
    names: [grantId === "grant-1" ? "DEEL_API_KEY" : "GWS_READ_TOKEN"],
    record_id: null,
  });
  const getCredentialGrants = spyOn(api, "getCredentialGrants").mockResolvedValue({
    grants: [grant("grant-1", null), grant("grant-2", "2026-09-30T01:00:00Z")],
  });

  try {
    renderPage();

    expect(await screen.findByRole("cell", { name: "When the session ends" })).toBeDefined();
    expect(document.querySelectorAll("time[datetime='2026-09-30T01:00:00Z']")).toHaveLength(1);
    // Two Created times and the one grant's Expires time: the grant with no expiry shows none.
    expect(document.querySelectorAll("time")).toHaveLength(3);
  } finally {
    cleanup();
    getCredentialPending.mockRestore();
    getCredentialGrants.mockRestore();
    for (const spy of Object.values(unrelated)) {
      spy.mockRestore();
    }
  }
});

const mapping: RepoProject = {
  created_at: "2026-09-10T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  default: true,
  project: "CORE",
  renamed_to: null,
  repo: "owner/repo",
};

/** The repository mapping routes over rows a test holds (`mappingServer`). */
interface MappingServer {
  answer(): void;
  /** Makes `repo` its project's default, as another browser's save does. */
  elsewhere(repo: string): void;
  holdReads(): void;
  list(): Promise<RepoProject[]>;
  put(repo: string, input: Parameters<typeof api.putRepoProject>[1]): Promise<RepoProject>;
  releaseRead(): void;
  releaseReads(): void;
  remove(repo: string): Promise<void>;
}

/** The repository mapping routes' server state as the Settings page reaches it, with every write
 *  held until the row answers it (`answer`): the write reaches the rows only then, or, with
 *  `savedFirst`, at once, its answer alone held. Once `holdReads` is called, each read answers
 *  the rows as they stood when it was asked, but only as the row lets it go - the oldest first
 *  (`releaseRead`), or all (`releaseReads`, which also ends the holding) - as a read Dispatch
 *  took before a write and answers after it does. */
function mappingServer(
  initial: readonly RepoProject[],
  { savedFirst = false }: { readonly savedFirst?: boolean } = {}
): MappingServer {
  let rows = initial.map((row) => ({ ...row }));
  const answer = Promise.withResolvers<void>();
  const heldReads: (() => void)[] = [];
  let holding = false;
  // As the server decides it: the body's default, else the mapping's own while its project is
  // unchanged, else not the default; one default per project.
  const save = (repo: string, input: Parameters<typeof api.putRepoProject>[1]): RepoProject => {
    const existing = rows.find((row) => row.repo === repo);
    const saved: RepoProject = {
      ...mapping,
      default: input.default ?? (existing?.project === input.project ? existing.default : false),
      project: input.project,
      renamed_to: existing?.renamed_to ?? null,
      repo,
    };
    rows = [
      ...rows
        .filter((row) => row.repo !== repo)
        .map((row) =>
          saved.default && row.project === saved.project ? { ...row, default: false } : row
        ),
      saved,
    ];
    return { ...saved };
  };
  return {
    answer: () => answer.resolve(),
    elsewhere: (repo) => {
      const row = rows.find((candidate) => candidate.repo === repo);
      if (row !== undefined) save(repo, { default: true, project: row.project });
    },
    holdReads: () => {
      holding = true;
    },
    list: async (): Promise<RepoProject[]> => {
      const listed = rows.map((row) => ({ ...row }));
      if (holding) {
        const released = Promise.withResolvers<void>();
        heldReads.push(released.resolve);
        await released.promise;
      }
      return listed;
    },
    put: async (
      repo: string,
      input: Parameters<typeof api.putRepoProject>[1]
    ): Promise<RepoProject> => {
      if (savedFirst) {
        const saved = save(repo, input);
        await answer.promise;
        return saved;
      }
      await answer.promise;
      return save(repo, input);
    },
    releaseRead: () => heldReads.shift()?.(),
    releaseReads: () => {
      holding = false;
      for (const release of heldReads.splice(0)) release();
    },
    remove: async (repo: string): Promise<void> => {
      await answer.promise;
      rows = rows.filter((row) => row.repo !== repo);
    },
  };
}

/** The Settings page on Dispatch's own client, as the app runs, with its mappings read from
 *  `server` over the projects CORE and OPS, and `restore` to put every stub back. */
async function renderMappings(server: MappingServer): Promise<{
  deleteRepoProject: Mock<typeof api.deleteRepoProject>;
  listRepoProjects: Mock<typeof api.listRepoProjects>;
  putRepoProject: Mock<typeof api.putRepoProject>;
  queryClient: DispatchQueryClient;
  restore: () => void;
}> {
  const unrelated = stubUnrelatedQueries();
  unrelated.listRepoProjects.mockImplementation(server.list);
  unrelated.listProjects.mockResolvedValue(
    ["CORE", "OPS"].map((key) => ({
      created_at: "2026-09-01T00:00:00Z",
      gated_steps: [],
      key,
      name: key,
      open_asks: 0,
    }))
  );
  const getCredentialPending = spyOn(api, "getCredentialPending").mockResolvedValue(null);
  const putRepoProject = spyOn(api, "putRepoProject").mockImplementation((repo, input) =>
    server.put(repo, input)
  );
  const deleteRepoProject = spyOn(api, "deleteRepoProject").mockImplementation(server.remove);
  const queryClient = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  renderPage(queryClient);
  await screen.findByRole("cell", { name: "owner/repo" });
  return {
    deleteRepoProject,
    listRepoProjects: unrelated.listRepoProjects,
    putRepoProject,
    queryClient,
    restore: () => {
      cleanup();
      queryClient.clear();
      getCredentialPending.mockRestore();
      putRepoProject.mockRestore();
      deleteRepoProject.mockRestore();
      for (const spy of Object.values(unrelated)) {
        spy.mockRestore();
      }
    },
  };
}

/** Each repository mapping the page lists, as `repo → project`, top to bottom. */
function listedMappings(): string[] {
  const section = screen.getByRole("region", { name: "Repositories → Projects" });
  return within(section)
    .getAllByRole("row")
    .flatMap((row) => {
      const [repo, project] = Array.from(row.querySelectorAll("td"), (cell) => cell.textContent);
      return project === undefined ? [] : [`${repo} → ${project}`];
    });
}

/** Maps `owner/repo` to `project` through the page's form. */
async function mapThrough(user: UserEvent, project: string) {
  await user.click(screen.getByLabelText("Repository"));
  await user.paste("owner/repo");
  await user.selectOptions(screen.getByLabelText("Project"), project);
  await user.click(screen.getByRole("button", { name: "Add mapping" }));
}

/** Whether the page's Default radio for `repo` of CORE is set. */
function isDefault(repo: string): boolean {
  return screen.getByRole<HTMLInputElement>("radio", {
    name: `Default repository of CORE: ${repo}`,
  }).checked;
}

// Dispatch saves this page's Default pick at once, and another browser then makes the first
// repository the default again, which this page hears (an event refreshes the mappings) and shows
// while its own pick's answer is still on the way. That read is asked after Dispatch took the
// pick, so it answers the newer default; held back for the pick's answer, it would show the pick
// until then. Once the late answer lands, the page reads the mappings again rather than put the
// pick back.
test("a default another browser sets while this page's pick is out shows at once, and the pick's late answer does not undo it", async () => {
  const user = userEvent.setup();
  const other: RepoProject = { ...mapping, default: false, repo: "owner/other" };
  const server = mappingServer([mapping, other], { savedFirst: true });
  const page = await renderMappings(server);

  try {
    expect(isDefault("owner/repo")).toBe(true);
    await user.click(
      screen.getByRole("radio", { name: "Default repository of CORE: owner/other" })
    );
    await waitFor(() =>
      expect(page.putRepoProject).toHaveBeenCalledWith(
        "owner/other",
        { default: true, project: "CORE" },
        keyedAttempt
      )
    );
    expect(isDefault("owner/other")).toBe(true);

    // The other browser's save, and the event's refresh of this page's mappings.
    server.elsewhere("owner/repo");
    await act(async () => {
      void page.queryClient.invalidateQueries({ queryKey: ["repo-projects"] });
      await settle();
    });
    await waitFor(() => expect(isDefault("owner/repo")).toBe(true));
    expect(isDefault("owner/other")).toBe(false);

    const asked = page.listRepoProjects.mock.calls.length;
    await act(async () => {
      server.answer();
    });
    await waitFor(() => expect(page.listRepoProjects.mock.calls.length).toBeGreaterThan(asked));
    await waitFor(() => expect(page.queryClient.isFetching()).toBe(0));
    await waitFor(() => expect(page.queryClient.isMutating()).toBe(0));
    expect(isDefault("owner/repo")).toBe(true);
    expect(isDefault("owner/other")).toBe(false);
  } finally {
    page.restore();
  }
});

// A read of the mappings out when a save's answer lands, asked while the save was out, would put
// the project from before the press back over the one the reader just picked, if Dispatch took
// it before the save. Each read here answers the rows as they stood when it reached the server.
test("a read of the mappings out when a mapping's save is answered does not put back the old project", async () => {
  const user = userEvent.setup();
  const server = mappingServer([mapping]);
  const page = await renderMappings(server);

  try {
    await mapThrough(user, "OPS");
    await waitFor(() =>
      expect(page.putRepoProject).toHaveBeenCalledWith(
        "owner/repo",
        { project: "OPS" },
        keyedAttempt
      )
    );

    // Asked while the save is out, as an event's refresh would ask it.
    server.holdReads();
    await act(async () => {
      void page.queryClient.invalidateQueries({ queryKey: ["repo-projects"] });
      await settle();
    });
    await act(async () => {
      server.answer();
    });
    // The answer is in: the form is emptied for the next mapping.
    await waitFor(() => expect(screen.getByLabelText("Repository")).toHaveProperty("value", ""));
    expect(listedMappings()).toEqual(["owner/repo → OPS"]);

    // The oldest read still out answers.
    await act(async () => {
      server.releaseRead();
      await settle();
    });
    expect(listedMappings()).toEqual(["owner/repo → OPS"]);

    await act(async () => {
      server.releaseReads();
      await settle();
    });
    await waitFor(() => expect(page.queryClient.isFetching()).toBe(0));
    expect(listedMappings()).toEqual(["owner/repo → OPS"]);
  } finally {
    page.restore();
  }
});

// A mapping's save shows its row in the step of the press: whatever reads the mappings then - a
// second press, a Default pick, an event's refresh - reads the project just picked, not the one
// from before it.
test("a mapping's save is in the mappings' query in the step of the press", async () => {
  const user = userEvent.setup();
  const server = mappingServer([mapping]);
  const page = await renderMappings(server);

  try {
    await user.click(screen.getByLabelText("Repository"));
    await user.paste("owner/repo");
    await user.selectOptions(screen.getByLabelText("Project"), "OPS");
    // The form's own submit, and the check in the same step: user-event has no form for it.
    fireEvent.submit(screen.getByLabelText("Repository").closest("form") as HTMLFormElement);
    expect(
      page.queryClient
        .getQueryData<RepoProject[]>(["repo-projects"])
        ?.map((row) => `${row.repo} → ${row.project}`)
    ).toEqual(["owner/repo → OPS"]);

    await act(async () => {
      server.answer();
    });
    await waitFor(() => expect(page.queryClient.isMutating()).toBe(0));
    await waitFor(() => expect(page.queryClient.isFetching()).toBe(0));
    expect(listedMappings()).toEqual(["owner/repo → OPS"]);
  } finally {
    page.restore();
  }
});

// A mapping's removal takes its row out in the step of the press, as a save puts one in.
test("a mapping's removal is out of the mappings' query in the step of the press", async () => {
  const server = mappingServer([mapping]);
  const page = await renderMappings(server);

  try {
    // The press and the check in one synchronous step: user-event's awaited press has no form
    // for it.
    fireEvent.click(screen.getByRole("button", { name: "Delete mapping for owner/repo" }));
    expect(page.queryClient.getQueryData<RepoProject[]>(["repo-projects"])).toEqual([]);

    await act(async () => {
      server.answer();
    });
    await waitFor(() => expect(page.queryClient.isMutating()).toBe(0));
    await waitFor(() => expect(page.queryClient.isFetching()).toBe(0));
    expect(listedMappings()).toEqual([]);
  } finally {
    page.restore();
  }
});

// A mapping's removal the reader stopped may have been made. The page keeps what it says of a
// failed mapping write from the write's own error handler (`onError`), so a stopped one is told
// apart there: the page says what Retry does with it, not that the removal failed.
test("a mapping removal the reader stopped says Retry makes it again, not that it failed", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const unrelated = stubUnrelatedQueries();
  unrelated.listProjects.mockResolvedValue([
    {
      created_at: "2026-09-01T00:00:00Z",
      gated_steps: [],
      key: "CORE",
      name: "Core",
      open_asks: 0,
    },
  ]);
  unrelated.listRepoProjects.mockResolvedValue([
    {
      created_at: "2026-09-01T00:00:00Z",
      created_by: { id: "alice", kind: "user" },
      default: true,
      project: "CORE",
      renamed_to: null,
      repo: "acme/api",
    },
  ]);
  const getCredentialPending = spyOn(api, "getCredentialPending").mockResolvedValue(null);
  const deleteRepoProject = spyOn(api, "deleteRepoProject").mockRejectedValue(
    new NetworkError("/api/v1/repo-projects/acme/api", new TypeError("Failed to fetch"))
  );

  try {
    const queryClient = renderPage();
    // The identity reaches a page already drawn, which it updates at once, so inside `act`.
    act(() => {
      queryClient.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
    });
    await user.click(await screen.findByRole("button", { name: "Delete mapping for acme/api" }));
    await stopTheLateWrite(queryClient);

    // The alert's paragraph, without its Retry button's name.
    expect(screen.getByRole("alert").querySelector("p")?.textContent).toBe(
      stoppedWriteNote("the removal of acme/api's mapping", "made", {
        method: "DELETE",
        stopped: new SendStoppedError(),
      })
    );
  } finally {
    realClock();
    cleanup();
    getCredentialPending.mockRestore();
    deleteRepoProject.mockRestore();
    for (const spy of Object.values(unrelated)) {
      spy.mockRestore();
    }
  }
});

// A mapping's save the reader stopped may have been made: the page says so, and that its Retry
// sends the same `PUT` again, which is the same however often it is made.
test("a mapping save the reader stopped says Retry sends it again, not that it failed", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const unrelated = stubUnrelatedQueries();
  unrelated.listProjects.mockResolvedValue([
    {
      created_at: "2026-09-01T00:00:00Z",
      gated_steps: [],
      key: "CORE",
      name: "Core",
      open_asks: 0,
    },
  ]);
  unrelated.listRepoProjects.mockResolvedValue([
    {
      created_at: "2026-09-01T00:00:00Z",
      created_by: { id: "alice", kind: "user" },
      default: false,
      project: "CORE",
      renamed_to: null,
      repo: "acme/api",
    },
  ]);
  const getCredentialPending = spyOn(api, "getCredentialPending").mockResolvedValue(null);
  const putRepoProject = spyOn(api, "putRepoProject").mockRejectedValue(
    new NetworkError("/api/v1/repo-projects/acme/api", new TypeError("Failed to fetch"))
  );

  try {
    const queryClient = renderPage();
    // The identity reaches a page already drawn, which it updates at once, so inside `act`.
    act(() => {
      queryClient.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
    });
    await user.click(
      await screen.findByRole("radio", { name: "Default repository of CORE: acme/api" })
    );
    await stopTheLateWrite(queryClient);

    // The alert's paragraph, without its Retry button's name.
    expect(screen.getByRole("alert").querySelector("p")?.textContent).toBe(
      stoppedWriteNote("the mapping of acme/api", "saved", {
        method: "PUT",
        stopped: new SendStoppedError(),
      })
    );
  } finally {
    realClock();
    cleanup();
    getCredentialPending.mockRestore();
    putRepoProject.mockRestore();
    for (const spy of Object.values(unrelated)) {
      spy.mockRestore();
    }
  }
});

// Groups are made and kept in Settings, beside the projects whose Access tabs name them.
test("lists every group under Groups, right after Projects", async () => {
  const unrelated = stubUnrelatedQueries();
  unrelated.listAccessGroups.mockResolvedValue([
    { members: ["carol@example.com"], name: "crew", not_signed_in: [], owner: "alice" },
  ]);
  const getCredentialPending = spyOn(api, "getCredentialPending").mockResolvedValue(null);

  try {
    renderPage();

    expect(await screen.findByRole("cell", { name: "crew" })).toBeDefined();
    const headings = screen.getAllByRole("heading", { level: 2 }).map((h) => h.textContent);
    expect(headings.slice(0, 2)).toEqual(["Projects", "Groups"]);
  } finally {
    cleanup();
    getCredentialPending.mockRestore();
    for (const spy of Object.values(unrelated)) {
      spy.mockRestore();
    }
  }
});
