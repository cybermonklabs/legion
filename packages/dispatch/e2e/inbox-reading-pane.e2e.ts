import { expect, type Page, test } from "@playwright/test";

import { getArtifact, getInbox, resolveAsk } from "./api";
import { inboxList, inboxPane, selectedRow } from "./inbox-pane";
import {
  ADDED_WORD,
  ANCHORED_QUESTION,
  CHANGE_CLASSES,
  CHANGED_BLOCKS,
  DECISION_BLOCK,
  DECISION_QUESTION,
  expectTwoColumns,
  INBOX,
  inboxAt,
  paneOffsets,
  REMOVED_WORD,
  RETIRED_BLOCK,
  SCOPE_ANCHOR,
  seedInbox,
  session,
  WINDOW_ANCHOR,
} from "./inbox-reading-pane";
import { resetDatabase } from "./seed";
import { navigateInApp } from "./sends";
import { asUser } from "./users";

/**
 * The Inbox's reading column (INFRA-575), in the rows whose claim reaches the server: an answer
 * and an approval written and read back, the two withdrawals the server makes while the page
 * watches, the refusal that reads the Inbox back after it, and the row that reads a decision
 * block out of the live document editor. What the column draws over one fixed list - its two
 * shapes of card, its keys and its address, and the widths that take it away - is in
 * `inbox-reading-pane.stub.e2e.ts`; the seed both files read is in `inbox-reading-pane.ts`.
 */

test.beforeEach(async () => {
  await resetDatabase();
});

test("the Inbox shows a decision block in the reading pane and answers it in two taps", async ({
  browser,
}) => {
  const seed = await seedInbox();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const list = inboxList(page);
    const pane = inboxPane(page);
    const block = pane.locator(`[data-dispatch-ask-block="${DECISION_BLOCK}"]`);

    // The column shows from 1280 px, so both widths draw the same two columns, select the same
    // first row, and place the same section heading at the top of the right column.
    for (const width of [1440, 1280]) {
      await page.setViewportSize({ height: 900, width });
      await page.goto(INBOX);
      await expectTwoColumns(page);
      await expect(list.locator("[data-inbox-row]").first()).toHaveAttribute(
        "data-inbox-selected",
        "true"
      );
      await expect(selectedRow(page)).toHaveAttribute("data-inbox-row", seed.decisionAskId);
      await expect(block).toContainText(DECISION_QUESTION);
      await expect(pane.getByTestId("pane-section-bar")).toBeVisible();
      // The column places the section's heading at its top, with the question below it. The
      // editor draws its blocks after the socket syncs, so the placement is polled for.
      await expect
        .poll(async () => {
          const offsets = await paneOffsets(page, DECISION_BLOCK);
          return offsets.heading - offsets.columnTop;
        })
        .toBeLessThanOrEqual(8);
      const placed = await paneOffsets(page, DECISION_BLOCK);
      expect(placed.heading - placed.columnTop).toBeGreaterThanOrEqual(0);
      expect(placed.block).toBeGreaterThan(placed.heading);
      // 36 lines stand between the heading and the question, so the answer is off the column.
      expect(placed.block).toBeGreaterThan(placed.columnBottom);
    }

    // "Skip to the answer" brings the decision block into the column it heads.
    await pane.getByRole("button", { name: "Skip to the answer" }).click();
    await expect
      .poll(async () => {
        const offsets = await paneOffsets(page, DECISION_BLOCK);
        return offsets.block >= offsets.columnTop - 1 && offsets.block < offsets.columnBottom;
      })
      .toBe(true);

    // Two taps answer it: the option, then Answer. The note disclosure stays shut - a reader who
    // picks an option is done - and nothing else is pressed.
    const card = block.locator("article[data-testid^=ask-]");
    const disclosure = card.getByRole("button", {
      name: "Add a note or answer in your own words",
    });
    await expect(disclosure).toHaveAttribute("aria-expanded", "false");
    await card.getByRole("radio", { exact: true, name: "Ledger first" }).click();
    await expect(disclosure).toHaveAttribute("aria-expanded", "false");
    await card.getByRole("button", { exact: true, name: "Answer" }).click();

    // The answered row leaves the list and the Inbox read behind it, and the next row is shown.
    await expect(list.locator(`[data-inbox-row="${seed.decisionAskId}"]`)).toHaveCount(0);
    await expect(selectedRow(page)).toHaveAttribute("data-inbox-row", seed.approvalAskId);
    await expect
      .poll(async () => (await getInbox()).some((row) => row.id === seed.decisionAskId))
      .toBe(false);
  } finally {
    await alice.close();
  }
});

test("the reading pane approves a version and marks what changed since the last approval", async ({
  browser,
}) => {
  const seed = await seedInbox();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.setViewportSize({ height: 900, width: 1440 });
    const pane = inboxPane(page);
    await page.goto(inboxAt(seed.approvalAskId));
    await expect(selectedRow(page)).toHaveAttribute("data-inbox-row", seed.approvalAskId);

    // The column shows the version Approve approves, with Approve at its head. The request is a
    // fresh one at version 4, so the chip is "Awaiting approval" and its action is plain Approve.
    const approve = pane.locator("header").getByRole("button", { exact: true, name: "Approve" });
    await expect(approve).toBeVisible();
    await expect(pane.getByRole("region", { name: "Document version 4" })).toBeVisible();

    // Every block changed since the approved version 3 carries the changed-block class.
    const changed = pane.locator(".dispatch-change-block");
    await expect(changed).toHaveCount(CHANGED_BLOCKS);

    // Inside one changed paragraph the word marks cover the words that changed, and nothing else.
    const words = pane.locator(".dispatch-change-block", { hasText: "The retry budget is" });
    await expect(words).toHaveCount(1);
    await expect(words.locator(".dispatch-change-added")).toHaveText([ADDED_WORD]);
    await expect(words.locator(".dispatch-change-removed-words")).toHaveText([REMOVED_WORD]);

    // The removed block is drawn struck through between the two blocks it sat between.
    const removed = pane.locator(".dispatch-change-removed");
    await expect(removed).toHaveText([RETIRED_BLOCK]);
    const placement = await removed.evaluate((node) => ({
      after: node.nextElementSibling?.textContent ?? null,
      before: node.previousElementSibling?.textContent ?? null,
      struck: window.getComputedStyle(node).textDecorationLine,
    }));
    expect(placement.before).toBe(SCOPE_ANCHOR);
    expect(placement.after).toBe(WINDOW_ANCHOR);
    expect(placement.struck).toContain("line-through");

    // A paragraph both versions hold carries no mark at all.
    const unchanged = pane.locator("p", { hasText: SCOPE_ANCHOR });
    await expect(unchanged).toHaveCount(1);
    await expect(unchanged).not.toHaveClass(/dispatch-change-block/);
    await expect(
      unchanged.locator(".dispatch-change-added, .dispatch-change-removed-words")
    ).toHaveCount(0);

    // One tap approves the version the column is showing.
    await approve.click();
    await expect
      .poll(async () => (await getArtifact(seed.approvalArtifactId, { login: "alice" })).approval)
      .toMatchObject({ state: "approved", version: 4 });
    await expect(inboxList(page).locator(`[data-inbox-row="${seed.approvalAskId}"]`)).toHaveCount(
      0
    );

    // A first request has no approval to compare against, so the column marks nothing.
    // The reader moves to it in the list, as a tap does: this row proves what the column marks,
    // not what a cold load of its address draws.
    await navigateInApp(page, inboxAt(seed.firstApprovalAskId));
    await expect(selectedRow(page)).toHaveAttribute("data-inbox-row", seed.firstApprovalAskId);
    await expect(pane.getByRole("region", { name: "Document version 1" })).toBeVisible();
    for (const mark of CHANGE_CLASSES) {
      await expect(pane.locator(`.${mark}`)).toHaveCount(0);
    }
  } finally {
    await alice.close();
  }
});

/**
 * Whether the reading column holds `text` where a reader can read and copy it: as drawn text, or
 * as the value of a box it keeps. The column may keep a reader's unsent answer either way, so the
 * row reads the behaviour rather than one markup.
 */
function paneHoldsText(page: Page, text: string): Promise<boolean> {
  return page.evaluate((wanted) => {
    const pane = document.querySelector<HTMLElement>('[data-testid="inbox-pane"]');
    if (pane === null) return false;
    if ((pane.textContent ?? "").includes(wanted)) return true;
    return [
      ...pane.querySelectorAll<HTMLTextAreaElement | HTMLInputElement>("textarea, input"),
    ].some((field) => field.value.includes(wanted));
  }, text);
}

test("a question the server withdraws leaves the list at once and the next row is selected", async ({
  browser,
}) => {
  const seed = await seedInbox();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.setViewportSize({ height: 900, width: 1440 });
    const list = inboxList(page);
    const pane = inboxPane(page);
    await page.goto(INBOX);
    await expect(selectedRow(page)).toHaveAttribute("data-inbox-row", seed.decisionAskId);

    // The server takes the selected question away. The reader has typed nothing into it: the
    // Inbox selected its row for them when the page opened.
    await resolveAsk(
      seed.decisionAskId,
      { kind: "retracted", reason: "A newer question supersedes this one." },
      session
    );

    // It leaves at once, with no reload and no second look: a withdrawn question with a live
    // answer form invites an answer the server refuses. The next visible row in list order is
    // selected, as after an answer in the column, and the address follows it.
    await expect(list.locator(`[data-inbox-row="${seed.decisionAskId}"]`)).toHaveCount(0);
    await expect(selectedRow(page)).toHaveAttribute("data-inbox-row", seed.approvalAskId);
    await expect(page).toHaveURL(inboxAt(seed.approvalAskId));
    await expect(
      pane.locator("header").getByRole("button", { exact: true, name: "Approve" })
    ).toBeVisible();
    await expect(pane.getByTestId(`ask-${seed.decisionAskId}`)).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

test("a withdrawn question the reader was answering keeps its text in the column, closed", async ({
  browser,
}) => {
  const seed = await seedInbox();
  const alice = await asUser(browser, "alice");
  const typed = "Only on a cold start, and only back to the last lease.";
  try {
    const page = await alice.newPage();
    await page.setViewportSize({ height: 900, width: 1440 });
    const pane = inboxPane(page);
    await page.goto(inboxAt(seed.anchoredAskId));
    const card = pane.getByTestId(`ask-${seed.anchoredAskId}`);
    await expect(card).toContainText(ANCHORED_QUESTION);

    // The reader is writing an answer to this question when the server takes it away.
    await card.locator("[data-ask-answer]").fill(typed);
    await resolveAsk(
      seed.anchoredAskId,
      { kind: "retracted", reason: "Answered on another issue." },
      session
    );

    // The one exception to the rule above: the column keeps the question, shows its closed state
    // in place of the answer form, and keeps what the reader typed where they can copy it.
    await expect(card.getByTestId("ask-resolution-badge")).toBeVisible();
    await expect(card.getByRole("button", { exact: true, name: "Answer" })).toHaveCount(0);
    await expect(card.locator("[data-ask-answer]")).toHaveCount(0);
    await expect.poll(() => paneHoldsText(page, typed)).toBe(true);

    // Until the reader selects another row, which shows that row's question and nothing of this
    // one: the question is gone from the Inbox and its row with it.
    await page.keyboard.press("j");
    await expect(selectedRow(page)).not.toHaveAttribute("data-inbox-row", seed.anchoredAskId);
    await expect(pane.getByTestId(`ask-${seed.anchoredAskId}`)).toHaveCount(0);
    await expect(inboxList(page).locator(`[data-inbox-row="${seed.anchoredAskId}"]`)).toHaveCount(
      0
    );
  } finally {
    await alice.close();
  }
});

test("an options-only answer the server refuses keeps its question in the column, with a Retry", async ({
  browser,
}) => {
  const seed = await seedInbox();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.setViewportSize({ height: 900, width: 1440 });
    const list = inboxList(page);
    const pane = inboxPane(page);
    await page.goto(INBOX);
    await expect(selectedRow(page)).toHaveAttribute("data-inbox-row", seed.decisionAskId);

    // The server refuses this one answer, in the words of its own route, and refuses it for a
    // reason the same answer may get past: the card offers Retry rather than closing its form.
    await page.route(`**/api/v1/asks/${seed.decisionAskId}/answer`, (route) =>
      route.fulfill({
        body: JSON.stringify({
          code: "ASK_REVISION_REQUIRED",
          error: "expected_edited_at is required",
        }),
        contentType: "application/json",
        status: 400,
      })
    );

    // The two taps of the row above, and nothing typed: with no draft to keep the question
    // here, the answer itself is what holds it.
    const block = pane.locator(`[data-dispatch-ask-block="${DECISION_BLOCK}"]`);
    await expect(block).toContainText(DECISION_QUESTION);
    await pane.getByRole("button", { name: "Skip to the answer" }).click();
    const card = block.locator("article[data-testid^=ask-]");
    await card.getByRole("radio", { exact: true, name: "Ledger first" }).click();
    await card.getByRole("button", { exact: true, name: "Answer" }).click();

    // The refusal is in the card that wrote it, on the question it was written to, and the
    // reader can send the same answer again from there. The selection never moved on, and the
    // row the optimistic removal took is listed again - the server still has the question open.
    await expect(card.getByRole("alert")).toContainText("Could not save your answer.");
    await expect(card.getByRole("alert").getByRole("button", { name: "Retry" })).toBeVisible();
    await expect(block).toContainText(DECISION_QUESTION);
    await expect(page).toHaveURL(inboxAt(seed.decisionAskId));
    await expect(list.locator(`[data-inbox-row="${seed.decisionAskId}"]`)).toHaveCount(1);
    await expect(selectedRow(page)).toHaveAttribute("data-inbox-row", seed.decisionAskId);
    await expect
      .poll(async () => (await getInbox()).some((row) => row.id === seed.decisionAskId))
      .toBe(true);
  } finally {
    await alice.close();
  }
});

test("Enter opens the selected decision block's note field and puts the focus in it", async ({
  browser,
}) => {
  const seed = await seedInbox();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.setViewportSize({ height: 900, width: 1440 });
    const pane = inboxPane(page);
    await page.goto(INBOX);
    await expect(selectedRow(page)).toHaveAttribute("data-inbox-row", seed.decisionAskId);
    const card = pane
      .locator(`[data-dispatch-ask-block="${DECISION_BLOCK}"]`)
      .locator("article[data-testid^=ask-]");
    const disclosure = card.getByRole("button", {
      name: "Add a note or answer in your own words",
    });
    await expect(disclosure).toHaveAttribute("aria-expanded", "false");

    // One Enter, the key the Inbox offers for answering the selected question: the note field
    // opens AND the reader is in it. A field that opens without the focus leaves every later
    // keystroke to the Inbox's own keys - `j` walks the list while the reader believes they are
    // writing an answer.
    await page.keyboard.press("Enter");
    await expect(disclosure).toHaveAttribute("aria-expanded", "true");
    await expect(card.locator("[data-ask-answer]")).toBeFocused();

    // What the reader types then reaches the answer, and the selection stays where it was.
    await page.keyboard.type("Ledger first, for the lease.");
    await expect(card.locator("[data-ask-answer]")).toHaveValue("Ledger first, for the lease.");
    await expect(selectedRow(page)).toHaveAttribute("data-inbox-row", seed.decisionAskId);
  } finally {
    await alice.close();
  }
});
