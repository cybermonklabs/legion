import type {
  Actor,
  ArtifactBlock,
  Ask,
  AskWithTrees,
  Comment,
  CommentWithTrees,
  DisplayTree,
  IssueCheckResults,
  IssueCoverage,
  IssueDecisions,
  IssueDetails,
  IssueReferences,
  MessageRead,
  ReferenceTarget,
} from "../../api/types";
import type { TextSource } from "../refs/text-references";
import { wordsOf } from "../refs/tree-fragment";
import type { GapKind, NodeState, NodeType } from "./page-words";
import { type Draft, treeContext, treeDrafts } from "./tree-drafts";

/**
 * The issue's node tree, built from the page's reads and nothing else
 * (dispatch://INFRA-1196 R4, `dispatch://INFRA-1196/artifact/plan-md#b-t13`).
 *
 * `buildIssueTree` is pure: it takes one value holding the page's reads and answers the issue's
 * root node. It sends no request, reads no document's text and holds no React, so the fold
 * (`tree-fold.ts`), the rows (`IssueTree.tsx`) and the unit tests all read the same model. The
 * placement and the names are that one function with that one input, so the day the chain read
 * lands it is the only thing that changes.
 *
 * ## The spine
 *
 * The `from` edge is the single placement authority: a decision sits under the question it
 * settles, a requirement under the node its `from` names, a check and a plan task under each
 * requirement their `serves` names (edited under the first, transcluded under the rest), and a
 * result under its check. A decision's `shaped` edge is that decision's chip and places nothing;
 * where a requirement's `from` and a decision's `shaped` disagree the node is placed by `from`
 * and the disagreement is drawn as a gap. A node whose spine parent is absent sits under the
 * issue, as a node like any other and never as an error: no gap says so, because under today's
 * reads every node of its kind lacks one, and a lack every sibling shares tells none of them
 * apart (R8). Every edge that is not the spine is a chip naming its target, and a node is drawn
 * again only under another requirement its own `serves` names.
 *
 * ## What today's reads hold, and what they do not
 *
 * Today's input forces three departures from the rules above, each read at the file and line
 * named, and each one `tree-drafts.ts` says again where it acts:
 *
 * - **No `from` on a requirement.** The `requirement` block carries no attributes at all
 *   (`packages/envoy/internal/dispatch/pmdoc/schema/blocks.json:70-75`, read 2026-11-05), so
 *   every requirement of the issue's spec sits under the issue, which is what c-home and c-spine
 *   read. A `requirement` block of any other document keeps its place
 *   under that document: the model holds no `from` for it and no coverage row either, so it is a
 *   block of a document and nothing more.
 * - **No `shaped` on a decision.** The decisions read answers `kind` and a boolean `unsure`
 *   (`packages/contracts/src/dispatch-api.ts:2519-2534`), so a decision's links to spec blocks
 *   and the asks its blocks mention are its chips, it keeps its place under its own document,
 *   and no disagreement is found in today's data: a `shaped` edge disagreeing with a `from` is
 *   drawn as the `shaped_disagreement` gap the day both edges exist.
 * - **No identities behind a requirement's coverage.** The coverage read counts the serving
 *   checks and plan tasks without naming them
 *   (`packages/contracts/src/dispatch-api.ts:805-816`), so a requirement draws those two counts
 *   and coverage's own gaps; what serves it is placed from each check's and each plan task's own
 *   `serves` pointer instead, which is the same edge read from the other end.
 *
 * ## The reads
 *
 * The issue read, the coverage read, the decisions read, the results read, the references read,
 * the conversation, each document's blocks, and the one batched reference read. A document's
 * blocks answer block ids, types and attributes and never text (`ArtifactBlock`), which is how
 * the tree holds a `serves` edge without reading a document the reader has not opened; a block's
 * own words come from the batched reference read, whose `name` for a block is that block's
 * stored excerpt. It is one read per document, never one per node.
 */

/** One edge of a node that is not the spine, drawn beside the node as its target's name. */
export interface NodeChip {
  /** Whether the batched reference read has answered for this reference. A target it answered
   *  with no text of its own (`name` empty, or `gone`) is told apart from one whose answer is
   *  still out by this and by nothing else, and the two are drawn differently: the first is
   *  named by its address's short form, the second by the address itself (`NodeRow.tsx`'s
   *  `Chip`). */
  readonly answered: boolean;
  /** Whether nothing answers to this reference now. */
  readonly gone: boolean;
  /** The words shown for the target, from the batched reference read; `""` until it answers. */
  readonly name: string;
  /** The target's own `dispatch://` address. */
  readonly ref: string;
  /** Whether the decision holding this chip changed after it first named the target and the
   *  target has not changed since (`DecisionLink.stale`); absent for every other edge. */
  readonly stale?: boolean;
}

/** What the coverage read counts for one requirement, with no identity behind either number. */
export interface NodeCounts {
  readonly checks: number;
  readonly planTasks: number;
}

/**
 * How a fact's value is drawn in a node's body: a command in a monospace block with its own
 * copy control (R15), a picture loaded through the bytes route the page already serves, one of
 * the page's own words, or the author's own text.
 */
export type FactDrawing = "command" | "picture" | "text" | "word";

/** One name/value fact of a node, which its body draws under its text: what the model holds
 *  about the node that a row has no line for, such as how a check is run and what a result
 *  recorded. The name is the key its label is declared under (`nodeFactLabel`). */
export interface NodeFact {
  readonly draw: FactDrawing;
  readonly name: string;
  readonly value: string;
}

/** One node of the issue's tree. A node drawn under more than one parent is one node per
 *  placement, each naming the others in `alsoUnder`. */
export interface TreeNode {
  /** The other requirements this node is also drawn under (R23); empty for a node drawn once. */
  readonly alsoUnder: readonly NodeChip[];
  /** Who made it, where the model holds an author (R7). */
  readonly author?: Actor;
  readonly children: readonly TreeNode[];
  readonly chips: readonly NodeChip[];
  /** What the coverage read counts for a requirement. */
  readonly counts?: NodeCounts;
  /** What the model holds about the node that a row has no line for, drawn in its body. */
  readonly facts: readonly NodeFact[];
  readonly gaps: readonly GapKind[];
  /** How many nodes this one holds, at any depth: what a folded row says it holds. */
  readonly held: number;
  /** The node's own address inside the issue: the issue key, an artifact slug,
   *  `<slug>#b-<block id>`, else the id the model gives it. */
  readonly id: string;
  /** Whether this node is an open question that waits on the reader, which the fold unfolds the
   *  path to whatever its depth (R6). */
  readonly question: boolean;
  /** The node's own `dispatch://` address, where it has one. */
  readonly reference?: string;
  /** Whether the decision this node holds was replaced, which folds its branch to one line
   *  (R1). */
  readonly replaced: boolean;
  /** The record `text` is, where it is one: an ask, a comment, a message, or the document a
   *  decision is a block of. A body drawn from `tree` links what the text cites against it, as
   *  every other surface drawing the same text does (`MarkdownBody`'s `source`). */
  readonly source?: TextSource;
  /** How it stands, for a type the model gives a state: one of the states the page has a word
   *  for, so a row draws that word and never the token (`NodeState`, R21). */
  readonly state?: NodeState;
  /** The node's own whole text; `""` where the model holds none. */
  readonly text: string;
  /** The server's reading of `text`, where the read that carried the text sends one: a
   *  decision's (`Decision.text_tree`), an ask's question (`question_tree`), a comment's and a
   *  message's body (`body_tree`). The row draws the node's words from it, so stored markdown
   *  is drawn as what it says and the browser parses none of it (R20). Every other read sends
   *  none - the coverage read's requirement, the results read's check and result, and the
   *  batched reference read's block excerpt - and such a node draws `text` as stored. */
  readonly tree?: DisplayTree;
  /** Whether this placement is a transclusion rather than the node's edited place. */
  readonly transcluded: boolean;
  readonly type: NodeType;
  /** Whether the model marks this decision unsure, which its row says in one word (R9). */
  readonly unsure?: true;
  /** Whether this decision waits for the reader's review, which is where Keep and Change are
   *  drawn and what the Decisions preset opens on (R7). */
  readonly waiting: boolean;
}

/** One document's blocks, as `GET /api/v1/artifacts/{id}/blocks` answers them. */
export interface DocumentBlocks {
  readonly artifactId: string;
  readonly blocks: readonly ArtifactBlock[];
}

/** An ask as the tree reads it: the issue read's open asks and the asks read both send the
 *  server's reading of the question beside it, which `Ask` leaves out. */
export type TreeAsk = Ask & Pick<AskWithTrees, "question_tree">;

/** The page's reads, as one value. Every field but the issue is optional, because the tree draws
 *  from the reads that have answered rather than waiting for the last of them. */
export interface IssueTreeReads {
  /** The issue's asks, from the conversation; merged with the issue read's open asks by id. */
  readonly asks?: readonly TreeAsk[];
  readonly coverage?: IssueCoverage;
  readonly decisions?: IssueDecisions;
  /** The issue's comments, each a node of the tree (`addTurns`), with each body's reading. */
  readonly comments?: readonly (Comment & Pick<CommentWithTrees, "body_tree">)[];
  /** One entry per document of the issue whose blocks the page has read. */
  readonly documents?: readonly DocumentBlocks[];
  readonly issue: IssueDetails;
  /** The issue's messages with their replies, each a node of the tree (`addTurns`). */
  readonly messages?: readonly MessageRead[];
  /** Which block cites each document the issue references (`via_blocks`). */
  readonly references?: IssueReferences;
  /** Each check of the Verification document with the results recorded against it. */
  readonly results?: IssueCheckResults;
  /** What every `dispatch://` address the tree holds points at, in one read. */
  readonly targets?: readonly ReferenceTarget[];
}

/** How many characters a row's first line may hold (R1). */
export const FIRST_LINE_LIMIT = 100;

/**
 * The node's own text to the first sentence end or `FIRST_LINE_LIMIT` characters, whichever
 * comes first, cut at a word wherever the limit falls near one. A line held to the limit that
 * holds no space at all - one unbroken token, a URL or an identifier - is cut at the limit
 * itself, because there is no word boundary to cut at and the limit is the row's whole budget.
 * `cut` says whether the text carries on past the line, which is what the row draws its
 * ellipsis for.
 */
export function firstLineOf(text: string): { readonly cut: boolean; readonly line: string } {
  const whole = text.replace(/\s+/gu, " ").trim();
  const sentence = /^.*?[.!?](?=\s|$)/u.exec(whole)?.[0];
  if (sentence !== undefined && sentence.length <= FIRST_LINE_LIMIT) {
    return { cut: sentence.length < whole.length, line: sentence };
  }
  if (whole.length <= FIRST_LINE_LIMIT) {
    return { cut: false, line: whole };
  }
  const window = whole.slice(0, FIRST_LINE_LIMIT + 1);
  const lastSpace = window.lastIndexOf(" ");
  const line = lastSpace <= 0 ? window.slice(0, FIRST_LINE_LIMIT) : window.slice(0, lastSpace);
  return { cut: true, line };
}

/** The node's own words as a reader reads them: the words of the server's reading of its text
 *  where its read sent one (`tree`), so its first line holds none of the symbols its body draws
 *  as formatting, and its text as stored otherwise. */
export function nodeWords(node: TreeNode): string {
  return node.tree === undefined ? node.text : wordsOf(node.tree);
}

/** The issue's whole tree, from the reads it was given. */
export function buildIssueTree(reads: IssueTreeReads): TreeNode {
  const context = treeContext(reads);
  return assemble(reads.issue, [...treeDrafts(reads, context).values()], context.chipOf);
}

/**
 * Every `dispatch://` address the tree holds: each node's own, so a block's words can be read,
 * and each chip's, so a chip can name its target. This is what the one batched reference read is
 * given, which is why the page draws every chip in one request rather than one per node.
 */
export function treeReferences(root: TreeNode): string[] {
  const refs = new Set<string>();
  const walk = (node: TreeNode): void => {
    if (node.reference !== undefined) refs.add(node.reference);
    for (const chip of [...node.chips, ...node.alsoUnder]) refs.add(chip.ref);
    for (const child of node.children) walk(child);
  };
  walk(root);
  return [...refs];
}

/**
 * The drafts as one tree under the issue: one node per placement, each naming the parents it is
 * also drawn under and counting the nodes it holds. A placement that would sit under itself, or
 * under a node it already holds, is dropped, so a `from` edge that ever points in a circle still
 * draws a tree.
 *
 * A parent no draft answers to is no placement at all: a document cited by a paragraph, which
 * is the document's own text and no node, and a reply to a turn the page never read. Rather
 * than dropping the node with its missing parent, it sits under the issue, which is where
 * every node whose parent is absent goes (R4). A draft the issue cannot reach is the same case
 * one step further out, and `bringHome` answers it the same way.
 */
function assemble(
  issue: IssueDetails,
  drafts: readonly Draft[],
  chipOf: (reference: string) => NodeChip
): TreeNode {
  const key = issue.key;
  const byParent = new Map<string, Draft[]>();
  const references = new Map<string, string>();
  const known = new Set(drafts.map((draft) => draft.id));
  /** Where each draft is actually drawn: the parents a draft of its own answers for, and the
   *  issue itself for a draft whose every named parent is absent. */
  const placements = new Map<string, readonly string[]>();
  for (const draft of drafts) {
    references.set(draft.id, draft.reference ?? draft.id);
    const named = draft.parents.filter((parent) => known.has(parent));
    placements.set(draft.id, named.length === 0 ? [key] : named);
  }
  bringHome(key, drafts, placements);
  for (const draft of drafts) {
    for (const parent of placements.get(draft.id) ?? []) {
      byParent.set(parent, [...(byParent.get(parent) ?? []), draft]);
    }
  }
  const childrenOf = (id: string, above: readonly string[]): readonly TreeNode[] =>
    (byParent.get(id) ?? [])
      .filter((child) => child.id !== id && !above.includes(child.id))
      .sort((one, other) => one.order - other.order || one.id.localeCompare(other.id))
      .map((child) => build(child, id, [...above, id]));
  const build = (draft: Draft, parent: string, above: readonly string[]): TreeNode => {
    const children = childrenOf(draft.id, above);
    const parents = placements.get(draft.id) ?? [];
    const others = parents.filter((one) => one !== parent);
    return {
      alsoUnder: parents.length > 1 ? others.map((one) => chipOf(references.get(one) ?? one)) : [],
      ...(draft.author === undefined ? {} : { author: draft.author }),
      children,
      chips: draft.chips,
      ...(draft.counts === undefined ? {} : { counts: draft.counts }),
      facts: draft.facts,
      gaps: draft.gaps,
      held: children.reduce((total, child) => total + child.held + 1, 0),
      id: draft.id,
      question: draft.question,
      ...(draft.reference === undefined ? {} : { reference: draft.reference }),
      replaced: draft.replaced,
      ...(draft.source === undefined ? {} : { source: draft.source }),
      ...(draft.state === undefined ? {} : { state: draft.state }),
      text: draft.text,
      transcluded: parents.indexOf(parent) > 0,
      ...(draft.tree === undefined ? {} : { tree: draft.tree }),
      type: draft.type,
      ...(draft.unsure === undefined ? {} : { unsure: draft.unsure }),
      waiting: draft.waiting,
    };
  };
  const children = childrenOf(key, [key]);
  return {
    alsoUnder: [],
    children,
    chips: [],
    facts: [],
    gaps: [],
    held: children.reduce((total, child) => total + child.held + 1, 0),
    id: key,
    question: false,
    replaced: false,
    // How Dispatch names an issue everywhere: its key and then its title, which is also what
    // tells this row from the title the identity block draws directly above it.
    text: `${key} ${issue.title}`,
    transcluded: false,
    type: "issue",
    waiting: false,
  };
}

/**
 * Puts back under the issue every draft the walk from the issue cannot reach, so that nothing
 * the reads hold is lost (R1): the tree is built downwards from the issue, so a ring of
 * placements nothing above it holds would be drawn nowhere, and every node inside it with it.
 * Two documents of the issue that cite each other make one - the references read answers each
 * one via the other, so each is placed under a block of the other (`tree-drafts.ts`'s
 * `citations`) - and so does a `from` edge that points in a circle.
 *
 * The ring is opened at one draft and no more, and that draft is a member of the ring itself:
 * the first of the ring members the issue cannot reach, in the order the tree draws siblings
 * in - its own order, then its id - comes home to the issue, and whatever that lets the issue
 * reach keeps the place its citation gave it. So the spec, which is the first document of the
 * issue, is drawn under the issue and the plan that cites it stays under the block of the spec
 * that cites the plan, and the same reads always draw the same tree.
 *
 * It is the ring's own member and never the lowest order among the drafts the issue cannot
 * reach, because a node hanging off a ring is unreachable too and sorts before every member of
 * it: a block's order is its place in its own document, and a requirement's is
 * `ROOT_ORDER.requirement` and up, so a plan task serving a requirement of a ring is the
 * first of them. Re-rooting that task would tear it off the requirement it serves and leave
 * the ring to be opened anyway.
 */
function bringHome(
  key: string,
  drafts: readonly Draft[],
  placements: Map<string, readonly string[]>
): void {
  const below = new Map<string, string[]>();
  for (const draft of drafts) {
    for (const parent of placements.get(draft.id) ?? []) {
      below.set(parent, [...(below.get(parent) ?? []), draft.id]);
    }
  }
  const reached = new Set<string>([key]);
  const walk = (from: string): void => {
    const queue = [from];
    for (;;) {
      const id = queue.pop();
      if (id === undefined) return;
      for (const child of below.get(id) ?? []) {
        if (reached.has(child)) continue;
        reached.add(child);
        queue.push(child);
      }
    }
  };
  walk(key);
  const ordered = [...drafts].sort(
    (one, other) => one.order - other.order || one.id.localeCompare(other.id)
  );
  /** Whether `id` is a member of a ring: a draft its own placements lead back to. */
  const inRing = (id: string): boolean => {
    const seen = new Set<string>();
    const queue = [...(placements.get(id) ?? [])];
    for (;;) {
      const above = queue.pop();
      if (above === undefined) return false;
      if (above === id) return true;
      if (seen.has(above)) continue;
      seen.add(above);
      queue.push(...(placements.get(above) ?? []));
    }
  };
  // Nothing the reads hold is lost (R1): every draft the issue cannot reach sits under a ring,
  // since a draft whose placements run out sits under the issue and is reached, so the rings
  // this opens answer for them all.
  for (;;) {
    const member = ordered.find((draft) => !reached.has(draft.id) && inRing(draft.id));
    if (member === undefined) break;
    placements.set(member.id, [key]);
    reached.add(member.id);
    walk(member.id);
  }
}
