import { expect, jest, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
// biome-ignore lint/style/noRestrictedImports: the submit site below is a form submitted with no control pressed, which user-event has no form for
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { realClock, settle, tick } from "../../__tests__/fake-clock";
import { pressHeld } from "../../__tests__/held-press";
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { listOf, panelOf, pick, rows, textBoxes } from "../../__tests__/list-picks";
import { stopTheLateWrite } from "../../__tests__/stop-late-write";
import { userOnFakeClock } from "../../__tests__/user";
import { ApiError, api, NetworkError } from "../../api/client";
import { projectDefaultAssigneesQuery } from "../../api/queries";
import { DispatchQueryClient } from "../../api/query-client";
import type {
  AuthenticatedUser,
  DispatchUser,
  Project,
  ProjectDefaultAssignee,
  ProjectDefaultRepository,
} from "../../api/types";
import { SendStoppedError, stoppedWriteNote } from "../writes/keyed-write";
import { ProjectsSection } from "./ProjectsSection";

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

// While the create is out, Key, Name and New project are held, by `aria-disabled`, never
// `disabled` (`pressHeld`): the control the reader submitted from keeps the focus, and a second
// press sends nothing.
test.each([
  { label: "Key", role: "textbox" },
  { label: "Name", role: "textbox" },
  { label: "New project", role: "button" },
] as const)("$label, held while the create is out, keeps the focus, and a second press sends nothing", async ({
  label,
  role,
}) => {
  const user = userEvent.setup();
  const listProjects = spyOn(api, "listProjects").mockResolvedValue([core]);
  const listDefaults = spyOn(api, "listProjectDefaultAssignees").mockResolvedValue([]);
  // The create's answer, held until the case refuses it inside `act`.
  const answer = Promise.withResolvers<never>();
  const createProject = spyOn(api, "createProject").mockReturnValue(answer.promise);
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
    await pressHeld(user, screen.getByRole(role, { name: label }), {
      name: label,
      sent: () => createProject.mock.calls.length,
    });
  } finally {
    await act(async () => answer.reject(new ApiError(403, { code: "FORBIDDEN", error: "no" })));
    cleanup();
    queryClient.clear();
    listProjects.mockRestore();
    listDefaults.mockRestore();
    createProject.mockRestore();
  }
});

/** The section over CORE, which names alice until a save names another, with `readUsers`
 *  answering the people list and `whoAmI` the viewer; every create and default-assignee save is
 *  recorded. */
function renderSection({
  readUsers = () => Promise.resolve([{ login: "alice" }, { login: "bob" }, { login: "carol" }]),
  whoAmI = () => Promise.resolve({ event_head: "0", kind: "user", login: "alice" }),
}: {
  readUsers?: () => Promise<DispatchUser[]>;
  whoAmI?: () => Promise<AuthenticatedUser>;
} = {}) {
  let defaults = [{ assignee: "alice", project: "CORE" }];
  const stubs = {
    createProject: spyOn(api, "createProject").mockImplementation(async (input) => ({
      ...core,
      default_assignee: input.default_assignee ?? "alice",
      key: input.key,
      name: input.name,
    })),
    listDefaults: spyOn(api, "listProjectDefaultAssignees").mockImplementation(
      async () => defaults
    ),
    listProjects: spyOn(api, "listProjects").mockResolvedValue([core]),
    listUsers: spyOn(api, "listUsers").mockImplementation(readUsers),
    putDefault: spyOn(api, "putProjectDefaultAssignee").mockImplementation(
      async (project, { assignee }) => {
        defaults = [...defaults.filter((row) => row.project !== project), { assignee, project }];
        return { assignee, project };
      }
    ),
    whoAmI: spyOn(api, "whoAmI").mockImplementation(whoAmI),
  };
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={queryClient}>
      <ProjectsSection />
    </QueryClientProvider>
  );
  return {
    ...stubs,
    done: () => {
      cleanup();
      queryClient.clear();
      for (const stub of Object.values(stubs)) stub.mockRestore();
    },
  };
}

// INFRA-3135, R3: "Every list of options a reader picks from holds a text box that narrows the
// options." The New project form's Default assignee offers everyone who has signed in, who grow
// with the deployment: opened from its trigger it holds a text box of its own, says the people
// are loading while their read is out, then a word typed there leaves only the people it
// matches, and the pick is the person the project is created with, with the focus back on the
// trigger.
test("the New project form's Default assignee list holds a text box that narrows the people, and a pick is who the project names", async () => {
  const user = userEvent.setup();
  const people = Promise.withResolvers<DispatchUser[]>();
  const view = renderSection({ readUsers: () => people.promise });
  try {
    await screen.findByRole("cell", { name: "CORE" });
    const trigger = screen.getByLabelText("Default assignee");
    await waitFor(() => expect(trigger).toHaveTextContent("alice"));
    const before = new Set(textBoxes());
    await user.click(trigger);
    const own = textBoxes().filter((box) => !before.has(box));
    expect(
      own,
      "the New project form's Default assignee list holds no text box of its own"
    ).toHaveLength(1);
    const list = listOf(own[0] as HTMLElement);
    await user.type(own[0] as HTMLElement, "car");
    // Until the people are read the list holds the viewer alone, whom "car" leaves out.
    expect(list).toHaveTextContent("Loading people…");

    await act(async () => {
      people.resolve([{ login: "alice" }, { login: "bob" }, { login: "carol" }]);
      await people.promise;
    });
    await waitFor(() => expect(rows(list)).toEqual(["carol"]));
    await user.keyboard("{Enter}");
    expect(trigger).toHaveFocus();
    expect(trigger).toHaveTextContent("carol");

    await user.click(screen.getByLabelText("Key"));
    await user.paste("qa");
    await user.click(screen.getByLabelText("Name"));
    await user.paste("Quality");
    await user.click(screen.getByRole("button", { name: "New project" }));
    await waitFor(() =>
      expect(view.createProject).toHaveBeenCalledWith(
        { default_assignee: "carol", key: "QA", name: "Quality" },
        keyedAttempt
      )
    );
  } finally {
    view.done();
  }
});

// INFRA-3135, R3, for each project's Default assignee: the same people, in a list of its own per
// row. A word leaves only the people it matches, Escape closes the list with nothing picked and
// nothing saved, and a pick saves the project's default with the focus back on the trigger.
test("a project's Default assignee list holds a text box that narrows the people, and a pick saves the default", async () => {
  const user = userEvent.setup();
  const view = renderSection();
  try {
    const trigger = await screen.findByLabelText("Default assignee of CORE");
    await waitFor(() => expect(trigger).toHaveTextContent("alice"));
    const before = new Set(textBoxes());
    await user.click(trigger);
    const own = textBoxes().filter((box) => !before.has(box));
    expect(own, "the Default assignee of CORE list holds no text box of its own").toHaveLength(1);
    const list = listOf(own[0] as HTMLElement);
    await within(list).findByRole("option", { name: "carol" });
    await user.type(own[0] as HTMLElement, "bo");
    expect(rows(list)).toEqual(["bob"]);
    await user.keyboard("{Escape}");
    expect(screen.queryByRole("listbox", { name: "Default assignee of CORE options" })).toBeNull();
    expect(trigger).toHaveFocus();
    expect(trigger).toHaveTextContent("alice");

    await user.click(trigger);
    await user.type(within(panelOf(trigger)).getByRole("combobox"), "bo");
    await user.keyboard("{Enter}");
    expect(trigger).toHaveFocus();
    await waitFor(() => expect(trigger).toHaveTextContent("bob"));
    await waitFor(() =>
      expect(view.putDefault).toHaveBeenCalledWith("CORE", { assignee: "bob" }, keyedAttempt)
    );
    expect(view.putDefault).toHaveBeenCalledTimes(1);
  } finally {
    view.done();
  }
});

// A pick of the person a project already names saves nothing, as the select the list replaced
// fired nothing for the value it held; and a list whose people could not be read says so.
test("a project's Default assignee list says the people could not be loaded, and a pick of the person it names saves nothing", async () => {
  const user = userEvent.setup();
  const view = renderSection({ readUsers: () => Promise.reject(new Error("people unreachable")) });
  try {
    const trigger = await screen.findByLabelText("Default assignee of CORE");
    await waitFor(() => expect(trigger).toHaveTextContent("alice"));
    await user.click(trigger);
    const panel = panelOf(trigger);
    expect(await within(panel).findByText("Could not load the people list.")).toBeTruthy();
    expect(within(panel).queryByText("Loading people…")).toBeNull();
    await user.click(within(panel).getByRole("option", { name: "alice" }));
    expect(trigger).toHaveFocus();
    await act(async () => {});
    expect(view.putDefault).not.toHaveBeenCalled();
  } finally {
    view.done();
  }
});

// The Default assignee list is a button, which no `required` holds: while the viewer has not
// resolved it names nobody and takes no pick, and the form files no project then, whatever
// submits it.
test("with no default assignee picked, New project files nothing", async () => {
  const user = userEvent.setup();
  const view = renderSection({ whoAmI: () => new Promise(() => {}) });
  try {
    await screen.findByRole("cell", { name: "CORE" });
    expect(screen.getByLabelText("Default assignee")).toBeDisabled();
    await user.click(screen.getByLabelText("Key"));
    await user.paste("qa");
    await user.click(screen.getByLabelText("Name"));
    await user.paste("Quality");
    await user.click(screen.getByRole("button", { name: "New project" }));
    await act(async () => {});
    expect(view.createProject, "a project was created naming nobody").not.toHaveBeenCalled();
  } finally {
    view.done();
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
function renderOverServer(server: ProjectServer) {
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
  const { done } = renderOverServer(projectServer(withDefault));
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
  const { done, listProjects } = renderOverServer(server);
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
  const { done, listProjects } = renderOverServer(server);
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
  const { done } = renderOverServer(projectServer(withDefault));
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
  const { done } = renderOverServer(server);
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
    const control = await screen.findByLabelText("Default assignee of CORE");
    await waitFor(() => expect(control).toHaveTextContent("alice"));
    // Reaching for the picker reads who may be picked.
    await user.click(control);
    await waitFor(() =>
      expect(within(panelOf(control)).getByRole("option", { name: "bob" })).toBeDefined()
    );
    await user.click(within(panelOf(control)).getByRole("option", { name: "bob" }));
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
    expect(control).toHaveTextContent("bob");

    // That read reached Dispatch before the save did, and answers the person from before it.
    await act(async () => {
      staleRead.resolve(alice);
      await settle();
    });
    expect(control).toHaveTextContent("bob");
    expect(listDefaults).toHaveBeenCalledTimes(3);

    // The read asked again answers what the save left. The refetch the save made as it settled
    // waited behind it, and asks once it is in: Dispatch still holds the save then.
    await act(async () => {
      freshRead.resolve(bob);
    });
    await waitFor(() => expect(listDefaults).toHaveBeenCalledTimes(4));
    await waitFor(() => expect(queryClient.isFetching()).toBe(0));
    expect(control).toHaveTextContent("bob");
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
    const control = await screen.findByLabelText("Default assignee of CORE");
    await waitFor(() => expect(control).not.toBeDisabled());

    await pick(user, "Default assignee of CORE", "alice@example.com");
    await stopTheLateWrite(queryClient);
    // A `PUT` carries no `Idempotency-Key`: the key a call is handed in its bounds is the page's
    // own name for the press the call is of, and what these lines compare is which press it is.
    const firstPress = putDefault.mock.calls[0]?.[2]?.key;

    await pick(user, "Default assignee of CORE", "alice@example.com");
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
    const control = await screen.findByLabelText("Default assignee of CORE");
    await waitFor(() => expect(control).not.toBeDisabled());
    await pick(user, "Default assignee of CORE", "alice@example.com");
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
