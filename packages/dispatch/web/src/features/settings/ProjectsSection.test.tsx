import { expect, jest, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
// biome-ignore lint/style/noRestrictedImports: the submit site below is a form submitted with no control pressed, which user-event has no form for
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";

import { realClock, tick } from "../../__tests__/fake-clock";
import { stopTheLateWrite } from "../../__tests__/stop-late-write";
import { userOnFakeClock } from "../../__tests__/user";
import { ApiError, api, NetworkError } from "../../api/client";
import type { Project } from "../../api/types";
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
  const createProject = spyOn(api, "createProject").mockRejectedValue(
    new ApiError(409, { code: "KEY_COMMITTED", error: "already made" })
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
    const key = screen.getByLabelText<HTMLInputElement>("Key");
    await user.click(key);
    await user.paste("qa");
    await user.click(screen.getByLabelText("Name"));
    await user.paste("Quality");
    await user.click(screen.getByRole("button", { name: "New project" }));

    const made = await screen.findByTestId("landed-without-answer");
    expect(made.textContent).toBe(
      "Project QA was made, but Dispatch lost its answer, so it may take a moment to appear."
    );
    expect(key.value).toBe("");
    expect(screen.getByLabelText<HTMLInputElement>("Name").value).toBe("");
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    await waitFor(() => expect(listProjects.mock.calls.length).toBeGreaterThan(1));

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

// A row's Retry is of the project's newest save. Here the reader stopped a save of alice, picked
// alice again and stopped that save too, so the two picks read alike: the Retry makes the newer
// save again under its own key, where Dispatch answers with what that save did, and never the
// older one, whose key Dispatch would answer with what an earlier pick did there.
test("Retry after two stopped saves of the same default assignee goes under the newer save's key", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const listProjects = spyOn(api, "listProjects").mockResolvedValue([core]);
  const listDefaults = spyOn(api, "listProjectDefaultAssignees").mockResolvedValue([]);
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
    await screen.findByRole("option", { name: "alice@example.com" });

    await user.selectOptions(control, "alice@example.com");
    await stopTheLateWrite(queryClient);
    const firstKey = putDefault.mock.calls[0]?.[2]?.key;

    await user.selectOptions(control, "alice@example.com");
    await tick(0);
    const secondKey = putDefault.mock.calls.at(-1)?.[2]?.key;
    expect(secondKey).not.toBe(firstKey);
    await stopTheLateWrite(queryClient);

    putDefault.mockResolvedValueOnce({ assignee: "alice@example.com", project: "CORE" });
    await user.click(screen.getByRole("button", { name: "Retry" }));
    await tick(0);
    expect(putDefault.mock.calls.at(-1)?.[2]?.key).toBe(secondKey);
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
