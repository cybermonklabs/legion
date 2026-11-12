import type { TreeNode } from "./issue-tree";
import type { NodeType, Preset } from "./page-words";
import type { OpenedBranches } from "./tree-fold";

/**
 * The five presets as queries over the one tree (dispatch://INFRA-1196 R3,
 * `dispatch://INFRA-1196/artifact/plan-md#b-t9`).
 *
 * A preset is not a panel of its own and holds no nodes of its own: it is a predicate over the
 * tree `issue-tree.ts` built, and what it draws is every node that matches together with the
 * chain of ancestors above each match. So no node is duplicated, none moves between presets -
 * a node sits under the same parent in every preset that draws it - and the Tree preset, whose
 * predicate takes everything, is the whole tree.
 *
 * An ancestor drawn only because a match sits under it is *context*: the row is faint and
 * carries `data-node-context`, and the arrow keys reach it like any other row.
 *
 * The filter (`?q=`) is part of the query rather than a second pass over its answer, so a
 * preset's own count says how many nodes it holds as the page draws it.
 *
 * A node the address names outranks the query: an address that names one node and then draws a
 * page without it would not have resolved at all (R19), so `named` is kept whatever the preset
 * asks for, and counted as a match rather than as context.
 *
 * This module is pure. It holds no React and makes no request: the rows (`IssueTree.tsx`), the
 * counts the preset row draws (`IssuePresets.tsx`) and the unit tests all read the same model.
 */

/**
 * The node types each preset's query keeps, and `undefined` for the Tree preset, which keeps
 * every node. A result is in the Checks preset because it is the evidence recorded against a
 * check and sits under it, and an ask is in the Decisions preset because an open question is
 * what a decision answers and the old Decisions tab listed the two together; nothing else is
 * in two presets' types.
 */
const PRESET_TYPES: Readonly<Record<Preset, readonly NodeType[] | undefined>> = {
  checks: ["check", "result"],
  decisions: ["ask", "decision"],
  plan: ["plan_task"],
  spec: ["requirement"],
  tree: undefined,
};

/** No node named by the address, which is every address but a deep link's. */
const EMPTY: ReadonlySet<string> = new Set();

/** What the preset draws over the tree it was given. */
export interface PresetView {
  /** The ids of the rows drawn only as the ancestor of a match. */
  readonly context: ReadonlySet<string>;
  /** How many distinct nodes the preset's query matched: a node drawn under two requirements is
   *  one node, so this is what the preset's own count says. */
  readonly matches: number;
  /** The ids of the nodes a filter opens: every node of this view that holds a match below it,
   *  which is the path down to each match. A filter exists to show what it matched, so while one
   *  is on, its matches are drawn - the fold is the unfiltered page's own rule (`tree-fold.ts`)
   *  and may not hide the thing the reader filtered for. The reader's own hand still outranks
   *  it, as it outranks the frontier: a branch they folded shut stays shut
   *  (`IssueTree.tsx`'s `shown`). Empty while nothing filters - the Tree preset with no query,
   *  where every node is a match and the frontier alone says what is open (R1). */
  readonly opened: ReadonlySet<string>;
  /** The tree as the preset leaves it: every match with the chain above it, and nothing else. */
  readonly root: TreeNode;
}

/**
 * The tree as `preset`, `query` and the address's own node leave it. The root is always drawn,
 * because an issue whose preset matches nothing still has to say so somewhere, and it is
 * context whenever nothing matched it.
 */
export function presetView(
  root: TreeNode,
  preset: Preset,
  query: string,
  named: ReadonlySet<string> = EMPTY
): PresetView {
  const types = PRESET_TYPES[preset];
  const wanted = query.trim().toLowerCase();
  // Whether this view is a filter at all: the Tree preset with no query keeps every node, so
  // nothing was filtered for and the fold is the frontier's alone.
  const filtering = types !== undefined || wanted !== "";
  const keeps = (node: TreeNode): boolean =>
    named.has(node.id) ||
    ((types === undefined || types.includes(node.type)) &&
      (wanted === "" || node.text.toLowerCase().includes(wanted)));
  const context = new Set<string>();
  const matched = new Set<string>();
  const opened = new Set<string>();
  const prune = (node: TreeNode): TreeNode | undefined => {
    const children = node.children
      .map(prune)
      .filter((child): child is TreeNode => child !== undefined);
    const own = keeps(node);
    if (!own && children.length === 0) return undefined;
    if (own) matched.add(node.id);
    else context.add(node.id);
    // The path down to each match, read off the walk that found them: a node holds a match
    // below it when a child of it is one or holds one itself, and the children are pruned
    // before their parent, so each is already in `matched` or in `opened` by here.
    if (filtering && children.some((child) => matched.has(child.id) || opened.has(child.id))) {
      opened.add(node.id);
    }
    return { ...node, children, held: children.reduce((held, child) => held + child.held + 1, 0) };
  };
  const pruned = prune(root);
  if (pruned !== undefined) {
    return { context, matches: matched.size, opened, root: pruned };
  }
  // Nothing under the issue matched, so the preset is the issue alone, drawn as context.
  context.add(root.id);
  return { context, matches: 0, opened, root: { ...root, children: [], held: 0 } };
}

/**
 * The fold the page draws a filtered view with: the reader's own opens and closes
 * (`OpenedBranches`), and the path down to each match opened beside them (`PresetView.opened`).
 *
 * A filter exists to show what it matched, so while one is on its matches are drawn: the
 * frontier's rule - every branch folded until something open runs through it - is the
 * unfiltered page's own, and left to itself it hides the very rows the reader filtered for.
 * The path is opened at every depth, as the address's own path is, so a match below the drawn
 * levels is a row of its own rather than a count (`tree-fold.ts`'s `DRAWN_LEVELS`).
 *
 * The reader's hand outranks it, which is the one way it differs from the address's path: a
 * branch they folded shut stays shut, because `closed` is left as they left it and the fold
 * reads that first.
 */
export function openedWithMatches(
  opened: OpenedBranches,
  unfolded: ReadonlySet<string>
): OpenedBranches {
  if (unfolded.size === 0) return opened;
  return {
    branches: new Set([...opened.branches, ...unfolded]),
    closed: opened.closed,
    deeper: new Set([...opened.deeper, ...unfolded]),
  };
}

/** How many nodes each preset holds under `query`, which is the count its own tab draws. */
export function presetCounts(
  root: TreeNode,
  query: string,
  presets: readonly Preset[]
): Readonly<Record<string, number>> {
  const counts: Record<string, number> = {};
  for (const preset of presets) {
    counts[preset] = presetView(root, preset, query).matches;
  }
  return counts;
}
