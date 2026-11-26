import { expect, type Locator, type Page } from "@playwright/test";

import type { IssueTreeNode, IssueTreeParent, IssueTreeRead } from "../web/src/api/types";
import {
  criterionEdits,
  criterionMarkdown,
} from "../web/src/features/issue-page/model-rows/criterion-write";
import type { NodeType } from "../web/src/features/issue-page/page-words";
import { treeIdOfNode } from "../web/src/features/issue-page/tree-read/tree-nodes";
import {
  answerAsk,
  baseUrl,
  createComment,
  createIssue,
  createIssueArtifact,
  editArtifact,
  getArtifact,
  getArtifactText,
  getAsk,
  getComment,
  getIssueCoverage,
  getIssueResults,
  recordCheckResult,
  requestApproval,
  reviewArtifact,
} from "./api";
import { seedWholeIssue, type WholeIssueSeed } from "./issue-layout-seed";
import { asPlanner, HEAD_SHA, PROJECT, pause, readTree } from "./issue-layout-seed-support";
import {
  checkBlockId,
  checkText,
  OPEN_QUESTION_BLOCK_ID,
  requirementBlockId,
  requirementText,
  specBlockRef,
} from "./issue-layout-seed-text";
import { blockNode, longWords, READER } from "./issue-page-g";
import { rowsOfType } from "./issue-page-i";
import { composer } from "./issue-page-l";

/**
 * What the rows of `e2e/issue-page-y.e2e.ts` seed and read of the new issue page: criteria,
 * readings, a child issue's checks under the parent's statements, editing a converted issue in
 * its one room on the outline and in each filter, and the Verification view drawing long words
 * whole. Each of those rows is selected by its title from the issue's checks. This file is
 * those rows' own: it imports the other issue-page e2e files' readers and changes none of them.
 *
 * - **The tree read** `GET /api/v1/issues/{key}/tree` is read through `readTree`
 *   (`e2e/issue-layout-seed-support.ts`), every page merged, and narrowed by `treeAnswerOf` to
 *   `converted`, `tree`, `view_of`, and each node's `id`, `block_id`, `document`, `issue`,
 *   `supertag`, `parent` and `text`. `artifactRoomOf` reads the artifact read's `room` through
 *   `getArtifact`, typed by hand, since the generated artifact holds no such member yet.
 * - **The rows** are the outline's `[data-node-row]`, each carrying `data-node-type`
 *   (`NodeRow.tsx`): a criterion's row is `criterion` and a reading's `reading`. A node of
 *   another issue carries a link `[data-node-issue]` to that issue before its kind tag, which
 *   marks a borrowed row.
 * - **The box's criterion line** is the page's one box set to write a criterion
 *   (`IssueComposer.tsx`'s `CriterionForm`): a form `[data-criterion-under="<the node's row id>"]`
 *   inside `[data-testid="issue-composer"]`, its refusal in `[data-criterion-answer]`. The act
 *   that aims it is the focused row's `[data-row-act="criterion"]`.
 * - **An editor's socket** is `/ws/doc/<artifact id>` (`ProofDocument`), which the rows count
 *   with `countDocumentSockets` (`e2e/editor.ts`) from just before the Enter that opens it.
 */

/** How long the server has to answer a write in a read the rows wait on: the seeds' own bound
 *  for the document service's settling (`awaitBlockAsk`, `awaitSeededCheckStates`). */
const SETTLED_WITHIN_MS = 20_000;

/** The block ids this file writes. None is a block id of the seed's documents, and the child's
 *  two are none of the parent's, so a block id names one node in either issue. */
const BLOCKS = {
  check: "c-child",
  decision: "d-model",
  decisionReading: "rd-decision",
  longCheck: "c-long",
  longStatement: "r-long",
  questionReading: "rd-question",
  task: "t-child",
} as const;

/** One word each written text holds and no other node of either issue holds: the seed finds each
 *  node in the tree read by it, and a row tells that node's row from every other by it. */
const MODEL_WORDS = {
  answer: "Copperwick",
  check: "Emberglade",
  comment: "Gorsevale",
  decision: "Ambershale",
  decisionReading: "Brindlecove",
  questionReading: "Dovetarn",
  task: "Flintmoor",
} as const;

// ---------------------------------------------------------------------------------------------
// The tree read and the artifact read.
// ---------------------------------------------------------------------------------------------

/** One node of the tree read, by the members these rows read. Its `parent` is the read's own
 *  `IssueTreeParent`: the node it stands under, named by its `id`. */
export interface TreeReadNode {
  /** The block's own id inside its document or tree, absent for a node that is no block. */
  readonly block_id?: string;
  /** The document a typed block of one of this issue's documents lives in, absent otherwise. */
  readonly document?: IssueTreeNode["document"];
  readonly id: string;
  /** The owning issue of a foreign node, absent for a node of this issue. */
  readonly issue?: string;
  /** The node it stands under, or null for a node at the top. */
  readonly parent: IssueTreeParent | null;
  readonly supertag: string;
  readonly text: string;
}

/** One issue's tree read, its pages merged. */
export interface TreeReadAnswer {
  readonly converted: boolean;
  /** Every page's nodes, in the read's order. */
  readonly nodes: readonly TreeReadNode[];
  /** The issue's tree artifact, which a criterion is written into; null where the conversion
   *  has not reached the issue. */
  readonly tree: string | null;
  /** The ancestor whose statements the issue is a view of, or null. */
  readonly view_of: string | null;
}

/** Whether the server serves the issue's tree read, from its public route index. Only rows
 * that need a converted issue use this gate; the long-words row reads coverage and results. */
export async function treeReadServed(): Promise<boolean> {
  const response = await fetch(new URL("/api/v1", baseUrl));
  if (!response.ok) throw new Error(`GET /api/v1 failed: ${response.status}`);
  const index = (await response.json()) as {
    readonly routes: readonly { readonly method: string; readonly path: string }[];
  };
  return index.routes.some(
    (route) => route.method === "GET" && route.path === "/api/v1/issues/{key}/tree"
  );
}

/** What a row says when it skips on that gate. */
export const TREE_READ_OWED =
  "owed: this row reads the issue's tree read, `GET /api/v1/issues/{key}/tree`, which this server does not serve";

/** The tree read of issue `key`, every page it names read in turn and merged. */
export async function treeAnswerOf(key: string): Promise<TreeReadAnswer> {
  const read: IssueTreeRead = await readTree(key, null);
  return {
    converted: read.converted,
    nodes: read.nodes ?? [],
    tree: read.tree,
    view_of: read.view_of,
  };
}

/** The room the artifact read of `id` answers, the issue's tree artifact on a converted issue,
 *  or null where it answers none. */
export async function artifactRoomOf(id: string): Promise<string | null> {
  const read = (await getArtifact(id)) as { readonly room?: string | null };
  return read.room ?? null;
}

/** One node a wait looks for in a tree read: by its supertag and one run of words its text
 *  holds, and, where it names one, under the node an earlier entry of the same wait found. */
interface Wanted {
  /** The exact read id to match, where text alone could select a duplicate block or ask node. */
  readonly id?: string;
  /** What a failure names it by. */
  readonly name: string;
  readonly supertag: NodeType;
  /** The `name` of the entry whose node it stands under. */
  readonly under?: string;
  /** Words its text holds; none for a node told apart by its place alone. */
  readonly words?: string;
}

/** What a wait found: the tree read's node for each entry, by its name, and the answer it was
 *  found in. */
interface Placed {
  readonly answer: TreeReadAnswer;
  readonly nodes: ReadonlyMap<string, TreeReadNode>;
}

/**
 * Waits, at most `SETTLED_WITHIN_MS`, until the tree read of `key` answers each of `wanted`
 * where it is placed, and fails naming the first one it does not answer so. The server settles
 * a write into the tree after the write has answered, which is why it waits at all.
 */
async function awaitPlaced(key: string, wanted: readonly Wanted[]): Promise<Placed> {
  const deadline = Date.now() + SETTLED_WITHIN_MS;
  for (;;) {
    const answer = await treeAnswerOf(key);
    const nodes = new Map<string, TreeReadNode>();
    const missing = wanted.find((want) => {
      const parent = want.under === undefined ? undefined : nodes.get(want.under)?.id;
      const node = answer.nodes.find(
        (one) =>
          one.supertag === want.supertag &&
          (want.id === undefined || one.id === want.id) &&
          (want.words === undefined || one.text.includes(want.words)) &&
          (want.under === undefined || (one.parent !== null && one.parent.id === parent))
      );
      if (node !== undefined) nodes.set(want.name, node);
      return node === undefined;
    });
    if (missing === undefined) return { answer, nodes };
    if (Date.now() > deadline) {
      throw new Error(
        `the tree read of ${key} does not answer ${missing.name} where it was written within ` +
          `${SETTLED_WITHIN_MS / 1000} s`
      );
    }
    await pause(100);
  }
}

/** The node `placed` found for the entry `name`. */
function placedNode(placed: Placed, name: string): TreeReadNode {
  const node = placed.nodes.get(name);
  if (node === undefined) throw new Error(`the wait found no ${name}`);
  return node;
}

/** The id `placed` found for the entry `name`. */
function placedId(placed: Placed, name: string): string {
  return placedNode(placed, name).id;
}

// ---------------------------------------------------------------------------------------------
// The page's readers.
// ---------------------------------------------------------------------------------------------

/** Every row of a criterion. */
export function criterionRows(page: Page): Locator {
  return rowsOfType(page, "criterion" satisfies NodeType);
}

/** The focused row's act that aims the box at its node to write a criterion under it. */
export function criterionAct(row: Locator): Locator {
  return row.locator('[data-row-act="criterion"]');
}

/** The box's line while it writes a criterion, which names the row it writes under. */
export function criterionLine(page: Page): Locator {
  return composer(page).locator("form[data-criterion-under]");
}

/** The sentence the criterion line draws for a refused send. */
export function criterionRefusal(page: Page): Locator {
  return criterionLine(page).locator("[data-criterion-answer]");
}

// ---------------------------------------------------------------------------------------------
// Writing a criterion as the page does.
// ---------------------------------------------------------------------------------------------

/** One criterion written into the issue's tree artifact (`writeCriterionApi`): its read id in the
 *  tree read (`id`) and the row id `treeIdOfNode` gives that bare tree block (`row`, its
 *  `block_id`), which `data-node-id` carries on the outline and in Spec. */
export interface WrittenCriterion {
  readonly id: string;
  readonly row: string;
}

/**
 * The page's own write of a criterion: the body `criterionEdits` (`criterion-write.ts`) builds
 * for `criterionMarkdown(parent, text)`, the one the page's request sends, appended to issue
 * `key`'s tree artifact `tree`, the tree read's `tree`, by `POST /api/v1/artifacts/{tree}/edits`
 * as the reader. It answers the criterion's read id and its page row id (`treeIdOfNode`) once
 * the read holds it, under no node in particular.
 */
export async function writeCriterionApi(
  key: string,
  tree: string,
  parent: string,
  text: string
): Promise<WrittenCriterion> {
  await editArtifact(tree, criterionEdits(criterionMarkdown(parent, text)), { login: READER });
  const name = "the criterion written through the edits route";
  const words = text.split("\n")[0]?.trim() ?? text;
  const placed = await awaitPlaced(key, [{ name, supertag: "criterion", words }]);
  const node = placedNode(placed, name);
  return {
    id: node.id,
    row: treeIdOfNode(node as IssueTreeNode, key),
  };
}

// ---------------------------------------------------------------------------------------------
// The seed.
// ---------------------------------------------------------------------------------------------

/** One node `seedModelRows` wrote or names. */
export interface ModelNode {
  /** Its own `dispatch://` address. */
  readonly address: string;
  /** The id the tree read answers for it. */
  readonly id: string;
  /** The id the outline draws its row under (`data-node-id`), matching `treeIdOfNode`: a document
   *  block keeps `<slug>#b-<block>`, a foreign block or bare tree block takes its `block_id`, and
   *  any non-block node keeps its read id. */
  readonly row: string;
}

/** The child `seedModelRows` files under the seeded issue. */
export interface ModelChild {
  /** Its `verification.md`'s one check, which serves the parent's statement 7. */
  readonly check: ModelNode;
  /** alice's comment on it: its id, which is its row's. */
  readonly comment: string;
  readonly key: string;
  /** The check's passing result: the tree read's id for it, which is its row's. */
  readonly result: string;
  /** Its plan's one task, which serves the parent's statement 7. */
  readonly task: ModelNode;
}

/** What `seedModelRows` wrote over `seedWholeIssue`, and the nodes of it the rows read. */
export interface ModelRowsSeed extends WholeIssueSeed {
  /** The spec's version alice approved, its current version when the seed returns. */
  readonly approvedVersion: number;
  /** Check c1 of the issue's `verification.md`. */
  readonly checkOne: ModelNode;
  readonly child: ModelChild;
  /** The planner's decision in the spec's `## Decisions`. */
  readonly decision: ModelNode;
  /** The planner's reading of that decision. */
  readonly decisionReading: ModelNode;
  /** The spec's `q-open` ask block, answered by alice. */
  readonly question: ModelNode;
  /** The planner's reading of alice's answer to it. */
  readonly questionReading: ModelNode;
  /** The slug of the spec, which part G's row layouts read the statements of. */
  readonly specSlug: string;
  readonly statementOne: ModelNode;
  readonly statementSeven: ModelNode;
  /** The first comment thread, by alice, and the first line of its words. */
  readonly thread: ModelNode & { readonly firstLine: string };
  /** The issue's tree artifact. */
  readonly tree: string;
}

/** The spec's `## Decisions` part: one decision of the planner's and its reading. A decision
 *  block is a four-colon fence in that part, so the reading's three colons close it not. */
const DECISIONS_PART =
  `## Decisions\n\n::::decision{#${BLOCKS.decision} uncertainty="low"}\n` +
  `${MODEL_WORDS.decision}: the model's rows come from the one tree read.\n\n` +
  `:::reading{#${BLOCKS.decisionReading}}\n` +
  `${MODEL_WORDS.decisionReading}: the planner reads this as one read for every row.\n:::\n` +
  "::::\n";

/** The opening line of the spec's `q-open` ask block. */
const QUESTION_OPENING = new RegExp(`^:::ask\\{[^}]*#${OPEN_QUESTION_BLOCK_ID}[\\s}]`);

/** Whether the spec's stored markdown holds the `q-open` ask block's opening fence. */
function hasQuestionBlock(markdown: string): boolean {
  return markdown.split("\n").some((line) => QUESTION_OPENING.test(line));
}

/**
 * The spec's text with a reading under the answered `q-open`: its opening and closing fences
 * lengthened to `::::` and the reading before that closing fence. pmdoc reads the outer block
 * first, and a reading's `:::` would close a `:::ask` (`pmdoc/directive.go`).
 */
function withQuestionReading(markdown: string): string {
  const lines = markdown.split("\n");
  const opening = lines.findIndex((line) => QUESTION_OPENING.test(line));
  const closing = lines.findIndex((line, at) => at > opening && line.trimEnd() === ":::");
  if (opening === -1 || closing === -1) {
    throw new Error(`the spec holds no whole ${OPEN_QUESTION_BLOCK_ID} block:\n${markdown}`);
  }
  return [
    ...lines.slice(0, opening),
    `:${lines[opening]}`,
    ...lines.slice(opening + 1, closing),
    `:::reading{#${BLOCKS.questionReading}}`,
    `${MODEL_WORDS.questionReading}: the planner reads the answer as the width the header condenses at.`,
    ":::",
    "::::",
    ...lines.slice(closing + 1),
  ].join("\n");
}

/** Waits until the spec's text, as its live room holds it, satisfies `holds`, answering both the
 *  markdown and the document token that read carried. */
async function awaitSpecText(
  id: string,
  holds: (markdown: string) => boolean,
  what: string
): Promise<{ readonly markdown: string; readonly token: string }> {
  const deadline = Date.now() + SETTLED_WITHIN_MS;
  for (;;) {
    const { markdown, token } = await getArtifactText(id);
    if (holds(markdown) && token !== undefined) return { markdown, token };
    if (Date.now() > deadline) {
      throw new Error(`the spec's text does not hold ${what} within ${SETTLED_WITHIN_MS / 1000} s`);
    }
    await pause(100);
  }
}

/** A check block of these rows' own: `block` serving `serves`, run in the browser by this file
 *  before merge, with its negative control and its words. */
function browserCheck(
  block: string,
  serves: string,
  negativeControl: string,
  words: string
): string {
  return (
    `:::check{#${block} serves="${serves}" surface="browser"` +
    ' run="bun run e2e -- e2e/issue-page-y.e2e.ts" when="before_merge"' +
    ` negative_control="${negativeControl}"}\n${words}\n:::\n`
  );
}

/** The child's `verification.md`: one check, which serves the parent's statement 7. */
function childVerification(parentKey: string): string {
  return (
    "How the child is proved.\n\n" +
    browserCheck(
      BLOCKS.check,
      specBlockRef(parentKey, requirementBlockId(7)),
      "Draw the check under the child's row and the row fails.",
      `${MODEL_WORDS.check}: the parent's seventh statement holds on the child's page.`
    )
  );
}

/** The child's `plan.md`: one task, which serves the parent's statement 7. */
function childPlan(parentKey: string): string {
  const serves = specBlockRef(parentKey, requirementBlockId(7));
  return (
    "How the child is built.\n\n" +
    `:::plan_task{#${BLOCKS.task} serves="${serves}"}\n` +
    `${MODEL_WORDS.task}: build the child's half of the seventh statement.\n:::\n`
  );
}

/** Waits until the child's Verification document holds its check, which a result is refused
 *  against until the document service has settled it (`400 UNKNOWN_CHECK`). */
async function awaitChildCheck(childKey: string): Promise<void> {
  const deadline = Date.now() + SETTLED_WITHIN_MS;
  for (;;) {
    const read = await getIssueResults(childKey);
    if ((read.checks ?? []).some((check) => check.block_id === BLOCKS.check)) return;
    if (Date.now() > deadline) {
      throw new Error(
        `${childKey}'s ${BLOCKS.check} check was not indexed within ${SETTLED_WITHIN_MS / 1000} s`
      );
    }
    await pause(100);
  }
}

/** The `dispatch://` address of block `block` of the document `slug` of issue `key`. */
function blockAddress(key: string, slug: string, block: string): string {
  return `dispatch://${key}/artifact/${slug}#b-${block}`;
}

/**
 * The whole issue (`seedWholeIssue`) with what these rows draw written over it, every write as the
 * planner agent (`asPlanner`) unless it says alice, in this order:
 *
 * 1. a decision node and its reading, in the spec's `## Decisions`;
 * 2. the spec's `q-open` answered as alice (whose answer stands on the ask's node, not in the
 *    markdown), then its reading uploaded against the spec's current document token;
 * 3. the spec's approval, requested as the planner and given as alice, which must stand at the
 *    spec's current version;
 * 4. a child with a `verification.md` whose one check serves statement 7 and a `plan.md` whose
 *    one task serves it too, a passing result with its negative control on that check, and a
 *    comment by alice on the child;
 * 5. a wait, at most 20 s, until the tree read answers each of these where it was written, which
 *    fails naming the one it does not.
 *
 * Each written text holds one word of `MODEL_WORDS`, which no other node holds.
 */
export async function seedModelRows(): Promise<ModelRowsSeed> {
  const seeded = await seedWholeIssue();
  const { key } = seeded;
  const [spec, { slug: verificationSlug }] = await Promise.all([
    getArtifact(seeded.specArtifactId),
    getArtifact(seeded.verificationArtifactId),
  ]);

  // (1) The decision and its reading.
  await editArtifact(
    spec.id,
    { ops: [{ after: "end", markdown: DECISIONS_PART, op: "insert" }] },
    asPlanner
  );

  // (2) The question answered, then its reading in the spec's next version.
  const answeredAsk = await answerAsk(seeded.openQuestion.askId, {
    expected_edited_at: null,
    selected: [],
    text: `${MODEL_WORDS.answer}: 1280 px.`,
  });
  expect(answeredAsk.state, `answering ${OPEN_QUESTION_BLOCK_ID} did not leave it answered`).toBe(
    "answered"
  );
  const checkedAsk = await getAsk(seeded.openQuestion.askId);
  expect(checkedAsk.ask.state, `reading ${OPEN_QUESTION_BLOCK_ID} did not answer answered`).toBe(
    "answered"
  );
  const currentSpec = await awaitSpecText(
    spec.id,
    (markdown) => markdown.includes(MODEL_WORDS.decision) && hasQuestionBlock(markdown),
    `the decision and ${OPEN_QUESTION_BLOCK_ID}`
  );
  await createIssueArtifact(
    key,
    {
      content: withQuestionReading(currentSpec.markdown),
      name: spec.name,
      precondition: { document: currentSpec.token },
    },
    asPlanner
  );
  await awaitSpecText(
    spec.id,
    (markdown) => markdown.includes(MODEL_WORDS.questionReading),
    `the reading of ${OPEN_QUESTION_BLOCK_ID}`
  );

  // (3) The approval, at the spec's current version.
  await requestApproval(spec.id, {}, asPlanner);
  const reviewed = await reviewArtifact(spec.id, { state: "approved" }, { login: READER });
  const approval = (await getArtifact(spec.id)).approval;
  expect(
    { state: approval?.state, version: reviewed.version },
    "alice's approval does not stand at the spec's current version"
  ).toEqual({ state: "approved", version: approval?.latest_version });

  // (4) The child, its check and task under statement 7, the check's result and alice's comment.
  const child = await createIssue(
    {
      force: true,
      parent: key,
      project: PROJECT,
      title: "Serve the seventh statement from the issue under it",
    },
    asPlanner
  );
  const childChecks = await createIssueArtifact(
    child.key,
    { content: childVerification(key), name: "verification.md" },
    asPlanner
  );
  const childTasks = await createIssueArtifact(
    child.key,
    { content: childPlan(key), name: "plan.md" },
    asPlanner
  );
  await awaitChildCheck(child.key);
  await recordCheckResult(
    child.key,
    {
      check: BLOCKS.check,
      commit: HEAD_SHA,
      failed: 0,
      negative_control: "Drew the check under the child's row and the row failed.",
      observed: "1 passed",
      role: "tester",
      run: "bun run e2e -- e2e/issue-page-y.e2e.ts",
      surface: "browser",
      verdict: "pass",
    },
    asPlanner
  );
  const comment = await createComment(child.key, {
    body: `${MODEL_WORDS.comment}: this child serves its parent's seventh statement.`,
  });

  // (5) Each of them in the tree read, where it was written.
  const thread = seeded.comments.threads[0] ?? "";
  const threadRead = await getComment(thread);
  const threadLine = threadRead.comment.body.split("\n")[0]?.trim() ?? "";
  const NAMES = {
    check: "the child's check",
    checkOne: "check c1",
    decision: "the decision",
    decisionReading: "the decision's reading",
    question: `the answered ${OPEN_QUESTION_BLOCK_ID}`,
    questionReading: `the reading of ${OPEN_QUESTION_BLOCK_ID}`,
    result: "the result of the child's check",
    statementOne: "statement 1",
    statementSeven: "statement 7",
    task: "the child's plan task",
    thread: "the first comment thread",
  } as const;
  // The child's wait needs nothing the parent's finds, so the two run together.
  const [placed] = await Promise.all([
    awaitPlaced(key, [
      { name: NAMES.statementOne, supertag: "requirement", words: requirementText(1) },
      { name: NAMES.statementSeven, supertag: "requirement", words: requirementText(7) },
      { name: NAMES.checkOne, supertag: "check", words: checkText(1) },
      { name: NAMES.thread, supertag: "comment", words: threadLine },
      { name: NAMES.decision, supertag: "decision", words: MODEL_WORDS.decision },
      {
        name: NAMES.decisionReading,
        supertag: "reading",
        under: NAMES.decision,
        words: MODEL_WORDS.decisionReading,
      },
      { id: seeded.openQuestion.askId, name: NAMES.question, supertag: "ask" },
      {
        name: NAMES.questionReading,
        supertag: "reading",
        under: NAMES.question,
        words: MODEL_WORDS.questionReading,
      },
      { name: NAMES.check, supertag: "check", words: MODEL_WORDS.check },
      { name: NAMES.result, supertag: "result", under: NAMES.check },
      { name: NAMES.task, supertag: "plan_task", words: MODEL_WORDS.task },
    ]),
    awaitPlaced(child.key, [
      { name: "alice's comment on the child", supertag: "comment", words: MODEL_WORDS.comment },
    ]),
  ]);
  const tree = placed.answer.tree;
  if (tree === null) throw new Error(`the tree read of ${key} names no tree artifact`);

  const id = (name: string) => placedId(placed, name);
  const rowOfPlaced = (name: string): string =>
    treeIdOfNode(placedNode(placed, name) as IssueTreeNode, key);
  const specBlock = (name: string, block: string): ModelNode => ({
    address: blockAddress(key, spec.slug, block),
    id: id(name),
    row: blockNode(spec.slug, block),
  });
  const borrowed = (name: string, slug: string, block: string): ModelNode => ({
    address: blockAddress(child.key, slug, block),
    id: id(name),
    row: rowOfPlaced(name),
  });
  return {
    ...seeded,
    approvedVersion: reviewed.version,
    checkOne: {
      address: blockAddress(key, verificationSlug, checkBlockId(1)),
      id: id(NAMES.checkOne),
      row: blockNode(verificationSlug, checkBlockId(1)),
    },
    child: {
      check: borrowed(NAMES.check, childChecks.artifact.slug, BLOCKS.check),
      comment: comment.id,
      key: child.key,
      result: rowOfPlaced(NAMES.result),
      task: borrowed(NAMES.task, childTasks.artifact.slug, BLOCKS.task),
    },
    decision: specBlock(NAMES.decision, BLOCKS.decision),
    decisionReading: specBlock(NAMES.decisionReading, BLOCKS.decisionReading),
    question: specBlock(NAMES.question, OPEN_QUESTION_BLOCK_ID),
    questionReading: specBlock(NAMES.questionReading, BLOCKS.questionReading),
    specSlug: spec.slug,
    statementOne: specBlock(NAMES.statementOne, requirementBlockId(1)),
    statementSeven: specBlock(NAMES.statementSeven, requirementBlockId(7)),
    thread: {
      address: `dispatch://${key}/comment/${thread}`,
      firstLine: threadLine,
      id: id(NAMES.thread),
      row: thread,
    },
    tree,
  };
}

// ---------------------------------------------------------------------------------------------
// The seed of row 5: a 200-word statement and a 200-word check that serves it.
// ---------------------------------------------------------------------------------------------

/** One long block `seedLongWords` wrote: the id the outline and the Verification view draw its
 *  row under (`data-node-id`, `blockNode`), and the 200 words written into it. */
export interface LongNode {
  readonly row: string;
  readonly words: string;
}

/** What `seedLongWords` wrote over `seedWholeIssue`, and the two long nodes row 5 reads. */
export interface LongWordsSeed extends WholeIssueSeed {
  readonly longCheck: LongNode;
  readonly longStatement: LongNode;
}

/**
 * `seedWholeIssue` with two 200-word blocks appended, as the seed's writer (`asPlanner`): one
 * statement at the end of the spec and one check at the end of `verification.md` whose `serves`
 * names that statement. It waits, at most 20 s, until the issue's coverage read counts one check
 * for the statement and its results read holds the check, so the Verification view reads both on
 * load rather than the indexing. It reads no tree read.
 */
export async function seedLongWords(): Promise<LongWordsSeed> {
  const seeded = await seedWholeIssue();
  const { key } = seeded;
  const [{ slug: specSlug }, { slug: verificationSlug }] = await Promise.all([
    getArtifact(seeded.specArtifactId),
    getArtifact(seeded.verificationArtifactId),
  ]);
  const statementWords = longWords("stated", 200);
  const checkWords = longWords("checked", 200);
  const statementBlock = `:::requirement{#${BLOCKS.longStatement}}\n${statementWords}\n:::\n`;
  const checkBlock = browserCheck(
    BLOCKS.longCheck,
    specBlockRef(key, BLOCKS.longStatement),
    "Cut either long block at six lines and the row fails.",
    checkWords
  );

  // Write the statement first, so the check names a block that has already been written.
  await editArtifact(
    seeded.specArtifactId,
    { ops: [{ after: "end", markdown: statementBlock, op: "insert" }] },
    asPlanner
  );
  await editArtifact(
    seeded.verificationArtifactId,
    { ops: [{ after: "end", markdown: checkBlock, op: "insert" }] },
    asPlanner
  );

  const deadline = Date.now() + SETTLED_WITHIN_MS;
  for (;;) {
    const [coverage, results] = await Promise.all([getIssueCoverage(key), getIssueResults(key)]);
    const covered = (coverage.requirements ?? []).some(
      (one) => one.block_id === BLOCKS.longStatement && one.checks === 1
    );
    const checked = (results.checks ?? []).some((one) => one.block_id === BLOCKS.longCheck);
    if (covered && checked) break;
    if (Date.now() > deadline) {
      throw new Error(
        `${key}'s coverage and results reads do not hold ${BLOCKS.longStatement} (covered=${covered}) ` +
          `and ${BLOCKS.longCheck} (checked=${checked}) within ${SETTLED_WITHIN_MS / 1000} s`
      );
    }
    await pause(100);
  }

  return {
    ...seeded,
    longCheck: {
      row: blockNode(verificationSlug, BLOCKS.longCheck),
      words: checkWords,
    },
    longStatement: {
      row: blockNode(specSlug, BLOCKS.longStatement),
      words: statementWords,
    },
  };
}
