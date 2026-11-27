/** The layout instrument's own controls: `measureLayout` (`e2e/layout-measure.ts`) and
 *  `judgeReading` (`e2e/layout-check.ts`) read small pages this file writes into the browser with
 *  `page.setContent`, so the rows need no answer at all - no recording, no server, no seed - and
 *  run on every pull request with the stubbed layer (packages/dispatch/README.md, "The stubbed
 *  layer"). Each defect the layout rule names is drawn once and must be reported, by kind and by
 *  the locator a breach list entry would name; each case the rule allows is drawn beside the
 *  others and must report nothing; and the judgement is held to its counts, its required groups
 *  and its floor. An instrument whose clean reading has never been seen to fail
 *  proves nothing, and these are the failures it is seen to report: each part below fails one of
 *  these rows when it is broken. */
import { readdirSync } from "node:fs";
import { fileURLToPath } from "node:url";

import { expect, type Page, type Route } from "@playwright/test";

import type { KnownLayoutBreach } from "./layout-breaches";
import {
  judgeReading,
  type LayoutFailure,
  PINNED_FACE_SHEET,
  PINNED_FAMILIES,
  pinFontFaces,
} from "./layout-check";
import { type LayoutBreach, type LayoutReading, measureLayout } from "./layout-measure";
import { openCappedGroups } from "./layout-routes";
import { ONE_PIXEL_PNG } from "./one-pixel-png";
import { trackDocumentRequests, waitForRouteSettled } from "./review-routes";
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
const PIXEL = `data:image/png;base64,${ONE_PIXEL_PNG}`;

/** `count` spans, each one entry of a group. */
function entries(count: number): string {
  return Array.from({ length: count }, (_, index) => `<span>entry ${index + 1}</span>`).join(" ");
}

/** What names each failure of `judgeReading`, without its message: its type and its place, and
 *  for a breach no entry accepts its kind, locator and count, or for an entry its reading did not
 *  match the entry's own place, kind and locator and how many it matched. */
function failureIdentities(failures: readonly LayoutFailure[]): unknown[] {
  return failures.map((failure) => {
    switch (failure.type) {
      case "unaccepted":
        return {
          count: failure.count,
          element: failure.element,
          kind: failure.kind,
          place: failure.place,
          type: failure.type,
        };
      case "entry-mismatch":
        return {
          element: failure.entry.element,
          kind: failure.entry.kind,
          matched: failure.matched,
          place: {
            route: failure.entry.route,
            state: failure.entry.state,
            width: failure.entry.width,
          },
          type: failure.type,
        };
      default:
        return { place: failure.place, type: failure.type };
    }
  });
}

/** Draws `body` as a page's own region at a phone's width, and reads it as a layout row reads a
 *  page. */
async function measure(page: Page, body: string): Promise<LayoutReading> {
  await page.setViewportSize({ height: 900, width: 360 });
  await page.setContent(
    `<!doctype html><html><body style="margin: 0"><main data-testid="main-content">${body}</main></body></html>`
  );
  return await page.evaluate(measureLayout);
}

/** A row of chips that scrolls sideways: the defect the judgement controls below list. */
const chipStrip = {
  body: `<div data-testid="strip" style="overflow-x: auto; white-space: nowrap; width: 200px">${entries(12)}</div>`,
  kind: "box-scroll",
  locator: '[data-testid="strip"]',
  name: "a row of chips that scrolls sideways",
} as const;

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
  chipStrip,
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
    // The chip inside the cut block, through a span, is cut because the block is: one breach.
    body:
      '<div style="overflow: hidden; width: 200px">' +
      '<div data-testid="cut" style="height: 20px; width: 400px">a block wider than its box ' +
      '<span><b style="display: inline-block; margin-left: 100px">and a chip</b></span></div></div>',
    kind: "clipped",
    locator: '[data-testid="cut"]',
    name: "a block cut off by the box that clips it, and the chip it holds",
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
  {
    // Three entries, each marked as a "+N" control as a copied list item would be: none of them
    // is counted, so the group would show any number while closed and pass.
    body:
      '<div data-layout-group="links">' +
      Array.from(
        { length: 3 },
        (_, index) => `<span data-layout-more>link ${index + 1}</span>`
      ).join("") +
      "</div>",
    kind: "group-over-cap",
    locator: '[data-layout-group="links"]',
    name: "a group with more than one child marked data-layout-more",
  },
  {
    // A code block may scroll its lines, not cut them.
    body: `<pre data-testid="cut-pre" style="overflow: hidden; width: 100px">${"x".repeat(100)}</pre>`,
    kind: "clipped",
    locator: '[data-testid="cut-pre"]',
    name: "a code block that cuts its text with no ellipsis",
  },
  {
    // An ellipsis stands in for text: a picture its box cuts loses what it does not stand for.
    body:
      '<div style="overflow: hidden; text-overflow: ellipsis; white-space: nowrap; width: 100px">' +
      `<img alt="" data-testid="cut-picture" height="10" src="${PIXEL}" width="300"></div>`,
    kind: "clipped",
    locator: '[data-testid="cut-picture"]',
    name: "a picture cut by a box whose ellipsis stands in for text",
  },
  {
    // A picture pushed out of its row by the chip before it, in a row with no ellipsis.
    body:
      '<div style="display: flex; overflow: hidden; width: 200px">' +
      '<span style="flex-shrink: 0; width: 150px">a chip</span>' +
      `<img alt="" data-testid="pushed-picture-out" height="10" src="${PIXEL}" style="flex-shrink: 0" width="100"></div>`,
    kind: "clipped",
    locator: '[data-testid="pushed-picture-out"]',
    name: "a picture pushed out of its row",
  },
  {
    // The cross-family review's probe: a fixed 100 px box whose own unspaced text runs past the
    // viewport, while the box itself fits.
    body: `<div data-testid="fixed-text" style="left: 10px; position: fixed; top: 0; white-space: nowrap; width: 100px">${"x".repeat(100)}</div>`,
    kind: "clipped",
    locator: '[data-testid="fixed-text"]',
    name: "a fixed box's own text that runs past the viewport",
  },
  {
    // A strip of chips in a table's cell: the table may scroll in its own box, the strip may not.
    body:
      '<table><tr><td><div data-testid="cell-strip" style="overflow-x: auto; width: 100px">' +
      '<span style="display: inline-block; width: 300px">a strip of chips</span></div></td></tr></table>',
    kind: "box-scroll",
    locator: '[data-testid="cell-strip"]',
    name: "a strip of chips that scrolls in a table's cell",
  },
  {
    // A strip of chips in a card of a board's lanes: the lanes may scroll, the card's strip may not.
    body:
      '<div data-layout-scroll="board" style="overflow-x: auto; width: 300px">' +
      '<div data-testid="card-strip" style="overflow-x: auto; width: 100px">' +
      '<span style="display: inline-block; width: 300px">a strip of chips</span></div></div>',
    kind: "box-scroll",
    locator: '[data-testid="card-strip"]',
    name: "a strip of chips that scrolls in a board's card",
  },
  {
    // A chip inside an inline span the box's ellipsis excuses: the span's text is the ellipsis's
    // to stand in for, the chip is still pushed out of its row.
    body:
      '<span style="display: block; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; width: 150px">' +
      '<span>Reply from a long sender name <span data-testid="chip-in-span" style="display: inline-flex">' +
      "Legion architect · INFRA-2969 replied</span></span></span>",
    kind: "clipped",
    locator: '[data-testid="chip-in-span"]',
    name: "a chip inside an inline span a truncating box excuses",
  },
  {
    // Text in a block child of a truncating box: the box's ellipsis stands in for its own inline
    // text alone, so the block's line is cut bare.
    body:
      '<div style="overflow: hidden; text-overflow: ellipsis; white-space: nowrap; width: 100px">' +
      `<div data-testid="block-text">${"x".repeat(100)}</div></div>`,
    kind: "clipped",
    locator: '[data-testid="block-text"]',
    name: "text in a block child of a truncating box",
  },
  {
    // A span in a block child of a truncating box: it draws in the block's lines, which the box's
    // ellipsis does not reach (`text-overflow` is not inherited), so it is cut bare.
    body:
      '<div style="overflow: hidden; text-overflow: ellipsis; white-space: nowrap; width: 100px">' +
      `<div><span data-testid="span-in-block">${LONG}</span></div></div>`,
    kind: "clipped",
    locator: '[data-testid="span-in-block"]',
    name: "a span in a block child of a truncating box",
  },
  {
    // The same in a flex row: the `min-w-0` item shrinks to the row, and its span draws in the
    // item's lines; a flex container has no lines for its ellipsis.
    body:
      '<div style="display: flex; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; width: 100px">' +
      `<div style="min-width: 0"><span data-testid="span-in-flex-item">${LONG}</span></div></div>`,
    kind: "clipped",
    locator: '[data-testid="span-in-flex-item"]',
    name: "a span in a min-width-0 flex item of a truncating box",
  },
  {
    // A highlighted line of a code block whose overflow is visible: the block cuts nothing, so the
    // box around it cuts the highlight's span, and that box's ellipsis does not reach the block's
    // lines.
    body:
      '<div style="overflow: hidden; text-overflow: ellipsis; white-space: nowrap; width: 100px">' +
      `<pre style="margin: 0"><span data-testid="highlight-in-pre">${LONG}</span></pre></div>`,
    kind: "clipped",
    locator: '[data-testid="highlight-in-pre"]',
    name: "a highlighted span of a code block a truncating box cuts",
  },
];

/** Every case the rule allows, on one page: none of them may be reported. */
const allowed = [
  `<span style="display: block; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; width: 100px">${LONG}</span>`,
  `<span style="display: block; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; width: 100px">Opened by <a href="#x">${LONG}</a></span>`,
  `<pre style="overflow-x: auto; width: 200px">${LONG}</pre>`,
  `<div style="overflow-x: auto; width: 200px"><pre>${LONG}</pre></div>`,
  // A highlighted code block that scrolls its own lines: its spans are its own to scroll.
  `<pre style="overflow-x: auto; width: 200px"><span>${LONG}</span> <span>${LONG}</span></pre>`,
  // A truncating block inside a truncating box, Tailwind's most common nested truncation: the
  // inner block clips its own text and draws its own ellipsis.
  `<div style="overflow: hidden; text-overflow: ellipsis; white-space: nowrap; width: 100px"><div style="overflow: hidden; text-overflow: ellipsis">${LONG}</div></div>`,
  // A link in a wrapper that draws no box (`display: contents`): it draws in the box's own lines.
  `<span style="display: block; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; width: 100px"><span style="display: contents"><a href="#x">${LONG}</a></span></span>`,
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
  // A fixed layer inside the viewport, one wholly left of it as a closed drawer is, a fixed box
  // whose own text wraps inside it, and one that ends its own text with an ellipsis.
  '<div style="left: 20px; position: fixed; top: 20px; width: 300px">A dialog inside the viewport</div>',
  '<div style="left: 0; position: fixed; top: 0; transform: translateX(-100%); width: 300px">A closed drawer</div>',
  `<div style="left: 10px; position: fixed; top: 60px; width: 100px">${LONG}</div>`,
  `<div style="left: 10px; overflow: hidden; position: fixed; text-overflow: ellipsis; top: 160px; white-space: nowrap; width: 100px">${"x".repeat(100)}</div>`,
  `<div style="display: flex; flex-wrap: wrap; gap: 4px; width: 200px">${entries(12)}</div>`,
  entries(120),
].join("");

test("the layout measure reports each defect the rule names, by kind and by locator @pr", async ({
  page,
}) => {
  const missed: string[] = [];
  for (const defect of defects) {
    const reading = await measure(page, defect.body);
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
  expect(said(await page.evaluate(measureLayout))).toEqual(expected);
});

// The rows require each pinned face to load (`pinFontFaces`, which any row that measures text may
// call): it passes once the faces load, and again after the next load, and a face the route
// answers with a 404 fails it, as a sheet that never reached the page would. And a page can
// replace its document after `page.goto` returns: the app reloads when a lazy chunk fails to
// download, which a loaded box provokes (`net::ERR_NETWORK_CHANGED`, seen in a trace of the
// agents row). The layout sheet reaches the new document without another call, so the reading
// taken there is still in the pinned faces and with no transitions.
test("a pinned face that does not load fails a reading, and the layout sheet reaches a document the page replaces @pr", async ({
  page,
}) => {
  const origin = "http://layout-fonts.test";
  await page.route(`${origin}/`, (route) =>
    route.fulfill({
      body: '<!doctype html><p style="transition: opacity 2s">words</p>',
      contentType: "text/html",
    })
  );
  await page.goto(`${origin}/`);
  await pinFontFaces(page);

  const firstDocument = await page.evaluate(() => performance.timeOrigin);
  await Promise.all([page.waitForEvent("load"), page.evaluate(() => window.location.reload())]);
  expect(await page.evaluate(() => performance.timeOrigin)).not.toBe(firstDocument);
  const sheet = await page.evaluate(() => {
    const style = document.getElementById("layout-pinned-sheet");
    return {
      fontFaces: style?.textContent?.match(/@font-face/g)?.length ?? 0,
      noTransitions: style?.textContent?.includes("transition: none !important") ?? false,
      transition: getComputedStyle(document.querySelector("p") as Element).transitionDuration,
    };
  });
  expect(sheet).toEqual({ fontFaces: 10, noTransitions: true, transition: "0s" });

  await page.goto(`${origin}/`);
  await pinFontFaces(page);
  await page.route("**/__layout-fonts/liberation/LiberationSans-Bold.ttf", (route) =>
    route.fulfill({ body: "gone", status: 404 })
  );
  await page.goto(`${origin}/`);
  await expect(pinFontFaces(page)).rejects.toThrow("Liberation Sans 700");
});

/** How long the settle row holds the shown document's own Inbox read before it answers it. */
const SHOWN_READ_HOLD_MS = 1500;

// The settle wait counts the Inbox number's reads of the document the page shows, and of no
// document the page replaced: a replaced document's reads stay in `page.requests()` answered by
// nothing and failed by nothing, and a wait that counted them never settled (dispatch://INFRA-5200:
// 10 of 80 repeats of the issue-spec-outline row under address changes). The replaced document
// here leaves two such reads: one sent before its successor's navigation request, and one sent
// after it and before the successor commits. The shown document's own read is held, and the wait
// settles only once that read has its answer.
//
// The bound. The wait polls every 100 ms, and one poll - reading `inFlight` and the Inbox number
// - costs d, measured on this box at a load average of about 500 over 934 polls of these rows:
// a median of 9-14 ms, a 90th percentile of 24-28, a 99th of 57-73 and a maximum of 93. The wait
// restarts its 500 ms window at the end of the last poll that saw the shown read in flight. That
// poll began before the read's answer reached the tracker, and the next one, at most 100 + d
// later, saw no read; so the window starts after answeredAt - 100 - d, and a wait that counts the
// read settles no earlier than answeredAt + 400 - d. A wait that did not count it would settle
// 500 ms and at most one poll after it starts, about 600 + d ms into the 1,500 ms hold, so about
// 900 ms before the answer. The bound of 250 stands between the two: it holds for every d up to
// 150 ms, well past the slowest poll measured, and a wait that ignored the read would miss it by
// about 1,150 ms. (A hold of 600 ms with a bound of 500 put the bound at the top of the first
// range, 400 + 4d to 500 + 5d as the poll's phase falls: CI read 492, dispatch-browser-pr shard 2
// of run 45450047809.)
//
// Tagged `@pr`, as every stubbed row is (`e2e/__tests__/pull-request-lane.test.ts`: a stubbed row
// the pull-request lane does not schedule is a row whose cost was paid and whose feedback was
// given away). That lane is at the 350 rows dispatch://INFRA-1407's R1 allows.
// SLOW(>=2s): a page held mid-reload, then its read held 1,500 ms and the 500 ms quiet after it.
test("the settle wait waits on the shown document's Inbox reads and on no replaced one's @pr", async ({
  page,
}) => {
  const origin = "http://layout-settle.test";
  const html = (read: string) =>
    `<!doctype html><main>A page</main><script>fetch("/api/v1/inbox?${read}")</script>`;
  // The first load is answered; the reload's document request is held, so the replaced document
  // is still there to send a read after its successor's navigation request.
  const successor = Promise.withResolvers<Route>();
  let documents = 0;
  await page.route(`${origin}/`, async (route) => {
    documents += 1;
    if (documents === 1) await route.fulfill({ body: html("before"), contentType: "text/html" });
    else successor.resolve(route);
  });
  const reads: Route[] = [];
  await page.route(`${origin}/api/v1/inbox?*`, (route) => {
    reads.push(route);
  });

  const inFlight = await trackDocumentRequests(page);
  await page.goto(`${origin}/`);
  await expect.poll(() => reads.length).toBe(1);
  // The replaced document reloads, and sends its second read 50 ms later from a timer of its own,
  // after its successor's navigation request: Playwright evaluates nothing in a page whose
  // navigation is pending.
  await page.evaluate(() => {
    window.location.reload();
    setTimeout(() => void fetch("/api/v1/inbox?after"), 50);
  });
  const held = await successor.promise;
  await expect.poll(() => reads.length).toBe(2);
  await held.fulfill({ body: html("shown"), contentType: "text/html" });
  await expect.poll(() => reads.length).toBe(3);
  const [before, after, shown] = reads;
  if (before === undefined || after === undefined || shown === undefined) {
    throw new Error(`the page sent ${reads.length} Inbox reads, where the row reads three`);
  }
  // The control's premise: both reads of the replaced document are still listed, answered by
  // nothing and failed by nothing, and the second was sent after the successor's own request.
  const listed = await page.requests();
  for (const read of [before, after]) {
    expect(listed.includes(read.request())).toBe(true);
    expect([read.request().existingResponse(), read.request().failure()]).toEqual([null, null]);
  }
  expect(listed.indexOf(held.request())).toBeLessThan(listed.indexOf(after.request()));

  let answeredAt = 0;
  const answer = (async () => {
    // Held long enough that a wait which ignored the read would settle well before it is answered.
    await page.waitForTimeout(SHOWN_READ_HOLD_MS);
    answeredAt = Date.now();
    await shown.fulfill({ json: [] });
  })();
  await waitForRouteSettled(page, { ready: (current) => [current.getByRole("main")] }, inFlight);
  const settledAt = Date.now();
  await answer;
  expect(settledAt - answeredAt).toBeGreaterThanOrEqual(250);
});

test("a layout reading fails on a blank page and on an entry its reading did not match @pr", async ({
  page,
}) => {
  const place = { route: "control", width: 360 } as const;
  const blank = await measure(page, "<p>Loading…</p>");
  expect(failureIdentities(judgeReading(place, blank))).toEqual([
    { place, type: "too-few-elements" },
  ]);
  expect(judgeReading(place, blank)[0]?.message).toContain("fewer than 100");
  // A route that names a lower floor for itself is still refused below it.
  expect(judgeReading(place, blank, { minElements: 40 })[0]?.message).toContain("fewer than 40");
  // The shell does not count: a blank region inside a shell of 200 elements is still blank.
  await page.evaluate(() => {
    document.body.insertAdjacentHTML(
      "afterbegin",
      // Spaced, so the shell's 200 words wrap and the one failure is the blank region's.
      `<nav>${"<span>shell</span> ".repeat(200)}</nav>`
    );
  });
  expect(failureIdentities(judgeReading(place, await page.evaluate(measureLayout)))).toEqual([
    { place, type: "too-few-elements" },
  ]);

  const drawn = await measure(page, entries(120));
  expect(judgeReading(place, drawn)).toEqual([]);

  const scrolled = await measure(page, `${chipStrip.body}${entries(120)}`);
  const entry: KnownLayoutBreach = {
    element: chipStrip.locator,
    kind: "box-scroll",
    route: "control",
    width: 360,
  };
  const elsewhere: KnownLayoutBreach = { ...entry, element: '[data-testid="gone"]' };
  expect(failureIdentities(judgeReading(place, scrolled, {}, [entry, elsewhere]))).toEqual([
    {
      element: '[data-testid="gone"]',
      kind: "box-scroll",
      matched: 0,
      place: { route: "control", state: undefined, width: 360 },
      type: "entry-mismatch",
    },
  ]);
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
    route: "counted",
    width: 360,
  };
  const two = await measure(page, region(2));
  expect(judgeReading(place, two, {}, [{ ...entry, count: 2 }])).toEqual([]);
  // A new breach in the listed region fails, and so does an entry with fewer breaches than its
  // count: each entry is one breach, or the number it says.
  expect(failureIdentities(judgeReading(place, two, {}, [entry]))).toEqual([
    { count: 1, element: '[data-testid="region"]', kind: "clipped", place, type: "unaccepted" },
  ]);
  const one = await measure(page, region(1));
  expect(failureIdentities(judgeReading(place, one, {}, [{ ...entry, count: 2 }]))).toEqual([
    {
      element: '[data-testid="region"]',
      kind: "clipped",
      matched: 1,
      place: { route: "counted", state: undefined, width: 360 },
      type: "entry-mismatch",
    },
  ]);
});

test("a layout entry names a required group no shown entry marks, and a page-scroll breach matches only its own locator @pr", async ({
  page,
}) => {
  const place = { route: "grouped", width: 360 } as const;
  const drawn = await measure(page, entries(120));
  const missing: KnownLayoutBreach = {
    element: "labels",
    kind: "group-missing",
    route: "grouped",
    width: 360,
  };
  expect(failureIdentities(judgeReading(place, drawn, { groups: ["labels"] }))).toEqual([
    { count: 1, element: "labels", kind: "group-missing", place, type: "unaccepted" },
  ]);
  expect(judgeReading(place, drawn, { groups: ["labels"] }, [missing])).toEqual([]);
  const marked = await measure(
    page,
    `<div data-layout-group="labels">${entries(3)}</div>${entries(120)}`
  );
  expect(failureIdentities(judgeReading(place, marked, { groups: ["labels"] }, [missing]))).toEqual(
    [
      {
        element: "labels",
        kind: "group-missing",
        matched: 0,
        place: { route: "grouped", state: undefined, width: 360 },
        type: "entry-mismatch",
      },
    ]
  );
  // A marked copy the page does not show - hidden, or in a closed Details - marks nothing.
  for (const hidden of [
    `<div data-layout-group="labels" style="display: none">${entries(3)}</div>`,
    `<details><summary>Labels</summary><div data-layout-group="labels">${entries(3)}</div></details>`,
  ]) {
    const copy = await measure(page, `${hidden}${entries(120)}`);
    expect(failureIdentities(judgeReading(place, copy, { groups: ["labels"] }))).toEqual([
      { count: 1, element: "labels", kind: "group-missing", place, type: "unaccepted" },
    ]);
  }

  // Two elements past the viewport inside one region: one named for itself, one named for the
  // region. Each breach matches the entry that names its own locator, and an entry that names the
  // region does not take the breach of the element named for itself inside it.
  const deep = '[data-testid="deep"]';
  const region = '[data-testid="region"]';
  const scrolled = await measure(
    page,
    `<div data-testid="region"><div data-testid="deep" style="height: 10px; width: 900px"></div>` +
      `<div style="height: 10px; width: 900px"></div></div>${entries(120)}`
  );
  const pageScroll = (element: string, count?: number): KnownLayoutBreach => ({
    count,
    element,
    kind: "page-scroll",
    route: "grouped",
    width: 360,
  });
  expect(judgeReading(place, scrolled, {}, [pageScroll(region), pageScroll(deep)])).toEqual([]);
  expect(failureIdentities(judgeReading(place, scrolled, {}, [pageScroll(region, 2)]))).toEqual([
    { count: 1, element: deep, kind: "page-scroll", place, type: "unaccepted" },
    {
      element: region,
      kind: "page-scroll",
      matched: 1,
      place: { route: "grouped", state: undefined, width: 360 },
      type: "entry-mismatch",
    },
  ]);
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
