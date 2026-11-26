import type { Browser, BrowserContext, Page, TestInfo } from "@playwright/test";

import { presetPath } from "../web/src/features/issue-page/issue-address";
import {
  CONTROL_LABELS,
  PRESET_NAMES,
  PRESET_ORDER,
  type Preset,
} from "../web/src/features/issue-page/page-words";
import { buildIssuePath } from "../web/src/features/refs/routes";
import { withIssuePage } from "./issue-page-option";
import { editNoticesSent } from "./issue-page-t-rows";
import {
  type LargestShapeSeed,
  seedLargestShape,
  seedLargestShapeWaiting,
} from "./issue-page-t-scale";
import { type Gate, OWED_BY, seedEventsLanded } from "./issue-page-t-tree";
import {
  type ActReading,
  type ActStep,
  type ActTarget,
  type ArmWhen,
  actBounds,
  armStep,
  type BoundReading,
  boundHolds,
  type ControlTarget,
  type InstrumentConfig,
  installInstruments,
  type LoadRecord,
  type LoadRequests,
  longestEvent,
  longestTaskAfterViewRow,
  makeInput,
  readingLine,
  resultShown,
  revealStep,
  watchLoadRequests,
} from "./load-instruments";
import { resetDatabase } from "./seed";
import { test as base, expect } from "./test";
import { watchApiRequests } from "./traffic";
import { asUser } from "./users";

/**
 * What the load rows of `e2e/issue-page-load.e2e.ts` share: the views they load and the acts they
 * make at each width, the rows they read, the seed one worker's rows read, the processor's
 * slowing, and the five loads of a bound, each in a new browser context so its cache is empty.
 * The instruments that read each bound are `e2e/load-instruments.ts`'s.
 */

// ---------------------------------------------------------------------------------------------
// What the rows read on the page.
// ---------------------------------------------------------------------------------------------

/** A row of a view: the outline's rows (`NodeRow.tsx`), Spec's statements and other rows,
 *  Verification's cards, checks and issues, Plan's steps and cycles, and the Decisions view's
 *  rows (`decisions-filter/DecisionsRow.tsx`, a `[data-node-row]` under the view's body). */
export const VIEW_ROW_SELECTOR = [
  "[data-node-row]",
  "[data-spec-statement]",
  "[data-spec-other]",
  "[data-verification-card]",
  "[data-verification-check]",
  "[data-verification-issue]",
  "[data-plan-bar-row]",
  "[data-plan-cycle-row]",
  "[data-decisions-body] [data-node-row]",
].join(", ");

/** A row of the children's section (`children-section.tsx`). */
export const CHILD_ROW_SELECTOR = "[data-child-entry]";

/** Every row the load rows read: a view's and the children's section's. The element of each that
 *  owns the text of its first line carries `elementtiming="issue-page-row"`, and the init script
 *  ties each Element Timing entry to the row that holds its element by this selector. */
export const LOAD_ROW_SELECTOR = `${VIEW_ROW_SELECTOR}, ${CHILD_ROW_SELECTOR}`;

/** What a row stands under at the viewport's top: the header's band, and the view row, which
 *  stays below 1280 px where the header scrolls away. */
export const HEADER_SELECTORS = ["[data-identity-block]", "[data-filter-row]"] as const;

/** The mark of a view's place while its read is out: the view's tree reads busy, as Plan's empty
 *  tree does while a read of it is out (`plan-filter/PlanFilterBody.tsx`). */
export const VIEW_LOADS_SELECTOR = '[data-issue-page] [aria-busy="true"]';

/** The reader every load is signed in as. */
export const READER = "alice";

// ---------------------------------------------------------------------------------------------
// The views (R1).
// ---------------------------------------------------------------------------------------------

/** A view a load row reads, and the gate it is owed on until the view row's list holds it. */
export interface LoadView {
  readonly gate: Gate;
  /** Its word, in the row's title. */
  readonly name: string;
  /** Its token in the view row's list. */
  readonly view: string;
}

/** Root R2's views in its order: Dashboard first, Threads and Documents after Decisions, Graph
 *  last. The view row's list (`PRESET_ORDER`) holds those of the parts under the head. */
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
] as const;

/** The three views a later part adds to the view row's list, each with its word and its part. */
const LATER_VIEWS: Readonly<Record<string, { readonly name: string; readonly part: string }>> = {
  dashboard: { name: "Dashboard", part: "INFRA-5702, Dashboard's part," },
  documents: { name: "Documents", part: "INFRA-3940, part N, which adds Documents to the row," },
  threads: { name: "Threads", part: "INFRA-5704, the Threads part," },
};

/** Every view a load row reads: each the view row's list holds at this head, and Dashboard,
 *  Threads and Documents, each owed while the list does not hold it, as `dashboardPartGate` of
 *  `e2e/issue-page-t-tree.ts` is. A view the list holds that root R2 does not name comes last,
 *  so no view the row draws goes unread. */
export const LOAD_VIEWS: readonly LoadView[] = [
  ...ROOT_VIEWS,
  ...PRESET_ORDER.filter((view) => !(ROOT_VIEWS as readonly string[]).includes(view)),
].flatMap((view) => {
  const drawn = (PRESET_ORDER as readonly string[]).includes(view);
  const later = LATER_VIEWS[view];
  if (!drawn && later === undefined) return [];
  const name = (PRESET_NAMES as Readonly<Record<string, string>>)[view] ?? later?.name ?? view;
  return [
    {
      gate: {
        met: drawn,
        owed:
          `owed until ${later?.part ?? "its part"} is on this branch's base: the view row's ` +
          `list holds no ${name} here; ${OWED_BY}`,
      },
      name,
      view,
    },
  ];
});

// ---------------------------------------------------------------------------------------------
// The acts (R2), at each width.
// ---------------------------------------------------------------------------------------------

/** A width the load rows read the page at: `chromium-load`'s and `iphone-load`'s. */
export type LoadWidth = 1440 | 390;

/** What kind of act an act is. */
export type LoadActKind =
  | "details"
  | "fold"
  | "keys"
  | "menu"
  | "next"
  | "previous"
  | "unfold"
  | "view";

/** One act of R2: its steps, the last of which is the act measured and the others what it needs
 *  done first, and the steps that put the page back after it, so the act after it starts from
 *  where this one did. */
export interface LoadAct {
  readonly after: readonly ActStep[];
  readonly kind: LoadActKind;
  /** Its words, in the row's title. */
  readonly name: string;
  readonly steps: readonly ActStep[];
  /** The view a press on a view turns on. */
  readonly view?: Preset;
  /** The first moment the act can be made during the load: a press on a control at the first
   *  drawing of the view row, and a fold, an unfold, `j` and `k` at the first drawing of the
   *  rows. */
  readonly when: "rows" | "view row";
}

const PRESETS = '[data-testid="issue-presets"]';
const MENU = `${PRESETS} [role="menu"]`;
const ONE_CONTROL: ControlTarget = {
  kind: "control",
  selector: `${PRESETS} button[aria-haspopup="menu"]`,
};
/** The list Keys opens (`shell/ShortcutHelp.tsx`). */
const KEYS_LIST = '[role="dialog"][aria-label="Keyboard shortcuts"]';
/** Details, open: the drawer the header's Details control opens (`margin/Margin.tsx`), an
 *  `aside`, where today's margin, which carries the same test id, is a `div`. */
const DETAILS_OPEN = 'aside[data-testid="desktop-margin-shell"]:not([hidden])';
/** The fold control of a row that holds children (`NodeRow.tsx`), and an open such row. */
const FOLD_CONTROL = "button[data-node-bullet]";
const OPEN_ROW = '[data-node-row][aria-expanded="true"]';

/** A step made with the mouse at `target`'s box. */
function press(name: string, target: ActTarget, result: ActStep["result"], measured: boolean) {
  return { input: { kind: "press", target }, measured, name, result } as const satisfies ActStep;
}

/** A step made with the key `key`. */
function stroke(name: string, key: string, result: ActStep["result"], measured: boolean) {
  return { input: { key, kind: "key" }, measured, name, result } as const satisfies ActStep;
}

/** A press on `view`: from 1280 px up its own control, which then reads as pressed; below that a
 *  choice in the one control's menu, opened first, after which the one control names the view. */
function viewPress(view: Preset, width: LoadWidth): LoadAct {
  const word = PRESET_NAMES[view];
  const name = `a press on ${word}`;
  const naming = (selector: string): ControlTarget => ({
    kind: "control",
    label: { text: word, within: "[data-preset-name]" },
    selector,
  });
  const steps =
    width === 1440
      ? [
          press(
            name,
            naming(`${PRESETS} button[aria-pressed]`),
            {
              busy: VIEW_LOADS_SELECTOR,
              current: naming(`${PRESETS} button[aria-pressed="true"]`),
              kind: "view",
            },
            true
          ),
        ]
      : [
          press(
            "a press on the one control",
            ONE_CONTROL,
            { kind: "present", selector: MENU },
            false
          ),
          press(
            name,
            {
              kind: "control",
              label: { text: word, within: "span[id]" },
              selector: `${MENU} [role="menuitemradio"]`,
            },
            { busy: VIEW_LOADS_SELECTOR, current: naming(ONE_CONTROL.selector), kind: "view" },
            true
          ),
        ];
  return { after: [], kind: "view", name, steps, view, when: "view row" };
}

const FOLD = press(
  "a fold",
  { control: FOLD_CONTROL, kind: "fold", row: OPEN_ROW },
  { kind: "expanded", value: "false" },
  true
);
const UNFOLD = press(
  "an unfold",
  { control: FOLD_CONTROL, kind: "folded" },
  { kind: "expanded", value: "true" },
  true
);
const NEXT = stroke("j", "j", { kind: "ring", step: 1 }, true);

/** The acts every width makes on Conversation's rows: a fold and an unfold of the first row in
 *  the viewport that holds children, and `j` and `k`. An unfold first folds that row, and `k`
 *  first steps forward with `j`, so each has a row to act on at its first moment. */
const OUTLINE_ACTS: readonly LoadAct[] = [
  {
    after: [{ ...UNFOLD, measured: false }],
    kind: "fold",
    name: "a fold",
    steps: [FOLD],
    when: "rows",
  },
  {
    after: [],
    kind: "unfold",
    name: "an unfold",
    steps: [{ ...FOLD, measured: false }, UNFOLD],
    when: "rows",
  },
  { after: [], kind: "next", name: "j", steps: [NEXT], when: "rows" },
  {
    after: [],
    kind: "previous",
    name: "k",
    steps: [{ ...NEXT, measured: false }, stroke("k", "k", { kind: "ring", step: -1 }, true)],
    when: "rows",
  },
];

/** A press on Keys, which opens the list of keys, closed after with Escape. */
const KEYS_ACT: LoadAct = {
  after: [stroke("Escape", "Escape", { kind: "absent", selector: KEYS_LIST }, false)],
  kind: "keys",
  name: "a press on Keys",
  steps: [
    press(
      "a press on Keys",
      {
        kind: "control",
        label: { text: CONTROL_LABELS.keys, within: null },
        selector: "[data-filter-row] button",
      },
      { kind: "present", selector: KEYS_LIST },
      true
    ),
  ],
  when: "view row",
};

/** A press on Details, which opens the drawer, closed after with its own Close. */
const DETAILS_ACT: LoadAct = {
  after: [
    press(
      "Close",
      { kind: "control", selector: `${DETAILS_OPEN} [data-testid="details-close"]` },
      { kind: "absent", selector: DETAILS_OPEN },
      false
    ),
  ],
  kind: "details",
  name: "a press on Details",
  steps: [
    press(
      "a press on Details",
      { kind: "control", selector: '[data-testid="issue-details-control"]' },
      { kind: "present", selector: DETAILS_OPEN },
      true
    ),
  ],
  when: "view row",
};

/** A press on the one control below 1280 px, which opens its menu, closed after with Escape. */
const ONE_CONTROL_ACT: LoadAct = {
  after: [stroke("Escape", "Escape", { kind: "absent", selector: MENU }, false)],
  kind: "menu",
  name: "a press on the one control",
  steps: [
    press("a press on the one control", ONE_CONTROL, { kind: "present", selector: MENU }, true),
  ],
  when: "view row",
};

/** The acts at each width (Verification v4, "The acts at each width"): at 1440 px a press on each
 *  view control, a fold, an unfold, `j`, `k`, Keys and Details; at 390 px a press on the one
 *  control, a press on each view in its menu, a fold, an unfold, `j`, `k` and Details. The page
 *  draws no Keys at 760 px and below, so 390 px has no such act. */
export const LOAD_ACTS: Readonly<Record<LoadWidth, readonly LoadAct[]>> = {
  390: [
    ONE_CONTROL_ACT,
    ...PRESET_ORDER.map((view) => viewPress(view, 390)),
    ...OUTLINE_ACTS,
    DETAILS_ACT,
  ],
  1440: [
    ...PRESET_ORDER.map((view) => viewPress(view, 1440)),
    ...OUTLINE_ACTS,
    KEYS_ACT,
    DETAILS_ACT,
  ],
};

/** The title of an act's row during the load (Verification v4, `c-load-1440` Row 4). */
export function actRowTitle(act: string): string {
  return `${act} shows its result within 200 ms during the load`;
}

/** The title of the row of every act after the load (Row 5). */
export const AFTER_LOAD_TITLE = "each act shows its result within 200 ms after the load";

/** The address an act is made on during the load: a press on Conversation on Spec's address,
 *  since Conversation pressed on its own address changes nothing, and every other act on
 *  Conversation's. */
export function actAddress(key: string, act: LoadAct): string {
  return presetPath(
    key,
    act.kind === "view" && act.view === "conversation" ? "spec" : "conversation"
  );
}

/** The acts of `acts` in the order one load after its end makes them, from Conversation's
 *  address: every act that is no press on a view, then each press on a view from the view before
 *  it, Conversation's last, so a press on Conversation is made from another view too. */
export function afterLoadOrder(acts: readonly LoadAct[]): LoadAct[] {
  const views = acts.filter((act) => act.kind === "view");
  return [
    ...acts.filter((act) => act.kind !== "view"),
    ...views.filter((act) => act.view !== "conversation"),
    ...views.filter((act) => act.view === "conversation"),
  ];
}

/** The width a row reads the page at: its project's viewport. */
export function loadWidth(testInfo: TestInfo): LoadWidth {
  const width = testInfo.project.use.viewport?.width;
  if (width === 1440 || width === 390) return width;
  throw new Error(
    `the load rows read the page at 1440 px (chromium-load) and 390 px (iphone-load), and ` +
      `${testInfo.project.name} opens it at ${width ?? "no fixed width"}`
  );
}

// ---------------------------------------------------------------------------------------------
// The processor and the bounds.
// ---------------------------------------------------------------------------------------------

/** How much each load project slows the processor (`Emulation.setCPUThrottlingRate`). */
export const LOAD_CPU_RATES: Readonly<Record<string, number>> = {
  "chromium-load": 2,
  "iphone-load": 4,
};

/** The rate the loads of `project` run at: `ISSUE_PAGE_LOAD_CPU_RATE` where it is set, which
 *  `c-factor` alone sets, else the project's own. */
export function loadCpuRate(project: string, set = process.env.ISSUE_PAGE_LOAD_CPU_RATE): number {
  if (set !== undefined && set !== "") {
    const rate = Number(set);
    if (!Number.isFinite(rate) || rate < 1) {
      throw new Error(
        `ISSUE_PAGE_LOAD_CPU_RATE is ${JSON.stringify(set)}, and a rate is 1 or more`
      );
    }
    return rate;
  }
  const rate = LOAD_CPU_RATES[project];
  if (rate === undefined) {
    throw new Error(
      `${project} is no load project, which are ${Object.keys(LOAD_CPU_RATES).join(" and ")}`
    );
  }
  return rate;
}

/** R1's limit on the first screen's paint and on the largest contentful paint. */
export const FIRST_SCREEN_MS = 2_500;

/** R2's limit on an act's result, on each part of an act that needs a read, on the longest Event
 *  Timing entry and on the longest task after the first drawing of the view row. */
export const ACT_MS = 200;

/** R3's limit on the cumulative layout shift. */
export const LAYOUT_SHIFT = 0.1;

/** How long an act's result may take to show before its load reads none. */
export const ACT_RESULT_MS = 10_000;

/** A bound name's word for an act: its words, lowercased, joined by `-`. */
export function boundWord(words: string): string {
  return words
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-|-$/g, "");
}

/** Prints `reading`'s line and records it on the row, and fails the row, softly so every bound
 *  it reads is printed, where the fourth smallest of the five readings is over the limit. */
export function holdBound(reading: BoundReading): void {
  const line = readingLine(reading);
  console.log(line);
  base.info().annotations.push({ description: line, type: "reading" });
  expect
    .soft(
      boundHolds(reading),
      `${reading.bound}: the fourth smallest of the five readings is over ${reading.limit}: ${line}`
    )
    .toBe(true);
}

/** Prints a line of what a row read beside its bounds, and records it on the row. */
export function printReading(line: string): void {
  console.log(line);
  base.info().annotations.push({ description: line, type: "reading" });
}

/** Holds an act's five readings to R2 under `prefix`, as `actBounds` reads them: the time to its
 *  result, and the two parts of a result that needed a read, each `n/a` for a load whose result
 *  needed none. */
export function holdAct(prefix: string, rate: number, readings: readonly ActReading[]): void {
  for (const bound of actBounds(prefix, rate, ACT_MS, readings)) holdBound(bound);
}

/** Holds the five loads' longest Event Timing entry and longest task after the first drawing of
 *  the view row to R2 under `prefix`. */
export function holdTasks(prefix: string, rate: number, records: readonly LoadRecord[]): void {
  holdBound({
    bound: `${prefix}.longest-event-ms`,
    limit: ACT_MS,
    rate,
    readings: records.map((record) => longestEvent(record.event)),
  });
  holdBound({
    bound: `${prefix}.longest-task-ms`,
    limit: ACT_MS,
    rate,
    readings: records.map(longestTaskAfterViewRow),
  });
}

// ---------------------------------------------------------------------------------------------
// The seed of one worker.
// ---------------------------------------------------------------------------------------------

/** The seeds the load rows read, one at a time in the worker's database. */
export interface LoadSeeds {
  /** The issue at the largest issue's shape (`seedLargestShape`), which every row but the first
   *  focus's reads; seeded again, in a phase of its own, where that row's seed replaced it. */
  shape(): Promise<LargestShapeSeed>;
  /** The same issue with one ask that waits on the reader (`seedLargestShapeWaiting`), in a phase
   *  of its own. */
  waiting(): Promise<LargestShapeSeed & { readonly waiting: string }>;
}

/** How long one seed and its waits may take: about 14 s of seed, up to 30 s for its events and
 *  60 s for its edit notices. */
const SEED_MS = 240_000;

/** Seeds with `seed` and waits until nothing the seed set off can reach a load: its events have
 *  landed (`seedEventsLanded`) and the server has sent the edit notice each document a person
 *  wrote is owed (`editNoticesSent`), which reads the issue again. Both wait on a signed-in page
 *  and its request log, so the seed opens a context of its own, and closes it before any load, so
 *  no page of it draws or reads beside one. */
async function settledSeed<T extends { readonly key: string }>(
  browser: Browser,
  seed: () => Promise<T>
): Promise<T> {
  const seeded = await seed();
  const context = await asUser(browser, READER);
  try {
    const page = await context.newPage();
    const traffic = watchApiRequests(page);
    await seedEventsLanded(page);
    await editNoticesSent(seeded.key, traffic);
  } finally {
    await context.close();
  }
  return seeded;
}

/** Whether this worker has emptied its database yet. */
let emptied = false;

/**
 * The `test` of the load rows: `e2e/test.ts`'s, with one seed for the worker's rows.
 *
 * Each seed costs about 14 s and its notices 60 s, and no act of these rows writes, so the rows
 * of one worker share one seed: the automatic `freshDatabase`, which empties the database before
 * every row of the suite, is overridden here with one that empties it on its first run in the
 * worker and does nothing after (an override keeps the fixture's scope and its `auto`), and the
 * worker-scoped `loadSeeds` seeds the shape once, after that first emptying, since Playwright sets
 * a row's automatic fixtures up before the ones it asks for. A load that writes a read mark leaves
 * the next load the state a returning reader has.
 */
export const test = base.extend<Record<never, never>, { readonly loadSeeds: LoadSeeds }>({
  // A bare function, which takes the overridden fixture's options: a tuple naming `auto` is the
  // declaration of a new fixture's options, which Playwright's types refuse on an override.
  // Playwright refuses a first argument that is not a destructuring pattern.
  // biome-ignore lint/correctness/noEmptyPattern: the reset takes no other fixture.
  freshDatabase: async ({}, use) => {
    if (!emptied) {
      emptied = true;
      await resetDatabase();
    }
    await use(undefined);
  },
  loadSeeds: [
    async ({ browser }, use) => {
      let held:
        | { readonly kind: "shape"; readonly seeded: LargestShapeSeed }
        | {
            readonly kind: "waiting";
            readonly seeded: LargestShapeSeed & { readonly waiting: string };
          }
        | undefined;
      const seeds: LoadSeeds = {
        shape: async () => {
          if (held?.kind === "shape") return held.seeded;
          if (held !== undefined) await resetDatabase();
          const seeded = await settledSeed(browser, seedLargestShape);
          held = { kind: "shape", seeded };
          return seeded;
        },
        waiting: async () => {
          if (held?.kind === "waiting") return held.seeded;
          if (held !== undefined) await resetDatabase();
          const seeded = await settledSeed(browser, seedLargestShapeWaiting);
          held = { kind: "waiting", seeded };
          return seeded;
        },
      };
      await seeds.shape();
      await use(seeds);
    },
    { scope: "worker", timeout: SEED_MS },
  ],
});

// ---------------------------------------------------------------------------------------------
// Loads.
// ---------------------------------------------------------------------------------------------

/** What one load opens with. */
export interface LoadOptions {
  readonly address: string;
  /** The boxes the frame recorder keeps (`InstrumentConfig`). */
  readonly boxes: InstrumentConfig["boxes"];
  /** The row the load names, where it names one. */
  readonly named?: InstrumentConfig["named"];
  /** What is set up on the load's context before its page opens: a route that holds a file
   *  back. */
  readonly prepare?: (context: BrowserContext) => Promise<void>;
}

/** One load's page, open on its address, and its requests. */
export interface OpenedLoad {
  readonly page: Page;
  readonly requests: LoadRequests;
}

/** How many loads a bound is read over. */
export const LOADS = 5;

/**
 * One load of `options.address` in a new browser context, so its cache is empty: signed in as the
 * reader, with the new page turned on, the instruments installed and the processor slowed by
 * `rate` through a CDP session on the page before the navigation. `read` reads the load from its
 * navigation's commit, and the context closes once it has.
 */
export async function oneLoad<T>(
  browser: Browser,
  rate: number,
  options: LoadOptions,
  read: (load: OpenedLoad) => Promise<T>
): Promise<T> {
  const context = await asUser(browser, READER);
  try {
    await withIssuePage(context, READER);
    await options.prepare?.(context);
    const page = await context.newPage();
    await installInstruments(page, {
      boxes: options.boxes,
      header: HEADER_SELECTORS,
      named: options.named ?? null,
      rows: LOAD_ROW_SELECTOR,
      viewRows: VIEW_ROW_SELECTOR,
    });
    // The session stays open with the page: the slowing is the session's, and ends with it.
    const cpu = await context.newCDPSession(page);
    await cpu.send("Emulation.setCPUThrottlingRate", { rate });
    const requests = watchLoadRequests(page);
    await page.goto(options.address, { waitUntil: "commit" });
    return await read({ page, requests });
  } finally {
    await context.close();
  }
}

/** `LOADS` loads, one after another, each as `oneLoad` makes it. */
export async function fiveLoads<T>(
  browser: Browser,
  rate: number,
  options: LoadOptions,
  read: (load: OpenedLoad) => Promise<T>
): Promise<T[]> {
  const loads: T[] = [];
  for (let made = 0; made < LOADS; made += 1) {
    loads.push(await oneLoad(browser, rate, options, read));
  }
  return loads;
}

/**
 * Makes `steps` on the load's page, the first under the index `first` and each after it under the
 * next: each armed once it can be made (the first at `when`, every other at once), made, and
 * waited on until its result shows. A step armed at once is first brought where a press reaches
 * its control (`revealStep`), which an earlier act may have scrolled away; a step made at a first
 * drawing is made where its target was first drawn. `settle` runs before each measured step, after
 * that: a row of acts after the load waits for the load to end again there. A step whose result
 * does not show ends the steps, since the step after it has nothing to act on. Answers the index
 * of the last step, the act measured, and the index the next steps start at.
 */
export async function makeSteps(
  page: Page,
  first: number,
  steps: readonly ActStep[],
  when: ArmWhen,
  settle?: () => Promise<unknown>
): Promise<{ readonly last: number; readonly next: number }> {
  for (const [at, step] of steps.entries()) {
    const armedAt = at === 0 ? when : "now";
    if (armedAt === "now") await revealStep(page, step);
    if (step.measured) await settle?.();
    const point = await armStep(page, first + at, step, armedAt);
    await makeInput(page, step, point);
    if (!(await resultShown(page, first + at, ACT_RESULT_MS))) break;
  }
  return { last: first + steps.length - 1, next: first + steps.length };
}

/** The last row of Conversation's outline as the load ends, before any control of the paging
 *  rule is pressed: its node's id and kind. */
export async function lastOutlineRow(
  page: Page
): Promise<{ readonly id: string; readonly type: string }> {
  const row = await page.evaluate(() => {
    const rows = document.querySelectorAll("[data-issue-body] [data-node-row]");
    const last = rows.item(rows.length - 1);
    return last === null
      ? null
      : {
          id: last.getAttribute("data-node-id") ?? "",
          type: last.getAttribute("data-node-type") ?? "",
        };
  });
  if (row === null || row.id === "") throw new Error("Conversation drew no row with a node id");
  return row;
}

/** The address that names a row of issue `key`'s outline: an ask's, a comment's or a message's
 *  own (`/asks/<id>`, `/comments/<id>`, `/messages/<id>`), and a block's `#b-` fragment on the
 *  issue's address, which the first node carrying the block answers. */
export function rowAddress(
  key: string,
  row: { readonly id: string; readonly type: string }
): string {
  if (row.type === "ask" || row.type === "comment" || row.type === "message") {
    return buildIssuePath({ id: row.id, key, kind: row.type });
  }
  const block = row.id.indexOf("#b-");
  if (block !== -1) return `${presetPath(key, "conversation")}${row.id.slice(block)}`;
  throw new Error(
    `the row ${row.type} ${row.id} is neither an item nor a block, and no address names it`
  );
}

/** The selector of the outline's row of node `id`. */
export function outlineRowSelector(id: string): string {
  return `[data-issue-body] [data-node-row][data-node-id="${id.replace(/["\\]/g, "\\$&")}"]`;
}
