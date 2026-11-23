/** The pages the layout layer reads against the layout rule: every reviewed page of
 *  `e2e/review-routes.ts`, the pages that list excludes only because a pixel comparison or a scan
 *  would not read them the same twice, and three pages a reader's choice in this browser draws
 *  another way: the new issue page, the project's board, and the project page a project with an
 *  architecture source opens on. A route may also carry states - what the page draws once the
 *  reader has acted on it - each read at every width after the plain load.
 *
 *  Two rows read them. `e2e/layout-rule.stub.e2e.ts` reads every route that draws from its HTTP
 *  answers and the live socket's subscription frames, against a recording, on every pull request;
 *  `e2e/layout.e2e.ts` reads, on the real stack, the routes whose `needsStack` says why a
 *  recording cannot draw them.
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
}

/** The name of that picture, which its page draws as its heading. */
export const AGENT_PICTURE = "layout-stress-screenshot-of-the-issue-header-at-1280.png";
/** The first words of the live session's reply, which its live view draws once the replay lands. */
export const AGENT_REPLY_OPENING = "The audit is read.";
/** The repository CORE's architecture source is read from. */
export const ARCHITECTURE_REPO = "acme/layout-architecture";

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
  /** Why a recording cannot draw this route, when it cannot: then the real-stack row reads it. */
  readonly needsStack?: string;
  /** The fewest elements the drawn page holds, where it is fewer than `MIN_ELEMENTS`
   *  (`e2e/layout-check.ts`), with why. The route's ready locators still have to be on the screen
   *  first, so a sign-in page or a skeleton still fails. */
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

/** Why a short page draws fewer than `MIN_ELEMENTS`: below the wide layout the shell draws no
 *  sidebar, so the page is its own few elements and the compact header. */
function narrowShell(measured: number): string {
  return `a short page below 1280 px, where the shell draws no sidebar: ${measured} elements at 360 px on the stress seed`;
}

/** What a reviewed route adds here: its states, the groups it must mark, and why the stack draws
 *  it alone. */
const additions: Readonly<
  Record<string, Pick<LayoutRoute, "minElements" | "needsStack" | "requiredGroups" | "states">>
> = {
  agents: { states: [peekDrawer] },
  answers: { minElements: { count: 40, reason: narrowShell(54) } },
  broadcast: { minElements: { count: 50, reason: narrowShell(70) } },
  broadcasts: { minElements: { count: 25, reason: narrowShell(36) } },
  "credentials-machine": {
    // Measured: 97 elements from 1280 px up on the stack.
    minElements: { count: 40, reason: "a one-field page: the code a machine shows, in the shell" },
  },
  machine: { minElements: { count: 50, reason: narrowShell(68) } },
  machines: { minElements: { count: 25, reason: narrowShell(37) } },
  "not-found": { minElements: { count: 20, reason: narrowShell(26) } },
  pipeline: { minElements: { count: 20, reason: narrowShell(26) } },
  "project-documents": { minElements: { count: 45, reason: narrowShell(62) } },
  "issue-artifact": {
    needsStack:
      "It settles on its document's connected status, which only the `/ws/doc` socket gives, and the stubbed layer answers no `/ws/doc`.",
  },
  "issue-plan": {
    needsStack:
      "Its plan document is drawn over the `/ws/doc` socket, which the stubbed layer does not answer: replayed, the page drew 32 elements fewer than on the stack at every width (358 against 390 at 360 px).",
  },
  "issue-verification": {
    needsStack:
      "It settles on the reference its Verification document draws over the `/ws/doc` socket, which the stubbed layer does not answer, so a replay never settles.",
  },
  "issue-spec": {
    needsStack:
      "It settles on its document's connected status, which only the `/ws/doc` socket gives, and the stubbed layer answers no `/ws/doc`.",
  },
  "project-document": {
    needsStack:
      "It settles on its document's connected status, which only the `/ws/doc` socket gives, and the stubbed layer answers no `/ws/doc`.",
  },
  "project-issues": { states: [twoRowsSelected] },
};

const reviewed: readonly LayoutRoute[] = reviewRoutes
  .filter((route): route is ReviewedRoute => route.kind === "reviewed")
  .map(({ name, path, pattern, ready }) => ({ name, path, pattern, ready, ...additions[name] }));

/** The Inbox, read again on the real stack: from the wide layout up its reading pane draws the
 *  selected row's document, which a recording does not draw (replayed, 341 elements against 416
 *  at 1280 px), so the stubbed `inbox` row reads the list and this one reads the pane too. */
const inboxOnStack: readonly LayoutRoute[] = reviewed
  .filter((route) => route.name === "inbox")
  .map((route) => ({
    ...route,
    name: "inbox-on-stack",
    needsStack:
      "From 1280 px up its reading pane draws the selected row's document, which a replay does not: 341 elements against 416 on the stack.",
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
  ...inboxOnStack,
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
      page.locator("[data-issue-page]").getByRole("heading", { level: 1 }),
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
    minElements: { count: 50, reason: narrowShell(68) },
    name: "agent-live",
    // Excluded from the review table because its frames land over a live socket and the view
    // scrolls as they do, so no two shots match; a layout reading taken once the replayed turns
    // are drawn reads the same boxes every time.
    needsStack:
      "Its turns arrive as the session's replay over the live relay, and the stubbed layer's `/ws/live` answers a subscription with the contract fixtures' opening frame alone (`e2e/stub/live-frames.ts`).",
    path: (seed) => `/agents/${seed.agentSession}/live`,
    pattern: "/agents/:sessionId/live",
    ready: (page) => [
      page.getByTestId("agent-message-assistant").filter({ hasText: AGENT_REPLY_OPENING }),
    ],
  },
  {
    minElements: { count: 45, reason: narrowShell(63) },
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
