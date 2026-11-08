/** The pages the review layers open, one row per page: the router path it stands for, the URL to
 *  open against `e2e/review-seed.ts`'s corpus, and the locator that says the page is drawn. Two
 *  layers read it - an accessibility pass and a containerised visual review - so the count of
 *  pages each covers is a program's answer rather than a sentence in a plan, and
 *  `e2e/__tests__/review-routes.test.ts` holds every row to the router in `web/src/app.tsx`
 *  through `e2e/two-builds-routes.ts`: a page the router adds is reviewed or excluded with a
 *  reason, never silently unreviewed.
 *
 *  This module holds data, locators and the one wait built from them. The visual review runs in
 *  a container that carries no `psql` binary, so nothing here may reach `./psql`, `./seed` or
 *  `./delivery-seed`: its only imports are `@playwright/test` - its types and `expect`, which
 *  `waitForRouteSettled` asserts with - and `./two-builds-routes`'s route paths, and that
 *  module imports nothing at all. */
import { expect, type Locator, type Page } from "@playwright/test";

import type { WalkRoutePath } from "./two-builds-routes";

/** The ids the review layers' seed makes, which a route's path is built from. */
export interface ReviewSeed {
  readonly issue: string;
  readonly project: string;
  /** A machine of the seeded registry, which `/machines/:machine` is opened on. */
  readonly machine: string;
  readonly broadcast: string;
  readonly projectDocumentSlug: string;
  readonly issueArtifactSlug: string;
  /** The `from=…&to=…` query string, without the leading "?". */
  readonly deliveryQuery: string;
  /** The `?at=` value the Pipeline page pins its seven days to. */
  readonly pipelineAt: string;
  /** When `e2e/visual-seed.ts` finished writing this corpus, ISO 8601. The visual rows pin
   *  their page clock a little after it, so every relative label the pages draw reads the same
   *  on every run; the accessibility rows, which seed per row and compare no pixels, ignore it
   *  and `e2e/review-seed.ts` does not set it. */
  readonly seededAt?: string;
}

export type ReviewLayer = "scan" | "shoot";

export type ReviewRoute =
  | {
      readonly kind: "reviewed";
      /** The entry's own name, which a row's title and a snapshot's file name are built from. */
      readonly name: string;
      /** The router path this entry stands for, exactly as `e2e/two-builds-routes.ts` writes
       *  it: typed as that list's own paths, so a pattern no router route has is a compile
       *  error and `e2e/__tests__/review-routes.test.ts` is left the direction no type can
       *  express - that every router route is covered. */
      readonly pattern: WalkRoutePath;
      readonly path: (seed: ReviewSeed) => string;
      /** Everything that has to be on the screen before this page has settled, one locator per
       *  fact: a page draws in parts, and the part that arrives last is not always the part a
       *  heading proves. Each row's comment says what each of its locators proves, and
       *  `waitForRouteSettled` is what both layers wait on, never one of these alone. */
      readonly ready: (page: Page) => readonly Locator[];
      readonly layers: readonly ReviewLayer[];
    }
  | {
      readonly kind: "excluded";
      readonly name: string;
      readonly pattern: WalkRoutePath;
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
    // The page's own heading, and - where the reader's column is drawn at all - the control
    // the column's head offers only once its document is drawn and placed at the block the row
    // is about ("Skip to the answer", `web/src/features/inbox/PaneDocument.tsx`). The column
    // waits `DOCUMENT_SETTLE_MS` before it reads its document, so a page read on the heading
    // alone caught the head without that control in one run of the drift probe and with it in
    // the next. The column shows from 1280 px and nowhere narrower (`READING_PANE_QUERY`,
    // `web/src/features/shell/useDialog.ts`), so the phone row, whose Inbox is the list of
    // cards alone, has nothing there to wait for.
    ready: (page) => {
      const heading = page.getByRole("heading", { exact: true, level: 1, name: "Inbox" });
      const wideEnoughForPane = (page.viewportSize()?.width ?? 0) >= 1280;
      return wideEnoughForPane
        ? [heading, page.getByRole("button", { name: "Skip to the answer" })]
        : [heading];
    },
  },
  {
    kind: "reviewed",
    layers: bothLayers,
    name: "answers",
    path: () => "/answers",
    pattern: "/answers",
    ready: (page) => [page.getByRole("heading", { name: "Answered by you" })],
  },
  {
    kind: "reviewed",
    layers: bothLayers,
    name: "agents",
    path: () => "/agents",
    pattern: "/agents",
    // Exact: the page draws `<section aria-label="Agents empty state">` inside the region when
    // the registry holds nobody, and a substring match would then find two.
    ready: (page) => [page.getByRole("region", { exact: true, name: "Agents" })],
  },
  {
    kind: "reviewed",
    layers: bothLayers,
    name: "broadcasts",
    path: () => "/agents/broadcasts",
    pattern: "/agents/broadcasts",
    ready: (page) => [page.getByRole("region", { name: "Broadcasts" })],
  },
  {
    kind: "reviewed",
    layers: bothLayers,
    name: "broadcast",
    path: (seed) => `/agents/broadcasts/${seed.broadcast}`,
    pattern: "/agents/broadcasts/:id",
    ready: (page) => [page.getByRole("region", { name: "Broadcast" })],
  },
  {
    kind: "reviewed",
    layers: bothLayers,
    name: "issue-spec",
    path: (seed) => `/issues/${seed.issue}/spec`,
    pattern: "/issues/:key/*",
    // Two facts, each of which one run of the drift probe caught this page without.
    //
    // The document's connection dot once its socket is up: its accessible name is its `title`
    // (`web/src/features/doc/ConnectionDot.tsx`), which reads "connected" only in that state,
    // so it matches nothing while the page still draws "Connecting to the document…". The dot
    // is drawn by the spec's own toolbar (`SpecToolbar.tsx`), which is drawn with the
    // document, so it proves the header and the editor are on the screen as well as
    // connected. Spelled here rather than imported from `./editor`, which locates the same
    // dot: `./editor` reaches no `./psql`, so the container rule would allow it, but it pulls
    // `@hocuspocus/provider`, `lib0` and `web/src/api/live-socket` - and so React and
    // @tanstack/react-query - into a module of data and locators that a bun unit test imports.
    //
    // And the margin's decision card with the block its decision shaped named by that block's
    // own words. The card arrives with the decisions view, after the document; its preview
    // then holds the reference as the reader wrote it (`dispatch://CORE-1/spec#b-<uuid>`)
    // until the reference graph answers, and the words replace it
    // (`web/src/features/refs/MarkdownPreview.tsx`). Both states were shot. The words are the
    // spec block `e2e/review-seed.ts` points the decision at.
    //
    // The margin is a column beside the document from 1280 px and a bottom sheet below it, shut
    // until a button opens it (`packages/dispatch/AGENTS.md`, "Phone acceptance"), so the card
    // is on the screen at a desktop width and nowhere narrower. The phone row of this page
    // waits on the dot alone, as the Inbox row waits on its heading alone.
    ready: (page) => {
      const connected = page.getByRole("status", { name: "connected" });
      const wideEnoughForMargin = (page.viewportSize()?.width ?? 0) >= 1280;
      return wideEnoughForMargin
        ? [
            connected,
            page
              .locator("[data-margin-item]")
              .getByText("One command, a seeded corpus, a signed-in browser."),
          ]
        : [connected];
    },
  },
  {
    kind: "reviewed",
    layers: bothLayers,
    name: "issue-conversation",
    path: (seed) => `/issues/${seed.issue}/conversation`,
    pattern: "/issues/:key/*",
    ready: (page) => [page.getByRole("list", { name: "Conversation turns" })],
  },
  {
    kind: "reviewed",
    layers: bothLayers,
    name: "issue-children",
    path: (seed) => `/issues/${seed.issue}/children`,
    pattern: "/issues/:key/*",
    // The child's own row, not the panel: the panel is in the DOM while another tab is open.
    ready: (page) => [page.locator("#issue-children-panel").getByRole("listitem").first()],
  },
  {
    kind: "reviewed",
    layers: bothLayers,
    name: "issue-artifacts",
    path: (seed) => `/issues/${seed.issue}/artifacts`,
    pattern: "/issues/:key/*",
    // The primary document's row. An issue's spec is `spec.md` under the slug `spec`
    // (`issue_create.go`), so the row's test id is `artifact-spec`.
    ready: (page) => [page.getByTestId("artifact-spec")],
  },
  {
    kind: "reviewed",
    layers: bothLayers,
    name: "issue-activity",
    path: (seed) => `/issues/${seed.issue}/activity`,
    pattern: "/issues/:key/*",
    ready: (page) => [page.getByRole("list", { name: "Issue activity" })],
  },
  {
    kind: "reviewed",
    layers: bothLayers,
    name: "issue-plan",
    path: (seed) => `/issues/${seed.issue}/plan`,
    pattern: "/issues/:key/*",
    // The Needs pointer of the second task, drawn as the block's own words once the reference
    // graph answers them and as the reference's short form ("CORE-1 plan-md block") until then
    // (`web/src/features/plan/PlanTab.tsx`'s `BlockLink`, `refs/routes.ts`'s `shortForm`). The
    // tab's own test id is drawn before that answer, which is why the probe read the chip in
    // one run and the title in the other. The words are the plan task `e2e/review-seed.ts`
    // writes, so a change to that corpus fails this row rather than a baseline.
    ready: (page) => [
      page.getByTestId("plan-task-needs").getByRole("link", { name: "Write the route table." }),
    ],
  },
  {
    kind: "reviewed",
    layers: bothLayers,
    name: "issue-verification",
    path: (seed) => `/issues/${seed.issue}/verification`,
    pattern: "/issues/:key/*",
    // The title the editor draws over the bare issue key its document's first line writes
    // ("How CORE-1 is proved."): the key's own characters are hidden and the issue's title is
    // drawn in their place once `issueSummaryQuery` answers (`proof-editor`'s
    // `reference-decorations.ts`, `web/src/features/doc/document-references.tsx`). The tab's
    // own test id is drawn with the empty document, so the probe read the bare key in one run
    // and the title in the other; this waits for the document and that answer together.
    ready: (page) => [page.getByTestId("verification-tab").locator("a.dispatch-reference")],
  },
  {
    kind: "reviewed",
    layers: bothLayers,
    name: "issue-decisions",
    path: (seed) => `/issues/${seed.issue}/decisions`,
    pattern: "/issues/:key/*",
    ready: (page) => [page.getByRole("region", { name: "Waiting for your review" })],
  },
  {
    kind: "reviewed",
    layers: bothLayers,
    name: "issue-artifact",
    path: (seed) => `/issues/${seed.issue}/artifacts/${seed.issueArtifactSlug}`,
    pattern: "/issues/:key/*",
    // The document's connection dot, as the issue-spec row reads it and for the same reason:
    // `artifact-header` is drawn while the editor still says "Connecting to the document…",
    // and `ArtifactHeader.tsx` draws the dot inside that header.
    ready: (page) => [page.getByRole("status", { name: "connected" })],
  },
  {
    kind: "reviewed",
    layers: bothLayers,
    name: "project-issues",
    path: (seed) => `/projects/${seed.project}/issues`,
    pattern: "/projects/:key/issues",
    ready: (page) => [page.locator("#project-issues-panel li[data-issue-row]").first()],
  },
  {
    kind: "reviewed",
    layers: bothLayers,
    name: "project-documents",
    path: (seed) => `/projects/${seed.project}/documents`,
    pattern: "/projects/:key/documents",
    ready: (page) => [page.locator("#project-documents-panel").getByRole("listitem").first()],
  },
  {
    kind: "reviewed",
    layers: bothLayers,
    name: "project-document",
    path: (seed) => `/projects/${seed.project}/documents/${seed.projectDocumentSlug}`,
    pattern: "/projects/:key/documents/:slug",
    // The document's connection dot, as the issue-artifact row reads it: this page draws the
    // same `ArtifactHeader` over the same editor. It did not drift in the probe's two runs,
    // which says its socket was up both times, not that it must be - the race is the one the
    // other two rows lost, so the row is held to the same proof rather than to its luck.
    ready: (page) => [page.getByRole("status", { name: "connected" })],
  },
  {
    kind: "reviewed",
    layers: bothLayers,
    name: "settings",
    path: () => "/settings",
    pattern: "/settings",
    ready: (page) => [page.getByRole("heading", { level: 1, name: "Settings" })],
  },
  {
    kind: "reviewed",
    layers: bothLayers,
    name: "delivery",
    path: (seed) => `/delivery?${seed.deliveryQuery}`,
    pattern: "/delivery",
    // The chart's own canvas, which ECharts creates only once the timeline query has answered
    // (`web/src/features/delivery/Timeline.tsx:550`, as `e2e/two-builds.multiprocess.e2e.ts`
    // locates it). The page's heading is drawn while that query is still pending, under
    // "Loading delivery timeline…" (`DeliveryPage.tsx:446`), and that is the whole page the
    // probe found missing in one run: the measure cards, the facets and the chart all wait on
    // the same answer.
    ready: (page) => [page.getByTestId("delivery-chart").locator("canvas").first()],
  },
  {
    kind: "reviewed",
    layers: bothLayers,
    name: "pipeline",
    path: (seed) => `/pipeline?at=${seed.pipelineAt}`,
    pattern: "/pipeline",
    ready: (page) => [page.getByRole("heading", { level: 1, name: "Pipeline" })],
  },
  {
    kind: "reviewed",
    layers: bothLayers,
    name: "not-found",
    path: () => "/no-such-page",
    pattern: "*",
    ready: (page) => [page.getByRole("heading", { name: "Page not found" })],
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
  // The three pages below are scanned and not shot. Each was excluded whole until the first run
  // of the two layers, and each of its reasons argues only against the shot: a scan reads the
  // DOM, which none of registry freshness, a tmux layout or a blinking probe changes. So they
  // carry `layers: ["scan"]`, which is what that field is for, and the reason each is not shot
  // stays beside it.
  {
    kind: "reviewed",
    // Not shot: its rows are built from the live registry's freshness, which moves between runs
    // (e2e/workspace.ts's note on which sessions age).
    layers: ["scan"],
    name: "machines",
    path: () => "/machines",
    pattern: "/machines",
    ready: (page) => [page.getByTestId("machines-page")],
  },
  {
    kind: "reviewed",
    // Not shot: the page draws a pixel layout computed from a tmux layout string, and the seed
    // carries none, so what it draws is the line that says so (e2e/machines.e2e.ts).
    layers: ["scan"],
    name: "machine",
    path: (seed) => `/machines/${encodeURIComponent(seed.machine)}`,
    pattern: "/machines/:machine",
    ready: (page) => [page.getByRole("heading", { level: 1 })],
  },
  {
    kind: "excluded",
    name: "credentials",
    pattern: "/credentials/*",
    reason: "It draws InboxPage, the same component as / (web/src/app.tsx).",
  },
  {
    kind: "reviewed",
    // Not shot: its one non-trivial state is a capability probe that is briefly out, which is
    // what a comparison would flake on (e2e/machine-login.e2e.ts).
    layers: ["scan"],
    name: "credentials-machine",
    path: () => "/credentials/machine",
    pattern: "/credentials/machine",
    ready: (page) => [page.getByLabel("Code shown on the machine")],
  },
];

/** A row of `reviewRoutes` that is reviewed rather than excluded, which is what every list
 *  below holds and what the two layers' rows are built from. */
export type ReviewedRoute = Extract<ReviewRoute, { kind: "reviewed" }>;

const reviewed: readonly ReviewedRoute[] = reviewRoutes.filter(
  (route): route is ReviewedRoute => route.kind === "reviewed"
);

export const scannedRoutes: readonly ReviewedRoute[] = reviewed.filter((route) =>
  route.layers.includes("scan")
);

export const shotRoutes: readonly ReviewedRoute[] = reviewed.filter((route) =>
  route.layers.includes("shoot")
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

/** Waits until `route`'s page has settled, which is more than drawn.
 *
 *  A page that drew is a page whose first frame reached the screen. A page that settled is one
 *  that has stopped changing on its own. Between the two, a document's socket is still
 *  connecting, a query is still pending behind a heading that is already up, a reference chip
 *  still reads as the short form of what it points at, and a Markdown body still holds its own
 *  source while the parse worker formats it - and every one of those redraws the page with no
 *  reader touching it. Three pages were read in that gap by two runs of the same corpus
 *  ninety seconds apart, and the two runs disagreed about all of their text; each row above now
 *  names everything its page draws only once its data is in, which is the half of this wait
 *  that is a route's own.
 *
 *  The other half is page-wide and belongs to no row: no element may be left carrying
 *  `data-markdown-pending`, the mark a body wears while the parse worker owes its formatting
 *  (`web/src/features/refs/MarkdownBody.tsx`, `MarkdownPreview.tsx`). A body that long holds
 *  its own literal text until the answer lands, so a page read in between is a page of
 *  unformatted source, and it is cheap to rule out for every row at once rather than to argue
 *  per row about which bodies cross the threshold.
 *
 *  Both layers wait on this, and for the same reason. The visual one would otherwise commit a
 *  baseline of a state that lasted a few hundred milliseconds and compare every later run
 *  against it. The accessibility one would otherwise scan that same state: a scan of a
 *  half-resolved page reports findings about a page no reader sees - the contrast of a
 *  skeleton, the name of a chip that is about to be replaced - and misses the findings of the
 *  page a reader does read. */
export async function waitForRouteSettled(page: Page, route: ReviewedRoute): Promise<void> {
  for (const locator of route.ready(page)) {
    await expect(locator).toBeVisible();
  }
  await expect(page.locator("[data-markdown-pending]")).toHaveCount(0);
}
