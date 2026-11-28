/** The three inventories of the load rows (`e2e/issue-page-load.e2e.ts`), each written from its
 *  own source and compared whole with what the rows read, so a member added on one side and
 *  missed on the other fails here rather than leaving a view, an act or a control unread:
 *
 *  - every view under the head, from the view row's one list (`PRESET_ORDER`), with Dashboard,
 *    Threads and Documents owed until their parts add them to that list (R1);
 *  - every act at each width, from R2 and Verification v8's "The acts at each width" and Rows 4
 *    and 5 of `c-load-1440` (R2), and the Indent in Graph after the load, Row 5's act of its own
 *    (spec v23's R2, Verification v35's Row 5);
 *  - every control whose box R3 holds, from the plan's Inventories (R3).
 */
import { expect, test } from "bun:test";

import { PRESET_NAMES, PRESET_ORDER } from "../../web/src/features/issue-page/page-words";
import { AFTER_LOAD_ACTS } from "../acceptance/page-load";
import {
  AFTER_LOAD_ENTERS,
  AFTER_LOAD_INDENT,
  actRowTitle,
  afterLoadOrder,
  INDENT_TITLE,
  LOAD_ACTS,
  LOAD_VIEWS,
} from "../issue-page-load";
import { BOX_GROUPS } from "../load-instruments";
import realConfig from "../playwright.config";

/** Root R2's views in its order: Dashboard first, Threads and Documents after Decisions, Graph
 *  last. The view row draws the ones its list holds. */
const ROOT_VIEWS = [
  "dashboard",
  "conversation",
  "spec",
  "verification",
  "plan",
  "decisions",
  "threads",
  "documents",
  "graph",
];

/** The three views a later part adds to the view row's list, each by the issue of that part. */
const LATER_VIEWS: Readonly<Record<string, string>> = {
  dashboard: "INFRA-5702",
  documents: "INFRA-3940",
  threads: "INFRA-5704",
};

test("the load rows read every view the view row draws and owe the three later views", () => {
  const drawn: readonly string[] = PRESET_ORDER;
  expect(
    drawn.filter((view) => !ROOT_VIEWS.includes(view)),
    "the view row draws a view root R2's list does not name"
  ).toEqual([]);
  expect(LOAD_VIEWS.map((view) => view.view)).toEqual(
    ROOT_VIEWS.filter((view) => drawn.includes(view) || LATER_VIEWS[view] !== undefined)
  );
  for (const view of LOAD_VIEWS) {
    const underHead = drawn.includes(view.view);
    expect(
      view.gate.met,
      underHead
        ? `${view.view} is in the view row's list, so its load row must run, and it is owed`
        : `${view.view} is not in the view row's list, so its load row is owed, and it runs`
    ).toBe(underHead);
    if (underHead) {
      expect(view.name).toBe((PRESET_NAMES as Readonly<Record<string, string>>)[view.view]);
    } else {
      expect(view.gate.owed, `${view.view}'s owed clause names its part`).toContain(
        LATER_VIEWS[view.view] ?? "a part"
      );
    }
  }
});

test("the act rows make every act at each width, and no Keys act at 390 px", () => {
  const presses = PRESET_ORDER.map((view) => `a press on ${PRESET_NAMES[view]}`);
  const outline = ["a fold", "an unfold", "j", "k", "Enter"];
  expect(LOAD_ACTS[1440].map((act) => act.name)).toEqual([
    ...presses,
    ...outline,
    "a press on Keys",
    "a press on Details",
  ]);
  expect(LOAD_ACTS[390].map((act) => act.name)).toEqual([
    "a press on the one control",
    ...presses,
    ...outline,
    "a press on Details",
  ]);
  expect(LOAD_ACTS[390].filter((act) => act.kind === "keys")).toEqual([]);

  // After the load, `Enter` is made four times in each load: in Conversation and Graph, on a
  // row already drawn and on a row drawn by the view's paging control. Graph's two presses
  // follow the switch to Graph; the last view press returns to Conversation.
  const conversationEnters = [
    "Enter in Conversation on a row of the first page",
    "Enter in Conversation on a row of a later page",
  ];
  const graphEnters = [
    "Enter in Graph on a node of the first page",
    "Enter in Graph on a row of a later page",
  ];
  const viewsAfter = PRESET_ORDER.filter((view) => view !== "conversation").flatMap((view) => [
    `a press on ${PRESET_NAMES[view]}`,
    ...(view === "graph" ? graphEnters : []),
  ]);
  for (const [width, others] of [
    [1440, ["a fold", "an unfold", "j", "k", "a press on Keys", "a press on Details"]],
    [390, ["a press on the one control", "a fold", "an unfold", "j", "k", "a press on Details"]],
  ] as const) {
    expect(afterLoadOrder(LOAD_ACTS[width]).map((act) => act.name)).toEqual([
      ...others,
      ...conversationEnters,
      ...viewsAfter,
      "a press on Conversation",
    ]);
  }
  // Each `Enter` after the load leaves the row's text with Escape before the next act.
  for (const act of afterLoadOrder(LOAD_ACTS[1440]).filter((each) => each.kind === "edit")) {
    expect(act.after.map((step) => step.input)).toEqual([{ key: "Escape", kind: "key" }]);
  }

  // A press on a view is its own control from 1280 px up, and below 1280 px a choice in the one
  // control's menu: the menu is opened first, and the choice is the act measured.
  for (const act of LOAD_ACTS[1440].filter((each) => each.kind === "view")) {
    expect(act.steps.map((step) => step.measured)).toEqual([true]);
  }
  for (const act of LOAD_ACTS[390].filter((each) => each.kind === "view")) {
    expect(act.steps.map((step) => step.measured)).toEqual([false, true]);
    expect(act.steps[0]?.input.kind).toBe("press");
  }
  // Every act measures its last step and no other.
  for (const act of [...LOAD_ACTS[1440], ...LOAD_ACTS[390], ...afterLoadOrder(LOAD_ACTS[1440])]) {
    expect(act.steps.map((step) => step.measured).lastIndexOf(true)).toBe(act.steps.length - 1);
    expect(act.steps.filter((step) => step.measured)).toHaveLength(1);
  }

  // Each load project runs the rows of its own width's acts and no other: the rows of the two
  // widths are declared once over both lists, and each project's `grepInvert` leaves out the
  // rows of the act its width does not have.
  const titles = [...new Set([...LOAD_ACTS[1440], ...LOAD_ACTS[390]].map((act) => act.name))].map(
    actRowTitle
  );
  for (const [project, width] of [
    ["chromium-load", 1440],
    ["iphone-load", 390],
  ] as const) {
    const declared = (realConfig.projects ?? []).find((each) => each.name === project);
    expect(declared, `e2e/playwright.config.ts declares no project ${project}`).toBeDefined();
    const inverted = declared?.grepInvert;
    const patterns = inverted === undefined ? [] : Array.isArray(inverted) ? inverted : [inverted];
    expect(
      titles.filter((title) => !patterns.some((pattern) => pattern.test(title))).sort(),
      `${project} runs the act rows of another width's acts, or leaves out one of its own`
    ).toEqual(LOAD_ACTS[width].map((act) => actRowTitle(act.name)).sort());
  }
});

/** The rows of the outline, Conversation's or Graph's, each drawn at its `aria-level`. */
const OUTLINE_ROWS = "[data-issue-body] [data-node-row]";

/** The node kinds Graph indents, as the root's editing rule names them, by the `data-node-type`
 *  each row carries (`NodeRow.tsx`): a criterion, a statement, a check, a plan task and a prose
 *  node. */
const INDENTED_KINDS = ["criterion", "requirement", "check", "plan_task", "prose"];

test("the Indent after the load is one act in Graph, its Tab measured and its Shift+Tab after it, and no list the production script reads holds it", () => {
  const keys = (steps: typeof AFTER_LOAD_INDENT.steps) =>
    steps.map((step) => [step.input.kind === "key" ? step.input.key : "a press", step.measured]);
  expect(AFTER_LOAD_INDENT.kind).toBe("indent");
  expect(AFTER_LOAD_INDENT.on).toBe("graph");
  // A press on the row puts the focus on it, `Enter` puts the caret in its text, untimed, and
  // `Tab`, the act measured, draws its node as a child of the node above it.
  expect(keys(AFTER_LOAD_INDENT.steps)).toEqual([
    ["a press", false],
    ["Enter", false],
    ["Tab", true],
  ]);
  const [press, enter, tab] = AFTER_LOAD_INDENT.steps;
  expect(press?.input).toEqual({
    kind: "press",
    target: {
      drawn: "any",
      kind: "row",
      outsideWords: "[data-node-words]",
      row: INDENTED_KINDS.map((kind) => `${OUTLINE_ROWS}[data-node-type="${kind}"]`).join(", "),
      sibling: OUTLINE_ROWS,
      words: '[elementtiming="issue-page-row"]',
    },
  });
  expect(enter?.result).toEqual({ box: "[data-editor-box]", kind: "caret" });
  expect(tab?.result).toEqual({ kind: "indent", rows: OUTLINE_ROWS, step: 1 });
  // After the reading, `Shift+Tab` puts the node back, and `Escape` leaves its text, untimed.
  expect(keys(AFTER_LOAD_INDENT.after)).toEqual([
    ["Shift+Tab", false],
    ["Escape", false],
  ]);
  expect(AFTER_LOAD_INDENT.after[0]?.result).toEqual({
    kind: "indent",
    rows: OUTLINE_ROWS,
    step: -1,
  });

  // Row 4, Row 5, the production script (`AFTER_LOAD_ACTS`) and the profiler of
  // `issue-page-load-longest.e2e.ts`, which reads `LOAD_ACTS`, `afterLoadOrder` and
  // `AFTER_LOAD_ENTERS`, make no Indent: the production check sends no write.
  for (const acts of [
    LOAD_ACTS[1440],
    LOAD_ACTS[390],
    afterLoadOrder(LOAD_ACTS[1440]),
    afterLoadOrder(LOAD_ACTS[390]),
    AFTER_LOAD_ENTERS,
    AFTER_LOAD_ACTS,
  ]) {
    expect(
      acts.filter(
        (act) =>
          act.kind === "indent" ||
          [...act.steps, ...act.after].some(
            (step) => step.input.kind === "key" && /Tab/.test(step.input.key)
          )
      )
    ).toEqual([]);
  }

  // Both load projects run the Indent's row: no `grepInvert` leaves it out.
  expect(INDENT_TITLE).toBe("an Indent in Graph shows its result within 200 ms after the load");
  for (const project of ["chromium-load", "iphone-load"]) {
    const declared = (realConfig.projects ?? []).find((each) => each.name === project);
    expect(declared, `e2e/playwright.config.ts declares no project ${project}`).toBeDefined();
    const inverted = declared?.grepInvert;
    const patterns = inverted === undefined ? [] : Array.isArray(inverted) ? inverted : [inverted];
    expect(
      patterns.filter((pattern) => pattern.test(INDENT_TITLE)),
      `${project} leaves out the Indent's row`
    ).toEqual([]);
  }
});

test("the box recorder names each control of the view row and the header and the header's band", () => {
  expect(BOX_GROUPS).toEqual({
    "view row's controls": [
      '[data-testid="issue-presets"] button',
      '[data-testid="issue-presets"] label',
      '[data-filter-row] button[aria-haspopup="menu"]',
    ],
    "header's controls": [
      "[data-identity-block] button",
      '[data-testid="issue-state-actions"] > *',
    ],
    "header's band": ["[data-identity-block]"],
  });
});
