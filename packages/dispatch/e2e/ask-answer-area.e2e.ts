import { expect, type Locator, type Page, test } from "@playwright/test";

import { type FakeSession, getSentMessages, setLiveSessions } from "./agents";
import { answerAsk, createAsk, createComment, createIssue, createProject, resolveAsk } from "./api";
import { recordClipboard } from "./clipboard";
import { documentEditor, indexedBlockAsk } from "./editor";
import { askCard, inboxRow } from "./inbox-pane";
import { type KeyedWrite, watchWrites } from "./keyed-writes";
import { openRefPicker, pickReference } from "./ref-picker";
import { resetDatabase } from "./seed";
import { pasteFile } from "./sends";
import { sessionIds } from "./session-ids";
import { asUser } from "./users";

/**
 * INFRA-341: the ask card's answer box and the reply box under an answered ask share the
 * Conversation tab's field and reference picker. Ask back and Reply send a picked session's
 * reference in the comment body, and the server delivers the comment to that session. An answer
 * keeps the reference as a link in its text. A refused button says why in place
 * (`RefusableButton`), and one ask's header reads the same wherever the ask is drawn.
 */

/** The one live session the `@` picker offers. */
const planner = {
  capabilities: ["steer"],
  dir: "/workspaces/planner",
  last_seen: 1_700_000_000_000,
  machine_id: "e2e",
  roles: [],
  session_id: sessionIds.planner,
  title: "Planner",
} satisfies FakeSession;

const PLANNER_TARGET = `session:${sessionIds.planner}`;
const PLANNER_MENTION = `[@Planner](dispatch://agent/${planner.session_id})`;

/** The asker of every ask these rows seed. */
const asker = { actor: { id: sessionIds.planner, kind: "session" as const }, as: "agent" as const };

const EMPTY_ANSWER_REFUSAL = "Pick an option or write an answer.";
const EMPTY_DRAFT_REFUSAL = "Write something first.";

test.beforeEach(async () => {
  await resetDatabase();
  await setLiveSessions([planner]);
});

/** An issue with one open ask on it, asked by the live session. */
async function seedOpenAsk(question = "Which release path?") {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Answer area" });
  const ask = await createAsk(issue.key, { question }, asker);
  return { ask, issue };
}

/** Picks Planner's session at the end of `prefix` and checks the draft's reference. */
async function pickPlanner(field: Locator, prefix: string): Promise<void> {
  await pickReference(field, `${prefix}@`, /^Planner\b/);
  await expect(field).toHaveValue(`${prefix}${PLANNER_MENTION}`);
}

/** The comments the page posts on `issueKey` from here on, as the bodies it sent. */
function postedComments(page: Page, issueKey: string): KeyedWrite<unknown>[] {
  return watchWrites(page, "POST", `/api/v1/issues/${issueKey}/comments`);
}

/** One ask's whole turn in the Conversation: an answered ask's card is the record alone, and the
 *  thread under it - its replies and its Write a reply control - is the turn's, beside it. */
function conversationTurn(page: Page, askId: string): Locator {
  return page
    .getByRole("list", { name: "Conversation turns" })
    .locator(":scope > li")
    .filter({ has: page.getByTestId(`ask-${askId}`) });
}

/** Every `data-ask-*` field the headers inside `place` hold, with the place's own anchor quote
 *  and the state label as the reader sees it: the rendered text (`innerText`) of each label the
 *  browser draws, so a label the place leaves out, or hides, is missing here whatever its
 *  attributes say. A decision block draws two headers - the block's own urgency and state, and
 *  the hosted card's, which adds the provenance, the reference and the label - so the fields are
 *  collected across them. A field two headers of one place disagree about is reported as both
 *  values, which fails the row with the disagreement in it. The labels are not deduplicated: a
 *  place draws one, so two read `Open + Open` and fail a row that expects `Open`. */
async function headerFacts(place: Locator): Promise<Record<string, string | undefined>> {
  return place.evaluate((node) => {
    const names = ["urgency", "asker", "at", "copy", "state"];
    const held: Record<string, string | undefined> = {};
    const headers = [...node.querySelectorAll<HTMLElement>("[data-ask-header]")];
    const joined = (values: readonly string[]) => {
      const distinct = [...new Set(values)];
      return distinct.length === 0 ? undefined : distinct.join(" ≠ ");
    };
    for (const name of names) {
      held[name] = joined(
        headers
          .map((header) => header.dataset[`ask${name[0]?.toUpperCase()}${name.slice(1)}`])
          .filter((value) => value !== undefined)
      );
    }
    const labels = headers
      .flatMap((header) => [...header.querySelectorAll<HTMLElement>("[data-ask-state-label]")])
      .filter((label) => label.checkVisibility({ opacityProperty: true, visibilityProperty: true }))
      .map((label) => label.innerText.trim());
    held.label = labels.length === 0 ? undefined : labels.join(" + ");
    const quote = node.querySelector("blockquote");
    held.anchor = quote?.textContent ?? undefined;
    return held;
  });
}

test("one ask header says the same of an open, an answered and a resolved ask wherever it is drawn @phone", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "## Specification\n\nThe mirror replays the ledger on a cold start.\n",
    title: "Header everywhere",
  });
  const quote = "replays the ledger";
  const open = await createAsk(
    issue.key,
    { anchor: { artifact: "spec", quote }, question: "Does a cold start replay the ledger?" },
    asker
  );
  const answeredAsk = await createAsk(
    issue.key,
    { anchor: { artifact: "spec", quote }, question: "Which mirror keeps the budget?" },
    asker
  );
  await answerAsk(answeredAsk.id, {
    expected_edited_at: null,
    selected: [],
    text: "The left one.",
  });
  const resolved = await createAsk(
    issue.key,
    { anchor: { artifact: "spec", quote }, question: "Should the mirror retry?" },
    asker
  );
  await resolveAsk(resolved.id, { kind: "resolved", reason: "Decided elsewhere." });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    // The Inbox, which draws the open ask's card: in the reading column at desktop width, in the
    // row itself on a phone.
    await page.goto("/");
    const inboxFacts = await headerFacts(await askCard(page, open.id));
    expect(inboxFacts, "the Inbox card's header").toMatchObject({
      anchor: quote,
      asker: "Planner",
      at: open.created_at,
      copy: `dispatch://${issue.key}/ask/${open.id}`,
      label: "Open",
      state: "open",
      urgency: "med",
    });

    // The Inbox drawer, over whatever page the reader is on, draws the same card.
    await page.goto(`/issues/${issue.key}`);
    await page
      .getByRole("button", { name: /^(Open inbox|Peek at the inbox)/ })
      .first()
      .click();
    const drawer = page.getByRole("dialog", { name: "Inbox" });
    await expect(drawer.getByTestId(`ask-${open.id}`)).toBeVisible();
    expect(
      await headerFacts(drawer.getByTestId(`ask-${open.id}`)),
      "the Inbox drawer card's header"
    ).toMatchObject(inboxFacts);
    await page.keyboard.press("Escape");

    // The Conversation tab, where all three are drawn: the open card, the answered record and the
    // resolved record.
    await page.goto(`/issues/${issue.key}/conversation`);
    const conversation = page.locator("#issue-conversation-panel");
    expect(
      await headerFacts(conversation.getByTestId(`ask-${open.id}`)),
      "the Conversation tab's open card's header"
    ).toMatchObject(inboxFacts);
    const answeredFacts = await headerFacts(conversation.getByTestId(`ask-${answeredAsk.id}`));
    expect(answeredFacts, "the Conversation tab's answered record's header").toMatchObject({
      anchor: quote,
      asker: "Planner",
      at: answeredAsk.created_at,
      copy: `dispatch://${issue.key}/ask/${answeredAsk.id}`,
      label: "Answered",
      state: "answered",
      urgency: "med",
    });
    const resolvedFacts = await headerFacts(conversation.getByTestId(`ask-${resolved.id}`));
    expect(resolvedFacts, "the Conversation tab's resolved record's header").toMatchObject({
      anchor: quote,
      asker: "Planner",
      at: resolved.created_at,
      copy: `dispatch://${issue.key}/ask/${resolved.id}`,
      label: "Resolved",
      state: "resolved",
      urgency: "med",
    });
  } finally {
    await alice.close();
  }
});

test("a decision block draws the same header fields as the ask's other places, without its anchor @phone", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: 'Intro.\n\n:::ask{#decide urgency="high" multiple="false" state="open"}\nWhich way?\n\n- Left\n- Right\n:::\n\n:::ask{#settle urgency="med" multiple="false" state="open"}\nShip it?\n\n- Ship\n- Hold\n:::\n',
    title: "Block header",
  });
  const blockAsk = await indexedBlockAsk(issue.key, "decide");
  const settled = await indexedBlockAsk(issue.key, "settle");
  await resolveAsk(settled.id, { kind: "resolved", reason: "Settled elsewhere." }, { as: "agent" });
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    const inboxFacts = await headerFacts(await askCard(page, blockAsk.id));
    expect(inboxFacts, "the Inbox card's header").toMatchObject({
      asker: "alice",
      at: blockAsk.created_at,
      copy: `dispatch://${issue.key}/ask/${blockAsk.id}`,
      label: "Open",
      state: "open",
      urgency: "high",
    });

    // The block is its own anchor, so the block draws no quote; every other field is the same,
    // and the block's two headers hold each field once between them - its label too, for an
    // open ask and a resolved one alike. Each block is its own reported check (`expect.soft`),
    // so a run where both draw a second label says so of both.
    await page.goto(`/issues/${issue.key}`);
    const block = documentEditor(page).locator('[data-dispatch-ask-block="decide"]');
    await expect(block.locator("article[data-testid^=ask-]")).toBeVisible();
    expect.soft(await headerFacts(block), "the open decision block's headers").toEqual({
      ...inboxFacts,
      anchor: undefined,
    });
    const settledBlock = documentEditor(page).locator('[data-dispatch-ask-block="settle"]');
    await expect(settledBlock.locator("article[data-testid^=ask-]")).toBeVisible();
    expect.soft(await headerFacts(settledBlock), "the resolved decision block's headers").toEqual({
      anchor: undefined,
      asker: "alice",
      at: settled.created_at,
      copy: `dispatch://${issue.key}/ask/${settled.id}`,
      label: "Resolved",
      state: "resolved",
      urgency: "med",
    });
  } finally {
    await alice.close();
  }
});

test("a session picked in an Inbox ask's answer area goes out on Ask back, and an answer keeps it as a link @phone", async ({
  browser,
}) => {
  const { ask, issue } = await seedOpenAsk();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const comments = postedComments(page, issue.key);
    const answers = watchWrites(page, "POST", `/api/v1/asks/${ask.id}/answer`);
    await page.goto("/");
    const card = await askCard(page, ask.id);
    const field = card.getByLabel("Your answer");
    await pickPlanner(field, "Ask ");

    // Ask back posts it as a comment that names the session, and the session gets it.
    await card.getByRole("button", { exact: true, name: "Ask back" }).click();
    await expect
      .poll(() => comments.map((write) => write.input))
      .toEqual([{ ask_id: ask.id, body: `Ask ${PLANNER_MENTION}` }]);
    await expect
      .poll(() => getSentMessages())
      .toMatchObject([{ target_session: planner.session_id }]);
    await expect(field).toHaveValue("");

    // Answer sends a draft that names the session, with the reference in the answer's text.
    await pickPlanner(field, "Over to ");
    await card.getByRole("button", { exact: true, name: "Answer" }).click();
    await expect
      .poll(() => answers.map((write) => write.input))
      .toEqual([{ expected_edited_at: null, selected: [], text: `Over to ${PLANNER_MENTION}` }]);

    // The recorded answer reads the session as a link to it.
    await page.goto(`/issues/${issue.key}/conversation`);
    const answered = page.locator("#issue-conversation-panel").getByTestId(`ask-${ask.id}`);
    const link = answered.locator(`a[data-dispatch-ref="dispatch://agent/${planner.session_id}"]`);
    await expect(link).toHaveText("@Planner");
    await expect(link).toHaveAttribute("href", `/agents/${planner.session_id}/live`);
  } finally {
    await alice.close();
  }
});

test("a decision block's answer area picks a session the same way and asks back with it @phone", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: 'Intro.\n\n:::ask{#decide urgency="med" multiple="false" state="open"}\nWhich way?\n\n- Left\n- Right\n:::\n',
    title: "Block answer area",
  });
  const blockAsk = await indexedBlockAsk(issue.key, "decide");
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const comments = postedComments(page, issue.key);
    await page.goto(`/issues/${issue.key}`);
    const block = documentEditor(page).locator('[data-dispatch-ask-block="decide"]');
    const card = block.locator("article[data-testid^=ask-]");
    await expect(card).toBeVisible();
    await card.getByRole("button", { name: "Add a note or answer in your own words" }).click();
    const field = card.getByLabel("Your answer");
    await pickPlanner(field, "Ask ");

    await card.getByRole("button", { exact: true, name: "Ask back" }).click();
    await expect
      .poll(() => comments.map((write) => write.input))
      .toEqual([{ ask_id: blockAsk.id, body: `Ask ${PLANNER_MENTION}` }]);
    await expect
      .poll(() => getSentMessages())
      .toMatchObject([{ target_session: planner.session_id }]);
  } finally {
    await alice.close();
  }
});

test("the Conversation tab's copy of an ask, and the reply box under an answered ask, send a picked session @phone", async ({
  browser,
}) => {
  const { ask, issue } = await seedOpenAsk();
  const decided = await createAsk(issue.key, { question: "Which mirror?" }, asker);
  await answerAsk(decided.id, { expected_edited_at: null, selected: [], text: "The left one." });
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const comments = postedComments(page, issue.key);
    await page.goto(`/issues/${issue.key}/conversation`);
    const conversation = page.locator("#issue-conversation-panel");

    // The open ask's own card in the timeline: the compact variant, whose answer area is behind
    // its note disclosure.
    const openCard = conversation.getByTestId(`ask-${ask.id}`);
    await openCard.getByRole("button", { name: "Add a note or answer in your own words" }).click();
    await pickPlanner(openCard.getByLabel("Your answer"), "Ask ");
    await openCard.getByRole("button", { exact: true, name: "Ask back" }).click();
    await expect
      .poll(() => comments.map((write) => write.input))
      .toEqual([{ ask_id: ask.id, body: `Ask ${PLANNER_MENTION}` }]);

    // The reply box under the answered ask, behind its own Write a reply control. An answered
    // ask's card is its record, and the thread with that control is the turn's, beside it.
    const answeredTurn = conversationTurn(page, decided.id);
    await answeredTurn.getByRole("button", { name: "Write a reply" }).click();
    await pickPlanner(answeredTurn.getByLabel("Reply"), "Over to ");
    await answeredTurn.getByRole("button", { exact: true, name: "Reply" }).click();
    await expect.poll(() => comments.map((write) => write.input)).toHaveLength(2);
    expect(comments[1]?.input).toEqual({
      ask_id: decided.id,
      body: `Over to ${PLANNER_MENTION}`,
    });
    await expect
      .poll(() => getSentMessages())
      .toMatchObject([
        { target_session: planner.session_id },
        { target_session: planner.session_id },
      ]);
  } finally {
    await alice.close();
  }
});

test("an ask draft keeps its picked mention when its box closes and opens again @phone", async ({
  browser,
}) => {
  const { issue } = await seedOpenAsk();
  const decided = await createAsk(issue.key, { question: "Which mirror?" }, asker);
  await answerAsk(decided.id, { expected_edited_at: null, selected: [], text: "The left one." });
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const comments = postedComments(page, issue.key);
    await page.goto(`/issues/${issue.key}/conversation`);
    const turn = conversationTurn(page, decided.id);
    const open = turn.getByRole("button", { name: "Write a reply" });
    await open.click();
    await pickPlanner(turn.getByLabel("Reply"), "Over to ");

    // The box closes with its disclosure. The draft keeps the reference, so the reply still
    // reaches the session chosen before the box closed.
    await open.click();
    await expect(turn.getByLabel("Reply")).toHaveCount(0);
    await open.click();
    await expect(turn.getByLabel("Reply")).toHaveValue(`Over to ${PLANNER_MENTION}`);
    await expect(turn.getByRole("list", { name: "Mention targets" })).toContainText(PLANNER_TARGET);
    await turn.getByRole("button", { exact: true, name: "Reply" }).click();
    await expect
      .poll(() => comments.map((write) => write.input))
      .toEqual([{ ask_id: decided.id, body: `Over to ${PLANNER_MENTION}` }]);
    await expect
      .poll(() => getSentMessages())
      .toMatchObject([{ target_session: planner.session_id }]);
  } finally {
    await alice.close();
  }
});

test("Answer, Ask back and Reply refuse an empty ask box in place, sending nothing @phone", async ({
  browser,
}) => {
  const { ask, issue } = await seedOpenAsk();
  const decided = await createAsk(issue.key, { question: "Which mirror?" }, asker);
  await answerAsk(decided.id, { expected_edited_at: null, selected: [], text: "The left one." });
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const comments = postedComments(page, issue.key);
    const answers = watchWrites(page, "POST", `/api/v1/asks/${ask.id}/answer`);
    await page.goto("/");
    const card = await askCard(page, ask.id);
    const answer = card.getByRole("button", { exact: true, name: "Answer" });
    const askBack = card.getByRole("button", { exact: true, name: "Ask back" });
    for (const [button, reason] of [
      [answer, EMPTY_ANSWER_REFUSAL],
      [askBack, EMPTY_DRAFT_REFUSAL],
    ] as const) {
      // A refused button keeps its place in the tab order, so the reader can reach and hold it;
      // pressing it anyway does nothing. The press is read after it as well as the reach before
      // it: a forced press lands on whatever the page leaves at that point, and the Reply half
      // of this row found its own press reaching another control's refused Send, so each press
      // here says where it landed - a refused button that keeps the focus is the button pressed.
      await button.focus();
      await expect(button).toBeFocused();
      await expect(button).toBeDisabled();
      await expect(button).toHaveAttribute("title", reason);
      await expect(button).toHaveAccessibleDescription(reason);
      await expect(card.getByText(reason, { exact: true })).toBeVisible();
      await button.scrollIntoViewIfNeeded();
      await button.click({ force: true });
      await expect(button).toBeFocused();
    }
    // Nothing being sent cannot be awaited: half a second is the window either press would have
    // reached the server in, and every write this row makes lands well inside it.
    await page.waitForTimeout(500);
    expect(answers).toEqual([]);
    expect(comments).toEqual([]);

    // And the same card, given words, answers: `answers` is the list this page's answers land
    // in, so its empty read above is Answer's refusal and not a watch on an address the page
    // never posts to. The answer is waited for as well as its request, so the page below loads
    // with no write of this one's still out; it posts no comment, so `comments` reads on as it
    // did.
    const field = card.getByLabel("Your answer");
    await field.fill("Ship it.");
    const landed = page.waitForResponse(
      (response) =>
        response.request().method() === "POST" &&
        new URL(response.url()).pathname === `/api/v1/asks/${ask.id}/answer`
    );
    await field.press("ControlOrMeta+Enter");
    await expect
      .poll(() => answers.map((write) => write.input))
      .toEqual([{ expected_edited_at: null, selected: [], text: "Ship it." }]);
    await landed;

    await page.goto(`/issues/${issue.key}/conversation`);
    const turn = conversationTurn(page, decided.id);
    await turn.getByRole("button", { name: "Write a reply" }).click();
    const reply = turn.getByRole("button", { exact: true, name: "Reply" });
    await reply.focus();
    await expect(reply).toBeFocused();
    await expect(reply).toBeDisabled();
    await expect(reply).toHaveAttribute("title", EMPTY_DRAFT_REFUSAL);
    await expect(reply).toHaveAccessibleDescription(EMPTY_DRAFT_REFUSAL);
    await expect(turn.getByText(EMPTY_DRAFT_REFUSAL, { exact: true })).toBeVisible();
    // The press is made where the button is the thing under it. On a phone the conversation's
    // own composer is pinned to the bottom of the screen, and the turn's Reply button lies
    // under it where the page leaves it: a forced press at that point reaches the composer's
    // own refused `Send` - which is where the focus then went, and why nothing below could be
    // read of Reply at all. A refused button takes no unforced press to wait for either
    // (`aria-disabled` is Playwright's "not enabled"), so the scroll is what makes the press
    // the reader's.
    await reply.evaluate((button) => {
      button.scrollIntoView({ block: "center" });
    });
    await reply.click({ force: true });
    // And the press left the focus where it was: what c-a1 asks of a refused Reply is that the
    // reader keeps it with its reason, and a focus read only before the press says nothing
    // about what the press did with it.
    await expect(reply).toBeFocused();
    await turn.getByLabel("Reply").press("ControlOrMeta+Enter");
    // Nothing being sent cannot be awaited: half a second is the window a press that was going
    // to post this reply would have posted it in.
    await page.waitForTimeout(500);
    expect(comments).toEqual([]);

    // And the box, given words, posts its reply: `comments` is the list this page's comments
    // land in, so both of its empty reads above are refusals - Ask back's and Reply's - and not
    // a watch on an address the page never posts to.
    await turn.getByLabel("Reply").fill("The right one.");
    await turn.getByLabel("Reply").press("ControlOrMeta+Enter");
    await expect
      .poll(() => comments.map((write) => write.input))
      .toEqual([{ ask_id: decided.id, body: "The right one." }]);
  } finally {
    await alice.close();
  }
});

test("a file pasted into an ask's answer box and into its reply box adds its line, and Ctrl+Enter sends", async ({
  browser,
}) => {
  const { ask, issue } = await seedOpenAsk();
  const decided = await createAsk(issue.key, { question: "Which mirror?" }, asker);
  await answerAsk(decided.id, { expected_edited_at: null, selected: [], text: "The left one." });
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const comments = postedComments(page, issue.key);
    const answers = watchWrites(page, "POST", `/api/v1/asks/${ask.id}/answer`);
    await page.goto("/");
    const card = await askCard(page, ask.id);
    const field = card.getByLabel("Your answer");
    await field.fill("See");
    await pasteFile(field, "notes.md", "the notes");
    // A file that is no picture joins the draft as its plain reference (`uploadedFileText`).
    const line = `dispatch://${issue.key}/artifact/notes-md`;
    await expect(field).toHaveValue(`See ${line}`);
    await field.press("ControlOrMeta+Enter");
    await expect
      .poll(() => answers.map((write) => write.input))
      .toEqual([{ expected_edited_at: null, selected: [], text: `See ${line}` }]);

    await page.goto(`/issues/${issue.key}/conversation`);
    const turn = conversationTurn(page, decided.id);
    await turn.getByRole("button", { name: "Write a reply" }).click();
    const replyField = turn.getByLabel("Reply");
    await replyField.fill("And");
    await pasteFile(replyField, "notes.md", "the notes");
    const second = `dispatch://${issue.key}/artifact/notes-md`;
    await expect(replyField).toHaveValue(`And ${second}`);
    await replyField.press("ControlOrMeta+Enter");
    await expect
      .poll(() => comments.map((write) => write.input))
      .toEqual([{ ask_id: decided.id, body: `And ${second}` }]);
  } finally {
    await alice.close();
  }
});

test("the open picker owns the arrows and Enter inside an Inbox ask card @phone", async ({
  browser,
}) => {
  const { ask } = await seedOpenAsk();
  await setLiveSessions([
    planner,
    {
      ...planner,
      last_seen: planner.last_seen - 1,
      session_id: sessionIds.tester,
      title: "Planning assistant",
    },
  ]);
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    const card = await askCard(page, ask.id);
    const field = card.getByLabel("Your answer");
    const list = await openRefPicker(field, "@plan");
    const option = list.getByRole("option", { name: /^Planner\b/ });
    const next = list.getByRole("option", { name: /^Planning assistant\b/ });
    await expect(next).toBeVisible();
    // The shared picker highlights its first row on open. Both arrows move its highlight
    // without moving the Inbox's selection or the field's focus.
    await expect(option).toHaveAttribute("aria-selected", "true");
    await field.press("ArrowDown");
    await expect(next).toHaveAttribute("aria-selected", "true");
    await expect(field).toBeFocused();
    await field.press("ArrowUp");
    await expect(option).toHaveAttribute("aria-selected", "true");
    await expect(field).toBeFocused();
    await field.press("Enter");
    await expect(field).toHaveValue(PLANNER_MENTION);
    await expect(list).toHaveCount(0);
    await expect(field).toBeFocused();
    await expect(inboxRow(page, ask.id)).toBeVisible();
  } finally {
    await alice.close();
  }
});

test("an ask reply in the Inbox copies its own dispatch:// reference @phone", async ({
  browser,
}) => {
  const { ask, issue } = await seedOpenAsk();
  const posted = await createComment(
    issue.key,
    { ask_id: ask.id, body: "Which environment?" },
    asker
  );
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const copied = await recordClipboard(page);
    await page.goto("/");
    const card = await askCard(page, ask.id);
    const reply = card.getByTestId(`thread-${ask.id}`).locator("li[data-reply]").first();
    await expect(reply).toContainText("Which environment?");
    await reply.getByRole("button", { name: /^Copy reference/ }).click();
    expect(await copied()).toEqual([`dispatch://${issue.key}/comment/${posted.id}`]);
  } finally {
    await alice.close();
  }
});
