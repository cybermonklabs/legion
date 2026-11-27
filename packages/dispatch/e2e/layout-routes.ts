/** The pages the layout layer reads against the layout rule: every reviewed page of
 *  `e2e/review-routes.ts`, the pages that list excludes only because a pixel comparison or a scan
 *  would not read them the same twice, two pages a reader's choice in this browser draws another
 *  way - the new issue page and the project's board - and the project page and its Architecture
 *  tab, which draw from the architecture source the stress layer syncs on the server. A route may
 *  also carry states - what the page draws once the reader has acted on it - each read at every
 *  width after the plain load.
 *
 *  `e2e/layout-rule.stub.e2e.ts` reads them all, against a recording of their HTTP answers and of
 *  the document and live sockets they draw from (`e2e/layout-sockets.ts`), on every pull request.
 *
 *  Data and locators only, as `e2e/review-routes.ts` is, so `e2e/__tests__/layout-routes.test.ts`
 *  reads it without a browser or a harness: its imports are Playwright's types, the review
 *  table, and `./issue-page-option`, which imports a type alone. */
import type { BrowserContext, Locator, Page } from "@playwright/test";

import { withIssuePage } from "./issue-page-option";
import { hasReadingPane } from "./reading-pane";
import { type ReviewedRoute, type ReviewSeed, reviewRoutes } from "./review-routes";
import type { WalkRoutePath } from "./two-builds-routes";

/** The ids the stress layer (`e2e/layout-seed.ts`) adds to the review corpus's. */
export interface LayoutSeed extends ReviewSeed {
  /** The live session `/agents/:sessionId/live` and `/agents/:sessionId/artifacts/:slug` open. */
  readonly agentSession: string;
  /** The picture uploaded to that session's conversation. */
  readonly agentArtifactSlug: string;
  /** When the stress layer finished writing, ISO 8601. The rows pin their page clock a minute
   *  after it, so every relative time a page draws ("2 minutes ago") reads the same gap on every
   *  run, and a replay of an old recording does not widen it to "3 weeks ago". */
  readonly seededAt: string;
}

/** The name of that picture, which its page draws as its heading. */
export const AGENT_PICTURE = "layout-stress-screenshot-of-the-issue-header-at-1280.png";
/** The first words of the live session's reply, which its live view draws once the replay lands. */
export const AGENT_REPLY_OPENING = "The audit is read.";
/** The repository CORE's architecture source is read from. */
export const ARCHITECTURE_REPO = "acme/layout-architecture";
/** The stress issue's title and labels, as `e2e/layout-seed.ts` writes them (its `prose(…, 170)`
 *  title, and these labels): the new issue page's ready locators name them, so a page that drew
 *  another issue, or this one without its stress data, never settles. */
export const STRESS_TITLE =
  "Local testing workflow for Dispatch, with every header property production holds - measured on production at every width from 360 to 1920 pixels, where a row of elements";
export const STRESS_LABELS = [
  "dispatch",
  "layout",
  "issue-header",
  "inbox",
  "accessibility",
  "regression",
  "phone-width",
  "needs-design-review",
  "customer-visible",
  "performance-budget",
  "browser-test",
  "priority-escalation",
  // A label of 40 wide characters with no break, the most the server takes (`maxIssueLabel16`).
  "mw".repeat(20),
];

/** Opens every closed `CappedGroup` in `scope`, one "+N" press at a time, until none is closed,
 *  and answers how many presses that took. It reads the component's own contract: a closed group
 *  is `[data-layout-group]` with no `data-layout-open`, and its "+N" control is the button marked
 *  `data-layout-more` (or the one inside the `li` so marked, for a named list), which reads
 *  `aria-expanded="false"` while the group is closed and names the group in `aria-controls`.
 *  Each press waits for its own group to open before the next is read, and a press may draw
 *  groups that were folded inside the one it opened, which the next round finds. `group` keeps
 *  it to the groups of that name. Past `maxPresses` presses it throws, since a group that never
 *  stays open would otherwise be pressed for ever. */
export async function openCappedGroups(
  scope: Page | Locator,
  { group, maxPresses = 50 }: { readonly group?: string; readonly maxPresses?: number } = {}
): Promise<number> {
  const groups =
    group === undefined
      ? "[data-layout-group]:not([data-layout-open])"
      : `[data-layout-group=${JSON.stringify(group)}]:not([data-layout-open])`;
  const closed = scope
    .locator(
      `${groups} :is(button[data-layout-more], [data-layout-more] > button)[aria-expanded="false"]`
    )
    .filter({ visible: true });
  for (let presses = 0; ; presses += 1) {
    // The caller settled the page, and each press below waits for its own group to open.
    const left = await closed.count();
    if (left === 0) return presses;
    if (presses >= maxPresses) {
      throw new Error(
        `openCappedGroups: ${left} group(s) still closed after ${maxPresses} presses of "+N"`
      );
    }
    const control = closed.first();
    // The control's own attribute, read once to find the group this one press opens.
    const opened = await control.getAttribute("aria-controls");
    await control.click();
    await scope
      .locator(`[id=${JSON.stringify(opened ?? "")}][data-layout-open]`)
      .waitFor({ state: "attached" });
  }
}

/** What the page draws once the reader has acted on it: a drawer open, rows selected, a group
 *  expanded. `act` runs on the settled page and returns once what it opened is drawn. */
export interface LayoutState {
  /** The state's own name, which a breach list entry names it by. */
  readonly name: string;
  readonly act: (page: Page) => Promise<void>;
  /** The `data-layout-group` names the page must mark in this state beyond its route's own: a
   *  state draws on the page its route loads, so a state reading requires the route's groups
   *  (`LayoutRoute.requiredGroups`) and these (`requiredGroupsOf` in `e2e/layout-check.ts`). */
  readonly requiredGroups?: readonly string[];
}

export interface LayoutRoute {
  /** The route's own name, which a row's title and a breach list entry name it by. */
  readonly name: string;
  readonly pattern: WalkRoutePath;
  readonly path: (seed: LayoutSeed) => string;
  /** What has to be on the screen before the page has settled (`waitForRouteSettled`). */
  readonly ready: (page: Page) => readonly Locator[];
  /** A choice the reader made in this browser that the page reads, set on the row's context
   *  before its first load. */
  readonly prepare?: (context: BrowserContext) => Promise<void>;
  /** The states read after the plain load, at every width. */
  readonly states?: readonly LayoutState[];
  /** The `data-layout-group` names the page must mark as it loads with the stress layer, so a
   *  growing group nobody marked fails rather than passing unmeasured. */
  readonly requiredGroups?: readonly string[];
  /** The fewest elements the page's own region (`[data-testid="main-content"]`) holds once drawn,
   *  where it is fewer than `MIN_ELEMENTS` (`e2e/layout-check.ts`), with why. The route's ready
   *  locators still have to be on the screen first, so a sign-in page or a skeleton still fails. */
  readonly minElements?: { readonly count: number; readonly reason: string };
}

/** The Peek drawer, the Inbox over the page it is opened from: the compact header's Peek below
 *  the wide layout, the sidebar's from it up (`app.tsx`, `Sidebar.tsx`). */
const peekDrawer: LayoutState = {
  act: async (page) => {
    await page
      .getByRole("button", { name: "Peek at the inbox without leaving this page" })
      .filter({ visible: true })
      .first()
      .click();
    const drawer = page.getByTestId("inbox-drawer");
    await drawer.getByRole("checkbox").first().waitFor();
  },
  name: "peek-drawer",
};

/** Two rows of the issues table selected, which draws the selection toolbar over the page. */
const twoRowsSelected: LayoutState = {
  act: async (page) => {
    const rows = page.locator("#project-issues-panel tr[data-issue-row]");
    for (const index of [0, 1]) {
      await rows.nth(index).getByRole("checkbox").check();
    }
    await page.getByRole("toolbar").filter({ hasText: "2 selected" }).waitFor();
  },
  name: "two-rows-selected",
};

/** Every agent selected for a broadcast, which draws the composer and its list of chosen agents
 *  (`AgentsPage.tsx`'s "Selected agents"): the stress layer's 17 sessions are past its cap. */
const allAgentsSelected: LayoutState = {
  act: async (page) => {
    await page.getByRole("checkbox", { name: "Select all matching agents" }).check();
    await page.getByRole("region", { exact: true, name: "Broadcast" }).waitFor();
  },
  name: "all-agents-selected",
  requiredGroups: ["chosen-agents"],
};

/** The first row of the Inbox opened, the way the pointer opens it: below the wide layout its
 *  reading view stands in place of the list (INFRA-5200), from it up the reading column shows it
 *  beside the list. */
const inboxItemOpened: LayoutState = {
  act: async (page) => {
    const row = page.locator("[data-inbox-row]").first();
    const id = await row.getAttribute("data-inbox-row");
    await row.locator("time").first().click();
    await (hasReadingPane(page)
      ? page.locator(`[data-inbox-row="${id}"][data-inbox-selected="true"]`)
      : page.locator(`[data-inbox-reading-row="${id}"]`)
    ).waitFor();
  },
  name: "item-opened",
};

/** That row opened and left again by the reading view's own Inbox control, which brings the
 *  list back at the row: below the wide layout alone, where the list gave way to the view; from
 *  it up the list never left, so this reads the opened row beside it. */
const inboxBackToList: LayoutState = {
  act: async (page) => {
    await inboxItemOpened.act(page);
    if (hasReadingPane(page)) return;
    await page
      .getByTestId("inbox-reading-view")
      .getByRole("button", { exact: true, name: "Back to the Inbox list" })
      .click();
    await page.getByTestId("inbox-reading-view").waitFor({ state: "detached" });
    await page.getByTestId("inbox-list").waitFor();
  },
  name: "back-to-list",
};

/** The lists of chips today's issue page draws from the stress issue's data: its 13 labels, its
 *  13 components, its 15 pull requests, and its links that are no pull request. Each is a list of
 *  R3, which `components/CappedGroup.tsx` marks with these names. */
const ISSUE_GROUPS = ["labels", "components", "pull-requests", "links"];

/** A short page's floor: four fifths of what its own region drew at its narrowest width on the
 *  stress seed, so a page that drew a heading and nothing else still fails. */
function shortPage(measured: number): NonNullable<LayoutRoute["minElements"]> {
  return {
    count: Math.max(3, Math.floor(measured * 0.8)),
    reason: `a short page: ${measured} elements in its own region at its narrowest, on the stress seed`,
  };
}

/** What a reviewed route adds here: its states, the groups it must mark and its floor. */
const additions: Readonly<
  Record<string, Pick<LayoutRoute, "minElements" | "requiredGroups" | "states">>
> = {
  agents: { requiredGroups: ["roles"], states: [peekDrawer, allAgentsSelected] },
  answers: { minElements: shortPage(32) },
  broadcast: { minElements: shortPage(48) },
  broadcasts: { minElements: shortPage(14) },
  "credentials-machine": { minElements: shortPage(24) },
  "issue-children": { requiredGroups: [...ISSUE_GROUPS, "child-links"] },
  inbox: { states: [inboxItemOpened, inboxBackToList] },
  machine: { minElements: shortPage(46) },
  machines: { minElements: shortPage(15) },
  "not-found": { minElements: shortPage(4) },
  person: { minElements: shortPage(27) },
  // The Labels cell of the stress issue's row in the priority's issues table: its 13 labels.
  priority: { requiredGroups: ["labels"] },
  "project-document": { minElements: shortPage(49) },
  "project-documents": { minElements: shortPage(40) },
  // The Labels cell of the stress issue's row: its 13 labels, a list of R3.
  "project-issues": { requiredGroups: ["labels"], states: [twoRowsSelected] },
};

const reviewed: readonly LayoutRoute[] = reviewRoutes
  .filter((route): route is ReviewedRoute => route.kind === "reviewed")
  .map(({ name, path, pattern, ready }) => ({
    name,
    path,
    pattern,
    ready,
    // Today's issue page, whichever tab: the header draws the three lists on every one.
    ...(name.startsWith("issue-") ? { requiredGroups: ISSUE_GROUPS } : {}),
    ...additions[name],
  }));

/** The routes the review table excludes that this layer does not read either, each with why.
 *  Every other excluded route is read below; `e2e/__tests__/layout-routes.test.ts` holds the two
 *  lists to the table. */
export const layoutUnread: Readonly<Record<string, string>> = {
  credentials:
    "It draws InboxPage, the same component as / (web/src/app.tsx), which the inbox route reads.",
  run:
    "A command bar run's own page draws only for the reader who asked the run, and the layout " +
    "stress seed asks no run. Every other reader gets NotFoundPage, which this layer reads at " +
    "its not-found route. The run's page is drawn at 1440 px and at 390 px, and read to fit " +
    "each width with nothing cut, by e2e/issue-page-z.e2e.ts's row 'the run's page is its " +
    "asker's alone'.",
};

export const layoutRoutes: readonly LayoutRoute[] = [
  ...reviewed,
  {
    name: "issue-spec-outline",
    // The new issue page: today's page unless the reader chose it, in this browser.
    path: (seed) => `/issues/${seed.issue}/spec`,
    pattern: "/issues/:key/*",
    prepare: (context) => withIssuePage(context, "alice"),
    // The page's root draws in its first frame, saying "Loading issue…"; the column and the
    // title are drawn once its read of the issue answered (`e2e/issue-page.ts`'s `gotoIssue`).
    ready: (page) => [
      page.getByTestId("issue-column"),
      // The stress issue's whole title, and its labels: one of them drawn, or - where the page
      // folds every one of them by width under INFRA-1196's R13, as at 360 px - its count of all
      // thirteen.
      page
        .locator("[data-issue-page]")
        .getByRole("heading", { exact: true, level: 1, name: STRESS_TITLE }),
      page
        .locator("[data-issue-page]")
        .getByText(new RegExp(`^(${STRESS_LABELS.join("|")})$`))
        .filter({ visible: true })
        .or(
          page
            .locator("[data-issue-page]")
            .getByRole("button", { exact: true, name: `+${STRESS_LABELS.length} labels` })
        )
        .first(),
    ],
  },
  {
    name: "project-board",
    // The project's board: its list unless the reader chose the board, in this browser.
    path: (seed) => `/projects/${seed.project}/issues`,
    pattern: "/projects/:key/issues",
    prepare: async (context) => {
      await context.addInitScript(() => {
        try {
          window.localStorage.setItem("dispatch.project.issue-view:alice", "board");
        } catch {
          // This frame has no storage of its own, and the page reads none from it.
        }
      });
    },
    ready: (page) => [page.locator("[data-board-card]").first()],
    // The stress issue's card draws its 13 labels, a list of R3.
    requiredGroups: ["labels"],
  },
  {
    name: "project",
    // Excluded from the review table because it redirects with no architecture source; the
    // stress layer gives CORE one, so it opens on its Architecture tab.
    path: (seed) => `/projects/${seed.project}`,
    pattern: "/projects/:key",
    ready: (page) => [
      page.getByRole("heading", { exact: true, level: 1, name: "Core" }),
      page.getByRole("tab", { name: "Architecture", selected: true }),
    ],
  },
  {
    name: "project-architecture",
    // Excluded from the review table for want of a synced architecture source, which the
    // stress layer syncs.
    path: (seed) => `/projects/${seed.project}/architecture`,
    pattern: "/projects/:key/architecture",
    ready: (page) => [
      page.getByTestId("architecture-source").filter({ hasText: `${ARCHITECTURE_REPO}/main` }),
    ],
  },
  {
    minElements: shortPage(45),
    name: "agent-live",
    // Excluded from the review table because its frames land over a live socket and the view
    // scrolls as they do, so no two shots match; a layout reading taken once the replayed turns
    // are drawn reads the same boxes every time.
    path: (seed) => `/agents/${seed.agentSession}/live`,
    pattern: "/agents/:sessionId/live",
    ready: (page) => [
      page.getByTestId("agent-message-assistant").filter({ hasText: AGENT_REPLY_OPENING }),
    ],
  },
  {
    minElements: shortPage(41),
    name: "agent-artifact",
    // Excluded from the review table because its chrome is the issue-artifact page's; the
    // layout rule reads that chrome around a picture, which no other route draws.
    path: (seed) => `/agents/${seed.agentSession}/artifacts/${seed.agentArtifactSlug}`,
    pattern: "/agents/:sessionId/artifacts/:slug",
    ready: (page) => [
      page.getByRole("heading", { name: AGENT_PICTURE }),
      page.getByRole("img", { name: `${AGENT_PICTURE} version 1` }),
    ],
  },
];
