/** The sweep of the foot of the screen (dispatch://INFRA-4731): Pending changes and the update
 *  banner float at the foot (`web/src/hooks/useFootInset.ts`), and wherever they show, every
 *  reading here is that the foot is clear, as the check `c-foot-clear` says it and the probe reads
 *  it, clause by clause (`e2e/foot-probe.ts`):
 *
 *  - no float's box meets another foot element's box;
 *  - no float covers the centre of a control, read where that centre lies inside every box that
 *    clips the control;
 *  - at each scroller's real end, no leaf element of its own content, as far as it lies inside
 *    each box that clips it, the scroller's own box among them, meets a float's box on both axes;
 *    the content of a float, a scroller inside it included, is not read against the floats;
 *  - the shell's Navigation stays on the screen at the end of a page;
 *  - a box that scrolls only sideways keeps its height under a float;
 *  - when a float goes, only a scroller whose reader stands in the end room it would lose keeps
 *    that room, no more than it held, and the document's scroller also while a box it holds
 *    would be clamped without it, each until a scroll leaves it;
 *  - with no float shown and every scroller at its start, no scroller carries end room;
 *  - a float that shows for a moment moves no content when it goes.
 *
 *  Each is read as a settled state: the same answer on two readings at least two animation
 *  frames apart.
 *
 *  Four kinds of row read it. The rows by route group open every reviewed route of
 *  `e2e/review-routes.ts` against the review corpus (`e2e/review-seed.ts`) at three desktop
 *  sizes with both floats shown, then close both and read the end room they leave: only room
 *  the reader stands in, and none once every scroller is back at its start. The rows of the
 *  fixed surface open eight pages, one row each, three for the issues table (the selection bar
 *  hidden; shown with the update banner; shown with the banner dismissed) and two for the live
 *  view (its four smaller sizes; its three desktop sizes), seeded so each page's scrollers reach
 *  the foot, and read every combination of the floats at seven sizes from a phone's up. Two
 *  rows hold three steps of their own: a float over a sticky column, a box
 *  that scrolls only sideways under a float, and a float that shows for a moment at the end of a
 *  scroll. And one row reads a page it writes itself: Pending changes, whose own list of refused
 *  changes scrolls in its box.
 *
 *  Each reading prints one `FOOT-CELL <page> <WxH> <floats> <clean|problems…>` line, so a run's
 *  output is the table of every cell; the two controls of the row that writes its own page are
 *  readings that must find a problem, and print none. Every title carries `@foot`, which the
 *  check c-foot-clear reads from the full lane; none carries `@pr`, so the pull-request lane runs
 *  none of them. None carries `@phone` either: each row sets every size it reads itself, the
 *  phone sizes included, so the `iphone` project would read the same sizes again. */
import {
  type FakeSession,
  listedSession,
  seedUnreadReply,
  setLiveSessions,
  setSessionLive,
} from "./agents";
import {
  createAsk,
  createIssue,
  createMessage,
  listIssues,
  seedAgentStreamConversations,
} from "./api";
import { expectTimeout } from "./expect-timeout";
import {
  discardPending,
  dismissBanner,
  openWithFloats,
  refuseChanges,
  reloadWithBanner,
  routeFloats,
  seedFillers,
  seedRefusedMove,
  updateBanner,
} from "./foot-floats";
import {
  expectFootClear,
  expectNoEndRoom,
  FOOT_NAMES,
  footCellLine,
  navigationProblems,
  readFootElements,
  readFootEnds,
  readSidewaysBox,
  type SidewaysReading,
  sampleFootRest,
} from "./foot-probe";
import { inboxPane, selectedRow } from "./inbox-pane";
import { APPROVAL_FOOT, inboxAt, seedApprovalPlace } from "./inbox-pane-scroll";
import { withIssuePage } from "./issue-page-option";
import { pendingWrites } from "./keyed-writes";
import { expectConversationInFront } from "./live-view-cost";
import { type ReviewedRoute, reviewRoutes, waitForRouteSettled } from "./review-routes";
import { seedReviewRoutes } from "./review-seed";
import { type Browser, expect, type Page, test } from "./test";
import { asUser } from "./users";

/** A size of the window. */
type Size = { readonly width: number; readonly height: number };

/** The three desktop sizes the rows by route group read at. */
const ROUTE_SIZES: readonly Size[] = [
  { width: 1280, height: 800 },
  { width: 1600, height: 1000 },
  { width: 1920, height: 1080 },
];

/** The seven sizes the rows of the fixed surface read at, from a phone's up: a phone, the `sm`
 *  edge, a small laptop, a short window, and the three desktops. */
const SURFACE_SIZES: readonly Size[] = [
  { width: 360, height: 640 },
  { width: 640, height: 480 },
  { width: 800, height: 600 },
  { width: 1024, height: 500 },
  { width: 1280, height: 720 },
  { width: 1600, height: 1000 },
  { width: 1920, height: 1080 },
];

/** The sizes the sticky step reads at: from 1280 px, where Pending changes sits at the window's
 *  lower right, over the issue page's Review margin. */
const STICKY_SIZES: readonly Size[] = [
  { width: 1280, height: 720 },
  { width: 1600, height: 1000 },
  { width: 1920, height: 1080 },
];

/** The size the sideways step reads at: the update banner, centred at the foot, lies over the
 *  issue page's document column there. */
const SIDEWAYS_SIZE: Size = { width: 1600, height: 1000 };

/** The sizes the moment step reads at: a small laptop's and a desktop's, both from `xl` up,
 *  where the chord indicator stands on its lower floor. */
const MOMENT_SIZES: readonly Size[] = [
  { width: 1280, height: 720 },
  { width: 1600, height: 1000 },
];

/** What a cell shows at the foot: Pending changes, the update banner and, on the issues table,
 *  the selection bar. */
type Shown = "pending" | "banner" | "bar";

/** A cell's name in its `FOOT-CELL` line and its failure: `<page> <WxH> <floats>`, the floats
 *  `none` where it shows nothing. */
function cellName(page: string, size: Size, shown: readonly Shown[]): string {
  return `${page} ${size.width}x${size.height} ${shown.join("+") || "none"}`;
}

/**
 * Fails unless, at the end of every scroll, each element of `main` stuck to the top, and the new
 * issue page's icon rail, keeps its place: its top no higher than its own `top`. End room in
 * `main` lengthens the page below every column `main` holds, so a column whose box ends before
 * that room - the frame's Contents pane, a stuck header - would be carried up off its place at
 * the end of the scroll, as end room below the shell once carried the shell's Navigation. An
 * element scrolled wholly off the top has no place left to keep and is not read. Read until it
 * holds; prints its own `FOOT-CELL` line, under the page `<name>:stuck`, either way.
 */
async function expectStuckInPlace(page: Page, cell: string): Promise<void> {
  let problems: readonly string[] = ["the page was never read"];
  try {
    await expect
      .poll(
        async () => {
          await page.evaluate(readFootEnds, FOOT_NAMES);
          problems = await page.evaluate(() => {
            const stuck = [
              ...(document.getElementById("main-content")?.querySelectorAll("*") ?? []),
            ].filter((element) => {
              const { position, top } = getComputedStyle(element);
              return position === "sticky" && top !== "auto";
            });
            const rail = document.querySelector('nav[data-testid="icon-rail"]');
            return [...stuck, ...(rail === null ? [] : [rail])].flatMap((element) => {
              const box = element.getBoundingClientRect();
              if (box.height === 0 || box.bottom <= 0) return [];
              const place = Number.parseFloat(getComputedStyle(element).top) || 0;
              if (box.top >= place - 1) return [];
              const testid = element.getAttribute("data-testid");
              const label = element.getAttribute("aria-label");
              const called = testid ?? label ?? element.tagName.toLowerCase();
              return [
                `${called}'s top is at y ${Math.round(box.top)}, above its place at ${place}`,
              ];
            });
          });
          return problems;
        },
        { message: `${cell}: every stuck element keeps its place at the end of the scroll` }
      )
      .toEqual([]);
  } finally {
    console.log(footCellLine(cell, problems));
  }
}

/** Reads `page`, named `name`, at each of `sizes` with `shown` at the foot: the foot clear where
 *  a float shows, and no end room where none does. Where `stuck`, each reading with a float shown
 *  is followed by `expectStuckInPlace`, as a cell of `<name>:stuck`. */
async function readEverySize(
  page: Page,
  name: string,
  shown: readonly Shown[],
  { stuck = false, sizes = SURFACE_SIZES }: { stuck?: boolean; sizes?: readonly Size[] } = {}
): Promise<void> {
  if (shown.includes("pending")) await expect(pendingWrites(page)).toBeVisible();
  if (shown.includes("banner")) await expect(updateBanner(page)).toBeVisible();
  if (shown.includes("bar")) await expect(page.getByRole("toolbar")).toBeVisible();
  const floatShown = shown.includes("pending") || shown.includes("banner");
  for (const size of sizes) {
    await page.setViewportSize(size);
    const cell = cellName(name, size, shown);
    if (floatShown) await expectFootClear(page, cell);
    else await expectNoEndRoom(page, cell);
    if (floatShown && stuck) await expectStuckInPlace(page, cellName(`${name}:stuck`, size, shown));
  }
}

// ---------------------------------------------------------------------------------------------
// The rows by route group.
// ---------------------------------------------------------------------------------------------

/** The most routes one row reads, so each row stays well inside the per-row budget
 *  (`testTimeout`, e2e/playwright.config.ts) after the corpus it seeds: a route costs a refused
 *  change on the issues table, two loads and three settled readings, measured at 8 s to 21 s on
 *  an issue pod, against 3.5 s for the corpus. */
const ROUTES_PER_ROW = 2;

/** The open issue of the review corpus whose change the rows by route group refuse
 *  (`e2e/workspace.ts` writes it): it stands under no epic, so taking it out of one changes
 *  nothing a reviewed page draws. */
const REFUSED_TITLE = "Snooze an Inbox row until tomorrow";

/** The route group of `route`: the first segment of the router path it stands for, so the
 *  pages of one part of the dashboard share a row; the catch-all route `*` is a group of its own. */
function routeGroup(route: ReviewedRoute): string {
  return route.pattern.split("/")[1] ?? route.pattern;
}

/**
 * Every reviewed route of `e2e/review-routes.ts`, in rows of at most `ROUTES_PER_ROW`, in the
 * list's order: a group's routes share a row, consecutive groups share one while they fit, and a
 * group larger than a row is split into rows of its own, whose last part may share the next
 * group's row. No route is left out.
 */
function routeRows(): ReviewedRoute[][] {
  const groups: ReviewedRoute[][] = [];
  for (const route of reviewRoutes) {
    if (route.kind !== "reviewed") continue;
    const group = groups.find((held) => routeGroup(held[0]) === routeGroup(route));
    if (group === undefined) groups.push([route]);
    else group.push(route);
  }
  const rows: ReviewedRoute[][] = [];
  for (const group of groups) {
    for (let start = 0; start < group.length; start += ROUTES_PER_ROW) {
      const part = group.slice(start, start + ROUTES_PER_ROW);
      const last = rows.at(-1);
      if (last !== undefined && last.length + part.length <= ROUTES_PER_ROW) last.push(...part);
      else rows.push(part);
    }
  }
  return rows;
}

for (const routes of routeRows()) {
  const names = routes.map((route) => route.name).join(", ");
  // SLOW(>=2s): the review corpus, then for each route a refused change made on the issues table,
  // a load with both floats and three settled readings of the foot, each at the end of every
  // scroller.
  test(`both floats leave the foot clear on ${names} at three desktop sizes, and no end room once closed @foot`, async ({
    browser,
  }) => {
    // The seed's waits give up at the bound this row's own assertions use, inside the row's clock.
    const seed = await seedReviewRoutes({ boundMs: expectTimeout });
    const refused = (await listIssues("CORE")).find((issue) => issue.title === REFUSED_TITLE);
    if (refused === undefined) throw new Error(`The review corpus holds no "${REFUSED_TITLE}"`);
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await routeFloats(page, refused.key);
      for (const route of routes) {
        await page.setViewportSize(ROUTE_SIZES[0]);
        await openWithFloats(page, refused.key, route.path(seed));
        for (const size of ROUTE_SIZES) {
          await page.setViewportSize(size);
          await waitForRouteSettled(page, route);
          await expectFootClear(page, cellName(route.name, size, ["pending", "banner"]));
        }
        await dismissBanner(page);
        await discardPending(page);
        await expectNoEndRoom(page, cellName(route.name, ROUTE_SIZES[2], []));
      }
    } finally {
      await context.close();
    }
  });
}

// ---------------------------------------------------------------------------------------------
// The rows of the fixed surface.
// ---------------------------------------------------------------------------------------------

/** One page of the fixed surface: its name in a cell, the address it opens at, and what it has
 *  drawn once it is ready to read, after each load. `first` runs once, after the first load,
 *  before the page is read. Where `stuck`, each reading with a float shown is followed by
 *  `expectStuckInPlace`. `sizes` are the sizes it is read at, every size of the fixed surface
 *  where it names none. */
interface Surface {
  readonly name: string;
  readonly path: string;
  readonly ready: (page: Page) => Promise<void>;
  readonly first?: (page: Page) => Promise<void>;
  readonly stuck?: true;
  readonly sizes?: readonly Size[];
}

/**
 * Reads `surface` at its sizes with each combination of the floats: both, Pending changes alone,
 * the banner alone and neither. The banner, once dismissed, stays so for the page's life, so the
 * page is loaded afresh for the banner alone: Pending changes keeps the refused change across the
 * load, and is then discarded.
 */
async function sweepSurface(page: Page, refusedKey: string, surface: Surface): Promise<void> {
  await routeFloats(page, refusedKey);
  await openWithFloats(page, refusedKey, surface.path);
  await surface.first?.(page);
  await surface.ready(page);
  const stuck = surface.stuck === true;
  const sizes = surface.sizes ?? SURFACE_SIZES;
  await readEverySize(page, surface.name, ["pending", "banner"], { stuck, sizes });
  await dismissBanner(page);
  await readEverySize(page, surface.name, ["pending"], { stuck, sizes });
  await reloadWithBanner(page, refusedKey, surface.path);
  await surface.ready(page);
  await discardPending(page);
  await readEverySize(page, surface.name, ["banner"], { stuck, sizes });
  await dismissBanner(page);
  await readEverySize(page, surface.name, [], { sizes });
}

/** The session that asks the margin's questions and writes the Conversation's messages. */
const agent = {
  actor: { id: "e2e-foot-sweep", kind: "session" as const },
  as: "agent" as const,
};

/** Twelve words of the spec, each the anchor of one question in the margin. */
const MARGIN_WORDS = [
  "alpha",
  "bravo",
  "charlie",
  "delta",
  "echo",
  "foxtrot",
  "golf",
  "hotel",
  "india",
  "juliet",
  "kilo",
  "lima",
];

/** An issue whose spec holds a paragraph for each of `MARGIN_WORDS` and a question anchored to
 *  each word, so its margin holds a card for each, as the foot row of `e2e/issues-table.e2e.ts`
 *  seeds the margin's column. Answers its key. */
async function seedMarginIssue(): Promise<string> {
  const issue = await createIssue({
    project: "CORE",
    spec: `${MARGIN_WORDS.map(
      (word) => `The ${word} part holds one line of the review, long enough to wrap on a phone.`
    ).join("\n\n")}\n`,
    title: "Margin review",
  });
  // One at a time, so the margin lists the cards in the order the words stand.
  for (const word of MARGIN_WORDS) {
    await createAsk(
      issue.key,
      {
        anchor: { artifact: "spec", quote: word },
        options: [{ label: "Keep" }, { label: "Drop" }],
        question: `Keep ${word}?`,
      },
      agent
    );
  }
  return issue.key;
}

/** What the rows of the issues table press and read: the table's checkbox for an issue, its
 *  selection bar, and the walk of the table's box and the document back to their start, where
 *  the rows a press picks are drawn. */
function tableControls(page: Page) {
  return {
    select: (key: string) => page.getByRole("checkbox", { name: `Select ${key}`, exact: true }),
    toolbar: page.getByRole("toolbar"),
    toStart: () =>
      page.evaluate(() => {
        window.scrollTo(0, 0);
        const box = document.querySelector<HTMLElement>('[data-testid="issues-table-scroll"]');
        if (box !== null) box.scrollTop = 0;
      }),
  };
}

/**
 * Seeds the issues table for its rows - the refused move, one issue more and the fillers that run
 * the table's box past the window - and opens it on a page of `browser` with the update banner
 * shown, as the table's own foot row shows it: the page asks for the builds at its load. Runs
 * `read` on that page with the refused issue and the other one, and closes the page after.
 */
async function withTable(
  browser: Browser,
  read: (page: Page, refused: string, other: string) => Promise<void>
): Promise<void> {
  const refused = await seedRefusedMove();
  const other = await createIssue({ project: "CORE", title: "Ship the parser" });
  await seedFillers();
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await routeFloats(page, refused.key);
    await page.goto("/projects/CORE/issues");
    await page.evaluate(() => window.dispatchEvent(new Event("focus")));
    await read(page, refused.key, other.key);
  } finally {
    await context.close();
  }
}

// The issues table with no selection, so the selection bar is hidden, and every combination of
// the floats in one page: the banner first, then the refused change made from the bar and the
// selection cleared, and each float closed once no combination left shows it.
// SLOW(>=2s): four combinations of the floats at seven sizes, each a settled reading at the end of
// every scroller.
test("the floats leave the foot of the issues table clear with the selection bar hidden, in every combination at seven sizes @foot", async ({
  browser,
}) => {
  await withTable(browser, async (page, refused) => {
    const { select, toolbar, toStart } = tableControls(page);
    await readEverySize(page, "issues-table", ["banner"]);
    await toStart();
    await select(refused).check();
    await toolbar.getByRole("button", { name: "Take out of epic" }).click({ timeout: 5_000 });
    await expect(pendingWrites(page)).toContainText(refused);
    await toolbar.getByRole("button", { name: "Clear selection" }).click({ timeout: 5_000 });
    await expect(toolbar).toHaveCount(0);
    await readEverySize(page, "issues-table", ["pending", "banner"]);
    await dismissBanner(page);
    await readEverySize(page, "issues-table", ["pending"]);
    await discardPending(page);
    await readEverySize(page, "issues-table", []);
  });
});

// The issues table with a selection, so the selection bar shows, with the update banner: alone and
// beside Pending changes. The refused change is made from the bar and the selection then moves to
// the other issue, which keeps the bar shown. With a selection a reading takes 1.5 s to 2.9 s on
// an issue pod, so the bar's four combinations are two rows, this one with the banner.
// SLOW(>=2s): two combinations of the floats and the bar at seven sizes, each a settled reading at
// the end of every scroller.
test("the floats leave the foot of the issues table clear with the selection bar and the update banner shown, at seven sizes @foot", async ({
  browser,
}) => {
  await withTable(browser, async (page, refused, other) => {
    const { select, toolbar, toStart } = tableControls(page);
    await select(refused).check();
    await readEverySize(page, "issues-table", ["banner", "bar"]);
    await toStart();
    await toolbar.getByRole("button", { name: "Take out of epic" }).click({ timeout: 5_000 });
    await expect(pendingWrites(page)).toContainText(refused);
    await select(refused).setChecked(false);
    await select(other).check();
    await readEverySize(page, "issues-table", ["pending", "banner", "bar"]);
  });
});

// The issues table with the selection bar shown and the update banner dismissed: Pending changes
// beside the bar, and the bar alone once the refused change is discarded.
// SLOW(>=2s): two combinations of the floats and the bar at seven sizes, each a settled reading at
// the end of every scroller.
test("the floats leave the foot of the issues table clear with the selection bar shown and the update banner dismissed, at seven sizes @foot", async ({
  browser,
}) => {
  await withTable(browser, async (page, refused, other) => {
    const { select, toolbar } = tableControls(page);
    await dismissBanner(page);
    await select(refused).check();
    await toolbar.getByRole("button", { name: "Take out of epic" }).click({ timeout: 5_000 });
    await expect(pendingWrites(page)).toContainText(refused);
    await select(refused).setChecked(false);
    await select(other).check();
    await readEverySize(page, "issues-table", ["pending", "bar"]);
    await discardPending(page);
    await readEverySize(page, "issues-table", ["bar"]);
  });
});

// The Board: the project's issues as columns, its Triage column run past the window by the
// fillers. The view is a preference the Board button stores, so the refused change is made on
// the table first and the Board is then turned on, which the load after keeps.
// SLOW(>=2s): four combinations of the floats at seven sizes, each a settled reading at the end
// of every scroller.
test("the floats leave the foot of the Board clear in every combination at seven sizes @foot", async ({
  browser,
}) => {
  const refused = await seedRefusedMove();
  await seedFillers();
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    const board = page.getByRole("region", { name: "Project board" });
    await sweepSurface(page, refused.key, {
      name: "board",
      path: "/projects/CORE/issues",
      ready: async () => {
        await expect(
          board.getByRole("article").filter({ hasText: "Filler issue number 39" })
        ).toHaveCount(1);
      },
      first: async () => {
        await page.getByRole("button", { name: "Board" }).click();
        await expect(page.getByRole("button", { name: "Board" })).toHaveAttribute(
          "aria-pressed",
          "true"
        );
      },
    });
  } finally {
    await context.close();
  }
});

// The Agents page: fourteen live sessions, which run its list past a short window, one ended
// with an unread reply under them, and three of them selected for broadcast, which brings the
// broadcast composer to the foot, as the Agents rows of `e2e/issues-table.e2e.ts` seed it.
// SLOW(>=2s): four combinations of the floats at seven sizes, each a settled reading at the end
// of every scroller.
test("the floats leave the foot of the Agents page clear in every combination at seven sizes @foot", async ({
  browser,
}) => {
  const titles = Array.from(
    { length: 14 },
    (_, index) => `Agent ${String(index + 1).padStart(2, "0")}`
  );
  const endedSession = "01a04731-0c07-7000-8000-000000000400";
  // The Agents setup and the issues setup touch nothing in common, so each chain goes out beside
  // the other, and each keeps its own order.
  const [, refused] = await Promise.all([
    (async () => {
      await setLiveSessions([
        ...titles.map((title, index) =>
          listedSession(
            `01a04731-0c07-7000-8000-0000000004${String(index + 1).padStart(2, "0")}`,
            title
          )
        ),
        listedSession(endedSession, "Ended"),
      ]);
      await seedUnreadReply(endedSession);
      await setSessionLive(endedSession, false);
    })(),
    seedRefusedMove(),
  ]);
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    const broadcast = page.getByRole("region", { name: "Broadcast" });
    await sweepSurface(page, refused.key, {
      name: "agents",
      path: "/agents",
      ready: async () => {
        await expect(
          page.getByRole("region", { name: "Replied, no longer connected" })
        ).toBeVisible();
        for (const title of titles.slice(0, 3)) {
          await page.getByRole("checkbox", { name: `Select ${title} for broadcast` }).check();
        }
        await expect(
          broadcast.getByRole("list", { name: "Selected agents" }).getByRole("listitem")
        ).toHaveCount(3);
      },
    });
  } finally {
    await context.close();
  }
});

// The issue page as it has been: its Spec tab, with a margin full of questions, a column from
// `xl` and a sheet at the foot below it.
// SLOW(>=2s): twelve questions seeded one at a time, and four combinations of the floats at seven
// sizes, each a settled reading at the end of every scroller.
test("the floats leave the foot of the issue page clear in every combination at seven sizes @foot", async ({
  browser,
}) => {
  const refused = await seedRefusedMove();
  const reviewed = await seedMarginIssue();
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await sweepSurface(page, refused.key, {
      name: "issue-page",
      path: `/issues/${reviewed}/spec`,
      ready: async () => {
        await expect(page.locator("[data-margin-item]")).toHaveCount(MARGIN_WORDS.length);
      },
    });
  } finally {
    await context.close();
  }
});

// The new issue page, in its frame, which `withIssuePage` turns on for alice as every row of
// `e2e/issue-page-*.e2e.ts` does: the same issue, its words and its questions, read in the frame.
// The document scrolls the frame's column (`IssueOutlinePage.tsx`), which takes end room for the
// floats, so each reading with a float shown is followed by one of the frame's icon rail, its
// Contents pane and every element stuck in `main` at the end of the scroll
// (`expectStuckInPlace`): the room must carry none of them off its place.
// SLOW(>=2s): twelve questions seeded one at a time, and four combinations of the floats at seven
// sizes, each a settled reading at the end of every scroller, and the stuck elements' places.
test("the floats leave the foot of the new issue page's frame clear in every combination at seven sizes @foot", async ({
  browser,
}) => {
  const refused = await seedRefusedMove();
  const reviewed = await seedMarginIssue();
  const context = await asUser(browser, "alice");
  try {
    await withIssuePage(context, "alice");
    const page = await context.newPage();
    await sweepSurface(page, refused.key, {
      name: "issue-frame",
      path: `/issues/${reviewed}/conversation`,
      ready: async () => {
        await expect(page.locator("[data-issue-page]")).toHaveCount(1);
        await expect(page.locator("[data-issue-page]")).toContainText(MARGIN_WORDS.at(-1) ?? "");
      },
      stuck: true,
    });
  } finally {
    await context.close();
  }
});

// The Inbox, its reading pane holding an approval's document far longer than the pane, as the
// Inbox row of `e2e/issues-table.e2e.ts` seeds it.
// SLOW(>=2s): four combinations of the floats at seven sizes, each a settled reading at the end
// of every scroller.
test("the floats leave the foot of the Inbox clear in every combination at seven sizes @foot", async ({
  browser,
}) => {
  const [refused, approval] = await Promise.all([seedRefusedMove(), seedApprovalPlace()]);
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await sweepSurface(page, refused.key, {
      name: "inbox",
      path: inboxAt(approval.approvalAskId),
      ready: async () => {
        await expect(selectedRow(page)).toHaveAttribute("data-inbox-row", approval.approvalAskId);
        await expect(inboxPane(page)).toContainText(APPROVAL_FOOT);
      },
    });
  } finally {
    await context.close();
  }
});

/**
 * Reads an agent's live view, its transcript at the stream's limit, as the live view row of
 * `e2e/issues-table.e2e.ts` seeds it, at `sizes` (`sweepSurface`).
 */
async function sweepLiveView(browser: Browser, sizes: readonly Size[]): Promise<void> {
  /** A live session whose conversation is at the stream's limit. Its id is a UUID, which
   *  Dispatch's agent routes require. */
  const live: FakeSession = {
    capabilities: ["aside", "btw", "steer"],
    dir: "/workspaces/foot-sweep",
    last_seen: Date.now() - 5_000,
    machine_id: "foot-sweep-host",
    roles: [],
    session_id: "01a04731-0c07-7000-8000-000000000500",
    title: "Sweep",
  };
  const [refused] = await Promise.all([
    seedRefusedMove(),
    (async () => {
      await setLiveSessions([live]);
      await seedAgentStreamConversations([live.session_id]);
    })(),
  ]);
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await sweepSurface(page, refused.key, {
      name: "live-view",
      path: `/agents/${live.session_id}/live`,
      ready: () => expectConversationInFront(page, live.session_id),
      sizes,
    });
  } finally {
    await context.close();
  }
}

// An agent's live view, read in two rows by size, each in its own row budget: a transcript at the
// stream's limit is seeded for each, and each resize lays it out again, which took the one row of
// all seven sizes to 51 s to 60 s on an issue pod. This row reads the four smaller sizes.
// SLOW(>=2s): a transcript at the stream's limit, and four combinations of the floats at four
// sizes, each a settled reading at the end of every scroller.
test("the floats leave the foot of an agent's live view clear in every combination at the four smaller sizes, 360x640 to 1024x500 @foot", async ({
  browser,
}) => {
  await sweepLiveView(browser, SURFACE_SIZES.slice(0, 4));
});

// The live view's second row: the three desktop sizes.
// SLOW(>=2s): a transcript at the stream's limit, and four combinations of the floats at three
// sizes, each a settled reading at the end of every scroller.
test("the floats leave the foot of an agent's live view clear in every combination at the three desktop sizes, 1280x720 to 1920x1080 @foot", async ({
  browser,
}) => {
  await sweepLiveView(browser, SURFACE_SIZES.slice(4));
});

/** How many messages the Conversation row seeds: forty run the tab thousands of pixels past a
 *  desktop window (`e2e/conversation.e2e.ts` measured 2,700 px at 1280x720). */
const CONVERSATION_MESSAGES = 40;

/** An issue whose Conversation holds `CONVERSATION_MESSAGES` messages, the newest last written.
 *  Answers its key. */
async function seedLongConversation(): Promise<string> {
  const issue = await createIssue({ project: "CORE", title: "A long conversation" });
  // One at a time: the server appends one event at a time per issue, so a burst costs the same,
  // and the tab lists the messages in the order they were written.
  for (let index = 1; index <= CONVERSATION_MESSAGES; index += 1) {
    await createMessage(issue.key, { body: `Conversation message ${index}` }, agent);
  }
  return issue.key;
}

// An issue's Conversation tab, run past the window by forty messages, with its composer: docked
// at the foot below `sm`, and stuck at the top from `sm` up. Its `Jump to latest` is a float of
// the foot's top layer, which shows once the reader leaves the top, where the newest turn is
// listed, and the document takes end room for it then. So with neither Pending changes nor the
// banner shown, the page is read back at the start of its scroll (`expectNoEndRoom`), where
// `Jump to latest` hides: at 360x640, read at the end, `main` held 317px of room for the button.
// SLOW(>=2s): forty messages written one at a time, and four combinations of the floats at seven
// sizes, each a settled reading at the end of every scroller.
test("the floats leave the foot of an issue's Conversation clear in every combination at seven sizes @foot", async ({
  browser,
}) => {
  const refused = await seedRefusedMove();
  const conversation = await seedLongConversation();
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await sweepSurface(page, refused.key, {
      name: "conversation",
      path: `/issues/${conversation}/conversation`,
      ready: async () => {
        await expect(page.getByRole("list", { name: "Conversation turns" })).toContainText(
          `Conversation message ${CONVERSATION_MESSAGES}`
        );
      },
    });
  } finally {
    await context.close();
  }
});

// ---------------------------------------------------------------------------------------------
// The sticky, sideways and moment steps.
// ---------------------------------------------------------------------------------------------

/** The lines of a code block wider than any document column here, which the sideways step puts
 *  under the update banner. */
const CODE_LINES = [
  "const sideways = 'a line of code far wider than the column that holds the document, so the block scrolls sideways';",
  "const under = 'the update banner lies over this block while the step reads its height';",
];

/** The paragraph right after the code block, which must not move. */
const AFTER_CODE = "The paragraph after the code block stays where it stands.";

/** A spec with the code block in its middle: enough paragraphs above it that the window can
 *  scroll it down to the banner, and below it that the window can scroll that far. */
const CODE_SPEC = [
  ...Array.from(
    { length: 24 },
    (_, index) => `Lead paragraph ${index + 1} holds a sentence of the spec before the code.`
  ),
  ["```ts", ...CODE_LINES, "```"].join("\n"),
  AFTER_CODE,
  ...Array.from(
    { length: 24 },
    (_, index) => `Trailing paragraph ${index + 1} holds a sentence of the spec after the code.`
  ),
].join("\n\n");

/**
 * Fails unless, on the issue page `page` shows with Pending changes over its Review margin, the
 * margin's content fits it, Pending changes lies over it, the margin is the scroller that takes
 * end room for the part of Pending changes over it, `body` carries none, and the shell's
 * Navigation as a reader sees it keeps its top on the screen at the end of the page; read until
 * it holds, each read at the end of every scroll (`readFootEnds`), as a settled state: the
 * probe's answer gives the Navigation's problems with those of a scroll short of its end and of
 * two readings that differ (`navigationProblems`), and the step's own read of the margin and
 * Pending changes is a problem where two readings two animation frames apart differ. Pending
 * changes is wider than the margin, so its left edge lies over `main`, which takes room for that
 * part by the same rule: the room `main` holds is read and printed after the cell's `FOOT-CELL`
 * line, which is printed either way, and is no problem of the step's.
 */
async function expectStickyClear(page: Page, cell: string): Promise<void> {
  let problems: readonly string[] = ["the page was never read"];
  let main = "";
  try {
    await expect
      .poll(
        async () => {
          const ends = await page.evaluate(readFootEnds, FOOT_NAMES);
          const read = await page.evaluate(async ({ clearance, mark }) => {
            const once = () => {
              const column = document.querySelector<HTMLElement>(
                'aside[aria-label="Review margin"]'
              );
              const pending = document.querySelector('[aria-label="Pending changes"]');
              const shell = document.getElementById("main-content");
              const mainRoom = shell?.hasAttribute(mark)
                ? `main holds ${shell.style.paddingBottom} of end room`
                : "main holds no end room";
              if (column === null) {
                return { main: mainRoom, boxes: [], problems: ["no Review margin shows"] };
              }
              if (pending === null) {
                return { main: mainRoom, boxes: [], problems: ["no Pending changes shows"] };
              }
              const a = column.getBoundingClientRect();
              const b = pending.getBoundingClientRect();
              const over =
                Math.min(a.right, b.right) - Math.max(a.left, b.left) > 1 &&
                Math.min(a.bottom, b.bottom) - Math.max(a.top, b.top) > 1;
              // The margin's own room lengthens its scroll: its content is measured without it.
              const room = Number.parseFloat(column.style.getPropertyValue(clearance)) || 0;
              return {
                main: mainRoom,
                boxes: [
                  `the Review margin at y ${Math.round(a.top)}-${Math.round(a.bottom)} ` +
                    `x ${Math.round(a.left)}-${Math.round(a.right)}`,
                  `Pending changes at y ${Math.round(b.top)}-${Math.round(b.bottom)} ` +
                    `x ${Math.round(b.left)}-${Math.round(b.right)}`,
                ],
                problems: [
                  ...(over ? [] : ["Pending changes does not lie over the Review margin"]),
                  ...(column.scrollHeight - room > column.clientHeight + 1
                    ? ["the Review margin's content does not fit it"]
                    : []),
                  ...(column.hasAttribute(mark) ? [] : [`the Review margin carries no ${mark}`]),
                  ...(document.body.hasAttribute(mark)
                    ? [`body carries ${mark} (${document.body.style.paddingBottom})`]
                    : []),
                ],
              };
            };
            // Settled: "the same answer on two readings at least two animation frames apart",
            // as the probe's reads take it (`e2e/foot-probe.ts`). The answer is each box and the
            // problems; where the two readings differ, the answer is the second one, with a
            // problem that says what changed.
            const first = once();
            const frames = Promise.withResolvers<void>();
            requestAnimationFrame(() => requestAnimationFrame(() => frames.resolve()));
            await frames.promise;
            const second = once();
            const before = [...first.boxes, ...first.problems];
            const after = [...second.boxes, ...second.problems];
            const went = before.filter((piece) => !after.includes(piece));
            const came = after.filter((piece) => !before.includes(piece));
            if (went.length === 0 && came.length === 0) return second;
            return {
              ...second,
              problems: [
                ...second.problems,
                `the Review margin and Pending changes were not settled: of two readings two ` +
                  `animation frames apart, the first read ` +
                  `${went.slice(0, 3).join("; ") || "nothing more"} and the second ` +
                  `${came.slice(0, 3).join("; ") || "nothing more"}`,
              ],
            };
          }, FOOT_NAMES);
          main = read.main;
          problems = [...read.problems, ...navigationProblems(ends)];
          return problems;
        },
        { message: cell }
      )
      .toEqual([]);
  } finally {
    console.log(`${footCellLine(cell, problems)} (${main})`);
  }
}

/** Reads the code block of `CODE_SPEC` on `page` through the probe (`readSidewaysBox`). Where
 *  `toBanner`, scrolls the document first so the block's middle stands at the banner's. */
function readSideways(page: Page, toBanner: boolean): Promise<SidewaysReading> {
  return page.evaluate(readSidewaysBox, { after: AFTER_CODE, code: CODE_LINES[0], toBanner });
}

// Two steps, each read as a settled state.
// A float over a sticky element: from 1280 px Pending changes sits at the window's lower right,
// over the issue page's Review margin, a column stuck at the screen's height whose content fits.
// The margin is the scroller that takes end room for it, so the walk never reaches `body`, which
// carries none, and the shell's Navigation keeps its top on the screen at the end of the page.
// Pending changes is wider than the margin, and `main` takes room for the part over it; that room
// is read and printed, and is no problem here.
// A box that scrolls only sideways: a code block of the issue's document computes `overflow-y:
// auto` only because its `overflow-x` is. Under the update banner, it keeps the height it has with
// the banner closed, and the paragraph after it stays where it stands.
// SLOW(>=2s): an issue page read at three sizes at the end of every scroll, and a code block read
// at rest under the banner and after it closed.
test("a float over a sticky column leaves body without end room and the Navigation on the screen, and a box that scrolls only sideways keeps its height under a float @foot", async ({
  browser,
}) => {
  const refused = await seedRefusedMove();
  const plain = await createIssue({ project: "CORE", title: "Ship the parser" });
  const coded = await createIssue({ project: "CORE", spec: CODE_SPEC, title: "Sideways code" });
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await routeFloats(page, refused.key);

    // The float over a sticky column: Pending changes alone, over the Review margin.
    await page.setViewportSize(STICKY_SIZES[0]);
    await openWithFloats(page, refused.key, `/issues/${plain.key}`);
    await expect(page.getByRole("heading", { name: "Ship the parser" })).toBeVisible();
    await dismissBanner(page);
    for (const size of STICKY_SIZES) {
      await page.setViewportSize(size);
      await expectStickyClear(page, `sticky-step ${size.width}x${size.height} pending`);
    }

    // The box that scrolls only sideways: the update banner alone, over the code block.
    await reloadWithBanner(page, refused.key, `/issues/${coded.key}/spec`);
    await discardPending(page);
    await page.setViewportSize(SIDEWAYS_SIZE);
    const at = `${SIDEWAYS_SIZE.width}x${SIDEWAYS_SIZE.height}`;
    let shown: SidewaysReading | undefined;
    let rest = 0;
    try {
      await expect
        .poll(
          async () => {
            await readSideways(page, true);
            // At rest: the foot's layers, the document's height, the scroll and every mark hold
            // one state for half a second, and the block is read after that.
            rest = (await page.evaluate(sampleFootRest, { ms: 500, names: FOOT_NAMES })).length;
            shown = await readSideways(page, false);
            return { ...shown, rest };
          },
          { message: `the code block under the banner, at rest, at ${at}` }
        )
        .toMatchObject({ overflow: "auto/auto", rest: 1, settled: true, under: true });
    } finally {
      console.log(
        footCellLine(
          `sideways-step ${at} banner`,
          shown?.under === true && shown.overflow === "auto/auto" && shown.settled && rest === 1
            ? []
            : [`the code block was read as ${JSON.stringify({ ...shown, rest })}`]
        )
      );
    }
    await dismissBanner(page);
    let closed: SidewaysReading | undefined;
    try {
      await expect
        .poll(
          async () => {
            closed = await readSideways(page, false);
            return { after: closed.after, height: closed.height, settled: closed.settled };
          },
          { message: `the code block's height and the paragraph after it, banner closed, at ${at}` }
        )
        .toEqual({ after: shown?.after, height: shown?.height, settled: true });
    } finally {
      console.log(
        footCellLine(
          `sideways-step ${at} none`,
          closed?.height === shown?.height && closed?.after === shown?.after && closed?.settled
            ? []
            : [
                `the code block is ${shown?.height}px under the banner and ${closed?.height}px ` +
                  `without it, and the paragraph after it at y ${shown?.after} and ${closed?.after}`,
              ]
        )
      );
    }
  } finally {
    await context.close();
  }
});

/** How long the moment step goes on reading after a float has gone. */
const AFTER_IT_GOES_MS = 500;

/** The end of a scroll the moment step stands at: the selector of the box that scrolls its
 *  content, or null where the document does, and the selector whose last match is that
 *  content's last. */
interface MomentEnd {
  readonly box: string | null;
  readonly last: string;
}

/** The issues table's box, whose last content is its last row; the document scrolls a little
 *  past the box at 1280x720, and the step stands at the end of both. */
const TABLE_END: MomentEnd = {
  box: '[data-testid="issues-table-scroll"]',
  last: '[data-testid="issues-table-scroll"] [data-issue-row]',
};

/** The Conversation, which the document scrolls, whose last content is its oldest turn: the tab
 *  lists the newest first. */
const CONVERSATION_END: MomentEnd = { box: null, last: '[data-turn^="message:"]' };

/** The chord indicator (`ChordIndicator`), a float of the foot's top layer while a chord is half
 *  typed. */
const CHORD_INDICATOR = '[data-testid="chord-indicator"]';

/** Pending changes, a float of the foot's status layer, by its accessible name as
 *  `readFootEnds` reads it. */
const PENDING_CHANGES = '[aria-label="Pending changes"]';

/** The two ways a half-typed chord goes without completing, each the name of its cell and what
 *  the row does for it: a key that continues no chord and is bound to nothing ends it (`F2`), or
 *  it is left to time out (`CHORD_TIMEOUT_MS` in `web/src/features/shell/keymap.ts`). */
const CHORD_ENDS = [
  { name: "chord-ended", end: (page: Page) => page.keyboard.press("F2") },
  { name: "chord-timed-out", end: () => Promise.resolve() },
] as const;

/** One animation frame of the end of a scroll, as `watchTheEnd` reads it: when, on the page's
 *  clock, whether the float shows, the top of the scroll's last content, and every element that
 *  carries the hook's mark (`FOOT_NAMES.mark`) with the room it holds. */
interface EndFrame {
  readonly at: number;
  readonly shown: boolean;
  readonly top: number;
  readonly marks: string;
}

declare global {
  interface Window {
    /** The frames `watchTheEnd` has read, a reading of its own at the moment the row asks for
     *  one, which answers whether the float showed in it, and how its reading stops. */
    __footMoment?: {
      readonly frames: EndFrame[];
      readonly read: () => boolean;
      readonly stop: () => void;
    };
  }
}

/** Scrolls the document, and the box `end.box` selects where it names one, to the real end of
 *  its scroll (`scrollHeight - scrollTop - clientHeight <= 1`), read until each stands there two
 *  frames after its scroll, so the end room a float's pass adds has landed first. */
async function standAtTheEnd(page: Page, end: MomentEnd, cell: string): Promise<void> {
  await expect
    .poll(
      () =>
        page.evaluate(async (box) => {
          const scrollers = [
            document.documentElement,
            ...(box === null ? [] : [document.querySelector(box)]),
          ];
          for (const scroller of scrollers) {
            if (scroller !== null) scroller.scrollTop = scroller.scrollHeight;
          }
          const frames = Promise.withResolvers<void>();
          requestAnimationFrame(() => requestAnimationFrame(() => frames.resolve()));
          await frames.promise;
          return scrollers.flatMap((scroller) => {
            if (scroller === null) return [`${box} is not in the page`];
            const gap = scroller.scrollHeight - scroller.scrollTop - scroller.clientHeight;
            return gap > 1 ? [`${scroller.tagName} stands ${gap}px short of its end`] : [];
          });
        }, end.box),
      { message: `${cell}: every scroll at its real end` }
    )
    .toEqual([]);
}

/** Reads the end of a scroll in every animation frame from now until `framesOfTheMoment` stops
 *  it (`EndFrame`): the last element `end.last` matches, and whether the float `float` selects
 *  has a box. The first frame is read at once, before anything the row presses next, and
 *  `window.__footMoment.read()` takes one more at a moment the row names, for the reason
 *  `expectStillWhenItGoes` gives. */
function watchTheEnd(page: Page, end: MomentEnd, float: string): Promise<void> {
  return page.evaluate(
    ({ float, last, names }) => {
      const frames: EndFrame[] = [];
      let reading = true;
      const read = (): boolean => {
        const content = [...document.querySelectorAll(last)].at(-1);
        const frame: EndFrame = {
          at: performance.now(),
          marks: [...document.querySelectorAll<HTMLElement>(`[${names.mark}]`)]
            .map(
              (element) =>
                `${element.tagName}${element.id ? `#${element.id}` : ""}:${
                  element.style.getPropertyValue(names.clearance) || element.style.paddingBottom
                }`
            )
            .join(","),
          shown: (document.querySelector(float)?.getBoundingClientRect().height ?? 0) > 0,
          top: content?.getBoundingClientRect().top ?? Number.NaN,
        };
        frames.push(frame);
        return frame.shown;
      };
      const frame = () => {
        if (!reading) return;
        read();
        requestAnimationFrame(frame);
      };
      read();
      requestAnimationFrame(frame);
      window.__footMoment = {
        frames,
        read,
        stop: () => {
          reading = false;
        },
      };
    },
    { float, last: end.last, names: FOOT_NAMES }
  );
}

/** The index of the first frame in which the float no longer shows after a frame in which it
 *  did, or -1 where no frame read yet is one. */
function goneAt(frames: readonly EndFrame[]): number {
  const shown = frames.findIndex((frame) => frame.shown);
  return shown < 0 ? -1 : frames.findIndex((frame, index) => index > shown && !frame.shown);
}

/** The frames `watchTheEnd` has read, once they run `AFTER_IT_GOES_MS` past the frame the float
 *  went in, on the page's own clock: read until they do, and the reading is then stopped. Each
 *  read takes a reading of its own first, so the frames run past the going on a page whose
 *  animation frames have stopped coming as well as on one whose have not. */
async function framesOfTheMoment(page: Page, cell: string): Promise<readonly EndFrame[]> {
  let frames: readonly EndFrame[] = [];
  await expect
    .poll(
      async () => {
        frames = await page.evaluate(() => {
          window.__footMoment?.read();
          return window.__footMoment?.frames ?? [];
        });
        const gone = frames[goneAt(frames)];
        return gone === undefined ? 0 : (frames.at(-1)?.at ?? 0) - gone.at;
      },
      { message: `${cell}: the frames run ${AFTER_IT_GOES_MS} ms past the float's going` }
    )
    .toBeGreaterThanOrEqual(AFTER_IT_GOES_MS);
  await page.evaluate(() => window.__footMoment?.stop());
  return frames;
}

/**
 * What `frames`, read from before a float showed until `AFTER_IT_GOES_MS` after it went, say of
 * the content at the end of the scroll: the top of its last content before the press, while the
 * float showed, one frame after it went and `AFTER_IT_GOES_MS` after that, for the cell's line;
 * and each move of that top by 1px or more, at the frame it moved in, with the end room then
 * held, which is a problem; so is a first frame that read no last content, since no move of it
 * can then be seen.
 */
function momentReading(frames: readonly EndFrame[]): { readonly line: string; problems: string[] } {
  const round = (value: number | undefined) =>
    value === undefined ? "unread" : `${Math.round(value * 100) / 100}`;
  const first = frames[0];
  const gone = goneAt(frames);
  const wentAt = frames[gone]?.at ?? Number.POSITIVE_INFINITY;
  const shown = frames.filter((frame) => frame.shown).map((frame) => frame.top);
  const later = frames.find((frame) => frame.at - wentAt >= AFTER_IT_GOES_MS);
  const line =
    `before ${round(first?.top)}, while shown ${round(Math.min(...shown))} to ` +
    `${round(Math.max(...shown))}, one frame after it went ${round(frames[gone + 1]?.top)}, ` +
    `${AFTER_IT_GOES_MS} ms later ${round(later?.top)}; ${frames.length} readings, ` +
    `${shown.length} of them with the float; end room before ${first?.marks || "nowhere"}, ` +
    `as it went ${frames[gone]?.marks || "nowhere"}`;
  const problems: string[] =
    first === undefined || Number.isNaN(first.top)
      ? ["the scroll's last content was not read in the first frame, before the press"]
      : [];
  let top = first?.top ?? Number.NaN;
  let held = first?.marks ?? "";
  for (const frame of frames) {
    const moved = !(
      Math.abs(frame.top - top) < 1 ||
      (Number.isNaN(frame.top) && Number.isNaN(top))
    );
    if (moved) {
      problems.push(
        `the last content's top went from ${round(top)} to ${round(frame.top)} ` +
          `${Math.round(frame.at - (first?.at ?? 0))} ms after the first frame, with the float ` +
          `${frame.shown ? "shown" : "gone"}, and end room ${held || "nowhere"} the frame ` +
          `before and ${frame.marks || "nowhere"} then`
      );
      top = frame.top;
    }
    held = frame.marks;
  }
  return { line, problems };
}

/**
 * Fails unless a float that shows for a moment moves no content the reader reads, from before it
 * shows until `AFTER_IT_GOES_MS` after it has gone, at the real end of the scroll `end` names:
 * the document and the box are scrolled there first (`standAtTheEnd`), then `show` brings the
 * float, or nothing where it already shows, and `hide` takes it away, and the top of the
 * scroll's last content is read in every animation frame between (`watchTheEnd`). Prints the
 * cell's `FOOT-CELL` line either way, with the four readings after it.
 *
 * Two of the readings are the row's own, taken where it knows what the page shows rather than
 * left to the animation frames: one while the float shows, before `hide`, and one the moment
 * `hide` has taken it away. A float this short is there for a frame or two - under four workers
 * on four cores, as the shard legs run, a chord ended by a key was read in 1 to 6 frames of the
 * 36 to 68 a reading holds (median 3), while frames ran 80 to 136 ms apart often enough that a
 * leg read none at all inside the moment. The reading then holds no frame the float showed in,
 * `goneAt` finds no going for the frames to run past, and the step fails 15 s later on a float
 * that showed and went as it should: shard (1) of merge group 45461576237, and 1 of 8 readings
 * of this row on a developer machine under that load.
 */
async function expectStillWhenItGoes(
  page: Page,
  cell: string,
  end: MomentEnd,
  float: string,
  show: () => Promise<unknown>,
  hide: () => Promise<unknown>
): Promise<void> {
  await standAtTheEnd(page, end, cell);
  await watchTheEnd(page, end, float);
  await show();
  await expect(page.locator(float)).toBeVisible();
  await expect
    .poll(() => page.evaluate(() => window.__footMoment?.read() ?? false), {
      message: `${cell}: a reading of the scroll's end while the float showed`,
    })
    .toBe(true);
  await hide();
  await expect(page.locator(float)).toHaveCount(0);
  await page.evaluate(() => window.__footMoment?.read());
  const { line, problems } = momentReading(await framesOfTheMoment(page, cell));
  console.log(`${footCellLine(cell, problems)} (${line})`);
  expect(problems, `${cell}: the content at the end of the scroll moved`).toEqual([]);
}

// A float that shows for a moment, at the real end of a scroll: the chord indicator, which shows
// while a chord is half typed (`g` and then a key, `app.tsx`), at the end of the issues table's
// box and of the Conversation, ended by a key that continues no chord and left to time out; and
// Pending changes, discarded at the end of the table's box. End room added below the end moves
// nothing, but room taken away while the reader stands in it shortens the scroll: its
// `scrollTop` clamps and the content jumps. So at 1280x720 and 1600x1000 the top of the
// scroll's last content is read in every frame from before the press until 500 ms after the
// float has gone, and in two readings of the row's own, while the float shows and the moment it
// has gone (`expectStillWhenItGoes` says why), and must not move. The builds are not routed, so the
// update banner never shows and each reading brings the one float it reads.
// SLOW(>=2s): forty issues and forty messages seeded, and at two sizes five readings, each from a
// scroll to the real end until 500 ms after its float has gone, two of them through the chord's
// one-second timeout.
test("a float that shows for a moment moves no content when it goes @foot", async ({ browser }) => {
  const refused = await seedRefusedMove();
  await seedFillers();
  const conversation = await seedLongConversation();
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await refuseChanges(page, refused.key);
    const { select, toolbar, toStart } = tableControls(page);
    const turns = page.getByRole("list", { name: "Conversation turns" });
    for (const size of MOMENT_SIZES) {
      await page.setViewportSize(size);
      const at = `${size.width}x${size.height}`;
      await page.goto("/projects/CORE/issues");
      // The table does not draw every filler at its start, and the fillers go out side by side
      // in no order; the refused issue, seeded before them, is drawn there, as `withTable`'s rows
      // press it.
      await expect(select(refused.key)).toBeVisible();
      for (const { end, name } of CHORD_ENDS) {
        await expectStillWhenItGoes(
          page,
          `issues-table ${at} ${name}`,
          TABLE_END,
          CHORD_INDICATOR,
          () => page.keyboard.press("g"),
          () => end(page)
        );
      }
      await toStart();
      await select(refused.key).check();
      await toolbar.getByRole("button", { name: "Take out of epic" }).click({ timeout: 5_000 });
      await expect(pendingWrites(page)).toContainText(refused.key);
      await toolbar.getByRole("button", { name: "Clear selection" }).click({ timeout: 5_000 });
      await expect(toolbar).toHaveCount(0);
      await expectStillWhenItGoes(
        page,
        `issues-table ${at} pending-discarded`,
        TABLE_END,
        PENDING_CHANGES,
        () => Promise.resolve(),
        () => discardPending(page)
      );
      await page.goto(`/issues/${conversation}/conversation`);
      await expect(turns).toContainText(`Conversation message ${CONVERSATION_MESSAGES}`);
      for (const { end, name } of CHORD_ENDS) {
        await expectStillWhenItGoes(
          page,
          `conversation ${at} ${name}`,
          CONVERSATION_END,
          CHORD_INDICATOR,
          () => page.keyboard.press("g"),
          () => end(page)
        );
      }
    }
  } finally {
    await context.close();
  }
});

// ---------------------------------------------------------------------------------------------
// A float's own scroller.
// ---------------------------------------------------------------------------------------------

/** The size the row of a float's own scroller reads at. */
const OWN_SCROLLER_SIZE: Size = { width: 1280, height: 720 };

/** How many refused changes Pending changes lists in the row of a float's own scroller: ten rows
 *  of 2rem run past its list's 12rem box, as more refused changes than the box shows do in the
 *  shell. */
const REFUSED_ROWS = 10;

/**
 * The page the row of a float's own scroller writes (`page.setContent`), so it needs no seed and
 * no API: `main#main-content`, at least as tall as the screen, so that a point out of Pending
 * changes hits the page, as it does in the shell; and Pending changes fixed at the foot of the
 * screen as the shell draws it at its fullest (`PendingWritesIndicator`), with a list of
 * `REFUSED_ROWS` refused changes, each with Retry and Discard, that scrolls in a 12rem box
 * (`max-h-48 overflow-y-auto`). Where `tall`, `main` runs past the screen, and its last
 * paragraph, 18rem tall, ends under Pending changes at the end of the document's scroll; else
 * `main` holds one short paragraph and fills the screen, with no scroll.
 * Above the list, Pending changes holds three controls more, each in a box of no height whose
 * overflow is hidden. That box clips neither `Close`, absolute, nor `Help`, fixed, since it is
 * not their containing block: Pending changes is Close's, and the screen is Help's. The box of
 * `Hidden`, fixed, has a transform, which makes it Hidden's containing block, so it clips Hidden
 * away, 3rem above Pending changes. The page is in standards mode, as Dispatch's pages are: in
 * quirks mode the body, not the root, scrolls the document, and the probe reads the root's scroll.
 */
function ownScrollerPage(tall: boolean): string {
  const rows = Array.from(
    { length: REFUSED_ROWS },
    (_, index) =>
      '<li style="display: flex; align-items: center; gap: 0.5rem; min-height: 2rem">' +
      `<span style="flex: 1">Not saved: change ${index + 1}</span>` +
      '<button type="button">Retry</button><button type="button">Discard</button></li>'
  ).join("");
  const main = tall
    ? '<p style="height: 150vh">The lead of a tall page.</p>' +
      '<p style="height: 18rem">The last paragraph of the page.</p>'
    : "<p>The one paragraph of the page.</p>";
  return `<!doctype html>
<body style="margin: 0; font: 16px sans-serif">
  <main id="main-content" style="display: flow-root; min-height: 100vh">${main}</main>
  <section aria-label="Pending changes" style="position: fixed; right: 1rem; bottom: 1rem; width: 20rem; padding: 0.5rem; border: 1px solid; background: white">
    <div style="height: 0; overflow: hidden">
      <button type="button" style="position: absolute; top: 0.25rem; right: 0.25rem">Close</button>
    </div>
    <div style="height: 0; overflow: hidden">
      <button type="button" style="position: fixed; top: 1rem; left: 50%">Help</button>
    </div>
    <div style="height: 0; overflow: hidden; transform: translateX(0)">
      <button type="button" style="position: fixed; top: -3rem; left: 0">Hidden</button>
    </div>
    <ul style="max-height: 12rem; overflow-y: auto; margin: 2rem 0 0; padding: 0; list-style: none">${rows}</ul>
  </section>
</body>`;
}

// A float's own scroller: Pending changes lists its refused changes in a 12rem box that scrolls
// (`PendingWritesIndicator`), so with more of them than the box shows, the list is a scroller
// inside a float. "The content of a float, a scroller inside it included, is not read against
// the floats": the probe neither scrolls the list nor reads its content at its end. And "no float
// covers the centre of a control, read where that centre lies inside every box that clips the
// control": a Retry or Discard whose centre the list scrolls out of its view is not read, and
// each control in its view is. The row writes the page itself (`ownScrollerPage`), at 1280x720,
// with the list scrolled to its middle, so that it clips rows above its view and below it, and
// the foot must read clean. Then two controls, each of which must find a problem, so a reading
// too broad fails. A fixed box laid over the centre of a Retry in the list's view, of `Close` and
// of `Help` covers each: those two lie in a box that clips but is not their containing block, so
// they are read, while `Hidden`, which its containing block clips away, is still not read. And a
// tall `main` whose last paragraph ends under Pending changes is a cover at the end of the
// document's scroll, the one cover there.
test("a list that scrolls inside Pending changes is not read against the floats, and a control goes unread only where a box that clips it hides its centre @foot", async ({
  page,
}) => {
  const at = `${OWN_SCROLLER_SIZE.width}x${OWN_SCROLLER_SIZE.height}`;
  await page.setViewportSize(OWN_SCROLLER_SIZE);
  await page.setContent(ownScrollerPage(false));
  await page.evaluate((pending) => {
    const list = document.querySelector(`${pending} ul`);
    if (list === null) throw new Error("The page holds no list of refused changes");
    list.scrollTop = (list.scrollHeight - list.clientHeight) / 2;
  }, PENDING_CHANGES);
  await expectFootClear(page, cellName("pending-list", OWN_SCROLLER_SIZE, ["pending"]));

  // The first control: a fixed box over the centre of Close, of Help and of a Retry in the
  // list's view.
  await page.evaluate((pending) => {
    const list = document.querySelector(`${pending} ul`);
    if (list === null) throw new Error("The page holds no list of refused changes");
    const view = list.getBoundingClientRect();
    const buttons = [...document.querySelectorAll(`${pending} button`)];
    const inView = buttons.find((button) => {
      const box = button.getBoundingClientRect();
      const y = box.top + box.height / 2;
      return button.textContent === "Retry" && y > view.top && y < view.bottom;
    });
    for (const button of [
      ...["Close", "Help"].map((text) => buttons.find((each) => each.textContent === text)),
      inView,
    ]) {
      if (button === undefined) throw new Error("Pending changes holds no control the row names");
      const box = button.getBoundingClientRect();
      const over = document.createElement("div");
      over.style.cssText =
        `position: fixed; z-index: 1; left: ${box.left}px; top: ${box.top}px; ` +
        `width: ${box.width}px; height: ${box.height}px; background: gray`;
      document.body.append(over);
    }
  }, PENDING_CHANGES);
  await expect
    .poll(async () => (await page.evaluate(readFootElements)).problems, {
      message: `pending-list ${at}: each control a fixed box lies over is covered, and no other`,
    })
    .toEqual([
      `Pending changes's control "Close" is covered by DIV`,
      `Pending changes's control "Help" is covered by DIV`,
      `Pending changes's control "Retry" is covered by DIV`,
    ]);

  // The second control: a tall `main`, read at the end of the document's scroll.
  await page.setContent(ownScrollerPage(true));
  await expect
    .poll(async () => (await page.evaluate(readFootEnds, FOOT_NAMES)).problems, {
      message: `pending-list ${at}: the last paragraph of a tall main is the one cover`,
    })
    .toEqual({
      covers: [
        expect.stringMatching(
          /^the document's own content meets a float at its end: "The last paragraph of the page\." at y \d+-\d+ x \d+-\d+ meets Pending changes at y \d+-\d+$/
        ),
      ],
      short: [],
      navigation: [],
      unsettled: [],
    });
});
