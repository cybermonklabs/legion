import { expect, type Page, test } from "@playwright/test";

import { createIssue, createProject, getIssue } from "./api";
import { openBoard, seedBoard } from "./keyboard";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

/**
 * The global keyboard, in the two rows whose claim reaches the server: the create dialog, whose
 * issue the API mints and the SPA then opens, and the card moves, each of which is a status and
 * rank write read back. The chords, the shortcut help, the Inbox selection and the board's
 * roving are in `keyboard.stub.e2e.ts`.
 */

test.beforeEach(async () => {
  await resetDatabase();
});

// @pr: Create through answer to close: the create itself - c opens the dialog, and the issue the API returns is the one the SPA opens.
test("c opens the create dialog; project and title create an issue the API returns and the SPA opens @pr", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  await createProject({ key: "OPS", name: "Operations" });
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.goto("/");
    await expect(page.getByRole("heading", { name: "Inbox" })).toBeVisible();
    await page.locator("body").focus();

    await page.keyboard.press("c");
    const dialog = page.getByRole("dialog", { name: "Create issue" });
    await expect(dialog).toBeVisible();
    const title = dialog.getByRole("textbox", { name: "Title" });
    await expect(title).toBeFocused();
    // `c` inside the dialog's own inputs types; it never reopens or steals the key.
    await title.fill("c");
    await expect(title).toHaveValue("c");
    await dialog.getByRole("combobox", { name: "Project" }).selectOption("OPS");
    await title.fill("Keyboard-created issue");
    await page.screenshot({
      path: testInfo.outputPath(`create-issue-${testInfo.project.name}.png`),
    });
    await title.press("Enter");

    await expect(page).toHaveURL(/\/issues\/OPS-1(\/|$)/);
    await expect(dialog).toHaveCount(0);
    await expect(
      page
        .getByTestId("issue-header")
        .getByRole("heading", { level: 1, name: "Keyboard-created issue" })
    ).toBeVisible();
    await expect
      .poll(() => getIssue("OPS-1"))
      .toMatchObject({
        project: "OPS",
        title: "Keyboard-created issue",
      });

    // Escape closes without creating, and focus returns to the caller.
    await page.locator("main").focus();
    await page.keyboard.press("c");
    await expect(dialog).toBeVisible();
    await page.keyboard.press("Escape");
    await expect(dialog).toHaveCount(0);
    await expect(page.locator("main")).toBeFocused();
  } finally {
    await context.close();
  }
});

function patchOf(page: Page, key: string) {
  return page.waitForResponse(
    (response) =>
      response.request().method() === "PATCH" &&
      new URL(response.url()).pathname === `/api/v1/issues/${key}`
  );
}

test("Shift+L/H move a focused card to the top of the adjacent status through the one move path: into Done closes, out of Done reopens, Triage has no previous", async ({
  browser,
}) => {
  const { bravo, delta, echo, foxtrot } = await seedBoard();
  const golf = await createIssue({ project: "CORE", title: "Golf" });
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.setViewportSize({ width: 1920, height: 900 });
    await openBoard(page);
    const live = page.getByRole("status", { name: "Board announcements" });
    const inProgress = page.getByRole("region", { name: "In progress" });
    const retro = page.getByRole("region", { name: "Retro" });

    // B3: Shift+L lands at the TOP of the next status (Linear's rule) and focus follows the
    // card into its new column.
    const bravoCard = page.getByRole("article", { name: `${bravo.key} Bravo` });
    await bravoCard.focus();
    const forwardPatch = patchOf(page, bravo.key);
    await page.keyboard.press("Shift+L");
    const forward = await forwardPatch;
    expect(forward.status()).toBe(200);
    expect(forward.request().postDataJSON()).toEqual({
      rank: { before: delta.key },
      status: "in_progress",
    });
    await expect(inProgress.getByRole("article")).toHaveText([/Bravo/, /Delta/]);
    await expect(inProgress.getByRole("article", { name: `${bravo.key} Bravo` })).toBeFocused();
    await expect(live).toHaveText(`${bravo.key} → In progress, position 1 of 2`);

    // Into the collapsed Done rail: the same PATCH a drop on the rail sends, the issue closes,
    // and focus lands on the rail since it renders no card.
    const echoCard = page.getByRole("article", { name: `${echo.key} Echo` });
    await echoCard.focus();
    const closePatch = patchOf(page, echo.key);
    await page.keyboard.press("Shift+L");
    expect((await closePatch).request().postDataJSON()).toEqual({ status: "done", rank: {} });
    const doneRail = page.getByRole("region", { name: "Done (collapsed)" });
    await expect(doneRail).toContainText("1");
    await expect(doneRail).toBeFocused();
    await expect(live).toHaveText(`${echo.key} → Done, closed`);
    await expect.poll(() => getIssue(echo.key)).toMatchObject({ status: "done" });
    expect((await getIssue(echo.key)).closed_at).not.toBeNull();

    // Out of Done reopens: with the edges shown, Shift+H from the Done column.
    await page.getByRole("button", { name: "Show Icebox & Done" }).click();
    const doneColumn = page.getByRole("region", { name: "Done", exact: true });
    const echoInDone = doneColumn.getByRole("article", { name: `${echo.key} Echo` });
    await expect(echoInDone).toBeVisible();
    await echoInDone.focus();
    const reopenPatch = patchOf(page, echo.key);
    await page.keyboard.press("Shift+H");
    expect((await reopenPatch).request().postDataJSON()).toEqual({
      rank: { before: foxtrot.key },
      status: "retro",
    });
    await expect(retro.getByRole("article")).toHaveText([/Echo/, /Foxtrot/]);
    await expect(retro.getByRole("article", { name: `${echo.key} Echo` })).toBeFocused();
    await expect(live).toHaveText(`${echo.key} → Retro, position 1 of 2`);
    await expect.poll(() => getIssue(echo.key)).toMatchObject({ closed_at: null, status: "retro" });

    // Triage has no previous status: nothing is sent and focus stays put.
    const patches: string[] = [];
    page.on("request", (request) => {
      if (request.method() === "PATCH") {
        patches.push(request.url());
      }
    });
    const golfCard = page.getByRole("article", { name: `${golf.key} Golf` });
    await golfCard.focus();
    await page.keyboard.press("Shift+H");
    await page.waitForTimeout(300);
    expect(patches).toEqual([]);
    await expect(golfCard).toBeFocused();
  } finally {
    await context.close();
  }
});
