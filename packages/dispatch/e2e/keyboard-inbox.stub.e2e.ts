/** The Inbox's keyboard, in the rows that read only what the page draws: where a mark goes,
 *  where Escape leaves the reader, what each row's controls are named, what a narrowed list
 *  counts, and the reading pane's own `l` and Escape.
 *
 *  None of them writes and reads that write back: a bulk snooze whose answer this layer would
 *  have to invent, and the thread walk whose `e` dismisses a row, stayed in `keyboard-inbox.e2e.ts`
 *  with the rest (packages/dispatch/AGENTS.md, "The stubbed layer"). Every refusal below is the
 *  row's own `page.route`, which replays as readily as it ran.
 */
import { expect } from "@playwright/test";

import { createAsk, createIssue, createProject, patchIssue } from "./api";
import {
  assigneeFilter,
  assigneeOptions,
  closeAssigneeFilter,
  openAssigneeFilter,
  pickAssignee,
} from "./inbox-filter";
import { hasReadingPane, inboxPane, paneScroller, selectedRow } from "./inbox-pane";
import {
  askIdOf,
  goToRow,
  markFromKeyboard,
  openInbox,
  openPane,
  RETRY_QUESTION,
  rowSnooze,
  seedInbox,
  seedPane,
  session,
} from "./keyboard-inbox";
import { stubbed } from "./stub/stub-test";

/** The three asks the marks are made on. */
const test = stubbed(async () => {
  await seedInbox();
  return { rows: 3 };
});

/** Two questions on one issue and a third on its own, for the names the controls carry. */
const twinsTest = stubbed(async () => {
  await createProject({ key: "CORE", name: "Core" });
  const twins = await createIssue({ project: "CORE", title: "Two asks on one issue" });
  // Two questions that agree far past any cut: only the position tells these two apart.
  await createAsk(
    twins.key,
    { question: "Which of the two long-running migration strategies should we adopt: A?" },
    session
  );
  await createAsk(
    twins.key,
    { question: "Which of the two long-running migration strategies should we adopt: B?" },
    session
  );
  const alone = await createIssue({ project: "CORE", title: "Release checklist" });
  await createAsk(alone.key, { question: "Ship it?" }, session);
  return { rows: 3 };
});

/** One ask of the reader's own and one of Bob's, so narrowing to the reader takes a row off the
 *  list while its mark is still made. */
const assigneeTest = stubbed(async () => {
  await createProject({ key: "CORE", name: "Core" });
  const mine = await createIssue({ project: "CORE", title: "Mine to answer" });
  await patchIssue(mine.key, { assignee: "alice" });
  await createAsk(mine.key, { question: "Ship it?" }, session);
  const theirs = await createIssue({ project: "CORE", title: "Bob answers this" });
  await patchIssue(theirs.key, { assignee: "bob" });
  const hidden = await createAsk(theirs.key, { question: "Hold it?" }, session);
  return { hiddenAskId: hidden.id };
});

/** Two agents asking on one issue, so the `?agent=` chip narrows the list to one of them. */
const agentFilterTest = stubbed(async () => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Two agents, one issue" });
  await patchIssue(issue.key, { assignee: "alice" });
  const other = {
    actor: { id: "e2e-other-agent", kind: "session" as const },
    as: "agent" as const,
  };
  const fromOther = await createAsk(issue.key, { question: "From another agent?" }, other);
  const fromSession = await createAsk(issue.key, { question: "Ship it?" }, session);
  return {
    agent: other.actor.id,
    fromOtherAskId: fromOther.id,
    fromSessionAskId: fromSession.id,
  };
});

/** One question to mark and one with options to answer. */
const answeringTest = stubbed(async () => {
  await createProject({ key: "CORE", name: "Core" });
  const marked = await createIssue({ project: "CORE", title: "Marked" });
  await createAsk(marked.key, { question: "Which release?" }, session);
  const answering = await createIssue({ project: "CORE", title: "Answering" });
  await createAsk(
    answering.key,
    { options: [{ label: "Ship" }, { label: "Hold" }], question: "Which way?" },
    session
  );
  return { rows: 2 };
});

/** The reading pane's two questions: a request to approve a document, and a question in none. */
const paneTest = stubbed(seedPane);

/** Nothing at all, for the Inbox that lists nothing. */
const emptyTest = stubbed(async () => ({ rows: 0 }));

// The rows below are keyboard work on the Inbox list: marks, bulk snoozes, the refusals they
// come back with, and where the focus lands after each. None of them reads a width, a box or a
// touch target, so the phone viewport decides nothing they assert and the describe takes no tag.
test.describe("inbox selection", () => {
  // Two rows can fail for two reasons; the bar names the first and says there are others rather
  // than flattening a sign-out and a validation refusal into one sentence.
  test("a bulk snooze refused two different ways names the first reason and says so @pr", async ({
    page,
    scene,
  }) => {
    await openInbox(page, scene.rows);
    const first = await askIdOf(page, 0);
    const second = await askIdOf(page, 1);

    await page.route(`**/api/v1/me/asks/${first}/snooze`, (route) =>
      route.fulfill({
        body: JSON.stringify({ code: "INVALID_SNOOZE", error: "snoozed_until must be a moment" }),
        contentType: "application/json",
        status: 400,
      })
    );
    await page.route(`**/api/v1/me/asks/${second}/snooze`, (route) =>
      route.fulfill({
        body: JSON.stringify({ code: "UNAUTHENTICATED", error: "sign in again" }),
        contentType: "application/json",
        status: 401,
      })
    );

    await markFromKeyboard(page, 2);
    const bar = page.getByRole("group", { name: "Selected rows" });
    await bar.getByRole("combobox", { name: "Snooze selected asks" }).selectOption("tomorrow");

    await expect(bar).toContainText(
      "Could not snooze 2 of 2: snoozed_until must be a moment, and 1 other reason."
    );
    await expect(bar).toContainText("2 selected");
  });

  // Ticking a box with the pointer leaves focus on the `<input>`, and the registry's editable
  // policy decides whether the next key is a shortcut or typing. A checkbox takes no text.
  test("keys still work with a pointer-ticked checkbox holding focus @pr", async ({
    page,
    scene,
  }) => {
    await openInbox(page, scene.rows);
    const rows = page.locator("[data-inbox-row]");
    await rows.nth(0).getByRole("checkbox").check();
    await rows.nth(1).getByRole("checkbox").check();
    await expect(rows.nth(1).getByRole("checkbox")).toBeFocused();
    const bar = page.getByRole("group", { name: "Selected rows" });
    await expect(bar).toContainText("2 selected");

    // `h` from the box the reader just ticked is the flow the pointer leaves them in: it
    // reaches the bulk picker with no roving in between.
    await page.keyboard.press("h");
    const picker = bar.getByRole("combobox", { name: "Snooze selected asks" });
    await expect(picker).toBeFocused();
    await page.keyboard.press("Escape");

    // j moves off the ticked row, and h reaches the picker from there too. A tick marks its
    // row and moves nothing else, so the reading column is still on the row the Inbox opened
    // on and the step from there is the second row; without the column j roves from the box
    // holding the focus and lands on the third.
    await page.keyboard.press("j");
    if (hasReadingPane(page)) {
      await expect(selectedRow(page)).toHaveAttribute("data-inbox-row", await askIdOf(page, 1));
    } else {
      await expect(rows.nth(2)).toBeFocused();
    }
    await page.keyboard.press("h");
    await expect(picker).toBeFocused();
  });

  // Escape is always one level out. The per-row picker honours it through `back`; the bulk picker
  // has no row ancestor, so it needs its own way back to the list.
  test("Escape leaves the bulk picker for the row it was opened from, keeping the marks @pr", async ({
    page,
    scene,
  }) => {
    await openInbox(page, scene.rows);
    const rows = page.locator("[data-inbox-row]");

    // Control: the Snooze the reader reaches for the row they are on, which already returns
    // to that row.
    await goToRow(page, 0);
    const rowPicker = await rowSnooze(page, await askIdOf(page, 0));
    await page.keyboard.press("h");
    await expect(rowPicker).toBeFocused();
    await page.keyboard.press("Escape");
    await expect(rows.nth(0)).toBeFocused();

    // The bulk picker does the same, and leaves the selection alone on the way.
    await goToRow(page, 1);
    await page.keyboard.press("x");
    const bar = page.getByRole("group", { name: "Selected rows" });
    await expect(bar).toContainText("1 selected");
    await page.keyboard.press("h");
    const picker = bar.getByRole("combobox", { name: "Snooze selected asks" });
    await expect(picker).toBeFocused();
    await page.keyboard.press("Escape");
    await expect(rows.nth(1)).toBeFocused();
    await expect(bar).toContainText("1 selected");

    // And from the row, Escape is the two levels it always was: off the row, then the marks.
    await page.keyboard.press("Escape");
    await page.keyboard.press("Escape");
    await expect(bar).toHaveCount(0);
  });

  // Escape is one level out, and while the assignee popover is open it is the innermost level:
  // it closes, and the selection the same key would otherwise clear stays made. Two undos for
  // one press is what the page's binding stands down for (`clear-selection`'s `!filterOpen`).
  test("Escape inside the assignee popover closes it and clears nothing @pr", async ({
    page,
    scene,
  }) => {
    await openInbox(page, scene.rows);
    const rows = page.locator("[data-inbox-row]");
    const bar = page.getByRole("group", { name: "Selected rows" });
    await markFromKeyboard(page, 1);
    await expect(bar).toContainText("1 selected");
    // No row focused, so the page's Escape is the Clear the popover now stands in front of.
    await page.evaluate(() => (document.activeElement as HTMLElement | null)?.blur());

    await openAssigneeFilter(page);
    await expect(page.getByRole("combobox", { name: "Search people" })).toBeFocused();
    await page.keyboard.press("Escape");
    await expect(assigneeOptions(page)).toHaveCount(0);
    await expect(assigneeFilter(page)).toBeFocused();
    await expect(bar).toContainText("1 selected");
    await expect(rows.nth(0).getByRole("checkbox")).toBeChecked();

    // With the popover shut, the same key clears the selection, as it always did.
    await page.keyboard.press("Escape");
    await expect(bar).toHaveCount(0);
    await expect(rows.nth(0).getByRole("checkbox")).not.toBeChecked();
  });

  // Two asks on one issue are two different questions, and a control named after the issue alone
  // says the same thing twice.
  twinsTest("each row's controls are named after its own ask @pr", async ({ page, scene }) => {
    await openInbox(page, scene.rows);
    const rows = page.locator("[data-inbox-row]");
    const named = async (attribute: string) =>
      rows.evaluateAll(
        (items, selector: string) =>
          items.map((item) => item.querySelector(selector)?.getAttribute("aria-label") ?? "none"),
        attribute
      );

    // An issue with one listed ask keeps the short name; the twins carry their position.
    const checkboxes = await named("input[type=checkbox]");
    expect(checkboxes).toContain("Select CORE-2: Ship it?");
    expect(checkboxes.filter((name) => name.startsWith("Select CORE-1")).sort()).toEqual([
      "Select CORE-1, ask 1 of 2: Which of the two long-running migration strategi…",
      "Select CORE-1, ask 2 of 2: Which of the two long-running migration strategi…",
    ]);
    expect(new Set(checkboxes).size).toBe(checkboxes.length);

    // Snooze is named from the same place, so the two controls of a row agree. Without the
    // reading column every row carries its own Snooze, so the names tell the twins apart by
    // their position exactly as the checkboxes do. With the column one Snooze is on the page
    // - the head's, for the row the column is showing - so it names that question without
    // the position, which is only there to tell two controls on one page apart.
    const pickers: string[] = [];
    for (const askId of await rows.evaluateAll((items) =>
      items.map((item) => item.getAttribute("data-inbox-row") ?? "")
    )) {
      const picker = await rowSnooze(page, askId);
      pickers.push((await picker.getAttribute("aria-label")) ?? "none");
    }
    if (hasReadingPane(page)) {
      expect(pickers).toEqual(
        checkboxes.map((name) =>
          name.replace(/^Select /, "Snooze ").replace(/, ask \d+ of \d+/, "")
        )
      );
    } else {
      expect(pickers).toContain("Snooze CORE-2: Ship it?");
      expect(new Set(pickers).size).toBe(pickers.length);
      expect(pickers.map((name) => name.replace(/^Snooze /, "")).sort()).toEqual(
        checkboxes.map((name) => name.replace(/^Select /, "")).sort()
      );
    }
  });

  // The bar speaks for the rows this list holds. A mark made with the filter cleared must not
  // follow the reader into their own list, where its row is not on screen and a bulk snooze
  // would write unseen.
  assigneeTest(
    "a mark made under a wider filter neither counts nor snoozes once its row is out of the list @pr",
    async ({ page, scene }) => {
      await page.goto("/?assignee=everyone");
      const rows = page.locator("[data-inbox-row]");
      await expect(rows).toHaveCount(2);
      await page.locator("body").focus();
      const bar = page.getByRole("group", { name: "Selected rows" });

      const hiddenRow = page.locator(`[data-inbox-row="${scene.hiddenAskId}"]`);
      await hiddenRow.getByRole("checkbox").check();
      await expect(bar).toContainText("1 selected");

      let wrote = false;
      await page.route(`**/api/v1/me/asks/${scene.hiddenAskId}/snooze`, (route) => {
        wrote = true;
        return route.fallback();
      });
      // Narrowing to the reader alone, through the control: the Inbox stays mounted, so the
      // mark is still made while its row is off the list. The tick also selected that row
      // where the reading column is on, and the list keeps the row it is showing, so the
      // reader moves on first - the mark is what this test follows, not the hold.
      if (hasReadingPane(page)) await page.keyboard.press("j");
      await pickAssignee(page, "alice");
      await closeAssigneeFilter(page);
      await expect(rows).toHaveCount(1);
      await expect(hiddenRow).toHaveCount(0);
      await expect(bar).toHaveCount(0);

      // Nothing is marked here, so `h` reaches the Snooze of the row the reader is on, not a
      // bulk write.
      const onlyPicker = await rowSnooze(page, await askIdOf(page, 0));
      await goToRow(page, 0);
      await page.keyboard.press("h");
      await expect(onlyPicker).toBeFocused();
      expect(wrote).toBe(false);
    }
  );

  // The `?agent=` chip narrows the list the same way the assignee filter does, and the Inbox
  // stays mounted across it: the chip's clear link and the browser's own Back are client-side
  // moves.
  agentFilterTest(
    "a mark made outside the agent filter does not count inside it @pr",
    async ({ page, scene }) => {
      await page.goto(`/?agent=${scene.agent}`);
      const rows = page.locator("[data-inbox-row]");
      const bar = page.getByRole("group", { name: "Selected rows" });
      await expect(rows).toHaveCount(1);
      await expect(rows.nth(0)).toHaveAttribute("data-inbox-row", scene.fromOtherAskId);

      await page.getByRole("link", { name: "Clear agent filter" }).click();
      await expect(rows).toHaveCount(2);
      await page
        .locator(`[data-inbox-row="${scene.fromSessionAskId}"]`)
        .getByRole("checkbox")
        .check();
      await expect(bar).toContainText("1 selected");

      // Let go of the row first: a row under the reader's hand is held on screen even when the
      // list stops carrying it, which is the Inbox's own rule and not what this test is about.
      await page.mouse.move(0, 0);
      await page.evaluate(() => (document.activeElement as HTMLElement | null)?.blur());

      // Back into the filter: the marked ask is not one of its rows, so nothing counts here.
      await page.goBack();
      await expect(rows).toHaveCount(1);
      await expect(rows.nth(0)).toHaveAttribute("data-inbox-row", scene.fromOtherAskId);
      await expect(bar).toHaveCount(0);
    }
  );

  // The keyboard raised the refusal, so the keyboard dismisses it - even once the reader has
  // unticked the rows it named and the bar is counting nothing.
  test("Escape dismisses a refusal the reader has already unticked @pr", async ({
    page,
    scene,
  }) => {
    await openInbox(page, scene.rows);
    await page.route("**/api/v1/me/asks/*/snooze", (route) =>
      route.fulfill({
        body: JSON.stringify({ code: "INVALID_SNOOZE", error: "snoozed_until must be a moment" }),
        contentType: "application/json",
        status: 400,
      })
    );

    await markFromKeyboard(page, 2);
    const bar = page.getByRole("group", { name: "Selected rows" });
    await bar.getByRole("combobox", { name: "Snooze selected asks" }).selectOption("tomorrow");
    await expect(bar).toContainText("Could not snooze 2 of 2");

    const rows = page.locator("[data-inbox-row]");
    await rows.nth(0).getByRole("checkbox").uncheck();
    await rows.nth(1).getByRole("checkbox").uncheck();
    await expect(bar).toContainText("0 selected");
    await page.evaluate(() => (document.activeElement as HTMLElement | null)?.blur());
    await page.mouse.move(0, 0);

    await page.keyboard.press("Escape");
    await expect(bar).toHaveCount(0);
  });

  // `h` with a selection live still starts from the list: a reader answering an ask is not in it.
  answeringTest(
    "h does not pull the reader out of an ask they are answering @pr",
    async ({ page, scene }) => {
      await openInbox(page, scene.rows);
      const bar = page.getByRole("group", { name: "Selected rows" });
      await markFromKeyboard(page, 1);
      await expect(bar).toContainText("1 selected");

      const hold = page.getByRole("radio", { name: "Hold" });
      await hold.click();
      await expect(hold).toBeFocused();
      await page.keyboard.press("h");
      await expect(hold).toBeFocused();
      await expect(bar.getByRole("combobox", { name: "Snooze selected asks" })).not.toBeFocused();
    }
  );

  // Reaching the picker with the pointer leaves no `h` behind it, so Escape must not send the
  // reader to whatever row an earlier `h` happened to be on.
  test("Escape from a pointer-opened bulk picker goes to the list, not a stale row @pr", async ({
    page,
    scene,
  }) => {
    await openInbox(page, scene.rows);
    const rows = page.locator("[data-inbox-row]");
    const bar = page.getByRole("group", { name: "Selected rows" });

    // An `h` from the first row records that row, then the reader leaves and marks row 3.
    await goToRow(page, 0);
    await page.keyboard.press("x");
    await page.keyboard.press("h");
    await page.keyboard.press("Escape");
    await expect(rows.nth(0)).toBeFocused();
    await page.keyboard.press("x");
    await rows.nth(2).getByRole("checkbox").check();
    await expect(bar).toContainText("1 selected");

    await bar.getByRole("combobox", { name: "Snooze selected asks" }).focus();
    await page.keyboard.press("Escape");
    await expect(rows.nth(2)).toBeFocused();
  });
});

// ---------------------------------------------------------------------------
// The reading pane's own keyboard (INFRA-963): `l` into the pane, the browser's scrolling keys
// there, and Escape back to the row `l` was pressed on. The pane is drawn from 1280 px, which
// only the `chromium` project is wide enough for, so none of the rows below takes the `@phone`
// tag: on a phone there is no pane to step into. Every fixture here is made up.
// ---------------------------------------------------------------------------

paneTest(
  "l focuses the pane and Escape returns from it where the pane draws the question as a card @pr",
  async ({ page, scene }) => {
    await openPane(page, scene);
    const rows = page.locator("[data-inbox-row]");
    await goToRow(page, 1);
    await expect(selectedRow(page)).toHaveAttribute("data-inbox-row", scene.cardAskId);

    // The scroller is the pane frame's, so a question in no document is drawn in the same box
    // the document was: one focus stop, whatever the pane is showing.
    const scroller = paneScroller(page);
    await expect(scroller.getByTestId(`ask-${scene.cardAskId}`)).toContainText(RETRY_QUESTION);
    await page.keyboard.press("l");
    await expect(scroller).toBeFocused();
    await expect(inboxPane(page).getByRole("region", { name: "Question body" })).toBeFocused();
    await page.keyboard.press("Escape");
    await expect(rows.nth(1)).toBeFocused();
  }
);

paneTest(
  "l moves no focus below 1280 px, where the Inbox draws no reading pane @pr",
  async ({ page, scene }) => {
    const errors: string[] = [];
    page.on("pageerror", (error) => errors.push(`${error.name}: ${error.message}`));

    // One pixel below the pane's width the Inbox is the list of cards it has always been: no
    // pane, no scroller, and nothing for `l` to step into.
    await page.setViewportSize({ height: 900, width: 1279 });
    await page.goto("/?assignee=everyone");
    const rows = page.locator("[data-inbox-row]");
    await expect(rows).toHaveCount(2);
    await expect(inboxPane(page)).toHaveCount(0);
    await expect(paneScroller(page)).toHaveCount(0);

    await rows.nth(0).focus();
    await page.keyboard.press("l");
    await expect(rows.nth(0)).toBeFocused();
    await expect(rows.nth(0)).toHaveAttribute("data-inbox-row", scene.approvalAskId);
    expect(errors).toEqual([]);
  }
);

emptyTest(
  "l moves no focus with an empty list, where no row is selected @pr",
  async ({ page, scene }) => {
    // The recording's seeding makes nothing, so the Inbox lists nothing: with no row selected
    // there is no pane at all, and `l` has nothing to focus.
    const errors: string[] = [];
    page.on("pageerror", (error) => errors.push(`${error.name}: ${error.message}`));

    await page.goto("/");
    await expect(page.getByText("Nothing needs you")).toBeVisible();
    await expect(page.locator("[data-inbox-row]")).toHaveCount(scene.rows);
    await expect(inboxPane(page)).toHaveCount(0);

    await page.locator("body").focus();
    await page.keyboard.press("l");
    expect(await page.evaluate(() => document.activeElement?.tagName ?? null)).toBe("BODY");
    expect(errors).toEqual([]);
  }
);

paneTest(
  "j from inside the reading pane leaves the focus on a row, not on the body @pr",
  async ({ page, scene }) => {
    await openPane(page, scene);
    const rows = page.locator("[data-inbox-row]");
    await goToRow(page, 0);
    await page.keyboard.press("l");
    await expect(paneScroller(page)).toBeFocused();

    // The step out of the pane comes back to the list first, so the focus lands on a row rather
    // than falling to the body when the pane swaps the document for a card.
    await page.keyboard.press("j");
    await expect(selectedRow(page)).toHaveAttribute("data-inbox-row", scene.cardAskId);
    await expect(rows.nth(0)).toBeFocused();
    expect(await page.evaluate(() => document.activeElement?.tagName ?? null)).not.toBe("BODY");

    // And Escape is still answered from there: it clears the row it is on, as it does for any
    // row the reader walked to.
    await page.keyboard.press("Escape");
    await expect(rows.nth(0)).not.toBeFocused();
    expect(await page.evaluate(() => document.activeElement?.tagName ?? null)).toBe("BODY");
  }
);

test("? lists l for the reading pane and f for the search box under Inbox alone @pr", async ({
  page,
  scene,
}) => {
  await openInbox(page, scene.rows);

  await page.keyboard.press("?");
  const dialog = page.getByRole("dialog", { name: "Keyboard shortcuts" });
  await expect(dialog).toBeVisible();
  const inboxScope = dialog.getByRole("region", { exact: true, name: "Inbox" });
  const globalScope = dialog.getByRole("region", { exact: true, name: "Global" });
  await expect(globalScope).toBeVisible();

  // Both bindings are the Inbox's own, so each is listed once, under Inbox and nowhere else.
  for (const [label, keys] of [
    ["Move the focus into the reading pane", ["l"]],
    ["Search asks", ["f"]],
  ] as const) {
    const inInbox = inboxScope.getByRole("listitem").filter({ hasText: label });
    await expect(inInbox).toHaveCount(1);
    await expect(inInbox.locator("kbd")).toHaveText(keys);
    await expect(globalScope.getByRole("listitem").filter({ hasText: label })).toHaveCount(0);
    await expect(dialog.getByRole("listitem").filter({ hasText: label })).toHaveCount(1);
  }
});
