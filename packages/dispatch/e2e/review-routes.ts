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
}

/** The seed the containerised visual review reads, which is `e2e/visual-seed.ts`'s output. A
 *  type of its own because a shot row that silently skipped its clock pin would bring back the
 *  minute-boundary drift the pin exists to remove, and that shows up only as baselines
 *  differing late in a run. */
export interface VisualSeed extends ReviewSeed {
  /** When `e2e/visual-seed.ts` finished writing this corpus, ISO 8601. The visual rows pin
   *  their page clock a little after it, so every relative label the pages draw reads the same
   *  on every run. */
  readonly seededAt: string;
}

/** Which layer reads an entry: the accessibility scan, the desktop shot, and the phone shot -
 *  the same page again in the `visual-phone` project, for the pages a phone width rearranges
 *  (`phoneShotRoutes` below). */
export type ReviewLayer = "scan" | "shoot" | "shoot-phone";

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

/** The scan and the desktop shot, which is every reviewed page unless its row says otherwise.
 *  A row that is shot on a phone too spreads this and adds `"shoot-phone"`. */
const bothLayers: readonly ReviewLayer[] = ["scan", "shoot"];

/** The width from which the shell draws its wide layout - the Inbox's reading pane beside the
 *  list, the document margin as a column rather than a bottom sheet. Two rows below branch on
 *  it, so the number lives here and a change to the breakpoint is one edit. */
const WIDE_LAYOUT_MIN_PX = 1280;

/** Whether `page`'s viewport is wide enough for that layout, which decides what a row has to
 *  wait for: the phone projects draw neither of the two elements above. */
function wideLayout(page: Page): boolean {
  return (page.viewportSize()?.width ?? 0) >= WIDE_LAYOUT_MIN_PX;
}

/** The spec block `e2e/review-seed.ts` points the issue's one decision at, by its own words.
 *  Several rows below wait on it, in the two places the issue page draws it - the margin's
 *  decision card and the decision row's chip in the tree - so the corpus's own sentence is
 *  written once here and a change to that corpus fails those rows rather than a baseline. */
const SHAPED_BLOCK_WORDS = "One command, a seeded corpus, a signed-in browser.";

/** The words the issue tree's rows take from the one batched reference read, which answers
 *  after the rows themselves are drawn: until it does, a chip names its target by the target's
 *  address alone and a block of a document carries no words at all ("The names come last and
 *  all at once", `web/src/features/issue/useIssueTree.ts`). The seeded decision's chip is where
 *  this corpus shows it - the spec block that decision shaped, named by that block's own words
 *  - so a row that waits on this is reading a tree whose words are in. Every address that opens
 *  the Tree preset draws that row, which is why the four rows of those addresses share it.
 *
 *  It is the same answer the `issue-spec` row gets from the margin's decision card: one
 *  reference, drawn twice over by two surfaces, each of which holds the address until the graph
 *  answers with the words.
 *
 *  The shot rows of those addresses need it for a second reason. A chip that is still waiting
 *  draws the target's raw address, and a block's address carries a uuid the corpus makes
 *  afresh every seed; a chip that has been answered draws the target's short form instead
 *  (`NodeRow.tsx`'s `Chip`). A uuid is a fixed count of characters and not a fixed width, so a
 *  shot taken before that answer would hold a rectangle that moves with the glyphs - the very
 *  difference the `issue-decisions` row was taken out of the shot set for. Waiting here is
 *  what keeps it out of a baseline. */
function treeNamesSettled(page: Page): Locator {
  return page
    .locator('[data-node-row][data-node-type="decision"] [data-node-chip]')
    .getByRole("link", { name: SHAPED_BLOCK_WORDS });
}

export const reviewRoutes: readonly ReviewRoute[] = [
  {
    kind: "reviewed",
    layers: [...bothLayers, "shoot-phone"],
    name: "inbox",
    path: () => "/",
    pattern: "/",
    // The page's own heading, and - where the reader's column is drawn at all - the control
    // the column's head offers only once its document is drawn and placed at the block the row
    // is about ("Skip to the answer", `web/src/features/inbox/PaneDocument.tsx`). The column
    // waits `DOCUMENT_SETTLE_MS` before it reads its document, so a page read on the heading
    // alone caught the head without that control in one run of the drift probe and with it in
    // the next. The column shows from the wide layout's width and nowhere narrower
    // (`READING_PANE_QUERY`, `web/src/features/shell/useDialog.ts`), so the phone row, whose
    // Inbox is the list of cards alone, has nothing there to wait for.
    ready: (page) => {
      const heading = page.getByRole("heading", { exact: true, level: 1, name: "Inbox" });
      const wideEnoughForPane = wideLayout(page);
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
    layers: [...bothLayers, "shoot-phone"],
    name: "issue-spec",
    path: (seed) => `/issues/${seed.issue}/spec`,
    pattern: "/issues/:key/*",
    // Two facts, each of which one run of the drift probe caught this page without. This
    // address is the Spec preset over the issue's one tree now, with the spec document drawn
    // beside it (`web/src/features/issue/IssuePresetPanels.tsx`), so each fact is read where
    // the redesign draws it rather than off the tab panel that used to hold it.
    //
    // The document's connection dot once its socket is up: it reads "connected" only in that
    // state, so it matches nothing while the page still draws "Connecting to the document…".
    // Its accessible name is its `aria-label` and no longer the `title` it used to lean on
    // (`web/src/features/doc/ConnectionDot.tsx`), because the issue page draws no tooltip at
    // all (dispatch://INFRA-1196 R2, R21) and the dot still has to say which state it is in:
    // the locator is unchanged, the attribute under it is not. The dot is drawn by the preset
    // row's toolbar (`PresetToolbar.tsx` over `SpecToolbar.tsx`), which that row draws only
    // once the open document has reported one, so it proves the editor is on the screen as
    // well as connected. Spelled here rather than imported from `./editor`, which locates the
    // same dot: `./editor` reaches no `./psql`, so the container rule would allow it, but it
    // pulls `@hocuspocus/provider`, `lib0` and `web/src/api/live-socket` - and so React and
    // @tanstack/react-query - into a module of data and locators that a bun unit test imports.
    //
    // And the margin's decision card with the block its decision shaped named by that block's
    // own words. The card arrives with the decisions view, after the document; its preview
    // then holds the reference as the reader wrote it (`dispatch://CORE-1/spec#b-<uuid>`)
    // until the reference graph answers, and the words replace it
    // (`web/src/features/refs/MarkdownPreview.tsx`). Both states were shot. The card is the
    // margin's own, drawn at the head of the page's one right-hand panel (`IssueMargin.tsx`,
    // which hands the margin's cards through), and it is placed against the spec block it
    // shaped - so of the issue's addresses this is the one that draws it.
    //
    // That panel is a column beside the page at the wide layout's width and the margin's own
    // sheet below it, shut until a button opens it (`packages/dispatch/AGENTS.md`, "Phone
    // acceptance"), so the card is on the screen at a desktop width and nowhere narrower. The
    // phone row of this page waits on the dot alone, as the Inbox row waits on its heading
    // alone.
    //
    // The tree this preset draws needs no fact of its own here: the Spec preset keeps the
    // issue's requirements, and the rows it leaves on the screen for this corpus - the issue
    // and the document the requirements live in - carry words the issue read already holds.
    ready: (page) => {
      const connected = page.getByRole("status", { name: "connected" });
      const wideEnoughForMargin = wideLayout(page);
      return wideEnoughForMargin
        ? [connected, page.locator("[data-margin-item]").getByText(SHAPED_BLOCK_WORDS)]
        : [connected];
    },
  },
  {
    kind: "reviewed",
    // Not shot: what this page draws last is a turn that reaches it over the live socket
    // rather than with the first read, so the page is sometimes 1280x905 where the baseline
    // is 1280x925, carrying the `Jump to latest` pill a settled view does not - measured in
    // one container run of five of one tree (42479821620), and a size mismatch is a
    // difference no pixel bound can absorb. It is the same race the phone copy of this page
    // was taken out of the shot set for, and the redesign only moved the list into the page's
    // one right-hand panel (dispatch://INFRA-1196 R12): the turns still arrive over that
    // socket. The binding the shots of it carried - the docked composer's `SURFACE`, in both
    // schemes - is read off the live element by the kept row of `e2e/theme.e2e.ts` instead,
    // which is a stronger claim than a snapshot made. A scan reads the DOM and does not care
    // which turns have landed, so this page keeps that layer.
    layers: ["scan"],
    name: "issue-conversation",
    path: (seed) => `/issues/${seed.issue}/conversation`,
    pattern: "/issues/:key/*",
    // The conversation's one list, which is drawn in the page's one right-hand panel and in
    // no column of the page any more (`web/src/features/issue/IssueMargin.tsx`), and the tree
    // this address draws beside it: `/conversation` names no preset, so it opens the Tree
    // preset, whose rows take their words from the batched reference read.
    ready: (page) => [
      page.getByRole("list", { name: "Conversation turns" }),
      treeNamesSettled(page),
    ],
  },
  {
    kind: "reviewed",
    layers: bothLayers,
    name: "issue-children",
    path: (seed) => `/issues/${seed.issue}/children`,
    pattern: "/issues/:key/*",
    // The child's own row, not the section: `#issue-children-panel` went with the eight tab
    // panels, and the children are a section of the page the tree sits under
    // (`web/src/features/issue/IssueChildren.tsx`), drawn at every address and open from
    // Tailwind's `md` for a reader who has never shut it (`useChildrenSection.ts`) - so the
    // section's own header is on the screen whether or not its rows are. The row is
    // `[data-child-row]`, which is how `e2e/issue-page.ts` reads one.
    //
    // And the tree's words, because this address opens the Tree preset beside the section.
    ready: (page) => [
      page.locator('[data-testid="issue-children"] [data-child-row]').first(),
      treeNamesSettled(page),
    ],
  },
  {
    kind: "reviewed",
    layers: bothLayers,
    name: "issue-artifacts",
    path: (seed) => `/issues/${seed.issue}/artifacts`,
    pattern: "/issues/:key/*",
    // What this address opens on the redesigned page is the node menu beside the presets,
    // which lists every artifact no block of the tree cites (`NodeMenu.tsx`; the issue's own
    // check records that `/artifacts` opens it). The menu's item for the issue's spec is the
    // primary document's row here - an issue's spec is `spec.md` under the slug `spec`
    // (`issue_create.go`) - and the menu is built out of the tree, so the item says the
    // issue's artifacts have reached it.
    //
    // The list that tab drew is still the panel under the tree (`ArtifactsTab.tsx` through
    // `ArtifactRoutePanel.tsx`) and `artifact-spec` is still that document's row in it, so
    // the fact this row has always held is kept rather than dropped. The tree's words are the
    // third, as on every address that opens the Tree preset.
    ready: (page) => [
      page.getByRole("menu").getByRole("menuitem").filter({ hasText: "spec.md" }),
      page.getByTestId("artifact-spec"),
      treeNamesSettled(page),
    ],
  },
  {
    kind: "reviewed",
    layers: bothLayers,
    name: "issue-activity",
    path: (seed) => `/issues/${seed.issue}/activity`,
    pattern: "/issues/:key/*",
    // The issue's whole event history, which this address still draws in the page's own
    // column (`features/activity/ActivityTab.tsx`, mounted by `IssuePresetPanels.tsx`) while
    // the panel beside it holds the conversation in its `everything` state - the state this
    // address names (`conversation-states.tsx`). And the tree's words, as on every address
    // that opens the Tree preset.
    ready: (page) => [page.getByRole("list", { name: "Issue activity" }), treeNamesSettled(page)],
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
    // list that holds it is drawn before that answer, which is why the probe read the chip in
    // one run and the title in the other. The words are the plan task `e2e/review-seed.ts`
    // writes, so a change to that corpus fails this row rather than a baseline.
    //
    // The redesign kept that list and moved it: this address opens the Plan preset over the
    // tree, and the plan document is drawn beside it inside the preset's own panel, framed by
    // the same `PlanTab` (`IssuePresetPanels.tsx` through `RoleDocumentPanel.tsx`). So the
    // fact and its race are untouched, and the locator reads them where they are now drawn.
    // The tree the preset leaves on the screen for this corpus - the issue and the document
    // the plan's tasks serve - carries words the issue read already holds, so it needs no
    // fact of its own.
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
    // `reference-decorations.ts`, `web/src/features/doc/document-references.tsx`). The panel
    // holding the document is drawn with that document still empty, so the probe read the bare
    // key in one run and the title in the other; this waits for the document and that answer
    // together.
    //
    // `verification-tab` went with the eight tab panels. This address opens the Checks preset
    // and keeps the Verification document beside the tree, in the preset's own panel
    // `#issue-verification-panel` (`RoleDocumentPanel.tsx`, whose Checks half gives the
    // document no frame because the checks themselves are rows of the tree), so the panel's
    // own id is what scopes the reference now.
    ready: (page) => [page.locator("#issue-verification-panel a.dispatch-reference")],
  },
  {
    kind: "reviewed",
    // Not shot, and no longer for the reason it was. A chip names its target by the target's
    // address as well as by its words, and this decision's chip points at a block of the
    // spec, so this row used to draw `dispatch://CORE-1/spec#b-<uuid>` as text of its own -
    // an id fresh in every seeded corpus, which no wait resolved because it was what the
    // reference IS, and which no mask held either, since a uuid is a fixed count of
    // characters and not a fixed width, so the painted rectangle moved with the glyphs.
    // Three container runs of the old page's Decisions panel, which drew the same id for the
    // same reason, measured it at 153, 781 and 794 pixels.
    //
    // `NodeRow.tsx`'s `Chip` now draws the target's short form ("CORE-1 spec block") once the
    // batched reference read has answered, and the raw address only while it has not - and
    // that answer is what the `ready` below waits for. So the fresh uuid is out of this page,
    // and out of every page that draws a chip, as long as the row waits for the words.
    //
    // What keeps the row off the shot layer now is that it has never had a baseline: this
    // route has no file under `e2e/__screenshots__`, a route this layer gains has none until
    // a run inside the pinned image writes one (`packages/dispatch/AGENTS.md`), and no pod of
    // this package can run that image. Giving it `shoot` here would add a row that fails
    // every run until CI wrote its two images. That is an architect's call, with the
    // measurement a container run would give it; until then a scan reads the DOM and is what
    // reviews this page.
    layers: ["scan"],
    name: "issue-decisions",
    path: (seed) => `/issues/${seed.issue}/decisions`,
    pattern: "/issues/:key/*",
    // The decision this preset opens on, in the state that put it there, and the link to the
    // spec block it shaped once the reference graph has answered.
    //
    // The panel's "Waiting for your review" region is gone with the Decisions tab: how a
    // decision stands is the node's own state now, drawn as that row's state word, and
    // `page-words.ts` records that the three states are the sections that tab grouped by. So
    // the row carries `data-node-state="waiting_review"`, and the preset opens that row's body
    // without being asked (`IssueBody.tsx`'s `opensOn`), which is the same "what waits for
    // your review" the region named.
    //
    // Until the batched reference read answers, the chip names the block by its address alone
    // - so a page read on the row alone was captured sometimes with the words and sometimes
    // without them, as the old page's was. Waiting for the words settles it, and is also what
    // keeps the raw address out of what is read, because the short form replaces it with the
    // same answer. The words are the spec block `e2e/review-seed.ts` names, so a change to
    // that corpus fails this row rather than a baseline.
    ready: (page) => [
      page.locator('[data-node-row][data-node-type="decision"][data-node-state="waiting_review"]'),
      treeNamesSettled(page),
    ],
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
    // Not shot: each row carries an unread dot when the issue's `last_seq` is above the
    // reader's own `last_read_seq` (`UnreadDot.tsx`), and opening an issue's pages marks them
    // read - which the rows before this one do, to the very issue this list names. Whether
    // that write has landed by the time this page's read-state query answers is a race, and
    // the dot is 36 counted pixels: present in three container runs of one tree (42476315586,
    // 42476318547, 42478599706) and absent in two more of the same tree (42478603064,
    // 42478605733), in both schemes, against the same baseline. It is the third page of this
    // set whose difference is a per-reader marker racing the capture, and like the other two
    // it keeps its scan, which reads the DOM and does not care.
    layers: ["scan"],
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

/** The routes a phone viewport draws differently, the rows `e2e/visual.e2e.ts` tags `@phone`
 *  and the `visual-phone` project runs. Derived from the layer each entry carries, as
 *  `shotRoutes` is, so a page joins this set by saying so in its own row.
 *
 *  A phone width is one under 768 px (`packages/dispatch/AGENTS.md`, "Phone acceptance"), where
 *  the shell hides the sidebar behind the navigation drawer and shows the compact top bar. That
 *  alone changes every page, so the entries that carry `shoot-phone` are those whose own content
 *  the width rearranges rather than only its frame: the Inbox becomes the compact row list with
 *  no reading pane beside it, and the Spec's margin becomes a bottom sheet rather than a column
 *  beside the document.
 *
 *  An issue's Conversation belongs in that company and is deliberately absent. At a phone width
 *  it draws a floating "Jump to latest" pill counting what has arrived since the reader last
 *  looked, and whether the seeded reply has arrived by the moment of capture is a race with the
 *  live socket: one container run shot "Jump to latest" and the next "Jump to latest - 1 new",
 *  which moved the card above it and differed by 15,776 pixels. Waiting for the pill would be
 *  waiting for that race to go one way. The desktop row of the same page is not shot either: a
 *  turn arriving over the live socket changes its height there (its entry above says so). */
export const phoneShotRoutes: readonly ReviewedRoute[] = reviewed.filter((route) =>
  route.layers.includes("shoot-phone")
);

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
 *  The other half is page-wide and belongs to no row. Two marks say the page still owes
 *  something, and neither may be left on it.
 *
 *  `data-markdown-pending` is the mark a body wears while the parse worker owes its formatting
 *  (`web/src/features/refs/MarkdownBody.tsx`, `MarkdownPreview.tsx`). A body that long holds
 *  its own literal text until the answer lands, so a page read in between is a page of
 *  unformatted source, and it is cheap to rule out for every row at once rather than to argue
 *  per row about which bodies cross the threshold.
 *
 *  `data-landing-ring` is the mark an artifact header wears for the two seconds after the
 *  address landed on it (`web/src/features/artifacts/ArtifactHeader.tsx`). That ring is the
 *  whole of why `issue-artifact-dark` was the one image of the twenty that two regenerations
 *  of this lane disagreed about: `e2e/visual.e2e.ts` shoots light and then dark on one load,
 *  the ring's two seconds expire somewhere between the two, and the later shot is the one the
 *  expiry can fall inside - so light matched itself and dark did not. It is a box shadow, so
 *  it moves nothing and the two images were the same size, which is why reading the DOM found
 *  it where reading the text did not: measured on this page, the ring is 2,604 pixels of the
 *  header's own outline (x=342-873, y=1013-1128 of a 1280x2239 shot), against the 2,428 the
 *  container measured. The page is right to draw it - a reader following a link to a document
 *  is meant to see which one it landed on - so the wait is here and the cue is untouched.
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
  await expect(page.locator("[data-landing-ring]")).toHaveCount(0);
}
