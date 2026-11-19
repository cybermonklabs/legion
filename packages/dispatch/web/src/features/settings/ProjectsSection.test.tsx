import { expect, jest, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
// biome-ignore lint/style/noRestrictedImports: the submit site below is a form submitted with no control pressed, which user-event has no form for
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { realClock, tick } from "../../__tests__/fake-clock";
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { userOnFakeClock } from "../../__tests__/user";
import { ApiError, api } from "../../api/client";
import type { Project, ProjectDefaultRepository } from "../../api/types";
import { ProjectsSection } from "./ProjectsSection";

const core: Project = {
  created_at: "2026-09-01T00:00:00Z",
  key: "CORE",
  name: "Core",
  open_asks: 0,
  gated_steps: [],
};

// Dispatch made the project and lost its answer (`409 KEY_COMMITTED`): the form lets go of the key
// and name and says what was made, the list is read again, and a second press - with nothing in
// the form - creates nothing, where the old draft would have tried the same project again.
test("a create answered KEY_COMMITTED clears the form, says the project was made, and a second press sends nothing", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const listProjects = spyOn(api, "listProjects").mockResolvedValue([core]);
  const listDefaults = spyOn(api, "listProjectDefaultAssignees").mockResolvedValue([]);
  // Held, and given inside `act` after the press, as packages/dispatch/AGENTS.md's act rule says.
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
