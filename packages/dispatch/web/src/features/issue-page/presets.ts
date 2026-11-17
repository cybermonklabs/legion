import {
  drawnLine,
  firstLineOf,
  heldBy,
  isCheck,
  isStatement,
  lineWords,
  nodeWords,
  type TreeNode,
} from "./issue-tree";
import {
  type FILTER_SIGNAL_WORDS,
  NODE_TYPE_NAMES,
  type NodeType,
  type Preset,
} from "./page-words";

/**
 * The page with no filter on, and the four filters, as views of the one tree.
 *
 * With no filter on - Conversation - the view is the whole tree, folded to the frontier by the
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
 * Today's reads give no query node, so each filter is read off the tree
 * `issue-tree.ts` builds from the reads that exist: the kinds are R2's, and the context and the
 * parent's line are this module's.
 *
 * A node the address names outranks the filter: an address that names one node and then draws a
 * page without it would not have resolved at all (R20), so `named` is kept whatever the filter
 * draws, and counted as its kind is.
 *
 * This module is pure. It holds no React and makes no request: the rows (`IssueTree.tsx`), the
 * signals the filter row draws (`IssuePresets.tsx`) and the unit tests all read the same model.
 */

/** One of the four filters: every control of the row but Conversation, which turns them off. */
export type Filter = Exclude<Preset, "conversation">;

/** Whether `preset` is a filter rather than the page with no filter on. */
export function isFilter(preset: Preset): preset is Filter {
  return preset !== "conversation";
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

/** No node named by the address, which is every address but a deep link's. */
const EMPTY: ReadonlySet<string> = new Set();

/** What the view draws over the tree it was given. */
export interface PresetView {
  /** The ids of the rows drawn as context: with no filter on, the chain above a match of the
   *  words filter; under Verification, each requirement. */
  readonly context: ReadonlySet<string>;
  /** Whether this view is filtered at all: false for the page with no filter on and no words
   *  typed, whose fold is the frontier's alone (R1). While one is, every branch the view kept
   *  is drawn open (`IssueTree.tsx`): a filter exists to show what it matched, and the fold may
   *  not hide it. */
  readonly filtering: boolean;
  /** How many distinct nodes of the view's main kind it draws: a check drawn under two
   *  requirements is one node, so this is what the control's own count says. */
  readonly matches: number;
  /** The tree as the view leaves it. Under a filter its children are the filter's top rows. */
  readonly root: TreeNode;
  /** Whether the issue itself is the outline's first row: with no filter on, and never under a
   *  filter, which draws its own kinds as rows and no other (R2). */
  readonly rootRow: boolean;
}

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
 *  them"), never the stored source: `**bold** text` is found by `bold text` and not by `**`. */
function holds(node: TreeNode, wanted: string): boolean {
  return wanted === "" || nodeWords(node).toLowerCase().includes(wanted);
}

/**
 * The tree as `preset`, `query` and the address's own node leave it. With no filter on the root
 * is always drawn, because an issue whose words filter matches nothing still has to say so
 * somewhere, and it is context whenever nothing matched it.
 *
 * `memo` is the spec's summary callout, which the head draws as the issue's three sentences
 * (R8) and which is no row of the conversation (c-tree: "The memo callout is not a row of the
 * outline"), unless an address names it.
 */
export function presetView(
  root: TreeNode,
  preset: Preset,
  query: string,
  named: ReadonlySet<string> = EMPTY,
  memo?: string
): PresetView {
  const wanted = query.trim().toLowerCase();
  return isFilter(preset)
    ? filterView(root, preset, wanted, named)
    : conversationView(root, wanted, named, memo);
}

/** The page with no filter on: the whole tree but the memo, or under a words filter each match
 *  with the chain above it as context. */
function conversationView(
  root: TreeNode,
  wanted: string,
  named: ReadonlySet<string>,
  memo: string | undefined
): PresetView {
  const context = new Set<string>();
  const matched = new Set<string>();
  const prune = (node: TreeNode): TreeNode | undefined => {
    if (node.id === memo && !named.has(node.id)) return undefined;
    const children = node.children
      .map(prune)
      .filter((child): child is TreeNode => child !== undefined);
    const own = named.has(node.id) || holds(node, wanted);
    if (!own && children.length === 0) return undefined;
    if (own) matched.add(node.id);
    else context.add(node.id);
    return { ...node, children, held: heldBy(children) };
  };
  const filtering = wanted !== "";
  const pruned = prune(root);
  if (pruned !== undefined) {
    return { context, filtering, matches: matched.size, root: pruned, rootRow: true };
  }
  // Nothing under the issue matched, so the view is the issue alone, drawn as context.
  context.add(root.id);
  return {
    context,
    filtering,
    matches: 0,
    root: { ...root, children: [], held: 0 },
    rootRow: true,
  };
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
  // Verification draws a check under every requirement it serves, as the outline does; every
  // other filter draws a node at its own place alone.
  const everyPlacement = filter === "verification";
  const context = new Set<string>();
  const matched = new Set<string>();
  // The rows `node` gives the view, and whether `node` is itself one of them: either the node
  // with its own rows under it, or, where it is no row, the rows of its branch lifted to where
  // it stood.
  const lift = (node: TreeNode): { readonly drawn: boolean; readonly rows: TreeNode[] } => {
    if (!everyPlacement && node.transcluded) return { drawn: false, rows: [] };
    const parts = node.children.map(lift);
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
    const drawn = own || placing;
    // A drawn child under a node that is no row carries that node as its one line, cut by R1's
    // first-line rule from the words of the node's first block (`lineWords`), so a heading is
    // never run into the paragraph after it, and drawn as a row's first line is (`drawnLine`):
    // an ellipsis only where the cut ended inside a sentence. A node with no words - no text, or
    // a reading none of whose blocks holds a word - is named by the page's word for its kind
    // (`NODE_TYPE_NAMES`), never by its id, which is no name a reader reads (R22). A drawn
    // child's own rows are the child alone.
    const line =
      drawn || !parts.some((part) => part.drawn)
        ? undefined
        : {
            id: node.id,
            line: parentLineOf(node),
            // The parent's own address, which makes its line the link a reader follows to it
            // (R18, and R26: a step's row reaches the statement it serves).
            ...(node.reference === undefined ? {} : { reference: node.reference }),
            type: node.type,
          };
    const below = parts.flatMap((part) =>
      part.drawn && line !== undefined
        ? part.rows.map((row) => ({ ...row, parentLine: line }))
        : part.rows
    );
    if (!drawn) return { drawn: false, rows: below };
    if (own && kind !== undefined && counted.includes(kind)) matched.add(node.id);
    if (placing) context.add(node.id);
    return { drawn: true, rows: [{ ...node, children: below, held: heldBy(below) }] };
  };
  // The issue is no row of a filter, and no row's parent line: the page is the issue.
  const top = root.children.flatMap((child) => lift(child).rows);
  return {
    context,
    filtering: true,
    matches: matched.size,
    root: { ...root, children: top, held: heldBy(top) },
    rootRow: false,
  };
}

/** The one line a node that is no row of a filter is drawn as on its child's row: R1's first
 *  line of its first block's words (`lineWords`), and the page's word for its kind where those
 *  hold none (R22). */
function parentLineOf(node: TreeNode): string {
  const words = lineWords(node);
  return words === "" ? NODE_TYPE_NAMES[node.type] : drawnLine(firstLineOf(words));
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

/** The decisions of the view that restate a person's answer: the sections of `decisions.md`
 *  the tree hangs under the answered question they name. The Decisions filter draws that
 *  answer as the decision and the section as no entry (R2, R9, `decisions-filter/
 *  decisions-parts.ts`), so its count leaves them out. */
function restatedAnswers(root: TreeNode): number {
  const found = new Set<string>();
  const walk = (node: TreeNode): void => {
    for (const child of node.children) {
      if (node.type === "ask" && node.answer !== undefined && child.type === "decision") {
        found.add(child.id);
      }
      walk(child);
    }
  };
  walk(root);
  return found.size;
}

/** The signal each filter's control draws under `query`, which its count follows. */
export function filterSignals(
  root: TreeNode,
  query: string
): Readonly<Record<Filter, PresetSignal>> {
  const view = (filter: Filter) => presetView(root, filter, query);
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
  return {
    decisions: {
      count: decisions.matches - restatedAnswers(decisions.root),
      ...(toReview === 0 ? {} : { flag: { count: toReview, word: "to_review" } }),
    },
    plan: { count: view("plan").matches, unit: "steps" },
    spec: { count: view("spec").matches },
    verification: {
      count: verification.matches,
      ...(failing === 0 ? {} : { flag: { count: failing, word: "failing" } }),
    },
  };
}
