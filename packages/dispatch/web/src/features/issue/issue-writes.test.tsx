import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
// biome-ignore lint/style/noRestrictedImports: this file keeps fireEvent for blur, submit, focus, one edit inside a single act batch, and one repeated identical selection
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { answerIssuePatches } from "../../__tests__/send-answers";
import { ApiError, api, NetworkError } from "../../api/client";
import type { Issue, IssueDetails, IssueSummary } from "../../api/types";
import type { IssueStatus } from "../project/board-model";
import { useBoardMoves } from "../project/board-moves";
import { projectIssuesQueryKey } from "../project/issue-filters";
import { writesOf } from "../writes/write-store";
import { blockedByControlName } from "./BlockedByControl";
import { IssuePage } from "./IssuePage";
import { PROPERTY_LABELS } from "./page-structure";
import { CONTROL_LABELS } from "./page-words";
import { issueUpdateLabel } from "./useIssueHeaderWrites";

const issue: IssueDetails = {
  route_status: null,
  route_holder: null,
  route_status_reason: null,
  holder_state: null,
  last_seen: null,
  takeable_at: null,
  artifacts: [
    {
      created_at: "2026-09-09T00:00:00Z",
      created_by: { id: "alice", kind: "user" },
      id: "artifact-1",
      issue_key: "CORE-1",
      project: "CORE",
      kind: "doc",
      name: "spec.md",
      primary: true,
      ref_key: "CORE-1/spec",
      session_id: null,
      slug: "spec",
      versions: [],
    },
  ],
  children: [],
  closed_at: null,
  created_at: "2026-09-09T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  design_gates: [],
  external_links: [],
  key: "CORE-1",
  labels: [],
  last_seq: 250,
  number: 1,
  open_asks: [],
  progress: { tasks: null, children: null },
  parent: null,
  blocked_by: [],
  assignee: null,
  claim: null,
  components: { mode: "inherit", ids: [], unknown: [], reason: null, inherited_from: null },
  plan_artifact_id: "",
  decisions_artifact_id: "",
  primary_artifact_id: "artifact-1",
  verification_artifact_id: "",
  project: "CORE",
  referenced_by_count: 0,
  route: null,
  repo: null,
  resolved_repo: null,
  status: "todo",
  priority: null,
  rank: "U",
  title: "Review the spec",
  updated_at: "2026-09-09T00:00:00Z",
};
function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((completion) => {
    resolve = completion;
  });
  return { promise, resolve };
}

function renderIssuePage() {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false, staleTime: Infinity } },
  });
  const view = render(
    <MemoryRouter initialEntries={["/issues/CORE-1"]}>
      <QueryClientProvider client={queryClient}>
        <Routes>
          <Route path="/issues/:key/*" element={<IssuePage />} />
        </Routes>
      </QueryClientProvider>
    </MemoryRouter>
  );
  return { queryClient, unmount: view.unmount };
}

function narrowIssue(details: IssueDetails): Issue {
  const { artifacts: _artifacts, children: _children, open_asks: _openAsks, ...rest } = details;
  return rest;
}

function stubIssueApi() {
  const getIssue = spyOn(api, "getIssue").mockResolvedValue(issue);
  const getIssueEvents = spyOn(api, "getIssueEvents").mockResolvedValue([]);
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([]);
  const getIssueSubscribers = spyOn(api, "getIssueSubscribers").mockResolvedValue([]);
  const getMyState = spyOn(api, "getMyState").mockResolvedValue({
    "CORE-1": { dismissed: [], last_read_seq: 0, pinned: false, seq: 0 },
  });
  // The header reads the project's repository mappings at once for an issue that resolves to
  // none, as this one does; a project that maps none shows no repository item.
  const listRepoProjects = spyOn(api, "listRepoProjects").mockResolvedValue([]);
  // The Spec tab's requirements and the header's component line read these for the issue under
  // test; both answer the API's empty shape, which is what this fixture's issue has.
  const getIssueCoverage = spyOn(api, "getIssueCoverage").mockResolvedValue({
    gaps: [],
    has_plan: false,
    issue: "CORE-1",
    requirements: [],
    spec_artifact_id: "",
  });
  const listProjectArchitectureSources = spyOn(
    api,
    "listProjectArchitectureSources"
  ).mockResolvedValue([]);
  // The one tree the page's body is reads these on every address (dispatch://INFRA-1196 R1),
  // and the conversation panel beside it reads the issue's asks, comments and messages. Each
  // answers the API's empty shape: these rows are about the header's writes, and a read that
  // reached the network would fail the file (`__tests__/network-guard.ts`).
  const treeReads = [
    spyOn(api, "getIssueDecisions").mockResolvedValue({
      decisions: [],
      document: null,
      open_questions: [],
    }),
    spyOn(api, "getIssueResults").mockResolvedValue({
      checks: [],
      issue: "CORE-1",
      verification_artifact_id: "",
    }),
    spyOn(api, "getIssueReferences").mockResolvedValue({ members: [], truncated: false }),
    spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] }),
    spyOn(api, "getArtifactBlocks").mockResolvedValue([]),
    spyOn(api, "getArtifactText").mockResolvedValue({ markdown: "", token: "", version: null }),
    spyOn(api, "listIssueAsks").mockResolvedValue([]),
    spyOn(api, "listComments").mockResolvedValue([]),
    spyOn(api, "listIssueMessages").mockResolvedValue([]),
  ];
  // The server answers a PATCH with the narrow `Issue`, not `IssueDetails`: no artifacts,
  // children, or open asks. The stub mirrors that so the page is exercised against the
  // real response shape.
  const patchIssue = answerIssuePatches().mockImplementation(
    // A null `status` or `title` in the body means "leave it as it is", which is what the server
    // does with it; every other field the body names is written through.
    async (_key, { rank: _rank, components: _components, status, title, ...update }) =>
      narrowIssue({
        ...issue,
        ...update,
        status: status ?? issue.status,
        title: title ?? issue.title,
      })
  );

  return {
    getIssue,
    patchIssue,
    restore: () => {
      getIssue.mockRestore();
      getIssueEvents.mockRestore();
      getInbox.mockRestore();
      getIssueSubscribers.mockRestore();
      getMyState.mockRestore();
      getIssueCoverage.mockRestore();
      listProjectArchitectureSources.mockRestore();
      listRepoProjects.mockRestore();
      for (const spy of treeReads) spy.mockRestore();
      patchIssue.mockRestore();
    },
  };
}

async function openTitleEditor(): Promise<HTMLInputElement> {
  const existing = screen.queryByLabelText("Issue title");
  if (existing instanceof HTMLInputElement) {
    return existing;
  }
  const user = userEvent.setup();
  await user.click(await screen.findByRole("heading", { level: 1 }));
  return (await screen.findByLabelText("Issue title")) as HTMLInputElement;
}

async function openRouteEditor(): Promise<HTMLInputElement> {
  const existing = screen.queryByLabelText("Message route");
  if (existing instanceof HTMLInputElement) {
    return existing;
  }
  const user = userEvent.setup();
  await user.click(await screen.findByRole("button", { name: /Messages default to/ }));
  return (await screen.findByLabelText("Message route")) as HTMLInputElement;
}

/** Opens the identity block's one actions control, which holds close, reopen and the control
 *  that sets the parent (dispatch://INFRA-1196 R11), and leaves it open if it already is. */
async function openIssueActions(): Promise<void> {
  const user = userEvent.setup();
  const control = await screen.findByRole("button", { name: "Issue actions" });
  if (control.getAttribute("aria-expanded") !== "true") {
    await user.click(control);
  }
}

test("IssuePage keeps an unsaved route draft when a stale refetch arrives", async () => {
  const user = userEvent.setup();
  const { patchIssue, restore } = stubIssueApi();
  const { unmount } = renderIssuePage();
  const firstSave = deferred<Issue>();
  patchIssue.mockImplementationOnce(() => firstSave.promise);

  try {
    await user.click(await screen.findByRole("button", { name: "Messages default to no route" }));
    let route = await openRouteEditor();
    let saveRoute = screen.getByRole("button", { name: "Save route" });
    await user.click(route);
    await user.paste("role:a");
    await user.click(saveRoute);
    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { route: "role:a" }, keyedAttempt)
    );
    route = await openRouteEditor();
    saveRoute = screen.getByRole("button", { name: "Save route" });

    await user.clear(route);
    await waitFor(() => expect(route.value).toBe(""));
    await act(async () => {
      firstSave.resolve(narrowIssue({ ...issue, route: "role:a" }));
    });
    await waitFor(() => expect(route.value).toBe(""));

    await user.click(saveRoute);
    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { route: "" }, keyedAttempt)
    );
    expect(route.value).toBe("");
  } finally {
    unmount();
    restore();
  }
});

test("IssuePage keeps a route cleared while the previous save was still in flight", async () => {
  const user = userEvent.setup();
  // The CI route-clear flake: the user clears the field while the earlier PATCH is
  // pending, and its response lands before React flushes the passive effects of the
  // clear. The success callback must see the cleared draft, not a stale mirror.
  const { patchIssue, restore } = stubIssueApi();
  const { unmount } = renderIssuePage();
  const firstSave = deferred<IssueDetails>();
  patchIssue.mockImplementationOnce(() => firstSave.promise);

  try {
    await user.click(await screen.findByRole("button", { name: "Messages default to no route" }));
    let route = await openRouteEditor();
    let saveRoute = screen.getByRole("button", { name: "Save route" });
    await user.click(route);
    await user.paste("role:a");
    await user.click(saveRoute);
    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { route: "role:a" }, keyedAttempt)
    );
    route = await openRouteEditor();
    saveRoute = screen.getByRole("button", { name: "Save route" });

    await act(async () => {
      // The clear and the pending save's response must land in one React flush, which is what
      // this act proves; user-event's own flush between them would undo the race.
      fireEvent.change(route, { target: { value: "" } });
      firstSave.resolve({ ...issue, route: "role:a" });
      await firstSave.promise;
      await Promise.resolve();
    });

    expect(route.value).toBe("");
    await user.click(saveRoute);
    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { route: "" }, keyedAttempt)
    );
  } finally {
    unmount();
    restore();
  }
});

test("IssuePage keeps the document and a route draft across a save response", async () => {
  const user = userEvent.setup();
  // A PATCH response is the narrow `Issue`. Applying it wholesale to the cached
  // `IssueDetails` drops `artifacts`, which swaps the whole page body for the
  // missing-document error and remounts the form on the next refetch with the
  // server's route in place of whatever the user had typed since.
  const { patchIssue, restore } = stubIssueApi();
  const { unmount } = renderIssuePage();

  try {
    await user.click(await screen.findByRole("button", { name: "Messages default to no route" }));
    let route = await openRouteEditor();
    await user.click(route);
    await user.paste("role:a");
    await user.click(screen.getByRole("button", { name: "Save route" }));
    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { route: "role:a" }, keyedAttempt)
    );
    route = await openRouteEditor();
    await user.clear(route);

    await waitFor(() =>
      expect(
        (screen.getByRole("button", { name: "Save route" }) as HTMLButtonElement).disabled
      ).toBe(false)
    );
    expect(
      screen.queryByText("Could not load this issue's primary document.")
    ).not.toBeInTheDocument();
    expect(screen.getByLabelText("Message route")).toBe(route);
    expect(route.value).toBe("");
  } finally {
    unmount();
    restore();
  }
});

test("IssuePage ignores a stale route refetch after a newer successful save", async () => {
  const user = userEvent.setup();
  const { getIssue, patchIssue, restore } = stubIssueApi();
  const { queryClient, unmount } = renderIssuePage();
  const staleRefetch = deferred<IssueDetails>();

  try {
    await user.click(await screen.findByRole("button", { name: "Messages default to no route" }));
    let route = await openRouteEditor();
    let saveRoute = screen.getByRole("button", { name: "Save route" });
    await user.click(route);
    await user.paste("role:a");
    await user.click(saveRoute);
    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { route: "role:a" }, keyedAttempt)
    );
    await waitFor(() =>
      expect(queryClient.getQueryData<IssueDetails>(["issue", "CORE-1"])?.route).toBe("role:a")
    );
    route = await openRouteEditor();
    saveRoute = screen.getByRole("button", { name: "Save route" });

    getIssue.mockImplementationOnce(() => staleRefetch.promise);
    act(() => {
      void queryClient.invalidateQueries({ queryKey: ["issue", "CORE-1"] });
    });
    await waitFor(() => expect(getIssue).toHaveBeenCalledTimes(2));

    await user.clear(route);
    await user.click(saveRoute);
    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { route: "" }, keyedAttempt)
    );
    await waitFor(() =>
      expect(queryClient.getQueryData<IssueDetails>(["issue", "CORE-1"])?.route).toBe("")
    );

    await act(async () => {
      staleRefetch.resolve({ ...issue, route: "role:a" });
    });
    await waitFor(() => expect(route.value).toBe(""));
  } finally {
    unmount();
    restore();
  }
});

test("IssuePage keeps an unsaved title draft when a stale refetch arrives", async () => {
  const user = userEvent.setup();
  const { patchIssue, restore } = stubIssueApi();
  const { unmount } = renderIssuePage();
  const firstSave = deferred<IssueDetails>();
  patchIssue.mockImplementationOnce(() => firstSave.promise);

  try {
    await user.click(await screen.findByRole("heading", { level: 1, name: "Review the spec" }));
    let title = (await screen.findByLabelText("Issue title")) as HTMLInputElement;
    await user.clear(title);
    await user.click(title);
    await user.paste("A");
    // Blur has no user-event form; it is what the title editor saves on.
    fireEvent.blur(title);
    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { title: "A" }, keyedAttempt)
    );

    // Blur exits click-to-edit mode; re-enter it to start a second, still-unsaved edit
    // before the first save's response arrives.
    await user.click(await screen.findByRole("heading", { level: 1, name: "A" }));
    title = (await screen.findByLabelText("Issue title")) as HTMLInputElement;
    await user.clear(title);
    await user.click(title);
    await user.paste("Edited locally");
    await act(async () => {
      firstSave.resolve({ ...issue, title: "A" });
    });
    await waitFor(() => expect(title.value).toBe("Edited locally"));

    // Blur has no user-event form; it is what the title editor saves on.
    fireEvent.blur(title);
    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith(
        "CORE-1",
        { title: "Edited locally" },
        keyedAttempt
      )
    );
    expect(title.value).toBe("Edited locally");
  } finally {
    unmount();
    restore();
  }
});

test("IssuePage ignores a same-task duplicate title save", async () => {
  const user = userEvent.setup();
  const { patchIssue, restore } = stubIssueApi();
  const { unmount } = renderIssuePage();

  try {
    const title = await openTitleEditor();
    await user.clear(title);
    await user.click(title);
    await user.paste("Guarded title");
    // Blur has no user-event form, and the pair is the point: two saves in one task.
    fireEvent.blur(title);
    fireEvent.blur(title);

    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(1));
    expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { title: "Guarded title" }, keyedAttempt);
  } finally {
    unmount();
    restore();
  }
});

test("IssuePage ignores same-task duplicate route saves", async () => {
  const user = userEvent.setup();
  const { patchIssue, restore } = stubIssueApi();
  const { unmount } = renderIssuePage();

  try {
    const route = await openRouteEditor();
    const saveRoute = screen.getByRole("button", { name: "Save route" });
    const routeForm = saveRoute.closest("form");
    if (routeForm === null) {
      throw new Error("Save route must be inside a form.");
    }
    await user.click(route);
    await user.paste("role:guarded");
    // Submit has no user-event form, and the pair is the point: two saves in one task.
    fireEvent.submit(routeForm);
    fireEvent.submit(routeForm);

    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(1));
    expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { route: "role:guarded" }, keyedAttempt);
  } finally {
    unmount();
    restore();
  }
});

test("IssuePage ignores a same-task duplicate status save", async () => {
  const { patchIssue, restore } = stubIssueApi();
  const { unmount } = renderIssuePage();

  try {
    const status = (await screen.findByLabelText("Status")) as HTMLSelectElement;
    // The same selection delivered twice in one task is what the guard under test ignores, and
    // user-event's `selectOptions` cannot deliver a selection the control already holds.
    fireEvent.change(status, { target: { value: "backlog" } });
    fireEvent.change(status, { target: { value: "backlog" } });

    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(1));
    expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { status: "backlog" }, keyedAttempt);
  } finally {
    unmount();
    restore();
  }
});

test("IssuePage submits the selected priority through the issue mutation", async () => {
  const user = userEvent.setup();
  const { patchIssue, restore } = stubIssueApi();
  const { unmount } = renderIssuePage();

  try {
    const priority = (await screen.findByLabelText("Priority of CORE-1")) as HTMLSelectElement;
    expect([...priority.options].map((option) => option.value)).toEqual(["", "0", "1", "2", "3"]);
    await user.selectOptions(priority, "0");

    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { priority: 0 }, keyedAttempt)
    );
  } finally {
    unmount();
    restore();
  }
});

test("IssuePage keeps an optimistic priority selection while its save is pending", async () => {
  const user = userEvent.setup();
  const { patchIssue, restore } = stubIssueApi();
  const save = Promise.withResolvers<Issue>();
  patchIssue.mockImplementationOnce(() => save.promise);
  const { unmount } = renderIssuePage();

  try {
    const priority = (await screen.findByLabelText("Priority of CORE-1")) as HTMLSelectElement;
    await user.selectOptions(priority, "0");
    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { priority: 0 }, keyedAttempt)
    );

    expect(priority.value).toBe("0");

    await act(async () => {
      save.resolve({ ...issue, priority: 0 });
      await save.promise;
    });
  } finally {
    unmount();
    restore();
  }
});

test("IssuePage restores the previous priority after a failed save", async () => {
  const user = userEvent.setup();
  const { patchIssue, restore } = stubIssueApi();
  const save = Promise.withResolvers<Issue>();
  patchIssue.mockImplementationOnce(() => save.promise);
  const { unmount } = renderIssuePage();

  try {
    const priority = (await screen.findByLabelText("Priority of CORE-1")) as HTMLSelectElement;
    await user.selectOptions(priority, "0");
    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { priority: 0 }, keyedAttempt)
    );
    expect(priority.value).toBe("0");

    act(() => {
      save.reject(new Error("offline"));
    });
    await screen.findByRole("alert");
    expect(priority.value).toBe("");
    expect(priority.disabled).toBe(false);
  } finally {
    unmount();
    restore();
  }
});

test("IssuePage preserves an edit made after a failed title save and retries that draft", async () => {
  const user = userEvent.setup();
  const { patchIssue, restore } = stubIssueApi();
  const failedSave = Promise.withResolvers<Issue>();
  patchIssue.mockImplementationOnce(() => failedSave.promise);
  const { unmount } = renderIssuePage();

  try {
    let title = await openTitleEditor();
    await user.clear(title);
    await user.click(title);
    await user.paste("a");
    // Blur has no user-event form; it is what the title editor saves on.
    fireEvent.blur(title);
    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { title: "a" }, keyedAttempt)
    );

    title = await openTitleEditor();
    await user.clear(title);
    await user.click(title);
    await user.paste("ab");
    act(() => {
      failedSave.reject(new Error("offline"));
    });
    await screen.findByRole("alert");
    expect(title.value).toBe("ab");

    await user.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { title: "ab" }, keyedAttempt)
    );
    expect(title.value).toBe("ab");
  } finally {
    unmount();
    restore();
  }
});

test("IssuePage does not retry an invalid route draft after a failed save", async () => {
  const user = userEvent.setup();
  const { patchIssue, restore } = stubIssueApi();
  const failedSave = Promise.withResolvers<Issue>();
  patchIssue.mockImplementationOnce(() => failedSave.promise);
  const { unmount } = renderIssuePage();

  try {
    let route = await openRouteEditor();
    let saveRoute = screen.getByRole("button", { name: "Save route" });
    let routeForm = saveRoute.closest("form");
    if (routeForm === null) {
      throw new Error("Save route must be inside a form.");
    }
    await user.click(route);
    await user.paste("role:a");
    // Submit has no user-event form; the route form saves on its own submit.
    fireEvent.submit(routeForm);
    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { route: "role:a" }, keyedAttempt)
    );

    act(() => {
      failedSave.reject(new Error("offline"));
    });
    await screen.findByRole("alert");
    route = await openRouteEditor();
    await user.clear(route);
    await user.click(route);
    await user.paste("not-a-route");
    await user.click(screen.getByRole("button", { name: "Retry" }));
    expect(patchIssue).toHaveBeenCalledTimes(1);

    saveRoute = screen.getByRole("button", { name: "Save route" });
    routeForm = saveRoute.closest("form");
    if (routeForm === null) {
      throw new Error("Save route must be inside a form.");
    }
    await user.clear(route);
    await user.click(route);
    await user.paste("role:b");
    // Submit has no user-event form; the route form saves on its own submit.
    fireEvent.submit(routeForm);
    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { route: "role:b" }, keyedAttempt)
    );
  } finally {
    unmount();
    restore();
  }
});

test("IssuePage does not retry an empty title draft after a failed save", async () => {
  const user = userEvent.setup();
  const { patchIssue, restore } = stubIssueApi();
  const failedSave = Promise.withResolvers<Issue>();
  patchIssue.mockImplementationOnce(() => failedSave.promise);
  const { unmount } = renderIssuePage();

  try {
    let title = await openTitleEditor();
    await user.clear(title);
    await user.click(title);
    await user.paste("a");
    // Blur has no user-event form; it is what the title editor saves on.
    fireEvent.blur(title);
    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { title: "a" }, keyedAttempt)
    );

    act(() => {
      failedSave.reject(new Error("offline"));
    });
    await screen.findByRole("alert");
    title = await openTitleEditor();
    await user.clear(title);
    await user.click(screen.getByRole("button", { name: "Retry" }));
    expect(patchIssue).toHaveBeenCalledTimes(1);

    // Pressing Retry takes focus off the title, which closes click-to-edit as it does in a
    // browser: the next edit re-opens the editor.
    title = await openTitleEditor();
    await user.click(title);
    await user.paste("b");
    // Blur has no user-event form; it is what the title editor saves on.
    fireEvent.blur(title);
    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { title: "b" }, keyedAttempt)
    );
  } finally {
    unmount();
    restore();
  }
});

test("IssuePage queues a title draft edited while a save is pending", async () => {
  const user = userEvent.setup();
  const { patchIssue, restore } = stubIssueApi();
  const firstSave = Promise.withResolvers<Issue>();
  patchIssue.mockImplementationOnce(() => firstSave.promise);
  const { unmount } = renderIssuePage();

  try {
    let title = await openTitleEditor();
    await user.clear(title);
    await user.click(title);
    await user.paste("a");
    // Blur has no user-event form; it is what the title editor saves on.
    fireEvent.blur(title);
    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { title: "a" }, keyedAttempt)
    );

    title = await openTitleEditor();
    await user.clear(title);
    await user.click(title);
    await user.paste("ab");
    await act(async () => {});
    // Blur has no user-event form; it is what the title editor saves on.
    fireEvent.blur(title);
    act(() => {
      firstSave.resolve({ ...issue, title: "a" });
    });

    await waitFor(() =>
      expect(patchIssue.mock.calls).toEqual([
        ["CORE-1", { title: "a" }, keyedAttempt],
        ["CORE-1", { title: "ab" }, keyedAttempt],
      ])
    );
    expect(title.value).toBe("ab");
  } finally {
    unmount();
    restore();
  }
});

test("IssuePage queues a route draft edited while a save is pending", async () => {
  const user = userEvent.setup();
  const { patchIssue, restore } = stubIssueApi();
  const firstSave = Promise.withResolvers<Issue>();
  patchIssue.mockImplementationOnce(() => firstSave.promise);
  const { unmount } = renderIssuePage();

  try {
    let route = await openRouteEditor();
    let saveRoute = screen.getByRole("button", { name: "Save route" });
    let routeForm = saveRoute.closest("form");
    if (routeForm === null) {
      throw new Error("Save route must be inside a form.");
    }
    await user.click(route);
    await user.paste("role:a");
    // Submit has no user-event form; the route form saves on its own submit.
    fireEvent.submit(routeForm);
    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { route: "role:a" }, keyedAttempt)
    );
    route = await openRouteEditor();
    saveRoute = screen.getByRole("button", { name: "Save route" });
    routeForm = saveRoute.closest("form");
    if (routeForm === null) {
      throw new Error("Save route must be inside a form.");
    }

    await user.clear(route);
    await user.click(route);
    await user.paste("role:b");
    await act(async () => {});
    // Submit has no user-event form; the route form saves on its own submit.
    fireEvent.submit(routeForm);
    act(() => {
      firstSave.resolve({ ...issue, route: "role:a" });
    });

    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { route: "role:b" }, keyedAttempt)
    );
    expect(route.value).toBe("role:b");
  } finally {
    unmount();
    restore();
  }
});

test("IssuePage drains a title draft after a different issue write settles", async () => {
  const user = userEvent.setup();
  const { patchIssue, restore } = stubIssueApi();
  const firstSave = Promise.withResolvers<Issue>();
  patchIssue.mockImplementationOnce(() => firstSave.promise);
  const { unmount } = renderIssuePage();

  try {
    const status = (await screen.findByLabelText("Status")) as HTMLSelectElement;
    await user.selectOptions(status, "backlog");
    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { status: "backlog" }, keyedAttempt)
    );

    // The status select takes focus when it is used, which would blur (and submit) an open title
    // editor as a click does in a browser; open the editor after that write is away.
    const title = await openTitleEditor();
    await user.clear(title);
    await user.click(title);
    await user.paste("Queued title");
    // Blur has no user-event form; it is what the title editor saves on.
    fireEvent.blur(title);
    act(() => {
      firstSave.resolve({ ...issue, status: "backlog" });
    });

    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { title: "Queued title" }, keyedAttempt)
    );
    expect(patchIssue).toHaveBeenCalledTimes(2);
  } finally {
    unmount();
    restore();
  }
});

test("IssuePage does not submit a route that was typed but never saved", async () => {
  const user = userEvent.setup();
  const { patchIssue, restore } = stubIssueApi();
  const statusSave = Promise.withResolvers<Issue>();
  patchIssue.mockImplementationOnce(() => statusSave.promise);
  const { unmount } = renderIssuePage();

  try {
    const status = (await screen.findByLabelText("Status")) as HTMLSelectElement;
    const route = await openRouteEditor();
    await user.selectOptions(status, "backlog");
    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { status: "backlog" }, keyedAttempt)
    );

    await user.click(route);
    await user.paste("role:legion");
    act(() => {
      statusSave.resolve({ ...issue, status: "backlog" });
    });
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(1));
    expect(route.value).toBe("role:legion");
  } finally {
    unmount();
    restore();
  }
});

test("IssuePage retries an unchanged failed title draft", async () => {
  const user = userEvent.setup();
  const { patchIssue, restore } = stubIssueApi();
  const failedSave = Promise.withResolvers<Issue>();
  patchIssue.mockImplementationOnce(() => failedSave.promise);
  const { unmount } = renderIssuePage();

  try {
    await user.click(await screen.findByRole("heading", { level: 1, name: "Review the spec" }));
    const title = (await screen.findByLabelText("Issue title")) as HTMLInputElement;
    await user.clear(title);
    await user.click(title);
    await user.paste("a");
    // Blur has no user-event form; it is what the title editor saves on.
    fireEvent.blur(title);
    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { title: "a" }, keyedAttempt)
    );

    act(() => {
      failedSave.reject(new Error("offline"));
    });
    await screen.findByRole("alert");
    expect(title.value).toBe("a");
    await user.click(screen.getByRole("button", { name: "Retry" }));

    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { title: "a" }, keyedAttempt)
    );
    expect(patchIssue).toHaveBeenCalledTimes(2);
  } finally {
    unmount();
    restore();
  }
});

test("IssuePage renders the server's reason when a title save is refused, with no Retry", async () => {
  const user = userEvent.setup();
  const { patchIssue, restore } = stubIssueApi();
  const refusal = "title is 1 characters over the 1000-character limit (1001/1000)";
  patchIssue.mockImplementationOnce(() =>
    Promise.reject(new ApiError(400, { code: "CAP_EXCEEDED", error: refusal }))
  );
  const { unmount } = renderIssuePage();

  try {
    const title = await openTitleEditor();
    await user.clear(title);
    await user.click(title);
    await user.paste("Renamed");
    // Blur has no user-event form; it is what the title editor saves on.
    fireEvent.blur(title);
    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { title: "Renamed" }, keyedAttempt)
    );

    await screen.findByText(refusal);
    expect(screen.queryByText("Could not update this issue.")).not.toBeInTheDocument();
    // Dispatch refuses the same title every time, so nothing offers to send it again.
    expect(screen.queryByRole("button", { name: "Retry" })).not.toBeInTheDocument();
  } finally {
    unmount();
    restore();
  }
});

test("IssuePage retries an unchanged failed route draft", async () => {
  const user = userEvent.setup();
  const { patchIssue, restore } = stubIssueApi();
  const failedSave = Promise.withResolvers<Issue>();
  patchIssue.mockImplementationOnce(() => failedSave.promise);
  const { unmount } = renderIssuePage();

  try {
    const route = await openRouteEditor();
    const routeForm = screen.getByRole("button", { name: "Save route" }).closest("form");
    if (routeForm === null) {
      throw new Error("Save route must be inside a form.");
    }
    await user.click(route);
    await user.paste("role:a");
    // Submit has no user-event form; the route form saves on its own submit.
    fireEvent.submit(routeForm);
    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { route: "role:a" }, keyedAttempt)
    );

    act(() => {
      failedSave.reject(new Error("offline"));
    });
    await screen.findByRole("alert");
    expect(route.value).toBe("role:a");
    await user.click(screen.getByRole("button", { name: "Retry" }));

    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { route: "role:a" }, keyedAttempt)
    );
    expect(patchIssue).toHaveBeenCalledTimes(2);
  } finally {
    unmount();
    restore();
  }
});

test("IssuePage retries a failed drained title before sending the queued route", async () => {
  const user = userEvent.setup();
  const { patchIssue, restore } = stubIssueApi();
  const statusSave = Promise.withResolvers<Issue>();
  const titleSave = Promise.withResolvers<Issue>();
  patchIssue
    .mockImplementationOnce(() => statusSave.promise)
    .mockImplementationOnce(() => titleSave.promise);
  const { unmount } = renderIssuePage();

  try {
    const status = (await screen.findByLabelText("Status")) as HTMLSelectElement;
    await user.selectOptions(status, "backlog");
    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { status: "backlog" }, keyedAttempt)
    );

    // The route input takes focus when it opens, which would blur (and submit) an open title
    // editor exactly as a click does in a browser; open it first so both drafts can be staged.
    const route = await openRouteEditor();
    const title = await openTitleEditor();
    const routeForm = screen.getByRole("button", { name: "Save route" }).closest("form");
    if (routeForm === null) {
      throw new Error("Save route must be inside a form.");
    }
    await user.clear(title);
    await user.click(title);
    await user.paste("Queued title");
    // Blur has no user-event form; it is what the title editor saves on.
    fireEvent.blur(title);
    await user.click(route);
    await user.paste("role:queued");
    // Submit has no user-event form; the route form saves on its own submit.
    fireEvent.submit(routeForm);

    act(() => {
      statusSave.resolve({ ...issue, status: "backlog" });
    });
    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { title: "Queued title" }, keyedAttempt)
    );
    titleSave.reject(new Error("offline"));
    await screen.findByRole("alert");
    expect(patchIssue).toHaveBeenCalledTimes(2);

    await user.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { route: "role:queued" }, keyedAttempt)
    );
    expect(patchIssue).toHaveBeenCalledTimes(4);
  } finally {
    unmount();
    restore();
  }
});

test("IssuePage mounts the header without reading who has signed in until the assignee control is reached for", async () => {
  const { restore } = stubIssueApi();
  const listUsers = spyOn(api, "listUsers").mockResolvedValue([
    { login: "alice" },
    { login: "bob" },
  ]);
  const { unmount } = renderIssuePage();
  try {
    const control = await screen.findByRole("combobox", { name: "Assignee of CORE-1" });
    await screen.findByRole("heading", { level: 1 });
    expect(listUsers).not.toHaveBeenCalled();
    // Focus has no user-event form; the control is reached for, never pressed.
    fireEvent.focus(control);
    await waitFor(() => expect(listUsers).toHaveBeenCalledTimes(1));
    await waitFor(() =>
      expect(
        [...(control as HTMLSelectElement).options].map((option) => option.textContent)
      ).toContain("bob")
    );
  } finally {
    unmount();
    restore();
    listUsers.mockRestore();
  }
});

test("IssuePage saves a parent from the header's parent editor", async () => {
  const user = userEvent.setup();
  const { patchIssue, restore } = stubIssueApi();
  const { unmount } = renderIssuePage();

  try {
    await openIssueActions();
    await user.click(screen.getByRole("button", { name: "Set parent issue" }));
    const parent = (await screen.findByLabelText("Parent")) as HTMLInputElement;
    await user.click(parent);
    await user.paste("CORE-9");
    await user.click(screen.getByRole("button", { name: "Save parent" }));

    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { parent: "CORE-9" }, keyedAttempt)
    );
    const link = (await screen.findByRole("link", { name: "CORE-9" })) as HTMLAnchorElement;
    expect(link).toHaveAttribute("href", "/issues/CORE-9");
  } finally {
    unmount();
    restore();
  }
});

test("IssuePage clears the parent with an empty editor save", async () => {
  const user = userEvent.setup();
  const { getIssue, patchIssue, restore } = stubIssueApi();
  getIssue.mockResolvedValue({ ...issue, parent: "CORE-9" });
  const { unmount } = renderIssuePage();

  try {
    await openIssueActions();
    await user.click(screen.getByRole("button", { name: "Edit parent issue" }));
    const parent = (await screen.findByLabelText("Parent")) as HTMLInputElement;
    expect(parent.value).toBe("CORE-9");
    await user.clear(parent);
    await user.click(screen.getByRole("button", { name: "Save parent" }));

    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { parent: null }, keyedAttempt)
    );
    await openIssueActions();
    await screen.findByRole("button", { name: "Set parent issue" });
    expect(screen.queryByRole("link", { name: "CORE-9" })).not.toBeInTheDocument();
  } finally {
    unmount();
    restore();
  }
});

test("IssuePage renders the server's reason inline when a parent save is refused", async () => {
  const user = userEvent.setup();
  const { patchIssue, restore } = stubIssueApi();
  patchIssue.mockImplementationOnce(() =>
    Promise.reject(
      new ApiError(409, {
        code: "PARENT_INPUT",
        error: "would create a cycle: CORE-1 → CORE-9 → CORE-1",
      })
    )
  );
  const { unmount } = renderIssuePage();

  try {
    await openIssueActions();
    await user.click(screen.getByRole("button", { name: "Set parent issue" }));
    const parent = (await screen.findByLabelText("Parent")) as HTMLInputElement;
    await user.click(parent);
    await user.paste("CORE-9");
    await user.click(screen.getByRole("button", { name: "Save parent" }));

    await screen.findByText("would create a cycle: CORE-1 → CORE-9 → CORE-1");
    expect(screen.queryByText("Could not update this issue.")).not.toBeInTheDocument();
  } finally {
    unmount();
    restore();
  }
});

// W34. Every write to one issue goes in the order it was pressed (`issue:<KEY>`), a board move's and
// a components save's as much as the header's: a board move pressed during an outage, then a
// status change from the issue's header, land in that order once Dispatch is back, even though the
// move's answer is the slower of the two.
test("a board move then a header status change land in press order after an outage", async () => {
  const user = userEvent.setup();
  const { patchIssue, restore } = stubIssueApi();
  const { queryClient, unmount } = renderIssuePage();
  let up = false;
  const landed: string[] = [];
  // The move's answer is the slower of the two, and how much slower is the test's rather than
  // real time's: `outstanding` says the move is out again, and its answer lands only when the
  // test lets it, with the header's change already queued behind it.
  const outstanding = Promise.withResolvers<void>();
  const moveAnswers = Promise.withResolvers<void>();
  patchIssue.mockImplementation(async (key, input, bounds) => {
    // As `api.patchIssue` does, the request is told to `sent` before anything goes out.
    bounds.sent?.({ body: input, method: "PATCH", path: `/api/v1/issues/${key}` });
    if (!up) throw new NetworkError("/api/v1/issues/CORE-1", new TypeError("Failed to fetch"));
    if (input.rank !== undefined) {
      outstanding.resolve();
      await moveAnswers.promise;
    }
    landed.push(input.rank !== undefined ? `board move to ${input.status}` : `${input.status}`);
    return narrowIssue({ ...issue, status: input.status ?? issue.status });
  });
  const { artifacts: _artifacts, children: _children, open_asks: _asks, ...fields } = issue;
  const summary: IssueSummary = { ...fields, open_asks: 0 };
  queryClient.setQueryData(projectIssuesQueryKey("CORE", []), [summary]);
  let moveCard: ((key: string, status: IssueStatus, index: number) => Promise<void>) | undefined;
  function Board(): null {
    moveCard = useBoardMoves("CORE").moveCard;
    return null;
  }
  const board = render(
    <QueryClientProvider client={queryClient}>
      <Board />
    </QueryClientProvider>
  );
  try {
    const status = (await screen.findByLabelText("Status")) as HTMLSelectElement;
    let moved: Promise<void> | undefined;
    act(() => {
      moved = moveCard?.("CORE-1", "in_progress", 0);
    });
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(1));
    await user.selectOptions(status, "backlog");
    up = true;
    act(() => {
      window.dispatchEvent(new Event("online"));
    });
    await act(async () => {
      await outstanding.promise;
    });
    moveAnswers.resolve();
    await act(async () => {
      await moved;
    });
    await waitFor(() => expect(landed).toHaveLength(2));
    expect(landed).toEqual(["board move to in_progress", "backlog"]);
  } finally {
    board.unmount();
    unmount();
    restore();
  }
});

// The issues an issue waits on are written where their value is drawn, in the panel's
// Development section (dispatch://INFRA-1196 R11), and a list Dispatch refuses - a key it does
// not know, the issue itself, an issue in another project - is said there, over the list the
// reader typed, so they mend it rather than retype it. It used to close at once and reach the
// header as "Could not update this issue.", whose Retry sent the same refused list again.
test("the blocked-by editor stays open on a refusal and says what Dispatch refused", async () => {
  const user = userEvent.setup();
  const { patchIssue, restore } = stubIssueApi();
  patchIssue.mockImplementationOnce(() =>
    Promise.reject(
      new ApiError(400, { code: "BLOCKED_BY_INPUT", error: "blocked_by issue CORE-9 not found" })
    )
  );
  const { unmount } = renderIssuePage();

  try {
    await user.click(await screen.findByRole("button", { name: "Add blocked by of CORE-1" }));
    const keys = (await screen.findByLabelText("Issues CORE-1 waits on")) as HTMLInputElement;
    await user.click(keys);
    await user.paste("CORE-9");
    await user.click(screen.getByRole("button", { name: "Save blocked by" }));

    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith(
        "CORE-1",
        { blocked_by: ["CORE-9"] },
        keyedAttempt
      )
    );
    expect(await screen.findByRole("alert")).toHaveTextContent("blocked_by issue CORE-9 not found");
    expect(
      (screen.getByLabelText("Issues CORE-1 waits on") as HTMLInputElement).value,
      "the refused list is no longer in the box for the reader to mend"
    ).toBe("CORE-9");
    expect(screen.queryByText("Could not update this issue.")).not.toBeInTheDocument();
  } finally {
    unmount();
    restore();
  }
});

test("a blocked-by that is set can be changed and cleared from the panel", async () => {
  const user = userEvent.setup();
  const { getIssue, patchIssue, restore } = stubIssueApi();
  getIssue.mockResolvedValue({ ...issue, blocked_by: ["CORE-2"] });
  const { unmount } = renderIssuePage();

  try {
    expect(await screen.findByRole("link", { name: "CORE-2" })).toBeInTheDocument();
    await user.click(await screen.findByRole("button", { name: "Edit blocked by of CORE-1" }));
    const keys = (await screen.findByLabelText("Issues CORE-1 waits on")) as HTMLInputElement;
    expect(keys.value).toBe("CORE-2");

    await user.clear(keys);
    await user.paste("core-3");
    await user.click(screen.getByRole("button", { name: "Save blocked by" }));
    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith(
        "CORE-1",
        { blocked_by: ["CORE-3"] },
        keyedAttempt
      )
    );
    // The editor closes on a landing, and the written list is what the panel draws.
    await waitFor(() =>
      expect(screen.queryByLabelText("Issues CORE-1 waits on")).not.toBeInTheDocument()
    );
    expect(await screen.findByRole("link", { name: "CORE-3" })).toBeInTheDocument();

    await user.click(await screen.findByRole("button", { name: "Edit blocked by of CORE-1" }));
    await user.clear(await screen.findByLabelText("Issues CORE-1 waits on"));
    await user.click(screen.getByRole("button", { name: "Save blocked by" }));
    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { blocked_by: [] }, keyedAttempt)
    );
  } finally {
    unmount();
    restore();
  }
});

// R21: the indicator names the field in the page's own words. It read "CORE-1: route saved",
// which is the schema's name for it, and "CORE-1: blocked_by saved" for the issues it waits on.
test("the shell's indicator names the field a header write is saving", async () => {
  const user = userEvent.setup();
  const { patchIssue, restore } = stubIssueApi();
  const saving = deferred<Issue>();
  patchIssue.mockImplementationOnce(() => saving.promise);
  const { queryClient, unmount } = renderIssuePage();

  try {
    await user.click(await screen.findByRole("button", { name: "Messages default to no route" }));
    const route = (await screen.findByLabelText("Message route")) as HTMLInputElement;
    await user.click(route);
    await user.paste("role:legion-controller-core");
    await user.click(screen.getByRole("button", { name: "Save route" }));

    await waitFor(() =>
      expect(
        writesOf(queryClient)
          .rows()
          .map((row) => row.label)
      ).toEqual(["Message route of CORE-1 saved"])
    );
    saving.resolve(narrowIssue({ ...issue, route: "role:legion-controller-core" }));
    await waitFor(() => expect(writesOf(queryClient).rows()).toHaveLength(0));
  } finally {
    unmount();
    restore();
  }
});

// The refusal is drawn in the editor and nowhere else, so a reader who closed the editor while
// the save was out was shown no refusal at all. Cancel and Escape wait for the answer.
test("the blocked-by editor refuses Cancel and Escape while the save is out", async () => {
  const user = userEvent.setup();
  const { patchIssue, restore } = stubIssueApi();
  const saving = deferred<Issue>();
  patchIssue.mockImplementationOnce(() => saving.promise);
  const { unmount } = renderIssuePage();

  try {
    await user.click(
      await screen.findByRole("button", { name: blockedByControlName("CORE-1", 0) })
    );
    const keys = (await screen.findByLabelText("Issues CORE-1 waits on")) as HTMLInputElement;
    await user.click(keys);
    await user.paste("CORE-9");
    await user.click(screen.getByRole("button", { name: CONTROL_LABELS.save_blocked_by }));
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(1));

    await user.click(screen.getByRole("button", { name: CONTROL_LABELS.cancel }));
    expect(
      screen.getByLabelText("Issues CORE-1 waits on"),
      "Cancel closed the editor over a save whose refusal it would have drawn"
    ).toBeInTheDocument();
    await user.click(keys);
    await user.keyboard("{Escape}");
    expect(
      screen.getByLabelText("Issues CORE-1 waits on"),
      "Escape closed the editor over a save whose refusal it would have drawn"
    ).toBeInTheDocument();

    // The answer closes it, which is the one thing that may.
    saving.resolve(narrowIssue({ ...issue, blocked_by: ["CORE-9"] }));
    await waitFor(() =>
      expect(screen.queryByLabelText("Issues CORE-1 waits on")).not.toBeInTheDocument()
    );
  } finally {
    unmount();
    restore();
  }
});

// A refusal belongs to the attempt that earned it. The editor held the last one until the next
// save started, so opening it again drew a refusal of a list the reader had already dropped.
test("the blocked-by editor opened again draws no refusal from the attempt before", async () => {
  const user = userEvent.setup();
  const { patchIssue, restore } = stubIssueApi();
  patchIssue.mockImplementationOnce(() =>
    Promise.reject(
      new ApiError(400, { code: "BLOCKED_BY_INPUT", error: "blocked_by issue CORE-9 not found" })
    )
  );
  const { unmount } = renderIssuePage();

  try {
    await user.click(
      await screen.findByRole("button", { name: blockedByControlName("CORE-1", 0) })
    );
    await user.click(await screen.findByLabelText("Issues CORE-1 waits on"));
    await user.paste("CORE-9");
    await user.click(screen.getByRole("button", { name: CONTROL_LABELS.save_blocked_by }));
    expect(await screen.findByRole("alert")).toHaveTextContent("blocked_by issue CORE-9 not found");

    await user.click(screen.getByRole("button", { name: CONTROL_LABELS.cancel }));
    await waitFor(() =>
      expect(screen.queryByLabelText("Issues CORE-1 waits on")).not.toBeInTheDocument()
    );
    await user.click(screen.getByRole("button", { name: blockedByControlName("CORE-1", 0) }));
    expect(await screen.findByLabelText("Issues CORE-1 waits on")).toBeInTheDocument();
    expect(
      screen.queryByRole("alert"),
      "the editor draws the refusal of a save this reader never made"
    ).not.toBeInTheDocument();
  } finally {
    unmount();
    restore();
  }
});

// R11: the menu lists the properties with no value and presses the control that sets each one,
// rather than drawing a second copy of it. It finds that control by the name the control
// exports (`blockedByControlName`), so the two sides cannot spell it apart: while each typed
// its own, a word changed in one made the entry disappear with no error anywhere.
test("the add menu opens the control the panel draws, under the name that control exports", async () => {
  const user = userEvent.setup();
  const { restore } = stubIssueApi();
  const { unmount } = renderIssuePage();

  try {
    expect(
      await screen.findByRole("button", { name: blockedByControlName("CORE-1", 0) }),
      "the panel draws no blocked-by control under the name it exports"
    ).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: CONTROL_LABELS.add }));
    await user.click(await screen.findByRole("menuitem", { name: PROPERTY_LABELS.blocked_by }));
    expect(
      await screen.findByLabelText("Issues CORE-1 waits on"),
      "the menu entry reached no control, so picking it opened nothing"
    ).toBeInTheDocument();
  } finally {
    unmount();
    restore();
  }
});

// R21: the indicator says what the page calls the field and what it calls the status, never the
// schema's token for either. It read the first key of the write ("CORE-1: blocked_by saved") and
// the status as the wire spells it ("CORE-1 set to in_progress").
test("the indicator names every field a header write carries, and a status by the page's word", () => {
  expect(issueUpdateLabel("CORE-1", { parent: "CORE-9" })).toBe("Parent of CORE-1 saved");
  expect(issueUpdateLabel("CORE-1", { route: "role:legion-controller-core" })).toBe(
    "Message route of CORE-1 saved"
  );
  expect(issueUpdateLabel("CORE-1", { status: "in_progress" })).toBe("CORE-1 set to In progress");
  expect(issueUpdateLabel("CORE-1", { parent: "CORE-9", title: "A title" })).toBe(
    "Parent and Title of CORE-1 saved"
  );
  expect(issueUpdateLabel("CORE-1", {})).toBe("CORE-1 saved");
});
