import { afterEach, beforeAll, expect, jest, spyOn, test } from "bun:test";
import { ISSUE_TITLE_MAX } from "@legion/contracts";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
// biome-ignore lint/style/noRestrictedImports: the click below leaves the page with the focus still in the title's field, which user-event has no form for: its click moves the focus first, and the field's blur would write before the page goes.
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";
import { Link, MemoryRouter, Route, Routes, useLocation } from "react-router-dom";
import { BeforeEffectsOfRead, stepBeforeEffects } from "../../__tests__/before-effects";
import { displayTree } from "../../__tests__/display-trees";
import { jump, onFakeClock, settle } from "../../__tests__/fake-clock";
import { keptWrites } from "../../__tests__/kept-sends-harness";
import { recordedEditors, selectText, writeAtEnd } from "../../__tests__/recorded-editors";
import {
  answerPriorityPatches,
  PRIORITY_DESCRIPTION_CHANGED,
  PRIORITY_TITLE_CHANGED,
} from "../../__tests__/send-answers";
import { stopTheLateWrite } from "../../__tests__/stop-late-write";
import { ApiError, api, priorityPatchRequest } from "../../api/client";
import { prioritiesQuery } from "../../api/queries";
import type {
  IssueSummary,
  PatchPriorityInput,
  Priorities,
  Priority,
  PriorityRow,
} from "../../api/types";
import { DocumentRuntime } from "../doc/runtime";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { KeymapProvider } from "../shell/KeymapProvider";
import { PendingWritesIndicator } from "../writes/PendingWritesIndicator";
import { writesOf } from "../writes/write-store";
import { SAVE_AFTER_MS } from "./InPlaceField";
import { PriorityPage } from "./PriorityPage";
import { PRIORITIES_PATH } from "./priority-routes";

/** Where a link or a key the page follows lands: the address, which a test reads. */
function Landed() {
  return <output data-testid="landed">{useLocation().pathname}</output>;
}

// The rows draw through the shared renderer, whose Markdown engine loads lazily, and they run in
// random order, so the file loads the engine before its first row, whichever row that is.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

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

/** The read of the priorities once another person wrote `overrides` over priority `id`. */
function writtenOver(id: string, overrides: Partial<PriorityRow>): Priorities {
  return {
    ...BODY,
    priorities: ROWS.map((row) => (row.id === id ? { ...row, ...overrides } : row)),
  };
}

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
  // A change of a priority goes on its priority's line, made from its request alone
  // (`priorityPatchRequest`), so it is answered on its route.
  const patch = answerPriorityPatches().mockImplementation(async (id) => saved(id));
  // The margin reads the priority's own history as soon as the page opens.
  const history = spyOn(api, "getPriorityHistory").mockResolvedValue([]);
  const descriptor = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "clientHeight");
  Object.defineProperty(HTMLElement.prototype, "clientHeight", {
    configurable: true,
    get() {
      return this.dataset.testid?.endsWith("-scroll") ? 600 : (descriptor?.get?.call(this) ?? 0);
    },
  });
  const signedIn = () => {
    const app = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    app.setQueryData(["whoami"], whoami);
    return app;
  };
  // The description is drawn in the documents' own editor, whose handles a test edits through.
  const editors = recordedEditors();
  // What runs in the commit the next read the page takes draws, before the page's effects: after
  // the page's render of it, before the page's effects take it in (`beforeEffects`).
  const { beforeEffects, step } = stepBeforeEffects();
  // The page, the priorities page its link leads to - a stand-in with a way back - the shell's
  // line of pending writes, which stands above the router as the app's does, the step a test runs
  // as a read of the priorities lands (`step`), a way to another priority's page, which the router
  // draws on the same page, and where any other link or key the page follows lands.
  const draw = (app: QueryClient) =>
    render(
      <MemoryRouter initialEntries={[`/priorities/${id}`]}>
        <QueryClientProvider client={app}>
          <DocumentRuntime.Provider value={editors.runtime}>
            <KeymapProvider>
              <Routes>
                <Route element={<PriorityPage />} path="/priorities/:id" />
                <Route
                  element={<Link to={`/priorities/${id}`}>Back to the priority</Link>}
                  path={PRIORITIES_PATH}
                />
                <Route element={<Landed />} path="*" />
              </Routes>
              <PendingWritesIndicator />
              <BeforeEffectsOfRead read={prioritiesQuery()} step={step} />
              <Link to="/priorities/runbook">The runbook's page</Link>
            </KeymapProvider>
          </DocumentRuntime.Provider>
        </QueryClientProvider>
      </MemoryRouter>
    );
  let client = signedIn();
  let view = draw(client);
  restores.push(() => {
    view.unmount();
    client.clear();
    for (const spy of [priorities, listing, state, agents, viewer, patch, history])
      spy.mockRestore();
    if (descriptor) Object.defineProperty(HTMLElement.prototype, "clientHeight", descriptor);
    else Reflect.deleteProperty(HTMLElement.prototype, "clientHeight");
  });
  /** Another person's write of the priority on the page reaching it, as a refresh of its read
   *  does. */
  const writtenElsewhere = (overrides: Partial<PriorityRow>) => {
    const written = writtenOver(id, overrides);
    priorities.mockResolvedValue(written);
    act(() => client.setQueryData(prioritiesQuery().queryKey, written));
  };
  /** The page reloaded on the tab it was on: a new app, with a client and a store of its own,
   *  which takes up the writes the tab kept, as the app does once signed in. */
  const reload = () => {
    view.unmount();
    client.clear();
    client = signedIn();
    view = draw(client);
    act(() => writesOf(client).resume("alice"));
    return client;
  };
  return {
    beforeEffects,
    client,
    editors,
    listing,
    patch,
    priorities,
    reload,
    view,
    writtenElsewhere,
  };
}

/** The page's own heading, the priority's title, in its header: the description is drawn as the
 *  markdown it is, and may hold a first-level heading of its own. */
function titleHeading(): HTMLElement {
  return within(screen.getByTestId("priority-header")).getByRole("heading", { level: 1 });
}

/** The title's field, opened as a reader opens it: a click on the heading the title reads as. */
async function openTitle(user: UserEvent): Promise<HTMLTextAreaElement> {
  await user.click(await waitFor(titleHeading));
  return screen.getByRole("textbox", { name: "Title" }) as HTMLTextAreaElement;
}

// A title as long as production's run (up to 94 characters) reads whole, as the heading of the
// page: it is no field until the reader opens it, as an issue's title is in its header.
test("the title is a heading holding the whole title, and no field shows until it is clicked", async () => {
  const user = userEvent.setup();
  const long =
    "Every Legion phase worker reaches a merged pull request without a person's hand in between it";
  const { writtenElsewhere } = page();
  await screen.findByRole("link", { name: "‹ Priorities" });
  writtenElsewhere({ title: long });
  const heading = await screen.findByRole("heading", { level: 1, name: long });
  expect(heading.textContent).toBe(long);
  expect(screen.queryByRole("textbox", { name: "Title" })).toBeNull();
  await user.click(heading);
  const title = screen.getByRole("textbox", { name: "Title" }) as HTMLInputElement;
  expect(title.value).toBe(long);
  expect(document.activeElement).toBe(title);
  expect(heading.contains(title)).toBe(true);
});

test("the header carries the title in place and the owner, and no line of order or counts", async () => {
  const user = userEvent.setup();
  page();
  expect((await waitFor(titleHeading)).textContent).toBe("Rocq and Lean speed work restarted");
  // The order and the counts are the priorities list's to show (INFRA-3221, R13): this page
  // carries no line about either.
  expect(screen.queryByText("#2")).toBeNull();
  expect(screen.queryByText(/in the order|#\d/)).toBeNull();
  expect(screen.queryByText(/\b(Active|In flight|Triage, Todo|Backlog, icebox) \d/)).toBeNull();
  // The owner is a chip, which opens into the picker the field is drawn as: it offers the owners
  // the deployment already uses and takes one nobody has written yet, so it is a combobox and not
  // a plain field.
  await user.click(screen.getByRole("button", { name: "Owner BET and LINUX controllers" }));
  expect(screen.getByRole("combobox", { name: "Owner" })).toBeDefined();
  expect(screen.getByRole("link", { name: "‹ Priorities" }).getAttribute("href")).toBe(
    "/priorities"
  );
});

test("the description is a disclosure that closes, and the work fills the page below it", async () => {
  const user = userEvent.setup();
  page("rocq-lean", [issue("CORE-1", ["rocq-lean"])]);
  const described = await screen.findByRole("textbox", { name: "Description" });
  expect(await within(described).findByText("Speedruns run again.")).toBeDefined();
  await user.click(screen.getByRole("button", { name: "Description" }));
  await waitFor(() => expect(screen.queryByRole("textbox", { name: "Description" })).toBeNull());
  expect(screen.getByRole("button", { name: "Description, closed" })).toBeDefined();
  // The work fills the page with the description closed.
  expect(
    await screen.findByRole("row", { name: "CORE-1 Work CORE-1" }, { timeout: 3000 })
  ).toBeDefined();
  await user.click(screen.getByRole("button", { name: "Description, closed" }));
  expect(await screen.findByRole("textbox", { name: "Description" })).toBeDefined();
});

test("the description is the documents' editor, with no action on a selection, and its write is the priority's, as markdown", async () => {
  const user = userEvent.setup();
  const { editors, patch } = page();
  const { box, handle } = await editors.named("Description");
  // The editor Dispatch's documents are written in, holding the markdown as it reads: the
  // heading is a heading, and no source is shown.
  expect(box.classList.contains("ProseMirror")).toBe(true);
  expect(box.getAttribute("contenteditable")).toBe("true");
  expect(
    within(box).getByRole("heading", { name: "Rocq and Lean speed work restarted" })
  ).toBeDefined();
  handle.view.focus();
  selectText(handle, "Speedruns");
  expect(document.querySelector(".dispatch-action-bar")).toBeNull();
  for (const name of ["Comment", "Suggest", "Ask"]) {
    expect(screen.queryByRole("button", { name })).toBeNull();
  }
  writeAtEnd(handle, " More.", "More");
  await waitFor(() => expect(patch).toHaveBeenCalledTimes(1), { timeout: 3000 });
  expect(patch.mock.calls[0]?.[0]).toBe("rocq-lean");
  expect(patch.mock.calls[0]?.[1]).toEqual({
    description: "# Rocq and Lean speed work restarted\n\nSpeedruns run again. **More**.\n",
    // Named with the description the edit started from, as the server stores it.
    expected: { description: "# Rocq and Lean speed work restarted\n\nSpeedruns run again." },
  });
  expect(screen.queryByRole("button", { name: /save/i })).toBeNull();
  // Leaving the editor writes nothing more: what it holds is written already.
  await user.tab();
  expect(document.activeElement).not.toBe(box);
  expect(patch).toHaveBeenCalledTimes(1);
});

test("the reader's description kept beside the editor outlives the disclosure closing, and is written when they use it", async () => {
  const user = userEvent.setup();
  const { editors, patch, writtenElsewhere } = page();
  const { handle } = await editors.named("Description");
  handle.view.focus();
  writeAtEnd(handle, " Mine.");
  // Another person's description reaches the page before the typing rests.
  writtenElsewhere({ description: "Written elsewhere." });
  const notice = () =>
    screen.queryByRole("alert", { name: "Description, changed by another person" });
  expect(notice()).not.toBeNull();
  // Closed while it keeps the reader's text, the description stays drawn with its notice: the
  // reader's text is on the page until they choose.
  await user.click(screen.getByRole("button", { name: "Description" }));
  expect(screen.getByRole("button", { name: "Description, closed" })).toBeDefined();
  await editors.named("Description");
  const kept = notice();
  if (kept === null) throw new Error("no notice beside the closed description");
  expect(within(kept).getByText(/Speedruns run again\. Mine\./)).toBeDefined();
  expect(patch).not.toHaveBeenCalled();
  await user.click(within(kept).getByRole("button", { name: "Use my text" }));
  await waitFor(() => expect(patch).toHaveBeenCalledTimes(1));
  expect(patch.mock.calls[0]?.[1]).toEqual({
    description: "# Rocq and Lean speed work restarted\n\nSpeedruns run again. Mine.\n",
    expected: { description: "Written elsewhere." },
  });
  // With nothing kept, the description is closed, as the reader asked.
  await waitFor(() => expect(screen.queryByRole("textbox", { name: "Description" })).toBeNull());
});

test("a title the reader empties writes nothing, and left, shows the title again", async () => {
  const user = userEvent.setup();
  const { patch } = page();
  const title = await openTitle(user);
  await user.clear(title);
  await user.tab();
  expect(titleHeading().textContent).toBe("Rocq and Lean speed work restarted");
  expect(patch).not.toHaveBeenCalled();
});

// Dispatch refuses a priority title longer than an issue title may be, so the field holds no
// more than that: a longer text pasted in is cut at the bound, and no longer title is sent.
test("the title holds no more than an issue title may, so no longer title is sent", async () => {
  const user = userEvent.setup();
  const { patch } = page();
  const title = await openTitle(user);
  expect(title.maxLength).toBe(ISSUE_TITLE_MAX);
  await user.clear(title);
  await user.paste("t".repeat(ISSUE_TITLE_MAX + 1));
  await user.tab();
  await waitFor(() => expect(patch).toHaveBeenCalledTimes(1));
  expect(patch.mock.calls[0]?.[1]).toEqual({
    expected: { title: "Rocq and Lean speed work restarted" },
    title: "t".repeat(ISSUE_TITLE_MAX),
  });
});

test("the title's notice stands outside the heading, which is named by the title alone, and the field names the notice", async () => {
  // The clock is held, so the typing has not rested when the other person's title arrives,
  // however long the typing takes here.
  const user = userEvent.setup({ delay: null });
  const { patch, writtenElsewhere } = page();
  const title = await openTitle(user);
  await onFakeClock(async () => {
    await user.type(title, ", mine");
    writtenElsewhere({ title: "Written elsewhere" });
  });
  const notice = screen.getByRole("alert", { name: "Title, changed by another person" });
  expect(within(notice).getByText("Rocq and Lean speed work restarted, mine")).toBeDefined();
  expect(title.value).toBe("Written elsewhere");
  expect(patch).not.toHaveBeenCalled();
  // The heading holds the field, and its name is the title the field shows: nothing of the
  // notice, which is drawn outside it.
  const heading = screen.getByRole("heading", { level: 1, name: "Written elsewhere" });
  expect(heading.contains(title)).toBe(true);
  expect(heading.contains(notice)).toBe(false);
  // The field and its notice stay tied all the same, and so do the heading and the notice once
  // the reader has left the field.
  expect(notice.id).not.toBe("");
  expect(title.getAttribute("aria-describedby")).toBe(notice.id);
  await user.tab();
  expect(screen.queryByRole("textbox", { name: "Title" })).toBeNull();
  expect(heading.getAttribute("aria-describedby")).toBe(notice.id);
  expect(patch).not.toHaveBeenCalled();
});

test("the keyboard reaches the description's editor from the disclosure above it", async () => {
  const user = userEvent.setup();
  const { editors } = page();
  const { box } = await editors.named("Description");
  screen.getByRole("button", { name: "Description" }).focus();
  await user.tab();
  expect(document.activeElement).toBe(box);
});

test("a reader who may not write reads the description in the same editor, read-only", async () => {
  const { editors, patch } = page("rocq-lean", [], false);
  const { box } = await editors.named("Description");
  expect(within(box).getByText("Speedruns run again.")).toBeDefined();
  expect(box.getAttribute("contenteditable")).toBe("false");
  expect(box.getAttribute("aria-readonly")).toBe("true");
  expect(patch).not.toHaveBeenCalled();
});

test("an empty description says none is written to a reader who may not write it", async () => {
  page("legion-first", [], false);
  expect(await screen.findByText("No description is written.")).toBeDefined();
  expect(screen.queryByRole("textbox", { name: "Description" })).toBeNull();
});

test("an empty description is, for a writer, the prompt above the empty editor", async () => {
  const { editors } = page("legion-first");
  const { box } = await editors.named("Description");
  expect(screen.getByText("No description is written. Write one below.")).toBeDefined();
  expect(box.textContent).toBe("");
  expect(box.getAttribute("contenteditable")).toBe("true");
});

test("the owner is a chip beside the title, which opens into its field and writes the owner", async () => {
  const user = userEvent.setup();
  const { patch } = page();
  await user.click(await screen.findByRole("button", { name: "Owner BET and LINUX controllers" }));
  const owner = screen.getByRole("combobox", { name: "Owner" });
  expect(document.activeElement).toBe(owner);
  await user.clear(owner);
  await user.type(owner, "LINUX controller");
  await user.tab();
  await waitFor(() => expect(patch).toHaveBeenCalledTimes(1));
  expect(patch.mock.calls[0]?.[1]).toEqual({ owner: "LINUX controller" });
  // Left, the field is the chip again, reading what the reader wrote.
  expect(screen.queryByRole("combobox", { name: "Owner" })).toBeNull();
  expect(screen.getByRole("button", { name: "Owner LINUX controller" })).toBeDefined();
});

// The changes to one priority go out in press order (`priority:<id>`): a change made while an
// earlier one is out waits for it, so an older write never lands after a newer one.
test("a change made while an earlier change to the priority is out waits for it", async () => {
  const user = userEvent.setup();
  const { patch } = page();
  const first = Promise.withResolvers<Priority>();
  patch.mockImplementationOnce(() => first.promise);
  await user.click(await screen.findByRole("button", { name: "Owner BET and LINUX controllers" }));
  const owner = screen.getByRole("combobox", { name: "Owner" });
  await user.clear(owner);
  await user.type(owner, "LINUX controller");
  await user.tab();
  await waitFor(() => expect(patch).toHaveBeenCalledTimes(1));
  const title = await openTitle(user);
  await user.type(title, " again");
  await user.tab();
  await act(() => new Promise((resolve) => setTimeout(resolve, 100)));
  expect(patch).toHaveBeenCalledTimes(1);
  await act(async () => first.resolve(saved("rocq-lean")));
  await waitFor(() => expect(patch).toHaveBeenCalledTimes(2));
  expect(patch.mock.calls[1]?.[1]).toEqual({
    expected: { title: "Rocq and Lean speed work restarted" },
    title: "Rocq and Lean speed work restarted again",
  });
});

test("the page carries no tab strip, no version control and no delete", async () => {
  page();
  await waitFor(titleHeading);
  expect(screen.queryByRole("tablist")).toBeNull();
  expect(screen.queryAllByRole("tab")).toHaveLength(0);
  expect(screen.queryByRole("button", { name: /delete|retire|remove/i })).toBeNull();
  expect(screen.queryByRole("button", { name: /version|history of this|restore/i })).toBeNull();
  const text = document.body.textContent ?? "";
  expect(/\bv\d+\b|Versions|Comments on this description/.test(text)).toBe(false);
});

test("one link out leaves the scope, naming the issues and why", async () => {
  page("rocq-lean", [issue("CORE-1", ["rocq-lean"]), issue("CORE-4", ["rocq-lean"])]);
  const out = await screen.findByRole("link", { name: "Open in CORE" });
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

// The work table opens with what a reader of one priority's work reads first, so Title, which
// takes what the others leave, is wide enough to read beside the margin. Every other column of
// the Issues table is one tick away under Columns.
test("the work table opens with the issue, its title, its status and its project", async () => {
  const user = userEvent.setup();
  page("rocq-lean", [issue("CORE-1", ["rocq-lean"])]);
  const work = await screen.findByRole("region", { name: "Work advancing this priority" });
  await within(work).findByRole("row", { name: "CORE-1 Work CORE-1" }, { timeout: 3000 });
  await user.click(within(work).getByRole("button", { name: "Columns" }));
  const menu = screen.getByRole("dialog", { name: "Columns" });
  for (const name of ["Key", "Title", "Status", "Project"])
    expect(within(menu).getByRole("checkbox", { name })).toBeChecked();
  for (const name of ["Claimed by", "Updated", "Priority", "Queue", "Epic", "Assignee"])
    expect(within(menu).getByRole("checkbox", { name })).not.toBeChecked();
  await user.keyboard("{Escape}");
});

test("j, k, x and Enter act on the work rows, the one table the page draws", async () => {
  const user = userEvent.setup();
  page("rocq-lean", [issue("CORE-1", ["rocq-lean"]), issue("CORE-4", ["rocq-lean"])]);
  const work = await screen.findByRole("region", { name: "Work advancing this priority" });
  const first = await within(work).findByRole(
    "row",
    { name: "CORE-1 Work CORE-1" },
    { timeout: 3000 }
  );
  await within(work).findByRole("row", { name: "CORE-4 Work CORE-4" });
  const focused = () => document.activeElement?.getAttribute("data-issue-row");
  first.focus();
  await user.keyboard("j");
  expect(focused()).toBe("CORE-4");
  await user.keyboard("k");
  expect(focused()).toBe("CORE-1");
  // `x` selects the row in hand, as on a project's table.
  await user.keyboard("x");
  await waitFor(() =>
    expect(within(work).getByRole("checkbox", { name: "Select CORE-1" })).toBeChecked()
  );
  expect(within(work).getByRole("checkbox", { name: "Select CORE-4" })).not.toBeChecked();
  // Enter opens the focused issue.
  await user.keyboard("{Enter}");
  expect((await screen.findByTestId("landed")).textContent).toBe("/issues/CORE-1");
});

test("the margin stands beside the page, with no tabs and no control of its own", async () => {
  page();
  const margin = await screen.findByRole("complementary", { name: "Margin" });
  expect(within(margin).queryAllByRole("combobox")).toHaveLength(0);
  expect(within(margin).queryAllByRole("tab")).toHaveLength(0);
});

test("an id no priority carries says so and gives the way back", async () => {
  page("not-written");
  expect(await screen.findByText("No priority of that id is written.")).toBeDefined();
  expect(screen.getByRole("link", { name: "‹ Priorities" })).toBeDefined();
});

/** Keeps in the tab, for a reload to take up, a rename of priority `id` to `text` that Dispatch
 *  refused, another person having written over `over`, the title the reader's edit started
 *  from. */
function keepRefusedTitle(id: string, text: string, over: string) {
  const key = `refused-rename-${id}`;
  sessionStorage.setItem(
    `dispatch.write:${key}`,
    JSON.stringify({
      carried: "PATCH",
      detail: { attribute: "title", expected: over, id, text },
      key,
      kind: "priority-text",
      label: `Rename the priority “${over}” to “${text}”`,
      login: "alice",
      presentation: "indicator",
      pressedAt: Date.now(),
      request: priorityPatchRequest(id, { expected: { title: over }, title: text }),
      sent: true,
      state: {
        refusal: {
          code: "PRIORITY_CHANGED",
          kind: "api",
          message: PRIORITY_TITLE_CHANGED.message,
          status: 409,
        },
        status: "refused",
      },
      target: `priority:${id}`,
      version: 1,
    })
  );
}

// A page that draws no priority - its read refused, or no priority of its id written - draws no
// field to offer the reader's text beside, so it claims none of the priority's writes: a rename
// another person's change refused stays on the shell's line, the reader's text offered to copy
// there, rather than held for a field nobody sees until the reader leaves the page.
test.each([
  ["whose read Dispatch refused", "rocq-lean", "Could not load the priority."],
  ["of an id no priority carries", "not-written", "No priority of that id is written."],
] as const)("a priority's page %s leaves a refused title on the shell's line", async (_how, id, says) => {
  const { priorities, reload } = page(id);
  await screen.findByRole("link", { name: "‹ Priorities" });
  const mine = "Their title, mine";
  keepRefusedTitle(id, mine, "Their title");
  if (id === "rocq-lean")
    priorities.mockRejectedValue(new ApiError(500, { error: "Dispatch could not read it" }));
  const client = reload();
  expect(await screen.findByText(says)).toBeDefined();
  expect(writesOf(client).rows()).toMatchObject([
    { claimed: false, detail: { id, text: mine }, status: "refused" },
  ]);
  const line = await screen.findByTestId("pending-writes");
  expect(within(line).getByRole("note", { name: "Your text:" }).textContent).toBe(mine);
  expect(screen.queryByRole("alert", { name: /changed by another person/ })).toBeNull();
});

// A write a reload took up is the page's to settle with the reader as much as one pressed on it:
// refused because another person wrote the description since the reader's edit started, it
// stands on no line with a Retry that would send it again. The description shows it beside the
// other person's text, drawn though the reader had closed it, and nothing of it is sent again;
// the store holds it for the page until the reader settles it.
test("a description a reload took up and another person's change refused is kept beside the description, closed or not", async () => {
  const user = userEvent.setup();
  const { client, editors, patch } = page();
  await editors.named("Description");
  await user.click(screen.getByRole("button", { name: "Description" }));
  await waitFor(() => expect(screen.queryByRole("textbox", { name: "Description" })).toBeNull());
  const mine = "Speedruns run again, every night.\n\nMine.";
  const body = { description: mine, expected: { description: "Speedruns run." } };
  const request = priorityPatchRequest("rocq-lean", body);
  const key = "resumed-description";
  sessionStorage.setItem(
    `dispatch.write:${key}`,
    JSON.stringify({
      carried: "PATCH",
      detail: { attribute: "description", expected: "Speedruns run.", id: "rocq-lean", text: mine },
      key,
      kind: "priority-text",
      label: "Description of the priority “Rocq and Lean speed work restarted”",
      login: "alice",
      presentation: "indicator",
      pressedAt: Date.now(),
      request,
      sent: true,
      state: { status: "sending" },
      target: "priority:rocq-lean",
      version: 1,
    })
  );
  patch.mockRejectedValueOnce(PRIORITY_DESCRIPTION_CHANGED);
  act(() => writesOf(client).resume("alice"));
  const notice = await screen.findByRole("alert", {
    name: "Description, changed by another person",
  });
  expect(within(notice).getByText(/Speedruns run again, every night\./)).toBeDefined();
  expect(within(notice).getByRole("button", { name: "Use my text" })).toBeDefined();
  expect(writesOf(client).rows()).toMatchObject([{ claimed: true, status: "refused" }]);
  expect(screen.queryByTestId("pending-writes")).toBeNull();
  expect(patch).toHaveBeenCalledTimes(1);
  expect(patch.mock.calls[0]?.[1]).toEqual(body);
});

/** The reader writes `, mine` at the end of the title, and leaves the field: Dispatch refuses it
 *  (`patch`), another person having written `Theirs` since, which the page reads (`priorities`)
 *  once it takes the refusal. Answers the notice the page then draws, holding the reader's
 *  text. */
async function refusedTitle(
  priorities: { mockResolvedValue: (body: Priorities) => unknown },
  patch: { mockRejectedValueOnce: (refusal: Error) => unknown }
) {
  // The clock is held, so the typing does not rest and write part of it before the reader leaves
  // the field, however long the typing takes here.
  const user = userEvent.setup({ delay: null });
  const title = await openTitle(user);
  priorities.mockResolvedValue(writtenOver("rocq-lean", { title: "Theirs" }));
  patch.mockRejectedValueOnce(PRIORITY_TITLE_CHANGED);
  await onFakeClock(async () => {
    await user.type(title, ", mine");
  });
  await user.tab();
  const notice = await screen.findByRole("alert", { name: "Title, changed by another person" });
  expect(within(notice).getByText(`${ROWS[1]?.title}, mine`)).toBeDefined();
  // Left, the field is the heading again, which reads the other person's title.
  expect(titleHeading().textContent).toBe("Theirs");
  return { notice, user };
}

/** The reader writes `, mine` at the end of the title, and another person's `Theirs` reaches the
 *  page before the typing rests: the field keeps the reader's text beside the title on its own,
 *  with no write of it sent and none refused. Answers the notice the page then draws. */
async function keptTitle(writtenElsewhere: (overrides: Partial<PriorityRow>) => void) {
  // The clock is held, so the typing has not rested when the other person's title arrives,
  // however long the typing takes here.
  const user = userEvent.setup({ delay: null });
  const title = await openTitle(user);
  await onFakeClock(async () => {
    await user.type(title, ", mine");
    writtenElsewhere({ title: "Theirs" });
  });
  const notice = await screen.findByRole("alert", { name: "Title, changed by another person" });
  expect(within(notice).getByText(`${ROWS[1]?.title}, mine`)).toBeDefined();
  expect(title).toHaveValue("Theirs");
  return { notice, user };
}

/** The page drawn, with the reader's title beside another person's as a write of the store: a
 *  write of it another person's change refused (`refusedTitle`) where a write of it went out
 *  (`sent`), and else a text the field kept on its own as another person's arrived
 *  (`keptTitle`). Answers the page and the notice it draws. */
async function titleBesideTheirs(sent: boolean) {
  const drawn = page();
  const { notice, user } = sent
    ? await refusedTitle(drawn.priorities, drawn.patch)
    : await keptTitle(drawn.writtenElsewhere);
  return { ...drawn, notice, user };
}

/** The two ways of `titleBesideTheirs`, each with the title the write names as the one it goes
 *  over, which the shell's line names it by. */
const titleFlows = [
  ["another person's change refused", { over: ROWS[1]?.title, sent: true }],
  ["the field kept as another person's arrived", { over: "Theirs", sent: false }],
] as const;

// The reader's text is the store's, not the page's, whether a refusal brought it back or the
// field kept it on its own with no write of it sent: leaving the page hands it to the shell's
// line, which offers it to copy in a conflict's words with no Retry, and the page drawn again
// takes it from there and offers it beside the title once more. Using it then writes it over the
// other person's title, as a press of its own, and nothing of it stays, in the store or in what
// the tab keeps.
test.each(
  titleFlows
)("a title %s outlives leaving the page, is offered again on the way back, and using it lets it go", async (_flow, {
  over,
  sent,
}) => {
  const { client, patch, user } = await titleBesideTheirs(sent);
  const mine = `${ROWS[1]?.title}, mine`;
  const sentBefore = sent ? 1 : 0;
  expect(patch).toHaveBeenCalledTimes(sentBefore);
  expect(screen.queryByTestId("pending-writes")).toBeNull();
  // The store holds it, as a write the page holds that another person's text refused: the
  // reader's text, over the title the write names.
  expect(writesOf(client).rows()).toMatchObject([
    {
      claimed: true,
      detail: { attribute: "title", expected: over, text: mine },
      sent,
      status: "refused",
    },
  ]);

  await user.click(screen.getByRole("link", { name: "‹ Priorities" }));
  const line = await screen.findByTestId("pending-writes");
  expect(
    within(line).getByText(
      `Not saved because another person changed it: Rename the priority “${over}” to “${mine}”`
    )
  ).toBeDefined();
  expect(within(line).getByRole("note", { name: "Your text:" }).textContent).toBe(mine);
  expect(within(line).queryByRole("button", { name: "Retry" })).toBeNull();

  await user.click(screen.getByRole("link", { name: "Back to the priority" }));
  const notice = await screen.findByRole("alert", { name: "Title, changed by another person" });
  expect(within(notice).getByText(mine)).toBeDefined();
  expect(screen.queryByTestId("pending-writes")).toBeNull();
  expect(patch).toHaveBeenCalledTimes(sentBefore);

  await user.click(within(notice).getByRole("button", { name: "Use my text" }));
  await waitFor(() => expect(patch).toHaveBeenCalledTimes(sentBefore + 1));
  expect(patch.mock.calls[sentBefore]?.slice(0, 2)).toEqual([
    "rocq-lean",
    { expected: { title: "Theirs" }, title: mine },
  ]);
  await waitFor(() => expect(writesOf(client).rows()).toEqual([]));
  expect(keptWrites()).toEqual([]);
});

// A reload takes the write up again from what the tab kept, as it takes up any refused write,
// and the page drawn on it offers the reader's text as the page before did. Keeping the other
// person's title lets the write go, and nothing of it is kept or sent.
test.each(titleFlows)("a title %s outlives a reload, and keeping theirs lets it go", async (_flow, {
  sent,
}) => {
  const { patch, reload } = await titleBesideTheirs(sent);
  expect(keptWrites()).toHaveLength(1);
  const client = reload();
  const notice = await screen.findByRole("alert", { name: "Title, changed by another person" });
  expect(within(notice).getByText(`${ROWS[1]?.title}, mine`)).toBeDefined();
  expect(writesOf(client).rows()).toMatchObject([{ claimed: true, status: "refused" }]);
  expect(screen.queryByTestId("pending-writes")).toBeNull();

  await userEvent.setup().click(within(notice).getByRole("button", { name: "Keep their text" }));
  expect(screen.queryByRole("alert", { name: /changed by another person/ })).toBeNull();
  expect(writesOf(client).rows()).toEqual([]);
  expect(keptWrites()).toEqual([]);
  expect(titleHeading().textContent).toBe("Theirs");
  expect(patch).toHaveBeenCalledTimes(sent ? 1 : 0);
});

// A write the page holds whose text the page then reads is one Dispatch holds: the reader wrote
// it again, or another person wrote the same text. It goes, from the store and from what the tab
// keeps, so leaving the page puts nothing on the shell's line.
test("a title another person's change refused goes once the page reads it, so leaving shows nothing", async () => {
  const { client, patch, priorities, writtenElsewhere } = page();
  const { user } = await refusedTitle(priorities, patch);
  writtenElsewhere({ title: `${ROWS[1]?.title}, mine` });
  await waitFor(() => expect(writesOf(client).rows()).toEqual([]));
  expect(keptWrites()).toEqual([]);
  expect(screen.queryByRole("alert", { name: /changed by another person/ })).toBeNull();

  await user.click(screen.getByRole("link", { name: "‹ Priorities" }));
  await screen.findByRole("link", { name: "Back to the priority" });
  expect(screen.queryByTestId("pending-writes")).toBeNull();
  expect(patch).toHaveBeenCalledTimes(1);
});

// The router keeps the page drawn as the reader goes from one priority's page to another's, and
// the page draws itself afresh for each: the write it held for the priority before goes back to
// the shell's line, which offers the reader's text to copy, as leaving the page does.
test("going to another priority's page hands the title another person's change refused back to the line", async () => {
  const { client, patch, priorities } = page();
  const { user } = await refusedTitle(priorities, patch);
  await user.click(screen.getByRole("link", { name: "The runbook's page" }));
  await waitFor(() => expect(titleHeading().textContent).toBe("The fleet has a runbook"));
  const line = await screen.findByTestId("pending-writes");
  expect(within(line).getByRole("note", { name: "Your text:" }).textContent).toBe(
    `${ROWS[1]?.title}, mine`
  );
  expect(writesOf(client).rows()).toMatchObject([{ claimed: false, status: "refused" }]);
  expect(screen.queryByRole("alert", { name: /changed by another person/ })).toBeNull();
});

// The title's field writes what the reader typed as it leaves with the page, which happens after
// the page has handed its writes back to the shell's line where the reader leaves with the focus
// still in the field. Typed while a refused write's text waits for the read its refusal started,
// that text is held rather than sent, and it is the line's at once, beside the refused write's:
// no page is left to claim it.
test("a title typed while a refused one waits for its read is on the shell's line once the page goes", async () => {
  // The clock is held, so no typing rests long enough to write before the field leaves.
  const user = userEvent.setup({ delay: null });
  const { client, patch, priorities } = page();
  const title = await openTitle(user);
  // The read the refusal starts never answers, so the refused write's text waits for it.
  priorities.mockImplementation(() => Promise.withResolvers<Priorities>().promise);
  patch.mockRejectedValueOnce(PRIORITY_TITLE_CHANGED);
  const writes = writesOf(client);
  await onFakeClock(async () => {
    await user.type(title, ", mine");
    await user.tab();
    await act(async () => {
      await settle();
    });
    expect(writes.rows()).toMatchObject([{ claimed: true, status: "refused" }]);
    // Left, the field is the heading again, which the reader opens to write on.
    await user.click(titleHeading());
    await user.type(screen.getByRole("textbox", { name: "Title" }), ", more");
    // A click alone, which leaves the focus in the field as it goes with the page.
    fireEvent.click(screen.getByRole("link", { name: "‹ Priorities" }));
  });
  expect(writes.rows()).toMatchObject([
    { claimed: false, detail: { text: `${ROWS[1]?.title}, mine` }, sent: true, status: "refused" },
    {
      claimed: false,
      detail: { text: `${ROWS[1]?.title}, mine, more` },
      sent: false,
      status: "refused",
    },
  ]);
  const line = await screen.findByTestId("pending-writes");
  expect(
    within(line)
      .getAllByRole("note", { name: "Your text:" })
      .map((note) => note.textContent)
  ).toEqual([`${ROWS[1]?.title}, mine`, `${ROWS[1]?.title}, mine, more`]);
  expect(patch).toHaveBeenCalledTimes(1);
});

// The page reads another person's title while the reader's typing waits, and the typing comes to
// rest once the page drew that title and before the field's effects took it in. The rename names
// the title the field started from, never the page's read, which holds the other person's by
// then: Dispatch refuses it, and the page offers the reader's text once, beside their title - the
// text the field kept as their title arrived and the refused write's are one.
test("a title written once the page read another person's, before the field took it in, names the title the field started from, and is offered once", async () => {
  const user = userEvent.setup({ delay: null });
  const { beforeEffects, client, patch, writtenElsewhere } = page();
  const title = await openTitle(user);
  patch.mockRejectedValueOnce(PRIORITY_TITLE_CHANGED);
  await onFakeClock(async () => {
    await user.type(title, ", mine");
    beforeEffects(() => jest.advanceTimersByTime(SAVE_AFTER_MS));
    writtenElsewhere({ title: "Theirs" });
    // The write goes out, and its refusal comes back, before the clock comes back.
    await act(async () => {
      await settle();
    });
  });
  const mine = `${ROWS[1]?.title}, mine`;
  await waitFor(() => expect(patch).toHaveBeenCalledTimes(1));
  expect(patch.mock.calls[0]?.slice(0, 2)).toEqual([
    "rocq-lean",
    { expected: { title: ROWS[1]?.title }, title: mine },
  ]);
  await waitFor(() =>
    expect(writesOf(client).rows()).toMatchObject([
      { claimed: true, detail: { text: mine }, sent: true, status: "refused" },
    ])
  );
  const notices = await screen.findAllByRole("alert", { name: "Title, changed by another person" });
  expect(notices).toHaveLength(1);
  expect(within(notices[0] as HTMLElement).getAllByText(mine)).toHaveLength(1);
  expect(title).toHaveValue("Theirs");
  expect(patch).toHaveBeenCalledTimes(1);
});

// The same for the description: the typing in its editor comes to rest once the page drew
// another person's description and before the editor's effects took it in. The write names the
// description the editor started from, as the server stores it, and the page offers the reader's
// text once, beside theirs.
test("a description written once the page read another person's, before the editor took it in, names the description the editor started from, and is offered once", async () => {
  const { beforeEffects, client, editors, patch, writtenElsewhere } = page();
  const { box, handle } = await editors.named("Description");
  handle.view.focus();
  patch.mockRejectedValueOnce(PRIORITY_DESCRIPTION_CHANGED);
  await onFakeClock(async () => {
    writeAtEnd(handle, " Mine.");
    beforeEffects(() => jest.advanceTimersByTime(SAVE_AFTER_MS));
    writtenElsewhere({ description: "Written elsewhere." });
    await act(async () => {
      await settle();
    });
  });
  const mine = "# Rocq and Lean speed work restarted\n\nSpeedruns run again. Mine.\n";
  await waitFor(() => expect(patch).toHaveBeenCalledTimes(1));
  expect(patch.mock.calls[0]?.slice(0, 2)).toEqual([
    "rocq-lean",
    {
      description: mine,
      expected: { description: "# Rocq and Lean speed work restarted\n\nSpeedruns run again." },
    },
  ]);
  await waitFor(() =>
    expect(writesOf(client).rows()).toMatchObject([
      { claimed: true, detail: { text: mine }, sent: true, status: "refused" },
    ])
  );
  const notices = await screen.findAllByRole("alert", {
    name: "Description, changed by another person",
  });
  expect(notices).toHaveLength(1);
  expect(
    within(notices[0] as HTMLElement).getAllByText(/Speedruns run again\. Mine\./)
  ).toHaveLength(1);
  expect(box.textContent).toBe("Written elsewhere.");
  expect(patch).toHaveBeenCalledTimes(1);
});

// Another person's title stood when the reader's rename reached Dispatch, and they set it back
// before the page read it again: the page reads the title the field took in already, so the field
// takes nothing in and still holds the reader's text, which the page offers beside it. Use my text
// writes it all the same, over the title the field took in, and it lands.
test("Use my text writes the title the field holds already, over the title it took in, and it lands", async () => {
  const user = userEvent.setup({ delay: null });
  const { client, patch } = page();
  const title = await openTitle(user);
  patch.mockRejectedValueOnce(PRIORITY_TITLE_CHANGED);
  await onFakeClock(async () => {
    await user.type(title, ", mine");
  });
  await user.tab();
  const mine = `${ROWS[1]?.title}, mine`;
  const notice = await screen.findByRole("alert", { name: "Title, changed by another person" });
  expect(within(notice).getByText(mine)).toBeDefined();
  expect(titleHeading().textContent).toBe(mine);
  await user.click(within(notice).getByRole("button", { name: "Use my text" }));
  await waitFor(() => expect(patch).toHaveBeenCalledTimes(2));
  expect(patch.mock.calls[1]?.slice(0, 2)).toEqual([
    "rocq-lean",
    { expected: { title: ROWS[1]?.title }, title: mine },
  ]);
  await waitFor(() => expect(writesOf(client).rows()).toEqual([]));
  expect(screen.queryByRole("alert", { name: /changed by another person/ })).toBeNull();
});

// The reader types over the other person's title while the notice shows, and presses Use my text:
// the click leaves the field, whose write of that typing goes first. Use my text's write goes after
// it and names its text, which the server holds by then, so it lands, and no second notice comes.
// The server here takes a title written over the one it holds, and refuses any other.
test("Use my text pressed as the field's own write leaves names that write's title, and lands", async () => {
  const user = userEvent.setup({ delay: null });
  const { client, patch, priorities, writtenElsewhere } = page();
  let stored = ROWS[1]?.title ?? "";
  const title = await openTitle(user);
  await onFakeClock(async () => {
    await user.type(title, ", mine");
    stored = "Written elsewhere";
    writtenElsewhere({ title: stored });
    await user.type(title, ", more");
  });
  patch.mockImplementation(async (id, input) => {
    if (input.expected?.title !== stored) throw PRIORITY_TITLE_CHANGED;
    stored = input.title ?? stored;
    return { ...saved(id), title: stored };
  });
  priorities.mockImplementation(async () => writtenOver("rocq-lean", { title: stored }));
  const notice = screen.getByRole("alert", { name: "Title, changed by another person" });
  await user.click(within(notice).getByRole("button", { name: "Use my text" }));
  const mine = `${ROWS[1]?.title}, mine`;
  await waitFor(() => expect(patch).toHaveBeenCalledTimes(2));
  expect(patch.mock.calls.map(([, input]) => input)).toEqual([
    { expected: { title: "Written elsewhere" }, title: "Written elsewhere, more" },
    { expected: { title: "Written elsewhere, more" }, title: mine },
  ]);
  await waitFor(() => expect(writesOf(client).rows()).toEqual([]));
  expect(stored).toBe(mine);
  expect(titleHeading().textContent).toBe(mine);
  expect(screen.queryByRole("alert", { name: /changed by another person/ })).toBeNull();
});

// The same for the description: the editor holds the reader's text still, and Use my text writes
// it over the description the editor took in.
test("Use my text writes the description the editor holds already, over the description it took in, and it lands", async () => {
  const { client, editors, patch } = page();
  const { box, handle } = await editors.named("Description");
  handle.view.focus();
  patch.mockRejectedValueOnce(PRIORITY_DESCRIPTION_CHANGED);
  writeAtEnd(handle, " Mine.");
  act(() => handle.view.dom.blur());
  const notice = await screen.findByRole("alert", {
    name: "Description, changed by another person",
  });
  expect(within(notice).getByText(/Speedruns run again\. Mine\./)).toBeDefined();
  expect(box.textContent).toContain("Speedruns run again. Mine.");
  await userEvent.setup().click(within(notice).getByRole("button", { name: "Use my text" }));
  await waitFor(() => expect(patch).toHaveBeenCalledTimes(2));
  expect(patch.mock.calls[1]?.slice(0, 2)).toEqual([
    "rocq-lean",
    {
      description: "# Rocq and Lean speed work restarted\n\nSpeedruns run again. Mine.\n",
      expected: { description: ROWS[1]?.description },
    },
  ]);
  await waitFor(() => expect(writesOf(client).rows()).toEqual([]));
  expect(screen.queryByRole("alert", { name: /changed by another person/ })).toBeNull();
});

/** An answer that is no verdict: the attempt failed, and the write would try again later. */
const UNAVAILABLE = new ApiError(503, { code: "UNAVAILABLE", error: "the server is down" });

/** Dispatch as the rows below meet it, for `attribute` of the page's priority: its answer to a
 *  write (`write`), which answers no attempt of `lost` - the network loses each - refuses one
 *  naming a text it does not hold, as another person's text refuses it, and takes any other; and
 *  a read of the priorities (`read`), which shows what it holds. */
function dispatchLosing(
  attribute: "description" | "title",
  lost: string
): {
  read: () => Promise<Priorities>;
  write: (id: string, input: PatchPriorityInput) => Promise<Priority>;
} {
  let stored = ROWS[1]?.[attribute] ?? "";
  const holding = () => (attribute === "title" ? { title: stored } : { description: stored });
  return {
    read: async () => writtenOver("rocq-lean", holding()),
    write: async (id, input) => {
      if (input[attribute] === lost) throw UNAVAILABLE;
      if (input.expected?.[attribute] !== stored) {
        throw attribute === "title" ? PRIORITY_TITLE_CHANGED : PRIORITY_DESCRIPTION_CHANGED;
      }
      stored = input[attribute] ?? stored;
      return { ...saved(id), ...holding() };
    },
  };
}

/** How many of the reader's writes the rows below make, one after another, each made on the one
 *  before, and whether the page is drawn again - left, and come back to - before she uses one. */
const RUNS = [
  ["her last write, the write it was made on stopped", 2, false],
  ["the newest of her refused writes, the write the first was made on stopped", 3, false],
  [
    "the newest of her refused writes, the write the first was made on stopped, on the page drawn again",
    3,
    true,
  ],
] as const;

// The reader writes the title or the description on, each write made on the one before: "A",
// then "AB" and "ABC". Dispatch answers no attempt of "A", and she stops it; the others waited
// behind it and name a text it never took, so each is refused 409 - the limit rule 3 accepts -
// and kept beside the field, newest first. She uses the newest: the same edit as its write, made
// on the text the write before it was made on, and so on through the texts the click lets go,
// down to "A". Its landing lets "A" go, from the line and from the tab, and its Retry sends
// nothing. A field drawn again knows none of its writes, and reads what each was made on from the
// page, which holds them.
test.each(
  (["title", "description"] as const).flatMap((attribute) =>
    RUNS.map(([run, count, again]) => [attribute, run, count, again] as const)
  )
)("Use my text in the %s, of %s, is made on the stopped write, which its landing lets go", async (attribute, _run, count, again) => {
  const user = userEvent.setup({ delay: null });
  const { client, editors, patch, priorities } = page();
  const start = ROWS[1]?.[attribute] ?? "";
  const ends = (attribute === "title" ? [", A", "B", "C"] : [" A", " B", " C"]).slice(0, count);
  const texts = ends.map((_end, at) => {
    const typed = `${start}${ends.slice(0, at + 1).join("")}`;
    return attribute === "title" ? typed : `${typed}\n`;
  });
  const dispatch = dispatchLosing(attribute, texts[0] ?? "");
  patch.mockImplementation(dispatch.write);
  priorities.mockImplementation(dispatch.read);
  if (attribute === "title") {
    const title = await openTitle(user);
    await onFakeClock(async () => {
      for (const end of ends) {
        await user.type(title, end);
        await jump(SAVE_AFTER_MS);
      }
      await stopTheLateWrite(client);
    });
  } else {
    const { handle } = await editors.named("Description");
    await onFakeClock(async () => {
      for (const end of ends) {
        handle.view.focus();
        writeAtEnd(handle, end);
        act(() => handle.view.dom.blur());
      }
      await stopTheLateWrite(client);
    });
  }
  const writes = writesOf(client);
  const [stopped] = writes.rows();
  expect(stopped).toMatchObject({ detail: { text: texts[0] }, status: "refused" });
  const named = `${attribute === "title" ? "Title" : "Description"}, changed by another person`;
  let notice = await screen.findByRole("alert", { name: named });
  if (again) {
    await user.click(screen.getByRole("link", { name: "‹ Priorities" }));
    await user.click(await screen.findByRole("link", { name: "Back to the priority" }));
    notice = await screen.findByRole("alert", { name: named });
  }
  const [newest, ...older] = within(notice).getAllByRole("button", {
    name: /^Use (my|this) text$/,
  });
  expect(older).toHaveLength(count - 2);
  if (newest === undefined) throw new Error("no Use my text beside the field");
  await user.click(newest);
  const used = texts.at(-1);
  await waitFor(() =>
    expect(patch.mock.calls.at(-1)?.[1]).toEqual(
      attribute === "title"
        ? { expected: { title: start }, title: used }
        : { description: used, expected: { description: start } }
    )
  );
  await waitFor(() => expect(sessionStorage.getItem(`dispatch.write:${stopped?.key}`)).toBeNull());
  const sent = patch.mock.calls.length;
  act(() => writes.retry(stopped?.id ?? 0));
  await waitFor(() => expect(writes.rows()).toEqual([]));
  expect(patch).toHaveBeenCalledTimes(sent);
  expect(screen.queryByRole("alert", { name: /changed by another person/ })).toBeNull();
});

/** Dispatch's refusal of a description it will not take: its body, not another person's text. */
const DESCRIPTION_REFUSED = new ApiError(400, {
  code: "PRIORITY_INPUT",
  error: "the description is longer than a description may be",
});

// A description write Dispatch refused never comes back, and the page tells the editor so: the
// editor waits for no answer with it. Once the server says it cannot read the stored text as
// markdown, the editor draws the server's reading of it, read-only, though the text did not
// change - whichever of the refusal and that word came first - rather than keep the markdown the
// reader wrote under the line that says Dispatch cannot read it.
test.each([
  ["the refusal", true],
  ["the server's word", false],
] as const)("after a description write Dispatch refused, the editor draws the server's reading once the server cannot read the text as markdown, %s first", async (_first, refusedFirst) => {
  // The server's reading of the stored text is read with the page's own engine (`displayTree`).
  await warmMarkdownRenderer();
  const { editors, patch, writtenElsewhere } = page();
  const { box, handle } = await editors.named("Description");
  handle.view.focus();
  const refusal = Promise.withResolvers<Priority>();
  patch.mockImplementationOnce(() => refusal.promise);
  writeAtEnd(handle, " Mine.");
  act(() => handle.view.dom.blur());
  await waitFor(() => expect(patch).toHaveBeenCalledTimes(1));
  const refuse = () =>
    act(async () => {
      refusal.reject(DESCRIPTION_REFUSED);
      await settle();
    });
  const stored = ROWS[1]?.description ?? "";
  if (refusedFirst) await refuse();
  writtenElsewhere({ description_tree: displayTree(stored), description_tree_exact: false });
  if (!refusedFirst) await refuse();
  await waitFor(() => expect(box.textContent).not.toContain("Mine."));
  expect(box.textContent).toBe("Rocq and Lean speed work restartedSpeedruns run again.");
  expect(box.getAttribute("contenteditable")).toBe("false");
  expect(screen.getByText(/^Dispatch cannot read this description as markdown/)).toBeDefined();
  expect(patch).toHaveBeenCalledTimes(1);
});
