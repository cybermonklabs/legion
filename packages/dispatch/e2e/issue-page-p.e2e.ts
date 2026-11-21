import { ACTION_WORDS } from "../web/src/features/issue-page/approval-chip-box";
import { statusGlyph } from "../web/src/features/issue-page/issue-status";
import { CONTROL_LABELS, STATE_LINES } from "../web/src/features/issue-page/page-words";
import {
  createAsk,
  createIssue,
  createIssueArtifact,
  createProject,
  editArtifact,
  getArtifact,
  getArtifactVersion,
  getAsk,
  getIssue,
  patchIssue,
  requestApproval,
  reviewArtifact,
  setGatedSteps,
} from "./api";
import { closeDetails, gotoIssue, phase } from "./issue-page";
import { focusThePage } from "./issue-page-g";
import { withIssuePage } from "./issue-page-option";
import {
  actionsMenuDrop,
  approvalChip,
  approvalEntry,
  asAgent,
  badgeValue,
  DESKTOP,
  details,
  expectHeaderDrawn,
  expectHeaderWithinItsLimit,
  firstControlOfRowOne,
  headerFamilies,
  headerLabels,
  headerParts,
  holdClipboard,
  holdLabelsAtTheirChips,
  holdReviewReads,
  IPHONE_13,
  identityBand,
  issueDecisions,
  issueHeader,
  LABELS_READING,
  type LabelsAgainstTheCount,
  LONGEST_LABEL,
  labelFillingTheRoom,
  labelsAgainstTheCount,
  labelsLeft,
  lineOrder,
  MEASURED_TITLE,
  PHONE_TITLE,
  propertiesLine,
  seedHeader,
  shortControls,
  stickyReading,
  TWENTY_LABELS,
  TYPEFACE,
  timesDrawn,
  titleOfLines,
  typefaceAddress,
  typefaceStatuses,
  undersizedTargets,
  viewportAt,
  waitingCount,
  waitingPressInHistory,
} from "./issue-page-p";
import { leaveFocus, onBody } from "./keyboard-issue";
import { boxesApart } from "./layout";
import { answeredPost, holdPosts, navigateInApp } from "./sends";
import { expect, type Locator, type Page, type Request, rowKey, rowPerson, test } from "./test";
import { assertPageFits, assertWhole } from "./unclipped";
import { asUser } from "./users";

/**
 * The new issue page's header: the list of what it draws and its limits, the approval control,
 * the typeface's fallback, the count of what waits, the title's editor, the keys, and what stays
 * as the page scrolls. Each row turns the new page on (`phase`), first waits for the header's
 * properties line (`expectHeaderDrawn`), and reads the header alone: no row waits for a row of
 * the outline.
 */

/** The person the server resolves the ask `askId` to (`waiting_on_person`): whoever it was handed
 *  to, else the issue's assignee, else the project's default assignee. `GET /api/v1/asks/{id}`
 *  answers `AskRead`, whose `ask` carries `waiting_on_person`, resolved at read time: the stored
 *  row holds no such field (`packages/contracts/src/dispatch-api-asks.ts`). */
async function waitingOnPersonOf(askId: string): Promise<string | undefined> {
  return (await getAsk(askId)).ask.waiting_on_person;
}

/** The page's own address of the view a press of the count opens. */
function decisionsAddress(key: string): RegExp {
  return new RegExp(`/issues/${key}/decisions$`);
}

/** Whether `request` is a write of `key`'s own fields whose body holds `field`. */
function writesField(request: Request, key: string, field: string): boolean {
  if (request.method() !== "PATCH" || new URL(request.url()).pathname !== `/api/v1/issues/${key}`)
    return false;
  const body: unknown = request.postDataJSON();
  return typeof body === "object" && body !== null && field in body;
}

/** The review actions of an approval inside `scope`: Approve (`Approve v2` on a stale approval)
 *  and Request changes. Each is found as the header finds it, by the words it draws
 *  (`ACTION_WORDS`), not by its name: where two documents' requests wait on the reader, each
 *  action's name also says its document ("Approve the spec"). The chip itself (`Approved v1`) is
 *  none of them, and neither is a button inside a dialog, as the Request changes dialog's own
 *  Request changes, which stands inside the header: the header's own list of the two actions
 *  (`CHIP_ACTIONS`) leaves those out too. */
function reviewActions(scope: Locator): Locator {
  const sources = Object.values(ACTION_WORDS).map((pattern) => pattern.source);
  return scope
    .getByRole("button")
    .filter({ hasText: new RegExp(sources.join("|")) })
    .and(scope.locator(':not([role="dialog"] *)'));
}

/** The words the labels' count reads with `left` labels it does not draw: `+3 labels`. */
function labelsLeftWords(left: number): string {
  return `+${left} ${left === 1 ? CONTROL_LABELS.labels_left_one : CONTROL_LABELS.labels_left_many}`;
}

/** A pattern that matches `words` whole and nothing else, each character as itself: a status's
 *  mark or a key holds characters a pattern reads otherwise. */
function exactly(words: string): RegExp {
  return new RegExp(`^${words.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}$`);
}

/** How a row presses a control: the mouse's click, a tap of the phone's touch screen, or Enter
 *  with the focus on it. */
type Press = "click" | "Enter" | "tap";

/** Presses `control` as the row's project and width do it. */
async function press(control: Locator, how: Press): Promise<void> {
  if (how === "click") {
    await control.click();
    return;
  }
  if (how === "tap") {
    await control.tap();
    return;
  }
  await control.press("Enter");
}

/** A real press on `control` straight out of the title's edit, `what` naming it: on `iphone` a
 *  tap, whose mouse events follow its touch events, and on `chromium` the mouse's down and up at
 *  its middle. */
async function pressOutOfTheEdit(
  page: Page,
  control: Locator,
  what: string,
  tap: boolean
): Promise<void> {
  if (tap) {
    await control.tap();
    return;
  }
  // Both callers wait for the header drawn and the title's editor focused first, so the control
  // is drawn here; a null box only throws.
  const box = await control.boundingBox();
  if (box === null) throw new Error(`${what} is not drawn while the title is edited`);
  await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2);
  await page.mouse.down();
  await page.mouse.up();
}

// Row (1): the header draws every property with a value at every width, on the phone's title
// (`PHONE_TITLE`, the measured title less a word, which the 390 px limit is read on) and 20
// labels.
// On the phone it reads what the 390 px viewport decides, a layout claim "Phone acceptance" in
// `packages/dispatch/AGENTS.md` tags `@phone` for: the header's height limit there, the 24 px
// targets, the page no wider than the window, the navigation's control first on row one and the
// dialog it opens, and the 44 px buttons and 16 px type of the dialog a header control opens.
// SLOW(>=2s): 15,176 ms on chromium and 7,070 ms on iphone (one worker, Job e2e-rows-rdf2g at
// 43949bc0): twelve loads of the issue on chromium and five on iphone, and at each of its widths
// every part of the header read whole and Details opened and closed three times.
test("the header holds its list and its limits at every width @phone", async ({ browser }) => {
  test.setTimeout(300_000);
  await phase(browser, seedHeader, async (page, seeded) => {
    await patchIssue(seeded.key, { labels: [...TWENTY_LABELS], title: PHONE_TITLE });
    const desktopProject = test.info().project.name !== "iphone";

    // No approval request waits on the reader: the approval control reads Not requested and
    // carries no Approve, and the header holds its limit for that case.
    for (const width of desktopProject ? [DESKTOP.width, IPHONE_13.width] : [IPHONE_13.width]) {
      await page.setViewportSize(viewportAt(width));
      await gotoIssue(page, `/issues/${seeded.key}`);
      await expectHeaderDrawn(page);
      await expect(issueHeader(page)).toContainText(PHONE_TITLE);
      await expect(issueHeader(page)).toContainText(CONTROL_LABELS.not_requested);
      await expect(issueHeader(page).getByRole("button", { name: "Approve" })).toHaveCount(0);
      await expectHeaderWithinItsLimit(page, width, false, TYPEFACE);
    }

    // An approval request that waits on the reader.
    await requestApproval(seeded.specArtifactId, { summary: "Read it back." });
    // The header's list, each part of the header by what it is: nothing else may be drawn there.
    // Its one link is the parent's, named exactly. The words it draws outside a control are in it:
    // the project, the crumbs' separators, the key, the priority and the assignee each drawn under
    // its select, and the one chip's "Not requested". The status's shape is an svg with no words.
    const named = [
      new RegExp(`^a ${seeded.parentKey}$`),
      /^button Open navigation$/,
      /^button Copy issue key /,
      /^button Issue actions$/,
      /^button Details$/,
      /^h1 /,
      /^select Status$/,
      new RegExp(`^select Priority of ${seeded.key}$`),
      new RegExp(`^select Assignee of ${seeded.key}$`),
      /^issue-header$/,
      /^issue-details-control$/,
      /^issue-state-actions$/,
      /^issue-header-labels$/,
      /^issue-labels-left$/,
      /^issue-progress-children$/,
      /^issue-progress-subtree$/,
      /^issue-whose-turn$/,
      /^issue-approval-steps$/,
      /^button (Awaiting approval|Approve|Request changes)/,
      /^issue-route-unreachable$/,
      exactly(`text ${seeded.project}`),
      exactly("text ›"),
      exactly(`text ${seeded.key}`),
      exactly("text P1"),
      exactly("text alice"),
      exactly(`text ${CONTROL_LABELS.not_requested}`),
    ];

    let drawnAtDesktop: number | undefined;
    for (const width of desktopProject ? [1440, 1280, 1024, 390] : [390]) {
      await page.setViewportSize(viewportAt(width));
      await gotoIssue(page, `/issues/${seeded.key}`);
      await expectHeaderDrawn(page);
      await expect(issueHeader(page)).toContainText(PHONE_TITLE);
      await page.evaluate(() => window.scrollTo(0, 0));

      // The header's own values, each where it is drawn, with no control opened.
      const header = issueHeader(page);
      const band = identityBand(page);
      const inTheHeader: readonly (readonly [string, Locator, (string | RegExp)?])[] = [
        ["the project", band.getByText(seeded.project, { exact: true }), seeded.project],
        ["the parent", header.getByRole("link", { exact: true, name: seeded.parentKey })],
        ["the key", band.getByText(seeded.key, { exact: true }), seeded.key],
        ["the copy control", header.getByRole("button", { name: /^Copy issue key / })],
        [
          "the actions menu",
          header.getByRole("button", { exact: true, name: CONTROL_LABELS.issue_actions }),
        ],
        ["Details", header.getByTestId("issue-details-control"), CONTROL_LABELS.details],
        ["the status", header.getByRole("combobox", { name: "Status" })],
        ["the priority", badgeValue(page, `Priority of ${seeded.key}`), "P1"],
        ["the assignee", badgeValue(page, `Assignee of ${seeded.key}`), "alice"],
        [
          "the children's progress",
          header.getByTestId("issue-progress-children"),
          `${CONTROL_LABELS.children} 1 ${CONTROL_LABELS.count_of} 3`,
        ],
        [
          "the whole tree's progress",
          header.getByTestId("issue-progress-subtree"),
          `${CONTROL_LABELS.whole_tree} 1 ${CONTROL_LABELS.count_of} 3`,
        ],
        ["the count of what waits", waitingCount(page), /^Waiting on you \(\d+\)$/],
        ["Approve", header.getByRole("button", { exact: true, name: "Approve" })],
        ["Request changes", header.getByRole("button", { exact: true, name: "Request changes" })],
      ];
      for (const [name, at, words] of inTheHeader) {
        await expect(at, `${name} is not drawn in the header at ${width} px`).toBeVisible();
        if (words !== undefined) {
          await expect(at, `${name} does not read "${words}" at ${width} px`).toHaveText(words);
        }
        await expect(at, `${name} is outside the ${width} px viewport`).toBeInViewport();
        await assertWhole(at);
      }
      // Each count of the children's progress draws no "done" and carries it in its name, the
      // whole sentence a reader hears.
      for (const [name, at, heard] of [
        [
          "the children's progress",
          header.getByTestId("issue-progress-children"),
          `1 ${CONTROL_LABELS.count_of} 3 ${CONTROL_LABELS.children.toLowerCase()} ${CONTROL_LABELS.count_done}`,
        ],
        [
          "the whole tree's progress",
          header.getByTestId("issue-progress-subtree"),
          `1 ${CONTROL_LABELS.count_of} 3 ${CONTROL_LABELS.count_of} ${CONTROL_LABELS.whole_tree.toLowerCase()} ${CONTROL_LABELS.count_done}`,
        ],
      ] as const) {
        await expect(at, `${name} is not named "${heard}" at ${width} px`).toHaveAccessibleName(
          heard
        );
      }
      // The first label, or, where no chip fits whole on the line, the labels' count alone,
      // which then names every label.
      if ((await headerLabels(page).count()) === 0) {
        await expect(
          labelsLeft(page),
          `the labels' count does not read every label at ${width} px`
        ).toHaveText(labelsLeftWords(TWENTY_LABELS.length));
        await expect(
          labelsLeft(page),
          `the labels' count is outside the ${width} px viewport`
        ).toBeInViewport();
        await assertWhole(labelsLeft(page));
      } else {
        const first = headerLabels(page).first();
        await expect(
          first,
          `the first label is not drawn in the header at ${width} px`
        ).toBeVisible();
        await expect(
          first,
          `the first label does not read "${TWENTY_LABELS[0]}" at ${width} px`
        ).toHaveText(TWENTY_LABELS[0]);
        await expect(first, `the first label is outside the ${width} px viewport`).toBeInViewport();
        await assertWhole(first);
      }
      expect(
        await header.getByRole("combobox", { name: "Status" }).inputValue(),
        `the status reads otherwise at ${width} px`
      ).toBe("in_progress");
      // The status draws its shape beside its word, as an svg that names the shape it draws.
      await expect(
        header.locator('label:has(> select[aria-label="Status"]) svg[data-status-glyph]'),
        `the status draws another shape, or none, at ${width} px`
      ).toHaveAttribute("data-status-glyph", statusGlyph("in_progress"));
      // Approve is filled with the page's ink, its words the paper's (row 17).
      expect(
        await header
          .getByRole("button", { exact: true, name: "Approve" })
          .evaluate((approve) => [
            getComputedStyle(approve).backgroundColor,
            getComputedStyle(approve).color,
          ]),
        `Approve is not drawn in the ink at ${width} px`
      ).toEqual(["rgb(28, 25, 23)", "rgb(255, 255, 255)"]);

      // The header draws its list and nothing else, in the list's order on its line.
      const parts = await headerParts(page);
      expect(
        parts.filter((part) => !named.some((pattern) => pattern.test(part))),
        `the header draws parts its list does not name at ${width} px`
      ).toEqual([]);
      // The list holds the words the header draws as text, so a property drawn that way is in it.
      expect(
        [`text ${seeded.project}`, `text ${seeded.key}`].filter((words) => !parts.includes(words)),
        `the header's parts leave out the words it draws as text at ${width} px`
      ).toEqual([]);
      const order = [
        "select Status",
        `select Priority of ${seeded.key}`,
        `select Assignee of ${seeded.key}`,
        "issue-progress-children",
        "issue-whose-turn",
        "issue-header-labels",
      ];
      expect(
        parts.filter((part) => order.includes(part)),
        `the properties line is out of the list's order at ${width} px`
      ).toEqual(order);
      // The approval control stands after the count and before the labels.
      expect(
        await lineOrder(page, [
          { name: "the count", selector: '[data-testid="issue-whose-turn"]' },
          { name: "the approval control", selector: "[data-approval-artifact]" },
          { name: "the labels", selector: '[data-testid="issue-header-labels"]' },
        ]),
        `the approval control is not between the count and the labels at ${width} px`
      ).toEqual(["the count", "the approval control", "the labels"]);

      // The page draws the key and the title one time, the title as its one `h1`.
      await expect(
        page.locator("[data-issue-page] h1"),
        `the issue page does not hold one h1 at ${width} px`
      ).toHaveCount(1);
      expect(
        await timesDrawn(page.locator("[data-issue-page]"), PHONE_TITLE),
        "the page draws the title more than once"
      ).toBe(1);
      expect(
        await timesDrawn(header, seeded.key),
        `the header does not draw the key once at ${width} px`
      ).toBe(1);

      // Where the shell draws no rail, row one starts with the navigation's control; beside the
      // rail the header draws none.
      if (width === IPHONE_13.width) {
        expect(
          await firstControlOfRowOne(page),
          "the navigation's control is not the first control of row one at 390 px"
        ).toBe("Open navigation");
      }
      if (width === DESKTOP.width) {
        await expect(
          header.getByRole("button", { exact: true, name: "Open navigation" }),
          "the navigation's control is drawn beside the rail at 1440 px"
        ).toHaveCount(0);
      }

      // The labels that fit, and a count equal to the labels the line does not draw.
      await expect(labelsLeft(page), `the labels end on no count at ${width} px`).toBeVisible();
      const drawn = await headerLabels(page).count();
      expect(drawn, `the header draws every label at ${width} px`).toBeLessThan(20);
      if (width === DESKTOP.width) drawnAtDesktop = drawn;
      await expect(labelsLeft(page)).toHaveText(labelsLeftWords(TWENTY_LABELS.length - drawn));

      // The frame after the line narrows, drawn before the labels are read again on the next
      // frame, draws each label whole or not at all: one that no longer fits wraps onto a line
      // the labels' box clips away. The line then takes its width back, and the labels are read
      // again before the next step.
      if (width === DESKTOP.width || width === IPHONE_13.width) {
        expect(
          (await labelsAgainstTheCount(page, 60)).cut,
          `a label is cut in the frame after the line narrowed at ${width} px`
        ).toEqual([]);
        await propertiesLine(page).evaluate((line) => line.style.removeProperty("max-width"));
        let before = -1;
        await expect
          .poll(
            async () => {
              const now = await headerLabels(page).count();
              const words = ((await labelsLeft(page).textContent()) ?? "").replace(/\s+/g, " ");
              const settled =
                now === before && words.trim() === labelsLeftWords(TWENTY_LABELS.length - now);
              before = now;
              return settled;
            },
            { message: `the labels were not read again once the line widened at ${width} px` }
          )
          .toBe(true);
      }

      await expectHeaderWithinItsLimit(page, width, true, TYPEFACE);
      expect(
        await undersizedTargets(page),
        `a header control fails WCAG 2.5.8 at ${width} px`
      ).toEqual([]);
      expect(
        await shortControls(page, seeded.key),
        `a control held to 24 px is shorter at ${width} px`
      ).toEqual([]);
      await assertPageFits(page);
      if (width === IPHONE_13.width) {
        // A dialog a header control opens keeps the page's floors at 390 px: 16 px type in its
        // fields, so a phone does not zoom into them, and 44 px buttons (`web/src/styles.css`).
        await header.getByRole("button", { exact: true, name: "Request changes" }).click();
        const requestChanges = page.getByRole("dialog", { name: "Request changes" });
        await expect(requestChanges, "Request changes opened no dialog at 390 px").toBeVisible();
        expect(
          await requestChanges
            .locator("textarea")
            .evaluate((reason) => getComputedStyle(reason).fontSize),
          "the Request changes dialog's reason box is under 16 px at 390 px"
        ).toBe("16px");
        const heights = await requestChanges
          .locator("button")
          .evaluateAll((buttons) => buttons.map((button) => button.getBoundingClientRect().height));
        expect(
          heights.length,
          "the Request changes dialog draws no button at 390 px"
        ).toBeGreaterThan(0);
        expect(
          heights.filter((height) => height < 44),
          "a button of the Request changes dialog is under 44 px at 390 px"
        ).toEqual([]);
        await page.keyboard.press("Escape");
        await expect(requestChanges, "Escape left the Request changes dialog open").toHaveCount(0);
      }

      // The labels' count, Details' control and the menu's parent action - Edit parent, on an
      // issue with a parent - each open Details.
      await labelsLeft(page).click();
      await expect(details(page), "the labels' count opened no Details").toBeVisible();
      await closeDetails(page);
      await header.getByTestId("issue-details-control").click();
      await expect(details(page), "Details' control opened no Details").toBeVisible();
      await closeDetails(page);
      const actionsControl = header.getByRole("button", {
        exact: true,
        name: CONTROL_LABELS.issue_actions,
      });
      await actionsControl.click();
      await expect(
        actionsControl,
        `the actions control opened no menu at ${width} px`
      ).toHaveAttribute("aria-expanded", "true");
      // The open menu hangs under its control, so a second press on the control reaches the
      // control, which closes the menu, rather than the menu's first action.
      expect(
        await actionsMenuDrop(page),
        `the actions menu opens over its own control at ${width} px`
      ).toBeGreaterThanOrEqual(-0.5);
      // From 761 px to 1279 px each action of the open menu keeps `main`'s 44 px floor, as a
      // menu a header control opens does.
      if (width === 1024) {
        const actionHeights = await header
          .getByRole("button", { exact: true, name: CONTROL_LABELS.issue_actions })
          .evaluate((control) => {
            const menu = document.getElementById(control.getAttribute("aria-controls") ?? "");
            return [...(menu?.querySelectorAll("button") ?? [])].map(
              (action) => action.getBoundingClientRect().height
            );
          });
        expect(actionHeights.length, "the actions menu draws no action at 1024 px").toBeGreaterThan(
          0
        );
        expect(
          actionHeights.filter((height) => height < 44 - 0.5),
          "an action of the actions menu is under 44 px at 1024 px"
        ).toEqual([]);
      }
      await header.getByRole("button", { exact: true, name: CONTROL_LABELS.edit_parent }).click();
      await expect(
        details(page),
        `the menu's parent action opened no Details at ${width} px`
      ).toBeVisible();
      await closeDetails(page);
      if (width === IPHONE_13.width) {
        await header.getByRole("button", { exact: true, name: "Open navigation" }).click();
        await expect(
          page.getByRole("dialog"),
          "the navigation's control opened no dialog at 390 px"
        ).toBeVisible();
        await page.keyboard.press("Escape");
      }
    }

    if (drawnAtDesktop !== undefined) {
      await page.setViewportSize(viewportAt(DESKTOP.width));

      // At 1440 px, the labels the line drew there of the twenty, which fit beside a count, and
      // after them a label no line holds whole, end on the count of one label.
      await patchIssue(seeded.key, {
        labels: [...TWENTY_LABELS.slice(0, drawnAtDesktop), LONGEST_LABEL],
      });
      await gotoIssue(page, `/issues/${seeded.key}`);
      await expectHeaderDrawn(page);
      await expect(
        labelsLeft(page),
        `${drawnAtDesktop + 1} labels at 1440 px do not end on "+1 ${CONTROL_LABELS.labels_left_one}"`
      ).toHaveText(`+1 ${CONTROL_LABELS.labels_left_one}`);
      await expect(headerLabels(page)).toHaveCount(drawnAtDesktop);

      // Labels that fit on the line only while no count is drawn are all drawn, and no count
      // is drawn: those labels alone, then with one more as wide as the room they leave less 4 px,
      // which the count and its gap would take.
      await patchIssue(seeded.key, { labels: TWENTY_LABELS.slice(0, drawnAtDesktop) });
      await gotoIssue(page, `/issues/${seeded.key}`);
      await expectHeaderDrawn(page);
      await expect(headerLabels(page)).toHaveCount(drawnAtDesktop);
      await expect(labelsLeft(page)).toHaveCount(0);
      const lastThatFits = await labelFillingTheRoom(page, 4);
      await patchIssue(seeded.key, {
        labels: [...TWENTY_LABELS.slice(0, drawnAtDesktop), lastThatFits],
      });
      await gotoIssue(page, `/issues/${seeded.key}`);
      await expectHeaderDrawn(page);
      await expect(headerLabels(page), "labels that fit draw a count").toHaveText([
        ...TWENTY_LABELS.slice(0, drawnAtDesktop),
        lastThatFits,
      ]);
      await expect(labelsLeft(page), "labels that fit draw a count").toHaveCount(0);
    }

    // An issue whose labels fit draws them all and no count.
    await page.setViewportSize(viewportAt(DESKTOP.width));
    await patchIssue(seeded.key, { labels: ["dispatch"] });
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectHeaderDrawn(page);
    await expect(headerLabels(page)).toHaveText(["dispatch"]);
    await expect(labelsLeft(page), "a label that fits ends on a count").toHaveCount(0);
    await expect(issueHeader(page).getByTestId("issue-route-unreachable")).toHaveCount(0);

    // A child of the issue, which has no children of its own, draws no children's progress.
    await gotoIssue(page, `/issues/${seeded.childKeys[0]}`);
    await expectHeaderDrawn(page);
    await expect(
      issueHeader(page).getByTestId("issue-progress-children"),
      "an issue with no children draws its progress"
    ).toHaveCount(0);

    // The parent, whose route reaches nobody, draws the header's one marker, after its approval
    // control and before its one label.
    await patchIssue(seeded.parentKey, { labels: ["dispatch"] });
    await gotoIssue(page, `/issues/${seeded.parentKey}`);
    await expectHeaderDrawn(page);
    await expect(issueHeader(page).getByTestId("issue-route-unreachable")).toHaveCount(1);
    await expect(issueHeader(page).getByRole("button", { name: "Approve" })).toHaveCount(0);
    await expect(issueHeader(page)).toContainText(CONTROL_LABELS.not_requested);
    await expect(headerLabels(page)).toHaveText(["dispatch"]);
    expect(
      await lineOrder(page, [
        { name: "the approval control", selector: "span", words: CONTROL_LABELS.not_requested },
        { name: "the route's marker", selector: '[data-testid="issue-route-unreachable"]' },
        { name: "the labels", selector: '[data-testid="issue-header-labels"]' },
      ]),
      `the route's marker is not between "${CONTROL_LABELS.not_requested}" and the labels`
    ).toEqual(["the approval control", "the route's marker", "the labels"]);
  });
});

// Row (2): a gated root's approval entries stay in the header, and each reads the document its
// step approves.
// SLOW(>=2s): 7,145 ms (one worker, Job e2e-rows-rdf2g at 43949bc0): two more projects and
// eight more issues written with their documents and reviews, seven loads and two moves within
// the app, and three approvals pressed in the header, each polled until Dispatch holds it.
test("the approval entries stay in the header and read the document each step approves", async ({
  browser,
}) => {
  test.setTimeout(150_000);
  await phase(browser, seedHeader, async (page) => {
    // Two projects of the row's own (`rowKey`, e2e/test.ts): one that gates three steps and one
    // that gates none.
    const gated = rowKey("GATED");
    const plain = rowKey("PLAIN");
    await createProject({ key: gated, name: "Gated" });
    await setGatedSteps(gated, ["decisions", "criteria", "spec"]);
    await createProject({ key: plain, name: "Plain" });

    // A root of a project that gates steps: one entry per step, in the project's order, each
    // reading its own document.
    const root = await createIssue({ project: gated, spec: "The plan.", title: "Gated root" });
    const decisions = await createIssueArtifact(root.key, {
      content: "# Decisions\n\nHTTP it is.\n",
      name: "decisions.md",
    });
    await reviewArtifact(decisions.artifact.id, { state: "approved" });
    await requestApproval(root.primary_artifact_id ?? "", { summary: "Read it." });
    await gotoIssue(page, `/issues/${root.key}`);
    await expectHeaderDrawn(page);
    const steps = issueHeader(page).getByTestId("issue-approval-steps");
    await expect
      .poll(async () =>
        steps
          .locator("[data-approval-step]")
          .evaluateAll((entries) =>
            entries.map((entry) => entry.getAttribute("data-approval-step"))
          )
      )
      .toEqual(["decisions", "criteria", "spec"]);
    await expect(
      approvalEntry(page, "decisions").getByRole("button", { name: "Approved v1" })
    ).toBeVisible();
    await expect(approvalEntry(page, "criteria")).toContainText(CONTROL_LABELS.not_requested);
    await expect(
      approvalEntry(page, "spec").getByRole("button", { name: "Awaiting approval" })
    ).toBeVisible();
    // The approval the request asks for pins a version of the spec, and that version is the
    // spec's own text. The version is the request's own: the document's approval names one only
    // once someone has reviewed it, and nobody has reviewed this spec.
    const awaiting = await getArtifact(root.primary_artifact_id ?? "");
    const pinned = (await getAsk(awaiting.approval?.ask_id ?? "")).ask.approval?.version ?? 0;
    expect(pinned, "the approval pins no version of the spec").toBeGreaterThan(0);
    expect(
      (await getArtifactVersion(root.primary_artifact_id ?? "", pinned)).markdown,
      "the document version the approval pins does not resolve to the spec's own text"
    ).toContain("The plan.");
    // The request waits on the reader, so the spec's entry carries Approve in the header,
    // and the approval it records reads back.
    await approvalEntry(page, "spec").getByRole("button", { exact: true, name: "Approve" }).click();
    await expect
      .poll(async () => (await getArtifact(root.primary_artifact_id ?? "")).approval?.state)
      .toBe("approved");
    await expect(
      approvalEntry(page, "spec").getByRole("button", { exact: true, name: "Approve" })
    ).toHaveCount(0);
    await expect(
      issueHeader(page).getByRole("button", { exact: true, name: "Request changes" }),
      "the header draws Request changes with no request waiting"
    ).toHaveCount(0);

    // A request of the Verification document waits on the reader: its own entry carries Approve
    // and Request changes, and the spec's, whose request was answered, carries neither. The
    // count's press puts the focus on the entry of the document that waits, and its Approve
    // answers that document's request.
    const verification = await createIssueArtifact(root.key, {
      content: "# Verification\n\nOne check.\n",
      name: "verification.md",
    });
    await requestApproval(verification.artifact.id, { summary: "Read it." });
    await gotoIssue(page, `/issues/${root.key}`);
    await expectHeaderDrawn(page);
    for (const action of ["Approve", "Request changes"]) {
      await expect(
        approvalEntry(page, "criteria").getByRole("button", { exact: true, name: action }),
        "the Verification document's entry carries no Approve while its request waits"
      ).toBeVisible();
    }
    await expect(
      reviewActions(approvalEntry(page, "spec")),
      "the spec's entry carries actions though no request of it waits"
    ).toHaveCount(0);
    await expect(waitingCount(page)).toHaveText("Waiting on you (1)");
    await waitingCount(page).click();
    await expect
      .poll(() =>
        page.evaluate(() => document.activeElement?.getAttribute("data-approval-artifact"))
      )
      .toBe(verification.artifact.id);
    await approvalEntry(page, "criteria")
      .getByRole("button", { exact: true, name: "Approve" })
      .click();
    await expect
      .poll(async () => (await getArtifact(verification.artifact.id)).approval?.state)
      .toBe("approved");
    await expect(reviewActions(issueHeader(page))).toHaveCount(0);

    // The page has read the projects, since it draws the gated root's steps from them, and the
    // issues it opens next it opens within the app, as a link does: so each draws what its gate
    // makes of it, not what a read of the projects still pending would. A root of a project that
    // gates nothing carries the one chip, "Not requested".
    await expect(steps.locator("[data-approval-step]")).toHaveCount(3);
    const openInApp = async (key: string) => {
      await navigateInApp(page, `/issues/${key}`);
      await expect(
        identityBand(page).getByText(key, { exact: true }),
        `the page did not open ${key}`
      ).toBeVisible();
    };
    const plainRoot = await createIssue({ project: plain, spec: "The plan.", title: "Plain root" });
    await openInApp(plainRoot.key);
    await expectHeaderDrawn(page);
    await expect(issueHeader(page).getByTestId("issue-approval-steps")).toHaveCount(0);
    await expect(issueHeader(page)).toContainText(CONTROL_LABELS.not_requested);

    // Every child carries the one chip, and no list of steps: the child of the root that gates
    // steps, opened within the app with the projects read, the child of a parent not yet
    // approved, of a parent whose spec changed since its approval, and of an approved parent.
    const oneChip = async (
      key: string,
      open = (opened: string) => gotoIssue(page, `/issues/${opened}`)
    ) => {
      await open(key);
      await expectHeaderDrawn(page);
      await expect(issueHeader(page).getByTestId("issue-approval-steps")).toHaveCount(0);
      await expect(
        issueHeader(page).getByText(CONTROL_LABELS.not_requested, { exact: true }),
        `the child's header draws no "${CONTROL_LABELS.not_requested}"`
      ).toHaveCount(1);
    };
    const gatedChild = await createIssue({
      force: true,
      parent: root.key,
      project: gated,
      spec: "The child's plan.",
      title: "Child of the gated root",
    });
    await oneChip(gatedChild.key, openInApp);
    const childOf = (parent: string, title: string) =>
      createIssue({ parent, project: plain, spec: "The child's plan.", title });
    const unapproved = await createIssue({
      project: plain,
      spec: "Not yet.",
      title: "Unapproved",
    });
    await oneChip((await childOf(unapproved.key, "Child of an unapproved parent")).key);
    const changed = await createIssue({
      project: plain,
      spec: "Before.",
      title: "Changed since",
    });
    await reviewArtifact(changed.primary_artifact_id ?? "", { state: "approved" });
    await editArtifact(changed.primary_artifact_id ?? "", {
      ops: [{ find: "Before.", op: "replace", with: "After." }],
    });
    expect((await getArtifact(changed.primary_artifact_id ?? "")).approval?.state).toBe("stale");
    await oneChip((await childOf(changed.key, "Child of a changed parent")).key);
    await requestApproval(plainRoot.primary_artifact_id ?? "", { summary: "Read it." });
    await reviewArtifact(plainRoot.primary_artifact_id ?? "", { state: "approved" });
    const child = await childOf(plainRoot.key, "Child of an approved parent");
    await oneChip(child.key);

    // A child with an approval request open answers it from its chip in the header.
    await requestApproval(child.primary_artifact_id ?? "", { summary: "Read it." });
    await gotoIssue(page, `/issues/${child.key}`);
    await expectHeaderDrawn(page);
    await issueHeader(page).getByRole("button", { exact: true, name: "Approve" }).click();
    await expect
      .poll(async () => (await getArtifact(child.primary_artifact_id ?? "")).approval?.state)
      .toBe("approved");

    // The plain root's spec approved, written again, and sent back with no request open: nothing
    // waits on the reader, so the header draws neither action.
    await editArtifact(plainRoot.primary_artifact_id ?? "", {
      ops: [{ find: "The plan.", op: "replace", with: "The plan, written again." }],
    });
    await reviewArtifact(plainRoot.primary_artifact_id ?? "", {
      reason: "Name the rollback path.",
      state: "changes_requested",
    });
    await gotoIssue(page, `/issues/${plainRoot.key}`);
    await expectHeaderDrawn(page);
    await expect(
      issueHeader(page).getByRole("button", { name: "Changes requested" }),
      "the plain root's spec does not read its changes request"
    ).toBeVisible();
    await expect(
      reviewActions(issueHeader(page)),
      "the header draws an action with no request waiting"
    ).toHaveCount(0);
  });
});

// Row (3): the header holds its limits in the typeface and, with every request for the
// typeface's file refused, in the fallback. What the file holds and its licence are not the
// header's, and the row reads neither. On the phone it reads what the 390 px viewport decides
// in the fallback, as row (1) reads it in the face, a layout claim "Phone acceptance" in
// `packages/dispatch/AGENTS.md` tags `@phone` for: the header's height limit and the 24 px
// targets at 390 px, and the page no wider than the window at 390 and 320 px.
// SLOW(>=2s): 14,270 ms on chromium and 13,365 ms on iphone (one worker, Job e2e-rows-rdf2g at
// 43949bc0): thirteen loads of the issue in two signed-in contexts, each waiting for the
// typeface to load or fail, and the header's limits measured at twelve of them.
test("the header holds its limits when the typeface does not load @phone", async ({ browser }) => {
  test.setTimeout(240_000);
  await phase(browser, seedHeader, async (page, seeded) => {
    await patchIssue(seeded.key, { labels: [...TWENTY_LABELS], title: PHONE_TITLE });

    // The page is drawn in the typeface.
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectHeaderDrawn(page);
    await expect
      .poll(() => typefaceStatuses(page), {
        message: `the document's font set holds no ${TYPEFACE} face that loaded`,
      })
      .toEqual(["loaded"]);
    expect(
      (await headerFamilies(page))[0],
      `the header's text is not drawn in ${TYPEFACE} first`
    ).toBe(TYPEFACE);
    const address = await typefaceAddress(page);

    // With no request waiting and then one, the header holds its limits in the typeface at
    // 1440 px and 390 px; and with every request for its file refused, in the fallback, where
    // the document is also no wider than the window at four widths.
    const blocked = await asUser(browser, "alice");
    try {
      await withIssuePage(blocked, "alice");
      let refused = 0;
      await blocked.route(address, async (route) => {
        refused += 1;
        await route.abort();
      });
      const without = await blocked.newPage();
      for (const requestWaiting of [false, true]) {
        if (requestWaiting) {
          await requestApproval(seeded.specArtifactId, { summary: "Read it back." });
        }
        for (const width of [DESKTOP.width, IPHONE_13.width]) {
          await page.setViewportSize(viewportAt(width));
          await gotoIssue(page, `/issues/${seeded.key}`);
          await expectHeaderDrawn(page);
          await expect(issueHeader(page)).toContainText(PHONE_TITLE);
          await expect
            .poll(() => typefaceStatuses(page), {
              message: `the ${TYPEFACE} face did not load at ${width} px`,
            })
            .toEqual(["loaded"]);
          await expectHeaderWithinItsLimit(page, width, requestWaiting, TYPEFACE);
        }
        for (const width of [DESKTOP.width, 1024, IPHONE_13.width, 320]) {
          await without.setViewportSize(viewportAt(width));
          await gotoIssue(without, `/issues/${seeded.key}`);
          await expectHeaderDrawn(without);
          await expect(issueHeader(without)).toContainText(PHONE_TITLE);
          await expect
            .poll(() => typefaceStatuses(without), {
              message: `the ${TYPEFACE} face did not fail at ${width} px, so this is not the fallback`,
            })
            .toEqual(["error"]);
          await expectHeaderWithinItsLimit(without, width, requestWaiting, "the fallback");
          if (requestWaiting && (width === DESKTOP.width || width === IPHONE_13.width)) {
            expect(
              await undersizedTargets(without),
              `a header control fails WCAG 2.5.8 at ${width} px in the fallback`
            ).toEqual([]);
            expect(
              await shortControls(without, seeded.key),
              `a control held to 24 px is shorter at ${width} px in the fallback`
            ).toEqual([]);
          }
          const fit = await without.evaluate(() => ({
            clientWidth: document.documentElement.clientWidth,
            scrollWidth: document.documentElement.scrollWidth,
          }));
          expect
            .soft(
              fit.scrollWidth,
              `the page scrolls sideways at ${width} px without the typeface: ${fit.scrollWidth} px of page in ${fit.clientWidth} px`
            )
            .toBe(fit.clientWidth);
        }
      }
      expect(refused, "no request for the typeface reached the refusal").toBeGreaterThan(0);
    } finally {
      await blocked.close();
    }
  });
});

// Row (4): the count of what waits on the reader opens the first thing that waits. The node the
// press reaches is drawn outside the header, and other rows read it. On the phone every press
// of the count is a tap of the touch screen, a touch claim "Phone acceptance" in
// `packages/dispatch/AGENTS.md` tags `@phone` for, and each opens what the mouse's click opens on
// `chromium`.
// SLOW(>=2s): 4,370 ms on chromium and 3,416 ms on iphone (one worker, Job e2e-rows-rdf2g at
// 43949bc0): three more issues written with their requests and documents, five loads on
// chromium and four on iphone, and the count pressed twice and Back pressed at each width.
test("the count of what waits is a control that opens the Decisions address @phone", async ({
  browser,
}) => {
  test.setTimeout(150_000);
  await phase(browser, seedHeader, async (page, seeded) => {
    const desktopProject = test.info().project.name !== "iphone";
    // The pointer the project presses with: the mouse on `chromium`, the touch screen on `iphone`.
    const pointer: Press = desktopProject ? "click" : "tap";
    for (const [width, how] of (desktopProject
      ? [
          [DESKTOP.width, "click"],
          [IPHONE_13.width, "Enter"],
        ]
      : [[IPHONE_13.width, "tap"]]) as readonly (readonly [number, Press])[]) {
      await page.setViewportSize(viewportAt(width));
      await gotoIssue(page, `/issues/${seeded.key}`);
      await expectHeaderDrawn(page);
      // Two questions wait on alice, and the count is a button.
      await expect(waitingCount(page)).toHaveText("Waiting on you (2)");
      expect(await waitingCount(page).evaluate((count) => count.tagName)).toBe("BUTTON");

      await press(waitingCount(page), how);
      await expect(page).toHaveURL(decisionsAddress(seeded.key));
      const first = await waitingPressInHistory(page);
      expect(first, `the press left no mark in the history entry at ${width} px`).not.toBeNull();
      const length = await page.evaluate(() => history.length);

      // A press made at that address replaces its entry, with a new mark: a number, and not the
      // first press's.
      await press(waitingCount(page), how);
      await expect
        .poll(
          async () => {
            const now = await waitingPressInHistory(page);
            return now !== null && now !== first;
          },
          { message: `a second press left the first press's mark at ${width} px` }
        )
        .toBe(true);
      expect(
        await page.evaluate(() => history.length),
        `a second press added a history entry at ${width} px`
      ).toBe(length);

      // Back leaves the address.
      await page.goBack();
      await expect(page).toHaveURL(new RegExp(`/issues/${seeded.key}$`));
    }

    // An approval request is all that waits: the count puts the focus on the approval control.
    const lone = await createIssue({
      force: true,
      project: seeded.project,
      spec: "Plan.",
      title: "Lone",
    });
    await requestApproval(lone.primary_artifact_id ?? "", { summary: "Read it." });
    await page.setViewportSize(viewportAt(DESKTOP.width));
    await gotoIssue(page, `/issues/${lone.key}`);
    await expectHeaderDrawn(page);
    await expect(waitingCount(page)).toHaveText("Waiting on you (1)");
    await press(waitingCount(page), pointer);
    await expect
      .poll(() =>
        page.evaluate(() => document.activeElement?.hasAttribute("data-approval-artifact"))
      )
      .toBe(true);
    expect(
      await page.evaluate(() => document.activeElement?.closest("[data-issue-header]") !== null),
      "the focus is on an approval control outside the header"
    ).toBe(true);

    // The spec approved and the Verification document's request alone waiting: the header draws
    // the spec's chip and no chip of the Verification document, so the press puts the focus on
    // no chip rather than on the spec's. The press's handler moves the focus as it runs, so the
    // focus is read once the press is done.
    const verifying = await createIssue({
      force: true,
      project: seeded.project,
      spec: "Plan.",
      title: "Lone Verification document",
    });
    const spec = verifying.primary_artifact_id ?? "";
    await reviewArtifact(spec, { state: "approved" });
    const verification = await createIssueArtifact(verifying.key, {
      content: "# Verification\n\nOne check.\n",
      name: "verification.md",
    });
    await requestApproval(verification.artifact.id, { summary: "Read it." });
    await gotoIssue(page, `/issues/${verifying.key}`);
    await expectHeaderDrawn(page);
    await expect(waitingCount(page)).toHaveText("Waiting on you (1)");
    await expect(
      approvalChip(page, spec),
      "the header draws no chip of the approved spec"
    ).toBeVisible();
    await press(waitingCount(page), pointer);
    expect(
      [null, verification.artifact.id],
      "the count's press focused another document's chip"
    ).toContain(
      await page.evaluate(
        () => document.activeElement?.getAttribute("data-approval-artifact") ?? null
      )
    );

    // A decision on the review list is all that waits: the issue's decisions document holds an
    // agent's decision that names the requirement its spec holds, which the server reads as
    // waiting for review, and no ask of the issue is open. The count counts it, and its press
    // opens the Decisions address.
    const decided = await createIssue({
      force: true,
      project: seeded.project,
      spec: ":::requirement{#r1}\nThe header counts a decision that waits.\n:::\n",
      title: "Decided",
    });
    await createIssueArtifact(decided.key, {
      content: `## The count reads the decisions\n\nThe header counts what the Decisions filter lists for review. It shapes dispatch://${decided.key}/spec#b-r1.\n`,
      name: "decisions.md",
    });
    expect(
      ((await issueDecisions(decided.key)).decisions ?? []).map((decision) => ({
        names: (decision.links ?? []).map((link) => link.to.block_id),
        waiting: decision.waiting_review,
      })),
      "the server does not read the agent's decision as waiting for review"
    ).toEqual([{ names: ["r1"], waiting: true }]);
    expect((await getIssue(decided.key)).open_asks ?? [], "an ask of the issue is open").toEqual(
      []
    );
    await gotoIssue(page, `/issues/${decided.key}`);
    await expectHeaderDrawn(page);
    await expect(waitingCount(page), "a decision on the review list is not counted").toHaveText(
      "Waiting on you (1)"
    );
    await press(waitingCount(page), pointer);
    await expect(
      page,
      "the count's press with a decision waiting did not open the Decisions address"
    ).toHaveURL(decisionsAddress(decided.key));
  });
});

// Row (5): the count of what waits holds only what waits on the reader. A question waits on the
// person the server resolves it to - whoever it was handed to, else the issue's assignee.
// SLOW(>=2s): 5,175 ms (one worker, Job e2e-rows-rdf2g at 43949bc0): a second person signed in
// beside alice, six loads of the issue between the two, and four writes, two before the first
// load and two between loads - two of its assignee and two questions - three of them read back
// through the API.
test("the count of what waits holds only what waits on the reader", async ({ browser }) => {
  test.setTimeout(120_000);
  await phase(browser, seedHeader, async (alice, seeded) => {
    const issue = await createIssue({ force: true, project: seeded.project, title: "Whose turn" });
    const bobs = await asUser(browser, "bob");
    try {
      await withIssuePage(bobs, "bob");
      const bob = await bobs.newPage();
      await bob.setViewportSize(viewportAt(DESKTOP.width));
      await patchIssue(issue.key, { assignee: "alice" });
      const asked = await createAsk(issue.key, { question: "Which way does it go?" }, asAgent);
      expect(await waitingOnPersonOf(asked.id)).toBe("alice");

      // The agent's question waits on alice, who holds the issue: her count, not bob's. Each
      // absence is read once the header draws the assignee.
      await gotoIssue(alice, `/issues/${issue.key}`);
      await expectHeaderDrawn(alice);
      await expect(waitingCount(alice)).toHaveText("Waiting on you (1)");
      await gotoIssue(bob, `/issues/${issue.key}`);
      await expectHeaderDrawn(bob);
      await expect(badgeValue(bob, `Assignee of ${issue.key}`)).toHaveText("alice");
      await expect(waitingCount(bob), "a question waiting on alice counts for bob").toHaveCount(0);

      // Bob's own question waits on alice too: it raises her count and not his.
      const bobsQuestion = await createAsk(
        issue.key,
        { question: "And who decides it?" },
        { as: "user", login: "bob" }
      );
      expect(await waitingOnPersonOf(bobsQuestion.id)).toBe("alice");
      await gotoIssue(bob, `/issues/${issue.key}`);
      await expectHeaderDrawn(bob);
      await expect(badgeValue(bob, `Assignee of ${issue.key}`)).toHaveText("alice");
      await expect(waitingCount(bob), "bob's own question raised bob's count").toHaveCount(0);
      await gotoIssue(alice, `/issues/${issue.key}`);
      await expect(waitingCount(alice)).toHaveText("Waiting on you (2)");

      // The issue handed to bob: both questions wait on him now, and none on alice.
      await patchIssue(issue.key, { assignee: "bob" });
      expect(await waitingOnPersonOf(asked.id)).toBe("bob");
      await gotoIssue(bob, `/issues/${issue.key}`);
      await expect(waitingCount(bob)).toHaveText("Waiting on you (2)");
      await gotoIssue(alice, `/issues/${issue.key}`);
      await expectHeaderDrawn(alice);
      await expect(badgeValue(alice, `Assignee of ${issue.key}`)).toHaveText("bob");
      await expect(
        waitingCount(alice),
        "the count stayed with alice after the issue went to bob"
      ).toHaveCount(0);
    } finally {
      await bobs.close();
    }
  });
});

// Row (6): a press on the status straight out of the title's editor lands on the status.
// SLOW(>=2s): 2,814 ms (one worker, Job e2e-rows-rdf2g at 43949bc0): three typed edits of the
// title and two picks of the status, each saved through Dispatch and four of them polled there,
// a pick made behind a title write a route holds, and 500 ms watched for a second write of it.
test("a press on the status straight out of the title's editor lands on the status", async ({
  browser,
}) => {
  await phase(browser, seedHeader, async (page, seeded) => {
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectHeaderDrawn(page);
    const title = issueHeader(page).getByRole("heading", { level: 1 });
    const editor = issueHeader(page).getByRole("textbox", { name: "Issue title" });
    const status = issueHeader(page).getByRole("combobox", { name: "Status" });

    // The control: with nothing edited, the press out of the editor focuses the status.
    await title.click();
    await expect(editor).toBeFocused();
    await status.click();
    await expect(status, "the status took no focus with nothing edited").toBeFocused();
    await page.keyboard.press("Escape");
    await page.keyboard.press("Escape");

    // The same press with the title edited, and no Enter: the title saves and the press lands.
    await title.click();
    await expect(editor).toBeFocused();
    await editor.pressSequentially("edited ");
    await status.click();
    await expect(status, "the press out of an edited title landed on nothing").toBeFocused();
    await expect
      .poll(async () => (await getIssue(seeded.key)).title, {
        message: "the title the reader left was not saved",
      })
      .toContain("edited");
    await status.selectOption("todo");
    await expect
      .poll(async () => (await getIssue(seeded.key)).status, {
        message: "the status picked straight after the title was not written",
      })
      .toBe("todo");

    // A status picked while the title's write is still out waits behind it in the issue's line:
    // the select draws the pick at once, and it is written once, after the title.
    // The issue is at `todo` already, so the pick is `testing`, which the select reads only if
    // the pick was drawn.
    let statusWrites = 0;
    const countStatusWrite = (request: Request) => {
      if (writesField(request, seeded.key, "status")) statusWrites += 1;
    };
    page.on("request", countStatusWrite);
    const held = `**/api/v1/issues/${seeded.key}`;
    const release = Promise.withResolvers<void>();
    let holding = false;
    await page.route(held, async (route) => {
      if (holding || !writesField(route.request(), seeded.key, "title")) {
        await route.fallback();
        return;
      }
      holding = true;
      await release.promise;
      await route.continue();
    });
    await title.click();
    await expect(editor).toBeFocused();
    await editor.pressSequentially("held ");
    await status.click();
    await expect.poll(() => holding, { message: "the title's write was not sent" }).toBe(true);
    await status.selectOption("testing");
    await expect(status, "the status picked while the title saved was not drawn").toHaveValue(
      "testing"
    );
    release.resolve();
    await expect
      .poll(async () => (await getIssue(seeded.key)).status, {
        message: "the status picked while the title saved was not written once",
      })
      .toBe("testing");
    expect(statusWrites, "the status picked while the title saved was not written once").toBe(1);
    await expect(
      issueHeader(page).getByRole("status"),
      "the status's write did not settle"
    ).toHaveCount(0);
    // A second write of the pick would be sent as the first one settles, so 500 ms past that is
    // many frames past where a late duplicate would show.
    await page.waitForTimeout(500);
    expect(statusWrites, "the status picked while the title saved was not written once").toBe(1);
    page.off("request", countStatusWrite);
    await page.unroute(held);

    // Escape and Enter leave the title's editor with the focus on the heading it opened from,
    // which does not open the editor again; Enter saves what was typed.
    for (const leave of ["Escape", "Enter"] as const) {
      await page.keyboard.press("Escape");
      await title.click();
      await expect(editor).toBeFocused();
      if (leave === "Enter") await editor.pressSequentially("entered ");
      await page.keyboard.press(leave);
      await expect(editor, `${leave} left the title's editor open`).toHaveCount(0);
      await expect(title, `${leave} dropped the focus out of the header`).toBeFocused();
    }
    await expect
      .poll(async () => (await getIssue(seeded.key)).title, {
        message: "Enter did not save the title",
      })
      .toContain("entered");
  });
});

// Row (7): ending the edit of a title of two or three lines must not move the control under it
// between the mousedown and the mouseup, as `e2e/issue.e2e.ts:1016` at 750e2716 ("issue header
// delivers the click that ends a title edit to the control under the pointer @phone") reads. On the
// phone the press is a tap of the touch screen, a touch claim "Phone acceptance" in
// `packages/dispatch/AGENTS.md` tags `@phone` for. A tap's mouse events come after its touch
// events: the order `IssueTitleRow.tsx` holds the title's height by until the press is
// delivered, at 390 px.
// SLOW(>=2s): 10,227 ms on chromium and 4,970 ms on iphone (one worker, Job e2e-rows-rdf2g at
// 43949bc0): at each width, for a title of two lines and of three, three loads of the issue and
// a title built a word at a time, each word measured in the page: twelve loads on chromium and
// six on iphone.
test("the press that ends a title edit reaches the control under it @phone", async ({
  browser,
}) => {
  test.setTimeout(150_000);
  await phase(browser, seedHeader, async (page, seeded) => {
    const tap = test.info().project.name === "iphone";
    const widths = tap ? [IPHONE_13.width] : [DESKTOP.width, IPHONE_13.width];
    for (const width of widths) {
      await page.setViewportSize(viewportAt(width));
      for (const lines of [2, 3]) {
        await gotoIssue(page, `/issues/${seeded.key}`);
        await expectHeaderDrawn(page);
        const title = await titleOfLines(page, lines);
        await patchIssue(seeded.key, { title });
        await gotoIssue(page, `/issues/${seeded.key}`);
        await expectHeaderDrawn(page);
        const heading = issueHeader(page).getByRole("heading", { level: 1 });
        await expect(heading).toHaveText(title);
        await heading.click();
        const editor = issueHeader(page).getByRole("textbox", { name: "Issue title" });
        await expect(editor).toBeFocused();
        // The editor draws the whole title, as many lines as it takes. Read softly, so a run
        // whose editor is one line still reads the press below.
        const drawnLines = await editor.evaluate((box) =>
          Math.round(
            (box.getBoundingClientRect().height - 2) /
              Number.parseFloat(getComputedStyle(box).lineHeight)
          )
        );
        expect
          .soft(drawnLines, `the editor draws the ${lines}-line title in another number of lines`)
          .toBe(lines);
        // A real press on the count under the title, straight out of the edit.
        await pressOutOfTheEdit(page, waitingCount(page), "the count", tap);
        await expect(
          page,
          `the press out of a ${lines}-line title's edit at ${width} px landed on nothing`
        ).toHaveURL(decisionsAddress(seeded.key));

        // The same press on the status, straight out of an edit of the same title, lands too,
        // and the status takes the focus.
        await gotoIssue(page, `/issues/${seeded.key}`);
        await expectHeaderDrawn(page);
        await heading.click();
        await expect(editor).toBeFocused();
        const status = issueHeader(page).getByRole("combobox", { name: "Status" });
        await pressOutOfTheEdit(page, status, "the status", tap);
        await expect(
          status,
          `the press out of a ${lines}-line title's edit at ${width} px did not reach the status`
        ).toBeFocused();
        await page.keyboard.press("Escape");

        // An edit the keyboard opens and ends after the one that press ended: the press is
        // over, so nothing holds the heading at the editor's height, and at 1440 px the title
        // of three lines is clamped to two again. The edit is opened with Enter on the focused
        // heading, which sends no click: `e` opens it by clicking the heading, and that click
        // would end a press still under way before the edit opened.
        if (width === DESKTOP.width && lines === 3) {
          await heading.focus();
          await expect(heading).toBeFocused();
          await page.keyboard.press("Enter");
          await expect(editor).toBeFocused();
          await page.keyboard.press("Enter");
          await expect(editor).toHaveCount(0);
          await expect
            .poll(() => heading.evaluate((title) => getComputedStyle(title).webkitLineClamp), {
              message:
                "the title stayed unclamped after a keyboard edit that followed a pressed one",
            })
            .toBe("2");
        }
      }
    }
  });
});

// A press the browser turns into a drag, as it does a press on words the reader selected in the
// title's editor, sends no mouseup and no click: Chromium ends it with a pointercancel as the drag
// starts and a dragend as the drop lands. The title row holds the heading at the editor's height
// while a press that ended the edit is under way, so it must read that press as over by the
// drag's end (`hearPresses` ends it at the pointercancel, as the drag starts); else the next edit
// a key ends finds the press under way, and a title of three lines stays drawn whole at 1440 px
// instead of clamped to two. A drag of selected words is a press of the mouse, so the row runs at
// the desktop width and on no phone.
// SLOW(>=2s): 2,221 ms (one worker, Job e2e-rows-rdf2g at 43949bc0): two loads, a three-line
// title built a word at a time with a measurement in the page per word, and a drag of the
// selected words in eight steps, polled until its dragend and the title's clamp.
test("the title is clamped again after a key ends an edit whose words the reader dragged", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  await phase(browser, seedHeader, async (page, seeded) => {
    await page.setViewportSize(viewportAt(DESKTOP.width));
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectHeaderDrawn(page);
    const title = await titleOfLines(page, 3);
    await patchIssue(seeded.key, { title });
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectHeaderDrawn(page);
    const heading = issueHeader(page).getByRole("heading", { level: 1 });
    const editor = issueHeader(page).getByRole("textbox", { name: "Issue title" });
    await expect(heading).toHaveText(title);
    await heading.click();
    await expect(editor).toBeFocused();

    // Every word of the title is selected, and the middle of the editor is on the second of its
    // three lines, which words fill: a press there is a press on selected words, which the
    // browser drags once the pointer moves.
    await editor.selectText();
    // The editor is focused above, so it is drawn here; a null box only throws.
    const box = await editor.boundingBox();
    if (box === null) throw new Error("the title's editor is not drawn while it is focused");
    const from = { x: box.x + box.width / 2, y: box.y + box.height / 2 };
    // What the page hears of the press, which says whether the mouse made a native drag.
    const heard = await page.evaluateHandle(() => {
      const events: string[] = [];
      for (const type of ["dragstart", "pointercancel", "dragend", "mouseup", "click"]) {
        window.addEventListener(type, () => events.push(type), true);
      }
      return events;
    });
    // The drop lands on the page's column under the header, where no field takes the words.
    await page.mouse.move(from.x, from.y);
    await page.mouse.down();
    await page.mouse.move(from.x, from.y + 240, { steps: 8 });
    await page.mouse.up();
    await expect
      .poll(() => heard.jsonValue(), { message: "the press on the selected words made no drag" })
      .toContain("dragend");
    // After its dragend the drag sends nothing more, so one read holds every event of its press.
    const events = await heard.jsonValue();
    for (const release of ["mouseup", "click"]) {
      expect(events, `the drag of the selected words sent its press's ${release}`).not.toContain(
        release
      );
    }

    // The drag leaves the focus in the editor, so the key ends the edit the drag was made in.
    await expect(editor, "the drag of the title's words moved the focus").toBeFocused();
    await page.keyboard.press("Enter");
    await expect(editor).toHaveCount(0);
    await expect(heading, "the drop of the title's words changed the title").toHaveText(title);
    await expect
      .poll(() => heading.evaluate((element) => getComputedStyle(element).webkitLineClamp), {
        message: "the title stayed unclamped after a key ended an edit whose words were dragged",
      })
      .toBe("2");
  });
});

// Row (8): the header's keys and the palette's rows reach the header's controls: the header's
// steps of the rows of `e2e/keyboard-issue.e2e.ts` and `e2e/keyboard-palette.e2e.ts`.
// SLOW(>=2s): 3,910 ms (one worker, Job e2e-rows-rdf2g at 43949bc0): three loads, seven writes
// of the issue and its pin - two refused by a route and one held by one - read back through the
// API, title edits, Details, and the palette opened four times.
test("the header's keys and the palette reach their controls", async ({ browser }) => {
  test.setTimeout(150_000);
  await phase(browser, seedHeader, async (page, seeded) => {
    const key = seeded.key;
    await page.setViewportSize({ height: 900, width: 1280 });
    await gotoIssue(page, `/issues/${key}`);
    await expectHeaderDrawn(page);
    await leaveFocus(page);
    const header = issueHeader(page);
    const priority = header.getByRole("combobox", { name: `Priority of ${key}` });
    const status = header.getByRole("combobox", { name: "Status" });
    const patched: string[] = [];
    page.on("request", (request) => {
      if (request.method() === "PATCH") patched.push(new URL(request.url()).pathname);
    });

    // p and s reach their own control, and Escape gives the focus back to the page.
    await page.keyboard.press("p");
    await expect(priority, "p did not put the focus on the priority").toBeFocused();
    await page.keyboard.press("Escape");
    await expect.poll(() => onBody(page)).toBe(true);
    await page.keyboard.press("s");
    await expect(status, "s did not put the focus on the status").toBeFocused();
    await page.keyboard.press("Escape");
    await expect.poll(() => onBody(page)).toBe(true);
    expect(patched, "a key that focuses a control wrote the issue").toEqual([]);

    // A refused write: the digit and the picker are one write, so the refusal is said once, in
    // the header, with Retry, and the issue keeps its priority.
    const refused = `**/api/v1/issues/${key}`;
    await page.route(refused, async (route) => {
      if (route.request().method() !== "PATCH") {
        await route.continue();
        return;
      }
      await route.fulfill({
        body: JSON.stringify({ code: "ISSUE_INPUT", error: "Dispatch refused the priority" }),
        contentType: "application/json",
        status: 400,
      });
    });
    await page.keyboard.press("2");
    const failure = header
      .getByRole("alert")
      .filter({ hasText: `Could not update the priority of ${key}.` });
    await expect(failure).toHaveCount(1);
    await expect(failure.getByRole("button", { name: "Retry" })).toBeVisible();
    // The picker's write is refused too, and only then is the alert counted: the one the digit
    // drew is on the screen before the picker's write has been sent.
    const pickerRefused = page.waitForResponse(
      (response) =>
        response.request().method() === "PATCH" &&
        new URL(response.url()).pathname === `/api/v1/issues/${key}` &&
        response.status() === 400
    );
    await priority.selectOption("3");
    await pickerRefused;
    await expect(failure).toHaveCount(1);
    await expect(failure.getByRole("button", { name: "Retry" })).toBeVisible();
    await expect.poll(() => getIssue(key)).toMatchObject({ priority: 1 });
    await page.unroute(refused);
    await gotoIssue(page, `/issues/${key}`);
    await expectHeaderDrawn(page);
    await leaveFocus(page);

    // A digit sets the priority through the picker's write, and Shift+P pins through the
    // header's own control; both hold after a new load.
    const write = page.waitForRequest(
      (request) =>
        request.method() === "PATCH" && new URL(request.url()).pathname === `/api/v1/issues/${key}`
    );
    await page.keyboard.press("2");
    expect((await write).postDataJSON()).toEqual({ priority: 2 });
    await expect(badgeValue(page, `Priority of ${key}`)).toHaveText("P2");
    await expect.poll(() => getIssue(key)).toMatchObject({ priority: 2 });
    await leaveFocus(page);
    const pinned = page.waitForResponse(
      (response) =>
        response.request().method() === "PUT" &&
        new URL(response.url()).pathname === `/api/v1/me/issues/${key}/state`
    );
    await page.keyboard.press("Shift+P");
    await expect(header.locator('[data-issue-action="pin"]')).toHaveAttribute(
      "aria-pressed",
      "true"
    );
    expect((await pinned).ok()).toBe(true);
    await gotoIssue(page, `/issues/${key}`);
    await expectHeaderDrawn(page);
    await expect(badgeValue(page, `Priority of ${key}`)).toHaveText("P2");
    await expect(header.locator('[data-issue-action="pin"]')).toHaveAttribute(
      "aria-pressed",
      "true"
    );
    await leaveFocus(page);

    // e puts the caret in the title, where every key is text: "sp0" is typed there, and Escape
    // keeps the title.
    await page.keyboard.press("e");
    const editor = header.getByRole("textbox", { name: "Issue title" });
    await expect(editor, "e did not put the caret in the title").toBeFocused();
    await page.keyboard.type("sp0");
    await expect(editor).toHaveValue(/sp0/);
    await page.keyboard.press("Escape");
    await expect(header.getByRole("heading", { level: 1 })).toHaveText("The header's issue");
    await expect
      .poll(() => getIssue(key))
      .toMatchObject({ priority: 2, title: "The header's issue" });

    // Escape and Enter leave the focus on the heading, and `e` there opens the editor again.
    const heading = header.getByRole("heading", { level: 1 });
    for (const leave of ["Escape", "Enter"] as const) {
      await expect(heading, `${leave} dropped the focus out of the header`).toBeFocused();
      await page.keyboard.press("e");
      await expect(
        editor,
        "e did not open the title's editor from the focused heading"
      ).toBeFocused();
      await page.keyboard.press("Enter");
      await expect(editor, "Enter left the title's editor open").toHaveCount(0);
    }

    // A focus the page gives back to the heading opens no editor: Details, opened from its
    // control while the heading holds the focus, gives the focus back to the heading as Escape
    // closes it, and a key typed then is no part of the title.
    await expect(heading, "Enter dropped the focus out of the header").toBeFocused();
    await header.getByTestId("issue-details-control").click();
    await expect(details(page), "Details' control opened no Details").toBeVisible();
    await page.keyboard.press("Escape");
    await expect(details(page), "Escape left Details open").toBeHidden();
    // The drawer hands the focus back in the frames after it closes, as `closeDetails` waits
    // them out, so the editor is looked for once they are drawn.
    await page.evaluate(() => {
      const { promise, resolve } = Promise.withResolvers<void>();
      requestAnimationFrame(() => requestAnimationFrame(() => resolve()));
      return promise;
    });
    await expect(editor, "a focus given back to the heading opened the title's editor").toHaveCount(
      0
    );
    await expect(heading, "Details did not give the focus back to the heading").toBeFocused();
    await page.keyboard.press("x");
    await leaveFocus(page);
    expect(
      (await getIssue(key)).title,
      "a focus given back to the heading opened the title's editor"
    ).toBe("The header's issue");

    // The palette offers Close issue and Set priority P2, and not Reopen issue.
    await page.keyboard.press("Control+k");
    const actions = page.getByRole("dialog", { name: "Search" }).getByRole("group", {
      name: "Actions",
    });
    await expect(actions.getByRole("option", { name: "Close issue" })).toBeVisible();
    await expect(actions.getByRole("option", { name: "Set priority P2" })).toBeVisible();
    await expect(actions.getByRole("option", { name: "Reopen issue" })).toHaveCount(0);
    await page.keyboard.press("Escape");
    const palette = page.getByRole("dialog", { name: "Search" });
    await expect(palette, "Escape did not close the palette").toHaveCount(0);

    // A status picked while its write is held in flight leaves the priority taking input, and
    // the palette offers Set priority with it, as the issue's line sends that write after the
    // status's. Escape closes the palette before the status's write is let go.
    const held = `**/api/v1/issues/${key}`;
    const release = Promise.withResolvers<void>();
    let holding = 0;
    await page.route(held, async (route) => {
      if (route.request().method() === "PATCH") {
        holding += 1;
        await release.promise;
      }
      await route.continue();
    });
    await status.selectOption("todo");
    await expect.poll(() => holding, { message: "the status's write was not sent" }).toBe(1);
    await expect(priority, "the priority refuses input while the status is saved").toBeEnabled();
    await leaveFocus(page);
    await page.keyboard.press("Control+k");
    await expect(
      actions.getByRole("option", { name: "Set priority P2" }),
      "the palette hides Set priority while the priority takes input"
    ).toBeVisible();
    await page.keyboard.press("Escape");
    await expect(palette, "Escape did not close the palette").toHaveCount(0);
    release.resolve();
    await expect.poll(() => getIssue(key).then((read) => read.status)).toBe("todo");
    await expect(header.getByRole("status"), "the status's write did not settle").toHaveCount(0);
    await page.unroute(held);

    // The palette's Close issue closes the issue, which then offers Reopen issue and neither
    // Close issue nor a priority, as the header offers Reopen and refuses a priority; Reopen
    // gives the issue back to the backlog. The rows are taken when the palette opens, so it
    // opens once the page has taken in the close and has no write in flight.
    await leaveFocus(page);
    await page.keyboard.press("Control+k");
    await actions.getByRole("option", { name: "Close issue" }).click();
    await expect(palette, "Close issue did not close the palette").toHaveCount(0);
    await expect.poll(() => getIssue(key).then((read) => read.status)).toBe("done");
    await expect(priority, "the closed issue's priority takes input").toBeDisabled();
    await expect(header.getByRole("status"), "the close is still being saved").toHaveCount(0);
    await leaveFocus(page);
    await page.keyboard.press("Control+k");
    const reopen = actions.getByRole("option", { name: "Reopen issue" });
    await expect(reopen, "the closed issue's palette offers no Reopen issue").toBeVisible();
    await expect(actions.getByRole("option", { name: "Close issue" })).toHaveCount(0);
    await expect(actions.getByRole("option", { name: "Set priority P2" })).toHaveCount(0);
    await reopen.click();
    await expect(palette, "Reopen issue did not close the palette").toHaveCount(0);
    await expect.poll(() => getIssue(key).then((read) => read.status)).toBe("backlog");
  });
});

/** Opens the issue `key` in a short window, `width` px wide and 420 px tall, and, once its header
 *  is drawn, makes the page 2000 px tall, taller than the window, so that the page scrolls, and
 *  scrolls it to its top. */
async function openScrollingPage(page: Page, key: string, width: number): Promise<void> {
  await page.setViewportSize({ height: 420, width });
  await gotoIssue(page, `/issues/${key}`);
  await expectHeaderDrawn(page);
  // The outline opens with the focus on the first question that waits on the reader and holds
  // that row on screen until the reader acts or the focus leaves it, so the page's
  // growth below would scroll back to it: the focus lands, then leaves the tree.
  await expect(page.locator("[data-node-row]:focus"), "the load focused no row").toHaveCount(1);
  await focusThePage(page);
  await page.evaluate(() => {
    const page = document.querySelector("[data-issue-page]");
    if (page instanceof HTMLElement) page.style.minHeight = "2000px";
    window.scrollTo(0, 0);
  });
}

// Row (9): the band, which holds the identity and the title, stays when the page scrolls. On
// the phone it reads what the 390 px viewport decides, a layout claim "Phone acceptance" in
// `packages/dispatch/AGENTS.md` tags `@phone` for: the title's 20 px type and its 362 px words
// with no clamp, and the whole header scrolling off the screen with the page, where the band
// stays from 761 px up.
// SLOW(>=2s): 3,328 ms on chromium and 1,642 ms on iphone (one worker, Job e2e-rows-rdf2g at
// 43949bc0): a load into a short window at each of four widths on chromium, each page scrolled
// 320 px and polled until the band reads its stuck state.
test("the band stays when the page scrolls, and on a phone the header goes @phone", async ({
  browser,
}) => {
  test.setTimeout(150_000);
  await phase(browser, seedHeader, async (page, seeded) => {
    const widths =
      test.info().project.name === "iphone"
        ? [IPHONE_13.width]
        : [DESKTOP.width, 761, 760, IPHONE_13.width];
    for (const width of widths) {
      const wide = width > 760;
      await openScrollingPage(page, seeded.key, width);

      // With the page at its top the band has no shadow, and the title is in its own type.
      const top = await stickyReading(page);
      if (top === null) throw new Error(`the page draws no header at ${width} px`);
      expect(top.condensed, `the band is stuck with the page at its top at ${width} px`).toBe(
        false
      );
      expect(top.shadow, `the band draws a shadow at the top at ${width} px`).toBe("none");
      const type = [top.title.weight, top.title.size, top.title.line, top.title.spacing];
      expect(type, `the title's type at ${width} px with the page at its top`).toEqual(
        wide
          ? ["600", "22px", "29px", `${(22 * -0.012).toFixed(3)}px`]
          : ["600", "20px", "26px", `${(20 * -0.012).toFixed(2)}px`]
      );
      if (width === DESKTOP.width) expect(top.title.clamp).toBe("2");
      if (width === IPHONE_13.width) {
        expect(top.title.clamp).toBe("none");
        expect(
          Math.abs(top.title.wordsWidth - 362),
          "the title's words are not 362 px wide"
        ).toBeLessThanOrEqual(1);
      }
      expect(
        top.properties.top,
        "the properties line is not under the title"
      ).toBeGreaterThanOrEqual(top.block.bottom - 1);

      await page.evaluate(() => window.scrollTo(0, 320));
      await expect
        .poll(async () => (await stickyReading(page))?.scrolled ?? 0, {
          message: `the page did not scroll at ${width} px`,
        })
        .toBeGreaterThan(300);
      await expect
        .poll(async () => (await stickyReading(page))?.condensed, {
          message: `the band's stuck state is wrong once scrolled at ${width} px`,
        })
        .toBe(wide);
      // The two polls above waited for the scroll and the band's stuck state, so this one reading
      // is of the settled page; null only throws.
      const scrolled = await stickyReading(page);
      if (scrolled === null) throw new Error("the header left the page as it scrolled");
      expect(
        [scrolled.title.weight, scrolled.title.size, scrolled.title.line, scrolled.title.spacing],
        `the title's type changed as the page scrolled at ${width} px`
      ).toEqual(type);
      if (wide) {
        expect(Math.abs(scrolled.block.top), "the band left the top").toBeLessThanOrEqual(1);
        expect(scrolled.block.height, "the band that stays is not 63 px").toBeGreaterThanOrEqual(
          62
        );
        expect(scrolled.block.height, "the band that stays is not 63 px").toBeLessThanOrEqual(64);
        expect(scrolled.shadow, "the stuck band draws no shadow").not.toBe("none");
        expect(
          scrolled.properties.bottom,
          "the properties line stayed on screen with the band"
        ).toBeLessThanOrEqual(scrolled.block.bottom);
        expect(scrolled.published, "the page publishes another height than the band's").toBe(
          Math.round(scrolled.block.height)
        );
        await expect(identityBand(page)).toBeInViewport();
      } else {
        expect(scrolled.block.bottom, "the header stayed on a phone").toBeLessThanOrEqual(0);
        expect(scrolled.properties.bottom).toBeLessThanOrEqual(0);
      }
    }
  });
});

/** The words `control` draws, read as a reader reads them off the screen: each glyph it draws,
 *  left to right, with a space wherever the glyphs stand more than 2 px apart and none wherever
 *  they touch. Its text content says nothing of that: a space the layout collapses is still in
 *  it, and a gap the layout opens is not. The control is one line. */
function drawnWords(control: Locator): Promise<string> {
  return control.evaluate((element) => {
    const glyphs: { char: string; left: number; right: number }[] = [];
    const range = document.createRange();
    const walker = document.createTreeWalker(element, NodeFilter.SHOW_TEXT);
    for (let node = walker.nextNode(); node !== null; node = walker.nextNode()) {
      const text = node.textContent ?? "";
      for (let at = 0; at < text.length; at += 1) {
        range.setStart(node, at);
        range.setEnd(node, at + 1);
        const box = range.getBoundingClientRect();
        if (box.width > 0) glyphs.push({ char: text[at] ?? "", left: box.left, right: box.right });
      }
    }
    glyphs.sort((a, b) => a.left - b.left);
    let words = "";
    let right = Number.NaN;
    for (const glyph of glyphs) {
      if (words !== "" && glyph.left - right > 2 && !words.endsWith(" ") && glyph.char !== " ") {
        words += " ";
      }
      words += glyph.char;
      right = glyph.right;
    }
    return words.replace(/\s+/g, " ").trim();
  });
}

// The labels' count reads "+1 label" or "+N labels" and the count of what waits reads
// "Waiting on you (N)", and a reader reads the words the screen draws, not the text the element
// holds. The labels' count once drew "+1label" (its space is a flex container's collapsed item)
// and the count of what waits drew "Waiting on you ( 2 )" (its flex gap opens between the
// brackets and the number), while every row's `toHaveText` read the right words in the text
// content. On the phone it reads the same at 390 px.
// SLOW(>=2s): 2,078 ms on chromium and 1,391 ms on iphone (one worker, Job e2e-rows-rdf2g at
// 43949bc0): a load of the issue with twenty labels at 1440 px and at 390 px, and at each both
// counts read twice, once as text and once glyph by glyph off the screen.
test("the header draws the words of its two counts as a reader reads them @phone", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  await phase(browser, seedHeader, async (page, seeded) => {
    await patchIssue(seeded.key, { labels: [...TWENTY_LABELS], title: MEASURED_TITLE });
    const widths =
      test.info().project.name !== "iphone" ? [DESKTOP.width, IPHONE_13.width] : [IPHONE_13.width];
    for (const width of widths) {
      await page.setViewportSize(viewportAt(width));
      await gotoIssue(page, `/issues/${seeded.key}`);
      await expectHeaderDrawn(page);
      await expect(waitingCount(page)).toHaveText(/^Waiting on you \(\d+\)$/);
      const count = ((await waitingCount(page).textContent()) ?? "").replace(/\s+/g, " ").trim();
      expect
        .soft(
          await drawnWords(waitingCount(page)),
          `the count of what waits draws other words at ${width} px`
        )
        .toBe(count);
      await expect(labelsLeft(page)).toHaveText(/^\+\d+ labels?$/);
      const left = ((await labelsLeft(page).textContent()) ?? "").replace(/\s+/g, " ").trim();
      expect
        .soft(
          await drawnWords(labelsLeft(page)),
          `the labels' count draws other words at ${width} px`
        )
        .toBe(left);
    }
  });
});

/** The logical login of a reader of the row's own (`rowPerson`, e2e/test.ts) whose login is as
 *  long as the measured root's assignee's, `dana@example.dev`: 16 characters, these eleven and
 *  the five letters of the row's own. Everyone who has signed in is one list, the options of
 *  every assignee control, which a fixed login would stay in for every later row of the worker;
 *  the row's own person is taken out of it again when the row ends (`ownPeople`). */
const LONG_LOGIN = "danaexample";

// The header holds its limits with 20 labels and the measured title, which is a real root's
// own. On a copy of that root - a root of a project that gates its spec, 24 children with 3 done,
// assigned to a 16-character login - with 20 labels and the spec's approval request waiting on
// the reader, the header once took 4 lines and 127 px at 1440 px and 11 lines and 327 px at
// 390 px. Row (1)'s seed is a child with one chip and 3 children, so no row read this issue's
// shape. This row seeds that shape, under the title the 390 px limit is read on (`PHONE_TITLE`,
// the measured title less a word), and reads the header's limits with no request waiting and
// with one; on the phone it reads 390 px.
// SLOW(>=2s): 3,938 ms on chromium and 2,597 ms on iphone (one worker, Job e2e-rows-rdf2g at
// 43949bc0): a root written with 24 children, a second context signed in as the long login, and
// four loads on chromium and two on iphone, the header's limits measured after each.
test("the header holds its limits on a large gated root @phone", async ({ browser }) => {
  test.setTimeout(240_000);
  await phase(
    browser,
    async () => {
      const like = rowKey("LIKE");
      await createProject({ key: like, name: "Large root" });
      await setGatedSteps(like, ["spec"]);
      const root = await createIssue({
        force: true,
        project: like,
        spec: "The plan.",
        title: PHONE_TITLE,
      });
      for (let at = 0; at < 24; at += 1) {
        const child = await createIssue({
          force: true,
          parent: root.key,
          project: like,
          title: `Part ${at + 1} of the root`,
        });
        if (at < 3) await patchIssue(child.key, { status: "done" });
      }
      return root;
    },
    async (_alice, root) => {
      const login = rowPerson(LONG_LOGIN);
      const context = await asUser(browser, login);
      try {
        await withIssuePage(context, login);
        const page = await context.newPage();
        await patchIssue(root.key, {
          assignee: login,
          labels: [...TWENTY_LABELS],
          priority: 1,
          status: "in_progress",
        });
        const widths =
          test.info().project.name !== "iphone"
            ? [DESKTOP.width, IPHONE_13.width]
            : [IPHONE_13.width];
        for (const waiting of [false, true]) {
          if (waiting) {
            await requestApproval(root.primary_artifact_id ?? "", { summary: "Read it." });
          }
          for (const width of widths) {
            await page.setViewportSize(viewportAt(width));
            await gotoIssue(page, `/issues/${root.key}`);
            await expectHeaderDrawn(page);
            await expect(issueHeader(page).getByTestId("issue-approval-steps")).toBeVisible();
            await expect(issueHeader(page).getByTestId("issue-progress-subtree")).toBeVisible();
            if (waiting) await expect(waitingCount(page)).toHaveText("Waiting on you (1)");
            await expectHeaderWithinItsLimit(page, width, waiting, TYPEFACE);
          }
        }
      } finally {
        await context.close();
      }
    }
  );
});

// R3 at 1440 px on a project that gates two or three steps, where the header draws one approval
// entry for each step (R6): at most 4 lines and 130 px, with no request waiting and with one on
// every gated document, so each entry carries Approve and Request changes (decision 213). The
// root has the large root's shape above - 20 labels, 24 children with 3 done and the
// 16-character login as its assignee - and the measured title whole, so the properties line is
// as long as R3's cases.
// SLOW(>=2s): 3,199 to 3,779 ms in three one-worker runs (Job e2e-rows-klnz7 at 4788afaa): two
// roots written with 24 children and their gated documents each, a second context signed in as
// the long login, and four loads, the header's limit measured after each.
test("the header holds its 1440 px limit on a root whose project gates two or three steps", async ({
  browser,
}) => {
  test.setTimeout(240_000);
  const projects = [
    { logical: "TWO", steps: ["criteria", "spec"] },
    { logical: "THREE", steps: ["decisions", "criteria", "spec"] },
  ] as const;
  await phase(
    browser,
    async () => {
      const roots: { key: string; documents: string[]; steps: readonly string[] }[] = [];
      for (const { logical, steps } of projects) {
        const key = rowKey(logical);
        await createProject({ key, name: `Gates ${steps.length} steps` });
        await setGatedSteps(key, [...steps]);
        const root = await createIssue({
          force: true,
          project: key,
          spec: "The plan.",
          title: MEASURED_TITLE,
        });
        // The spec is the root's own document; each other gated step gets the document it gates.
        const documents = [root.primary_artifact_id ?? ""];
        for (const step of steps) {
          if (step === "spec") continue;
          const name = step === "criteria" ? "verification.md" : "decisions.md";
          const made = await createIssueArtifact(root.key, { content: `# ${name}\n`, name });
          documents.push(made.artifact.id);
        }
        for (let at = 0; at < 24; at += 1) {
          const child = await createIssue({
            force: true,
            parent: root.key,
            project: key,
            title: `Part ${at + 1} of the root`,
          });
          if (at < 3) await patchIssue(child.key, { status: "done" });
        }
        roots.push({ documents, key: root.key, steps });
      }
      return roots;
    },
    async (_alice, roots) => {
      const login = rowPerson(LONG_LOGIN);
      const context = await asUser(browser, login);
      try {
        await withIssuePage(context, login);
        const page = await context.newPage();
        await page.setViewportSize(viewportAt(DESKTOP.width));
        for (const root of roots) {
          await patchIssue(root.key, {
            assignee: login,
            labels: [...TWENTY_LABELS],
            priority: 1,
            status: "in_progress",
          });
          for (const waiting of [false, true]) {
            if (waiting) {
              for (const document of root.documents) {
                await requestApproval(document, { summary: "Read it." });
              }
            }
            await gotoIssue(page, `/issues/${root.key}`);
            await expectHeaderDrawn(page);
            for (const step of root.steps) {
              await expect(
                approvalEntry(page, step),
                `the header draws no entry for the ${step} step of ${root.key}`
              ).toBeVisible();
            }
            if (waiting) {
              await expect(waitingCount(page)).toHaveText(
                `Waiting on you (${root.documents.length})`
              );
            }
            await expectHeaderWithinItsLimit(
              page,
              DESKTOP.width,
              waiting,
              TYPEFACE,
              root.steps.length
            );
          }
        }
      } finally {
        await context.close();
      }
    }
  );
});

/** Waits for the frames in which the header's labels read the line again after its width
 *  changed: the observer's notification, the frame it waits for, and the commits after it. */
function framesDrawn(page: Page, frames: number): Promise<void> {
  return page.evaluate((frames) => {
    const { promise, resolve } = Promise.withResolvers<void>();
    const next = (left: number) =>
      left === 0 ? resolve() : requestAnimationFrame(() => next(left - 1));
    next(frames);
    return promise;
  }, frames);
}

// The labels' count names every label the line leaves out, at every width of the line: it is a
// count of the rest. Row (1) reads the count at four widths, where a band of a few pixels in
// which the count was wrong may not fall, so a count read at a width other than the one drawn
// passed it: with the count's held width taken out, row (1) still passed.
// This row narrows the line pixel by pixel and reads, at each width, that the chips drawn whole
// and the count's number add up to every label, and that no chip is drawn cut.
// SLOW(>=2s): 2,088 frames - four at each of 261 widths of the line, at two viewport widths -
// after two loads of the issue: 36,518 to 37,382 ms on one worker with the walk in the page (three
// runs, Job e2e-rows-5q7j2 at 13d67d68 with this walk), and 47,383 ms as three round trips a step
// (one worker, Job e2e-rows-rdf2g at 43949bc0).
test("the labels' count names every label the line leaves out, at every width of the line", async ({
  browser,
}) => {
  test.setTimeout(240_000);
  await phase(browser, seedHeader, async (page, seeded) => {
    await patchIssue(seeded.key, { labels: [...TWENTY_LABELS], title: MEASURED_TITLE });
    await requestApproval(seeded.specArtifactId, { summary: "Read it back." });
    for (const width of [DESKTOP.width, 1024]) {
      await page.setViewportSize(viewportAt(width));
      await gotoIssue(page, `/issues/${seeded.key}`);
      await expectHeaderDrawn(page);
      await expect(labelsLeft(page)).toBeVisible();
      // The whole walk is one page function, so a step costs its four frames and no round trip of
      // the row's: each width is set, drawn and read in the page, with the reading
      // `labelsAgainstTheCount` makes (`LABELS_READING`), and only what is wrong comes back.
      const wrong = await propertiesLine(page).evaluate(
        async (line, { labels, reading }) => {
          const read = new Function(`return ${reading}`)() as (
            line: Element
          ) => LabelsAgainstTheCount;
          const full = line.getBoundingClientRect().width;
          const wrong: string[] = [];
          for (let by = 0; by <= 260; by += 1) {
            line.style.maxWidth = `${full - by}px`;
            for (let frames = 0; frames < 4; frames += 1) {
              const drawn = Promise.withResolvers<void>();
              requestAnimationFrame(() => drawn.resolve());
              await drawn.promise;
            }
            // Four animation frames have begun since the width changed (the read runs in the
            // fourth's callback), so the labels were read again at that width.
            const { cut, left, whole } = read(line);
            if (whole + left !== labels || cut.length > 0) {
              wrong.push(
                `${(full - by).toFixed(0)} px: ${whole} drawn whole, ${cut.length} cut, the count names ${left}, of ${labels}`
              );
            }
          }
          line.style.maxWidth = "";
          return wrong;
        },
        { labels: TWENTY_LABELS.length, reading: LABELS_READING }
      );
      expect
        .soft(wrong, `the labels' count does not name the labels left out at ${width} px`)
        .toEqual([]);
    }
  });
});

// The labels are read in script, and the typeface (`font-display: swap`, `web/src/styles.css`)
// can arrive after that reading and draw the chips wider while the labels' box keeps its width,
// which the box's resize observer never sees: a chip then wraps onto the line the box clips away,
// and the count, which is a count of the rest, does not name it. At this seed the swap widens the
// line's other items as well - on a 1440 px line the status, the approval control and the labels'
// count each by 5 to 7 px - so the box narrows, and that resize alone has the labels read again.
// The row therefore holds the box at one width across the swap, as a line whose other items keep
// their whole-pixel widths holds it, so the chips are all the swap changes; and that width is the
// one the chips drawn in the fallback fill, so a chip any wider no longer fits.
// SLOW(>=2s): 3,146 ms (one worker, Job e2e-rows-rdf2g at 43949bc0): a second context signed in
// whose requests for the typeface's file a route holds, a load in each context, the font set
// polled until the face waits and again until it loads, and the labels read after each.
test("the labels' count names every label the line does not draw whole after the typeface loads", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  await phase(browser, seedHeader, async (page, seeded) => {
    await patchIssue(seeded.key, { labels: [...TWENTY_LABELS], title: MEASURED_TITLE });
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectHeaderDrawn(page);
    const address = await typefaceAddress(page);

    // A context of its own, with nothing of the file in hand, whose requests for it wait until
    // the row lets them go.
    const held = await asUser(browser, "alice");
    try {
      await withIssuePage(held, "alice");
      const file = Promise.withResolvers<void>();
      await held.route(address, async (route) => {
        await file.promise;
        await route.fallback();
      });
      const swapping = await held.newPage();
      await swapping.setViewportSize(DESKTOP);
      await gotoIssue(swapping, `/issues/${seeded.key}`);
      await expectHeaderDrawn(swapping);
      await expect(labelsLeft(swapping)).toBeVisible();
      await expect
        .poll(() => typefaceStatuses(swapping), {
          message: `the ${TYPEFACE} face is not waiting for its file`,
        })
        .toEqual(["loading"]);

      // In the fallback, the box held at the width its chips fill, and the line read again there.
      await holdLabelsAtTheirChips(swapping);
      await framesDrawn(swapping, 4);
      const inTheFallback = await labelsAgainstTheCount(swapping);
      expect(inTheFallback.whole, "the line draws no label in the fallback").toBeGreaterThan(0);
      expect(inTheFallback.cut, "a label is cut in the fallback").toEqual([]);
      expect(
        inTheFallback.whole + inTheFallback.left,
        `in the fallback ${inTheFallback.whole} labels are drawn whole and the count names ${inTheFallback.left}`
      ).toBe(TWENTY_LABELS.length);

      // The file arrives, and the face draws the chips.
      file.resolve();
      await expect
        .poll(() => typefaceStatuses(swapping), {
          message: `the ${TYPEFACE} face did not load once its file was let go`,
        })
        .toEqual(["loaded"]);
      await framesDrawn(swapping, 4);
      const inTheFace = await labelsAgainstTheCount(swapping);
      expect(inTheFace.cut, `a label is cut once ${TYPEFACE} has loaded`).toEqual([]);
      expect(
        inTheFace.whole + inTheFace.left,
        `once ${TYPEFACE} has loaded ${inTheFace.whole} labels are drawn whole and the count names ${inTheFace.left}`
      ).toBe(TWENTY_LABELS.length);
      // A face whose chips came out no wider than the fallback's would leave every label where it
      // was, and the row would pass with the typeface's listener taken out.
      expect(
        inTheFace.left,
        `the face pushed no label off the line: the count names ${inTheFace.left} once ${TYPEFACE} has loaded and ${inTheFallback.left} in the fallback`
      ).toBeGreaterThan(inTheFallback.left);
    } finally {
      await held.close();
    }
  });
});

// A focus alone opens no title editor, so a key typed after the page or the browser gives the
// heading the focus back is no part of the title. Row (8) reads that through Details, whose
// drawer gives the focus back again in the frames after it closes: with the heading's old
// `onFocus` put back, the editor that focus opened was closed again by that second focus, and
// row (8) passed. Here the page gives the heading its focus back once, as a return to the window
// does, and nothing focuses it again.
// SLOW(>=2s): 1,477 to 1,896 ms in six one-worker runs, Jobs e2e-rows-2chx9 at 2e3f0319 and
// e2e-rows-9whgn at 4d06932a: one load, an edit opened by e and ended by Enter, the focus taken and
// given back with two frames waited, a key typed, and the title read back from the server.
test("a focus given back to the title's heading opens no editor", async ({ browser }) => {
  test.setTimeout(120_000);
  await phase(browser, seedHeader, async (page, seeded) => {
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectHeaderDrawn(page);
    await leaveFocus(page);
    const header = issueHeader(page);
    const heading = header.getByRole("heading", { level: 1 });
    const editor = header.getByRole("textbox", { name: "Issue title" });
    const title = (await getIssue(seeded.key)).title;
    // Enter ends an edit with the focus on the heading; the page then loses the focus and gives
    // it back to that heading.
    await page.keyboard.press("e");
    await expect(editor, "e did not put the caret in the title").toBeFocused();
    await page.keyboard.press("Enter");
    await expect(heading, "Enter dropped the focus out of the header").toBeFocused();
    await heading.evaluate((element) => {
      (element as HTMLElement).blur();
      (element as HTMLElement).focus();
    });
    await framesDrawn(page, 2);
    await expect(editor, "a focus given back to the heading opened the title's editor").toHaveCount(
      0
    );
    await page.keyboard.press("x");
    await leaveFocus(page);
    expect(
      (await getIssue(seeded.key)).title,
      "a key typed after a focus given back to the heading changed the title"
    ).toBe(title);
  });
});

// On the new page a keyboard pick of the status keeps the focus on the status (WCAG 2.4.3). The
// select was disabled while its own write saved, and the browser moves the focus off a disabled
// control, so after `s` and one arrow key the focus was on the page's body: a second arrow key
// did nothing, Tab started from the top, and an Escape pressed before the select was disabled
// was not taken. Each focus is read once, after the pick is drawn, because a retrying read
// passes in the frame before the pick disables the select. The status's writes are held open
// with a route, so the save stays in flight while the row reads it.
// SLOW(>=2s): 2,085 ms in one of three one-worker runs, Job e2e-rows-2chx9 at 2e3f0319 (1,573 and
// 1,748 ms in the others): one load, the status's writes held open by a route, and keyboard picks
// each followed by two frames and a read of the focus.
test("a keyboard pick of the status keeps the focus on the status while it saves", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  await phase(browser, seedHeader, async (page, seeded) => {
    const key = seeded.key;
    await page.setViewportSize(viewportAt(DESKTOP.width));
    await gotoIssue(page, `/issues/${key}`);
    await expectHeaderDrawn(page);
    await leaveFocus(page);
    const status = issueHeader(page).getByRole("combobox", { name: "Status" });
    await expect(status).toHaveValue("in_progress");
    const statusFocused = () => status.evaluate((select) => select === document.activeElement);

    const held: (() => void)[] = [];
    const writes: unknown[] = [];
    await page.route(`**/api/v1/issues/${key}`, async (route) => {
      if (route.request().method() !== "PATCH") {
        await route.continue();
        return;
      }
      writes.push(route.request().postDataJSON());
      const { promise, resolve } = Promise.withResolvers<void>();
      held.push(resolve);
      await promise;
      await route.continue();
    });

    await page.keyboard.press("s");
    await expect(status, "s did not put the focus on the status").toBeFocused();
    await page.keyboard.press("ArrowDown");
    await expect(status).toHaveValue("testing");
    await framesDrawn(page, 2);
    expect(
      await statusFocused(),
      "a keyboard pick of the status moved the focus off the status"
    ).toBe(true);

    // A second arrow key while the first pick saves picks again, and the focus stays.
    await page.keyboard.press("ArrowDown");
    await expect(status, "a second arrow key while the status saved picked nothing").toHaveValue(
      "needs_review"
    );
    await framesDrawn(page, 2);
    expect(
      await statusFocused(),
      "a second keyboard pick of the status moved the focus off the status"
    ).toBe(true);

    // Escape, while the save is still in flight, gives the focus back to the page.
    await page.keyboard.press("Escape");
    await expect
      .poll(() => onBody(page), { message: "Escape did not leave the status while it saved" })
      .toBe(true);

    // Once the writes are let through, the issue holds the last status picked.
    await expect.poll(() => held.length).toBeGreaterThan(0);
    for (;;) {
      const release = held.shift();
      if (release === undefined) break;
      release();
      await framesDrawn(page, 2);
    }
    await page.unroute(`**/api/v1/issues/${key}`);
    await expect
      .poll(async () => (await getIssue(key)).status, {
        message: "the status picked last is not the issue's status",
      })
      .toBe("needs_review");
    expect(writes.length, "no status was written").toBeGreaterThan(0);
  });
});

/** A Tab's walk through one part of the page, read where the page stands now (`tabWalk`). */
interface TabWalk {
  /** Each control of the part that a Tab reaches - drawn, not disabled, a `tabIndex` of 0 or
   *  more - in document order: its name, its computed `scroll-margin-top` and its top on the
   *  screen. */
  readonly controls: readonly {
    readonly margin: string;
    readonly name: string;
    readonly top: number;
  }[];
  /** Which of them holds the focus, or -1 where none does. */
  readonly focused: number;
  /** How far the page has scrolled. */
  readonly scrolled: number;
}

/** `TabWalk` of `part`. With `focusNext`, it first focuses the control a Tab reaches next after
 *  the part's last one, in document order, without scrolling the page (`preventScroll`): where a
 *  focus that comes back into the part from below starts. It throws where the part holds no
 *  control, or where no control follows its last one. */
function tabWalk(part: Locator, focusNext = false): Promise<TabWalk> {
  return part.evaluate((scope, focusNext) => {
    const reached = [...document.querySelectorAll<HTMLElement>("*")].filter(
      (element) =>
        element.tabIndex >= 0 && !element.matches(":disabled") && element.checkVisibility()
    );
    const inside = reached.filter((element) => scope.contains(element));
    const name = (element: Element) => {
      const words = element.getAttribute("aria-label") ?? (element.textContent ?? "").trim();
      return `${element.tagName.toLowerCase()} "${words}"`;
    };
    if (focusNext) {
      const last = inside.at(-1);
      if (last === undefined) throw new Error(`${scope.tagName} holds no control`);
      const after = reached[reached.indexOf(last) + 1];
      if (after === undefined) {
        throw new Error(`no control follows the last one of ${scope.tagName}`);
      }
      after.focus({ preventScroll: true });
      if (document.activeElement !== after) throw new Error(`${name(after)} took no focus`);
    }
    return {
      controls: inside.map((element) => ({
        margin: getComputedStyle(element).scrollMarginTop,
        name: name(element),
        top: element.getBoundingClientRect().top,
      })),
      focused: inside.indexOf(document.activeElement as HTMLElement),
      scrolled: window.scrollY,
    };
  }, focusNext);
}

// A focus that enters the header from below moves no page. The page keeps the band at its top
// clear of what a focus brings into view (`scroll-padding-top` in `web/src/styles.css`), so
// without a top scroll margin of minus that band a focus on a header control lying in it scrolls
// the page up, and a control of what stays does not move with it. At 1440 px the identity block
// is stuck: Shift+Tab from the properties line under it walks back through every control of the
// block to its first and the page does not move, each control of the block takes minus the
// padding, and each control of the properties line takes none, so a focus there under the block
// still scrolls it out from under it. At 390 px the whole header scrolls away and the padding is
// the view row's height: Shift+Tab from below onto the header's last control, while that control
// lies 10 px below the top of the window, does not move the page, and each control of the header
// takes minus the padding. On the phone it reads 390 px.
// SLOW(>=2s): 3,657 ms on chromium and 2,183 ms on iphone with four workers (CI run
// 44646643988), 1,959 ms on chromium on one worker (Job e2e-rows-rdf2g at 43949bc0): a load into
// a short window at each width, and at 1440 px a Shift+Tab back through each control of the
// band, each followed by two frames and a walk of every element of the page.
test("a focus that enters the header from below moves no page @phone", async ({ browser }) => {
  test.setTimeout(150_000);
  await phase(browser, seedHeader, async (page, seeded) => {
    const widths =
      test.info().project.name === "iphone" ? [IPHONE_13.width] : [DESKTOP.width, IPHONE_13.width];
    for (const width of widths) {
      await openScrollingPage(page, seeded.key, width);

      if (width > 760) {
        await page.evaluate(() => window.scrollTo(0, 320));
        await expect
          .poll(async () => (await stickyReading(page))?.condensed, {
            message: `the band is not stuck once scrolled at ${width} px`,
          })
          .toBe(true);
        const padding = await page.evaluate(
          () => getComputedStyle(document.documentElement).scrollPaddingTop
        );
        const from = await tabWalk(identityBand(page), true);
        for (let at = from.controls.length - 1; at >= 0; at -= 1) {
          const name = from.controls[at]?.name;
          await page.keyboard.press("Shift+Tab");
          await framesDrawn(page, 2);
          const walk = await tabWalk(identityBand(page));
          expect(walk.focused, `Shift+Tab did not reach the band's ${name} at ${width} px`).toBe(
            at
          );
          expect(
            walk.scrolled,
            `Shift+Tab onto the band's ${name} moved the page at ${width} px`
          ).toBe(from.scrolled);
        }
        expect(
          from.controls.map(({ margin, name }) => `${name}: ${margin}`),
          `a control of the band takes no top scroll margin of minus the padding at ${width} px`
        ).toEqual(from.controls.map(({ name }) => `${name}: -${padding}`));
        const properties = (await tabWalk(propertiesLine(page))).controls;
        expect(
          properties.length,
          `the properties line holds no control at ${width} px`
        ).toBeGreaterThan(0);
        expect(
          properties.map(({ margin, name }) => `${name}: ${margin}`),
          `a control of the properties line takes a top scroll margin at ${width} px`
        ).toEqual(properties.map(({ name }) => `${name}: 0px`));
      } else {
        const top = await tabWalk(issueHeader(page));
        const last = top.controls.at(-1);
        if (last === undefined) throw new Error(`the header holds no control at ${width} px`);
        const to = Math.round(top.scrolled + last.top - 10);
        await page.evaluate((to) => window.scrollTo(0, to), to);
        await expect
          .poll(() => page.evaluate(() => window.scrollY), {
            message: `the page did not scroll the header's ${last.name} to the top at ${width} px`,
          })
          .toBeCloseTo(to, 0);
        const padding = await page.evaluate(
          () => getComputedStyle(document.documentElement).scrollPaddingTop
        );
        expect(
          Number.parseFloat(padding),
          `the padding at ${width} px does not reach the header's ${last.name} 10 px below the top`
        ).toBeGreaterThan(10);
        const from = await tabWalk(issueHeader(page), true);
        await page.keyboard.press("Shift+Tab");
        await framesDrawn(page, 2);
        const walk = await tabWalk(issueHeader(page));
        expect(
          walk.focused,
          `Shift+Tab did not reach the header's ${last.name} at ${width} px`
        ).toBe(from.controls.length - 1);
        expect(
          walk.scrolled,
          `Shift+Tab onto the header's ${last.name} moved the page at ${width} px`
        ).toBe(from.scrolled);
        expect(
          from.controls.map(({ margin, name }) => `${name}: ${margin}`),
          `a control of the header takes no top scroll margin of minus the padding at ${width} px`
        ).toEqual(from.controls.map(({ name }) => `${name}: -${padding}`));
      }
    }
  });
});

// The header's ⋯ menu gives the focus back to its own control, 'Issue actions', on Escape and
// after each action, as the ARIA menu-button pattern does (WCAG 2.4.3). The menu hid the focused
// action as it closed, and nothing gave the focus back, so the browser left it on the page's
// body: the next Tab started again from the top of the page. Each focus is read once, after the
// menu has closed and two frames are drawn, so a focus that only passes through the control is
// not taken for one that stays there. It reads 1440 px and 390 px, where the menu is drawn as a
// panel of its own.
// SLOW(>=2s): 2,831 ms (one worker, Job e2e-rows-rdf2g at 43949bc0): a load at 1440 px and at
// 390 px, the menu opened from the keyboard seven times, a pin and an unpin at each width waited
// on until Dispatch answers them, and two frames drawn before each read of the focus.
test("the header's menu gives the focus back to its control on Escape and after an action", async ({
  browser,
}) => {
  test.setTimeout(150_000);
  await phase(browser, seedHeader, async (page, seeded) => {
    const header = issueHeader(page);
    const trigger = header.getByRole("button", {
      exact: true,
      name: CONTROL_LABELS.issue_actions,
    });
    const triggerFocused = () => trigger.evaluate((control) => control === document.activeElement);
    // Opens the menu from the keyboard and puts the focus on its action `at`, counted from 1.
    const onAction = async (at: number) => {
      await trigger.focus();
      await page.keyboard.press("Enter");
      await expect(trigger).toHaveAttribute("aria-expanded", "true");
      for (let step = 0; step < at; step += 1) await page.keyboard.press("Tab");
      // The menu was read open above, and each Tab moves the focus before its press resolves;
      // null only throws.
      const focused = await page.evaluate(
        () => (document.activeElement as HTMLElement | null)?.dataset.issueAction ?? null
      );
      if (focused === null) throw new Error(`Tab did not reach the menu's action ${at}`);
      return focused;
    };
    for (const width of [DESKTOP.width, IPHONE_13.width]) {
      await page.setViewportSize(viewportAt(width));
      await gotoIssue(page, `/issues/${seeded.key}`);
      await expectHeaderDrawn(page);
      await leaveFocus(page);

      await onAction(1);
      await page.keyboard.press("Escape");
      await expect(trigger).toHaveAttribute("aria-expanded", "false");
      await framesDrawn(page, 2);
      expect(
        await triggerFocused(),
        `Escape out of the menu left the focus off its control at ${width} px`
      ).toBe(true);

      for (const words of ["pin", "unpin"]) {
        const action = await onAction(1);
        expect(action, `the menu's first action is not the pin at ${width} px`).toBe("pin");
        const written = page.waitForResponse(
          (response) =>
            response.request().method() !== "GET" &&
            new URL(response.url()).pathname.includes(seeded.key)
        );
        await page.keyboard.press("Enter");
        await written;
        await expect(trigger).toHaveAttribute("aria-expanded", "false");
        await framesDrawn(page, 2);
        expect(
          await triggerFocused(),
          `the menu's ${words} by Enter left the focus off its control at ${width} px`
        ).toBe(true);
      }
    }

    // Close issue, last, since it closes the issue.
    const action = await onAction(2);
    expect(action, "the menu's second action is not Close issue").toBe("close");
    await page.keyboard.press("Enter");
    await expect(header.getByText(STATE_LINES.closed, { exact: true })).toBeVisible();
    await framesDrawn(page, 2);
    expect(
      await triggerFocused(),
      "the menu's Close issue by Enter left the focus off its control"
    ).toBe(true);
  });
});

/** Waits for the focused element to be in a reader's sight against the header's actions menu, the
 *  element whose id is `menuId`: the focus off the page's body and outside the menu, the menu's
 *  box apart from the focused element's, and the focused element itself drawn at its centre. A
 *  hidden menu covers nothing. `message` names the press that moved the focus there. */
async function expectFocusInSight(page: Page, menuId: string, message: string): Promise<void> {
  /** What keeps the focused element from sight: nothing, or each check above that fails. */
  const keptFromSight = async (): Promise<string[]> => {
    // Each read is inside the poll below, which reads again until this answers nothing, so one
    // instant's answer is safe here.
    const read = await page.evaluate((id) => {
      const menu = document.getElementById(id);
      if (menu === null) throw new Error(`no element carries the actions menu's id ${id}`);
      const focused = document.activeElement;
      if (focused === null || focused === document.body) return null;
      const boxOf = (element: Element) => {
        const { height, width, x, y } = element.getBoundingClientRect();
        return { height, width, x, y };
      };
      const box = boxOf(focused);
      const drawn = document.elementFromPoint(box.x + box.width / 2, box.y + box.height / 2);
      const onTop = drawn !== null && focused.contains(drawn);
      const name = focused.getAttribute("aria-label") ?? focused.textContent ?? "";
      return {
        box,
        drawn: onTop ? "" : (drawn?.outerHTML.slice(0, 120) ?? "nothing"),
        inMenu: menu.contains(focused),
        menu: menu.checkVisibility() ? boxOf(menu) : null,
        onTop,
        what: `${focused.tagName.toLowerCase()} "${name.trim()}"`,
      };
    }, menuId);
    if (read === null) return ["the focus is on the page's body"];
    const unseen: string[] = [];
    if (read.inMenu) unseen.push(`the focus is on ${read.what}, inside the menu`);
    if (read.menu !== null && !boxesApart(read.box, read.menu)) {
      unseen.push(
        `the menu at ${JSON.stringify(read.menu)} covers ${read.what} at ${JSON.stringify(read.box)}`
      );
    }
    if (!read.onTop) unseen.push(`${read.drawn} is drawn at the centre of ${read.what}`);
    return unseen;
  };
  await expect.poll(keptFromSight, { message }).toEqual([]);
}

// The header's ⋯ menu closes once Tab takes the focus on out of it, as the ARIA menu-button
// pattern closes a menu the focus leaves. At 390 px the open menu is a panel across the screen
// over the title and the properties line, and it stayed open as Tab took the focus on past its
// last action, so the focus went on to controls under it that the reader could not see (WCAG
// 2.4.11). The row opens the menu by Enter from its control, Tabs once per action and once
// more, and reads the menu closed. It then Tabs on to the title's heading and then the Status,
// the two the open menu covers at 390 px, and reads the focus in sight at every control it stops
// on. It sets 390 px for every project before it reads anything, so the phone project would only
// repeat it.
// SLOW(>=2s): 1,333 to 1,941 ms in six one-worker runs, Jobs e2e-rows-2chx9 at 2e3f0319 and
// e2e-rows-9whgn at 4d06932a: one load at 390 px, the menu opened by Enter, a Tab to each of its
// actions with the menu's state read at each, and one Tab more with the focus read in sight.
test("the header's menu closes once Tab takes the focus out of it, leaving the focus in sight", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  await phase(browser, seedHeader, async (page, seeded) => {
    await page.setViewportSize(IPHONE_13);
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectHeaderDrawn(page);
    const header = issueHeader(page);
    const trigger = header.getByRole("button", { exact: true, name: CONTROL_LABELS.issue_actions });
    await trigger.focus();
    await page.keyboard.press("Enter");
    await expect(trigger).toHaveAttribute("aria-expanded", "true");
    // The control names its menu from its first render, so a missing name only throws.
    const menuId = await trigger.getAttribute("aria-controls");
    if (menuId === null) throw new Error("the actions control names no menu");
    const actions = page.locator(`[id="${menuId}"] button:enabled`);
    // The control read expanded above, and the open menu draws its actions with it.
    const reachable = await actions.count();
    expect(reachable, "the open menu holds no action a Tab reaches at 390 px").toBeGreaterThan(0);
    for (let at = 0; at < reachable; at += 1) {
      await page.keyboard.press("Tab");
      await expect(actions.nth(at), `Tab ${at + 1} did not reach the menu's action`).toBeFocused();
      await expect(trigger, `Tab ${at + 1} closed the menu inside it`).toHaveAttribute(
        "aria-expanded",
        "true"
      );
    }
    // One Tab more takes the focus out of the menu, to the header's next control.
    await page.keyboard.press("Tab");
    await expect(
      trigger,
      "the menu stayed open once Tab took the focus out of it at 390 px"
    ).toHaveAttribute("aria-expanded", "false");
    await expectFocusInSight(page, menuId, "the first Tab out of the menu left the focus unseen");
    // On from there to the title's heading and then the Status, each found by its role and name.
    for (const [what, target] of [
      ["the title's heading", header.getByRole("heading", { level: 1 })],
      ["the Status select", header.getByRole("combobox", { name: "Status" })],
    ] as const) {
      let reached = false;
      for (let press = 1; press <= 6 && !reached; press += 1) {
        await page.keyboard.press("Tab");
        await expectFocusInSight(page, menuId, `Tab ${press} toward ${what} left the focus unseen`);
        // Each Tab moves the focus before its press resolves, so one read says where it went.
        reached = await target.evaluate((element) => element === document.activeElement);
      }
      expect(reached, `six Tabs did not take the focus on to ${what} at 390 px`).toBe(true);
    }
  });
});

/** What `focusNow` reads with the focus on the header's "Issue actions", the first place the
 *  header's keeper puts a focus whose control has gone. */
const ON_ISSUE_ACTIONS = {
  inHeader: true,
  onBody: false,
  what: `button ${CONTROL_LABELS.issue_actions}`,
} as const;

/** Waits for Details to hold the focus: just opened, as it takes the focus as it opens, or after a
 *  dialog opened from it gave the focus back as it closed, which `message` then names. */
async function expectFocusInDetails(
  page: Page,
  message = "Details took no focus as it opened"
): Promise<void> {
  await expect
    .poll(() => details(page).evaluate((drawer) => drawer.contains(document.activeElement)), {
      message,
    })
    .toBe(true);
}

/** Closes Details by Escape, and waits until it is hidden and two frames are drawn, so a focus
 *  read next that only passes through a control is not taken for one that stays there. */
async function escapeDetails(page: Page): Promise<void> {
  await page.keyboard.press("Escape");
  await expect(details(page), "Escape left Details open").toBeHidden();
  await framesDrawn(page, 2);
}

// Details gives the focus back to the control it was opened from as it closes, but a write made
// while it is open can take that control off the header: the labels' count goes once every label
// fits, and the parent link once the parent is cleared. Details then had nothing to give the
// focus back to and left it on the page's body, from which the next Tab starts again at the top
// of the page; the header's keeper had stopped watching once the focus left the header. The
// keeper now waits for Details to close and places that focus by the header's own fallbacks, the
// first of which is "Issue actions" (decision 237). The focus is read once, after Details has
// closed and two frames are drawn, so a focus that only passes through a control is not taken
// for one that stays there.
// SLOW(>=2s): 2,053 ms (one worker, Job e2e-rows-rdf2g at 43949bc0): a load with twenty labels,
// Details opened by Enter, a write of the labels through the API that the open page takes in,
// and Details closed with two frames drawn after.
test("Details closed after a write removed the labels' count leaves the focus in the header", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  await phase(browser, seedHeader, async (page, seeded) => {
    await patchIssue(seeded.key, { labels: [...TWENTY_LABELS] });
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectHeaderDrawn(page);
    const count = labelsLeft(page);
    await expect(count, "twenty labels draw no labels' count").toBeVisible();
    await count.focus();
    await page.keyboard.press("Enter");
    await expect(details(page), "Enter on the labels' count opened no Details").toBeVisible();
    await expectFocusInDetails(page);
    await patchIssue(seeded.key, { labels: ["dispatch"] });
    await expect(count, "one label still draws the labels' count").toHaveCount(0);
    await escapeDetails(page);
    expect(
      await focusNow(page),
      "Details, closed after its labels' count went, left the focus off Issue actions"
    ).toEqual(ON_ISSUE_ACTIONS);
  });
});

// The same with the search palette opened by Control+K from inside Details before the write. The
// palette is a dialog the app draws in its own root, beside Details rather than inside it, and it
// gives the focus back into Details as it closes. The keeper stopped watching Details once the
// focus went into the palette, so the count's removal and Details' close then left the focus on
// the page's body. A focus in another layer now keeps the watch going, since that layer gives the
// focus back, and the focus ends on "Issue actions" as in the row above.
// SLOW(>=2s): 1,644 to 2,040 ms in nine one-worker runs, Jobs e2e-rows-2chx9 at 2e3f0319,
// e2e-rows-9whgn at 4d06932a and e2e-rows-tnxt6 at 214ed1ee: one load with twenty labels, Details
// opened from the labels' count by Enter, the search palette opened by Control+K and its focus
// polled, a write of one label waited on until the count goes, and the palette and Details each
// closed by Escape before the focus is read.
test("Details closed after a write removed the labels' count while the palette was open leaves the focus in the header", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  await phase(browser, seedHeader, async (page, seeded) => {
    await patchIssue(seeded.key, { labels: [...TWENTY_LABELS] });
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectHeaderDrawn(page);
    const count = labelsLeft(page);
    await expect(count, "twenty labels draw no labels' count").toBeVisible();
    await count.focus();
    await page.keyboard.press("Enter");
    await expect(details(page), "Enter on the labels' count opened no Details").toBeVisible();
    await expectFocusInDetails(page);
    await page.keyboard.press("Control+k");
    const palette = page.getByRole("dialog", { name: "Search" });
    await expect(palette, "Control+k in Details did not open the palette").toBeVisible();
    await expect
      .poll(() => palette.evaluate((dialog) => dialog.contains(document.activeElement)), {
        message: "the palette took no focus as it opened",
      })
      .toBe(true);
    await patchIssue(seeded.key, { labels: ["dispatch"] });
    await expect(count, "one label still draws the labels' count").toHaveCount(0);
    await page.keyboard.press("Escape");
    await expect(palette, "Escape did not close the palette").toHaveCount(0);
    await expect(details(page), "Escape in the palette closed Details too").toBeVisible();
    await expectFocusInDetails(page, "the palette did not give the focus back into Details");
    await escapeDetails(page);
    expect(
      await focusNow(page),
      "Details, closed after the palette and after its labels' count went, left the focus off Issue actions"
    ).toEqual(ON_ISSUE_ACTIONS);
  });
});

// The same, opened by Shift+M from the header's parent link, which goes once the parent is
// cleared while Details is open.
test("Details closed after a write removed the parent link leaves the focus in the header", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  await phase(browser, seedHeader, async (page, seeded) => {
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectHeaderDrawn(page);
    const parent = issueHeader(page).getByRole("link", { exact: true, name: seeded.parentKey });
    await parent.focus();
    await expect(parent, "the header's parent link took no focus").toBeFocused();
    await page.keyboard.press("Shift+M");
    await expect(details(page), "Shift+M on the parent link opened no Details").toBeVisible();
    await expectFocusInDetails(page);
    await patchIssue(seeded.key, { parent: null });
    await expect(parent, "a cleared parent still draws the parent link").toHaveCount(0);
    await escapeDetails(page);
    expect(
      await focusNow(page),
      "Details, closed after its parent link went, left the focus off Issue actions"
    ).toEqual(ON_ISSUE_ACTIONS);
  });
});

// The same, opened by Shift+M from the header's Approve while the document's request waits on
// the reader, and bob answers that request while Details is open: the header then draws neither
// action, and the box that keeps the two together goes with them. The keeper reads the elements
// around Approve as the focus leaves it, so it still finds that document's chip and gives it the
// focus, as after an approval made from the header, rather than going on to "Issue actions".
// SLOW(>=2s): 1,670 to 1,999 ms in six one-worker runs, Jobs e2e-rows-2chx9 at 2e3f0319 and
// e2e-rows-9whgn at 4d06932a: one load after an approval request, Details opened by Shift+M,
// another reader's approval written and polled until the header draws no action, and Details closed
// by Escape before the focus is read.
test("Details closed after an answer elsewhere removed Approve leaves the focus on the document's chip", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  await phase(browser, seedHeader, async (page, seeded) => {
    await requestApproval(seeded.specArtifactId, { summary: "Read it." });
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectHeaderDrawn(page);
    const header = issueHeader(page);
    const approve = header.getByRole("button", { exact: true, name: "Approve" });
    await expect(approve, "the chip carries no Approve while its request waits").toBeVisible();
    await expect(
      reviewActions(header),
      "the header's Approve and Request changes are not found by their drawn words"
    ).toHaveCount(2);
    await approve.focus();
    await expect(approve, "the header's Approve took no focus").toBeFocused();
    await page.keyboard.press("Shift+M");
    await expect(details(page), "Shift+M on Approve opened no Details").toBeVisible();
    await expectFocusInDetails(page);
    await reviewArtifact(seeded.specArtifactId, { state: "approved" }, { login: "bob" });
    await expect(
      reviewActions(header),
      "the header still draws an action once bob answered the request"
    ).toHaveCount(0);
    await escapeDetails(page);
    const focus = await focusNow(page);
    await expect(
      approvalChip(page, seeded.specArtifactId),
      `Details, closed after Approve went, left the focus on ${focus.what}, not on the spec's chip`
    ).toBeFocused();
  });
});

/** The documents the page sends a review of, in the order it sends them: each `POST` of
 *  `/api/v1/artifacts/{id}/reviews`, the write Approve and Request changes make. */
function reviewsSent(page: Page): string[] {
  const sent: string[] = [];
  page.on("request", (request) => {
    const reviewed = /^\/api\/v1\/artifacts\/([^/]+)\/reviews$/.exec(
      new URL(request.url()).pathname
    );
    if (request.method() === "POST" && reviewed !== null) {
      sent.push(decodeURIComponent(reviewed[1] ?? ""));
    }
  });
  return sent;
}

// With the focus on the approval chip of a document whose request waits on the reader, `a`
// presses its Approve and the approval is recorded, and the focus stays on the chip. With the
// focus on another of the header's controls, the title's heading, `a` is not the approval's: it
// sends no review and the request still waits. Nor is it on the chip once the document is
// approved, which draws no Approve.
// SLOW(>=2s): 1,510 to 1,960 ms in six one-worker runs, Jobs e2e-rows-2chx9 at 2e3f0319 and
// e2e-rows-9whgn at 4d06932a: one load after an approval request, a pressed on the title's heading
// and two frames waited, then a pressed on the chip and polled until Dispatch records the approval,
// and a pressed again on the approved chip with two frames waited.
test("the approval chip's a approves the document whose request waits, and a elsewhere approves nothing", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  await phase(browser, seedHeader, async (page, seeded) => {
    await requestApproval(seeded.specArtifactId, { summary: "Read it." });
    const reviewed = reviewsSent(page);
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectHeaderDrawn(page);
    const header = issueHeader(page);
    const approve = header.getByRole("button", { exact: true, name: "Approve" });
    await expect(approve, "the chip carries no Approve while its request waits").toBeVisible();

    const heading = header.getByRole("heading", { level: 1 });
    await heading.focus();
    await expect(heading).toBeFocused();
    await page.keyboard.press("a");
    await framesDrawn(page, 2);
    expect(reviewed, "a on the title's heading sent a review").toEqual([]);
    expect(
      (await getArtifact(seeded.specArtifactId)).approval?.state,
      "a on the title's heading answered the request"
    ).toBe("awaiting");
    await expect(approve, "a on the title's heading took the chip's Approve away").toBeVisible();

    const chip = approvalChip(page, seeded.specArtifactId);
    await chip.focus();
    await expect(chip).toBeFocused();
    await page.keyboard.press("a");
    await expect
      .poll(async () => (await getArtifact(seeded.specArtifactId)).approval?.state, {
        message: "a on the chip recorded no approval",
      })
      .toBe("approved");
    expect(reviewed, "a on the chip sent other reviews than the spec's one").toEqual([
      seeded.specArtifactId,
    ]);
    // The header takes the approval in - no request waits, so it draws neither action - and the
    // focus is still on the chip, not on the page's body.
    await expect(reviewActions(header), "the header still draws an action").toHaveCount(0);
    await framesDrawn(page, 2);
    await expect(chip, "the approval left the focus off the chip").toBeFocused();
    // The approved document's chip draws no Approve, so there `a` sends nothing more.
    await page.keyboard.press("a");
    await framesDrawn(page, 2);
    expect(reviewed, "a on the approved document's chip sent another review").toEqual([
      seeded.specArtifactId,
    ]);
  });
});

// With the focus on the chip whose request waits, or on its Approve, Shift+R presses its Request
// changes, which opens the Request changes dialog, and Escape closes the dialog and gives the
// focus back. With the focus on the title's heading it opens nothing.
// SLOW(>=2s): 1,473 to 1,875 ms in six one-worker runs, Jobs e2e-rows-2chx9 at 2e3f0319 and
// e2e-rows-9whgn at 4d06932a: one load after an approval request, Shift+R on the title's heading
// with two frames waited, then the Request changes dialog opened by Shift+R and closed by Escape
// twice, from the chip and from its Approve.
test("the approval chip's Shift+R opens Request changes, from the chip and from its Approve", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  await phase(browser, seedHeader, async (page, seeded) => {
    await requestApproval(seeded.specArtifactId, { summary: "Read it." });
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectHeaderDrawn(page);
    const header = issueHeader(page);
    const approve = header.getByRole("button", { exact: true, name: "Approve" });
    await expect(approve, "the chip carries no Approve while its request waits").toBeVisible();
    const dialog = page.getByRole("dialog", { name: "Request changes" });

    const heading = header.getByRole("heading", { level: 1 });
    await heading.focus();
    await expect(heading).toBeFocused();
    await page.keyboard.press("Shift+R");
    await framesDrawn(page, 2);
    await expect(dialog, "Shift+R on the title's heading opened Request changes").toHaveCount(0);

    for (const [from, control] of [
      ["the chip", approvalChip(page, seeded.specArtifactId)],
      ["its Approve", approve],
    ] as const) {
      await control.focus();
      await expect(control).toBeFocused();
      await page.keyboard.press("Shift+R");
      await expect(dialog, `Shift+R on ${from} did not open Request changes`).toBeVisible();
      await expect(dialog.getByRole("textbox", { name: "Reason" })).toBeFocused();
      await page.keyboard.press("Escape");
      await expect(dialog, "Escape did not close Request changes").toHaveCount(0);
      await expect(control, `Escape did not give the focus back to ${from}`).toBeFocused();
    }
    expect(
      (await getArtifact(seeded.specArtifactId)).approval?.state,
      "opening and closing Request changes answered the request"
    ).toBe("awaiting");
  });
});

// On a gated root, whose header draws an entry per step: with the requests of the spec and of
// the Verification document both waiting on the reader, each entry carries its own Approve and
// Request changes, and `a` on the Verification document's entry approves that document alone.
// The spec's request still waits, and its entry still carries its actions.
test("an approval entry's a approves that entry's document and no other", async ({ browser }) => {
  test.setTimeout(120_000);
  await phase(
    browser,
    async () => {
      const gated = rowKey("GATED");
      await createProject({ key: gated, name: "Gated" });
      await setGatedSteps(gated, ["decisions", "criteria", "spec"]);
      const root = await createIssue({ project: gated, spec: "The plan.", title: "Gated root" });
      const verification = await createIssueArtifact(root.key, {
        content: "# Verification\n\nOne check.\n",
        name: "verification.md",
      });
      const spec = root.primary_artifact_id ?? "";
      await requestApproval(spec, { summary: "Read it." });
      await requestApproval(verification.artifact.id, { summary: "Read it." });
      return { key: root.key, spec, verification: verification.artifact.id };
    },
    async (page, seeded) => {
      const reviewed = reviewsSent(page);
      await gotoIssue(page, `/issues/${seeded.key}`);
      await expectHeaderDrawn(page);
      await expect(waitingCount(page)).toHaveText("Waiting on you (2)");
      for (const step of ["criteria", "spec"]) {
        for (const [words, name] of [
          ["Approve", /^Approve\b/],
          ["Request changes", /^Request changes\b/],
        ] as const) {
          await expect(
            approvalEntry(page, step).getByRole("button", { name }),
            `the ${step} entry carries no ${words} while its request waits`
          ).toBeVisible();
        }
      }

      // The Verification document's chip is the one its own entry draws.
      const chip = approvalEntry(page, "criteria").locator("[data-approval-artifact]");
      await expect(
        chip,
        "the Verification document's entry draws another document's chip"
      ).toHaveAttribute("data-approval-artifact", seeded.verification);
      await chip.focus();
      await expect(chip).toBeFocused();
      await page.keyboard.press("a");
      await expect
        .poll(async () => (await getArtifact(seeded.verification)).approval?.state, {
          message: "a on the Verification document's entry recorded no approval",
        })
        .toBe("approved");
      expect(reviewed, "a on the Verification document's entry reviewed another document").toEqual([
        seeded.verification,
      ]);
      expect(
        (await getArtifact(seeded.spec)).approval?.state,
        "a on the Verification document's entry answered the spec's request"
      ).toBe("awaiting");
      await expect(waitingCount(page)).toHaveText("Waiting on you (1)");
      await expect(
        approvalEntry(page, "criteria").getByRole("button", {
          name: /^(Approve|Request changes)\b/,
        }),
        "the Verification document's entry still draws an action"
      ).toHaveCount(0);
      await expect(
        approvalEntry(page, "spec").getByRole("button", { exact: true, name: "Approve" }),
        "the spec's entry lost its Approve while its request waits"
      ).toBeVisible();
      await expect(
        approvalEntry(page, "spec").getByRole("button", { exact: true, name: "Request changes" }),
        "the spec's entry lost its Request changes while its request waits"
      ).toBeVisible();
      await expect(chip, "the approval left the focus off the entry's chip").toBeFocused();
    }
  );
});

// The key card lists the two keys with their lines, applying while the focus is on the chip
// whose request waits and greyed with it anywhere else; the palette offers the two as rows on
// that same focus alone, and its Request changes row opens the dialog the key does.
// SLOW(>=2s): 2,082 and 2,058 ms in two of three one-worker runs, Job e2e-rows-2chx9 at 2e3f0319
// (1,901 ms in the third): one load after an approval request, the key card opened twice with both
// its lines read each time, the palette opened twice, and Request changes opened from its row and
// closed.
test("the key card and the palette offer the approval chip's two keys while the focus is on it", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  await phase(browser, seedHeader, async (page, seeded) => {
    await requestApproval(seeded.specArtifactId, { summary: "Read it." });
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectHeaderDrawn(page);
    await expect(
      issueHeader(page).getByRole("button", { exact: true, name: "Approve" }),
      "the chip carries no Approve while its request waits"
    ).toBeVisible();
    const chip = approvalChip(page, seeded.specArtifactId);
    const lines = [
      ["Approve the focused approval request", "a"],
      ["Request changes on the focused approval request", "Shift+R"],
    ] as const;

    const help = page.getByRole("dialog", { name: "Keyboard shortcuts" });
    const issueKeys = help.getByRole("region", { exact: true, name: "Issue" });
    for (const onChip of [true, false]) {
      if (onChip) {
        await chip.focus();
        await expect(chip).toBeFocused();
      } else {
        await leaveFocus(page);
      }
      await page.keyboard.press("?");
      await expect(help, "? did not open the key card").toBeVisible();
      for (const [label, keys] of lines) {
        const line = issueKeys.getByRole("listitem").filter({ hasText: label });
        await expect(line.locator("kbd"), `the key card lists no "${label}"`).toHaveText([keys]);
        await expect(
          line,
          onChip
            ? `"${label}" does not apply with the focus on the chip`
            : `"${label}" applies with the focus off the chip`
        ).toHaveAttribute("data-enabled", String(onChip));
      }
      await page.keyboard.press("Escape");
      await expect(help, "Escape did not close the key card").toHaveCount(0);
      if (onChip) await expect(chip, "the key card left the focus off the chip").toBeFocused();
    }

    const palette = page.getByRole("dialog", { name: "Search" });
    const actions = palette.getByRole("group", { name: "Actions" });
    await leaveFocus(page);
    await page.keyboard.press("Control+k");
    await expect(palette, "Control+k did not open the palette").toBeVisible();
    for (const [label] of lines) {
      await expect(
        actions.getByRole("option", { name: label }),
        `the palette offers "${label}" with the focus off the chip`
      ).toHaveCount(0);
    }
    await page.keyboard.press("Escape");
    await expect(palette, "Escape did not close the palette").toHaveCount(0);
    await chip.focus();
    await expect(chip).toBeFocused();
    await page.keyboard.press("Control+k");
    for (const [label] of lines) {
      await expect(
        actions.getByRole("option", { name: label }),
        `the palette offers no "${label}" with the focus on the chip`
      ).toBeVisible();
    }
    await actions.getByRole("option", { name: lines[1][0] }).click();
    await expect(palette, "the palette's row did not close the palette").toHaveCount(0);
    const requestChanges = page.getByRole("dialog", { name: "Request changes" });
    await expect(requestChanges, "the palette's row opened no Request changes").toBeVisible();
    await page.keyboard.press("Escape");
    await expect(requestChanges, "Escape did not close Request changes").toHaveCount(0);
    await expect(chip, "Request changes did not give the focus back to the chip").toBeFocused();
  });
});

/** Where the focus is, as a reader of a screen reader hears it: `body` when no control holds it,
 *  and otherwise the focused element's tag and name, with whether the header holds it. */
function focusNow(page: Page): Promise<{ inHeader: boolean; onBody: boolean; what: string }> {
  return page.evaluate(() => {
    const active = document.activeElement;
    const header = document.querySelector("[data-issue-page] header[data-issue-header]");
    if (!(active instanceof HTMLElement) || active === document.body) {
      return { inHeader: false, onBody: true, what: "body" };
    }
    const name = (active.getAttribute("aria-label") ?? active.textContent ?? "").trim();
    return {
      inHeader: header?.contains(active) === true,
      onBody: false,
      what: `${active.tagName.toLowerCase()} ${name.slice(0, 40)}`,
    };
  });
}

// The palette's Close issue, run with the focus on the status, the priority or the assignee,
// closes the issue, and the close then refuses input on that control. A keyboard reader who ran
// it keeps a focus in the header rather than on the page's body, from which the next Tab starts
// again at the top of the page.
// SLOW(>=2s): 3,434 ms (one worker, Job e2e-rows-rdf2g at 43949bc0): three loads, one for each
// control, each after the issue is set back in progress through the API, and the palette's
// Close issue run from each and polled until Dispatch holds the close.
test("the palette's Close issue, run from a header control, leaves the focus in the header", async ({
  browser,
}) => {
  test.setTimeout(150_000);
  await phase(browser, seedHeader, async (page, seeded) => {
    const key = seeded.key;
    const header = issueHeader(page);
    for (const [name, control] of [
      ["the status", header.getByRole("combobox", { name: "Status" })],
      ["the priority", header.getByRole("combobox", { name: `Priority of ${key}` })],
      ["the assignee", header.getByRole("combobox", { name: `Assignee of ${key}` })],
    ] as const) {
      await patchIssue(key, { status: "in_progress" });
      await gotoIssue(page, `/issues/${key}`);
      await expectHeaderDrawn(page);
      await control.focus();
      await expect(control).toBeFocused();
      await page.keyboard.press("Control+k");
      const palette = page.getByRole("dialog", { name: "Search" });
      await expect(palette, "Control+k did not open the palette").toBeVisible();
      await palette
        .getByRole("group", { name: "Actions" })
        .getByRole("option", { name: "Close issue" })
        .click();
      await expect(palette, "the palette's row did not close the palette").toHaveCount(0);
      await expect
        .poll(async () => (await getIssue(key)).status, {
          message: `the palette's Close issue from ${name} did not close the issue`,
        })
        .toBe("done");
      await expect(header.getByText(STATE_LINES.closed, { exact: true })).toBeVisible();
      await framesDrawn(page, 2);
      const focus = await focusNow(page);
      expect(
        focus.onBody || !focus.inHeader,
        `the palette's Close issue from ${name} left the focus on ${focus.what}`
      ).toBe(false);
    }
  });
});

// A change request sent from the header - from its Request changes by Enter, or by Shift+R from
// its Approve - answers the request, so the header draws neither action any more, and the dialog
// gives the focus back to a button the header then removes. The focus stays in the header.
// SLOW(>=2s): 3,078 ms (one worker, Job e2e-rows-rdf2g at 43949bc0): two approval requests,
// each followed by a load, a change request typed and sent from the header, and a poll until
// Dispatch records it.
test("a change request sent from the header leaves the focus in the header", async ({
  browser,
}) => {
  test.setTimeout(150_000);
  await phase(browser, seedHeader, async (page, seeded) => {
    const header = issueHeader(page);
    const dialog = page.getByRole("dialog", { name: "Request changes" });
    for (const [from, open] of [
      [
        "its Request changes by Enter",
        async () => {
          const button = header.getByRole("button", { exact: true, name: "Request changes" });
          await button.focus();
          await page.keyboard.press("Enter");
        },
      ],
      [
        "Shift+R from its Approve",
        async () => {
          const button = header.getByRole("button", { exact: true, name: "Approve" });
          await button.focus();
          await page.keyboard.press("Shift+R");
        },
      ],
    ] as const) {
      await requestApproval(seeded.specArtifactId, { summary: "Read it." });
      await gotoIssue(page, `/issues/${seeded.key}`);
      await expectHeaderDrawn(page);
      await expect(
        header.getByRole("button", { exact: true, name: "Approve" }),
        "the chip carries no Approve while its request waits"
      ).toBeVisible();
      await open();
      await expect(dialog, `${from} opened no Request changes`).toBeVisible();
      // The dialog moves the focus into its text box a frame after it is drawn (`useDialog`).
      // Keys typed before then go to the header's control that still holds the focus: from
      // Approve, the space presses Approve and the request is approved, not answered with changes.
      await expect(
        dialog.getByRole("textbox", { name: "Reason" }),
        `Request changes opened from ${from} took no focus into its text box`
      ).toBeFocused();
      await page.keyboard.type("One change.");
      await dialog.getByRole("button", { exact: true, name: "Request changes" }).focus();
      await page.keyboard.press("Enter");
      await expect(dialog, `the change request sent from ${from} left its dialog open`).toHaveCount(
        0
      );
      await expect
        .poll(async () => (await getArtifact(seeded.specArtifactId)).approval?.state, {
          message: `the change request sent from ${from} was not recorded`,
        })
        .toBe("changes_requested");
      await expect(reviewActions(header), "the header still draws an action").toHaveCount(0);
      await framesDrawn(page, 2);
      const focus = await focusNow(page);
      expect(
        focus.onBody || !focus.inHeader,
        `the change request sent from ${from} left the focus on ${focus.what}`
      ).toBe(false);
    }
  });
});

// Approve pressed by Enter on the header's chip keeps the focus while its review is written:
// it is `aria-disabled` then and a press of it does nothing, so nothing moves the focus off it
// (decision 223). Once the approval is read the header draws no Approve, and the focus goes to
// the chip of the document the reader approved, not to another control of the header. The
// review's write is held open with a route so the row can read the focus while Approve refuses
// a press.
// SLOW(>=2s): 2,093 ms in one of three one-worker runs (Job e2e-rows-9whgn at 4d06932a), and 1,548
// to 1,657 ms in three (Job e2e-rows-2chx9 at 2e3f0319): one load after an approval request, the
// review's post held by a route while Approve is pressed by Enter and the focus is read after two
// frames, then the post let go and polled until Dispatch records the approval, and two frames
// before the focus is read again.
test("Approve by Enter on the header's chip leaves the focus on the approved chip", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  await phase(browser, seedHeader, async (page, seeded) => {
    await requestApproval(seeded.specArtifactId, { summary: "Read it." });
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectHeaderDrawn(page);
    const header = issueHeader(page);
    const approve = header.getByRole("button", { exact: true, name: "Approve" });
    await expect(approve, "the chip carries no Approve while its request waits").toBeVisible();
    const chip = approvalChip(page, seeded.specArtifactId);

    const review = await holdPosts(page, `**/api/v1/artifacts/${seeded.specArtifactId}/reviews`);

    await approve.focus();
    await expect(approve).toBeFocused();
    await page.keyboard.press("Enter");
    await expect.poll(() => review.posts(), { message: "Approve by Enter sent no review" }).toBe(1);
    await expect(
      approve,
      "Approve is not aria-disabled while its review is written"
    ).toHaveAttribute("aria-disabled", "true");
    await framesDrawn(page, 2);
    const written = await focusNow(page);
    await expect(
      approve,
      `while the review was written the focus was on ${written.what}, not on Approve`
    ).toBeFocused();

    review.release();
    await expect
      .poll(async () => (await getArtifact(seeded.specArtifactId)).approval?.state, {
        message: "Approve by Enter recorded no approval",
      })
      .toBe("approved");
    await expect(reviewActions(header), "the header still draws an action").toHaveCount(0);
    await framesDrawn(page, 2);
    const focus = await focusNow(page);
    await expect(
      chip,
      `Approve by Enter left the focus on ${focus.what}, not on the spec's chip`
    ).toBeFocused();
  });
});

// A change request sent by Enter from the header's Request changes answers the request, so the
// header draws neither action any more, and the dialog gives the focus back to Request changes,
// which the header then removes. The focus goes to the chip of the document the reader reviewed.
// SLOW(>=2s): 2,048 ms in one of three one-worker runs, Job e2e-rows-2chx9 at 2e3f0319 (1,602 and
// 1,657 ms in the others): one load after an approval request, the dialog opened by Enter and typed
// in, the change request sent and polled until Dispatch records it, and two frames before the focus
// is read.
test("a change request sent from the header's Request changes leaves the focus on its chip", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  await phase(browser, seedHeader, async (page, seeded) => {
    await requestApproval(seeded.specArtifactId, { summary: "Read it." });
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectHeaderDrawn(page);
    const header = issueHeader(page);
    const requestChanges = header.getByRole("button", { exact: true, name: "Request changes" });
    await expect(
      requestChanges,
      "the chip carries no Request changes while its request waits"
    ).toBeVisible();
    await requestChanges.focus();
    await expect(requestChanges).toBeFocused();
    await page.keyboard.press("Enter");
    const dialog = page.getByRole("dialog", { name: "Request changes" });
    await expect(dialog, "Enter on Request changes opened no dialog").toBeVisible();
    // The dialog moves the focus into its text box a frame after it is drawn (`useDialog`);
    // keys typed before then go to the header's Request changes.
    await expect(
      dialog.getByRole("textbox", { name: "Reason" }),
      "Request changes took no focus into its text box"
    ).toBeFocused();
    await page.keyboard.type("One change.");
    await dialog.getByRole("button", { exact: true, name: "Request changes" }).focus();
    await page.keyboard.press("Enter");
    await expect(dialog, "the change request left its dialog open").toHaveCount(0);
    await expect
      .poll(async () => (await getArtifact(seeded.specArtifactId)).approval?.state, {
        message: "the change request was not recorded",
      })
      .toBe("changes_requested");
    await expect(reviewActions(header), "the header still draws an action").toHaveCount(0);
    await framesDrawn(page, 2);
    const focus = await focusNow(page);
    await expect(
      approvalChip(page, seeded.specArtifactId),
      `the change request left the focus on ${focus.what}, not on the spec's chip`
    ).toBeFocused();
  });
});

// A change request sent from the header's Request changes after its request was answered while
// the dialog was open: the header drew neither action by then, so the dialog has no Request
// changes to give the focus back to as it shuts, and the focus falls from the dialog's own
// Request changes, which reads "Submitting…" while the change request is written. The focus goes
// to the chip of the document the reader reviewed. Bob answers the request through the API while
// the dialog is open; Dispatch then takes the change request as a review with no request open.
// SLOW(>=2s): 2,002 ms (one worker, Job e2e-rows-rdf2g at 43949bc0): one load, the dialog
// opened and typed in, bob's approval written through the API and taken in by the open page, and
// the change request sent and polled until Dispatch records it.
test("a change request sent after the header's actions went leaves the focus on its chip", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  await phase(browser, seedHeader, async (page, seeded) => {
    await requestApproval(seeded.specArtifactId, { summary: "Read it." });
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectHeaderDrawn(page);
    const header = issueHeader(page);
    const requestChanges = header.getByRole("button", { exact: true, name: "Request changes" });
    await expect(
      requestChanges,
      "the chip carries no Request changes while its request waits"
    ).toBeVisible();
    await requestChanges.focus();
    await expect(requestChanges).toBeFocused();
    await page.keyboard.press("Enter");
    const dialog = page.getByRole("dialog", { name: "Request changes" });
    await expect(dialog, "Enter on Request changes opened no dialog").toBeVisible();
    // The dialog moves the focus into its text box a frame after it is drawn (`useDialog`);
    // keys typed before then go to the header's Request changes.
    await expect(
      dialog.getByRole("textbox", { name: "Reason" }),
      "Request changes took no focus into its text box"
    ).toBeFocused();
    await page.keyboard.type("One change.");

    await reviewArtifact(seeded.specArtifactId, { state: "approved" }, { login: "bob" });
    // The dialog stands inside the header, so its own Request changes is one of the header's
    // buttons, but no action of the chip: `reviewActions` leaves out every button of a dialog.
    await expect(
      reviewActions(header),
      "the header still draws an action once bob answered the request"
    ).toHaveCount(0);
    await expect(dialog, "bob's answer shut the Request changes dialog").toBeVisible();

    const submit = dialog.getByRole("button", { exact: true, name: "Request changes" });
    await submit.focus();
    await expect(submit).toBeFocused();
    await page.keyboard.press("Enter");
    await expect(dialog, "the change request left its dialog open").toHaveCount(0);
    await expect
      .poll(async () => (await getArtifact(seeded.specArtifactId)).approval?.state, {
        message: "the change request was not recorded",
      })
      .toBe("changes_requested");
    await expect(reviewActions(header), "the header draws an action again").toHaveCount(0);
    await framesDrawn(page, 2);
    const focus = await focusNow(page);
    await expect(
      approvalChip(page, seeded.specArtifactId),
      `the change request left the focus on ${focus.what}, not on the spec's chip`
    ).toBeFocused();
  });
});

// Approve pressed by Enter, and then by Enter again after its review has landed but before the
// page has drawn the approval it reads back, sends one review: a shared control's write is out,
// and a press of it does nothing, until the new read is drawn (decision 223). The reads that
// redraw the chip - the issue's, which the header draws the approval from, and the document's -
// are held with a route, so the second press falls in that gap: the review's answer has reached
// the page and the server holds the approval, while the header still draws Approve. The second
// Enter is pressed where the focus is, as a keyboard reader's next Enter is, and the rule keeps
// the focus on Approve while its write is out: the row reads that it is there, so the press
// reaches Approve's own guard and not another control. The focus is read in the header in the
// gap, and again once the reads go on and the header draws neither action.
// SLOW(>=2s): 2,000 ms in one of three one-worker runs (Job e2e-rows-9whgn at 4d06932a), and 1,403
// to 1,983 ms in three (Job e2e-rows-2chx9 at 2e3f0319): one load after an approval request, the
// issue's and the document's reads held by a route while the review is posted and answered, the
// reads polled until the page makes one, two presses by Enter each followed by two frames, and the
// reads let go and the redraw waited on.
test("Approve pressed again by Enter before its approval is drawn sends no second review", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  await phase(browser, seedHeader, async (page, seeded) => {
    await requestApproval(seeded.specArtifactId, { summary: "Read it." });
    const reviewed = reviewsSent(page);
    const reads = await holdReviewReads(page, seeded.key, seeded.specArtifactId);
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectHeaderDrawn(page);
    const header = issueHeader(page);
    const approve = header.getByRole("button", { exact: true, name: "Approve" });
    await expect(approve, "the chip carries no Approve while its request waits").toBeVisible();

    await approve.focus();
    await expect(approve).toBeFocused();
    reads.hold();
    const answered = answeredPost(page, `/api/v1/artifacts/${seeded.specArtifactId}/reviews`, 201);
    await page.keyboard.press("Enter");
    await (await answered).finished();
    expect(
      (await getArtifact(seeded.specArtifactId)).approval?.state,
      "Approve by Enter recorded no approval"
    ).toBe("approved");
    await expect
      .poll(() => reads.issueReadsHeld(), {
        message: "the page made no read of the issue after the review landed",
      })
      .toBeGreaterThan(0);
    await expect(approve, "the header drew the approval while its reads were held").toBeVisible();
    await framesDrawn(page, 2);
    const pressedOn = await focusNow(page);
    await expect(
      approve,
      `in the gap the focus was on ${pressedOn.what}, not on Approve`
    ).toBeFocused();
    await page.keyboard.press("Enter");
    await framesDrawn(page, 2);
    expect(reviewed, "Approve pressed again by Enter in the gap sent another review").toEqual([
      seeded.specArtifactId,
    ]);
    const inGap = await focusNow(page);
    expect(
      inGap.onBody || !inGap.inHeader,
      `Approve pressed again in the gap left the focus on ${inGap.what}`
    ).toBe(false);

    reads.release();
    await expect(reviewActions(header), "the header still draws an action").toHaveCount(0);
    await framesDrawn(page, 2);
    const drawn = await focusNow(page);
    expect(
      drawn.onBody || !drawn.inHeader,
      `once the header drew the approval the focus was on ${drawn.what}`
    ).toBe(false);
    expect(reviewed, "Approve pressed again in the gap sent another review later").toEqual([
      seeded.specArtifactId,
    ]);
  });
});

// The Request changes dialog's own Request changes, pressed by Enter, and then by Enter again
// after the change request has landed but before the page has drawn it, sends one review
// (decision 223, as for Approve above). The reads are held the same way. The second Enter is
// pressed wherever the focus is then, as a keyboard reader's next Enter is: the rule keeps the
// focus on the dialog's own button while its write is out, and the dialog's one hand-over of the
// focus waits for the new read. Once the reads go on, the dialog is shut, the header draws
// neither action, and the focus is in the header.
// SLOW(>=2s): 2,256 ms (one worker, Job e2e-rows-rdf2g at 43949bc0): one load, the dialog
// opened and typed in, the change request sent while a route holds the page's reads, the focus
// read three times after two frames each, and the redraw waited on once the reads go on.
test("Request changes in its dialog pressed again by Enter before the change request is drawn sends no second review", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  await phase(browser, seedHeader, async (page, seeded) => {
    await requestApproval(seeded.specArtifactId, { summary: "Read it." });
    const reviewed = reviewsSent(page);
    const reads = await holdReviewReads(page, seeded.key, seeded.specArtifactId);
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectHeaderDrawn(page);
    const header = issueHeader(page);
    const requestChanges = header.getByRole("button", { exact: true, name: "Request changes" });
    await expect(
      requestChanges,
      "the chip carries no Request changes while its request waits"
    ).toBeVisible();
    await requestChanges.focus();
    await expect(requestChanges).toBeFocused();
    await page.keyboard.press("Enter");
    const dialog = page.getByRole("dialog", { name: "Request changes" });
    await expect(dialog, "Enter on Request changes opened no dialog").toBeVisible();
    // The dialog moves the focus into its text box a frame after it is drawn (`useDialog`);
    // keys typed before then go to the header's Request changes.
    await expect(
      dialog.getByRole("textbox", { name: "Reason" }),
      "Request changes took no focus into its text box"
    ).toBeFocused();
    await page.keyboard.type("One change.");
    const submit = dialog.getByRole("button", { exact: true, name: "Request changes" });
    await submit.focus();
    await expect(submit).toBeFocused();

    reads.hold();
    const answered = answeredPost(page, `/api/v1/artifacts/${seeded.specArtifactId}/reviews`, 201);
    await page.keyboard.press("Enter");
    await (await answered).finished();
    expect(
      (await getArtifact(seeded.specArtifactId)).approval?.state,
      "the change request was not recorded"
    ).toBe("changes_requested");
    await expect
      .poll(() => reads.issueReadsHeld(), {
        message: "the page made no read of the issue after the change request landed",
      })
      .toBeGreaterThan(0);
    await expect(
      header.getByRole("button", { exact: true, name: "Approve" }),
      "the header drew the change request while its reads were held"
    ).toBeVisible();
    await framesDrawn(page, 2);
    const pressedOn = await focusNow(page);
    // This only names the dialog's state in the messages below; no step of the row depends on it.
    const dialogOpen = await dialog.isVisible();
    const where = `${pressedOn.what}, with the dialog ${dialogOpen ? "open" : "shut"}`;
    await page.keyboard.press("Enter");
    await framesDrawn(page, 2);
    expect(reviewed, `Enter pressed again in the gap, on ${where}, sent another review`).toEqual([
      seeded.specArtifactId,
    ]);
    const inGap = await focusNow(page);
    expect(
      inGap.onBody || !inGap.inHeader,
      `Enter pressed again in the gap, on ${where}, left the focus on ${inGap.what}`
    ).toBe(false);

    reads.release();
    await expect(
      dialog,
      `the dialog is open once the change request was drawn: the Enter in the gap was on ${where}`
    ).toHaveCount(0);
    await expect(reviewActions(header), "the header still draws an action").toHaveCount(0);
    await framesDrawn(page, 2);
    const drawn = await focusNow(page);
    expect(
      drawn.onBody || !drawn.inHeader,
      `once the header drew the change request the focus was on ${drawn.what}`
    ).toBe(false);
    expect(reviewed, "Enter pressed again in the gap sent another review later").toEqual([
      seeded.specArtifactId,
    ]);
  });
});

// Copy issue key, the identity row's copy button, pressed by Enter and then by Enter again while
// its write is out makes one write (decision 223). Its write is the clipboard's: `copyText`
// awaits `navigator.clipboard.writeText`, so the write is out from the press until that promise
// settles, which is the write landing. The new read is the "Copied" the button draws from the
// status the settled write sets, and the button's one hand-over of the focus (`CopyButton`'s
// effect on that status) runs as it is drawn: no frame falls between the two for a press to land
// in. So a press made while the clipboard holds the write must make no write, and a press made
// once "Copied" is drawn, inside its 1.5 s, is a copy of its own and makes its one write: the
// write before it has ended, and "Copied" is what the button read back from it. The clipboard is
// replaced, before the page loads, by one that counts its writes and holds each until the row
// lets it land. Nothing disables the button, so the focus stays on it throughout.
// SLOW(>=2s): 2,005 ms in one of three one-worker runs, Job e2e-rows-2chx9 at 2e3f0319 (1,475 and
// 1,765 ms in the others): one load with a clipboard that holds its writes, three presses by Enter
// each followed by two frames or a poll of the clipboard, and the held write let land in between.
test("Copy issue key pressed again by Enter while its write is out makes no second write", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  await phase(browser, seedHeader, async (page, seeded) => {
    const clipboard = await holdClipboard(page);
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectHeaderDrawn(page);
    const header = issueHeader(page);
    const copy = header.getByRole("button", { name: new RegExp(`^Copy issue key ${seeded.key} `) });
    const copied = header.getByText("Copied", { exact: true });
    await copy.focus();
    await expect(copy).toBeFocused();

    await page.keyboard.press("Enter");
    await expect
      .poll(() => clipboard.writes(), {
        message: "Enter on Copy issue key wrote nothing to the clipboard",
      })
      .toBe(1);
    await framesDrawn(page, 2);
    await page.keyboard.press("Enter");
    await framesDrawn(page, 2);
    expect(
      await clipboard.writes(),
      "Copy issue key, pressed again while its write was out, wrote to the clipboard again"
    ).toBe(1);
    await expect(copied, "Copy issue key drew Copied while its write was held").toHaveCount(0);
    const whileOut = await focusNow(page);
    await expect(
      copy,
      `while the copy's write was out the focus was on ${whileOut.what}`
    ).toBeFocused();

    await clipboard.land();
    await expect(copied, "Copy issue key drew no Copied once its write landed").toBeVisible();
    const landed = await focusNow(page);
    await expect(copy, `once Copied was drawn the focus was on ${landed.what}`).toBeFocused();

    // A press made while Copied shows is a copy of its own (decision 255): Copied describes the
    // newest copy only, so it goes while that write is held and is drawn again once it lands.
    expect(await copied.isVisible(), "the press made for Copied's time came after it").toBe(true);
    await page.keyboard.press("Enter");
    await expect
      .poll(() => clipboard.writes(), {
        message: "Copy issue key, pressed while Copied showed, wrote nothing",
      })
      .toBe(2);
    await expect(copied, "Copy issue key drew Copied while its second write was held").toHaveCount(
      0
    );
    await clipboard.land();
    await expect(
      copied,
      "Copy issue key drew no Copied once its second write landed"
    ).toBeVisible();
    await framesDrawn(page, 2);
    const after = await focusNow(page);
    await expect(
      copy,
      `after the copy made inside Copied's time the focus was on ${after.what}`
    ).toBeFocused();
  });
});

// A write the server refuses draws its line under the header with Retry: the pin's, from the
// menu, and the issue's, from the title. Retry by Enter keeps the focus on that Retry while the
// write runs again, so a disabled Retry whose focus a fallback took is a failure here too; once
// the write lands and the line goes, the focus is still in the header.
// SLOW(>=2s): 3,035 ms (one worker, Job e2e-rows-rdf2g at 43949bc0): two loads, each with a
// write a route refuses and then holds - the pin's from the menu, the title's from its editor -
// and the focus read after two frames while Retry's write runs and again once it lands.
test("a keyboard Retry of a refused write keeps the focus in the header", async ({ browser }) => {
  test.setTimeout(150_000);
  await phase(browser, seedHeader, async (page, seeded) => {
    const key = seeded.key;
    const header = issueHeader(page);
    for (const [line, address, method, refuse] of [
      [
        "the pin's",
        `**/api/v1/me/issues/${key}/state`,
        "PUT",
        async () => {
          const trigger = header.getByRole("button", {
            exact: true,
            name: CONTROL_LABELS.issue_actions,
          });
          await trigger.focus();
          await page.keyboard.press("Enter");
          await header.getByRole("button", { name: "Pin issue" }).focus();
          await page.keyboard.press("Enter");
        },
      ],
      [
        "the issue's",
        `**/api/v1/issues/${key}`,
        "PATCH",
        async () => {
          const heading = header.getByRole("heading", { level: 1 });
          await heading.focus();
          await page.keyboard.press("e");
          await expect(header.getByRole("textbox", { name: "Issue title" })).toBeFocused();
          await page.keyboard.press("End");
          await page.keyboard.type(" again");
          await page.keyboard.press("Enter");
        },
      ],
    ] as const) {
      await gotoIssue(page, `/issues/${key}`);
      await expectHeaderDrawn(page);
      // Every write is refused until Retry is pressed; the Retry's write is held open, so the
      // focus is read while it runs, and then let through.
      let refusing = true;
      const held = Promise.withResolvers<void>();
      const retried = Promise.withResolvers<void>();
      await page.route(address, async (route) => {
        if (route.request().method() !== method) {
          await route.continue();
          return;
        }
        if (refusing) {
          await route.fulfill({
            body: JSON.stringify({ code: "INVALID_INPUT", error: "Dispatch refused the write" }),
            contentType: "application/json",
            status: 400,
          });
          return;
        }
        retried.resolve();
        await held.promise;
        await route.continue();
      });
      await refuse();
      const retry = header.getByRole("button", { name: "Retry" });
      await expect(retry, `${line} refused write drew no Retry`).toBeVisible();
      await retry.focus();
      refusing = false;
      await page.keyboard.press("Enter");
      await retried.promise;
      await framesDrawn(page, 2);
      await expect(
        retry,
        `${line} Retry did not keep the focus while its write ran again`
      ).toBeFocused();
      const whileRetrying = await focusNow(page);
      expect(
        whileRetrying.onBody || !whileRetrying.inHeader,
        `${line} Retry, while its write ran again, left the focus on ${whileRetrying.what}`
      ).toBe(false);
      held.resolve();
      await expect(retry, `${line} line stayed after its write landed`).toHaveCount(0);
      await framesDrawn(page, 2);
      const landed = await focusNow(page);
      expect(
        landed.onBody || !landed.inHeader,
        `${line} line went and left the focus on ${landed.what}`
      ).toBe(false);
      await page.unroute(address);
    }
  });
});

// `Shift+P` presses the pin while the menu is shut, so it opens nothing and closes nothing: the
// focus stays where the reader had it, on the page's body or on the title's heading, and never
// jumps to the menu's control.
// SLOW(>=2s): 2,489 ms with four workers (CI run 44646643988), 1,572 ms on one worker (Job
// e2e-rows-rdf2g at 43949bc0): one load, then a pin and an unpin by Shift+P, each read back on
// the pin and followed by two frames before the focus is read.
test("Shift+P pins and unpins and leaves the focus where it was", async ({ browser }) => {
  test.setTimeout(120_000);
  await phase(browser, seedHeader, async (page, seeded) => {
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectHeaderDrawn(page);
    const header = issueHeader(page);
    const pin = header.locator('[data-issue-action="pin"]');
    await expect(pin).toHaveAttribute("aria-pressed", "false");

    await leaveFocus(page);
    await page.keyboard.press("Shift+P");
    await expect(pin, "Shift+P did not pin the issue").toHaveAttribute("aria-pressed", "true");
    await framesDrawn(page, 2);
    const fromBody = await focusNow(page);
    expect(
      fromBody.onBody,
      `Shift+P from the page's body moved the focus to ${fromBody.what}`
    ).toBe(true);

    const heading = header.getByRole("heading", { level: 1 });
    await heading.focus();
    await expect(heading).toBeFocused();
    await page.keyboard.press("Shift+P");
    await expect(pin, "Shift+P did not unpin the issue").toHaveAttribute("aria-pressed", "false");
    await framesDrawn(page, 2);
    await expect(heading, "Shift+P from the title's heading moved the focus").toBeFocused();
  });
});

// On a gated root whose Verification document's and spec's requests both wait on the reader, `a`
// on the spec's entry - the line's last entry, after the Verification document's - approves the
// spec alone: it presses the Approve of the focused entry, never the first Approve on the line.
test("an approval entry's a approves its own document when an earlier entry also waits", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  await phase(
    browser,
    async () => {
      const last = rowKey("LAST");
      await createProject({ key: last, name: "Last entry" });
      await setGatedSteps(last, ["decisions", "criteria", "spec"]);
      const root = await createIssue({ project: last, spec: "The plan.", title: "Gated root" });
      const verification = await createIssueArtifact(root.key, {
        content: "# Verification\n\nOne check.\n",
        name: "verification.md",
      });
      const spec = root.primary_artifact_id ?? "";
      await requestApproval(spec, { summary: "Read it." });
      await requestApproval(verification.artifact.id, { summary: "Read it." });
      return { key: root.key, spec, verification: verification.artifact.id };
    },
    async (page, seeded) => {
      const reviewed = reviewsSent(page);
      await gotoIssue(page, `/issues/${seeded.key}`);
      await expectHeaderDrawn(page);
      await expect(waitingCount(page)).toHaveText("Waiting on you (2)");
      const chip = approvalEntry(page, "spec").locator("[data-approval-artifact]");
      await expect(chip, "the spec's entry draws another document's chip").toHaveAttribute(
        "data-approval-artifact",
        seeded.spec
      );
      await chip.focus();
      await expect(chip).toBeFocused();
      await page.keyboard.press("a");
      await expect
        .poll(async () => (await getArtifact(seeded.spec)).approval?.state, {
          message: "a on the spec's entry recorded no approval of the spec",
        })
        .toBe("approved");
      expect(reviewed, "a on the spec's entry reviewed another document").toEqual([seeded.spec]);
      expect(
        (await getArtifact(seeded.verification)).approval?.state,
        "a on the spec's entry answered the Verification document's request"
      ).toBe("awaiting");
      await expect(waitingCount(page)).toHaveText("Waiting on you (1)");
    }
  );
});
