import { expect, type Page, test } from "@playwright/test";

import { setLiveSessions } from "./agents";
import { createAsk, createComment, createIssue, createProject, getInbox, patchIssue } from "./api";
import { recordClipboard } from "./clipboard";
import { clearAssigneeFilter, closeAssigneeFilter, pickAssignee } from "./inbox-filter";
import {
  hasReadingPane,
  inboxList,
  inboxPane,
  paneScroller,
  selectedRow,
  selectRow,
} from "./inbox-pane";
import {
  agentRowId,
  replyRowId,
  seedAgentThread,
  seedCommentThread,
  THREAD_SESSION,
} from "./inbox-threads";
import {
  askIdOf,
  goToRow,
  markFromKeyboard,
  openInbox,
  openPane,
  rowSnooze,
  seedInbox,
  seedPane,
  session,
} from "./keyboard-inbox";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

/**
 * The Inbox's keyboard, in the rows whose claim reaches the server: a bulk snooze written and
 * read back, the refusals a pick in flight comes back with, the order the server serves, and
 * the thread row `e` dismisses. What the page draws over one fixed answer - marks, focus,
 * Escape, the names a row's controls carry and the reading pane's own keys - is in
 * `keyboard-inbox.stub.e2e.ts`.
 */

function snoozeRequest(page: Page, askId: string) {
  return page.waitForRequest(
    (request) =>
      request.method() === "PUT" &&
      new URL(request.url()).pathname === `/api/v1/me/asks/${askId}/snooze`
  );
}

test.beforeEach(async () => {
  await resetDatabase();
});

// The rows below are keyboard work on the Inbox list: marks, bulk snoozes, the refusals they
// come back with, and where the focus lands after each. None of them reads a width, a box or a
// touch target, so the phone viewport decides nothing they assert and the describe takes no tag.
test.describe("inbox selection", () => {
  test("x marks rows, h snoozes every marked ask into Later, and Escape clears the selection", async ({
    browser,
  }, testInfo) => {
    await seedInbox();
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openInbox(page);
      const rows = page.locator("[data-inbox-row]");
      const first = await askIdOf(page, 0);
      const second = await askIdOf(page, 1);

      // x marks the row the reader is on; j moves on and x marks the next one.
      await markFromKeyboard(page, 2);
      await expect(rows.nth(0).getByRole("checkbox")).toBeChecked();
      await expect(rows.nth(1).getByRole("checkbox")).toBeChecked();
      await expect(rows.nth(2).getByRole("checkbox")).not.toBeChecked();
      await expect(rows.nth(0).getByRole("checkbox")).toHaveAttribute(
        "aria-label",
        /^Select CORE-\d: \w+ decision$/
      );

      const bar = page.getByRole("group", { name: "Selected rows" });
      await expect(bar).toContainText("2 selected");
      const selectionShot = testInfo.outputPath("inbox-selection.png");
      await page.screenshot({ fullPage: true, path: selectionShot });
      await testInfo.attach("inbox selection", {
        contentType: "image/png",
        path: selectionShot,
      });

      // h with a selection focuses the bulk picker; one pick writes every marked ask.
      const picker = bar.getByRole("combobox", { name: "Snooze selected asks" });
      await page.keyboard.press("h");
      await expect(picker).toBeFocused();
      const writes = Promise.all([snoozeRequest(page, first), snoozeRequest(page, second)]);
      await picker.selectOption("later-today");
      for (const request of await writes) {
        expect(Date.parse(request.postDataJSON().snoozed_until)).toBeGreaterThan(Date.now());
      }

      // Both rows fold into Later, and a snooze that lands takes its rows off the selection.
      await expect(page.getByRole("button", { name: /^Later \(2\)/ })).toBeVisible();
      await expect(page.locator(`[data-inbox-row="${first}"]`)).toHaveCount(0);
      await expect(page.locator(`[data-inbox-row="${second}"]`)).toHaveCount(0);
      await expect(bar).toHaveCount(0);

      // One row marked, the reader back on it: Escape steps off the row first, and clears the
      // selection only once they are off it.
      await goToRow(page, 0);
      await page.keyboard.press("x");
      await expect(bar).toContainText("1 selected");
      await page.keyboard.press("Escape");
      await expect(rows.nth(0)).not.toBeFocused();
      await expect(bar).toContainText("1 selected");
      await page.keyboard.press("Escape");
      await expect(bar).toHaveCount(0);
      await expect(rows.nth(0).getByRole("checkbox")).not.toBeChecked();

      // The row's own checkbox is the mouse path into the same selection.
      await rows.nth(0).getByRole("checkbox").check();
      await expect(bar).toContainText("1 selected");
    } finally {
      await context.close();
    }
  });

  test("a refused bulk snooze names the count and keeps the refused rows marked", async ({
    browser,
  }) => {
    await seedInbox();
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openInbox(page);
      const rows = page.locator("[data-inbox-row]");
      const first = await askIdOf(page, 0);

      await page.route(`**/api/v1/me/asks/${first}/snooze`, (route) =>
        route.fulfill({
          body: JSON.stringify({ code: "INVALID_SNOOZE", error: "snoozed_until must be a moment" }),
          contentType: "application/json",
          status: 400,
        })
      );

      await markFromKeyboard(page, 2);
      const bar = page.getByRole("group", { name: "Selected rows" });
      await bar.getByRole("combobox", { name: "Snooze selected asks" }).selectOption("tomorrow");

      // The reason is the server's own, as the per-row control shows it - a refusal a reader
      // cannot tell from a sign-out is not a report.
      await expect(bar).toContainText("Could not snooze 1 of 2: snoozed_until must be a moment");
      // The refused row keeps its mark and its place; the one that landed left both.
      await expect(bar).toContainText("1 selected");
      await expect(rows.nth(0)).toHaveAttribute("data-inbox-row", first);
      await expect(rows.nth(0).getByRole("checkbox")).toBeChecked();
      await expect(page.getByRole("button", { name: /^Later \(1\)/ })).toBeVisible();

      // Clear is the pointer's way out of the selection, whatever a pick left marked.
      await bar.getByRole("button", { name: "Clear selection" }).click();
      await expect(bar).toHaveCount(0);
      await expect(rows.nth(0).getByRole("checkbox")).not.toBeChecked();
    } finally {
      await context.close();
    }
  });

  // A refusal is about the ids the server refused, not the selection as it happens to stand when
  // that answer comes back. Clear stays live during a pick; when one answer is late and refuses,
  // that row returns marked under its reason while ids the server took stay unmarked.
  test("a late bulk-snooze refusal re-marks only its refused row after Clear", async ({
    browser,
  }) => {
    await seedInbox();
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openInbox(page);
      const rows = page.locator("[data-inbox-row]");
      const refusedAsk = await askIdOf(page, 0);
      const takenAsk = await askIdOf(page, 1);
      const held = Promise.withResolvers<void>();
      const requested = Promise.withResolvers<void>();
      await page.route(`**/api/v1/me/asks/${refusedAsk}/snooze`, async (route) => {
        requested.resolve();
        await held.promise;
        return route.fulfill({
          body: JSON.stringify({ code: "INVALID_SNOOZE", error: "snoozed_until must be a moment" }),
          contentType: "application/json",
          status: 400,
        });
      });

      await markFromKeyboard(page, 2);
      const bar = page.getByRole("group", { name: "Selected rows" });
      await bar.getByRole("combobox", { name: "Snooze selected asks" }).selectOption("tomorrow");
      await requested.promise;
      await expect(bar).toContainText("Snoozing 2…");

      await bar.getByRole("button", { name: "Clear selection" }).click();
      await expect(bar).toContainText("Snoozing 2…");
      held.resolve();

      await expect(bar).toContainText("Could not snooze 1 of 2: snoozed_until must be a moment");
      await expect(bar).toContainText("1 selected");
      await expect(rows.nth(0)).toHaveAttribute("data-inbox-row", refusedAsk);
      await expect(rows.nth(0).getByRole("checkbox")).toBeChecked();
      await page.getByRole("button", { name: /^Later \(1\)/ }).click();
      await expect(
        page.locator(`[data-inbox-row="${takenAsk}"]`).getByRole("checkbox")
      ).not.toBeChecked();
    } finally {
      await context.close();
    }
  });

  // Escape with no row focused is the keyboard's Clear, and it is offered whenever the bar's Clear
  // is - including while a pick is in flight, when the optimistic move has folded the marked rows
  // into Later and the bar counts nothing on the page. It clears every mark, one made under a
  // wider filter included; the late refusal then re-marks only the id it refused.
  test("Escape during a pending bulk snooze clears every mark, and a late refusal re-marks only its own", async ({
    browser,
  }) => {
    await createProject({ key: "CORE", name: "Core" });
    const mine = await createIssue({ project: "CORE", title: "Mine to answer" });
    await patchIssue(mine.key, { assignee: "alice" });
    const refusedAsk = await createAsk(mine.key, { question: "Ship it?" }, session);
    const takenAsk = await createAsk(mine.key, { question: "Hold it?" }, session);
    const theirs = await createIssue({ project: "CORE", title: "Bob answers this" });
    await patchIssue(theirs.key, { assignee: "bob" });
    const hidden = await createAsk(theirs.key, { question: "Wait on it?" }, session);
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      const held = Promise.withResolvers<void>();
      const requested = Promise.withResolvers<void>();
      await page.route(`**/api/v1/me/asks/${refusedAsk.id}/snooze`, async (route) => {
        requested.resolve();
        await held.promise;
        return route.fulfill({
          body: JSON.stringify({ code: "INVALID_SNOOZE", error: "snoozed_until must be a moment" }),
          contentType: "application/json",
          status: 400,
        });
      });
      const checkbox = (askId: string) =>
        page.locator(`[data-inbox-row="${askId}"]`).getByRole("checkbox");

      // A mark made with the filter cleared, on a row the reader's own list does not hold. The
      // filter is driven rather than navigated to: a reload would lose the mark this is about.
      await page.goto("/?assignee=everyone");
      const rows = page.locator("[data-inbox-row]");
      await expect(rows).toHaveCount(3);
      await checkbox(hidden.id).check();
      // Ticking a row also selects it where the reading column is on, and the list keeps the
      // row that column is showing: the reader moves on before the filter narrows, so the mark
      // is all that is left of it here.
      if (hasReadingPane(page)) await page.keyboard.press("j");
      await pickAssignee(page, "alice");
      await closeAssigneeFilter(page);
      await expect(rows).toHaveCount(2);
      await page.evaluate(() => (document.activeElement as HTMLElement | null)?.blur());

      await markFromKeyboard(page, 2);
      const bar = page.getByRole("group", { name: "Selected rows" });
      await expect(bar).toContainText("2 selected");
      await page.keyboard.press("h");
      const picker = bar.getByRole("combobox", { name: "Snooze selected asks" });
      await expect(picker).toBeFocused();
      await picker.selectOption("tomorrow");
      await requested.promise;
      await expect(bar).toContainText("Snoozing 2…");

      // Both marked rows have folded into Later, so Escape leaves the picker for the bar, and
      // Escape from the bar clears the selection while the pick is still in the air.
      await page.keyboard.press("Escape");
      await expect(bar).toBeFocused();
      await page.keyboard.press("Escape");
      await expect(bar).toContainText("Snoozing 2…");
      held.resolve();

      await expect(bar).toContainText("Could not snooze 1 of 2: snoozed_until must be a moment");
      await expect(bar).toContainText("1 selected");
      await expect(checkbox(refusedAsk.id)).toBeChecked();
      await page.getByRole("button", { name: /^Later \(1\)/ }).click();
      await expect(checkbox(takenAsk.id)).not.toBeChecked();
      await clearAssigneeFilter(page);
      await closeAssigneeFilter(page);
      await expect(checkbox(hidden.id)).not.toBeChecked();
    } finally {
      await context.close();
    }
  });

  // In a view that lists only what is waiting on the reader, the optimistic snooze takes every
  // marked row off the list at once, so the bar's own selection empties while its write is still
  // in the air. The refusal that comes back has to survive that.
  test("a refusal reaches the reader even when the optimistic snooze empties the view", async ({
    browser,
  }) => {
    await createProject({ key: "CORE", name: "Core" });
    const issue = await createIssue({ project: "CORE", title: "Needs you" });
    const refusedAsk = await createAsk(issue.key, { question: "First decision" }, session);
    await createAsk(issue.key, { question: "Second decision" }, session);
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      // Only the asks waiting on the reader, from this one agent: a snoozed row leaves this list
      // altogether rather than folding into `Later`.
      await page.goto(`/?agent=${session.actor.id}&section=needs-you`);
      const rows = page.locator("[data-inbox-row]");
      await expect(rows).toHaveCount(2);
      await page.locator("body").focus();

      // Hold one refusal open, so the answer lands well after the optimistic write has emptied
      // the view.
      let refuse: (() => void) | undefined;
      const held = new Promise<void>((resolve) => {
        refuse = resolve;
      });
      await page.route(`**/api/v1/me/asks/${refusedAsk.id}/snooze`, async (route) => {
        await held;
        return route.fulfill({
          body: JSON.stringify({ code: "INVALID_SNOOZE", error: "snoozed_until must be a moment" }),
          contentType: "application/json",
          status: 400,
        });
      });

      await markFromKeyboard(page, 2);
      // Let go of the row: one under the reader's hand is held on screen whatever the list does,
      // and this test is about the write, not that mechanism. With the reading column on the
      // hand is the selection, which has nowhere to be put down, so the row the column is
      // showing is the one row the list keeps.
      if (!hasReadingPane(page)) {
        await page.keyboard.press("Escape");
      }
      await page.mouse.move(0, 0);
      const bar = page.getByRole("group", { name: "Selected rows" });
      await expect(bar).toContainText("2 selected");
      await bar.getByRole("combobox", { name: "Snooze selected asks" }).selectOption("tomorrow");

      // Both rows go at once; the bar stays while its write is in the air, and counts the pick
      // it is waiting on rather than the selection the optimistic move has just emptied. The
      // list itself empties where the reader's hand was let go of above; where the hand is the
      // selection, the row the column is showing is the one the list keeps.
      await expect(rows).toHaveCount(hasReadingPane(page) ? 1 : 0);
      await expect(bar).toHaveCount(1);
      await expect(bar).toContainText("Snoozing 2…");
      await expect(bar).not.toContainText("0 selected");

      // The pick is the one thing in the air: the picker takes no second one while it settles.
      await expect(bar.getByRole("combobox", { name: "Snooze selected asks" })).toBeDisabled();

      refuse?.();

      await expect(bar).toContainText("Could not snooze 1 of 2: snoozed_until must be a moment.");
      await expect(bar).toContainText("1 selected");
      await expect(rows).toHaveCount(1);
      await expect(rows.nth(0)).toHaveAttribute("data-inbox-row", refusedAsk.id);
    } finally {
      await context.close();
    }
  });

  // A mark is the reader's, and outlives the fold that takes its row off the page; what it does
  // not do while it is folded away is count or be written. Opening the band brings both back,
  // and once a refusal is up with nothing rendered under it and no ask left in the selection to
  // snooze, Escape still has somewhere to go.
  test("a folded mark stops counting, comes back with its band, and leaves Escape a way out", async ({
    browser,
  }) => {
    await createProject({ key: "CORE", name: "Core" });
    const issue = await createIssue({ project: "CORE", title: "The only ask" });
    await createAsk(issue.key, { question: "Ship it?" }, session);
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openInbox(page, 1);
      const rows = page.locator("[data-inbox-row]");
      const bar = page.getByRole("group", { name: "Selected rows" });
      const later = page.getByRole("button", { name: /^Later \(1\)/ });

      // Mark it, then snooze it from its own control: it folds into Later.
      await markFromKeyboard(page, 1);
      await expect(bar).toContainText("1 selected");
      await (await rowSnooze(page, await askIdOf(page, 0))).selectOption("tomorrow");
      await expect(later).toBeVisible();
      await expect(rows).toHaveCount(0);
      await expect(bar).toHaveCount(0);

      // The mark itself is still made: the band opens on the row the reader ticked.
      await later.click();
      await expect(rows).toHaveCount(1);
      await expect(rows.nth(0).getByRole("checkbox")).toBeChecked();
      await expect(bar).toContainText("1 selected");

      // A pick the server refuses leaves the row where it was, marked, with the reason up.
      await page.route("**/api/v1/me/asks/*/snooze", (route) =>
        route.fulfill({
          body: JSON.stringify({ code: "INVALID_SNOOZE", error: "snoozed_until must be a moment" }),
          contentType: "application/json",
          status: 400,
        })
      );
      await rows.nth(0).focus();
      await page.keyboard.press("h");
      const picker = bar.getByRole("combobox", { name: "Snooze selected asks" });
      await expect(picker).toBeFocused();
      await picker.selectOption("next-week");
      await expect(bar).toContainText("Could not snooze 1 of 1: snoozed_until must be a moment.");

      // Fold the band again: nothing of the list is rendered, and the bar is all that is left -
      // a count of nothing, Select all, Clear selection and the refusal. The Snooze goes with
      // the ask the fold took off the page: the bar draws it for the asks in the selection, and
      // the selection counts no row the list is not holding.
      await later.click();
      await expect(rows).toHaveCount(0);
      await expect(bar).toContainText("0 selected");
      await expect(picker).toHaveCount(0);
      await expect(bar.getByRole("button", { name: "Clear selection" })).toBeVisible();

      // And the keyboard that raised the refusal dismisses it.
      await page.keyboard.press("Escape");
      await expect(bar).toHaveCount(0);
    } finally {
      await context.close();
    }
  });

  // A folded band is off the page: the reader has no checkbox to untick there, and may well have
  // set that row's moment by hand since marking it. A pick must not write it.
  test("a bulk pick leaves a folded row's own snooze alone", async ({ browser }) => {
    await createProject({ key: "CORE", name: "Core" });
    const first = await createIssue({ project: "CORE", title: "Deferred by hand" });
    const deferred = await createAsk(first.key, { question: "Which release?" }, session);
    const second = await createIssue({ project: "CORE", title: "Still open" });
    const live = await createAsk(second.key, { question: "Ship it?" }, session);
    const moment = async (askId: string) =>
      (await getInbox()).find((row) => row.id === askId)?.snoozed_until ?? null;
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openInbox(page, 2);
      const rows = page.locator("[data-inbox-row]");
      const bar = page.getByRole("group", { name: "Selected rows" });
      const writes: string[] = [];
      await page.route("**/api/v1/me/asks/*/snooze", (route) => {
        writes.push(new URL(route.request().url()).pathname);
        return route.fallback();
      });

      await markFromKeyboard(page, 2);
      await expect(bar).toContainText("2 selected");

      // The reader defers one row themselves; it folds into Later, out of sight but still marked.
      await (await rowSnooze(page, deferred.id)).selectOption("tomorrow");
      await expect(page.getByRole("button", { name: /^Later \(1\)/ })).toBeVisible();
      await expect(rows).toHaveCount(1);
      await expect(rows.nth(0)).toHaveAttribute("data-inbox-row", live.id);
      await expect(bar).toContainText("1 selected");
      await expect.poll(() => moment(deferred.id)).toBeTruthy();
      const byHand = await moment(deferred.id);

      // The bar picks a different moment for what is left on the page. The folded row took its
      // own picker with it, so focus is back on the page and `h` is the bar's.
      await page.mouse.move(0, 0);
      await page.keyboard.press("h");
      await bar.getByRole("combobox", { name: "Snooze selected asks" }).selectOption("next-week");
      await expect.poll(() => moment(live.id)).toBeTruthy();

      expect(await moment(deferred.id)).toBe(byHand);
      expect(writes.length).toBe(2);
    } finally {
      await context.close();
    }
  });

  // One pick at a time: a second while the first is in the air re-sends every id and lets the
  // stale answer overwrite the live one's.
  test("the bulk picker is inert while its pick is in flight", async ({ browser }) => {
    await seedInbox();
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openInbox(page);
      let release: (() => void) | undefined;
      const held = new Promise<void>((resolve) => {
        release = resolve;
      });
      await page.route("**/api/v1/me/asks/*/snooze", async (route) => {
        await held;
        return route.fallback();
      });

      await markFromKeyboard(page, 2);
      const bar = page.getByRole("group", { name: "Selected rows" });
      const picker = bar.getByRole("combobox", { name: "Snooze selected asks" });
      await page.keyboard.press("h");
      await expect(picker).toBeFocused();
      const writes: string[] = [];
      page.on("request", (request) => {
        if (request.method() === "PUT") writes.push(request.url());
      });
      await picker.selectOption("tomorrow");

      // Inert, not taken away: a control that disabled itself would drop the reader to the
      // document mid-write, and Escape would have nothing to leave.
      await expect(bar).toContainText("Snoozing 2…");
      await expect(picker).toBeDisabled();
      await expect(picker).toBeFocused();
      // A second pick from the keyboard the reader still holds: an arrow on a focused native
      // select changes it and fires `change`, and the bar ignores it while its own write is out.
      await page.keyboard.press("ArrowDown");
      expect(writes.length).toBe(2);

      release?.();
      await expect(bar).toHaveCount(0);
      expect(writes.length).toBe(2);
    } finally {
      await context.close();
    }
  });

  // The ordinal is only usable if it counts the way the bands read: top to bottom. The server
  // orders by whose turn it is and how recent the activity is; `Later` is the SPA's own band and
  // sits last whatever the server said, so a snoozed ask is where the two orders come apart.
  test("ask positions follow the order the bands show them, not the server's", async ({
    browser,
  }) => {
    await createProject({ key: "CORE", name: "Core" });
    const issue = await createIssue({ project: "CORE", title: "Two asks, two bands" });
    const deferred = await createAsk(issue.key, { question: "Alpha decision" }, session);
    const handedBack = await createAsk(issue.key, { question: "Beta decision" }, session);
    // A human reply hands Beta's turn to the agents, so the server lists Alpha - still the
    // reader's turn - first.
    await createComment(issue.key, { ask_id: handedBack.id, body: "Looking into it." });
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openInbox(page, 2);
      const rows = page.locator("[data-inbox-row]");

      // Defer Alpha: it drops into `Later`, which renders below every turn band.
      await (await rowSnooze(page, deferred.id)).selectOption("tomorrow");
      await page.getByRole("button", { name: /^Later \(1\)/ }).click();
      await expect(rows).toHaveCount(2);

      // The precondition this test exists for: the server's order is the opposite of the bands'.
      const served = (await getInbox({ login: "alice" })).map((row) => row.id);
      expect(served).toEqual([deferred.id, handedBack.id]);
      await expect(rows.nth(0)).toHaveAttribute("data-inbox-row", handedBack.id);
      await expect(rows.nth(1)).toHaveAttribute("data-inbox-row", deferred.id);

      await expect(rows.nth(0).getByRole("checkbox")).toHaveAttribute(
        "aria-label",
        "Select CORE-1, ask 1 of 2: Beta decision"
      );
      await expect(rows.nth(1).getByRole("checkbox")).toHaveAttribute(
        "aria-label",
        "Select CORE-1, ask 2 of 2: Alpha decision"
      );
    } finally {
      await context.close();
    }
  });
});

/**
 * The Inbox's keys over its thread rows (INFRA-450): a Reply row and an Agent row stand in the
 * same list as the asks, so the movement keys step through them, `o` opens the thread and `y`
 * copies its reference. `x` and the row's own checkbox select a thread row as they select an
 * ask, the bulk bar offering **Dismiss** for the threads it holds, and `e` dismisses the row
 * in hand; `h` is the asks' key and has nothing to act on there.
 */
test("j, k and the arrows step through thread rows, o opens them, y copies only what has a reference, x selects them and e dismisses them", async ({
  browser,
}) => {
  await setLiveSessions([THREAD_SESSION]);
  await createProject({ key: "CORE", name: "Core" });
  const asked = await createIssue({ project: "CORE", title: "Lease handover" });
  const ask = await createAsk(asked.key, { question: "Ship the lease handover?" }, session);
  const threadIssue = await createIssue({ project: "CORE", title: "Replay budget" });
  const thread = await seedCommentThread(
    threadIssue.key,
    "How far back does a replay go?",
    "One hour, then it gives up."
  );
  const agent = await seedAgentThread({ body: "What is the rollout status?", replies: ["Green."] });

  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    const copied = await recordClipboard(page);
    // A thread the column shows is read where it stands and its row stays there, so the walk
    // never has the rows renumbered underfoot.
    await page.goto("/");
    await expect(page.locator("[data-inbox-row]")).toHaveCount(3);
    await page.locator("body").focus();
    test.skip(!hasReadingPane(page), "the selection the keys move is the reading column's");
    const rows = page.locator("[data-inbox-row]");
    const order = await rows.evaluateAll((nodes) =>
      nodes.map((node) => node.getAttribute("data-inbox-row") ?? "")
    );
    expect(new Set(order)).toEqual(new Set([ask.id, replyRowId(thread), agentRowId(agent)]));

    // The keys are pressed from the list, where a reader walking it stands. The column follows
    // the selection and takes no focus of its own: the thread it draws has a standing reply
    // box, not a Reply the reader opened, so the focus the walk needs stays in the list and
    // the arrows stay the list's.
    const press = async (key: string): Promise<void> => {
      await page.keyboard.press(key);
      await expect(inboxList(page).locator(":focus")).toHaveCount(1);
    };
    // The list selects a row for the reader on the first commit that has one, which is before
    // the threads' own read has landed, so which row that is belongs to the list: the walk
    // starts from the top row, picked the way a reader picks it.
    await selectRow(page, order[0] ?? "");
    await expect(selectedRow(page)).toHaveAttribute("data-inbox-row", order[0] ?? "");
    await selectedRow(page).focus();
    for (const next of order.slice(1)) {
      await press("j");
      await expect(selectedRow(page)).toHaveAttribute("data-inbox-row", next);
    }
    for (const previous of [...order].reverse().slice(1)) {
      await press("k");
      await expect(selectedRow(page)).toHaveAttribute("data-inbox-row", previous);
    }
    await press("ArrowDown");
    await expect(selectedRow(page)).toHaveAttribute("data-inbox-row", order[1] ?? "");
    await press("ArrowUp");
    await expect(selectedRow(page)).toHaveAttribute("data-inbox-row", order[0] ?? "");

    // A thread row carries the same checkbox an ask does, and `x` ticks it: the bar counts the
    // row and offers Dismiss for it. `h` snoozes asks alone, so it reaches nothing here and
    // draws no picker to take the focus.
    const threadRow = page.locator(`[data-inbox-row="${replyRowId(thread)}"]`);
    await expect(threadRow.getByRole("checkbox")).toHaveAttribute(
      "aria-label",
      "Select How far back does a replay go?"
    );
    await selectRow(page, replyRowId(thread));
    await page.locator("body").focus();
    await page.keyboard.press("x");
    const bar = page.getByRole("group", { name: "Selected rows" });
    await expect(bar).toContainText("1 selected");
    await expect(bar.getByRole("button", { name: "Dismiss selected threads" })).toBeVisible();
    await page.keyboard.press("h");
    await expect(bar.getByRole("combobox", { name: "Snooze selected asks" })).toHaveCount(0);
    // The mark is the reader's own: a second `x` gives the row back before the walk goes on.
    await page.keyboard.press("x");
    await expect(bar).toHaveCount(0);

    // y copies the thread's own reference.
    await page.keyboard.press("y");
    await expect.poll(copied).toEqual([`dispatch://${threadIssue.key}/comment/${thread}`]);

    // An Agent row has no reference at all, so y writes nothing more; the question beside them
    // still answers to x, which the bulk bar counts.
    await selectRow(page, agentRowId(agent));
    await page.locator("body").focus();
    await page.keyboard.press("y");
    await expect(page.locator("[data-inbox-row]")).toHaveCount(3);
    expect(await copied()).toEqual([`dispatch://${threadIssue.key}/comment/${thread}`]);
    await selectRow(page, ask.id);
    await page.locator("body").focus();
    await page.keyboard.press("x");
    await expect(bar).toContainText("1 selected");

    // `e` dismisses the thread row in hand: the Agent row leaves the list and the two rows
    // beside it stay where they stood.
    await selectRow(page, agentRowId(agent));
    await page.locator("body").focus();
    await page.keyboard.press("e");
    await expect(page.locator(`[data-inbox-row="${agentRowId(agent)}"]`)).toHaveCount(0);
    await expect(page.locator("[data-inbox-row]")).toHaveCount(2);

    // o opens the thread's own page, not the issue it sits on.
    await selectRow(page, replyRowId(thread));
    await page.locator("body").focus();
    await page.keyboard.press("o");
    await expect(page).toHaveURL(new RegExp(`/issues/${threadIssue.key}/comments/${thread}$`));
  } finally {
    await context.close();
  }
});

// ---------------------------------------------------------------------------
// The reading pane's own keyboard (INFRA-963): `l` into the pane, the browser's scrolling keys
// there, and Escape back to the row `l` was pressed on. The pane is drawn from 1280 px, which
// only the `chromium` project is wide enough for, so none of the rows below takes the `@phone`
// tag: on a phone there is no pane to step into. Every fixture here is made up.
// ---------------------------------------------------------------------------

test("l moves the focus into the reading pane, the keyboard scrolls the document there, and Escape returns to the row l was pressed on", async ({
  browser,
}) => {
  const seed = await seedPane();
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await openPane(page, seed);
    const rows = page.locator("[data-inbox-row]");
    await goToRow(page, 0);

    // `l` hands the focus to the pane's one scroller, which is the region the pane names
    // "Question body": one element under both names.
    await page.keyboard.press("l");
    const scroller = paneScroller(page);
    await expect(scroller).toBeFocused();
    const body = inboxPane(page).getByRole("region", { name: "Question body" });
    await expect(body).toHaveCount(1);
    await expect(body).toBeFocused();
    const drawn = await body.elementHandle();
    if (drawn === null) {
      throw new Error("the pane drew no Question body region");
    }
    expect(await scroller.evaluate((node, other) => node === other, drawn)).toBe(true);

    // The document the approval request is for is taller than the column it is drawn in, so
    // the scroller has somewhere to scroll. The editor draws it after the socket syncs.
    await expect
      .poll(() => scroller.evaluate((node) => node.scrollHeight - node.clientHeight))
      .toBeGreaterThan(1000);

    // Every key the browser scrolls a focused box with moves it, and none of them moves the
    // selection: the reader is reading the question the list is still on. The order is a
    // reader's own - down to the bottom, back up to the top, then one line down - so each press
    // has somewhere to go rather than meeting an end it is already at.
    for (const key of ["PageDown", "Space", "End", "ArrowUp", "PageUp", "Home", "ArrowDown"]) {
      const before = await scroller.evaluate((node) => node.scrollTop);
      await page.keyboard.press(key);
      await expect.poll(() => scroller.evaluate((node) => node.scrollTop)).not.toBe(before);
      await expect(scroller).toBeFocused();
      await expect(selectedRow(page)).toHaveAttribute("data-inbox-row", seed.approvalAskId);
    }

    // Escape is the way back out, to the row `l` was pressed on.
    await page.keyboard.press("Escape");
    await expect(rows.nth(0)).toBeFocused();

    // That row is the one `l` was pressed on and not the selected one, which `j` moves while
    // the focus stays on the row: the two come apart, and Escape follows the focus.
    await page.keyboard.press("j");
    await expect(selectedRow(page)).toHaveAttribute("data-inbox-row", seed.cardAskId);
    await expect(rows.nth(0)).toBeFocused();
    await page.keyboard.press("l");
    await expect(scroller).toBeFocused();
    await page.keyboard.press("Escape");
    await expect(rows.nth(0)).toBeFocused();
    await expect(selectedRow(page)).toHaveAttribute("data-inbox-row", seed.cardAskId);
  } finally {
    await context.close();
  }
});
