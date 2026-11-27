/** The ten pages of INFRA-4948's R8 (eight of them INFRA-3135's R2), as a browser row drives
 *  them, and the readings R1, R2, R4 and R8 are made with. Shared by `table-controls.e2e.ts` and
 *  the rows of that family which run against a recording (`table-controls.stub.e2e.ts`), so the
 *  two files name one page, one first row and one narrowing each, and a page whose controls move
 *  is edited here once.
 *
 *  Nothing here asserts but R8's readings of the bar at the end of the file, which the rows of
 *  `k-one-row`, `k-bar` and `k-no-growth` in both files run page by page, so a page reads one way
 *  in either. Every other function hands back what a row reads, and the row says what it expects
 *  of it; the one exception is the waiting each opener does, which is how it knows the surface is
 *  drawn at all.
 */
import { expect, type Locator, type Page } from "@playwright/test";

import { CONTROL, type ControlName, NARROW_BELOW } from "../web/src/components/control-names";
import { shownAgentRows } from "./agents";
import {
  createAsk,
  createIssue,
  createIssueArtifact,
  createPriority,
  createProject,
  patchIssue,
} from "./api";
import { control, controlRow, openDisplayMenu, openFilterPanel } from "./filters";
import { nextFrames } from "./frames";
import { drawer, peekTrigger } from "./inbox-drawer";
import { seedCommentThread } from "./inbox-threads";
import { expandedState, settledState } from "./settled";
import { assertPageFits } from "./unclipped";

/** The ten pages R8 names, spelled as a failure spells them. */
export type SurfaceName =
  | "Inbox"
  | "Inbox drawer"
  | "project List"
  | "project Board"
  | "Agents page"
  | "Delivery page"
  | "search bar"
  | "Artifacts tab"
  | "priorities page"
  | "priority page";

/** The eight a recording can answer: the Agents page and the Delivery page are read on the real
 *  stack alone, because a recording freezes a session's last-seen time and the delivery
 *  window's dates (`packages/dispatch/README.md`, "The stubbed layer"). */
export const STUBBABLE_SURFACES: readonly SurfaceName[] = [
  "Inbox",
  "Inbox drawer",
  "project List",
  "project Board",
  "search bar",
  "Artifacts tab",
  "priorities page",
  "priority page",
];

/** The two the real stack reads. */
export const REAL_STACK_SURFACES: readonly SurfaceName[] = ["Agents page", "Delivery page"];

/**
 * The `data-control` value of every control each page draws, and nothing else: the control bar's
 * slots the Design's table "The pages" gives the page (R8), and the page's other shared controls
 * - its table, its pager, its column filters and its selection bar. A row reads that each of these
 * is drawn and that no other value of the vocabulary is.
 *
 * The Inbox's search box stands in its header line, beside the title, and the bar holds the
 * switch, **Filter**, the chips, **View**, **More** with **Mark all read**, and **New message**
 * (a `bar-action`, drawn filled). An issue's Artifacts tab draws its actions as `bar-action`s
 * where its bar is 640 px wide or wider and as items of **More** below. The priorities page
 * draws the priorities table's bar, with no trigger, and the project List's bar above each issues
 * table; a priority's own page draws the project List's bar above its issues table.
 *
 * The Delivery page's measures table, which R8 keeps and which draws the shared table markup,
 * is not here and is no exception to it: that table is inside a fold the page opens shut
 * (`MeasuresPanel.tsx`, "Definitions & alternatives"), so a reader who opens `/delivery` meets
 * no `table-shell` at all. It is read where it belongs, among the tables that offer no
 * controls.
 */
export const SURFACE_CONTROLS: Record<SurfaceName, readonly ControlName[]> = {
  Inbox: [
    CONTROL.controlRow,
    CONTROL.searchBox,
    CONTROL.segmentedControl,
    CONTROL.panelTrigger,
    CONTROL.chip,
    CONTROL.displayMenu,
    CONTROL.moreMenu,
    CONTROL.barAction,
    CONTROL.selectionBar,
  ],
  "Inbox drawer": [
    CONTROL.controlRow,
    CONTROL.searchBox,
    CONTROL.segmentedControl,
    CONTROL.panelTrigger,
    CONTROL.chip,
    CONTROL.displayMenu,
    CONTROL.moreMenu,
    CONTROL.barAction,
    CONTROL.selectionBar,
  ],
  "project List": [
    CONTROL.controlRow,
    CONTROL.searchBox,
    CONTROL.panelTrigger,
    CONTROL.chip,
    CONTROL.displayMenu,
    CONTROL.columnsMenu,
    CONTROL.columnFilter,
    CONTROL.pager,
    CONTROL.selectionBar,
    CONTROL.dataTable,
  ],
  "project Board": [CONTROL.controlRow, CONTROL.searchBox, CONTROL.panelTrigger, CONTROL.chip],
  "Agents page": [CONTROL.controlRow, CONTROL.searchBox, CONTROL.panelTrigger, CONTROL.chip],
  "Delivery page": [
    CONTROL.controlRow,
    CONTROL.searchBox,
    CONTROL.panelTrigger,
    CONTROL.chip,
    CONTROL.columnFilter,
    CONTROL.dataTable,
  ],
  "search bar": [CONTROL.controlRow, CONTROL.searchBox, CONTROL.panelTrigger, CONTROL.chip],
  "Artifacts tab": [CONTROL.controlRow, CONTROL.searchBox, CONTROL.barAction, CONTROL.moreMenu],
  "priorities page": [
    CONTROL.controlRow,
    CONTROL.searchBox,
    CONTROL.chip,
    CONTROL.columnsMenu,
    CONTROL.displayMenu,
    CONTROL.barAction,
    CONTROL.columnFilter,
    CONTROL.pager,
    CONTROL.moreMenu,
    CONTROL.dataTable,
  ],
  "priority page": [
    CONTROL.controlRow,
    CONTROL.searchBox,
    CONTROL.panelTrigger,
    CONTROL.chip,
    CONTROL.displayMenu,
    CONTROL.columnsMenu,
    CONTROL.columnFilter,
    CONTROL.pager,
    CONTROL.selectionBar,
    CONTROL.dataTable,
  ],
};

/** The three controls R1 refuses between a page's title and its first row, outside the one row. */
export const STRIP_CONTROLS: readonly ControlName[] = [
  CONTROL.searchBox,
  CONTROL.panelTrigger,
  CONTROL.chip,
];

/** What the openers need of the seed: the project both project pages read, the issue whose
 *  Artifacts tab is read, the priority whose own page is read, and the query `/delivery` is
 *  opened on, where the caller has one. A scene recorded before the priority was seeded names
 *  none, and its priority page is opened on nothing. */
export interface SurfaceScene {
  readonly project: string;
  readonly issue: string;
  readonly priority?: string;
  readonly deliveryQuery?: string;
}

/** The session that asks the seed's questions, so each ask waits on the issue's own side and
 *  the reader's Inbox lists it. */
export const SURFACE_SESSION = {
  actor: { id: "e2e-table-controls", kind: "session" as const },
  as: "agent" as const,
};

/** The word the search bar is driven with: it is in the title of two seeded issues and in the
 *  body of one seeded artifact, so the bar always has hits to list. */
export const SURFACE_SEARCH_TERM = "rollout";

/** The priority the seed writes. */
const SURFACE_PRIORITY = { title: "Keep the books whole" } as const;

/** The one issue that advances it, in a project of its own, and a word of its title the priority's
 *  own page is narrowed by. */
const SURFACE_PRIORITY_WORK = { title: "Audit the books", word: "audit" } as const;

/** The issue in flight under no priority, and a word of its title its fault tab is narrowed by. */
const SURFACE_LOOSE_WORK = { title: "Ledger cleanup", word: "Ledger" } as const;

/** The priority's work parked in backlog, and a word of its title its fault tab is narrowed by. */
const SURFACE_PARKED_WORK = { title: "File the audit notes", word: "audit" } as const;

/**
 * One project of six issues, every one of them holding an open ask and a label, and three
 * artifacts on the first: enough for each of the stubbable surfaces to have a list with a first
 * row, a narrowing that takes no row off it, and something for the search bar to find. Beside it,
 * one priority and one issue in flight that advances it, in a project of its own, so the
 * priorities page lists a row and the priority's own page an issue, and no page of CORE - its
 * List, its Board, the Inbox, the search bar's hits for `rollout` - draws anything more.
 *
 * Every issue of CORE holds an ask deliberately: **Needs you** is the one narrowing the project
 * List and the Board both draw a chip for, and a seed where it emptied the list would leave R1's
 * reading with no first row to measure to.
 */
export async function seedSurfaces(): Promise<SurfaceScene> {
  // A row that seeds the Agents fixture beside this one has already made CORE, and the server
  // refuses a second one with `PROJECT_EXISTS`: the project is what both seeds need, not what
  // either of them owns.
  await createProject({ key: "CORE", name: "Core" }).catch((cause: unknown) => {
    if (!String(cause).includes("PROJECT_EXISTS")) throw cause;
  });
  const titles = [
    "Rollout order",
    "Rollout budget",
    "Ledger mirror",
    "Clock drift",
    "Replay window",
    "Lease handover",
  ];
  const keys: string[] = [];
  for (const [index, title] of titles.entries()) {
    const issue = await createIssue({ project: "CORE", title });
    await patchIssue(issue.key, {
      labels: index % 2 === 0 ? ["frontend"] : ["backend"],
      status: "todo",
    });
    // One unread thread on the first issue, so the Inbox offers Mark all read in its bar's More.
    // It is the oldest row, so the Inbox's first row is an ask: a row a surface selects opens in
    // the reading column, and a thread opened there is read, which would take Mark all read away.
    if (index === 0) {
      await seedCommentThread(
        issue.key,
        "Is the mirror still nightly?",
        "It moved to the weekend."
      );
    }
    await createAsk(issue.key, { question: `${title}: what is the rule?` }, SURFACE_SESSION);
    keys.push(issue.key);
  }
  const issue = keys[0] ?? "";
  for (const name of ["plan.md", "notes.md", "budget.md"]) {
    await createIssueArtifact(issue, {
      content: `# ${name}\n\nThe rollout note this file holds.\n`,
      name,
    });
  }
  await createProject({ key: "BOOKS", name: "Books" }).catch((cause: unknown) => {
    if (!String(cause).includes("PROJECT_EXISTS")) throw cause;
  });
  const priority = await createPriority({ owner: "alice", title: SURFACE_PRIORITY.title });
  const work = await createIssue({ project: "BOOKS", title: SURFACE_PRIORITY_WORK.title });
  await patchIssue(work.key, { advances: [priority.id], status: "in_progress" });
  // And one in flight under no priority, so the priorities page's issues table has a row to mark.
  const loose = await createIssue({ project: "BOOKS", title: SURFACE_LOOSE_WORK.title });
  await patchIssue(loose.key, { status: "in_progress" });
  const parked = await createIssue({ project: "BOOKS", title: SURFACE_PARKED_WORK.title });
  await patchIssue(parked.key, { advances: [priority.id], status: "backlog" });
  return { issue, priority: priority.id, project: "CORE" };
}

/** One surface, open and drawn, with the acts R1 measures across. */
export interface Surface {
  readonly name: SurfaceName;
  /** What holds this surface's own controls and list: `<main>`, or the dialog the drawer and
   *  the search bar draw in. Every reading is scoped to it, so the page under a drawer is no
   *  part of the drawer's reading and the drawer is no part of the page's. */
  readonly root: Locator;
  /** The one row of controls. */
  readonly row: Locator;
  /** The first row of the surface's list or table. */
  readonly firstRow: Locator;
  /** What the strip is measured from: the page's title, or - for the drawer and the search
   *  bar, which have none - the surface's own top. */
  readonly head: Locator;
  /** Puts one narrowing on, leaving no panel open. */
  readonly narrow: () => Promise<void>;
  /** Opens the surface's filter panel, or undefined where it draws none. */
  readonly panel?: () => Promise<void>;
  /** Opens the surface's display menu, or undefined where it draws none. */
  readonly display?: () => Promise<void>;
  /** Marks one row, or undefined where the surface allows no selection. */
  readonly select?: () => Promise<void>;
}

/** The two desktop widths R1 is read at. */
export const DESKTOP_WIDTHS: readonly { height: number; width: number }[] = [
  { height: 900, width: 1440 },
  { height: 800, width: 1280 },
];

/** The Inbox's own ceiling: the first item starts this far down the window or less, with no
 *  narrowing on, with a panel open and with the display menu open. The New message control in its
 *  control row adds no height under it (`e2e/inbox-compose.e2e.ts`). */
export const INBOX_TOP = 203;

/** Whether the row is running in one of the suite's phone projects (`iphone`, `stub-phone`),
 *  where R1's third width is that project's own and the row sets no viewport of its own. */
export function isPhoneProject(testInfo: { project: { name: string } }): boolean {
  return testInfo.project.name.includes("phone");
}

/** The sizes one project reads R1 at: a phone project its own viewport, a desktop one the two
 *  widths R1 names. */
export function widthsOf(
  page: Page,
  testInfo: { project: { name: string } }
): { height: number; width: number }[] {
  if (!isPhoneProject(testInfo)) return [...DESKTOP_WIDTHS];
  const viewport = page.viewportSize() ?? { height: 664, width: 390 };
  return [viewport];
}

/** The top of a box in the viewport, as `getBoundingClientRect()` reads it. */
export async function topOf(locator: Locator): Promise<number> {
  return locator.evaluate((node) => node.getBoundingClientRect().top);
}

/** The height of a box, as `getBoundingClientRect()` reads it. */
export async function heightOf(locator: Locator): Promise<number> {
  return locator.evaluate((node) => node.getBoundingClientRect().height);
}

/** The `data-control` values `root` draws, each once and sorted: what a row compares against
 *  the design's own list for that page. */
export async function drawnControlNames(root: Locator): Promise<string[]> {
  const found = await root.evaluate((node) =>
    [...node.querySelectorAll("[data-control]")].map(
      (element) => element.getAttribute("data-control") ?? ""
    )
  );
  return [...new Set(found)].sort();
}

/** The controls R1 refuses that are drawn between `top` and `bottom` and sit outside the one
 *  control row: the reading of "no other element between the page's title and the first row of
 *  its list draws a search box, a panel trigger or a chip". Each is described by its value and
 *  its accessible name. */
export async function straysBetween(root: Locator, top: number, bottom: number): Promise<string[]> {
  return root.evaluate(
    (node, [from, to, wanted]) =>
      [...node.querySelectorAll<HTMLElement>(wanted.map((n) => `[data-control="${n}"]`).join(","))]
        .filter((element) => element.closest('[data-control="control-row"]') === null)
        .filter((element) => {
          const box = element.getBoundingClientRect();
          return box.height > 0 && box.top >= from - 0.5 && box.top < to;
        })
        .map(
          (element) =>
            `${element.getAttribute("data-control")} "${(
              element.getAttribute("aria-label") ??
              element.textContent ??
              ""
            )
              .trim()
              .slice(0, 48)}" at ${Math.round(element.getBoundingClientRect().top)}px`
        ),
    [top, bottom, [...STRIP_CONTROLS]] as [number, number, string[]]
  );
}

/** The outermost boxes `root` draws between `top` and `bottom`, each with its height and the
 *  first words it carries: what a failure of R1's rule needs beside the two numbers, because
 *  the question it leaves open is which box took the room. A box whose parent is already
 *  wholly inside the gap is left out, so the answer is one line per thing in the gap rather
 *  than one per node. */
export async function gapBoxes(root: Locator, top: number, bottom: number): Promise<string[]> {
  return root.evaluate(
    (node, [from, to]) => {
      const inside = (element: Element): boolean => {
        const box = element.getBoundingClientRect();
        return box.height > 0 && box.top >= from - 0.5 && box.bottom <= to + 0.5;
      };
      return [...node.querySelectorAll("*")]
        .filter((element) => inside(element))
        .filter((element) => element.parentElement === null || !inside(element.parentElement))
        .map(
          (element) =>
            `${element.tagName.toLowerCase()} ${Math.round(element.getBoundingClientRect().height)}px "${(element.textContent ?? "").trim().slice(0, 40)}"`
        );
    },
    [top, bottom] as [number, number]
  );
}

/** The surface's own display menu, opened unless it is open already: the trigger is found inside
 *  this surface's row, so a drawer over a page that draws one of its own opens the drawer's, and
 *  among **More**'s items where the bar holds it there (`openDisplayMenu`). */
export async function openDisplay(page: Page, within: Locator | Page): Promise<Locator> {
  return openDisplayMenu(page, "View", within);
}

/**
 * Shuts the surface's own filter panel by pressing its trigger again, which is what a reader
 * does and the one way that is safe everywhere: `AnchoredPanel` takes Escape only while the
 * focus is inside the panel or on its anchor, so an Escape pressed after a pick whose control
 * moved the focus elsewhere travels on to the layer behind - on the search bar, to the bar
 * itself, which then closes.
 */
export async function closeControlPanel(within: Locator | Page): Promise<void> {
  const trigger = control(controlRow(within), CONTROL.panelTrigger).first();
  if ((await expandedState("the row's filter trigger", trigger)) === "open") await trigger.click();
  await expect(trigger).toHaveAttribute("aria-expanded", "false");
}

/** Ticks one value of one of the Inbox's four fields, from the panel its one trigger opens, and
 *  leaves neither the popover nor the panel open. `within` is the surface whose row holds the
 *  trigger: the page, or the drawer. */
export async function pickInboxField(
  page: Page,
  field: string,
  option: RegExp,
  within: Locator | Page = page
): Promise<void> {
  await openFilterPanel(page, within);
  const trigger = page.getByRole("button", { name: `Filter by ${field}` });
  if ((await expandedState(`the ${field} filter`, trigger)) === "shut") await trigger.click();
  await page.getByRole("option", { name: option }).first().click();
  // The popover stays open on a pick, as a multi-select does; its own trigger shuts it.
  await trigger.click();
  await expect(trigger).toHaveAttribute("aria-expanded", "false");
  await closeControlPanel(within);
}

/** Presses one of the chips the project List's and the Board's shared filter panel holds -
 *  **Needs you**, **Unread** or **Unclaimed** - and leaves the panel shut. */
export async function pickIssueFlag(page: Page, flag: string): Promise<void> {
  await openFilterPanel(page);
  await page.getByRole("dialog", { name: "Filters" }).getByRole("button", { name: flag }).click();
  await closeControlPanel(page);
}

/** Puts every box the surface scrolls in back at the top, so a reading of a box's place in the
 *  viewport is a reading of the layout and not of where the page happens to be scrolled: a
 *  panel that takes the focus scrolls the window or the list to it. */
export async function resetScroll(page: Page, firstRow: Locator): Promise<void> {
  await page.evaluate(() => window.scrollTo(0, 0));
  await firstRow.evaluate((node) => {
    for (let box = node.parentElement; box !== null; box = box.parentElement) {
      if (box.scrollTop !== 0) box.scrollTop = 0;
    }
  });
}

/** The Inbox, at `/`, with its first row drawn. */
async function openInbox(page: Page): Promise<Surface> {
  const root = page.getByTestId("main-content");
  await page.goto("/");
  const firstRow = page.locator("[data-inbox-row]").first();
  await expect(firstRow).toBeVisible();
  return {
    name: "Inbox",
    root,
    row: controlRow(root),
    firstRow,
    head: root.getByRole("heading", { level: 1, name: "Inbox" }),
    // A narrowing every seeded row passes, the unread thread too, so Mark all read stays offered
    // in More under it.
    narrow: () => pickInboxField(page, "project", /^CORE,/),
    panel: async () => {
      await openFilterPanel(page, root);
    },
    display: async () => {
      await openDisplay(page, root);
    },
    select: async () => {
      await firstRow.getByRole("checkbox").first().click();
      await expect(control(root, CONTROL.selectionBar)).toBeVisible();
    },
  };
}

/** Opens the project's Issues tab on the view named, whichever of the two the reader's stored
 *  preference (`project.issue-view`) was left on by an earlier surface. */
export async function openIssuesTab(
  page: Page,
  project: string,
  view: "List" | "Board"
): Promise<void> {
  await page.goto(`/projects/${project}/issues`);
  const button = page.getByRole("button", { exact: true, name: view });
  await expect(button).toBeVisible();
  const shown = await settledState(`the ${view} switch`, [
    ["pressed", button.and(page.locator('[aria-pressed="true"]'))],
    ["unpressed", button.and(page.locator('[aria-pressed="false"]'))],
  ]);
  if (shown === "unpressed") await button.click();
  await expect(button).toHaveAttribute("aria-pressed", "true");
}

/** Shows or hides the project Board's lanes by the header's switch, whichever state the reader's
 *  stored preference left them in, and waits until the switch offers the other state. */
export async function setBoardLanes(page: Page, lanes: "hidden" | "shown"): Promise<void> {
  const showLanes = page.getByRole("button", { name: "Show lanes" });
  const hideLanes = page.getByRole("button", { name: "Hide lanes" });
  const now = await settledState("the Board's lanes switch", [
    ["shown", hideLanes],
    ["hidden", showLanes],
  ]);
  if (now !== lanes) await (now === "shown" ? hideLanes : showLanes).click();
  await expect(lanes === "shown" ? hideLanes : showLanes).toBeVisible();
}

/** The Inbox drawer, peeked over the project List, which is the page R5 reads it over. */
async function openInboxDrawer(page: Page, scene: SurfaceScene): Promise<Surface> {
  await openIssuesTab(page, scene.project, "List");
  await expect(page.locator("[data-issue-row]").first()).toBeVisible();
  await peekTrigger(page).click();
  const root = drawer(page);
  const firstRow = root.locator("[data-inbox-row]").first();
  await expect(firstRow).toBeVisible();
  return {
    name: "Inbox drawer",
    root,
    row: controlRow(root),
    firstRow,
    // No title of its own, so the drawer is measured from its own top.
    head: root,
    narrow: () => pickInboxField(page, "project", /^CORE,/, root),
    panel: async () => {
      await openFilterPanel(page, root);
    },
    display: async () => {
      await openDisplay(page, root);
    },
    select: async () => {
      await firstRow.getByRole("checkbox").first().click();
      await expect(control(root, CONTROL.selectionBar)).toBeVisible();
    },
  };
}

/** The project's issue List. */
async function openProjectList(page: Page, scene: SurfaceScene): Promise<Surface> {
  const root = page.getByTestId("main-content");
  await openIssuesTab(page, scene.project, "List");
  const firstRow = page.locator("[data-issue-row]").first();
  await expect(firstRow).toBeVisible();
  return {
    name: "project List",
    root,
    row: controlRow(root),
    firstRow,
    head: root.getByRole("heading", { level: 1 }),
    // Every seeded issue holds an open ask, so this narrowing keeps every row and the list
    // still has a first row to measure to.
    narrow: () => pickIssueFlag(page, "Needs you"),
    panel: async () => {
      await openFilterPanel(page, root);
    },
    display: async () => {
      await openDisplay(page, root);
    },
    select: async () => {
      await firstRow.getByRole("checkbox").first().click();
      await expect(control(root, CONTROL.selectionBar)).toBeVisible();
    },
  };
}

/** The project's Board, reached by the header's own List/Board switch. */
async function openProjectBoard(page: Page, scene: SurfaceScene): Promise<Surface> {
  const root = page.getByTestId("main-content");
  await openIssuesTab(page, scene.project, "Board");
  const firstRow = page.locator("[data-board-card]").first();
  await expect(firstRow).toBeVisible();
  return {
    name: "project Board",
    root,
    row: controlRow(root),
    firstRow,
    head: root.getByRole("heading", { level: 1 }),
    narrow: () => pickIssueFlag(page, "Needs you"),
    panel: async () => {
      await openFilterPanel(page, root);
    },
  };
}

/** The Agents page, read on the real stack alone. */
async function openAgentsPage(page: Page): Promise<Surface> {
  const root = page.getByTestId("main-content");
  await page.goto("/agents");
  await expect(root.getByRole("heading", { level: 1, name: "Agents" })).toBeVisible();
  const firstRow = shownAgentRows(page).first();
  await expect(firstRow).toBeVisible();
  return {
    name: "Agents page",
    root,
    row: controlRow(root),
    firstRow,
    head: root.getByRole("heading", { level: 1, name: "Agents" }),
    // Both seeded sessions are on one machine, so this narrowing takes no row off the list.
    narrow: async () => {
      await openFilterPanel(page, root);
      const trigger = page.getByRole("button", { name: "Filter by machine" });
      if ((await expandedState("the machine filter", trigger)) === "shut") await trigger.click();
      // The popover's own options: the page's window control is a `select`, whose options
      // carry the same role and are never visible.
      await page.getByRole("listbox").getByRole("option").first().click();
      await trigger.click();
      await closeControlPanel(root);
    },
    panel: async () => {
      await openFilterPanel(page, root);
    },
  };
}

/** The Delivery page's pull-request list, read on the real stack alone. */
async function openDeliveryPage(page: Page, scene: SurfaceScene): Promise<Surface> {
  const root = page.getByTestId("main-content");
  await page.goto(`/delivery?${scene.deliveryQuery ?? ""}&mode=list`);
  await expect(root.getByRole("heading", { level: 1, name: "Delivery timeline" })).toBeVisible();
  const firstRow = page.locator("[data-pr-row]").first();
  await expect(firstRow).toBeVisible();
  return {
    name: "Delivery page",
    root,
    row: controlRow(root),
    firstRow,
    head: root.getByRole("heading", { level: 1, name: "Delivery timeline" }),
    // One of the window's own repositories: every seeded pull request is in it, so the list
    // keeps a first row. The facet's trigger is named for what it picked (`Repository: ...`), and
    // not the chip of that pick, which the panel lists at its top (R8).
    narrow: async () => {
      await openFilterPanel(page, root);
      const trigger = page.getByRole("dialog", { name: "Filter" }).getByRole("button", {
        name: /^Repository: /,
      });
      if ((await expandedState("the Repository filter", trigger)) === "shut") await trigger.click();
      await page.getByRole("listbox").getByRole("option").first().click();
      await trigger.click();
      await closeControlPanel(root);
    },
    panel: async () => {
      await openFilterPanel(page, root);
    },
  };
}

/**
 * Makes a session group named `name` of the session groups fixture's "Planner, first run"
 * (`seedSessionGroupsFixture`), in the Session groups dialog the Delivery page's filter panel
 * opens, and waits for its create to end. The page is open on the fixture's window. Hands back
 * the dialog (`editor`), the group's card, and the id its create answered.
 */
export async function createSessionGroup(
  page: Page,
  name: string
): Promise<{ card: Locator; editor: Locator; id: string }> {
  await expect(page.getByText("3 PRs in current filter/window")).toBeVisible();
  await openFilterPanel(page);
  await page.getByRole("button", { name: "Edit groups" }).click();
  const editor = page.getByRole("dialog", { name: "Session groups" });
  await editor.getByRole("checkbox", { exact: true, name: "Planner, first run" }).check();
  await editor.getByLabel("New group name").fill(name);
  const created = page.waitForResponse(
    (response) =>
      response.request().method() === "POST" &&
      response.url().endsWith("/api/v1/delivery/session-groups")
  );
  await editor.getByRole("button", { name: "Create group" }).click();
  const group = (await (await created).json()) as { id: string };
  const card = editor.getByRole("group", { name });
  // Every button of a card is held while a write of the editor is out, and Delete group by
  // nothing else, so its coming back is the create's end.
  await expect(card.getByRole("button", { name: "Delete group" })).toBeEnabled();
  return { card, editor, id: group.id };
}

/** The search bar, opened from the rail, with a query that lists hits. */
async function openSearchBar(page: Page, phone: boolean, term: string): Promise<Surface> {
  await page.goto("/");
  if (phone) await page.getByRole("button", { name: "Open navigation" }).click();
  await page.getByRole("button", { name: /^search/i }).click();
  const root = page.getByRole("dialog", { name: "Search" });
  await expect(root).toBeVisible();
  await control(controlRow(root), CONTROL.searchBox).fill(term);
  // The first row of the list is the first thing the list draws, which for this list is the
  // heading of the group the first hit falls in: a narrowing changes which hits the list holds
  // and so which of them carry a heading, and R1 is about the chrome above the list rather
  // than about the list's own contents.
  await expect(root.locator("#search-results [role='option']").first()).toBeVisible();
  const firstRow = root.locator("#search-results > *").first();
  return {
    name: "search bar",
    root,
    // The dialog's own strip is the control row (R1): no second strip is added to it.
    row: controlRow(root),
    firstRow,
    // No title of its own, so the bar is measured from its own top.
    head: root,
    // The status every seeded issue holds, so the hits the bar is listing stay listed.
    narrow: async () => {
      await openFilterPanel(page, root);
      await page
        .getByRole("dialog", { name: "Filter" })
        .getByRole("button", { name: "Status" })
        .click();
      await page
        .getByRole("listbox", { name: "Status options" })
        .getByRole("option", { name: "Todo" })
        .click();
      await closeControlPanel(root);
    },
    panel: async () => {
      await openFilterPanel(page, root);
    },
  };
}

/** An issue's Artifacts tab. */
async function openArtifactsTab(page: Page, scene: SurfaceScene): Promise<Surface> {
  const root = page.getByTestId("main-content");
  await page.goto(`/issues/${scene.issue}/artifacts`);
  const firstRow = root.locator('[data-testid^="artifact-"]').first();
  await expect(firstRow).toBeVisible();
  return {
    name: "Artifacts tab",
    root,
    row: controlRow(root),
    firstRow,
    head: page.getByTestId("issue-header").getByRole("heading", { level: 1 }),
    // The tab's one narrowing is what the reader types, which draws no chip: its search box is
    // every control the design gives it beside the row itself.
    narrow: async () => {
      await control(controlRow(root), CONTROL.searchBox).fill("a");
    },
  };
}

/** The opening tab of the Priorities page, which draws the priorities table alone. */
async function openPrioritiesPage(page: Page): Promise<Surface> {
  const root = page.getByTestId("main-content");
  await page.goto("/priorities");
  const head = root.getByRole("heading", { level: 1, name: "Priorities" });
  await expect(head).toBeVisible();
  const table = root.getByRole("region", { exact: true, name: "Priorities" });
  const firstRow = table.locator("[data-priority-row]").first();
  await expect(firstRow).toBeVisible();
  return {
    name: "priorities page",
    root,
    row: controlRow(table),
    firstRow,
    head,
    // The table has no trigger: its narrowings are its columns' and its search's. The Owner pick
    // its column filter writes into the address keeps the seeded priority's row and draws a chip,
    // as every page's narrowing here does.
    narrow: async () => {
      await page.goto("/priorities?p.f.owner=alice");
      await expect(control(controlRow(table), CONTROL.chip).first()).toBeVisible();
    },
    display: async () => {
      await openDisplay(page, table);
    },
  };
}

/** A priority's own page: its issues table, under the project List's bar. */
async function openPriorityPage(page: Page, scene: SurfaceScene): Promise<Surface> {
  if (scene.priority === undefined) throw new Error("the seed named no priority to open");
  const root = page.getByTestId("main-content");
  await page.goto(`/priorities/${scene.priority}`);
  const head = root.getByRole("heading", { level: 1 });
  await expect(head).toBeVisible();
  const firstRow = root.locator("[data-issue-row]").first();
  await expect(firstRow).toBeVisible();
  return {
    name: "priority page",
    root,
    row: controlRow(root),
    firstRow,
    head,
    // A word of the title of the one issue that advances it, so the table keeps its row.
    narrow: async () => {
      await control(controlRow(root), CONTROL.searchBox).fill(SURFACE_PRIORITY_WORK.word);
    },
    panel: async () => {
      await openFilterPanel(page, root);
    },
    display: async () => {
      await openDisplay(page, root);
    },
    select: async () => {
      await firstRow.getByRole("checkbox").first().click();
      await expect(control(root, CONTROL.selectionBar)).toBeVisible();
    },
  };
}

/** Opens one of the ten surfaces and hands back what a row reads it through. `phone` is
 *  whether the project is a phone one, which decides how the search bar is reached. */
export async function openSurface(
  page: Page,
  name: SurfaceName,
  scene: SurfaceScene,
  options: { phone?: boolean; searchTerm?: string } = {}
): Promise<Surface> {
  switch (name) {
    case "Inbox":
      return openInbox(page);
    case "Inbox drawer":
      return openInboxDrawer(page, scene);
    case "project List":
      return openProjectList(page, scene);
    case "project Board":
      return openProjectBoard(page, scene);
    case "Agents page":
      return openAgentsPage(page);
    case "Delivery page":
      return openDeliveryPage(page, scene);
    case "search bar":
      return openSearchBar(page, options.phone ?? false, options.searchTerm ?? "rollout");
    case "Artifacts tab":
      return openArtifactsTab(page, scene);
    case "priorities page":
      return openPrioritiesPage(page);
    case "priority page":
      return openPriorityPage(page, scene);
  }
}

// R8's control bar, read page by page: the bodies of `k-one-row`, `k-bar` and `k-no-growth`,
// which the stubbed rows run over the stubbable pages on the bar's slots and the real-stack rows
// over the Agents page and the Delivery page. Each failure names the page, the width and what was
// read.

/** The name of a bar's `+N more` chip, `<N> more filters`, and of the panel it opens. */
export const MORE_CHIPS = /^\d+ more filters?$/;

/** A chip's remove control, as `filterChips` names every one. */
export const REMOVE_CHIP = /^Remove .+ filter$/;

/** The Inbox's header line, on the page and in the drawer: the title, the search box and the link
 *  or the close button (`web/src/features/inbox/Inbox.tsx`). */
export const HEADER_LINE = "[data-inbox-header-line]";

/** A computed background no fill paints. */
const NO_FILL = /^(transparent|rgba\(0, 0, 0, 0\))$/;

/** The two tabs that each draw an Issues table, with their own address keys, and a word of the
 *  title of the one seeded issue each lists. */
const PRIORITY_FAULTS = [
  {
    tab: "none",
    label: "Active under no priority",
    status: "in_progress",
    word: "In progress",
    key: "n",
    title: SURFACE_LOOSE_WORK.word,
  },
  {
    tab: "backlog",
    label: "Priority work in backlog or icebox",
    status: "backlog",
    word: "Backlog",
    key: "k",
    title: SURFACE_PARKED_WORK.word,
  },
] as const;

async function openPriorityFault(
  page: Page,
  fault: (typeof PRIORITY_FAULTS)[number]
): Promise<Surface> {
  const address = `/priorities?tab=${fault.tab}`;
  await page.goto(address);
  const main = page.getByTestId("main-content");
  const root = main.getByRole("region", { exact: true, name: fault.label });
  const firstRow = root.locator("[data-issue-row]").first();
  await expect(firstRow).toBeVisible();
  await expect(controlRow(main), `${fault.label}: only the active tab draws a bar`).toHaveCount(1);
  const bar = controlRow(root);
  return {
    name: "project List",
    root,
    row: bar,
    firstRow,
    head: main.getByRole("heading", { level: 1, name: "Priorities" }),
    narrow: async () => {
      await page.goto(`${address}&${fault.key}.status=${fault.status}`);
      await settledBar(page, "project List", bar, 1, `${fault.label}, one narrowing on`);
    },
    panel: async () => {
      await openFilterPanel(page, root);
    },
    display: async () => {
      await openDisplay(page, root);
    },
    select: async () => {
      await firstRow.getByRole("checkbox").first().click();
      await expect(control(root, CONTROL.selectionBar)).toBeVisible();
    },
  };
}

/**
 * A fault tab with its first `count` narrowings on, as the address writes them: **Needs you**, a
 * flag the address keeps bare whichever table is drawn, and the tab's own status filter - one
 * flag and one column filter - then, under the tab's own prefix, **Unclaimed** and its Key and
 * Title filters, and **Unread**, bare too: six, each one chip, as a priority's own page takes
 * them. The status, the key and the title word are the seeded issue's own, and no status picked
 * is `done`, so no narrowing reads the closed issues (`IssuesTable`'s `wantsDone`); no reading of
 * a bar needs a row.
 */
function faultAddress(fault: (typeof PRIORITY_FAULTS)[number], count: number): string {
  const narrowings = [
    "needs-you=1",
    `${fault.key}.status=${fault.status}`,
    `${fault.key}.unclaimed=1`,
    `${fault.key}.f.key=BOOKS`,
    `${fault.key}.f.title=${fault.title}`,
    "unread=1",
  ];
  return [`/priorities?tab=${fault.tab}`, ...narrowings.slice(0, count)].join("&");
}

/** The narrowings each page that keeps them in its address is read under, as the address writes
 *  them, in the order a row takes them: the first one, two, three or all six, each one chip.
 *  Every page keeps its bar under all six, and the seed keeps rows under the first three. The
 *  Inbox keeps every row under all six: each of its asks is a question on CORE, and its one
 *  thread a reply on CORE. The Board narrows by every label it names at once and each seeded
 *  issue carries one, so its last two leave it no card; a priority's own issue has no ask, so
 *  `needs-you` leaves its table no row; the Agents page's role and the Delivery page's last four
 *  name nothing the seed holds; no reading of a bar needs a row. The search bar keeps its
 *  narrowings in its dialog, and an issue's Artifacts tab has none but its words (`narrowBar`). */
const BAR_NARROWINGS: Partial<Record<SurfaceName, readonly string[]>> = {
  Inbox: [
    "type=question",
    "type=reply",
    "type=decision",
    "type=approval",
    "type=agent",
    "project=CORE",
  ],
  "project List": [
    "needs-you=1",
    "label=frontend",
    "unclaimed=1",
    "unread=1",
    "status=todo",
    "f.key=CORE",
  ],
  "project Board": [
    "needs-you=1",
    "q=Rollout",
    "unclaimed=1",
    "unread=1",
    "label=frontend",
    "label=backend",
  ],
  "Agents page": [
    "machine=box-1",
    "group=needs-you",
    "group=active",
    "role=planner",
    "group=idle",
    "group=quiet",
  ],
  "Delivery page": [
    "repo=acme/widgets",
    "author=octocat",
    "priority=P1",
    "component=ACME/api",
    "session=e2e-planner-1",
    "issue=CORE-1",
  ],
  // The four order-tab columns that accept filters. Its work counts do not.
  "priorities page": ["p.f.owner=alice", "p.f.title=books", "p.f.order=%231", "p.f.newest=Today"],
  "priority page": [
    "w.unclaimed=1",
    "w.status=in_progress",
    "w.f.key=BOOKS",
    "w.f.title=Audit",
    "needs-you=1",
    "unread=1",
  ],
};

/** The Inbox's six, as a reader picks them from its Filter panel: a field, and its option. */
export const INBOX_PICKS: readonly (readonly [string, RegExp])[] = [
  ["type", /^Question,/],
  ["type", /^Threads,/],
  ["type", /^Decision,/],
  ["type", /^Approval,/],
  ["type", /^Agent message,/],
  ["project", /^CORE,/],
];

/** The page `name` with its first `count` narrowings on. The Inbox and the Agents page write
 *  each field's values as one comma list (`group=active,idle`), and the Delivery page is opened
 *  on its list over the scene's window, as `openSurface` opens it. */
export function narrowedAddress(name: SurfaceName, scene: SurfaceScene, count: number): string {
  const picked = (BAR_NARROWINGS[name] ?? []).slice(0, count);
  if (name === "Inbox" || name === "Agents page") {
    const lists = new Map<string, string[]>();
    for (const one of picked) {
      const [key = "", value = ""] = one.split("=");
      lists.set(key, [...(lists.get(key) ?? []), value]);
    }
    const path = name === "Inbox" ? "/" : "/agents";
    return `${path}?${[...lists].map(([key, values]) => `${key}=${values.join(",")}`).join("&")}`;
  }
  if (name === "Delivery page") {
    return `/delivery?${scene.deliveryQuery ?? ""}&mode=list&${picked.join("&")}`;
  }
  const path =
    name === "priorities page"
      ? "/priorities"
      : name === "priority page"
        ? `/priorities/${scene.priority ?? ""}`
        : `/projects/${scene.project}/issues`;
  return `${path}?${picked.join("&")}`;
}

/** The search bar's four narrowings, each picked from its Filter panel as a reader picks it:
 *  the status every seeded issue holds, the scene's project, an author typed and taken on Enter,
 *  and a day long past. */
const SEARCH_BAR_PICKS: readonly ((
  page: Page,
  panel: Locator,
  scene: SurfaceScene
) => Promise<void>)[] = [
  async (page, panel) => {
    await panel.getByRole("button", { exact: true, name: "Status" }).click();
    await page
      .getByRole("listbox", { name: "Status options" })
      .getByRole("option", { name: "Todo" })
      .click();
  },
  async (page, panel, scene) => {
    await panel.getByRole("button", { exact: true, name: "Project" }).click();
    await page
      .getByRole("listbox", { name: "Project options" })
      .getByRole("option", { name: new RegExp(`^${scene.project}\\b`) })
      .click();
  },
  async (_page, panel) => {
    const author = panel.getByRole("searchbox", { name: "Author" });
    await author.fill("alice");
    await author.press("Enter");
  },
  async (_page, panel) => {
    await panel.getByLabel("Updated since").fill("2020-01-01");
  },
];

/** A bar's chips: the ones its line draws, each by its remove control, and its `+N more`. */
function barChips(bar: Locator): { line: Locator; more: Locator } {
  return {
    line: bar.locator("[data-chip-strip]").getByRole("button", { name: REMOVE_CHIP }),
    more: bar.getByRole("button", { name: MORE_CHIPS }),
  };
}

/** Waits until the chips `bar` draws stand for `count` narrowings: the chips its line draws and
 *  the N its `+N more` names add up to `count`, N alone where the narrow layout of a bar with no
 *  trigger keeps every chip behind `+N more`. Hands back N as the passing reading saw it. */
async function expectChipsStandFor(bar: Locator, count: number, where: string): Promise<number> {
  const { line, more } = barChips(bar);
  let n = 0;
  await expect
    .poll(
      async () => {
        n =
          (await more.count()) === 0
            ? 0
            : Number.parseInt((await more.getAttribute("aria-label")) ?? "0", 10);
        return (await line.count()) + n;
      },
      {
        message: `${where}: the chips line and its +N more stand for another number than the ${count} narrowings on`,
      }
    )
    .toBe(count);
  return n;
}

/** Waits until the bar of `name` stands for exactly the `count` narrowings put on, and fails,
 *  naming `where`, if it stands for another number: **Filter** or **Filters** says their number,
 *  or - on the priorities table's bar, which has no trigger - its chips do, counted with the N of
 *  its `+N more` (`expectChipsStandFor`), so a bar that drew one chip of four never passes for one
 *  that drew all four. The chips line reads its chips' widths after the commit that drew them and
 *  draws again by that reading, so two frames more hold both. */
export async function settledBar(
  page: Page,
  name: SurfaceName,
  bar: Locator,
  count: number,
  where: string
) {
  if (name === "priorities page") await expectChipsStandFor(bar, count, where);
  else {
    await expect(
      control(bar, CONTROL.panelTrigger),
      `${where}: its trigger counts another number than the ${count} narrowings on`
    ).toHaveAccessibleName(new RegExp(`^Filters? · ${count}$`));
  }
  await nextFrames(page);
  await nextFrames(page);
}

/**
 * Puts up to `count` narrowings on the page `name` and waits until its bar stands for exactly
 * the ones it put on (`settledBar`, whose failure names `where` and their number): by the address
 * on the pages that keep their narrowings there; on the Inbox drawer and the search bar, which
 * keep them in their own state, the search bar offering four, by picks in the Filter panel; and
 * on an issue's Artifacts tab, which has no trigger and no chip, by one word typed in its search
 * box, which no count stands for. Hands back what is on, in the words a failure names it by.
 */
async function narrowBar(
  page: Page,
  name: SurfaceName,
  scene: SurfaceScene,
  surface: Surface,
  count: number,
  phone: boolean,
  where: string
): Promise<string> {
  if (name === "Artifacts tab") {
    await surface.narrow();
    await nextFrames(page);
    return "a word typed";
  }
  if (name === "Inbox drawer" || name === "search bar") {
    // The picks go on a bar that stands for no narrowing: the one the caller holds, as on its
    // first narrowing, which every caller makes on a surface it has just opened, and else the bar
    // opened afresh. Every caller read the bar settled before this, so its trigger's count is
    // read once, from its name: its words in the wide layout, its label in the narrow one.
    const said = await control(surface.row, CONTROL.panelTrigger).evaluate(
      (trigger) => trigger.getAttribute("aria-label") ?? trigger.textContent ?? ""
    );
    if (said.includes("·")) {
      await openSurface(page, name, scene, { phone, searchTerm: SURFACE_SEARCH_TERM });
    }
    let picked: number;
    if (name === "Inbox drawer") {
      picked = Math.min(count, INBOX_PICKS.length);
      for (const [field, option] of INBOX_PICKS.slice(0, picked)) {
        await pickInboxField(page, field, option, surface.root);
      }
    } else {
      picked = Math.min(count, SEARCH_BAR_PICKS.length);
      const panel = await openFilterPanel(page, surface.root);
      for (const pick of SEARCH_BAR_PICKS.slice(0, picked)) await pick(page, panel, scene);
      await closeControlPanel(surface.root);
    }
    const on = `${picked} narrowings on`;
    await settledBar(page, name, surface.row, picked, `${where}, ${on}`);
    return on;
  }
  await page.goto(narrowedAddress(name, scene, count));
  const on = `${count} narrowings on`;
  await settledBar(page, name, surface.row, count, `${where}, ${on}`);
  return on;
}

/** One control of a bar as a row reads it: what it is (its `data-control`, or `clear` for Clear
 *  filters), its accessible name, the words it shows - a badge's number among them - and its
 *  paint and box. */
interface BarControl {
  readonly background: string;
  readonly border: number;
  /** Whether it shares the box it stands in with the bar's trigger: the trigger's group. */
  readonly besideTrigger: boolean;
  readonly button: boolean;
  readonly control: string;
  readonly height: number;
  readonly left: number;
  readonly name: string;
  /** Whether it is the chosen part of the switch (`aria-pressed`). */
  readonly pressed: boolean;
  readonly right: number;
  /** The words it shows a sighted reader, without what it hides from a screen reader. */
  readonly shown: string;
  readonly weight: string;
  readonly width: number;
  readonly words: string;
}

/** A search box's room for its placeholder: the width the box's content gives its words beside
 *  the width of its placeholder in the box's own font. */
export interface PlaceholderRoom {
  readonly placeholder: string;
  readonly placeholderWidth: number;
  readonly room: number;
}

/** The room the search box `scope` holds gives its placeholder, read in one pass: a hidden probe
 *  in the box's own font measures the placeholder. None of either where `scope` draws no search
 *  box, as the Inbox's bar draws none, its search box standing in the header line. */
export async function placeholderRoom(scope: Locator): Promise<PlaceholderRoom> {
  const field = scope.locator('input[data-control="search-box"]').first();
  // Every caller reads a bar or a header line it waited for, drawn with its controls, so whether
  // it holds a search box is settled by now.
  if ((await field.count()) === 0) return { placeholder: "", placeholderWidth: 0, room: 0 };
  return field.evaluate((node: HTMLInputElement) => {
    const style = getComputedStyle(node);
    const probe = document.createElement("span");
    probe.textContent = node.placeholder;
    probe.style.cssText = "position: absolute; visibility: hidden; white-space: pre";
    for (const property of [
      "fontFamily",
      "fontSize",
      "fontStyle",
      "fontWeight",
      "letterSpacing",
    ] as const) {
      probe.style[property] = style[property];
    }
    document.body.append(probe);
    const placeholderWidth = probe.getBoundingClientRect().width;
    probe.remove();
    return {
      placeholder: node.placeholder,
      placeholderWidth,
      room:
        node.clientWidth -
        Number.parseFloat(style.paddingLeft) -
        Number.parseFloat(style.paddingRight),
    };
  });
}

/** A bar as a row reads it: its box, the controls of its line in their order, and its search
 *  box's room for its placeholder. */
interface BarReading extends PlaceholderRoom {
  readonly controls: readonly BarControl[];
  readonly height: number;
  readonly width: number;
}

/** The bar read: its box and every control of its line that a reader sees, in one pass, and its
 *  search box's room (`placeholderRoom`). */
export async function readBar(bar: Locator): Promise<BarReading> {
  const line = await bar.evaluate((node) => {
    const trigger = node.querySelector('[data-control="panel-trigger"]');
    const controls = [...node.querySelectorAll<HTMLElement>("input, button, a[href]")]
      .filter((element) =>
        element.checkVisibility({ opacityProperty: true, visibilityProperty: true })
      )
      .map((element) => {
        const style = getComputedStyle(element);
        const box = element.getBoundingClientRect();
        // The name a screen reader hears: its label, else its words without what it hides.
        const spoken = element.cloneNode(true) as HTMLElement;
        for (const hidden of spoken.querySelectorAll('[aria-hidden="true"]')) hidden.remove();
        const name = (
          element.getAttribute("aria-label") ??
          element.getAttribute("title") ??
          spoken.textContent ??
          ""
        ).trim();
        return {
          background: style.backgroundColor,
          border:
            Number.parseFloat(style.borderTopWidth) +
            Number.parseFloat(style.borderRightWidth) +
            Number.parseFloat(style.borderBottomWidth) +
            Number.parseFloat(style.borderLeftWidth),
          besideTrigger:
            trigger !== null &&
            trigger !== element &&
            trigger.parentElement === element.parentElement,
          button: element.tagName === "BUTTON",
          // A part of the switch is read as the switch: its parts carry no name of their own.
          control:
            element.getAttribute("data-control") ??
            element.closest('[data-control="segmented-control"]')?.getAttribute("data-control") ??
            (name === "Clear filters" ? "clear" : element.tagName.toLowerCase()),
          height: box.height,
          left: box.left,
          name,
          pressed: element.getAttribute("aria-pressed") === "true",
          right: box.right,
          shown: (spoken.textContent ?? "").trim(),
          weight: style.fontWeight,
          width: box.width,
          words: (element.textContent ?? "").trim(),
        };
      });
    const box = node.getBoundingClientRect();
    return { controls, height: box.height, width: box.width };
  });
  return { ...line, ...(await placeholderRoom(bar)) };
}

/** A bar as a failure names it: its width and height, and each control with its height. */
function saidOf(reading: BarReading): string {
  return `a ${Math.round(reading.width)}px bar ${Math.round(reading.height)}px tall holding ${reading.controls
    .map((one) => `${one.control} "${one.name}" (${Math.round(one.height)}px)`)
    .join(", ")}`;
}

/** The chips a wide bar stands for: the ones its line draws, then the ones its `+N more` lists in
 *  the panel it opens, read once the line and `N` add up to `count`; `N` itself is read against
 *  the panel. */
async function chipsOfBar(
  page: Page,
  bar: Locator,
  count: number,
  where: string
): Promise<{ line: string[]; listed: string[] }> {
  const { line, more } = barChips(bar);
  const n = await expectChipsStandFor(bar, count, where);
  const names = (chips: Locator) =>
    chips.evaluateAll((all) => all.map((chip) => chip.getAttribute("aria-label") ?? ""));
  const drawn = await names(line);
  if (n === 0) return { line: drawn, listed: [] };
  await more.click();
  const panel = page.getByRole("dialog", { name: MORE_CHIPS });
  await expect(panel, `${where}: +N more opened no panel`).toBeVisible();
  const listed = await names(panel.getByRole("button", { name: REMOVE_CHIP }));
  expect(listed.length, `${where}: +${n} more lists ${listed.join(", ")}`).toBe(n);
  await page.keyboard.press("Escape");
  await expect(panel, `${where}: Escape left the panel of +N more open`).toHaveCount(0);
  return { line: drawn, listed };
}

/** `k-one-row`'s reading of one bar at `where`: one line, as tall as its tallest control within
 *  1 px; its first control the search box, with room for its whole placeholder; the page not
 *  scrolled sideways; and in the narrow layout no chip in its line - but the priorities table's one
 *  `+N more` - and **Filters**, the display menu and **More** showing no words but a badge's
 *  number, each with its name. Hands back the bar as it read it. */
async function expectOneBar(
  page: Page,
  name: SurfaceName,
  bar: Locator,
  where: string
): Promise<BarReading> {
  // Every caller waited before this for the bar it reads: its page's first row, or the narrowings
  // it stands for and two frames more (`settledBar`), or a word typed and two frames more; and the
  // bar takes its layout from its own width before it first paints, so this is its settled layout.
  const reading = await readBar(bar);
  const said = saidOf(reading);
  const tallest = Math.max(...reading.controls.map((one) => one.height));
  expect(
    Math.abs(reading.height - tallest),
    `${where}: ${said}, so it is not one line as tall as its tallest control`
  ).toBeLessThanOrEqual(1);
  // The Inbox's bar starts with its switch, its search box standing in the header line.
  expect(reading.controls[0]?.control, `${where}: ${said}, its first control`).toBe(
    INBOXES.includes(name) ? CONTROL.segmentedControl : CONTROL.searchBox
  );
  expect(
    reading.placeholderWidth,
    `${where}: the search box has ${Math.round(reading.room)}px for its placeholder "${reading.placeholder}", which takes ${Math.round(reading.placeholderWidth)}px in the box's font`
  ).toBeLessThanOrEqual(reading.room);
  await assertPageFits(page);
  if (reading.width >= NARROW_BELOW) return reading;
  const chips = reading.controls.filter((one) => one.control === CONTROL.chip);
  const kept = name === "priorities page" ? chips.filter((one) => MORE_CHIPS.test(one.name)) : [];
  expect(
    chips.filter((one) => !kept.includes(one)).map((one) => one.name),
    `${where}: ${said}, chips in the narrow bar's line`
  ).toEqual([]);
  expect(kept.length, `${where}: ${said}, more than one +N more`).toBeLessThanOrEqual(1);
  for (const one of reading.controls) {
    if (one.control === CONTROL.barAction) {
      // The main action, the one action the narrow line keeps, shows its icon alone.
      expect(one.name, `${where}: ${said}, ${one.control} has no name`).not.toBe("");
      expect(one.shown, `${where}: ${said}, ${one.name} shows words in the narrow layout`).toBe("");
      continue;
    }
    if (
      one.control !== CONTROL.panelTrigger &&
      one.control !== CONTROL.displayMenu &&
      one.control !== CONTROL.moreMenu
    ) {
      continue;
    }
    expect(one.name, `${where}: ${said}, ${one.control} has no name`).not.toBe("");
    expect(one.words, `${where}: ${said}, ${one.control} shows words in the narrow layout`).toMatch(
      one.control === CONTROL.panelTrigger ? /^\d*$/ : /^$/
    );
  }
  return reading;
}

/** **New document** and **Upload** are items of **More** in an issue's Artifacts tab's narrow
 *  bar, and buttons of the line in no layout of it but the wide one. */
async function expectActionsInMore(
  page: Page,
  bar: Locator,
  reading: BarReading,
  where: string
): Promise<void> {
  expect(
    reading.controls.filter((one) => one.control === CONTROL.barAction).map((one) => one.name),
    `${where}: ${saidOf(reading)}, actions in the narrow bar's line`
  ).toEqual([]);
  await control(bar, CONTROL.moreMenu).click();
  for (const label of ["New document", "Upload"]) {
    await expect(
      page.getByRole("menuitem", { exact: true, name: label }),
      `${where}: More holds no ${label}`
    ).toBeVisible();
  }
  await page.keyboard.press("Escape");
  await expect(
    page.getByRole("menu", { name: "More" }),
    `${where}: Escape left More open`
  ).toHaveCount(0);
}

/** `k-one-row` on the page `name` at `size`: exactly one bar above its list or table (one above
 *  each table of the priorities page), nothing between its title - or its own top, for the search
 *  bar - and its first row outside the bar, and the bar one line with its search box first and
 *  its placeholder whole, with no narrowing on and with six, or as many as the page has, each
 *  read on before the bar is measured under them. */
export async function expectOneBarOn(
  page: Page,
  name: SurfaceName,
  scene: SurfaceScene,
  size: { height: number; width: number },
  phone: boolean
): Promise<void> {
  const surface = await openSurface(page, name, scene, { phone, searchTerm: SURFACE_SEARCH_TERM });
  const where = `${name} at ${size.width}x${size.height}`;
  await expect(surface.row, `${where}: not exactly one control bar above its list`).toHaveCount(1);
  // The Inbox and its drawer draw their search box in the header line, beside the title (R8), so
  // there the strays are read from under that line, which holds the page's one search box.
  const headed = INBOXES.includes(name);
  const strays = await straysBetween(
    surface.root,
    headed
      ? await surface.root
          .locator(HEADER_LINE)
          .evaluate((node) => node.getBoundingClientRect().bottom)
      : await topOf(surface.head),
    await topOf(surface.firstRow)
  );
  expect(
    strays,
    `${where}: ${strays.join("; ")} is drawn between the title and the first row, outside the bar${headed ? " and the header line" : ""}`
  ).toEqual([]);
  const rest = await expectOneBar(page, name, surface.row, `${where}, no narrowing on`);
  // The drawer is at most 448 px wide from 768 px, and as wide as the window below, so its bar
  // takes the narrow layout at every width a row drives.
  if (name === "Inbox drawer") {
    expect(
      rest.width,
      `${where}: the drawer's bar is ${Math.round(rest.width)}px wide`
    ).toBeLessThan(NARROW_BELOW);
  }
  if (name === "Artifacts tab" && phone) await expectActionsInMore(page, surface.row, rest, where);
  // `narrowBar` reads that the bar stands for exactly the narrowings it put on, at this width,
  // before the bar is measured under them: the trigger's count, or - on the priorities table,
  // which has no trigger and filters four columns - its chips, with the N of its `+N more`.
  const wanted = name === "priorities page" ? 4 : 6;
  const on = await narrowBar(page, name, scene, surface, wanted, phone, where);
  await expectOneBar(page, name, surface.row, `${where}, ${on}`);
  if (phone && name === "priorities page") {
    // A bar with chips and no trigger keeps one `+N more` in its narrow line, which holds them.
    await page.goto(narrowedAddress(name, scene, 2));
    await settledBar(page, name, surface.row, 2, `${where}, two narrowings on`);
    const line = (await readBar(surface.row)).controls.filter(
      (one) => one.control === CONTROL.chip
    );
    expect(
      line.map((one) => one.name),
      `${where}, two narrowings on: the narrow bar's chips`
    ).toEqual(["2 more filters"]);
  }
  if (name === "priorities page") {
    for (const fault of PRIORITY_FAULTS) {
      const tab = await openPriorityFault(page, fault);
      const at = `${where}, ${fault.label}`;
      await expectOneBar(page, tab.name, tab.row, `${at}, no narrowing on`);
      await tab.narrow();
      await expectOneBar(page, tab.name, tab.row, `${at}, one narrowing on`);
      // Six, each one chip, read on by the trigger's count before the bar is measured under them.
      const six = `${at}, six narrowings on`;
      await page.goto(faultAddress(fault, 6));
      await settledBar(page, tab.name, tab.row, 6, six);
      await expectOneBar(page, tab.name, tab.row, six);
    }
  }
}

/** The controls the Design's table gives each page's bar with no narrowing on, each by what it is
 *  and its name, in the bar's order, in the layout its width gives it: an action - the priorities
 *  table's **New priority**, the Artifacts tab's **New document** and **Upload** - is a button of
 *  the line in the wide layout and an item of **More** in the narrow one. `designOf` reads a bar
 *  in these words. */
export function barDesign(name: SurfaceName, narrow: boolean, phone = false): string[] {
  switch (name) {
    case "Inbox":
    case "Inbox drawer":
      return narrow
        ? [
            'segmented-control "All"',
            'segmented-control "Blocked on you"',
            'panel-trigger "Filter"',
            'more-menu "More"',
            'bar-action "New message"',
          ]
        : [
            'segmented-control "All"',
            'segmented-control "Blocked on you"',
            'panel-trigger "Filter"',
            'display-menu "View"',
            'more-menu "More"',
            'bar-action "New message"',
          ];
    case "project Board":
      return ['search-box "Search issues"', 'panel-trigger "Filters"'];
    case "Agents page":
      return ['search-box "Search agents"', 'panel-trigger "Filter"'];
    case "Delivery page":
      return ['search-box "Search pull requests"', 'panel-trigger "Filter"'];
    case "search bar":
      return ['search-box "Search"', 'panel-trigger "Filter"'];
    case "Artifacts tab":
      return narrow
        ? ['search-box "Filter artifacts"', 'more-menu "More"']
        : ['search-box "Filter artifacts"', 'bar-action "New document"', 'bar-action "Upload"'];
    case "priorities page":
      return [
        'search-box "Search priorities"',
        ...(narrow ? [] : ['bar-action "New priority"']),
        ...(phone ? [] : ['columns-menu "Columns"']),
        'display-menu "View"',
        ...(narrow ? ['more-menu "More"'] : []),
      ];
    default:
      return [
        'search-box "Search issues"',
        'panel-trigger "Filters"',
        'columns-menu "Columns"',
        'display-menu "View"',
      ];
  }
}

/** The pages whose bar draws **Clear filters** beside its trigger while a narrowing is on: the
 *  Inbox's, and the project List's bar, which a priority's own page draws too. Every other page
 *  had none before the bar and has none in it (D24). */
const CLEARS: readonly SurfaceName[] = ["Inbox", "project List", "priority page"];

/** The Inbox's two bars, on the page and in the drawer. */
export const INBOXES: readonly SurfaceName[] = ["Inbox", "Inbox drawer"];

/** The controls of `reading` as `barDesign` spells them: what each is and its name, the switch's
 *  **Blocked on you** without its count and the oldest one's age, which it carries after a colon
 *  (`blockedOnYouText`) and `expectInboxBar` reads. */
export function designOf(reading: BarReading): string[] {
  return reading.controls.map(
    (one) => `${one.control} "${one.name.replace(/^Blocked on you: .*$/, "Blocked on you")}"`
  );
}

/**
 * `k-bar`'s readings of the Inbox's bar, on the page and in the drawer, at rest (`rest`): the
 * switch's two parts by their accessible names, the same at every width, reading **All** and
 * **Blocked on you** in the wide layout and **All** and **You · N** in the narrow one, the chosen
 * part on a fill and in a heavier weight than the other; **New message** the bar's last control
 * and its one filled action, its icon alone with its name in the narrow layout; **Mark all read**
 * an item of **More**, with **View** beside it in the narrow layout; at the phone width no item
 * outside the bar's box or over another's; and on the page, an agent narrowing counted by
 * **Filter** and listed at the top of its panel.
 */
async function expectInboxBar(
  page: Page,
  name: SurfaceName,
  bar: Locator,
  rest: BarReading,
  phone: boolean,
  where: string
): Promise<void> {
  const narrow = rest.width < NARROW_BELOW;
  const said = `${where}: ${saidOf(rest)}`;
  const [all, blocked] = rest.controls;
  expect(all?.name, `${said}, the switch's first part`).toBe("All");
  expect(blocked?.name, `${said}, Blocked on you's name`).toMatch(/^Blocked on you: \d+ items?\b/);
  const count = /: (\d+) item/.exec(blocked?.name ?? "")?.[1] ?? "";
  expect([all?.shown, blocked?.shown], `${said}, the switch's words`).toEqual(
    narrow ? ["All", `You · ${count}`] : ["All", `Blocked on you ${count}`]
  );
  expect(all?.pressed, `${said}, All is not the chosen part`).toBe(true);
  expect(
    [all?.background !== blocked?.background, all?.weight !== blocked?.weight],
    `${said}, the chosen part does not differ in its fill and its weight`
  ).toEqual([true, true]);
  const main = rest.controls.at(-1);
  expect(main?.name, `${said}, New message is not the bar's last control`).toBe("New message");
  expect(main?.background, `${said}, New message is not filled`).not.toMatch(NO_FILL);
  if (narrow) expect(main?.shown, `${said}, New message shows words`).toBe("");
  await control(bar, CONTROL.moreMenu).click();
  const more = page.getByRole("menu", { name: "More" });
  for (const item of narrow ? ["View", "Mark all read"] : ["Mark all read"]) {
    await expect(
      more.getByRole("menuitem", { exact: true, name: item }),
      `${where}: More holds no ${item}`
    ).toBeVisible();
  }
  await page.keyboard.press("Escape");
  await expect(more, `${where}: Escape left More open`).toHaveCount(0);
  if (phone) {
    // More was waited shut above and nothing since has resized the page, so the bar's box is the
    // one the controls of `rest` were read in.
    const box = await bar.evaluate((node) => node.getBoundingClientRect().toJSON() as DOMRect);
    const misplaced = rest.controls.flatMap((one, index) => {
      const out =
        one.left < box.left - 0.5 || one.right > box.right + 0.5 ? [`${one.name} outside`] : [];
      const over = rest.controls
        .slice(index + 1)
        .filter((other) => other.left < one.right - 0.5 && one.left < other.right - 0.5)
        .map((other) => `${one.name} over ${other.name}`);
      return [...out, ...over];
    });
    expect(misplaced, `${said}, items outside the bar or over another`).toEqual([]);
  }
  if (name !== "Inbox") return;
  await page.goto(`/?agent=${encodeURIComponent(SURFACE_SESSION.actor.id)}`);
  await settledBar(page, name, bar, 1, `${where}, an agent narrowing on`);
  const panel = await openFilterPanel(page, page.getByTestId("main-content"));
  await expect(
    panel.getByRole("button", { name: /^Remove Asks from .+ filter$/ }),
    `${where}: the Filter panel does not list the agent narrowing`
  ).toHaveCount(1);
  await closeControlPanel(page.getByTestId("main-content"));
}

/** Read exactly this bar's controls and their resting paint, in its own layout. */
async function expectRestingBar(
  bar: Locator,
  name: SurfaceName,
  where: string,
  phone: boolean
): Promise<BarReading> {
  await expect(bar, `${where}: no control bar`).toBeVisible();
  // The bar's own width determines the layout, not the viewport width.
  const rest = await readBar(bar);
  const narrow = rest.width < NARROW_BELOW;
  const said = `${where}: ${saidOf(rest)}`;
  expect(
    designOf(rest),
    `${said}, not its design's controls in the ${narrow ? "narrow" : "wide"} layout`
  ).toEqual(barDesign(name, narrow, phone));
  // The chosen part of the switch and the main action are filled; every other button is quiet.
  const painted = rest.controls.filter(
    (one) =>
      one.button &&
      !one.pressed &&
      !(
        one.control === CONTROL.barAction &&
        one === rest.controls.at(-1) &&
        INBOXES.includes(name)
      ) &&
      (one.border !== 0 || !NO_FILL.test(one.background))
  );
  expect(
    painted.map((one) => `${one.control} "${one.name}": ${one.border}px border, ${one.background}`),
    `${said}, buttons with a border or a fill at rest`
  ).toEqual([]);
  if (phone) {
    const small = rest.controls.filter(
      (one) => one.button && (one.width < 43.5 || one.height < 43.5)
    );
    expect(
      small.map(
        (one) => `${one.control} "${one.name}" ${Math.round(one.width)}x${Math.round(one.height)}px`
      ),
      `${said}, buttons under the 44 px floor`
    ).toEqual([]);
  }
  return rest;
}

async function expectPriorityIssueChips(
  page: Page,
  region: Locator,
  where: string,
  status: string
): Promise<void> {
  const bar = controlRow(region);
  await settledBar(page, "project List", bar, 1, where);
  const chip = `Remove Status: ${status} filter`;
  // settledBar waited for the bar to count its one narrowing, so its width is settled.
  const width = await bar.evaluate((node) => node.getBoundingClientRect().width);
  if (width < NARROW_BELOW) {
    const panel = await openFilterPanel(page, region);
    await expect(
      control(panel, CONTROL.chip).and(panel.getByRole("button", { exact: true, name: chip })),
      `${where}: Filters lists no ${status} chip`
    ).toBeVisible();
    await closeControlPanel(region);
  } else {
    const { line, listed } = await chipsOfBar(page, bar, 1, where);
    expect([...line, ...listed], `${where}: the bar's ${status} chip`).toEqual([chip]);
  }
}

/** `k-bar`'s reading of the List's bar - the project List's, or the one above an issues table of
 *  the priorities page - with one flag and one column filter on, at `address`: **Filters** counts
 *  the two, and its open panel lists `chips`, each with the control that removes it. */
async function expectFlagAndColumn(
  page: Page,
  bar: Locator,
  root: Locator,
  address: string,
  chips: readonly string[],
  where: string
): Promise<void> {
  await page.goto(address);
  await settledBar(page, "project List", bar, chips.length, `${where}, a flag and a column on`);
  const panel = await openFilterPanel(page, root);
  for (const chip of chips) {
    await expect(
      panel.getByRole("button", { exact: true, name: `Remove ${chip} filter` }),
      `${where}: the Filters panel does not list ${chip}`
    ).toHaveCount(1);
  }
  await closeControlPanel(root);
}

/** `k-bar`'s reading of a bar whose trigger counted the `count` narrowings on (`settledBar`): in
 *  the wide layout the chips that do not fit stand behind one `+N more`, each chip once; in the
 *  narrow one none stands in the line, the trigger's badge says their number and no Clear filters
 *  is drawn; at every width the trigger's panel lists them all at its top, each with the control
 *  that removes it. `at` names the page, its width and the narrowings on. */
async function expectNarrowingsListed(
  page: Page,
  bar: Locator,
  root: Locator,
  count: number,
  at: string
): Promise<void> {
  const many = await readBar(bar);
  if (many.width < NARROW_BELOW) {
    expect(
      many.controls.find((each) => each.control === CONTROL.panelTrigger)?.words,
      `${at}: ${saidOf(many)}, its trigger's badge`
    ).toBe(String(count));
    expect(
      many.controls.filter((each) => each.control === CONTROL.chip || each.control === "clear"),
      `${at}: ${saidOf(many)}, chips or Clear filters in the narrow line`
    ).toEqual([]);
  } else {
    const { line, listed } = await chipsOfBar(page, bar, count, at);
    expect(new Set([...line, ...listed]).size, `${at}: ${count} chips, each once`).toBe(count);
  }
  const panel = await openFilterPanel(page, root);
  await expect(
    panel.getByRole("button", { name: REMOVE_CHIP }),
    `${at}: the panel's top does not list every narrowing with its remove control`
  ).toHaveCount(count);
  await closeControlPanel(root);
  await expect(control(bar, CONTROL.panelTrigger)).toHaveAttribute("aria-expanded", "false");
}

/** That `reading`, a bar under narrowings, draws **Clear filters** as the control right after its
 *  trigger, in the trigger's group, a button of its own. `at` names the page, its width and what
 *  is on. */
function expectClearBesideTrigger(reading: BarReading, at: string): void {
  const trigger = reading.controls.findIndex((each) => each.control === CONTROL.panelTrigger);
  expect(
    reading.controls[trigger + 1]?.control,
    `${at}: ${saidOf(reading)}, no Clear filters after its trigger`
  ).toBe("clear");
  expect(
    reading.controls.find((each) => each.control === "clear")?.besideTrigger,
    `${at}: Clear filters is not in its trigger's group`
  ).toBe(true);
}

/** `k-bar`'s **Filters** and **Clear filters** on a fault tab's bar, with the flag and the column
 *  filter of `address` on (`expectFlagAndColumn`), read as the project List's: at every width the
 *  trigger is drawn on a fill; in the wide layout Clear filters is the control right after the
 *  trigger, in the trigger's group, and a press on it takes both off - the trigger counts
 *  nothing, the line draws no chip and no `+N more`, Clear filters goes, and the address holds
 *  neither narrowing's key; in the narrow layout none is drawn. `at` names the page, its width
 *  and the tab. */
async function expectClearTakesBoth(
  page: Page,
  bar: Locator,
  address: string,
  at: string
): Promise<void> {
  const on = `${at}, a flag and a column on`;
  // `expectFlagAndColumn` waited until the trigger counted the two, and two frames more, and shut
  // its panel, so the bar read here stands for them.
  const two = await readBar(bar);
  expect(
    two.controls.find((each) => each.control === CONTROL.panelTrigger)?.background,
    `${on}: its trigger has no fill`
  ).not.toMatch(NO_FILL);
  if (two.width < NARROW_BELOW) {
    expect(
      two.controls.filter((each) => each.control === "clear").length,
      `${on}: ${saidOf(two)}, a Clear filters`
    ).toBe(0);
    return;
  }
  expectClearBesideTrigger(two, on);
  // The narrowings' own keys, which the press takes out of the address. `tab` stays, so the press
  // leads to the tab's unnarrowed address, which the row opened the tab at.
  const keys = [...new URL(address, page.url()).searchParams.keys()].filter((key) => key !== "tab");
  const button = bar.getByRole("button", { exact: true, name: "Clear filters" });
  await button.click();
  const cleared = `${at}, Clear filters pressed`;
  await expect(
    control(bar, CONTROL.panelTrigger),
    `${cleared}: its trigger still counts a narrowing`
  ).toHaveAccessibleName(/^Filters?$/);
  const { line, more } = barChips(bar);
  await expect(line, `${cleared}: chips left in the line`).toHaveCount(0);
  await expect(more, `${cleared}: a +N more left in the line`).toHaveCount(0);
  await expect(button, `${cleared}: Clear filters is still drawn`).toHaveCount(0);
  await expect
    .poll(() => [...new URL(page.url()).searchParams.keys()].filter((key) => keys.includes(key)), {
      message: `${cleared}: the address still holds these of ${keys.join(", ")}`,
    })
    .toEqual([]);
}

/** `k-bar`'s reading of a fault tab's line under six narrowings, made before any panel is
 *  opened: in the wide layout the six do not fit a bar at 1280x800, so one `+N more` holds at
 *  least one of them, and each chip the line draws stands inside the bar, its right edge at most
 *  the bar's within 1 px. The narrow layout draws no chip in the line (`expectNarrowingsListed`). */
async function expectLineFits(bar: Locator, at: string): Promise<void> {
  // `settledBar` waited until the trigger counted the six, and two frames more, which the line
  // takes to read its chips' widths and draw again by them, so this is the line it settled on.
  const box = await bar.evaluate((node) => {
    const { left, right, width } = node.getBoundingClientRect();
    return { left, right, width };
  });
  if (box.width < NARROW_BELOW) return;
  const { line, more } = barChips(bar);
  const chips = await line.evaluateAll((all) =>
    all.map((chip) => {
      const { left, right } = chip.getBoundingClientRect();
      return { left, name: chip.getAttribute("aria-label") ?? "", right };
    })
  );
  // The line drew its last layout before this read (`settledBar`, above), so whether it draws a
  // `+N more` is settled here too.
  const drawsMore = (await more.count()) > 0;
  const n = drawsMore ? Number.parseInt((await more.getAttribute("aria-label")) ?? "0", 10) : 0;
  const drawn = chips.map(
    (chip) => `"${chip.name}" at ${Math.round(chip.left)}-${Math.round(chip.right)}px`
  );
  const said = `a bar at ${Math.round(box.left)}-${Math.round(box.right)}px whose line draws ${drawn.length === 0 ? "no chip" : drawn.join(", ")}`;
  expect(n, `${at}: ${said}, and no +N more holds the rest`).toBeGreaterThanOrEqual(1);
  expect(
    chips.filter((chip) => chip.right > box.right + 1).map((chip) => chip.name),
    `${at}: ${said}, so these stand past its right edge`
  ).toEqual([]);
}

/** `assertPageFits`, its failure naming `at`: one row reads both fault tabs in each layout, so the
 *  page and the tab must be in the failure itself and not only in the line under it. */
async function expectPageFitsAt(page: Page, at: string): Promise<void> {
  try {
    await assertPageFits(page);
  } catch (cause) {
    const said = cause instanceof Error ? cause.message : String(cause);
    throw new Error(`${at}: ${said}`, { cause });
  }
}

/** `k-bar` on the page `name` at `size`: its design's controls and no other in the layout its
 *  bar's width gives, each button quiet at rest and, at the phone's width, at least 44 px each
 *  way; and on a page with a trigger, **Filter**'s tint and count, **Clear filters** in its group
 *  where the page has one, and its chips in the line, behind `+N more` or - below 640 px - behind
 *  the trigger's badge, each listed at the top of its panel. On the priorities page the bar above
 *  each fault tab's issues table, which is the List's, is read as the List's is: under one flag
 *  and one column filter, which **Clear filters** takes off in the wide layout, and under six
 *  narrowings, whose chips stand inside the bar, behind `+N more` in the wide layout, on a page
 *  that does not scroll sideways. */
export async function expectBarDesign(
  page: Page,
  name: SurfaceName,
  scene: SurfaceScene,
  size: { height: number; width: number },
  phone: boolean
): Promise<void> {
  const where = `${name} at ${size.width}x${size.height}`;
  const surface = await openSurface(page, name, scene, { phone, searchTerm: SURFACE_SEARCH_TERM });
  const bar = surface.row;
  // At rest: no narrowing, nothing hovered, nothing focused.
  const toRest = async () => {
    await page.mouse.move(size.width - 1, size.height - 1);
    await page.evaluate(() => (document.activeElement as HTMLElement | null)?.blur());
  };
  await toRest();
  const rest = await expectRestingBar(bar, name, where, phone);
  const narrow = rest.width < NARROW_BELOW;
  if (narrow && name === "priorities page") {
    await control(bar, CONTROL.moreMenu).click();
    await expect(
      page.getByRole("menuitem", { name: "New priority" }),
      `${where}: More holds no New priority`
    ).toBeVisible();
    await page.keyboard.press("Escape");
  }
  // No trigger and no chip: its actions, in the line or in More, are the whole bar.
  if (name === "Artifacts tab") {
    if (narrow) await expectActionsInMore(page, bar, rest, where);
    return;
  }
  if (name === "priorities page") {
    // The priorities table's own chips, which have no Filter trigger to stand for them.
    if (!narrow) {
      await narrowBar(page, name, scene, surface, 4, phone, where);
      const { line, listed } = await chipsOfBar(page, bar, 4, `${where}, four narrowings on`);
      expect(new Set([...line, ...listed]).size, `${where}: four chips, each once`).toBe(4);
    }
    for (const fault of PRIORITY_FAULTS) {
      const tab = await openPriorityFault(page, fault);
      const at = `${where}, ${fault.label}`;
      await toRest();
      await expectRestingBar(tab.row, tab.name, at, phone);
      await tab.narrow();
      await expectPriorityIssueChips(page, tab.root, at, fault.word);
      // The bar above the tab's own issues table is the List's, read as the List's is: one flag
      // and one column filter, which Clear filters takes off in the wide layout, then six
      // narrowings, six at the phone's width too. Their line is read before any panel opens, so a
      // line that draws every chip with no `+N more` fails at its own read and not at the panels'
      // count.
      const flagAndColumn = faultAddress(fault, 2);
      await expectFlagAndColumn(
        page,
        tab.row,
        tab.root,
        flagAndColumn,
        ["Needs you", `Status: ${fault.word}`],
        at
      );
      // Shutting the panel clicked the trigger, and a trigger under the pointer is drawn on its
      // hover fill, so the pointer rests in the corner before Filters' fill is read.
      await toRest();
      await expectClearTakesBoth(page, tab.row, flagAndColumn, at);
      const six = `${at}, six narrowings on`;
      await page.goto(faultAddress(fault, 6));
      await settledBar(page, tab.name, tab.row, 6, six);
      await expectLineFits(tab.row, six);
      await expectNarrowingsListed(page, tab.row, tab.root, 6, six);
      await expectPageFitsAt(page, six);
    }
    // The work table now lives on the priority's own page. Read its status chip there.
    const work = await openPriorityPage(page, scene);
    await toRest();
    await expectRestingBar(work.row, work.name, `${where}, work under the priority`, phone);
    await page.goto(`/priorities/${scene.priority}?w.status=in_progress`);
    await expectPriorityIssueChips(
      page,
      work.root,
      `${where}, work under the priority`,
      "In progress"
    );
    return;
  }
  if (INBOXES.includes(name)) await expectInboxBar(page, name, bar, rest, phone, where);

  // One narrowing: the trigger on a tint, its count in its name, and - on the List's bar in the
  // wide layout - Clear filters beside it in its group, a button of its own.
  await narrowBar(page, name, scene, surface, 1, phone, where);
  // `narrowBar` waited until the trigger counted the one narrowing, and two frames more for the
  // chips line (`settledBar`), so the bar read here stands for it.
  const one = await readBar(bar);
  const tinted = one.controls.find((each) => each.control === CONTROL.panelTrigger);
  expect(tinted?.background, `${where}, one narrowing on: its trigger has no fill`).not.toMatch(
    NO_FILL
  );
  const clear = one.controls.filter((each) => each.control === "clear");
  if (narrow || !CLEARS.includes(name)) {
    expect(clear.length, `${where}, one narrowing on: ${saidOf(one)}, a Clear filters`).toBe(0);
  } else {
    expectClearBesideTrigger(one, `${where}, one narrowing on`);
  }

  // The List's flag and column filter: counted as two, each listed at the panel's top with the
  // control that removes it.
  if (name === "project List") {
    await expectFlagAndColumn(
      page,
      bar,
      surface.root,
      `/projects/${scene.project}/issues?needs-you=1&label=frontend`,
      ["Needs you", "Label: frontend"],
      where
    );
  }

  // Six at 1280x800 (the search bar's four), three at the phone width: in the wide layout the
  // chips that do not fit stand behind one `+N more`, and in the narrow one none stands in the
  // line, the trigger's badge says their number and no Clear filters is drawn; at every width the
  // panel lists them all.
  const wanted = phone ? 3 : 6;
  const count = name === "search bar" ? Math.min(wanted, SEARCH_BAR_PICKS.length) : wanted;
  const at = `${where}, ${await narrowBar(page, name, scene, surface, count, phone, where)}`;
  await expectNarrowingsListed(page, bar, surface.root, count, at);
}

/**
 * `k-no-growth` on the page `name` at `size`, in the layout its bar's width gives there: the top
 * of its first row, read before and after each act - one narrowing that draws a chip (one word
 * on the Artifacts tab), the filter panel opened, the display menu opened, and a row selected
 * where the page allows it (on the Agents page one row's checkbox, then every matching row) -
 * holds within 1 px.
 */
export async function expectNoGrowth(
  page: Page,
  name: SurfaceName,
  scene: SurfaceScene,
  size: { height: number; width: number },
  phone: boolean
): Promise<void> {
  const surface = await openSurface(page, name, scene, { phone, searchTerm: SURFACE_SEARCH_TERM });
  const at = `${name} at ${size.width}x${size.height}`;
  await expectSurfaceNoGrowth(
    page,
    surface,
    at,
    name === "priority page" ? () => pickIssueFlag(page, "Unclaimed") : surface.narrow
  );
  if (name === "priorities page") {
    for (const fault of PRIORITY_FAULTS) {
      const tab = await openPriorityFault(page, fault);
      await expectSurfaceNoGrowth(page, tab, `${at}, ${fault.label}`, tab.narrow);
    }
  }
}

async function expectSurfaceNoGrowth(
  page: Page,
  surface: Surface,
  at: string,
  narrowing: () => Promise<void>
): Promise<void> {
  const { name } = surface;
  // `openSurface` waited for the page's first row, so the bar is laid out, and its width follows
  // the viewport the row set before it opened the page.
  const width = await surface.row.evaluate((node) => node.getBoundingClientRect().width);
  const where = `${at}, its bar ${Math.round(width)}px wide (the ${width < NARROW_BELOW ? "narrow" : "wide"} layout)`;
  await resetScroll(page, surface.firstRow);
  const first = await topOf(surface.firstRow);
  // The Agents page marks rows for a broadcast: a checkbox on each row and one over the list for
  // every row the filters match (`features/agents/AgentsPage.tsx`). A mark draws the broadcast
  // composer, which sticks to the bottom of the viewport rather than standing above the rows.
  const agents =
    name === "Agents page"
      ? ([
          [
            "one row's checkbox ticked",
            () =>
              surface.firstRow.getByRole("checkbox", { name: /^Select .+ for broadcast$/ }).check(),
          ],
          [
            "every matching row selected",
            () =>
              surface.root
                .getByRole("checkbox", { exact: true, name: "Select all matching agents" })
                .check(),
          ],
        ] as const)
      : [];
  for (const [act, run] of [
    ["one narrowing applied", narrowing],
    ["the filter panel opened", surface.panel],
    ["the display menu opened", surface.display],
    ["a row selected", surface.select],
    ...agents,
  ] as const) {
    if (run === undefined) continue;
    await run();
    await expect(surface.firstRow, `${where}: ${act} left no first row`).toBeVisible();
    await resetScroll(page, surface.firstRow);
    const after = await topOf(surface.firstRow);
    expect(
      Math.abs(after - first),
      `${where}: ${act} moved the first row from ${Math.round(first)}px to ${Math.round(after)}px`
    ).toBeLessThanOrEqual(1);
    if (run === surface.panel) {
      await page.keyboard.press("Escape");
      await expect(
        control(surface.row, CONTROL.panelTrigger),
        `${where}: Escape left the filter panel open`
      ).toHaveAttribute("aria-expanded", "false");
    }
  }
}
