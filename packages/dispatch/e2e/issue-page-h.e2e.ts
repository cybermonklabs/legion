import AxeBuilder from "@axe-core/playwright";
import { labelsControlName } from "../web/src/features/issue/IssueLabels";
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
import { getIssue, patchIssue, putRepoProject } from "./api";
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
 * Details' properties and pins (dispatch://INFRA-3945, part H of dispatch://INFRA-1196): the
 * rows of the three sections and their controls, the one Add control, the key `l`, the reader's
 * pins, and no comment, question or message of the issue. Every row reads `seedDetailsIssue`
 * as alice with the new page on (`phase`), and writes through Dispatch.
 */

/** Development's rows, in the order R2 of the part gives them, as ONE holds them: ONE's spec
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
      // The facts of the row the reader is on quote that row, which may be the issue's
      // question or a comment, by design (part M's R6, decision 147); nothing else of Details
      // draws the conversation.
      const outsideFacts = (text: string) =>
        marginShell(page)
          .getByText(text, { exact: false })
          .evaluateAll(
            (found) =>
              found.filter((one) => one.closest('[data-testid="focused-node-facts"]') === null)
                .length
          );
      expect(await outsideFacts(UNPINNED_COMMENT), "Details draws a comment of the issue").toBe(0);
      expect(await outsideFacts(QUESTION), "Details draws a question of the issue").toBe(0);
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

    // (5) Every width: R2's order, the twenty labels whole, and each value in its row.
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
      const values: readonly (readonly [string, Locator])[] = [
        [
          "the repository",
          detailsRow(page, PROPERTY_LABELS.repository).getByText(REPOSITORY).first(),
        ],
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
          "Referenced by's control",
          marginSection(page, "references").getByRole("button", {
            exact: true,
            name: PROPERTY_LABELS.referenced_by,
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
    // Who opened the issue and who holds its claim are part E's `AgentName`, which draws a
    // session's link in the ink inside `NoTooltips` (its own underline is E's, R8's last
    // sentence): alice opened ONE, a person, so her name is words; the planner's claim is a
    // session's link.
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

// R4 and (5): the Labels row lists every label, none cut. Found by the tester on the preview of
// d4344116: on INFRA-3570 a 40-character label ran 39 px past Details' edge at 1440 px and was
// cut, at 390 px as well.
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

// (5): the repository is visible in its row, whole. Found by the tester on the preview of
// 962ec632: on INFRA-3570 a repository of 44 characters ran 101 px past Details' Repository row
// at 1440 px and was cut at Details' edge, at 390 px as well.
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
    await addControl(page).click();
    // The assignee is listed where the header draws its control, which the menu presses: the
    // header is read once it is drawn.
    await expect(
      issuePage(page).getByRole("heading", { level: 1, name: (await getIssue(two)).title })
    ).toBeVisible();
    const headerAssignee = await page.locator(`[aria-label="Assignee of ${two}"]`).count();
    await expect(
      openMenu(page).getByRole("menuitem"),
      "the Add menu lists other than TWO's empty fields, in R5's order"
    ).toHaveText([
      PROPERTY_LABELS.parent,
      ...(headerAssignee > 0 ? [PROPERTY_LABELS.assignee] : []),
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
    // drawer for that row first (part D's `useOpenDetailsAt`), and the section opens for it.
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
