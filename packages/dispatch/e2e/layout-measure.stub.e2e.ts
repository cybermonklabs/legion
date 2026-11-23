/** The layout instrument's own controls: `measureLayout` (`e2e/layout-measure.ts`) and
 *  `judgeReading` (`e2e/layout-check.ts`) read small pages this file writes into the browser with
 *  `page.setContent`, so the rows need no answer at all - no recording, no server, no seed - and
 *  run on every pull request with the stubbed layer (packages/dispatch/README.md, "The stubbed
 *  layer"). Each defect the layout rule names is drawn once and must be reported, by kind and by
 *  the locator a breach list entry would name; each case the rule allows is drawn beside the
 *  others and must report nothing; and the judgement is held to its counts, its required groups,
 *  its floor and its stale entries. An instrument whose clean reading has never been seen to fail
 *  proves nothing, and these are the failures it is seen to report: each part below fails one of
 *  these rows when it is broken. */
import { expect, type Page } from "@playwright/test";

import type { KnownLayoutBreach } from "./layout-breaches";
import { judgeReading, staleEntries } from "./layout-check";
import { type LayoutBreach, type LayoutReading, measureLayout } from "./layout-measure";
import { stubbed } from "./stub/stub-test";

// No row takes a `scene`, so no seed ever runs and no recording is read or written.
const test = stubbed(async () => ({}));

/** Words enough to run past any box below. */
const LONG = "a line of words much wider than the box it is drawn in, with no place to break it";

/** A one-pixel picture, drawn as wide as an `img` is told to be. */
const PIXEL =
  "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVQIHWP4z8DwHwAFgAI/ScL0eAAAAABJRU5ErkJggg==";

/** `count` spans, each one entry of a group. */
function entries(count: number): string {
  return Array.from({ length: count }, (_, index) => `<span>entry ${index + 1}</span>`).join(" ");
}

/** Draws `body` as a page's own region at a phone's width, and reads it as a layout row reads a
 *  page. */
async function measure(
  page: Page,
  body: string,
  listed: readonly string[] = []
): Promise<LayoutReading> {
  await page.setViewportSize({ height: 900, width: 360 });
  await page.setContent(
    `<!doctype html><html><body style="margin: 0"><main data-testid="main-content">${body}</main></body></html>`
  );
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
    // The box's own loose text runs past it beside a data table that does too: the table is
    // exempt and the text is not.
    body:
      `<div data-testid="loose" style="overflow-x: auto; white-space: nowrap; width: 200px">${LONG}` +
      `<table><tr><td style="white-space: nowrap">${LONG}</td></tr></table></div>`,
    kind: "box-scroll",
    locator: '[data-testid="loose"]',
    name: "a box that scrolls for its own text beside its data table",
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
    // A truncating block wider than the box that cuts it: its ellipsis is drawn at its own edge,
    // which the box cuts away with the rest.
    body:
      '<div style="overflow: hidden; width: 100px">' +
      '<div data-testid="own-ellipsis" style="overflow: hidden; text-overflow: ellipsis; white-space: nowrap; width: 300px">' +
      `${LONG}</div></div>`,
    kind: "clipped",
    locator: '[data-testid="own-ellipsis"]',
    name: "a truncating block its box cuts, ellipsis and all",
  },
  {
    body: `<div data-testid="cut-text" style="overflow: hidden; white-space: nowrap; width: 100px">${LONG}</div>`,
    kind: "clipped",
    locator: '[data-testid="cut-text"]',
    name: "a line of text its own box cuts with no ellipsis",
  },
  {
    // The Inbox card row's reply chip on production at 400 px: a pill inside a truncating span,
    // cut at the span's edge with no ellipsis, since an ellipsis stands in for text alone.
    body:
      '<span style="display: block; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; width: 150px">' +
      'Reply <span data-testid="chip" style="border-radius: 9999px; display: inline-flex">' +
      "Legion architect · INFRA-2969 replied</span></span>",
    kind: "clipped",
    locator: '[data-testid="chip"]',
    name: "a chip a truncating box cuts, which shows no ellipsis",
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
  {
    // A group that raises its own cap past the rule's ten, and shows as many as it declares.
    body: `<div data-layout-group="checks" data-layout-max="11">${entries(11)}</div>`,
    kind: "group-over-cap",
    locator: '[data-layout-group="checks"]',
    name: "a group declaring a cap of eleven and showing eleven",
  },
  {
    body: `<div data-layout-group="roles" data-layout-max="abc">${entries(2)}</div>`,
    kind: "group-over-cap",
    locator: '[data-layout-group="roles"]',
    name: "a group declaring a cap that is no number",
  },
  {
    body: `<div data-layout-group="options" data-layout-max="0">${entries(0)}</div>`,
    kind: "group-over-cap",
    locator: '[data-layout-group="options"]',
    name: "a group declaring a cap of nought",
  },
];

/** Every case the rule allows, on one page: none of them may be reported. */
const allowed = [
  `<span style="display: block; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; width: 100px">${LONG}</span>`,
  `<span style="display: block; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; width: 100px">Opened by <a href="#x">${LONG}</a></span>`,
  `<pre style="overflow-x: auto; width: 200px">${LONG}</pre>`,
  `<div style="overflow-x: auto; width: 200px"><pre>${LONG}</pre></div>`,
  `<div style="overflow-x: auto; width: 200px"><table><tr><td style="white-space: nowrap">${LONG}</td></tr></table></div>`,
  '<div style="overflow-x: auto; width: 200px"><div role="grid" style="height: 20px; width: 400px"></div></div>',
  `<div style="overflow-x: auto; width: 200px"><img alt="" height="10" src="${PIXEL}" style="display: block" width="400"></div>`,
  // An icon's own drawing past its box: the svg cuts it, as a picture may.
  '<svg height="20" width="20"><rect height="10" width="40" x="-10" y="0"></rect></svg>',
  '<div data-layout-scroll="board" style="overflow-x: auto; width: 200px"><div style="display: flex; height: 20px; width: 800px"></div></div>',
  `<div data-layout-group="labels" data-layout-open>${entries(15)}<button data-layout-more type="button">Show fewer</button></div>`,
  `<div data-layout-group="components">${entries(10)}<button data-layout-more type="button">+5</button></div>`,
  `<div style="position: relative">A label<span style="clip: rect(0, 0, 0, 0); height: 1px; overflow: hidden; position: absolute; white-space: nowrap; width: 1px">${LONG}</span></div>`,
  // The shell's skip link: Tailwind's `sr-only`, given a touch target's size by the shell.
  `<a href="#main-content" style="clip-path: inset(50%); height: 44px; overflow: hidden; position: absolute; white-space: nowrap; width: 44px">Skip to content, ${LONG}</a>`,
  `<div style="overflow: hidden; width: 200px"><details><summary>More</summary><div style="width: 400px">${LONG}</div></details></div>`,
  `<p style="-webkit-box-orient: vertical; -webkit-line-clamp: 2; display: -webkit-box; overflow: hidden; width: 200px">${LONG} ${LONG}</p>`,
  `<div style="display: flex; flex-wrap: wrap; gap: 4px; width: 200px">${entries(12)}</div>`,
  entries(120),
].join("");

test("the layout measure reports each defect the rule names, by kind and by locator @pr", async ({
  page,
}) => {
  const missed: string[] = [];
  for (const defect of defects) {
    const reading = await measure(page, defect.body, [defect.locator]);
    const said = reading.breaches.map((breach) => `${breach.kind} ${breach.locator}`);
    if (said.join(" | ") !== `${defect.kind} ${defect.locator}`) {
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

test("a layout reading fails on a blank page and keeps a stale entry @pr", async ({ page }) => {
  const place = { route: "control", width: 360 } as const;
  const blank = await measure(page, "<p>Loading…</p>");
  expect(judgeReading(place, blank).join("\n")).toContain("fewer than 100");
  // A route that names a lower floor for itself is still refused below it.
  expect(judgeReading(place, blank, { minElements: 40 }).join("\n")).toContain("fewer than 40");
  // The shell does not count: a blank region inside a shell of 200 elements is still blank.
  await page.evaluate(() => {
    document.body.insertAdjacentHTML(
      "afterbegin",
      `<nav>${"<span>shell</span>".repeat(200)}</nav>`
    );
  });
  expect(judgeReading(place, await page.evaluate(measureLayout, [])).join("\n")).toContain(
    "fewer than 100"
  );

  const drawn = await measure(page, entries(120));
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
  expect(judgeReading(place, scrolled, {}, [entry, elsewhere])).toEqual([
    'control 360 box-scroll [data-testid="gone"]: the entry accepts 1, and this reading had 0',
  ]);
  expect(staleEntries([entry, elsewhere])).toEqual(['control 360 box-scroll [data-testid="gone"]']);
});

test("a layout entry accepts its own count of breaches and no new one in its region @pr", async ({
  page,
}) => {
  const place = { route: "counted", width: 360 } as const;
  // Two cut blocks in one named region, whose nearest name is the region's.
  const cut = '<div style="height: 20px; width: 400px">a block wider than its box</div>';
  const region = (cuts: number) =>
    `<div data-testid="region" style="overflow: hidden; width: 200px">${cut.repeat(cuts)}</div>${entries(120)}`;
  const entry: KnownLayoutBreach = {
    element: '[data-testid="region"]',
    kind: "clipped",
    reason: "D surfaces",
    route: "counted",
    width: 360,
  };
  const two = await measure(page, region(2));
  expect(judgeReading(place, two, {}, [{ ...entry, count: 2 }])).toEqual([]);
  // A new breach in the listed region fails, and so does an entry with fewer breaches than its
  // count: each entry is one breach, or the number it says.
  expect(judgeReading(place, two, {}, [entry]).join("\n")).toContain(
    'kind: "clipped", element: "[data-testid=\\"region\\"]" }'
  );
  const one = await measure(page, region(1));
  expect(judgeReading(place, one, {}, [{ ...entry, count: 2 }])).toEqual([
    'counted 360 clipped [data-testid="region"]: the entry accepts 2, and this reading had 1',
  ]);
});

test("a layout entry names a required group no element marks, and a page-scroll breach takes its own entry first @pr", async ({
  page,
}) => {
  const place = { route: "grouped", width: 360 } as const;
  const drawn = await measure(page, entries(120));
  const missing: KnownLayoutBreach = {
    element: "labels",
    kind: "group-missing",
    reason: "B issue page",
    route: "grouped",
    width: 360,
  };
  expect(judgeReading(place, drawn, { groups: ["labels"] }).join("\n")).toContain(
    'kind: "group-missing", element: "labels" } no element carries data-layout-group="labels"'
  );
  expect(judgeReading(place, drawn, { groups: ["labels"] }, [missing])).toEqual([]);
  const marked = await measure(
    page,
    `<div data-layout-group="labels">${entries(3)}</div>${entries(120)}`
  );
  expect(judgeReading(place, marked, { groups: ["labels"] }, [missing])).toEqual([
    "grouped 360 group-missing labels: the entry accepts 1, and this reading had 0",
  ]);

  // Two elements past the viewport inside one region: one named for itself, one named for the
  // region. With the region's entry listed first, the named one must still take its own entry.
  const deep = '[data-testid="deep"]';
  const wide = '[data-testid="region"]';
  const scrolled = await measure(
    page,
    `<div data-testid="region"><div data-testid="deep" style="height: 10px; width: 900px"></div>` +
      `<div style="height: 10px; width: 900px"></div></div>${entries(120)}`,
    [wide, deep]
  );
  const both = [wide, deep].map(
    (element): KnownLayoutBreach => ({
      element,
      kind: "page-scroll",
      reason: "D surfaces",
      route: "grouped",
      width: 360,
    })
  );
  expect(judgeReading(place, scrolled, {}, both)).toEqual([]);
});
