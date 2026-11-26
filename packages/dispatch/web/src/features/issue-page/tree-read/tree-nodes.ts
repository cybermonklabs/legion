import { type BlockItemKind, parseBlockPointerValue } from "@legion/contracts";

import { primarySpec, roleDocument } from "../../../api/issue-cache";
import type { TreeFilter } from "../../../api/queries";
import type {
  ArtifactBlock,
  BlockAttributeValue,
  CheckBlock,
  CheckResult,
  Decision,
  DecisionEntry,
  DisplayTree,
  IssueChild,
  IssueDetails,
  RequirementCoverage,
} from "../../../api/types";
import { compareTimestamps, isTimestamp } from "../../../lib/timestamps";
import { type PostDeletion, postDeletion } from "../../refs/post-marks";
import type { ReferenceNameReading } from "../../refs/reference-names";
import {
  buildDispatchReference,
  parseDispatchReference,
  parseNodeReference,
} from "../../refs/routes";
import { firstWordedBlock } from "../../refs/tree-fragment";
import { checkFacts, resultFacts } from "../check-facts";
import {
  blockIdOfNode,
  decisionNodeChanges,
  decisionNodes,
  deletedPostIds,
  firstOfEachId,
  inReviewList,
  parentsIn,
} from "../decision-review";
import type { IssueTreeReads, NodeChip, TreeAsk, TreeNode } from "../issue-tree";
import { lensChips, lensDrafts } from "../lenses/lens-drafts";
import { storeWords } from "../lenses/store-words";
import {
  blockAttributeLabel,
  coverageNotes,
  NODE_TYPE_NAMES,
  NODE_TYPES,
  type NodeType,
} from "../page-words";
import { titleReading } from "../statement-rules";
import { storedReading } from "../stored-reading";
import {
  addDocumentChips,
  answerOf,
  askState,
  blank,
  blockNodeId,
  checkStanding,
  type Draft,
  deciderOf,
  decisionState,
  deletedPostLine,
  markKeptAndUnsure,
  requirementGaps,
  stableSourceOf,
  type TreeContext,
} from "../tree-drafts";
import type { TreeAnswer, TreeAnswerNode, TreeTuple } from "./tree-read";

/**
 * The tree read's answer as the outline's drafts (`tree-drafts.ts`), for `assemble` to build the
 * outline from, and as the blocks a role's document holds (`blocksOfRole`).
 *
 * The read places and orders every node, so nothing here places a node by a pointer or orders
 * one by a time: each draft stands under the node's `parent`, walked up the answer to the
 * nearest node a draft stands for where the parent has none, and its order is its place in the
 * read's walk. The one placement added is the outline's transcluded one: the read gives every
 * node one parent, and a check or a plan task stands again under each other requirement its
 * `serves` tuples name (root R4).
 *
 * What the answer does not hold - an ask's options and answer, a check's standing and facts, a
 * requirement's counts and gaps, a child's row, a decision's decider - comes from the page's
 * other reads, each row matched to its node by its `dispatch://` address (`KeptReads`). A node's
 * words are its `text_tree`, read by the outline's word rules (`nodeWords`, `lineWordsRaw`), so a
 * row reads as it read when the outline was built from the page's other reads; an `ask` block's
 * are its question as the asks read holds it (`draftAsk`).
 *
 * Each kind the page draws is one step of `treeDraftSteps`. A node of any other kind - a
 * criterion, a lens and its kind, a finding, a risk, a constraint, an interaction - reaches no
 * step of this part's, makes no draft and stays in the answer, which the page keeps whole (R7);
 * a later part draws a kind of its own with one line in that list, and adds a member to another
 * step's draft with one line in `treeDraftMarks`. A check of an issue under this one makes no
 * draft either, nor does its plan task but where the read answers its place in the plan
 * (`draftServing`), as the Plan view's read does.
 *
 * `buildIssueTree` builds the tree from these drafts, so this module imports no code of
 * `issue-tree.ts`, which would close an import cycle (`biome.json`'s `noImportCycles`): the word
 * rule it reads is handed in (`TreeWords`).
 */

/** The outline's reading of the issue's addresses and names (`treeContext`), with the word rule
 *  of `issue-tree.ts` a draft's line is read by: what `buildIssueTree` hands `treeDraftsFrom`. */
export interface TreeWords extends TreeContext {
  /** The words a node's one line is cut from (`lineWordsRaw`). */
  readonly lineWords: (node: Pick<TreeNode, "text" | "tree">) => string;
}

/** What the author of a comment or a message did to it: deleted it (`postDeletion`), or last
 *  replaced its body (`editedAt`). */
export interface PostMarks {
  readonly deleted: PostDeletion | null;
  readonly editedAt: string | null;
}

/** The page's other reads, each row by the tree id of the node it tells about (`treeIdOf`): the
 *  facts the answer does not hold, each row's `dispatch://` address read to a node id as the
 *  outline's builders read it (`blockNodeId`, `nodeIdOf`), so a row and its node meet by one
 *  address. */
export interface KeptReads {
  /** The comments the comments read anchors to text (`Comment.anchor`, which names the text's
   *  block and quote), by their own ids: the tree read answers no anchor on a comment node, and
   *  hangs such a comment under the node whose text it is on (`onNodeText`). */
  readonly anchored: ReadonlySet<string>;
  /** Each ask of the issue read's open asks and of the asks read, the asks read's where both
   *  hold it. */
  readonly asks: ReadonlyMap<string, TreeAsk>;
  /** Each check of the results read, with the results recorded against it, a check that names
   *  no document by its own id (`keptReads`). */
  readonly checks: ReadonlyMap<string, CheckBlock>;
  /** Each child of the issue read, by its key. */
  readonly children: ReadonlyMap<string, IssueChild>;
  /** Each entry of the decisions read that a decision node of the answer is (`decisionNodes`). */
  readonly decisions: ReadonlyMap<string, DecisionEntry>;
  /** Each requirement of the coverage read: the statements, with their counts and gaps. */
  readonly requirements: ReadonlyMap<string, RequirementCoverage>;
  /** Each result of the results read, by its own id. */
  readonly results: ReadonlyMap<string, CheckResult>;
  /** The coverage read's own lines, which name no requirement's gap: while a document of the
   *  issue is still being indexed, its line, which the server writes in place of every missing
   *  check (`coverageNotes`). Each requirement the read holds draws them. */
  readonly coverageNotes: string[];
  /** Whether the Plan view's answer the page holds holds a plan task that is not gone
   *  (`IssueTreeReads.holdsPlanTask`): one of this issue, or of an issue under it, which that read
   *  answers with the issue's own. Once one is held a statement no plan task serves is a gap,
   *  since a plan is written after the spec is approved; false while that read holds no answer. */
  readonly planned: boolean;
  /** What the author of each comment and message of the comments read and the messages read did
   *  to it, by the post's own id: the tree read answers its words, a deleted post's none, and no
   *  mark of either. A post nobody corrected has no entry. */
  readonly posts: ReadonlyMap<string, PostMarks>;
  /** The artifact id of the issue's Verification document (`roleDocument`): a check block of it
   *  is a check, which draws a state, and one of any other document is a block and nothing more,
   *  since the results read never answers for it. */
  readonly verification: string | undefined;
}

/** What a step and a mark read: the words and the names `buildIssueTree` hands in, the page's
 *  other reads by node (`kept`), and the answer, looked up by the read's own ids, each node's
 *  store id, which a tuple's ends and a node's `parent` name it by too. Two blocks of two
 *  documents can share a block id, and a draft and its place are keyed on the node's own
 *  identity (`treeIdOfNode`, `placeOf`), so each of the two is a row. */
export interface TreeDraftContext extends TreeWords {
  /** The Change each decision node waits on (`decision-review.ts`' `decisionNodeChanges`), by
   *  the node's tree id (`treeIdOfNode`), so two decision nodes of one block id in two documents
   *  are two decisions, each with its own Changes. */
  readonly changes: ReadonlyMap<string, TreeAnswerNode>;
  /** Whether a draft stands for the node `readId` names: a step drafted it, or drafted a node
   *  the tree draws by the same id before it. While the steps run, the nodes offered so far are
   *  the ones that can have one, and every ancestor of the node being offered is among them. */
  readonly drafted: (readId: string) => boolean;
  readonly kept: KeptReads;
  /** The words of the one line of the draft that stands for the node `readId` names
   *  (`lineWords`). It answers once every node has been offered to the steps, so a mark reads
   *  it and a step does not, since the draft of a node later in the walk is not made yet; it is
   *  undefined before then, and for a node no draft stands for. */
  readonly lineOf: (readId: string) => string | undefined;
  /** The node of the answer `readId` names. */
  readonly nodeOf: (readId: string) => TreeAnswerNode | undefined;
  /** The node of the answer by its drawn id, for a pointer written as a document address. */
  readonly nodeByTreeId: (id: string) => TreeAnswerNode | undefined;
  /** Where the read places `node`, as the tree draws it: under the nearest node a draft stands
   *  for, walking up from its parent, which is the nearest node before it in the walk that its
   *  `parent` names, since a parent comes before its children; and under the issue, `[]`, where
   *  none stands. */
  readonly placeOf: (node: TreeAnswerNode) => string[];
  /** The node's own `dispatch://` address, as its draft carries it (`referenceOf`), or undefined
   *  where it has none. */
  readonly referenceOf: (node: TreeAnswerNode) => string | undefined;
  /** The id the tree draws the node `readId` names by (`treeIdOf`). */
  readonly treeIdOf: (readId: string) => string;
  /** The id the tree draws `node` by, read from the node itself (`treeIdOfNode`). */
  readonly treeIdOfNode: (node: TreeAnswerNode) => string;
  /** The tuples of the answer that start at the node `readId` names, in the answer's order. */
  readonly tuplesFrom: (readId: string) => readonly TreeTuple[];
  /** The tuples of the answer that end at the node `readId` names, in the answer's order. */
  readonly tuplesTo: (readId: string) => readonly TreeTuple[];
}

/** One kind's step: the draft of `node` where it is of a kind the step draws, else undefined,
 *  and the next step of `treeDraftSteps` is asked. A draft whose `parents` the step leaves empty
 *  is placed where the read places the node (`treeDraftsFrom`). */
export type TreeDraftStep = (
  node: TreeAnswerNode,
  answer: TreeAnswer,
  context: TreeDraftContext
) => Draft | undefined;

/** A member added to a draft another step made, run on every draft once every node has been
 *  offered to the steps, so it can read any other node's draft (`TreeDraftContext.lineOf`). */
export type TreeDraftMark = (
  draft: Draft,
  node: TreeAnswerNode,
  answer: TreeAnswer,
  context: TreeDraftContext
) => void;

/**
 * The steps that draft the answer's nodes, each node offered to them in this order and drafted by
 * the first that answers one. These steps draw the kinds the outline draws, a fixed list read
 * from each node's supertag and not from the page's names for kinds (`NODE_TYPE_NAMES`), which
 * later parts widen: a requirement, a check and a plan task, a result, a decision, an ask, a
 * comment and a message, a callout and an image, a paragraph and every other plain block
 * (`isPlainBlock`), a reading, and a child issue.
 */
export const treeDraftSteps: readonly TreeDraftStep[] = [
  lensDrafts,
  draftRequirement,
  draftServing,
  draftResult,
  draftDecision,
  draftAsk,
  draftTurn,
  draftBlock,
  draftProse,
  draftReading,
  draftChild,
];

/** The marks run on every draft, each in turn, once every node has been offered to the steps.
 *  This part adds none: a member of its drafts is its own step's. */
export const treeDraftMarks: readonly TreeDraftMark[] = [
  // A later part adds its mark here, one line each.
  lensChips,
  storeWords,
];

/**
 * Every draft the answer holds, by its tree id. Each node is offered to `treeDraftSteps` in the
 * walk's order (`walkOf`), and the first draft a step answers is kept, ordered by the node's
 * place in that walk, so the tree draws siblings in the read's order (`drawOrder`). A draft's id
 * is read from its node (`treeIdOfNode`), so two nodes one read id names in two documents are
 * two drafts, and two nodes the tree draws by one id are one node of it, the first the walk
 * reaches; where both are asks - an ask drawn in the row of the block it was indexed from - the
 * row names what each of the two shaped.
 *
 * Once every node has been offered, a draft whose step set no `parents` is placed where the read
 * places its node (`placeOf`), each draft gains a chip for each whole document its words cite
 * (`addDocumentChips`), as the outline's drafts do, and then each of `treeDraftMarks` is run on
 * every draft.
 */
export function treeDraftsFrom(
  answer: TreeAnswer,
  reads: IssueTreeReads,
  context: TreeWords
): Map<string, Draft> {
  const nodes = firstOfEachId(answer.nodes ?? []);
  const tuples = new Map<string, TreeTuple[]>();
  const ending = new Map<string, TreeTuple[]>();
  for (const tuple of answer.tuples ?? []) {
    pushInto(tuples, tuple.from.id, tuple);
    pushInto(ending, tuple.to.id, tuple);
  }
  const idOfNode = nodeIdsIn(reads);
  let byTreeId: Map<string, TreeAnswerNode> | undefined;
  const idOf = (readId: string): string => {
    const node = nodes.get(readId);
    return node === undefined ? readId : idOfNode(node);
  };
  const walk = walkOf(answer, reads.issue.key);
  const parents = parentsIn(walk, nodes);
  const drafts = new Map<string, Draft>();
  let offered = false;
  const drafting: TreeDraftContext = {
    ...context,
    changes: decisionNodeChanges(answer, idOfNode, deletedPostIds(reads.comments)),
    drafted: (readId) => drafts.has(idOf(readId)),
    kept: keptReads(reads, context, answer, idOfNode),
    lineOf: (readId) => {
      const draft = offered ? drafts.get(idOf(readId)) : undefined;
      return draft === undefined ? undefined : context.lineWords(draft);
    },
    nodeOf: (readId) => nodes.get(readId),
    nodeByTreeId: (id) => {
      if (byTreeId === undefined) {
        byTreeId = new Map();
        for (const node of answer.nodes ?? []) {
          const drawn = idOfNode(node);
          if (!byTreeId.has(drawn)) byTreeId.set(drawn, node);
        }
      }
      return byTreeId.get(id);
    },
    // A node under a kind the page does not draw stands under that node's nearest drawn
    // ancestor, and a chain of parents that runs in a circle ends where it comes round.
    placeOf: (node) => {
      const passed = new Set<TreeAnswerNode>();
      for (
        let above = parents.get(node);
        above !== undefined && !passed.has(above);
        above = parents.get(above)
      ) {
        passed.add(above);
        const id = idOfNode(above);
        if (drafts.has(id)) return [id];
      }
      return [];
    },
    referenceOf: (node) => referenceOf(node, drafting),
    treeIdOf: idOf,
    treeIdOfNode: idOfNode,
    tuplesFrom: (readId) => tuples.get(readId) ?? [],
    tuplesTo: (readId) => ending.get(readId) ?? [],
  };
  const made: [Draft, TreeAnswerNode][] = [];
  for (const [order, node] of walk.entries()) {
    const draft = firstDraft(node, answer, drafting);
    if (draft === undefined) continue;
    const held = drafts.get(draft.id);
    if (held !== undefined) {
      // An ask the asks read indexed from a block is drawn in that block's row, so what the ask
      // shaped is named on that row as well as what the block shaped.
      if (held.type === "ask" && draft.type === "ask") {
        const named = new Set(held.chips.map((chip) => chip.ref));
        held.chips = [...held.chips, ...draft.chips.filter((chip) => !named.has(chip.ref))];
      }
      continue;
    }
    draft.order = order;
    draft.readId = node.id;
    drafts.set(draft.id, draft);
    made.push([draft, node]);
  }
  offered = true;
  for (const [draft, node] of made) {
    if (draft.parents.length === 0) draft.parents = drafting.placeOf(node);
  }
  addDocumentChips(drafts, reads, drafting);
  for (const mark of treeDraftMarks) {
    for (const [draft, node] of made) mark(draft, node, answer, drafting);
  }
  return drafts;
}

/** Adds `value` to the list `groups` holds under `key`, which it starts where there is none. */
function pushInto<Value>(groups: Map<string, Value[]>, key: string, value: Value): void {
  const group = groups.get(key);
  if (group === undefined) groups.set(key, [value]);
  else group.push(value);
}

/** The first draft a step of `treeDraftSteps` answers for `node`, in the list's order. A node the
 *  read answers `gone` makes none: the read's walk leaves out a node gone at its cut, and says
 *  nothing of how one it answers all the same - another issue's check whose `serves` still
 *  stands, a context node - is drawn, so it is kept as a node of a kind no step drafts is, whole
 *  in the answer and drawn by no row, what the read hangs under it standing under its nearest
 *  drawn ancestor (`placeOf`). */
function firstDraft(
  node: TreeAnswerNode,
  answer: TreeAnswer,
  context: TreeDraftContext
): Draft | undefined {
  if (node.state === "gone") return undefined;
  for (const step of treeDraftSteps) {
    const draft = step(node, answer, context);
    if (draft !== undefined) return draft;
  }
  return undefined;
}

/** What `treeIdOf` reads beside the answer: the issue, for its key and its documents' slugs, and
 *  the asks read, which names the block an ask was indexed from. */
export type TreeIdReads = Pick<IssueTreeReads, "asks" | "issue">;

/** No ask named by the block it was indexed from. */
const NO_ASK_IDS: ReadonlyMap<string, string> = new Map();

/**
 * The id the tree draws `node` of issue `key`'s answer by, read from the node itself. The read's
 * id is the node's store id, which no other node holds (a document block's is
 * `<KEY>/<slug>#<block id>`); the page's own ids are kept wherever the read names enough to make
 * one, so each place that reads a node's id - the rows, `documentOfNode`, `pathToTarget`,
 * `specSummary` - reads the tree by the ids it read before, a block by its id in its document
 * (`block_id`):
 *
 * - **A node of another issue** - the read marks it with that issue (`issue`) - is its block id,
 *   even where its `document` names a document: a child's documents carry its parent's slugs, so
 *   the outline's `<slug>#b-<block id>` would name a block of this issue.
 * - **The issue itself** is its key, which is the outline's root.
 * - **A typed block of one of this issue's documents** is the outline's `blockNodeId` of the
 *   slug its `document` names and its block id.
 * - **An ask** is the node id of the block it was indexed from, where `askIds` names one for the
 *   ask's id, and its own id otherwise.
 * - **Anything else** - a comment, a message, a child issue, whose id is its key, a block of the
 *   issue's own tree - is its block id, the read's id for a node that is no block.
 */
export function treeIdOfNode(
  node: TreeAnswerNode,
  key: string,
  askIds: ReadonlyMap<string, string> = NO_ASK_IDS
): string {
  const blockId = blockIdOfNode(node);
  if (isForeign(node, key)) return blockId;
  // The issue the read is of is the outline's root, the parent of every top-level node.
  if (node.supertag === "issue" && (node.id === key || node.parent === null)) return key;
  if (node.document !== undefined) return blockNodeId(node.document.slug, blockId);
  return node.supertag === "ask" ? (askIds.get(node.id) ?? blockId) : blockId;
}

/**
 * The id the tree draws each node of `answer` by (`treeIdOfNode`), as a lookup by the read's own
 * id, which answers the first node the walk reaches where two share the id; a read id the answer
 * does not hold is its own tree id.
 */
export function treeIdOf(answer: TreeAnswer, reads: TreeIdReads): (readId: string) => string {
  const nodes = firstOfEachId(answer.nodes ?? []);
  const idOfNode = nodeIdsIn(reads);
  return (readId) => {
    const node = nodes.get(readId);
    return node === undefined ? readId : idOfNode(node);
  };
}

/** `treeIdOfNode` for the issue `reads` names, each ask by the block the asks read names it was
 *  indexed from (`askNodeId`). */
function nodeIdsIn(reads: TreeIdReads): (node: TreeAnswerNode) => string {
  const slugs = new Map(
    (reads.issue.artifacts ?? []).map((artifact) => [artifact.id, artifact.slug])
  );
  const asks = new Map(
    [...asksOf(reads).values()].map((ask) => [
      ask.id,
      askNodeId(ask, (artifactId) => slugs.get(artifactId)),
    ])
  );
  return (node) => treeIdOfNode(node, reads.issue.key, asks);
}

/**
 * The answer's nodes in the order they are offered to the steps: the read's own walk, a parent
 * before its children, with the nodes that hang from nothing in this issue's tree after the rest,
 * in the read's order among themselves - a node whose parent is in an issue above, which the
 * answer does not hold, and the context the read carries (`role: "context"`), which stands in no
 * issue's tree. Every node a step is offered then has its ancestors' drafts made before it.
 */
function walkOf(answer: TreeAnswer, key: string): TreeAnswerNode[] {
  const nodes = firstOfEachId(answer.nodes ?? []);
  const inTree = new Map<TreeAnswerNode, boolean>();
  const reachesIssue = (node: TreeAnswerNode): boolean => {
    const known = inTree.get(node);
    if (known !== undefined) return known;
    // A chain of parents that runs in a circle reaches no issue.
    inTree.set(node, false);
    const parent = node.parent === null ? undefined : nodes.get(node.parent.id);
    const reached =
      node.role !== "context" &&
      (node.parent === null ||
        node.parent.id === key ||
        (parent !== undefined && reachesIssue(parent)));
    inTree.set(node, reached);
    return reached;
  };
  const all = answer.nodes ?? [];
  const rooted = all.filter(reachesIssue);
  return [...rooted, ...all.filter((node) => !reachesIssue(node))];
}

/** A requirement, with the counts and the gaps the coverage read holds for it, and a statement of
 *  the issue where it is one (`isStatementNode`). Where that read holds it, its gaps are the ones
 *  that read carries (`requirementGaps`); a statement lacks a check by the answer's own count of
 *  them, and a plan task where that read counts none serving it once the Plan view holds a plan
 *  task (`KeptReads.planned`). */
function draftRequirement(
  node: TreeAnswerNode,
  _answer: TreeAnswer,
  context: TreeDraftContext
): Draft | undefined {
  if (node.supertag !== "requirement") return undefined;
  const draft = draftOf(node, "requirement", context);
  const coverage = context.kept.requirements.get(draft.id);
  if (isStatementNode(node, context)) draft.statement = true;
  if (coverage !== undefined) {
    draft.counts = { checks: coverage.checks, planTasks: coverage.plan_tasks };
    if (context.kept.coverageNotes.length > 0) draft.coverageNotes = context.kept.coverageNotes;
    draft.gaps = requirementGaps(coverage.gaps ?? []);
  }
  // Where the answer counts the checks that serve it (`checks`, which every read of the route
  // answers on a requirement), a statement lacks a check by that count, whatever the coverage
  // read says (R9).
  const { checks: served } = node;
  if (draft.statement === true && typeof served === "number") {
    const others = draft.gaps.filter((gap) => gap !== "no_check");
    draft.gaps = served === 0 ? [...others, "no_check"] : others;
  }
  // The coverage read counts the plan tasks of this issue and of every issue under it that serve
  // the statement (`plan_tasks`). While a document of the issue is still being indexed that count
  // may not hold them yet, and the server names no requirement as lacking one then
  // (`coverageNotes`), so neither does the page.
  if (
    draft.statement === true &&
    coverage?.plan_tasks === 0 &&
    context.kept.planned &&
    context.kept.coverageNotes.length === 0
  ) {
    draft.gaps = [...draft.gaps, "no_plan_task"];
  }
  // A proposed statement is a question still open and an overtaken one is closed: neither owes
  // any work, so neither draws a gap, whatever the counts say.
  if (node.state === "proposed" || node.state === "overtaken") draft.gaps = [];
  return draft;
}

/**
 * A check or a plan task of this issue: under the parent the read answers, and again under each
 * other statement of this issue its `serves` tuples name, transcluded there as the outline
 * places it (root R4); its `needs` tuples are chips, one per task needed (`needsChips`). It
 * carries the outline's three gaps of the chain:
 *
 * - **A citation whose target is no statement** (`serves_not_requirement`): a `serves` naming a
 *   node of another kind, or a `requirement` block of a document other than the spec, which is a
 *   requirement and no statement, so the gap's words say what both are; or one of this issue's
 *   own pointers that names no node of the answer - a target deleted after the write, such as a
 *   requirement taken out of the spec - where the answer holds blocks of the document it names,
 *   since only there can the page tell a deleted block from one it has not read. A reference
 *   this issue does not address (`nodeIdOf`) is no such citation: the page cannot tell it from a
 *   statement of an issue above, which a check serves.
 * - **A check that names nothing it serves** (`serves_nothing`): a check that draws a state,
 *   one of the Verification document or of the results read, whose block names no `serves` at
 *   all, so it stands under the issue with nothing to prove.
 * - **A plan task nobody asked for** (`nobody_asked`): one naming nothing it serves and nothing it
 *   attempts.
 *
 * A check stands as the server reads the commit each of its results ran at (`checkStanding`),
 * and draws the results read's facts; a check of the Verification document that read does not
 * hold has no result, and a check block of any other document draws no state.
 */
function draftServing(
  node: TreeAnswerNode,
  answer: TreeAnswer,
  context: TreeDraftContext
): Draft | undefined {
  // A plan task of an issue under this one is drafted where the read answers its place in the
  // plan (`plan`), which only the Plan view's read does: that view draws it as a step, marked
  // with its issue (`presets.ts`' `heldFilterView`). Every other node of another issue makes no
  // draft here.
  const foreign = isForeign(node, context.key);
  if (
    (node.supertag !== "check" && node.supertag !== "plan_task") ||
    (foreign && (node.supertag !== "plan_task" || node.plan === undefined))
  ) {
    return undefined;
  }
  const draft = draftOf(node, node.supertag, context);
  const served = context.tuplesFrom(node.id).flatMap((tuple) => {
    const target = tuple.kind === "serves" ? context.nodeOf(tuple.to.id) : undefined;
    return target === undefined || isForeign(target, context.key) ? [] : [target];
  });
  // The Plan read omits a tuple whose statement is outside its answer. When the outline
  // supplies that statement as context, read the step's authored pointer to it.
  if (node.supertag === "plan_task" && node.role === "query") {
    for (const pointer of pointersOf(node, "serves")) {
      const id = context.nodeIdOf(pointer);
      const target = id === undefined ? undefined : context.nodeByTreeId(id);
      if (
        target !== undefined &&
        !isForeign(target, context.key) &&
        isStatementNode(target, context)
      ) {
        served.push(target);
      }
    }
  }
  const requirements = served.flatMap((target) =>
    isStatementNode(target, context) ? [context.treeIdOf(target.id)] : []
  );
  draft.parents = [...new Set([...context.placeOf(node), ...requirements])];
  // A pointer of this issue's own whose target the answer holds no node of, in a document whose
  // blocks the answer holds.
  const namesNothingHeld = (pointer: string): boolean => {
    const id = context.nodeIdOf(pointer);
    if (id === undefined || context.nodeByTreeId(id) !== undefined) return false;
    const route = parseNodeReference(pointer);
    const slug =
      route?.kind === "spec" ? context.specSlug : route?.kind === "artifact" ? route.slug : "";
    return slug !== "" && (answer.nodes ?? []).some((one) => one.document?.slug === slug);
  };
  if (
    served.some((target) => !isStatementNode(target, context)) ||
    (!foreign && pointersOf(node, "serves").some(namesNothingHeld))
  ) {
    draft.gaps.push("serves_not_requirement");
  }
  // What a task of another issue serves is that issue's statements, which its own page says it
  // lacks, so it carries no gap here.
  if (
    !foreign &&
    node.supertag === "plan_task" &&
    !namesAny(node, "serves", context) &&
    !namesAny(node, "attempts", context)
  ) {
    draft.gaps.push("nobody_asked");
  }
  draft.chips = needsChips(node, context);
  if (node.supertag === "check") {
    const check = context.kept.checks.get(draft.id);
    if (check !== undefined) {
      draft.state = checkStanding(check).state;
      draft.facts = checkFacts(check);
    } else if (
      context.kept.verification !== undefined &&
      node.document?.artifact_id === context.kept.verification
    ) {
      draft.state = "no-result";
    }
  }
  if (
    node.supertag === "check" &&
    draft.state !== undefined &&
    !namesAny(node, "serves", context)
  ) {
    draft.gaps.push("serves_nothing");
  }
  return draft;
}

/** Whether `node` is a statement: a `requirement` block of the issue's spec (`isSpecBlock`),
 *  whose blocks the coverage read's rows are too (`KeptReads.requirements`). A `requirement`
 *  block of any other document is a block and nothing more: nothing stands under it. */
function isStatementNode(node: TreeAnswerNode, context: TreeDraftContext): boolean {
  return node.supertag === "requirement" && isSpecBlock(node, context);
}

/** Whether `node` is a block of this issue's spec, the document the issue read names
 *  (`TreeContext.specSlug`), as the read places it (`document`). */
function isSpecBlock(node: TreeAnswerNode, context: TreeDraftContext): boolean {
  return (
    context.specSlug !== "" &&
    !isForeign(node, context.key) &&
    node.document?.slug === context.specSlug
  );
}

/** A result of a check of this issue: who recorded it and when, and what it recorded, from the
 *  results read. A result of an issue under this one stands with its check, out of this part's
 *  scope. */
function draftResult(
  node: TreeAnswerNode,
  _answer: TreeAnswer,
  context: TreeDraftContext
): Draft | undefined {
  if (node.supertag !== "result" || isForeign(node, context.key)) return undefined;
  const draft = draftOf(node, "result", context);
  madeBy(draft, node);
  const result = context.kept.results.get(node.id);
  if (result !== undefined) draft.facts = resultFacts(result);
  return draft;
}

/**
 * A decision node, with the review state the read answers on it as the decisions read answers it
 * for a decision (`DecisionEntry`'s members): its state in the page's words (`decisionState`),
 * who kept it, whether it was replaced and whether its agent was unsure, and its `shaped` tuples
 * as chips, each stale where the decision changed after it named that node. Its time is its
 * words' last change, the decisions read's `changed_at` as the outline draws it, and its links
 * are that read's `shaped` edge, where it holds an entry for the decision; the read answers both
 * on the node's `decision`, the decisions read's own object, where it holds none. Its author is
 * who decided it, as that entry names them (`DecisionEntry.decided_by`, `deciderOf`), since the
 * node's `decision` names no decider; none where the read holds no entry or the entry names
 * nobody. A person's Change the read hangs under it says it waits on its agent
 * (`decisionNodeChanges`), and it waits on the reader by the outline's review list rule
 * (`inReviewList`), where a link ties it to a statement the coverage read holds.
 *
 * An agent's decision of the decisions read that is no node of the answer - a section of
 * `decisions.md` a server of the release before answers - is drawn as the outline draws it, by
 * `buildIssueTree` (`tree-drafts.ts`'s `addDecisions`).
 */
function draftDecision(
  node: TreeAnswerNode,
  _answer: TreeAnswer,
  context: TreeDraftContext
): Draft | undefined {
  if (node.supertag !== "decision") return undefined;
  const draft = draftOf(node, "decision", context);
  const decision: Partial<Decision> = node.decision ?? {};
  const kept = context.kept.decisions.get(draft.id);
  const changedAt = kept === undefined ? decision.changed_at : kept.changed_at;
  if (typeof changedAt === "string") draft.at = changedAt;
  const decider = deciderOf(kept?.decided_by ?? null);
  if (decider !== undefined) draft.author = decider;
  const links = kept?.shaped ?? decision.links ?? [];
  const stale = new Set(
    links.flatMap((link) =>
      link.stale && link.to.ref !== undefined ? [context.nodeIdOf(link.to.ref) ?? link.to.ref] : []
    )
  );
  draft.chips = tupleChips(node, "shaped", context).map((chip) => ({
    ...chip,
    stale: stale.has(context.nodeIdOf(chip.ref) ?? chip.ref),
  }));
  const changed = context.changes.has(draft.id);
  draft.state = decisionState(decision, changed);
  markKeptAndUnsure(draft, decision);
  draft.replaced = decision.replaced === true;
  draft.waiting =
    context.viewer !== undefined &&
    inReviewList(
      { shaped: links, waiting_review: decision.waiting_review === true },
      (reference) => context.kept.requirements.has(context.nodeIdOf(reference) ?? ""),
      changed
    );
  return draft;
}

/**
 * A question asked of the issue: who asked it and when, its `shaped` tuples as chips, each naming
 * a node the question shaped, and from the asks read how it stands, its options, a person's
 * answer, and whether it waits on the reader, whose turn it is, as the outline drew a question,
 * with the ask as the record its words are, kept by the tree read's reading of them, else by the
 * node (`stableSourceOf`). An approval request belongs to the document it reviews and is no node
 * of the tree, as in the outline. An ask the asks read has not answered for yet is open, as the
 * outline drew an ask block before that read answered.
 *
 * An `ask` block of a document is drawn by its question as the asks read holds it, with that
 * read's reading of it, as the outline drew an ask: the read's reading of the block is of its
 * whole content, whose options the asks read holds apart and the row's body draws apart.
 */
function draftAsk(
  node: TreeAnswerNode,
  _answer: TreeAnswer,
  context: TreeDraftContext
): Draft | undefined {
  if (node.supertag !== "ask") return undefined;
  const draft = draftOf(node, "ask", context);
  const ask = context.kept.asks.get(draft.id);
  if (ask !== undefined && ask.kind !== "question") return undefined;
  madeBy(draft, node);
  draft.chips = tupleChips(node, "shaped", context);
  const askId = askIdOf(node, context);
  if (askId !== undefined) {
    draft.source = stableSourceOf(draft.tree ?? node, "ask", askId, context.key);
  }
  if (ask === undefined) {
    draft.state = "open";
    return draft;
  }
  draft.state = askState(ask);
  if (node.document !== undefined) {
    draft.text = ask.question;
    if (ask.question_tree === undefined) delete draft.tree;
    else draft.tree = ask.question_tree;
  }
  if (ask.answer !== null) draft.answer = answerOf(ask.answer);
  if ((ask.options ?? []).length > 0) {
    draft.options = (ask.options ?? []).map((option) => ({
      ...(option.description === undefined || option.description === ""
        ? {}
        : {
            description: option.description,
            ...(option.description_tree === undefined
              ? {}
              : { descriptionTree: option.description_tree }),
          }),
      label: option.label,
      ...(option.label_tree === undefined ? {} : { labelTree: option.label_tree }),
    }));
  }
  if (ask.multiple) draft.multiple = true;
  // An open question is what the fold unfolds the path to, whatever its depth (R6).
  draft.question = ask.state === "open";
  draft.waiting =
    ask.state === "open" &&
    ask.waiting_on === "human" &&
    context.viewer !== undefined &&
    ask.waiting_on_person === context.viewer;
  return draft;
}

/** A turn of the conversation, a comment or a message: who wrote it and when, and the record
 *  its words are, which its body's links resolve against, kept by the reading of its words
 *  (`stableSourceOf`). A turn its author corrected keeps its place (INFRA-1523, R2 and R3): an
 *  edited one keeps its words and carries when it was last edited (`editedAt`); a deleted one
 *  carries when and by whom (`deletedAt`, `deletedBy`) and holds the deleted line as its text
 *  (`deletedPostLine`) and no reading, so no line, body, filter or citation read from the node
 *  reads any part of its body. It carries no `editedAt`: the deleted line already says what
 *  became of the text. The marks are the comments read's and the messages read's, by the post's
 *  id (`KeptReads.posts`): the tree read holds neither.
 *
 * A comment on a node's text is no row of either outline (R9, R10): it stays in the answer, and
 * the margin beside that node draws it (root R41). A post on the issue, a comment tied to no
 * text, is a row, with its replies under it. */
function draftTurn(
  node: TreeAnswerNode,
  _answer: TreeAnswer,
  context: TreeDraftContext
): Draft | undefined {
  if (node.supertag !== "comment" && node.supertag !== "message") return undefined;
  if (onNodeText(node, context)) return undefined;
  const draft = draftOf(node, node.supertag, context);
  madeBy(draft, node);
  const marks = context.kept.posts.get(node.id);
  if (marks?.deleted != null) {
    // The read answers a deleted post with no body, and no mark of the delete: the line is made
    // from the delete alone, so it holds no part of the body even where a read still carries
    // one, and the post keeps no reading.
    draft.deletedAt = marks.deleted.at;
    if (marks.deleted.by !== null) draft.deletedBy = marks.deleted.by;
    draft.text = deletedPostLine(marks.deleted);
    delete draft.tree;
  } else if (marks?.editedAt != null) {
    draft.editedAt = marks.editedAt;
  }
  draft.source = stableSourceOf(draft.tree ?? node, node.supertag, node.id, context.key);
  return draft;
}

/**
 * Whether `node` is a comment on a node's text, or a reply in such a comment's thread (R9, R10:
 * no row of either outline, since the margin beside that node draws it, root R41). The tree read
 * answers no anchor on a comment node: the comments read does (`Comment.anchor`, the text's block
 * and its quote, `KeptReads.anchored`), and the read hangs a comment anchored inside a typed
 * block under that block's node. So a comment is on a node's text where the comments read anchors
 * it and the read hangs it under a node other than the issue's own. The second half is why the
 * rule reads the read's placement too: a comment the read hangs under the issue - anchored in a
 * document whose block the answer holds no node for, as a `decisions.md` section's Change is -
 * has no node for the margin to draw it beside, and is drawn as a post. A reply the read hangs
 * under such a comment is in its thread, which the margin draws whole. Until every answer of the
 * read carries the comments on each node's text, a comment on the text of a node an answer lacks
 * is a post of that answer. A chain of parents that runs in a circle ends where it comes round.
 */
function onNodeText(
  node: TreeAnswerNode,
  context: TreeDraftContext,
  passed?: Set<string>
): boolean {
  const { parent } = node;
  if (node.supertag !== "comment" || parent === null || passed?.has(node.id)) return false;
  if (context.kept.anchored.has(node.id)) return parent.supertag !== "issue";
  if (parent.supertag !== "comment") return false;
  const above = context.nodeOf(parent.id);
  if (above === undefined) return false;
  const seen = passed ?? new Set<string>();
  seen.add(node.id);
  return onNodeText(above, context, seen);
}

/** A callout or an image: a typed block with its words and nothing more. */
function draftBlock(
  node: TreeAnswerNode,
  _answer: TreeAnswer,
  context: TreeDraftContext
): Draft | undefined {
  if (node.supertag !== "callout" && node.supertag !== "image") return undefined;
  return draftOf(node, node.supertag, context);
}

/** A prose node: a block of a document's text that opens no typed block - a paragraph, a list, a
 *  heading - drawn by its kind and its words and nothing more, whether the read names it by the
 *  supertag `prose` or answers it as a plain block of this issue (`isPlainBlock`): a plain block
 *  the read marks as another issue's is a block of that issue's text, which the conversation of
 *  this issue never wrote. It stands under the parent the read gives it, as every draft whose
 *  step names no parent does (`placeOf`). */
function draftProse(
  node: TreeAnswerNode,
  _answer: TreeAnswer,
  context: TreeDraftContext
): Draft | undefined {
  if (
    node.supertag !== "prose" &&
    !(isPlainBlock(node, context) && !isForeign(node, context.key))
  ) {
    return undefined;
  }
  return draftOf(node, "prose", context);
}

/**
 * Whether `node` is a plain block, which the read's `kind` filter takes as prose by the server's
 * kind rule (`nodeKind` in `api/issue_tree_kinds.go`) while every read answers it as a block node
 * of the supertag `block`: a block node that is no finding and whose text opens no typed block.
 * The read types a block by the typed block its text opens and answers that type as its
 * supertag (`typedNodeOf` reads the typed node of that reading), an ask's block's `ask` among
 * them, so a block of the supertag `block` opens none. A finding is a block whose id names its
 * lens (`lens/<uuid>#<block id>`, as `lenses/store-link.ts` reads a finding's read id) or whose
 * parent is a lens, so it is never drafted as prose, even where nothing drafts its lens. A block
 * of an ask, a comment or a message is a node of its item's own block kind (`comment_block`),
 * part of that item, and no block node.
 */
function isPlainBlock(node: TreeAnswerNode, context: TreeDraftContext): boolean {
  if (node.kind !== "block" || node.supertag !== "block" || node.id.startsWith("lens/")) {
    return false;
  }
  return !(
    node.parent !== null &&
    (node.parent.kind === "lens" || context.nodeOf(node.parent.id)?.supertag === "lens")
  );
}

/** A reading: an agent's reading of the ask or the decision it reads, never of the issue, drawn
 *  as the outline drew one, by its kind and its words and nothing more. It stands under the
 *  parent the read gives it, the ask or the decision its `part_of` reaches, as every draft whose
 *  step names no parent does (`placeOf`), and nowhere else. */
function draftReading(
  node: TreeAnswerNode,
  _answer: TreeAnswer,
  context: TreeDraftContext
): Draft | undefined {
  if (node.supertag !== "reading") return undefined;
  return draftOf(node, "reading", context);
}

/** A child issue, with its row as the issue read carries it (`TreeNode.child`). The issue itself
 *  is the outline's root, which `assemble` makes, and no draft. */
function draftChild(
  node: TreeAnswerNode,
  _answer: TreeAnswer,
  context: TreeDraftContext
): Draft | undefined {
  if (node.supertag !== "issue" || context.treeIdOfNode(node) === context.key) return undefined;
  const draft = draftOf(node, "child_issue", context);
  const child = context.kept.children.get(node.id);
  if (child !== undefined) draft.child = child;
  return draft;
}

/** A draft of `node` as every kind starts one: its tree id, its words and their reading
 *  (`readingOf`), its address (`referenceOf`), and, where the read sent a reading, the document a
 *  typed block is written in, which the links of the body drawn from that reading resolve
 *  against, kept by that reading (`stableSourceOf`). The order and the read id are
 *  `treeDraftsFrom`'s. */
function draftOf(node: TreeAnswerNode, type: NodeType, context: TreeDraftContext): Draft {
  const draft = blank(context.treeIdOfNode(node), type, 0);
  draft.text = node.text;
  const reference = referenceOf(node, context);
  if (reference !== undefined) draft.reference = reference;
  const reading = readingOf(node);
  if (reading !== undefined) {
    draft.tree = reading;
    if (node.document !== undefined) {
      draft.source = stableSourceOf(reading, "artifact", node.document.artifact_id, context.key);
    }
  }
  return draft;
}

/** Each typed block's reading cut to its own content (`readingOf`), by the reading it was cut
 *  from. A re-read whose answer is unchanged keeps each reading's object, and so a row is handed
 *  the cut it was handed before, and the source kept by that cut (`stableSourceOf`), and its
 *  words are not drawn again. */
const cutReadings = new WeakMap<DisplayTree, DisplayTree>();

/** The reading of `node`'s words that a row and a chip draw. The read sends a block's reading
 *  as the blocks read does, of the block's whole stored text: a typed block's reading is the one
 *  typed node, its type and its attributes on it, which the row draws apart (its kind, its chips,
 *  its facts). So a block whose reading is that one node of its own kind (`typedNodeOf`) reads as
 *  the node's own content, as the server's other reads cut a typed block's text to its content,
 *  the same object for the same reading (`cutReadings`). Every other reading is the node's words
 *  as sent. A view's line (`presets.ts`) reads it here too. */
export function readingOf(node: TreeAnswerNode): DisplayTree | undefined {
  const reading = node.text_tree;
  const content = typedNodeOf(node)?.content;
  if (reading === undefined || !Array.isArray(content)) return reading;
  let cut = cutReadings.get(reading);
  if (cut === undefined) {
    cut = { ...reading, content };
    cutReadings.set(reading, cut);
  }
  return cut;
}

/** The one node of `node`'s own kind its reading is, where the read sent a typed block's reading,
 *  which carries the attributes the block's opening line writes (`attrs`); undefined for any
 *  other reading. */
function typedNodeOf(node: TreeAnswerNode): DisplayTree | undefined {
  const content = node.text_tree?.content;
  if (!Array.isArray(content) || content.length !== 1) return undefined;
  const only = content[0] as DisplayTree;
  return only.type === node.supertag ? only : undefined;
}

/**
 * The pointers `node`'s `block[]` attribute `name` holds, in the order they are written, none
 * where it holds none: the read's `attributes`, which hold the values the store states of the
 * node, else the attribute of that name on its reading's typed node (`typedNodeOf`), the list of
 * references the block's opening line writes. The store states no pointer as a value: each is a
 * tuple, which the read answers only where the answer holds its far end by the address the tuple
 * names (`issue_tree_read.go`'s `answer`). So a pointer at a task a write removed has none, and
 * so has one whose tuple names the task by its document's address where the answer keys the node
 * by the issue's tree (`<KEY>/plan-md#t1` beside `<KEY>/tree#t1`), and each is read from the
 * block's own words, as the blocks read listed them.
 */
function pointersOf(node: TreeAnswerNode, name: string): readonly string[] {
  const attrs = typedNodeOf(node)?.attrs;
  const written =
    node.attributes?.[name] ??
    (typeof attrs === "object" && attrs !== null
      ? (attrs as Record<string, unknown>)[name]
      : undefined);
  const value = Array.isArray(written)
    ? written.filter((one) => typeof one === "string").join(" ")
    : written;
  return typeof value === "string" ? (parseBlockPointerValue(value, POINTER_ITEMS) ?? []) : [];
}

/** When the node was made and who made it, as the read answers them: what the outline draws for
 *  an ask, a comment, a message and a result, and for no document block, decision or child
 *  issue. The read lists every author of the node's first version, whoever made it first ahead
 *  of the rest, and a row draws one name: the first, and none where the list is empty or null;
 *  and no time where the store records none. */
function madeBy(draft: Draft, node: TreeAnswerNode): void {
  if (node.made_at !== null) draft.at = node.made_at;
  const author = node.author?.[0];
  if (author !== undefined) draft.author = author;
}

/**
 * The node's own `dispatch://` address, which the kept reads are matched by and a chip naming it
 * links: the address the coverage, results or decisions read gives a requirement, a check or a
 * decision where it holds one, as the outline's draft carried it; an ask's, a comment's and a
 * message's own; and a typed block's in the document its `document` names, by its block id. A
 * node of another issue is addressed in that issue. Anything else has none.
 *
 * A typed block's row is addressed by its document's slug, as the outline addressed every block
 * it drew (`artifact/<slug>#b-<id>`). A chip addresses its target as the server's reads and the
 * outline's chips did (`chipTo`, `specBlocks: "spec"`): a block of the issue's spec as
 * `spec#b-<id>`, which the page draws as the spec's own path.
 */
function referenceOf(
  node: TreeAnswerNode,
  context: TreeDraftContext,
  specBlocks: "artifact" | "spec" = "artifact"
): string | undefined {
  const kept = keptReference(node, context);
  if (kept !== undefined) return kept;
  const key = node.issue ?? context.key;
  if (node.supertag === "comment" || node.supertag === "message") {
    return buildDispatchReference({ id: node.id, key, kind: node.supertag });
  }
  const ask = node.supertag === "ask" ? askIdOf(node, context) : undefined;
  if (ask !== undefined) return buildDispatchReference({ id: ask, key, kind: "ask" });
  if (node.document === undefined) return undefined;
  const block = { id: blockIdOfNode(node) };
  return specBlocks === "spec" && isSpecBlock(node, context)
    ? buildDispatchReference({ block, key, kind: "spec" })
    : buildDispatchReference({ block, key, kind: "artifact", slug: node.document.slug });
}

/** The address the read that tells about the node gives it, where that read holds a row for it:
 *  the coverage read's for a requirement, the results read's for a check, the decisions read's
 *  for a decision. */
function keptReference(node: TreeAnswerNode, context: TreeDraftContext): string | undefined {
  const id = context.treeIdOfNode(node);
  switch (node.supertag) {
    case "check":
      return context.kept.checks.get(id)?.ref;
    case "decision":
      return context.kept.decisions.get(id)?.ref;
    case "requirement":
      return context.kept.requirements.get(id)?.ref;
    default:
      return undefined;
  }
}

/** The id of the ask an ask node is: the asks read's, matched by the node's tree id, else the
 *  node's own where it is no typed block, whose id would be the block's. */
function askIdOf(node: TreeAnswerNode, context: TreeDraftContext): string | undefined {
  const ask = context.kept.asks.get(context.treeIdOfNode(node));
  return ask?.id ?? (node.document === undefined ? node.id : undefined);
}

/** The chips naming the nodes `node`'s tuples of `kind` point at, one for each node, in the
 *  answer's order. */
function tupleChips(node: TreeAnswerNode, kind: string, context: TreeDraftContext): NodeChip[] {
  const chips = new Map<string, NodeChip>();
  for (const tuple of context.tuplesFrom(node.id)) {
    const chip = tuple.kind === kind ? chipTo(tuple.to.id, context) : undefined;
    if (chip !== undefined && !chips.has(chip.ref)) chips.set(chip.ref, chip);
  }
  return [...chips.values()];
}

/**
 * The chips of what a check or a plan task `needs`, one per task needed, labelled as the outline
 * labels them: each its tuples name, in the order they first name each, then each pointer of its
 * `needs` attribute (`pointersOf`) no tuple names, in the order it is written. The read answers a
 * tuple only where the answer holds its far end (`issue_tree_read.go`'s `answer`), so a pointer at
 * a task a write removed has none, and the outline drew a chip for every pointer the attribute
 * holds, named by the batched reference read (`chipOf`), which names a removed task as gone; a
 * pointer no tuple names is drawn so. The server accepts one task needed by a pinned and an
 * unpinned pointer at once (`#b-a1@v1 #b-a1`), and both need the one task, so the chips are told
 * apart by the node they name with any version pin dropped (`pinDropped`, `nodeIdOf`). A task
 * named both ways is drawn by its unpinned chip, which names the task's current words; one only
 * pinned ties name, by the first one's.
 */
function needsChips(node: TreeAnswerNode, context: TreeDraftContext): NodeChip[] {
  const needed = new Map<string, { readonly chip: NodeChip; readonly pinned: boolean }>();
  const keyOf = (reference: string): { readonly key: string; readonly pinned: boolean } => {
    const { pinned, target } = pinDropped(reference);
    return { key: context.nodeIdOf(target) ?? target, pinned };
  };
  const offer = (chip: NodeChip | undefined): void => {
    if (chip === undefined) return;
    const { key, pinned } = keyOf(chip.ref);
    const held = needed.get(key);
    if (held === undefined || (held.pinned && !pinned)) needed.set(key, { chip, pinned });
  };
  for (const tuple of context.tuplesFrom(node.id)) {
    if (tuple.kind === "needs") offer(chipTo(tuple.to.id, context));
  }
  const tied = new Set(needed.keys());
  for (const pointer of pointersOf(node, "needs")) {
    if (!tied.has(keyOf(pointer).key)) offer(context.chipOf(pointer));
  }
  return [...needed.values()].map(({ chip }) => ({
    ...chip,
    label: blockAttributeLabel("needs"),
  }));
}

/**
 * What `reference` names with any version pin dropped, a block's (`#b-a1@v1`) and a document's
 * (`artifact/plan-md@v1`), as the reference its route writes with none (`target`), and whether
 * it pinned one (`pinned`). A reference that does not parse is its own target, pinning nothing.
 */
function pinDropped(reference: string): { readonly pinned: boolean; readonly target: string } {
  const route = parseNodeReference(reference);
  if (route === undefined) return { pinned: false, target: reference };
  const block = "block" in route ? route.block : undefined;
  const version = "version" in route ? route.version : undefined;
  return {
    pinned: block?.pin !== undefined || version !== undefined,
    target: buildDispatchReference({
      ...route,
      ...(block === undefined ? {} : { block: { id: block.id } }),
      ...(version === undefined ? {} : { version: undefined }),
    }),
  };
}

/**
 * The chip naming the node `readId` names: addressed by that node's address as a chip addresses
 * it (`referenceOf`, a block of the issue's spec as `spec#b-<id>`, and an issue's own), and named
 * by the node the answer holds, by the outline's rule for the kind of target it is
 * (`chipReading`). Where the answer holds no words for it, or does not hold it and the tuple
 * names it by an address, the batched reference read names it (`chipOf`), as it names every chip
 * the outline draws. A target with no address has no chip, as in the outline.
 */
function chipTo(readId: string, context: TreeDraftContext): NodeChip | undefined {
  const target = context.nodeOf(readId);
  if (target === undefined) {
    return parseDispatchReference(readId) === undefined ? undefined : context.chipOf(readId);
  }
  const ref =
    referenceOf(target, context, "spec") ??
    (target.supertag === "issue"
      ? buildDispatchReference({ key: target.id, kind: "issue" })
      : undefined);
  if (ref === undefined) return undefined;
  const reading = chipReading(target);
  return reading === undefined
    ? context.chipOf(ref)
    : { answered: true, gone: false, reading, ref };
}

/** How a chip names a node the answer holds, by the outline's rule for each kind of target
 *  (`treeContext`), from the node's own words: a question, a comment and a message by the reading
 *  of its words, the read's own else its stored text's (`storedReading`); an issue by its title
 *  read as one text run (`titleReading`); and a block by its reading, with the page's word for
 *  its kind where those words project to none. None where the node holds no such words - a block
 *  the read sent no reading for, one whose reading holds no words - which the batched reference
 *  read then names (`chipTo`), by the page's word for its kind for a block of this issue. */
function chipReading(target: TreeAnswerNode): ReferenceNameReading | undefined {
  if (target.supertag === "ask" || target.supertag === "comment" || target.supertag === "message") {
    return { tree: target.text_tree ?? storedReading(target.text, target) };
  }
  if (target.supertag === "issue") return { tree: titleReading(target.text) };
  const reading = readingOf(target);
  if (reading === undefined || firstWordedBlock(reading) === undefined) return undefined;
  const kind = NODE_TYPES.find((type) => type === target.supertag);
  return kind === undefined
    ? { tree: reading }
    : { fallback: NODE_TYPE_NAMES[kind], tree: reading };
}

/** Whether `node` names anything by `kind`: a tuple of that kind, or a pointer of its attribute of
 *  that name (`pointersOf`), which names a node outside the answer too - a requirement of the
 *  issue above, which the read answers no tuple to. */
function namesAny(node: TreeAnswerNode, kind: string, context: TreeDraftContext): boolean {
  if (context.tuplesFrom(node.id).some((tuple) => tuple.kind === kind)) return true;
  return pointersOf(node, kind).length > 0;
}

/** Whether the read marks `node` as another issue's: one whose `issue` is not `key`. */
export function isForeign(
  node: TreeAnswerNode,
  key: string
): node is TreeAnswerNode & { readonly issue: string } {
  return node.issue !== undefined && node.issue !== key;
}

/** Whether `answer`, the Plan view read's answer, holds a plan task that is not gone: one of this
 *  issue, or of an issue under it, which that read answers with the issue's own
 *  (`IssueTreeReads.holdsPlanTask`). False where it holds no answer. */
export function holdsPlanTask(answer: TreeAnswer | undefined): boolean {
  return (answer?.nodes ?? []).some(
    (node) => node.supertag === "plan_task" && node.state !== "gone"
  );
}

/** The page's other reads, each row by the tree id of the node it tells about (`KeptReads`). A
 *  decision's row is the one whose node the read answers (`decisionNodes`). A check's row is its
 *  block in its document; a check of this issue that names no document, a node of its own tree
 *  once it has converted, plays Verification by its kind (`blocksOfRole`), so the row of its
 *  block in the Verification document tells about it, by the check's own tree id. */
function keptReads(
  reads: IssueTreeReads,
  context: TreeContext,
  answer: TreeAnswer,
  idOf: (node: TreeAnswerNode) => string
): KeptReads {
  const rows = reads.results?.checks ?? [];
  const decisions = reads.decisions?.entries ?? [];
  const nodes = decisionNodes(decisions, answer);
  const verification = roleDocument(reads.issue, "verification")?.id;
  const checks = indexBy(rows, (check) => {
    const slug = context.slugOf(check.artifact_id);
    return slug === undefined ? undefined : blockNodeId(slug, check.block_id);
  });
  const verificationSlug = verification === undefined ? undefined : context.slugOf(verification);
  if (answer.converted && verificationSlug !== undefined) {
    for (const node of answer.nodes ?? []) {
      if (node.supertag !== "check" || node.document !== undefined) continue;
      if (isForeign(node, context.key)) continue;
      const row = checks.get(blockNodeId(verificationSlug, blockIdOfNode(node)));
      if (row !== undefined) checks.set(idOf(node), row);
    }
  }
  return {
    anchored: new Set(
      (reads.comments ?? []).flatMap((comment) => (comment.anchor == null ? [] : [comment.id]))
    ),
    asks: indexBy([...asksOf(reads).values()], (ask) => askNodeId(ask, context.slugOf)),
    checks,
    children: indexBy(reads.issue.children ?? [], (child) => child.key),
    coverageNotes: coverageNotes(reads.coverage),
    decisions: indexBy(decisions, (decision) => {
      const node = nodes.get(decision.ref);
      return node === undefined ? undefined : idOf(node);
    }),
    planned: reads.holdsPlanTask === true,
    posts: postMarksOf(reads),
    requirements: indexBy(reads.coverage?.requirements ?? [], (requirement) =>
      blockNodeId(context.specSlug, requirement.block_id)
    ),
    results: indexBy(
      rows.flatMap((check) => check.results ?? []),
      (result) => result.id
    ),
    verification,
  };
}

/** What the author of each comment and message did to it (`PostMarks`), by the post's id: the
 *  comments read's and the messages read's, which carry the marks the tree read does not. A post
 *  nobody deleted or edited has none, and a deleted post has no edit mark, since the deleted
 *  line says what became of its text. */
function postMarksOf(reads: Pick<IssueTreeReads, "comments" | "messages">): Map<string, PostMarks> {
  const marks = new Map<string, PostMarks>();
  const posts = [
    ...(reads.comments ?? []),
    ...(reads.messages ?? []).flatMap((thread) => [thread.message, ...(thread.replies ?? [])]),
  ];
  for (const post of posts) {
    const deleted = postDeletion(post);
    const editedAt = deleted === null ? (post.edited_at ?? null) : null;
    if (deleted !== null || editedAt !== null) marks.set(post.id, { deleted, editedAt });
  }
  return marks;
}

/** `rows` by the id `idOf` gives each, a row with none left out and a later row over an earlier
 *  one of the same id. */
function indexBy<Row>(
  rows: readonly Row[],
  idOf: (row: Row) => string | undefined
): Map<string, Row> {
  const index = new Map<string, Row>();
  for (const row of rows) {
    const id = idOf(row);
    if (id !== undefined) index.set(id, row);
  }
  return index;
}

/** The issue's asks by id: the issue read's open asks and the asks read, the asks read's where
 *  both hold one, since it is the newer read of the two. */
function asksOf(reads: TreeIdReads): Map<string, TreeAsk> {
  const asks = new Map<string, TreeAsk>();
  for (const ask of [...(reads.issue.open_asks ?? []), ...(reads.asks ?? [])]) {
    asks.set(ask.id, ask);
  }
  return asks;
}

/** The node id the outline drew an ask by: the block it was indexed from, in the document that
 *  holds that block (`block_artifact`), else the document it is anchored in, where it names a
 *  block; its own id otherwise. */
function askNodeId(ask: TreeAsk, slugOf: (artifactId: string) => string | undefined): string {
  const slug = ask.block_artifact?.slug ?? slugOf(ask.artifact_id ?? "");
  const block = ask.block_id ?? undefined;
  return slug !== undefined && block !== undefined ? blockNodeId(slug, block) : ask.id;
}

/**
 * The node each ask of the issue is drawn by, by the ask's own id, for each ask a draft stands
 * for: an ask the page keeps (`asksOf`) by the node id the outline drew it by (`askNodeId`), and
 * an ask node of the answer by its tree id. It is how a decision of the decisions read names the
 * question it settles (`DecisionEntry.from_ask`, `tree-drafts.ts`'s `addDecisions`).
 */
export function askNodeIds(
  drafts: ReadonlyMap<string, Draft>,
  answer: TreeAnswer,
  reads: TreeIdReads
): ReadonlyMap<string, string> {
  const idOf = nodeIdsIn(reads);
  const slugs = new Map(
    (reads.issue.artifacts ?? []).map((artifact) => [artifact.id, artifact.slug])
  );
  const places = new Map<string, string>();
  for (const node of answer.nodes ?? []) {
    // An ask node that is no typed block is the ask itself, by its own id.
    if (node.supertag === "ask" && node.document === undefined) {
      places.set(node.id, idOf(node));
    }
  }
  for (const ask of asksOf(reads).values()) {
    places.set(
      ask.id,
      askNodeId(ask, (artifactId) => slugs.get(artifactId))
    );
  }
  return new Map([...places].filter(([, id]) => drafts.has(id)));
}

/** A block as the blocks read answers one, with the words the tree read answers beside it, so a
 *  reader of the blocks read reads a block of the tree read with no change, and a reader of the
 *  words finds them here. */
export interface TreeBlock extends ArtifactBlock {
  readonly text: string;
  readonly text_tree?: DisplayTree;
}

/** The node kinds each role's query takes: what plays the role on an issue that has converted,
 *  for a node that names no document. */
const ROLE_KINDS: Readonly<Record<TreeFilter, readonly string[]>> = {
  plan: ["plan_task"],
  spec: ["criterion", "requirement"],
  verification: ["check"],
};

/** A node the tree read answers as a typed block of a document. */
type PlacedNode = TreeAnswerNode & { readonly document: NonNullable<TreeAnswerNode["document"]> };

/**
 * The id of the document that plays `role` for the issue: the one the issue read names
 * (`primarySpec`, `roleDocument`), which is the server's own answer to which document plays the
 * part, as `useIssueTree` reads it. The issue read's two role fields go once every caller reads
 * the tree read's ids instead, and while the issue has not converted the tree read answers the
 * same ids (`documents`), so those stand where the issue read names none.
 */
function roleDocumentId(
  answer: TreeAnswer,
  role: TreeFilter,
  issue: IssueDetails
): string | undefined {
  const named = role === "spec" ? primarySpec(issue) : roleDocument(issue, role);
  return named?.id ?? (answer.converted ? undefined : (answer.documents?.[role] ?? undefined));
}

/** The item kinds a pointer of a `block[]` attribute may name, each of them, so a value is read
 *  as a pointer list whatever the attribute declares. */
const POINTER_ITEMS: readonly BlockItemKind[] = ["ask", "comment", "message", "issue"];

/** A node's attributes as the blocks read lists a block's, in the order the node gives them,
 *  each its value as the read answers it, the text the block's opening line writes, with its kind
 *  read from that value: a list of pointers - references joined by one space, as
 *  `parseBlockPointerValue` reads one - is `block[]`, which is what a reader follows as pointers,
 *  and anything else reads as text. This module reads no block schema - the e2e helpers reach
 *  it through `issue-tree.ts` and cannot load the schema's JSON - and no reader of these blocks
 *  on the issue page reads any other kind: a drawing that needs the schema's own kind reads the
 *  schema itself (`typed-block-drawing.ts`, `ReferencedBy.tsx`). */
function attributesOf(node: TreeAnswerNode): BlockAttributeValue[] {
  return Object.entries(node.attributes ?? {}).map(([name, value]) => ({
    kind:
      value !== "" && parseBlockPointerValue(value, POINTER_ITEMS) !== undefined
        ? "block[]"
        : "string",
    name,
    value,
  }));
}

/**
 * One node as the blocks read answers a block. The tree read answers no markdown range and no
 * edit token: both ends of the range are the block's place (its `document.position`, else its
 * place in the walk), which keeps the order a reader sorts the blocks by (`from`), and the token
 * is empty, so a block from here is never an edit's precondition. The comments and asks that
 * reference it are the ones the read hangs under it (`hungUnder`).
 */
function blockOf(
  node: TreeAnswerNode,
  place: number,
  hung: ReadonlyMap<TreeAnswerNode, { asks: number; comments: number }>
): TreeBlock {
  return {
    attributes: attributesOf(node),
    from: place,
    id: blockIdOfNode(node),
    references: hung.get(node) ?? { asks: 0, comments: 0 },
    text: node.text,
    text_tree: node.text_tree,
    to: place,
    token: "",
    type: node.supertag,
  };
}

/** How many asks and comments the read hangs under each node, by the node itself: each under
 *  the node its `parent` names (`parentsIn`), so a comment on one document's block is not counted
 *  on another document's block of the same id. */
function hungUnder(answer: TreeAnswer): Map<TreeAnswerNode, { asks: number; comments: number }> {
  const counts = new Map<TreeAnswerNode, { asks: number; comments: number }>();
  for (const [node, parent] of parentsIn(answer.nodes ?? [])) {
    if (node.supertag !== "ask" && node.supertag !== "comment") continue;
    const count = counts.get(parent) ?? { asks: 0, comments: 0 };
    if (node.supertag === "ask") count.asks += 1;
    else count.comments += 1;
    counts.set(parent, count);
  }
  return counts;
}

/**
 * The typed blocks of the document that plays `role` for the issue, from the tree read, in the
 * shape the blocks read of that document (`artifactBlocksQuery`) answered them, so a reader of
 * that read reads these with no change; undefined while there is no answer, as that read's data
 * is before it answers.
 *
 * A node's role is read node by node. The tree read answers each typed block of a role document
 * once, with a `document` member naming its document and its place there, so a node that names
 * its `document` plays the role of that document (`roleDocumentId`), and none where that
 * document plays no role. On an issue that has converted, a node of this issue that names no
 * document - a criterion, written into the issue's own tree and into no document - plays the role
 * whose query takes its kind (`ROLE_KINDS`), by that alone, so the spec's blocks hold its
 * criteria beside its statements. The blocks stand in their place's order in the role's document
 * where each names it, and in the walk's order, which is the order the role's query reads them
 * in, where any is a node of the issue's own tree.
 */
export function blocksOfRole(
  answer: TreeAnswer | undefined,
  role: TreeFilter,
  issue: IssueDetails
): TreeBlock[] | undefined {
  if (answer === undefined) return undefined;
  const hung = hungUnder(answer);
  const documentId = roleDocumentId(answer, role, issue);
  const kinds = ROLE_KINDS[role];
  const inRoleDocument = (node: TreeAnswerNode): node is PlacedNode =>
    documentId !== undefined && node.document?.artifact_id === documentId;
  const members = (answer.nodes ?? []).flatMap((node, place) => {
    if (node.issue !== undefined) return [];
    const plays =
      node.document === undefined
        ? answer.converted && kinds.includes(node.supertag)
        : inRoleDocument(node);
    return plays ? [{ node, place }] : [];
  });
  const placed = members.flatMap(({ node }) => (inRoleDocument(node) ? [node] : []));
  if (placed.length === members.length) {
    return placed
      .sort((left, right) => left.document.position - right.document.position)
      .map((node) => blockOf(node, node.document.position, hung));
  }
  return members.map(({ node, place }) => blockOf(node, place, hung));
}

/**
 * The pages of one read as one answer: every page's nodes and tuples in the order the pages
 * came, so the walk's order holds across them, what describes the issue rather than a page
 * (`converted`, `tree`, `view_of`, `documents`, `comments_before`) from the first, and the last
 * page's `cursor` and `remaining`, so the answer names the page after the last one read, and how
 * many nodes are left after it, where one remains. One page is its own answer. A next page the
 * reader's press reads merges into the answer the page holds by the same rule, that answer as the
 * first page (`tree-read.ts`' `readNextTreePage`). Here rather than beside the read
 * (`tree-read.ts`' `readTree`), so the e2e helpers, which load this module and not that one,
 * merge the pages the page received by the same rule.
 */
export function mergeTreePages(pages: readonly [TreeAnswer, ...TreeAnswer[]]): TreeAnswer {
  const [first] = pages;
  if (pages.length === 1) return first;
  const last = pages[pages.length - 1] ?? first;
  const merged: TreeAnswer = {
    ...first,
    cursor: last.cursor ?? null,
    nodes: pages.flatMap((page) => page.nodes ?? []),
    remaining: last.remaining,
    tuples: pages.flatMap((page) => page.tuples ?? []),
  };
  return merged;
}

/**
 * How many nodes the read holds after the pages `answer` holds (`remaining`), the exact number the
 * server answers beside a `cursor`; undefined where the answer names no next page
 * (`nextTreeCursor`) or gives no count.
 */
export function nodesRemaining(answer: TreeAnswer): number | undefined {
  const { remaining } = answer;
  return nextTreeCursor(answer) !== undefined && typeof remaining === "number"
    ? remaining
    : undefined;
}

/**
 * The cursor of the page after `page` in its read, or undefined where `page` is the last: one
 * whose cursor is null, left out of the body, or empty, as a server that drops an empty field
 * writes it. The one rule every walk of the read ends by: the page's own (`tree-read.ts`'
 * `readTree`) and the e2e helpers', which load this module and not that one, as they do for
 * `mergeTreePages`.
 */
export function nextTreeCursor(page: TreeAnswer): string | undefined {
  const { cursor } = page;
  return typeof cursor === "string" && cursor !== "" ? cursor : undefined;
}

/**
 * Where the next older comment page of an outline read's answer starts (`comments_before`,
 * `<made_at>,<id>` of the oldest comment the answer holds), which `?comments_before=` reads the
 * 200 comments older than; undefined where no older comment remains: null, left out, or empty, as
 * a view's answer, which answers none, leaves it.
 */
export function olderCommentsBefore(answer: TreeAnswer): string | undefined {
  const before = answer.comments_before;
  return typeof before === "string" && before !== "" ? before : undefined;
}

/**
 * How two comment positions (`<made_at>,<id>`, `comments_before`) stand in the read's order of
 * comments, by `made_at` and then by id: below 0 where `left` is the older, above 0 where it is the
 * newer, 0 where they are one.
 */
export function compareCommentPositions(left: string, right: string): number {
  const [leftAt, leftId] = splitPosition(left);
  const [rightAt, rightId] = splitPosition(right);
  return compareTimes(leftAt, rightAt) || compareIds(leftId, rightId);
}

/** A comment position's `made_at` and id: the text before its first comma, and the rest. */
function splitPosition(position: string): readonly [string, string] {
  const comma = position.indexOf(",");
  return comma < 0 ? [position, ""] : [position.slice(0, comma), position.slice(comma + 1)];
}

/** How two times the read writes stand, by the instants they name (`compareTimestamps`, which
 *  keeps the microseconds the read writes), and by their text where either is no timestamp. */
function compareTimes(left: string, right: string): number {
  return isTimestamp(left) && isTimestamp(right)
    ? compareTimestamps(left, right)
    : compareIds(left, right);
}

/** Two ids in byte order, as the read orders ties. */
function compareIds(left: string, right: string): number {
  return left === right ? 0 : left < right ? -1 : 1;
}

/**
 * `answer`, an outline read's answer, with `page` merged into it: the older comment page
 * `?comments_before=` answered for it, which holds the 200 comments older than the answer's
 * oldest, each with the nodes under it and their tuples, and its own `comments_before`. The
 * page's nodes stand where the read's order puts them (R15), as `mergeTreePages` keeps the
 * walk's order across pages: each comment of the page with the nodes under it, under the node its
 * `parent` names where the answer holds that node - an ask, a comment of another page - and else
 * under the issue, as the page places it; among that node's children before the first the read
 * orders after it (ranked siblings by `rank`, ties by id, then the rest by `made_at`, ties by
 * id), else after the last of them and every node under it. A node the answer holds already is
 * not added again. The answer then has the page's `comments_before`, so the next older page is
 * read from there, and none once the page says no older comment remains.
 */
export function mergeCommentPage(answer: TreeAnswer, page: TreeAnswer): TreeAnswer {
  const nodes = [...(answer.nodes ?? [])];
  const parents = new Map(nodes.map((node) => [node.id, node.parent]));
  const runs = commentRuns((page.nodes ?? []).filter((node) => !parents.has(node.id)));
  for (const run of runs) {
    const at = placeOfRun(nodes, run[0], answer.issue, parents);
    nodes.splice(at, 0, ...run);
    for (const node of run) {
      // If a page repeats an id, keep the last placement in the merged walk.
      const last = parents.has(node.id) ? nodes[lastPlaceOf(nodes, node.id)] : node;
      parents.set(node.id, last?.parent ?? null);
    }
  }
  return {
    ...answer,
    comments_before: page.comments_before ?? null,
    nodes,
    tuples: [...(answer.tuples ?? []), ...(page.tuples ?? [])],
  };
}

/** A comment page's nodes in runs, in the page's order: each node whose parent is no node of the
 *  page starts one, and each other node is in its parent's, after it. */
function commentRuns(nodes: readonly TreeAnswerNode[]): [TreeAnswerNode, ...TreeAnswerNode[]][] {
  const runs: [TreeAnswerNode, ...TreeAnswerNode[]][] = [];
  const runOf = new Map<string, TreeAnswerNode[]>();
  for (const node of nodes) {
    const run = node.parent === null ? undefined : runOf.get(node.parent.id);
    if (run === undefined) {
      const started: [TreeAnswerNode, ...TreeAnswerNode[]] = [node];
      runs.push(started);
      runOf.set(node.id, started);
    } else {
      run.push(node);
      runOf.set(node.id, run);
    }
  }
  return runs;
}

/**
 * Where in `nodes` the run of a comment page that `head` starts goes (`mergeCommentPage`): under
 * the node its `parent` names where `nodes` holds it, else under the issue's own node (`issue`),
 * before the first of that node's children the read orders after `head` (`sortsAfter`), else
 * where that node's branch ends. A parent's branch is every node after it whose chain of parents
 * reaches it, which the read's walk keeps together, each node after its parent.
 */
function placeOfRun(
  nodes: readonly TreeAnswerNode[],
  head: TreeAnswerNode,
  issue: string,
  parents: ReadonlyMap<string, TreeAnswerNode["parent"]>
): number {
  const named = head.parent === null ? -1 : lastPlaceOf(nodes, head.parent.id);
  const parentAt =
    named >= 0
      ? named
      : nodes.findIndex(
          (node) => node.supertag === "issue" && (node.id === issue || node.parent === null)
        );
  const parent = nodes[parentAt];
  if (parent === undefined) return nodes.length;
  const reaches = (node: TreeAnswerNode): boolean => {
    let above = node.parent;
    for (let steps = 0; above !== null && steps < nodes.length; steps += 1) {
      if (above.id === parent.id) return true;
      above = parents.get(above.id) ?? null;
    }
    return false;
  };
  for (let place = parentAt + 1; place < nodes.length; place += 1) {
    const node = nodes[place];
    if (node === undefined || !reaches(node)) return place;
    if (node.parent?.id === parent.id && sortsAfter(node, head)) return place;
  }
  return nodes.length;
}

/** The last placement of `id`, as the map of a walk's ids would see it. */
function lastPlaceOf(nodes: readonly TreeAnswerNode[], id: string): number {
  for (let at = nodes.length - 1; at >= 0; at -= 1) {
    if (nodes[at]?.id === id) return at;
  }
  return -1;
}

/** Whether the read orders `sibling` after `node` under one parent: ranked siblings first, by
 *  `rank` in byte order, ties by id; then the rest by `made_at`, ties by id. */
function sortsAfter(sibling: TreeAnswerNode, node: TreeAnswerNode): boolean {
  if (node.rank !== null) {
    if (sibling.rank === null) return true;
    return (compareIds(sibling.rank, node.rank) || compareIds(sibling.id, node.id)) > 0;
  }
  if (sibling.rank !== null) return false;
  const at =
    sibling.made_at === null || node.made_at === null
      ? 0
      : compareTimes(sibling.made_at, node.made_at);
  return (at || compareIds(sibling.id, node.id)) > 0;
}
