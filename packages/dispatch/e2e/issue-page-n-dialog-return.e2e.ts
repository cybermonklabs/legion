/** The row seeds an issue and its comment threads through the API and reads
 *  `/issues/<key>/conversation` with the new issue page on: the server decides the threads and the
 *  rows of the tree drawn from its answer, on which the row drives a thread row's Copy reference
 *  and the command palette. */

import { seedWholeIssue } from "./issue-layout-seed";
import { gotoIssue, phase } from "./issue-page";
import {
  DESKTOP,
  expectIssuePageDrawn,
  expectOpeningLanded,
  focusRow,
  unfoldEveryBranch,
} from "./issue-page-g";
import { expect, test } from "./test";

// Decision 396 (3): from an act of a focused row, Copy reference, Ctrl+K and then Escape give the
// focus back to the row that held the act, which went with the focus when the palette took it.
// SLOW(>=2s): it seeds a whole issue, loads the page, unfolds every branch and drives the palette.
test("a dialog opened from a row's act gives the focus back to the row", async ({ browser }) => {
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    await page.setViewportSize({ height: DESKTOP.height, width: DESKTOP.width });
    await gotoIssue(page, `/issues/${seeded.key}/conversation`);
    await expectIssuePageDrawn(page);
    await expectOpeningLanded(page);
    await unfoldEveryBranch(page);
    const [thread = ""] = seeded.comments.threads;
    const row = await focusRow(page, thread);
    const copy = row.locator("[data-row-copy]");
    await expect(copy, "the focused row draws no Copy reference").toBeVisible();
    await copy.focus();
    await page.keyboard.press("ControlOrMeta+k");
    await expect(page.getByRole("dialog"), "Ctrl+K opens no dialog").toBeVisible();
    await expect(copy, "the act stayed drawn once the row lost the focus").toHaveCount(0);
    await page.keyboard.press("Escape");
    await expect(page.getByRole("dialog"), "Escape leaves the dialog open").toHaveCount(0);
    await expect(
      row,
      "the dialog's close left the focus off the row that held the act"
    ).toBeFocused();
  });
});
