import { DISPATCH_EVENT_BODY_MAX } from "@legion/contracts/dispatch-tools";
import { expect, type Locator, type Page, test } from "@playwright/test";

import {
  answerAsk,
  createAsk,
  createComment,
  createIssue,
  createProject,
  getAsk,
  requestApproval,
  resolveAsk,
  withOpenedAsk,
} from "./api";
import { paragraphsOf } from "./bodies";
import { documentEditor, indexedBlockAsk } from "./editor";
import { askCard, inboxPane, inboxRow, READING_PANE_WIDTH, selectRow } from "./inbox-pane";
import { resetDatabase } from "./seed";
import { signIn } from "./users";

/**
 * A person declines to answer a question, and the agent that asked is told so
 * (dispatch://INFRA-1282).
 *
 * Every row writes the decline through the person's own signed-in browser, on the control the
 * card draws, rather than seeding a resolution: what the suite is asked to hold is that a reader
 * can reach the act and that every surface then reads the closed question the same way. The
 * control belongs to the one card every surface draws (`features/inbox/AskCard.tsx`), so the
 * rows read it where a reader meets it: the Inbox list, the Inbox's reading column, a document's
 * decision block, the issue page, and the margin that folds the closed question away.
 */

const session = {
  actor: {
    kind: "session" as const,
    id: "e2e-session",
    origin: { session_title: "e2e-session-title", tmux: "dispatch:1.2" },
  },
  as: "agent" as const,
};

/** Both choices of every seeded question, so an answer is one the server takes as it stands. */
const shipOrHold = [{ label: "Ship" }, { label: "Hold" }];

test.beforeEach(async ({ context }) => {
  await resetDatabase();
  await signIn(context, "alice");
});

/** The person's way out of a question, on the card `card` draws: **Decline to answer**. While the
 *  dialog is open the card holds the dialog's own button of that name too, so this is read before
 *  it opens and counted where the claim is that the act is drawn or is not. */
function declineControl(card: Locator): Locator {
  return card.getByRole("button", { exact: true, name: "Decline to answer" });
}

/** The dialog the control opens, which is the page's: it is drawn over everything else. */
function declineDialog(page: Page): Locator {
  return page.getByRole("dialog", { name: "Decline to answer" });
}

/** Declines the question `card` draws, as the signed-in person: the control, the note where there
 *  is one, and the dialog's own **Decline to answer**. An empty note is no note - the field is
 *  left as the dialog opened it - which is what the write carries. */
async function decline(page: Page, card: Locator, note: string): Promise<void> {
  await declineControl(card).click();
  const dialog = declineDialog(page);
  await expect(dialog).toBeVisible();
  if (note !== "") {
    await dialog.getByLabel("Note (optional)").fill(note);
  }
  await dialog.getByRole("button", { exact: true, name: "Decline to answer" }).click();
  // The dialog goes with the write the server took: the card swaps to its record, and in the
  // Inbox the row it stood in has already left the list.
  await expect(dialog).toHaveCount(0);
}

/** The question's card on the issue page, where the Conversation lists every question of the
 *  issue, open or closed. The address is the one the Conversation tab answered and still
 *  resolves (dispatch://INFRA-1196 R19), since the redesigned page draws no such tab; it is
 *  how every other spec of this suite reaches the conversation (`conversation.e2e.ts:209`). */
async function conversationCard(page: Page, issueKey: string, askId: string): Promise<Locator> {
  await page.goto(`/issues/${issueKey}/conversation`);
  return page.getByRole("region", { name: "Conversation" }).getByTestId(`ask-${askId}`);
}

// SLOW(>=2s): two page loads - the Inbox and the issue page - with a decline written through
// the dialog between them.
test("a question declined from the Inbox with a note leaves the list and reads declined on the issue page", async ({
  page,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Rollout order" });
  const ask = await createAsk(
    issue.key,
    { options: shipOrHold, question: "Which service moves first?" },
    session
  );
  const note = "The release owner decides this one.";

  await page.goto("/");
  const card = await askCard(page, ask.id);
  await expect(card).toContainText("Which service moves first?");
  await decline(page, card, note);

  // The row goes the moment the person presses, as an answered row does, and the Inbox is empty.
  await expect(inboxRow(page, ask.id)).toHaveCount(0);
  await expect(page.getByText("Nothing needs you")).toBeVisible();
  await expect
    .poll(async () => (await getAsk(ask.id)).ask.resolution, {
      intervals: [50, 50, 100, 200, 500],
    })
    .toMatchObject({ kind: "declined", reason: note });

  // The issue page reads the closed question as the act the person took: declined, by them, with
  // what they wrote. It is none of the other three ways a question closes.
  const record = await conversationCard(page, issue.key, ask.id);
  await expect(record.getByTestId("ask-resolution-badge")).toHaveText("Declined");
  await expect(record).toContainText("alice declined to answer");
  await expect(record).toContainText(note);
  await expect(record).not.toContainText("Withdrawn");
  await expect(record).not.toContainText("Retracted by");
  await expect(record).not.toContainText("Resolved by");
  await expect(declineControl(record)).toHaveCount(0);
});

// SLOW(>=2s): the reading column drawn at its own width, a decline written in it, and the issue
// page loaded for the record it leaves.
test("the Inbox's reading pane declines a question with no note at all", async ({ page }) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Retry budget" });
  const ask = await createAsk(
    issue.key,
    { options: shipOrHold, question: "Should the mirror keep retrying?" },
    session
  );

  // The reading column draws from 1280 px, which is the width this row reads the card at.
  await page.setViewportSize({ height: 900, width: READING_PANE_WIDTH });
  await page.goto("/");
  await selectRow(page, ask.id);
  const pane = inboxPane(page);
  await expect(pane).toBeVisible();
  const card = pane.getByTestId(`ask-${ask.id}`);
  await expect(card).toContainText("Should the mirror keep retrying?");
  await decline(page, card, "");

  await expect(inboxRow(page, ask.id)).toHaveCount(0);
  await expect
    .poll(async () => (await getAsk(ask.id)).ask.resolution, {
      intervals: [50, 50, 100, 200, 500],
    })
    .toMatchObject({ kind: "declined", reason: "" });

  // A decline with no note holds no reason, so the record's line ends at the act itself.
  const record = await conversationCard(page, issue.key, ask.id);
  await expect(record.getByTestId("ask-resolution-badge")).toHaveText("Declined");
  await expect(record).toContainText("alice declined to answer");
  await expect(record).not.toContainText("declined to answer -");
});

test("an approval request beside a question in one Inbox draws no Decline to answer", async ({
  page,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const questionIssue = await createIssue({ project: "CORE", title: "Rollout order" });
  const question = await createAsk(
    questionIssue.key,
    { options: shipOrHold, question: "Which service moves first?" },
    session
  );
  const approvalIssue = await createIssue({
    project: "CORE",
    spec: "# Ledger rollout\n\nThe ledger moves first, and the mirror follows it.\n",
    title: "Ledger rollout",
  });
  const approval = withOpenedAsk(
    await requestApproval(
      approvalIssue.primary_artifact_id,
      { summary: "The ledger-first rollout." },
      session
    )
  );

  // Below the reading column's width every row draws its own card, so the question and the
  // approval request stand as two cards in one list, which is what this row compares.
  await page.setViewportSize({ height: 900, width: READING_PANE_WIDTH - 1 });
  await page.goto("/");
  await expect(page.locator("[data-inbox-row]")).toHaveCount(2);

  const questionCard = await askCard(page, question.id);
  await expect(declineControl(questionCard)).toHaveCount(1);

  // The approval request is answered with Approve or Request changes, and the server refuses a
  // decline of one, so its card offers the person no such way out.
  const approvalCard = await askCard(page, approval.ask.id);
  await expect(approvalCard).toContainText("Approval requested");
  const options = approvalCard.getByRole("group", { name: "Answer options" });
  await expect(options.getByRole("radio", { name: /^Approve/ })).toHaveCount(1);
  await expect(options.getByRole("radio", { name: /^Request changes/ })).toHaveCount(1);
  await expect(declineControl(approvalCard)).toHaveCount(0);
});

// SLOW(>=2s): the spec's live document room is drawn before its block hosts a card, and the
// issue page's Conversation is read after it.
test("a document's decision block and the issue page both draw Decline to answer", async ({
  page,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const block = "rollout-order";
  const issue = await createIssue({
    project: "CORE",
    spec: [
      "# Rollout order",
      "",
      "The ledger and the mirror each keep a clock of their own.",
      "",
      `:::ask{#${block} urgency="med" multiple="false" state="open"}`,
      "Which service moves first?",
      "",
      "- Ledger first",
      "- Mirror first",
      ":::",
      "",
    ].join("\n"),
    title: "Rollout order",
  });
  const blockAsk = await indexedBlockAsk(issue.key, block);

  // The control belongs to the one card every surface draws, so a question written into a
  // document is declinable where the document draws it as well as where the issue lists it.
  await page.goto(`/issues/${issue.key}`);
  const hosted = documentEditor(page)
    .locator(`[data-dispatch-ask-block="${block}"]`)
    .locator("article[data-testid^=ask-]");
  await expect(hosted).toBeVisible();
  await expect(declineControl(hosted)).toHaveCount(1);

  const listed = await conversationCard(page, issue.key, blockAsk.id);
  await expect(declineControl(listed)).toHaveCount(1);
});

// SLOW(>=2s): a seeded reply waited for until the server has moved the turn, then the Inbox and
// the issue page.
test("a question waiting on its agent is declined from the Inbox", async ({ page }) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Cold start" });
  const ask = await createAsk(
    issue.key,
    { options: shipOrHold, question: "Does a cold start replay the whole ledger?" },
    session
  );
  // The person asked back instead of answering, which hands the turn to the agent; the question
  // is still open, and still theirs to close.
  await createComment(issue.key, { ask_id: ask.id, body: "Which ledger do you mean?" });
  await expect
    .poll(() => getAsk(ask.id), { intervals: [50, 50, 100, 200, 500] })
    .toMatchObject({ ask: { waiting_on: "agent" } });

  await page.goto("/");
  const card = await askCard(page, ask.id);
  await expect(card.getByText("Waiting on e2e-session-title")).toBeVisible();
  await decline(page, card, "The agent decides this one without me.");

  await expect(inboxRow(page, ask.id)).toHaveCount(0);
  const record = await conversationCard(page, issue.key, ask.id);
  await expect(record.getByTestId("ask-resolution-badge")).toHaveText("Declined");
});

// SLOW(>=2s): three anchored asks settled into the document's margin, with a decline written in
// the Inbox between the two page loads.
test("the margin folds a declined ask away with a retracted one and keeps the answered decision", async ({
  page,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: [
      "# Mirror runbook",
      "",
      "The mirror replays the ledger on a cold start.",
      "",
      "The mirror keeps a retry budget of its own.",
      "",
      "The mirror hands the lease back when it stops.",
      "",
    ].join("\n"),
    title: "Mirror runbook",
  });
  const anchored = (quote: string, question: string) =>
    createAsk(
      issue.key,
      { anchor: { artifact: "spec", quote }, options: shipOrHold, question },
      session
    );
  const answered = await anchored("replays the ledger", "Does a cold start replay it all?");
  await answerAsk(answered.id, { expected_edited_at: null, selected: ["Ship"] });
  const retracted = await anchored("retry budget", "Should the budget be three attempts?");
  await resolveAsk(
    retracted.id,
    { kind: "retracted", reason: "A newer question supersedes this one." },
    session
  );
  const declined = await anchored("hands the lease back", "Who takes the lease back?");

  await page.goto("/");
  await decline(page, await askCard(page, declined.id), "The lease owner decides this one.");

  await page.goto(`/issues/${issue.key}`);
  await page.getByRole("tab", { name: "Spec" }).click();
  const margin = page.getByTestId("margin-sheet");

  // The answered question is a decision, and stays where a reader reads the decisions. The
  // declined one is no decision - nobody made it - so the margin folds it away with the
  // retracted one, behind the toggle that counts both.
  await expect(margin.getByTestId(`ask-${answered.id}`)).toBeVisible();
  await expect(margin.getByTestId(`ask-${declined.id}`)).toHaveCount(0);
  await expect(margin.getByTestId(`ask-${retracted.id}`)).toHaveCount(0);

  await margin.getByRole("button", { exact: true, name: "Resolved (2)" }).click();
  await expect(margin.getByTestId(`ask-${declined.id}`)).toContainText("alice declined to answer");
  await expect(margin.getByTestId(`ask-${retracted.id}`)).toContainText("Retracted by");
  await expect(margin.getByTestId(`ask-${answered.id}`)).toBeVisible();
});

// SLOW(>=2s): three asks on one issue, a decline written in the Inbox, and the issue page's
// Conversation read after it.
test("an answered ask and a declined ask draw no Decline to answer, which an open one draws", async ({
  page,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Lease handover" });
  const ask = (question: string) =>
    createAsk(issue.key, { options: shipOrHold, question }, session);
  const answered = await ask("Does the lease move with the ledger?");
  await answerAsk(answered.id, { expected_edited_at: null, selected: ["Ship"] });
  const declined = await ask("Does the lease move with the mirror?");
  const open = await ask("Does the lease move at all?");

  await page.goto("/");
  await decline(page, await askCard(page, declined.id), "The lease owner decides this one.");

  // A closed question is a record, whichever way it closed: the act is drawn on an open question
  // and nowhere else.
  await page.goto(`/issues/${issue.key}/conversation`);
  const conversation = page.getByRole("region", { name: "Conversation" });
  await expect(conversation.getByTestId(`ask-${answered.id}`)).toBeVisible();
  await expect(declineControl(conversation.getByTestId(`ask-${answered.id}`))).toHaveCount(0);
  await expect(
    conversation.getByTestId(`ask-${declined.id}`).getByTestId("ask-resolution-badge")
  ).toHaveText("Declined");
  await expect(declineControl(conversation.getByTestId(`ask-${declined.id}`))).toHaveCount(0);
  await expect(declineControl(conversation.getByTestId(`ask-${open.id}`))).toHaveCount(1);
});

// SLOW(>=2s): a note of 32,000 characters filled into the dialog, written, and read back from
// the server.
test("the decline dialog counts what is left of the note's bound and takes a note of exactly it", async ({
  page,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Replay window" });
  const ask = await createAsk(
    issue.key,
    { options: shipOrHold, question: "Which window does the mirror replay?" },
    session
  );

  await page.goto("/");
  const card = await askCard(page, ask.id);
  await declineControl(card).click();
  const dialog = declineDialog(page);
  const left = dialog.getByText(/^\d+ characters? left$/);

  // The count opens at the whole of the bound the route holds a resolution's reason to
  // (`DISPATCH_EVENT_BODY_MAX`, which the server enforces in UTF-16 units), and falls by what
  // the person has written.
  await expect(left).toHaveText(`${DISPATCH_EVENT_BODY_MAX} characters left`);
  const note = dialog.getByLabel("Note (optional)");
  const opening = "Ask the release owner.";
  await note.fill(opening);
  await expect(left).toHaveText(`${DISPATCH_EVENT_BODY_MAX - opening.length} characters left`);

  // At the bound itself the count is zero. The note is paragraphs, so no block of it is longer
  // than Dispatch holds one block to, and it ends in a letter, so the server's trim leaves the
  // length as written.
  const whole = `${paragraphsOf(DISPATCH_EVENT_BODY_MAX - 1)}x`;
  expect(whole.length).toBe(DISPATCH_EVENT_BODY_MAX);
  await note.fill(whole);
  await expect.poll(async () => (await note.inputValue()).length).toBe(DISPATCH_EVENT_BODY_MAX);
  await expect(left).toHaveText("0 characters left");

  // A note of exactly that size is one the route takes: no refusal is drawn, the dialog closes,
  // and the server kept every unit of it.
  await dialog.getByRole("button", { exact: true, name: "Decline to answer" }).click();
  await expect(dialog).toHaveCount(0);
  await expect(inboxRow(page, ask.id)).toHaveCount(0);
  await expect
    .poll(async () => (await getAsk(ask.id)).ask.resolution?.reason.length, {
      intervals: [50, 50, 100, 200, 500],
    })
    .toBe(DISPATCH_EVENT_BODY_MAX);
});
