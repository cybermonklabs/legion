import type { Locator, Page } from "@playwright/test";

import { createIssue, createIssueArtifact, createProject } from "./api";
import { specBlockRef } from "./issue-layout-seed-text";
import { issuePage } from "./issue-page";

/**
 * What the Plan view's rows read of the view, and the one plan they seed themselves: the chart,
 * the one control that switches it to the ordered steps (`web/src/features/issue-page/
 * plan-filter/PlanFilterBody.tsx`), and the box both stand in. The steps are the outline's own
 * rows, which `e2e/issue-page-g.ts` reads.
 *
 * - **The box** `[data-plan-body]`, which holds the control and the chart or the steps.
 * - **The control** `[data-plan-switch]`, a `group` named "Plan": the two buttons
 *   `[data-plan-view="chart"]` and `[data-plan-view="steps"]`, the one that is on carrying
 *   `aria-pressed="true"`.
 * - **The chart** `[data-plan-chart]`: from 761 px up its head row `[data-plan-head]`, whose
 *   middle cell (`[data-plan-head] > div`) holds the axis `[data-plan-axis]`, one `i` per unit;
 *   then the tree. A step's row is `[data-plan-bar-row]`, a `treeitem` with `data-node-id` and
 *   `data-plan-start` (the unit its bar starts at), whose three children are its words (the
 *   task's first line `[data-node-words]` and its facts `[data-plan-facts]`: the mark that it
 *   holds no size `[data-plan-no-size]` and each task it needs `[data-plan-need]`, by that
 *   task's node id), the track that holds its bar `[data-plan-bar]`, and the statements it
 *   serves `[data-plan-serves]`, one chip each: `[data-plan-serve]` is the chip's bordered box,
 *   and a chip that links to its statement holds its link inside that box
 *   (`[data-plan-serve] a`), stretched over the whole box. At 760 px and below the row is one
 *   card, and its first and third children and its facts lay nothing out of their own
 *   (`display: contents`), so a reader measures what they hold. A cycle's row is
 *   `[data-plan-cycle-row]`, a `treeitem` whose `data-node-id` is the task it stands for and
 *   whose attribute holds every task of it, around its one line `[data-plan-cycle]`. The curves
 *   between two bars are `svg[data-plan-links] path[data-plan-link="<needed id> <id>"]`.
 * - **The seed** `seedPlanMarks`: a plan whose first task's words are bold and numbered, as a
 *   written plan's often are, written through `e2e/api.ts` alone.
 */

/** One row of the Plan view's chart: a step with where its bar starts, what it says of its
 *  size, the steps it needs and the statements it serves; or a cycle's one line. */
export interface ChartRow {
  readonly id: string;
  readonly kind: "bar" | "cycle";
  readonly needs: readonly string[];
  readonly serves: readonly string[];
  readonly size: string;
  /** The bar's left edge and its width, in px, or null for a cycle's line. */
  readonly bar: { readonly left: number; readonly width: number } | null;
  readonly start: number;
}

/** Every row of the Plan view's chart, in the order it is drawn. */
export function planChartRows(page: Page): Promise<ChartRow[]> {
  return page.evaluate(() => {
    const text = (element: Element | null) =>
      (element?.textContent ?? "").replace(/\s+/g, " ").trim();
    return [
      ...document.querySelectorAll(
        "[data-plan-chart] :is([data-plan-bar-row], [data-plan-cycle-row])"
      ),
    ].map((row) => {
      const bar = row.querySelector("[data-plan-bar]")?.getBoundingClientRect();
      return {
        bar: bar === undefined ? null : { left: bar.left, width: bar.width },
        id: row.getAttribute("data-node-id") ?? "",
        kind: row.hasAttribute("data-plan-cycle-row") ? ("cycle" as const) : ("bar" as const),
        needs: [...row.querySelectorAll("[data-plan-need]")].map(
          (need) => need.getAttribute("data-plan-need") ?? ""
        ),
        serves: [...row.querySelectorAll("[data-plan-serve]")].map(text),
        size: text(row.querySelector("[data-plan-no-size]")),
        start: Number(row.getAttribute("data-plan-start") ?? "-1"),
      };
    });
  });
}

/** One of the Plan view's two controls, the chart or the ordered steps. */
export function planView(page: Page, view: "chart" | "steps"): Locator {
  return issuePage(page).locator(`[data-plan-view="${view}"]`);
}

/** The view's box, which holds the control and the chart or the steps. */
export function planBody(page: Page): Locator {
  return issuePage(page).locator("[data-plan-body]");
}

/** The chart, the drawing the view opens on. */
export function planChart(page: Page): Locator {
  return issuePage(page).locator("[data-plan-chart]");
}

/** A box on the page, in CSS pixels. */
export interface Box {
  readonly bottom: number;
  readonly height: number;
  readonly left: number;
  readonly right: number;
  readonly top: number;
  readonly width: number;
}

/** Where one step's row stands and how it is drawn, at any width. */
export interface StepLayout {
  readonly id: string;
  /** Its bar: where it stands, and its border and its fill. */
  readonly bar: Box & {
    readonly backgroundImage: string;
    readonly borderColor: string;
    readonly borderStyle: string;
  };
  /** The bordered box of each statement chip it serves (`[data-plan-serve]`). */
  readonly chips: readonly Box[];
  /** The row's own box, and how it is drawn: its left edge, its mark and its fill. */
  readonly row: Box & {
    readonly backgroundColor: string;
    readonly borderLeftColor: string;
    readonly borderLeftWidth: string;
    readonly boxShadow: string;
  };
  /** The widths of the row's first column and its third, where the row is the chart's three
   *  columns; at 760 px and below the boxes of those columns lay nothing out and are 0. */
  readonly columns: { readonly first: number; readonly third: number };
  /** The tops of the parts of the card's line: "No size", each task it needs, each statement. */
  readonly line: {
    readonly needs: readonly number[];
    readonly noSize: number | null;
    readonly serves: readonly number[];
  };
  /** The track that holds the bar. */
  readonly track: Box;
}

/** Every step's row of the chart, in the order it is drawn, with its layout. */
export function stepLayouts(page: Page): Promise<StepLayout[]> {
  return page.evaluate(() => {
    const box = (element: Element) => {
      const rect = element.getBoundingClientRect();
      return {
        bottom: rect.bottom,
        height: rect.height,
        left: rect.left,
        right: rect.right,
        top: rect.top,
        width: rect.width,
      };
    };
    return [...document.querySelectorAll("[data-plan-chart] [data-plan-bar-row]")].map((row) => {
      const bar = row.querySelector("[data-plan-bar]");
      const barStyle = bar === null ? null : getComputedStyle(bar);
      const rowStyle = getComputedStyle(row);
      const first = row.firstElementChild;
      const third = row.querySelector(":scope > [data-plan-serves]");
      const noSize = row.querySelector("[data-plan-no-size]");
      const nothing = { bottom: 0, height: 0, left: 0, right: 0, top: 0, width: 0 };
      // One query for the statement chips, so the line's tops are those of the chips' boxes.
      const chips = [...row.querySelectorAll("[data-plan-serve]")].map(box);
      return {
        bar: {
          ...(bar === null ? nothing : box(bar)),
          backgroundImage: barStyle?.backgroundImage ?? "",
          borderColor: barStyle?.borderTopColor ?? "",
          borderStyle: barStyle?.borderTopStyle ?? "",
        },
        chips,
        columns: {
          first: first === null ? 0 : first.getBoundingClientRect().width,
          third: third === null ? 0 : third.getBoundingClientRect().width,
        },
        id: row.getAttribute("data-node-id") ?? "",
        line: {
          needs: [...row.querySelectorAll("[data-plan-need]")].map(
            (need) => need.getBoundingClientRect().top
          ),
          noSize: noSize === null ? null : noSize.getBoundingClientRect().top,
          serves: chips.map((chip) => chip.top),
        },
        row: {
          ...box(row),
          backgroundColor: rowStyle.backgroundColor,
          borderLeftColor: rowStyle.borderLeftColor,
          borderLeftWidth: rowStyle.borderLeftWidth,
          boxShadow: rowStyle.boxShadow,
        },
        track: bar?.parentElement == null ? nothing : box(bar.parentElement),
      };
    });
  });
}

/** What the rows read of the chart's head row: where "Serves" starts against the first
 *  statements' column, and the boxes of its middle cell and of each line of the axis. Null where
 *  no chart is drawn. At 760 px and below the chart draws its head row hidden (`display: none`),
 *  so the row is there and every box and edge read of it is 0. */
export interface HeadLayout {
  readonly axis: readonly Box[];
  readonly middle: Box;
  readonly serves: number;
  readonly servesColumn: number | null;
}

export function headLayout(page: Page): Promise<HeadLayout | null> {
  return page.evaluate(() => {
    const box = (element: Element) => {
      const rect = element.getBoundingClientRect();
      return {
        bottom: rect.bottom,
        height: rect.height,
        left: rect.left,
        right: rect.right,
        top: rect.top,
        width: rect.width,
      };
    };
    const head = document.querySelector("[data-plan-chart] [data-plan-head]");
    const middle = head?.querySelector(":scope > div") ?? null;
    const serves = head?.lastElementChild ?? null;
    if (head === null || middle === null || serves === null) return null;
    const column = document.querySelector("[data-plan-chart] [data-plan-serves]");
    return {
      axis: [...head.querySelectorAll("[data-plan-axis] i")].map(box),
      middle: box(middle),
      serves: serves.getBoundingClientRect().left,
      servesColumn: column === null ? null : column.getBoundingClientRect().left,
    };
  });
}

/** The text of each element the box marks as the page's own words (`[data-page-chrome]`), less
 *  what an author wrote inside it (`[data-author-text]`), which a reader checks against the
 *  page's one list. */
export function chromeTexts(page: Page): Promise<string[]> {
  return planBody(page).evaluate((body) =>
    [...body.querySelectorAll("[data-page-chrome]")].map((element) => {
      const own = element.cloneNode(true) as Element;
      for (const authored of own.querySelectorAll("[data-author-text]")) authored.remove();
      return own.textContent ?? "";
    })
  );
}

/** The project `seedPlanMarks` makes. */
const MARKS_PROJECT = "MARKS";

/** The words `seedPlanMarks` gives its two tasks and its two statements: each starts bold and
 *  numbered, as a written plan's and spec's often do, the task's number escaped as markdown
 *  writes a number that starts a line (`1\.`). */
const PLAN_MARKS_WORDS = {
  first: "**1\\. Stand the harness up.** The rows run on the real stack.",
  firstStatement: "**R1.** The page draws a need by its words.",
  second: "**2\\. Read the needs back.** Each need is drawn by the words of the task it names.",
  secondStatement: "**R2.** A word of the page shows no mark of its source.",
} as const;

export interface PlanMarksSeed {
  readonly key: string;
  readonly planArtifactId: string;
  readonly specArtifactId: string;
}

/** A plan of two tasks whose words carry bold, numbered labels (`PLAN_MARKS_WORDS`): the second
 *  task needs the first and serves both statements of the spec, whose words start with labels
 *  of their own. Every write goes through `e2e/api.ts`, and the second task names the first in
 *  the same write that makes it, which the server reads from that write's own blocks. */
export async function seedPlanMarks(): Promise<PlanMarksSeed> {
  await createProject({ key: MARKS_PROJECT, name: "Plan marks" });
  const issue = await createIssue({
    force: true,
    project: MARKS_PROJECT,
    spec:
      `:::requirement{#r1}\n${PLAN_MARKS_WORDS.firstStatement}\n:::\n\n` +
      `:::requirement{#r2}\n${PLAN_MARKS_WORDS.secondStatement}\n:::\n`,
    title: "The plan names a need by its words",
  });
  const statement = (id: string) => specBlockRef(issue.key, id);
  const plan = await createIssueArtifact(issue.key, {
    content:
      "How the marks are read.\n\n" +
      `:::plan_task{#t1 serves="${statement("r1")}"}\n${PLAN_MARKS_WORDS.first}\n:::\n\n` +
      `:::plan_task{#t2 serves="${statement("r1")} ${statement("r2")}" ` +
      `needs="dispatch://${issue.key}/artifact/plan-md#b-t1"}\n${PLAN_MARKS_WORDS.second}\n:::\n`,
    name: "plan.md",
  });
  return {
    key: issue.key,
    planArtifactId: plan.artifact.id,
    specArtifactId: issue.primary_artifact_id ?? "",
  };
}
