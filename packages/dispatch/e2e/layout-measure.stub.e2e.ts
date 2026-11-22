/** The layout instrument's own controls: `measureLayout` (`e2e/layout-measure.ts`) and
 *  `judgeReading` (`e2e/layout-check.ts`) read small pages this file writes into the browser with
 *  `page.setContent`, so the rows need no answer at all - no recording, no server, no seed - and
 *  run on every pull request with the stubbed layer (packages/dispatch/README.md, "The stubbed
 *  layer"). Each defect the layout rule names is drawn once and must be reported, by kind and by
 *  the locator a breach list entry would name; each case the rule allows is drawn beside the
 *  others and must report nothing. An instrument whose clean reading has never been seen to fail
 *  proves nothing, and these are the failures it is seen to report. */
import { expect, type Page } from "@playwright/test";

import type { KnownLayoutBreach } from "./layout-breaches";
import { judgeReading, staleEntries } from "./layout-check";
import { type LayoutBreach, type LayoutReading, measureLayout } from "./layout-measure";
import { stubbed } from "./stub/stub-test";

// No row takes a `scene`, so no seed ever runs and no recording is read or written.
const test = stubbed(async () => ({}));

/** Words enough to run past any box below. */
const LONG = "a line of words much wider than the box it is drawn in, with no place to break it";

/** `count` spans, each one entry of a group. */
function entries(count: number): string {
  return Array.from({ length: count }, (_, index) => `<span>entry ${index + 1}</span>`).join(" ");
}

/** Draws `body` at a phone's width and reads it as a layout row reads a page. */
async function measure(
  page: Page,
  body: string,
  listed: readonly string[] = []
): Promise<LayoutReading> {
  await page.setViewportSize({ height: 900, width: 360 });
  await page.setContent(`<!doctype html><html><body style="margin: 0">${body}</body></html>`);
  return await page.evaluate(measureLayout, listed);
}

/** Each defect the rule names, the page that draws it, and the breach it must be reported as. */
const defects: readonly {
  readonly name: string;
  readonly body: string;
  readonly kind: LayoutBreach["kind"];
  readonly locator: string;
}[] = [
  {
    body: '<div data-testid="wide" style="height: 20px; width: 900px"></div>',
    kind: "page-scroll",
    locator: '[data-testid="wide"]',
    name: "a page wider than the viewport",
  },
  {
    body: `<div data-testid="strip" style="overflow-x: auto; white-space: nowrap; width: 200px">${entries(12)}</div>`,
    kind: "box-scroll",
    locator: '[data-testid="strip"]',
    name: "a row of chips that scrolls sideways",
  },
  {
    body:
      '<div data-testid="mixed" style="overflow-x: auto; width: 200px">' +
      `<table><tr><td style="white-space: nowrap">${LONG}</td></tr></table>` +
      '<div style="width: 400px">a row of chips beside the table</div></div>',
    kind: "box-scroll",
    locator: '[data-testid="mixed"]',
    name: "a box that scrolls for a row beside its data table",
  },
  {
    body:
      '<div data-testid="icons" style="display: flex; overflow-x: auto; width: 100px">' +
      '<svg height="20" style="flex-shrink: 0" width="300"></svg></div>',
    kind: "box-scroll",
    locator: '[data-testid="icons"]',
    name: "a box that scrolls for an inline svg icon",
  },
  {
    body:
      '<div style="overflow: hidden; width: 200px">' +
      '<div data-testid="cut" style="height: 20px; width: 400px">a block wider than its box</div></div>',
    kind: "clipped",
    locator: '[data-testid="cut"]',
    name: "a block cut off by the box that clips it",
  },
  {
    body: `<div data-testid="cut-text" style="overflow: hidden; white-space: nowrap; width: 100px">${LONG}</div>`,
    kind: "clipped",
    locator: '[data-testid="cut-text"]',
    name: "a line of text its own box cuts with no ellipsis",
  },
  {
    body: `<div data-layout-group="labels">${entries(11)}<button data-layout-more type="button">+1</button></div>`,
    kind: "group-over-cap",
    locator: '[data-layout-group="labels"]',
    name: "a closed group showing eleven entries",
  },
  {
    body: `<div data-layout-group="links" data-layout-max="3">${entries(4)}</div>`,
    kind: "group-over-cap",
    locator: '[data-layout-group="links"]',
    name: "a closed group past its own cap of three",
  },
];

/** Every case the rule allows, on one page: none of them may be reported. */
const allowed = [
  `<span style="display: block; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; width: 100px">${LONG}</span>`,
  `<pre style="overflow-x: auto; width: 200px">${LONG}</pre>`,
  `<div style="overflow-x: auto; width: 200px"><pre>${LONG}</pre></div>`,
  `<div style="overflow-x: auto; width: 200px"><table><tr><td style="white-space: nowrap">${LONG}</td></tr></table></div>`,
  '<div data-layout-scroll="board" style="overflow-x: auto; width: 200px"><div style="display: flex; height: 20px; width: 800px"></div></div>',
  `<div data-layout-group="labels" data-layout-open>${entries(15)}<button data-layout-more type="button">Show fewer</button></div>`,
  `<div data-layout-group="components">${entries(10)}<button data-layout-more type="button">+5</button></div>`,
  `<div style="position: relative">A label<span style="clip: rect(0, 0, 0, 0); height: 1px; overflow: hidden; position: absolute; white-space: nowrap; width: 1px">${LONG}</span></div>`,
  `<p style="-webkit-box-orient: vertical; -webkit-line-clamp: 2; display: -webkit-box; overflow: hidden; width: 200px">${LONG} ${LONG}</p>`,
  `<div style="display: flex; flex-wrap: wrap; gap: 4px; width: 200px">${entries(12)}</div>`,
].join("");

test("the layout measure reports each defect the rule names, by kind and by locator @pr", async ({
  page,
}) => {
  const missed: string[] = [];
  for (const defect of defects) {
    const reading = await measure(page, defect.body, [defect.locator]);
    const said = reading.breaches.map((breach) => `${breach.kind} ${breach.locator}`);
    const listed = reading.breaches.every((breach) => breach.listed.includes(defect.locator));
    if (said.join(" | ") !== `${defect.kind} ${defect.locator}` || !listed) {
      missed.push(`${defect.name}: read as [${said.join(" | ")}]`);
    }
  }
  expect(missed, "defects the measure did not report as their one breach").toEqual([]);
});

test("the layout measure reports nothing of what the rule allows @pr", async ({ page }) => {
  const reading = await measure(page, allowed);
  expect(
    reading.breaches.map((breach) => `${breach.kind} ${breach.where}: ${breach.detail}`)
  ).toEqual([]);
  expect([...reading.groups].sort()).toEqual(["components", "labels"]);
});

test("a layout reading fails on a blank page, on a required group no element marks, and keeps a stale entry @pr", async ({
  page,
}) => {
  const place = { route: "control", width: 360 } as const;
  const blank = await measure(page, "<p>Loading…</p>");
  expect(judgeReading(place, blank).join("\n")).toContain("fewer than 100");

  const drawn = await measure(page, entries(120));
  expect(judgeReading(place, drawn, ["labels"]).join("\n")).toContain(
    'no element carries data-layout-group="labels"'
  );
  expect(judgeReading(place, drawn)).toEqual([]);

  const strip = defects[1];
  if (strip === undefined) throw new Error("the controls lost their box-scroll defect");
  const scrolled = await measure(page, `${strip.body}${entries(120)}`, [strip.locator]);
  const entry: KnownLayoutBreach = {
    element: strip.locator,
    kind: "box-scroll",
    reason: "D surfaces",
    route: "control",
    width: 360,
  };
  const elsewhere: KnownLayoutBreach = { ...entry, element: '[data-testid="gone"]' };
  expect(judgeReading(place, scrolled, [], [entry, elsewhere])).toEqual([]);
  expect(staleEntries([entry, elsewhere])).toEqual(['control 360 box-scroll [data-testid="gone"]']);
});
