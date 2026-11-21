import type { Locator, Page, Route } from "@playwright/test";
import { presetPath } from "../web/src/features/issue-page/issue-address";
import {
  BLOCK_ATTRIBUTE_LABELS,
  DECLARED_WORDS,
  EMPTY_STATE_LINES,
  PLAN_WORDS,
  PRESET_NAMES,
  STATE_LINES,
  wordsIn,
} from "../web/src/features/issue-page/page-words";
import { blockNodeId } from "../web/src/features/issue-page/tree-drafts";
import { createComment, createNamedVersion, getArtifact, getIssue } from "./api";
import {
  type PlanOrderSeed,
  seedPlanOrder,
  seedWholeIssue,
  type WholeIssueSeed,
} from "./issue-layout-seed";
import { PLAN_ORDER_TASKS, requirementBlockId } from "./issue-layout-seed-text";
import { gotoIssue, issuePage, phase, presetToggle } from "./issue-page";
import {
  DESKTOP,
  drawnRows,
  expectIssuePageDrawn,
  expectOpeningLanded,
  expectRowGroups,
  expectRowLayouts,
  focusThePage,
  nodeRow,
  rowGroups,
  rowLayouts,
  rowOf,
  tree,
} from "./issue-page-g";
import { focusedNodeId, focusInside, idsOf, onScreen } from "./issue-page-i";
import {
  chromeTexts,
  headLayout,
  planBody,
  planChart,
  planChartRows,
  planView,
  seedPlanMarks,
  stepLayouts,
} from "./issue-page-k";
import { expect, test } from "./test";
import { assertPageFits } from "./unclipped";

/**
 * The Plan view's rows of the new issue page, read on the real stack. Each row turns the new
 * page on for its reader with `phase`, seeds the plan it reads (`seedPlanOrder` and
 * `seedWholeIssue` of `e2e/issue-layout-seed.ts`, or `seedPlanMarks` of `e2e/issue-page-k.ts`),
 * opens the page with `gotoIssue`, and reads it through `e2e/issue-page.ts`,
 * `e2e/issue-page-g.ts`, `e2e/issue-page-i.ts` and `e2e/issue-page-k.ts`.
 *
 * The rows are the five of the check c-rows, under the titles it names and with its messages,
 * and with readings of their own beyond it. Each reads at 1440 by 900 px, where `phase` opens
 * the page, and at 390 by 664 px (`PHONE_K`, the iPhone 13's own viewport, where
 * `e2e/issue-page-g.ts`'s `PHONE` is 390 by 844); row (4) reads the phone alone. Each row's
 * first reading is an assertion of its own on the view, so on a build whose slot draws nothing
 * it fails there and not on its deadline.
 *
 * Rows (1), (2) and (4) read `seedPlanOrder`, whose second write deletes the gone task by its
 * block id; row (5) reads the tree's names (a block chip named by the words of its `text_tree`,
 * and a number label kept with its sentence) as the outline's chip draws them; and row (2)
 * reads at 390 px that a card the keys move to stands clear of what stays at the top of the
 * screen, whose height the page publishes for the rows to be placed below
 * (`--issue-identity-height`).
 */

/** The phone every row reads: 390 px wide, and as tall as the iPhone 13's viewport. */
const PHONE_K = { height: 664, width: 390 } as const;

/** The page's colour tokens as the browser computes them. */
const COLOURS = {
  /** `page-ink`, the focused card's ring. */
  ink: "rgb(28, 25, 23)",
  /** `page-quiet`, the curves of a row that is not focused. */
  quiet: "rgb(120, 113, 108)",
  /** `page-step`: a bar's border, the focused row's rule, a card's left edge. */
  step: "rgb(180, 83, 9)",
  /** `page-step-tint`, a bar and the focused row at 761 px and above. */
  stepTint: "rgb(255, 251, 235)",
} as const;

/** The view's own address, and the address beside it. */
function planPath(key: string, rest = ""): string {
  return `${presetPath(key, "plan")}${rest}`;
}

/** The slug of the issue's own spec, which is how its blocks are addressed. */
async function primarySlug(key: string): Promise<string> {
  const primary = ((await getIssue(key)).artifacts ?? []).find((artifact) => artifact.primary);
  expect(primary, `${key} carries no primary document`).toBeDefined();
  return primary?.slug ?? "";
}

/** `seedPlanOrder`'s places by node id: its four steps, the chart's rows (the cycle as
 *  `"cycle"`) and the steps' rows, each in the order the page draws them. Each task stands after
 *  every task in its needs, which is not the order the plan is written in: the document holds
 *  `T.first`, `T.second`, `T.third`, and the first needs the second and the second the third.
 *  The two that need each other stand where the order puts them; the task whose need names no
 *  task the plan holds waits on nothing. */
async function planOrderPlaces(seeded: PlanOrderSeed) {
  const planSlug = (await getArtifact(seeded.planArtifactId)).slug;
  const step = (blockId: string) => blockNodeId(planSlug, blockId);
  const T = PLAN_ORDER_TASKS;
  return {
    bars: [step(T.third), step(T.second), step(T.first), step(T.loose)],
    charted: [step(T.third), step(T.second), step(T.first), "cycle", step(T.loose)],
    step,
    stepped: [T.third, T.second, T.first, T.knotA, T.knotB, T.loose].map(step),
  };
}

/** The chart's rows in order, each step's by its node id and each cycle's as `"cycle"`. */
async function chartOrder(page: Page): Promise<string[]> {
  return (await planChartRows(page)).map((row) => (row.kind === "cycle" ? "cycle" : row.id));
}

/** `locator`'s box, failing the row where it draws none. */
async function boxOf(
  locator: Locator,
  what: string
): Promise<{ height: number; width: number; x: number; y: number }> {
  const box = await locator.boundingBox();
  expect(box, `${what} draws no box`).not.toBeNull();
  return box ?? { height: 0, width: 0, x: 0, y: 0 };
}

/** The words the view never draws, since the model holds none of them: states, their legend,
 *  the longest path and points. */
const MADE_UP_WORDS = [
  "Not started",
  "In progress",
  "Done",
  "Blocked",
  "Longest path",
  "points",
] as const;

/** What the box draws: none of the made-up words, and every word the page draws of its own in
 *  its one list. */
async function expectPlainView(page: Page, where: string): Promise<void> {
  const text = await planBody(page).innerText();
  expect(
    MADE_UP_WORDS.filter((word) => text.includes(word)),
    `the view under ${where} draws what today's model does not hold`
  ).toEqual([]);
  expect(
    (await chromeTexts(page)).flatMap(wordsIn).filter((word) => !DECLARED_WORDS.includes(word)),
    `the view under ${where} draws words of its own that the page's list does not declare`
  ).toEqual([]);
}

/** Every read of a document's blocks, which the tree's tasks and statements are built from
 *  (`useIssueTree.ts`). */
const BLOCKS_READ = "**/api/v1/artifacts/*/blocks";

/** Before the reads it needs answer: opened with every document's blocks read held, `path`
 *  draws no line saying the issue has no task and no switch, since a task is a block of the
 *  plan document and the plan holds none until that read answers (`slots/view-plan.tsx`, which
 *  draws the view's tasks, its switch and its empty line once the plan's and the spec's blocks
 *  reads have answered); let go, it draws the chart. */
async function expectNothingBeforeTheBlocks(page: Page, path: string): Promise<void> {
  const held = Promise.withResolvers<void>();
  let asked = 0;
  const hold = async (route: Route) => {
    asked += 1;
    await held.promise;
    await route.continue();
  };
  await page.route(BLOCKS_READ, hold);
  try {
    await gotoIssue(page, path);
    await expect
      .poll(() => asked, { message: `${path} reads no document's blocks` })
      .toBeGreaterThan(0);
    // An absence window: a blocks read is out, so the page that sent it has drawn, and a view
    // that drew before its reads answer would draw its empty line within a frame; 250 ms is
    // many frames.
    await page.waitForTimeout(250);
    await expect(
      issuePage(page).getByText(EMPTY_STATE_LINES.plan),
      "the view says the issue has no plan tasks before its blocks are read"
    ).toHaveCount(0);
    await expect(
      issuePage(page).locator("[data-plan-switch]"),
      "the view draws its switch before its blocks are read"
    ).toHaveCount(0);
  } finally {
    held.resolve();
  }
  await expect(
    planView(page, "chart"),
    "the view does not draw the chart once its blocks are read"
  ).toHaveAttribute("aria-pressed", "true");
  await page.unroute(BLOCKS_READ, hold);
}

/** What the view waits for (`slots/view-plan.tsx`): the blocks held, not the last read's
 *  success. With the chart drawn, a version of the plan document refreshes that document's reads
 *  (`api/event-refresh.ts`), and every blocks read then fails: the plan's is asked three times,
 *  the page's two retries 500 ms apart included (`api/query-client.ts`'s `queryRetry`), and then
 *  holds its blocks as a read that failed. A move to Spec and back to Plan draws the view again,
 *  which draws the chart from the blocks held. The version is given again until its event
 *  reaches the page, since an event published before the page's feed has started reaches no
 *  page. */
async function expectChartAfterAFailedRead(page: Page, seeded: WholeIssueSeed): Promise<void> {
  let failed = 0;
  const fail = async (route: Route) => {
    if (route.request().url().includes(seeded.planArtifactId)) failed += 1;
    await route.fulfill({ status: 500 });
  };
  await page.route(BLOCKS_READ, fail);
  try {
    await expect(async () => {
      const before = failed;
      await createNamedVersion(seeded.planArtifactId, "The plan as it stands");
      await expect.poll(() => failed - before, { timeout: 5_000 }).toBeGreaterThanOrEqual(3);
    }, "a version of the plan document sends no read of its blocks again").toPass({
      timeout: 30_000,
    });
    await expect
      .poll(
        async () => {
          const at = failed;
          // An absence window longer than the wait between two retries: no read of the plan's
          // blocks is sent in it, so the last one has failed and none is still to come.
          await page.waitForTimeout(750);
          return failed - at;
        },
        { message: "the read of the plan's blocks is still being asked again" }
      )
      .toBe(0);
    await presetToggle(page, "spec").click();
    await expect(presetToggle(page, "spec")).toHaveAttribute("aria-pressed", "true");
    await presetToggle(page, "plan").click();
    await expect(
      planChart(page).locator("[data-plan-bar-row]"),
      "the Plan view drawn again after a failed read of the plan's blocks draws no bar of the blocks held"
    ).toHaveCount(seeded.planTasks.length);
  } finally {
    await page.unroute(BLOCKS_READ, fail);
  }
}

/** A first read that fails (`slots/view-plan.tsx`): with every read of the plan document's
 *  blocks refused, the page's two retries included, the page still opens on the question that
 *  waits on the reader, the spec's open question. `t p` from the conversation then opens Plan,
 *  which says in the page's words that it could not load the plan, with a Retry that holds the
 *  focus, and draws no bar and no switch. `t p` pressed from a control that holds the focus, the
 *  Spec control of the view row, puts the focus on that Retry too: the chord focuses the view's
 *  tree, and the stage the view is drawn in hands the focus on to the Retry
 *  (`web/src/features/issue-page/slots/stage.tsx`). With the read let through, Enter on that Retry
 *  asks it again, and the view draws the chart with the focus on its first row. */
async function expectRetryAfterAFailedFirstRead(page: Page, seeded: WholeIssueSeed): Promise<void> {
  let refusing = true;
  const refuse = async (route: Route) => {
    if (refusing && route.request().url().includes(seeded.planArtifactId)) {
      await route.fulfill({ status: 500 });
      return;
    }
    await route.continue();
  };
  await page.route(BLOCKS_READ, refuse);
  try {
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectIssuePageDrawn(page);
    // The page places its first focus once, when the reads its first rows come from have
    // answered: on the question that waits on the reader, whatever reads failed (`IssueTree.tsx`).
    // The focus is moved to the page only after that, or the first focus could land after the
    // move and keep the focus off the page.
    const question = blockNodeId(await primarySlug(seeded.key), seeded.openQuestion.blockId);
    await expectOpeningLanded(page);
    await expect
      .poll(() => focusedNodeId(page), {
        message: "the conversation does not open on the question that waits on the reader",
      })
      .toBe(question);
    await focusThePage(page);
    await page.keyboard.press("t");
    await page.keyboard.press("p");
    await expect(page).toHaveURL(new RegExp(`${planPath(seeded.key)}$`));
    const line = issuePage(page).locator('[data-plan-unreadable="plan"]');
    await expect(
      line,
      "the Plan view whose first read of the plan's blocks failed does not say it could not load the plan"
    ).toContainText(STATE_LINES.plan_unreadable);
    const retry = line.getByRole("button");
    await expect(retry, "the line saying the plan could not be loaded draws no Retry").toHaveCount(
      1
    );
    await expect(
      retry,
      "the Plan view with a refused read does not put the focus on its Retry"
    ).toBeFocused();
    await expect(
      planBody(page).locator("[data-plan-bar-row]"),
      "the Plan view whose first read of the plan's blocks failed draws a bar"
    ).toHaveCount(0);
    await expect(
      planBody(page).locator("[data-plan-switch]"),
      "the Plan view whose first read of the plan's blocks failed draws its switch"
    ).toHaveCount(0);
    // The Spec control pressed from the keyboard, which leaves the focus on it: a pointer's press
    // of a view's control leaves the focus where it was (`IssuePresets.tsx`).
    await presetToggle(page, "spec").focus();
    await page.keyboard.press("Enter");
    await expect(presetToggle(page, "spec")).toHaveAttribute("aria-pressed", "true");
    await expect(
      presetToggle(page, "spec"),
      "Enter on the Spec control leaves the focus off it"
    ).toBeFocused();
    await page.keyboard.press("t");
    await page.keyboard.press("p");
    await expect(page).toHaveURL(new RegExp(`${planPath(seeded.key)}$`));
    await expect(
      retry,
      "t p from the Spec control with the plan's blocks refused does not put the focus on the plan's Retry"
    ).toBeFocused();
    // The chord shows the tree it focused from its own top (`useIssueKeymap.ts`'s `showPart`),
    // and the Retry stands over the tree, so the stage, which focuses the Retry, brings it on
    // screen in the frame after (`slots/stage.tsx`'s `focusOn`).
    await expect
      .poll(() => onScreen(retry), {
        message: "t p from the Spec control leaves the plan's focused Retry off the screen",
      })
      .toBe(true);
    refusing = false;
    await page.keyboard.press("Enter");
    await expect(
      planView(page, "chart"),
      "Enter on Retry once the plan's blocks are read does not draw the chart"
    ).toHaveAttribute("aria-pressed", "true");
    const bars = planChart(page).locator("[data-plan-bar-row]");
    await expect(bars).toHaveCount(seeded.planTasks.length);
    await expect(line, "the line saying the plan could not be loaded stays").toHaveCount(0);
    const first = await bars.first().getAttribute("data-node-id");
    await expect
      .poll(() => focusedNodeId(page), {
        message:
          "Enter on Retry once the plan's blocks are read does not put the focus on the chart's first row",
      })
      .toBe(first);
  } finally {
    await page.unroute(BLOCKS_READ, refuse);
  }
}

// c-rows (1): the Plan view opens on the chart of time, and one control switches to the ordered
// steps. The order and each bar's place are the page's own, worked out from the `needs` the plan
// document holds (`web/src/features/issue-page/plan-filter/plan-order.ts`), because no read
// gives them. Not read here, since no read gives them either: each task's position and size - so
// every bar is one unit long and its row says "No size" - a parent's Plan view drawing its
// children's tasks, and a step's state. Read here too: each task is drawn once, and after every
// task it needs. Beyond the check: the head row's three columns, the box's place, the treeitem's
// name and the switch's group, a press on the edge of a statement chip, the breakpoints at 760,
// 761, 1240 and 1241 px and 1000 px, and the words filter, which keeps the whole plan's places.
// The plan is `seedPlanOrder` (e2e/issue-layout-seed.ts), whose second write deletes the gone
// task by its block id, which leaves the loose task's need pointing at no task the plan holds.
// SLOW(>=2s): a seeded plan read at seven widths, under both drawings and at a second address.
test("the Plan filter draws the tasks as ordered steps @phone", async ({ browser }) => {
  test.setTimeout(180_000);
  await phase(browser, seedPlanOrder, async (page, seeded) => {
    const { bars, charted, step, stepped } = await planOrderPlaces(seeded);
    const specSlug = await primarySlug(seeded.key);
    const T = PLAN_ORDER_TASKS;

    await gotoIssue(page, planPath(seeded.key));

    // The view opens on the chart, and its one control says so.
    await expect(planView(page, "chart"), "Plan does not open on the chart").toHaveAttribute(
      "aria-pressed",
      "true"
    );
    await expect(planView(page, "steps"), "Plan opens on the steps").toHaveAttribute(
      "aria-pressed",
      "false"
    );
    await expect(
      issuePage(page).getByRole("group", { exact: true, name: PRESET_NAMES.plan }),
      'the switch is not a group named "Plan"'
    ).toHaveCount(1);
    await expect
      .poll(() => chartOrder(page), {
        message: "the chart does not draw each step after the steps it needs",
      })
      .toEqual(charted);
    await expectIssuePageDrawn(page);
    const chart = await planChartRows(page);
    // A cycle's row carries the node id of the task it stands for, so a bar is read on a step's
    // row alone.
    const bar = (id: string) => chart.find((row) => row.kind === "bar" && row.id === id);

    // One bar per step, each drawn once, starting where every task it needs has ended.
    const ids = chart.filter((row) => row.kind === "bar").map((row) => row.id);
    expect(ids.length - new Set(ids).size, "a step is drawn more than once").toBe(0);
    expect(
      bars.map((id) => bar(id)?.start),
      "a bar does not start after the tasks it needs"
    ).toEqual([0, 1, 2, 0]);
    for (const [task, needed] of [
      [T.first, T.second],
      [T.second, T.third],
    ] as const) {
      const mine = bar(step(task))?.bar;
      const theirs = bar(step(needed))?.bar;
      expect(
        mine?.left ?? -1,
        `the bar of ${task} starts before the bar of ${needed}, which it needs, has ended`
      ).toBeGreaterThanOrEqual((theirs?.left ?? 0) + (theirs?.width ?? 1e6) - 1);
    }
    // Today's model holds no size, so each bar is one unit long and its row says so: a
    // dashed border in the step's colour around the step's plain tint, which is no hatch.
    const layouts = await stepLayouts(page);
    expect(
      layouts.map((layout) => layout.id),
      "the chart's bars are not its steps"
    ).toEqual(bars);
    const widths = layouts.map((layout) => layout.bar.width);
    expect(
      Math.max(...widths) - Math.min(...widths),
      `the bars are not one unit long each: ${widths}`
    ).toBeLessThanOrEqual(1);
    expect(Math.min(...widths), "a bar draws no box").toBeGreaterThan(20);
    for (const layout of layouts) {
      const at = `the bar of ${layout.id}`;
      expect(layout.bar.borderStyle, `${at} has no dashed border`).toBe("dashed");
      expect(layout.bar.borderColor, `${at}'s border is not the step's colour`).toBe(COLOURS.step);
      expect(layout.bar.backgroundImage, `${at} is drawn with a hatch`).toBe("none");
      expect(layout.bar.height, `${at} is not 22 px high`).toBeCloseTo(22, 0);
      expect(
        layout.row.height,
        `the row of ${layout.id} is under 54 px high`
      ).toBeGreaterThanOrEqual(54);
    }
    expect(
      bars.map((id) => bar(id)?.size),
      "a bar whose task holds no size does not say so"
    ).toEqual(bars.map(() => PLAN_WORDS.no_size));
    await expect(
      issuePage(page).getByRole("treeitem", {
        name: new RegExp(`^Task ${T.third}.*${PLAN_WORDS.no_size}`),
      }),
      `the row of ${T.third} is not named by its first line, then "No size"`
    ).toHaveCount(1);

    // Each step shows what it needs and the statements it serves, each statement a link to it;
    // the step that serves two statements is one row naming both.
    expect(bar(step(T.first))?.needs, `${T.first} does not name the step it needs`).toEqual([
      step(T.second),
    ]);
    const facts = (task: string) =>
      nodeRow(page, step(task))
        .first()
        .locator("[data-plan-facts]")
        .evaluate((line) =>
          [...line.children].map((part) => (part.textContent ?? "").replace(/\s+/g, " ").trim())
        );
    expect(await facts(T.first), `${T.first} does not name ${T.second} after "needs"`).toEqual([
      PLAN_WORDS.no_size,
      `· ${PLAN_WORDS.needs}`,
      expect.stringMatching(new RegExp(`^Task ${T.second}\\b`)),
    ]);
    expect(bar(step(T.third))?.needs, `${T.third} names a step it does not need`).toEqual([]);
    expect(bar(step(T.loose))?.needs, `${T.loose} names a step the plan does not hold`).toEqual([]);
    expect(await facts(T.loose), `${T.loose} names a task after "needs"`).toEqual([
      PLAN_WORDS.no_size,
    ]);
    // A statement chip's name comes with the batched names read, as the words of the treeitem's
    // name read above do, and the chart draws no chip that has no name yet: so the statements
    // are read again until they hold.
    const servesOf = async (task: string) =>
      (await planChartRows(page)).find((row) => row.kind === "bar" && row.id === step(task))
        ?.serves;
    await expect
      .poll(() => servesOf(T.first), {
        message: `${T.first} names other than the one statement it serves`,
      })
      .toHaveLength(1);
    await expect
      .poll(() => servesOf(T.third), {
        message: `${T.third} does not name both statements it serves`,
      })
      .toHaveLength(2);
    await expect(
      nodeRow(page, step(T.first)).locator(
        `[data-plan-serve] a[href$="#b-${requirementBlockId(1)}"]`
      ),
      `${T.first} does not link the statement it serves`
    ).toHaveCount(1);
    // The whole chip is the link to its statement, its edge included:
    // a press 2 px inside its left edge, on its padding, and one 1 px under its top edge, above
    // the words, each open the statement, where they would land on the row and give it the
    // focus. At 1280 px and up the link takes no padding of its own (`styles.css`'s
    // `.dispatch-inline-link`), so neither place is in the link's own box. Each press is read on
    // the statement's address, and the page goes back to the chart after it.
    const statement = new RegExp(`${presetPath(seeded.key, "spec")}#b-${requirementBlockId(1)}$`);
    const chip = nodeRow(page, step(T.first))
      .first()
      .locator(`[data-plan-serve]:has(a[href$="#b-${requirementBlockId(1)}"])`);
    for (const edge of ["left", "top"] as const) {
      await chip.scrollIntoViewIfNeeded();
      const edges = await boxOf(chip, `${T.first}'s statement chip`);
      await page.mouse.click(
        edge === "left" ? edges.x + 2 : edges.x + edges.width / 2,
        edge === "left" ? edges.y + edges.height / 2 : edges.y + 1
      );
      await expect(
        page,
        `a press on the ${edge} edge of ${T.first}'s statement chip does not open the statement`
      ).toHaveURL(statement);
      await page.goBack();
      await expect(planView(page, "chart")).toHaveAttribute("aria-pressed", "true");
      await expect.poll(() => chartOrder(page)).toEqual(charted);
      await expect(chip).toHaveCount(1);
    }
    // The lines between a step and the steps it needs: two on this plan.
    await expect(
      planChart(page).locator("svg[data-plan-links] path[data-plan-link]"),
      "the chart does not draw one line for each need"
    ).toHaveCount(2);

    // The two tasks that need each other are one line that lists both, each a link to its own
    // node, and neither is a bar: no order can put either after every task it needs.
    const cycle = planChart(page).locator("[data-plan-cycle]");
    await expect(cycle, "the cycle is not drawn as one line").toHaveCount(1);
    await expect(cycle).toContainText(PLAN_WORDS.cycle);
    for (const knot of [T.knotA, T.knotB]) {
      await expect(cycle, `the cycle's line does not list ${knot}`).toContainText(`Task ${knot}`);
      await expect(
        cycle.locator(`a[href$="#b-${knot}"]`),
        `the cycle's line does not link ${knot} to its own node`
      ).toHaveCount(1);
      expect(bar(step(knot)), `${knot} is drawn as a bar as well as in the cycle`).toBeUndefined();
    }

    // The head row's three columns: "Serves" over the statements, and every line of the axis
    // inside the middle one.
    const head = await headLayout(page);
    expect(head, "the chart draws no head row at 1440 px").not.toBeNull();
    expect(
      Math.abs((head?.serves ?? 0) - (head?.servesColumn ?? 1e6)),
      '"Serves" does not stand over the column of the statements'
    ).toBeLessThanOrEqual(1);
    expect(head?.axis.length ?? 0, "the head row draws no axis").toBeGreaterThan(0);
    expect(
      (head?.axis ?? []).filter(
        (line) =>
          line.left < (head?.middle.left ?? 0) - 0.5 || line.right > (head?.middle.right ?? 0) + 0.5
      ),
      "a line of the axis stands outside the head row's middle cell"
    ).toEqual([]);

    // The box: 1300 px wide, 22 px under the view row, and 120 px above the page's foot.
    const column = page.getByTestId("issue-column");
    const box = await boxOf(planBody(page), "the view's box at 1440 px");
    expect(box.width, "the view's box is not 1300 px wide at 1440 px").toBeCloseTo(1300, 0);
    expect(
      Math.abs(box.y - (await boxOf(column, "the issue's column")).y - 22),
      "the view's box is not 22 px under the view row at 1440 px"
    ).toBeLessThanOrEqual(1);
    await expect(planBody(page), "the view's box does not keep 120 px under the chart").toHaveCSS(
      "margin-bottom",
      "120px"
    );
    await expectPlainView(page, "the chart");

    // The columns from 1240 px down, and the prototype's from 1241 px up. Each statement
    // chip's border holds its 18 px lines alone at each width: below 1280 px a link takes 6 px
    // of padding above and below (`styles.css`'s `.dispatch-inline-link`), which the chip's
    // link takes inside the chip's line, so a linked chip is as tall as an unlinked one.
    for (const [width, first, third] of [
      [1000, 260, 220],
      [1240, 260, 220],
      [1241, 330, 320],
    ] as const) {
      await page.setViewportSize({ height: 900, width });
      await expect
        .poll(
          async () => {
            const columns = (await stepLayouts(page))[0]?.columns;
            return [Math.round(columns?.first ?? 0), Math.round(columns?.third ?? 0)];
          },
          { message: `at ${width} px a step's outer columns are not ${first} px and ${third} px` }
        )
        .toEqual([first, third]);
      if (width <= 1240) await assertPageFits(page);
      const chips = (await stepLayouts(page)).flatMap((layout) => layout.chips);
      expect(chips.length, `at ${width} px the chart draws no statement chip`).toBeGreaterThan(0);
      expect(
        chips
          .map((chip) => chip.height - 2)
          .filter((lines) => Math.abs(lines - 18 * Math.max(1, Math.round(lines / 18))) > 0.5),
        `at ${width} px a statement chip's border is taller than its lines`
      ).toEqual([]);
    }
    // At 760 px and below each step is a card, and the chart has no head row; at 761 px it has.
    const headRow = planChart(page).locator("[data-plan-head]");
    await page.setViewportSize({ height: 900, width: 760 });
    await expect(headRow, "the chart draws its head row at 760 px").toBeHidden();
    await expect
      .poll(
        async () =>
          (await stepLayouts(page)).map((layout) => [
            layout.row.borderLeftWidth,
            layout.row.borderLeftColor,
          ]),
        { message: "a step at 760 px is not a card with a 4 px left edge of the step's colour" }
      )
      .toEqual(bars.map(() => ["4px", COLOURS.step]));
    await page.setViewportSize({ height: 900, width: 761 });
    await expect(headRow, "the chart draws no head row at 761 px").toBeVisible();
    await page.setViewportSize(DESKTOP);

    // The control switches to the ordered steps, which are the outline's own rows in the same
    // order, with the cycle's tasks as rows where the order puts the cycle.
    await planView(page, "steps").click();
    await expect(planView(page, "steps")).toHaveAttribute("aria-pressed", "true");
    await expectIssuePageDrawn(page);
    await expect
      .poll(async () => idsOf(await drawnRows(page)), {
        message: "the steps are not drawn after the steps they need",
      })
      .toEqual(stepped);
    const rows = await drawnRows(page);
    const first = rowOf(rows, step(T.first));
    expect(first?.chips.join(" "), `${T.first} does not say what it needs`).toContain(
      BLOCK_ATTRIBUTE_LABELS.needs
    );
    expect(first?.chips.join(" "), `${T.first} does not name the task it needs`).toContain(
      `Task ${T.second}`
    );
    expect(first?.parent?.id, `${T.first} does not name the statement it serves`).toBe(
      blockNodeId(specSlug, requirementBlockId(1))
    );
    expect(first?.parent?.type).toBe("requirement");
    const cycleLine = planBody(page).locator("[data-plan-cycle]");
    await expect(cycleLine, "the steps do not draw the cycle's one line").toHaveCount(1);
    const line = await boxOf(cycleLine, "the cycle's line over the steps");
    expect(
      line.y + line.height,
      "the cycle's line does not stand above the steps"
    ).toBeLessThanOrEqual(
      (await boxOf(tree(page).locator("[data-node-row]").first(), "the first step")).y + 1
    );
    await expect(planBody(page), "the view's box does not keep 120 px under the steps").toHaveCSS(
      "margin-bottom",
      "120px"
    );
    await expectPlainView(page, "the steps");

    // The same order on a phone, in both views, where the page fits in one direction, in
    // a box 10 px under the view row and 10 px in from each side of the column.
    await page.setViewportSize(PHONE_K);
    await expectIssuePageDrawn(page);
    await expect
      .poll(async () => idsOf(await drawnRows(page)), {
        message: "the steps' order is not the same at 390 px",
      })
      .toEqual(stepped);
    const phoneBox = await boxOf(planBody(page), "the view's box at 390 px");
    expect(
      Math.abs(phoneBox.y - (await boxOf(column, "the issue's column")).y - 10),
      "the view's box is not 10 px under the view row at 390 px"
    ).toBeLessThanOrEqual(1);
    expect(
      Math.abs(phoneBox.width - 342),
      "the view's box is not 342 px wide at 390 px"
    ).toBeLessThanOrEqual(1);
    await assertPageFits(page);
    await planView(page, "chart").click();
    await expect
      .poll(() => chartOrder(page), { message: "the chart's order is not the same at 390 px" })
      .toEqual(charted);
    expect(
      (await planChartRows(page)).filter((row) => row.kind === "bar").map((row) => row.size),
      "a bar does not say it holds no size at 390 px"
    ).toEqual(bars.map(() => PLAN_WORDS.no_size));
    await assertPageFits(page);

    // The words filter chooses which places are drawn, and every place is the whole plan's:
    // `T.first` alone, its bar where it stands on the whole plan, naming the task it needs.
    await page.setViewportSize(DESKTOP);
    await gotoIssue(page, planPath(seeded.key, "?q=Task%20t1"));
    await expect
      .poll(
        async () =>
          (await planChartRows(page)).map((row) => ({
            id: row.id,
            needs: row.needs,
            start: row.start,
          })),
        { message: `?q=Task t1 does not draw ${T.first} alone, where the whole plan puts it` }
      )
      .toEqual([{ id: step(T.first), needs: [step(T.second)], start: 2 }]);
  });
});

// c-rows (2): the chart is one tab stop of the page, and the outline's own keys move through its
// steps in their order, passing over a cycle's row. Beyond the check: an address naming a task of
// a cycle puts the focus on the cycle's row, an address naming a node that is no task draws the
// plan as /plan does, Escape gives a row back from its link, and a press of the switch under an
// address leaves the focus on the button pressed.
// The plan is `seedPlanOrder`, as row (1) reads it. At 390 by 664 px, after End and after Home,
// the focused card stands on screen, clear of what stays at the top, whose height the page
// publishes (`--issue-identity-height`).
// SLOW(>=2s): a seeded plan walked by the keyboard at two widths and at five addresses.
test("the Plan view's chart is reached in order with the keyboard @phone", async ({ browser }) => {
  test.setTimeout(180_000);
  await phase(browser, seedPlanOrder, async (page, seeded) => {
    const { charted, step, stepped } = await planOrderPlaces(seeded);
    const specSlug = await primarySlug(seeded.key);
    const T = PLAN_ORDER_TASKS;
    const reach = async (key: string, id: string, by = "the arrow") => {
      await page.keyboard.press(key);
      await expect
        .poll(() => focusedNodeId(page), { message: `${by} did not reach ${id}` })
        .toBe(id);
    };

    // `t p` turns Plan on from the conversation, and puts the focus on the first step's row. The
    // page's opening puts the focus on a row first, and the focus leaves for the page after it.
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectIssuePageDrawn(page);
    await expectOpeningLanded(page);
    await focusThePage(page);
    await page.keyboard.press("t");
    await page.keyboard.press("p");
    await expect(planView(page, "chart"), "t p does not open Plan on the chart").toHaveAttribute(
      "aria-pressed",
      "true"
    );
    await expect(page).toHaveURL(new RegExp(`${planPath(seeded.key)}$`));
    await expect
      .poll(() => focusedNodeId(page), { message: `t p does not put the focus on ${T.third}` })
      .toBe(step(T.third));
    // The row places itself once it holds the focus (`useRowOnScreen`), so it is read until it
    // stands on screen.
    await expect
      .poll(() => onScreen(nodeRow(page, step(T.third)).first()), {
        message: `t p leaves ${T.third}'s row off screen`,
      })
      .toBe(true);

    // The arrows reach the bars in order, past the cycle's row, and so do the outline's keys.
    for (const next of [T.second, T.first, T.loose]) await reach("ArrowDown", step(next));
    await reach("k", step(T.first), "k");
    await reach("Home", step(T.third), "Home");
    await reach("j", step(T.second), "j");
    await reach("End", step(T.loose), "End");
    await reach("k", step(T.first), "k");

    // The focused row's mark is a rule, not its colour alone, and its curves are heavier and in
    // the step's colour; the others' are the quiet grey. The row's shadow is read by the layers
    // it draws: Tailwind writes its empty ring layers, transparent, before the row's own.
    await expect
      .poll(
        () =>
          nodeRow(page, step(T.first))
            .first()
            .evaluate((row) =>
              getComputedStyle(row)
                .boxShadow.split(/,(?![^(]*\))/)
                .map((layer) => layer.trim())
                .filter((layer) => !layer.startsWith("rgba(0, 0, 0, 0)"))
            ),
        { message: `${T.first}'s focused row draws no 3 px rule of the step's colour` }
      )
      .toEqual([`${COLOURS.step} 3px 0px 0px 0px inset`]);
    const curve = (from: string, to: string) =>
      planChart(page).locator(
        `svg[data-plan-links] path[data-plan-link="${step(from)} ${step(to)}"]`
      );
    await expect(
      curve(T.second, T.first),
      `the curve from ${T.second} to the focused ${T.first} is not 2 px wide`
    ).toHaveCSS("stroke-width", "2px");
    await expect(
      curve(T.second, T.first),
      `the curve from ${T.second} to the focused ${T.first} is not the step's colour`
    ).toHaveCSS("stroke", COLOURS.step);
    await expect(
      curve(T.third, T.second),
      `the curve from ${T.third} to ${T.second} is not 1.4 px wide`
    ).toHaveCSS("stroke-width", "1.4px");
    await expect(
      curve(T.third, T.second),
      `the curve from ${T.third} to ${T.second} is not the quiet grey`
    ).toHaveCSS("stroke", COLOURS.quiet);

    // `f` reaches the row's statement link, Escape gives the row back, and one Tab from the row
    // leaves the chart, which is one tab stop.
    await page.keyboard.press("f");
    await expect(
      nodeRow(page, step(T.first)).locator(
        `[data-plan-serve] a[href$="#b-${requirementBlockId(1)}"]`
      ),
      `f does not reach ${T.first}'s statement link`
    ).toBeFocused();
    await page.keyboard.press("Escape");
    await expect(
      nodeRow(page, step(T.first)).first(),
      `Escape does not give ${T.first}'s row back from its link`
    ).toBeFocused();
    await page.keyboard.press("Tab");
    await expect
      .poll(() => focusInside(planChart(page)), {
        message: "one Tab from a row stays in the chart",
      })
      .toBe(false);

    // Enter on a step goes to its task in the conversation.
    await nodeRow(page, step(T.loose)).first().focus();
    await page.keyboard.press("Enter");
    await expect(page, `Enter on ${T.loose} does not go to it in the conversation`).toHaveURL(
      new RegExp(`/issues/${seeded.key}#b-${T.loose}$`)
    );

    // Back on /plan, Tab goes from "Chart" to "Steps", then into the chart at its first step;
    // Enter and Space press the two buttons.
    await gotoIssue(page, planPath(seeded.key));
    await expect(planView(page, "chart")).toHaveAttribute("aria-pressed", "true");
    await expect.poll(() => chartOrder(page)).toEqual(charted);
    await planView(page, "chart").focus();
    await page.keyboard.press("Tab");
    await expect(planView(page, "steps"), 'Tab from "Chart" does not reach "Steps"').toBeFocused();
    await page.keyboard.press("Tab");
    await expect(
      planChart(page).locator('[data-node-row][tabindex="0"]'),
      'Tab from "Steps" does not reach the chart'
    ).toBeFocused();
    expect(await focusedNodeId(page), "the chart's tab stop is not its first step").toBe(
      step(T.third)
    );
    await planView(page, "steps").focus();
    await page.keyboard.press("Enter");
    await expect(planView(page, "steps"), 'Enter on "Steps" does not press it').toHaveAttribute(
      "aria-pressed",
      "true"
    );
    await expect
      .poll(async () => idsOf(await drawnRows(page)), {
        message: 'Enter on "Steps" does not draw the steps',
      })
      .toEqual(stepped);
    await planView(page, "chart").focus();
    await page.keyboard.press("Space");
    await expect(planView(page, "chart"), 'Space on "Chart" does not press it').toHaveAttribute(
      "aria-pressed",
      "true"
    );
    await expect
      .poll(() => chartOrder(page), { message: 'Space on "Chart" does not draw the bars' })
      .toEqual(charted);

    // Under an address that names a step, a press of the switch is the reader taking over: the
    // focus stays on the button pressed.
    await gotoIssue(page, planPath(seeded.key, `#b-${T.first}`));
    await expectOpeningLanded(page);
    await expect
      .poll(() => focusedNodeId(page), { message: `#b-${T.first} does not focus its row` })
      .toBe(step(T.first));
    await planView(page, "steps").focus();
    await page.keyboard.press("Enter");
    await expect
      .poll(async () => idsOf(await drawnRows(page)), {
        message: `Enter on "Steps" under #b-${T.first} does not draw the steps`,
      })
      .toEqual(stepped);
    await expect(
      planView(page, "steps"),
      `Enter on "Steps" under #b-${T.first} moves the focus off it`
    ).toBeFocused();
    await planView(page, "chart").focus();
    await page.keyboard.press("Space");
    await expect
      .poll(() => chartOrder(page), {
        message: `Space on "Chart" under #b-${T.first} does not draw the bars`,
      })
      .toEqual(charted);
    await expect(
      planView(page, "chart"),
      `Space on "Chart" under #b-${T.first} moves the focus off it`
    ).toBeFocused();

    // An address naming a task of a cycle puts the focus on the cycle's row, which stands for
    // that task, and the arrow goes on to the step after it.
    await gotoIssue(page, planPath(seeded.key, `#b-${T.knotA}`));
    await expectOpeningLanded(page);
    await expect(
      planChart(page).locator("[data-plan-cycle-row]"),
      `#b-${T.knotA} does not put the focus on the cycle's row`
    ).toBeFocused();
    await expect
      .poll(() => focusedNodeId(page), {
        message: `the cycle's row does not stand for ${T.knotA}, which the address names`,
      })
      .toBe(step(T.knotA));
    await reach("ArrowDown", step(T.loose));

    // An address naming a node that is no task draws the plan as /plan does: the statement's
    // address draws no row of it, and a comment's no bar of it.
    const statement = blockNodeId(specSlug, requirementBlockId(1));
    await gotoIssue(page, planPath(seeded.key, `#b-${requirementBlockId(1)}`));
    await expect
      .poll(() => chartOrder(page), {
        message: `#b-${requirementBlockId(1)} does not draw the bars of /plan`,
      })
      .toEqual(charted);
    await expect(
      nodeRow(page, statement),
      `#b-${requirementBlockId(1)} draws a row of the statement it names`
    ).toHaveCount(0);
    const comment = await createComment(seeded.key, { body: "The plan reads in its order." });
    await gotoIssue(page, planPath(seeded.key, `?comment=${comment.id}`));
    await expect
      .poll(() => chartOrder(page), {
        message: "?comment= does not draw the bars of /plan",
      })
      .toEqual(charted);
    await expect(
      planBody(page).locator('[data-node-row][data-node-type="comment"]'),
      "?comment= draws the comment it names as a bar"
    ).toHaveCount(0);

    // On a phone the down arrow moves from card to card in the same order.
    await page.setViewportSize(PHONE_K);
    await gotoIssue(page, planPath(seeded.key));
    await expect.poll(() => chartOrder(page)).toEqual(charted);
    await nodeRow(page, step(T.third)).first().focus();
    await expect.poll(() => focusedNodeId(page)).toBe(step(T.third));
    for (const next of [T.second, T.first, T.loose]) await reach("ArrowDown", step(next));
    // After End and after Home the focused card stands on screen, clear of the view row that
    // stays at the top at 760 px and below (`onScreen`): the rows' own hook places it below the
    // height the page publishes for that band (`useRowOnScreen`, `bringOnScreen`,
    // `--issue-identity-height`). Each key is pressed with the page put at its far end from the
    // card the key goes to, so that card is off the screen until its placing puts it on.
    const first = nodeRow(page, step(T.third)).first();
    const last = nodeRow(page, step(T.loose)).first();
    /** The card that holds the focus, itself or a link inside it. */
    const focusedCard = planChart(page).locator("[data-node-row]:focus-within");
    /** Puts the page at its `end`, from where `card` stands off the screen (`before` fails the
     *  row where it does not), runs `press`, which presses a key and reads where it left the
     *  focus, and reads until `shown` - the card, or the link the key focused - stands on screen
     *  (`after` fails the row where it never does). */
    const backOnScreen = async (
      press: () => Promise<void>,
      card: Locator,
      end: "top" | "bottom",
      before: string,
      after: string,
      shown: Locator = card
    ): Promise<void> => {
      // The key before this one moved the focus, and the card it focused places itself on screen
      // after the focus lands (`useRowOnScreen`: in its layout effect, and in its resize
      // observer's first callback a frame later). The page is scrolled once that placing has
      // settled - the card on screen and the scroll position still over two consecutive animation
      // frames - or the placing undoes the scroll. A resize observer reports in the rendering
      // step after the call that observes, which runs after the first of the two frame callbacks,
      // so a scroll it makes there is read by the second.
      await expect
        .poll(
          async () => [
            await onScreen(focusedCard),
            await page.evaluate(() => {
              const { promise, resolve } = Promise.withResolvers<boolean>();
              const start = window.scrollY;
              requestAnimationFrame(() => {
                const next = window.scrollY;
                requestAnimationFrame(() => resolve(start === next && next === window.scrollY));
              });
              return promise;
            }),
          ],
          {
            message: "the focused card's placing does not settle before the page is scrolled",
            timeout: 10_000,
          }
        )
        .toEqual([true, true]);
      await page.evaluate(
        (bottom) =>
          window.scrollTo({
            behavior: "instant",
            top: bottom ? document.documentElement.scrollHeight : 0,
          }),
        end === "bottom"
      );
      expect(await onScreen(card), before).toBe(false);
      await press();
      await expect.poll(() => onScreen(shown), { message: after }).toBe(true);
    };
    /** Whether the focus is on a link inside `card`, and not on the card itself. */
    const onALinkOf = (card: Locator): Promise<boolean> =>
      card.evaluate(
        (element) =>
          element.contains(document.activeElement) &&
          document.activeElement !== element &&
          document.activeElement?.tagName === "A"
      );
    await backOnScreen(
      () => reach("Home", step(T.third), "Home"),
      first,
      "bottom",
      `${T.third}'s card is on screen before Home`,
      "Home leaves the focused card under what stays at the top at 390 px"
    );
    await backOnScreen(
      () => reach("End", step(T.loose), "End"),
      last,
      "top",
      `${T.loose}'s card is on screen before End`,
      "End leaves the focused card off the screen at 390 px"
    );
    // A move key that lands on the card already holding the focus brings it back on screen, as
    // the outline's own move keys do (`useTreeKeymap.ts`'s `moveToRow`): End on the last card and
    // Home on the first, each after the reader scrolled that card off the screen, move no focus,
    // so the card's own placing never runs unless the key brings the card back itself. Under
    // "Steps" the outline's rows come back, and the chart's cards must too.
    await backOnScreen(
      () => reach("End", step(T.loose), "End again"),
      last,
      "top",
      `${T.loose}'s card is on screen before End again`,
      "End on the focused last card leaves it off the screen at 390 px"
    );
    await reach("Home", step(T.third), "Home");
    await backOnScreen(
      () => reach("Home", step(T.third), "Home again"),
      first,
      "bottom",
      `${T.third}'s card is on screen before Home again`,
      "Home on the focused first card leaves it off the screen at 390 px"
    );
    // The arrows at the ends of the chart are move keys too: the outline's down arrow on its last
    // row and its up arrow on its first land on the row they are on and bring it back on screen.
    // The chart's must do the same with a card the reader scrolled off the screen.
    await backOnScreen(
      () => reach("ArrowUp", step(T.third), "ArrowUp on the first card"),
      first,
      "bottom",
      `${T.third}'s card is on screen before ArrowUp`,
      "ArrowUp on the focused first card leaves it off the screen at 390 px"
    );
    await reach("End", step(T.loose), "End");
    await backOnScreen(
      () => reach("ArrowDown", step(T.loose), "ArrowDown on the last card"),
      last,
      "top",
      `${T.loose}'s card is on screen before ArrowDown`,
      "ArrowDown on the focused last card leaves it off the screen at 390 px"
    );
    // A move key pressed from a link of the card works on the card: Home from the first card's
    // statement link focuses that card and brings it back on screen, as it does under "Steps".
    await reach("Home", step(T.third), "Home");
    await page.keyboard.press("f");
    await expect
      .poll(() => onALinkOf(first), { message: `f does not reach a link of ${T.third}'s card` })
      .toBe(true);
    await backOnScreen(
      async () => {
        await reach("Home", step(T.third), "Home from the link");
        await expect(first, `Home from ${T.third}'s link does not focus its card`).toBeFocused();
      },
      first,
      "bottom",
      `${T.third}'s card is on screen before Home from its link`,
      "Home from a link of the focused first card leaves it off the screen at 390 px"
    );
    // Escape from the link gives the card back, and the card it gives back stands on screen, as
    // the outline's row does under "Steps".
    await page.keyboard.press("f");
    await expect
      .poll(() => onALinkOf(first), {
        message: `f does not reach a link of ${T.third}'s card again`,
      })
      .toBe(true);
    await backOnScreen(
      async () => {
        await page.keyboard.press("Escape");
        await expect(
          first,
          `Escape from ${T.third}'s link does not give its card back`
        ).toBeFocused();
      },
      first,
      "bottom",
      `${T.third}'s card is on screen before Escape`,
      "Escape from a link of the first card leaves the card off the screen at 390 px"
    );
    // f is a move key as well: from a card the reader scrolled off the screen it puts the focus
    // on the card's first link and brings that link on screen, as the outline's f does.
    const link = first.locator("a[href]").first();
    await backOnScreen(
      async () => {
        await page.keyboard.press("f");
        await expect(
          link,
          `f does not reach a link of ${T.third}'s card from off the screen`
        ).toBeFocused();
      },
      first,
      "bottom",
      `${T.third}'s card is on screen before f`,
      "f on the focused first card leaves its link off the screen at 390 px",
      link
    );
  });
});

// c-rows (3), read on Plan: the steps are the outline's own rows, so the outline's readers of
// every row (`e2e/issue-page-g.ts`) hold on them, and the chart names each tie by words and never
// by an address or a slug. Beyond the check: at 1440 px, with the blocks reads held, the view
// draws no line saying the issue has no task and no switch, and draws the chart once they are let
// go (`expectNothingBeforeTheBlocks`); a first read of the plan's blocks that fails draws the
// page's line for it with a Retry, which `t p` leaves the focus on, from the page itself and from
// the Spec control the reader pressed, and no bar and no switch, and Enter on the Retry draws the
// chart with the focus on its first row once the read is let through
// (`expectRetryAfterAFailedFirstRead`); and a read of the plan's blocks that fails once the chart
// is drawn leaves the chart drawn on a return to Plan (`expectChartAfterAFailedRead`).
// SLOW(>=2s): the whole seeded issue, loaded and read under both drawings at two widths.
test("the Plan view's steps are drawn as every row of the outline @phone", async ({ browser }) => {
  test.setTimeout(180_000);
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    const addresses = new RegExp(`dispatch://|${seeded.key} \\S+( block)?$|-md\\b`, "u");
    const specSlug = await primarySlug(seeded.key);
    for (const size of [DESKTOP, PHONE_K]) {
      const where = `Plan at ${size.width} px`;
      await page.setViewportSize(size);
      await gotoIssue(page, planPath(seeded.key));
      await expect(planView(page, "chart"), `${where} does not open on the chart`).toHaveAttribute(
        "aria-pressed",
        "true"
      );
      if (size === DESKTOP) {
        await expectNothingBeforeTheBlocks(page, planPath(seeded.key));
        await expectRetryAfterAFailedFirstRead(page, seeded);
      }
      await expect(
        planChart(page).locator("[data-plan-bar-row]"),
        `${where} does not draw a bar for each task of the plan`
      ).toHaveCount(seeded.planTasks.length);
      // The chart's words are the batched names read's, which is sent once every other read of
      // the tree has answered (`useIssueTree.ts`): until it lands a bar reads its kind's word and
      // the chart draws no need, since a needs chip holds no name until then. So the texts are
      // read once a need is drawn (the seed's later tasks need the earlier ones), and read again
      // until they hold.
      await expect(
        planChart(page).locator("[data-plan-need]").first(),
        `the chart on ${where} names no need by its words`
      ).toHaveText(/\S/);
      await expect
        .poll(
          async () =>
            (
              await planChart(page).evaluate((chart) =>
                [chart, ...chart.querySelectorAll("*")]
                  .map((element) => (element.textContent ?? "").replace(/\s+/g, " ").trim())
                  .filter((text) => text !== "")
              )
            ).filter((text) => addresses.test(text)),
          { message: `the chart on ${where} draws an address or a slug` }
        )
        .toEqual([]);

      await planView(page, "steps").click();
      await expect(planView(page, "steps")).toHaveAttribute("aria-pressed", "true");
      await expect(
        planBody(page).locator('[data-node-row][data-node-type="plan_task"]'),
        `the steps on ${where} do not draw a row for each task of the plan`
      ).toHaveCount(seeded.planTasks.length);
      expectRowLayouts(await rowLayouts(page, specSlug), where, false, size.width);
      expectRowGroups(await rowGroups(page), where, size.width, addresses);
      if (size === DESKTOP) await expectChartAfterAFailedRead(page, seeded);
    }
  });
});

// c-rows (4): at 760 px and below the chart is one card for each step, in the same order, with
// no head row and no curve, and the page does not scroll sideways from 320 px. "No size" is read
// on the card's line under the track, with the needs and the statements: `T.third`'s line wraps,
// so it is not the card's last line of text.
// The plan is `seedPlanOrder`, as row (1) reads it.
// SLOW(>=2s): a seeded plan read at two phone widths, with a key pressed between.
test("on a phone the Plan chart is one card for each step @phone @phone-only", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  await phase(browser, seedPlanOrder, async (page, seeded) => {
    const { bars, charted, step } = await planOrderPlaces(seeded);
    const T = PLAN_ORDER_TASKS;
    await page.setViewportSize(PHONE_K);
    await gotoIssue(page, planPath(seeded.key));

    await expect(
      planChart(page).locator("[data-plan-bar-row]"),
      "the chart does not draw one card for each of its four steps"
    ).toHaveCount(bars.length);
    await expect
      .poll(() => chartOrder(page), {
        message: "the cards are not t3, t2, t1 and t6, with the cycle's line between t1 and t6",
      })
      .toEqual(charted);
    await expect(
      planChart(page).locator("[data-plan-head]"),
      "the chart draws its head row at 390 px"
    ).toBeHidden();
    await expect(
      planChart(page).locator("svg[data-plan-links] path:visible"),
      "the chart draws a curve at 390 px"
    ).toHaveCount(0);

    // The cards are settled here: their count and their order are each awaited just above.
    for (const card of await stepLayouts(page)) {
      const at = `the card of ${card.id}`;
      expect(card.row.borderLeftWidth, `${at} has no 4 px left edge`).toBe("4px");
      expect(card.row.borderLeftColor, `${at}'s left edge is not the step's colour`).toBe(
        COLOURS.step
      );
      expect(card.track.height, `${at}'s track is not 10 px high`).toBeCloseTo(10, 0);
      expect(
        card.bar.left >= card.track.left - 0.5 &&
          card.bar.right <= card.track.right + 0.5 &&
          card.bar.top >= card.track.top - 0.5 &&
          card.bar.bottom <= card.track.bottom + 0.5,
        `${at}'s track does not hold its bar`
      ).toBe(true);
      expect(card.line.noSize, `${at} does not say "No size"`).not.toBeNull();
      expect(
        [card.line.noSize ?? -1, ...card.line.needs, ...card.line.serves].filter(
          (top) => top < card.track.bottom
        ),
        `${at} draws "No size", a need or a statement above the bottom of its track`
      ).toEqual([]);
    }

    // The focused card is marked by a ring of ink, not by the step's tint.
    await nodeRow(page, step(T.third)).first().focus();
    await expect.poll(() => focusedNodeId(page)).toBe(step(T.third));
    await page.keyboard.press("ArrowDown");
    await expect
      .poll(() => focusedNodeId(page), { message: `the arrow did not reach ${step(T.second)}` })
      .toBe(step(T.second));
    await expect
      .poll(
        async () =>
          (await stepLayouts(page)).find((card) => card.id === step(T.second))?.row.boxShadow ?? "",
        { message: `${T.second}'s focused card draws no ring of ink` }
      )
      .toContain(COLOURS.ink);
    expect(
      (await stepLayouts(page)).find((card) => card.id === step(T.second))?.row.backgroundColor,
      `${T.second}'s focused card is marked by the step's tint`
    ).not.toBe(COLOURS.stepTint);
    await assertPageFits(page);

    await page.setViewportSize({ height: PHONE_K.height, width: 320 });
    await assertPageFits(page);
  });
});

// c-rows (5): a need is named by the needed task's words, as a reader reads them, with none of
// the marks its markdown is written in, in the chart and in the steps. Beyond the check: the
// chart's first reading is that each step's row names its step by its words past the number they
// start with, and a statement chip draws no `*` or `\` either.
// The names are the tree's: a task's first line is the first line of its first block's words,
// its number label kept with its sentence, so each chart row reads past `1.` and `2.`; a task's
// `needs` chip is named by that line of the needed task's `text_tree`, so a need holds none of
// its markdown. The outline's chip draws `chipWords(chip.name)` with no markdown pass
// (`NodeRow.tsx`'s `Chip`), so the steps' needs chip draws the same words.
// SLOW(>=2s): a seeded plan read under both drawings at two widths.
test("the Plan view names a needed task by its words, with no mark of their source @phone", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  await phase(browser, seedPlanMarks, async (page, seeded) => {
    const planSlug = (await getArtifact(seeded.planArtifactId)).slug;
    const first = blockNodeId(planSlug, "t1");
    const second = blockNodeId(planSlug, "t2");
    const expectPlainNeed = (words: string) => {
      expect(
        ["*", "\\", ":::", "dispatch://"].filter((mark) => words.includes(mark)),
        "a need draws a mark of its source"
      ).toEqual([]);
      expect(words, "a need drops the number its task's words start with").toMatch(/^1\./);
      expect(words, "a need is not named by the words of the task it needs").toContain("Stand");
    };
    for (const size of [DESKTOP, PHONE_K]) {
      await page.setViewportSize(size);
      await gotoIssue(page, planPath(seeded.key));
      await expect(
        planView(page, "chart"),
        `Plan does not open on the chart at ${size.width} px`
      ).toHaveAttribute("aria-pressed", "true");
      // The chart's words of a task are the batched names read's, which lands after the blocks
      // are read (`useIssueTree.ts`): until it does, a need holds no words and a row its kind's
      // word. So the chart is read once the second task's need holds words.
      const need = nodeRow(page, second).first().locator("[data-plan-need]");
      await expect(need.first(), `t2's need draws no words at ${size.width} px`).toHaveText(/\S/);
      // Each step's row names its step by its words, and not by the number they start with
      // alone: the first task's words start `**1\. Stand the harness up.**`, the second's
      // `**2\. Read the needs back.**` (`seedPlanMarks`).
      for (const [id, words] of [
        [first, "Stand"],
        [second, "Read"],
      ] as const) {
        await expect(
          nodeRow(page, id).first().locator("[data-node-words]"),
          "a chart row names its step by its number alone"
        ).toContainText(words);
      }
      await expect(need, `t2 does not name the task it needs at ${size.width} px`).toHaveCount(1);
      await expect(need).toHaveAttribute("data-plan-need", first);
      expectPlainNeed(await need.innerText());
      const serves = nodeRow(page, second).first().locator("[data-plan-serve]");
      await expect(serves, `t2 does not name both statements it serves`).toHaveCount(2);
      expect(
        (await serves.allInnerTexts()).filter((words) =>
          ["*", "\\"].some((mark) => words.includes(mark))
        ),
        "a statement chip draws a mark of its source"
      ).toEqual([]);

      await planView(page, "steps").click();
      await expect(planView(page, "steps")).toHaveAttribute("aria-pressed", "true");
      const chips = nodeRow(page, second).first().locator("[data-node-chip]");
      await expect(chips.first(), `t2's row draws no chip at ${size.width} px`).toBeVisible();
      // The outline's chip draws its name once its own markdown reading answers
      // (`NodeRow.tsx`'s `Chip`), so the needs chip is read until it is drawn with a name.
      const needs = async () =>
        (
          await chips.evaluateAll((all) =>
            all.map((chip) => ({
              label: (chip.querySelector("[data-page-chrome]")?.textContent ?? "").trim(),
              name: (chip.querySelector("[data-chip-name]")?.textContent ?? "").trim(),
            }))
          )
        ).flatMap((chip) => (chip.label === BLOCK_ATTRIBUTE_LABELS.needs ? [chip.name] : []));
      await expect
        .poll(needs, { message: `t2's row does not draw one needs chip at ${size.width} px` })
        .toEqual([expect.stringMatching(/\S/)]);
      expectPlainNeed((await needs())[0] ?? "");
    }
  });
});
