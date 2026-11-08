/** The pages the review layers open, one row per page: the router path it stands for, the URL to
 *  open against `e2e/review-seed.ts`'s corpus, and the locator that says the page is drawn. Two
 *  layers read it - an accessibility pass and a containerised visual review - so the count of
 *  pages each covers is a program's answer rather than a sentence in a plan, and
 *  `e2e/__tests__/review-routes.test.ts` holds every row to the router in `web/src/app.tsx`
 *  through `e2e/two-builds-routes.ts`: a page the router adds is reviewed or excluded with a
 *  reason, never silently unreviewed.
 *
 *  This module holds data and locators only. The visual review runs in a container that carries
 *  no `psql` binary, so nothing here may reach `./psql`, `./seed` or `./delivery-seed`, and its
 *  one import is Playwright's types. */
import type { Locator, Page } from "@playwright/test";

/** The ids the review layers' seed makes, which a route's path is built from. */
export interface ReviewSeed {
  readonly issue: string;
  readonly childIssue: string;
  readonly project: string;
  readonly broadcast: string;
  readonly projectDocumentSlug: string;
  readonly issueArtifactSlug: string;
  /** The `from=…&to=…` query string, without the leading "?". */
  readonly deliveryQuery: string;
  /** The `?at=` value the Pipeline page pins its seven days to. */
  readonly pipelineAt: string;
}

export type ReviewLayer = "scan" | "shoot";

export type ReviewRoute =
  | {
      readonly kind: "reviewed";
      /** The entry's own name, which a row's title and a snapshot's file name are built from. */
      readonly name: string;
      /** The router path this entry stands for, exactly as `e2e/two-builds-routes.ts` writes it. */
      readonly pattern: string;
      readonly path: (seed: ReviewSeed) => string;
      /** Resolves once the page is drawn, so neither layer reads a loading skeleton. */
      readonly ready: (page: Page) => Locator;
      readonly layers: readonly ReviewLayer[];
    }
  | {
      readonly kind: "excluded";
      readonly name: string;
      readonly pattern: string;
      readonly reason: string;
    };

/** Both layers, which is every reviewed page unless its row says otherwise. */
const bothLayers: readonly ReviewLayer[] = ["scan", "shoot"];

export const reviewRoutes: readonly ReviewRoute[] = [
  {
    kind: "reviewed",
    layers: bothLayers,
    name: "inbox",
    path: () => "/",
    pattern: "/",
    ready: (page) => page.getByRole("heading", { exact: true, level: 1, name: "Inbox" }),
  },
  {
    kind: "reviewed",
    layers: bothLayers,
    name: "answers",
    path: () => "/answers",
    pattern: "/answers",
    ready: (page) => page.getByRole("heading", { name: "Answered by you" }),
  },
  {
    kind: "reviewed",
    layers: bothLayers,
    name: "agents",
    path: () => "/agents",
    pattern: "/agents",
    // Exact: the page draws `<section aria-label="Agents empty state">` inside the region when
    // the registry holds nobody, and a substring match would then find two.
    ready: (page) => page.getByRole("region", { exact: true, name: "Agents" }),
  },
  {
    kind: "reviewed",
    layers: bothLayers,
    name: "broadcasts",
    path: () => "/agents/broadcasts",
    pattern: "/agents/broadcasts",
    ready: (page) => page.getByRole("region", { name: "Broadcasts" }),
  },
  {
    kind: "reviewed",
    layers: bothLayers,
    name: "broadcast",
    path: (seed) => `/agents/broadcasts/${seed.broadcast}`,
    pattern: "/agents/broadcasts/:id",
    ready: (page) => page.getByRole("region", { name: "Broadcast" }),
  },
  {
    kind: "reviewed",
    layers: bothLayers,
    name: "issue-spec",
    path: (seed) => `/issues/${seed.issue}/spec`,
    pattern: "/issues/:key/*",
    // The issue header's own h1, as `e2e/theme.e2e.ts` reads it: the page's other headings
    // belong to the spec's own body.
    ready: (page) => page.getByTestId("issue-header").getByRole("heading", { level: 1 }),
  },
  {
    kind: "reviewed",
    layers: bothLayers,
    name: "issue-conversation",
    path: (seed) => `/issues/${seed.issue}/conversation`,
    pattern: "/issues/:key/*",
    ready: (page) => page.getByRole("list", { name: "Conversation turns" }),
  },
  {
    kind: "reviewed",
    layers: bothLayers,
    name: "issue-children",
    path: (seed) => `/issues/${seed.issue}/children`,
    pattern: "/issues/:key/*",
    // The child's own row, not the panel: the panel is in the DOM while another tab is open.
    ready: (page) => page.locator("#issue-children-panel").getByRole("listitem").first(),
  },
  {
    kind: "reviewed",
    layers: bothLayers,
    name: "issue-artifacts",
    path: (seed) => `/issues/${seed.issue}/artifacts`,
    pattern: "/issues/:key/*",
    // The primary document's row. An issue's spec is `spec.md` under the slug `spec`
    // (`issue_create.go`), so the row's test id is `artifact-spec`.
    ready: (page) => page.getByTestId("artifact-spec"),
  },
  {
    kind: "reviewed",
    layers: bothLayers,
    name: "issue-activity",
    path: (seed) => `/issues/${seed.issue}/activity`,
    pattern: "/issues/:key/*",
    ready: (page) => page.getByRole("list", { name: "Issue activity" }),
  },
  {
    kind: "reviewed",
    layers: bothLayers,
    name: "issue-plan",
    path: (seed) => `/issues/${seed.issue}/plan`,
    pattern: "/issues/:key/*",
    ready: (page) => page.getByTestId("plan-tab"),
  },
  {
    kind: "reviewed",
    layers: bothLayers,
    name: "issue-verification",
    path: (seed) => `/issues/${seed.issue}/verification`,
    pattern: "/issues/:key/*",
    ready: (page) => page.getByTestId("verification-tab"),
  },
  {
    kind: "reviewed",
    layers: bothLayers,
    name: "issue-decisions",
    path: (seed) => `/issues/${seed.issue}/decisions`,
    pattern: "/issues/:key/*",
    ready: (page) => page.getByRole("region", { name: "Waiting for your review" }),
  },
  {
    kind: "reviewed",
    layers: bothLayers,
    name: "issue-artifact",
    path: (seed) => `/issues/${seed.issue}/artifacts/${seed.issueArtifactSlug}`,
    pattern: "/issues/:key/*",
    ready: (page) => page.getByTestId("artifact-header"),
  },
  {
    kind: "reviewed",
    layers: bothLayers,
    name: "project-issues",
    path: (seed) => `/projects/${seed.project}/issues`,
    pattern: "/projects/:key/issues",
    ready: (page) => page.locator("#project-issues-panel li[data-issue-row]").first(),
  },
  {
    kind: "reviewed",
    layers: bothLayers,
    name: "project-documents",
    path: (seed) => `/projects/${seed.project}/documents`,
    pattern: "/projects/:key/documents",
    ready: (page) => page.locator("#project-documents-panel").getByRole("listitem").first(),
  },
  {
    kind: "reviewed",
    layers: bothLayers,
    name: "project-document",
    path: (seed) => `/projects/${seed.project}/documents/${seed.projectDocumentSlug}`,
    pattern: "/projects/:key/documents/:slug",
    ready: (page) => page.getByTestId("artifact-header"),
  },
  {
    kind: "reviewed",
    layers: bothLayers,
    name: "settings",
    path: () => "/settings",
    pattern: "/settings",
    ready: (page) => page.getByRole("heading", { level: 1, name: "Settings" }),
  },
  {
    kind: "reviewed",
    layers: bothLayers,
    name: "delivery",
    path: (seed) => `/delivery?${seed.deliveryQuery}`,
    pattern: "/delivery",
    ready: (page) => page.getByRole("heading", { name: "Delivery timeline" }),
  },
  {
    kind: "reviewed",
    layers: bothLayers,
    name: "pipeline",
    path: (seed) => `/pipeline?at=${seed.pipelineAt}`,
    pattern: "/pipeline",
    ready: (page) => page.getByRole("heading", { level: 1, name: "Pipeline" }),
  },
  {
    kind: "reviewed",
    layers: bothLayers,
    name: "not-found",
    path: () => "/no-such-page",
    pattern: "*",
    ready: (page) => page.getByRole("heading", { name: "Page not found" }),
  },
  {
    kind: "excluded",
    name: "project",
    pattern: "/projects/:key",
    reason:
      "With no architecture source it redirects to /projects/:key/issues, which the " +
      "project-issues entry already reviews (e2e/architecture.e2e.ts).",
  },
  {
    kind: "excluded",
    name: "project-architecture",
    pattern: "/projects/:key/architecture",
    reason:
      "It needs a fake-GitHub repository and a synced architecture source, and without one the " +
      "tab does not render at all (e2e/architecture.e2e.ts).",
  },
  {
    kind: "excluded",
    name: "agent-live",
    pattern: "/agents/:sessionId/live",
    reason:
      "Its content arrives over a live socket as stream frames and the view auto-scrolls as " +
      "they land, so neither a scan nor a shot would read the same page twice " +
      "(e2e/agent-view.e2e.ts).",
  },
  {
    kind: "excluded",
    name: "agent-artifact",
    pattern: "/agents/:sessionId/artifacts/:slug",
    reason:
      "It draws the same ArtifactHeader chrome the issue-artifact and project-document entries " +
      "already review, over an uploaded picture.",
  },
  {
    kind: "excluded",
    name: "machines",
    pattern: "/machines",
    reason:
      "Its rows are built from the live registry's freshness, which moves between runs " +
      "(e2e/workspace.ts).",
  },
  {
    kind: "excluded",
    name: "machine",
    pattern: "/machines/:machine",
    reason:
      "It draws a pixel layout computed from a tmux layout string, which the seed carries none " +
      "of (e2e/machines.e2e.ts).",
  },
  {
    kind: "excluded",
    name: "credentials",
    pattern: "/credentials/*",
    reason: "It draws InboxPage, the same component as / (web/src/app.tsx).",
  },
  {
    kind: "excluded",
    name: "credentials-machine",
    pattern: "/credentials/machine",
    reason:
      "It is a one-field form whose only non-trivial state is a capability probe that is " +
      "briefly out, which is what a shot would flake on (e2e/machine-login.e2e.ts).",
  },
];

const reviewed: readonly Extract<ReviewRoute, { kind: "reviewed" }>[] = reviewRoutes.filter(
  (route): route is Extract<ReviewRoute, { kind: "reviewed" }> => route.kind === "reviewed"
);

export const scannedRoutes: readonly Extract<ReviewRoute, { kind: "reviewed" }>[] = reviewed.filter(
  (route) => route.layers.includes("scan")
);

export const shotRoutes: readonly Extract<ReviewRoute, { kind: "reviewed" }>[] = reviewed.filter(
  (route) => route.layers.includes("shoot")
);

/** The desktop routes a phone viewport draws differently, a subset of `shotRoutes` by name.
 *
 *  A phone width is one under 768 px (`packages/dispatch/AGENTS.md`, "Phone acceptance"), where
 *  the shell hides the sidebar behind the navigation drawer and shows the compact top bar. That
 *  alone changes every page, so the three named here are the ones whose own content the width
 *  rearranges rather than only its frame: the Inbox becomes the compact row list with no reading
 *  pane beside it, the Conversation docks its composer at the foot of the screen and opens a
 *  comment's thread full-screen over the list, and the Spec's margin becomes a bottom sheet
 *  instead of a column beside the document. */
export const phoneShotNames: readonly string[] = ["inbox", "issue-conversation", "issue-spec"];
