import { afterEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type {
  ColumnDef,
  ColumnFiltersState,
  GroupingState,
  PaginationState,
  SortingState,
} from "@tanstack/react-table";
// biome-ignore lint/style/noRestrictedImports: each case reads the hand-off right after the press, in the task the press is made in: a press that holds nothing is armed only until a frame and a task after it, and user-event waits a task after every press it makes, which leaves no form for that read.
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";
import { type ReactNode, useState } from "react";
import { MemoryRouter } from "react-router-dom";

import { disarm } from "../__tests__/armed-press";
import { layOutLine } from "../__tests__/chips-line";
import { DEPLOY_NAMES, DEPLOY_NOUN } from "../__tests__/deploy-table";
import { settle } from "../__tests__/fake-clock";
import { panelOf } from "../__tests__/list-picks";
import {
  type FakeLiveSocket,
  installFakeLiveSockets,
  snapshotFrame,
} from "../__tests__/live-socket-harness";
import { ApiError, api } from "../api/client";
import type {
  Agent,
  DeliveryMeasuresResponse,
  DeliveryTimelineWithMeasures,
  Issue,
  IssueDetails,
  IssueSummary,
} from "../api/types";
import { AgentsPage } from "../features/agents/AgentsPage";
import { DeliveryPage } from "../features/delivery/DeliveryPage";
import { idlePriorityWrite, mockHeaderReads } from "../features/issue/header-fixture";
import { IssueHeader } from "../features/issue/IssueHeader";
import { stateForIssue } from "../features/issue/pins";
import { claimIssue } from "../features/issue-page/issue-fixtures";
import { MarginProvider } from "../features/margin/margin-context";
import { IssuesTable } from "../features/project/IssuesTable";
import { KeymapProvider } from "../features/shell/KeymapProvider";
import { issueProject } from "../lib/issue-key";
import { Chip } from "./Chip";
import { ControlRow } from "./ControlRow";
import { DataTable, DataTableCells, useDataTable } from "./DataTable";
import { DisplayMenu } from "./DisplayMenu";
import { causedByReader } from "./focus-place";
import { PanelTrigger } from "./PanelTrigger";
import { ROW_MARKER } from "./row-marker";
import { SelectionBar } from "./SelectionBar";
import { useTableLayout } from "./table-layout";

// The inventory of the component hand-offs' presses (`armHandOff`): every component that hands
// its focus off arms that hand-off at its own root (`armOnPress`), so that a loss its press causes
// is read as the reader's (`causedByReader`), and with nothing armed a loss is answered as the page
// answered it before it had a hand-off. Each case draws the real component, makes one press of one
// of its controls through the event path a reader's press takes - a click, a pick, or a key the
// press acts at - and reads the hand-off at once: nothing of that control's was armed before the
// press, and the press armed it. A press whose control arms nothing fails here. The pages' members
// are drawn on the pages themselves, in the shell's main content, as their own test files draw
// them, and their cases at the end of this file also follow the press to where its commit hands
// the focus.

/** Puts back what a case replaced, after it. */
const restores: (() => void)[] = [];
afterEach(() => {
  for (const restore of restores.splice(0)) restore();
  // A press a case left armed ends, so that none outlives its case.
  disarm();
});

/** Draws `tree`, unmounted after the case. */
function draw(tree: ReactNode): void {
  const view = render(tree);
  restores.push(() => view.unmount());
}

/**
 * Expects `press` - a click on `control` unless a case names another - to arm the hand-off for
 * `control`: no loss of it was the reader's before the press, and one is right after it, in the
 * task the press was made in (`causedByReader`).
 */
function expectArms(control: Element, press: () => void = () => fireEvent.click(control)): void {
  expect(causedByReader(control)).toBe(false);
  press();
  expect(causedByReader(control)).toBe(true);
}

/** A page's control row: a removable chip per name, Clear filters, a search box, the filter
 *  trigger, and in the page's own slot Mark all read, which goes once it has run. */
function FilterRow({ names }: { readonly names: readonly string[] }): ReactNode {
  const [on, setOn] = useState(names);
  const [unread, setUnread] = useState(true);
  return (
    <ControlRow
      chips={
        on.length === 0
          ? undefined
          : on.map((name) => (
              <Chip
                aria-label={`Remove ${name}`}
                key={name}
                onClick={() => setOn((chips) => chips.filter((chip) => chip !== name))}
                removable
                selected
              >
                {name}
              </Chip>
            ))
      }
      onClear={() => setOn([])}
      search={<input aria-label="Search" type="search" />}
      trigger={
        <button data-control="panel-trigger" type="button">
          Filter
        </button>
      }
    >
      {unread ? (
        <button onClick={() => setUnread(false)} type="button">
          Mark all read
        </button>
      ) : null}
    </ControlRow>
  );
}

/** Four chips of 100 px, by the words each draws with its `×`, laid out as `ControlRow.test.tsx`
 *  lays them out (`layOutLine`): a 300 px line holds two of them beside `+2 more`. */
const FOUR_CHIPS = { "Alpha×": 100, "Beta×": 100, "Delta×": 100, "Gamma×": 100 };

test("ControlRow: a chip's ×, pressed on its glyph, arms the row's hand-off", () => {
  draw(<FilterRow names={["Alpha", "Beta"]} />);
  const chip = screen.getByRole("button", { name: "Remove Beta" });
  const glyph = within(chip).getByText("×");
  // The arm is read in the task the click is made in, which user-event's own click waits past.
  expectArms(chip, () => fireEvent.click(glyph));
});

test("ControlRow: Clear filters arms the row's hand-off", () => {
  draw(<FilterRow names={["Alpha", "Beta"]} />);
  expectArms(screen.getByRole("button", { name: "Clear filters" }));
});

test("ControlRow: +N more arms the row's hand-off", () => {
  restores.push(layOutLine({ width: 300 }, FOUR_CHIPS));
  draw(<FilterRow names={["Alpha", "Beta", "Gamma", "Delta"]} />);
  expectArms(screen.getByRole("button", { name: "2 more filters" }));
});

test("ControlRow: a chip in the panel +N more opens arms the row's hand-off", async () => {
  const user = userEvent.setup();
  restores.push(layOutLine({ width: 300 }, FOUR_CHIPS));
  draw(<FilterRow names={["Alpha", "Beta", "Gamma", "Delta"]} />);
  await user.click(screen.getByRole("button", { name: "2 more filters" }));
  const panel = screen.getByRole("dialog", { name: "2 more filters" });
  expectArms(within(panel).getByRole("button", { name: "Remove Delta" }));
});

test("ControlRow: a page's own control in a slot, the Inbox's Mark all read, arms the row's hand-off", () => {
  draw(<FilterRow names={[]} />);
  expectArms(screen.getByRole("button", { name: "Mark all read" }));
});

/** A row that is not an issue, as `DataTable.test.tsx` draws one. */
interface Deploy {
  readonly id: string;
  readonly service: string;
}

const DEPLOY_COLUMNS: ColumnDef<Deploy>[] = [
  { accessorFn: (row) => row.id, enableColumnFilter: false, header: "Deploy", id: "deploy" },
  { accessorFn: (row) => row.service, enableColumnFilter: false, header: "Service", id: "service" },
];

/** A caller of the table core whose every row draws Roll back in its trailing cell, which goes
 *  once it has run while the row stays, as `DataTable.test.tsx`'s deploys do. */
function Deploys(): ReactNode {
  const [sorting, setSorting] = useState<SortingState>([]);
  const [grouping, setGrouping] = useState<GroupingState>([]);
  const [columnFilters, setColumnFilters] = useState<ColumnFiltersState>([]);
  const [globalFilter, setGlobalFilter] = useState("");
  const [pagination, setPagination] = useState<PaginationState>({ pageIndex: 0, pageSize: 25 });
  const [rolledBack, setRolledBack] = useState<ReadonlySet<string>>(new Set());
  const layout = useTableLayout("inventory-deploys", {
    ids: ["deploy", "service"],
    rowAttribute: "data-deploy-row",
  });
  const state = { columnFilters, globalFilter, grouping, pagination, sorting };
  const model = useDataTable<Deploy>({
    columns: DEPLOY_COLUMNS,
    getRowId: (row) => row.id,
    layout,
    names: DEPLOY_NAMES,
    onColumnFiltersChange: setColumnFilters,
    onGlobalFilterChange: setGlobalFilter,
    onGroupingChange: setGrouping,
    onPaginationChange: setPagination,
    onSortingChange: setSorting,
    rows: [
      { id: "d1", service: "billing" },
      { id: "d2", service: "search" },
    ],
    state,
    viewKey: JSON.stringify(state),
  });
  return (
    <DataTable
      empty="No deploys match."
      filterCounts={new Map()}
      label="Deploys"
      model={model}
      noun={DEPLOY_NOUN}
      onOpenFilterChange={() => undefined}
      openFilter={undefined}
      renderGroup={() => null}
      renderRow={({ row, index, measure }) => (
        <tr data-deploy-row={row.id} data-index={index} key={row.id} ref={measure} tabIndex={-1}>
          <DataTableCells
            row={row}
            trailing={
              rolledBack.has(row.id) ? null : (
                <button
                  onClick={() => setRolledBack((done) => new Set(done).add(row.id))}
                  type="button"
                >
                  Roll back {row.id}
                </button>
              )
            }
          />
        </tr>
      )}
      scrollTestId="inventory-deploys-scroll"
      trailingWidth={104}
    />
  );
}

/** Gives the box the rows scroll in, by its test id, a window's height for the case, as the unit
 *  DOM lays nothing out, and clears what the table stored once the case is over. */
function scrollBoxHeight(testId: string): void {
  const height = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "clientHeight");
  Object.defineProperty(HTMLElement.prototype, "clientHeight", {
    configurable: true,
    get() {
      return this.dataset.testid === testId ? 600 : (height?.get?.call(this) ?? 0);
    },
  });
  restores.push(() => {
    if (height) Object.defineProperty(HTMLElement.prototype, "clientHeight", height);
    else Reflect.deleteProperty(HTMLElement.prototype, "clientHeight");
    localStorage.clear();
  });
}

test("DataTable: a row's own action arms the table's hand-off", async () => {
  // The unit DOM lays nothing out: the box the rows scroll in gets a window's height, so the
  // table draws its rows.
  scrollBoxHeight("inventory-deploys-scroll");
  draw(<Deploys />);
  expectArms(await screen.findByRole("button", { name: "Roll back d2" }));
});

/** A trigger whose panel holds one control, as the search bar's filter panel holds its Author. */
function AuthorPanel(): ReactNode {
  const [open, setOpen] = useState(false);
  return (
    <PanelTrigger label="Filter" onOpenChange={setOpen} open={open}>
      {() => <button type="button">Author</button>}
    </PanelTrigger>
  );
}

test("PanelTrigger: a control in the panel arms the panel's hand-off", async () => {
  const user = userEvent.setup();
  draw(<AuthorPanel />);
  await user.click(screen.getByRole("button", { name: "Filter" }));
  const panel = screen.getByRole("dialog", { name: "Filter" });
  expectArms(within(panel).getByRole("button", { name: "Author" }));
});

/** The display menu at a row's end, which draws its panel through `PanelTrigger`. */
function ViewMenu(): ReactNode {
  const [open, setOpen] = useState(false);
  return (
    <DisplayMenu
      label="View"
      onOpenChange={setOpen}
      open={open}
      sections={[
        {
          id: "sort",
          label: "Sort by",
          onChange: () => undefined,
          options: [
            { label: "Newest first", value: "newest" },
            { label: "Urgency", value: "urgency" },
          ],
          value: "urgency",
        },
      ]}
    />
  );
}

test("DisplayMenu: a choice in its panel, made through PanelTrigger, arms the panel's hand-off", async () => {
  const user = userEvent.setup();
  draw(<ViewMenu />);
  await user.click(screen.getByRole("button", { name: "View" }));
  expectArms(screen.getByRole("radio", { name: "Newest first" }));
});

/** Three rows of a list, each carrying the row marker and its mark, and the bar over them with
 *  Clear selection, as `SelectionBar.test.tsx`'s marked table draws them. */
function MarkedRows(): ReactNode {
  return (
    <>
      {["CORE-1", "CORE-2", "CORE-3"].map((key) => (
        <div key={key} {...ROW_MARKER}>
          <input aria-label={`Select ${key}`} type="checkbox" />
        </div>
      ))}
      <SelectionBar count={1} label="Selected rows" returnFocus={() => null}>
        <button type="button">Clear selection</button>
      </SelectionBar>
    </>
  );
}

test("SelectionBar: an action of the bar arms the bar's hand-off", () => {
  draw(<MarkedRows />);
  expectArms(screen.getByRole("button", { name: "Clear selection" }));
});

test("SelectionBar: a row's mark, outside the bar, arms the bar's hand-off while the bar is drawn", () => {
  draw(<MarkedRows />);
  expectArms(screen.getByRole("checkbox", { name: "Select CORE-2" }));
});

/** `MarkedRows` with one action in the bar, Move to, whose Escape clears the selection, as a
 *  page's keymap clears it from a control inside the bar; the bar hands its focus to the second
 *  row's mark. */
function EscapableBar(): ReactNode {
  const [count, setCount] = useState(1);
  return (
    <>
      {["CORE-1", "CORE-2", "CORE-3"].map((key) => (
        <div key={key} {...ROW_MARKER}>
          <input aria-label={`Select ${key}`} type="checkbox" />
        </div>
      ))}
      <SelectionBar
        count={count}
        label="Selected rows"
        returnFocus={() => screen.getByRole("checkbox", { name: "Select CORE-2" })}
      >
        <button
          onKeyDown={(event) => {
            if (event.key === "Escape") setCount(0);
          }}
          type="button"
        >
          Move to
        </button>
      </SelectionBar>
    </>
  );
}

// Escape from a control inside the bar is the reader's own going of the bar (`SelectionBar`), so
// the bar's root arms its hand-off on Escape, as no other component's root does: the bar that
// Escape empties hands the focus to what the page names, not to the body.
test("SelectionBar: Escape from a control inside the bar arms the bar's hand-off, and the bar it empties hands the focus to what returnFocus names", () => {
  draw(<EscapableBar />);
  const move = screen.getByRole("button", { name: "Move to" });
  act(() => move.focus());
  expectArms(move, () => fireEvent.keyDown(move, { key: "Escape" }));
  expect(screen.queryByRole("toolbar")).toBeNull();
  expect(screen.getByRole("checkbox", { name: "Select CORE-2" })).toHaveFocus();
});

/** The shell's main content, which a page is drawn in as the app draws it, after a link that
 *  stands for the shell's navigation; both go after the case. */
function mainContent(): HTMLElement {
  const navigation = document.createElement("a");
  navigation.href = "/";
  navigation.textContent = "Inbox";
  const main = document.createElement("main");
  document.body.append(navigation, main);
  restores.push(() => {
    main.remove();
    navigation.remove();
  });
  return main;
}

/** Draws `page` at `url` in the main content (`mainContent`), with the providers a page reads
 *  through, as the page's own test file draws it. Answers what unmounts it, which the case calls
 *  itself where the page's own timers must stop inside its body. */
function drawPage(page: ReactNode, url: string, client = plainClient()): () => void {
  const view = render(
    <MemoryRouter initialEntries={[url]}>
      <QueryClientProvider client={client}>
        <KeymapProvider>{page}</KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>,
    { container: mainContent() }
  );
  let unmounted = false;
  const unmount = () => {
    if (unmounted) return;
    unmounted = true;
    view.unmount();
    client.clear();
  };
  restores.push(unmount);
  return unmount;
}

/** A query client that does not retry a read, as the pages' own test files read through. */
function plainClient(): QueryClient {
  return new QueryClient({ defaultOptions: { queries: { retry: false } } });
}

/** Puts back `spies`, the case's answers to the reads of `api` a page makes, after the case. */
function answering(...spies: { mockRestore(): void }[]): void {
  restores.push(() => {
    for (const spy of spies) spy.mockRestore();
  });
}

/** When the file loaded: the agents' activity is dated from it. */
const now = Date.now();

/** Two running agents, as `AgentsPage.test.tsx` lists them. */
const AGENTS = [
  {
    capabilities: ["aside", "btw"],
    dir: "/workspaces/planner",
    host: "build-host",
    last_activity: new Date(now - 60_000).toISOString(),
    last_seen: now - 30_000,
    machine_id: "build-host",
    open_asks: 0,
    roles: ["planner"],
    session_id: "planner-session",
    state: "running",
    title: "Planner",
  },
  {
    capabilities: ["aside"],
    dir: "/workspaces/reviewer",
    host: "review-host",
    last_activity: new Date(now - 10 * 60_000).toISOString(),
    last_seen: now - 5 * 60_000,
    machine_id: "review-host",
    open_asks: 0,
    roles: [],
    session_id: "reviewer-session",
    state: "running",
    title: "Reviewer",
  },
] satisfies Agent[];

/** The Agents page, its reads answered as `AgentsPage.test.tsx` answers them, and its list's
 *  region once the agents are listed. */
async function drawAgents(): Promise<HTMLElement> {
  answering(
    spyOn(api, "whoAmI").mockResolvedValue({ event_head: "0", kind: "user", login: "alice" }),
    spyOn(api, "getInbox").mockResolvedValue([]),
    spyOn(api, "listAgents").mockResolvedValue(AGENTS),
    spyOn(api, "listAgentMessages").mockResolvedValue([]),
    spyOn(api, "listIssues").mockResolvedValue([]),
    spyOn(api, "listProjects").mockResolvedValue([]),
    spyOn(api, "getMyAgentState").mockResolvedValue({})
  );
  drawPage(<AgentsPage />, "/agents");
  const region = await screen.findByRole("region", { name: "Agents" });
  await within(region).findByRole("heading", { name: "Planner" });
  return region;
}

/** The Agents list's select-all checkbox, in its header (`SelectionHeader`). */
function selectAll(region: HTMLElement): HTMLElement {
  return within(region).getByRole("checkbox", { name: "Select all matching agents" });
}

test("AgentsPage: the header's select-all arms the header's hand-off", async () => {
  const region = await drawAgents();
  expectArms(selectAll(region));
});

test("AgentsPage: the header's Clear selection arms the header's hand-off", async () => {
  const user = userEvent.setup();
  const region = await drawAgents();
  await user.click(selectAll(region));
  expectArms(within(region).getByRole("button", { name: "Clear selection" }));
});

// The header's armed hand-off (`SelectionHeader`): Clear selection goes with the selection it
// empties, and its focus goes to the select-all checkbox before it. While no row matches, that
// checkbox is disabled and the header holds no other control, so the header's own hand-off walks
// back over the main content it was in (`walkBackInRegion`) to the Filter button, and never out
// to the navigation. The page's control row draws the search box first and the Filter button
// after it (`ControlRow`), so the walk reaches the Filter button before the search box.
test("AgentsPage: Clear selection hands its focus to the select-all checkbox, and with no row matching walks back to the Filter button", async () => {
  const user = userEvent.setup();
  const region = await drawAgents();
  const header = selectAll(region);
  const search = within(region).getByRole("searchbox", { name: "Search agents" });
  const clear = () => within(region).getByRole("button", { name: "Clear selection" });
  await user.click(header);
  act(() => clear().focus());
  await user.keyboard("{Enter}");
  expect(within(region).queryByRole("button", { name: "Clear selection" })).toBeNull();
  expect(header).toHaveFocus();
  // Every row selected, then a search that matches none of them: the checkbox is disabled.
  await user.click(header);
  await user.click(search);
  await user.paste("nothing matches this");
  await waitFor(() => expect(header).toBeDisabled());
  act(() => clear().focus());
  await user.keyboard("{Enter}");
  expect(within(region).queryByRole("button", { name: "Clear selection" })).toBeNull();
  await waitFor(() =>
    expect(within(region).getByRole("button", { name: "Filter · 0" })).toHaveFocus()
  );
  expect(search).not.toHaveFocus();
  expect(screen.getByRole("link", { name: "Inbox" })).not.toHaveFocus();
});

/** The Delivery page's list view over one day, which needs no chart, as `DeliveryPage.test.tsx`
 *  draws it, and the same with a brush window set in it, so the header draws clear brush window. */
const DELIVERY_URL =
  "/delivery?mode=list&from=2024-06-01T00%3A00%3A00Z&to=2024-06-02T00%3A00%3A00Z";
const BRUSHED_URL = `${DELIVERY_URL}&ws=${encodeURIComponent("2024-06-01T06:00:00.000Z")}&we=${encodeURIComponent("2024-06-01T12:00:00.000Z")}`;

const EMPTY_FRESHNESS: DeliveryTimelineWithMeasures["freshness"] = {
  last_error: null,
  last_event_at: null,
  last_reconcile_at: null,
  unfetchable_count: 0,
};

/** The measures of the page's window, with nothing in it (`DeliveryPage.test.tsx`'s). */
const EMPTY_MEASURES: DeliveryMeasuresResponse = {
  window: { from: "2024-06-01T00:00:00Z", to: "2024-06-02T00:00:00Z" },
  computed_at: "2024-06-02T00:00:00Z",
  measures: {
    deploy_frequency: {
      successful_deploys: 0,
      deploys_with_prs: 0,
      per_day: 0,
      with_prs_per_day: 0,
    },
    lead_time: {
      merge_to_production: { median_minutes: null, p90_minutes: null, max_minutes: null },
      first_commit_to_production: { median_minutes: null, p90_minutes: null, max_minutes: null },
      opened_to_production: { median_minutes: null, p90_minutes: null, max_minutes: null },
      opened_to_merge: { median_minutes: null, p90_minutes: null, max_minutes: null },
    },
    change_failure_rate: {
      per_pr: { confirmed: 0, pending: 0, rejected: 0, reverts: 0, total: 0, rate: 0 },
      per_deploy: {
        confirmed: 0,
        pending: 0,
        rejected: 0,
        reverts: 0,
        total: 0,
        rate: 0,
        confirmed_or_pending: 0,
        upper_bound_rate: 0,
      },
    },
    time_to_restore: { median_minutes: null },
    rework_share: 0,
    deploy_run_success: { concluded: 0, reached_production: 0, cancelled: 0, rate: null },
    daily: [],
  },
  flags_source: "none",
  unowned_p0: [],
  targets: {
    deploys_per_day: 20,
    change_failure_rate: 0.05,
    merge_to_production_minutes: 45,
    opened_to_merge_median_minutes: 60,
    deploy_run_success_rate: 0.9,
    unowned_p0: 0,
  },
  status: {
    deploys_per_day: false,
    change_failure_rate: null,
    change_failure_rate_upper_bound: null,
    merge_to_production_median: null,
    merge_to_production: null,
    opened_to_merge: null,
    deploy_run_success: null,
    unowned_p0: true,
  },
  freshness: EMPTY_FRESHNESS,
};

/** The page's timeline with nothing in it (`DeliveryPage.test.tsx`'s). */
const EMPTY_TIMELINE: DeliveryTimelineWithMeasures = {
  color_counts: { repo: {}, author: {}, priority: {}, component: {}, parent_agent: {} },
  components: {},
  facet_counts: {
    repo: {},
    parent_agent: {},
    session: {},
    issue: {},
    priority: {},
    component: {},
    author: {},
    rework: {},
    deployed: {},
  },
  freshness: EMPTY_FRESHNESS,
  issue_titles: {},
  measures: EMPTY_MEASURES,
  prs: [],
  runs: [],
  session_groups: [],
  session_pr_counts: {},
  session_titles: {},
  waiting: [],
  window: { from: "2024-06-01T00:00:00Z", to: "2024-06-02T00:00:00Z" },
};

/** The Delivery page at `url`, its live socket open and its timeline answered with nothing in it.
 *  Answers what unmounts it: the page's search debounce and its freshness tick are real timers,
 *  so a case unmounts it inside its own body, before a turn of the event loop could fire them
 *  outside `act` (`DeliveryPage.test.tsx`'s rows do the same). */
async function drawDelivery(url: string): Promise<() => void> {
  const sockets = installFakeLiveSockets();
  restores.push(() => sockets.restore());
  const unmount = drawPage(<DeliveryPage />, url);
  await waitFor(() => expect(sockets.sockets.length).toBeGreaterThan(0));
  const socket: FakeLiveSocket = sockets.latest();
  act(() => socket.open());
  const timeline = await waitFor(() => socket.subscribed("timeline"));
  act(() => socket.receive(snapshotFrame(timeline.sub, EMPTY_TIMELINE)));
  return unmount;
}

test("DeliveryPage: the header's clear brush window arms the header's hand-off", async () => {
  const unmount = await drawDelivery(BRUSHED_URL);
  try {
    expectArms(await screen.findByRole("button", { name: "clear brush window" }));
  } finally {
    unmount();
  }
});

test("DeliveryPage: a pick in the header's Window list arms the header's hand-off", async () => {
  const user = userEvent.setup();
  const unmount = await drawDelivery(DELIVERY_URL);
  try {
    const trigger = await screen.findByLabelText("Window");
    await user.click(trigger);
    expectArms(await within(panelOf(trigger)).findByRole("option", { name: "Last 84 days" }));
  } finally {
    unmount();
  }
});

/** An issue claimed by a person, as the issue page reads it, so today's issue header draws the
 *  claim with Release beside it (`IssueHeader.test.tsx`'s claimed issue). */
const CLAIMED: IssueDetails = {
  ...claimIssue,
  artifacts: [],
  children: [],
  claim: { actor: { id: "alice", kind: "user" }, at: "2026-09-24T06:00:00Z" },
  design_gates: [],
  holder_state: null,
  last_seen: null,
  open_asks: [],
  progress: { tasks: null, children: null },
  referenced_by_count: 0,
  route_holder: null,
  route_status: null,
  route_status_reason: null,
  takeable_at: null,
};

test("IssueHeader: Release arms the Attention group's hand-off", async () => {
  restores.push(mockHeaderReads());
  answering(
    spyOn(api, "getIssueSubscribers").mockResolvedValue([]),
    // The release stays out: the case reads the arm, not what the release's answer does.
    spyOn(api, "releaseIssueClaim").mockReturnValue(new Promise(() => undefined))
  );
  drawPage(
    <MarginProvider>
      <IssueHeader
        documentArtifact={undefined}
        isClosed={false}
        issue={CLAIMED}
        priorityWrite={idlePriorityWrite}
        state={stateForIssue(undefined, CLAIMED.key)}
      />
    </MarginProvider>,
    `/issues/${CLAIMED.key}`
  );
  expectArms(await screen.findByRole("button", { name: `Release the claim on ${CLAIMED.key}` }));
  // The release the press sent draws its pending state as it goes out, inside `act`.
  await act(() => settle());
});

/** An issue as the project List's listing answers it (`IssuesTable.test.tsx`'s). */
function issue(key: string, overrides: Partial<IssueSummary> = {}): IssueSummary {
  return {
    key,
    project: issueProject(key),
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

/** An issue as a write answers it (`IssuesTable.test.tsx`'s). */
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

/** The project List over `issues` at `search`, as `IssuesTable.test.tsx`'s `table()` draws it,
 *  in the shell's main content. `open` answers the listing of the open issues alone in place of
 *  `issues`, as a read the case holds out. */
function drawList(issues: IssueSummary[], search = "", open?: Promise<IssueSummary[]>): void {
  answering(
    spyOn(api, "listIssues").mockImplementation(async (options = {}) => {
      if (options.open && open) return open;
      return issues.filter((each) => !options.open || each.status !== "done");
    }),
    spyOn(api, "getMyState").mockResolvedValue({}),
    spyOn(api, "listAgents").mockResolvedValue([]),
    spyOn(api, "whoAmI").mockResolvedValue({ event_head: "0", kind: "user", login: "alice" })
  );
  // The unit DOM lays nothing out: the box the rows scroll in gets a window's height.
  scrollBoxHeight("issues-table-scroll");
  const client = plainClient();
  client.setQueryData(["whoami"], { event_head: "0", login: "alice", kind: "user" });
  drawPage(<IssuesTable listing={{ project: "CORE" }} />, `/projects/CORE/issues${search}`, client);
}

/** The List opened on its Done and Todo issues, its Status column filter opened, and the filter's
 *  text box, which holds the focus. */
async function openStatusFilter(user: UserEvent, open?: Promise<IssueSummary[]>) {
  drawList(
    [issue("CORE-1"), issue("CORE-2", { status: "done" })],
    "?status=done&status=todo",
    open
  );
  await screen.findByRole("row", { name: "CORE-2 Work CORE-2" });
  await user.click(screen.getByRole("button", { name: /^Filter Status/ }));
  const box = await screen.findByRole("combobox", { name: "Search Status" });
  expect(box).toHaveFocus();
  return box;
}

test("the List: a column filter's trigger in the table's head arms the List's hand-off", async () => {
  drawList([issue("CORE-1")]);
  expectArms(await screen.findByRole("button", { name: /^Filter Status/ }));
});

test("the List: a pick in a column filter's list, portalled out of the List, arms the List's hand-off", async () => {
  const user = userEvent.setup();
  await openStatusFilter(user);
  expectArms(screen.getByRole("option", { name: /^Done/ }));
});

test("the List: a column filter pick made by Enter in its text box arms the List's hand-off", async () => {
  const user = userEvent.setup();
  const box = await openStatusFilter(user);
  await user.paste("Done");
  // The arm is read in the task the key is pressed in, which user-event's keyboard waits past.
  expectArms(box, () => fireEvent.keyDown(box, { key: "Enter" }));
});

/** The List over CORE-1 with an interrupted Start next of it left in the browser's store, so the
 *  List draws the interrupted action's notice with Finish and Dismiss. */
async function interruptedList(): Promise<HTMLElement> {
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
  answering(spyOn(api, "send").mockResolvedValue(savedIssue("CORE-1") as never));
  drawList([issue("CORE-1")]);
  return screen.findByRole("region", { name: "Interrupted table action" });
}

/** The List over CORE-1 with a New epic from it refused as a possible duplicate, so the List draws
 *  the duplicate's notice with Create anyway and Cancel new epic. */
async function duplicateEpicList(user: UserEvent): Promise<HTMLElement> {
  answering(
    spyOn(api, "createIssue").mockImplementation(async (input) => {
      if (!input.force) {
        throw new ApiError(409, {
          code: "POSSIBLE_DUPLICATE",
          error: "Possible duplicate of CORE-9",
        });
      }
      return savedIssue("CORE-9");
    }),
    spyOn(api, "send").mockResolvedValue(savedIssue("CORE-1"))
  );
  // On the backlog: main's parent-status rule (bcc5d7e66d) needs no epic in work for it, so the
  // create is the action's first write, with no question before it.
  drawList([issue("CORE-1", { status: "backlog" })]);
  await user.click(await screen.findByRole("checkbox", { name: "Select CORE-1" }));
  await user.click(screen.getByRole("button", { name: "New epic from these" }));
  const dialog = await screen.findByRole("dialog", { name: /epic/i });
  await user.type(within(dialog).getByRole("textbox", { name: "Name" }), "Existing epic");
  await user.click(within(dialog).getByRole("button", { name: "Create epic" }));
  return screen.findByRole("alert");
}

/** Waits until the List has settled what its notice's press started: the notice gone, and the
 *  promises the press's writes and reads chained, which the case's stubs answer at once, run as
 *  far as they go inside `act` (`settle`), so nothing of them lands after the case. */
async function noticeSettled(region: HTMLElement): Promise<void> {
  await waitFor(() => expect(region.isConnected).toBe(false));
  await act(() => settle());
}

for (const press of ["Finish", "Dismiss"]) {
  test(`the List: the interrupted action's ${press} arms the List's hand-off`, async () => {
    const recovery = await interruptedList();
    expectArms(within(recovery).getByRole("button", { name: press }));
    await noticeSettled(recovery);
  });
}

for (const press of ["Create anyway", "Cancel new epic"]) {
  test(`the List: the possible duplicate's ${press} arms the List's hand-off`, async () => {
    const user = userEvent.setup();
    const duplicate = await duplicateEpicList(user);
    expectArms(within(duplicate).getByRole("button", { name: press }));
    await noticeSettled(duplicate);
  });
}

// The List's armed hand-off (`IssuesTable`): a column filter's pick that sends the List off to
// read the issues it has not loaded takes the table's head away with the filter's own list, and
// the List's hand-off walks back over the main content it was in (`walkBackInRegion`), from the
// place the head left, to the toolbar's Clear filters, never out to the navigation. The toolbar
// draws the search box first and Clear filters last, after the Status chip (`ControlRow`), so the
// walk reaches Clear filters before the search box. By the pointer's pick, and by Enter in the
// filter's text box.
for (const way of ["a pointer's pick", "Enter in its text box"]) {
  test(`the List: a column filter pick by ${way} that sends the List to read its issues walks back to the toolbar's Clear filters`, async () => {
    const user = userEvent.setup();
    const open = Promise.withResolvers<IssueSummary[]>();
    await openStatusFilter(user, open.promise);
    if (way === "a pointer's pick") {
      await user.click(screen.getByRole("option", { name: /^Done/ }));
    } else {
      await user.paste("Done");
      await user.keyboard("{Enter}");
    }
    await screen.findByRole("status", { name: "Loading issues" });
    expect(screen.queryByRole("table", { name: "Project issues table" })).toBeNull();
    const clear = screen.getByRole("button", { name: "Clear filters" });
    await waitFor(() => expect(clear).toHaveFocus());
    expect(screen.getByRole("link", { name: "Inbox" })).not.toHaveFocus();
    open.resolve([issue("CORE-1")]);
    await screen.findByRole("row", { name: "CORE-1 Work CORE-1" });
    expect(clear).toHaveFocus();
  });
}

// A notice's button that takes the notice away, by Enter, hands its focus on by the List's own
// hand-off: the notices hold no other control, so it walks back over the main content to the
// toolbar's nearest control before them that takes no typed text, the View menu, never into the
// search box.
for (const press of ["Finish", "Dismiss"]) {
  test(`the List: the interrupted action's ${press} hands its focus to the toolbar's View menu`, async () => {
    const user = userEvent.setup();
    const recovery = await interruptedList();
    act(() => within(recovery).getByRole("button", { name: press }).focus());
    await user.keyboard("{Enter}");
    await waitFor(() => expect(recovery.isConnected).toBe(false));
    await waitFor(() => expect(screen.getByRole("button", { name: /^View/ })).toHaveFocus());
    await noticeSettled(recovery);
  });
}

for (const press of ["Create anyway", "Cancel new epic"]) {
  test(`the List: the possible duplicate's ${press} hands its focus to the toolbar's View menu`, async () => {
    const user = userEvent.setup();
    const duplicate = await duplicateEpicList(user);
    act(() => within(duplicate).getByRole("button", { name: press }).focus());
    await user.keyboard("{Enter}");
    expect(screen.queryByRole("button", { name: press })).toBeNull();
    await waitFor(() => expect(screen.getByRole("button", { name: /^View/ })).toHaveFocus());
    await noticeSettled(duplicate);
  });
}
