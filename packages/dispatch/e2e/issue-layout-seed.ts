import { eventItemId, pinnedItemMarker } from "../web/src/features/issue/pins";
import {
  answerAsk,
  claimIssue,
  createAsk,
  createComment,
  createIssue,
  createIssueArtifact,
  createMessage,
  createPriority,
  createProject,
  declineAsk,
  editArtifact,
  getIssue,
  getIssueDecisions,
  getIssueEvents,
  keepDecision,
  patchIssue,
  putIssueState,
  replyToMessageBlock,
  resolveAsk,
  uploadIssueFile,
} from "./api";
import { seedFakeGithub } from "./fake-github-helpers";
import {
  asPlanner,
  awaitBlockAsk,
  awaitSeededCheckStates,
  BASELINE_REPO,
  PLANNER,
  PROJECT,
  REVIEWER,
  seedIssueWithDocuments,
  seedSessions,
  WHOLE_REPO,
} from "./issue-layout-seed-support";
import {
  CITATION_BLOCK_ID,
  checkBlockId,
  citationRequirement,
  OPEN_QUESTION_BLOCK_ID,
  PLAN_ORDER_TASKS,
  planOrderDocument,
  planTaskBlockId,
  requirementBlockId,
  requirementText,
  SPINE_ANSWERED_QUESTION,
  SPINE_CHECK_BLOCK_ID,
  SPINE_DECISION_BLOCK_ID,
  SPINE_REQUIREMENTS,
  SPINE_SPEC,
  SUMMARY_BLOCK_ID,
  specBlockRef,
  specDocument,
  spineDecisionsPart,
  spineVerificationDocument,
} from "./issue-layout-seed-text";
import { ONE_PIXEL_PNG } from "./one-pixel-png";

/**
 * The whole issues the browser rows of the source's `e2e/issue-layout.e2e.ts` read, and in this
 * tree those of `e2e/issue-page-i.e2e.ts`, for the redesigned issue page. A caller needs
 * `resetDatabase()` and one of these calls in `beforeEach` and nothing else: each seed makes its
 * own project, component model, repository and sessions.
 *
 * Every write goes through `e2e/api.ts`, as a person for a human's write and under the agent
 * bearer for a session's, with two exceptions that are not Dispatch's own state: the component
 * model and the pull request come from the fake GitHub (`e2e/fake-github-helpers.ts`), and a
 * subscriber is a fake Envoy interest (`e2e/agents.ts`), because Dispatch stores none and its one
 * subscriber write is a DELETE (`packages/envoy/internal/dispatch/api/routes_table.go:364-365`).
 * `resetDatabase()` clears both fakes. Each seed returns the issue key with a name for every
 * block id its tests read, so no test derives an id from rendered text.
 *
 * ## What today's model cannot hold, and what is seeded instead
 *
 * - **The summary node.** `blocks.json` is at version 3 and declares five types, `callout`,
 *   `ask`, `requirement`, `plan_task` and `check`, and no summary node, no author of the three
 *   sentences and no `last_read_back_at`, none of which the schema holds yet
 *   (`packages/envoy/internal/dispatch/pmdoc/schema/blocks.json:1-118`, read 2026-11-05). The
 *   three sentences are seeded as a `callout` with the block id `summary`.
 * - **`from` on a requirement.** `requirement` carries no attributes at all
 *   (`blocks.json:70-75`), so there is no `from` edge to seed and *every* seeded requirement's
 *   `from` is absent until the schema adds it. For the disagreement `c-spine` names,
 *   `seedSpineCases` seeds a decision whose block links one requirement while the answered
 *   question a reader would read as that requirement's origin is another node.
 * - **A decision.** A decision is the planner's `decision` block of the spec's `## Decisions`
 *   part, a node of the store, which carries its facts as attributes: the blocks it shaped
 *   (`shaped`), the ask it settles (`from_ask`), how unsure it is (`uncertainty`) and whether a
 *   later one replaced it (`replaced`). Dispatch refuses a document of decisions (409
 *   `DECISIONS_ARE_NODES`), and the decisions read answers each decision as an entry (`entries`)
 *   of its node (`packages/envoy/internal/dispatch/api/decisions.go`). A decision's words still
 *   name the blocks it shaped and the ask it settles, as a reader would write them.
 * - **Depth below a document.** `document_blocks` holds no parent and no ordinal
 *   (`packages/envoy/internal/dispatch/store/migrations/0121_block_graph.up.sql:16-27`, read
 *   2026-11-05), and a document is no node of the tree, so a typed block sits directly under the
 *   issue unless its own `serves` places it, and the deepest path a seed reaches is a turn's
 *   replies or a requirement's check and that check's result.
 * - **The five result states this seed can reach, on one issue.** A before-merge result stands
 *   at another commit when its commit is not a stored pull-request head and the check's issue
 *   resolves to a repository Dispatch can read, and is not known only when it cannot read one
 *   (`packages/envoy/internal/dispatch/api/results_comparison.go`'s `commitStands`, read
 *   2026-11-12). So `seedWholeIssue` records its results while the fake GitHub serves the
 *   repository and the pull request and then seeds the fake with no repository: the head results
 *   go on standing at that head, the reworded check's result speaks for words that have moved
 *   and reads `older`, and the one at a commit no pull request names is not known, so no pass of
 *   its check stands and the page draws the `unmarked` state of `NODE_STATE_WORDS.check` as
 *   `Cannot tell`.
 *   `seedRequestBaseline` keeps the repository installed, because its three GitHub reads are
 *   part of the measured baseline, so only four of the five states appear there.
 * - **The reply that answers a reply.** `threadHeadOf` in
 *   `packages/envoy/internal/dispatch/api/comment_create.go` climbs a comment's `reply_to` to
 *   the thread head before storing, so the entry a comment answered is `comments.answers`
 *   (migration 20261105014808) and never `reply_to`. `seedReplyNesting` writes that thread
 *   through the route a person writes through, so the value the page reads is the server's own.
 * - **A check's `serves` read back.** `GET /api/v1/issues/{key}/verification` is pull request
 *   2754 and not in this build, so `serves` is seeded on the block and read only as coverage.
 */

export interface WholeIssueSeed {
  /** The answered question the second decision settles (`from_ask`), which places that decision
   *  under it. */
  readonly answeredAskId: string;
  /** The checks of the Verification document, by the state each one is seeded in; `all` is
   *  every check, and `uncontrolled` is a pass whose result records no negative control. */
  readonly checks: {
    readonly all: readonly string[];
    readonly failed: string;
    readonly noResult: string;
    readonly older: string;
    readonly passed: string;
    readonly uncontrolled: string;
    readonly unmarked: string;
  };
  readonly children: { readonly blocked: string; readonly done: string; readonly keys: string[] };
  readonly comments: {
    /** The comment anchored to the spec block `anchoredBlock`. */
    readonly anchored: string;
    readonly anchoredBlock: string;
    readonly replies: readonly string[];
    readonly threads: readonly string[];
  };
  /** The block id of each decision of the spec's `## Decisions` part, in document order; the
   *  last one is replaced. */
  readonly decisions: readonly string[];
  readonly documents: {
    /** The slug of the document a spec block cites, and that block. */
    readonly cited: string;
    readonly citingBlock: string;
    readonly image: string;
    readonly uncited: string;
  };
  readonly key: string;
  readonly messages: {
    readonly reply: string;
    /** The reply that answers a block of `reply`, which is a reply itself. */
    readonly replyToReply: string;
    readonly root: string;
  };
  /** The ask the spec's `ask` block was indexed into, two levels below the issue. */
  readonly openQuestion: { readonly askId: string; readonly blockId: string };
  readonly parentKey: string;
  readonly pinnedEventId: number;
  readonly planArtifactId: string;
  readonly planTasks: readonly string[];
  readonly requirements: readonly string[];
  readonly specArtifactId: string;
  readonly summaryBlockId: string;
  readonly verificationArtifactId: string;
}

/** The whole issue the page's verification document describes, in one call. */
export async function seedWholeIssue(): Promise<WholeIssueSeed> {
  const seeded = await seedIssueWithDocuments({
    checks: 6,
    children: 7,
    commentReplies: 3,
    commentThreads: 2,
    decisions: 7,
    planTasks: 9,
    repository: WHOLE_REPO,
    requirements: 12,
    title: "The issue page draws one ordered body",
  });

  const cited = await createIssueArtifact(seeded.key, {
    content: "# Notes\n\nThe measurements behind the first requirement.\n",
    name: "notes.md",
  });
  const uncited = await createIssueArtifact(seeded.key, {
    content: "# Background\n\nNothing cites this document.\n",
    name: "background.md",
  });
  const image = await uploadIssueFile(seeded.key, {
    bytes: new Uint8Array(Buffer.from(ONE_PIXEL_PNG, "base64")),
    contentType: "image/png",
    name: "shot.png",
  });
  await editArtifact(seeded.specArtifactId, {
    ops: [
      {
        after: `block:${requirementBlockId(1)}`,
        markdown: citationRequirement(seeded.key, cited.artifact.slug),
        op: "insert",
      },
    ],
  });

  const anchored = await createComment(seeded.key, {
    anchor: { artifact: "spec", quote: requirementText(2) },
    body: "This requirement is the one the tree reads first.",
  });
  const root = await createMessage(
    seeded.key,
    { body: "The tree is folded to the frontier.\n\nThe fold budget counts no question." },
    asPlanner
  );
  const reply = await replyToMessageBlock(seeded.key, root.id, "b2", {
    body: "Which is why the deepest question is still on the first screen.",
  });
  const replyToReply = await replyToMessageBlock(seeded.key, reply.id, "b1", {
    body: "Read that against the row count with the question left out.",
  });

  const events = await getIssueEvents(seeded.key, { limit: 1 });
  const pinned = events[0];
  if (pinned === undefined) throw new Error(`${seeded.key} recorded no event to pin`);
  await putIssueState(seeded.key, {
    dismissed: [pinnedItemMarker(eventItemId(pinned))],
    pinned: true,
    seq: 1,
  });

  // With the App installed on nothing, the result at a commit no pull request names is the one
  // the page draws as `Cannot tell`: see this module's doc comment on the five result states.
  await seedFakeGithub({});
  await awaitSeededCheckStates(seeded.key, 6, false);

  return {
    answeredAskId: seeded.answeredAskId,
    checks: {
      all: seeded.checks,
      failed: checkBlockId(2),
      noResult: checkBlockId(3),
      older: checkBlockId(4),
      passed: checkBlockId(1),
      uncontrolled: checkBlockId(6),
      unmarked: checkBlockId(5),
    },
    children: seeded.children,
    comments: {
      anchored: anchored.id,
      anchoredBlock: requirementBlockId(2),
      replies: seeded.commentReplies,
      threads: seeded.commentThreads,
    },
    decisions: seeded.decisions,
    documents: {
      cited: cited.artifact.slug,
      citingBlock: CITATION_BLOCK_ID,
      image: image.artifact.slug,
      uncited: uncited.artifact.slug,
    },
    key: seeded.key,
    messages: { reply: reply.id, replyToReply: replyToReply.id, root: root.id },
    openQuestion: { askId: seeded.openQuestionAskId, blockId: OPEN_QUESTION_BLOCK_ID },
    parentKey: seeded.parentKey,
    pinnedEventId: pinned.id,
    planArtifactId: seeded.planArtifactId,
    planTasks: Array.from({ length: 9 }, (_, index) => planTaskBlockId(index + 1)),
    requirements: Array.from({ length: 12 }, (_, index) => requirementBlockId(index + 1)),
    specArtifactId: seeded.specArtifactId,
    summaryBlockId: SUMMARY_BLOCK_ID,
    verificationArtifactId: seeded.verificationArtifactId,
  };
}

export interface AdvancingIssueSeed extends WholeIssueSeed {
  /** The ids of the two priorities the issue is under, in the order the priorities page ranks
   *  them: `own` is the issue's own relation, `inherited` its parent's. */
  readonly priorities: { readonly inherited: string; readonly own: string };
}

/**
 * `seedWholeIssue`, with the issue under two priorities: one it advances itself, which its
 * priority row draws as a link with a remove beside it, and one its parent advances, which the
 * row draws as a link marked inherited and with no remove. That is the whole priority row an
 * issue can draw short of its cap, so in the source it is what the page inventory is recorded
 * over (`record-page-inventory.e2e.ts`) and what c-nothing-lost reads the new page against; both
 * call this one seed there, so the two read the same row. No row of this tree calls it yet.
 *
 * It is a seed of its own rather than part of `seedWholeIssue`, because each relation makes the
 * page read the priorities on its first load, and every other row of the source's
 * `issue-layout.e2e.ts` reads that issue with the requests and the properties it has without
 * them.
 */
export async function seedWholeIssueAdvancing(): Promise<AdvancingIssueSeed> {
  const seeded = await seedWholeIssue();
  // One after the other, because a priority takes the last place of the one order as it is
  // written (`POST /api/v1/priorities`): the issue's own is P1 and its parent's P2.
  const own = await createPriority({ owner: "alice", title: "Ship the redrawn issue page" });
  const inherited = await createPriority({ owner: "alice", title: "Keep every old address" });
  await patchIssue(seeded.key, { advances: [own.id] });
  await patchIssue(seeded.parentKey, { advances: [inherited.id] });
  return { ...seeded, priorities: { inherited: inherited.id, own: own.id } };
}

export interface SpineSeed {
  /** The answered question a reader would read as `shapedElsewhere`'s origin. No `from` edge
   *  names it, because the model holds none; see the doc comment. */
  readonly answeredAskId: string;
  /** The check whose `serves` names both `servedTwiceA` and `servedTwiceB`. */
  readonly checkServingTwo: string;
  /** The block id of the decision of the spec's `## Decisions` part that links `shapedElsewhere`
   *  and settles `answeredAskId`. */
  readonly decisionBlockId: string;
  readonly key: string;
  readonly requirements: typeof SPINE_REQUIREMENTS;
  readonly specArtifactId: string;
  readonly verificationArtifactId: string;
}

/** The spine's three cases: a requirement nothing explains, a requirement a decision links while
 *  the answered question names another node, and a check that serves two requirements. */
export async function seedSpineCases(): Promise<SpineSeed> {
  await createProject({ key: PROJECT, name: "Issue layout" });
  const issue = await createIssue({
    force: true,
    project: PROJECT,
    spec: SPINE_SPEC,
    title: "The spine places every node",
  });
  await seedSessions(issue.key, [PLANNER]);
  const answered = await createAsk(issue.key, { question: SPINE_ANSWERED_QUESTION }, asPlanner);
  await answerAsk(answered.id, {
    expected_edited_at: null,
    selected: [],
    text: "The issue itself.",
  });
  await editArtifact(
    issue.primary_artifact_id ?? "",
    {
      ops: [{ after: "end", markdown: spineDecisionsPart(issue.key, answered.id), op: "insert" }],
    },
    asPlanner
  );
  const verification = await createIssueArtifact(issue.key, {
    content: spineVerificationDocument(issue.key),
    name: "verification.md",
  });
  return {
    answeredAskId: answered.id,
    checkServingTwo: SPINE_CHECK_BLOCK_ID,
    decisionBlockId: SPINE_DECISION_BLOCK_ID,
    key: issue.key,
    requirements: SPINE_REQUIREMENTS,
    specArtifactId: issue.primary_artifact_id ?? "",
    verificationArtifactId: verification.artifact.id,
  };
}

export interface PlainIssueSeed {
  readonly key: string;
  readonly requirements: readonly string[];
  readonly specArtifactId: string;
  /** The block id of the three sentences, null on the spec that carries none. */
  readonly summaryBlockId: string | null;
}

/** One issue with a three-requirement spec and no children, with or without its summary node. */
async function seedPlainIssue(summary: boolean, title: string): Promise<PlainIssueSeed> {
  await createProject({ key: PROJECT, name: "Issue layout" });
  const issue = await createIssue({
    force: true,
    project: PROJECT,
    spec: specDocument({ openQuestion: false, requirements: 3, summary }),
    title,
  });
  return {
    key: issue.key,
    requirements: Array.from({ length: 3 }, (_, index) => requirementBlockId(index + 1)),
    specArtifactId: issue.primary_artifact_id ?? "",
    summaryBlockId: summary ? SUMMARY_BLOCK_ID : null,
  };
}

/** An issue with no children at all, which draws no children section. */
export async function seedNoChildren(): Promise<PlainIssueSeed> {
  return await seedPlainIssue(true, "This issue has no children");
}

/** An issue whose spec carries no summary node, so the head of the page says so. */
export async function seedNoSummary(): Promise<PlainIssueSeed> {
  return await seedPlainIssue(false, "This spec carries no summary node");
}

/** A thread of four comments - its head and three replies, each answering the one before it -
 *  and three questions in three states (c-reply-nesting). */
export interface ReplyNestingSeed {
  /** A question a person answered, which still takes a reply. */
  readonly answeredAsk: string;
  /** The reply to the head of the thread. */
  readonly first: string;
  /** The head of the thread, which sits under the issue. */
  readonly head: string;
  readonly key: string;
  /** A question that waits on the reader, with two options, which its row answers. */
  readonly openAsk: string;
  /** A question its asker resolved, which still takes a reply. */
  readonly resolvedAsk: string;
  /** The reply that answers `first`, and is drawn under it rather than under `head`. */
  readonly second: string;
  /** The reply that answers `second`, a level below the third under the issue, which a laptop
   *  folds to one line counting it. */
  readonly third: string;
}

/** An agent asking the questions of `seedReplyNesting`. */
const NESTING_ASKER = {
  actor: { id: "e2e-nesting-asker", kind: "session" as const },
  as: "agent" as const,
};

/** An issue carrying a thread whose second reply answers the first, and three questions: one a
 *  person answered, one its asker resolved, and one open and waiting on the reader. Every comment
 *  is written through the route a person writes through - `POST /api/v1/issues/{key}/comments`
 *  with `reply_to`, which the line at the foot of the outline sends - so the stored `answers` is
 *  the server's own and not a value this seed invented: the write climbs `reply_to` to the head
 *  of the thread and keeps the comment the request named beside it. */
export async function seedReplyNesting(): Promise<ReplyNestingSeed> {
  await createProject({ key: PROJECT, name: "Issue layout" });
  const issue = await createIssue({
    force: true,
    project: PROJECT,
    title: "A reply of this thread answers another reply",
  });
  const head = await createComment(issue.key, { body: "The first entry of the thread." });
  const first = await createComment(issue.key, {
    body: "An answer to the first entry.",
    reply_to: head.id,
  });
  const second = await createComment(issue.key, {
    body: "An answer to that answer, and not to the entry above it.",
    reply_to: first.id,
  });
  const third = await createComment(issue.key, {
    body: "A third answer, one level deeper again.",
    reply_to: second.id,
  });
  const answered = await createAsk(
    issue.key,
    { options: [{ label: "Yes" }, { label: "No" }], question: "Is the answered question kept?" },
    NESTING_ASKER
  );
  await answerAsk(answered.id, { expected_edited_at: null, selected: ["Yes"] }, { login: "alice" });
  const resolved = await createAsk(
    issue.key,
    { question: "Was the resolved question still worth a reply?" },
    NESTING_ASKER
  );
  await resolveAsk(resolved.id, { kind: "resolved", reason: "Settled elsewhere." }, NESTING_ASKER);
  const open = await createAsk(
    issue.key,
    {
      options: [{ label: "Left" }, { label: "Right" }],
      question: "Which way does the open question go?",
    },
    NESTING_ASKER
  );
  return {
    answeredAsk: answered.id,
    first: first.id,
    head: head.id,
    key: issue.key,
    openAsk: open.id,
    resolvedAsk: resolved.id,
    second: second.id,
    third: third.id,
  };
}

/** The unbroken run a requirement's own words can hold: one file path, 108 characters with no
 *  space in it, which is wider than the card that draws it at 390 px and at 1440 px alike. A
 *  person writes one whenever a requirement names a path, a key or an address. */
export const UNBROKEN_PATH =
  "packages/dispatch/web/src/features/issue/useIssueHeaderWrites.ts/and/one/more/segment/to/pass/one/hundred.ts";

/** An issue whose spec marks one requirement whose words hold that run. */
export async function seedUnbrokenWords(): Promise<PlainIssueSeed> {
  await createProject({ key: PROJECT, name: "Issue layout" });
  const issue = await createIssue({
    force: true,
    project: PROJECT,
    spec: [
      "# A requirement of this issue names a path",
      "",
      ":::requirement{#r-unbroken}",
      `The page reads ${UNBROKEN_PATH} at load.`,
      ":::",
      "",
    ].join("\n"),
    title: "A requirement of this issue names a long path",
  });
  return {
    key: issue.key,
    requirements: ["r-unbroken"],
    specArtifactId: issue.primary_artifact_id ?? "",
    summaryBlockId: null,
  };
}

/** The issue a reader opens to read that run in a child's own title, and the child that holds
 *  it. The child's row of the outline draws its title as its words and its key as its tag, so
 *  the run is the title's and not the key's. */
export interface ChildTitleSeed {
  /** The child whose title holds the run. */
  readonly childKey: string;
  /** The parent, which is the issue the page is opened at. */
  readonly key: string;
}

/** An issue one of whose children carries that same run in its own title. It is a second
 *  surface for the one run `UNBROKEN_PATH` already states, and reads it without touching the
 *  seed the requirement row uses. */
export async function seedUnbrokenChildTitle(): Promise<ChildTitleSeed> {
  await createProject({ key: PROJECT, name: "Issue layout" });
  const issue = await createIssue({
    force: true,
    project: PROJECT,
    title: "A child of this issue names a long path",
  });
  const child = await createIssue({
    force: true,
    parent: issue.key,
    project: PROJECT,
    title: `Draw ${UNBROKEN_PATH}`,
  });
  return { childKey: child.key, key: issue.key };
}

export interface BaselineSeed {
  readonly checks: readonly string[];
  readonly children: { readonly blocked: string; readonly done: string; readonly keys: string[] };
  readonly comments: { readonly replies: readonly string[]; readonly threads: readonly string[] };
  readonly decisions: readonly string[];
  readonly key: string;
  /** 1 for the baseline itself, 2 for the issue carrying twice its nodes. */
  readonly nodeMultiplier: 1 | 2;
  readonly openQuestionAskId: string;
  readonly planArtifactId: string;
  readonly planTasks: readonly string[];
  /** The slugs of the two documents the spec cites, which are the baseline's two references. */
  readonly references: readonly string[];
  readonly requirements: readonly string[];
  readonly specArtifactId: string;
  readonly verificationArtifactId: string;
}

/** The content the page's request baseline describes, at `multiplier` times its nodes: three
 *  labels, a repository with a pull request, two components, a parent, a claim, a message route,
 *  three subscribers, two references, seven children (one blocked, one done), twelve
 *  requirements, six checks with results in several states, nine plan tasks, two decision
 *  records, one open ask, six comment threads with eight replies, two handoffs. Its 37 events
 *  are what those writes record, not a number seeded on its own, and the properties are not
 *  multiplied: the multiplier doubles the nodes, which the request count is read against. */
async function seedBaseline(multiplier: 1 | 2): Promise<BaselineSeed> {
  const requirements = 12 * multiplier;
  const planTasks = 9 * multiplier;
  const seeded = await seedIssueWithDocuments({
    checks: 6 * multiplier,
    children: 7 * multiplier,
    commentReplies: 8 * multiplier,
    commentThreads: 6 * multiplier,
    decisions: 2 * multiplier,
    planTasks,
    repository: BASELINE_REPO,
    requirements,
    title:
      multiplier === 1
        ? "The first load stays inside its request budget"
        : "The same content with twice the nodes",
  });
  const references: string[] = [];
  for (const name of ["notes.md", "measurements.md"]) {
    const cited = await createIssueArtifact(seeded.key, {
      content: `# ${name}\n\nCited from the spec.\n`,
      name,
    });
    await editArtifact(seeded.specArtifactId, {
      ops: [
        {
          after: `block:${requirementBlockId(1)}`,
          markdown: `See dispatch://${seeded.key}/artifact/${cited.artifact.slug}.`,
          op: "insert",
        },
      ],
    });
    references.push(cited.artifact.slug);
  }
  await awaitSeededCheckStates(seeded.key, 6 * multiplier, true);
  return {
    checks: seeded.checks,
    children: seeded.children,
    comments: { replies: seeded.commentReplies, threads: seeded.commentThreads },
    decisions: seeded.decisions,
    key: seeded.key,
    nodeMultiplier: multiplier,
    openQuestionAskId: seeded.openQuestionAskId,
    planArtifactId: seeded.planArtifactId,
    planTasks: Array.from({ length: planTasks }, (_, index) => planTaskBlockId(index + 1)),
    references,
    requirements: Array.from({ length: requirements }, (_, index) => requirementBlockId(index + 1)),
    specArtifactId: seeded.specArtifactId,
    verificationArtifactId: seeded.verificationArtifactId,
  };
}

/** The issue the request baseline was measured on, seeded from that artifact's own description. */
export async function seedRequestBaseline(): Promise<BaselineSeed> {
  return await seedBaseline(1);
}

/** The same content with twice its nodes, which proves the page makes no request per node. */
export async function seedTwiceTheNodes(): Promise<BaselineSeed> {
  return await seedBaseline(2);
}

/** What `seedConversationState` adds to the whole issue: the things the history draws whole
 *  that no row of the outline does. */
export interface ConversationStateSeed extends WholeIssueSeed {
  /** Two comments agents wrote to each other, a thread no person wrote in. */
  readonly agentComments: { readonly reply: string; readonly root: string };
  /** The Legion daemon's message on the issue. */
  readonly daemonMessage: string;
  /** An agent's status note: a comment of its own, in no thread a person wrote in. */
  readonly statusNote: string;
  /** The names of three files one author uploaded within a minute. */
  readonly uploads: readonly string[];
}

/** The Legion daemon, which writes on an issue as a session of its own. */
const DAEMON = {
  actor: { id: "legion-daemon", kind: "session" as const, origin: { session_title: "Legion" } },
  as: "agent" as const,
};

/** The reviewer agent, which answers the planner on the issue. */
const asReviewer = {
  actor: { id: REVIEWER, kind: "session" as const },
  as: "agent" as const,
};

/** The whole issue (which carries its status, its two handoffs, its recorded results and its
 *  children's moves), claimed, with the daemon's message, an agent's status note, two comments
 *  agents wrote to each other and three uploads one author made within a minute
 *  (c-conversation-state). */
export async function seedConversationState(): Promise<ConversationStateSeed> {
  const seeded = await seedWholeIssue();
  await patchIssue(seeded.key, { status: "in_progress" });
  await claimIssue(seeded.key, asPlanner);
  const daemon = await createMessage(
    seeded.key,
    { body: "The planner's session was replaced; its claim moves to the new one." },
    DAEMON
  );
  const note = await createComment(
    seeded.key,
    { body: "Status: the plan is drafted and waits on its review." },
    asPlanner
  );
  const root = await createComment(
    seeded.key,
    { body: "Reviewer, the plan's third task needs a second look." },
    asPlanner
  );
  const reply = await createComment(
    seeded.key,
    { body: "Looked again: the third task stands as written.", reply_to: root.id },
    asReviewer
  );
  const uploads = ["shot-1.png", "shot-2.png", "shot-3.png"];
  for (const name of uploads) {
    await uploadIssueFile(
      seeded.key,
      {
        bytes: new Uint8Array(Buffer.from(ONE_PIXEL_PNG, "base64")),
        contentType: "image/png",
        name,
      },
      "bob"
    );
  }
  return {
    ...seeded,
    agentComments: { reply: reply.id, root: root.id },
    daemonMessage: daemon.id,
    statusNote: note.id,
    uploads,
  };
}

/** The whole issue with every kind of entry the Decisions filter draws (c-decisions). */
export interface DecisionsFilterSeed extends WholeIssueSeed {
  /** A question alice declined, with a note. */
  readonly declinedAskId: string;
  /** The agent decision alice kept: the fifth, which shapes the fourth requirement. */
  readonly keptBlock: string;
  /** The agent decision whose words changed last, so it heads the review list: the sixth. */
  readonly newestBlock: string;
  /** A question with two options, which alice answered with one of them and words of her own. */
  readonly optionedAskId: string;
}

/** `seedWholeIssue` and, on top of it, an ask answered by an option with words whose line
 *  breaks and spaces are the person's own, an ask the reader declined, a decision the reader
 *  kept, and a decision whose words changed after every other. */
export async function seedDecisionsFilter(): Promise<DecisionsFilterSeed> {
  const seeded = await seedWholeIssue();
  const optioned = await createAsk(
    seeded.key,
    {
      options: [{ label: "Rail" }, { label: "Sidebar" }],
      question: "Which navigation does the issue page keep?",
      urgency: "med",
    },
    asPlanner
  );
  await answerAsk(optioned.id, {
    expected_edited_at: null,
    selected: ["Rail"],
    text: "The rail.\n\n  It is 56 px wide -  no wider.",
  });
  const declined = await createAsk(
    seeded.key,
    { question: "On a phone, where does the box you write in sit?", urgency: "med" },
    asPlanner
  );
  await declineAsk(declined.id, { note: "The release owner decides this one." });
  // The Keep is of the fifth decision's `decision` block in the spec, as its entry's text reads
  // now; the sixth's words are then changed by the planner, in the spec, as an agent edits its
  // own decision.
  const keptBlock = seeded.decisions[4] ?? "";
  const kept = ((await getIssueDecisions(seeded.key)).entries ?? []).find(
    (entry) => entry.document?.block_id === keptBlock
  );
  await keepDecision(seeded.specArtifactId, keptBlock, kept?.text_sha256 ?? "");
  await editArtifact(
    seeded.specArtifactId,
    {
      ops: [
        {
          find: "A document is a node under the block that cites it.",
          op: "replace",
          with: "A document is one node under the block that cites it.",
        },
      ],
    },
    asPlanner
  );
  return {
    ...seeded,
    declinedAskId: declined.id,
    keptBlock,
    newestBlock: seeded.decisions[5] ?? "",
    optionedAskId: optioned.id,
  };
}

/** Four agent decisions, by what each one's link names (c-review-list): each a `decision` block
 *  of the spec's `## Decisions` part, by its block id, and `specSlug` the spec's slug their
 *  nodes are named under. */
export interface ReviewListSeed {
  readonly key: string;
  readonly specSlug: string;
  /** Tied to the second statement, and changed after the others: the newest of the list. */
  readonly tiedNewer: string;
  /** Tied to the first statement. */
  readonly tiedOlder: string;
  /** Tied to the spec's summary callout, which is no statement. */
  readonly tiedToNote: string;
  /** Tied to nothing. */
  readonly untied: string;
}

/** An issue with two open questions that wait on alice and four agent decisions, of which two
 *  name a statement: only those two wait for her review. */
export async function seedReviewList(): Promise<ReviewListSeed> {
  await createProject({ key: PROJECT, name: "Issue layout" });
  const issue = await createIssue({
    force: true,
    project: PROJECT,
    spec: specDocument({ openQuestion: true, requirements: 2, summary: true }),
    title: "Only a decision that changed a statement waits",
  });
  const spec = issue.primary_artifact_id ?? "";
  await patchIssue(issue.key, { assignee: "alice" });
  await awaitBlockAsk(issue.key, OPEN_QUESTION_BLOCK_ID);
  await createAsk(issue.key, { question: "Does the foot stay folded?", urgency: "med" }, asPlanner);
  const ref = (block: string) => specBlockRef(issue.key, block);
  const ids = {
    tiedNewer: "d-tied-newer",
    tiedOlder: "d-tied-older",
    tiedToNote: "d-tied-note",
    untied: "d-untied",
  };
  // Each tie is the decision's `shaped` attribute, which its words name as well.
  const decision = (id: string, heading: string, shaped: string | null, words: string) =>
    `:::decision{#${id}${shaped === null ? "" : ` shaped="${shaped}"`}}\n### ${heading}\n\n${words}\n:::`;
  const decisions = [
    decision(
      ids.tiedOlder,
      "Tie the first statement",
      ref(requirementBlockId(1)),
      `Shapes ${ref(requirementBlockId(1))}.`
    ),
    decision(
      ids.tiedNewer,
      "Tie the second statement",
      ref(requirementBlockId(2)),
      `Shapes ${ref(requirementBlockId(2))}.`
    ),
    decision(
      ids.tiedToNote,
      "Tie the note",
      ref(SUMMARY_BLOCK_ID),
      `Shapes ${ref(SUMMARY_BLOCK_ID)}.`
    ),
    decision(ids.untied, "Tie nothing", null, "A choice no statement rests on."),
  ];
  await editArtifact(
    spec,
    {
      ops: [
        { after: "end", markdown: `## Decisions\n\n${decisions.join("\n\n")}\n`, op: "insert" },
      ],
    },
    asPlanner
  );
  await editArtifact(
    spec,
    {
      ops: [
        {
          find: `Shapes ${ref(requirementBlockId(2))}.`,
          op: "replace",
          with: `It shapes ${ref(requirementBlockId(2))}.`,
        },
      ],
    },
    asPlanner
  );
  const specSlug =
    ((await getIssue(issue.key)).artifacts ?? []).find((artifact) => artifact.id === spec)?.slug ??
    "";
  return { key: issue.key, specSlug, ...ids };
}

export interface PlanOrderSeed {
  readonly key: string;
  readonly planArtifactId: string;
  readonly specArtifactId: string;
}

/** The plan c-plan-order reads: tasks written in an order their `needs` refuse, two tasks that
 *  need each other, and a task whose `needs` names a task the plan no longer holds. The second
 *  write is what leaves that pointer dangling, and it changes no pointer of its own. */
export async function seedPlanOrder(): Promise<PlanOrderSeed> {
  await createProject({ key: PROJECT, name: "Issue layout" });
  const issue = await createIssue({
    force: true,
    project: PROJECT,
    spec: specDocument({ openQuestion: false, requirements: 3, summary: true }),
    title: "The plan is drawn as ordered steps",
  });
  const plan = await createIssueArtifact(issue.key, {
    content: planOrderDocument(issue.key, true),
    name: "plan.md",
  });
  // The server matches a quote against the block's text as it renders it, not the directive's
  // markdown, so the task goes by its block id rather than by a quote of its source.
  await editArtifact(plan.artifact.id, {
    ops: [{ block: PLAN_ORDER_TASKS.gone, op: "delete" }],
  });
  return {
    key: issue.key,
    planArtifactId: plan.artifact.id,
    specArtifactId: issue.primary_artifact_id ?? "",
  };
}

/** What `seedWideCode` wrote: the issue, the comment whose words hold a wide code block and a
 *  wide table, and the comment of words alone after it. */
export interface WideCodeSeed {
  readonly key: string;
  readonly plain: string;
  readonly wide: string;
}

/**
 * One issue whose conversation is two comments of the reader's. The first one's body holds a
 * code block of one line and a table of sixteen columns, each far wider than a row's words at
 * 1440 px, so each scrolls sideways inside the row; the second's holds words alone. The reader's
 * own words are never cut, so the first row draws both blocks whole. It is an issue of its own,
 * so no seed another row reads changes.
 */
export async function seedWideCode(): Promise<WideCodeSeed> {
  await createProject({ key: PROJECT, name: "Issue layout" });
  const issue = await createIssue({
    force: true,
    project: PROJECT,
    title: "A comment holds a code block wider than its row",
  });
  const flags = Array.from({ length: 60 }, (_unused, at) => `--flag${at + 1}`).join(" ");
  const line = (cells: readonly string[]) => `| ${cells.join(" | ")} |`;
  const columns = Array.from({ length: 16 }, (_unused, at) => at + 1);
  const table = [
    line(columns.map((column) => `columnheading${column}`)),
    line(columns.map(() => "---")),
    line(columns.map((column) => `value${column}`)),
  ].join("\n");
  const wide = await createComment(issue.key, {
    body: `Run it so:\n\n\`\`\`sh\nbun run e2e ${flags}\n\`\`\`\n\n${table}\n`,
  });
  const plain = await createComment(issue.key, {
    body: "A comment of words alone, which hold no code block and no table.",
  });
  return { key: issue.key, plain: plain.id, wide: wide.id };
}
