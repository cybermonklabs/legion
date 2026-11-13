import type { TreeNode } from "./issue-tree";

/**
 * How much of the issue's tree is drawn (dispatch://INFRA-1196 R1, R6). Pure, over the model
 * `issue-tree.ts` builds, so the rows, the zoom and the unit tests fold the same way.
 *
 * The first drawing is the frontier's own: the path to what is open and unanswered is
 * unfolded whatever its depth, and every other branch arrives folded to one row saying how
 * many nodes it holds. Nothing is counted against anything - there is no budget and no fixed
 * depth - because a branch is folded unless the frontier or the reader opened it, so what is
 * open costs no other branch a row.
 *
 * The reader's own hand outranks the rule, both ways: a branch they folded stays shut though
 * the frontier runs through it, and a branch they opened stays open though nothing under it
 * is open at all (`OpenedBranches`).
 *
 * ## What is open, by kind
 *
 * The frontier is the path to the nodes `unanswered` names, and that function says, kind by
 * kind, what "open and unanswered" is for a node of that kind and why. An open node's own
 * branch is not unfolded by it: the frontier is the path *to* it, so what it holds is a set
 * the reader opens, as every other branch is.
 *
 * ## What the depth still decides
 *
 * `DRAWN_LEVELS` no longer decides what is drawn: it decides how far one opening reaches. A
 * branch the reader opens below the third level draws one row saying how many nodes it holds
 * rather than the nodes themselves, which they open in place (`DeeperRow`, `OpenedBranches`'s
 * `deeper`), so one gesture deep in the tree cannot flood the page. The frontier outranks it:
 * a path to something open is drawn to its end whatever the depth (R6).
 */

/** How deep a branch may sit and still draw its nodes when it is opened: below this, one
 *  opening draws one row with a count instead, which the reader opens in place. */
export const DRAWN_LEVELS = 3;

/** What the reader has done to the fold by hand: the branches they unfolded, the branches they
 *  folded shut, and the deeper branches they opened in place. */
export interface OpenedBranches {
  /** The nodes whose children the reader unfolded. */
  readonly branches: ReadonlySet<string>;
  /** The nodes whose children the reader folded shut. A branch the frontier draws of its own
   *  accord - the path to what is open and unanswered - is drawn until the reader closes it,
   *  and this is what says they did: without it a reader could open a branch and never close
   *  it again, and the Fold control on it would do nothing. */
  readonly closed: ReadonlySet<string>;
  /** The nodes whose branch below `DRAWN_LEVELS` the reader opened in place. */
  readonly deeper: ReadonlySet<string>;
}

/** One row of the tree as the fold leaves it: the node, how deep it sits, and what its own
 *  twisty says. */
export interface TreeRow {
  /** `true` when its children are drawn, `false` when it holds children and they are not, and
   *  undefined for a node that holds none. */
  readonly expanded?: boolean;
  /** How many nodes the row holds that are drawn as no row of their own: the branch it folds
   *  shut, and the branch below the drawn levels that folds to one line under it. Undefined
   *  for a row whose children are all drawn. */
  readonly folded?: number;
  readonly level: number;
  readonly node: TreeNode;
  /** The ids of the nodes this row sits under, the outermost first and its own parent last;
   *  empty for the root. One node is drawn under two parents (R23), so this is what tells its
   *  rows apart, down to the rows below them (`rowKeyOf`). */
  readonly trail: readonly string[];
}

/** One row standing for a branch below the drawn levels, which opens it in place. */
export interface DeeperRow {
  /** How many nodes the branch holds. */
  readonly held: number;
  readonly level: number;
  /** The node whose branch it stands for. */
  readonly of: TreeNode;
  /** The ids of the nodes that node sits under, as a `TreeRow`'s own trail. */
  readonly trail: readonly string[];
}

/** Every row the tree draws, in reading order: a node's own row, or the one row a branch below
 *  the drawn levels folds to. */
export type DrawnRow =
  | ({ readonly kind: "node" } & TreeRow)
  | ({ readonly kind: "deeper" } & DeeperRow);

/** Nothing opened or folded by hand, which is how the page opens. */
export const NOTHING_OPENED: OpenedBranches = {
  branches: new Set(),
  closed: new Set(),
  deeper: new Set(),
};

/**
 * Whether the node is itself open and unanswered, which is what the frontier is the path to
 * (R1). One answer per kind the tree draws, because "open" is a different fact in each, and
 * each is named here with the reason the issue's readers wait on it.
 */
function unanswered(node: TreeNode): boolean {
  switch (node.type) {
    // A question nobody has answered. `question` is the ask's own `state === "open"`
    // (`tree-drafts.ts`'s `addAsks`), and an ask that was answered, declined or withdrawn
    // asks the reader nothing.
    case "ask":
      return node.question;
    // A check that has not passed: any of the six other states `checkState` returns
    // (`tree-drafts.ts`), which the page draws as Failed, Out of date, Retracted, Cannot tell,
    // Could not read and No result (`NODE_STATE_WORDS.check`). Each leaves the work it covers
    // unproved, which is what the reader came to see; only `passed` says the issue need not
    // look.
    case "check":
      return node.state !== "passed";
    // A decision that waits for the reader's review: Keep and Change are drawn on exactly
    // these (`DecisionActions.tsx`), so it is the one kind of decision the issue is waiting
    // on an answer to. A decision a person made, one the reader kept and one a later
    // decision replaced are all settled.
    case "decision":
      return node.waiting;
    // A plan task, never: Dispatch records no completion for a `plan_task` block at all -
    // the block declares `serves` and `needs` and nothing else
    // (`packages/envoy/internal/dispatch/pmdoc/schema/blocks.json`), and the issue's own
    // `tasks_done` counts the checked task items of its primary document
    // (`packages/envoy/internal/dispatch/docs/task_progress.go`), which name no block. A
    // task nobody can mark done is no evidence that anything is open: read as open it would
    // hold every task of every plan on the frontier for the life of the issue, and the
    // frontier is what is open and *unanswered*. What the work of a task leaves open the
    // issue can show on its own - the check that has not passed, the requirement nothing
    // proves - and each of those opens the path to itself. The day the model holds a task's
    // own state, this reads it, and a task nobody has finished puts its path back.
    case "plan_task":
      return false;
    // A requirement the issue cannot yet show it meets: the coverage read counts no check
    // serving it, so nothing proves it. A requirement whose checks have not all passed needs
    // no rule of its own - each of those checks is open itself and sits under it, so the
    // path to that check runs through the requirement and opens its branch down to the check
    // that is not passing. A requirement the coverage read does not hold at all - a
    // `requirement` block of a document that is not this issue's spec - carries no counts,
    // and the page knows nothing about what proves it, so it says nothing either.
    case "requirement":
      return node.counts !== undefined && node.counts.checks === 0;
    // Nothing else holds an answer of its own for the reader to wait on. A note, a document
    // and an image are what somebody wrote; a child issue is read and answered on its own
    // page, and the children section is where a reader waits on one (R10); a comment, a
    // message and a handoff are what somebody said, and what is unanswered in a conversation
    // is the ask inside it, which puts its turn on the frontier as that ask's ancestor; a
    // result is a record of what a run saw, and its check carries the verdict; and the issue
    // itself is the root, which is drawn whatever this says.
    case "callout":
    case "child_issue":
    case "comment":
    case "document":
    case "handoff":
    case "image":
    case "issue":
    case "message":
    case "result":
      return false;
  }
}

/**
 * The ids of every node above something open and unanswered (`unanswered`): the path the fold
 * unfolds whatever its depth. The open nodes themselves are not in it - what one holds is a
 * set the reader opens, like any other branch - so an open node that holds a deeper open one
 * is in it, as that one's ancestor.
 */
export function frontier(root: TreeNode): ReadonlySet<string> {
  const path = new Set<string>();
  const walk = (node: TreeNode, above: readonly string[]): void => {
    if (unanswered(node)) {
      for (const id of above) path.add(id);
    }
    for (const child of node.children) walk(child, [...above, node.id]);
  };
  walk(root, []);
  return path;
}

/**
 * Every row the tree draws over `root`, in reading order. The root is always unfolded, because
 * an issue drawn as one folded row would say nothing at all; a branch below it is unfolded
 * when the frontier runs through it or when the reader opened it, and nothing else - and
 * folded shut, whichever of those holds, once the reader has folded it by hand, because a
 * branch a reader cannot close is a branch they cannot read past.
 *
 * `above` is the path down to `root` itself, outermost first: the ids a reader who zoomed into
 * it left above the tree (`IssueTree.tsx`). It seeds the trails, so every row is keyed by its
 * path from the issue however much of the tree is drawn, and the pin, the open body and the
 * tree's one tab stop a key holds survive a zoom in both directions (`rowKeyOf`).
 */
export function treeRows(
  root: TreeNode,
  opened: OpenedBranches = NOTHING_OPENED,
  above: readonly string[] = []
): DrawnRow[] {
  const path = frontier(root);
  const rows: DrawnRow[] = [];
  const walk = (node: TreeNode, level: number, trail: readonly string[]): void => {
    const holds = node.children.length > 0;
    // Every branch arrives folded, and the frontier is what opens one: the path to what is
    // open and unanswered, drawn to its end whatever its depth. A replaced decision's branch
    // folds to the one line that settled it even there - the frontier does not open it, and
    // nothing deletes it (R1). The reader's own hand outranks both: what they folded stays
    // shut until they open it again, and what they opened stays open.
    const unfolded =
      level === 1 ||
      (holds &&
        !opened.closed.has(node.id) &&
        (opened.branches.has(node.id) || (!node.replaced && path.has(node.id))));
    // How far one opening reaches: a branch opened below the drawn levels draws one row of
    // its own rather than its nodes, which the reader opens in place, so one gesture deep in
    // the tree cannot flood the page. The frontier outranks it, since a path to something
    // open is drawn to its end. Either way the node's children are drawn as no row of
    // theirs, so the row says how many it holds.
    const toOneLine =
      unfolded &&
      holds &&
      level >= DRAWN_LEVELS &&
      !opened.deeper.has(node.id) &&
      !path.has(node.id);
    rows.push({
      ...(holds ? { expanded: unfolded } : {}),
      ...(holds && (!unfolded || toOneLine) ? { folded: node.held } : {}),
      kind: "node",
      level,
      node,
      trail,
    });
    if (!unfolded || !holds) return;
    if (toOneLine) {
      rows.push({ held: node.held, kind: "deeper", level: level + 1, of: node, trail });
      return;
    }
    const below = [...trail, node.id];
    for (const child of node.children) walk(child, level + 1, below);
  };
  walk(root, 1, above);
  return rows;
}

/** One row the zoomed tree draws below the node it is on: the node, and the path it is drawn
 *  under. That path is the row's own rather than the zoomed node's, because one node is drawn
 *  under two parents (R23) and the zoomed tree reaches it both ways. */
interface ZoomedRow {
  readonly node: TreeNode;
  readonly under: readonly string[];
}

/**
 * The rows the tree draws at 1024 px and below, where it is read one node at a time: the node
 * the reader is on and the rows below it as one line each, with no second indent level under
 * them. The path down to it is the breadcrumb above the rows (`zoomBreadcrumb`) rather than a
 * row apiece, because the identity block leaves a phone one screenful of rows, not five.
 *
 * The tree opens on the issue itself (`zoom` undefined), so every document, decision and child
 * of the issue is one row away and the breadcrumb always walks back to the whole issue: a
 * reader on a phone reaches every node a reader at 1440 px reaches, one zoom at a time (R6,
 * R7).
 *
 * A row the preset draws only as the ancestor of a match (`context`) is no stop of its own
 * here: the one screen a phone has goes to what the preset matched, so a node's children are
 * the nearest rows below it the preset kept. The Checks preset at 390 px therefore draws its
 * checks under the issue, where at 1440 px each sits under the requirement it serves and that
 * requirement is drawn faint above it (R3, R17).
 *
 * One path is drawn deeper than that, and only one: the trail down to the first open question
 * in reading order. Each node of it keeps its own row and the next node of the trail follows
 * it, at the same one level, so that question is on the first screen with nothing unfolded by
 * hand even while the tree is on the issue itself (R6). The branches those nodes hold besides
 * the trail are a zoom away, because a screen that is one trail deep is still one screen.
 *
 * That trail is the open question's alone, and not the whole frontier `treeRows` unfolds: a
 * phone has one screen, and a trail per failing check and uncovered requirement would fill it
 * with paths rather than with the one thing R6 puts there. Everything else the frontier
 * reaches is a zoom away, as every other branch is.
 */
export function zoomedRows(
  root: TreeNode,
  zoom: string | undefined,
  context: ReadonlySet<string> = new Set()
): DrawnRow[] {
  const above = pathTo(root, zoom ?? root.id) ?? [root];
  const node = above.at(-1) ?? root;
  const trail = firstQuestion(root, [root]) ?? [];
  // The path down to the node the tree is zoomed on, which its rows are told apart by even
  // though the breadcrumb draws it rather than a row apiece.
  const path = above.slice(0, -1).map((one) => one.id);
  const under = [...path, node.id];
  // The children of `of` the preset kept, each with the path it is drawn under: a match
  // hoisted out of a context ancestor hangs under that ancestor, not under the zoomed node.
  const matched = (of: TreeNode, within: readonly string[]): ZoomedRow[] =>
    of.children.flatMap((child) =>
      context.has(child.id)
        ? matched(child, [...within, child.id])
        : [{ node: child, under: within }]
    );
  // Every row below the node, in reading order: its children, with the rest of the trail
  // after whichever of them the trail runs through. Each node of that trail tail hangs under
  // the node of the trail above it, so a node this walk reaches twice - hoisted out of a
  // context ancestor and again down a trail - is two rows with two keys rather than one key
  // drawn twice (`rowKeyOf`).
  const below: ZoomedRow[] = [];
  for (const child of matched(node, under)) {
    const at = trail.findIndex((one) => one.id === child.node.id);
    if (at === -1) {
      below.push(child);
      continue;
    }
    let within = child.under;
    for (const one of trail.slice(at)) {
      below.push({ node: one, under: within });
      within = [...within, one.id];
    }
  }
  const rows: DrawnRow[] = [
    {
      ...(node.children.length > 0 ? { expanded: true } : {}),
      kind: "node",
      level: 1,
      node,
      trail: path,
    },
  ];
  for (const [index, drawn] of below.entries()) {
    // Its own twisty says "open" exactly where the trail carries its next node into the row
    // under it, and "closed", with a count, wherever its branch is a zoom away.
    const child = drawn.node;
    const next = below[index + 1]?.node;
    const unfolded = next !== undefined && child.children.includes(next);
    rows.push({
      ...(child.children.length === 0
        ? {}
        : unfolded
          ? { expanded: true }
          : { expanded: false, folded: child.held }),
      kind: "node",
      level: 2,
      node: child,
      trail: drawn.under,
    });
  }
  return rows;
}

/** The nodes above the one the zoomed tree is on, outermost first: the breadcrumb a reader
 *  zooms back out through. Empty while the tree is on the issue itself. */
export function zoomBreadcrumb(root: TreeNode, zoom: string | undefined): TreeNode[] {
  return (pathTo(root, zoom ?? root.id) ?? [root]).slice(0, -1);
}

/** The chain from the root down to `id`, the root first; undefined when the tree holds no such
 *  node. */
export function pathTo(root: TreeNode, id: string): TreeNode[] | undefined {
  if (root.id === id) return [root];
  for (const child of root.children) {
    const below = pathTo(child, id);
    if (below !== undefined) return [root, ...below];
  }
  return undefined;
}

/** The chain down to the first open question in reading order, the root first and the question
 *  itself last; undefined when the tree holds none. */
function firstQuestion(node: TreeNode, above: readonly TreeNode[]): TreeNode[] | undefined {
  if (node.question) return [...above];
  for (const child of node.children) {
    const found = firstQuestion(child, [...above, child]);
    if (found !== undefined) return found;
  }
  return undefined;
}
