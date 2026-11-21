import { expect, jest, spyOn, test } from "bun:test";
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
import type { Project, ProjectDefaultAssignee } from "../../api/types";
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
