import { afterEach, expect, spyOn, test } from "bun:test";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { answerPriorityPatches } from "../../__tests__/send-answers";
import { cacheOf, QueryClient, QueryClientProvider } from "../../api/cache/test-client";
import { ApiError, api } from "../../api/client";
import type { IssueSummary, Priorities, Priority, PriorityRow } from "../../api/types";
import { PendingWritesIndicator } from "../writes/PendingWritesIndicator";
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

/** The page as a writer reads it, over a Dispatch that keeps what it makes: a patch it answers
 *  changes the rows every later read of the priorities answers with. The shell's pending-writes
 *  indicator is drawn beside it, where a refused or waiting write is said. */
function page(id = "rocq-lean", issues: IssueSummary[] = [], writer = true) {
  let rows = ROWS;
  const priorities = spyOn(api, "listPriorities").mockImplementation(async () => ({
    ...BODY,
    priorities: rows,
  }));
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
  const patch = answerPriorityPatches().mockImplementation(async (patched, input) => {
    rows = rows.map((row) =>
      row.id === patched
        ? {
            ...row,
            description: input.description ?? row.description,
            owner: input.owner ?? row.owner,
            title: input.title ?? row.title,
          }
        : row
    );
    return saved(patched);
  });
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
        <PendingWritesIndicator />
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
  return { client, listing, patch, priorities, view };
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

/** Writes `more` onto the end of P2's description and leaves the editor, which saves it. */
async function writeDescription(user: UserEvent, more: string) {
  const described = await screen.findByRole("group", { name: "Description of P2" });
  await user.click(within(described).getByText("Speedruns run again."));
  await user.type(await screen.findByRole("textbox", { name: "Description of P2" }), more);
  await user.tab();
}

const P2_DESCRIPTION_WRITE = "Description of the priority “Rocq and Lean speed work restarted”";

// A save Dispatch refuses leaves nothing of itself on the page: the block reads as Dispatch holds
// the description, and the refusal is said where every refused write is, in Pending changes.
test("a description save Dispatch refuses shows Dispatch's description again, and the refusal", async () => {
  const user = userEvent.setup();
  const { patch } = page();
  patch.mockRejectedValueOnce(
    new ApiError(400, { code: "PRIORITY_INPUT", error: "the description is not accepted" })
  );
  await writeDescription(user, " Refused.");
  const indicator = await screen.findByTestId("pending-writes");
  expect(
    await within(indicator).findByText(
      `Not saved: ${P2_DESCRIPTION_WRITE} — the description is not accepted`
    )
  ).toBeDefined();
  const back = screen.getByRole("group", { name: "Description of P2" });
  expect(within(back).queryByText(/Refused\./)).toBeNull();
  expect(within(back).getByText("Speedruns run again.")).toBeDefined();
});

// A rename Dispatch has not answered holds the priority's line, so a description written after
// it waits its turn, which is where Pending changes offers Discard. A discarded save is gone.
test("a description save discarded from Pending changes shows Dispatch's description again", async () => {
  const user = userEvent.setup();
  const { patch } = page();
  const rename = Promise.withResolvers<Priority>();
  patch.mockReturnValueOnce(rename.promise);
  const title = await screen.findByRole("textbox", { name: "Title" });
  await user.clear(title);
  await user.type(title, "Rocq first{Enter}");
  await waitFor(() => expect(patch).toHaveBeenCalledTimes(1));
  await writeDescription(user, " Discarded.");
  const indicator = await screen.findByTestId("pending-writes");
  const described = /^Description of the priority .* — Waiting to send$/;
  const waiting = (await within(indicator).findByText(described)).closest("li");
  if (waiting === null) throw new Error("the waiting description save is not a row");
  await user.click(within(waiting).getByRole("button", { name: "Discard" }));
  await waitFor(() => expect(within(indicator).queryByText(described)).toBeNull());
  const back = screen.getByRole("group", { name: "Description of P2" });
  expect(within(back).queryByText(/Discarded\./)).toBeNull();
  expect(within(back).getByText("Speedruns run again.")).toBeDefined();
  expect(patch).toHaveBeenCalledTimes(1);
  await act(async () => rename.resolve(saved("rocq-lean")));
});

// A read of the priorities begun before the save lands - an event's refresh - can answer after
// it with the row as it stood. The block shows what landed over that answer, and drawn afresh
// it shows it too, since what it shows is the read's and not a copy of its own.
test("a description save that lands shows the new text, also over a read Dispatch answers late", async () => {
  const user = userEvent.setup();
  const { client, patch, priorities } = page();
  await screen.findByRole("group", { name: "Description of P2" });
  const late = Promise.withResolvers<Priorities>();
  const after = Promise.withResolvers<Priorities>();
  priorities.mockReturnValueOnce(late.promise).mockReturnValueOnce(after.promise);
  const before = priorities.mock.calls.length;
  void cacheOf(client).invalidate({ queryKey: ["priorities", "full"] });
  await waitFor(() => expect(priorities.mock.calls.length).toBe(before + 1));

  await writeDescription(user, " Landed.");
  await waitFor(() => expect(patch).toHaveBeenCalledTimes(1));
  await act(async () => {
    await patch.mock.results[0]?.value;
  });
  await act(async () => late.resolve(BODY));
  await user.click(screen.getByRole("button", { name: "Description" }));
  await user.click(screen.getByRole("button", { name: "Description, closed" }));
  const shown = await screen.findByRole("group", { name: "Description of P2" });
  expect(within(shown).getByText("Speedruns run again. Landed.")).toBeDefined();

  // The read begun after the landing answers with the row as Dispatch made it.
  const landed = ROWS.map((row) =>
    row.id === "rocq-lean" ? { ...row, description: `${row.description} Landed.` } : row
  );
  await act(async () => after.resolve({ ...BODY, priorities: landed }));
  expect(
    within(screen.getByRole("group", { name: "Description of P2" })).getByText(
      "Speedruns run again. Landed."
    )
  ).toBeDefined();
});

// The title and the owner are shown as the description is: from the read, with the write composed
// over it. Enter saves a rename once, and a refused rename or owner leaves Dispatch's own text in
// the field, not the words that were refused.
test("a rename and an owner Dispatch refuses show Dispatch's title and owner again", async () => {
  const user = userEvent.setup();
  const { patch } = page();
  const refusal = new ApiError(400, { code: "PRIORITY_INPUT", error: "not accepted" });
  patch.mockRejectedValueOnce(refusal).mockRejectedValueOnce(refusal);
  const title = await screen.findByRole("textbox", { name: "Title" });
  await user.clear(title);
  await user.type(title, "Refused name{Enter}");
  const indicator = await screen.findByTestId("pending-writes");
  expect(await within(indicator).findByText(/^Not saved: Rename the priority/)).toBeDefined();
  expect(patch).toHaveBeenCalledTimes(1);
  expect((title as HTMLInputElement).value).toBe("Rocq and Lean speed work restarted");

  const owner = screen.getByRole("combobox", { name: "Owner" });
  await user.clear(owner);
  await user.type(owner, "Refused owner");
  await user.tab();
  expect(await within(indicator).findByText(/^Not saved: Owner of the priority/)).toBeDefined();
  expect(patch).toHaveBeenCalledTimes(2);
  expect((owner as HTMLInputElement).value).toBe("BET and LINUX controllers");
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
