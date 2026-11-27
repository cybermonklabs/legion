/** the history reads the issue's event log and its live stream, the people rows read the fake
 *  listener's subscriptions and Legion's record, and every row signs in for real with the new
 *  page turned on, by that login's stored preference or by the issue's own address */

import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { HANDOFF_BODIES_MAX_IDS, type LegionRole, roleToken } from "@legion/contracts";

import type { DispatchEvent, IssueAskThreadRow } from "../web/src/api/types";
import { conversationItemId, pinnedItemMarker } from "../web/src/features/issue/pins";
import { KEY_REPORTS, PROPERTY_LABELS } from "../web/src/features/issue-page/page-structure";
import {
  ANSWER_LINES,
  CONTROL_LABELS,
  DELIVERY_STATE_WORDS,
  NODE_REFERENCES_WORDS,
  NODE_TYPE_NAMES,
  PHASE_WORDS,
  sentenceCase,
} from "../web/src/features/issue-page/page-words";
import { PAGE_SIZE, showMoreName } from "../web/src/features/issue-page/show-more-fixture";
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
  deleteMessage,
  editArtifact,
  editAsk,
  editComment,
  followAsk,
  getAsk,
  getIssue,
  getIssueCoverage,
  getIssueEvents,
  getMyState,
  patchIssue,
  putArchitectureSource,
  putIssueState,
  putStep,
  requestApproval,
  resolveAsk,
  resolveComment,
  reviewArtifact,
  setGatedSteps,
  syncArchitectureSource,
  uploadIssueFile,
  withOpenedAsk,
} from "./api";
import { runDispatch } from "./dispatch-cli";
import { documentEditor, indexedBlockAsk, needsYouCards } from "./editor";
import { seedFakeGithub } from "./fake-github-helpers";
import { nextFrames } from "./frames";
import {
  historyScrollRange,
  historyScrollTop,
  historyViewport,
  scrollHistoryBy,
  scrollHistoryTo,
} from "./history-scroll";
import { backToList, inboxList, inboxPane, inboxRow, selectRow } from "./inbox-pane";
import { focusAfterStaleness } from "./inbox-pane-scroll";
import { replyRowId } from "./inbox-threads";
import { seedConversationState } from "./issue-layout-seed";
import { asPlanner, PROJECT } from "./issue-layout-seed-support";
import { requirementText } from "./issue-layout-seed-text";
import {
  closeDetails,
  gotoIssue,
  iconRail,
  issuePage,
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
  expectOpeningLanded,
  focusRow,
  focusThePage,
  nodeRow,
  PHONE,
  rowWords,
  tree,
  unfoldEveryBranch,
} from "./issue-page-g";
import {
  detailsRow,
  filesList,
  marginSection,
  marginSectionDisclosure,
  pinsList,
} from "./issue-page-h";
import { focusedNodeId, focusInside, openEveryDeeperBranch } from "./issue-page-i";
import {
  askFacts,
  bandPlaces,
  daemonOf,
  drawnInstant,
  fieldsPastTheirEntry,
  focusedNodeFacts,
  focusedRowFacts,
  handoffFieldsOf,
  handoffFoldControl,
  handoffLineOf,
  historyDayOf,
  historyDaysNotOpen,
  historyEntries,
  historyHeading,
  historyItem,
  historyNextPage,
  historyPanel,
  historyRuns,
  historyShowMore,
  historyShowMoreState,
  holdBodiesReads,
  isOlderPageRead,
  isReadMarkRead,
  newestHistoryEntry,
  olderPageAnswered,
  putPlan,
  putStepExperiment,
  referencesTo,
  rowFacts,
  runValuesUnread,
  type StepsReads,
  seededHistoryEntry,
  stepRun,
  stepTask,
  taskRuns,
  watchStepsReads,
  whollyInHistoryView,
} from "./issue-page-m";
import { withIssuePage } from "./issue-page-option";
import { approvalChip, viewportAt } from "./issue-page-p";
import { tabWalk } from "./issue-page-v";
import { seedLegionState } from "./legion-state-seed";
import { withoutScrollAnchoring } from "./mermaid-helpers";
import { ONE_PIXEL_PNG } from "./one-pixel-png";
import { sql } from "./psql";
import { READING_PANE_WIDTH } from "./reading-pane";
import { setCreatedAt, setEventCreatedAt, sqlLiteral } from "./seed";
import { navigateInApp } from "./sends";
import {
  type Browser,
  type BrowserContext,
  expect,
  type Locator,
  type Page,
  type Response,
  test,
} from "./test";
import { expectLoadSettled, watchApiRequests } from "./traffic";
import { assertPageFits } from "./unclipped";
import { asUser } from "./users";

/**
 * Details' history, its people rows, the facts of the focused row and what references it, on the
 * new issue page.
 *
 * The rows of c-history and of c-people-facts.
 * Most are copies of rows the source rewrote for the new page while it was the only page: the
 * checkpoint's `issue-layout.e2e.ts` row, and the rows of `activity.e2e.ts`, `handoffs.e2e.ts`,
 * `collaborators.e2e.ts`, `asked-of.e2e.ts`, `asked-of-cli.e2e.ts` and `attention.e2e.ts` at
 * `750e2716`, whose files stay as `main` has them for today's page. Each copy signs its reader
 * in with the new page turned on (`reader`, `withIssuePage`) and opens it with `gotoIssue`; an
 * item's own address does not put the focus on its node yet, so a copy that read a
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

/**
 * Presses the history's Show more by `press` until it reaches `end`: `reads`, where the control
 * says "<n>+" with `n` at most `PAGE_SIZE` and its press reads the log's next page before it
 * draws (`historyNextPage`), or `gone`, where no control is left under the last entry, so the
 * history draws the whole log of `key`, which holds `events` events. Each press draws the next
 * entries, waited for until more are drawn; a press that reads the next page first draws them
 * once that page's answer has landed. A control that says no number fails: the history says how
 * many entries are left while any may be. Each press draws an entry, so a control still offered
 * after as many presses as the log has entries and pages fails as offered past the log's end.
 */
async function readHistoryDown(
  page: Page,
  key: string,
  events: number,
  press: (control: Locator) => Promise<void>,
  end: "gone" | "reads"
): Promise<void> {
  const most = Math.ceil(events / PAGE_SIZE) + Math.ceil(events / 200) + 1;
  for (let presses = 0; ; presses += 1) {
    const state = await historyShowMoreState(page);
    if (state === end || state === "gone") return;
    expect(
      state,
      `${CONTROL_LABELS.show_more} says no number while entries of the log may be left`
    ).not.toBe("uncounted");
    if (state === "reading") {
      await expect(
        historyPanel(page).locator('button[aria-busy="true"]'),
        "the log's next page is read for ever"
      ).toHaveCount(0);
      continue;
    }
    expect(presses, `${CONTROL_LABELS.show_more} is still offered past the log's end`).toBeLessThan(
      most
    );
    // The entries drawn as the press is made: the press draws at least one more.
    const drawn = await historyEntries(page).count();
    const answered = state === "reads" ? olderPageAnswered(page, key) : undefined;
    await press(historyShowMore(page));
    await answered;
    await expect
      .poll(() => historyEntries(page).count(), {
        message: `${CONTROL_LABELS.show_more} drew no entry`,
      })
      .toBeGreaterThan(drawn);
  }
}

/** The Legion daemon of `CORE`, the project most rows seed their issue in. */
const asDaemon = daemonOf("CORE");

/** The reviewer agent row (1)'s planner writes to, and which answers it. */
const asReviewerAgent = {
  actor: { id: "e2e-history-reviewer", kind: "session" as const },
  as: "agent" as const,
};

/** The words of the person's reply in the daemon's thread of row (1), by which the row knows the
 *  page's read of the issue's messages that brings it. */
const PERSON_IN_DAEMON_THREAD = "A person answers the daemon in its thread.";

/** The whole issue of `seedConversationState`, with what the history holds whole and what it
 *  holds not: a thread the issue's Legion daemon began, with an agent's reply in it; an agent's
 *  note to the issue's people, a message with no target; and a thread between two agents, whose
 *  root the planner sends to the reviewer's role and which the reviewer answers. */
async function seedHistoryThreads() {
  const seeded = await seedConversationState();
  const root = await createMessage(
    seeded.key,
    { body: "The daemon moved the planner's claim to its new session." },
    daemonOf(PROJECT)
  );
  const reply = await createMessage(
    seeded.key,
    { body: "The new planner session holds the claim now.", in_reply_to: root.id },
    asPlanner
  );
  const note = await createMessage(
    seeded.key,
    { body: "A note to the issue's people: the plan waits on its review." },
    asPlanner
  );
  const toReviewer = await createMessage(
    seeded.key,
    {
      body: "Reviewer, the plan's second task waits on you.",
      delivery: "btw",
      target: "role:reviewer",
    },
    asPlanner
  );
  const answer = await createMessage(
    seeded.key,
    { body: "Read it: the second task stands.", in_reply_to: toReviewer.id },
    asReviewerAgent
  );
  return {
    ...seeded,
    betweenAgents: { reply: answer.id, root: toReviewer.id },
    daemonThread: { reply: reply.id, root: root.id },
    note: note.id,
  };
}

/**
 * Watches, from the call on, the answers of the page's reads that the outline's posts come from:
 * the issue's messages read (`GET /api/v1/issues/<key>/messages`), and the tree read with no view
 * (`GET /api/v1/issues/<key>/tree`) where the page draws its outline from one. `stop` ends the
 * watch and gives the body of the read the outline is drawn from, as text: the newest tree read's
 * where the page made one, else the newest messages read's, and `undefined` where it made
 * neither. A post's id in that text says the read returned the post.
 */
function watchOutlineReads(page: Page, key: string): { stop: () => Promise<string | undefined> } {
  const reads: { body: Promise<string>; tree: boolean }[] = [];
  const answered = (response: Response) => {
    const url = new URL(response.url());
    if (response.request().method() !== "GET") return;
    const tree = url.pathname === `/api/v1/issues/${key}/tree` && !url.searchParams.has("view");
    if (tree || url.pathname === `/api/v1/issues/${key}/messages`) {
      reads.push({ body: response.text(), tree });
    }
  };
  page.on("response", answered);
  return {
    stop: async () => {
      page.off("response", answered);
      const bodies = await Promise.all(
        reads.map(async (read) => ({ body: await read.body, tree: read.tree }))
      );
      const trees = bodies.filter((read) => read.tree);
      return (trees.length > 0 ? trees : bodies).at(-1)?.body;
    },
  };
}

// c-conversation-state. The conversation is the outline and nothing else: each seeded comment
// and message a person took part in is one node, nothing offers a choice between words,
// questions, pinned and everything, and Details draws none of them. An agent's note to the
// issue's people and a thread between two agents are no entry of the history, and nodes of the
// outline only where the read the outline is drawn from returns them, as the issue's messages
// read does and the issue's tree read does not: so is the seed's message by the session
// `legion-daemon`, which is no project's daemon (`legion-daemon:<PROJECT>`). The history in the
// panel draws what no node does - the status change, the claim, both handoffs, the three
// uploads, the thread the daemon began, an agent's status note and the two comments agents wrote
// to each other, each once - and no event of a node, no version of a document, no result against
// a check and no child's move. A person's reply leaves the daemon's thread whole in the history,
// the reply among its turns, and moves the agents' thread into the outline. The replies are sent
// by the API: the page draws no box at the foot of the outline yet.
test("the conversation is the outline and the history is in the panel", async ({ browser }) => {
  // The whole issue's seed and two loads of its page, the history read to its end between.
  test.setTimeout(240_000);
  await phase(browser, seedHistoryThreads, async (page, seeded) => {
    // The asks, comments and messages arrive by reads of their own after the tree, so the load
    // is waited out until its reads settle before the branches they draw are opened.
    const settling = watchApiRequests(page);
    const outlineReads = watchOutlineReads(page, seeded.key);
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectLoadSettled(settling);
    const outlineRead = await outlineReads.stop();
    await expectIssuePageDrawn(page);
    await unfoldEveryBranch(page);
    await openEveryDeeperBranch(page);
    const turns = [
      ...seeded.comments.threads,
      ...seeded.comments.replies,
      seeded.messages.root,
      seeded.messages.reply,
      seeded.messages.replyToReply,
      seeded.daemonMessage,
      seeded.note,
      seeded.betweenAgents.root,
      seeded.betweenAgents.reply,
    ];
    // An agent's untargeted notes, the daemon's message among them, and the thread between two
    // agents, which the outline draws only where its read returns them; every other turn is of a
    // thread a person took part in.
    const agentsOnly = [
      seeded.daemonMessage,
      seeded.note,
      seeded.betweenAgents.root,
      seeded.betweenAgents.reply,
    ];
    for (const id of turns.filter((turn) => !agentsOnly.includes(turn))) {
      await expect(nodeRow(page, id), `${id} is not one node of the outline`).toHaveCount(1);
    }
    expect(outlineRead, "the page read neither the issue's messages nor its tree").toBeDefined();
    for (const id of agentsOnly) {
      const returned = outlineRead?.includes(id) === true;
      await expect(
        nodeRow(page, id),
        returned
          ? `${id} is not one node of the outline, though the page's read returned it`
          : `${id} is in the outline, though the page's read did not return it`
      ).toHaveCount(returned ? 1 : 0);
    }
    const daemonTurns = [seeded.daemonThread.root, seeded.daemonThread.reply];
    for (const id of daemonTurns) {
      await expect(nodeRow(page, id), `${id} of the daemon's thread is in the outline`).toHaveCount(
        0
      );
    }
    await expect(page.locator("[data-conversation-state], [data-turn]")).toHaveCount(0);
    for (const name of ["Words", "Questions", "Pinned", "Everything"]) {
      await expect(page.getByRole("tab", { exact: true, name })).toHaveCount(0);
      await expect(page.getByRole("radio", { exact: true, name })).toHaveCount(0);
    }
    await openDetails(page);
    // The newest entry, the daemon's thread, says the history has drawn the log it reads.
    await expect(await newestHistoryEntry(page)).toHaveAttribute(
      "data-history-entry",
      `thread:${seeded.daemonThread.root}`
    );
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
    // The history draws its entries 20 at a time and pages the log 200 events at a time: Show
    // more is pressed until the whole log is read and drawn.
    await newestHistoryEntry(page);
    const events = await allEvents(seeded.key);
    await readHistoryDown(page, seeded.key, events.length, (control) => control.click(), "gone");
    // With the whole log read and drawn, no turn of a thread a person took part in is drawn here,
    // and neither is an agent's note or a turn between agents, whatever the outline's read held.
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
    const outlined = new Set(turns);
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
          outlined.has(event.payload.id))
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
      [seeded.daemonThread.root, "The daemon moved the planner's claim"],
      [seeded.daemonThread.reply, "The new planner session holds the claim"],
      [seeded.statusNote, "Status: the plan is drafted"],
      [seeded.agentComments.root, "the plan's third task needs a second look"],
      [seeded.agentComments.reply, "the third task stands as written"],
    ] as const) {
      await expect(historyItem(page, id), `${words} is not drawn once`).toHaveCount(1);
      await expect(historyItem(page, id)).toContainText(words);
      await expect(historyPanel(page).locator(`[data-history-reply="${id}"]`)).toHaveCount(1);
    }
    const daemonThread = historyPanel(page).locator(
      `[data-history-entry="thread:${seeded.daemonThread.root}"]`
    );
    await expect(
      daemonThread.locator(`[data-history-item="${seeded.daemonThread.reply}"]`),
      "the agent's reply is not a turn of the daemon's thread"
    ).toHaveCount(1);
    const uploads = historyPanel(page).locator('[data-history-uploads="3"]');
    await expect(uploads, "the three uploads are not one entry").toHaveCount(1);
    for (const name of seeded.uploads) {
      await expect(uploads.getByRole("link", { exact: true, name })).toHaveCount(1);
    }

    // A person replies in the daemon's thread: the thread stays in the history, whole, with the
    // person's reply among its turns, once the page's read of the issue's messages brings it.
    const messagesRead = page.waitForResponse(
      async (response) =>
        new URL(response.url()).pathname === `/api/v1/issues/${seeded.key}/messages` &&
        (await response.text()).includes(PERSON_IN_DAEMON_THREAD)
    );
    const person = await createMessage(
      seeded.key,
      { body: PERSON_IN_DAEMON_THREAD, in_reply_to: seeded.daemonThread.root },
      { login: "alice" }
    );
    await messagesRead;
    await nextFrames(page);
    await expect(
      historyItem(page, seeded.daemonThread.root),
      "the daemon thread left the history when a person replied"
    ).toHaveCount(1);
    await expect(
      daemonThread.locator(`[data-history-item="${person.id}"]`),
      "the person's reply is not among the daemon thread's turns"
    ).toContainText(PERSON_IN_DAEMON_THREAD);
    await expect(
      daemonThread.locator(`[data-history-item="${seeded.daemonThread.reply}"]`)
    ).toHaveCount(1);
    for (const id of [...daemonTurns, person.id]) {
      await expect(
        nodeRow(page, id),
        `${id} of the daemon's thread is in the outline once a person replied`
      ).toHaveCount(0);
    }

    // A person's reply to one of the agents' comments moves that thread into the outline.
    await historyPanel(page).locator(`[data-history-reply="${seeded.agentComments.root}"]`).click();
    await createComment(
      seeded.key,
      { body: "A person joins the agents' thread.", reply_to: seeded.agentComments.root },
      { login: "alice" }
    );
    await expect(
      historyItem(page, seeded.agentComments.root),
      "the agents' thread stayed in the history once a person wrote in it"
    ).toHaveCount(0);
    await expect(
      nodeRow(page, seeded.agentComments.root),
      "the agents' thread a person wrote in is not drawn in the outline"
    ).toHaveCount(1);
    // The daemon's thread is in the history still.
    await expect(historyItem(page, seeded.daemonThread.root)).toHaveCount(1);
  });
});

// ---------------------------------------------------------------------------------------------
// c-history (2) to (6): `activity.e2e.ts` at `750e2716`.
// ---------------------------------------------------------------------------------------------

const activityAgent = {
  actor: { id: "e2e-activity-agent", kind: "session" as const },
  as: "agent" as const,
};

// The issue's history is the list in Details that `/activity` opens: every event no row of the
// outline draws, and the threads the history holds, whole - two agents' posts and a thread the
// Legion daemon began.
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
  const message = await createMessage(issue.key, { body: "A message thread." }, asDaemon);
  await patchIssue(issue.key, { status: "in_progress" });
  await claimIssue(issue.key, activityAgent);
  await createIssueArtifact(issue.key, { content: "# Note\n", name: "note.md" });

  const alice = await reader(browser, "alice");
  try {
    const page = await alice.newPage();
    // The asks arrive by a read of their own after the tree, so the load is waited out until its
    // reads settle before the branches they draw are opened.
    const settling = watchApiRequests(page);
    await gotoIssue(page, `/issues/${issue.key}/activity`);
    await expectLoadSettled(settling);
    const history = historyPanel(page);
    await expect(history).toBeVisible();
    const rows = historyEntries(page);
    // The issue's own events, each a line of its own, read by its event (`seededHistoryEntry`).
    const events = await getIssueEvents(issue.key, { limit: 100 });
    const lines: [string, (event: DispatchEvent) => boolean][] = [
      ["updated the issue", (event) => event.type === "issue.updated"],
      ["claimed the issue", (event) => event.type === "issue.claimed"],
      [
        "added note.md",
        (event) => event.type === "artifact.created" && event.payload.artifact.name === "note.md",
      ],
    ];
    for (const [description, drawn] of lines) {
      const event = events.find(drawn);
      if (event === undefined) throw new Error(`the seed recorded no event of "${description}"`);
      await seededHistoryEntry(
        page,
        history.locator(`[data-event-seq="${event.seq}"]`),
        event.created_at
      );
      await expect(rows.filter({ hasText: description })).toHaveCount(1);
    }
    // The threads the history holds, each once, whole, with its Reply - and no row of the
    // outline.
    for (const thread of [resolved, open, message]) {
      await seededHistoryEntry(page, historyItem(page, thread.id), thread.created_at);
      await expect(historyItem(page, thread.id)).toHaveCount(1);
      await expect(history.locator(`[data-history-reply="${thread.id}"]`)).toHaveCount(1);
      await expect(nodeRow(page, thread.id)).toHaveCount(0);
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
// before one cold load of the history. Every day is drawn open, under a line that names it with
// no count and is no control; the reader's watermark is drawn where it stood when the history
// first drew, at the end of today's list when the reader stopped on an earlier day, and a later
// move of the mark - a write the outline does not make yet, made here by the API and read back
// on a refocus - leaves the line where it was, across a close and an open of Details too. Each
// day is the one the page files its moves under, named as the page names it, worked out in the
// page from the moves' own times (`historyDayOf`): a midnight between the seeding and the read
// makes the newer moves' day a day before the reader's today too.
test("the history draws every day open and marks where the reader stopped", async ({ browser }) => {
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

    // The day of each move, as the page files and names it: the newer two moves' is the reader's
    // today unless a midnight came between the seeding and this read, the three back-dated
    // ones' a day before it.
    const newest = await historyDayOf(page, fourth.created_at);
    const oldestUnread = await historyDayOf(page, third.created_at);
    const backDated = await historyDayOf(page, yesterday);
    const days = historyPanel(page).locator("[data-history-day]");
    await expect(days).toHaveCount(new Set([newest.date, oldestUnread.date, backDated.date]).size);
    await expect(days.first()).toHaveAttribute("data-history-day", newest.date);
    await expect(days.last()).toHaveAttribute("data-history-day", backDated.date);
    // The day before is drawn open: its moves stand under its line with no press.
    for (const move of [second, first, created]) {
      await expect(
        days.last().locator(`[data-event-seq="${move.seq}"]`),
        "Yesterday's entries are not drawn"
      ).toBeVisible();
    }
    // Each day's line names the day, with no count, and is no control.
    await expect(days.first().locator(":scope > p")).toHaveText(newest.label);
    await expect(days.last().locator(":scope > p"), "a day's line carries a count").toHaveText(
      backDated.label
    );
    await expect(
      historyPanel(page).locator("[data-history-day] > button"),
      "a day's line is a control"
    ).toHaveCount(0);
    // Newest first: the line that says what is new sits below the oldest unread line and above
    // the newest line the reader had already seen, which here is the first line of the day
    // before.
    const order = () =>
      historyPanel(page)
        .locator("[data-event-seq], li[aria-label]")
        .evaluateAll((items) =>
          items.map(
            (item) => item.getAttribute("data-event-seq") ?? item.getAttribute("aria-label")
          )
        );
    const marked = [
      String(fourth.seq),
      String(third.seq),
      CONTROL_LABELS.history_unread,
      String(second.seq),
      String(first.seq),
      String(created.seq),
    ];
    await expect
      .poll(order, "the line that says what is new is not where the reader stopped")
      .toEqual(marked);
    // The line closes the list of the oldest unread move's day, above the back-dated lines.
    await expect(
      historyPanel(page).locator(`[data-history-day="${oldestUnread.date}"] > ol > li`).last(),
      "the line that says what is new is not at the end of today's list"
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
  const updated = (await getIssueEvents(issue.key, { limit: 100 })).find(
    (event) => event.type === "issue.updated"
  );
  if (updated === undefined) throw new Error(`${issue.key} recorded no issue.updated`);

  const alice = await reader(browser, "alice");
  try {
    const page = await alice.newPage();
    const line = historyPanel(page).locator(`[data-event-seq="${updated.seq}"]`);
    await gotoIssue(page, `/issues/${issue.key}/activity`);
    await seededHistoryEntry(page, line, updated.created_at);
    const row = historyEntries(page).filter({ hasText: "updated the issue" });
    await expect(row).toHaveCount(1);
    await row.getByRole("button", { exact: true, name: CONTROL_LABELS.pin }).click();
    await expect(
      row.getByRole("button", { exact: true, name: CONTROL_LABELS.unpin })
    ).toHaveAttribute("aria-pressed", "true");
    // The pin is one write of the page's store: nothing is pending once Dispatch has it.
    await expect(page.getByRole("region", { name: "Pending changes" })).toHaveCount(0);

    await page.reload();
    await seededHistoryEntry(page, line, updated.created_at);
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
// The history draws no event of a row the outline draws, so this delivery is no line of it.
// The comment's own address does not focus its row yet, so the row is focused
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
  const created = (await getIssueEvents(issue.key, { limit: 100 })).find(
    (event) => event.type === "issue.created"
  );
  if (created === undefined) throw new Error(`${issue.key} recorded no issue.created`);

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
    await seededHistoryEntry(
      page,
      historyPanel(page).locator(`[data-event-seq="${created.seq}"]`),
      created.created_at
    );
    await expect(historyEntries(page).filter({ hasText: "created the issue" })).toHaveCount(1);
    await expect(historyEntries(page).filter({ hasText: "mention" })).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

// A comment body is bounded block by block, so one far longer than an event carries rides its
// event as its first part. The history draws a thread agents wrote among themselves from the
// comment itself, not from its event, so the whole body is there, and the comment's time links
// to its own address, which marks the turn. Every turn is drawn whole, at any length, whether or
// not an address names it: at `/activity`, which names none, a turn of forty lines is drawn to
// its last line, with no "more · N words" control, where a cut would draw its first six.
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
  // Forty lines, each a paragraph of its own; the newest entry, so it stands at the history's
  // head, in Details' view.
  const lines = Array.from({ length: 40 }, (_, index) => `Line ${index + 1} of the agents' turn.`);
  const tall = await createComment(issue.key, { body: lines.join("\n\n") }, activityAgent);

  const alice = await reader(browser, "alice");
  try {
    const page = await alice.newPage();
    await gotoIssue(page, `/issues/${issue.key}/activity`);
    const tallTurn = historyItem(page, tall.id);
    await seededHistoryEntry(page, tallTurn, tall.created_at);
    await expect(tallTurn.getByText(lines.at(-1) ?? "", { exact: true })).toBeAttached();
    await nextFrames(page);
    await expect(tallTurn.locator("[data-node-more]"), "the history cut a turn").toHaveCount(0);
    // Forty lines of 20 px each, with their paragraphs' spacing on top: a cut turn is far short.
    const height = (await tallTurn.boundingBox())?.height ?? 0;
    expect(height, "the history cut a turn").toBeGreaterThan(lines.length * 20);

    const item = historyItem(page, comment.id);
    await seededHistoryEntry(page, item, comment.created_at);
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

/** Waits until the history draws the line of each handoff version `seedHandoffs` recorded on
 *  `key`, each read by its event (`seededHistoryEntry`). */
async function handoffLinesDrawn(page: Page, key: string): Promise<void> {
  const recorded = (await getIssueEvents(key, { limit: 100 })).filter(
    (event) => event.type === "handoff.recorded"
  );
  expect(recorded, "the seed recorded both handoffs").toHaveLength(2);
  for (const event of recorded) {
    await seededHistoryEntry(
      page,
      historyPanel(page).locator(`[data-event-seq="${event.seq}"]`),
      event.created_at
    );
  }
}

// The conversation's handoff cards are the history's entries (the
// Design's "Where each thing goes": "The conversation's handoff cards, and the Activity tab -
// The history in the panel"), each drawn from the list of the issue's handoffs, and each keeps
// what the card opened: its fields and every proof entry, drawn open, which the reader folds by
// its fold control and opens again by it; a list in them is one numbered entry for each item.
test("the history lists each handoff version, and opening one shows its fields and every proof entry", async ({
  browser,
}, testInfo) => {
  const key = await seedHandoffs();

  const alice = await reader(browser, "alice");
  try {
    const page = await alice.newPage();
    await gotoIssue(page, `/issues/${key}/activity`);
    await expect(historyPanel(page)).toBeVisible();
    await handoffLinesDrawn(page, key);

    const lines = historyEntries(page).filter({ hasText: "recorded the implement handoff" });
    await expect(lines).toHaveCount(2);
    // Newest first: version 2 was written after version 1.
    await expect(lines.first()).toContainText("version 2");
    await expect(lines.last()).toContainText("version 1");

    const v1 = handoffLine(page, 1);
    const v2 = handoffLine(page, 2);
    await expect(v2).toContainText("Implementer (e2e)");
    await expect(v1).toContainText("Implementer (e2e)");

    // Drawn open, folded by its line, and opened again by it.
    await v2.getByRole("button", { name: CONTROL_LABELS.hide_fields }).click();
    const toggle = v2.getByRole("button", { name: CONTROL_LABELS.show_fields });
    await expect(toggle).toHaveAttribute("aria-expanded", "false");
    await expect(v2.locator("[data-handoff-field]")).toHaveCount(0);
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
    // A list field is one numbered entry for each item.
    await expect(field("files").locator("ol > li")).toHaveText(["web/src/a.ts", "web/src/b.ts"]);
    await expect(field("files").locator("pre")).toHaveCount(0);
    // The proof list is one row per entry, each named by the page's word for it and counted.
    const proof = field("proof");
    await expect(proof).toHaveCount(2);
    await expect(proof.nth(0)).toContainText(`${CONTROL_LABELS.handoff_fields.proof} 1`);
    await expect(proof.nth(0)).toContainText("browser suite green");
    await expect(proof.nth(1)).toContainText(`${CONTROL_LABELS.handoff_fields.proof} 2`);
    await expect(proof.nth(1)).toContainText("lint clean");
    // Version 1 stays as it was drawn, open: its own fields, and none of version 2's.
    await expect(v1.locator('[data-handoff-field="summary"]')).toHaveText(/First pass$/u);
    await expect(v1.locator('[data-handoff-field="proof"]')).toHaveCount(2);
    await expect(v1.locator('[data-handoff-field="files"]')).toHaveCount(0);

    await page.screenshot({
      fullPage: true,
      path: testInfo.outputPath(`handoff-history-${testInfo.project.name}.png`),
    });
  } finally {
    await alice.close();
  }
});

// A handoff written while the history is open reaches it over the event stream: the write's
// `handoff.recorded` reads the list of the issue's handoffs again (`api/event-refresh.ts`), from
// which the history draws each handoff, so the new version's entry arrives with no reload.
test("a handoff written while the history is open arrives without a reload", async ({
  browser,
}) => {
  const key = await seedHandoffs();

  const alice = await reader(browser, "alice");
  try {
    const page = await alice.newPage();
    await gotoIssue(page, `/issues/${key}/activity`);
    await handoffLinesDrawn(page, key);
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

// One entry per version, drawn from the list of handoffs, and no second drawing of either
// anywhere on the page: the event of a recorded handoff is no line of its own.
test("the history has one line per recorded handoff version", async ({ browser }) => {
  const key = await seedHandoffs();

  const alice = await reader(browser, "alice");
  try {
    const page = await alice.newPage();
    await gotoIssue(page, `/issues/${key}/activity`);
    await expect(historyPanel(page)).toBeVisible();
    await handoffLinesDrawn(page, key);
    const rows = historyEntries(page);
    await expect(rows.filter({ hasText: "recorded the implement handoff" })).toHaveCount(2);
    for (const version of [1, 2]) {
      await expect(handoffLine(page, version)).toHaveCount(1);
    }
    await expect(page.locator("[data-handoff-line]")).toHaveCount(2);
    await expect(historyPanel(page).locator('[data-history-entry^="event:"]')).toHaveCount(1);
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
 * The issue's history, in Details: where the threads the Legion daemon began, whoever wrote in
 * them, and the agents' posts - comment threads tied to no text that no person wrote in - are
 * read, newest first, each thread one entry carrying its own id in `data-history-entry`. The
 * history holds the reader's place in the box Details scrolls in (`IssueHistory.tsx`'s
 * `ViewportAnchor`), and these rows read that.
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
 *  history draws the seed `newest`, a comment's or a message's turn, read by its id
 *  (`seededHistoryEntry`). */
async function openHistory(
  page: Page,
  issueKey: string,
  newest: { readonly created_at: string; readonly id: string }
): Promise<void> {
  await gotoIssue(page, `/issues/${issueKey}/activity`);
  await seededHistoryEntry(page, historyItem(page, newest.id), newest.created_at);
}

/** An issue with `count` messages the Legion daemon wrote, `Existing message <n>`, newest last,
 *  each a thread the history holds, and the newest of them. */
async function seedMessages(title: string, count: number, words = "Existing message") {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title });
  let newest = await createMessage(issue.key, { body: `${words} 0` }, asDaemon);
  for (let index = 1; index < count; index += 1) {
    newest = await createMessage(issue.key, { body: `${words} ${index}` }, asDaemon);
  }
  return { issue, newest };
}

/** Moves the status of `key` `count` times, back and forth between two statuses, so each write
 *  is an `issue.updated` of its own and a line of the history. The history's read of the log
 *  leaves out every message and comment, so a row that needs a log of many events writes these. */
async function writeStatusMoves(key: string, count: number): Promise<void> {
  let status = (await getIssue(key)).status;
  for (let index = 0; index < count; index += 1) {
    status = status === "in_progress" ? "testing" : "in_progress";
    await patchIssue(key, { status });
  }
}

/** An issue with `count` status moves (`writeStatusMoves`). */
async function seedStatusMoves(title: string, count: number) {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title });
  await writeStatusMoves(issue.key, count);
  return issue;
}

test("a reader scrolled down the history keeps their place when a new entry arrives", async ({
  browser,
}) => {
  const { issue, newest } = await seedMessages("Attention test", 30);

  const context = await reader(browser, "alice");
  try {
    const page = await context.newPage();
    await openHistory(page, issue.key, newest);
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
    await createMessage(issue.key, { body: "Arrived while reading" }, asDaemon);

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
  const { issue, newest } = await seedMessages("Attention test", 30);

  const context = await reader(browser, "alice");
  try {
    const page = await context.newPage();
    await openHistory(page, issue.key, newest);
    await scrollHistoryTo(page, 0);

    await createMessage(issue.key, { body: "Arrived at the top" }, asDaemon);

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
  const { issue, newest } = await seedMessages("Away from the history", 30);

  const context = await reader(browser, "alice");
  try {
    const page = await context.newPage();
    await page.setViewportSize(DESKTOP);
    await openHistory(page, issue.key, newest);
    // Up to the panel's own top, off the history and onto the issue's properties.
    await scrollHistoryTo(page, 0);

    // Away to a view the page draws in its own column; Details, and the history in it, stay
    // drawn beside it - that is what makes the arrival below reach a history the reader is not
    // reading.
    const spec = presetToggle(page, "spec");
    await spec.click();
    await expect(spec).toHaveAttribute("aria-pressed", "true");
    expect(await historyScrollTop(page)).toBe(0);

    await createMessage(issue.key, { body: "Arrived while away" }, asDaemon);

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

// The page reads its today from the browser's clock at each render, while the server stamps
// each seed with its own time: a row whose seeding and drawing fall on either side of the
// reader's midnight finds its seeds a day before today. So the rows read a seed through
// `seededHistoryEntry`, which finds it by its id in the day the page files its time under, and
// none of them assumes which day a seed falls in. Here, and in no other row, the page's own clock
// is moved past the reader's next midnight after the seeding, before the history is drawn: the
// seeds are then in the day before the page's today, named as the page names it and drawn open
// under its line, and the rows' reading finds them there.
test("entries made before the reader's midnight are found in the day the history draws them in after it", async ({
  browser,
}) => {
  const { issue, newest } = await seedMessages("Seeded before midnight", 3);

  const context = await reader(browser, "alice");
  try {
    const page = await context.newPage();
    // The page's own clock, from the real time, so the row can move it once the seeds are made.
    await page.clock.install();
    await gotoIssue(page, `/issues/${issue.key}`);
    // Every write of this issue is a message of the Legion daemon, which the outline draws no
    // row for, so the tree is drawn with no row in it.
    await expectIssuePageDrawn(page);
    // How far the reader's next midnight is, by the page's clock.
    const toMidnight = await page.evaluate(() => {
      const now = new Date();
      const midnight = new Date(now.getFullYear(), now.getMonth(), now.getDate() + 1);
      return midnight.getTime() - now.getTime();
    });
    await page.clock.fastForward(toMidnight + 5_000);
    const seeded = await historyDayOf(page, newest.created_at);
    expect(seeded.today, "the page's clock did not pass the reader's midnight").toBe(false);

    // The history is drawn past that midnight, and read as every row reads it.
    await openHistory(page, issue.key, newest);
    const day = historyPanel(page).locator(`[data-history-day="${seeded.date}"]`);
    await expect(
      day.locator(":scope > p"),
      "the seeds' day is not the day the page names before its today"
    ).toHaveText(seeded.label);
    for (let index = 0; index < 3; index += 1) {
      await expect(day.getByText(`Existing message ${index}`, { exact: true })).toBeVisible();
    }
    await expect(
      historyPanel(page).locator("[data-history-day]"),
      "the history drew a day the seeds are not in"
    ).toHaveCount(1);
  } finally {
    await context.close();
  }
});

// Paging is the history's: the outline reads each of its three lists whole and offers no Show
// more at all. The history draws 20 entries at a time, and its Show more says how many of the
// entries read are left, with "+" while an older page of the log is unread; a press that would
// leave none of them reads that page first, and then draws the next entries. Those land beneath
// the entries already on screen, so the entry the reader is on does not move; and the reader who
// presses on reaches the log's start, where the control goes.
test("loading older history entries keeps the reader anchor", async ({ browser }) => {
  // The seed's 205 status moves and the presses that draw 200 of them, 20 at a time.
  test.setTimeout(120_000);
  // Five more than the history's 200-event page (`IssueHistory.tsx`), so a second page exists:
  // each is a line of the history, which a message of the daemon's is not, since the history's
  // read of the log leaves every message and comment out.
  const issue = await seedStatusMoves("Older events", 205);
  const events = await allEvents(issue.key);
  const created = events.find((event) => event.type === "issue.created");
  if (created === undefined) throw new Error(`${issue.key} recorded no issue.created`);
  const newest = events[0];
  if (newest === undefined) throw new Error(`${issue.key} recorded no event`);

  const context = await reader(browser, "alice");
  try {
    const page = await context.newPage();
    await gotoIssue(page, `/issues/${issue.key}/conversation`);
    // Every write of this issue is a status move, which the outline draws no row for, so the
    // tree is drawn with no row in it.
    await expectIssuePageDrawn(page);
    await expect(page.getByRole("button", { name: CONTROL_LABELS.show_more })).toHaveCount(0);

    await gotoIssue(page, `/issues/${issue.key}/activity`);
    // The browser's own scroll anchoring is turned off, as in the row of an arrival above, so
    // the poll below reads the page alone. The older page lands beneath the reader: what can
    // move the entry the reader is on is a history that draws something else in place of its
    // list while that page is read, or that hands the focus to a control off screen once it lands.
    await withoutScrollAnchoring(page);
    await seededHistoryEntry(
      page,
      historyPanel(page).locator(`[data-event-seq="${newest.seq}"]`),
      newest.created_at
    );
    await expect(
      historyShowMore(page),
      "Show more does not say that an older page of the log is unread"
    ).toHaveText(new RegExp(`\\(\\d+\\+ ${CONTROL_LABELS.show_more_left}\\)$`, "u"));
    // The reader draws the entries the first page holds, until the press of the control reads
    // the older page before it draws.
    await readHistoryDown(page, issue.key, events.length, (control) => control.click(), "reads");
    const showMore = historyNextPage(page);
    await expect(showMore).toBeVisible();
    await showMore.scrollIntoViewIfNeeded();
    await nextFrames(page);
    const clip = await historyViewport(page);
    const showMoreBox = await showMore.boundingBox();
    expect(showMoreBox).not.toBeNull();
    expect(showMoreBox?.y ?? -1).toBeGreaterThanOrEqual(clip.top);
    expect((showMoreBox?.y ?? 0) + (showMoreBox?.height ?? 0)).toBeLessThanOrEqual(clip.bottom);

    const entry = await readingAnchor(page, clip);
    const anchor = historyPanel(page).locator(`[data-history-entry="${entry}"]`);
    const before = (await anchor.boundingBox())?.y;
    expect(before, `the entry the reader is on, ${entry}, has no box`).toBeDefined();
    const drawn = await historyEntries(page).count();
    const answered = olderPageAnswered(page, issue.key);
    await showMore.click();

    // The older page was the log's last: once it has landed and the press has drawn the next
    // entries, the count says no "+".
    await answered;
    await expect
      .poll(() => historyEntries(page).count(), {
        message: `${CONTROL_LABELS.show_more} drew no entry once the older page landed`,
      })
      .toBeGreaterThan(drawn);
    await expect(
      historyShowMore(page),
      "Show more says an older page is unread once the log's last page has landed"
    ).toHaveText(new RegExp(`\\(\\d+ ${CONTROL_LABELS.show_more_left}\\)$`, "u"));
    await expect
      .poll(
        async () => (await anchor.boundingBox())?.y,
        "loading older entries moved the entry the reader is on"
      )
      .toBe(before);

    // The reader presses on to the log's start: the oldest entry of all, the issue's own
    // creation, which only the page beneath the first 200 holds, is drawn, and the control goes.
    await readHistoryDown(page, issue.key, events.length, (control) => control.click(), "gone");
    await seededHistoryEntry(
      page,
      historyPanel(page).locator(`[data-event-seq="${created.seq}"]`),
      created.created_at
    );
    await expect(historyEntries(page).filter({ hasText: "created the issue" })).toHaveCount(1);
    await expect(
      historyShowMore(page),
      `${CONTROL_LABELS.show_more} is still offered once the whole log is read`
    ).toHaveCount(0);
  } finally {
    await context.close();
  }
});

/** How many messages of the daemon's the row below writes under a person's comments, each an
 *  entry of the history: more than it draws at once. */
const UNDER_COMMENTS = 30;

/** How many comments a person writes over those messages in the row below, each left out of the
 *  history: more than the log's first page of 200 events holds, so that page holds no entry. */
const OVER_ENTRIES = 205;

// The log's first page is its newest 200 events, and on a real issue most of them are comments
// of people, which the outline draws and the history leaves out. Found by a row on copies of real
// issues: the first page held 0, 12 or 17 entries of the history, which drew them under a Show
// more that said no number, the older pages unread. The history now reads the log without the
// events that write a post (`exclude_types`), so a log of thousands of comments is read in one
// request. Here the log read whole holds the person's comments in its first page of 200 and the
// history's entries in its second: the first view is one read of the log that answers every entry,
// draws 20, says how many are left, and the reader presses down to the log's start with no read
// of another page.
// SLOW(>=2s): 30 messages and 205 comments written, a sign-in, one load of the history and its
// read down.
test("the history leaves the comments of people out of its read of the log, so its first view is one read that draws 20 entries and Show more says how many are left", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  const { issue, newest } = await seedMessages(
    "A history under comments",
    UNDER_COMMENTS,
    "Older message"
  );
  for (let index = 0; index < OVER_ENTRIES; index += 1) {
    await createComment(issue.key, { body: `A person's comment ${index}` });
  }
  const events = await allEvents(issue.key);
  const created = events.find((event) => event.type === "issue.created");
  if (created === undefined) throw new Error(`${issue.key} recorded no issue.created`);
  expect(
    events.length,
    "the seed wrote events the row does not count, so its count of entries proves nothing"
  ).toBe(1 + UNDER_COMMENTS + OVER_ENTRIES);
  expect(
    events.slice(0, 200).filter((event) => event.type !== "comment.created"),
    "the log's first page holds events other than the person's comments, so the row proves nothing"
  ).toEqual([]);
  // Every entry of the history: the daemon's messages and the issue's creation.
  const total = UNDER_COMMENTS + 1;

  const context = await reader(browser, "alice");
  try {
    const page = await context.newPage();
    // Each read of a page of the log that the page sends: 200 events long, as the history reads
    // them, so the read of the first event past the read mark (`after`) is none of them, and nor
    // is the read mark's read of the newest event (`isReadMarkRead`), which is no read of Details.
    const logReads: string[] = [];
    page.on("request", (request) => {
      const url = new URL(request.url());
      if (
        url.pathname === `/api/v1/issues/${issue.key}/events` &&
        !url.searchParams.has("after") &&
        !isReadMarkRead(url, issue.key)
      ) {
        logReads.push(url.search);
      }
    });
    await gotoIssue(page, `/issues/${issue.key}/activity`);
    expect(
      await historyShowMoreState(page),
      "the history's first view drew a Show more that says no number, the older page unread"
    ).not.toBe("uncounted");
    await seededHistoryEntry(page, historyItem(page, newest.id), newest.created_at);
    await expect(
      historyEntries(page),
      "the history's first view did not draw 20 entries"
    ).toHaveCount(PAGE_SIZE);
    await expect(historyShowMore(page)).toHaveText(showMoreName(total - PAGE_SIZE));
    expect(logReads, "the first view sent more than one read of the log").toHaveLength(1);

    await readHistoryDown(page, issue.key, events.length, (control) => control.click(), "gone");
    await seededHistoryEntry(
      page,
      historyPanel(page).locator(`[data-event-seq="${created.seq}"]`),
      created.created_at
    );
    await expect(historyEntries(page)).toHaveCount(total);
    expect(logReads, "reading down read the log again").toHaveLength(1);
  } finally {
    await context.close();
  }
});

/**
 * An issue of `title` whose log's first page of 200 events ends in a run of ten uploads by one
 * author that straddles it (`history-model.ts`): eight uploads, `between` status moves, which
 * split no run, the run's two newest uploads within the minute, and as many status moves after
 * them as fill that page, so the first page holds the run's two newest uploads and the older page
 * its other eight. A status move is an event the history's read of the log keeps; a message of
 * the daemon's is not. That page holds 199 entries with 19 or 20 status moves between the
 * uploads: the history draws them 20 at a time down to the 180th, and Show more then says "19+",
 * fewer entries left than a press draws (`historyNextPage`), so that press reads the older page,
 * which extends the run, before it draws. With 19 status moves between the uploads the run is the
 * 180th entry, the last drawn above Show more; with 20, the newest of them stands between the run
 * and Show more. Answers the issue's key, the newest status move between the uploads, the newest
 * upload's event and the log's events.
 */
async function seedRunAcrossPage(
  title: string,
  between: 19 | 20
): Promise<{
  between: DispatchEvent;
  events: DispatchEvent[];
  key: string;
  newestUpload: DispatchEvent;
}> {
  await createProject({ key: "CORE", name: "Core" });
  const { key } = await createIssue({ project: "CORE", title });
  const upload = (index: number) =>
    uploadIssueFile(key, {
      bytes: new Uint8Array(Buffer.from(ONE_PIXEL_PNG, "base64")),
      contentType: "image/png",
      name: `shot-${index}.png`,
    });
  for (let index = 0; index < 8; index += 1) await upload(index);
  await writeStatusMoves(key, between);
  const [newestBetween] = await getIssueEvents(key, { limit: 1, order: "desc" });
  if (newestBetween === undefined)
    throw new Error(`${key} holds no status move between the uploads`);
  for (let index = 8; index < 10; index += 1) await upload(index);
  await writeStatusMoves(key, 200 - 2 - between);
  const events = await allEvents(key);
  const newestUpload = events.find(
    (one) => one.type === "artifact.created" && one.payload.artifact.name === "shot-9.png"
  );
  if (newestUpload === undefined) throw new Error(`${key} recorded no upload of shot-9.png`);
  return { between: newestBetween, events, key, newestUpload };
}

// Show more, where it reads the log's next page, adds entries under the reader, and the line
// they read keeps its place on the screen; and by the focus rule of `IssueHistory.tsx`, that
// press made by keyboard hands the focus to a control the page it read added, never one out of
// the reader's view. One author's newest upload and their uploads made at most a minute before
// it are one entry (`history-model.ts`), so a run whose uploads straddle the log's 200-event page
// is extended by the older page. Found by the tester on a running build of e3bf280f with a copy
// of a large real issue, when a run listed its files oldest first and was keyed by its oldest
// upload read so far: a reader on a file of such a run saw it move 572 px down when the older
// page landed (the entry's key went from uploads:4736 to uploads:4709), where an event line in
// the same place stayed put; and the press by keyboard put the focus on that run's author link
// 752 px above Details' view (6,624 px on the next press). A run now lists its files newest first
// and keeps its key as the older page extends it. The run stands at the foot of the log's first
// page (`seedRunAcrossPage`), so the reader first draws that page's entries down to it, pressing
// Show more while it draws the next ones at once; those presses are dispatched to the control, so
// they move neither the focus nor Details, and the reader comes to the run by a scroll of their
// own. The press that reads the older page is the one made once fewer entries are left than it
// draws, with the run the last entry drawn above it.
test("loading older keeps the reader's place on an upload run the older page extends, and the focus in view", async ({
  browser,
}) => {
  test.setTimeout(240_000);
  const {
    events: log,
    key,
    newestUpload: upload,
  } = await seedRunAcrossPage("Uploads across a page", 19);
  const newest = "shot-9.png";
  const events = log.length;

  const context = await reader(browser, "alice");
  try {
    const page = await context.newPage();
    const putFileAtCentre = async (): Promise<void> => {
      await page.evaluate((name) => {
        const link = [
          ...document.querySelectorAll<HTMLElement>(
            '[data-testid="issue-history"] [data-history-uploads] a'
          ),
        ].find((one) => one.textContent === name);
        let box = link?.parentElement ?? null;
        while (box !== null && !/(auto|scroll)/.test(getComputedStyle(box).overflowY)) {
          box = box.parentElement;
        }
        if (link === undefined || box === null) throw new Error(`${name} is drawn nowhere`);
        const view = box.getBoundingClientRect();
        box.scrollTop += link.getBoundingClientRect().top - (view.top + view.height / 2);
      }, newest);
      await nextFrames(page);
    };
    const file = historyPanel(page)
      .locator("[data-history-uploads]")
      .getByRole("link", { exact: true, name: newest });
    const showMore = historyNextPage(page);
    const dispatched = (control: Locator) => control.dispatchEvent("click");

    // By pointer: the reader is on the newest file of the run, and the older page lands.
    await gotoIssue(page, `/issues/${key}/activity`);
    await withoutScrollAnchoring(page);
    await newestHistoryEntry(page);
    await readHistoryDown(page, key, events, dispatched, "reads");
    await seededHistoryEntry(page, file, upload.created_at);
    await expect(file).toHaveCount(1);
    await expect(showMore).toBeVisible();
    const run = historyPanel(page).locator("[data-history-uploads]");
    const firstPage = Number(await run.getAttribute("data-history-uploads"));
    expect(
      firstPage > 0 && firstPage < 10,
      `the log's first page holds ${firstPage} of the run's 10 uploads, so the run does not straddle the page and the row proves nothing`
    ).toBe(true);
    await expect(
      historyEntries(page).last(),
      "the run is not the last entry drawn above Show more, so the row proves nothing"
    ).toHaveAttribute("data-history-uploads", String(firstPage));
    await putFileAtCentre();
    const before = (await file.boundingBox())?.y;
    expect(before, `${newest} has no box`).toBeDefined();
    await showMore.dispatchEvent("click");
    await expect(
      historyPanel(page).locator("[data-history-uploads]"),
      "the older page did not extend the run"
    ).toHaveAttribute("data-history-uploads", "10");
    await nextFrames(page);
    await expect
      .poll(
        async () => (await file.boundingBox())?.y,
        "loading older entries moved the upload the reader is on"
      )
      .toBe(before);

    // By keyboard: Show more pressed with the focus on it hands the focus to a control in view.
    await gotoIssue(page, `/issues/${key}/activity`);
    await newestHistoryEntry(page);
    await readHistoryDown(page, key, events, dispatched, "reads");
    await seededHistoryEntry(page, file, upload.created_at);
    await expect(file).toHaveCount(1);
    await putFileAtCentre();
    await showMore.focus();
    await page.keyboard.press("Enter");
    await expect(historyPanel(page).locator("[data-history-uploads]")).toHaveAttribute(
      "data-history-uploads",
      "10"
    );
    await nextFrames(page);
    const clip = await historyViewport(page);
    const focused = await page.evaluate(() => {
      const box = document.activeElement?.getBoundingClientRect();
      return box === undefined ? null : { bottom: box.bottom, top: box.top };
    });
    expect(focused, "Show more left the focus nowhere").not.toBeNull();
    expect(
      (focused?.top ?? -1) >= clip.top - 1 && (focused?.bottom ?? 0) <= clip.bottom + 1,
      `Show more handed the focus to a control out of Details' view (${Math.round(focused?.top ?? 0)} px, the view ${Math.round(clip.top)} to ${Math.round(clip.bottom)})`
    ).toBe(true);
  } finally {
    await context.close();
  }
});

// The focus rule of `IssueHistory.tsx`: Show more pressed by keyboard where it reads the log's
// next page hands the focus to a control the page it read added, never one out of the reader's
// view, where a press of Enter would follow a link the reader cannot see. Here a status move
// stands between the run and Show more, so the older page's files of the run land above that
// line and push every control below it down by their height: the first entry the press
// draws, the next status move between the uploads, is pushed down under Details' foot. Found by the
// tester on a running build of 864163bf with a copy of a large real issue: after the press by
// keyboard the focus was on a turn's time link at 1,425 px, the view ending at 900, with 13 files
// of a run (572 px) added above the entry between that run and the control; the next page's Retry
// did the same. The run and the status move stand at the foot of the log's first page
// (`seedRunAcrossPage`), so the reader first draws that page's entries down to them, by presses
// dispatched to Show more while it draws the next ones at once, which move neither the focus nor
// Details.
test("Show more by keyboard keeps the focus in view when the older page it reads extends a run above the entries under it", async ({
  browser,
}) => {
  test.setTimeout(240_000);
  const { between, events, key, newestUpload } = await seedRunAcrossPage(
    "A run above a message",
    20
  );
  const created = events.find((event) => event.type === "issue.created");
  if (created === undefined) throw new Error(`${key} recorded no issue.created`);

  const context = await reader(browser, "alice");
  try {
    const page = await context.newPage();
    await gotoIssue(page, `/issues/${key}/activity`);
    await newestHistoryEntry(page);
    await readHistoryDown(
      page,
      key,
      events.length,
      (control) => control.dispatchEvent("click"),
      "reads"
    );
    const run = historyPanel(page).locator("[data-history-uploads]");
    await seededHistoryEntry(page, run, newestUpload.created_at);
    await expect(run).toHaveCount(1);
    const firstPage = Number(await run.getAttribute("data-history-uploads"));
    expect(
      firstPage > 0 && firstPage < 10,
      `the log's first page holds ${firstPage} of the run's 10 uploads, so the run does not straddle the page and the row proves nothing`
    ).toBe(true);
    const betweenLine = historyPanel(page).locator(`[data-event-seq="${between.seq}"]`);
    await seededHistoryEntry(page, betweenLine, between.created_at);
    await expect(
      betweenLine,
      "the status move between the uploads is not drawn on the first page"
    ).toHaveCount(1);
    const showMore = historyNextPage(page);
    await showMore.focus();
    await nextFrames(page);
    await page.keyboard.press("Enter");
    await expect(run, "the older page did not extend the run").toHaveAttribute(
      "data-history-uploads",
      "10"
    );
    await seededHistoryEntry(
      page,
      historyPanel(page).locator(`[data-event-seq="${created.seq}"]`),
      created.created_at
    );
    await expect(historyEntries(page).filter({ hasText: "created the issue" })).toHaveCount(1);
    await nextFrames(page);
    const clip = await historyViewport(page);
    const focused = await page.evaluate(() => {
      const box = document.activeElement?.getBoundingClientRect();
      return box === undefined ? null : { bottom: box.bottom, top: box.top };
    });
    expect(focused, "Show more left the focus nowhere").not.toBeNull();
    expect(
      (focused?.top ?? -1) >= clip.top - 1 && (focused?.bottom ?? 0) <= clip.bottom + 1,
      `Show more handed the focus to a control out of Details' view (${Math.round(focused?.top ?? 0)} px, the view ${Math.round(clip.top)} to ${Math.round(clip.bottom)})`
    ).toBe(true);
  } finally {
    await context.close();
  }
});

// Details keeps the focus rule of every page of Dispatch. A control of Details that the reader's
// own press or key takes away hands the focus on inside Details: each Retry, Show more and
// Unsubscribe hands it on itself, and the Details focus keeper (`details-focus-keeper.ts`) under
// them hands on what no site did. A control that goes with no press of the reader's - a live
// update, another session's change that takes a row away - leaves the focus on the page's body,
// where the page's keys work, and nothing scrolls: the reader asked for no move, so the page puts
// them nowhere, and Details stays on what they were reading. Only an open modal dialog takes the
// focus then. Here another session's write takes the focused control away: when a person writes
// in a thread only agents wrote, the thread leaves the history for the outline, and the control
// of it that held the reader's focus goes with it. The keeper once handed that focus on to the
// nearest control of Details, a move the reader had not asked for. The thread stands near the
// foot of Details, which the focus scrolled to its end, so the thread's going shortens what
// Details can scroll: the browser brings Details back by as much as it lost there, and the page
// moves it no further.
test("a control of Details that another session takes away under the focus leaves the focus on the page's body, and Details does not scroll", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "An agents' thread a person joins" });
  const root = await createComment(
    issue.key,
    { body: "The plan's third task needs a second look." },
    activityAgent
  );
  await createComment(
    issue.key,
    { body: "The third task stands as written.", reply_to: root.id },
    activityAgent
  );

  const context = await reader(browser, "alice");
  try {
    const page = await context.newPage();
    await gotoIssue(page, `/issues/${issue.key}`);
    await openDetails(page);
    const thread = await seededHistoryEntry(page, historyItem(page, root.id), root.created_at);
    // The turn's time link holds the focus as a keyboard reader leaves it there, with no press.
    await thread.locator("[data-history-time]").focus();
    await nextFrames(page);
    const before = await historyScrollRange(page);
    await createComment(
      issue.key,
      { body: "A person joins the agents' thread.", reply_to: root.id },
      { login: "bob" }
    );
    await expect(
      historyItem(page, root.id),
      "the thread stayed in the history once a person wrote in it"
    ).toHaveCount(0, { timeout: 30_000 });
    await nextFrames(page);
    const focus = await page.evaluate(() => {
      const active = document.activeElement;
      return {
        name: `${active?.tagName} "${(active?.getAttribute("aria-label") ?? active?.textContent ?? "").trim().slice(0, 40)}"`,
        onBody: active === document.body,
      };
    });
    expect(
      focus.onBody,
      `another session's write took the focused control away, and the focus went to ${focus.name} rather than staying on the page's body`
    ).toBe(true);
    const after = await historyScrollRange(page);
    expect(
      after.top,
      `Details scrolled from ${before.top} px to ${after.top} px as another session's write took the focused control away, though the write took its end only to ${after.end} px`
    ).toBe(Math.min(before.top, after.end));
  } finally {
    await context.close();
  }
});

/** A paragraph of a long agents' turn: one sentence of the agents' work six times over. Every row
 *  that draws long turns rests on the height this gives them, so each draws its turns of it. */
const TURN_PARAGRAPH = "the agents read the history and wrote what they found ".repeat(6);

// The focus rule of `IssueHistory.tsx`, with long agents' turns under Show more. A press moves
// the focus to the first entry it added, as Show more under every other list of Details does: to
// a control of that entry the reader can see, its time at its head, where the control stood,
// and not its Reply a thousand pixels below at its foot. The place keeper records the reader's
// place at each focus that moves into the history, so it holds the entry the hand-on puts the
// focus in as the turns under it are drawn. Found by the tester on a running build of f589637b
// with a copy of a large real issue, when turns were still cut as they came near the screen: a
// press made as the focus reached the control left the focus on a turn's time link 212 px above
// Details' view at 1440 px (two runs of four), 224 px above at 768 px and 280 px above at 390
// px. Every turn is drawn whole now, and Enter is still pressed as the focus reaches the control.
test("Show more pressed as the focus reaches it hands the focus to the first long turn it draws, in view", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Long turns under Show more" });
  // Each turn is about a thousand pixels at Details' width, as the long turns of the real issue
  // were; five more than the history draws at first, so one press draws them.
  for (let index = 0; index < PAGE_SIZE + 5; index += 1) {
    await createMessage(
      issue.key,
      {
        body: Array.from(
          { length: 10 },
          (_, part) => `Turn ${index}, part ${part}: ${TURN_PARAGRAPH}`
        ).join("\n\n"),
      },
      asDaemon
    );
  }

  const context = await reader(browser, "alice");
  try {
    const page = await context.newPage();
    await gotoIssue(page, `/issues/${issue.key}/activity`);
    await newestHistoryEntry(page);
    const more = historyShowMore(page);
    await expect(more, "the history drew every turn at once").toBeVisible();
    await more.focus();
    await page.keyboard.press("Enter");
    // The issue's creation, the oldest entry, is drawn with the five turns: every entry is.
    await expect(historyEntries(page)).toHaveCount(PAGE_SIZE + 6);
    await expect(more, "Show more stayed with every entry drawn").toHaveCount(0);
    await nextFrames(page);
    const clip = await historyViewport(page);
    const focused = await page.evaluate(() => {
      const active = document.activeElement;
      const box =
        active === null || active === document.body ? undefined : active.getBoundingClientRect();
      return box === undefined ? null : { bottom: box.bottom, top: box.top };
    });
    expect(focused, "Show more left the focus on the page").not.toBeNull();
    expect(
      (focused?.top ?? -1) >= clip.top - 1 && (focused?.bottom ?? 0) <= clip.bottom + 1,
      `Show more handed the focus to a control out of Details' view (${Math.round(focused?.top ?? 0)} px, the view ${Math.round(clip.top)} to ${Math.round(clip.bottom)})`
    ).toBe(true);
    expect(
      await historyEntries(page)
        .nth(PAGE_SIZE)
        .evaluate((entry) => entry.contains(document.activeElement)),
      "Show more did not hand the focus to the first entry it drew"
    ).toBe(true);
  } finally {
    await context.close();
  }
});

/** How many entries the last Show more of the row below draws: the issue's creation and 13 of the
 *  daemon's notes, dated a day before every entry above them, fewer than a press draws. */
const EARLIER_DAY_ENTRIES = 14;

/**
 * An issue whose history holds `2 * PAGE_SIZE` notes of the Legion daemon's, each a thread of its
 * own, and under them `EARLIER_DAY_ENTRIES` entries dated a day before the oldest of those notes:
 * the issue's creation and the notes the daemon wrote first, each with its event. The history's
 * last Show more draws those, under the line that names their day. Answers the issue's key and
 * the time those entries are dated to.
 */
async function seedEarlierDayUnderLastPage(): Promise<{ earlierAt: string; key: string }> {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "An earlier day under Show more" });
  const notes: { readonly created_at: string; readonly id: string }[] = [];
  for (let index = 0; index < 2 * PAGE_SIZE + EARLIER_DAY_ENTRIES - 1; index += 1) {
    notes.push(await createMessage(issue.key, { body: `The daemon's note ${index}.` }, asDaemon));
  }
  const earlier = new Set(notes.slice(0, EARLIER_DAY_ENTRIES - 1).map((note) => note.id));
  const oldestLater = notes[EARLIER_DAY_ENTRIES - 1];
  if (oldestLater === undefined) throw new Error(`${issue.key} holds no later note`);
  // A day before the oldest of the later notes, so the earlier entries' day comes before the day
  // of every later note, whenever the seed ran.
  const earlierAt = new Date(
    new Date(oldestLater.created_at).getTime() - 24 * 60 * 60 * 1_000
  ).toISOString();
  const events = (await getIssueEvents(issue.key, { limit: 200 })).filter(
    (event) =>
      event.type === "issue.created" ||
      (event.type === "message.created" && earlier.has(event.payload.id))
  );
  if (events.length !== EARLIER_DAY_ENTRIES) {
    throw new Error(
      `${issue.key} recorded ${events.length} events of its earlier entries, not ${EARLIER_DAY_ENTRIES}`
    );
  }
  await Promise.all([
    ...[...earlier].map((id) => setCreatedAt("messages", id, earlierAt)),
    ...events.map((event) => setEventCreatedAt(event.id, earlierAt)),
  ]);
  return { earlierAt, key: issue.key };
}

/**
 * The reader's own scroll of Details' box that puts `more`, Show more holding the focus, at the
 * foot of Details' view, where a Tab can leave it, so the first entry a press draws stands below
 * the view. Waits for the box to stand still, then fails the row where the scroll took the focus
 * off Show more or left it anywhere but at the view's foot, since the row proves nothing then.
 * `size` is the window's, whose width the message names.
 */
async function showMoreAtViewFoot(
  page: Page,
  more: Locator,
  size: { readonly width: number }
): Promise<void> {
  const view = await historyViewport(page);
  const reached = await boxOf(more, CONTROL_LABELS.show_more);
  await scrollHistoryBy(page, reached.y + reached.height - view.bottom);
  await detailsStandStill(page, "the reader's scroll");
  await expect(more, "the reader's scroll took the focus off Show more").toBeFocused();
  const pressedAt = await boxOf(more, CONTROL_LABELS.show_more);
  expect(
    Math.abs(pressedAt.y + pressedAt.height - view.bottom) <= 1,
    `at ${size.width} px Show more stands at ${Math.round(pressedAt.y)} to ${Math.round(pressedAt.y + pressedAt.height)} px, not at the foot of Details' view at ${Math.round(view.bottom)}, so the row proves nothing`
  ).toBe(true);
}

// Show more pressed by keyboard hands the focus to the first entry it drew, and brings that entry
// into Details' view by the least scroll, a scroll of Details' box alone that leaves the page
// where it was (`IssueHistory.tsx`). The last press of this history draws the entries of an
// earlier day, so the line that names that day stands above the first of them, where the control
// stood. Tab reaches the control from the entries above it and Enter presses it, up to the last
// press. A Tab leaves the control anywhere from the middle of Details' view to its foot, so before
// that press the reader's own scroll puts it at the foot, where the tester's Tab had left it: the
// first entry the press draws then stands under the day's line, below the view. Found by the
// tester on a running build of 82e96bda with a copy of a large real issue, at 1440 by 900 after
// four presses: the press of "Show more (14 left)" left the focus on the first added entry's link,
// its top at 849 px in a view that ended at 847, wholly below it.
// SLOW(>=2s): 53 notes written and 14 entries dated a day back, and at each of two widths one load
// of the history, read down by Tab and Enter.
test("a keyboard press of the history's last Show more leaves the focus in view", async ({
  browser,
}) => {
  // The seed's 53 notes, and at each of the two widths a load and about a hundred presses of Tab.
  test.setTimeout(120_000);
  const { earlierAt, key } = await seedEarlierDayUnderLastPage();
  const total = 2 * PAGE_SIZE + EARLIER_DAY_ENTRIES;
  // Enough presses of Tab to pass every control of the entries one press draws: each note's time,
  // its author and its Reply.
  const tabs = 5 * PAGE_SIZE;
  // Each width's outcome, so one run says every width the last press failed at.
  const failures: string[] = [];
  for (const size of [DESKTOP, PHONE]) {
    const context = await reader(browser, "alice");
    try {
      const page = await context.newPage();
      await page.setViewportSize(size);
      await gotoIssue(page, `/issues/${key}/activity`);
      await expect(marginShell(page), "/activity did not open Details").toBeVisible();
      await newestHistoryEntry(page);
      // The reader's focus starts on the history's heading, above its first entry: a load of
      // `/activity` opens Details at the history, and puts no focus in it.
      await historyHeading(page).focus();
      await expect(historyHeading(page), "the history's heading took no focus").toBeFocused();
      const more = historyShowMore(page);
      const entries = historyEntries(page);

      // The first press draws the next of the later notes, and hands the focus to the first.
      await tabWalk(page, tabs)(
        historyShowMore(page),
        `Tab did not bring the focus to ${CONTROL_LABELS.show_more} before the first press`
      );
      await expect(more).toHaveText(showMoreName(total - PAGE_SIZE));
      await page.keyboard.press("Enter");
      await expect(entries).toHaveCount(2 * PAGE_SIZE);
      await expect
        .poll(
          () => focusInside(entries.nth(PAGE_SIZE)),
          "the first press did not hand the focus to the first entry it drew"
        )
        .toBe(true);

      // The last press, made with the control at the foot of Details' view.
      await tabWalk(page, tabs)(
        historyShowMore(page),
        `Tab did not bring the focus to ${CONTROL_LABELS.show_more} before the last press`
      );
      await expect(more, "the next press does not draw the earlier day's entries").toHaveText(
        showMoreName(EARLIER_DAY_ENTRIES)
      );
      await showMoreAtViewFoot(page, more, size);
      const daysBefore = await historyPanel(page)
        .locator("[data-history-day]")
        .evaluateAll((days) => days.map((day) => day.getAttribute("data-history-day")));
      // One read before the press, with Details' box standing still: where the page stands.
      const pageBefore = await page.evaluate(() => ({ x: window.scrollX, y: window.scrollY }));
      await page.keyboard.press("Enter");
      await expect(entries, "the last press did not draw the earlier day's entries").toHaveCount(
        total
      );
      await expect(more, "Show more stayed with every entry drawn").toHaveCount(0);
      await detailsStandStill(page, `the last ${CONTROL_LABELS.show_more}`);

      // The first entry the press drew is the first of a day the history had not drawn, the
      // earlier one, under the line that names it.
      const earlierDay = await historyDayOf(page, earlierAt);
      expect(
        daysBefore,
        "the history drew the earlier day before its last press, so the row proves nothing"
      ).not.toContain(earlierDay.date);
      const first = entries.nth(2 * PAGE_SIZE);
      const day = first.locator("xpath=ancestor::li[@data-history-day][1]");
      await expect(
        day,
        "the first entry the last press drew is not of the earlier day"
      ).toHaveAttribute("data-history-day", earlierDay.date);
      await expect(day.locator(":scope > p"), "no line names the earlier day").toHaveText(
        earlierDay.label
      );
      await expect(
        first.locator("xpath=preceding-sibling::*"),
        "the first entry the last press drew is not the first under its day's line"
      ).toHaveCount(0);

      // Two reads once Details' box has stood still for two frames: where the press left the
      // focus, and the view it left.
      const landed = await first.evaluate((entry) => {
        const active = document.activeElement;
        const box =
          active === null || active === document.body ? null : active.getBoundingClientRect();
        return {
          focus: box === null ? null : { bottom: box.bottom, top: box.top },
          onEntry: entry.contains(active),
          page: { x: window.scrollX, y: window.scrollY },
        };
      });
      const shown = await historyViewport(page);
      const pressed = `at ${size.width} px the last ${CONTROL_LABELS.show_more}`;
      if (!landed.onEntry) {
        failures.push(`${pressed} did not hand the focus to the first entry it drew`);
      }
      if (landed.focus === null) {
        failures.push(`${pressed} left the focus on the page`);
      } else if (landed.focus.top < shown.top - 1 || landed.focus.bottom > shown.bottom + 1) {
        failures.push(
          `${pressed} left the focus at ${Math.round(landed.focus.top)} to ${Math.round(landed.focus.bottom)} px, ${landed.focus.bottom > shown.bottom + 1 ? "below" : "above"} Details' view (${Math.round(shown.top)} to ${Math.round(shown.bottom)})`
        );
      }
      if (landed.page.x !== pageBefore.x || landed.page.y !== pageBefore.y) {
        failures.push(
          `${pressed} scrolled the page from ${pageBefore.x}, ${pageBefore.y} px to ${landed.page.x}, ${landed.page.y}`
        );
      }
    } finally {
      await context.close();
    }
  }
  expect(failures, failures.join("; ")).toEqual([]);
});

/** The start of the title of the issue `seedApprovalUnderFirstPage` cites, which the link to it
 *  reads once the names of the references are read. */
const CITED_TITLE = "Cited:";

/**
 * Asks, as `actor`, for the approval of `issue`'s spec, with the Dispatch address `cited`
 * in the summary (without its scheme). Answers the `ask.opened` event the request recorded.
 */
async function approvalCiting(
  issue: { readonly key: string; readonly primary_artifact_id: string },
  cited: string,
  actor: Parameters<typeof requestApproval>[2]
): Promise<DispatchEvent> {
  const requested = withOpenedAsk(
    await requestApproval(
      issue.primary_artifact_id,
      { summary: `Follows dispatch://${cited} closely.` },
      actor
    )
  );
  const opened = (await getIssueEvents(issue.key, { limit: 100 })).find(
    (event) => event.type === "ask.opened" && event.payload.id === requested.ask.id
  );
  if (opened === undefined) throw new Error(`the seed recorded no ask.opened for ${issue.key}`);
  return opened;
}

/** Puts an approval request just below the first page of history, behind the daemon's notes. */
async function approvalBeneathNotes(
  issue: { readonly key: string; readonly primary_artifact_id: string },
  cited: string
): Promise<DispatchEvent> {
  const opened = await approvalCiting(issue, cited, { login: "bob" });
  for (let index = 0; index < PAGE_SIZE; index += 1) {
    await createMessage(issue.key, { body: `The daemon's note ${index}.` }, asDaemon);
  }
  return opened;
}

/**
 * An issue whose history's first Show more draws, as its first entry, the line of a person's
 * approval request whose question cites another issue by its address: that issue's title is long
 * enough to wrap over several lines of Details, and the person's name is no control, so the link
 * to it is the line's first control. `PAGE_SIZE` notes of the Legion daemon's, each a thread of
 * its own, are newer than the request. Answers the issue's key, the cited issue's key, and the
 * sequence of the request's `ask.opened`.
 */
async function seedApprovalUnderFirstPage(): Promise<{ cited: string; key: string; seq: number }> {
  await createProject({ key: "CORE", name: "Core" });
  const cited = await createIssue({
    project: "CORE",
    title: `${CITED_TITLE} ${"a title long enough to wrap over several lines of Details ".repeat(3)}`,
  });
  const issue = await createIssue({
    project: "CORE",
    spec: "The plan.",
    title: "An approval request under Show more",
  });
  const opened = await approvalBeneathNotes(issue, cited.key);
  return { cited: cited.key, key: issue.key, seq: opened.seq };
}

// A keyboard press of Show more hands the focus to the first control of the first entry it drew,
// brought into Details' view by the least scroll (`IssueHistory.tsx`'s `handTo`). That control
// can be a reference in the entry's line, drawn by its address until the names of the references
// are read, then by its target's name, which can wrap over several lines and so grow the link
// after the hand-on. Found by the tester on a running build of 9bcd9f18 with a copy of a large
// real issue: about 120 ms after the press the focused link grew from 15 px to 51 px, and its
// foot stood 35 px below Details' view at 1440 px, 9 px at 390. Here the names are read late, as
// a slow answer is: the batched read of the references' names, and the cited issue's summary,
// which names a reference to an issue, are held back until the press has landed and the focus is
// read. The reader's own scroll puts Show more at the foot of Details' view before the press, so
// the hand-on brings the link in by its foot.
// SLOW(>=2s): two issues, an approval request and 20 notes written, and at each of two widths a
// sign-in and one load of the history.
test("a keyboard press of the history's Show more keeps the reference link it focused wholly in Details' view as the link's target's long name lands and wraps it", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  const seeded = await seedApprovalUnderFirstPage();
  for (const size of [DESKTOP, PHONE]) {
    const context = await reader(browser, "alice");
    const names = Promise.withResolvers<void>();
    try {
      const page = await context.newPage();
      await page.setViewportSize(size);
      for (const read of [
        "**/api/v1/reference-targets?**",
        `**/api/v1/issues/${seeded.cited}/summary`,
      ]) {
        await page.route(read, async (route) => {
          await names.promise;
          await route.continue();
        });
      }
      await gotoIssue(page, `/issues/${seeded.key}/activity`);
      await expect(marginShell(page), "/activity did not open Details").toBeVisible();
      await newestHistoryEntry(page);
      const more = historyShowMore(page);
      await more.focus();
      await showMoreAtViewFoot(page, more, size);

      await page.keyboard.press("Enter");
      const first = historyEntries(page).nth(PAGE_SIZE);
      await expect(
        first,
        "the press did not draw the approval request's line first, so the row proves nothing"
      ).toHaveAttribute("data-event-seq", String(seeded.seq));
      const link = first.locator(`.dispatch-markdown a[href="/issues/${seeded.cited}"]`);
      await expect(
        link,
        `at ${size.width} px the press did not hand the focus to the reference in the line it drew first`
      ).toBeFocused();
      await detailsStandStill(page, `the press of ${CONTROL_LABELS.show_more}`);
      await expect(
        link,
        "the cited issue's name landed before the press, so the row proves nothing"
      ).not.toContainText(CITED_TITLE);
      // Read once Details' box has stood still after the press: where the hand-on left the link.
      const handed = (await boxOf(link, "the focused link")).y;

      names.resolve();
      await expect(link, "the cited issue's name never landed").toContainText(CITED_TITLE);
      await detailsStandStill(page, "the cited issue's name landing");
      // Two reads once Details' box has stood still for two frames: the link, and the view.
      const landed = await link.evaluate((element) => {
        const at = element.getBoundingClientRect();
        return {
          bottom: at.bottom,
          focused: element === document.activeElement,
          height: at.height,
          top: at.top,
        };
      });
      const shown = await historyViewport(page);
      expect(
        handed + landed.height > shown.bottom + 1,
        `at ${size.width} px the link, ${Math.round(landed.height)} px tall once its target's name landed, fits under the top the hand-on left it at (${Math.round(handed)} px, the view ending at ${Math.round(shown.bottom)}), so the row proves nothing`
      ).toBe(true);
      expect(landed.focused, `at ${size.width} px the focus left the link`).toBe(true);
      expect(
        landed.top >= shown.top - 1 && landed.bottom <= shown.bottom + 1,
        `at ${size.width} px once the cited issue's name landed, the focused link stood at ${Math.round(landed.top)} to ${Math.round(landed.bottom)} px, out of Details' view (${Math.round(shown.top)} to ${Math.round(shown.bottom)})`
      ).toBe(true);
    } finally {
      names.resolve();
      await context.close();
    }
  }
});

// The press gives a reference link the focus before its target answers. If the target is missing,
// the renderer takes the link away. The first new entry still has controls, and the reader has
// done nothing since pressing Show more. The focus must stay in that entry, not fall to the body.
// SLOW(>=2s): one issue, an approval request and 20 notes written, then a sign-in and history load.
test("the first new history entry keeps focus when its focused reference turns out to be missing", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "The plan.",
    title: "An approval request citing a missing question",
  });
  const missing = crypto.randomUUID();
  const opened = await approvalBeneathNotes(issue, `${issue.key}/ask/${missing}`);
  const context = await reader(browser, "alice");
  const target = Promise.withResolvers<void>();
  try {
    const page = await context.newPage();
    await page.setViewportSize(DESKTOP);
    for (const read of ["**/api/v1/reference-targets?**", `**/api/v1/asks/${missing}`]) {
      await page.route(read, async (route) => {
        await target.promise;
        await route.continue();
      });
    }
    await gotoIssue(page, `/issues/${issue.key}/activity`);
    await newestHistoryEntry(page);
    await historyShowMore(page).press("Enter");

    const first = historyEntries(page).nth(PAGE_SIZE);
    await expect(first, "the request is not the first new entry").toHaveAttribute(
      "data-event-seq",
      String(opened.seq)
    );
    const link = first.locator(`.dispatch-markdown a[href="/issues/${issue.key}/asks/${missing}"]`);
    await expect(link, "Show more did not focus the unresolved reference").toBeFocused();
    target.resolve();
    await expect(link, "the missing reference is still a link").toHaveCount(0);
    await expect
      .poll(() => first.evaluate((entry) => entry.contains(document.activeElement)), {
        message: "the first new history entry lost focus when the reference disappeared",
      })
      .toBe(true);
  } finally {
    target.resolve();
    await context.close();
  }
});

/** The words of a long agents' turn: `parts` paragraphs, each `TURN_PARAGRAPH`. */
function longTurnBody(parts: number): string {
  return Array.from({ length: parts }, (_, part) => `Part ${part}: ${TURN_PARAGRAPH}`).join("\n\n");
}

// The read mark draws one line in the history, "New since you last read", where the mark falls
// among its entries (`history-model.ts`): a mark that moves back adds that line under the
// entries the reader has not read, above the reader's place, so the reader's own entry has to
// stay where it was. The reader is parked over a gap between two entries, where
// `ViewportAnchor`'s probe has to fall back to the entry below. One day's entries are drawn flush,
// one rule down their left (the prototype's `.hev`), so the gap between two entries is where one
// day ends and the next opens with its line: the first ten comments and the issue's creation are
// a day old, drawn open in a day of their own.
async function verifyGapDetection(
  browser: Browser,
  viewport?: { height: number; width: number }
): Promise<void> {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Attention test" });
  const comments: { readonly created_at: string; readonly id: string }[] = [];
  for (let index = 0; index < 20; index += 1) {
    comments.push(
      await createComment(issue.key, { body: `Gap comment ${index}` }, attentionSession)
    );
  }
  const newest = comments.at(-1);
  if (newest === undefined) throw new Error("the seed made no comment");
  const events = await getIssueEvents(issue.key, { limit: 200 });
  const dayOld = new Set(comments.slice(0, 10).map((comment) => comment.id));
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
    (event) => event.type === "comment.created" && event.payload.id === comments[11]?.id
  )?.seq;
  if (twelfth === undefined) throw new Error("the seed recorded no twelfth comment");

  const context = await reader(browser, "alice");
  try {
    const page = await context.newPage();
    if (viewport !== undefined) {
      await page.setViewportSize(viewport);
    }
    await openHistory(page, issue.key, newest);
    const divider = historyPanel(page).getByRole("listitem", {
      name: CONTROL_LABELS.history_unread,
    });
    await expect(divider).toHaveCount(0);
    // The back-dated day, named as the page names the day it files those comments under, drawn
    // open under the reader's today.
    const backDated = await historyDayOf(page, yesterday);
    await expect(
      historyPanel(page)
        .locator(`[data-history-day="${backDated.date}"]`)
        .getByText("Gap comment 0", { exact: true })
    ).toBeVisible();
    // The reader's hand leaves the list, so the place the history keeps is the one at its centre.
    await handsOff(page);

    // Where the gap nearest the middle of the history sits, as an offset from the centre of the
    // box it is clipped to: the reader is moved by that offset, so the gap lands on the very
    // point `ViewportAnchor` probes (`rowAtCenter`).
    const clip = await historyViewport(page);
    // One instant's layout is what this measures: the gaps' places now, for the scroll below.
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
    // The centre is read at this instant on purpose: the hit test below probes exactly this point.
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
// c-history (16) to (18): how the history draws a handoff, its pages and a corrected turn.
// ---------------------------------------------------------------------------------------------

// Each handoff is drawn open, its fields and every proof entry under its line with no press, as
// the conversation's handoff card showed them once opened; it folds only when the reader presses
// its fold control, and folding one leaves the others open.
// SLOW(>=2s): two handoff versions written, a sign-in and one load of the history.
test("each handoff in the history is drawn open, and the reader folds it", async ({ browser }) => {
  const key = await seedHandoffs();

  const alice = await reader(browser, "alice");
  try {
    const page = await alice.newPage();
    await gotoIssue(page, `/issues/${key}/activity`);
    await handoffLinesDrawn(page, key);
    for (const [version, summary] of [
      [2, "Second pass, review addressed"],
      [1, "First pass"],
    ] as const) {
      const line = handoffLine(page, version);
      await expect(
        line.locator('[data-handoff-field="summary"]'),
        "the handoff is not drawn open"
      ).toBeVisible();
      await expect(line.locator('[data-handoff-field="summary"]')).toHaveText(
        new RegExp(`${summary}$`, "u")
      );
      const proof = line.locator('[data-handoff-field="proof"]');
      await expect(proof, `version ${version}'s proof entries are not drawn`).toHaveCount(2);
      await expect(proof.last()).toBeVisible();
      await expect(line.getByRole("button", { name: CONTROL_LABELS.hide_fields })).toHaveAttribute(
        "aria-expanded",
        "true"
      );
    }

    // A press on version 2's fold control folds it, and version 1 stays open.
    const v2 = handoffLine(page, 2);
    await v2.getByRole("button", { name: CONTROL_LABELS.hide_fields }).click();
    await expect(
      v2.locator("[data-handoff-field]"),
      "a press on the handoff's fold control did not fold it"
    ).toHaveCount(0);
    await expect(v2.getByRole("button", { name: CONTROL_LABELS.show_fields })).toHaveAttribute(
      "aria-expanded",
      "false"
    );
    await expect(
      handoffLine(page, 1).locator('[data-handoff-field="summary"]'),
      "folding one handoff folded another"
    ).toBeVisible();
  } finally {
    await alice.close();
  }
});

// The history draws its first 20 entries, and under the last of them one control that draws the
// next 20 and says how many entries are left; each press keeps the entries drawn and adds the
// next ones, and the control goes once every entry is drawn. The seed is 44 of the daemon's
// threads under the issue's own creation, 45 entries on one page of the log, so no count carries
// "+".
// SLOW(>=2s): 44 messages written, a sign-in and one load of the history.
test("the history draws 20 entries at a time and its control says how many are left", async ({
  browser,
}) => {
  const { issue, newest } = await seedMessages("Twenty at a time", 44);
  const created = (await allEvents(issue.key)).find((event) => event.type === "issue.created");
  if (created === undefined) throw new Error(`${issue.key} recorded no issue.created`);

  const context = await reader(browser, "alice");
  try {
    const page = await context.newPage();
    await openHistory(page, issue.key, newest);
    const entries = historyEntries(page);
    await expect(entries, "the history drew 45 entries, not 20").toHaveCount(PAGE_SIZE);
    const more = historyShowMore(page);
    await expect(more, "Show more does not say that 25 entries are left").toHaveText(
      showMoreName(25)
    );

    await more.click();
    await expect(entries, "a press of Show more did not draw the next 20").toHaveCount(40);
    await expect(
      entries.first(),
      "a press of Show more took the first entries away"
    ).toHaveAttribute("data-history-entry", `thread:${newest.id}`);
    await expect(more, "Show more does not say that 5 entries are left").toHaveText(
      showMoreName(5)
    );

    await more.click();
    await expect(entries, "a second press of Show more did not draw the last 5").toHaveCount(45);
    await expect(
      entries.last(),
      "the issue's creation is not the last entry drawn"
    ).toHaveAttribute("data-event-seq", String(created.seq));
    await expect(more, "Show more stayed with every entry drawn").toHaveCount(0);
  } finally {
    await context.close();
  }
});

/** The agent that corrects its own posts in the row below, named by the title its writes stamp. */
const asCorrector = {
  actor: {
    id: "e2e-history-corrector",
    kind: "session" as const,
    origin: { session_title: "Corrections agent" },
  },
  as: "agent" as const,
};

// A post its author corrected carries, in the history, the marks every surface of `main` draws
// on one: an agents' post its agent edited reads " · edited" after its author (`EditedMark`),
// and a reply in the daemon's thread its agent deleted reads its deleted line, "Deleted by" with
// its author and its time, in place of its words, and offers no Reply (`DeletedLine`).
// SLOW(>=2s): a post written and edited, a thread written and its reply deleted, a sign-in and
// one load of the history.
test("a history turn carries the marks of a post its author corrected", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Corrected turns" });
  const post = await createComment(issue.key, { body: "The build runs at noon." }, asCorrector);
  await editComment(post.id, { body: "Corrected: the build runs at midnight." }, asCorrector);
  const root = await createMessage(
    issue.key,
    { body: "The daemon handed the build to its agent." },
    asDaemon
  );
  const reply = await createMessage(
    issue.key,
    { body: "Words the agent takes back.", in_reply_to: root.id },
    asCorrector
  );
  await deleteMessage(reply.id, asCorrector);

  const alice = await reader(browser, "alice");
  try {
    const page = await alice.newPage();
    await gotoIssue(page, `/issues/${issue.key}/activity`);
    const head = (turn: Locator) => turn.locator("p", { has: page.locator("[data-history-time]") });

    const edited = historyItem(page, post.id);
    await seededHistoryEntry(page, edited, post.created_at);
    await expect(edited).toContainText("Corrected: the build runs at midnight.");
    await expect(head(edited), "the edited turn carries no edited mark").toHaveText(
      new RegExp(` · ${CONTROL_LABELS.post_edited}$`, "u")
    );

    const deleted = historyItem(page, reply.id);
    await seededHistoryEntry(page, deleted, root.created_at);
    await expect(
      deleted.locator("[data-post-deleted]"),
      "the deleted turn does not read its deleted line"
    ).toHaveText(new RegExp(`^${CONTROL_LABELS.post_deleted_by} Corrections agent · \\S`, "u"));
    await expect(historyPanel(page).getByText("Words the agent takes back.")).toHaveCount(0);
    await expect(
      historyPanel(page).locator(`[data-history-reply="${reply.id}"]`),
      "the deleted turn offers Reply"
    ).toHaveCount(0);
    // The thread's other turn and the post are no deleted line, and the root, never edited,
    // carries no mark.
    await expect(historyItem(page, root.id)).toContainText("The daemon handed the build");
    await expect(head(historyItem(page, root.id))).not.toContainText(
      ` · ${CONTROL_LABELS.post_edited}`
    );
    await expect(historyPanel(page).locator("[data-post-deleted]")).toHaveCount(1);
  } finally {
    await alice.close();
  }
});

// ---------------------------------------------------------------------------------------------
// c-people-facts (1) and (2): `collaborators.e2e.ts` at `750e2716`.
// ---------------------------------------------------------------------------------------------

// An issue's agents stay reachable from its page once they stop: Details
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
    // `/collaborators` opens the list inside Details, and its
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
// address does not focus its row yet.
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
  const stays = await createAsk(hers.key, { question: "Which region goes first?" }, asker);
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
    // Alice reads below the reading column's width, where the row she opens is read in place of
    // the list. Bob reads above it, where the card is the reading column's.
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

    // 2. Alice hands it to bob with the control on the question's card, in the view her row
    // opens. The hand-on is her own write, so the view then reads her next question, as the
    // column does, and she goes back to the list from there.
    await selectRow(alicePage, moved.id);
    const rowControl = handOnControl(inboxPane(alicePage), moved.id);
    await expect(rowControl).toHaveCount(1);
    await expect(rowControl).toHaveValue("alice");
    await expect(inboxPane(alicePage)).toContainText("Asked of alice");
    await handTo(rowControl, "bob");
    await expect(alicePage.locator(`[data-inbox-reading-row="${stays.id}"]`)).toBeVisible();
    await backToList(alicePage);
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

    // 5. The control is on the card wherever the card renders an open ask: in the Inbox's view
    // below the column's width (pressed above), on the reading column's card, in the facts of the
    // focused ask, on a document's margin and in a document's ask block.
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
    // one line, so the row is read with its branch opened.
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
    // The replies of step 6 and the answer reach this page by reads of their own after the
    // writes, so a branch drawn after one opening is opened on the poll's next pass.
    await expect
      .poll(async () => {
        await unfoldEveryBranch(aliceIssue);
        return (await nodeRow(aliceIssue, moved.id).first().textContent()) ?? "";
      }, "the ask's row never drew its answer with its branch open")
      .toContain("Read DISPATCH_ROLLOUT_FLAG.");
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

/** The three numbers the listing carries: its total, and the size of each of its two groups. The
 *  total opens the header in both forms: `<n> unanswered asks you authored …` for the session's
 *  own listing, and `<n> unanswered asks on the <PROJECT> project's …` for `--project`. */
function printedCounts(stdout: string): { agent: number; human: number; total: number } {
  const number = (pattern: RegExp): number => {
    const found = pattern.exec(stdout);
    expect(found, `${pattern.source} is not in: ${stdout}`).not.toBeNull();
    return Number(found?.[1] ?? -1);
  };
  return {
    agent: number(/^Waiting on agent \((\d+)\):$/m),
    human: number(/^Waiting on human \((\d+)\):$/m),
    total: number(/^(\d+) unanswered asks? (?:you authored|on the [A-Z][A-Z0-9]* project's)/m),
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
    // facts.
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
// c-people-facts (5) to (11): new rows.
// ---------------------------------------------------------------------------------------------

/** The session the People row counts and unsubscribes. */
const SUBSCRIBER = "e2e-details-subscriber";

// "Subscribers" counts the sessions subscribed to the issue, opens
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

/** The facts `facts` draws, each as its name and its value (`FocusedNodeFacts.tsx`'s `Fact`), in
 *  the order it draws them. */
function drawnFacts(facts: Locator): Promise<[string, string][]> {
  return facts
    .locator("dl > div")
    .evaluateAll((all) =>
      all.map((fact): [string, string] => [
        fact.querySelector("dt")?.textContent ?? "",
        fact.querySelector("dd")?.textContent ?? "",
      ])
    );
}

// With Details shut the page reads neither the history's log,
// nor the collaborators, nor a row's references; opened, Details draws the facts and the history
// once each, inside it; the facts name the focused row's kind and who wrote it, and draw no line
// of its words, which the row beside them draws whole; the facts follow the focus; and Details
// closed and opened again keeps the facts of the row the reader was on.
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
      // Every request's path but the read mark's read of the newest event (`isReadMarkRead`),
      // which the new page sends to mark the issue read and which is no read of Details.
      const read: string[] = [];
      page.on("request", (request) => {
        const url = new URL(request.url());
        if (!isReadMarkRead(url, seeded.key)) read.push(url.pathname);
      });
      const settling = watchApiRequests(page);
      await gotoIssue(page, `/issues/${seeded.key}`);
      await expect(tree(page)).toBeVisible();
      // The comments arrive by a read of their own after the tree, so the load's reads settle
      // before the branches they draw are opened.
      await expectLoadSettled(settling);
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
      const kind = [`${CONTROL_LABELS.node_kind}:`, sentenceCase(NODE_TYPE_NAMES.comment)];
      const author = [`${CONTROL_LABELS.written_by}:`, "bob"];
      await expect
        .poll(() => drawnFacts(facts), "the facts do not name the row's kind and who wrote it")
        .toEqual(expect.arrayContaining([kind, author]));
      await expect(facts, "the facts drew the row's first line").not.toContainText(
        "The first row's words."
      );
      // The collaborators are read only once their list opens, and Details opens with it
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
      await expect
        .poll(() => drawnFacts(facts), "the facts of the next node do not name its kind and author")
        .toEqual(expect.arrayContaining([kind, author]));
      await expect(facts, "the facts drew the next row's first line").not.toContainText(
        "The second row's words."
      );

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

/** An issue with an agents' thread, a thread the Legion daemon began that no person wrote in,
 *  dated a day back with its event, so it stands in a day before today, below each of the
 *  daemon's 24 notes of today, each a thread of its own, and past the history's first 20
 *  entries: its id, the time it is dated to (`messageAt`), and the newest of the notes after it
 *  (`newest`). */
async function seedAgentsThread(): Promise<{
  key: string;
  message: string;
  messageAt: string;
  newest: { readonly created_at: string; readonly id: string };
}> {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "The history's addresses" });
  await createComment(issue.key, { body: "A person's comment, a row of the outline." });
  for (let index = 0; index < 12; index += 1) {
    await createMessage(issue.key, { body: `An agent's earlier note ${index}.` }, asDaemon);
  }
  const message = await createMessage(
    issue.key,
    { body: "The agents' turn an address names." },
    asDaemon
  );
  let newest = await createMessage(issue.key, { body: "An agent's later note 0." }, asDaemon);
  for (let index = 1; index < 12; index += 1) {
    newest = await createMessage(issue.key, { body: `An agent's later note ${index}.` }, asDaemon);
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
  return { key: issue.key, message: message.id, messageAt: yesterday, newest };
}

// `t v` opens `/activity` with the history's heading in view and the
// focus in the history, and a Close there is the later word; `/collaborators` and `t o` open the
// People section the reader closed, with the collaborators' list open and the focus on its
// control; and Keys lists both keys. The address of a turn of the daemon's thread, a person's
// reply in it included, opens Details at that turn, the history drawn down to it past its first
// 20 entries, marked and on screen, once per following of the address: Pinned's link to the
// turn, followed again while Details is open, brings it on screen again, and the reader's Close
// is the later word. A turn that arrives while the history's log is held is reached at once. The
// address of an agent's note to the issue's people, or of a message between agents, opens no
// Details: neither is a turn of a thread the history holds.
test("t v and t o open Details at the history and at the collaborators, and an agents' turn's address marks it", async ({
  browser,
}) => {
  // One seed and ten loads of the issue, each address's landing waited for.
  test.setTimeout(120_000);
  await phase(browser, seedAgentsThread, async (page, seeded) => {
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expect(tree(page)).toBeVisible();
    await expect(marginShell(page)).toBeHidden();
    await expectOpeningLanded(page);

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
      historyHeading(page),
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
    await expectOpeningLanded(page);
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

    // The turn is pinned, so Pinned links its address (`IssuePins.tsx`).
    const dismissed = (await getMyState("alice"))[seeded.key]?.dismissed ?? [];
    await putIssueState(seeded.key, {
      dismissed: [...dismissed, pinnedItemMarker(conversationItemId("message", seeded.message))],
    });

    // The turn's own address, on a fresh load, opens Details there: the history drawn down to the
    // turn, past its first 20 entries, the turn marked and on screen in its day before today.
    await gotoIssue(page, `/issues/${seeded.key}/messages/${seeded.message}`);
    await expect(marginShell(page), "the turn's address did not open Details").toBeVisible();
    const turn = historyItem(page, seeded.message);
    await expect(turn, "the turn's address did not mark it").toHaveAttribute(
      "aria-current",
      "true"
    );
    await expect(turn, "the turn's address left it off screen").toBeInViewport();
    await seededHistoryEntry(page, turn, seeded.messageAt);
    const place = await historyEntries(page).evaluateAll(
      (entries, id) =>
        entries.findIndex((entry) => entry.querySelector(`[data-history-item="${id}"]`) !== null),
      seeded.message
    );
    expect(
      place,
      `the turn is entry ${place} of the history, among its first ${PAGE_SIZE}, so the row proves nothing`
    ).toBeGreaterThanOrEqual(PAGE_SIZE);

    // Pinned's link to the turn, followed again while Details is open, brings the turn back on
    // screen from where the reader scrolled to Pinned, above the history.
    const pinned = pinsList(page)
      .locator(`[data-pin="message:${seeded.message}"]`)
      .getByRole("link");
    await expect(pinned, "Pinned does not link the turn's address").toHaveAttribute(
      "href",
      `/issues/${seeded.key}/messages/${seeded.message}`
    );
    await pinned.scrollIntoViewIfNeeded();
    await detailsStandStill(page, "the scroll to Pinned");
    await expect(
      turn,
      "the scroll to Pinned left the turn on screen, so the row proves nothing"
    ).not.toBeInViewport();
    await pinned.click();
    await expect(
      turn,
      "Pinned's link to the turn did not bring it on screen again"
    ).toBeInViewport();
    await expect(turn).toHaveAttribute("aria-current", "true");
    await expect(marginShell(page)).toBeVisible();
    // Once per following of the address: the reader's Close is the later word.
    await closeDetails(page);
    await nextFrames(page);
    await expect(marginShell(page), "Details opened again after the reader closed it").toBeHidden();

    // A person's reply in the daemon's thread is a turn of it, and its address opens Details at
    // it the same way, once.
    const personReply = await createMessage(
      seeded.key,
      { body: "A person answers in the daemon's thread.", in_reply_to: seeded.message },
      { login: "bob" }
    );
    await gotoIssue(page, `/issues/${seeded.key}/messages/${personReply.id}`);
    await expect(
      marginShell(page),
      "the address of a person's reply in the daemon's thread did not open Details"
    ).toBeVisible();
    const reply = historyItem(page, personReply.id);
    await expect(reply, "the person's reply's address did not mark it").toHaveAttribute(
      "aria-current",
      "true"
    );
    await expect(reply).toBeInViewport();
    await closeDetails(page);
    await nextFrames(page);
    await expect(
      marginShell(page),
      "Details opened again at the person's reply after the reader closed it"
    ).toBeHidden();

    // An agent's note to the issue's people, and a message between agents, are no turn of a
    // thread the history holds: the outline lands on each one's own row, and Details stays shut.
    const note = await createMessage(
      seeded.key,
      { body: "A note to the issue's people from an agent." },
      attentionSession
    );
    const betweenAgents = await createMessage(
      seeded.key,
      { body: "Reviewer, the second task waits on you.", delivery: "btw", target: "role:reviewer" },
      attentionSession
    );
    for (const [id, what] of [
      [note.id, "an agent's note to the issue's people"],
      [betweenAgents.id, "a message between agents"],
    ] as const) {
      await gotoIssue(page, `/issues/${seeded.key}/messages/${id}`);
      await expect
        .poll(
          () => focusedNodeId(page),
          `the address of ${what} landed the outline on no row of it`
        )
        .toBe(id);
      await nextFrames(page);
      await expect(marginShell(page), `the address of ${what} opened Details`).toBeHidden();
    }

    // A turn that arrives while the history's log is held in the app is reached at once: the
    // history reads the log at `/activity`, Details closes, the turn arrives, and a person's
    // comment in the outline links its address. Reached in the app - the router's own history
    // moving to it, as a link the router follows moves it, with no reload - the turn is marked
    // and on screen. The link in the comment's words is an anchor the browser follows, which
    // loads the page again, and it reaches the turn too.
    await gotoIssue(page, `/issues/${seeded.key}/activity`);
    await seededHistoryEntry(page, historyItem(page, seeded.newest.id), seeded.newest.created_at);
    await expect(historyPanel(page).getByText("An agent's later note 11.")).toBeVisible();
    await closeDetails(page);
    await page.evaluate(() => document.documentElement.setAttribute("data-loaded-once", "yes"));
    const arrived = await createMessage(
      seeded.key,
      { body: "A turn that arrived while the log was held." },
      asDaemon
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

/** An issue whose one row is a person's comment, with a thread the Legion daemon began: one
 *  message, which the outline draws no row for and the history draws (`agents-only.ts`). */
async function seedPinnedAgentsTurn(): Promise<{
  comment: string;
  key: string;
  message: string;
}> {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "An agents' turn a pin names" });
  const comment = await createComment(issue.key, {
    body: "A person's comment, the outline's one row.",
  });
  const message = await createMessage(
    issue.key,
    { body: "The agents' turn the reader pinned." },
    asDaemon
  );
  return { comment: comment.id, key: issue.key, message: message.id };
}

// A pin's link to a turn of a thread the history holds, the daemon's here. The new page gives
// such a turn no Pin of its own: the outline draws no row for it, so no row's facts hold its Pin,
// and the history's Pin on a line of it pins that line's event, whose link in Pinned opens
// `/activity` and names no turn. Today's page pins it - its Conversation card's Pin, under the
// thread's own `message:<id>` - and Pinned on the new page links that pin to the turn's address
// (`IssuePins.tsx`). Followed, the address lands the outline on the turn's nearest drawn
// ancestor - the issue here, which is no row, so the outline's tab stop, the person's comment -
// with the note that the thread is in the history, in Details, and opens Details at the history
// with the turn marked, by one decision (`address-target.ts`'s `useAddressFallback`).
test("a pin's link to an agents' turn lands the outline on its nearest drawn ancestor with the note and opens Details at the turn marked", async ({
  browser,
}) => {
  await phase(browser, seedPinnedAgentsTurn, async (page, seeded) => {
    // The reader pins the turn on today's page, in a context of their own that leaves the new
    // page off, and the pin is stored before the new page reads the reader's state.
    const tabs = await asUser(browser, "alice");
    try {
      const today = await tabs.newPage();
      await today.goto(`/issues/${seeded.key}/conversation`);
      const card = today.locator(`[data-turn="message:${seeded.message}"]`);
      const stored = today.waitForResponse(
        (response) =>
          response.request().method() === "PUT" &&
          new URL(response.url()).pathname === `/api/v1/me/issues/${seeded.key}/state`
      );
      await card.getByRole("button", { exact: true, name: "Pin" }).click();
      expect((await stored).ok(), "today's page did not store the pin").toBe(true);
      await expect(card.getByRole("button", { exact: true, name: "Unpin" })).toHaveAttribute(
        "aria-pressed",
        "true"
      );
    } finally {
      await tabs.close();
    }

    const address = `/issues/${seeded.key}/messages/${seeded.message}`;
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expect(tree(page)).toBeVisible();
    await openDetails(page);
    const link = pinsList(page).locator(`[data-pin="message:${seeded.message}"]`).getByRole("link");
    await expect(link, "Pinned does not link the agents' turn's address").toHaveAttribute(
      "href",
      address
    );
    await link.click();
    await expect(page).toHaveURL(new RegExp(`${address}$`));

    // The outline lands on the tab stop and says there that the thread is in the history.
    await expect
      .poll(() => focusedNodeId(page), "the pin's link landed the outline on no row")
      .toBe(seeded.comment);
    await expect(
      nodeRow(page, seeded.comment).first().locator("[data-row-report]"),
      "the outline's landing did not say at its row that the thread is in the history"
    ).toHaveText(KEY_REPORTS.agents_thread);
    // Read once the outline has landed for this address, so it has drawn its rows for it.
    await expect(nodeRow(page, seeded.message), "the outline drew the agents' turn").toHaveCount(0);

    // Details, left open by the link, is at the history with the turn marked and on screen.
    await expect(marginShell(page), "the pin's link closed Details").toBeVisible();
    const turn = historyItem(page, seeded.message);
    await expect(turn, "the pin's link did not mark the agents' turn").toHaveAttribute(
      "aria-current",
      "true"
    );
    await expect(turn).toBeInViewport();
  });
});

// `t o`, and a load of `/collaborators`, put the focus on the Collaborators control: Details
// brings the control's row into its view, then focuses it. A row above it that reads what it
// shows once Details opens - Blocked by, which reads the titles of the issues it names - grows
// when that read lands, after the focus, and pushes the focused control down out of Details'
// view, where the reader cannot see the focus. Found by the tester on a running build of 7c09739d
// with a copy of a large real issue whose Blocked by names five issues: `t o` and three loads of
// `/collaborators` each left the focused Collaborators control at 1,135 px, under Details' view
// (53 to 900 px) at 1440 px, once the five titles landed and the row grew from 181 px to 481 px.
// Here the titles' read is held back until the focus has landed, as a slow answer is, so it lands after the focus.
test("t o and a load of /collaborators keep the focused Collaborators control in Details' view when a row above it grows", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "An issue that waits on five others" });
  const blockers: string[] = [];
  for (let index = 0; index < 5; index += 1) {
    // `force`: the five titles are alike on purpose, and the duplicate check would refuse them.
    const blocker = await createIssue({
      force: true,
      project: "CORE",
      title: `Blocker ${index}: ${"a title long enough to wrap over several lines of Details ".repeat(3)}`,
    });
    blockers.push(blocker.key);
  }
  await patchIssue(issue.key, { blocked_by: blockers });
  const context = await reader(browser, "alice");
  let titles = Promise.withResolvers<void>();
  try {
    const page = await context.newPage();
    await page.setViewportSize(DESKTOP);
    await page.route("**/api/v1/reference-targets?**", async (route) => {
      await titles.promise;
      await route.continue();
    });
    const collaborators = marginShell(page)
      .locator('[data-details-row="collaborators"]')
      .getByRole("button", { exact: true, name: PROPERTY_LABELS.collaborators });
    const blockedBy = detailsRow(page, PROPERTY_LABELS.blocked_by);
    // Where the focused control stands against the box Details scrolls in, once the titles are in.
    const focusedPlace = async (how: string) => {
      await expect(collaborators, `${how} did not focus the Collaborators control`).toBeFocused();
      const before = (await blockedBy.boundingBox())?.height ?? 0;
      titles.resolve();
      await expect(blockedBy, "the blockers' titles never landed").toContainText("Blocker 4:");
      // Half a second for whatever the page does about the taller row to finish: a landing that
      // scrolls again, or a keeper that puts the control back, has done so well within it.
      await page.waitForTimeout(500);
      await nextFrames(page);
      const grew = ((await blockedBy.boundingBox())?.height ?? 0) - before;
      expect(
        grew > 100,
        `Blocked by grew ${Math.round(grew)} px once the titles landed, so the row proves nothing`
      ).toBe(true);
      const place = await collaborators.evaluate((control) => {
        let box = control.parentElement;
        while (
          box !== null &&
          !(
            /(auto|scroll)/.test(getComputedStyle(box).overflowY) &&
            box.scrollHeight > box.clientHeight
          )
        ) {
          box = box.parentElement;
        }
        const view = box === null ? { bottom: innerHeight, top: 0 } : box.getBoundingClientRect();
        const at = control.getBoundingClientRect();
        return {
          bottom: at.bottom,
          focused: control === document.activeElement,
          top: at.top,
          viewBottom: view.bottom,
          viewTop: view.top,
        };
      });
      expect(place.focused, `${how}: the focus left the Collaborators control`).toBe(true);
      expect(
        place.top >= place.viewTop - 1 && place.bottom <= place.viewBottom + 1,
        `${how}: once the blockers' titles landed, the focused Collaborators control stood at ${Math.round(place.top)} px, out of Details' view (${Math.round(place.viewTop)} to ${Math.round(place.viewBottom)})`
      ).toBe(true);
    };

    await gotoIssue(page, `/issues/${issue.key}`);
    // The issue holds no node, so its tree is drawn empty: the page is drawn once it is there.
    await expectIssuePageDrawn(page);
    await expect(marginShell(page)).toBeHidden();
    await focusThePage(page);
    await page.keyboard.press("t");
    await page.keyboard.press("o");
    await expect(page).toHaveURL(new RegExp(`/issues/${issue.key}/collaborators$`));
    await focusedPlace("t o");

    titles = Promise.withResolvers<void>();
    await gotoIssue(page, `/issues/${issue.key}/collaborators`);
    await focusedPlace("a load of /collaborators");
  } finally {
    titles.resolve();
    await context.close();
  }
});

// The hold of the place `t o` lands the focus at, as the reader acts around it. Each row seeds
// the `t o` row's five blockers, whose titles' read it holds back until it lets it through, so
// Blocked by grows once the reader has acted.

const TABLET = { height: 1024, width: 768 } as const;

/** An issue whose Blocked by names five issues with titles long enough to wrap over several
 *  lines of Details, as the `t o` row above seeds it; with `waiting`, five more such issues wait
 *  on it, which Blocks lists once Details has read them. */
async function seedHeldRows({ waiting }: { waiting: boolean }): Promise<string> {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "An issue that waits on five others" });
  const wrapped = "a title long enough to wrap over several lines of Details ".repeat(3);
  const blockers: string[] = [];
  for (let index = 0; index < 5; index += 1) {
    // `force`: the five titles are alike on purpose, and the duplicate check would refuse them.
    const blocker = await createIssue({
      force: true,
      project: "CORE",
      title: `Blocker ${index}: ${wrapped}`,
    });
    blockers.push(blocker.key);
  }
  await patchIssue(issue.key, { blocked_by: blockers });
  for (let index = 0; waiting && index < 5; index += 1) {
    // `force`: as for the blockers above.
    const waiter = await createIssue({
      force: true,
      project: "CORE",
      title: `Waiter ${index}: ${wrapped}`,
    });
    await patchIssue(waiter.key, { blocked_by: [issue.key] });
  }
  return issue.key;
}

/** The reads Details makes once it opens that a row holds back, each until its promise settles:
 *  the blockers' titles, and, where named, the issue's log and the issues waiting on it with
 *  their titles. Blocks reads what waits on the issue, then those issues' titles in a read of
 *  their own, so the issues and their titles land together, as a reader's slow read of what
 *  waits brings both. */
interface HeldReads {
  readonly log?: Promise<void>;
  readonly titles: Promise<void>;
  readonly waiting?: Promise<void>;
}

/** A reader's page at `size` on `key`, loaded with Details shut, then `t o`: Details open at
 *  the collaborators' row and the focus on its control, with `held`'s reads held back. */
async function afterTo(
  browser: Browser,
  key: string,
  size: { readonly height: number; readonly width: number },
  held: HeldReads
): Promise<{ context: BrowserContext; page: Page }> {
  const context = await reader(browser, "alice");
  const page = await context.newPage();
  await page.setViewportSize(size);
  await page.route(new RegExp(`/api/v1/issues/${key}/events`), async (route) => {
    await held.log;
    await route.continue();
  });
  await page.route("**/api/v1/references?**", async (route) => {
    if (new URL(route.request().url()).searchParams.get("kind") === "blocked_by") {
      await held.waiting;
    }
    await route.continue();
  });
  // A read of titles that names a blocker is the blockers' read, held by `titles`; any other is
  // the read of the titles of what waits on the issue, held with that read by `waiting`.
  const blockers = new Set(
    ((await getIssue(key)).blocked_by ?? []).map((blocker) => `dispatch://${blocker}`)
  );
  await page.route("**/api/v1/reference-targets?**", async (route) => {
    const refs = new URL(route.request().url()).searchParams.getAll("ref");
    await (refs.some((ref) => blockers.has(ref)) ? held.titles : (held.waiting ?? held.titles));
    await route.continue();
  });
  await gotoIssue(page, `/issues/${key}`);
  // The issue holds no node, so its tree is drawn empty: the page is drawn once it is there.
  await expectIssuePageDrawn(page);
  await expect(marginShell(page)).toBeHidden();
  await focusThePage(page);
  await page.keyboard.press("t");
  await page.keyboard.press("o");
  await expect(page).toHaveURL(new RegExp(`/issues/${key}/collaborators$`));
  await expect(
    collaboratorsControl(page),
    "t o did not focus the Collaborators control"
  ).toBeFocused();
  return { context, page };
}

/** The collaborators' row of Details' People section, which `t o` opens Details at. */
function collaboratorsRow(page: Page): Locator {
  return marginShell(page).locator('[data-details-row="collaborators"]');
}

/** The Collaborators control of Details' People section, which `t o` focuses. */
function collaboratorsControl(page: Page): Locator {
  return collaboratorsRow(page).getByRole("button", {
    exact: true,
    name: PROPERTY_LABELS.collaborators,
  });
}

/** Where `locator` is drawn; `what` names it where it is drawn nowhere. */
async function boxOf(locator: Locator, what: string) {
  // Each caller reads a place after the page settled, so one instant's box is the place it means.
  const box = await locator.boundingBox();
  if (box === null) throw new Error(`${what} has no box`);
  return box;
}

/** True where `box` lies wholly inside `view`. */
function wholly(
  box: { readonly height: number; readonly y: number },
  view: { readonly bottom: number; readonly top: number }
): boolean {
  return box.y >= view.top && box.y + box.height <= view.bottom;
}

/** The reader turning the wheel over the middle of Details' view by `by` px, until the box has
 *  moved and then stood still for two frames. */
async function wheelOverDetails(page: Page, by: number): Promise<void> {
  const view = await historyViewport(page);
  const shell = await boxOf(marginShell(page), "Details");
  const from = await historyScrollTop(page);
  await page.mouse.move(shell.x + shell.width / 2, (view.top + view.bottom) / 2);
  await page.mouse.wheel(0, by);
  await expect
    .poll(() => historyScrollTop(page), { message: "the wheel did not scroll Details" })
    .not.toBe(from);
  await detailsStandStill(page, "the wheel");
}

/** Lets a held read through with `release`, waits for `row` to draw `drawn` and for what the
 *  page does about the taller row to finish, and answers how far `row` grew. */
async function grows(
  page: Page,
  row: Locator,
  what: string,
  release: () => void,
  drawn: string
): Promise<number> {
  const before = (await boxOf(row, what)).height;
  release();
  await expect(row, `${what} never drew what it read`).toContainText(drawn);
  // Half a second for whatever the page does about the taller row to finish: a hold or an
  // anchor that puts a row back has done so well within it.
  await page.waitForTimeout(500);
  await nextFrames(page);
  const grew = (await boxOf(row, what)).height - before;
  expect(
    grew > 100,
    `${what} grew ${Math.round(grew)} px once its read landed, so the row proves nothing`
  ).toBe(true);
  return grew;
}

// After `t o` the reader turns the wheel over Details until Blocked by's top is above the view,
// its last 80 px still in it, the focused Collaborators control below them in the view. Blocked
// by then grows as the titles of the issues it names land. The browser's own scroll anchoring
// keeps a node in the part of Blocked by left in view, which a growth below that node does not
// move, so it puts nothing back; the hold of the place `t o` landed the focus at had ended at the
// wheel, and every row under Blocked by went down by the growth, the focused control with them.
// Found on a running build with a copy of a large real issue: the control went from 635 to 935 px
// in a view of 53 to 900 at 1440 px, and the same at 768. The hold now lasts while the focus
// stays on the control, and takes the place where the reader's scroll left it.
test("t o, then a wheel that leaves Blocked by partly in view above the focused control: the control stays where the wheel left it as Blocked by grows", async ({
  browser,
}) => {
  test.setTimeout(180_000);
  const key = await seedHeldRows({ waiting: false });
  // Each width's move of the control, so one run says every width the growth moved it at.
  const moved: string[] = [];
  for (const size of [DESKTOP, TABLET]) {
    const titles = Promise.withResolvers<void>();
    const { context, page } = await afterTo(browser, key, size, { titles: titles.promise });
    try {
      const blockedBy = detailsRow(page, PROPERTY_LABELS.blocked_by);
      const view = await historyViewport(page);
      const blocked = await boxOf(blockedBy, "Blocked by");
      await wheelOverDetails(page, Math.round(blocked.y + blocked.height - 80 - view.top));
      const left = await boxOf(blockedBy, "Blocked by");
      expect(
        left.y < view.top && left.y + left.height > view.top,
        `at ${size.width} px the wheel left Blocked by at ${Math.round(left.y)} to ${Math.round(left.y + left.height)} px, not across the view's top at ${Math.round(view.top)}`
      ).toBe(true);
      // One read of the place the wheel left: the box has stood still for two frames, and the
      // titles, which alone move it next, are still held.
      const before = await boxOf(collaboratorsControl(page), "the Collaborators control");
      expect(
        wholly(before, view),
        `at ${size.width} px the wheel took the Collaborators control out of Details' view`
      ).toBe(true);

      const grew = await grows(page, blockedBy, "Blocked by", titles.resolve, "Blocker 4:");
      // One read once the titles have landed and the half second in `grows` has passed.
      const after = await boxOf(collaboratorsControl(page), "the Collaborators control");
      await expect(collaboratorsControl(page)).toBeFocused();
      if (Math.abs(after.y - before.y) > 1) {
        moved.push(
          `at ${size.width} px, after the wheel, Blocked by grew ${Math.round(grew)} px and moved the focused Collaborators control from ${Math.round(before.y)} px to ${Math.round(after.y)} px`
        );
      }
    } finally {
      titles.resolve();
      await context.close();
    }
  }
  expect(moved, moved.join("; ")).toEqual([]);
});

// After `t o` the reader presses Shift, a key that leaves the focus on the control and scrolls
// nothing, and Blocked by, in view above the control, grows as the titles land. The hold had
// ended at the key, and the growth pushed the focused control down out of Details' view. Found on
// a running build with a copy of a large real issue: the control went from 835 to 1,135 px in a
// view of 53 to 900 at 1440 px, and the same at 768 and 390. The hold now lasts while the focus
// stays on the control.
test("t o, then Shift: the focused Collaborators control stays where it landed as Blocked by grows above it", async ({
  browser,
}) => {
  test.setTimeout(240_000);
  const key = await seedHeldRows({ waiting: false });
  // Each width's move of the control, so one run says every width the growth moved it at.
  const moved: string[] = [];
  for (const size of [DESKTOP, TABLET, PHONE]) {
    const titles = Promise.withResolvers<void>();
    const { context, page } = await afterTo(browser, key, size, { titles: titles.promise });
    try {
      await page.keyboard.press("Shift");
      await nextFrames(page);
      // One read of where the control landed: the key moved nothing, and the titles, which alone
      // move it next, are still held.
      const before = await boxOf(collaboratorsControl(page), "the Collaborators control");
      const blockedBy = detailsRow(page, PROPERTY_LABELS.blocked_by);
      const grew = await grows(page, blockedBy, "Blocked by", titles.resolve, "Blocker 4:");
      // One read once the titles have landed and the half second in `grows` has passed.
      const after = await boxOf(collaboratorsControl(page), "the Collaborators control");
      await expect(collaboratorsControl(page)).toBeFocused();
      if (Math.abs(after.y - before.y) > 1) {
        moved.push(
          `at ${size.width} px, after Shift, Blocked by grew ${Math.round(grew)} px and moved the focused Collaborators control from ${Math.round(before.y)} px to ${Math.round(after.y)} px`
        );
      }
    } finally {
      titles.resolve();
      await context.close();
    }
  }
  expect(moved, moved.join("; ")).toEqual([]);
});

// At 390 px the reader's wheel after `t o` takes Blocked by wholly above Details' view. Its
// growth as the titles land is then above every row in view, and the browser's own scroll
// anchoring keeps those rows where they are, while the hold, which keeps the control's place
// from where the wheel left it, finds nothing to put back. Found on a running build at 390 px,
// where nothing moved; the row holds that case, which the hold that ended at the wheel kept too.
test("t o, then a wheel at 390 px that takes Blocked by wholly above Details' view: no row in view moves as Blocked by grows", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  const key = await seedHeldRows({ waiting: false });
  const titles = Promise.withResolvers<void>();
  const { context, page } = await afterTo(browser, key, PHONE, { titles: titles.promise });
  try {
    const blockedBy = detailsRow(page, PROPERTY_LABELS.blocked_by);
    const view = await historyViewport(page);
    const blocked = await boxOf(blockedBy, "Blocked by");
    await wheelOverDetails(page, Math.ceil(blocked.y + blocked.height - view.top) + 8);
    const left = await boxOf(blockedBy, "Blocked by");
    expect(
      left.y + left.height <= view.top,
      `the wheel left Blocked by's foot at ${Math.round(left.y + left.height)} px, in the view from ${Math.round(view.top)}`
    ).toBe(true);
    // One read of the place the wheel left: the box has stood still for two frames, and the
    // titles, which alone move it next, are still held. The row at the middle of the view
    // stands for every row in it.
    const shell = await boxOf(marginShell(page), "Details");
    const middle = await page.evaluateHandle(({ x, y }) => document.elementFromPoint(x, y), {
      x: shell.x + shell.width / 2,
      y: (view.top + view.bottom) / 2,
    });
    const middleTop = () => middle.evaluate((element) => element?.getBoundingClientRect().top);
    const before = await middleTop();
    expect(before, "nothing is drawn at the middle of Details' view").toBeDefined();

    const grew = await grows(page, blockedBy, "Blocked by", titles.resolve, "Blocker 4:");
    // One read once the titles have landed and the half second in `grows` has passed.
    const after = await middleTop();
    expect(
      Math.abs((after ?? Number.POSITIVE_INFINITY) - (before ?? 0)) <= 1,
      `Blocked by grew ${Math.round(grew)} px above Details' view and moved the row at its middle from ${Math.round(before ?? 0)} px to ${Math.round(after ?? 0)} px`
    ).toBe(true);
  } finally {
    titles.resolve();
    await context.close();
  }
});

// The hold keeps the control's place only while the control lies at least partly in Details'
// view. After `t o` the reader turns the wheel up until the control is wholly below the view, and
// Blocked by, in view above, grows as the titles land. Keeping the control's place then would
// scroll the box by the growth and take the rows the reader turned to up out of the view: the
// hold yields, and Blocked by's top stays where the reader sees it. The reader turns the wheel
// down until the control is in view again, which makes it the place to keep once more, and Blocks,
// in view above it, grows as the issues waiting on this one land: the control stays where the
// wheel left it. The window is short, so the control lies out of view below the rows above it.
test("t o, then a wheel that takes the focused control out of Details' view: the hold yields as Blocked by grows, and keeps the control again once a wheel brings it back", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  const key = await seedHeldRows({ waiting: true });
  const titles = Promise.withResolvers<void>();
  const waiting = Promise.withResolvers<void>();
  const { context, page } = await afterTo(
    browser,
    key,
    { height: 700, width: 1440 },
    { titles: titles.promise, waiting: waiting.promise }
  );
  try {
    const blockedBy = detailsRow(page, PROPERTY_LABELS.blocked_by);
    const view = await historyViewport(page);
    const landed = await boxOf(collaboratorsControl(page), "the Collaborators control");
    await wheelOverDetails(page, -Math.ceil(view.bottom - landed.y + 24));
    const below = await boxOf(collaboratorsControl(page), "the Collaborators control");
    expect(
      below.y >= view.bottom,
      `the wheel left the Collaborators control at ${Math.round(below.y)} px, not below Details' view, which ends at ${Math.round(view.bottom)}`
    ).toBe(true);
    // One read of the place the wheel left: the box has stood still for two frames, and the
    // titles, which alone move it next, are still held.
    const blockedTop = (await boxOf(blockedBy, "Blocked by")).y;
    expect(
      blockedTop >= view.top && blockedTop < view.bottom,
      `Blocked by's top stood at ${Math.round(blockedTop)} px, out of Details' view`
    ).toBe(true);

    const grew = await grows(page, blockedBy, "Blocked by", titles.resolve, "Blocker 4:");
    // One read once the titles have landed and the half second in `grows` has passed.
    const blockedAfter = (await boxOf(blockedBy, "Blocked by")).y;
    expect(
      Math.abs(blockedAfter - blockedTop) <= 1,
      `with the focused control out of view, Blocked by grew ${Math.round(grew)} px and its top went from ${Math.round(blockedTop)} px to ${Math.round(blockedAfter)} px`
    ).toBe(true);

    const away = await boxOf(collaboratorsControl(page), "the Collaborators control");
    await wheelOverDetails(page, Math.ceil(away.y + away.height + 16 - view.bottom));
    // One read of the place the wheel left: the box has stood still for two frames, and the
    // issues waiting on this one, which alone move it next, are still held.
    const before = await boxOf(collaboratorsControl(page), "the Collaborators control");
    expect(
      wholly(before, view),
      `the wheel back left the Collaborators control at ${Math.round(before.y)} px, out of Details' view`
    ).toBe(true);
    const blocks = detailsRow(page, PROPERTY_LABELS.blocks);
    const blocksAt = await boxOf(blocks, "Blocks");
    expect(
      blocksAt.y >= view.top,
      `Blocks stood at ${Math.round(blocksAt.y)} px, above Details' view, where the browser's own anchoring keeps the control`
    ).toBe(true);
    const blocksGrew = await grows(page, blocks, "Blocks", waiting.resolve, "Waiter 4:");
    // One read once the waiting issues have landed and the half second in `grows` has passed.
    const after = await boxOf(collaboratorsControl(page), "the Collaborators control");
    await expect(collaboratorsControl(page)).toBeFocused();
    expect(
      Math.abs(after.y - before.y) <= 1,
      `back in view, Blocks grew ${Math.round(blocksGrew)} px above the focused Collaborators control and moved it from ${Math.round(before.y)} px to ${Math.round(after.y)} px`
    ).toBe(true);
  } finally {
    titles.resolve();
    waiting.resolve();
    await context.close();
  }
});

// The history's anchor keeps the place of the entry the reader's pointer rests on in Details' box
// (`ViewportAnchor.tsx`), with the browser's own scroll anchoring off there while it does, and the
// hold of the place `t o` landed the focus at keeps the Collaborators control's place in the same
// box by scrolling it in its own resize observer (`details-more.tsx`). Blocks grows above the
// control when the issues waiting on this one are read, and the hold scrolls the box by as much
// before the anchor's observer runs. That scroll of the hold's left the anchor's kept place stale:
// the anchor found the box off the offset it kept the place at, put nothing back, and then took
// the scroll's event for no move of the view. Once the reader moved the focus off the control
// with Shift+Tab, which ends the hold, Blocked by, wholly above the view, grew when the titles of
// the issues it names landed, and every row in Details' view went down by as much, with nothing
// to put them back. The three reads are held until the row lets each go: the log until the hold
// has the focus, so the anchor's observer starts after the hold's, as it does when the log
// answers slowly; the waiting issues until the pointer is on an entry; the titles until the focus
// has left the control.
test("a growth above Details' view once the t o hold has ended leaves the rows in view where they stood, though the hold scrolled while the history kept the pointer's entry", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  const key = await seedHeldRows({ waiting: true });
  const log = Promise.withResolvers<void>();
  const waiting = Promise.withResolvers<void>();
  const titles = Promise.withResolvers<void>();
  // Tall enough that Details shows the history's entries beside the Collaborators control.
  const { context, page } = await afterTo(
    browser,
    key,
    { height: 1600, width: 1440 },
    { log: log.promise, titles: titles.promise, waiting: waiting.promise }
  );
  try {
    // The hold looks for the focus two frames after the key and holds from the frame it finds
    // it; two frames more, and the log is let through.
    await nextFrames(page);
    await nextFrames(page);
    log.resolve();
    const entry = await newestHistoryEntry(page);
    await expect(entry, "the history drew no entry").toBeAttached();
    await nextFrames(page);

    // The reader's pointer comes to rest on the history's first entry.
    const entryAt = await boxOf(entry, "the history's first entry");
    const shown = await historyViewport(page);
    expect(
      wholly(entryAt, shown),
      `the history's first entry stood out of Details' view (${Math.round(entryAt.y)} px)`
    ).toBe(true);
    const pointer = { x: entryAt.x + entryAt.width / 2, y: entryAt.y + entryAt.height / 2 };
    await page.mouse.move(pointer.x, pointer.y);
    await nextFrames(page);

    // The issues waiting on this one land, Blocks grows above the control, and the hold keeps
    // the control where it landed.
    const blocks = detailsRow(page, PROPERTY_LABELS.blocks);
    await grows(page, blocks, "Blocks", waiting.resolve, "Waiter 4:");

    // The reader moves the focus off the control, to the Subscribers control above it in view,
    // which ends the hold and scrolls nothing.
    await page.keyboard.press("Shift+Tab");
    await expect(collaboratorsControl(page)).not.toBeFocused();
    await nextFrames(page);
    // One read of the place the focus move left: the box has not moved since the hold's scroll,
    // and the titles, which alone move it next, are still held. Details' box scrolls now that
    // Blocks has grown, so its view is read again.
    const view = await historyViewport(page);
    const blockedBy = detailsRow(page, PROPERTY_LABELS.blocked_by);
    const blockedAt = await boxOf(blockedBy, "Blocked by");
    expect(
      blockedAt.y + blockedAt.height <= view.top,
      `Blocked by still reached into Details' view (its foot at ${Math.round(blockedAt.y + blockedAt.height)} px, the view from ${Math.round(view.top)}), so its growth is not above the view`
    ).toBe(true);
    const onEntry = await page.evaluate(
      ({ x, y }) => document.elementFromPoint(x, y)?.closest("[data-history-entry]") !== null,
      pointer
    );
    expect(onEntry, "the pointer no longer rests on an entry of the history").toBe(true);
    const before = await boxOf(collaboratorsControl(page), "the Collaborators control");
    expect(
      wholly(before, view),
      `the Collaborators control stood out of Details' view (${Math.round(before.y)} px, the view ${Math.round(view.top)} to ${Math.round(view.bottom)})`
    ).toBe(true);

    const grew = await grows(page, blockedBy, "Blocked by", titles.resolve, "Blocker 4:");
    // One read once the titles have landed and the half second in `grows` has passed.
    const after = await boxOf(collaboratorsControl(page), "the Collaborators control");
    expect(
      Math.abs(after.y - before.y) <= 1,
      `after the hold ended, Blocked by grew ${Math.round(grew)} px above Details' view and moved the Collaborators control in it from ${Math.round(before.y)} px to ${Math.round(after.y)} px`
    ).toBe(true);
  } finally {
    log.resolve();
    waiting.resolve();
    titles.resolve();
    await context.close();
  }
});

/** Waits until Details' box has stood still for two frames, `after` naming what moved it last. */
async function detailsStandStill(page: Page, after: string): Promise<void> {
  await expect
    .poll(
      async () => {
        const at = await historyScrollTop(page);
        await nextFrames(page);
        return (await historyScrollTop(page)) - at;
      },
      { message: `Details' box kept scrolling after ${after}` }
    )
    .toBe(0);
}

/** The reader reading down the history of `key`, whose log holds `events` events, to its end:
 *  Show more focused and pressed by Enter until no control is left under the last entry
 *  (`readHistoryDown`), and Details' box standing still after. Each focus brings Show more into
 *  Details' view, at the foot of what the history draws. Answers how far Details' box is then
 *  scrolled. */
async function readDownByShowMore(page: Page, key: string, events: number): Promise<number> {
  await expect(
    historyShowMore(page),
    "the history offers no Show more to read down by"
  ).toBeVisible();
  await readHistoryDown(
    page,
    key,
    events,
    async (control) => {
      await control.focus();
      await nextFrames(page);
      await page.keyboard.press("Enter");
    },
    "gone"
  );
  await detailsStandStill(page, `the last ${CONTROL_LABELS.show_more}`);
  return historyScrollTop(page);
}

/** How many Legion agents worked the issue of the row below: enough that the collaborators' row,
 *  its list open, is taller than Details' view at 1440 px and at 390. */
const MANY_AGENTS = 16;

/** How many notes the history of each of the two rows below holds: more than it draws at once,
 *  so reading down takes three presses of Show more. */
const LONG_HISTORY_NOTES = 61;

/** An issue whose history holds `LONG_HISTORY_NOTES` notes of the daemon's, which the history
 *  reads from the issue's messages, and which `MANY_AGENTS` Legion agents worked, so the
 *  collaborators' row with its list open is taller than Details' view: its key, and how many
 *  events its log holds. */
async function seedCollaboratorsOverLongHistory(): Promise<{ events: number; key: string }> {
  const { issue } = await seedMessages(
    "Collaborators over a long history",
    LONG_HISTORY_NOTES,
    "Older message"
  );
  const roles: readonly LegionRole[] = ["implementer", "tester", "reviewer", "planner"];
  await setLegionAgents(
    Array.from({ length: MANY_AGENTS }, (_, index) =>
      legionAgentEntry({
        issue: issue.key,
        legionProject: LEGION_PROJECT,
        role: roles[index % roles.length] ?? "implementer",
        sessionId: `e2e-agent-${index}`,
        startedAt: `2026-10-26T${String(index).padStart(2, "0")}:00:00Z`,
      })
    )
  );
  return { events: (await allEvents(issue.key)).length, key: issue.key };
}

/** Below 768 px Details is a sheet, a dialog that masks the page's keys while it is open
 *  (`Margin.tsx`), so a reader there closes it with Escape before a key of the page; from 768 px
 *  up it is no dialog, and the key is pressed with it open. */
async function readyForPageKey(page: Page, size: { readonly width: number }): Promise<void> {
  if (size.width >= 768) return;
  await page.keyboard.press("Escape");
  await expect(marginShell(page), "Escape did not close Details").toBeHidden();
}

/**
 * The reader at `size`'s width reading the history of `key`, whose log holds `events` events,
 * down to its end (`readDownByShowMore`), then pressing `opener`, Details closed first where it is
 * a sheet (`readyForPageKey`). `landing`, named `what`, is what the key brings into Details' view:
 * the reading must leave it wholly above the view, or the row proves nothing. `t o` puts the
 * focus on the Collaborators control and `t v` on the history. Answers how far the reading
 * scrolled Details' box, and Details' view and `landing`'s box once the box has stood still after
 * the key.
 */
async function keyAfterReadingDown(
  page: Page,
  key: string,
  events: number,
  size: { readonly width: number },
  opener: "t o" | "t v",
  landing: Locator,
  what: string
) {
  const scrolled = await readDownByShowMore(page, key, events);
  // One read of the place the reading left: Details' box has stood still for two frames.
  const view = await historyViewport(page);
  const away = await boxOf(landing, what);
  expect(
    away.y + away.height <= view.top,
    `at ${size.width} px reading down left ${what} at ${Math.round(away.y)} px, in Details' view from ${Math.round(view.top)}, so the row proves nothing`
  ).toBe(true);

  await readyForPageKey(page, size);

  await page.keyboard.press("t");
  await page.keyboard.press(opener === "t o" ? "o" : "v");
  const [focused, focusedWhat] =
    opener === "t o"
      ? [collaboratorsControl(page), "the Collaborators control"]
      : [historyPanel(page), "the history"];
  await expect(focused, `at ${size.width} px ${opener} did not focus ${focusedWhat}`).toBeFocused();
  await detailsStandStill(page, opener);
  // One read of where the key left `landing`: Details' box has stood still for two frames.
  const shown = await historyViewport(page);
  const landed = await boxOf(landing, what);
  return { landed, scrolled, shown };
}

// A load of `/collaborators` opens Details with the collaborators' list open, the Collaborators
// control focused; the reader then reads the history down to its end, pressing Show more by
// Enter until it goes, and presses `t o`. Details brings the collaborators' row into its view by
// the least scroll (`useOpenDetailsAt`), and the row, its list open, is taller than the view:
// coming from below, that scroll shows the row's foot, and the control at its head stayed above
// the view, where the hold of the landed place found it wholly outside the view and kept nothing.
// The hold now brings the focused control into the view as the focus lands. At 390 px Details is
// a sheet whose dialog masks the page's keys, so there the reader closes it with Escape before the
// key, and `t o` opens it again (`readyForPageKey`). Found by the tester on a running build with a
// copy of a large real issue, Details' box scrolled to 43,854 px: the control 151 px above
// Details' view at 1440 px and 435 px above it at 390, in two runs of two.
test("t o after the history is read down by Show more leaves the focused Collaborators control wholly in Details' view, though its open list makes its row taller than the view", async ({
  browser,
}) => {
  test.setTimeout(240_000);
  const { events, key } = await seedCollaboratorsOverLongHistory();
  // Each width's place of the control, so one run says every width it was out of view at.
  const outOfView: string[] = [];
  for (const size of [DESKTOP, PHONE]) {
    const context = await reader(browser, "alice");
    try {
      const page = await context.newPage();
      await page.setViewportSize(size);
      await gotoIssue(page, `/issues/${key}/collaborators`);
      await expect(
        collaboratorsControl(page),
        "a load of /collaborators did not focus the Collaborators control"
      ).toBeFocused();
      const row = collaboratorsRow(page);
      await expect
        .poll(() => row.locator("[data-collaborator] [data-agent-name]").count(), {
          message: "the collaborators' list never drew the issue's Legion agents",
        })
        .toBeGreaterThanOrEqual(MANY_AGENTS);
      await nextFrames(page);
      // One read of the list's height: the agents are drawn, and nothing it holds reads more.
      const view = await historyViewport(page);
      const rowAt = await boxOf(row, "the collaborators' row");
      expect(
        rowAt.height > view.bottom - view.top,
        `at ${size.width} px the collaborators' row is ${Math.round(rowAt.height)} px tall, no taller than Details' view of ${Math.round(view.bottom - view.top)} px, so the row proves nothing`
      ).toBe(true);

      const { landed, scrolled, shown } = await keyAfterReadingDown(
        page,
        key,
        events,
        size,
        "t o",
        collaboratorsControl(page),
        "the Collaborators control"
      );
      if (!wholly(landed, { bottom: shown.bottom + 1, top: shown.top - 1 })) {
        outOfView.push(
          `at ${size.width} px, with Details read down to ${Math.round(scrolled)} px, t o left the focused Collaborators control at ${Math.round(landed.y)} to ${Math.round(landed.y + landed.height)} px, out of Details' view (${Math.round(shown.top)} to ${Math.round(shown.bottom)})`
        );
      }
    } finally {
      await context.close();
    }
  }
  expect(outOfView, outOfView.join("; ")).toEqual([]);
});

// `/issues/<key>/activity` opens Details at the history. The page
// arrives with the log unread, so the history's heading has its box while the list under it is
// still the loading line, and Details cannot yet scroll it to the top: the landing must still
// leave the heading at the top of Details' view once the list is drawn. The log's answer is
// held for 1.5 s, which a cold load over a slow link gives, and the window is short, so what
// Details draws above the history is taller than its view. Found by the tester on a running
// build of 3bd86200 with a copy of a large real issue: a load of /activity, and `t v` from a
// load with Details shut, left the heading at 828 of 900 px, under every property, with one
// entry's top in view and Details scrolled to the end of what it then held.
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
    await newestHistoryEntry(page);
    await expect(historyEntries(page).first(), "the history drew no entry").toBeAttached();
    await nextFrames(page);
    const heading = historyHeading(page);
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

// `t v` from a load with Details shut opens Details at the history and puts the focus there. The
// log is unread when the key is pressed, so the history first stands at the end of what Details
// holds, its list still the loading line, and the landing brings the heading to the top of
// Details' view once the list is drawn, as a load of `/activity` does. The hold of the place a
// key lands the focus at (`details-more.tsx`) took the history's top as the focus found it, at
// the end of Details, and put it back there when the list drew, undoing the landing. Found by
// the tester on a running build of 52f5803d with a copy of a large real issue: `t v` left the
// heading at 860 of 900 px at 1440 px, 984 of 1,024 at 768 and 804 of 844 at 390, in every run;
// a frame trace shows the heading reach 53 px as the list drew and go back to 860 px a frame
// later. Round 3 found the same place for `t v` and a load of `/activity`; this row holds the
// key, as the row above holds the load.
test("t v from a load with Details shut opens Details with the history's heading at the top of its view", async ({
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
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expect(tree(page)).toBeVisible();
    await expect(marginShell(page)).toBeHidden();
    await expectOpeningLanded(page);
    await focusThePage(page);
    await page.keyboard.press("t");
    await page.keyboard.press("v");
    await expect(page, "t v did not open the history").toHaveURL(
      new RegExp(`/issues/${seeded.key}/activity$`)
    );
    await expect(marginShell(page), "t v did not open Details").toBeVisible();
    await newestHistoryEntry(page);
    await expect(historyEntries(page).first(), "the history drew no entry").toBeAttached();
    await nextFrames(page);
    const heading = historyHeading(page);
    await expect
      .poll(
        async () => {
          const view = await historyViewport(page);
          const box = await heading.boundingBox();
          return box === null ? Number.POSITIVE_INFINITY : Math.round(box.y - view.top);
        },
        { message: "t v left the history's heading below the top of Details' view" }
      )
      .toBeLessThanOrEqual(24);
    // The heading held there: a hold of the place the key landed the focus at moves it back a
    // frame after the list draws, so the place is read again once that frame has passed.
    await nextFrames(page);
    const view = await historyViewport(page);
    const after = Math.round(
      ((await heading.boundingBox())?.y ?? Number.POSITIVE_INFINITY) - view.top
    );
    expect(
      after,
      "t v's heading went back down Details' view after the list drew"
    ).toBeLessThanOrEqual(24);
  });
});

// A load of `/activity` opens Details with the history's heading at the top of its view; the
// reader then reads the history down to its end, pressing Show more by Enter until it goes, and
// presses `t v`. Each press is a following of `/activity` of its own (`useFollowing`), which
// brings the heading back to the top of Details' view. The history's landing told one following
// from the next by the address alone, the same for every following of `/activity`, so `t v` put
// the focus on the history and left its heading far above the view. At 390 px Details is a sheet
// whose dialog masks the page's keys, so there the reader closes it with Escape before the key
// (`readyForPageKey`). Found by the tester on a running build with a copy of a large real issue:
// the heading 39,585 px above Details' view at 1440 px and 121 px above it at 390.
test("t v after the history is read down by Show more brings the history's heading to the top of Details' view", async ({
  browser,
}) => {
  test.setTimeout(240_000);
  const { issue } = await seedMessages(
    "A long history read down",
    LONG_HISTORY_NOTES,
    "Older message"
  );
  const events = (await allEvents(issue.key)).length;
  // Each width's place of the heading, so one run says every width it was out of place at.
  const outOfPlace: string[] = [];
  for (const size of [DESKTOP, PHONE]) {
    const context = await reader(browser, "alice");
    try {
      const page = await context.newPage();
      await page.setViewportSize(size);
      await gotoIssue(page, `/issues/${issue.key}/activity`);
      await expect(marginShell(page), "/activity did not open Details").toBeVisible();
      await newestHistoryEntry(page);
      const { landed, scrolled, shown } = await keyAfterReadingDown(
        page,
        issue.key,
        events,
        size,
        "t v",
        historyHeading(page),
        "the history's heading"
      );
      if (Math.abs(landed.y - shown.top) > 2) {
        outOfPlace.push(
          `at ${size.width} px, with Details read down to ${Math.round(scrolled)} px, t v left the history's heading at ${Math.round(landed.y)} px, not at the top of Details' view at ${Math.round(shown.top)}`
        );
      }
    } finally {
      await context.close();
    }
  }
  expect(outOfPlace, outOfPlace.join("; ")).toEqual([]);
});

// `t v` brings the history's heading to the top of Details' view as the key's focus lands: the
// history's own landing puts it there as soon as its log is drawn, which, with the log already
// read, is the first frame after the key, and the focus follows two frames later. A row above the
// history that changes height between the two moved the heading from the top, and the hold of the
// landed place kept it there: 18 px above Details' view on a copy of a real issue at 390 px, by
// pointer and by keyboard. Here the row above the history shrinks by 18 px in the very turn the
// heading says it has landed (`history-heading-landed`, which the history's heading raises), and
// the heading must stand at the top of Details' view once the key has focused the history. The
// browser's own scroll anchoring is turned off, so the row reads the page's own hold alone.
// SLOW(>=2s): 30 messages written, a sign-in, the history read, Details closed and opened again.
test("t v at 390 px with the history's log read leaves its heading at the top of Details' view when a row above it shrinks as the key lands", async ({
  browser,
}) => {
  const { issue, newest } = await seedMessages("A heading after a key", 30);
  const context = await reader(browser, "alice");
  try {
    const page = await context.newPage();
    await page.setViewportSize(PHONE);
    await openHistory(page, issue.key, newest);
    await withoutScrollAnchoring(page);
    await readyForPageKey(page, PHONE);
    await page.evaluate(() => {
      document.addEventListener(
        "history-heading-landed",
        () => {
          const row = document.querySelector(
            '[data-testid="issue-history"]'
          )?.previousElementSibling;
          if (!(row instanceof HTMLElement)) return;
          row.style.marginBottom = "-18px";
          document.documentElement.dataset.rowAboveShrank = "";
        },
        { once: true }
      );
    });

    await page.keyboard.press("t");
    await page.keyboard.press("v");
    await expect(historyPanel(page), "t v did not focus the history").toBeFocused();
    await expect(
      page.locator("html"),
      "the row above the history did not shrink, so the row proves nothing"
    ).toHaveAttribute("data-row-above-shrank", "");
    await detailsStandStill(page, "t v");
    const view = await historyViewport(page);
    const heading = await boxOf(historyHeading(page), "the history's heading");
    expect(
      Math.abs(heading.y - view.top) <= 1,
      `t v left the History heading ${Math.round(heading.y - view.top)} px from the top of Details' view`
    ).toBe(true);
  } finally {
    await context.close();
  }
});

// The address of an agents' turn opens Details with that turn marked
// and on screen. Every turn is drawn whole, so a long one is taller than
// Details' view: its head - the author and the time that is its address - must be what the
// reader lands on, never cut above the view. Found by the tester on a running build of
// 3bd86200 with a copy of a large real issue: a turn of 338 words, 1,083 px tall, landed
// centred, its top at -65 px, its author and time out of view.
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
      const message = await createMessage(issue.key, { body }, asDaemon);
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

// The address of a short reply under a long agents' turn opens Details with the reply marked in
// the middle of its view, the turn above it drawn whole. A change of that turn's height after the
// landing's scroll - its words finishing their drawing, as a cut to six lines once did - moves
// the reply and leaves the top of the thread's entry where it was; the history's anchor kept
// that entry's top, so the reply went up by the change, out of Details' view. Found by the
// tester on a running build with a copy of a large real issue, when the turn was still cut: at
// 1440 px 3 of 12 reply addresses showed none of the reply, up to 2,820 px above the view, and 8
// left its head above it; at 390 px too. The anchor now keeps the landed turn's own place inside
// its entry (`ViewportAnchor.tsx`'s `keepRow`).
test("the address of a short reply under a long agents' turn keeps the reply in Details' view, the turn above it drawn whole", async ({
  browser,
}) => {
  test.setTimeout(180_000);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "A reply under a long turn" });
  await createComment(issue.key, { body: "A person's comment, a row of the outline." });
  // Oldest first: sixty short notes of the daemon's, then the long turn and its reply. The
  // history lists them newest first, so the notes stand under the reply, enough of the history
  // for Details to bring the reply to the middle of its view.
  for (let index = 0; index < 60; index += 1) {
    await createMessage(issue.key, { body: `Older note ${index}` }, asDaemon);
  }
  const root = await createComment(issue.key, { body: longTurnBody(15) }, attentionSession);
  const reply = await createComment(
    issue.key,
    { body: "A short reply under the long turn.", reply_to: root.id },
    attentionSession
  );
  // Each width's place of the reply, so one run says every width it was out of view at.
  const outOfView: string[] = [];
  for (const size of [DESKTOP, PHONE]) {
    const context = await reader(browser, "alice");
    try {
      const page = await context.newPage();
      await page.setViewportSize(size);
      await gotoIssue(page, `/issues/${issue.key}/comments/${reply.id}`);
      await expect(marginShell(page), "the reply's address did not open Details").toBeVisible();
      const marked = historyItem(page, reply.id);
      await expect(marked, "the reply's address did not mark it").toHaveAttribute(
        "aria-current",
        "true"
      );
      // The turn above the reply is drawn whole, taller than Details' view, with no control
      // that cuts it.
      const long = historyItem(page, root.id);
      await expect(long).toContainText("Part 14:");
      await expect(long.locator("[data-node-more]"), "the history cut a turn").toHaveCount(0);
      const view = await historyViewport(page);
      expect(
        ((await long.boundingBox())?.height ?? 0) > view.bottom - view.top,
        `at ${size.width} px the long turn above the reply is no taller than Details' view, so the row proves nothing`
      ).toBe(true);
      // The reply is read once it has stood still for two frames, where the keeper left it.
      await expect
        .poll(
          async () => {
            const top = (await marked.boundingBox())?.y;
            await nextFrames(page);
            return (await marked.boundingBox())?.y === top;
          },
          { message: `at ${size.width} px the reply kept moving after its address landed it` }
        )
        .toBe(true);
      // Read once the reply has stood still, so this one box is the place the keeper left it.
      const at = await marked.boundingBox();
      if (at === null) {
        outOfView.push(`at ${size.width} px the reply drew no box`);
      } else if (!wholly(at, { bottom: view.bottom + 1, top: view.top - 1 })) {
        outOfView.push(
          `at ${size.width} px the reply stood from ${Math.round(at.y)} to ${Math.round(at.y + at.height)} px, out of Details' view (${Math.round(view.top)} to ${Math.round(view.bottom)})`
        );
      }
    } finally {
      await context.close();
    }
  }
  expect(outOfView, outOfView.join("; ")).toEqual([]);
});

// Each turn of the history is drawn whole, and a node's words keep a wide block - a table of
// many columns, a long line of code - scrolling sideways inside itself (`styles.css`, the rule on
// `[data-node-words] .dispatch-markdown table` and `pre`), so Details' own box never scrolls
// sideways. Found by the tester on a running build with a copy of a large real issue: an agent's
// turn holding a table 565 px wide made Details' box 605 px wide in a box of 368 px at 1440 px
// and of 389 px at 390 px, so the whole of Details, its headings included, moved sideways.
test("a wide table or a long line of code in a history turn scrolls inside itself, and Details never scrolls sideways", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "A turn with a wide table" });
  await createComment(issue.key, { body: "A person's comment, a row of the outline." });
  const columns = Array.from({ length: 8 }, (_, index) => `Column${index}heading`);
  const body = [
    "The agents' findings, one row each:",
    "",
    `| ${columns.join(" | ")} |`,
    `| ${columns.map(() => "---").join(" | ")} |`,
    `| ${columns.map((_, index) => `finding${index}withoutanybreak`).join(" | ")} |`,
    "",
    "```",
    `const reading = ${"x".repeat(120)};`,
    "```",
  ].join("\n");
  const turn = await createComment(issue.key, { body }, attentionSession);
  // Each width's widths, so one run says every width Details scrolled sideways at.
  const readings: { client: number; scroll: number; width: number }[] = [];
  for (const size of [DESKTOP, PHONE]) {
    const context = await reader(browser, "alice");
    try {
      const page = await context.newPage();
      await page.setViewportSize(size);
      await gotoIssue(page, `/issues/${issue.key}/activity`);
      await expect(marginShell(page), "the history's address did not open Details").toBeVisible();
      const drawn = historyItem(page, turn.id);
      await expect(drawn.locator("table"), "the turn's table is not drawn").toHaveCount(1);
      await expect(drawn.locator("pre"), "the turn's code is not drawn").toHaveCount(1);
      // Read once the table and the code are drawn and two frames have laid them out, so one
      // instant's widths are the widths the reader sees.
      await nextFrames(page);
      const widths = await drawn.evaluate((item) => {
        let box = item.parentElement;
        while (box !== null && !/(auto|scroll)/.test(getComputedStyle(box).overflowY)) {
          box = box.parentElement;
        }
        const table = item.querySelector("table");
        return {
          client: box?.clientWidth ?? 0,
          scroll: box?.scrollWidth ?? 0,
          table: table?.scrollWidth ?? 0,
        };
      });
      expect(
        widths.client,
        `at ${size.width} px the history has no box it scrolls in`
      ).toBeGreaterThan(0);
      expect(
        widths.table,
        `at ${size.width} px the table is no wider than Details, so the row proves nothing`
      ).toBeGreaterThan(widths.client);
      readings.push({ client: widths.client, scroll: widths.scroll, width: size.width });
    } finally {
      await context.close();
    }
  }
  const sideways = readings
    .filter((reading) => reading.scroll > reading.client + 1)
    .map(
      (reading) =>
        `at ${reading.width} px Details scrolls sideways: its content is ${reading.scroll} px wide in a box of ${reading.client} px`
    );
  expect(sideways, sideways.join("; ")).toEqual([]);
});

// At 767 px and below Details covers the page, so a link of the
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
        asDaemon
      );
      await uploadIssueFile(issue.key, {
        bytes: new Uint8Array(Buffer.from(ONE_PIXEL_PNG, "base64")),
        contentType: "image/png",
        name: "phone-shot.png",
      });
      return { key: issue.key, message: message.id, messageAt: message.created_at };
    },
    async (page, seeded) => {
      await page.setViewportSize(PHONE);
      await gotoIssue(page, `/issues/${seeded.key}/activity`);
      await expect(marginShell(page)).toBeVisible();
      await seededHistoryEntry(page, historyItem(page, seeded.message), seeded.messageAt);

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
      await seededHistoryEntry(page, historyItem(page, seeded.message), seeded.messageAt);
      await historyPanel(page).locator(`[data-history-reply="${seeded.message}"]`).click();
      await expect(marginShell(page), "Reply left Details over the page").toBeHidden();
    }
  );
});

/** The lines of the long comment the references row's seed writes, its first citing the
 *  statement at `reference`: eight paragraphs, far more than five lines in Details. */
function longCitingLines(reference: string): string[] {
  return Array.from({ length: 8 }, (_, index) =>
    index === 0
      ? `Bob's long note cites ${reference} first.`
      : `Line ${index + 1} of Bob's long note on the statement, which runs on past five lines.`
  );
}

/** What the page draws of one element of words: its text with the whitespace collapsed, its line
 *  clamp, how many pixels of it a box hides, and how many lines it takes. */
interface DrawnWords {
  readonly clamp: string;
  readonly hidden: number;
  readonly lines: number;
  readonly text: string;
}

/** Measures every element `words` finds, in one read of the page. */
function measureWords(words: Locator): Promise<DrawnWords[]> {
  return words.evaluateAll((elements) =>
    elements.map((element) => {
      const style = getComputedStyle(element);
      return {
        clamp: style.webkitLineClamp,
        hidden: element.scrollHeight - element.clientHeight,
        lines: element.getBoundingClientRect().height / Number.parseFloat(style.lineHeight),
        text: (element.textContent ?? "").replace(/\s+/gu, " ").trim(),
      };
    })
  );
}

/** Fails unless `drawn`, the words of `what`, are whole on the screen: more than five lines
 *  drawn, no clamp, and nothing hidden under a box. */
function expectWordsWhole(drawn: DrawnWords | undefined, what: string): void {
  if (drawn === undefined) throw new Error(`${what} draws no words`);
  expect(drawn.lines, `${what} is drawn in five lines or fewer`).toBeGreaterThan(5);
  expect(drawn.clamp, `${what} is clamped`).toBe("none");
  expect(drawn.hidden, `${what} hides some of its words`).toBeLessThanOrEqual(1);
}

// "What references this row" is read only when pressed, with
// Details open, and lists each comment that cites the focused statement by the page's word for a
// comment and the comment's words whole - a comment longer than five lines whole too, with no
// clamp and no control that cuts it - a link that focuses the comment's row; no row is named by
// an address, a slug or "spec block"; the issue draws no control; the focus moving closes the
// list; and a failed read says so, with Retry, after at most three requests.
// SLOW(>=2s): a statement cited by two comments, one load of the issue and the list opened, read
// and failed once more with the read answered 500.
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
      const reference = `dispatch://${issue.key}/spec#b-r-cited`;
      const comment = await createComment(
        issue.key,
        { body: `Bob checks ${reference} against the page.` },
        { login: "bob" }
      );
      await createComment(
        issue.key,
        { body: longCitingLines(reference).join("\n\n") },
        { login: "bob" }
      );
      const spec = ((await getIssue(issue.key)).artifacts ?? []).find(
        (artifact) => artifact.primary
      );
      if (spec === undefined) throw new Error(`${issue.key} holds no spec`);
      return {
        comment: comment.id,
        key: issue.key,
        reference,
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
      await expectOpeningLanded(page);
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
      await expect(rows).toHaveCount(2);
      expect(references, "the references were not read once").toHaveLength(1);
      // Each comment's words, from its tree, with the reference it writes named as the outline
      // names it: by the statement's own words.
      const short = rows.filter({ hasText: "Bob checks" });
      await expect(short).toContainText(NODE_REFERENCES_WORDS.kinds.comment);
      await expect(short).toContainText(
        "Bob checks The history is drawn in Details. against the page."
      );
      const long = rows.filter({ hasText: "Bob's long note" });
      await expect(long).toContainText(NODE_REFERENCES_WORDS.kinds.comment);
      await expect(long, "the long comment's row does not draw its words whole").toContainText(
        longCitingLines("The history is drawn in Details.").join(" ")
      );
      // Whole on the screen too: more than five lines drawn, nothing hidden under a clamp, and no
      // control that cuts it.
      const [drawn] = await measureWords(long.getByRole("link"));
      expectWordsWhole(drawn, "the long comment's row");
      await expect(rows.getByRole("button"), "a row draws a control that cuts it").toHaveCount(0);
      // The list's rows are drawn and settled: their count and the long comment's words were read
      // above, so one read of their text is the state the row means to check.
      for (const text of await rows.allTextContents()) {
        for (const name of ["dispatch://", seeded.specSlug, "spec block", "#b-"]) {
          expect(
            text,
            `a row of what references the statement names it by "${name}"`
          ).not.toContain(name);
        }
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
      await short.getByRole("link").click();
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

/** The first lines of the two tasks of the plan the row below seeds, each serving the statement:
 *  each task holds a second sentence after it, which its row does not draw. */
const SERVING_TASK_LINES = [
  "Draw the history in Details.",
  "Read the history from the event log.",
] as const;

// Decision 560: each row of what references the focused row names the node that references it by
// the first line of its own words, as the outline names a row, beside its tie's word and its day.
// A statement that two tasks of one plan serve drew two rows that each read "served by" and a day
// alone, which a reader cannot tell apart; each now reads its own task's first line, the two
// differ, and no row draws an address, a slug, an id or the task's words past its first line.
// SLOW(>=2s): a statement and a plan of two tasks written, the plan's index waited for, and one
// load of the issue with the list opened.
test("what references the focused row names each task that serves it by the task's first line", async ({
  browser,
}) => {
  await phase(
    browser,
    async () => {
      await createProject({ key: "CORE", name: "Core" });
      const issue = await createIssue({
        project: "CORE",
        spec: ":::requirement{#r-served}\nThe history is drawn in Details.\n:::\n",
        title: "Two tasks serve a statement",
      });
      const statement = `dispatch://${issue.key}/spec#b-r-served`;
      const plan = await createIssueArtifact(issue.key, {
        content:
          `How ${issue.key} is built.\n\n` +
          `:::plan_task{#t-draw serves="${statement}"}\n` +
          `${SERVING_TASK_LINES[0]} Each entry keeps its day.\n:::\n\n` +
          `:::plan_task{#t-read serves="${statement}"}\n` +
          `${SERVING_TASK_LINES[1]} It pages two hundred at a time.\n:::\n`,
        name: "plan.md",
      });
      // The serving edges are read from the block index of the version just written, which the
      // coverage read counts too: the list is read once, when the reader opens it, so the seed
      // waits for both tasks to stand there first.
      await expect
        .poll(
          async () =>
            (await getIssueCoverage(issue.key)).requirements?.find(
              (row) => row.block_id === "r-served"
            )?.plan_tasks,
          `the plan's two tasks never came to serve ${statement}`
        )
        .toBe(2);
      const spec = ((await getIssue(issue.key)).artifacts ?? []).find(
        (artifact) => artifact.primary
      );
      if (spec === undefined) throw new Error(`${issue.key} holds no spec`);
      return {
        key: issue.key,
        planSlug: plan.artifact.slug,
        specSlug: spec.slug,
        statement: blockNode(spec.slug, "r-served"),
      };
    },
    async (page, seeded) => {
      await gotoIssue(page, `/issues/${seeded.key}`);
      await expect(tree(page)).toBeVisible();
      const facts = await focusedRowFacts(page, seeded.statement);
      const references = facts.locator("[data-node-references]");
      await references
        .getByRole("button", { exact: true, name: NODE_REFERENCES_WORDS.what_references })
        .click();
      const rows = references.locator("li");
      await expect(rows, "the list does not draw one row per serving task").toHaveCount(2);
      // Each row's link names its task by the task's first line alone, beside "served by".
      for (const line of SERVING_TASK_LINES) {
        await expect(
          rows.getByRole("link", { exact: true, name: line }),
          `no row names its task by the task's first line "${line}"`
        ).toHaveCount(1);
      }
      // The rows are drawn and settled: each link was found by its line above, so one read of
      // their text is the state the row means to check.
      for (const text of await rows.allTextContents()) {
        expect(text, "a task's row does not say what the task does for the statement").toContain(
          NODE_REFERENCES_WORDS.edges.serves
        );
        for (const name of [
          "dispatch://",
          seeded.planSlug,
          seeded.specSlug,
          "#b-",
          "t-draw",
          "t-read",
          ":::",
          "Each entry keeps its day.",
          "It pages two hundred at a time.",
        ]) {
          expect(
            text,
            `a row of what references the statement names its task by "${name}"`
          ).not.toContain(name);
        }
      }
    }
  );
});

/** The first lines of the two tasks of the row below, each a whole sentence of at most 100
 *  characters, so the outline's first-line rule draws each whole (`firstLineOf`). */
const LONG_OPENER_TASK_LINES = [
  "Draw the history in Details.",
  "Read the history from the event log one page of two hundred events at a time, newest first.",
] as const;

// Decision 560, where a task's block opens with long attributes. The read's excerpt of a block is
// its first 480 runes of markdown, the block's opening line included (`refs.BlockExcerpt`). A
// task that serves many statements opens with a line longer than that, so its excerpt holds the
// opening line alone; one that serves fewer holds the opening line and the start of its first
// sentence. Neither may name its row by that excerpt: each row names its task by the task's own
// first line, as the outline names a row, and draws no markup of its block.
// Found by the tester on a running build of 621ddcba with a copy of INFRA-3570: its statement
// spec#b-w-tasks drew `:::plan_task{#p-tasks serves="R1.` and `A and` for two of its rows.
// SLOW(>=2s): a spec of 41 statements and a plan of two tasks written, the plan's index waited for,
// and one load of the issue with the list opened.
test("what references the focused row names a task whose block opens with long attributes by the task's first line", async ({
  browser,
}) => {
  await phase(
    browser,
    async () => {
      await createProject({ key: "CORE", name: "Core" });
      const pads = Array.from({ length: 40 }, (_, index) => `r-p${String(index).padStart(2, "0")}`);
      const issue = await createIssue({
        project: "CORE",
        spec: [
          ":::requirement{#r-served}\nThe history is drawn in Details.\n:::\n",
          ...pads.map((id) => `:::requirement{#${id}}\nStatement ${id}.\n:::\n`),
        ].join("\n"),
        title: "Two tasks with long openings serve a statement",
      });
      const address = (id: string) => `dispatch://${issue.key}/spec#b-${id}`;
      // The opening line of a task that serves the statement and as many more as it takes for
      // that line to reach `at least` runes.
      const opener = (task: string, atLeast: number): string => {
        const serves = [address("r-served")];
        const line = () => `:::plan_task{#${task} serves="${serves.join(" ")}"}`;
        for (const id of pads) {
          if (line().length >= atLeast) break;
          serves.push(address(id));
        }
        return line();
      };
      const long = opener("t-long", 520);
      const cut = opener("t-cut", 420);
      // The seed holds the shapes the row is about: the first opening line passes the excerpt's
      // 480 runes, and the second ends before them with the cut inside its first sentence.
      expect(
        long.length,
        "the first task's opening line is no longer than the excerpt"
      ).toBeGreaterThan(480);
      expect(
        cut.length,
        "the second task's opening line leaves its first sentence uncut"
      ).toBeLessThan(480 - 20);
      expect(
        cut.length + 1 + LONG_OPENER_TASK_LINES[1].length,
        "the excerpt's cut does not fall inside the second task's first sentence"
      ).toBeGreaterThan(480);
      const statement = address("r-served");
      await createIssueArtifact(issue.key, {
        content:
          `How ${issue.key} is built.\n\n` +
          `${long}\n${LONG_OPENER_TASK_LINES[0]} Each entry keeps its day.\n:::\n\n` +
          `${cut}\n${LONG_OPENER_TASK_LINES[1]} The read mark stays.\n:::\n`,
        name: "plan.md",
      });
      await expect
        .poll(
          async () =>
            (await getIssueCoverage(issue.key)).requirements?.find(
              (row) => row.block_id === "r-served"
            )?.plan_tasks,
          `the plan's two tasks never came to serve ${statement}`
        )
        .toBe(2);
      const spec = ((await getIssue(issue.key)).artifacts ?? []).find(
        (artifact) => artifact.primary
      );
      if (spec === undefined) throw new Error(`${issue.key} holds no spec`);
      return { key: issue.key, statement: blockNode(spec.slug, "r-served") };
    },
    async (page, seeded) => {
      await gotoIssue(page, `/issues/${seeded.key}`);
      await expect(tree(page)).toBeVisible();
      const facts = await focusedRowFacts(page, seeded.statement);
      const references = facts.locator("[data-node-references]");
      await references
        .getByRole("button", { exact: true, name: NODE_REFERENCES_WORDS.what_references })
        .click();
      const rows = references.locator("li");
      await expect(rows, "the list does not draw one row per serving task").toHaveCount(2);
      for (const line of LONG_OPENER_TASK_LINES) {
        await expect(
          rows.getByRole("link", { exact: true, name: line }),
          `no row names its task by the task's first line "${line}"`
        ).toHaveCount(1);
      }
      for (const text of await rows.allTextContents()) {
        for (const markup of [":::", "{#", "serves=", "dispatch://"]) {
          expect(text, `a row of what references the statement draws "${markup}"`).not.toContain(
            markup
          );
        }
      }
    }
  );
});

/** The second line of the code of the note the row below seeds, under the line that quotes the
 *  plan's task by its address: the code block's row draws its first line alone. */
const QUOTING_CODE_NEXT_LINE = "read the log one page at a time";

// Decision 565 (2): a code block of another document that quotes a plan task by its address is a
// node that references the task, since the server records a mention from inside code as `main`
// does (`text/refs.go`'s `InCode`). The task's list of what references it names the code block by
// the first line of its code, drawn as code in the look a code span in a node's words takes (the
// task's own row draws its words' code span so): its address as its author typed it, with no name
// of the task in its place and no link of its own, beside the word for a document and the day.
// SLOW(>=2s): a statement, a plan of one task and a note whose code quotes it written, the note's
// mention waited for, and one load of the issue with the list opened.
test("what references the focused row names a code block that quotes it by the first line of its code, as code", async ({
  browser,
}) => {
  await phase(
    browser,
    async () => {
      await createProject({ key: "CORE", name: "Core" });
      const issue = await createIssue({
        project: "CORE",
        spec: ":::requirement{#r-served}\nThe history is drawn in Details.\n:::\n",
        title: "A note's code quotes a task",
      });
      const statement = `dispatch://${issue.key}/spec#b-r-served`;
      const plan = await createIssueArtifact(issue.key, {
        content:
          `How ${issue.key} is built.\n\n` +
          `:::plan_task{#t-draw serves="${statement}"}\n` +
          `${SERVING_TASK_LINES[0]} Run \`make history\` first.\n:::\n`,
        name: "plan.md",
      });
      const task = `dispatch://${issue.key}/artifact/${plan.artifact.slug}#b-t-draw`;
      const notes = await createIssueArtifact(issue.key, {
        content: `How the history is read.\n\n\`\`\`\nopen ${task}\n${QUOTING_CODE_NEXT_LINE}\n\`\`\`\n`,
        name: "notes.md",
      });
      // The mention is placed in the note's code block by the note's index, after the version is
      // written, and the list is read once, when the reader opens it: the seed waits for it there.
      const document = `dispatch://${issue.key}/artifact/${notes.artifact.slug}`;
      await expect
        .poll(
          async () =>
            ((await referencesTo(task)).edges ?? []).some(
              (edge) =>
                edge.kind === "mentions" &&
                edge.node.kind === "block" &&
                edge.node.document === document
            ),
          `the note's code block never came to reference ${task}`
        )
        .toBe(true);
      return {
        key: issue.key,
        line: `open ${task}`,
        task: blockNode(plan.artifact.slug, "t-draw"),
      };
    },
    async (page, seeded) => {
      await gotoIssue(page, `/issues/${seeded.key}`);
      await expect(tree(page)).toBeVisible();
      const facts = await focusedRowFacts(page, seeded.task);
      const references = facts.locator("[data-node-references]");
      await references
        .getByRole("button", { exact: true, name: NODE_REFERENCES_WORDS.what_references })
        .click();
      // The code block's row, once the names read of the list's citing blocks has answered.
      const quoting = references.locator("li").filter({ has: page.locator("code") });
      await expect(quoting, "no row draws the code block's line as code").toHaveCount(1);
      const code = quoting.locator("code");
      await expect(
        code,
        "the code block's row does not draw its code's first line as typed"
      ).toHaveText(seeded.line);
      await expect(
        quoting.getByRole("link", { exact: true, name: seeded.line }),
        "the code block's row is no link named by its code's first line"
      ).toHaveCount(1);
      await expect(code.locator("a"), "an address in the code is drawn as a link").toHaveCount(0);
      // The look of a code span in a node's words: the task's own row draws `make history` so. Its
      // font is a code span's; its colour is its link's ink, as the renderer draws a code span
      // inside a link (`a code` takes the link's colour).
      const spanned = rowWords(nodeRow(page, seeded.task)).locator("code").first();
      await expect(spanned, "the task's row draws no code span of its words").toHaveCount(1);
      const link = quoting.getByRole("link", { exact: true, name: seeded.line });
      const [drawn, span, ink] = await Promise.all(
        [code, spanned, link].map((element) =>
          element.evaluate((node) => {
            const style = getComputedStyle(node);
            return { color: style.color, family: style.fontFamily, weight: style.fontWeight };
          })
        )
      );
      expect(
        { family: drawn.family, weight: drawn.weight },
        "the code block's line is not drawn in the font of a code span"
      ).toEqual({ family: span.family, weight: span.weight });
      expect(drawn.color, "the code block's line is not drawn in its link's ink").toBe(ink.color);
      // The row is drawn and settled: its code was read whole above, so one read of its text is
      // the state the row means to check.
      const text = ((await quoting.textContent()) ?? "").replace(/\s+/gu, " ");
      expect(text, "the code block's row does not say it is a document's").toContain(
        NODE_REFERENCES_WORDS.kinds.block
      );
      expect(text, "the code block's row names the task its code quotes").not.toContain(
        SERVING_TASK_LINES[0]
      );
      expect(text, "the code block's row draws its code's next line").not.toContain(
        QUOTING_CODE_NEXT_LINE
      );
      await expect(quoting.locator("[data-reference-time]"), "the row draws no day").toHaveCount(1);
    }
  );
});

/** The agent session that asks the question of the row below, with the title and the tmux target
 *  its writes stamp, which its copy handles hold. */
const factsAsker = {
  actor: {
    id: "e2e-facts-asker",
    kind: "session" as const,
    origin: { session_title: "Facts asker (e2e)", tmux: "facts:2.1" },
  },
  as: "agent" as const,
};

/** The agent session that follows the question of the row below. */
const factsFollower = { id: "e2e-facts-follower", kind: "session" as const };

// The facts of a question the reader is on hold what the ask card draws around it: its edit
// history, as `AskEditHistory` draws it, the earlier wording behind its own disclosure; who a
// person's answer reaches, as `AskRecipients` draws it, its asker and the agent that follows it;
// and, behind the asker's name, which a press opens, the session's ID, its title and its tmux
// target, each with its copy control. The question's thread is the answer of the page's read of
// the issue's questions with their threads where one held it before Details opened, and Details
// then reads nothing of the question's own; else it is the question's own read, which the page
// sends only once Details is open, and at most once. The facts are found by what Details' section
// on the focused row draws, its heading, words and controls, and never through the row's body,
// which a page that draws a row's facts on its face does not draw.
// SLOW(>=2s): a question written, reworded and followed, and one load of the issue.
test("the facts of a focused ask hold its edit history, its recipients and its copy handles", async ({
  browser,
}) => {
  await phase(
    browser,
    async () => {
      await createProject({ key: "CORE", name: "Core" });
      const issue = await createIssue({ project: "CORE", title: "A question reworded once" });
      const ask = await createAsk(
        issue.key,
        { question: "Which store holds the drafts?" },
        factsAsker
      );
      await editAsk(ask.id, { question: "Which store holds a reply's drafts?" }, factsAsker);
      await followAsk(ask.id, factsFollower.id, factsFollower);
      await setLiveSessions([
        { session_id: factsAsker.actor.id, title: factsAsker.actor.origin.session_title },
        { session_id: factsFollower.id, title: "Facts follower (e2e)" },
      ]);
      return { ask: ask.id, key: issue.key };
    },
    async (page, seeded) => {
      const askReads: string[] = [];
      page.on("request", (request) => {
        const url = new URL(request.url());
        if (request.method() === "GET" && url.pathname === `/api/v1/asks/${seeded.ask}`) {
          askReads.push(url.pathname);
        }
      });
      // Each answer of the page's reads of the issue's questions with their threads, as whether
      // it holds this question's thread, until Details opens.
      const threadReads: Promise<boolean>[] = [];
      const threadRead = (response: Response) => {
        const url = new URL(response.url());
        if (
          response.request().method() === "GET" &&
          url.pathname === `/api/v1/issues/${seeded.key}/asks` &&
          url.searchParams.get("thread") === "1"
        ) {
          threadReads.push(
            response
              .json()
              .then((rows: IssueAskThreadRow[]) =>
                rows.some((row) => row.id === seeded.ask && row.thread != null)
              )
          );
        }
      };
      page.on("response", threadRead);

      // With Details shut, the question focused, the page reads nothing of the question's own.
      // The row is focused and read by the focus alone, as a page that draws a row's facts on
      // its face draws the focused row no body.
      const shut = watchApiRequests(page);
      await gotoIssue(page, `/issues/${seeded.key}`);
      await expect(tree(page)).toBeVisible();
      await expectLoadSettled(shut);
      const row = nodeRow(page, seeded.ask).first();
      await expect(row, "the question is not a row of the outline").toBeVisible();
      await row.focus();
      await expect(row, "the question's row did not take the focus").toBeFocused();
      await expect.poll(() => focusedNodeId(page)).toBe(seeded.ask);
      await expect(marginShell(page)).toBeHidden();
      await expectLoadSettled(shut);
      expect(askReads, "the facts read the question with Details shut").toEqual([]);
      // The load has settled, so every read of the questions' threads sent so far has answered.
      page.off("response", threadRead);
      const threadsBeforeOpen = threadReads.length;
      const threadHeld = (await Promise.all(threadReads)).some((holds) => holds);

      // Opened, Details draws the facts of the row the reader is on under their own heading.
      const opened = watchApiRequests(page);
      await openDetails(page);
      const facts = focusedNodeFacts(page);
      await expect(
        facts.getByRole("heading", { exact: true, name: PROPERTY_LABELS.focused_node }),
        "Details draws no facts of the row the reader is on"
      ).toBeVisible();
      await expect(facts.getByText(/^Edited /), "the ask's edit is not in its facts").toBeVisible();
      await facts.getByText("Show 1 previous version", { exact: true }).click();
      await expect(facts.getByText("Which store holds the drafts?", { exact: true })).toBeVisible();
      await expect(facts.getByText(/^Reworded by /)).toBeVisible();

      const recipients = facts.getByRole("region", { name: "Recipients" });
      await expect(recipients, "the question's recipients are not in its facts").toContainText(
        "Reaches 2"
      );
      await recipients.getByRole("button", { name: /^Reaches/ }).click();
      const followers = recipients.getByRole("list", { name: "Followers" });
      await expect(followers).toContainText(factsAsker.actor.origin.session_title);
      await expect(followers).toContainText("Facts follower (e2e)");

      // The asker's name, written by it, opens its copy handles.
      await expect(facts.getByText(`${CONTROL_LABELS.written_by}:`, { exact: true })).toBeVisible();
      const { session_title: askerTitle, tmux: askerTmux } = factsAsker.actor.origin;
      const handles = [
        `Copy session ID ${factsAsker.actor.id}`,
        `Copy session title ${askerTitle}`,
        `Copy tmux target ${askerTmux}`,
      ];
      for (const name of handles) {
        await expect(facts.getByRole("button", { exact: true, name })).toHaveCount(0);
      }
      const askerName = (expanded: boolean) =>
        facts.getByRole("button", { exact: true, expanded, name: askerTitle });
      await askerName(false).click();
      await expect(askerName(true), "the asker's name did not open its handles").toBeVisible();
      for (const name of handles) {
        await expect(
          facts.getByRole("button", { exact: true, name }),
          `the asker's handles hold no "${name}"`
        ).toBeVisible();
      }

      await expectLoadSettled(opened);
      const most = threadHeld ? 0 : 1;
      expect(
        askReads.length,
        `Details read the question ${askReads.length} times, where ${threadsBeforeOpen} reads of the questions' threads before it opened ${threadHeld ? "held" : "did not hold"} its thread`
      ).toBeLessThanOrEqual(most);
    }
  );
});

/** The architecture repository of the row below, whose model holds `MANY_COMPONENTS`. */
const LISTS_ARCHITECTURE = "acme/lists-architecture";

/** How many entries each list of the row below holds: five past the `PAGE_SIZE` Details draws at
 *  first. */
const LONG_LIST = PAGE_SIZE + 5;

/** The ids of the components of the row below's model, one file each: `part-01` and on. */
const MANY_COMPONENTS = Array.from(
  { length: LONG_LIST },
  (_, index) => `part-${String(index + 1).padStart(2, "0")}`
);

/** The lines of the comment `number` of another issue that cites the issue `key`: seven, which
 *  the seed writes as one paragraph with a hard line break, a backslash at the line's end, after
 *  each line but the last, so the renderer draws the comment in seven lines wherever it draws it
 *  whole. A paragraph of a comment is one block, and Referenced by lists the block that cites the
 *  issue by the server's excerpt of that block, which holds the whole paragraph: seven
 *  paragraphs would be seven blocks, and the excerpt the first line alone. The address stands
 *  before the end of its line, as a backslash written straight after it would be read as more of
 *  the address, which then names nothing. */
function citingNoteLines(key: string, number: number): string[] {
  const note = String(number).padStart(2, "0");
  return [
    `Note ${note} of the other issue cites dispatch://${key} in its first line.`,
    ...Array.from({ length: 6 }, (_, line) => `Line ${line + 2} of note ${note}.`),
  ];
}

/**
 * An issue whose lists of unbounded length in Details each hold `LONG_LIST` entries: the issues
 * it blocks, its components, its files and documents (its spec and 24 more), the comments that
 * cite its statement, and the comments of another issue that cite it, which its Referenced by
 * lists after the issues it blocks. Its labels, its priorities and its blockers are left out: the
 * server holds no more than 20 of each.
 */
async function seedLongLists() {
  await Promise.all([
    createProject({ key: "CORE", name: "Core" }),
    seedFakeGithub({
      [LISTS_ARCHITECTURE]: {
        contents: "read",
        files: Object.fromEntries(
          MANY_COMPONENTS.map((id) => [`${id}.md`, `---\ntitle: Component ${id}\n---\nA part.\n`])
        ),
        installation_id: 421,
      },
    }),
  ]);
  await putArchitectureSource("CORE", { branch: "main", repo: LISTS_ARCHITECTURE });
  const source = await syncArchitectureSource("CORE", LISTS_ARCHITECTURE);
  if (source.last_error !== null) {
    throw new Error(`the component model did not import: ${source.last_error}`);
  }
  const issue = await createIssue({
    components: { ids: MANY_COMPONENTS, mode: "explicit" },
    project: "CORE",
    spec: ":::requirement{#r-cited}\nEvery list draws 20 entries at a time.\n:::\n",
    title: "An issue whose lists run past 20",
  });
  // Each list is written at once: its writes are independent, and the row reads no creation order.
  await Promise.all(
    Array.from({ length: LONG_LIST - 1 }, (_, index) =>
      createIssueArtifact(issue.key, {
        content: `Notes ${index + 1}.\n`,
        name: `notes-${String(index + 1).padStart(2, "0")}.md`,
      })
    )
  );
  await Promise.all(
    Array.from({ length: LONG_LIST }, (_, index) =>
      // `force`: the titles are alike on purpose, and the duplicate check would refuse them.
      createIssue({
        blocked_by: [issue.key],
        force: true,
        project: "CORE",
        title: `Waits on the issue (${index + 1})`,
      })
    )
  );
  await Promise.all(
    Array.from({ length: LONG_LIST }, (_, index) =>
      createComment(
        issue.key,
        { body: `Citing comment ${index + 1} reads dispatch://${issue.key}/spec#b-r-cited.` },
        { login: "bob" }
      )
    )
  );
  const other = await createIssue({
    force: true,
    project: "CORE",
    title: "Another issue, whose comments cite the first",
  });
  await Promise.all(
    Array.from({ length: LONG_LIST }, (_, index) =>
      createComment(
        other.key,
        { body: citingNoteLines(issue.key, index + 1).join("\\\n") },
        { login: "bob" }
      )
    )
  );
  const spec = ((await getIssue(issue.key)).artifacts ?? []).find((artifact) => artifact.primary);
  if (spec === undefined) throw new Error(`${issue.key} holds no spec`);
  return { key: issue.key, statement: blockNode(spec.slug, "r-cited") };
}

/**
 * Presses `more`, the Show more under a list of Details of `total` entries, until it goes: each
 * press keeps the entries drawn and draws the next 20, or the rest, and moves the focus to the
 * first entry it drew, which `focused(index)` finds by its place in the list: by default, the
 * focus on or inside entry `index` of `entries`. Before each press the list draws the entries
 * drawn so far, and `more` says how many are left.
 */
async function showEveryEntry(
  entries: Locator,
  more: Locator,
  total: number,
  what: string,
  focused: (index: number) => Promise<boolean> = (index) => focusInside(entries.nth(index))
): Promise<void> {
  for (let drawn = PAGE_SIZE; drawn < total; drawn += PAGE_SIZE) {
    await expect(entries, `${what} drew another count than ${drawn}`).toHaveCount(drawn);
    await expect(more, `${what}'s Show more does not say ${total - drawn} are left`).toHaveText(
      showMoreName(total - drawn)
    );
    await more.click();
    await expect(entries, `${what}'s Show more did not draw the next entries`).toHaveCount(
      Math.min(drawn + PAGE_SIZE, total)
    );
    await expect
      .poll(() => focused(drawn), `${what}'s Show more left the focus off the first entry it drew`)
      .toBe(true);
  }
  await expect(more, `${what}'s Show more stayed with every entry drawn`).toHaveCount(0);
}

// Each list of unbounded length that Details draws shows its first 20 entries and, under them,
// Show more, which says how many are left; a press draws the next 20, the control goes once
// every entry is drawn, and the focus goes to the first entry the press drew: the issues the
// issue blocks, its components, its files and documents, what references its statement, and its
// own Referenced by, which lists the issues it blocks and then the comments of another issue
// that cite it, each comment whole with no clamp and no Show more of its own. The server holds
// no more than 20 labels, priorities or blockers on an issue, so those lists never page.
// SLOW(>=2s): an architecture model, 25 issues, 24 documents and 50 comments written, and one
// load of the issue whose lists are read down.
test("each list of Details shows 20 entries and a control for the next 20", async ({ browser }) => {
  test.setTimeout(180_000);
  await phase(browser, seedLongLists, async (page, seeded) => {
    await gotoIssue(page, `/issues/${seeded.key}`);
    await openDetails(page);

    const blocks = detailsRow(page, PROPERTY_LABELS.blocks);
    const blocking = blocks.getByRole("link");
    await expect(blocking, "Blocks drew 10 entries, not 20").toHaveCount(PAGE_SIZE);
    await showEveryEntry(
      blocking,
      blocks.getByRole("button", { name: CONTROL_LABELS.show_more }),
      LONG_LIST,
      "Blocks"
    );

    // Components past the first 20 stand in the page hidden, so the chips counted are the ones
    // drawn, each named by its title once the model is read.
    const components = detailsRow(page, PROPERTY_LABELS.components);
    await expect(components).toContainText(`Component ${MANY_COMPONENTS[0]}`);
    const chips = components
      .locator('[data-testid="issue-components"] > *')
      .filter({ hasText: /^Component part-\d+$/ })
      .filter({ visible: true });
    await showEveryEntry(
      chips,
      components.getByRole("button", { name: CONTROL_LABELS.show_more }),
      LONG_LIST,
      "Components"
    );

    const documents = filesList(page).getByRole("link");
    await showEveryEntry(
      documents,
      filesList(page).locator("xpath=..").getByRole("button", { name: CONTROL_LABELS.show_more }),
      LONG_LIST,
      "Files and documents"
    );

    // Referenced by, once read, counts every row of its list: the issues the issue blocks, then
    // the other issue's comments.
    const { control: referencedBy, panel } = await openReferencedBy(page);
    const referenceRows = panel.locator("[data-reference-rows]");
    const counted = new RegExp(`^${PROPERTY_LABELS.referenced_by} \\((\\d+)\\)$`, "u");
    await expect(referencedBy).toHaveText(counted);
    // The count appears only once the list has been read, so the first count drawn is the final one.
    const listed = Number(counted.exec((await referencedBy.textContent()) ?? "")?.[1]);
    expect(
      listed,
      "Referenced by does not count the issues the issue blocks and the comments that cite it"
    ).toBeGreaterThanOrEqual(LONG_LIST * 2);
    await showEveryEntry(
      referenceRows,
      // The list's own Show more, by its name, which says how many rows are left: an excerpt
      // clamped with a Show more of its own is no control of the list's.
      panel.getByRole("button", { name: new RegExp(`^${CONTROL_LABELS.show_more} \\(`, "u") }),
      listed,
      "Referenced by",
      // The first row a press drew takes the focus, or, where it is the first row of its card,
      // the card does, whose link names its source.
      (index) =>
        referenceRows.nth(index).evaluate((row) => {
          const card =
            row.parentElement?.firstElementChild === row ? row.parentElement?.parentElement : row;
          return card?.contains(document.activeElement) === true;
        })
    );
    // The list is drawn and settled: every row was counted and pressed down above, so one read of
    // its excerpts is the state the row checks. An excerpt is the element the renderer draws a
    // citing comment's words in, under its row's line of the time, in the comment's card.
    const excerpts = await measureWords(referenceRows.locator(":scope > div > .dispatch-markdown"));
    for (let number = 1; number <= LONG_LIST; number += 1) {
      const lines = citingNoteLines(seeded.key, number);
      const drawn = excerpts.filter((excerpt) => excerpt.text.includes(lines[1] ?? ""));
      expect(drawn, `Referenced by draws no citing comment ${number}`).toHaveLength(1);
      for (const excerpt of drawn) {
        // Every line of the comment, the first on each side of the address it cites, which the
        // renderer draws as its target's name.
        for (const line of [...(lines[0] ?? "").split(/ dispatch:\S+ /u), ...lines.slice(1)]) {
          expect(excerpt.text, `Referenced by cut citing comment ${number}`).toContain(line);
        }
        expectWordsWhole(excerpt, `citing comment ${number}`);
      }
    }
    // No citing comment's card draws a control of the excerpt's own that cuts it: one read of the
    // settled list for every card that holds a citing comment's second line.
    await expect(
      panel
        .locator("[data-reference-card]")
        .filter({ hasText: /Line 2 of note \d{2}\./u })
        .getByRole("button", { name: /^Show (?:more|less)$/u }),
      "Referenced by draws a control that cuts a citing comment"
    ).toHaveCount(0);

    // What references the statement, from its facts.
    const facts = await focusedRowFacts(page, seeded.statement);
    const references = facts.locator("[data-node-references]");
    await references
      .getByRole("button", { exact: true, name: NODE_REFERENCES_WORDS.what_references })
      .click();
    const rows = references.locator("li");
    await showEveryEntry(
      rows,
      references.getByRole("button", { name: CONTROL_LABELS.show_more }),
      LONG_LIST,
      "What references the statement"
    );
  });
});

// The history's anchor keeps the place of the entry the reader's pointer rests on in Details' box
// (`ViewportAnchor.tsx`), as the browser's own scroll anchoring would, with that anchoring off
// there while it does, and only an act of the reader's records another place. A hand that comes
// to rest moves a few pixels on inside the entry it reached, which raises a pointer move and no
// boundary event. Blocked by, wholly above Details' view, then grows as the titles of the issues
// it names land, which takes the entry from under the resting pointer; Chromium raises that
// move's boundary events at the start of the next frame, at the point the pointer rests at, and
// observes the growth only after them. The rows below hold that.

/** An issue whose Blocked by names five issues with titles long enough to wrap
 *  (`seedHeldRows`), and whose history holds thirty of the Legion daemon's notes, each a thread
 *  the history holds, so Details scrolls past every one of its sections to the history's
 *  entries. */
async function seedHeldHistory(): Promise<string> {
  const key = await seedHeldRows({ waiting: false });
  for (let index = 0; index < 30; index += 1) {
    await createMessage(key, { body: `A note of the daemon's ${index}.` }, asDaemon);
  }
  return key;
}

/** Every row of Details that lies in the view of the box it scrolls in, wholly or in part, by a
 *  name of its own, with its top: the history's heading, its entries by id, and each section's
 *  rows by the words they start with. */
function rowsInDetailsView(page: Page): Promise<Record<string, number>> {
  return page.evaluate(() => {
    const history = document.querySelector('[data-testid="issue-history"]');
    let box = history?.parentElement ?? null;
    while (box !== null && !/^(auto|scroll)$/.test(getComputedStyle(box).overflowY)) {
      box = box.parentElement;
    }
    const view = box?.getBoundingClientRect() ?? { bottom: window.innerHeight, top: 0 };
    const named: [string, Element][] = [];
    const heading = history?.querySelector("h2");
    if (heading) named.push(["the history's heading", heading]);
    for (const entry of document.querySelectorAll("[data-history-entry]")) {
      named.push([`entry ${entry.getAttribute("data-history-entry")}`, entry]);
    }
    for (const row of document.querySelectorAll("[data-margin-section] > div > div")) {
      named.push([`the row "${row.textContent?.trim().slice(0, 24)}"`, row]);
    }
    const seen: Record<string, number> = {};
    for (const [name, element] of named) {
      const { bottom, top } = element.getBoundingClientRect();
      if (bottom > view.top && top < view.bottom) seen[name] = Math.round(top);
    }
    return seen;
  });
}

/** Details' rows in view once they have stood still for two frames (`rowsInDetailsView`): what a
 *  growth moved, and what put them back, have both run by then. */
async function settledRows(page: Page): Promise<Record<string, number>> {
  let settled: Record<string, number> = {};
  await expect
    .poll(
      async () => {
        const first = await rowsInDetailsView(page);
        await nextFrames(page);
        settled = await rowsInDetailsView(page);
        return JSON.stringify(settled) === JSON.stringify(first);
      },
      { message: "Details' rows kept moving" }
    )
    .toBe(true);
  return settled;
}

/** Each row of `before` that does not stand within a pixel of its top there in `after`, said with
 *  where it went. */
function movedRows(before: Record<string, number>, after: Record<string, number>): string[] {
  return Object.entries(before).flatMap(([name, top]) => {
    const now = after[name];
    if (now !== undefined && Math.abs(now - top) <= 1) return [];
    return [`${name} from ${top} px to ${now === undefined ? "out of the view" : `${now} px`}`];
  });
}

/** The reader's pointer coming to rest at `point`: it reaches the point, then moves on a few
 *  pixels, as a hand that stops does, which is a move inside what it reached and raises no
 *  boundary event. */
async function restThePointer(page: Page, point: { x: number; y: number }): Promise<void> {
  await page.mouse.move(point.x, point.y);
  await page.mouse.move(point.x + 3, point.y + 1);
  await nextFrames(page);
}

/** A reader's page on `key` at 1440 px, loaded with Details shut, then `opener`: `t o`, as
 *  `afterTo` has it, or `t v`, which opens Details at the history with the focus on it. The
 *  titles of the issues Blocked by names are held until `titles` settles. */
async function afterOpener(
  browser: Browser,
  key: string,
  opener: "t o" | "t v",
  titles: Promise<void>
): Promise<{ context: BrowserContext; page: Page }> {
  if (opener === "t o") return afterTo(browser, key, DESKTOP, { titles });
  const context = await reader(browser, "alice");
  const page = await context.newPage();
  await page.setViewportSize(DESKTOP);
  await page.route("**/api/v1/reference-targets?**", async (route) => {
    await titles;
    await route.continue();
  });
  await gotoIssue(page, `/issues/${key}`);
  await expectIssuePageDrawn(page);
  await expect(marginShell(page)).toBeHidden();
  await focusThePage(page);
  await page.keyboard.press("t");
  await page.keyboard.press("v");
  await expect(page).toHaveURL(new RegExp(`/issues/${key}/activity$`));
  await expect(historyPanel(page), "t v did not focus the history").toBeFocused();
  return { context, page };
}

// The tester's case. After `t o` the reader turns the wheel until the history's newest entry
// stands at 500 px, and rests the pointer 12 px below that entry's top, so a growth of more than
// 12 px above takes the entry from under it. Blocked by, wholly above the view, grows as the
// titles land, by 500 px at 1440 px here. The anchor read the entry's `pointerout` as the hand
// moving, let its place go, and the growth's observation found nothing to put back. Found by the
// tester on a running build at 1440 px with a copy of a large real issue: the entry under the
// pointer went from 500 to 800 px in three runs of three, and every row in view with it.
test("t o, a wheel to the history and the pointer coming to rest on its newest entry: Blocked by growing wholly above Details' view moves no row in the view", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  const key = await seedHeldHistory();
  const titles = Promise.withResolvers<void>();
  const { context, page } = await afterTo(browser, key, DESKTOP, { titles: titles.promise });
  try {
    const entry = await newestHistoryEntry(page);
    await expect(entry, "the history drew no entry").toBeAttached();
    await nextFrames(page);
    // One read of where `t o` left the newest entry: Details has drawn the history and stood
    // still for two frames, and the titles, which alone move it next, are still held.
    const landed = await boxOf(entry, "the history's newest entry");
    await wheelOverDetails(page, Math.round(landed.y - 500));
    // One read of Details' view, which no act of the row's moves from here on.
    const view = await historyViewport(page);
    const blockedBy = detailsRow(page, PROPERTY_LABELS.blocked_by);
    // One read of the place the wheel left: the box has stood still for two frames, and the
    // titles, which alone move it next, are still held.
    const blocked = await boxOf(blockedBy, "Blocked by");
    expect(
      blocked.y + blocked.height <= view.top,
      `the wheel left Blocked by's foot at ${Math.round(blocked.y + blocked.height)} px, in the view from ${Math.round(view.top)}`
    ).toBe(true);
    // The same settled place, read for the entry the pointer comes to rest on.
    const resting = await boxOf(entry, "the history's newest entry");
    expect(
      wholly(resting, view),
      `the wheel left the newest entry at ${Math.round(resting.y)} px, out of Details' view`
    ).toBe(true);
    await restThePointer(page, { x: resting.x + 60, y: resting.y + 12 });
    // One read of every row in view with the pointer at rest: nothing has moved since the wheel.
    const before = await rowsInDetailsView(page);

    const grew = await grows(page, blockedBy, "Blocked by", titles.resolve, "Blocker 4:");
    const moved = movedRows(before, await settledRows(page));
    expect(
      moved,
      `Blocked by grew ${Math.round(grew)} px above Details' view and moved ${moved.join(", ")}`
    ).toEqual([]);
  } finally {
    titles.resolve();
    await context.close();
  }
});

/** One place the sweep below puts the reader at before Blocked by grows. */
interface ReaderPlace {
  /** What the reader did after the key and before the wheel brought the history to its place:
   *  nothing, Shift, or Tab. */
  readonly act: "nothing" | "Shift" | "Tab";
  /** Where the focus is: where the key left it in Details' box, or on the page outside it. */
  readonly focus: "Details" | "page";
  /** Where the wheel brought the history's top, as a share of Details' view down from its top. */
  readonly historyTop: number;
  /** The keys that opened Details. */
  readonly opener: "t o" | "t v";
}

/** The places of the sweep below: each key and each focus at each of the three places of the
 *  history's top, the act going round the three so that each key with each focus, and each
 *  place of the history's top, meets each act. The first is the tester's case: `t o`, the focus
 *  left on the Collaborators control, and a wheel that brings the history's newest entry to some
 *  500 px. */
const READER_PLACES: readonly ReaderPlace[] = (["t o", "t v"] as const).flatMap((opener, o) =>
  [0.45, 0.1, 0.75].flatMap((historyTop, t) =>
    (["Details", "page"] as const).map((focus, f) => ({
      act: (["nothing", "Shift", "Tab"] as const)[(o + t * 2 + f) % 3] ?? "nothing",
      focus,
      historyTop,
      opener,
    }))
  )
);

/** How many of the history's entries, from its newest, the pointer comes to rest on at each
 *  place, at the upper and at the lower part of each: the entries the view holds wholly, up to
 *  this many. */
const RESTED_ENTRIES = 6;

/** The sweep's rests at one place of `READER_PLACES`, on the issue `key` in a page of its own:
 *  what Blocked by's growth above the view moved at each, said with the place, the entry and its
 *  part. Empty where no growth moved a row. */
async function sweepPlace(browser: Browser, key: string, place: ReaderPlace): Promise<string[]> {
  const focused = place.focus === "Details" ? "left in Details" : "moved to the page";
  const named = `${place.opener}, the focus ${focused}, then ${place.act}, then the wheel to the history's top at ${place.historyTop * 100} % of the view`;
  const moved: string[] = [];
  const titles = Promise.withResolvers<void>();
  const { context, page } = await afterOpener(browser, key, place.opener, titles.promise);
  try {
    const history = historyPanel(page);
    await newestHistoryEntry(page);
    await expect(historyEntries(page).first(), "the history drew no entry").toBeAttached();
    await nextFrames(page);
    if (place.focus === "page") await focusThePage(page);
    if (place.act !== "nothing") {
      await page.keyboard.press(place.act);
      // Tab can bring the control it focuses into Details' view: the box stands still first.
      await detailsStandStill(page, place.act);
    }
    // One read of Details' view, which no act of the row's moves.
    const view = await historyViewport(page);
    // One read of where the key and the act left the history: Details has drawn it and stood
    // still for two frames, and the titles, which alone move it next, are still held.
    const landed = await boxOf(history, "the history");
    const target = view.top + place.historyTop * (view.bottom - view.top);
    await wheelOverDetails(page, Math.round(landed.y - target));
    const blockedBy = detailsRow(page, PROPERTY_LABELS.blocked_by);
    // One read of the place the reader's acts left: the box has stood still for two frames,
    // and the titles, which alone move it next, are still held.
    const blocked = await boxOf(blockedBy, "Blocked by");
    expect(
      blocked.y + blocked.height <= view.top,
      `${named}: Blocked by's foot stood at ${Math.round(blocked.y + blocked.height)} px, in the view from ${Math.round(view.top)}`
    ).toBe(true);
    // The same settled place, read for the entries the view holds wholly: the rests below walk
    // them, and a rest moves nothing unless a growth does.
    const entries = await historyEntries(page).evaluateAll(
      (all, shown) =>
        all
          .map((entry) => ({
            box: entry.getBoundingClientRect(),
            id: entry.getAttribute("data-history-entry") ?? "",
          }))
          .filter(({ box }) => box.top >= shown.top && box.bottom <= shown.bottom)
          .map(({ id }) => id),
      view
    );
    expect(entries.length, `${named}: no entry lies wholly in Details' view`).toBeGreaterThan(0);
    let growths = 0;
    for (const id of entries.slice(0, RESTED_ENTRIES)) {
      for (const part of ["upper", "lower"] as const) {
        const entry = history.locator(`[data-history-entry="${id}"]`);
        // One read of the entry the pointer is to rest on: the rows have stood still since the
        // last growth, which `settledRows` waited out.
        const at = await boxOf(entry, `entry ${id}`);
        await restThePointer(page, {
          x: at.x + 60,
          y: part === "upper" ? at.y + 8 : at.y + at.height - 8,
        });
        // One read of every row in view with the pointer at rest: nothing has moved since.
        const before = await rowsInDetailsView(page);
        let grew = 300;
        if (growths === 0) {
          grew = await grows(page, blockedBy, "Blocked by", titles.resolve, "Blocker 4:");
        } else {
          await blockedBy.evaluate((row, padding) => {
            (row as HTMLElement).style.paddingBottom = `${padding}px`;
          }, growths * 300);
        }
        growths += 1;
        const shifted = movedRows(before, await settledRows(page));
        if (shifted.length > 0) {
          moved.push(
            `${named}, the pointer on entry ${entries.indexOf(id)}'s ${part} part: Blocked by grew ${Math.round(grew)} px above the view and moved ${shifted.join(", ")}`
          );
        }
      }
    }
  } finally {
    titles.resolve();
    await context.close();
  }
  return moved;
}

/** The places of `READER_PLACES` by the key that opened Details and the place of the history's
 *  top, in their order there, each group named by the words its row's title ends with. Every
 *  place falls in one group, so the rows of the sweep below cover every place between them. */
const READER_PLACE_GROUPS = new Map<string, ReaderPlace[]>();
for (const place of READER_PLACES) {
  const group = `${place.opener}, the history's top at ${place.historyTop * 100} % of the view`;
  READER_PLACE_GROUPS.set(group, [...(READER_PLACE_GROUPS.get(group) ?? []), place]);
}

// The sweep, one row for each group of `READER_PLACE_GROUPS`, so that the rows run apart on the
// leg's workers; each seeds an issue of its own whose history runs past every section of Details.
// At each place of the row's group - opened by `t o` or by `t v`; the focus left in Details' box,
// on the Collaborators control or the history, or moved to the page outside it; then nothing,
// Shift, or Tab, which can move the focus and bring the control it reaches into view; then the
// wheel, which brings the history's top to 45 %, 10 % or 75 % of Details' view, Blocked by wholly
// above it at each - the reader's pointer comes to rest on each of the history's entries the view
// holds wholly, from the newest, up to six, at its upper part and at its lower part, a few pixels
// on from where it arrived (`sweepPlace`). For each rest Blocked by grows above the view, and
// every row in the view stands where it stood, within a pixel. At the first rest at each place the
// titles' read lands, which grows the row by 500 px at 1440 px here; that read lands once per
// load, so each later rest grows the row by 300 px more of its own, as a second read would. Two
// loads a row, each well under a quarter of a minute here.
for (const [group, places] of READER_PLACE_GROUPS) {
  test(`wherever the reader is in Details and wherever their pointer rests on the history, Blocked by growing above the view moves no row in it (${group})`, async ({
    browser,
  }) => {
    // A minute for the seed, and 35 s for each place's load and its rests.
    test.setTimeout(60_000 + places.length * 35_000);
    const key = await seedHeldHistory();
    // Each combination that moved a row, so one run names every one of them.
    const moved: string[] = [];
    for (const place of places) moved.push(...(await sweepPlace(browser, key, place)));
    expect(moved, moved.join("\n")).toEqual([]);
  });
}

// ---------------------------------------------------------------------------------------------
// The runs of the issue's steps and its handoffs, each run an entry of the history in Details.
// ---------------------------------------------------------------------------------------------

/** One run the seed writes: the record's id, what the daemon writes of it (`putStep`), each
 *  labelled value the steps draw of it but its two times, and those two times as written. */
interface SeededRun {
  readonly ended: string | null;
  readonly fields: Readonly<Record<string, string>>;
  readonly id: string;
  readonly input: Parameters<typeof putStep>[2];
  readonly started: string | null;
}

/** A moment of the day the runs are dated to, as the daemon writes one. */
const stepAt = (time: string) => `2026-11-16T${time}:00Z`;

/**
 * An issue whose steps ran in two tasks, written as Legion's daemon writes them (`putStep`, as
 * `steps.e2e.ts` writes them): in `docs`, one run that passed; in `default`, its implementation,
 * which passed; a review that failed and sent the task back to step 2; the implementation's
 * second run, which runs and names no model yet; and a merge GitHub made, which names no type,
 * agent, model or start. The server reads a run with no start first, so `default`, the merge's
 * task, comes before `docs`, which no sort by name gives. And an issue no step ran on.
 */
async function seedSteps(): Promise<{ key: string; never: string; runs: readonly SeededRun[] }> {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Runs of two tasks" });
  const never = await createIssue({ project: "CORE", title: "Never ran in Legion" });
  const agent = (role: string) => `legion-core-${issue.key.toLowerCase()}-${role}`;
  const { none, step_results: results } = CONTROL_LABELS;
  const run = (input: SeededRun["input"], fields: SeededRun["fields"]): SeededRun => ({
    ended: input.ended_at ?? null,
    fields: { ...fields, task: input.task ?? "" },
    id: crypto.randomUUID(),
    input,
    started: input.started_at ?? null,
  });
  const implementer = { agent: agent("implementer"), role: "implementer", type: "implementation" };
  const runs = [
    run(
      {
        ...implementer,
        ended_at: stepAt("08:30"),
        generation: 1,
        kind: "step",
        model: "anthropic/claude-opus-5",
        result: "passed",
        started_at: stepAt("08:00"),
        step: 1,
        task: "docs",
        type: "documentation",
      },
      {
        agent: agent("implementer"),
        attempt: "1",
        model: "anthropic/claude-opus-5",
        result: results.passed,
        role: "implementer",
        step: "1",
        type: "documentation",
      }
    ),
    run(
      {
        ...implementer,
        ended_at: stepAt("09:40"),
        generation: 1,
        kind: "step",
        model: "anthropic/claude-opus-5",
        result: "passed",
        started_at: stepAt("09:00"),
        step: 2,
        task: "default",
      },
      {
        agent: agent("implementer"),
        attempt: "1",
        model: "anthropic/claude-opus-5",
        result: results.passed,
        role: "implementer",
        step: "2",
        type: "implementation",
      }
    ),
    run(
      {
        agent: agent("reviewer"),
        ended_at: stepAt("10:30"),
        generation: 1,
        kind: "step",
        model: "openai/gpt-6.1-sol",
        result: "failed",
        returned_to: 2,
        role: "reviewer",
        started_at: stepAt("10:00"),
        step: 4,
        task: "default",
        type: "review",
      },
      {
        agent: agent("reviewer"),
        attempt: "1",
        model: "openai/gpt-6.1-sol",
        result: results.failed,
        returned_to: "step 2",
        role: "reviewer",
        step: "4",
        type: "review",
      }
    ),
    run(
      {
        ...implementer,
        generation: 1,
        kind: "step",
        started_at: stepAt("10:31"),
        step: 2,
        task: "default",
      },
      {
        agent: agent("implementer"),
        attempt: "2",
        model: none,
        result: results.running,
        role: "implementer",
        step: "2",
        type: "implementation",
      }
    ),
    run(
      {
        ended_at: stepAt("11:00"),
        generation: 1,
        kind: "merge",
        result: "passed",
        role: "merger",
        step: 0,
        task: "default",
      },
      {
        agent: none,
        attempt: "1",
        model: none,
        result: results.passed,
        role: "merger",
        step: "Merge",
        type: none,
      }
    ),
  ];
  for (const seeded of runs) await putStep(issue.key, seeded.id, seeded.input);
  return { key: issue.key, never: never.key, runs };
}

/** The ids of `runs` in the order the history draws them, newest first: each at its start, and a
 *  run with none at its end. */
function newestFirst(runs: readonly SeededRun[]): string[] {
  const placeOf = (run: SeededRun) => run.started ?? run.ended ?? "";
  return [...runs]
    .sort((one, other) => placeOf(other).localeCompare(placeOf(one)))
    .map((run) => run.id);
}

// The history lists every run of the issue's steps as one entry, newest first, each at its
// start: each run's task, step, role, type, agent, model, attempt, start, end and result - and the
// step a failed review sent the task back to - as the record holds them, None where it holds
// none, as labelled values and never as a table, and no Steps section beside it. At 1440 px and
// at 390 px every value is whole: Details' box scrolls no way but down, and no value is wider
// than its own box, stands out of Details or of its entry, or is cut. It carries `@phone` for
// that width claim, a layout claim "Phone acceptance" in `packages/dispatch/AGENTS.md` tags
// `@phone` for; the phone reads 390 px alone.
// SLOW(>=2s): five step records written, and the issue loaded and Details opened at two widths.
test("the history draws each run of each task as its record holds it, whole at every width @phone", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  await phase(browser, seedSteps, async (page, seeded) => {
    const desktopProject = test.info().project.name !== "iphone";
    for (const width of desktopProject ? [1440, 390] : [390]) {
      await page.setViewportSize(viewportAt(width));
      await gotoIssue(page, `/issues/${seeded.key}`);
      await openDetails(page);
      for (const run of seeded.runs) await expectRunDrawn(page, run, `at ${width} px`);
      for (const run of seeded.runs) {
        const times = stepRun(page, run.id).locator("time");
        await expect(times).toHaveCount(
          [run.started, run.ended].filter((at) => at !== null).length
        );
      }
      expect(
        await historyRuns(page),
        `the runs are not newest first, each at its start, at ${width} px`
      ).toEqual(newestFirst(seeded.runs));
      await expect(historyPanel(page).getByRole("table")).toHaveCount(0);
      await expect(marginShell(page).getByTestId("issue-steps")).toHaveCount(0);
      expect(await runValuesUnread(page), `a value of a run is not whole at ${width} px`).toEqual(
        []
      );
      expect(
        await fieldsPastTheirEntry(page),
        `a field does not fit its entry at ${width} px`
      ).toEqual([]);
      await assertPageFits(page);
    }
  });
});

/**
 * An issue whose runs Legion's daemon wrote after the issue was made, so each stands above the
 * issue's own creation in the history, with a handoff of the tester's that no run reports: an
 * implementation that passed, a review that failed and sent the task back to step 2, and the
 * implementation's second run, which runs.
 */
async function seedRecentRuns(): Promise<{
  handoff: string;
  key: string;
  runs: readonly string[];
}> {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Runs whose read is refused" });
  const handoff = await createHandoff(
    issue.key,
    "test",
    { phase: "test", summary: "A test pass no run reports." },
    implementer
  );
  const agent = (role: string) => `legion-core-${issue.key.toLowerCase()}-${role}`;
  const later = (minutes: number) => new Date(Date.now() + minutes * 60_000).toISOString();
  const inputs: Parameters<typeof putStep>[2][] = [
    {
      agent: agent("implementer"),
      ended_at: later(2),
      generation: 1,
      kind: "step",
      model: "anthropic/claude-opus-5",
      result: "passed",
      role: "implementer",
      started_at: later(1),
      step: 2,
      task: "default",
      type: "implementation",
    },
    {
      agent: agent("reviewer"),
      ended_at: later(4),
      generation: 1,
      kind: "step",
      model: "openai/gpt-6.1-sol",
      result: "failed",
      returned_to: 2,
      role: "reviewer",
      started_at: later(3),
      step: 4,
      task: "default",
      type: "review",
    },
    {
      agent: agent("implementer"),
      generation: 1,
      kind: "step",
      role: "implementer",
      started_at: later(5),
      step: 2,
      task: "default",
      type: "implementation",
    },
  ];
  const runs: string[] = [];
  for (const input of inputs) {
    const id = crypto.randomUUID();
    await putStep(issue.key, id, input);
    runs.push(id);
  }
  return { handoff: handoff.id, key: issue.key, runs };
}

// The runs are read only once Details opens. A read Dispatch refuses is not asked again on its
// own: the history says so at once with its Retry under its heading, and every other entry it
// reads stays drawn - the issue's creation and the handoff no run reports - with no run entry.
// Only the catch-up's next reads, 2 and 5 s after the opening, ask again - refused here too -
// never retries 500 ms apart. Retry, pressed after those two reads and long before the
// catch-up's next at 15 s, reads the runs again, and each run's entry is drawn above the entries
// drawn before, as each is newer than all of them (the check b-c-runs-failed, its runs half).
// Then, on a load of its own, a read of the bodies Dispatch refuses says so in the entry of the
// handoff it was for, with a Retry, and the handoff's phase, version, author and time stay; its
// Retry, with the read let through, reads the body and the fields appear (its bodies half).
// SLOW(>=2s): three step records and a handoff written, and the catch-up's two refused reads.
test("the history says when the runs fail to read, with a Retry", async ({ browser }) => {
  await phase(browser, seedRecentRuns, async (page, seeded) => {
    const reads: number[] = [];
    let refusing = true;
    await page.route(`**/api/v1/issues/${seeded.key}/steps`, (route) => {
      reads.push(Date.now());
      if (!refusing) return route.fallback();
      return route.fulfill({
        body: JSON.stringify({ code: "INTERNAL", error: "refused" }),
        contentType: "application/json",
        status: 500,
      });
    });
    const settling = watchApiRequests(page);
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectLoadSettled(settling);
    expect(reads, "the runs were read on a load that left Details shut").toEqual([]);

    await openDetails(page);
    const history = historyPanel(page);
    await expect(history.getByRole("alert")).toHaveText(ANSWER_LINES.steps_unreadable);
    await expect(historyHeading(page)).toBeVisible();
    await expect(
      handoffLineOf(page, seeded.handoff),
      "the handoff no run reports went with the runs' failed read"
    ).toBeVisible();
    await expect(
      historyEntries(page).filter({ hasText: "created the issue" }),
      "the issue's creation went with the runs' failed read"
    ).toHaveCount(1);
    await expect(history.locator("[data-step-run]")).toHaveCount(0);
    // The catch-up's reads 2 and 5 s after the opening, inside the default wait of 15 s.
    await expect
      .poll(() => reads.length, { message: "the catch-up did not read again after the refusal" })
      .toBe(3);
    const gaps = reads.slice(1).map((at, index) => at - (reads[index] ?? at));
    expect(
      gaps.every((gap) => gap >= 1_500),
      `a refused read was asked again ${gaps.join(" ms and ")} ms after the one before`
    ).toBe(true);
    await expect(history.getByRole("alert")).toHaveText(ANSWER_LINES.steps_unreadable);
    await expect(history.locator("[data-step-run]")).toHaveCount(0);

    refusing = false;
    await history.getByRole("button", { exact: true, name: CONTROL_LABELS.retry }).click();
    for (const run of seeded.runs) {
      await expect(stepRun(page, run), `Retry did not draw run ${run}`).toHaveCount(1);
    }
    expect(await historyRuns(page)).toEqual([...seeded.runs].reverse());
    await expect(history.getByRole("alert")).toHaveCount(0);
    await expect(handoffLineOf(page, seeded.handoff)).toBeVisible();
    expect(reads, "Retry did not read the runs once more").toHaveLength(4);

    // The bodies half, on a load of its own, whose page holds no body read yet.
    const fields = handoffFieldsOf(page, seeded.handoff);
    const summary = fields.filter({ hasText: "A test pass no run reports." });
    await expect(summary).toHaveCount(1);
    const line = (await handoffLineOf(page, seeded.handoff).textContent()) ?? "";
    const bodiesReads: number[] = [];
    let refusingBodies = true;
    await page.route(
      (url) => url.pathname === `/api/v1/issues/${seeded.key}/handoffs/bodies`,
      (route) => {
        bodiesReads.push(Date.now());
        if (!refusingBodies) return route.fallback();
        return route.fulfill({
          body: JSON.stringify({ code: "INTERNAL", error: "refused" }),
          contentType: "application/json",
          status: 500,
        });
      }
    );
    // A load of the history's own address, which opens Details at the history, with the handoff
    // brought into the view, where its body is read: the runs now drawn above it take it more
    // than one screen below the top of Details at the issue's own address.
    await gotoIssue(page, `/issues/${seeded.key}/activity`);
    await handoffLineOf(page, seeded.handoff).scrollIntoViewIfNeeded();
    const entry = history.locator(`[data-history-entry="handoff:${seeded.handoff}"]`);
    await expect(entry.getByRole("alert")).toHaveText(ANSWER_LINES.handoff_unreadable);
    await expect(
      handoffLineOf(page, seeded.handoff),
      "the handoff's phase, version, author or time went with its failed read"
    ).toHaveText(line);
    await expect(handoffLineOf(page, seeded.handoff).locator("time")).toHaveCount(1);
    await expect(fields).toHaveCount(0);
    expect(bodiesReads, "the bodies were not read once").toHaveLength(1);

    refusingBodies = false;
    await entry.getByRole("button", { exact: true, name: CONTROL_LABELS.retry }).click();
    await expect(summary, "Retry did not draw the handoff's fields").toHaveCount(1);
    await expect(entry.getByRole("alert")).toHaveCount(0);
    await expect(handoffLineOf(page, seeded.handoff)).toHaveText(line);
    expect(bodiesReads, "Retry did not read the bodies once more").toHaveLength(2);
  });
});

// An issue no step ran on draws no run entry and says no failure: its history holds its own
// creation alone.
// SLOW(>=2s): five step records written, and an issue loaded with Details opened.
test("an issue with no run draws no run entry", async ({ browser }) => {
  await phase(browser, seedSteps, async (page, seeded) => {
    const reads = watchStepsReads(page, seeded.never);
    await gotoIssue(page, `/issues/${seeded.never}`);
    await openDetails(page);
    await expect(historyEntries(page).filter({ hasText: "created the issue" })).toHaveCount(1);
    await expect.poll(() => reads.answered(0), { message: "the runs were never read" }).toBe(true);
    await expect(historyPanel(page).locator("[data-step-run]")).toHaveCount(0);
    await expect(historyPanel(page).getByRole("alert")).toHaveCount(0);
    await expect(marginShell(page).getByTestId("issue-steps")).toHaveCount(0);
  });
});

/** How long the steps' reads stay quiet before the row below acts on the issue: longer than each
 *  of the first two gaps of the catch-up Details' opening starts (2 s, then 3 s) and far shorter
 *  than its third (10 s), so the act falls seconds before that catch-up would read again. */
const STEPS_QUIET_MS = 3_500;

// A step record leaves Legion's daemon on a lane of its own and sends no event, so the event of
// the phase it ends can reach the page, and the read that event's refresh starts can answer,
// before the record reaches the server. The history draws the run all the same, at a later read
// of the catch-up that refresh started, with no other event and no reload: here the record is
// written only once that read has answered, and its entry is then drawn, above the entries drawn
// before, as its start is newer than all of them.
// SLOW(>=2s): five step records written, the opening's catch-up waited out, and an event's.
test("the history draws a run whose record reaches the server after its event's refresh has read", async ({
  browser,
}) => {
  await phase(browser, seedSteps, async (page, seeded) => {
    const reads = watchStepsReads(page, seeded.key);
    await gotoIssue(page, `/issues/${seeded.key}`);
    await openDetails(page);
    for (const run of seeded.runs) await expect(stepRun(page, run.id)).toHaveCount(1);
    // The opening's catch-up reads at once and 2 and 5 s on, then at 15 s: the quiet comes after
    // its 5 s read, and so no read of that catch-up's falls between the act below and its event's.
    await expect
      .poll(() => reads.quietFor(STEPS_QUIET_MS), {
        message: "the reads of the steps never went quiet after Details opened",
        timeout: 30_000,
      })
      .toBe(true);

    // A move of the status, an event the feed refreshes the steps for (`changesSteps` in
    // `web/src/api/event-refresh.ts`): the first read after it is the one that refresh starts.
    const eventRead = reads.sent();
    await patchIssue(seeded.key, { status: "in_progress" });
    await expect
      .poll(() => reads.answered(eventRead), {
        message: "no read of the steps after the status moved has answered",
      })
      .toBe(true);

    const late = crypto.randomUUID();
    await putStep(seeded.key, late, {
      agent: `legion-core-${seeded.key.toLowerCase()}-reviewer`,
      generation: 1,
      kind: "step",
      model: "openai/gpt-6.1-sol",
      role: "reviewer",
      started_at: new Date().toISOString(),
      step: 4,
      task: "default",
      type: "review",
    });
    // The catch-up the event started reads again 2 and 5 s after it: 10 s from here covers both
    // reads and the drawing of the second's answer, and ends before that catch-up's 15 s read.
    await expect(
      stepRun(page, late),
      "the run written after its event's read was never drawn"
    ).toHaveCount(1, { timeout: 10_000 });
    expect(await historyRuns(page)).toHaveLength(seeded.runs.length + 1);
  });
});

/**
 * Two issues whose steps ran as Legion's daemon writes them (`putStep`, then `putStepExperiment`
 * for each run that names an experiment): `named`, whose first generation drew the variant `a` of
 * the experiment `planning` in one task and whose newest, the second, drew its variant `b` in two
 * tasks; and `unnamed`, whose first generation drew that same variant `a` and whose newest, the
 * second, names none.
 */
async function seedExperiment(): Promise<{ named: string; unnamed: string }> {
  await createProject({ key: "CORE", name: "Core" });
  const named = await createIssue({ project: "CORE", title: "Runs of an experiment" });
  const unnamed = await createIssue({ project: "CORE", title: "Runs of a newest run with none" });
  const write = async (
    key: string,
    generation: number,
    task: string,
    drawn?: Parameters<typeof putStepExperiment>[2]
  ) => {
    const id = crypto.randomUUID();
    await putStep(key, id, {
      agent: `legion-core-${key.toLowerCase()}-implementer`,
      ended_at: stepAt(`${9 + generation}:30`),
      generation,
      kind: "step",
      model: "anthropic/claude-opus-5",
      result: "passed",
      role: "implementer",
      started_at: stepAt(`${9 + generation}:00`),
      step: 2,
      task,
      type: "implementation",
    });
    if (drawn !== undefined) await putStepExperiment(key, id, drawn);
  };
  const first = { experiment: "planning", variant: "a" };
  const newest = { experiment: "planning", variant: "b" };
  await write(named.key, 1, "default", first);
  await write(named.key, 2, "default", newest);
  await write(named.key, 2, "docs", newest);
  await write(unnamed.key, 1, "default", first);
  await write(unnamed.key, 2, "default");
  return { named: named.key, unnamed: unnamed.key };
}

// Where the records of the issue's newest run, its highest generation, name an experiment, the
// history draws "Experiment <experiment>: variant <variant>" once, directly under its heading and
// above its entries, at 1440 px and at 390 px, and not the variant the first generation drew.
// Where no record of the newest run names one, though the first generation's does, it draws no
// such line. It carries no `@phone` tag: it reads the same markup in the same order at both
// widths, which it sets itself, and the phone changes none of it.
// SLOW(>=2s): nine step and experiment records written, and two issues loaded at two widths.
test("the history names the experiment and the variant of the newest run once under its heading, and none where none is named", async ({
  browser,
}) => {
  await phase(browser, seedExperiment, async (page, seeded) => {
    for (const width of [1440, 390]) {
      await page.setViewportSize(viewportAt(width));
      await gotoIssue(page, `/issues/${seeded.named}`);
      await openDetails(page);
      const history = historyPanel(page);
      await expect(
        history.locator("[data-step-run]"),
        `the history of ${seeded.named} does not draw its three runs at ${width} px`
      ).toHaveCount(3);
      const line = history.locator("[data-step-variant]");
      await expect(
        line,
        `the history draws no experiment line for the newest run of ${seeded.named} at ${width} px`
      ).toHaveCount(1);
      await expect(line, `the experiment line at ${width} px`).toHaveText(
        "Experiment planning: variant b"
      );
      await expect(
        history.getByText(/^Experiment /u),
        `the history draws more than one experiment line at ${width} px`
      ).toHaveCount(1);
      await expect(
        history.locator("h2 + [data-step-variant]"),
        `the experiment line is not directly under the heading at ${width} px`
      ).toHaveCount(1);
      const lineTop = (await line.boundingBox())?.y ?? Number.POSITIVE_INFINITY;
      const firstRun = (await history.locator("[data-step-run]").first().boundingBox())?.y ?? 0;
      expect(lineTop, `the experiment line is not above the runs at ${width} px`).toBeLessThan(
        firstRun
      );

      await gotoIssue(page, `/issues/${seeded.unnamed}`);
      await openDetails(page);
      await expect(history.locator("[data-step-run]")).toHaveCount(2);
      await expect(
        history.getByText(/^Experiment /u),
        `the history of ${seeded.unnamed} draws an experiment its newest run does not name at ${width} px`
      ).toHaveCount(0);
      await expect(history.locator("[data-step-variant]")).toHaveCount(0);
    }
  });
});

/**
 * An issue whose steps ran from a plan, written as Legion's daemon writes them: the plan of the
 * first run (`putPlan`), then each run (`putStep`). `planned`'s plan gives four tasks: `docs`, an
 * implementation, and `default`, an implementation and a review, each needing no task; `release`,
 * a test, which needs both; and `cleanup`, a review, written with no needs, so its needs are not
 * recorded. `docs`' implementation and `default`'s passed and `default`'s review runs; no step of
 * `release` or `cleanup` ran; and a run of `earlier`, a task the plan does not hold, passed.
 */
async function seedPlannedSteps(): Promise<{ planned: string }> {
  await createProject({ key: "CORE", name: "Core" });
  const planned = await createIssue({ project: "CORE", title: "Runs of a plan" });
  await putPlan(planned.key, [
    { id: "docs", needs: [], steps: ["implementer"] },
    { id: "default", needs: [], steps: ["implementer", "reviewer"] },
    { id: "release", needs: ["docs", "default"], steps: ["tester"] },
    { id: "cleanup", steps: ["reviewer"] },
  ]);
  // A run of step `step` of `task` that started at `from`, and passed at `to` where it is given.
  const run = (task: string, step: number, role: string, from: string, to?: string) => {
    const input: Parameters<typeof putStep>[2] = {
      agent: `legion-core-${planned.key.toLowerCase()}-${role}`,
      generation: 1,
      kind: "step",
      model: "anthropic/claude-opus-5",
      role,
      started_at: stepAt(from),
      step,
      task,
      type: role === "reviewer" ? "review" : "implementation",
    };
    return putStep(
      planned.key,
      crypto.randomUUID(),
      to === undefined ? input : { ...input, ended_at: stepAt(to), result: "passed" }
    );
  };
  await run("earlier", 1, "implementer", "07:00", "07:30");
  await run("docs", 1, "implementer", "08:00", "08:30");
  await run("default", 1, "implementer", "09:00", "09:40");
  await run("default", 2, "reviewer", "10:00");
  return { planned: planned.key };
}

/** The issue of `seedPlannedSteps`, and `unrun`, whose plan gives `default` and `docs`, which
 *  needs `default`, and none of whose steps ran. */
async function seedPlannedAndUnrunSteps(): Promise<{ planned: string; unrun: string }> {
  const seeded = await seedPlannedSteps();
  const unrun = await createIssue({ project: "CORE", title: "A plan none of whose steps ran" });
  await putPlan(unrun.key, [
    { id: "default", needs: [], steps: ["implementer", "reviewer"] },
    { id: "docs", needs: ["default"], steps: ["implementer"] },
  ]);
  return { ...seeded, unrun: unrun.key };
}

// Each task of the plan draws the tasks of the plan it needs on a line of its own, in its group
// under the History heading, directly under the group's heading and above its planned steps, in
// the words of today's Steps tab: "Needs docs, default" for `release`, which needs both, and
// "Needs not recorded" for `cleanup`, whose plan node was written with no needs. `docs` and
// `default`, which need none, and `earlier`, a task the plan does not hold, draw no such line;
// `earlier` holds a run and nothing the plan says of a task, so the history draws it as its run's
// entry alone, with no group.
// SLOW(>=2s): a plan and four step records written, and the issue loaded and Details opened.
test("the history names the tasks of the plan each task needs, and says where its plan recorded none", async ({
  browser,
}) => {
  await phase(browser, seedPlannedSteps, async (page, seeded) => {
    await gotoIssue(page, `/issues/${seeded.planned}`);
    await openDetails(page);
    await expect(
      historyPanel(page).locator("[data-step-run]"),
      `the history of ${seeded.planned} does not draw its four runs`
    ).toHaveCount(4);
    await expect(
      stepTask(page, "release").locator("[data-step-needs]"),
      "the history does not name the two tasks of the plan release needs"
    ).toHaveText("Needs docs, default");
    await expect(
      stepTask(page, "release").locator("h3 + [data-step-needs] + [data-step-planned]"),
      "release's line of needs is not directly under its heading and above its planned steps"
    ).toHaveCount(1);
    await expect(
      stepTask(page, "cleanup").locator("[data-step-needs]"),
      "the history does not say that cleanup's plan node recorded no needs"
    ).toHaveText("Needs not recorded");
    for (const task of ["docs", "default"]) {
      await expect(stepTask(page, task), `the history draws no group of ${task}`).toHaveCount(1);
    }
    await expect(taskRuns(page, "earlier"), "the history draws no run of earlier").toHaveCount(1);
    for (const task of ["docs", "default", "earlier"]) {
      await expect(
        stepTask(page, task).locator("[data-step-needs]"),
        `${task}, which needs no task or which the plan does not hold, draws a line of needs`
      ).toHaveCount(0);
    }
  });
});

// Each task of the plan lists the steps the plan gives it in its group under the History
// heading, named as today's Steps tab names the list, above its runs' entries: each step's place,
// its name and where it stands - its newest run's state, in the words a run's result is drawn in,
// or not started before any run of it. `earlier`, a task the plan does not hold, lists none.
// SLOW(>=2s): a plan and four step records written, and the issue loaded and Details opened.
test("the history lists the steps the plan gives each task, each with its place, name and state, above its runs", async ({
  browser,
}) => {
  await phase(browser, seedPlannedSteps, async (page, seeded) => {
    await gotoIssue(page, `/issues/${seeded.planned}`);
    await openDetails(page);
    const { step_not_started: notStarted, step_results: results } = CONTROL_LABELS;
    for (const [task, steps] of [
      ["docs", [`1 implementer ${results.passed}`]],
      ["default", [`1 implementer ${results.passed}`, `2 reviewer ${results.running}`]],
      ["release", [`1 tester ${notStarted}`]],
      ["cleanup", [`1 reviewer ${notStarted}`]],
    ] as const) {
      const planned = stepTask(page, task).getByRole("list", {
        exact: true,
        name: `Planned steps of task ${task}`,
      });
      await expect(planned, `the history lists no planned steps of ${task}`).toHaveCount(1);
      await expect(
        planned.getByRole("listitem"),
        `the planned steps of ${task} do not each read their place, their name and their state`
      ).toHaveText([...steps]);
    }
    await expect(
      taskRuns(page, "default").and(
        historyPanel(page).locator(
          '[data-step-task="default"]:has(> [data-step-planned]) ~ * [data-step-run]'
        )
      ),
      "default's planned steps are not above its two runs"
    ).toHaveCount(2);
    await expect(taskRuns(page, "earlier"), "the history draws no run of earlier").toHaveCount(1);
    await expect(
      stepTask(page, "earlier").locator("[data-step-planned]"),
      "earlier, which the plan does not hold, lists planned steps"
    ).toHaveCount(0);
  });
});

// A task of the plan none of whose steps has run says so in its group under the History heading,
// in the words of today's Steps tab, directly under its planned steps, and a task with a run says
// nothing of the kind. An issue whose plan's tasks hold no run at all draws those tasks, each with
// that line, and no run, and not the None of an issue that holds no task.
// SLOW(>=2s): two plans and four step records written, and two issues loaded and Details opened.
test("the history says a task of the plan has no run yet in place of its runs, and draws a plan none of whose steps ran rather than None", async ({
  browser,
}) => {
  await phase(browser, seedPlannedAndUnrunSteps, async (page, seeded) => {
    await gotoIssue(page, `/issues/${seeded.planned}`);
    await openDetails(page);
    for (const task of ["release", "cleanup"]) {
      await expect(
        stepTask(page, task).locator("[data-step-no-run]"),
        `the history does not say that none of ${task}'s steps has run`
      ).toHaveText(`No step of task ${task} has run yet.`);
      await expect(
        stepTask(page, task).locator("[data-step-planned] + [data-step-no-run]"),
        `${task}'s line is not directly under its planned steps`
      ).toHaveCount(1);
      await expect(taskRuns(page, task)).toHaveCount(0);
    }
    for (const task of ["docs", "default"]) {
      await expect(stepTask(page, task), `the history draws no group of ${task}`).toHaveCount(1);
    }
    await expect(taskRuns(page, "earlier"), "the history draws no run of earlier").toHaveCount(1);
    for (const task of ["docs", "default", "earlier"]) {
      await expect(
        stepTask(page, task).locator("[data-step-no-run]"),
        `${task}, which ran, says that none of its steps has run`
      ).toHaveCount(0);
    }

    await gotoIssue(page, `/issues/${seeded.unrun}`);
    await openDetails(page);
    const history = historyPanel(page);
    await expect(
      history.locator("[data-step-no-run]"),
      `the history does not draw the tasks of ${seeded.unrun}'s plan, each saying none of its steps has run`
    ).toHaveText(["No step of task default has run yet.", "No step of task docs has run yet."]);
    // The runs are read once the lines above are drawn, so no run is drawn from that read on.
    await expect(
      history.locator("[data-step-run]"),
      `the history of ${seeded.unrun} draws a run`
    ).toHaveCount(0);
    await expect(
      history.getByText(CONTROL_LABELS.none, { exact: true }),
      `the history of ${seeded.unrun}, whose plan holds two tasks, reads None`
    ).toHaveCount(0);
  });
});

/** The handoffs and the runs of the timeline's seed, by what each is. */
interface TimelineSeed {
  /** The run of an earlier day, which reports the plan's handoff. */
  readonly earlier: SeededRun;
  /** The handoffs, by their phase: three a run reports, and the tester's, which none reports. */
  readonly handoffs: {
    readonly implement: string;
    readonly plan: string;
    readonly review: string;
    readonly unreported: string;
  };
  readonly key: string;
  /** Today's runs, newest first, each with the labelled values the history draws of it. */
  readonly runs: readonly SeededRun[];
}

/** A commit's id, a path and an address as real handoffs hold them: each one word with no space,
 *  40, 140 and 200 characters long, wider than an entry at 390 px. */
const LONG_SHA = "3f9c2a7e1b4d6f8091a2b3c4d5e6f708192a3b4c";
const LONG_PATH = `packages/dispatch/${"nested-directory/".repeat(6)}handoff-body-load.ts`;
const LONG_URL = `https://example.com/${"segment-".repeat(22)}page`;

/**
 * The issue of the one timeline: 22 threads the project's daemon began three days ago, more than
 * one page of the history and older than everything else; a run of the planner's on an earlier
 * day that reports the plan's handoff; today, an implementation that passed and reports its
 * handoff, whose body holds a list and an object, and a commit's id, a path and an address with
 * no space in them, at the top, in a list and in an object; a review that failed, sent the task
 * back to step 2 and reports its handoff, whose findings are a list of objects; and the
 * implementation's second run, which runs and reports none; and the tester's handoff, which no
 * run reports.
 */
async function seedTimeline(): Promise<TimelineSeed> {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "One timeline of runs" });
  const threeDaysAgo = Date.now() - 3 * 24 * 3_600_000;
  for (let index = 0; index < 22; index += 1) {
    const note = await createMessage(
      issue.key,
      { body: `An older note of the daemon's, ${index}.` },
      asDaemon
    );
    await setCreatedAt("messages", note.id, new Date(threeDaysAgo + index * 60_000).toISOString());
  }
  const write = async (phase: string, body: object) =>
    (await createHandoff(issue.key, phase, { phase, issue: issue.key, ...body }, implementer)).id;
  const handoffs = {
    plan: await write("plan", { summary: "The plan of the timeline." }),
    implement: await write("implement", {
      summary:
        "The first pass of the timeline: the history draws each run as one entry with the handoff it reported.",
      filesChanged: ["web/src/a.ts", "web/src/b.ts"],
      implementerProof: { verdict: "verified", how: "Each check passed." },
      head: LONG_SHA,
      commits: [LONG_PATH, LONG_SHA],
      links: { pullRequest: LONG_URL },
    }),
    review: await write("review", {
      decision: "changes_requested",
      keyFindings: [{ severity: "minor", file: "web/src/a.ts" }],
    }),
    unreported: await write("test", { summary: "A test pass no run reports." }),
  };
  const agent = (role: string) => `legion-core-${issue.key.toLowerCase()}-${role}`;
  const ago = (minutes: number) => new Date(Date.now() - minutes * 60_000).toISOString();
  const { none, step_results: results } = CONTROL_LABELS;
  const run = (input: SeededRun["input"], fields: SeededRun["fields"]): SeededRun => ({
    ended: input.ended_at ?? null,
    fields: { ...fields, task: input.task ?? "" },
    id: crypto.randomUUID(),
    input,
    started: input.started_at ?? null,
  });
  const earlier = run(
    {
      agent: agent("planner"),
      ended_at: ago(25 * 60),
      generation: 1,
      kind: "step",
      model: "anthropic/claude-opus-5",
      report: handoffs.plan,
      result: "passed",
      role: "planner",
      started_at: ago(26 * 60),
      step: 1,
      task: "default",
      type: "planning",
    },
    { agent: agent("planner"), attempt: "1", result: results.passed, role: "planner", step: "1" }
  );
  const runs = [
    run(
      {
        agent: agent("implementer"),
        generation: 1,
        kind: "step",
        role: "implementer",
        started_at: ago(2),
        step: 2,
        task: "default",
        type: "implementation",
      },
      {
        agent: agent("implementer"),
        attempt: "2",
        model: none,
        result: results.running,
        role: "implementer",
        step: "2",
        type: "implementation",
      }
    ),
    run(
      {
        agent: agent("reviewer"),
        ended_at: ago(2),
        generation: 1,
        kind: "step",
        model: "openai/gpt-6.1-sol",
        report: handoffs.review,
        result: "failed",
        returned_to: 2,
        role: "reviewer",
        started_at: ago(3),
        step: 4,
        task: "default",
        type: "review",
      },
      {
        agent: agent("reviewer"),
        attempt: "1",
        model: "openai/gpt-6.1-sol",
        result: results.failed,
        returned_to: "step 2",
        role: "reviewer",
        step: "4",
        type: "review",
      }
    ),
    run(
      {
        agent: agent("implementer"),
        ended_at: ago(3),
        generation: 1,
        kind: "step",
        model: "anthropic/claude-opus-5",
        report: handoffs.implement,
        result: "passed",
        role: "implementer",
        started_at: ago(4),
        step: 2,
        task: "default",
        type: "implementation",
      },
      {
        agent: agent("implementer"),
        attempt: "1",
        model: "anthropic/claude-opus-5",
        result: results.passed,
        role: "implementer",
        step: "2",
        type: "implementation",
      }
    ),
  ];
  for (const seeded of [earlier, ...[...runs].reverse()]) {
    await putStep(issue.key, seeded.id, seeded.input);
  }
  return { earlier, handoffs, key: issue.key, runs };
}

/** Checks that `run`'s entry draws every labelled value the seed holds of it, its two times
 *  among them, and no step it went back to where its record names none. */
async function expectRunDrawn(page: Page, run: SeededRun, what: string): Promise<void> {
  const drawn = stepRun(page, run.id);
  await expect(drawn, `${what}: the history draws no entry of run ${run.id}`).toHaveCount(1);
  for (const [field, value] of Object.entries(run.fields)) {
    await expect(
      drawn.locator(`[data-step-field="${field}"] > dd`),
      `${what}: the ${field} of run ${run.id}`
    ).toHaveText(value);
  }
  await expect(
    drawn.locator('[data-step-field="returned_to"]'),
    `${what}: run ${run.id} names a step it went back to that its record does not`
  ).toHaveCount(run.fields.returned_to === undefined ? 0 : 1);
  for (const [field, at] of [
    ["started", run.started],
    ["ended", run.ended],
  ] as const) {
    const value = drawn.locator(`[data-step-field="${field}"] > dd`);
    await expect(value, `${what}: the ${field} of run ${run.id}`).toHaveText(
      at === null ? CONTROL_LABELS.none : await drawnInstant(page, at)
    );
    if (at !== null) await expect(value.locator("time")).toHaveAttribute("datetime", at);
  }
}

// The history is the issue's one timeline (the check b-c-timeline): each run is one entry,
// newest first, with its task, step, role, type, agent, model, attempt, start, end and result,
// and the step it went back to where it sent the work back. The handoff each run reports is drawn
// open in its entry, a list as numbered entries and an object as keys with values, and folds only
// at its fold control; the handoff no run reports has an entry of its own, open too; and the run
// of an earlier day draws its handoff open at `/activity`, with no day folded. A fold the reader
// made stays through a run written live, Details shut and opened, and the history's next page.
// No Steps section is drawn, and no other part of the page lists a run or a handoff. On the
// phone each field fits its entry's box. Then the parts the later tasks of the plan finish: the
// page read the bodies of the entries in view in one request and sent no read of one handoff
// (t7), and the folded handoff's page address opens it again (t8).
// SLOW(>=2s): 22 messages, four handoffs and four runs written, and the history read live.
test("the history is the one timeline: each run holds the handoff it reported @phone", async ({
  browser,
}) => {
  test.setTimeout(180_000);
  const seeded = await seedTimeline();
  const alice = await reader(browser, "alice");
  try {
    const page = await alice.newPage();
    const handoffReads: string[] = [];
    const bodiesReads: string[] = [];
    page.on("request", (request) => {
      const path = new URL(request.url()).pathname;
      if (/^\/api\/v1\/handoffs\/[0-9a-f-]{36}$/u.test(path)) handoffReads.push(path);
      if (path === `/api/v1/issues/${seeded.key}/handoffs/bodies`) bodiesReads.push(path);
    });
    await gotoIssue(page, `/issues/${seeded.key}/activity`);
    const history = historyPanel(page);
    await expect(history).toBeVisible();
    const { earlier, handoffs, runs } = seeded;

    // The run of an earlier day, at `/activity` and with no press: its entry stands in its own
    // day, and the handoff it reports is drawn open there, as no day of the history is folded.
    // A body is read once its entry is within one screen of Details' view (R6), and at 390 px
    // that run stands further down, so the reader scrolls to it, which presses nothing.
    const earlierRun = stepRun(page, earlier.id);
    await expect(earlierRun, "the run of an earlier day is not drawn").toHaveCount(1);
    await earlierRun.scrollIntoViewIfNeeded();
    await expect(
      earlierRun.locator(
        `[data-handoff-line="${handoffs.plan}"] ~ span [data-handoff-field="summary"]`
      ),
      "the handoff of an earlier day is not drawn open"
    ).toHaveText(/The plan of the timeline\.$/u);
    const { date } = await historyDayOf(page, earlier.started ?? "");
    await expect(earlierRun.locator("xpath=ancestor::li[@data-history-day][1]")).toHaveAttribute(
      "data-history-day",
      date
    );

    for (const run of runs) await expectRunDrawn(page, run, "today's runs");
    await expectRunDrawn(page, earlier, "the run of an earlier day");
    expect(await historyRuns(page), "the runs are not one entry each, newest first").toEqual([
      ...runs.map((run) => run.id),
      earlier.id,
    ]);

    // Each handoff a run reports is drawn open in that run's entry, and in no other place.
    const [running, reviewRun, implementRun] = runs;
    if (running === undefined || reviewRun === undefined || implementRun === undefined) {
      throw new Error("the seed wrote three runs today");
    }
    for (const [run, handoff] of [
      [implementRun, handoffs.implement],
      [reviewRun, handoffs.review],
      [earlier, handoffs.plan],
    ] as const) {
      await expect(
        stepRun(page, run.id).locator(`[data-handoff-line="${handoff}"]`),
        `run ${run.id} does not hold the handoff it reports`
      ).toHaveCount(1);
      await expect(page.locator(`[data-handoff-line="${handoff}"]`)).toHaveCount(1);
      await expect(handoffFoldControl(page, handoff)).toHaveAttribute("aria-expanded", "true");
    }
    await expect(stepRun(page, running.id).locator("[data-handoff-line]")).toHaveCount(0);
    const implementFields = handoffFieldsOf(page, handoffs.implement);
    await expect(
      implementFields.filter({ hasText: "The first pass of the timeline:" }),
      "the implementation's handoff is not drawn open"
    ).toHaveCount(1);
    await expect(
      history.locator(
        `[data-handoff-line="${handoffs.implement}"] ~ span [data-handoff-field="filesChanged"] ol > li`
      ),
      "a list is not drawn as numbered entries"
    ).toHaveText(["web/src/a.ts", "web/src/b.ts"]);
    await expect(
      history.locator(
        `[data-handoff-line="${handoffs.implement}"] ~ span [data-handoff-field="implementerProof"] [data-handoff-key="verdict"] > dd`
      ),
      "an object is not drawn as its keys with their values"
    ).toHaveText("verified");
    await expect(
      history.locator(
        `[data-handoff-line="${handoffs.review}"] ~ span [data-handoff-field="keyFindings"] ol > li [data-handoff-key="severity"] > dd`
      )
    ).toHaveText(["minor"]);

    // The handoff no run reports: an entry of its own, open too.
    const unreported = history.locator(`[data-history-entry="handoff:${handoffs.unreported}"]`);
    await expect(unreported).toHaveCount(1);
    await expect(unreported.locator("[data-step-run]")).toHaveCount(0);
    await expect(unreported.locator('[data-handoff-field="summary"]')).toHaveText(
      /A test pass no run reports\.$/u
    );

    // No Steps section, and no other part of the page lists a run or a handoff.
    await expect(page.getByTestId("issue-steps")).toHaveCount(0);
    await expect(page.locator("[data-step-run]")).toHaveCount(
      await history.locator("[data-step-run]").count()
    );
    await expect(page.locator("[data-handoff-line]")).toHaveCount(
      await history.locator("[data-handoff-line]").count()
    );

    if (test.info().project.name === "iphone") {
      expect(await fieldsPastTheirEntry(page), "a field does not fit its entry's box").toEqual([]);
    }

    // The fold control folds a handoff and opens it again; folded once more, it stays folded.
    // The handoff no run reports is the one folded, so that the run written live below takes it
    // into its own entry: a fold held in the handoff's own drawing would go with that move.
    const fold = handoffFoldControl(page, handoffs.unreported);
    const unreportedFields = handoffFieldsOf(page, handoffs.unreported);
    await fold.click();
    await expect(fold).toHaveAttribute("aria-expanded", "false");
    await expect(unreportedFields).toHaveCount(0);
    await fold.click();
    await expect(fold).toHaveAttribute("aria-expanded", "true");
    await expect(unreportedFields.first()).toBeVisible();
    await fold.click();
    await expect(fold).toHaveAttribute("aria-expanded", "false");

    // A run written while the history is open, which it follows live, and which reports the
    // folded handoff: the handoff moves into the run's entry and stays folded there.
    const live = crypto.randomUUID();
    await putStep(seeded.key, live, {
      agent: `legion-core-${seeded.key.toLowerCase()}-tester`,
      generation: 1,
      kind: "step",
      report: handoffs.unreported,
      role: "tester",
      started_at: new Date().toISOString(),
      step: 3,
      task: "default",
      type: "test",
    });
    await expect(
      stepRun(page, live).locator(`[data-handoff-line="${handoffs.unreported}"]`),
      "the run written live, with the handoff it reports, is not drawn"
    ).toHaveCount(1, { timeout: 20_000 });
    await expect(fold, "a live update opened the folded handoff").toHaveAttribute(
      "aria-expanded",
      "false"
    );

    // Details shut and opened again, at the issue's own address, where shutting Details takes
    // the history away with it.
    await navigateInApp(page, `/issues/${seeded.key}`);
    await closeDetails(page);
    await expect(history).toHaveCount(0);
    await openDetails(page);
    await expect(stepRun(page, live)).toHaveCount(1);
    await expect(fold, "Details opened again drew the folded handoff open").toHaveAttribute(
      "aria-expanded",
      "false"
    );

    // The history's next page, which the seed's older threads fill.
    const entries = await historyEntries(page).count();
    await historyShowMore(page).click();
    await expect.poll(() => historyEntries(page).count()).toBeGreaterThan(entries);
    await expect(fold, "the next page drew the folded handoff open").toHaveAttribute(
      "aria-expanded",
      "false"
    );

    // t7: the bodies of the entries in view are read in one request, and no handoff alone.
    if (test.info().project.name !== "iphone") {
      expect(
        handoffReads,
        "the page read a handoff's body by a read of that handoff alone"
      ).toEqual([]);
      expect(bodiesReads.length, "the page read no bodies in one request").toBeGreaterThan(0);
    }

    // t8: the folded handoff's page address opens it again.
    await navigateInApp(page, `/issues/${seeded.key}/handoffs/${handoffs.unreported}`);
    await expect(fold, "the handoff's page address did not open it").toHaveAttribute(
      "aria-expanded",
      "true"
    );
  } finally {
    await alice.close();
  }
});

/** `count` short strings, each 40 characters, the first naming `label`. */
function shortStrings(label: string, count: number): string[] {
  return Array.from({ length: count }, (_, index) => `${label}, entry ${index}`.padEnd(40, "."));
}

/**
 * The body of a handoff whose stored text the bodies answer sends as exactly `size` bytes, as
 * Go's `json.Marshal` of that text counts them: one JSON object with one list of short strings
 * (`shortStrings`), the last padded to the size. The text holds no character JSON escapes but
 * its quotes, so it is sent as its length, its own two quotes and one byte more for each quote
 * in it: 45 bytes for each item of 40 characters, with its comma.
 */
function handoffSentAs(label: string, size: number): { items: string[] } {
  const sent = (items: readonly string[]) => JSON.stringify(JSON.stringify({ items })).length;
  const items = shortStrings(label, Math.floor((size - sent([])) / 45));
  const last = items.length - 1;
  items[last] = `${items[last]}${".".repeat(size - sent(items))}`;
  if (sent(items) !== size) {
    throw new Error(`the body of ${label} is sent as ${sent(items)} bytes, not ${size}`);
  }
  return { items };
}

/** An issue of the band's seed and its handoffs, newest first, as the history draws them. */
interface BandIssue {
  readonly handoffs: readonly string[];
  readonly key: string;
}

/**
 * The two issues of the band (the check b-c-band). `runs`: twenty runs of the implementer, two
 * minutes apart, each reporting a handoff of about 20,000 bytes, whose summary names its run and
 * whose notes are a list of 440 short strings, so the entries reach well past one screen at
 * every width. `large`: six handoffs no run reports, each sent as 250,000 bytes, so the bodies
 * route answers the first four asked and leaves two.
 */
async function seedBand(): Promise<{ readonly large: BandIssue; readonly runs: BandIssue }> {
  await createProject({ key: "CORE", name: "Core" });
  const runs = await createIssue({
    project: "CORE",
    title: "Twenty runs, each with a large handoff",
  });
  const reported: string[] = [];
  for (let run = 1; run <= 20; run += 1) {
    const handoff = await createHandoff(
      runs.key,
      "implement",
      {
        phase: "implement",
        summary: `The handoff of run ${run}.`,
        notes: shortStrings(`Run ${run}`, 440),
      },
      implementer
    );
    const ago = (minutes: number) => new Date(Date.now() - minutes * 60_000).toISOString();
    await putStep(runs.key, crypto.randomUUID(), {
      agent: `legion-core-${runs.key.toLowerCase()}-implementer`,
      ended_at: ago(60 - 2 * run),
      generation: 1,
      kind: "step",
      model: "anthropic/claude-opus-5",
      report: handoff.id,
      result: "passed",
      role: "implementer",
      started_at: ago(61 - 2 * run),
      step: 2,
      task: "default",
      type: "implementation",
    });
    reported.unshift(handoff.id);
  }
  const large = await createIssue({ project: "CORE", title: "Six handoffs of 250,000 bytes" });
  const written: string[] = [];
  for (let index = 1; index <= 6; index += 1) {
    const body = handoffSentAs(`Handoff ${index}`, 250_000);
    written.unshift((await createHandoff(large.key, "review", body, implementer)).id);
  }
  return {
    large: { handoffs: written, key: large.key },
    runs: { handoffs: reported, key: runs.key },
  };
}

/** The entry of the history the point (`x`, `y`) of the page stands in: its id, and its top. */
function entryAtPoint(page: Page, x: number, y: number): Promise<{ id: string; top: number }> {
  return page.evaluate(
    ([atX, atY]) => {
      const entry = document.elementFromPoint(atX, atY)?.closest("[data-history-entry]");
      const id = entry?.getAttribute("data-history-entry");
      if (entry == null || id == null) throw new Error(`no entry of the history at ${atX}, ${atY}`);
      return { id, top: entry.getBoundingClientRect().top };
    },
    [x, y] as const
  );
}

/** The top of the history's entry `id` on the screen. */
async function entryTop(page: Page, id: string): Promise<number> {
  // The row waited for the entry drawn before it reads this, so one instant's box is its box.
  const box = await historyPanel(page).locator(`[data-history-entry="${id}"]`).boundingBox();
  if (box === null) throw new Error(`the history draws no entry ${id}`);
  return box.y;
}

/**
 * The band's first half, over `runs`: the first read of the bodies names only the handoffs
 * within one screen of Details' view, in the order drawn, and while it is held each of their
 * entries draws its handoff's phase, version, author and time and the word for a body being
 * read. Then the reader turns the wheel until the topmost handoff not read yet stands half a
 * screen above the view: the scroll sends the next read, which asks for it, and as the bodies of
 * the entries above the reader's arrive, the entry under the reader's pointer keeps its top.
 */
async function readTheBand(page: Page, runs: BandIssue): Promise<void> {
  const held = await holdBodiesReads(page, runs.key);
  await gotoIssue(page, `/issues/${runs.key}/activity`);
  await expect(historyPanel(page)).toBeVisible();
  await expect.poll(() => held.reads.length, { message: "the history read no body" }).toBe(1);
  const first = held.reads[0];
  if (first === undefined) throw new Error("the history read no body");
  // The read is held, so every entry stands where it stood as the page sent it.
  const places = await bandPlaces(page, runs.handoffs);
  const near = runs.handoffs.filter((id) => places[id]?.near === true);
  const edge = runs.handoffs.filter((id) => places[id]?.edge === true);
  expect(
    runs.handoffs.length - near.length,
    "every handoff stands within one screen of the view, so the seed proves nothing"
  ).toBeGreaterThan(0);
  expect(first.ids.length).toBeLessThanOrEqual(HANDOFF_BODIES_MAX_IDS);
  expect(
    first.ids.filter((id) => !near.includes(id) && !edge.includes(id)),
    "the first read asks for a handoff beyond one screen of the view"
  ).toEqual([]);
  expect(
    near.filter((id) => !first.ids.includes(id) && !edge.includes(id)),
    "the first read leaves out a handoff within one screen of the view"
  ).toEqual([]);
  expect(first.ids, "the first read does not ask in the order drawn").toEqual(
    runs.handoffs.filter((id) => first.ids.includes(id))
  );
  for (const id of first.ids) {
    const version = runs.handoffs.length - runs.handoffs.indexOf(id);
    const line = handoffLineOf(page, id);
    await expect(
      line,
      `the pending handoff ${id} does not name its phase and version`
    ).toContainText(`recorded the implement handoff, version ${version}`);
    await expect(line, `the pending handoff ${id} does not name its author`).toContainText(
      implementer.origin.session_title
    );
    await expect(line.locator("time"), `the pending handoff ${id} does not say when`).toHaveCount(
      1
    );
    await expect(
      historyPanel(page).locator(`[data-handoff-line="${id}"] ~ span [role="status"]`),
      `the pending handoff ${id} does not say its body is being read`
    ).toHaveText(ANSWER_LINES.loading_handoff);
  }
  await first.release();
  for (const id of first.ids) {
    await expect(handoffFieldsOf(page, id).filter({ hasText: "The handoff of run" })).toHaveCount(
      1
    );
  }

  // The reader turns the wheel, the pointer at the middle of Details' view, until the topmost
  // handoff not read yet stands half a screen above the view.
  const unread = runs.handoffs.find((id) => !first.ids.includes(id));
  if (unread === undefined) throw new Error("the first read asked for every handoff");
  const view = await historyViewport(page);
  // The history was waited for visible above, so its box is drawn at this instant.
  const panel = await historyPanel(page).boundingBox();
  if (panel === null) throw new Error("the history has no box");
  const x = panel.x + panel.width / 2;
  const y = (view.top + view.bottom) / 2;
  // Every handoff's line was drawn before the first read went out, so its place has a box now.
  const unreadTop = await historyPanel(page)
    .locator(`[data-handoff-line="${unread}"] ~ span`)
    .boundingBox();
  if (unreadTop === null) throw new Error(`the history draws no handoff ${unread}`);
  await page.mouse.move(x, y);
  await page.mouse.wheel(0, unreadTop.y - view.top + (view.bottom - view.top) / 2);
  let last = Number.NaN;
  await expect
    .poll(
      async () => {
        const now = await historyScrollTop(page);
        const still = now === last;
        last = now;
        return still;
      },
      { intervals: [200], message: "the wheel's scroll of Details never came to rest" }
    )
    .toBe(true);
  await page.mouse.move(x, y + 1);
  await expect.poll(() => held.reads.length, { message: "the scroll read no body" }).toBe(2);
  const second = held.reads[1];
  expect(second?.ids, "the scroll's read does not ask for the handoff it brought near").toContain(
    unread
  );
  expect(
    (await bandPlaces(page, [unread]))[unread]?.above,
    "the handoff the scroll brought near stands in the view, not above it"
  ).toBe(true);
  const reading = await entryAtPoint(page, x, y + 1);
  held.passAll();
  for (const id of second?.ids ?? []) {
    await expect(handoffFieldsOf(page, id).filter({ hasText: "The handoff of run" })).toHaveCount(
      1
    );
  }
  await expect
    .poll(async () => Math.abs((await entryTop(page, reading.id)) - reading.top), {
      message: `the entry under the reader's pointer (${reading.id}) moved as bodies above it arrived`,
    })
    .toBeLessThanOrEqual(1);
}

/**
 * The band's second half, over `large`: the first read asks for the six handoffs newest first,
 * and its answer leaves the two oldest. With the reader not scrolling, 2 seconds after that
 * answer is drawn the page asks for the left ones whose entries stand within one screen of the
 * view, as they stand once the answer is drawn, and for none beyond it; `every` says that the
 * viewport holds the six in its view, so both are asked for. Answers the viewport's height that
 * holds the six, as drawn once their bodies are.
 */
async function readTheLeftIds(page: Page, large: BandIssue, every: boolean): Promise<number> {
  const held = await holdBodiesReads(page, large.key);
  await gotoIssue(page, `/issues/${large.key}/activity`);
  await expect.poll(() => held.reads.length, { message: "the history read no body" }).toBe(1);
  const first = held.reads[0];
  if (first === undefined) throw new Error("the history read no body");
  expect(first.ids, "the first read does not ask for the six handoffs, newest first").toEqual(
    large.handoffs
  );
  held.passAll();
  await first.release();
  const answered = first.answeredAt() ?? Date.now();
  const left = large.handoffs.slice(4);
  expect(first.left(), "the answer does not leave the two oldest handoffs").toEqual(left);
  for (const id of large.handoffs.slice(0, 4)) {
    await expect(handoffFieldsOf(page, id)).toHaveCount(1);
  }
  const drawnAt = Date.now();
  const places = await bandPlaces(page, left);
  const near = left.filter((id) => places[id]?.near === true);
  expect(
    left.filter((id) => places[id]?.edge === true),
    "a left handoff stands at an edge of the band, where the row cannot tell which side"
  ).toEqual([]);
  expect(near, "the viewport does not hold the six handoffs within one screen").toEqual(
    every ? left : near
  );
  // The window the claim is about: 2 s after the answer is drawn the page asks for the left ids
  // in the band, and 1.5 s more lets that read reach the route before the row reads what came.
  await page.waitForTimeout(Math.max(0, drawnAt + 3_500 - Date.now()));
  expect(
    held.reads.slice(1).map((read) => read.ids),
    "the reads after the first do not ask for the left handoffs within one screen alone"
  ).toEqual([near].filter((ids) => ids.length > 0));
  for (const read of held.reads.slice(1)) {
    expect(
      read.sentAt - answered,
      "the left handoffs were asked for sooner than 2 s after the answer"
    ).toBeGreaterThanOrEqual(1_900);
  }
  for (const id of near) await expect(handoffFieldsOf(page, id)).toHaveCount(1);
  // The four bodies were waited for drawn above, so the last entry has its box at this instant.
  const lastEntry = await historyPanel(page)
    .locator(`[data-history-entry="handoff:${large.handoffs.at(-1)}"]`)
    .boundingBox();
  const view = await historyViewport(page);
  const size = page.viewportSize();
  if (lastEntry === null || size === null) throw new Error("the history has no box");
  return size.height + Math.max(0, Math.ceil(lastEntry.y + lastEntry.height - view.bottom)) + 120;
}

// The history reads a handoff's body only once the handoff's entry is within one screen of
// Details' view (the check b-c-band): the first read names those within one screen alone, each
// pending entry draws its handoff's phase, version, author and time and the word for a body
// being read, a scroll sends the next read, and the reader's entry keeps its place as the bodies
// of entries above it arrive. An answer that leaves ids, as the route's 1 MiB leaves two of six
// bodies of 250,000 bytes, is followed 2 seconds after it is drawn by a read of the left ids
// within one screen and of none beyond; at a viewport tall enough that all six stand within one
// screen, that read names both. It carries `@phone` for the reader's place at 390 px, where the
// entries are tallest and the place-keeping control of the check is read.
// SLOW(>=2s): 26 handoffs and 20 runs written, three loads, and two windows of 3.5 s.
test("the history reads the bodies of the entries near the view and keeps the reader in place @phone", async ({
  browser,
}) => {
  test.setTimeout(240_000);
  const seeded = await seedBand();
  const alice = await reader(browser, "alice");
  try {
    await readTheBand(await alice.newPage(), seeded.runs);
    const height = await readTheLeftIds(await alice.newPage(), seeded.large, false);
    const tall = await alice.newPage();
    const size = tall.viewportSize();
    if (size === null) throw new Error("the page has no viewport");
    await tall.setViewportSize({ height, width: size.width });
    await readTheLeftIds(tall, seeded.large, true);
  } finally {
    await alice.close();
  }
});

/** The issues of the handoffs' page addresses (the check b-c-address), with the handoffs and the
 *  run their addresses name. */
interface AddressSeed {
  /** The plan's handoff, written three days ago, which no run reports, at `earlierAt`. */
  readonly earlier: string;
  readonly earlierAt: string;
  readonly key: string;
  /** The four handoffs written after `today`, each sent as 200,000 bytes, so they stand above it. */
  readonly large: readonly string[];
  /** The newest run, half an hour old, under every entry of today written after it. */
  readonly newestRun: string;
  /** An issue whose handoff was written before 205 status moves: neither the log's first page of
   *  200 events nor the history's first 20 entries holds it. */
  readonly paged: { readonly key: string; readonly older: string };
  /** The handoff of today, under the four of 200,000 bytes and twelve notes of the daemon's, so
   *  that it stands below the first screen of today's Conversation tab too. */
  readonly today: string;
  /** When yesterday's run started, the day between today and `earlierAt`. */
  readonly yesterdayAt: string;
}

/** Dates handoff `id` of issue `key`, and the `handoff.recorded` event of its write, at `at`: the
 *  server stamps both with the moment of the write. */
async function dateHandoff(key: string, id: string, at: string): Promise<void> {
  await sql(
    `UPDATE handoffs SET created_at = ${sqlLiteral(at)}::timestamptz WHERE id = ${sqlLiteral(id)}::uuid`
  );
  const recorded = (await allEvents(key)).find(
    (event) => event.type === "handoff.recorded" && event.payload.handoff_id === id
  );
  if (recorded === undefined) throw new Error(`${key} recorded no event of handoff ${id}`);
  await setEventCreatedAt(recorded.id, at);
}

/**
 * The seed of the handoffs' page addresses. Its first issue holds, oldest first: the plan's
 * handoff of three days ago, which no run reports; yesterday's run of the planner's; the newest
 * run, half an hour ago; then, today, 25 status moves, the implementer's handoff, four handoffs of
 * 200,000 bytes and twelve notes of the daemon's. Its second holds one handoff and then 205 status
 * moves, more than 20 entries and more than 200 events.
 */
async function seedAddresses(): Promise<AddressSeed> {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Handoff addresses" });
  const ago = (minutes: number) => new Date(Date.now() - minutes * 60_000).toISOString();
  const agent = (role: string) => `legion-core-${issue.key.toLowerCase()}-${role}`;
  const write = async (key: string, phase: string, body: object) =>
    (await createHandoff(key, phase, { phase, issue: key, ...body }, implementer)).id;
  const earlierAt = ago(3 * 24 * 60);
  const earlier = await write(issue.key, "plan", { summary: "The plan of three days ago." });
  await dateHandoff(issue.key, earlier, earlierAt);
  const yesterdayAt = ago(26 * 60);
  await putStep(issue.key, crypto.randomUUID(), {
    agent: agent("planner"),
    ended_at: ago(25 * 60),
    generation: 1,
    kind: "step",
    model: "anthropic/claude-opus-5",
    result: "passed",
    role: "planner",
    started_at: yesterdayAt,
    step: 1,
    task: "default",
    type: "planning",
  });
  const newestRun = crypto.randomUUID();
  await putStep(issue.key, newestRun, {
    agent: agent("implementer"),
    generation: 1,
    kind: "step",
    role: "implementer",
    started_at: ago(30),
    step: 2,
    task: "default",
    type: "implementation",
  });
  await writeStatusMoves(issue.key, 25);
  const today = await write(issue.key, "implement", {
    summary: "The implementation the address names.",
  });
  const large: string[] = [];
  for (let index = 1; index <= 4; index += 1) {
    const body = handoffSentAs(`Review ${index}`, 200_000);
    large.unshift((await createHandoff(issue.key, "review", body, implementer)).id);
  }
  for (let index = 0; index < 12; index += 1) {
    await createMessage(
      issue.key,
      { body: `A note of the daemon's after the handoffs, ${index}.` },
      asDaemon
    );
  }
  const paged = await createIssue({ project: "CORE", title: "A handoff under 205 status moves" });
  const older = await write(paged.key, "test", { summary: "The test pass under the first page." });
  await writeStatusMoves(paged.key, 205);
  return {
    earlier,
    earlierAt,
    key: issue.key,
    large,
    newestRun,
    paged: { key: paged.key, older },
    today,
    yesterdayAt,
  };
}

/**
 * Waits until the history at a handoff's page address has landed on handoff `id`, whose entry is
 * `entry`: Details open, the entry marked, the handoff drawn open with its fields and its line
 * wholly in Details' view, and no day of the history folded. `what` names the case.
 */
async function expectHandoffLanded(
  page: Page,
  entry: string,
  id: string,
  what: string
): Promise<void> {
  await expect(marginShell(page), `${what}: the address did not open Details`).toBeVisible();
  await expect(
    historyPanel(page).locator(`[data-history-entry="${entry}"]`),
    `${what}: the address did not mark the entry that holds the handoff`
  ).toHaveAttribute("aria-current", "true");
  await expect(
    handoffFoldControl(page, id),
    `${what}: the address left the handoff folded`
  ).toHaveAttribute("aria-expanded", "true");
  await expect(
    handoffFieldsOf(page, id).first(),
    `${what}: the handoff's fields are not drawn`
  ).toBeVisible();
  await expect
    .poll(() => whollyInHistoryView(handoffLineOf(page, id)), {
      message: `${what}: the handoff is not in Details' view`,
    })
    .toBe(true);
  expect(await historyDaysNotOpen(page), `${what}: a day of the history is folded`).toEqual([]);
}

// A handoff's page address opens the new page with Details open at the history's entry that
// holds that handoff, marked, the handoff open and in view, no day folded (the check
// b-c-address): a handoff of today, under four handoffs of 200,000 bytes whose bodies, read once
// the history has landed, grow above it and leave it in view; a handoff of three days ago, with
// yesterday drawn open between; and a handoff older than the log's first page of 200 events and
// the history's first 20 entries, drawn with no older page of the log read. An id that is no
// handoff of the issue opens Details and says no such handoff exists, and `/steps` opens Details
// at the newest run. On a phone Details stays open at each. With the new page off, the same
// address opens today's Conversation tab with the handoff's card open and in view, though it
// stands below the tab's first screen.
// SLOW(>=2s): two issues of 236 writes, four handoffs of 200,000 bytes read, and six loads.
test("a handoff page address opens its entry in the history @phone", async ({ browser }) => {
  test.setTimeout(240_000);
  const seeded = await seedAddresses();
  const alice = await reader(browser, "alice");
  try {
    const page = await alice.newPage();

    // Today's handoff, under the four of 200,000 bytes: the address marks it and draws it open,
    // and the bodies of the four, within one screen of the view, are read once the history has
    // landed and drawn above it, and it stays in view.
    await gotoIssue(page, `/issues/${seeded.key}/handoffs/${seeded.today}`);
    await expect(
      historyPanel(page).locator(`[data-history-entry="handoff:${seeded.today}"]`),
      "the address of a handoff of today opened no entry"
    ).toHaveAttribute("aria-current", "true");
    for (const id of seeded.large) {
      await expect(
        handoffFieldsOf(page, id).first(),
        `the body of the handoff of 200,000 bytes ${id} above the marked one is not drawn`
      ).toBeVisible();
    }
    await expectHandoffLanded(
      page,
      `handoff:${seeded.today}`,
      seeded.today,
      "a handoff of today under four handoffs of 200,000 bytes"
    );

    // A handoff of an earlier day, three days ago: the history is drawn down to it, and every
    // day it draws is open, yesterday's between them among them.
    await gotoIssue(page, `/issues/${seeded.key}/handoffs/${seeded.earlier}`);
    for (const at of [seeded.yesterdayAt, seeded.earlierAt]) {
      const { date } = await historyDayOf(page, at);
      await expect(
        historyPanel(page).locator(`[data-history-day="${date}"]`),
        `the history at a handoff of an earlier day draws no day ${date}`
      ).toHaveCount(1);
    }
    expect(
      await historyDaysNotOpen(page),
      "the history at a handoff of an earlier day folds another day"
    ).toEqual([]);
    await expectHandoffLanded(
      page,
      `handoff:${seeded.earlier}`,
      seeded.earlier,
      "a handoff of an earlier day"
    );

    // A handoff older than the log's first page of 200 events and the history's first 20
    // entries: its entry is drawn under them, and no older page of the log is read for it.
    const olderReads: string[] = [];
    page.on("request", (request) => {
      const url = new URL(request.url());
      if (isOlderPageRead(url, seeded.paged.key)) olderReads.push(url.search);
    });
    await gotoIssue(page, `/issues/${seeded.paged.key}/handoffs/${seeded.paged.older}`);
    await expectHandoffLanded(
      page,
      `handoff:${seeded.paged.older}`,
      seeded.paged.older,
      "a handoff older than the history's first page"
    );
    expect(olderReads, "the history read an older page of the log for the handoff").toEqual([]);

    // An id that is no handoff of the issue.
    await gotoIssue(page, `/issues/${seeded.key}/handoffs/${crypto.randomUUID()}`);
    await expect(marginShell(page), "an address of no handoff did not open Details").toBeVisible();
    await expect(
      historyPanel(page).locator("[data-handoff-missing]"),
      "an address of no handoff of the issue does not say that no such handoff exists"
    ).toHaveText(ANSWER_LINES.handoff_missing);
    await expect(historyPanel(page).locator('[aria-current="true"]')).toHaveCount(0);

    // `/steps`: Details at the history's newest run, which stands under the first 20 entries.
    await gotoIssue(page, `/issues/${seeded.key}/steps`);
    await expect(marginShell(page), "/steps did not open Details").toBeVisible();
    const newestHead = stepRun(page, seeded.newestRun).locator('[data-step-field="task"]');
    await expect(newestHead, "/steps did not draw the newest run").toHaveCount(1);
    await expect
      .poll(() => whollyInHistoryView(newestHead), {
        message: "/steps did not open Details at the newest run",
      })
      .toBe(true);
    await expect(marginShell(page), "Details shut after /steps landed").toBeVisible();
  } finally {
    await alice.close();
  }

  // With the new page off, today's page: the Conversation tab, the handoff's card open and in
  // view, though the cards of the twelve notes and the four handoffs written after it take it
  // below the tab's first screen.
  const bob = await asUser(browser, "bob");
  try {
    const page = await bob.newPage();
    await page.goto(`/issues/${seeded.key}/handoffs/${seeded.today}`);
    await expect(page.getByRole("tab", { name: "Conversation" })).toHaveAttribute(
      "aria-selected",
      "true"
    );
    const card = page.locator(`li[data-turn="${conversationItemId("handoff", seeded.today)}"]`);
    await expect(card, "the address did not mark the handoff's card").toHaveAttribute(
      "aria-current",
      "true"
    );
    await expect(
      card.getByRole("button", { name: "Hide fields" }),
      "the address left the handoff's card shut"
    ).toHaveAttribute("aria-expanded", "true");
    await expect(card.locator('[data-handoff-field="summary"]')).toContainText(
      "The implementation the address names."
    );
    expect(
      await card.evaluate((turn) => {
        let box: HTMLElement | null = turn.parentElement;
        while (box !== null && !["auto", "scroll"].includes(getComputedStyle(box).overflowY)) {
          box = box.parentElement;
        }
        const port = box ?? document.scrollingElement ?? document.documentElement;
        const top = box === null ? 0 : box.getBoundingClientRect().top;
        return turn.getBoundingClientRect().top - top + port.scrollTop > port.clientHeight;
      }),
      "the handoff's card stands within the tab's first screen, so the row proves nothing"
    ).toBe(true);
    await expect(card, "the address did not bring the handoff's card into view").toBeInViewport();
  } finally {
    await bob.close();
  }
});

/** The index of the read of the runs that came 9 seconds or more after the one before it: the
 *  catch-up's 15-second read, after its reads at once, at 2 s and at 5 s, of the newest start of
 *  the history's catch-up; undefined until the page has sent it. */
function fifteenSecondRead(reads: StepsReads): number | undefined {
  for (let index = 1; index < reads.sent(); index += 1) {
    const gap = (reads.sentAt(index) ?? 0) - (reads.sentAt(index - 1) ?? 0);
    if (gap >= 9_000) return index;
  }
  return undefined;
}

// With the new page and Details open, the Legion block shows a change of the issue's stored state
// within 5 seconds, and the history shows a new run within 5 seconds of its write, though the
// write is the run's alone and appends no event: the history reads the runs again as the runs'
// revision of the issue's live view moves. The run is written 6 seconds after the history's
// 15-second catch-up read, so the catch-up's own next read, 30 seconds after that one, cannot
// bring it in time. Then the run's handoff and its end, which reports it, as the daemon writes
// them, and the handoff is in the run's entry within 5 seconds. The page makes no navigation (the
// check b-c-live).
// SLOW(>=2s): the catch-up's first 15 seconds waited out, then three writes each waited for.
test("the Legion block and the history follow a change with no reload", async ({ browser }) => {
  test.setTimeout(120_000);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "The block and the history live" });
  await seedLegionState(issue.key, { claims: { planner: "working" }, phase: "planning", slot: 0 });
  const alice = await reader(browser, "alice");
  try {
    const page = await alice.newPage();
    const reads = watchStepsReads(page, issue.key);
    await gotoIssue(page, `/issues/${issue.key}/activity`);
    const phaseLine = marginShell(page)
      .getByRole("region", { name: CONTROL_LABELS.legion.heading })
      .locator('[data-legion-line="phase"]');
    await expect(phaseLine).toContainText(PHASE_WORDS.steps.planning);
    // A mark on this document alone: a reload would load a new one, which carries no attribute of
    // the row's, while a live update leaves the document as it is.
    await page.evaluate(() => document.documentElement.setAttribute("data-loaded-once", "yes"));

    await seedLegionState(issue.key, {
      claims: { implementer: "working" },
      phase: "implementing",
      slot: 0,
    });
    await expect(
      phaseLine,
      "the block did not show the stored phase within 5 seconds"
    ).toContainText(PHASE_WORDS.steps.implementing, { timeout: 5_000 });

    // The catch-up's 15-second read, the first 9 seconds or more after the read before it.
    await expect
      .poll(
        () => {
          const read = fifteenSecondRead(reads);
          return read !== undefined && reads.answered(read);
        },
        { message: "the history's catch-up never read the runs 15 seconds on", timeout: 30_000 }
      )
      .toBe(true);
    const answeredAt = reads.answeredAt(fifteenSecondRead(reads) ?? 0) ?? Date.now();
    // The row's claim is a span: the run is written 6 s after the 15 s read, so only its revision can bring it in 5 s.
    await page.waitForTimeout(Math.max(0, answeredAt + 6_000 - Date.now()));
    const sentBefore = reads.sent();

    const run = crypto.randomUUID();
    await putStep(issue.key, run, {
      agent: `legion-core-${issue.key.toLowerCase()}-implementer`,
      generation: 1,
      kind: "step",
      role: "implementer",
      started_at: new Date().toISOString(),
      step: 2,
      task: "default",
      type: "implementation",
    });
    await expect(
      stepRun(page, run),
      "the history did not show the run within 5 seconds"
    ).toHaveCount(1, { timeout: 5_000 });
    expect(reads.sent(), "the run reached the history with no read of the runs").toBeGreaterThan(
      sentBefore
    );

    const handoff = await createHandoff(
      issue.key,
      "implement",
      { phase: "implement", issue: issue.key, summary: "Written as the run ends." },
      implementer
    );
    await putStep(issue.key, run, {
      ended_at: new Date().toISOString(),
      report: handoff.id,
      result: "passed",
    });
    await expect(
      stepRun(page, run).locator(`[data-handoff-line="${handoff.id}"]`),
      "the run's handoff was not in its entry within 5 seconds"
    ).toHaveCount(1, { timeout: 5_000 });
    await expect(page.locator("html")).toHaveAttribute("data-loaded-once", "yes");
  } finally {
    await alice.close();
  }
});

// A person's comment cites a block of a document the issue never held, by an address as long as a
// real block's: the issue's key, the document's slug and the block's UUID. The read of what the
// words cite names no target for it, so the outline draws its chip in place in the comment's
// words by the whole address, which is wider than the row at 390 px. The chip breaks the address
// where it must, so the page is no wider than the window; and the chip is drawn, holding the
// address, so a page that draws no chip fails the row.
// SLOW(>=2s): an issue and a comment written, a sign-in, and one load of the issue waited out
// until its reads settle.
test("a row's chip whose target is gone wraps inside the page at 390 px", async ({ browser }) => {
  await phase(
    browser,
    async () => {
      await createProject({ key: "CORE", name: "Core" });
      const issue = await createIssue({ project: "CORE", title: "A chip whose target is gone" });
      const block = "4f4f7706-5e2f-4f98-ba08-ba9afa843985";
      const address = `dispatch://${issue.key}/artifact/release-notes-md#b-${block}`;
      const comment = await createComment(
        issue.key,
        { body: `Bob reads ${address} before the release.` },
        { login: "bob" }
      );
      return { address, comment: comment.id, key: issue.key };
    },
    async (page, seeded) => {
      await page.setViewportSize(PHONE);
      // The names of what the words cite arrive by a read of their own after the tree, so the
      // load is waited out until its reads settle: the chip is then drawn as that read left it.
      const settling = watchApiRequests(page);
      await gotoIssue(page, `/issues/${seeded.key}`);
      await expectLoadSettled(settling);
      await expectIssuePageDrawn(page);
      const chip = rowWords(nodeRow(page, seeded.comment)).locator("a[data-row-chip]");
      await expect(chip, "the comment's row draws no chip in its words").toHaveCount(1);
      await expect(chip, "the chip does not draw its target's whole address").toHaveText(
        seeded.address
      );
      await assertPageFits(page);
    }
  );
});

/**
 * Asserts that `link`, a reference link drawn by the renderer inside Details, is drawn in the
 * words' own ink and never in the old application's blue (`rgb(0, 105, 168)`): first that the
 * link is drawn, so the assertion cannot pass on a page with no link, then its computed colour
 * against the old blue and against the words around it (`.dispatch-markdown`).
 */
async function expectLinkInWordsInk(link: Locator, where: string): Promise<void> {
  await expect(link, `${where} draws no link for its reference`).toHaveCount(1);
  const { color, words } = await link.evaluate((element) => {
    const markdown = element.closest(".dispatch-markdown");
    return {
      color: getComputedStyle(element).color,
      words: markdown === null ? "" : getComputedStyle(markdown).color,
    };
  });
  expect(color, `a link of ${where} is drawn in the old application's blue`).not.toBe(
    "rgb(0, 105, 168)"
  );
  expect(color, `a link of ${where} is not drawn in the words' own ink`).toBe(words);
}

/** Opens Details' Referenced by section and returns its toggle and panel. */
async function openReferencedBy(page: Page): Promise<{ control: Locator; panel: Locator }> {
  const control = marginSection(page, "references").getByRole("button", {
    name: new RegExp(`^${PROPERTY_LABELS.referenced_by}`, "u"),
  });
  await control.click();
  await expect(control).toHaveAttribute("aria-expanded", "true");
  return { control, panel: page.locator(`[id="${await control.getAttribute("aria-controls")}"]`) };
}

/** An issue a comment of another issue cites, as bob, so Details' Referenced by draws that
 *  comment's excerpt whole through the renderer, with a link to the issue in it; the issue's key.
 *  The project is `CORE`. */
async function seedCitedIssue(): Promise<string> {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Cited by another issue" });
  const other = await createIssue({ project: "CORE", title: "Citing issue" });
  await createComment(
    other.key,
    { body: `This comment cites dispatch://${issue.key} in its excerpt.` },
    { login: "bob" }
  );
  return issue.key;
}

/** The link to the issue `key` in an excerpt Details' Referenced by draws, in `panel`. */
function excerptLinkTo(panel: Locator, key: string): Locator {
  return panel.locator(`[data-reference-rows] .dispatch-markdown a[href="/issues/${key}"]`);
}

// What the rows below keep on the page's window.
declare global {
  interface Window {
    /** Set by a row before its press; a load of a document drops it, a move of the router
     *  keeps it. */
    __stayed?: true;
  }
}

/**
 * Opens the issue `seedCitedIssue` writes at `?issue_page=new` as alice, who has never turned the
 * new page on, at `width` px; opens Details' Referenced by; presses `part` of the excerpt's link
 * to the issue itself (the key the link draws in a span of its own, or the link); and holds that
 * the press kept the parameter and the new page, and loaded no document.
 */
async function pressLinkToItself(
  browser: Browser,
  width: number,
  part: "key" | "link"
): Promise<void> {
  const key = await seedCitedIssue();
  const opened = `/issues/${key}?issue_page=new`;
  const pressed = part === "key" ? "the press on the key" : "the press";
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.setViewportSize(viewportAt(width));
    await gotoIssue(page, opened);
    await openDetails(page);
    const { panel } = await openReferencedBy(page);
    const link = excerptLinkTo(panel, key);
    await expect(link, "Referenced by draws no link to the issue in its excerpt").toHaveCount(1);
    let target = link;
    if (part === "key") {
      target = link.locator("span").filter({ hasText: key });
      await expect(target, "the link draws no key of its own").toHaveCount(1);
    }
    await page.evaluate(() => {
      window.__stayed = true;
    });
    await target.click();
    await expect(page, `${pressed} loaded the issue's address without the parameter`).toHaveURL(
      opened
    );
    await expectIssuePageDrawn(page);
    // A press that starts a load returns once that load has committed (Playwright's click waits
    // for the navigation it began), so this read is of the document the press left the reader in.
    expect(
      await page.evaluate(() => window.__stayed === true),
      `${pressed} loaded the document again`
    ).toBe(true);
  } finally {
    await alice.close();
  }
}

// At 767 px and below Details covers the page, and a link pressed inside it closes Details in the
// capture phase (`Margin.tsx`'s `closeOnLink`), before the press reaches the document. A reference
// to a known issue is drawn as its key in a span, then its title, so a press on the key lands on
// that span, and Details' unmount takes the span out of the link before the document hears the
// press. The page must still follow the link in place, with `?issue_page=new` kept, and load no
// document. SLOW(>=2s): two issues and a citing comment written, a sign-in, and one load of the
// issue with Referenced by opened.
test("at 390 px a press on the key of a link in Details' Referenced by to the issue itself keeps ?issue_page=new and loads no document", async ({
  browser,
}) => {
  await pressLinkToItself(browser, 390, "key");
});

// A comment of another issue cites the issue, so Details' Referenced by draws its whole excerpt
// through the renderer. The excerpt's wrapper carries `data-node-words`, so the link the renderer
// draws for that reference takes the words' own ink rather than the old application's link blue.
// SLOW(>=2s): two issues and a citing comment written, a sign-in, and one load of the issue with
// Referenced by opened.
test("a reference in an excerpt of Details' Referenced by is drawn in the words' ink, not the old application's blue", async ({
  browser,
}) => {
  await phase(
    browser,
    async () => ({ key: await seedCitedIssue() }),
    async (page, seeded) => {
      await gotoIssue(page, `/issues/${seeded.key}`);
      await openDetails(page);
      const { panel } = await openReferencedBy(page);
      await expectLinkInWordsInk(excerptLinkTo(panel, seeded.key), "Referenced by");
    }
  );
});

// An approval request is a line of Details' history whose question is drawn inline through the
// renderer (`SentenceParts`). The event line carries `data-node-words`, so a reference in that
// question takes the words' own ink rather than the old application's link blue.
// SLOW(>=2s): two issues and an approval request written, a sign-in, and one load of the history.
test("a reference in a question the history quotes is drawn in the words' ink, not the old application's blue", async ({
  browser,
}) => {
  await phase(
    browser,
    async () => {
      await createProject({ key: "CORE", name: "Core" });
      const cited = await createIssue({ project: "CORE", title: "Referenced issue" });
      const issue = await createIssue({
        project: "CORE",
        spec: "The plan.",
        title: "Approval quoting a reference",
      });
      const opened = await approvalCiting(issue, cited.key, activityAgent);
      return { at: opened.created_at, cited: cited.key, key: issue.key, seq: opened.seq };
    },
    async (page, seeded) => {
      await gotoIssue(page, `/issues/${seeded.key}/activity`);
      const line = await seededHistoryEntry(
        page,
        historyPanel(page).locator(`[data-event-seq="${seeded.seq}"]`),
        seeded.at
      );
      await expectLinkInWordsInk(
        line.locator(`.dispatch-markdown a[href="/issues/${seeded.cited}"]`),
        "the history"
      );
      // Pinning the line draws it in Details' Pinned too (`IssuePins.tsx`'s `SentenceParts`):
      // the same link there must take the words' own ink as well.
      await line.getByRole("button", { exact: true, name: CONTROL_LABELS.pin }).click();
      await expectLinkInWordsInk(
        pinsList(page).locator(`.dispatch-markdown a[href="/issues/${seeded.cited}"]`),
        "Pinned"
      );
    }
  );
});

// The issue touches no component, for a reason that cites another issue: Details' Components row
// draws that reason through the renderer (`IssueComponentsLine`), inside the words' mark, so the
// link the renderer draws for the reference takes the words' own ink rather than the old
// application's link blue.
// SLOW(>=2s): two issues written and one of them patched, a sign-in, and one load of the issue
// with Details opened.
test("a reference in the reason for no components in Details is drawn in the words' ink, not the old application's blue", async ({
  browser,
}) => {
  await phase(
    browser,
    async () => {
      await createProject({ key: "CORE", name: "Core" });
      const cited = await createIssue({ project: "CORE", title: "The staffing plan" });
      const issue = await createIssue({ project: "CORE", title: "Hire someone" });
      await patchIssue(issue.key, {
        components: { mode: "none", reason: `See dispatch://${cited.key}` },
      });
      return { cited: cited.key, key: issue.key };
    },
    async (page, seeded) => {
      await gotoIssue(page, `/issues/${seeded.key}`);
      await openDetails(page);
      await expectLinkInWordsInk(
        detailsRow(page, PROPERTY_LABELS.components).locator(
          `[data-testid="issue-components"] .dispatch-markdown a[href="/issues/${seeded.cited}"]`
        ),
        "the reason for no components"
      );
    }
  );
});

// bob sends the spec of a root issue under the design gate back, for a reason that cites another
// issue: Details' Approval row draws that reason under the spec's step through the renderer
// (`ApprovalSteps`), inside the words' mark, so the link the renderer draws for the reference
// takes the words' own ink rather than the old application's link blue.
// SLOW(>=2s): two issues, the project's gated steps and a review written, a sign-in, and one load
// of the issue with Details opened.
test("a reference in a reviewer's reason for changes requested in Details is drawn in the words' ink, not the old application's blue", async ({
  browser,
}) => {
  await phase(
    browser,
    async () => {
      await createProject({ key: "CORE", name: "Core" });
      await setGatedSteps("CORE", ["spec"]);
      const cited = await createIssue({ project: "CORE", title: "The rollback plan" });
      const issue = await createIssue({
        project: "CORE",
        spec: "The plan.",
        title: "A spec sent back",
      });
      await reviewArtifact(
        issue.primary_artifact_id,
        {
          reason: `Name the rollback path: see dispatch://${cited.key}`,
          state: "changes_requested",
        },
        { login: "bob" }
      );
      return { cited: cited.key, key: issue.key };
    },
    async (page, seeded) => {
      await gotoIssue(page, `/issues/${seeded.key}`);
      await openDetails(page);
      await expectLinkInWordsInk(
        detailsRow(page, PROPERTY_LABELS.approval).locator(
          `[data-approval-step-reason="spec"] .dispatch-markdown a[href="/issues/${seeded.cited}"]`
        ),
        "a reviewer's reason"
      );
    }
  );
});

// ---------------------------------------------------------------------------------------------
// The address that opens the new page for one issue, with the reader's setting off.
// ---------------------------------------------------------------------------------------------

// alice has never turned the new page on, and opens an issue's address that carries
// `?issue_page=new`: the shell draws the new page's frame, its icon rail, and the new page in it.
// A view's control moves through the router, so the view's address keeps the parameter, and a
// reload and Back open the new page again. The same issue's address without the parameter is
// today's page, with its tabs, and Settings' control is still off: the address stored nothing.
// SLOW(>=2s): an issue written, a sign-in, two loads of the issue, a reload, a move back and a load
// of Settings.
test("an issue address with ?issue_page=new opens the new page with the setting off, keeps it through a view, a reload and Back, and stores nothing", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Opened by its address" });
  const opened = `/issues/${issue.key}?issue_page=new`;
  const view = `/issues/${issue.key}/spec?issue_page=new`;
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.setViewportSize(DESKTOP);
    await page.goto(opened);
    await expectIssuePageDrawn(page);
    await expect(iconRail(page), "the new page's frame draws no icon rail").toHaveCount(1);
    await issueRead(page);

    await presetToggle(page, "spec").click();
    await expect(page).toHaveURL(view);
    await expect(presetToggle(page, "spec")).toHaveAttribute("aria-pressed", "true");
    await expect(issuePage(page), "the view's address drew today's page").toHaveCount(1);
    await expect(iconRail(page)).toHaveCount(1);

    await page.reload();
    await expect(page).toHaveURL(view);
    await expect(issuePage(page), "a reload of the view's address drew today's page").toHaveCount(
      1
    );
    await expect(presetToggle(page, "spec")).toHaveAttribute("aria-pressed", "true");
    await expect(iconRail(page)).toHaveCount(1);

    await page.goBack();
    await expect(page).toHaveURL(opened);
    await expectIssuePageDrawn(page);
    await expect(iconRail(page)).toHaveCount(1);

    await page.goto(`/issues/${issue.key}`);
    await expect(
      page.getByRole("tab", { exact: true, name: "Spec" }),
      "the address without the parameter drew no tab of today's page"
    ).toBeVisible();
    await expect(
      issuePage(page),
      "the address without the parameter drew the new page"
    ).toHaveCount(0);
    await expect(iconRail(page)).toHaveCount(0);

    await page.goto("/settings");
    await expect(
      page
        .getByRole("region", { name: "Preview" })
        .getByRole("checkbox", { exact: true, name: "New issue page" })
    ).not.toBeChecked();
    expect(
      await page.evaluate(() => window.localStorage.getItem("dispatch.issue.page:alice")),
      "the address stored a choice"
    ).toBeNull();
  } finally {
    await alice.close();
  }
});

// alice has never turned the new page on, and opens the issue's address with `?issue_page=new`.
// Details' Referenced by draws a comment's excerpt through the renderer, whose link to the issue
// itself is a plain `<a href>` the router never drew, to the address without the parameter. A
// press on it is followed in place by the router, which keeps the parameter: no document is
// loaded, and the new page stays where a load of that address would draw today's page.
// SLOW(>=2s): two issues and a citing comment written, a sign-in, one load of the issue with
// Referenced by opened, and a press on its link.
test("a link in Details' Referenced by to the issue itself, pressed at ?issue_page=new with the setting off, keeps the new page and loads no document", async ({
  browser,
}) => {
  await pressLinkToItself(browser, 1440, "link");
});

// alice has never turned the new page on, and opens the address with `?issue_page=new` of an issue
// whose spec bob sent back, for a reason that cites the issue itself. The Reviews dialog, opened
// from the header's approval chip, draws that reason through the renderer, whose link to the issue
// is a plain `<a href>` to the address without the parameter. The dialog does not close on a move
// of the router, so a move in place would leave it, its scroll lock and its keys over the page:
// the press loads the address with the parameter instead, and the dialog goes with the document.
// SLOW(>=2s): an issue, the project's gated steps and a review written, a sign-in, one load of the
// issue, and a press that loads it again.
test("a link in the Reviews dialog, pressed at ?issue_page=new with the setting off, loads the issue's address with the parameter and leaves no dialog over the new page", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  await setGatedSteps("CORE", ["spec"]);
  const issue = await createIssue({
    project: "CORE",
    spec: "The plan.",
    title: "A spec sent back",
  });
  await reviewArtifact(
    issue.primary_artifact_id,
    { reason: `Name the rollback path: see dispatch://${issue.key}`, state: "changes_requested" },
    { login: "bob" }
  );
  const opened = `/issues/${issue.key}?issue_page=new`;
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.setViewportSize(DESKTOP);
    await gotoIssue(page, opened);
    await approvalChip(page, issue.primary_artifact_id).click();
    const reviews = page.getByRole("dialog", { name: "Reviews" });
    const link = reviews.locator(`.dispatch-markdown a[href="/issues/${issue.key}"]`);
    await expect(link, "the Reviews dialog draws no link to the issue in its reason").toHaveCount(
      1
    );
    await page.evaluate(() => {
      window.__stayed = true;
    });
    await link.click();
    await expect(page, "the press lost the parameter").toHaveURL(opened);
    await expectIssuePageDrawn(page);
    await expect(reviews, "the Reviews dialog stayed over the page").toHaveCount(0);
    // A press that starts a load returns once that load has committed, so this read is of the
    // document the press left the reader in.
    expect(
      await page.evaluate(() => window.__stayed === true),
      "the press loaded no document"
    ).toBe(false);
  } finally {
    await alice.close();
  }
});
