import {
  afterEach,
  beforeAll,
  expect,
  jest,
  type Mock,
  setSystemTime,
  spyOn,
  test,
} from "bun:test";
import * as dndCore from "@dnd-kit/core";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import {
  act,
  // biome-ignore lint/style/noRestrictedImports: this file keeps fireEvent for the mouse and pointer presses that drive dnd-kit, the key inside one of those drags, and the clicks whose boolean return it reads
  fireEvent,
  render,
  renderHook,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { MemoryRouter, useLocation } from "react-router-dom";

import { installCountingStorage } from "../../__tests__/counting-storage";
import { realClock, settle } from "../../__tests__/fake-clock";
import { afterFrameAndTasks } from "../../__tests__/frames";
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { pickNamed } from "../../__tests__/list-picks";
import { answerBoard, liveSockets, waitMs } from "../../__tests__/live-socket-harness";
import { answerIssuePatches } from "../../__tests__/send-answers";
import { stopTheLateWrite } from "../../__tests__/stop-late-write";
import { ApiError, api, NetworkError } from "../../api/client";
import { boardMoveMutationKey } from "../../api/queries";
import type {
  Issue,
  IssueClaim,
  IssueSummary,
  LegionBoardCard,
  LegionBoardColumn,
  LegionBoardFacts,
  LegionBoardResponse,
  LegionSetting,
  StatusChangeAnswer,
  UpdateIssueInput,
} from "../../api/types";
import {
  answerStatusChanges,
  fixtureAnswer,
  fixtureRefusal,
} from "../issue/parent-status/status-change-fixtures";
import * as routeReach from "../issue/RouteReach";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { KeymapProvider } from "../shell/KeymapProvider";
import { useKeymapScope } from "../shell/keymap";
import { useDialog } from "../shell/useDialog";
import { userPreferenceStorageKey } from "../shell/userPreference";
import { stoppedWriteNote } from "../writes/keyed-write";
import { DAEMON_SETS_PHASE } from "./board-model";
import { useBoardMoves } from "./board-moves";
import { IssuesTable } from "./IssuesTable";
import { LEGION_PHASES, type LegionPhase } from "./issues-legion";
import { MEASURE_METHODS } from "./issues-table-model";

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

/** What a PATCH answers with: the issue `key` as the server saved it. */
function saved(key: string, overrides: Partial<Issue> = {}): Issue {
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
    ...overrides,
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

/** Puts the focus on `card`'s own title link and returns it. */
function focusTitleLink(card: Element, key: string): HTMLElement {
  const link = card.querySelector<HTMLElement>("[data-issue-link]");
  if (link === null) throw new Error(`${key}'s card draws no title link`);
  act(() => link.focus());
  return link;
}

const restores: (() => void)[] = [];
afterEach(() => {
  for (const restore of restores.splice(0)) restore();
  localStorage.clear();
});

/** A project's daemon setting that names a daemon. */
const DAEMON: LegionSetting = {
  created_by: { kind: "user", id: "alice" },
  project: "CORE",
  state_url: "http://legion.test",
  updated_at: "2026-11-01T00:00:00Z",
};

/**
 * Draws the Issues tab of CORE at `/projects/CORE/issues<search>` over `issues`, as the listing
 * route answers them: `open` leaves the closed rows out and `status` keeps the rows in those
 * statuses. `daemon` is the project's Legion daemon setting, none by default. `inMain` draws the
 * tab inside the shell's main content, as `app.tsx` does.
 */
function board(
  issues: IssueSummary[],
  search: string,
  daemon: LegionSetting | null = null,
  inMain = false
) {
  const listing = spyOn(api, "listIssues").mockImplementation(async (options = {}) =>
    issues.filter(
      (row) =>
        (!options.open || row.status !== "done") &&
        (options.status === undefined || options.status.includes(row.status))
    )
  );
  const page = spyOn(api, "listIssuePage").mockImplementation(async (options) => {
    const rows = issues.filter(
      (row) =>
        (!options.open || row.status !== "done") &&
        (options.status === undefined || options.status.includes(row.status))
    );
    return {
      issues: rows.slice(0, options.limit),
      limit: options.limit ?? rows.length,
      offset: 0,
      total: rows.length,
    };
  });
  const setting = spyOn(api, "getLegionDaemon").mockResolvedValue(daemon);
  const state = spyOn(api, "getMyState").mockResolvedValue({});
  const agents = spyOn(api, "listAgents").mockResolvedValue([]);
  const viewer = spyOn(api, "whoAmI").mockResolvedValue({
    event_head: "0",
    login: "alice",
    kind: "user",
  });
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(["whoami"], { event_head: "0", login: "alice", kind: "user" });
  const view = render(
    <MemoryRouter initialEntries={[`/projects/CORE/issues${search}`]}>
      <QueryClientProvider client={client}>
        <KeymapProvider>
          <Route />
          {inMain ? (
            <main>
              <IssuesTable project="CORE" />
            </main>
          ) : (
            <IssuesTable project="CORE" />
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
    agents.mockRestore();
    viewer.mockRestore();
  });
  return { client, listing };
}

/** The address the page is at now. */
const routeSearch = () => new URLSearchParams(screen.getByTestId("route-search").textContent ?? "");

/** The Board, once it is drawn. */
const boardRegion = () => screen.findByRole("region", { name: "Project board" });

/** The ids of the columns and rails the Board draws, in its order. */
function columnIds(region: HTMLElement): (string | null)[] {
  return [...region.querySelectorAll("[data-board-column]")].map((column) =>
    column.getAttribute("data-board-column")
  );
}

/** The keys of the cards a column or a region draws, in their order. */
function cardKeys(region: HTMLElement): (string | null)[] {
  return [...region.querySelectorAll("[data-board-card]")].map((card) =>
    card.getAttribute("data-board-card")
  );
}

const liveRegion = () => screen.getByRole("status", { name: "Board announcements" });

/** A PATCH that never answers: the Board stays as the move placed it. */
function patchInFlight() {
  const patch = answerIssuePatches().mockImplementation(
    () => Promise.withResolvers<Issue>().promise
  );
  restores.push(() => patch.mockRestore());
  return patch;
}

/** A card of the Legion board, with no fact but its key. */
function legionCard(key: string, overrides: Partial<LegionBoardCard> = {}): LegionBoardCard {
  return {
    blocked_by: null,
    children: 0,
    key,
    phase: "implementing",
    phase_since: null,
    priority: null,
    roles: null,
    slot: null,
    slot_since: null,
    slot_use: null,
    title: `Work ${key}`,
    tree: key,
    tree_slot: null,
    waiting: null,
    waiting_on: null,
    waiting_place: null,
    ...overrides,
  };
}

/** The facts the board's map holds for an issue with `workHours` of work. */
function boardFacts(workHours: number): LegionBoardFacts {
  return {
    as_of: "2026-11-17T08:00:00Z",
    closed_at: null,
    growth: { hours_by_phase: {}, slot_hours: 0, work_hours: 0, work_last_at: 0 },
    hours_by_phase: { implementing: workHours },
    opened_at: "2026-11-01T00:00:00Z",
    review_rounds: 0,
    root_key: "CORE-1",
    slot_hours: workHours,
    tree_depth: 0,
    updated_at: "2026-11-17T08:00:00Z",
    waiting_reason: "",
    work_first_at: "2026-11-02T00:00:00Z",
    work_hours: workHours,
    work_last_at: "2026-11-10T00:00:00Z",
  };
}

/** The board route's answer: its twelve columns with `cards` in them, and the facts its map
 *  holds. */
function legionBoard(
  cards: Partial<Record<LegionPhase, LegionBoardCard[]>>,
  issues: Record<string, LegionBoardFacts> = {}
): LegionBoardResponse {
  return {
    columns: LEGION_PHASES.map((phase) => ({
      id: phase.id,
      label: phase.title,
      cards: cards[phase.id] ?? [],
    })),
    computed_at: "2026-11-17T08:00:00Z",
    header: {
      facts_cursor_age_seconds: null,
      facts_updated_at: null,
      queue_length: (cards.queue ?? []).length,
      read_at: "2026-11-17T08:00:00Z",
      read_error: null,
      read_ok: true,
      slot_cap: 4,
      slots_in_use: 1,
      trees: 1,
    },
    issues,
    project: "CORE",
    settings: null,
  };
}

/** Every listing read the page made: none of them is the full listing, which names neither
 *  `open` nor `status`. */
function expectNoFullListing(listing: Mock<typeof api.listIssues>) {
  expect(listing).toHaveBeenCalled();
  for (const [options] of listing.mock.calls) {
    expect(options?.project).toBe("CORE");
    expect(options?.open === true || options?.status !== undefined).toBe(true);
    expect(options?.label).toBeUndefined();
  }
}

/** A row of each status, Done among them. */
const STATUS_ROWS = () => [
  issue("CORE-1", { status: "triage" }),
  issue("CORE-2", { status: "icebox" }),
  issue("CORE-3", { status: "todo" }),
  issue("CORE-4", { status: "in_progress" }),
  issue("CORE-5", { status: "done" }),
];

// By Status, Open picks the eight open statuses: a column for each, an empty one too, with the
// Icebox rail and its count after Triage, and the Done rail, with no count, since Open lists no
// done issue and so reads none.
test("Open's Board draws the open statuses, the Icebox rail with its count and the Done rail", async () => {
  const user = userEvent.setup();
  const { listing } = board(STATUS_ROWS(), "?layout=board");
  const region = await boardRegion();
  await waitFor(() => expect(cardKeys(region)).toEqual(["CORE-1", "CORE-3", "CORE-4"]));
  expect(columnIds(region)).toEqual([
    "triage",
    "icebox",
    "backlog",
    "todo",
    "in_progress",
    "testing",
    "needs_review",
    "retro",
    "done",
  ]);
  const icebox = within(region).getByRole("region", { name: "Icebox (collapsed)" });
  expect(icebox.textContent).toBe("1Icebox");
  expect(within(region).getByRole("region", { name: "Done (collapsed)" }).textContent).toBe("Done");
  expect(
    within(within(region).getByRole("region", { name: "Backlog" })).getByText("Nothing here")
  ).toBeDefined();
  expect(region.querySelector("[data-testid=board-column-header]")?.textContent).toBe("Triage1");
  expectNoFullListing(listing);
  expect(listing.mock.calls.every(([options]) => options?.open === true)).toBe(true);

  // Show Icebox turns the rail into the Icebox column, with its cards.
  await user.click(screen.getByRole("button", { name: "Show Icebox" }));
  expect(cardKeys(within(region).getByRole("region", { name: "Icebox" }))).toEqual(["CORE-2"]);
  expect(within(region).queryByRole("region", { name: "Icebox (collapsed)" })).toBeNull();
});

test.each([
  // A Status filter's picks are the columns, with the Done rail.
  ["status=triage", "?layout=board&status=triage", ["triage", "done"], ["CORE-1"]],
  // No Status filter: each status a row is in, and the Done column with the done cards, since
  // the view lists done issues.
  [
    "nofilter=1",
    "?layout=board&nofilter=1",
    ["triage", "icebox", "todo", "in_progress", "done"],
    ["CORE-1", "CORE-3", "CORE-4", "CORE-5"],
  ],
  // Done: its done cards in the Done column.
  ["view=done", "?view=done&layout=board", ["done"], ["CORE-5"]],
])("by Status with %s the Board draws its columns and cards", async (_name, search, columns, cards) => {
  const { listing } = board(STATUS_ROWS(), search);
  const region = await boardRegion();
  await waitFor(() => expect(cardKeys(region)).toEqual(cards));
  expect(columnIds(region)).toEqual(columns);
  expectNoFullListing(listing);
  if (columns.includes("done") && cards.includes("CORE-5"))
    expect(cardKeys(within(region).getByRole("region", { name: "Done" }))).toEqual(["CORE-5"]);
});

/** Open's rows on a project with a daemon: an Implementing card, a Queue card, and two rows with
 *  no card. */
const PHASE_ROWS = () => [
  issue("CORE-1", { status: "in_progress" }),
  issue("CORE-2", { status: "todo" }),
  issue("CORE-3", { status: "todo" }),
  issue("CORE-4", { status: "backlog" }),
];
const PHASE_BOARD = () =>
  legionBoard({
    implementing: [legionCard("CORE-1")],
    queue: [legionCard("CORE-2", { phase: "admitted", waiting_place: 1 })],
  });

// By Legion phase: one column for each of the board's twelve columns in its order, empty ones
// included, then the None rail with the count of the rows with no card; no Show Icebox and no
// Done rail.
test("by Legion phase the Board draws the twelve phases and the None rail, and no Show Icebox", async () => {
  const sockets = liveSockets(restores);
  const { listing } = board(PHASE_ROWS(), "?layout=board&group=legion_phase", DAEMON);
  // The view's columns read the board, so it waits for it.
  expect(await screen.findByRole("status", { name: "Loading issues" })).toBeDefined();
  await answerBoard(sockets, PHASE_BOARD());
  const region = await boardRegion();
  expect(columnIds(region)).toEqual([...LEGION_PHASES.map((phase) => phase.id), "none"]);
  expect(
    [...region.querySelectorAll("[data-testid=board-column-header]")].map(
      (header) => header.firstElementChild?.firstElementChild?.textContent
    )
  ).toEqual(LEGION_PHASES.map((phase) => phase.title));
  expect(cardKeys(within(region).getByRole("region", { name: "Queue" }))).toEqual(["CORE-2"]);
  expect(cardKeys(within(region).getByRole("region", { name: "Implementing" }))).toEqual([
    "CORE-1",
  ]);
  expect(within(region).getByRole("region", { name: "None (collapsed)" }).textContent).toBe(
    "2None"
  );
  expect(within(region).queryByRole("region", { name: "Done (collapsed)" })).toBeNull();
  expect(screen.queryByRole("button", { name: "Show Icebox" })).toBeNull();
  expect(screen.getByRole("button", { name: "Lanes" })).toBeDefined();
  expectNoFullListing(listing);
});

// A column a newer server adds after Held is a Board column under the route's label, holding its
// card: the Board draws the board route's columns, so no card of a column it does not know is
// dropped.
test("by Legion phase a column the board route adds after Held is a Board column under its label, holding its card", async () => {
  const sockets = liveSockets(restores);
  board(PHASE_ROWS(), "?layout=board&group=legion_phase", DAEMON);
  const routed = PHASE_BOARD();
  await answerBoard(sockets, {
    ...routed,
    columns: [
      ...(routed.columns ?? []),
      {
        // The generated column ids name today's twelve: a newer server's column is none of them.
        id: "x_phase" as string as LegionBoardColumn["id"],
        label: "X phase",
        cards: [legionCard("CORE-3", { phase: "x_phase" })],
      },
    ],
  });
  const region = await boardRegion();
  expect(columnIds(region)).toEqual([...LEGION_PHASES.map((phase) => phase.id), "x_phase", "none"]);
  const added = within(region).getByRole("region", { name: "X phase" });
  expect(cardKeys(added)).toEqual(["CORE-3"]);
  expect(added.querySelector("[data-testid=board-column-header]")?.textContent).toBe("X phase1");
  expect(within(region).getByRole("region", { name: "None (collapsed)" }).textContent).toBe(
    "1None"
  );
});

test.each([
  ["implementing", ["implementing"], ["CORE-1"]],
  ["none", ["none"], []],
])("a Legion phase filter of %s keeps only its columns", async (pick, columns, cards) => {
  const sockets = liveSockets(restores);
  board(PHASE_ROWS(), `?layout=board&group=legion_phase&f.legion_phase=${pick}`, DAEMON);
  await answerBoard(sockets, PHASE_BOARD());
  const region = await boardRegion();
  expect(columnIds(region)).toEqual(columns);
  expect(cardKeys(region)).toEqual(cards);
});

// Group by on the Board: Status and Legion phase where the project has a daemon, kept in the
// address as `group=legion_phase`, and no menu with no daemon.
test("the Board's Group by picks Status or Legion phase, and a project with no daemon draws none", async () => {
  const user = userEvent.setup();
  const sockets = liveSockets(restores);
  board(PHASE_ROWS(), "?layout=board", DAEMON);
  await answerBoard(sockets, PHASE_BOARD());
  await boardRegion();
  await user.click(screen.getByRole("button", { name: "Group by" }));
  expect(screen.getAllByRole("radio")).toHaveLength(2);
  expect(screen.getByRole("radio", { name: "Legion phase" })).not.toBeChecked();
  expect(screen.getByRole("radio", { name: "Status" })).toBeChecked();
  await user.click(screen.getByRole("radio", { name: "Legion phase" }));
  await waitFor(() => expect(routeSearch().get("group")).toBe("legion_phase"));
  await waitFor(async () => expect(columnIds(await boardRegion())).toContain("none"));
  await user.click(screen.getByRole("radio", { name: "Status" }));
  await waitFor(() => expect(routeSearch().has("group")).toBe(false));
  await waitFor(async () => expect(columnIds(await boardRegion())).toContain("done"));
});

test("a Board with no Legion daemon draws no Group by menu", async () => {
  board(STATUS_ROWS(), "?layout=board");
  await boardRegion();
  expect(screen.queryByRole("button", { name: "Group by" })).toBeNull();
});

const CLAIM: IssueClaim = {
  actor: { kind: "session", id: "session-one", origin: { session_title: "Implementer" } },
  at: "2026-09-24T06:00:00Z",
};

// A card shows today's card and, for an issue with a card on the Legion board, its Legion fields
// from the board route, each left out where the board gives it no value.
test("a card draws its Legion fields and leaves out every one the board gives no value", async () => {
  const user = userEvent.setup();
  const sockets = liveSockets(restores);
  const since = new Date(Date.now() - (2 * 60 + 5) * 60_000).toISOString();
  board(
    [
      issue("CORE-1", { status: "in_progress", claim: CLAIM, open_asks: 2 }),
      issue("CORE-2", { status: "todo", open_asks: 1 }),
      issue("CORE-3", { status: "todo" }),
    ],
    "?layout=board",
    DAEMON
  );
  await answerBoard(
    sockets,
    legionBoard(
      {
        implementing: [
          legionCard("CORE-1", {
            phase_since: since,
            roles: ["implementer", "tester"],
            slot: 2,
            waiting: { reason: "blocked", since, blocked_by: ["CORE-9"] },
            waiting_on: {
              kind: "ask",
              since: "2026-11-16T10:00:00Z",
              answered_at: "2026-11-16T11:00:00Z",
            },
            children: 1,
          }),
        ],
        queue: [legionCard("CORE-2", { phase: "admitted", slot: 1, waiting_place: 2 })],
      },
      { "CORE-1": boardFacts(3) }
    )
  );
  const region = await boardRegion();
  const first = await waitFor(() => {
    const card = within(region).getByRole("article", { name: "CORE-1 Work CORE-1" });
    expect(card.querySelector("[data-legion-fact=phase-time]")).not.toBeNull();
    return card;
  });
  const fact = (card: HTMLElement, hook: string) =>
    card.querySelector<HTMLElement>(`[data-legion-fact=${hook}]`);
  expect(fact(first, "phase-time")?.textContent).toBe("Time in Legion phase2 h 5 m");
  expect(fact(first, "work-time")?.textContent).toBe("Work time3 h 0 m");
  expect(fact(first, "roles")?.textContent).toBe("WorkersImplementer, Tester");
  // `blocked_by` names no blocker, so the wait's blockers do.
  expect(fact(first, "blocked-by")?.textContent).toBe("Blocked by CORE-9");
  expect(fact(first, "waiting-on")?.textContent).toBe(
    `Waiting on ask since ${new Date("2026-11-16T10:00:00Z").toLocaleString()}, answered`
  );
  expect(fact(first, "children")?.textContent).toBe("1 issue of this tree not started");
  // The claim chip writes the holder's whole name over its age and state.
  expect(first.querySelector("[data-claim-stacked]")).not.toBeNull();
  // No slot number, and no word "slot"; and no Place, which the board gives this card none of.
  expect(first.textContent).not.toMatch(/slot|#/i);
  expect(first.querySelector("[data-queue-place]")).toBeNull();

  // Each measure opens its column's method in the shared tooltip, from the keyboard and the
  // pointer, and carries no native title.
  const phaseTime = fact(first, "phase-time") as HTMLElement;
  expect(phaseTime).toHaveAttribute("tabindex", "0");
  expect(phaseTime.hasAttribute("title")).toBe(false);
  act(() => phaseTime.focus());
  expect((await screen.findByRole("tooltip")).textContent).toBe(
    MEASURE_METHODS.time_in_phase ?? "a method for Time in Legion phase"
  );
  act(() => phaseTime.blur());
  await user.hover(fact(first, "work-time") as HTMLElement);
  expect((await screen.findByRole("tooltip")).textContent).toBe(
    MEASURE_METHODS.work_hours ?? "a method for Work time"
  );
  await user.unhover(fact(first, "work-time") as HTMLElement);

  // The open asks badge says what it counts in the tooltip, is a tab stop, and has no title.
  const asks = first.querySelector<HTMLElement>("[data-open-asks]") as HTMLElement;
  expect(asks).toHaveAttribute("tabindex", "0");
  expect(asks.hasAttribute("title")).toBe(false);
  expect(asks.textContent).toBe("2 open asks");
  act(() => asks.focus());
  expect((await screen.findByRole("tooltip")).textContent).toBe("2 open asks");
  act(() => asks.blur());
  const second = within(region).getByRole("article", { name: "CORE-2 Work CORE-2" });
  expect(second.querySelector("[data-open-asks]")?.textContent).toBe("1 open ask");

  // A card with a card on the board and no value for any fact draws none: no Workers with no
  // worker, no time with no phase entry, no Work time with no facts, and never a dash.
  expect(second.querySelector("[data-legion-fact]")).toBeNull();
  expect(second.textContent).not.toContain("—");
  // A Queue card draws its Place in the Legion queue beside its key.
  expect(second.querySelector("[data-queue-place]")?.textContent).toBe("#2");
  expect(second.querySelector("[data-issue-link]")?.firstElementChild?.textContent).toBe(
    "CORE-2#2"
  );
  // A row with no card draws today's card alone, with no Place.
  const third = within(region).getByRole("article", { name: "CORE-3 Work CORE-3" });
  expect(third.querySelector("[data-legion-fact]")).toBeNull();
  expect(third.querySelector("[data-queue-place]")).toBeNull();
  expect(third.textContent).not.toContain("#");
});

// Todo: Alpha, Bravo, Charlie; In progress: Delta - the keyboard rows' seed. No row is in the
// Legion queue, so Open's sort, Queue, orders them by rank.
const keyboardIssues = () => [
  issue("CORE-1", {
    rank: "a",
    status: "todo",
    title: "Alpha",
    created_at: "2026-09-01T00:00:00Z",
  }),
  issue("CORE-2", {
    rank: "b",
    status: "todo",
    title: "Bravo",
    created_at: "2026-09-03T00:00:00Z",
  }),
  issue("CORE-3", {
    rank: "c",
    status: "todo",
    title: "Charlie",
    created_at: "2026-09-02T00:00:00Z",
  }),
  issue("CORE-4", { rank: "d", status: "in_progress", title: "Delta" }),
];

test("Shift+J under the Place sort writes the rank, reads the card's place out and keeps focus; Shift+K at the top does nothing", async () => {
  const user = userEvent.setup();
  const patch = patchInFlight();
  board(keyboardIssues(), "?layout=board");
  const region = await boardRegion();
  const alpha = await within(region).findByRole("article", { name: "CORE-1 Alpha" });
  expect(liveRegion()).toHaveClass("sr-only");
  act(() => alpha.focus());

  await user.keyboard("{Shift>}K{/Shift}");
  expect(patch).not.toHaveBeenCalled();
  expect(liveRegion().textContent).toBe("");

  await user.keyboard("{Shift>}J{/Shift}");
  expect(patch).toHaveBeenCalledWith("CORE-1", { rank: { before: "CORE-3" } }, keyedAttempt);
  expect(liveRegion().textContent).toBe("CORE-1 → Todo, position 2 of 3");
  const todo = within(region).getByRole("region", { name: "Todo" });
  await waitFor(() => expect(cardKeys(todo)).toEqual(["CORE-2", "CORE-1", "CORE-3"]));
  expect(within(region).getByRole("article", { name: "CORE-1 Alpha" })).toHaveFocus();
});

test("Shift+L sets the next status, to the top where the rank rule lets it, and focus follows the card", async () => {
  const user = userEvent.setup();
  const patch = patchInFlight();
  board(keyboardIssues(), "?layout=board");
  const region = await boardRegion();
  act(() => within(region).getByRole("article", { name: "CORE-2 Bravo" }).focus());
  await user.keyboard("{Shift>}L{/Shift}");
  expect(patch).toHaveBeenCalledWith(
    "CORE-2",
    { status: "in_progress", rank: { before: "CORE-4" } },
    keyedAttempt
  );
  expect(liveRegion().textContent).toBe("CORE-2 → In progress, position 1 of 2");
  const inProgress = within(region).getByRole("region", { name: "In progress" });
  await waitFor(() => expect(cardKeys(inProgress)).toEqual(["CORE-2", "CORE-4"]));
  await waitFor(() =>
    expect(within(inProgress).getByRole("article", { name: "CORE-2 Bravo" })).toHaveFocus()
  );
});

// Shift+L through every column that takes a drop, to the Done rail, which closes the issue: the
// card leaves Open, and the focus lands on the rail. At the Board's other edge, Triage, a card
// moved toward that edge has no column to go to and writes nothing. That press comes first: the
// Board sends its moves one at a time, so one pressed while the PATCH that never answers is out
// would not be sent, edge or not.
test("Shift+L walks a card to the Done rail, which closes it", async () => {
  const user = userEvent.setup();
  const patch = patchInFlight();
  board(
    [
      issue("CORE-1", { status: "retro", title: "Last" }),
      issue("CORE-2", { status: "triage", title: "First" }),
    ],
    "?layout=board"
  );
  const region = await boardRegion();
  // Triage is the Board's first column: Shift+H on its card moves it nowhere.
  act(() => within(region).getByRole("article", { name: "CORE-2 First" }).focus());
  await user.keyboard("{Shift>}H{/Shift}");
  expect(patch).not.toHaveBeenCalled();
  act(() => within(region).getByRole("article", { name: "CORE-1 Last" }).focus());
  await user.keyboard("{Shift>}L{/Shift}");
  expect(patch).toHaveBeenCalledWith("CORE-1", { status: "done" }, keyedAttempt);
  expect(liveRegion().textContent).toBe("CORE-1 → Done, closed");
  await waitFor(() => expect(cardKeys(region)).toEqual(["CORE-2"]));
  await waitFor(() =>
    expect(within(region).getByRole("region", { name: "Done (collapsed)" })).toHaveFocus()
  );
});

// A sorted Board takes no move within a column, at its end too, since the sort would put the
// card back; a move into another column sets the status and writes no rank, the card landing
// where the sort puts it.
test("under another sort a move within a column is refused with the sort named, and a status move writes no rank", async () => {
  const user = userEvent.setup();
  const patch = patchInFlight();
  board(keyboardIssues(), "?layout=board&sort=-created");
  const region = await boardRegion();
  const todo = within(region).getByRole("region", { name: "Todo" });
  await waitFor(() => expect(cardKeys(todo)).toEqual(["CORE-2", "CORE-3", "CORE-1"]));
  const refusal = "The view sorts by Created. Sort by Queue to move cards in a column.";
  act(() => within(region).getByRole("article", { name: "CORE-1 Alpha" }).focus());
  await user.keyboard("{Shift>}J{/Shift}");
  expect(liveRegion().textContent).toBe(refusal);
  expect(screen.getByTestId("board-refusal").textContent).toBe(refusal);
  await user.keyboard("{Shift>}K{/Shift}");
  expect(patch).not.toHaveBeenCalled();

  await user.keyboard("{Shift>}L{/Shift}");
  expect(patch).toHaveBeenCalledWith("CORE-1", { status: "in_progress" }, keyedAttempt);
  expect(screen.queryByTestId("board-refusal")).toBeNull();
  const inProgress = within(region).getByRole("region", { name: "In progress" });
  // Where the sort put it: newest first, after CORE-4, which was created after it.
  await waitFor(() => expect(cardKeys(inProgress)).toEqual(["CORE-4", "CORE-1"]));
  await waitFor(() =>
    expect(liveRegion().textContent).toBe("CORE-1 → In progress, position 2 of 2")
  );
});

// A live region reads out a change of its content. The same refusal twice in a row is a new
// node each time, so a screen reader reads the second press's refusal too.
test("the same refusal twice is two writes of the live region", async () => {
  const user = userEvent.setup();
  const patch = patchInFlight();
  board(keyboardIssues(), "?layout=board&sort=-created");
  const region = await boardRegion();
  const refusal = "The view sorts by Created. Sort by Queue to move cards in a column.";
  act(() => within(region).getByRole("article", { name: "CORE-1 Alpha" }).focus());
  await user.keyboard("{Shift>}J{/Shift}");
  expect(liveRegion().textContent).toBe(refusal);
  const first = liveRegion().firstChild;
  await user.keyboard("{Shift>}J{/Shift}");
  expect(liveRegion().textContent).toBe(refusal);
  expect(liveRegion().firstChild).not.toBe(first);
  expect(patch).not.toHaveBeenCalled();
});

// Under the Place sort a card with a place in the Legion queue (here Start next's, the Todo
// issues labelled `legion`, with no daemon) sorts before every card with none, so the rank
// decides a move only among cards that all have a place or all have none.
test("a move between cards with a place and cards with none is refused with why", async () => {
  const user = userEvent.setup();
  const patch = patchInFlight();
  board(
    [
      issue("CORE-1", { rank: "a", labels: ["legion"], title: "First" }),
      issue("CORE-2", { rank: "b", labels: ["legion"], title: "Second" }),
      issue("CORE-3", { rank: "c", title: "Unplaced" }),
    ],
    "?layout=board"
  );
  const region = await boardRegion();
  act(() => within(region).getByRole("article", { name: "CORE-3 Unplaced" }).focus());
  await user.keyboard("{Shift>}K{/Shift}");
  expect(liveRegion().textContent).toBe(
    "A card with no place in the Legion queue stays after the cards with a place."
  );
  act(() => within(region).getByRole("article", { name: "CORE-2 Second" }).focus());
  await user.keyboard("{Shift>}J{/Shift}");
  expect(liveRegion().textContent).toBe(
    "A card with a place in the Legion queue stays before the cards with no place."
  );
  expect(patch).not.toHaveBeenCalled();
  await user.keyboard("{Shift>}K{/Shift}");
  expect(patch).toHaveBeenCalledWith("CORE-2", { rank: { before: "CORE-1" } }, keyedAttempt);
  expect(liveRegion().textContent).toBe("CORE-2 → Todo, position 1 of 3");
});

// By Legion phase the daemon sets each card's column: a move within Queue writes the rank, and
// a Queue card takes its new place on the daemon's next read; every other key move is refused,
// at the column's end too.
test("by Legion phase only a move within Queue is taken, and every other key move is refused", async () => {
  const user = userEvent.setup();
  const patch = patchInFlight();
  const sockets = liveSockets(restores);
  board(
    [
      issue("CORE-1", { status: "in_progress", title: "Building" }),
      issue("CORE-2", { status: "in_progress", title: "Testing" }),
      issue("CORE-3", { status: "todo", title: "Next" }),
      issue("CORE-4", { status: "todo", title: "Later" }),
    ],
    "?layout=board&group=legion_phase",
    DAEMON
  );
  await answerBoard(
    sockets,
    legionBoard({
      implementing: [legionCard("CORE-1"), legionCard("CORE-2")],
      queue: [
        legionCard("CORE-3", { phase: "admitted", waiting_place: 1 }),
        legionCard("CORE-4", { phase: "admitted", waiting_place: 2 }),
      ],
    })
  );
  const region = await boardRegion();
  act(() => within(region).getByRole("article", { name: "CORE-2 Testing" }).focus());
  await user.keyboard("{Shift>}J{/Shift}");
  expect(liveRegion().textContent).toBe(DAEMON_SETS_PHASE);
  await user.keyboard("{Shift>}K{/Shift}");
  await user.keyboard("{Shift>}L{/Shift}");
  await user.keyboard("{Shift>}H{/Shift}");
  expect(screen.getByTestId("board-refusal").textContent).toBe(DAEMON_SETS_PHASE);
  expect(patch).not.toHaveBeenCalled();

  act(() => within(region).getByRole("article", { name: "CORE-3 Next" }).focus());
  await user.keyboard("{Shift>}J{/Shift}");
  expect(patch).toHaveBeenCalledWith("CORE-3", { rank: { after: "CORE-4" } }, keyedAttempt);
  // The place the card takes once the daemon reads the rank.
  expect(liveRegion().textContent).toBe("CORE-3 → Queue, position 2 of 2");
  await user.keyboard("{Shift>}L{/Shift}");
  expect(liveRegion().textContent).toBe(DAEMON_SETS_PHASE);
  expect(patch).toHaveBeenCalledTimes(1);
});

// A Board card is a row of its column's list. A refetch that takes the card holding the focus off
// the Board - another reader moved its issue on - is a loss the reader did not cause, so with the
// Board in the page's main content, as the shell draws it, the focus goes into no other card: the
// reader's next key acts on no card they did not pick.
test("a card a refetch takes away from under the focus hands it to no other card", async () => {
  const rows = keyboardIssues();
  const { client } = board(rows, "?layout=board", null, true);
  const region = await boardRegion();
  const bravo = await within(region).findByRole("article", { name: "CORE-2 Bravo" });
  focusTitleLink(bravo, "CORE-2");

  rows.splice(1, 1);
  await act(() => client.invalidateQueries({ queryKey: ["issues"] }));
  await waitFor(() => expect(bravo).not.toBeInTheDocument());
  await afterFrameAndTasks();

  expect(document.activeElement?.closest("[data-board-card]") ?? null).toBeNull();
  expect(within(region).getByRole("article", { name: "CORE-3 Charlie" })).toBeInTheDocument();
});

test("j/k/h/l rove real focus over cards, columns and rails", async () => {
  const user = userEvent.setup();
  board(keyboardIssues(), "?layout=board");
  const region = await boardRegion();
  const alpha = await within(region).findByRole("article", { name: "CORE-1 Alpha" });
  const bravo = within(region).getByRole("article", { name: "CORE-2 Bravo" });
  const delta = within(region).getByRole("article", { name: "CORE-4 Delta" });
  await user.keyboard("j");
  expect(alpha).toHaveFocus();
  await user.keyboard("j");
  expect(bravo).toHaveFocus();
  await user.keyboard("{ArrowUp}");
  expect(alpha).toHaveFocus();
  await user.keyboard("l");
  expect(delta).toHaveFocus();
  await user.keyboard("l");
  expect(within(region).getByRole("region", { name: "Testing" })).toHaveFocus();
  await user.keyboard("h");
  expect(delta).toHaveFocus();
  for (const _ of ["testing", "needs_review", "retro", "done"]) await user.keyboard("l");
  const doneRail = within(region).getByRole("region", { name: "Done (collapsed)" });
  expect(doneRail).toHaveFocus();
  await user.keyboard("l");
  expect(doneRail).toHaveFocus();
  await user.keyboard("{Escape}");
  expect(document.body).toHaveFocus();
  await user.keyboard("k");
  expect(delta).toHaveFocus();
  // Back to Todo, and h onto the Icebox rail.
  await user.keyboard("h");
  await user.keyboard("h");
  await user.keyboard("h");
  expect(within(region).getByRole("region", { name: "Icebox (collapsed)" })).toHaveFocus();
});

test("Enter and o open the focused card once, p focuses its priority list's trigger, Escape returns to the card and then out", async () => {
  const user = userEvent.setup();
  board(keyboardIssues(), "?layout=board");
  const region = await boardRegion();
  const alpha = await within(region).findByRole("article", { name: "CORE-1 Alpha" });
  const link = within(alpha).getByRole("link", { name: /CORE-1/ });
  let clicks = 0;
  link.addEventListener("click", () => {
    clicks += 1;
  });
  act(() => alpha.focus());
  await user.keyboard("p");
  const priority = within(alpha).getByRole("button", { name: pickNamed("Priority of CORE-1") });
  expect(priority).toHaveFocus();
  // Single letters never fire on an editable target: on the closed trigger a letter is the list's,
  // which opens with it in its text box, and the Board's `j` moves to no other card.
  await user.keyboard("j");
  const search = screen.getByRole("combobox", { name: "Search priorities" });
  expect(search).toHaveFocus();
  expect(search).toHaveValue("j");
  await user.keyboard("{Escape}");
  expect(priority).toHaveFocus();
  await user.keyboard("{Escape}");
  expect(alpha).toHaveFocus();
  await user.keyboard("{Escape}");
  expect(document.body).toHaveFocus();
  act(() => alpha.focus());
  await user.keyboard("{Enter}");
  expect(clicks).toBe(1);
  expect(screen.getByTestId("route").textContent).toBe("/issues/CORE-1");
});

/** The keys of the cards dnd-kit holds lifted. A lifted card follows the pointer by the
 *  `transform` dnd-kit hands it, which the card draws as its style only while it is dragged. */
const liftedCards = (region: HTMLElement) =>
  [...region.querySelectorAll<HTMLElement>("[data-board-card]")]
    .filter((card) => card.style.transform !== "")
    .map((card) => card.dataset.boardCard);

test("Space on a focused card lifts nothing, and the card's instructions name the registry keys, not dnd-kit's grammar", async () => {
  const user = userEvent.setup();
  const patch = patchInFlight();
  board(keyboardIssues(), "?layout=board");
  const region = await boardRegion();
  const alpha = await within(region).findByRole("article", { name: "CORE-1 Alpha" });
  act(() => alpha.focus());
  await user.keyboard("[Space]");
  await user.keyboard("{ArrowDown}");
  await user.keyboard("[Space]");
  await act(async () => {});
  expect(patch).not.toHaveBeenCalled();
  expect(liftedCards(region)).toEqual([]);
  const describedBy = alpha.getAttribute("aria-describedby");
  expect(describedBy).not.toBeNull();
  const instructions = document.getElementById(describedBy as string)?.textContent ?? "";
  expect(instructions).toContain("Shift with J, K, H or L");
  expect(instructions).not.toContain("space bar");
});

// The click the browser fires after a drop is the drag's, however late the main thread delivers
// it: a busy tab can take far longer than any window of milliseconds between the drop and the
// click, and a drop that navigates to the issue page loses the Board the reader was arranging.
/** Press a card and move far enough to lift it, then release it where it was picked up. */
async function dropInPlace(card: HTMLElement): Promise<void> {
  await act(async () => {
    // dnd-kit's mouse sensor is driven by this press; user-event's pointer API sends a fixed
    // sequence that never lifts the card.
    fireEvent.mouseDown(card, { button: 0, clientX: 10, clientY: 10 });
  });
  await act(async () => {
    // The move that lifts the card: user-event sends no mousemove of its own.
    fireEvent.mouseMove(document, { clientX: 10, clientY: 40 });
  });
  expect(card.className).toContain("opacity-50");
  await act(async () => {
    // The release that drops it, the other half of a gesture user-event cannot drive.
    fireEvent.mouseUp(document, { clientX: 10, clientY: 40 });
  });
}

/** The card as the drop drew it. */
async function droppedCard(): Promise<HTMLElement> {
  return within(await boardRegion()).getByRole("article", { name: "CORE-1 Alpha" });
}

/** Waits until a click reaches React again: dnd-kit stops click propagation for 50 ms after a
 * drag ends, with a capture listener on `document` that a 50 ms timer removes (`detach` in
 * `@dnd-kit/core`) and no unmount cancels. Every row that drags ends through this: the next row
 * shares `document`, and a row that ends inside the 50 ms hands it that listener, so the next
 * row's first click never reaches its target. */
async function pastDndKitClickGuard(): Promise<void> {
  await waitMs(80);
}

/** A PATCH that answers at once with the issue as it was. */
function patchAnswered() {
  const patch = answerIssuePatches().mockImplementation(async (key) => saved(key));
  restores.push(() => patch.mockRestore());
  return patch;
}

test("the click that trails a drop never follows the card's link, however late it arrives", async () => {
  patchAnswered();
  board(keyboardIssues(), "?layout=board");
  const region = await boardRegion();
  await dropInPlace(await within(region).findByRole("article", { name: "CORE-1 Alpha" }));
  const link = within(await droppedCard()).getByRole("link", { name: /CORE-1/ });
  const now = spyOn(performance, "now").mockReturnValue(performance.now() + 1000);
  let trailingFollowed = true;
  try {
    await act(async () => {
      // The assertion reads fireEvent's boolean: whether the click was cancelled.
      trailingFollowed = fireEvent.click(link, { button: 0 });
    });
  } finally {
    now.mockRestore();
  }
  expect(trailingFollowed).toBe(false);
  expect(screen.getByTestId("route").textContent).toBe("/projects/CORE/issues");
  await pastDndKitClickGuard();
});

// Only the drag's own click is the drag's: the mark a drop leaves does not outlive the gesture
// that follows it, by pointer or by key.
test("a drop that draws no click leaves the next press on the Board working", async () => {
  const user = userEvent.setup();
  patchAnswered();
  board(keyboardIssues(), "?layout=board");
  const region = await boardRegion();
  await dropInPlace(await within(region).findByRole("article", { name: "CORE-1 Alpha" }));
  await pastDndKitClickGuard();
  act(() => within(region).getByRole("article", { name: "CORE-1 Alpha" }).focus());
  await user.keyboard("{Enter}");
  expect(screen.getByTestId("route").textContent).toBe("/issues/CORE-1");
});

test("a drag cancelled with Escape keeps its own release off the card's link", async () => {
  patchAnswered();
  board(keyboardIssues(), "?layout=board");
  const region = await boardRegion();
  const alpha = await within(region).findByRole("article", { name: "CORE-1 Alpha" });
  await act(async () => {
    // The press that starts a dnd-kit drag, as in `dropInPlace` above.
    fireEvent.mouseDown(alpha, { button: 0, clientX: 10, clientY: 10 });
  });
  await act(async () => {
    // The move that lifts the card: user-event sends no mousemove of its own.
    fireEvent.mouseMove(document, { clientX: 10, clientY: 40 });
  });
  await act(async () => {
    // dnd-kit's own cancel reads `code`, not `key`, and this key is one step of the mouse drag
    // above, which user-event cannot drive.
    fireEvent.keyDown(document, { code: "Escape", key: "Escape" });
  });
  await act(async () => {
    // The release of that same drag, with the button still down.
    fireEvent.mouseUp(document, { clientX: 10, clientY: 40 });
  });
  expect(liftedCards(region)).toEqual([]);
  let followed = true;
  await act(async () => {
    // The assertion reads fireEvent's boolean: whether the click was cancelled.
    followed = fireEvent.click(within(region).getByRole("link", { name: /CORE-1/ }), {
      button: 0,
    });
  });
  expect(followed).toBe(false);
  expect(screen.getByTestId("route").textContent).toBe("/projects/CORE/issues");
  await pastDndKitClickGuard();
});

test("a drop's mark never cancels a click outside the Board", async () => {
  patchAnswered();
  board(keyboardIssues(), "?layout=board");
  const elsewhere = document.createElement("a");
  elsewhere.href = "/issues/CORE-2";
  document.body.append(elsewhere);
  restores.push(() => elsewhere.remove());
  const region = await boardRegion();
  await dropInPlace(await within(region).findByRole("article", { name: "CORE-1 Alpha" }));
  let followed = false;
  await act(async () => {
    // The assertion reads fireEvent's boolean: whether the click was cancelled.
    followed = fireEvent.click(elsewhere, { button: 0 });
  });
  expect(followed).toBe(true);
  await pastDndKitClickGuard();
});

// A listing read asked for while a move is unanswered waits for the answer: the server may not
// have applied the move, and its list would put the card back.
test("a listing read asked for while a move is unanswered waits for the answer, then reads the moved Board", async () => {
  const user = userEvent.setup();
  const answer = Promise.withResolvers<Issue>();
  const patch = answerIssuePatches().mockImplementation(() => answer.promise);
  restores.push(() => patch.mockRestore());
  const rows = keyboardIssues();
  const { client, listing } = board(rows, "?layout=board");
  const region = await boardRegion();
  act(() => within(region).getByRole("article", { name: "CORE-2 Bravo" }).focus());
  await user.keyboard("{Shift>}L{/Shift}");
  const inProgress = within(region).getByRole("region", { name: "In progress" });
  await waitFor(() => expect(cardKeys(inProgress)).toEqual(["CORE-2", "CORE-4"]));
  const reads = listing.mock.calls.length;
  await act(async () => {
    void client.invalidateQueries({ queryKey: ["issues"] });
  });
  expect(listing.mock.calls.length).toBe(reads);
  expect(cardKeys(inProgress)).toEqual(["CORE-2", "CORE-4"]);

  rows[1] = { ...(rows[1] as IssueSummary), status: "in_progress", rank: "0" };
  await act(async () => {
    answer.resolve(saved("CORE-2", { status: "in_progress" }));
  });
  await waitFor(() => expect(listing.mock.calls.length).toBeGreaterThan(reads));
  await waitFor(() => expect(cardKeys(inProgress)).toEqual(["CORE-2", "CORE-4"]));
  expect(within(inProgress).getByRole("article", { name: "CORE-2 Bravo" })).toHaveFocus();
});

// A view that lists done issues reads its open and its done issues apart. A card moved out of
// Done leaves the page with the done read's answer while the open read is out, and comes back
// with the open read's answer: the focus comes back to it.
test("Shift+H out of Done on a view that lists done issues keeps the focus on the card through the reads after the move", async () => {
  const user = userEvent.setup();
  const rows = [
    issue("CORE-5", { rank: "e", status: "done", title: "Echo" }),
    issue("CORE-6", { rank: "f", status: "retro", title: "Foxtrot" }),
  ];
  const { listing } = board(rows, "?layout=board&nofilter=1");
  const region = await boardRegion();
  const done = within(region).getByRole("region", { name: "Done" });
  const echo = await within(done).findByRole("article", { name: "CORE-5 Echo" });
  // Once the move is written, the open read waits for `openRead` and the done read answers.
  const openRead = Promise.withResolvers<void>();
  let written = false;
  listing.mockImplementation(async (options = {}) => {
    if (written && options.open) await openRead.promise;
    return rows.filter(
      (row) =>
        (!options.open || row.status !== "done") &&
        (options.status === undefined || options.status.includes(row.status))
    );
  });
  const patch = answerIssuePatches().mockImplementation(async () => {
    rows[0] = { ...(rows[0] as IssueSummary), closed_at: null, rank: "a", status: "retro" };
    written = true;
    return saved("CORE-5", { status: "retro" });
  });
  restores.push(() => patch.mockRestore());
  act(() => echo.focus());
  await user.keyboard("{Shift>}H{/Shift}");
  expect(patch).toHaveBeenCalledWith(
    "CORE-5",
    { rank: { before: "CORE-6" }, status: "retro" },
    keyedAttempt
  );
  await waitFor(() => expect(cardKeys(region)).toEqual(["CORE-6"]));
  await act(async () => openRead.resolve());
  const retro = within(region).getByRole("region", { name: "Retro" });
  await waitFor(() => expect(cardKeys(retro)).toEqual(["CORE-5", "CORE-6"]));
  await waitFor(() =>
    expect(within(retro).getByRole("article", { name: "CORE-5 Echo" })).toHaveFocus()
  );
});

/** The move hook alone, over a client that holds `open` as the open listing. */
function renderMoves(open: IssueSummary[]) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const key = ["issues", "project", "CORE", "open"] as const;
  client.setQueryData(key, open);
  const hook = renderHook(() => useBoardMoves("CORE"), {
    wrapper: ({ children }: { children: ReactNode }) => (
      <QueryClientProvider client={client}>{children}</QueryClientProvider>
    ),
  });
  restores.push(() => hook.unmount());
  return { client, hook, key };
}

const move = (input: UpdateIssueInput, rank?: string) => ({
  input,
  ...(rank === undefined ? {} : { rank }),
});

test("a refused move puts the listing back, reads it again and shows the server's message", async () => {
  const open = keyboardIssues();
  const { client, hook, key } = renderMoves(open);
  const patch = answerIssuePatches().mockRejectedValue(
    new ApiError(409, { code: "ISSUE_CLOSED", error: "issue is closed" })
  );
  restores.push(() => patch.mockRestore());
  await act(() =>
    hook.result.current.move(
      "CORE-1",
      move({ status: "in_progress", rank: { before: "CORE-4" } }, "")
    )
  );
  expect(patch).toHaveBeenCalledWith(
    "CORE-1",
    { status: "in_progress", rank: { before: "CORE-4" } },
    keyedAttempt
  );
  expect(hook.result.current.error).toBe("issue is closed");
  expect(client.getQueryData<IssueSummary[]>(key)).toEqual(open);
  expect(client.getQueryState(key)?.isInvalidated).toBe(true);
});

test("any other failed move shows the server's message and reads the listing again", async () => {
  const { client, hook, key } = renderMoves(keyboardIssues());
  const patch = answerIssuePatches().mockRejectedValue(
    new ApiError(403, { code: "FORBIDDEN", error: "not your project" })
  );
  restores.push(() => patch.mockRestore());
  await act(() => hook.result.current.move("CORE-1", move({ status: "backlog" })));
  expect(hook.result.current.error).toBe("not your project");
  expect(client.getQueryState(key)?.isInvalidated).toBe(true);
});

// Every listing a view reads that holds the card takes the move at once, its other issues kept
// as they were, and two moves on one Board go out one after the other.
test("a move writes every listing that holds the card at once, and two moves go out one after the other", async () => {
  const { client, hook, key } = renderMoves(keyboardIssues());
  const doneKey = ["issues", "project", "CORE", "done", "", ""] as const;
  const closed = [issue("CORE-7", { status: "done" })];
  client.setQueryData(doneKey, closed);
  const settle: ((issue: Issue) => void)[] = [];
  const patch = answerIssuePatches().mockImplementation(() => {
    const { promise, resolve } = Promise.withResolvers<Issue>();
    settle.push(resolve);
    return promise;
  });
  restores.push(() => patch.mockRestore());
  const before = client.getQueryData<IssueSummary[]>(key) ?? [];
  let first: Promise<void> = Promise.resolve();
  let second: Promise<void> = Promise.resolve();
  act(() => {
    first = hook.result.current.move(
      "CORE-1",
      move({ status: "in_progress", rank: { before: "CORE-4" } }, "")
    );
    second = hook.result.current.move("CORE-7", move({ status: "todo" }));
  });
  const after = client.getQueryData<IssueSummary[]>(key) ?? [];
  expect(after.find((row) => row.key === "CORE-1")).toMatchObject({
    status: "in_progress",
    rank: "",
  });
  // An issue the move left alone keeps its object.
  expect(after[1]).toBe(before[1] as IssueSummary);
  expect(client.getQueryData<IssueSummary[]>(doneKey)?.[0]?.status).toBe("todo");
  await waitFor(() => expect(patch).toHaveBeenCalledTimes(1));
  await act(async () => {
    settle[0]?.(saved("CORE-1"));
    await first;
  });
  await waitFor(() => expect(patch).toHaveBeenCalledTimes(2));
  expect(patch.mock.calls[1]?.[0]).toBe("CORE-7");
  await act(async () => {
    settle[1]?.(saved("CORE-7"));
    await second;
  });
  expect(hook.result.current.error).toBeUndefined();
});

// The control row above the Board: the view's search and the Filters panel, with the Labels
// picker and the three flags, and a chip for every narrowing that differs from the view's own.
test("every narrowing that differs from the view's own is a chip, and Clear filters takes them all off", async () => {
  const user = userEvent.setup();
  board(
    [
      issue("CORE-1", { labels: ["frontend"], title: "core work", priority: 0 }),
      issue("CORE-2", { labels: ["frontend"], title: "other work", priority: 0 }),
      issue("CORE-3", { title: "core work", priority: 1 }),
    ],
    "?layout=board&label=frontend&q=core&status=todo&f.priority=0&needs-you=1"
  );
  await boardRegion();
  expect(screen.getByRole("button", { name: "Filters · 5" })).toBeDefined();
  for (const chip of [
    "Label: frontend",
    "Search: core",
    "Needs you",
    "Status: Todo",
    "Priority: P0",
  ])
    expect(screen.getByRole("button", { name: `Remove ${chip} filter` })).toBeDefined();
  await user.click(screen.getByRole("button", { name: "Remove Priority: P0 filter" }));
  await waitFor(() => expect(routeSearch().has("f.priority")).toBe(false));
  expect(screen.getByRole("button", { name: "Filters · 4" })).toBeDefined();
  await user.click(screen.getByRole("button", { name: "Clear filters" }));
  await waitFor(() => expect(routeSearch().toString()).toBe("layout=board"));
  expect(screen.getByRole("button", { name: "Filters · 0" })).toBeDefined();
  expect(screen.queryByRole("button", { name: "Clear filters" })).toBeNull();
});

test("the Labels picker writes label=, shows its chip, and the cards are the view's rows of that address", async () => {
  const user = userEvent.setup();
  const { listing } = board(
    [
      issue("CORE-1", { labels: ["frontend", "api"], title: "Navigation" }),
      issue("CORE-2", { labels: ["docs"], title: "Guidance" }),
    ],
    "?layout=board"
  );
  const region = await boardRegion();
  await waitFor(() => expect(cardKeys(region)).toEqual(["CORE-1", "CORE-2"]));
  await user.click(screen.getByRole("button", { name: "Filters · 0" }));
  await user.click(screen.getByRole("button", { name: "Labels" }));
  await user.click(screen.getByRole("option", { name: "docs" }));
  await waitFor(() => expect(routeSearch().getAll("label")).toEqual(["docs"]));
  await waitFor(() => expect(cardKeys(region)).toEqual(["CORE-2"]));
  expect(screen.getByRole("button", { name: "Remove Label: docs filter" })).toBeDefined();
  // The view narrows the listing it read: no read names the label.
  expectNoFullListing(listing);
});

test("the search box is the view's search, and a search the address brings is a chip", async () => {
  const user = userEvent.setup();
  board(
    [issue("CORE-1", { title: "Guidance" }), issue("CORE-2", { title: "Navigation" })],
    "?layout=board&q=guid"
  );
  const region = await boardRegion();
  await waitFor(() => expect(cardKeys(region)).toEqual(["CORE-1"]));
  expect(screen.getByRole("button", { name: "Remove Search: guid filter" })).toBeDefined();
  const search = screen.getByRole("searchbox", { name: "Search issues" });
  expect(search).toHaveValue("guid");
  await user.clear(search);
  await waitFor(() => expect(cardKeys(region)).toEqual(["CORE-1", "CORE-2"]));
  expect(routeSearch().has("q")).toBe(false);
});

const PANEL_CHOICE = userPreferenceStorageKey("alice", "project.issue-filters");

// The stored choice opens or shuts the Filters panel as the Board loads and takes no focus, and
// only the reader's own open or close writes it.
test("a Filters panel the stored choice opens takes no focus, and only the reader's open and close write it", async () => {
  const user = userEvent.setup();
  const storage = installCountingStorage();
  restores.push(() => storage.restore());
  board([issue("CORE-1", { labels: ["bug"], title: "Core work" })], "?layout=board&label=bug");
  const region = await boardRegion();
  await waitFor(() => expect(cardKeys(region)).toEqual(["CORE-1"]));
  const trigger = screen.getByRole("button", { name: "Filters · 1" });
  // Nothing is stored and a narrowing is on: the panel stands open, with no focus in it.
  expect(trigger).toHaveAttribute("aria-expanded", "true");
  expect(document.activeElement).toBe(document.body);
  expect(storage.writes.filter(([key]) => key === PANEL_CHOICE)).toEqual([]);
  await user.click(trigger);
  await user.click(trigger);
  expect(
    within(screen.getByRole("dialog", { name: "Filters" })).getByRole("button", {
      name: "Labels · 1",
    })
  ).toHaveFocus();
  await user.keyboard("{Escape}");
  expect(trigger).toHaveAttribute("aria-expanded", "false");
  expect(storage.writes.filter(([key]) => key === PANEL_CHOICE)).toEqual([
    [PANEL_CHOICE, "collapsed"],
    [PANEL_CHOICE, "expanded"],
    [PANEL_CHOICE, "collapsed"],
  ]);
});

/** Open's rows for the redraw rows: `count` cards, a third of them In progress. */
const manyRows = (count: number) =>
  Array.from({ length: count }, (_, index) =>
    issue(`CORE-${1000 + index}`, {
      rank: String(1000 + index),
      status: index % 3 === 0 ? "in_progress" : "todo",
    })
  );

/** Counts the card bodies and card shells a change draws: every card body draws
 *  `UnreachableRouteMarker` once, and every card shell calls `useDraggable` once, and on the
 *  Board nothing else does either. */
function countCards() {
  const bodies = spyOn(routeReach, "UnreachableRouteMarker");
  const shells = spyOn(dndCore, "useDraggable");
  restores.push(() => {
    bodies.mockRestore();
    shells.mockRestore();
  });
  return {
    clear() {
      bodies.mockClear();
      shells.mockClear();
    },
    bodies: () => bodies.mock.calls.length,
    shells: () => shells.mock.calls.length,
  };
}

// R11: a live change to one issue draws that issue's card, not the Board's others.
test("a live read that moves one card of 120 draws that card alone", async () => {
  const rows = manyRows(120);
  const { client } = board(rows, "?layout=board");
  const region = await boardRegion();
  await waitFor(() => expect(cardKeys(region)).toHaveLength(120));
  const counts = countCards();
  counts.clear();
  const index = rows.findIndex((row) => row.status === "todo");
  const key = rows[index]?.key ?? "";
  rows[index] = {
    ...(rows[index] as IssueSummary),
    status: "in_progress",
    updated_at: "2026-11-17T09:00:00Z",
  };
  await act(async () => {
    await client.invalidateQueries({ queryKey: ["issues"] });
  });
  await waitFor(() =>
    expect(
      region.querySelector(`[data-board-column="in_progress"] [data-board-card="${key}"]`)
    ).not.toBeNull()
  );
  // Whatever the change still draws after the card lands is counted too.
  await waitMs(100);
  expect(counts.bodies()).toBeGreaterThan(0);
  expect(counts.bodies()).toBeLessThanOrEqual(2);
  expect(counts.shells()).toBeLessThanOrEqual(2);
});

// A card compares the Time in Legion phase text it draws, not the clock: a draw in a new minute
// draws again only the cards whose time reads differently.
test("a draw in a new minute draws again only the cards whose Time in Legion phase text changed", async () => {
  const start = new Date("2026-11-17T12:00:30Z");
  setSystemTime(start);
  restores.push(() => setSystemTime());
  const sockets = liveSockets(restores);
  const rows = manyRows(30);
  const minutes = (count: number) => new Date(start.getTime() - count * 60_000).toISOString();
  board(rows, "?layout=board", DAEMON);
  const cards = rows.map((row, index) =>
    // One card counts minutes; every other card has been in its phase for days, which reads the
    // same a minute on.
    legionCard(row.key, { phase_since: index === 1 ? minutes(30) : minutes(3 * 24 * 60 + 7) })
  );
  await answerBoard(sockets, legionBoard({ implementing: cards }));
  const region = await boardRegion();
  await waitFor(() => expect(cardKeys(region)).toHaveLength(30));
  const ticking = rows[1]?.key ?? "";
  expect(
    region.querySelector(`[data-board-card="${ticking}"] [data-legion-fact=phase-time]`)
      ?.textContent
  ).toBe("Time in Legion phase30 m");
  const counts = countCards();
  counts.clear();
  setSystemTime(new Date(start.getTime() + 60_000));
  // Another reader's change to one other issue draws the Board again.
  const changed = rows[5]?.key ?? "";
  await answerBoard(
    sockets,
    legionBoard({
      implementing: cards.map((card) =>
        card.key === changed ? { ...card, roles: ["implementer"] } : card
      ),
    })
  );
  await waitFor(() =>
    expect(
      region.querySelector(`[data-board-card="${changed}"] [data-legion-fact=roles]`)
    ).not.toBeNull()
  );
  expect(
    region.querySelector(`[data-board-card="${ticking}"] [data-legion-fact=phase-time]`)
      ?.textContent
  ).toBe("Time in Legion phase31 m");
  expect(counts.bodies()).toBeGreaterThanOrEqual(2);
  expect(counts.bodies()).toBeLessThanOrEqual(4);
});

/** The height of each card in the geometry below, and where a column's first card stands. */
const CARD_PX = 120;
const FIRST_CARD_TOP = 200;

/**
 * The geometry the Board's anchor reads, which the unit DOM has no layout engine for: the
 * columns 288 px wide and 304 px apart, In progress in the middle of the window, each card
 * CARD_PX tall from FIRST_CARD_TOP in its column, all scrolled by `window.scrollY`, which the
 * test and `scrollBy` move; a hit test answers the card drawn at that point. Returns the scroll
 * a reader makes, as the browser reports it.
 */
function boardGeometry(): (top: number) => void {
  const scrollY = Object.getOwnPropertyDescriptor(window, "scrollY");
  const setScrollY = (value: number) =>
    Object.defineProperty(window, "scrollY", { configurable: true, value, writable: true });
  const scrollBy = window.scrollBy;
  window.scrollBy = ((x: number | ScrollToOptions, y?: number) => {
    const top = typeof x === "number" ? y : x.top;
    if (top !== undefined) setScrollY(window.scrollY + top);
  }) as typeof window.scrollBy;
  setScrollY(0);
  const columns = () => [...document.querySelectorAll<HTMLElement>("[data-board-column]")];
  const middle = () =>
    columns().findIndex((column) => column.dataset.boardColumn === "in_progress");
  const leftOf = (index: number) => window.innerWidth / 2 - 144 + (index - middle()) * 304;
  // On `Element.prototype`, which owns the method: bun's `mockRestore` leaves a copy on a
  // prototype that only inherits it.
  const rect = spyOn(Element.prototype, "getBoundingClientRect").mockImplementation(function (
    this: Element
  ) {
    const column = this.closest<HTMLElement>("[data-board-column]");
    if (this.hasAttribute("data-board-card") && column !== null) {
      const index = [...column.querySelectorAll("[data-board-card]")].indexOf(this);
      return new DOMRect(
        leftOf(columns().indexOf(column)),
        FIRST_CARD_TOP + index * CARD_PX - window.scrollY,
        288,
        CARD_PX
      );
    }
    return new DOMRect(0, -window.scrollY, window.innerWidth, 3000);
  });
  const point = spyOn(document, "elementFromPoint").mockImplementation((x, y) => {
    const at = middle() + Math.floor((x - leftOf(middle())) / 304);
    const column = columns()[at];
    if (column === undefined || x - leftOf(at) > 288) return null;
    const index = Math.floor((y + window.scrollY - FIRST_CARD_TOP) / CARD_PX);
    return index < 0
      ? null
      : (column.querySelectorAll<HTMLElement>("[data-board-card]")[index] ?? null);
  });
  restores.push(() => {
    rect.mockRestore();
    point.mockRestore();
    window.scrollBy = scrollBy;
    if (scrollY === undefined) Reflect.deleteProperty(window, "scrollY");
    else Object.defineProperty(window, "scrollY", scrollY);
  });
  return (top) => {
    setScrollY(top);
    document.dispatchEvent(new Event("scroll"));
  };
}

/** A card's top on screen, by its key. */
const cardTop = (key: string) =>
  document.querySelector(`[data-board-card="${key}"]`)?.getBoundingClientRect().top;

/** The keys of a column's cards, in its order. */
const columnCards = (column: string) =>
  cardKeys(document.querySelector<HTMLElement>(`[data-board-column="${column}"]`) as HTMLElement);

/** Waits until the frame the Board's last draw went into is painted. */
const painted = () => waitMs(50);

// R11: the Board keeps the reader's place as a table does. A card another reader adds above the
// card the reader's hand is on does not move that card on screen: the window scrolls by the added
// card instead. A card that itself leaves its column is not followed. That the Board's scroll
// boxes leave the window to the anchor alone is the browser's own scroll anchoring, which the
// unit DOM does not do: the browser row "on the Board a card the pointer rests on or the focus
// holds keeps its top through writes above it" (`e2e/issues-views-live.e2e.ts`) measures it.
test("a card added above the card in hand keeps its top, and a card that leaves its column is not followed", async () => {
  boardGeometry();
  const rows = [issue("CORE-2"), issue("CORE-3")];
  const { client } = board(rows, "?layout=board");
  const region = await boardRegion();
  const reading = await within(region).findByRole("article", { name: "CORE-3 Work CORE-3" });
  act(() => reading.focus());
  const before = cardTop("CORE-3");
  expect(before).toBe(FIRST_CARD_TOP + CARD_PX);

  rows.unshift(issue("CORE-1"));
  await act(() => client.invalidateQueries({ queryKey: ["issues"] }));
  await waitFor(() => expect(columnCards("todo")).toEqual(["CORE-1", "CORE-2", "CORE-3"]));
  expect(cardTop("CORE-3")).toBe(before);
  expect(window.scrollY).toBe(CARD_PX);

  rows[2] = issue("CORE-3", { status: "backlog" });
  await act(() => client.invalidateQueries({ queryKey: ["issues"] }));
  await waitFor(() => expect(columnCards("backlog")).toEqual(["CORE-3"]));
  expect(window.scrollY).toBe(CARD_PX);
});

// R11: with no hand on a card and the window at its top, the anchor holds the first card of the
// column nearest the middle once the reader has seen it: a card added above it in that column
// stands above it, the window scrolling by its height, and the counts follow.
test("a card added above the first card of the middle column keeps that card's top at the top", async () => {
  boardGeometry();
  const rows = [
    issue("CORE-2"),
    issue("CORE-5", { status: "in_progress" }),
    issue("CORE-6", { status: "in_progress" }),
    issue("CORE-8", { status: "needs_review" }),
  ];
  const { client } = board(rows, "?layout=board");
  await within(await boardRegion()).findByRole("article", { name: "CORE-5 Work CORE-5" });
  await painted();
  expect(cardTop("CORE-5")).toBe(FIRST_CARD_TOP);

  rows.unshift(issue("CORE-4", { status: "in_progress" }));
  await act(() => client.invalidateQueries({ queryKey: ["issues"] }));
  await waitFor(() => expect(columnCards("in_progress")).toEqual(["CORE-4", "CORE-5", "CORE-6"]));
  expect(cardTop("CORE-5")).toBe(FIRST_CARD_TOP);
  expect(window.scrollY).toBe(CARD_PX);
  expect(screen.getByRole("tab", { name: "Open, 5 issues" })).toBeDefined();
});

// R11: the Board draws nothing when the window scrolls, so a reader who scrolled down, saw a
// change land there and scrolled back to the top is held by the record of their own scroll: the
// middle column's first card keeps its top when a card is added above it.
test("after a change while scrolled and a scroll back to the top, the middle column's first card keeps its top", async () => {
  const scrollTo = boardGeometry();
  const rows = [
    issue("CORE-5", { status: "in_progress" }),
    issue("CORE-6", { status: "in_progress" }),
    issue("CORE-7", { status: "in_progress" }),
  ];
  const { client } = board(rows, "?layout=board");
  await within(await boardRegion()).findByRole("article", { name: "CORE-5 Work CORE-5" });
  await painted();
  scrollTo(2 * CARD_PX + FIRST_CARD_TOP);
  rows[2] = issue("CORE-7", { status: "in_progress", title: "Work CORE-7, reworded" });
  await act(() => client.invalidateQueries({ queryKey: ["issues"] }));
  await screen.findByRole("article", { name: "CORE-7 Work CORE-7, reworded" });
  await painted();
  scrollTo(0);
  await painted();
  expect(cardTop("CORE-5")).toBe(FIRST_CARD_TOP);

  rows.unshift(issue("CORE-4", { status: "in_progress" }));
  await act(() => client.invalidateQueries({ queryKey: ["issues"] }));
  await waitFor(() => expect(columnCards("in_progress")).toHaveLength(4));
  expect(cardTop("CORE-5")).toBe(FIRST_CARD_TOP);
  expect(window.scrollY).toBe(CARD_PX);
});

// R11: the Board's anchor is off while a card is dragged, until the drop is drawn: the dragged
// card moves under the reader's hand, and a change landing then is not held against it.
test("a card added while another is dragged is not held, and the anchor holds again after the drop", async () => {
  boardGeometry();
  patchAnswered();
  const rows = [
    issue("CORE-5", { status: "in_progress" }),
    issue("CORE-6", { status: "in_progress" }),
  ];
  const { client } = board(rows, "?layout=board");
  const region = await boardRegion();
  const dragged = await within(region).findByRole("article", { name: "CORE-6 Work CORE-6" });
  await painted();
  await act(async () => {
    // The press that starts a dnd-kit drag, as in `dropInPlace` above.
    fireEvent.mouseDown(dragged, { button: 0, clientX: 10, clientY: 10 });
  });
  await act(async () => {
    // The move that lifts the card: user-event sends no mousemove of its own.
    fireEvent.mouseMove(document, { clientX: 10, clientY: 40 });
  });
  expect(liftedCards(region)).toEqual(["CORE-6"]);

  rows.unshift(issue("CORE-4", { status: "in_progress" }));
  await act(() => client.invalidateQueries({ queryKey: ["issues"] }));
  await waitFor(() => expect(columnCards("in_progress")).toHaveLength(3));
  expect(window.scrollY).toBe(0);

  await act(async () => {
    // dnd-kit's own cancel reads `code`, not `key` (`core.esm.js` `handleKeydown`), and this
    // key is one step of the mouse drag above, which user-event cannot drive.
    fireEvent.keyDown(document, { code: "Escape", key: "Escape" });
  });
  await act(async () => {
    // The release of that same drag, with the button still down.
    fireEvent.mouseUp(document, { clientX: 10, clientY: 40 });
  });
  await painted();
  expect(cardTop("CORE-4")).toBe(FIRST_CARD_TOP);
  rows.unshift(issue("CORE-3", { status: "in_progress" }));
  await act(() => client.invalidateQueries({ queryKey: ["issues"] }));
  await waitFor(() => expect(columnCards("in_progress")).toHaveLength(4));
  expect(cardTop("CORE-4")).toBe(FIRST_CARD_TOP);
  expect(window.scrollY).toBe(CARD_PX);
  await pastDndKitClickGuard();
});

/** The move hook alone, over `client`. */
function moveHook(client: QueryClient) {
  return renderHook(() => useBoardMoves("CORE"), {
    wrapper: ({ children }: { children: ReactNode }) => (
      <QueryClientProvider client={client}>{children}</QueryClientProvider>
    ),
  });
}

// A move the reader stopped may have been made: the Board goes back to where the page read it,
// reads itself again, and says so, rather than a failure it cannot know of.
test("a move the reader stopped says it may have been made, not that it failed", async () => {
  jest.useFakeTimers();
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const queryKey = ["issues", "project", "CORE"] as const;
  queryClient.setQueryData(queryKey, keyboardIssues());
  const patchIssue = answerIssuePatches().mockRejectedValue(
    new NetworkError("/api/v1/issues/CORE-1", new TypeError("Failed to fetch"))
  );
  const listIssues = spyOn(api, "listIssues").mockResolvedValue(keyboardIssues());
  const hook = moveHook(queryClient);
  try {
    let moved: Promise<void> | undefined;
    act(() => {
      moved = hook.result.current.move("CORE-1", move({ rank: { after: "CORE-2" } }, "b\u0000"));
    });
    await stopTheLateWrite(queryClient);
    await act(async () => moved);
    expect(hook.result.current.error).toBe(
      stoppedWriteNote("the move of CORE-1", "made", {
        anew: "The board is read again from Dispatch, and moving the card again sends a new move.",
      })
    );
  } finally {
    realClock();
    hook.unmount();
    queryClient.clear();
    patchIssue.mockRestore();
    listIssues.mockRestore();
  }
});

// The Board's rows of the parent-status question's state table (INFRA-4731's plan, "Lists named
// before code"). A card with a parent or an open child (`progress.children`) moves to another
// status only after the rule's dry run; the route's answers are the real server's
// (`status-change-fixtures.ts`).

/** CORE-1 in Todo, a card with one open child. */
const parentCard = issue("CORE-1", {
  progress: { children: { done: 0, total: 1 }, tasks: null },
  rank: "a",
  status: "todo",
  title: "Parent",
});

/** CORE-1 with a child in work, then two cards with neither a parent nor a child. */
const parentIssues: IssueSummary[] = [
  parentCard,
  issue("CORE-2", { rank: "b", status: "todo", title: "Bravo" }),
  issue("CORE-3", { rank: "c", status: "in_progress", title: "Charlie" }),
];

/** Which request of the route `input` is: a dry run or a write, with `include_children` or not. */
function statusChangeOf(input: { dry_run?: boolean; include_children?: boolean }): string {
  const option = input.include_children === true ? "include_children" : "another option";
  return `${input.dry_run === true ? "dry run" : "write"} with ${option}`;
}

/** A move that asks: its own dry run is refused by the rule (ACME-4's child in work), once `held`
 *  settles where a row holds it, `include_children`'s dry run answers, `unlink_children`'s is held,
 *  and `write` answers the status change with the option. */
function answerTheQuestion(write: () => Promise<StatusChangeAnswer>, held?: Promise<void>) {
  return answerStatusChanges().mockImplementation(async (_key, input) => {
    if (input.dry_run !== true) return write();
    if (input.include_children === true) return fixtureAnswer("dry-run");
    if (input.unlink_children === true) return new Promise<StatusChangeAnswer>(() => {});
    if (held !== undefined) await held;
    throw fixtureRefusal("conflict");
  });
}

test("board: checking - a second drop while the first is checking waits behind it in the board's one move scope", async () => {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const queryKey = ["issues", "project", "CORE"] as const;
  queryClient.setQueryData(queryKey, parentIssues);
  const dryRun = Promise.withResolvers<StatusChangeAnswer>();
  const statusChanges = answerStatusChanges().mockImplementation(() => dryRun.promise);
  const patchIssue = answerIssuePatches().mockResolvedValue(saved("CORE-1"));
  const hook = moveHook(queryClient);
  try {
    let first: Promise<void> = Promise.resolve();
    let second: Promise<void> = Promise.resolve();
    await act(async () => {
      const toInProgress = move({ rank: { before: "CORE-3" }, status: "in_progress" }, "");
      first = hook.result.current.move("CORE-1", toInProgress);
      second = hook.result.current.move("CORE-2", toInProgress);
      await settle();
    });
    expect(statusChanges).toHaveBeenCalledTimes(1);
    // The card being checked has not moved; the second has, and its PATCH waits its turn.
    const statusOf = (key: string) =>
      queryClient.getQueryData<IssueSummary[]>(queryKey)?.find((entry) => entry.key === key)
        ?.status;
    expect(statusOf("CORE-1")).toBe("todo");
    expect(statusOf("CORE-2")).toBe("in_progress");
    expect(patchIssue).not.toHaveBeenCalled();

    await act(async () => {
      dryRun.resolve(fixtureAnswer("dry-run"));
      await first;
      await second;
    });
    expect(patchIssue.mock.calls.map(([key, input]) => [key, input.status])).toEqual([
      ["CORE-1", "in_progress"],
      ["CORE-2", "in_progress"],
    ]);
    expect(hook.result.current.error).toBeUndefined();
  } finally {
    hook.unmount();
    patchIssue.mockRestore();
    statusChanges.mockRestore();
  }
});

/** A dialog of another part of the page, open over the Board: `useDialog` takes the focus as it
 *  opens and gives it to its control a frame later. */
function AnotherDialog(): ReactNode {
  const dialog = useDialog<HTMLDivElement>({ onClose: () => {}, open: true });
  return (
    <div aria-label="Another dialog" ref={dialog.containerRef} role="dialog">
      <button type="button">In another dialog</button>
    </div>
  );
}

// INFRA-3135: a keyboard move the rule's dry run checks first places the card only once the dry
// run answers. The focus follows the card then only while it stands in the card the reader's press
// moved: on the card itself, or on a control inside it, as a Tab onto the card's own link leaves
// it. The Board's draw of the card in its new column takes that control away with the card, so
// the focus goes with the card. A reader who has moved on keeps their place: here `j` to the next
// card, or a blur that puts the focus on the body, as a click on a blank spot of the page's shell
// does in Chromium and WebKit. That draw removes the node the press focused, so after it the body
// tells nothing: only what the move read as it placed the card tells the reader's blur from the
// card's going. Another dialog open over the Board is no place the press left the focus: with the
// focus on the body under it, as a press on its text leaves it, the move takes no focus to the
// card behind it.
for (const reader of [
  "stayed",
  "put the focus on the card's own link",
  "moved on",
  "put the focus on the body",
  "opened another dialog and put the focus on the body",
] as const) {
  test(`board: checking - focus follows a checked keyboard move only while it stands in the card the press moved (the reader ${reader})`, async () => {
    const user = userEvent.setup();
    const dryRun = Promise.withResolvers<StatusChangeAnswer>();
    const statusChanges = answerStatusChanges().mockImplementation(() => dryRun.promise);
    const patchIssue = patchInFlight();
    board(parentIssues, "?layout=board");
    let closeAnother: (() => void) | undefined;
    try {
      const region = await boardRegion();
      const pressed = await within(region).findByRole("article", { name: "CORE-1 Parent" });
      act(() => pressed.focus());
      await user.keyboard("{Shift>}L{/Shift}");
      await waitFor(() => expect(statusChanges).toHaveBeenCalledTimes(1));
      const bravo = within(region).getByRole("article", { name: "CORE-2 Bravo" });
      if (reader === "moved on") {
        await user.keyboard("j");
        expect(bravo).toHaveFocus();
      }
      if (reader === "put the focus on the card's own link") {
        expect(focusTitleLink(pressed, "CORE-1")).toHaveFocus();
      }
      if (reader === "put the focus on the body") {
        act(() => pressed.blur());
        expect(document.body).toHaveFocus();
      }
      if (reader === "opened another dialog and put the focus on the body") {
        closeAnother = render(<AnotherDialog />).unmount;
        await afterFrameAndTasks();
        const control = screen.getByRole("button", { name: "In another dialog" });
        expect(control).toHaveFocus();
        act(() => control.blur());
        expect(document.body).toHaveFocus();
      }
      await act(async () => {
        dryRun.resolve(fixtureAnswer("dry-run"));
        await settle();
      });
      const inProgress = within(region).getByRole("region", { name: "In progress" });
      await waitFor(() =>
        expect(within(inProgress).getByRole("article", { name: "CORE-1 Parent" })).toBeDefined()
      );
      expect(pressed).not.toBeInTheDocument();
      await afterFrameAndTasks();
      const kept = {
        "moved on": bravo,
        "opened another dialog and put the focus on the body": document.body,
        "put the focus on the body": document.body,
        "put the focus on the card's own link": within(inProgress).getByRole("article", {
          name: "CORE-1 Parent",
        }),
        stayed: within(inProgress).getByRole("article", { name: "CORE-1 Parent" }),
      };
      expect(kept[reader]).toHaveFocus();
    } finally {
      closeAnother?.();
      patchIssue.mockRestore();
      statusChanges.mockRestore();
    }
  });
}

// The rule's question takes the focus as it opens and gives it back as it closes
// (`useDialog`). A keyboard move whose card a choice of the question places takes the focus to
// that card only where the question took the focus from the card the press moved, or from a
// control inside it, as a Tab onto the card's own link leaves it while the move's dry run is out.
// A reader who puts the focus on the body while the move's dry run is out has the question open
// on the body: once the choice has placed the card and the question has closed, the focus stays
// on the body.
for (const opener of ["the pressed card", "the pressed card's own link", "the body"] as const) {
  test(`board: asking - a choice that places a keyboard move's card takes the focus to it only where the question took the focus from that card or a control inside it (the question opened on ${opener})`, async () => {
    const user = userEvent.setup();
    const refusal = Promise.withResolvers<void>();
    const statusChanges = answerTheQuestion(async () => fixtureAnswer("changed"), refusal.promise);
    const patchIssue = patchInFlight();
    board(parentIssues, "?layout=board");
    try {
      const region = await boardRegion();
      const pressed = await within(region).findByRole("article", { name: "CORE-1 Parent" });
      act(() => pressed.focus());
      await user.keyboard("{Shift>}L{/Shift}");
      await waitFor(() => expect(statusChanges).toHaveBeenCalledTimes(1));
      if (opener === "the body") {
        act(() => pressed.blur());
        expect(document.body).toHaveFocus();
      }
      if (opener === "the pressed card's own link") {
        expect(focusTitleLink(pressed, "CORE-1")).toHaveFocus();
      }
      refusal.resolve();
      const question = await screen.findByRole("dialog", { name: "Move CORE-1 to In progress?" });
      const moveThem = within(question).getByRole("button", { name: "Move them with it" });
      await waitFor(() => expect(moveThem).toBeEnabled());
      await user.click(moveThem);
      const inProgress = within(region).getByRole("region", { name: "In progress" });
      await waitFor(() =>
        expect(within(inProgress).getByRole("article", { name: "CORE-1 Parent" })).toBeDefined()
      );
      await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
      await afterFrameAndTasks();
      expect(
        opener === "the body"
          ? document.body
          : within(inProgress).getByRole("article", { name: "CORE-1 Parent" })
      ).toHaveFocus();
    } finally {
      patchIssue.mockRestore();
      statusChanges.mockRestore();
    }
  });
}

test("board: checking - the dry run answers 200, and the move goes as today", async () => {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const queryKey = ["issues", "project", "CORE"] as const;
  queryClient.setQueryData(queryKey, parentIssues);
  const statusChanges = answerStatusChanges().mockResolvedValue(fixtureAnswer("dry-run"));
  const patchIssue = answerIssuePatches().mockResolvedValue(saved("CORE-1"));
  const hook = moveHook(queryClient);
  try {
    await act(() =>
      hook.result.current.move(
        "CORE-1",
        move({ rank: { after: "CORE-3" }, status: "in_progress" }, "c\u0000")
      )
    );
    expect(statusChanges).toHaveBeenCalledWith(
      "CORE-1",
      { dry_run: true, status: "in_progress" },
      keyedAttempt
    );
    expect(patchIssue).toHaveBeenCalledWith(
      "CORE-1",
      { rank: { after: "CORE-3" }, status: "in_progress" },
      keyedAttempt
    );
    expect(statusChanges.mock.invocationCallOrder[0]).toBeLessThan(
      patchIssue.mock.invocationCallOrder[0] ?? 0
    );
    expect(hook.result.current.question).toBeUndefined();
  } finally {
    hook.unmount();
    patchIssue.mockRestore();
    statusChanges.mockRestore();
  }
});

// The Board may change while the dry run is out, so the move is planned again once it answers. A
// plan the Board's rules then refuse - the Board went to Legion phase meanwhile - says why
// (`onRefused`), and the move places and writes nothing.
test("board: checking - a move the Board's rules refuse once the dry run answers says why, and places and writes nothing", async () => {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const queryKey = ["issues", "project", "CORE"] as const;
  queryClient.setQueryData(queryKey, parentIssues);
  const statusChanges = answerStatusChanges().mockResolvedValue(fixtureAnswer("dry-run"));
  const patchIssue = answerIssuePatches().mockResolvedValue(saved("CORE-1"));
  const hook = moveHook(queryClient);
  const placed = jest.fn();
  const refused = jest.fn();
  // The plan as the move starts, then the plan once the dry run answers.
  const plans = [
    move({ rank: { after: "CORE-3" }, status: "in_progress" }, "c\u0000"),
    { refused: DAEMON_SETS_PHASE },
  ];
  try {
    await act(() => hook.result.current.move("CORE-1", () => plans.shift(), placed, refused));
    expect(statusChanges).toHaveBeenCalledTimes(1);
    expect(refused.mock.calls).toEqual([[DAEMON_SETS_PHASE]]);
    expect(placed).not.toHaveBeenCalled();
    expect(patchIssue).not.toHaveBeenCalled();
    expect(queryClient.getQueryData<IssueSummary[]>(queryKey)).toEqual(parentIssues);
    expect(hook.result.current.error).toBeUndefined();
  } finally {
    hook.unmount();
    patchIssue.mockRestore();
    statusChanges.mockRestore();
  }
});

test("board: writing - a choice that lands sends the status change, then the card's rank PATCH, and the card moves", async () => {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const queryKey = ["issues", "project", "CORE"] as const;
  queryClient.setQueryData(queryKey, parentIssues);
  const statusChanges = answerTheQuestion(async () => fixtureAnswer("changed"));
  const patchIssue = answerIssuePatches().mockResolvedValue(
    saved("CORE-1", { status: "in_progress" })
  );
  const hook = moveHook(queryClient);
  try {
    let moved: Promise<void> = Promise.resolve();
    await act(async () => {
      moved = hook.result.current.move(
        "CORE-1",
        move({ rank: { after: "CORE-3" }, status: "in_progress" }, "c\u0000")
      );
      await settle();
    });
    // Asking: the card has not moved, and nothing is written.
    expect(hook.result.current.question?.issues.map((entry) => entry.key)).toEqual(["ACME-5"]);
    expect(
      queryClient.getQueryData<IssueSummary[]>(queryKey)?.find((entry) => entry.key === "CORE-1")
        ?.status
    ).toBe("todo");
    expect(patchIssue).not.toHaveBeenCalled();

    await act(async () => {
      hook.result.current.question?.choose("include_children");
      await moved;
    });
    expect(statusChanges.mock.calls.map(([, input]) => statusChangeOf(input))).toContain(
      "write with include_children"
    );
    // The status is the status change's; the PATCH carries the card's place alone.
    expect(patchIssue).toHaveBeenCalledTimes(1);
    expect(patchIssue).toHaveBeenCalledWith("CORE-1", { rank: { after: "CORE-3" } }, keyedAttempt);
    expect(statusChanges.mock.invocationCallOrder.at(-1)).toBeLessThan(
      patchIssue.mock.invocationCallOrder[0] ?? 0
    );
    expect(
      queryClient.getQueryData<IssueSummary[]>(queryKey)?.find((entry) => entry.key === "CORE-1")
        ?.status
    ).toBe("in_progress");
    expect(hook.result.current.question).toBeUndefined();
    expect(hook.result.current.error).toBeUndefined();
  } finally {
    hook.unmount();
    patchIssue.mockRestore();
    statusChanges.mockRestore();
  }
});

// INFRA-3221 (`b007275733`) dropped the board's own words for a stale rank: a move writes one
// neighbour, which no stale Board can leave unordered, and any refusal says the server's own.
test("board: writing - the rank PATCH refused leaves the status standing, says the server's reason and refetches", async () => {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const queryKey = ["issues", "project", "CORE"] as const;
  queryClient.setQueryData(queryKey, parentIssues);
  const statusChanges = answerTheQuestion(async () => fixtureAnswer("changed"));
  const patchIssue = answerIssuePatches().mockRejectedValue(
    new ApiError(400, { code: "RANK_INPUT", error: "rank neighbors are not ordered" })
  );
  const hook = moveHook(queryClient);
  try {
    let moved: Promise<void> = Promise.resolve();
    await act(async () => {
      moved = hook.result.current.move(
        "CORE-1",
        move({ rank: { after: "CORE-3" }, status: "in_progress" }, "c\u0000")
      );
      await settle();
    });
    await act(async () => {
      hook.result.current.question?.choose("include_children");
      await moved;
    });
    expect(patchIssue).toHaveBeenCalledWith("CORE-1", { rank: { after: "CORE-3" } }, keyedAttempt);
    expect(hook.result.current.error).toBe("rank neighbors are not ordered");
    expect(queryClient.getQueryState(queryKey)?.isInvalidated).toBe(true);
    // The status change that landed is the only one: nothing puts the status back.
    expect(
      statusChanges.mock.calls.filter(([, input]) => input.dry_run !== true).map(([key]) => key)
    ).toEqual(["CORE-1"]);
  } finally {
    hook.unmount();
    patchIssue.mockRestore();
    statusChanges.mockRestore();
  }
});

test("board: a PATCH of a card with no parent and no open child, refused by the rule, asks", async () => {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const queryKey = ["issues", "project", "CORE"] as const;
  queryClient.setQueryData(queryKey, parentIssues);
  const statusChanges = answerTheQuestion(() => new Promise<StatusChangeAnswer>(() => {}));
  const patchIssue = answerIssuePatches().mockRejectedValue(fixtureRefusal("conflict"));
  const hook = moveHook(queryClient);
  try {
    await act(async () => {
      void hook.result.current.move("CORE-2", move({ status: "done" }));
      await settle();
    });
    await waitFor(() => expect(hook.result.current.question).toBeDefined());
    expect(patchIssue).toHaveBeenCalledTimes(1);
    // The card went back, and the question says why rather than the board's line.
    expect(
      queryClient.getQueryData<IssueSummary[]>(queryKey)?.find((entry) => entry.key === "CORE-2")
        ?.status
    ).toBe("todo");
    expect(hook.result.current.error).toBeUndefined();
    expect(statusChanges.mock.calls.map(([key, input]) => [key, statusChangeOf(input)])).toEqual([
      ["CORE-2", "dry run with include_children"],
      ["CORE-2", "dry run with another option"],
    ]);
  } finally {
    hook.unmount();
    patchIssue.mockRestore();
    statusChanges.mockRestore();
  }
});

/** How many board moves of CORE are pending: each holds every read of the project's issues
 *  (`boardMovesSettled`). */
const boardMovesPending = (queryClient: QueryClient) =>
  queryClient.isMutating({ mutationKey: boardMoveMutationKey("CORE") });

// The browser's Back while the board's question is open takes the board away. The move ends as
// Keep as it is would end it, so its turn of the board's scope ends: the project's lists are read
// again rather than wait on a question nobody can see, and nothing is written.
test("board: asking - the board going away ends the move, leaves no board move pending and writes nothing", async () => {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const queryKey = ["issues", "project", "CORE"] as const;
  queryClient.setQueryData(queryKey, parentIssues);
  const statusChanges = answerTheQuestion(() => new Promise<StatusChangeAnswer>(() => {}));
  const patchIssue = answerIssuePatches().mockResolvedValue(saved("CORE-1"));
  const hook = moveHook(queryClient);
  try {
    let moved: Promise<void> = Promise.resolve();
    await act(async () => {
      moved = hook.result.current.move(
        "CORE-1",
        move({ rank: { after: "CORE-3" }, status: "in_progress" }, "c\u0000")
      );
      await settle();
    });
    expect(hook.result.current.question).toBeDefined();
    expect(boardMovesPending(queryClient)).toBe(1);

    hook.unmount();
    await settle();
    expect(boardMovesPending(queryClient)).toBe(0);
    await moved;
    expect(patchIssue).not.toHaveBeenCalled();
    expect(statusChanges.mock.calls.filter(([, input]) => input.dry_run !== true)).toEqual([]);
    expect(
      queryClient.getQueryData<IssueSummary[]>(queryKey)?.find((entry) => entry.key === "CORE-1")
        ?.status
    ).toBe("todo");
  } finally {
    hook.unmount();
    patchIssue.mockRestore();
    statusChanges.mockRestore();
  }
});

// A listing read already on the wire when a card's move asks its question does not wait for the
// move, so it can fail while the question is open. The Board then says it could not load the
// issues, over the cards it holds, and the question stays: it belongs to the Board's move, not to
// the listing, and its answer ends the move that every later read of the listing waits for.
test("board: asking - a list read that fails while the question is open leaves the question drawn, and its answer ends the move", async () => {
  const user = userEvent.setup();
  const statusChanges = answerTheQuestion(() => new Promise<StatusChangeAnswer>(() => {}));
  const patchIssue = patchInFlight();
  const { client, listing } = board(parentIssues, "?layout=board");
  try {
    const region = await boardRegion();
    await within(region).findByRole("article", { name: "CORE-1 Parent" });
    const inFlight = Promise.withResolvers<IssueSummary[]>();
    const reads = listing.mock.calls.length;
    listing.mockImplementationOnce(() => inFlight.promise);
    await act(async () => {
      void client.invalidateQueries({ queryKey: ["issues"] });
    });
    await waitFor(() => expect(listing.mock.calls.length).toBeGreaterThan(reads));
    act(() => within(region).getByRole("article", { name: "CORE-1 Parent" }).focus());
    await user.keyboard("{Shift>}L{/Shift}");
    const question = await screen.findByRole("dialog", { name: "Move CORE-1 to In progress?" });
    expect(boardMovesPending(client)).toBe(1);

    await act(async () => {
      inFlight.reject(new ApiError(500, { code: "INTERNAL", error: "the list failed" }));
    });
    await screen.findByText("Could not load issues.");
    expect(screen.getByRole("dialog", { name: "Move CORE-1 to In progress?" })).toBe(question);

    await user.click(within(question).getByRole("button", { name: "Keep as it is" }));
    await waitFor(() => expect(boardMovesPending(client)).toBe(0));
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(patchIssue).not.toHaveBeenCalled();
    expect(statusChanges.mock.calls.filter(([, input]) => input.dry_run !== true)).toEqual([]);
  } finally {
    patchIssue.mockRestore();
    statusChanges.mockRestore();
  }
});

// The board goes while the move's dry run is out. Where the rule asks nothing, the drop needs no
// answer of the person's, so its PATCH still goes, as a card with no parent is saved at once, and
// the board's move settles once it lands.
test("board: checking - a dry run answered 200 once the board has gone still sends the PATCH, once, and the board's move settles", async () => {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const queryKey = ["issues", "project", "CORE"] as const;
  queryClient.setQueryData(queryKey, parentIssues);
  const dryRun = Promise.withResolvers<StatusChangeAnswer>();
  const statusChanges = answerStatusChanges().mockImplementation(() => dryRun.promise);
  const patchIssue = answerIssuePatches().mockResolvedValue(saved("CORE-1"));
  const hook = moveHook(queryClient);
  try {
    let moved: Promise<void> = Promise.resolve();
    await act(async () => {
      moved = hook.result.current.move(
        "CORE-1",
        move({ rank: { after: "CORE-3" }, status: "in_progress" }, "c\u0000")
      );
      await settle();
    });
    expect(boardMovesPending(queryClient)).toBe(1);

    hook.unmount();
    dryRun.resolve(fixtureAnswer("dry-run"));
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(1));
    await moved;
    expect(patchIssue).toHaveBeenCalledWith(
      "CORE-1",
      { rank: { after: "CORE-3" }, status: "in_progress" },
      keyedAttempt
    );
    expect(statusChanges).toHaveBeenCalledTimes(1);
    expect(boardMovesPending(queryClient)).toBe(0);
  } finally {
    hook.unmount();
    patchIssue.mockRestore();
    statusChanges.mockRestore();
  }
});

// Where the rule would refuse the drop, only the person could say what to do, and they have gone
// with the board: the question never opens, nothing is written, and the board's turn ends.
test("board: checking - a dry run answered 409 once the board has gone ends the move, leaves no board move pending and writes nothing", async () => {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const queryKey = ["issues", "project", "CORE"] as const;
  queryClient.setQueryData(queryKey, parentIssues);
  const dryRun = Promise.withResolvers<StatusChangeAnswer>();
  const statusChanges = answerStatusChanges().mockImplementation(() => dryRun.promise);
  const patchIssue = answerIssuePatches().mockResolvedValue(saved("CORE-1"));
  const hook = moveHook(queryClient);
  try {
    let moved: Promise<void> = Promise.resolve();
    await act(async () => {
      moved = hook.result.current.move(
        "CORE-1",
        move({ rank: { after: "CORE-3" }, status: "in_progress" }, "c\u0000")
      );
      await settle();
    });
    expect(boardMovesPending(queryClient)).toBe(1);

    hook.unmount();
    dryRun.reject(fixtureRefusal("conflict"));
    await settle();
    expect(boardMovesPending(queryClient)).toBe(0);
    await moved;
    expect(statusChanges).toHaveBeenCalledTimes(1);
    expect(patchIssue).not.toHaveBeenCalled();
  } finally {
    hook.unmount();
    patchIssue.mockRestore();
    statusChanges.mockRestore();
  }
});

// A keyboard move of a card with a parent or an open child places nothing before its dry run
// answers, so the Board says the move once the card lands, with its place in its cell then, in
// the words of every other keyboard move. On a Board in swimlanes the cell is the card's lane.
for (const [layout, lanes] of [
  ["the Board", false],
  ["a Board in swimlanes", true],
] as const) {
  test(`Shift+L on ${layout} moves a card with an open child after its dry run, and announces the move once the card lands`, async () => {
    const user = userEvent.setup();
    const dryRun = Promise.withResolvers<StatusChangeAnswer>();
    const statusChanges = answerStatusChanges().mockImplementation(() => dryRun.promise);
    const patchIssue = patchInFlight();
    if (lanes)
      localStorage.setItem(userPreferenceStorageKey("alice", "project.board-lanes"), "shown");
    board(parentIssues, "?layout=board");
    try {
      const region = await boardRegion();
      const status = within(region).getByRole("status", { name: "Board announcements" });
      act(() => within(region).getByRole("article", { name: "CORE-1 Parent" }).focus());
      await user.keyboard("{Shift>}L{/Shift}");
      // Checking: the card has not moved, so nothing is said of it yet.
      expect(statusChanges).toHaveBeenCalledTimes(1);
      expect(patchIssue).not.toHaveBeenCalled();
      expect(status.textContent).toBe("");

      await act(async () => {
        dryRun.resolve(fixtureAnswer("dry-run"));
        await settle();
      });
      expect(patchIssue).toHaveBeenCalledWith(
        "CORE-1",
        { rank: { before: "CORE-3" }, status: "in_progress" },
        keyedAttempt
      );
      expect(status.textContent).toBe("CORE-1 → In progress, position 1 of 2");
    } finally {
      patchIssue.mockRestore();
      statusChanges.mockRestore();
    }
  });
}

// A keyboard move the rule's dry run checks first is planned again once the dry run answers,
// from the Board as it stands then. Another writer's change can reach the listing while the dry
// run is out and leave the move nothing to do: the card has left the listing, or it already
// stands where the move puts it, first in In progress. Either takes the pressed card's node off
// the page. The move then places nothing: no PATCH goes, the live region says no place, and the
// focus stays where the change left it.
for (const change of ["the card left the listing", "the card stands where it moves to"] as const) {
  test(`Shift+L of a card with an open child whose dry run answers once ${change} says no place and takes no focus`, async () => {
    const user = userEvent.setup();
    const dryRun = Promise.withResolvers<StatusChangeAnswer>();
    const statusChanges = answerStatusChanges().mockImplementation(() => dryRun.promise);
    const patchIssue = patchInFlight();
    const rows = [...parentIssues];
    const { client } = board(rows, "?layout=board");
    try {
      const region = await boardRegion();
      const pressed = await within(region).findByRole("article", { name: "CORE-1 Parent" });
      act(() => pressed.focus());
      await user.keyboard("{Shift>}L{/Shift}");
      await waitFor(() => expect(statusChanges).toHaveBeenCalledTimes(1));

      // The change reaches the listing the Board draws, and every read after it.
      const edit =
        change === "the card left the listing"
          ? (list: IssueSummary[]): IssueSummary[] => list.filter((row) => row.key !== "CORE-1")
          : (list: IssueSummary[]): IssueSummary[] =>
              list.map((row) => (row.key === "CORE-1" ? { ...row, status: "in_progress" } : row));
      rows.splice(0, rows.length, ...edit(rows));
      act(() => {
        client.setQueriesData<unknown>(
          { queryKey: ["issues", "project", "CORE"] },
          (data: unknown) => (Array.isArray(data) ? edit(data) : data)
        );
      });
      await waitFor(() => expect(pressed).not.toBeInTheDocument());
      if (change === "the card stands where it moves to") {
        const inProgress = within(region).getByRole("region", { name: "In progress" });
        expect(cardKeys(inProgress)).toEqual(["CORE-1", "CORE-3"]);
      }
      const stood = document.activeElement;

      await act(async () => {
        dryRun.resolve(fixtureAnswer("dry-run"));
        await settle();
      });
      await afterFrameAndTasks();
      expect(liveRegion().textContent).toBe("");
      expect(patchIssue).not.toHaveBeenCalled();
      expect(document.activeElement).toBe(stood);
    } finally {
      patchIssue.mockRestore();
      statusChanges.mockRestore();
    }
  });
}

// A keyboard move of a card with a parent or an open child places nothing before its dry run, so
// focus follows the card only once the move places it. A move that ends without placing it takes no
// focus later: the next change of the Board's columns leaves focus where the reader put it.
test("Shift+L of a card with an open child, its question cancelled, leaves focus where the reader puts it when the board next changes", async () => {
  const user = userEvent.setup();
  const statusChanges = answerTheQuestion(() => new Promise<StatusChangeAnswer>(() => {}));
  const patchIssue = patchInFlight();
  const rows = [...parentIssues];
  const { client } = board(rows, "?layout=board");
  try {
    const region = await boardRegion();
    act(() => within(region).getByRole("article", { name: "CORE-1 Parent" }).focus());
    await user.keyboard("{Shift>}L{/Shift}");
    const question = await screen.findByRole("dialog", { name: "Move CORE-1 to In progress?" });
    await user.click(within(question).getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    // The reader goes on to another card.
    const bravo = within(region).getByRole("article", { name: "CORE-2 Bravo" });
    act(() => bravo.focus());

    // Another writer's change, read again, moves Charlie into Todo.
    rows[2] = { ...(rows[2] as IssueSummary), status: "todo" };
    await act(async () => {
      await client.invalidateQueries({ queryKey: ["issues"] });
    });
    await waitFor(() =>
      expect(cardKeys(within(region).getByRole("region", { name: "Todo" }))).toContain("CORE-3")
    );
    expect(within(region).getByRole("article", { name: "CORE-2 Bravo" })).toHaveFocus();
    expect(patchIssue).not.toHaveBeenCalled();
  } finally {
    patchIssue.mockRestore();
    statusChanges.mockRestore();
  }
});

/** CORE-1 with its open child CORE-2 in work under it, both in Todo. */
const childIssues: IssueSummary[] = [
  parentCard,
  issue("CORE-2", { parent: "CORE-1", rank: "b", status: "todo", title: "Child" }),
];

// A keyboard move whose question's Move them with it lands moves the card's open child with it.
// The Board says the card's place once the card lands, and the child is in the cell by then: the
// count holds it, and the card's rank PATCH names it as the card's neighbour there.
test("Shift+H of a card with an open child, its question's Move them with it landed, announces the count with the child in the cell", async () => {
  const user = userEvent.setup();
  const statusChanges = answerTheQuestion(async () => ({
    changed: [
      { key: "CORE-2", parent: "CORE-1", previous_last_seq: 1, status: "backlog" },
      { key: "CORE-1", parent: null, previous_last_seq: 1, status: "backlog" },
    ],
    issue: { ...saved("CORE-1"), status: "backlog" },
  }));
  const patchIssue = patchInFlight();
  board(childIssues, "?layout=board");
  try {
    const region = await boardRegion();
    const status = within(region).getByRole("status", { name: "Board announcements" });
    act(() => within(region).getByRole("article", { name: "CORE-1 Parent" }).focus());
    await user.keyboard("{Shift>}H{/Shift}");
    const question = await screen.findByRole("dialog", { name: "Move CORE-1 to Backlog?" });
    const moveThem = within(question).getByRole("button", { name: "Move them with it" });
    await waitFor(() => expect(moveThem).toBeEnabled());
    // Asking: the card has not moved, so nothing is said of it yet.
    expect(status.textContent).toBe("");

    await user.click(moveThem);
    await waitFor(() => expect(status.textContent).toBe("CORE-1 → Backlog, position 1 of 2"));
    expect(patchIssue).toHaveBeenCalledWith("CORE-1", { rank: { before: "CORE-2" } }, keyedAttempt);
    expect(
      within(within(region).getByRole("region", { name: "Backlog" }))
        .getAllByRole("article")
        .map((node) => node.textContent)
    ).toEqual([expect.stringContaining("Parent"), expect.stringContaining("Child")]);
  } finally {
    patchIssue.mockRestore();
    statusChanges.mockRestore();
  }
});
