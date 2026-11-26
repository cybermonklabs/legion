/** The three inventories of the load rows (`e2e/issue-page-load.e2e.ts`), each written from its
 *  own source and compared whole with what the rows read, so a member added on one side and
 *  missed on the other fails here rather than leaving a view, an act or a control unread:
 *
 *  - every view under the head, from the view row's one list (`PRESET_ORDER`), with Dashboard,
 *    Threads and Documents owed until their parts add them to that list (R1);
 *  - every act at each width, from R2 and Verification v8's "The acts at each width" and Rows 4
 *    and 5 of `c-load-1440` (R2);
 *  - every control whose box R3 holds, from the plan's Inventories (R3).
 */
import { expect, test } from "bun:test";

import { PRESET_NAMES, PRESET_ORDER } from "../../web/src/features/issue-page/page-words";
import { actRowTitle, afterLoadOrder, LOAD_ACTS, LOAD_VIEWS } from "../issue-page-load";
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
