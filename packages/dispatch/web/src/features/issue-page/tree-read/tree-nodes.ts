import { primarySpec, roleDocument } from "../../../api/issue-cache";
import type {
  ArtifactBlock,
  BlockAttributeValue,
  CheckBlock,
  CheckResult,
  Decision,
  DisplayTree,
  IssueChild,
  IssueDetails,
  RequirementCoverage,
} from "../../../api/types";
import {
  buildDispatchReference,
  parseDispatchReference,
  parseNodeReference,
} from "../../refs/routes";
import { textSourceOf } from "../../refs/text-references";
import { checkFacts, resultFacts } from "../check-facts";
import { decisionNodeChanges, decisionNodes, inReviewList } from "../decision-review";
import type { IssueTreeReads, NodeChip, TreeAsk, TreeNode } from "../issue-tree";
import { lensChips, lensDrafts } from "../lenses/lens-drafts";
import { blockAttributeLabel, coverageGapKind, type GapKind, type NodeType } from "../page-words";
import {
  addDocumentChips,
  answerOf,
  askState,
  blank,
  blockNodeId,
  checkStanding,
  type Draft,
  decisionState,
  markKeptAndUnsure,
  plainWords,
  type TreeContext,
} from "../tree-drafts";
import type { TreeAnswer, TreeAnswerNode, TreeFilter, TreeTuple } from "./tree-read";

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
 * requirement's counts and gaps, a child's row - comes from the page's other reads, each row
 * matched to its node by its `dispatch://` address (`KeptReads`). A node's words are its
 * `text_tree`, read by the outline's word rules (`nodeWords`, `lineWords`), so a row reads as it
 * read when the outline was built from the page's other reads.
 *
 * Each kind the page draws is one step of `treeDraftSteps`. A node of any other kind - a
 * criterion, a reading, a lens and its kind, a finding, a risk, a constraint, an interaction -
 * reaches no step of this part's, makes no draft and stays in the answer, which the page keeps
 * whole (R7); a later part draws a kind of its own with one line in that list, and adds a member
 * to another step's draft with one line in `treeDraftMarks`. A check or a plan task of an issue
 * under this one is out of this part's scope and makes no draft either.
 *
 * `buildIssueTree` builds the tree from these drafts, so this module imports no code of
 * `issue-tree.ts`, which would close an import cycle (`biome.json`'s `noImportCycles`): the word
 * rules it reads are handed in (`TreeWords`), as `buildIssueTree` hands `chipLine` to
 * `treeContext`.
 */

/** The outline's reading of the issue's addresses and names (`treeContext`), with the two word
 *  rules of `issue-tree.ts` a draft's line and a chip's name are read by: what `buildIssueTree`
 *  hands `treeDraftsFrom`. */
export interface TreeWords extends TreeContext {
  /** The line a chip names a block by, from the block's reading (`chipLine`). */
  readonly chipLine: (reading: DisplayTree) => string;
  /** The words a node's one line is cut from (`lineWords`). */
  readonly lineWords: (node: Pick<TreeNode, "text" | "tree">) => string;
}

/** The page's other reads, each row by the tree id of the node it tells about (`treeIdOf`): the
 *  facts the answer does not hold, each row's `dispatch://` address read to a node id as the
 *  outline's builders read it (`blockNodeId`, `nodeIdOf`), so a row and its node meet by one
 *  address. */
export interface KeptReads {
  /** Each ask of the issue read's open asks and of the asks read, the asks read's where both
   *  hold it. */
  readonly asks: ReadonlyMap<string, TreeAsk>;
  /** Each check of the results read, with the results recorded against it, a check that names
   *  no document by its own id (`keptReads`). */
  readonly checks: ReadonlyMap<string, CheckBlock>;
  /** Each child of the issue read, by its key. */
  readonly children: ReadonlyMap<string, IssueChild>;
  /** Each decision of the decisions read. */
  readonly decisions: ReadonlyMap<string, Decision>;
  /** Each requirement of the coverage read: the statements, with their counts and gaps. */
  readonly requirements: ReadonlyMap<string, RequirementCoverage>;
  /** Each result of the results read, by its own id. */
  readonly results: ReadonlyMap<string, CheckResult>;
  /** The artifact id of the issue's Verification document (`roleDocument`): a check block of it
   *  is a check, which draws a state, and one of any other document is a block and nothing more,
   *  since the results read never answers for it. */
  readonly verification: string | undefined;
}

/** What a step and a mark read: the words and the names `buildIssueTree` hands in, the page's
 *  other reads by node (`kept`), and the answer, looked up by the read's own ids. A read id can
 *  name two nodes - blocks of two documents of an issue that has not converted - and a lookup by
 *  one answers the first the walk reaches, while a draft and its place are keyed on the node's
 *  own identity (`treeIdOfNode`, `placeOf`), so each of the two is a row. */
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
 * comment and a message, a callout and an image, and a child issue.
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
  draftChild,
];

/** The marks run on every draft, each in turn, once every node has been offered to the steps.
 *  This part adds none: a member of its drafts is its own step's. */
export const treeDraftMarks: readonly TreeDraftMark[] = [
  // A later part adds its mark here, one line each.
  lensChips,
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
  const nodes = nodesById(answer);
  const tuples = new Map<string, TreeTuple[]>();
  for (const tuple of answer.tuples) {
    const from = tuples.get(tuple.from);
    if (from === undefined) tuples.set(tuple.from, [tuple]);
    else from.push(tuple);
  }
  const idOfNode = nodeIdsIn(reads);
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
    changes: decisionNodeChanges(answer, idOfNode),
    drafted: (readId) => drafts.has(idOf(readId)),
    kept: keptReads(reads, context, answer, idOfNode),
    lineOf: (readId) => {
      const draft = offered ? drafts.get(idOf(readId)) : undefined;
      return draft === undefined ? undefined : context.lineWords(draft);
    },
    nodeOf: (readId) => nodes.get(readId),
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

/** The first draft a step of `treeDraftSteps` answers for `node`, in the list's order. */
function firstDraft(
  node: TreeAnswerNode,
  answer: TreeAnswer,
  context: TreeDraftContext
): Draft | undefined {
  for (const step of treeDraftSteps) {
    const draft = step(node, answer, context);
    if (draft !== undefined) return draft;
  }
  return undefined;
}

/**
 * The node each node of `order` hangs under: the nearest node before it in `order` that its
 * `parent` names, since a parent comes before its children and two nodes - blocks of two
 * documents - can share an id; else the answer's node of that id (`nodes`). A decision's Change
 * is the comment the read hangs under it by the same rule (`decision-review.ts`' `parentsIn`).
 */
function parentsIn(
  order: readonly TreeAnswerNode[],
  nodes: ReadonlyMap<string, TreeAnswerNode>
): Map<TreeAnswerNode, TreeAnswerNode> {
  const parents = new Map<TreeAnswerNode, TreeAnswerNode>();
  const latest = new Map<string, TreeAnswerNode>();
  for (const node of order) {
    const parent =
      node.parent === null ? undefined : (latest.get(node.parent) ?? nodes.get(node.parent));
    if (parent !== undefined) parents.set(node, parent);
    latest.set(node.id, node);
  }
  return parents;
}

/** What `treeIdOf` reads beside the answer: the issue, for its key and its documents' slugs, and
 *  the asks read, which names the block an ask was indexed from. */
export type TreeIdReads = Pick<IssueTreeReads, "asks" | "issue">;

/** No ask named by the block it was indexed from. */
const NO_ASK_IDS: ReadonlyMap<string, string> = new Map();

/**
 * The id the tree draws `node` of issue `key`'s answer by, read from the node itself, so two
 * nodes one read id names - blocks of two documents of an issue that has not converted, which
 * may share a block id - are told apart. The page's own ids are kept wherever the read names
 * enough to make one, so each place that reads a node's id - the rows, `documentOfNode`,
 * `pathToTarget`, `specSummary` - reads the tree by the ids it read before:
 *
 * - **A node of another issue** - the read marks it with that issue (`issue`) - keeps the read's
 *   id, even where its `document` names a document: a child's documents carry its parent's
 *   slugs, so the outline's `<slug>#b-<block id>` would name a block of this issue.
 * - **The issue itself** is its key, which is the outline's root.
 * - **A typed block of one of this issue's documents** is the outline's `blockNodeId` of the
 *   slug its `document` names and its own id.
 * - **An ask** is the node id of the block it was indexed from, where `askIds` names one for the
 *   ask's id, and its own id otherwise.
 * - **Anything else** - a comment, a message, a child issue, whose id is its key, a node of the
 *   issue's own tree - keeps the read's id.
 */
export function treeIdOfNode(
  node: TreeAnswerNode,
  key: string,
  askIds: ReadonlyMap<string, string> = NO_ASK_IDS
): string {
  if (isForeign(node, key)) return node.id;
  // The issue the read is of is the outline's root, the parent of every top-level node.
  if (node.supertag === "issue" && (node.id === key || node.parent === null)) return key;
  if (node.document !== undefined) return blockNodeId(node.document.slug, node.id);
  return node.supertag === "ask" ? (askIds.get(node.id) ?? node.id) : node.id;
}

/**
 * The id the tree draws each node of `answer` by (`treeIdOfNode`), as a lookup by the read's own
 * id, which answers the first node the walk reaches where two share the id; a read id the answer
 * does not hold is its own tree id.
 */
export function treeIdOf(answer: TreeAnswer, reads: TreeIdReads): (readId: string) => string {
  const nodes = nodesById(answer);
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
 * answer does not hold, and the catalogue's context (`mark: "context"`), which stands in no
 * issue's tree. Every node a step is offered then has its ancestors' drafts made before it.
 */
function walkOf(answer: TreeAnswer, key: string): TreeAnswerNode[] {
  const nodes = nodesById(answer);
  const inTree = new Map<TreeAnswerNode, boolean>();
  const reachesIssue = (node: TreeAnswerNode): boolean => {
    const known = inTree.get(node);
    if (known !== undefined) return known;
    // A chain of parents that runs in a circle reaches no issue.
    inTree.set(node, false);
    const parent = node.parent === null ? undefined : nodes.get(node.parent);
    const reached =
      node.mark !== "context" &&
      (node.parent === null ||
        node.parent === key ||
        (parent !== undefined && reachesIssue(parent)));
    inTree.set(node, reached);
    return reached;
  };
  const rooted = answer.nodes.filter(reachesIssue);
  return [...rooted, ...answer.nodes.filter((node) => !reachesIssue(node))];
}

/** A requirement, with the counts and the gaps the coverage read holds for it, and a statement of
 *  the issue where it is one (`isStatementNode`). */
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
    draft.gaps = (coverage.gaps ?? []).flatMap((line): GapKind[] => {
      const kind = coverageGapKind(line);
      return kind === undefined ? [] : [kind];
    });
  }
  return draft;
}

/**
 * A check or a plan task of this issue: under the parent the read answers, and again under each
 * other statement of this issue its `serves` tuples name, transcluded there as the outline
 * places it (root R4); its `needs` tuples are chips, one per task needed (`needsChips`). It
 * carries the outline's two gaps of the chain: a `serves` naming a node that is no statement - a
 * node of another kind, or a `requirement` block of a document other than the spec, which is a
 * requirement and no statement, so the gap's words say what both are - and a plan task naming
 * nothing it serves and nothing it attempts. A check stands as the server reads the commit each
 * of its results ran at (`checkStanding`), and draws the results read's facts; a check of the
 * Verification document that read does not hold has no result, and a check block of any other
 * document draws no state.
 */
function draftServing(
  node: TreeAnswerNode,
  _answer: TreeAnswer,
  context: TreeDraftContext
): Draft | undefined {
  if (
    (node.supertag !== "check" && node.supertag !== "plan_task") ||
    isForeign(node, context.key)
  ) {
    return undefined;
  }
  const draft = draftOf(node, node.supertag, context);
  const served = context.tuplesFrom(node.id).flatMap((tuple) => {
    const target = tuple.kind === "serves" ? context.nodeOf(tuple.to) : undefined;
    return target === undefined || isForeign(target, context.key) ? [] : [target];
  });
  const requirements = served.flatMap((target) =>
    isStatementNode(target, context) ? [context.treeIdOf(target.id)] : []
  );
  draft.parents = [...new Set([...context.placeOf(node), ...requirements])];
  if (served.some((target) => !isStatementNode(target, context))) {
    draft.gaps.push("serves_not_requirement");
  }
  if (
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
  return draft;
}

/** Whether `node` is a statement: a `requirement` block of the issue's spec, the document the
 *  issue read names (`TreeContext.specSlug`), whose blocks the coverage read's rows are too
 *  (`KeptReads.requirements`). A `requirement` block of any other document is a block and
 *  nothing more: nothing stands under it. */
function isStatementNode(node: TreeAnswerNode, context: TreeDraftContext): boolean {
  return (
    node.supertag === "requirement" &&
    context.specSlug !== "" &&
    context.treeIdOfNode(node) === blockNodeId(context.specSlug, node.id)
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
 * for a decision (`model.Decision`'s members): its state in the page's words (`decisionState`),
 * who kept it, whether it was replaced and whether its agent was unsure, and its `shaped` tuples
 * as chips, each stale where the decision changed after it named that node. Its time is its
 * words' last change, the decisions read's `changed_at` as the outline draws it, and its links
 * are that read's, where it holds a row for the decision; the read answers both on the node by
 * the same names and values where it holds none. A person's Change the read hangs under it says
 * it waits on its agent (`decisionNodeChanges`), and it waits on the reader by the outline's
 * review list rule (`inReviewList`), where a link ties it to a statement the coverage read holds.
 *
 * A decision of today's decisions read is no node of the answer - a section of `decisions.md` -
 * and is drawn as the outline draws it, by `buildIssueTree`, until the page draws decisions from
 * the tree read's nodes.
 */
function draftDecision(
  node: TreeAnswerNode,
  _answer: TreeAnswer,
  context: TreeDraftContext
): Draft | undefined {
  if (node.supertag !== "decision") return undefined;
  const draft = draftOf(node, "decision", context);
  const kept = context.kept.decisions.get(draft.id);
  const changedAt = kept === undefined ? node.changed_at : kept.changed_at;
  if (typeof changedAt === "string") draft.at = changedAt;
  const links = kept?.links ?? node.links ?? [];
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
  draft.state = decisionState(node, changed);
  markKeptAndUnsure(draft, node);
  draft.replaced = node.replaced === true;
  draft.waiting =
    context.viewer !== undefined &&
    inReviewList(
      { links, waiting_review: node.waiting_review === true },
      (reference) => context.kept.requirements.has(context.nodeIdOf(reference) ?? ""),
      changed
    );
  return draft;
}

/**
 * A question asked of the issue: who asked it and when, its `shaped` tuples as chips, each naming
 * a node the question shaped, and from the asks read how it stands, its options, a person's
 * answer, and whether it waits on the reader, whose turn it is (the outline's `addAsks`). An
 * approval request belongs to the document it reviews and is no node of the tree, as in the
 * outline. An ask the asks read has not answered for yet is open, as the outline drew an ask
 * block before that read answered.
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
  if (askId !== undefined) draft.source = textSourceOf("ask", askId);
  if (ask === undefined) {
    draft.state = "open";
    return draft;
  }
  draft.state = askState(ask);
  if (ask.answer !== null) draft.answer = answerOf(ask.answer);
  if ((ask.options ?? []).length > 0) {
    draft.options = (ask.options ?? []).map((option) => ({
      ...(option.description === undefined || option.description === ""
        ? {}
        : { description: option.description }),
      label: option.label,
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
 *  its words are, which its body's links resolve against. */
function draftTurn(
  node: TreeAnswerNode,
  _answer: TreeAnswer,
  context: TreeDraftContext
): Draft | undefined {
  if (node.supertag !== "comment" && node.supertag !== "message") return undefined;
  const draft = draftOf(node, node.supertag, context);
  madeBy(draft, node);
  draft.source = textSourceOf(node.supertag, node.id);
  return draft;
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

/** A draft of `node` as every kind starts one: its tree id, its words and their reading, its
 *  address (`referenceOf`), and, where the read sent a reading, the document a typed block is
 *  written in, which the links of the body drawn from that reading resolve against. The order
 *  and the read id are `treeDraftsFrom`'s. */
function draftOf(node: TreeAnswerNode, type: NodeType, context: TreeDraftContext): Draft {
  const draft = blank(context.treeIdOfNode(node), type, 0);
  draft.text = node.text;
  const reference = referenceOf(node, context);
  if (reference !== undefined) draft.reference = reference;
  if (node.text_tree !== undefined) {
    draft.tree = node.text_tree;
    if (node.document !== undefined) {
      draft.source = textSourceOf("artifact", node.document.artifact_id);
    }
  }
  return draft;
}

/** When the node was made and who made it, as the read answers them: what the outline draws for
 *  an ask, a comment, a message and a result, and for no document block, decision or child
 *  issue. The read lists every author of the node's first version, whoever made it first ahead
 *  of the rest, and a row draws one name: the first, and none where the list is empty. */
function madeBy(draft: Draft, node: TreeAnswerNode): void {
  draft.at = node.made_at;
  const author = node.author[0];
  if (author !== undefined) draft.author = author;
}

/**
 * The node's own `dispatch://` address, which the kept reads are matched by and a chip naming it
 * links: the address the coverage, results or decisions read gives a requirement, a check or a
 * decision where it holds one, as the outline's draft carried it; an ask's, a comment's and a
 * message's own; and a typed block's in the document its `document` names. A node of another
 * issue is addressed in that issue. Anything else has none.
 */
function referenceOf(node: TreeAnswerNode, context: TreeDraftContext): string | undefined {
  const kept = keptReference(node, context);
  if (kept !== undefined) return kept;
  const key = node.issue ?? context.key;
  if (node.supertag === "comment" || node.supertag === "message") {
    return buildDispatchReference({ id: node.id, key, kind: node.supertag });
  }
  const ask = node.supertag === "ask" ? askIdOf(node, context) : undefined;
  if (ask !== undefined) return buildDispatchReference({ id: ask, key, kind: "ask" });
  return node.document === undefined
    ? undefined
    : buildDispatchReference({
        block: { id: node.id },
        key,
        kind: "artifact",
        slug: node.document.slug,
      });
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
    const chip = tuple.kind === kind ? chipTo(tuple.to, context) : undefined;
    if (chip !== undefined && !chips.has(chip.ref)) chips.set(chip.ref, chip);
  }
  return [...chips.values()];
}

/**
 * The chips of what a check or a plan task `needs`, one per task needed, in the order its tuples
 * first name each, labelled as the outline labels them. The server accepts one task needed by a
 * pinned and an unpinned pointer at once (`#b-a1@v1 #b-a1`), and both need the one task, so the
 * chips are told apart by what they name with any version pin dropped (`pinDropped`). A task
 * named both ways is drawn by its unpinned chip, which names the task's current words; one only
 * pinned ties name, by the first one's.
 */
function needsChips(node: TreeAnswerNode, context: TreeDraftContext): NodeChip[] {
  const needed = new Map<string, { readonly chip: NodeChip; readonly pinned: boolean }>();
  for (const tuple of context.tuplesFrom(node.id)) {
    const chip = tuple.kind === "needs" ? chipTo(tuple.to, context) : undefined;
    if (chip === undefined) continue;
    const { pinned, target } = pinDropped(chip.ref);
    const held = needed.get(target);
    if (held === undefined || (held.pinned && !pinned)) needed.set(target, { chip, pinned });
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
 * The chip naming the node `readId` names: addressed by that node's address (`referenceOf`, and an
 * issue's own), and named by the node the answer holds, by the outline's rule for the kind of
 * target it is (`chipName`). Where the answer holds no words for it, or does not hold it and the
 * tuple names it by an address, the batched reference read names it (`chipOf`), as it names
 * every chip the outline draws. A target with no address has no chip, as in the outline.
 */
function chipTo(readId: string, context: TreeDraftContext): NodeChip | undefined {
  const target = context.nodeOf(readId);
  if (target === undefined) {
    return parseDispatchReference(readId) === undefined ? undefined : context.chipOf(readId);
  }
  const ref =
    referenceOf(target, context) ??
    (target.supertag === "issue"
      ? buildDispatchReference({ key: target.id, kind: "issue" })
      : undefined);
  if (ref === undefined) return undefined;
  const name = chipName(target, context);
  return name === "" ? context.chipOf(ref) : { answered: true, gone: false, name, ref };
}

/** The name a chip draws for a node the answer holds, by the outline's rule for each kind of
 *  target (`treeContext`), from the node's own words: a question's, a comment's and a message's
 *  stored words read to plain words (`plainWords`), an issue's title as it stands, and a block's
 *  first line from its reading (`chipLine`). `""` where the node holds no such words - a block
 *  the read sent no reading for, one whose reading holds no words - which the batched reference
 *  read then names (`chipTo`), by the page's word for its kind for a block of this issue. */
function chipName(target: TreeAnswerNode, context: TreeDraftContext): string {
  if (target.supertag === "ask" || target.supertag === "comment" || target.supertag === "message") {
    return plainWords(target.text);
  }
  if (target.supertag === "issue") return target.text;
  return target.text_tree === undefined ? "" : context.chipLine(target.text_tree);
}

/** Whether `node` names anything by `kind`: a tuple of that kind, or its attribute of that name,
 *  which names a node outside the answer too - a requirement of the issue above, which the read
 *  answers no tuple to. */
function namesAny(node: TreeAnswerNode, kind: string, context: TreeDraftContext): boolean {
  if (context.tuplesFrom(node.id).some((tuple) => tuple.kind === kind)) return true;
  const value = node.attributes[kind];
  return typeof value === "string" ? value.trim() !== "" : Array.isArray(value) && value.length > 0;
}

/** Whether the read marks `node` as another issue's. */
function isForeign(node: TreeAnswerNode, key: string): boolean {
  return node.issue !== undefined && node.issue !== key;
}

/** Each node of the answer by its read id, the first the answer holds where two share one. */
function nodesById(answer: TreeAnswer): Map<string, TreeAnswerNode> {
  const nodes = new Map<string, TreeAnswerNode>();
  for (const node of answer.nodes) {
    if (!nodes.has(node.id)) nodes.set(node.id, node);
  }
  return nodes;
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
  const decisions = reads.decisions?.decisions ?? [];
  const nodes = decisionNodes(decisions, answer);
  const verification = roleDocument(reads.issue, "verification")?.id;
  const checks = indexBy(rows, (check) => {
    const slug = context.slugOf(check.artifact_id);
    return slug === undefined ? undefined : blockNodeId(slug, check.block_id);
  });
  const verificationSlug = verification === undefined ? undefined : context.slugOf(verification);
  if (answer.converted && verificationSlug !== undefined) {
    for (const node of answer.nodes) {
      if (node.supertag !== "check" || node.document !== undefined) continue;
      if (isForeign(node, context.key)) continue;
      const row = checks.get(blockNodeId(verificationSlug, node.id));
      if (row !== undefined) checks.set(idOf(node), row);
    }
  }
  return {
    asks: indexBy([...asksOf(reads).values()], (ask) => askNodeId(ask, context.slugOf)),
    checks,
    children: indexBy(reads.issue.children ?? [], (child) => child.key),
    decisions: indexBy(decisions, (decision) => {
      const node = nodes.get(decision.ref);
      return node === undefined ? undefined : idOf(node);
    }),
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
 *  both hold one, since it is the newer read of the two (the outline's `addAsks`). */
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
 * an ask node of the answer by its tree id. It is how a decision of today's decisions read names
 * the question it answers (`Decision.asks`, `tree-drafts.ts`'s `addDecisions`).
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
  for (const node of answer.nodes) {
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
  return named?.id ?? (answer.converted ? undefined : answer.documents?.[role]);
}

/** An attribute's value as the blocks read gives it, the text the block's opening line writes:
 *  a pointer list as its references joined by one space (`parseBlockPointerValue` reads it
 *  back), a number or a flag as its text. A value with no one line of text - none, or a record
 *  such as an actor - is left out, as the opening line leaves it out. */
function attributeText(value: unknown): string | undefined {
  if (typeof value === "string") return value;
  if (typeof value === "number" || typeof value === "boolean") return String(value);
  if (Array.isArray(value)) return value.map(String).join(" ");
  return undefined;
}

/** A node's attributes as the blocks read lists a block's, in the order the node gives them,
 *  each with its kind read from its value: a list of pointers is `block[]`, which is what a
 *  reader follows as pointers, and anything else reads as text. This module reads no block
 *  schema - the e2e helpers reach it through `issue-tree.ts` and cannot load the schema's JSON -
 *  and no reader of these blocks on the issue page reads any other kind: a drawing that needs
 *  the schema's own kind reads the schema itself (`typed-block-drawing.ts`,
 *  `ReferencedBy.tsx`). */
function attributesOf(node: TreeAnswerNode): BlockAttributeValue[] {
  return Object.entries(node.attributes).flatMap(([name, raw]) => {
    const value = attributeText(raw);
    return value === undefined
      ? []
      : [{ kind: Array.isArray(raw) ? "block[]" : "string", name, value }];
  });
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
    id: node.id,
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
  for (const [node, parent] of parentsIn(answer.nodes, nodesById(answer))) {
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
  const members = answer.nodes.flatMap((node, place) => {
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
 * came, so the walk's order holds across them, and what describes the issue rather than a page
 * (`converted`, `tree`, `view_of`, `documents`) from the first. The answer has no next page.
 * One page is its own answer, which is every issue the page size was set above. Here rather than
 * beside the read (`tree-read.ts`' `readTree`), so the e2e helpers, which load this module and
 * not that one, merge the pages the page received by the same rule.
 */
export function mergeTreePages(pages: readonly [TreeAnswer, ...TreeAnswer[]]): TreeAnswer {
  const [first] = pages;
  if (pages.length === 1) return first;
  return {
    ...first,
    cursor: null,
    nodes: pages.flatMap((page) => page.nodes),
    tuples: pages.flatMap((page) => page.tuples),
  };
}
