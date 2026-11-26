import { describe, expect, test } from "bun:test";
import { readdirSync, readFileSync } from "node:fs";
import { join, relative } from "node:path";

import { GATES } from "./gates";
import { TEXT_MARGIN_ATTRIBUTE } from "./TextMargin";

/**
 * R2 and decision 472: each layout step of the stage keeps its rule when the column of comments
 * on text is drawn. happy-dom applies no CSS, so this reads the steps from the class tokens the
 * stage's files write, as `git grep -c` counts them, and the browser suite measures the widths
 * (c-margin (8)): the window steps are those of B0, each named here with its rule, and a step a
 * later part adds fails this test until the list names it. With the column drawn, Plan's chart
 * (and, from part J, the Spec view) steps by the width of its own column, through container
 * variants beside the window classes, which apply only where the stage carries the attribute that
 * the column sets.
 */

/** The directory of `issue-page/`. */
const PAGE = join(import.meta.dir, "..");

/** What a file of the stage holds at one window width, by the token that names the step: how many
 *  lines hold it (`git grep -c`) and what it does. */
interface WindowStep {
  readonly file: string;
  /** Whether the step also steps by the column's width where the column is drawn. */
  readonly inColumn?: true;
  readonly lines: number;
  readonly rule: string;
  readonly token: string;
}

/** Every layout step at a window width inside the stage on B0, each with its rule. The `max-[N]:`
 *  of Tailwind is `(width < N)`, so `max-[761px]` takes 760 px and below. */
const WINDOW_STEPS: readonly WindowStep[] = [
  {
    file: "NodeRow.tsx",
    lines: 9,
    rule: "a phone row: its indent is 17 px a level, its guides, bullet and fold control sit closer, and its three columns are two, the margin under the words",
    token: "max-[761px]",
  },
  {
    file: "NodeRow.tsx",
    lines: 1,
    rule: "named in a comment only: a phone class is max-[761px], never max-[760px]",
    token: "max-[760px]",
  },
  {
    file: "NodeRow.tsx",
    lines: 1,
    rule: "the row's box takes a 6 px gap between its columns below 768 px",
    token: "max-[768px]",
  },
  {
    file: "TreeRows.tsx",
    lines: 1,
    rule: "the line of a row that holds more rows indents 17 px a level and 22 px on a phone",
    token: "max-[761px]",
  },
  {
    file: "slots/body.tsx",
    lines: 1,
    rule: "the outline's body takes the column's width, drawn 10 px left and 4 px right of the page's inset on a phone",
    token: "max-[761px]",
  },
  {
    file: "slots/view-plan.tsx",
    lines: 2,
    rule: "Plan's column is 1300 px at most, the column's width less 20 px on a phone (one line is the comment that names the class)",
    token: "max-[761px]",
  },
  {
    file: "slots/view-spec.tsx",
    lines: 1,
    rule: "the Spec view's column is 1080 px at most and takes the column's width on a phone, as the body's does",
    token: "max-[761px]",
  },
  {
    file: "spec-filter/SpecFilterBody.tsx",
    lines: 2,
    rule: "a statement's row has columns of 16 px, the words and 22 px with a 6 px gap on a phone",
    token: "max-[761px]",
  },
  {
    file: "spec-filter/SpecFilterBody.tsx",
    lines: 10,
    rule: "the Spec view's two columns, a decision's cell at 280 px and the statements, are one column below 902 px of window: no head, the cell above its statements, margins 0",
    token: "max-[901px]",
  },
  {
    file: "plan-filter/PlanFilterBody.tsx",
    lines: 15,
    rule: "Plan's phone cards: no head row, axis or frame, and each step is a wrapping card of its words, its bar and the statements it serves",
    token: "max-[761px]",
  },
  {
    file: "plan-filter/PlanFilterBody.tsx",
    inColumn: true,
    lines: 2,
    rule: "the chart's columns are 330 px, the rest and 320 px, and 260 px, the rest and 220 px below 1242 px of window (one line is the comment that names the class); with the column drawn, 330/320 from a 869 px column and 260/220 from a 482 px column",
    token: "max-[1241px]",
  },
  {
    file: "decisions-filter/DecisionsFilterBody.tsx",
    lines: 3,
    rule: "the Decisions body's top margin is 10 px and the margins above a part's heading and its foot 18 px on a phone",
    token: "max-[761px]",
  },
  {
    file: "verification-filter/VerificationFilterBody.tsx",
    lines: 1,
    rule: "named in a comment only: max-[760px] leaves 760 px itself out",
    token: "max-[760px]",
  },
  {
    file: "verification-filter/VerificationFilterBody.tsx",
    lines: 7,
    rule: "from 761 px of window the table is one frame under a head row, 1180 px wide at most, its statements at 38% and 62% and its checks at 58% and 42%; below it each row is a card",
    token: "min-[761px]",
  },
];

/** The files of `issue-page/` outside the stage, which hold window steps of their own: the
 *  header, the title and properties rows, the view row, the Contents pane, Details, and the
 *  controls beside the page's head. The column of comments on text (`text-comments/`) stands
 *  beside the stage, and its own width steps are read by `column.test.tsx`. */
const OUTSIDE_THE_STAGE: ReadonlySet<string> = new Set([
  "IssueActionsMenu.tsx",
  "IssueFieldForms.tsx",
  "IssueHeader.tsx",
  "IssueMargin.tsx",
  "IssuePresets.tsx",
  "IssuePropertiesRow.tsx",
  "IssueRouteControl.tsx",
  "IssueTitleRow.tsx",
  "contents-frame.tsx",
  "details-leaves.tsx",
  "identity-block.ts",
  "slots/view-row.tsx",
]);

/** A class token that is a window step: `max-[761px]:`, `min-[1241px]:` or a breakpoint (`md:`).
 *  One a container query writes (`@min-[869px]:`) is no window step. */
const WINDOW_VARIANT =
  /(?<![@\w-])((?:max|min)-\[[\d.]+(?:px|rem)\]|(?:max-)?(?:sm|md|lg|xl|2xl)):/g;

/** A class token that steps by a container's width: `@min-[869px]:` or `@max-[482px]:`. */
const CONTAINER_VARIANT = /@(?:(?:min|max)-)?\[(\d+(?:\.\d+)?)px\]:/g;

/** What each file of `issue-page/` holds, by its path there, read once. */
const SOURCES = new Map<string, string>();

/** What `file` of `issue-page/` holds. */
function read(file: string): string {
  let text = SOURCES.get(file);
  if (text === undefined) {
    text = readFileSync(join(PAGE, file), "utf8");
    SOURCES.set(file, text);
  }
  return text;
}

/** The lines of `file` that hold `token`, as `git grep -c` counts them. */
function linesHolding(file: string, token: string): number {
  return read(file)
    .split("\n")
    .filter((line) => line.includes(token)).length;
}

/** The source files of the stage: each `.ts` and `.tsx` under `issue-page/` that is no test, no
 *  fixture of this part, and none of the files outside the stage. */
const STAGE_FILES: readonly string[] = (readdirSync(PAGE, { recursive: true }) as string[])
  .map((path) => relative(PAGE, join(PAGE, path)))
  .filter((path) => /\.(ts|tsx)$/.test(path) && !/\.test\./.test(path))
  .filter((path) => !path.startsWith("text-comments/") && !OUTSIDE_THE_STAGE.has(path))
  .sort();

/** The window variants `file` writes, each once. */
function windowVariantsOf(file: string): string[] {
  return [...new Set([...read(file).matchAll(WINDOW_VARIANT)].map((match) => match[1] ?? ""))];
}

/** The container widths, in px, that `file` steps at, ascending and each once. */
function containerThresholds(file: string): number[] {
  const widths = [...read(file).matchAll(CONTAINER_VARIANT)].map((match) => Number(match[1]));
  return [...new Set(widths)].sort((a, b) => a - b);
}

/** Every class token of `file` that steps by a container's width. */
function containerTokens(file: string): string[] {
  return read(file).match(/[^\s"'`{}]*@(?:(?:min|max)-)?\[\d+(?:\.\d+)?px\]:[^\s"'`{}]*/g) ?? [];
}

/** The name the data variant of Tailwind carries for the stage's attribute: `data-text-margin`
 *  is written `data-[text-margin]:`, `in-data-[text-margin]:` or `group-data-text-margin:`. */
const ATTRIBUTE_NAME = TEXT_MARGIN_ATTRIBUTE.replace(/^data-/, "");

test("each window step of the stage keeps its rule", () => {
  const counts = WINDOW_STEPS.map(
    ({ file, token }) => `${file} ${token} ${linesHolding(file, token)}`
  );
  expect(counts, "a count of lines that hold a window step differs from B0's").toEqual(
    WINDOW_STEPS.map(({ file, lines, token }) => `${file} ${token} ${lines}`)
  );
  const named = new Set(WINDOW_STEPS.map(({ file, token }) => `${file} ${token}`));
  const unnamed = STAGE_FILES.flatMap((file) =>
    windowVariantsOf(file)
      .filter((variant) => !named.has(`${file} ${variant}`))
      .map((variant) => `${file} ${variant}`)
  );
  expect(unnamed, "a window step this list does not name: name it with its rule").toEqual([]);
  const withoutColumn = WINDOW_STEPS.filter(
    ({ file, inColumn }) => inColumn === true && containerThresholds(file).length === 0
  ).map(({ file, token }) => `${file} ${token}`);
  expect(
    withoutColumn,
    "a window step that also steps by the column's width carries no container variant"
  ).toEqual([]);
});

/** Whether the container class `token` applies in a container `width` px wide: Tailwind writes
 *  `@min-[N]:` as `(width >= N)` and `@max-[N]:` as `(width < N)`. */
function appliesAt(token: string, width: number): boolean {
  const [, side, at] = /@(min|max)-\[([\d.]+)px\]:/.exec(token) ?? [];
  return side === "min" ? width >= Number(at) : side === "max" && width < Number(at);
}

/** The grid columns of Plan's chart, by the form they draw (`PlanFilterBody.tsx`). */
const PLAN_GRIDS: Readonly<Record<string, string>> = {
  "grid-cols-[260px_minmax(0,1fr)_220px]": "narrow grid",
  "grid-cols-[330px_minmax(0,1fr)_320px]": "wide chart",
};

/** What Plan draws in a main column `width` px wide, by the container classes of `file` that apply
 *  there to a step's row (`[data-plan-bar-row]`), as the browser suite reads its form: its phone
 *  step's cards where the row is a flex box, else the grid its columns draw. */
function planFormIn(file: string, width: number): string {
  const utilities = containerTokens(file)
    .filter((token) => token.includes("_[data-plan-bar-row]]:") && appliesAt(token, width))
    .map((token) => token.slice(token.lastIndexOf(":") + 1));
  if (utilities.includes("flex")) return "cards";
  return utilities.flatMap((utility) => PLAN_GRIDS[utility] ?? []).join(" and ") || "no grid";
}

describe("every view steps by its column's width", () => {
  // The main column, in px, and the form Plan draws in it: the cards of its phone step below the
  // width its narrow grid's fixed tracks hold (about 482 px; `CHART_COLUMNS`' 260 px and 220 px,
  // measured on the base), the narrow grid from there, the wide chart from 869 px.
  const PLAN_FORMS: readonly (readonly [number, string])[] = [
    [392, "cards"],
    [481, "cards"],
    [482, "narrow grid"],
    [588, "narrow grid"],
    [748, "narrow grid"],
    [868, "narrow grid"],
    [869, "wide chart"],
  ];

  test("Plan's chart carries container variants at 482 px and 869 px beside its window classes", () => {
    const file = "plan-filter/PlanFilterBody.tsx";
    const thresholds = containerThresholds(file);
    expect(thresholds, "Plan's steps by its column's width").toEqual([482, 869]);
    expect(
      PLAN_FORMS.map(([width]) => `${width} px ${planFormIn(file, width)}`),
      "the form Plan draws at each main column"
    ).toEqual(PLAN_FORMS.map(([width, form]) => `${width} px ${form}`));
    expect(linesHolding(file, "max-[1241px]"), "the window step stays beside it").toBe(2);
    expect(linesHolding(file, "max-[761px]"), "the phone step stays beside it").toBe(15);
  });

  test("no container class applies without the attribute the column sets on the stage", () => {
    const containerClasses = STAGE_FILES.flatMap((file) =>
      containerTokens(file).map((token) => ({ file, token }))
    );
    expect(
      containerClasses.length,
      "no file of the stage steps by a container's width"
    ).toBeGreaterThan(0);
    const unguarded = containerClasses
      .filter(({ token }) => !token.includes(ATTRIBUTE_NAME))
      .map(({ file, token }) => `${file} ${token}`);
    expect(unguarded, "a container class that applies with no column drawn").toEqual([]);
  });

  // Part J's Spec view draws its two columns from a column 672 px wide (R2).
  test.skipIf(!GATES.PART_J)("the Spec view draws its two columns from a 672 px column", () => {
    const thresholds = containerThresholds("spec-filter/SpecFilterBody.tsx");
    expect(thresholds, "the Spec view's steps by its column's width").toEqual([672]);
  });
});
