import { afterEach, beforeAll, expect, jest, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
// biome-ignore lint/style/noRestrictedImports: this file keeps fireEvent for blur, submit, focus and one edit inside a single act batch
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { realClock } from "../../__tests__/fake-clock";
import { ALICE_DETAILS_KEY, openIssueDetails } from "../../__tests__/issue-details";
import { pickIn } from "../../__tests__/issue-picks";
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { panelOf, pickNamed, rows } from "../../__tests__/list-picks";
import { answerIssuePatches } from "../../__tests__/send-answers";
import { stopTheLateWrite } from "../../__tests__/stop-late-write";
import { userOnFakeClock } from "../../__tests__/user";
import { ApiError, api, NetworkError } from "../../api/client";
import type { Issue, IssueDetails, IssueSummary } from "../../api/types";
import { narrowIssue, patchAnswer } from "../issue-page/issue-fixtures";
import { titleWrittenElsewhere } from "../issue-page/title-elsewhere-fixture";
import type { IssueStatus } from "../project/board-model";
import { useBoardMoves } from "../project/board-moves";
import { projectIssuesQueryKey } from "../project/issue-filters";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { stoppedWriteNote } from "../writes/keyed-write";
import { IssuePage } from "./IssuePage";

// The rows draw through the shared renderer, whose Markdown engine loads lazily, and they run in
// random order, so the file loads the engine before its first row, whichever row that is.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

const issue: IssueDetails = {
  route_status: null,
  route_holder: null,
  route_status_reason: null,
  holder_state: null,
  last_seen: null,
  takeable_at: null,
  advances: [],
  advances_revision: 0,
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

/** Renders the issue page at `path`, its Spec tab unless a case names another. */
function renderIssuePage(path = "/issues/CORE-1") {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false, staleTime: Infinity } },
  });
  const view = render(
    <MemoryRouter initialEntries={[path]}>
      <QueryClientProvider client={queryClient}>
        <Routes>
          <Route path="/issues/:key/*" element={<IssuePage />} />
        </Routes>
      </QueryClientProvider>
    </MemoryRouter>
  );
  return { queryClient, unmount: view.unmount };
}

function stubIssueApi() {
  const getIssue = spyOn(api, "getIssue").mockResolvedValue(issue);
  const getIssueEvents = spyOn(api, "getIssueEvents").mockResolvedValue([]);
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([]);
  const getIssueSubscribers = spyOn(api, "getIssueSubscribers").mockResolvedValue([]);
  const getMyState = spyOn(api, "getMyState").mockResolvedValue({
    "CORE-1": { dismissed: [], last_read_seq: 0, pinned: false, seq: 0 },
  });
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
  // The server answers a PATCH with the narrow `Issue`, not `IssueDetails`: no artifacts,
  // children, or open asks. The stub mirrors that so the page is exercised against the
  // real response shape.
  const patchIssue = answerIssuePatches().mockImplementation(async (_key, input) =>
    patchAnswer(issue, input)
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
  await user.click(await screen.findByRole("button", { name: "Edit title" }));
  return (await screen.findByLabelText("Issue title")) as HTMLInputElement;
}

// A row that reads alice's identity keeps Details open for her once it opens it; each row starts
// with it closed.
afterEach(() => window.localStorage.removeItem(ALICE_DETAILS_KEY));

async function openRouteEditor(): Promise<HTMLInputElement> {
  const existing = screen.queryByLabelText("Message route");
  if (existing instanceof HTMLInputElement) {
    return existing;
  }
  const user = userEvent.setup();
  await openIssueDetails(user);
  await user.click(await screen.findByRole("button", { name: /Messages default to/ }));
  return (await screen.findByLabelText("Message route")) as HTMLInputElement;
}

test("IssuePage keeps an unsaved route draft when a stale refetch arrives", async () => {
  const user = userEvent.setup();
  const { patchIssue, restore } = stubIssueApi();
  const { unmount } = renderIssuePage();
  const firstSave = deferred<Issue>();
  patchIssue.mockImplementationOnce(() => firstSave.promise);

  try {
    await openIssueDetails(user);
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

// A Cancel while a route's write is out puts back the route that write carries, the route the
// issue is about to hold (decision 316), and the write's landing then gives the draft the route it
// lands with: put back to the old route, the draft would read as changed against the route out,
// the trigger would name no route while Dispatch has one, and a Save would send the old route over
// the new one.
test("IssuePage gives a route draft cancelled while its write is out the route that write lands with", async () => {
  const user = userEvent.setup();
  const { patchIssue, restore } = stubIssueApi();
  const { unmount } = renderIssuePage();
  const firstSave = deferred<Issue>();
  patchIssue.mockImplementationOnce(() => firstSave.promise);

  try {
    let route = await openRouteEditor();
    await user.click(route);
    await user.paste("role:a");
    await user.click(screen.getByRole("button", { name: "Save route" }));
    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { route: "role:a" }, keyedAttempt)
    );
    route = await openRouteEditor();
    expect(route.value).toBe("role:a");
    await user.click(screen.getByRole("button", { name: "Cancel" }));
    await screen.findByRole("button", { name: "Messages default to role:a" });

    await act(async () => {
      firstSave.resolve(narrowIssue({ ...issue, route: "role:a" }));
    });
    await screen.findByRole("button", { name: "Messages default to role:a" });
    route = await openRouteEditor();
    expect(route.value).toBe("role:a");
    await user.click(screen.getByRole("button", { name: "Save route" }));
    expect(patchIssue.mock.calls).toEqual([["CORE-1", { route: "role:a" }, keyedAttempt]]);
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
    await openIssueDetails(user);
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
    await openIssueDetails(user);
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
    await openIssueDetails(user);
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
    await user.click(
      await screen.findByRole("button", { description: "Review the spec", name: "Edit title" })
    );
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
    await user.click(await screen.findByRole("button", { description: "A", name: "Edit title" }));
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
  const user = userEvent.setup();
  const { patchIssue, restore } = stubIssueApi();
  const { unmount } = renderIssuePage();

  try {
    await user.click(await screen.findByRole("button", { name: pickNamed("Status") }));
    const backlog = screen.getByRole("option", { name: "Backlog" });
    // The same choice pressed twice in one task is what the guard under test ignores: the
    // list closes on the first press, and a second press in the same task comes before it has.
    act(() => {
      backlog.click();
      backlog.click();
    });

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
    const priority = await screen.findByRole("button", { name: pickNamed("Priority of CORE-1") });
    await user.click(priority);
    const list = within(panelOf(priority)).getByRole("listbox");
    expect(rows(list)).toEqual(["✓Unset", "P0", "P1", "P2", "P3"]);
    await user.click(within(list).getByRole("option", { name: "P0" }));

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
    const priority = await screen.findByRole("button", { name: pickNamed("Priority of CORE-1") });
    await pickIn(user, () => priority, "P0");
    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { priority: 0 }, keyedAttempt)
    );

    expect(priority).toHaveAccessibleName("Priority of CORE-1 P0");

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
    const priority = await screen.findByRole("button", { name: pickNamed("Priority of CORE-1") });
    await pickIn(user, () => priority, "P0");
    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { priority: 0 }, keyedAttempt)
    );
    expect(priority).toHaveAccessibleName("Priority of CORE-1 P0");

    act(() => {
      save.reject(new Error("offline"));
    });
    await screen.findByRole("alert");
    expect(priority).toHaveAccessibleName("Priority of CORE-1 Priority");
    expect(priority).not.toHaveAttribute("aria-disabled");
    expect((priority as HTMLButtonElement).disabled).toBe(false);
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

// A status write holds the guard a title's write needs, so the title waits queued and goes once
// the status write settles. A value is marked out only inside the guard, as its write goes: marked
// out while the guard held it back, the title was taken for the status write's own as that write
// settled, and dropped.
test("IssuePage drains a title draft after a different issue write settles", async () => {
  const user = userEvent.setup();
  const { patchIssue, restore } = stubIssueApi();
  const firstSave = Promise.withResolvers<Issue>();
  patchIssue.mockImplementationOnce(() => firstSave.promise);
  const { unmount } = renderIssuePage();

  try {
    const status = await screen.findByRole("button", { name: pickNamed("Status") });
    await pickIn(user, () => status, "Backlog");
    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { status: "backlog" }, keyedAttempt)
    );

    // The Status takes focus when it is used, which would blur (and submit) an open title editor
    // as a click does in a browser; open the editor after that write is away.
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
    const status = await screen.findByRole("button", { name: pickNamed("Status") });
    const route = await openRouteEditor();
    await pickIn(user, () => status, "Backlog");
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
    await user.click(
      await screen.findByRole("button", { description: "Review the spec", name: "Edit title" })
    );
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
    const status = await screen.findByRole("button", { name: pickNamed("Status") });
    await pickIn(user, () => status, "Backlog");
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

// An Escape while a title's write is out puts back the title that write carries, the title the
// issue is about to hold (decision 316). An edit opened after it and left with nothing typed then
// has nothing to send: put back to the issue's old title, the draft read as a change against the
// title out, and the landing's settle sent the old title over the saved one.
test("IssuePage sends no second title after an Escape while a title's write is out and an edit left with nothing typed", async () => {
  const user = userEvent.setup();
  const { patchIssue, restore } = stubIssueApi();
  const firstSave = Promise.withResolvers<Issue>();
  patchIssue.mockImplementationOnce(() => firstSave.promise);
  const { queryClient, unmount } = renderIssuePage();

  try {
    let title = await openTitleEditor();
    await user.clear(title);
    await user.click(title);
    await user.paste("Review the plan");
    // Blur has no user-event form; it is what the title editor saves on.
    fireEvent.blur(title);
    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith(
        "CORE-1",
        { title: "Review the plan" },
        keyedAttempt
      )
    );

    await openTitleEditor();
    await user.keyboard("{Escape}");
    title = await openTitleEditor();
    // Blur has no user-event form; it is what the title editor saves on.
    fireEvent.blur(title);
    await act(async () => {
      firstSave.resolve(narrowIssue({ ...issue, title: "Review the plan" }));
    });
    await waitFor(() => expect(queryClient.isMutating()).toBe(0));

    expect(patchIssue.mock.calls).toEqual([["CORE-1", { title: "Review the plan" }, keyedAttempt]]);
    expect(screen.getByRole("heading", { level: 1 })).toHaveTextContent(/^Review the plan$/);
  } finally {
    unmount();
    restore();
  }
});

// A title queued behind the write out is the reader's last ended choice, and typing ends no edit,
// so it neither chooses nor withdraws one (decision 324): with the saved title typed back in the
// editor, the queued title still goes as that write lands, and the editor keeps the reader's text
// through both landings. The end of the edit then sends that text, once. Withdrawn by the text
// typed back, the queued title never went; sent, the landing put it into the editor under the
// reader.
test("IssuePage sends the queued title as the write ahead lands while the saved title typed back stays in the editor, and the end of the edit sends it", async () => {
  const user = userEvent.setup();
  const { patchIssue, restore } = stubIssueApi();
  const firstSave = Promise.withResolvers<Issue>();
  patchIssue.mockImplementationOnce(() => firstSave.promise);
  const { queryClient, unmount } = renderIssuePage();

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
    // Blur has no user-event form; it is what the title editor saves on.
    fireEvent.blur(title);
    title = await openTitleEditor();
    // The last character taken out, so the draft goes from "ab" to "a" with no empty text between,
    // which would ask for nothing by itself.
    await user.type(title, "{End}{Backspace}");
    expect(title.value).toBe("a");
    await act(async () => {
      firstSave.resolve(narrowIssue({ ...issue, title: "a" }));
    });
    await waitFor(() => expect(queryClient.isMutating()).toBe(0));

    expect({ editor: title.value, writes: patchIssue.mock.calls }).toEqual({
      editor: "a",
      writes: [
        ["CORE-1", { title: "a" }, keyedAttempt],
        ["CORE-1", { title: "ab" }, keyedAttempt],
      ],
    });

    // Blur has no user-event form; it is what the title editor saves on.
    fireEvent.blur(title);
    await waitFor(() => expect(queryClient.isMutating()).toBe(0));
    expect(patchIssue.mock.calls.slice(2)).toEqual([["CORE-1", { title: "a" }, keyedAttempt]]);
  } finally {
    unmount();
    restore();
  }
});

// The reader saves a title, saves a second one, which waits behind the first write, and opens the
// editor again to type a third. The first write lands while the reader is still typing: what goes
// then is the second title, the reader's last ended choice, and what the reader has typed so far
// stays in the editor unsent, since no end of an edit chose it (decision 324). The end of that
// edit sends it, once. Sent as the first write landed, a title a reader was still typing reached
// Dispatch, half written, under every other reader's eyes, and the title they saved never went.
test("IssuePage sends the queued title and not the title the reader is still typing when the write ahead of it lands", async () => {
  const user = userEvent.setup();
  const { patchIssue, restore } = stubIssueApi();
  const firstSave = Promise.withResolvers<Issue>();
  patchIssue.mockImplementationOnce(() => firstSave.promise);
  const { queryClient, unmount } = renderIssuePage();

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
    // Blur has no user-event form; it is what the title editor saves on.
    fireEvent.blur(title);
    title = await openTitleEditor();
    // One character typed onto the end, as a reader goes on writing: the draft goes from "ab" to
    // "abc" with no empty text between, which would ask for nothing by itself.
    await user.type(title, "{End}c");
    expect(title.value).toBe("abc");
    await act(async () => {
      firstSave.resolve(narrowIssue({ ...issue, title: "a" }));
    });
    await waitFor(() => expect(queryClient.isMutating()).toBe(0));

    expect({ editor: title.value, writes: patchIssue.mock.calls }).toEqual({
      editor: "abc",
      writes: [
        ["CORE-1", { title: "a" }, keyedAttempt],
        ["CORE-1", { title: "ab" }, keyedAttempt],
      ],
    });

    // Blur has no user-event form; it is what the title editor saves on.
    fireEvent.blur(title);
    await waitFor(() => expect(queryClient.isMutating()).toBe(0));
    expect(patchIssue.mock.calls.slice(2)).toEqual([["CORE-1", { title: "abc" }, keyedAttempt]]);
  } finally {
    unmount();
    restore();
  }
});

// The reader saves a title, then types the issue's own title back and ends the edit, which queues
// it behind the write out. Dispatch refuses that write, so the issue keeps its title, and the
// draft typed back is that title: it has nothing to send, and follows the issue. Another reader's
// title then reaches the heading, and a route save sends the route alone. Left queued, the issue's
// old title held the heading and went ahead of the route, over the other reader's title.
test("IssuePage lets another reader's title through after a refused save with the issue's title queued behind it, and a route save sends only the route", async () => {
  const user = userEvent.setup();
  const { patchIssue, restore } = stubIssueApi();
  const firstSave = Promise.withResolvers<Issue>();
  patchIssue.mockImplementationOnce(() => firstSave.promise);
  const { queryClient, unmount } = renderIssuePage();

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
    await user.paste(issue.title);
    // Blur has no user-event form; it is what the title editor saves on.
    fireEvent.blur(title);
    act(() => {
      firstSave.reject(
        new ApiError(400, { code: "INVALID_ISSUE", error: "the write was refused" })
      );
    });
    await screen.findByText("Could not update this issue.");
    titleWrittenElsewhere(queryClient, "Plan the review");
    const heading = screen.getByRole("heading", { level: 1 }).textContent;

    const route = await openRouteEditor();
    await user.click(route);
    await user.paste("role:a");
    await user.click(screen.getByRole("button", { name: "Save route" }));
    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { route: "role:a" }, keyedAttempt)
    );
    await waitFor(() => expect(queryClient.isMutating()).toBe(0));

    expect({ heading, writes: patchIssue.mock.calls }).toEqual({
      heading: "Plan the review",
      writes: [
        ["CORE-1", { title: "a" }, keyedAttempt],
        ["CORE-1", { route: "role:a" }, keyedAttempt],
      ],
    });
  } finally {
    unmount();
    restore();
  }
});

// The same order with no other reader: the issue's own title, typed back and queued behind a
// write Dispatch then refuses, has nothing left to send once the issue keeps that title, so a
// route saved next goes alone. Left queued, the title went ahead of the route, and would have put
// the issue's old title over any title another reader had set since.
test("IssuePage sends only the route after a refused save with the issue's title queued behind it", async () => {
  const user = userEvent.setup();
  const { patchIssue, restore } = stubIssueApi();
  const firstSave = Promise.withResolvers<Issue>();
  patchIssue.mockImplementationOnce(() => firstSave.promise);
  const { queryClient, unmount } = renderIssuePage();

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
    await user.paste(issue.title);
    // Blur has no user-event form; it is what the title editor saves on.
    fireEvent.blur(title);
    act(() => {
      firstSave.reject(
        new ApiError(400, { code: "INVALID_ISSUE", error: "the write was refused" })
      );
    });
    await screen.findByText("Could not update this issue.");

    const route = await openRouteEditor();
    await user.click(route);
    await user.paste("role:a");
    await user.click(screen.getByRole("button", { name: "Save route" }));
    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { route: "role:a" }, keyedAttempt)
    );
    await waitFor(() => expect(queryClient.isMutating()).toBe(0));

    expect(patchIssue.mock.calls).toEqual([
      ["CORE-1", { title: "a" }, keyedAttempt],
      ["CORE-1", { route: "role:a" }, keyedAttempt],
    ]);
  } finally {
    unmount();
    restore();
  }
});

// The reader saves "a", then saves "b", which waits queued behind the write out, and opens the
// editor again to empty the title and end the edit. Dispatch takes no empty title, so that end is
// refused, and it drops the queued "b": the reader's last act was to clear the title (decision
// 324). So "a"'s landing sends nothing more. Dispatch's hook before this change does the same:
// [a]. Kept queued, "b" went as "a" landed, a title the reader had cleared since.
test("IssuePage sends no title queued behind the write out once the reader empties the title and ends the edit", async () => {
  const user = userEvent.setup();
  const { patchIssue, restore } = stubIssueApi();
  const firstSave = Promise.withResolvers<Issue>();
  patchIssue.mockImplementationOnce(() => firstSave.promise);
  const { queryClient, unmount } = renderIssuePage();

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
    await user.paste("b");
    // Blur has no user-event form; it is what the title editor saves on.
    fireEvent.blur(title);
    title = await openTitleEditor();
    await user.clear(title);
    // Blur has no user-event form; it is what the title editor saves on.
    fireEvent.blur(title);
    await screen.findByText("Title is required.");
    await act(async () => {
      firstSave.resolve(narrowIssue({ ...issue, title: "a" }));
    });
    await waitFor(() => expect(queryClient.isMutating()).toBe(0));

    expect(patchIssue.mock.calls).toEqual([["CORE-1", { title: "a" }, keyedAttempt]]);
  } finally {
    unmount();
    restore();
  }
});

// The reader types a title, and with the editor still open another reader sets the issue's title
// to that same text, then to a title of their own. The text is the reader's, typed by them, so the
// editor keeps it through both, and the end of the edit sends it (decision 332). Judged against
// the issue's title as each arrived, the draft read as not changed once the first matched it, then
// followed the issue to the second, and the end of the edit sent nothing.
test("IssuePage keeps a typed title through another reader setting the issue to it and then changing it, and sends it as the edit ends", async () => {
  const user = userEvent.setup();
  const { patchIssue, restore } = stubIssueApi();
  const { queryClient, unmount } = renderIssuePage();

  try {
    const title = await openTitleEditor();
    await user.clear(title);
    await user.click(title);
    await user.paste("Review the plan");
    const editor = [title.value];
    titleWrittenElsewhere(queryClient, "Review the plan");
    editor.push(title.value);
    titleWrittenElsewhere(queryClient, "Plan the review");
    editor.push(title.value);
    // Blur has no user-event form; it is what the title editor saves on.
    fireEvent.blur(title);
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(queryClient.isMutating()).toBe(0));

    expect({ editor, writes: patchIssue.mock.calls }).toEqual({
      editor: ["Review the plan", "Review the plan", "Review the plan"],
      writes: [["CORE-1", { title: "Review the plan" }, keyedAttempt]],
    });
  } finally {
    unmount();
    restore();
  }
});

// The reader opens the title's editor, types a character and takes it back out, and another
// reader then renames the issue. The keystroke that left the draft on the issue's title handed it
// back to the issue (decision 332), so the editor follows the rename and the end of the edit sends
// nothing. Dispatch's hook before this change does the same: no write. Kept the reader's, the
// draft sent the issue's old title over the rename.
test("IssuePage follows another reader's title after a character typed and taken back out, and sends nothing as the edit ends", async () => {
  const user = userEvent.setup();
  const { patchIssue, restore } = stubIssueApi();
  const { queryClient, unmount } = renderIssuePage();

  try {
    const title = await openTitleEditor();
    // A character typed and taken back out, so the draft goes from the issue's title to one more
    // character and back, with no empty text between.
    await user.type(title, "x{Backspace}");
    expect(title.value).toBe(issue.title);
    titleWrittenElsewhere(queryClient, "Plan the review");
    const editor = title.value;

    // Blur has no user-event form; it is what the title editor saves on.
    fireEvent.blur(title);
    await waitFor(() => expect(queryClient.isMutating()).toBe(0));
    expect({
      editor,
      heading: screen.getByRole("heading", { level: 1 }).textContent,
      writes: patchIssue.mock.calls,
    }).toEqual({ editor: "Plan the review", heading: "Plan the review", writes: [] });
  } finally {
    unmount();
    restore();
  }
});

// The reader opens the title's editor and types a space at the end of the title, as a reader does
// before the next word, and another reader then renames the issue. The space is the reader's text:
// as typed, the draft is not the issue's title, so the keystroke hands nothing back and the editor
// keeps the reader's text through the rename (decision 332). The end of the edit sends it trimmed,
// the old title, once. Dispatch's hook before this change does the same: [Review the spec].
// Compared trimmed, the keystroke handed the draft back to the issue, the rename replaced the
// reader's text in the open editor, and the end of the edit sent nothing.
test("IssuePage keeps a space typed after the title through another reader's rename, and sends the title trimmed as the edit ends", async () => {
  const user = userEvent.setup();
  const { patchIssue, restore } = stubIssueApi();
  const { queryClient, unmount } = renderIssuePage();

  try {
    const title = await openTitleEditor();
    await user.type(title, " ");
    expect(title.value).toBe(`${issue.title} `);
    titleWrittenElsewhere(queryClient, "Plan the review");
    // The editor holds the reader's text through the rename, before the end of the edit sends it.
    expect(title.value).toBe(`${issue.title} `);

    // Blur has no user-event form; it is what the title editor saves on.
    fireEvent.blur(title);
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(queryClient.isMutating()).toBe(0));
    expect(patchIssue.mock.calls).toEqual([["CORE-1", { title: issue.title }, keyedAttempt]]);
  } finally {
    unmount();
    restore();
  }
});

// The reader saves "a", and while its write is out opens the editor again, types a character and
// takes it back out, which leaves the draft on "a", the title out. The write's settle then hands
// the draft back to the issue because the write landed with it (decision 332), whatever the
// keystroke made it, so the editor follows the title another reader sets next, and the end of the
// edit sends nothing. Dispatch's hook before this change does the same: [a]. An earlier form of
// this change's hook handed an ended choice back on another reader's title and kept a typed
// draft, as the keystroke made this one, so it held "a" over the other reader's title and the end
// of the edit sent it a second time.
test("IssuePage gives the issue a title the reader typed back to while its write was out as that write lands, so the editor follows another reader's title and the end of the edit sends nothing", async () => {
  const user = userEvent.setup();
  const { patchIssue, restore } = stubIssueApi();
  const firstSave = Promise.withResolvers<Issue>();
  patchIssue.mockImplementationOnce(() => firstSave.promise);
  const { queryClient, unmount } = renderIssuePage();

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
    expect(title.value).toBe("a");
    // A character typed and taken back out, so the draft goes from "a" to "ax" and back with no
    // empty text between, which would ask for nothing by itself.
    await user.type(title, "x{Backspace}");
    await act(async () => {
      firstSave.resolve(narrowIssue({ ...issue, title: "a" }));
    });
    await waitFor(() => expect(queryClient.isMutating()).toBe(0));
    titleWrittenElsewhere(queryClient, "Plan the review");
    const editor = title.value;

    // Blur has no user-event form; it is what the title editor saves on.
    fireEvent.blur(title);
    await waitFor(() => expect(queryClient.isMutating()).toBe(0));
    expect({ editor, writes: patchIssue.mock.calls }).toEqual({
      editor: "Plan the review",
      writes: [["CORE-1", { title: "a" }, keyedAttempt]],
    });
  } finally {
    unmount();
    restore();
  }
});

// The same keystroke while the write is out, a character typed and taken back out, and Dispatch
// then refuses the write. The keystroke handed the draft back as the reader's ended choice, the
// title out (decision 332), as an Escape would leave it, so the refusal keeps "a" in the field,
// the reader's unsent choice: another reader's title leaves it in place, and the end of the edit
// sends "a" again, over that title. Dispatch's hook before this change does the same: [a, a].
// Handed back to the issue instead, the draft followed the other reader's title, and the end of
// the edit sent nothing.
test("IssuePage keeps a title the reader typed back to while its write was out once Dispatch refuses that write, and sends it again as the edit ends", async () => {
  const user = userEvent.setup();
  const { patchIssue, restore } = stubIssueApi();
  const firstSave = Promise.withResolvers<Issue>();
  patchIssue.mockImplementationOnce(() => firstSave.promise);
  const { queryClient, unmount } = renderIssuePage();

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
    expect(title.value).toBe("a");
    // A character typed and taken back out, so the draft goes from "a" to "ax" and back.
    await user.type(title, "x{Backspace}");
    act(() => {
      firstSave.reject(
        new ApiError(400, { code: "INVALID_ISSUE", error: "the write was refused" })
      );
    });
    await screen.findByText("Could not update this issue.");
    titleWrittenElsewhere(queryClient, "Plan the review");
    const editor = title.value;

    // Blur has no user-event form; it is what the title editor saves on.
    fireEvent.blur(title);
    await waitFor(() => expect(queryClient.isMutating()).toBe(0));
    expect({ editor, writes: patchIssue.mock.calls }).toEqual({
      editor: "a",
      writes: [
        ["CORE-1", { title: "a" }, keyedAttempt],
        ["CORE-1", { title: "a" }, keyedAttempt],
      ],
    });
  } finally {
    unmount();
    restore();
  }
});

// The reader saves "a", and while its write is out opens the editor again and types a space after
// it, as a reader does before the next word. The write then lands with "a". The space is the
// reader's text, so the draft as typed is not the title the write landed with, the settle hands
// nothing back, and the editor still holds "a " (decision 332). In what a landing does to text
// typed while its write was out, this is the one place this change means to differ from
// Dispatch's hook before it, which trims the draft as the write lands: there the editor holds "a",
// the space is gone, and the reader's next word joins the title.
test("IssuePage keeps a space typed after a saved title while its write is out as that write lands", async () => {
  const user = userEvent.setup();
  const { patchIssue, restore } = stubIssueApi();
  const firstSave = Promise.withResolvers<Issue>();
  patchIssue.mockImplementationOnce(() => firstSave.promise);
  const { queryClient, unmount } = renderIssuePage();

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
    expect(title.value).toBe("a");
    await user.type(title, " ");
    expect(title.value).toBe("a ");
    await act(async () => {
      firstSave.resolve(narrowIssue({ ...issue, title: "a" }));
    });
    await waitFor(() => expect(queryClient.isMutating()).toBe(0));

    expect({ editor: title.value, writes: patchIssue.mock.calls }).toEqual({
      editor: "a ",
      writes: [["CORE-1", { title: "a" }, keyedAttempt]],
    });
  } finally {
    unmount();
    restore();
  }
});

// The reader writes "a " in place of the title, a space after it, and ends the edit. Every end
// trims, so the end sends "a" and leaves the draft "a", which the write then lands with: its
// settle hands the draft back to the issue (decision 332), the heading follows the title another
// reader sets next, and the end of a later edit sends nothing over it. Dispatch's hook before this
// change does the same: [a]. That the end trims the draft itself is held by the sequence test
// (I7 at the next end): an end that left "a " as typed is handed back at this landing all the
// same, since the hand-back compares the draft trimmed, so this row passes either way.
test("IssuePage gives the issue a title ended with a space after it as its write lands, so the heading follows another reader's title and a later end sends nothing", async () => {
  const user = userEvent.setup();
  const { patchIssue, restore } = stubIssueApi();
  const { queryClient, unmount } = renderIssuePage();

  try {
    let title = await openTitleEditor();
    await user.clear(title);
    await user.click(title);
    await user.paste("a ");
    // Blur has no user-event form; it is what the title editor saves on.
    fireEvent.blur(title);
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(queryClient.isMutating()).toBe(0));
    titleWrittenElsewhere(queryClient, "Y");
    const heading = screen.getByRole("heading", { level: 1 }).textContent;

    title = await openTitleEditor();
    const editor = title.value;
    // Blur has no user-event form; it is what the title editor saves on.
    fireEvent.blur(title);
    await waitFor(() => expect(queryClient.isMutating()).toBe(0));
    expect({ heading, editor, writes: patchIssue.mock.calls }).toEqual({
      heading: "Y",
      editor: "Y",
      writes: [["CORE-1", { title: "a" }, keyedAttempt]],
    });
  } finally {
    unmount();
    restore();
  }
});

// The reader saves "a", then ends "b" while "a"'s write is out, so "b" waits queued behind it, and
// opens the editor again to type a character onto "b" and take it back out. With a title queued no
// keystroke hands the draft back, so the draft is "b", typed. "a" lands and "b" goes, and "b"
// lands with the draft on it: the settle hands the draft back to the issue, as it does for any
// draft a write landed with, whoever's it was (decision 332). So the editor follows the title
// another reader sets next, and the end of the edit sends nothing. Dispatch's hook before this
// change does the same: [a, b]. Handed back only as the reader's ended choice, the typed "b" stayed
// in the editor over the other reader's title, and the end of the edit sent it a second time.
test("IssuePage gives the issue a queued title typed on and back while the write ahead was out as that title lands, so the editor follows another reader's title and the end of the edit sends nothing", async () => {
  const user = userEvent.setup();
  const { patchIssue, restore } = stubIssueApi();
  const firstSave = Promise.withResolvers<Issue>();
  patchIssue.mockImplementationOnce(() => firstSave.promise);
  const { queryClient, unmount } = renderIssuePage();

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
    await user.paste("b");
    // Blur has no user-event form; it is what the title editor saves on.
    fireEvent.blur(title);
    title = await openTitleEditor();
    // A character typed and taken back out, so the draft goes from "b" to "bx" and back with no
    // empty text between, which would ask for nothing by itself.
    await user.type(title, "x{Backspace}");
    expect(title.value).toBe("b");
    await act(async () => {
      firstSave.resolve(narrowIssue({ ...issue, title: "a" }));
    });
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(queryClient.isMutating()).toBe(0));
    titleWrittenElsewhere(queryClient, "C");
    const editor = title.value;

    // Blur has no user-event form; it is what the title editor saves on.
    fireEvent.blur(title);
    await waitFor(() => expect(queryClient.isMutating()).toBe(0));
    expect({ editor, writes: patchIssue.mock.calls }).toEqual({
      editor: "C",
      writes: [
        ["CORE-1", { title: "a" }, keyedAttempt],
        ["CORE-1", { title: "b" }, keyedAttempt],
      ],
    });
  } finally {
    unmount();
    restore();
  }
});

// A title Dispatch refuses stays in the field, the reader's unsent choice, which Retry or the end
// of the next edit sends. A status picked next is a write of its own, and nothing drains after a
// refusal (decision 315), so the status write's landing does not send the refused title again.
// Main's hook does send it again after the status write: [title, status, title].
test("IssuePage sends a refused title no second time as a status pick lands after it, and keeps the title in the field", async () => {
  const user = userEvent.setup();
  const { patchIssue, restore } = stubIssueApi();
  const titleSave = Promise.withResolvers<Issue>();
  patchIssue.mockImplementationOnce(() => titleSave.promise);
  const { queryClient, unmount } = renderIssuePage();

  try {
    const title = await openTitleEditor();
    await user.clear(title);
    await user.click(title);
    await user.paste("B");
    // Blur has no user-event form; it is what the title editor saves on.
    fireEvent.blur(title);
    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { title: "B" }, keyedAttempt)
    );
    act(() => {
      titleSave.reject(
        new ApiError(400, { code: "INVALID_ISSUE", error: "the write was refused" })
      );
    });
    await screen.findByText("Could not update this issue.");

    await pickIn(user, () => screen.getByRole("button", { name: pickNamed("Status") }), "Backlog");
    await waitFor(() =>
      expect(patchIssue).toHaveBeenCalledWith("CORE-1", { status: "backlog" }, keyedAttempt)
    );
    await waitFor(() => expect(queryClient.isMutating()).toBe(0));

    expect({
      heading: screen.getByRole("heading", { level: 1 }).textContent,
      writes: patchIssue.mock.calls,
    }).toEqual({
      heading: "B",
      writes: [
        ["CORE-1", { title: "B" }, keyedAttempt],
        ["CORE-1", { status: "backlog" }, keyedAttempt],
      ],
    });
  } finally {
    unmount();
    restore();
  }
});

// The reader saves "B", and Dispatch refuses it, so "B" stays in the field, the reader's unsent
// choice. Another reader then sets the issue to "B", and then to "C". Of the answers (a landing,
// a refusal, another reader's title), only the settle of the title's own write hands a typed or
// ended field back to the issue, never another reader's title (decision 332); the reader's own
// keystroke back to the issue's title, an end that changes nothing or an Escape do too. No title
// write has settled since the refusal and the reader makes none of those, so the heading keeps
// "B" and Retry sends it again, over "C". Dispatch's hook before this change does the same:
// [B, B]. Handed back as another reader's title matched it, the field followed "C", and Retry
// sent nothing.
test("IssuePage keeps a refused title through another reader setting the issue to it and then changing it, and Retry sends it again", async () => {
  const user = userEvent.setup();
  const { patchIssue, restore } = stubIssueApi();
  const titleSave = Promise.withResolvers<Issue>();
  patchIssue.mockImplementationOnce(() => titleSave.promise);
  const { queryClient, unmount } = renderIssuePage();

  try {
    const title = await openTitleEditor();
    await user.clear(title);
    await user.click(title);
    await user.paste("B");
    // Blur has no user-event form; it is what the title editor saves on.
    fireEvent.blur(title);
    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { title: "B" }, keyedAttempt)
    );
    act(() => {
      titleSave.reject(
        new ApiError(400, { code: "INVALID_ISSUE", error: "the write was refused" })
      );
    });
    await screen.findByText("Could not update this issue.");
    titleWrittenElsewhere(queryClient, "B");
    titleWrittenElsewhere(queryClient, "C");
    const heading = screen.getByRole("heading", { level: 1 }).textContent;

    await user.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(queryClient.isMutating()).toBe(0));
    expect({ heading, writes: patchIssue.mock.calls }).toEqual({
      heading: "B",
      writes: [
        ["CORE-1", { title: "B" }, keyedAttempt],
        ["CORE-1", { title: "B" }, keyedAttempt],
      ],
    });
  } finally {
    unmount();
    restore();
  }
});

// A draft queued behind a write of another field stays the reader's last ended choice while they
// type. Here a route waits behind a status write that fails: the failure settles no route, so the
// route stays queued, the reader's choice. The reader then clears the route in its editor, which
// ends no edit and withdraws nothing (decision 324), and saves a parent: the queued route goes
// first, one write of the issue's fields at a time, then the parent. Through both the route's
// editor keeps the reader's cleared text.
test("IssuePage sends a route that waited behind a failed status write ahead of a parent saved while the route's editor was cleared, and keeps the cleared text", async () => {
  const user = userEvent.setup();
  const { patchIssue, restore } = stubIssueApi();
  const statusSave = Promise.withResolvers<Issue>();
  patchIssue.mockImplementationOnce(() => statusSave.promise);
  const { queryClient, unmount } = renderIssuePage();

  try {
    const status = await screen.findByRole("button", { name: pickNamed("Status") });
    await pickIn(user, () => status, "Backlog");
    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { status: "backlog" }, keyedAttempt)
    );

    let route = await openRouteEditor();
    const routeForm = screen.getByRole("button", { name: "Save route" }).closest("form");
    if (routeForm === null) {
      throw new Error("Save route must be inside a form.");
    }
    await user.click(route);
    await user.paste("role:b");
    // Submit has no user-event form; the route form saves on its own submit.
    fireEvent.submit(routeForm);
    // The route waits behind the status write, one write of the issue's fields at a time, and the
    // editor closes on it.
    expect(screen.queryByLabelText("Message route")).not.toBeInTheDocument();
    expect(patchIssue).toHaveBeenCalledTimes(1);
    act(() => {
      statusSave.reject(new Error("offline"));
    });
    await waitFor(() => expect(queryClient.isMutating()).toBe(0));

    route = await openRouteEditor();
    expect(route.value).toBe("role:b");
    await user.clear(route);
    await openIssueDetails(user);
    await user.click(await screen.findByRole("button", { name: "None, set parent issue" }));
    const parent = (await screen.findByLabelText("Parent")) as HTMLInputElement;
    await user.click(parent);
    await user.paste("CORE-9");
    await user.click(screen.getByRole("button", { name: "Save parent" }));
    await waitFor(() =>
      expect(patchIssue).toHaveBeenCalledWith("CORE-1", { parent: "CORE-9" }, keyedAttempt)
    );
    await waitFor(() => expect(queryClient.isMutating()).toBe(0));

    expect({ route: route.value, writes: patchIssue.mock.calls }).toEqual({
      route: "",
      writes: [
        ["CORE-1", { status: "backlog" }, keyedAttempt],
        ["CORE-1", { route: "role:b" }, keyedAttempt],
        ["CORE-1", { parent: "CORE-9" }, keyedAttempt],
      ],
    });
  } finally {
    unmount();
    restore();
  }
});

test("IssuePage mounts the header without reading who has signed in until the assignee control is reached for", async () => {
  const user = userEvent.setup();
  const { restore } = stubIssueApi();
  const listUsers = spyOn(api, "listUsers").mockResolvedValue([
    { login: "alice" },
    { login: "bob" },
  ]);
  const { unmount } = renderIssuePage();
  try {
    const control = await screen.findByRole("button", { name: pickNamed("Assignee of CORE-1") });
    await screen.findByRole("heading", { level: 1 });
    expect(listUsers).not.toHaveBeenCalled();
    // Focus has no user-event form; the control is reached for, never pressed.
    fireEvent.focus(control);
    await waitFor(() => expect(listUsers).toHaveBeenCalledTimes(1));
    // The list draws its options only while it stands open.
    await user.click(control);
    expect(
      await within(
        await screen.findByRole("listbox", { name: "Assignee of CORE-1 options" })
      ).findByRole("option", { name: "bob" })
    ).toBeTruthy();
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
    await openIssueDetails(user);
    await user.click(await screen.findByRole("button", { name: "None, set parent issue" }));
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

// The parent's draft the same way: a Cancel while its write is out puts back the parent that
// write carries (decision 316), and the landing gives the draft the parent it lands with, so the
// editor opens on that parent and a Save sends nothing more.
test("IssuePage gives a parent draft cancelled while its write is out the parent that write lands with", async () => {
  const user = userEvent.setup();
  const { patchIssue, restore } = stubIssueApi();
  const firstSave = Promise.withResolvers<Issue>();
  patchIssue.mockImplementationOnce(() => firstSave.promise);
  const { unmount } = renderIssuePage();

  try {
    await openIssueDetails(user);
    await user.click(await screen.findByRole("button", { name: "None, set parent issue" }));
    let parent = (await screen.findByLabelText("Parent")) as HTMLInputElement;
    await user.click(parent);
    await user.paste("CORE-9");
    await user.click(screen.getByRole("button", { name: "Save parent" }));
    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { parent: "CORE-9" }, keyedAttempt)
    );
    // The write is out, so the issue has no parent yet.
    await user.click(await screen.findByRole("button", { name: "None, set parent issue" }));
    parent = (await screen.findByLabelText("Parent")) as HTMLInputElement;
    expect(parent.value).toBe("CORE-9");
    await user.click(screen.getByRole("button", { name: "Cancel" }));

    await act(async () => {
      firstSave.resolve(narrowIssue({ ...issue, parent: "CORE-9" }));
    });
    const link = await screen.findByRole("link", { name: "CORE-9" });
    expect(link).toHaveAttribute("href", "/issues/CORE-9");
    await user.click(screen.getByRole("button", { name: "Edit parent issue" }));
    parent = (await screen.findByLabelText("Parent")) as HTMLInputElement;
    expect(parent.value).toBe("CORE-9");
    await user.click(screen.getByRole("button", { name: "Save parent" }));
    expect(patchIssue.mock.calls).toEqual([["CORE-1", { parent: "CORE-9" }, keyedAttempt]]);
  } finally {
    unmount();
    restore();
  }
});

// A parent cleared while its write is out is a change: the issue has no parent yet, but the write
// out is about to set the one the reader took out. The landing leaves the cleared draft, and Save
// then clears the parent the write set.
test("IssuePage keeps a parent draft cleared while its write is out as that write lands, and Save clears the parent", async () => {
  const user = userEvent.setup();
  const { patchIssue, restore } = stubIssueApi();
  const firstSave = Promise.withResolvers<Issue>();
  patchIssue.mockImplementationOnce(() => firstSave.promise);
  const { queryClient, unmount } = renderIssuePage();

  try {
    await openIssueDetails(user);
    await user.click(await screen.findByRole("button", { name: "None, set parent issue" }));
    let parent = (await screen.findByLabelText("Parent")) as HTMLInputElement;
    await user.click(parent);
    await user.paste("CORE-9");
    await user.click(screen.getByRole("button", { name: "Save parent" }));
    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { parent: "CORE-9" }, keyedAttempt)
    );
    // The write is out, so the issue has no parent yet.
    await user.click(await screen.findByRole("button", { name: "None, set parent issue" }));
    parent = (await screen.findByLabelText("Parent")) as HTMLInputElement;
    expect(parent.value).toBe("CORE-9");
    await user.clear(parent);

    await act(async () => {
      firstSave.resolve(narrowIssue({ ...issue, parent: "CORE-9" }));
    });
    await waitFor(() =>
      expect(queryClient.getQueryData<IssueDetails>(["issue", "CORE-1"])?.parent).toBe("CORE-9")
    );
    expect(parent.value).toBe("");
    await user.click(screen.getByRole("button", { name: "Save parent" }));
    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { parent: null }, keyedAttempt)
    );
    expect(patchIssue).toHaveBeenCalledTimes(2);
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
    await openIssueDetails(user);
    await user.click(await screen.findByRole("button", { name: "Edit parent issue" }));
    const parent = (await screen.findByLabelText("Parent")) as HTMLInputElement;
    expect(parent.value).toBe("CORE-9");
    await user.clear(parent);
    await user.click(screen.getByRole("button", { name: "Save parent" }));

    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { parent: null }, keyedAttempt)
    );
    await screen.findByRole("button", { name: "None, set parent issue" });
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
    await openIssueDetails(user);
    await user.click(await screen.findByRole("button", { name: "None, set parent issue" }));
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

const unreachable = new NetworkError("/api/v1/issues/CORE-1", new TypeError("Failed to fetch"));

/** The issue page on its Children tab, whose reads `stubIssueApi` answers: the two cases below
 *  hold the clock through a write's whole minute, in which the Spec tab's own reads of its
 *  document would go out unanswered. */
const childrenTab = "/issues/CORE-1/children";

// A title save the reader stopped may have been saved, so the header does not say the update
// failed. Its Retry is the title field's own submit (`drafts.retry`): a press of its own, of what
// the field holds now, and the header says that.
test("IssuePage says what Retry does after a title save the reader stopped", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const { patchIssue, restore } = stubIssueApi();
  patchIssue.mockRejectedValue(unreachable);
  const { queryClient, unmount } = renderIssuePage(childrenTab);
  queryClient.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });

  try {
    await user.click(
      await screen.findByRole("button", { description: "Review the spec", name: "Edit title" })
    );
    const title = (await screen.findByLabelText("Issue title")) as HTMLInputElement;
    await user.clear(title);
    await user.click(title);
    await user.paste("Renamed");
    // Blur has no user-event form; it is what the title editor saves on.
    fireEvent.blur(title);
    await stopTheLateWrite(queryClient);

    // The alert's paragraph, without its Retry button's name.
    expect(screen.getByRole("alert").querySelector("p")?.textContent).toBe(
      stoppedWriteNote("this change to the issue", "saved", {
        anew: "Retry sends what the field holds now as a new write.",
      })
    );
  } finally {
    unmount();
    restore();
    realClock();
  }
});

// A parent save the reader stopped may have been saved. The header shows a parent save's failure
// on a line of its own, with no Retry: saving the parent again is a press of its own, and the line
// says that rather than that the save failed.
test("IssuePage says what saving the parent again does after a save the reader stopped", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const { patchIssue, restore } = stubIssueApi();
  patchIssue.mockRejectedValue(unreachable);
  const { queryClient, unmount } = renderIssuePage(childrenTab);
  queryClient.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });

  try {
    await openIssueDetails(user);
    await user.click(await screen.findByRole("button", { name: "None, set parent issue" }));
    await user.click(await screen.findByLabelText("Parent"));
    await user.paste("CORE-9");
    await user.click(screen.getByRole("button", { name: "Save parent" }));
    await stopTheLateWrite(queryClient);

    expect(document.getElementById("issue-parent-error")?.textContent).toBe(
      stoppedWriteNote("the new parent", "saved", {
        anew: "Saving it again sends it as a new write.",
      })
    );
  } finally {
    unmount();
    restore();
    realClock();
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
    const status = await screen.findByRole("button", { name: pickNamed("Status") });
    let moved: Promise<void> | undefined;
    act(() => {
      moved = moveCard?.("CORE-1", "in_progress", 0);
    });
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(1));
    await pickIn(user, () => status, "Backlog");
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
