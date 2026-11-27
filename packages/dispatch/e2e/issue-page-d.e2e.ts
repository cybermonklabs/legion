import { ISSUE_TAB_LABELS } from "../web/src/features/issue/issue-tab-list";
import { HEADER_SCROLLS_QUERY } from "../web/src/features/issue-page/identity-block";
import { presetPath } from "../web/src/features/issue-page/issue-address";
import { PRESET_NAMES, PRESET_ORDER, type Preset } from "../web/src/features/issue-page/page-words";
import {
  createAsk,
  createIssue,
  createIssueArtifact,
  createProject,
  createProjectDocument,
} from "./api";
import {
  closeDetails,
  contentsPane,
  detailsClose,
  filterRow,
  gotoIssue,
  iconRail,
  issuePage,
  issueRead,
  marginResizeHandle,
  marginShell,
  openDetails,
  openMenu,
  outlineControl,
  outlineSheet,
  phase,
  presetChooser,
  presetCount,
  presetMenuItem,
  presetRow,
  presetToggle,
  presetToggles,
  presetToolbar,
  viewportAt,
} from "./issue-page";
import { expectOpeningLanded, focusThePage } from "./issue-page-g";
import { withIssuePage } from "./issue-page-option";
import { expect, type Locator, type Page, type Route, test } from "./test";
import { assertPageFits, assertWhole } from "./unclipped";
import { asUser } from "./users";

/**
 * The new issue page's frame (part D): the icon
 * rail, Details over the page, the Contents pane and the Outline sheet, the view row and its
 * chords, the frame's skeleton, and the guards that today's shell is what every other reader
 * and every other page still gets. Every row but the preference-off guard turns the new page on
 * (`phase`, `withIssuePage`).
 */

/** Two statements, so Spec counts two, and one check, so Verification counts one. */
const SPEC = `:::requirement{#r1}
The frame keeps the page's width.
:::

:::requirement{#r2}
The view row is one line at every width.
:::
`;

/** What the seed below gives each control of the view row to count: no question and no thread
 *  of the reader's, two statements, one check, no plan and no decision. */
const SEEDED_COUNTS: Readonly<Record<Preset, number>> = {
  conversation: 0,
  decisions: 0,
  plan: 0,
  spec: 2,
  verification: 1,
};

/** An agent, whose question waits on the reader. */
const AGENT = { actor: { id: "e2e-frame-agent", kind: "session" as const }, as: "agent" as const };

/** One issue with a spec of two statements and a Verification document of one check, and beside
 *  it a second issue whose question waits on alice, FRM's default assignee, so the rail's Inbox
 *  has a count to draw. */
async function seedFrame(): Promise<{ key: string }> {
  await createProject({ key: "FRM", name: "Frame" });
  const issue = await createIssue({ project: "FRM", spec: SPEC, title: "The new page's frame" });
  await createIssueArtifact(issue.key, {
    content: `:::check{#c1 serves="dispatch://${issue.key}/spec#b-r1" surface="browser" run="bun run e2e -- e2e/issue-page-d.e2e.ts"}
The frame keeps the page's width.
:::
`,
    name: "verification.md",
  });
  const waiting = await createIssue({ project: "FRM", title: "A question for the reader" });
  await createAsk(waiting.key, { question: "Is the frame ready?" }, AGENT);
  return { key: issue.key };
}

/** The widths R10 of the part holds the frame to. */
const WIDTHS = [320, 390, 768, 1024, 1280, 1440] as const;

/** R10's widths with 390 px first, the width the view row's negative control names. */
const PHONE_FIRST = [390, ...WIDTHS.filter((width) => width !== 390)] as const;

/** The column that holds the stage and the foot, inside its insets. */
async function stageWidth(page: Page): Promise<number> {
  return page.getByTestId("issue-column").evaluate((column) => {
    const style = getComputedStyle(column);
    return (
      column.clientWidth -
      Number.parseFloat(style.paddingLeft) -
      Number.parseFloat(style.paddingRight)
    );
  });
}

/** `locator`'s box once it is drawn, failing the row where it is never drawn. */
async function boxOf(
  locator: Locator,
  what: string
): Promise<{
  height: number;
  width: number;
  x: number;
  y: number;
}> {
  await expect(locator, `${what} is not drawn`).toBeVisible();
  // The wait above drew the element, and a drawn element has a box, so the box is there.
  const box = await locator.boundingBox();
  if (box === null) throw new Error(`${what} draws no box`);
  return box;
}

/** Fails the row where `locator`'s own box scrolls sideways: its content wider than its box. A
 *  box whose overflow scrolls is checked itself, which `assertWhole` does not do for an empty
 *  box or for a box under its first leaf. */
async function assertNoSidewaysScroll(locator: Locator, what: string): Promise<void> {
  const { client, scroll } = await locator.evaluate((element) => ({
    client: element.clientWidth,
    scroll: element.scrollWidth,
  }));
  expect(
    scroll,
    `${what} scrolls sideways: ${scroll} px of content in ${client} px`
  ).toBeLessThanOrEqual(client);
}

// c-frame (1): the frame's half of the checkpoint's "one panel of properties holds no
// conversation list @phone".
test("the frame draws an icon rail, Details over the page and the Contents pane @phone", async ({
  browser,
}) => {
  test.setTimeout(180_000);
  await phase(browser, seedFrame, async (page, { key }) => {
    await gotoIssue(page, `/issues/${key}`);

    // The rail: 56 px at the left edge, the window's height, R2's links in order, no tooltip, and
    // no sidebar beside it.
    await expect(iconRail(page), "the new page draws no icon rail").toHaveCount(1);
    const rail = await boxOf(iconRail(page), "the rail");
    expect(rail.x, "the rail is not at the left edge").toBe(0);
    expect(rail.width, "the rail is not 56 px wide").toBe(56);
    expect(rail.height, "the rail is not the window's height").toBe(900);
    // The Inbox link names its count: the seed's question for alice.
    await expect
      .poll(
        async () =>
          (await iconRail(page).getByRole("link").nth(1).getAttribute("aria-label")) ?? "",
        { message: "the rail's second link is not the Inbox with its count" }
      )
      .toMatch(/^Inbox [1-9]\d*$/);
    const links = await iconRail(page)
      .getByRole("link")
      .evaluateAll((all) => all.map((link) => link.getAttribute("aria-label") ?? ""));
    expect(
      [links[0], ...links.slice(2)],
      "the rail's links are not Dispatch, Inbox, Agents, Priorities, Delivery, Pipeline, the project and Settings"
    ).toEqual(["Dispatch", "Agents", "Priorities", "Delivery", "Pipeline", "FRM", "Settings"]);
    await expect(iconRail(page).getByRole("link", { exact: true, name: "FRM" })).toHaveAttribute(
      "aria-current",
      "true"
    );
    expect(await iconRail(page).locator("[title]").count(), "a rail entry carries a title").toBe(0);
    await expect(
      page.getByRole("complementary", { exact: true, name: "Navigation" }),
      "the new page draws the sidebar beside its rail"
    ).toHaveCount(0);
    // Its foot opens the whole navigation over the page, and Escape closes it.
    await iconRail(page).getByRole("button", { exact: true, name: "Open navigation" }).click();
    const navigation = page.getByRole("dialog", { exact: true, name: "Navigation" });
    await expect(navigation, "the rail's foot opens no navigation").toBeVisible();
    await page.keyboard.press("Escape");
    await expect(navigation).toBeHidden();
    // `Shift+S`, which folds today's sidebar, does nothing in the frame: no sidebar, the rail, and
    // the reader's stored sidebar unchanged, which the Inbox, in today's shell, draws open.
    await focusThePage(page);
    await page.keyboard.press("Shift+S");
    await expect(
      page.getByRole("complementary", { exact: true, name: "Navigation" }),
      "Shift+S drew the sidebar in the frame"
    ).toHaveCount(0);
    await expect(iconRail(page), "Shift+S took the rail away").toHaveCount(1);
    await iconRail(page).getByRole("link", { exact: true, name: "Dispatch" }).click();
    await expect(
      page.getByRole("complementary", { exact: true, name: "Navigation" }),
      "Shift+S in the frame folded the sidebar of today's pages"
    ).toBeVisible();
    await expect(page.getByTestId("sidebar-rail")).toHaveCount(0);
    await page.goBack();
    if (!page.url().includes(`/issues/${key}`)) await page.goBack();
    await issueRead(page);
    await expectOpeningLanded(page);

    // The page's column: the window less the rail.
    const column = await boxOf(issuePage(page), "the page");
    expect(column.x, "the page does not start at the rail's edge").toBe(56);
    expect(column.width, "the page's column is not 1384 px wide").toBe(1384);

    // Details: hidden at the load; `Shift+M` opens it over the right of the page, the focus on
    // its Close, with the prototype's edge shadow, no tab and no shade, the page as wide as it was.
    await expect(marginShell(page), "Details is open on a fresh load").toBeHidden();
    await presetToggle(page, "conversation").focus();
    await page.keyboard.press("Shift+M");
    await expect(marginShell(page), "Shift+M did not open Details").toBeVisible();
    const drawer = await boxOf(marginShell(page), "Details");
    expect(Math.round(drawer.x), "Details does not open against the right edge").toBe(1056);
    expect(Math.round(drawer.width), "Details does not open 384 px wide").toBe(384);
    await expect(detailsClose(page), "opening Details left the focus off its Close").toBeFocused();
    expect(
      await marginShell(page).evaluate((element) => getComputedStyle(element).boxShadow),
      "Details casts no edge shadow"
    ).toContain("rgba(28, 25, 23, 0.14) -12px 0px 30px");
    await expect(marginShell(page).getByRole("tab"), "Details draws a tab").toHaveCount(0);
    await expect(
      page.getByTestId("details-shade"),
      "Details shades the page at 1440 px"
    ).toBeHidden();
    expect((await boxOf(issuePage(page), "the page")).width, "Details took a column").toBe(1384);
    // Escape closes it and gives the focus back to where it was taken from.
    await page.keyboard.press("Escape");
    await expect(marginShell(page), "Escape did not close Details").toBeHidden();
    await expect(presetToggle(page, "conversation")).toBeFocused();
    // With the list of keys open over it, Escape closes the list alone.
    await openDetails(page);
    await page.keyboard.press("?");
    const keys = page.getByRole("dialog", { name: "Keyboard shortcuts" });
    await expect(keys).toBeVisible();
    await page.keyboard.press("Escape");
    await expect(keys).toBeHidden();
    await expect(marginShell(page), "Escape closed Details under the list of keys").toBeVisible();

    // Its handle widens it, and a reload keeps the width.
    const handle = await boxOf(marginResizeHandle(page), "Details' handle");
    await page.mouse.move(handle.x + 2, handle.y + 2);
    await page.mouse.down();
    await page.mouse.move(handle.x - 120, handle.y + 2);
    await page.mouse.up();
    const resized = (await boxOf(marginShell(page), "Details")).width;
    expect(resized, "dragging the handle did not widen Details").toBeGreaterThan(384 + 50);
    await page.reload();
    await issueRead(page);
    await expectOpeningLanded(page);
    await expect(marginShell(page), "a load opened Details").toBeHidden();
    await openDetails(page);
    expect((await boxOf(marginShell(page), "Details")).width, "the width was not kept").toBeCloseTo(
      resized,
      0
    );
    // The handle moves by the arrow keys too: ArrowLeft widens Details, ArrowRight narrows it.
    await marginResizeHandle(page).focus();
    await page.keyboard.press("ArrowLeft");
    await expect
      .poll(async () => (await boxOf(marginShell(page), "Details")).width, {
        message: "ArrowLeft on the handle did not widen Details",
      })
      .toBeGreaterThan(resized + 1);
    await page.keyboard.press("ArrowRight");
    await expect
      .poll(async () => (await boxOf(marginShell(page), "Details")).width, {
        message: "ArrowRight on the handle did not narrow Details",
      })
      .toBeCloseTo(resized, 0);

    // Leaving the issue and coming back finds it closed.
    await iconRail(page)
      .getByRole("link", { name: /^Inbox/ })
      .click();
    // The Inbox selects its one entry, so its address may carry `?item=`.
    await expect(page).toHaveURL(/^https?:\/\/[^/]+\/(\?[^/]*)?$/);
    await page.goBack();
    // Where the Inbox pushed its selection as an entry of its own, the issue is one more back.
    if (!page.url().includes(`/issues/${key}`)) await page.goBack();
    await issueRead(page);
    await expect(marginShell(page), "coming back to the issue found Details open").toBeHidden();

    // The pane: 260 px against the rail with no view on; under a view, no pane and the stage
    // has the column's 1328 px.
    const pane = await boxOf(contentsPane(page), "the Contents pane");
    expect(pane.x, "the pane is not against the rail").toBe(56);
    expect(pane.width, "the pane is not 260 px wide").toBe(260);
    expect(await stageWidth(page), "the stage beside the pane is not 1068 px").toBe(1068);
    // It is sticky under the identity's height and as tall as the window less that height.
    const paneFit = await contentsPane(page).evaluate((element) => {
      const style = getComputedStyle(element);
      return {
        height: element.getBoundingClientRect().height,
        position: style.position,
        room: window.innerHeight - Number.parseFloat(style.top),
      };
    });
    expect(paneFit.position, "the pane is not sticky").toBe("sticky");
    expect(
      Math.round(paneFit.height),
      "the pane is not the window's height less the identity's"
    ).toBe(Math.round(paneFit.room));
    // At 1240 px and below it is 240 px wide.
    await page.setViewportSize(viewportAt(1240));
    await expect
      .poll(async () => (await boxOf(contentsPane(page), "the Contents pane")).width, {
        message: "the pane is not 240 px wide at 1240 px",
      })
      .toBe(240);
    await page.setViewportSize(viewportAt(1440));
    await gotoIssue(page, presetPath(key, "spec"));
    await expect(contentsPane(page), "a view draws the pane").toHaveCount(0);
    expect(await stageWidth(page), "the stage under a view is not 1328 px").toBe(1328);

    // The rail follows `md`: drawn at 768 px, not at 767 px, where the compact bar's "Open
    // navigation" is not drawn either: the one at 768 px is the rail's own.
    await page.setViewportSize(viewportAt(768));
    await expect(iconRail(page), "no rail at 768 px").toHaveCount(1);
    await expect(page.getByRole("button", { exact: true, name: "Open navigation" })).toHaveCount(1);
    await page.setViewportSize(viewportAt(767));
    await expect(iconRail(page), "a rail at 767 px").toHaveCount(0);
    // `md` is in `rem`, as the page's room for the rail (`md:pl-14`) is: at a 20 px default font
    // it is 960 px, so there is no rail at 900 px and the page is no wider than the window.
    const cdp = await page.context().newCDPSession(page);
    await cdp.send("Page.setFontSizes", { fontSizes: { standard: 20 } });
    try {
      await page.setViewportSize(viewportAt(900));
      await gotoIssue(page, presetPath(key, "spec"));
      await expect(iconRail(page), "a rail at 900 px under a 20 px default font").toHaveCount(0);
      await assertPageFits(page);
      await page.setViewportSize(viewportAt(1000));
      await expect(iconRail(page), "no rail at 1000 px under a 20 px default font").toHaveCount(1);
    } finally {
      await cdp.send("Page.setFontSizes", { fontSizes: { standard: 16 } });
      await cdp.detach();
    }
    // Below 1280 px the frame lifts the page's scroll padding for the compact bar it does not
    // draw.
    await page.setViewportSize(viewportAt(1279));
    await gotoIssue(page, `/issues/${key}`);
    expect(
      await page.evaluate(() => getComputedStyle(document.documentElement).scrollPaddingBottom),
      "the frame keeps today's scroll padding at 1279 px"
    ).toBe("0px");

    // At 390 px: no rail; Details a dialog the window's width that holds the focus, over the
    // shade; the Outline control opens the Contents sheet 56 px from the top.
    await page.setViewportSize(viewportAt(390));
    await gotoIssue(page, `/issues/${key}`);
    await expect(iconRail(page), "the phone draws the rail").toHaveCount(0);
    // The header's own "Open navigation" is the frame's; one outside the header is
    // today's compact bar's.
    const openNavigation = { exact: true, name: "Open navigation" };
    await expect(
      page.locator("[data-issue-header]").getByRole("button", openNavigation),
      "the header draws no navigation control at 390 px"
    ).toHaveCount(1);
    await expect
      .poll(
        () =>
          page
            .getByRole("button", openNavigation)
            .evaluateAll(
              (all) => all.filter((one) => one.closest("[data-issue-header]") === null).length
            ),
        { message: "the frame draws today's compact bar at 390 px" }
      )
      .toBe(0);
    await expectOpeningLanded(page);
    await focusThePage(page);
    await page.keyboard.press("Shift+M");
    const phoneDetails = page.getByRole("dialog", { exact: true, name: "Details" });
    await expect(phoneDetails, "Details is no dialog on the phone").toBeVisible();
    expect(Math.round((await boxOf(phoneDetails, "Details")).width)).toBe(390);
    await expect(detailsClose(page)).toBeFocused();
    const shade = page.getByTestId("details-shade");
    await expect(shade, "Details draws no shade on the phone").toBeVisible();
    expect(await shade.evaluate((element) => getComputedStyle(element).backgroundColor)).toBe(
      "rgba(28, 25, 23, 0.25)"
    );
    // The focus stays inside it: a Tab past its last control comes back into it.
    for (let press = 0; press < 12; press += 1) {
      await page.keyboard.press("Tab");
      expect(
        await phoneDetails.evaluate((element) => element.contains(document.activeElement)),
        "a Tab took the focus out of the phone's Details"
      ).toBe(true);
    }
    await page.keyboard.press("Escape");
    await expect(phoneDetails).toBeHidden();
    // `Shift+M` opens it and closes it on the phone too.
    await focusThePage(page);
    await page.keyboard.press("Shift+M");
    await expect(phoneDetails).toBeVisible();
    await expect(detailsClose(page)).toBeFocused();
    await page.keyboard.press("Shift+M");
    await expect(phoneDetails, "Shift+M did not close the phone's Details").toBeHidden();
    await outlineControl(page).click();
    const sheet = page.getByRole("dialog", { exact: true, name: "Contents" });
    await expect(sheet).toBeVisible();
    await expect(outlineSheet(page)).toHaveCount(1);
    expect(Math.round((await boxOf(sheet, "the Outline sheet")).y)).toBe(56);
    await sheet.getByRole("button", { exact: true, name: "Close" }).click();
    await expect(sheet).toBeHidden();
    await outlineControl(page).click();
    await expect(sheet).toBeVisible();
    await page.keyboard.press("Escape");
    await expect(sheet).toBeHidden();

    // At each of R10's widths, Details closed and open, nothing is wider than the window, the
    // rail and the view row are whole, and no scrolling box of the frame scrolls sideways.
    for (const width of WIDTHS) {
      await page.setViewportSize(viewportAt(width));
      await gotoIssue(page, `/issues/${key}`);
      for (const open of [false, true]) {
        if (open) await openDetails(page);
        await assertPageFits(page);
        await assertWhole(filterRow(page));
        await assertNoSidewaysScroll(filterRow(page), `the view row at ${width} px`);
        // The shell draws the rail in the render that routes to the new page, from a width query
        // read as it renders, and the view row was drawn above, so one count says whether this
        // width has the rail.
        if ((await iconRail(page).count()) > 0) {
          await assertWhole(iconRail(page));
          await assertNoSidewaysScroll(iconRail(page), `the rail at ${width} px`);
        }
        // The page draws the Contents pane in the render that draws the view row read above,
        // from a width query read as it renders, so one count says whether this width has it.
        if ((await contentsPane(page).count()) > 0) {
          await assertNoSidewaysScroll(contentsPane(page), `the pane at ${width} px`);
        }
        if (open) {
          await assertNoSidewaysScroll(marginShell(page), `Details at ${width} px`);
          await assertNoSidewaysScroll(
            marginShell(page).getByTestId("margin-sheet"),
            `Details' scrolling box at ${width} px`
          );
          await closeDetails(page);
        }
      }
      // The view row read above draws the Outline control in its own render, from a width query
      // read as it renders, so one count says whether this width has it.
      if ((await outlineControl(page).count()) > 0) {
        await outlineControl(page).click();
        await assertNoSidewaysScroll(outlineSheet(page), `the Outline sheet at ${width} px`);
        await assertNoSidewaysScroll(
          outlineSheet(page).locator(":scope > div").last(),
          `the Outline sheet's scrolling box at ${width} px`
        );
        await page.keyboard.press("Escape");
        await expect(outlineSheet(page)).toHaveCount(0);
      }
    }
  });
});

/** The address `view` has on `key`, at the end of the page's URL. */
function addressOf(key: string, view: Preset): RegExp {
  return new RegExp(`${presetPath(key, view)}$`);
}

/** The view row's height and each control in it, with its box and the words it draws. */
async function rowControls(page: Page): Promise<{
  height: number;
  controls: { height: number; text: string; top: number; width: number }[];
}> {
  const row = await boxOf(filterRow(page), "the view row");
  const controls = await filterRow(page)
    .locator("button, a, input, select")
    .evaluateAll((all) =>
      all
        .filter((control) => control.closest('[role="menu"], [role="dialog"]') === null)
        .map((control) => {
          const box = control.getBoundingClientRect();
          return {
            height: box.height,
            text: (control as HTMLElement).innerText.trim(),
            top: box.top,
            width: box.width,
          };
        })
    );
  return { controls, height: row.height };
}

// c-frame (2): the toggle half of the checkpoint's "the five controls are toggles that draw their
// query's answer @phone", its "the filter row is one line at every width @phone" less the words
// box, and five chords of `e2e/keyboard-issue.e2e.ts`'s "every chord the page bound before the
// redesign brings a target on screen with the focus in it".
test("the five controls are toggles with an address each, one line at every width, and the t chords reach them @phone", async ({
  browser,
}) => {
  test.setTimeout(180_000);
  await phase(browser, seedFrame, async (page, { key }) => {
    await gotoIssue(page, `/issues/${key}`);

    // The toolbar Filters: five toggles in R7's order, each named by its view's word alone and
    // described by its signal, whose count is the seed's.
    await expect(presetToolbar(page), "no toolbar named Filters holds the five").toHaveCount(1);
    await expect(presetToggles(page)).toHaveCount(5);
    expect(
      await presetToggles(page).evaluateAll((buttons) =>
        buttons.map((button) => button.querySelector("[data-preset-name]")?.textContent ?? "")
      ),
      "the row's controls are not Conversation, Spec, Verification, Plan and Decisions"
    ).toEqual(PRESET_ORDER.map((preset) => PRESET_NAMES[preset]));
    for (const preset of PRESET_ORDER) {
      const toggle = presetToggle(page, preset);
      await expect(toggle, `no toggle named ${PRESET_NAMES[preset]}`).toHaveCount(1);
      await expect(toggle).toHaveAttribute(
        "aria-pressed",
        preset === "conversation" ? "true" : "false"
      );
      await expect(presetCount(page, preset)).toHaveText(String(SEEDED_COUNTS[preset]));
      await expect(
        toggle,
        `${PRESET_NAMES[preset]} is not described by its count`
      ).toHaveAccessibleDescription(new RegExp(`^${SEEDED_COUNTS[preset]}\\b`));
    }

    // A press turns a view on and the others off, a second press turns it off, and each state
    // is its own address.
    for (const view of ["spec", "verification", "plan", "decisions"] as const) {
      await presetToggle(page, view).click();
      await expect(page).toHaveURL(addressOf(key, view));
      for (const other of PRESET_ORDER) {
        await expect(presetToggle(page, other)).toHaveAttribute(
          "aria-pressed",
          other === view ? "true" : "false"
        );
      }
      await presetToggle(page, view).click();
      await expect(page).toHaveURL(addressOf(key, "conversation"));
      await expect(presetToggle(page, "conversation")).toHaveAttribute("aria-pressed", "true");
    }
    for (const view of PRESET_ORDER) {
      await gotoIssue(page, presetPath(key, view));
      await expect(presetToggle(page, view)).toHaveAttribute("aria-pressed", "true");
    }
    // Conversation turns every view off from an address of the issue that is no view's.
    await gotoIssue(page, `/issues/${key}/collaborators`);
    await presetToggle(page, "conversation").click();
    await expect(page).toHaveURL(addressOf(key, "conversation"));

    // One tab stop, moved by the arrows, Home and End.
    expect(
      await presetToggles(page).evaluateAll(
        (buttons) => buttons.filter((button) => (button as HTMLElement).tabIndex === 0).length
      ),
      "the toolbar is not one tab stop"
    ).toBe(1);
    await presetToggle(page, "conversation").focus();
    await page.keyboard.press("ArrowRight");
    await expect(presetToggle(page, "spec")).toBeFocused();
    await page.keyboard.press("End");
    await expect(presetToggle(page, "decisions")).toBeFocused();
    await page.keyboard.press("Home");
    await expect(presetToggle(page, "conversation")).toBeFocused();
    await page.keyboard.press("ArrowLeft");
    await expect(presetToggle(page, "decisions")).toBeFocused();

    // Keys opens the list of keys, which names the five chords.
    await filterRow(page).getByRole("button", { exact: true, name: "Keys" }).click();
    const keys = page.getByRole("dialog", { name: "Keyboard shortcuts" });
    await expect(keys).toBeVisible();
    for (const view of ["spec", "verification", "plan", "decisions"] as const) {
      await expect(keys).toContainText(`Turn the ${PRESET_NAMES[view]} filter on or off`);
    }
    await expect(keys).toContainText(`${PRESET_NAMES.conversation}: turn every filter off`);
    await page.keyboard.press("Escape");
    await expect(keys).toBeHidden();

    // The chords, each with its pressed control and its address.
    for (const [second, view] of [
      ["s", "spec"],
      ["k", "verification"],
      ["p", "plan"],
      ["d", "decisions"],
      ["c", "conversation"],
    ] as const) {
      await focusThePage(page);
      await page.keyboard.press("t");
      await page.keyboard.press(second);
      await expect(page, `t ${second} did not open ${PRESET_NAMES[view]}`).toHaveURL(
        addressOf(key, view)
      );
      await expect(presetToggle(page, view)).toHaveAttribute("aria-pressed", "true");
    }
    // A chord on the view that is on turns it off: `t s` twice comes back to Conversation.
    await focusThePage(page);
    await page.keyboard.press("t");
    await page.keyboard.press("s");
    await expect(page).toHaveURL(addressOf(key, "spec"));
    await focusThePage(page);
    await page.keyboard.press("t");
    await page.keyboard.press("s");
    await expect(page, "t s on Spec did not turn it off").toHaveURL(addressOf(key, "conversation"));
    await expect(presetToggle(page, "conversation")).toHaveAttribute("aria-pressed", "true");

    // Below 1280 px the one control, named by the view that is on and its signal, opens a menu
    // of five, the one on checked; below 1024 px the Outline control stands beside it.
    for (const width of [1279, 390]) {
      await page.setViewportSize(viewportAt(width));
      await gotoIssue(page, presetPath(key, "spec"));
      await expect(
        presetChooser(page),
        `the one control does not name Spec and its count at ${width} px`
      ).toHaveAccessibleName(new RegExp(`^${PRESET_NAMES.spec} ${SEEDED_COUNTS.spec}\\b`));
      await presetChooser(page).click();
      await expect(openMenu(page)).toHaveCount(1);
      await expect(openMenu(page).getByRole("menuitemradio"), "the menu is not five").toHaveCount(
        5
      );
      for (const preset of PRESET_ORDER) {
        await expect(presetMenuItem(page, preset)).toHaveAttribute(
          "aria-checked",
          preset === "spec" ? "true" : "false"
        );
      }
      await page.keyboard.press("Escape");
      await expect(openMenu(page)).toHaveCount(0);
      await expect(presetChooser(page), "Escape left the focus off the one control").toBeFocused();
      // The item of the view that is on keeps it on, and gives the focus back to the control.
      await presetChooser(page).click();
      await presetMenuItem(page, "spec").click();
      await expect(openMenu(page)).toHaveCount(0);
      await expect(page, "the checked item turned Spec off").toHaveURL(addressOf(key, "spec"));
      await expect(presetChooser(page), "a chosen item left the focus nowhere").toBeFocused();
      // Conversation, chosen from an address of the issue that is no view's, goes to the issue's.
      await gotoIssue(page, `/issues/${key}/collaborators`);
      await presetChooser(page).click();
      await presetMenuItem(page, "conversation").click();
      await expect(page).toHaveURL(addressOf(key, "conversation"));
    }
    // With the menu open under the Outline sheet, Escape closes the sheet and leaves the menu.
    await gotoIssue(page, `/issues/${key}`);
    await presetChooser(page).click();
    await expect(openMenu(page)).toHaveCount(1);
    await outlineControl(page).click();
    await expect(outlineSheet(page)).toHaveCount(1);
    await page.keyboard.press("Escape");
    await expect(outlineSheet(page), "Escape in the sheet did not close it").toHaveCount(0);
    await expect(openMenu(page), "Escape in the sheet closed the menu under it").toHaveCount(1);
    await page.keyboard.press("Escape");
    await expect(openMenu(page)).toHaveCount(0);
    for (const width of [1023, 390]) {
      await page.setViewportSize(viewportAt(width));
      await gotoIssue(page, `/issues/${key}`);
      await expect(outlineControl(page), `no Outline control at ${width} px`).toHaveCount(1);
    }

    // At each of R10's widths, 390 px first, the row is one line, at most 35 px tall from
    // 1280 px and 51 px below, no control in it under 24 px, and each control's words inside
    // its name.
    for (const width of PHONE_FIRST) {
      await page.setViewportSize(viewportAt(width));
      await gotoIssue(page, `/issues/${key}`);
      await assertPageFits(page);
      const limit = width >= 1280 ? 35 : 51;
      await expect
        .poll(async () => (await rowControls(page)).height, {
          message: `the view row is over ${limit} px tall at ${width} px`,
        })
        .toBeLessThanOrEqual(limit);
      const { controls } = await rowControls(page);
      const [first] = controls;
      for (const control of controls) {
        expect(
          first !== undefined &&
            control.top < first.top + first.height &&
            control.top + control.height > first.top,
          `"${control.text}" is not on the row's one line at ${width} px`
        ).toBe(true);
      }
      for (const control of controls) {
        expect(
          control.height,
          `"${control.text}" is under 24 px tall at ${width} px`
        ).toBeGreaterThanOrEqual(24);
        expect(
          control.width,
          `"${control.text}" is under 24 px wide at ${width} px`
        ).toBeGreaterThanOrEqual(24);
      }
      // A control's words are the text it shows less what it hides from assistive technology,
      // such as a glyph that names nothing (WCAG 2.5.3 reads words).
      const named = filterRow(page).locator("button:visible");
      // The poll above waited for the row to draw at this width and its controls were measured
      // since, with no press between, so one count lists the buttons it shows.
      for (let at = 0; at < (await named.count()); at += 1) {
        // The count above found this control drawn, and its words are the ones its render wrote,
        // so one read says whether it shows any words.
        const words = await named.nth(at).evaluate((control: HTMLElement) => {
          let text = control.innerText;
          for (const hidden of control.querySelectorAll<HTMLElement>('[aria-hidden="true"]')) {
            text = text.replace(hidden.innerText, " ");
          }
          return text.replace(/\s+/g, " ").trim();
        });
        if (words === "") continue;
        await expect(
          named.nth(at),
          `"${words}" is not inside its control's name`
        ).toHaveAccessibleName(new RegExp(words.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")));
      }
    }

    // At 390 px a scrolled page keeps the row at the top; at 1440 px it scrolls away.
    for (const [width, stays] of [
      [390, true],
      [1440, false],
    ] as const) {
      await page.setViewportSize(viewportAt(width));
      await gotoIssue(page, `/issues/${key}`);
      await page.evaluate(() => window.scrollTo(0, document.documentElement.scrollHeight));
      const row = await boxOf(filterRow(page), "the view row");
      if (stays) expect(Math.round(row.y), "the row left the top at 390 px").toBe(0);
      else expect(row.y, "the row stayed at the top at 1440 px").toBeLessThan(0);
    }
  });
});

// "760 px and below" starts at 760 px itself, where `HEADER_SCROLLS_QUERY` (`(max-width:
// 760px)`) matches, the header scrolls away and the view row is the band that stays (decision
// 72 of the parts). Tailwind builds `max-[760px]:` as `@media not all and (min-width:
// 760px)`, which is under 760 px alone, so the row's classes are `max-[761px]:`; with
// `max-[760px]:` the row has no sticky box at 760 px, and this row fails on "the view row has no
// sticky box at 760 px", and with the one control's `max-[760px]:flex-1` it fails on "the one
// control does not fill the row at 760 px".
test("at 760 px the header scrolls away and the view row stays at the top of a scrolled page", async ({
  browser,
}) => {
  await phase(browser, seedFrame, async (page, { key }) => {
    await page.setViewportSize(viewportAt(760));
    await gotoIssue(page, `/issues/${key}`);
    expect(
      await page.evaluate((query) => window.matchMedia(query).matches, HEADER_SCROLLS_QUERY),
      `${HEADER_SCROLLS_QUERY} does not match at 760 px`
    ).toBe(true);
    // The band whose height `useIdentityBlock` publishes at this width is the row's nearest
    // sticky box (`identity-block.ts`'s `stickyAncestor`), and with none it publishes 0. The
    // hook is mounted by the identity block, which no slot of this part draws, so the row reads
    // that band itself.
    const band = await filterRow(page).evaluate((row) => {
      for (let at: Element | null = row; at !== null; at = at.parentElement) {
        const style = getComputedStyle(at);
        if (style.position === "sticky") {
          return { height: at.getBoundingClientRect().height, top: style.top };
        }
      }
      return null;
    });
    expect(band, "the view row has no sticky box at 760 px").not.toBeNull();
    expect(band?.top, "the view row's sticky box is not at the top of the screen").toBe("0px");
    expect(band?.height ?? 0, "the band that stays is 0 px tall at 760 px").toBeGreaterThan(0);
    // The one control fills the row beside Outline at 760 px, as it does below.
    expect(
      await presetRow(page).evaluate((control) => getComputedStyle(control).flexGrow),
      "the one control does not fill the row at 760 px"
    ).toBe("1");

    await page.evaluate(() => window.scrollTo(0, document.documentElement.scrollHeight));
    await expect
      .poll(() => page.evaluate(() => window.scrollY), {
        message: "the page did not scroll at 760 px",
      })
      .toBeGreaterThan(0);
    const row = await boxOf(filterRow(page), "the view row");
    expect(Math.round(row.y), "the row left the top at 760 px").toBe(0);
  });
});

/** The document's `scroll-padding-top`, and against it the summed height of the bands above the
 *  rows that stay at the top of the screen: the header's identity block and the view row's box,
 *  each counted only while it is `position: sticky`. */
async function topPadding(page: Page): Promise<{ padding: string; stays: number }> {
  return page.evaluate(() => {
    const bands = new Set<Element>();
    const block = document.querySelector("[data-identity-block]");
    if (block !== null) bands.add(block);
    for (let at = document.querySelector("[data-filter-row]"); at !== null; at = at.parentElement) {
      if (getComputedStyle(at).position === "sticky") {
        bands.add(at);
        break;
      }
    }
    let stays = 0;
    for (const band of bands) {
      if (getComputedStyle(band).position === "sticky") {
        stays += Math.round(band.getBoundingClientRect().height);
      }
    }
    return { padding: getComputedStyle(document.documentElement).scrollPaddingTop, stays };
  });
}

// What a scroll brings into view stands below everything that stays at the top of the screen
// (decision 113 of the parts): the page scrolls the document, whose
// `scroll-padding-top` is the height of the bands above the rows that are sticky at that width,
// the view row at 390 px and the header's identity block, where a part draws it, at 1440 px. A
// later part's focused row lands by `scrollIntoView`, which reads that padding. With no padding
// set the row fails at 390 px on "scroll-padding-top is not the height of what stays at 390 px".
test("the page's scroll padding at the top is the height of what stays above the rows, at 390 and 1440 px", async ({
  browser,
}) => {
  await phase(browser, seedFrame, async (page, { key }) => {
    for (const width of [390, 1440]) {
      await page.setViewportSize(viewportAt(width));
      await gotoIssue(page, `/issues/${key}`);
      if (width === 390) {
        expect(
          (await topPadding(page)).stays,
          "nothing above the rows stays at the top at 390 px"
        ).toBeGreaterThan(0);
      }
      await expect
        .poll(
          async () => {
            const { padding, stays } = await topPadding(page);
            return padding === `${stays}px` ? "equal" : `${padding} against ${stays}px`;
          },
          { message: `scroll-padding-top is not the height of what stays at ${width} px` }
        )
        .toBe("equal");
    }
  });
});

/** `window.scrollY` two frames from now, so a scroll that a focus started is read. */
async function scrollYInTwoFrames(page: Page): Promise<number> {
  return page.evaluate(() => {
    const { promise, resolve } = Promise.withResolvers<number>();
    requestAnimationFrame(() => requestAnimationFrame(() => resolve(window.scrollY)));
    return promise;
  });
}

// A focus that enters the view row while it stays at the top of the screen leaves a scrolled page
// where it is (decision 113 of the parts, review 5930786767): each control in the row
// takes a top scroll margin of minus the row's height. The row stands in the band the page's
// `scroll-padding-top` keeps clear, so without that margin the browser scrolls the page up to
// bring a focused control below the band, and the row does not move: this row then fails on
// "Escape in the view menu moved the page".
test("at 390 px a focus that enters the view row leaves a scrolled page where it is: Escape in the view menu, Tab and the Outline sheet's Close", async ({
  browser,
}) => {
  await phase(browser, seedFrame, async (page, { key }) => {
    await page.setViewportSize(viewportAt(390));
    await gotoIssue(page, `/issues/${key}`);
    await page.evaluate(() => window.scrollTo(0, document.documentElement.scrollHeight));
    await expect
      .poll(() => page.evaluate(() => window.scrollY), {
        message: "the page did not scroll at 390 px",
      })
      .toBeGreaterThan(0);
    const from = await scrollYInTwoFrames(page);
    // What makes a focus there scroll the page: the one control stands in the band the padding
    // keeps clear.
    const padding = await page.evaluate(
      () => getComputedStyle(document.documentElement).scrollPaddingTop
    );
    const chooser = await boxOf(presetChooser(page), "the one control");
    expect(
      chooser.y,
      `the one control does not stand in the ${padding} the scroll padding keeps clear`
    ).toBeLessThan(Number.parseFloat(padding));
    const stays = async (what: string) =>
      expect(await scrollYInTwoFrames(page), `${what} moved the page`).toBe(from);
    // The control is pressed where it is drawn, as a reader's pointer presses it: a locator's
    // click first scrolls its target into view, a scroll of the test's and not of the page's.
    const pressChooser = () =>
      page.mouse.click(chooser.x + chooser.width / 2, chooser.y + chooser.height / 2);

    await pressChooser();
    await expect(presetMenuItem(page, "conversation")).toBeFocused();
    await stays("opening the view menu");
    await page.keyboard.press("Escape");
    await expect(openMenu(page)).toHaveCount(0);
    await expect(presetChooser(page), "Escape left the focus off the one control").toBeFocused();
    await stays("Escape in the view menu");

    await page.keyboard.press("Tab");
    await expect(outlineControl(page), "Tab did not move on to Outline").toBeFocused();
    await stays("Tab into Outline");
    await page.keyboard.press("Shift+Tab");
    await expect(presetChooser(page), "Shift+Tab did not move back").toBeFocused();
    await stays("Shift+Tab into the one control");

    await page.keyboard.press("Tab");
    await page.keyboard.press("Enter");
    await expect(outlineSheet(page)).toHaveCount(1);
    await outlineSheet(page).getByRole("button", { exact: true, name: "Close" }).click();
    await expect(outlineSheet(page)).toHaveCount(0);
    await expect(outlineControl(page), "the sheet left the focus off Outline").toBeFocused();
    await stays("closing the Outline sheet");

    // Each control the row holds, the menu's items among them, takes the margin.
    await pressChooser();
    await expect(openMenu(page)).toHaveCount(1);
    const margins = await filterRow(page).evaluate((row) =>
      [...row.querySelectorAll("button, a[href], input, select, [tabindex]")].map(
        (control) => getComputedStyle(control).scrollMarginTop
      )
    );
    expect(margins.length, "the view row holds no control").toBeGreaterThan(0);
    expect(
      new Set(margins),
      "a control of the view row takes no top scroll margin of minus the padding"
    ).toEqual(new Set([`-${padding}`]));
    await stays("opening the view menu again");
  });
});

/** How many rows the cost row draws beside the view row: as many as a long outline holds. */
const MANY_ROWS = 5000;

/** The most elements whose style one added row may recalculate: the row and its neighbours. */
const RECALCULATED_FOR_ONE_ROW = 100;

// A change the new page makes recalculates the style of what it changed, not of the whole page:
// adding one row to a page of `MANY_ROWS` rows recalculates the style of that row alone. The
// view row's scroll margin once read `html:has([data-issue-page]) [data-filter-row] *`. A
// selector whose subject is universal under an `html:has()` ancestor made each element added
// with an attribute recalculate every element, so the issue page's rows drew four times
// slower. The reading is Chrome's own count of the elements a style recalculation visits
// (`UpdateLayoutTree`'s `elementCount`), not a time. On a reduced page of 5000 rows in Chrome 158,
// one added row recalculated 10012 elements with that rule, and 3 (1440 px) or 5 (390 px)
// without it. So with it this row fails on "adding one row recalculated the style of".
test("adding one row to a page of many rows recalculates the style of that row alone, not of the whole page", async ({
  browser,
}) => {
  await phase(browser, seedFrame, async (page, { key }) => {
    for (const width of [390, 1440]) {
      await page.setViewportSize(viewportAt(width));
      await gotoIssue(page, `/issues/${key}`);
      const elements = await issuePage(page).evaluate((root, many) => {
        const list = document.createElement("ol");
        list.id = "e2e-many-rows";
        for (let at = 0; at < many; at += 1) {
          const row = document.createElement("li");
          row.textContent = `row ${at}`;
          list.append(row);
        }
        root.append(list);
        getComputedStyle(list.lastElementChild ?? list).color;
        return document.getElementsByTagName("*").length;
      }, MANY_ROWS);
      await browser.startTracing(page, { categories: ["devtools.timeline"] });
      await page.evaluate(() => {
        const row = document.createElement("li");
        row.setAttribute("data-added", "");
        document.getElementById("e2e-many-rows")?.append(row);
        getComputedStyle(row).color;
      });
      const trace = JSON.parse((await browser.stopTracing()).toString()) as {
        traceEvents: { args?: { elementCount?: number }; name: string }[];
      };
      const recalculations = trace.traceEvents.filter((event) => event.name === "UpdateLayoutTree");
      expect(recalculations.length, "the trace read no style recalculation").toBeGreaterThan(0);
      const recalculated = recalculations.reduce(
        (sum, event) => sum + (event.args?.elementCount ?? 0),
        0
      );
      expect(
        recalculated,
        `adding one row recalculated the style of ${recalculated} elements of the page's ${elements} at ${width} px`
      ).toBeLessThan(RECALCULATED_FOR_ONE_ROW);
    }
  });
});

// From an address that names a node (`#b-`), a pointer press on Spec, then on Decisions, then on
// Verification's count moves to each view's address, one history entry each, and leaves the
// focus where it was (decision 93 of the parts). There, a press whose mousedown took the
// focus out of the outline closed the body of the outline's row, the page grew shorter under the
// pointer, and the press ended elsewhere and did nothing. The outline is a later part's; here a
// control outside the outline and every view, the icon rail's project link, holds the focus in
// the row's place: the page's `main` holds the stage, so a view drawn there takes a focus left
// on `main` as nobody's. A count is its button's `<label>`, whose own activation focuses the
// button at the click: with it, this row fails on "a press on Verification's count took the
// focus".
// SLOW(>=2s): a seeded frame, a load waited on until the outline puts the focus on the
// address's row, then three presses each waited on until the view's address moves.
test("from an address that names a node, a press on Spec, on Decisions and on Verification's count moves to each view's address, one history entry each, and leaves the focus where it was", async ({
  browser,
}) => {
  await phase(browser, seedFrame, async (page, { key }) => {
    await gotoIssue(page, `/issues/${key}#b-r1`);
    await expect(page).toHaveURL(new RegExp(`/issues/${key}#b-r1$`));
    // With the outline the page opens with the focus on the row the address names, and takes it
    // back from what holds the outline until the reader acts, so the opening is
    // waited out and the reader acts first - a Shift, which no key of the page binds - or the
    // page would take the focus from the project link below.
    await expectOpeningLanded(page);
    await page.keyboard.press("Shift");
    const holder = iconRail(page).getByRole("link", { exact: true, name: key.split("-")[0] });
    await holder.focus();
    await expect(holder, "the icon rail's project link took no focus").toBeFocused();
    const entries = () => page.evaluate(() => window.history.length);
    const before = await entries();
    await presetToggle(page, "spec").click();
    await expect(page, "a press on Spec did not move to Spec's address").toHaveURL(
      addressOf(key, "spec")
    );
    expect(await entries(), "a press on Spec added no history entry").toBe(before + 1);
    await expect(holder, "a press on Spec took the focus").toBeFocused();
    await presetToggle(page, "decisions").click();
    await expect(page, "a press on Decisions did not move to Decisions' address").toHaveURL(
      addressOf(key, "decisions")
    );
    expect(await entries(), "a press on Decisions added no history entry").toBe(before + 2);
    await expect(holder, "a press on Decisions took the focus").toBeFocused();
    await presetCount(page, "verification").click();
    await expect(page, "a press on Verification's count did not move to its address").toHaveURL(
      addressOf(key, "verification")
    );
    expect(await entries(), "a press on Verification's count added no history entry").toBe(
      before + 3
    );
    await expect(holder, "a press on Verification's count took the focus").toBeFocused();
  });
});

// On the phone, Details is a modal dialog, so a press on its text keeps the focus inside it, and
// the sheet's own `Shift+M`, which acts only while the focus is inside, still closes it (decision
// 168 of the parts). With no focusable box in the sheet, the press moves the focus to
// the body, and this row fails on "a press on Details' text took the focus out of the sheet".
test("at 390 px a press on Details' text keeps the focus in the sheet, and Shift+M then closes it", async ({
  browser,
}) => {
  await phase(browser, seedFrame, async (page, { key }) => {
    await page.setViewportSize(viewportAt(390));
    await gotoIssue(page, `/issues/${key}`);
    await expectOpeningLanded(page);
    await focusThePage(page);
    await page.keyboard.press("Shift+M");
    const phoneDetails = page.getByRole("dialog", { exact: true, name: "Details" });
    await expect(phoneDetails, "Shift+M did not open the phone's Details").toBeVisible();
    await expect(detailsClose(page)).toBeFocused();
    await phoneDetails.getByRole("heading", { exact: true, name: "Details" }).click();
    expect(
      await phoneDetails.evaluate((sheet) => sheet.contains(document.activeElement)),
      "a press on Details' text took the focus out of the sheet"
    ).toBe(true);
    await page.keyboard.press("Shift+M");
    await expect(
      phoneDetails,
      "Shift+M after a press on Details' text did not close the sheet"
    ).toBeHidden();
  });
});

/** An issue whose spec links another issue, so today's page draws a reference to hover. */
async function seedTodaysPage(): Promise<{ key: string }> {
  await createProject({ key: "OFF", name: "Today" });
  const target = await createIssue({ project: "OFF", title: "The issue the spec links" });
  const issue = await createIssue({
    project: "OFF",
    spec: `See [the other issue](dispatch://${target.key}) for context.\n`,
    title: "A reader with the preference off",
  });
  return { key: issue.key };
}

// c-frame (3): with the preference off, today's shell on the issue page.
test("a reader with the preference off keeps today's shell on the issue page", async ({
  browser,
}) => {
  const { key } = await seedTodaysPage();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.setViewportSize({ height: 900, width: 1440 });
    await page.goto(`/issues/${key}/spec`);
    await expect(page.getByTestId("main-content")).toBeVisible();
    await expect(iconRail(page), "today's issue page draws the rail").toHaveCount(0);
    await expect(
      page.getByRole("complementary", { exact: true, name: "Navigation" }),
      "today's issue page lost its sidebar"
    ).toBeVisible();
    await expect(marginShell(page), "today's issue page lost its margin column").toBeVisible();
    await expect(marginShell(page).getByRole("tab", { name: "Comments" })).toBeVisible();
    await expect(marginResizeHandle(page)).toHaveCount(1);
    // `Shift+M` folds the margin to its rail, and back.
    // no opening focus: today's issue page, which the preference off keeps, has no outline and
    // places no focus of its own.
    await focusThePage(page);
    await page.keyboard.press("Shift+M");
    await expect(page.getByTestId("margin-rail"), "Shift+M did not fold the margin").toBeVisible();
    await page.keyboard.press("Shift+M");
    await expect(page.getByTestId("margin-rail")).toHaveCount(0);
    // A resting pointer on a reference opens its card.
    const reference = page.locator("[data-dispatch-href]").first();
    await expect(reference).toBeVisible();
    await reference.hover();
    await expect(page.getByRole("tooltip"), "a hover opened no card on today's page").toBeVisible();

    // While the issue page's chunk loads, today's skeleton names its nine tabs.
    const held = Promise.withResolvers<void>();
    await page.route("**/assets/IssuePage-*.js", async (route) => {
      await held.promise;
      await route.continue();
    });
    const arrival = page.goto(`/issues/${key}`).catch(() => undefined);
    const tabs = page.locator('[aria-busy="true"] [role="tablist"]');
    await expect(tabs).toBeVisible();
    expect(
      (await tabs.innerText())
        .split("\n")
        .map((word) => word.trim())
        .filter((word) => word !== ""),
      "today's skeleton does not name its nine tabs"
    ).toEqual([...ISSUE_TAB_LABELS]);
    held.resolve();
    await arrival;
  } finally {
    await alice.close();
  }
});

/**
 * The new page fills a slot in the shell's margin and draws its own frame (R12, R14), and the
 * margin is mounted once for every route. These three rows say the frame
 * changed no other page for a reader who turned the new page on: each draws its margin as it did,
 * no rail, and no Details. Details carries `data-details` (`margin-slot.tsx`'s
 * `DETAILS_ATTRIBUTE`) open or closed, wherever the shell hands the margin a drawer.
 */
const ISSUE_PANEL = "[data-details]";

test("a project document page draws its margin, and none of the issue page's panel", async ({
  browser,
}) => {
  await createProject({ key: "DOC", name: "Documents" });
  const document = await createProjectDocument("DOC", {
    content: "# The handbook\n\nIts margin is the margin every document page has.\n",
    name: "handbook.md",
  });
  const alice = await asUser(browser, "alice");
  try {
    await withIssuePage(alice, "alice");
    const page = await alice.newPage();
    await page.setViewportSize({ height: 900, width: 1440 });
    await page.goto(`/projects/DOC/documents/${document.artifact.slug}`);
    await expect(page.getByRole("heading", { level: 1, name: "The handbook" })).toBeVisible();
    await expect(iconRail(page), "a project document page draws the rail").toHaveCount(0);
    await expect(page.getByTestId("desktop-margin-shell")).toHaveCount(1);
    await expect(page.getByTestId("margin-sheet")).toBeVisible();
    await expect(page.getByRole("tab", { name: "Comments" })).toBeVisible();
    await expect(page.getByLabel("Resize margin")).toHaveCount(1);
    await expect(page.locator(ISSUE_PANEL)).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

test("the Inbox keeps its margin-less layout", async ({ browser }) => {
  await createProject({ key: "INB", name: "Inbox" });
  await createIssue({ project: "INB", spec: SPEC, title: "An issue to list" });
  const alice = await asUser(browser, "alice");
  try {
    await withIssuePage(alice, "alice");
    const page = await alice.newPage();
    await page.setViewportSize({ height: 900, width: 1440 });
    await page.goto("/");
    await expect(page.getByTestId("main-content")).toBeVisible();
    await expect(iconRail(page), "the Inbox draws the rail").toHaveCount(0);
    // A route with no margin spends no width on one, which is what it did before the slot.
    await expect(page.getByTestId("desktop-margin-shell")).toHaveCount(0);
    await expect(page.locator(ISSUE_PANEL)).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

test("an Agents page keeps its margin-less layout", async ({ browser }) => {
  const alice = await asUser(browser, "alice");
  try {
    await withIssuePage(alice, "alice");
    const page = await alice.newPage();
    await page.setViewportSize({ height: 900, width: 1440 });
    await page.goto("/agents");
    await expect(page.getByTestId("main-content")).toBeVisible();
    await expect(iconRail(page), "the Agents page draws the rail").toHaveCount(0);
    await expect(page.getByTestId("desktop-margin-shell")).toHaveCount(0);
    await expect(page.locator(ISSUE_PANEL)).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

// c-frame (7): the checkpoint's "the issue page's skeleton fits a phone and names its presets
// @phone" (`e2e/phone.e2e.ts`), with the new page on: the frame's skeleton names nothing.
test("the issue page's skeleton fits a phone and names no tab @phone", async ({ browser }) => {
  await phase(browser, seedFrame, async (page, { key }) => {
    // At each of R10's widths, 390 px first. The new page's own chunk is held until the
    // skeleton has been read: it is the only thing between the address and the page, so holding
    // it is what makes the skeleton readable at all. The navigation is not awaited while it is
    // held, and is let go once the width is read.
    for (const width of PHONE_FIRST) {
      await page.setViewportSize(viewportAt(width));
      const held = Promise.withResolvers<void>();
      const hold = async (route: Route) => {
        await held.promise;
        await route.continue();
      };
      await page.route("**/assets/IssuePage-*.js", hold);
      const arrival = page.goto(`/issues/${key}`).catch(() => undefined);
      // The page's skeleton, in `<main>`: the app's own loading state outside it, which names
      // Dispatch while the session is read, is no skeleton of the page's.
      const skeleton = page.getByTestId("main-content").locator('[aria-busy="true"]');
      // The words first: today's skeleton names its tabs and draws no toolbar, so a frame that
      // drew it fails here, on the assertion this row is for.
      await expect(skeleton).toBeVisible();
      expect(
        (await skeleton.innerText()).trim(),
        `the frame's skeleton names a tab at ${width} px`
      ).toBe("");
      await expect(skeleton.locator('[role="toolbar"] > *')).toHaveCount(5);
      const layout = await page.evaluate(() => ({
        client: document.documentElement.clientWidth,
        scroll: document.documentElement.scrollWidth,
      }));
      expect(
        layout.scroll,
        `the skeleton made the page ${layout.scroll}px wide in a ${layout.client}px viewport`
      ).toBeLessThanOrEqual(layout.client);
      held.resolve();
      await arrival;
      await page.unroute("**/assets/IssuePage-*.js", hold);
    }
  });
});
