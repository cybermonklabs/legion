import { afterEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { MemoryRouter, useLocation } from "react-router-dom";
import { ApiError, api } from "../../api/client";
import { boardMoveMutationKey, projectIssuesQuery } from "../../api/queries";
import type { CreateIssueInput, Issue, IssueDetails, IssueSummary } from "../../api/types";
import { KeymapProvider } from "../shell/KeymapProvider";
import { useKeymapScope } from "../shell/keymap";
import { SendStoppedError } from "../writes/keyed-write";
import { writesOf } from "../writes/write-store";
import { IssuesTable } from "./IssuesTable";

function issue(key: string, overrides: Partial<IssueSummary> = {}): IssueSummary {
  return {
    key,
    title: `Work ${key}`,
    status: "todo",
    priority: null,
    rank: key,
    labels: [],
    parent: null,
    blocked_by: [],
    assignee: null,
    claim: null,
    components: { mode: "inherit", ids: [], unknown: [], reason: null, inherited_from: null },
    route: null,
    repo: null,
    resolved_repo: null,
    route_status: null,
    route_holder: null,
    route_status_reason: null,
    last_seq: 2,
    open_asks: 0,
    progress: { tasks: null, children: null },
    primary_artifact_id: "spec",
    created_by: { kind: "user", id: "alice" },
    created_at: "2026-09-10T00:00:00Z",
    updated_at: "2026-09-10T00:00:00Z",
    ...overrides,
  };
}
function savedIssue(key: string): Issue {
  return {
    ...issue(key),
    labels: [],
    project: "CORE",
    number: Number(key.split("-")[1]),
    closed_at: null,
    external_links: [],
  };
}
function Route(): ReactNode {
  useKeymapScope("project");
  return <output data-testid="route">{useLocation().pathname}</output>;
}
const restores: (() => void)[] = [];
afterEach(() => {
  for (const restore of restores.splice(0)) restore();
  localStorage.clear();
});
function table(issues: IssueSummary[], search = "", full?: Promise<IssueSummary[]>) {
  const settings = spyOn(api, "listLegionSettings").mockResolvedValue([]);
  restores.push(() => settings.mockRestore());
  const parents = [
    ...new Set(
      issues.flatMap((row) =>
        row.parent && !issues.some((candidate) => candidate.key === row.parent) ? [row.parent] : []
      )
    ),
  ].map((key) => issue(key, { status: "triage" }));
  const listing = spyOn(api, "listIssues").mockImplementation(async (options = {}) =>
    !options.open && full
      ? full
      : [...issues, ...parents].filter((issue) => !options.open || issue.status !== "done")
  );
  const state = spyOn(api, "getMyState").mockResolvedValue({});
  const agents = spyOn(api, "listAgents").mockResolvedValue([]);
  const viewer = spyOn(api, "whoAmI").mockResolvedValue({
    event_head: "0",
    login: "alice",
    kind: "user",
  });
  // The unit DOM has no layout engine. Supply only the browser's viewport measurements,
  // leaving filtering, rendering, focus and navigation to the actual table.
  const descriptor = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "clientHeight");
  Object.defineProperty(HTMLElement.prototype, "clientHeight", {
    configurable: true,
    get() {
      return this.dataset.testid === "issues-table-scroll"
        ? 600
        : (descriptor?.get?.call(this) ?? 0);
    },
  });
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(["whoami"], { event_head: "0", login: "alice", kind: "user" });
  const view = render(
    <MemoryRouter initialEntries={[`/projects/CORE/issues${search}`]}>
      <QueryClientProvider client={client}>
        <KeymapProvider>
          <Route />
          <IssuesTable project="CORE" />
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  restores.push(() => {
    view.unmount();
    client.clear();
    listing.mockRestore();
    state.mockRestore();
    agents.mockRestore();
    viewer.mockRestore();
    if (descriptor) Object.defineProperty(HTMLElement.prototype, "clientHeight", descriptor);
    else Reflect.deleteProperty(HTMLElement.prototype, "clientHeight");
  });
  return { client, listing };
}

test("rows keep parent links, unread marks, asks, progress and route reach, with priority editable", async () => {
  table([
    issue("CORE-1", {
      parent: "CORE-9",
      open_asks: 2,
      progress: { tasks: { done: 3, total: 7 }, children: { done: 1, total: 2 } },
      route: "role:sre",
      route_status: "no_holder",
    }),
  ]);
  const row = await screen.findByRole("row", { name: "CORE-1 Work CORE-1" });
  expect(within(row).getByRole("link", { name: "CORE-9" }).getAttribute("href")).toBe(
    "/issues/CORE-9"
  );
  expect(within(row).getByTestId("issue-progress-tasks").textContent).toBe("3/7 tasks");
  expect(within(row).getByTestId("issue-progress-children").textContent).toBe("1/2 children");
  expect(within(row).getAllByText("2", { exact: true }).length).toBeGreaterThan(0);
  expect(within(row).getByTestId("issue-route-unreachable").textContent).toContain(
    "Nobody holds it right now"
  );
  expect(within(row).getByTitle("Unread")).toBeDefined();
  expect(within(row).getByRole("combobox", { name: "Priority of CORE-1" })).toBeDefined();
});

test("j and k focus table issues and o and Enter open the issue rather than its parent", async () => {
  table([issue("CORE-1", { parent: "CORE-9" }), issue("CORE-2")]);
  const first = await screen.findByRole("row", { name: "CORE-1 Work CORE-1" });
  const second = screen.getByRole("row", { name: "CORE-2 Work CORE-2" });
  fireEvent.keyDown(window, { key: "j" });
  expect(document.activeElement).toBe(first);
  fireEvent.keyDown(window, { key: "j" });
  expect(document.activeElement).toBe(second);
  fireEvent.keyDown(window, { key: "k" });
  expect(document.activeElement).toBe(first);
  fireEvent.keyDown(window, { key: "o" });
  await waitFor(() => expect(screen.getByTestId("route").textContent).toBe("/issues/CORE-1"));
  second.focus();
  fireEvent.keyDown(window, { key: "Enter" });
  await waitFor(() => expect(screen.getByTestId("route").textContent).toBe("/issues/CORE-2"));
});

test("typed title and every picked label match case-insensitively, as on Board", async () => {
  table(
    [
      issue("CORE-1", { title: "Search both", labels: ["FrontEnd", "BACKEND"] }),
      issue("CORE-2", { title: "Search backend", labels: ["backend"] }),
      issue("CORE-3", { title: "Other", labels: ["frontend", "backend"] }),
    ],
    "?label=frontend&label=backend&f.title=SEARCH"
  );
  await screen.findByRole("row", { name: "CORE-1 Search both" });
  expect(screen.queryByRole("row", { name: "CORE-2 Search backend" })).toBeNull();
  expect(screen.queryByRole("row", { name: "CORE-3 Other" })).toBeNull();
});

test("opening a Labels facet does not pass its values through another column's formatter", async () => {
  table([issue("CORE-1", { labels: ["frontend"] })]);
  const trigger = await screen.findByRole("button", { name: "Filter Labels" });
  fireEvent.click(trigger);
  expect(await screen.findByRole("combobox", { name: "Search Labels" })).toBeDefined();
  expect(screen.getByRole("option", { name: "frontend, 1 issue" })).toBeDefined();
});

test("Created sorts RFC3339 fractional timestamps by instant, not by text", async () => {
  table(
    [
      issue("CORE-1", { created_at: "2026-09-10T00:00:00.1Z" }),
      issue("CORE-2", { created_at: "2026-09-10T00:00:00Z" }),
    ],
    "?sort=created"
  );
  await screen.findByRole("row", { name: "CORE-2 Work CORE-2" });
  expect(
    screen
      .getAllByRole("row")
      .filter((row) => row.hasAttribute("data-issue-row"))
      .map((row) => row.getAttribute("data-issue-row"))
  ).toEqual(["CORE-2", "CORE-1"]);
});

test("a table rank write is held by the durable store on the issue's write line", async () => {
  const response = Promise.withResolvers<Response>();
  const send = spyOn(globalThis, "fetch").mockReturnValue(response.promise);
  const { client } = table([
    issue("CORE-1", { labels: ["legion"] }),
    issue("CORE-2", { labels: ["legion"] }),
  ]);
  restores.push(() => send.mockRestore());
  fireEvent.click(await screen.findByRole("checkbox", { name: "Select CORE-2" }));
  fireEvent.click(within(screen.getByRole("toolbar")).getByRole("button", { name: "Start next" }));
  await waitFor(() => expect(writesOf(client).rows().length).toBe(1));
  expect(writesOf(client).rows()[0]?.target).toBe("issue:CORE-2");
  expect(writesOf(client).rows()[0]?.login).toBe("alice");
  response.resolve(
    new Response(JSON.stringify(savedIssue("CORE-2")), {
      status: 200,
      headers: { "Content-Type": "application/json" },
    })
  );
  await waitFor(() =>
    expect(
      within(screen.getByRole("region", { name: "Project issues" })).getByRole("status").textContent
    ).toContain("now starts next")
  );
});

test("an epic creation committed without an answer never sends parent writes to an unknown issue", async () => {
  const create = spyOn(api, "createIssue").mockRejectedValue(
    new ApiError(409, { code: "KEY_COMMITTED", error: "already made" })
  );
  const send = spyOn(api, "send").mockRejectedValue(new Error("Unexpected parent write"));
  const prompt = spyOn(window, "prompt").mockReturnValue("Made once");
  table([issue("CORE-1")]);
  restores.push(() => {
    create.mockRestore();
    send.mockRestore();
    prompt.mockRestore();
  });
  fireEvent.click(await screen.findByRole("checkbox", { name: "Select CORE-1" }));
  fireEvent.click(screen.getByRole("button", { name: "New epic from these" }));
  expect((await screen.findByTestId("landed-without-answer")).textContent).toContain(
    "The selected issues have not been moved"
  );
  expect(send).not.toHaveBeenCalled();
  expect(
    screen.getByRole<HTMLButtonElement>("button", { name: "New epic from these" }).disabled
  ).toBe(true);
  expect(create.mock.calls.length).toBe(1);
  expect(screen.queryByRole("alert")).toBeNull();
});

test("a possible duplicate offers an explicit force choice rather than forcing the first create", async () => {
  const calls: CreateIssueInput[] = [];
  const create = spyOn(api, "createIssue").mockImplementation(async (input) => {
    calls.push(input);
    if (!input.force)
      throw new ApiError(409, {
        code: "POSSIBLE_DUPLICATE",
        error: "Possible duplicate of CORE-9",
      });
    return savedIssue("CORE-9");
  });
  const send = spyOn(api, "send").mockResolvedValue(savedIssue("CORE-1"));
  const prompt = spyOn(window, "prompt").mockReturnValue("Existing epic");
  table([issue("CORE-1")]);
  restores.push(() => {
    create.mockRestore();
    send.mockRestore();
    prompt.mockRestore();
  });
  fireEvent.click(await screen.findByRole("checkbox", { name: "Select CORE-1" }));
  fireEvent.click(screen.getByRole("button", { name: "New epic from these" }));
  expect((await screen.findByRole("alert")).textContent).toContain("CORE-9");
  fireEvent.click(screen.getByRole("button", { name: "Create anyway" }));
  await waitFor(() =>
    expect(
      within(screen.getByRole("region", { name: "Project issues" })).getByRole("status").textContent
    ).toContain("Made the epic")
  );
  expect(calls.map((input) => input.force)).toEqual([undefined, true]);
});

test("a stopped epic creation retries its committed request under the same key", async () => {
  const rows = [issue("CORE-1")];
  const created = new Map<string, Issue>();
  const keys: string[] = [];
  let stopped = false;
  const create = spyOn(api, "createIssue").mockImplementation(async (input, bounds) => {
    keys.push(bounds.key);
    bounds.sent?.({ method: "POST", path: "/api/v1/issues", body: input });
    if (!created.has(bounds.key)) {
      const epic = savedIssue("CORE-9");
      created.set(bounds.key, epic);
      rows.push({ ...issue(epic.key), ...epic });
    }
    if (!stopped) {
      stopped = true;
      throw new SendStoppedError();
    }
    return created.get(bounds.key) as Issue;
  });
  const send = spyOn(api, "send").mockImplementation(async (request) => {
    const row = rows.find((value) => request.path.endsWith(value.key));
    if (!row) throw new Error("Missing issue");
    Object.assign(row, request.body);
    return { ...savedIssue(row.key), ...row } as never;
  });
  const prompt = spyOn(window, "prompt").mockReturnValue("Committed once");
  restores.push(() => {
    create.mockRestore();
    send.mockRestore();
    prompt.mockRestore();
  });
  const { client } = table(rows);
  fireEvent.click(await screen.findByRole("checkbox", { name: "Select CORE-1" }));
  fireEvent.click(screen.getByRole("button", { name: "New epic from these" }));
  await screen.findByRole("alert");
  const refused = writesOf(client)
    .rows()
    .find((write) => write.status === "refused");
  expect(refused).toBeDefined();
  if (!refused) throw new Error("The stopped creation was dropped");
  writesOf(client).retry(refused.id);
  await waitFor(() => expect(keys).toHaveLength(2));
  expect(keys[1]).toBe(keys[0]);
  expect(created.size).toBe(1);
  await waitFor(() => expect(screen.queryByRole("alert")).toBeNull());
  const recovery = screen.getByRole("region", { name: "Interrupted table action" });
  expect(recovery.textContent).toContain("not yet adopted");
  fireEvent.click(within(recovery).getByRole("button", { name: "Finish" }));
  await waitFor(() => expect(rows[0]?.parent).toBe("CORE-9"));
  await waitFor(() =>
    expect(screen.queryByRole("region", { name: "Interrupted table action" })).toBeNull()
  );
});

test("dismissing an interrupted action preserves layout and sends none of its future steps", async () => {
  localStorage.setItem(
    "dispatch.project-table.CORE",
    JSON.stringify({
      sizing: { title: 360 },
      action: {
        id: "unfinished",
        kind: "startNext",
        project: "CORE",
        keys: ["CORE-1"],
        writes: {},
      },
    })
  );
  const send = spyOn(api, "send").mockResolvedValue(savedIssue("CORE-1") as never);
  restores.push(() => send.mockRestore());
  table([issue("CORE-1")]);
  const recovery = await screen.findByRole("region", { name: "Interrupted table action" });
  fireEvent.click(within(recovery).getByRole("button", { name: "Dismiss" }));
  expect(screen.queryByRole("region", { name: "Interrupted table action" })).toBeNull();
  const saved = JSON.parse(localStorage.getItem("dispatch.project-table.CORE") ?? "{}");
  expect(saved.action).toBeUndefined();
  expect(saved.sizing.title).toBe(360);
  expect(send).not.toHaveBeenCalled();
});

test("the open table read waits behind an unanswered Board move", async () => {
  const { client, listing } = table([issue("CORE-1")]);
  await screen.findByRole("row", { name: "CORE-1 Work CORE-1" });
  const gate = Promise.withResolvers<void>();
  const started = Promise.withResolvers<void>();
  const move = client
    .getMutationCache()
    .build(client, {
      mutationKey: boardMoveMutationKey("CORE"),
      mutationFn: () => {
        started.resolve();
        return gate.promise;
      },
    })
    .execute(undefined);
  await started.promise;
  const calls = listing.mock.calls.length;
  const reading = client.fetchQuery(projectIssuesQuery("CORE", [], true));
  await Promise.resolve();
  expect(listing.mock.calls.length).toBe(calls);
  gate.resolve();
  await move;
  await reading;
  expect(listing.mock.calls.length).toBe(calls + 1);
});

test("a failed project listing can be retried without navigating away", async () => {
  const { client, listing } = table([issue("CORE-1")]);
  await screen.findByRole("row", { name: "CORE-1 Work CORE-1" });
  listing.mockRejectedValueOnce(new Error("unreachable"));
  await client.invalidateQueries({ queryKey: ["issues"] });
  await screen.findByRole("alert");
  fireEvent.click(screen.getByRole("button", { name: "Retry loading issues" }));
  expect(await screen.findByRole("row", { name: "CORE-1 Work CORE-1" })).toBeDefined();
});

test("Title and Columns popovers allow internal Tab, close on focus leaving, and restore focus on Escape", async () => {
  table([issue("CORE-1")]);
  const title = await screen.findByRole("button", { name: "Filter Title" });
  fireEvent.click(title);
  const dialog = screen.getByRole("dialog", { name: "Filter Title" });
  expect(screen.getByTestId("issues-table-scroll").contains(dialog)).toBe(false);
  fireEvent.keyDown(document, { key: "Escape" });
  expect(screen.queryByRole("dialog")).toBeNull();
  expect(document.activeElement).toBe(title);
  const columns = screen.getByRole("button", { name: "Columns" });
  fireEvent.click(columns);
  expect(screen.getByRole("dialog", { name: "Columns" })).toBeDefined();
  fireEvent.keyDown(document, { key: "Tab" });
  expect(screen.getByRole("dialog", { name: "Columns" })).toBeDefined();
  const controls = within(screen.getByRole("dialog", { name: "Columns" })).getAllByRole("checkbox");
  controls[1]?.focus();
  expect(screen.getByRole("dialog", { name: "Columns" })).toBeDefined();
  title.focus();
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
});

test("a partially refused batch keeps selection and identifies the writes already saved", async () => {
  const send = spyOn(api, "send")
    .mockResolvedValueOnce(savedIssue("CORE-1"))
    .mockRejectedValueOnce(new ApiError(403, { error: "refused", code: "FORBIDDEN" }));
  table([issue("CORE-1"), issue("CORE-2")]);
  restores.push(() => send.mockRestore());
  fireEvent.click(await screen.findByRole("checkbox", { name: "Select CORE-1" }));
  fireEvent.click(screen.getByRole("checkbox", { name: "Select CORE-2" }));
  fireEvent.click(screen.getByRole("button", { name: "Take out of epic" }));
  expect((await screen.findByRole("alert")).textContent).toContain("1 of 2 changes saved.");
  expect(screen.getByRole<HTMLInputElement>("checkbox", { name: "Select CORE-1" }).checked).toBe(
    true
  );
  expect(screen.getByRole("row", { name: "CORE-1 Work CORE-1" }).getAttribute("data-moved")).toBe(
    "true"
  );
});

test("Clear filters also clears Needs you, Unread and Unclaimed", async () => {
  table([issue("CORE-1", { open_asks: 2 })], "?needs-you=1&unread=1&unclaimed=1");
  fireEvent.click(await screen.findByRole("button", { name: "Clear filters" }));
  for (const name of ["Needs you", "Unread", "Unclaimed"]) {
    await waitFor(() =>
      expect(screen.getByRole("button", { name }).getAttribute("aria-pressed")).toBe("false")
    );
  }
});

test("the Done facet does not claim an unloaded count is zero", async () => {
  table([issue("CORE-1"), issue("CORE-2", { status: "done" })]);
  fireEvent.click(await screen.findByRole("button", { name: /^Filter Status/ }));
  expect(screen.getByRole("option", { name: "Done, not loaded" })).toBeDefined();
});

test("loading closed issues retains the open rows and controls, including after a refusal", async () => {
  const { listing } = table([issue("CORE-1")]);
  await screen.findByRole("row", { name: "CORE-1 Work CORE-1" });
  const closed = Promise.withResolvers<IssueSummary[]>();
  listing.mockReturnValueOnce(closed.promise);
  fireEvent.click(screen.getByRole("button", { name: "Clear filters" }));
  expect(screen.getByRole("table", { name: "Project issues table" })).toBeDefined();
  expect(screen.getByRole("searchbox", { name: "Search issues" })).toBeDefined();
  closed.reject(new Error("closed listing refused"));
  await screen.findByRole("alert");
  expect(screen.getByRole("row", { name: "CORE-1 Work CORE-1" })).toBeDefined();
  expect(screen.getByRole("button", { name: "Retry loading closed issues" })).toBeDefined();
});

test("an epic's Start all includes queued siblings hidden by a priority filter", async () => {
  const written: string[] = [];
  const send = spyOn(api, "send").mockImplementation(async (request) => {
    const key = request.path.split("/").at(-1) ?? "";
    written.push(key);
    return savedIssue(key) as never;
  });
  table(
    [
      issue("CORE-1", { labels: ["epic"] }),
      issue("CORE-2", { parent: "CORE-1", priority: 0, labels: ["legion"] }),
      issue("CORE-3", { parent: "CORE-1", priority: 1, labels: ["legion"] }),
      issue("CORE-9", { labels: ["legion"], rank: "0" }),
    ],
    "?group=epic&f.priority=0"
  );
  restores.push(() => send.mockRestore());
  fireEvent.click(await screen.findByRole("button", { name: "Epic actions ▾" }));
  fireEvent.click(screen.getByRole("button", { name: "Start all its queued issues next" }));
  await waitFor(() => expect(new Set(written)).toEqual(new Set(["CORE-2", "CORE-3"])));
});

test("a refused bulk operation retries independently while the other operations land", async () => {
  const sent: string[] = [];
  let refused = false;
  const send = spyOn(api, "send").mockImplementation(async (request) => {
    const key = request.path.split("/").at(-1) ?? "";
    sent.push(key);
    if (key === "CORE-2" && !refused) {
      refused = true;
      throw new ApiError(403, { error: "refused", code: "FORBIDDEN" });
    }
    return savedIssue(key) as never;
  });
  restores.push(() => send.mockRestore());
  const { client, listing } = table([issue("CORE-1"), issue("CORE-2"), issue("CORE-3")]);
  await screen.findByRole("checkbox", { name: "Select CORE-1" });
  for (const key of ["CORE-1", "CORE-2", "CORE-3"])
    fireEvent.click(screen.getByRole("checkbox", { name: `Select ${key}` }));
  const before = listing.mock.calls.length;
  fireEvent.click(screen.getByRole("button", { name: "Take out of epic" }));
  await screen.findByRole("alert");
  expect(sent).toEqual(["CORE-1", "CORE-2", "CORE-3"]);
  expect(
    writesOf(client)
      .rows()
      .map((write) => write.label)
  ).toEqual(["Update CORE-2 from the issues table"]);
  expect(listing.mock.calls.length - before).toBe(1);
  const retry = writesOf(client)
    .rows()
    .find((write) => write.status === "refused");
  if (!retry) throw new Error("The refused operation was lost");
  writesOf(client).retry(retry.id);
  await waitFor(() => expect(sent).toEqual(["CORE-1", "CORE-2", "CORE-3", "CORE-2"]));
  await waitFor(() => expect(writesOf(client).rows()).toHaveLength(0));
  await waitFor(() => expect(screen.queryByRole("alert")).toBeNull());
  expect(
    within(screen.getByRole("region", { name: "Project issues" })).getByRole("status").textContent
  ).toContain("All changes saved");
});

test("a live unseen ancestor keeps the same scroll box and marks unknown ancestry on failure", async () => {
  const read = Promise.withResolvers<IssueDetails>();
  const get = spyOn(api, "getIssue").mockReturnValue(read.promise);
  restores.push(() => get.mockRestore());
  const { client } = table([issue("CORE-1")]);
  await screen.findByRole("row", { name: "CORE-1 Work CORE-1" });
  const box = screen.getByTestId("issues-table-scroll");
  box.scrollTop = 200;
  box.scrollLeft = 300;
  client.setQueryData(projectIssuesQuery("CORE", [], true).queryKey, [
    issue("CORE-1", { parent: "CORE-9" }),
  ]);
  await waitFor(() => expect(get).toHaveBeenCalled());
  expect(screen.getByTestId("issues-table-scroll")).toBe(box);
  expect(box.scrollTop).toBe(200);
  read.reject(new Error("ancestor unreachable"));
  await screen.findByText("Unknown epic");
  expect(screen.getByTestId("issues-table-scroll")).toBe(box);
  expect(box.scrollLeft).toBe(300);
});

test("a late Done listing applies the requested URL page instead of clamping it early", async () => {
  const done = Promise.withResolvers<IssueSummary[]>();
  table([issue("CORE-1")], "?status=done&page=3&size=25", done.promise);
  await screen.findByText("Loading closed issues…");
  done.resolve(
    Array.from({ length: 80 }, (_, index) => issue(`CORE-${index + 2}`, { status: "done" }))
  );
  await screen.findByRole("row", { name: "CORE-52 Work CORE-52" });
  expect(screen.getByText("Page 3 of 4")).toBeDefined();
});

test("parent-changing actions skip closed selections and New epic refuses an all-closed selection", async () => {
  const send = spyOn(api, "send").mockResolvedValue(savedIssue("CORE-1"));
  const create = spyOn(api, "createIssue").mockRejectedValue(new Error("Unexpected create"));
  const prompt = spyOn(window, "prompt").mockReturnValue("Should not be created");
  restores.push(() => {
    send.mockRestore();
    create.mockRestore();
    prompt.mockRestore();
  });
  table([issue("CORE-1"), issue("CORE-2", { status: "done" })], "?nofilter=1");
  fireEvent.click(await screen.findByRole("checkbox", { name: "Select CORE-1" }));
  fireEvent.click(screen.getByRole("checkbox", { name: "Select CORE-2" }));
  fireEvent.click(screen.getByRole("button", { name: "Take out of epic" }));
  await screen.findByText(/Skipped 1 closed issue/);
  expect(send.mock.calls.map(([request]) => request.path)).toEqual(["/api/v1/issues/CORE-1"]);
  fireEvent.click(screen.getByRole("checkbox", { name: "Select CORE-2" }));
  fireEvent.click(screen.getByRole("button", { name: "New epic from these" }));
  expect((await screen.findByRole("alert")).textContent).toContain(
    "Select at least one open issue"
  );
  expect(create).not.toHaveBeenCalled();
  expect(prompt).not.toHaveBeenCalled();
});

test("created-by sessions use their live name and state, and a null creator is Unknown", async () => {
  table([
    issue("CORE-1", {
      created_by: {
        kind: "session",
        id: "table-creator",
        origin: { session_title: "Table author" },
      },
    }),
    issue("CORE-2", { created_by: null }),
  ]);
  const row = await screen.findByRole("row", { name: "CORE-1 Work CORE-1" });
  expect(
    within(row)
      .getByRole("link", { name: /Table author/ })
      .getAttribute("href")
  ).toContain("table-creator");
  expect(within(row).getByRole("img", { name: /running/i })).toBeDefined();
  expect(
    within(screen.getByRole("row", { name: "CORE-2 Work CORE-2" })).getByText("Unknown")
  ).toBeDefined();
});

test("a refused merge move leaves the source open and does not post the close reason", async () => {
  const requests: { path: string; body: unknown }[] = [];
  const send = spyOn(api, "send").mockImplementation(async (request) => {
    requests.push({ path: request.path, body: request.body });
    if (request.path.endsWith("CORE-3")) throw new ApiError(403, { error: "Move refused" });
    return savedIssue("CORE-4") as never;
  });
  const message = spyOn(api, "createMessage").mockRejectedValue(
    new Error("Unexpected close reason")
  );
  restores.push(() => {
    send.mockRestore();
    message.mockRestore();
  });
  table(
    [
      issue("CORE-1", { labels: ["epic"], title: "Source" }),
      issue("CORE-2", { labels: ["epic"], title: "Target" }),
      issue("CORE-3", { parent: "CORE-1" }),
      issue("CORE-4", { parent: "CORE-1" }),
    ],
    "?group=epic"
  );
  fireEvent.click(await screen.findByRole("button", { name: "Epic actions ▾" }));
  fireEvent.change(screen.getByRole("combobox", { name: "Merge into another epic" }), {
    target: { value: "CORE-2" },
  });
  expect((await screen.findByRole("alert")).textContent).toContain("CORE-3");
  expect(requests.map(({ path }) => path)).toEqual([
    "/api/v1/issues/CORE-3",
    "/api/v1/issues/CORE-4",
  ]);
  expect(message).not.toHaveBeenCalled();
});

test("ancestor reads have one retry policy rather than multiplying inner and outer retries", async () => {
  const get = spyOn(api, "getIssue").mockRejectedValue(new Error("Unavailable ancestor"));
  restores.push(() => get.mockRestore());
  const { client } = table([issue("CORE-1")]);
  await screen.findByRole("row", { name: "CORE-1 Work CORE-1" });
  client.setDefaultOptions({ queries: { retry: 2, retryDelay: 1 } });
  client.setQueryData(projectIssuesQuery("CORE", [], true).queryKey, [
    issue("CORE-1", { parent: "CORE-9" }),
  ]);
  await screen.findByText(/Could not load some ancestors/);
  expect(get.mock.calls.length).toBe(3);
});

test("Start next stops at a refused predecessor and names every issue that did not move up", async () => {
  const sent: string[] = [];
  const send = spyOn(api, "send").mockImplementation(async (request) => {
    sent.push(request.path);
    throw new ApiError(403, { error: "Rank refused" });
  });
  restores.push(() => send.mockRestore());
  const { client } = table([
    issue("CORE-1", { labels: ["legion"], rank: "A" }),
    issue("CORE-2", { labels: ["legion"], rank: "B" }),
    issue("CORE-3", { labels: ["legion"], rank: "C" }),
  ]);
  fireEvent.click(await screen.findByRole("checkbox", { name: "Select CORE-2" }));
  fireEvent.click(screen.getByRole("checkbox", { name: "Select CORE-3" }));
  fireEvent.click(within(screen.getByRole("toolbar")).getByRole("button", { name: "Start next" }));
  expect((await screen.findByRole("alert")).textContent).toContain(
    "Did not move up: CORE-3, CORE-2"
  );
  expect(sent).toEqual(["/api/v1/issues/CORE-3"]);
  expect(writesOf(client).rows()).toHaveLength(1);
});
