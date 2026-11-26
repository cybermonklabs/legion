import { afterEach, beforeAll, expect, jest, setSystemTime, spyOn, test } from "bun:test";
import * as dndCore from "@dnd-kit/core";
import { onlineManager, QueryClient, QueryClientProvider } from "@tanstack/react-query";
import {
  act,
  // biome-ignore lint/style/noRestrictedImports: a column's drag and drop, which user-event has no form for.
  fireEvent,
  render,
  renderHook,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";
import { type ComponentProps, type ReactNode, useRef } from "react";
import { MemoryRouter, useLocation, useNavigate } from "react-router-dom";
import { drawnSize } from "../../__tests__/control-size";
import { realClock, tick } from "../../__tests__/fake-clock";
import { afterFrameAndTasks } from "../../__tests__/frames";
import { drawOver, layRowsOut, watchMoves } from "../../__tests__/laid-out-rows";
import { openList, optionNames, pickNamed } from "../../__tests__/list-picks";
import { answerBoard, liveSockets, waitMs } from "../../__tests__/live-socket-harness";
import { endWindowScroll, installResizeObserver } from "../../__tests__/scroll-box";
import { answerIssuePatches } from "../../__tests__/send-answers";
import { userOnFakeClock } from "../../__tests__/user";
import { ApiError, api, REQUEST_DEADLINE_MS } from "../../api/client";
import { boardMoveMutationKey, legionBoardQuery, projectIssuesQuery } from "../../api/queries";
import { DispatchQueryClient, queryRetry } from "../../api/query-client";
import type {
  Agent,
  EvalItemRow,
  Issue,
  IssueSummary,
  LegionBoardCard,
  LegionBoardColumn,
  LegionBoardFacts,
  LegionBoardResponse,
  LegionSetting,
} from "../../api/types";
import { SORT_MARK_WIDTH, useTableLayout } from "../../components/table-layout";
import { readLayout } from "../../components/table-storage";
import { useFootInset } from "../../hooks/useFootInset";
import * as routeReach from "../issue/RouteReach";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { readSelectedIssues } from "../search/bar-scope";
import { KeymapProvider } from "../shell/KeymapProvider";
import { appKeymap, useKeymapScope } from "../shell/keymap";
import { SendStoppedError, stoppedWriteNote } from "../writes/keyed-write";
import { writesOf } from "../writes/write-store";
import { IssuesTable } from "./IssuesTable";
import { LEGION_PHASES } from "./issues-legion";
import { issueTableLayout } from "./issues-table-columns";
import { type AdvancesListing, CLOSE_ITEMS_AGAIN_MS } from "./issues-table-data";
import { ALL_ROWS, ISSUE_TABLE_COLUMNS, tableStorageKey } from "./issues-table-model";
import { ListingTable } from "./ListingTable";

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
/** Steps the router's history back one entry, as the browser's Back does: `Route` sets it. */
let back: () => void = () => {};
/** Opens CORE's Issues tab at `search` in the same page, as a link inside the app does, with no
 *  reload and no new read the address does not ask for: `Route` sets it. */
let go: (search: string) => void = () => {};
function Route(): ReactNode {
  useKeymapScope("project");
  const location = useLocation();
  const navigate = useNavigate();
  back = () => navigate(-1);
  go = (search) => navigate(`/projects/CORE/issues${search}`);
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
/** Draws the List over `issues`. `full` answers the listing of the closed issues, and `open` the
 *  one that holds the open ones alone, in place of `issues` itself; `agents` answers the agent
 *  registry, an empty one unless the case holds or fills it; `inMain` draws the List in the
 *  shell's main content, after a link that stands for the shell's navigation, as the app draws
 *  it; `client` is the query client the List reads through, a plain one unless the case needs
 *  the app's own; `sortApplied` says whether the server applies a page read's `sort` and names
 *  it in the answer, which a server older than `sort` does not; `retries` makes that plain
 *  client read a failed read again as the app's shared policy does (`queryRetry`), with no wait
 *  between the reads. With no `props` the List is CORE's Issues tab, as the project page draws
 *  it; `props` draws the table a priority page draws instead (`ListingTable`): its listing, the
 *  priorities' titles, the layout it keeps and the prefix its view is written under. `daemon` is
 *  the project's Legion daemon setting, none by default. */
function table(
  issues: IssueSummary[],
  search = "",
  {
    agents,
    retries = false,
    client = new QueryClient({
      defaultOptions: {
        queries: retries ? { retry: queryRetry, retryDelay: 0 } : { retry: false },
      },
    }),
    full,
    open,
    inMain = false,
    sortApplied = () => true,
  }: {
    agents?: Promise<Agent[]>;
    client?: QueryClient;
    full?: Promise<IssueSummary[]>;
    open?: Promise<IssueSummary[]>;
    inMain?: boolean;
    retries?: boolean;
    sortApplied?: () => boolean;
  } = {},
  props:
    | {
        layout?: ComponentProps<typeof ListingTable>["layout"];
        listing: AdvancesListing;
        priorityTitles?: ReadonlyMap<string, string>;
        stateKey: string;
      }
    | { listing?: undefined } = {},
  daemon: Promise<LegionSetting | null> = Promise.resolve(null)
) {
  // The listing route as the server answers it: `open` leaves the closed rows out, `status`
  // keeps the rows in those statuses, and `advances` keeps the issues that carry that relation,
  // whichever project they are in.
  const answers =
    (options: Parameters<typeof api.listIssues>[0] = {}) =>
    (issue: IssueSummary) =>
      (!options.open || issue.status !== "done") &&
      (options.status === undefined || options.status.includes(issue.status)) &&
      (options.advances === undefined ||
        (issue.advances ?? []).some((relation) => options.advances?.includes(relation.id)));
  const listing = spyOn(api, "listIssues").mockImplementation(async (options = {}) => {
    if (!options.open && full) return (await full).filter(answers(options));
    if (options.open && open) return open;
    return issues.filter(answers(options));
  });
  // One page of it, as `limit` and `offset` ask, newest close first for `sort=-closed_at`, with
  // the sort it applied named in the answer.
  const page = spyOn(api, "listIssuePage").mockImplementation(async (options) => {
    const rows = issues.filter(answers(options));
    const sorted = options.sort !== undefined && sortApplied();
    if (sorted && options.sort === "-closed_at")
      rows.sort((a, b) => (b.closed_at ?? "").localeCompare(a.closed_at ?? ""));
    const offset = options.offset ?? 0;
    const limit = options.limit ?? rows.length;
    return {
      issues: rows.slice(offset, offset + limit),
      limit,
      offset,
      total: rows.length,
      ...(sorted ? { sort: options.sort } : {}),
    };
  });
  const setting = spyOn(api, "getLegionDaemon").mockImplementation(() => daemon);
  const state = spyOn(api, "getMyState").mockResolvedValue({});
  const registry = spyOn(api, "listAgents").mockImplementation(async () => agents ?? []);
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
  client.setQueryData(["whoami"], { event_head: "0", login: "alice", kind: "user" });
  const list =
    props.listing === undefined ? <IssuesTable project="CORE" /> : <ListingTable {...props} />;
  const view = render(
    <MemoryRouter initialEntries={[`/projects/CORE/issues${search}`]}>
      <QueryClientProvider client={client}>
        <KeymapProvider>
          <Route />
          {inMain ? (
            <>
              <a href="/">Inbox</a>
              <main>{list}</main>
            </>
          ) : (
            list
          )}
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  restores.push(() => {
    view.unmount();
    client.clear();
    listing.mockRestore();
    page.mockRestore();
    setting.mockRestore();
    state.mockRestore();
    registry.mockRestore();
    viewer.mockRestore();
    if (descriptor) Object.defineProperty(HTMLElement.prototype, "clientHeight", descriptor);
    else Reflect.deleteProperty(HTMLElement.prototype, "clientHeight");
  });
  return { client, listing, page, registry, setting, view };
}

/** Picks a grouping in the row's display menu, which is where **Group by** lives and which the
 *  Issues tab names Group by: the menu is opened if it is shut, and a choice leaves it open for
 *  the next one. */
async function groupRowsBy(user: UserEvent, option: string) {
  const menu = screen.getByRole("button", { name: "Group by" });
  if (menu.getAttribute("aria-expanded") === "false") await user.click(menu);
  await user.click(screen.getByRole("radio", { name: option }));
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
  expect(within(row).getByRole("button", { name: pickNamed("Priority of CORE-1") })).toBeDefined();
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
  const reading = client.fetchQuery(projectIssuesQuery("CORE", true));
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

// Every panel the table hangs off a control - a header's filter, Columns - and the shared
// MultiSelect dismiss by one set of rules: a press outside, or Escape, which puts focus back on
// the control that opened it.
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

// A refused change is said in Dispatch's own words. `String` of the refusal would lead them with
// its class's name (`ApiError: `), which means nothing to the reader.
test("a refused change's notice says the refusal in Dispatch's words, without the error's name", async () => {
  const user = userEvent.setup();
  const send = spyOn(api, "send").mockRejectedValue(
    new ApiError(403, { error: "Moves are closed for CORE.", code: "FORBIDDEN" })
  );
  restores.push(() => send.mockRestore());
  table([
    issue("CORE-1", { labels: ["legion"], rank: "A" }),
    issue("CORE-2", { labels: ["legion"], rank: "B" }),
  ]);
  await user.click(await screen.findByRole("checkbox", { name: "Select CORE-2" }));
  await user.click(within(screen.getByRole("toolbar")).getByRole("button", { name: "Start next" }));
  expect((await screen.findByRole("alert")).textContent).toBe(
    "Did not move up: CORE-2. 0 of 1 changes saved. Changes not saved: CORE-2. Moves are closed for CORE. Retry each failed change in Pending changes."
  );
});

test("Clear filters also clears Needs you, Unread and the lapsed-claim filter", async () => {
  const user = userEvent.setup();
  table([issue("CORE-1", { open_asks: 2 })], "?needs-you=1&unread=1&unclaimed=1");
  await user.click(await screen.findByRole("button", { name: "Clear filters" }));
  expect(
    screen.getByRole("button", { name: /^Filter Claimed by/ }).getAttribute("aria-label")
  ).toBe("Filter Claimed by");
  // The flags are behind the row's one filter trigger now, so the reader opens it to read them.
  await user.click(screen.getByRole("button", { name: /^Filters · \d+$/ }));
  for (const name of ["Needs you", "Unread", "Unclaimed"]) {
    await waitFor(() =>
      expect(screen.getByRole("button", { name }).getAttribute("aria-pressed")).toBe("false")
    );
  }
});

// R2: each control takes its size from the page, comfortable or compact, as a value the page
// passes. The project List is compact, as its row's controls and its bar's steps are, so the
// chips of its Filters panel and its bar's Start next draw the compact size's room
// (`controlPadding.compact`), rather than the comfortable size or room of their own.
test("R2: the Filters panel's chips and the bar's Start next draw the List's compact size", async () => {
  const user = userEvent.setup();
  table([issue("CORE-1"), issue("CORE-2")]);
  await user.click(await screen.findByRole("checkbox", { name: "Select CORE-1" }));
  const startNext = within(screen.getByRole("toolbar")).getByRole("button", { name: "Start next" });
  await user.click(screen.getByRole("button", { name: /^Filters · \d+$/ }));
  const chip = (name: string) => drawnSize(screen.getByRole("button", { name }));
  expect({
    startNext: drawnSize(startNext),
    needsYou: chip("Needs you"),
    unclaimed: chip("Unclaimed"),
    unread: chip("Unread"),
  }).toEqual({
    startNext: "compact",
    needsYou: "compact",
    unclaimed: "compact",
    unread: "compact",
  });
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

test("a late Done listing applies the requested URL page instead of clamping it early", async () => {
  const done = Promise.withResolvers<IssueSummary[]>();
  table([issue("CORE-1")], "?status=done&page=3&size=25", { full: done.promise });
  // Status Done alone reads no open row, so the table waits for the closed issues.
  await screen.findByRole("status", { name: "Loading issues" });
  done.resolve(
    Array.from({ length: 80 }, (_, index) => issue(`CORE-${index + 2}`, { status: "done" }))
  );
  // Page 3 of the 80 closed issues, 25 rows of them, rather than page 1.
  expect(await screen.findByText("Page 3 of 4")).toBeDefined();
  expect(listedKeys()).toHaveLength(25);
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
  // A sorting function blind to the direction cannot do this: the table reverses its answer for a
  // descending sort, so the column turns its answer for an empty cell round by the direction it
  // is sorted in (`emptyLast`). Two rows without a time keep their own order beside the one that
  // has one.
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
  // Queue is no default column of a project with no Legion daemon: the address shows it.
  table(
    [
      issueFromOlderServer("CORE-1", { labels: ["legion"], rank: "m" }),
      issue("CORE-2", { labels: ["legion"], rank: "m" }),
    ],
    "?cols=key,title,queue"
  );
  const older = await screen.findByRole("row", { name: "CORE-1 Work CORE-1" });
  expect(
    within(screen.getByRole("row", { name: "CORE-2 Work CORE-2" })).getByText("#1")
  ).toBeDefined();
  expect(within(older).getByText("#2")).toBeDefined();
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

/** `count` issues in Legion's queue, CORE-1 first, in rank order: the issues the bar's Start next
 *  moves. */
const queuedIssues = (count: number) =>
  Array.from({ length: count }, (_, index) =>
    issue(`CORE-${index + 1}`, { labels: ["legion"], rank: String(index).padStart(3, "0") })
  );

// Deep in a long List, a press of the reader's takes the two issues they marked off the list,
// so the selection empties and the bar goes while it holds the focus. The row after the issue
// marked last moves up into its place, where the reader can see it, and its checkbox takes the
// focus with no scroll. The first row, nine rows above the window, would scroll the box back to
// its top through `focusRow`. The press is the bar's Start next, by Enter, whose writes
// land and whose read of the list after them no longer lists the two issues. The press arms the
// bar's own hand-off (`armOnPress`), which the action's run holds until its read has settled
// (`perform`), so the bar reads its going as the reader's (`causedByReader`). The same read with
// no press of the reader's is a loss they did not cause, and the focus stays on the page.
test("the List's bar gone with the focus after the issue marked last left the list hands the focus to the row now at its place, with no scroll", async () => {
  const user = userEvent.setup();
  const send = spyOn(api, "send").mockImplementation(
    async (request) => savedIssue(request.path.split("/").at(-1) ?? "") as never
  );
  restores.push(() => send.mockRestore());
  const issues = queuedIssues(20);
  table(issues);
  await screen.findByRole("checkbox", { name: "Select CORE-20" });
  // The rows in the order the table draws them, and the three this case reads.
  const order = [...document.querySelectorAll("[data-issue-row]")].map(
    (row) => row.getAttribute("data-issue-row") ?? ""
  );
  const [marked, last, next] = order.slice(11, 14);
  // Each row's checkbox is drawn where its row stands now, 100 px a row, with the tenth row at
  // the window's top: happy-dom lays nothing out, and the hand-off reads what the reader sees.
  restores.push(layRowsOut("[data-issue-row]", 9));
  await user.click(screen.getByRole("checkbox", { name: `Select ${marked}` }));
  await user.click(screen.getByRole("checkbox", { name: `Select ${last}` }));
  act(() =>
    within(screen.getByRole("toolbar", { name: "Selected issues" }))
      .getByRole("button", { name: "Start next" })
      .focus()
  );
  const moves = watchMoves();
  restores.push(moves.restore);
  // The listing answers from `issues`, so the read after the writes no longer lists the two.
  issues.splice(0, issues.length, ...issues.filter(({ key }) => key !== marked && key !== last));
  await user.keyboard("{Enter}");
  await waitFor(() => expect(screen.queryByRole("toolbar")).not.toBeInTheDocument());
  const landing = screen.getByRole("checkbox", { name: `Select ${next}` });
  await waitFor(() => expect(landing).toHaveFocus());
  expect(moves.focused.at(-1)).toEqual({ options: { preventScroll: true }, target: landing });
  expect(moves.scrolled).toEqual([]);
});

// A select-all from the table's head marks no issue of the reader's own. Deep in a long List,
// Clear selection empties the selection, and the bar goes while it holds the focus. The table's
// sticky head is drawn over the top of the window's first row. The focus goes, with no scroll,
// to the checkbox of the first row the reader can see below the head. The first row, nine rows
// above the window, would scroll the box back to its top through `focusRow`.
test("the List's bar gone with the focus while no issue is marked hands the focus to the first row in sight below the table's head, with no scroll", async () => {
  const user = userEvent.setup();
  // Drawn in the shell's main content, as the app draws it: Clear selection is a press of the
  // reader's, so the bar hands its focus on.
  table(
    Array.from({ length: 20 }, (_, index) => issue(`CORE-${index + 1}`)),
    "",
    { inMain: true }
  );
  await screen.findByRole("checkbox", { name: "Select CORE-20" });
  // The rows in the order the table draws them.
  const order = [...document.querySelectorAll("[data-issue-row]")].map(
    (row) => row.getAttribute("data-issue-row") ?? ""
  );
  // Each row's checkbox is drawn where its row stands now, 100 px a row, with the tenth row at
  // the window's top: happy-dom lays nothing out, and the hand-off reads what the reader sees.
  restores.push(layRowsOut("[data-issue-row]", 9));
  await user.click(screen.getByRole("checkbox", { name: "Select every row on this page" }));
  expect(readSelectedIssues()).toHaveLength(20);
  // The table's head, 60 px tall, over the top of the tenth row, which stands from 0 to 100 px.
  const head = screen.getByRole("table", { name: "Project issues table" }).querySelector("thead");
  if (head === null) throw new Error("The List drew no table head");
  restores.push(drawOver(head, 60, "[data-issue-row]", 9));
  act(() => screen.getByRole("button", { name: "Clear selection" }).focus());
  const moves = watchMoves();
  restores.push(moves.restore);
  await user.keyboard("{Enter}");
  await waitFor(() => expect(screen.queryByRole("toolbar")).not.toBeInTheDocument());
  const landing = screen.getByRole("checkbox", { name: `Select ${order[10]}` });
  await waitFor(() => expect(landing).toHaveFocus());
  expect(moves.focused.at(-1)).toEqual({ options: { preventScroll: true }, target: landing });
  expect(moves.scrolled).toEqual([]);
  // A frame and two tasks on, the focus is still where the bar put it.
  await afterFrameAndTasks();
  expect(landing).toHaveFocus();
});

/** What holds the focus, as a string a failed assertion prints whole: the body, or the control's
 *  label, else its text. */
function focusedName(): string {
  const focused = document.activeElement;
  if (focused === null || focused === document.body) return "the body";
  return focused.getAttribute("aria-label") ?? focused.textContent?.trim() ?? focused.tagName;
}

// Deep in a long List the reader marks two adjacent rows, the lower first, and presses the bar's
// Start next with Enter. The writes land, and the list read after them no longer shows the two
// rows. The bar goes only once that read has settled, so its hand-off reads the view the action
// left: the row after the two has moved up into their place, where the reader can see it, and its
// checkbox takes the focus with no scroll. A bar that went as the writes landed found the row
// marked last still listed and focused its checkbox, and the read then took that row out of the
// view with the focus.
test("Start next from the bar hands the focus to the row now at the marked rows' place, with no scroll and never the body, once the list read takes the two out of the view", async () => {
  const user = userEvent.setup();
  const send = spyOn(api, "send").mockImplementation(
    async (request) => savedIssue(request.path.split("/").at(-1) ?? "") as never
  );
  restores.push(() => send.mockRestore());
  const rows = queuedIssues(20);
  // Drawn in the shell's main content, as the app draws it: the bar hands its focus on only while
  // the reader's press is still the cause of its going.
  const { listing } = table(rows, "", { inMain: true });
  await screen.findByRole("checkbox", { name: "Select CORE-20" });
  // The rows in the order the table draws them, and the three this case reads: the two it marks
  // and moves, the upper one marked last, and the row after them.
  const order = [...document.querySelectorAll("[data-issue-row]")].map(
    (row) => row.getAttribute("data-issue-row") ?? ""
  );
  const [last, marked, next] = order.slice(11, 14);
  // Each row's checkbox is drawn where its row stands now, 100 px a row, with the tenth row at
  // the window's top: happy-dom lays nothing out, and the hand-off reads what the reader sees.
  restores.push(layRowsOut("[data-issue-row]", 9));
  await user.click(screen.getByRole("checkbox", { name: `Select ${marked}` }));
  await user.click(screen.getByRole("checkbox", { name: `Select ${last}` }));
  // The read after the writes, held until the case answers it without the two issues.
  const reread = Promise.withResolvers<IssueSummary[]>();
  const reads = listing.mock.calls.length;
  listing.mockImplementation(() => reread.promise);
  act(() =>
    within(screen.getByRole("toolbar", { name: "Selected issues" }))
      .getByRole("button", { name: "Start next" })
      .focus()
  );
  const moves = watchMoves();
  restores.push(moves.restore);
  await user.keyboard("{Enter}");
  await waitFor(() => expect(send).toHaveBeenCalledTimes(2));
  await waitFor(() => expect(listing.mock.calls.length).toBeGreaterThan(reads));
  await act(async () => reread.resolve(rows.filter(({ key }) => key !== marked && key !== last)));
  await waitFor(() => expect(screen.queryByRole("toolbar")).not.toBeInTheDocument());
  expect(screen.queryByRole("checkbox", { name: `Select ${last}` })).toBeNull();
  await waitFor(() => expect(focusedName()).toBe(`Select ${next}`));
  const move = moves.focused.at(-1);
  expect(move && `${move.target.getAttribute("aria-label")} ${JSON.stringify(move.options)}`).toBe(
    `Select ${next} {"preventScroll":true}`
  );
  expect(moves.scrolled.map((element) => element.tagName)).toEqual([]);
  // A frame and two tasks on, the focus is still where the bar put it.
  await afterFrameAndTasks();
  expect(focusedName()).toBe(`Select ${next}`);
});

/** How tall `layTableOut` draws each of the List's rows. */
const TABLE_ROW_PX = 100;

/** The box the List's rows scroll in, as drawn now. */
const tableBox = () => document.querySelector<HTMLElement>('[data-testid="issues-table-scroll"]');

/** The List's rows, in the order the table draws them. */
const drawnRows = () => [...(tableBox()?.querySelectorAll("[data-issue-row]") ?? [])];

/** Where the row of `key` stands from the window's top, as `layTableOut` draws it; undefined
 *  where the table draws no such row. */
const rowTop = (key: string) =>
  document.querySelector(`[data-issue-row="${key}"]`)?.getBoundingClientRect().top;

/** Lays the List's rows down the box they scroll in, `TABLE_ROW_PX` a row in the order the table
 *  draws them, from the box's top less its `scrollTop`, as a reader deep in the List sees them.
 *  The box fills a 600 px window from the page's top. A row, and everything in it, is drawn in the
 *  row's box, and the box's content where its rows run. A point of the window names the row drawn
 *  there (`document.elementFromPoint`). happy-dom lays nothing out and scrolls nothing, and the
 *  List's anchor (`ViewportAnchor`) reads each of these to keep the row in hand where the reader
 *  sees it. Answers the function that gives happy-dom its own answers back. */
function layTableOut(): () => void {
  const real = Element.prototype.getBoundingClientRect;
  const rect = spyOn(Element.prototype, "getBoundingClientRect").mockImplementation(function (
    this: Element
  ) {
    const box = tableBox();
    if (box === null) return real.call(this);
    if (this === box) return new DOMRect(0, 0, 1000, 600);
    const rows = drawnRows();
    const row = this.closest("[data-issue-row]");
    if (row !== null) {
      return new DOMRect(0, rows.indexOf(row) * TABLE_ROW_PX - box.scrollTop, 1000, TABLE_ROW_PX);
    }
    if (this === box.firstElementChild) {
      return new DOMRect(0, -box.scrollTop, 1000, rows.length * TABLE_ROW_PX);
    }
    return real.call(this);
  });
  const point = spyOn(document, "elementFromPoint").mockImplementation(
    (_x, y) => drawnRows()[Math.floor((y + (tableBox()?.scrollTop ?? 0)) / TABLE_ROW_PX)] ?? null
  );
  return () => {
    rect.mockRestore();
    point.mockRestore();
  };
}

/** Twenty-one queued issues, and `send`, which answers each write of the bar's Start next. With
 *  `leaves`, an issue goes off the listing once its write lands, as a change of what the view's
 *  filter holds does; without it, the issue stays listed. The listing answers from the array the
 *  case is given. */
function queuedRows(leaves: boolean) {
  const rows = queuedIssues(21);
  const send = spyOn(api, "send").mockImplementation(async (request) => {
    const key = request.path.split("/").at(-1) ?? "";
    if (leaves)
      rows.splice(
        rows.findIndex((row) => row.key === key),
        1
      );
    return savedIssue(key) as never;
  });
  restores.push(() => send.mockRestore());
  return rows;
}

/** The box the List's rows scroll in, scrolled five rows down: the rows at 6, 7 and 8 in the
 *  order the table draws them stand at 100, 200 and 300 px, the last at the box's centre. Answers
 *  the box and that order. */
function scrollFiveRowsDown(): { box: HTMLElement; order: string[] } {
  restores.push(layTableOut());
  const box = tableBox();
  if (box === null) throw new Error("The List drew no scroll box");
  box.scrollTop = 5 * TABLE_ROW_PX;
  return { box, order: drawnRows().map((row) => row.getAttribute("data-issue-row") ?? "") };
}

// Deep in the List the reader marks two adjacent rows above the row at the box's centre and
// presses the bar's Start next. The list read after the writes no longer shows the two rows. The
// reader asked for them to go, so the rows below them move up into their place and the box does
// not scroll to hold the centre row where it stood: the box's `scrollTop` is as it was, and the
// row after the two stands where the first of them stood.
test("a bulk action whose list read takes rows out of the view above the row in hand leaves the box's scrollTop as it was, and the rows below move up", async () => {
  const user = userEvent.setup();
  const rows = queuedRows(true);
  table(rows);
  await screen.findByRole("checkbox", { name: "Select CORE-21" });
  const { box, order } = scrollFiveRowsDown();
  const [first, second, next] = order.slice(6, 9) as [string, string, string];
  const place = rowTop(first);
  await user.click(screen.getByRole("checkbox", { name: `Select ${first}` }));
  await user.click(screen.getByRole("checkbox", { name: `Select ${second}` }));
  await user.click(
    within(screen.getByRole("toolbar", { name: "Selected issues" })).getByRole("button", {
      name: "Start next",
    })
  );
  await waitFor(() => expect(rowTop(first)).toBeUndefined());
  expect(rowTop(second)).toBeUndefined();
  expect(box.scrollTop).toBe(5 * TABLE_ROW_PX);
  expect(rowTop(next)).toBe(place);
});

// An action's rows are the reader's to take away only while the action's list read is out. Start
// next leaves the two issues listed, and the bar's hand-off puts the focus on the checkbox of the
// one marked last. Later a live update takes the other one, above it, off the list. Nobody
// pressed for that, so the box scrolls up by that row's height and the focused row stays where
// the reader sees it.
test("a live update that takes away, above the row in hand, a row an earlier action changed is still made up for by a scroll", async () => {
  const user = userEvent.setup();
  const rows = queuedRows(false);
  const { client } = table(rows);
  await screen.findByRole("checkbox", { name: "Select CORE-21" });
  const { box, order } = scrollFiveRowsDown();
  const [taken, kept] = order.slice(6, 8) as [string, string];
  await user.click(screen.getByRole("checkbox", { name: `Select ${taken}` }));
  await user.click(screen.getByRole("checkbox", { name: `Select ${kept}` }));
  await user.click(
    within(screen.getByRole("toolbar", { name: "Selected issues" })).getByRole("button", {
      name: "Start next",
    })
  );
  await waitFor(() => expect(focusedName()).toBe(`Select ${kept}`));
  expect(box.scrollTop).toBe(5 * TABLE_ROW_PX);
  const seen = rowTop(kept);
  // The live update: the listing no longer holds `taken`.
  rows.splice(
    rows.findIndex((row) => row.key === taken),
    1
  );
  await act(() => client.invalidateQueries({ queryKey: ["issues"] }));
  await waitFor(() => expect(rowTop(taken)).toBeUndefined());
  expect(box.scrollTop).toBe(4 * TABLE_ROW_PX);
  expect(rowTop(kept)).toBe(seen);
});

// The List's own case of the selection bar's row under a list's sticky head
// (`SelectionBar.test.tsx`). The reader marks a row by its checkbox. Then the table's box scrolls
// until that row stands at the box's top, where the table's sticky head covers the top 60 px of
// the checkbox. Clear selection empties the selection, and the bar goes while it holds the focus.
// The issue marked last is still listed, so its checkbox takes the focus (`useLastMarkedPlace`)
// with no scroll of the engine's own. The engine does not see the head, so its own scroll to the
// nearest edge would move nothing. The table's box scrolls up by the least distance, the 60 px,
// and the checkbox stands whole below the head.
test("Clear selection hands the focus to the checkbox of the issue marked last under the List's sticky head, and the table's box scrolls it out from under the head by the least distance", async () => {
  const user = userEvent.setup();
  // Drawn in the shell's main content, as the app draws it: Clear selection is a press of the
  // reader's, so the bar hands its focus on.
  table(
    Array.from({ length: 20 }, (_, index) => issue(`CORE-${index + 1}`)),
    "",
    { inMain: true }
  );
  await screen.findByRole("checkbox", { name: "Select CORE-20" });
  // The reader marks the sixth row the table draws.
  const marked = drawnRows()[5]?.getAttribute("data-issue-row") ?? "";
  const mark = screen.getByRole("checkbox", { name: `Select ${marked}` });
  await user.click(mark);
  const box = tableBox();
  if (box === null) throw new Error("The List drew no scroll box");
  const boxRect = spyOn(box, "getBoundingClientRect").mockReturnValue(new DOMRect(0, 0, 1000, 600));
  restores.push(() => boxRect.mockRestore());
  // The checkbox is at the top of the scrolled box, behind the sticky header.
  let markTop = 0;
  const markRect = spyOn(mark, "getBoundingClientRect").mockImplementation(
    () => new DOMRect(12, markTop, 14, 14)
  );
  restores.push(() => markRect.mockRestore());
  // The box clips what overflows it, as its `overflow-auto` makes it do in a browser. happy-dom
  // applies no utility class and does not expand the `overflow` shorthand, so the case sets the
  // two values the browser computes.
  box.style.overflowX = "auto";
  box.style.overflowY = "auto";
  // The head's cells stick to the box's top, and the first of them is the Select column's. It is
  // 60 px tall and stays at the box's top however far the box scrolls. happy-dom lays out no
  // sticky element, so the case gives the cell its box.
  const selectAll = screen.getByRole("checkbox", { name: "Select every row on this page" });
  const cell = selectAll.closest("th");
  if (cell === null) throw new Error("The List drew no header cell for its Select column");
  const cellBox = spyOn(cell, "getBoundingClientRect").mockReturnValue(new DOMRect(0, 0, 44, 60));
  restores.push(() => cellBox.mockRestore());
  // Model only the checkbox's movement. Other rows need no layout in this case.
  const scrolled = spyOn(box, "scrollBy").mockImplementation((by?: ScrollToOptions | number) => {
    if (typeof by === "object") markTop -= by.top ?? 0;
  });
  restores.push(() => scrolled.mockRestore());
  act(() => screen.getByRole("button", { name: "Clear selection" }).focus());
  const moves = watchMoves();
  restores.push(moves.restore);
  await user.keyboard("{Enter}");
  await waitFor(() => expect(screen.queryByRole("toolbar")).not.toBeInTheDocument());
  await waitFor(() => expect(mark).toHaveFocus());
  expect(moves.focused.at(-1)).toEqual({ options: { preventScroll: true }, target: mark });
  expect<readonly unknown[]>(scrolled.mock.calls).toEqual([[{ top: -60 }]]);
  // The rows moved down 60 px, and the head stayed: the checkbox's top meets the head's bottom.
  expect(mark.getBoundingClientRect().top).toBe(cell.getBoundingClientRect().bottom);
  expect(moves.scrolled).toEqual([]);
});

// INFRA-3135, round 29 (the architect's ruling of 2026-11-26 07:53Z): the List's box is at least
// its head and its first three drawn rows tall (`table-layout.ts`'s `measure`), so rows whose
// height the action's settle changes change the box. At 390x664 the box went from 434 to 414 px a
// frame after the bar's going, and a landing shown whole in the old box stood below the new one
// (builds Jobs `infra-3135-infra-3135-r30q-frames` and `-r30r-frames`). So the List makes its box
// current in the commit that lets the bar go, in its `returnFocus`, before the bar's hand-off
// reads it. Here the pager
// stands far below the window, so the box is its floor: 492 px while the action's rows are 150 px
// tall, 432 once they are 130. The row now at the marked rows' place has its checkbox at 480 to
// 500 then: partly in the old box, and out of the new one.
test("Start next lands its focus wholly in the box the List draws once the action settles", async () => {
  const user = userEvent.setup();
  const send = spyOn(api, "send").mockImplementation(
    async (request) => savedIssue(request.path.split("/").at(-1) ?? "") as never
  );
  restores.push(() => send.mockRestore());
  const rows = queuedIssues(13);
  const { listing } = table(rows, "", { inMain: true });
  await screen.findByRole("checkbox", { name: "Select CORE-13" });
  const box = tableBox();
  if (box === null) throw new Error("The List drew no box for its rows");
  box.style.overflowX = "auto";
  box.style.overflowY = "auto";
  // Each row is 150 px tall until the action settles and 130 once it has: what the case sets
  // `settled` for. The head is 40 px tall, and the pager stands 1,000 px under the box.
  let settled = false;
  const rowPx = () => (settled ? 130 : 150);
  // The box is as tall as the height the page gives it, in its rect and in its scrollport, which
  // `table()` gives 600 px for every box.
  const boxPx = () => Number.parseFloat(box.style.height || box.style.maxHeight) || 600;
  Object.defineProperty(box, "clientHeight", { configurable: true, get: boxPx });
  restores.push(() => Reflect.deleteProperty(box, "clientHeight"));
  const real = Element.prototype.getBoundingClientRect;
  const rect = spyOn(Element.prototype, "getBoundingClientRect").mockImplementation(function (
    this: Element
  ) {
    const height = boxPx();
    if (this === box) return new DOMRect(0, 0, 1000, height);
    if (this instanceof HTMLTableSectionElement && this.tagName === "THEAD" && box.contains(this)) {
      return new DOMRect(0, 0, 1000, 40);
    }
    if (this.getAttribute("data-testid") === "issues-table-pager") {
      return new DOMRect(0, height + 1000, 1000, 100);
    }
    const row = this.closest("[data-issue-row]");
    if (row !== null && box.contains(row)) {
      const top = 40 + drawnRows().indexOf(row) * rowPx() - box.scrollTop;
      if (this instanceof HTMLInputElement) return new DOMRect(12, top + 50, 14, 20);
      return new DOMRect(0, top, 1000, rowPx());
    }
    return real.call(this);
  });
  restores.push(() => rect.mockRestore());
  const point = spyOn(document, "elementFromPoint").mockImplementation((_x, y) => {
    const index = Math.floor((y - 40 + box.scrollTop) / rowPx());
    return y < 40 ? null : (drawnRows()[index] ?? null);
  });
  restores.push(() => point.mockRestore());
  const scrolled = spyOn(box, "scrollBy").mockImplementation((by?: ScrollToOptions | number) => {
    if (typeof by === "object") box.scrollTop += by.top ?? 0;
  });
  restores.push(() => scrolled.mockRestore());
  const order = drawnRows().map((row) => row.getAttribute("data-issue-row") ?? "");
  // The two marked, the upper one last.
  const [last, marked] = order.slice(3, 5);
  await user.click(screen.getByRole("checkbox", { name: `Select ${marked}` }));
  await user.click(screen.getByRole("checkbox", { name: `Select ${last}` }));
  const reread = Promise.withResolvers<IssueSummary[]>();
  const reads = listing.mock.calls.length;
  listing.mockImplementation(() => reread.promise);
  act(() =>
    within(screen.getByRole("toolbar", { name: "Selected issues" }))
      .getByRole("button", { name: "Start next" })
      .focus()
  );
  await user.keyboard("{Enter}");
  await waitFor(() => expect(send).toHaveBeenCalledTimes(2));
  await waitFor(() => expect(listing.mock.calls.length).toBeGreaterThan(reads));
  // The box as the page lays it out while the action is out: its floor, 40 + 3 x 150 + 2.
  act(() => window.dispatchEvent(new Event("resize")));
  expect(box.getBoundingClientRect().height).toBe(492);
  await act(async () => {
    settled = true;
    reread.resolve(rows.filter(({ key }) => key !== marked && key !== last));
  });
  await waitFor(() => expect(screen.queryByRole("toolbar")).not.toBeInTheDocument());
  await waitFor(() =>
    expect(document.activeElement?.closest("[data-issue-row]") ?? null).not.toBeNull()
  );
  const focused = document.activeElement;
  if (!(focused instanceof HTMLInputElement)) throw new Error(`The focus is on ${focusedName()}`);
  // The box the page lays out once the action has settled, as its own resize read finds it.
  act(() => window.dispatchEvent(new Event("resize")));
  expect(box.getBoundingClientRect().height).toBe(432);
  const landed = focused.getBoundingClientRect();
  expect(`${focusedName()} at ${landed.top} to ${landed.bottom}`).toBe(
    `${focusedName()} at ${Math.max(40, landed.top)} to ${Math.min(432, landed.bottom)}`
  );
});

/** A float of the foot's status layer, as Pending changes is (`hooks/useFootInset.ts`), with a box
 *  48 px tall at the window's foot, given to it as it is drawn since happy-dom lays nothing out:
 *  while it has a box, a change of the foot's page elements, the bar's going among them, asks for
 *  the foot's pass at the next frame. */
function FootFloat(): ReactNode {
  const ref = useRef<HTMLDivElement | null>(null);
  useFootInset(ref, true, "status");
  return (
    <div
      data-testid="foot-float"
      ref={(node) => {
        ref.current = node;
        if (node !== null) {
          node.getBoundingClientRect = () => new DOMRect(600, window.innerHeight - 64, 300, 48);
        }
      }}
    />
  );
}

// INFRA-3135, round 31 (the tester's row of `e2e/table-controls.e2e.ts` for the project List's
// box keeping the frame it had when the focus landed): at 1440x900 the List gave its box its
// height as the bar went, 400 px while main still held 100 px of end room for the bar's foot,
// and the foot's pass then took that room away, so the box's next read made it 430 px, a frame
// after the landing. The List
// gives its box its height in the hand-off's own turn after the foot's pass instead, right before
// the landing (`MarkedList`'s `fit`). Here a float stands at the foot, so the foot's pass is still
// to come as the bar goes; main holds 100 px of end room while the bar is drawn, and none after
// the pass that follows its going. The box takes the window's height less that room, as the page
// lays it out. The landing on the checkbox of the issue marked last is made against the box with
// no room taken from it, and the box's own read after the landing moves nothing.
test("with the foot's pass still to come as the bar goes, the List gives its box its height after that pass and lands against it, and the box keeps that height", async () => {
  const user = userEvent.setup();
  table(
    Array.from({ length: 3 }, (_, index) => issue(`CORE-${index + 1}`)),
    "",
    { inMain: true }
  );
  const mark = await screen.findByRole("checkbox", { name: "Select CORE-2" });
  const box = tableBox();
  const main = document.querySelector("main");
  if (box === null || main === null) throw new Error("The List drew no box in main");
  const boxPx = () => Number.parseFloat(box.style.height || box.style.maxHeight);
  const bar = () => screen.queryByRole("toolbar", { name: "Selected issues" });
  // The foot's pass reads the screen under the float first (`clearScrollers`), and gives main the
  // end room the bar's foot needs: 100 px while the bar is drawn, none once it has gone. happy-dom
  // has no hit test, so the case gives main that room there, as the pass would.
  Object.defineProperty(document, "elementsFromPoint", {
    configurable: true,
    value: () => {
      main.style.paddingBottom = bar() === null ? "" : "100px";
      return [];
    },
  });
  restores.push(() => Reflect.deleteProperty(document, "elementsFromPoint"));
  const float = render(<FootFloat />);
  // Taken away before the List, so no pass is asked for once the case is over.
  restores.unshift(() => float.unmount());
  await user.click(mark);
  await afterFrameAndTasks();
  expect(main.style.paddingBottom).toBe("100px");
  // The box as the page lays it out with the bar at the foot, as its own resize read finds it.
  act(() => window.dispatchEvent(new Event("resize")));
  expect(boxPx()).toBe(window.innerHeight - 100);
  // The box's height as the landing finds it.
  const realFocus = HTMLElement.prototype.focus;
  let atLanding = Number.NaN;
  const landing = spyOn(mark, "focus").mockImplementation(function (
    this: HTMLElement,
    options?: FocusOptions
  ) {
    atLanding = boxPx();
    realFocus.call(this, options);
  });
  restores.push(() => landing.mockRestore());
  act(() =>
    within(bar() as HTMLElement)
      .getByRole("button", { name: "Clear selection" })
      .focus()
  );
  await user.keyboard("{Enter}");
  await waitFor(() => expect(mark).toHaveFocus());
  expect(main.style.paddingBottom).toBe("");
  expect(atLanding).toBe(window.innerHeight);
  // The box's own read after the landing finds the height it landed against.
  act(() => window.dispatchEvent(new Event("resize")));
  expect(boxPx()).toBe(atLanding);
});

// A table action's bar stays, held, until the list read after its writes has settled, and goes
// in the commit that lets its controls go. Start next leaves the two issues listed, so the bar's
// hand-off puts the focus on the checkbox of the issue marked last.
test("Start next from the bar, whose issues stay listed, keeps the bar held until the list read again settles, then focuses the checkbox of the issue marked last", async () => {
  const user = userEvent.setup();
  const send = spyOn(api, "send").mockImplementation(
    async (request) => savedIssue(request.path.split("/").at(-1) ?? "") as never
  );
  restores.push(() => send.mockRestore());
  const rows = queuedIssues(3);
  const { listing } = table(rows, "", { inMain: true });
  await user.click(await screen.findByRole("checkbox", { name: "Select CORE-3" }));
  await user.click(screen.getByRole("checkbox", { name: "Select CORE-2" }));
  // The read after the writes, held until the case answers it with the issues still listed.
  const reread = Promise.withResolvers<IssueSummary[]>();
  const reads = listing.mock.calls.length;
  listing.mockImplementation(() => reread.promise);
  const bar = screen.getByRole("toolbar", { name: "Selected issues" });
  const startNext = within(bar).getByRole("button", { name: "Start next" });
  act(() => startNext.focus());
  await user.keyboard("{Enter}");
  await waitFor(() => expect(send).toHaveBeenCalledTimes(2));
  await waitFor(() => expect(listing.mock.calls.length).toBeGreaterThan(reads));
  await afterFrameAndTasks();
  // The writes have landed and the read is out: the bar stays, held, with the focus.
  expect(
    screen.queryByRole("toolbar", { name: "Selected issues" })?.textContent ?? "no bar"
  ).toContain("2 selected");
  expect(startNext.getAttribute("aria-disabled")).toBe("true");
  expect(focusedName()).toBe("Start next");
  await act(async () => reread.resolve(rows));
  await waitFor(() => expect(screen.queryByRole("toolbar")).not.toBeInTheDocument());
  await waitFor(() => expect(focusedName()).toBe("Select CORE-2"));
  await afterFrameAndTasks();
  expect(focusedName()).toBe("Select CORE-2");
});

/** React's switch for its warning about an update outside `act`, which Testing Library's own
 *  waits turn off while they let the page's tasks run. */
const reactGlobals = globalThis as unknown as { IS_REACT_ACT_ENVIRONMENT?: boolean };

// INFRA-3135 round 24 (the tester's, measured offline in builds Jobs infra-3135-d-h and -d-b at
// 1440x900): the action's last step runs outside any event of the reader's - the turn a write's
// answer takes, or the read's own bound - so React committed the bar's going at normal priority
// and ran the bar's hand-off, a passive effect, in a later task: the bar went with the focus on
// the page's body, and the next frame still showed the body. The going and the hand-off are one
// render instead, committed in the turn the action ends in, so the focus is on the checkbox of
// the issue marked last the moment the bar goes. Read in that turn alone: the read's answer is
// let through and only the turns it takes are awaited, with no animation frame and no task of
// React's scheduler, which is where a commit at normal priority and its passive effects would
// run. React's own act queue is off for it, so these turns are the page's own.
test("the bar's going and its hand-off are one render when the action ends outside the reader's event", async () => {
  const user = userEvent.setup();
  const send = spyOn(api, "send").mockImplementation(
    async (request) => savedIssue(request.path.split("/").at(-1) ?? "") as never
  );
  restores.push(() => send.mockRestore());
  const rows = queuedIssues(3);
  const { listing } = table(rows, "", { inMain: true });
  await user.click(await screen.findByRole("checkbox", { name: "Select CORE-3" }));
  await user.click(screen.getByRole("checkbox", { name: "Select CORE-2" }));
  const reread = Promise.withResolvers<IssueSummary[]>();
  const reads = listing.mock.calls.length;
  listing.mockImplementation(() => reread.promise);
  const bar = screen.getByRole("toolbar", { name: "Selected issues" });
  const startNext = within(bar).getByRole("button", { name: "Start next" });
  act(() => startNext.focus());
  await user.keyboard("{Enter}");
  await waitFor(() => expect(send).toHaveBeenCalledTimes(2));
  await waitFor(() => expect(listing.mock.calls.length).toBeGreaterThan(reads));
  const acting = reactGlobals.IS_REACT_ACT_ENVIRONMENT;
  reactGlobals.IS_REACT_ACT_ENVIRONMENT = false;
  try {
    reread.resolve(rows);
    // Only the turns the read's own answer takes, and no more.
    for (const _ of Array.from({ length: 40 })) await Promise.resolve();
    expect(screen.queryByRole("toolbar", { name: "Selected issues" })).toBeNull();
    expect(focusedName()).toBe("Select CORE-2");
  } finally {
    reactGlobals.IS_REACT_ACT_ENVIRONMENT = acting;
  }
  await act(async () => undefined);
});

// Start next over three queued issues writes one step at a time, and each step starts only once
// the one before it is answered, long after the press that began the action. The whole action is
// still that press's, so once the list read after it settles and the bar goes, the bar's hand-off
// puts the focus on the checkbox of the issue marked last, which the list still holds, and never
// on the page's body.
test("Start next over three queued issues from the bar, its steps answered one after another, hands the focus to the issue marked last", async () => {
  const user = userEvent.setup();
  // What answers each step's write, in the order the steps are sent.
  const answers: ((saved: Issue) => void)[] = [];
  const send = spyOn(api, "send").mockImplementation(() => {
    const answer = Promise.withResolvers<Issue>();
    answers.push(answer.resolve);
    return answer.promise as never;
  });
  restores.push(() => send.mockRestore());
  table(
    Array.from({ length: 10 }, (_, index) =>
      issue(`CORE-${index + 1}`, { labels: ["legion"], rank: String(index).padStart(3, "0") })
    ),
    "",
    { inMain: true }
  );
  await screen.findByRole("row", { name: "CORE-10 Work CORE-10" });
  for (const key of ["CORE-8", "CORE-9", "CORE-10"])
    await user.click(screen.getByRole("checkbox", { name: `Select ${key}` }));
  act(() =>
    within(screen.getByRole("toolbar", { name: "Selected issues" }))
      .getByRole("button", { name: /^Start next$/ })
      .focus()
  );
  await user.keyboard("{Enter}");
  // The steps move CORE-10 up first, then CORE-9, then CORE-8.
  for (const [step, key] of ["CORE-10", "CORE-9", "CORE-8"].entries()) {
    await waitFor(() => expect(send).toHaveBeenCalledTimes(step + 1));
    // A frame and two tasks, by when a press whose action had been answered would have ended.
    await afterFrameAndTasks();
    expect(focusedName()).toBe("Start next");
    await act(async () => answers[step]?.(savedIssue(key)));
  }
  await waitFor(() => expect(screen.queryByRole("toolbar")).not.toBeInTheDocument());
  await waitFor(() => expect(focusedName()).toBe("Select CORE-10"));
  await afterFrameAndTasks();
  expect(focusedName()).toBe("Select CORE-10");
});

test("leaving the table clears what the bar says it will act on", async () => {
  const user = userEvent.setup();
  const { view } = table([issue("CORE-1")]);
  await user.click(await screen.findByRole("checkbox", { name: "Select CORE-1" }));
  expect(readSelectedIssues()).toEqual(["CORE-1"]);
  view.unmount();
  expect(readSelectedIssues()).toEqual([]);
});

// The Board in the table's place takes the selection with the table, as leaving the page does,
// and the table drawn again comes back with no row selected.
test("the Board in the table's place clears what the bar says it will act on", async () => {
  const user = userEvent.setup();
  table([issue("CORE-1"), issue("CORE-2")]);
  await user.click(await screen.findByRole("checkbox", { name: "Select CORE-1" }));
  await user.click(screen.getByRole("checkbox", { name: "Select CORE-2" }));
  expect(readSelectedIssues()).toEqual(["CORE-1", "CORE-2"]);
  await user.click(screen.getByRole("button", { name: "Board" }));
  await screen.findByRole("region", { name: "Project board" });
  expect(readSelectedIssues()).toEqual([]);
  await user.click(screen.getByRole("button", { name: "Table" }));
  const first = await screen.findByRole<HTMLInputElement>("checkbox", { name: "Select CORE-1" });
  expect(first.checked).toBe(false);
  expect(screen.getByRole<HTMLInputElement>("checkbox", { name: "Select CORE-2" }).checked).toBe(
    false
  );
  expect(readSelectedIssues()).toEqual([]);
});

// The Board's own keys answer while it shows: the shortcut help lists them and none of the table
// it hides, and the table's come back with the table.
test("the Board in the table's place takes the table's keys off the help, and the table brings them back", async () => {
  const user = userEvent.setup();
  table([issue("CORE-1")]);
  await screen.findByRole("row", { name: "CORE-1 Work CORE-1" });
  const labels = () => appKeymap.describe().map((entry) => entry.label);
  expect(labels()).toContain("Next issue");
  await user.click(screen.getByRole("button", { name: "Board" }));
  await screen.findByRole("region", { name: "Project board" });
  expect(labels()).toContain("Next card");
  expect(labels()).not.toContain("Next issue");
  expect(labels()).not.toContain("Open the focused issue");
  await user.click(screen.getByRole("button", { name: "Table" }));
  await screen.findByRole("row", { name: "CORE-1 Work CORE-1" });
  expect(labels()).toContain("Next issue");
  expect(labels()).not.toContain("Next card");
});

// Open's bar counts the rows the table lists. With closed issues unloaded it shows no Done
// segment at all, rather than a cell that reads as a count; the Status filter's own Done option
// says they load when it is picked.
test("the status bar has no Done cell while closed issues are unloaded", async () => {
  const user = userEvent.setup();
  table([issue("CORE-1")]);
  const bar = await screen.findByRole("region", { name: "Issues by status" });
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

const TWO_STATUSES = [
  issue("CORE-1", { advances: [{ id: "p", inherited: false }] }),
  issue("CORE-2", { advances: [{ id: "p", inherited: false }], status: "in_progress" }),
];
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
  table(TWO_STATUSES, "", undefined, {
    layout: LEGEND_LAYOUT,
    listing: { advances: "p" },
    stateKey: "w",
  });
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

// Where the names fit, or where the table does not ask for it, as a priority's table with no
// layout does not, each segment draws its name and its count and no legend stands under the bar.
for (const [what, layout, names] of [
  ["a bar its names fit", LEGEND_LAYOUT, 150],
  ["a bar that asks for no legend", undefined, 300],
] as const) {
  test(`${what} draws each name beside its count, with no legend`, async () => {
    sizeStatusBar(200, names);
    table(TWO_STATUSES, "", undefined, { layout, listing: { advances: "p" }, stateKey: "w" });
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
  // And no chip loose in the control row: the flag sits behind the row's filter trigger, and
  // the rule is still the Claimed by column's, picked like any other value.
  expect(screen.queryByRole("button", { name: "Unclaimed" })).toBeNull();
  await user.click(screen.getByRole("button", { name: /^Filter Claimed by/ }));
  const picked = screen.getByRole("option", { name: /^Nobody or lapsed/ });
  expect(picked.getAttribute("aria-selected")).toBe("true");
  await user.click(picked);
  await screen.findByRole("row", { name: "CORE-1 Work CORE-1" });
});

// R3: a narrowing that is no column of the table sits behind the row's one filter trigger.
// Unclaimed is `?unclaimed=1` there, which is the Claimed by column's "Nobody or lapsed".
test("the row's filter panel turns Unclaimed on and the table narrows to it", async () => {
  const user = userEvent.setup();
  const held: IssueSummary["claim"] = {
    actor: { kind: "user", id: "bob" },
    at: "2026-09-10T00:00:00Z",
  };
  table([issue("CORE-1", { claim: held }), issue("CORE-2")]);
  await screen.findByRole("row", { name: "CORE-1 Work CORE-1" });
  await user.click(screen.getByRole("button", { name: "Filters · 0" }));
  await user.click(screen.getByRole("button", { name: "Unclaimed" }));
  await waitFor(() =>
    expect(screen.getByTestId("route-search").textContent).toContain("unclaimed=1")
  );
  await waitFor(() => expect(screen.queryByRole("row", { name: "CORE-1 Work CORE-1" })).toBeNull());
  expect(screen.getByRole("row", { name: "CORE-2 Work CORE-2" })).toBeDefined();
  expect(screen.getByRole("button", { name: "Remove Unclaimed filter" })).toBeDefined();
});

// Which claims have lapsed is the agent registry's answer. Until `/api/v1/agents` answers, no
// session's claim reads lapsed, so a count of Nobody or lapsed would leave out the issue whose
// session has gone. The Claimed by filter offers the values the registry decides with no count
// beside them until then, and counts them once the registry answers. Nobody counts the issues no
// one claims, which the registry never decides, so it draws its count from the first.
test("Claimed by draws no count beside the values the agent registry decides until it answers, and Nobody's from the first", async () => {
  const user = userEvent.setup();
  const registry = Promise.withResolvers<Agent[]>();
  const gone: IssueSummary["claim"] = {
    actor: { kind: "session", id: "session-gone", origin: { session_title: "Gone holder" } },
    at: "2026-09-10T00:00:00Z",
  };
  table([issue("CORE-1"), issue("CORE-2", { claim: gone })], "", { agents: registry.promise });
  await screen.findByRole("row", { name: "CORE-2 Work CORE-2" });
  await user.click(screen.getByRole("button", { name: /^Filter Claimed by/ }));
  const claims = () => optionNames(screen.getByRole("listbox", { name: "Claimed by options" }));
  expect(claims()).toHaveLength(3);
  expect(claims().filter((name) => /, \d+ issues?$/.test(name))).toEqual(["Nobody, 1 issue"]);
  registry.resolve([]);
  await waitFor(() => expect(claims()).toContain("Nobody or lapsed, 2 issues"));
});

/** One header filter's options, by the name each is read out as, with its panel shut after. */
async function headerOptions(user: UserEvent, header: string): Promise<string[]> {
  await user.click(screen.getByRole("button", { name: new RegExp(`^Filter ${header}`) }));
  const names = optionNames(screen.getByRole("listbox", { name: `${header} options` }));
  await user.keyboard("{Escape}");
  await waitFor(() => expect(screen.queryByRole("listbox")).toBeNull());
  return names;
}

// With the agent list refused, the page says it could not check which claims have lapsed, and
// Nobody or lapsed lists the issues no one claims, holding every claim as held. Each header
// filter's count that the registry does not decide draws once the issues answer - Assignee's,
// Priority's and Claimed by's Nobody - and the Claimed by values it decides draw none, never a
// count it has not decided.
test("with the agent registry refused, the header filters draw every count it does not decide, and none that it decides", async () => {
  const user = userEvent.setup();
  const refused = Promise.reject(new Error("agent list refused"));
  refused.catch(() => undefined);
  const running: IssueSummary["claim"] = {
    actor: { kind: "session", id: "session-running", origin: { session_title: "Running holder" } },
    at: "2026-09-10T00:00:00Z",
  };
  table(
    [
      issue("CORE-1", { assignee: "alice", priority: 1 }),
      issue("CORE-2", { assignee: "bob", priority: 2 }),
      issue("CORE-3", { assignee: "alice", claim: running, priority: 1 }),
    ],
    "?unclaimed=1",
    { agents: refused }
  );
  await screen.findByRole("row", { name: "CORE-2 Work CORE-2" });
  await screen.findByTestId("unclaimed-unchecked");
  expect(screen.queryByRole("row", { name: "CORE-3 Work CORE-3" })).toBeNull();
  expect(await headerOptions(user, "Assignee")).toEqual(["alice, 1 issue", "bob, 1 issue"]);
  expect(await headerOptions(user, "Priority")).toEqual(["P1, 1 issue", "P2, 1 issue"]);
  const claims = await headerOptions(user, "Claimed by");
  expect(claims).toHaveLength(3);
  expect(claims.filter((name) => /, \d+ issues?$/.test(name))).toEqual(["Nobody, 2 issues"]);
});

// INFRA-3135, round 28 (the tester's red row `f8722500`): the counts the registry does not decide
// are drawn once its first read has settled on its error, and a later read of that list takes none
// of them away. The rows are the same while that read is out - every claim counts as held until a
// read answers - so nothing the counts stand on has changed, and the page goes on saying it could
// not check which claims have lapsed.
test("a later read of a refused agent list keeps the counts it does not decide, and the notice", async () => {
  const user = userEvent.setup();
  const refused = Promise.reject(new Error("agent list refused"));
  refused.catch(() => undefined);
  const held = Promise.withResolvers<Agent[]>();
  const { client, registry } = table(
    [
      issue("CORE-1", { assignee: "alice", priority: 1 }),
      issue("CORE-2", { assignee: "bob", priority: 2 }),
    ],
    "?unclaimed=1",
    { agents: refused }
  );
  await screen.findByRole("row", { name: "CORE-2 Work CORE-2" });
  await screen.findByTestId("unclaimed-unchecked");
  expect(await headerOptions(user, "Assignee")).toEqual(["alice, 1 issue", "bob, 1 issue"]);
  // The shell's next poll of the list: its read is out while the filters are read again.
  registry.mockImplementation(() => held.promise);
  await act(async () => {
    void client.refetchQueries({ queryKey: ["agents"] });
    await Promise.resolve();
  });
  expect(registry).toHaveBeenCalledTimes(2);
  expect(await headerOptions(user, "Assignee")).toEqual(["alice, 1 issue", "bob, 1 issue"]);
  expect(await headerOptions(user, "Priority")).toEqual(["P1, 1 issue", "P2, 1 issue"]);
  expect(screen.getByTestId("unclaimed-unchecked")).toBeTruthy();
  // The held read lands before the row ends, so what it draws is this row's own.
  await act(async () => {
    held.resolve([]);
    await held.promise;
  });
});

// With Nobody or lapsed picked and `/api/v1/agents` still pending, the listed rows can still
// change when the registry answers, so no column draws a count until the registry settles.
test("with Nobody or lapsed picked, no header filter draws a count while the agent registry is pending", async () => {
  const user = userEvent.setup();
  const registry = Promise.withResolvers<Agent[]>();
  const gone: IssueSummary["claim"] = {
    actor: { kind: "session", id: "session-gone", origin: { session_title: "Gone holder" } },
    at: "2026-09-10T00:00:00Z",
  };
  table(
    [
      issue("CORE-1", { assignee: "alice", priority: 1 }),
      issue("CORE-2", { assignee: "bob", claim: gone, priority: 2 }),
    ],
    "?unclaimed=1",
    { agents: registry.promise }
  );
  await screen.findByRole("row", { name: "CORE-1 Work CORE-1" });
  for (const header of ["Assignee", "Priority", "Claimed by"]) {
    const names = await headerOptions(user, header);
    expect(names.length).toBeGreaterThan(0);
    for (const name of names) expect(name).not.toMatch(/, \d+ issues?$/);
  }
  registry.resolve([]);
  await screen.findByRole("row", { name: "CORE-2 Work CORE-2" });
  expect(await headerOptions(user, "Assignee")).toEqual(["alice, 1 issue", "bob, 1 issue"]);
  expect(await headerOptions(user, "Priority")).toEqual(["P1, 1 issue", "P2, 1 issue"]);
  expect(await headerOptions(user, "Claimed by")).toContain("Nobody or lapsed, 2 issues");
});

// R5, R9: no column, grouping, filter or action is about epics. An address of the build before
// that names the Epic column's filter or its grouping lists every row, with no heading.
test("no column, grouping or selection action is about epics, and an old epic address lists every row", async () => {
  const user = userEvent.setup();
  table([issue("CORE-1", { parent: "CORE-9" }), issue("CORE-2")], "?f.epic=No+epic&group=epic");
  const list = await screen.findByRole("table", { name: "Project issues table" });
  await within(list).findByRole("row", { name: "CORE-1 Work CORE-1" });
  expect(within(list).getByRole("row", { name: "CORE-2 Work CORE-2" })).toBeDefined();
  expect(headings()).toEqual([]);
  const headers = within(list)
    .getAllByRole("columnheader")
    .map((header) => header.textContent ?? "");
  expect(headers.filter((text) => /epic/i.test(text))).toEqual([]);
  await user.click(screen.getByRole("button", { name: "Columns" }));
  expect(screen.queryByRole("checkbox", { name: /epic/i })).toBeNull();
  await user.keyboard("{Escape}");
  await user.click(screen.getByRole("button", { name: "Group by" }));
  expect(screen.getByRole("radio", { name: "Status" })).toBeDefined();
  expect(screen.queryByRole("radio", { name: /epic/i })).toBeNull();
  await user.keyboard("{Escape}");
  await user.click(screen.getByRole("checkbox", { name: "Select CORE-2" }));
  const bar = screen.getByRole("toolbar");
  expect(within(bar).getByRole("button", { name: "Start next" })).toBeDefined();
  expect(within(bar).queryByRole("combobox")).toBeNull();
  expect(
    within(bar)
      .getAllByRole("button")
      .filter((button) => /epic/i.test(button.textContent ?? ""))
  ).toEqual([]);
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
  // The scroll ends on the table's own timer, inside `act`; its end reads no layout either.
  await endWindowScroll();
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
])("a grouping picked from the toolbar orders its headings under %s", async (_name, search, ungrouped) => {
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
  await groupRowsBy(user, "Priority");
  await waitFor(() =>
    expect(headings().map((text) => text.match(/P\d/)?.[0])).toEqual(["P0", "P1", "P2", "P3"])
  );
  await groupRowsBy(user, "Status");
  await waitFor(() =>
    expect(
      headings().map((text) =>
        ["Todo", "In progress", "Needs review"].find((name) => text.includes(name))
      )
    ).toEqual(["Todo", "In progress", "Needs review"])
  );
  // Ungrouped again, the rows follow the reader's sort alone: no grouping order survives it.
  await groupRowsBy(user, "Nothing");
  await waitFor(() => expect(headings()).toEqual([]));
  await waitFor(() =>
    expect(
      [...document.querySelectorAll("[data-issue-row]")].map((row) =>
        row.getAttribute("data-issue-row")
      )
    ).toEqual(ungrouped)
  );
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

/** What Open keeps hidden until a reader shows it, in its stored record's words: every column
 *  but its defaults, the Legion ones of a project with a daemon among them. */
const OPEN_HIDDEN = [
  "updated",
  "closed",
  "roles",
  "activity",
  "waited",
  "waiting_reason",
  "time_in_phase",
  "work_hours",
  "review_rounds",
  "legion",
  "phase_hours",
].map((id) => `"${id}":false`);

// A reader's saved layout is read and written back as it always was: under the same key, which
// is Open's now, in the same JSON, with a column the table does not have, a pinned column's
// visibility and a width out of bounds dropped, and every other field of the record left where
// it was. A project's table has none of the listing columns, so it writes none of them. Open
// keeps hidden the columns it does not open with, Updated among them, until a reader shows one.
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
    `{"sizing":{"title":360},"order":["status","key"],"visibility":{"updated":true,${OPEN_HIDDEN.slice(1).join(",")},"labels":false},"kept":1}`
  );
});

test("the List stores a reader's first layout with Updated hidden and no listing column", async () => {
  table([issue("CORE-1")]);
  await screen.findByRole("row", { name: "CORE-1 Work CORE-1" });
  expect(localStorage.getItem("dispatch.project-table.CORE")).toBe(
    `{"sizing":{},"order":[],"visibility":{${OPEN_HIDDEN.join(",")}}}`
  );
});

// What `in` read on main as a column: a name every object inherits. No app path writes one, so
// only a corrupt record holds it, and it is dropped like any other name the table has no column for.
test("a stored width under an inherited name such as toString is dropped", () => {
  localStorage.setItem("dispatch.project-table.CORE", '{"sizing":{"toString":200,"title":360}}');
  expect(readLayout(tableStorageKey("CORE"), ISSUE_TABLE_COLUMNS).sizing).toEqual({ title: 360 });
});

/** Makes `getBoundingClientRect` report an issue table's scroll box `width` pixels wide and
 *  every other element 100, each 40 high, until the row ends; and the box's `offsetWidth` and
 *  `clientWidth` its border box and the room inside its borders and beside its scrollbar, `edge`
 *  pixels less (2, its two borders, by default). The spy is on `Element.prototype`, which owns
 *  the method: one on `HTMLElement` would leave an own copy behind that shadows every later spy.
 *  The two widths are getters happy-dom puts on `HTMLElement.prototype`, so they are replaced
 *  there and put back as they were. */
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

/** Each header cell of the table named `name`, left to right, as `<its column's words> <width>
 *  <the offset a pinned cell is held at, or ->`. */
function headerCells(name: string): string[] {
  const head = screen.getByRole("table", { name }).querySelector("thead");
  return [...(head?.querySelectorAll("th") ?? [])].map((cell) =>
    [
      (cell.textContent ?? "").replace(/[▲▼]|Filter$/g, "") || "-",
      cell.style.width,
      cell.style.left || "-",
    ].join(" ")
  );
}

/** The header cells of an issue table another page draws (the work under a priority), drawn
 *  with the scroll box `width` pixels wide, at the address's `search`. */
async function headerLayout(width: number, search = ""): Promise<string[]> {
  stubScrollBoxWidth(width);
  table([issue("CORE-1", { advances: [{ id: "ship-it", inherited: false }] })], search, undefined, {
    listing: { advances: "ship-it" },
    stateKey: "w",
  });
  await screen.findByRole("row", { name: "CORE-1 Work CORE-1" });
  return headerCells("Priority issues table");
}

/** The columns after the pinned ones, at the registry's widths, then the Start next column. */
const UNPINNED_REST = [
  "Status 132px -",
  "Assignee 180px -",
  "Created by 200px -",
  "Labels 200px -",
  "Claimed by 220px -",
  "Open asks 148px -",
  "Created 148px -",
  "- 104px -",
];

// A table another page draws keeps today's widths and pins at a desktop box, a box too narrow for
// every pin, and a phone: the registry's starting widths and minimums, Select, Priority, Key and
// Title pinned within 60% of a wide box with Title narrowing first and the rightmost unpinning,
// and Select, Key and Title within 85% of a phone's, Key at its phone width and Title taking what
// is left.
test("an embedded table draws its columns at today's widths and pins at 1400 px", async () => {
  expect(await headerLayout(1400)).toEqual([
    "- 44px 0px",
    "Priority 140px 44px",
    "Key 108px 184px",
    "Title 380px 292px",
    "Queue 124px -",
    ...UNPINNED_REST,
  ]);
});

test("an embedded table narrows Title and unpins from the right in a 700 px box", async () => {
  localStorage.setItem(
    "dispatch.project-table.priority-work",
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

test("an embedded table pins Select, Key and Title on a 390 px phone", async () => {
  expect(await headerLayout(390)).toEqual([
    "- 44px 0px",
    "Key 100px 44px",
    "Title 187.5px 144px",
    "Priority 140px -",
    "Queue 124px -",
    ...UNPINNED_REST,
  ]);
});

// dispatch://INFRA-3221/comment/141bbad2-b75e-4bb4-b0a7-b5b1af541a3c, with R13's leave to widen a
// width that cuts its header: on a phone the embedded tables' Key holds its header whole, sorted
// by it and not. Measured in Chromium: its sort button is a 44 px touch target, which holds Key
// with its mark (38 px), its filter button 44 px, and the cell's padding 12 px below `md`; 88 px
// left the filter 12 px past the cell.
test("on a phone an embedded table's Key holds its header whole, sorted by it and not", async () => {
  const needs = 44 + 44 + 12;
  const unsorted = await headerLayout(390);
  expect(widthOf(unsorted, "Key")).toBeGreaterThanOrEqual(needs);
  expect(unsorted).toContain("Key 100px 44px");
  for (const restore of restores.splice(0)) restore();

  const sorted = await headerLayout(390, "?w.sort=key");
  const table = screen.getByRole("table", { name: "Priority issues table" });
  expect(within(table).getByRole("columnheader", { name: /^Key/ })).toHaveAttribute(
    "aria-sort",
    "ascending"
  );
  expect(widthOf(sorted, "Key")).toBeGreaterThanOrEqual(needs);
  expect(sorted).toContain("Key 100px 44px");
});

// A width the reader dragged on a desktop is the width the header is drawn at, and a stored one
// below a column's minimum is drawn at the minimum.
test("a stored desktop width reaches an embedded table's header", async () => {
  localStorage.setItem(
    "dispatch.project-table.priority-work",
    JSON.stringify({ sizing: { assignee: 300, status: 50 } })
  );
  const cells = await headerLayout(1400);
  expect(cells).toContain("Assignee 300px -");
  expect(cells).toContain("Status 132px -");
});

// R13: the priority pages keep today's rule. A Title the reader widened by 300 px would take the
// pins past 60% of a 1400 px box, so it narrows by what they would pass it by, and stays pinned.
test("an embedded table narrows a Title the reader widened to keep its pins within their share", async () => {
  localStorage.setItem(
    "dispatch.project-table.priority-work",
    JSON.stringify({ sizing: { title: 380 + 300 } })
  );
  expect(await headerLayout(1400)).toEqual([
    "- 44px 0px",
    "Priority 140px 44px",
    "Key 108px 184px",
    "Title 548px 292px",
    "Queue 124px -",
    ...UNPINNED_REST,
  ]);
});

// dispatch://INFRA-3221/comment/141bbad2-b75e-4bb4-b0a7-b5b1af541a3c: today's minimums of the
// priority pages' tables hold each header with its sort mark (Open asks' 148 px its 83 px label
// and mark, measured), so a sort by Open asks widens it not at all.
test("an embedded table's Open asks is 148 px sorted by it and not", async () => {
  expect(await headerLayout(1400)).toContain("Open asks 148px -");
  for (const restore of restores.splice(0)) restore();
  expect(await headerLayout(1400, "?w.sort=asks")).toContain("Open asks 148px -");
});

/** The header cells of the Issues tab's Open, in a project with no daemon, drawn with the
 *  scroll box `width` pixels wide, at the address's `search`. */
async function openHeaderLayout(width: number, search = ""): Promise<string[]> {
  stubScrollBoxWidth(width);
  table([issue("CORE-1")], search);
  await screen.findByRole("row", { name: "CORE-1 Work CORE-1" });
  return headerCells("Project issues table");
}

/** The width the Issues tab draws the header `name` at, in px. */
const widthOf = (cells: readonly string[], name: string): number =>
  Number.parseFloat(
    cells
      .find((cell) => cell.startsWith(`${name} `))
      ?.split(" ")
      .at(-2) ?? ""
  );

// dispatch://INFRA-3221/comment/141bbad2-b75e-4bb4-b0a7-b5b1af541a3c: in a 1000 px box every
// column of Open is at its minimum. Open asks' 140 px holds its header and its 44 px filter
// button, and while Open is sorted by it the sort mark too, at least 148 px (its 83 px label with
// the mark, measured). A sort by another column gives the mark's width back, and Created by,
// whose minimum already holds its own mark, keeps it.
test("on the Issues tab Open asks is 140 px, and takes its sort mark on only while sorted by it", async () => {
  const user = userEvent.setup();
  expect(widthOf(await openHeaderLayout(1000), "Open asks")).toBe(140);
  for (const restore of restores.splice(0)) restore();

  const sorted = await openHeaderLayout(1000, "?sort=asks");
  expect(widthOf(sorted, "Open asks")).toBeGreaterThanOrEqual(148);
  expect(widthOf(sorted, "Open asks")).toBe(140 + SORT_MARK_WIDTH);
  const author = widthOf(sorted, "Created by");

  const tableNamed = screen.getByRole("table", { name: "Project issues table" });
  await user.click(within(tableNamed).getByRole("button", { name: /^Created by/ }));
  await waitFor(() => expect(routeSearch()).toContain("sort=author"));
  const resorted = headerCells("Project issues table");
  expect(widthOf(resorted, "Open asks")).toBe(140);
  expect(widthOf(resorted, "Created by")).toBe(author);
});

// k12 (R10): on the Issues tab a Title the reader widened by 300 px is drawn at its stored width.
// With it the pins would cover 980 px of a 1400 px box, past their 840, so Title scrolls with the
// table after them rather than drawing narrower.
test("on the Issues tab a Title the reader widened keeps its width, and scrolls past the pins' share", async () => {
  localStorage.setItem(
    "dispatch.project-table.CORE",
    JSON.stringify({ sizing: { title: 380 + 300 } })
  );
  expect((await openHeaderLayout(1400)).slice(0, 5)).toEqual([
    "- 44px 0px",
    "Priority 132px 44px",
    "Key 124px 176px",
    "Title 680px -",
    "Status 150px -",
  ]);
  expect(JSON.parse(localStorage.getItem("dispatch.project-table.CORE") ?? "{}").sizing).toEqual({
    title: 680,
  });
});

// k12 (R10): on a phone Select and Key are pinned, and Title follows them, not pinned, and fills
// the rest of the row: 99% of the box, whose two border pixels are not the rows'.
test("on a 390 px phone the Issues tab pins Select and Key, and Title follows them and fills the row", async () => {
  const [select, key, title, next] = await openHeaderLayout(390);
  expect([select, key, next]).toEqual(["- 44px 0px", "Key 124px 44px", "Priority 132px -"]);
  const [name, width, left] = (title ?? "").split(" ");
  expect([name, left]).toEqual(["Title", "-"]);
  expect(Number.parseFloat(width ?? "")).toBeCloseTo(390 * 0.99 - 44 - 124);
});

// k12 (R10): on a phone a header dropped onto the one after Title moves there, and one dropped
// onto Title lands right after it, since Title stays after Key. The order drawn is the one
// stored, so a reload draws it again.
test("on a phone a header dropped after Title or onto Title moves there, and a reload keeps the order", async () => {
  /** The words of the first six headers, left to right. */
  const drawn = () =>
    headerCells("Project issues table")
      .slice(0, 6)
      .map((cell) => cell.replace(/ \S+ \S+$/, ""));
  await openHeaderLayout(390);
  expect(drawn()).toEqual(["-", "Key", "Title", "Priority", "Status", "Created by"]);
  const header = (name: RegExp) =>
    within(screen.getByRole("table", { name: "Project issues table" })).getByRole("columnheader", {
      name,
    });
  const drag = (dragged: RegExp, onto: RegExp) => {
    const handle = header(dragged).querySelector('button[draggable="true"]');
    if (handle === null) throw new Error(`The ${dragged} header has no drag handle`);
    // A header's drag start, drag over, drop and drag end: HTML5 drag and drop, which user-event
    // has no form for.
    fireEvent.dragStart(handle);
    fireEvent.dragOver(header(onto));
    fireEvent.drop(header(onto));
    fireEvent.dragEnd(handle);
  };

  drag(/^Status/, /^Priority/);
  await waitFor(() =>
    expect(drawn()).toEqual(["-", "Key", "Title", "Status", "Priority", "Created by"])
  );
  drag(/^Created by/, /^Title/);
  const moved = ["-", "Key", "Title", "Created by", "Status", "Priority"];
  await waitFor(() => expect(drawn()).toEqual(moved));
  // Stored as drawn: Title after Key, the column dropped onto it right after it.
  const stored = JSON.parse(localStorage.getItem("dispatch.project-table.CORE") ?? "{}");
  expect(stored.order.slice(0, 6)).toEqual([
    "select",
    "key",
    "title",
    "author",
    "status",
    "priority",
  ]);

  // A reload: the page drawn again from what the browser kept.
  for (const restore of restores.splice(0)) restore();
  await openHeaderLayout(390);
  expect(drawn()).toEqual(moved);
});

/** What every sortable header's tooltip says of its sort, and what a header that a drag moves
 *  adds to it (`DataTable`). */
const SORT_HINT = "Sort (shift-click to add a second sort)";
const DRAG_HINT = `${SORT_HINT}; drag to move the column`;

// R10: on a phone Title stays right after Key, so it is no drag handle and its sort hint says
// nothing of a drag; Priority, after it, is dragged as before. On a desktop the rule is today's:
// a Title the reader widened past the pins' share scrolls with the table and is dragged as any
// other column there.
test("on a phone Title is no drag handle and its hint names no drag, and on a desktop it is unchanged", async () => {
  const user = userEvent.setup();
  const sort = (name: string) =>
    within(screen.getByRole("table", { name: "Project issues table" })).getByRole("button", {
      name,
    });
  const hintOf = async (name: string) => {
    await user.hover(sort(name));
    const hint = screen.getByRole("tooltip").textContent;
    await user.unhover(sort(name));
    return hint;
  };
  await openHeaderLayout(390);
  expect(sort("Title")).toHaveAttribute("draggable", "false");
  expect(await hintOf("Title")).toBe(SORT_HINT);
  expect(sort("Priority")).toHaveAttribute("draggable", "true");
  expect(await hintOf("Priority")).toBe(DRAG_HINT);

  for (const restore of restores.splice(0)) restore();
  localStorage.setItem(
    "dispatch.project-table.CORE",
    JSON.stringify({ sizing: { title: 380 + 300 } })
  );
  await openHeaderLayout(1400);
  expect(sort("Title")).toHaveAttribute("draggable", "true");
  expect(await hintOf("Title")).toBe(DRAG_HINT);
});

test("the Status filter counts Done once the closed issues are loaded", async () => {
  const user = userEvent.setup();
  table([issue("CORE-1"), issue("CORE-2", { status: "done" })], "?status=done");
  await screen.findByRole("row", { name: "CORE-2 Work CORE-2" });
  await user.click(screen.getByRole("button", { name: /^Filter Status/ }));
  expect(screen.getByRole("option", { name: "Done, 1 issue" })).toBeDefined();
});

// The header filters count the rows the table waits on. Before Done is picked, the open issues
// are every one of them, so the filters count them and Done says it is not loaded. Once Done is
// picked and the closed issues are out, a count of the open issues alone would read as every
// issue's, so no filter draws a count until the closed issues answer, and then each counts them.
test("picking Done draws no header filter count until the closed issues are read", async () => {
  const user = userEvent.setup();
  const closed = Promise.withResolvers<IssueSummary[]>();
  const rows = [
    issue("CORE-1", { assignee: "alice", priority: 1 }),
    issue("CORE-2", { assignee: "bob", priority: 2 }),
    issue("CORE-3", { assignee: "alice", priority: 1, status: "done" }),
    issue("CORE-4", { assignee: "alice", priority: 1, status: "done" }),
  ];
  table(rows, "", { full: closed.promise });
  await screen.findByRole("row", { name: "CORE-2 Work CORE-2" });
  expect(await headerOptions(user, "Assignee")).toEqual(["alice, 1 issue", "bob, 1 issue"]);
  await user.click(screen.getByRole("button", { name: /^Filter Status/ }));
  await user.click(screen.getByRole("option", { name: "Done, not loaded" }));
  await user.keyboard("{Escape}");
  await screen.findByText("Loading closed issues…");
  for (const header of ["Assignee", "Priority"]) {
    const names = await headerOptions(user, header);
    expect(names).toHaveLength(2);
    for (const name of names) expect(name).not.toMatch(/, \d+ issues?$/);
  }
  closed.resolve(rows);
  await screen.findByRole("row", { name: "CORE-3 Work CORE-3" });
  expect(await headerOptions(user, "Assignee")).toEqual(["alice, 3 issues", "bob, 1 issue"]);
  expect(await headerOptions(user, "Priority")).toEqual(["P1, 3 issues", "P2, 1 issue"]);
});

// A write of an action the reader dismissed is nobody's interrupted action. Retried after, it
// reports like any other change rather than telling the reader to finish steps that are gone.
test("a write retried after its action was dismissed reports in the ordinary wording", async () => {
  const user = userEvent.setup();
  let refused = false;
  const send = spyOn(api, "send").mockImplementation(async (request) => {
    if (!refused) {
      refused = true;
      throw new ApiError(403, { error: "refused", code: "FORBIDDEN" });
    }
    return savedIssue(request.path.split("/").at(-1) ?? "CORE-2") as never;
  });
  restores.push(() => send.mockRestore());
  const { client } = table([
    issue("CORE-1", { labels: ["legion"], rank: "a" }),
    issue("CORE-2", { labels: ["legion"], rank: "b" }),
  ]);
  await user.click(await screen.findByRole("checkbox", { name: "Select CORE-2" }));
  await user.click(within(screen.getByRole("toolbar")).getByRole("button", { name: "Start next" }));
  const recovery = await screen.findByRole("region", { name: "Interrupted table action" });
  await user.click(within(recovery).getByRole("button", { name: "Dismiss" }));
  const retry = writesOf(client)
    .rows()
    .find((write) => write.status === "refused");
  if (!retry) throw new Error("The refused write was lost");
  writesOf(client).retry(retry.id);
  await waitFor(() =>
    expect(
      within(screen.getByRole("region", { name: "Project issues" })).getByRole("status").textContent
    ).toContain("All changes saved")
  );
  expect(screen.queryByRole("region", { name: "Interrupted table action" })).toBeNull();
});

// B1: a status the bundle does not know — a bookmark in another casing, a deploy window — is a
// filter that matches nothing, not a page the reader loses.
test.each([
  "bogus",
  "Done",
  "in-progress",
])("an unknown ?status=%s leaves the table standing, with the value in the filter", async (status) => {
  const user = userEvent.setup();
  table([issue("CORE-1"), issue("CORE-2")], `?status=${status}`);
  expect(await screen.findByRole("table", { name: "Project issues table" })).toBeDefined();
  expect(screen.getByRole("searchbox", { name: "Search issues" })).toBeDefined();
  await user.click(screen.getByRole("button", { name: /^Filter Status/ }));
  expect(screen.getAllByRole("option", { name: new RegExp(`^${status}`) }).length).toBeGreaterThan(
    0
  );
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

// R4: Start next keeps its own set, the Todo issues labelled legion, and its texts name it. A
// selection with none of the set is refused, and nothing is sent.
test("Start next on a selection with no Todo issue labelled legion is refused with a text that names the set", async () => {
  const user = userEvent.setup();
  const send = spyOn(api, "send").mockResolvedValue(savedIssue("CORE-1") as never);
  restores.push(() => send.mockRestore());
  table([
    issue("CORE-1", { labels: ["legion"], rank: "a" }),
    issue("CORE-2", { rank: "b" }),
    issue("CORE-3", { labels: ["legion"], status: "testing", rank: "c" }),
  ]);
  await user.click(await screen.findByRole("checkbox", { name: "Select CORE-2" }));
  await user.click(screen.getByRole("checkbox", { name: "Select CORE-3" }));
  await user.click(
    within(screen.getByRole("toolbar")).getByRole("button", { name: /^Start next$/ })
  );
  const region = screen.getByRole("region", { name: "Project issues" });
  expect((await within(region).findByRole("alert")).textContent).toBe(
    "Start next moves only Todo issues labelled legion."
  );
  expect(screen.queryByRole("dialog", { name: "Start these next?" })).toBeNull();
  expect(send).not.toHaveBeenCalled();
});

// R4: in a mixed selection the confirm says how many selected issues are outside Start next's set
// and stay where they are, and the issues of the set move.
test("Start next on a mixed selection names how many selected issues stay where they are", async () => {
  const user = userEvent.setup();
  const send = spyOn(api, "send").mockResolvedValue(savedIssue("CORE-2") as never);
  restores.push(() => send.mockRestore());
  table([
    issue("CORE-1", { labels: ["legion"], rank: "a" }),
    issue("CORE-2", { labels: ["legion"], rank: "b" }),
    issue("CORE-3", { rank: "c" }),
    issue("CORE-4", { labels: ["legion"], status: "testing", rank: "d" }),
  ]);
  for (const key of ["CORE-2", "CORE-3", "CORE-4"])
    await user.click(await screen.findByRole("checkbox", { name: `Select ${key}` }));
  await user.click(
    within(screen.getByRole("toolbar")).getByRole("button", { name: /^Start next$/ })
  );
  const asked = await screen.findByRole("dialog", { name: "Start these next?" });
  expect(asked.textContent).toContain(
    "1 issue moves ahead of the other Todo issues labelled legion. Dispatch sends one change for each issue. 2 selected issues are not Todo issues labelled legion. They stay where they are."
  );
  expect(send).not.toHaveBeenCalled();
  await user.click(within(asked).getByRole("button", { name: /^Start next$/ }));
  await waitFor(() => expect(send).toHaveBeenCalledTimes(1));
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

// The List draws its toolbar while it reads issues it has not loaded, as the Board draws its
// controls: a press in the toolbar that starts that read leaves the toolbar where it was, with
// the keyboard focus in it, and the skeleton stands in for the table alone. **Clear filters**
// takes every chip off with it and goes itself, since nothing then differs from the view's own,
// and hands its focus to the row's filter trigger, which is the rule for it on every page
// (`components/ControlRow.tsx`); what must not happen is the focus dropping to the page's body.
test("a press in the toolbar that sends the List off to read its issues keeps the toolbar, and the focus in it", async () => {
  const user = userEvent.setup();
  // Opened on the closed issues, whose listing answers. The open issues' listing is still out
  // when Clear filters goes back to them.
  const open = Promise.withResolvers<IssueSummary[]>();
  table([issue("CORE-1"), issue("CORE-2", { status: "done" })], "?status=done", {
    open: open.promise,
  });
  await screen.findByRole("row", { name: "CORE-2 Work CORE-2" });
  await user.click(screen.getByRole("button", { name: "Clear filters" }));
  expect(await screen.findByRole("status", { name: "Loading issues" })).toBeDefined();
  expect(screen.queryByRole("table", { name: "Project issues table" })).toBeNull();
  expect(screen.getByRole("searchbox", { name: "Search issues" })).toBeDefined();
  expect(screen.queryByRole("button", { name: "Clear filters" })).toBeNull();
  const trigger = screen.getByRole("button", { name: /^Filters · \d+$/ });
  // Not the page's body, from which a keyboard reader would start again at its first link.
  expect(trigger).toHaveFocus();
  open.resolve([issue("CORE-1")]);
  expect(await screen.findByRole("row", { name: "CORE-1 Work CORE-1" })).toBeDefined();
  expect(trigger).toHaveFocus();
});

// A column filter goes with the table it heads when its press sends the List off to read its
// issues. The pick is the pointer's, whose option keeps the focus in the list's text box, and the
// List stands in the shell's main content: the press inside the list's panel is the reader's own,
// armed at the List's section (`armOnPress`), so the List's own hand-off walks back over the main
// content from the place the panel was opened from. Its walk back passes over the search box,
// which takes typed text, to the toolbar's Clear filters, and never sends the focus out of the main
// content to the shell's navigation.
test("a column filter press that sends the List off to read its issues leaves the focus on the toolbar", async () => {
  const user = userEvent.setup();
  // Opened on Done, whose listing answers. Ticking Todo asks for the open issues too, whose
  // listing is still out.
  const open = Promise.withResolvers<IssueSummary[]>();
  table([issue("CORE-1"), issue("CORE-2", { status: "done" })], "?status=done", {
    open: open.promise,
    inMain: true,
  });
  await screen.findByRole("row", { name: "CORE-2 Work CORE-2" });
  await user.click(screen.getByRole("button", { name: /^Filter Status/ }));
  expect(await screen.findByRole("combobox", { name: "Search Status" })).toHaveFocus();
  await user.click(screen.getByRole("option", { name: /^Todo/ }));
  expect(await screen.findByRole("status", { name: "Loading issues" })).toBeDefined();
  expect(screen.queryByRole("table", { name: "Project issues table" })).toBeNull();
  await afterFrameAndTasks();
  // Not the page's body, from which a keyboard reader would start again at its first link, nor
  // that link itself, outside the main content, nor the search box.
  const clear = screen.getByRole("button", { name: "Clear filters" });
  expect(clear).toHaveFocus();
  expect(screen.getByRole("link", { name: "Inbox" })).not.toHaveFocus();
  open.resolve([issue("CORE-1")]);
  expect(await screen.findByRole("row", { name: "CORE-1 Work CORE-1" })).toBeDefined();
  expect(clear).toHaveFocus();
});

// A row's own Start next is held (`aria-disabled`, `heldPress`) from its press until the list
// read again moves its issue up the queue and takes the Start next away: it keeps the focus while
// its write is out, and its going is the reader's doing, so the focus goes to a control of that
// row, never to the table's box. With the hold ended at the write's answer, before that read, the
// Start next's going read as a live update's, and the focus went to the box.
test("a row's Start next stays held until the list read again takes it away, and its row keeps the focus", async () => {
  const user = userEvent.setup();
  const send = spyOn(api, "send").mockResolvedValue(savedIssue("CORE-2") as never);
  restores.push(() => send.mockRestore());
  const { listing } = table(
    [
      issue("CORE-1", { labels: ["legion"], rank: "B" }),
      issue("CORE-2", { labels: ["legion"], rank: "C" }),
    ],
    "",
    { inMain: true }
  );
  const row = await screen.findByRole("row", { name: "CORE-2 Work CORE-2" });
  const startNext = within(row).getByRole("button", { name: "Start next" });
  // The read after the write, held until the test answers it with CORE-2 at the queue's head.
  const reread = Promise.withResolvers<IssueSummary[]>();
  const reads = listing.mock.calls.length;
  listing.mockImplementation(() => reread.promise);
  act(() => startNext.focus());
  await user.keyboard("{Enter}");
  await waitFor(() => expect(send).toHaveBeenCalledTimes(1));
  await waitFor(() => expect(listing.mock.calls.length).toBeGreaterThan(reads));
  await afterFrameAndTasks();
  expect(startNext).toHaveAttribute("aria-disabled", "true");
  expect(startNext).toHaveFocus();

  await act(async () =>
    reread.resolve([
      issue("CORE-1", { labels: ["legion"], rank: "B" }),
      issue("CORE-2", { labels: ["legion"], rank: "A" }),
    ])
  );
  const moved = () => screen.getByRole("row", { name: "CORE-2 Work CORE-2" });
  await waitFor(() =>
    expect(within(moved()).queryByRole("button", { name: "Start next" })).toBeNull()
  );
  await afterFrameAndTasks();
  expect(moved().contains(document.activeElement)).toBe(true);
  expect(screen.getByTestId("issues-table-scroll")).not.toHaveFocus();
});

// A row's Start next whose write Dispatch refuses changed nothing, so the list read again after it
// has nothing to take away: the Start next is free again at the refusal, while that read is still
// out, rather than held through it.
test("a row's Start next that Dispatch refuses is free again before the list read again answers", async () => {
  const user = userEvent.setup();
  const send = spyOn(api, "send").mockRejectedValue(
    new ApiError(403, { error: "Rank refused", code: "FORBIDDEN" })
  );
  restores.push(() => send.mockRestore());
  const { listing } = table([
    issue("CORE-1", { labels: ["legion"], rank: "B" }),
    issue("CORE-2", { labels: ["legion"], rank: "C" }),
  ]);
  const row = await screen.findByRole("row", { name: "CORE-2 Work CORE-2" });
  const startNext = within(row).getByRole("button", { name: "Start next" });
  // The read after the refusal, held until the test answers it with the queue unchanged.
  const reread = Promise.withResolvers<IssueSummary[]>();
  const reads = listing.mock.calls.length;
  listing.mockImplementation(() => reread.promise);
  act(() => startNext.focus());
  await user.keyboard("{Enter}");
  expect((await screen.findByRole("alert")).textContent).toContain("Rank refused");
  await waitFor(() => expect(listing.mock.calls.length).toBeGreaterThan(reads));
  expect(send).toHaveBeenCalledTimes(1);
  expect(startNext).not.toHaveAttribute("aria-disabled");

  await act(async () =>
    reread.resolve([
      issue("CORE-1", { labels: ["legion"], rank: "B" }),
      issue("CORE-2", { labels: ["legion"], rank: "C" }),
    ])
  );
  expect(
    within(screen.getByRole("row", { name: "CORE-2 Work CORE-2" })).getByRole("button", {
      name: "Start next",
    })
  ).not.toHaveAttribute("aria-disabled");
});

// `DispatchQueryClient` answers the invalidation of the idle list after an action with the list's
// own fetch, which TanStack pauses while the browser is offline, and no request deadline runs
// while a fetch is paused. So the List's hold through that read has a timer of its own, the
// request deadline (`REQUEST_DEADLINE_MS`): a row's Start next whose write landed offline is free
// again then, and the notice says, after what the action did, that the list is not read yet and
// why. Once the network returns, the list is read and the notice says what the action did alone.
test("offline, a row's Start next whose write landed is free again at the request deadline, and the notice says why", async () => {
  const online = onlineManager.isOnline();
  // The write's answer, held until the case moves the clock, so what it sets off runs in `act`.
  const answer = Promise.withResolvers<Issue>();
  const send = spyOn(api, "send").mockImplementation(() => answer.promise as never);
  restores.push(() => send.mockRestore());
  const client = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  const { listing } = table(
    [
      issue("CORE-1", { labels: ["legion"], rank: "B" }),
      issue("CORE-2", { labels: ["legion"], rank: "C" }),
    ],
    "",
    { client }
  );
  const row = await screen.findByRole("row", { name: "CORE-2 Work CORE-2" });
  const startNext = within(row).getByRole("button", { name: "Start next" });
  const reads = listing.mock.calls.length;
  jest.useFakeTimers();
  try {
    const user = userOnFakeClock();
    onlineManager.setOnline(false);
    act(() => startNext.focus());
    await user.keyboard("{Enter}");
    await tick(0);
    expect(send).toHaveBeenCalledTimes(1);
    answer.resolve(savedIssue("CORE-2"));
    await tick(1_000);
    expect(
      client
        .getQueryCache()
        .findAll({ queryKey: ["issues"] })
        .some((query) => query.state.fetchStatus === "paused")
    ).toBe(true);
    expect(startNext).toHaveAttribute("aria-disabled", "true");

    // The write landed within the second before, which is when the hold's timer started.
    await tick(REQUEST_DEADLINE_MS - 2_000);
    expect(startNext).toHaveAttribute("aria-disabled", "true");

    await tick(2_000);
    expect(startNext).not.toHaveAttribute("aria-disabled");
    expect(listing.mock.calls.length).toBe(reads);
    expect(screen.getByRole("alert").textContent).toBe(
      "CORE-2 “Work CORE-2” now starts next. Could not load issues. Dispatch could not be reached."
    );

    act(() => onlineManager.setOnline(true));
    await tick(0);
    expect(listing.mock.calls.length).toBeGreaterThan(reads);
    expect(screen.getByText("CORE-2 “Work CORE-2” now starts next.")).toHaveAttribute(
      "role",
      "status"
    );
    expect(screen.queryByRole("alert")).toBeNull();
  } finally {
    onlineManager.setOnline(online);
    realClock();
  }
});

// Offline, the bar's Start next lands its write, and the list read after it is paused until
// the network returns. The action holds the bar through that read until the request deadline
// (`REQUEST_DEADLINE_MS`), counted from the write's landing, which is past the write deadline
// counted from the press. The read is part of the press's action, so the bar's going then is the
// reader's doing: the bar goes in the commit that lets the action's controls go, and its hand-off
// puts the focus on the checkbox of the issue marked last, which the list still holds. From the
// press to that commit the focus stays on Start next, and it is never on the page's body.
test("offline, Start next from the bar whose list read never settles hands the focus to the marked issue's checkbox at the request deadline, never to the body", async () => {
  const online = onlineManager.isOnline();
  // The write's answer, held until the case moves the clock, so what it sets off runs in `act`.
  const answer = Promise.withResolvers<Issue>();
  const send = spyOn(api, "send").mockImplementation(() => answer.promise as never);
  restores.push(() => send.mockRestore());
  const client = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  table(queuedIssues(2), "", { client, inMain: true });
  await userEvent.setup().click(await screen.findByRole("checkbox", { name: "Select CORE-2" }));
  const bar = () => screen.queryByRole("toolbar", { name: "Selected issues" });
  const startNext = within(bar() as HTMLElement).getByRole("button", { name: "Start next" });
  jest.useFakeTimers();
  try {
    const user = userOnFakeClock();
    onlineManager.setOnline(false);
    act(() => startNext.focus());
    await user.keyboard("{Enter}");
    await tick(0);
    expect(send).toHaveBeenCalledTimes(1);
    answer.resolve(savedIssue("CORE-2"));
    // Where the focus is at every half second from the press while the bar stays, until 2 s past
    // the request deadline at the most.
    const focused = new Set<string>();
    for (let waited = 0; bar() !== null && waited <= REQUEST_DEADLINE_MS + 2_000; waited += 500) {
      focused.add(focusedName());
      await tick(500);
    }
    expect(bar()?.textContent ?? "no bar").toBe("no bar");
    expect([...focused]).toEqual(["Start next"]);
    expect(focusedName()).toBe("Select CORE-2");
    // Half a second on, the focus is still there.
    await tick(500);
    expect(focusedName()).toBe("Select CORE-2");

    act(() => onlineManager.setOnline(true));
    await tick(0);
  } finally {
    onlineManager.setOnline(online);
    realClock();
  }
});

// INFRA-3135, round 29 (the architect's red team of 2026-11-26 07:3xZ, F1): the List names the
// rows its action changed as leaving while the action or the read after it is out
// (`useIssueTableActions`'s `leaving`), and its bar's hand-off reads that name. Offline, the read
// after Start next outlives its bound, so the bar goes at the request deadline while the issue
// moved is still listed: the landing is owed then, and the commit of the read that drops that
// issue makes it, on the row now at its place. A landing made at the deadline would stand on the
// issue the read drops, and its going would drop the focus onto the page's body.
test("offline, Start next from the bar whose list read outlives its bound lands on the row at the issue's place once the read drops it", async () => {
  const online = onlineManager.isOnline();
  const answer = Promise.withResolvers<Issue>();
  const send = spyOn(api, "send").mockImplementation(() => answer.promise as never);
  restores.push(() => send.mockRestore());
  const client = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  const rows = queuedIssues(4);
  const { listing } = table(rows, "", { client, inMain: true });
  await userEvent.setup().click(await screen.findByRole("checkbox", { name: "Select CORE-3" }));
  const bar = () => screen.queryByRole("toolbar", { name: "Selected issues" });
  const startNext = within(bar() as HTMLElement).getByRole("button", { name: "Start next" });
  jest.useFakeTimers();
  try {
    const user = userOnFakeClock();
    onlineManager.setOnline(false);
    act(() => startNext.focus());
    await user.keyboard("{Enter}");
    await tick(0);
    expect(send).toHaveBeenCalledTimes(1);
    answer.resolve(savedIssue("CORE-3"));
    for (let waited = 0; bar() !== null && waited <= REQUEST_DEADLINE_MS + 2_000; waited += 500) {
      await tick(500);
    }
    expect(bar()?.textContent ?? "no bar").toBe("no bar");
    // The read is still paused, so the issue moved is still listed.
    expect(screen.queryByRole("checkbox", { name: "Select CORE-3" })).not.toBeNull();

    // The network returns, and the read no longer lists CORE-3.
    listing.mockImplementation(async () => rows.filter(({ key }) => key !== "CORE-3"));
    act(() => onlineManager.setOnline(true));
    for (
      let waited = 0;
      screen.queryByRole("checkbox", { name: "Select CORE-3" }) !== null && waited <= 5_000;
      waited += 250
    ) {
      await tick(250);
    }
    expect(screen.queryByRole("checkbox", { name: "Select CORE-3" })).toBeNull();
    expect(focusedName()).toBe("Select CORE-4");
  } finally {
    onlineManager.setOnline(online);
    realClock();
  }
});

/** Gives each drawn row of the List its measured height so the 600 px virtualizer window draws
 *  only the rows near its scroll position, and restores the descriptor after the case. */
function sizeVirtualRows(linePx = 64): void {
  const descriptor = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "offsetHeight");
  Object.defineProperty(HTMLElement.prototype, "offsetHeight", {
    configurable: true,
    get() {
      return this.dataset.index === undefined ? (descriptor?.get?.call(this) ?? 0) : linePx;
    },
  });
  restores.push(() => {
    if (descriptor) Object.defineProperty(HTMLElement.prototype, "offsetHeight", descriptor);
    else Reflect.deleteProperty(HTMLElement.prototype, "offsetHeight");
  });
}

/** Moves the List's scroll box to `top` and fires `scroll`, as a browser does when scrolling the
 *  virtualizer's window. */
function scrollListBoxTo(box: HTMLElement, top: number): void {
  act(() => {
    box.scrollTop = top;
    box.dispatchEvent(new Event("scroll"));
  });
}

// INFRA-3135, round 29 (F3): when the table's landing on a listed, undrawn row is pending
// (`DataTableModel.focusRow`), a wheel of the reader's own cancels it before the row is drawn,
// so drawing the row later never pulls the focus back to it.
test("a wheel while the List's table is drawing an undrawn row cancels its pending landing", async () => {
  sizeVirtualRows();
  const user = userEvent.setup();
  const rows = Array.from({ length: 50 }, (_, index) => issue(`CORE-${index + 1}`));
  table(rows, "", { inMain: true });
  await screen.findByRole("checkbox", { name: "Select CORE-1" });
  const box = tableBox();
  if (box === null) throw new Error("The List drew no box for its rows");
  // Hold `scrollTo` as a browser's smooth/async scroll: the virtualizer asks to scroll back to
  // CORE-1, and the scroll event that draws CORE-1 comes only after the reader's wheel.
  const scrolledTo: number[] = [];
  const scrollToSpy = spyOn(box, "scrollTo").mockImplementation(
    (options?: ScrollToOptions | number, y?: number) => {
      const top = typeof options === "number" ? y : options?.top;
      if (top !== undefined) scrolledTo.push(top);
    }
  );
  restores.push(() => scrollToSpy.mockRestore());

  // Mark CORE-1, then scroll far enough down that the virtualizer drops CORE-1 from the DOM.
  await user.click(screen.getByRole("checkbox", { name: "Select CORE-1" }));
  scrollListBoxTo(box, 2_500);
  expect(screen.queryByRole("checkbox", { name: "Select CORE-1" })).toBeNull();

  // Clear selection from the bar: CORE-1 is listed but undrawn, so `drawRow` asks `focusRow` to
  // scroll to it and owes its canceller while the table's own stop holds the focus.
  const clear = within(screen.getByRole("toolbar", { name: "Selected issues" })).getByRole(
    "button",
    { name: "Clear selection" }
  );
  act(() => clear.focus());
  await user.keyboard("{Enter}");
  await waitFor(() => expect(screen.queryByRole("toolbar")).toBeNull());
  expect(scrolledTo.length).toBeGreaterThan(0);
  expect(screen.queryByRole("checkbox", { name: "Select CORE-1" })).toBeNull();
  expect(focusedName()).toBe("Project issues table");

  // The reader's own wheel arrives before CORE-1 is drawn and cancels the pending landing.
  document.dispatchEvent(new WheelEvent("wheel", { bubbles: true }));

  // Even when the box scrolls back and draws CORE-1 later, focus never goes to CORE-1.
  scrollListBoxTo(box, scrolledTo.at(-1) ?? 0);
  expect(screen.getByRole("checkbox", { name: "Select CORE-1" })).toBeInTheDocument();
  expect(focusedName()).not.toBe("Select CORE-1");
  expect(focusedName()).toBe("Project issues table");
});

// INFRA-3135, round 29 (N1): offline, a table action whose list read outlives its bound drops the
// bar while the changed rows are still named as leaving (state Owed). When the read settles with
// the row marked last still listed but outside the virtualizer's drawn window, the delayed landing
// asks the table to draw that row and lands on its checkbox, never staying on the table's stop or
// dropping to the body.
test("offline, Start next from the bar whose list read outlives its bound lands on an undrawn marked row once the read settles", async () => {
  sizeVirtualRows();
  const online = onlineManager.isOnline();
  const answer = Promise.withResolvers<Issue>();
  const send = spyOn(api, "send").mockImplementation(() => answer.promise as never);
  restores.push(() => send.mockRestore());
  const client = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  // Start next keeps CORE-2 listed after the read settles.
  const rows = queuedIssues(50);
  const { listing } = table(rows, "", { client, inMain: true });
  await screen.findByRole("checkbox", { name: "Select CORE-2" });
  const box = tableBox();
  if (box === null) throw new Error("The List drew no box for its rows");
  const scrolledTo: number[] = [];
  const scrollToSpy = spyOn(box, "scrollTo").mockImplementation(
    (options?: ScrollToOptions | number, y?: number) => {
      const top = typeof options === "number" ? y : options?.top;
      if (top !== undefined) scrolledTo.push(top);
    }
  );
  restores.push(() => scrollToSpy.mockRestore());

  await userEvent.setup().click(screen.getByRole("checkbox", { name: "Select CORE-2" }));
  // Scroll down so CORE-2 leaves the virtualizer's drawn window before the action starts.
  scrollListBoxTo(box, 2_500);
  expect(screen.queryByRole("checkbox", { name: "Select CORE-2" })).toBeNull();

  const bar = () => screen.queryByRole("toolbar", { name: "Selected issues" });
  const startNext = within(bar() as HTMLElement).getByRole("button", { name: "Start next" });
  jest.useFakeTimers();
  try {
    const user = userOnFakeClock();
    onlineManager.setOnline(false);
    act(() => startNext.focus());
    await user.keyboard("{Enter}");
    await tick(0);
    expect(send).toHaveBeenCalledTimes(1);
    answer.resolve(savedIssue("CORE-2"));
    const focusedWhileWaiting = new Set<string>();
    for (let waited = 0; bar() !== null && waited <= REQUEST_DEADLINE_MS + 2_000; waited += 500) {
      focusedWhileWaiting.add(focusedName());
      await tick(500);
    }
    expect(bar()?.textContent ?? "no bar").toBe("no bar");
    expect(focusedWhileWaiting).not.toContain("the body");
    // The read is still paused, so CORE-2 is still named as leaving (Owed), and nothing has asked
    // the table to scroll to CORE-2 yet.
    expect(scrolledTo).toEqual([]);
    expect(focusedName()).toBe("Project issues table");

    // The network returns and the read settles with CORE-2 still listed and still undrawn. The
    // delayed landing calls `drawRow`, which scrolls the box to draw CORE-2.
    listing.mockImplementation(async () => rows);
    act(() => onlineManager.setOnline(true));
    for (let waited = 0; scrolledTo.length === 0 && waited <= 5_000; waited += 250) {
      await tick(250);
    }
    expect(scrolledTo.length).toBeGreaterThan(0);
    // The box scrolls to where the virtualizer asked and draws CORE-2; `focusRow` lands on it.
    scrollListBoxTo(box, scrolledTo.at(-1) ?? 0);
    expect(screen.getByRole("checkbox", { name: "Select CORE-2" })).toBeInTheDocument();
    expect(focusedName()).toBe("Select CORE-2");
  } finally {
    onlineManager.setOnline(online);
    realClock();
  }
});

// INFRA-3135, round 29 (the architect's ruling of 2026-11-26 07:42Z, and the red team's (iv)): a
// table action that saved one of its changes and was refused the other keeps its selection, so its
// bar stays and no hand-off runs then. It changed the list all the same, so the List names the
// issue it saved as leaving, but only while the action and the read after it are out
// (`useIssueTableActions`'s `leaving`): the issue is still listed once they are over, and is
// staying. So when the reader then clears the selection, nothing is leaving: the landing is made
// at once on the checkbox of the issue marked last, and nothing is owed after it, so a wheel of
// theirs moves no focus. A name that outlived the action would owe that landing instead, and the
// wheel would take the focus off the checkbox the hand-off parked it on.
test("after a partly refused Start next, Clear selection lands on the issue marked last and owes nothing", async () => {
  const user = userEvent.setup();
  answerIssuePatches().mockImplementation(async (key) => {
    if (key === "CORE-3") return savedIssue(key);
    throw new ApiError(403, { error: "Rank refused", code: "FORBIDDEN" });
  });
  table(queuedIssues(3), "", { inMain: true });
  await user.click(await screen.findByRole("checkbox", { name: "Select CORE-3" }));
  await user.click(screen.getByRole("checkbox", { name: "Select CORE-2" }));
  const bar = () => screen.getByRole("toolbar", { name: "Selected issues" });
  await user.click(within(bar()).getByRole("button", { name: "Start next" }));
  expect((await screen.findByRole("alert")).textContent).toContain("Did not move up: CORE-2.");
  // The action is over once it lets its controls go, after the read of the list that draws the
  // change it saved. The issue it saved is the one it named as moved.
  const clear = () => within(bar()).getByRole("button", { name: "Clear selection" });
  await waitFor(() => expect(clear().getAttribute("aria-disabled")).not.toBe("true"));
  expect(screen.getByRole("checkbox", { name: "Select CORE-3" }).closest("tr")?.dataset.moved).toBe(
    "true"
  );
  expect(screen.getByRole<HTMLInputElement>("checkbox", { name: "Select CORE-2" }).checked).toBe(
    true
  );

  await user.click(clear());
  // The landing waits for the foot's own pass alone (`hooks/useFootInset.ts`), not for a name.
  await afterFrameAndTasks();
  expect(screen.queryByRole("toolbar", { name: "Selected issues" })).toBeNull();
  expect(focusedName()).toBe("Select CORE-2");
  document.dispatchEvent(new WheelEvent("wheel", { bubbles: true }));
  await afterFrameAndTasks();
  expect(focusedName()).toBe("Select CORE-2");
});

// INFRA-3135, round 31 (the tester's row of `e2e/table-controls.e2e.ts` for the project List
// leaving the focus on a control of the list, never the page body, once the rows leave): the List
// names each issue an action's writes are for as leaving from the reader's press
// (`useIssueTableActions`'s `leaving`), not from its write's answer. The reader marks two rows,
// the lower first, presses the bar's Start next, and unticks both marks while its first write is
// out, so the bar goes by their own untick. A click that leaves the focus on the mark, as
// Chromium's and Firefox's do, keeps it the reader's: the bar hands on nothing (`SelectionBar`),
// and the hand-off owes the landing itself because the focus stands in a row the List names
// (`useLastMarkedPlace`). A click that leaves the focus where it was, on Start next, as WebKit's
// does, drops it onto the body with the bar: the bar's hand-off owes the landing and parks the
// focus on the mark of the row marked last. Each way the focus stands on that mark while the
// writes are out. Once they are let through, one after the other as Start next sends them, the
// list read after them takes both rows out of the view, and the focus goes to the checkbox of
// the row now at their place, never to the page's body. A List that named a row only once its
// write had answered named neither at the bar's going, so the landing was made at once on the
// mark, and the row's going then dropped the focus onto the body.
for (const [how, untick] of [
  [
    "a click that leaves the focus on the mark",
    (user: UserEvent, mark: HTMLElement) => user.click(mark),
  ],
  [
    "a click that leaves the focus where it was, as WebKit's does",
    async (_user: UserEvent, mark: HTMLElement) =>
      act(() => {
        mark.dispatchEvent(new PointerEvent("pointerdown", { bubbles: true }));
        mark.click();
      }),
  ],
] as const) {
  test(`with Start next's writes held and both marks then unticked by ${how}, the List names the rows from the press and the focus ends on the checkbox of the row now at their place`, async () => {
    const user = userEvent.setup();
    const rows = queuedIssues(21);
    // Each write, in the order Start next sends them, held until the case lets it through: then
    // the listing holds that issue out, and the write answers. Start next sends the second write
    // once the first is answered. The list read after the last answer, which holds neither issue,
    // takes the two rows out of the view together.
    const held: (() => void)[] = [];
    const send = spyOn(api, "send").mockImplementation((request) => {
      const key = request.path.split("/").at(-1) ?? "";
      const answer = Promise.withResolvers<Issue>();
      held.push(() => {
        rows.splice(
          rows.findIndex((row) => row.key === key),
          1
        );
        answer.resolve(savedIssue(key));
      });
      return answer.promise as never;
    });
    restores.push(() => send.mockRestore());
    table(rows, "", { inMain: true });
    await screen.findByRole("checkbox", { name: "Select CORE-21" });
    // The two rows the reader marks, the upper one last, and the row after them.
    const order = drawnRows().map((row) => row.getAttribute("data-issue-row") ?? "");
    const [upper = "", lower = "", next = ""] = order.slice(11, 14);
    // Each row's checkbox is drawn where its row stands now, 100 px a row, with the tenth row at
    // the window's top: happy-dom lays nothing out, and the hand-off reads what the reader sees.
    restores.push(layRowsOut("[data-issue-row]", 9));
    const marks = [lower, upper].map((key) =>
      screen.getByRole("checkbox", { name: `Select ${key}` })
    );
    for (const mark of marks) await user.click(mark);
    act(() =>
      within(screen.getByRole("toolbar", { name: "Selected issues" }))
        .getByRole("button", { name: "Start next" })
        .focus()
    );
    await user.keyboard("{Enter}");
    await waitFor(() => expect(held).toHaveLength(1));
    for (const mark of marks) await untick(user, mark);
    await waitFor(() => expect(screen.queryByRole("toolbar")).not.toBeInTheDocument());
    expect(focusedName()).toBe(`Select ${upper}`);
    for (const step of [0, 1]) {
      await waitFor(() => expect(held).toHaveLength(step + 1));
      await act(async () => held[step]?.());
    }
    await waitFor(() =>
      expect(screen.queryByRole("checkbox", { name: `Select ${upper}` })).toBeNull()
    );
    expect(screen.queryByRole("checkbox", { name: `Select ${lower}` })).toBeNull();
    await waitFor(() => expect(focusedName()).toBe(`Select ${next}`));
    await afterFrameAndTasks();
    expect(focusedName()).toBe(`Select ${next}`);
  });
}

// INFRA-3135, round 31: the name of an issue a Finish sends a write again for ends at once where
// that write is refused or stopped again, as a write the action sends itself does (`apply`). Start
// next over CORE-2 is refused, so it is left to be finished and keeps its selection: the bar stays
// over CORE-2. Finish sends the refused rank write again and that write is stopped, as the reader's
// stop in Pending changes ends it; a stopped write may have landed, so the Finish holds through the
// list read after it, which the case holds. With the focus in the bar, the reader unticks
// CORE-2 by a click that leaves the focus where it was, as WebKit's does, and the bar goes with it.
// Nothing is leaving, so the landing is made on CORE-2's checkbox and nothing is owed after it: a
// wheel of the reader's moves no focus. A name that outlived the stopped write owed that landing
// instead, and parked the focus meanwhile.
test("a write Finish sends again that is stopped ends its issue's name at once, so the bar's going lands on the issue marked last and owes nothing", async () => {
  const user = userEvent.setup();
  const rows = [
    issue("CORE-2", { labels: ["legion"], rank: "b" }),
    issue("CORE-3", { labels: ["legion"], rank: "a" }),
  ];
  // The first rank write is refused, and the one Finish sends again is stopped.
  let patches = 0;
  answerIssuePatches().mockImplementation(async () => {
    patches += 1;
    if (patches === 1) throw new ApiError(400, { code: "REFUSED_ONCE", error: "refused once" });
    throw new SendStoppedError();
  });
  const { listing } = table(rows, "", { inMain: true });
  const mark = await screen.findByRole("checkbox", { name: "Select CORE-2" });
  await user.click(mark);
  const bar = () => screen.queryByRole("toolbar", { name: "Selected issues" });
  await user.click(within(bar() as HTMLElement).getByRole("button", { name: /^Start next$/ }));
  await screen.findByRole("alert");
  await afterFrameAndTasks();
  // The list read after the Finish, held: the Finish is out until it answers.
  const reread = Promise.withResolvers<IssueSummary[]>();
  listing.mockImplementation(() => reread.promise);
  await pressFinish(user);
  await waitFor(() => expect(patches).toBe(2));
  await afterFrameAndTasks();
  const clear = within(bar() as HTMLElement).getByRole("button", { name: "Clear selection" });
  expect(clear.getAttribute("aria-disabled")).toBe("true");
  act(() => clear.focus());
  act(() => {
    mark.dispatchEvent(new PointerEvent("pointerdown", { bubbles: true }));
    mark.click();
  });
  await afterFrameAndTasks();
  expect(bar()).toBeNull();
  expect(focusedName()).toBe("Select CORE-2");
  document.dispatchEvent(new WheelEvent("wheel", { bubbles: true }));
  await afterFrameAndTasks();
  expect(focusedName()).toBe("Select CORE-2");
  await act(async () => reread.resolve([...rows]));
});

// A notice's button that takes the notice away hands the focus on, never to the page's body: to
// the control now at its place among the List's notices, else the one before it, else, by the
// List's own walk back over the main content, to the toolbar's nearest control before the notices
// that takes no typed text, and never into the search box. On the Issues tab that is the row's
// display menu at its right end, which the tab names Group by (R9).
test.each([
  "Finish",
  "Dismiss",
])("the interrupted action's %s hands its focus to the toolbar's Group by menu, not the search box", async (press) => {
  const user = userEvent.setup();
  localStorage.setItem(
    "dispatch.project-table.CORE",
    JSON.stringify({
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
  table([issue("CORE-1")], "", { inMain: true });
  const recovery = await screen.findByRole("region", { name: "Interrupted table action" });
  within(recovery).getByRole("button", { name: press }).focus();
  await user.keyboard("{Enter}");
  await waitFor(() =>
    expect(screen.queryByRole("region", { name: "Interrupted table action" })).toBeNull()
  );
  await afterFrameAndTasks();
  expect(screen.getByRole("button", { name: "Group by" })).toHaveFocus();
});

// F6: a page never ends with a heading whose rows all start on the next one, and a heading
// that shows part of its group says how much of it is here.
test.each([
  ["a heading with no rows left on the page is dropped", 24, "bob"],
  ["a heading showing part of its group counts what is here", 20, "3 of 20 issues"],
])("%s", async (_name, first, expected) => {
  const leaves = [
    ...Array.from({ length: first }, (_, index) =>
      issue(`CORE-1${String(index).padStart(2, "0")}`, { assignee: "alice" })
    ),
    ...Array.from({ length: 20 }, (_, index) =>
      issue(`CORE-2${String(index).padStart(2, "0")}`, { assignee: "bob" })
    ),
  ];
  table(leaves, "?group=assignee&size=25");
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
  if (expected === "bob") expect(headings().join(" | ")).not.toContain(expected);
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

// The bulk action runs above the table of the open view: a press on another view draws a new
// table, and a Start next the old one began goes on until every selected issue has moved.
test("a press on Done while Start next writes does not stop it, and every selected issue moves", async () => {
  const user = userEvent.setup();
  const first = Promise.withResolvers<void>();
  const sent: string[] = [];
  const send = spyOn(api, "send").mockImplementation(async (request) => {
    sent.push(request.path);
    if (sent.length === 1) await first.promise;
    return savedIssue(request.path.slice(request.path.lastIndexOf("/") + 1)) as never;
  });
  restores.push(() => send.mockRestore());
  const { client } = table([
    issue("CORE-1", { labels: ["legion"], rank: "a" }),
    issue("CORE-2", { labels: ["legion"], rank: "b" }),
    issue("CORE-3", { labels: ["legion"], rank: "c" }),
    issue("CORE-4", { labels: ["legion"], rank: "d" }),
    issue("CORE-5", { status: "done", closed_at: "2026-11-10T10:00:00Z" }),
  ]);
  for (const key of ["CORE-2", "CORE-3", "CORE-4"])
    await user.click(await screen.findByRole("checkbox", { name: `Select ${key}` }));
  await user.click(
    within(screen.getByRole("toolbar")).getByRole("button", { name: /^Start next$/ })
  );
  await waitFor(() => expect(sent).toHaveLength(1));
  await user.click(screen.getByRole("tab", { name: /^Done/ }));
  await screen.findByRole("row", { name: "CORE-5 Work CORE-5" });
  first.resolve();
  await waitFor(() =>
    expect([...sent].sort()).toEqual([
      "/api/v1/issues/CORE-2",
      "/api/v1/issues/CORE-3",
      "/api/v1/issues/CORE-4",
    ])
  );
  await waitFor(() =>
    expect(
      within(screen.getByRole("region", { name: "Project issues" })).getByRole("status").textContent
    ).toBe("3 issues now start next.")
  );
  expect(writesOf(client).rows()).toEqual([]);
  expect(JSON.parse(localStorage.getItem("dispatch.project-table.CORE") ?? "{}").action).toBe(
    undefined
  );
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

// R1 and the Design's columns: the group heading's next in Legion queue names the Place, the
// Queue column's number, and not Start next's own count of the Todo issues labelled legion. So
// it names the first Place among the group's rows, labelled or not.
test("the Todo group's heading names the first Place in the Legion queue, labelled or not", async () => {
  const sockets = liveSockets(restores);
  table(
    [issue("CORE-1", { labels: ["legion"] }), issue("CORE-2"), issue("CORE-3")],
    "?group=status",
    {},
    {},
    Promise.resolve(DAEMON)
  );
  await screen.findByRole("button", { name: /^Collapse Todo, 3 issues/ });
  await answerBoard(sockets, {
    ...legionBoard(),
    columns: [
      {
        id: "queue",
        label: "Queue",
        cards: [
          { ...waitingCard("CORE-3"), waiting_place: 2 },
          { ...waitingCard("CORE-2"), waiting_place: 3 },
        ],
      },
    ],
  });
  await waitFor(() =>
    expect(screen.getByRole("button", { name: /^Collapse Todo,/ }).textContent).toContain(
      "3 issues · next in Legion queue #2"
    )
  );
});

// A priority's table keeps `next in queue` and Start next's number (`queue`) in its group heading.
test("a priority's table names the next issue in Start next's queue in its group heading", async () => {
  const advancing = { advances: [{ id: "PRI-7", inherited: false }] };
  table(
    [issue("CORE-1", advancing), issue("CORE-2", { ...advancing, labels: ["legion"] })],
    "?w.group=status",
    undefined,
    { listing: { advances: "PRI-7" }, stateKey: "w" }
  );
  const heading = await screen.findByRole("button", { name: /^Collapse Todo, 2 issues/ });
  expect(heading.textContent).toContain("2 issues · next in queue #1");
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
    '{"sizing":{},"order":[],"visibility":{"updated":false,"closed":false,"project":false,"advances":false}}'
  );
  expect(localStorage.getItem("dispatch.project-table.priority:PRI-7")).toBeNull();
});

test("a listing table's stateKey puts its own view under its prefix, and the Issues tab keeps the bare keys", async () => {
  const user = userEvent.setup();
  const advances = [{ id: "ship-it", inherited: false }];
  const { view } = table([issue("CORE-1", { title: "Sextant", advances })], "", undefined, {
    listing: { advances: "ship-it" },
    stateKey: "advances",
  });
  await user.type(await screen.findByRole("searchbox", { name: "Search issues" }), "sextant");
  await waitFor(() =>
    expect(screen.getByTestId("route-search").textContent).toContain("advances.q=sextant")
  );
  expect(screen.getByTestId("route-search").textContent).not.toContain("&q=");
  view.unmount();
  // The Issues tab takes no prefix, so the address it publishes is the bare one.
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
  // Named as every other chip of the row is, and wearing the selected fill they wear.
  const chip = screen.getByRole("button", { name: "Remove Showing: Finished last week filter" });
  expect(chip).toHaveAttribute("aria-pressed", "true");
  expect(chip.textContent).toContain("Finished last week");
  expect(chip.textContent).toContain("2 of 2 issues");
  await user.click(chip);
  await waitFor(() => expect(listedKeys().sort()).toEqual(["CORE-1", "CORE-2", "CORE-3"]));
  expect(screen.getByTestId("route-search").textContent).not.toContain("only=");
  expect(screen.queryByRole("button", { name: /^Remove Showing: / })).toBeNull();
});

// The toolbar is drawn while the issues load and after their read fails, so the chip counts what
// it shows only once the table holds every row it waits on: a count of the empty listing, or of
// the open issues alone while the closed ones are out, would read as picked issues that are not
// there.
test("the narrowing chip counts the picked issues only once the table has loaded them", async () => {
  const picked = "?only=CORE-1,CORE-3&onlyLabel=Finished+last+week&nofilter=1";
  const count = () => {
    const chip = screen.getByRole("button", { name: "Remove Showing: Finished last week filter" });
    expect(chip.textContent).toContain("Showing: Finished last week");
    return chip.textContent?.match(/\d+ of \d+ issues?/)?.[0];
  };
  const open = Promise.withResolvers<IssueSummary[]>();
  const full = Promise.withResolvers<IssueSummary[]>();
  const { view } = table([], picked, { full: full.promise, open: open.promise });
  expect(await screen.findByRole("status", { name: "Loading issues" })).toBeDefined();
  expect(count()).toBeUndefined();
  // The open issues are in and the closed ones still out. One picked issue is closed, so a count
  // now would say one of two.
  open.resolve([issue("CORE-1"), issue("CORE-2")]);
  await screen.findByText("Loading closed issues…");
  expect(count()).toBeUndefined();
  full.resolve([issue("CORE-1"), issue("CORE-2"), issue("CORE-3", { status: "done" })]);
  await waitFor(() => expect(count()).toBe("2 of 2 issues"));
  view.unmount();

  // A read that fails leaves the chip naming its narrowing beside the error, with no count.
  const failing = Promise.withResolvers<IssueSummary[]>();
  table([], picked, {
    full: Promise.withResolvers<IssueSummary[]>().promise,
    open: failing.promise,
  });
  failing.reject(new Error("the listing is down"));
  expect(await screen.findByRole("button", { name: "Retry loading issues" })).toBeDefined();
  expect(count()).toBeUndefined();
});

// R1, the architect's chip rule: every narrowing that is on is visible and removable from the
// control row, whatever set it. A column filter is set on that column's header, and the reader
// may hide the column with the columns menu - which leaves the filter narrowing the list with no
// header to show it - so the row draws a chip for it either way.
test("a column filter is a chip of the row, counted by the trigger, while its column is hidden", async () => {
  const user = userEvent.setup();
  table(
    [
      issue("CORE-1", { labels: ["frontend"] }),
      issue("CORE-2", { labels: ["backend"] }),
      issue("CORE-3", { labels: ["frontend"] }),
    ],
    "?label=frontend"
  );
  await screen.findByRole("row", { name: "CORE-1 Work CORE-1" });
  expect(listedKeys().sort()).toEqual(["CORE-1", "CORE-3"]);
  // One chip for the one narrowing, in the words the Board gives `?label=` too, since both
  // views read that parameter.
  const chip = () => screen.getByRole("button", { name: "Remove Label: frontend filter" });
  expect(screen.getAllByRole("button", { name: /^Remove .* filter$/ }).length).toBe(1);
  expect(chip()).toBeDefined();
  expect(screen.getByRole("button", { name: "Filters · 1" })).toBeDefined();

  // Hidden, the column takes its header and its filter control off the page; the chip is then
  // the one place the narrowing shows, and it still takes it off.
  await user.click(screen.getByRole("button", { name: "Columns" }));
  await user.click(screen.getByRole("checkbox", { name: "Labels" }));
  await user.keyboard("{Escape}");
  await waitFor(() => expect(screen.queryByRole("button", { name: /^Filter Labels/ })).toBeNull());
  expect(listedKeys().sort()).toEqual(["CORE-1", "CORE-3"]);
  expect(screen.getByRole("button", { name: "Filters · 1" })).toBeDefined();
  await user.click(chip());
  await waitFor(() => expect(listedKeys().sort()).toEqual(["CORE-1", "CORE-2", "CORE-3"]));
  expect(screen.getByTestId("route-search").textContent).not.toContain("label=");
  expect(screen.getByRole("button", { name: "Filters · 0" })).toBeDefined();
});

// The statuses the List opens with are no narrowing of the reader's: a link leaves them out and
// Clear filters goes back to them, so they draw no chip. A status the reader picked does, and
// taking that chip off goes back to the open statuses rather than asking the server for every
// closed issue in the project.
test("the open statuses draw no chip, and taking a Status chip off goes back to them", async () => {
  const user = userEvent.setup();
  const { listing } = table([issue("CORE-1"), issue("CORE-2", { status: "done" })], "?status=done");
  await screen.findByRole("row", { name: "CORE-2 Work CORE-2" });
  const chip = screen.getByRole("button", { name: "Remove Status: Done filter" });
  await user.click(chip);
  await waitFor(() =>
    expect(screen.getByTestId("route-search").textContent).not.toContain("status=")
  );
  expect(screen.getByTestId("route-search").textContent).not.toContain("nofilter");
  expect(screen.queryByRole("button", { name: /^Remove Status: / })).toBeNull();
  expect(listing.mock.calls.every(([options]) => options?.open !== false)).toBe(true);
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

// The Issues tab's views: the reads each one makes, the tab counts, the daemon setting, the
// facts of the done rows, Save view and Clear filters.

/** The address the page is on, as the route shows it. */
const routeSearch = () => screen.getByTestId("route-search").textContent ?? "";

/** Open, Done, and the rows each lists: two open issues and two closed ones. */
const VIEW_ROWS = (): IssueSummary[] => [
  issue("CORE-1", { labels: ["legion"] }),
  issue("CORE-2", { status: "in_progress" }),
  issue("CORE-3", { status: "done", closed_at: "2026-11-10T10:00:00Z" }),
  issue("CORE-5", { status: "done", closed_at: "2026-11-11T10:00:00Z" }),
];

/** Every request of the project's listing, page reads included, in one order. */
function listingReads(
  listing: ReturnType<typeof table>["listing"],
  page: ReturnType<typeof table>["page"]
): string[] {
  return [
    ...listing.mock.calls.map(([options]) => `list ${JSON.stringify(options)}`),
    ...page.mock.calls.map(([options]) => `page ${JSON.stringify(options)}`),
  ].sort();
}

const OPEN_COUNT_READ = `page ${JSON.stringify({ project: "CORE", open: true, limit: 1 })}`;

// R5: no view reads the full listing. Open reads the open listing alone; Done reads the page on
// screen while it is sorted by Closed and narrowed by nothing but a Closed window, and every done
// issue its window selects otherwise; a view with no status picked reads the open listing and the
// done issues. On Done, Open's count is the total of a one-row page of the open listing.
test.each([
  ["Open", "", [`list ${JSON.stringify({ project: "CORE", open: true })}`]],
  [
    "Done",
    "?view=done",
    [
      `page ${JSON.stringify({ project: "CORE", status: ["done"], sort: "-closed_at", limit: 50, offset: 0 })}`,
      OPEN_COUNT_READ,
    ],
  ],
  [
    "Done's third page of 25 rows",
    "?view=done&size=25&page=3",
    [
      `page ${JSON.stringify({ project: "CORE", status: ["done"], sort: "-closed_at", limit: 25, offset: 50 })}`,
      OPEN_COUNT_READ,
    ],
  ],
  [
    "Done in a Closed window",
    "?view=done&f.closed=2026-11-03..2026-11-12",
    [
      `page ${JSON.stringify({ project: "CORE", status: ["done"], sort: "-closed_at", limit: 50, offset: 0, closed_since: "2026-11-03T00:00:00.000Z", closed_before: "2026-11-12T00:00:00.000Z" })}`,
      OPEN_COUNT_READ,
    ],
  ],
  [
    "Done with a search",
    "?view=done&q=work",
    [`list ${JSON.stringify({ project: "CORE", status: ["done"] })}`, OPEN_COUNT_READ],
  ],
  [
    "Done in a Closed window, sorted by Key",
    "?view=done&sort=key&f.closed=2026-11-03..2026-11-12",
    [
      `list ${JSON.stringify({ project: "CORE", status: ["done"], closed_since: "2026-11-03T00:00:00.000Z", closed_before: "2026-11-12T00:00:00.000Z" })}`,
      OPEN_COUNT_READ,
    ],
  ],
  [
    "Done grouped by Assignee",
    "?view=done&group=assignee",
    [`list ${JSON.stringify({ project: "CORE", status: ["done"] })}`, OPEN_COUNT_READ],
  ],
  [
    "Done with every row on one page",
    `?view=done&size=${ALL_ROWS}`,
    [`list ${JSON.stringify({ project: "CORE", status: ["done"] })}`, OPEN_COUNT_READ],
  ],
  [
    "a Status filter with no pick",
    "?nofilter=1",
    [
      `list ${JSON.stringify({ project: "CORE", open: true })}`,
      `list ${JSON.stringify({ project: "CORE", status: ["done"] })}`,
    ],
  ],
  [
    "a Status filter of Todo and Done",
    "?status=todo&status=done",
    [
      `list ${JSON.stringify({ project: "CORE", open: true })}`,
      `list ${JSON.stringify({ project: "CORE", status: ["done"] })}`,
    ],
  ],
])("%s makes its own reads and never reads the full listing", async (_name, search, reads) => {
  // Enough closed issues for a third page of 25, so no page asked for is past the last one.
  const closed = Array.from({ length: 60 }, (_, index) =>
    issue(`CORE-${index + 10}`, { status: "done", closed_at: "2026-11-04T10:00:00Z" })
  );
  const { listing, page } = table([...VIEW_ROWS(), ...closed], search);
  await screen.findByRole("table", { name: "Project issues table" });
  await waitFor(() => expect(listingReads(listing, page)).toEqual([...reads].sort()));
  // Every read names `open` or `status`: none is the project's full listing.
  for (const [options] of [...listing.mock.calls, ...page.mock.calls])
    expect(options?.open === true || options?.status !== undefined).toBe(true);
});

// k9: Done sends the list route no window it refuses. A to day before the from day holds no
// moment: Done lists no row and asks for none, and the address and the chip keep the window. A day
// count that reaches back past the first time the route reads holds every close, so Done reads
// every done row with no `closed_since`.
test("Done asks for no window the list route refuses: to before from lists no row, and a count past every date reads every done row", async () => {
  const { listing, page } = table(VIEW_ROWS(), "?view=done&f.closed=2026-11-10..2026-11-03");
  expect(await screen.findByText("No issues match these filters.")).toBeDefined();
  await waitFor(() => expect(listingReads(listing, page)).toEqual([OPEN_COUNT_READ]));
  expect(
    screen.getByRole("button", { name: "Remove Closed: 2026-11-10 to 2026-11-02 filter" })
  ).toBeDefined();
  expect(routeSearch()).toBe("?view=done&f.closed=2026-11-10..2026-11-03");
  expect(screen.queryByText(/Could not load/)).toBeNull();
  // The next page starts over, with spies of its own.
  for (const restore of restores.splice(0)) restore();

  const reads = table(VIEW_ROWS(), "?view=done&f.closed=750000d");
  expect(await screen.findByRole("row", { name: /^CORE-5 / })).toBeDefined();
  expect(screen.getByRole("row", { name: /^CORE-3 / })).toBeDefined();
  await waitFor(() =>
    expect(listingReads(reads.listing, reads.page)).toEqual(
      [
        `page ${JSON.stringify({ project: "CORE", status: ["done"], sort: "-closed_at", limit: 50, offset: 0 })}`,
        OPEN_COUNT_READ,
      ].sort()
    )
  );
});

/** The saved views alice keeps for CORE: Open narrowed to Todo, Done, and every status. */
function keepSavedViews(views: readonly { name: string; query: string }[]) {
  localStorage.setItem(
    "dispatch.project.saved-views:alice",
    JSON.stringify({ version: 1, projects: { CORE: views } })
  );
}

/** alice's saved views of CORE, as stored. */
const savedViews = (): { name: string; query: string }[] =>
  JSON.parse(localStorage.getItem("dispatch.project.saved-views:alice") ?? "{}").projects?.CORE ??
  [];

// R2: each tab counts its view where the page can. Open counts from the open listing, and on
// Done from the total of a one-row page of it; Done counts once it has been opened; a saved view
// counts from the open listing, so on Done it has no count, and one that lists closed issues
// counts only while it is open. The open tab counts the rows the table lists now.
test("the tabs count Open, Done once opened, and the saved views the open listing can count", async () => {
  const user = userEvent.setup();
  // Each query as Save view writes one, with its columns and its layout.
  const drawn = "&cols=key,title,status&layout=table";
  keepSavedViews([
    { name: "Todo", query: `status=todo${drawn}` },
    { name: "Closed", query: `view=done${drawn}` },
    { name: "Everything", query: `nofilter=1${drawn}` },
  ]);
  table(VIEW_ROWS());
  const tab = (name: string) => screen.findByRole("tab", { name });
  expect(await tab("Open, 2 issues")).toHaveAttribute("aria-selected", "true");
  expect(await tab("Done")).toBeDefined();
  expect(await tab("Todo, 1 issue")).toBeDefined();
  expect(await tab("Closed")).toBeDefined();
  expect(await tab("Everything")).toBeDefined();

  // The reader's own search narrows the open tab's count, and marks the view changed.
  await user.type(screen.getByRole("searchbox", { name: "Search issues" }), "CORE-2");
  expect(await tab("Open, 1 issue, changed")).toBeDefined();
  await user.clear(screen.getByRole("searchbox", { name: "Search issues" }));
  expect(await tab("Open, 2 issues")).toBeDefined();

  // Done reads no open row: Open counts by the open listing's total, and no saved view counts.
  await user.click(await tab("Done"));
  expect(await tab("Done, 2 issues")).toHaveAttribute("aria-selected", "true");
  expect(await tab("Open, 2 issues")).toBeDefined();
  expect(await tab("Todo")).toBeDefined();

  // Back on Open, Done keeps the count of its own read.
  await user.click(await tab("Open, 2 issues"));
  expect(await tab("Open, 2 issues")).toHaveAttribute("aria-selected", "true");
  expect(await tab("Done, 2 issues")).toBeDefined();
  expect(await tab("Todo, 1 issue")).toBeDefined();

  // A saved view that lists closed issues counts while it is the open tab.
  await user.click(await tab("Everything"));
  expect(await tab("Everything, 4 issues")).toHaveAttribute("aria-selected", "true");
  await user.click(await tab("Closed"));
  expect(await tab("Closed, 2 issues")).toHaveAttribute("aria-selected", "true");
  expect(new URLSearchParams(routeSearch()).toString()).toBe(
    new URLSearchParams(`view=done${drawn}`).toString()
  );
});

// R11: Done's tab counts by Done's own page read, held while another view is open, and a live
// change reaches it there: an issue closed elsewhere raises Done's count on Open.
test("on Open, an issue closed elsewhere raises the count of Done's tab", async () => {
  const user = userEvent.setup();
  const rows = VIEW_ROWS();
  const { client } = table(rows);
  await user.click(await screen.findByRole("tab", { name: "Done" }));
  expect(await screen.findByRole("tab", { name: "Done, 2 issues" })).toHaveAttribute(
    "aria-selected",
    "true"
  );
  await user.click(screen.getByRole("tab", { name: /^Open/ }));
  expect(await screen.findByRole("tab", { name: "Open, 2 issues" })).toHaveAttribute(
    "aria-selected",
    "true"
  );
  expect(screen.getByRole("tab", { name: "Done, 2 issues" })).toBeDefined();
  // CORE-1 closes elsewhere. What the event feed refreshes for it reads Done's held page again,
  // whose `total` is one more.
  rows[0] = issue("CORE-1", { status: "done", closed_at: "2026-11-12T10:00:00Z" });
  await act(() => client.invalidateQueries({ queryKey: ["issues", "project", "CORE"] }));
  expect(await screen.findByRole("tab", { name: "Open, 1 issue" })).toBeDefined();
  expect(await screen.findByRole("tab", { name: "Done, 3 issues" })).toBeDefined();
});

// An older link in today's spelling: `view=queue` opens Open with the Queue filter, written over
// the address the page opened on, and the tab marks the change Clear filters takes back.
test("an older view address is written over in today's spelling, and Clear filters takes its narrowing off", async () => {
  const user = userEvent.setup();
  table(VIEW_ROWS(), "?view=queue");
  await waitFor(() => expect(routeSearch()).toBe("?f.queue=Queued"));
  expect(await screen.findByRole("tab", { name: "Open, 1 issue, changed" })).toBeDefined();
  await user.click(screen.getByRole("button", { name: "Clear filters" }));
  await waitFor(() => expect(routeSearch()).toBe(""));
  expect(await screen.findByRole("tab", { name: "Open, 2 issues" })).toBeDefined();
});

// A press on the tab of the view the reader changed opens the view as it is its own: its sort,
// its filters and its search all go back.
test("a press on the changed tab gives the view back", async () => {
  const user = userEvent.setup();
  table(VIEW_ROWS(), "?q=CORE-2&sort=key&f.priority=unset");
  const changed = await screen.findByRole("tab", { name: /^Open, .*changed$/ });
  await user.click(changed);
  await waitFor(() => expect(routeSearch()).toBe(""));
  expect(await screen.findByRole("tab", { name: "Open, 2 issues" })).toBeDefined();
});

// A view is an address, and a press on a view tab leaves a history entry, as the page's other
// tabs do: Back opens the view before it, with its tab selected and its rows listed.
test("a press on a view tab leaves a history entry, and Back opens the view before it", async () => {
  const user = userEvent.setup();
  table(VIEW_ROWS());
  await screen.findByRole("row", { name: "CORE-1 Work CORE-1" });
  await user.click(screen.getByRole("tab", { name: "Done" }));
  await screen.findByRole("row", { name: "CORE-5 Work CORE-5" });
  expect(routeSearch()).toBe("?view=done");
  act(() => back());
  await waitFor(() => expect(routeSearch()).toBe(""));
  expect(await screen.findByRole("tab", { name: "Open, 2 issues" })).toHaveAttribute(
    "aria-selected",
    "true"
  );
  await waitFor(() => expect(listedKeys().sort()).toEqual(["CORE-1", "CORE-2"]));
});

// A press on the open view's tab, while the view is as it opens, writes over its history entry
// rather than leaving a second one at the same address: Back still opens the view before it.
test("a press on the open, unchanged view's tab leaves no history entry", async () => {
  const user = userEvent.setup();
  table(VIEW_ROWS());
  await screen.findByRole("row", { name: "CORE-1 Work CORE-1" });
  await user.click(screen.getByRole("tab", { name: "Done" }));
  await screen.findByRole("row", { name: "CORE-5 Work CORE-5" });
  await user.click(screen.getByRole("tab", { name: /^Done/ }));
  expect(routeSearch()).toBe("?view=done");
  act(() => back());
  await waitFor(() => expect(routeSearch()).toBe(""));
  expect(await screen.findByRole("tab", { name: "Open, 2 issues" })).toHaveAttribute(
    "aria-selected",
    "true"
  );
});

// The open saved view's name goes with its history entry, so Back to a saved view the reader
// changed selects its tab again, with its mark, and not the built-in view it narrows.
test("Back to a saved view the reader changed selects its tab again", async () => {
  const user = userEvent.setup();
  keepSavedViews([{ name: "Todo", query: "status=todo&cols=key,title,status&layout=table" }]);
  table(VIEW_ROWS());
  await user.click(await screen.findByRole("tab", { name: "Todo, 1 issue" }));
  expect(await screen.findByRole("tab", { name: "Todo, 1 issue" })).toHaveAttribute(
    "aria-selected",
    "true"
  );
  await user.type(screen.getByRole("searchbox", { name: "Search issues" }), "CORE-1");
  expect(await screen.findByRole("tab", { name: "Todo, 1 issue, changed" })).toHaveAttribute(
    "aria-selected",
    "true"
  );
  await user.click(screen.getByRole("tab", { name: /^Open/ }));
  await waitFor(() => expect(routeSearch()).toBe(""));
  act(() => back());
  expect(await screen.findByRole("tab", { name: "Todo, 1 issue, changed" })).toHaveAttribute(
    "aria-selected",
    "true"
  );
  expect(screen.getByRole("tab", { name: /^Open/ })).toHaveAttribute("aria-selected", "false");
  expect(new URLSearchParams(routeSearch()).get("q")).toBe("CORE-1");
});

// A saved view may hold the issues a link names (`only=`). Taking their chip off leaves a history
// entry that names the saved view, as every write of the address does, so its tab stays open,
// marked changed, and not the built-in view it narrows.
test("removing the narrowing chip of a saved view keeps its tab selected, marked changed", async () => {
  const user = userEvent.setup();
  keepSavedViews([
    {
      name: "Picked",
      query:
        "only=CORE-1,CORE-3&onlyLabel=Finished+last+week&nofilter=1&cols=key,title,status&layout=table",
    },
  ]);
  table(VIEW_ROWS());
  await user.click(await screen.findByRole("tab", { name: /^Picked/ }));
  expect(await screen.findByRole("tab", { name: /^Picked/ })).toHaveAttribute(
    "aria-selected",
    "true"
  );
  await user.click(
    await screen.findByRole("button", { name: "Remove Showing: Finished last week filter" })
  );
  await waitFor(() => expect(routeSearch()).not.toContain("only="));
  expect(await screen.findByRole("tab", { name: /^Picked, .*changed$/ })).toHaveAttribute(
    "aria-selected",
    "true"
  );
  expect(screen.getByRole("tab", { name: /^Open/ })).toHaveAttribute("aria-selected", "false");
});

// The view tabs are a tablist with its keyboard: the arrow keys, Home and End move the focus and
// open the view they reach, and only the open view's tab is in the Tab order.
test("the arrow keys, Home and End move the focus and the open view along the view tabs", async () => {
  const user = userEvent.setup();
  keepSavedViews([{ name: "Todo", query: "status=todo&cols=key,title,status&layout=table" }]);
  table(VIEW_ROWS());
  await screen.findByRole("row", { name: "CORE-1 Work CORE-1" });
  const views = () => within(screen.getByRole("tablist", { name: "Views" }));
  expect(
    views()
      .getAllByRole("tab")
      .map((tab) => tab.tabIndex)
  ).toEqual([0, -1, -1]);

  views().getByRole("tab", { name: /^Open/ }).focus();
  await user.keyboard("{ArrowRight}");
  await waitFor(() => expect(views().getByRole("tab", { name: /^Done/ })).toHaveFocus());
  expect(views().getByRole("tab", { name: /^Done/ })).toHaveAttribute("aria-selected", "true");
  expect(routeSearch()).toBe("?view=done");

  await user.keyboard("{End}");
  await waitFor(() => expect(views().getByRole("tab", { name: /^Todo/ })).toHaveFocus());
  expect(views().getByRole("tab", { name: /^Todo/ })).toHaveAttribute("aria-selected", "true");
  expect(new URLSearchParams(routeSearch()).get("status")).toBe("todo");

  await user.keyboard("{Home}");
  await waitFor(() => expect(views().getByRole("tab", { name: /^Open/ })).toHaveFocus());
  expect(views().getByRole("tab", { name: /^Open/ })).toHaveAttribute("aria-selected", "true");
  expect(routeSearch()).toBe("");
  // Only the open view's tab is a Tab stop: the arrow keys reach the others.
  expect(
    views()
      .getAllByRole("tab")
      .map((tab) => tab.tabIndex)
  ).toEqual([0, -1, -1]);
});

// Clear filters shows while the narrowings differ from the view's own and takes them back to
// it, leaving the sort, the grouping and the page size as the reader set them.
test("Clear filters shows while the view is narrowed and gives its own narrowings back", async () => {
  const user = userEvent.setup();
  const { view } = table(VIEW_ROWS(), "?sort=key");
  await screen.findByRole("row", { name: "CORE-1 Work CORE-1" });
  // Sorted but not narrowed: the view is changed, so Save view shows, and Clear filters does not.
  expect(screen.getByRole("button", { name: "Save view" })).toBeDefined();
  expect(screen.queryByRole("button", { name: "Clear filters" })).toBeNull();
  await user.type(screen.getByRole("searchbox", { name: "Search issues" }), "CORE-2");
  await user.click(await screen.findByRole("button", { name: "Clear filters" }));
  await waitFor(() => expect(routeSearch()).toBe("?sort=key"));
  expect(screen.queryByRole("button", { name: "Clear filters" })).toBeNull();
  view.unmount();

  // Done's own narrowing is its own: a Closed window is the reader's, and goes.
  table(VIEW_ROWS(), "?view=done&f.closed=7d&size=25");
  await user.click(await screen.findByRole("button", { name: "Clear filters" }));
  await waitFor(() => expect(routeSearch()).toBe("?view=done&size=25"));
});

// R3: Save view saves the view under a name of at most 14 characters, unique in the project and
// none of the built-in views' names; a name in use asks to replace that view first. The saved
// view is a tab after the built-in ones, and its delete button takes it away.
test("Save view checks the name, saves the view as a tab, replaces it on asking, and its tab deletes it", async () => {
  const user = userEvent.setup();
  table(VIEW_ROWS());
  await screen.findByRole("row", { name: "CORE-1 Work CORE-1" });
  // A view as it opens is no change to save.
  expect(screen.queryByRole("button", { name: "Save view" })).toBeNull();
  await user.type(screen.getByRole("searchbox", { name: "Search issues" }), "CORE-1");
  const saveView = await screen.findByRole("button", { name: "Save view" });
  const named = async (name: string) => {
    const box = screen.getByRole("textbox", { name: "Name of the view" });
    await user.clear(box);
    if (name !== "") await user.type(box, name);
    await user.click(screen.getByRole("button", { name: "Save" }));
  };

  await user.click(saveView);
  await named("Done");
  expect(screen.getByRole("alert").textContent).toBe(
    "Done is a built-in view. Type a different name."
  );
  await named("A view over the limit");
  expect(screen.getByRole("alert").textContent).toBe("Use 14 characters or fewer.");
  await named("");
  expect(screen.getByRole("alert").textContent).toBe("Type a name.");
  await named("Mine");
  expect(await screen.findByRole("tab", { name: "Mine, 1 issue" })).toHaveAttribute(
    "aria-selected",
    "true"
  );
  const [mine] = savedViews();
  expect(mine?.name).toBe("Mine");
  const query = new URLSearchParams(mine?.query);
  expect(query.get("q")).toBe("CORE-1");
  expect(query.get("layout")).toBe("table");
  // `cols=` names the whole set of columns shown: Open's own in a project with no daemon.
  expect(query.get("cols")?.split(",").sort()).toEqual([
    "asks",
    "assignee",
    "author",
    "claim",
    "created",
    "key",
    "labels",
    "priority",
    "status",
    "title",
  ]);
  // The address is the saved view's query, so its tab shows no mark.
  expect(routeSearch()).toBe(`?${mine?.query}`);

  // Changed, the saved view's tab shows its mark; saving it under its own name asks first.
  await user.clear(screen.getByRole("searchbox", { name: "Search issues" }));
  expect(await screen.findByRole("tab", { name: "Mine, 2 issues, changed" })).toBeDefined();
  await user.click(screen.getByRole("button", { name: "Save view" }));
  expect(screen.getByRole("textbox", { name: "Name of the view" })).toHaveValue("Mine");
  await user.click(screen.getByRole("button", { name: "Save" }));
  expect(screen.getByRole("alert").textContent).toBe("A saved view has the name Mine. Replace it?");
  expect(savedViews()[0]?.query).toBe(mine?.query);
  await user.click(screen.getByRole("button", { name: "Replace" }));
  await waitFor(() => expect(new URLSearchParams(savedViews()[0]?.query).has("q")).toBe(false));
  expect(savedViews()).toHaveLength(1);
  expect(await screen.findByRole("tab", { name: "Mine, 2 issues" })).toBeDefined();

  const remove = screen.getByRole("button", { name: "Delete saved view Mine" });
  expect(remove).toHaveAttribute("title", "Delete this saved view");
  await user.click(remove);
  await waitFor(() => expect(screen.queryByRole("tab", { name: /^Mine/ })).toBeNull());
  expect(savedViews()).toEqual([]);
});

/** A project's daemon setting that names a daemon. */
const DAEMON: LegionSetting = {
  created_by: { kind: "user", id: "alice" },
  project: "CORE",
  state_url: "http://legion.test",
  updated_at: "2026-11-01T00:00:00Z",
};

/** The board route's answer: the facts its `issues` map holds, and its header. */
function legionBoard(
  issues: Record<string, LegionBoardFacts> = {},
  header: Partial<LegionBoardResponse["header"]> = {}
): LegionBoardResponse {
  return {
    columns: null,
    computed_at: "2026-11-17T08:00:00Z",
    header: {
      facts_cursor_age_seconds: null,
      facts_updated_at: null,
      queue_length: 0,
      read_at: "2026-11-17T08:00:00Z",
      read_error: null,
      read_ok: true,
      slot_cap: 4,
      slots_in_use: 1,
      trees: 1,
      ...header,
    },
    issues,
    project: "CORE",
    settings: null,
  };
}

/** The facts the board's map holds for an issue. */
function boardFacts(workHours: number): LegionBoardFacts {
  return {
    as_of: "2026-11-17T08:00:00Z",
    closed_at: "2026-11-10T10:00:00Z",
    growth: { hours_by_phase: {}, slot_hours: 0, work_hours: 0, work_last_at: 0 },
    hours_by_phase: { implementing: workHours },
    opened_at: "2026-11-01T00:00:00Z",
    review_rounds: 1,
    root_key: "CORE-3",
    slot_hours: workHours,
    tree_depth: 0,
    updated_at: "2026-11-17T08:00:00Z",
    waiting_reason: "",
    work_first_at: "2026-11-02T00:00:00Z",
    work_hours: workHours,
    work_last_at: "2026-11-10T00:00:00Z",
  };
}

/** The facts the board's map holds for an open issue at work: 1 hour of work and 1 hour in
 *  Implementing at `asOf`, each of which grows an hour an hour from then. */
function openFacts(asOf: string): LegionBoardFacts {
  return {
    ...boardFacts(1),
    as_of: asOf,
    closed_at: null,
    growth: { hours_by_phase: { implementing: 1 }, slot_hours: 1, work_hours: 1, work_last_at: 1 },
    work_last_at: asOf,
  };
}

/** A closed issue's row of `legion-work-time`'s items. */
function factsRow(key: string, closedAt: string, workHours: number): EvalItemRow {
  return {
    cols: {
      closed_at: closedAt,
      hours_implementing: workHours,
      review_rounds: 1,
      root_key: key,
      tree_depth: 0,
      work_first_at: closedAt,
      work_hours: workHours,
    },
    href: `/issues/${key}`,
    links: [],
    measured_at: closedAt,
    range: "window",
    row_key: key,
    row_kind: "issue",
    source: "legionfacts",
  };
}

/** The items route, answering what `answer` gives at each read: the rows, or a refusal. */
function answerItems(answer: () => Promise<readonly EvalItemRow[]>) {
  const items = spyOn(api, "listEvalItems").mockImplementation(async (_project, _slug, query) => {
    const rows = await answer();
    const offset = query.offset ?? 0;
    return {
      compare: null,
      items: rows.slice(offset),
      limit: 200,
      offset,
      total: rows.length,
      window: { days: 1, from: String(query.from), to: String(query.to) },
    };
  });
  restores.push(() => items.mockRestore());
  return items;
}

// While the daemon setting's first read is out, the page shows its loading state and reads no
// listing: what it reads and draws turns on the answer.
test("while the daemon setting is read, the tab shows its loading state and reads nothing more", async () => {
  const { listing, page, setting } = table(
    VIEW_ROWS(),
    "",
    {},
    {},
    Promise.withResolvers<LegionSetting | null>().promise
  );
  expect(await screen.findByRole("status", { name: "Loading issues" })).toBeDefined();
  expect(setting).toHaveBeenCalledWith("CORE");
  expect(screen.queryByRole("tablist", { name: "Views" })).toBeNull();
  expect(screen.queryByRole("table")).toBeNull();
  expect(listing).not.toHaveBeenCalled();
  expect(page).not.toHaveBeenCalled();
});

// A failed read of the daemon setting is an alert under the tabs with its error and Retry, and
// until a read answers the project is drawn as one with no daemon: no board-fed column, no
// Legion phase in Group by, no board read, and `f.queue=Queued` lists the Todo issues labelled
// `legion`.
test("a failed daemon setting read names its error with Retry, and the project is drawn with no daemon until it answers", async () => {
  const user = userEvent.setup();
  const sockets = liveSockets(restores);
  const { setting } = table(
    [...VIEW_ROWS(), issue("CORE-4", { status: "in_progress", labels: ["legion"] })],
    "?f.queue=Queued",
    {},
    {},
    Promise.reject(new ApiError(503, { code: "UNAVAILABLE", error: "The store did not answer." }))
  );
  await screen.findByText(/^Could not read the Legion daemon setting/);
  expect(screen.getByRole("alert").textContent).toBe(
    "Could not read the Legion daemon setting: The store did not answer. Retry"
  );
  expect(listedKeys()).toEqual(["CORE-1"]);
  const headers = () =>
    screen.getAllByRole("columnheader").map((header) => header.textContent ?? "");
  expect(headers().some((text) => text.startsWith("Legion phase"))).toBe(false);
  await user.click(screen.getByRole("button", { name: "Group by" }));
  expect(screen.queryByRole("radio", { name: "Legion phase" })).toBeNull();
  await user.keyboard("{Escape}");
  expect(sockets.sockets).toEqual([]);

  setting.mockImplementation(async () => DAEMON);
  await user.click(screen.getByRole("button", { name: "Retry reading the Legion daemon setting" }));
  await waitFor(() =>
    expect(screen.queryByText(/^Could not read the Legion daemon setting/)).toBeNull()
  );
  await answerBoard(sockets, legionBoard());
  await waitFor(() => expect(headers().some((text) => text.startsWith("Legion phase"))).toBe(true));
});

// A project whose setting names a daemon holds the board, draws its Legion columns and its
// Legion phase grouping, and says under the tabs when the daemon's last read failed. A later
// failed read of the setting keeps the page as the answered read drew it.
test("a daemon setting that names a daemon draws the Legion columns, says a failed daemon read, and outlasts a later failed read", async () => {
  const user = userEvent.setup();
  const sockets = liveSockets(restores);
  const { client, setting } = table(VIEW_ROWS(), "", {}, {}, Promise.resolve(DAEMON));
  await answerBoard(
    sockets,
    legionBoard({}, { read_ok: false, read_error: "dial tcp 10.0.0.9:7400: connection refused" })
  );
  expect(sockets.latest().subscribed("legion").view).toEqual({ kind: "legion", project: "CORE" });
  expect(
    await screen.findByText(
      "The last read of the Legion daemon failed: dial tcp 10.0.0.9:7400: connection refused"
    )
  ).toBeDefined();
  const legionPhase = () =>
    screen
      .getAllByRole("columnheader")
      .some((header) => (header.textContent ?? "").startsWith("Legion phase"));
  await waitFor(() => expect(legionPhase()).toBe(true));
  await user.click(screen.getByRole("button", { name: "Group by" }));
  expect(screen.getByRole("radio", { name: "Legion phase" })).toBeDefined();
  await user.keyboard("{Escape}");

  setting.mockImplementation(() => Promise.reject(new ApiError(503, { error: "down" })));
  await act(() => client.invalidateQueries({ queryKey: ["legion-daemon", "CORE"] }));
  await waitFor(() => expect(setting).toHaveBeenCalledTimes(2));
  expect(legionPhase()).toBe(true);
  expect(screen.queryByText(/^Could not read the Legion daemon setting/)).toBeNull();
});

// A view whose filter reads facts shows the table's loading state until the board and the items
// of its done rows answer, and an error with Retry when the items read fails: never an empty
// list, or a count, from a read that has not answered.
test("a view that filters by a fact waits for the facts, and a failed read of them is an error with Retry", async () => {
  const user = userEvent.setup();
  const sockets = liveSockets(restores);
  let answer = Promise.withResolvers<readonly EvalItemRow[]>();
  const items = answerItems(() => answer.promise);
  table(VIEW_ROWS(), "?view=done&f.work_hours=over-7d", {}, {}, Promise.resolve(DAEMON));
  expect(await screen.findByRole("status", { name: "Loading issues" })).toBeDefined();
  expect(await screen.findByRole("tab", { name: "Done, changed" })).toBeDefined();
  await answerBoard(sockets, legionBoard());
  await waitFor(() => expect(items).toHaveBeenCalled());
  expect(screen.getByRole("status", { name: "Loading issues" })).toBeDefined();
  expect(screen.queryByText("No issues match these filters.")).toBeNull();

  answer.reject(new ApiError(404, { code: "EVAL_NOT_FOUND", error: "No eval legion-work-time." }));
  expect(
    await screen.findByText("Could not read the Legion columns, which this view needs.")
  ).toBeDefined();
  expect(
    screen.getByText(
      "Could not read the Legion columns of the closed issues: No eval legion-work-time."
    )
  ).toBeDefined();
  expect(screen.getByRole("tab", { name: "Done, changed" })).toBeDefined();

  answer = Promise.withResolvers<readonly EvalItemRow[]>();
  answer.resolve([factsRow("CORE-5", "2026-11-11T10:00:00Z", 200)]);
  await user.click(screen.getByRole("button", { name: "Retry" }));
  await screen.findByRole("row", { name: "CORE-5 Work CORE-5" });
  expect(listedKeys()).toEqual(["CORE-5"]);
  expect(screen.getByRole("tab", { name: "Done, 1 issue, changed" })).toBeDefined();
});

// A view with no facts filter draws its rows before the facts answer; a failed items read marks
// each facts cell of a done row with the error mark, never the empty cell of no Legion work.
test("a failed items read marks the done rows' facts cells and names the error under the tabs", async () => {
  const sockets = liveSockets(restores);
  answerItems(() =>
    Promise.reject(new ApiError(500, { error: "The facts store did not answer." }))
  );
  table(VIEW_ROWS(), "?view=done", {}, {}, Promise.resolve(DAEMON));
  await screen.findByRole("row", { name: "CORE-5 Work CORE-5" });
  await answerBoard(sockets, legionBoard());
  expect(
    await screen.findByText(
      "Could not read the Legion columns of the closed issues: The facts store did not answer."
    )
  ).toBeDefined();
  const row = screen.getByRole("row", { name: "CORE-5 Work CORE-5" });
  expect(row.querySelectorAll('[data-facts-read="error"]').length).toBeGreaterThan(0);
  expect(
    screen.getByRole("button", { name: "Retry reading the Legion columns" })
  ).toBeInTheDocument();
});

// `exclude_tree` leaves out each tree it names: the root itself, and every row whose facts name
// that root. A row's tree is in its facts, so the view shows its loading state until the board
// and the items of its done rows answer, never a list from a read that has not answered.
test("exclude_tree leaves out the rows of each tree it names, and shows the loading state until the facts that name a row's tree answer", async () => {
  const sockets = liveSockets(restores);
  const answer = Promise.withResolvers<readonly EvalItemRow[]>();
  const items = answerItems(() => answer.promise);
  const child = factsRow("CORE-6", "2026-11-12T10:00:00Z", 1);
  table(
    [...VIEW_ROWS(), issue("CORE-6", { status: "done", closed_at: "2026-11-12T10:00:00Z" })],
    "?view=done&exclude_tree=CORE-5",
    {},
    {},
    Promise.resolve(DAEMON)
  );
  expect(await screen.findByRole("status", { name: "Loading issues" })).toBeDefined();
  await answerBoard(sockets, legionBoard({ "CORE-3": boardFacts(2) }));
  await waitFor(() => expect(items).toHaveBeenCalled());
  expect(screen.getByRole("status", { name: "Loading issues" })).toBeDefined();
  expect(screen.queryByText("No issues match these filters.")).toBeNull();

  answer.resolve([
    factsRow("CORE-5", "2026-11-11T10:00:00Z", 3),
    { ...child, cols: { ...child.cols, root_key: "CORE-5", tree_depth: 1 } },
  ]);
  await screen.findByRole("row", { name: "CORE-3 Work CORE-3" });
  expect(listedKeys()).toEqual(["CORE-3"]);
});

// Each tree left out is a chip of the control row and a line of the Filters panel, each with the
// remove that gives its rows back and leaves the other tree out.
test("each tree exclude_tree leaves out is a chip of the row and a line of the Filters panel, and either remove gives its rows back", async () => {
  const user = userEvent.setup();
  const sockets = liveSockets(restores);
  answerItems(async () => [factsRow("CORE-5", "2026-11-11T10:00:00Z", 3)]);
  table(VIEW_ROWS(), "?exclude_tree=CORE-3&exclude_tree=CORE-5", {}, {}, Promise.resolve(DAEMON));
  expect(await screen.findByRole("status", { name: "Loading issues" })).toBeDefined();
  await answerBoard(
    sockets,
    legionBoard({ "CORE-2": { ...boardFacts(2), root_key: "CORE-3", tree_depth: 1 } })
  );
  await screen.findByRole("row", { name: "CORE-1 Work CORE-1" });
  expect(listedKeys()).toEqual(["CORE-1"]);
  const chip = screen.getByRole("button", { name: "Remove Not in tree CORE-3 filter" });
  expect(chip).toHaveAttribute("aria-pressed", "true");
  expect(screen.getByRole("button", { name: "Remove Not in tree CORE-5 filter" })).toBeDefined();

  await user.click(screen.getByRole("button", { name: /^Filters/ }));
  const panel = screen.getByRole("list", { name: "Other filters" });
  expect(within(panel).getByText("Not in tree CORE-3")).toBeDefined();
  await user.click(within(panel).getByRole("button", { name: "Remove Not in tree CORE-5" }));
  await waitFor(() => expect(routeSearch()).not.toContain("exclude_tree=CORE-5"));
  expect(routeSearch()).toContain("exclude_tree=CORE-3");
  expect(listedKeys()).toEqual(["CORE-1"]);

  await user.click(screen.getByRole("button", { name: "Remove Not in tree CORE-3 filter" }));
  await waitFor(() => expect(listedKeys().sort()).toEqual(["CORE-1", "CORE-2"]));
  expect(routeSearch()).not.toContain("exclude_tree");
  expect(screen.queryByRole("button", { name: /^Remove Not in tree / })).toBeNull();
});

/** Done, in a project with a daemon whose board's map holds CORE-3 and not CORE-5: the items read
 *  of CORE-5's close day, and the counts of each read made. */
async function doneWithFacts(rows = VIEW_ROWS()) {
  const sockets = liveSockets(restores);
  const items = answerItems(async () => [factsRow("CORE-5", "2026-11-11T10:00:00Z", 3)]);
  const drawn = table(rows, "?view=done", {}, {}, Promise.resolve(DAEMON));
  await screen.findByRole("row", { name: "CORE-5 Work CORE-5" });
  // The board's map decides which rows need the items, so they are read once it has answered.
  expect(items).not.toHaveBeenCalled();
  await answerBoard(sockets, legionBoard({ "CORE-3": boardFacts(2) }));
  await waitFor(() => expect(items).toHaveBeenCalledTimes(1));
  const pageReads = () =>
    drawn.page.mock.calls.filter(([options]) => options.sort === "-closed_at").length;
  const openCountReads = () =>
    drawn.page.mock.calls.filter(([options]) => options.open === true).length;
  return { ...drawn, items, openCountReads, pageReads };
}

test("Done reads the items of the close days of the rows the board's map does not hold", async () => {
  const { items } = await doneWithFacts();
  expect(items.mock.calls[0]?.[1]).toBe("legion-work-time");
  expect(items.mock.calls[0]?.[2]).toEqual({
    from: "2026-11-11T00:00:00Z",
    to: "2026-11-12T00:00:00Z",
    row_kind: "issue",
    limit: 200,
    offset: 0,
  });
});

// The first rule: a live event on an issue reads the view's listing again, with its items, and
// on Done Open's count too.
test("a live event reads Done's page again, with its items and Open's count", async () => {
  const { client, items, openCountReads, pageReads } = await doneWithFacts();
  expect(pageReads()).toBe(1);
  expect(openCountReads()).toBe(1);
  // What the event feed refreshes for an event of one of the project's issues.
  await act(() => client.invalidateQueries({ queryKey: ["issues", "project", "CORE"] }));
  await waitFor(() => expect(items).toHaveBeenCalledTimes(2));
  expect(pageReads()).toBe(2);
  expect(openCountReads()).toBe(2);
});

// The second rule: a close whose items answer holds no row for the closed issue reads the items
// once more 60 s later, since a stored facts row is never more than 60 s behind its event.
test("a close whose items answer lacks its row reads the items once more 60 s later", async () => {
  const rows = VIEW_ROWS();
  const { client, items } = await doneWithFacts(rows);
  // The wait is the product's 60 s, run on the test's clock from the close on.
  jest.useFakeTimers();
  try {
    // CORE-1 closes elsewhere; the event reads Done's page again, and the items read it starts
    // answers no row for CORE-1 yet.
    rows[0] = issue("CORE-1", { status: "done", closed_at: "2026-11-12T10:00:00Z" });
    await act(async () => {
      void client.invalidateQueries({ queryKey: ["issues", "project", "CORE"] });
    });
    await screen.findByRole("row", { name: "CORE-1 Work CORE-1" });
    await waitFor(() => expect(items.mock.calls.length).toBeGreaterThanOrEqual(2));
    await tick(0);
    const reads = items.mock.calls.length;
    await tick(CLOSE_ITEMS_AGAIN_MS - 1_000);
    expect(items).toHaveBeenCalledTimes(reads);
    await tick(2_000);
    expect(items).toHaveBeenCalledTimes(reads + 1);
    // Once: the answer of that read is no close.
    await tick(CLOSE_ITEMS_AGAIN_MS + 1_000);
    expect(items).toHaveBeenCalledTimes(reads + 1);
  } finally {
    realClock();
  }
});

// The third rule: a board push that changes or removes the key of a done row the view lists
// reads its items again; a push that leaves every listed key as it was reads nothing.
test("a board push that changes or removes a listed done row's facts reads the items again", async () => {
  const { client, items } = await doneWithFacts();
  const push = (board: LegionBoardResponse) =>
    act(() => client.setQueryData(legionBoardQuery("CORE").queryKey, board));
  push(legionBoard({ "CORE-3": boardFacts(2), "CORE-1": boardFacts(1) }));
  await new Promise((resolve) => setTimeout(resolve, 50));
  expect(items).toHaveBeenCalledTimes(1);
  push(legionBoard({ "CORE-3": boardFacts(4), "CORE-1": boardFacts(1) }));
  await waitFor(() => expect(items).toHaveBeenCalledTimes(2));
  push(legionBoard({ "CORE-1": boardFacts(1) }));
  await waitFor(() => expect(items).toHaveBeenCalledTimes(3));
  // With CORE-3 out of the map, its close day is read too.
  expect(items.mock.calls[2]?.[2]).toMatchObject({ from: "2026-11-10T00:00:00Z" });
});

// The bar under the tabs, the pager and the (i) hint.

/** Open's rows: three in progress, two in Todo and one in Triage, given out of the status order,
 *  and a closed one Open does not list. */
const BAR_ROWS = (): IssueSummary[] => [
  issue("CORE-1", { status: "in_progress" }),
  issue("CORE-2", { status: "todo" }),
  issue("CORE-3", { status: "triage" }),
  issue("CORE-4", { status: "in_progress" }),
  issue("CORE-5", { status: "todo" }),
  issue("CORE-6", { status: "in_progress" }),
  issue("CORE-7", { status: "done", closed_at: "2026-11-10T10:00:00Z" }),
];

/** The bar's segments, each with its status and its width in percent of the bar. */
const barSegments = (): [string | undefined, number][] =>
  [...document.querySelectorAll<HTMLElement>("[data-bar-segment]")].map((segment) => [
    segment.dataset.barSegment,
    Number.parseFloat(segment.style.width),
  ]);

/** The bar's legend entries, as they read. */
const legendEntries = () =>
  within(screen.getByRole("region", { name: "Issues by status" }))
    .getAllByRole("button")
    .map((entry) => entry.textContent);

// R6: Open's bar counts the rows the table lists, by status, in the status order and from the
// left edge, each segment as wide as its share, with no segment for a status with no row.
test("Open's bar counts the listed rows by status in the status order, and follows the search", async () => {
  const user = userEvent.setup();
  table(BAR_ROWS());
  await screen.findByRole("region", { name: "Issues by status" });
  expect(legendEntries()).toEqual(["Triage 1", "Todo 2", "In progress 3"]);
  const segments = barSegments();
  expect(segments.map(([status]) => status)).toEqual(["triage", "todo", "in_progress"]);
  expect(segments[0]?.[1]).toBeCloseTo(100 / 6);
  expect(segments[2]?.[1]).toBeCloseTo(50);
  expect(segments.reduce((sum, [, width]) => sum + width, 0)).toBeCloseTo(100);
  // No count of the strip it replaces: no Legion queue, no running issues, no epics.
  const bar = screen.getByRole("region", { name: "Issues by status" });
  expect(bar.textContent).not.toMatch(/queue|running|epic/i);

  await user.type(screen.getByRole("searchbox", { name: "Search issues" }), "CORE-1");
  await waitFor(() => expect(legendEntries()).toEqual(["In progress 1"]));
  expect(barSegments()).toEqual([["in_progress", 100]]);
});

test("with a Status filter of In progress the bar is one segment of every listed row", async () => {
  table(BAR_ROWS(), "?status=in_progress");
  await screen.findByRole("region", { name: "Issues by status" });
  expect(listedKeys().sort()).toEqual(["CORE-1", "CORE-4", "CORE-6"]);
  expect(legendEntries()).toEqual(["In progress 3"]);
  expect(barSegments()).toEqual([["in_progress", 100]]);
  expect(screen.getByRole("tab", { name: "Open, 3 issues, changed" })).toBeDefined();
});

test("a press on a legend entry narrows the view to that status, and a second press gives Open's statuses back", async () => {
  const user = userEvent.setup();
  table(BAR_ROWS());
  await user.click(await screen.findByRole("button", { name: "Todo 2" }));
  await waitFor(() => expect(routeSearch()).toBe("?status=todo"));
  expect(listedKeys().sort()).toEqual(["CORE-2", "CORE-5"]);
  expect(legendEntries()).toEqual(["Todo 2"]);
  const todo = screen.getByRole("button", { name: "Todo 2" });
  expect(todo).toHaveAttribute("aria-pressed", "true");

  await user.click(todo);
  await waitFor(() => expect(routeSearch()).toBe(""));
  expect(legendEntries()).toEqual(["Triage 1", "Todo 2", "In progress 3"]);
});

/** The saved view "Busy": Todo and In progress, as Save view writes one. */
const BUSY = "status=todo&status=in_progress&cols=key,title,status&layout=table";

// A saved view the reader opened, by its tab or by an address that is its query, stays the open
// tab while they change it, with the dot of a changed view, so the second press gives back its
// statuses, not Open's.
test.each([
  [
    "by its tab",
    "",
    async (user: UserEvent) =>
      user.click(await screen.findByRole("tab", { name: "Busy, 5 issues" })),
  ],
  ["by its address", `?${BUSY}`, async () => {}],
])("a saved view opened %s stays open while changed, and the second press gives its statuses back", async (_name, search, open) => {
  const user = userEvent.setup();
  keepSavedViews([{ name: "Busy", query: BUSY }]);
  table(BAR_ROWS(), search);
  await open(user);
  const busy = await screen.findByRole("tab", { name: "Busy, 5 issues" });
  expect(busy).toHaveAttribute("aria-selected", "true");
  expect(busy.querySelector("[data-view-changed]")).toBeNull();
  await waitFor(() => expect(legendEntries()).toEqual(["Todo 2", "In progress 3"]));

  await user.click(screen.getByRole("button", { name: "In progress 3" }));
  await waitFor(() =>
    expect(new URLSearchParams(routeSearch()).getAll("status")).toEqual(["in_progress"])
  );
  expect(legendEntries()).toEqual(["In progress 3"]);
  const changed = await screen.findByRole("tab", { name: "Busy, 3 issues, changed" });
  expect(changed).toHaveAttribute("aria-selected", "true");
  expect(changed.querySelector("[data-view-changed]")).not.toBeNull();
  expect(screen.getByRole("tab", { name: /^Open/ })).toHaveAttribute("aria-selected", "false");

  await user.click(screen.getByRole("button", { name: "In progress 3" }));
  await waitFor(() =>
    expect(new URLSearchParams(routeSearch()).getAll("status")).toEqual(["todo", "in_progress"])
  );
  expect(legendEntries()).toEqual(["Todo 2", "In progress 3"]);
  const restored = await screen.findByRole("tab", { name: "Busy, 5 issues" });
  expect(restored).toHaveAttribute("aria-selected", "true");
  expect(restored.querySelector("[data-view-changed]")).toBeNull();
});

/** The chips of the control row, by their names, in the order they are drawn. */
const rowChips = () =>
  screen
    .queryAllByRole("button", { name: /^Remove .* filter$/ })
    .map((chip) => chip.getAttribute("aria-label"));

// One chip rule for the table and the Board (`viewChips`): a saved view's own filter is no
// narrowing of the reader's, so neither layout draws a chip for it, and no chip's press can take
// it away. A narrowing the reader adds on top of the view is one chip in both.
test.each([
  ["Status", "status=todo"],
  ["Priority", "f.priority=0"],
])("a saved view's own %s filter draws no chip in the table or on the Board, and a narrowing added on top draws one in both", async (_name, filter) => {
  const user = userEvent.setup();
  const query = `${filter}&cols=key,title,status,priority&layout=table`;
  keepSavedViews([{ name: "Mine", query }]);
  table(
    [
      issue("CORE-1", { status: "todo", priority: 0, open_asks: 1 }),
      issue("CORE-2", { status: "todo", priority: 1 }),
      issue("CORE-3", { status: "in_progress", priority: 0 }),
    ],
    `?${query}`
  );
  expect(await screen.findByRole("tab", { name: "Mine, 2 issues" })).toHaveAttribute(
    "aria-selected",
    "true"
  );
  expect(rowChips()).toEqual([]);
  await user.click(screen.getByRole("button", { name: "Filters · 0" }));
  await user.click(screen.getByRole("button", { name: "Needs you" }));
  expect(await screen.findByRole("tab", { name: "Mine, 1 issue, changed" })).toHaveAttribute(
    "aria-selected",
    "true"
  );
  expect(rowChips()).toEqual(["Remove Needs you filter"]);
  await user.keyboard("{Escape}");

  await user.click(screen.getByRole("button", { name: "Board" }));
  await screen.findByRole("region", { name: "Project board" });
  expect(screen.getByRole("tab", { name: /^Mine/ })).toHaveAttribute("aria-selected", "true");
  expect(rowChips()).toEqual(["Remove Needs you filter"]);
});

// R6: Done draws no bar and keeps no room for one, and the Board draws none in either grouping.
test.each([
  ["Done", "?view=done", () => screen.findByRole("row", { name: "CORE-7 Work CORE-7" })],
  ["Open's Board", "?layout=board", () => screen.findByRole("region", { name: "Project board" })],
])("%s draws no bar", async (_name, search, drawn) => {
  table(BAR_ROWS(), search);
  await drawn();
  expect(screen.queryByRole("region", { name: "Issues by status" })).toBeNull();
  expect(document.querySelector("[data-bar-segment], [data-bar-legend]")).toBeNull();
});

// R13: a priority's table keeps its status strip, without the strip's numbers.
test("a priority's table keeps its status strip with no queue or running number", async () => {
  const advances = [{ id: "PRI-7", inherited: false }];
  table(
    [
      issue("CORE-2", { advances, labels: ["legion"] }),
      issue("OPS-3", { advances, status: "in_progress" }),
    ],
    "",
    undefined,
    { listing: { advances: "PRI-7" }, stateKey: "w" }
  );
  const strip = await screen.findByRole("group", { name: "Issues by status" });
  expect(
    within(strip)
      .getAllByRole("button")
      .map((cell) => cell.textContent)
  ).toEqual(["Todo 1", "In progress 1"]);
  expect(strip.parentElement?.textContent).toBe("Todo 1In progress 1");
  expect(screen.queryByRole("region", { name: "Issues by status" })).toBeNull();
  const pager = screen.getByTestId("issues-table-pager");
  expect(within(pager).getByText("Rows 1–2")).toBeDefined();
  expect(pager.textContent).not.toMatch(/of 2|issues/);
});

// The List's pager draws the range of rows, the page, its steps and the page size, and no total:
// the tab says how many rows the view lists.
test("the pager reads Rows and Page, its steps and the page size, and no total", async () => {
  const user = userEvent.setup();
  table(Array.from({ length: 120 }, (_, index) => issue(`CORE-${index + 1}`)));
  expect(await screen.findByRole("tab", { name: "Open, 120 issues" })).toBeDefined();
  const pager = screen.getByTestId("issues-table-pager");
  expect(within(pager).getByText("Rows 1–50")).toBeDefined();
  expect(within(pager).getByText("Page 1 of 3")).toBeDefined();
  expect(within(pager).getByRole("button", { name: "‹ Previous" })).toBeDefined();
  expect(within(pager).getByRole("button", { name: "Next ›" })).toBeDefined();
  // The page size is a single pick (`Pager`), opened from its trigger.
  const sizes = await openList(user, "Rows per page");
  expect(optionNames(sizes)).toContain("All rows");
  expect(pager.textContent).not.toMatch(/120|issues/);

  await user.click(within(sizes).getByRole("option", { name: "All rows" }));
  // The range reads All rows. The page size's trigger shows the same words, so its text is
  // skipped.
  expect(await within(pager).findByText("All rows", { ignore: "button *" })).toBeDefined();
  expect(pager.textContent).not.toMatch(/120|issues/);
});

// Done's page is the server's: its range and page count follow the server's count of the done
// rows, not the one page of them the table holds.
test("Done's second server page reads Rows 51–100 of the server's count", async () => {
  const closed = Array.from({ length: 120 }, (_, index) =>
    issue(`CORE-${index + 10}`, { status: "done", closed_at: "2026-11-04T10:00:00Z" })
  );
  const { page } = table(closed, "?view=done&page=2");
  expect(await screen.findByRole("tab", { name: "Done, 120 issues" })).toBeDefined();
  expect(page).toHaveBeenCalledWith({
    project: "CORE",
    status: ["done"],
    sort: "-closed_at",
    limit: 50,
    offset: 50,
  });
  const pager = screen.getByTestId("issues-table-pager");
  expect(within(pager).getByText("Rows 51–100")).toBeDefined();
  expect(within(pager).getByText("Page 2 of 3")).toBeDefined();
  expect(pager.textContent).not.toMatch(/120|issues/);
});

// While a release rolls out, a server older than the list route's `sort` ignores it and answers
// the first done issues by rank, `closed_at` and all. Its answer names no sort, so Done draws
// none of its rows and counts none of them on its tab: the shared retry policy reads it twice
// more, and then Done says the server did not sort them, with Retry. Once the server applies the
// sort, Retry reads the page again and Done draws it, newest close first.
test("Done draws no page whose answer names no sort, reads it again, then says so with Retry", async () => {
  const user = userEvent.setup();
  let updated = false;
  const { page } = table(
    [
      issue("CORE-1", { status: "done", closed_at: "2026-11-04T10:00:00Z" }),
      issue("CORE-2", { status: "done", closed_at: "2026-11-05T10:00:00Z" }),
    ],
    "?view=done",
    { retries: true, sortApplied: () => updated }
  );
  const pageReads = () => page.mock.calls.filter(([options]) => options.sort === "-closed_at");
  const alert = await screen.findByRole("alert");
  expect(alert.textContent).toContain(
    "The server did not sort the closed issues by close time. Retry when the server update is complete."
  );
  expect(pageReads().length).toBe(3);
  expect(screen.queryByRole("row", { name: "CORE-1 Work CORE-1" })).toBeNull();
  expect(screen.queryByRole("row", { name: "CORE-2 Work CORE-2" })).toBeNull();
  expect(screen.getByRole("tab", { name: "Done" })).toBeDefined();

  updated = true;
  await user.click(within(alert).getByRole("button", { name: "Retry loading issues" }));
  expect(await screen.findByRole("tab", { name: "Done, 2 issues" })).toBeDefined();
  expect(pageReads().length).toBe(4);
  const rows = screen.getAllByRole("row").filter((row) => row.hasAttribute("data-issue-row"));
  expect(rows.map((row) => row.getAttribute("data-issue-row"))).toEqual(["CORE-2", "CORE-1"]);
  expect(screen.queryByRole("alert")).toBeNull();
});

/** A card of the board's Queue column that waits for capacity, its worker at work. */
function waitingCard(key: string): LegionBoardCard {
  return {
    blocked_by: null,
    children: 0,
    key,
    phase: "admitted",
    phase_since: "2026-11-17T07:00:00Z",
    priority: null,
    roles: ["implementer"],
    slot: 2,
    slot_since: "2026-11-17T07:00:00Z",
    slot_use: "working",
    title: `Work ${key}`,
    tree: key,
    tree_slot: 1,
    waiting: { reason: "slot", since: "2026-11-17T07:00:00Z", blocked_by: null },
    waiting_on: null,
    waiting_place: 1,
  };
}

/** Every text the page draws and every name it gives a control or a mark. */
function pageWords(): string[] {
  const words = [document.body.textContent ?? ""];
  for (const element of document.body.querySelectorAll("*"))
    for (const attribute of ["aria-label", "aria-description", "title", "placeholder", "alt"]) {
      const value = element.getAttribute(attribute);
      if (value !== null) words.push(value);
    }
  return words;
}

// R1, R2, R9: the (i) hint follows Save view in a project with a daemon, and holds the capacity,
// the trees and the last read. No text or name on the tab says "slot".
test("the (i) hint follows Save view in a project with a daemon, and nothing on the tab says slot", async () => {
  const user = userEvent.setup();
  const sockets = liveSockets(restores);
  table(
    VIEW_ROWS(),
    "?cols=key,title,status,legion_phase,waiting_reason,activity",
    {},
    {},
    Promise.resolve(DAEMON)
  );
  await answerBoard(sockets, {
    ...legionBoard({}, { slot_cap: null, slots_in_use: 4, trees: 0 }),
    columns: [{ id: "queue", label: "Queue", cards: [waitingCard("CORE-1")] }],
  });
  await waitFor(() =>
    expect(
      within(screen.getByRole("row", { name: "CORE-1 Work CORE-1" })).getByText("Capacity to go on")
    ).toBeDefined()
  );
  const hint = screen.getByRole("button", { name: "Legion daemon" });
  const save = screen.getByRole("button", { name: "Save view", hidden: true });
  expect(save.compareDocumentPosition(hint) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  expect(pageWords().filter((words) => /slot/i.test(words))).toEqual([]);

  await user.click(hint);
  const panel = screen.getByRole("dialog", { name: "Legion daemon" });
  expect(
    [...panel.querySelectorAll("dt")].map((name) => [
      name.textContent,
      name.nextElementSibling?.textContent,
    ])
  ).toEqual([
    ["Capacity", "4"],
    ["Trees", "0"],
    ["Last read", expect.any(String)],
  ]);
  expect(pageWords().filter((words) => /slot/i.test(words))).toEqual([]);

  await user.click(screen.getByRole("button", { name: "Columns" }));
  expect(pageWords().filter((words) => /slot/i.test(words))).toEqual([]);
});

test("a project with no daemon has no (i) hint", async () => {
  table(VIEW_ROWS());
  await screen.findByRole("row", { name: "CORE-1 Work CORE-1" });
  expect(screen.queryByRole("button", { name: "Legion daemon" })).toBeNull();
});

// A column a newer server adds after Held is a Legion phase under the route's label: the Legion
// phase filter offers it after Held and before None, its card's cell reads that label, and Group
// by heads its group in the board's order, before None.
test("a column the board route adds after Held is a Legion phase pick, cell and group under its label", async () => {
  const user = userEvent.setup();
  const sockets = liveSockets(restores);
  table(
    [issue("CORE-1"), issue("CORE-2"), issue("CORE-3")],
    "?cols=key,title,legion_phase&group=legion_phase",
    {},
    {},
    Promise.resolve(DAEMON)
  );
  await answerBoard(sockets, {
    ...legionBoard(),
    columns: [
      ...LEGION_PHASES.map(({ id, title }) => ({
        id,
        label: title,
        cards:
          id === "held" ? [{ ...phaseCard("CORE-1", "2026-11-17T07:00:00Z"), phase: "held" }] : [],
      })),
      {
        // The generated column ids name today's twelve: a newer server's column is none of them.
        id: "x_phase" as string as LegionBoardColumn["id"],
        label: "X phase",
        cards: [{ ...phaseCard("CORE-2", "2026-11-17T07:00:00Z"), phase: "x_phase" }],
      },
    ],
  });
  const row = await screen.findByRole("row", { name: "CORE-2 Work CORE-2" });
  expect(within(row).getByText("X phase")).toBeDefined();

  await user.click(screen.getByRole("button", { name: "Filter Legion phase" }));
  const options = within(screen.getByRole("listbox", { name: "Legion phase options" }))
    .getAllByRole("option")
    .map((option) => option.getAttribute("aria-label"));
  expect(options).toEqual([
    ...LEGION_PHASES.map(({ id, title }) => `${title}, ${id === "held" ? "1 issue" : "0 issues"}`),
    "X phase, 1 issue",
    "None, 1 issue",
  ]);
  await user.keyboard("{Escape}");

  expect(
    screen
      .getAllByRole("button", { name: /^Collapse / })
      .map((heading) => heading.getAttribute("aria-label"))
  ).toEqual(["Collapse Held, 1 issue", "Collapse X phase, 1 issue", "Collapse None, 1 issue"]);
});

/** The height of each row of the table in the geometry below. */
const ROW_PX = 64;

/**
 * The geometry the table and its anchor read, which the unit DOM has no layout engine for: the
 * box `width` px wide at `top()`, its header `head()` tall, each row ROW_PX tall under it, all
 * scrolled by the box's `scrollTop`; a hit test answers the row drawn at that height, and none
 * over the header.
 */
function tableGeometry({
  head,
  top,
  width = 1000,
}: {
  head: () => number;
  top: () => number;
  width?: number;
}) {
  const scrolled = () => screen.queryByTestId("issues-table-scroll")?.scrollTop ?? 0;
  const rows = () => [...document.querySelectorAll<HTMLElement>("tr[data-issue-row]")];
  // On `Element.prototype`, which owns the method (see above).
  const rect = spyOn(Element.prototype, "getBoundingClientRect").mockImplementation(function (
    this: Element
  ) {
    if (this instanceof HTMLElement && this.dataset.testid === "issues-table-scroll")
      return new DOMRect(0, top(), width, 600);
    if (this.tagName === "THEAD") return new DOMRect(0, top(), width, head());
    const index = rows().indexOf(this as HTMLElement);
    if (index >= 0)
      return new DOMRect(0, top() + head() + index * ROW_PX - scrolled(), width, ROW_PX);
    return new DOMRect(0, top() - scrolled(), width, 2000);
  });
  const point = spyOn(document, "elementFromPoint").mockImplementation((_x, y) =>
    y < top() + head()
      ? null
      : (rows()[Math.floor((y - top() - head() + scrolled()) / ROW_PX)] ?? null)
  );
  restores.push(() => {
    rect.mockRestore();
    point.mockRestore();
  });
  return {
    rowTop: (key: string) =>
      document.querySelector(`tr[data-issue-row="${key}"]`)?.getBoundingClientRect().top,
    scrolled,
  };
}

// R11: content that others add does not move a reader's place while it stays on screen. With no
// hand on a row and the box at its top, the anchor holds the first row on screen once the reader
// has seen it, so a row added above it lands out of view above, under the header, and the Open
// tab counts it at once.
test("a row added above the first row on screen lands out of view for a reader at the top", async () => {
  const geometry = tableGeometry({ head: () => 48, top: () => 300 });
  const rows = [issue("CORE-2"), issue("CORE-3")];
  const { client } = table(rows);
  await screen.findByRole("row", { name: "CORE-2 Work CORE-2" });
  // The rows painted: the reader has seen the first row on screen.
  await waitMs(50);
  const before = geometry.rowTop("CORE-2");
  expect(before).toBe(300 + 48);
  expect(geometry.scrolled()).toBe(0);

  rows.unshift(issue("CORE-1"));
  await act(() => client.invalidateQueries({ queryKey: ["issues"] }));
  await waitFor(() => expect(listedKeys()).toEqual(["CORE-1", "CORE-2", "CORE-3"]));

  expect(geometry.rowTop("CORE-2")).toBe(before);
  expect(geometry.scrolled()).toBe(ROW_PX);
  expect(geometry.rowTop("CORE-1")).toBe((before ?? 0) - ROW_PX);
  expect(screen.getByRole("tab", { name: "Open, 3 issues" })).toBeDefined();
});

// R11: in a project with a Legion daemon the board's first snapshot is part of Open's first draw.
// Open sorts by Queue and draws its rows before the board answers; the snapshot sorts them again,
// and the anchor holds nothing for it, even once the rows were painted: Open starts at its top
// with the Queue sort's first row, the issue with Place 1, whole under the header.
test("the board's first snapshot after Open's rows are painted starts Open at its top with the Queue sort's first row", async () => {
  const sockets = liveSockets(restores);
  const geometry = tableGeometry({ head: () => 48, top: () => 300 });
  table([issue("CORE-1"), issue("CORE-2"), issue("CORE-3")], "", {}, {}, Promise.resolve(DAEMON));
  await screen.findByRole("row", { name: "CORE-1 Work CORE-1" });
  expect(listedKeys()).toEqual(["CORE-1", "CORE-2", "CORE-3"]);
  // The rows painted before the board answers.
  await waitMs(50);
  await answerBoard(sockets, {
    ...legionBoard(),
    columns: [
      {
        id: "queue",
        label: "Queue",
        cards: [
          { ...waitingCard("CORE-3"), waiting_place: 1 },
          { ...waitingCard("CORE-2"), waiting_place: 2 },
        ],
      },
    ],
  });
  await waitFor(() => expect(listedKeys()).toEqual(["CORE-3", "CORE-2", "CORE-1"]));
  await waitMs(50);
  expect(geometry.scrolled()).toBe(0);
  expect(geometry.rowTop("CORE-3")).toBe(300 + 48);
});

// k13 (R11): a notice above the table moves the reader's place no more than an arrival does.
// With the hand on a row, a failed read's alert that appears above the table while its rows stay
// leaves that row's top where it was, and the box scrolls by the alert's height instead.
test("an alert above the table with the hand on a row leaves that row's top in place", async () => {
  const ALERT = 40;
  const geometry = tableGeometry({
    head: () => 48,
    top: () => 300 + (document.querySelector('[role="alert"]') === null ? 0 : ALERT),
  });
  const { client, listing } = table([issue("CORE-1"), issue("CORE-2"), issue("CORE-3")]);
  await screen.findByRole("row", { name: "CORE-2 Work CORE-2" });
  act(() => screen.getByRole("checkbox", { name: "Select CORE-2" }).focus());
  const before = geometry.rowTop("CORE-2");
  expect(before).toBe(300 + 48 + ROW_PX);

  listing.mockRejectedValueOnce(new Error("unreachable"));
  await act(() => client.invalidateQueries({ queryKey: ["issues"] }));
  expect((await screen.findByRole("alert")).textContent).toContain("Could not load issues.");

  expect(listedKeys()).toEqual(["CORE-1", "CORE-2", "CORE-3"]);
  expect(geometry.rowTop("CORE-2")).toBe(before);
  expect(geometry.scrolled()).toBe(ALERT);
});

// k13 (R11): the first draw of a view's rows starts at the top. On a 390 px phone the table
// first draws at the width it starts from, then measures its box and draws again before any frame
// is painted, and the Title header stacks its sort over its filter in a box under 640 px, 44 px
// taller. The rows move down under it; no reader has seen them yet, so nothing holds the first
// row where it was, and the box stays at its top with the first row whole under the header.
test("a table that draws its rows after a loading state opens at its top with its first row whole on a phone", async () => {
  const HEAD = 49;
  const STACKED = 44;
  // The Title header stacks in a box under 640 px (`STACKED_HEADERS`), in the draw that leads
  // with Select, Key and Title on a phone (R10): the draw where Key's header, not Priority's,
  // follows Select's.
  const titleStacked = () =>
    (document.querySelectorAll<HTMLElement>("thead th")[1]?.textContent ?? "").startsWith("Key");
  const geometry = tableGeometry({
    head: () => HEAD + (titleStacked() ? STACKED : 0),
    top: () => 300,
    width: 390,
  });
  table(
    ["CORE-1", "CORE-2", "CORE-3"].map((key) => issue(key, { status: "done" })),
    "?view=done"
  );
  expect(await screen.findByRole("status", { name: "Loading issues" })).toBeDefined();
  await screen.findByRole("row", { name: "CORE-1 Work CORE-1" });
  await waitMs(50);
  expect(titleStacked()).toBe(true);
  expect(geometry.scrolled()).toBe(0);
  expect(geometry.rowTop(listedKeys()[0] ?? "")).toBe(300 + HEAD + STACKED);
});

/** A card of the board's Implementing column, in its phase since `since`. */
function phaseCard(key: string, since: string): LegionBoardCard {
  return {
    ...waitingCard(key),
    phase: "implementing",
    phase_since: since,
    waiting: null,
    waiting_place: null,
  };
}

// k13 (R11), by a count and not by a clock: a live change to one issue draws that issue's card,
// not the Board's hundreds, and a card whose drawn time changed since the Board last drew. Every
// card body draws `UnreachableRouteMarker` once per render and every card shell calls
// `useDraggable` once, and on the Board nothing else does either, so their calls name the bodies
// and the shells a change drew. Of the 100 cards with Legion facts, 50 have been in their phase
// under a day, which reads anew each minute, and 50 between one and two days, ten minutes past
// an hour, which reads the same 61 s on. Those 50 are at work too (k13, found by INFRA-3221's
// goal red team): their Work time grows an hour an hour from 30 h 10 m, which reads 1 d 6 h at
// the start and 61 s on, so no text those cards draw changes and none of them draws again. One
// more card, CORE-1102, has no phase_since and has worked 2 h 30 m 30 s at the start, an hour an
// hour, which reads 2 h 31 m 61 s on: only its Work time can draw it again (k13).
test("a live read that moves one card of 600 draws at most 5 card bodies and 5 card shells", async () => {
  const start = new Date("2026-11-17T12:00:30Z");
  setSystemTime(start);
  restores.push(() => setSystemTime());
  const sockets = liveSockets(restores);
  const rows = Array.from({ length: 600 }, (_, index) =>
    issue(`CORE-${1000 + index}`, { status: index % 3 === 0 ? "in_progress" : "todo" })
  );
  const ago = (minutes: number) => new Date(start.getTime() - minutes * 60_000).toISOString();
  const ticking = rows.slice(0, 50).map((row) => row.key);
  const steady = rows.slice(50, 100).map((row) => row.key);
  // In progress, so neither move picks it.
  const working = rows[102]?.key ?? "";
  const { client } = table(rows, "?layout=board", {}, {}, Promise.resolve(DAEMON));
  await answerBoard(sockets, {
    ...legionBoard({
      ...Object.fromEntries(ticking.map((key) => [key, boardFacts(3)])),
      ...Object.fromEntries(steady.map((key) => [key, openFacts(ago(29 * 60 + 10))])),
      [working]: openFacts(ago(90.5)),
    }),
    columns: [
      {
        id: "implementing",
        label: "Implementing",
        cards: [
          ...ticking.map((key, index) => phaseCard(key, ago(30 + index))),
          ...steady.map((key, index) => phaseCard(key, ago(24 * 60 + (index % 20) * 60 + 10))),
          { ...phaseCard(working, start.toISOString()), phase_since: null },
        ],
      },
    ],
  });
  await waitFor(
    () => {
      expect(document.querySelectorAll("[data-board-card]")).toHaveLength(600);
      expect(document.querySelectorAll("[data-legion-fact=phase-time]")).toHaveLength(100);
    },
    { timeout: 30_000 }
  );
  /** The Work time each of the 50 steady cards draws. */
  const steadyWork = () =>
    steady.map(
      (key) =>
        document.querySelector(`[data-board-card="${key}"] [data-legion-fact=work-time]`)
          ?.textContent
    );
  expect(steadyWork()).toEqual(steady.map(() => expect.stringContaining("1 d 6 h")));
  /** The text of the working card's Legion fact, by its hook. */
  const workingFact = (hook: string) =>
    document.querySelector(`[data-board-card="${working}"] [data-legion-fact=${hook}]`)
      ?.textContent;
  expect(workingFact("work-time")).toBe("Work time2 h 30 m");
  expect(workingFact("phase-time")).toBeUndefined();
  const bodies = spyOn(routeReach, "UnreachableRouteMarker");
  const shells = spyOn(dndCore, "useDraggable");
  restores.push(() => {
    bodies.mockRestore();
    shells.mockRestore();
  });
  /** Moves the `nth` Todo card with no Legion card to In progress, as another reader's change
   *  read again, and gives its key and the cards whose bodies and shells drew for it. */
  const move = async (nth: number, updatedAt: string) => {
    bodies.mockClear();
    shells.mockClear();
    const index =
      rows.flatMap((row, at) => (at >= 100 && row.status === "todo" ? [at] : []))[nth] ?? 0;
    const key = rows[index]?.key ?? "";
    rows[index] = issue(key, { status: "in_progress", updated_at: updatedAt });
    await act(() => client.invalidateQueries({ queryKey: ["issues"] }));
    await waitFor(
      () =>
        expect(
          document.querySelector(`[data-board-column="in_progress"] [data-board-card="${key}"]`)
        ).not.toBeNull(),
      { timeout: 30_000 }
    );
    // Whatever the change still draws after the card lands is counted too.
    await waitMs(200);
    return {
      bodies: new Set(
        bodies.mock.calls.map(([props]) => {
          // The marker's props type names the route fields alone; a card body hands it the
          // card's whole issue, whose key names the body.
          const routed: object = props.issue;
          if (!("key" in routed) || typeof routed.key !== "string")
            throw new Error("A card body drew its route marker for an issue with no key");
          return routed.key;
        })
      ),
      key,
      shells: new Set(shells.mock.calls.map(([options]) => String(options.id))),
    };
  };

  const sameMinute = await move(0, "2026-11-17T12:00:40Z");
  for (const drawn of [sameMinute.bodies, sameMinute.shells]) {
    expect(drawn.size).toBeGreaterThanOrEqual(1);
    expect(drawn.size).toBeLessThanOrEqual(5);
    expect(drawn.has(sameMinute.key)).toBe(true);
    expect(drawn.has(working)).toBe(false);
  }
  expect(bodies.mock.calls.length).toBeLessThanOrEqual(5);
  expect(shells.mock.calls.length).toBeLessThanOrEqual(5);

  setSystemTime(new Date(start.getTime() + 61_000));
  const nextMinute = await move(1, "2026-11-17T12:01:35Z");
  for (const drawn of [nextMinute.bodies, nextMinute.shells]) {
    for (const key of ticking) expect(drawn.has(key)).toBe(true);
    expect(drawn.has(working)).toBe(true);
    for (const key of steady) expect(drawn.has(key)).toBe(false);
    const others = [...drawn].filter((key) => !ticking.includes(key) && key !== working);
    expect(others).toContain(nextMinute.key);
    expect(others.length).toBeLessThanOrEqual(5);
  }
  expect(steadyWork()).toEqual(steady.map(() => expect.stringContaining("1 d 6 h")));
  expect(workingFact("work-time")).toBe("Work time2 h 31 m");
}, 120_000);

/** The text of `key`'s cell under the column whose header starts with `name`. */
function cellText(key: string, name: string): string | null | undefined {
  const headers = screen.getAllByRole("columnheader").map((header) => header.textContent ?? "");
  const row = screen.getByRole("row", { name: `${key} Work ${key}` });
  const cells = within(row).getAllByRole("cell");
  return cells[headers.findIndex((text) => text.startsWith(name))]?.textContent;
}

// k5 (R4): the live view sends a board entry again only when its stored row moves, so the page
// grows an open issue's facts itself: each value plus its rate times the hours from as_of to the
// clock the rows are built at. G (CORE-1) worked 0.5 h, all of it in Implementing, as of 120
// minutes before the clock, each growing an hour an hour, so 2.5 h now; H (CORE-2) worked 1.5 h
// in Implementing with no growth. A build that draws or reads the entry as sent shows G at 30 m
// and 0.5 h, puts H first under both sorts, and leaves G out of 1 hour to 1 day.
test("an open row's facts are its board entry grown from its as_of to the page's clock, in its cell, its card, both sorts, the Work time filter and Time per phase in Hours", async () => {
  const user = userEvent.setup();
  const now = new Date("2026-11-17T12:00:00Z");
  setSystemTime(now);
  restores.push(() => setSystemTime());
  const sockets = liveSockets(restores);
  const cols = "cols=key,title,work_hours,phase_hours";
  table(
    [issue("CORE-1", { status: "in_progress" }), issue("CORE-2", { status: "in_progress" })],
    `?${cols}&sort=-work_hours`,
    {},
    {},
    Promise.resolve(DAEMON)
  );
  const asOf = new Date(now.getTime() - 120 * 60_000).toISOString();
  await answerBoard(sockets, {
    ...legionBoard({
      "CORE-1": {
        ...boardFacts(0.5),
        as_of: asOf,
        closed_at: null,
        growth: {
          hours_by_phase: { implementing: 1 },
          slot_hours: 1,
          work_hours: 1,
          work_last_at: 1,
        },
        work_last_at: asOf,
      },
      "CORE-2": { ...boardFacts(1.5), closed_at: null },
    }),
    columns: [{ id: "implementing", label: "Implementing", cards: [phaseCard("CORE-1", asOf)] }],
  });

  await waitFor(() => expect(cellText("CORE-1", "Work time")).toBe("2 h 30 m"));
  expect(cellText("CORE-2", "Work time")).toBe("1 h 30 m");
  expect(listedKeys()).toEqual(["CORE-1", "CORE-2"]);

  // Time per phase in its Hours mode writes each row's total, and its tooltip each phase's hours.
  await user.click(screen.getByRole("button", { name: "Hours" }));
  const g = () =>
    within(screen.getByRole("row", { name: "CORE-1 Work CORE-1" })).getByRole("button", {
      name: /^Time per phase, /,
    });
  await waitFor(() => expect(g().textContent).toBe("2.5 h"));
  await user.click(g());
  const line = screen
    .getByRole("tooltip")
    .querySelector<HTMLElement>('[data-phase-hours-line="implementing"]');
  expect(line?.textContent).toContain("2.5 h");
  await user.keyboard("{Escape}");

  act(() => go(`?${cols}&sort=-phase_hours`));
  await waitFor(() => expect(routeSearch()).toContain("sort=-phase_hours"));
  expect(listedKeys()).toEqual(["CORE-1", "CORE-2"]);

  act(() => go(`?${cols}&f.work_hours=1h-1d`));
  await waitFor(() => expect(routeSearch()).toContain("f.work_hours=1h-1d"));
  await waitFor(() => expect(listedKeys()).toContain("CORE-1"));

  act(() => go("?layout=board"));
  const region = await screen.findByRole("region", { name: "Project board" });
  await waitFor(() =>
    expect(
      region.querySelector('[data-board-card="CORE-1"] [data-legion-fact=work-time]')?.textContent
    ).toBe("Work time2 h 30 m")
  );
});

// k5 (R4): one build of the rows gives them one clock (`Item.at`): a row's Time in Legion phase,
// Work time and Time waiting, those of its card on the Board, and the filters that read them, are
// of that moment. A clock that moves on with no new board frame and no listing read draws them
// as they were: on the Board, in the table drawn again, under another sort, grouping and column
// set, and in each filter's Under 1 hour. A build that counted its times at each render would
// read 1 h 5 m, 2 h 40 m and 1 h 5 m 10 minutes on, and each filter would leave the issue out.
test("a row's and its card's Time in Legion phase, Work time and Time waiting, and their filters, are of the clock the rows were built at", async () => {
  const user = userEvent.setup();
  const start = new Date("2026-11-17T12:00:00Z");
  setSystemTime(start);
  restores.push(() => setSystemTime());
  const sockets = liveSockets(restores);
  const ago = (minutes: number) => new Date(start.getTime() - minutes * 60_000).toISOString();
  const cols = "cols=key,title,time_in_phase,work_hours,waited";
  const { listing } = table(
    [issue("CORE-1", { status: "in_progress" })],
    `?${cols}`,
    {},
    {},
    Promise.resolve(DAEMON)
  );
  await answerBoard(sockets, {
    ...legionBoard({ "CORE-1": openFacts(ago(90)) }),
    columns: [
      {
        id: "implementing",
        label: "Implementing",
        cards: [
          {
            ...phaseCard("CORE-1", ago(55)),
            waiting: { reason: "slot", since: ago(55), blocked_by: null },
          },
        ],
      },
    ],
  });
  /** The row's Time in Legion phase, Work time and Time waiting cells. */
  const cellTimes = () =>
    ["Time in Legion phase", "Work time", "Time waiting"].map((name) => cellText("CORE-1", name));
  const AT_BUILD = ["55 m", "2 h 30 m", "55 m"];
  await waitFor(() => expect(cellTimes()).toEqual(AT_BUILD));
  const reads = listing.mock.calls.length;

  // Ten minutes on, with no board frame and no listing read.
  setSystemTime(new Date(start.getTime() + 10 * 60_000));
  await user.click(screen.getByRole("button", { name: "Board" }));
  const region = await screen.findByRole("region", { name: "Project board" });
  const fact = (hook: string) =>
    region.querySelector(`[data-board-card="CORE-1"] [data-legion-fact=${hook}]`)?.textContent;
  await waitFor(() => expect(fact("phase-time")).toBe("Time in Legion phase55 m"));
  expect(fact("work-time")).toBe("Work time2 h 30 m");

  await user.click(screen.getByRole("button", { name: "Table" }));
  await waitFor(() => expect(cellTimes()).toEqual(AT_BUILD));

  // Another sort, a grouping and one more column draw the table again.
  const regrouped = `?${cols},review_rounds&sort=created&group=priority`;
  act(() => go(regrouped));
  await waitFor(() => expect(routeSearch()).toContain("group=priority"));
  await waitFor(() =>
    expect(screen.getAllByRole("columnheader").map((header) => header.textContent)).toContainEqual(
      expect.stringMatching(/^Review rounds/)
    )
  );
  expect(cellTimes()).toEqual(AT_BUILD);

  // Each filter's Under 1 hour, alone, lists the issue.
  for (const name of ["Time waiting", "Time in Legion phase"]) {
    act(() => go(regrouped));
    await user.click(await screen.findByRole("button", { name: new RegExp(`^Filter ${name}`) }));
    await user.click(await screen.findByRole("option", { name: /^Under 1 hour/ }));
    await user.keyboard("{Escape}");
    await waitFor(() => expect(routeSearch()).toContain("under-1h"));
    expect(listedKeys()).toEqual(["CORE-1"]);
  }
  expect(listing.mock.calls.length).toBe(reads);
});

/** Presses Finish on the interrupted action. */
async function pressFinish(user: UserEvent) {
  const recovery = await screen.findByRole("region", { name: "Interrupted table action" });
  await user.click(within(recovery).getByRole("button", { name: "Finish" }));
}

/** What the table says once an action ends. */
async function noticeOnceDone(text: string) {
  await waitFor(() =>
    expect(
      within(screen.getByRole("region", { name: "Project issues" })).getByRole("status").textContent
    ).toContain(text)
  );
}

/** Changes `key` in `rows`, which the table's listing answers from, so its next read finds it. */
function rewrite(rows: IssueSummary[], key: string, fields: Partial<IssueSummary>) {
  const at = rows.findIndex((row) => row.key === key);
  const row = rows[at];
  if (row !== undefined) rows[at] = { ...row, ...fields };
}

/** The label of each write that stands refused in Pending changes. */
function refusedWrites(client: QueryClient): string[] {
  return writesOf(client)
    .rows()
    .flatMap((write) => (write.status === "refused" ? [write.label] : []));
}

// A Start next whose rank write was refused is left to be finished. Finish waits on what is in
// flight and sends that write again as it was, and asks no question first: the table asks one
// only where a move needs a parent in work, which no rank write does.
test("Finish of a start next whose rank write was refused sends it again, with no question", async () => {
  const user = userEvent.setup();
  const rows = [
    issue("CORE-2", { labels: ["legion"], rank: "b" }),
    issue("CORE-3", { labels: ["legion"], rank: "a" }),
  ];
  const sent: (readonly ["PATCH", string, unknown])[] = [];
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

/** The registry's columns a table opens with all shown, by the names the Columns control gives
 *  them, but Updated and the Legion ones, and Epic, which no table has. */
const REGISTRY_OPENS = [
  "Priority",
  "Queue",
  "Key",
  "Title",
  "Status",
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

/** The columns the Issues tab's Open opens with in a project with no Legion daemon, by the names
 *  the Columns control gives them (`viewColumns`): Queue stays in the menu, unticked. With the
 *  row's checkbox, which no reader hides, that is 11. */
const OPEN_COLUMNS = [
  "Priority",
  "Key",
  "Title",
  "Status",
  "Created by",
  "Open asks",
  "Assignee",
  "Labels",
  "Claimed by",
  "Created",
];

test("a project's table with no daemon opens with Open's columns, and ticks neither Queue nor Updated", async () => {
  const user = userEvent.setup();
  table([issue("CORE-1")]);
  const list = await screen.findByRole("table", { name: "Project issues table" });
  const row = await within(list).findByRole("row", { name: "CORE-1 Work CORE-1" });
  expect(within(row).getByRole("checkbox", { name: "Select CORE-1" })).toBeDefined();
  await user.click(screen.getByRole("button", { name: "Columns" }));
  for (const name of OPEN_COLUMNS)
    expect({ name, ticked: ticked(name) }).toEqual({
      name,
      ticked: true,
    });
  for (const name of ["Queue", "Updated"])
    expect({ name, ticked: ticked(name) }).toEqual({
      name,
      ticked: false,
    });
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
  const { result, unmount } = renderHook(() => useTableLayout("fills-none", built, []));
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
  const wide = renderHook(() => useTableLayout("fills-narrow", narrow, []));
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
      ),
      []
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
    stateKey: "w",
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
      stateKey: "w",
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
      stateKey: "w",
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

// A phone keeps its own widths, whatever the layout names: Key at a table another page draws' phone
// width, 100 px, which holds its header whole, sorted by it or not (R13, `EMBEDDED_WIDTHS`), and
// Title what the pins' 85% of the box leaves: 331.5 - 44 - 100.
test("the same layout in a 390 px phone's box keeps the phone's widths and stacks Title's header", async () => {
  expect(await narrowHeaders(390)).toEqual({
    cells: ["- 44px", "Key 100px", "Title 187.5px", "Project 72px", "Status 150px"],
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
    stateKey: "w",
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
