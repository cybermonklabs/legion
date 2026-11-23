/** The pages the layout layer reads against the layout rule: every reviewed page of
 *  `e2e/review-routes.ts`, the pages that list excludes only because a pixel comparison or a scan
 *  would not read them the same twice, and three pages a reader's choice in this browser draws
 *  another way: the new issue page, the project's board, and the project page a project with an
 *  architecture source opens on. A route may also carry states - what the page draws once the
 *  reader has acted on it - each read at every width after the plain load.
 *
 *  `e2e/layout-rule.stub.e2e.ts` reads them all, against a recording of their HTTP answers and of
 *  the document and live sockets they draw from (`e2e/layout-sockets.ts`), on every pull request.
 *
 *  Data and locators only, as `e2e/review-routes.ts` is, so `e2e/__tests__/layout-routes.test.ts`
 *  reads it without a browser or a harness: its imports are Playwright's types, the review
 *  table, and `./issue-page-option`, which imports a type alone. */
import type { BrowserContext, Locator, Page } from "@playwright/test";

import { withIssuePage } from "./issue-page-option";
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
 *  title and its `LABELS`): the new issue page's ready locators name them, so a page that drew
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
];

/** What the page draws once the reader has acted on it: a drawer open, rows selected, a group
 *  expanded. `act` runs on the settled page and returns once what it opened is drawn. */
export interface LayoutState {
  /** The state's own name, which a breach list entry names it by. */
  readonly name: string;
  readonly act: (page: Page) => Promise<void>;
  /** The `data-layout-group` names the page must mark in this state. */
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

/** The lists of chips today's issue page draws from the stress issue's data: its 12 labels, its
 *  13 components and its 15 pull requests. Each is a list of R3, which `components/CappedGroup.tsx`
 *  marks with these names. */
const ISSUE_GROUPS = ["labels", "components", "pull-requests"];

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
  machine: { minElements: shortPage(46) },
  machines: { minElements: shortPage(15) },
  "not-found": { minElements: shortPage(4) },
  "project-document": { minElements: shortPage(49) },
  "project-documents": { minElements: shortPage(40) },
  "project-issues": { states: [twoRowsSelected] },
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
      // twelve.
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
