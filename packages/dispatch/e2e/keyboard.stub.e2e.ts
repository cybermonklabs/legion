/** The global keyboard as the page draws it: the chord indicator and its expiry, the shortcut
 *  help's own listing and filter, the Inbox selection the keys walk, and the board's roving.
 *
 *  Each reads what the browser does over one fixed answer, so each runs against a recording
 *  rather than against a harness (packages/dispatch/AGENTS.md, "The stubbed layer"). The row
 *  that creates an issue and the one that moves a card between statuses stayed in
 *  `keyboard.e2e.ts`: both are writes read back from the server.
 */
import { expect } from "@playwright/test";

import { openAgents } from "./agents";
import { recordClipboard } from "./clipboard";
import { openBoard, openInbox, seedBoard, seedInbox } from "./keyboard";
import { stubbed } from "./stub/stub-test";

/** Nothing seeded: the two chord rows need a page with the keymap bound, which the Agents page
 *  is with no session listed, and the addresses the chords move between. */
const chordTest = stubbed(async () => ({ agents: "/agents", inbox: "/" }));

/** Two questions on one issue, each with three options. */
const inboxTest = stubbed(seedInbox);

/** Six cards across three statuses. */
const boardTest = stubbed(seedBoard);

chordTest(
  "g then i goes to the Inbox, showing the pending chord until it completes @pr",
  async ({ page, scene }, testInfo) => {
    await openAgents(page);

    // The chord's second key must follow within the keymap's 1000 ms window, so nothing slow sits
    // between the two keys: the screenshot is taken once the Inbox is reached.
    const indicator = page.getByTestId("chord-indicator");
    await page.keyboard.press("g");
    await expect(indicator).toBeVisible();
    await expect(indicator).toHaveText(/g/);
    await page.keyboard.press("i");
    await expect(page).toHaveURL(new RegExp(`${scene.inbox}$`));
    await expect(indicator).toHaveCount(0);
    await page.screenshot({
      path: testInfo.outputPath(`chord-g-i-${testInfo.project.name}.png`),
    });
  }
);

chordTest(
  "an unfinished chord expires after a second and the next key stands alone @pr",
  async ({ page, scene }) => {
    await openAgents(page);

    const indicator = page.getByTestId("chord-indicator");
    await page.keyboard.press("g");
    await expect(indicator).toBeVisible();
    await expect(indicator).toHaveCount(0, { timeout: 2000 });
    await page.keyboard.press("i");
    await page.waitForTimeout(300);
    await expect(page).toHaveURL(new RegExp(`${scene.agents}$`));
  }
);

inboxTest(
  "? lists the registry with unavailable rows greyed, filters live, and Escape returns to the row @pr",
  async ({ page, scene }, testInfo) => {
    await openInbox(page, scene.rows);
    const rows = page.locator("[data-inbox-row]");

    // With the focus outside the list, the shortcut bound to the row holding it is registered
    // but unavailable (`when()` is false), so ? lists it greyed.
    await page.keyboard.press("?");
    const dialog = page.getByRole("dialog", { name: "Keyboard shortcuts" });
    await expect(dialog).toBeVisible();
    const backRow = dialog
      .getByRole("listitem")
      .filter({ hasText: "Back to the row, then clear the focused row" });
    await expect(backRow).toHaveAttribute("data-enabled", "false");
    await expect(backRow.locator("kbd")).toHaveText(["Esc"]);
    await page.keyboard.press("Escape");
    await expect(dialog).toHaveCount(0);

    // The keyboard's way into the list: Enter puts the focus in the reading column's answer
    // box, and Escape hands it back to the row the column is showing.
    await page.keyboard.press("Enter");
    await page.keyboard.press("Escape");
    await expect(rows.nth(0)).toBeFocused();
    await page.keyboard.press("?");
    await expect(dialog).toBeVisible();
    const filter = dialog.getByRole("searchbox", { name: "Filter shortcuts" });
    await expect(filter).toBeFocused();

    const goToInbox = dialog.getByRole("listitem").filter({ hasText: "Go to Inbox" });
    await expect(goToInbox.locator("kbd")).toHaveText(["g", "i"]);
    await expect(goToInbox).toHaveAttribute("data-enabled", "true");
    // Row-bound shortcuts describe the row that was focused when ? was pressed.
    await expect(backRow).toHaveAttribute("data-enabled", "true");
    await expect(dialog.getByRole("region", { name: "Global" })).toBeVisible();
    await expect(dialog.getByRole("region", { name: "Inbox" })).toBeVisible();
    await page.screenshot({
      path: testInfo.outputPath(`shortcut-help-${testInfo.project.name}.png`),
    });

    await filter.fill("agents");
    await expect(dialog.getByRole("listitem")).toHaveCount(1);
    await expect(dialog.getByRole("listitem")).toContainText("Go to Agents");

    await page.keyboard.press("Escape");
    await expect(dialog).toHaveCount(0);
    await expect(rows.nth(0)).toBeFocused();
  }
);

inboxTest(
  "j/k move the reading column's selection, digits pick its options, Enter and Escape move between row and answer, y copies the ask's reference, o opens the issue @pr",
  async ({ page, scene }, testInfo) => {
    const copied = await recordClipboard(page);
    await openInbox(page, scene.rows);
    const rows = page.locator("[data-inbox-row]");
    const first = rows.nth(0);
    const pane = page.getByTestId("inbox-pane");

    // The Inbox selects its first row when it opens; j and k walk that selection, and the
    // column follows it.
    await expect(first).toHaveAttribute("data-inbox-selected", "true");
    await page.keyboard.press("j");
    await expect(rows.nth(1)).toHaveAttribute("data-inbox-selected", "true");
    await page.keyboard.press("k");
    await expect(first).toHaveAttribute("data-inbox-selected", "true");

    // y copies the selected ask's reference through the column head's own control, so the
    // column confirms it.
    const firstAskId = await first.getAttribute("data-inbox-row");
    await page.keyboard.press("y");
    await expect(pane.getByText("Copied", { exact: true })).toBeVisible();
    await expect.poll(copied).toEqual([`dispatch://${scene.issueKey}/ask/${firstAskId}`]);
    await page.screenshot({
      fullPage: true,
      path: testInfo.outputPath(`inbox-selected-row-${testInfo.project.name}.png`),
    });

    await page.keyboard.press("2");
    await expect(pane.getByRole("radio", { name: "Hold" })).toBeChecked();
    await expect(pane.getByRole("radio", { name: "Ship" })).not.toBeChecked();

    await page.keyboard.press("Enter");
    const answer = pane.getByRole("textbox", { name: "Your answer" });
    await expect(answer).toBeFocused();
    await page.keyboard.type("j?");
    await expect(answer).toHaveValue("j?");
    // Typing followed nothing and opened nothing: the address is still the Inbox, carrying the
    // row the column is showing.
    await expect(page).toHaveURL(`/?item=${firstAskId}`);
    await expect(page.getByRole("dialog")).toHaveCount(0);

    // The search toggle is the one shortcut that still works while typing.
    await page.keyboard.press("Control+k");
    const search = page.getByRole("dialog", { name: "Search" });
    await expect(search).toBeVisible();
    await page.keyboard.press("Escape");
    await expect(search).toHaveCount(0);
    await expect(answer).toBeFocused();

    await page.keyboard.press("Escape");
    await expect(first).toBeFocused();
    await expect(answer).toHaveValue("j?");

    await page.keyboard.press("o");
    await expect(page).toHaveURL(new RegExp(`/issues/${scene.issueKey}`));
  }
);

boardTest(
  "board focus roves with j/k/h/l over cards, empty columns and the Done rail, and ? lists the Board and Project scopes @pr",
  async ({ page, scene }, testInfo) => {
    const { alpha, bravo, delta } = scene;
    const copied = await recordClipboard(page);
    await page.setViewportSize({ width: 1280, height: 900 });
    await openBoard(page);
    const card = (issue: { key: string }, title: string) =>
      page.getByRole("article", { name: `${issue.key} ${title}` });
    const alphaCard = card(alpha, "Alpha");
    const deltaCard = card(delta, "Delta");

    // y / Shift+Y copy the focused card's reference / key and say so on the board's status line.
    await page.keyboard.press("j");
    await expect(alphaCard).toBeFocused();
    await page.keyboard.press("y");
    await expect(page.getByRole("status", { name: "Board announcements" })).toHaveText(
      `Copied dispatch://${alpha.key}`
    );
    await page.keyboard.press("Shift+Y");
    await expect(page.getByRole("status", { name: "Board announcements" })).toHaveText(
      `Copied ${alpha.key}`
    );
    await expect.poll(copied).toEqual([`dispatch://${alpha.key}`, alpha.key]);
    await page.keyboard.press("Escape");
    await expect
      .poll(() => page.evaluate(() => document.activeElement === document.body))
      .toBe(true);

    // B1: j/k rove within Todo and clamp at its ends.
    await page.keyboard.press("j");
    await expect(alphaCard).toBeFocused();
    await page.keyboard.press("j");
    await expect(card(bravo, "Bravo")).toBeFocused();
    await page.keyboard.press("k");
    await expect(alphaCard).toBeFocused();
    await page.keyboard.press("k");
    await expect(alphaCard).toBeFocused();

    // l steps into the next column at the same index, clamped; an empty column takes focus
    // itself, so h/l stay meaningful across it.
    await page.keyboard.press("l");
    await expect(deltaCard).toBeFocused();
    await page.keyboard.press("l");
    await expect(page.getByRole("region", { name: "Testing" })).toBeFocused();
    await page.keyboard.press("h");
    await expect(deltaCard).toBeFocused();
    await page.keyboard.press("h");
    await expect(alphaCard).toBeFocused();

    // l all the way reaches the collapsed Done rail and stays there.
    for (const _ of ["in_progress", "testing", "needs_review", "retro", "done"]) {
      await page.keyboard.press("l");
    }
    const doneRail = page.getByRole("region", { name: "Done (collapsed)" });
    await expect(doneRail).toBeFocused();
    await page.keyboard.press("l");
    await expect(doneRail).toBeFocused();

    // Escape leaves the board; with nothing focused k picks the last card of the last
    // non-empty full column - Foxtrot in Retro.
    await page.keyboard.press("Escape");
    await expect
      .poll(() => page.evaluate(() => document.activeElement === document.body))
      .toBe(true);
    await page.keyboard.press("k");
    const foxtrotCard = page.getByRole("article", { name: /Foxtrot/ });
    await expect(foxtrotCard).toBeFocused();

    // B5: ? groups the board's rows under Board and v under Project; the page's own
    // "Project board" region is outside the dialog.
    await page.keyboard.press("?");
    const dialog = page.getByRole("dialog", { name: "Keyboard shortcuts" });
    await expect(dialog).toBeVisible();
    const boardSection = dialog.getByRole("region", { name: "Board", exact: true });
    await expect(boardSection).toBeVisible();
    await expect(boardSection).toContainText("Move card down");
    await expect(
      boardSection.getByRole("listitem").filter({ hasText: "Move card down" }).locator("kbd")
    ).toHaveText(["Shift+J"]);
    await expect(dialog.getByRole("region", { name: "Project", exact: true })).toContainText(
      "Toggle List / Board"
    );
    await page.screenshot({
      path: testInfo.outputPath(`board-shortcut-help-${testInfo.project.name}.png`),
    });
    await page.keyboard.press("Escape");
    await expect(dialog).toHaveCount(0);
    await expect(foxtrotCard).toBeFocused();

    // Mounting straight into the Board (the persisted preference) registers `board` before
    // `project`; the help still lists Global, Project, Board.
    await page.reload();
    await expect(page.getByRole("region", { name: "Todo" }).getByRole("article")).toHaveCount(3);
    await page.locator("body").focus();
    await page.keyboard.press("?");
    await expect(dialog.getByRole("heading", { level: 3 })).toHaveText([
      "Global",
      "Project",
      "Board",
    ]);
  }
);
