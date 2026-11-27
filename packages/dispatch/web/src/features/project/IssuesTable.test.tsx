import { afterEach, beforeAll, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, renderHook, screen, waitFor, within } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";
import type { ComponentProps, ReactNode } from "react";
import { MemoryRouter, useLocation } from "react-router-dom";
import { installResizeObserver } from "../../__tests__/scroll-box";
import { answerIssuePatches, answerRoute } from "../../__tests__/send-answers";
import { ApiError, api } from "../../api/client";
import { boardMoveMutationKey, projectIssuesQuery } from "../../api/queries";
import type { CreateIssueInput, Issue, IssueDetails, IssueSummary } from "../../api/types";
import { answerStatusChanges, fixtureRefusal } from "../issue/parent-status/status-change-fixtures";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { readSelectedIssues } from "../search/bar-scope";
import { KeymapProvider } from "../shell/KeymapProvider";
import { useKeymapScope } from "../shell/keymap";
import { SendExpiredError, SendStoppedError, stoppedWriteNote } from "../writes/keyed-write";
import { writesOf } from "../writes/write-store";
import { IssuesTable } from "./IssuesTable";
import { issueTableLayout } from "./issues-table-columns";
import type { IssueListing } from "./issues-table-data";
import { useTableLayout } from "./issues-table-layout";
import { ISSUE_TABLE_COLUMNS } from "./issues-table-model";
import { readLayout } from "./issues-table-storage";

// The rows draw through the shared renderer, whose Markdown engine loads lazily, and they run in
// random order, so the file loads the engine before its first row, whichever row that is.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

function issue(key: string, overrides: Partial<IssueSummary> = {}): IssueSummary {
  return {
    key,
    advances: [],
    advances_revision: 0,
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
    closed_at: overrides.status === "done" ? "2026-09-10T00:00:00Z" : null,
    ...overrides,
  };
}
/** An issue as a Dispatch process of the release before the one that added `created_by` and
 *  `created_at` answers it: neither member is there. While a release rolls out, a page of this
 *  build reads such a row ("Two builds at once", `packages/envoy/cmd/dispatch/README.md`). */
function issueFromOlderServer(key: string, overrides: Partial<IssueSummary> = {}): IssueSummary {
  const row: Partial<IssueSummary> = issue(key, overrides);
  delete row.created_at;
  delete row.created_by;
  return row as IssueSummary;
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
  const location = useLocation();
  return (
    <>
      <output data-testid="route">{location.pathname}</output>
      <output data-testid="route-search">{location.search}</output>
    </>
  );
}
const restores: (() => void)[] = [];
afterEach(() => {
  for (const restore of restores.splice(0)) restore();
  localStorage.clear();
});
function table(
  issues: IssueSummary[],
  search = "",
  full?: Promise<IssueSummary[]>,
  props: {
    layout?: ComponentProps<typeof IssuesTable>["layout"];
    listing?: IssueListing;
    priorityTitles?: ReadonlyMap<string, string>;
    stateKey?: string;
  } = {}
) {
  const parents = [
    ...new Set(
      issues.flatMap((row) =>
        row.parent && !issues.some((candidate) => candidate.key === row.parent) ? [row.parent] : []
      )
    ),
  ].map((key) => issue(key, { status: "triage" }));
  // The listing route as the server answers it: `open` leaves the closed rows out, and
  // `advances` keeps the issues that carry that relation, whichever project they are in.
  const listing = spyOn(api, "listIssues").mockImplementation(async (options = {}) =>
    !options.open && full
      ? full
      : [...issues, ...parents].filter(
          (issue) =>
            (!options.open || issue.status !== "done") &&
            (options.advances === undefined ||
              (issue.advances ?? []).some((relation) => options.advances?.includes(relation.id)))
        )
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
          <IssuesTable listing={{ project: "CORE" }} {...props} />
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
  // On the backlog: the rule needs no epic in work, so the create is the action's first write.
  table([issue("CORE-1", { status: "backlog" })]);
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
  // On the backlog: the rule needs no epic in work, so the create is the action's first write.
  table([issue("CORE-1", { status: "backlog" })]);
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
  // On the backlog: the rule needs no epic in work, so the create is the action's first write.
  const rows = [issue("CORE-1", { status: "backlog" })];
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
  await act(async () => {
    await client.invalidateQueries({ queryKey: ["issues"] });
  });
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

// A refused change is said in Dispatch's own words. `String` of the refusal would lead them with
// its class's name (`ApiError: `), which means nothing to the reader.
test("a refused change's notice says the refusal in Dispatch's words, without the error's name", async () => {
  const user = userEvent.setup();
  const send = spyOn(api, "send").mockRejectedValue(
    new ApiError(403, { error: "Moves are closed for CORE.", code: "FORBIDDEN" })
  );
  restores.push(() => send.mockRestore());
  table([issue("CORE-1")]);
  await user.click(await screen.findByRole("checkbox", { name: "Select CORE-1" }));
  await user.click(screen.getByRole("button", { name: "Take out of epic" }));
  expect((await screen.findByRole("alert")).textContent).toBe(
    "0 of 1 changes saved. Changes not saved: CORE-1. Moves are closed for CORE. Retry each failed change in Pending changes."
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
  // Asking the Status filter for Done is what loads the closed issues; Clear filters goes back
  // to the open view, which needs no second listing.
  await user.click(screen.getByRole("button", { name: /^Filter Status/ }));
  await user.click(screen.getByRole("option", { name: /^Done/ }));
  expect(screen.getByRole("table", { name: "Project issues table" })).toBeDefined();
  expect(screen.getByRole("searchbox", { name: "Search issues" })).toBeDefined();
  closed.reject(new Error("closed listing refused"));
  await screen.findByRole("alert");
  expect(screen.getByRole("row", { name: "CORE-1 Work CORE-1" })).toBeDefined();
  expect(screen.getByRole("button", { name: "Retry loading closed issues" })).toBeDefined();
});

// The rows' cells are drawn by the column definitions, and a new definition is a new component
// to React: whether the closed issues are loaded is the table's, not a column's, so their arrival
// keeps every cell already drawn, and with it whatever a reader left focused there.
test("the closed issues arriving keeps the cells already drawn", async () => {
  const user = userEvent.setup();
  table([issue("CORE-1"), issue("CORE-2", { status: "done" })]);
  const select = await screen.findByRole("checkbox", { name: "Select CORE-1" });
  await user.click(screen.getByRole("button", { name: /^Filter Status/ }));
  await user.click(screen.getByRole("option", { name: /^Done/ }));
  await screen.findByRole("row", { name: "CORE-2 Work CORE-2" });
  expect(screen.getByRole("checkbox", { name: "Select CORE-1" })).toBe(select);
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
  act(() => {
    client.setQueryData(projectIssuesQuery("CORE", [], true).queryKey, [
      issue("CORE-1", { parent: "CORE-9" }),
    ]);
  });
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

/** The rows below give the issue a server sent without the two members the lower key, so neither
 *  the key order nor the order they are listed in can stand in for the ordering under test. */
test("a row a server sends without created_by or created_at renders, with no author and no time", async () => {
  table([issue("CORE-2"), issueFromOlderServer("CORE-1")], "?sort=created");
  const row = await screen.findByRole("row", { name: "CORE-1 Work CORE-1" });
  expect(within(row).getByRole("link", { name: "CORE-1" }).getAttribute("href")).toContain(
    "CORE-1"
  );
  expect(within(row).getByText("Unknown")).toBeDefined();
  expect(row.querySelector("time")).toBeNull();
  expect(
    screen.getByRole("row", { name: "CORE-2 Work CORE-2" }).querySelector("time")
  ).not.toBeNull();
  // Created sorts without throwing on the absent time, and keeps that row after the one that
  // carries a time, although its key sorts first.
  expect(
    screen
      .getAllByRole("row")
      .filter((candidate) => candidate.hasAttribute("data-issue-row"))
      .map((candidate) => candidate.getAttribute("data-issue-row"))
  ).toEqual(["CORE-2", "CORE-1"]);
});

test("a row with no created_at stays last when Created is sorted the other way", async () => {
  // A sorting function alone cannot do this: the table orders an absent accessor value before it
  // calls one and then reverses that for a descending sort, so the column says `sortUndefined:
  // "last"`. Two rows without a time keep their own order beside the one that has one.
  table(
    [issue("CORE-3"), issueFromOlderServer("CORE-1"), issueFromOlderServer("CORE-2")],
    "?sort=-created"
  );
  await screen.findByRole("row", { name: "CORE-3 Work CORE-3" });
  expect(
    screen
      .getAllByRole("row")
      .filter((candidate) => candidate.hasAttribute("data-issue-row"))
      .map((candidate) => candidate.getAttribute("data-issue-row"))
  ).toEqual(["CORE-3", "CORE-1", "CORE-2"]);
});

test("the Created filter files a row a server sent with no created_at under Older", async () => {
  // The column's filter asks `createdAt` and files a row without a time under Older
  // (`issues-table-model.ts`), the bucket an issue old enough to come from a server of the
  // release before `created_at` would reach anyway, so a reader who picks that age still sees
  // the row. The row that carries a time is today's, so the filter is doing the choosing.
  table(
    [issue("CORE-1", { created_at: new Date().toISOString() }), issueFromOlderServer("CORE-2")],
    "?f.created=Older"
  );
  expect(await screen.findByRole("row", { name: "CORE-2 Work CORE-2" })).toBeDefined();
  expect(screen.queryByRole("row", { name: "CORE-1 Work CORE-1" })).toBeNull();
});

test("the Legion queue puts an issue with no created_at after one of the same rank that has one", async () => {
  table([
    issueFromOlderServer("CORE-1", { labels: ["legion"], rank: "m" }),
    issue("CORE-2", { labels: ["legion"], rank: "m" }),
  ]);
  const older = await screen.findByRole("row", { name: "CORE-1 Work CORE-1" });
  expect(
    within(screen.getByRole("row", { name: "CORE-2 Work CORE-2" })).getByText("#1")
  ).toBeDefined();
  expect(within(older).getByText("#2")).toBeDefined();
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
  await user.click(screen.getByRole("button", { name: "Merge…" }));
  await user.click(
    within(await screen.findByRole("dialog", { name: "Merge this epic?" })).getByRole("button", {
      name: /^Merge$/,
    })
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
  act(() => {
    client.setQueryData(projectIssuesQuery("CORE", [], true).queryKey, [
      issue("CORE-1", { parent: "CORE-9" }),
    ]);
  });
  await screen.findByText(/Could not load 1 ancestor;/);
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

/** What the table says of a step to `key` the reader stopped in Pending changes: Dispatch may
 *  have saved it, and its Retry there asks again under its key. */
const stoppedStep = (key: string) =>
  stoppedWriteNote(`the change to ${key}`, "saved", {
    method: "PATCH",
    stopped: new SendStoppedError(),
  });

// A step the reader stopped may have moved its issue up: the table does not list it among those
// that did not, and says of it what a stopped write is, rather than the stopped write's own
// words, which are a composer's, of a message.
test("Start next names a stopped step as unconfirmed, not as an issue that did not move up", async () => {
  const user = userEvent.setup();
  const send = spyOn(api, "send").mockRejectedValue(new SendStoppedError());
  restores.push(() => send.mockRestore());
  table([
    issue("CORE-1", { labels: ["legion"], rank: "A" }),
    issue("CORE-2", { labels: ["legion"], rank: "B" }),
    issue("CORE-3", { labels: ["legion"], rank: "C" }),
  ]);
  await user.click(await screen.findByRole("checkbox", { name: "Select CORE-2" }));
  await user.click(screen.getByRole("checkbox", { name: "Select CORE-3" }));
  await user.click(within(screen.getByRole("toolbar")).getByRole("button", { name: "Start next" }));
  const said = (await screen.findByRole("alert")).textContent;
  expect(said).toContain(
    `Did not move up: CORE-2. 0 of 1 changes saved. ${stoppedStep("CORE-3")} Retry each failed change in Pending changes.`
  );
  expect(said).not.toContain("CORE-3, CORE-2");
});

// A move of a merge the reader stopped may have been made: the table says so of it, and that the
// source stays open, rather than the stopped write's own words.
test("a merge with a stopped move says the move may have been made and the source stays open", async () => {
  const user = userEvent.setup();
  const send = spyOn(api, "send").mockImplementation(async (request) => {
    if (request.path.endsWith("CORE-3")) throw new SendStoppedError();
    return savedIssue("CORE-4") as never;
  });
  restores.push(() => send.mockRestore());
  table(
    [
      issue("CORE-1", { labels: ["epic"], title: "Source" }),
      issue("CORE-2", { labels: ["epic"], title: "Target" }),
      issue("CORE-3", { parent: "CORE-1" }),
      issue("CORE-4", { parent: "CORE-1" }),
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
  await user.click(screen.getByRole("button", { name: "Merge…" }));
  await user.click(
    within(await screen.findByRole("dialog", { name: "Merge this epic?" })).getByRole("button", {
      name: /^Merge$/,
    })
  );
  expect((await screen.findByRole("alert")).textContent).toContain(
    `The source epic CORE-1 remains open. 1 of 2 changes saved. ${stoppedStep("CORE-3")} Retry each failed change in Pending changes.`
  );
});

// A batch whose stopped changes promise different Retries: one key Dispatch still holds, so its
// Retry asks again under it, and one that ran out, so its Retry is a new write. Each is said with
// its own promise, never both with the first one's.
test("a merge with a stopped move and one whose key ran out says each move's own Retry", async () => {
  const user = userEvent.setup();
  const send = spyOn(api, "send").mockImplementation(async (request) => {
    throw request.path.endsWith("CORE-3") ? new SendStoppedError() : new SendExpiredError();
  });
  restores.push(() => send.mockRestore());
  table(
    [
      issue("CORE-1", { labels: ["epic"], title: "Source" }),
      issue("CORE-2", { labels: ["epic"], title: "Target" }),
      issue("CORE-3", { parent: "CORE-1" }),
      issue("CORE-4", { parent: "CORE-1" }),
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
  await user.click(screen.getByRole("button", { name: "Merge…" }));
  await user.click(
    within(await screen.findByRole("dialog", { name: "Merge this epic?" })).getByRole("button", {
      name: /^Merge$/,
    })
  );
  expect((await screen.findByRole("alert")).textContent).toContain(
    "The source epic CORE-1 remains open. 0 of 2 changes saved. Dispatch has not confirmed the " +
      "change to CORE-3: it may have been saved. Retry asks again under the same key, so it is " +
      "saved at most once if you retry within 24 hours of the first try. After that Dispatch no " +
      "longer knows the key: a Retry is a new write and may be saved twice. Dispatch has not " +
      "confirmed the change to CORE-4: it may have been saved. Its key is 24 hours old or more, " +
      "so Dispatch no longer holds it: a Retry is a new write, which can be saved a second time " +
      "if the first one was. Retry each failed change in Pending changes."
  );
});

// An epic's create the reader stopped may have made the epic: the table says so, and that its
// Retry in Pending changes asks again under its key, rather than the stopped write's own words.
test("a stopped epic creation says the epic may have been made, not a message's words", async () => {
  const user = userEvent.setup();
  const create = spyOn(api, "createIssue").mockRejectedValue(new SendStoppedError());
  const named = nameTheEpic(user, "Committed once");
  restores.push(() => create.mockRestore());
  // On the backlog: the rule needs no epic in work, so the create is the action's first write.
  table([issue("CORE-1", { status: "backlog" })]);
  await user.click(await screen.findByRole("checkbox", { name: "Select CORE-1" }));
  await user.click(screen.getByRole("button", { name: "New epic from these" }));
  await named;
  expect((await screen.findByRole("alert")).textContent).toContain(
    `${stoppedWriteNote("the epic “Committed once”", "made", {
      method: "POST",
      stopped: new SendStoppedError(),
    })} Retry it in Pending changes.`
  );
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

/** The unit DOM lays nothing out: the status bar `bar` px wide, and the hidden copy of its
 *  segments with their names `names` px wide, which the bar is measured against. */
function sizeStatusBar(bar: number, names: number) {
  const measured = Element.prototype.getBoundingClientRect;
  const rect = spyOn(Element.prototype, "getBoundingClientRect").mockImplementation(function (
    this: Element
  ) {
    if (this.matches('fieldset[aria-label="Issues by status"]')) return new DOMRect(0, 0, bar, 28);
    if (this.matches('[aria-hidden="true"] > .w-max')) return new DOMRect(0, 0, names, 28);
    return measured.call(this);
  });
  restores.push(() => rect.mockRestore());
}

const TWO_STATUSES = [issue("CORE-1"), issue("CORE-2", { status: "in_progress" })];
const LEGEND_LAYOUT = {
  key: "legend",
  opens: ["key", "title", "status"],
  statusLegend: true,
} as const;

// A table that asks for it (`statusLegend`) draws each segment's count alone where the segments
// with their names are wider than the bar: each still named by its status, to a reader and in
// its tooltip, and the names in a legend under the bar, in the bar's order, each beside a swatch
// of its status's colour.
test("a status bar narrower than its names draws the counts alone and the names in a legend", async () => {
  sizeStatusBar(200, 300);
  table(TWO_STATUSES, "", undefined, { layout: LEGEND_LAYOUT });
  const bar = await screen.findByRole("group", { name: "Issues by status" });
  await waitFor(() =>
    expect(
      within(bar)
        .getAllByRole("button")
        .map((segment) => [segment.textContent, segment.getAttribute("aria-label"), segment.title])
    ).toEqual([
      ["1", "Todo 1", "Todo: 1"],
      ["1", "In progress 1", "In progress: 1"],
    ])
  );
  const legend = bar.nextElementSibling;
  expect(legend?.tagName).toBe("UL");
  expect([...(legend?.children ?? [])].map((item) => item.textContent)).toEqual([
    "Todo",
    "In progress",
  ]);
  expect(legend?.querySelector("span")?.className).toContain("bg-sky-400");
});

// Where the names fit, or where the table does not ask for it, as a project's does not, each
// segment draws its name and its count and no legend stands under the bar.
for (const [what, layout, names] of [
  ["a bar its names fit", LEGEND_LAYOUT, 150],
  ["a project's bar", undefined, 300],
] as const) {
  test(`${what} draws each name beside its count, with no legend`, async () => {
    sizeStatusBar(200, names);
    table(TWO_STATUSES, "", undefined, { layout });
    const bar = await screen.findByRole("group", { name: "Issues by status" });
    await waitFor(() =>
      expect(
        within(bar)
          .getAllByRole("button")
          .map((segment) => segment.textContent)
      ).toEqual(["Todo 1", "In progress 1"])
    );
    expect(bar.nextElementSibling).toBeNull();
  });
}

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

/** Puts what `replacement` makes of the page storage's own `name` method in place of that
 *  method, and returns what puts the real one back. `replacement` is handed the real method, so
 *  it never reads `localStorage[name]` itself, which would find the replacement and call itself.
 *  happy-dom's `localStorage` is a Proxy that copies each method of `Storage.prototype` onto
 *  itself, bound, the first time anything reads it: a spy on the prototype reaches the storage
 *  only when nothing in the process has read that method yet, and then stays its method after
 *  `mockRestore`, a mock that no longer reaches the real method. `Object.defineProperty` passes
 *  the Proxy's trap onto the storage itself, where every later read finds it. */
function onLocalStorage<Name extends "getItem" | "setItem">(
  name: Name,
  replacement: (real: Storage[Name]) => Storage[Name]
): () => void {
  const real = localStorage[name];
  const descriptor = { configurable: true, writable: true };
  Object.defineProperty(localStorage, name, { ...descriptor, value: replacement(real) });
  return () => Object.defineProperty(localStorage, name, { ...descriptor, value: real });
}

// A browser that refuses local storage loses the saved layout and the interrupted-action
// record, and nothing else: no click may throw out of the table.
test("a browser that refuses storage still renders the table and runs an action", async () => {
  const user = userEvent.setup();
  const refuse = (key: string) => {
    if (key.startsWith("dispatch.project-table.")) throw new Error("Storage refused");
  };
  const restoreRead = onLocalStorage("getItem", (read) => (key) => {
    refuse(key);
    return read(key);
  });
  const restoreWrite = onLocalStorage("setItem", (write) => (key, value) => {
    refuse(key);
    write(key, value);
  });
  const send = spyOn(api, "send").mockResolvedValue(savedIssue("CORE-2"));
  restores.push(() => {
    restoreRead();
    restoreWrite();
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
  act(() => {
    box.dispatchEvent(new Event("scroll", { bubbles: false }));
    window.dispatchEvent(new Event("scroll"));
  });
  expect(measured.filter((name) => name === "issues-table-scroll" || name === "THEAD")).toEqual([]);
});

// What moves the box's top edge is the content above it, which no row count reports: the box
// and the block above it are observed instead. happy-dom has no `ResizeObserver`, so the stub
// is the geometry — what the table observes, and what a notice appearing there fires.
test("the box is measured through an observer over itself and the content above it", async () => {
  const sizes = installResizeObserver();
  restores.push(() => sizes.restore());
  table([issue("CORE-1")]);
  await screen.findByRole("row", { name: "CORE-1 Work CORE-1" });
  const box = screen.getByTestId("issues-table-scroll");
  const above = screen.getByRole("region", { name: "Project issues" }).firstElementChild;
  if (!above) throw new Error("The table has no content above it");
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
  // A notice appearing above the box changes only that block's size.
  sizes.resize(above);
  expect(measured).toContain("issues-table-scroll");
  measured.length = 0;
  sizes.resize(box);
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

// An epic is filterable and searchable in every grouping: grouped by Epic its heading is made
// by the children the table lists, under the same filters and search, so one that leaves the
// epic no child leaves the epic its own row rather than taking it out of the view.
test("an epic whose children a status filter hides keeps its own row while grouped by epic", async () => {
  table(
    [
      issue("CORE-1", { labels: ["epic"], status: "triage", title: "Telemetry rollout" }),
      issue("CORE-2", { parent: "CORE-1", status: "todo", title: "Wire the exporter" }),
    ],
    "?group=epic&status=triage"
  );
  expect(await screen.findByRole("row", { name: "CORE-1 Telemetry rollout" })).toBeDefined();
  expect(screen.queryByRole("row", { name: "CORE-2 Wire the exporter" })).toBeNull();
});

test("a Labels filter of epic shows the epics while grouped by epic", async () => {
  table(
    [
      issue("CORE-1", { labels: ["epic"], title: "Telemetry rollout" }),
      issue("CORE-2", { parent: "CORE-1", title: "Wire the exporter" }),
    ],
    "?group=epic&label=epic&nofilter=1"
  );
  expect(await screen.findByRole("row", { name: "CORE-1 Telemetry rollout" })).toBeDefined();
  expect(screen.queryByRole("row", { name: "CORE-2 Wire the exporter" })).toBeNull();
});

/** The group headings on the page, in the order they are drawn. */
function headings(): string[] {
  return [...document.querySelectorAll("tbody tr")]
    .filter((row) => row.querySelector("td[colspan]"))
    .map((row) => (row.textContent ?? "").trim());
}

// A group's place is its column's, not the row sort's: the registry lists the values in the
// order the filter offers them, and the headings read the same way.
test.each([
  ["the default Queue sort", ""],
  ["Created ascending", "&sort=created"],
  ["Key descending", "&sort=-key"],
])("grouped by Priority, the headings run P0, P1, P2, P3 under %s", async (_name, sort) => {
  // Queued rows give each group a number to sum and a rank to sort by, which is what let the
  // row sort carry the headings along with it.
  table(
    [
      issue("CORE-1", { labels: ["legion"], priority: 0, rank: "a" }),
      issue("CORE-2", { labels: ["legion"], priority: 1, rank: "b" }),
      issue("CORE-3", { labels: ["legion"], priority: 3, rank: "c" }),
      issue("CORE-4", { labels: ["legion"], priority: 2, rank: "d" }),
    ],
    `?group=priority${sort}`
  );
  await screen.findByRole("row", { name: "CORE-1 Work CORE-1" });
  await waitFor(() =>
    expect(headings().map((text) => text.match(/P\d/)?.[0])).toEqual(["P0", "P1", "P2", "P3"])
  );
});

test.each([
  ["the default Queue sort", ""],
  ["Created ascending", "&sort=created"],
  ["Key descending", "&sort=-key"],
])("grouped by Status, the headings run in workflow order under %s", async (_name, sort) => {
  table(
    [
      issue("CORE-1", { labels: ["legion"], status: "needs_review", rank: "a" }),
      issue("CORE-2", { labels: ["legion"], status: "todo", rank: "b" }),
      issue("CORE-3", { labels: ["legion"], status: "in_progress", rank: "c" }),
    ],
    `?group=status&nofilter=1${sort}`
  );
  await screen.findByRole("row", { name: "CORE-1 Work CORE-1" });
  await waitFor(() =>
    expect(
      headings().map((text) =>
        ["Todo", "In progress", "Needs review"].find((name) => text.includes(name))
      )
    ).toEqual(["Todo", "In progress", "Needs review"])
  );
});

// The reader usually arrives ungrouped and picks the grouping from the toolbar, which the
// table must answer as it answers a `?group=` link: TanStack asks for its sorted rows once and
// keeps that answer for the table's life, so the ordering has to read the grouping of the day.
test.each([
  // Ungrouped, the rows are the reader's sort: Queue puts the two queued rows first, and
  // Created leaves rows of one timestamp as they came.
  ["the default Queue sort", "", ["CORE-2", "CORE-4", "CORE-1", "CORE-3"]],
  ["Created ascending", "?sort=created", ["CORE-1", "CORE-2", "CORE-3", "CORE-4"]],
])(
  "a grouping picked from the toolbar orders its headings under %s",
  async (_name, search, ungrouped) => {
    const user = userEvent.setup();
    table(
      [
        issue("CORE-1", { labels: ["legion"], priority: 0, rank: "a", status: "needs_review" }),
        issue("CORE-2", { labels: ["legion"], priority: 1, rank: "b", status: "todo" }),
        issue("CORE-3", { labels: ["legion"], priority: 3, rank: "c", status: "in_progress" }),
        issue("CORE-4", { labels: ["legion"], priority: 2, rank: "d", status: "todo" }),
      ],
      search
    );
    await screen.findByRole("row", { name: "CORE-1 Work CORE-1" });
    const groupBy = screen.getByRole("combobox", { name: "Group by" });
    await user.selectOptions(groupBy, "priority");
    await waitFor(() =>
      expect(headings().map((text) => text.match(/P\d/)?.[0])).toEqual(["P0", "P1", "P2", "P3"])
    );
    await user.selectOptions(groupBy, "status");
    await waitFor(() =>
      expect(
        headings().map((text) =>
          ["Todo", "In progress", "Needs review"].find((name) => text.includes(name))
        )
      ).toEqual(["Todo", "In progress", "Needs review"])
    );
    // Ungrouped again, the rows follow the reader's sort alone: no grouping order survives it.
    await user.selectOptions(groupBy, "");
    await waitFor(() => expect(headings()).toEqual([]));
    await waitFor(() =>
      expect(
        [...document.querySelectorAll("[data-issue-row]")].map((row) =>
          row.getAttribute("data-issue-row")
        )
      ).toEqual(ungrouped)
    );
  }
);

// Start all its queued issues next is Start next over an epic's queued children: it moves them
// and says so, rather than leaving the reader to guess whether anything happened.
test("Start all its queued issues next moves the epic's queued children and reports it", async () => {
  const user = userEvent.setup();
  const sent: { path: string; body: unknown }[] = [];
  const send = spyOn(api, "send").mockImplementation(async (request) => {
    sent.push({ path: request.path, body: request.body });
    return savedIssue("CORE-3") as never;
  });
  restores.push(() => send.mockRestore());
  table(
    [
      issue("CORE-1", { labels: ["epic"], title: "Queue epic" }),
      issue("CORE-2", { labels: ["legion"], rank: "a", title: "Ahead of the epic" }),
      issue("CORE-3", { labels: ["legion"], parent: "CORE-1", rank: "b", title: "Behind" }),
    ],
    "?group=epic"
  );
  const heading = (await screen.findByRole("link", { name: "CORE-1 · Queue epic" })).closest("tr");
  if (!heading) throw new Error("The epic has no heading row");
  await user.click(within(heading).getByRole("button", { name: "Epic actions ▾" }));
  await user.click(screen.getByRole("button", { name: "Start all its queued issues next" }));
  await waitFor(() =>
    expect(
      within(screen.getByRole("region", { name: "Project issues" })).getByRole("status").textContent
    ).toContain("now starts next")
  );
  expect(sent).toEqual([{ path: "/api/v1/issues/CORE-3", body: { rank: { before: "CORE-2" } } }]);
});

// Finish sends what a refusal left behind under the same bound a bulk action keeps, so a
// 250-row refusal is a stream of eight rather than 250 requests at once.
test("Finish retries a refused action's writes eight at a time", async () => {
  const user = userEvent.setup();
  let inFlight = 0;
  let peak = 0;
  let refuse = true;
  const held: (() => void)[] = [];
  const send = spyOn(api, "send").mockImplementation(async () => {
    if (refuse) throw new ApiError(403, { error: "refused", code: "FORBIDDEN" });
    inFlight += 1;
    peak = Math.max(peak, inFlight);
    const holding = Promise.withResolvers<void>();
    held.push(holding.resolve);
    await holding.promise;
    inFlight -= 1;
    return savedIssue("CORE-1") as never;
  });
  restores.push(() => send.mockRestore());
  const children = Array.from({ length: 20 }, (_, index) =>
    issue(`CORE-${index + 1}`, { parent: "CORE-98" })
  );
  const { client } = table(
    [
      issue("CORE-98", { labels: ["epic"], title: "Merge source" }),
      issue("CORE-99", { labels: ["epic"], title: "Merge target" }),
      ...children,
    ],
    "?group=epic"
  );
  const heading = (await screen.findByRole("link", { name: "CORE-98 · Merge source" })).closest(
    "tr"
  );
  if (!heading) throw new Error("The source epic has no heading row");
  await user.click(within(heading).getByRole("button", { name: "Epic actions ▾" }));
  await user.selectOptions(
    screen.getByRole("combobox", { name: "Merge into another epic" }),
    "CORE-99"
  );
  await user.click(screen.getByRole("button", { name: "Merge…" }));
  await user.click(
    within(await screen.findByRole("dialog", { name: "Merge this epic?" })).getByRole("button", {
      name: /^Merge$/,
    })
  );
  await screen.findByRole("alert");
  refuse = false;
  expect(
    writesOf(client)
      .rows()
      .filter((write) => write.status === "refused").length
  ).toBeGreaterThan(8);
  const interrupted = await screen.findByRole("region", { name: "Interrupted table action" });
  await user.click(within(interrupted).getByRole("button", { name: "Finish" }));
  await waitFor(() => expect(inFlight).toBe(8));
  expect(peak).toBe(8);
  for (const resolve of held.splice(0)) resolve();
  await waitFor(() => expect(peak).toBe(8));
});

// TanStack hands a resize to the state the reader owns, not to the widths a narrow window
// derives from it, so a drag on a phone leaves the saved desktop key and title widths alone.
test("resizing a column on a phone leaves the saved key and title widths alone", async () => {
  const user = userEvent.setup();
  localStorage.setItem(
    "dispatch.project-table.CORE",
    JSON.stringify({ sizing: { key: 200, title: 500 } })
  );
  stubScrollBoxWidth(390);
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

// A reader's saved layout is read and written back as it always was: under the same key, in the
// same JSON, with a column the table does not have, a pinned column's visibility and a width out
// of bounds dropped, and every other field of the record left where it was. A project's table has
// none of the listing columns, so it writes none of them.
test("the List writes a reader's saved layout back under the same key in the same JSON", async () => {
  localStorage.setItem(
    "dispatch.project-table.CORE",
    JSON.stringify({
      sizing: { title: 360, name: 200, key: 10, status: 3000, project: 140 },
      order: ["status", "name", "key", "status", "moved"],
      visibility: { labels: false, name: false, key: false, updated: true, advances: true },
      kept: 1,
    })
  );
  table([issue("CORE-1")]);
  await screen.findByRole("row", { name: "CORE-1 Work CORE-1" });
  expect(localStorage.getItem("dispatch.project-table.CORE")).toBe(
    '{"sizing":{"title":360},"order":["status","key"],"visibility":{"updated":true,"labels":false},"kept":1}'
  );
});

test("the List stores a reader's first layout with Updated hidden and no listing column", async () => {
  table([issue("CORE-1")]);
  await screen.findByRole("row", { name: "CORE-1 Work CORE-1" });
  expect(localStorage.getItem("dispatch.project-table.CORE")).toBe(
    '{"sizing":{},"order":[],"visibility":{"updated":false}}'
  );
});

// What `in` read on main as a column: a name every object inherits. No app path writes one, so
// only a corrupt record holds it, and it is dropped like any other name the table has no column for.
test("a stored width under an inherited name such as toString is dropped", () => {
  localStorage.setItem("dispatch.project-table.CORE", '{"sizing":{"toString":200,"title":360}}');
  expect(readLayout("CORE", ISSUE_TABLE_COLUMNS).sizing).toEqual({ title: 360 });
});

/** Makes `getBoundingClientRect` report the List's scroll box `width` pixels wide and every other
 *  element 100, each 40 high, until the row ends; and the box's `offsetWidth` and `clientWidth`
 *  its border box and the room inside its borders and beside its scrollbar, `edge` pixels less
 *  (2, its two borders, by default). The spy is on `Element.prototype`, which owns the method:
 *  one on `HTMLElement` would leave an own copy behind that shadows every later spy. The two
 *  widths are getters happy-dom puts on `HTMLElement.prototype`, so they are replaced there and
 *  put back as they were. */
function stubScrollBoxWidth(width: number, edge = 2): void {
  const isBox = (element: Element) => element.getAttribute("data-testid") === "issues-table-scroll";
  const rect = spyOn(Element.prototype, "getBoundingClientRect").mockImplementation(function (
    this: Element
  ) {
    return new DOMRect(0, 0, isBox(this) ? width : 100, 40);
  });
  restores.push(() => rect.mockRestore());
  for (const [name, boxWidth] of [
    ["offsetWidth", width],
    ["clientWidth", width - edge],
  ] as const) {
    const original = Object.getOwnPropertyDescriptor(HTMLElement.prototype, name);
    Object.defineProperty(HTMLElement.prototype, name, {
      configurable: true,
      get(this: HTMLElement) {
        return isBox(this) ? boxWidth : (original?.get?.call(this) ?? 0);
      },
    });
    restores.push(() => {
      if (original !== undefined) Object.defineProperty(HTMLElement.prototype, name, original);
    });
  }
}

/** Each header cell of the List, left to right, as `<its column's words> <width> <the offset a
 *  pinned cell is held at, or ->`, drawn with the scroll box `width` pixels wide. */
async function headerLayout(width: number): Promise<string[]> {
  stubScrollBoxWidth(width);
  table([issue("CORE-1")]);
  await screen.findByRole("row", { name: "CORE-1 Work CORE-1" });
  const head = screen.getByRole("table", { name: "Project issues table" }).querySelector("thead");
  return [...(head?.querySelectorAll("th") ?? [])].map((cell) =>
    [
      (cell.textContent ?? "").replace(/[▲▼]|Filter$/g, "") || "-",
      cell.style.width,
      cell.style.left || "-",
    ].join(" ")
  );
}

/** The columns after the pinned ones, at the registry's widths, then the Start next column. */
const UNPINNED_REST = [
  "Status 132px -",
  "Epic 220px -",
  "Assignee 180px -",
  "Created by 200px -",
  "Labels 200px -",
  "Claimed by 220px -",
  "Open asks 148px -",
  "Created 148px -",
  "- 104px -",
];

// The List's widths and pins at a desktop box, a box too narrow for every pin, and a phone: the
// registry's starting widths and minimums, Select, Priority, Key and Title pinned within 60% of a
// wide box with Title narrowing first and the rightmost unpinning, and Select, Key and Title
// within 85% of a phone's, Key at its phone width and Title taking what is left.
test("the List draws its columns at the registry's widths and pins at 1400 px", async () => {
  expect(await headerLayout(1400)).toEqual([
    "- 44px 0px",
    "Priority 140px 44px",
    "Key 108px 184px",
    "Title 380px 292px",
    "Queue 124px -",
    ...UNPINNED_REST,
  ]);
});

test("the List narrows Title and unpins from the right in a 700 px box", async () => {
  localStorage.setItem(
    "dispatch.project-table.CORE",
    JSON.stringify({ sizing: { key: 200, title: 500 } })
  );
  expect(await headerLayout(700)).toEqual([
    "- 44px 0px",
    "Priority 140px 44px",
    "Key 200px 184px",
    "Queue 124px -",
    "Title 116px -",
    ...UNPINNED_REST,
  ]);
});

test("the List pins Select, Key and Title on a 390 px phone", async () => {
  expect(await headerLayout(390)).toEqual([
    "- 44px 0px",
    "Key 88px 44px",
    "Title 199.5px 132px",
    "Priority 140px -",
    "Queue 124px -",
    ...UNPINNED_REST,
  ]);
});

// A width the reader dragged on a desktop is the width the header is drawn at, and a stored one
// below a column's minimum is drawn at the minimum.
test("a stored desktop width reaches the List's header", async () => {
  localStorage.setItem(
    "dispatch.project-table.CORE",
    JSON.stringify({ sizing: { epic: 300, status: 50 } })
  );
  const cells = await headerLayout(1400);
  expect(cells).toContain("Epic 300px -");
  expect(cells).toContain("Status 132px -");
});

test("the Status filter counts Done once the closed issues are loaded", async () => {
  const user = userEvent.setup();
  table([issue("CORE-1"), issue("CORE-2", { status: "done" })], "?status=done");
  await screen.findByRole("row", { name: "CORE-2 Work CORE-2" });
  await user.click(screen.getByRole("button", { name: /^Filter Status/ }));
  expect(screen.getByRole("option", { name: "Done, 1 issue" })).toBeDefined();
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
  await user.click(screen.getByRole("button", { name: "Merge…" }));
  await user.click(
    within(await screen.findByRole("dialog", { name: "Merge this epic?" })).getByRole("button", {
      name: /^Merge$/,
    })
  );
  await screen.findByRole("alert");
  const refused = writesOf(client)
    .rows()
    .find((write) => write.status === "refused");
  if (!refused) throw new Error("The merge reason was not refused");
  act(() => {
    writesOf(client).discard(refused.id);
  });
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

// B1: a status the bundle does not know — a bookmark in another casing, a deploy window — is a
// filter that matches nothing, not a page the reader loses.
test.each(["bogus", "Done", "in-progress"])(
  "an unknown ?status=%s leaves the table standing, with the value in the filter",
  async (status) => {
    const user = userEvent.setup();
    table([issue("CORE-1"), issue("CORE-2")], `?status=${status}`);
    expect(await screen.findByRole("table", { name: "Project issues table" })).toBeDefined();
    expect(screen.getByRole("searchbox", { name: "Search issues" })).toBeDefined();
    await user.click(screen.getByRole("button", { name: /^Filter Status/ }));
    expect(
      screen.getAllByRole("option", { name: new RegExp(`^${status}`) }).length
    ).toBeGreaterThan(0);
  }
);

// B2: a closed `<select>` commits on an arrow key in Chrome on Linux. Neither of the table's
// two destructive selects may write on one, so each stages a value for a button to commit.
test("an arrow key on the move or merge select writes nothing", async () => {
  const user = userEvent.setup();
  const send = spyOn(api, "send").mockResolvedValue(savedIssue("CORE-3") as never);
  restores.push(() => send.mockRestore());
  table(
    [
      issue("CORE-1", { labels: ["epic"], title: "Source" }),
      issue("CORE-2", { labels: ["epic"], title: "Target" }),
      issue("CORE-3", { parent: "CORE-1" }),
    ],
    "?group=epic"
  );
  const source = (await screen.findByRole("link", { name: "CORE-1 · Source" })).closest("tr");
  if (!source) throw new Error("The source epic has no heading row");
  await user.click(within(source).getByRole("button", { name: "Epic actions ▾" }));
  const merge = screen.getByRole("combobox", { name: "Merge into another epic" });
  merge.focus();
  await user.keyboard("{ArrowDown}");
  expect(send).not.toHaveBeenCalled();
  expect(screen.queryByRole("dialog", { name: "Merge this epic?" })).toBeNull();
  await user.keyboard("{Escape}");

  await user.click(await screen.findByRole("checkbox", { name: "Select CORE-3" }));
  const move = screen.getByRole("combobox", { name: "Move to epic" });
  move.focus();
  await user.keyboard("{ArrowDown}");
  expect(send).not.toHaveBeenCalled();
  // The staged value is committed by its own button, and only then is anything written.
  await user.selectOptions(move, "CORE-2");
  expect(send).not.toHaveBeenCalled();
  await user.click(screen.getByRole("button", { name: /^Move$/ }));
  await waitFor(() => expect(send).toHaveBeenCalled());
});

// B2: Start next over a selection the reader cannot see all of says how many it will move.
test("Start next on a selection larger than the page asks first", async () => {
  const user = userEvent.setup();
  const send = spyOn(api, "send").mockResolvedValue(savedIssue("CORE-1") as never);
  restores.push(() => send.mockRestore());
  const rows = [
    // Ahead of the queue and out of this view's filter, so the selection has somewhere to move.
    issue("CORE-0", { labels: ["legion"], priority: 0, rank: "000" }),
    ...Array.from({ length: 30 }, (_, index) =>
      issue(`CORE-${index + 1}`, {
        labels: ["legion"],
        priority: 1,
        rank: String(index + 1).padStart(3, "0"),
      })
    ),
  ];
  table(rows, "?f.priority=1&size=25");
  await user.click(await screen.findByRole("checkbox", { name: "Select every row on this page" }));
  await user.click(screen.getByRole("button", { name: "Select all 30 matching" }));
  await user.click(
    within(screen.getByRole("toolbar")).getByRole("button", { name: /^Start next$/ })
  );
  const asked = await screen.findByRole("dialog", { name: "Start these next?" });
  expect(asked.textContent).toContain("30 issues");
  expect(send).not.toHaveBeenCalled();
  await user.click(within(asked).getByRole("button", { name: /^Start next$/ }));
  await waitFor(() => expect(send).toHaveBeenCalled());
});

// F1: the button named Clear filters goes back to the view the table opens with, which is not
// "every closed issue in the project".
test("Clear filters restores the default view rather than loading every closed issue", async () => {
  const user = userEvent.setup();
  const { listing } = table([issue("CORE-1")], "?q=rank&status=done");
  await user.click(await screen.findByRole("button", { name: "Clear filters" }));
  await waitFor(() =>
    expect(screen.getByTestId("route-search")?.textContent ?? "").not.toContain("nofilter")
  );
  expect(screen.getByTestId("route-search").textContent).not.toContain("status=");
  expect(listing.mock.calls.every(([options]) => options?.open !== false)).toBe(true);
});

// F3: the sentence a merge posted is kept with the record, so a target renamed or closed before
// Finish cannot make the resume post a second one.
test.each([
  ["renamed", { title: "Payments and billing" }],
  ["closed", { status: "done" as const }],
])("a resumed merge whose target was %s posts no second reason", async (_name, change) => {
  const user = userEvent.setup();
  const paths: string[] = [];
  const send = spyOn(api, "send").mockImplementation(async (request) => {
    paths.push(request.path);
    return savedIssue("CORE-1") as never;
  });
  const messages = spyOn(api, "listIssueMessages").mockResolvedValue([
    { message: { body: "Merged into CORE-2 (“Payments”) from the issues table." } },
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
        reasonBody: "Merged into CORE-2 (“Payments”) from the issues table.",
        writes: {},
      },
    })
  );
  table([
    issue("CORE-1", { labels: ["epic"], title: "Source" }),
    issue("CORE-2", { labels: ["epic"], title: "Payments", ...change }),
  ]);
  const recovery = await screen.findByRole("region", { name: "Interrupted table action" });
  await user.click(within(recovery).getByRole("button", { name: "Finish" }));
  await waitFor(() => expect(paths).toEqual(["/api/v1/issues/CORE-1"]));
  expect(paths.some((path) => path.endsWith("/messages"))).toBe(false);
});

// F4: a parent this reader cannot read costs its own rows their epic, and nothing else.
test("one unreadable ancestor leaves every other row its epic", async () => {
  const get = spyOn(api, "getIssue").mockImplementation(async (key: string) =>
    key === "CORE-99"
      ? Promise.reject(new Error("404 NOT_FOUND"))
      : ({ ...savedIssue(key), labels: ["epic"], title: `Epic ${key}` } as never)
  );
  restores.push(() => get.mockRestore());
  const { client } = table([issue("CORE-1")]);
  await screen.findByRole("row", { name: "CORE-1 Work CORE-1" });
  act(() => {
    client.setQueryData(
      projectIssuesQuery("CORE", [], true).queryKey,
      Array.from({ length: 12 }, (_, index) =>
        issue(`CORE-${index + 1}`, { parent: index === 0 ? "CORE-99" : `CORE-${100 + index}` })
      )
    );
  });
  expect((await screen.findByRole("alert")).textContent).toContain("Could not load 1 ancestor");
  await waitFor(() => expect(screen.getAllByText("Unknown epic").length).toBe(1));
});

// F6: a page never ends with a heading whose rows all start on the next one, and a heading
// that shows part of its group says how much of it is here.
test.each([
  ["a heading with no rows left on the page is dropped", 24, "Epic two"],
  ["a heading showing part of its group counts what is here", 20, "3 of 20 issues"],
])("%s", async (_name, first, expected) => {
  const epics = [
    issue("CORE-1", { labels: ["epic"], title: "Epic one" }),
    issue("CORE-2", { labels: ["epic"], title: "Epic two" }),
  ];
  const leaves = [
    ...Array.from({ length: first }, (_, index) =>
      issue(`CORE-1${String(index).padStart(2, "0")}`, { parent: "CORE-1" })
    ),
    ...Array.from({ length: 20 }, (_, index) =>
      issue(`CORE-2${String(index).padStart(2, "0")}`, { parent: "CORE-2" })
    ),
  ];
  table([...epics, ...leaves], "?group=epic&size=25");
  await screen.findByRole("row", { name: "CORE-100 Work CORE-100" });
  const rows = [...document.querySelectorAll("tbody tr")].filter(
    (row) => row.querySelector("td[colspan]") !== null || row.hasAttribute("data-issue-row")
  );
  // No heading stands last, and none is followed straight by another heading.
  const isHeading = (row: Element | undefined) =>
    row !== undefined && row.querySelector("td[colspan]") !== null;
  expect(isHeading(rows.at(-1))).toBe(false);
  for (const [index, row] of rows.entries())
    if (isHeading(row)) expect(isHeading(rows[index + 1])).toBe(false);
  if (expected === "Epic two") expect(headings().join(" | ")).not.toContain(expected);
  else expect(headings().join(" | ")).toContain(expected);
});

// F7: the steps of a start-next are known before the first write, so the store is written once.
// Three issues move up, because with one a record written once per issue is the same two writes.
test("Start next writes the store once for the whole batch", async () => {
  const user = userEvent.setup();
  const send = spyOn(api, "send").mockResolvedValue(savedIssue("CORE-1") as never);
  restores.push(() => send.mockRestore());
  const rows = Array.from({ length: 10 }, (_, index) =>
    issue(`CORE-${index + 1}`, { labels: ["legion"], rank: String(index).padStart(3, "0") })
  );
  table(rows);
  await screen.findByRole("row", { name: "CORE-10 Work CORE-10" });
  for (const key of ["CORE-8", "CORE-9", "CORE-10"])
    await user.click(screen.getByRole("checkbox", { name: `Select ${key}` }));
  const written: string[] = [];
  restores.push(
    onLocalStorage("setItem", (write) => (key, value) => {
      written.push(value);
      write(key, value);
    })
  );
  await user.click(
    within(screen.getByRole("toolbar")).getByRole("button", { name: /^Start next$/ })
  );
  // The notice follows the last rank write and the record's dismissal, so no write of the
  // action lands after it.
  await waitFor(() =>
    expect(
      within(screen.getByRole("region", { name: "Project issues" })).getByRole("status").textContent
    ).toContain("3 issues now start next")
  );
  expect(send).toHaveBeenCalledTimes(3);
  const records = written.filter((value) => value.includes('"kind":"startNext"'));
  // One write to begin the action and one for its whole batch of keys, not one per issue.
  expect(records.length).toBe(2);
});

// O8: a record a tab left behind a day ago is not this reader's unfinished work.
test("an action record older than a day is gone, and a new action may begin", async () => {
  const user = userEvent.setup();
  const send = spyOn(api, "send").mockResolvedValue(savedIssue("CORE-1") as never);
  restores.push(() => send.mockRestore());
  localStorage.setItem(
    "dispatch.project-table.CORE",
    JSON.stringify({
      action: {
        id: "yesterday",
        kind: "startNext",
        project: "CORE",
        keys: ["CORE-1"],
        startedAt: Date.now() - 25 * 60 * 60 * 1000,
        writes: {},
      },
    })
  );
  table([
    issue("CORE-1", { labels: ["legion"], rank: "b" }),
    issue("CORE-2", { labels: ["legion"], rank: "a" }),
  ]);
  await screen.findByRole("row", { name: "CORE-1 Work CORE-1" });
  expect(screen.queryByRole("region", { name: "Interrupted table action" })).toBeNull();
  await user.click(screen.getByRole("checkbox", { name: "Select CORE-1" }));
  await user.click(
    within(screen.getByRole("toolbar")).getByRole("button", { name: /^Start next$/ })
  );
  await waitFor(() => expect(send).toHaveBeenCalled());
});

// I5: a refused write the reader retried in Pending changes answers no second retry, and Finish
// must not sit waiting for an outcome that will never come.
test("Finish completes when a refused write was retried from Pending changes", async () => {
  const user = userEvent.setup();
  let refuse = true;
  const send = spyOn(api, "send").mockImplementation(async () => {
    if (refuse) throw new ApiError(403, { error: "refused", code: "FORBIDDEN" });
    return savedIssue("CORE-2") as never;
  });
  restores.push(() => send.mockRestore());
  const { client } = table([
    issue("CORE-1", { labels: ["epic"], title: "Epic" }),
    issue("CORE-2", { labels: ["legion"], rank: "b" }),
    issue("CORE-3", { labels: ["legion"], rank: "a" }),
  ]);
  await user.click(await screen.findByRole("checkbox", { name: "Select CORE-2" }));
  await user.click(
    within(screen.getByRole("toolbar")).getByRole("button", { name: /^Start next$/ })
  );
  await screen.findByRole("alert");
  refuse = false;
  const refused = writesOf(client)
    .rows()
    .find((write) => write.status === "refused");
  if (!refused) throw new Error("The rank write was not refused");
  // The reader retries it themselves, so Finish finds a write already on its way.
  act(() => {
    writesOf(client).retry(refused.id);
  });
  const recovery = await screen.findByRole("region", { name: "Interrupted table action" });
  await user.click(within(recovery).getByRole("button", { name: "Finish" }));
  await waitFor(() =>
    expect(screen.queryByRole("region", { name: "Interrupted table action" })).toBeNull()
  );
});

// A collapsed group keeps its heading however few of its rows the page holds: the heading is
// the only way back to them.
test("a collapsed group keeps its heading and reopens", async () => {
  const user = userEvent.setup();
  table(
    [
      issue("CORE-1", { status: "backlog", title: "Alpha" }),
      issue("CORE-2", { status: "todo", title: "Bravo" }),
    ],
    "?group=status"
  );
  await screen.findByRole("row", { name: "CORE-2 Bravo" });
  const todo = screen.getByRole("button", { name: /Todo.*1 issue/ });
  await user.click(todo);
  await waitFor(() => expect(screen.queryByRole("row", { name: "CORE-2 Bravo" })).toBeNull());
  // The heading is still there, and still reads the group's own count.
  const collapsed = screen.getByRole("button", { name: /Todo.*1 issue/ });
  expect(collapsed.textContent).not.toContain("0 of");
  await user.click(collapsed);
  expect(await screen.findByRole("row", { name: "CORE-2 Bravo" })).toBeDefined();
});

/** The issue keys the table is listing, in the order the rows stand in. */
function listedKeys(): string[] {
  return screen
    .getAllByRole("row")
    .filter((row) => row.hasAttribute("data-issue-row"))
    .map((row) => row.getAttribute("data-issue-row") ?? "");
}

test("a scoped table lists the issues the server answers for the priority, over every project", async () => {
  const { listing } = table(
    [
      issue("CORE-1"),
      issue("CORE-2", { advances: [{ id: "PRI-7", inherited: false }] }),
      issue("OPS-3", { advances: [{ id: "PRI-7", inherited: true }] }),
    ],
    "",
    undefined,
    // A scoped table that names no layout, the fallback: the priority's own listing, no project
    // and the priorities' titles. `PriorityPage` passes the same with a layout and key of its own
    // (`priority-page-work`).
    {
      listing: { advances: "PRI-7" },
      priorityTitles: new Map([["PRI-7", "Ship it"]]),
      stateKey: "w",
    }
  );
  await screen.findByRole("row", { name: "CORE-2 Work CORE-2" });
  expect(listedKeys().sort()).toEqual(["CORE-2", "OPS-3"]);
  expect(screen.queryByRole("row", { name: "CORE-1 Work CORE-1" })).toBeNull();
  // The narrowing is the server's, and it names no project: a priority belongs to none.
  expect(listing).toHaveBeenCalledWith({ advances: ["PRI-7"], open: true });
  expect(listing.mock.calls.every(([options]) => options?.project === undefined)).toBe(true);
  // The table is named for what it lists rather than for a project it does not stand on.
  expect(screen.getByRole("table", { name: "Priority issues table" })).toBeDefined();
  expect(screen.queryByRole("table", { name: "Project issues table" })).toBeNull();
  // The fallback's one stored layout, shared by every scoped table that names no layout, under
  // no key that names a priority; `PriorityPage` keeps its own key (`priority-page-work`). It has
  // Project and Advances, both hidden until the reader shows them, and no Moved there or Reason,
  // since its listing asks for no move.
  expect(localStorage.getItem("dispatch.project-table.priority-work")).toBe(
    '{"sizing":{},"order":[],"visibility":{"updated":false,"project":false,"advances":false}}'
  );
  expect(localStorage.getItem("dispatch.project-table.priority:PRI-7")).toBeNull();
});

test("a scoped table offers no epic action, because an epic belongs to a project", async () => {
  const user = userEvent.setup();
  table(
    [
      issue("CORE-2", { advances: [{ id: "PRI-7", inherited: false }] }),
      issue("OPS-3", { advances: [{ id: "PRI-7", inherited: false }] }),
    ],
    "",
    undefined,
    { listing: { advances: "PRI-7" }, stateKey: "w" }
  );
  await user.click(await screen.findByRole("checkbox", { name: "Select CORE-2" }));
  await user.click(screen.getByRole("checkbox", { name: "Select OPS-3" }));
  // The selection is of two projects, so no epic could hold it and none is offered; what each
  // issue can be written one at a time still is.
  expect(screen.queryByRole("button", { name: "New epic from these" })).toBeNull();
  expect(screen.getByRole("button", { name: "Start next" })).toBeDefined();
  expect(screen.getByRole("button", { name: "Take out of epic" })).toBeDefined();
});

test("a stateKey puts the table's own view under its prefix, and none keeps the bare keys", async () => {
  const user = userEvent.setup();
  const { view } = table([issue("CORE-1", { title: "Sextant" })], "", undefined, {
    stateKey: "advances",
  });
  await user.type(await screen.findByRole("searchbox", { name: "Search issues" }), "sextant");
  await waitFor(() =>
    expect(screen.getByTestId("route-search").textContent).toContain("advances.q=sextant")
  );
  expect(screen.getByTestId("route-search").textContent).not.toContain("&q=");
  view.unmount();
  // The project page passes no stateKey, so the address it publishes is the bare one.
  table([issue("CORE-2", { title: "Astrolabe" })]);
  await user.type(await screen.findByRole("searchbox", { name: "Search issues" }), "astrolabe");
  await waitFor(() =>
    expect(screen.getByTestId("route-search").textContent).toContain("q=astrolabe")
  );
  expect(screen.getByTestId("route-search").textContent).not.toContain("advances.");
});

test("the narrowing chip shows the picked issues alone, and its remove gives the table back", async () => {
  const user = userEvent.setup();
  table(
    [issue("CORE-1"), issue("CORE-2"), issue("CORE-3")],
    "?only=CORE-1,CORE-3&onlyLabel=Finished+last+week&nofilter=1"
  );
  await screen.findByRole("row", { name: "CORE-1 Work CORE-1" });
  expect(listedKeys().sort()).toEqual(["CORE-1", "CORE-3"]);
  const chip = screen.getByRole("button", { name: "Stop showing only Finished last week" });
  expect(chip.textContent).toContain("Finished last week");
  expect(chip.textContent).toContain("2 of 2 issues");
  await user.click(chip);
  await waitFor(() => expect(listedKeys().sort()).toEqual(["CORE-1", "CORE-2", "CORE-3"]));
  expect(screen.getByTestId("route-search").textContent).not.toContain("only=");
  expect(screen.queryByRole("button", { name: /^Stop showing only/ })).toBeNull();
});

// The listing columns exist only where they mean something: a project's own table would name its
// own project on every row, it is given no priority's title, and its listing carries no move, so
// it neither shows nor offers any of the four.
test("a project's table offers none of the listing columns", async () => {
  const user = userEvent.setup();
  table([issue("CORE-1")]);
  const list = await screen.findByRole("table", { name: "Project issues table" });
  await within(list).findByRole("row", { name: "CORE-1 Work CORE-1" });
  const headers = within(list)
    .getAllByRole("columnheader")
    .map((header) => header.textContent ?? "");
  for (const name of ["Project", "Advances", "Moved there", "Reason"])
    expect(headers.some((text) => text.startsWith(name))).toBe(false);
  // Nor does it show Updated, which the fault lists open with shown: a project's table opens
  // with it hidden, as it always has.
  expect(headers.some((text) => text.startsWith("Updated"))).toBe(false);
  await user.click(screen.getByRole("button", { name: "Columns" }));
  expect(screen.getByRole("checkbox", { name: "Updated" })).not.toBeChecked();
  for (const name of ["Project", "Advances", "Moved there", "Reason"])
    expect(screen.queryByRole("checkbox", { name })).toBeNull();
});

// The work under one priority is every project's, so its table offers the Project column; given
// the priorities' titles it offers Advances, which names each by its place in the order and its
// title; and its listing asks for no move, so it never offers Moved there, nor Reason, which says
// why a move was made. Both open hidden, for the reader to show.
test("a priority's work table offers Project and, given the titles, Advances, and never Moved there or Reason", async () => {
  const user = userEvent.setup();
  table(
    [
      issue("CORE-1", {
        advances: [
          { id: "ship-it", inherited: false },
          { id: "keep-it", inherited: true },
        ],
      }),
    ],
    "",
    undefined,
    {
      listing: { advances: "ship-it" },
      priorityTitles: new Map([
        ["ship-it", "Ship it everywhere"],
        ["keep-it", "Keep it running"],
      ]),
      stateKey: "w",
    }
  );
  const list = await screen.findByRole("table", { name: "Priority issues table" });
  await within(list).findByRole("row", { name: "CORE-1 Work CORE-1" });
  const headers = () =>
    within(list)
      .getAllByRole("columnheader")
      .map((header) => header.textContent ?? "");
  await user.click(screen.getByRole("button", { name: "Columns" }));
  for (const name of ["Project", "Advances"])
    expect(screen.getByRole("checkbox", { name })).not.toBeChecked();
  for (const name of ["Moved there", "Reason"])
    expect(screen.queryByRole("checkbox", { name })).toBeNull();
  await user.click(screen.getByRole("checkbox", { name: "Project" }));
  await user.click(screen.getByRole("checkbox", { name: "Advances" }));
  await waitFor(() => expect(headers()).toContain("Advances"));
  for (const name of ["Moved there", "Reason"])
    expect(headers().some((text) => text.startsWith(name))).toBe(false);
  // A listing column names a row's value and does not sort, filter or group by it: the
  // address keeps only the registry's columns (`issues-table-model.ts`).
  expect(within(list).queryByRole("button", { name: "Filter Project" })).toBeNull();
  const row = within(list).getByRole("row", { name: "CORE-1 Work CORE-1" });
  const cells = within(row).getAllByRole("cell");
  expect(cells[headers().indexOf("Project")]?.textContent).toBe("CORE");
  // Each priority is a chip reading its place in the order and then its title, whole, in one link
  // to its page, its title in its name and its tooltip; one the issue inherits from an issue
  // above says so in both.
  const chips = within(cells[headers().indexOf("Advances")] as HTMLElement).getAllByRole("link");
  expect(
    chips.map((link) => ({
      drawn: [...link.children].map((part) => part.textContent),
      href: link.getAttribute("href"),
      name: link.getAttribute("aria-label"),
      title: link.getAttribute("title"),
    }))
  ).toEqual([
    {
      drawn: ["#1", "Ship it everywhere"],
      href: "/priorities/ship-it",
      name: "#1 Ship it everywhere",
      title: "Ship it everywhere",
    },
    {
      drawn: ["#2", "Keep it running"],
      href: "/priorities/keep-it",
      name: "#2 Keep it running, inherited",
      title: "Keep it running, inherited from an issue above",
    },
  ]);
});

// Without the titles a priority's work table has no Advances column: it could name each
// priority only by its id, which is no name to a reader.
test("a priority's work table given no titles offers no Advances column", async () => {
  const user = userEvent.setup();
  table([issue("CORE-1", { advances: [{ id: "ship-it", inherited: false }] })], "", undefined, {
    listing: { advances: "ship-it" },
    stateKey: "w",
  });
  const list = await screen.findByRole("table", { name: "Priority issues table" });
  await within(list).findByRole("row", { name: "CORE-1 Work CORE-1" });
  await user.click(screen.getByRole("button", { name: "Columns" }));
  expect(screen.getByRole("checkbox", { name: "Project" })).toBeDefined();
  for (const name of ["Advances", "Moved there", "Reason"])
    expect(screen.queryByRole("checkbox", { name })).toBeNull();
});

// A fault list names the statuses it holds, and the server answers every row of it in one read:
// even a Status filter that picks Done reads no closed issue beside it.
test("a listing that names its statuses is read with them alone, never with every closed issue", async () => {
  const { listing } = table([issue("CORE-1")], "?k.status=done", undefined, {
    listing: { advances: "any", statuses: ["backlog", "icebox"], moved: true },
    stateKey: "k",
  });
  await screen.findByRole("table", { name: "Priority issues table" });
  expect(listing).toHaveBeenCalledWith({
    advances: ["any"],
    status: ["backlog", "icebox"],
    moved: true,
    open: true,
  });
  expect(listing.mock.calls.every(([options]) => options?.status !== undefined)).toBe(true);
  expect(screen.queryByText("Loading closed issues…")).toBeNull();
});

// D12 (INFRA-4731): New epic, Move to epic and Merge keep each parent's status in line with its
// children's (R24). Moving an issue in work under an epic that is not in work, or any open issue
// under a done epic, first asks to move the epic into work, listing each change that makes. A yes
// sends that status change, then the action's own writes. A Cancel writes nothing.

/** An epic as Dispatch answers it, at `status`. */
function epicIssue(key: string, title: string, status: string): Issue {
  return {
    ...savedIssue(key),
    closed_at: status === "done" ? "2026-09-10T00:00:00Z" : null,
    labels: ["epic"],
    status,
    title,
  };
}

/** One write an epic action sent: the route it took, the issue it names, and its body. */
type EpicWrite = readonly [
  route: "create" | "message" | "PATCH" | "status-change",
  key: string,
  body: unknown,
];

/** Answers each merge reason an action posts, and lists it in `sent`. */
function answerMergeReasons(sent: EpicWrite[]) {
  answerRoute<(key: string, body: unknown) => Promise<unknown>>(
    "POST",
    "/api/v1/issues/*/messages"
  ).mockImplementation(async (key, body) => {
    sent.push(["message", key, body]);
    return {};
  });
}

/**
 * Answers every write an epic action can make, and lists each in the order the page sent it: a
 * create (the epic CORE-10), each request of the status-change route (a dry run answers the epic
 * it names as `epics` holds it, and a change moves that epic into work), each issue `PATCH`, and
 * each merge reason.
 */
function answerEpicWrites(epics: readonly Issue[]): EpicWrite[] {
  const sent: EpicWrite[] = [];
  const held = (key: string) => epics.find((epic) => epic.key === key) ?? savedIssue(key);
  answerStatusChanges().mockImplementation(async (key, input) => {
    sent.push(["status-change", key, input]);
    const now = held(key);
    const moved: Issue = { ...now, closed_at: null, status: "in_progress" };
    const entry = { key, parent: null, previous_last_seq: now.last_seq, status: "in_progress" };
    return input.dry_run
      ? { changed: [entry], issue: now }
      : { changed: [{ ...entry, issue: moved }], issue: moved };
  });
  answerIssuePatches().mockImplementation(async (key, input) => {
    sent.push(["PATCH", key, input]);
    return savedIssue(key);
  });
  answerMergeReasons(sent);
  const create = spyOn(api, "createIssue").mockImplementation(async (input) => {
    sent.push(["create", "CORE-10", input]);
    return held("CORE-10");
  });
  restores.push(() => create.mockRestore());
  return sent;
}

const intoWorkDryRun = { dry_run: true, include_parent: true, status: "in_progress" };
const intoWork = { include_parent: true, status: "in_progress" };

/** Selects `key` and presses Move with the epic `epic` picked in the selection bar. */
async function moveToEpic(user: UserEvent, key: string, epic: string) {
  await user.click(await screen.findByRole("checkbox", { name: `Select ${key}` }));
  await user.selectOptions(screen.getByRole("combobox", { name: "Move to epic" }), epic);
  await user.click(screen.getByRole("button", { name: "Move" }));
}

/** Selects `key` and makes a new epic named `title` of it from the selection bar. */
async function newEpicOf(user: UserEvent, key: string, title: string) {
  await user.click(await screen.findByRole("checkbox", { name: `Select ${key}` }));
  await user.click(screen.getByRole("button", { name: "New epic from these" }));
  await nameTheEpic(user, title);
}

/** Presses Merge… for the epic `source` into `target`, from the source's heading. */
async function mergeInto(user: UserEvent, source: string, target: string) {
  const link = await screen.findByRole("link", { name: new RegExp(`^${source} · `) });
  const heading = link.closest("tr");
  if (!heading) throw new Error(`The epic ${source} has no heading row`);
  await user.click(within(heading).getByRole("button", { name: "Epic actions ▾" }));
  await user.selectOptions(
    screen.getByRole("combobox", { name: "Merge into another epic" }),
    target
  );
  await user.click(screen.getByRole("button", { name: "Merge…" }));
}

/** What the table says once an action ends. */
async function noticeOnceDone(text: string) {
  await waitFor(() =>
    expect(
      within(screen.getByRole("region", { name: "Project issues" })).getByRole("status").textContent
    ).toContain(text)
  );
}

test("Move to epic of an issue in work to an epic on the backlog asks, and on yes moves the epic into work before the issue", async () => {
  const user = userEvent.setup();
  const sent = answerEpicWrites([epicIssue("CORE-9", "Platform", "backlog")]);
  table([
    issue("CORE-1", { status: "in_progress" }),
    issue("CORE-9", { labels: ["epic"], status: "backlog", title: "Platform" }),
  ]);
  await moveToEpic(user, "CORE-1", "CORE-9");
  const dialog = await screen.findByRole("dialog", { name: "Move CORE-9 into work too?" });
  expect(dialog.textContent).toContain(
    "CORE-1 “Work CORE-1” is in work, and the epic CORE-9 “Platform” is not. An epic stays in work while an issue under it is in work. Before the move, Dispatch makes these changes:"
  );
  expect(within(dialog).getByRole("list", { name: "Changes" }).textContent).toBe(
    "CORE-9 “Platform” moves to In progress"
  );
  expect(sent).toEqual([["status-change", "CORE-9", intoWorkDryRun]]);
  await user.click(within(dialog).getByRole("button", { name: "Move into work too" }));
  await noticeOnceDone("Moved CORE-1 “Work CORE-1” to the epic “Platform”.");
  expect(sent).toEqual([
    ["status-change", "CORE-9", intoWorkDryRun],
    ["status-change", "CORE-9", intoWork],
    ["PATCH", "CORE-1", { parent: "CORE-9" }],
  ]);
});

test("New epic over an issue in work asks before its create, and on yes makes the epic, moves it into work, then the issue", async () => {
  const user = userEvent.setup();
  const sent = answerEpicWrites([epicIssue("CORE-10", "Platform", "triage")]);
  table([issue("CORE-1", { status: "in_progress" })]);
  await newEpicOf(user, "CORE-1", "Platform");
  const dialog = await screen.findByRole("dialog", { name: "Move “Platform” into work too?" });
  expect(dialog.textContent).toContain(
    "CORE-1 “Work CORE-1” is in work, and a new epic is not. An epic stays in work while an issue under it is in work. Dispatch makes the epic and then makes this change:"
  );
  expect(within(dialog).getByRole("list", { name: "Changes" }).textContent).toBe(
    "“Platform” moves to In progress"
  );
  // A new epic is no issue yet, so the question needs no dry run.
  expect(sent).toEqual([]);
  await user.click(within(dialog).getByRole("button", { name: "Move into work too" }));
  await noticeOnceDone("Made the epic “Platform” from CORE-1 “Work CORE-1”.");
  expect(sent).toEqual([
    ["create", "CORE-10", { labels: ["epic"], project: "CORE", title: "Platform" }],
    ["status-change", "CORE-10", intoWork],
    ["PATCH", "CORE-1", { parent: "CORE-10" }],
  ]);
});

test("Merge of an epic holding an issue in work into an epic on the backlog asks once, and on yes moves the target into work before the merge's writes", async () => {
  const user = userEvent.setup();
  const sent = answerEpicWrites([epicIssue("CORE-2", "Target", "backlog")]);
  table(
    [
      issue("CORE-1", { labels: ["epic"], status: "in_progress", title: "Source" }),
      issue("CORE-2", { labels: ["epic"], status: "backlog", title: "Target" }),
      issue("CORE-3", { parent: "CORE-1", status: "in_progress" }),
    ],
    "?group=epic"
  );
  await mergeInto(user, "CORE-1", "CORE-2");
  // The one question says what the merge does, and what it changes first.
  const dialog = await screen.findByRole("dialog", { name: "Move CORE-2 into work too?" });
  expect(dialog.textContent).toContain(
    "1 open issue moves from CORE-1 “Source” to CORE-2 “Target”, and CORE-1 closes as done. CORE-3 “Work CORE-3” is in work, and the epic CORE-2 “Target” is not."
  );
  expect(screen.queryByRole("dialog", { name: "Merge this epic?" })).toBeNull();
  expect(sent).toEqual([["status-change", "CORE-2", intoWorkDryRun]]);
  await user.click(within(dialog).getByRole("button", { name: "Move into work too" }));
  await noticeOnceDone("Merged “Source” into “Target”.");
  expect(sent).toEqual([
    ["status-change", "CORE-2", intoWorkDryRun],
    ["status-change", "CORE-2", intoWork],
    ["PATCH", "CORE-3", { parent: "CORE-2" }],
    ["message", "CORE-1", { body: "Merged into CORE-2 (“Target”) from the issues table." }],
    ["PATCH", "CORE-1", { status: "done" }],
  ]);
});

/** One epic action of a table of cases: what the table lists, at which address, how the action
 *  is driven, and the writes it has sent once it is done. */
interface EpicActionCase {
  readonly action: string;
  readonly rows: () => IssueSummary[];
  readonly search: string;
  readonly run: (user: UserEvent) => Promise<void>;
  readonly sent: EpicWrite[];
}

/** Each epic action over an issue in work, driven up to the question it asks (`question`), with
 *  the one write it has sent by then: the dry run, or none for a new epic, which is no issue yet. */
const askingActions: (EpicActionCase & { readonly question: string })[] = [
  {
    action: "Move to epic",
    rows: () => [
      issue("CORE-1", { status: "in_progress" }),
      issue("CORE-9", { labels: ["epic"], status: "backlog", title: "Platform" }),
    ],
    search: "",
    run: (user: UserEvent) => moveToEpic(user, "CORE-1", "CORE-9"),
    question: "Move CORE-9 into work too?",
    sent: [["status-change", "CORE-9", intoWorkDryRun]],
  },
  {
    action: "New epic",
    rows: () => [issue("CORE-1", { status: "in_progress" })],
    search: "",
    run: (user: UserEvent) => newEpicOf(user, "CORE-1", "Platform"),
    question: "Move “Platform” into work too?",
    sent: [],
  },
  {
    action: "Merge",
    rows: () => [
      issue("CORE-1", { labels: ["epic"], status: "in_progress", title: "Source" }),
      issue("CORE-2", { labels: ["epic"], status: "backlog", title: "Target" }),
      issue("CORE-3", { parent: "CORE-1", status: "in_progress" }),
    ],
    search: "?group=epic",
    run: (user: UserEvent) => mergeInto(user, "CORE-1", "CORE-2"),
    question: "Move CORE-2 into work too?",
    sent: [["status-change", "CORE-2", intoWorkDryRun]],
  },
];

test.each(askingActions)("$action's question, cancelled, writes nothing", async (action) => {
  const user = userEvent.setup();
  const sent = answerEpicWrites([
    epicIssue("CORE-2", "Target", "backlog"),
    epicIssue("CORE-9", "Platform", "backlog"),
    epicIssue("CORE-10", "Platform", "triage"),
  ]);
  table(action.rows(), action.search);
  await action.run(user);
  const dialog = await screen.findByRole("dialog", { name: action.question });
  await user.click(within(dialog).getByRole("button", { name: "Cancel" }));
  expect(screen.queryByRole("dialog")).toBeNull();
  // Nothing was written: the dry run, where the action sends one, is its one request.
  expect(sent).toEqual(action.sent);
  expect(
    within(screen.getByRole("region", { name: "Project issues" })).queryByRole("status")
  ).toBeNull();
});

/** Each epic action over issues none of which is in work, which asks nothing, and the writes it
 *  makes as it did before the rule. */
const quietActions: (EpicActionCase & { readonly said: string })[] = [
  {
    action: "Move to epic",
    rows: () => [
      issue("CORE-1", { status: "backlog" }),
      issue("CORE-9", { labels: ["epic"], status: "backlog", title: "Platform" }),
    ],
    search: "",
    run: (user: UserEvent) => moveToEpic(user, "CORE-1", "CORE-9"),
    said: "Moved CORE-1 “Work CORE-1” to the epic “Platform”.",
    sent: [["PATCH", "CORE-1", { parent: "CORE-9" }]],
  },
  {
    action: "New epic",
    rows: () => [issue("CORE-1", { status: "backlog" })],
    search: "",
    run: (user: UserEvent) => newEpicOf(user, "CORE-1", "Platform"),
    said: "Made the epic “Platform” from CORE-1 “Work CORE-1”.",
    sent: [
      ["create", "CORE-10", { labels: ["epic"], project: "CORE", title: "Platform" }],
      ["PATCH", "CORE-1", { parent: "CORE-10" }],
    ],
  },
  {
    action: "Merge",
    rows: () => [
      issue("CORE-1", { labels: ["epic"], status: "backlog", title: "Source" }),
      issue("CORE-2", { labels: ["epic"], status: "backlog", title: "Target" }),
      issue("CORE-3", { parent: "CORE-1", status: "backlog" }),
    ],
    search: "?group=epic",
    // A merge closes an issue, so it still asks its own question.
    run: async (user: UserEvent) => {
      await mergeInto(user, "CORE-1", "CORE-2");
      await user.click(
        within(await screen.findByRole("dialog", { name: "Merge this epic?" })).getByRole(
          "button",
          { name: "Merge" }
        )
      );
    },
    said: "Merged “Source” into “Target”.",
    sent: [
      ["PATCH", "CORE-3", { parent: "CORE-2" }],
      ["message", "CORE-1", { body: "Merged into CORE-2 (“Target”) from the issues table." }],
      ["PATCH", "CORE-1", { status: "done" }],
    ],
  },
];

test.each(quietActions)("$action with no issue in work sends no dry run", async (action) => {
  const user = userEvent.setup();
  const sent = answerEpicWrites([
    epicIssue("CORE-2", "Target", "backlog"),
    epicIssue("CORE-9", "Platform", "backlog"),
    epicIssue("CORE-10", "Platform", "triage"),
  ]);
  table(action.rows(), action.search);
  await action.run(user);
  await noticeOnceDone(action.said);
  expect(sent).toEqual(action.sent);
});

// An epic that closes once the reader picked it: the open listing (the default view) leaves it
// out, so the page cannot tell its status and asks Dispatch, while a listing of every issue reads it
// done. Either way the move asks to reopen it, with no issue in work.
test.each([
  ["the open listing", ""],
  ["every issue", "?nofilter=1"],
])(
  "Move to epic into an epic closed since it was picked asks to reopen it, over %s",
  async (_listing, search) => {
    const user = userEvent.setup();
    const sent = answerEpicWrites([epicIssue("CORE-9", "Platform", "done")]);
    // The listing answers from this array, so the epic's close below reaches the table's next read.
    const rows = [
      issue("CORE-1", { status: "backlog" }),
      issue("CORE-9", { labels: ["epic"], status: "backlog", title: "Platform" }),
    ];
    const { client } = table(rows, search);
    await user.click(await screen.findByRole("checkbox", { name: "Select CORE-1" }));
    const pick = screen.getByRole("combobox", { name: "Move to epic" });
    await user.selectOptions(pick, "CORE-9");
    // The epic closes while it is picked, and the select offers it no more.
    rows[1] = issue("CORE-9", { labels: ["epic"], status: "done", title: "Platform" });
    await act(() => client.invalidateQueries({ queryKey: ["issues"] }));
    await waitFor(() =>
      expect(within(pick).queryByRole("option", { name: "Platform" })).toBeNull()
    );
    await user.click(screen.getByRole("button", { name: "Move" }));
    const dialog = await screen.findByRole("dialog", { name: "Reopen CORE-9 too?" });
    expect(dialog.textContent).toContain(
      "The epic CORE-9 “Platform” is done. A done epic holds no open issue. Before the move, Dispatch makes these changes:"
    );
    expect(within(dialog).getByRole("list", { name: "Changes" }).textContent).toBe(
      "CORE-9 “Platform” moves to In progress"
    );
    await user.click(within(dialog).getByRole("button", { name: "Reopen it too" }));
    await noticeOnceDone("Moved CORE-1 “Work CORE-1” to the epic");
    expect(sent).toEqual([
      ["status-change", "CORE-9", intoWorkDryRun],
      ["status-change", "CORE-9", intoWork],
      ["PATCH", "CORE-1", { parent: "CORE-9" }],
    ]);
  }
);

// An action a reload cut off, finished later: the epic it moves issues under may have left work or
// closed meanwhile. Finish asks the action's own question before it moves a not-in-work epic into
// work, and a Cancel writes nothing; it refuses a closed one, as it never reopens an epic unasked.

/** A New epic a reload cut off after its create, with CORE-1 still to adopt into CORE-9. */
const madeEpic = {
  id: "made",
  kind: "newEpic",
  project: "CORE",
  keys: ["CORE-1"],
  target: "CORE-9",
  writes: {},
};

/** A merge of CORE-1 into CORE-2 a reload cut off once it began, before any of its writes. */
const mergeBegun = {
  id: "merging",
  kind: "merge",
  project: "CORE",
  keys: ["CORE-3"],
  source: "CORE-1",
  target: "CORE-2",
  writes: {},
};

/** The table `mergeBegun` stopped on: the source in work, its issue CORE-3 in work, and the target
 *  at `status` now. */
function mergeRows(status: string): IssueSummary[] {
  return [
    issue("CORE-1", { labels: ["epic"], status: "in_progress", title: "Source" }),
    issue("CORE-2", { labels: ["epic"], status, title: "Target" }),
    issue("CORE-3", { parent: "CORE-1", status: "in_progress" }),
  ];
}

/** Stores `action` as the table's interrupted action, as a reload finds it. */
function interruptedBy(action: object) {
  localStorage.setItem("dispatch.project-table.CORE", JSON.stringify({ action }));
}

/** Presses Finish on the interrupted action. */
async function pressFinish(user: UserEvent) {
  const recovery = await screen.findByRole("region", { name: "Interrupted table action" });
  await user.click(within(recovery).getByRole("button", { name: "Finish" }));
}

test("Finish of a New epic cut off after its create asks before it moves the epic into work, and on yes moves it before it adopts an issue in work", async () => {
  const user = userEvent.setup();
  const sent = answerEpicWrites([epicIssue("CORE-9", "Platform", "triage")]);
  interruptedBy(madeEpic);
  table([
    issue("CORE-1", { status: "in_progress" }),
    issue("CORE-9", { labels: ["epic"], status: "triage", title: "Platform" }),
  ]);
  await pressFinish(user);
  const dialog = await screen.findByRole("dialog", { name: "Move CORE-9 into work too?" });
  expect(dialog.textContent).toContain(
    "CORE-1 “Work CORE-1” is in work, and the epic CORE-9 “Platform” is not. An epic stays in work while an issue under it is in work. Before the move, Dispatch makes these changes:"
  );
  expect(sent).toEqual([["status-change", "CORE-9", intoWorkDryRun]]);
  await user.click(within(dialog).getByRole("button", { name: "Move into work too" }));
  await noticeOnceDone("Finished the interrupted action.");
  expect(sent).toEqual([
    ["status-change", "CORE-9", intoWorkDryRun],
    ["status-change", "CORE-9", intoWork],
    ["PATCH", "CORE-1", { parent: "CORE-9" }],
  ]);
});

test("Finish of a merge whose target closed since refuses, as New epic's Finish refuses a closed epic, and writes nothing", async () => {
  const user = userEvent.setup();
  const sent = answerEpicWrites([epicIssue("CORE-2", "Target", "done")]);
  interruptedBy(mergeBegun);
  table(mergeRows("done"));
  await pressFinish(user);
  expect((await screen.findByRole("alert")).textContent).toBe(
    "The target epic CORE-2 is unavailable or closed. The source epic CORE-1 remains open, and its issues have not moved to it."
  );
  expect(screen.queryByRole("dialog")).toBeNull();
  expect(sent).toEqual([]);
  expect(screen.getByRole("region", { name: "Interrupted table action" })).toBeDefined();
});

test("Finish of a merge whose target left work asks the merge's question, and on yes moves the target into work, then finishes the merge", async () => {
  const user = userEvent.setup();
  const rows = mergeRows("backlog");
  const sent = answerEpicWrites([epicIssue("CORE-2", "Target", "backlog")]);
  // The listing answers from `rows`, so the move of CORE-3 reaches the merge's next read of it.
  answerIssuePatches().mockImplementation(async (key, input) => {
    sent.push(["PATCH", key, input]);
    if (input.parent !== undefined) rewrite(rows, key, { parent: input.parent });
    return savedIssue(key);
  });
  const messages = spyOn(api, "listIssueMessages").mockResolvedValue([]);
  restores.push(() => messages.mockRestore());
  interruptedBy(mergeBegun);
  table(rows);
  await pressFinish(user);
  const dialog = await screen.findByRole("dialog", { name: "Move CORE-2 into work too?" });
  expect(dialog.textContent).toContain(
    "1 open issue moves from CORE-1 “Source” to CORE-2 “Target”, and CORE-1 closes as done. CORE-3 “Work CORE-3” is in work, and the epic CORE-2 “Target” is not."
  );
  expect(sent).toEqual([["status-change", "CORE-2", intoWorkDryRun]]);
  await user.click(within(dialog).getByRole("button", { name: "Move into work too" }));
  await noticeOnceDone("Finished the interrupted action.");
  expect(sent).toEqual([
    ["status-change", "CORE-2", intoWorkDryRun],
    ["status-change", "CORE-2", intoWork],
    ["PATCH", "CORE-3", { parent: "CORE-2" }],
    ["message", "CORE-1", { body: "Merged into CORE-2 (“Target”) from the issues table." }],
    ["PATCH", "CORE-1", { status: "done" }],
  ]);
});

// A merge whose move of an issue in work was refused once its target was in work leaves that move
// to Finish. Another writer then takes the target out of work. Finish asks the merge's question
// before it writes anything (D12: the table asks whatever the setting), so its first request is the
// target's dry run and no move of the issue goes out while the question is open. With the setting
// off, a move sent first would put the issue in work under an epic on the backlog, unasked.
test("Finish of a merge whose issue's move was refused, its target since out of work, asks before it sends that move again", async () => {
  const user = userEvent.setup();
  const rows = mergeRows("backlog");
  const sent = answerEpicWrites([epicIssue("CORE-2", "Target", "backlog")]);
  let refuse = true;
  answerIssuePatches().mockImplementation(async (key, input) => {
    sent.push(["PATCH", key, input]);
    if (refuse && key === "CORE-3") {
      refuse = false;
      throw new ApiError(400, { code: "REFUSED_ONCE", error: "refused once" });
    }
    return savedIssue(key);
  });
  table(rows, "?group=epic");
  const heading = (await screen.findByRole("link", { name: "CORE-1 · Source" })).closest("tr");
  if (!heading) throw new Error("The source epic has no heading row");
  await user.click(within(heading).getByRole("button", { name: "Epic actions ▾" }));
  await user.selectOptions(
    screen.getByRole("combobox", { name: "Merge into another epic" }),
    "CORE-2"
  );
  await user.click(screen.getByRole("button", { name: "Merge…" }));
  const asked = await screen.findByRole("dialog", { name: "Move CORE-2 into work too?" });
  await user.click(within(asked).getByRole("button", { name: "Move into work too" }));
  await screen.findByRole("alert");
  expect(sent).toEqual([
    ["status-change", "CORE-2", intoWorkDryRun],
    ["status-change", "CORE-2", intoWork],
    ["PATCH", "CORE-3", { parent: "CORE-2" }],
  ]);
  // The target is out of work again: the listing and the dry run both read it on the backlog.
  const before = sent.length;
  await pressFinish(user);
  await screen.findByRole("dialog", { name: "Move CORE-2 into work too?" });
  expect(sent.slice(before)).toEqual([["status-change", "CORE-2", intoWorkDryRun]]);
});

// Every Finish below starts from a stored action holding a refused write. Finish waits on what is
// in flight and sends nothing again until the kind's guard has answered: the guard asks the
// action's own question where the moves it still makes need the epic in work, a Cancel writes
// nothing, and a step the guard decides afresh (the move into work, a merge's close of its
// source) is never sent again, its refused write gone from Pending changes once the guard goes on.

/** Changes `key` in `rows`, which the table's listing answers from, so its next read finds it. */
function rewrite(rows: IssueSummary[], key: string, fields: Partial<IssueSummary>) {
  const at = rows.findIndex((row) => row.key === key);
  const row = rows[at];
  if (row !== undefined) rows[at] = { ...row, ...fields };
}

/**
 * Answers every write an epic action makes as Dispatch does over `rows`, which the table's listing
 * answers from, and makes each write there, so the listing and the dry run read after it find it:
 * a dry run answers its epic as `rows` hold it, a move into work puts the epic in work, a `PATCH`
 * sets the parent or the status it names, a create adds the epic CORE-10 in triage, and an issue's
 * messages are the reasons posted to it. The first write of each of `refusedOnce` (`"PATCH
 * CORE-3"`, `"status-change CORE-2"`, `"create CORE-10"`) is refused. Lists each write in the order
 * the page sent it, as `answerEpicWrites` does.
 */
function answerOnRows(rows: IssueSummary[], refusedOnce: readonly string[] = []): EpicWrite[] {
  const sent: EpicWrite[] = [];
  answerMergeReasons(sent);
  const refusing = new Set(refusedOnce);
  const refuse = (write: string) => {
    if (refusing.delete(write))
      throw new ApiError(400, { code: "REFUSED_ONCE", error: `${write} refused once` });
  };
  answerStatusChanges().mockImplementation(async (key, input) => {
    sent.push(["status-change", key, input]);
    const row = rows.find((candidate) => candidate.key === key);
    const now = epicIssue(key, row?.title ?? `Work ${key}`, row?.status ?? "triage");
    const entry = { key, parent: null, previous_last_seq: now.last_seq, status: "in_progress" };
    if (input.dry_run) return { changed: [entry], issue: now };
    refuse(`status-change ${key}`);
    rewrite(rows, key, { status: "in_progress" });
    const moved: Issue = { ...now, closed_at: null, status: "in_progress" };
    return { changed: [{ ...entry, issue: moved }], issue: moved };
  });
  answerIssuePatches().mockImplementation(async (key, input) => {
    sent.push(["PATCH", key, input]);
    refuse(`PATCH ${key}`);
    rewrite(rows, key, {
      ...(input.parent === undefined ? {} : { parent: input.parent }),
      ...(typeof input.status === "string" ? { status: input.status } : {}),
    });
    return savedIssue(key);
  });
  const create = spyOn(api, "createIssue").mockImplementation(async (input) => {
    sent.push(["create", "CORE-10", input]);
    refuse("create CORE-10");
    const title = input.title ?? "";
    rows.push(issue("CORE-10", { labels: ["epic"], status: "triage", title }));
    return epicIssue("CORE-10", title, "triage");
  });
  restores.push(() => create.mockRestore());
  const messages = spyOn(api, "listIssueMessages").mockImplementation(
    async (key) =>
      sent.flatMap(([route, at, body]) =>
        route === "message" && at === key ? [{ message: body }] : []
      ) as never
  );
  restores.push(() => messages.mockRestore());
  return sent;
}

/** The label of each write that stands refused in Pending changes. */
function refusedWrites(client: QueryClient): string[] {
  return writesOf(client)
    .rows()
    .flatMap((write) => (write.status === "refused" ? [write.label] : []));
}

test("Finish of a merge whose refused move waits on a target since out of work: a Cancel writes nothing and keeps the refused move, and a yes moves the target into work, then the issue, then posts the reason and closes the source", async () => {
  const user = userEvent.setup();
  const rows = mergeRows("backlog");
  const sent = answerOnRows(rows, ["PATCH CORE-3"]);
  const { client } = table(rows, "?group=epic");
  await mergeInto(user, "CORE-1", "CORE-2");
  const asked = await screen.findByRole("dialog", { name: "Move CORE-2 into work too?" });
  await user.click(within(asked).getByRole("button", { name: "Move into work too" }));
  await screen.findByRole("alert");
  // Another writer takes the target out of work again.
  rewrite(rows, "CORE-2", { status: "backlog" });
  const before = sent.length;
  await pressFinish(user);
  const cancelled = await screen.findByRole("dialog", { name: "Move CORE-2 into work too?" });
  await user.click(within(cancelled).getByRole("button", { name: "Cancel" }));
  expect(await screen.findByRole("region", { name: "Interrupted table action" })).toBeDefined();
  expect(sent.slice(before)).toEqual([["status-change", "CORE-2", intoWorkDryRun]]);
  expect(refusedWrites(client)).toEqual(["Update CORE-3 from the issues table"]);
  await pressFinish(user);
  const dialog = await screen.findByRole("dialog", { name: "Move CORE-2 into work too?" });
  await user.click(within(dialog).getByRole("button", { name: "Move into work too" }));
  await noticeOnceDone("Finished the interrupted action.");
  expect(sent.slice(before)).toEqual([
    ["status-change", "CORE-2", intoWorkDryRun],
    ["status-change", "CORE-2", intoWorkDryRun],
    ["status-change", "CORE-2", intoWork],
    ["PATCH", "CORE-3", { parent: "CORE-2" }],
    ["message", "CORE-1", { body: "Merged into CORE-2 (“Target”) from the issues table." }],
    ["PATCH", "CORE-1", { status: "done" }],
  ]);
  expect(refusedWrites(client)).toEqual([]);
});

test("Finish of a merge whose move of its target into work was refused asks again, and a yes makes one move into work in place of the refused one", async () => {
  const user = userEvent.setup();
  const rows = mergeRows("backlog");
  const sent = answerOnRows(rows, ["status-change CORE-2"]);
  const { client } = table(rows, "?group=epic");
  await mergeInto(user, "CORE-1", "CORE-2");
  const asked = await screen.findByRole("dialog", { name: "Move CORE-2 into work too?" });
  await user.click(within(asked).getByRole("button", { name: "Move into work too" }));
  await screen.findByRole("alert");
  expect(refusedWrites(client)).toEqual(["Move CORE-2 into work from the issues table"]);
  const before = sent.length;
  await pressFinish(user);
  const dialog = await screen.findByRole("dialog", { name: "Move CORE-2 into work too?" });
  expect(sent.slice(before)).toEqual([["status-change", "CORE-2", intoWorkDryRun]]);
  await user.click(within(dialog).getByRole("button", { name: "Move into work too" }));
  await noticeOnceDone("Finished the interrupted action.");
  expect(sent.slice(before)).toEqual([
    ["status-change", "CORE-2", intoWorkDryRun],
    ["status-change", "CORE-2", intoWork],
    ["PATCH", "CORE-3", { parent: "CORE-2" }],
    ["message", "CORE-1", { body: "Merged into CORE-2 (“Target”) from the issues table." }],
    ["PATCH", "CORE-1", { status: "done" }],
  ]);
  // The refused move into work is gone from Pending changes: the one Finish made took its place.
  expect(refusedWrites(client)).toEqual([]);
});

test("Finish of a merge whose close of its source was refused closes the source only once an issue added under it since has moved", async () => {
  const user = userEvent.setup();
  const rows = mergeRows("in_progress");
  const sent = answerOnRows(rows, ["PATCH CORE-1"]);
  const { client } = table(rows, "?group=epic");
  await mergeInto(user, "CORE-1", "CORE-2");
  await user.click(
    within(await screen.findByRole("dialog", { name: "Merge this epic?" })).getByRole("button", {
      name: "Merge",
    })
  );
  await screen.findByRole("alert");
  expect(refusedWrites(client)).toEqual(["Update CORE-1 from the issues table"]);
  // Another writer puts a new issue under the source.
  rows.push(issue("CORE-4", { parent: "CORE-1", status: "in_progress" }));
  const before = sent.length;
  await pressFinish(user);
  await noticeOnceDone("Finished the interrupted action.");
  // The reason was posted before the close was refused, so it is not posted again.
  expect(sent.slice(before)).toEqual([
    ["PATCH", "CORE-4", { parent: "CORE-2" }],
    ["PATCH", "CORE-1", { status: "done" }],
  ]);
  expect(refusedWrites(client)).toEqual([]);
});

test("Finish of a merge whose refused move waits on a target closed since refuses, and writes nothing", async () => {
  const user = userEvent.setup();
  const rows = mergeRows("in_progress");
  const sent = answerOnRows(rows, ["PATCH CORE-3"]);
  const { client } = table(rows, "?group=epic");
  await mergeInto(user, "CORE-1", "CORE-2");
  await user.click(
    within(await screen.findByRole("dialog", { name: "Merge this epic?" })).getByRole("button", {
      name: "Merge",
    })
  );
  await screen.findByRole("alert");
  rewrite(rows, "CORE-2", { status: "done" });
  const before = sent.length;
  await pressFinish(user);
  await waitFor(() =>
    expect(screen.getByRole("alert").textContent).toBe(
      "The target epic CORE-2 is unavailable or closed. The source epic CORE-1 remains open, and its issues have not moved to it."
    )
  );
  expect(screen.queryByRole("dialog")).toBeNull();
  expect(sent.slice(before)).toEqual([]);
  expect(refusedWrites(client)).toEqual(["Update CORE-3 from the issues table"]);
  expect(screen.getByRole("region", { name: "Interrupted table action" })).toBeDefined();
});

// A merge whose move of its target into work was refused leaves that move in Pending changes. The
// source then closes by another writer, so Finish has no merge left to finish, and it drops the
// refused move there: a Retry of it would move the target into work with no merge to need it.
test("Finish of a merge whose source closed since drops its refused move of the target into work from Pending changes", async () => {
  const user = userEvent.setup();
  const rows = mergeRows("backlog");
  const sent = answerOnRows(rows, ["status-change CORE-2"]);
  // Every issue: the source stays a row of the listing once it closes.
  const { client } = table(rows, "?group=epic&nofilter=1");
  await mergeInto(user, "CORE-1", "CORE-2");
  const asked = await screen.findByRole("dialog", { name: "Move CORE-2 into work too?" });
  await user.click(within(asked).getByRole("button", { name: "Move into work too" }));
  await screen.findByRole("alert");
  expect(refusedWrites(client)).toEqual(["Move CORE-2 into work from the issues table"]);
  // Another writer closes the source.
  rewrite(rows, "CORE-1", { status: "done" });
  const before = sent.length;
  await pressFinish(user);
  await noticeOnceDone("Finished the interrupted action.");
  expect(sent.slice(before)).toEqual([]);
  expect(refusedWrites(client)).toEqual([]);
});

/** The dry run of a merge's close of its source CORE-1. */
const sourceDoneDryRun = { dry_run: true, status: "done" };

// A child of a merge's source can be in another project, which this table does not list, so the
// merge does not move it. Before the close, the listing read again still counts it under the
// source, and Dispatch's dry run of the close names it: Dispatch answers that dry run as the rule
// is on, whatever its setting. The merge names the child with its project, and posts no reason and
// closes nothing: with the rule off the close would land over the open child, and with the rule on
// it would be refused after the reason, so the absence of `PATCH CORE-1` in `sent` covers both
// settings. Finish does the same until the child has moved out, then posts the reason and closes
// the source.
test("a merge whose source holds an open child in another project names the child with its project and closes nothing, whatever the rule's setting, on its first run and on Finish, until the child has moved", async () => {
  const user = userEvent.setup();
  const rows = mergeRows("in_progress");
  // CORE-1 has two children: CORE-3, which the table lists, and ACME-5, which it does not. Each
  // refusal of CORE-1's close is the recorded one (`conflict`), which names ACME-5, of the project
  // ACME, as an open child.
  rewrite(rows, "CORE-1", { progress: { tasks: null, children: { done: 0, total: 2 } } });
  const sent = answerOnRows(rows);
  answerStatusChanges().mockImplementation(async (key, input) => {
    sent.push(["status-change", key, input]);
    throw fixtureRefusal("conflict");
  });
  const said =
    "The source epic CORE-1 remains open: ACME-5 “Child in work” (project ACME) still needs to move.";
  table(rows, "?group=epic");
  await mergeInto(user, "CORE-1", "CORE-2");
  await user.click(
    within(await screen.findByRole("dialog", { name: "Merge this epic?" })).getByRole("button", {
      name: "Merge",
    })
  );
  expect((await screen.findByRole("alert")).textContent).toBe(said);
  expect(sent).toEqual([
    ["PATCH", "CORE-3", { parent: "CORE-2" }],
    ["status-change", "CORE-1", sourceDoneDryRun],
  ]);
  expect(screen.getByRole("region", { name: "Interrupted table action" })).toBeDefined();

  let before = sent.length;
  await pressFinish(user);
  await waitFor(() =>
    expect(sent.slice(before)).toEqual([["status-change", "CORE-1", sourceDoneDryRun]])
  );
  await waitFor(() => expect(screen.getByRole("alert").textContent).toBe(said));
  expect(await screen.findByRole("region", { name: "Interrupted table action" })).toBeDefined();
  expect(sent.slice(before)).toEqual([["status-change", "CORE-1", sourceDoneDryRun]]);

  // The reader moves ACME-5 out of CORE-1 in its own project.
  rewrite(rows, "CORE-1", { progress: { tasks: null, children: { done: 0, total: 0 } } });
  before = sent.length;
  await pressFinish(user);
  await noticeOnceDone("Finished the interrupted action.");
  expect(sent.slice(before)).toEqual([
    ["message", "CORE-1", { body: "Merged into CORE-2 (“Target”) from the issues table." }],
    ["PATCH", "CORE-1", { status: "done" }],
  ]);
});

test("Finish of a New epic whose refused adoption waits on an epic since out of work: a Cancel writes nothing, and a yes moves the epic into work, then adopts the issue", async () => {
  const user = userEvent.setup();
  const rows = [issue("CORE-1", { status: "in_progress" })];
  const sent = answerOnRows(rows, ["PATCH CORE-1"]);
  const { client } = table(rows);
  await newEpicOf(user, "CORE-1", "Platform");
  const asked = await screen.findByRole("dialog", { name: "Move “Platform” into work too?" });
  await user.click(within(asked).getByRole("button", { name: "Move into work too" }));
  await screen.findByRole("alert");
  expect(sent).toEqual([
    ["create", "CORE-10", { labels: ["epic"], project: "CORE", title: "Platform" }],
    ["status-change", "CORE-10", intoWork],
    ["PATCH", "CORE-1", { parent: "CORE-10" }],
  ]);
  // Another writer takes the new epic out of work.
  rewrite(rows, "CORE-10", { status: "backlog" });
  const before = sent.length;
  await pressFinish(user);
  const cancelled = await screen.findByRole("dialog", { name: "Move CORE-10 into work too?" });
  await user.click(within(cancelled).getByRole("button", { name: "Cancel" }));
  expect(await screen.findByRole("region", { name: "Interrupted table action" })).toBeDefined();
  expect(sent.slice(before)).toEqual([["status-change", "CORE-10", intoWorkDryRun]]);
  expect(refusedWrites(client)).toEqual(["Update CORE-1 from the issues table"]);
  await pressFinish(user);
  const dialog = await screen.findByRole("dialog", { name: "Move CORE-10 into work too?" });
  await user.click(within(dialog).getByRole("button", { name: "Move into work too" }));
  await noticeOnceDone("Finished the interrupted action.");
  expect(sent.slice(before)).toEqual([
    ["status-change", "CORE-10", intoWorkDryRun],
    ["status-change", "CORE-10", intoWorkDryRun],
    ["status-change", "CORE-10", intoWork],
    ["PATCH", "CORE-1", { parent: "CORE-10" }],
  ]);
  expect(refusedWrites(client)).toEqual([]);
});

test("Finish of a New epic whose create was refused asks the first run's question before it sends the create again: a Cancel writes nothing, and a yes makes the epic, moves it into work, then adopts", async () => {
  const user = userEvent.setup();
  const rows = [issue("CORE-1", { status: "in_progress" })];
  const sent = answerOnRows(rows, ["create CORE-10"]);
  const { client } = table(rows);
  await newEpicOf(user, "CORE-1", "Platform");
  const asked = await screen.findByRole("dialog", { name: "Move “Platform” into work too?" });
  await user.click(within(asked).getByRole("button", { name: "Move into work too" }));
  await screen.findByRole("alert");
  expect(refusedWrites(client)).toEqual(["Create epic “Platform”"]);
  const before = sent.length;
  await pressFinish(user);
  // The epic is no issue yet, so the question is the first run's, from the listing, with no dry run.
  const cancelled = await screen.findByRole("dialog", { name: "Move “Platform” into work too?" });
  expect(cancelled.textContent).toContain(
    "CORE-1 “Work CORE-1” is in work, and a new epic is not. An epic stays in work while an issue under it is in work. Dispatch makes the epic and then makes this change:"
  );
  await user.click(within(cancelled).getByRole("button", { name: "Cancel" }));
  expect(await screen.findByRole("region", { name: "Interrupted table action" })).toBeDefined();
  expect(sent.slice(before)).toEqual([]);
  expect(refusedWrites(client)).toEqual(["Create epic “Platform”"]);
  await pressFinish(user);
  const dialog = await screen.findByRole("dialog", { name: "Move “Platform” into work too?" });
  expect(sent.slice(before)).toEqual([]);
  await user.click(within(dialog).getByRole("button", { name: "Move into work too" }));
  await noticeOnceDone("Finished the interrupted action.");
  expect(sent.slice(before)).toEqual([
    ["create", "CORE-10", { labels: ["epic"], project: "CORE", title: "Platform" }],
    ["status-change", "CORE-10", intoWork],
    ["PATCH", "CORE-1", { parent: "CORE-10" }],
  ]);
  expect(refusedWrites(client)).toEqual([]);
});

test("Finish of a start next whose rank write was refused sends it again, with no question", async () => {
  const user = userEvent.setup();
  const rows = [
    issue("CORE-2", { labels: ["legion"], rank: "b" }),
    issue("CORE-3", { labels: ["legion"], rank: "a" }),
  ];
  const sent: EpicWrite[] = [];
  let refuse = true;
  answerIssuePatches().mockImplementation(async (key, input) => {
    sent.push(["PATCH", key, input]);
    if (refuse) {
      refuse = false;
      throw new ApiError(400, { code: "REFUSED_ONCE", error: "refused once" });
    }
    // The rank write puts the issue at the head of the queue, where the next read finds it.
    rewrite(rows, key, { rank: "0" });
    return savedIssue(key);
  });
  const { client } = table(rows);
  await user.click(await screen.findByRole("checkbox", { name: "Select CORE-2" }));
  await user.click(
    within(screen.getByRole("toolbar")).getByRole("button", { name: /^Start next$/ })
  );
  await screen.findByRole("alert");
  const before = sent.length;
  await pressFinish(user);
  await noticeOnceDone("Finished the interrupted action.");
  expect(sent.slice(before)).toEqual([["PATCH", "CORE-2", { rank: { before: "CORE-3" } }]]);
  expect(screen.queryByRole("dialog")).toBeNull();
  expect(refusedWrites(client)).toEqual([]);
});

/** Each interrupted action whose epic Finish finds not in work while an issue it still moves is,
 *  with the question Finish asks of it. */
const finishingActions = [
  {
    action: "New epic",
    record: madeEpic,
    rows: () => [
      issue("CORE-1", { status: "in_progress" }),
      issue("CORE-9", { labels: ["epic"], status: "triage", title: "Platform" }),
    ],
    epic: epicIssue("CORE-9", "Platform", "triage"),
    question: "Move CORE-9 into work too?",
  },
  {
    action: "merge",
    record: mergeBegun,
    rows: () => mergeRows("backlog"),
    epic: epicIssue("CORE-2", "Target", "backlog"),
    question: "Move CORE-2 into work too?",
  },
];

test.each(finishingActions)(
  "Finish of a $action whose epic is not in work, its question cancelled, writes nothing and keeps the action",
  async ({ epic, question, record, rows }) => {
    const user = userEvent.setup();
    const sent = answerEpicWrites([epic]);
    interruptedBy(record);
    table(rows());
    await pressFinish(user);
    const dialog = await screen.findByRole("dialog", { name: question });
    await user.click(within(dialog).getByRole("button", { name: "Cancel" }));
    expect(screen.queryByRole("dialog")).toBeNull();
    // The action stays to be finished, and the dry run is the one request Finish sent.
    expect(await screen.findByRole("region", { name: "Interrupted table action" })).toBeDefined();
    expect(sent).toEqual([["status-change", epic.key, intoWorkDryRun]]);
    expect(
      within(screen.getByRole("region", { name: "Project issues" })).queryByRole("status")
    ).toBeNull();
  }
);

// The table going away while Finish's question is open ends the Finish, as a Cancel does: nothing
// more is written, and nothing is left waiting on a dialog nobody can see.
test("Finish's question, open when the table goes away, ends the Finish and writes nothing", async () => {
  const user = userEvent.setup();
  const sent = answerEpicWrites([epicIssue("CORE-2", "Target", "backlog")]);
  interruptedBy(mergeBegun);
  const { client, view } = table(mergeRows("backlog"));
  await pressFinish(user);
  await screen.findByRole("dialog", { name: "Move CORE-2 into work too?" });
  const invalidated = spyOn(client, "invalidateQueries");
  restores.push(() => invalidated.mockRestore());
  view.unmount();
  // Finish has ended: its last step reads the issues again.
  await waitFor(() => expect(invalidated).toHaveBeenCalledWith({ queryKey: ["issues"] }));
  expect(sent).toEqual([["status-change", "CORE-2", intoWorkDryRun]]);
  expect(JSON.parse(localStorage.getItem("dispatch.project-table.CORE") ?? "{}").action?.id).toBe(
    "merging"
  );
});

/** The registry's columns a project's table opens with, by the names the Columns control gives
 *  them: every one but Updated. With the row's checkbox, which no reader hides, that is 13. */
const REGISTRY_OPENS = [
  "Priority",
  "Queue",
  "Key",
  "Title",
  "Status",
  "Epic",
  "Assignee",
  "Created by",
  "Labels",
  "Claimed by",
  "Open asks",
  "Created",
];

/** Whether the Columns control, once open, ticks the column it names `name`. */
const ticked = (name: string) =>
  (screen.getByRole("checkbox", { name }) as HTMLInputElement).checked;

// A fault list of the priorities page opens with R6's columns alone (the backlog-or-icebox list's
// set here: the issue, its project, its status, who holds it, how long it has stood, and the move),
// so it fits a desktop window. Every other column is one tick away under Columns, and no table
// offers a Reason column until a reason is recorded (dispatch://INFRA-2934).
test("a table given the columns it opens with shows those alone, with the row's checkbox, and offers every other one unticked", async () => {
  const user = userEvent.setup();
  table(
    [issue("CORE-1", { advances: [{ id: "ship-it", inherited: false }], status: "backlog" })],
    "",
    undefined,
    {
      layout: {
        key: "backlog",
        opens: ["key", "title", "status", "claim", "updated", "project", "moved"],
      },
      listing: { advances: "ship-it", moved: true },
      priorityTitles: new Map([["ship-it", "Ship it everywhere"]]),
      stateKey: "k",
    }
  );
  const list = await screen.findByRole("table", { name: "Priority issues table" });
  const row = await within(list).findByRole("row", { name: "CORE-1 Work CORE-1" });
  // `x` selects with the row's checkbox, which no reader can hide or show, so it stays.
  expect(within(row).getByRole("checkbox", { name: "Select CORE-1" })).toBeDefined();
  await user.click(screen.getByRole("button", { name: "Columns" }));
  const opened = ["Key", "Title", "Status", "Claimed by", "Updated", "Project", "Moved there"];
  for (const name of opened)
    expect({ name, ticked: ticked(name) }).toEqual({
      name,
      ticked: true,
    });
  const others = REGISTRY_OPENS.filter((name) => !opened.includes(name));
  for (const name of [...others, "Advances"])
    expect({ name, ticked: ticked(name) }).toEqual({
      name,
      ticked: false,
    });
  expect(screen.queryByRole("checkbox", { name: "Reason" })).toBeNull();
  // Ticked, a column the layout opened hidden is drawn.
  await user.click(screen.getByRole("checkbox", { name: "Advances" }));
  await waitFor(() =>
    expect(
      within(list)
        .getAllByRole("columnheader")
        .some((header) => header.textContent?.startsWith("Advances"))
    ).toBe(true)
  );
});

test("a project's table, given no layout, opens with every registry column but Updated", async () => {
  const user = userEvent.setup();
  table([issue("CORE-1")]);
  const list = await screen.findByRole("table", { name: "Project issues table" });
  const row = await within(list).findByRole("row", { name: "CORE-1 Work CORE-1" });
  expect(within(row).getByRole("checkbox", { name: "Select CORE-1" })).toBeDefined();
  await user.click(screen.getByRole("button", { name: "Columns" }));
  for (const name of REGISTRY_OPENS)
    expect({ name, ticked: ticked(name) }).toEqual({
      name,
      ticked: true,
    });
  expect(ticked("Updated")).toBe(false);
});

test("the widths a layout names reach the layout it builds, over the registry's and the listing columns'", () => {
  const built = issueTableLayout(["project", "moved"], ["key", "title", "moved"], {
    claim: { min: 112, size: 112 },
    moved: { min: 104, size: 120 },
  });
  expect([built.sizes?.claim, built.minimums?.claim]).toEqual([112, 112]);
  expect([built.sizes?.moved, built.minimums?.moved]).toEqual([120, 104]);
  // A column it names no width for keeps its own.
  expect([built.sizes?.status, built.minimums?.status]).toEqual([
    ISSUE_TABLE_COLUMNS.sizes?.status,
    ISSUE_TABLE_COLUMNS.minimums?.status,
  ]);
  expect([built.sizes?.project, built.minimums?.project]).toEqual([120, 96]);
});

test("a project's table keeps the registry's opening set, sizes and minimums", () => {
  const built = issueTableLayout([]);
  expect(built.ids).toEqual(ISSUE_TABLE_COLUMNS.ids);
  expect(built.hiddenByDefault).toEqual(ISSUE_TABLE_COLUMNS.hiddenByDefault);
  expect(built.sizes).toEqual(ISSUE_TABLE_COLUMNS.sizes);
  expect(built.minimums).toEqual(ISSUE_TABLE_COLUMNS.minimums);
  // Its Title fills nothing: in the 1000 px box a table starts with, it gives up only what keeps
  // the pinned columns (select 44, priority 140, key 108, title 380) within 60% of the box.
  expect(built.fills).toBeUndefined();
  const { result, unmount } = renderHook(() => useTableLayout("fills-none", built));
  expect(result.current.boxWidth).toBe(1000);
  expect(result.current.responsive.sizing.title).toBe(380 - (672 - 600));
  unmount();
});

// A table given a layout - a fault list of the priorities page - fits a desktop box: its Title
// takes the width the other shown columns and the row's actions (104 px) leave inside the box's
// borders and beside its scrollbar, never below its 116 px minimum. One sizing path: the layout
// declares it (`fills`) and `useTableLayout` applies it.
test("a layout's Title fills a desktop box with what the other shown columns and the row's actions leave", () => {
  const narrow = issueTableLayout(["project"], ["key", "title", "status"]);
  expect([narrow.fills, narrow.trailing]).toEqual(["title", 104]);
  const wide = renderHook(() => useTableLayout("fills-narrow", narrow));
  expect(wide.result.current.boxWidth).toBe(1000);
  // Select 44, key 108 and status 132 shown, and the row's actions, in a box not yet measured,
  // whose borders read as nothing: 1000 - 388.
  expect(wide.result.current.responsive.sizing.title).toBe(612);
  wide.unmount();
  // With more shown than leave Title its minimum, it stays at that minimum.
  const full = renderHook(() =>
    useTableLayout(
      "fills-full",
      issueTableLayout(
        ["project"],
        ["key", "title", "status", "assignee", "claim", "updated", "project"]
      )
    )
  );
  expect(full.result.current.responsive.sizing.title).toBe(116);
  full.unmount();
});

/** A layout that opens the columns a priority's work table opens, at set widths, where that
 *  table fits them to what they hold, which the unit DOM lays out at no width: in a box 606 px
 *  wide, the box that page leaves its work table beside the margin at 1280 px, it draws no row's
 *  actions (`rowActions`), so select 44, key 96, status 150 and project 72 leave Title 242 of the
 *  604 px inside the box's two borders, past its 116 px minimum. */
const FITS_NARROW = {
  key: "fits-narrow",
  opens: ["key", "title", "status", "project"],
  rowActions: false,
  widths: {
    key: { min: 96, size: 96 },
    project: { min: 72, size: 72 },
    status: { min: 150, size: 150 },
  },
} as const;

/** Each header cell of a table of `FITS_NARROW` drawn in a box `width` pixels wide, left to
 *  right, as `<its column's words> <width>`, and whether Title's header stacks its name over its
 *  filter. */
async function narrowHeaders(width: number): Promise<{ cells: string[]; stacked: boolean }> {
  stubScrollBoxWidth(width);
  table([issue("CORE-1", { advances: [{ id: "p", inherited: false }] })], "", undefined, {
    layout: FITS_NARROW,
    listing: { advances: "p" },
  });
  await screen.findByRole("row", { name: "CORE-1 Work CORE-1" });
  const head = screen.getByRole("table", { name: "Priority issues table" }).querySelector("thead");
  const cells = [...(head?.querySelectorAll("th") ?? [])];
  const title = cells.find((cell) => cell.textContent?.startsWith("Title"));
  return {
    cells: cells.map(
      (cell) =>
        `${(cell.textContent ?? "").replace(/[▲▼]|Filter$/g, "") || "-"} ${cell.style.width}`
    ),
    stacked: title?.firstElementChild?.classList.contains("flex-col") ?? true,
  };
}

// A box narrower than 640 px is a desktop's where a layout that fills it fits it with Title at
// its minimum: every column keeps its desktop width, Title takes what they leave, and its header
// reads its name and its filter on one line, as every header does. A phone's box, which those
// columns cannot fit, keeps the phone's widths, and there Title's header stacks.
test("a filling layout that fits a 606 px box lays it out as a desktop's", async () => {
  expect(await narrowHeaders(606)).toEqual({
    cells: ["- 44px", "Key 96px", "Title 242px", "Project 72px", "Status 150px"],
    stacked: false,
  });
});

// The 20 px a narrow box keeps for its borders and a scrollbar is no gap in the table's lines:
// Title takes the room inside the box, so the table is exactly as wide as the box's own inner
// width and its header and rows reach the border, with two borders (604 px of 606) or with a
// classic 17 px scrollbar beside them too (587 px), which no sideways scrollbar then follows.
test("a filling layout's table is as wide as the room inside its box, beside a scrollbar too", async () => {
  for (const [edge, title] of [
    [2, 242],
    [19, 225],
  ] as const) {
    stubScrollBoxWidth(606, edge);
    table([issue("CORE-1", { advances: [{ id: "p", inherited: false }] })], "", undefined, {
      layout: FITS_NARROW,
      listing: { advances: "p" },
    });
    const list = await screen.findByRole("table", { name: "Priority issues table" });
    await within(list).findByRole("row", { name: "CORE-1 Work CORE-1" });
    expect({
      table: list.style.width,
      title: within(list).getByRole("columnheader", { name: /^Title/ }).style.width,
    }).toEqual({ table: `${606 - edge}px`, title: `${title}px` });
    for (const restore of restores.splice(0)) restore();
    localStorage.clear();
  }
});

// The registry's pins are select, Priority, key and Title within 60% of a desktop's box, and
// count the Priority column a work table opens hidden: in a 606 px box that leaves Title no room,
// so the divider falls after the key. A layout that pins the checkbox, the key and Title keeps
// Title pinned there, its divider after Title, as each fault list's is at 1280 px.
test("a layout's pins keep Title pinned in a 606 px box, where the registry's would unpin it", async () => {
  for (const [pins, last] of [
    [undefined, "Key"],
    [["select", "key", "title"], "Title"],
  ] as const) {
    stubScrollBoxWidth(606);
    table([issue("CORE-1", { advances: [{ id: "p", inherited: false }] })], "", undefined, {
      layout: { ...FITS_NARROW, key: `pins-${last}`, ...(pins === undefined ? {} : { pins }) },
      listing: { advances: "p" },
    });
    const list = await screen.findByRole("table", { name: "Priority issues table" });
    await within(list).findByRole("row", { name: "CORE-1 Work CORE-1" });
    const pinned = [...list.querySelectorAll<HTMLElement>("thead th")]
      .filter((cell) => cell.style.left !== "")
      .map((cell) => (cell.textContent ?? "").replace(/[▲▼]|Filter$/g, "") || "-");
    expect(pinned.at(-1)).toBe(last);
    for (const restore of restores.splice(0)) restore();
    localStorage.clear();
  }
});

test("the same layout in a 390 px phone's box keeps the phone's widths and stacks Title's header", async () => {
  expect(await narrowHeaders(390)).toEqual({
    cells: ["- 44px", "Key 88px", "Title 199.5px", "Project 72px", "Status 150px"],
    stacked: true,
  });
});

// `rowActions: false` leaves the row's actions out of a table whose listing holds rows they act
// on: the queue's second row, which a table with them offers Start next on, draws no cell after
// its columns, nor does the header, and the table is its columns' width alone. Title takes the
// 104 px the actions take where they are drawn (`ROW_ACTIONS_WIDTH`): 138 + 104 in the 604 px
// inside a 606 px box's borders.
test("a layout that leaves out the row's actions draws no cell for them and gives their width to Title", async () => {
  stubScrollBoxWidth(606);
  const queued = { advances: [{ id: "p", inherited: false }], labels: ["legion"] };
  table([issue("CORE-1", queued), issue("CORE-2", queued)], "", undefined, {
    layout: FITS_NARROW,
    listing: { advances: "p" },
  });
  const list = await screen.findByRole("table", { name: "Priority issues table" });
  const second = await within(list).findByRole("row", { name: "CORE-2 Work CORE-2" });
  const headers = [...list.querySelectorAll("thead th")];
  expect(headers.map((cell) => cell.textContent?.replace(/[▲▼]|Filter$/g, "") || "-")).toEqual([
    "-",
    "Key",
    "Title",
    "Project",
    "Status",
  ]);
  expect(second.querySelectorAll("td").length).toBe(headers.length);
  expect(within(list).queryByRole("button", { name: "Start next" })).toBeNull();
  expect(within(list).getByRole("columnheader", { name: /^Title/ }).style.width).toBe("242px");
  expect(list.style.width).toBe("604px");
});
