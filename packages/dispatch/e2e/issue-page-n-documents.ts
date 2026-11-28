/*
 * The helpers of piece 7's rows of part N (dispatch://INFRA-6480, under dispatch://INFRA-3940),
 * `e2e/issue-page-n-documents.e2e.ts`: Documents' list and the document it opens, the version
 * control and the filter by words, the filter row as it is drawn, every address of today's page
 * (INV-1), the states and widths of R9 (INV-2) and the log of the focus's moves (decision 413).
 * Each is the helper `e2e/issue-page-n.ts` held, which imports from here the ones its own rows
 * still read. `expectActiveControl`, `ViewRowShape`, `viewRowShape`, `VIEWS_FOCUS` and
 * `rowSelector` are dispatch://INFRA-6478's too, under those names and with these bodies
 * (decision 428). Every other helper is imported from the part that brought it.
 */

import { expect, type Locator, type Page } from "@playwright/test";
import { presetPath } from "../web/src/features/issue-page/issue-address";
import {
  CONTROL_LABELS,
  DASHBOARD_WORDS,
  PRESET_NAMES,
  PRESET_ORDER,
  type Preset,
} from "../web/src/features/issue-page/page-words";
import { type IssueRoute, parseIssuePath } from "../web/src/features/refs/routes";
import { PAGE_COLORS } from "../web/src/theme/classes";
import { oklchToSrgb8 } from "../web/src/theme/contrast";
import { getIssue } from "./api";
import { nextFrames } from "./frames";
import {
  contentsPane,
  gotoIssue,
  issuePage,
  outlineControl,
  outlineSheet,
  presetChooser,
  presetToolbar,
} from "./issue-page";
import { expectIssuePageDrawn, nodeRows, tree } from "./issue-page-g";
import { expectActivePreset, pickPreset } from "./issue-page-i";
import { expectThreadsDrawn } from "./issue-page-threads";
import { expandedState, settledState } from "./settled";
import { type ApiRequests, expectLoadSettled } from "./traffic";

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

/** The selectors the readers below pass into the browser. Nothing outside this module and
 *  `e2e/issue-page-n.ts` spells one: a test reads the page through the locators and readers the
 *  two modules export. */
export const SELECTORS = {
  authorText: "[data-author-text]",
  /** Details' drawer by the mark R13 names it with (`margin-slot.tsx`'s `DETAILS_ATTRIBUTE`),
   *  whose every colour c-page-colours reads. */
  detailsDrawer: "aside[data-details]",
  /** One crumb of the line a zoomed outline draws above its rows; none while it is not zoomed. */
  crumb: "[data-crumb]",
  /** Dashboard, drawn in place of the outline at the issue's own address and at `/dashboard`
   *  (`slots/view-dashboard.tsx`, dispatch://INFRA-5702). */
  dashboard: "[data-dashboard]",
  /** Documents' list, drawn in place of the outline and of every view (`DocumentsList.tsx`). */
  documentsList: "[data-documents-list]",
  /** The whole filter row: the five controls or the one control, Outline, and the small
   *  controls at its right end (`IssuePresets.tsx`). */
  filterRow: "[data-filter-row]",
  /** The part of the identity block that stays at the top of the screen from 761 px up: the
   *  key's row and the title (`IssueHeader.tsx`). */
  identityBand: "[data-identity-block]",
  identityBlock: "header[data-issue-header]",
  issuePage: "[data-issue-page]",
  nodeRow: "[data-node-row]",
  /** The sheet the Outline control opens below 1024 px, drawn in the page's body and not inside
   *  its root (`contents-frame.tsx`), which holds the view row's three small controls at 760 px
   *  and below. */
  outlineSheet: '[data-testid="issue-outline-sheet"]',
  pageChrome: "[data-page-chrome]",
} as const;

export function identityBlock(page: Page): Locator {
  return page.locator(SELECTORS.identityBlock);
}

export function nodeMenuButton(page: Page): Locator {
  return issuePage(page).getByRole("button", { exact: true, name: CONTROL_LABELS.other_nodes });
}

/** Documents' list, the one the stage draws at `/documents` and `/artifacts`. */
export function documentsList(page: Page): Locator {
  return page.locator(SELECTORS.documentsList);
}

/** Every drawn row of Documents' list, in order (up to 20 per page). */
export function documentsRows(page: Page): Locator {
  return documentsList(page).getByRole("listitem");
}

/** The link in Documents' list that opens the document or file named `name`. */
export function documentLink(page: Page, name: string): Locator {
  return documentsList(page).getByRole("link", { exact: true, name });
}

/** The link that opens the first document of Documents' list, which `t a` puts the focus on:
 *  each row's other links are its download. */
export function firstDocumentLink(page: Page): Locator {
  return documentsList(page).locator("[data-document-open]").first();
}

/** The row in Documents' list for the document or file named `name`. */
export function documentRow(page: Page, name: string): Locator {
  return documentsRows(page).filter({
    has: page.getByRole("link", { exact: true, name }),
  });
}

/** Opens Documents' list from the view row, at any width, and waits until the list is drawn. */
export async function openDocuments(page: Page): Promise<void> {
  await pickPreset(page, PRESET_NAMES.documents);
  await expect(documentsList(page), "Documents' list did not open").toBeVisible();
}

/** Dashboard, the one the stage draws in place of the outline at the issue's own address and at
 *  `/dashboard` (dispatch://INFRA-5702). */
export function dashboardView(page: Page): Locator {
  return issuePage(page).locator(SELECTORS.dashboard);
}

/** Dashboard's list of what waits on the reader. */
export function dashboardWaiting(page: Page): Locator {
  return dashboardView(page).getByRole("region", {
    exact: true,
    name: DASHBOARD_WORDS.parts.waiting,
  });
}

/** Fails unless the page has Dashboard on and draws it with its list read, and no outline beside
 *  it, as `e2e/issue-page-dashboard.e2e.ts`' `expectDashboard` reads it: the list of what waits on
 *  the reader draws its count only once every read its items come from has answered
 *  (`WaitingList.tsx`), so the count says the list is drawn. */
export async function expectDashboardDrawn(page: Page, where: string): Promise<void> {
  await expectActiveControl(page, "dashboard");
  await expect(dashboardView(page), `${where} draws no Dashboard`).toBeVisible();
  await expect(
    dashboardWaiting(page).getByTestId("dashboard-waiting-count"),
    `${where} draws no count of what waits on the reader, so its list has not been read`
  ).toBeVisible();
  await expect(tree(page), `${where} draws the outline with Dashboard`).toHaveCount(0);
}

/** Stages `file` in Documents' list, optionally writes `summary` into its Summary box, and
 *  presses Upload to send it (part N's R2). The staged row is today's `ArtifactUploadRow`, which
 *  names its box "Summary (optional)". */
export async function uploadInDocuments(
  page: Page,
  file: Parameters<Locator["setInputFiles"]>[0],
  summary?: string
): Promise<void> {
  await uploadInput(page).first().setInputFiles(file);
  if (summary !== undefined) {
    await documentsList(page).getByRole("textbox", { name: "Summary (optional)" }).fill(summary);
  }
  await documentsList(page)
    .getByRole("button", { exact: true, name: CONTROL_LABELS.upload })
    .click();
}

/** The control under a list that draws its next 20 entries: "Show more (5 left)". The list
 *  names how many entries are left (root R40). */
export function showMoreControl(scope: Locator): Locator {
  return scope.getByRole("button", {
    name: new RegExp(
      `^${CONTROL_LABELS.show_more} \\(\\d+ ${CONTROL_LABELS.show_more_left}\\)$`,
      "u"
    ),
  });
}

/** Fails unless the view row's control `preset` is active, whichever shape the row draws at this
 *  width: the toolbar's pressed toggle from 1280 px up, or the one control below it. Unlike
 *  `issue-page-i.ts`'s `expectActivePreset`, this accepts a control that draws no count, such as
 *  Conversation or Documents (part N's R4). `timeout`, where given, replaces the project's expect
 *  timeout for each read, for a copy whose view row draws later than that
 *  (`INFRA_663_DRAW_TIMEOUT_MS`). */
export async function expectActiveControl(
  page: Page,
  preset: Preset,
  timeout?: number
): Promise<void> {
  const name = PRESET_NAMES[preset];
  const wait = timeout === undefined ? undefined : { timeout };
  const shape = await viewRowShape(page, timeout);
  if (shape === "toolbar") {
    await expect(
      presetToolbar(page).getByRole("button", { exact: true, name }),
      `${name} is not active in the view row`
    ).toHaveAttribute("aria-pressed", "true", wait);
    return;
  }
  await expect(
    presetChooser(page),
    `${name} is not active in the one control`
  ).toHaveAccessibleName(new RegExp(`^${name}(?: \\d+\\b|$)`, "u"), wait);
}

/** The two shapes the view row draws: the toolbar from 1280 px up, the one control below it. */
export type ViewRowShape = "toolbar" | "one control";

/** The shape the view row draws, read as `settledState` reads a state. `settledState` waits under
 *  the project's expect timeout and takes no other, so a `timeout` reads the same two states in
 *  the same poll under that timeout instead. */
export async function viewRowShape(page: Page, timeout: number | undefined): Promise<ViewRowShape> {
  // The toolbar is the row of toggles; the one control's own name can be a view's alone, as
  // Conversation's and Documents' are, so a button by that name does not tell the shapes apart.
  const states: readonly (readonly [ViewRowShape, Locator])[] = [
    ["toolbar", presetToolbar(page)],
    ["one control", presetChooser(page)],
  ];
  if (timeout === undefined) return await settledState("the view row's shape", states);
  const failure = "the view row's shape: none of toolbar, one control showed";
  let shown: ViewRowShape | undefined;
  await expect
    .poll(
      async () => {
        for (const [shape, locator] of states) {
          if (await locator.isVisible()) {
            shown = shape;
            return true;
          }
        }
        return false;
      },
      // Playwright's own web-first retry schedule, so a page that draws early is read early.
      { intervals: [20, 50, 100, 100, 500], message: failure, timeout }
    )
    .toBe(true);
  // A poll whose row has already ended passes without reading, and leaves no state behind.
  if (shown === undefined) throw new Error(failure);
  return shown;
}

/** INV-4 (part N's R14): the four views whose focused row is part F's focused node, so Details'
 *  "The row you are on" follows each move in their trees and returns to that node in the outline. */
export const VIEWS_FOCUS = ["spec", "verification", "plan", "decisions"] as const;

/** The hidden file input the upload opens, which is how a test hands it a file. */
function uploadInput(page: Page): Locator {
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

/** One colour an element draws: where, and the value the browser computed for it. */
interface DrawnColour {
  /** The element and the property, as a failure names them: `button "Keys" color`, and an element
   *  by its test id where it has one, `aside[data-testid="desktop-margin-shell"] "…" color`, or by
   *  the nearest one around it, `span "Draft" in [data-testid="issue-header"] color`. */
  readonly drawn: string;
  /** The computed value, as `getComputedStyle` gives it. */
  readonly value: string;
}

/** Every colour the first element each of `roots` selects draws, as the browser computed it, over
 *  that element and each of its visible elements: the text colour of each that draws words or a
 *  shape of its own (an `svg` fills with its `currentColor`), each background, and each border
 *  that has a width. An element that draws no word and no shape draws no text colour, though it
 *  computes one. An element inside one that `chosen` selects is left out, and so is each colour
 *  that is `rgba(0, 0, 0, 0)`, which `offPalette` leaves out as transparent. */
export function drawnColours(
  page: Page,
  roots: readonly string[],
  chosen: readonly string[] = []
): Promise<DrawnColour[]> {
  return page.evaluate(
    (asked) => {
      const found: { drawn: string; value: string }[] = [];
      for (const root of asked.roots.map((selector) => document.querySelector(selector))) {
        if (root === null) continue;
        for (const element of [root, ...root.querySelectorAll<Element>("*")]) {
          if (element.closest("[hidden]") !== null || !element.checkVisibility()) continue;
          if (asked.chosen.some((selector) => element.closest(selector) !== null)) continue;
          const style = getComputedStyle(element);
          const drawsWords = [...element.childNodes].some(
            (child) => child.nodeType === Node.TEXT_NODE && (child.textContent ?? "").trim() !== ""
          );
          const colours: [property: string, value: string][] = [];
          if (
            drawsWords ||
            element instanceof SVGSVGElement ||
            element instanceof HTMLInputElement
          ) {
            colours.push(["color", style.color]);
          }
          colours.push(["background-color", style.backgroundColor]);
          for (const side of ["top", "right", "bottom", "left"] as const) {
            if (style.getPropertyValue(`border-${side}-style`) === "none") continue;
            if (Number.parseFloat(style.getPropertyValue(`border-${side}-width`)) === 0) continue;
            colours.push([`border-${side}-color`, style.getPropertyValue(`border-${side}-color`)]);
          }
          const drawn = colours.filter(([, value]) => value !== "rgba(0, 0, 0, 0)");
          if (drawn.length === 0) continue;
          const name =
            element.getAttribute("aria-label") ??
            (element.textContent ?? "").replace(/\s+/g, " ").trim().slice(0, 40);
          const testId = element.getAttribute("data-testid");
          const around =
            element.parentElement?.closest("[data-testid]")?.getAttribute("data-testid") ?? null;
          const own = testId === null ? "" : `[data-testid="${testId}"]`;
          const near = testId !== null || around === null ? "" : ` in [data-testid="${around}"]`;
          const at = `${element.tagName.toLowerCase()}${own} "${name}"${near}`;
          for (const [property, value] of drawn) found.push({ drawn: `${at} ${property}`, value });
        }
      }
      return found;
    },
    { chosen, roots }
  );
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

/** Each of `colours` that is none of B's `PAGE_COLORS` in `scheme` and not transparent, named
 *  with where it is drawn. Each page colour is computed from its swatch as
 *  `e2e/theme.stub.e2e.ts` computes a swatch's 8-bit value (`oklchToSrgb8`); a colour drawn part
 *  transparent, or in a form `srgbOf` does not read, is none. */
export function offPalette(colours: readonly DrawnColour[], scheme: "dark" | "light"): string[] {
  const palette = new Set(
    Object.values(PAGE_COLORS).map((pair) => oklchToSrgb8(pair[scheme].oklch).join(", "))
  );
  return colours.flatMap(({ drawn, value }) => {
    const srgb = srgbOf(value);
    if (srgb?.[3] === 0) return [];
    if (srgb?.[3] === 1 && palette.has(srgb.slice(0, 3).join(", "))) return [];
    return [`${drawn} is ${value}`];
  });
}

/** Each colour the filter row draws that is none of B's `PAGE_COLORS` in `scheme` and not
 *  transparent (part N's R4: the row is drawn in none but those, the prototype's colours), named
 *  with where it is drawn (`offPalette`). */
export async function offThePalette(page: Page, scheme: "dark" | "light"): Promise<string[]> {
  return offPalette(await drawnColours(page, [SELECTORS.filterRow]), scheme);
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

/** The look of each button the filter row draws on the page itself, keyed by the words it shows
 *  with its `aria-hidden` parts left out: its text alone, where `filterRowReading` takes a
 *  button's `aria-label` or `aria-labelledby` before its text. */
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
const AUTHOR_SCROLLERS = ["pre", "table", "img"] as const;

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
 *  `assertWhole`. Both facts of each row are read in one pass over the page, but each row is
 *  answered as a locator by its index (`nodeRows(page).nth`), which is found again each time it is
 *  read: a row drawn after the pass moves every index after it. */
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
      // A `display: contents` parent draws no box of its own, so the holder is the nearest
      // ancestor that does: the stage's box that `scope` stands in.
      let holding = element.parentElement;
      while (holding !== null && getComputedStyle(holding).display === "contents") {
        holding = holding.parentElement;
      }
      const holder = holding?.getBoundingClientRect();
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
export type AddressPart = "INFRA-4870" | "INFRA-5704" | "T";

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
  // Dashboard's own address (dispatch://INFRA-5702), a route kind of `IssueRoute` since that part
  // landed under this branch; the bare address that opens Dashboard too stays in
  // `PART_ADDRESSES`, since its kind is the issue's own.
  dashboard: [
    {
      address: (seed) => issuePath(seed, "/dashboard"),
      opens: () => ({ kind: "control", name: PRESET_NAMES.dashboard }),
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
  // Threads' list and a record's own address (dispatch://INFRA-5704), a route kind of
  // `IssueRoute` since that part landed on `main`. Each is marked with the part that builds the
  // view, so the row reads it where the page draws Threads (`drawsPart`).
  threads: [
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
  ],
  verification: [
    {
      address: (seed) => issuePath(seed, "/verification"),
      opens: () => ({ kind: "view", preset: "verification" }),
    },
  ],
};

/**
 * The addresses of the views that parts other than this one build (R6): Dashboard's bare address
 * (dispatch://INFRA-5702), whose route kind is the issue's own, Graph's (part T) and a handoff's
 * (dispatch://INFRA-4870). None but Dashboard's has a route kind of `IssueRoute` on this branch,
 * so none is a case of `OLD_PAGE_ADDRESSES`, where Dashboard's own `/dashboard` is. Dashboard is
 * under this branch's head and its address is read; each other is marked with the part that
 * builds it, and the row reads it only where the page draws that part (`drawsPart`). Where it
 * does not, the row names the address as not read, and the pull request names it. When a part
 * lands, its kind joins `OLD_PAGE_ADDRESSES` with its cases, as Threads' (dispatch://INFRA-5704)
 * has.
 */
export const PART_ADDRESSES: readonly AddressCase[] = [
  {
    address: (seed) => issuePath(seed),
    opens: () => ({ kind: "control", name: PRESET_NAMES.dashboard }),
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
    case "INFRA-5704":
      return controls.includes("threads");
    case "T":
      return controls.includes("graph");
  }
}

// ---------------------------------------------------------------------------------------------
// INV-2: the states and widths of part N's R9.
// ---------------------------------------------------------------------------------------------

/** One state of the page R9 reads: a control of the view row on - Dashboard, drawn in place of
 *  the outline, Conversation, which is no view on, each of the four views, or Documents, whose
 *  list is drawn in place of the outline; a document open in place of the outline; or the Outline
 *  sheet open. */
export type PageState =
  | { readonly kind: "view"; readonly preset: Preset }
  | { readonly kind: "document" }
  | { readonly kind: "outline-sheet" };

/** One state of the page at one width of the viewport. */
export interface StateAtWidth {
  readonly state: PageState;
  readonly width: number;
}

/** Each control of the view row on, in the order the row draws them (`PRESET_ORDER`): Dashboard
 *  (dispatch://INFRA-5702, under this branch), no view, each of the four views, Threads
 *  (dispatch://INFRA-5704), which has joined through `PRESET_ORDER`, and Documents' list. Graph
 *  joins through `PRESET_ORDER` once it is on `main`. */
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
 *  open and the Outline sheet open, at R9's seven widths. From 1024 px the page draws no Outline
 *  control, so there the sheet's state is no view with the outline's list in the Contents pane
 *  (`openPageState`), and `stateName` names it so. */
export const REFLOW_STATES: readonly StateAtWidth[] = atEachWidth(
  [...VIEWS, { kind: "document" }, { kind: "outline-sheet" }],
  [320, 390, 768, 1024, 1280, 1440, 1920]
);

/** How a failure names `state`, at `width` px where the state's page depends on the width. */
export function stateName(state: PageState, width?: number): string {
  switch (state.kind) {
    case "view":
      if (state.preset === "conversation") return "no view";
      if (state.preset === "documents") return `${PRESET_NAMES.documents}' list`;
      return `the ${PRESET_NAMES[state.preset]} view`;
    case "document":
      return "the spec open as a document";
    case "outline-sheet":
      return width !== undefined && width >= 1024
        ? "no view, with the Contents pane in place of the Outline sheet"
        : `the Outline sheet open over the ${PRESET_NAMES.spec} view`;
  }
}

/**
 * Loads issue `key` in `state`, as a reader reaches it, and waits until the page draws it: a
 * view at its address, with the rows it draws, Dashboard at its address with its list read,
 * Threads at its address with its own view, and Documents' list at its address with its rows;
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
      if (state.preset === "dashboard") {
        // Dashboard is drawn in place of the outline, so the page draws no tree and no row to wait
        // for: it is drawn once its list of what waits on the reader has been read.
        await expectDashboardDrawn(page, stateName(state));
        return;
      }
      if (state.preset === "threads") {
        // Threads is drawn in place of the outline too, so the page draws no tree and no row to
        // wait for: it is drawn once its own view is.
        await expectThreadsDrawn(page, stateName(state));
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
      // With no view on, the page says which of the two it draws the outline's list in. That is
      // Conversation's own address: the issue's own draws Dashboard (dispatch://INFRA-5702).
      await gotoIssue(page, presetPath(key, "conversation"));
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

// ---------------------------------------------------------------------------------------------
// Decision 413: an address moves the focus once (part N's R6, `c-addresses` (6)).
// ---------------------------------------------------------------------------------------------

/** The page's own name for the list the focus log is kept in, so no row reads a global by its
 *  spelling. */
const FOCUS_LOG = "__issuePageFocusMoves";

/**
 * Starts a log of every element the focus moves to, in each document `page` loads from now on,
 * from the first moment the document exists: an init script runs before the page's own, so a move
 * made while the page loads is in the log as a later one is. It names each element by the row of
 * the outline or of a view that holds it, or else by its tag and its words. Call it before the load
 * it is to log.
 */
export async function logFocusMoves(page: Page): Promise<void> {
  await page.addInitScript((name) => {
    const moves: string[] = [];
    Object.defineProperty(window, name, { value: moves });
    document.addEventListener(
      "focusin",
      (event) => {
        const element = event.target;
        if (!(element instanceof Element)) return;
        const row = element.closest<HTMLElement>("[data-node-row]");
        const view = element.closest(
          "[data-spec-body], [data-verification-body], [data-plan-body]"
        );
        if (row === null) {
          const words = (element.textContent ?? "").replace(/\s+/g, " ").trim().slice(0, 60);
          moves.push(`${element.tagName.toLowerCase()} "${words}"`);
          return;
        }
        const viewName = [...(view?.attributes ?? [])].find((one) => one.name.endsWith("-body"));
        const place = view === null ? "the outline's" : `the ${viewName?.name.slice(5, -5)} view's`;
        moves.push(`${place} row of ${row.dataset.nodeId ?? "no node"}`);
      },
      true
    );
  }, FOCUS_LOG);
}

/**
 * Fails unless, from the load of the address on, the focus moved once and onto `landed`, which
 * holds it, and fails on any earlier move by naming the element that took the focus before the
 * last. The log is read once the page's reads are quiet (`traffic`) and two frames after, so a take
 * that comes with a late read or a frame later is in it. `what` names the address.
 */
export async function expectFocusLandedOnce(
  page: Page,
  traffic: ApiRequests,
  landed: Locator,
  what: string
): Promise<void> {
  await expect(landed, `${what}: the focus did not land where the address names`).toBeFocused();
  await expectLoadSettled(traffic);
  await nextFrames(page);
  const moves = await page.evaluate(
    (name) => [...((window as unknown as Record<string, readonly string[]>)[name] ?? [])],
    FOCUS_LOG
  );
  const before = moves.slice(0, -1);
  expect(
    before,
    `${what}: the focus went to ${before.join(", then to ")} before it landed on ${moves.at(-1)}`
  ).toEqual([]);
  expect(moves, `${what}: the focus moved to nothing`).toHaveLength(1);
}

/** The row of node `id`, which is how a view and the outline both mark one. */
export function rowSelector(id: string): string {
  return `[data-node-row][data-node-id="${id}"]`;
}
