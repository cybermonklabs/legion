/*
 * The helpers part N's rows read (dispatch://INFRA-1196 decision 2): `e2e/page-inventory.ts`'s
 * functions, which read what a page offers for the inventory row and the recorder, and each helper
 * of the checkpoint's `e2e/issue-page.ts` that no earlier part's helper file holds. Every other
 * helper is imported from the part that brought it.
 */

import { expect, type Locator, type Page } from "@playwright/test";
import { presetPath } from "../web/src/features/issue-page/issue-address";
import {
  CONTROL_LABELS,
  PRESET_NAMES,
  PRESET_ORDER,
  type Preset,
} from "../web/src/features/issue-page/page-words";
import { type IssueRoute, parseIssuePath } from "../web/src/features/refs/routes";
import { PAGE_COLORS } from "../web/src/theme/classes";
import { oklchToSrgb8 } from "../web/src/theme/contrast";
import { getIssue } from "./api";
import { connectedDot, documentEditor } from "./editor";
import {
  contentsPane,
  gotoIssue,
  issuePage,
  openMenu,
  outlineControl,
  outlineSheet,
} from "./issue-page";
import { expectIssuePageDrawn, nodeRows } from "./issue-page-g";
import { expectActivePreset, pickPreset } from "./issue-page-i";
import type { Box } from "./layout";
import { navigateInApp } from "./sends";
import { expandedState, settledState } from "./settled";
import { assertPageFits } from "./unclipped";

/** The address that opens an issue's spec with its editor: the spec's own address,
 *  `/issues/KEY/artifacts/<slug>`, which draws it in place of the outline. `/issues/KEY/spec`
 *  is the Spec view and the issue's own address the outline, and neither draws the spec's text
 *  (dispatch://INFRA-1196 R3, R20). The checkpoint's `e2e/editor.ts` held it; no earlier part
 *  brought it, so it is here (decision 2). */
export async function specAddress(issueKey: string): Promise<string> {
  const issue = await getIssue(issueKey);
  const spec = (issue.artifacts ?? []).find(
    (artifact) => artifact.id === issue.primary_artifact_id
  );
  if (spec === undefined) throw new Error(`${issueKey} has no spec to open`);
  return `/issues/${issueKey}/artifacts/${spec.slug}`;
}

/** Opens an issue's spec at its own address and waits until its editor shows `text` and its
 *  document is connected (`expectDocumentConnected`), so a selection or a margin card the row
 *  reaches for next is live. `e2e/editor.ts`'s `openSpec` opens `/spec`, which on the new page is
 *  the Spec view and draws no editor. */
export async function openSpecDocument(page: Page, issueKey: string, text: string): Promise<void> {
  await page.goto(await specAddress(issueKey));
  await expect(documentEditor(page)).toContainText(text);
  await expectDocumentConnected(page);
}

/** Waits until the opened document's editor is live: its connection dot reads "connected".
 *  Today's page draws the dot in the document's own toolbar; the new page draws it in the
 *  version control, beside an open editor alone (part N's R3), and the version control stands
 *  behind its small control in the filter row (R4), so the dot is read with the control open, as
 *  a reader sees it. The control is left open. */
export async function expectDocumentConnected(page: Page): Promise<void> {
  await openVersionControl(page);
  await expect(connectedDot(versionControl(page))).toHaveText("connected");
}

/** The selectors the readers below pass into the browser. Nothing outside this module spells one:
 *  a test reads the page through the locators and readers this module exports. */
const SELECTORS = {
  answerAuthor: "[data-answer-author]",
  answerLine: "[data-node-answer]",
  answerText: "[data-answer-text]",
  authorText: "[data-author-text]",
  branchMore: "[data-branch-more]",
  details: '[data-testid="desktop-margin-shell"]',
  childRow: "[data-child-row]",
  /** One crumb of the line a zoomed outline draws above its rows; none while it is not zoomed. */
  crumb: "[data-crumb]",
  /** Documents' list, drawn in place of the outline and of every view (`DocumentsList.tsx`). */
  documentsList: "[data-documents-list]",
  /** The whole filter row: the five controls or the one control, Outline, and the small
   *  controls at its right end (`IssuePresets.tsx`). */
  filterRow: "[data-filter-row]",
  gap: "[data-gap]",
  /** The part of the identity block that stays at the top of the screen from 761 px up: the
   *  key's row and the title (`IssueHeader.tsx`). */
  identityBand: "[data-identity-block]",
  identityBlock: "header[data-issue-header]",
  issuePage: "[data-issue-page]",
  marginSlot: '[data-testid="issue-margin"]',
  nodeAuthor: "[data-node-author]",
  nodeBody: "[data-node-body]",
  nodeChip: "[data-node-chip]",
  nodeFirstLine: "[data-node-first-line]",
  nodeGlyph: "svg[data-glyph]",
  nodeRow: "[data-node-row]",
  nodeUnsure: "[data-node-unsure]",
  nodeTag: "[data-node-tag]",
  nodeWords: "[data-node-words]",
  nodeState: "[data-node-state]",
  /** The sheet the Outline control opens below 1024 px, drawn in the page's body and not inside
   *  its root (`contents-frame.tsx`), which holds the view row's three small controls at 760 px
   *  and below. */
  outlineSheet: '[data-testid="issue-outline-sheet"]',
  pageChrome: "[data-page-chrome]",
  parentLine: "[data-node-parent-line]",
  presetCount: "[data-preset-count]",
  presetRow: '[data-testid="issue-presets"]',
  tree: '[role="tree"]',
} as const;

// ---------------------------------------------------------------------------------------------
// The page's parts, as locators.
// ---------------------------------------------------------------------------------------------

export function identityBlock(page: Page): Locator {
  return page.locator(SELECTORS.identityBlock);
}

export function nodeMenuButton(page: Page): Locator {
  return issuePage(page).getByRole("button", { exact: true, name: CONTROL_LABELS.other_nodes });
}

export function nodeMenuItems(page: Page): Locator {
  return openMenu(page).getByRole("menuitem");
}

/** The crumbs of the outline's zoom: none while the outline draws the whole issue, and at least
 *  one once a twisty below 1024 px has zoomed it into a node. */
export function zoomCrumbs(page: Page): Locator {
  return issuePage(page).locator(SELECTORS.crumb);
}

/** Documents' list, the one the stage draws at `/documents` and `/artifacts`. */
export function documentsList(page: Page): Locator {
  return page.locator(SELECTORS.documentsList);
}

/** The hidden file input the upload opens, which is how a test hands it a file. */
export function uploadInput(page: Page): Locator {
  return page.locator('input[type="file"]');
}

/** The box of the filter by words, which is `?q=`: drawn while it holds words or has the caret,
 *  and in the Outline sheet at 760 px and below. */
export function filterBox(page: Page): Locator {
  return page.getByTestId("issue-filter");
}

/** The filter by words' small control at the right end of the filter row, which opens its box
 *  with the caret in it; `t f` does the same (`IssuePresets.tsx`). */
export function filterControl(page: Page): Locator {
  return issuePage(page).getByRole("button", { exact: true, name: CONTROL_LABELS.filter });
}

/** The version control: the version picker, the name-a-version control, the link, and beside an
 *  open editor its live connection (`VersionControl.tsx`). It is drawn - and counted - while a
 *  filter is on or a document is open, behind its small control at the right end of the filter
 *  row, which `openVersionControl` presses; at 760 px and below it is in the Outline sheet. */
export function versionControl(page: Page): Locator {
  return page.getByTestId("issue-version-control");
}

/** The version control's own small control at the right end of the filter row. */
export function versionControlButton(page: Page): Locator {
  return issuePage(page).getByRole("button", { exact: true, name: CONTROL_LABELS.version });
}

/** Opens the version control as a reader does, unless it is open: from its small control at the
 *  right end of the filter row, or, at 760 px and below where the row draws none, by opening the
 *  Outline sheet that holds it. Which of the three the page shows is read once it shows one: a
 *  read taken before the row is drawn finds neither control. Between 761 and 1023 px the row
 *  draws both controls, and the version control's own comes first. */
export async function openVersionControl(page: Page): Promise<void> {
  const shown = await settledState("the version control", [
    ["open", versionControl(page)],
    ["own control", versionControlButton(page)],
    ["Outline sheet", outlineControl(page)],
  ]);
  if (shown === "open") return;
  await (shown === "own control" ? versionControlButton(page) : outlineControl(page)).click();
  await expect(versionControl(page), "the version control did not open").toBeVisible();
}

/** Opens the filter by words' box as a reader does, unless it is drawn: it is drawn while it holds
 *  words, and below 1024 px it stands in the Outline sheet, which this opens. Which of the two the
 *  page shows is read once it shows one. */
export async function openFilterBox(page: Page): Promise<void> {
  const shown = await settledState("the filter by words", [
    ["drawn", filterBox(page)],
    ["Outline sheet", outlineControl(page)],
  ]);
  if (shown === "drawn") return;
  await outlineControl(page).click();
  await expect(filterBox(page), "the Outline sheet draws no filter by words").toBeVisible();
}

/** Which version the version control is showing, as the picker's own value: the empty string
 *  for the current version and the number of a pinned one. The control's text cannot say it - a
 *  `select`'s option texts are part of its container's `innerText`, so "Version 1" is in that
 *  text whichever version is shown - so the value is what is read, open or shut. */
export function versionPicker(page: Page): Locator {
  return versionControl(page).locator(`select[aria-label="${CONTROL_LABELS.version}"]`);
}

/** The document or file opened in place of the outline (`ArtifactRoutePanel.tsx`). */
export function openedArtifact(page: Page): Locator {
  return page.getByTestId("opened-artifact");
}

/** The editor of a document opened in place of the outline. */
export function openedEditor(page: Page): Locator {
  return openedArtifact(page).getByRole("textbox", { name: "Document editor" });
}

/** A control of the page by the role and accessible name the old page's inventory recorded for it
 *  (`e2e/fixtures/issue-page-inventory.json`), which is how c-nothing-lost acts on each name that
 *  writes today. */
export function offered(page: Page, role: Parameters<Page["getByRole"]>[0], name: string): Locator {
  return page.getByRole(role, { exact: true, name });
}

/** What a `select` offers, as its value and its label: a test that writes a property picks the
 *  option it means rather than an index (c-nothing-lost). */
export function optionsOf(control: Locator): Promise<{ label: string; value: string }[]> {
  return control.evaluateAll((found) =>
    found.flatMap((element) =>
      [...(element as HTMLSelectElement).options].map((option) => ({
        label: option.label,
        value: option.value,
      }))
    )
  );
}

/** What the page says in its own words: the text of each `[data-page-chrome]` element with every
 *  `[data-author-text]` descendant removed, which is what c-plain-words reads word by word. The
 *  Outline sheet is the page's too, though it is drawn outside the page's root. */
export function chromeTexts(page: Page): Promise<string[]> {
  return page.evaluate((selectors) => {
    const found: string[] = [];
    for (const element of document.querySelectorAll(
      `${selectors.issuePage} ${selectors.pageChrome}, ${selectors.outlineSheet} ${selectors.pageChrome}`
    )) {
      const copy = element.cloneNode(true) as Element;
      for (const authored of copy.querySelectorAll(selectors.authorText)) authored.remove();
      const text = (copy.textContent ?? "").replace(/\s+/g, " ").trim();
      if (text !== "") found.push(text);
    }
    return found;
  }, SELECTORS);
}

/** Every element of the page - its root and the Outline sheet - that carries visible text and no
 *  `[data-page-chrome]` or `[data-author-text]` ancestor or self, which is text the page divides
 *  into neither. */
export function undividedTexts(page: Page): Promise<string[]> {
  return page.evaluate((selectors) => {
    const found: string[] = [];
    for (const root of document.querySelectorAll(
      `${selectors.issuePage}, ${selectors.outlineSheet}`
    )) {
      const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
      for (let node = walker.nextNode(); node !== null; node = walker.nextNode()) {
        const text = (node.textContent ?? "").replace(/\s+/g, " ").trim();
        if (text === "") continue;
        const element = node.parentElement;
        if (element === null) continue;
        if (element.closest(`${selectors.pageChrome}, ${selectors.authorText}`) !== null) continue;
        if (element.getClientRects().length === 0) continue;
        found.push(text);
      }
    }
    return found;
  }, SELECTORS);
}

/** The filter row as it is drawn (ruling 5: one line at every width). */
export interface FilterRowReading {
  /** The name of each control the row draws, left to right: what a reader hears it called. */
  readonly controls: readonly string[];
  /** Each of those controls' own height in CSS pixels, in the same order. */
  readonly heights: readonly number[];
  /** The row's own height in CSS pixels. */
  readonly height: number;
  /** How many lines its controls stand on: controls whose boxes overlap vertically are one. */
  readonly lines: number;
  /** How far the row's content runs past its own right edge, in px; 0 where it fits. */
  readonly overflow: number;
}

/** `FilterRowReading` of the page, or null where it draws no filter row. A control is a button,
 *  a box or a select the row draws on the page itself: one inside a menu, a dialog or a part
 *  the row keeps shut is not on the row. */
export function filterRowReading(page: Page): Promise<FilterRowReading | null> {
  return page.evaluate((selectors) => {
    const row = document.querySelector(selectors.filterRow);
    if (row === null) return null;
    const controls = [...row.querySelectorAll<HTMLElement>("button, input, select")]
      .filter(
        (control) =>
          control.checkVisibility() &&
          control.closest('[role="menu"], [role="dialog"], [hidden]') === null &&
          control.getAttribute("type") !== "file"
      )
      .map((control) => {
        // The name a reader hears: the label, what names it, or its own words less the parts
        // hidden from them (a control's `▾`).
        const heard = control.cloneNode(true) as HTMLElement;
        for (const hidden of heard.querySelectorAll('[aria-hidden="true"]')) hidden.remove();
        const named = control.getAttribute("aria-labelledby");
        const name =
          control.getAttribute("aria-label") ??
          (named === null
            ? null
            : named
                .split(" ")
                .map((id) => document.getElementById(id)?.textContent ?? "")
                .join(" ")) ??
          heard.textContent ??
          "";
        return { box: control.getBoundingClientRect(), name: name.replace(/\s+/g, " ").trim() };
      })
      .sort((a, b) => a.box.left - b.box.left);
    let lines = 0;
    let lineBottom = Number.NEGATIVE_INFINITY;
    for (const { box } of [...controls].sort((a, b) => a.box.top - b.box.top)) {
      if (box.top >= lineBottom - 1) lines += 1;
      lineBottom = Math.max(lineBottom, box.bottom);
    }
    const edge = row.getBoundingClientRect();
    return {
      controls: controls.map((control) => control.name),
      height: edge.height,
      heights: controls.map((control) => control.box.height),
      lines,
      overflow: Math.max(0, ...controls.map((control) => control.box.right - edge.right)),
    };
  }, SELECTORS);
}

/** One colour the filter row draws: where, and the value the browser computed for it. */
interface DrawnColour {
  /** The element and the property, as a failure names them: `button "Keys" color`. */
  readonly drawn: string;
  /** The computed value, as `getComputedStyle` gives it. */
  readonly value: string;
}

/** Every colour the filter row draws, as the browser computed it, over the row and each of its
 *  visible elements: the text colour of each that draws words or a shape of its own (an `svg`
 *  fills with its `currentColor`), each background, and each border that has a width. An element
 *  that draws no word and no shape draws no text colour, though it computes one. */
function filterRowColours(page: Page): Promise<DrawnColour[]> {
  return page.evaluate((selectors) => {
    const row = document.querySelector<HTMLElement>(selectors.filterRow);
    if (row === null) return [];
    const found: { drawn: string; value: string }[] = [];
    for (const element of [row, ...row.querySelectorAll<Element>("*")]) {
      if (element.closest("[hidden]") !== null || !element.checkVisibility()) continue;
      const style = getComputedStyle(element);
      const name =
        element.getAttribute("aria-label") ??
        (element.textContent ?? "").replace(/\s+/g, " ").trim().slice(0, 40);
      const at = `${element.tagName.toLowerCase()} "${name}"`;
      const drawsWords = [...element.childNodes].some(
        (child) => child.nodeType === Node.TEXT_NODE && (child.textContent ?? "").trim() !== ""
      );
      if (drawsWords || element instanceof SVGSVGElement || element instanceof HTMLInputElement) {
        found.push({ drawn: `${at} color`, value: style.color });
      }
      found.push({ drawn: `${at} background-color`, value: style.backgroundColor });
      for (const side of ["top", "right", "bottom", "left"] as const) {
        if (style.getPropertyValue(`border-${side}-style`) === "none") continue;
        if (Number.parseFloat(style.getPropertyValue(`border-${side}-width`)) === 0) continue;
        found.push({
          drawn: `${at} border-${side}-color`,
          value: style.getPropertyValue(`border-${side}-color`),
        });
      }
    }
    return found;
  }, SELECTORS);
}

/** A computed colour as Chromium writes one - `rgb(…)`, `rgba(…)` or `oklch(…)`, the last when
 *  it was declared in that space - as `[r, g, b, alpha]` in 8-bit sRGB, by the conversion
 *  `e2e/theme.stub.e2e.ts` reads swatches with; undefined for a form this does not read. */
function srgbOf(value: string): readonly [number, number, number, number] | undefined {
  const rgb = /^rgba?\((\d+), (\d+), (\d+)(?:, ([\d.]+))?\)$/u.exec(value);
  if (rgb !== null) {
    return [Number(rgb[1]), Number(rgb[2]), Number(rgb[3]), Number(rgb[4] ?? 1)];
  }
  const oklch = /^oklch\(([\d.]+)(%?) ([\d.]+) ([\d.]+)(?: \/ ([\d.]+))?\)$/u.exec(value);
  if (oklch === null) return undefined;
  const l = Number(oklch[1]) / (oklch[2] === "%" ? 100 : 1);
  const [r, g, b] = oklchToSrgb8({ c: Number(oklch[3]), h: Number(oklch[4]), l });
  return [r, g, b, Number(oklch[5] ?? 1)];
}

/** Each colour the filter row draws that is none of B's `PAGE_COLORS` in `scheme` and not
 *  transparent (part N's R4: the row is drawn in none but those, the prototype's colours), named
 *  with where it is drawn. Each page colour is computed from its swatch as
 *  `e2e/theme.stub.e2e.ts` computes a swatch's 8-bit value (`oklchToSrgb8`). */
export async function offThePalette(page: Page, scheme: "dark" | "light"): Promise<string[]> {
  const palette = new Set(
    Object.values(PAGE_COLORS).map((pair) => oklchToSrgb8(pair[scheme].oklch).join(", "))
  );
  return (await filterRowColours(page)).flatMap(({ drawn, value }) => {
    const srgb = srgbOf(value);
    if (srgb?.[3] === 0) return [];
    if (srgb?.[3] === 1 && palette.has(srgb.slice(0, 3).join(", "))) return [];
    return [`${drawn} is ${value}`];
  });
}

/** How a small control of the filter row is drawn, as the prototype draws Keys (part N's R4): its
 *  border, its corner, its words' size, weight and line, and its colours. */
export interface ControlLook {
  readonly background: string;
  readonly border: string;
  readonly color: string;
  readonly font: string;
  readonly radius: string;
}

/** The look of each button the filter row draws on the page itself, by its name, as
 *  `filterRowReading` names it. */
export function filterRowLooks(page: Page): Promise<Record<string, ControlLook>> {
  return page.evaluate((selectors) => {
    const row = document.querySelector(selectors.filterRow);
    const looks: Record<string, ControlLook> = {};
    for (const control of row?.querySelectorAll<HTMLElement>("button") ?? []) {
      if (!control.checkVisibility()) continue;
      if (control.closest('[role="menu"], [role="dialog"], [hidden]') !== null) continue;
      const heard = control.cloneNode(true) as HTMLElement;
      for (const hidden of heard.querySelectorAll('[aria-hidden="true"]')) hidden.remove();
      const style = getComputedStyle(control);
      looks[(heard.textContent ?? "").replace(/\s+/g, " ").trim()] = {
        background: style.backgroundColor,
        border: `${style.borderTopWidth} ${style.borderTopStyle} ${style.borderTopColor}`,
        color: style.color,
        font: `${style.fontSize} ${style.fontWeight} ${style.lineHeight}`,
        radius: style.borderTopLeftRadius,
      };
    }
    return looks;
  }, SELECTORS);
}

/** The three things inside a node's own text that may scroll sideways (R17): a code block, a
 *  table and a picture. c-reflow names them by selector and leaves them out of its walk. */
export const AUTHOR_SCROLLERS = ["pre", "table", "img"] as const;

/** Each of those three that is drawn outside a `[data-author-text]` element, which is where the
 *  exception does not reach. */
export function scrollersOutsideAuthorText(page: Page): Promise<string[]> {
  return page.evaluate(
    ({ scrollers, selectors }) => {
      const found: string[] = [];
      for (const kind of scrollers) {
        for (const element of document.querySelectorAll(`${selectors.issuePage} ${kind}`)) {
          if (element.closest(selectors.authorText) !== null) continue;
          if (element.getClientRects().length === 0) continue;
          found.push(`${kind} in ${element.parentElement?.tagName.toLowerCase() ?? "?"}`);
        }
      }
      return found;
    },
    { scrollers: AUTHOR_SCROLLERS, selectors: SELECTORS }
  );
}

/** Every row of the tree that holds none of those three, which is the set c-reflow walks with
 *  `assertWhole`. Both facts of each row are read in one pass over the page, so each index is a
 *  row of the same drawing: a row drawn between two reads would move every index after it. */
export async function rowsWithoutAuthorScrollers(page: Page): Promise<Locator[]> {
  const walked = await page.evaluate(
    ({ scrollers, selectors }) =>
      [...document.querySelectorAll(`${selectors.issuePage} ${selectors.nodeRow}`)].flatMap(
        (row, index) =>
          row.querySelector(scrollers.join(", ")) === null && row.getClientRects().length > 0
            ? [index]
            : []
      ),
    { scrollers: AUTHOR_SCROLLERS, selectors: SELECTORS }
  );
  return walked.map((index) => nodeRows(page).nth(index));
}

/**
 * Fails where `scope`, or a box inside it, scrolls sideways, or where `scope` reaches past the
 * box that holds it: R9's question, no part of the page scrolls in two dimensions, for a part
 * that scrolls down by design and folds what it holds behind its own controls - an open
 * document, whose editor scrolls down in a box of its own and whose references are folded
 * (part N's R2). A box counts where it can be scrolled (`overflow-x` of `auto`, `scroll` or
 * `overlay`) and is wider inside than out, as `e2e/unclipped.ts`'s `"scrolls-only"` reading
 * counts one; a code block, a table or a picture inside an author's text may scroll sideways
 * (R17), so a box inside one of those is left out.
 */
export async function assertNoSidewaysScroll(scope: Locator): Promise<void> {
  const scrolls = await scope.evaluate(
    (element, { scrollers, selectors }) => {
      const found: string[] = [];
      for (const node of [element, ...element.querySelectorAll("*")]) {
        const exempt = node.closest(scrollers.join(", "));
        if (exempt !== null && exempt.closest(selectors.authorText) !== null) continue;
        const style = getComputedStyle(node);
        if (!["auto", "scroll", "overlay"].includes(style.overflowX)) continue;
        if (node.scrollWidth <= node.clientWidth + 1) continue;
        const classes = node.getAttribute("class");
        found.push(
          `${node.tagName.toLowerCase()}${classes === null ? "" : `.${classes}`}: ${node.scrollWidth} wide in ${node.clientWidth}`
        );
      }
      const holder = element.parentElement?.getBoundingClientRect();
      const box = element.getBoundingClientRect();
      if (holder !== undefined && box.right > holder.right + 1) {
        found.push(
          `${Math.round(box.right)} past the ${Math.round(holder.right)} its holder ends at`
        );
      }
      return found;
    },
    { scrollers: AUTHOR_SCROLLERS, selectors: SELECTORS }
  );
  expect(scrolls, scrolls.join(", ")).toEqual([]);
}

// ---------------------------------------------------------------------------------------------
// Opening the page.
// ---------------------------------------------------------------------------------------------

/** Opens one of the addresses the old tabs answered, from wherever the page is on that issue:
 *  every one of them still resolves (R19, c-urls), and a spec that only needs to be there goes
 *  straight to it rather than pressing a control that no longer exists. The move is inside the
 *  page, as the tab it replaces was: a reload would drop a staged upload, a held transport and
 *  everything else the page is holding. */
export async function openIssueAddress(page: Page, tab: string): Promise<void> {
  const key = new URL(page.url()).pathname.match(/^\/issues\/([A-Z][A-Z0-9]*-\d+)/)?.[1] ?? "";
  await navigateInApp(page, `/issues/${key}/${tab}`);
}

/** The identity block's one actions control, open. Close, reopen and the control that gives the
 *  issue a parent are behind it now (R11), where the nine-tab page drew them as bare buttons.
 *  The control toggles, so this opens it only while it is shut: a spec that asks twice leaves
 *  the actions on screen rather than hiding them again. */
export async function openIssueActions(page: Page): Promise<void> {
  const control = offered(page, "button", CONTROL_LABELS.issue_actions);
  if ((await expandedState("the issue's actions control", control)) === "shut") {
    await control.click();
  }
}

/**
 * What a page offers a reader, recorded before a redesign and asserted after it: every landmark
 * and every control, each as the role it exposes and the name a screen reader announces for it.
 *
 * The walk is Playwright's own accessibility snapshot (`page.ariaSnapshotJSON`), so a name is the
 * one Playwright computes from `aria-label`, `aria-labelledby`, the element's own text, `title`
 * and `alt`, which is the name `getByRole(role, { name })` resolves and the name a screen reader
 * reads. Nothing here extracts DOM text by hand but the text of the outline's `treeitem` rows,
 * for the one kind of entry a parent row carries (`ParentRowMapping`), and nothing here knows
 * anything about the redesign: this module records what is on a page and asserts that a later
 * page still carries it.
 *
 * The snapshot leaves out what no reader can reach - an element with `display: none`, `hidden`,
 * or `aria-hidden` - and renders only one view of the page at a time, so a page that hides part
 * of itself behind a tab is recorded one view at a time and the views merged (`mergeInventory`).
 */
export interface InventoryEntry {
  /** The accessible name, with every server-generated uuid in it written as `<id>` (`collect`),
   *  and empty for a control that exposes no name at all. */
  readonly name: string;
  readonly role: string;
}

/** The landmarks an inventory records: the regions a reader navigates a page by. */
export const INVENTORY_LANDMARKS = [
  "banner",
  "complementary",
  "contentinfo",
  "form",
  "main",
  "navigation",
  "region",
  "search",
] as const;

/** The controls an inventory records: everything a reader can act on. */
export const INVENTORY_CONTROLS = [
  "button",
  "checkbox",
  "combobox",
  "link",
  "menuitem",
  "option",
  "radio",
  "searchbox",
  "slider",
  "spinbutton",
  "switch",
  "tab",
  "textbox",
] as const;

const inventoryRoles: ReadonlySet<string> = new Set<string>([
  ...INVENTORY_LANDMARKS,
  ...INVENTORY_CONTROLS,
]);

/** One entry with where it sits, which is how reachability is read. A node the snapshot gave no
 *  box for is recorded with none and only its presence is asserted. */
interface MeasuredEntry extends InventoryEntry {
  readonly box?: Box;
}

/** `page.ariaSnapshotJSON` answers a free-form value, so every field is read defensively. */
interface AriaSnapshotNode {
  readonly box?: unknown;
  readonly children?: unknown;
  readonly name?: unknown;
  readonly role?: unknown;
}

function readBox(value: unknown): Box | undefined {
  if (typeof value !== "object" || value === null) return undefined;
  const { height, width, x, y } = value as Record<string, unknown>;
  if (
    typeof height !== "number" ||
    typeof width !== "number" ||
    typeof x !== "number" ||
    typeof y !== "number"
  ) {
    return undefined;
  }
  return { height, width, x, y };
}

/**
 * What a name carries that belongs to the run rather than to the page, and what it is recorded
 * as. A control names the row it acts on - `Copy reference dispatch://LAY-2/comment/<id>`, `2
 * replies · last reply <time>` - so its name holds the uuid the server gave that row and the
 * relative time `Timestamp` draws for it (`web/src/features/refs/Timestamp.tsx`), and neither is a
 * property of the page: the uuid is new in every run, and the time crosses from `just now` to `1
 * minute ago` while the recording is still being made. Recording them as placeholders is what
 * makes two runs of one page give the same list.
 */
const runSpecific: readonly (readonly [RegExp, string])[] = [
  [/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/g, "<id>"],
  [
    /just now|yesterday|tomorrow|(?:last|next) (?:week|month|year)|(?:in )?\d+ (?:second|minute|hour|day|week|month|year)s?(?: ago)?/g,
    "<time>",
  ],
];

/** Every landmark and control of one accessibility snapshot, in the order the tree holds them. */
function collect(node: unknown, found: MeasuredEntry[]): void {
  if (Array.isArray(node)) {
    for (const child of node) collect(child, found);
    return;
  }
  if (typeof node !== "object" || node === null) return;
  const { box, children, name, role } = node as AriaSnapshotNode;
  if (typeof role === "string" && inventoryRoles.has(role)) {
    const announced =
      typeof name === "string"
        ? runSpecific.reduce(
            (text, [carried, placeholder]) => text.replace(carried, placeholder),
            name
          )
        : "";
    found.push({ box: readBox(box), name: announced, role });
  }
  collect(children, found);
}

/** The key two entries are the same control under: its role and its name. */
function entryKey(entry: InventoryEntry): string {
  return `${entry.role}\u0000${entry.name}`;
}

/** How an entry is written where a person reads it: in a failure, and as both halves of the
 *  fixture's rename table, so a rename is pasted straight from the failure that named it. */
function describe(entry: InventoryEntry): string {
  return `${entry.role} "${entry.name}"`;
}

/** The entry `described` names, read back from the form `describe` writes. A value that is not
 *  that form names no entry, and the caller fails rather than quietly looking for nothing. */
function entryOf(described: string): InventoryEntry | undefined {
  const opened = described.indexOf(' "');
  if (opened <= 0 || !described.endsWith('"')) return undefined;
  return { name: described.slice(opened + 2, -1), role: described.slice(0, opened) };
}

/**
 * One list out of however many views were recorded: every `{role, name}` once, ordered by role
 * and then by name, so the view order, the DOM order and two runs of the same page all give the
 * same list.
 */
export function mergeInventory(entries: readonly InventoryEntry[]): InventoryEntry[] {
  const byKey = new Map<string, InventoryEntry>();
  for (const entry of entries) {
    byKey.set(entryKey(entry), { name: entry.name, role: entry.role });
  }
  return [...byKey.values()].sort(
    (a, b) => a.role.localeCompare(b.role) || a.name.localeCompare(b.name)
  );
}

/** How long one reading waits before it is taken again, and how long the page has to stop. */
const SETTLE_INTERVAL_MS = 400;

const SETTLE_TIMEOUT_MS = 20_000;

/** Every landmark and every control the page offers, de-duplicated and ordered, once the page has
 *  stopped changing them. */
export async function recordInventory(page: Page): Promise<InventoryEntry[]> {
  return mergeInventory(await settled(() => measured(page.ariaSnapshotJSON({ boxes: true }))));
}

/** The same walk over one part of the page, which is how a view that keeps its other panels
 *  `hidden` records what one panel of it offers. */
export async function recordScopeInventory(scope: Locator): Promise<InventoryEntry[]> {
  return mergeInventory(await settled(() => measured(scope.ariaSnapshotJSON({ boxes: true }))));
}

/**
 * One reading of `read` once two readings `SETTLE_INTERVAL_MS` apart agree. A panel still filling
 * - a reference chip that arrives with its own read, a card the list mounts late - is on the page
 * after the network has gone quiet, so a single reading records a half-drawn page and two runs of
 * it differ. A page that never stops changing fails here rather than being recorded mid-render.
 */
async function settled(read: () => Promise<MeasuredEntry[]>): Promise<MeasuredEntry[]> {
  const deadline = Date.now() + SETTLE_TIMEOUT_MS;
  let before = mergeInventory(await read())
    .map(describe)
    .join("\n");
  for (;;) {
    const { promise, resolve } = Promise.withResolvers<void>();
    setTimeout(resolve, SETTLE_INTERVAL_MS);
    await promise;
    const entries = await read();
    const after = mergeInventory(entries).map(describe).join("\n");
    if (after === before) return entries;
    expect(
      Date.now() <= deadline,
      `what the page offers kept changing for ${SETTLE_TIMEOUT_MS} ms; it last went from\n${before}\nto\n${after}`
    ).toBe(true);
    before = after;
  }
}

async function measured(snapshot: Promise<unknown>): Promise<MeasuredEntry[]> {
  const found: MeasuredEntry[] = [];
  collect(await snapshot, found);
  return found;
}

/**
 * One link of the old page from a node to another node, which the new page carries by drawing
 * the target as that node's parent row, the row the left arrow reaches from it - the one form of
 * carrying an entry that is no landmark and no control of its own. A decision's card linked the
 * ask it answers; the outline draws the decision under that ask instead (R4), so the link's job
 * is done by the tree itself, and the entry is carried exactly when the tree does it.
 */
export interface ParentRowMapping {
  /** Words of the node the old link sat on, which its row holds. */
  readonly node: string;
  /** Words of the node the old link pointed at, which the node's parent row holds. */
  readonly parent: string;
  /** Why the parent row stands in for the link. */
  readonly reason: string;
}

export interface CarriedOptions {
  /**
   * The fixture's rename table: each recorded entry, written as the `role "name"` a failure
   * prints it as, mapped to the entry the page offers in its place, in the same form. One form
   * serves a name that changed and a role that changed with it - `tab "Children"` became
   * `button "Children"` when the tabs became presets - and a rename is pasted straight from
   * the failure that named it. Where the page offers a different entry at a different width -
   * the preset row is a row of tabs on a desktop and one menu on a phone - the value names
   * each of them, joined by ` | `, and the entry is carried while any one of them is there. A
   * renamed entry is looked for as the entries it maps to and nothing else, so a rename is not
   * reported as a loss; a value that is not that form fails the check rather than quietly
   * matching nothing.
   */
  readonly renames: Readonly<Record<string, string>>;
  /** Recorded entries, in the same `role "name"` form, that only a part not yet on the page
   *  carries (`AWAITING_THE_BOX`): none is looked for, and one the page offers fails the check,
   *  since that part's own pull request is what moves it back into the reading. */
  readonly awaiting?: readonly string[];
  /**
   * The fixture's links carried by a parent row (`ParentRowMapping`), keyed by the recorded
   * entry in the same `role "name"` form. Such an entry is looked for in the outline's
   * `treeitem` rows and nowhere else: it is carried when a row holding the node's words is drawn
   * under a row holding the target's words, one level up, and the left arrow from that row
   * reaches that parent. Every other entry is read as a landmark or a control, as above.
   */
  readonly parentRows?: Readonly<Record<string, ParentRowMapping>>;
  /** Puts the page where the outline draws the rows a parent-row mapping names, both of them:
   *  the walk's own views leave the page wherever the last one did. It returns once that outline
   *  is drawn - a load whose reads went quiet with every branch open, or below 1024 px the
   *  parent's own zoom drawn - since its rows are read the moment it returns. */
  readonly outline?: (mapping: ParentRowMapping) => Promise<void>;
  /** Which recorded state this is (`open-1440`), named in every failure. */
  readonly state: string;
  /**
   * The page's own views, each a call that puts the page in one of them and answers whether
   * that view is there. The recording is the merge of the views the old page had - its nine
   * tab panels, one at a time, because a page renders one and hides the rest
   * (`record-page-inventory.e2e.ts`) - so the page it is read against is merged the same way,
   * or a page that still offers everything would be read as having lost whatever its first
   * view does not show. Each view is read in turn and the readings merged, and every one of
   * them passes `assertPageFits` on its own.
   *
   * A view the page does not have at this width - the eighth row of a tree that draws five, a
   * control a wider page never folds away - answers `false` and leaves the page exactly as the
   * view before it left it, and the walk reads nothing for it: a reading there would record the
   * view before it a second time, under its own settle and its own fit. So a walk may name
   * more views than a width has without paying for the ones it does not, and a walk that
   * reaches no view at all fails the check below for want of anything to merge.
   */
  readonly views: readonly (() => Promise<boolean>)[];
}

/** How many presses of the left arrow reach a parent row: a row that holds rows of its own and
 *  is open folds on the first press, and moves to its parent on the second. */
const LEFT_ARROW_PRESSES = 2;

/**
 * Whether the outline carries one parent-row mapping: a `treeitem` holding the node's words,
 * drawn one level under the nearest `treeitem` above it that holds the target's words, from
 * which the left arrow moves the focus to that parent. Undefined where it does, and otherwise
 * what the rows showed instead.
 */
async function parentRowMissing(
  page: Page,
  mapping: ParentRowMapping
): Promise<string | undefined> {
  const rows = page.getByRole("treeitem");
  // The outline callback returns once the outline it opens is drawn (`CarriedOptions.outline`),
  // so one read lists every row it draws.
  const drawn = await rows.evaluateAll((items) =>
    items.map((item) => ({
      level: Number(item.getAttribute("aria-level")),
      text: (item.textContent ?? "").replace(/\s+/g, " "),
    }))
  );
  const placements = drawn.flatMap((row, at) => {
    if (!row.text.includes(mapping.node)) return [];
    let parent = at - 1;
    while (parent >= 0 && drawn[parent]?.level !== row.level - 1) parent -= 1;
    return [{ at, parent }];
  });
  if (placements.length === 0) return `no row of the outline holds "${mapping.node}"`;
  const under = placements.find(
    ({ parent }) => drawn[parent]?.text.includes(mapping.parent) === true
  );
  if (under === undefined) {
    const parents = placements.map(({ parent }) => drawn[parent]?.text ?? "the outline's top");
    return `"${mapping.node}" is drawn under ${JSON.stringify(parents)}, not under "${mapping.parent}"`;
  }
  await rows.nth(under.at).focus();
  for (let press = 0; press < LEFT_ARROW_PRESSES; press += 1) {
    await page.keyboard.press("ArrowLeft");
    // Where the whole tree is drawn the arrow moves the focus inside its own keydown, and in the
    // parent's zoom the callback opened below 1024 px it moves none, so one read is settled.
    const reached = await rows
      .nth(under.parent)
      .evaluate((row) => row === document.activeElement || row.contains(document.activeElement));
    if (reached) return undefined;
  }
  return `the left arrow from "${mapping.node}" does not reach its parent row "${mapping.parent}"`;
}

/**
 * Nothing recorded was lost: every recorded `{role, name}`, under the name the rename table gives
 * it, is a landmark or control of the page as it stands, and a reader reaches each of them
 * without scrolling sideways - the page is no wider than its viewport (`assertPageFits`) and
 * every instance of the entry sits inside the viewport's own width, so nothing is reached by
 * scrolling a row or a card sideways.
 *
 * What a state lost fails the row softly (`expect.soft`): the row goes on to the next state it
 * reads and fails at its end, so one run names the losses of every state rather than those of
 * the first state that lost anything.
 *
 * It asserts nothing about the redesign: a page may offer more than it did, in any order.
 */
export async function assertInventoryCarried(
  page: Page,
  recorded: readonly InventoryEntry[],
  options: CarriedOptions
): Promise<void> {
  // One row per recorded entry, each naming every entry that carries it: itself, or whatever
  // the rename table says stands in its place. An entry a parent row carries is read off the
  // outline's rows below, and is no row of this list.
  const seen = new Set<string>();
  const wanted: { carried: InventoryEntry[]; described: string; instances: MeasuredEntry[] }[] = [];
  const byParentRow: { described: string; mapping: ParentRowMapping }[] = [];
  for (const entry of recorded) {
    const described = describe(entry);
    if (seen.has(described)) continue;
    seen.add(described);
    if (options.awaiting?.includes(described) === true) continue;
    const mapping = options.parentRows?.[described];
    if (mapping !== undefined) {
      byParentRow.push({ described, mapping });
      continue;
    }
    const renamed = options.renames[described];
    const carried =
      renamed === undefined ? [entry] : renamed.split(" | ").map((one) => entryOf(one));
    expect(
      carried.every((one) => one !== undefined),
      `${options.state}: the rename of ${described} reads ${JSON.stringify(renamed)}, which is no \`role "name"\``
    ).toBe(true);
    wanted.push({
      carried: carried.filter((one): one is InventoryEntry => one !== undefined),
      described,
      instances: [],
    });
  }
  const present = new Map<string, MeasuredEntry[]>();
  let read = 0;
  for (const view of options.views) {
    if ((await view()) === false) continue;
    await assertPageFits(page);
    read += 1;
    for (const entry of await settled(() => measured(page.ariaSnapshotJSON({ boxes: true })))) {
      const key = entryKey(entry);
      present.set(key, [...(present.get(key) ?? []), entry]);
    }
  }
  expect(
    read,
    `${options.state}: every one of the ${options.views.length} views answered that it is not there, so nothing was read`
  ).toBeGreaterThan(0);
  for (const one of wanted) {
    one.instances = one.carried.flatMap((entry) => present.get(entryKey(entry)) ?? []);
  }
  const lost = wanted.filter((one) => one.instances.length === 0).map((one) => one.described);
  // Each link a parent row carries, read once the views are done: the left arrow moves the
  // focus and may fold a row, which no view's own reading should meet.
  for (const { described, mapping } of byParentRow) {
    expect(
      options.outline,
      `${options.state}: ${described} is carried by a parent row, and the check names no way to the outline`
    ).toBeDefined();
    await options.outline?.(mapping);
    const missing = await parentRowMissing(page, mapping);
    if (missing !== undefined) lost.push(`${described} (${missing})`);
  }
  expect.soft(lost, `${options.state} no longer offers: ${lost.join(", ")}`).toEqual([]);
  const early = (options.awaiting ?? []).filter((described) => {
    const entry = entryOf(described);
    return entry !== undefined && (present.get(entryKey(entry)) ?? []).length > 0;
  });
  expect
    .soft(early, `${options.state} offers what only part L's box carries: ${early.join(", ")}`)
    .toEqual([]);

  const viewport = await page.evaluate(() => document.documentElement.clientWidth);
  const sideways = wanted
    .filter((one) => {
      // A control a page hides from sight and keeps for the keyboard - the shell's "Skip to
      // content", which is `sr-only` until it is focused - is a 1 by 1 clipped box wherever it
      // sits. It is reached by Tab and not by scrolling, so its box says nothing about
      // scrolling sideways and the rule passes over it.
      const placed = one.instances.filter(
        (instance) =>
          instance.box !== undefined && instance.box.width > 1 && instance.box.height > 1
      );
      return (
        placed.length > 0 &&
        placed.every(({ box }) => box !== undefined && (box.x < 0 || box.x + box.width > viewport))
      );
    })
    .map((one) => one.described);
  expect(
    sideways,
    `${options.state} reaches these only by scrolling sideways in a ${viewport}px viewport: ${sideways.join(", ")}`
  ).toEqual([]);
}

/**
 * The rename table of the inventory row (part N's R7): each entry today's page offered, written
 * as the `role "name"` a failure prints it as, mapped to what the new page offers in its place,
 * from the Design's table "Where each thing goes" and ruling 5. The checkpoint kept it in the
 * fixture, which the recorder writes again after each merge of `main`; declared here, a new
 * recording does not lose it.
 */
export const INVENTORY_RENAMES: Readonly<Record<string, string>> = {
  'tab "Spec"': 'button "Spec" | menuitem "Spec"',
  'tab "Verification"': 'button "Verification" | menuitem "Verification"',
  'tab "Plan"': 'button "Plan" | menuitem "Plan"',
  'tab "Conversation"': 'region "Conversation"',
  'tab "Children"': 'button "Children"',
  'tab "Artifacts 7"': 'button "Files and documents"',
  'tab "Activity"': 'region "Activity"',
  'tab "Decisions"': 'button "Decisions" | menuitem "Decisions"',
  'tab "Collaborators"': 'button "Collaborators"',
  'button "Reopen issue"': 'button "Reopen into Backlog"',
  'button "Upload"': 'menuitem "Upload"',
  'button "Upload artifact"': 'button "Upload a file"',
  'searchbox "Filter artifacts"': 'searchbox "Filter files and documents"',
  'button "New document"': 'menuitem "New document"',
  'link "Block r1"': 'link "Every property of the issue that carries a value is visible …"',
  'link "Block r2"': 'link "Requirement 2 of the redesign stands on a row of its own."',
  'link "Block r3"': 'link "Requirement 3 of the redesign stands on a row of its own."',
  'link "Block r4"': 'link "Requirement 4 of the redesign stands on a row of its own."',
  'link "Block r5"': 'link "Requirement 5 of the redesign stands on a row of its own."',
  'region "Open questions"': 'region "Decisions"',
  'region "Waiting for your review"': 'region "Decisions"',
  'region "Your decisions"': 'region "Decisions"',
  'region "Replaced"': 'region "Decisions"',
  'link "session:e2e-seed…"': 'link "Recorder (issue layout)"',
  'button "Hide margin"': 'button "Close"',
  'button "Hide sidebar"': 'button "Open navigation"',
  'complementary "Navigation"': 'navigation "App"',
  'banner ""': 'navigation "Navigation"',
  'button "Open review panel (1 open ask)"': 'button "Details"',
  'button "Open review panel (0 open asks)"': 'button "Details"',
  'complementary "Review panel"': 'complementary "Review margin"',
};

/** Why a rename is the same control under a new name, keyed as `INVENTORY_RENAMES` is, for a
 *  rename that no view-to-control or block-to-words rule explains on its own. */
export const INVENTORY_RENAME_REASONS: Readonly<Record<string, string>> = {
  'button "Hide margin"':
    "The margin's collapse is Details' Close now: the panel opens over the right of the page as a drawer and closing it is its collapse (R14), the same control under the drawer's word.",
  'button "Hide sidebar"':
    "On the issue page the sidebar is the 56 px icon rail (R14), already collapsed; its one control over the navigation is the rail's foot, Open navigation, which opens the whole list over the page, or the header's on a phone.",
  'complementary "Navigation"':
    "The sidebar's landmark on the issue page is the icon rail's (R14), a navigation landmark named App so it is not taken for the whole navigation the rail's foot opens.",
  'banner ""':
    "On a phone the shell's bar was the issue page's banner, holding Dispatch, the inbox count and Menu; on the issue page the header's `☰` opens the navigation that holds them (R14), so the bar is that navigation now.",
  'button "Open review panel (1 open ask)"':
    "The phone's control that opened the panel is the header's Details control (R14); the panel it opens is the same.",
  'button "Open review panel (0 open asks)"':
    "As above, on the closed issue, which waits on no ask.",
  'complementary "Review panel"':
    "The phone's review sheet is the margin inside Details, which on the issue page is one box at every width and named as the margin is (R14); Details opens it over the whole screen.",
};

/** Links of today's page that a parent row of the outline carries (`ParentRowMapping`), keyed
 *  as `INVENTORY_RENAMES` is. */
export const INVENTORY_PARENT_ROWS: Readonly<Record<string, ParentRowMapping>> = {
  'link "The ask it answers"': {
    node: "Keep every address the old page answered",
    parent: "Which addresses must the new page still answer?",
    reason:
      "A decision's card linked the ask it answers; the outline draws the decision under that ask, its parent row, which the left arrow reaches (R4).",
  },
};

/**
 * What only part L's box carries (part N's R11): today's composer, as the fixture recorded it in
 * every state. While pull request 3125 and part L have not merged, `slots/foot.tsx` draws
 * nothing, so the inventory row looks for none of these and fails if the page offers one. Part
 * L's pull request empties this list, and the row then reads each of them again.
 */
export const AWAITING_THE_BOX: readonly string[] = [
  'form "Comment composer"',
  'textbox "Comment"',
  'button "Send"',
  'button "Messages default to Planner (issue layout)"',
];

// ---------------------------------------------------------------------------------------------
// INV-1: every address of today's page (part N's R6).
// ---------------------------------------------------------------------------------------------

/** The records of one seeded issue the addresses of `OLD_PAGE_ADDRESSES` are written with: the
 *  whole issue's (`seedWholeIssue`), and what the row that walks them writes beside it. */
export interface AddressSeed {
  /** A thread an approval request opened on the spec, which the outline draws no node for, and
   *  the spec whose approval the header draws. */
  readonly approval: { readonly askId: string; readonly artifactId: string };
  /** A comment an agent wrote outside every thread a person wrote in, a turn of the history. */
  readonly agentsOnly: string;
  /** The comment anchored in the spec, its node's words, and its id written short. */
  readonly comment: { readonly id: string; readonly short: string; readonly words: string };
  /** A message the Legion daemon of the issue's project wrote with no target, which begins a
   *  thread the history holds whole (`agents-only.ts`), and its words. */
  readonly daemon: { readonly id: string; readonly words: string };
  /** The id of no ask at all. */
  readonly goneAsk: string;
  /** A handoff the issue holds (INFRA-4870's view of it). */
  readonly handoff: string;
  readonly key: string;
  /** The root message of the issue's conversation and its words. */
  readonly message: { readonly id: string; readonly words: string };
  /** The document a requirement of the spec cites, which no view opens: its slug, the words of
   *  its one paragraph, and that paragraph's block, which no node of the outline holds. */
  readonly notes: { readonly paragraph: string; readonly slug: string; readonly words: string };
  /** The open question the spec's `ask` block was indexed into: its ask, the node the outline
   *  draws for it, which is that block's, its words, and its id written short. */
  readonly openQuestion: {
    readonly askId: string;
    readonly node: string;
    readonly short: string;
    readonly words: string;
  };
  /** A comment of another issue, which an address under this issue still names. */
  readonly otherIssue: { readonly comment: string; readonly key: string; readonly words: string };
  /** The plan's last task: the node the outline draws for its typed block, and that block. */
  readonly planTask: { readonly block: string; readonly node: string };
  /** The spec's slug and words its text holds. */
  readonly spec: { readonly slug: string; readonly words: string };
  /** A document of tasks that is no plan of the issue, so no node of the outline stands for any
   *  block of it (decision 449): its slug, a typed block far down its text, which is off screen
   *  until a link to it scrolls there, and that block's words. */
  readonly tasks: { readonly block: string; readonly slug: string; readonly words: string };
  /** A prefix two comments of the issue carry, and the two. */
  readonly twins: { readonly ids: readonly string[]; readonly prefix: string };
}

/** What an address of today's page opens on the new page (INV-1), as the row reads it. */
export type AddressContent =
  /** That view on, with no document open in place of the outline. */
  | { readonly kind: "view"; readonly preset: Preset }
  /** Documents' list, drawn in place of the outline and of every view, with Documents the
   *  control that is on. */
  | { readonly kind: "documents" }
  /** A document in place of the outline and of every view, drawing `words` of its text. */
  | { readonly kind: "document"; readonly words: string }
  /** That document opened at `block`, a block no node of the outline holds, with the focus on it:
   *  on screen, the document drawn in place of Documents' list and of the outline. */
  | { readonly kind: "document-block"; readonly block: string; readonly words: string }
  /** That document opened with the ask or comment `id` marked in it. */
  | { readonly kind: "document-item"; readonly id: string }
  /** A version of the view's document, pinned: the version control shows it. */
  | { readonly kind: "version"; readonly version: number }
  /** The filter by words, holding `words`. */
  | { readonly kind: "words"; readonly words: string }
  /** The outline, with `node`'s row focused, on screen, drawing `words` where they are given. */
  | { readonly kind: "node"; readonly node: string; readonly words?: string }
  /** The outline, with the first row of the issue's children focused, on screen. */
  | { readonly kind: "children" }
  /** Details, open at its history, at the issue's collaborators, or at its steps. */
  | { readonly kind: "details"; readonly part: "collaborators" | "history" | "steps" }
  /** Details' history, the turn `id` marked in it, drawing `words`; no row of the outline. */
  | { readonly kind: "history-turn"; readonly id: string; readonly words: string }
  /** The header's approval of the document `artifact`, focused. */
  | { readonly kind: "approval"; readonly artifact: string }
  /** The items a short id names, listed to choose from. */
  | { readonly kind: "matches"; readonly ids: readonly string[] }
  /** The page's line that the issue holds no such item, under `heading`. */
  | { readonly kind: "not-found"; readonly heading: string }
  /** The line that the item could not be read, with its Retry. */
  | { readonly kind: "retry" }
  /** The view row's control `name` is the one that is on: the view of a part this branch may not
   *  hold (`PART_ADDRESSES`). */
  | { readonly kind: "control"; readonly name: string }
  /** The handoff's own view, drawn at its address with no refusal (INFRA-4870). */
  | { readonly kind: "handoff" }
  /** The address replaced by `to`, which then opens `lands`: an id written short replaced by the
   *  whole id, and another issue's item at that issue's address. */
  | { readonly kind: "replaced"; readonly lands: AddressContent; readonly to: string };

/** The parts that build a view an address of INV-1 opens, none of which this part builds: each
 *  by its issue or its letter in the plan. */
export type AddressPart = "INFRA-4870" | "INFRA-5702" | "INFRA-5704" | "T";

/** One address of today's page, and what it opens on the new page. */
export interface AddressCase {
  /** The address, on the issue `seed` holds. */
  readonly address: (seed: AddressSeed) => string;
  /** The part that builds the view this address opens, where it is not this part's: the row
   *  reads the case only where the page draws that part (`drawsPart`), and names the address as
   *  not read otherwise. */
  readonly builtBy?: AddressPart;
  /** What it opens. */
  readonly opens: (seed: AddressSeed) => AddressContent;
  /** The read the row answers with a server error for this address alone: the lookup that
   *  fails, by its URL. */
  readonly unreadable?: (seed: AddressSeed) => (url: URL) => boolean;
  /** The part whose view takes this address from what `opens` says: the row leaves the case
   *  out once the page draws that part, whose own case reads the address. */
  readonly until?: AddressPart;
}

/** The issue's own address on `seed`, with `rest` after it. */
function issuePath(seed: AddressSeed, rest = ""): string {
  return `/issues/${seed.key}${rest}`;
}

/**
 * INV-1 (part N's R6): every address today's page answers, by the kind of route it parses to
 * (`refs/routes.ts`), each with the content it opens on the new page. A kind added to
 * `IssueRoute` fails the type check here until it has its addresses. The forms ride on the kind
 * whose address carries them: `?q=`, `?ask=`, `?comment=` and a `#b-` of a typed block on the
 * issue's own; `/spec?comment=` and `?v=` on the spec's; a `#b-` of a block of a document no view
 * opens, and `?mark=`, on a document's; an id written short, an id that names several items, a
 * gone id, a failed lookup, another issue's item and an approval request on an item's. Row (1)
 * of `c-addresses`, "every address of the old page still resolves and each new one opens its
 * view", walks it and `PART_ADDRESSES`.
 */
export const OLD_PAGE_ADDRESSES: Readonly<Record<IssueRoute["kind"], readonly AddressCase[]>> = {
  activity: [
    {
      address: (seed) => issuePath(seed, "/activity"),
      opens: () => ({ kind: "details", part: "history" }),
    },
  ],
  artifact: [
    {
      address: (seed) => issuePath(seed, `/artifacts/${seed.notes.slug}`),
      opens: (seed) => ({ kind: "document", words: seed.notes.words }),
    },
    {
      address: (seed) => issuePath(seed, `/artifact/${seed.notes.slug}`),
      opens: (seed) => ({ kind: "document", words: seed.notes.words }),
    },
    {
      address: (seed) => issuePath(seed, `/artifacts/${seed.spec.slug}`),
      opens: (seed) => ({ kind: "document", words: seed.spec.words }),
    },
    {
      // An item named beside a document is a node of the outline, wherever it is anchored.
      address: (seed) => issuePath(seed, `/artifacts/${seed.spec.slug}?comment=${seed.comment.id}`),
      opens: (seed) => ({ kind: "node", node: seed.comment.id, words: seed.comment.words }),
    },
    {
      address: (seed) => issuePath(seed, `/artifacts/${seed.notes.slug}#b-${seed.notes.paragraph}`),
      opens: (seed) => ({
        block: seed.notes.paragraph,
        kind: "document-block",
        words: seed.notes.words,
      }),
    },
    {
      // Decision 449: a block of a document the outline does not draw - a task of a document of
      // tasks that is no plan, far down its text, which no node of the outline holds - opens that
      // document in place of Documents' list and of the outline, on that block.
      address: (seed) => issuePath(seed, `/artifacts/${seed.tasks.slug}#b-${seed.tasks.block}`),
      opens: (seed) => ({
        block: seed.tasks.block,
        kind: "document-block",
        words: seed.tasks.words,
      }),
    },
    {
      address: (seed) => issuePath(seed, `/artifacts/${seed.spec.slug}?mark=${seed.comment.id}`),
      opens: (seed) => ({ id: seed.comment.id, kind: "document-item" }),
    },
    {
      address: (seed) => issuePath(seed, `/artifacts/${seed.spec.slug}?mark=${seed.comment.short}`),
      opens: (seed) => ({
        kind: "replaced",
        lands: { id: seed.comment.id, kind: "document-item" },
        to: issuePath(seed, `/artifacts/${seed.spec.slug}?mark=${seed.comment.id}`),
      }),
    },
  ],
  artifacts: [
    { address: (seed) => issuePath(seed, "/artifacts"), opens: () => ({ kind: "documents" }) },
  ],
  ask: [
    {
      address: (seed) => issuePath(seed, `/asks/${seed.openQuestion.askId}`),
      opens: (seed) => ({
        kind: "node",
        node: seed.openQuestion.node,
        words: seed.openQuestion.words,
      }),
    },
    {
      address: (seed) => issuePath(seed, `/asks/${seed.openQuestion.short}`),
      opens: (seed) => ({
        kind: "replaced",
        lands: { kind: "node", node: seed.openQuestion.node, words: seed.openQuestion.words },
        to: issuePath(seed, `/asks/${seed.openQuestion.askId}`),
      }),
    },
    {
      address: (seed) => issuePath(seed, `/asks/${seed.goneAsk}`),
      opens: () => ({ heading: "Question not found", kind: "not-found" }),
    },
    {
      // An approval request is no node of the outline: the header draws the approval of the
      // document it reviews, and the address stays the request's.
      address: (seed) => issuePath(seed, `/asks/${seed.approval.askId}`),
      opens: (seed) => ({ artifact: seed.approval.artifactId, kind: "approval" }),
    },
  ],
  children: [
    { address: (seed) => issuePath(seed, "/children"), opens: () => ({ kind: "children" }) },
  ],
  collaborators: [
    {
      address: (seed) => issuePath(seed, "/collaborators"),
      opens: () => ({ kind: "details", part: "collaborators" }),
    },
  ],
  comment: [
    {
      address: (seed) => issuePath(seed, `/comments/${seed.comment.id}`),
      opens: (seed) => ({ kind: "node", node: seed.comment.id, words: seed.comment.words }),
    },
    {
      address: (seed) => issuePath(seed, `/comments/${seed.agentsOnly}`),
      opens: (seed) => ({
        id: seed.agentsOnly,
        kind: "history-turn",
        words: "Planner to reviewer",
      }),
    },
    {
      address: (seed) => issuePath(seed, `/comments/${seed.twins.prefix}`),
      opens: (seed) => ({ ids: seed.twins.ids, kind: "matches" }),
    },
    {
      address: (seed) => issuePath(seed, `/comments/${seed.comment.short}`),
      opens: () => ({ kind: "retry" }),
      // The lookup of the id written short, which the server resolves within the issue.
      unreadable: (seed) => (url) =>
        url.pathname === "/api/v1/references" &&
        url.searchParams.get("to") === `dispatch://${seed.key}/comment/${seed.comment.short}`,
    },
    {
      address: (seed) => issuePath(seed, `/comments/${seed.otherIssue.comment}`),
      opens: (seed) => ({
        kind: "replaced",
        lands: { kind: "node", node: seed.otherIssue.comment, words: seed.otherIssue.words },
        to: `/issues/${seed.otherIssue.key}/comments/${seed.otherIssue.comment}`,
      }),
    },
  ],
  conversation: [
    {
      address: (seed) => issuePath(seed, "/conversation"),
      opens: () => ({ kind: "view", preset: "conversation" }),
    },
  ],
  decisions: [
    {
      address: (seed) => issuePath(seed, "/decisions"),
      opens: () => ({ kind: "view", preset: "decisions" }),
    },
  ],
  issue: [
    {
      address: (seed) => issuePath(seed),
      opens: () => ({ kind: "view", preset: "conversation" }),
      // Until Dashboard is drawn, which opens the bare address (`PART_ADDRESSES`).
      until: "INFRA-5702",
    },
    {
      address: (seed) => issuePath(seed, `?q=${encodeURIComponent("Requirement 2")}`),
      opens: () => ({ kind: "words", words: "Requirement 2" }),
    },
    {
      address: (seed) => issuePath(seed, `?ask=${seed.openQuestion.askId}`),
      opens: (seed) => ({
        kind: "node",
        node: seed.openQuestion.node,
        words: seed.openQuestion.words,
      }),
    },
    {
      address: (seed) => issuePath(seed, `?comment=${seed.comment.id}`),
      opens: (seed) => ({ kind: "node", node: seed.comment.id, words: seed.comment.words }),
    },
    {
      address: (seed) => issuePath(seed, `?comment=${seed.comment.short}`),
      opens: (seed) => ({
        kind: "replaced",
        lands: { kind: "node", node: seed.comment.id, words: seed.comment.words },
        to: issuePath(seed, `?comment=${seed.comment.id}`),
      }),
    },
    {
      // A typed block of a document the issue's own address opens no view of first.
      address: (seed) => issuePath(seed, `#b-${seed.planTask.block}`),
      opens: (seed) => ({ kind: "node", node: seed.planTask.node }),
    },
  ],
  // The new page's own address of Documents, which opens the same list as `/artifacts`.
  "issue-documents": [
    { address: (seed) => issuePath(seed, "/documents"), opens: () => ({ kind: "documents" }) },
  ],
  message: [
    {
      address: (seed) => issuePath(seed, `/messages/${seed.message.id}`),
      opens: (seed) => ({ kind: "node", node: seed.message.id, words: seed.message.words }),
    },
    {
      // A turn of a thread the Legion daemon began opens in Details' history, and no row of the
      // outline draws it (part M).
      address: (seed) => issuePath(seed, `/messages/${seed.daemon.id}`),
      opens: (seed) => ({
        id: seed.daemon.id,
        kind: "history-turn",
        words: seed.daemon.words,
      }),
    },
  ],
  plan: [
    {
      address: (seed) => issuePath(seed, "/plan"),
      opens: () => ({ kind: "view", preset: "plan" }),
    },
  ],
  spec: [
    {
      address: (seed) => issuePath(seed, "/spec"),
      opens: () => ({ kind: "view", preset: "spec" }),
    },
    {
      address: (seed) => issuePath(seed, `/spec?comment=${seed.comment.id}`),
      opens: (seed) => ({ kind: "node", node: seed.comment.id, words: seed.comment.words }),
    },
    {
      address: (seed) => issuePath(seed, "/spec?v=1"),
      opens: () => ({ kind: "version", version: 1 }),
    },
  ],
  steps: [
    {
      address: (seed) => issuePath(seed, "/steps"),
      opens: () => ({ kind: "details", part: "steps" }),
    },
  ],
  verification: [
    {
      address: (seed) => issuePath(seed, "/verification"),
      opens: () => ({ kind: "view", preset: "verification" }),
    },
  ],
};

/**
 * The addresses of the views that parts other than this one build (R6): Dashboard's, which opens
 * the bare address too (dispatch://INFRA-5702), Threads' (dispatch://INFRA-5704), Graph's (part
 * T) and a handoff's (dispatch://INFRA-4870). Each has no route kind of `IssueRoute` on this
 * branch, so none is a case of `OLD_PAGE_ADDRESSES`; each is marked with the part that builds it,
 * and the row reads it only where the page draws that part (`drawsPart`). Where it does not, the
 * row names the address as not read, and the pull request names it. While dispatch://INFRA-5704
 * follows this part, `/threads`, `/threads/<id>` and each address root R20 lands in Threads are
 * left out here (R11). When a part lands, its kind joins `OLD_PAGE_ADDRESSES` with its cases.
 */
export const PART_ADDRESSES: readonly AddressCase[] = [
  {
    address: (seed) => issuePath(seed),
    builtBy: "INFRA-5702",
    opens: () => ({ kind: "control", name: "Dashboard" }),
  },
  {
    address: (seed) => issuePath(seed, "/dashboard"),
    builtBy: "INFRA-5702",
    opens: () => ({ kind: "control", name: "Dashboard" }),
  },
  {
    address: (seed) => issuePath(seed, "/threads"),
    builtBy: "INFRA-5704",
    opens: () => ({ kind: "control", name: "Threads" }),
  },
  {
    address: (seed) => issuePath(seed, `/threads/${seed.message.id}`),
    builtBy: "INFRA-5704",
    opens: () => ({ kind: "control", name: "Threads" }),
  },
  {
    address: (seed) => issuePath(seed, "/graph"),
    builtBy: "T",
    opens: () => ({ kind: "control", name: "Graph" }),
  },
  {
    address: (seed) => issuePath(seed, `/graph/${seed.planTask.node}`),
    builtBy: "T",
    opens: () => ({ kind: "control", name: "Graph" }),
  },
  {
    address: (seed) => issuePath(seed, `/handoffs/${seed.handoff}`),
    builtBy: "INFRA-4870",
    opens: () => ({ kind: "handoff" }),
  },
];

/**
 * Whether this branch's page draws the view `part` builds, which is what lets the row read that
 * part's addresses (`PART_ADDRESSES`): the part's control stands in the view row, or, for the
 * handoff's view, which has no control, the page parses its route.
 */
export function drawsPart(part: AddressPart): boolean {
  const controls: readonly string[] = PRESET_ORDER;
  switch (part) {
    case "INFRA-4870":
      return parseIssuePath("/issues/KEY-1/handoffs/handoff-1") !== undefined;
    case "INFRA-5702":
      return controls.includes("dashboard");
    case "INFRA-5704":
      return controls.includes("threads");
    case "T":
      return controls.includes("graph");
  }
}

// ---------------------------------------------------------------------------------------------
// INV-2: the states and widths of part N's R9.
// ---------------------------------------------------------------------------------------------

/** One state of the page R9 reads: a control of the view row on - Conversation, which is no view
 *  on, each of the four views, or Documents, whose list is drawn in place of the outline; a
 *  document open in place of the outline; or the Outline sheet open. */
export type PageState =
  | { readonly kind: "view"; readonly preset: Preset }
  | { readonly kind: "document" }
  | { readonly kind: "outline-sheet" };

/** One state of the page at one width of the viewport. */
export interface StateAtWidth {
  readonly state: PageState;
  readonly width: number;
}

/** Each control of the view row on, in the order the row draws them (`PRESET_ORDER`): no view,
 *  each of the four views, and Documents' list. Dashboard, Threads and Graph join through
 *  `PRESET_ORDER` once each is on `main`. */
const VIEWS: readonly PageState[] = PRESET_ORDER.map((preset) => ({ kind: "view", preset }));

/** Each of `states` at each of `widths`, the widths outermost: the page is resized once for every
 *  state read at that width. */
function atEachWidth(states: readonly PageState[], widths: readonly number[]): StateAtWidth[] {
  return widths.flatMap((width) => states.map((state) => ({ state, width })));
}

/** INV-2's states in which no text the page draws as its own holds an internal word (R9's first
 *  sentence, `c-documents` (4)): each control of the view row on, Documents' list among them,
 *  and a document open, at 1440 px and at 390 px. */
export const WORD_STATES: readonly StateAtWidth[] = atEachWidth(
  [...VIEWS, { kind: "document" }],
  [1440, 390]
);

/** INV-2's states in which no part of the page scrolls in two dimensions (R9's second sentence,
 *  `c-documents` (5)): each control of the view row on, Documents' list among them, a document
 *  open and the Outline sheet open, at R9's seven widths. */
export const REFLOW_STATES: readonly StateAtWidth[] = atEachWidth(
  [...VIEWS, { kind: "document" }, { kind: "outline-sheet" }],
  [320, 390, 768, 1024, 1280, 1440, 1920]
);

/** How a failure names `state`. */
export function stateName(state: PageState): string {
  switch (state.kind) {
    case "view":
      if (state.preset === "conversation") return "no view";
      if (state.preset === "documents") return `${PRESET_NAMES.documents}' list`;
      return `the ${PRESET_NAMES[state.preset]} view`;
    case "document":
      return "the spec open as a document";
    case "outline-sheet":
      return `the Outline sheet open over the ${PRESET_NAMES.spec} view`;
  }
}

/**
 * Loads issue `key` in `state`, as a reader reaches it, and waits until the page draws it: a
 * view at its address, with the rows it draws, and Documents' list at its address with its rows;
 * the issue's spec at its own address, `/issues/<key>/artifacts/<slug>`, which is where the
 * spec's row of Documents' list goes, with its editor's words; and below 1024 px the Outline
 * sheet opened by its control with the Spec view on, where the sheet holds the view row's small
 * controls besides the outline's list (R4). From 1024 px the page draws no Outline control, and
 * with no view on the same list stands in the Contents pane beside the outline, which is drawn
 * open; under a view it draws no Contents pane.
 */
export async function openPageState(page: Page, key: string, state: PageState): Promise<void> {
  switch (state.kind) {
    case "view":
      await gotoIssue(page, presetPath(key, state.preset));
      if (state.preset === "documents") {
        // The list is drawn in place of the outline, so the page draws no tree to wait for. Its
        // rows come with the issue's read, all at once, so its first row says they are drawn.
        await expect(documentsList(page), `${stateName(state)} is not drawn`).toBeVisible();
        await expect(
          documentsList(page).getByRole("listitem").first(),
          `${stateName(state)} draws no row`
        ).toBeVisible();
        return;
      }
      await expectIssuePageDrawn(page);
      await expect
        .poll(async () => await nodeRows(page).count(), {
          message: `${stateName(state)} draws no row`,
        })
        .toBeGreaterThan(0);
      return;
    case "document":
      await gotoIssue(page, await specAddress(key));
      await expect(openedEditor(page), "the spec's address opens no editor").toBeVisible();
      await expect(openedEditor(page)).toContainText(/\S/u);
      return;
    case "outline-sheet": {
      // With no view on, the page says which of the two it draws the outline's list in.
      await gotoIssue(page, `/issues/${key}`);
      await expectIssuePageDrawn(page);
      await expect.poll(async () => await nodeRows(page).count()).toBeGreaterThan(0);
      const drawn = await settledState("the outline's list", [
        ["Outline control", outlineControl(page)],
        ["Contents pane", contentsPane(page)],
      ]);
      if (drawn === "Contents pane") return;
      // The Spec view is turned on from the view row, as a reader turns it on, and the sheet is
      // opened once the view draws its rows.
      await pickPreset(page, PRESET_NAMES.spec);
      await expectActivePreset(page, PRESET_NAMES.spec);
      await expect
        .poll(async () => await nodeRows(page).count(), {
          message: `the ${PRESET_NAMES.spec} view draws no row`,
        })
        .toBeGreaterThan(0);
      await outlineControl(page).click();
      await expect(outlineSheet(page), "the Outline control opens no sheet").toBeVisible();
      return;
    }
  }
}

/** The header's size as root R13 states it: the height from the top of the identity block to the
 *  top of the filter row, and the lines of text and control values drawn between the two, each as
 *  the words it holds, left to right. */
export interface HeaderSize {
  readonly height: number;
  readonly lines: readonly string[];
}

/** Root R13's limit on the header at `width` (R22 repeats it for the pull request's link): at
 *  1440 px 3 lines and 130 px, at 390 px 6 lines and 180 px, none at any other width. */
export function headerLimit(
  width: number
): { readonly height: number; readonly lines: number } | undefined {
  if (width === 1440) return { height: 130, lines: 3 };
  if (width === 390) return { height: 180, lines: 6 };
  return undefined;
}

/**
 * The header as it is drawn now, with the page at its top: the lines are those of the text nodes
 * and the controls' values that stand between the top of the identity block and the top of the
 * filter row, each text that wraps counted line by line, and a line is every piece whose top
 * lies above the lowest bottom of the pieces gathered into it. Text laid out unseen (hidden,
 * transparent, or a clipped box a pixel wide or high) is left out, as are the options of a
 * select, which stand in the list and are drawn as its value. Null where the page draws no
 * header or no filter row.
 */
export function headerSize(page: Page): Promise<HeaderSize | null> {
  return page.evaluate((selectors) => {
    window.scrollTo(0, 0);
    const header = document.querySelector(`${selectors.issuePage} ${selectors.identityBlock}`);
    const band = header?.querySelector(selectors.identityBand) ?? null;
    const row = document.querySelector(`${selectors.issuePage} ${selectors.filterRow}`);
    if (header === null || band === null || row === null) return null;
    const top = band.getBoundingClientRect().top;
    const bottom = row.getBoundingClientRect().top;
    const inBand = (box: DOMRect): boolean =>
      box.width > 0 && box.height > 0 && box.bottom > top && box.top < bottom;
    const drawn = (element: Element): boolean => {
      for (let at: Element | null = element; at !== null; at = at.parentElement) {
        const style = getComputedStyle(at);
        if (style.visibility !== "visible" || style.opacity === "0") return false;
        const box = at.getBoundingClientRect();
        if (style.overflow !== "visible" && (box.width <= 1 || box.height <= 1)) return false;
      }
      return true;
    };
    const pieces: { bottom: number; left: number; text: string; top: number }[] = [];
    const range = document.createRange();
    const walker = document.createTreeWalker(header, NodeFilter.SHOW_TEXT);
    for (let node = walker.nextNode(); node !== null; node = walker.nextNode()) {
      const content = node.textContent ?? "";
      const element = node.parentElement;
      if (content.trim() === "" || element === null || element.closest("select") !== null) continue;
      range.selectNodeContents(node);
      const boxes = [...range.getClientRects()].filter(inBand);
      if (boxes.length === 0 || !drawn(element)) continue;
      // A text that wraps is one box per line: each line is given the characters drawn in it.
      const words = boxes.length === 1 ? [content] : boxes.map(() => "");
      for (let at = 0; boxes.length > 1 && at < content.length; at += 1) {
        range.setStart(node, at);
        range.setEnd(node, at + 1);
        const glyph = range.getClientRects()[0];
        if (glyph === undefined) continue;
        const line = boxes.findIndex(
          (box) => glyph.top >= box.top - 1 && glyph.bottom <= box.bottom + 1
        );
        if (line >= 0) words[line] += content[at];
      }
      boxes.forEach((box, line) => {
        const text = (words[line] ?? "").replace(/\s+/g, " ").trim();
        pieces.push({ bottom: box.bottom, left: box.left, text, top: box.top });
      });
    }
    for (const control of header.querySelectorAll("select, input:not([type='hidden']), textarea")) {
      const box = control.getBoundingClientRect();
      if (!inBand(box) || !drawn(control)) continue;
      const size = Number.parseFloat(getComputedStyle(control).fontSize);
      const middle = (box.top + box.bottom) / 2;
      const text =
        control instanceof HTMLSelectElement
          ? (control.selectedOptions[0]?.textContent ?? "").trim()
          : (control as HTMLInputElement).value;
      pieces.push({ bottom: middle + size / 2, left: box.left, text, top: middle - size / 2 });
    }
    pieces.sort((a, b) => a.top - b.top);
    const lines: (typeof pieces)[] = [];
    let lineBottom = Number.NEGATIVE_INFINITY;
    for (const piece of pieces) {
      const line = lines.at(-1);
      if (line !== undefined && piece.top < lineBottom - 1) {
        line.push(piece);
        lineBottom = Math.max(lineBottom, piece.bottom);
      } else {
        lines.push([piece]);
        lineBottom = piece.bottom;
      }
    }
    return {
      height: bottom - top,
      lines: lines.map((line) =>
        line
          .sort((a, b) => a.left - b.left)
          .map((piece) => piece.text)
          .join(" ")
      ),
    };
  }, SELECTORS);
}
