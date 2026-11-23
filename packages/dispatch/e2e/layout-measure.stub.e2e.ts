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
import { readdirSync } from "node:fs";
import { fileURLToPath } from "node:url";

import { expect, type Page } from "@playwright/test";

import type { KnownLayoutBreach } from "./layout-breaches";
import {
  judgeReading,
  PINNED_FACE_SHEET,
  PINNED_FAMILIES,
  pinFontFaces,
  staleEntries,
} from "./layout-check";
import { type LayoutBreach, type LayoutReading, measureLayout } from "./layout-measure";
import { openCappedGroups } from "./layout-routes";
import { stubbed } from "./stub/stub-test";

// No row takes a `scene`, so no seed ever runs and no recording is read or written.
const test = stubbed(async () => ({}));

/** Words enough to run past any box below. */
const LONG = "a line of words much wider than the box it is drawn in, with no place to break it";

/** An 80-character path with no space, which no line of a narrow box can hold; the browser may
 *  break it at its hyphens. */
const PATH = "packages/dispatch/e2e/acceptance/converted-verification-check-with-a-long-path.sh";

/** A 75-character path with no space and no hyphen, which the browser does not break. */
const BARE_PATH = "packages/dispatch/e2e/acceptance/convertedverificationcheckwithalongpath.sh";

/** A one-pixel picture, drawn as wide as an `img` is told to be. */
const PIXEL =
  "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVQIHWP4z8DwHwAFgAI/ScL0eAAAAABJRU5ErkJggg==";

/** `count` spans, each one entry of a group. */
function entries(count: number): string {
  return Array.from({ length: count }, (_, index) => `<span>entry ${index + 1}</span>`).join(" ");
}

/** What each failure of `judgeReading` names, so a row holds the judgement and not its wording:
 *  `unaccepted` and the place, kind and locator of the entry that would accept a breach no entry
 *  accepts, or `entry` and the key of an entry its reading did not match exactly. */
function failureKeys(failures: readonly string[]): string[] {
  return failures.map((failure) => {
    const unlisted =
      /^\{ route: "([^"]+)", (?:state: "([^"]+)", )?width: (\d+), kind: "([^"]+)", element: ("(?:[^"\\]|\\.)*")/.exec(
        failure
      );
    if (unlisted === null) return `entry ${failure.slice(0, failure.indexOf(": "))}`;
    const [, route, state, width, kind, element] = unlisted;
    return `unaccepted ${route}${state === undefined ? "" : `#${state}`} ${width} ${kind} ${JSON.parse(element ?? '""')}`;
  });
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
    // The Inbox card row's reply chip on production at 400 px: a pill that starts its truncating
    // span's line and is wider than the span, so it is pushed out of its row (point 2 of the
    // rule); the span's ellipsis stands in for text, not for a control.
    body:
      '<span style="display: block; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; width: 150px">' +
      '<span data-testid="chip" style="border-radius: 9999px; display: inline-flex">' +
      "Legion architect · INFRA-2969 replied</span></span>",
    kind: "clipped",
    locator: '[data-testid="chip"]',
    name: "a chip wider than the truncating box whose line it starts",
  },
  {
    // A row laid out from its end: the first of two 300 px buttons ends up left of the viewport,
    // where no scroll reaches, and the document is no wider than the viewport.
    body:
      '<div style="display: flex; justify-content: flex-end; width: 340px">' +
      '<button data-testid="first" style="flex-shrink: 0; width: 300px" type="button">first</button>' +
      '<button style="flex-shrink: 0; width: 300px" type="button">second</button></div>',
    kind: "clipped",
    locator: '[data-testid="first"]',
    name: "an element left of the viewport, which no scroll reaches",
  },
  {
    // A fixed box that runs past the viewport's right edge only: it is cut there, and a fixed
    // layer adds nothing to the page's width, so no page scroll reaches it.
    body: '<div data-testid="layer-box" style="height: 20px; left: 100px; position: fixed; top: 0; width: 400px"></div>',
    kind: "clipped",
    locator: '[data-testid="layer-box"]',
    name: "a box on a fixed layer past the viewport's right edge",
  },
  {
    // A two-line clamp whose first line is an 80-character path: the clamp draws its ellipsis
    // on its last line only, so the path is cut bare.
    body:
      '<p data-testid="clamp" style="-webkit-box-orient: vertical; -webkit-line-clamp: 2; display: -webkit-box; overflow: hidden; width: 200px">' +
      `${PATH} and the words of a second line that the clamp ends with its ellipsis</p>`,
    kind: "clipped",
    locator: '[data-testid="clamp"]',
    name: "a line-clamped box that cuts a line above its last",
  },
  {
    // A two-line clamp that holds "The first line, then" and a path with no space and no
    // hyphen: the text needs no more than its two lines, so the clamp hides none, draws no
    // ellipsis, and the path on its last line is cut bare.
    body:
      '<p data-testid="clamp-hides-none" style="-webkit-box-orient: vertical; -webkit-line-clamp: 2; display: -webkit-box; overflow: hidden; width: 200px">' +
      `The first line, then ${BARE_PATH}</p>`,
    kind: "clipped",
    locator: '[data-testid="clamp-hides-none"]',
    name: "a line-clamped box that hides no line and cuts its last",
  },
  {
    // A strip of pictures each narrower than the box: a row, which scrolls as one.
    body:
      '<div data-testid="thumbs" style="display: flex; overflow-x: auto; width: 200px">' +
      `${`<img alt="" height="40" src="${PIXEL}" style="flex-shrink: 0" width="40">`.repeat(12)}</div>`,
    kind: "box-scroll",
    locator: '[data-testid="thumbs"]',
    name: "a strip of pictures each narrower than the box that scrolls for them",
  },
  {
    // Three pictures each wider than the box, side by side: a row, which scrolls as one.
    body:
      '<div data-testid="wide-strip" style="display: flex; overflow-x: auto; width: 200px">' +
      `${`<img alt="" height="10" src="${PIXEL}" style="flex-shrink: 0" width="250">`.repeat(3)}</div>`,
    kind: "box-scroll",
    locator: '[data-testid="wide-strip"]',
    name: "a strip of pictures each wider than the box that scrolls for them",
  },
  {
    // One picture narrower than the box, pushed past its edge by the margin before it: the box
    // scrolls for the row, not for a picture too wide for it.
    body:
      '<div data-testid="pushed-picture" style="overflow-x: auto; width: 200px">' +
      `<img alt="" height="10" src="${PIXEL}" style="display: block; margin-left: 100px" width="150"></div>`,
    kind: "box-scroll",
    locator: '[data-testid="pushed-picture"]',
    name: "one picture narrower than the box, pushed past its edge",
  },
  {
    // A chip, then a picture wider than the box: the box scrolls for the row the chip begins.
    body:
      '<div data-testid="chip-then-picture" style="display: flex; overflow-x: auto; width: 200px">' +
      '<span style="flex-shrink: 0; width: 150px">a chip</span>' +
      `<img alt="" height="10" src="${PIXEL}" style="flex-shrink: 0" width="250"></div>`,
    kind: "box-scroll",
    locator: '[data-testid="chip-then-picture"]',
    name: "a chip, then a picture wider than the box",
  },
  {
    // A narrow picture, then a wide one: only the wide one runs past, but the box scrolls for both.
    body:
      '<div data-testid="narrow-then-wide" style="display: flex; overflow-x: auto; width: 200px">' +
      `<img alt="" height="10" src="${PIXEL}" style="flex-shrink: 0" width="50">` +
      `<img alt="" height="10" src="${PIXEL}" style="flex-shrink: 0" width="250"></div>`,
    kind: "box-scroll",
    locator: '[data-testid="narrow-then-wide"]',
    name: "a narrow picture, then a wide one",
  },
  {
    // A wide picture pushed further by a large margin: the box scrolls past what the picture needs.
    body:
      '<div data-testid="wide-pushed" style="display: flex; overflow-x: auto; width: 200px">' +
      `<img alt="" height="10" src="${PIXEL}" style="flex-shrink: 0; margin-left: 300px" width="250"></div>`,
    kind: "box-scroll",
    locator: '[data-testid="wide-pushed"]',
    name: "a wide picture pushed further by its margin",
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
  // A clamp whose path, broken at its hyphens, runs onto a third line, which the clamp hides: it
  // draws its ellipsis on its last visible line, so the cut there is marked.
  `<p style="-webkit-box-orient: vertical; -webkit-line-clamp: 2; display: -webkit-box; overflow: hidden; width: 200px">The first line, then ${PATH}</p>`,
  // A fixed layer inside the viewport, and one wholly left of it as a closed drawer is.
  '<div style="left: 20px; position: fixed; top: 20px; width: 300px">A dialog inside the viewport</div>',
  '<div style="left: 0; position: fixed; top: 0; transform: translateX(-100%); width: 300px">A closed drawer</div>',
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

/** The rules of the built sheets that set a font stack the pin does not open, each with why the
 *  page never draws in it: the stack is overridden, or it is a face the build ships itself. */
const OVERRIDDEN_STACKS: readonly { selector: string; property: string; why: string }[] = [
  {
    property: "--font-sans",
    selector: ":root, :host",
    why: "Tailwind's theme; the pin sheet's :root sets it",
  },
  {
    property: "--font-mono",
    selector: ":root, :host",
    why: "Tailwind's theme; the pin sheet's :root sets it",
  },
  {
    property: "--font-family",
    selector: ".proof-editor",
    why: "the editor's own; .dispatch-doc .proof-editor sets it to inherit, and both editor hosts are .dispatch-doc",
  },
  {
    property: "--font-sans",
    selector: "html:has([data-issue-page])",
    why: "the new issue page's, which opens with the Inter the build ships (Dispatch Inter)",
  },
];

/** Every font stack the style rules of the page's sheets set - `font-family`, and the custom
 *  properties that hold a stack (`--font-*`, `--default-*font-family`) - whose first family is
 *  not one of `pinned`, written as `selector { property: value }`. A value that is a keyword
 *  (`inherit`, `revert-layer`, ...) or a `var()` names no family of its own. The walk goes into
 *  `@layer`, `@media`, `@supports` and nested rules. Runs in the page. */
function unpinnedStacks(pinned: readonly string[]): string[] {
  const found: string[] = [];
  const keyword = /^(inherit|initial|unset|revert|revert-layer)$/;
  const read = (rule: CSSStyleRule, property: string): void => {
    const value = rule.style.getPropertyValue(property).trim();
    if (value === "" || keyword.test(value) || value.startsWith("var(")) return;
    const first =
      value
        .split(",")[0]
        ?.trim()
        .replace(/^["']|["']$/g, "") ?? "";
    if (!pinned.includes(first)) found.push(`${rule.selectorText} { ${property}: ${value} }`);
  };
  const walk = (rules: CSSRuleList): void => {
    for (const rule of rules) {
      if (rule instanceof CSSStyleRule) {
        read(rule, "font-family");
        for (const property of rule.style) {
          // A family variable (`--font-sans`, `--font-family`, `--default-mono-font-family`), not
          // a size, a weight (`--font-weight-bold`) or a style.
          if (
            /^--(font-(?!size$|weight$|style$|stretch$)[a-z]+|default-(mono-)?font-family)$/.test(
              property
            )
          ) {
            read(rule, property);
          }
        }
      }
      if ("cssRules" in rule && rule.cssRules instanceof CSSRuleList) walk(rule.cssRules);
    }
  };
  for (const sheet of document.styleSheets) walk(sheet.cssRules);
  return found;
}

// Every font stack the app's built sheets set opens with a family the layout rows declare a
// pinned face for, or is one of the overridden rules named above, so no browser - a Mac's
// included, which resolves a keyword such as `ui-monospace` or `-apple-system` itself - draws a
// reading's text in a face of its own. The row walks every rule of the sheets the rows load
// (`web/dist/assets`), reads the computed stack of the root's text, Tailwind's `font-mono` and
// `font-sans`, the `code` that takes `font-mono`, the editor's code and a document's words, and
// shows that a sheet that adds an unpinned stack fails it.
test("every font stack the app sets opens with a pinned face @pr", async ({ page }) => {
  const assets = fileURLToPath(new URL("../web/dist/assets/", import.meta.url));
  const sheets = readdirSync(assets).filter((name) => name.endsWith(".css"));
  expect(sheets.length, `no built sheet under ${assets}: build the SPA first`).toBeGreaterThan(0);
  await page.setContent(
    '<!doctype html><html><body><p data-font="root">words</p>' +
      '<p class="font-mono" data-font="font-mono">mono</p><code data-font="code">code</code>' +
      '<p class="font-sans" data-font="font-sans">sans</p>' +
      '<div class="milkdown"><code data-font="editor code">code</code></div>' +
      '<div class="dispatch-doc"><div class="proof-editor"><p data-font="document">words</p></div></div>' +
      "</body></html>"
  );
  for (const sheet of sheets) await page.addStyleTag({ path: `${assets}${sheet}` });
  await page.addStyleTag({ content: PINNED_FACE_SHEET });
  const overridden = OVERRIDDEN_STACKS.map(
    ({ selector, property }) => `${selector} { ${property}: `
  );
  const unexplained = (found: readonly string[]) =>
    found.filter((stack) => !overridden.some((rule) => stack.startsWith(rule)));
  const found = await page.evaluate(unpinnedStacks, [...PINNED_FAMILIES]);
  expect(unexplained(found)).toEqual([]);
  // Each overridden rule named above is still in the sheets: one that left them goes from the list.
  expect(overridden.filter((rule) => !found.some((stack) => stack.startsWith(rule)))).toEqual([]);
  const firsts = await page.evaluate(() =>
    [...document.querySelectorAll("[data-font]")].map((element) => [
      element.getAttribute("data-font"),
      getComputedStyle(element).fontFamily.split(",")[0]?.trim().replace(/^"|"$/g, ""),
    ])
  );
  expect(firsts).toHaveLength(6);
  expect(firsts.filter(([, first]) => !PINNED_FAMILIES.includes(first ?? ""))).toEqual([]);
  // A sheet that adds a stack of its own, as a new library's would, is read and refused.
  await page.addStyleTag({
    content: "@layer extra { .new-stack { font-family: Georgia, serif; } }",
  });
  expect(unexplained(await page.evaluate(unpinnedStacks, [...PINNED_FAMILIES]))).toEqual([
    ".new-stack { font-family: Georgia, serif }",
  ]);
});

// A page that scrolls sideways reads the same scrolled as not: the measure reads the elements
// the page holds in the document, so a window a state left scrolled to the right reports the one
// page-scroll breach and the same cut at the page's left edge.
test("a window scrolled sideways reads as it does unscrolled @pr", async ({ page }) => {
  const row =
    '<div data-testid="left-cut" style="height: 10px; margin-left: -60px; width: 100px"></div>' +
    '<div data-testid="row" style="display: flex; justify-content: space-between; width: 900px">' +
    `<span>near</span><span data-testid="far">far</span></div>${entries(120)}`;
  const said = (reading: LayoutReading) =>
    reading.breaches.map((breach) => `${breach.kind} ${breach.locator}`);
  const expected = ['page-scroll [data-testid="far"]', 'clipped [data-testid="left-cut"]'];
  const unscrolled = await measure(page, row);
  expect(said(unscrolled)).toEqual(expected);
  await page.evaluate(() => window.scrollTo(540, 0));
  expect(await page.evaluate(() => window.scrollX)).toBe(540);
  expect(said(await page.evaluate(measureLayout, []))).toEqual(expected);
});

// The rows require each pinned face to load (`pinFontFaces`, which any row that measures text may
// call): it passes once the faces load, and again after the next load, and a face the route
// answers with a 404 fails it, as a sheet that never reached the page would.
test("a layout reading fails when a pinned face does not load @pr", async ({ page }) => {
  const origin = "http://layout-fonts.test";
  await page.route(`${origin}/`, (route) =>
    route.fulfill({ body: "<!doctype html><p>words</p>", contentType: "text/html" })
  );
  await page.goto(`${origin}/`);
  await pinFontFaces(page);
  await page.goto(`${origin}/`);
  await pinFontFaces(page);
  await page.route("**/__layout-fonts/liberation/LiberationSans-Bold.ttf", (route) =>
    route.fulfill({ body: "gone", status: 404 })
  );
  await page.goto(`${origin}/`);
  await expect(pinFontFaces(page)).rejects.toThrow("Liberation Sans 700");
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
  const failures = judgeReading(place, scrolled, {}, [entry, elsewhere]);
  expect(failures).toHaveLength(1);
  expect(failureKeys(failures)).toEqual(['entry control 360 box-scroll [data-testid="gone"]']);
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
  const unaccepted = judgeReading(place, two, {}, [entry]);
  expect(unaccepted).toHaveLength(1);
  expect(failureKeys(unaccepted)).toEqual([
    'unaccepted counted 360 clipped [data-testid="region"]',
  ]);
  const one = await measure(page, region(1));
  const short = judgeReading(place, one, {}, [{ ...entry, count: 2 }]);
  expect(short).toHaveLength(1);
  expect(failureKeys(short)).toEqual(['entry counted 360 clipped [data-testid="region"]']);
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
  const unmarked = judgeReading(place, drawn, { groups: ["labels"] });
  expect(unmarked).toHaveLength(1);
  expect(failureKeys(unmarked)).toEqual(["unaccepted grouped 360 group-missing labels"]);
  expect(judgeReading(place, drawn, { groups: ["labels"] }, [missing])).toEqual([]);
  const marked = await measure(
    page,
    `<div data-layout-group="labels">${entries(3)}</div>${entries(120)}`
  );
  const unused = judgeReading(place, marked, { groups: ["labels"] }, [missing]);
  expect(unused).toHaveLength(1);
  expect(failureKeys(unused)).toEqual(["entry grouped 360 group-missing labels"]);

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

/** `count` closed groups of eleven, each with a "+1" control as `CappedGroup` draws it: it reads
 *  `aria-expanded="false"`, names its group in `aria-controls`, and opens it when pressed. */
function closedGroups(count: number): string {
  return Array.from(
    { length: count },
    (_, index) =>
      `<div data-layout-group="g${index + 1}" id="group-${index + 1}">${entries(11)}` +
      `<button aria-controls="group-${index + 1}" aria-expanded="false" data-layout-more type="button" ` +
      `onclick="this.parentElement.setAttribute('data-layout-open', ''); this.setAttribute('aria-expanded', 'true')">+1</button></div>`
  ).join("");
}

test("openCappedGroups opens every closed group, one press each, and stops at its bound @pr", async ({
  page,
}) => {
  await measure(page, closedGroups(3));
  expect(await openCappedGroups(page)).toBe(3);
  expect(await page.locator("[data-layout-group]:not([data-layout-open])").count()).toBe(0);
  expect(await openCappedGroups(page)).toBe(0);

  // Kept to one group by name, it leaves the others closed.
  await measure(page, closedGroups(3));
  expect(await openCappedGroups(page, { group: "g2" })).toBe(1);
  expect(await page.locator("[data-layout-group]:not([data-layout-open])").count()).toBe(2);

  // Past its bound it throws rather than pressing on.
  await measure(page, closedGroups(3));
  await expect(openCappedGroups(page, { maxPresses: 2 })).rejects.toThrow("still closed");
});
