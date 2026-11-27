import { beforeAll, expect, jest, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
// biome-ignore lint/style/noRestrictedImports: the submit site below is a form submitted with no control pressed, which user-event has no form for
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { realClock, settle, tick } from "../../__tests__/fake-clock";
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { stopTheLateWrite } from "../../__tests__/stop-late-write";
import { userOnFakeClock } from "../../__tests__/user";
import { ApiError, api, NetworkError } from "../../api/client";
import { projectDefaultAssigneesQuery } from "../../api/queries";
import { DispatchQueryClient } from "../../api/query-client";
import type { Project, ProjectDefaultAssignee, ProjectDefaultRepository } from "../../api/types";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { SendStoppedError, stoppedWriteNote } from "../writes/keyed-write";
import { ProjectsSection } from "./ProjectsSection";

// The rows draw through the shared renderer, whose Markdown engine loads lazily, and they run in
// random order, so the file loads the engine before its first row, whichever row that is.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

const core: Project = {
  created_at: "2026-09-01T00:00:00Z",
  key: "CORE",
  name: "Core",
  open_asks: 0,
  gated_steps: [],
  proven_sessions: false,
};

// Dispatch made the project and lost its answer (`409 KEY_COMMITTED`): the form lets go of the key
// and name and says what was made, the list is read again, and a second press - with nothing in
// the form - creates nothing, where the old draft would have tried the same project again.
test("a create answered KEY_COMMITTED clears the form, says the project was made, and a second press sends nothing", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const listProjects = spyOn(api, "listProjects").mockResolvedValue([core]);
  const listDefaults = spyOn(api, "listProjectDefaultAssignees").mockResolvedValue([]);
  // Held, and given inside `act` after the press, as docs/testing.md ("The act rule of a Dispatch
  // unit test") says.
  const lostCreate = Promise.withResolvers<never>();
  const createProject = spyOn(api, "createProject").mockReturnValue(lostCreate.promise);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  queryClient.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });

  try {
    render(
      <QueryClientProvider client={queryClient}>
        <ProjectsSection />
      </QueryClientProvider>
    );
    await screen.findByRole("cell", { name: "CORE" });
    const key = screen.getByLabelText<HTMLInputElement>("Key");
    await user.click(key);
    await user.paste("qa");
    await user.click(screen.getByLabelText("Name"));
    await user.paste("Quality");
    await user.click(screen.getByRole("button", { name: "New project" }));
    await act(async () => {
      lostCreate.reject(new ApiError(409, { code: "KEY_COMMITTED", error: "already made" }));
    });

    const made = await screen.findByTestId("landed-without-answer");
    expect(made.textContent).toBe(
      "Project QA was made, but Dispatch lost its answer, so it may take a moment to appear."
    );
    expect(key.value).toBe("");
    expect(screen.getByLabelText<HTMLInputElement>("Name").value).toBe("");
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    await waitFor(() => expect(listProjects.mock.calls.length).toBeGreaterThan(1));
    // The project list carries no default: the new project's is read from the defaults read.
    await waitFor(() => expect(listDefaults.mock.calls.length).toBeGreaterThan(1));

    // The form submitted with no control pressed: user-event has no form for submit.
    fireEvent.submit(key.closest("form") as HTMLFormElement);
    await tick(20);
    expect(createProject).toHaveBeenCalledTimes(1);
  } finally {
    realClock();
    cleanup();
    queryClient.clear();
    listProjects.mockRestore();
    listDefaults.mockRestore();
    createProject.mockRestore();
  }
});

/** The project routes' server state as the section reaches it: `list` answers what the default
 *  repository writes have left, and `put` stores the name in lower case, as the server does. A
 *  project with no default carries no `default_repo`. */
interface ProjectServer {
  list: () => Promise<Project[]>;
  put: (project: string, input: { repo: string | null }) => Promise<ProjectDefaultRepository>;
}

function projectServer(initial: Project[]): ProjectServer {
  let rows = initial.map((row) => ({ ...row }));
  return {
    list: async () => rows.map((row) => ({ ...row })),
    put: async (project, { repo }) => {
      const stored = repo === null ? null : repo.toLowerCase();
      rows = rows.map((row) => {
        if (row.key !== project) {
          return row;
        }
        const { default_repo: _previous, ...rest } = row;
        return stored === null ? rest : { ...rest, default_repo: stored };
      });
      return { project, repo: stored };
    },
  };
}

/** Renders the section over `server`'s projects, with no default assignee read for any of them.
 *  `done` unmounts it and restores the stubs. */
function renderSection(server: ProjectServer) {
  const listProjects = spyOn(api, "listProjects").mockImplementation(server.list);
  const listDefaults = spyOn(api, "listProjectDefaultAssignees").mockResolvedValue([]);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  queryClient.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  render(
    <QueryClientProvider client={queryClient}>
      <ProjectsSection />
    </QueryClientProvider>
  );
  return {
    done: () => {
      cleanup();
      queryClient.clear();
      listProjects.mockRestore();
      listDefaults.mockRestore();
    },
    listProjects,
  };
}

/** CORE with a default repository, and OPS with none. */
const withDefault: Project[] = [
  { ...core, default_repo: "acme/widgets" },
  { ...core, key: "OPS", name: "Operations" },
];

const repositoryBox = (project: string): Promise<HTMLInputElement> =>
  screen.findByRole<HTMLInputElement>("textbox", { name: `Default repository of ${project}` });

test("the default repository cell reads the project's default, and None for a project with none", async () => {
  const { done } = renderSection(projectServer(withDefault));
  try {
    expect(await screen.findByRole("columnheader", { name: "Default repository" })).toBeDefined();
    const coreBox = await repositoryBox("CORE");
    expect(coreBox.value).toBe("acme/widgets");
    expect(
      screen.getByRole("button", { name: "Clear the default repository of CORE" })
    ).toBeDefined();
    const opsBox = await repositoryBox("OPS");
    expect(opsBox.value).toBe("");
    expect(opsBox.placeholder).toBe("None");
    // Nothing to clear on a project with no default.
    expect(
      screen.queryByRole("button", { name: "Clear the default repository of OPS" })
    ).not.toBeInTheDocument();
  } finally {
    done();
  }
});

test("Save sends the typed owner/name to the project's default repository, and the cell reads what the server stored", async () => {
  const user = userEvent.setup();
  const server = projectServer(withDefault);
  const put = spyOn(api, "putProjectDefaultRepository").mockImplementation(server.put);
  const { done, listProjects } = renderSection(server);
  try {
    const opsBox = await repositoryBox("OPS");
    await user.click(opsBox);
    await user.paste("Acme/Gadgets");
    const reads = listProjects.mock.calls.length;
    await user.click(screen.getByRole("button", { name: "Save the default repository of OPS" }));
    await waitFor(() =>
      expect(put).toHaveBeenCalledWith("OPS", { repo: "Acme/Gadgets" }, keyedAttempt)
    );
    // The answer is the stored value, in lower case, and the project list is read again.
    await waitFor(() => expect(opsBox.value).toBe("acme/gadgets"));
    await waitFor(() => expect(listProjects.mock.calls.length).toBeGreaterThan(reads));
    expect(
      await screen.findByRole("button", { name: "Clear the default repository of OPS" })
    ).toBeDefined();
    expect(put).toHaveBeenCalledTimes(1);
  } finally {
    done();
    put.mockRestore();
  }
});

test("Clear sends null, and the cell reads None", async () => {
  const user = userEvent.setup();
  const server = projectServer(withDefault);
  const put = spyOn(api, "putProjectDefaultRepository").mockImplementation(server.put);
  const { done, listProjects } = renderSection(server);
  try {
    const coreBox = await repositoryBox("CORE");
    await waitFor(() => expect(coreBox.value).toBe("acme/widgets"));
    const reads = listProjects.mock.calls.length;
    await user.click(screen.getByRole("button", { name: "Clear the default repository of CORE" }));
    await waitFor(() => expect(put).toHaveBeenCalledWith("CORE", { repo: null }, keyedAttempt));
    await waitFor(() => expect(coreBox.value).toBe(""));
    expect(coreBox.placeholder).toBe("None");
    await waitFor(() => expect(listProjects.mock.calls.length).toBeGreaterThan(reads));
    expect(
      screen.queryByRole("button", { name: "Clear the default repository of CORE" })
    ).not.toBeInTheDocument();
  } finally {
    done();
    put.mockRestore();
  }
});

// A typo belongs at the box, not in a 400 a round trip later: the cell reads `owner/name` itself,
// sends nothing, and keeps what was typed for the reader to correct.
test("a default repository that is not owner/name is refused at the box and sends nothing", async () => {
  const user = userEvent.setup();
  const put = spyOn(api, "putProjectDefaultRepository");
  const { done } = renderSection(projectServer(withDefault));
  try {
    const opsBox = await repositoryBox("OPS");
    for (const typed of ["acme gadgets", "acme", "acme/gadgets/extra"]) {
      await user.clear(opsBox);
      await user.click(opsBox);
      await user.paste(typed);
      await user.click(screen.getByRole("button", { name: "Save the default repository of OPS" }));
      expect(await screen.findByRole("alert")).toHaveTextContent(
        "Write the repository as owner/name."
      );
      expect(opsBox.value).toBe(typed);
      expect(opsBox).toHaveAttribute("aria-invalid", "true");
    }
    expect(put).not.toHaveBeenCalled();
    // An edit takes the refusal off.
    await user.type(opsBox, "x");
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  } finally {
    done();
    put.mockRestore();
  }
});

// What the box lets through, the server decides on: its refusal shows under the cell, the box
// reads the stored default again, and Retry sends the refused value again.
test("a default repository the server refuses shows the server's reason under the cell", async () => {
  const user = userEvent.setup();
  const server = projectServer(withDefault);
  const put = spyOn(api, "putProjectDefaultRepository")
    .mockRejectedValueOnce(
      new ApiError(400, {
        code: "INVALID_PROJECT",
        error: 'repo must be "owner/name" (got "-acme/gadgets")',
      })
    )
    .mockImplementation(server.put);
  const { done } = renderSection(server);
  try {
    const coreBox = await repositoryBox("CORE");
    await waitFor(() => expect(coreBox.value).toBe("acme/widgets"));
    const cell = within(coreBox.closest("td") as HTMLElement);
    await user.clear(coreBox);
    await user.click(coreBox);
    await user.paste("-acme/gadgets");
    await user.click(cell.getByRole("button", { name: "Save the default repository of CORE" }));
    await waitFor(() =>
      expect(cell.getByRole("alert")).toHaveTextContent(
        'repo must be "owner/name" (got "-acme/gadgets")'
      )
    );
    await waitFor(() => expect(coreBox.value).toBe("acme/widgets"));

    await user.click(cell.getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(cell.queryByRole("alert")).not.toBeInTheDocument());
    await waitFor(() => expect(coreBox.value).toBe("-acme/gadgets"));
    expect(put.mock.calls).toEqual([
      ["CORE", { repo: "-acme/gadgets" }, keyedAttempt],
      ["CORE", { repo: "-acme/gadgets" }, keyedAttempt],
    ]);
  } finally {
    done();
    put.mockRestore();
  }
});

// A defaults read asked while a pick's save is out - after the press cancelled the reads out then
// - can reach Dispatch before the save and still be out when the save's answer lands. Landing
// after the answer, it would put back the person the pick replaced. The answer gives that read up
// and asks for the defaults again. Dispatch's own client, as the app runs, holds each refetch
// behind the read out rather than cancelling it, so no refetch drops that read's answer either.
test("a defaults read asked while a pick saved does not put back the assignee the pick replaced", async () => {
  const user = userEvent.setup();
  const alice: ProjectDefaultAssignee[] = [{ assignee: "alice", project: "CORE" }];
  const bob: ProjectDefaultAssignee[] = [{ assignee: "bob", project: "CORE" }];
  const staleRead = Promise.withResolvers<ProjectDefaultAssignee[]>();
  const freshRead = Promise.withResolvers<ProjectDefaultAssignee[]>();
  const saved = Promise.withResolvers<ProjectDefaultAssignee>();
  const listProjects = spyOn(api, "listProjects").mockResolvedValue([core]);
  const listDefaults = spyOn(api, "listProjectDefaultAssignees")
    .mockResolvedValueOnce(alice)
    .mockReturnValueOnce(staleRead.promise)
    .mockReturnValueOnce(freshRead.promise)
    .mockResolvedValueOnce(bob);
  const listUsers = spyOn(api, "listUsers").mockResolvedValue([
    { login: "alice" },
    { login: "bob" },
  ]);
  const putDefault = spyOn(api, "putProjectDefaultAssignee").mockReturnValue(saved.promise);
  const whoAmI = spyOn(api, "whoAmI").mockResolvedValue({
    event_head: "0",
    kind: "user",
    login: "alice",
  });
  const queryClient = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  queryClient.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });

  try {
    render(
      <QueryClientProvider client={queryClient}>
        <ProjectsSection />
      </QueryClientProvider>
    );
    const control = await screen.findByRole<HTMLSelectElement>("combobox", {
      name: "Default assignee of CORE",
    });
    await waitFor(() => expect(control.value).toBe("alice"));
    // Reaching for the picker reads who may be picked.
    await user.click(control);
    await waitFor(() => expect(within(control).getByRole("option", { name: "bob" })).toBeDefined());
    await user.selectOptions(control, "bob");
    await waitFor(() =>
      expect(putDefault).toHaveBeenCalledWith("CORE", { assignee: "bob" }, keyedAttempt)
    );

    // Asked while the save is out, as another surface's refresh would ask it.
    await act(async () => {
      void queryClient.invalidateQueries({ queryKey: projectDefaultAssigneesQuery().queryKey });
    });
    expect(listDefaults).toHaveBeenCalledTimes(2);
    // The save's answer gives that read up and asks for the defaults again, after the save.
    await act(async () => {
      saved.resolve({ assignee: "bob", project: "CORE" });
    });
    await waitFor(() => expect(queryClient.isMutating()).toBe(0));
    expect(control.value).toBe("bob");

    // That read reached Dispatch before the save did, and answers the person from before it.
    await act(async () => {
      staleRead.resolve(alice);
      await settle();
    });
    expect(control.value).toBe("bob");
    expect(listDefaults).toHaveBeenCalledTimes(3);

    // The read asked again answers what the save left. The refetch the save made as it settled
    // waited behind it, and asks once it is in: Dispatch still holds the save then.
    await act(async () => {
      freshRead.resolve(bob);
    });
    await waitFor(() => expect(listDefaults).toHaveBeenCalledTimes(4));
    await waitFor(() => expect(queryClient.isFetching()).toBe(0));
    expect(control.value).toBe("bob");
    expect(
      queryClient.getQueryData<ProjectDefaultAssignee[]>(projectDefaultAssigneesQuery().queryKey)
    ).toEqual(bob);
  } finally {
    cleanup();
    queryClient.clear();
    listProjects.mockRestore();
    listDefaults.mockRestore();
    listUsers.mockRestore();
    putDefault.mockRestore();
    whoAmI.mockRestore();
  }
});

// A row's Retry is of the project's newest save. Here the reader stopped a save of alice, picked
// alice again and stopped that save too, so the two picks read alike. A default assignee is a
// `PUT`, which sends no key, so Dispatch could not tell the two apart and keeps nothing under
// either: the key each call is handed is the page's own, a key of its own for each press, and
// what it shows here is which press the Retry made again - the newer save, the one the refusal
// on screen is about, and never the older one.
test("Retry after two stopped saves of the same default assignee makes the newer save again", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const listProjects = spyOn(api, "listProjects").mockResolvedValue([core]);
  // Every project names a person (#3707), so the control is read from a stored default.
  const listDefaults = spyOn(api, "listProjectDefaultAssignees").mockResolvedValue([
    { assignee: "bob@example.com", project: "CORE" },
  ]);
  const listUsers = spyOn(api, "listUsers").mockResolvedValue([{ login: "alice@example.com" }]);
  const putDefault = spyOn(api, "putProjectDefaultAssignee").mockRejectedValue(
    new NetworkError("/api/v1/projects/CORE/default-assignee", new TypeError("Failed to fetch"))
  );
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  queryClient.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });

  try {
    render(
      <QueryClientProvider client={queryClient}>
        <ProjectsSection />
      </QueryClientProvider>
    );
    const control = await screen.findByRole<HTMLSelectElement>("combobox", {
      name: "Default assignee of CORE",
    });
    await waitFor(() => expect(control.disabled).toBe(false));
    await user.hover(control);
    // The new-project form offers the same people, so the option is read inside this row.
    await within(control).findByRole("option", { name: "alice@example.com" });

    await user.selectOptions(control, "alice@example.com");
    await stopTheLateWrite(queryClient);
    // A `PUT` carries no `Idempotency-Key`: the key a call is handed in its bounds is the page's
    // own name for the press the call is of, and what these lines compare is which press it is.
    const firstPress = putDefault.mock.calls[0]?.[2]?.key;

    await user.selectOptions(control, "alice@example.com");
    await tick(0);
    const secondPress = putDefault.mock.calls.at(-1)?.[2]?.key;
    expect(secondPress).not.toBe(firstPress);
    await stopTheLateWrite(queryClient);

    // The Retry is a write of its own: the last call is otherwise the stopped save's own attempt,
    // which carries the newer key too, so a Retry that sent nothing would read as one that did.
    const sentBeforeRetry = putDefault.mock.calls.length;
    putDefault.mockResolvedValueOnce({ assignee: "alice@example.com", project: "CORE" });
    await user.click(screen.getByRole("button", { name: "Retry" }));
    await tick(0);
    expect(putDefault).toHaveBeenCalledTimes(sentBeforeRetry + 1);
    expect(putDefault.mock.calls.at(-1)?.[2]?.key).toBe(secondPress);
  } finally {
    realClock();
    cleanup();
    queryClient.clear();
    listProjects.mockRestore();
    listDefaults.mockRestore();
    listUsers.mockRestore();
    putDefault.mockRestore();
  }
});

// A create the reader stopped may have made the project, so the form does not say it failed. Its
// Retry makes that create again under its key (`retryLast`, then `retry`), and the form says what
// that does.
test("a new project the reader stopped says Retry makes it again, not that it failed", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const listProjects = spyOn(api, "listProjects").mockResolvedValue([core]);
  const listDefaults = spyOn(api, "listProjectDefaultAssignees").mockResolvedValue([]);
  const createProject = spyOn(api, "createProject").mockRejectedValue(
    new NetworkError("/api/v1/projects", new TypeError("Failed to fetch"))
  );
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  queryClient.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });

  try {
    render(
      <QueryClientProvider client={queryClient}>
        <ProjectsSection />
      </QueryClientProvider>
    );
    await screen.findByRole("cell", { name: "CORE" });
    await user.click(screen.getByLabelText("Key"));
    await user.paste("qa");
    await user.click(screen.getByLabelText("Name"));
    await user.paste("Quality");
    await user.click(screen.getByRole("button", { name: "New project" }));
    await stopTheLateWrite(queryClient);

    // The alert's paragraph, without its Retry button's name.
    expect(screen.getByRole("alert").querySelector("p")?.textContent).toBe(
      stoppedWriteNote("the new project", "made", {
        method: "POST",
        stopped: new SendStoppedError(),
      })
    );
  } finally {
    realClock();
    cleanup();
    queryClient.clear();
    listProjects.mockRestore();
    listDefaults.mockRestore();
    createProject.mockRestore();
  }
});

// The row reads its refusal from the mutation cache, not from the write's own result, so a save
// the reader stopped reaches it by that road: the row says the save may have been made and what
// its Retry does, not that it failed.
test("a default assignee save the reader stopped says Retry makes it again, not that it failed", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const listProjects = spyOn(api, "listProjects").mockResolvedValue([core]);
  // Every project names a person (#3707), so the control is read from a stored default.
  const listDefaults = spyOn(api, "listProjectDefaultAssignees").mockResolvedValue([
    { assignee: "bob@example.com", project: "CORE" },
  ]);
  const listUsers = spyOn(api, "listUsers").mockResolvedValue([{ login: "alice@example.com" }]);
  const putDefault = spyOn(api, "putProjectDefaultAssignee").mockRejectedValue(
    new NetworkError("/api/v1/projects/CORE/default-assignee", new TypeError("Failed to fetch"))
  );
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  queryClient.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });

  try {
    render(
      <QueryClientProvider client={queryClient}>
        <ProjectsSection />
      </QueryClientProvider>
    );
    const control = await screen.findByRole<HTMLSelectElement>("combobox", {
      name: "Default assignee of CORE",
    });
    await waitFor(() => expect(control.disabled).toBe(false));
    await user.hover(control);
    // The new-project form offers the same people, so the option is read inside this row.
    await within(control).findByRole("option", { name: "alice@example.com" });
    await user.selectOptions(control, "alice@example.com");
    await stopTheLateWrite(queryClient);

    expect(screen.getByRole("alert").querySelector("p")?.textContent).toBe(
      stoppedWriteNote("the default assignee", "saved", {
        method: "PUT",
        stopped: new SendStoppedError(),
      })
    );
  } finally {
    realClock();
    cleanup();
    queryClient.clear();
    listProjects.mockRestore();
    listDefaults.mockRestore();
    listUsers.mockRestore();
    putDefault.mockRestore();
  }
});
