import { existsSync } from "node:fs";
import { expect, type Locator, type Page, test } from "@playwright/test";

import { type Body, type Result, routeUrl } from "../web/src/api/paths";
import type { IssueTreeNode, IssueTreeRead } from "../web/src/api/types";
import { blockIdOfNode } from "../web/src/features/issue-page/decision-review";
import { PRESET_NAMES, type Preset } from "../web/src/features/issue-page/page-words";
import { checkStanding } from "../web/src/features/issue-page/tree-drafts";
import type { TreeIdReads } from "../web/src/features/issue-page/tree-read/tree-nodes";
import { buildDispatchReference } from "../web/src/features/refs/routes";
import {
  baseUrlOf,
  createIssue,
  createProject,
  editArtifact,
  getIssue,
  getIssueCoverage,
  getIssueDecisions,
  getIssueResults,
  listComments,
  listIssueAsks,
  recordCheckResult,
  userHeaders,
} from "./api";
import { seedWholeIssue, type WholeIssueSeed } from "./issue-layout-seed";
import { asPlanner, HEAD_SHA, pause, readTree, recorderActor } from "./issue-layout-seed-support";
import { OPEN_QUESTION_BLOCK_ID, requirementText } from "./issue-layout-seed-text";
import { issuePage, presetCount, presetRow } from "./issue-page";
import { type Gate, OWED_BY } from "./issue-page-t-tree";
import {
  addFinding,
  type ConstraintRead,
  invokeLens,
  type LensFinding,
  type LensInvoked,
  type RiskWritten,
  writeConstraint,
  writeRisk,
} from "./issue-page-w";
import { expandedState } from "./settled";
import type { ApiRequest } from "./traffic";

/**
 * What the rows of `e2e/issue-page-x.e2e.ts` write and read: a node's life - proposed, settled
 * or overtaken - and a node reserved for its author, and the marks, the overtaken rows, the
 * counts and the Settle act the new issue page draws for them
 * (`web/src/features/issue-page/node-states/`). It reuses the readers and writers the other
 * `e2e/issue-page-*.ts` files export, and none of them changes for it. It holds its own copy of
 * one that `e2e/issue-page-w.ts` keeps private: `postAs` mirrors its `write`.
 *
 * - **The writers** `writeNodeState` and `settleNodeApi` send one request each, as a person,
 *   through the server's node state routes (`packages/envoy/internal/dispatch/api/node_state.go`):
 *   the state route takes `{ "state": … }` and the settle route no body, and each names the node
 *   by its whole address, percent-encoded as one path segment (`routeUrl`). Each asserts the
 *   answer it expects: a 200 that leaves the node at the state asked for.
 * - **The seeds**: `seedGateIssue`, one issue of a project of its own that a row reads its gates
 *   on before it writes anything else; and `seedNodeStates` (proposed, settled and overtaken
 *   nodes, an overtaken question with a criterion and a statement under it and a proposed one,
 *   each question's state written by its ask's address, and the agent's decisions that shape
 *   statements 7 and 8 and one that waits for review) and `seedAuthorOnly` (nodes reserved for
 *   their author), each on an issue `seedWholeIssue` made. `seedNodeStates` names each node by
 *   the address the tree read answers alice for it, so it needs the tree read served.
 * - **The readers**: the marks a node carries where the page draws it (`[data-node-mark]`, its
 *   word and its shape) among the outline's rows, an overtaken one among them, and the counts the
 *   page draws - each folded branch's (`data-folded-count`), each filter control's
 *   (`data-preset-count`) and its flag (`data-preset-flag`), the Verification summary's "N of M
 *   covered" and Details' "Referenced by" - beside the same counts made from a page's own tree
 *   read (`heldUnder`, `reservedUnder`, `referencesTo`) and comments read (`marginComments`),
 *   with `viewCount` of `e2e/issue-page-t-tree.ts` for a view's; a question of the spec is named
 *   by the read id the read it is drawn from holds it by (`questionReadId`).
 * - **What the page sent**: the requests to the node state routes (`nodeStateRequests`) and the
 *   address each settle names (`settlesSent`).
 * - **The gates** a row reads once, each holding back the readings that need code not yet under
 *   the head: Details' facts of the focused row (`FOCUSED_FACTS_GATE`) and the Graph control
 *   (`graphGate`), each noted as owed (`noteOwed`) where it is not met.
 */

// ---------------------------------------------------------------------------------------------
// The writers.
// ---------------------------------------------------------------------------------------------

/** What `POST /api/v1/issues/{key}/nodes/{id}/state` and `/settle` answer: the node as the write
 *  leaves it, and the state it held before. */
export type NodeStateWritten = Result<"/api/v1/issues/{key}/nodes/{id}/state", "post">;

/** A value the state route writes: `proposed` and `overtaken` on any node, `live` on one that is
 *  not gone, `gone` on a lens, a finding, a risk or a constraint. */
export type WrittenState = Body<"/api/v1/issues/{key}/nodes/{id}/state", "post">["state"];

/** One `POST` of `path` as `login`, with `body` as JSON where there is one, under an
 *  `Idempotency-Key` of its own, asserted to answer 200; answers its body. */
async function postAs(path: string, login: string, body?: object): Promise<NodeStateWritten> {
  const response = await fetch(new URL(path, baseUrlOf()), {
    ...(body === undefined ? {} : { body: JSON.stringify(body) }),
    headers: {
      ...(await userHeaders(login)),
      ...(body === undefined ? {} : { "Content-Type": "application/json" }),
      "Idempotency-Key": crypto.randomUUID(),
    },
    method: "POST",
  });
  const text = await response.text();
  expect(response.status, `POST ${path} as ${login} answers ${response.status}: ${text}`).toBe(200);
  return JSON.parse(text) as NodeStateWritten;
}

/** Writes the state of the node of issue `key` that `address` names (its whole address) as
 *  `state`, as `login`, and asserts the write left it there. */
export async function writeNodeState(
  key: string,
  address: string,
  state: WrittenState,
  login = "alice"
): Promise<NodeStateWritten> {
  const path = routeUrl("/api/v1/issues/{key}/nodes/{id}/state", { id: address, key });
  const written = await postAs(path, login, { state });
  expect(written.node.state, `the write of ${state} on ${address} left it otherwise`).toBe(state);
  return written;
}

/** Settles the proposed node of issue `key` that `address` names, as `login`, with no body, and
 *  asserts the settle left it settled. */
export async function settleNodeApi(
  key: string,
  address: string,
  login = "alice"
): Promise<NodeStateWritten> {
  const path = routeUrl("/api/v1/issues/{key}/nodes/{id}/settle", { id: address, key });
  const written = await postAs(path, login);
  expect(written.node.state, `the settle of ${address} as ${login} left it otherwise`).toBe(
    "settled"
  );
  return written;
}

// ---------------------------------------------------------------------------------------------
// The tree read, as one reader gets it.
// ---------------------------------------------------------------------------------------------

/** The reads `treeIdOf` names a node by - the issue read and the asks read - as `login` reads
 *  them, so a row names each node by the id the page draws it under. */
export async function treeIdReads(key: string, login = "alice"): Promise<TreeIdReads> {
  const [issue, asks] = await Promise.all([
    getIssue(key, { login }),
    listIssueAsks(key, { login }),
  ]);
  return { asks, issue };
}

/** The comments the page draws in the margin beside a node's text, as `login` reads them: the ids
 *  of the comments the comments read of issue `key` (`GET /api/v1/issues/{key}/comments`)
 *  anchors (`Comment.anchor` non-null), as `tree-read/tree-nodes.ts`' `keptReads` keeps them
 *  (`KeptReads.anchored`) for `onNodeText`. A count reads them as `HeldRow.margin`. */
export async function marginComments(key: string, login = "alice"): Promise<ReadonlySet<string>> {
  const comments = await listComments(key, undefined, { login });
  return new Set(comments.flatMap((comment) => (comment.anchor == null ? [] : [comment.id])));
}

/** Whether `node` is a node of issue `key` itself that the read did not bring in as context. */
function ownOf(node: IssueTreeNode, key: string): boolean {
  return (node.issue === undefined || node.issue === key) && node.role !== "context";
}

/** The node of issue `key` of kind `supertag` the seed named `id`: a typed block by its block id
 *  (`block_id`), any other node by its read id. Throws, naming `what`, where the answer holds
 *  none. */
export function seededNode(
  answer: IssueTreeRead,
  key: string,
  supertag: string,
  id: string,
  what: string
): IssueTreeNode {
  const node = answer.nodes?.find(
    (one) => blockIdOfNode(one) === id && one.supertag === supertag && ownOf(one, key)
  );
  if (node === undefined) throw new Error(`the tree read of ${key} holds no ${what} (${id})`);
  return node;
}

/** The node's whole address, as the tree read gives it: a typed block's in the document its
 *  `document` names (as `tree-read/tree-nodes.ts`' `referenceOf` addresses one). */
export function addressIn(node: IssueTreeNode, key: string): string {
  if (node.document === undefined) {
    throw new Error(`the tree read names no document for ${node.supertag} ${node.id}`);
  }
  return buildDispatchReference({
    block: { id: blockIdOfNode(node) },
    key: node.issue ?? key,
    kind: "artifact",
    slug: node.document.slug,
  });
}

/** The first node of `answer` whose words or attributes hold `word`: one of the words a seed
 *  wrote into one node and no other. */
export function nodeHolding(answer: IssueTreeRead, word: string): IssueTreeNode | undefined {
  return answer.nodes?.find(
    (node) =>
      node.text.includes(word) ||
      Object.values(node.attributes ?? {}).some(
        (value) => typeof value === "string" && value.includes(word)
      )
  );
}

/** The id of the ask the server indexed the question `question` of `seeded` from its block: the
 *  read id of its ask's node, and what `/asks/<id>` names it by. */
function askIdOf(seeded: NodeStateSeed, question: SpecQuestion): string {
  const asks: Readonly<Record<SpecQuestion, string>> = {
    openQuestion: seeded.whole.openQuestion.askId,
    overtakenQuestion: seeded.overtakenAskId,
    proposedQuestion: seeded.proposedAskId,
  };
  return asks[question];
}

/**
 * The read id `answer` holds the question `question` of `seeded` by: the read id the row the page
 * draws for it from `answer` carries (`data-node-read-id`), and the one `treeIdOf(answer, …)` of
 * `tree-read/tree-nodes.ts` maps to the question's page id (`<slug>#b-<block id>`), where it maps
 * a read id `answer` does not hold to itself. A question written as an `ask` block is two nodes of
 * the whole tree read, the block and the ask the server indexed from it, which the page draws as
 * one row carrying the block's read id. Conversation's read names its kinds (`queries.ts`'
 * `CONVERSATION_KINDS`), and the server's `kind` filter takes for `ask` the ask's node and never
 * an ask block (`issue_tree_read.go`'s `issueTreeQuery.Kind`), so Conversation draws the question
 * from its ask's node alone; nor does that read hold a result, a plan task or a constraint, which
 * are none of its kinds. Graph's read names no kind and holds the block, as the whole tree read
 * (`readTree(key, null)`, `NodeStateSeed.answer`) does. So: the block's read id where `answer`
 * holds the block, else its ask's.
 */
export function questionReadId(
  answer: IssueTreeRead,
  seeded: NodeStateSeed,
  question: SpecQuestion
): string {
  const block = seeded.ids[question];
  return (answer.nodes ?? []).some((node) => node.id === block) ? block : askIdOf(seeded, question);
}

/** A tree read's nodes by the read `parent` each stands under. */
type Children = ReadonlyMap<string, readonly IssueTreeNode[]>;

/** Each tree read's `Children`, made once per answer. */
const childrenByAnswer = new WeakMap<IssueTreeRead, Children>();

/** The `Children` of `answer`, made at its first reading. */
function childrenIn(answer: IssueTreeRead): Children {
  const made = childrenByAnswer.get(answer);
  if (made !== undefined) return made;
  const children = new Map<string, IssueTreeNode[]>();
  for (const node of answer.nodes ?? []) {
    if (node.parent === null) continue;
    const siblings = children.get(node.parent.id);
    if (siblings === undefined) children.set(node.parent.id, [node]);
    else siblings.push(node);
  }
  childrenByAnswer.set(answer, children);
  return children;
}

/** The kinds of a block of an ask, a comment or a message: the page draws such a block inside
 *  its item's row and no row of its own, and a reply to it under that item's row. */
const ITEM_BLOCKS: Readonly<Record<string, true>> = {
  ask_block: true,
  comment_block: true,
  message_block: true,
};

/** The kinds of node `seedNodeStates` writes that no step of `tree-read/tree-nodes.ts`'s
 *  `treeDraftSteps` drafts at this head, so the page draws no row for one and counts none in a
 *  branch: a criterion, which part Y (INFRA-4066) draws. A reading of a count leaves them out
 *  (`HeldRow.undrawn`) until a gate finds the page drawing the kind. */
export const UNDRAWN_KINDS: Readonly<Record<string, true>> = { criterion: true };

/** The kinds Conversation draws a row of only where a row stands under one (root R9), as
 *  `presets.ts`' `NO_CONVERSATION_ROW` and `conversationView`'s prune keep them: a plan task and a
 *  result. Every count reading of part X's rows is made on Conversation, at an address that names
 *  no such node, so a count counts one only where a node it counts stands under it, and walks on
 *  through it to what stands under it either way. */
const NO_CONVERSATION_ROW: Readonly<Record<string, true>> = { plan_task: true, result: true };

/** What a count of the nodes under a row reads besides the row's own node. */
export interface HeldRow {
  /** The ids of the comments the reader's comments read anchors (`marginComments`). As
   *  `tree-read/tree-nodes.ts`' `onNodeText` reads one, a comment of them the read hangs under a
   *  node other than the issue's own is on that node's text: the page draws it, and the replies
   *  in its thread, in the margin beside that node and no row of any (R9, R10, root R41), and a
   *  node of another kind under it has no row above it to stand under, so `issue-tree.ts`'
   *  `assemble` places it under the issue. None of them counts in a branch. */
  readonly margin?: ReadonlySet<string>;
  /** The kinds the page draws no row for: a node of one is no row and counts none, and the
   *  nodes under it stand under the row above it, as the page places them. */
  readonly undrawn?: Readonly<Record<string, true>>;
}

/** Every node of issue `key` in `answer` under the node `readId`, at any depth, by the read's
 *  `parent`, that Conversation draws as a row: the tree each count walks. A question is one node
 *  of Conversation's read, its ask's (`questionReadId`), so its branch is that node's alone.
 *  The walk passes through a node the page draws no row for - a block of an ask, a comment or a
 *  message, which the page draws inside its item's row, or a node of an `undrawn` kind - to what
 *  stands under it, and through a plan task or a result (`NO_CONVERSATION_ROW`), which it counts
 *  only where a node it counts stands under it. It stops at a comment on a node's text (`margin`,
 *  by `tree-read/tree-nodes.ts`' `onNodeText`): neither it nor anything under it stands in the
 *  branch. */
function underIn(
  answer: IssueTreeRead,
  key: string,
  readId: string,
  { margin, undrawn }: HeldRow = {}
): IssueTreeNode[] {
  const children = childrenIn(answer);
  const seen = new Set<string>([readId]);
  const walk = (id: string): IssueTreeNode[] => {
    const found: IssueTreeNode[] = [];
    for (const child of children.get(id) ?? []) {
      if (!ownOf(child, key) || seen.has(child.id)) continue;
      seen.add(child.id);
      if (
        child.supertag === "comment" &&
        margin?.has(child.id) === true &&
        child.parent?.supertag !== "issue"
      ) {
        continue;
      }
      const under = walk(child.id);
      if (
        ITEM_BLOCKS[child.supertag] !== true &&
        undrawn?.[child.supertag] !== true &&
        (NO_CONVERSATION_ROW[child.supertag] !== true || under.length > 0)
      ) {
        found.push(child);
      }
      found.push(...under);
    }
    return found;
  };
  return walk(readId);
}

/** How many nodes the branch of `readId` holds in `answer` that Conversation draws: what its row's
 *  folded count counts there. */
export function heldUnder(
  answer: IssueTreeRead,
  key: string,
  readId: string,
  row: HeldRow = {}
): number {
  return underIn(answer, key, readId, row).length;
}

/** How many of the nodes `reserved` names stand in the branch of `readId` in `answer`. */
export function reservedUnder(
  answer: IssueTreeRead,
  key: string,
  readId: string,
  reserved: ReadonlySet<string>
): number {
  return underIn(answer, key, readId).filter((node) => reserved.has(node.id)).length;
}

/** How many tuples of `answer` reach the node `readId` from a node the read holds: what a
 *  reader of that read sees reference it. */
export function referencesTo(answer: IssueTreeRead, readId: string): number {
  const held = new Set((answer.nodes ?? []).map((node) => node.id));
  return (answer.tuples ?? []).filter((tuple) => tuple.to.id === readId && held.has(tuple.from.id))
    .length;
}

// ---------------------------------------------------------------------------------------------
// The issue a row reads its gates on.
// ---------------------------------------------------------------------------------------------

/** The project of the issue a row reads its gates on, which no seed writes into. */
const GATE_PROJECT = "ACME";

/** Writes one issue in a project of its own and answers its key: a row reads its gates on it -
 *  whether the tree read is served, and whether an issue is born converted - before it writes
 *  its own issue, so a row that skips on a gate writes nothing it does not read. */
export async function seedGateIssue(): Promise<string> {
  await createProject({ key: GATE_PROJECT, name: "Gates" });
  const issue = await createIssue({
    force: true,
    project: GATE_PROJECT,
    title: "The tree read is served for this issue",
  });
  return issue.key;
}

// ---------------------------------------------------------------------------------------------
// The seed of the proposed, settled and overtaken nodes.
// ---------------------------------------------------------------------------------------------

/** Statement 9's words once `seedNodeStates` wrote them: they hold the word "proposed", while the
 *  node stays live, so a page that reads a node's life from its words draws a mark on statement 9
 *  that its state does not give it. */
export const STATEMENT_9_WORDS =
  "Requirement 9 is proposed in its words alone, and its state is live.";

/** The one word statement 8's words hold once `seedNodeStates` wrote them, and no other node of
 *  the issue holds: the words filter with it draws statement 8 alone. */
export const STATEMENT_8_WORD = "saltmarsh";

/** Statement 8's words once `seedNodeStates` wrote them. */
const STATEMENT_8_WORDS = `Requirement 8 is said better by a later statement, and only this one names the ${STATEMENT_8_WORD}.`;

/** The block ids of the agent's decisions `seedNodeStates` writes into the spec: the pointer route
 *  takes no `shaped` tuple, so each decision is a block of the spec's `## Decisions` part carrying
 *  its `shaped` pointer. The first two shape statements 7 and 8, and each one's row draws a chip
 *  naming its statement; the third shapes statement 10, waits for review and is made overtaken. */
export const SHAPING_DECISION_ID = "d-shapes-r7";
export const SHAPING_DECISION_8_ID = "d-shapes-r8";
export const OVERTAKEN_DECISION_ID = "d-overtaken";

/** What those decisions say. */
const SHAPING_DECISION_WORDS = "Settle statement 7 once a person has read the proposal.";
const SHAPING_DECISION_8_WORDS = "Keep statement 8 only while no later statement says it better.";
const OVERTAKEN_DECISION_WORDS =
  "Draw statement 10 as it stands until a person has read the proposal.";

/** The block id of the `ask` block `seedNodeStates` writes into the spec right after the spec's own
 *  question, which the server indexes into an open question that waits on alice, the issue's
 *  assignee, and whose ask's node is made overtaken. */
export const OVERTAKEN_QUESTION_ID = "q-overtaken";

/** That question, as the spec holds it. */
const OVERTAKEN_QUESTION_BLOCK = `:::ask{#${OVERTAKEN_QUESTION_ID} urgency="med"}
Which width does the Contents pane keep once a later question has settled this one?

- 1024 px: the pane begins there.
- 1280 px: the pane keeps its labels.
:::`;

/** The block id of the `ask` block `seedNodeStates` writes right after the overtaken question's,
 *  which the server indexes into a second open question that waits on alice, and whose ask's node
 *  is made proposed. */
export const PROPOSED_QUESTION_ID = "q-put-forward";

/** That question, as the spec holds it: words of its own, which hold no word of a node's life. */
const PROPOSED_QUESTION_BLOCK = `:::ask{#${PROPOSED_QUESTION_ID} urgency="med"}
Which order does the Decisions view give two questions asked on the same day?

- Oldest first: the view keeps the order they were asked in.
- Newest first: the view keeps the latest question on top.
:::`;

/** The criterion and the statement `seedNodeStates` writes under the overtaken question, each with
 *  that question as its `parent`, and what each says. */
const QUESTION_CRITERION_ID = "k-overtaken";
const QUESTION_STATEMENT_ID = "r-overtaken";
const QUESTION_CRITERION_WORDS =
  "I want every question that was asked to stay in the Contents list.";
const QUESTION_STATEMENT_WORDS = "The Contents list names a question that a later one overtook.";

/** The verdict of the lens `seedNodeStates` invokes on statement 8, by which the read names it. */
const STATEMENT_8_VERDICT = "Statement 8 says what a later statement says better.";

/** The nodes `seedNodeStates` names, each by its read id. */
export interface NodeStateIds {
  /** The second check, failed at the seed's head, which is proposed. */
  readonly check2: string;
  /** The fourth check, failing at the seed's head, which is overtaken. */
  readonly check4: string;
  /** The first decision of the spec's `## Decisions` part, which is proposed. */
  readonly decision1: string;
  /** The lens on statement 8. */
  readonly lens: string;
  /** The open question the spec's own `ask` block was indexed into, by its block
   *  (`OPEN_QUESTION_BLOCK_ID`), which waits on alice and stays live. A read that holds its ask
   *  alone holds it by its ask's id (`questionReadId`). */
  readonly openQuestion: string;
  /** The block `OVERTAKEN_QUESTION_ID`, by which a read that holds it draws its question's one
   *  row (`questionReadId`): the block stays live, and the ask it was indexed into
   *  (`NodeStateSeed.overtakenAskId`), which waits on alice, is overtaken. */
  readonly overtakenQuestion: string;
  readonly planTask1: string;
  readonly planTask2: string;
  /** The block `PROPOSED_QUESTION_ID`, by which a read that holds it draws its question's one
   *  row (`questionReadId`): the block stays live, and the ask it was indexed into
   *  (`NodeStateSeed.proposedAskId`), which waits on alice, is proposed. */
  readonly proposedQuestion: string;
  /** The criterion written under the overtaken question. */
  readonly questionCriterion: string;
  /** The statement written under the overtaken question. */
  readonly questionStatement: string;
  /** The agent's decisions that shape statements 7 and 8 (`SHAPING_DECISION_ID`,
   *  `SHAPING_DECISION_8_ID`), which stay live. */
  readonly shapingDecision: string;
  readonly shapingDecision8: string;
  readonly statement7: string;
  readonly statement8: string;
  readonly statement9: string;
  /** The statement the overtaken decision shapes, which no other decision names. */
  readonly statement10: string;
  /** The agent's decision that waits for review (`OVERTAKEN_DECISION_ID`), which is overtaken. */
  readonly waitingDecision: string;
}

/** A question of the spec `seedNodeStates` names by its block (`NodeStateIds`), written as an
 *  `ask` block the server indexed an ask from. */
export type SpecQuestion = "openQuestion" | "overtakenQuestion" | "proposedQuestion";

/** The nodes whose state `seedNodeStates` writes by their own address, by the state each is
 *  written at. */
export const PROPOSED_NODES = ["statement7", "check2", "decision1", "planTask1"] as const;
export const OVERTAKEN_NODES = ["statement8", "planTask2", "check4", "waitingDecision"] as const;

/** The questions whose state `seedNodeStates` writes by their ask's address, by the state each is
 *  written at. A question written as an `ask` block is two nodes of the tree read, the block and
 *  the ask indexed from it: the ask's node is the question, whose state the page draws on the
 *  question's one row, and the block is only its place in the spec, which stays live. */
export const QUESTIONS = { overtakenQuestion: "overtaken", proposedQuestion: "proposed" } as const;

/** One node `seedNodeStates` writes the state of. */
type StatedNode =
  | (typeof PROPOSED_NODES)[number]
  | (typeof OVERTAKEN_NODES)[number]
  | keyof typeof QUESTIONS;

/** What `seedNodeStates` wrote on the issue `whole` made. */
export interface NodeStateSeed {
  /** Each node whose state it wrote, and statement 9, by its address in the tree read: each
   *  question's by its ask's address (`dispatch://<KEY>/ask/<id>`), which names the ask's node. */
  readonly addresses: Readonly<Record<StatedNode | "statement9", string>>;
  /** Alice's whole tree read once every state was written (`readTree(key, null)`): it names no
   *  kind, so it holds every node Graph's read holds, each question's block among them
   *  (`questionReadId`). */
  readonly answer: IssueTreeRead;
  readonly ids: NodeStateIds;
  readonly lens: LensInvoked;
  /** The id of the ask the overtaken question was indexed into: the read id of its ask's node, and
   *  what `/asks/<id>` names it by. */
  readonly overtakenAskId: string;
  /** The id of the ask the proposed question was indexed into: the read id of its ask's node, and
   *  what `/asks/<id>` names it by. */
  readonly proposedAskId: string;
  readonly whole: WholeIssueSeed;
}

/** How long a seed waits for what it wrote to stand in a read. */
const SEED_READ_MS = 20_000;

/** Polls `read` until `holds` is true of its answer, and answers it; throws `message` past the
 *  seed's deadline. */
async function readUntil<T>(
  read: () => Promise<T>,
  holds: (answer: T) => boolean,
  message: string
): Promise<T> {
  const deadline = Date.now() + SEED_READ_MS;
  for (;;) {
    const answer = await read();
    if (holds(answer)) return answer;
    if (Date.now() > deadline) throw new Error(message);
    await pause(100);
  }
}

/**
 * Writes, on the issue `whole` that `seedWholeIssue` made: statement 9's words holding
 * "proposed" while it stays live, and statement 8's holding one word of their own; a failing
 * result on check 4, recorded at the commit the seed's own results name and read Failed before
 * anything relies on it, so a flag that counted an overtaken check would count it; the agent's
 * decisions of the spec, the first two shaping statements 7 and 8, so each decision's row draws a
 * chip naming its statement, and the third waiting for review; two open questions that wait on
 * alice, the first with a criterion and a statement written under it; a lens on statement 8
 * (`invokeLens`); statement 7, check 2, the first decision and plan task 1 proposed; statement
 * 8, plan task 2, check 4 and the third decision overtaken; and the first question overtaken and
 * the second proposed, each by its ask's address, its block left live. Each node is named by the
 * address alice's tree read answers for it, so it runs after a row's gates found that read
 * served; it asserts the read answers each state it wrote, on a question's ask's node and not on
 * its block's, both statements' words, and the criterion and the statement under the question,
 * which the coverage read holds with no check serving it.
 */
export async function seedNodeStates(whole: WholeIssueSeed): Promise<NodeStateSeed> {
  const key = whole.key;
  const coverage = await getIssueCoverage(key);
  const statementRef = (block: string | undefined, what: string): string => {
    const ref = coverage.requirements?.find((requirement) => requirement.block_id === block)?.ref;
    if (ref === undefined) throw new Error(`the coverage read of ${key} holds no ${what}`);
    return ref;
  };
  for (const [index, words] of [
    [9, STATEMENT_9_WORDS],
    [8, STATEMENT_8_WORDS],
  ] as const) {
    const reworded = await editArtifact(whole.specArtifactId, {
      ops: [{ find: requirementText(index), op: "replace", with: words }],
    });
    expect(reworded.applied, `the spec does not take statement ${index}'s words`).toBe(1);
  }
  const decisions = [
    [SHAPING_DECISION_ID, statementRef(whole.requirements[6], "r7"), SHAPING_DECISION_WORDS],
    [SHAPING_DECISION_8_ID, statementRef(whole.requirements[7], "r8"), SHAPING_DECISION_8_WORDS],
    [OVERTAKEN_DECISION_ID, statementRef(whole.requirements[9], "r10"), OVERTAKEN_DECISION_WORDS],
  ] as const;
  const decided = await editArtifact(
    whole.specArtifactId,
    {
      ops: [
        {
          after: `block:${OPEN_QUESTION_BLOCK_ID}`,
          markdown: `## Decisions\n\n${decisions
            .map(([id, ref, words]) => `:::decision{#${id} shaped="${ref}"}\n${words}\n:::`)
            .join("\n\n")}`,
          op: "insert",
        },
      ],
    },
    asPlanner
  );
  expect(decided.applied, "the spec does not take the agent's decisions").toBe(1);
  // The questions go in right after the spec's own, so the overtaken one stands next below it among
  // the spec's blocks, and `q` from the one reaches the other; the proposed one stands right after
  // the overtaken one.
  const asked = await editArtifact(
    whole.specArtifactId,
    {
      ops: [
        {
          after: `block:${OPEN_QUESTION_BLOCK_ID}`,
          markdown: `${OVERTAKEN_QUESTION_BLOCK}\n\n${PROPOSED_QUESTION_BLOCK}`,
          op: "insert",
        },
      ],
    },
    asPlanner
  );
  expect(asked.applied, "the spec does not take the questions made overtaken and proposed").toBe(1);
  // Both questions go in in one edit, so one read of the issue that holds both their asks names
  // each, and whom the proposed one waits on.
  const [overtakenAsk, proposedAsk] = await readUntil(
    async () => {
      const open = (await getIssue(key)).open_asks ?? [];
      return [OVERTAKEN_QUESTION_ID, PROPOSED_QUESTION_ID].map((block) =>
        open.find((ask) => ask.block_id === block)
      );
    },
    (found) => found.every((ask) => ask !== undefined),
    `the issue read of ${key} never holds the asks of the questions made overtaken and proposed`
  );
  const overtakenAskId = overtakenAsk?.id ?? "";
  const proposedAskId = proposedAsk?.id ?? "";
  expect(
    { on: proposedAsk?.waiting_on, person: proposedAsk?.waiting_on_person },
    "the second question is no open question that waits on alice"
  ).toEqual({ on: "human", person: "alice" });
  // A question's address is its ask's (`dispatch://<KEY>/ask/<id>`), which names the ask's node:
  // the node a `parent` pointer takes (`conversationKinds`), and the one whose state the page
  // draws on the question's one row, drawn from its block where the read holds the block
  // (`questionReadId`).
  const askAddress = (id: string) => buildDispatchReference({ id, key, kind: "ask" });
  const parent = askAddress(overtakenAskId);
  const under = await editArtifact(
    whole.specArtifactId,
    {
      ops: [
        {
          after: "end",
          markdown:
            `:::criterion{#${QUESTION_CRITERION_ID} parent="${parent}"}\n${QUESTION_CRITERION_WORDS}\n:::\n\n` +
            `:::requirement{#${QUESTION_STATEMENT_ID} parent="${parent}"}\n${QUESTION_STATEMENT_WORDS}\n:::`,
          op: "insert",
        },
      ],
    },
    asPlanner
  );
  expect(
    under.applied,
    "the spec does not take the criterion and the statement under the question"
  ).toBe(1);
  // No check serves the statement under the question, so live it would draw "No check": row 4
  // reads that under the overtaken question it draws no gap.
  await readUntil(
    () => getIssueCoverage(key),
    (read) =>
      (read.requirements ?? []).some(
        (requirement) => requirement.block_id === QUESTION_STATEMENT_ID && requirement.checks === 0
      ),
    "the coverage read never holds the statement under the question with no check serving it"
  );

  await recordCheckResult(
    key,
    {
      check: whole.checks.older,
      commit: HEAD_SHA,
      failed: 1,
      negative_control: "Removed the row's state and the run failed.",
      observed: "1 failed",
      role: "tester",
      run: "bun run e2e -- e2e/issue-page-x.e2e.ts",
      surface: "browser",
      verdict: "fail",
    },
    { actor: recorderActor, as: "agent" }
  );
  await readUntil(
    () => getIssueResults(key),
    (read) =>
      (read.checks ?? []).some(
        (check) => check.block_id === whole.checks.older && checkStanding(check).state === "failed"
      ),
    "check 4 never reads Failed after its failing result, so no flag reading can rely on it"
  );

  const written = [
    SHAPING_DECISION_ID,
    SHAPING_DECISION_8_ID,
    OVERTAKEN_DECISION_ID,
    OVERTAKEN_QUESTION_ID,
    PROPOSED_QUESTION_ID,
    QUESTION_CRITERION_ID,
    QUESTION_STATEMENT_ID,
  ];
  const asks = [overtakenAskId, proposedAskId];
  const before = await readUntil(
    () => readTree(key, null),
    (read) => {
      const held = read.nodes ?? [];
      return (
        written.every((id) => held.some((node) => blockIdOfNode(node) === id)) &&
        asks.every((id) => held.some((node) => node.id === id)) &&
        held.some((node) => node.text.includes(STATEMENT_9_WORDS)) &&
        held.some((node) => node.text.includes(STATEMENT_8_WORDS))
      );
    },
    `the tree read of ${key} never holds the statements' new words, the agent's decisions, and the two questions' blocks and asks with what stands under the first`
  );
  expect(before.converted, "the seeded issue's tree read does not answer converted").toBe(true);
  await readUntil(
    () => getIssueDecisions(key),
    (read) =>
      (read.decisions ?? []).some(
        (decision) =>
          decision.block_id === OVERTAKEN_DECISION_ID &&
          decision.kind === "agent" &&
          decision.waiting_review
      ),
    "the decisions read never holds the agent's decision that waits for review"
  );
  const node = (supertag: string, id: string | undefined, what: string) =>
    seededNode(before, key, supertag, id ?? "", what);
  const nodes = {
    check2: node("check", whole.checks.failed, "check 2"),
    check4: node("check", whole.checks.older, "check 4"),
    decision1: node("decision", whole.decisions[0], "the first decision"),
    openQuestion: node("ask", OPEN_QUESTION_BLOCK_ID, "the spec's open question"),
    overtakenQuestion: node("ask", OVERTAKEN_QUESTION_ID, "the question that is made overtaken"),
    planTask1: node("plan_task", whole.planTasks[0], "plan task 1"),
    planTask2: node("plan_task", whole.planTasks[1], "plan task 2"),
    proposedQuestion: node("ask", PROPOSED_QUESTION_ID, "the question that is made proposed"),
    questionCriterion: node("criterion", QUESTION_CRITERION_ID, "the criterion under the question"),
    questionStatement: node(
      "requirement",
      QUESTION_STATEMENT_ID,
      "the statement under the question"
    ),
    shapingDecision: node("decision", SHAPING_DECISION_ID, "the decision that shapes statement 7"),
    shapingDecision8: node(
      "decision",
      SHAPING_DECISION_8_ID,
      "the decision that shapes statement 8"
    ),
    statement10: node("requirement", whole.requirements[9], "statement 10"),
    statement7: node("requirement", whole.requirements[6], "statement 7"),
    statement8: node("requirement", whole.requirements[7], "statement 8"),
    statement9: node("requirement", whole.requirements[8], "statement 9"),
    waitingDecision: node("decision", OVERTAKEN_DECISION_ID, "the decision that waits for review"),
  };
  const addresses = {
    check2: addressIn(nodes.check2, key),
    check4: addressIn(nodes.check4, key),
    decision1: addressIn(nodes.decision1, key),
    overtakenQuestion: askAddress(overtakenAskId),
    planTask1: addressIn(nodes.planTask1, key),
    planTask2: addressIn(nodes.planTask2, key),
    proposedQuestion: askAddress(proposedAskId),
    statement7: addressIn(nodes.statement7, key),
    statement8: addressIn(nodes.statement8, key),
    statement9: addressIn(nodes.statement9, key),
    waitingDecision: addressIn(nodes.waitingDecision, key),
  };

  const lens = await invokeLens(key, {
    kind: "trade-off",
    on: addresses.statement8,
    verdict: STATEMENT_8_VERDICT,
  });
  for (const name of PROPOSED_NODES) await writeNodeState(key, addresses[name], "proposed");
  for (const name of OVERTAKEN_NODES) await writeNodeState(key, addresses[name], "overtaken");
  const askIds: Readonly<Record<keyof typeof QUESTIONS, string>> = {
    overtakenQuestion: overtakenAskId,
    proposedQuestion: proposedAskId,
  };
  for (const name of Object.keys(QUESTIONS) as (keyof typeof QUESTIONS)[]) {
    await writeNodeState(key, addresses[name], QUESTIONS[name]);
  }

  const answer = await readTree(key, null);
  const stateOf = (id: string) => answer.nodes?.find((one) => one.id === id)?.state;
  for (const name of PROPOSED_NODES) {
    expect(stateOf(nodes[name].id), `the tree read does not answer ${name} proposed`).toBe(
      "proposed"
    );
  }
  for (const name of OVERTAKEN_NODES) {
    expect(stateOf(nodes[name].id), `the tree read does not answer ${name} overtaken`).toBe(
      "overtaken"
    );
  }
  for (const name of Object.keys(QUESTIONS) as (keyof typeof QUESTIONS)[]) {
    const state = QUESTIONS[name];
    expect(
      stateOf(askIds[name]),
      `the tree read does not answer ${name}'s ask's node ${state}`
    ).toBe(state);
    expect(stateOf(nodes[name].id), `the tree read does not answer ${name}'s block live`).toBe(
      "live"
    );
  }
  const statement9 = answer.nodes?.find((one) => one.id === nodes.statement9.id);
  expect(statement9?.text, "the tree read does not answer statement 9's words").toContain(
    STATEMENT_9_WORDS
  );
  expect(statement9?.state, "the tree read does not answer statement 9 live").toBe("live");
  const lensNode = answer.nodes?.find(
    (one) => one.supertag === "lens" && one.attributes?.verdict === STATEMENT_8_VERDICT
  );
  expect(lensNode?.parent?.id, "the tree read does not stand the lens under statement 8").toBe(
    nodes.statement8.id
  );
  // The criterion and the statement name the question by its ask's address, so the read stands
  // each under the ask's node (the node its `part_of` names), which the page draws as the
  // question's one row, drawn from its block where the read holds the block (`questionReadId`).
  for (const [id, what] of [
    [nodes.questionCriterion.id, "criterion"],
    [nodes.questionStatement.id, "statement"],
  ] as const) {
    expect(
      answer.nodes?.find((one) => one.id === id)?.parent?.id,
      `the tree read does not stand the ${what} under the overtaken question's ask`
    ).toBe(overtakenAskId);
  }
  // The coverage read draws no requirement's gap while a document of the issue owes its block
  // index (the server's `owedGap`, which writes one line in place of every missing check), and
  // the block-index reconciliation writes an owed index on its own tick. So the seed waits until
  // no document owes one, and each row reads a coverage read that holds every gap: statement 9's
  // "No check" among them.
  await expect
    .poll(async () => (await getIssueCoverage(key)).owed_documents ?? [], {
      message: "a document of the seeded issue still owes its block index",
      timeout: 90_000,
    })
    .toEqual([]);
  return {
    addresses,
    answer,
    ids: {
      check2: nodes.check2.id,
      check4: nodes.check4.id,
      decision1: nodes.decision1.id,
      lens: lensNode?.id ?? "",
      openQuestion: nodes.openQuestion.id,
      overtakenQuestion: nodes.overtakenQuestion.id,
      planTask1: nodes.planTask1.id,
      planTask2: nodes.planTask2.id,
      proposedQuestion: nodes.proposedQuestion.id,
      questionCriterion: nodes.questionCriterion.id,
      questionStatement: nodes.questionStatement.id,
      shapingDecision: nodes.shapingDecision.id,
      shapingDecision8: nodes.shapingDecision8.id,
      statement10: nodes.statement10.id,
      statement7: nodes.statement7.id,
      statement8: nodes.statement8.id,
      statement9: nodes.statement9.id,
      waitingDecision: nodes.waitingDecision.id,
    },
    lens,
    overtakenAskId,
    proposedAskId,
    whole,
  };
}

// ---------------------------------------------------------------------------------------------
// The seed of the nodes reserved for their author.
// ---------------------------------------------------------------------------------------------

/** The one word each node of `seedAuthorOnly` holds and no other node of the issue does. */
export const NODE_WORDS = {
  /** Her reserved constraint. */
  constraint: "Tamarisk",
  /** The finding under her reserved lens. */
  finding: "Ocelot",
  /** The risk under her reserved pre-mortem written with no visibility of its own, which is hers
   *  as that pre-mortem is. */
  inheritedRisk: "Sorrel",
  /** Her reserved lens on statement 2. */
  lens: "Halyard",
  /** Her reserved pre-mortem on statement 3. */
  preMortem: "Juniper",
  /** The project lens on statement 2, triggered by her reserved lens. */
  projectLens: "Bramble",
  /** Her reserved risk under her reserved pre-mortem, which check 3 prevents. */
  risk: "Kestrel",
} as const;

/** A word no node of the issue holds, whose words filter is what a reserved node's words filter
 *  draws for a reader who may not see it. */
export const NOBODY_WROTE = "Quokka";

/** The nodes reserved for their author, alice: the risk under her reserved pre-mortem by that
 *  pre-mortem's visibility, which it takes for want of its own, and every other by its own. */
export const RESERVED_NODES = [
  "lens",
  "finding",
  "preMortem",
  "risk",
  "inheritedRisk",
  "constraint",
] as const;

/** One node `seedAuthorOnly` wrote: its address, as the write answered it, and its one word. */
export interface WrittenNode {
  readonly address: string;
  readonly word: string;
}

/** What `seedAuthorOnly` wrote on the issue `whole` made. */
export interface AuthorOnlySeed {
  readonly nodes: Readonly<Record<keyof typeof NODE_WORDS, WrittenNode>>;
  readonly whole: WholeIssueSeed;
}

/** The answer of one write of `seedAuthorOnly`, asserted to be reserved for its author or not. */
function written(
  answer: LensInvoked | LensFinding | RiskWritten | ConstraintRead,
  name: keyof typeof NODE_WORDS,
  visibility: "author" | "project"
): WrittenNode {
  expect(answer.visibility, `the ${name} is not written for ${visibility}`).toBe(visibility);
  return { address: answer.address, word: NODE_WORDS[name] };
}

/**
 * Writes, on an issue `seedWholeIssue` makes, as alice: reserved for her (`visibility:
 * "author"`), a lens on statement 2 with a finding, a pre-mortem on statement 3 with a risk that
 * check 3 prevents, and a constraint; under that pre-mortem, a risk with no visibility of its own,
 * which the server answers as hers, since a child is never wider than its parent; and for the
 * project, a lens on statement 2 triggered by her reserved lens. Each node holds one word no
 * other node holds (`NODE_WORDS`), and is returned with the address its write answered. Each
 * statement's and the check's address is the server's own (the coverage read's and the results
 * read's `ref`).
 */
export async function seedAuthorOnly(): Promise<AuthorOnlySeed> {
  const whole = await seedWholeIssue();
  const key = whole.key;
  const [coverage, results] = await Promise.all([getIssueCoverage(key), getIssueResults(key)]);
  const statement = (block: string | undefined): string => {
    const ref = coverage.requirements?.find((requirement) => requirement.block_id === block)?.ref;
    if (ref === undefined) throw new Error(`the coverage read of ${key} holds no ${block}`);
    return ref;
  };
  const two = statement(whole.requirements[1]);
  const three = statement(whole.requirements[2]);
  const check3 = results.checks?.find((check) => check.block_id === whole.checks.noResult)?.ref;
  if (check3 === undefined) throw new Error(`the results read of ${key} holds no check 3`);
  const author = { visibility: "author" } as const;
  const W = NODE_WORDS;

  const lens = await invokeLens(key, {
    kind: "trade-off",
    on: two,
    verdict: `${W.lens} holds the reasons she keeps to herself.`,
    ...author,
  });
  const finding = await addFinding(
    key,
    lens.address,
    `${W.finding} marks what she found and keeps.`,
    author
  );
  const preMortem = await invokeLens(key, {
    kind: "pre-mortem",
    on: three,
    verdict: `${W.preMortem} is the failure she imagines alone.`,
    ...author,
  });
  const risk = await writeRisk(key, {
    early_warning: "A reader sees a branch count that names nothing drawn.",
    heading: `${W.risk} is the risk she keeps to herself.`,
    lens: preMortem.address,
    prevented_by: check3,
    prevention: "Each count is made from what the reader's own read holds.",
    ...author,
  });
  const constraint = await writeConstraint(
    key,
    {
      measurement: `${W.constraint} rows drawn on her first screen`,
      taken_where: "a 1440 by 900 desktop window on the seeded issue",
      value: "40",
    },
    author
  );
  const projectLens = await invokeLens(key, {
    kind: "pre-mortem",
    on: two,
    triggered_by: lens.address,
    verdict: `${W.projectLens} is the lens every reader of the project reads.`,
  });
  const inheritedRisk = await writeRisk(key, {
    early_warning: "A reader who may not see the pre-mortem reads a risk with no lens above it.",
    heading: `${W.inheritedRisk} is the risk she keeps with the pre-mortem above it.`,
    lens: preMortem.address,
    prevention: "The risk is no wider than the pre-mortem above it.",
  });
  return {
    nodes: {
      constraint: written(constraint, "constraint", "author"),
      finding: written(finding, "finding", "author"),
      inheritedRisk: written(inheritedRisk, "inheritedRisk", "author"),
      lens: written(lens, "lens", "author"),
      preMortem: written(preMortem, "preMortem", "author"),
      projectLens: written(projectLens, "projectLens", "project"),
      risk: written(risk, "risk", "author"),
    },
    whole,
  };
}

// ---------------------------------------------------------------------------------------------
// The readers.
// ---------------------------------------------------------------------------------------------

/** One mark the page draws for a node (`NodeMarks.tsx`): its token (`data-node-mark`), its word,
 *  the token of its shape (`data-glyph`), and whether that shape is drawn with a box and a
 *  colour, which forced colours must leave it. */
export interface DrawnMark {
  readonly mark: string | null;
  readonly shape: string | null;
  readonly shown: boolean;
  readonly word: string;
}

/** What draws a node of its own, or names one inside another's row: a mark inside one of them is
 *  that node's, and no mark of the element around it. */
const MARK_HOLDERS =
  "[data-node-row], [data-verification-check], [data-plan-bar-row], [data-plan-overtaken-row], " +
  "[data-node-chip], [data-node-parent-line], [data-lens-line]";

/** The marks inside `scope`; with `own`, only those it draws for its own node, none of a node,
 *  a chip or a parent line drawn inside it. */
function marksIn(scope: Locator, own: boolean): Promise<DrawnMark[]> {
  return scope.first().evaluate(
    (element, { holders, own: onlyOwn }) =>
      [...element.querySelectorAll("[data-node-mark]")]
        .filter((mark) => {
          if (!onlyOwn) return true;
          for (let at = mark.parentElement; at !== null && at !== element; at = at.parentElement) {
            if (at.matches(holders)) return false;
          }
          return true;
        })
        .map((mark) => {
          const shape = mark.querySelector("svg[data-glyph]");
          const box = shape?.getBoundingClientRect();
          const color = shape === null ? "" : getComputedStyle(shape).color;
          return {
            mark: mark.getAttribute("data-node-mark"),
            shape: shape?.getAttribute("data-glyph") ?? null,
            shown:
              box !== undefined &&
              box.width > 0 &&
              box.height > 0 &&
              color !== "" &&
              color !== "transparent" &&
              color !== "rgba(0, 0, 0, 0)",
            word: (mark.querySelector("[data-page-chrome]")?.textContent ?? "").trim(),
          };
        }),
    { holders: MARK_HOLDERS, own }
  );
}

/** The marks the row, check or bar `scope` draws in its own state line, in order. */
export function ownMarks(scope: Locator): Promise<DrawnMark[]> {
  return marksIn(scope, true);
}

/** Every mark inside `scope` - a chip, a parent line - in order: the compact form. */
export function compactMarks(scope: Locator): Promise<DrawnMark[]> {
  return marksIn(scope, false);
}

/** One row of the outline's tree, in drawing order. */
export interface TreeItem {
  /** `aria-expanded`: "true" or "false" for a row that holds children, null for one that holds
   *  none. */
  readonly expanded: string | null;
  /** `data-node-id`. */
  readonly id: string;
  /** `aria-level`. */
  readonly level: number;
  /** `data-node-read-id`. */
  readonly readId: string | null;
  /** Its whole text. */
  readonly text: string;
  readonly visible: boolean;
}

/** Every row of the page's tree, in the order it draws them. */
export function treeItemsDrawn(page: Page): Promise<TreeItem[]> {
  return issuePage(page)
    .locator("[data-node-row]")
    .evaluateAll((elements) =>
      elements.map((element) => ({
        expanded: element.getAttribute("aria-expanded"),
        id: element.getAttribute("data-node-id") ?? "",
        level: Number(element.getAttribute("aria-level") ?? "0"),
        readId: element.getAttribute("data-node-read-id"),
        text: (element.textContent ?? "").replace(/\s+/g, " ").trim(),
        visible: element.getClientRects().length > 0,
      }))
    );
}

/** The rows drawn directly under `parent`, a row of `items`, or at the top for null, in order, by
 *  the tree's own levels; none where `items` does not hold `parent`. */
export function childItems(
  items: readonly TreeItem[],
  parent: TreeItem | null | undefined
): TreeItem[] {
  if (parent === undefined) return [];
  const at = parent === null ? -1 : items.indexOf(parent);
  if (parent !== null && at === -1) return [];
  const level = parent?.level ?? 0;
  const children: TreeItem[] = [];
  for (const item of items.slice(at + 1)) {
    if (item.level <= level) break;
    if (item.level === level + 1) children.push(item);
  }
  return children;
}

/** One folded row's count: what `data-folded-count` says its branch holds. */
export interface FoldedCount {
  readonly count: number;
  readonly id: string;
  readonly readId: string | null;
}

/** The count the control of `preset` draws, or NaN where it draws none at this width. Every
 *  caller reads it inside `expect.poll`, which reads it again until the count holds. */
export async function filterCount(page: Page, preset: Preset): Promise<number> {
  const count = presetCount(page, preset);
  // Every caller polls this, so a count read before the page draws it is read again.
  if ((await count.count()) === 0) return Number.NaN;
  return Number(String(await count.first().textContent()).trim());
}

/** How many nodes a filter's flag says ask something of the reader: the checks that fail, the
 *  decisions that wait for review. NaN while the page draws no preset row, and 0 only where the
 *  drawn row draws no such flag, so a poll that waits for 0 cannot pass on a read made before
 *  the row is drawn. Every caller reads it inside `expect.poll`, which reads it again until the
 *  count holds. */
export async function flagCount(page: Page, word: "failing" | "to_review"): Promise<number> {
  const row = presetRow(page);
  // Every caller polls this, so a read made before the page draws the row is read again.
  if ((await row.count()) === 0) return Number.NaN;
  const flag = row.locator(`[data-preset-flag="${word}"] [data-author-text]`);
  // Every caller polls this, so a flag read before the page draws it is read again.
  if ((await flag.count()) === 0) return 0;
  return Number(String(await flag.first().textContent()).trim());
}

/** The Verification summary's "N of M covered": how many statements carry a check, of how many
 *  it counts; null where the view draws no summary. Every caller reads it inside `expect.poll`,
 *  which reads it again until the summary holds. */
export async function coveredCount(
  page: Page
): Promise<{ readonly covered: number; readonly of: number } | null> {
  const line = issuePage(page).locator("[data-verification-covered]");
  // Every caller polls this, so a summary read before the page draws it is read again.
  if ((await line.count()) === 0) return null;
  const [covered, of] = await line.locator("[data-author-text]").allTextContents();
  return { covered: Number(String(covered).trim()), of: Number(String(of).trim()) };
}

/** Details' "Referenced by" list of the focused row (`node-references/NodeReferences.tsx`):
 *  opened by its control, then counted once it answered, its "none" line counting none. */
export async function referencedByCount(page: Page): Promise<number> {
  const section = page.locator("[data-node-references]");
  const toggle = section.locator(":scope > button");
  if ((await expandedState("Details' Referenced by", toggle)) === "shut") await toggle.click();
  const list = section.locator("ul > li");
  await expect
    .poll(async () => (await list.count()) > 0 || (await section.locator("p").count()) > 0, {
      message: "Details' Referenced by never answered",
    })
    .toBe(true);
  return list.count();
}

/** The text of every element of the page whose whole text is a raw value of the tree read - a
 *  `state` or a `visibility` - or a word-list key, in any letter case, unless `book` (the words
 *  `page-words.ts` writes) holds it as written. */
export function rawValueTexts(page: Page, book: readonly string[]): Promise<string[]> {
  return issuePage(page).evaluate(
    (root, { book: words, raw }) =>
      [root, ...root.querySelectorAll("*")].flatMap((element) => {
        const text = (element.textContent ?? "").replace(/\s+/g, " ").trim();
        return raw.includes(text.toLowerCase()) && !words.includes(text) ? [text] : [];
      }),
    {
      book,
      raw: ["proposed", "overtaken", "settled", "live", "gone", "author", "project", "only_you"],
    }
  );
}

// ---------------------------------------------------------------------------------------------
// What the page sent.
// ---------------------------------------------------------------------------------------------

/** A request to a node state route: a node's state, its settle, and an issue's proposed nodes,
 *  which the page reads none of (it reads a node's life from the tree read alone). */
const NODE_STATE_ROUTE = /^\/api\/v1\/issues\/[^/]+\/(?:nodes\/[^/]+\/(?:state|settle)|proposed)$/u;

/** The settle route's path, with the node's address as its one encoded segment. */
const SETTLE_ROUTE = /^\/api\/v1\/issues\/[^/]+\/nodes\/([^/]+)\/settle$/u;

/** Each request of `requests` to a node state route, as its method and path. */
export function nodeStateRequests(requests: readonly ApiRequest[]): string[] {
  return requests.flatMap((request) =>
    NODE_STATE_ROUTE.test(new URL(request.path, "http://requests.invalid").pathname)
      ? [`${request.method} ${request.path}`]
      : []
  );
}

/** The address each `POST` to the settle route of `requests` names, decoded. */
export function settlesSent(requests: readonly ApiRequest[]): string[] {
  return requests.flatMap((request) => {
    const matched = SETTLE_ROUTE.exec(new URL(request.path, "http://requests.invalid").pathname);
    return request.method === "POST" && matched !== null
      ? [decodeURIComponent(matched[1] ?? "")]
      : [];
  });
}

// ---------------------------------------------------------------------------------------------
// Details' facts of the focused row.
// ---------------------------------------------------------------------------------------------

/** The file that draws Details' facts of the focused row and their "Referenced by" list. */
const FOCUSED_FACTS_FILE = new URL(
  "../web/src/features/issue-page/FocusedNodeFacts.tsx",
  import.meta.url
);

/** Details' facts of the focused row and their "Referenced by" list, which the rows read only
 *  where this tree holds the file that draws them: until then the page draws none, and nothing
 *  of this change is owed there. The file is looked for once, as this module loads. */
export const FOCUSED_FACTS_GATE: Gate = {
  met: existsSync(FOCUSED_FACTS_FILE),
  owed:
    "owed until Details draws the focused row's facts (FocusedNodeFacts.tsx is not in this " +
    "tree), so those facts and Referenced by are not read here",
};

/** Goes on where `gate` is met; where it is not, records an `owed` annotation naming it, once
 *  for the row however often the row reaches it, and the row goes on without the reading it
 *  gates. Answers whether the gate is met. */
export function noteOwed(gate: Gate): boolean {
  const annotations = test.info().annotations;
  const noted = annotations.some(({ description }) => description === gate.owed);
  if (!gate.met && !noted) annotations.push({ description: gate.owed, type: "owed" });
  return gate.met;
}

/** Details' facts of the row the focus is on (`FocusedNodeFacts.tsx`). */
export function focusedNodeFacts(page: Page): Locator {
  return page.getByTestId("focused-node-facts");
}

// ---------------------------------------------------------------------------------------------
// What the page draws once the parts after this one are under the head.
// ---------------------------------------------------------------------------------------------

/** The name of the Graph control in the page's word list, which holds none until part T's Graph
 *  is under the head. */
export const GRAPH_NAME: string | undefined = (PRESET_NAMES as Readonly<Record<string, string>>)
  .graph;

/** Whether the page draws a Graph control, so a row that reads a node in Graph reads it only
 *  where there is one. */
export async function graphGate(page: Page): Promise<Gate> {
  const control =
    GRAPH_NAME === undefined ? undefined : presetRow(page).getByText(GRAPH_NAME, { exact: true });
  const drawn = control !== undefined && (await control.count()) > 0;
  return {
    met: drawn,
    owed:
      "owed until part T's Graph is under the head: the page draws no Graph control here, so " +
      `each Graph reading is not made; ${OWED_BY}`,
  };
}

/** The name of the Graph control, for a row that found it drawn (`graphGate`). */
export function graphName(): string {
  if (GRAPH_NAME === undefined) throw new Error("the page's word list names no Graph control");
  return GRAPH_NAME;
}
