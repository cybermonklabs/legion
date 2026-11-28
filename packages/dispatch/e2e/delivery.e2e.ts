import { createProject, mintAgentToken, registerEval } from "./api";
import {
  FIXTURE_WINDOW as MEASURES_WINDOW,
  seedDeliveryFixture,
  seedMeasuresFixture,
  TIMELINE_FIXTURE_QUERY,
} from "./delivery-seed";
import { deploySource, projectWithDefault, saveDeliverySettings } from "./delivery-settings-steps";
import {
  evalsSection,
  evalsTabPath,
  expectEvalsTab,
  openEvalsTimeline,
  openMergeTimeline,
  paintedSection,
} from "./evals-tab";
import { seedFakeGithub } from "./fake-github-helpers";
import { sql } from "./psql";
import { expect, type Locator, type Page, test } from "./test";
import { pressFinger } from "./touch";
import { asUser } from "./users";

/** Where the chart draws a run's shape: its series' name, its column on the x axis (the deploys
 *  at 0.15 and the failures at 0.85 of the Deploys/fails column, `Timeline.tsx`'s
 *  GLOBAL_DEPLOY_X/GLOBAL_FAILURE_X) and its time. Run 500's production job finished at 01:40 and
 *  run 501's integration tests failed at 12:20 (`seedDeliveryFixture`). */
const DEPLOY_500 = { series: "Deploys", x: 0.15, at: "2024-06-01T01:40:00Z" };
const FAILURE_501 = { series: "Pipeline failures", x: 0.85, at: "2024-06-01T12:20:00Z" };

/** One reading of the chart the page made: where a point of a series stands on the page, and
 *  whether the chart has taken the size of its host. Both answers come from ECharts' own
 *  instance, because the chart draws on a canvas and a deploy dot or a failure triangle has no
 *  element to click: the page loads the echarts chunk once, so importing the same URL here hands
 *  back that module, whose `getInstanceByDom` finds the chart. `x` and `at` name the point;
 *  without them it is the series' first data point (a merge dot's x carries a per-PR jitter).
 *  Null until the chart exists, and `position` is null on its own until the chart has painted
 *  the series, the moments it holds an instance that has drawn nothing included: `Timeline.tsx`
 *  registers the chart with `echarts.init` in its mount effect and pushes the first `setOption`
 *  from a second effect React runs in a later task, and in between `getOption()` answers
 *  undefined (`this._model && this._model.getOption()`, echarts' own `core/echarts.js`). A
 *  caller polls this function, and a poll fails on a throw rather than asking again, so those
 *  moments have to read as "nothing painted yet". */
async function chartReading(
  page: Page,
  shape: { series: string; x?: number; at?: string }
): Promise<{ position: { x: number; y: number } | null; tookItsHost: boolean } | null> {
  return page.evaluate(async ({ series, x, at }) => {
    interface Chart {
      convertToPixel: (finder: { seriesName: string }, value: number[]) => number[];
      getHeight: () => number;
      getOption: () => { series?: { name?: string; data?: { value?: number[] }[] }[] } | undefined;
      getWidth: () => number;
    }
    const host = document.querySelector<HTMLElement>("[data-testid=delivery-chart]");
    if (host === null) return null;
    const chunks = performance
      .getEntriesByType("resource")
      .map((resource) => resource.name)
      .filter((name) => /\/assets\/index-[\w-]+\.js$/.test(name));
    let chart: Chart | undefined;
    for (const chunk of chunks) {
      // The page's own chunks, found by URL at run time: no specifier is known in advance.
      const loaded = (await import(chunk)) as {
        getInstanceByDom?: (element: HTMLElement) => Chart | undefined;
      };
      chart = loaded.getInstanceByDom?.(host);
      if (chart !== undefined) break;
    }
    if (chart === undefined) return null;
    const tookItsHost =
      chart.getWidth() === host.clientWidth && chart.getHeight() === host.clientHeight;
    const painted = chart.getOption()?.series?.find((candidate) => candidate.name === series);
    const first = painted?.data?.[0]?.value;
    if (first === undefined) return { position: null, tookItsHost };
    const value = x !== undefined && at !== undefined ? [x, Date.parse(at)] : first;
    const [px, py] = chart.convertToPixel({ seriesName: series }, value);
    if (px === undefined || py === undefined) return { position: null, tookItsHost };
    const rect = host.getBoundingClientRect();
    return { position: { x: rect.left + px, y: rect.top + py }, tookItsHost };
  }, shape);
}

/** The page position of a chart point, from ECharts' own coordinate conversion. Null until the
 *  chart has painted the series the shape names. */
async function shapePosition(
  page: Page,
  shape: { series: string; x?: number; at?: string }
): Promise<{ x: number; y: number } | null> {
  return (await chartReading(page, shape))?.position ?? null;
}

/** Opens the seeded timeline and waits for its chart to paint run 500's deploy and to have taken
 *  the size of its host, so a position or a page measurement read next is the settled one rather
 *  than one of a layout still running. On a phone the facets sit above the chart, so the chart is
 *  scrolled into view before anything points at it.
 *
 *  Both are read out of one `chartReading`, the one in-page call that answers them: ECharts
 *  measures the host once at `echarts.init` and again on every notice its `ResizeObserver` gets,
 *  and the lane header above it is relaid out from the same `containerWidth` (`Timeline.tsx`), so
 *  until a pass it owes has run its canvas is drawn to a size the host no longer has, every shape
 *  on it stands somewhere other than where it will stand, and the page is still being laid out
 *  around it. The poll's answer names both so a failure says which of the two the chart never
 *  reached. */
async function openTimeline(page: Page, query = ""): Promise<void> {
  await openEvalsTimeline(page, `${TIMELINE_FIXTURE_QUERY}${query}`);
  await page.getByTestId("delivery-chart").scrollIntoViewIfNeeded();
  await expect
    .poll(async () => {
      const reading = await chartReading(page, DEPLOY_500);
      return { painted: reading?.position != null, tookItsHost: reading?.tookItsHost === true };
    })
    .toEqual({ painted: true, tookItsHost: true });
}

/** Clicks a run's shape, placed again at the moment of the click: a phone's browser can scroll
 *  the page between a hover and the click that follows it. */
async function clickShape(page: Page, shape: { series: string; x: number; at: string }) {
  const position = await shapePosition(page, shape);
  if (position === null) throw new Error(`no ${shape.series} shape painted at ${shape.at}`);
  await page.mouse.click(position.x, position.y);
}

test("the Delivery address opens the default project's Evals tab", async ({ browser }) => {
  await seedDeliveryFixture();
  const context = await asUser(browser, "alice");
  const page = await context.newPage();

  // The old page's address keeps working: it opens the tab of the one project with delivery on,
  // with the window the address named.
  await page.goto(`/delivery?${TIMELINE_FIXTURE_QUERY}`);
  await expect(page).toHaveURL(evalsTabPath(TIMELINE_FIXTURE_QUERY));
  await expectEvalsTab(page);
  await openMergeTimeline(page);

  await context.close();
});

test("a facet lists each value with its count and narrows the PRs, kept in the URL", async ({
  browser,
}) => {
  await seedDeliveryFixture();
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  await openTimeline(page);

  await expect(page.getByText("2 PRs in current filter/window")).toBeVisible();
  const priority = page.getByRole("button", { name: /^Priority:/ });
  await expect(priority).toHaveText("Any priority");
  await priority.click();
  const options = page.getByRole("listbox", { name: "Priority options" }).getByRole("option");
  await expect(options).toHaveText([/^P0\s*1$/, /^No issue\s*1$/]);
  await expect(options.first()).toHaveAccessibleName("P0, 1 PR");
  await options.filter({ hasText: "P0" }).click();
  await page.keyboard.press("Escape");

  await expect(page.getByText("1 PRs in current filter/window")).toBeVisible();
  await expect(page).toHaveURL(/[?&]priority=P0(&|$)/);
  await context.close();
});

test("colour by priority with swimlanes splits the merges into one column per priority", async ({
  browser,
}) => {
  await seedDeliveryFixture();
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  await openTimeline(page);

  const lanes = page.getByRole("switch", { name: "Swimlanes" });
  await expect(lanes).toHaveAttribute("aria-checked", "false");
  await expect(page.getByTitle("Merges (2)")).toBeVisible();
  await page.getByLabel("Color merges by").selectOption("priority");
  await lanes.click();

  await expect(page).toHaveURL(/colorBy=priority/);
  await expect(page).toHaveURL(/lanes=1/);
  await expect(page.getByTitle("P0 (1)")).toBeVisible();
  await expect(page.getByTitle("No issue (1)")).toBeVisible();
  await context.close();
});

test("hovering a deploy names its run, and clicking it lists what it shipped and every job", async ({
  browser,
}) => {
  await seedDeliveryFixture();
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  await openTimeline(page);

  const deploy = await shapePosition(page, DEPLOY_500);
  if (deploy === null) throw new Error("no deploy dot painted");
  await page.mouse.move(deploy.x, deploy.y);
  await expect(page.getByText("Run #500: 1 PR(s) shipped")).toBeVisible();
  await clickShape(page, DEPLOY_500);

  const details = page.getByRole("dialog", { name: "Details" });
  await expect(details.getByRole("heading", { name: "Deploy: run #500" })).toBeVisible();
  await expect(details.getByText("acme/widgets#1 — feat: a shipped widget")).toBeVisible();
  await expect(details.getByText("widgets-release / widgets-release")).toBeVisible();
  await expect(details.getByText("success (480s)")).toBeVisible();
  await details.getByRole("button", { name: "Close details" }).click();
  await expect(details).toHaveCount(0);
  await context.close();
});

test("clicking a pipeline failure shows its failed job in red and every job's result", async ({
  browser,
}) => {
  await seedDeliveryFixture();
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  await openTimeline(page);

  const failure = await shapePosition(page, FAILURE_501);
  if (failure === null) throw new Error("no failure triangle painted");
  await page.mouse.move(failure.x, failure.y);
  await expect(page.getByText("Run #501 failed: integration tests")).toBeVisible();
  await clickShape(page, FAILURE_501);

  const details = page.getByRole("dialog", { name: "Details" });
  await expect(details.getByRole("heading", { name: "Pipeline failure: run #501" })).toBeVisible();
  await expect(details.getByText(/^integration tests at /)).toBeVisible();
  await expect(details.getByText("skipped")).toBeVisible();
  await context.close();
});

test("dragging along the time axis sets a brush window, which the header clears", async ({
  browser,
}) => {
  await seedDeliveryFixture();
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  await openTimeline(page);

  const chart = page.getByTestId("delivery-chart");
  // `openTimeline` scrolled the chart into view, which waits for it to show, and polled it until
  // painted, so the chart's box is there.
  const box = await chart.boundingBox();
  if (box === null) throw new Error("no chart");
  const x = box.x + box.width * 0.6;
  await page.mouse.move(x, box.y + box.height * 0.2);
  await page.mouse.down();
  await page.mouse.move(x, box.y + box.height * 0.4, { steps: 6 });
  await expect(page.getByTestId("delivery-brush-band")).toBeVisible();
  await page.mouse.move(x, box.y + box.height * 0.6, { steps: 6 });
  await page.mouse.up();

  await expect(page).toHaveURL(/[?&]ws=.*[?&]we=/);
  const clear = page.getByRole("button", { name: "clear brush window" });
  await expect(clear).toBeVisible();
  await clear.click();
  await expect(page).not.toHaveURL(/[?&]ws=/);
  await expect(clear).toHaveCount(0);
  await context.close();
});

test("dragging the zoom slider zooms without setting a brush window", async ({ browser }) => {
  await seedDeliveryFixture();
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  await openTimeline(page);

  // The vertical slider is the chart's rightmost 16 px (Timeline.tsx: `right: 4, width: 12`),
  // outside the plot grid, on the same canvas the brush listens to.
  const chart = page.getByTestId("delivery-chart");
  // `openTimeline` scrolled the chart into view, which waits for it to show, and polled it until
  // painted, so the chart's box is there.
  const box = await chart.boundingBox();
  if (box === null) throw new Error("no chart");
  const x = box.x + box.width - 10;
  await page.mouse.move(x, box.y + box.height * 0.4);
  await page.mouse.down();
  await page.mouse.move(x, box.y + box.height * 0.5, { steps: 6 });
  await page.mouse.move(x, box.y + box.height * 0.6, { steps: 6 });
  await page.mouse.up();

  await expect(page.getByTestId("delivery-brush-band")).toHaveCount(0);
  await expect(page.getByRole("button", { name: "clear brush window" })).toHaveCount(0);
  await expect(page).not.toHaveURL(/[?&](ws|we)=/);
  await expect(page.getByText("2 PRs in current filter/window")).toBeVisible();
  await context.close();
});

test("on a phone a finger dragged along the chart sets a brush window and moves nothing else @phone @phone-only", async ({
  browser,
}) => {
  await seedDeliveryFixture();
  // The reconcile pass the server runs on its own five-minute tick (`ReconcileInterval`,
  // `delivery/reconcile.go`) can land anywhere inside this row: the fake GitHub holds no
  // installation for acme/widgets, so the pass fails its permission check and
  // RecordReconcileError writes that failure, which the page's live socket carries to the
  // freshness rows. Measured with the timeline open and no finger on the page: the `prs` row
  // grows from `PRs never checked` to that sentence, the Source freshness block rewraps
  // 56 px -> 96 px, and the page, scrolled to the chart, follows it from 2186 to 2226. Written
  // here, word for word what that pass writes, so the page the finger meets is already the one
  // the pass leaves and a pass landing mid-row moves nothing (INFRA-1260).
  await sql(
    `UPDATE project_deploy_sources
       SET last_error = 'the GitHub App is not installed on acme/widgets'`
  );
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  await openTimeline(page);
  // The settle `before` is read after: the page's first body already carries that failure, and
  // this is what says so, rather than the drag being measured against rows still rewrapping.
  await expect(page.locator('[data-source="prs"]')).toHaveText(
    /the GitHub App is not installed on acme\/widgets/
  );

  // A real touch through Chromium's Input.dispatchTouchEvent (touch.ts), not page.mouse, which
  // the other rows use on this project too. The finger lands on #1's merge dot, so its first
  // contact shows that dot's tooltip, and drags from there along the time axis. `openTimeline`
  // scrolled the chart into view, which waits for it to show, so the chart's box is there.
  const box = await page.getByTestId("delivery-chart").boundingBox();
  if (box === null) throw new Error("no chart");
  const dot = await shapePosition(page, { series: "PR merges" });
  if (dot === null) throw new Error("no merge dot painted");
  const layout = () =>
    page.evaluate(() => ({
      scrollY: Math.round(window.scrollY),
      width: document.documentElement.scrollWidth,
    }));
  const before = await layout();
  const finger = await pressFinger(page, dot);
  await expect(page.getByText("acme/widgets#1: feat: a shipped widget")).toBeVisible();
  // Nothing the touch brings up stands past the viewport's right edge: ECharts paints the
  // tooltip inside the chart's host, whose wrapper clips it (`Timeline.tsx`), and the rows above
  // the chart hold their column (`SourceFreshness.tsx`). A phone lays its page out to the widest
  // thing on it, so one element past that edge widens the whole page and moves it under the
  // finger. Read once, not polled: a widening of a single frame is the defect, and a poll that
  // asks again until one sample fits cannot fail for it.
  expect(await layout()).toEqual(before);
  // Up the time axis (later times) from the dot, which sits near the chart's foot.
  await finger.moveTo({ x: dot.x, y: Math.max(box.y + 20, dot.y - box.height * 0.4) }, 8);
  await expect(page.getByTestId("delivery-brush-band")).toBeVisible();
  // The chart claims the finger: the page neither scrolls nor widens under it. Measured on this
  // project, the chart's own handlers are what hold it - with `touch-none` taken off
  // `Timeline.tsx` and the bundle rebuilt, a finger dragged down the chart with 2,226 px of room
  // above still moved the page 2,226 -> 2,226, while the same drag on the heading moved it
  // 0 -> 285 - so the class declares the intent and this reading catches a page moving under a
  // finger that is down, which is what it last failed on (INFRA-1260).
  expect(await layout()).toEqual(before);
  await finger.lift();

  await expect(page).toHaveURL(/[?&]ws=.*[?&]we=/);
  await expect(page.getByRole("button", { name: "clear brush window" })).toBeVisible();
  await context.close();
});

// Guards `min-w-0` on the freshness row (`SourceFreshness.tsx`): the row's text is one
// `truncate` line, so a flex item's automatic minimum size is the whole sentence, and without
// that class a row naming a reconcile error stands past the column and a phone, which lays its
// page out to its widest content, widens and scrolls the whole page under the reader. The row
// above carries that failure too, but written before its page opens, so what it reads is a page
// laid out once around the longest text the rows can carry; here the failure is written while
// the finger is down, so the rewrap itself happens under the finger and the width is read
// across it.
test("on a phone a reconcile error arriving under the finger widens nothing @phone @phone-only", async ({
  browser,
}) => {
  await seedDeliveryFixture();
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  await openTimeline(page);

  const dot = await shapePosition(page, { series: "PR merges" });
  if (dot === null) throw new Error("no merge dot painted");
  // The width alone, not the row above's whole layout: a row that turns red carries a longer
  // sentence and the freshness rows rewrap, which moves what is under them down and takes the
  // page's scroll offset with it. Standing past the viewport's right edge is the defect.
  const width = () => page.evaluate(() => document.documentElement.scrollWidth);
  const before = await width();
  const finger = await pressFinger(page, dot);
  await expect(page.getByText("acme/widgets#1: feat: a shipped widget")).toBeVisible();
  // The failed pass the desktop freshness row below seeds, written here over the page's live
  // socket instead: reconcile.go's RecordReconcileError leaves last_reconcile_at set and
  // last_error naming the failure, and the PRs row carries that name.
  await sql(
    `UPDATE project_deploy_sources
       SET last_reconcile_at = '2024-06-01T01:00:00Z',
           last_error = 'the installation lacks Actions: read on acme/widgets'`
  );
  await expect(page.locator('[data-source="prs"]')).toHaveText(
    /the installation lacks Actions: read on acme\/widgets/
  );
  expect(await width()).toBe(before);
  await finger.lift();
  await context.close();
});

test("the Repository facet narrows the list through the server", async ({ browser }) => {
  await seedDeliveryFixture();
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  await openTimeline(page);

  await page.getByRole("button", { name: "List", exact: true }).click();
  await expect(page).toHaveURL(/mode=list/);
  const titles = page.locator("tbody tr td:nth-child(4)");
  await expect(titles).toHaveCount(2);

  // The Repository facet narrows the list server-side; acme/widgets is the only repository
  // seeded, so selecting it is a no-op on the result but proves the picker and the facet
  // round-trip to the server run. Each option carries its count.
  const repository = page.getByRole("button", { name: /^Repository:/ });
  await repository.click();
  await page.getByRole("option", { name: "acme/widgets, 2 PRs" }).click();
  await page.keyboard.press("Escape");
  await expect(repository).toHaveText("1 selected");
  await expect(titles).toHaveCount(2);
  await context.close();
});

test("the list reaches a deploy's drill-down by keyboard, and Escape hands focus back", async ({
  browser,
}) => {
  await seedDeliveryFixture();
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  await openEvalsTimeline(page, `${TIMELINE_FIXTURE_QUERY}&mode=list`);

  // The rows are one tab stop: the first row takes focus, the arrow key moves to the next.
  const rows = evalsSection(page, "timeline").locator("tbody tr[aria-rowindex]");
  await expect(rows).toHaveCount(2);
  await rows.first().focus();
  await page.keyboard.press("ArrowDown");
  await expect(rows.nth(1)).toBeFocused();
  await page.keyboard.press("Home");
  await expect(rows.first()).toBeFocused();

  // #1 shipped in run 500: its "deployed" opens that run's drill-down.
  const deployed = page.getByRole("button", { name: "Open deploy run #500" });
  await deployed.focus();
  await page.keyboard.press("Enter");
  const details = page.getByRole("dialog", { name: "Details" });
  await expect(details.getByRole("heading", { name: "Deploy: run #500" })).toBeVisible();
  await expect(details.getByRole("button", { name: "Close details" })).toBeFocused();
  await page.keyboard.press("Escape");
  await expect(details).toHaveCount(0);
  await expect(deployed).toBeFocused();
  await context.close();
});

test("the freshness row shows a named reconcile failure, distinct from mere staleness", async ({
  browser,
}) => {
  await seedDeliveryFixture();
  // A reconcile pass that failed (a missing GitHub App permission here) still advances nothing
  // happy: last_reconcile_at is set (a prior pass did succeed once) but last_error names the most
  // recent failure, and the freshness row must show that failure by name, not the generic
  // "Reconcile X ago"/"never ran" wording a merely-stale-but-healthy row gets (lib/freshness.ts's
  // reconcileRow). The stack's own server runs the delivery reconcile on a ticker of its own, so a
  // pass can land between the write below and the page's read of it, and a failing pass
  // overwrites last_error (store.go's RecordReconcileError). Hence the fake GitHub: acme/widgets
  // with Contents and Pull-requests but no Actions makes every such pass fail with exactly the
  // text seeded here (DeliveryPermissions' ErrNoActionsRead, "the installation lacks Actions:
  // read on acme/widgets"), so a pass inside this row's window changes nothing. The SQL seed
  // stays: the page must show the failure the instant it loads, before any pass has run.
  // To see that for oneself, run this row with DISPATCH_TEST_DELIVERY_RECONCILE_INTERVAL=1s, which
  // e2e/run-server.sh forwards to the stack's server: several passes then land inside the window
  // below and the row still reads the same.
  await seedFakeGithub({
    "acme/widgets": { contents: "read", installation_id: 690, pull_requests: "read" },
  });
  await sql(
    `UPDATE project_deploy_sources
       SET last_reconcile_at = '2024-06-01T01:00:00Z',
           last_error = 'the installation lacks Actions: read on acme/widgets'`
  );
  const context = await asUser(browser, "alice");
  const page = await context.newPage();

  await page.goto(evalsTabPath(TIMELINE_FIXTURE_QUERY));

  await expectEvalsTab(page);
  await expect(
    page.getByText(/^PRs \d+ h ago: the installation lacks Actions: read on acme\/widgets$/)
  ).toBeVisible();
  // Never the never-checked wording once a pass has succeeded.
  await expect(page.getByText("PRs never checked")).toHaveCount(0);

  await context.close();
});

test("an unfetchable pull request shows on the freshness row by count", async ({ browser }) => {
  await seedDeliveryFixture();
  // S3: a population pull request whose completing fetch answered a permanent 404/410 from
  // GitHub is marked unfetchable (store.go's MarkPullRequestUnfetchable) rather than retried
  // every reconcile pass forever -- the freshness row surfaces it by count.
  await sql(
    `UPDATE delivery_pull_requests
       SET unfetchable_at = '2024-06-01T05:00:00Z', unfetchable_reason = 'pull request not found or gone (status 404)'
       WHERE repo = 'acme/widgets' AND number = 2`
  );
  const context = await asUser(browser, "alice");
  const page = await context.newPage();

  await page.goto(evalsTabPath(TIMELINE_FIXTURE_QUERY));

  await expectEvalsTab(page);
  await expect(
    page.getByText(/1 pull request can no longer be fetched from GitHub/i)
  ).toBeVisible();

  await context.close();
});

// @pr: Live delivery: the Delivery page from nothing - a project's settings tab saved against the GitHub App's own read, then the timeline it draws.
test("an unconfigured Delivery page links to the project settings tabs, and a save there brings the timeline @pr", async ({
  browser,
}, testInfo) => {
  // The save proves the GitHub App can read each source's repository (the installation with
  // Contents: read, a minted token, then the repository read under it); the fake answers all
  // three for a seeded repository and 404s an unseeded one.
  await seedFakeGithub({ "acme/widgets": { contents: "read", installation_id: 301 } });
  await projectWithDefault("ACME", "Acme", "acme/widgets");
  const context = await asUser(browser, "alice");
  const page = await context.newPage();

  try {
    // ACME has no Legion daemon and no delivery settings: its Evals tab shows one line that links
    // to both settings, the delivery settings on its own settings tab.
    await page.goto(evalsTabPath(TIMELINE_FIXTURE_QUERY));
    await expectEvalsTab(page);
    const missing = page.locator("[data-missing-sources]");
    await expect(missing).toContainText("Nothing to show yet");
    await missing.getByRole("link", { name: "delivery settings" }).click();
    await expect(page).toHaveURL(/\/projects\/ACME\/settings$/);
    const delivery = page.getByRole("switch", { name: "Delivery" });
    await expect(delivery).not.toBeChecked();
    await delivery.check();
    const source = deploySource(page, 1);
    const repository = source.getByLabel("Repository", { exact: true });
    await expect(repository).toHaveValue("");
    await expect(repository).toHaveAttribute("placeholder", "acme/widgets");

    // A repository the App cannot read is refused with the server's own reason, and nothing is
    // stored: the switch and the draft stay.
    await repository.fill("acme/unseen");
    await source
      .getByLabel("Deploy workflow", { exact: true })
      .fill(".github/workflows/deploy.yml");
    await source
      .getByLabel("Production jobs", { exact: true })
      .fill("widgets-release / widgets-release\nrelease / release");
    await source
      .getByLabel("Production apply step", { exact: true })
      .fill("Run ./.github/actions/apply");
    await source
      .getByLabel("PR checks workflow", { exact: true })
      .fill(".github/workflows/pr-checks.yml");
    await page
      .getByLabel("Population authors", { exact: true })
      .fill("octocat\noctocat-agent[bot]");
    await page.getByLabel("Excluded repositories", { exact: true }).fill("acme/dojo");
    await page.getByRole("button", { name: "Save delivery settings" }).click();
    await expect(
      page.getByRole("alert").filter({ hasText: "the GitHub App is not installed on acme/unseen" })
    ).toBeVisible();
    await expect(delivery).toBeChecked();
    expect(await sql("SELECT count(*) FROM project_delivery_settings")).toBe("0");
    await page.screenshot({
      path: testInfo.outputPath("delivery-setup-refused.png"),
      fullPage: true,
    });

    // Emptied, the source reads the project's default repository, which the App can read.
    await repository.fill("");
    await saveDeliverySettings(page, "ACME");
    expect(
      await sql(
        `SELECT s.delivery || ' ' || coalesce(d.repo, 'default') || ' ' || array_to_string(s.population_authors, ',') || ' ' || array_to_string(s.excluded_repos, ',') || ' ' || array_to_string(d.production_job_names, ',') || ' ' || d.production_apply_step_name
           FROM project_delivery_settings s JOIN project_deploy_sources d USING (project_key)
          WHERE s.project_key = 'ACME'`
      )
    ).toBe(
      "true default octocat,octocat-agent[bot] acme/dojo widgets-release / widgets-release,release / release Run ./.github/actions/apply"
    );
    await page.screenshot({
      path: testInfo.outputPath("delivery-settings.png"),
      fullPage: true,
    });

    // The Evals tab now draws its sections and the merge timeline: nothing has been reconciled
    // yet, so the freshness row reads never-happened and the list view is empty.
    await page.goto(evalsTabPath(TIMELINE_FIXTURE_QUERY));
    await expectEvalsTab(page);
    await expect(page.locator("[data-missing-sources]")).toHaveCount(0);
    await expect(page.getByText("PRs never checked")).toBeVisible();
    const timeline = await openMergeTimeline(page);
    await page.screenshot({
      path: testInfo.outputPath("delivery-after-save.png"),
      fullPage: true,
    });
    await timeline.getByRole("button", { name: "List", exact: true }).click();
    await expect(timeline.getByText("No PRs in the current filter/window.")).toBeVisible();
  } finally {
    await context.close();
  }
});

const MEASURES_QUERY = `from=${encodeURIComponent(MEASURES_WINDOW.from)}&to=${encodeURIComponent(MEASURES_WINDOW.to)}`;

/** The measures fixture's Evals tab once its target cards have painted. */
async function openMeasures(page: Page, query = ""): Promise<Locator> {
  await page.goto(evalsTabPath(`${MEASURES_QUERY}${query}`));
  await expectEvalsTab(page);
  return paintedSection(page, "cards");
}

/** A target card's figure over the window (`TargetCards.tsx`, `EvalChart.tsx`'s
 *  `data-window-statistic`): its eval's slug, its series and its statistic. */
function cardFigure(cards: Locator, slug: string, series: string, statistic: string): Locator {
  return cards.locator(
    `[data-card="${slug}"] [data-window-series="${series}"] [data-window-statistic="${statistic}"]`
  );
}

test("the measures panel shows the four measures for the window and follows a facet", async ({
  browser,
}) => {
  await seedMeasuresFixture();
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  const cards = await openMeasures(page);

  await expect(cardFigure(cards, "deploy-frequency", "deploys", "deploys")).toHaveText(/^5(?!\d)/);
  // The stored-path median: 510 minutes, each PR's deploy derived from the stored runs.
  await expect(cardFigure(cards, "merge-to-production", "minutes", "median")).toHaveText("8.5h");
  // ACME-103 is the one open P0 with neither a claim nor a route; the card opens it when pressed.
  await expect(cards.locator('[data-card="unowned-p0"] [data-group-count]')).toHaveAttribute(
    "data-group-count",
    "1"
  );
  const rework = (await paintedSection(page, "deploys")).locator('[data-eval="rework-share"]');
  await expect(rework).toContainText("28.6%");

  // The repository narrows every number: acme/gadgets#20 drops out, 4 rework of 13.
  await page.getByLabel("Repository", { exact: true }).fill("acme/widgets");
  await page.getByRole("button", { exact: true, name: "Add" }).click();
  await expect(page).toHaveURL(/[?&]repo=acme%2Fwidgets/);
  await expect(rework).toContainText("30.8%");
  await context.close();
});

test("the measures API gives an agent the numbers the panel shows", async ({ browser }) => {
  await seedMeasuresFixture();
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  const token = await mintAgentToken("measures-e2e", "alice");
  const response = await page.request.get(`/api/v1/delivery/measures?${MEASURES_QUERY}`, {
    headers: { Authorization: `Bearer ${token}` },
  });
  expect(response.status()).toBe(200);
  const body = await response.json();
  expect(body.measures.deploy_frequency.successful_deploys).toBe(5);
  expect(body.status.unowned_p0).toBe(false);
  await context.close();
});

// dispatch://INFRA-2916's R5: a card judges its figure by the target a person sets on the
// project's eval, not a constant of the code (dispatch://INFRA-978, D9). A target written on
// deploy-frequency moves the card's verdict, and a null target turns it off.
test("the measures panel judges deploys a day by the target a person set on the eval", async ({
  browser,
}) => {
  // Made through the API, so its evals are seeded in the create's own transaction and ACME holds
  // them once it has answered, before the fixture's settings name it.
  await createProject({ key: "ACME", name: "Acme" });
  await seedMeasuresFixture();
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  const verdict = (cards: Locator) =>
    cards.locator('[data-card="deploy-frequency"] [data-target-met]');

  // 5 deploys over the window: under the catalogue's 20 a day, which is also the target ACME's seed
  // wrote.
  await expect(verdict(await openMeasures(page))).toHaveAttribute("data-target-met", "false");

  // Each write is a new version of ACME's eval, which the tab's evals view reads at its next
  // answer, so the tab is opened again until it judges by the new target.
  await registerEval("ACME", { slug: "deploy-frequency", target: 0.1, target_inclusive: true });
  await expect
    .poll(async () => await verdict(await openMeasures(page)).getAttribute("data-target-met"))
    .toBe("true");

  await registerEval("ACME", { slug: "deploy-frequency", target: null });
  await expect.poll(async () => verdict(await openMeasures(page)).count()).toBe(0);
  await context.close();
});

test("a brush window narrows the measures to it", async ({ browser }) => {
  await seedMeasuresFixture();
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  // The prototype's own computeDora over the brush window 2026-09-05..2026-09-12 (PRs #101-#107,
  // each deploy derived by its containment): 3 deploys, merge → production median 1140 minutes.
  // The target cards read the whole UTC days the brush touches.
  const brush = `ws=${encodeURIComponent("2026-09-05T00:00:00.000Z")}&we=${encodeURIComponent("2026-09-12T00:00:00.000Z")}`;
  const cards = await openMeasures(page, `&${brush}`);
  await expect(cards.locator("[data-card-days]").first()).toBeVisible();
  await expect(cardFigure(cards, "deploy-frequency", "deploys", "deploys")).toHaveText(/^3(?!\d)/);
  await expect(cardFigure(cards, "merge-to-production", "minutes", "median")).toHaveText("19.0h");

  await page.getByRole("button", { name: "clear brush window" }).click();
  await expect(cardFigure(cards, "deploy-frequency", "deploys", "deploys")).toHaveText(/^5(?!\d)/);
  await context.close();
});

test("the definitions fold opens to the per-day table", async ({ browser }, testInfo) => {
  await seedMeasuresFixture();
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  await openMeasures(page);

  // Each chart of the Deploys and lead time section turns to its per-day table.
  const deploys = await paintedSection(page, "deploys");
  const chart = deploys.locator('[data-eval="deploy-run-success"]');
  await chart.getByRole("button", { name: "Table" }).click();
  await expect(chart.getByRole("table").locator('[data-x="2026-09-27"]').first()).toBeVisible();
  await page.screenshot({ path: testInfo.outputPath("delivery-measures.png"), fullPage: true });
  await context.close();
});
