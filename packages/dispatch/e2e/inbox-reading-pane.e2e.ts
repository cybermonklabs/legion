import {
  createAsk,
  createIssue,
  createProject,
  getArtifact,
  getAsk,
  getInbox,
  getIssue,
  patchIssue,
  resolveAsk,
} from "./api";
import { documentEditor } from "./editor";
import { inboxList, inboxPane, selectedRow, selectRow } from "./inbox-pane";
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
  OPEN_BEFORE_HEADING,
  OPEN_PROJECT,
  OPEN_RUNBOOK_HEADING,
  OPEN_SPEC_HEADING,
  OPEN_TALK_DECISION,
  OPEN_TALK_HEADING,
  paneOffsets,
  REMOVED_WORD,
  RETIRED_BLOCK,
  SCOPE_ANCHOR,
  seedInbox,
  seedOpenPage,
  session,
  WINDOW_ANCHOR,
} from "./inbox-reading-pane";
import {
  expectReadWhileHeld,
  holdIssuePatches,
  inboxRead,
  logReads,
  savesEnded,
  watchShown,
} from "./pick-writes";
import { navigateInApp } from "./sends";
import { expect, type Page, test } from "./test";
import { asUser } from "./users";

/**
 * The Inbox's reading column (INFRA-575), in the rows whose claim reaches the server: an answer
 * and an approval written and read back, the two withdrawals the server makes while the page
 * watches, the refusal that reads the Inbox back after it, and the row that reads a decision
 * block out of the live document editor. What the column draws over one fixed list - its two
 * shapes of card, its keys and its address, and the widths that take it away - is in
 * `inbox-reading-pane.stub.e2e.ts`; the seed both files read is in `inbox-reading-pane.ts`.
 */

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

    // Two taps answer it: the option, then Answer. The answer box stands in place and keeps
    // nothing the reader did not type - a reader who picks an option is done - and nothing else
    // is pressed.
    const card = block.locator("article[data-testid^=ask-]");
    const answerBox = card.locator("[data-ask-answer]");
    await expect(answerBox).toHaveValue("");
    await card.getByRole("radio", { exact: true, name: "Ledger first" }).click();
    await expect(answerBox).toHaveValue("");
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

// @pr: An ask answered and refused: the server's No keeps the question in the column with a Retry, rather than losing the answer.
test("an options-only answer the server refuses keeps its question in the column, with a Retry @pr", async ({
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

test("Enter puts the focus in the selected decision block's answer field", async ({ browser }) => {
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
    const answerBox = card.locator("[data-ask-answer]");
    await expect(answerBox).toBeVisible();
    await expect(answerBox).not.toBeFocused();

    // One Enter, the key the Inbox offers for answering the selected question: the reader is in
    // the answer field, which stands in place. A field without the focus leaves every later
    // keystroke to the Inbox's own keys - `j` walks the list while the reader believes they are
    // writing an answer.
    await page.keyboard.press("Enter");
    await expect(answerBox).toBeFocused();

    // What the reader types then reaches the answer, and the selection stays where it was.
    await page.keyboard.type("Ledger first, for the lease.");
    await expect(card.locator("[data-ask-answer]")).toHaveValue("Ledger first, for the lease.");
    await expect(selectedRow(page)).toHaveAttribute("data-inbox-row", seed.decisionAskId);
  } finally {
    await alice.close();
  }
});

// The row of dispatch://INFRA-299 on the Inbox: below the reading pane's width each row is a
// card carrying its own priority picker, and while a pick there is live - held by Playwright's
// route control at least `PICK_HOLD_MS` (`holdIssuePatches`) - the row shows it over every answer
// of the Inbox's read, which a rename of the issue starts (`api/event-refresh.ts`: an Inbox row
// shows its issue's title). The row records every value its picker shows, at every animation
// frame (`watchShown`), from the pick until the save ends.
// SLOW(>=2s): a save held three seconds while the Inbox reads the issue's rows again.
test("picker keeps a pick on screen: the priority on an Inbox row, over a read that answers while its save is out", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Priority under a held save" });
  await patchIssue(issue.key, { priority: 3 });
  await createAsk(
    issue.key,
    { options: [{ label: "Yes" }, { label: "No" }], question: "Ship it behind a flag?" },
    session
  );
  const renamed = "Priority under a held save, renamed while it is out";
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.setViewportSize({ height: 800, width: 1100 });
    const shown = await watchShown(page, `Priority of ${issue.key}`);
    const held = await holdIssuePatches(page, issue.key);
    const reads = logReads(page, inboxRead);
    await page.goto(INBOX);
    const picker = page.getByRole("combobox", { name: `Priority of ${issue.key}` });
    await expect(picker).toHaveValue("3");

    await picker.selectOption("1");
    await expect.poll(() => shown.values.at(-1)).toBe("1");
    const fromPick = shown.values.length - 1;
    await expect.poll(() => held.sent.length).toBe(1);
    // A rename of the issue, straight to the server: its live event has the Inbox read the
    // issue's rows again, and the answer, carrying P3, lands while the pick's save is held.
    const renamedAt = Date.now();
    await patchIssue(issue.key, { title: renamed });
    await expect(page.getByText(renamed).first()).toBeVisible();
    held.release();
    await savesEnded(page, held);

    expectReadWhileHeld(reads, held, renamedAt);

    expect(shown.values.slice(fromPick), "every value the row showed from the pick on").toEqual([
      "1",
    ]);
    expect(await getIssue(issue.key)).toMatchObject({ priority: 1 });
    await expect(picker).toHaveValue("1");
  } finally {
    await alice.close();
  }
});

// The head's Open page opens the document a question is about, at the section the column is
// opened at: a decision block's document at the heading above the block, or at the block where no
// heading stands above it; an approval request's document at its top; the quoted document at the
// quote's block; the issue for a question in no document; a project document's question opens that
// document at its section.
// The Reply thread of a decision block opens its document at the section once the column shows
// that document, and `o` follows it there.
// SLOW(>=2s): three documents and eight questions seeded, then four pages opened from the column
// and walked back.
test("Open page opens the document of the question, at its section, for each kind of question and for the Reply thread of a decision block", async ({
  browser,
}) => {
  // The eight `href` reads are soft, so on a tree without this change each one that fails there
  // waits out the whole expect timeout (15 s, `e2e/expect-timeout.ts`) and the row still reaches
  // item 8: six of them outlast a row's default limit.
  test.setTimeout(180_000);
  const seed = await seedOpenPage();

  // 1. What decides the row's scope, before the Inbox opens. Question 1 has the shape the server
  // sends for a decision block in an issue's second document, and its Inbox row names no project
  // document; items 1 to 7 each wait on alice, question 6 because it was handed to her. The seed
  // waits until item 8 is her Reply thread.
  const first = (await getAsk(seed.talkDecision)).ask;
  expect(
    {
      anchor: first.anchor,
      artifact_id: first.artifact_id,
      block_artifact: {
        primary: first.block_artifact?.primary,
        slug: first.block_artifact?.slug,
      },
      block_id: first.block_id,
      kind: first.kind,
    },
    "question 1 is a decision block in the issue's second document"
  ).toEqual({
    anchor: null,
    artifact_id: null,
    block_artifact: { primary: false, slug: "talk-md" },
    block_id: OPEN_TALK_DECISION,
    kind: "question",
  });
  const questions = [
    seed.talkDecision,
    seed.specDecision,
    seed.approval,
    seed.quoted,
    seed.plain,
    seed.runbookDecision,
    seed.beforeHeading,
  ];
  const inbox = await getInbox();
  expect(
    inbox.find((row) => row.id === seed.talkDecision),
    "question 1's Inbox row names no project document"
  ).not.toHaveProperty("document");
  expect(
    questions.map((id) => {
      const row = inbox.find((listed) => listed.id === id);
      return { id, waiting_on: row?.waiting_on, waiting_on_person: row?.waiting_on_person };
    }),
    "items 1 to 7 each wait on alice"
  ).toEqual(questions.map((id) => ({ id, waiting_on: "human", waiting_on_person: "alice" })));
  expect((await getAsk(seed.runbookDecision)).ask.asked_of, "question 6 is handed to alice").toBe(
    "alice"
  );

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.setViewportSize({ height: 900, width: 1440 });
    const pane = inboxPane(page);
    const openPage = pane.getByRole("link", { name: "Open page" });
    const showsTalk = () => expect(pane).toContainText(OPEN_TALK_HEADING);
    const issue = `/issues/${seed.issueKey}`;
    const talk = `${issue}/artifacts/${seed.talkSlug}`;
    const talkSection = `#b-${seed.headings.talk}`;
    const talkDecision = { fragment: talkSection, path: talk, row: seed.talkDecision };
    const specDecision = {
      fragment: `#b-${seed.headings.spec}`,
      path: `${issue}/spec`,
      row: seed.specDecision,
    };
    const approval = { fragment: "", path: talk, row: seed.approval };
    const thread = { fragment: talkSection, path: talk, row: seed.answeredThread };
    const items = [
      { ...talkDecision, shows: showsTalk },
      { ...specDecision, shows: () => expect(pane).toContainText(OPEN_SPEC_HEADING) },
      {
        ...approval,
        shows: () =>
          expect(
            pane.getByRole("region", { name: `Document version ${seed.approvalVersion}` })
          ).toContainText(OPEN_TALK_HEADING),
      },
      {
        fragment: `#b-${seed.quotedBlockId}`,
        path: talk,
        row: seed.quoted,
        shows: () => expect(pane.getByTestId(`ask-${seed.quoted}`)).toBeVisible(),
      },
      {
        fragment: "",
        path: issue,
        row: seed.plain,
        shows: () => expect(pane.getByTestId(`ask-${seed.plain}`)).toBeVisible(),
      },
      {
        fragment: `#b-${seed.headings.runbook}`,
        path: `/projects/${OPEN_PROJECT}/documents/${seed.runbookSlug}`,
        row: seed.runbookDecision,
        shows: () => expect(pane).toContainText(OPEN_RUNBOOK_HEADING),
      },
      // No heading stands above this block, so the page opens at the block itself.
      // `blockFragment` leaves letters, digits and `-` bare, so its id is its own fragment.
      {
        fragment: `#b-${OPEN_BEFORE_HEADING}`,
        path: talk,
        row: seed.beforeHeading,
        shows: showsTalk,
      },
      { ...thread, shows: showsTalk },
    ];

    // 2. Each item as the column draws it: the row selected, the column showing the question's
    // document or card, and Open page at that document's section. The read retries until the
    // editor has drawn the block and the address moves from the block to its heading.
    for (const [index, item] of items.entries()) {
      if (index === 0) {
        await page.goto(inboxAt(item.row));
      } else {
        await navigateInApp(page, inboxAt(item.row));
      }
      await expect(selectedRow(page)).toHaveAttribute("data-inbox-row", item.row);
      await item.shows();
      await expect
        .soft(openPage, `item ${index + 1}'s Open page`)
        .toHaveAttribute("href", `${item.path}${item.fragment}`);
    }

    // 3. Four of them opened from the column: the link lands on that address, and the page there
    // is the question's document.
    for (const [item, holds, lacks] of [
      [talkDecision, OPEN_TALK_HEADING, OPEN_SPEC_HEADING],
      [specDecision, OPEN_SPEC_HEADING, OPEN_TALK_HEADING],
      [approval, OPEN_TALK_HEADING, OPEN_SPEC_HEADING],
      [thread, OPEN_TALK_HEADING, OPEN_SPEC_HEADING],
    ] as const) {
      const address = `${item.path}${item.fragment}`;
      await navigateInApp(page, inboxAt(item.row));
      await expect(selectedRow(page)).toHaveAttribute("data-inbox-row", item.row);
      await expect(openPage).toHaveAttribute("href", address);
      await openPage.click();
      await expect(page).toHaveURL(address);
      await expect(documentEditor(page)).toContainText(holds);
      await expect(documentEditor(page)).not.toContainText(lacks);
      await page.goBack();
      await expect(selectedRow(page)).toHaveAttribute("data-inbox-row", item.row);
    }

    // 4. `o` follows a thread row's Open page, so on item 8 it opens the same address.
    await selectRow(page, thread.row);
    await expect(openPage).toHaveAttribute("href", `${thread.path}${thread.fragment}`);
    await page.locator("body").focus();
    await page.keyboard.press("o");
    await expect(page).toHaveURL(`${thread.path}${thread.fragment}`);
  } finally {
    await alice.close();
  }
});
