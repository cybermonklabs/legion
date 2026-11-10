import { afterEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";
import type { ReactNode } from "react";
import { MemoryRouter, useLocation } from "react-router-dom";
import { ApiError, api } from "../../api/client";
import { boardMoveMutationKey, projectIssuesQuery } from "../../api/queries";
import type { CreateIssueInput, Issue, IssueDetails, IssueSummary } from "../../api/types";
import { readSelectedIssues } from "../search/bar-scope";
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
    decisions_artifact_id: "",
    plan_artifact_id: "",
    verification_artifact_id: "",
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
  return { client, listing, view };
}

/** Fills in the in-page dialog an epic is named or renamed through, once it opens. */
async function nameTheEpic(user: UserEvent, name: string) {
  const dialog = await screen.findByRole("dialog", { name: /epic/i });
  const field = within(dialog).getByRole("textbox", { name: "Name" });
  await user.clear(field);
  await user.type(field, name);
  await user.click(within(dialog).getByRole("button", { name: /^(Create|Rename) epic$/ }));
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
  const user = userEvent.setup();
  table([issue("CORE-1", { parent: "CORE-9" }), issue("CORE-2")]);
  const first = await screen.findByRole("row", { name: "CORE-1 Work CORE-1" });
  const second = screen.getByRole("row", { name: "CORE-2 Work CORE-2" });
  await user.keyboard("j");
  expect(first).toHaveFocus();
  await user.keyboard("j");
  expect(second).toHaveFocus();
  await user.keyboard("k");
  expect(first).toHaveFocus();
  await user.keyboard("o");
  await waitFor(() => expect(screen.getByTestId("route").textContent).toBe("/issues/CORE-1"));
  second.focus();
  await user.keyboard("{Enter}");
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
  const user = userEvent.setup();
  table([issue("CORE-1", { labels: ["frontend"] })]);
  const trigger = await screen.findByRole("button", { name: "Filter Labels" });
  await user.click(trigger);
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
  const user = userEvent.setup();
  const response = Promise.withResolvers<Response>();
  const send = spyOn(globalThis, "fetch").mockReturnValue(response.promise);
  const { client } = table([
    issue("CORE-1", { labels: ["legion"] }),
    issue("CORE-2", { labels: ["legion"] }),
  ]);
  restores.push(() => send.mockRestore());
  await user.click(await screen.findByRole("checkbox", { name: "Select CORE-2" }));
  await user.click(within(screen.getByRole("toolbar")).getByRole("button", { name: "Start next" }));
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
  const user = userEvent.setup();
  const create = spyOn(api, "createIssue").mockRejectedValue(
    new ApiError(409, { code: "KEY_COMMITTED", error: "already made" })
  );
  const send = spyOn(api, "send").mockRejectedValue(new Error("Unexpected parent write"));
  const named = nameTheEpic(user, "Made once");
  table([issue("CORE-1")]);
  restores.push(() => {
    create.mockRestore();
    send.mockRestore();
  });
  await user.click(await screen.findByRole("checkbox", { name: "Select CORE-1" }));
  await user.click(screen.getByRole("button", { name: "New epic from these" }));
  await named;
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
  const user = userEvent.setup();
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
  const named = nameTheEpic(user, "Existing epic");
  table([issue("CORE-1")]);
  restores.push(() => {
    create.mockRestore();
    send.mockRestore();
  });
  await user.click(await screen.findByRole("checkbox", { name: "Select CORE-1" }));
  await user.click(screen.getByRole("button", { name: "New epic from these" }));
  await named;
  expect((await screen.findByRole("alert")).textContent).toContain("CORE-9");
  await user.click(screen.getByRole("button", { name: "Create anyway" }));
  await waitFor(() =>
    expect(
      within(screen.getByRole("region", { name: "Project issues" })).getByRole("status").textContent
    ).toContain("Made the epic")
  );
  expect(calls.map((input) => input.force)).toEqual([undefined, true]);
});

test("a stopped epic creation retries its committed request under the same key", async () => {
  const user = userEvent.setup();
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
  const named = nameTheEpic(user, "Committed once");
  restores.push(() => {
    create.mockRestore();
    send.mockRestore();
  });
  const { client } = table(rows);
  await user.click(await screen.findByRole("checkbox", { name: "Select CORE-1" }));
  await user.click(screen.getByRole("button", { name: "New epic from these" }));
  await named;
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
  await user.click(within(recovery).getByRole("button", { name: "Finish" }));
  await waitFor(() => expect(rows[0]?.parent).toBe("CORE-9"));
  await waitFor(() =>
    expect(screen.queryByRole("region", { name: "Interrupted table action" })).toBeNull()
  );
});

test("dismissing an interrupted action preserves layout and sends none of its future steps", async () => {
  const user = userEvent.setup();
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
  await user.click(within(recovery).getByRole("button", { name: "Dismiss" }));
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
  const user = userEvent.setup();
  const { client, listing } = table([issue("CORE-1")]);
  await screen.findByRole("row", { name: "CORE-1 Work CORE-1" });
  listing.mockRejectedValueOnce(new Error("unreachable"));
  await client.invalidateQueries({ queryKey: ["issues"] });
  await screen.findByRole("alert");
  await user.click(screen.getByRole("button", { name: "Retry loading issues" }));
  expect(await screen.findByRole("row", { name: "CORE-1 Work CORE-1" })).toBeDefined();
});

// Every panel the table hangs off a control - a header's filter, Columns, Epic actions - and
// the shared MultiSelect dismiss by one set of rules: a press outside, or Escape, which puts
// focus back on the control that opened it.
test("Title and Columns popovers allow internal Tab, dismiss on an outside press and on Escape", async () => {
  const user = userEvent.setup();
  table([issue("CORE-1")]);
  const title = await screen.findByRole("button", { name: "Filter Title" });
  await user.click(title);
  const dialog = screen.getByRole("dialog", { name: "Filter Title" });
  expect(screen.getByTestId("issues-table-scroll").contains(dialog)).toBe(false);
  await user.keyboard("{Escape}");
  expect(screen.queryByRole("dialog")).toBeNull();
  expect(document.activeElement).toBe(title);
  const columns = screen.getByRole("button", { name: "Columns" });
  await user.click(columns);
  expect(screen.getByRole("dialog", { name: "Columns" })).toBeDefined();
  await user.tab();
  expect(screen.getByRole("dialog", { name: "Columns" })).toBeDefined();
  const controls = within(screen.getByRole("dialog", { name: "Columns" })).getAllByRole("checkbox");
  controls[1]?.focus();
  expect(screen.getByRole("dialog", { name: "Columns" })).toBeDefined();
  await user.click(screen.getByRole("searchbox", { name: "Search issues" }));
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
});

test("a partially refused batch keeps selection and identifies the writes already saved", async () => {
  const user = userEvent.setup();
  const send = spyOn(api, "send")
    .mockResolvedValueOnce(savedIssue("CORE-1"))
    .mockRejectedValueOnce(new ApiError(403, { error: "refused", code: "FORBIDDEN" }));
  table([issue("CORE-1"), issue("CORE-2")]);
  restores.push(() => send.mockRestore());
  await user.click(await screen.findByRole("checkbox", { name: "Select CORE-1" }));
  await user.click(screen.getByRole("checkbox", { name: "Select CORE-2" }));
  await user.click(screen.getByRole("button", { name: "Take out of epic" }));
  expect((await screen.findByRole("alert")).textContent).toContain("1 of 2 changes saved.");
  expect(screen.getByRole<HTMLInputElement>("checkbox", { name: "Select CORE-1" }).checked).toBe(
    true
  );
  expect(screen.getByRole("row", { name: "CORE-1 Work CORE-1" }).getAttribute("data-moved")).toBe(
    "true"
  );
});

test("Clear filters also clears Needs you, Unread and the lapsed-claim filter", async () => {
  const user = userEvent.setup();
  table([issue("CORE-1", { open_asks: 2 })], "?needs-you=1&unread=1&unclaimed=1");
  await user.click(await screen.findByRole("button", { name: "Clear filters" }));
  expect(
    screen.getByRole("button", { name: /^Filter Claimed by/ }).getAttribute("aria-label")
  ).toBe("Filter Claimed by");
  for (const name of ["Needs you", "Unread"]) {
    await waitFor(() =>
      expect(screen.getByRole("button", { name }).getAttribute("aria-pressed")).toBe("false")
    );
  }
});

test("the Done facet does not claim an unloaded count is zero", async () => {
  const user = userEvent.setup();
  table([issue("CORE-1"), issue("CORE-2", { status: "done" })]);
  await user.click(await screen.findByRole("button", { name: /^Filter Status/ }));
  expect(screen.getByRole("option", { name: "Done, not loaded" })).toBeDefined();
});

test("loading closed issues retains the open rows and controls, including after a refusal", async () => {
  const user = userEvent.setup();
  const { listing } = table([issue("CORE-1")]);
  await screen.findByRole("row", { name: "CORE-1 Work CORE-1" });
  const closed = Promise.withResolvers<IssueSummary[]>();
  listing.mockReturnValueOnce(closed.promise);
  await user.click(screen.getByRole("button", { name: "Clear filters" }));
  expect(screen.getByRole("table", { name: "Project issues table" })).toBeDefined();
  expect(screen.getByRole("searchbox", { name: "Search issues" })).toBeDefined();
  closed.reject(new Error("closed listing refused"));
  await screen.findByRole("alert");
  expect(screen.getByRole("row", { name: "CORE-1 Work CORE-1" })).toBeDefined();
  expect(screen.getByRole("button", { name: "Retry loading closed issues" })).toBeDefined();
});

test("an epic's Start all includes queued siblings hidden by a priority filter", async () => {
  const user = userEvent.setup();
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
  await user.click(await screen.findByRole("button", { name: "Epic actions ▾" }));
  await user.click(screen.getByRole("button", { name: "Start all its queued issues next" }));
  await waitFor(() => expect(new Set(written)).toEqual(new Set(["CORE-2", "CORE-3"])));
});

test("a refused bulk operation retries independently while the other operations land", async () => {
  const user = userEvent.setup();
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
    await user.click(screen.getByRole("checkbox", { name: `Select ${key}` }));
  const before = listing.mock.calls.length;
  await user.click(screen.getByRole("button", { name: "Take out of epic" }));
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
  const user = userEvent.setup();
  const send = spyOn(api, "send").mockResolvedValue(savedIssue("CORE-1"));
  const create = spyOn(api, "createIssue").mockRejectedValue(new Error("Unexpected create"));
  restores.push(() => {
    send.mockRestore();
    create.mockRestore();
  });
  table([issue("CORE-1"), issue("CORE-2", { status: "done" })], "?nofilter=1");
  await user.click(await screen.findByRole("checkbox", { name: "Select CORE-1" }));
  await user.click(screen.getByRole("checkbox", { name: "Select CORE-2" }));
  await user.click(screen.getByRole("button", { name: "Take out of epic" }));
  await screen.findByText(/Skipped 1 closed issue/);
  expect(send.mock.calls.map(([request]) => request.path)).toEqual(["/api/v1/issues/CORE-1"]);
  await user.click(screen.getByRole("checkbox", { name: "Select CORE-2" }));
  await user.click(screen.getByRole("button", { name: "New epic from these" }));
  expect((await screen.findByRole("alert")).textContent).toContain(
    "Select at least one open issue"
  );
  expect(create).not.toHaveBeenCalled();
  expect(screen.queryByRole("dialog")).toBeNull();
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
  const user = userEvent.setup();
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
  // The childless target epic keeps a heading of its own, so the merge starts from the source's.
  const source = (await screen.findByRole("link", { name: "CORE-1 · Source" })).closest("tr");
  if (!source) throw new Error("The source epic has no heading row");
  await user.click(within(source).getByRole("button", { name: "Epic actions ▾" }));
  await user.selectOptions(
    screen.getByRole("combobox", { name: "Merge into another epic" }),
    "CORE-2"
  );
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
  const user = userEvent.setup();
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
  await user.click(await screen.findByRole("checkbox", { name: "Select CORE-2" }));
  await user.click(screen.getByRole("checkbox", { name: "Select CORE-3" }));
  await user.click(within(screen.getByRole("toolbar")).getByRole("button", { name: "Start next" }));
  expect((await screen.findByRole("alert")).textContent).toContain(
    "Did not move up: CORE-3, CORE-2"
  );
  expect(sent).toEqual(["/api/v1/issues/CORE-3"]);
  expect(writesOf(client).rows()).toHaveLength(1);
});

test("x selects the focused issue and the bar is told, in the order the table lists them", async () => {
  const user = userEvent.setup();
  table([issue("CORE-1"), issue("CORE-2")]);
  await screen.findByRole("row", { name: "CORE-1 Work CORE-1" });
  await user.keyboard("j");
  await user.keyboard("x");
  expect(screen.getByRole<HTMLInputElement>("checkbox", { name: "Select CORE-1" }).checked).toBe(
    true
  );
  expect(readSelectedIssues()).toEqual(["CORE-1"]);
  await user.keyboard("j");
  await user.keyboard("x");
  expect(readSelectedIssues()).toEqual(["CORE-1", "CORE-2"]);
  // The same key on the same row takes the selection back off, as the row's own checkbox does.
  await user.keyboard("x");
  expect(screen.getByRole<HTMLInputElement>("checkbox", { name: "Select CORE-2" }).checked).toBe(
    false
  );
  expect(readSelectedIssues()).toEqual(["CORE-1"]);
});

test("an issue the table stops listing drops out of the selection and out of what the bar acts on", async () => {
  const user = userEvent.setup();
  table([issue("CORE-1", { title: "Sextant" }), issue("CORE-2", { title: "Astrolabe" })]);
  await user.click(await screen.findByRole("checkbox", { name: "Select CORE-1" }));
  await user.click(screen.getByRole("checkbox", { name: "Select CORE-2" }));
  expect(readSelectedIssues()).toEqual(["CORE-1", "CORE-2"]);
  await user.type(screen.getByRole("searchbox", { name: "Search issues" }), "sextant");
  await waitFor(() => expect(readSelectedIssues()).toEqual(["CORE-1"]));
  expect(screen.queryByRole("checkbox", { name: "Select CORE-2" })).toBeNull();
  // Listed again, the row comes back unselected rather than carrying a hidden mark.
  await user.clear(screen.getByRole("searchbox", { name: "Search issues" }));
  const second = await screen.findByRole<HTMLInputElement>("checkbox", { name: "Select CORE-2" });
  expect(second.checked).toBe(false);
  expect(readSelectedIssues()).toEqual(["CORE-1"]);
});

test("leaving the table clears what the bar says it will act on", async () => {
  const user = userEvent.setup();
  const { view } = table([issue("CORE-1")]);
  await user.click(await screen.findByRole("checkbox", { name: "Select CORE-1" }));
  expect(readSelectedIssues()).toEqual(["CORE-1"]);
  view.unmount();
  expect(readSelectedIssues()).toEqual([]);
});

// The status bar counts what is loaded. With closed issues unloaded it shows no Done segment at
// all, rather than a cell that reads as a count; the Status filter's own Done option says they
// load when it is picked.
test("the status bar has no Done cell while closed issues are unloaded", async () => {
  const user = userEvent.setup();
  table([issue("CORE-1")]);
  const bar = await screen.findByRole("group", { name: "Issues by status" });
  expect(within(bar).queryByText(/Done/)).toBeNull();
  await user.click(screen.getByRole("button", { name: /^Filter Status/ }));
  expect(screen.getByRole("option", { name: "Done, not loaded" })).toBeDefined();
});

test("Claimed by picks the issues nobody is working, and ?unclaimed=1 still opens that view", async () => {
  const user = userEvent.setup();
  // A person's claim is never takeable, whatever the agent registry says, so it is held here.
  const held: IssueSummary["claim"] = {
    actor: { kind: "user", id: "bob" },
    at: "2026-09-10T00:00:00Z",
  };
  table([issue("CORE-1", { claim: held }), issue("CORE-2")], "?unclaimed=1");
  await screen.findByRole("row", { name: "CORE-2 Work CORE-2" });
  expect(screen.queryByRole("row", { name: "CORE-1 Work CORE-1" })).toBeNull();
  // And no chip of its own: the rule is the Claimed by column's, picked like any other value.
  expect(screen.queryByRole("button", { name: "Unclaimed" })).toBeNull();
  await user.click(screen.getByRole("button", { name: /^Filter Claimed by/ }));
  const picked = screen.getByRole("option", { name: /^Nobody or lapsed/ });
  expect(picked.getAttribute("aria-selected")).toBe("true");
  await user.click(picked);
  await screen.findByRole("row", { name: "CORE-1 Work CORE-1" });
});

test("an open epic is an ordinary row, and a group heading only when grouped by Epic", async () => {
  const user = userEvent.setup();
  table([
    issue("CORE-1", { labels: ["epic"], title: "Platform" }),
    issue("CORE-2", { parent: "CORE-1" }),
  ]);
  await screen.findByRole("row", { name: "CORE-1 Platform" });
  await user.click(screen.getByRole("checkbox", { name: "Select CORE-1" }));
  expect(readSelectedIssues()).toEqual(["CORE-1"]);
  await user.selectOptions(screen.getByRole("combobox", { name: "Group by" }), "epic");
  await waitFor(() => expect(screen.queryByRole("row", { name: "CORE-1 Platform" })).toBeNull());
  expect(screen.getByRole("link", { name: "CORE-1 · Platform" })).toBeDefined();
});

test("Start next says so when every queued row selected already starts next", async () => {
  const user = userEvent.setup();
  const send = spyOn(api, "send").mockRejectedValue(new Error("Unexpected rank write"));
  restores.push(() => send.mockRestore());
  table([issue("CORE-1", { labels: ["legion"] }), issue("CORE-2", { labels: ["legion"] })]);
  await user.click(await screen.findByRole("checkbox", { name: "Select CORE-1" }));
  await user.click(screen.getByRole("checkbox", { name: "Select CORE-2" }));
  await user.click(within(screen.getByRole("toolbar")).getByRole("button", { name: "Start next" }));
  await waitFor(() =>
    expect(
      within(screen.getByRole("region", { name: "Project issues" })).getByRole("status").textContent
    ).toContain("already start next")
  );
  expect(send).not.toHaveBeenCalled();
});

// "Select all N matching" is two clicks from every open issue in the project, so a bulk action
// sends a bounded stream rather than one request per row at once.
test("a bulk action keeps at most eight writes in flight", async () => {
  const user = userEvent.setup();
  const gates: (() => void)[] = [];
  let running = 0;
  let peak = 0;
  const send = spyOn(api, "send").mockImplementation(async (request) => {
    running += 1;
    peak = Math.max(peak, running);
    const gate = Promise.withResolvers<void>();
    gates.push(gate.resolve);
    await gate.promise;
    running -= 1;
    return savedIssue(request.path.split("/").at(-1) ?? "CORE-1") as never;
  });
  restores.push(() => send.mockRestore());
  const keys = Array.from({ length: 20 }, (_, index) => `CORE-${index + 1}`);
  table(keys.map((key) => issue(key, { parent: null })));
  await screen.findByRole("checkbox", { name: "Select CORE-1" });
  await user.click(screen.getByRole("checkbox", { name: "Select every row on this page" }));
  await user.click(screen.getByRole("button", { name: "Take out of epic" }));
  await waitFor(() => expect(gates.length).toBe(8));
  for (let index = 0; index < keys.length; index += 1) {
    await waitFor(() => expect(gates.length).toBeGreaterThan(index));
    gates[index]?.();
  }
  await waitFor(() => expect(send.mock.calls.length).toBe(keys.length));
  expect(peak).toBeLessThanOrEqual(8);
});

// A browser that refuses local storage loses the saved layout and the interrupted-action
// record, and nothing else: no click may throw out of the table.
test("a browser that refuses storage still renders the table and runs an action", async () => {
  const user = userEvent.setup();
  const refuse = (key: string) => {
    if (key.startsWith("dispatch.project-table.")) throw new Error("Storage refused");
  };
  const read = Storage.prototype.getItem;
  const write = Storage.prototype.setItem;
  const getItem = spyOn(Storage.prototype, "getItem").mockImplementation(function (
    this: Storage,
    key: string
  ) {
    refuse(key);
    return read.call(this, key);
  });
  const setItem = spyOn(Storage.prototype, "setItem").mockImplementation(function (
    this: Storage,
    key: string,
    value: string
  ) {
    refuse(key);
    write.call(this, key, value);
  });
  const send = spyOn(api, "send").mockResolvedValue(savedIssue("CORE-2"));
  restores.push(() => {
    getItem.mockRestore();
    setItem.mockRestore();
    send.mockRestore();
  });
  table([
    issue("CORE-1", { labels: ["legion"], rank: "A" }),
    issue("CORE-2", { labels: ["legion"], rank: "B" }),
  ]);
  await user.click(await screen.findByRole("checkbox", { name: "Select CORE-2" }));
  await user.click(within(screen.getByRole("toolbar")).getByRole("button", { name: "Start next" }));
  await waitFor(() =>
    expect(
      within(screen.getByRole("region", { name: "Project issues" })).getByRole("status").textContent
    ).toContain("now starts next")
  );
});

// The box is measured when its contents change height. A scroll alone must read no layout: the
// virtualizer commits on every visible range, and a measurement there is a reflow per row.
test("a scroll alone does no layout reads for the table's own box", async () => {
  table([issue("CORE-1"), issue("CORE-2")]);
  await screen.findByRole("row", { name: "CORE-1 Work CORE-1" });
  const box = screen.getByTestId("issues-table-scroll");
  const measured: string[] = [];
  const rect = spyOn(Element.prototype, "getBoundingClientRect").mockImplementation(function (
    this: Element
  ) {
    measured.push(
      this instanceof HTMLElement ? (this.dataset.testid ?? this.tagName) : this.tagName
    );
    return new DOMRect(0, 0, 1000, 600);
  });
  restores.push(() => rect.mockRestore());
  box.dispatchEvent(new Event("scroll", { bubbles: false }));
  window.dispatchEvent(new Event("scroll"));
  expect(measured.filter((name) => name === "issues-table-scroll" || name === "THEAD")).toEqual([]);
});

// What moves the box's top edge is the content above it, which no row count reports: the box
// and the block above it are observed instead. happy-dom has no `ResizeObserver`, so the stub
// is the geometry — what the table observes, and what a notice appearing there fires.
test("the box is measured through an observer over itself and the content above it", async () => {
  const live: { elements: Element[]; fire: () => void }[] = [];
  const original = globalThis.ResizeObserver;
  globalThis.ResizeObserver = class {
    readonly elements: Element[] = [];
    constructor(callback: ResizeObserverCallback) {
      live.push({
        elements: this.elements,
        fire: () => callback([], this as unknown as ResizeObserver),
      });
    }
    observe(element: Element) {
      this.elements.push(element);
    }
    unobserve() {}
    disconnect() {}
  } as unknown as typeof ResizeObserver;
  restores.push(() => {
    globalThis.ResizeObserver = original;
  });
  table([issue("CORE-1")]);
  await screen.findByRole("row", { name: "CORE-1 Work CORE-1" });
  const box = screen.getByTestId("issues-table-scroll");
  const above = screen.getByRole("region", { name: "Project issues" }).firstElementChild;
  if (!above) throw new Error("The table has no content above it");
  const watching = [...live].reverse().find((entry) => entry.elements.includes(box));
  if (!watching) throw new Error("Nothing observes the table's box");
  expect(watching.elements).toContain(above);
  const measured: string[] = [];
  const rect = spyOn(Element.prototype, "getBoundingClientRect").mockImplementation(function (
    this: Element
  ) {
    measured.push(
      this instanceof HTMLElement ? (this.dataset.testid ?? this.tagName) : this.tagName
    );
    return new DOMRect(0, 0, 1000, 600);
  });
  restores.push(() => rect.mockRestore());
  watching.fire();
  expect(measured).toContain("issues-table-scroll");
});

// An epic becomes a heading through the rows that belong to it. Taking the last one out leaves
// it with no heading to be, so it must keep its own row rather than leave the view.
test("an epic left with no children is still a row while grouped by epic", async () => {
  const user = userEvent.setup();
  // The listing answers from this array, so the move takes the child out of the epic there too.
  const rows = [
    issue("CORE-1", { labels: ["epic"], title: "Source" }),
    issue("CORE-2", { parent: "CORE-1" }),
  ];
  const send = spyOn(api, "send").mockImplementation(async () => {
    rows[1] = issue("CORE-2");
    return savedIssue("CORE-2") as never;
  });
  restores.push(() => send.mockRestore());
  table(rows, "?group=epic");
  await user.click(await screen.findByRole("checkbox", { name: "Select CORE-2" }));
  await user.click(screen.getByRole("button", { name: "Take out of epic" }));
  const row = await screen.findByRole("row", { name: "CORE-1 Source" });
  expect(within(row).getByRole("checkbox", { name: "Select CORE-1" })).toBeDefined();
});

// TanStack hands a resize to the state the reader owns, not to the widths a narrow window
// derives from it, so a drag on a phone leaves the saved desktop key and title widths alone.
test("resizing a column on a phone leaves the saved key and title widths alone", async () => {
  const user = userEvent.setup();
  localStorage.setItem(
    "dispatch.project-table.CORE",
    JSON.stringify({ sizing: { key: 200, title: 500 } })
  );
  const rect = spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (
    this: HTMLElement
  ) {
    return new DOMRect(0, 0, this.dataset.testid === "issues-table-scroll" ? 390 : 100, 40);
  });
  restores.push(() => rect.mockRestore());
  table([issue("CORE-1")]);
  const header = await screen.findByRole("columnheader", { name: /Status/ });
  const handle = header.querySelector(".cursor-col-resize");
  if (!handle) throw new Error("The Status column has no resize edge");
  await user.pointer([
    { keys: "[MouseLeft>]", target: handle, coords: { clientX: 100 } },
    { target: document.body, coords: { clientX: 160 } },
    { keys: "[/MouseLeft]", target: document.body, coords: { clientX: 160 } },
  ]);
  await waitFor(() => {
    const saved = JSON.parse(localStorage.getItem("dispatch.project-table.CORE") ?? "{}");
    expect(saved.sizing.status).toBeGreaterThan(128);
  });
  const saved = JSON.parse(localStorage.getItem("dispatch.project-table.CORE") ?? "{}");
  expect(saved.sizing.key).toBe(200);
  expect(saved.sizing.title).toBe(500);
});

// A Move is nobody's interrupted action. Retried beside one, it reports like any other change
// rather than telling the reader to finish steps it has nothing to do with.
test("a retried Move reports in the ordinary wording while an action waits to be finished", async () => {
  const user = userEvent.setup();
  localStorage.setItem(
    "dispatch.project-table.CORE",
    JSON.stringify({
      action: { id: "waiting", kind: "startNext", project: "CORE", keys: ["CORE-2"], writes: {} },
    })
  );
  let refused = false;
  const send = spyOn(api, "send").mockImplementation(async (request) => {
    if (!refused) {
      refused = true;
      throw new ApiError(403, { error: "refused", code: "FORBIDDEN" });
    }
    return savedIssue(request.path.split("/").at(-1) ?? "CORE-1") as never;
  });
  restores.push(() => send.mockRestore());
  const { client } = table([issue("CORE-1"), issue("CORE-2", { labels: ["legion"] })]);
  await user.click(await screen.findByRole("checkbox", { name: "Select CORE-1" }));
  await user.click(screen.getByRole("button", { name: "Take out of epic" }));
  await screen.findByRole("alert");
  const retry = writesOf(client)
    .rows()
    .find((write) => write.status === "refused");
  if (!retry) throw new Error("The refused move was lost");
  writesOf(client).retry(retry.id);
  await waitFor(() =>
    expect(
      within(screen.getByRole("region", { name: "Project issues" })).getByRole("status").textContent
    ).toContain("All changes saved")
  );
  expect(screen.getByRole("region", { name: "Interrupted table action" })).toBeDefined();
});

// On a resume the server decides whether the reason was posted: the source's own messages,
// compared as the whole sentence the merge builds.
test("a resumed merge posts no second reason when the source already holds it", async () => {
  const user = userEvent.setup();
  const paths: string[] = [];
  const send = spyOn(api, "send").mockImplementation(async (request) => {
    paths.push(request.path);
    return savedIssue("CORE-1") as never;
  });
  const messages = spyOn(api, "listIssueMessages").mockResolvedValue([
    { message: { body: "Merged into CORE-2 (“Target”) from the issues table." } },
  ] as never);
  restores.push(() => {
    send.mockRestore();
    messages.mockRestore();
  });
  localStorage.setItem(
    "dispatch.project-table.CORE",
    JSON.stringify({
      action: {
        id: "merging",
        kind: "merge",
        project: "CORE",
        keys: [],
        source: "CORE-1",
        target: "CORE-2",
        writes: {},
      },
    })
  );
  table([
    issue("CORE-1", { labels: ["epic"], title: "Source" }),
    issue("CORE-2", { labels: ["epic"], title: "Target" }),
  ]);
  const recovery = await screen.findByRole("region", { name: "Interrupted table action" });
  await user.click(within(recovery).getByRole("button", { name: "Finish" }));
  await waitFor(() => expect(paths).toEqual(["/api/v1/issues/CORE-1"]));
  expect(paths.some((path) => path.endsWith("/messages"))).toBe(false);
});

// A refused reason the reader discards leaves no write behind, so the record must not go on
// claiming it: Finish posts the reason itself, and only then closes the source.
test("a resumed merge whose discarded reason never landed posts it before closing", async () => {
  const user = userEvent.setup();
  const paths: string[] = [];
  let refuseReason = true;
  const send = spyOn(api, "send").mockImplementation(async (request) => {
    paths.push(request.path);
    if (refuseReason && request.path.endsWith("/messages"))
      throw new ApiError(403, { error: "Message refused", code: "FORBIDDEN" });
    return savedIssue("CORE-1") as never;
  });
  const messages = spyOn(api, "listIssueMessages").mockResolvedValue([]);
  restores.push(() => {
    send.mockRestore();
    messages.mockRestore();
  });
  const { client } = table(
    [
      issue("CORE-1", { labels: ["epic"], title: "Source" }),
      issue("CORE-2", { labels: ["epic"], title: "Target" }),
    ],
    "?group=epic"
  );
  const source = (await screen.findByRole("link", { name: "CORE-1 · Source" })).closest("tr");
  if (!source) throw new Error("The source epic has no heading row");
  await user.click(within(source).getByRole("button", { name: "Epic actions ▾" }));
  await user.selectOptions(
    screen.getByRole("combobox", { name: "Merge into another epic" }),
    "CORE-2"
  );
  await screen.findByRole("alert");
  const refused = writesOf(client)
    .rows()
    .find((write) => write.status === "refused");
  if (!refused) throw new Error("The merge reason was not refused");
  writesOf(client).discard(refused.id);
  refuseReason = false;
  await waitFor(() =>
    expect(
      JSON.parse(localStorage.getItem("dispatch.project-table.CORE") ?? "{}").action?.writes.reason
    ).toBeUndefined()
  );
  const recovery = await screen.findByRole("region", { name: "Interrupted table action" });
  await user.click(within(recovery).getByRole("button", { name: "Finish" }));
  await waitFor(() =>
    expect(paths).toEqual([
      "/api/v1/issues/CORE-1/messages",
      "/api/v1/issues/CORE-1/messages",
      "/api/v1/issues/CORE-1",
    ])
  );
});
