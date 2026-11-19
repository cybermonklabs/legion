import { expect, jest, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
// biome-ignore lint/style/noRestrictedImports: the submit site below is a form submitted with no control pressed, which user-event has no form for
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { realClock, settle, tick } from "../../__tests__/fake-clock";
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { userOnFakeClock } from "../../__tests__/user";
import { ApiError, api } from "../../api/client";
import { projectDefaultAssigneesQuery } from "../../api/queries";
import { DispatchQueryClient } from "../../api/query-client";
import type { Project, ProjectDefaultAssignee } from "../../api/types";
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

// A defaults read asked while a pick's save is out - after the press cancelled the reads out then
// - can reach Dispatch before the save and still be out when the save's answer lands. Landing
// after the answer, it would put back the person the pick replaced. Dispatch's own client, as
// the app runs, holds the refetch the save makes as it settles behind that read rather than
// cancelling it, so the read's answer is not dropped for the reader by the refetch either.
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
    .mockReturnValueOnce(freshRead.promise);
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

    // The read the save made as it settled answers what the save left.
    await waitFor(() => expect(listDefaults).toHaveBeenCalledTimes(3));
    await act(async () => {
      freshRead.resolve(bob);
    });
    await waitFor(() => expect(control.value).toBe("bob"));
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
