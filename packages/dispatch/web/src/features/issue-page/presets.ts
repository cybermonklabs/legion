import { TREE_FILTERS, type TreeFilter } from "../../api/queries";
import type { DisplayTree } from "../../api/types";
import type { ReferenceNameReading } from "../refs/reference-names";
import { parseNodeReference } from "../refs/routes";
import { firstWordedBlock, wordsOf } from "../refs/tree-fragment";
import { blockIdOfNode } from "./decision-review";
import {
  heldBy,
  isCheck,
  isStatement,
  type NodeChip,
  nodeWords,
  type ParentLine,
  parentLineOf,
  type TreeNode,
  type TreeViews,
} from "./issue-tree";
import {
  type FILTER_SIGNAL_WORDS,
  NODE_TYPE_NAMES,
  NODE_TYPES,
  type NodeType,
  type Preset,
} from "./page-words";
import { storedReading } from "./stored-reading";
import { nextTreeCursor, readingOf, treeIdOfNode } from "./tree-read/tree-nodes";
import type { TreeAnswer, TreeAnswerNode } from "./tree-read/tree-read";

/**
 * The page with no filter on, and the four filters, as views of the one tree.
 *
 * With no filter on - Conversation - the view is the whole tree, every node of it unfolded by the
 * outline (`tree-fold.ts`). The words filter (`?q=`) narrows it to the nodes whose words hold
 * what is typed together with the chain above each, which is drawn faint and marked as context
 * (`data-node-context`), so a match is read where it stands in the conversation.
 *
 * A filter draws as rows its own kinds and no other (R2): Spec the requirements; Verification
 * each requirement as context with the checks under it and their results; Plan the plan tasks;
 * Decisions the asks and the decisions. The issue itself is no row of a filter. A requirement
 * is one only where it is a statement of the spec (`isStatement`), and a check only where it is
 * a check of the Verification document (`isCheck`): a `requirement` or `check` block of any
 * other document is a block and nothing more, which no filter draws or counts as its kind
 * (`kindOf`). Every other parent of a drawn node - an ask,
 * a comment, a message, a requirement under Plan - is one line on its child's row
 * (`TreeNode.parentLine`), never a row: a drawn node whose parent is no row is drawn where that
 * parent stood, under the nearest drawn node above it, or at the top. A check serves several
 * requirements and Verification draws it under each, as the outline does; every other filter
 * draws a node once, at its own place, since a plan task drawn under each requirement it serves
 * would be one step drawn several times (R26).
 *
 * Spec, Verification and Plan are each drawn from the issue's view read of that filter once the
 * page holds it (`TreeNode.views` on the issue's node, `heldFilterView`): the view's query names
 * the nodes, and each is drawn as the tree's node of that id, so a row reads as it does in the
 * outline. Until the view answers, and for Decisions until its view read stands in the decisions
 * read's place, a filter is read off the tree `issue-tree.ts` builds: the kinds are R2's, and the
 * context and the parent's line are this module's.
 *
 * A node the address names outranks the filter: an address that names one node and then draws a
 * page without it would not have resolved at all (R20), so `named` is kept whatever the filter
 * draws, and counted as its kind is.
 *
 * This module is pure. It holds no React and makes no request: the rows (`IssueTree.tsx`), the
 * signals the filter row draws (`IssuePresets.tsx`) and the unit tests all read the same model.
 */

/** The four filters, each a control of the row that draws the tree's nodes of its own kinds (R2):
 *  every control but Conversation, which turns them off, and the views that are no filter -
 *  Graph (R10), and Dashboard, Threads and Documents once their parts add them. */
const FILTERS = ["spec", "verification", "plan", "decisions"] as const satisfies readonly Preset[];

/** One of the four filters (`FILTERS`). */
export type Filter = (typeof FILTERS)[number];

/** Whether `preset` is a filter rather than the page with no filter on or a view that is no
 *  filter: Graph draws the whole tree read, and no version control (R10). */
export function isFilter(preset: Preset): preset is Filter {
  return (FILTERS as readonly Preset[]).includes(preset);
}

/** The kinds each filter draws as rows of its own (R2). */
const ROW_KINDS: Readonly<Record<Filter, readonly NodeType[]>> = {
  decisions: ["ask", "decision"],
  plan: ["plan_task"],
  spec: ["requirement"],
  verification: ["check", "result"],
};

/** The kinds a filter draws as context: rows that place its own, faint and marked, and drawn
 *  whether or not anything of the filter's sits under them (R2's "each requirement as
 *  context"). */
const CONTEXT_KINDS: Readonly<Record<Filter, readonly NodeType[]>> = {
  decisions: [],
  plan: [],
  spec: [],
  verification: ["requirement"],
};

/** The main kind each filter counts (R2): the criteria and requirements for Spec, the checks
 *  for Verification, the plan tasks for Plan, the asks and decisions for Decisions. */
const COUNTED_KINDS: Readonly<Record<Filter, readonly NodeType[]>> = {
  decisions: ["ask", "decision"],
  plan: ["plan_task"],
  spec: ["requirement"],
  verification: ["check"],
};

/** Whether `filter` draws a node at every placement the tree has it: Verification draws a check
 *  under every requirement it serves, as the outline does; every other filter draws a node at
 *  its own place alone. */
function drawsEveryPlacement(filter: Filter): boolean {
  return filter === "verification";
}

/** No node named by the address, which is every address but a deep link's. */
const EMPTY: ReadonlySet<string> = new Set();

/** What the view draws over the tree it was given. */
export interface PresetView {
  /** The ids of the rows drawn as context: with no filter on, the chain above a match of the
   *  words filter; under Verification, each requirement. */
  readonly context: ReadonlySet<string>;
  /** The decisions and constraints of the view read each row carries as a line of its own, by
   *  the row's id: in the view read of Spec, Verification or Plan, a decision or a constraint is
   *  a line of a row and never a row itself. Empty where the view is read off the tree. */
  readonly lines: ReadonlyMap<string, readonly PresetLine[]>;
  /** How many distinct nodes of the view's main kind it draws: a check drawn under two
   *  requirements is one node, so this is what the control's own count says. */
  readonly matches: number;
  /** The reading the view names each block by that a chip or a parent line of one of its rows
   *  names - a step's need, a statement a row stands under or also serves - by the block's
   *  reference: the reading the row's chip carries, else that of the block's own words, read off
   *  the tree that holds it (`nodeReading`). The view's own read is often the one read that holds
   *  such a block, as it holds the plan's tasks, so the page's names read was never asked for it;
   *  the page publishes these for the names (`slots/view-readings.ts`), so each chip draws as
   *  every other does. A block the page's read names too is named again by the same words.
   *  Empty where the view is read off the outline's tree. */
  readonly readings: ReadonlyMap<string, ReferenceNameReading>;
  /** The tree as the view leaves it. Under a filter its children are the filter's top rows. */
  readonly root: TreeNode;
  /** Whether the issue itself is the outline's first row: with no filter on, and never under a
   *  filter, which draws its own kinds as rows and no other (R2). */
  readonly rootRow: boolean;
}

/** A node of a view read drawn as a line of a row, never as a row itself, which is how the view
 *  read of Spec, Verification or Plan draws a decision and a constraint: a decision whose
 *  `shaped` tuple names the row's node, or a constraint the row's node's `constrained_by` tuple
 *  names. Later parts of the page draw these lines. The Decisions filter and the outline draw a
 *  decision as a row of its own. */
export interface PresetLine {
  /** The node's id: a decision's block id (`block_id`), and the view read's id of any other. */
  readonly id: string;
  /** The node's own words, and the server's reading of them where the read sent one. */
  readonly text: string;
  readonly tree?: DisplayTree;
  readonly type: "constraint" | "decision";
}

/** No line on any row: a view read off the tree. */
const NO_LINES: ReadonlyMap<string, readonly PresetLine[]> = new Map();

/** No block named by the view: a view read off the outline's tree. */
const NO_READINGS: ReadonlyMap<string, ReferenceNameReading> = new Map();

/** The kind a filter reads `node` as: its type, but for a requirement that is no statement
 *  (`isStatement`) and a check that is no check (`isCheck`), which are blocks of a document that
 *  does not play that part and nothing more, so no filter draws or counts either as its kind;
 *  undefined for both. */
function kindOf(node: TreeNode): NodeType | undefined {
  if (node.type === "requirement" && !isStatement(node)) return undefined;
  if (node.type === "check" && !isCheck(node)) return undefined;
  return node.type;
}

/** Whether `node`'s words hold `wanted`, which is already lowercased; `""` holds everywhere.
 *  The words are the ones a reader reads (`nodeWords`, R1, R4's "the nodes whose words hold
 *  them"), never the stored source: `**bold** text` is found by `bold text` and not by `**`. A
 *  post its author deleted holds none: its text is the deleted line as the model writes it
 *  (`tree-drafts.ts`'s `deletedPostLine`), naming the actor by a short id and the time as stored,
 *  and its row draws the page's own deleted line in its place (`node-line.tsx`'s `DeletedLine`),
 *  so no word typed finds it (deleted-post R3). */
function holds(node: TreeNode, wanted: string): boolean {
  return (
    wanted === "" ||
    (node.deletedAt === undefined && nodeWords(node).toLowerCase().includes(wanted))
  );
}

/**
 * The tree as `preset`, `query` and the address's own node leave it. With no filter on the root
 * is always drawn, because an issue whose words filter matches nothing still has to say so
 * somewhere, and it is context whenever nothing matched it. Graph, a view and no filter (R10), is
 * drawn so too, from Graph's read: every node the page draws as a row, each check and plan task
 * under each requirement it serves as well.
 *
 * `memo` is the spec's summary callout, which the head draws as the issue's three sentences
 * (R8) and which is no row of the conversation (c-tree: "The memo callout is not a row of the
 * outline"), nor of Graph, unless an address names it.
 */
export function presetView(
  root: TreeNode,
  preset: Preset,
  query: string,
  named: ReadonlySet<string> = EMPTY,
  memo?: string
): PresetView {
  const wanted = query.trim().toLowerCase();
  if (!isFilter(preset)) {
    return conversationView(root, wanted, named, memo, preset === "conversation");
  }
  // Decisions filters the whole tree: `decisions-entries.ts` reads each decision's members.
  if (preset === "decisions") return filterView(root, preset, wanted, named);
  const held = root.views?.[preset];
  return held === undefined
    ? filterView(root, preset, wanted, named)
    : heldFilterView(root, preset, held, wanted, named);
}

/** The kinds Conversation draws no row of (R9), each with the filter whose rows they are, which
 *  an address of one opens in Conversation's place (`filterOfUndrawn`): a plan task's Plan and a
 *  result's Verification. The answer may hold one as an ancestor of a row, which is drawn whole
 *  with its kind. A constraint is a line on each requirement it constrains, and no step drafts
 *  one (`tree-read/tree-nodes.ts`). */
const NO_CONVERSATION_ROW: Readonly<Partial<Record<NodeType, Filter>>> = {
  plan_task: "plan",
  result: "verification",
};

/** The filter an address of `node` opens in Conversation's place, where Conversation draws no row
 *  of it (R9): a plan task's Plan and a result's Verification, where it holds no node a row
 *  stands for. Undefined for every node Conversation draws as a row. */
export function filterOfUndrawn(node: TreeNode): Filter | undefined {
  return node.children.length === 0 ? NO_CONVERSATION_ROW[node.type] : undefined;
}

/**
 * The filter whose view answer the page holds (`TreeNode.views`) answers the block `target` names
 * as a node of that filter's own kinds (`ROW_KINDS`): the filter an address of a node
 * Conversation's read does not hold opens at that node (R9), a plan task's Plan, a result's
 * Verification. Undefined where no held view answers it.
 */
export function filterHolding(
  views: TreeViews | undefined,
  target: BlockTarget
): Filter | undefined {
  return TREE_FILTERS.find((filter) => answerHolds(views?.[filter], target, ROW_KINDS[filter]));
}

/** The block an address names: its id, and the document that holds it where the address names
 *  one (`issue-address.ts`' block `AddressTarget`). */
interface BlockTarget {
  readonly document?: string;
  readonly id: string;
}

/** Whether `answer` holds the block `target` names as a node of one of `kinds`, or of any kind
 *  where none is given: the node whose block id is `target.id`, in the document `target.document`
 *  names where it names one. Graph draws every node of its read (R10), so an address of a block
 *  Conversation's read holds none of opens Graph where Graph's read, as the page holds it,
 *  answers the block. */
export function answerHolds(
  answer: TreeAnswer | undefined,
  target: BlockTarget,
  kinds?: readonly NodeType[]
): boolean {
  return (answer?.nodes ?? []).some(
    (node) =>
      (kinds === undefined || (kinds as readonly string[]).includes(node.supertag)) &&
      blockIdOfNode(node) === target.id &&
      (target.document === undefined || node.document?.slug === target.document)
  );
}

/** The filter a row's kind tag opens its node in (R9): a criterion or a statement in Spec, a
 *  check in Verification, a decision in Decisions. Undefined for every other node, whose tag
 *  opens nothing: a requirement that is no statement and a check that is no check of the
 *  Verification document (`kindOf`) among them. */
export function tagFilter(node: TreeNode): Filter | undefined {
  if (node.type === "criterion") return "spec";
  switch (kindOf(node)) {
    case "requirement":
      return "spec";
    case "check":
      return "verification";
    case "decision":
      return "decisions";
    default:
      return undefined;
  }
}

/** The kinds of a turn of the conversation, which a message's thread holds under it. */
const TURN_KINDS: readonly NodeType[] = ["comment", "message"];

/** Whether `node` is a message under which a node it produced stands - any node but a turn of
 *  its thread - which Conversation draws as one context row with its kind (R9): the thread's
 *  reply is written in Threads, so its row takes none. */
export function isThreadContext(node: Pick<TreeNode, "children" | "type">): boolean {
  return node.type === "message" && node.children.some((child) => !TURN_KINDS.includes(child.type));
}

/**
 * The page with no filter on, or Graph: the whole tree but the memo, or under a words filter each
 * match with the chain above it as context. In Conversation (`conversation`), which draws
 * Conversation's read and nothing else (R9), a result and a plan task are no row
 * (`NO_CONVERSATION_ROW`), and neither is a block of a converted document drawn as prose (root
 * R1: the conversation's own blocks are the ones written in it), but where a row stands under one
 * or the address names it; and a message under which a node it produced stands - any node but a
 * turn of its thread - is one context row with its kind. Graph draws every node of the tree as a
 * row, a block of a converted document among them.
 */
function conversationView(
  root: TreeNode,
  wanted: string,
  named: ReadonlySet<string>,
  memo: string | undefined,
  conversation: boolean
): PresetView {
  const context = new Set<string>();
  const matched = new Set<string>();
  const prune = (node: TreeNode): TreeNode | undefined => {
    if (node.id === memo && !named.has(node.id)) return undefined;
    const children = node.children
      .map(prune)
      .filter((child): child is TreeNode => child !== undefined);
    // A prose node is a block of a converted document where its draft is addressed in that
    // document (`tree-read/tree-nodes.ts`' `referenceOf`, from the node's `document`); a block
    // written in the conversation, the room's own fragment, has no address of its own.
    const documentProse = node.type === "prose" && node.reference !== undefined;
    if (
      conversation &&
      (NO_CONVERSATION_ROW[node.type] !== undefined || documentProse) &&
      children.length === 0 &&
      !named.has(node.id)
    ) {
      return undefined;
    }
    const own = named.has(node.id) || holds(node, wanted);
    if (!own && children.length === 0) return undefined;
    if (own) matched.add(node.id);
    else context.add(node.id);
    if (conversation && isThreadContext({ children, type: node.type })) context.add(node.id);
    return { ...node, children, held: heldBy(children) };
  };
  const pruned = prune(root);
  if (pruned !== undefined) {
    return {
      context,
      lines: NO_LINES,
      matches: matched.size,
      readings: NO_READINGS,
      root: pruned,
      rootRow: true,
    };
  }
  // Nothing under the issue matched, so the view is the issue alone, drawn as context.
  context.add(root.id);
  return {
    context,
    lines: NO_LINES,
    matches: 0,
    readings: NO_READINGS,
    root: { ...root, children: [], held: 0 },
    rootRow: true,
  };
}

/** What one node of the tree gives a filter at its place: the rows its branch draws there, and
 *  whether the node is itself one of them. */
interface Lifted {
  readonly drawn: boolean;
  readonly rows: TreeNode[];
}

/**
 * The rows `node`'s branch gives a filter at the place `node` stands in the tree, in the tree's
 * order: the row `rowOf` draws `node` as, given what each of its children gave, or, where it
 * draws none, the rows of the branch at `node`'s place (`liftPast`). A transclusion gives a
 * filter nothing unless the filter draws every placement.
 */
function lift(
  node: TreeNode,
  everyPlacement: boolean,
  rowOf: (node: TreeNode, parts: readonly Lifted[]) => TreeNode | undefined
): Lifted {
  if (!everyPlacement && node.transcluded) return { drawn: false, rows: [] };
  const parts = node.children.map((child) => lift(child, everyPlacement, rowOf));
  const row = rowOf(node, parts);
  return row === undefined
    ? { drawn: false, rows: liftPast(node, parts) }
    : { drawn: true, rows: [row] };
}

/** The rows `parts` give the place of `node`, which the filter draws as no row: each row of its
 *  own children carrying it as its parent's line (`parentLineOf`), so a drawn node whose parent
 *  is no row is drawn where that parent stood, and each row from deeper as it came. */
function liftPast(node: TreeNode, parts: readonly Lifted[]): TreeNode[] {
  const line = parts.some((part) => part.drawn) ? parentLineOf(node) : undefined;
  return parts.flatMap((part) =>
    part.drawn && line !== undefined
      ? part.rows.map((row) => ({ ...row, parentLine: line }))
      : part.rows
  );
}

/** One filter over the tree: its kinds as rows, each other parent a line on its child's row. */
function filterView(
  root: TreeNode,
  filter: Filter,
  wanted: string,
  named: ReadonlySet<string>
): PresetView {
  const rows = ROW_KINDS[filter];
  const contextKinds = CONTEXT_KINDS[filter];
  const counted = COUNTED_KINDS[filter];
  const context = new Set<string>();
  const matched = new Set<string>();
  // The row `node` is, with the rows of its branch under it, where the filter draws it.
  const rowOf = (node: TreeNode, parts: readonly Lifted[]): TreeNode | undefined => {
    // A `requirement` or `check` block of a document that does not play that part is of no
    // kind a filter draws (`kindOf`): it is a row only where the address names it, and counted
    // nowhere.
    const kind = kindOf(node);
    const own =
      named.has(node.id) || (kind !== undefined && rows.includes(kind) && holds(node, wanted));
    const placing =
      !own &&
      kind !== undefined &&
      contextKinds.includes(kind) &&
      (wanted === "" || holds(node, wanted) || parts.some((part) => part.rows.length > 0));
    if (!own && !placing) return undefined;
    if (own && kind !== undefined && counted.includes(kind)) matched.add(node.id);
    if (placing) context.add(node.id);
    const below = parts.flatMap((part) => part.rows);
    return { ...node, children: below, held: heldBy(below) };
  };
  const everyPlacement = drawsEveryPlacement(filter);
  // The issue is no row of a filter, and no row's parent line: the page is the issue.
  const top = root.children.flatMap((child) => lift(child, everyPlacement, rowOf).rows);
  return {
    context,
    lines: NO_LINES,
    matches: matched.size,
    readings: NO_READINGS,
    root: { ...root, children: top, held: heldBy(top) },
    rootRow: false,
  };
}

/**
 * One filter drawn from its view read (`TreeNode.views`): the nodes the view's query took of
 * the filter's row kinds, each drawn as the tree's node of the same identity (`treeIdOfNode`),
 * so a row reads as it does in the outline, under the row the read places it under, else at the
 * top; two nodes of one read id, blocks of two documents, are two rows of two nodes. A node the
 * outline's tree holds no node of is drawn as the node of the tree the view's own answer builds
 * (`TreeViews.trees`), so the view draws its own read's rows whatever the outline read holds or
 * whether it was sent at all (R4, R5). Verification draws each statement the view holds as
 * context, with each check under each statement it serves and each result under its check. A
 * node neither tree drafts - a criterion - is no row, and neither is the memo, a callout. A
 * decision and a constraint are never rows: each is a line of the row its tie names
 * (`PresetView.lines`). The words filter keeps the rows whose words hold it, as `filterView`
 * keeps them, and a node the address names is drawn whatever the filter: from the view where the
 * view holds it as a row, and else where `filterView` draws it, at each place the tree has it,
 * under the nearest row of the view above it there or at the top, in the tree's order among the
 * view's rows there.
 *
 * Plan draws as rows, beside the issue's own tasks, the plan tasks of every issue under it,
 * which its read answers as context, each marked with its issue (`viewRow`): after the issue's
 * own, each issue under it in the tree's order, the order the read first answers a node of it,
 * and each issue's tasks in the order its document writes them (`document.position`), which is
 * the written order the plan's walk starts from (`plan-filter/plan-order.ts`). Each of its rows
 * carries what the read answers of the task beside the node: its size and the issue it is
 * another issue's task of (`planFactsOf`). No other view takes a node of another issue as a
 * row.
 */
function heldFilterView(
  root: TreeNode,
  filter: TreeFilter,
  answer: TreeAnswer,
  wanted: string,
  named: ReadonlySet<string>
): PresetView {
  // Each node of the tree the read drew, by its id and by the read's id, at its first placement,
  // and each placement by its id and the node it stands under. A view's node is matched by its
  // id; a node the view does not hold, by the read's. The outline's tree is read first, and the
  // view's own tree after it fills in only what the outline's holds no node of.
  const fromRead = new Map<string, TreeNode>();
  const byRead = new Map<string, TreeNode>();
  const ofRead = new Map<string, TreeNode[]>();
  const placements = new Map<string, TreeNode>();
  // Each node of the two trees by its own address, the outline's first: a row drawn from the
  // view's own tree names each block a chip of it names - a step it needs, a statement of the
  // outline read the view's tree holds as context - by that block's line (`chipsNamed`).
  const byReference = new Map<string, TreeNode>();
  const index = (parent: TreeNode, outline: boolean): void => {
    for (const child of parent.children) {
      if (child.reference !== undefined && !byReference.has(child.reference)) {
        byReference.set(child.reference, child);
      }
      if (child.readId !== undefined) {
        if (!fromRead.has(child.id)) {
          fromRead.set(child.id, child);
          const same = ofRead.get(child.readId);
          if (same === undefined) ofRead.set(child.readId, [child]);
          else same.push(child);
        }
        if (!byRead.has(child.readId)) byRead.set(child.readId, child);
        const placement = `${child.id} ${parent.id}`;
        if (outline || !placements.has(placement)) placements.set(placement, child);
      }
      index(child, outline);
    }
  };
  index(root, true);
  const own = root.views?.trees?.[filter];
  if (own !== undefined) index(own, false);
  // Each chip of a row that names a block of a document - a need (`TreeNode.chips`), another
  // statement the row serves (`TreeNode.alsoUnder`) - with the reading it is named by: its own,
  // else, for a chip with no words, the block's own words (`nodeReading`) where a tree holds the
  // block. The page publishes each such reading for the names (`PresetView.readings`), as it does
  // the block a row's parent line names (`drawTop`), so the chip is drawn as any other is.
  const readings = new Map<string, ReferenceNameReading>();
  const keep = (reference: string, reading: ReferenceNameReading): void => {
    const route = parseNodeReference(reference);
    if (
      route !== undefined &&
      (route.kind === "spec" || route.kind === "artifact") &&
      route.block !== undefined
    ) {
      readings.set(reference, reading);
    }
  };
  const chipsNamed = (chips: readonly NodeChip[]): readonly NodeChip[] =>
    chips.map((chip) => {
      if (chip.gone) return chip;
      const target = chip.reading === undefined ? byReference.get(chip.ref) : undefined;
      const reading = chip.reading ?? (target === undefined ? undefined : nodeReading(target));
      if (reading === undefined) return chip;
      keep(chip.ref, reading);
      return chip.reading === undefined ? { ...chip, reading } : chip;
    });
  // The tree's node each node of the view stands for: the one of its own id (`treeIdOfNode`), so
  // two nodes of one block id, blocks of two documents, are two; else, for a view node that names
  // no document, the one of its read id. And each one's parent: the nearest node before it that
  // its `parent` names, since a parent comes before its children, else the tree's node of that
  // read id. The view names the parent's kind beside its id (`parent.supertag`), so the parent is
  // a node of that kind where the tree holds one of that read id.
  const treeOf = new Map<TreeAnswerNode, TreeNode>();
  const firstOf = new Map<string, TreeNode>();
  const nodes = answer.nodes ?? [];
  for (const node of nodes) {
    const tree = fromRead.get(treeIdOfNode(node, root.id)) ?? byRead.get(node.id);
    if (tree === undefined) continue;
    treeOf.set(node, tree);
    if (!firstOf.has(node.id)) firstOf.set(node.id, tree);
  }
  const treeNodeOf = (readId: string): TreeNode | undefined =>
    firstOf.get(readId) ?? byRead.get(readId);
  const parentOf = new Map<TreeAnswerNode, TreeNode>();
  const latest = new Map<string, TreeNode>();
  for (const node of nodes) {
    const above = node.parent;
    const nearest = above === null ? undefined : (latest.get(above.id) ?? treeNodeOf(above.id));
    const parent =
      above === null || nearest?.type === above.supertag
        ? nearest
        : (ofRead.get(above.id)?.find((one) => one.type === above.supertag) ?? nearest);
    if (parent !== undefined) parentOf.set(node, parent);
    const tree = treeOf.get(node);
    if (tree !== undefined) latest.set(node.id, tree);
  }
  const kinds: readonly string[] = ROW_KINDS[filter];
  const contextKinds: readonly string[] = CONTEXT_KINDS[filter];
  const rows = nodes.filter(
    (node) =>
      treeOf.has(node) &&
      (viewRow(node, filter, root.id) ? kinds : contextKinds).includes(node.supertag)
  );
  if (filter === "plan") {
    const issues = new Map<string, number>();
    for (const node of nodes) {
      if (node.issue !== undefined && !issues.has(node.issue)) issues.set(node.issue, issues.size);
    }
    const place = (node: TreeAnswerNode): readonly [number, number] =>
      node.issue === undefined || node.issue === root.id
        ? [-1, 0]
        : [issues.get(node.issue) ?? 0, node.document?.position ?? Number.POSITIVE_INFINITY];
    rows.sort((one, other) => {
      const [issue, at] = place(one);
      const [otherIssue, otherAt] = place(other);
      return issue - otherIssue || at - otherAt;
    });
  }
  const rowIds = new Set(rows.map((node) => treeOf.get(node)?.id));
  // Where each row stands: under its parent where that is a row, and under Verification a check
  // under each statement its `serves` tuples name too.
  const homes = new Map<string, string[]>();
  for (const node of rows) {
    const parent = parentOf.get(node)?.id;
    const id = treeOf.get(node)?.id;
    if (id !== undefined && parent !== undefined && rowIds.has(parent)) homes.set(id, [parent]);
  }
  for (const tuple of drawsEveryPlacement(filter) ? (answer.tuples ?? []) : []) {
    const to = treeNodeOf(tuple.to.id)?.id;
    const from = treeNodeOf(tuple.from.id)?.id;
    if (tuple.kind !== "serves" || to === undefined || from === undefined || !rowIds.has(to)) {
      continue;
    }
    const held = homes.get(from);
    if (held === undefined) homes.set(from, [to]);
    else if (!held.includes(to)) held.push(to);
  }
  const below = new Map<string, TreeAnswerNode[]>();
  const top: TreeAnswerNode[] = [];
  for (const node of rows) {
    const at = homes.get(treeOf.get(node)?.id ?? "") ?? [];
    if (at.length === 0) top.push(node);
    for (const home of at) {
      const under = below.get(home);
      if (under === undefined) below.set(home, [node]);
      else under.push(node);
    }
  }
  const counted: readonly NodeType[] = COUNTED_KINDS[filter];
  const context = new Set<string>();
  const matched = new Set<string>();
  const drawnIds = new Set<string>();
  // The tree as `filterView` lifts it (`lift`), with the view's rows and the nodes the address
  // names as its rows: so a named node that is no row of the view stands where `filterView`
  // draws it, at each place the tree has it (every placement under Verification, as there),
  // under the nearest row of the view above it or at the top, carrying its parent's line where
  // that parent is no row. What the tree holds under each placement of a row of the view is kept
  // by that placement (`liftedAt`), and by the row's id at its first placement the lift reaches
  // (`liftedOf`), for a placement it stops at: under Spec and Plan it stops at a transclusion,
  // which can come first in the tree's order. An address that names no node, or only rows of the
  // view, lifts nothing: the view read places every row.
  const liftedAt = new Map<TreeNode, readonly Lifted[]>();
  const liftedOf = new Map<string, readonly Lifted[]>();
  const treeRowOf = (node: TreeNode, parts: readonly Lifted[]): TreeNode | undefined => {
    if (rowIds.has(node.id)) {
      liftedAt.set(node, parts);
      if (!liftedOf.has(node.id)) liftedOf.set(node.id, parts);
      return node;
    }
    // A named node with what its branch gave, which `amongView` draws (`namedRow`).
    return named.has(node.id)
      ? { ...node, children: parts.flatMap((part) => part.rows) }
      : undefined;
  };
  const liftedTop = [...named].every((id) => rowIds.has(id))
    ? undefined
    : root.children.map((child) => lift(child, drawsEveryPlacement(filter), treeRowOf));
  // The rows the view draws at one place, each of `nodes` drawn by `drawNode`, with each node the
  // address names that the tree holds at that place (`lifted`), in the tree's order: before the
  // first of the view's rows here that comes after it in the tree, else after them all. A row of
  // the view among what the tree holds there only marks that order: the view read places it.
  const amongView = (
    nodes: readonly TreeAnswerNode[],
    lifted: readonly Lifted[] | undefined,
    drawNode: (node: TreeAnswerNode) => Lifted
  ): Lifted[] => {
    if (lifted === undefined) return nodes.map(drawNode);
    const here = new Set(nodes.map((node) => treeOf.get(node)?.id));
    const before = new Map<string, Lifted[]>();
    let waiting: Lifted[] = [];
    const reach = (id: string): void => {
      if (waiting.length === 0 || !here.has(id) || before.has(id)) return;
      before.set(id, waiting);
      waiting = [];
    };
    // A named node's row as drawn, counted where its kind is counted: each row of the view the
    // tree holds in its branch is none of its rows, and is reached in turn after it (`inside`).
    const namedRow = (row: TreeNode, inside: string[]): TreeNode => {
      if (counted.includes(row.type)) matched.add(row.id);
      const children = row.children.flatMap((child) => {
        if (!rowIds.has(child.id)) return [namedRow(child, inside)];
        inside.push(child.id);
        return [];
      });
      return { ...row, children, held: heldBy(children) };
    };
    for (const part of lifted) {
      for (const row of part.rows) {
        if (rowIds.has(row.id)) {
          reach(row.id);
          continue;
        }
        const inside: string[] = [];
        waiting.push({ drawn: part.drawn, rows: [namedRow(row, inside)] });
        for (const id of inside) reach(id);
      }
    }
    const drawn = nodes.flatMap((node) => [
      ...(before.get(treeOf.get(node)?.id ?? "") ?? []),
      drawNode(node),
    ]);
    return [...drawn, ...waiting];
  };
  // As `filterView`'s `lift`: the node with its own rows under it, or, where it is no row, the
  // rows of its branch at its place (`liftPast`).
  const draw = (
    node: TreeAnswerNode,
    home: TreeNode | undefined,
    above: ReadonlySet<string>
  ): Lifted => {
    const held = treeOf.get(node);
    const tree =
      (home === undefined || held === undefined
        ? undefined
        : placements.get(`${held.id} ${home.id}`)) ?? held;
    if (tree === undefined || above.has(tree.id)) return { drawn: false, rows: [] };
    const inner = new Set([...above, tree.id]);
    const lifted = liftedAt.get(tree) ?? liftedOf.get(tree.id);
    const parts = amongView(below.get(tree.id) ?? [], lifted, (child) => draw(child, tree, inner));
    const row = viewRow(node, filter, root.id);
    const own = named.has(tree.id) || (row && holds(tree, wanted));
    const placing =
      !own &&
      !row &&
      (wanted === "" || holds(tree, wanted) || parts.some((part) => part.rows.length > 0));
    if (!own && !placing) return { drawn: false, rows: liftPast(tree, parts) };
    if (own && counted.includes(tree.type)) matched.add(tree.id);
    if (placing) context.add(tree.id);
    drawnIds.add(tree.id);
    const kept = parts.flatMap((part) => part.rows);
    return {
      drawn: true,
      rows: [
        {
          ...tree,
          alsoUnder: chipsNamed(tree.alsoUnder),
          chips: chipsNamed(tree.chips),
          children: kept,
          held: heldBy(kept),
          ...(filter === "plan" ? planFactsOf(node, root.id) : {}),
        },
      ],
    };
  };
  const drawTop = (node: TreeAnswerNode): Lifted => {
    const part = draw(node, undefined, new Set());
    const parent = parentOf.get(node);
    const line = part.drawn ? viewParentLine(node, root, parent) : undefined;
    if (line?.reference !== undefined && parent !== undefined) {
      keep(line.reference, nodeReading(parent));
    }
    return line === undefined
      ? part
      : { drawn: true, rows: part.rows.map((row) => ({ ...row, parentLine: line })) };
  };
  const children = amongView(top, liftedTop, drawTop).flatMap((part) => part.rows);
  // The view says by its own read alone whether nodes remain to read (`TreeNode.moreToRead`),
  // never by the outline read's mark on the root.
  const { moreToRead: _outline, ...issueNode } = root;
  return {
    context,
    lines: viewLines(answer, treeNodeOf, drawnIds),
    matches: matched.size,
    readings,
    root: {
      ...issueNode,
      children,
      held: heldBy(children),
      ...(nextTreeCursor(answer) === undefined ? {} : { moreToRead: true }),
    },
    rootRow: false,
  };
}

/** Whether the view read of `filter` answers `node` as one of the view's rows rather than as
 *  context: a node its query took, and under Plan a node of an issue under `key`, the issue the
 *  page is of, which the Plan read answers beside the issue's own as context, each marked with
 *  its issue. The row kinds (`ROW_KINDS`) still choose which of them are rows. */
function viewRow(node: TreeAnswerNode, filter: TreeFilter, key: string): boolean {
  return (
    node.role === "query" || (filter === "plan" && node.issue !== undefined && node.issue !== key)
  );
}

/**
 * What the Plan view's read answers of a task beside its node, which the task's row carries: its
 * size, the whole number of minutes the read answers as `plan.size` from the task's `size`
 * attribute, where it holds one; and the issue it belongs to, where that is an issue under `key`,
 * the issue the page is of. A size that is no whole number of minutes is no size. This is the
 * one place the page reads the read's `plan` of a node.
 */
function planFactsOf(node: TreeAnswerNode, key: string): Pick<TreeNode, "issue" | "size"> {
  const written = node.plan?.size;
  const size = typeof written === "string" && written !== "" ? Number(written) : Number.NaN;
  return {
    ...(node.issue === undefined || node.issue === key ? {} : { issue: node.issue }),
    ...(Number.isInteger(size) && size >= 0 ? { size } : {}),
  };
}

/**
 * The line a row the view draws at the top carries for its parent in the view read, where that
 * parent is no row: the tree's node for it (`parent`), drawn as `parentLineOf` draws it, where the
 * tree holds it, and else the parent's id, kind and first line the view read answers beside the
 * node (`parent`), for a kind the page has a word for. The issue is no row's parent line.
 */
function viewParentLine(
  node: TreeAnswerNode,
  root: TreeNode,
  parent: TreeNode | undefined
): ParentLine | undefined {
  if (node.parent === null || node.parent.id === root.id) return undefined;
  if (parent !== undefined) return parentLineOf(parent);
  const read = node.parent;
  const types: readonly string[] = NODE_TYPES;
  if (read.supertag === "issue" || !types.includes(read.supertag)) return undefined;
  return {
    id: read.id,
    text: read.first_line,
    type: read.supertag as NodeType,
  };
}

/** The decisions and constraints of `answer` as lines of the rows the view draws, by each row's
 *  id: a decision on the row its `shaped` tuple names, a constraint on the row whose
 *  `constrained_by` tuple names it, each once. `rowOf` is the tree's node a read id names. */
function viewLines(
  answer: TreeAnswer,
  rowOf: (readId: string) => TreeNode | undefined,
  drawn: ReadonlySet<string>
): ReadonlyMap<string, readonly PresetLine[]> {
  const nodes = new Map((answer.nodes ?? []).map((node) => [node.id, node]));
  const lines = new Map<string, PresetLine[]>();
  const add = (rowId: string, node: TreeAnswerNode | undefined, type: PresetLine["type"]) => {
    const row = rowOf(rowId)?.id;
    if (node?.supertag !== type || row === undefined || !drawn.has(row)) return;
    const id = blockIdOfNode(node);
    const held = lines.get(row);
    if (held?.some((line) => line.id === id) === true) return;
    const tree = readingOf(node);
    const line: PresetLine = {
      id,
      text: node.text,
      ...(tree === undefined ? {} : { tree }),
      type,
    };
    if (held === undefined) lines.set(row, [line]);
    else held.push(line);
  };
  for (const tuple of answer.tuples ?? []) {
    if (tuple.kind === "shaped") add(tuple.to.id, nodes.get(tuple.from.id), "decision");
    if (tuple.kind === "constrained_by") add(tuple.from.id, nodes.get(tuple.to.id), "constraint");
  }
  return lines;
}

/** How a chip names `node`, a node of a tree whose chip the names read gave no words, or the
 *  parent a row's line names (`ParentLine`, which carries the same words): by the reading of its
 *  words, its read's own else its stored text's (`storedReading`), with the page's word for its
 *  kind where those words project to none, as a chip names a block (`tree-drafts.ts`'s
 *  `treeContext`), and by that word alone where it holds no words (R22). */
export function nodeReading(node: Pick<TreeNode, "text" | "tree" | "type">): ReferenceNameReading {
  const word = NODE_TYPE_NAMES[node.type];
  const tree = node.tree ?? (node.text === "" ? undefined : storedReading(node.text, node));
  return tree === undefined || firstWordedBlock(tree) === undefined
    ? { words: word }
    : { fallback: word, tree };
}

/** What one control of the filter row draws after its name (R2): its count, then, where the
 *  picture draws one, the noun the count is in or what of it waits on the reader. */
export interface PresetSignal {
  /** How many nodes of the view's main kind it draws (R2), or for Conversation how many entries
   *  the Contents list holds, as the prototype's signal counts them. */
  readonly count: number;
  /** The part of the count that asks something of the reader, with the word it is drawn with:
   *  the checks that failed, the decisions waiting for the reader's review. */
  readonly flag?: { readonly count: number; readonly word: keyof typeof FILTER_SIGNAL_WORDS };
  /** Whether the read the count is read from holds nodes no page read yet holds, so the count is
   *  of the nodes loaded and carries "+" (R15, `TreeNode.moreToRead`). */
  readonly more?: true;
  /** The noun the count is drawn in, where the picture draws one ("13 steps"). */
  readonly unit?: keyof typeof FILTER_SIGNAL_WORDS;
}

/** The distinct nodes of `type` the view draws that `counts` holds for, each read as its kind
 *  (`kindOf`), so a block that is no check is no failing check. */
function countOf(root: TreeNode, type: NodeType, counts: (node: TreeNode) => boolean): number {
  const found = new Set<string>();
  const walk = (node: TreeNode): void => {
    if (kindOf(node) === type && counts(node)) found.add(node.id);
    for (const child of node.children) walk(child);
  };
  walk(root);
  return found.size;
}

/** The signal each filter's control draws under `query`, which its count follows. A count read
 *  from a read that names a next page carries "+" (`PresetSignal.more`): the held view's own read
 *  for a filter whose view is held, and the tree's for every other. */
export function filterSignals(
  root: TreeNode,
  query: string
): Readonly<Record<Filter, PresetSignal>> {
  const view = (filter: Filter) => presetView(root, filter, query);
  const more = (answer: TreeAnswer | undefined): Pick<PresetSignal, "more"> =>
    (answer === undefined ? root.moreToRead === true : nextTreeCursor(answer) !== undefined)
      ? { more: true }
      : {};
  // Once a filter's view read is held, its count is the held view's own nodes of its main kind
  // whose words hold the query; until then, what the view read off the tree draws.
  const count = (filter: TreeFilter): number => {
    const held = root.views?.[filter];
    if (held === undefined) return view(filter).matches;
    const kinds: readonly string[] = COUNTED_KINDS[filter];
    const wanted = query.trim().toLowerCase();
    const words = (node: TreeAnswerNode): string =>
      node.text_tree === undefined ? node.text : wordsOf(node.text_tree);
    return new Set(
      (held.nodes ?? []).flatMap((node) =>
        viewRow(node, filter, root.id) &&
        kinds.includes(node.supertag) &&
        (wanted === "" || words(node).toLowerCase().includes(wanted))
          ? [node.id]
          : []
      )
    ).size;
  };
  const verification = view("verification");
  const decisions = view("decisions");
  // A check's state is the page's one reading of it, as the server reads its results
  // (`tree-drafts.ts`'s `checkStanding`), so this flag and the Verification body count the same
  // checks as failed.
  const failing = countOf(verification.root, "check", (node) => node.state === "failed");
  // The decisions of the review list (R35): an agent's decision that waits and that a link ties
  // to a statement of this issue, which is what the tree marks `waiting` (`decision-review.ts`'s
  // `inReviewList`), so the signal, the list and the header's count cannot disagree.
  const toReview = countOf(decisions.root, "decision", (node) => node.waiting);
  // Each ask and each decision is one entry of the count: a person's decision is the answered
  // ask itself (`tree-drafts.ts`'s `addDecisions`), so no decision node restates an answer, and
  // a decision under an answered question is an agent's own that settles it.
  return {
    decisions: {
      count: decisions.matches,
      ...(toReview === 0 ? {} : { flag: { count: toReview, word: "to_review" } }),
      ...more(undefined),
    },
    plan: { count: count("plan"), ...more(root.views?.plan), unit: "steps" },
    spec: { count: count("spec"), ...more(root.views?.spec) },
    verification: {
      count: count("verification"),
      ...(failing === 0 ? {} : { flag: { count: failing, word: "failing" } }),
      ...more(root.views?.verification),
    },
  };
}
