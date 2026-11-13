import type { IssueDetails } from "../web/src/api/types";
import { NODE_TYPE_NAMES } from "../web/src/features/issue/page-words";

import {
  createComment,
  createIssue,
  createIssueArtifact,
  createProject,
  editArtifact,
  getArtifactText,
  getIssue,
  patchIssue,
} from "./api";
import { expectTextOnlyDecisions, seedDecisionBody } from "./decision-bodies";
import { documentEditor, indexedBlockAsk, openWithCaret } from "./editor";
import { type Browser, type BrowserContext, expect, type Locator, type Page, test } from "./test";
import { asUser } from "./users";

// A typed block in a text body (a comment, an ask, a message) is drawn as a document draws it: a
// callout carries its header, the block's name with its kind badge and its title on one row, and
// a decision (`:::ask`) block is the decision card, its urgency pill and "Decision" in the header
// and its options as the card's rows, with the source list folded away. A decision block is no
// shape the API takes in a body: it refuses such a body with 400 DECISION_BLOCK_IN_BODY, so every
// row below that needs one seeds the body straight into `comments` (`e2e/decision-bodies.ts`).
// The card a body draws differs from a document's in one thing: it carries the line saying the
// question is text and must be opened as an ask, and offers nothing to answer it with.

const agent = {
  actor: { id: "e2e-text-blocks-agent", kind: "session" as const },
  as: "agent" as const,
};

/** Opens the comment's thread, where its body is drawn whole (a collapsed turn shows a two-line
 * preview), and returns the turn that holds it. */
async function openCommentThread(page: Page, commentId: string): Promise<Locator> {
  const turn = page
    .getByRole("list", { name: "Conversation turns" })
    .locator(`:scope > li[data-turn="comment:${commentId}"]`);
  await turn.getByRole("button", { name: "Expand thread" }).click();
  await expect(turn.getByRole("button", { name: "Collapse thread" })).toBeVisible();
  return turn;
}

test("a comment's callout shows the document's header with its kind and title", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Callout in a comment" });
  const comment = await createComment(
    issue.key,
    { body: ':::callout{kind="warning" title="Read this"}\nBody text.\n:::\n' },
    agent
  );
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    const thread = await openCommentThread(page, comment.id);
    const callout = thread.locator('section[data-proof-block-type="callout"]');
    await expect(
      callout.locator("[data-proof-block-content]").getByText("Body text.", { exact: true })
    ).toBeVisible();

    const header = callout.locator("[data-proof-block-summary]");
    await expect(header).toBeVisible();
    // The block type is still `callout`; what the header draws is the page's word for it.
    await expect(callout.locator("[data-proof-block-name]")).toHaveText(NODE_TYPE_NAMES.callout);

    // The kind reads as a badge: a filled pill, not bare text.
    const badge = callout.locator("[data-proof-block-badge]");
    await expect(badge).toBeVisible();
    await expect(badge).toHaveText("warning");
    expect(await badge.evaluate((element) => getComputedStyle(element).backgroundColor)).not.toBe(
      "rgba(0, 0, 0, 0)"
    );

    const title = callout.locator("[data-proof-block-title]");
    await expect(title).toHaveText("Read this");
    await expect(title).toBeVisible();
    expect((await title.boundingBox())?.width).toBeGreaterThan(0);

    // The header reads as a label rather than a stack of prose lines: one row.
    expect((await header.boundingBox())?.height).toBeLessThanOrEqual(40);

    // A body that holds no decision block is told nothing about opening an ask.
    await expect(thread.locator("[data-dispatch-ask-text-only]")).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

// @pr: The typed-block family's one row on the lane: a comment's ask block has to draw as the decision card.
test("a comment's ask block shows the decision card @pr", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Decision in a comment" });
  // The server refuses a body holding a decision block (400 DECISION_BLOCK_IN_BODY), so the
  // comment is seeded straight into `comments` (`e2e/decision-bodies.ts`).
  const commentId = await seedDecisionBody({
    author: agent.actor,
    body: ':::ask{urgency="high"}\nWhich one?\n\n- A: first\n- B: second\n:::\n',
    issueKey: issue.key,
  });
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    const thread = await openCommentThread(page, commentId);
    const decision = thread.locator('section[data-proof-block-type="ask"]');
    await expect(decision.getByText("Which one?", { exact: true })).toBeVisible();

    // The header names the block a decision and its urgency.
    const header = decision.locator("[data-dispatch-ask-header]");
    await expect(header).toBeVisible();
    await expect(header).toContainText("Decision");
    await expect(header.getByText("Decision", { exact: true })).toBeVisible();
    const pill = decision.locator('[data-dispatch-ask-pill="high"]');
    await expect(pill).toBeVisible();
    await expect(pill).toHaveText("High");

    // The options are the card's rows, label and description each.
    const record = decision.locator("[data-dispatch-ask-record]");
    await expect(record).toBeVisible();
    const rows = record.locator('ul[aria-label="Options"] > li');
    await expect(rows).toHaveCount(2);
    for (const [index, label, description] of [
      [0, "A", "first"],
      [1, "B", "second"],
    ] as const) {
      const row = rows.nth(index);
      await expect(row).toBeVisible();
      await expect(row).toContainText(label);
      await expect(row).toContainText(description);
    }

    // The source "- Option: description" list is there, folded behind the rows.
    const source = decision.locator("[data-proof-block-content] > ul");
    await expect(source).toHaveCount(1);
    await expect(source).toBeHidden();

    // The card's urgency accent: a 4px left border.
    await expect(decision).toHaveCSS("border-left-width", "4px");

    // The one card the thread drew says the question is text, and offers nothing to answer it
    // with.
    await expectTextOnlyDecisions(thread, 1);
  } finally {
    await alice.close();
  }
});

/** Files an issue titled `title` whose document holds an open decision, replies `body` under that
 * decision, and opens the issue as alice. The reply is seeded straight into `comments`
 * (`e2e/decision-bodies.ts`): every body here holds a decision block, which the server refuses.
 * Returns alice's context, for the caller to close, the document's own decision block, and the
 * reply's thread inside that decision's card. */
async function replyUnderDocumentDecision(
  browser: Browser,
  title: string,
  body: string
): Promise<{ alice: BrowserContext; decision: Locator; thread: Locator }> {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "## Specification\n\nContext.\n",
    title,
  });
  await editArtifact(
    issue.primary_artifact_id,
    {
      ops: [
        {
          after: "end",
          markdown:
            ':::ask{#storage urgency="high" multiple="false" state="open"}\nWhich engine?\n\n- Postgres: reuse\n- SQLite: one file\n:::\n',
          op: "insert",
        },
      ],
    },
    { as: "agent" }
  );
  let storage: NonNullable<IssueDetails["open_asks"]>[number] | undefined;
  await expect
    .poll(async () => {
      [storage] = (await getIssue(issue.key)).open_asks ?? [];
      return storage;
    })
    .toBeDefined();
  if (storage === undefined) {
    throw new Error("the storage decision was not indexed");
  }
  await seedDecisionBody({ askId: storage.id, author: agent.actor, body, issueKey: issue.key });
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}`);
    const decision = documentEditor(page).locator('[data-dispatch-ask-block="storage"]');
    return { alice, decision, thread: decision.getByTestId(`thread-${storage.id}`) };
  } catch (error) {
    await alice.close();
    throw error;
  }
}

// A reply under a document's decision is a text body drawn inside that decision's card. The
// decision folds its own source option list once its card's rows show the options; that fold
// must not reach the typed blocks a reply holds: a callout's bullet list is what its author
// wrote, and a malformed decision in the reply has no rows, so its list is the only place its
// options show.
test("a reply under a document's decision keeps the lists of the typed blocks it holds", async ({
  browser,
}) => {
  const { alice, decision, thread } = await replyUnderDocumentDecision(
    browser,
    "Reply under a decision",
    ':::callout{kind="warning" title="Steps first"}\n- step one\n- step two\n:::\n\n' +
      ":::ask\nBroken inner?\n\n- Fine: ok\n- : blank\n:::\n"
  );

  try {
    await expect(thread.getByText("Broken inner?", { exact: true })).toBeVisible();

    // The callout's own list shows.
    await expect(thread.getByText("step one", { exact: true })).toBeVisible();
    await expect(thread.getByText("step two", { exact: true })).toBeVisible();
    // The malformed decision's source list shows: it has no rows to stand in for it.
    await expect(thread.getByText("Fine: ok", { exact: true })).toBeVisible();

    // The one decision the reply holds says the question is text, and offers nothing to answer
    // it with — while the document's own decision around it says no such thing: every line of
    // that kind inside the document's card is the reply's.
    await expectTextOnlyDecisions(thread, 1);
    await expect(decision.locator("[data-dispatch-ask-text-only]")).toHaveCount(1);
    const hosted = decision.locator("article[data-testid^=ask-]");
    await expect(hosted.getByRole("radio")).toHaveCount(3);
    await hosted.getByRole("radio", { name: "Postgres reuse" }).check();
    await expect(hosted.getByRole("button", { exact: true, name: "Answer" })).toBeEnabled();
  } finally {
    await alice.close();
  }
});

// A decision drawn inside a reply sits in a card of its own, so its question and its last line
// sit flush with the card's edges as a document's decision does. The reply sits in the footer of
// the document's decision, whose `prose-sm` spacing for the reply's paragraphs and lists must not
// reach the nested card's first and last lines.
test("a decision in a reply under a document's decision sits flush in its card", async ({
  browser,
}) => {
  const { alice, thread } = await replyUnderDocumentDecision(
    browser,
    "Nested decision in a reply",
    ":::ask\nNested question?\n\n- A: a\n- B: b\n:::\n\n" +
      ":::ask\nBroken nested?\n\n- Fine: ok\n- : blank\n:::\n"
  );

  try {
    const nested = thread.locator('section[data-proof-block-type="ask"]');

    // The well-formed nested decision's question sits flush under its card's header.
    const wellFormedQuestion = nested
      .filter({ hasText: "Nested question?" })
      .locator(":scope > [data-proof-block-content] > :first-child");
    await expect(wellFormedQuestion).toHaveText("Nested question?");
    await expect(wellFormedQuestion).toBeVisible();
    await expect(wellFormedQuestion).toHaveCSS("margin-top", "0px");

    // The malformed one shows its source list; question and list sit flush in the card.
    const brokenHole = nested
      .filter({ hasText: "Broken nested?" })
      .locator(":scope > [data-proof-block-content]");
    const brokenQuestion = brokenHole.locator(":scope > :first-child");
    await expect(brokenQuestion).toHaveText("Broken nested?");
    await expect(brokenQuestion).toBeVisible();
    await expect(brokenQuestion).toHaveCSS("margin-top", "0px");
    const brokenList = brokenHole.locator(":scope > :last-child");
    await expect(brokenList).toHaveJSProperty("tagName", "UL");
    await expect(brokenList).toBeVisible();
    await expect(brokenList).toHaveCSS("margin-bottom", "0px");
  } finally {
    await alice.close();
  }
});

// A malformed decision shows its source list, the only place its options show. A blank item in
// that list is no option at all, so it shows no bullet where nothing is edited: a text body,
// whose tight list items carry no paragraph, hides it, and so does a read-only render of a
// document. In an editable document the blank item is where a human types the option, so it shows.
test("a malformed decision's blank option shows no bullet in a body or a read-only document", async ({
  browser,
}) => {
  const source = ':::ask{#blank urgency="high"}\nWhich one?\n\n- A: first\n-\n:::\n';
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "## Specification\n\nContext.\n",
    title: "Blank option",
  });
  // Edits to the issue's own document refuse a malformed decision, so it is filed as one of the
  // issue's documents.
  const created = await createIssueArtifact(
    issue.key,
    { content: source, name: "blank.md" },
    { as: "agent" }
  );
  // The server refuses this body too (400 DECISION_BLOCK_IN_BODY): what it holds is a decision
  // block, malformed or not, so this comment is seeded the same way.
  const commentId = await seedDecisionBody({
    author: agent.actor,
    body: source,
    issueKey: issue.key,
  });
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    const thread = await openCommentThread(page, commentId);
    const bodyDecision = thread.locator('section[data-proof-block-type="ask"]');
    await expect(bodyDecision).toHaveAttribute("data-dispatch-ask-malformed", "true");
    await expect(
      bodyDecision.locator("[data-proof-block-content] li").filter({ hasText: "A: first" })
    ).toBeVisible();
    const bodyItems = bodyDecision.locator("[data-proof-block-content] > ul > li");
    await expect(bodyItems).toHaveCount(2);
    await expect(bodyItems.nth(1)).toHaveCSS("display", "none");

    // A malformed decision in a body says the question is text where it drew nothing before.
    await expectTextOnlyDecisions(thread, 1);

    await page.goto(`/issues/${issue.key}/artifacts/${created.artifact.slug}`);
    const docDecision = documentEditor(page).locator('[data-dispatch-ask-block="blank"]');
    await expect(docDecision).toHaveAttribute("data-dispatch-ask-malformed", "true");
    const docItems = docDecision.locator("[data-proof-block-content] > ul > li");
    await expect(docItems).toHaveCount(2);
    await expect(docItems.filter({ hasText: "A: first" })).toBeVisible();
    const docBlank = docItems.nth(1);
    await expect(docBlank).toHaveText("");
    await expect(docBlank).toHaveCSS("display", "list-item");
    // A document surface says nothing about opening an ask: the block is the document's own.
    await expect(docDecision.locator("[data-dispatch-ask-text-only]")).toHaveCount(0);

    await page.goto(`/issues/${issue.key}/artifacts/${created.artifact.slug}?v=1`);
    const versionView = page.getByTestId("version-view");
    await expect(versionView.locator('.dispatch-doc[data-read-only="true"]')).toBeVisible();
    const readOnlyDecision = versionView.locator('[data-dispatch-ask-block="blank"]');
    await expect(readOnlyDecision).toHaveAttribute("data-dispatch-ask-malformed", "true");
    const readOnlyItems = readOnlyDecision.locator("[data-proof-block-content] > ul > li");
    await expect(readOnlyItems).toHaveCount(2);
    await expect(readOnlyItems.filter({ hasText: "A: first" })).toBeVisible();
    await expect(readOnlyItems.nth(1)).toHaveText("");
    await expect(readOnlyItems.nth(1)).toHaveCSS("display", "none");
    // A saved version draws the same record with nothing to answer it with, and no such line.
    const answer = readOnlyDecision.getByRole("button", { exact: true, name: "Answer" });
    await expect(readOnlyDecision.getByRole("radio")).toHaveCount(0);
    await expect(answer).toHaveCount(0);
    await expect(readOnlyDecision.locator("[data-dispatch-ask-text-only]")).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

// The line belongs to a body alone: a decision block a document holds is answered where it
// stands, so no document surface draws it. The three document surfaces this tree has are the
// live editor, a saved version read at `?v=1` and a closed issue's read-only document, and all
// three are read below. A surface added later says which it is: `AskBlockCard`'s `inBody`
// carries no default, so a card drawn for a document is written with `inBody={false}` and can
// never take the line by forgetting to.
test("a document's own decision block draws no line about opening an ask, live, saved or closed", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec:
      "## Specification\n\nContext.\n\n" +
      ':::ask{#storage urgency="high" multiple="false" state="open"}\n' +
      "Which engine?\n\n- Postgres: reuse\n- SQLite: one file\n:::\n",
    title: "Decision in a document",
  });
  await indexedBlockAsk(issue.key, "storage");
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}`);
    const live = documentEditor(page).locator('[data-dispatch-ask-block="storage"]');
    const hosted = live.locator("article[data-testid^=ask-]");
    await expect(hosted).toBeVisible();

    // The reader answers it where it stands: the options are controls, and picking one arms
    // the Answer button.
    await expect(hosted.getByRole("radio")).toHaveCount(3);
    await hosted.getByRole("radio", { name: "Postgres reuse" }).check();
    await expect(hosted.getByRole("button", { exact: true, name: "Answer" })).toBeEnabled();
    await expect(live.locator("[data-dispatch-ask-text-only]")).toHaveCount(0);

    // The saved version draws the same record, with nothing to answer it with.
    await page.goto(`/issues/${issue.key}/artifacts/spec?v=1`);
    const saved = page.getByTestId("version-view").locator('[data-dispatch-ask-block="storage"]');
    await expect(saved.locator('ul[aria-label="Options"] > li')).toHaveCount(2);
    await expect(saved.getByRole("radio")).toHaveCount(0);
    await expect(saved.getByRole("button", { exact: true, name: "Answer" })).toHaveCount(0);
    await expect(saved.locator("[data-dispatch-ask-text-only]")).toHaveCount(0);

    // A closed issue's document draws the same record, read-only, and no line either.
    await patchIssue(issue.key, { status: "done" }, { login: "bob" });
    await page.goto(`/issues/${issue.key}`);
    await expect(page.getByText("This issue is closed.", { exact: true })).toBeVisible();
    const closed = documentEditor(page).locator('[data-dispatch-ask-block="storage"]');
    await expect(closed.locator('ul[aria-label="Options"] > li')).toHaveCount(2);
    await expect(closed.getByRole("radio")).toHaveCount(0);
    await expect(closed.getByRole("button", { exact: true, name: "Answer" })).toHaveCount(0);
    await expect(closed.locator("[data-dispatch-ask-text-only]")).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

// Typing a decision's first option goes through a blank item: Enter after the question, then
// `- `, leaves an option without a label, so the decision is malformed until its label is typed.
// The blank item holds the caret, so it shows, and the label typed next lands in it, not in the
// question.
test("typing a new option into a decision with no options stores it as a typed option", async ({
  browser,
}) => {
  const { alice, issue, page } = await openWithCaret(
    browser,
    "Typed option",
    "Intro.\n\n:::ask{#a1}\nWhich one?\n:::\n\nAfter.\n",
    "Which one?",
    "end"
  );
  try {
    await page.keyboard.press("Enter");
    await page.keyboard.type("- ");
    await page.keyboard.type("Option A");

    await expect
      .poll(async () => (await getArtifactText(issue.primary_artifact_id)).markdown)
      .toMatch(/\nWhich one\?\n\n- Option A\n:::\n/);
  } finally {
    await alice.close();
  }
});
