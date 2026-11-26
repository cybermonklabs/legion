/** These rows need the real server to index ask blocks and decision ties, determine whose turn
 *  it is, render the approval of a stored document version, and read back the answers, Keeps
 *  and anchored Changes the reader sends. The inventory reads the same records on both builds. */

// DOM contract (INFRA-5702): data-ask-body and data-decision-card mark the shared roots.
// AskCard still carries data-ask-card and the ask-<id> test id. The answer field is marked
// data-ask-answer. Only real options carry data-ask-option. The body's question, type, author
// and time share the first line, except that the document frame draws the question and type.
// The decision card sits in its existing section or margin placement; its time is a <time>.

import { readFile, writeFile } from "node:fs/promises";
import {
  createAsk,
  createIssue,
  createProject,
  getAsk,
  getInbox,
  getIssueDecisions,
  listComments,
  listMyAnswers,
} from "./api";
import {
  ASK_TODAY,
  type AskTodayDecision,
  type AskTodaySeed,
  type MainHolds,
  mainHolds,
  READER,
  seedAskToday,
} from "./dashboard-seed";
import { literal } from "./dashboard-support";
import { documentEditor } from "./editor";
import { drawer, peekTrigger } from "./inbox-drawer";
import { inboxList, inboxPane, inboxRow, selectedRow, selectRow } from "./inbox-pane";
import { inboxAt } from "./inbox-pane-scroll";
import { replyRowId } from "./inbox-threads";
import { viewportAt } from "./issue-page";
import { navigateInApp } from "./sends";
import { expect, type Locator, type Page, test } from "./test";
import { type ApiRequests, expectLoadSettled, watchApiRequests } from "./traffic";

const BODY = "[data-ask-body]";
const CARD = "[data-decision-card]";
const ANSWER = "[data-ask-answer]";
const OPTION = "[data-ask-option]";
const DISCLOSURE = "Add a note or answer in your own words";
const CHANGE = "Measure the update rate before fixing the count.";
const SHAPED_ANSWER = "Can we try the smaller batch first?";
const inventoryFile = process.env.ASK_INVENTORY_FILE;
const recording = process.env.ASK_INVENTORY === "record";
const inventoryMode = recording || inventoryFile !== undefined;

let holds: MainHolds | undefined;
test.use({ signedIn: READER });
test.beforeAll(async () => {
  holds = await mainHolds();
});

async function seed(): Promise<AskTodaySeed> {
  if (holds === undefined) throw new Error("The server probe did not run");
  return seedAskToday(holds);
}

function button(scope: Locator, name: string): Locator {
  return scope.getByRole("button", { exact: true, name });
}

function ask(scope: Page | Locator, id: string): Locator {
  return scope.getByTestId(`ask-${id}`);
}

function turn(page: Page, id: string): Locator {
  return ask(page.locator(`[data-turn="ask:${id}"]`), id);
}

function spec(seed: AskTodaySeed): string {
  return `/issues/${seed.key}/spec`;
}

function conversation(seed: AskTodaySeed): string {
  return `/issues/${seed.key}/conversation`;
}

function margin(page: Page): Locator {
  return page.getByTestId("margin-sheet");
}

function block(page: Page | Locator): Locator {
  return page.locator(`[data-dispatch-ask-block="${ASK_TODAY.blockId}"]`);
}

function answerRow(page: Page, id: string): Locator {
  return page.locator(`[data-answer-row="${id}"]`).filter({
    has: page.getByRole("button", { exact: true, name: "Change answer" }),
  });
}

async function openChangeAnswer(page: Page, id: string): Promise<Locator> {
  await button(answerRow(page, id), "Change answer").click();
  return ask(page.locator(`[data-answer-card="${id}"]`), id);
}

async function wide(page: Page): Promise<void> {
  await page.setViewportSize(viewportAt(1440));
}

async function narrow(page: Page): Promise<void> {
  await page.setViewportSize(viewportAt(1279));
}

interface Surface {
  readonly name: string;
  readonly id: (seed: AskTodaySeed) => string;
  readonly block?: boolean;
  readonly changing?: boolean;
  readonly multiple?: boolean;
  readonly question: string;
  open(page: Page, seed: AskTodaySeed): Promise<Locator>;
}

const SURFACES: readonly Surface[] = [
  {
    id: (seed) => seed.asks.byOption.id,
    name: "Inbox beside the reading pane at 1440 px",
    question: ASK_TODAY.optionQuestion,
    async open(page, seed) {
      await wide(page);
      await page.goto(inboxAt(seed.asks.byOption.id));
      return ask(inboxPane(page), seed.asks.byOption.id);
    },
  },
  {
    id: (seed) => seed.asks.byOption.id,
    name: "Inbox card at 1279 px",
    question: ASK_TODAY.optionQuestion,
    async open(page, seed) {
      await narrow(page);
      await page.goto("/");
      return ask(inboxList(page), seed.asks.byOption.id);
    },
  },
  {
    id: (seed) => seed.asks.byOption.id,
    name: "Inbox drawer",
    question: ASK_TODAY.optionQuestion,
    async open(page, seed) {
      await wide(page);
      await page.goto("/agents");
      await peekTrigger(page).click();
      return ask(drawer(page), seed.asks.byOption.id);
    },
  },
  {
    id: (seed) => seed.records.thread,
    multiple: true,
    name: "ThreadBody",
    question: "Which orders work for the next batch?",
    async open(page, seed) {
      await wide(page);
      await page.goto("/?type=reply");
      await inboxRow(page, replyRowId(seed.records.thread)).locator("time").first().click();
      return ask(inboxPane(page).locator('[data-inbox-thread-body="ask"]'), seed.records.thread);
    },
  },
  {
    id: (seed) => seed.records.anchored,
    name: "reading pane for an anchored ask",
    question: "How should the reader order these items?",
    async open(page, seed) {
      await wide(page);
      await page.goto(inboxAt(seed.records.anchored));
      return ask(inboxPane(page), seed.records.anchored);
    },
  },
  {
    id: (seed) => seed.asks.byOption.id,
    name: "Conversation tab",
    question: ASK_TODAY.optionQuestion,
    async open(page, seed) {
      await wide(page);
      await page.goto(conversation(seed));
      return turn(page, seed.asks.byOption.id);
    },
  },
  {
    changing: true,
    id: (seed) => seed.records.changed,
    name: "Answers page in change mode",
    question: "Which batch should run first?",
    async open(page, seed) {
      await wide(page);
      await page.goto("/answers");
      return openChangeAnswer(page, seed.records.changed);
    },
  },
  {
    block: true,
    id: (seed) => seed.records.block,
    name: "document ask block",
    question: ASK_TODAY.blockQuestion,
    async open(page, seed) {
      await wide(page);
      await page.goto(seed.blockPath);
      return ask(block(documentEditor(page)), seed.records.block);
    },
  },
  {
    id: (seed) => seed.records.anchored,
    name: "comments margin",
    question: "How should the reader order these items?",
    async open(page, seed) {
      await wide(page);
      await page.goto(spec(seed));
      return ask(margin(page), seed.records.anchored);
    },
  },
];

const KIND = /^question$/i;

/** The first text rectangle, not the box of a paragraph that can span several lines. */
async function firstWords(words: Locator) {
  return words.evaluate((element) => {
    const range = document.createRange();
    range.selectNodeContents(element);
    const rect = range.getClientRects()[0] ?? element.getBoundingClientRect();
    return { bottom: rect.bottom, right: rect.right, top: rect.top };
  });
}

async function expectRow(body: Locator, question: string, authorName: string): Promise<void> {
  const words = body.getByText(question, { exact: true }).first();
  const kind = body.getByText(KIND).first();
  const author = body.getByText(authorName, { exact: true }).first();
  const time = body.locator("time").first();
  for (const part of [words, kind, author, time]) await expect(part).toBeVisible();
  const first = await firstWords(words);
  let right = first.right;
  for (const part of [kind, author, time]) {
    // The part is visible above, so its layout box is settled and this one read is its place.
    const box = await part.boundingBox();
    if (box === null) throw new Error("The first line has a part with no layout box");
    expect(box.y, "first-line metadata sits under the words").toBeLessThan(first.bottom);
    expect(box.y + box.height, "first-line metadata sits above the words").toBeGreaterThan(
      first.top
    );
    expect(
      box.x,
      "first-line metadata sits left of the words or out of order"
    ).toBeGreaterThanOrEqual(right - 1);
    right = box.x + box.width;
  }
}

async function expectOrdered(parts: readonly Locator[]): Promise<void> {
  for (const part of parts) await expect(part).toBeVisible();
  // The parts can have different wrappers on different surfaces. Compare their DOM order.
  const nodes = await Promise.all(parts.map((part) => part.elementHandle()));
  const inOrder = await parts[0]?.evaluate(
    (_, elements) =>
      elements.every((node, index) => {
        const next = elements[index + 1];
        return (
          next === undefined ||
          Boolean(node.compareDocumentPosition(next) & Node.DOCUMENT_POSITION_FOLLOWING)
        );
      }),
    nodes
  );
  expect(inOrder, "the answer parts run in the same order in the body").toBe(true);
}

// SLOW(>=2s): seed the real records and open each of the nine surfaces in turn.
test("each surface draws the ask with the one ask body", async ({ page }) => {
  test.skip(inventoryMode, "The inventory run reads the legacy layout as well");
  const data = await seed();
  for (const surface of SURFACES) {
    await test.step(surface.name, async () => {
      const card = await surface.open(page, data);
      await expect(card).toBeVisible();
      if (surface === SURFACES[0]) {
        await expect(selectedRow(page)).toHaveAttribute("data-inbox-row", surface.id(data));
        await expect(inboxRow(page, surface.id(data)).locator(BODY)).toHaveCount(0);
      }
      const body = card.locator(BODY);
      await expect(body, surface.name).toHaveCount(1);
      await expect(card).toHaveAttribute("data-ask-card", surface.id(data));
      if (surface.block) {
        await expect(body).not.toContainText(surface.question);
        await expect(
          block(documentEditor(page)).getByRole("img", { name: ASK_TODAY.picture })
        ).toBeVisible();
      } else {
        await expectRow(body, surface.question, data.plannerName);
      }

      const role = surface.multiple ? "checkbox" : "radio";
      const options = ASK_TODAY.options.map(({ description, label }) =>
        body.getByRole(role, { exact: true, name: `${label} ${description}` })
      );
      for (const option of options) await expect(option).toHaveAttribute("data-ask-option", "");
      await expect(body.locator(OPTION)).toHaveCount(options.length);
      const other = body.getByRole(role, { exact: true, name: "Other" });
      await expect(other).not.toHaveAttribute("data-ask-option");
      await expectOrdered([
        ...(surface.block ? [] : [body.getByText(surface.question, { exact: true }).first()]),
        ...options,
        other,
        body.locator(ANSWER),
        button(body, surface.changing ? "Save answer" : "Answer"),
        button(body, surface.changing ? "Cancel" : "Ask back"),
        ...(surface.changing ? [] : [button(body, "Decline to answer")]),
      ]);

      if (surface.multiple) {
        const thread = card.getByTestId(`thread-${surface.id(data)}`);
        await expect(thread).toContainText(ASK_TODAY.reply);
        await expect(body.getByTestId(`thread-${surface.id(data)}`)).toHaveCount(0);
        const [bodyBox, threadBox] = await Promise.all([body.boundingBox(), thread.boundingBox()]);
        expect(
          bodyBox !== null &&
            threadBox !== null &&
            (threadBox.y + threadBox.height <= bodyBox.y ||
              bodyBox.y + bodyBox.height <= threadBox.y),
          "AskCard's thread must sit wholly above or below its body"
        ).toBe(true);
      }
    });
  }
});

// SLOW(>=2s): seed the real records and read the untouched answer field on each surface.
test("the answer box stands in place on every surface", async ({ page }) => {
  test.skip(inventoryMode, "The inventory run reads the legacy layout as well");
  const data = await seed();
  // Begin at Conversation, where the legacy layout hides the field.
  for (const surface of [...SURFACES.slice(5), ...SURFACES.slice(0, 5)]) {
    const card = await surface.open(page, data);
    await expect(card.locator(ANSWER), surface.name).toBeVisible();
    await expect(button(card, DISCLOSURE), surface.name).toHaveCount(0);
  }
});

/** The words of `words` that the page breaks over two lines: each word's own text rectangles,
 *  read where it stands, sit on more than one line. A word that fits its line is one rectangle. */
async function brokenWords(words: Locator): Promise<string[]> {
  return words.evaluate((element) => {
    const broken: string[] = [];
    const walker = document.createTreeWalker(element, NodeFilter.SHOW_TEXT);
    for (let node = walker.nextNode(); node !== null; node = walker.nextNode()) {
      for (const match of (node.textContent ?? "").matchAll(/\S+/g)) {
        const range = document.createRange();
        range.setStart(node, match.index);
        range.setEnd(node, match.index + match[0].length);
        const lines = new Set([...range.getClientRects()].map((rect) => Math.round(rect.top)));
        if (lines.size > 1) broken.push(match[0]);
      }
    }
    return broken;
  });
}

// The comments margin is a column about 384 px wide at a desktop width, where the first line's
// end (the kind, an agent's name control and the time) stands beside the words. The words must
// keep room to wrap at their spaces there, as they do on a phone, and never break inside a word.
test("in today's comments margin an agent's ask keeps each word of its question whole", async ({
  page,
}) => {
  test.skip(inventoryMode, "The inventory run reads the legacy layout as well");
  const question = "Should the runbook keep its troubleshooting section?";
  await createProject({ key: "WHOLE", name: "Whole words" });
  const issue = await createIssue({
    project: "WHOLE",
    spec: "Restart, reseed, reopen the signed-in URL.",
    title: "Whole words",
  });
  const asked = await createAsk(
    issue.key,
    { anchor: { artifact: "spec", quote: "reseed" }, question },
    {
      actor: { id: "whole-words-tester", kind: "session", origin: { session_title: "Tester" } },
      as: "agent",
    }
  );
  await wide(page);
  await page.goto(`/issues/${issue.key}/spec`);
  const body = ask(margin(page), asked.id).locator(BODY);
  const words = body.getByText(question, { exact: true }).first();
  await expect(words).toBeVisible();
  expect(
    await brokenWords(words),
    "the margin breaks a word of the question over two lines"
  ).toEqual([]);
});

// SLOW(>=2s): seed an approval and a pictured ask block, then open them in the reading pane.
test("the reading pane keeps its approval view and the block draws the body without its question", async ({
  page,
}) => {
  test.skip(inventoryMode, "The inventory run reads the legacy layout as well");
  const data = await seed();
  await wide(page);
  await page.goto(inboxAt(data.asks.approval.id));
  const pane = inboxPane(page);
  await expect(pane.getByTestId("pane-approval-question")).toBeVisible();
  await expect(
    pane.getByRole("region", { name: `Document version ${data.asks.approval.version}` })
  ).toBeVisible();
  await expect(button(pane.locator("header"), "Approve")).toBeVisible();
  await expect(pane.getByTestId(`ask-${data.asks.approval.id}`)).toHaveCount(0);
  await expect(pane.locator(BODY)).toHaveCount(0);

  for (const id of [data.asks.byOption.id, data.records.anchored, data.records.questionShaped]) {
    await selectRow(page, id);
    await expect(ask(pane, id).locator(BODY)).toBeVisible();
  }
  await selectRow(page, data.records.block);
  const frame = block(pane);
  await expect(frame.getByText(ASK_TODAY.blockQuestion, { exact: true })).toBeVisible();
  await expect(frame.locator("[data-dispatch-ask-pill]")).toHaveText("High");
  const body = ask(frame, data.records.block).locator(BODY);
  await expect(body).toBeVisible();
  await expect(body).not.toContainText(ASK_TODAY.blockQuestion);
  await expect(body.getByText(KIND)).toHaveCount(0);
  await expect(body.getByText(data.plannerName, { exact: true }).first()).toBeVisible();
  await expect(body.locator("time").first()).toHaveAttribute("datetime", /.+/);

  // The awaiting pane uses ApprovalChip's Approve; the same request's earlier approval and
  // Show diff are drawn on its card in the drawer, not duplicated under the header chip.
  await page.goto("/agents");
  await peekTrigger(page).click();
  const approval = ask(drawer(page), data.asks.approval.id).locator(BODY);
  await expect(approval.getByText("Approval requested", { exact: true })).toBeVisible();
  await expect(approval.getByText(/^Approved at version /)).toBeVisible();
  await expect(button(approval, "Show diff")).toBeVisible();
});

// SLOW(>=2s): the full issue seed and the live document in the Inbox's reading pane.
test("in the Inbox 1 to 9 pick only real options and Enter reaches the box", async ({ page }) => {
  test.skip(inventoryMode, "The inventory run reads the legacy layout as well");
  const data = await seed();
  await wide(page);
  await page.goto(inboxAt(data.records.block));
  const row = selectedRow(page);
  await expect(row).toHaveAttribute("data-inbox-row", data.records.block);
  const card = ask(inboxPane(page), data.records.block);
  const field = card.locator(ANSWER);
  await expect(field, "Enter must reach an already visible field").toBeVisible();
  const options = card.locator(OPTION);
  const other = card.getByRole("radio", { exact: true, name: "Other" });
  await expect(options).toHaveCount(2);
  for (const digit of [1, 2, 3, 4, 5, 6, 7, 8, 9]) {
    await row.focus();
    await page.keyboard.press(String(digit));
    await expect(options.nth(digit === 1 ? 0 : 1)).toBeChecked();
    await expect(other).not.toBeChecked();
  }
  await row.focus();
  await page.keyboard.press("Enter");
  await expect(field).toBeFocused();
});

async function showQuestionChoice(card: Locator): Promise<void> {
  await card.locator(ANSWER).fill(SHAPED_ANSWER);
  await button(card, "Answer").click();
}

// SLOW(>=2s): seed and read a question-shaped choice, a decline and two answer versions.
test("a question-shaped answer offers Ask back instead, and a settled ask draws its record", async ({
  page,
}) => {
  test.skip(inventoryMode, "The inventory run reads the legacy layout as well");
  const data = await seed();
  await wide(page);
  await page.goto(conversation(data));
  const open = turn(page, data.records.questionShaped).locator(BODY);
  await expect(open.locator(ANSWER)).toBeVisible();
  await expect(open.locator(`${OPTION}:checked`)).toHaveCount(0);
  await showQuestionChoice(open);
  await expect(open.getByRole("group", { name: "Your answer reads as a question" })).toBeVisible();
  await expect(button(open, "Ask back instead")).toBeVisible();
  await expect(button(open, "Answer with it anyway")).toBeVisible();
  await expect(turn(page, data.records.declined).getByTestId("ask-resolution-badge")).toHaveText(
    "Declined"
  );
  await expect(
    turn(page, data.records.changed).getByText("Show 1 earlier answer", { exact: true })
  ).toBeVisible();
});

// SLOW(>=2s): answer in Conversation, then open the document and Answers on cold loads.
test("an answer from today's Conversation is drawn the same in the block and Change answer opens the body", async ({
  page,
}) => {
  test.skip(inventoryMode, "The inventory run reads the legacy layout as well");
  const data = await seed();
  const id = data.records.block;
  const answer = "Begin with one small batch.";
  const label = ASK_TODAY.options[0].label;
  await wide(page);
  await page.goto(conversation(data));
  const open = turn(page, id);
  await expect(open.locator(ANSWER)).toBeVisible();
  await open.getByRole("radio", { name: /^Newest first/ }).check();
  await open.locator(ANSWER).fill(answer);
  await button(open, "Answer").click();
  await expect
    .poll(async () => (await getAsk(id)).ask)
    .toMatchObject({
      answer: { selected: [label], text: answer, user: READER },
      state: "answered",
    });
  await expect(open).toContainText(`Answered by ${READER}`);
  await expect(open.getByText(answer, { exact: true })).toBeVisible();
  await expect(open.locator('[data-selected="true"]')).toContainText(label);

  await page.goto(data.blockPath);
  const settled = ask(block(documentEditor(page)), id);
  await expect(settled).toContainText(`Answered by ${READER}`);
  await expect(settled.getByText(answer, { exact: true })).toBeVisible();
  await expect(settled.locator('[data-selected="true"]')).toContainText(label);
  await expect(settled.locator(ANSWER)).toHaveCount(0);
  await expect(settled.locator(BODY)).toHaveCount(1);

  await page.goto("/answers");
  const edit = (await openChangeAnswer(page, id)).locator(BODY);
  await expect(edit.locator(ANSWER)).toHaveValue(answer);
  const save = button(edit, "Save answer");
  const cancel = button(edit, "Cancel");
  await expectOrdered([save, cancel]);
  const [a, b] = await Promise.all([save.boundingBox(), cancel.boundingBox()]);
  expect(
    a !== null && b !== null && a.x < b.x && a.y < b.y + b.height && b.y < a.y + a.height
  ).toBe(true);
});

function decisionRoots(page: Page, entry: AskTodayDecision, inMargin: boolean): Locator {
  // Name the record by its words in main's region. These wrappers are also used by the
  // inventory, so the selector must not require the new card marker.
  return inMargin
    ? margin(page).getByRole("article", { exact: true, name: `Decision: ${entry.heading}` })
    : page
        .getByRole("region", { exact: true, name: entry.section })
        .locator("li[data-decision]")
        .filter({
          has: page.getByRole("heading", { exact: true, name: entry.heading }),
        });
}

function markedCard(root: Locator): Locator {
  return root.and(root.page().locator(CARD)).or(root.locator(CARD));
}

async function expectUnclippedInCard(words: Locator): Promise<void> {
  const unclipped = await words.evaluate((element, cardSelector) => {
    const range = document.createRange();
    range.selectNodeContents(element);
    const last = [...range.getClientRects()].at(-1);
    if (last === undefined) return false;
    for (let parent: Element | null = element; parent !== null; parent = parent.parentElement) {
      const style = getComputedStyle(parent);
      if (style.webkitLineClamp !== "none" || style.textOverflow === "ellipsis") return false;
      if (
        /hidden|clip/.test(style.overflowY) &&
        last.bottom > parent.getBoundingClientRect().bottom + 1
      ) {
        return false;
      }
      if (parent.matches(cardSelector)) return true;
    }
    return false;
  }, CARD);
  expect(unclipped, "the card clips its decision text").toBe(true);
}
async function expectDecision(
  card: Locator,
  entry: AskTodayDecision,
  data: AskTodaySeed
): Promise<void> {
  const person = entry.case === "person";
  const waiting = entry.case === "first" || entry.case === "second";
  const unsure = entry.case === "second";
  await expect(
    card.getByText(person ? "A person's decision" : "Agent's decision", { exact: true })
  ).toBeVisible();
  const text = card.getByText(new RegExp(literal(entry.text))).last();
  await expect(text).toBeVisible();
  await expectUnclippedInCard(text);
  await expect(card.locator("time").first()).toHaveAttribute("datetime", /.+/);
  await expect(card.getByText("Unsure", { exact: true })).toHaveCount(unsure ? 1 : 0);
  await expect(card.getByText("Waiting for review", { exact: true })).toHaveCount(waiting ? 1 : 0);
  await expect(card.getByText(/^Kept by /)).toHaveCount(entry.case === "kept" ? 1 : 0);
  await expect(card.getByText("Replaced", { exact: true })).toHaveCount(
    entry.case === "replaced" ? 1 : 0
  );
  await expect(card.getByText(/^(?:Author unknown|Changed by |Level:)/)).toHaveCount(0);
  await expect(card.getByText(data.plannerName, { exact: true })).toHaveCount(0);
  await expect(card.getByText("Shaped in the spec:", { exact: true })).toHaveCount(0);
  await expect(card.getByRole("link", { name: /^Block / })).toHaveCount(0);
  for (const line of entry.ties) {
    await expect(
      card
        .locator("[data-decision-statements]")
        .getByRole("link", { name: new RegExp(`^${literal(line)}`) })
    ).toHaveAttribute("href", /#b-/);
  }
  await expect(card.getByText(/^Changed since/)).toHaveCount(unsure ? 1 : 0);
  await expect(card.getByText("out of date", { exact: true })).toHaveCount(unsure ? 1 : 0);
  if (unsure) {
    const last = card.getByText(ASK_TODAY.unsure, { exact: true });
    await expect(last).toBeVisible();
    await expectUnclippedInCard(last);
  }
  await expect(card.getByRole("link", { name: "The ask it answers" })).toHaveCount(person ? 1 : 0);
  for (const action of ["Keep", "Change"])
    await expect(button(card, action)).toHaveCount(waiting ? 1 : 0);
}

// SLOW(>=2s): index five decisions and their ties, then open the tab and the comments margin.
test("today's Decisions tab and the margin draw every decision with the one card", async ({
  page,
}) => {
  test.skip(inventoryMode, "The inventory run reads the legacy layout as well");
  const data = await seed();
  await wide(page);
  await page.goto(`/issues/${data.key}/decisions`);
  const panel = page.locator("#issue-decisions-panel");
  await expect(panel.getByRole("heading", { level: 2 })).toHaveText([
    "Open questions",
    "Waiting for your review",
    "Your decisions",
    "Replaced",
  ]);
  await expect(
    panel
      .getByRole("region", { name: "Open questions" })
      .getByRole("link", { name: ASK_TODAY.blockQuestion, exact: false })
  ).toBeVisible();
  await expect(panel.locator(BODY)).toHaveCount(0);
  await expect(panel.locator(CARD)).toHaveCount(5);
  for (const entry of data.decisionCases)
    await expectDecision(markedCard(decisionRoots(page, entry, false)), entry, data);
  await page.goto(spec(data));
  for (const entry of data.decisionCases) {
    const roots = decisionRoots(page, entry, true);
    await expect(roots).toHaveCount(entry.case === "replaced" ? 0 : entry.ties.length);
    // The count above has settled on this many roots, so one read of all of them lists them.
    for (const root of await roots.all()) await expectDecision(markedCard(root), entry, data);
  }
});

function changePost(page: Page, key: string) {
  return page.waitForRequest(
    (request) =>
      request.method() === "POST" &&
      new URL(request.url()).pathname === `/api/v1/issues/${key}/comments`
  );
}

async function readKept(seed: AskTodaySeed): Promise<boolean> {
  const read = await getIssueDecisions(seed.key);
  if (!seed.holds.decisionsAreNodes) {
    return (read.decisions ?? []).some(
      (item) =>
        item.block_id === seed.decisions.first.id &&
        item.kept?.kept_by === READER &&
        !item.waiting_review
    );
  }
  return (
    "entries" in read &&
    Array.isArray(read.entries) &&
    read.entries.some(
      (entry) =>
        entry.document?.block_id === seed.decisions.first.id && entry.kept_by?.id === READER
    )
  );
}

// SLOW(>=2s): two decision writes from the margin and server reads of both effects.
test("Keep and Change in the margin write as DecisionActions writes", async ({ page }) => {
  test.skip(inventoryMode, "The inventory run reads the legacy layout as well");
  const data = await seed();
  await wide(page);
  await page.goto(spec(data));
  const first = margin(page).getByRole("article", {
    name: `Decision: ${data.decisions.first.heading}`,
  });
  await expect(first).toHaveCount(2);
  const keep = button(first.first(), "Keep");
  await expect(keep).toBeVisible();
  await keep.click();
  await expect.poll(() => readKept(data)).toBe(true);
  await expect(button(first, "Keep")).toHaveCount(0);
  await expect(first.getByText(/^Kept by /)).toHaveCount(2);

  const second = margin(page).getByRole("article", {
    name: `Decision: ${data.decisions.second.heading}`,
  });
  await button(second, "Change").click();
  await second.getByRole("textbox", { name: "Your change" }).fill(CHANGE);
  const sent = changePost(page, data.key);
  await button(second, "Send change").click();
  expect((await sent).postDataJSON()).toMatchObject({
    anchor: {
      artifact: data.decisionsDocumentId,
      occurrence: 0,
      quote: expect.stringContaining(data.decisions.second.words),
    },
    body: CHANGE,
  });
  await expect
    .poll(async () =>
      (await listComments(data.key, data.decisionsDocumentId)).find((row) => row.body === CHANGE)
    )
    .toMatchObject({
      anchor: { orphaned: false, quote: expect.stringContaining(data.decisions.second.words) },
      author: { id: READER },
    });
});

/** Keep path and query, including the id or scope of an ask, artifact or list read. */
function getPaths(traffic: ApiRequests): string[] {
  return [
    ...new Set(traffic.requests.filter(({ method }) => method === "GET").map(({ path }) => path)),
  ].sort();
}

// SLOW(>=2s): cold loads of nine surfaces, plus the tab and margin's staged Change reads.
test("each surface sends main's reads and no other", async ({ context }) => {
  test.skip(inventoryMode, "The inventory run reads the legacy layout as well");
  test.setTimeout(120_000);
  const data = await seed();
  for (const surface of SURFACES) {
    const page = await context.newPage();
    try {
      const traffic = watchApiRequests(page);
      const card = await surface.open(page, data);
      await expect(card.locator(BODY), surface.name).toBeVisible();
      await expectLoadSettled(traffic);
      expectReads(getPaths(traffic), data, surface.name);
    } finally {
      await page.close();
    }
  }
  for (const where of ["decisions", "spec"] as const) {
    const page = await context.newPage();
    try {
      await wide(page);
      const traffic = watchApiRequests(page);
      await page.goto(`/issues/${data.key}/${where}`);
      const scope = where === "spec" ? margin(page) : page.locator("#issue-decisions-panel");
      const card = scope.locator(CARD).filter({ hasText: data.decisions.second.heading });
      await expect(card).toBeVisible();
      await expectLoadSettled(traffic);
      const reads = getPaths(traffic);
      expectReads(reads, data, where);
      const text = `/api/v1/artifacts/${data.decisionsDocumentId}/text`;
      expect(reads).not.toContain(text);
      await button(card, "Change").click();
      await card.getByRole("textbox", { name: "Your change" }).fill(CHANGE);
      await expectLoadSettled(traffic);
      expect(getPaths(traffic)).toEqual(reads);
      traffic.reset();
      const sent = changePost(page, data.key);
      await button(card, "Send change").click();
      await sent;
      expect(getPaths(traffic)).toContain(text);
    } finally {
      await page.close();
    }
  }
});

/** Main's shell reads and the issue, Inbox, Conversation, answer and document reads its surfaces
 *  already use. In particular a document read here must be for the document actually drawn,
 *  never decisions.md on a surface that shows only a decision card. */
function expectReads(paths: readonly string[], data: AskTodaySeed, surface: string): void {
  const issue = `/api/v1/issues/${data.key}`;
  const api = "/api/v1";
  const specArtifact = `${api}/artifacts/${data.specArtifactId}`;
  const picture = `${issue}/artifacts/${data.pictureSlug}/versions/1`;
  const askRead = (id: string) => `${api}/asks/${id}`;
  const shell = new Set([
    `${api}/agents`,
    `${api}/credential-requests`,
    `${api}/inbox`,
    `${api}/issues`,
    `${api}/me/agents/state`,
    `${api}/priorities`,
    `${api}/projects`,
    `${issue}/subscribers`,
  ]);
  // These are present even with the Spec tab hidden: today's page keeps its margin mounted.
  const project = data.key.split("-")[0];
  const issuePage = new Set([
    issue,
    `${issue}/asks?state=all`,
    `${issue}/comments`,
    `${api}/me/state`,
    `${api}/projects/${project}/architecture-sources`,
  ]);
  const document = new Set([
    picture,
    specArtifact,
    `${specArtifact}/text`,
    `${specArtifact}/blocks`,
    `${issue}/coverage`,
    `${api}/me/documents/${data.specArtifactId}/read`,
  ]);
  const askReads = new Set(
    [
      ...Object.values(data.records),
      ...Object.values(data.asks).filter((id): id is string => typeof id === "string"),
      data.asks.byOption.id,
      data.asks.approval.id,
    ].map(askRead)
  );
  const replyPaneReads = new Set([
    askRead(data.records.thread),
    issue,
    `${issue}/comments`,
    `${issue}/messages`,
  ]);
  const inbox =
    surface.startsWith("Inbox") || surface.startsWith("reading pane") || surface === "ThreadBody";
  const answers = surface.startsWith("Answers");
  const conversationTab = surface === "Conversation tab";
  const specSurface =
    surface === "document ask block" || surface === "comments margin" || surface === "spec";
  const decisionsTab = surface === "decisions";
  const allowed = (path: string) => {
    const sent = new URL(path, "http://ask-body.test");
    if (shell.has(sent.pathname) || sent.pathname === `${api}/reference-targets`) return true;
    if (sent.pathname === `${api}/me/replies`) {
      return (
        sent.searchParams.get("kind") === "reply,agent" ||
        sent.searchParams.get("issue") === data.key
      );
    }
    if (inbox) {
      if (surface === "ThreadBody") return replyPaneReads.has(path);
      if (surface === "Inbox card at 1279 px" || surface === "Inbox drawer") {
        return replyPaneReads.has(path) || askReads.has(path) || path === picture;
      }
      return false;
    }
    if (answers)
      return sent.pathname === `${api}/me/answers` || path === askRead(data.records.changed);
    if (issuePage.has(path) || (data.holds.treeRead && path === `${issue}/tree`)) return true;
    if (askReads.has(path)) return true;
    if (specSurface && document.has(path)) return true;
    if (specSurface || decisionsTab) {
      if (path === `${issue}/decisions`) return true;
      const target = sent.searchParams.get("to");
      if (
        sent.pathname === `${api}/references` &&
        target !== null &&
        (target.startsWith(`dispatch://${data.key}/spec#b-`) ||
          (specSurface &&
            target === `dispatch://${data.key}/spec` &&
            sent.searchParams.get("kind") === "part_of"))
      ) {
        return true;
      }
    }
    return (
      conversationTab &&
      (path === `${issue}/asks?state=all&thread=1` ||
        [`${issue}/messages`, `${issue}/handoffs`, picture].includes(path) ||
        (sent.pathname === `${issue}/events` && sent.searchParams.get("limit") === "1"))
    );
  };
  expect(
    paths.filter((path) => !allowed(path)),
    `${surface} sent reads absent on main`
  ).toEqual([]);
  if (surface === "ThreadBody") expect(paths).toContain(askRead(data.records.thread));
  if (answers) expect(paths).toContain(askRead(data.records.changed));
  if (decisionsTab || specSurface) expect(paths).toContain(`${issue}/decisions`);
}

interface Control {
  readonly role: string;
  readonly name: string;
}

interface Reading {
  readonly controls: readonly Control[];
  readonly sharedAskBody: boolean;
  readonly testIds: readonly string[];
  readonly words: readonly string[];
}

interface Inventory {
  readonly records: Readonly<Record<string, Reading>>;
  readonly reads: Readonly<Record<string, readonly string[]>>;
}

const ACTIONABLE =
  /^(?:article|button|checkbox|combobox|dialog|form|group|heading|img|link|listbox|menuitem|option|radio|region|searchbox|switch|textbox)$/;
const UUID = /\b[0-9a-f]{8}-(?:[0-9a-f]{4}-){3}[0-9a-f]{12}\b/gi;

function normalize(text: string, data: AskTodaySeed): string {
  return text
    .replaceAll(data.key, "<issue>")
    .replace(UUID, "<id>")
    .replace(/\b\d{4}-\d{2}-\d{2}\b/g, "<date>")
    .replace(/\s+/g, " ")
    .trim();
}

async function inspect(root: Locator, data: AskTodaySeed): Promise<Reading> {
  const snapshot = await root.ariaSnapshot();
  const controls = snapshot.split("\n").flatMap((line) => {
    const match = /^\s*- (\w+)(?: "((?:[^"\\]|\\.)*)")?(?:\s|:|$)/.exec(line);
    if (match === null || !ACTIONABLE.test(match[1] ?? "")) return [];
    return [{ name: normalize(match[2] ?? "", data), role: match[1] ?? "" }];
  });
  const drawn = await root.evaluate((element, bodySelector) => {
    // Keep the approval phrase together, including the date and actor in child elements.
    const approval = [...element.querySelectorAll("p")].find((line) =>
      line.textContent?.startsWith("Approved at version ")
    );
    const text: string[] = [];
    const walker = document.createTreeWalker(element, NodeFilter.SHOW_TEXT);
    for (let node = walker.nextNode(); node !== null; node = walker.nextNode()) {
      const owner = node.parentElement;
      if (
        !approval?.contains(owner) &&
        owner !== null &&
        owner.closest("time, [aria-hidden='true'], .sr-only") === null &&
        owner.checkVisibility({ visibilityProperty: true }) &&
        node.textContent?.trim()
      ) {
        text.push(node.textContent);
      }
    }
    if (approval?.textContent) text.push(approval.textContent);
    return {
      ids: [element, ...element.querySelectorAll("[data-testid]")]
        .map((node) => node.getAttribute("data-testid"))
        .filter((id): id is string => id !== null),
      sharedAskBody: element.matches(bodySelector) || element.querySelector(bodySelector) !== null,
      text,
    };
  }, BODY);
  return {
    controls,
    sharedAskBody: drawn.sharedAskBody,
    testIds: drawn.ids.map((id) => normalize(id, data)),
    words: drawn.text.map((line) => normalize(line, data)),
  };
}

/** Read a control after the card has settled, including the legacy note disclosure when present. */
async function inspectAsk(root: Locator, data: AskTodaySeed): Promise<Reading> {
  const first = await inspect(root, data);
  if (first.controls.some(({ name, role }) => role === "button" && name === DISCLOSURE)) {
    await button(root, DISCLOSURE).click();
    await root.locator(ANSWER).waitFor({ state: "visible" });
    const opened = await inspect(root, data);
    return {
      controls: [...first.controls, ...opened.controls],
      sharedAskBody: opened.sharedAskBody,
      testIds: [...new Set([...first.testIds, ...opened.testIds])],
      words: [...new Set([...first.words, ...opened.words])],
    };
  }
  return first;
}

async function collectInventory(page: Page, data: AskTodaySeed): Promise<Inventory> {
  const records: Record<string, Reading> = {};
  const reads: Record<string, string[]> = {};
  const traffic = watchApiRequests(page);
  const seeded = {
    ...data.records,
    agentsTurn: data.asks.agentsTurn,
    answered: data.asks.answered,
    approval: data.asks.approval.id,
    byOption: data.asks.byOption.id,
    handedOn: data.asks.handedOn,
    inWords: data.asks.inWords,
    replied: data.asks.replied,
  };
  const all = Object.entries(seeded);
  const inboxIds = new Set((await getInbox()).map((row) => row.id));
  const open = all.filter(([, id]) => inboxIds.has(id));
  const capture = async (part: string, root: Locator, composer = false) => {
    await root.waitFor({ state: "visible" });
    records[part] = composer ? await inspectAsk(root, data) : await inspect(root, data);
  };
  const measure = async (surface: string) => {
    await page.waitForFunction(() => document.readyState === "complete");
    reads[surface] = getPaths(traffic).map((path) => normalize(path, data));
    traffic.reset();
  };

  await wide(page);
  await page.goto(inboxAt(seeded.byOption));
  await selectedRow(page).waitFor({ state: "visible" });
  for (const [part, id] of open) {
    await capture(`Inbox list/${part}`, inboxRow(page, id));
    await inboxRow(page, id).locator("time").first().click();
    await selectedRow(page).and(inboxRow(page, id)).waitFor({ state: "visible" });
    if (part === "approval") {
      await inboxPane(page)
        .getByRole("region", { name: `Document version ${data.asks.approval.version}` })
        .waitFor({ state: "visible" });
      await capture("reading pane/approval", inboxPane(page));
    } else {
      const root = ask(inboxPane(page), id);
      await capture(`reading pane/${part}`, root, true);
    }
  }
  await measure("reading pane");

  await page.goto("/?type=reply");
  await inboxRow(page, replyRowId(seeded.thread)).locator("time").first().click();
  const thread = ask(inboxPane(page).locator('[data-inbox-thread-body="ask"]'), seeded.thread);
  await thread.getByText(ASK_TODAY.reply, { exact: true }).waitFor({ state: "visible" });
  await capture("ThreadBody/thread", thread, true);
  await measure("ThreadBody");

  await narrow(page);
  await page.goto("/?assignee=everyone");
  for (const [part, id] of open)
    await capture(`Inbox cards/${part}`, ask(inboxRow(page, id), id), true);
  await measure("Inbox cards");

  await wide(page);
  await page.goto("/agents");
  await peekTrigger(page).click();
  await drawer(page).locator("[data-inbox-filter]").click();
  await button(drawer(page), "Filter by assignee").click();
  await page.getByRole("button", { name: "Clear assignee" }).click();
  for (const [part, id] of open) {
    await capture(
      `Inbox drawer/${part}`,
      ask(drawer(page).locator(`[data-inbox-row="${id}"]`), id),
      true
    );
  }
  await measure("Inbox drawer");

  await page.goto(conversation(data));
  for (const [part, id] of all) await capture(`Conversation/${part}`, turn(page, id), true);
  const shaped = turn(page, seeded.questionShaped);
  await showQuestionChoice(shaped);
  await button(shaped, "Ask back instead").waitFor({ state: "visible" });
  await capture("Conversation/questionShaped/choice", shaped);
  const decline = turn(page, seeded.byOption);
  await button(decline, "Decline to answer").click();
  const dialog = page.getByRole("dialog", { name: "Decline to answer" });
  await capture("Conversation/byOption/decline", dialog);
  await button(dialog, "Cancel").click();
  await measure("Conversation");

  await page.goto("/answers");
  const answers = (await listMyAnswers()).rows ?? [];
  for (const [part, id] of all) {
    const rows = page.locator(`[data-answer-row="${id}"]`);
    const count = answers.filter((answer) => answer.ask_id === id).length;
    for (let index = 0; index < count; index += 1) {
      await capture(`Answers/${part}/${index + 1}`, rows.nth(index));
    }
  }
  await capture("Answers/changed/edit", await openChangeAnswer(page, seeded.changed), true);
  await measure("Answers");

  await page.goto(data.blockPath);
  const frame = block(documentEditor(page));
  await capture("document/block", frame, true);
  await measure("document");

  await page.goto(spec(data));
  for (const [part, id] of open) await capture(`margin/${part}`, ask(margin(page), id), true);
  for (const entry of data.decisionCases.filter(({ case: part }) => part !== "replaced")) {
    const roots = decisionRoots(page, entry, true);
    for (const [index] of entry.ties.entries()) {
      await capture(`margin/decision/${entry.case}/${index + 1}`, roots.nth(index));
    }
  }
  await measure("margin");

  await navigateInApp(page, `/issues/${data.key}/decisions`);
  for (const entry of data.decisionCases) {
    const root = decisionRoots(page, entry, false);
    await capture(`Decisions/${entry.case}`, root);
    if (entry.section === "Waiting for your review") {
      await button(root, "Change").click();
      const form = root.getByRole("form", { name: `Change ${entry.heading}` });
      await capture(`Decisions/${entry.case}/change`, form);
      await button(form, "Cancel").click();
    }
  }
  await measure("Decisions");
  return { reads, records };
}

const OLD_DECLINE =
  "The question closes with no answer. Dispatch tells the agent that asked to decide without one.";
const NEW_DECLINE =
  "The question closes with no answer, and the agent that asked decides without one";

function compareInventory(before: Inventory, after: Inventory): void {
  const losses: string[] = [];
  for (const [part, old] of Object.entries(before.records)) {
    const now = after.records[part];
    if (now === undefined) {
      losses.push(`${part}: missing record`);
      continue;
    }
    const isDecisionTab = part.startsWith("Decisions/");
    const sharedAskBody =
      now.sharedAskBody ||
      (part === "Conversation/byOption/decline" &&
        after.records["Conversation/byOption"]?.sharedAskBody === true);
    const dropped = (name: string) =>
      name === DISCLOSURE ||
      (isDecisionTab &&
        (name === "Shaped in the spec:" || /^Block [^ ]+$/.test(name) || name === "Send"));
    for (const { name, role } of old.controls) {
      if (dropped(name)) continue;
      const newName =
        sharedAskBody && name === "Question-shaped answer"
          ? "Your answer reads as a question"
          : name;
      if (!now.controls.some((control) => control.role === role && control.name === newName)) {
        losses.push(`${part}: ${role} ${JSON.stringify(newName)}`);
      }
    }
    for (const id of old.testIds) {
      if (!now.testIds.includes(id)) losses.push(`${part}: test id ${id}`);
    }
    const text = now.words.join(" ");
    for (const words of old.words) {
      if (dropped(words) || words === "·") continue;
      const revised = sharedAskBody
        ? words === OLD_DECLINE
          ? NEW_DECLINE
          : words
              .replace(/^Text changed\.$/, "Text changed")
              .replace(/^(Approved at version .*)\.$/, "$1")
        : words;
      if (revised !== words ? !now.words.includes(revised) : !text.includes(words)) {
        losses.push(`${part}: ${JSON.stringify(revised)}`);
      }
    }
  }
  expect(losses, "controls and words lost from today's surfaces").toEqual([]);
}

// SLOW(>=2s): record every surface and its disclosed controls on the real issue seed.
test("the inventory preserves each surface's words and controls", async ({ page }, testInfo) => {
  test.skip(!inventoryMode, "Set ASK_INVENTORY_FILE to record or compare the inventory");
  test.setTimeout(180_000);
  if (!inventoryFile) throw new Error("ASK_INVENTORY=record requires ASK_INVENTORY_FILE");
  const data = await seed();
  const inventory = await collectInventory(page, data);
  const json = `${JSON.stringify(inventory, null, 2)}\n`;
  await testInfo.attach("ask inventory", { body: json, contentType: "application/json" });
  if (recording) await writeFile(inventoryFile, json, "utf8");
  else compareInventory(JSON.parse(await readFile(inventoryFile, "utf8")), inventory);
});
