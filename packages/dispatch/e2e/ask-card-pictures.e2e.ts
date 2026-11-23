import { existsSync } from "node:fs";

import { setInterests, setLiveSessions } from "./agents";
import {
  answerAsk,
  createAsk,
  createComment,
  createIssue,
  createIssueArtifact,
  createMessage,
  createProject,
  documentBase,
  editArtifact,
  getArtifact,
  getAsk,
  getHeld,
  markThreadRead,
  patchIssue,
  replyToMessageDelivery,
  requestApproval,
  rewriteAsk,
  setAskedOf,
} from "./api";
import { framesTo, REWRITE_DEFINE_MODEL, rewriteText, selectWords } from "./ask-card";
import {
  ANSWERED,
  ANSWERED_QUOTE,
  APPEND_QUOTE,
  APPEND_TYPED,
  AUDIT_ORIGINAL,
  AUDIT_REWRITTEN,
  asSession,
  COLD_START_BLOCK,
  COLD_START_BLOCK_MARKDOWN,
  COLD_START_MARKDOWN,
  COLD_START_ORIGINAL,
  COLD_START_REWRITTEN,
  COMMENT_ORIGINAL,
  COMMENT_QUOTE,
  COMMENT_REWRITTEN,
  type DemoDefinition,
  type DemoSession,
  DRY_RUN_ORIGINAL,
  DRY_RUN_QUOTE,
  DRY_RUN_REWRITTEN,
  FLIP,
  FLIP_AUDIT,
  FLIP_REWRITTEN,
  HANDED_ON,
  HANDOVER_APPROVAL_SUMMARY,
  HANDOVER_MARKDOWN,
  ISSUES,
  LEDGER_APPROVAL_SUMMARY,
  LEDGER_ROLLOUT_VERSIONS,
  liveSession,
  MESSAGE_QUOTE,
  MIRROR,
  ORDER,
  ORDER_ASK,
  ORDER_BROKEN_REWRITE,
  ORDER_MESSAGE,
  PEOPLE,
  PICTURES,
  type Picture,
  PROJECT,
  PROJECT_NAME,
  plainSpec,
  READER,
  REFUSED_LEASE,
  REVIEWER,
  SPEC_MARKDOWN,
  TOOL,
  type Wording,
} from "./ask-card-fixtures";
import { comparePictures } from "./ask-card-pictures-diff";
import { askCardPicturesVariable } from "./configurations";
import { connectedDot, documentEditor, indexedBlockAsk, selectTextIn } from "./editor";
import type { FakeModelRule } from "./fake-model";
import { setModelRules } from "./fake-model-helpers";
import { sql } from "./psql";
import { expect, REVIEWER_MODEL, type ReviewerServer, test } from "./reviewer-server";
import { sqlLiteral } from "./seed";
import type { Locator, Page } from "./test";
import { asUser } from "./users";

/**
 * The ask card's 29 pictures (INFRA-3231's check b5ee93c7, Decision 22 of its plan): the demo's
 * fixture text (`ask-card-fixtures.ts`) seeded into the real stack, with the stand-in model and
 * the fake Envoy in place of the prototype's stub, and each picture the spec's Design names taken
 * again from the build, at the demo's own sizes - 1440x900, and 390x844 for the two phone
 * pictures - each in a fresh context, as the demo's own run took them. Where
 * DISPATCH_E2E_ASK_CARD_DEMO names a directory of the demo's pictures, each is laid beside the
 * demo's of the same screen with its share of changed pixels (`ask-card-pictures-diff.ts`).
 *
 * What the prototype's stub faked, the server does here: a person's Rewrite and Define call the
 * stand-in, the asking agent's answer to a definition is the reply to the side question Dispatch
 * sends it, and the check's rewrites of an ask, a comment and an issue are the reviewer's, through
 * a server of its own with the reviewer's settings (`e2e/reviewer-server.ts`). The decision
 * block's rewrite is a person's Rewrite of it, which writes the same new version of its document.
 * Each age is the demo's, written into the rows once they exist.
 *
 * The pictures are a rendering: drawn in Inter, the app's font, as the demo's were. A machine
 * without Inter draws them in a fallback, so a run meant for comparison points Chromium's
 * fontconfig at Inter's files (`FONTCONFIG_FILE`), as `packages/dispatch/README.md` says.
 *
 * At the commit before the change this file fails: once the seeding has written what that build
 * takes, it waits for the flip audit's card to draw its split button, which that build does not.
 */

test.skip(
  process.env[askCardPicturesVariable] !== "1",
  "the ask card's pictures run only where the command names their project: " +
    "`bun run e2e -- --project=ask-card-pictures`"
);
test.skip(
  !!process.env.PLAYWRIGHT_BASE_URL,
  "a deployed run starts no server of its own, so it has no reviewer's server to seed through"
);

const DESKTOP = { height: 900, width: 1440 };
const PHONE = { height: 844, width: 390 };

/** What the seeding made, by what the pictures name. */
interface Seeded {
  readonly flip: string;
  readonly order: string;
  readonly approval: string;
  readonly checked: string;
  readonly coldStart: string;
  readonly handedOn: string;
  readonly comment: string;
  /** The rollout architect's note on PANE-3, which Quote reply's picture 28 quotes. */
  readonly message: string;
  /** When the seeding ended, in milliseconds: the moment every age was written against. */
  readonly seededAt: number;
}

/** A wording's options as an ask's write takes them. */
function optionsOf(wording: Wording) {
  return wording.options.map((option) => ({
    description: option.description ?? "",
    label: option.label,
  }));
}

/** The stand-in's definition of `words`, as the words of the request name them. */
function definitionRule(words: string, definition: DemoDefinition): FakeModelRule {
  return {
    contains: `<words>\n- ${words}\n</words>`,
    model: REWRITE_DEFINE_MODEL,
    reply: {
      text: JSON.stringify({
        definitions: [
          {
            links: [`dispatch://PANE-5/spec#b-${definition.requirement}`],
            meaning: definition.meaning,
            words,
          },
        ],
      }),
    },
  };
}

/** The stand-in's rewrite of the question that holds `question`, after `delayMs`. */
function rewriteRuleOf(question: string, rewrite: Wording, delayMs = 0): FakeModelRule {
  return {
    contains: question,
    model: REWRITE_DEFINE_MODEL,
    reply: { delayMs, text: rewriteText(rewrite.question, optionsOf(rewrite)) },
  };
}

/** The reviewer's answer to the text that holds `holds`: one finding of `quote`, and `rewrite`. */
function reviewRuleOf(holds: string, quote: string, rewrite: object): FakeModelRule {
  return {
    contains: holds,
    model: REVIEWER_MODEL,
    reply: {
      text: JSON.stringify({
        findings: [
          {
            fix: "Say it in words the reader knows.",
            quote,
            rule_set: "phrases",
            why: "The reader cannot tell what it names.",
          },
        ],
        rewrite,
      }),
    },
  };
}

/** Every answer the stand-in gives the run: Define's first, since a definition's request holds
 *  the question it was made in; then a person's Rewrites; then the reviewer's. */
function modelRules(): FakeModelRule[] {
  return [
    definitionRule("flip audit", FLIP_AUDIT),
    definitionRule("Flip audit", FLIP_AUDIT),
    definitionRule("refused lease", REFUSED_LEASE),
    // The rewrite runs long enough for a picture of it running.
    rewriteRuleOf(FLIP.question, FLIP_REWRITTEN, 6_000),
    rewriteRuleOf(ORDER_ASK.question, ORDER_BROKEN_REWRITE),
    rewriteRuleOf(COLD_START_ORIGINAL.question, COLD_START_REWRITTEN),
    reviewRuleOf(DRY_RUN_ORIGINAL.question, "R3 flip audit", {
      body: DRY_RUN_REWRITTEN.question,
      multiple: false,
      options: optionsOf(DRY_RUN_REWRITTEN),
    }),
    reviewRuleOf(COMMENT_ORIGINAL, "B per the train", { body: COMMENT_REWRITTEN }),
    reviewRuleOf(AUDIT_ORIGINAL.title, "hb rows", {
      body: AUDIT_REWRITTEN.spec,
      title: AUDIT_REWRITTEN.title,
    }),
  ];
}

/** Waits until the reviewer has posted the held text `id`. */
async function posted(id: string): Promise<void> {
  await expect
    .poll(async () => (await getHeld(id)).state, { intervals: [250, 500, 1_000], timeout: 60_000 })
    .toBe("posted");
}

/** Sets the row of `table` whose `key` is `id` to have been written `minutes` ago. */
async function writtenAgo(table: string, key: string, id: string, minutes: number): Promise<void> {
  await sql(
    `UPDATE ${table} SET created_at = now() - interval '${minutes} minutes' WHERE ${key} = ${sqlLiteral(id)}`
  );
}

/** The sessions, live in the fake Envoy; `sideQuestions` takes the one that answers a definition's
 *  side question, the lease-mirror implementer. */
async function liveSessions(sideQuestions: DemoSession | undefined): Promise<void> {
  await setLiveSessions(
    [MIRROR, TOOL, ORDER, REVIEWER].map((session) =>
      liveSession(session, session === sideQuestions)
    )
  );
}

/** Seeds the demo's world: PANE's issues and documents, its questions, the reviewer's rewrites of
 *  an ask, a comment and an issue, the decision block a person rewrote, and the question dana
 *  handed on, each with the demo's age; then the sessions live. Nothing is live while it seeds, so
 *  no Rewrite waits on a side question and every held text posts once its review ends.
 *
 *  The seeding writes everything the build before the change takes first, then calls
 *  `splitButton` with the flip audit's id, before the first write that build has no route for: so
 *  at the commit before the change the row fails where it waits for the split button. */
async function seedDemo(
  reviewer: ReviewerServer,
  splitButton: (flip: string) => Promise<void>
): Promise<Seeded> {
  await sql(
    `INSERT INTO people (email) VALUES ${PEOPLE.map((login) => `(${sqlLiteral(login)})`).join(", ")} ON CONFLICT DO NOTHING`
  );
  await setLiveSessions([]);
  await setModelRules(modelRules());
  await createProject({ default_assignee: READER, key: PROJECT, name: PROJECT_NAME });

  const specs: Record<string, string> = {
    "PANE-1": LEDGER_ROLLOUT_VERSIONS[0]?.markdown ?? "",
    "PANE-2": plainSpec("Lease tool: dry-run flag"),
    "PANE-3": plainSpec("Rollout order"),
    "PANE-4": HANDOVER_MARKDOWN,
    "PANE-5": SPEC_MARKDOWN,
    "PANE-6": COLD_START_MARKDOWN,
  };
  const specOf = new Map<string, string>();
  for (const issue of ISSUES) {
    const created = await createIssue({
      assignee: READER,
      force: true,
      priority: issue.priority,
      project: PROJECT,
      spec: specs[issue.key] ?? "",
      title: issue.title,
    });
    expect(created.key).toBe(issue.key);
    await patchIssue(created.key, { status: issue.status });
    specOf.set(issue.key, created.primary_artifact_id);
  }
  const spec = (key: string) => specOf.get(key) ?? "";

  // PANE-1's spec grows to its fourth version, the one its approval request is for.
  const ledgerName = (await getArtifact(spec("PANE-1"))).name;
  for (const version of LEDGER_ROLLOUT_VERSIONS.slice(1)) {
    await createIssueArtifact("PANE-1", {
      content: version.markdown,
      name: ledgerName,
      // Each upload replaces the spec, so it names the version it replaces (main's #3000).
      precondition: await documentBase(spec("PANE-1")),
      summary: version.summary,
    });
  }
  await expect
    .poll(async () => ((await getArtifact(spec("PANE-1"))).versions ?? []).length)
    .toBe(LEDGER_ROLLOUT_VERSIONS.length);

  const order = await createAsk(
    "PANE-3",
    { options: optionsOf(ORDER_ASK), question: ORDER_ASK.question, urgency: "med" },
    asSession(ORDER)
  );
  const approval = await requestApproval(
    spec("PANE-1"),
    { summary: LEDGER_APPROVAL_SUMMARY },
    asSession(ORDER)
  );
  const handoverApproval = await requestApproval(
    spec("PANE-4"),
    { summary: HANDOVER_APPROVAL_SUMMARY },
    asSession(ORDER)
  );
  const flip = await createAsk(
    "PANE-5",
    { options: optionsOf(FLIP), question: FLIP.question, urgency: "high" },
    asSession(MIRROR)
  );
  const handedOn = await createAsk(
    "PANE-4",
    { options: optionsOf(HANDED_ON), question: HANDED_ON.question, urgency: "med" },
    asSession(ORDER)
  );
  await setAskedOf(handedOn.id, "bruno", { login: "dana" });
  const message = await createMessage("PANE-3", { body: ORDER_MESSAGE.body }, asSession(ORDER));

  // The decision block of PANE-6's spec, as its agent wrote it, and a person's Rewrite of it,
  // which writes the new version of the document the check's rewrite would.
  await editArtifact(
    spec("PANE-6"),
    { ops: [{ after: "end", markdown: COLD_START_BLOCK_MARKDOWN, op: "insert" }] },
    asSession(MIRROR)
  );
  const coldStart = (await indexedBlockAsk("PANE-6", COLD_START_BLOCK)).id;
  await splitButton(flip.id);
  await rewriteAsk(coldStart, { login: READER });
  await expect
    .poll(async () => (await getAsk(coldStart)).ask.rewrite?.state, { timeout: 60_000 })
    .toBe("rewritten");

  // The check's rewrites: an ask, a comment and a new issue, each held by the reviewer's server
  // and posted rewritten.
  const atReviewerServer = (session: DemoSession) => ({
    ...asSession(session),
    baseUrl: reviewer.url,
  });
  const checked = await createAsk(
    "PANE-2",
    { options: optionsOf(DRY_RUN_ORIGINAL), question: DRY_RUN_ORIGINAL.question, urgency: "med" },
    atReviewerServer(TOOL)
  );
  const comment = await createComment(
    "PANE-5",
    { body: COMMENT_ORIGINAL },
    atReviewerServer(MIRROR)
  );
  const audit = await createIssue(
    { project: PROJECT, spec: AUDIT_ORIGINAL.spec, title: AUDIT_ORIGINAL.title },
    atReviewerServer(TOOL)
  );
  const auditKey = "key" in audit && typeof audit.key === "string" ? audit.key : "";
  expect(auditKey).toBe("PANE-7");
  for (const id of [checked.id, comment.id, auditKey]) await posted(id);

  // Each row's age, as the demo gave it.
  const hours = (count: number) => count * 60;
  for (const [id, minutes] of [
    [order.id, hours(23)],
    [approval.ask?.id ?? "", hours(23)],
    [handoverApproval.ask?.id ?? "", hours(23)],
    [flip.id, FLIP.minutesAgo],
    [handedOn.id, hours(5)],
    [coldStart, hours(23)],
    [checked.id, 7],
  ] as const) {
    await writtenAgo("asks", "id", id, minutes);
  }
  await sql(
    `UPDATE asks SET asked_of_at = now() - interval '20 minutes' WHERE id = ${sqlLiteral(handedOn.id)}`
  );
  await writtenAgo("comments", "id", comment.id, 40);
  await writtenAgo("messages", "id", message.id, ORDER_MESSAGE.minutesAgo);
  for (const issue of ISSUES) await writtenAgo("issues", "key", issue.key, issue.openedMinutesAgo);
  await writtenAgo("issues", "key", auditKey, 12);
  // PANE-1's spec versions, a day apart, the last one yesterday.
  await sql(
    `UPDATE artifact_versions SET created_at = now() - interval '1 day' * (${LEDGER_ROLLOUT_VERSIONS.length + 1} - number) WHERE artifact_id = ${sqlLiteral(spec("PANE-1"))}::uuid`
  );

  await liveSessions(undefined);
  await setInterests([
    { session_id: REVIEWER.id, topics: ["notifications.dispatch.issue.PANE-5.>"] },
  ]);
  return {
    approval: approval.ask?.id ?? "",
    checked: checked.id,
    coldStart,
    comment: comment.id,
    flip: flip.id,
    handedOn: handedOn.id,
    message: message.id,
    order: order.id,
    seededAt: Date.now(),
  };
}

const cardOf = (page: Page | Locator, id: string) => page.locator(`[data-ask-card="${id}"]`);
const BLOCK = '[data-proof-block-type="ask"]';

/** Off any control, so no hover state or tooltip stands in the picture. */
const rest = (page: Page) => page.mouse.move(1, 1);

/** Opens `path` and waits until `ready` is drawn, then for the drawing to settle. */
async function open(page: Page, path: string, ready: Locator): Promise<void> {
  await page.goto(path);
  await expect(ready.first()).toBeVisible({ timeout: 30_000 });
  // The demo's pictures were taken 900 ms after their state was drawn, which lets fonts, late
  // reads and transitions settle; the build's are taken the same way.
  await page.waitForTimeout(900);
}

/** The Inbox with one question's card in the reading pane, its split button drawn. The card is
 *  waited for by its own marker, which the build before the change draws too, so there the wait
 *  that fails is the split button's. */
async function inbox(page: Page, id: string, query = ""): Promise<Locator> {
  const card = cardOf(page.getByTestId("inbox-pane"), id);
  await open(page, `/?${query}item=${id}`, card);
  await expect(card.locator("[data-ask-menu-button]")).toBeVisible();
  return card;
}

/** Presses `key` on the card in hand and waits for its badge to read `state`. */
async function pressOnCard(page: Page, card: Locator, key: string, state: string): Promise<void> {
  await page.keyboard.press(key);
  await expect(card.locator(`[data-ask-rewrite="${state}"]`)).toBeVisible({ timeout: 30_000 });
}

/** The menu of the card's split button, open. */
async function openMenu(page: Page, card: Locator): Promise<void> {
  await card.locator("[data-ask-menu-button]").click();
  await expect(page.locator("[data-ask-menu]")).toBeVisible();
  // The chevron turns as the menu opens; the picture waits for it to stand pointing up.
  await page.waitForTimeout(400);
}

/** Selects `words` in the card's question and presses `d`, answering the definition's id. */
async function define(page: Page, scope: Locator, words: string): Promise<string> {
  const defined = page.waitForResponse(
    (response) =>
      response.request().method() === "POST" &&
      new URL(response.url()).pathname === "/api/v1/definitions"
  );
  await selectWords(scope, words);
  await page.keyboard.press("d");
  const body: unknown = await (await defined).json();
  return typeof body === "object" && body !== null && "id" in body ? String(body.id) : "";
}

/** PANE-5's Conversation tab with the rewritten comment's thread open, at the tab's top. */
async function openComment(page: Page, id: string): Promise<Locator> {
  const turn = page.locator(`[data-turn="comment:${id}"]`);
  await open(page, "/issues/PANE-5/conversation", turn);
  await turn.getByRole("button", { name: "Expand thread" }).click();
  const comment = turn.locator(`[data-margin-item="${id}"]`);
  await expect(comment.locator('[data-ask-rewrite="rewritten"]')).toBeVisible();
  await page.evaluate(() => window.scrollTo(0, 0));
  await rest(page);
  // The thread's expansion animates; the picture waits for it to stand still.
  await page.waitForTimeout(400);
  return comment;
}

/** PANE-6's spec with its rewritten decision block in the middle of the window. */
async function openBlock(page: Page): Promise<Locator> {
  const block = page.locator(BLOCK).first();
  await open(page, "/issues/PANE-6/spec", block.locator('[data-ask-rewrite="rewritten"]'));
  await block.evaluate((element) => element.scrollIntoView({ block: "center" }));
  // The scroll lands over a frame or two; the picture waits for the page to stand still.
  await page.waitForTimeout(400);
  return block;
}

/** The phone Inbox scrolled to the flip audit's card, a little of the list above it. */
async function phoneCard(page: Page, id: string): Promise<Locator> {
  const card = cardOf(page, id);
  await open(page, "/", card.locator("[data-ask-question]"));
  await card.evaluate((element) => element.scrollIntoView({ block: "start" }));
  await page.evaluate(() => window.scrollBy(0, -64));
  // The scroll lands over a frame or two; the picture waits for the page to stand still.
  await page.waitForTimeout(300);
  return card;
}

/** The bar over words selected outside a document. */
const BAR = "[data-selection-bar]";

/** The bar's actions, each as it reads: its name and then its key. */
async function barActions(page: Page): Promise<string[]> {
  await expect(page.locator(BAR)).toBeVisible();
  return page
    .locator(`${BAR} [data-selection-action]`)
    .evaluateAll((actions) => actions.map((action) => action.textContent ?? ""));
}

/** Selects with the mouse, as a reader's drag does and as the demo's run selected, from the start
 *  of `from` to the end of `to`, each in the first text under `scope` that holds it, and waits
 *  for the bar over the words. */
async function drag(page: Page, scope: Locator, from: string, to: string): Promise<void> {
  const ends = await scope.evaluate(
    (root, words) => {
      const find = (wanted: string, start: boolean) => {
        const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
        for (let node = walker.nextNode(); node !== null; node = walker.nextNode()) {
          const at = node.textContent?.indexOf(wanted) ?? -1;
          if (at < 0) continue;
          const range = document.createRange();
          range.setStart(node, start ? at : at + wanted.length - 1);
          range.setEnd(node, start ? at + 1 : at + wanted.length);
          const box = range.getBoundingClientRect();
          return { x: start ? box.left + 1 : box.right - 1, y: box.top + box.height / 2 };
        }
        throw new Error(`no text holds ${JSON.stringify(wanted)}`);
      };
      return { from: find(words.from, true), to: find(words.to, false) };
    },
    { from, to }
  );
  await page.mouse.move(ends.from.x, ends.from.y);
  await page.mouse.down();
  await page.mouse.move(ends.to.x, ends.to.y, { steps: 8 });
  await page.mouse.up();
  await expect(page.locator(BAR)).toBeVisible();
  // The demo's pictures were taken 400 ms after its bar stood over the words; so are these.
  await page.waitForTimeout(400);
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

/** The answered question Quote reply's last picture reads, written once the Inbox's pictures are
 *  taken: its thread gives the reader a Reply row, which the demo's Inbox did not list. Each age is
 *  the demo's, counted back from `seededAt`, the moment every picture reads, and the reader has
 *  read the reply, as the demo's Inbox count of 6 says. */
async function seedAnsweredThread(seededAt: number): Promise<string> {
  const answered = await createAsk(
    "PANE-2",
    { options: [], question: ANSWERED.question, urgency: "low" },
    asSession(TOOL)
  );
  await answerAsk(answered.id, { expected_edited_at: null, selected: [], text: ANSWERED.answer });
  const reply = await createComment(
    "PANE-2",
    { ask_id: answered.id, body: ANSWERED.reply },
    asSession(TOOL)
  );
  const before = (minutes: number) =>
    `to_timestamp(${seededAt / 1000}) - interval '${minutes} minutes'`;
  await sql(
    `UPDATE asks SET created_at = ${before(ANSWERED.askedMinutesAgo)}, answer = jsonb_set(answer, '{at}', to_jsonb(${before(ANSWERED.answeredMinutesAgo)})) WHERE id = ${sqlLiteral(answered.id)}`
  );
  await sql(
    `UPDATE comments SET created_at = ${before(ANSWERED.repliedMinutesAgo)} WHERE id = ${sqlLiteral(reply.id)}`
  );
  await markThreadRead(
    answered.id,
    new Date(seededAt - (ANSWERED.repliedMinutesAgo - 1) * 60_000).toISOString()
  );
  return answered.id;
}

// SLOW(>=2s): seeds a project of seven issues through two servers, waits for three reviews and a
// rewrite, then takes 29 pictures, each in a fresh context.
test("the design's 29 pictures, taken from the build beside the demo's", async ({
  browser,
  reviewer,
}, testInfo) => {
  test.setTimeout(20 * 60_000);
  const seeded = await seedDemo(reviewer, async (flip) => {
    // The card the design changed: the flip audit's in the reading pane, with its split button.
    const context = await asUser(browser, READER, { viewport: DESKTOP });
    try {
      await inbox(await context.newPage(), flip);
    } finally {
      await context.close();
    }
  });
  const out = testInfo.outputPath("ask-card-pictures");
  const taken = new Set<Picture>();

  /** One picture: a fresh context at `viewport`, signed in as the reader, where `take` brings the
   *  page to the picture's state and `shoot` takes it, with the text cursor drawn where the
   *  picture is of where it waits (`caret`). */
  async function picture(
    viewport: { height: number; width: number },
    take: (
      page: Page,
      shoot: (name: Picture, options?: { caret?: boolean }) => Promise<void>
    ) => Promise<void>
  ): Promise<void> {
    const context = await asUser(browser, READER, { viewport });
    try {
      const page = await context.newPage();
      // Each age is read at the moment the seeding ended, as the demo read every age at its own
      // fixed moment, so a picture taken a minute later says what the first one says.
      await page.clock.install({ time: seeded.seededAt });
      await take(page, async (name, { caret = false } = {}) => {
        if (caret) {
          // A still picture of where the cursor waits draws it steadily, as the demo's did.
          await page.addStyleTag({ content: "textarea { caret-animation: manual; }" });
          // The style applies on the next frame; a tenth of a second is several frames.
          await page.waitForTimeout(100);
        }
        // CSS animations - the Rewriting… badge's pulse, Define's waiting dots - run on no clock
        // the page's fake one holds, so each picture stops them at their start.
        await page.screenshot({
          animations: "disabled",
          caret: caret ? "initial" : "hide",
          path: `${out}/${name}.png`,
        });
        taken.add(name);
      });
    } finally {
      await context.close();
    }
  }

  // The pictures are taken in the order their states come: every one before the flip audit's
  // rewrite reads its question as its agent wrote it, and the shortcut help comes after it.
  await test.step("ask-card-01-closed-inbox", () =>
    picture(DESKTOP, async (page, shoot) => {
      await inbox(page, seeded.flip);
      await shoot("ask-card-01-closed-inbox");
    }));
  await test.step("ask-card-02-menu-open", () =>
    picture(DESKTOP, async (page, shoot) => {
      const card = await inbox(page, seeded.flip);
      await card.locator("[data-ask-option]").first().check();
      await openMenu(page, card);
      await rest(page);
      await shoot("ask-card-02-menu-open");
    }));
  await test.step("ask-card-14-phone-card", () =>
    picture(PHONE, async (page, shoot) => {
      await phoneCard(page, seeded.flip);
      await shoot("ask-card-14-phone-card");
      // Below the reading column's width the Inbox draws each question's card in its list; the
      // decision block's card shows the bold its writer gave the question.
      await expect(cardOf(page, seeded.coldStart).locator("[data-ask-question] strong")).toHaveText(
        "Recommended: from the snapshot."
      );
    }));
  await test.step("ask-card-15-phone-menu", () =>
    picture(PHONE, async (page, shoot) => {
      const card = await phoneCard(page, seeded.flip);
      await openMenu(page, card);
      await shoot("ask-card-15-phone-menu");
    }));
  await test.step("ask-card-17-reaches", () =>
    picture(DESKTOP, async (page, shoot) => {
      const card = await inbox(page, seeded.flip);
      await card.locator("[data-ask-asker]").click();
      const recipients = page.getByRole("dialog").getByRole("region", { name: "Recipients" });
      await expect(recipients.getByText(REVIEWER.title)).toBeVisible();
      await expect(recipients.getByRole("button", { name: "Unfollow" })).toHaveCount(1);
      await expect(recipients.getByRole("button", { name: "Unsubscribe" })).toHaveCount(1);
      await rest(page);
      // The popover opens with a short transition; the picture waits for it to stand still.
      await page.waitForTimeout(300);
      await shoot("ask-card-17-reaches");
    }));
  await test.step("ask-card-08 and 09: Define on the flip audit, then its agent's words", async () => {
    await liveSessions(MIRROR);
    try {
      await picture(DESKTOP, async (page, shoot) => {
        const card = await inbox(page, seeded.flip);
        const id = await define(page, card.locator("[data-ask-question]"), FLIP_AUDIT.words);
        const popover = page.locator("[data-define-popover]");
        await expect(popover).toContainText(FLIP_AUDIT.meaning);
        await expect(popover.locator('[data-define-agent="waiting"]')).toBeVisible();
        // The popover's agent line pulses while it waits; the picture is taken once it is drawn.
        await page.waitForTimeout(200);
        await shoot("ask-card-08-define-ask-from-record");
        await expect
          .poll(async () => (await framesTo(MIRROR.id)).some((frame) => frame.id === id))
          .toBe(true);
        await replyToMessageDelivery(
          id,
          { attempt: 1, body: FLIP_AUDIT.agent ?? "" },
          asSession(MIRROR).actor
        );
        await expect(popover.locator('[data-define-agent="answered"]')).toContainText(
          FLIP_AUDIT.agent ?? ""
        );
        // The citation names the requirement it cites by its own label, after its document.
        await expect(popover.getByRole("link")).toHaveText("PANE-5 spec · R3 →");
        // The agent's words arrive with a short transition; the picture waits for it to end.
        await page.waitForTimeout(200);
        await shoot("ask-card-09-define-ask-with-agent");
      });
    } finally {
      await liveSessions(undefined);
    }
  });
  await test.step("ask-card-10-define-document-read-view", () =>
    picture(DESKTOP, async (page, shoot) => {
      const document = page.getByTestId("inbox-pane").locator(".ProseMirror");
      await open(page, `/?item=${seeded.approval}`, document);
      await expect(document).toContainText(REFUSED_LEASE.words);
      await define(page, document, REFUSED_LEASE.words);
      await expect(page.locator("[data-define-popover]")).toContainText(REFUSED_LEASE.meaning);
      // The popover opens with a short transition; the picture waits for it to stand still.
      await page.waitForTimeout(300);
      await shoot("ask-card-10-define-document-read-view");
    }));
  for (const name of [
    "ask-card-11-define-document-edit-bar",
    "ask-card-12-define-document-edit-view",
  ] as const) {
    await test.step(name, () =>
      picture(DESKTOP, async (page, shoot) => {
        await open(page, "/issues/PANE-5/spec", documentEditor(page));
        await expect(connectedDot(page)).toBeVisible();
        await selectTextIn(page, documentEditor(page), "Flip audit");
        if (name === "ask-card-11-define-document-edit-bar") {
          await expect(
            page
              .locator(".dispatch-action-bar")
              .filter({ visible: true })
              .getByRole("button", { exact: true, name: "Define" })
          ).toBeVisible();
          await shoot(name);
          return;
        }
        // The editor's own key for Define: Ctrl+Alt+Y, Cmd+Alt+Y on a Mac.
        await page.keyboard.press("Control+Alt+y");
        await expect(page.locator("[data-define-popover]")).toContainText(FLIP_AUDIT.meaning);
        // The popover opens with a short transition; the picture waits for it to stand still.
        await page.waitForTimeout(300);
        await shoot(name);
      })
    );
  }
  await test.step("ask-card-16-approval-menu", () =>
    picture(DESKTOP, async (page, shoot) => {
      const card = cardOf(page.locator("main"), seeded.approval);
      await open(page, "/issues/PANE-1/conversation", card);
      await card.evaluate((element) => element.scrollIntoView({ block: "start" }));
      // The scroll lands over a frame or two; the menu is opened once the page stands still.
      await page.waitForTimeout(300);
      await openMenu(page, card);
      const items = await page
        .locator("[data-ask-menu-item]")
        .evaluateAll((all) => all.map((item) => item.getAttribute("data-ask-menu-item")));
      expect(items).toEqual(["ask-back", "hand-on", "define"]);
      await rest(page);
      await shoot("ask-card-16-approval-menu");
    }));
  await test.step("ask-card-18-block-rewritten", () =>
    picture(DESKTOP, async (page, shoot) => {
      await openBlock(page);
      // The margin's card of the decision block shows the bold its writer gave the question.
      await expect(
        cardOf(page.locator("[data-margin-item]"), seeded.coldStart).locator(
          "[data-ask-question] strong"
        )
      ).toHaveText("Recommended: from the snapshot.");
      await shoot("ask-card-18-block-rewritten");
    }));
  await test.step("ask-card-19-block-original", () =>
    picture(DESKTOP, async (page, shoot) => {
      const block = await openBlock(page);
      await block.locator('[data-ask-rewrite="rewritten"]').click();
      await expect(block.locator('[data-ask-rewrite="original"]')).toBeVisible();
      await expect(cardOf(block, seeded.coldStart).locator("[data-ask-question]")).toContainText(
        COLD_START_ORIGINAL.question
      );
      await rest(page);
      // The block swaps its text for the agent's; the picture waits for the swap to settle.
      await page.waitForTimeout(300);
      await shoot("ask-card-19-block-original");
    }));
  await test.step("ask-card-20-handed-on", () =>
    picture(DESKTOP, async (page, shoot) => {
      // Everyone's questions, since this one waits on bruno rather than on the reader.
      const card = await inbox(page, seeded.handedOn, "assignee=everyone&");
      await expect(card.locator("[data-ask-handed-on]")).toContainText("handed on by dana");
      await shoot("ask-card-20-handed-on");
    }));
  await test.step("ask-card-21-comment-rewritten", () =>
    picture(DESKTOP, async (page, shoot) => {
      await openComment(page, seeded.comment);
      await shoot("ask-card-21-comment-rewritten");
    }));
  await test.step("ask-card-22-comment-original", () =>
    picture(DESKTOP, async (page, shoot) => {
      const comment = await openComment(page, seeded.comment);
      // `w` with the comment in focus: a press on its text puts the focus on the comment.
      await comment.locator(".dispatch-markdown").first().click();
      await page.keyboard.press("w");
      await expect(comment.locator('[data-ask-rewrite="original"]')).toBeVisible();
      await expect(comment).toContainText(COMMENT_ORIGINAL);
      await rest(page);
      // The comment swaps its text for the agent's; the picture waits for the swap to settle.
      await page.waitForTimeout(300);
      await shoot("ask-card-22-comment-original");
    }));
  await test.step("ask-card-23-issue-rewritten", () =>
    picture(DESKTOP, async (page, shoot) => {
      const badge = page.getByTestId("issue-header").locator("[data-ask-rewrite]");
      await open(page, "/issues/PANE-7", badge);
      // The badge shows the agent's title and spec in place, and a second press returns.
      await badge.click();
      await expect(badge).toHaveText("Original");
      await expect(page.locator("[data-issue-original-spec]")).toContainText("Need hb rows");
      await badge.click();
      await expect(badge).toHaveText("Rewritten");
      await rest(page);
      // The header swaps back to the rewrite; the picture waits for the swap to settle.
      await page.waitForTimeout(500);
      await shoot("ask-card-23-issue-rewritten");
    }));
  await test.step("ask-card-24-quote-select", () =>
    picture(DESKTOP, async (page, shoot) => {
      const card = await inbox(page, seeded.checked);
      await drag(
        page,
        card.locator("[data-ask-question]"),
        "The R3 audit decides",
        "only out of date for a week."
      );
      expect(await barActions(page)).toEqual(["Quote replyq", "Defined"]);
      await rest(page);
      await shoot("ask-card-24-quote-select");
    }));
  await test.step("ask-card-25-quote-ask", () =>
    picture(DESKTOP, async (page, shoot) => {
      const card = await inbox(page, seeded.checked);
      await drag(
        page,
        card.locator("[data-ask-question]"),
        "The R3 audit decides",
        "only out of date for a week."
      );
      await page.keyboard.press("q");
      await expectQuoted(card.locator("[data-ask-answer]"), DRY_RUN_QUOTE);
      await expect(page.locator(BAR)).toHaveCount(0);
      await rest(page);
      await shoot("ask-card-25-quote-ask", { caret: true });
    }));
  await test.step("ask-card-26-quote-comment", () =>
    picture(DESKTOP, async (page, shoot) => {
      const comment = await openComment(page, seeded.comment);
      await drag(
        page,
        comment.locator(".dispatch-markdown").first(),
        "Of last week's 412 refused leases",
        "so a retry wins back very little."
      );
      await page.keyboard.press("q");
      await expectQuoted(comment.getByRole("textbox", { name: "Reply" }), COMMENT_QUOTE);
      await rest(page);
      await shoot("ask-card-26-quote-comment", { caret: true });
    }));
  await test.step("ask-card-27-quote-append", () =>
    picture(DESKTOP, async (page, shoot) => {
      // The answer box holds a line the reader typed; the quote of the question's words follows.
      const card = await inbox(page, seeded.flip);
      const answer = card.locator("[data-ask-answer]");
      const action = card.locator("[data-ask-action]");
      await answer.fill(APPEND_TYPED);
      await expect(action).toBeDisabled();
      await drag(
        page,
        card.locator("[data-ask-question]"),
        "go with B per the train?",
        "go with B per the train?"
      );
      await page.keyboard.press("q");
      await expectQuoted(answer, APPEND_QUOTE);
      // The quote ends in a question mark and the typed line does not: Answer stands as the
      // typed line alone leaves it, refused until an option is picked.
      await expect(action).toBeDisabled();
      await rest(page);
      await shoot("ask-card-27-quote-append", { caret: true });
    }));
  await test.step("ask-card-28-quote-message", () =>
    picture(DESKTOP, async (page, shoot) => {
      const message = page.locator(`[data-turn="message:${seeded.message}"] .dispatch-markdown`);
      await open(page, "/issues/PANE-3/conversation", message);
      // The tab's top, as a reader who opened it stands: the issue's header above the composer.
      await page.evaluate(() => window.scrollTo(0, 0));
      // The scroll lands over a frame or two; the drag waits for the page to stand still.
      await page.waitForTimeout(400);
      await drag(page, message.first(), "The mirror can move", "on Thursday.");
      // The bar's own button this time, rather than `q`.
      await page.locator(`${BAR} [data-selection-action="Quote reply"]`).click();
      const composer = page
        .getByRole("form", { name: "Comment composer" })
        .filter({ has: page.getByRole("textbox", { exact: true, name: "Comment" }) });
      await expectQuoted(
        composer.getByRole("textbox", { exact: true, name: "Comment" }),
        MESSAGE_QUOTE
      );
      // The composer replies to the message the words were quoted from.
      await expect(composer).toContainText(ORDER.title);
      await rest(page);
      await shoot("ask-card-28-quote-message", { caret: true });
    }));
  await test.step("ask-card-06-rewrite-failed", () =>
    picture(DESKTOP, async (page, shoot) => {
      const card = await inbox(page, seeded.order);
      await pressOnCard(page, card, "r", "failed");
      // The reason is the badge's tooltip alone, and no other part of the card says why.
      const reason = await card.locator('[data-ask-rewrite="failed"]').getAttribute("title");
      expect(reason).toContain("label");
      await rest(page);
      // The badge turns from Rewriting to Rewrite failed; the picture waits for it to settle.
      await page.waitForTimeout(400);
      await shoot("ask-card-06-rewrite-failed");
    }));
  await test.step("ask-card-07-reviewer-rewritten-inbox", () =>
    picture(DESKTOP, async (page, shoot) => {
      const card = await inbox(page, seeded.checked);
      await expect(card.locator('[data-ask-rewrite="rewritten"]')).toBeVisible();
      await shoot("ask-card-07-reviewer-rewritten-inbox");
    }));
  await test.step("ask-card-03, 04 and 05: the flip audit's rewrite, running, done, and its original", () =>
    picture(DESKTOP, async (page, shoot) => {
      const card = await inbox(page, seeded.flip);
      await pressOnCard(page, card, "r", "rewriting");
      // The badge's dots move; the picture is taken once the badge reads Rewriting.
      await page.waitForTimeout(300);
      await shoot("ask-card-03-rewriting");
      await expect(card.locator('[data-ask-rewrite="rewritten"]')).toBeVisible({ timeout: 30_000 });
      await expect(card.locator("[data-ask-question]")).toContainText("When the ledger refuses");
      // The rewritten wording swaps in; the picture waits for the swap to settle.
      await page.waitForTimeout(400);
      await shoot("ask-card-04-rewritten");
      await pressOnCard(page, card, "w", "original");
      await expect(card.locator("[data-ask-question]")).toContainText(FLIP.question);
      // The original wording swaps in; the picture waits for the swap to settle.
      await page.waitForTimeout(300);
      await shoot("ask-card-05-original-shown");
    }));
  await test.step("ask-card-13-shortcut-help", () =>
    picture(DESKTOP, async (page, shoot) => {
      const card = await inbox(page, seeded.flip);
      await selectWords(card.locator("[data-ask-question]"), "ledger");
      // The row in hand, as a reader who walked the list with j and k stands.
      await page.locator(`[data-inbox-row="${seeded.flip}"]`).focus();
      await page.keyboard.press("Shift+Slash");
      await expect(page.getByRole("dialog", { name: "Keyboard shortcuts" })).toBeVisible();
      // The help opens with a short transition; the picture waits for it to stand still.
      await page.waitForTimeout(300);
      await shoot("ask-card-13-shortcut-help");
    }));

  // Quote reply's last picture reads an answered question written now, after the last picture of
  // the Inbox, whose list its thread would change.
  const answered = await seedAnsweredThread(seeded.seededAt);
  await test.step("ask-card-29-quote-answered-thread", () =>
    picture(DESKTOP, async (page, shoot) => {
      const record = page.locator(`main [data-testid="ask-${answered}"]`).first();
      const thread = page.locator(`main [data-testid="thread-${answered}"]`).first();
      await open(page, "/issues/PANE-2/conversation", thread.locator(".dispatch-markdown"));
      // The answered question under the tab's composer, the agent's reply under its answer.
      await record.evaluate((element) => element.scrollIntoView({ block: "start" }));
      // The scroll lands over a frame or two; the drag waits for the page to stand still.
      await page.waitForTimeout(400);
      await drag(page, thread, "skip", "the skipped leases too.");
      expect(await barActions(page)).toEqual(["Quote replyq"]);
      await page.keyboard.press("q");
      await expectQuoted(
        page.locator(`main [data-turn="ask:${answered}"]`).getByRole("textbox", { name: "Reply" }),
        ANSWERED_QUOTE
      );
      // The card above the answered question out of the window, the Reply button well inside it.
      await page.evaluate(() => window.scrollBy(0, 36));
      // The scroll lands over a frame or two; the picture waits for the page to stand still.
      await page.waitForTimeout(300);
      await rest(page);
      await shoot("ask-card-29-quote-answered-thread", { caret: true });
    }));

  expect([...taken].sort()).toEqual([...PICTURES].sort());
  const demo = process.env.DISPATCH_E2E_ASK_CARD_DEMO;
  if (demo !== undefined && demo !== "") {
    // A run named for the comparison makes it: a directory that is not there fails the row.
    expect(existsSync(demo), `DISPATCH_E2E_ASK_CARD_DEMO names ${demo}, which does not exist`).toBe(
      true
    );
    const comparisons = comparePictures(PICTURES, out, demo, out);
    await testInfo.attach("ask-card-pictures", {
      body: JSON.stringify(comparisons, null, 2),
      contentType: "application/json",
    });
  }
  console.log(`ask-card-pictures: ${taken.size} pictures in ${out}`);
});
