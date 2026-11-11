/**
 * The Inbox reading column's place-keeping (INFRA-2512), as `inbox-pane-scroll.e2e.ts` reads it:
 * the long documents its rows open, the drives that used to take the reader's place away, and
 * the readings of the column that say where the reader stands in it.
 *
 * Every document, project, person and host here is made up: this repository is public, so
 * nothing names a real one.
 */
import { expect, type Page } from "@playwright/test";

import type { InboxRow } from "../web/src/api/types";
import { LIMIT_MS } from "../web/src/features/doc/refocus-when-settled";
import {
  answerAsk,
  createArtifactComment,
  createAsk,
  createComment,
  createIssue,
  createProject,
  createProjectDocument,
  disconnectLiveSockets,
  editArtifact,
  getInbox,
  patchIssue,
  requestApproval,
  withOpenedAsk,
} from "./api";
import { replyRowId } from "./inbox-threads";

/** The session that writes as somebody else: the author of every reply these rows read and of
 *  every edit a drive makes. */
export const session = {
  actor: { id: "e2e-pane-scroll", kind: "session" as const },
  as: "agent" as const,
};

/** The one project every row of this file seeds into. */
const PROJECT = { key: "SCROLL", name: "Scroll rollout" };

/** The address the Inbox writes for a selected row: the item, then the assignee filter cleared,
 *  so a project document's question - which nobody holds - is listed beside the rest. A thread
 *  row's item carries a colon, which the Inbox's own address escapes. */
export function inboxAt(item: string): string {
  return `/?item=${encodeURIComponent(item)}&assignee=everyone`;
}

// ---------------------------------------------------------------------------
// The documents these rows open.
// ---------------------------------------------------------------------------

/** A diagram the editor draws after its text, through the renderer's own worker: a placement
 *  that waits on the diagrams is in what these rows decide. */
const DIAGRAM = [
  "```mermaid",
  "flowchart TD",
  "  ledger[Ledger] --> mirror[Mirror]",
  "  mirror --> ledger",
  "```",
].join("\n");

/** `count` paragraphs of filler, so a document is longer than the column that shows it. */
function notes(prefix: string, count: number): string[] {
  return Array.from(
    { length: count },
    (_, index) =>
      `${prefix} note ${index + 1}: the ledger and the mirror each keep a clock of their own.`
  );
}

/** One markdown body from the blocks it holds. */
function body(blocks: readonly string[]): string {
  return `${blocks.join("\n\n")}\n`;
}

/** A question written as a decision block, with the two options every one of them offers. */
function askBlock(id: string, question: string): string {
  return [
    `:::ask{#${id} urgency="med" multiple="false" state="open"}`,
    question,
    "",
    "- The last hour",
    "- The last day",
    ":::",
  ].join("\n");
}

/** The last paragraph of every long document here: a row reads it to say the document is drawn. */
export const DOCUMENT_FOOT = "Window note 24: the replay budget is three attempts per lease.";
/** The heading the opened block's section starts at. */
export const SECTION_HEADING = "Replay window";
/** The heading another session inserts between that heading and the opened block. */
export const INSERTED_HEADING = "Retry budget";
/** The plain block another session inserts above a question that has no heading above it. */
const INSERTED_NOTE = "Inserted note: the lease moves with the ledger, never the mirror.";
/** What another session writes while the document's own read is refused, which is what asks
 *  for that read again. */
export const FAILING_NOTE = "Failing note: the ledger asked for its clock under a refusal.";
/** The write that brings the refused read back once the route serves it again. */
export const RECOVERY_NOTE = "Recovery note: the ledger wrote its clock again.";
/** One more write, which the place the reader took after the recovery must survive. */
export const SECOND_NOTE = "Second note: the mirror read the clock after the ledger wrote it.";

const INTRO = "The mirror replays what the ledger wrote while it was away.";
const RUNBOOK_HEADING = "Mirror runbook";

// ---------------------------------------------------------------------------
// Reading the Inbox the server has indexed.
// ---------------------------------------------------------------------------

/** The block index lands about 150 ms after the write that asks for it, which falls inside
 *  Playwright's own 100/250/500 ladder: that ladder would not look again until a quarter-second
 *  after the row was already there, and every row of this file pays the seed. */
const INDEX_PROBE = { intervals: [50, 50, 100, 200, 500] };

/** The Inbox row `match` names, once the server has indexed the block that carries it. */
async function indexedRow(match: (row: InboxRow) => boolean): Promise<InboxRow> {
  let found: InboxRow | undefined;
  await expect
    .poll(async () => {
      found = (await getInbox()).find(match);
      return found !== undefined;
    }, INDEX_PROBE)
    .toBe(true);
  if (found === undefined) {
    throw new Error("the seeded question never reached the Inbox");
  }
  return found;
}

/** The issue every row commits its own events on, which no row of this file opens. */
async function otherIssue(): Promise<string> {
  const issue = await createIssue({ force: true, project: PROJECT.key, title: "Lease handover" });
  await patchIssue(issue.key, { priority: 3 });
  return issue.key;
}

// ---------------------------------------------------------------------------
// The approval request the reader reads down.
// ---------------------------------------------------------------------------

const APPROVAL_NAME = "Ledger rollout";
/** A line of the version the column draws, which says the document is still drawn. */
export const APPROVAL_FOOT = "Rollout note 80: the mirror hands the lease back when asked.";

const approvalSpec = body([`# ${APPROVAL_NAME}`, ...notes("Rollout", 79), APPROVAL_FOOT]);

export interface ApprovalPlaceSeed {
  approvalAskId: string;
  /** The issue a drive commits an event on, which is never the open row's own. */
  otherIssueKey: string;
}

/** One approval request, on a document far longer than the column that shows it, with nothing
 *  approved before it: the column marks nothing and reads no baseline. */
export async function seedApprovalPlace(): Promise<ApprovalPlaceSeed> {
  await createProject(PROJECT);
  const issue = await createIssue({
    force: true,
    project: PROJECT.key,
    spec: approvalSpec,
    title: APPROVAL_NAME,
  });
  await patchIssue(issue.key, { priority: 2 });
  const other = await otherIssue();
  const requested = withOpenedAsk(
    await requestApproval(
      issue.primary_artifact_id,
      { summary: "The ledger-first rollout." },
      session
    )
  );
  expect(requested.version).toBe(1);
  return { approvalAskId: requested.ask.id, otherIssueKey: other };
}

// ---------------------------------------------------------------------------
// The question and thread columns the reader moves.
// ---------------------------------------------------------------------------

/** Which of the three the row opens: a question under a heading, a question with no heading
 *  above it, or a comment thread anchored under a heading of its own. */
export type QuestionRun = "heading" | "plain" | "thread";

export const QUESTION_BLOCK = "replay-window";
export const QUESTION = "Which window does the mirror replay?";
export const PLAIN_BLOCK = "lease-window";
const PLAIN_QUESTION = "Should the mirror keep retrying after a refused lease?";
export const ANCHORED_LINE = "The mirror replays the ledger on a cold start, window by window.";
const ANCHOR_QUOTE = "replays the ledger";
const THREAD_ROOT = "How far back does a cold start replay?";
export const THREAD_REPLY = "One hour, then it gives up.";
/** The reply another session writes while the reader is far below the thread, which leaves the
 *  thread unread. It is long enough to stand at the top of the column on its own. */
export const LATE_REPLY = Array.from(
  { length: 14 },
  (_, index) => `Cold start line ${index + 1}: the replay reads one window and then the next.`
).join("\n\n");
/** The first line of that reply, which a row reads it by. */
export const LATE_REPLY_FIRST_LINE =
  "Cold start line 1: the replay reads one window and then the next.";

const headingDocument = body([
  `# ${RUNBOOK_HEADING}`,
  INTRO,
  DIAGRAM,
  ...notes("Runbook", 40),
  `## ${SECTION_HEADING}`,
  ...notes("Section", 6),
  askBlock(QUESTION_BLOCK, QUESTION),
  ...notes("Window", 23),
  DOCUMENT_FOOT,
]);

const plainDocument = body([
  INTRO,
  DIAGRAM,
  ...notes("Runbook", 40),
  askBlock(PLAIN_BLOCK, PLAIN_QUESTION),
  ...notes("Window", 23),
  DOCUMENT_FOOT,
]);

const threadDocument = body([
  `# ${RUNBOOK_HEADING}`,
  INTRO,
  DIAGRAM,
  ...notes("Runbook", 40),
  `## ${SECTION_HEADING}`,
  ANCHORED_LINE,
  ...notes("Window", 23),
  DOCUMENT_FOOT,
]);

export interface QuestionColumnSeed {
  artifactId: string;
  /** The block the row is opened at: the question's own, or the thread's anchored one. */
  blockId: string;
  /** What another session inserts above that block in the first drive. */
  insertedMarkdown: string;
  /** The words that insert puts on the page, which the row waits for. */
  insertedText: string;
  /** The text the insert goes above, as the edit route anchors one. */
  insertedBefore: string;
  /** The `?item=` the reader opens. */
  item: string;
  otherIssueKey: string;
  /** The thread's root, for the run that opens a thread. */
  threadId?: string;
}

/** The document one run of the question rows opens, with the question or the thread it is
 *  opened at. */
export async function seedQuestionColumn(run: QuestionRun): Promise<QuestionColumnSeed> {
  await createProject(PROJECT);
  const other = await otherIssue();

  if (run === "thread") {
    const created = await createProjectDocument(PROJECT.key, {
      content: threadDocument,
      name: "cold-start-runbook.md",
    });
    const root = await createArtifactComment(created.artifact.id, {
      anchor: { artifact: created.artifact.slug, quote: ANCHOR_QUOTE },
      body: THREAD_ROOT,
    });
    const blockId = root.anchor?.block_id;
    if (blockId === undefined || blockId === null) {
      throw new Error("the seeded thread was anchored to no block");
    }
    await createArtifactComment(
      created.artifact.id,
      { body: THREAD_REPLY, reply_to: root.id },
      session
    );
    return {
      artifactId: created.artifact.id,
      blockId,
      insertedBefore: ANCHORED_LINE,
      insertedMarkdown: `### ${INSERTED_HEADING}\n`,
      insertedText: INSERTED_HEADING,
      item: replyRowId(root.id),
      otherIssueKey: other,
      threadId: root.id,
    };
  }

  const underHeading = run === "heading";
  const created = await createProjectDocument(PROJECT.key, {
    content: underHeading ? headingDocument : plainDocument,
    name: underHeading ? "mirror-runbook.md" : "lease-runbook.md",
  });
  const blockId = underHeading ? QUESTION_BLOCK : PLAIN_BLOCK;
  const row = await indexedRow((indexed) => indexed.block_id === blockId);
  return {
    artifactId: created.artifact.id,
    blockId,
    insertedBefore: `block:${blockId}`,
    insertedMarkdown: underHeading ? `### ${INSERTED_HEADING}\n` : `${INSERTED_NOTE}\n`,
    insertedText: underHeading ? INSERTED_HEADING : INSERTED_NOTE,
    item: row.id,
    otherIssueKey: other,
  };
}

// ---------------------------------------------------------------------------
// The three questions of one document the opening rows step through.
// ---------------------------------------------------------------------------

export const SECOND_HEADING = "Retry budget";
export const SECOND_BLOCK = "retry-budget";
export const SECOND_QUESTION = "How many attempts does one lease get?";

/** One document with three questions: one before any heading at all, and two far down, each
 *  under a heading of its own. */
const questionsDocument = body([
  INTRO,
  askBlock(PLAIN_BLOCK, PLAIN_QUESTION),
  DIAGRAM,
  ...notes("Runbook", 40),
  `## ${SECTION_HEADING}`,
  ...notes("Section", 6),
  askBlock(QUESTION_BLOCK, QUESTION),
  ...notes("Window", 20),
  `## ${SECOND_HEADING}`,
  ...notes("Budget", 6),
  askBlock(SECOND_BLOCK, SECOND_QUESTION),
  ...notes("Window", 23),
  DOCUMENT_FOOT,
]);

export interface QuestionsSeed {
  /** The question under `SECTION_HEADING`, far down the document. */
  firstAskId: string;
  /** The question before any heading, which has no section at all. */
  plainAskId: string;
  /** The question under `SECOND_HEADING`, further down still. */
  secondAskId: string;
}

export async function seedQuestions(): Promise<QuestionsSeed> {
  await createProject(PROJECT);
  await createProjectDocument(PROJECT.key, {
    content: questionsDocument,
    name: "mirror-runbook.md",
  });
  const [first, plain, second] = await Promise.all([
    indexedRow((row) => row.block_id === QUESTION_BLOCK),
    indexedRow((row) => row.block_id === PLAIN_BLOCK),
    indexedRow((row) => row.block_id === SECOND_BLOCK),
  ]);
  return { firstAskId: first.id, plainAskId: plain.id, secondAskId: second.id };
}

// ---------------------------------------------------------------------------
// Two threads anchored to one block.
// ---------------------------------------------------------------------------

const SHARED_LINE =
  "The mirror replays the ledger on a cold start, and hands the lease back when it is done.";
const FIRST_QUOTE = "replays the ledger";
const SECOND_QUOTE = "hands the lease back";
const FIRST_THREAD_ROOT = "How far back does a cold start replay?";
export const SECOND_THREAD_ROOT = "Who holds the lease while the replay runs?";

const twoThreadsDocument = body([
  `# ${RUNBOOK_HEADING}`,
  INTRO,
  DIAGRAM,
  ...notes("Runbook", 40),
  `## ${SECTION_HEADING}`,
  SHARED_LINE,
  ...notes("Window", 23),
  DOCUMENT_FOOT,
]);

export interface TwoThreadsSeed {
  /** The block both threads are anchored to. */
  blockId: string;
  firstItem: string;
  secondItem: string;
}

/** Two comment threads anchored to two quotes of one paragraph, so both carry one block id. */
export async function seedTwoThreads(): Promise<TwoThreadsSeed> {
  await createProject(PROJECT);
  const created = await createProjectDocument(PROJECT.key, {
    content: twoThreadsDocument,
    name: "mirror-runbook.md",
  });
  const first = await createArtifactComment(created.artifact.id, {
    anchor: { artifact: created.artifact.slug, quote: FIRST_QUOTE },
    body: FIRST_THREAD_ROOT,
  });
  const second = await createArtifactComment(created.artifact.id, {
    anchor: { artifact: created.artifact.slug, quote: SECOND_QUOTE },
    body: SECOND_THREAD_ROOT,
  });
  const blockId = first.anchor?.block_id;
  expect(second.anchor?.block_id).toBe(blockId);
  if (blockId === undefined || blockId === null) {
    throw new Error("the seeded threads were anchored to no block");
  }
  for (const root of [first, second]) {
    await createArtifactComment(
      created.artifact.id,
      { body: THREAD_REPLY, reply_to: root.id },
      session
    );
  }
  return { blockId, firstItem: replyRowId(first.id), secondItem: replyRowId(second.id) };
}

// ---------------------------------------------------------------------------
// An approval request whose version marks what changed.
// ---------------------------------------------------------------------------

/** What the first paragraph the newer version changed opens with. The drawn block interleaves
 *  the words that version removed with the words it added, so a row names the lead both
 *  versions share rather than either wording. */
export const CHANGED_STEP = "Step 1:";
const CHANGED_STEPS = 6;
const steps = Array.from({ length: CHANGED_STEPS }, (_, index) => ({
  after: `Step ${index + 1}: the mirror reads the clock before the ledger writes it.`,
  before: `Step ${index + 1}: the ledger writes the clock before the mirror reads it.`,
}));

// The steps stand in the middle: a column cannot put the first of them at its top unless the
// document goes on below them.
const baselineSpec = body([
  "# Ledger rollout",
  ...notes("Rollout", 40),
  ...steps.map((step) => step.before),
  ...notes("Window", 30),
]);

export interface ApprovalMarksSeed {
  approvalAskId: string;
  /** How many blocks the version the column draws marks as changed. */
  changedBlocks: number;
}

/** A document approved at version 1, whose version 2 changed six paragraphs far down it. */
export async function seedApprovalWithMarks(): Promise<ApprovalMarksSeed> {
  await createProject(PROJECT);
  const issue = await createIssue({
    force: true,
    project: PROJECT.key,
    spec: baselineSpec,
    title: "Ledger rollout",
  });
  await patchIssue(issue.key, { priority: 1 });
  const artifactId = issue.primary_artifact_id;
  const atOne = withOpenedAsk(
    await requestApproval(artifactId, { summary: "The ledger-first rollout." }, session)
  );
  expect(atOne.version).toBe(1);
  await answerAsk(
    atOne.ask.id,
    { expected_edited_at: null, selected: ["Approve"] },
    { login: "alice" }
  );
  await editArtifact(
    artifactId,
    {
      ops: steps.map((step) => ({ find: step.before, op: "replace" as const, with: step.after })),
      summary: "The mirror-first rollout",
    },
    session
  );
  const atTwo = withOpenedAsk(
    await requestApproval(artifactId, { summary: "The mirror-first rollout." }, session)
  );
  expect(atTwo.version).toBe(2);
  return { approvalAskId: atTwo.ask.id, changedBlocks: CHANGED_STEPS };
}

// ---------------------------------------------------------------------------
// Two approval requests the reader steps between.
// ---------------------------------------------------------------------------

const FIRST_APPROVAL_SUMMARY = "The ledger-first rollout.";
export const SECOND_APPROVAL_SUMMARY = "The lease handover draft.";

export interface TwoApprovalsSeed {
  firstAskId: string;
  secondAskId: string;
}

/**
 * Two approval requests, neither of the two documents approved before.
 *
 * One document cannot carry two of them: the route renews the open request rather than opening a
 * second (`OpenApprovalAskForDocuments`). So the two stand on two documents, which is the rule's
 * other half - with neither approved, neither request waits on a baseline read, so stepping from
 * one to the other draws no baseline skeleton of its own.
 */
export async function seedTwoApprovals(): Promise<TwoApprovalsSeed> {
  await createProject(PROJECT);
  const first = await createIssue({
    force: true,
    project: PROJECT.key,
    spec: body([`# ${APPROVAL_NAME}`, ...notes("Rollout", 79), APPROVAL_FOOT]),
    title: APPROVAL_NAME,
  });
  await patchIssue(first.key, { priority: 1 });
  const second = await createIssue({
    force: true,
    project: PROJECT.key,
    spec: body(["# Lease handover", ...notes("Handover", 60)]),
    title: "Lease handover",
  });
  await patchIssue(second.key, { priority: 2 });
  const [requestedFirst, requestedSecond] = await Promise.all([
    requestApproval(first.primary_artifact_id, { summary: FIRST_APPROVAL_SUMMARY }, session),
    requestApproval(second.primary_artifact_id, { summary: SECOND_APPROVAL_SUMMARY }, session),
  ]);
  return {
    firstAskId: withOpenedAsk(requestedFirst).ask.id,
    secondAskId: withOpenedAsk(requestedSecond).ask.id,
  };
}

// ---------------------------------------------------------------------------
// The drives.
// ---------------------------------------------------------------------------

const ADDED_QUESTION = "Which service hands the lease over first?";

/** A question on a new issue at the top priority, which the Inbox bands above every row these
 *  rows open. Answers the row's own id. */
export async function addRowAbove(): Promise<string> {
  const issue = await createIssue({ force: true, project: PROJECT.key, title: "Refused lease" });
  await patchIssue(issue.key, { priority: 0 });
  const ask = await createAsk(
    issue.key,
    { options: [{ label: "Yes" }, { label: "No" }], question: ADDED_QUESTION },
    session
  );
  return ask.id;
}

/** One event on an issue no row of this file opens: a comment nobody is waiting on, which still
 *  moves the workspace's event head. */
export async function commitEventElsewhere(issueKey: string, note: string): Promise<void> {
  await createComment(issueKey, { body: note }, session);
}

/** How many events the page's live socket has carried it. */
export function watchLiveEvents(page: Page): () => number {
  let seen = 0;
  page.on("websocket", (socket) => {
    if (new URL(socket.url()).pathname !== "/ws/live") return;
    socket.on("framereceived", ({ payload }) => {
      const frame = JSON.parse(String(payload)) as { readonly type?: string };
      if (frame.type === "event") seen += 1;
    });
  });
  return () => seen;
}

/** How many times the page has read the signed-in person. */
export function watchWhoAmI(page: Page): () => number {
  let reads = 0;
  page.on("response", (response) => {
    if (new URL(response.url()).pathname === "/auth/whoami") reads += 1;
  });
  return () => reads;
}

export interface LiveGap {
  /** Closes every live socket and lets the page resume above the log's head, so its event feed
   *  cannot replay what it missed and refreshes the whole cache instead. */
  readonly open: () => Promise<void>;
  /** Whether each feed start the page was sent said the replay covered the gap. */
  readonly starts: () => readonly boolean[];
}

/** Watches the page's live socket, and holds the one switch that makes its next resume a gap.
 *  Installed before the page is opened, as every route is. */
export async function watchLiveGap(page: Page): Promise<LiveGap> {
  let force = false;
  let opens = 0;
  await page.routeWebSocket(/\/ws\/live$/u, (route) => {
    opens += 1;
    const rewrite = force && opens > 1;
    const server = route.connectToServer();
    route.onMessage((message) => {
      const frame = JSON.parse(String(message)) as {
        readonly type?: string;
        readonly view?: { readonly kind?: string };
        readonly resume?: { readonly since?: string };
      };
      if (
        rewrite &&
        frame.type === "subscribe" &&
        frame.view?.kind === "events" &&
        frame.resume?.since !== undefined
      ) {
        server.send(JSON.stringify({ ...frame, resume: { since: "999999999999" } }));
        return;
      }
      server.send(message);
    });
  });
  const starts: boolean[] = [];
  page.on("websocket", (socket) => {
    if (new URL(socket.url()).pathname !== "/ws/live") return;
    socket.on("framereceived", ({ payload }) => {
      const frame = JSON.parse(String(payload)) as {
        readonly type?: string;
        readonly replay_complete?: boolean;
      };
      if (frame.type === "events_start") starts.push(frame.replay_complete === true);
    });
  });
  return {
    open: async () => {
      force = true;
      await disconnectLiveSockets();
    },
    starts: () => starts,
  };
}

/** How far past the reads' staleness the window is left before it is given the focus again
 *  (`staleTime` is 30 s in `web/src/main.tsx`). */
const PAST_STALENESS_MS = 31_000;

/**
 * Takes the window out of focus and gives it back past the staleness, which reads the
 * signed-in person again. The page's own clock is moved ahead rather than waited out.
 *
 * It is moved, not stopped: the refresh scheduler holds a query's next fetch until a second has
 * passed since its last one (`MIN_REFETCH_INTERVAL_MS`), and a clock that answers one time for
 * ever would hold every later refresh of this page at that second, so no drive after this one
 * would reach the page at all.
 */
export async function focusAfterStaleness(page: Page): Promise<void> {
  await page.evaluate((ahead) => {
    const reading = Date.now.bind(Date);
    Date.now = () => reading() + ahead;
    const visibility = (state: "hidden" | "visible") => {
      Object.defineProperty(document, "visibilityState", { configurable: true, value: state });
      document.dispatchEvent(new Event("visibilitychange", { bubbles: true }));
    };
    visibility("hidden");
    visibility("visible");
    window.dispatchEvent(new Event("focus"));
  }, PAST_STALENESS_MS);
}

export interface DocumentTextRead {
  /** Answers the document's text read `409 DOCUMENT_UNLOADABLE`, which the page does not retry. */
  readonly refuse: () => Promise<void>;
  /** Serves it from the server again. */
  readonly serve: () => Promise<void>;
}

/** The page's read of one document's text, which a row can make fail and then succeed again. */
export function documentTextRead(page: Page, artifactId: string): DocumentTextRead {
  const read = `**/api/v1/artifacts/${artifactId}/text`;
  return {
    refuse: async () => {
      await page.route(read, (route) =>
        route.fulfill({
          body: JSON.stringify({
            code: "DOCUMENT_UNLOADABLE",
            error: "the stored history cannot load",
          }),
          contentType: "application/json",
          status: 409,
        })
      );
    },
    serve: () => page.unroute(read),
  };
}

export interface HeldReplies {
  /** Serves every held read of the thread's replies, and every read after it. */
  readonly release: () => Promise<void>;
}

/** Holds the read that serves a document's comment threads, so the thread under the opened
 *  block is drawn only once the row lets it be. */
export async function holdThreadReplies(page: Page, artifactId: string): Promise<HeldReplies> {
  const read = `**/api/v1/artifacts/${artifactId}/comments`;
  const held = Promise.withResolvers<void>();
  await page.route(read, async (route) => {
    await held.promise;
    await route.continue();
  });
  return {
    release: async () => {
      held.resolve();
      await page.unroute(read);
    },
  };
}

// ---------------------------------------------------------------------------
// Reading the column.
// ---------------------------------------------------------------------------

/** Waits the placement's own limit out, with a little over. Nothing on the page says the bound
 *  has passed, so a row that must read the column after it waits rather than polls, and the page
 *  gets no attribute of its own for a test to read. */
export async function waitOutPlacement(page: Page): Promise<void> {
  await page.waitForTimeout(LIMIT_MS + 500);
}

export interface ColumnPlace {
  /** The id of the block standing at the top of the column; null where none is drawn. */
  readonly block: string | null;
  /** The column's own scroll position. */
  readonly top: number;
}

/**
 * Where the column stands once it has stood still: the block at its top and its position, read
 * every frame until ten frames running say the same.
 *
 * A column an editor rebuilt under the reader empties in the frame it is rebuilt in, and a
 * placement writes it a frame or two after the drawing pass that woke it, so a reading taken the
 * moment a drive lands could be read before that drive had done anything.
 */
export function restingColumn(page: Page): Promise<ColumnPlace> {
  return page.evaluate(() => {
    const column = document.querySelector<HTMLElement>('[data-testid="inbox-pane-scroller"]');
    if (column === null) {
      throw new Error("the reading column has no scroller");
    }
    const read = (): { block: string | null; top: number } => {
      const edge = column.getBoundingClientRect().top;
      for (const block of column.querySelectorAll<HTMLElement>("[data-block-id]")) {
        if (block.getBoundingClientRect().bottom > edge + 2) {
          return { block: block.dataset.blockId ?? null, top: column.scrollTop };
        }
      }
      return { block: null, top: column.scrollTop };
    };
    const settled = Promise.withResolvers<{ block: string | null; top: number }>();
    let last = read();
    let still = 0;
    const again = () => {
      const now = read();
      still = now.block === last.block && now.top === last.top ? still + 1 : 0;
      last = now;
      if (still >= 10) {
        settled.resolve(now);
        return;
      }
      requestAnimationFrame(again);
    };
    requestAnimationFrame(again);
    return settled.promise;
  });
}

/** Moves the column by writing its position, which is how a test drives a scroll the pane's four
 *  reader actions do not carry. */
export async function moveColumnTo(page: Page, top: number): Promise<void> {
  await page.evaluate((to) => {
    const column = document.querySelector<HTMLElement>('[data-testid="inbox-pane-scroller"]');
    if (column === null) {
      throw new Error("the reading column has no scroller");
    }
    column.scrollTop = to;
  }, top);
}

/** The same, measured from a block: the content `below` pixels under the foot of that block
 *  stands at the top, so the reader ends past the block however tall the block is drawn. */
export async function moveColumnBelow(page: Page, blockId: string, below: number): Promise<void> {
  await page.evaluate(
    ({ by, id }) => {
      const column = document.querySelector<HTMLElement>('[data-testid="inbox-pane-scroller"]');
      if (column === null) {
        throw new Error("the reading column has no scroller");
      }
      const block = column.querySelector<HTMLElement>(`[data-block-id="${CSS.escape(id)}"]`);
      if (block === null) {
        throw new Error(`the column has not drawn the block ${id}`);
      }
      const edge = column.getBoundingClientRect().top;
      column.scrollTop += block.getBoundingClientRect().bottom - edge + by;
    },
    { by: below, id: blockId }
  );
}

/** Where a block of the drawn document stands, in pixels below the top of the column. */
export function blockOffset(page: Page, blockId: string): Promise<number> {
  return page.evaluate((id) => {
    const column = document.querySelector<HTMLElement>('[data-testid="inbox-pane-scroller"]');
    const block = column?.querySelector<HTMLElement>(`[data-block-id="${CSS.escape(id)}"]`);
    if (column === null || block === null || block === undefined) {
      throw new Error(`the column has not drawn the block ${id}`);
    }
    return block.getBoundingClientRect().top - column.getBoundingClientRect().top;
  }, blockId);
}

/** Where the drawn heading reading `text` stands, in pixels below the top of the column. */
export function headingOffset(page: Page, text: string): Promise<number> {
  return page.evaluate((wanted) => {
    const column = document.querySelector<HTMLElement>('[data-testid="inbox-pane-scroller"]');
    if (column === null) {
      throw new Error("the reading column has no scroller");
    }
    const headings = [...column.querySelectorAll<HTMLElement>("h1, h2, h3, h4, h5, h6")];
    const found = headings.find((node) => (node.textContent ?? "").trim() === wanted);
    if (found === undefined) {
      throw new Error(`the column has not drawn the heading ${wanted}`);
    }
    return found.getBoundingClientRect().top - column.getBoundingClientRect().top;
  }, text);
}

/** Where the first element matching `selector` stands, in pixels below the top of the column. */
export function offsetInColumn(page: Page, selector: string): Promise<number> {
  return page.evaluate((css) => {
    const column = document.querySelector<HTMLElement>('[data-testid="inbox-pane-scroller"]');
    const found = column?.querySelector<HTMLElement>(css);
    if (column === null || found === null || found === undefined) {
      throw new Error(`the column has drawn nothing matching ${css}`);
    }
    return found.getBoundingClientRect().top - column.getBoundingClientRect().top;
  }, selector);
}

/** Where the section bar starts in the column's own scrolled content, which is the offset the
 *  pane measured rather than the one the reader's scroll leaves. */
export function sectionBarTop(page: Page): Promise<number> {
  return page.evaluate(() => {
    const bar = document.querySelector<HTMLElement>('[data-testid="pane-section-bar"]');
    if (bar === null) {
      throw new Error("the column has drawn no section bar");
    }
    return Number.parseFloat(bar.style.top);
  });
}
