/** the row reads each task's size and the tasks of the issue under an issue as the server's Plan
 *  read answers them from the plan documents a person uploads, with the needs and the statements
 *  its names read gives their words */

import type { Page } from "@playwright/test";

import { presetPath } from "../web/src/features/issue-page/issue-address";
import {
  BLOCK_ATTRIBUTE_LABELS,
  nodeFactLabel,
  PLAN_WORDS,
} from "../web/src/features/issue-page/page-words";
import { blockNodeId } from "../web/src/features/issue-page/tree-drafts";
import { buildIssuePath } from "../web/src/features/refs/routes";
import { getArtifact } from "./api";
import { gotoIssue, phase } from "./issue-page";
import { DESKTOP, PHONE } from "./issue-page-g";
import { planBody, planView } from "./issue-page-k";
import {
  CHILD_TASKS,
  OWN_TASKS,
  PLAN_SIZES,
  PLAN_SIZES_WORDS,
  seedPlanSizes,
} from "./issue-page-t-plan";
import { awaitIndexedCoverage, ROW_MS } from "./issue-page-t-rows";
import { heldAnswer, owedUnless, treeReadGate, viewAnswerOf } from "./issue-page-t-tree";
import { expect, test } from "./test";

/**
 * The Plan view's row of sizes and of the tasks of the issues under an issue: on the seeded
 * issue's `/plan` (`seedPlanSizes`), each step draws its size in words and its bar as long as its
 * size on one scale, a task with no size draws one line of the axis long, dashed, and says "No
 * size", and the plan holds the tasks of the issue under it after the issue's own, each in the
 * page's own walk (`plan-filter/plan-order.ts`) and marked with that issue's key, a link to it.
 * On the issue under it, its own `/plan` draws its tasks in the same order and with no mark. The
 * chart and the steps are read through the marks `plan-filter/PlanFilterBody.tsx` carries, as
 * `e2e/issue-page-k.ts` names them.
 */

/** A link to an issue's page a row draws, by the key it reads and the address it goes to. */
interface KeyLink {
  readonly href: string;
  readonly key: string;
}

/** One step of the chart as the row reads it: its bar's left edge, width and border, how far
 *  apart two lines of the axis stand in its own track (0 where the track draws no axis, at 760
 *  px and below), the issue keys it links, the tasks it names after "needs" by their words, the
 *  statements it serves and its size in words. */
interface ChartStep {
  readonly axisStep: number;
  readonly bar: { readonly borderStyle: string; readonly left: number; readonly width: number };
  readonly id: string;
  readonly keys: readonly KeyLink[];
  readonly needs: readonly { readonly id: string; readonly words: string }[];
  readonly serves: readonly string[];
  readonly size: string;
}

/** Every step of the chart, in the order it is drawn. */
function chartSteps(page: Page): Promise<ChartStep[]> {
  return page.evaluate(() => {
    const text = (element: Element | null) =>
      (element?.textContent ?? "").replace(/\s+/g, " ").trim();
    return [
      ...document.querySelectorAll("[data-issue-page] [data-plan-chart] [data-plan-bar-row]"),
    ].map((row) => {
      const bar = row.querySelector("[data-plan-bar]");
      const box = bar?.getBoundingClientRect();
      const lines = [...row.querySelectorAll("[data-plan-axis] > i")].map(
        (line) => line.getBoundingClientRect().left
      );
      return {
        axisStep: lines.length < 2 ? 0 : (lines[1] ?? 0) - (lines[0] ?? 0),
        bar: {
          borderStyle: bar === null ? "" : getComputedStyle(bar).borderTopStyle,
          left: box?.left ?? 0,
          width: box?.width ?? 0,
        },
        id: row.getAttribute("data-node-id") ?? "",
        keys: [...row.querySelectorAll("a[data-child-key]")].map((link) => ({
          href: link.getAttribute("href") ?? "",
          key: text(link),
        })),
        needs: [...row.querySelectorAll("[data-plan-need]")].map((need) => ({
          id: need.getAttribute("data-plan-need") ?? "",
          words: text(need),
        })),
        serves: [...row.querySelectorAll("[data-plan-serve]")].map(text),
        size: text(row.querySelector("[data-plan-size], [data-plan-no-size]")),
      };
    });
  });
}

/** One step under Steps as the row reads it: the facts its body draws whose label is one of the
 *  labels asked, each as its label and its value, the issue keys it links, the names of its
 *  `needs` chips, and how many times its words say "No size". */
interface StepRow {
  readonly facts: readonly (readonly string[])[];
  readonly id: string;
  readonly keys: readonly KeyLink[];
  readonly needs: readonly string[];
  readonly noSize: number;
}

/** The fact labels a step's size and its issue are drawn under. */
const STEP_FACT_LABELS = { issue: nodeFactLabel("issue"), size: nodeFactLabel("size") } as const;

/** Every step under Steps, in the order it is drawn. */
function stepRows(page: Page): Promise<StepRow[]> {
  return page.evaluate(
    ({ labels, needsLabel, noSize }) => {
      const text = (element: Element | null) =>
        (element?.textContent ?? "").replace(/\s+/g, " ").trim();
      return [
        ...document.querySelectorAll(
          '[data-issue-page] [data-plan-body] [data-node-row][data-node-type="plan_task"]'
        ),
      ].map((row) => ({
        facts: [...row.querySelectorAll("[data-node-body] dt")]
          .map((label) => [text(label), text(label.nextElementSibling)])
          .filter(([label]) => labels.includes(label ?? "")),
        id: row.getAttribute("data-node-id") ?? "",
        keys: [...row.querySelectorAll("a[data-child-key]")].map((link) => ({
          href: link.getAttribute("href") ?? "",
          key: text(link),
        })),
        needs: [...row.querySelectorAll("[data-node-chip]")].flatMap((chip) =>
          text(chip.querySelector("[data-page-chrome]")) === needsLabel
            ? [text(chip.querySelector("[data-chip-name]"))]
            : []
        ),
        noSize: (row.textContent ?? "").split(noSize).length - 1,
      }));
    },
    {
      labels: [STEP_FACT_LABELS.issue, STEP_FACT_LABELS.size],
      needsLabel: BLOCK_ATTRIBUTE_LABELS.needs,
      noSize: PLAN_WORDS.no_size,
    }
  );
}

/** The words each sized task's size is drawn in. */
const SIZE_WORDS = {
  a: "45 min",
  b: "1 h 30 min",
  childFirst: "2 h",
  childSecond: "30 min",
} as const;

/** How far apart, in px, two edges that stand at one place may be drawn. */
const PX = 2;

// SLOW(>=2s): two issues and their plans seeded, each issue's Plan view loaded at two widths and
// read under both drawings, with the names read waited out on each.
test("Plan draws each task's size, and a parent's plan holds the steps of the issue under it in the page's own order @phone", async ({
  browser,
}) => {
  test.setTimeout(ROW_MS);
  await phase(browser, seedPlanSizes, async (page, seeded) => {
    owedUnless(await treeReadGate(page, seeded.key));
    await awaitIndexedCoverage(seeded.key);
    await awaitIndexedCoverage(seeded.child);
    const W = PLAN_SIZES_WORDS;
    const project = test.info().project.name;
    const planSlug = (await getArtifact(seeded.planArtifactId)).slug;
    const childSlug = (await getArtifact(seeded.childPlanArtifactId)).slug;
    const [a, b, c] = [OWN_TASKS.a, OWN_TASKS.b, OWN_TASKS.c].map((id) =>
      blockNodeId(planSlug, id)
    );
    // The parent's plan draws a task of the issue under it by its block id alone, as the tree
    // draws every node of another issue (`tree-read/tree-nodes.ts`' `treeIdOfNode`).
    const [xa, xb] = [CHILD_TASKS.first, CHILD_TASKS.second];
    const order = [b, a, c, xa, xb];
    const childKey: readonly KeyLink[] = [
      { href: buildIssuePath({ key: seeded.child, kind: "issue" }), key: seeded.child },
    ];
    for (const size of [DESKTOP, PHONE]) {
      const where = `at ${size.width} px`;
      await page.setViewportSize(size);
      const planRead = viewAnswerOf(page, "plan");
      await gotoIssue(page, presetPath(seeded.key, "plan"));

      // The chart, which the view opens on: the steps in the walk's order, b before a since c
      // keeps the chain a, c together, then the child's two in its plan's order.
      await expect(
        planView(page, "chart"),
        `Plan does not open on the chart ${where}`
      ).toHaveAttribute("aria-pressed", "true");
      await expect
        .poll(async () => (await chartSteps(page)).map((step) => step.id), {
          message: `the chart ${where} does not draw b, a, c and then the child's two in order`,
        })
        .toEqual(order);
      const answer = await heldAnswer(planRead, `${seeded.key}'s Plan read ${where}`);
      const readBack = [
        ...new Set(
          (answer.nodes ?? [])
            .filter((node) => node.supertag === "plan_task")
            .map((node) => {
              const issue =
                node.issue === undefined || node.issue === seeded.key ? "own" : node.issue;
              return `${issue} ${node.block_id ?? node.id} ${node.plan?.size || "none"}`;
            })
        ),
      ].sort();
      // The read, printed so what the server answered is on the record when the row passes too.
      console.log(
        `PLAN-SIZES [${project}] the Plan read of ${seeded.key} ${where}: ${readBack.join("; ")}`
      );
      expect(readBack, `the Plan read ${where} does not answer each task's size and issue`).toEqual(
        [
          `own ${OWN_TASKS.a} ${PLAN_SIZES.a.minutes}`,
          `own ${OWN_TASKS.b} ${PLAN_SIZES.b.minutes}`,
          `own ${OWN_TASKS.c} none`,
          `${seeded.child} ${CHILD_TASKS.first} ${PLAN_SIZES.childFirst.minutes}`,
          `${seeded.child} ${CHILD_TASKS.second} ${PLAN_SIZES.childSecond.minutes}`,
        ].sort()
      );

      // A need and a statement are named by the words the batched names read gives, which lands
      // after the blocks are read: the chart is read once c names a and the child's xb names xa,
      // each by the needed task's first line, and a, b and c each name the one statement they
      // serve, by its words.
      await expect
        .poll(async () => (await chartSteps(page)).find((step) => step.id === c)?.needs, {
          message: `c does not name a, which it needs, by a's first line ${where}`,
        })
        .toEqual([{ id: a, words: W.a }]);
      await expect
        .poll(async () => (await chartSteps(page)).find((step) => step.id === xb)?.needs, {
          message: `the child's xb does not name xa, which it needs, by xa's first line ${where}`,
        })
        .toEqual([{ id: xa, words: W.childFirst }]);
      await expect
        .poll(
          async () =>
            (await chartSteps(page))
              .filter((step) => [a, b, c].includes(step.id))
              .map((step) => step.serves),
          { message: `a step of the issue's own does not name its statement by its words ${where}` }
        )
        .toEqual([[W.statements[1]], [W.statements[0]], [W.statements[2]]]);
      const chart = await chartSteps(page);
      const step = (id: string) => chart.find((one) => one.id === id);
      const sized = [
        { id: a, minutes: PLAN_SIZES.a.minutes },
        { id: b, minutes: PLAN_SIZES.b.minutes },
        { id: xa, minutes: PLAN_SIZES.childFirst.minutes },
        { id: xb, minutes: PLAN_SIZES.childSecond.minutes },
      ].map((one) => ({ ...one, width: step(one.id)?.bar.width ?? 0 }));
      const widths = sized
        .map(({ id, minutes, width }) => `${id} ${minutes} min ${width.toFixed(1)} px`)
        .join(", ");
      console.log(`PLAN-SIZES [${project}] the bars ${where}: ${widths}`);
      const perMinute =
        sized.reduce((sum, one) => sum + one.width, 0) /
        sized.reduce((sum, one) => sum + one.minutes, 0);
      expect(
        sized
          .filter(({ minutes, width }) => Math.abs(width - minutes * perMinute) > PX)
          .map(({ id }) => id),
        `the bars ${where} are not as long as their sizes on one scale, 45 : 90 : 120 : 30: ${widths}`
      ).toEqual([]);
      expect(
        PLAN_SIZES.childSecond.minutes * perMinute,
        `the shortest sized bar ${where} draws no box: ${widths}`
      ).toBeGreaterThan(10);
      expect(
        order.map((id) => step(id)?.size),
        `a step ${where} does not draw its size in words, or "No size" where it holds none`
      ).toEqual([
        SIZE_WORDS.b,
        SIZE_WORDS.a,
        PLAN_WORDS.no_size,
        SIZE_WORDS.childFirst,
        SIZE_WORDS.childSecond,
      ]);
      expect(
        order.map((id) => step(id)?.bar.borderStyle),
        `the bar ${where} of c, which holds no size, is not the one dashed`
      ).toEqual(["solid", "solid", "dashed", "solid", "solid"]);
      // The axis is drawn from 761 px up alone, so the bar of no size is read against it there.
      if (size.width > 760) {
        expect(step(c)?.axisStep ?? 0, `c's track draws no axis ${where}`).toBeGreaterThan(10);
        expect(
          Math.abs((step(c)?.bar.width ?? 0) - (step(c)?.axisStep ?? 0)),
          `c's bar ${where} is not one line of the axis long: ${step(c)?.bar.width} px, the axis's ` +
            `lines ${step(c)?.axisStep} px apart`
        ).toBeLessThanOrEqual(PX);
      }
      // A bar starts where the bars it needs end, and one that needs nothing at the chart's start.
      for (const [mine, needed] of [
        [c, a],
        [xb, xa],
      ] as const) {
        expect(
          Math.abs(
            (step(mine)?.bar.left ?? Number.NaN) -
              (step(needed)?.bar.left ?? 0) -
              (step(needed)?.bar.width ?? 0)
          ),
          `the bar of ${mine} ${where} does not start where the bar of ${needed}, which it needs, ends`
        ).toBeLessThanOrEqual(PX);
      }
      expect(
        Math.max(
          ...[a, xa].map((id) =>
            Math.abs((step(id)?.bar.left ?? Number.NaN) - (step(b)?.bar.left ?? 0))
          )
        ),
        `a bar ${where} that needs nothing does not start where b's, which needs nothing, starts`
      ).toBeLessThanOrEqual(PX);
      expect(
        order.map((id) => step(id)?.keys),
        `the chart ${where} does not mark each of the child's rows, and none of the issue's own, with the child's key`
      ).toEqual([[], [], [], childKey, childKey]);

      // The steps: the same order, each with its size as a fact, the child's with its key.
      await planView(page, "steps").click();
      await expect(
        planView(page, "steps"),
        `Steps does not take the switch ${where}`
      ).toHaveAttribute("aria-pressed", "true");
      await expect
        .poll(async () => (await stepRows(page)).map((row) => row.id), {
          message: `the steps ${where} do not stand b, a, c and then the child's two in order`,
        })
        .toEqual(order);
      await expect
        .poll(async () => (await stepRows(page)).find((row) => row.id === c)?.needs, {
          message: `c's needs chip ${where} does not name a by a's first line`,
        })
        .toEqual([W.a]);
      await expect
        .poll(async () => (await stepRows(page)).find((row) => row.id === xb)?.needs, {
          message: `the child's xb's needs chip ${where} does not name xa by xa's first line`,
        })
        .toEqual([W.childFirst]);
      const steps = await stepRows(page);
      const { issue: issueLabel, size: sizeLabel } = STEP_FACT_LABELS;
      expect(
        steps.map((row) => row.facts),
        `a step ${where} does not draw its size, and the child's its issue, as facts`
      ).toEqual([
        [[sizeLabel, SIZE_WORDS.b]],
        [[sizeLabel, SIZE_WORDS.a]],
        [[sizeLabel, PLAN_WORDS.no_size]],
        [
          [issueLabel, seeded.child],
          [sizeLabel, SIZE_WORDS.childFirst],
        ],
        [
          [issueLabel, seeded.child],
          [sizeLabel, SIZE_WORDS.childSecond],
        ],
      ]);
      expect(
        steps.slice(0, 3).reduce((sum, row) => sum + row.noSize, 0),
        `"No size" is not drawn once among the issue's own steps ${where}`
      ).toBe(1);
      expect(
        steps.map((row) => row.keys),
        `the steps ${where} do not link the child's key on each of its steps, and on none of the issue's own`
      ).toEqual([[], [], [], childKey, childKey]);

      // The child's own plan: its two tasks in the same order, xb naming xa as it needs it, with
      // no key mark.
      const childOrder = [CHILD_TASKS.first, CHILD_TASKS.second].map((id) =>
        blockNodeId(childSlug, id)
      );
      const [ownXa, ownXb] = childOrder;
      await gotoIssue(page, presetPath(seeded.child, "plan"));
      await expect
        .poll(async () => (await chartSteps(page)).map((one) => one.id), {
          message: `the child's chart ${where} does not draw its two tasks in their order`,
        })
        .toEqual(childOrder);
      await expect
        .poll(async () => (await chartSteps(page)).find((one) => one.id === ownXb)?.needs, {
          message: `on the child's own chart ${where} xb does not name xa by xa's first line`,
        })
        .toEqual([{ id: ownXa, words: W.childFirst }]);
      const childChart = await chartSteps(page);
      expect(
        childChart.map((one) => one.size),
        `the child's chart ${where} does not draw its sizes`
      ).toEqual([SIZE_WORDS.childFirst, SIZE_WORDS.childSecond]);
      expect(
        childChart.map((one) => one.keys),
        `the child's own chart ${where} marks a task with a key`
      ).toEqual([[], []]);
      await planView(page, "steps").click();
      await expect
        .poll(async () => (await stepRows(page)).map((row) => row.id), {
          message: `the child's steps ${where} do not stand in the order of its parent's plan`,
        })
        .toEqual(childOrder);
      await expect
        .poll(async () => (await stepRows(page)).find((row) => row.id === ownXb)?.needs, {
          message: `on the child's own steps ${where} xb's needs chip does not name xa by xa's first line`,
        })
        .toEqual([W.childFirst]);
      const childSteps = await stepRows(page);
      expect(
        childSteps.map((row) => row.facts),
        `the child's own steps ${where} draw an issue fact, or no size`
      ).toEqual([[[sizeLabel, SIZE_WORDS.childFirst]], [[sizeLabel, SIZE_WORDS.childSecond]]]);
      await expect(
        planBody(page).locator("a[data-child-key]"),
        `the child's own steps ${where} link an issue key`
      ).toHaveCount(0);
    }
  });
});
