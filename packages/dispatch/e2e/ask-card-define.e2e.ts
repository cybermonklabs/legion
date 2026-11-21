import type { WritingCheckRow } from "@legion/contracts";
import { setLiveSessions, setSessionState } from "./agents";
import {
  createProjectDocument,
  getArtifactText,
  getEventLog,
  getIssueEvents,
  getReaderWords,
  getWritingCheckCounts,
  listAgentMessages,
  listMyDefinitions,
  replyToMessageDelivery,
} from "./api";
import {
  askNow,
  callsOf,
  DEFINE_PROMPT,
  DEFINING_PARAGRAPH,
  defineRule,
  definingBlockLink,
  framesTo,
  type Hand,
  openSurface,
  PROJECT,
  QUESTION,
  type Seeded,
  SURFACES,
  type Surface,
  seedProject,
  seedSurface,
  selectWords,
  TESTER,
  tester,
} from "./ask-card";
import { connectedDot, documentEditor, selectTextIn } from "./editor";
import { setModelRules } from "./fake-model-helpers";
import { sql } from "./psql";
import { sqlLiteral } from "./seed";
import { expect, type Locator, type Page, test } from "./test";
import { asUser } from "./users";

/**
 * Define (INFRA-3231's R7 and R8, check 294fc4ed): the rows of INFRA-3552's check c3, then R8's
 * counts and the reader's words route for the words defined. Select words in an open question on
 * each surface and press `d` - or, in a decision block inside the document editor, Ctrl+Alt+Y,
 * the editor's own key (Cmd+Alt+Y on a Mac) - and the definition from Dispatch's record shows
 * beside them, with a link to the spec block the stand-in cited; the live asking session gets one
 * side question from Dispatch carrying the definition's id, and its answer is kept with the
 * definition and shown beside it while the side question's wait lasts, writing no message and no
 * event; the ask is unchanged. A spec's read view, its edit view and a project document's read
 * view define words as well, and change nothing. A definition is its reader's alone, and each is
 * that reader's question about those words.
 *
 * At the commit before the change this file fails: `d` does nothing and no selection offers
 * Define.
 */

const WORDS = "flip audit";
const MEANING = "The check that compares each ledger row with its mirror before the ledger moves.";
const AGENT_MEANING = "I meant the nightly flip audit, the one the tester runs against the mirror.";

/** The definition beside the words. */
function popover(page: Page): Locator {
  return page.locator("[data-define-popover]");
}

/** Presses Define on the words selected on `surface`: `d` with the card in hand, and on a
 *  decision block, whose words are the document's in its editor, the editor's own key. Answers
 *  the definition's id, from the route's answer. */
async function define(page: Page, surface: Surface, hand: Hand): Promise<string> {
  const defined = page.waitForResponse(
    (response) =>
      response.request().method() === "POST" &&
      new URL(response.url()).pathname === "/api/v1/definitions"
  );
  if (surface === "decision block") {
    await selectTextIn(page, hand.question, WORDS);
    await page.keyboard.press("Control+Alt+y");
  } else {
    await hand.grab();
    await selectWords(hand.question, WORDS);
    await page.keyboard.press("d");
  }
  const response = await defined;
  expect(response.status()).toBe(201);
  const body: unknown = await response.json();
  expect(body).toMatchObject({ id: expect.any(String) });
  return typeof body === "object" && body !== null && "id" in body ? String(body.id) : "";
}

/** c3 on one surface: the record's definition beside the words with the cited block's link, one
 *  side question to the live asking session, its answer beside the first, no message and no
 *  event, the ask unchanged, one model call, and Escape closing the definition. */
async function defineOn(page: Page, surface: Surface): Promise<void> {
  await seedProject();
  const seeded = await seedSurface(surface);
  await setLiveSessions([TESTER]);
  const { block, link } = await definingBlockLink(seeded);
  await setModelRules([defineRule(WORDS, MEANING, [link])]);
  const eventsBefore = (await getEventLog()).at(-1)?.id ?? 0;
  const hand = await openSurface(page, surface, seeded);
  const id = await define(page, surface, hand);

  await expect(popover(page)).toContainText(MEANING);
  await expect(popover(page).getByRole("link")).toHaveAttribute("href", new RegExp(block));
  if (surface === "decision block") {
    // The editor's key typed no letter over the selected words: the block's text is as it was.
    await expect(hand.question).toContainText(QUESTION);
  }

  // One side question, from Dispatch, to the tester, naming the words and carrying the
  // definition's id; the tester's conversation lists no message for it.
  const asked = await sideQuestions();
  expect(asked).toHaveLength(1);
  expect(asked[0]).toMatchObject({ actor: { kind: "system" }, id, mode: "btw" });
  expect(asked[0]?.body).toContain(WORDS);
  const messages = await listAgentMessages(TESTER.session_id);
  expect(JSON.stringify(messages)).not.toContain(id);

  // The tester answers at 5 seconds, inside the side question's 60: the answer comes beside the
  // record's, under the tester's name.
  // The side question's answer is read on a clock the claim names: five seconds after the press.
  await page.waitForTimeout(5_000);
  await replyToMessageDelivery(id, { attempt: 1, body: AGENT_MEANING }, tester);
  const agent = popover(page).locator('[data-define-agent="answered"]');
  await expect(agent).toContainText(AGENT_MEANING);

  // Escape closes the definition.
  await page.keyboard.press("Escape");
  await expect(popover(page)).toHaveCount(0);

  // Nothing changed: the ask has no edit, the stand-in was called once, and no event holds the
  // words, the record's meaning or the tester's answer.
  expect((await askNow(seeded.askId)).question).toContain(WORDS);
  expect((await askNow(seeded.askId)).rewrite).toBeUndefined();
  expect(await callsOf(DEFINE_PROMPT)).toBe(1);
  const appended = JSON.stringify(await getEventLog({ after: eventsBefore }));
  for (const text of [WORDS, MEANING, AGENT_MEANING]) expect(appended).not.toContain(text);
  const issueEvents = JSON.stringify(await getIssueEvents(seeded.issueKey));
  for (const text of [MEANING, AGENT_MEANING]) expect(issueEvents).not.toContain(text);
}

/** The side questions Dispatch sent the tester. */
async function sideQuestions() {
  return (await framesTo(TESTER.session_id)).filter((frame) => frame.actor.kind === "system");
}

for (const surface of SURFACES) {
  // SLOW(>=2s): a seed, a page, a definition, and the tester's answer five seconds after it.
  test(`Define on the ${surface} answers beside the words, and the asking agent's answer comes beside it`, async ({
    browser,
  }) => {
    const alice = await asUser(browser, "alice");
    try {
      await defineOn(await alice.newPage(), surface);
    } finally {
      await alice.close();
    }
  });
}

// SLOW(>=2s): two definitions, and an answer past the side question's wait.
test("an unresponsive asking session gets no side question, and an answer past the wait is not shown", async ({
  browser,
}) => {
  await seedProject();
  const seeded: Seeded = await seedSurface("reading pane");
  await setLiveSessions([TESTER]);
  const { link } = await definingBlockLink(seeded);
  await setModelRules([defineRule(WORDS, MEANING, [link])]);
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const hand = await openSurface(page, "reading pane", seeded);

    // Unresponsive: the record's definition alone, and nothing sent.
    await setSessionState(TESTER.session_id, "unresponsive");
    await define(page, "reading pane", hand);
    await expect(popover(page)).toContainText(MEANING);
    await expect(popover(page).locator("[data-define-agent]")).toHaveCount(0);
    expect(await sideQuestions()).toHaveLength(0);
    await page.keyboard.press("Escape");
    await expect(popover(page)).toHaveCount(0);

    // Live again: a side question, answered once its 60 seconds have gone. The wait is read from
    // the definition's own time, so the definition is dated 61 seconds back rather than waited
    // out: the answer is then late by the server's reading and by the popover's.
    await setSessionState(TESTER.session_id, "live");
    const id = await define(page, "reading pane", hand);
    await expect(popover(page).locator('[data-define-agent="waiting"]')).toHaveCount(1);
    await sql(
      `UPDATE definitions SET created_at = created_at - interval '61 seconds' WHERE id = ${sqlLiteral(id)}`
    );
    const late = await replyToMessageDelivery(id, { attempt: 1, body: AGENT_MEANING }, tester).then(
      () => "accepted",
      (error: unknown) => String(error)
    );
    expect(late).toContain("409");
    expect(late).toContain("DEFINITION_ANSWER_LATE");
    await expect(popover(page).locator("[data-define-agent]")).toHaveCount(0, { timeout: 10_000 });
    await expect(popover(page)).not.toContainText(AGENT_MEANING);
  } finally {
    await alice.close();
  }
});

/** Opens a document version's read view and waits for `text` in it. */
async function openReadView(page: Page, path: string, text: string): Promise<Locator> {
  await page.setViewportSize({ height: 900, width: 1440 });
  await page.goto(path);
  const view = page.getByTestId("version-view");
  await expect(view).toContainText(text);
  return view;
}

// SLOW(>=2s): three documents opened, four definitions made in them.
test("a spec's read and edit views and a project document's read view define words and change nothing", async ({
  browser,
}) => {
  await seedProject();
  const seeded = await seedSurface("conversation");
  const document = await createProjectDocument(PROJECT, {
    content: "# Mirror runbook\n\nThe mirror replays the ledger after a cold start.\n",
    name: "Mirror runbook",
  });
  await setModelRules([defineRule(WORDS, MEANING, [])]);
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();

    // The spec's read view: `d`, then Define from the selection's own actions.
    const view = await openReadView(
      page,
      `/issues/${seeded.issueKey}/artifacts/spec?v=1`,
      DEFINING_PARAGRAPH
    );
    await selectWords(view.locator('[role="textbox"]'), WORDS);
    await page.keyboard.press("d");
    await expect(popover(page)).toContainText(MEANING);
    await page.keyboard.press("Escape");
    await expect(popover(page)).toHaveCount(0);
    await selectWords(view.locator('[role="textbox"]'), "ledger row");
    // The page draws the spec's live document beside the version, each with a selection bar of
    // its own; the words are selected in the version's.
    const bar = view.locator(".dispatch-action-bar");
    await expect(bar).toBeVisible();
    await bar.getByRole("button", { exact: true, name: "Define" }).click();
    await expect(popover(page)).toContainText(MEANING);
    await page.keyboard.press("Escape");

    // The edit view: the editor's own key, which types no letter and writes no version. The
    // version is read once the editor is connected, so whatever its opening writes is in it.
    await page.goto(`/issues/${seeded.issueKey}/spec`);
    await expect(documentEditor(page)).toContainText(DEFINING_PARAGRAPH);
    await expect(connectedDot(page)).toHaveText("connected");
    const before = await getArtifactText(seeded.artifactId);
    await selectTextIn(page, documentEditor(page), "ledger row");
    await page.keyboard.press("Control+Alt+y");
    await expect(popover(page)).toContainText(MEANING);
    await expect(documentEditor(page)).toContainText(DEFINING_PARAGRAPH);
    await page.keyboard.press("Escape");
    await expect(popover(page)).toHaveCount(0);

    // A project document's read view.
    const projectView = await openReadView(
      page,
      `/projects/${PROJECT}/documents/${document.artifact.slug}?version=1`,
      "The mirror replays the ledger"
    );
    await selectWords(projectView.locator('[role="textbox"]'), "cold start");
    await page.keyboard.press("d");
    await expect(popover(page)).toContainText(MEANING);
    await page.keyboard.press("Escape");

    // No document has a new version: a definition writes nothing.
    const after = await getArtifactText(seeded.artifactId);
    expect(after.version).toBe(before.version);
    expect(after.markdown).toBe(before.markdown);
    expect((await getArtifactText(document.artifact.id)).version).toBe(1);
    expect(await callsOf(DEFINE_PROMPT)).toBe(4);
  } finally {
    await alice.close();
  }
});

// SLOW(>=2s): two definitions, then three reads of what they recorded.
test("a definition is its reader's alone, counted as her Define use, and its words are her words", async ({
  browser,
}) => {
  await seedProject();
  const seeded = await seedSurface("reading pane");
  const document = await createProjectDocument(PROJECT, {
    content: "# Mirror runbook\n\nThe mirror replays the ledger after a cold start.\n",
    name: "Mirror runbook",
  });
  await setModelRules([defineRule(WORDS, MEANING, [])]);
  // The UTC day the first Define is counted on, read before it: a run that crosses midnight UTC
  // counts its two uses on two days.
  const first = new Date().toISOString().slice(0, 10);
  const alice = await asUser(browser, "alice");
  const ids: string[] = [];
  try {
    const page = await alice.newPage();
    const hand = await openSurface(page, "reading pane", seeded);
    ids.push(await define(page, "reading pane", hand));
    await expect(popover(page)).toContainText(MEANING);
    await page.keyboard.press("Escape");
    const view = await openReadView(
      page,
      `/projects/${PROJECT}/documents/${document.artifact.slug}?version=1`,
      "The mirror replays the ledger"
    );
    const defined = page.waitForResponse(
      (response) =>
        response.request().method() === "POST" &&
        new URL(response.url()).pathname === "/api/v1/definitions"
    );
    await selectWords(view.locator('[role="textbox"]'), "cold start");
    await page.keyboard.press("d");
    const body: unknown = await (await defined).json();
    expect(body).toMatchObject({ id: expect.any(String) });
    if (typeof body === "object" && body !== null && "id" in body) ids.push(String(body.id));
    await expect(popover(page)).toContainText(MEANING);
  } finally {
    await alice.close();
  }

  // Alice's own definitions route lists both; bob's lists neither.
  const mine = await listMyDefinitions({}, { login: "alice" });
  expect((mine.items ?? []).map((definition) => definition.id).sort()).toEqual([...ids].sort());
  expect(mine.total).toBe(ids.length);
  expect((await listMyDefinitions({}, { login: "bob" })).items ?? []).toHaveLength(0);

  // R8: each Define use is alice's question about those words, counted on the day, project and
  // person, and her words route lists the words. The rows of every day the two uses can fall on,
  // of every project and person, come a page at a time, so every page is read, and alice's uses
  // in the project are summed over those days.
  const last = new Date().toISOString().slice(0, 10);
  const rows: WritingCheckRow[] = [];
  for (let offset = 0; ; ) {
    const counts = await getWritingCheckCounts({ first, last, limit: 200, offset });
    const page = counts.items ?? [];
    rows.push(...page);
    offset += page.length;
    if (page.length === 0 || offset >= counts.total) break;
  }
  const uses = rows
    .filter((candidate) => candidate.project === PROJECT && candidate.person === "alice")
    .reduce((sum, candidate) => sum + candidate.define_uses, 0);
  expect(uses).toBe(2);
  const words = ((await getReaderWords("alice")).items ?? []).map((entry) => entry.word);
  expect(words).toEqual(expect.arrayContaining([WORDS, "cold start"]));
});
