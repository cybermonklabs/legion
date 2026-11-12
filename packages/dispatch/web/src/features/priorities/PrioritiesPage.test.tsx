import { afterEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
// biome-ignore lint/style/noRestrictedImports: the drag of a row onto another below is a drag and a drop, which user-event has no form for
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { api } from "../../api/client";
import type {
  IssueSummary,
  Priorities,
  Priority,
  PriorityIssue,
  PriorityRow,
} from "../../api/types";
import { PrioritiesPage } from "./PrioritiesPage";

function priority(id: string, overrides: Partial<PriorityRow> = {}): PriorityRow {
  return {
    id,
    title: `Priority ${id}`,
    description: "",
    owner: "INFRA controller",
    rank: id,
    state: "live",
    made_at: "2026-11-01T00:00:00Z",
    made_by: { kind: "user", id: "alice" },
    work: {
      by_status: null,
      claimed: 0,
      closed_last_7_days: 0,
      newest_event_at: null,
      open_asks: 0,
    },
    ...overrides,
  };
}

function faultIssue(key: string, overrides: Partial<PriorityIssue> = {}): PriorityIssue {
  return {
    key,
    title: `Work ${key}`,
    project: key.split("-")[0] ?? key,
    status: "in_progress",
    assignee: null,
    claim: null,
    changed_at: "2026-11-10T00:00:00Z",
    priorities: null,
    moved_at: null,
    moved_by: null,
    ...overrides,
  };
}

function body(overrides: Partial<Priorities> = {}): Priorities {
  return {
    priorities: [],
    work_under_no_priority: { by_project: null, count: 0, issues: null },
    priority_work_parked: { by_priority: null, count: 0, issues: null },
    ...overrides,
  };
}

function issue(key: string, advances: string[] = []): IssueSummary {
  return {
    key,
    advances: advances.map((id) => ({ id, inherited: false })),
    advances_revision: 0,
    title: `Work ${key}`,
    status: "in_progress",
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
    closed_at: null,
    created_by: { kind: "user", id: "alice" },
    created_at: "2026-11-01T00:00:00Z",
    updated_at: "2026-11-01T00:00:00Z",
  };
}

const saved = (id: string): Priority => ({
  id,
  title: `Priority ${id}`,
  description: "",
  owner: "",
  rank: "n",
  state: "live",
  made_at: "2026-11-01T00:00:00Z",
  made_by: { kind: "user", id: "alice" },
});

const restores: (() => void)[] = [];
afterEach(() => {
  for (const restore of restores.splice(0)) restore();
  localStorage.clear();
});

/** The page as a signed-in person sees it, with the reads it makes answered from `answer`. */
function page(answer: Priorities, issues: IssueSummary[] = []) {
  const priorities = spyOn(api, "listPriorities").mockResolvedValue(answer);
  const listing = spyOn(api, "listIssues").mockImplementation(async (options = {}) =>
    issues.filter(
      (row) =>
        options.advances === undefined ||
        (row.advances ?? []).some((relation) => options.advances?.includes(relation.id))
    )
  );
  const state = spyOn(api, "getMyState").mockResolvedValue({});
  const agents = spyOn(api, "listAgents").mockResolvedValue([]);
  const viewer = spyOn(api, "whoAmI").mockResolvedValue({
    event_head: "0",
    kind: "user",
    login: "alice",
  });
  const create = spyOn(api, "createPriority").mockResolvedValue(saved("new"));
  const patch = spyOn(api, "patchPriority").mockImplementation(async (id) => saved(id));
  // The unit DOM lays nothing out: give every table's box the height a window would.
  const descriptor = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "clientHeight");
  Object.defineProperty(HTMLElement.prototype, "clientHeight", {
    configurable: true,
    get() {
      return this.dataset.testid?.endsWith("-scroll") ? 600 : (descriptor?.get?.call(this) ?? 0);
    },
  });
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  const view = render(
    <MemoryRouter initialEntries={["/priorities"]}>
      <QueryClientProvider client={client}>
        <PrioritiesPage />
      </QueryClientProvider>
    </MemoryRouter>
  );
  restores.push(() => {
    view.unmount();
    client.clear();
    for (const spy of [priorities, listing, state, agents, viewer, create, patch])
      spy.mockRestore();
    if (descriptor) Object.defineProperty(HTMLElement.prototype, "clientHeight", descriptor);
    else Reflect.deleteProperty(HTMLElement.prototype, "clientHeight");
  });
  return { create, listing, patch, view };
}

const FIFTEEN = body({
  priorities: [
    priority("legion-first", {
      title: "Legion works reliably first",
      description: "The first line.\nThe rest of it, which the row shows when it is open.",
      owner: "INFRA controller",
      work: {
        by_status: { in_progress: 5, testing: 1, needs_review: 1, retro: 0, todo: 9, triage: 3 },
        claimed: 2,
        closed_last_7_days: 22,
        newest_event_at: "2026-11-11T10:00:00Z",
        open_asks: 1,
      },
    }),
    priority("linkage-closure", {
      title: "Linkage closure eval",
      owner: "BET controller",
      work: {
        by_status: { in_progress: 1, backlog: 2, icebox: 1 },
        claimed: 0,
        closed_last_7_days: 2,
        newest_event_at: null,
        open_asks: 0,
      },
    }),
    priority("oss-verify", { title: "OSS Verify", owner: "OSSV controller" }),
  ],
});

test("the page draws the priorities table above both fault lists", async () => {
  page(
    body({
      ...FIFTEEN,
      work_under_no_priority: {
        by_project: { CORE: 1 },
        count: 1,
        issues: [faultIssue("CORE-1")],
      },
      priority_work_parked: {
        by_priority: { "linkage-closure": 1 },
        count: 1,
        issues: [faultIssue("CORE-9", { status: "backlog", priorities: ["linkage-closure"] })],
      },
    })
  );
  const priorities = await screen.findByRole("table", { name: "Priorities table" });
  const headings = screen.getAllByRole("heading").map((heading) => heading.textContent);
  expect(headings).toEqual([
    "Priorities",
    "In flight under no priority",
    "Priority work in backlog or icebox",
  ]);
  const faults = screen.getByRole("table", { name: "In flight under no priority" });
  const parked = screen.getByRole("table", { name: "Priority work in backlog or icebox" });
  // The priorities come first, which is what the page's own name says it is about.
  expect(priorities.compareDocumentPosition(faults) & Node.DOCUMENT_POSITION_FOLLOWING).toBe(
    Node.DOCUMENT_POSITION_FOLLOWING
  );
  expect(faults.compareDocumentPosition(parked) & Node.DOCUMENT_POSITION_FOLLOWING).toBe(
    Node.DOCUMENT_POSITION_FOLLOWING
  );
  // Each fault stands under its rule in plain words, with its count and where it falls.
  expect(screen.getByText(/advances no priority, its own or one it inherits/)).toBeDefined();
  expect(screen.getByText(/stands in the backlog or the icebox/)).toBeDefined();
  expect(screen.getByText(/1 issue · CORE 1/)).toBeDefined();
  expect(within(parked).getByRole("cell", { name: "CORE-9" })).toBeDefined();
});

test("a row counts the work in flight, waiting, parked and closed in the last 7 days", async () => {
  page(FIFTEEN);
  const row = await screen.findByRole("row", { name: "P1 Legion works reliably first" });
  const cells = within(row)
    .getAllByRole("cell")
    .map((cell) => cell.textContent);
  // In flight is in_progress, testing, needs_review and retro; waiting is todo and triage; the
  // third count is backlog and icebox; the fourth is what the read says closed in 7 days.
  expect(cells.slice(3, 7)).toEqual(["7", "12", "0", "22"]);
  // The newest event is read as a time, whose words follow the clock.
  expect(within(row).getByRole("time").getAttribute("datetime")).toBe("2026-11-11T10:00:00Z");
  const second = screen.getByRole("row", { name: "P2 Linkage closure eval" });
  expect(
    within(second)
      .getAllByRole("cell")
      .map((cell) => cell.textContent)
      .slice(3)
  ).toEqual(["1", "0", "3", "2", "Never"]);
});

test("the first line of a description shows on a closed row and the whole of it when it opens", async () => {
  const user = userEvent.setup();
  page(FIFTEEN, [issue("CORE-1", ["legion-first"]), issue("CORE-2")]);
  await screen.findByRole("row", { name: "P1 Legion works reliably first" });
  expect(screen.getByText("The first line.")).toBeDefined();
  await user.click(screen.getByRole("button", { name: "Open P1 Legion works reliably first" }));
  const whole = await screen.findByRole("textbox", { name: "Description of P1" });
  expect((whole as HTMLTextAreaElement).value).toBe(
    "The first line.\nThe rest of it, which the row shows when it is open."
  );
});

test("opening a row lists the work advancing that priority, over every project", async () => {
  const user = userEvent.setup();
  const { listing } = page(FIFTEEN, [issue("CORE-1", ["legion-first"]), issue("OPS-3")]);
  await screen.findByRole("row", { name: "P1 Legion works reliably first" });
  await user.click(screen.getByRole("button", { name: "Open P1 Legion works reliably first" }));
  const work = await screen.findByRole("region", {
    name: "Work advancing Legion works reliably first",
  });
  expect(await within(work).findByRole("row", { name: "CORE-1 Work CORE-1" })).toBeDefined();
  expect(within(work).queryByRole("row", { name: "OPS-3 Work OPS-3" })).toBeNull();
  // The narrowing is the server's, and it names no project: a priority belongs to none.
  expect(listing).toHaveBeenCalledWith({ advances: ["legion-first"], open: true });
});

test("a person who may write adds a priority", async () => {
  const user = userEvent.setup();
  const { create } = page(FIFTEEN);
  await screen.findByRole("row", { name: "P1 Legion works reliably first" });
  await user.click(screen.getByRole("button", { name: "New priority" }));
  await user.type(
    screen.getByRole("textbox", { name: "Title of the new priority" }),
    "Spend ruthlessly"
  );
  await user.click(screen.getByRole("button", { name: "Add priority" }));
  await waitFor(() => expect(create).toHaveBeenCalledTimes(1));
  expect(create.mock.calls[0]?.[0]).toEqual({ title: "Spend ruthlessly" });
});

test("a title is renamed where it is read, with no edit mode and no Save button", async () => {
  const user = userEvent.setup();
  const { patch } = page(FIFTEEN);
  const field = await screen.findByRole("textbox", { name: "Title of P2" });
  await user.clear(field);
  await user.type(field, "Linkage closure, restarted");
  await user.tab();
  await waitFor(() => expect(patch).toHaveBeenCalledTimes(1));
  expect(patch.mock.calls[0]?.slice(0, 2)).toEqual([
    "linkage-closure",
    { title: "Linkage closure, restarted" },
  ]);
  expect(screen.queryByRole("button", { name: /^Save/ })).toBeNull();
});

test("a description is written on the open row", async () => {
  const user = userEvent.setup();
  const { patch } = page(FIFTEEN);
  await screen.findByRole("row", { name: "P3 OSS Verify" });
  await user.click(screen.getByRole("button", { name: "Open P3 OSS Verify" }));
  const field = await screen.findByRole("textbox", { name: "Description of P3" });
  await user.type(field, "What it is.");
  await user.tab();
  await waitFor(() => expect(patch).toHaveBeenCalledTimes(1));
  expect(patch.mock.calls[0]?.slice(0, 2)).toEqual(["oss-verify", { description: "What it is." }]);
});

test("dragging a row onto another writes the place it landed", async () => {
  const { patch } = page(FIFTEEN);
  const third = await screen.findByRole("row", { name: "P3 OSS Verify" });
  const first = screen.getByRole("row", { name: "P1 Legion works reliably first" });
  // user-event has no form for a drag: it drives pointer events, and the order is moved by the
  // HTML drag-and-drop events the handle listens for.
  fireEvent.dragStart(
    within(third).getByRole("button", {
      name: "Move P3 OSS Verify: drag it, or press the up and down arrows",
    })
  );
  // user-event has no form for a drag.
  fireEvent.dragOver(first);
  // user-event has no form for a drop.
  fireEvent.drop(first);
  await waitFor(() => expect(patch).toHaveBeenCalledTimes(1));
  // Dragged up, it lands above the row it was dropped on.
  expect(patch.mock.calls[0]?.slice(0, 2)).toEqual([
    "oss-verify",
    { rank: { before: "legion-first" } },
  ]);
});

test("the keyboard moves a priority one place, as the drag does", async () => {
  const user = userEvent.setup();
  const { patch } = page(FIFTEEN);
  const first = await screen.findByRole("row", { name: "P1 Legion works reliably first" });
  within(first)
    .getByRole("button", {
      name: "Move P1 Legion works reliably first: drag it, or press the up and down arrows",
    })
    .focus();
  await user.keyboard("{ArrowDown}");
  await waitFor(() => expect(patch).toHaveBeenCalledTimes(1));
  expect(patch.mock.calls[0]?.slice(0, 2)).toEqual([
    "legion-first",
    { rank: { after: "linkage-closure" } },
  ]);
});

test("the table's own view is in the address under its prefix", async () => {
  const user = userEvent.setup();
  page(FIFTEEN);
  await screen.findByRole("row", { name: "P1 Legion works reliably first" });
  await user.type(screen.getByRole("searchbox", { name: "Search priorities" }), "linkage");
  await waitFor(() =>
    expect(screen.queryByRole("row", { name: "P1 Legion works reliably first" })).toBeNull()
  );
  expect(screen.getByRole("row", { name: "P2 Linkage closure eval" })).toBeDefined();
});

test("the page carries no estimate, no forecast, no percentage and no written status", async () => {
  page(FIFTEEN);
  await screen.findByRole("row", { name: "P1 Legion works reliably first" });
  const text = document.body.textContent ?? "";
  expect(/\d+%/.test(text)).toBe(false);
  expect(/estimate|forecast|on track|at risk/i.test(text)).toBe(false);
});
