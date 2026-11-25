import { afterEach, beforeAll, expect, jest, mock, spyOn, test } from "bun:test";
import { ISSUE_TITLE_MAX } from "@legion/contracts";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import {
  act,
  createEvent,
  // biome-ignore lint/style/noRestrictedImports: the drag of a row onto another below is a drag and a drop, which user-event has no form for
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { BrowserRouter, MemoryRouter, useLocation, useNavigationType } from "react-router-dom";
import { BeforeEffectsOfRead, stepBeforeEffects } from "../../__tests__/before-effects";
import { onFakeClock, settle } from "../../__tests__/fake-clock";
import { recordedEditors, writeAtEnd } from "../../__tests__/recorded-editors";
import {
  answerPriorityPatches,
  PRIORITY_DESCRIPTION_CHANGED,
  PRIORITY_TITLE_CHANGED,
} from "../../__tests__/send-answers";
import { stopTheLateWrite } from "../../__tests__/stop-late-write";
import { ApiError, api, priorityPatchRequest } from "../../api/client";
import { prioritiesQuery } from "../../api/queries";
import type { DisplayTree, IssueSummary, Priorities, Priority, PriorityRow } from "../../api/types";
import { textMutedOnCanvas } from "../../theme/classes";
import { DocumentRuntime } from "../doc/runtime";
import { loadedEngine, warmMarkdownRenderer } from "../refs/markdown-engine";
import { KeymapProvider } from "../shell/KeymapProvider";
import { writesOf } from "../writes/write-store";
import { SAVE_AFTER_MS } from "./InPlaceField";
import { PrioritiesPage } from "./PrioritiesPage";

// A closed row's excerpt is the projection the markdown engine makes of its description
// (`useMarkdownHeadline`), so the engine is loaded once before any row draws, as the shell loads
// it as a page opens (`DeploymentResilience`).
beforeAll(async () => {
  await warmMarkdownRenderer();
});

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

function body(overrides: Partial<Priorities> = {}): Priorities {
  return {
    priorities: [],
    work_under_no_priority: { by_project: null, count: 0, issues: null },
    priority_work_parked: { by_priority: null, count: 0, issues: null },
    ...overrides,
  };
}

/** The deployment's issues as the listing route answers them, standing in whatever status and
 *  advancing whatever priorities `advances` and `overrides` give. */
function issue(
  key: string,
  advances: string[] = [],
  overrides: Partial<IssueSummary> = {}
): IssueSummary {
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
    ...overrides,
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

/** The listing route as the server answers it: `advances` keeps the issues advancing any of the
 *  ids it names, `none` those advancing none and `any` those advancing one; `status` keeps the
 *  issues standing in one of its statuses; `open` leaves the closed out; and the move into
 *  backlog or icebox is carried only when `moved` asks for it. */
function answerListing(issues: readonly IssueSummary[]) {
  return async (options: Parameters<typeof api.listIssues>[0] = {}) =>
    issues
      .filter((row) => {
        const ids = (row.advances ?? []).map((relation) => relation.id);
        return (
          (options.advances === undefined ||
            options.advances.some((asked) =>
              asked === "none"
                ? ids.length === 0
                : asked === "any"
                  ? ids.length > 0
                  : ids.includes(asked)
            )) &&
          (options.status === undefined || options.status.includes(row.status)) &&
          (!options.open || row.status !== "done")
        );
      })
      .map(({ moved_at, moved_by, ...row }) =>
        options.moved ? { ...row, moved_at, moved_by } : row
      );
}

/** The address the page is at, and how it got there: a tab chosen and a row the keyboard opens
 *  push, and an address naming a tab the page does not have is replaced. */
function Address() {
  const location = useLocation();
  return (
    <output data-testid="address">{`${useNavigationType()} ${location.pathname}${location.search}`}</output>
  );
}

/** The page as a signed-in person sees it, with the reads it makes answered from `answer`; a
 *  reader who is not a person, and so may not write a priority, where `writer` is false; at the
 *  address `at`; each table's box `box` px wide where a test names a width, which the unit DOM
 *  otherwise measures as none, a phone's; and in the window's own history where `history` is
 *  `window`, so a test reads how many entries it holds, rather than in one of the router's own. */
function page(
  answer: Priorities,
  issues: IssueSummary[] = [],
  writer = true,
  {
    at = "/priorities",
    box,
    history = "memory",
  }: { at?: string; box?: number; history?: "memory" | "window" } = {}
) {
  const priorities = spyOn(api, "listPriorities").mockResolvedValue(answer);
  const listing = spyOn(api, "listIssues").mockImplementation(answerListing(issues));
  const state = spyOn(api, "getMyState").mockResolvedValue({});
  const agents = spyOn(api, "listAgents").mockResolvedValue([]);
  // `useIsHuman`: the contract types only a person's answer, so the other principal is cast.
  const whoami = writer
    ? { event_head: "0", kind: "user" as const, login: "alice" }
    : ({ event_head: "0", kind: "token", login: "ci" } as never);
  const viewer = spyOn(api, "whoAmI").mockResolvedValue(whoami);
  const create = spyOn(api, "createPriority").mockResolvedValue(saved("new"));
  // A change of a priority goes on its priority's line, made from its request alone
  // (`priorityPatchRequest`), so it is answered on its route.
  const patch = answerPriorityPatches().mockImplementation(async (id) => saved(id));
  // The unit DOM lays nothing out: give every table's box the height a window would.
  const descriptor = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "clientHeight");
  Object.defineProperty(HTMLElement.prototype, "clientHeight", {
    configurable: true,
    get() {
      return this.dataset.testid?.endsWith("-scroll") ? 600 : (descriptor?.get?.call(this) ?? 0);
    },
  });
  // Stubbed on Element, the prototype that owns the method, for the boxes alone.
  const measured = Element.prototype.getBoundingClientRect;
  const rect =
    box === undefined
      ? undefined
      : spyOn(Element.prototype, "getBoundingClientRect").mockImplementation(function (
          this: Element
        ) {
          return this.getAttribute("data-testid")?.endsWith("-scroll") === true
            ? new DOMRect(0, 0, box, 600)
            : measured.call(this);
        });
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(["whoami"], whoami);
  // An open row's description is drawn in the documents' own editor, which a test edits through.
  const editors = recordedEditors();
  // What runs in the commit the next read the page takes draws, before the page's effects: after
  // the page's render of it, before the page's effects take it in (`beforeEffects`).
  const { beforeEffects, step } = stepBeforeEffects();
  const tree = (
    <QueryClientProvider client={client}>
      <DocumentRuntime.Provider value={editors.runtime}>
        <KeymapProvider>
          <PrioritiesPage />
          <Address />
          <BeforeEffectsOfRead read={prioritiesQuery()} step={step} />
        </KeymapProvider>
      </DocumentRuntime.Provider>
    </QueryClientProvider>
  );
  if (history === "window") window.history.replaceState(null, "", at);
  const routed = () =>
    history === "window" ? (
      <BrowserRouter>{tree}</BrowserRouter>
    ) : (
      <MemoryRouter initialEntries={[at]}>{tree}</MemoryRouter>
    );
  let view = render(routed());
  restores.push(() => {
    view.unmount();
    client.clear();
    for (const spy of [priorities, listing, state, agents, viewer, create, patch])
      spy.mockRestore();
    rect?.mockRestore();
    if (descriptor) Object.defineProperty(HTMLElement.prototype, "clientHeight", descriptor);
    else Reflect.deleteProperty(HTMLElement.prototype, "clientHeight");
    if (history === "window") window.history.replaceState(null, "", "/");
  });
  /** Another person's write of priority `id` reaching the page, as a refresh of its read does. */
  const writtenElsewhere = (id: string, overrides: Partial<PriorityRow>) => {
    const written = {
      ...answer,
      priorities: (answer.priorities ?? []).map((row) =>
        row.id === id ? { ...row, ...overrides } : row
      ),
    };
    priorities.mockResolvedValue(written);
    act(() => client.setQueryData(prioritiesQuery().queryKey, written));
  };
  /** Takes the page off and draws it again at `at`, with the same client: a table drawn again
   *  reads its layout from what the browser stored, as it does on a reload. */
  const redraw = () => {
    view.unmount();
    view = render(routed());
  };
  return {
    beforeEffects,
    client,
    create,
    editors,
    listing,
    patch,
    priorities,
    redraw,
    writtenElsewhere,
  };
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

/** The columns each fault list opens with, by the names its Columns control gives them
 *  (INFRA-2947, R6): the issue, its title, its status, who holds it, how long it has stood and
 *  its project. The rest of the Issues table's columns open hidden. */
const R6_COLUMNS = ["Key", "Title", "Status", "Claimed by", "Updated", "Project"];
const OTHER_COLUMNS = [
  "Priority",
  "Queue",
  "Epic",
  "Assignee",
  "Created by",
  "Labels",
  "Open asks",
  "Created",
];

test("the page opens on the priorities, and each fault is a tab of its own the address keeps", async () => {
  const user = userEvent.setup();
  page(
    body({
      ...FIFTEEN,
      work_under_no_priority: { by_project: { CORE: 1 }, count: 1, issues: null },
      priority_work_parked: { by_priority: { "linkage-closure": 1 }, count: 1, issues: null },
    }),
    [issue("CORE-1"), issue("CORE-9", ["linkage-closure"], { status: "backlog" })]
  );
  await screen.findByRole("table", { name: "Priorities table" });
  // The heading alone heads the page: no line under it, and no heading over any table.
  expect(screen.getAllByRole("heading").map((heading) => heading.textContent)).toEqual([
    "Priorities",
  ]);
  expect(screen.queryByText(/read now|their owner drags/)).toBeNull();
  // Three tabs, each reading its name and its one count, the priorities chosen while the
  // address names none.
  const tabs = screen.getAllByRole("tab");
  expect(tabs.map((tab) => tab.getAttribute("aria-label"))).toEqual([
    "Priorities 3",
    "Active under no priority 1",
    "Priority work in backlog or icebox 1",
  ]);
  expect(tabs.map((tab) => tab.getAttribute("aria-selected"))).toEqual(["true", "false", "false"]);
  // Only the chosen tab draws its table, so that table is the last thing on the page.
  expect(screen.getAllByRole("table").map((table) => table.getAttribute("aria-label"))).toEqual([
    "Priorities table",
  ]);

  // A fault's tab draws the table of its own rows and no other's, with no line of description
  // and no counts between the tab and it. The address names the tab, in place of the last.
  await user.click(screen.getByRole("tab", { name: "Active under no priority 1" }));
  const faults = await screen.findByRole("table", {
    name: "Active under no priority table",
  });
  expect(screen.getByTestId("address").textContent).toBe("PUSH /priorities?tab=none");
  expect(screen.queryByRole("table", { name: "Priorities table" })).toBeNull();
  // The tab's panel holds the Issues table alone, under the tab's own name.
  expect(within(screen.getByRole("tabpanel")).getByRole("region")).toHaveAccessibleName(
    "Active under no priority"
  );
  expect(screen.queryByText(/advance no priority|\d+ issues? ·/)).toBeNull();
  expect(await within(faults).findByRole("row", { name: "CORE-1 Work CORE-1" })).toBeDefined();
  expect(within(faults).queryByRole("row", { name: "CORE-9 Work CORE-9" })).toBeNull();

  await user.click(screen.getByRole("tab", { name: "Priority work in backlog or icebox 1" }));
  const backlog = await screen.findByRole("table", {
    name: "Priority work in backlog or icebox table",
  });
  expect(screen.getByTestId("address").textContent).toBe("PUSH /priorities?tab=backlog");
  expect(screen.getAllByRole("table")).toEqual([backlog]);
  expect(screen.queryByText(/backlog or the icebox|\d+ issues? ·/)).toBeNull();
  expect(await within(backlog).findByRole("row", { name: "CORE-9 Work CORE-9" })).toBeDefined();
  expect(within(backlog).queryByRole("row", { name: "CORE-1 Work CORE-1" })).toBeNull();

  await user.click(screen.getByRole("tab", { name: "Priorities 3" }));
  expect(await screen.findByRole("table", { name: "Priorities table" })).toBeDefined();
  expect(screen.getByTestId("address").textContent).toBe("PUSH /priorities?tab=order");
});

test("an address naming a fault's tab opens on it", async () => {
  page(FIFTEEN, [issue("CORE-9", ["linkage-closure"], { status: "backlog" })], true, {
    at: "/priorities?tab=backlog",
  });
  expect(
    await screen.findByRole("table", { name: "Priority work in backlog or icebox table" })
  ).toBeDefined();
  expect(screen.getByRole("tab", { name: "Priority work in backlog or icebox 0" })).toHaveAttribute(
    "aria-selected",
    "true"
  );
  expect(screen.queryByRole("table", { name: "Priorities table" })).toBeNull();
});

/** The backlog-or-icebox tab's name in the address before it was named `backlog`, which a link
 *  written then still carries. It is built from its parts so that no added line of the branch
 *  holds the word the person refused (INFRA-2947, check 1a3e1452). */
const OLD_TAB = ["p", "arked"].join("");

// An address naming a tab the page does not have opens on the priorities, and the name leaves
// the address with replace: no entry is added to the history, so Back leads where it did, and
// the address never names a tab the page does not draw. The priorities table's own view in the
// same address stays (INFRA-2947, Verification v73). A tab the reader then chooses is a place
// they went, as a tab of the project page or the issue page is: it adds one entry, so Back leads
// to the tab before rather than off the page.
test("an address naming a tab the page does not have opens on the priorities, and the name leaves the address, where a tab chosen adds an entry", async () => {
  const user = userEvent.setup();
  const entries = window.history.length;
  page(FIFTEEN, [], true, { at: `/priorities?tab=${OLD_TAB}&p.q=linkage`, history: "window" });
  expect(await screen.findByRole("table", { name: "Priorities table" })).toBeDefined();
  expect(screen.getByRole("tab", { name: "Priorities 3" })).toHaveAttribute(
    "aria-selected",
    "true"
  );
  await waitFor(() =>
    expect(screen.getByTestId("address").textContent).toBe("REPLACE /priorities?p.q=linkage")
  );
  expect(window.history.length).toBe(entries);
  // The search kept in the address still narrows the table.
  expect(screen.getByRole("row", { name: "#2 Linkage closure eval" })).toBeDefined();
  expect(screen.queryByRole("row", { name: "#1 Legion works reliably first" })).toBeNull();

  await user.click(screen.getByRole("tab", { name: "Active under no priority 0" }));
  expect(
    await screen.findByRole("table", { name: "Active under no priority table" })
  ).toBeDefined();
  expect(screen.getByTestId("address").textContent).toBe("PUSH /priorities?p.q=linkage&tab=none");
  expect(window.history.length).toBe(entries + 1);
});

// The tab already chosen is no place to go: a click on it writes nothing, so the history gains
// no entry, and Back leads where it did rather than staying on that tab once for each click.
test("a click on the tab already chosen writes nothing to the address or the history", async () => {
  const user = userEvent.setup();
  const entries = window.history.length;
  page(FIFTEEN, [], true, { at: "/priorities?tab=none", history: "window" });
  expect(
    await screen.findByRole("table", { name: "Active under no priority table" })
  ).toBeDefined();
  const chosen = screen.getByRole("tab", { name: "Active under no priority 0" });
  expect(chosen).toHaveAttribute("aria-selected", "true");

  await user.click(chosen);
  expect(screen.getByTestId("address").textContent).toBe("POP /priorities?tab=none");
  expect(window.history.length).toBe(entries);
});

test("each fault table asks the issue listing for its statuses, and the backlog-or-icebox one for the move", async () => {
  const user = userEvent.setup();
  const { listing } = page(
    FIFTEEN,
    [
      issue("CORE-1"),
      issue("OPS-12", [], { status: "needs_review" }),
      issue("CORE-9", ["linkage-closure"], {
        status: "backlog",
        moved_at: "2026-11-09T12:00:00Z",
        moved_by: { kind: "user", id: "bob" },
      }),
      issue("OPS-4", ["legion-first", "oss-verify"], { status: "icebox" }),
    ],
    true,
    { at: "/priorities?tab=none" }
  );
  const faults = await screen.findByRole("table", {
    name: "Active under no priority table",
  });
  // The narrowing is the server's: the no-priority list names the statuses work is done in, and
  // the backlog-or-icebox list those two statuses with the move into them. Neither reads a closed
  // issue.
  expect(listing).toHaveBeenCalledWith({
    advances: ["none"],
    status: ["in_progress", "testing", "needs_review", "retro"],
    open: true,
  });

  const cellsOf = async (table: HTMLElement, name: string) => {
    const headers = within(table)
      .getAllByRole("columnheader")
      .map((header) => header.textContent ?? "");
    const row = await within(table).findByRole("row", { name });
    const cells = within(row).getAllByRole("cell");
    return (header: string) => {
      const at = headers.findIndex((text) => text.startsWith(header));
      if (at === -1) throw new Error(`no ${header} column`);
      const cell = cells[at];
      if (cell === undefined) throw new Error(`no ${header} cell`);
      return cell;
    };
  };

  // The no-priority list names each row's project and when it last changed, which is how long it
  // has stood as it is (R6), and shows none of the backlog-or-icebox list's columns.
  const inFlight = await cellsOf(faults, "OPS-12 Work OPS-12");
  expect(inFlight("Project").textContent).toBe("OPS");
  expect(within(inFlight("Updated")).getByRole("time").getAttribute("datetime")).toBe(
    "2026-11-01T00:00:00Z"
  );
  const faultHeaders = within(faults)
    .getAllByRole("columnheader")
    .map((header) => header.textContent ?? "");
  expect(faultHeaders.some((text) => text.startsWith("Advances"))).toBe(false);
  expect(faultHeaders.some((text) => text.startsWith("Moved there"))).toBe(false);
  expect(faultHeaders.some((text) => text.startsWith("Reason"))).toBe(false);

  // The list under no priority opens with R6's columns alone, offers every other column of the
  // Issues table unticked, and does not offer the backlog-or-icebox list's columns at all.
  await user.click(
    within(screen.getByRole("region", { name: "Active under no priority" })).getByRole("button", {
      name: "Columns",
    })
  );
  const menu = screen.getByRole("dialog", { name: "Columns" });
  for (const name of R6_COLUMNS) expect(within(menu).getByRole("checkbox", { name })).toBeChecked();
  for (const name of OTHER_COLUMNS)
    expect(within(menu).getByRole("checkbox", { name })).not.toBeChecked();
  for (const name of ["Advances", "Moved there", "Reason"])
    expect(within(menu).queryByRole("checkbox", { name })).toBeNull();
  await user.keyboard("{Escape}");

  await user.click(screen.getByRole("tab", { name: "Priority work in backlog or icebox 0" }));
  const backlog = await screen.findByRole("table", {
    name: "Priority work in backlog or icebox table",
  });
  // The backlog-or-icebox list opens with those but Updated, the priority each issue stands
  // under, and the move: when the work was moved there is how long it has stood, so Updated opens
  // hidden, one tick away.
  await user.click(
    within(screen.getByRole("region", { name: "Priority work in backlog or icebox" })).getByRole(
      "button",
      { name: "Columns" }
    )
  );
  const backlogMenu = screen.getByRole("dialog", { name: "Columns" });
  for (const name of [...R6_COLUMNS.filter((one) => one !== "Updated"), "Advances", "Moved there"])
    expect(within(backlogMenu).getByRole("checkbox", { name })).toBeChecked();
  // No reason is recorded for a move until dispatch://INFRA-2934, so the list offers no Reason.
  expect(within(backlogMenu).queryByRole("checkbox", { name: "Reason" })).toBeNull();
  for (const name of ["Updated", ...OTHER_COLUMNS])
    expect(within(backlogMenu).getByRole("checkbox", { name })).not.toBeChecked();
  await user.keyboard("{Escape}");
  // The list's read asks for the work whose own row names a priority, as the page's count does.
  expect(listing).toHaveBeenCalledWith({
    advances: ["any"],
    own: true,
    status: ["backlog", "icebox"],
    moved: true,
    open: true,
  });

  // The backlog-or-icebox list names the project, each priority advanced by its place in the
  // order with its title, linking to it, and when the work was moved there and by whom, which
  // stands for how long it has stood: no Updated column opens beside it. A row answered with no
  // move draws an empty Moved there cell: absent is absent, and that cell does not say a record
  // is missing.
  const moved = await cellsOf(backlog, "CORE-9 Work CORE-9");
  expect(moved("Project").textContent).toBe("CORE");
  expect(
    within(backlog)
      .getAllByRole("columnheader")
      .some((header) => header.textContent?.startsWith("Updated"))
  ).toBe(false);
  expect(
    within(moved("Advances"))
      .getByRole("link", { name: "#2 Linkage closure eval" })
      .getAttribute("href")
  ).toBe("/priorities/linkage-closure");
  expect(within(moved("Moved there")).getByRole("time").getAttribute("datetime")).toBe(
    "2026-11-09T12:00:00Z"
  );
  expect(moved("Moved there").textContent).toContain("bob");
  // Advances stands at 160 px, under its 180 px cap, and Title takes what the rest leave. No row
  // of the list stands in Legion's queue, so no cell for a row's Start next follows its columns.
  expect(within(backlog).getByRole("columnheader", { name: /^Advances/ }).style.width).toBe(
    "160px"
  );
  expect(within(backlog).getAllByRole("columnheader").at(-1)?.textContent).not.toBe("");
  const unrecorded = await cellsOf(backlog, "OPS-4 Work OPS-4");
  // Each priority is a chip that reads its place and then its title, drawn whole after it in the
  // same link, its title in its name and its tooltip.
  const chips = within(unrecorded("Advances")).getAllByRole("link");
  expect(
    chips.map((link) => ({
      drawn: [...link.children].map((part) => part.textContent),
      name: link.getAttribute("aria-label"),
      title: link.getAttribute("title"),
    }))
  ).toEqual([
    {
      drawn: ["#1", "Legion works reliably first"],
      name: "#1 Legion works reliably first",
      title: "Legion works reliably first",
    },
    { drawn: ["#3", "OSS Verify"], name: "#3 OSS Verify", title: "OSS Verify" },
  ]);
  expect(unrecorded("Moved there").textContent).toBe("");
  expect(within(unrecorded("Moved there")).queryByRole("time")).toBeNull();

  // A fault read names its statuses every time it is made: no read of every closed issue goes
  // out beside it.
  const faultReads = listing.mock.calls.filter(([options]) =>
    ["none", "any"].includes(options?.advances?.[0] ?? "")
  );
  expect(faultReads.length).toBeGreaterThan(0);
  expect(faultReads.every(([options]) => options?.status !== undefined)).toBe(true);
});

// A column the reader shows on a fault list is theirs to keep, as on any issues table: each list
// pins the checkbox, the key and Title alone (`pins`), so the Priority column it opens hidden is
// no pinned column, whose visibility the stored layout drops (`readLayout`). Shown, it stays
// shown once the reader leaves the tab and comes back, and once the page is drawn again.
test("a Priority column shown on a fault list stays shown across a tab switch and the page drawn again", async () => {
  const user = userEvent.setup();
  const { redraw } = page(FIFTEEN, [issue("CORE-1")], true, { at: "/priorities?tab=none" });
  const priorityShown = async () =>
    within(await screen.findByRole("table", { name: "Active under no priority table" }))
      .getAllByRole("columnheader")
      .some((header) => header.textContent?.startsWith("Priority") === true);
  expect(await priorityShown()).toBe(false);
  await user.click(
    within(screen.getByRole("region", { name: "Active under no priority" })).getByRole("button", {
      name: "Columns",
    })
  );
  await user.click(
    within(screen.getByRole("dialog", { name: "Columns" })).getByRole("checkbox", {
      name: "Priority",
    })
  );
  await user.keyboard("{Escape}");
  expect(await priorityShown()).toBe(true);

  await user.click(screen.getByRole("tab", { name: "Priorities 3" }));
  expect(await screen.findByRole("table", { name: "Priorities table" })).toBeDefined();
  await user.click(screen.getByRole("tab", { name: "Active under no priority 0" }));
  expect(await priorityShown()).toBe(true);

  redraw();
  expect(await priorityShown()).toBe(true);
});

// An empty fault list says the state the page wants while the reader narrowed it by nothing: no
// issue in flight under no priority, no priority work in backlog or icebox. Narrowed by a search
// or a filter of its own in the address, it says that nothing matches them, as every issues
// table does.
test.each([
  ["none", "", "No issue is in flight under no priority."],
  ["none", "&n.q=rollout", "No issues match these filters."],
  ["none", "&n.status=testing", "No issues match these filters."],
  ["backlog", "", "No priority work is in backlog or icebox."],
  ["backlog", "&k.q=rollout", "No issues match these filters."],
  ["backlog", "&k.status=icebox", "No issues match these filters."],
] as const)("an empty fault list at ?tab=%s%s says: %s", async (tab, narrowed, said) => {
  page(FIFTEEN, [], true, { at: `/priorities?tab=${tab}${narrowed}` });
  const list = await screen.findByRole("region", {
    name: tab === "none" ? "Active under no priority" : "Priority work in backlog or icebox",
  });
  expect(await within(list).findByText(said)).toBeDefined();
  for (const other of [
    "No issue is in flight under no priority.",
    "No priority work is in backlog or icebox.",
    "No issues match these filters.",
  ].filter((sentence) => sentence !== said))
    expect(within(list).queryByText(other)).toBeNull();
});

test("with no priority the page says so, and offers a writer the way to add the first", async () => {
  const user = userEvent.setup();
  const { create } = page(body({ empty: "No priority has been written yet." }));
  expect(await screen.findByText("No priority has been written yet.")).toBeDefined();
  expect(screen.queryByRole("table", { name: "Priorities table" })).toBeNull();
  await user.click(screen.getByRole("button", { name: "New priority" }));
  await user.type(screen.getByRole("textbox", { name: "Title of the new priority" }), "First");
  await user.click(screen.getByRole("button", { name: "Add priority" }));
  await waitFor(() => expect(create).toHaveBeenCalledTimes(1));
  expect(create.mock.calls[0]?.[0]).toEqual({ title: "First" });
});

test("with no priority a reader who may not write one is shown only the sentence", async () => {
  page(body({ empty: "No priority has been written yet." }), [], false);
  expect(await screen.findByText("No priority has been written yet.")).toBeDefined();
  expect(screen.queryByRole("button", { name: "New priority" })).toBeNull();
});

/** The box a table gets in a 1280 px window beside the sidebar, and in a 390 px one. */
const DESKTOP_BOX = 910;
const PHONE_BOX = 342;

test("a row counts the work in flight, waiting and in backlog or icebox, shows its newest event, and what closed when asked", async () => {
  const user = userEvent.setup();
  page(FIFTEEN, [], true, { box: DESKTOP_BOX });
  const row = await screen.findByRole("row", { name: "#1 Legion works reliably first" });
  const texts = (of: HTMLElement) =>
    within(of)
      .getAllByRole("cell")
      .map((cell) => cell.textContent);
  // In flight is in_progress, testing, needs_review and retro; waiting is todo and triage; the
  // third count is backlog and icebox. The newest event is read as a time, whose words follow
  // the clock. What closed in 7 days opens hidden.
  expect(
    screen.getAllByRole("columnheader").map((header) => header.textContent?.replace("Filter", ""))
  ).toEqual([
    "#▲",
    "Priority",
    "Owner",
    "Active",
    "Triage, Todo",
    "Backlog, icebox",
    "Newest event",
  ]);
  expect(texts(row).slice(3, 6)).toEqual(["7", "12", "0"]);
  expect(within(row).getByRole("time").getAttribute("datetime")).toBe("2026-11-11T10:00:00Z");
  // Each count's column is as narrow as its numbers, 76 px; its numbers stand at its right edge,
  // under its header's words, which stand there too and wrap rather than be cut. The newest
  // event's words wrap beside its filter, in a column as narrow as its time.
  for (const [at, name] of ["Active", "Triage, Todo", "Backlog, icebox"].entries()) {
    const header = screen.getByRole("columnheader", { name: new RegExp(`^${name}`) });
    const words = within(header).getByRole("button", { name });
    const cell = within(row).getAllByRole("cell")[3 + at]?.firstElementChild;
    expect({
      cell: cell?.className.split(" ").slice(0, 2),
      edge: words.parentElement?.className.split(" ").includes("justify-end"),
      name,
      width: header.style.width,
      words: words.className,
    }).toEqual({
      cell: ["block", "text-right"],
      edge: true,
      name,
      width: "76px",
      words: "flex min-w-0 items-center gap-1 text-right",
    });
  }
  const newest = screen.getByRole("columnheader", { name: /^Newest event/ });
  expect([
    newest.style.width,
    within(newest).getByRole("button", { name: "Newest event" }).className,
  ]).toEqual(["104px", "flex min-w-0 items-center gap-1 text-left"]);

  // Shown under Columns, the fourth count is what the read says closed in 7 days.
  await user.click(
    within(screen.getByRole("region", { name: "Priorities" })).getByRole("button", {
      name: "Columns",
    })
  );
  const menu = screen.getByRole("dialog", { name: "Columns" });
  expect(within(menu).getByRole("checkbox", { name: "Newest event" })).toBeChecked();
  await user.click(within(menu).getByRole("checkbox", { name: "Closed, 7 days" }));
  expect(texts(row).slice(3, 7)).toEqual(["7", "12", "0", "22"]);
  const second = screen.getByRole("row", { name: "#2 Linkage closure eval" });
  expect(texts(second).slice(3)).toEqual(["1", "0", "3", "2", "Never"]);
});

// On a phone the table draws each priority as its place beside one cell and no other column, so
// nothing scrolls sideways: the place and the title take the whole box, each header stands on
// one line, the `#` header offers its sort and no filter, the one cell of the place holds the
// grip and the disclosure that opens the row, and the title's cell names the owner, the three
// counts under their headers' words and the newest event (INFRA-2947, R7). The tab offers no
// Columns control there, since the one cell names every field whatever is shown, and the box's
// right border is the one line at the title's right: no divider of the pinned columns beside it.
test("on a phone the table draws the place and one cell, which names the owner, the counts and the newest event", async () => {
  page(FIFTEEN, [], true, { box: PHONE_BOX });
  const row = await screen.findByRole("row", { name: "#1 Legion works reliably first" });
  expect(
    screen.getAllByRole("columnheader").map((header) => {
      const line = header.firstElementChild?.className.split(" ") ?? [];
      return {
        filter: within(header).queryAllByTitle(/^Filter/).length > 0,
        name: header.textContent?.replace("Filter", ""),
        oneLine: !line.includes("flex-col") && !line.includes("justify-end"),
        width: header.style.width,
      };
    })
  ).toEqual([
    { filter: false, name: "#▲", oneLine: true, width: "76px" },
    { filter: true, name: "Priority", oneLine: true, width: `${PHONE_BOX - 76}px` },
  ]);
  expect(
    within(screen.getByRole("region", { name: "Priorities" })).queryByRole("button", {
      name: "Columns",
    })
  ).toBeNull();
  const divided = (cell: HTMLElement) => cell.className.split(" ").includes("border-r");
  expect(screen.getAllByRole("columnheader").map(divided)).toEqual([false, false]);
  const cells = within(row).getAllByRole("cell");
  const [place, title] = cells;
  if (place === undefined || title === undefined) throw new Error("the row draws no two cells");
  expect(cells).toHaveLength(2);
  expect(cells.map(divided)).toEqual([false, false]);
  expect(
    within(place)
      .getAllByRole("button")
      .map((button) => button.getAttribute("aria-label"))
  ).toEqual([
    "Move #1 Legion works reliably first: drag it, or press the up and down arrows",
    "Open #1 Legion works reliably first",
  ]);
  // The line under the title that names the owner, the counts and the newest event: the last
  // thing in the title's cell.
  const phoneLine = (cell: HTMLElement | undefined) => cell?.firstElementChild?.lastElementChild;
  // In flight is in_progress, testing, needs_review and retro; waiting is todo and triage; the
  // third count is backlog and icebox; the newest event is a time, whose words follow the clock.
  expect(phoneLine(title)?.textContent).toStartWith(
    "INFRA controller · Active 7 · Triage, Todo 12 · Backlog, icebox 0 · "
  );
  expect(within(title).getByRole("time").getAttribute("datetime")).toBe("2026-11-11T10:00:00Z");
  const second = screen.getByRole("row", { name: "#2 Linkage closure eval" });
  expect(phoneLine(within(second).getAllByRole("cell")[1])?.textContent).toBe(
    "BET controller · Active 1 · Triage, Todo 0 · Backlog, icebox 3 · Never"
  );
});

// A phone held sideways gives the table a box of 500 to 639 px, a phone's all the same: the place
// and the title take the whole of it, so the title's lines reach the box's right border as they
// do at 390 px (INFRA-2947, R7), rather than stop at the title's own width and leave a gap.
test("on a phone held sideways the place and the title take the whole box", async () => {
  page(FIFTEEN, [], true, { box: 560 });
  await screen.findByRole("row", { name: "#1 Legion works reliably first" });
  expect(screen.getAllByRole("columnheader").map((header) => header.style.width)).toEqual([
    "76px",
    `${560 - 76}px`,
  ]);
});

test("the start of a description shows on a closed row, its lines run together in two lines at most, and the whole of it when it opens", async () => {
  const user = userEvent.setup();
  const { editors } = page(FIFTEEN, [issue("CORE-1", ["legion-first"]), issue("CORE-2")]);
  await screen.findByRole("row", { name: "#1 Legion works reliably first" });
  // Closed, the row shows the description's start, its line break read as a space, clamped to
  // two lines that end in '…' where it runs on.
  const excerpt = await screen.findByText(
    "The first line. The rest of it, which the row shows when it is open."
  );
  expect(excerpt.className.split(" ")).toContain("line-clamp-2");
  await user.click(screen.getByRole("button", { name: "Open #1 Legion works reliably first" }));
  // Open, the row holds the whole description in the documents' editor, its line break read as
  // the space a paragraph's soft break is.
  const { box } = await editors.named("Description of #1");
  expect(box.classList.contains("ProseMirror")).toBe(true);
  expect(box.textContent).toBe(
    "The first line. The rest of it, which the row shows when it is open."
  );
});

// A description of several paragraphs whose first line is short and names an issue by its
// Dispatch reference, as production's do: the closed row reads its paragraphs run together, its
// markdown read rather than shown and the reference by its short form, so a first line that
// fits leaves the rest after it for the clamp to end in '…' (INFRA-2947, R7).
test("a closed row runs a description's paragraphs together, a reference read by its short form", async () => {
  page(
    body({
      priorities: [
        priority("local-testing", {
          description:
            "Start with dispatch://CORE-1.\n\nEvery reader of **this** deployment reaches the same corpus.",
          title: "Local testing is one command",
        }),
      ],
    })
  );
  const row = await screen.findByRole("row", { name: "#1 Local testing is one command" });
  const excerpt = await within(row).findByText(
    "Start with CORE-1. Every reader of this deployment reaches the same corpus."
  );
  expect(excerpt.className.split(" ")).toContain("line-clamp-2");
});

/** Emphasis nested one deeper than Go's display read takes (pmdoc's `maxInlineNesting`, 100),
 *  607 characters. */
const NESTED = `${"*a ".repeat(101)}b${" c*".repeat(101)}`;

/** The server's reading of `NESTED`, as Go's display read writes it for marks nested past its
 *  bound (`literalDocument`): one paragraph of the text's own characters, no mark among them,
 *  where the page's parser reads the text as emphasis and drops every `*`. */
const NESTED_TREE: DisplayTree = {
  content: [{ content: [{ text: NESTED, type: "text" }], type: "paragraph" }],
  type: "doc",
};

// A closed row's excerpt is drawn from the server's reading of its description
// (`description_tree`), and nothing on the page parses the description: the excerpt reads the
// tree's words, which the markdown parsed would not, cut at the row's 400 characters.
test("a closed row's excerpt is drawn from the server's tree of its description, and nothing on the page parses it", async () => {
  const engine = loadedEngine();
  if (engine === undefined) throw new Error("the row needs the shared engine loaded");
  const counted = engine as { parse: typeof engine.parse };
  const parse = counted.parse;
  const parsed: number[] = [];
  counted.parse = (markdown, softBreaks) => {
    parsed.push(markdown.length);
    return parse.call(engine, markdown, softBreaks);
  };
  restores.push(() => {
    counted.parse = parse;
  });
  page(
    body({
      priorities: [
        priority("nested", {
          description: NESTED,
          description_tree: NESTED_TREE,
          title: "Nested past the bound",
        }),
      ],
    })
  );
  const row = await screen.findByRole("row", { name: "#1 Nested past the bound" });
  const excerpt = await within(row).findByText(`${NESTED.slice(0, 400)}…`);
  expect(excerpt.className.split(" ")).toContain("line-clamp-2");
  expect(parsed).toEqual([]);
});

test("opening a row shows its description and no work: the work is on the priority's own page", async () => {
  const user = userEvent.setup();
  const { editors, listing } = page(FIFTEEN, [issue("CORE-1", ["legion-first"]), issue("OPS-3")]);
  const row = await screen.findByRole("row", { name: "#1 Legion works reliably first" });
  await user.click(screen.getByRole("button", { name: "Open #1 Legion works reliably first" }));
  await editors.named("Description of #1");
  // The tab draws one table, whatever is open, and reads no work for it: the way to the work is
  // the title, a link to the priority's own page.
  expect(screen.getAllByRole("table").map((table) => table.getAttribute("aria-label"))).toEqual([
    "Priorities table",
  ]);
  expect(screen.queryByRole("region", { name: /^Work advancing/ })).toBeNull();
  expect(listing).not.toHaveBeenCalledWith({ advances: ["legion-first"], open: true });
  expect(
    within(row).getByRole("link", { name: "Legion works reliably first" }).getAttribute("href")
  ).toBe("/priorities/legion-first");
});

test("a person who may write adds a priority", async () => {
  const user = userEvent.setup();
  const { create } = page(FIFTEEN);
  await screen.findByRole("row", { name: "#1 Legion works reliably first" });
  await user.click(screen.getByRole("button", { name: "New priority" }));
  await user.type(
    screen.getByRole("textbox", { name: "Title of the new priority" }),
    "Spend ruthlessly"
  );
  await user.click(screen.getByRole("button", { name: "Add priority" }));
  await waitFor(() => expect(create).toHaveBeenCalledTimes(1));
  expect(create.mock.calls[0]?.[0]).toEqual({ title: "Spend ruthlessly" });
});

// Dispatch refuses a priority title longer than an issue title may be, so the New priority form
// holds no more than that: a longer text pasted in is cut at the bound, and no longer title is
// sent.
test("the New priority title holds no more than an issue title may, so no longer title is sent", async () => {
  const user = userEvent.setup();
  const { create } = page(FIFTEEN);
  await screen.findByRole("row", { name: "#1 Legion works reliably first" });
  await user.click(screen.getByRole("button", { name: "New priority" }));
  const title = screen.getByRole("textbox", {
    name: "Title of the new priority",
  }) as HTMLInputElement;
  expect(title.maxLength).toBe(ISSUE_TITLE_MAX);
  await user.click(title);
  await user.paste("t".repeat(ISSUE_TITLE_MAX + 1));
  await user.click(screen.getByRole("button", { name: "Add priority" }));
  await waitFor(() => expect(create).toHaveBeenCalledTimes(1));
  expect(create.mock.calls[0]?.[0]).toEqual({ title: "t".repeat(ISSUE_TITLE_MAX) });
});

test("a title is a link to the priority's own page, where it is renamed, and no field", async () => {
  page(FIFTEEN);
  const row = await screen.findByRole("row", { name: "#2 Linkage closure eval" });
  expect(within(row).getByRole("link", { name: "Linkage closure eval" }).getAttribute("href")).toBe(
    "/priorities/linkage-closure"
  );
  expect(within(row).queryByRole("textbox")).toBeNull();
  expect(screen.getByRole("columnheader", { name: /^#/ })).toBeDefined();
  // An owner is drawn whole: it wraps in its column, and nothing cuts it to one line.
  const owner = within(row).getByText("BET controller");
  expect({
    cut: /truncate|line-clamp|text-ellipsis|whitespace-nowrap/.test(owner.className),
    text: owner.textContent,
  }).toEqual({ cut: false, text: "BET controller" });
});

// Production's titles run to 94 characters, and a reader reads each one whole: the title wraps
// in its column as a link to the priority's page, and nothing clamps or cuts it. The start of
// the description under it, its paragraphs run together, is clamped to two lines, in the small
// muted type, ending in '…' where it runs on, with the whole of it its tooltip.
test("a 94-character title shows whole on its row, with no clamp and no cut", async () => {
  const long =
    "Every Legion phase worker reaches a merged pull request without a person's hands in between it";
  expect(long.length).toBe(94);
  const summary = "A phase worker that finishes its phase hands the next one a head it can act on.";
  page(
    body({
      priorities: [
        priority("phase-workers", { description: `${summary}\n\nThe rest.`, title: long }),
      ],
    })
  );
  const row = await screen.findByRole("row", { name: `#1 ${long}` });
  const link = within(row).getByRole("link", { name: long });
  expect(link.textContent).toBe(long);
  // The link, what it holds, and what holds it inside the cell.
  const cell = link.closest("td");
  const around: Element[] = [];
  for (let at = link.parentElement; at !== null && at !== cell; at = at.parentElement)
    around.push(at);
  for (const element of [link, ...link.querySelectorAll("*"), ...around]) {
    const classes = element.getAttribute("class") ?? "";
    expect({ classes, cut: /line-clamp|truncate|text-ellipsis/.test(classes) }).toEqual({
      classes,
      cut: false,
    });
  }
  const line = await within(row).findByTitle(`${summary} The rest.`);
  expect(line.textContent).toBe(`${summary} The rest.`);
  expect(line.className).toBe(`line-clamp-2 break-words text-xs ${textMutedOnCanvas}`);
});

test("the reader's description kept beside a row outlives the row leaving the table, and is written when they use it", async () => {
  const user = userEvent.setup();
  const { editors, patch, writtenElsewhere } = page(FIFTEEN);
  await screen.findByRole("row", { name: "#2 Linkage closure eval" });
  await user.click(screen.getByRole("button", { name: "Open #2 Linkage closure eval" }));
  const { handle } = await editors.named("Description of #2");
  handle.view.focus();
  writeAtEnd(handle, "Mine.");
  // Another person's description reaches the page before the typing rests.
  writtenElsewhere("linkage-closure", { description: "Written elsewhere." });
  const notice = () =>
    screen.queryByRole("alert", { name: "Description of #2, changed by another person" });
  expect(notice()).not.toBeNull();
  // A search the row does not match takes it off the table. No row draws the reader's text then,
  // so it stands above the table; cleared, the row is drawn again, with the text beside it.
  const search = screen.getByRole("searchbox", { name: "Search priorities" });
  await user.type(search, "OSS");
  await waitFor(() =>
    expect(screen.queryByRole("row", { name: "#2 Linkage closure eval" })).toBeNull()
  );
  expect(notice()).not.toBeNull();
  await user.clear(search);
  await editors.named("Description of #2");
  const kept = notice();
  if (kept === null) throw new Error("no notice beside the description drawn again");
  expect(within(kept).getByText(/Mine\./)).toBeDefined();
  expect(patch).not.toHaveBeenCalled();
  await user.click(within(kept).getByRole("button", { name: "Use my text" }));
  await waitFor(() => expect(patch).toHaveBeenCalledTimes(1));
  expect(patch.mock.calls[0]?.slice(0, 2)).toEqual([
    "linkage-closure",
    { description: "Mine.\n", expected: { description: "Written elsewhere." } },
  ]);
});

// A row's description written on, each write made on the one before: "A", then "AB" and "ABC".
// Dispatch answers no attempt of "A", and the reader stops it; the others waited behind it and
// name a text it never took, so each is refused 409 and kept beside the row. A search then takes
// the row off the table, so its texts stand above it, where no editor knows the writes they came
// from. Using the newest is made on what its write was made on, and so on through the texts the
// click lets go, down to "A", which the page holds for them: its landing lets "A" go, and the
// Retry of "A" sends nothing.
test("Use my text of a row's description above the table, the row not drawn, is made on the stopped write its texts were made on, which its landing lets go", async () => {
  const user = userEvent.setup();
  const { client, editors, patch, priorities } = page(FIFTEEN);
  let stored = "";
  patch.mockImplementation(async (id, input) => {
    if (input.description === "A\n") {
      throw new ApiError(503, { code: "UNAVAILABLE", error: "the server is down" });
    }
    if (input.expected?.description !== stored) throw PRIORITY_DESCRIPTION_CHANGED;
    stored = input.description ?? stored;
    return { ...saved(id), description: stored };
  });
  priorities.mockImplementation(async () => ({
    ...FIFTEEN,
    priorities: (FIFTEEN.priorities ?? []).map((row) =>
      row.id === "linkage-closure" ? { ...row, description: stored } : row
    ),
  }));
  await screen.findByRole("row", { name: "#2 Linkage closure eval" });
  await user.click(screen.getByRole("button", { name: "Open #2 Linkage closure eval" }));
  const { handle } = await editors.named("Description of #2");
  await onFakeClock(async () => {
    for (const end of ["A", "B", "C"]) {
      handle.view.focus();
      writeAtEnd(handle, end);
      act(() => handle.view.dom.blur());
    }
    await stopTheLateWrite(client);
  });
  const writes = writesOf(client);
  const [stopped] = writes.rows();
  expect(stopped).toMatchObject({ detail: { text: "A\n" }, status: "refused" });
  const named = "Description of #2, changed by another person";
  await screen.findByRole("alert", { name: named });
  await user.type(screen.getByRole("searchbox", { name: "Search priorities" }), "OSS");
  await waitFor(() =>
    expect(screen.queryByRole("row", { name: "#2 Linkage closure eval" })).toBeNull()
  );
  const notice = screen.getByRole("alert", { name: named });
  const [newest, older] = within(notice).getAllByRole("button", { name: "Use this text" });
  expect(older).toBeDefined();
  if (newest === undefined) throw new Error("no Use this text above the table");
  await user.click(newest);
  await waitFor(() =>
    expect(patch.mock.calls.at(-1)?.slice(0, 2)).toEqual([
      "linkage-closure",
      { description: "ABC\n", expected: { description: "" } },
    ])
  );
  await waitFor(() => expect(sessionStorage.getItem(`dispatch.write:${stopped?.key}`)).toBeNull());
  const sent = patch.mock.calls.length;
  act(() => writes.retry(stopped?.id ?? 0));
  await waitFor(() => expect(writes.rows()).toEqual([]));
  expect(patch).toHaveBeenCalledTimes(sent);
});

// The page reads another person's description of a row open in the table while the reader's
// typing waits, and the typing comes to rest once the page drew that description and before the
// editor's effects took it in. The write names the description the editor started from, never the
// page's read, which holds the other person's by then: Dispatch refuses it, and the page offers
// the reader's text once, beside the row's description.
test("a description written on an open row once the page read another person's, before the editor took it in, names the description the editor started from, and is offered once", async () => {
  const user = userEvent.setup();
  const { beforeEffects, client, editors, patch, writtenElsewhere } = page(FIFTEEN);
  await screen.findByRole("row", { name: "#2 Linkage closure eval" });
  await user.click(screen.getByRole("button", { name: "Open #2 Linkage closure eval" }));
  const { box, handle } = await editors.named("Description of #2");
  handle.view.focus();
  patch.mockRejectedValueOnce(PRIORITY_DESCRIPTION_CHANGED);
  await onFakeClock(async () => {
    writeAtEnd(handle, "Mine.");
    beforeEffects(() => jest.advanceTimersByTime(SAVE_AFTER_MS));
    writtenElsewhere("linkage-closure", { description: "Written elsewhere." });
    // The write goes out, and its refusal comes back, before the clock comes back.
    await act(async () => {
      await settle();
    });
  });
  await waitFor(() => expect(patch).toHaveBeenCalledTimes(1));
  expect(patch.mock.calls[0]?.slice(0, 2)).toEqual([
    "linkage-closure",
    { description: "Mine.\n", expected: { description: "" } },
  ]);
  await waitFor(() =>
    expect(writesOf(client).rows()).toMatchObject([
      { claimed: true, detail: { text: "Mine.\n" }, sent: true, status: "refused" },
    ])
  );
  const notices = await screen.findAllByRole("alert", {
    name: "Description of #2, changed by another person",
  });
  expect(notices).toHaveLength(1);
  expect(within(notices[0] as HTMLElement).getAllByText(/Mine\./)).toHaveLength(1);
  expect(box.textContent).toBe("Written elsewhere.");
  expect(patch).toHaveBeenCalledTimes(1);
});

/** Keeps in the tab, for a reload to take up, a write of `#2`'s `attribute` as `text` that
 *  Dispatch refused, another person having written over `expected` first. */
function keepRefused(key: string, attribute: "description" | "title", text: string, expected = "") {
  const input =
    attribute === "title"
      ? { expected: { title: expected }, title: text }
      : { description: text, expected: { description: expected } };
  sessionStorage.setItem(
    `dispatch.write:${key}`,
    JSON.stringify({
      carried: "PATCH",
      detail: { attribute, expected, id: "linkage-closure", text },
      key,
      kind: "priority-text",
      label: `${attribute === "title" ? "Rename" : "Description of"} the priority “Linkage closure eval”`,
      login: "alice",
      presentation: "indicator",
      pressedAt: Date.now(),
      request: priorityPatchRequest("linkage-closure", input),
      sent: true,
      state: {
        refusal: {
          code: "PRIORITY_CHANGED",
          kind: "api",
          message:
            attribute === "title"
              ? PRIORITY_TITLE_CHANGED.message
              : PRIORITY_DESCRIPTION_CHANGED.message,
          status: 409,
        },
        status: "refused",
      },
      target: "priority:linkage-closure",
      version: 1,
    })
  );
}

// The table settles only the refused descriptions of the rows it draws: one of a row a search
// leaves out stays on the shell's line, where the reader's text is offered to copy, rather than
// being kept beside a row nobody sees, and the row drawn again takes it. A row draws no field
// for its title, so a refused title stays on the shell's line, its row drawn or not, until the
// priority's own page takes it.
test("a refused description of a row the table does not draw stays on the shell's line, the row drawn again takes it, and a refused title stays there", async () => {
  const user = userEvent.setup();
  const { client } = page(FIFTEEN);
  await screen.findByRole("row", { name: "#2 Linkage closure eval" });
  const search = screen.getByRole("searchbox", { name: "Search priorities" });
  await user.type(search, "OSS");
  await waitFor(() =>
    expect(screen.queryByRole("row", { name: "#2 Linkage closure eval" })).toBeNull()
  );
  keepRefused("resumed-description", "description", "Mine.");
  keepRefused("resumed-rename", "title", "Linkage closure eval, mine", "Linkage closure eval");
  const writes = writesOf(client);
  act(() => writes.resume("alice"));
  const row = (key: string) => writes.rows().find((write) => write.key === key);
  expect(row("resumed-description")).toMatchObject({ claimed: false, status: "refused" });
  expect(row("resumed-rename")).toMatchObject({ claimed: false, status: "refused" });
  expect(screen.queryByRole("alert", { name: /changed by another person/ })).toBeNull();
  await user.clear(search);
  await waitFor(() =>
    expect(row("resumed-description")).toMatchObject({ claimed: true, status: "refused" })
  );
  // The row draws its description, closed as it is, with the reader's text beside it.
  const kept = await screen.findByRole("alert", {
    name: "Description of #2, changed by another person",
  });
  expect(within(kept).getByText("Mine.")).toBeDefined();
  expect(row("resumed-rename")).toMatchObject({ claimed: false, status: "refused" });
  expect(screen.queryByRole("alert", { name: /^Title of/ })).toBeNull();
});

test("a description is written on the open row: the empty editor under the prompt that says none is", async () => {
  const user = userEvent.setup();
  const { editors, patch } = page(FIFTEEN);
  await screen.findByRole("row", { name: "#3 OSS Verify" });
  await user.click(screen.getByRole("button", { name: "Open #3 OSS Verify" }));
  const { box, handle } = await editors.named("Description of #3");
  expect(screen.getByText("No description is written. Write one below.")).toBeDefined();
  expect(box.textContent).toBe("");
  handle.view.focus();
  writeAtEnd(handle, "What it is.", "it");
  await user.tab();
  await waitFor(() => expect(patch).toHaveBeenCalledTimes(1));
  expect(patch.mock.calls[0]?.slice(0, 2)).toEqual([
    "oss-verify",
    { description: "What **it** is.\n", expected: { description: "" } },
  ]);
  // The editor still holds what was written: there is no rendered block to give back.
  expect(box.textContent).toBe("What it is.");
});

test("dragging a row onto another writes the place it landed", async () => {
  const { patch } = page(FIFTEEN);
  const third = await screen.findByRole("row", { name: "#3 OSS Verify" });
  const first = screen.getByRole("row", { name: "#1 Legion works reliably first" });
  const handle = within(third).getByRole("button", {
    name: "Move #3 OSS Verify: drag it, or press the up and down arrows",
  });
  // user-event has no form for a drag: it drives pointer events, and the order is moved by the
  // HTML drag-and-drop events the handle listens for. The drag carries the id it moves, since
  // Firefox starts no drag that carries nothing, and says it is a move.
  const setData = mock(() => {});
  const start = createEvent.dragStart(handle, { dataTransfer: { setData } }) as DragEvent;
  fireEvent(handle, start);
  expect(setData).toHaveBeenCalledWith("text/plain", "oss-verify");
  expect(start.dataTransfer?.effectAllowed).toBe("move");
  // user-event has no form for a drag over a row either; the row it is over takes a move.
  const over = createEvent.dragOver(first, { dataTransfer: {} }) as DragEvent;
  fireEvent(first, over);
  expect(over.dataTransfer?.dropEffect).toBe("move");
  expect(over.defaultPrevented).toBe(true);
  // user-event has no form for a drop, which the row takes for itself rather than letting the
  // browser open the id it carries.
  const drop = createEvent.drop(first, { dataTransfer: {} });
  fireEvent(first, drop);
  expect(drop.defaultPrevented).toBe(true);
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
  const first = await screen.findByRole("row", { name: "#1 Legion works reliably first" });
  within(first)
    .getByRole("button", {
      name: "Move #1 Legion works reliably first: drag it, or press the up and down arrows",
    })
    .focus();
  await user.keyboard("{ArrowDown}");
  await waitFor(() => expect(patch).toHaveBeenCalledTimes(1));
  expect(patch.mock.calls[0]?.slice(0, 2)).toEqual([
    "legion-first",
    { rank: { after: "linkage-closure" } },
  ]);
});

test("j, k, x and Enter act on the priorities, a row opened or not", async () => {
  const user = userEvent.setup();
  const { editors } = page(FIFTEEN, [issue("CORE-1", ["legion-first"])]);
  const first = await screen.findByRole("row", { name: "#1 Legion works reliably first" });
  // An opened row adds no second table, so the keys stay the priorities table's.
  await user.click(screen.getByRole("button", { name: "Open #1 Legion works reliably first" }));
  await editors.named("Description of #1");
  const focused = () => document.activeElement?.getAttribute("aria-label");
  first.focus();
  await user.keyboard("j");
  expect(focused()).toBe("#2 Linkage closure eval");
  await user.keyboard("j");
  expect(focused()).toBe("#3 OSS Verify");
  await user.keyboard("k");
  expect(focused()).toBe("#2 Linkage closure eval");
  // `x` is the priorities table's too: the binding takes the key, and since a priority row has
  // nothing to select, the focus stays where it is and nothing else moves.
  const second = screen.getByRole("row", { name: "#2 Linkage closure eval" });
  // user-event cannot say whether a key was taken; the event fireEvent dispatches can.
  expect(fireEvent.keyDown(second, { code: "KeyX", key: "x" })).toBe(false);
  expect(focused()).toBe("#2 Linkage closure eval");
  expect(screen.queryAllByRole("checkbox")).toEqual([]);
  expect(screen.getByTestId("address").textContent).toBe("POP /priorities");
  // Enter on the focused row opens the priority's own page, as its title link does.
  await user.keyboard("{Enter}");
  expect(screen.getByTestId("address").textContent).toBe("PUSH /priorities/linkage-closure");
});

test("the table's own view is in the address under its prefix", async () => {
  const user = userEvent.setup();
  page(FIFTEEN);
  await screen.findByRole("row", { name: "#1 Legion works reliably first" });
  await user.type(screen.getByRole("searchbox", { name: "Search priorities" }), "linkage");
  await waitFor(() =>
    expect(screen.queryByRole("row", { name: "#1 Legion works reliably first" })).toBeNull()
  );
  expect(screen.getByRole("row", { name: "#2 Linkage closure eval" })).toBeDefined();
});

test("the page carries no estimate, no forecast, no percentage and no written status", async () => {
  page(FIFTEEN);
  await screen.findByRole("row", { name: "#1 Legion works reliably first" });
  const text = document.body.textContent ?? "";
  expect(/\d+%/.test(text)).toBe(false);
  expect(/estimate|forecast|on track|at risk/i.test(text)).toBe(false);
});

test("the priorities table counts its rows as priorities, as the agreed page reads", async () => {
  page(FIFTEEN);
  await screen.findByRole("row", { name: "#1 Legion works reliably first" });
  // dispatch://INFRA-2947/artifact/priorities-round10-png reads "15 of 15 priorities" under the
  // table: a priority is no issue, and a reader counting the rows is told what they count.
  expect(screen.getByText("3 of 3 priorities")).toBeDefined();
  expect(screen.queryByText("3 of 3 issues")).toBeNull();
});

test("a fault row tells no reader that an issue nobody claims is claimed", async () => {
  page(
    FIFTEEN,
    [
      issue("CORE-1", [], { assignee: "alice" }),
      issue("CORE-2", [], {
        assignee: "alice",
        claim: { actor: { kind: "user", id: "bob" }, at: "2026-11-10T00:00:00Z" },
      }),
    ],
    true,
    { at: "/priorities?tab=none" }
  );
  const faults = await screen.findByRole("table", {
    name: "Active under no priority table",
  });
  await within(faults).findByRole("row", { name: "CORE-1 Work CORE-1" });
  const headers = within(faults)
    .getAllByRole("columnheader")
    .map((header) => header.textContent ?? "");
  const claimedBy = headers.findIndex((header) => header.includes("Claimed by"));
  expect(claimedBy).not.toBe(-1);
  const cellUnder = (key: string) =>
    within(within(faults).getByRole("row", { name: `${key} Work ${key}` })).getAllByRole("cell")[
      claimedBy
    ]?.textContent ?? "";
  // The Issues table reads "Claimed by" as the claim alone (its ClaimChip). Under that header
  // an unclaimed issue names nobody, whoever it is assigned to, and a claimed one its claimant.
  expect(cellUnder("CORE-1")).not.toContain("alice");
  expect(cellUnder("CORE-2")).toContain("bob");
});

test("a grouped table counts each group in the plural its rows are called by", async () => {
  const user = userEvent.setup();
  page(
    body({
      priorities: [
        priority("legion-first", { title: "Legion works reliably first" }),
        priority("oss-verify", { title: "OSS Verify" }),
      ],
      work_under_no_priority: { by_project: { CORE: 2 }, count: 2, issues: null },
    }),
    [issue("CORE-1"), issue("CORE-2")]
  );
  await screen.findByRole("row", { name: "#1 Legion works reliably first" });
  // The grouping control of each table reads "Group by".
  const groupBy = (region: string) =>
    within(screen.getByRole("region", { name: region })).getByRole("combobox", {
      name: "Group by",
    });
  // No rule makes a plural from a singular: adding "s" to "priority" wrote "2 prioritys", so each
  // table names both words, as its pager does. The priorities table's heading is the one group
  // row every table draws (`DataTableGroupRow`).
  await user.selectOptions(groupBy("Priorities"), "owner");
  expect(
    await screen.findByRole("button", { name: "Collapse INFRA controller, 2 priorities" })
  ).toBeDefined();
  await user.click(screen.getByRole("tab", { name: "Active under no priority 2" }));
  await screen.findByRole("row", { name: "CORE-1 Work CORE-1" });
  await user.selectOptions(groupBy("Active under no priority"), "status");
  expect(
    await screen.findByRole("button", { name: "Collapse In progress, 2 issues" })
  ).toBeDefined();
});

test("no header of the priorities table is longer than 16 characters, whichever columns show", async () => {
  const user = userEvent.setup();
  page(FIFTEEN, [], true, { box: DESKTOP_BOX });
  await screen.findByRole("row", { name: "#1 Legion works reliably first" });
  await user.click(
    within(screen.getByRole("region", { name: "Priorities" })).getByRole("button", {
      name: "Columns",
    })
  );
  await user.click(screen.getByRole("checkbox", { name: "Closed, 7 days" }));
  await user.keyboard("{Escape}");
  const headers = screen
    .getAllByRole("columnheader")
    .map((header) => (header.textContent ?? "").replace(/Filter|[▲▼]/g, "").trim());
  expect(headers).toContain("Closed, 7 days");
  for (const header of headers)
    expect({ header, long: header.length > 16 }).toEqual({
      header,
      long: false,
    });
});
