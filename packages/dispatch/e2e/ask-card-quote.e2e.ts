import type { AskOption } from "../web/src/api/types";
import { setLiveSessions } from "./agents";
import {
  answerAsk,
  createArtifactComment,
  createAsk,
  createComment,
  createIssue,
  createMessage,
  createProjectDocument,
  getArtifactText,
  getAsk,
  rejectSuggestion,
  requestApproval,
  withOpenedAsk,
} from "./api";
import { asTester, defineRule, PROJECT, seedProject, seedSurface, selectWords } from "./ask-card";
import { setModelRules } from "./fake-model-helpers";
import { inboxPane, inboxRow, selectRow } from "./inbox-pane";
import {
  asSession,
  replyRowId,
  seedAgentThread,
  seedAskThread,
  seedMessageThread,
  THREAD_SESSION,
} from "./inbox-threads";
import { expect, type Locator, type Page, test } from "./test";

/**
 * Quote reply (INFRA-3231's R7, check 9529ef59). Selecting words of an ask, a comment, a reply or
 * a message whose thread takes a reply shows a bar over them with Quote reply (`q`), and over an
 * open ask's question or options, or an answered ask's while its answer is changed, Define (`d`)
 * too; on a phone the bar stands at the foot of the screen. Quote reply puts the selection into
 * the box that answers that text as a Markdown quote: each block the page draws its own quoted
 * block with a bare `>` line between two, bold, code and list items kept, every other character
 * escaped so it posts as it reads, then a blank line, with the cursor after it and the box in
 * focus. Text already in the box stays above the quote. Every row compares the box's value with
 * the exact string expected.
 *
 * A decided suggestion's thread, a document's own text and a decision block offer no Quote reply,
 * and `q` quotes nothing there. The shortcut help lists `q`.
 *
 * At the commit before the change this file fails: a selection shows no bar and `q` does nothing.
 */

const WIDE = { height: 900, width: 1440 };

/** An ask whose question holds a paragraph with a bold word and a code word, and whose options
 *  have no description. */
const ORDER_QUESTION =
  "Which service moves first?\n\nThe **ledger** moves on Monday, and `renew` runs every 20 seconds.";
const ORDER_OPTIONS: readonly AskOption[] = [{ label: "Ledger first" }, { label: "Mirror first" }];
/** That paragraph and the first option, each a quoted block of its own. */
const ORDER_QUOTE =
  "> The **ledger** moves on Monday, and `renew` runs every 20 seconds.\n>\n> Ledger first\n\n";

/** An ask whose first option has a description that draws `__tests__`, which its Markdown
 *  writes escaped. */
const DESCRIBED_OPTIONS: readonly AskOption[] = [
  { description: "Run \\_\\_tests\\_\\_ before the move.", label: "Run it first" },
  { description: "Audit after the move.", label: "Move first" },
];
const DESCRIBED_QUOTE = "> Run it first\n>\n> Run \\_\\_tests\\_\\_ before the move.\n\n";

/** A question that ends in a question mark, and a line typed before quoting it that asks
 *  nothing. */
const FLIP_QUESTION = "R3 flip audit: go with B per the train?";
const TYPED = "Flip. One thing I can't read here:";

/** An agent's reply that lists two items, each opening with code. */
const LIST_REPLY =
  "Done. `--dry-run` now prints:\n\n- `skip` and the lease's id, for a lease another holder has\n- `stale` and the id, for a lease past its renewal";
const LIST_QUOTE =
  "> - `skip` and the lease's id, for a lease another holder has\n>\n> - `stale` and the id, for a lease past its renewal\n\n";

/** One question of the tester's on an issue of alice's of its own. */
async function seedAsk(
  question: string,
  options: readonly AskOption[],
  title: string
): Promise<{ askId: string; issueKey: string }> {
  const issue = await createIssue({
    assignee: "alice",
    force: true,
    project: PROJECT,
    spec: `# ${title}\n`,
    title,
  });
  const ask = await createAsk(
    issue.key,
    { options: [...options], question, urgency: "high" },
    asTester
  );
  return { askId: ask.id, issueKey: issue.key };
}

/** The bar over selected words. */
function bar(page: Page): Locator {
  return page.locator("[data-selection-bar]");
}

/** The bar's actions, each as it reads: its name and then its key. */
async function barActions(page: Page): Promise<string[]> {
  await expect(bar(page)).toBeVisible();
  return bar(page)
    .locator("[data-selection-action]")
    .evaluateAll((actions) => actions.map((action) => action.textContent ?? ""));
}

/** The bar's Quote reply button. */
function quoteButton(page: Page): Locator {
  return bar(page).locator('[data-selection-action="Quote reply"]');
}

/**
 * Selects from the start of `start`, the first text under `from` that holds it, to the end of
 * `end`, the first text under `to` that holds it after that, as a reader's drag would. Nothing
 * here moves the focus.
 */
async function selectAcross(from: Locator, start: string, to: Locator, end: string): Promise<void> {
  const last = await to.elementHandle();
  try {
    await from.evaluate(
      (first, words) => {
        const find = (root: Node, wanted: string, after?: { node: Node; at: number }) => {
          const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
          for (let node = walker.nextNode(); node !== null; node = walker.nextNode()) {
            if (
              after !== undefined &&
              node !== after.node &&
              (after.node.compareDocumentPosition(node) & Node.DOCUMENT_POSITION_FOLLOWING) === 0
            ) {
              continue;
            }
            const at = (node.textContent ?? "").indexOf(
              wanted,
              node === after?.node ? after.at : 0
            );
            if (at >= 0) return { at, node };
          }
          throw new Error(`no text holds ${JSON.stringify(wanted)}`);
        };
        const head = find(first, words.start);
        const tail = find(words.last ?? first, words.end, head);
        const range = document.createRange();
        range.setStart(head.node, head.at);
        range.setEnd(tail.node, tail.at + words.end.length);
        const selection = window.getSelection();
        selection?.removeAllRanges();
        selection?.addRange(range);
      },
      { end, last, start }
    );
  } finally {
    await last.dispose();
  }
}

/**
 * Selects `words`, the first text under `scope` that holds them, with the mouse, as a reader's
 * drag does: the press and the drag put the focus where a click on that text puts it.
 */
async function dragOver(page: Page, scope: Locator, words: string): Promise<void> {
  const ends = await scope.evaluate((root, wanted) => {
    const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
    for (let node = walker.nextNode(); node !== null; node = walker.nextNode()) {
      const at = node.textContent?.indexOf(wanted) ?? -1;
      if (at < 0) continue;
      const range = document.createRange();
      range.setStart(node, at);
      range.setEnd(node, at + wanted.length);
      const box = range.getBoundingClientRect();
      return { from: box.left + 1, to: box.right - 1, y: box.top + box.height / 2 };
    }
    throw new Error(`no text holds ${JSON.stringify(wanted)}`);
  }, words);
  await page.mouse.move(ends.from, ends.y);
  await page.mouse.down();
  await page.mouse.move(ends.to, ends.y, { steps: 8 });
  await page.mouse.up();
}

/** That `box` holds `value` exactly, has the focus, and has the cursor at its end. */
async function expectQuoted(box: Locator, value: string): Promise<void> {
  await expect(box).toHaveValue(value);
  await expect(box).toBeFocused();
  await expect
    .poll(() =>
      box.evaluate((field) => {
        const area = field as HTMLTextAreaElement;
        return area.selectionStart === area.value.length && area.selectionEnd === area.value.length;
      })
    )
    .toBe(true);
}

/** Takes the focus off whatever holds it, so no field takes the keys pressed next. */
async function releaseFocus(page: Page): Promise<void> {
  await page.evaluate(() => {
    const active = document.activeElement;
    if (active instanceof HTMLElement) active.blur();
  });
}

/** That the selection standing now draws no bar, and that `q` then quotes into no box. */
async function expectNoQuoteReply(page: Page): Promise<void> {
  // The bar reads a selection one task after it changes; half a second is far past that.
  await page.waitForTimeout(500);
  await expect(bar(page)).toHaveCount(0);
  await page.keyboard.press("q");
  // A quote lands in its box within the press's own render; half a second is far past that.
  await page.waitForTimeout(500);
  const values = await page
    .locator("textarea")
    .evaluateAll((boxes) => boxes.map((box) => (box as HTMLTextAreaElement).value));
  expect(values.filter((value) => value.includes(">"))).toEqual([]);
}

// SLOW(>=2s): a seed, the Inbox, and one selection quoted twice, by the bar's button and by q.
test("in the Inbox the bar over an ask's question and first option offers Quote reply and Define, and both quote each shown block in Markdown", async ({
  page,
}) => {
  await seedProject();
  const { askId } = await seedAsk(ORDER_QUESTION, ORDER_OPTIONS, "Move order");
  await page.goto(`/?item=${askId}`);
  const card = inboxPane(page).locator(`[data-ask-card="${askId}"]`);
  await expect(card).toBeVisible();
  const box = card.locator("[data-ask-answer]");
  const select = () =>
    selectAcross(
      card.locator("[data-ask-question]"),
      "The ",
      card.locator("fieldset"),
      "Ledger first"
    );

  await select();
  expect(await barActions(page)).toEqual(["Quote replyq", "Defined"]);
  await quoteButton(page).click();
  await expectQuoted(box, ORDER_QUOTE);
  // The box holds the focus, so the words are no longer the reader's to quote.
  await expect(bar(page)).toHaveCount(0);

  // Again with `q`, from an empty box and no field in focus.
  await box.fill("");
  await releaseFocus(page);
  await select();
  await expect(bar(page)).toBeVisible();
  await page.keyboard.press("q");
  await expectQuoted(box, ORDER_QUOTE);
});

// SLOW(>=2s): two asks quoted in the Inbox, one answered, then its record read on its issue.
test("two options quote as two blocks, a described option's label and description apart, and a quoted __tests__ is sent and drawn as it reads", async ({
  page,
}) => {
  await seedProject();
  const order = await seedAsk(ORDER_QUESTION, ORDER_OPTIONS, "Move order");
  const audit = await seedAsk(
    "Should the audit run before the ledger moves?",
    DESCRIBED_OPTIONS,
    "Audit order"
  );

  await page.goto(`/?item=${order.askId}`);
  const orderCard = inboxPane(page).locator(`[data-ask-card="${order.askId}"]`);
  await expect(orderCard).toBeVisible();
  const orderOptions = orderCard.locator("fieldset");
  await selectAcross(orderOptions, "Ledger first", orderOptions, "Mirror first");
  await expect(bar(page)).toBeVisible();
  await page.keyboard.press("q");
  await expectQuoted(
    orderCard.locator("[data-ask-answer]"),
    "> Ledger first\n>\n> Mirror first\n\n"
  );

  await page.goto(`/?item=${audit.askId}`);
  const auditCard = inboxPane(page).locator(`[data-ask-card="${audit.askId}"]`);
  await expect(auditCard).toBeVisible();
  const auditOptions = auditCard.locator("fieldset");
  await expect(auditOptions).toContainText("Run __tests__ before the move.");
  await selectAcross(auditOptions, "Run it first", auditOptions, "before the move.");
  await expect(bar(page)).toBeVisible();
  await page.keyboard.press("q");
  const box = auditCard.locator("[data-ask-answer]");
  await expectQuoted(box, DESCRIBED_QUOTE);

  // The answer goes with the quote as the box holds it, escapes and all.
  await auditCard.getByRole("radio", { name: /^Run it first/ }).check();
  await auditCard.locator("[data-ask-action]").click();
  await expect
    .poll(async () => (await getAsk(audit.askId)).ask.answer?.text ?? "")
    .toContain("> Run \\_\\_tests\\_\\_ before the move.");
  // Drawn, the quoted words read as the option read: __tests__, not a bold "tests".
  await page.goto(`/issues/${audit.issueKey}/conversation`);
  const quote = page.locator(`[data-turn="ask:${audit.askId}"] blockquote`);
  await expect(quote).toContainText("Run __tests__ before the move.");
  await expect(quote.locator("strong")).toHaveCount(0);
});

// SLOW(>=2s): an issue page, a compact card quoted twice, then an answered ask changed and quoted.
test("on an issue's page Quote reply opens a compact card's box, follows typed text, leaves Answer as the typed line does, and quotes while an answer is changed", async ({
  page,
}) => {
  await page.setViewportSize(WIDE);
  await seedProject();
  const issue = await createIssue({
    assignee: "alice",
    force: true,
    project: PROJECT,
    spec: "# Flip audit\n",
    title: "Flip audit",
  });
  const flip = await createAsk(
    issue.key,
    { options: [{ label: "Keep" }, { label: "Flip" }], question: FLIP_QUESTION, urgency: "high" },
    asTester
  );
  const shipped = await createAsk(
    issue.key,
    {
      options: [{ label: "Ship" }, { label: "Hold" }],
      question: "Ship the **mirror** on Monday?",
      urgency: "med",
    },
    asTester
  );
  await answerAsk(shipped.id, { expected_edited_at: null, selected: ["Ship"] });

  await page.goto(`/issues/${issue.key}/conversation`);
  const card = page.locator(`[data-turn] [data-ask-card="${flip.id}"]`);
  await expect(card).toBeVisible();
  const question = card.locator("[data-ask-question]");
  const box = card.locator("[data-ask-answer]");
  const answer = card.locator("[data-ask-action]");
  // The compact card's box is closed behind Add a note, and Quote reply opens it.
  await expect(box).toHaveCount(0);
  await expect(card.getByRole("button", { name: "Add a note" })).toBeVisible();
  await selectWords(question, "go with B per the train?");
  expect(await barActions(page)).toEqual(["Quote replyq", "Defined"]);
  await quoteButton(page).click();
  await expectQuoted(box, "> go with B per the train?\n\n");

  // Typed text stays, and the quote follows it after an empty line. The quote ends in a question
  // mark and the typed line asks nothing: Answer stays refused, as the typed line alone leaves it.
  await box.fill(TYPED);
  await releaseFocus(page);
  await expect(answer).toBeDisabled();
  await selectWords(question, "go with B per the train?");
  await expect(bar(page)).toBeVisible();
  await page.keyboard.press("q");
  await expectQuoted(box, `${TYPED}\n\n> go with B per the train?\n\n`);
  await expect(answer).toBeDisabled();

  // An answered ask whose answer the reader changes quotes into its answer box, and its question
  // offers Define.
  const changed = page.locator(`[data-turn="ask:${shipped.id}"]`);
  await changed.getByRole("button", { name: "Change answer" }).click();
  const changedCard = changed.locator(`[data-ask-card="${shipped.id}"]`);
  await releaseFocus(page);
  await selectWords(changedCard.locator("[data-ask-question]"), "mirror");
  expect(await barActions(page)).toEqual(["Quote replyq", "Defined"]);
  await page.keyboard.press("q");
  await expectQuoted(changedCard.locator("[data-ask-answer]"), "> **mirror**\n\n");
});

// The card's own `d` acts while the focus is inside the card; a reader who selects an ask's words
// with the mouse on an issue's page leaves the focus where the press put it, and the page's `d`
// defines the words there.
// SLOW(>=2s): an issue page, a drag over an ask's question, and its definition.
test("on an issue's page d defines the words of an ask's question selected with the mouse", async ({
  page,
}) => {
  await page.setViewportSize(WIDE);
  await seedProject();
  const { askId, issueKey } = await seedAsk(ORDER_QUESTION, ORDER_OPTIONS, "Move order");
  const meaning = "The service of record whose rows the mirror copies.";
  await setModelRules([defineRule("ledger", meaning, [])]);
  await page.goto(`/issues/${issueKey}/conversation`);
  const card = page.locator(`[data-turn] [data-ask-card="${askId}"]`);
  await expect(card).toBeVisible();
  await dragOver(page, card.locator("[data-ask-question]"), "ledger");
  expect(await barActions(page)).toEqual(["Quote replyq", "Defined"]);
  expect(await card.evaluate((element) => element.contains(document.activeElement))).toBe(false);
  await page.keyboard.press("d");
  await expect(page.locator("[data-define-popover]")).toContainText(meaning);
});

// SLOW(>=2s): the Conversation tab and the spec, each with a thread opened by a quote.
test("a comment on the Conversation tab and one in a document's margin quote into their thread's reply box, and offer no Define", async ({
  page,
}) => {
  await page.setViewportSize(WIDE);
  await seedProject();
  const issue = await createIssue({
    assignee: "alice",
    force: true,
    project: PROJECT,
    spec: "# Lease mirror\n\nThe mirror keeps one lease per shard.\n",
    title: "Lease mirror",
  });
  const timeline = await createComment(
    issue.key,
    { body: "Of last week's 412 refused leases, 9 went through on a retry." },
    asTester
  );
  const margin = await createComment(
    issue.key,
    {
      anchor: { artifact: "spec", quote: "one lease per shard" },
      body: "A retry wins back **very little**.",
    },
    asTester
  );

  await page.goto(`/issues/${issue.key}/conversation`);
  const turn = page.locator(`[data-turn="comment:${timeline.id}"]`);
  await turn.getByRole("button", { name: "Expand thread" }).click();
  const thread = turn.locator(`[data-margin-item="${timeline.id}"]`);
  // The opened thread puts the cursor in its reply box; a reader selecting words leaves it.
  await expect(thread.getByRole("textbox", { name: "Reply" })).toBeVisible();
  await releaseFocus(page);
  await selectWords(thread.locator(".dispatch-markdown").first(), "9 went through on a retry.");
  expect(await barActions(page)).toEqual(["Quote replyq"]);
  await page.keyboard.press("q");
  await expectQuoted(
    thread.getByRole("textbox", { name: "Reply" }),
    "> 9 went through on a retry.\n\n"
  );

  // The reply box holds the quote unsent; the page asks before it is left, and the row leaves.
  page.on("dialog", (dialog) => void dialog.accept());
  await page.goto(`/issues/${issue.key}/spec`);
  // A folded margin card draws its comment inside the button that opens it, whose words are no
  // text to select; the card is opened first, as a reader opens it to read the comment whole.
  const card = page
    .getByRole("complementary", { name: "Review margin" })
    .locator(`[data-margin-item="${margin.id}"]`);
  await card.getByRole("button", { name: "A retry wins back very little." }).click();
  await expect(card).toHaveAttribute("aria-expanded", "true");
  await expect(card.getByRole("textbox", { name: "Reply" })).toBeVisible();
  const body = card.locator(".dispatch-markdown").filter({ hasText: "A retry wins back" });
  await expect(body).toBeVisible();
  await releaseFocus(page);
  await selectAcross(body, "A retry", body, ".");
  expect(await barActions(page)).toEqual(["Quote replyq"]);
  await page.keyboard.press("q");
  await expectQuoted(
    card.getByRole("textbox", { name: "Reply" }),
    "> A retry wins back **very little**.\n\n"
  );
});

// SLOW(>=2s): two asks with replies in their threads, each quoted from the Conversation tab.
test("a reply under an answered ask quotes into its thread's reply box, and a reply in an open ask's thread into the ask's answer box with no Define", async ({
  page,
}) => {
  await page.setViewportSize(WIDE);
  await seedProject();
  const issue = await createIssue({
    assignee: "alice",
    force: true,
    project: PROJECT,
    spec: "# Dry run\n",
    title: "Dry run",
  });
  const answered = await seedAskThread(
    issue.key,
    "What should `--dry-run` print for a lease the tool leaves alone?",
    LIST_REPLY
  );
  const open = await createAsk(
    issue.key,
    {
      options: [{ label: "Ship" }, { label: "Hold" }],
      question: "Ship `--dry-run` on Monday?",
      urgency: "med",
    },
    asTester
  );
  await createComment(
    issue.key,
    { ask_id: open.id, body: "The tool holds **one lease** per shard." },
    asTester
  );

  await page.goto(`/issues/${issue.key}/conversation`);
  const answeredTurn = page.locator(`[data-turn="ask:${answered}"]`);
  const answeredThread = answeredTurn.locator("[data-ask-thread]");
  await expect(answeredThread).toContainText("for a lease past its renewal");
  await selectAcross(answeredThread, "skip", answeredThread, "renewal");
  expect(await barActions(page)).toEqual(["Quote replyq"]);
  await page.keyboard.press("q");
  await expectQuoted(answeredTurn.getByRole("textbox", { name: "Reply" }), LIST_QUOTE);

  const openTurn = page.locator(`[data-turn="ask:${open.id}"]`);
  const openThread = openTurn.locator("[data-ask-thread]");
  await expect(openThread).toContainText("one lease");
  await releaseFocus(page);
  await selectAcross(openThread, "The tool holds", openThread, "per shard.");
  expect(await barActions(page)).toEqual(["Quote replyq"]);
  await page.keyboard.press("q");
  await expectQuoted(
    openTurn.locator("[data-ask-answer]"),
    "> The tool holds **one lease** per shard.\n\n"
  );
});

// SLOW(>=2s): a project document opened in the Inbox's column at its block, and a quote.
test("a comment drawn under its block in a document quotes into its thread's reply box", async ({
  page,
}) => {
  await seedProject();
  const document = await createProjectDocument(PROJECT, {
    content: "# Mirror runbook\n\nThe mirror replays the ledger on a cold start.\n",
    name: "mirror-runbook.md",
  });
  const root = await createArtifactComment(document.artifact.id, {
    anchor: { artifact: document.artifact.slug, quote: "replays the ledger" },
    body: "How far back does a cold start replay?",
  });
  await createArtifactComment(
    document.artifact.id,
    { body: "One hour, then it **gives up**.", reply_to: root.id },
    asSession()
  );

  await page.goto("/");
  await selectRow(page, replyRowId(root.id));
  const pane = inboxPane(page);
  const thread = pane
    .locator(`[data-dispatch-thread-anchor="${root.anchor?.block_id ?? ""}"]`)
    .locator("[data-inbox-thread-body]");
  const reply = thread.locator(".dispatch-markdown").filter({ hasText: "One hour" });
  await expect(reply).toBeVisible();
  await selectAcross(reply, "One hour", reply, ".");
  expect(await barActions(page)).toEqual(["Quote replyq"]);
  await page.keyboard.press("q");
  await expectQuoted(
    thread.getByRole("textbox", { name: "Reply" }),
    "> One hour, then it **gives up**.\n\n"
  );
});

// SLOW(>=2s): three pages - the Conversation tab, the Agents page and the Inbox - each quoted once.
test("a message on the Conversation tab and on the Agents page sets the composer to reply to it with the quote, and one read in the Inbox quotes into its thread's reply box", async ({
  page,
}) => {
  await page.setViewportSize(WIDE);
  await setLiveSessions([THREAD_SESSION]);
  await seedProject();
  const issue = await createIssue({
    assignee: "alice",
    force: true,
    project: PROJECT,
    spec: "# Rollout\n",
    title: "Rollout",
  });
  const message = await createMessage(
    issue.key,
    { body: "The mirror can move on any day.\n\nThe ledger follows on **Thursday**." },
    asSession()
  );
  const conversation = await seedMessageThread(
    issue.key,
    "Where is the nightly build?",
    "It moved to the `north` runner."
  );
  await seedAgentThread({
    body: "Which checks have you read?",
    replies: ["Both hold: the mirror keeps **one lease per shard**."],
  });

  // The Conversation tab: its docked composer replies to the message and takes the quote.
  await page.goto(`/issues/${issue.key}/conversation`);
  const turn = page.locator(`[data-turn="message:${message.id}"]`);
  const body = turn.locator(".dispatch-markdown").first();
  await expect(body).toContainText("The ledger follows on Thursday.");
  await selectAcross(body, "The ledger", body, ".");
  expect(await barActions(page)).toEqual(["Quote replyq"]);
  await page.keyboard.press("q");
  // The composer docked at the tab's foot, the one whose field is the Comment box.
  const docked = page
    .getByRole("form", { name: "Comment composer" })
    .filter({ has: page.getByRole("textbox", { exact: true, name: "Comment" }) });
  await expectQuoted(
    docked.getByRole("textbox", { exact: true, name: "Comment" }),
    "> The ledger follows on **Thursday**.\n\n"
  );
  await expect(docked).toContainText("Replying to");

  // The Agents page: the row's composer replies to the session's message and takes the quote.
  await page.goto("/agents");
  const row = page.locator(`[data-agent-row="${THREAD_SESSION.session_id}"]`);
  await row.locator("[data-agent-toggle]").click();
  const reply = row.locator(".dispatch-markdown").filter({ hasText: "Both hold" });
  await expect(reply).toBeVisible();
  await releaseFocus(page);
  await selectAcross(reply, "Both hold", reply, ".");
  expect(await barActions(page)).toEqual(["Quote replyq"]);
  await page.keyboard.press("q");
  const composer = row.locator("[data-agent-composer]");
  await expectQuoted(
    composer.locator("textarea"),
    "> Both hold: the mirror keeps **one lease per shard**.\n\n"
  );
  await expect(composer).toContainText("Replying to");

  // The Inbox: a message conversation read in its column quotes into its own reply box.
  await page.goto("/");
  await selectRow(page, replyRowId(conversation));
  const thread = inboxPane(page).locator('[data-inbox-thread-body="message"]');
  const answer = thread.locator(".dispatch-markdown").filter({ hasText: "It moved" });
  await expect(answer).toBeVisible();
  await selectAcross(answer, "It moved", answer, "runner.");
  expect(await barActions(page)).toEqual(["Quote replyq"]);
  await page.keyboard.press("q");
  await expectQuoted(thread.locator("textarea"), "> It moved to the `north` runner.\n\n");
});

// SLOW(>=2s): three surfaces read, each with a fixed wait past the bar's own.
test("over a decided suggestion's thread, a document's own text and a decision block, a selection offers no Quote reply and q quotes nothing", async ({
  page,
}) => {
  await page.setViewportSize(WIDE);
  await seedProject();
  const issue = await createIssue({
    assignee: "alice",
    force: true,
    project: PROJECT,
    spec: "# Order\n\nThe quick brown fox moves first.\n",
    title: "Order",
  });
  const suggestion = await createComment(issue.key, {
    anchor: { artifact: "spec", quote: "fox" },
    body: "Suggested replacement.",
    suggestion: { replace_with: "cat" },
  });
  await rejectSuggestion(suggestion.id, { login: "bob" });
  const approval = withOpenedAsk(
    await requestApproval(issue.primary_artifact_id, { summary: "The order." }, asTester)
  );
  const block = await seedSurface("decision block", { title: "Flip order" });

  // A decided suggestion's thread takes no reply.
  await page.goto(`/issues/${issue.key}/conversation`);
  const turn = page.locator(`[data-turn="comment:${suggestion.id}"]`);
  await turn.getByRole("button", { name: "Expand thread" }).click();
  const thread = turn.locator(`[data-margin-item="${suggestion.id}"]`);
  await expect(thread.getByText(/^Rejected by bob/)).toBeVisible();
  // Its card draws the replacement it proposed, which was decided.
  await selectWords(thread.getByRole("insertion"), "cat");
  await expectNoQuoteReply(page);

  // A document's own text, read in the Inbox's column.
  await page.goto(`/?item=${approval.ask.id}`);
  const document = inboxPane(page).locator(".ProseMirror");
  await expect(document).toContainText("quick brown fox");
  await selectWords(document, "quick brown fox");
  await expectNoQuoteReply(page);
  expect((await getArtifactText(issue.primary_artifact_id)).markdown).toContain(
    "The quick brown fox moves first."
  );

  // A decision block, its question and its card's options alike.
  await page.goto(`/?item=${block.askId}`);
  const frame = inboxPane(page).locator('[data-proof-block-type="ask"]');
  await expect(frame).toBeVisible();
  await selectWords(frame.locator("[data-proof-block-content]"), "flip audit");
  await expectNoQuoteReply(page);
  await selectWords(frame.locator(`[data-ask-card="${block.askId}"] fieldset`), "Run it first");
  await expectNoQuoteReply(page);
});

// SLOW(>=2s): the phone Inbox, a touch selection's settling time, and one quote.
test("on a phone the bar stands at the foot of the screen, and its Quote reply quotes as on a desktop @phone @phone-only", async ({
  page,
}) => {
  await seedProject();
  const { askId } = await seedAsk(ORDER_QUESTION, ORDER_OPTIONS, "Move order");
  await page.goto("/");
  const card = inboxRow(page, askId).locator(`[data-ask-card="${askId}"]`);
  await expect(card).toBeVisible();
  await card.locator("[data-ask-question]").scrollIntoViewIfNeeded();
  await selectAcross(
    card.locator("[data-ask-question]"),
    "The ",
    card.locator("fieldset"),
    "Ledger first"
  );
  expect(await barActions(page)).toEqual(["Quote replyq", "Defined"]);
  // At the foot of the screen, 12 px above its edge, and centred across it, read again until the
  // bar's box is there rather than taken at one instant.
  const screen = page.viewportSize();
  if (screen === null) throw new Error("the page has no viewport size");
  await expect(async () => {
    const placed = await bar(page).boundingBox();
    if (placed === null) throw new Error("the bar has no box");
    expect(Math.abs(screen.height - 12 - (placed.y + placed.height))).toBeLessThanOrEqual(1);
    expect(Math.abs(placed.x + placed.width / 2 - screen.width / 2)).toBeLessThanOrEqual(1);
  }).toPass({ timeout: 5_000 });
  await quoteButton(page).tap();
  await expectQuoted(card.locator("[data-ask-answer]"), ORDER_QUOTE);
});

/** The key a shortcut help row shows for `label` in the section `scope` names. */
function helpKey(help: Locator, scope: string, label: string): Locator {
  return help
    .getByRole("region", { name: scope })
    .locator("li")
    .filter({ has: help.page().getByText(label, { exact: true }) })
    .locator("kbd");
}

// SLOW(>=2s): the help opened twice, on the Inbox and on an issue page.
test("the shortcut help lists q on the ask card and on the page", async ({ page }) => {
  await seedProject();
  const { askId, issueKey } = await seedAsk(ORDER_QUESTION, ORDER_OPTIONS, "Move order");
  await page.goto(`/?item=${askId}`);
  const card = inboxPane(page).locator(`[data-ask-card="${askId}"]`);
  await selectWords(card.locator("[data-ask-question]"), "ledger");
  await inboxRow(page, askId).focus();
  await page.keyboard.press("Shift+Slash");
  const help = page.getByRole("dialog", { name: "Keyboard shortcuts" });
  await expect(help).toBeVisible();
  await expect(helpKey(help, "Ask card", "Quote selected words in your answer")).toHaveText("q");
  await expect(helpKey(help, "Inbox", "Quote selected words in a reply")).toHaveText("q");
  await page.keyboard.press("Escape");
  await expect(help).toHaveCount(0);

  await page.goto(`/issues/${issueKey}`);
  await expect(page.getByTestId("issue-header")).toBeVisible();
  await page.keyboard.press("Shift+Slash");
  await expect(help).toBeVisible();
  await expect(helpKey(help, "Issue", "Quote selected words in a reply")).toHaveText("q");
});
