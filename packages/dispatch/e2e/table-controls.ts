/** The eight surfaces of INFRA-3135's R2, as a browser row drives them, and the readings R1,
 *  R2 and R4 are made with. Shared by `table-controls.e2e.ts` and the rows of that family which
 *  run against a recording (`table-controls.stub.e2e.ts`), so the two files name one page, one
 *  first row and one narrowing each, and a page whose controls move is edited here once.
 *
 *  Nothing here asserts; each function hands back what a row reads, and the row says what it
 *  expects of it. The one exception is the waiting each opener does, which is how it knows the
 *  surface is drawn at all.
 */
import { expect, type Locator, type Page } from "@playwright/test";

import { CONTROL, type ControlName } from "../web/src/components/control-names";
import { shownAgentRows } from "./agents";
import { createAsk, createIssue, createIssueArtifact, createProject, patchIssue } from "./api";
import { control, controlRow, openFilterPanel } from "./filters";
import { drawer, peekTrigger } from "./inbox-drawer";

/** The eight pages R2 names, spelled as a failure spells them. */
export type SurfaceName =
  | "Inbox"
  | "Inbox drawer"
  | "project List"
  | "project Board"
  | "Agents page"
  | "Delivery page"
  | "search bar"
  | "Artifacts tab";

/** The six a recording can answer: the Agents page and the Delivery page are read on the real
 *  stack alone, because a recording freezes a session's last-seen time and the delivery
 *  window's dates (`packages/dispatch/AGENTS.md`, "The stubbed layer"). */
export const STUBBABLE_SURFACES: readonly SurfaceName[] = [
  "Inbox",
  "Inbox drawer",
  "project List",
  "project Board",
  "search bar",
  "Artifacts tab",
];

/** The two the real stack reads. */
export const REAL_STACK_SURFACES: readonly SurfaceName[] = ["Agents page", "Delivery page"];

/**
 * The `data-control` value of every control the Design gives each page, and nothing else: a row
 * reads that each of these is drawn and that no other value of the vocabulary is.
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
    CONTROL.panelTrigger,
    CONTROL.chip,
    CONTROL.displayMenu,
    CONTROL.selectionBar,
  ],
  "Inbox drawer": [
    CONTROL.controlRow,
    CONTROL.searchBox,
    CONTROL.panelTrigger,
    CONTROL.chip,
    CONTROL.displayMenu,
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
  "Artifacts tab": [CONTROL.controlRow, CONTROL.searchBox],
};

/** The three controls R1 refuses between a page's title and its first row, outside the one row. */
export const STRIP_CONTROLS: readonly ControlName[] = [
  CONTROL.searchBox,
  CONTROL.panelTrigger,
  CONTROL.chip,
];

/** What the openers need of the seed: the project both project pages read, the issue whose
 *  Artifacts tab is read, and the query `/delivery` is opened on, where the caller has one. */
export interface SurfaceScene {
  readonly project: string;
  readonly issue: string;
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

/**
 * One project of six issues, every one of them holding an open ask and a label, and three
 * artifacts on the first: enough for each of the six stubbable surfaces to have a list with a
 * first row, a narrowing that takes no row off it, and something for the search bar to find.
 *
 * Every issue holds an ask deliberately: **Needs you** is the one narrowing the project List
 * and the Board both draw a chip for, and a seed where it emptied the list would leave R1's
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
  return { issue, project: "CORE" };
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

/** The surface's own display menu, opened unless it is open already: the trigger is found by
 *  its `data-control` inside this surface's row, so a drawer over a page that draws one of its
 *  own opens the drawer's. */
export async function openDisplay(page: Page, within: Locator | Page): Promise<Locator> {
  const trigger = control(controlRow(within), CONTROL.displayMenu);
  if ((await trigger.getAttribute("aria-expanded")) !== "true") await trigger.click();
  const menu = page.getByRole("dialog", { name: "View" });
  await expect(menu).toHaveCount(1);
  return menu;
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
  if ((await trigger.getAttribute("aria-expanded")) === "true") await trigger.click();
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
  if ((await trigger.getAttribute("aria-expanded")) !== "true") await trigger.click();
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
    narrow: () => pickInboxField(page, "type", /^Question,/),
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
async function openIssuesTab(page: Page, project: string, view: "List" | "Board"): Promise<void> {
  await page.goto(`/projects/${project}/issues`);
  const button = page.getByRole("button", { exact: true, name: view });
  await expect(button).toBeVisible();
  if ((await button.getAttribute("aria-pressed")) !== "true") await button.click();
  await expect(button).toHaveAttribute("aria-pressed", "true");
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
    narrow: () => pickInboxField(page, "type", /^Question,/, root),
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
      if ((await trigger.getAttribute("aria-expanded")) !== "true") await trigger.click();
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
    // keeps a first row.
    narrow: async () => {
      await openFilterPanel(page, root);
      const trigger = page.getByRole("dialog", { name: "Filter" }).getByRole("button", {
        name: "Repository",
      });
      if ((await trigger.getAttribute("aria-expanded")) !== "true") await trigger.click();
      await page.getByRole("listbox").getByRole("option").first().click();
      await trigger.click();
      await closeControlPanel(root);
    },
    panel: async () => {
      await openFilterPanel(page, root);
    },
  };
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
        .getByRole("dialog", { name: "Status" })
        .getByRole("button", { name: "Todo" })
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

/** Opens one of the eight surfaces and hands back what a row reads it through. `phone` is
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
  }
}
