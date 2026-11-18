import AxeBuilder from "@axe-core/playwright";
import { labelsControlName } from "../web/src/features/issue/IssueLabels";
import { repoControlName } from "../web/src/features/issue/RepoControl";
import { DETAILS_ROW_REQUEST_EVENT } from "../web/src/features/issue-page/details-rows";
import {
  MARGIN_SECTION_ORDER,
  PROPERTY_LABELS,
} from "../web/src/features/issue-page/page-structure";
import {
  ANSWER_LINES,
  CONTROL_LABELS,
  PROGRESS_NOUNS,
} from "../web/src/features/issue-page/page-words";
import { buildDispatchReference, buildIssuePath } from "../web/src/features/refs/routes";
import { setLiveSessions } from "./agents";
import { createPriority, getIssue, patchIssue, putRepoProject } from "./api";
import { nextFrames } from "./frames";
import {
  closeDetails,
  filterRow,
  gotoIssue,
  issuePage,
  issueRead,
  marginShell,
  openDetails,
  openMenu,
  phase,
} from "./issue-page";
import {
  addControl,
  CHANGES_REASON,
  COMPONENT_TITLE,
  detailsRow,
  FOLD_AFTER,
  filesList,
  marginSection,
  marginSectionDisclosure,
  marginSlot,
  ONE_PAST_FOLD,
  OTHER_LINK,
  PINNED_FIRST_LINE,
  PLANNER,
  PLANNER_TITLE,
  PRIORITY_TITLE,
  PROJECT,
  PULL_NUMBER,
  pinsList,
  QUESTION,
  REPOSITORY,
  seedDetailsIssue,
  TWENTY_LABELS,
  UNPINNED_COMMENT,
} from "./issue-page-h";
import { expect, type Locator, type Page, type Request, test } from "./test";
import { assertWhole } from "./unclipped";

/**
 * Details' properties and pins: the rows of the three sections and their controls, the one Add
 * control, the key `l`, the reader's pins, and no comment, question or message of the issue.
 * Every row reads `seedDetailsIssue` as alice with the new page on (`phase`), and writes through
 * Dispatch.
 */

/** Development's rows, in the order the section is to draw them, as ONE holds them: ONE's spec
 *  holds a task list, so its last row is `PROPERTY_LABELS.tasks`. */
const ONE_DEVELOPMENT_ROWS = [
  PROPERTY_LABELS.status,
  PROPERTY_LABELS.priority,
  PROPERTY_LABELS.assignee,
  PROPERTY_LABELS.labels,
  PROPERTY_LABELS.advances,
  PROPERTY_LABELS.parent,
  PROPERTY_LABELS.blocked_by,
  PROPERTY_LABELS.blocks,
  PROPERTY_LABELS.repository,
  PROPERTY_LABELS.pull_request,
  PROPERTY_LABELS.external_links,
  PROPERTY_LABELS.claim,
  PROPERTY_LABELS.route,
  PROPERTY_LABELS.components,
  PROPERTY_LABELS.tasks,
];

/** The tags `e2e/accessibility.e2e.ts` holds the dashboard to. */
const AXE_TAGS = ["wcag2a", "wcag2aa", "wcag21a", "wcag21aa"];

/** The page's words the labels control is named from inside `NoTooltips`
 *  (`labelsControlName`). */
const CONTROL_NAME_WORDS = { ANSWER_LINES, CONTROL_LABELS };

/** The page's ink, the grey of a link's underline, as the prototype's `.lnk` draws them, and
 *  the quieter grey of a row's name. */
const INK = "rgb(28, 25, 23)";
const UNDERLINE = "rgb(168, 162, 158)";
const QUIET = "rgb(120, 113, 108)";

/** A viewport of `width`: the desktop height from 768 px up, a phone's below. */
function viewportAt(width: number): { height: number; width: number } {
  return { height: width >= 768 ? 900 : 844, width };
}

/** The page's reads of Dispatch from now on: every URL it asked for, decoded, and whether none
 *  is still out. Only `fetch` and `xhr` count, so the live socket never holds a read open. */
function watchRequests(page: Page): { readonly settled: () => boolean; readonly urls: string[] } {
  const urls: string[] = [];
  let out = 0;
  const counted = (request: Request) => ["fetch", "xhr"].includes(request.resourceType());
  const ended = (request: Request) => {
    if (counted(request)) out -= 1;
  };
  page.on("request", (request) => {
    if (!counted(request)) return;
    urls.push(decodeURIComponent(request.url()));
    out += 1;
  });
  page.on("requestfailed", ended);
  page.on("requestfinished", ended);
  return { settled: () => out === 0, urls };
}

/** Whether `locator`'s box lies inside the viewport sideways, so it is read with no sideways
 *  scroll. */
async function expectInsideSideways(page: Page, locator: Locator, what: string): Promise<void> {
  const box = await locator.boundingBox();
  const width = page.viewportSize()?.width ?? 0;
  expect(box, `${what} has no box`).not.toBeNull();
  expect(box?.x ?? -1, `${what} starts left of the viewport`).toBeGreaterThanOrEqual(0);
  expect(
    (box?.x ?? 0) + (box?.width ?? 0),
    `${what} ends right of the viewport`
  ).toBeLessThanOrEqual(width + 1);
}

/** Each row's glyph in Details, the column before the row's name (`IssueMargin.tsx`'s `Row`). */
const ROW_GLYPHS = '[data-margin-section] > div > div > div:first-child > span[aria-hidden="true"]';
/** Each document's and file's mark of its kind, in Details' list of them. */
const FILE_MARKS = '[data-testid="issue-files"] a > span[aria-hidden="true"]';
/** The status's own mark, in the pill of the Status row, Development's first row. */
const STATUS_MARK =
  '[data-margin-section="development"] > div > div:first-child > div:last-child > span > svg[data-status-glyph]';

/** A font Chromium says it drew a text in (`CSS.getPlatformFontsForNode`). */
interface DrawnFont {
  readonly familyName: string;
  readonly glyphCount: number;
  readonly isCustomFont: boolean;
  readonly postScriptName: string;
}

/** A glyph of Details: the row it is drawn in, what it holds, and the fonts its text is drawn
 *  in. An icon holds no text, and so no font. */
interface GlyphFonts {
  readonly fonts: readonly DrawnFont[];
  readonly icon: boolean;
  readonly row: string;
  readonly text: string;
}

/** What `drawnFonts` reads of a glyph in the page: the name of the row it is drawn in, followed
 *  for a file's mark by the file's name, and what it holds. */
function describeGlyph(this: Element): Omit<GlyphFonts, "fonts"> {
  const label =
    this.closest("[data-margin-section] > div > div")?.querySelector(
      ":scope > div:first-child > span[data-page-chrome]"
    )?.textContent ?? "";
  const file = this.closest("a")?.querySelector("[data-author-text]")?.textContent;
  return {
    icon: this.querySelector("svg") !== null,
    row: file === undefined || file === null ? label : `${label}, ${file}`,
    text: this.textContent ?? "",
  };
}

/** The glyphs of Details that `selectors` find, each with the fonts Chromium drew its text in, as
 *  Chromium reports them (`CSS.getPlatformFontsForNode`), once the page's typeface has loaded. */
async function drawnFonts(
  page: Page,
  selectors: readonly string[],
  describe: (this: Element) => Omit<GlyphFonts, "fonts"> | null = describeGlyph
): Promise<GlyphFonts[]> {
  await page.evaluate(() => document.fonts.ready.then(() => undefined));
  const cdp = await page.context().newCDPSession(page);
  try {
    await cdp.send("DOM.enable");
    await cdp.send("CSS.enable");
    const { root } = await cdp.send("DOM.getDocument", { depth: 0 });
    const glyphs: GlyphFonts[] = [];
    for (const selector of selectors) {
      // Inside Details' panel (`marginSlot`).
      const { nodeIds } = await cdp.send("DOM.querySelectorAll", {
        nodeId: root.nodeId,
        selector: `[data-testid="issue-margin"] ${selector}`,
      });
      for (const nodeId of nodeIds) {
        const { object } = await cdp.send("DOM.resolveNode", { nodeId });
        const { result } = await cdp.send("Runtime.callFunctionOn", {
          functionDeclaration: describe.toString(),
          objectId: object.objectId,
          returnByValue: true,
        });
        const described = result.value as Omit<GlyphFonts, "fonts"> | null;
        if (described === null) continue;
        const { fonts } = await cdp.send("CSS.getPlatformFontsForNode", { nodeId });
        glyphs.push({ ...described, fonts });
      }
    }
    return glyphs;
  } finally {
    await cdp.detach();
  }
}

/** What `drawnFonts` reads of a text of Details' own chrome (`data-page-chrome`) that holds a
 *  symbol outside ASCII, a control's mark such as a priority's Remove, or `null` for any other:
 *  one without such a symbol in its own text, or one inside a reader's own text
 *  (`data-author-text`), which is the author's and drawn in whatever face holds it. It names the
 *  row it stands in and the control's name, where it has one. */
function describeChromeSymbol(this: Element): Omit<GlyphFonts, "fonts"> | null {
  if (this.closest("[data-author-text]") !== null) return null;
  const own = [...this.childNodes]
    .filter((node) => node.nodeType === Node.TEXT_NODE)
    .map((node) => node.textContent ?? "")
    .join("");
  const symbol = [...own].some(
    (character) => (character.codePointAt(0) ?? 0) > 0x7f && /[\p{So}\p{Sm}]/u.test(character)
  );
  if (!symbol) return null;
  const label =
    this.closest("[data-margin-section] > div > div")?.querySelector(
      ":scope > div:first-child > span[data-page-chrome]"
    )?.textContent ?? "";
  const name = this.getAttribute("aria-label");
  return { icon: false, row: name === null ? label : `${label} (${name})`, text: own };
}

/** Whether a glyph's text is drawn in a font other than the page's own typeface, or in none: a
 *  face of the reader's machine draws it at that face's width, which no column holds on every
 *  machine. Chromium names a font by the names in its file, so the page's cut of Inter
 *  (`Dispatch Inter`, `styles.css`) is reported as a web font whose PostScript name is Inter's,
 *  `InterVariable`. */
function drawnOutsideThePagesTypeface({ fonts }: GlyphFonts): boolean {
  return (
    fonts.length === 0 ||
    fonts.some((font) => !font.isCustomFont || !font.postScriptName.startsWith("Inter"))
  );
}

/** A glyph's reading as a message says it: its row, its text with its code points, and each
 *  font Chromium drew it in. */
function glyphReading({ fonts, row, text }: GlyphFonts): string {
  const points = [...text]
    .map(
      (character) =>
        `U+${(character.codePointAt(0) ?? 0).toString(16).toUpperCase().padStart(4, "0")}`
    )
    .join(" ");
  const drawn =
    fonts.length === 0
      ? "no font"
      : fonts
          .map(
            (font) =>
              `${font.familyName} (${font.postScriptName}, ${
                font.isCustomFont ? "the page's" : "the machine's"
              }, ${font.glyphCount} glyphs)`
          )
          .join(", ");
  return `${row}: ${text} (${points}) in ${drawn}`;
}

/**
 * `assertWhole` on each of `locators` in Details, with the window made tall enough at its width
 * for every surface that scrolls between them and the document to hold its content whole, as
 * `approval.e2e.ts` holds a card: Details scrolls as it is meant to wherever it is taller than
 * the window, so what is left to flag is a clip. The window's own height is put back after.
 */
async function assertWholeInDetails(page: Page, locators: readonly Locator[]): Promise<void> {
  const viewport = page.viewportSize();
  if (viewport === null) throw new Error("the page has no viewport");
  let taller = 0;
  for (const locator of locators) {
    const hidden = await locator.evaluate((element) => {
      let most = 0;
      for (let node = element.parentElement; node !== null; node = node.parentElement) {
        if (getComputedStyle(node).overflowY !== "visible") {
          most = Math.max(most, node.scrollHeight - node.clientHeight);
        }
      }
      return most;
    });
    taller = Math.max(taller, hidden);
  }
  await page.setViewportSize({ height: viewport.height + taller + 16, width: viewport.width });
  try {
    for (const locator of locators) await assertWhole(locator);
  } finally {
    await page.setViewportSize(viewport);
  }
}

/** The first element with a box that Details draws under its head, the element that holds
 *  Details' Close: it is the section Development, or inside it, while no document is open. */
function firstUnderTheHead(page: Page): Promise<string> {
  return marginShell(page).evaluate((drawer) => {
    const close = drawer.querySelector('[data-testid="details-close"]');
    const head = [...drawer.children].find((child) => close !== null && child.contains(close));
    const development = drawer.querySelector('[data-margin-section="development"]');
    if (head === undefined || development === null) return "no head or no Development";
    const walker = document.createTreeWalker(drawer, NodeFilter.SHOW_ELEMENT);
    for (let node = walker.nextNode(); node !== null; node = walker.nextNode()) {
      if (!(node instanceof HTMLElement) || head.contains(node) || node === head) continue;
      if (
        !(head.compareDocumentPosition(node) & Node.DOCUMENT_POSITION_FOLLOWING) ||
        node.contains(development)
      ) {
        continue;
      }
      if (node.getBoundingClientRect().height === 0) continue;
      return development === node || development.contains(node)
        ? "development"
        : node.outerHTML.slice(0, 120);
    }
    return "nothing";
  });
}

test("Details opens on the properties and holds no conversation @phone", async ({ browser }) => {
  test.setTimeout(180_000);
  await phase(browser, seedDetailsIssue, async (page, seeded) => {
    const { one, three } = seeded.keys;
    const desktopProject = test.info().project.name !== "iphone";
    if (desktopProject) {
      // (1) Closed, Details reads neither the blockers' titles, nor what the issue blocks, nor
      // its links on GitHub.
      await page.setViewportSize(viewportAt(1440));
      const traffic = watchRequests(page);
      await gotoIssue(page, `/issues/${one}`);
      await expect(marginSlot(page)).toHaveCount(1);
      await expect
        .poll(() => traffic.settled(), { message: "the load's reads never ended" })
        .toBe(true);
      const urls = traffic.urls;
      const threeRef = `ref=${buildDispatchReference({ key: three, kind: "issue" })}`;
      const blocksRead = (url: string) =>
        url.includes("/api/v1/references?") && url.includes("kind=blocked_by");
      const titlesRead = (url: string) =>
        url.includes("/api/v1/reference-targets?") && url.includes(threeRef);
      // A link on GitHub is read through Dispatch's proxy (`web/src/api/client.ts`'s
      // `githubRest`), which reaches the fake GitHub the seed fills.
      const githubRead = (url: string) => new URL(url).pathname.startsWith("/api/github/rest/");
      expect(urls.filter(blocksRead), "closed Details read what ONE blocks").toEqual([]);
      expect(urls.filter(titlesRead), "closed Details read its blockers' titles").toEqual([]);
      expect(urls.filter(githubRead), "closed Details read ONE's links on GitHub").toEqual([]);
      await openDetails(page);
      await expect
        .poll(() => urls.some(blocksRead), { message: "Details read no Blocks" })
        .toBe(true);
      await expect
        .poll(() => urls.some(titlesRead), { message: "Details read no blocker's title" })
        .toBe(true);
      await expect
        .poll(() => urls.some(githubRead), {
          message: "Details read none of ONE's links on GitHub",
        })
        .toBe(true);

      // (2) Development first under the head, each section drawn once, through a close and an
      // open, and nothing of the conversation.
      expect(await firstUnderTheHead(page), "Details does not open on Development").toBe(
        "development"
      );
      for (let round = 0; round < 2; round += 1) {
        for (const section of MARGIN_SECTION_ORDER) {
          await expect(
            page.locator(`[data-margin-section="${section}"]`),
            `the page draws ${section} other than once`
          ).toHaveCount(1);
        }
        await closeDetails(page);
        await openDetails(page);
      }
      await expect(
        marginShell(page).getByRole("tab", { name: /^(Comments|Pinned)\b/ })
      ).toHaveCount(0);
      await expect(
        marginShell(page).getByText(UNPINNED_COMMENT, { exact: false }),
        "Details draws a comment of the issue"
      ).toHaveCount(0);
      await expect(
        marginShell(page).getByText(QUESTION, { exact: false }),
        "Details draws a question of the issue"
      ).toHaveCount(0);
      await expect(
        marginShell(page).locator("[title]"),
        "an element of Details carries a title"
      ).toHaveCount(0);
      const editLabels = detailsRow(page, PROPERTY_LABELS.labels).getByRole("button", {
        name: labelsControlName(one, {
          labels: TWENTY_LABELS,
          saving: false,
          words: CONTROL_NAME_WORDS,
        }),
      });
      await editLabels.click();
      await expect(page.getByRole("combobox", { name: "Search or create label" })).toBeVisible();
      // The popover is drawn over the page, outside the drawer (`AnchoredPanel.tsx`), with no
      // role of its own: it is read as the box that holds the labels' listbox.
      const labelsPopover = page
        .getByRole("listbox", { name: "Labels options" })
        .locator("xpath=..");
      await expect(labelsPopover.getByRole("option").first()).toBeVisible();
      await expect(
        marginShell(page).locator("[title]").or(labelsPopover.locator("[title]")),
        "an element of Details or of its labels' popover carries a title"
      ).toHaveCount(0);
      await page.keyboard.press("Escape");

      // (3) The two pins, each a link to its own address with its Unpin, the comment's by the
      // words of its first line and none of the marks its body writes them with.
      const commentPin = pinsList(page).getByRole("link", { exact: true, name: PINNED_FIRST_LINE });
      await expect(commentPin).toHaveAttribute(
        "href",
        buildIssuePath({ id: seeded.pinnedCommentId, key: one, kind: "comment" })
      );
      await expect(commentPin).toHaveText(PINNED_FIRST_LINE);
      await expect(commentPin, "the comment's pin draws its body's marks").not.toContainText("*");
      const historyPin = pinsList(page).getByRole("link", {
        exact: true,
        name: CONTROL_LABELS.history,
      });
      await expect(historyPin).toHaveAttribute(
        "href",
        buildIssuePath({ key: one, kind: "activity" })
      );
      await expect(pinsList(page).getByRole("button", { name: CONTROL_LABELS.unpin })).toHaveCount(
        2
      );
      await closeDetails(page);
    }

    // (4) On a phone: Escape in a box, the three sections inside Details' width, and a pin's
    // and a document's link that close it.
    await page.setViewportSize(viewportAt(390));
    await gotoIssue(page, `/issues/${one}`);
    await openDetails(page);
    // Escape in the blocked-by box leaves the box's editor and goes no further: the sheet stays.
    const blockedBy = detailsRow(page, PROPERTY_LABELS.blocked_by);
    await blockedBy.getByRole("button", { name: `Edit issues ${one} waits on` }).click();
    const blockersBox = blockedBy.getByLabel(`Issues ${one} waits on`, { exact: true });
    await expect(blockersBox).toBeFocused();
    await blockersBox.press("Escape");
    await nextFrames(page);
    await expect(marginShell(page), "Escape in the blocked-by box closed Details").toBeVisible();
    await expect(blockersBox, "Escape in the blocked-by box left its editor open").toHaveCount(0);
    for (const section of MARGIN_SECTION_ORDER) {
      await expect(marginSection(page, section)).toBeVisible();
    }
    expect(
      await marginShell(page).evaluate((drawer) => drawer.scrollWidth <= drawer.clientWidth),
      "Details scrolls sideways at 390 px"
    ).toBe(true);
    const phonePin = pinsList(page).getByRole("link", { exact: true, name: PINNED_FIRST_LINE });
    await assertWholeInDetails(page, [phonePin]);
    await phonePin.click();
    await expect(marginShell(page), "a pin's link left Details open on a phone").toBeHidden();
    await expect(page).toHaveURL(
      buildIssuePath({ id: seeded.pinnedCommentId, key: one, kind: "comment" })
    );
    // A document's link closes it too, as every link followed in Details does on a phone.
    await openDetails(page);
    const firstDocument = (await getIssue(one)).artifacts?.[0];
    expect(firstDocument, `${one} holds no document`).toBeDefined();
    const documentPath = buildIssuePath({
      key: one,
      kind: "artifact",
      slug: firstDocument?.slug ?? "",
    });
    const phoneDocument = filesList(page).locator(`a[href="${documentPath}"]`);
    await assertWholeInDetails(page, [phoneDocument]);
    await phoneDocument.click();
    await expect(marginShell(page), "a document's link left Details open on a phone").toBeHidden();
    await expect(page).toHaveURL(documentPath);
    await openDetails(page);
    for (let left = 2; left > 0; left -= 1) {
      const unpin = pinsList(page).getByRole("button", { name: CONTROL_LABELS.unpin });
      await expect(unpin).toHaveCount(left);
      await unpin.first().click();
    }
    await expect(pinsList(page)).toContainText(CONTROL_LABELS.none);
  });
});

test("every property with a value is visible in Details at every width @phone", async ({
  browser,
}) => {
  test.setTimeout(240_000);
  await phase(browser, seedDetailsIssue, async (page, seeded) => {
    const { one, parent, three } = seeded.keys;
    const desktopProject = test.info().project.name !== "iphone";
    // ONE as Dispatch reads it, which the readings below take who opened it from.
    const oneRead = await getIssue(one);
    const opener = oneRead.created_by?.id;
    expect(opener, `${one} names no one who opened it`).toBeDefined();

    // (5) Every width: the rows' order, the twenty labels whole, and each value in its row.
    for (const width of desktopProject ? [1440, 1280, 1024, 390] : [390]) {
      await page.setViewportSize(viewportAt(width));
      await gotoIssue(page, `/issues/${one}`);
      await openDetails(page);
      // The labels of Development's rows, in the order they stand (`IssueMargin.tsx`'s `Row`).
      await expect(
        marginSection(page, "development").locator(
          ":scope > div > div > div:first-child > span[data-page-chrome]"
        ),
        `Development's rows are out of order at ${width} px`
      ).toHaveText(ONE_DEVELOPMENT_ROWS);
      // Each row's glyph is drawn inside its own column, before the row's name (`Row`'s
      // `w-3.5`): the page's typeface drew Blocked by's ⊘ 19 px wide in that 14 px column, to
      // 2 px from the name, and a face of the machine drew Opened by's ☺ 15.3 px wide in it. An
      // icon is measured by its own box and a text glyph by its text, since the span around
      // either is the column itself, once the page's typeface has loaded, since that is what
      // draws ⊘. Soft, so that a width it fails at does not stop the widths after it.
      await page.evaluate(() => document.fonts.ready.then(() => undefined));
      const glyphs = await marginSlot(page)
        .locator(ROW_GLYPHS)
        .evaluateAll((spans) =>
          spans.map((span) => {
            const icon = span.querySelector("svg");
            const text = document.createRange();
            text.selectNodeContents(span);
            return {
              column: span.getBoundingClientRect().width,
              drawn: (icon ?? text).getBoundingClientRect().width,
              glyph: icon === null ? (span.textContent ?? "") : "icon",
              row: span.nextElementSibling?.textContent ?? "",
            };
          })
        );
      expect(
        glyphs.map(({ row }) => row),
        `Details measures no Blocked by glyph at ${width} px`
      ).toContain(PROPERTY_LABELS.blocked_by);
      expect
        .soft(
          glyphs
            .filter(({ column, drawn }) => drawn > column)
            .map(
              ({ column, drawn, glyph, row }) =>
                `${row}'s ${glyph}: ${drawn} px in a column of ${column} px`
            ),
          `a row's glyph is drawn wider than its column at ${width} px`
        )
        .toEqual([]);
      // Each glyph of Details that is text is drawn in the page's own typeface, as Chromium
      // reports the fonts it drew it in, whatever fonts the machine holds: a character that face
      // does not hold is drawn in one of the machine's faces, at that face's width, or in an
      // emoji face, and a `unicode-range` that takes it in does not say the face holds it (Inter
      // has no ⇄, which DejaVu Sans drew here). Each row's glyph that is text and each file's
      // mark that is. Soft, as above.
      const textGlyphs = (await drawnFonts(page, [ROW_GLYPHS, FILE_MARKS])).filter(
        ({ icon }) => !icon
      );
      expect(
        textGlyphs.map(({ row }) => row),
        `Details reads the fonts of no Blocked by glyph at ${width} px`
      ).toContain(PROPERTY_LABELS.blocked_by);
      expect
        .soft(
          textGlyphs.filter(drawnOutsideThePagesTypeface).map(glyphReading),
          `a glyph of Details is drawn in a font the page does not ship at ${width} px`
        )
        .toEqual([]);
      // So is every symbol in the text of Details' own chrome, whichever control draws it: a
      // priority's Remove, a section's toggle, a control's mark. A reader's own text is left out.
      const chromeSymbols = await drawnFonts(page, ["[data-page-chrome]"], describeChromeSymbol);
      expect
        .soft(
          chromeSymbols.filter(drawnOutsideThePagesTypeface).map(glyphReading),
          `a mark of Details' controls is drawn in a font the page does not ship at ${width} px`
        )
        .toEqual([]);
      // The status's own mark in the Status row's pill is a shape, as the page's other status
      // marks are (`StatusGlyph`), so no font of the machine draws it.
      await expect(
        marginSlot(page).locator(STATUS_MARK),
        `the Status row's pill draws its status mark other than as one shape at ${width} px`
      ).toHaveCount(1);
      // The readings, printed so that what each machine drew is on the record when the row
      // passes too.
      console.log(
        `Details' glyphs at ${width} px: ${glyphs
          .map(({ column, drawn, glyph, row }) => `${row} ${glyph} ${drawn} px in ${column} px`)
          .join("; ")}\nDetails' glyph fonts at ${width} px: ${[...textGlyphs, ...chromeSymbols]
          .map(glyphReading)
          .join("; ")}`
      );
      const labels = detailsRow(page, PROPERTY_LABELS.labels).locator("[data-author-text]");
      await expect
        .poll(async () =>
          (await labels.allTextContents()).filter((label) => TWENTY_LABELS.includes(label))
        )
        .toEqual(TWENTY_LABELS);
      await assertWholeInDetails(
        page,
        TWENTY_LABELS.map((label) =>
          detailsRow(page, PROPERTY_LABELS.labels).getByText(label, { exact: true })
        )
      );
      // The checklist of ONE's spec: one of its two items checked.
      await expect(
        detailsRow(page, PROPERTY_LABELS.tasks).getByTestId("issue-progress-tasks"),
        `the ${PROPERTY_LABELS.tasks} row does not count the spec's items at ${width} px`
      ).toHaveText(`1/2 ${PROGRESS_NOUNS.tasks}`);
      // The status, the priority and the assignee are the header's to change: their rows only
      // say them.
      for (const label of [
        PROPERTY_LABELS.status,
        PROPERTY_LABELS.priority,
        PROPERTY_LABELS.assignee,
      ]) {
        await expect(
          detailsRow(page, label).locator('button, select, input, [role="combobox"]'),
          `the ${label} row holds a control at ${width} px`
        ).toHaveCount(0);
      }
      // The rows a control elsewhere on the page opens Details at (`details-rows.ts`).
      for (const [label, row] of [
        [PROPERTY_LABELS.labels, "labels"],
        [PROPERTY_LABELS.parent, "parent"],
        [PROPERTY_LABELS.route, "route"],
      ] as const) {
        await expect(detailsRow(page, label)).toHaveAttribute("data-details-row", row);
      }
      const pullRequest = detailsRow(page, PROPERTY_LABELS.pull_request).getByRole("link", {
        name: new RegExp(`#${PULL_NUMBER}\\b`),
      });
      // The row draws the repository as text, which wraps where a box would cut a long name: a
      // button whose press opens the box, named by the repository it shows and what it sets.
      const repository = detailsRow(page, PROPERTY_LABELS.repository).getByRole("button", {
        exact: true,
        name: `${REPOSITORY}, ${repoControlName(one)}`,
      });
      const values: readonly (readonly [string, Locator])[] = [
        ["the repository", repository],
        ["the pull request", pullRequest],
        [
          "the other link",
          detailsRow(page, PROPERTY_LABELS.external_links).locator(`a[href="${OTHER_LINK}"]`),
        ],
        ["the component", detailsRow(page, PROPERTY_LABELS.components).getByText(COMPONENT_TITLE)],
        [
          "the claim's holder",
          detailsRow(page, PROPERTY_LABELS.claim).getByRole("link", {
            exact: true,
            name: PLANNER_TITLE,
          }),
        ],
        [
          "the release",
          detailsRow(page, PROPERTY_LABELS.claim).getByRole("button", {
            name: `Release the claim on ${one}`,
          }),
        ],
        [
          "the route's control",
          detailsRow(page, PROPERTY_LABELS.route).getByRole("button", {
            name: "Messages default to role:architect",
          }),
        ],
        [
          "the priority's link",
          detailsRow(page, PROPERTY_LABELS.advances).getByRole("link").first(),
        ],
        [
          "who opened it",
          detailsRow(page, PROPERTY_LABELS.opened_by).getByText(opener ?? "", { exact: true }),
        ],
        [
          // The chip of ONE's spec approval request, not the step alone: Details draws a Spec
          // step on every issue, which says Not requested where no request exists.
          "the spec's approval request",
          detailsRow(page, PROPERTY_LABELS.approval).locator(
            '[data-approval-step="spec"] [data-approval-artifact]'
          ),
        ],
        [
          "Referenced by's count",
          marginSection(page, "references").getByRole("button", {
            exact: true,
            name: `Referenced by (${oneRead.referenced_by_count})`,
          }),
        ],
        [
          "a document's link",
          marginSection(page, "references").getByRole("link", { name: /notes-01\.md/ }),
        ],
      ];
      for (const [what, at] of values) {
        await expect(at, `${what} is not drawn in Details at ${width} px`).toBeVisible();
        await expectInsideSideways(page, at, `${what} at ${width} px`);
      }
      await expect(
        repository,
        `the Repository row does not draw ${REPOSITORY} at ${width} px`
      ).toHaveText(REPOSITORY);
      // Enter on the text opens the box in its place, with the focus and the repository in it;
      // Escape draws the text again with the focus on it, so the next width starts from the
      // same row.
      await repository.focus();
      await page.keyboard.press("Enter");
      const repositoryBox = detailsRow(page, PROPERTY_LABELS.repository).getByRole("combobox", {
        exact: true,
        name: repoControlName(one),
      });
      await expect(
        repositoryBox,
        `Enter on the repository's text opened no box at ${width} px`
      ).toBeVisible();
      await expect(
        repositoryBox,
        `the repository's box did not take the focus at ${width} px`
      ).toBeFocused();
      await expect(
        repositoryBox,
        `the repository's box does not hold ${REPOSITORY} at ${width} px`
      ).toHaveValue(REPOSITORY);
      // The open box is the row's value: as wide as the row's value column, the second of the
      // row's two columns (`IssueMargin.tsx`'s `Row`), and wide enough to show the repository
      // whole. The box fills its list of suggestions from a read it starts when it takes the
      // focus, and Chromium draws the list's arrow inside the box once the list holds an option,
      // so both are measured once the list holds the repository, as the reader sees the box.
      // Both are soft, so that a width the box fails at does not stop the widths after it.
      await expect(
        detailsRow(page, PROPERTY_LABELS.repository).locator(
          `datalist > option[value="${REPOSITORY}"]`
        ),
        `the repository's box does not suggest ${REPOSITORY} at ${width} px`
      ).toHaveCount(1);
      const valueColumn = detailsRow(page, PROPERTY_LABELS.repository).locator(
        ":scope > div:last-child"
      );
      const [fieldBox, columnBox] = [
        await repositoryBox.boundingBox(),
        await valueColumn.boundingBox(),
      ];
      expect(fieldBox, `the repository's box has no box at ${width} px`).not.toBeNull();
      expect(columnBox, `the Repository row's value has no box at ${width} px`).not.toBeNull();
      const [fieldWidth, columnWidth] = [fieldBox?.width ?? 0, columnBox?.width ?? 0];
      const { clientWidth, scrollWidth } = await repositoryBox.evaluate((box) => ({
        clientWidth: box.clientWidth,
        scrollWidth: box.scrollWidth,
      }));
      expect
        .soft(
          Math.abs(fieldWidth - columnWidth),
          `the repository's box is ${fieldWidth} px wide in a value column of ${columnWidth} px at ${width} px`
        )
        .toBeLessThanOrEqual(1);
      expect
        .soft(
          scrollWidth,
          `the repository's box cuts ${REPOSITORY}: its text takes ${scrollWidth} px and the box shows ${clientWidth} px of it at ${width} px`
        )
        .toBeLessThanOrEqual(clientWidth);
      await page.keyboard.press("Escape");
      await expect(
        repository,
        `Escape in the repository's box did not give the focus back to its text at ${width} px`
      ).toBeFocused();
      await expect(detailsRow(page, PROPERTY_LABELS.blocked_by)).toContainText(three);
      await expect(detailsRow(page, PROPERTY_LABELS.blocked_by)).toContainText(seeded.titles.three);
      for (const label of [
        ...ONE_DEVELOPMENT_ROWS,
        PROPERTY_LABELS.opened_by,
        PROPERTY_LABELS.approval,
      ]) {
        await expect(
          detailsRow(page, label).getByText(label, { exact: true }),
          `the ${label} row draws its name other than once at ${width} px`
        ).toHaveCount(1);
      }
      if (width === 390) {
        // On a phone the claim's words and the pull request's link wrap, and nothing cuts them.
        await assertWholeInDetails(page, [
          detailsRow(page, PROPERTY_LABELS.claim).getByTestId("issue-claim"),
          pullRequest,
        ]);
      }
      // Each of ONE's eight properties the add control would list has a value.
      await expect(addControl(page), `ONE draws the add control at ${width} px`).toHaveCount(0);
      await closeDetails(page);
    }

    // (6) Blocks, and Files and documents, fold after ten behind Show all with the count, read on
    // a phone.
    await page.setViewportSize(viewportAt(390));
    await gotoIssue(page, `/issues/${one}`);
    await openDetails(page);
    const blocking = detailsRow(page, PROPERTY_LABELS.blocks).getByRole("link");
    await expect(blocking).toHaveCount(FOLD_AFTER);
    await detailsRow(page, PROPERTY_LABELS.blocks)
      .getByRole("button", { name: `${CONTROL_LABELS.show_all} ${ONE_PAST_FOLD}` })
      .click();
    await expect(blocking).toHaveCount(ONE_PAST_FOLD);
    expect(
      await blocking.evaluateAll((links) =>
        links
          .filter((link) => Number(getComputedStyle(link).fontWeight) < 600)
          .map((link) => link.textContent)
      ),
      "a key of Blocks is not bold"
    ).toEqual([]);
    // Each issue that waits on ONE, by its key as the way to it and its own title after it.
    for (const [index, key] of seeded.keys.blocks.entries()) {
      const link = detailsRow(page, PROPERTY_LABELS.blocks).getByRole("link", {
        exact: true,
        name: key,
      });
      await expect(link, `Blocks draws no ${key}`).toHaveCount(1);
      await expect(
        link.locator("xpath=following-sibling::span[1]"),
        `Blocks draws ${key} without its title`
      ).toHaveText(seeded.titles.blocks[index] ?? "");
    }
    // Show all is read as it is drawn, with the space before its count: below 1280 px every
    // button is a flex box (`styles.css`), which drops a space between two of its children.
    const showAllFiles = marginSection(page, "references").getByRole("button", {
      name: `${CONTROL_LABELS.show_all} ${ONE_PAST_FOLD}`,
    });
    await expect
      .poll(() => showAllFiles.evaluate((button) => (button as HTMLElement).innerText), {
        message: "Files and documents' Show all is drawn without the space before its count",
      })
      .toBe(`${CONTROL_LABELS.show_all} ${ONE_PAST_FOLD}`);
    const documents = filesList(page).getByRole("link");
    await expect(documents).toHaveCount(FOLD_AFTER);
    await showAllFiles.click();
    await expect(documents).toHaveCount(ONE_PAST_FOLD);
    const firstDocument = (await getIssue(one)).artifacts?.[0];
    expect(firstDocument, `${one} holds no document`).toBeDefined();
    await expect(documents.first()).toHaveAttribute(
      "href",
      buildIssuePath({ key: one, kind: "artifact", slug: firstDocument?.slug ?? "" })
    );
    // Referenced by's control opens the list of what references ONE.
    const referencedBy = marginSection(page, "references").getByRole("button", {
      name: /Referenced by/,
    });
    await expect(referencedBy).toHaveAttribute("aria-expanded", "false");
    await referencedBy.click();
    await expect(referencedBy).toHaveAttribute("aria-expanded", "true");
    const referencesPanel = page.locator(
      `[id="${await referencedBy.getAttribute("aria-controls")}"]`
    );
    await expect(referencesPanel.getByRole("listitem").first()).toBeVisible();

    if (!desktopProject) return;
    // (7) At 1440 px, the prototype's sizes and the page's own colours.
    await page.setViewportSize(viewportAt(1440));
    await gotoIssue(page, `/issues/${one}`);
    await openDetails(page);
    const row = await detailsRow(page, PROPERTY_LABELS.parent).evaluate((element) => {
      const style = getComputedStyle(element);
      const name = element.firstElementChild;
      return {
        borderBottomWidth: style.borderBottomWidth,
        columnGap: style.columnGap,
        fontSize: style.fontSize,
        lineHeight: style.lineHeight,
        name: Math.round(name?.getBoundingClientRect().width ?? 0),
        nameColour: name === null ? "" : getComputedStyle(name).color,
        paddingBottom: style.paddingBottom,
        paddingTop: style.paddingTop,
      };
    });
    expect(row).toEqual({
      borderBottomWidth: "1px",
      columnGap: "10px",
      fontSize: "14px",
      lineHeight: "20px",
      name: 112,
      nameColour: QUIET,
      paddingBottom: "8px",
      paddingTop: "8px",
    });
    const chip = await detailsRow(page, PROPERTY_LABELS.labels)
      .getByText(TWENTY_LABELS[0] ?? "", { exact: true })
      .evaluate((element) => {
        const style = getComputedStyle(element);
        return {
          border: [
            style.borderTopWidth,
            style.borderRightWidth,
            style.borderBottomWidth,
            style.borderLeftWidth,
          ],
          fontSize: style.fontSize,
          fontWeight: style.fontWeight,
          lineHeight: style.lineHeight,
          paddingLeft: style.paddingLeft,
          paddingRight: style.paddingRight,
          radius: style.borderTopLeftRadius,
        };
      });
    expect(chip).toEqual({
      border: ["1px", "1px", "1px", "1px"],
      fontSize: "12px",
      fontWeight: "500",
      lineHeight: "18px",
      paddingLeft: "7px",
      paddingRight: "7px",
      radius: "6px",
    });
    const links: readonly (readonly [string, Locator])[] = [
      [
        "the parent",
        detailsRow(page, PROPERTY_LABELS.parent).getByRole("link", { exact: true, name: parent }),
      ],
      [
        "a blocker's key",
        detailsRow(page, PROPERTY_LABELS.blocked_by).getByRole("link", {
          exact: true,
          name: three,
        }),
      ],
      ["a key of Blocks", blocking.first()],
      ["a pin", pinsList(page).getByRole("link").first()],
      ["Referenced by's control", referencedBy],
    ];
    for (const [what, link] of links) {
      const colours = await link.evaluate((element) => {
        const style = getComputedStyle(element);
        return {
          colour: style.color,
          offset: style.textUnderlineOffset,
          underline: style.textDecorationColor,
        };
      });
      expect(colours, `${what} is not the ink, underlined in the grey 3 px under it`).toEqual({
        colour: INK,
        offset: "3px",
        underline: UNDERLINE,
      });
    }
    // Who opened the issue and who holds its claim are drawn by `AgentName`, which draws a
    // session's link in the ink inside `NoTooltips` (its underline is `AgentName`'s own, which
    // this row does not read): alice opened ONE, a person, so her name is words; the planner's
    // claim is a session's link.
    for (const [what, name] of [
      [
        "the opener",
        detailsRow(page, PROPERTY_LABELS.opened_by).getByText("alice", { exact: true }),
      ],
      [
        "the claim's holder",
        detailsRow(page, PROPERTY_LABELS.claim).getByRole("link", {
          exact: true,
          name: PLANNER_TITLE,
        }),
      ],
    ] as const) {
      expect(
        await name.evaluate((element) => getComputedStyle(element).color),
        `${what} in Details is not the ink`
      ).toBe(INK);
    }
    expect(
      await documents.first().evaluate((element) => getComputedStyle(element).color),
      "a document's entry in Details is not the ink"
    ).toBe(INK);
    expect(
      await documents.evaluateAll((entries) =>
        entries.map((entry) => getComputedStyle(entry).textDecorationLine)
      ),
      "a document's entry in Details is underlined"
    ).toEqual(Array.from({ length: FOLD_AFTER }, () => "none"));
    const approve = detailsRow(page, PROPERTY_LABELS.approval).getByRole("button", {
      exact: true,
      name: "Approve",
    });
    expect(
      await approve.evaluate((element) => {
        const style = getComputedStyle(element);
        return { background: style.backgroundColor, colour: style.color };
      })
    ).toEqual({ background: INK, colour: "rgb(255, 255, 255)" });
    const axe = await new AxeBuilder({ page })
      .withTags(AXE_TAGS)
      .include("aside[data-details]")
      .analyze();
    expect(axe.violations.map((violation) => violation.id)).toEqual([]);
  });
});

/** A label of the most characters Dispatch takes (`IssueLabels.tsx`'s `maxLabelLength`, and the
 *  server's `LABELS_INPUT` refusal "each label must be 1 to 40 characters"), with no space to
 *  break it at. */
const LONGEST_LABEL = "a-label-of-forty-characters-and-no-space";

// Step (5) above, for the longest label: the Labels row lists every label, none cut. Found by the
// tester on the preview of d4344116: on an issue there a 40-character label ran 39 px past
// Details' edge at 1440 px and was cut, at 390 px as well.
test("a label of the most characters Dispatch takes is whole in Details at every width @phone", async ({
  browser,
}) => {
  test.setTimeout(180_000);
  await phase(browser, seedDetailsIssue, async (page, seeded) => {
    const { two } = seeded.keys;
    expect(LONGEST_LABEL).toHaveLength(40);
    await patchIssue(two, { labels: [LONGEST_LABEL] });
    const desktopProject = test.info().project.name !== "iphone";
    for (const width of desktopProject ? [1440, 1280, 1024, 390] : [390]) {
      await page.setViewportSize(viewportAt(width));
      await gotoIssue(page, `/issues/${two}`);
      await openDetails(page);
      const row = detailsRow(page, PROPERTY_LABELS.labels);
      const label = row.getByText(LONGEST_LABEL, { exact: true });
      await expect(label).toBeVisible();
      const [labelBox, rowBox] = [await label.boundingBox(), await row.boundingBox()];
      expect(labelBox, `the label has no box at ${width} px`).not.toBeNull();
      expect(rowBox, `the Labels row has no box at ${width} px`).not.toBeNull();
      expect(
        (labelBox?.x ?? 0) + (labelBox?.width ?? 0),
        `a label of 40 characters runs past the Labels row at ${width} px`
      ).toBeLessThanOrEqual((rowBox?.x ?? 0) + (rowBox?.width ?? 0) + 1);
      await assertWholeInDetails(page, [label]);
    }
  });
});

/** A repository whose name runs to 45 characters, which GitHub allows (a name of up to 100), with
 *  no space to break it at. A name of 32 characters, which a project of this deployment maps,
 *  already runs 14 px past the row at 1440 px. */
const LONG_REPOSITORY = "acme/a-repository-name-of-exactly-forty-chars";

// Step (5) above, for a long repository: the repository is visible in its row, whole. Found by
// the tester on the preview of 962ec632: on an issue there a repository of 44 characters ran
// 101 px past Details' Repository row at 1440 px and was cut at Details' edge, at 390 px as well.
test("a repository of a long name is whole in Details at every width @phone", async ({
  browser,
}) => {
  test.setTimeout(180_000);
  await phase(browser, seedDetailsIssue, async (page, seeded) => {
    const { two } = seeded.keys;
    await putRepoProject(LONG_REPOSITORY, { default: false, project: PROJECT });
    await patchIssue(two, { repo: LONG_REPOSITORY });
    const desktopProject = test.info().project.name !== "iphone";
    for (const width of desktopProject ? [1440, 1280, 1024, 390] : [390]) {
      await page.setViewportSize(viewportAt(width));
      await gotoIssue(page, `/issues/${two}`);
      await openDetails(page);
      const row = detailsRow(page, PROPERTY_LABELS.repository);
      const repository = row.getByText(LONG_REPOSITORY, { exact: true }).first();
      await expect(repository).toBeVisible();
      const [repositoryBox, rowBox] = [await repository.boundingBox(), await row.boundingBox()];
      expect(repositoryBox, `the repository has no box at ${width} px`).not.toBeNull();
      expect(rowBox, `the Repository row has no box at ${width} px`).not.toBeNull();
      expect(
        (repositoryBox?.x ?? 0) + (repositoryBox?.width ?? 0),
        `a repository of ${LONG_REPOSITORY.length} characters runs past the Repository row at ${width} px`
      ).toBeLessThanOrEqual((rowBox?.x ?? 0) + (rowBox?.width ?? 0) + 1);
      await assertWholeInDetails(page, [repository]);
    }
  });
});

/** The bottom of Details' head, the element that holds its Close, which stays at the top while
 *  the panel under it scrolls. */
function detailsHeadBottom(page: Page): Promise<number> {
  return marginShell(page).evaluate((drawer) => {
    const close = drawer.querySelector('[data-testid="details-close"]');
    const head = [...drawer.children].find((child) => close !== null && child.contains(close));
    return head?.getBoundingClientRect().bottom ?? Number.NaN;
  });
}

// Add opens the control of the property it lists. In a short window the Labels row has
// scrolled up under Details' head by the time Add, at the end of Development, is in view; Add
// brings the control into view before it presses it, since the control's popover is placed from
// where the control is on the screen. Found by the implementer's c-use at cf4dd251 (the popover
// opened over the head at 390 px), fixed in 29c83c9b; the tester pins it here.
test("Add brings a control it reaches into view, so its popover opens under Details' head @phone", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  await phase(browser, seedDetailsIssue, async (page, seeded) => {
    const { two } = seeded.keys;
    await page.setViewportSize({ height: 400, width: 390 });
    await gotoIssue(page, `/issues/${two}`);
    await openDetails(page);
    const labelsControl = detailsRow(page, PROPERTY_LABELS.labels).getByRole("button", {
      name: labelsControlName(two, { labels: [], saving: false, words: CONTROL_NAME_WORDS }),
    });
    await expect(labelsControl).toHaveCount(1);
    await addControl(page).evaluate((control) => control.scrollIntoView({ block: "end" }));
    await nextFrames(page);
    const headBottom = await detailsHeadBottom(page);
    const before = await labelsControl.boundingBox();
    expect(
      (before?.y ?? 0) + (before?.height ?? 0),
      "the Labels control is in view below the head before Add reaches it, so the row proves nothing"
    ).toBeLessThanOrEqual(headBottom);
    await addControl(page).click();
    await openMenu(page)
      .getByRole("menuitem", { exact: true, name: PROPERTY_LABELS.labels })
      .click();
    const popover = page.getByRole("listbox", { name: "Labels options" }).locator("xpath=..");
    await expect(popover).toBeVisible();
    const [control, opened] = [await labelsControl.boundingBox(), await popover.boundingBox()];
    expect(
      control?.y ?? -1,
      "Add pressed the Labels control under Details' head"
    ).toBeGreaterThanOrEqual(headBottom - 1);
    expect(opened?.y ?? -1, "the labels' popover opened over Details' head").toBeGreaterThanOrEqual(
      headBottom - 1
    );
    await page.keyboard.press("Escape");
  });
});

test("each control in Details saves and reads back", async ({ browser }) => {
  test.setTimeout(300_000);
  await phase(browser, seedDetailsIssue, async (page, seeded) => {
    const { one, three, two } = seeded.keys;
    const reopen = async (key: string) => {
      await gotoIssue(page, `/issues/${key}`);
      await openDetails(page);
    };

    // (8) The labels: one removed and one added, then `l` from a closed Details.
    await reopen(one);
    const editLabels = detailsRow(page, PROPERTY_LABELS.labels).getByRole("button", {
      name: labelsControlName(one, {
        labels: TWENTY_LABELS,
        saving: false,
        words: CONTROL_NAME_WORDS,
      }),
    });
    await expect(editLabels, "Details draws no Edit labels").toBeVisible();
    await editLabels.click();
    await page.getByRole("option", { exact: true, name: "label-20" }).click();
    const search = page.getByRole("combobox", { name: "Search or create label" });
    await search.fill("label-new");
    await expect(page.getByRole("option", { name: 'Create "label-new"' })).toBeVisible();
    await search.press("Enter");
    await editLabels.click();
    const relabelled = [...TWENTY_LABELS.slice(0, 19), "label-new"];
    await expect
      .poll(async () => [...((await getIssue(one)).labels ?? [])].sort())
      .toEqual([...relabelled].sort());
    await reopen(one);
    await expect
      .poll(async () =>
        (
          await detailsRow(page, PROPERTY_LABELS.labels)
            .locator("[data-author-text]")
            .allTextContents()
        ).filter((label) => label.startsWith("label-"))
      )
      .toEqual(relabelled);
    await closeDetails(page);
    await page.keyboard.press("l");
    // The labels' control is in the DOM while Details is closed, so whether `l` pressed it is
    // read first, and only then whether Details is open on it.
    await expect(
      detailsRow(page, PROPERTY_LABELS.labels).getByRole("button", {
        name: labelsControlName(one, {
          labels: relabelled,
          saving: false,
          words: CONTROL_NAME_WORDS,
        }),
      }),
      "l did not open Details at Edit labels"
    ).toHaveAttribute("aria-expanded", "true");
    await expect(marginShell(page), "l did not open Details").toBeVisible();
    await page.keyboard.press("Escape");
    // The list of keys Keys opens names `l` by what it does.
    await closeDetails(page);
    await filterRow(page).getByRole("button", { exact: true, name: CONTROL_LABELS.keys }).click();
    const keyList = page.getByRole("dialog", { name: "Keyboard shortcuts" });
    await expect(keyList).toBeVisible();
    await expect(
      keyList.getByRole("listitem").filter({ hasText: "Edit labels" }).locator("kbd"),
      "the list of keys does not name l Edit labels"
    ).toHaveText(["l"]);
    await page.keyboard.press("Escape");
    await expect(keyList).toBeHidden();

    // (9) TWO's parent, from Add, then the priority it inherits.
    await reopen(two);
    await expect(detailsRow(page, PROPERTY_LABELS.parent)).toContainText(CONTROL_LABELS.none);
    await expect(detailsRow(page, PROPERTY_LABELS.parent).getByRole("button").first()).toHaveText(
      CONTROL_LABELS.set_parent
    );
    // TWO resolves to no repository, so its Repository row reads None, as an empty row does:
    // a button whose press draws the repository's box.
    await expect(
      detailsRow(page, PROPERTY_LABELS.repository).getByRole("button", {
        exact: true,
        name: `${CONTROL_LABELS.none}, ${repoControlName(two)}`,
      }),
      "TWO resolves to no repository, but its Repository row does not read None"
    ).toHaveText(CONTROL_LABELS.none);
    await addControl(page).click();
    // Add lists Assignee only where the header draws the assignee's control, which the menu
    // presses, and only while TWO has no assignee: an issue someone is assigned to has a value for
    // it, and Add lists no property that has one. The header is read once it is drawn.
    const twoStored = await getIssue(two);
    await expect(
      issuePage(page).getByRole("heading", { level: 1, name: twoStored.title })
    ).toBeVisible();
    // One read is the header's answer here: the assignee's control waits on no read, so a header
    // that draws it draws it in the commit that drew the heading awaited above.
    const assigneeControls = await page.locator(`[aria-label="Assignee of ${two}"]`).count();
    const listsAssignee = assigneeControls > 0 && twoStored.assignee === null;
    await expect(
      openMenu(page).getByRole("menuitem"),
      "the Add menu lists other than TWO's empty fields, in `IssueAddMenu`'s order"
    ).toHaveText([
      PROPERTY_LABELS.parent,
      ...(listsAssignee ? [PROPERTY_LABELS.assignee] : []),
      PROPERTY_LABELS.labels,
      PROPERTY_LABELS.route,
      PROPERTY_LABELS.blocked_by,
      PROPERTY_LABELS.repository,
      PROPERTY_LABELS.components,
      PROPERTY_LABELS.advances,
    ]);
    await openMenu(page)
      .getByRole("menuitem", { exact: true, name: PROPERTY_LABELS.parent })
      .click();
    // The editor and what it holds outlive a close and an open of Details.
    const parentBox = detailsRow(page, PROPERTY_LABELS.parent).getByLabel(PROPERTY_LABELS.parent, {
      exact: true,
    });
    await parentBox.fill(one);
    await closeDetails(page);
    await openDetails(page);
    await expect(parentBox, "closing Details threw the parent's draft away").toHaveValue(one);
    await detailsRow(page, PROPERTY_LABELS.parent)
      .getByRole("button", { name: CONTROL_LABELS.save_parent })
      .click();
    await expect.poll(async () => (await getIssue(two)).parent).toBe(one);
    await reopen(two);
    const parentRow = detailsRow(page, PROPERTY_LABELS.parent);
    await expect(parentRow.getByRole("button").first()).toHaveText(CONTROL_LABELS.edit_parent);
    await expect(parentRow.getByRole("link", { exact: true, name: one })).toBeVisible();
    const advancesRow = detailsRow(page, PROPERTY_LABELS.advances);
    await expect(advancesRow).toContainText(PRIORITY_TITLE);
    await expect(advancesRow).toContainText(CONTROL_LABELS.advances_inherited);
    await expect(
      advancesRow.getByRole("button", { name: /^Remove / }),
      "TWO can remove the priority it inherits from ONE"
    ).toHaveCount(0);
    // An editor open on TWO is not carried to the issue a link moves the page to.
    const routeRow = detailsRow(page, PROPERTY_LABELS.route);
    const routeBox = routeRow.getByLabel(PROPERTY_LABELS.route, { exact: true });
    await routeRow.getByRole("button", { name: "Messages default to no route" }).click();
    await expect(routeBox).toBeVisible();
    await page.evaluate(() => {
      (window as Window & { detailsStayed?: boolean }).detailsStayed = true;
    });
    await parentRow.getByRole("link", { exact: true, name: one }).click();
    await expect(page).toHaveURL(buildIssuePath({ key: one, kind: "issue" }));
    await issueRead(page);
    expect(
      await page.evaluate(() => (window as Window & { detailsStayed?: boolean }).detailsStayed),
      "the parent's link loaded the page rather than moving it"
    ).toBe(true);
    await openDetails(page);
    await expect(
      routeRow.getByRole("button", { name: "Messages default to role:architect" })
    ).toBeVisible();
    await expect(routeBox, `${one} opened with TWO's route editor`).toHaveCount(0);
    await reopen(two);

    // (10) TWO's route, and whether it reaches anyone.
    const noRoute = routeRow.getByRole("button", { name: "Messages default to no route" });
    await expect(noRoute).toHaveText(CONTROL_LABELS.none);
    await noRoute.click();
    await routeBox.fill("role:implementer");
    await routeRow.getByRole("button", { name: CONTROL_LABELS.save_route }).click();
    await expect.poll(async () => (await getIssue(two)).route).toBe("role:implementer");
    await reopen(two);
    await expect(
      routeRow.getByRole("button", {
        name: "Messages default to role:implementer",
      })
    ).toBeVisible();
    expect((await getIssue(two)).route_status, "a live session holds role:implementer").toBe(
      "no_holder"
    );
    await expect(page.getByTestId("issue-details-route-unreachable")).toHaveCount(1);
    // And a route that reaches someone draws no such marker: a live session takes the role.
    await setLiveSessions([{ roles: ["implementer"], session_id: PLANNER, title: PLANNER_TITLE }]);
    await expect
      .poll(async () => (await getIssue(two)).route_status, {
        message: "role:implementer does not reach the live session that holds it",
      })
      .toBe("live");
    await reopen(two);
    await expect(
      routeRow.getByRole("button", { name: "Messages default to role:implementer" })
    ).toBeVisible();
    await expect(
      page.getByTestId("issue-details-route-unreachable"),
      "Details says a route that reaches a live session reaches nobody"
    ).toHaveCount(0);

    // (11) TWO's blockers: a refused key keeps the editor open, then THREE.
    const blockedBy = detailsRow(page, PROPERTY_LABELS.blocked_by);
    await blockedBy.getByRole("button", { name: `Add issues ${two} waits on` }).click();
    const keys = blockedBy.getByLabel(`Issues ${two} waits on`, { exact: true });
    await keys.fill(`${PROJECT}-9999`);
    await blockedBy.getByRole("button", { name: CONTROL_LABELS.save_blocked_by }).click();
    await expect(blockedBy.getByRole("alert")).toHaveText(`There is no issue ${PROJECT}-9999.`);
    await expect(keys, "the refused list closed the editor").toBeVisible();
    await keys.fill(three);
    await blockedBy.getByRole("button", { name: CONTROL_LABELS.save_blocked_by }).click();
    await expect.poll(async () => (await getIssue(two)).blocked_by).toEqual([three]);
    await reopen(two);
    const blocker = detailsRow(page, PROPERTY_LABELS.blocked_by).getByRole("link", {
      exact: true,
      name: three,
    });
    await expect(blocker).toBeVisible();
    expect(
      Number(await blocker.evaluate((element) => getComputedStyle(element).fontWeight))
    ).toBeGreaterThanOrEqual(600);
    await expect(detailsRow(page, PROPERTY_LABELS.blocked_by)).toContainText(seeded.titles.three);

    // (12) THREE's priority, added and removed.
    await reopen(three);
    await expect(detailsRow(page, PROPERTY_LABELS.advances)).toHaveCount(0);
    await marginSlot(page)
      .getByRole("button", { name: `${CONTROL_LABELS.add_priority} to ${three}` })
      .click();
    await marginSlot(page)
      .getByRole("combobox", { name: CONTROL_LABELS.priority_to_advance })
      .selectOption(seeded.priorityId);
    await marginSlot(page)
      .getByRole("button", { exact: true, name: CONTROL_LABELS.add })
      .last()
      .click();
    await expect.poll(async () => (await getIssue(three)).advances?.length ?? 0).toBe(1);
    await reopen(three);
    const added = detailsRow(page, PROPERTY_LABELS.advances).getByRole("link");
    await expect(added).toHaveCount(1);
    await added.click();
    await expect(page).toHaveURL(`/priorities/${seeded.priorityId}`);
    await reopen(three);
    await detailsRow(page, PROPERTY_LABELS.advances)
      .getByRole("button", { name: /^Remove / })
      .click();
    await expect.poll(async () => (await getIssue(three)).advances?.length ?? 0).toBe(0);
    await reopen(three);
    await expect(detailsRow(page, PROPERTY_LABELS.advances)).toHaveCount(0);

    // (13) Approval: its two controls where no request waits on alice, and bob's reason.
    await expect(detailsRow(page, PROPERTY_LABELS.approval)).toContainText(CHANGES_REASON);
    await reopen(one);
    const approval = detailsRow(page, PROPERTY_LABELS.approval);
    await expect(approval.getByRole("button", { exact: true, name: "Approve" })).toBeVisible();
    await approval.getByRole("button", { exact: true, name: "Request changes" }).click();
    await expect(page.getByRole("dialog")).toBeVisible();
    await page.keyboard.press("Escape");
    await expect(page.getByRole("dialog")).toHaveCount(0);
    await reopen(two);
    await expect(
      detailsRow(page, PROPERTY_LABELS.approval).getByRole("button", {
        name: /^(Approve|Request changes)$/,
      })
    ).toHaveCount(0);

    // (14) The claim, released.
    await reopen(one);
    await detailsRow(page, PROPERTY_LABELS.claim)
      .getByRole("button", { name: `Release the claim on ${one}` })
      .click();
    await expect.poll(async () => (await getIssue(one)).claim).toBeNull();
    await reopen(one);
    await expect(detailsRow(page, PROPERTY_LABELS.claim)).toContainText(CONTROL_LABELS.none);

    // (15) Every section opened on each load so far, and one closed is kept for the next issue.
    await reopen(two);
    await expect(marginSectionDisclosure(page, "development")).toHaveAttribute(
      "aria-expanded",
      "true"
    );
    await marginSectionDisclosure(page, "development").click();
    await expect(detailsRow(page, PROPERTY_LABELS.repository)).toHaveCount(0);
    expect(
      await page.evaluate(() => localStorage.getItem("dispatch.issue.margin-sections:alice"))
    ).toBe("development");
    await reopen(one);
    await expect(marginSectionDisclosure(page, "development")).toHaveAttribute(
      "aria-expanded",
      "false"
    );
    // A control elsewhere on the page that opens Details at a row of a closed section asks the
    // drawer for that row first (`useOpenDetailsAt`), and the section opens for it.
    await marginShell(page).evaluate(
      (aside, [event, row]) => aside.dispatchEvent(new CustomEvent(event, { detail: row })),
      [DETAILS_ROW_REQUEST_EVENT, "labels"] as const
    );
    await expect(
      marginSectionDisclosure(page, "development"),
      "a request for the Labels row left Development closed"
    ).toHaveAttribute("aria-expanded", "true");
    await expect(detailsRow(page, PROPERTY_LABELS.labels)).toBeVisible();
    await marginSectionDisclosure(page, "development").click();
    await expect(marginSectionDisclosure(page, "development")).toHaveAttribute(
      "aria-expanded",
      "false"
    );
    await marginSectionDisclosure(page, "development").click();
    await expect(marginSectionDisclosure(page, "development")).toHaveAttribute(
      "aria-expanded",
      "true"
    );

    // Last, a closed issue: `l` leaves Details shut, and Details offers nothing to write.
    await patchIssue(two, { status: "done" });
    await reopen(two);
    await closeDetails(page);
    await page.keyboard.press("l");
    await nextFrames(page);
    await expect(marginShell(page), "l opened Details on a closed issue").toBeHidden();
    await openDetails(page);
    // TWO has no label, so its control is named for adding one.
    await expect(
      detailsRow(page, PROPERTY_LABELS.labels).getByRole("button", {
        name: labelsControlName(two, { labels: [], saving: false, words: CONTROL_NAME_WORDS }),
      }),
      "a closed issue's labels can be edited"
    ).toBeDisabled();
    await expect(addControl(page), "a closed issue draws the add control").toHaveCount(0);
  });
});

/** The priority's line in Details: the Advances row while the issue advances a priority, and
 *  the control that adds one, beside Add, while it advances none. Both places carry the one test
 *  id, and Details draws one of them at a time. */
function priorityLine(page: Page): Locator {
  return marginSlot(page).getByTestId("issue-advances");
}

/** A priority the rows below create for themselves, beside the seed's own: the second one an
 *  issue advances, or the one its parent advances and it inherits. */
const SECOND_PRIORITY_TITLE = "Details hold a second priority";

/** What a row holds of the page's writes of one issue and of its reads of that issue after. */
interface HeldIssue {
  /** Resolves once the first write's answer has been handed to the page. */
  readonly landed: Promise<void>;
  /** How many reads of the issue are held. */
  readonly readsHeld: () => number;
  /** Lets every held read go, and resolves once one of them has answered the page. */
  readonly releaseReads: () => Promise<void>;
  /** Lets the held write go. */
  readonly releaseWrite: () => void;
  /** Resolves once the first write has left the page. */
  readonly writeOut: Promise<void>;
  /** How many writes of the issue the page has sent: one for each key a write goes under, so a
   *  write the page sends again under its own key, as it does after a long silence, is one. */
  readonly writes: () => number;
}

/**
 * Counts every write of the issue `key` the page sends (`PATCH /api/v1/issues/<key>`), holds
 * the first until `releaseWrite` where `holdWrite` is set, and holds every read of the issue the
 * page asks for once a write has gone on to Dispatch, until `releaseReads`: so a row sees the page
 * while the write is out, and between the write landing and the new read being drawn.
 */
async function holdIssue(page: Page, key: string, holdWrite: boolean): Promise<HeldIssue> {
  const path = `/api/v1/issues/${key}`;
  const writeKeys = new Set<string>();
  let readsHeld = 0;
  let holdingReads = false;
  const writeOut = Promise.withResolvers<void>();
  const writeGoes = Promise.withResolvers<void>();
  const landed = Promise.withResolvers<void>();
  const readsGo = Promise.withResolvers<void>();
  if (!holdWrite) writeGoes.resolve();
  await page.route(
    (url) => url.pathname === path,
    async (route) => {
      const request = route.request();
      if (request.method() === "PATCH") {
        writeKeys.add(request.headers()["idempotency-key"] ?? `unkeyed ${writeKeys.size}`);
        writeOut.resolve();
        await writeGoes.promise;
        // Held from before Dispatch takes the write: the issue's own event may ask for the read
        // before the write's answer reaches the page.
        holdingReads = true;
        await route.fulfill({ response: await route.fetch() });
        landed.resolve();
        return;
      }
      if (request.method() !== "GET" || !holdingReads) return route.fallback();
      readsHeld += 1;
      await readsGo.promise;
      // A read the page dropped meanwhile, as a later refresh of the same issue drops the one
      // before it, has nothing left to answer.
      await route.fallback().catch(() => undefined);
    }
  );
  return {
    landed: landed.promise,
    readsHeld: () => readsHeld,
    releaseReads: async () => {
      const answered = page.waitForResponse(
        (response) =>
          response.request().method() === "GET" && new URL(response.url()).pathname === path
      );
      readsGo.resolve();
      await answered;
    },
    releaseWrite: () => writeGoes.resolve(),
    writeOut: writeOut.promise,
    writes: () => writeKeys.size,
  };
}

/** Waits until the page has asked for a read of the issue after its write, and that read is
 *  held, then for the frames in which the page draws what it holds by then. */
async function readHeld(page: Page, issue: HeldIssue): Promise<void> {
  await expect
    .poll(() => issue.readsHeld(), { message: "the page read the issue no more after its write" })
    .toBeGreaterThan(0);
  await nextFrames(page);
}

/** The rule every row below holds at `moment`: one write of the issue sent, and the focus on
 *  the control or inside `region`, never on the page's body. */
async function expectFocusKept(
  page: Page,
  region: Locator,
  issue: HeldIssue,
  moment: string
): Promise<void> {
  await nextFrames(page);
  const focus = await region.evaluateAll((elements) => {
    const active = document.activeElement;
    if (active === null || active === document.body) return "the page's body";
    return elements.some((element) => element.contains(active))
      ? "kept"
      : active.outerHTML.slice(0, 160);
  });
  expect(
    { focus, writes: issue.writes() },
    `${moment}: the focus left the control's place, or other than one write went out`
  ).toEqual({ focus: "kept", writes: 1 });
}

/** The rule each priority's row holds after an Enter on `pressed` between the write landing and
 *  the new read being drawn: the press was refused, so `pressed` says it takes no press
 *  (`aria-disabled`) and the add control's form did not open. A control that took that Enter and
 *  opened its form would keep the focus in the line and send no write, so the focus and the
 *  writes alone cannot tell it from one that refused the press. */
async function expectPressRefused(page: Page, pressed: Locator, moment: string): Promise<void> {
  await nextFrames(page);
  await expect(
    priorityLine(page).getByRole("combobox", { name: CONTROL_LABELS.priority_to_advance }),
    `${moment}: the press opened the add control's form`
  ).toHaveCount(0);
  await expect(pressed, `${moment}: the control pressed takes a press`).toHaveAttribute(
    "aria-disabled",
    "true"
  );
}

// A control of Details shared with the rest of the page keeps the focus where the reader pressed
// it while its write is out and until the new read is drawn, and a second press meanwhile sends
// nothing. Each row presses its control by keyboard and holds the write, presses Enter again
// while it is out, lets it land and holds the issue's read that follows, presses Enter again
// while that read is held, and lets it go: at each of the three it reads where the focus is and
// how many writes went out. The priority's rows act on ONE's Advances row, which stays drawn
// through the write, so what they read is the shared line's alone.
test.describe("the shared controls of Details keep the focus while their write lands", () => {
  test("Details' priority Remove keeps the focus in the priority's line while the removal lands, and sends one write", async ({
    browser,
  }) => {
    test.setTimeout(120_000);
    await phase(browser, seedDetailsIssue, async (page, seeded) => {
      const { one, parent } = seeded.keys;
      // ONE's parent advances a second priority, which ONE inherits, so the Advances row is
      // still drawn once ONE's own priority is removed.
      const inherited = await createPriority({ owner: "alice", title: SECOND_PRIORITY_TITLE });
      await patchIssue(parent, { advances: [inherited.id] });
      await gotoIssue(page, `/issues/${one}`);
      await openDetails(page);
      const advances = detailsRow(page, PROPERTY_LABELS.advances);
      await expect(advances).toContainText(SECOND_PRIORITY_TITLE);
      const remove = advances.getByRole("button", { name: /^Remove / });
      await expect(remove).toHaveCount(1);
      const issue = await holdIssue(page, one, true);
      await remove.focus();
      await expect(remove).toBeFocused();
      await page.keyboard.press("Enter");
      await issue.writeOut;
      await nextFrames(page);
      await page.keyboard.press("Enter");
      await expectFocusKept(
        page,
        priorityLine(page),
        issue,
        "after a second Enter while it is out"
      );
      issue.releaseWrite();
      await issue.landed;
      await readHeld(page, issue);
      await page.keyboard.press("Enter");
      await expectFocusKept(
        page,
        priorityLine(page),
        issue,
        "after a second Enter between the removal landing and the new read"
      );
      await issue.releaseReads();
      await expect(advances.getByRole("button", { name: /^Remove / })).toHaveCount(0);
      await expect(advances).toContainText(SECOND_PRIORITY_TITLE);
      await expectFocusKept(page, priorityLine(page), issue, "once the new read is drawn");
    });
  });

  test("Details' Add a priority keeps the focus in the priority's line while the added priority lands, and sends one write", async ({
    browser,
  }) => {
    test.setTimeout(120_000);
    await phase(browser, seedDetailsIssue, async (page, seeded) => {
      const { one } = seeded.keys;
      const second = await createPriority({ owner: "alice", title: SECOND_PRIORITY_TITLE });
      await gotoIssue(page, `/issues/${one}`);
      await openDetails(page);
      const advances = detailsRow(page, PROPERTY_LABELS.advances);
      const addPriority = advances.getByRole("button", {
        name: `${CONTROL_LABELS.add_priority} to ${one}`,
      });
      await addPriority.focus();
      await expect(addPriority).toBeFocused();
      await page.keyboard.press("Enter");
      const picker = advances.getByRole("combobox", { name: CONTROL_LABELS.priority_to_advance });
      await expect(picker).toBeFocused();
      await expect(picker.locator("option", { hasText: SECOND_PRIORITY_TITLE })).toHaveCount(1);
      // The one priority ONE does not advance yet, the first after the picker's empty choice.
      await page.keyboard.press("ArrowDown");
      await expect(picker).toHaveValue(second.id);
      const issue = await holdIssue(page, one, true);
      await page.keyboard.press("Tab");
      const add = picker
        .locator("xpath=ancestor::form[1]")
        .getByRole("button", { exact: true, name: CONTROL_LABELS.add });
      await expect(add).toBeFocused();
      await page.keyboard.press("Enter");
      await issue.writeOut;
      await nextFrames(page);
      await page.keyboard.press("Enter");
      await expectFocusKept(
        page,
        priorityLine(page),
        issue,
        "after a second Enter while it is out"
      );
      issue.releaseWrite();
      await issue.landed;
      await readHeld(page, issue);
      await expect(
        addPriority,
        "the add control lost the focus as Add's write landed"
      ).toBeFocused();
      await page.keyboard.press("Enter");
      await expectFocusKept(
        page,
        priorityLine(page),
        issue,
        "after a second Enter between the added priority landing and the new read"
      );
      await expectPressRefused(
        page,
        addPriority,
        "after a second Enter between the added priority landing and the new read"
      );
      await issue.releaseReads();
      await expect(advances.getByRole("link")).toHaveCount(2);
      await expectFocusKept(page, priorityLine(page), issue, "once the new read is drawn");
      await expect(
        addPriority,
        "once the new read is drawn, the focus is not on the add control"
      ).toBeFocused();
    });
  });

  test("Details' Blocked by Save keeps the focus in its row while the save is out and lands, and sends one write", async ({
    browser,
  }) => {
    test.setTimeout(120_000);
    await phase(browser, seedDetailsIssue, async (page, seeded) => {
      const { three, two } = seeded.keys;
      await gotoIssue(page, `/issues/${two}`);
      await openDetails(page);
      const row = detailsRow(page, PROPERTY_LABELS.blocked_by);
      const open = row.getByRole("button", { name: `Add issues ${two} waits on` });
      await open.focus();
      await page.keyboard.press("Enter");
      await expect(row.getByLabel(`Issues ${two} waits on`, { exact: true })).toBeFocused();
      await page.keyboard.type(three);
      const issue = await holdIssue(page, two, true);
      const save = row.getByRole("button", { exact: true, name: CONTROL_LABELS.save_blocked_by });
      await save.focus();
      await expect(save).toBeFocused();
      await page.keyboard.press("Enter");
      await issue.writeOut;
      await nextFrames(page);
      await page.keyboard.press("Enter");
      await expectFocusKept(page, row, issue, "after a second Enter while the save is out");
      issue.releaseWrite();
      await issue.landed;
      // The save's answer is drawn as it lands: TWO waits on THREE.
      await expect(row.getByRole("link", { exact: true, name: three })).toBeVisible();
      await readHeld(page, issue);
      await page.keyboard.press("Enter");
      await expectFocusKept(
        page,
        row,
        issue,
        "after a second Enter between the save landing and the new read"
      );
      await issue.releaseReads();
      await expectFocusKept(page, row, issue, "once the new read is drawn");
    });
  });

  test("Details' Blocked by Cancel keeps the focus in its row while the save is out, and the save sends one write", async ({
    browser,
  }) => {
    test.setTimeout(120_000);
    await phase(browser, seedDetailsIssue, async (page, seeded) => {
      const { three, two } = seeded.keys;
      await gotoIssue(page, `/issues/${two}`);
      await openDetails(page);
      const row = detailsRow(page, PROPERTY_LABELS.blocked_by);
      const open = row.getByRole("button", { name: `Add issues ${two} waits on` });
      await open.focus();
      await page.keyboard.press("Enter");
      await expect(row.getByLabel(`Issues ${two} waits on`, { exact: true })).toBeFocused();
      await page.keyboard.type(three);
      const issue = await holdIssue(page, two, true);
      // The box's Enter sends the list, so Save never held the focus as the save went out.
      await page.keyboard.press("Enter");
      await issue.writeOut;
      await nextFrames(page);
      const cancel = row.getByRole("button", { exact: true, name: CONTROL_LABELS.cancel });
      await cancel.focus();
      await expect(cancel, "Cancel does not take the focus while the save is out").toBeFocused();
      await page.keyboard.press("Enter");
      await expectFocusKept(page, row, issue, "after Cancel's Enter while the save is out");
      issue.releaseWrite();
      await issue.landed;
      // The save's answer is drawn as it lands: TWO waits on THREE.
      await expect(row.getByRole("link", { exact: true, name: three })).toBeVisible();
      await readHeld(page, issue);
      await page.keyboard.press("Enter");
      await expectFocusKept(
        page,
        row,
        issue,
        "after a second Enter between the save landing and the new read"
      );
      await issue.releaseReads();
      await expectFocusKept(page, row, issue, "once the new read is drawn");
    });
  });
});

// Details draws the priority's line in one of two places: beside Add while the issue advances
// none, and in the Advances row once it advances one. The first priority added and the last one
// removed move the line from one place to the other, and a focus the line still holds when it
// leaves moves with it, to the other place's add control, rather than fall to the page's body
// with the line it left. Each of these two rows presses Enter again on the control it pressed
// while the issue's read after the write is held, which the control refuses, and reads, once the
// new read is drawn, that the focus is on the add control of the line's new place, and the
// writes sent.
test("Details' Add a priority on an issue that advances none keeps the focus in Details' priority place once the line moves into the Advances row, and sends one write", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  await phase(browser, seedDetailsIssue, async (page, seeded) => {
    const { three } = seeded.keys;
    await gotoIssue(page, `/issues/${three}`);
    await openDetails(page);
    const addPriority = priorityLine(page).getByRole("button", {
      name: `${CONTROL_LABELS.add_priority} to ${three}`,
    });
    await addPriority.focus();
    await expect(addPriority).toBeFocused();
    await page.keyboard.press("Enter");
    const picker = priorityLine(page).getByRole("combobox", {
      name: CONTROL_LABELS.priority_to_advance,
    });
    await expect(picker).toBeFocused();
    await expect(picker.locator("option", { hasText: PRIORITY_TITLE })).toHaveCount(1);
    // The first priority after the picker's empty choice, by its arrow key.
    await page.keyboard.press("ArrowDown");
    await expect(picker).toHaveValue(seeded.priorityId);
    const issue = await holdIssue(page, three, false);
    await page.keyboard.press("Tab");
    const add = picker
      .locator("xpath=ancestor::form[1]")
      .getByRole("button", { exact: true, name: CONTROL_LABELS.add });
    await expect(add).toBeFocused();
    await page.keyboard.press("Enter");
    await issue.landed;
    await readHeld(page, issue);
    await expect(addPriority, "the add control lost the focus as Add's write landed").toBeFocused();
    await page.keyboard.press("Enter");
    await expectPressRefused(
      page,
      addPriority,
      "after a second Enter between the added priority landing and the new read"
    );
    await issue.releaseReads();
    // THREE advances the priority now, so its line is the Advances row.
    await expect(detailsRow(page, PROPERTY_LABELS.advances).getByRole("link")).toHaveCount(1);
    await expectFocusKept(
      page,
      priorityLine(page),
      issue,
      "once the new read moved the line into the Advances row"
    );
    await expect(
      detailsRow(page, PROPERTY_LABELS.advances).getByRole("button", {
        name: `${CONTROL_LABELS.add_priority} to ${three}`,
      }),
      "once the new read moved the line into the Advances row, the focus is not on its add control"
    ).toBeFocused();
  });
});

test("Details' priority Remove of an issue's last priority keeps the focus in Details' priority place once the line moves back beside Add, and sends one write", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  await phase(browser, seedDetailsIssue, async (page, seeded) => {
    const { one } = seeded.keys;
    await gotoIssue(page, `/issues/${one}`);
    await openDetails(page);
    const remove = detailsRow(page, PROPERTY_LABELS.advances).getByRole("button", {
      name: /^Remove /,
    });
    await expect(remove).toHaveCount(1);
    const issue = await holdIssue(page, one, false);
    await remove.focus();
    await expect(remove).toBeFocused();
    await page.keyboard.press("Enter");
    await issue.landed;
    await readHeld(page, issue);
    await expect(remove, "Remove lost the focus as its write landed").toBeFocused();
    await page.keyboard.press("Enter");
    await expectPressRefused(
      page,
      remove,
      "after a second Enter between the removal landing and the new read"
    );
    await issue.releaseReads();
    // ONE advances nothing now, so its line is the control beside Add.
    await expect(detailsRow(page, PROPERTY_LABELS.advances)).toHaveCount(0);
    const addPriority = priorityLine(page).getByRole("button", {
      name: `${CONTROL_LABELS.add_priority} to ${one}`,
    });
    await expect(addPriority).toBeVisible();
    await expectFocusKept(
      page,
      priorityLine(page),
      issue,
      "once the new read moved the line back beside Add"
    );
    await expect(
      addPriority,
      "once the new read moved the line back beside Add, the focus is not on its add control"
    ).toBeFocused();
  });
});
