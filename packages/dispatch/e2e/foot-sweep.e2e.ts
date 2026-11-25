/** The sweep of the foot of the screen (dispatch://INFRA-4731): Pending changes and the update
 *  banner float at the foot (`web/src/hooks/useFootInset.ts`), and wherever they show, every
 *  reading here is that the foot is clear, as the tester's probe reads it (`e2e/foot-probe.ts`):
 *
 *  - no float's box meets another foot element's box;
 *  - no float covers the centre of a control;
 *  - each scroller's last content, read at its real end, lies wholly above every float's top;
 *  - the shell's Navigation stays on the screen at the end of a page;
 *  - a scroller that scrolls only sideways keeps its height under a float;
 *  - with no float shown, no scroller carries end room.
 *
 *  Three kinds of row read it. The rows by route group open every reviewed route of
 *  `e2e/review-routes.ts` against the review corpus (`e2e/review-seed.ts`) at three desktop
 *  sizes with both floats shown, then close both and read that no end room is left. The rows of
 *  the fixed surface open eight pages, one row each and two for the issues table (the selection
 *  bar hidden and shown), seeded so each page's scrollers reach the foot, and read every
 *  combination of the floats at seven sizes from a phone's up. And one row holds
 *  the two steps the architect named: a float over a sticky column, and a box that scrolls only
 *  sideways under a float.
 *
 *  Each reading prints one `FOOT-CELL <page> <WxH> <floats> <clean|problems…>` line, so a run's
 *  output is the table of every cell. Every title carries `@foot`, which the check c-foot-clear
 *  reads from the full lane; none carries `@pr`, so the pull-request lane runs none of them. None
 *  carries `@phone` either: each row sets every size it reads itself, the phone sizes included,
 *  so the `iphone` project would read the same sizes again. */
import { MARK_ATTRIBUTE } from "../web/src/hooks/useFootInset";
import { type FakeSession, setLiveSessions, setSessionLive } from "./agents";
import {
  createAgentMessage,
  createAsk,
  createIssue,
  createMessage,
  listIssues,
  replyToMessageDelivery,
  seedAgentStreamConversations,
} from "./api";
import { expectTimeout } from "./expect-timeout";
import { openWithFloats, routeFloats, seedRefusedMove, updateBanner } from "./foot-floats";
import {
  expectFootClear,
  type FootSelectors,
  footCellLine,
  readFootEnds,
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

/** Every foot element of the page layer a page here can show (`FOOT_LAYERS`), each by a selector
 *  that matches it and no other element of these pages, and the Conversation's and the live
 *  view's `Jump to latest`. `readFootElements` leaves out each one a page does not show. The
 *  selection bar is the one toolbar with no name: the new issue page's view row is a toolbar
 *  named by its controls. */
const FOOT_ELEMENTS: FootSelectors = {
  "the selection bar": '[role="toolbar"]:not([aria-label])',
  "the broadcast composer": 'section[aria-label="Broadcast"]',
  "the comment composer": 'form[aria-label="Comment composer"]',
  "the margin's sheet": 'aside[aria-label="Review panel"]',
  "the live view's composer": '[data-testid="agent-composer"]',
  "Jump to latest": '[data-testid="jump-to-latest"]',
};

/** What a cell shows at the foot: Pending changes, the update banner and, on the issues table,
 *  the selection bar. */
type Shown = "pending" | "banner" | "bar";

/** A cell's name in its `FOOT-CELL` line and its failure: `<page> <WxH> <floats>`, the floats
 *  `none` where it shows nothing. */
function cellName(page: string, size: Size, shown: readonly Shown[]): string {
  return `${page} ${size.width}x${size.height} ${shown.join("+") || "none"}`;
}

/** Fails unless no element of `page` carries `MARK_ATTRIBUTE`, read until it holds with the
 *  document and every scroller back at the start of its scroll: with no float shown, no scroller
 *  keeps end room. The start is where the Conversation's `Jump to latest`, a float of the foot's
 *  top layer, is gone: the Conversation lists its newest turn first, so the button shows once the
 *  reader leaves the top, and the document takes room for it there, as the rule asks. Each read
 *  is made two frames after the scroll, so the foot's pass for it has run. Prints the cell's
 *  `FOOT-CELL` line either way, naming each marked element where the read failed. */
async function expectNoEndRoom(page: Page, cell: string): Promise<void> {
  let marked: readonly string[] = ["the page was never read"];
  try {
    await expect
      .poll(
        async () => {
          marked = await page.evaluate(async (attribute) => {
            const scrolled = [document.documentElement, ...document.querySelectorAll("*")];
            for (const element of scrolled) if (element.scrollTop > 0) element.scrollTop = 0;
            const frames = Promise.withResolvers<void>();
            requestAnimationFrame(() => requestAnimationFrame(() => frames.resolve()));
            await frames.promise;
            const jump = document.querySelector('[data-testid="jump-to-latest"]');
            const jumpShown = jump !== null && jump.getBoundingClientRect().height > 0;
            return [...document.querySelectorAll(`[${attribute}]`)].map(
              (element) =>
                `${element.tagName}${element.id ? `#${element.id}` : ""} carries ${attribute}` +
                (jumpShown ? " while Jump to latest shows" : "")
            );
          }, MARK_ATTRIBUTE);
          return marked;
        },
        { message: `${cell}: no element carries ${MARK_ATTRIBUTE}` }
      )
      .toEqual([]);
  } finally {
    console.log(footCellLine(cell, marked));
  }
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
          await page.evaluate(readFootEnds);
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

/** Reads `page`, named `name`, at every size of the fixed surface with `shown` at the foot: the
 *  foot clear where a float shows, and no end room where none does. Where `stuck`, each reading
 *  with a float shown is followed by `expectStuckInPlace`, as a cell of `<name>:stuck`. */
async function readEverySize(
  page: Page,
  name: string,
  shown: readonly Shown[],
  stuck = false
): Promise<void> {
  if (shown.includes("pending")) await expect(pendingWrites(page)).toBeVisible();
  if (shown.includes("banner")) await expect(updateBanner(page)).toBeVisible();
  if (shown.includes("bar")) await expect(page.getByRole("toolbar")).toBeVisible();
  const floatShown = shown.includes("pending") || shown.includes("banner");
  for (const size of SURFACE_SIZES) {
    await page.setViewportSize(size);
    const cell = cellName(name, size, shown);
    if (floatShown) await expectFootClear(page, FOOT_ELEMENTS, cell);
    else await expectNoEndRoom(page, cell);
    if (floatShown && stuck) await expectStuckInPlace(page, cellName(`${name}:stuck`, size, shown));
  }
}

/** Dismisses the update banner, which stays dismissed for the page's life. */
async function dismissBanner(page: Page): Promise<void> {
  const banner = updateBanner(page);
  await banner.getByRole("button", { name: "Dismiss update notice" }).click({ timeout: 5_000 });
  await expect(banner).toHaveCount(0);
}

/** Discards the refused change, which closes Pending changes. */
async function discardPending(page: Page): Promise<void> {
  const pending = pendingWrites(page);
  await pending.getByRole("button", { name: "Discard", exact: true }).click({ timeout: 5_000 });
  await expect(pending).toHaveCount(0);
}

/** Loads `path` on `page` again with the update banner shown, as a load of the page shows it
 *  from the builds `routeFloats` answers; Pending changes keeps the change to `refusedKey`
 *  across the load. */
async function reloadWithBanner(page: Page, refusedKey: string, path: string): Promise<void> {
  await page.goto(path);
  await page.evaluate(() => window.dispatchEvent(new Event("focus")));
  await expect(updateBanner(page)).toBeVisible();
  await expect(pendingWrites(page)).toContainText(refusedKey);
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
          await expectFootClear(
            page,
            FOOT_ELEMENTS,
            cellName(route.name, size, ["pending", "banner"])
          );
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
 *  drawn once it is ready to read, after each load. Where `stuck`, each reading with a float
 *  shown is followed by `expectStuckInPlace`. */
interface Surface {
  readonly name: string;
  readonly path: string;
  readonly ready: (page: Page) => Promise<void>;
  readonly stuck?: true;
}

/**
 * Reads `surface` at every size of the fixed surface with each combination of the floats: both,
 * Pending changes alone, the banner alone and neither. The banner, once dismissed, stays so for
 * the page's life, so the page is loaded afresh for the banner alone: Pending changes keeps the
 * refused change across the load, and is then discarded. `first` runs once, after the first
 * load, before the page is read.
 */
async function sweepSurface(
  page: Page,
  refusedKey: string,
  surface: Surface,
  first?: (page: Page) => Promise<void>
): Promise<void> {
  await routeFloats(page, refusedKey);
  await openWithFloats(page, refusedKey, surface.path);
  await first?.(page);
  await surface.ready(page);
  const stuck = surface.stuck === true;
  await readEverySize(page, surface.name, ["pending", "banner"], stuck);
  await dismissBanner(page);
  await readEverySize(page, surface.name, ["pending"], stuck);
  await reloadWithBanner(page, refusedKey, surface.path);
  await surface.ready(page);
  await discardPending(page);
  await readEverySize(page, surface.name, ["banner"], stuck);
  await dismissBanner(page);
  await readEverySize(page, surface.name, []);
}

/** Issues enough to run the issues table's box, and the Board's Triage column, past the window
 *  at every size. Nothing reads their order among themselves, so they go out side by side. */
async function seedFillers(): Promise<void> {
  await Promise.all(
    Array.from({ length: 40 }, (_, index) =>
      createIssue({ project: "CORE", title: `Filler issue number ${index}` })
    )
  );
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
    await sweepSurface(
      page,
      refused.key,
      {
        name: "board",
        path: "/projects/CORE/issues",
        ready: async () => {
          await expect(
            board.getByRole("article").filter({ hasText: "Filler issue number 39" })
          ).toHaveCount(1);
        },
      },
      async () => {
        await page.getByRole("button", { name: "Board" }).click();
        await expect(page.getByRole("button", { name: "Board" })).toHaveAttribute(
          "aria-pressed",
          "true"
        );
      }
    );
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
  /** A live session for the Agents page to list. Each id is a UUID, which Dispatch's agent
   *  routes require. */
  const session = (sessionID: string, title: string): FakeSession => ({
    capabilities: ["aside", "btw"],
    dir: `/workspaces/${title.toLowerCase()}`,
    last_seen: Date.now() - 5_000,
    machine_id: `${title.toLowerCase()}-host`,
    roles: [title.toLowerCase()],
    session_id: sessionID,
    title,
  });
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
          session(`01a04731-0c07-7000-8000-0000000004${String(index + 1).padStart(2, "0")}`, title)
        ),
        session(endedSession, "Ended"),
      ]);
      const asked = await createAgentMessage(endedSession, {
        body: "How far along?",
        delivery: "aside",
      });
      await replyToMessageDelivery(
        asked.id,
        { attempt: 1, body: "Done and gone." },
        { id: endedSession, kind: "session" }
      );
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
// The document scrolls the frame's `main`, which takes end room for the floats, so each reading
// with a float shown is followed by one of the frame's icon rail, its Contents pane and every
// element stuck in `main` at the end of the scroll (`expectStuckInPlace`): the room must carry
// none of them off its place.
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
      path: `/issues/${reviewed}`,
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

// An agent's live view, its transcript at the stream's limit, as the live view row of
// `e2e/issues-table.e2e.ts` seeds it.
// SLOW(>=2s): a transcript at the stream's limit, and four combinations of the floats at seven
// sizes, each a settled reading at the end of every scroller.
test("the floats leave the foot of an agent's live view clear in every combination at seven sizes @foot", async ({
  browser,
}) => {
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
    });
  } finally {
    await context.close();
  }
});

/** How many messages the Conversation row seeds: forty run the tab thousands of pixels past a
 *  desktop window (`e2e/conversation.e2e.ts` measured 2,700 px at 1280x720). */
const CONVERSATION_MESSAGES = 40;

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
  const issue = await createIssue({ project: "CORE", title: "A long conversation" });
  // One at a time: the server appends one event at a time per issue, so a burst costs the same,
  // and the tab lists the messages in the order they were written.
  for (let index = 1; index <= CONVERSATION_MESSAGES; index += 1) {
    await createMessage(issue.key, { body: `Conversation message ${index}` }, agent);
  }
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await sweepSurface(page, refused.key, {
      name: "conversation",
      path: `/issues/${issue.key}/conversation`,
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
// The two steps the architect named.
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
 * it holds, each read at the end of every scroll (`readFootEnds`). Pending changes is wider than
 * the margin, so its left edge lies over `main`, which takes room for that part by the same
 * rule: the room `main` holds is read and printed after the cell's `FOOT-CELL` line, which is
 * printed either way, and is no problem of the step's.
 */
async function expectStickyClear(page: Page, cell: string): Promise<void> {
  let problems: readonly string[] = ["the page was never read"];
  let main = "";
  try {
    await expect
      .poll(
        async () => {
          const ends = await page.evaluate(readFootEnds);
          const read = await page.evaluate((attribute) => {
            const column = document.querySelector<HTMLElement>('aside[aria-label="Review margin"]');
            const pending = document.querySelector('[aria-label="Pending changes"]');
            const shell = document.getElementById("main-content");
            const mainRoom = shell?.hasAttribute(attribute)
              ? `main holds ${shell.style.paddingBottom} of end room`
              : "main holds no end room";
            if (column === null) return { main: mainRoom, problems: ["no Review margin shows"] };
            if (pending === null) return { main: mainRoom, problems: ["no Pending changes shows"] };
            const a = column.getBoundingClientRect();
            const b = pending.getBoundingClientRect();
            const over =
              Math.min(a.right, b.right) - Math.max(a.left, b.left) > 1 &&
              Math.min(a.bottom, b.bottom) - Math.max(a.top, b.top) > 1;
            // The margin's own room lengthens its scroll, so its content is measured without it.
            const room = Number.parseFloat(column.style.getPropertyValue("--foot-clearance")) || 0;
            return {
              main: mainRoom,
              problems: [
                ...(over ? [] : ["Pending changes does not lie over the Review margin"]),
                ...(column.scrollHeight - room > column.clientHeight + 1
                  ? ["the Review margin's content does not fit it"]
                  : []),
                ...(column.hasAttribute(attribute)
                  ? []
                  : [`the Review margin carries no ${attribute}`]),
                ...(document.body.hasAttribute(attribute)
                  ? [`body carries ${attribute} (${document.body.style.paddingBottom})`]
                  : []),
              ],
            };
          }, MARK_ATTRIBUTE);
          main = read.main;
          problems = [
            ...read.problems,
            ...(ends.navigationTop === null
              ? ["the Navigation does not show"]
              : ends.navigationTop < 0
                ? [`the Navigation's top is at y ${ends.navigationTop} at the end of the page`]
                : []),
          ];
          return problems;
        },
        { message: cell }
      )
      .toEqual([]);
  } finally {
    console.log(`${footCellLine(cell, problems)} (${main})`);
  }
}

/** The code block's computed overflow on both axes, its height, and the top of the paragraph
 *  after it in the document, which no scroll moves; and whether the update banner lies over the
 *  block. */
interface SidewaysReading {
  readonly overflow: string;
  readonly height: number;
  readonly after: number;
  readonly under: boolean;
}

/** Reads the code block of `CODE_SPEC` on `page` (`SidewaysReading`). Where `toBanner`, scrolls
 *  the document first so the block's middle stands at the banner's. */
function readSideways(page: Page, toBanner: boolean): Promise<SidewaysReading> {
  return page.evaluate(
    ({ after, code, toBanner }) => {
      const block = [...document.querySelectorAll("pre")].find((pre) =>
        (pre.textContent ?? "").includes(code)
      );
      const next = [...document.querySelectorAll("p")].find(
        (paragraph) => paragraph.textContent === after
      );
      if (block === undefined || next === undefined) {
        return { after: Number.NaN, height: Number.NaN, overflow: "no block", under: false };
      }
      const banner = document.querySelector('[aria-label="A new version is available"]');
      const bannerBox = banner?.getBoundingClientRect();
      if (toBanner && bannerBox !== undefined) {
        const box = block.getBoundingClientRect();
        window.scrollBy(0, box.top + box.height / 2 - (bannerBox.top + bannerBox.height / 2));
      }
      const box = block.getBoundingClientRect();
      const { overflowX, overflowY } = getComputedStyle(block);
      const under =
        bannerBox !== undefined &&
        bannerBox.height > 0 &&
        Math.min(box.right, bannerBox.right) - Math.max(box.left, bannerBox.left) > 1 &&
        Math.min(box.bottom, bannerBox.bottom) - Math.max(box.top, bannerBox.top) > 1;
      return {
        after: Math.round(next.getBoundingClientRect().top + window.scrollY),
        height: Math.round(box.height),
        overflow: `${overflowX}/${overflowY}`,
        under,
      };
    },
    { after: AFTER_CODE, code: CODE_LINES[0], toBanner }
  );
}

// Two steps the architect named, each read as a settled state.
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
            rest = (await page.evaluate(sampleFootRest, 500)).length;
            shown = await readSideways(page, false);
            return { ...shown, rest };
          },
          { message: `the code block under the banner, at rest, at ${at}` }
        )
        .toMatchObject({ overflow: "auto/auto", rest: 1, under: true });
    } finally {
      console.log(
        footCellLine(
          `sideways-step ${at} banner`,
          shown?.under === true && shown.overflow === "auto/auto" && rest === 1
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
            return { after: closed.after, height: closed.height };
          },
          { message: `the code block's height and the paragraph after it, banner closed, at ${at}` }
        )
        .toEqual({ after: shown?.after, height: shown?.height });
    } finally {
      console.log(
        footCellLine(
          `sideways-step ${at} none`,
          closed?.height === shown?.height && closed?.after === shown?.after
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
