import { afterEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { api } from "../../api/client";
import type { IssueSummary, Priorities, Priority, PriorityRow } from "../../api/types";
import { PriorityPage } from "./PriorityPage";

function priority(id: string, overrides: Partial<PriorityRow> = {}): PriorityRow {
  return {
    id,
    title: `Priority ${id}`,
    description: "",
    owner: "BET and LINUX controllers",
    rank: id,
    state: "live",
    made_at: "2026-11-10T09:00:00Z",
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

const ROWS: PriorityRow[] = [
  priority("legion-first", { title: "Legion works reliably first", owner: "INFRA controller" }),
  priority("rocq-lean", {
    title: "Rocq and Lean speed work restarted",
    description: "# Rocq and Lean speed work restarted\n\nSpeedruns run again.",
    work: {
      by_status: { in_progress: 2, testing: 1, todo: 11, backlog: 4, icebox: 1 },
      claimed: 1,
      closed_last_7_days: 9,
      newest_event_at: "2026-11-11T08:00:00Z",
      open_asks: 0,
    },
  }),
  priority("runbook", {
    title: "The fleet has a runbook",
    description: "Read [the runbook](https://example.com/runbook) before a roll-out.",
  }),
];

const BODY: Priorities = {
  priorities: ROWS,
  work_under_no_priority: { by_project: null, count: 0, issues: null },
  priority_work_parked: { by_priority: null, count: 0, issues: null },
};

const saved = (id: string): Priority => ({
  id,
  title: "Rocq and Lean speed work restarted",
  description: "",
  owner: "",
  rank: "n",
  state: "live",
  made_at: "2026-11-10T09:00:00Z",
  made_by: { kind: "user", id: "alice" },
});

const restores: (() => void)[] = [];
afterEach(() => {
  for (const restore of restores.splice(0)) restore();
  localStorage.clear();
});

function page(id = "rocq-lean", issues: IssueSummary[] = [], writer = true) {
  const priorities = spyOn(api, "listPriorities").mockResolvedValue(BODY);
  const listing = spyOn(api, "listIssues").mockImplementation(async (options = {}) =>
    issues.filter(
      (row) =>
        options.advances === undefined ||
        (row.advances ?? []).some((relation) => options.advances?.includes(relation.id))
    )
  );
  const state = spyOn(api, "getMyState").mockResolvedValue({});
  const agents = spyOn(api, "listAgents").mockResolvedValue([]);
  // A reader who may not write a priority is any principal that is not a person
  // (`useIsHuman`): the contract types only the person's answer, so the other is cast.
  const whoami = writer
    ? { event_head: "0", kind: "user" as const, login: "alice" }
    : ({ event_head: "0", kind: "token", login: "ci" } as never);
  const viewer = spyOn(api, "whoAmI").mockResolvedValue(whoami);
  const patch = spyOn(api, "patchPriority").mockImplementation(async (key) => saved(key));
  // The margin reads the priority's own history as soon as it opens on Everything.
  const history = spyOn(api, "getPriorityHistory").mockResolvedValue([]);
  const descriptor = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "clientHeight");
  Object.defineProperty(HTMLElement.prototype, "clientHeight", {
    configurable: true,
    get() {
      return this.dataset.testid?.endsWith("-scroll") ? 600 : (descriptor?.get?.call(this) ?? 0);
    },
  });
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(["whoami"], whoami);
  const view = render(
    <MemoryRouter initialEntries={[`/priorities/${id}`]}>
      <QueryClientProvider client={client}>
        <Routes>
          <Route element={<PriorityPage />} path="/priorities/:id" />
        </Routes>
      </QueryClientProvider>
    </MemoryRouter>
  );
  restores.push(() => {
    view.unmount();
    client.clear();
    for (const spy of [priorities, listing, state, agents, viewer, patch, history])
      spy.mockRestore();
    if (descriptor) Object.defineProperty(HTMLElement.prototype, "clientHeight", descriptor);
    else Reflect.deleteProperty(HTMLElement.prototype, "clientHeight");
  });
  return { listing, patch, view };
}

test("the header carries the order, the title in place, the owner and the three counts", async () => {
  page();
  const title = await screen.findByRole("textbox", { name: "Title" });
  expect((title as HTMLInputElement).value).toBe("Rocq and Lean speed work restarted");
  expect(screen.getByText("P2")).toBeDefined();
  // The owner is the picker the page is drawn as: it offers the owners the deployment already
  // uses and takes one nobody has written yet, so it is a combobox and not a plain field.
  expect(screen.getByRole("combobox", { name: "Owner" })).toBeDefined();
  expect(screen.getByText("Order P2, dragged on the priorities page")).toBeDefined();
  expect(screen.getByText(/In flight 3 · waiting 11 · in backlog or icebox 5/)).toBeDefined();
  expect(screen.getByRole("link", { name: "‹ Priorities" }).getAttribute("href")).toBe(
    "/priorities"
  );
});

test("the description is a disclosure that closes, and the work stays below it", async () => {
  const user = userEvent.setup();
  page("rocq-lean", [issue("CORE-1", ["rocq-lean"])]);
  const described = await screen.findByRole("group", { name: "Description of P2" });
  expect(await within(described).findByText("Speedruns run again.")).toBeDefined();
  await user.click(screen.getByRole("button", { name: "Description" }));
  await waitFor(() =>
    expect(screen.queryByRole("group", { name: "Description of P2" })).toBeNull()
  );
  expect(screen.getByRole("button", { name: "Description, closed" })).toBeDefined();
  // The work fills the page with the description closed.
  expect(
    await screen.findByRole("row", { name: "CORE-1 Work CORE-1" }, { timeout: 3000 })
  ).toBeDefined();
});

test("the description reads rendered, opens on a click on its text and is saved as it is written, with no Save button", async () => {
  const user = userEvent.setup();
  const { patch } = page();
  const described = await screen.findByRole("group", { name: "Description of P2" });
  // Read as the markdown it is, not as its source: the heading is a heading.
  expect(
    await within(described).findByRole("heading", { name: "Rocq and Lean speed work restarted" })
  ).toBeDefined();
  expect(screen.queryByRole("textbox", { name: "Description of P2" })).toBeNull();
  await user.click(within(described).getByText("Speedruns run again."));
  const field = await screen.findByRole("textbox", { name: "Description of P2" });
  expect(document.activeElement).toBe(field);
  await user.type(field, " More.");
  await waitFor(() => expect(patch).toHaveBeenCalledTimes(1), { timeout: 3000 });
  expect(patch.mock.calls[0]?.[0]).toBe("rocq-lean");
  expect(patch.mock.calls[0]?.[1]).toEqual({
    description: "# Rocq and Lean speed work restarted\n\nSpeedruns run again. More.",
  });
  expect(screen.queryByRole("button", { name: /save/i })).toBeNull();
  // Leaving the field gives the rendered block back, with what was written in it.
  await user.tab();
  const back = await screen.findByRole("group", { name: "Description of P2" });
  expect(await within(back).findByText("Speedruns run again. More.")).toBeDefined();
  expect(screen.queryByRole("textbox", { name: "Description of P2" })).toBeNull();
  expect(patch).toHaveBeenCalledTimes(1);
});

test("the rendered description is no control: the keyboard opens the editor with a button named by its own words", async () => {
  const user = userEvent.setup();
  page();
  const described = await screen.findByRole("group", { name: "Description of P2" });
  // The one control in the block is the button, and it says what it is called (WCAG 2.5.3).
  const edit = within(described).getByRole("button");
  expect(edit).toBe(screen.getByRole("button", { name: "Edit the description of P2" }));
  expect(edit.textContent).toBe("Edit the description of P2");
  expect(edit.getAttribute("aria-label")).toBeNull();
  edit.focus();
  await user.keyboard("{Enter}");
  expect(await screen.findByRole("textbox", { name: "Description of P2" })).toBeDefined();
});

test("a link in the description is drawn as a link, and a click on it is followed, not edited", async () => {
  const user = userEvent.setup();
  page("runbook");
  const described = await screen.findByRole("group", { name: "Description of P3" });
  const link = await within(described).findByRole("link", { name: "the runbook" });
  expect(link.getAttribute("href")).toBe("https://example.com/runbook");
  // The test document follows no link; the press itself is what reaches the block.
  link.addEventListener("click", (event) => event.preventDefault());
  await user.click(link);
  expect(screen.queryByRole("textbox", { name: "Description of P3" })).toBeNull();
  expect(screen.getByRole("group", { name: "Description of P3" })).toBeDefined();
  // The words around it are still the way in.
  await user.click(within(described).getByText(/before a roll-out/));
  expect(await screen.findByRole("textbox", { name: "Description of P3" })).toBeDefined();
});

test("a reader who may not write reads the description rendered, with no control over it", async () => {
  page("rocq-lean", [], false);
  expect(await screen.findByText("Speedruns run again.")).toBeDefined();
  expect(screen.queryByRole("group", { name: "Description of P2" })).toBeNull();
  expect(screen.queryByRole("button", { name: "Edit the description of P2" })).toBeNull();
  expect(screen.queryByRole("textbox", { name: "Description of P2" })).toBeNull();
});

test("an empty description says none is written to a reader who may not write it", async () => {
  page("legion-first", [], false);
  expect(await screen.findByText("No description is written.")).toBeDefined();
  expect(screen.queryByRole("button", { name: "Edit the description of P1" })).toBeNull();
});

test("an empty description is, for a writer, the prompt that opens the editor", async () => {
  const user = userEvent.setup();
  page("legion-first");
  const described = await screen.findByRole("group", { name: "Description of P1" });
  const prompt = within(described).getByText(/No description is written\./);
  expect(prompt.textContent).toBe("No description is written. Click here to write one.");
  await user.click(prompt);
  expect(await screen.findByRole("textbox", { name: "Description of P1" })).toBeDefined();
});

test("the owner is a control beside the title and writes the owner", async () => {
  const user = userEvent.setup();
  const { patch } = page();
  const owner = await screen.findByRole("combobox", { name: "Owner" });
  await user.clear(owner);
  await user.type(owner, "LINUX controller");
  await user.tab();
  await waitFor(() => expect(patch).toHaveBeenCalledTimes(1));
  expect(patch.mock.calls[0]?.[1]).toEqual({ owner: "LINUX controller" });
});

test("the page carries no tab strip, no version control and no delete", async () => {
  page();
  await screen.findByRole("textbox", { name: "Title" });
  expect(screen.queryByRole("tablist")).toBeNull();
  expect(screen.queryAllByRole("tab")).toEqual([]);
  expect(screen.queryByRole("button", { name: /delete|retire|remove/i })).toBeNull();
  expect(screen.queryByRole("button", { name: /version|history of this|restore/i })).toBeNull();
  const text = document.body.textContent ?? "";
  expect(/\bv\d+\b|Versions|Comments on this description/.test(text)).toBe(false);
});

test("one link out leaves the scope, naming the issues and why", async () => {
  page("rocq-lean", [issue("CORE-1", ["rocq-lean"]), issue("CORE-4", ["rocq-lean"])]);
  const out = await screen.findByRole("link", { name: "CORE’s issues table" });
  expect(out.getAttribute("href")).toBe(
    "/projects/CORE/issues?only=CORE-1%2CCORE-4&onlyLabel=Rocq+and+Lean+speed+work+restarted&nofilter=1"
  );
});

test("the work under the priority is the issues table, scoped and under its own prefix", async () => {
  const { listing } = page("rocq-lean", [issue("CORE-1", ["rocq-lean"]), issue("OPS-2")]);
  const work = await screen.findByRole("region", { name: "Work advancing this priority" });
  expect(
    await within(work).findByRole("row", { name: "CORE-1 Work CORE-1" }, { timeout: 3000 })
  ).toBeDefined();
  expect(within(work).queryByRole("row", { name: "OPS-2 Work OPS-2" })).toBeNull();
  expect(listing).toHaveBeenCalledWith({ advances: ["rocq-lean"], open: true });
});

test("the margin stands beside the page, with no tabs of its own", async () => {
  page();
  const margin = await screen.findByRole("complementary", { name: "Margin" });
  expect(within(margin).getByRole("combobox", { name: /Margin/ })).toBeDefined();
  expect(within(margin).queryAllByRole("tab")).toEqual([]);
});

test("an id no priority carries says so and gives the way back", async () => {
  page("not-written");
  expect(await screen.findByText("No priority of that id is written.")).toBeDefined();
  expect(screen.getByRole("link", { name: "‹ Priorities" })).toBeDefined();
});
