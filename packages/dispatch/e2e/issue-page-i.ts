import { expect, type Locator, type Page } from "@playwright/test";

import { FOOT_HEIGHT_PROPERTY } from "../web/src/features/issue-page/identity-block";
import { presetPath } from "../web/src/features/issue-page/issue-address";
import type { Preset } from "../web/src/features/issue-page/page-words";
import { gotoIssue, issuePage, openMenu, presetChooser, presetRow } from "./issue-page";
import { type DrawnRow, nodeRows, parentOfRow, tree } from "./issue-page-g";

/**
 * How the outline's rows read the outline of the new issue page (`e2e/issue-page-i.e2e.ts`):
 * the readers that `e2e/issue-page.ts` (the page's root and its frame) and
 * `e2e/issue-page-g.ts` (one node's row, and the tree as rows) do not hold. Later rows import
 * from this file rather than change it.
 *
 * What the outline draws, beyond what `e2e/issue-page-g.ts` names:
 *
 * - **The tree** `role="tree"` named `CONTROL_LABELS.tree` (`issue-page-g.ts`'s `tree`), its
 *   rows `treeitem`s carrying `aria-level`, and the one row with `tabindex="0"`, its tab stop. A
 *   row of the outline carries `data-row-key`, which `NodeRow.tsx` draws and no view's row does:
 *   every view draws a tree of its own, so a tree is no sign that the outline is drawn.
 * - **The stage** the outline is drawn in: the column `[data-testid="issue-column"]` that holds
 *   the stage and the foot (`IssueOutlinePage.tsx`).
 * - **A row's bullet** `[data-node-bullet]`: its column, or its fold control where its node holds
 *   others; its tag `[data-node-tag]` in its margin; and a link its words draw for a reference,
 *   `a[data-row-reference]` naming the reference it links.
 * - **A node's body** `[data-node-body]`, drawn for the focused row and each row pinned open
 *   (`[data-node-body][data-pinned]`), and for no other row.
 * - **A branch deeper than the drawn levels**, one row `[data-branch-more]` whose text says how
 *   many nodes it holds and whose `button` opens them in place.
 * - **A gap the page names** `[data-gap]` with `data-gap-kind`.
 * - **What a key reports** where it has nowhere to go: `[data-row-report]`, a `role="status"`
 *   drawn at the row the key was pressed on.
 * - **The children**: rows of the outline, `[data-node-type="child_issue"]`, one group; and the
 *   section the page drew above the outline before the children became its rows,
 *   `[data-testid="issue-children"]`, which no address draws now.
 * - **What stays at the top of the screen**: the header's band `[data-identity-block]` from
 *   761 px up, and the view row's band at 760 px and below.
 * - **What stays at the foot of the screen**: the box to write in, whose height the page
 *   publishes on the document element as `--issue-composer-height` (`FOOT_HEIGHT_PROPERTY` of
 *   `identity-block.ts`). Until it is drawn, `standInFootBox` draws a box in its place.
 * - **A control under the outline**, which a body closing above it moves: none of the page's own
 *   at this head. `standInControlBelow` draws one in its place.
 */

/** The selectors the readers pass into the browser. */
const SELECTORS = {
  branchMore: "[data-branch-more]",
  /** The part of the header that stays at the top of the screen from 761 px up. */
  identityBand: "[data-identity-block]",
  gap: "[data-gap]",
  issuePage: "[data-issue-page]",
  nodeBody: "[data-node-body]",
  nodeBullet: "[data-node-bullet]",
  /** A shape a row draws beside its type, its state or its author (`NodeGlyph.tsx`). */
  nodeGlyph: "svg[data-glyph]",
  nodeRow: "[data-node-row]",
  nodeTag: "[data-node-margin] [data-node-tag]",
  /** A row of the outline, and of no view's tree. */
  outlineRow: "[data-node-row][data-row-key]",
  presetRow: '[data-testid="issue-presets"]',
  stage: '[data-testid="issue-column"]',
  tree: '[role="tree"]',
} as const;

/** A `"` inside an attribute value would end the selector's string, and no id, slug or kind the
 *  page draws holds one; a value that did would otherwise match nothing silently. */
function cssValue(value: string): string {
  expect(value, `${value} cannot be written into a selector`).not.toContain('"');
  return value;
}

// ---------------------------------------------------------------------------------------------
// The outline's parts, as locators.
// ---------------------------------------------------------------------------------------------

export function treeItems(page: Page): Locator {
  return tree(page).getByRole("treeitem");
}

export function rowsOfType(page: Page, type: string): Locator {
  return nodeRows(page).filter({
    has: page.locator(`:scope[data-node-type="${cssValue(type)}"]`),
  });
}

export function nodeBodies(page: Page): Locator {
  return issuePage(page).locator(SELECTORS.nodeBody);
}

export function pinnedBodies(page: Page): Locator {
  return issuePage(page).locator(`${SELECTORS.nodeBody}[data-pinned]`);
}

export function branchMoreRows(page: Page): Locator {
  return issuePage(page).locator(SELECTORS.branchMore);
}

/** The gaps drawn on one row. */
export function rowGaps(row: Locator): Locator {
  return row.locator(SELECTORS.gap);
}

/** The children's section the page drew above the outline before the architect ruled that the
 *  children are rows of it: no view draws one now, and a row reads this to say so. */
export function childrenSection(page: Page): Locator {
  return page.getByTestId("issue-children");
}

/** The children as the page draws them: rows of the outline, one group of them. */
export function childRows(page: Page): Locator {
  return tree(page).locator('[data-node-row][data-node-type="child_issue"]');
}

/** One child's row of the outline, by the child's key, which is its node's id. */
export function childRow(page: Page, key: string): Locator {
  return tree(page).locator(`[data-node-row][data-node-id="${cssValue(key)}"]`);
}

/** What the page says when a key has nowhere to go, such as the next-question key with no
 *  further question: drawn at the row the key was pressed on, and gone with the reader's next
 *  move. */
export function pageStatus(page: Page): Locator {
  return tree(page).locator("[data-row-report]");
}

/** The outline's own rows, by the mark a row of the outline carries and no view's row does: a
 *  view's tree is named as the outline's is, so the tree alone does not say which is drawn. */
export function outlineRows(page: Page): Locator {
  return issuePage(page).locator(SELECTORS.outlineRow);
}

/** The outline's one row in the tab order, which takes the focus back when the reader comes
 *  back to the tree (the WAI-ARIA tree pattern). */
export function tabStopRow(page: Page): Locator {
  return tree(page).locator(`${SELECTORS.nodeRow}[tabindex="0"]`);
}

/** The link `row`'s words draw for `reference` (`dispatch://<key>` for an issue), which names
 *  what it links by the page's batched read (`row-references.ts`). */
export function rowReference(row: Locator, reference: string): Locator {
  return row.locator(`[data-node-words] a[data-row-reference="${cssValue(reference)}"]`);
}

// ---------------------------------------------------------------------------------------------
// The readers: one `page.evaluate` each, so a page that draws none of this answers an empty
// list or null instead of timing out.
// ---------------------------------------------------------------------------------------------

/** The ids of `rows`, in order, a row with none named so in a failure. */
export function idsOf(rows: readonly DrawnRow[]): string[] {
  return rows.map((row) => row.id ?? "<no id>");
}

/** The text of each row's tag, by its node's id: a typed kind's word, or a child's key. A row
 *  that draws no tag is not in it. */
export async function rowTags(page: Page): Promise<ReadonlyMap<string, string>> {
  const tags = await page.evaluate(
    (selectors) =>
      [...document.querySelectorAll(`${selectors.issuePage} ${selectors.nodeRow}`)].flatMap(
        (row): [string, string][] => {
          const id = row.getAttribute("data-node-id");
          const tag = row.querySelector(selectors.nodeTag);
          return id === null || tag === null ? [] : [[id, (tag.textContent ?? "").trim()]];
        }
      ),
    SELECTORS
  );
  return new Map(tags);
}

/** Where `scope` stands in the stage, the column that holds the stage and the foot, inside the
 *  column's insets: its width, and its gap to the column's inner edge at each side. Null where
 *  `scope` stands in no such column. */
export function placeInStage(
  scope: Locator
): Promise<{ readonly left: number; readonly right: number; readonly width: number } | null> {
  return scope.first().evaluate((element, selectors) => {
    const column = element.closest(selectors.stage);
    if (column === null) return null;
    const style = getComputedStyle(column);
    const inner = column.getBoundingClientRect().left + column.clientLeft;
    const start = inner + Number.parseFloat(style.paddingLeft);
    const end = inner + column.clientWidth - Number.parseFloat(style.paddingRight);
    const box = element.getBoundingClientRect();
    return { left: box.left - start, right: end - box.right, width: box.width };
  }, SELECTORS);
}

/** The box of each visible row's bullet, in document order, as `issue-page-g.ts`'s `rowLayouts`
 *  reads the rows: the bullet's column on a node that holds no other, and its fold control on
 *  one that does. That reader gives the left of the bullet's dot, which stands where every
 *  bullet's does, and not the edges of the box it stands in. */
export function bulletBoxes(
  page: Page
): Promise<{ readonly id: string; readonly left: number; readonly right: number }[]> {
  return page.evaluate(
    (selectors) =>
      [...document.querySelectorAll(`${selectors.issuePage} ${selectors.nodeRow}`)]
        .filter((row) => row.getClientRects().length > 0)
        .map((row) => {
          const box = row.querySelector(selectors.nodeBullet)?.getBoundingClientRect();
          return {
            id: row.getAttribute("data-node-id") ?? "",
            left: box?.left ?? -1,
            right: box?.right ?? -1,
          };
        }),
    SELECTORS
  );
}

/** Every item the tree draws, in order, with its `aria-level`: a node's row by its node's id,
 *  and the one line a branch below the drawn levels folds to by its text, which counts what it
 *  holds. */
export function drawnItems(
  page: Page
): Promise<
  { readonly id: string | null; readonly level: string | null; readonly more: string | null }[]
> {
  return page.evaluate(
    (selectors) =>
      [
        ...document.querySelectorAll(
          `${selectors.issuePage} ${selectors.tree} :is(${selectors.nodeRow}, ${selectors.branchMore})`
        ),
      ].map((item) => ({
        id: item.getAttribute("data-node-id"),
        level: item.getAttribute("aria-level"),
        more: item.matches(selectors.branchMore)
          ? (item.textContent ?? "").replace(/\s+/g, " ").trim()
          : null,
      })),
    SELECTORS
  );
}

/** Each deeper branch's one line: the `--row-depth` it is drawn at and its computed left
 *  padding, in pixels, which is its indent (`TreeRows.tsx`'s `BranchMore`). */
export function branchMoreIndents(
  page: Page
): Promise<{ readonly depth: number; readonly padding: number }[]> {
  return page.evaluate(
    (selectors) =>
      [...document.querySelectorAll(`${selectors.issuePage} ${selectors.branchMore}`)].map(
        (line) => {
          const style = getComputedStyle(line);
          return {
            depth: Number.parseFloat(style.getPropertyValue("--row-depth")),
            padding: Number.parseFloat(style.paddingLeft),
          };
        }
      ),
    SELECTORS
  );
}

/** The `data-enabled` of each line of the open Keys list (`ShortcutHelp.tsx`) whose keys are
 *  `chord`, such as `t h`: the list greys a binding that cannot run where the focus is, and
 *  does not hide it. */
export function keysListLines(page: Page, chord: string): Promise<(string | null)[]> {
  return page
    .getByRole("dialog", { name: "Keyboard shortcuts" })
    .evaluate(
      (dialog, keys) =>
        [...dialog.querySelectorAll("li[data-enabled]")]
          .filter(
            (line) =>
              [...line.querySelectorAll("kbd")]
                .map((kbd) => (kbd.textContent ?? "").trim())
                .join(" ") === keys
          )
          .map((line) => line.getAttribute("data-enabled")),
      chord
    );
}

/** The chain of ancestors above `id`, outermost first, as the drawn levels give it. */
export async function ancestorsOfRow(page: Page, id: string): Promise<string[]> {
  const chain: string[] = [];
  for (let at: string | null | undefined = id; at !== null && at !== undefined; ) {
    const parent: string | null | undefined = await parentOfRow(page, at);
    if (parent === null || parent === undefined) break;
    chain.unshift(parent);
    at = parent;
  }
  return chain;
}

/** The id of the row the keyboard focus is on: the row holding the focus, or the row the tree's
 *  `aria-activedescendant` names, whichever the page uses. */
export function focusedNodeId(page: Page): Promise<string | null> {
  return page.evaluate((selectors) => {
    const active = document.activeElement;
    const own = active?.closest(selectors.nodeRow) ?? null;
    if (own !== null) return own.getAttribute("data-node-id");
    const named = active?.getAttribute("aria-activedescendant") ?? null;
    if (named === null) return null;
    const row = document.getElementById(named)?.closest(selectors.nodeRow) ?? null;
    return row?.getAttribute("data-node-id") ?? null;
  }, SELECTORS);
}

/** Whether the keyboard focus is inside `scope`: a chord that reaches a target has to put the
 *  focus in it, not only draw it. A scope the page does not draw answers false. */
export async function focusInside(scope: Locator): Promise<boolean> {
  if ((await scope.count()) === 0) return false;
  return await scope
    .first()
    .evaluate(
      (element) => element.contains(document.activeElement) || element === document.activeElement
    );
}

/**
 * Whether every pixel of `scope` is inside the viewport, clear of what stays at the top of the
 * screen and above the box that stays at its foot, which is what "on screen" is read as, and what
 * the page's own `bringOnScreen` (`web/src/features/issue-page/identity-block.ts`) places a target
 * by. Details' drawer is a layer over the page, so a target inside it needs only the viewport.
 *
 * A scope taller than the screen can satisfy neither, by construction, so a caller reads the
 * control the page leaves the reader on rather than the whole part around it.
 */
export async function onScreen(scope: Locator): Promise<boolean> {
  if ((await scope.count()) === 0) return false;
  return await scope.first().evaluate(
    (element, { footHeight, selectors }) => {
      const rect = element.getBoundingClientRect();
      // What stays at the top of the screen: the header's band from 761 px up, and the view
      // row's band at 760 px and below - whichever is `position: sticky` and at the top now.
      // What stays at the foot: the box whose height the page publishes on the document element,
      // as `bringOnScreen` reads it, unset while no box is drawn, which bounds the scope by the
      // window alone.
      let sticky = 0;
      let foot = 0;
      if (element.closest('[data-testid="desktop-margin-shell"]') === null) {
        for (const start of [selectors.identityBand, selectors.presetRow]) {
          for (let at = document.querySelector(start); at !== null; at = at.parentElement) {
            if (getComputedStyle(at).position !== "sticky") continue;
            const band = at.getBoundingClientRect();
            if (band.top <= 1) sticky = Math.max(sticky, band.bottom);
            break;
          }
        }
        foot =
          Number.parseFloat(
            getComputedStyle(document.documentElement).getPropertyValue(footHeight)
          ) || 0;
      }
      return (
        rect.height > 0 &&
        rect.top >= sticky - 1 &&
        rect.bottom <= window.innerHeight - foot + 1 &&
        rect.left >= -1 &&
        rect.right <= window.innerWidth + 1
      );
    },
    { footHeight: FOOT_HEIGHT_PROPERTY, selectors: SELECTORS }
  );
}

/** The contrast a shape has to stand out from what is behind it by to count as drawn: WCAG's
 *  3:1 for a part of a graphic the reader needs to understand the content (success criterion
 *  1.4.11, non-text contrast). */
export const SHAPE_CONTRAST = 3;

/** One shape a row draws (`svg[data-glyph]`, `NodeGlyph.tsx`), as the screen shows it. */
export interface PaintedShape {
  /** The highest contrast ratio (WCAG's, 1 to 21) any mark of the shape has against what is
   *  behind that mark: 1 where it paints nothing that stands out, an empty or hidden box
   *  included. */
  readonly contrast: number;
  /** Its `data-glyph` token, which no reader sees. */
  readonly token: string;
}

/**
 * How each row's shapes stand out from what is behind them, row by row in document order as
 * `issue-page-g.ts`'s `drawnRows` reads the rows. A shape's marks are the stroke and the fill of
 * each outline in it and the background of its own box and of each box inside it, each weighed
 * by its own alpha and the opacity of everything it stands in; what is behind a mark is every
 * background under it down to the first opaque one, laid over the page's canvas.
 *
 * It reads the computed colours, which are what forced colours change: with them on, the
 * browser puts one of the reader's own colours in place of each colour the page asked for,
 * except where `forced-color-adjust` keeps the page's, so a shape drawn by a colour it replaces
 * - a box's background, which takes the canvas's colour - is drawn no longer. A shape's token
 * and its accessible name stay as they were, which is why `drawnRows` cannot tell.
 */
export function shapesPainted(
  page: Page
): Promise<{ readonly id: string | null; readonly shapes: readonly PaintedShape[] }[]> {
  return page.evaluate((selectors) => {
    type Colour = readonly [number, number, number, number];
    // Any colour a computed style holds - `rgb()`, `oklch()`, a system colour - as sRGB, by
    // painting it into one pixel: a value the canvas cannot paint, such as a `url()` fill, leaves
    // the pixel transparent.
    const pixel = document.createElement("canvas").getContext("2d", { willReadFrequently: true });
    if (pixel === null) throw new Error("the browser draws no 2D canvas to read a colour with");
    const rgba = (colour: string): Colour => {
      pixel.clearRect(0, 0, 1, 1);
      pixel.fillStyle = "transparent";
      pixel.fillStyle = colour;
      pixel.fillRect(0, 0, 1, 1);
      const [red = 0, green = 0, blue = 0, alpha = 0] = pixel.getImageData(0, 0, 1, 1).data;
      return [red, green, blue, alpha / 255];
    };
    const over = ([red, green, blue, alpha]: Colour, under: Colour): Colour => [
      red * alpha + under[0] * (1 - alpha),
      green * alpha + under[1] * (1 - alpha),
      blue * alpha + under[2] * (1 - alpha),
      1,
    ];
    // Where no box paints a background the reader sees the canvas, the system colour `Canvas`,
    // which is also the colour forced colours give every background they replace.
    const probe = document.body.appendChild(document.createElement("div"));
    probe.style.backgroundColor = "Canvas";
    const canvas = rgba(getComputedStyle(probe).backgroundColor);
    probe.remove();
    /** What is behind a mark drawn in `box`: the backgrounds of `box` and of each box it stands
     *  in, down to the first opaque one, laid over the canvas. */
    const behind = (box: Element | null): Colour => {
      const layers: Colour[] = [];
      for (let at = box; at !== null; at = at.parentElement) {
        const layer = rgba(getComputedStyle(at).backgroundColor);
        if (layer[3] === 0) continue;
        layers.push(layer);
        if (layer[3] === 1) break;
      }
      return layers.reduceRight((under, layer) => over(layer, under), canvas);
    };
    /** How much of `element` shows: its opacity and that of every box it stands in. */
    const shows = (element: Element): number => {
      let opacity = 1;
      for (let at: Element | null = element; at !== null; at = at.parentElement) {
        opacity *= Number.parseFloat(getComputedStyle(at).opacity);
      }
      return opacity;
    };
    const luminance = ([red, green, blue]: Colour): number => {
      const linear = (channel: number) => {
        const value = channel / 255;
        return value <= 0.04045 ? value / 12.92 : ((value + 0.055) / 1.055) ** 2.4;
      };
      return 0.2126 * linear(red) + 0.7152 * linear(green) + 0.0722 * linear(blue);
    };
    const contrast = (one: Colour, other: Colour): number => {
      const [a, b] = [luminance(one), luminance(other)];
      return (Math.max(a, b) + 0.05) / (Math.min(a, b) + 0.05);
    };
    const standsOut = (glyph: Element): number => {
      const box = glyph.getBoundingClientRect();
      if (box.width === 0 || box.height === 0) return 1;
      let best = 1;
      for (const part of [glyph, ...glyph.querySelectorAll("*")]) {
        const look = getComputedStyle(part);
        if (look.display === "none" || look.visibility !== "visible") continue;
        const marks: { alpha: number; colour: string; under: Element | null }[] = [
          { alpha: 1, colour: look.backgroundColor, under: part.parentElement },
        ];
        if (part instanceof SVGGeometryElement) {
          if (look.fill !== "none") {
            marks.push({
              alpha: Number.parseFloat(look.fillOpacity),
              colour: look.fill,
              under: part,
            });
          }
          if (look.stroke !== "none" && Number.parseFloat(look.strokeWidth) > 0) {
            marks.push({
              alpha: Number.parseFloat(look.strokeOpacity),
              colour: look.stroke,
              under: part,
            });
          }
        }
        const seen = shows(part);
        for (const mark of marks) {
          const [red, green, blue, alpha] = rgba(mark.colour);
          const under = behind(mark.under);
          const painted = over([red, green, blue, alpha * mark.alpha * seen], under);
          best = Math.max(best, contrast(painted, under));
        }
      }
      return best;
    };
    return [...document.querySelectorAll(`${selectors.issuePage} ${selectors.nodeRow}`)].map(
      (row) => ({
        id: row.getAttribute("data-node-id"),
        shapes: [...row.querySelectorAll(selectors.nodeGlyph)].map((glyph) => ({
          contrast: standsOut(glyph),
          token: glyph.getAttribute("data-glyph") ?? "",
        })),
      })
    );
  }, SELECTORS);
}

/** Whether the caret is in a node's own text: the focus is inside an editable inside a row,
 *  which is what Enter does and what Escape leaves. */
export function caretInNode(page: Page): Promise<boolean> {
  return page.evaluate((selectors) => {
    const editable = document.activeElement?.closest('[contenteditable="true"]') ?? null;
    return editable !== null && editable.closest(selectors.nodeRow) !== null;
  }, SELECTORS);
}

/** Whether the caret is in block `blockId` of the editor a row holds: the focus inside that
 *  editable, and the selection's anchor inside the block. */
export function caretInRowBlock(page: Page, blockId: string): Promise<boolean> {
  return page.evaluate(
    ({ block, selectors }) => {
      const editable = document.activeElement?.closest('[contenteditable="true"]') ?? null;
      if (editable === null || editable.closest(selectors.nodeRow) === null) return false;
      const anchor = window.getSelection()?.anchorNode ?? null;
      const drawn = editable.querySelector(`[data-block-id="${block}"]`);
      return anchor !== null && drawn?.contains(anchor) === true;
    },
    { block: cssValue(blockId), selectors: SELECTORS }
  );
}

/** The node id the tree's `aria-activedescendant` names, for a tree that roves with it rather
 *  than by moving the focus itself. Null when the tree names none. */
export function treeActiveDescendant(page: Page): Promise<string | null> {
  return page.evaluate((selectors) => {
    const drawn = document.querySelector(`${selectors.issuePage} ${selectors.tree}`);
    const named = drawn?.getAttribute("aria-activedescendant") ?? null;
    if (named === null) return null;
    return (
      document.getElementById(named)?.closest(selectors.nodeRow)?.getAttribute("data-node-id") ??
      null
    );
  }, SELECTORS);
}

/** Where the sticky header's band ends, which is the line the focused row must clear; 0 where
 *  the band is not sticky, which is 760 px and below; null where the page draws none. */
export function identityBlockBottom(page: Page): Promise<number | null> {
  return page.evaluate((selectors) => {
    const block = document.querySelector(selectors.identityBand);
    if (block === null) return null;
    const rect = block.getBoundingClientRect();
    return getComputedStyle(block).position === "sticky" ? rect.bottom : 0;
  }, SELECTORS);
}

/** The kind each gap the page names carries, in document order: a gap with no kind reads as the
 *  empty string, so an unnamed gap is named in the failure rather than dropped. */
export function gapKinds(page: Page): Promise<string[]> {
  return page.evaluate(
    (selectors) =>
      [...document.querySelectorAll(`${selectors.issuePage} ${selectors.gap}`)].map(
        (gap) => gap.getAttribute("data-gap-kind") ?? ""
      ),
    SELECTORS
  );
}

/** Every `title` attribute the page carries, which is every tooltip a pointer can raise. */
export function tooltipTexts(page: Page): Promise<string[]> {
  return page.evaluate(
    (selectors) =>
      [...document.querySelectorAll(`${selectors.issuePage} [title]`)].map(
        (element) => element.getAttribute("title") ?? ""
      ),
    SELECTORS
  );
}

/** The whole visible text of the tree, for a reading taken before and after a pointer crosses
 *  it. */
export function treeText(page: Page): Promise<string> {
  return page.evaluate((selectors) => {
    const drawn = document.querySelector(`${selectors.issuePage} ${selectors.tree}`);
    return (drawn?.textContent ?? "").replace(/\s+/g, " ").trim();
  }, SELECTORS);
}

/** The name of the view the page is on: the pressed toggle's own name at a width that draws the
 *  row, and the one control's name part below it. Null while the page draws neither. */
export function activePreset(page: Page): Promise<string | null> {
  return page.evaluate((selectors) => {
    const row = document.querySelector(selectors.presetRow);
    if (row === null) return null;
    const pressed = row.querySelector('button[aria-pressed="true"] [data-preset-name]');
    if (pressed !== null) return (pressed.textContent ?? "").trim();
    const chooser = row.querySelector('button[aria-haspopup="menu"] [data-preset-name]');
    return chooser === null ? null : (chooser.textContent ?? "").trim();
  }, SELECTORS);
}

// ---------------------------------------------------------------------------------------------
// Moves.
// ---------------------------------------------------------------------------------------------

/** Opens the view `preset` on `key` at its own address. */
export async function openPreset(page: Page, key: string, preset: Preset): Promise<void> {
  await gotoIssue(page, presetPath(key, preset));
}

/**
 * Draws a stand-in for the box to write in at the foot of the screen (`slots/foot.tsx`),
 * `height` px tall, until the page draws it: the column's last part, staying at the screen's
 * foot, with its height published on the document element as that box publishes it, which is
 * how the page's `bringOnScreen` and this file's `onScreen` know it is there. It takes its
 * height at the column's end as a box at the foot does, so the outline's last row can be
 * scrolled above it. Answers what takes it away again.
 */
export async function standInFootBox(page: Page, height: number): Promise<() => Promise<void>> {
  const drawn = await page.evaluate(
    ({ footHeight, height, stage }) => {
      const column = document.querySelector(stage);
      if (column === null) return false;
      const box = document.createElement("div");
      box.setAttribute("data-foot-stand-in", "");
      box.style.cssText = `position: sticky; bottom: 0; flex: none; height: ${height}px;`;
      column.append(box);
      document.documentElement.style.setProperty(footHeight, `${height}px`);
      return true;
    },
    { footHeight: FOOT_HEIGHT_PROPERTY, height, stage: SELECTORS.stage }
  );
  expect(drawn, "the page draws no stage column to stand a box at the foot of").toBe(true);
  return async () => {
    await page.evaluate((footHeight) => {
      document.querySelector("[data-foot-stand-in]")?.remove();
      document.documentElement.style.removeProperty(footHeight);
    }, FOOT_HEIGHT_PROPERTY);
  };
}

/**
 * Draws a stand-in for a control of the page under the outline: nothing of the page's own stands
 * there at this head - the line at the foot is not drawn (`slots/foot.tsx`), Details is a
 * drawer fixed at the right of the screen, and the Contents pane stays where it is as the page
 * scrolls - and a control under the outline is the one a body closing above it moves. It is a
 * button, the column's last part, where the line at the foot will stand, that counts the presses
 * that fire it, with a screen's height of page under it: a row can put it in the middle of the
 * screen, where a body closing above it moves it. With the page scrolled to its foot, the
 * browser keeps the foot on the screen as the page shortens, and the button with it, so no press
 * there could tell a body closing from one that stays. Answers the button, how many presses
 * fired it, and what takes it away again.
 */
export async function standInControlBelow(page: Page): Promise<{
  readonly control: Locator;
  readonly presses: () => Promise<number>;
  readonly remove: () => Promise<void>;
}> {
  const drawn = await page.evaluate((stage) => {
    const column = document.querySelector(stage);
    if (column === null) return false;
    const box = document.createElement("div");
    box.setAttribute("data-control-stand-in", "");
    box.style.cssText = "flex: none; padding-bottom: 100vh;";
    const control = document.createElement("button");
    control.type = "button";
    control.textContent = "A control under the outline";
    control.dataset.presses = "0";
    control.addEventListener("click", () => {
      control.dataset.presses = String(Number(control.dataset.presses) + 1);
    });
    box.append(control);
    column.append(box);
    return true;
  }, SELECTORS.stage);
  expect(drawn, "the page draws no stage column to stand a control under the outline in").toBe(
    true
  );
  const control = page.locator("[data-control-stand-in] button");
  return {
    control,
    presses: async () => Number(await control.getAttribute("data-presses")),
    remove: async () => {
      await page.evaluate(() => document.querySelector("[data-control-stand-in]")?.remove());
    },
  };
}

/** How many presses `openEveryDeeperBranch` makes before it gives up: well past the branches any
 *  seeded issue holds, so a tree that draws the control whatever is pressed ends as the
 *  caller's own failed reading rather than as this helper running until the row's deadline. */
const BRANCHES_OPENED_AT_MOST = 200;

/** Opens every branch the tree folded to one line because it sits below the drawn levels
 *  (`tree-fold.ts`'s `DRAWN_LEVELS`), so a row reads the nodes such a branch holds as rows of
 *  their own, as `issue-page-g.ts`'s `unfoldEveryBranch` does with a fold control. */
export async function openEveryDeeperBranch(page: Page): Promise<void> {
  const controls = branchMoreRows(page).getByRole("button");
  for (let pressed = 0; pressed < BRANCHES_OPENED_AT_MOST; pressed += 1) {
    const first = controls.first();
    if ((await first.count()) === 0) return;
    await first.click();
  }
  expect(
    await controls.count(),
    `the tree still draws a deeper branch after ${BRANCHES_OPENED_AT_MOST} openings`
  ).toBe(0);
}

/**
 * Moves to the view `name` by pressing the view row itself, however that row draws at the width
 * a row runs at: five toggles from 1280 px, and one control opening a menu of five below it. A
 * toggle already pressed is left as it is, since pressing it again turns it off. `openPreset`
 * goes to the control's own address instead, which is a load.
 */
export async function pickPreset(page: Page, name: string): Promise<void> {
  const row = presetRow(page);
  // The row has to be on the page before its shape can be read: a count taken while the page
  // is still loading answers zero for both shapes.
  await row.waitFor();
  const toggle = row.getByRole("button", { exact: true, name });
  if ((await toggle.count()) > 0) {
    if ((await toggle.getAttribute("aria-pressed")) !== "true") await toggle.click();
    return;
  }
  await presetChooser(page).click();
  // Each item is named by its view's word and then its signal (`presetMenuItem`).
  await openMenu(page)
    .getByRole("menuitemradio", { name: new RegExp(`^${name}( |$)`) })
    .click();
}

/** Fails unless the page is on the view `name`, whichever shape the row has at this width: the
 *  row of toggles presses the one it is on (`aria-pressed`), and the one control below 1280 px
 *  draws its name and its signal as its own name (`IssuePresets.tsx`). */
export async function expectActivePreset(page: Page, name: string): Promise<void> {
  const row = presetRow(page);
  await row.waitFor();
  const toggle = row.getByRole("button", { exact: true, name });
  if ((await toggle.count()) > 0) {
    await expect(toggle).toHaveAttribute("aria-pressed", "true");
    return;
  }
  await expect(presetChooser(page)).toHaveAccessibleName(new RegExp(`^${name} \\d+\\b`));
}
