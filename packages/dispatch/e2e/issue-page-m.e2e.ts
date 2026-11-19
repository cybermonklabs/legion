import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { type LegionRole, roleToken } from "@legion/contracts";

import type { DispatchEvent } from "../web/src/api/types";
import { PROPERTY_LABELS } from "../web/src/features/issue-page/page-structure";
import {
  ANSWER_LINES,
  CONTROL_LABELS,
  DELIVERY_STATE_WORDS,
  NODE_REFERENCES_WORDS,
} from "../web/src/features/issue-page/page-words";
import {
  agentRow,
  getUnsubscribeCalls,
  legionAgentEntry,
  setInterests,
  setLegionAgents,
  setLegionRegistryState,
  setLiveSessions,
  setSessionRecord,
} from "./agents";
import {
  answerAsk,
  claimIssue,
  createArtifactAsk,
  createAsk,
  createComment,
  createHandoff,
  createIssue,
  createIssueArtifact,
  createMessage,
  createProject,
  createProjectDocument,
  editArtifact,
  getAsk,
  getIssue,
  getIssueEvents,
  patchIssue,
  putIssueState,
  resolveAsk,
  resolveComment,
  uploadIssueFile,
} from "./api";
import { runDispatch } from "./dispatch-cli";
import { documentEditor, indexedBlockAsk, needsYouCards } from "./editor";
import { nextFrames } from "./frames";
import {
  historyScrollTop,
  historyViewport,
  scrollHistoryBy,
  scrollHistoryTo,
} from "./history-scroll";
import { inboxList, inboxPane, inboxRow, READING_PANE_WIDTH, selectRow } from "./inbox-pane";
import { focusAfterStaleness } from "./inbox-pane-scroll";
import { replyRowId } from "./inbox-threads";
import { seedConversationState } from "./issue-layout-seed";
import { asPlanner } from "./issue-layout-seed-support";
import { requirementText } from "./issue-layout-seed-text";
import {
  closeDetails,
  gotoIssue,
  issueRead,
  marginShell,
  openDetails,
  phase,
  presetToggle,
} from "./issue-page";
import {
  blockNode,
  DESKTOP,
  expectIssuePageDrawn,
  focusRow,
  focusThePage,
  nodeRow,
  PHONE,
  tree,
  unfoldEveryBranch,
} from "./issue-page-g";
import { marginSectionDisclosure } from "./issue-page-h";
import { focusedNodeId, focusInside, openEveryDeeperBranch } from "./issue-page-i";
import {
  askFacts,
  focusedNodeFacts,
  focusedRowFacts,
  historyEntries,
  historyItem,
  historyPanel,
  rowFacts,
} from "./issue-page-m";
import { withIssuePage } from "./issue-page-option";
import { withoutScrollAnchoring } from "./mermaid-helpers";
import { setCreatedAt, setEventCreatedAt } from "./seed";
import { navigateInApp, ONE_PIXEL_PNG } from "./sends";
import { type Browser, type BrowserContext, expect, type Locator, type Page, test } from "./test";
import { expectLoadSettled, watchApiRequests } from "./traffic";
import { assertPageFits } from "./unclipped";
import { asUser } from "./users";

/**
 * Part M of dispatch://INFRA-1196 (dispatch://INFRA-3948): Details' history, its people rows,
 * the facts of the focused row and what references it, on the new issue page.
 *
 * The rows of c-history and of c-people-facts (dispatch://INFRA-3948/artifact/verification-md).
 * Most are copies of rows the source rewrote for the new page while it was the only page: the
 * checkpoint's `issue-layout.e2e.ts` row, and the rows of `activity.e2e.ts`, `handoffs.e2e.ts`,
 * `collaborators.e2e.ts`, `asked-of.e2e.ts`, `asked-of-cli.e2e.ts` and `attention.e2e.ts` at
 * `750e2716`, whose files stay as `main` has them for today's page. Each copy signs its reader
 * in with the new page turned on (`reader`, `withIssuePage`) and opens it with `gotoIssue`; an
 * item's own address puts the focus on its node only once part N lands, so a copy that read a
 * node's facts from its address focuses the node's row through the outline (`rowFacts`). The
 * rows after them are new.
 */

/** A context signed in as `login` with the new issue page turned on. */
async function reader(browser: Browser, login: string): Promise<BrowserContext> {
  const context = await asUser(browser, login);
  await withIssuePage(context, login);
  return context;
}

// ---------------------------------------------------------------------------------------------
// c-history (1): the checkpoint's `issue-layout.e2e.ts`.
// ---------------------------------------------------------------------------------------------

/** Every event of `key`'s log, newest first, read 200 at a time as the history pages it. */
async function allEvents(key: string): Promise<DispatchEvent[]> {
  const all: DispatchEvent[] = [];
  let before: number | undefined;
  for (;;) {
    const page = await getIssueEvents(
      key,
      before === undefined ? { limit: 200, order: "desc" } : { before, limit: 200 }
    );
    all.push(...page);
    if (page.length < 200) return all;
    before = page.at(-1)?.seq;
  }
}

// c-conversation-state. The conversation is the outline and nothing else (dispatch://INFRA-1196
// R1, R14): each seeded comment and message a person took part in is one node, nothing offers a
// choice between words, questions, pinned and everything, and Details draws none of them. The
// history in the panel (R15) draws what no node does - the status change, the claim, both
// handoffs, the daemon's message, an agent's status note and the two comments agents wrote to
// each other, each once - and no event of a node, no version of a document, no result against a
// check and no child's move. The person's reply that moves the agents' thread into the outline
// is sent by the API: the box at the foot of the outline is part L's.
test("the conversation is the outline and the history is in the panel", async ({ browser }) => {
  // The whole issue's seed and two loads of its page, the history opened day by day between.
  test.setTimeout(240_000);
  await phase(browser, seedConversationState, async (page, seeded) => {
    // A day before today, which the history folds to one line with its count.
    const created = (await allEvents(seeded.key)).find((event) => event.type === "issue.created");
    if (created === undefined) throw new Error(`${seeded.key} recorded no issue.created`);
    await setEventCreatedAt(created.id, new Date(Date.now() - 86_400_000).toISOString());

    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectIssuePageDrawn(page);
    await unfoldEveryBranch(page);
    await openEveryDeeperBranch(page);
    const turns = [
      ...seeded.comments.threads,
      ...seeded.comments.replies,
      seeded.messages.root,
      seeded.messages.reply,
      seeded.messages.replyToReply,
    ];
    for (const id of turns) {
      await expect(
        page.locator(`[data-node-row][data-node-id="${id}"]`),
        `${id} is not one node of the outline`
      ).toHaveCount(1);
    }
    await expect(page.locator("[data-conversation-state], [data-turn]")).toHaveCount(0);
    for (const name of ["Words", "Questions", "Pinned", "Everything"]) {
      await expect(page.getByRole("tab", { exact: true, name })).toHaveCount(0);
      await expect(page.getByRole("radio", { exact: true, name })).toHaveCount(0);
    }
    await openDetails(page);
    // The newest entry, the three uploads, says the history has drawn the log it reads.
    await expect(historyPanel(page).locator('[data-history-uploads="3"]')).toHaveCount(1);
    await expect(
      marginShell(page).getByText("Thread 1 opens here.", { exact: false }),
      "Details draws a comment a person took part in, which is the outline's"
    ).toHaveCount(0);

    // A version of the spec saved and a question asked and resolved: neither is a line of the
    // history, which leaves every event of a node and every document version out.
    await editArtifact(seeded.specArtifactId, {
      ops: [{ find: requirementText(1), op: "replace", with: `${requirementText(1)} Saved.` }],
    });
    const asked = await createAsk(
      seeded.key,
      { question: "Is a resolved question left out of the history?" },
      asPlanner
    );
    await resolveAsk(asked.id, { kind: "resolved", reason: "It is." }, asPlanner);

    await gotoIssue(page, `/issues/${seeded.key}/activity`);
    await expect(
      historyPanel(page),
      "/activity does not open the panel at its history"
    ).toBeVisible();
    // The history pages the log 200 events at a time: every page is read in before it is. A
    // page is pressed for only once the first is drawn, and the log's length bounds the presses.
    const events = await allEvents(seeded.key);
    await expect(historyEntries(page).first()).toBeVisible();
    const older = historyPanel(page).getByRole("button", {
      exact: true,
      name: CONTROL_LABELS.load_older,
    });
    for (let presses = 1; await older.isVisible(); presses += 1) {
      expect(
        presses,
        `${CONTROL_LABELS.load_older} is still offered past the log's last page`
      ).toBeLessThanOrEqual(Math.floor(events.length / 200));
      const answered = page.waitForResponse((response) => {
        const url = new URL(response.url());
        return (
          url.pathname === `/api/v1/issues/${seeded.key}/events` && url.searchParams.has("before")
        );
      });
      await older.click();
      await answered;
      await nextFrames(page);
    }
    const folded = historyPanel(page).locator('[data-history-day] > button[aria-expanded="false"]');
    await expect(folded.first(), "no day before today is folded to one line").toHaveText(/ · \d+$/);
    for (let left = await folded.count(); left > 0; left -= 1) {
      await folded.first().click();
      await expect(folded).toHaveCount(left - 1);
    }
    // With the whole log read and every day open, no turn of a person's thread is drawn here.
    for (const id of turns) {
      await expect(historyItem(page, id), `${id} is drawn in the history too`).toHaveCount(0);
    }

    const seqs = (
      await historyEntries(page).evaluateAll((items) =>
        items.map((item) => Number(item.getAttribute("data-event-seq")))
      )
    ).filter((seq) => seq > 0);
    expect(seqs, "the history is not newest first").toEqual([...seqs].sort((a, b) => b - a));
    const drawn = new Set(seqs);
    const people = new Set(turns);
    const ofNodes = events.filter(
      (event) =>
        ((event.type === "ask.opened" ||
          event.type === "ask.answered" ||
          event.type === "ask.resolved") &&
          event.payload.kind === "question") ||
        event.type === "result.recorded" ||
        event.type === "child.status" ||
        (event.type === "artifact.version" &&
          event.payload.artifact_id === seeded.specArtifactId) ||
        ((event.type === "comment.created" || event.type === "message.created") &&
          people.has(event.payload.id))
    );
    for (const type of ["ask.resolved", "result.recorded", "artifact.version"]) {
      expect(
        ofNodes.some((event) => event.type === type),
        `the seed recorded no ${type}`
      ).toBe(true);
    }
    expect(
      ofNodes.filter((event) => drawn.has(event.seq)).map((event) => event.type),
      "the history draws an event of a node, a document's version, a result or a child's move"
    ).toEqual([]);

    // What it draws, each once.
    const once = async (seq: number | undefined, what: string) => {
      expect(seq, `the seed recorded no ${what}`).toBeDefined();
      await expect(historyPanel(page).locator(`[data-event-seq="${seq}"]`), what).toHaveCount(1);
    };
    const status = events.filter(
      (event) => event.type === "issue.updated" && event.payload.status === "in_progress"
    );
    await once(status.at(-1)?.seq, "the status change");
    await once(events.find((event) => event.type === "issue.claimed")?.seq, "the claim");
    const handoffs = events.filter((event) => event.type === "handoff.recorded");
    expect(handoffs, "the seed recorded both handoffs").toHaveLength(2);
    for (const handoff of handoffs) await once(handoff.seq, `handoff ${handoff.seq}`);
    for (const [id, words] of [
      [seeded.daemonMessage, "The planner's session was replaced"],
      [seeded.statusNote, "Status: the plan is drafted"],
      [seeded.agentComments.root, "the plan's third task needs a second look"],
      [seeded.agentComments.reply, "the third task stands as written"],
    ] as const) {
      await expect(historyItem(page, id), `${words} is not drawn once`).toHaveCount(1);
      await expect(historyItem(page, id)).toContainText(words);
    }
    for (const id of [seeded.agentComments.root, seeded.agentComments.reply]) {
      await expect(historyPanel(page).locator(`[data-history-reply="${id}"]`)).toHaveCount(1);
    }
    const uploads = historyPanel(page).locator('[data-history-uploads="3"]');
    await expect(uploads, "the three uploads are not one entry").toHaveCount(1);
    for (const name of seeded.uploads) {
      await expect(uploads.getByRole("link", { exact: true, name })).toHaveCount(1);
    }

    // A person's reply to one of the agents' comments moves that thread into the outline.
    await historyPanel(page).locator(`[data-history-reply="${seeded.agentComments.root}"]`).click();
    await createComment(
      seeded.key,
      { body: "A person joins the agents' thread.", reply_to: seeded.agentComments.root },
      { login: "alice" }
    );
    await expect(historyItem(page, seeded.agentComments.root)).toHaveCount(0);
    await expect(
      page.locator(`[data-node-row][data-node-id="${seeded.agentComments.root}"]`)
    ).toHaveCount(1);
  });
});

// ---------------------------------------------------------------------------------------------
// c-history (2) to (6): `activity.e2e.ts` at `750e2716`.
// ---------------------------------------------------------------------------------------------

const activityAgent = {
  actor: { id: "e2e-activity-agent", kind: "session" as const },
  as: "agent" as const,
};

// The issue's history is the list in Details that `/activity` opens (dispatch://INFRA-1196 R15):
// every event no row of the outline draws, and the threads agents wrote among themselves, whole.
// What the outline draws - an ask, answered or retracted - is not in it, and nothing the
// history draws is a row of the outline.
// The source's row carried `@pr`, and this copy does not: the pull-request lane holds its 150
// real-stack rows (`e2e/__tests__/pull-request-lane.test.ts`), and `activity.e2e.ts` keeps its
// own `@pr` row for today's page. The same holds for the three other copies that carried it.
test("the history lists the events and the agents' threads the outline does not draw", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "Review this specification.",
    title: "One card per conversation",
  });
  const answered = await createAsk(
    issue.key,
    { options: [{ label: "Ship" }], question: "Ship it?" },
    activityAgent
  );
  await answerAsk(answered.id, { expected_edited_at: null, selected: ["Ship"] }, { login: "bob" });
  const retracted = await createAsk(issue.key, { question: "Withdrawn question?" }, activityAgent);
  await resolveAsk(retracted.id, { kind: "retracted", reason: "Superseded." }, activityAgent);
  const resolved = await createComment(issue.key, { body: "A resolved thread." }, activityAgent);
  await resolveComment(resolved.id);
  const open = await createComment(issue.key, { body: "An open thread." }, activityAgent);
  const message = await createMessage(issue.key, { body: "A message thread." }, activityAgent);
  await patchIssue(issue.key, { status: "in_progress" });
  await claimIssue(issue.key, activityAgent);
  await createIssueArtifact(issue.key, { content: "# Note\n", name: "note.md" });

  const alice = await reader(browser, "alice");
  try {
    const page = await alice.newPage();
    await gotoIssue(page, `/issues/${issue.key}/activity`);
    const history = historyPanel(page);
    await expect(history).toBeVisible();
    const rows = historyEntries(page);
    // The issue's own events, each a line of its own.
    for (const description of ["updated the issue", "claimed the issue", "added note.md"]) {
      await expect(rows.filter({ hasText: description })).toHaveCount(1);
    }
    // The threads agents wrote among themselves, each once, whole, with its Reply - and no row
    // of the outline.
    for (const id of [resolved.id, open.id, message.id]) {
      await expect(historyItem(page, id)).toHaveCount(1);
      await expect(history.locator(`[data-history-reply="${id}"]`)).toHaveCount(1);
      await expect(nodeRow(page, id)).toHaveCount(0);
    }
    await expect(historyItem(page, open.id)).toContainText("An open thread.");
    // The asks are rows of the outline, answered and retracted, and no line of the history.
    await unfoldEveryBranch(page);
    for (const ask of [answered, retracted]) await expect(nodeRow(page, ask.id)).not.toHaveCount(0);
    await expect(history).not.toContainText("Ship it?");
    await expect(history).not.toContainText("Withdrawn question?");
    await expect(tree(page)).not.toContainText("An open thread.");
    // The panel draws no list of the conversation.
    await expect(page.getByRole("list", { name: "Conversation turns" })).toHaveCount(0);
    await page.screenshot({
      fullPage: true,
      path: testInfo.outputPath(`activity-rows-${testInfo.project.name}.png`),
    });
  } finally {
    await alice.close();
  }
});

// SLOW(>=2s): four status moves, three of them back-dated a day and the read watermark set,
// before one cold load of the history. A day before today is one line with its count, which
// opens it; the reader's watermark is drawn where it stood when the history first drew, at the
// end of today's list when the reader stopped on an earlier day, and a later move of the mark -
// the outline's own write once part L lands, made here by the API and read back on a refocus -
// leaves the line where it was, across a close and an open of Details too.
test("the history folds each day before today and marks where the reader stopped", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Dividers" });
  await patchIssue(issue.key, { status: "in_progress" });
  await patchIssue(issue.key, { status: "testing" });
  await patchIssue(issue.key, { status: "needs_review" });
  await patchIssue(issue.key, { status: "in_progress" });

  const events = await getIssueEvents(issue.key, { limit: 100 });
  const created = events.find((event) => event.type === "issue.created");
  const updates = events
    .filter((event) => event.type === "issue.updated")
    .sort((one, other) => one.seq - other.seq);
  const [first, second, third, fourth] = updates;
  if (
    created === undefined ||
    first === undefined ||
    second === undefined ||
    third === undefined ||
    fourth === undefined
  ) {
    throw new Error("seeded divider events were not recorded");
  }
  const yesterday = new Date(Date.now() - 24 * 60 * 60 * 1_000).toISOString();
  await Promise.all([
    setEventCreatedAt(created.id, yesterday),
    setEventCreatedAt(first.id, yesterday),
    setEventCreatedAt(second.id, yesterday),
  ]);
  await putIssueState(issue.key, { last_read_seq: second.seq }, { login: "alice" });
  const alice = await reader(browser, "alice");
  try {
    const page = await alice.newPage();
    await gotoIssue(page, `/issues/${issue.key}/activity`);

    const days = historyPanel(page).locator("[data-history-day]");
    await expect(days).toHaveCount(2);
    await expect(days.first()).toContainText(CONTROL_LABELS.history_today);
    const earlier = days.nth(1).getByRole("button", {
      name: `${CONTROL_LABELS.history_yesterday} · 3`,
    });
    await expect(earlier).toHaveAttribute("aria-expanded", "false");
    // Today's lines are drawn, and the line that says what is new closes today's list, which is
    // open, rather than heading the day the list folds; yesterday's are behind its one line.
    const order = () =>
      historyPanel(page)
        .locator("[data-event-seq], li[aria-label]")
        .evaluateAll((items) =>
          items.map(
            (item) => item.getAttribute("data-event-seq") ?? item.getAttribute("aria-label")
          )
        );
    await expect
      .poll(order, "the line that says what is new is not at the end of today's list")
      .toEqual([String(fourth.seq), String(third.seq), CONTROL_LABELS.history_unread]);

    await earlier.click();
    await expect(earlier).toHaveAttribute("aria-expanded", "true");
    // Newest first: the watermark sits below the oldest unread line and above the newest line
    // the reader had already seen, which here is the first line of the day before.
    const marked = [
      String(fourth.seq),
      String(third.seq),
      CONTROL_LABELS.history_unread,
      String(second.seq),
      String(first.seq),
      String(created.seq),
    ];
    expect(await order()).toEqual(marked);
    // The line stays the last of today's list, above yesterday's lines, with the day open.
    await expect(
      days.first().locator(":scope > ol > li").last(),
      "opening Yesterday moved the line out of today's list"
    ).toHaveAttribute("aria-label", CONTROL_LABELS.history_unread);

    // The mark moves on to the newest line, as the outline's end writes it, and the page reads
    // it again on a refocus, as a tab left open does: the line stays where the reader stopped.
    await putIssueState(issue.key, { last_read_seq: fourth.seq }, { login: "alice" });
    const reread = page.waitForResponse(
      (response) =>
        response.request().method() === "GET" &&
        new URL(response.url()).pathname === "/api/v1/me/state"
    );
    await focusAfterStaleness(page);
    await reread;
    await nextFrames(page);
    expect(await order(), "a later move of the read mark moved the line").toEqual(marked);

    // Details closed and opened again draws the history as the reader left it.
    await closeDetails(page);
    await openDetails(page);
    await nextFrames(page);
    expect(await order(), "closing and opening Details moved the line").toEqual(marked);
  } finally {
    await alice.close();
  }
});

// A line of the history pins its event: the line's own Pin, which a reload keeps, and its Unpin.
test("a history line pins its event, the pin outlives a reload, and the line unpins it", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Pin an event" });
  await patchIssue(issue.key, { status: "in_progress" });

  const alice = await reader(browser, "alice");
  try {
    const page = await alice.newPage();
    await gotoIssue(page, `/issues/${issue.key}/activity`);
    const row = historyEntries(page).filter({ hasText: "updated the issue" });
    await expect(row).toHaveCount(1);
    await row.getByRole("button", { exact: true, name: CONTROL_LABELS.pin }).click();
    await expect(
      row.getByRole("button", { exact: true, name: CONTROL_LABELS.unpin })
    ).toHaveAttribute("aria-pressed", "true");
    // The pin is one write of the page's store: nothing is pending once Dispatch has it.
    await expect(page.getByRole("region", { name: "Pending changes" })).toHaveCount(0);

    await page.reload();
    const again = historyEntries(page).filter({ hasText: "updated the issue" });
    await expect(
      again.getByRole("button", { exact: true, name: CONTROL_LABELS.unpin })
    ).toHaveAttribute("aria-pressed", "true");
    await again.getByRole("button", { exact: true, name: CONTROL_LABELS.unpin }).click();
    await expect(
      again.getByRole("button", { exact: true, name: CONTROL_LABELS.pin })
    ).toHaveAttribute("aria-pressed", "false");
  } finally {
    await alice.close();
  }
});

// The fake listener holds no record of `ghost`, so the mention is refused as `no live session
// ghost` and its receipt settles `failed`. A person's comment is a row of the outline, and its
// deliveries are the focused row's facts in Details: the one there must not read as delivered.
// The history draws no event of a row the outline draws (R15), so this delivery is no line of
// it. The comment's own address focuses its row only once part N lands, so the row is focused
// through the outline.
test("a mention that could not be delivered reads as not delivered in the focused row's facts", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Mention of a missing session" });
  const comment = await createComment(
    issue.key,
    { body: "Look at this, @session:ghost.", mentions: [{ target: "session:ghost" }] },
    { login: "alice" }
  );
  await expect
    .poll(async () =>
      (await getIssueEvents(issue.key, { limit: 100 })).flatMap((event) =>
        event.type === "comment.delivery" ? [event.payload.state] : []
      )
    )
    .toEqual(["failed"]);

  const alice = await reader(browser, "alice");
  try {
    const page = await alice.newPage();
    const facts = await rowFacts(page, issue.key, comment.id);
    const deliveries = facts.locator("[data-deliveries] li");
    await expect(deliveries).toHaveCount(1);
    await expect(deliveries).toContainText(DELIVERY_STATE_WORDS.failed);
    await expect(deliveries).not.toContainText(DELIVERY_STATE_WORDS.sent);
    // Details' history, open beside the facts, draws no line of the comment or its delivery:
    // read once its log is drawn, which the issue's own creation says.
    await expect(historyPanel(page)).toBeVisible();
    await expect(historyEntries(page).filter({ hasText: "created the issue" })).toHaveCount(1);
    await expect(historyEntries(page).filter({ hasText: "mention" })).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

// A comment body is bounded block by block, so one far longer than an event carries rides its
// event as its first part. The history draws a thread agents wrote among themselves from the
// comment itself, not from its event, so the whole body is there, and the comment's time links
// to its own address, which marks the turn.
test("the history draws an agent's comment longer than one event whole, linked to its address", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "A comment longer than one event" });
  // Two 20,000-unit paragraphs: each is inside the 32,000-unit block bound, and together they
  // are past the 32,000 units one event carries, so the event keeps the first paragraph alone.
  const first = "A".repeat(20_000);
  const second = "B".repeat(20_000);
  const comment = await createComment(issue.key, { body: `${first}\n\n${second}` }, activityAgent);

  const alice = await reader(browser, "alice");
  try {
    const page = await alice.newPage();
    await gotoIssue(page, `/issues/${issue.key}/activity`);
    const item = historyItem(page, comment.id);
    await expect(item).toHaveCount(1);
    await expect(item).toContainText(first);
    await expect(item).toContainText(second);

    // The turn's time is the link to its own address; its author's name links to the agent.
    await item.locator(`a[href$="/comments/${comment.id}"]`).click();
    await expect(page).toHaveURL(
      new RegExp(`/issues/${issue.key}/comments/${comment.id}(\\?|#|$)`)
    );
    await expect(historyItem(page, comment.id)).toHaveAttribute("aria-current", "true");
  } finally {
    await alice.close();
  }
});

// ---------------------------------------------------------------------------------------------
// c-history (7) to (9): `handoffs.e2e.ts` at `750e2716`.
// ---------------------------------------------------------------------------------------------

// The session that writes both versions, as `dispatch handoff-write` does: under the agent bearer,
// naming itself, with the title its origin stamps on the write.
const implementer = {
  id: "e2e-handoff-implementer",
  kind: "session" as const,
  origin: { session_title: "Implementer (e2e)" },
};

/** An issue carrying v1 and v2 of its `implement` handoff, each body with a two-entry `proof`. */
async function seedHandoffs(): Promise<string> {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Handoffs on the issue" });
  await createHandoff(
    issue.key,
    "implement",
    {
      schemaVersion: 1,
      phase: "implement",
      issue: issue.key,
      verified: true,
      summary: "First pass",
      proof: [
        { claim: "unit tests pass", command: "bun run test" },
        { claim: "types check", command: "bun run typecheck" },
      ],
    },
    implementer
  );
  await createHandoff(
    issue.key,
    "implement",
    {
      schemaVersion: 1,
      phase: "implement",
      issue: issue.key,
      verified: true,
      summary: "Second pass, review addressed",
      files: ["web/src/a.ts", "web/src/b.ts"],
      proof: [
        { claim: "browser suite green", command: "bun run e2e" },
        { claim: "lint clean", command: "bunx biome check" },
      ],
    },
    implementer
  );
  return issue.key;
}

/** The history's line for one handoff version, by the words the line reads. */
function handoffLine(page: Page, version: number): Locator {
  return historyEntries(page).filter({
    hasText: `recorded the implement handoff, version ${version}`,
  });
}

// The conversation's handoff cards are the history's lines (dispatch://INFRA-1196, the
// Design's "Where each thing goes": "The conversation's handoff cards, and the Activity tab -
// The history in the panel"), and each line keeps what the card opened: its fields and every
// proof entry, read when the reader opens them.
test("the history lists each handoff version, and opening one shows its fields and every proof entry", async ({
  browser,
}, testInfo) => {
  const key = await seedHandoffs();

  const alice = await reader(browser, "alice");
  try {
    const page = await alice.newPage();
    await gotoIssue(page, `/issues/${key}/activity`);
    await expect(historyPanel(page)).toBeVisible();

    const lines = historyEntries(page).filter({ hasText: "recorded the implement handoff" });
    await expect(lines).toHaveCount(2);
    // Newest first: version 2 was written after version 1.
    await expect(lines.first()).toContainText("version 2");
    await expect(lines.last()).toContainText("version 1");

    const v1 = handoffLine(page, 1);
    const v2 = handoffLine(page, 2);
    await expect(v2).toContainText("Implementer (e2e)");
    await expect(v1).toContainText("Implementer (e2e)");

    const toggle = v2.getByRole("button", { name: CONTROL_LABELS.show_fields });
    await expect(toggle).toHaveAttribute("aria-expanded", "false");
    await toggle.click();
    await expect(v2.getByRole("button", { name: CONTROL_LABELS.hide_fields })).toHaveAttribute(
      "aria-expanded",
      "true"
    );

    const field = (name: string) => v2.locator(`[data-handoff-field="${name}"]`);
    await expect(field("schemaVersion")).toContainText("1");
    await expect(field("phase")).toContainText("implement");
    await expect(field("issue")).toContainText(key);
    await expect(field("verified")).toContainText("true");
    await expect(field("summary")).toContainText("Second pass, review addressed");
    // An array field is indented JSON.
    await expect(field("files").locator("pre")).toContainText('"web/src/a.ts"');
    await expect(field("files").locator("pre")).toContainText('"web/src/b.ts"');
    // The proof list is one row per entry, each named by the page's word for it and counted.
    const proof = field("proof");
    await expect(proof).toHaveCount(2);
    await expect(proof.nth(0)).toContainText(`${CONTROL_LABELS.handoff_fields.proof} 1`);
    await expect(proof.nth(0)).toContainText("browser suite green");
    await expect(proof.nth(1)).toContainText(`${CONTROL_LABELS.handoff_fields.proof} 2`);
    await expect(proof.nth(1)).toContainText("lint clean");
    // Version 1 stays folded: its body is not on the page.
    await expect(v1.locator("[data-handoff-field]")).toHaveCount(0);
    await expect(page.getByText("First pass", { exact: true })).toHaveCount(0);

    await page.screenshot({
      fullPage: true,
      path: testInfo.outputPath(`handoff-history-${testInfo.project.name}.png`),
    });
  } finally {
    await alice.close();
  }
});

// A handoff written while the history is open reaches it over the event stream: the write's
// `handoff.recorded` is prepended to the issue's `["events", key]` log the history reads
// (`prependEventToLog`), so the new version's line arrives with no reload.
test("a handoff written while the history is open arrives without a reload", async ({
  browser,
}) => {
  const key = await seedHandoffs();

  const alice = await reader(browser, "alice");
  try {
    const page = await alice.newPage();
    await gotoIssue(page, `/issues/${key}/activity`);
    const cards = historyEntries(page).filter({ hasText: "recorded the implement handoff" });
    await expect(cards).toHaveCount(2);
    // A mark on this document alone: a reload would load a new one, which carries no attribute of
    // the test's, while a refresh of the list leaves the document as it is.
    await page.evaluate(() => document.documentElement.setAttribute("data-loaded-once", "yes"));

    await createHandoff(
      key,
      "implement",
      {
        schemaVersion: 1,
        phase: "implement",
        issue: key,
        verified: true,
        summary: "Third pass, written while the history was open",
        proof: [
          { claim: "browser suite green", command: "bun run e2e" },
          { claim: "lint clean", command: "bunx biome check" },
        ],
      },
      implementer
    );

    await expect(cards).toHaveCount(3);
    await expect(cards.first()).toContainText("version 3");
    await expect(handoffLine(page, 3)).toContainText("Implementer (e2e)");
    await expect(page.locator("html")).toHaveAttribute("data-loaded-once", "yes");
  } finally {
    await alice.close();
  }
});

// One line per version, and no second drawing of either anywhere on the page.
test("the history has one line per recorded handoff version", async ({ browser }) => {
  const key = await seedHandoffs();

  const alice = await reader(browser, "alice");
  try {
    const page = await alice.newPage();
    await gotoIssue(page, `/issues/${key}/activity`);
    await expect(historyPanel(page)).toBeVisible();
    const rows = historyEntries(page);
    await expect(rows.filter({ hasText: "recorded the implement handoff" })).toHaveCount(2);
    for (const version of [1, 2]) {
      await expect(handoffLine(page, version)).toHaveCount(1);
    }
  } finally {
    await alice.close();
  }
});

// ---------------------------------------------------------------------------------------------
// c-history (10) to (15): `attention.e2e.ts` at `750e2716`, with `e2e/history-scroll.ts`.
// ---------------------------------------------------------------------------------------------

const attentionSession = {
  actor: { id: "e2e-attention", kind: "session" as const },
  as: "agent" as const,
};

/**
 * The issue's history, in Details (dispatch://INFRA-1196 R15): where the comments and messages
 * an agent wrote outside every thread a person wrote in are read, newest first, each thread one
 * entry carrying its own id in `data-history-entry`. The history holds the reader's place in the
 * box Details scrolls in (`IssueHistory.tsx`'s `ViewportAnchor`), and these rows read that.
 */
const HISTORY = '[data-testid="issue-history"]';

/**
 * The entry the reader is on, read the way `ViewportAnchor` reads it: the entry nearest the
 * visible centre of the history, probed through `elementFromPoint`. `clip` is the box the
 * history is clipped to (`history-scroll.ts`).
 */
async function readingAnchor(page: Page, clip: { bottom: number; top: number }): Promise<string> {
  const value = await page.evaluate(
    ({ clipBox, rootSelector }) => {
      const list = document.querySelector<HTMLElement>(rootSelector);
      if (list === null) {
        return null;
      }
      const rect = list.getBoundingClientRect();
      const x = rect.left + rect.width / 2;
      const y = (Math.max(rect.top, clipBox.top) + Math.min(rect.bottom, clipBox.bottom)) / 2;
      for (const offset of [0, 8, 16, 24, 32, 40, -8, -16, -24, -32, -40]) {
        const row = document
          .elementFromPoint(x, y + offset)
          ?.closest<HTMLElement>("[data-history-entry]");
        const found = row?.getAttribute("data-history-entry");
        if (found !== null && found !== undefined && found !== "") {
          return found;
        }
      }
      return null;
    },
    { clipBox: clip, rootSelector: HISTORY }
  );
  expect(value).not.toBeNull();
  return value ?? "";
}

/** Opens `/activity` on `issueKey`, which opens Details at the history, and waits until the
 *  history holds `newest`. */
async function openHistory(page: Page, issueKey: string, newest: string): Promise<void> {
  await gotoIssue(page, `/issues/${issueKey}/activity`);
  await expect(historyPanel(page).getByText(newest)).toBeVisible();
}

/** An issue with `count` messages an agent wrote, `Existing message <n>`, newest last. */
async function seedMessages(title: string, count: number, words = "Existing message") {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title });
  for (let index = 0; index < count; index += 1) {
    await createMessage(issue.key, { body: `${words} ${index}` }, attentionSession);
  }
  return issue;
}

test("a reader scrolled down the history keeps their place when a new entry arrives", async ({
  browser,
}) => {
  const issue = await seedMessages("Attention test", 30);

  const context = await reader(browser, "alice");
  try {
    const page = await context.newPage();
    await openHistory(page, issue.key, "Existing message 29");
    // The browser's own scroll anchoring is turned off. No box of the page sets
    // `overflow-anchor`, so Chromium anchors Details' box itself and would put the entry back
    // where it was when the arrival lands above it: the row would pass with the history's own
    // `ViewportAnchor` taken out. With it off, only that anchor keeps the reader's place.
    await withoutScrollAnchoring(page);
    // The reader is put on an entry in the middle of the history rather than at a fixed
    // offset, because the panel's properties take the top of its box and a fixed offset need
    // not reach an entry at all - and a reader on no entry is not what this row is about.
    const clip = await historyViewport(page);
    await scrollHistoryBy(
      page,
      await page.evaluate((clipBox) => {
        const entries = [
          ...document.querySelectorAll('[data-testid="issue-history"] [data-history-entry]'),
        ];
        const middle = entries[Math.floor(entries.length / 2)]?.getBoundingClientRect();
        if (middle === undefined) return 0;
        return middle.top + middle.height / 2 - (clipBox.top + clipBox.bottom) / 2;
      }, clip)
    );

    const entry = await readingAnchor(page, clip);
    const anchor = historyPanel(page).locator(`[data-history-entry="${entry}"]`);
    const before = (await anchor.boundingBox())?.y;
    expect(before, `the entry the reader is on, ${entry}, has no box`).toBeDefined();

    // The history is newest first, so the arrival lands above the reader.
    await createMessage(issue.key, { body: "Arrived while reading" }, attentionSession);

    await expect(historyPanel(page).getByText("Arrived while reading")).toBeAttached();
    await expect
      .poll(
        async () => (await anchor.boundingBox())?.y,
        "the entry the reader is on moved when an entry arrived above it"
      )
      .toBe(before);
  } finally {
    await context.close();
  }
});

// The history is drawn newest first, so a reader at the top of Details has the newest entry at
// the history's head with nothing to follow: the arrival is the history's first entry, and the
// panel has not moved.
test("a reader at the top of Details finds a new entry at the history's head", async ({
  browser,
}) => {
  const issue = await seedMessages("Attention test", 30);

  const context = await reader(browser, "alice");
  try {
    const page = await context.newPage();
    await openHistory(page, issue.key, "Existing message 29");
    await scrollHistoryTo(page, 0);

    await createMessage(issue.key, { body: "Arrived at the top" }, attentionSession);

    await expect(historyEntries(page).first()).toContainText("Arrived at the top");
    expect(await historyScrollTop(page), "an arrival moved Details under a reader at its top").toBe(
      0
    );
  } finally {
    await context.close();
  }
});

// The twin of the row above, at every other address of the page: the history follows nothing,
// so an entry arriving while the reader is on a filter leaves Details where they left it, and
// coming back leaves it there too.
test("a new entry arriving while the reader is on a filter leaves Details where they left it", async ({
  browser,
}) => {
  const issue = await seedMessages("Away from the history", 30);

  const context = await reader(browser, "alice");
  try {
    const page = await context.newPage();
    await page.setViewportSize(DESKTOP);
    await openHistory(page, issue.key, "Existing message 29");
    // Up to the panel's own top, off the history and onto the issue's properties.
    await scrollHistoryTo(page, 0);

    // Away to a view the page draws in its own column; Details, and the history in it, stay
    // drawn beside it - that is what makes the arrival below reach a history the reader is not
    // reading.
    const spec = presetToggle(page, "spec");
    await spec.click();
    await expect(spec).toHaveAttribute("aria-pressed", "true");
    expect(await historyScrollTop(page)).toBe(0);

    await createMessage(issue.key, { body: "Arrived while away" }, attentionSession);

    await expect(historyPanel(page).getByText("Arrived while away")).toBeAttached();
    expect(
      await historyScrollTop(page),
      "an entry arriving while the reader was on a filter moved Details"
    ).toBe(0);

    // And they come back to the place they left, with the new entry in the history.
    await page.goBack();
    await expect(page).toHaveURL(new RegExp(`/issues/${issue.key}/activity$`, "u"));
    await expect(historyPanel(page).getByText("Arrived while away")).toBeAttached();
    expect(
      await historyScrollTop(page),
      "coming back to the history did not leave the reader where they left it"
    ).toBe(0);
  } finally {
    await context.close();
  }
});

// Paging is the history's: the outline reads each of its three lists whole and offers no
// `Load older` at all. Older events land beneath the entries already on screen, so the entry
// the reader is on does not move; and the reader who presses on reaches the log's start, where
// the control goes (R2).
test("loading older history entries keeps the reader anchor", async ({ browser }) => {
  // One more than the history's 200-event page (`IssueHistory.tsx`), so a second page exists.
  const issue = await seedMessages("Older events", 205, "Older message");

  const context = await reader(browser, "alice");
  try {
    const page = await context.newPage();
    await gotoIssue(page, `/issues/${issue.key}/conversation`);
    // Every write of this issue is an agent's message, which the outline draws no row for, so
    // the tree is drawn with no row in it.
    await expectIssuePageDrawn(page);
    await expect(page.getByRole("button", { name: CONTROL_LABELS.load_older })).toHaveCount(0);

    await gotoIssue(page, `/issues/${issue.key}/activity`);
    // The browser's own scroll anchoring is turned off, as in the row of an arrival above, so
    // the poll below reads the page alone. The older page lands beneath the reader: what can
    // move the entry the reader is on is a history that draws something else in place of its
    // list while that page is read, or that hands the focus to a control off screen once it lands.
    await withoutScrollAnchoring(page);
    const loadOlder = historyPanel(page).getByRole("button", { name: CONTROL_LABELS.load_older });
    await expect(loadOlder).toBeVisible();
    await loadOlder.scrollIntoViewIfNeeded();
    await nextFrames(page);
    const clip = await historyViewport(page);
    const loadOlderBox = await loadOlder.boundingBox();
    expect(loadOlderBox).not.toBeNull();
    expect(loadOlderBox?.y ?? -1).toBeGreaterThanOrEqual(clip.top);
    expect((loadOlderBox?.y ?? 0) + (loadOlderBox?.height ?? 0)).toBeLessThanOrEqual(clip.bottom);

    const entry = await readingAnchor(page, clip);
    const anchor = historyPanel(page).locator(`[data-history-entry="${entry}"]`);
    const before = (await anchor.boundingBox())?.y;
    expect(before, `the entry the reader is on, ${entry}, has no box`).toBeDefined();
    await loadOlder.click();

    // The proof the older page landed is the oldest entry of all: the issue's own creation,
    // which only the page beneath the first 200 holds.
    await expect(historyEntries(page).filter({ hasText: "created the issue" })).toHaveCount(1);
    await expect
      .poll(
        async () => (await anchor.boundingBox())?.y,
        "loading older entries moved the entry the reader is on"
      )
      .toBe(before);

    // Pressed until it goes: each press reads the next page, and the last page takes it away.
    for (let presses = 1; await loadOlder.isVisible(); presses += 1) {
      expect(
        presses,
        `${CONTROL_LABELS.load_older} is still offered past the log's last page`
      ).toBeLessThan(3);
      const answered = page.waitForResponse((response) => {
        const url = new URL(response.url());
        return (
          url.pathname === `/api/v1/issues/${issue.key}/events` && url.searchParams.has("before")
        );
      });
      await loadOlder.click();
      await answered;
      await nextFrames(page);
    }
    await expect(
      historyPanel(page).getByRole("button", { name: CONTROL_LABELS.load_older }),
      `${CONTROL_LABELS.load_older} is still offered once the whole log is read`
    ).toHaveCount(0);
  } finally {
    await context.close();
  }
});

// The read mark draws one line in the history, "New since you last read", where the mark falls
// among its entries (`history-model.ts`): a mark that moves back adds that line under the
// entries the reader has not read, above the reader's place, so the reader's own entry has to
// stay where it was. The reader is parked over a gap between two entries, where
// `ViewportAnchor`'s probe has to fall back to the entry below. One day's entries are drawn flush,
// one rule down their left (the prototype's `.hev`), so the gap between two entries is where one
// day ends and the next opens with its line: the first ten comments and the issue's creation are
// a day old, and the reader opens that day.
async function verifyGapDetection(
  browser: Browser,
  viewport?: { height: number; width: number }
): Promise<void> {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Attention test" });
  const comments: string[] = [];
  for (let index = 0; index < 20; index += 1) {
    const comment = await createComment(
      issue.key,
      { body: `Gap comment ${index}` },
      attentionSession
    );
    comments.push(comment.id);
  }
  const events = await getIssueEvents(issue.key, { limit: 200 });
  const dayOld = new Set(comments.slice(0, 10));
  const yesterday = new Date(Date.now() - 24 * 60 * 60 * 1_000).toISOString();
  await Promise.all([
    ...[...dayOld].map((id) => setCreatedAt("comments", id, yesterday)),
    ...events
      .filter(
        (event) =>
          event.type === "issue.created" ||
          (event.type === "comment.created" && dayOld.has(event.payload.id))
      )
      .map((event) => setEventCreatedAt(event.id, yesterday)),
  ]);
  // Far beyond the issue's head, so nothing is unread and the history draws no line yet.
  await putIssueState(issue.key, { last_read_seq: 10_000 }, { login: "alice" });
  // The mark moves back to the twelfth comment below: the eight after it are unread, so the line
  // comes in today's list, two entries above the gap the reader is parked on.
  const twelfth = events.find(
    (event) => event.type === "comment.created" && event.payload.id === comments[11]
  )?.seq;
  if (twelfth === undefined) throw new Error("the seed recorded no twelfth comment");

  const context = await reader(browser, "alice");
  try {
    const page = await context.newPage();
    if (viewport !== undefined) {
      await page.setViewportSize(viewport);
    }
    await openHistory(page, issue.key, "Gap comment 19");
    const divider = historyPanel(page).getByRole("listitem", {
      name: CONTROL_LABELS.history_unread,
    });
    await expect(divider).toHaveCount(0);
    const dayBefore = historyPanel(page).getByRole("button", {
      name: new RegExp(`^${CONTROL_LABELS.history_yesterday} · \\d+$`),
    });
    await dayBefore.click();
    await expect(dayBefore).toHaveAttribute("aria-expanded", "true");
    await expect(historyPanel(page).getByText("Gap comment 0", { exact: true })).toBeVisible();
    // The reader's hand leaves the list, so the place the history keeps is the one at its centre.
    await handsOff(page);

    // Where the gap nearest the middle of the history sits, as an offset from the centre of the
    // box it is clipped to: the reader is moved by that offset, so the gap lands on the very
    // point `ViewportAnchor` probes (`rowAtCenter`).
    const clip = await historyViewport(page);
    const gap = await page.evaluate((clipBox) => {
      const entries = [
        ...document.querySelectorAll<HTMLElement>(
          '[data-testid="issue-history"] [data-history-entry]'
        ),
      ];
      const gaps = [];
      for (let index = 0; index < entries.length - 1; index += 1) {
        const upper = entries[index]?.getBoundingClientRect();
        const lower = entries[index + 1]?.getBoundingClientRect();
        if (upper !== undefined && lower !== undefined && lower.top > upper.bottom) {
          gaps.push({
            fromCenter: (upper.bottom + lower.top) / 2 - (clipBox.top + clipBox.bottom) / 2,
            index,
            lowerEntry: entries[index + 1]?.getAttribute("data-history-entry"),
          });
        }
      }
      if (gaps.length === 0) {
        return null;
      }
      const middle = entries.length / 2;
      gaps.sort((left, right) => Math.abs(left.index - middle) - Math.abs(right.index - middle));
      return gaps[0];
    }, clip);
    if (
      gap === null ||
      gap === undefined ||
      gap.lowerEntry === undefined ||
      gap.lowerEntry === null
    ) {
      throw new Error("no gap found between adjacent history entries");
    }

    await scrollHistoryBy(page, gap.fromCenter);
    const centre = await page.evaluate(
      ({ clipBox, root }) => {
        const history = document.querySelector(root);
        if (history === null) {
          return null;
        }
        const rect = history.getBoundingClientRect();
        const x = rect.left + rect.width / 2;
        const visibleTop = Math.max(rect.top, clipBox.top);
        const visibleBottom = Math.min(rect.bottom, clipBox.bottom);
        return { x, y: (visibleTop + visibleBottom) / 2 };
      },
      { clipBox: await historyViewport(page), root: HISTORY }
    );
    if (centre === null) {
      throw new Error("could not locate the history");
    }
    const centreHitsEntry = await page.evaluate((point) => {
      const element = document.elementFromPoint(point.x, point.y);
      return element !== null && element.closest("[data-history-entry]") !== null;
    }, centre);
    expect(centreHitsEntry).toBe(false);

    const lowerEntry = historyPanel(page).locator(`[data-history-entry="${gap.lowerEntry}"]`);
    const before = (await lowerEntry.boundingBox())?.y;
    expect(before, `the entry below the gap, ${gap.lowerEntry}, has no box`).toBeDefined();
    // The reader's mark moves back, so the history gains its line under the unread entries,
    // above the reader. Only a refocus re-reads it, as a tab left open overnight would.
    await putIssueState(issue.key, { last_read_seq: twelfth }, { login: "alice" });
    await focusAfterStaleness(page);

    await expect(divider).toHaveCount(1);
    await expect.poll(async () => (await lowerEntry.boundingBox())?.y).toBe(before);
  } finally {
    await context.close();
  }
}

test("a read mark moving under a reader parked in a history gap keeps the lower entry fixed", async ({
  browser,
}) => {
  await verifyGapDetection(browser);
});

test("a read mark moving in a narrow history keeps the lower entry fixed", async ({ browser }) => {
  await verifyGapDetection(browser, { height: 1024, width: 768 });
});

// ---------------------------------------------------------------------------------------------
// c-people-facts (1) and (2): `collaborators.e2e.ts` at `750e2716`.
// ---------------------------------------------------------------------------------------------

// An issue's agents stay reachable from its page once they stop (dispatch://INFRA-470): Details
// lists everyone who worked the issue, running or not, each agent one click from its live
// conversation.
//
// Fixture: a closed issue assigned to alice, which bob commented on and messaged before the close;
// one live Legion tester, two reviewer sessions of one role token (the old one started first) and a
// stopped implementer. Every role token is built from a Legion project token, `e2elegion`, that is
// not the Dispatch project.

const LEGION_PROJECT = "e2elegion";
const liveTester = "e2e-live-tester";
const oldReviewer = "e2e-old-reviewer";
const newReviewer = "e2e-new-reviewer";
const oldImplementer = "e2e-old-implementer";

async function seedCollaborators(): Promise<string> {
  await createProject({ key: "E2E", name: "Collaborators" });
  const issue = await createIssue({
    project: "E2E",
    spec: "The quick brown fox jumps over the lazy dog.",
    title: "Asked after it closed",
  });
  const issueKey = issue.key;
  await patchIssue(issueKey, { assignee: "alice" });
  await createComment(issueKey, { body: "Bob was here before the close" }, { login: "bob" });
  await createMessage(issueKey, { body: "Bob's question" }, { login: "bob" });
  await patchIssue(issueKey, { status: "done" }, { login: "bob" });

  const token = (role: LegionRole) => roleToken(LEGION_PROJECT, issueKey, role);
  const entry = (sessionId: string, role: LegionRole, startedAt: string) =>
    legionAgentEntry({
      issue: issueKey,
      legionProject: LEGION_PROJECT,
      role,
      sessionId,
      startedAt,
    });
  await setLiveSessions([
    {
      capabilities: ["aside", "btw", "steer"],
      roles: [token("tester")],
      session_id: liveTester,
      title: `Legion tester · ${issueKey}`,
    },
    { session_id: oldReviewer, title: `Legion reviewer · ${issueKey}` },
    { session_id: newReviewer, title: `Legion reviewer · ${issueKey}` },
    { session_id: oldImplementer, title: `Legion implementer · ${issueKey}` },
  ]);
  for (const stopped of [oldReviewer, newReviewer, oldImplementer]) {
    await setSessionRecord(stopped, "absent");
  }
  await setLegionAgents([
    entry(oldImplementer, "implementer", "2026-10-26T00:00:00Z"),
    entry(liveTester, "tester", "2026-10-26T01:00:00Z"),
    entry(oldReviewer, "reviewer", "2026-10-26T02:00:00Z"),
    entry(newReviewer, "reviewer", "2026-10-26T03:00:00Z"),
  ]);
  return issueKey;
}

/** The selector for an agent's own row name: the name `Row` marks `data-collaborator`
 *  (`IssueCollaboratorList.tsx`), never one that names it as another row's replacement. */
const rowNameSelector = (sessionId: string) =>
  `[data-collaborator] [data-agent-name="${sessionId}"]`;

// No `@phone` tag: this row starts at the project's own desktop width and narrows the window
// itself, so under `iphone` it would start from the wrong layout (packages/dispatch/AGENTS.md,
// "Phone acceptance").
test("the panel lists an issue's people and its Legion agents, running or not", async ({
  browser,
}) => {
  const issueKey = await seedCollaborators();
  const alice = await reader(browser, "alice");

  try {
    const page = await alice.newPage();
    await gotoIssue(page, `/issues/${issueKey}/collaborators`);
    // `/collaborators` opens the list inside Details (dispatch://INFRA-1196 R12, R19), and its
    // control says it is open.
    const collaborators = marginShell(page).getByRole("region", {
      exact: true,
      name: PROPERTY_LABELS.collaborators,
    });
    await expect(
      marginShell(page).getByRole("button", { exact: true, name: PROPERTY_LABELS.collaborators })
    ).toHaveAttribute("aria-expanded", "true");

    // People, with how each of them worked the issue, in the page's words for the server's
    // `reasons` tokens.
    const people = collaborators.getByRole("region", { name: "People" });
    await expect(people.getByRole("listitem").filter({ hasText: "alice" })).toContainText(
      CONTROL_LABELS.collaborator_reasons.assignee
    );
    await expect(people.getByRole("listitem").filter({ hasText: "bob" })).toContainText(
      CONTROL_LABELS.collaborator_reasons.posted
    );

    // The Legion agents, by role in workflow order (`LEGION_ROLES`), each role's newest first.
    const legion = collaborators.getByRole("region", { name: "Legion agents" });
    await expect(legion.getByRole("heading", { level: 3 })).toHaveText([
      "implementer",
      "tester",
      "reviewer",
    ]);
    const reviewers = legion.getByRole("region", { name: "reviewer" });
    const reviewerNames = reviewers.locator("[data-collaborator] [data-agent-name]");
    await expect(reviewerNames).toHaveCount(2);
    expect(
      await reviewerNames.evaluateAll((nodes) =>
        nodes.map((node) => node.getAttribute("data-agent-name"))
      )
    ).toEqual([newReviewer, oldReviewer]);
    // Only the old reviewer's own row names its replacement; the newest row names nobody else.
    const rowOf = (sessionId: string) =>
      reviewers.getByRole("listitem").filter({ has: page.locator(rowNameSelector(sessionId)) });
    await expect(rowOf(oldReviewer).locator(`[data-replaced-by="${newReviewer}"]`)).toContainText(
      `replaced by Legion reviewer · ${issueKey}`
    );
    await expect(rowOf(newReviewer).locator("[data-replaced-by]")).toHaveCount(0);

    // Each agent's running state, as the agents registry has it: the tester is live, and every
    // session the listener has lost reads as stopped.
    const agentRowName = (sessionId: string) => legion.locator(rowNameSelector(sessionId));
    await expect(agentRowName(liveTester)).toHaveAttribute("data-agent-state", "running");
    await expect(agentRowName(liveTester).getByRole("img", { name: "Running" })).toBeVisible();
    for (const stopped of [oldReviewer, newReviewer, oldImplementer]) {
      await expect(agentRowName(stopped)).toHaveAttribute("data-agent-state", "not-running");
      await expect(agentRowName(stopped).getByRole("img", { name: "Not running" })).toBeVisible();
    }

    // A session title is a whole sentence of its own, and the list holds four of them: the page
    // stays as wide as the window at a desktop width and at a phone's.
    await assertPageFits(page);
    await page.setViewportSize({ height: page.viewportSize()?.height ?? 800, width: 390 });
    await expect(agentRowName(oldReviewer)).toBeVisible();
    await assertPageFits(page);

    // A stopped agent is one click from the conversation it had, which names it.
    await agentRowName(oldReviewer).getByRole("link").click();
    await expect(page).toHaveURL(`/agents/${oldReviewer}/live`);
    await expect(
      page.getByRole("heading", { level: 1 }).locator(`[data-agent-name="${oldReviewer}"]`)
    ).toBeVisible();
    await page.goBack();
    await agentRowName(oldImplementer).getByRole("link").click();
    await expect(page).toHaveURL(`/agents/${oldImplementer}/live`);
    await expect(
      page.getByRole("heading", { level: 1 }).locator(`[data-agent-name="${oldImplementer}"]`)
    ).toBeVisible();
  } finally {
    await alice.close();
  }
});

// A registry the listener cannot read is an answer of its own, never a list that failed:
// Dispatch answers `legion_registry: "unavailable"` and still names the issue's people and the
// sessions its own events hold. So Details says the record could not be read, and alice and bob
// stand where they did.
test("the panel says so when Legion's record of the issue cannot be read", async ({ browser }) => {
  const issueKey = await seedCollaborators();
  await setLegionRegistryState("unreadable");
  const alice = await reader(browser, "alice");

  try {
    const page = await alice.newPage();
    const collaborators = page.waitForResponse(
      (response) =>
        response.request().method() === "GET" &&
        new URL(response.url()).pathname === `/api/v1/issues/${issueKey}/collaborators`
    );
    await gotoIssue(page, `/issues/${issueKey}/collaborators`);
    // What Details is showing is the server's answer: the read itself succeeded, and it named
    // the registry unavailable and held no agent.
    const answer = await collaborators;
    expect(answer.status()).toBe(200);
    expect(await answer.json()).toMatchObject({ agents: [], legion_registry: "unavailable" });

    const panel = marginShell(page).getByRole("region", {
      exact: true,
      name: PROPERTY_LABELS.collaborators,
    });
    await expect(panel.getByRole("status")).toHaveText(
      "Legion's record of this issue's agents could not be read."
    );

    // The people are Dispatch's own, so the notice takes none of them with it.
    const people = panel.getByRole("region", { name: "People" });
    await expect(people.getByRole("listitem").filter({ hasText: "alice" })).toContainText(
      CONTROL_LABELS.collaborator_reasons.assignee
    );
    await expect(people.getByRole("listitem").filter({ hasText: "bob" })).toContainText(
      CONTROL_LABELS.collaborator_reasons.posted
    );
    // And nothing stands in for the agents.
    await expect(panel.getByRole("region", { name: "Legion agents" })).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

// ---------------------------------------------------------------------------------------------
// c-people-facts (3) and (4): `asked-of.e2e.ts` and `asked-of-cli.e2e.ts` at `750e2716`.
// ---------------------------------------------------------------------------------------------

/** The session that asks these questions: an agent of the team, live, so the Agents page has a
 *  card to count its open asks on. */
const asker = {
  actor: {
    id: "e2e-asked-of-asker",
    kind: "session" as const,
    origin: { session_title: "Planner" },
  },
  as: "agent" as const,
};

/** The ask card's hand-on control, wherever that card is drawn: a native `<select>` scoped to the
 *  card's own ask. */
function handOnControl(scope: Locator | Page, askId: string): Locator {
  return scope.locator(`select[data-ask-hand-on="${askId}"]`);
}

/** Hands the ask the control belongs to over to `login`, as a reader does: the people it offers
 *  are read only once the reader reaches for it, so the control is focused first and the option
 *  waited for. */
async function handTo(control: Locator, login: string): Promise<void> {
  await control.focus();
  await expect(control.locator(`option[value="${login}"]`)).toHaveCount(1);
  await control.selectOption(login);
}

/** Every band heading the Inbox list is drawing, with its count. */
function bandHeadings(page: Page): Locator {
  return inboxList(page).locator("h2 button");
}

/** Takes the reader's hand - the focus and the pointer - off the list they read, the Inbox list
 *  or the history, so the place the list keeps is the row at its centre rather than one under
 *  the hand, and a row the server moved is drawn where it is (`interacted` in
 *  `web/src/features/shell/ViewportAnchor.tsx`). */
async function handsOff(page: Page): Promise<void> {
  await page.evaluate(() => {
    if (document.activeElement instanceof HTMLElement) document.activeElement.blur();
  });
  await page.mouse.move(2, 2);
}

// SLOW(>=2s): one hand-on read on nine surfaces at once - two Inboxes, two Agents pages, two
// issue pages, two Replies lists and a document - each of which must follow the move with no
// reload. The two issue pages are the new page, each in a context of its own with the new page
// turned on, so the Inbox, the Agents page and the document stay today's; each reads the ask's
// facts in Details, its row focused through the outline (`askFacts`), because the ask's own
// address is part N's.
test("a person hands an ask to another person and it moves between the two Inboxes", async ({
  browser,
}, testInfo) => {
  test.setTimeout(240_000);
  await setLiveSessions([{ session_id: asker.actor.id, title: "Planner" }]);
  // CORE names carol, a third person: every project names a default assignee, and making it
  // somebody other than these two readers keeps the two questions below out of both counts.
  await createProject({ key: "CORE", name: "Core" }, { login: "carol" });

  // A human's issue is assigned to its creator, so these two are alice's and bob's own.
  const hers = await createIssue({ project: "CORE", title: "Alice's rollout" }, { login: "alice" });
  expect(hers.assignee).toBe("alice");
  const moved = await createAsk(hers.key, { question: "Which rollout flag?" }, asker);
  await createAsk(hers.key, { question: "Which region goes first?" }, asker);
  const his = await createIssue({ project: "CORE", title: "Bob's migration" }, { login: "bob" });
  expect(his.assignee).toBe("bob");
  await createAsk(his.key, { question: "Drop the legacy column?" }, asker);

  // Two more open asks neither reader waits on - a project document's, and an `:::ask` block on
  // an issue the shared token created, which nobody holds, so both reach CORE's default.
  const runbook = await createProjectDocument(
    "CORE",
    { content: "# Runbook\n\nRestart, reseed, reopen.\n", name: "Runbook" },
    asker
  );
  const docAsk = await createArtifactAsk(
    runbook.artifact.id,
    { question: "Keep the runbook in the repository?" },
    asker
  );
  const blocked = await createIssue(
    {
      project: "CORE",
      spec: ':::ask{#rollout-decision urgency="med" multiple="false"}\nShip on Friday?\n\n- Ship\n- Hold\n:::\n',
      title: "Blocked decision",
    },
    asker
  );
  expect(blocked.assignee).toBe("carol");
  const blockAsk = await indexedBlockAsk(blocked.key, "rollout-decision");

  let alice: BrowserContext | undefined;
  let bob: BrowserContext | undefined;
  let aliceNew: BrowserContext | undefined;
  let bobNew: BrowserContext | undefined;
  try {
    alice = await asUser(browser, "alice");
    bob = await asUser(browser, "bob");
    aliceNew = await reader(browser, "alice");
    bobNew = await reader(browser, "bob");
    // Alice reads below the reading column's width, where every row draws its own card. Bob
    // reads above it, where the card is the reading column's.
    const alicePage = await alice.newPage();
    await alicePage.setViewportSize({ height: 900, width: 1100 });
    const bobPage = await bob.newPage();
    await bobPage.setViewportSize({ height: 900, width: READING_PANE_WIDTH });
    const aliceIssue = await aliceNew.newPage();
    await aliceIssue.setViewportSize(DESKTOP);
    const aliceAgents = await alice.newPage();
    const bobAgents = await bob.newPage();

    await alicePage.goto("/?group=waiting-on");
    await bobPage.goto("/?group=waiting-on");
    const aliceCard = await askFacts(aliceIssue, hers.key, moved.id);
    await aliceAgents.goto("/agents");
    await bobAgents.goto("/agents");

    // 1. The question is in alice's group of questions that need her, and in no group of bob's.
    await expect(inboxRow(alicePage, moved.id)).toBeVisible();
    await expect(alicePage.locator("[data-inbox-row]")).toHaveCount(2);
    await expect(bandHeadings(alicePage)).toHaveText(["Waiting on you (2)"]);
    await expect(alicePage.getByText(/^Blocked on you: 2 items/)).toBeVisible();
    await expect(
      agentRow(aliceAgents, asker.actor.id).getByRole("link", { name: "Needs you 2" })
    ).toBeVisible();

    await expect(inboxRow(bobPage, moved.id)).toHaveCount(0);
    await expect(bobPage.locator("[data-inbox-row]")).toHaveCount(1);
    await expect(bandHeadings(bobPage)).toHaveText(["Waiting on you (1)"]);
    await expect(bobPage.getByText(/^Blocked on you: 1 item/)).toBeVisible();
    await expect(
      agentRow(bobAgents, asker.actor.id).getByRole("link", { name: "Needs you 1" })
    ).toBeVisible();

    // 2. Alice hands it to bob with the control on her Inbox row.
    const rowControl = handOnControl(inboxRow(alicePage, moved.id), moved.id);
    await expect(rowControl).toHaveCount(1);
    await expect(rowControl).toHaveValue("alice");
    await expect(inboxRow(alicePage, moved.id)).toContainText("Asked of alice");
    await handTo(rowControl, "bob");
    await handsOff(alicePage);

    // 3. Without a reload, the row leaves her list and enters his, and every count follows.
    await expect(inboxRow(alicePage, moved.id)).toHaveCount(0);
    await expect(alicePage.locator("[data-inbox-row]")).toHaveCount(1);
    await expect(bandHeadings(alicePage)).toHaveText(["Waiting on you (1)"]);
    await expect(alicePage.getByText(/^Blocked on you: 1 item/)).toBeVisible();
    await expect(
      agentRow(aliceAgents, asker.actor.id).getByRole("link", { name: "Needs you 1" })
    ).toBeVisible();

    await expect(inboxRow(bobPage, moved.id)).toBeVisible();
    await expect(bobPage.locator("[data-inbox-row]")).toHaveCount(2);
    await expect(bandHeadings(bobPage)).toHaveText(["Waiting on you (2)"]);
    await expect(bobPage.getByText(/^Blocked on you: 2 items/)).toBeVisible();
    await expect(
      agentRow(bobAgents, asker.actor.id).getByRole("link", { name: "Needs you 2" })
    ).toBeVisible();

    for (const [who, page] of [
      ["alice", alicePage],
      ["bob", bobPage],
    ] as const) {
      const shot = testInfo.outputPath(`asked-of-inbox-${who}-${testInfo.project.name}.png`);
      await page.screenshot({ path: shot, fullPage: true });
      await testInfo.attach(`${who}'s Inbox after the hand-on (${testInfo.project.name})`, {
        contentType: "image/png",
        path: shot,
      });
    }

    // 4. The facts in Details name who it is asked of, who handed it on and when - the moment
    // the server stored - with no reload.
    const handedOn = (await getAsk(moved.id, { login: "alice" })).ask;
    expect(handedOn.asked_of).toBe("bob");
    expect(handedOn.asked_of_by).toBe("alice");
    expect(handedOn.waiting_on_person).toBe("bob");
    await expect(aliceCard).toBeVisible();
    await expect(aliceCard).toContainText("Asked of bob");
    await expect(aliceCard).toContainText("handed on by alice");
    await expect(aliceCard.locator(`time[datetime="${handedOn.asked_of_at}"]`)).toHaveCount(1);

    const bobIssue = await bobNew.newPage();
    await bobIssue.setViewportSize(DESKTOP);
    const bobIssueCard = await askFacts(bobIssue, hers.key, moved.id);
    await expect(bobIssueCard).toBeVisible();
    await expect(bobIssueCard).toContainText("Asked of bob");
    await expect(handOnControl(bobIssueCard, moved.id)).toHaveCount(1);
    await bobIssue.close();

    // 5. The control is on the card wherever the card renders an open ask: on the Inbox row
    // (pressed above), on the reading column's card, in the facts of the focused ask, on a
    // document's margin and in a document's ask block.
    await selectRow(bobPage, moved.id);
    const paneControl = handOnControl(inboxPane(bobPage), moved.id);
    await expect(paneControl).toHaveCount(1);
    await expect(paneControl).toHaveValue("bob");
    await expect(handOnControl(aliceCard, moved.id)).toHaveCount(1);

    const docPage = await alice.newPage();
    await docPage.goto(`/projects/CORE/documents/${runbook.artifact.slug}`);
    const marginCard = needsYouCards(docPage).getByTestId(`ask-${docAsk.id}`);
    await expect(marginCard).toBeVisible();
    await expect(handOnControl(marginCard, docAsk.id)).toHaveCount(1);
    await docPage.goto(`/issues/${blocked.key}`);
    const block = documentEditor(docPage).locator('[data-dispatch-ask-block="rollout-decision"]');
    const blockCard = block.locator(`article[data-testid="ask-${blockAsk.id}"]`);
    await expect(blockCard).toBeVisible();
    await expect(handOnControl(blockCard, blockAsk.id)).toHaveCount(1);
    await docPage.close();

    // 6. Each person's Replies list follows the same expression.
    await createComment(hers.key, { ask_id: moved.id, body: "Which flag do you mean?" });
    await createComment(hers.key, { ask_id: moved.id, body: "The rollout one." }, asker);
    await createComment(
      hers.key,
      { ask_id: moved.id, body: "I will take this one." },
      { login: "bob" }
    );
    await createComment(hers.key, { ask_id: moved.id, body: "Over to you, then." }, asker);

    const aliceReplies = await alice.newPage();
    const bobReplies = await bob.newPage();
    await aliceReplies.goto("/?type=reply");
    await bobReplies.goto("/?type=reply");
    await expect(inboxRow(aliceReplies, replyRowId(moved.id))).toBeVisible();
    await expect(inboxRow(bobReplies, replyRowId(moved.id))).toHaveCount(0);

    // 7. The same control on the reading column hands it back to the issue's assignee, and the
    // row returns to her list, each Replies list following with no reload.
    await handTo(paneControl, "alice");
    await handsOff(bobPage);
    await expect
      .poll(async () => (await getAsk(moved.id, { login: "alice" })).ask.asked_of ?? null)
      .toBeNull();
    expect((await getAsk(moved.id, { login: "alice" })).ask.waiting_on_person).toBe("alice");
    await expect(inboxRow(bobPage, moved.id)).toHaveCount(0);
    await expect(inboxRow(alicePage, moved.id)).toBeVisible();
    await expect(inboxRow(bobReplies, replyRowId(moved.id))).toBeVisible();
    await expect(inboxRow(aliceReplies, replyRowId(moved.id))).toHaveCount(0);
    await aliceReplies.close();
    await bobReplies.close();

    // 8. The control in the focused ask's facts hands it on and hands it back the same way.
    const issueControl = handOnControl(aliceCard, moved.id);
    await handTo(issueControl, "bob");
    await expect(inboxRow(bobPage, moved.id)).toBeVisible();
    await expect(inboxRow(alicePage, moved.id)).toHaveCount(0);
    await handTo(issueControl, "alice");
    await expect
      .poll(async () => (await getAsk(moved.id, { login: "alice" })).ask.asked_of ?? null)
      .toBeNull();
    await expect(inboxRow(alicePage, moved.id)).toBeVisible();
    await expect(inboxRow(bobPage, moved.id)).toHaveCount(0);

    // 9. Bob answers it, and the control is gone from the focused ask's facts the moment the
    // question is no longer open; the answer, and who gave it, is drawn on the ask's own row of
    // the outline. The replies of step 6 made that row a branch, which the outline folds to its
    // one line (part G's R1), so the row is read with its branch opened.
    await handTo(issueControl, "bob");
    await expect(inboxRow(bobPage, moved.id)).toBeVisible();
    await selectRow(bobPage, moved.id);
    const bobCard = inboxPane(bobPage).getByTestId(`ask-${moved.id}`);
    await bobCard.getByLabel("Your answer").fill("Read DISPATCH_ROLLOUT_FLAG.");
    await bobCard.getByRole("button", { name: "Answer", exact: true }).click();
    await handsOff(bobPage);
    await expect
      .poll(async () => (await getAsk(moved.id, { login: "bob" })).ask.state)
      .toBe("answered");
    await expect(aliceCard).toHaveAttribute("data-focused-node", moved.id);
    await expect(
      handOnControl(aliceCard, moved.id),
      "the hand-on control stayed on an answered question"
    ).toHaveCount(0);
    await unfoldEveryBranch(aliceIssue);
    await expect(nodeRow(aliceIssue, moved.id).first()).toContainText(
      "Read DISPATCH_ROLLOUT_FLAG."
    );
    await expect(nodeRow(aliceIssue, moved.id).first()).toContainText("bob");
    await expect(inboxRow(bobPage, moved.id)).toHaveCount(0);
    await expect(inboxRow(alicePage, moved.id)).toHaveCount(0);
  } finally {
    await alice?.close();
    await bob?.close();
    await aliceNew?.close();
    await bobNew?.close();
  }
});

/** The `- ` lines of one `dispatch open-asks` run, each one open ask. */
function printedRows(stdout: string): string[] {
  return stdout.split("\n").filter((line) => line.startsWith("- "));
}

/** The one printed row for an ask, by the deep link each row ends in. */
function rowFor(stdout: string, askId: string): string {
  const rows = printedRows(stdout);
  const row = rows.find((line) => (line.split(" · ").at(-1) ?? "").includes(askId));
  expect(row, `no open-asks row names ${askId}: ${rows.join(" | ")}`).toBeDefined();
  return row ?? "";
}

/** The three numbers the listing carries: its total, and the size of each of its two groups. */
function printedCounts(stdout: string): { agent: number; human: number; total: number } {
  const number = (pattern: RegExp): number => {
    const found = pattern.exec(stdout);
    expect(found, `${pattern.source} is not in: ${stdout}`).not.toBeNull();
    return Number(found?.[1] ?? -1);
  };
  return {
    agent: number(/^Waiting on agent \((\d+)\):$/m),
    human: number(/^Waiting on human \((\d+)\):$/m),
    total: number(/^(\d+) unanswered asks? you authored/m),
  };
}

/** The id of the ask a `dispatch ask` run opened, from the line it prints. */
function askedId(stdout: string): string {
  const id = /^Asked ([0-9a-f-]{36}) on /m.exec(stdout)?.[1];
  expect(id, `dispatch ask printed no ask id: ${stdout}`).toBeDefined();
  return id ?? "";
}

// SLOW(>=2s): five cold `bun` starts of the real `dispatch` command, each its own process,
// around one hand-on made in the new page's Details.
test("open-asks names the person each ask waits on, before and after a hand-on", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "chromium", "the command runs outside the browser");
  test.setTimeout(240_000);
  // carol creates the project, so she is its default assignee and the person a question on its
  // document waits on: a project document has no assignee of its own.
  await createProject({ key: "CORE", name: "Core" }, { login: "carol" });
  const issue = await createIssue(
    { project: "CORE", title: "Ship the rollout flag" },
    { login: "alice" }
  );
  expect(issue.assignee).toBe("alice");
  const runbook = await createProjectDocument("CORE", {
    content: "# Runbook\n\nRestart, reseed, reopen.\n",
    name: "Runbook",
  });

  const home = mkdtempSync(join(tmpdir(), "dispatch-asked-of-open-asks-"));
  const bob = await reader(browser, "bob");
  try {
    const opened = await runDispatch(
      [
        "ask",
        "--issue",
        issue.key,
        "--question",
        "The rollout reads one flag name and the runbook names another. Which one ships?",
      ],
      home
    );
    expect(opened.code, `${opened.stdout}${opened.stderr}`).toBe(0);
    const issueAsk = askedId(opened.stdout);

    const onDocument = await runDispatch(
      [
        "ask",
        "--project",
        "CORE",
        "--artifact",
        runbook.artifact.slug,
        "--question",
        "The runbook is the only copy of the restart steps. Keep it in the repository?",
      ],
      home
    );
    expect(onDocument.code, `${onDocument.stdout}${onDocument.stderr}`).toBe(0);
    const documentAsk = askedId(onDocument.stdout);

    // 1. The session's own listing names the issue's assignee on the first row and the project's
    // default on the second, both waiting on a human.
    const own = await runDispatch(["open-asks"], home);
    expect(own.code, `${own.stdout}${own.stderr}`).toBe(0);
    expect(rowFor(own.stdout, issueAsk)).toContain(" · asked of alice · ");
    expect(rowFor(own.stdout, documentAsk)).toContain(" · asked of carol · ");
    expect(printedCounts(own.stdout)).toEqual({ agent: 0, human: 2, total: 2 });

    // 2. The project-wide listing names the same two people and carries the same numbers.
    const project = await runDispatch(["open-asks", "--project", "CORE"], home);
    expect(project.code, `${project.stdout}${project.stderr}`).toBe(0);
    expect(rowFor(project.stdout, issueAsk)).toContain(" · asked of alice · ");
    expect(rowFor(project.stdout, documentAsk)).toContain(" · asked of carol · ");
    expect(printedCounts(project.stdout)).toEqual({ agent: 0, human: 2, total: 2 });

    // 3. Bob, who holds neither the issue nor the document, takes the first question himself
    // with the control the new page offers on it: in Details, beside the focused question's
    // facts (dispatch://INFRA-1196 R14, R19).
    const page = await bob.newPage();
    await page.setViewportSize(DESKTOP);
    const card = await askFacts(page, issue.key, issueAsk);
    await expect(card, "the focused question's facts name nobody it is asked of").toContainText(
      "Asked of alice"
    );
    await handTo(handOnControl(card, issueAsk), "bob");
    await expect(card).toContainText("Asked of bob");
    await expect
      .poll(async () => (await getAsk(issueAsk, { login: "bob" })).ask.asked_of)
      .toBe("bob");

    // 4. The same author session's listing names him from then on, with nothing else moved.
    const after = await runDispatch(["open-asks"], home);
    expect(after.code, `${after.stdout}${after.stderr}`).toBe(0);
    expect(rowFor(after.stdout, issueAsk)).toContain(" · asked of bob · ");
    expect(rowFor(after.stdout, documentAsk)).toContain(" · asked of carol · ");
    expect(printedCounts(after.stdout)).toEqual({ agent: 0, human: 2, total: 2 });
  } finally {
    await bob.close();
    rmSync(home, { force: true, recursive: true });
  }
});

// ---------------------------------------------------------------------------------------------
// c-people-facts (5) to (9): new rows.
// ---------------------------------------------------------------------------------------------

/** The session the People row counts and unsubscribes. */
const SUBSCRIBER = "e2e-details-subscriber";

// R5 of dispatch://INFRA-3948: "Subscribers" counts the sessions subscribed to the issue, opens
// the list of them, and an agent's Unsubscribe asks for Confirm before it removes the agent; only
// the dialog's Confirm tells the session.
test("the People section counts the subscribed agents and unsubscribes one after Confirm", async ({
  browser,
}) => {
  await phase(
    browser,
    async () => {
      await createProject({ key: "CORE", name: "Core" });
      const issue = await createIssue({ project: "CORE", title: "Subscribers in Details" });
      await setLiveSessions([{ session_id: SUBSCRIBER, title: "Worker (e2e)" }]);
      await setInterests([
        {
          session_id: SUBSCRIBER,
          topics: [
            `notifications.dispatch.issue.${issue.key}`,
            `notifications.dispatch.issue.${issue.key}.>`,
          ],
        },
      ]);
      return issue.key;
    },
    async (page, key) => {
      await gotoIssue(page, `/issues/${key}`);
      await openDetails(page);
      const details = marginShell(page);
      const subscribers = details.getByRole("button", {
        exact: true,
        name: `${PROPERTY_LABELS.subscribers}: 1`,
      });
      await expect(subscribers, "Details does not count the one subscribed agent").toBeVisible();
      await subscribers.click();
      const listed = details.getByRole("region", { name: "Subscribed agents" });
      await expect(listed.getByText("Worker (e2e)", { exact: true })).toBeVisible();

      await listed.getByRole("button", { name: "Unsubscribe" }).click();
      const dialog = page.getByRole("dialog", { name: "Unsubscribe" });
      await expect(dialog, "Unsubscribe asked for no Confirm").toBeVisible();
      await expect(dialog).toContainText(
        `Unsubscribe Worker (e2e) from ${key}? They will be told.`
      );
      expect(
        (await getUnsubscribeCalls()).some((call) => call.session_id === SUBSCRIBER),
        "the session was told before Confirm"
      ).toBe(false);
      await dialog.getByRole("button", { name: "Confirm" }).click();

      await expect(details.getByRole("region", { name: "Subscribed agents" })).toHaveCount(0);
      await expect
        .poll(async () =>
          (await getUnsubscribeCalls()).some((call) => call.session_id === SUBSCRIBER)
        )
        .toBe(true);
    }
  );
});

// R6 and R9 of dispatch://INFRA-3948: with Details shut the page reads neither the history's log,
// nor the collaborators, nor a row's references; opened, Details draws the facts and the history
// once each, inside it; the facts follow the focus; and Details closed and opened again keeps the
// facts of the row the reader was on.
test("the focused node's facts follow the focus, and Details reads nothing more until it opens", async ({
  browser,
}) => {
  await phase(
    browser,
    async () => {
      await createProject({ key: "CORE", name: "Core" });
      const issue = await createIssue({ project: "CORE", title: "Facts that follow the focus" });
      const first = await createComment(
        issue.key,
        { body: "The first row's words." },
        { login: "bob" }
      );
      const second = await createComment(
        issue.key,
        { body: "The second row's words." },
        { login: "bob" }
      );
      return { first: first.id, key: issue.key, second: second.id };
    },
    async (page, seeded) => {
      const read: string[] = [];
      page.on("request", (request) => read.push(new URL(request.url()).pathname));
      const settling = watchApiRequests(page);
      await gotoIssue(page, `/issues/${seeded.key}`);
      await expect(tree(page)).toBeVisible();
      await unfoldEveryBranch(page);
      await focusRow(page, seeded.first);
      await expect(marginShell(page)).toBeHidden();
      await expectLoadSettled(settling);
      for (const path of [
        `/api/v1/issues/${seeded.key}/events`,
        `/api/v1/issues/${seeded.key}/collaborators`,
        "/api/v1/references",
      ]) {
        expect(read, `the page read ${path} with Details shut`).not.toContain(path);
      }

      await openDetails(page);
      const facts = focusedNodeFacts(page);
      await expect(
        page.getByRole("heading", { exact: true, name: PROPERTY_LABELS.focused_node })
      ).toHaveCount(1);
      await expect(
        marginShell(page).getByRole("heading", { exact: true, name: PROPERTY_LABELS.focused_node })
      ).toHaveCount(1);
      await expect(historyPanel(page)).toHaveCount(1);
      await expect(marginShell(page).getByTestId("issue-history")).toHaveCount(1);
      await expect(
        marginShell(page).getByRole("heading", { exact: true, name: CONTROL_LABELS.history })
      ).toHaveCount(1);
      await expect(facts, "the facts are not the row the reader is on").toHaveAttribute(
        "data-focused-node",
        seeded.first
      );
      await expect(facts).toContainText("The first row's words.");
      // R9: the collaborators are read only once their list opens, and Details opens with it
      // shut. The history's log, read as Details opens, says Details' reads have gone out.
      const collaboratorsRead = `/api/v1/issues/${seeded.key}/collaborators`;
      await expect(
        marginShell(page).getByRole("button", { exact: true, name: PROPERTY_LABELS.collaborators })
      ).toHaveAttribute("aria-expanded", "false");
      await expect
        .poll(
          () => read.includes(`/api/v1/issues/${seeded.key}/events`),
          "Details drew the history and read no log"
        )
        .toBe(true);
      expect(read, "opening Details read the collaborators with their list shut").not.toContain(
        collaboratorsRead
      );

      // `j` moves the focus to the next row, and the facts follow it.
      await focusRow(page, seeded.first);
      await page.keyboard.press("j");
      await expect.poll(() => focusedNodeId(page)).toBe(seeded.second);
      await expect(facts, "the facts did not follow the focus to the next node").toHaveAttribute(
        "data-focused-node",
        seeded.second
      );
      await expect(facts).toContainText("The second row's words.");

      // Closed and opened again, Details keeps the facts of the row the reader was on.
      await closeDetails(page);
      await openDetails(page);
      await expect(facts).toHaveAttribute("data-focused-node", seeded.second);
      expect(read, "Details read the collaborators with their list shut").not.toContain(
        collaboratorsRead
      );
    }
  );
});

/** An issue with an agents' thread, a message an agent wrote that no person wrote in, dated a
 *  day back with its event, so the history folds the day it is in. */
async function seedAgentsThread(): Promise<{ key: string; message: string }> {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "The history's addresses" });
  await createComment(issue.key, { body: "A person's comment, a row of the outline." });
  for (let index = 0; index < 12; index += 1) {
    await createMessage(issue.key, { body: `An agent's earlier note ${index}.` }, attentionSession);
  }
  const message = await createMessage(
    issue.key,
    { body: "The agents' turn an address names." },
    attentionSession
  );
  for (let index = 0; index < 12; index += 1) {
    await createMessage(issue.key, { body: `An agent's later note ${index}.` }, attentionSession);
  }
  const created = (await getIssueEvents(issue.key, { limit: 200 })).find(
    (event) => event.type === "message.created" && event.payload.id === message.id
  );
  if (created === undefined) throw new Error(`${issue.key} recorded no event of ${message.id}`);
  const yesterday = new Date(Date.now() - 24 * 60 * 60 * 1_000).toISOString();
  await Promise.all([
    setCreatedAt("messages", message.id, yesterday),
    setEventCreatedAt(created.id, yesterday),
  ]);
  return { key: issue.key, message: message.id };
}

// R4 of dispatch://INFRA-3948: `t v` opens `/activity` with the history's heading in view and the
// focus in the history, and a Close there is the later word; `/collaborators` and `t o` open the
// People section the reader closed, with the collaborators' list open and the focus on its
// control; the address of an agents' turn opens Details at that turn, its folded day open, marked
// and on screen, once per arrival, and a turn that arrives while the history's log is held is
// reached at once; and Keys lists both keys.
test("t v and t o open Details at the history and at the collaborators, and an agents' turn's address marks it", async ({
  browser,
}) => {
  await phase(browser, seedAgentsThread, async (page, seeded) => {
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expect(tree(page)).toBeVisible();
    await expect(marginShell(page)).toBeHidden();

    await focusThePage(page);
    await page.keyboard.press("t");
    await page.keyboard.press("v");
    await expect(page, "t v did not open the history").toHaveURL(
      new RegExp(`/issues/${seeded.key}/activity$`)
    );
    await expect(marginShell(page)).toBeVisible();
    await expect
      .poll(() => focusInside(historyPanel(page)), "t v did not put the focus in the history")
      .toBe(true);
    await expect(
      historyPanel(page).getByRole("heading", { exact: true, name: CONTROL_LABELS.history }),
      "t v left the history's heading out of view"
    ).toBeInViewport();
    // At `/activity` the reader's Close is the later word.
    await closeDetails(page);
    await nextFrames(page);
    await expect(
      marginShell(page),
      "Details opened again at /activity after its Close"
    ).toBeHidden();

    // The reader closes People with its own control, and a reload keeps it closed.
    const people = marginSectionDisclosure(page, "people");
    const collaborators = marginShell(page)
      .locator('[data-details-row="collaborators"]')
      .getByRole("button", { exact: true, name: PROPERTY_LABELS.collaborators });
    await openDetails(page);
    await expect(people).toHaveAttribute("aria-expanded", "true");
    await people.click();
    await expect(people).toHaveAttribute("aria-expanded", "false");
    await page.reload();
    await issueRead(page);
    await openDetails(page);
    await expect(people, "a reload opened People the reader closed").toHaveAttribute(
      "aria-expanded",
      "false"
    );

    // `/collaborators` opens it, with the list open and the focus on its control.
    await gotoIssue(page, `/issues/${seeded.key}/collaborators`);
    await expect(people, "/collaborators left People closed").toHaveAttribute(
      "aria-expanded",
      "true"
    );
    await expect(collaborators).toHaveAttribute("aria-expanded", "true");
    await expect(
      collaborators,
      "/collaborators did not focus the Collaborators control"
    ).toBeFocused();

    // And so does `t o`, from the issue with People closed again.
    await people.click();
    await expect(people).toHaveAttribute("aria-expanded", "false");
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expect(marginShell(page)).toBeHidden();
    await focusThePage(page);
    await page.keyboard.press("t");
    await page.keyboard.press("o");
    await expect(page, "t o did not open the collaborators").toHaveURL(
      new RegExp(`/issues/${seeded.key}/collaborators$`)
    );
    await expect(people, "t o left People closed").toHaveAttribute("aria-expanded", "true");
    await expect(collaborators).toHaveAttribute("aria-expanded", "true");
    await expect(collaborators, "t o did not focus the Collaborators control").toBeFocused();

    // Keys names both.
    await focusThePage(page);
    await page.keyboard.press("?");
    const keys = page.getByRole("dialog", { name: "Keyboard shortcuts" });
    await expect(keys).toBeVisible();
    await expect(keys).toContainText(CONTROL_LABELS.open_history);
    await expect(keys).toContainText(CONTROL_LABELS.open_collaborators);
    await page.keyboard.press("Escape");
    await expect(keys).toBeHidden();

    // An agents' turn's own address, on a fresh load, opens Details there: the day before today
    // the turn is in open, the turn marked and on screen. A press on that day's line folds it.
    await gotoIssue(page, `/issues/${seeded.key}/messages/${seeded.message}`);
    await expect(marginShell(page), "the turn's address did not open Details").toBeVisible();
    const turn = historyItem(page, seeded.message);
    const dayBefore = historyPanel(page).getByRole("button", {
      name: new RegExp(`^${CONTROL_LABELS.history_yesterday} · \\d+$`),
    });
    await expect(dayBefore, "the turn's address left its day folded").toHaveAttribute(
      "aria-expanded",
      "true"
    );
    await expect(turn).toHaveAttribute("aria-current", "true");
    await expect(turn).toBeInViewport();
    await dayBefore.click();
    await expect(dayBefore, "a press on the opened day's line left it open").toHaveAttribute(
      "aria-expanded",
      "false"
    );
    await expect(turn).toHaveCount(0);
    // Once per arrival: the reader's Close is the later word.
    await closeDetails(page);
    await nextFrames(page);
    await expect(marginShell(page), "Details opened again after the reader closed it").toBeHidden();

    // A turn that arrives while the history's log is held in the app is reached at once: the
    // history reads the log at `/activity`, Details closes, the turn arrives, and a person's
    // comment in the outline links its address. Reached in the app - the router's own history
    // moving to it, as a link the router follows moves it, with no reload - the turn is marked
    // and on screen. The link in the comment's words is an anchor the browser follows, which
    // loads the page again, and it reaches the turn too.
    await gotoIssue(page, `/issues/${seeded.key}/activity`);
    await expect(historyPanel(page).getByText("An agent's later note 11.")).toBeVisible();
    await closeDetails(page);
    await page.evaluate(() => document.documentElement.setAttribute("data-loaded-once", "yes"));
    const arrived = await createMessage(
      seeded.key,
      { body: "A turn that arrived while the log was held." },
      attentionSession
    );
    const citing = await createComment(
      seeded.key,
      { body: `Read dispatch://${seeded.key}/message/${arrived.id} first.` },
      { login: "bob" }
    );
    const link = nodeRow(page, citing.id).locator(`a[href$="/messages/${arrived.id}"]`);
    await expect(link).toBeVisible();
    const address = new URL((await link.getAttribute("href")) ?? "", page.url()).pathname;
    await navigateInApp(page, address);
    await expect(page).toHaveURL(new RegExp(`/issues/${seeded.key}/messages/${arrived.id}$`));
    const arrival = historyItem(page, arrived.id);
    await expect(arrival, "the arrived turn's address did not mark it").toHaveAttribute(
      "aria-current",
      "true"
    );
    await expect(arrival).toBeInViewport();
    await expect(page.locator("html"), "reaching the address reloaded the page").toHaveAttribute(
      "data-loaded-once",
      "yes"
    );

    await closeDetails(page);
    await link.click();
    await expect(page).toHaveURL(new RegExp(`/issues/${seeded.key}/messages/${arrived.id}$`));
    await expect(marginShell(page), "the comment's link did not open Details").toBeVisible();
    await expect(arrival).toHaveAttribute("aria-current", "true");
    await expect(arrival).toBeInViewport();
  });
});

// R4 of dispatch://INFRA-3948: `/issues/<key>/activity` opens Details at the history. The page
// arrives with the log unread, so the history's heading has its box while the list under it is
// still the loading line, and Details cannot yet scroll it to the top: the landing must still
// leave the heading at the top of Details' view once the list is drawn. The log's answer is
// held for 1.5 s, which a cold load over a slow link gives, and the window is short, so what
// Details draws above the history is taller than its view. Found by the tester on a running
// build of 3bd86200 with INFRA-1196's copy: a load of /activity, and `t v` from a load with
// Details shut, left the heading at 828 of 900 px, under every property, with one entry's top
// in view and Details scrolled to the end of what it then held.
test("/activity read cold opens Details with the history's heading at the top of its view", async ({
  browser,
}) => {
  await phase(browser, seedAgentsThread, async (page, seeded) => {
    await page.setViewportSize({ height: 520, width: 1280 });
    await page.route(new RegExp(`/api/v1/issues/${seeded.key}/events`), async (route) => {
      const held = Promise.withResolvers<void>();
      setTimeout(held.resolve, 1_500);
      await held.promise;
      await route.continue();
    });
    await gotoIssue(page, `/issues/${seeded.key}/activity`);
    await expect(marginShell(page), "/activity did not open Details").toBeVisible();
    await expect(historyEntries(page).first(), "the history drew no entry").toBeAttached();
    await nextFrames(page);
    const heading = historyPanel(page).getByRole("heading", {
      exact: true,
      name: CONTROL_LABELS.history,
    });
    await expect
      .poll(
        async () => {
          const view = await historyViewport(page);
          const box = await heading.boundingBox();
          return box === null ? Number.POSITIVE_INFINITY : Math.round(box.y - view.top);
        },
        { message: "/activity left the history's heading below the top of Details' view" }
      )
      .toBeLessThanOrEqual(24);
  });
});

// R4 of dispatch://INFRA-3948: the address of an agents' turn opens Details with that turn marked
// and on screen. The marked turn is drawn whole (`CutWords whole`), so a long one is taller than
// Details' view: its head - the author and the time that is its address - must be what the
// reader lands on, never cut above the view. Found by the tester on a running build of
// 3bd86200 with INFRA-1196's copy: a turn of 338 words, 1,083 px tall, landed centred, its top at
// -65 px, its author and time out of view.
test("the address of a long agents' turn lands on its head", async ({ browser }) => {
  await phase(
    browser,
    async () => {
      await createProject({ key: "CORE", name: "Core" });
      const issue = await createIssue({ project: "CORE", title: "A long agents' turn" });
      await createComment(issue.key, { body: "A person's comment, a row of the outline." });
      const body = Array.from(
        { length: 60 },
        (_, index) => `Paragraph ${index + 1} of the agent's long turn.`
      ).join("\n\n");
      const message = await createMessage(issue.key, { body }, attentionSession);
      return { key: issue.key, message: message.id };
    },
    async (page, seeded) => {
      await gotoIssue(page, `/issues/${seeded.key}/messages/${seeded.message}`);
      await expect(marginShell(page), "the turn's address did not open Details").toBeVisible();
      const turn = historyItem(page, seeded.message);
      await expect(turn).toHaveAttribute("aria-current", "true");
      await nextFrames(page);
      const view = await historyViewport(page);
      const box = await turn.boundingBox();
      expect(box, "the marked turn has no box").not.toBeNull();
      expect(
        (box?.height ?? 0) > view.bottom - view.top,
        "the seeded turn is no taller than Details' view, so it proves nothing"
      ).toBe(true);
      expect(
        Math.round((box?.y ?? 0) - view.top),
        "the address of a long turn cut its head above Details' view"
      ).toBeGreaterThanOrEqual(-1);
      expect(
        Math.round((box?.y ?? 0) - view.top),
        "the address of a long turn left its head below Details' view"
      ).toBeLessThanOrEqual(view.bottom - view.top - 40);
    }
  );
});

// R2 of dispatch://INFRA-3948: at 767 px and below Details covers the page, so a link of the
// history that leaves it, and Reply, close Details; a turn's own time is the turn's address,
// which opens Details at it, so it leaves Details open, and never hides it for a moment.
test("on a phone a link of the history and Reply close Details", async ({ browser }) => {
  await phase(
    browser,
    async () => {
      await createProject({ key: "CORE", name: "Core" });
      const issue = await createIssue({ project: "CORE", title: "The history on a phone" });
      const message = await createMessage(
        issue.key,
        { body: "An agents' note with a Reply." },
        attentionSession
      );
      await uploadIssueFile(issue.key, {
        bytes: new Uint8Array(Buffer.from(ONE_PIXEL_PNG, "base64")),
        contentType: "image/png",
        name: "phone-shot.png",
      });
      return { key: issue.key, message: message.id };
    },
    async (page, seeded) => {
      await page.setViewportSize(PHONE);
      await gotoIssue(page, `/issues/${seeded.key}/activity`);
      await expect(marginShell(page)).toBeVisible();

      // A turn's time keeps Details open: the page records each value the aside of Details takes
      // for its `hidden` attribute and its class during the press, and keeps the hidden ones.
      await page.evaluate(() => {
        const aside = document.querySelector<HTMLElement>('[data-testid="desktop-margin-shell"]');
        if (aside === null) throw new Error("the page draws no Details");
        const record: string[] = [];
        const held = window as unknown as { detailsAside?: HTMLElement; detailsHidden?: string[] };
        held.detailsAside = aside;
        held.detailsHidden = record;
        new MutationObserver((changes) => {
          changes.forEach((change, index) => {
            const name = change.attributeName ?? "";
            // The value after this change is the next change's old value, or the one it has now.
            const next = changes.slice(index + 1).find((later) => later.attributeName === name);
            const value = next === undefined ? aside.getAttribute(name) : next.oldValue;
            const hidden =
              name === "hidden" ? value !== null : (value ?? "").split(/\s+/u).includes("hidden");
            if (hidden) record.push(`${name}="${value ?? ""}"`);
          });
        }).observe(aside, { attributeFilter: ["hidden", "class"], attributeOldValue: true });
      });
      await historyItem(page, seeded.message).locator("[data-history-time]").click();
      await expect(page).toHaveURL(new RegExp(`/messages/${seeded.message}$`));
      await expect(marginShell(page), "a turn's time closed Details").toBeVisible();
      await nextFrames(page);
      const pressed = await page.evaluate(() => {
        const held = window as unknown as { detailsAside?: HTMLElement; detailsHidden?: string[] };
        return {
          hidden: held.detailsHidden ?? null,
          same:
            held.detailsAside !== undefined &&
            held.detailsAside === document.querySelector('[data-testid="desktop-margin-shell"]'),
        };
      });
      expect(pressed.same, "the press on a turn's time drew Details anew").toBe(true);
      expect(
        pressed.hidden,
        "Details was hidden for a moment of the press on a turn's time"
      ).toEqual([]);

      // A file's link closes it.
      const file = historyPanel(page).getByRole("link", { exact: true, name: "phone-shot.png" });
      await file.click();
      await expect(marginShell(page), "the history's link left Details over the page").toBeHidden();

      // Reply closes it too.
      await gotoIssue(page, `/issues/${seeded.key}/activity`);
      await expect(marginShell(page)).toBeVisible();
      await historyPanel(page).locator(`[data-history-reply="${seeded.message}"]`).click();
      await expect(marginShell(page), "Reply left Details over the page").toBeHidden();
    }
  );
});

// R8 and R9 of dispatch://INFRA-3948: "What references this row" is read only when pressed, with
// Details open, and lists the comment that cites the focused statement by the page's word for a
// comment and the comment's first line, a link that focuses the comment's row; no row is named by
// an address, a slug or "spec block"; the issue draws no control; the focus moving closes the
// list; and a failed read says so, with Retry, after at most three requests.
test("what references the focused row opens from its facts, and each row leads to the citing node", async ({
  browser,
}) => {
  await phase(
    browser,
    async () => {
      await createProject({ key: "CORE", name: "Core" });
      const issue = await createIssue({
        project: "CORE",
        spec: ":::requirement{#r-cited}\nThe history is drawn in Details.\n:::\n",
        title: "What references a statement",
      });
      const comment = await createComment(
        issue.key,
        { body: `Bob checks dispatch://${issue.key}/spec#b-r-cited against the page.` },
        { login: "bob" }
      );
      const spec = ((await getIssue(issue.key)).artifacts ?? []).find(
        (artifact) => artifact.primary
      );
      if (spec === undefined) throw new Error(`${issue.key} holds no spec`);
      return {
        comment: comment.id,
        key: issue.key,
        reference: `dispatch://${issue.key}/spec#b-r-cited`,
        specSlug: spec.slug,
        statement: blockNode(spec.slug, "r-cited"),
      };
    },
    async (page, seeded) => {
      // The reads of what references the statement, by its own address: the page reads others
      // of `/references` (Blocks', Referenced by's), which are not this list's.
      const references: string[] = [];
      page.on("request", (request) => {
        const url = new URL(request.url());
        if (
          url.pathname === "/api/v1/references" &&
          url.searchParams.get("to") === seeded.reference
        ) {
          references.push(request.url());
        }
      });

      // The issue itself draws no control: Details' "Referenced by" row holds its list. The
      // focus is on no row, so the facts are the issue's.
      await gotoIssue(page, `/issues/${seeded.key}`);
      await expect(tree(page)).toBeVisible();
      await focusThePage(page);
      await openDetails(page);
      await expect(focusedNodeFacts(page)).toHaveAttribute("data-focused-node", seeded.key);
      const control = focusedNodeFacts(page).getByRole("button", {
        exact: true,
        name: NODE_REFERENCES_WORDS.what_references,
      });
      await expect(control, "the issue draws a control of its own").toHaveCount(0);
      await closeDetails(page);

      const facts = await focusedRowFacts(page, seeded.statement);
      await expect(control).toHaveAttribute("aria-expanded", "false");
      expect(references, "the references were read before the reader asked").toEqual([]);
      await control.click();
      await expect(control).toHaveAttribute("aria-expanded", "true");
      const rows = facts.locator("[data-node-references] li");
      await expect(rows).toHaveCount(1);
      expect(references, "the references were not read once").toHaveLength(1);
      await expect(rows.first()).toContainText(NODE_REFERENCES_WORDS.kinds.comment);
      // The comment's first line, from its tree, with the reference it writes named as the
      // outline names it: by the statement's own words.
      await expect(rows.first()).toContainText(
        "Bob checks The history is drawn in Details. against the page."
      );
      const text = (await rows.first().textContent()) ?? "";
      for (const name of ["dispatch://", seeded.specSlug, "spec block", "#b-"]) {
        expect(text, `a row of what references the statement names it by "${name}"`).not.toContain(
          name
        );
      }

      // The focus moving to another row closes the list.
      await focusRow(page, seeded.statement);
      await page.keyboard.press("j");
      await expect.poll(() => focusedNodeId(page)).not.toBe(seeded.statement);
      await page.keyboard.press("k");
      await expect.poll(() => focusedNodeId(page)).toBe(seeded.statement);
      await expect(control, "the list stayed open when the focus moved").toHaveAttribute(
        "aria-expanded",
        "false"
      );

      // The row's link focuses the citing comment's row.
      await control.click();
      await rows.first().getByRole("link").click();
      await expect.poll(() => focusedNodeId(page)).toBe(seeded.comment);

      // A read that fails says so, with Retry, after at most three requests.
      await page.route("**/api/v1/references?**", (route) =>
        new URL(route.request().url()).searchParams.get("to") === seeded.reference
          ? route.fulfill({
              body: JSON.stringify({ code: "INTERNAL", error: "refused" }),
              contentType: "application/json",
              status: 500,
            })
          : route.fallback()
      );
      references.length = 0;
      const again = await rowFacts(page, seeded.key, seeded.statement);
      await again
        .getByRole("button", { exact: true, name: NODE_REFERENCES_WORDS.what_references })
        .click();
      await expect(again).toContainText(ANSWER_LINES.references_unreadable);
      await expect(
        again.getByRole("button", { exact: true, name: CONTROL_LABELS.retry })
      ).toBeVisible();
      expect(
        references.length,
        "a failed read was asked more than three times"
      ).toBeLessThanOrEqual(3);
    }
  );
});
