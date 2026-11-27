import { blockFragment } from "../../refs/block-reference";
import type { ANSWER_LINES } from "../page-words";
import type { TreeAnswer, TreeAnswerNode } from "../tree-read/tree-read";

/**
 * Which entry of the node menu is grey, and the body of each write that is not (R2 to R5 of
 * INFRA-5705's spec, its Design's "Which entry is grey"). One pure module: it reads the held tree
 * read (`TreeAnswer`, every page the page has loaded merged into it) and nothing else, imports no
 * React, and holds no word a reader sees - each grey reason is a key of `ANSWER_LINES`
 * (`page-words-r.ts`) that starts `grey_`.
 *
 * It reads, for each node, `kind`, `supertag`, `parent`, `rank`, `document`, `block_id`,
 * `placement_seq`, `read_only`, `issue` and `text_tree`, and for the answer `converted`,
 * `view_of` and `cursor`; never the page's own `TreeNode`. `document` is set on a typed block of
 * the spec, the Verification document or the plan and gives that block's band; it is absent on a
 * block of the issue's own fragment, which is the band of the issue's own fragment. Under one
 * parent the blocks stand in the order INFRA-1253 R7 gives - spec, the issue's own fragment,
 * Verification, plan - and a rank stays within its document's band, so a move names a sibling of
 * the node's own document alone.
 *
 * It decides nothing that only the server can decide: whether a parent is in the walk, whether a
 * move empties a container and whether a delete is possible come back as refusals (R8), and
 * Delete on a node R2 admits is answered `{ kind: "ask" }`, for the dry run to settle. Where a
 * body names a sibling or a last child that may stand on a page the page has not loaded (the
 * answer holds a `cursor`), the answer is `{ kind: "read_next" }`: the caller reads the next page
 * and asks again.
 */

/** R1's nine entries, in R1's order. The word of an entry is `CONTROL_LABELS["edit_" + entry]`
 *  (`page-words-r.ts`); Lens's is part W's `LENS_WORDS.act`. */
export const ENTRIES = [
  "new_below",
  "new_under",
  "retype",
  "indent",
  "outdent",
  "move_up",
  "move_down",
  "delete",
  "lens",
] as const;

export type Entry = (typeof ENTRIES)[number];

/** The kinds Retype offers (R5), in order. `statement` is the tree read's supertag `requirement`. */
export const RETYPE_KINDS = ["criterion", "statement", "check", "plan_task"] as const;

export type RetypeKind = (typeof RETYPE_KINDS)[number];

/** A reason an entry is grey: a key of `ANSWER_LINES` that starts `grey_`. */
export type GreyReason = Extract<keyof typeof ANSWER_LINES, `grey_${string}`>;

/** The line a new row carries where it stands after the spec's blocks (a key of `ANSWER_LINES`). */
export type PlaceLine = Extract<keyof typeof ANSWER_LINES, "spec_band">;

/** The row the menu opens from: the node's own row, or its second row under the node of that
 *  read id that it `serves` (part T). */
export type PlacementRow = "own" | { readonly under: string };

export interface PlacementInput {
  /** The outline read the page holds, every page it has loaded merged into it. */
  readonly held: TreeAnswer;
  /** The node's read id (`TreeAnswerNode.id`). */
  readonly node: string;
  /** The issue's status; `"done"` closes the issue. */
  readonly status: string;
  readonly row?: PlacementRow;
  /** The read id of the row Graph is zoomed to (`TreeNode.readId`), which can draw two nodes
   *  (`rowReadIds`). */
  readonly zoomRoot?: string;
  /** The kinds the editor's schema for the fragment lets the editor make (default: all four). */
  readonly kinds?: readonly RetypeKind[];
  /** A new row the reader typed in that no `PATCH` has placed yet. */
  readonly unplaced?: true;
}

/** The body of the `PATCH` that moves a node, or places a new one. A node is named by its route
 *  id (`routeId`); `after` is absent where the body names no sibling, and null where the node
 *  becomes the first of its document under `parent`. */
export interface MoveBody {
  readonly parent: string;
  readonly after?: string | null;
}

/** Where a new block stands: `body` is the `PATCH` that gives it its place once it holds a rank
 *  and a `placement_seq`, `after` the read id of the row the new row is drawn right after under
 *  `body.parent`, null where it is that parent's first child. */
export interface NewPlace {
  readonly after: string | null;
  readonly body: MoveBody;
}

/** The attributes of the block a retype makes, which keep the node where it stands: the
 *  `dispatch://` address of the node it stands under, as `parent` or as `serves`. */
export type RetypeAttributes = { readonly parent: string } | { readonly serves: string };

export type RetypeOption =
  | { readonly kind: "grey"; readonly reason: GreyReason }
  | { readonly kind: "to"; readonly attributes: RetypeAttributes };

export type Verdict =
  | { readonly kind: "grey"; readonly reason: GreyReason }
  | { readonly kind: "move"; readonly body: MoveBody }
  | { readonly kind: "read_next" }
  | { readonly kind: "new"; readonly place: NewPlace; readonly line?: PlaceLine }
  | {
      readonly kind: "retype";
      /** One member for each kind of `PlacementInput.kinds`, in `RETYPE_KINDS`' order. */
      readonly kinds: Readonly<Partial<Record<RetypeKind, RetypeOption>>>;
    }
  | { readonly kind: "ask" }
  | { readonly kind: "open" };

export type Placement = Readonly<Record<Entry, Verdict>>;

/** The held read, indexed once for one answer. */
interface Walk {
  /** Each node by its read id, the first the walk reaches where two share one. */
  readonly byId: ReadonlyMap<string, TreeAnswerNode>;
  /** The children of each parent, by its read id, in the walk's order, which is the order the read
   *  answers them in: no context and nothing gone. */
  readonly children: ReadonlyMap<string, readonly TreeAnswerNode[]>;
  /** The ranked children of each parent, by its read id, in the walk's order. Ranks of one page
   *  are no ranks of another, so the walk's order is the one order of siblings. */
  readonly ranked: ReadonlyMap<string, readonly TreeAnswerNode[]>;
  /** The nodes whose children may continue on a page not loaded: the last node the loaded pages
   *  hold and each of its ancestors, while the answer names a next page. Empty otherwise. */
  readonly open: ReadonlySet<string>;
}

/** What one verdict is made of: the answer, its index and the node asked about. */
interface Focus {
  readonly held: TreeAnswer;
  readonly walk: Walk;
  readonly node: TreeAnswerNode;
}

type Parent = NonNullable<TreeAnswerNode["parent"]>;

type MoveEntry = "indent" | "outdent" | "move_up" | "move_down";

const NONE: readonly TreeAnswerNode[] = [];

/** The bands of INFRA-1253 R7, in the order the server stands them under one parent. The band of
 *  other documents is last and never appears: the tree read leaves their blocks out. */
const BAND = { own: 1, plan: 3, spec: 0, verification: 2 } as const;

/** The slugs the model gives an issue's Verification document and plan (`model.ArtifactSlug*`);
 *  the tree read sets `document` on a typed block of the spec, of these two documents and of no
 *  other, so any other slug is the spec's. */
const VERIFICATION_SLUG = "verification-md";
const PLAN_SLUG = "plan-md";

/** The slug of an issue's tree artifact, the issue room's own fragment. */
const TREE_SLUG = "tree";

/** The kinds of node a person writes under as under a criterion (R3): an ask, a comment and a
 *  message. */
const ITEM_KINDS: Readonly<Record<string, true>> = { ask: true, comment: true, message: true };

/** The kinds of node the read answers first among a node's children, before every ranked one,
 *  whatever their rank: a conversation item's own blocks, then the conversation - an ask, a
 *  comment, a message, a lens and a risk (the server's `conversationKinds`,
 *  `refs/tree_walk.go`). An item's own block is drawn in the item's row, never in one of its own. */
const ITEM_BLOCK_KINDS: Readonly<Record<string, true>> = {
  ask_block: true,
  comment_block: true,
  message_block: true,
};
const CONVERSATION_KINDS: Readonly<Record<string, true>> = {
  ...ITEM_KINDS,
  ...ITEM_BLOCK_KINDS,
  lens: true,
  risk: true,
};

const READ_NEXT: Verdict = { kind: "read_next" };

function grey(reason: GreyReason): Verdict {
  return { kind: "grey", reason };
}

function move(body: MoveBody): Verdict {
  return { body, kind: "move" };
}

/** Why R2 greys every editing entry of a node it does not admit (`refusal`). */
type Refusal = Extract<GreyReason, "grey_closed" | "grey_kind" | "grey_own_issue">;

/** The reasons R2 greys every editing entry of a block it does not admit (`refusal`): every other
 *  grey reason is an edge of a block it admits. The editor's keys (`move-keys.ts`) keep today's
 *  behaviour on a block of these, and write nothing at an edge of any other. */
export const NOT_ADMITTED: Readonly<Partial<Record<GreyReason, true>>> = {
  grey_closed: true,
  grey_kind: true,
  grey_own_issue: true,
} satisfies Readonly<Record<Refusal, true>>;

/** The entries of the menu with their verdicts for `input.node`, as the page asks them for the
 *  node's own row or its second row. A node the held read lacks is a bug of the caller. */
export function placementOf(input: PlacementInput): Placement {
  const { held } = input;
  const walk = walkOf(held);
  const node = walk.byId.get(input.node);
  if (node === undefined) throw new Error(`the held read holds no node ${input.node}`);
  const focus: Focus = { held, node, walk };
  const closed = input.status === "done";
  const edit = refusal(focus, closed, "edit");
  const under = refusal(focus, closed, "under");
  const unplaced = input.unplaced === true ? grey("grey_not_placed") : undefined;
  const moves = (entry: MoveEntry): Verdict => {
    if (edit !== undefined) return grey(edit);
    if (unplaced !== undefined) return unplaced;
    if (input.row !== undefined && input.row !== "own") return grey("grey_own_row");
    if (!hasRank(node)) return grey("grey_no_rank");
    switch (entry) {
      case "indent":
        return indent(focus);
      case "outdent":
        return outdent(focus, input.zoomRoot);
      case "move_up":
        return moveUp(focus);
      case "move_down":
        return moveDown(focus);
    }
  };
  return {
    new_below: edit !== undefined ? grey(edit) : (unplaced ?? newBelow(focus)),
    new_under: under !== undefined ? grey(under) : newUnder(focus),
    retype: edit !== undefined ? grey(edit) : retype(focus, input.kinds ?? RETYPE_KINDS),
    indent: moves("indent"),
    outdent: moves("outdent"),
    move_up: moves("move_up"),
    move_down: moves("move_down"),
    delete: edit !== undefined ? grey(edit) : { kind: "ask" },
    lens: { kind: "open" },
  };
}

/** What `placingOf` is asked: the held read, the new row's node and the place its entry made. */
export interface PlacingInput {
  readonly held: TreeAnswer;
  /** The new node: its read id, or the block id the page chose for it in the issue's own
   *  fragment, which the tree read returns the node by once its index has run. */
  readonly node: string;
  readonly place: NewPlace;
}

export type Placing =
  | { readonly kind: "wait" }
  | { readonly kind: "send"; readonly body: MoveBody; readonly placementSeq: number };

/** The new node's `PATCH` (R3): to wait until `held` holds the node with a `rank` and a
 *  `placement_seq`, since the route refuses a node with no rank, and then the one body that gives
 *  it its place with the `placement_seq` it is sent under. */
export function placingOf({ held, node, place }: PlacingInput): Placing {
  const nodes = held.nodes ?? [];
  const found =
    nodes.find((each) => each.id === node) ??
    nodes.find((each) => each.block_id === node && each.document === undefined);
  if (found === undefined || !hasRank(found)) return { kind: "wait" };
  const seq = found.placement_seq;
  if (seq === null || seq === undefined) return { kind: "wait" };
  return { body: place.body, kind: "send", placementSeq: seq };
}

/** Indexes `held`: its nodes by read id, each parent's children and its ranked children in the
 *  walk's order, and the nodes whose children may go on past the pages loaded. */
function walkOf(held: TreeAnswer): Walk {
  const byId = new Map<string, TreeAnswerNode>();
  const children = new Map<string, TreeAnswerNode[]>();
  const ranked = new Map<string, TreeAnswerNode[]>();
  const add = (under: Map<string, TreeAnswerNode[]>, parent: string, node: TreeAnswerNode) => {
    const siblings = under.get(parent);
    if (siblings === undefined) under.set(parent, [node]);
    else siblings.push(node);
  };
  let last: TreeAnswerNode | undefined;
  for (const node of held.nodes ?? []) {
    if (byId.has(node.id)) continue;
    byId.set(node.id, node);
    // The context the read carries stands in no issue's tree.
    if (node.role === "context") continue;
    last = node;
    if (node.state === "gone" || node.parent === null) continue;
    add(children, node.parent.id, node);
    if (hasRank(node)) add(ranked, node.parent.id, node);
  }
  const open = new Set<string>();
  if (typeof held.cursor === "string" && held.cursor !== "") {
    let id = last?.id;
    while (id !== undefined && !open.has(id)) {
      open.add(id);
      id = byId.get(id)?.parent?.id;
    }
  }
  return { byId, children, open, ranked };
}

function hasRank(node: TreeAnswerNode): boolean {
  return typeof node.rank === "string";
}

function rankedChildren(walk: Walk, parent: string): readonly TreeAnswerNode[] {
  return walk.ranked.get(parent) ?? NONE;
}

/** The band of `node` among its parent's children (INFRA-1253 R7). */
export function bandOf(node: TreeAnswerNode): number {
  const slug = node.document?.slug;
  if (slug === undefined) return BAND.own;
  if (slug === VERIFICATION_SLUG) return BAND.verification;
  if (slug === PLAN_SLUG) return BAND.plan;
  return BAND.spec;
}

/** Whether two nodes are blocks of one document: the artifact the read names, or the issue's own
 *  fragment where it names none. */
function sameDocument(left: TreeAnswerNode, right: TreeAnswerNode): boolean {
  const key = (node: TreeAnswerNode) => `${node.issue ?? ""}|${node.document?.artifact_id ?? ""}`;
  return key(left) === key(right);
}

/** Whether `parent`'s children of `band` may go on past the loaded pages: its children may
 *  continue there (`Walk.open`), and none of a later band has been read, which would show the
 *  children of `band` to be all held. */
function incomplete(walk: Walk, parent: string, band: number): boolean {
  return (
    walk.open.has(parent) && !rankedChildren(walk, parent).some((child) => bandOf(child) > band)
  );
}

/** A node as the node routes name it (`{id}` of `PATCH /api/v1/issues/{key}/nodes/{id}`): a block
 *  by its read id, which holds its own `#`, any other node by `<kind>/<read id>`. */
function routeId({ id, kind }: { readonly id: string; readonly kind: string }): string {
  return kind === "block" || id.includes("#") ? id : `${kind}/${id}`;
}

function parentOf(node: TreeAnswerNode): Parent {
  if (node.parent === null) throw new Error(`the held read gives ${node.id} no parent`);
  return node.parent;
}

/** The `dispatch://` address of a block node: the spec's by its own form, any other document of
 *  the issue under `artifact/<slug>`, and a block of the issue's own fragment under the tree
 *  artifact. */
function blockAddress(node: TreeAnswerNode, held: TreeAnswer): string {
  const key = node.issue ?? held.issue;
  const block = blockFragment(node.block_id ?? node.id);
  const slug = node.document?.slug;
  if (slug === undefined) return `dispatch://${key}/artifact/${TREE_SLUG}${block}`;
  if (bandOf(node) === BAND.spec) return `dispatch://${key}/spec${block}`;
  return `dispatch://${key}/artifact/${encodeURIComponent(slug)}${block}`;
}

/** The kind of node R2 and R3 tell: a criterion, a statement, a check or a plan task (`typed`), a
 *  prose block (`prose`: a block that is no finding and opens no typed block), an ask, a comment
 *  or a message (`item`, which takes New node under alone), and any other node. */
function classOf(node: TreeAnswerNode): "item" | "other" | "prose" | "typed" {
  if (node.kind !== "block") return Object.hasOwn(ITEM_KINDS, node.kind) ? "item" : "other";
  switch (node.supertag) {
    case "criterion":
    case "requirement":
    case "check":
    case "plan_task":
      return "typed";
    case "ask":
      return "item";
    case "block":
    case "prose":
      return node.parent?.kind === "lens" ? "other" : "prose";
    default:
      return "other";
  }
}

/** Whether `node` is one of R2's typed blocks (`classOf`): a criterion, a statement, a check or a
 *  plan task, inside which Shift+Enter at the end makes a paragraph (R3). */
export function isTyped(node: TreeAnswerNode): boolean {
  return classOf(node) === "typed";
}

/** Why R2 greys an entry on the node, or undefined where it admits it. On a closed issue every
 *  editing entry says the issue is closed, whatever the node; on an open one the reason is the
 *  node's kind, then whose issue it is. `offered` is `under` for New node under, which an ask, a
 *  comment and a message take too. */
function refusal(
  { held, node }: Focus,
  closed: boolean,
  offered: "edit" | "under"
): Refusal | undefined {
  if (closed) return "grey_closed";
  const kind = classOf(node);
  if (!held.converted || kind === "other" || (kind === "item" && offered === "edit")) {
    return "grey_kind";
  }
  // The read marks a node of another issue with that issue, and `read_only`.
  if (node.read_only === true || (typeof node.issue === "string" && node.issue !== held.issue)) {
    return "grey_own_issue";
  }
  return undefined;
}

/** Indent: under the ranked sibling that stands above the node, after its last ranked child of
 *  the node's own document. */
function indent({ walk, node }: Focus): Verdict {
  const parent = parentOf(node);
  const siblings = rankedChildren(walk, parent.id);
  const above = siblings[siblings.indexOf(node) - 1];
  if (above === undefined) return grey("grey_no_sibling_above");
  if (incomplete(walk, above.id, bandOf(node))) return READ_NEXT;
  const inside = rankedChildren(walk, above.id).filter((child) => sameDocument(child, node));
  const last = inside.at(-1);
  return move({ after: last === undefined ? null : routeId(last), parent: routeId(above) });
}

/** Outdent: under the parent's parent, after the old parent where that is a block of the node's
 *  own document; grey where the parent's row is the row Graph is zoomed to, read as either node
 *  that row draws (`rowReadIds`). */
function outdent({ held, walk, node }: Focus, zoomRoot: string | undefined): Verdict {
  const parent = parentOf(node);
  if (parent.id === held.issue) return grey("grey_directly_under_issue");
  if (zoomRoot !== undefined && rowReadIds(held, parent.id).includes(zoomRoot)) {
    return grey("grey_zoom_root");
  }
  const old = walk.byId.get(parent.id);
  if (old === undefined) throw new Error(`the held read holds no parent ${parent.id}`);
  const above = routeId(parentOf(old));
  if (old.kind === "block" && sameDocument(old, node)) {
    return move({ after: routeId(old), parent: above });
  }
  return move({ parent: above });
}

/** The ranked siblings of the node's own document, in order, and the node's place among them. */
function siblingsOf(
  { walk, node }: Focus,
  parent: Parent
): { readonly at: number; readonly siblings: readonly TreeAnswerNode[] } {
  const siblings = rankedChildren(walk, parent.id).filter((each) => sameDocument(each, node));
  return { at: siblings.indexOf(node), siblings };
}

/** Move up: after the sibling that is to stand right before the node, null where it becomes the
 *  first. */
function moveUp(focus: Focus): Verdict {
  const parent = parentOf(focus.node);
  const { at, siblings } = siblingsOf(focus, parent);
  if (at <= 0) return grey("grey_first");
  const before = siblings[at - 2];
  return move({ after: before === undefined ? null : routeId(before), parent: routeId(parent) });
}

/** Move down: after the sibling below the node. */
function moveDown(focus: Focus): Verdict {
  const parent = parentOf(focus.node);
  const { at, siblings } = siblingsOf(focus, parent);
  const below = siblings[at + 1];
  if (below === undefined) {
    return incomplete(focus.walk, parent.id, bandOf(focus.node)) ? READ_NEXT : grey("grey_last");
  }
  return move({ after: routeId(below), parent: routeId(parent) });
}

/** New node below (R3): right after the node where it is a block of the issue's own fragment;
 *  right after the last block of the spec under the parent where it is a block of the spec, which
 *  is the first place of the new block's own band (`after: null`, since a body with no `after`
 *  would put it after every ranked sibling of that band), with the line that says so where that
 *  is not right below the node; grey on a block of any other document, where a block of the
 *  issue's own fragment would stand above it. */
function newBelow({ walk, node }: Focus): Verdict {
  const parent = parentOf(node);
  switch (bandOf(node)) {
    case BAND.own:
      return {
        kind: "new",
        place: { after: node.id, body: { after: routeId(node), parent: routeId(parent) } },
      };
    case BAND.spec: {
      if (incomplete(walk, parent.id, BAND.spec)) return READ_NEXT;
      const end = rankedChildren(walk, parent.id)
        .filter((each) => sameDocument(each, node))
        .at(-1);
      const place: NewPlace = {
        after: end?.id ?? node.id,
        body: { after: null, parent: routeId(parent) },
      };
      return end === undefined || end === node
        ? { kind: "new", place }
        : { kind: "new", line: "spec_band", place };
    }
    default:
      return grey("grey_below_document");
  }
}

/** New node under (R3): first among the node's children of the issue's own fragment, drawn where
 *  the read answers the node once placed. The read answers a node's children in one order - the
 *  conversation (`CONVERSATION_KINDS`), then the ranked children by rank, band by band, then the
 *  rest - so the new block stands right before the first child, in the walk's order, that the
 *  read answers after it: a ranked child of its own band or a later one, or one of the rest. The
 *  new row is drawn right after the last child before that one that the outline draws in a row of
 *  its own, and first where there is none. */
function newUnder({ walk, node }: Focus): Verdict {
  if (incomplete(walk, node.id, BAND.own)) return READ_NEXT;
  let before: TreeAnswerNode | undefined;
  for (const child of walk.children.get(node.id) ?? NONE) {
    const earlier =
      Object.hasOwn(CONVERSATION_KINDS, child.kind) || (hasRank(child) && bandOf(child) < BAND.own);
    if (!earlier) break;
    if (!Object.hasOwn(ITEM_BLOCK_KINDS, child.kind)) before = child;
  }
  return {
    kind: "new",
    place: { after: before?.id ?? null, body: { after: null, parent: routeId(node) } },
  };
}

/** Whether the top node of the node's `text_tree` says it is one paragraph (R5): a `doc` with a
 *  single `paragraph`. */
function isOneParagraph(node: TreeAnswerNode): boolean {
  const tree = node.text_tree;
  if (tree === undefined || tree.type !== "doc" || !Array.isArray(tree.content)) return false;
  const content: readonly { readonly type?: unknown }[] = tree.content;
  return content.length === 1 && content[0]?.type === "paragraph";
}

/** The `dispatch://` address of the conversation item `parent` is, which a criterion and a
 *  statement under it name as their `parent` (R5): an ask, a comment or a message by its own node,
 *  and an ask by the block of a document it was asked on (its `asked_on` tuple). Graph draws the
 *  question once, in that block's row, read as the ask, the first of the two the read's walk
 *  reaches (`rowReadIds`): New node under on that row puts a node under the ask, and Indent of the
 *  ranked sibling right after the block puts one under the block, since the block is ranked and
 *  the ask is not. Undefined for any other parent. */
function itemAddress(parent: Parent, held: TreeAnswer): string | undefined {
  const issue = parent.issue ?? held.issue;
  if (Object.hasOwn(ITEM_KINDS, parent.kind))
    return `dispatch://${issue}/${parent.kind}/${parent.id}`;
  if (parent.kind !== "block" || parent.supertag !== "ask") return undefined;
  const asked = (held.tuples ?? []).find(
    (tuple) => tuple.kind === "asked_on" && tuple.from.kind === "ask" && tuple.to.id === parent.id
  );
  return asked === undefined ? undefined : `dispatch://${issue}/ask/${asked.from.id}`;
}

/** The read ids that name the row Graph draws the node of read id `id` in, `id` first: the node's
 *  own, and, for an ask and the block of supertag `ask` of a document its `asked_on` tuple names,
 *  the other of the two as well. The outline draws the ask by the block the asks read indexed it
 *  from, the block of the asks row, which is the block the store states that tuple to, and draws
 *  the block by its id in its document (`tree-nodes.ts`' `askNodeId`, `treeIdOfNode`): one row,
 *  read as the first of the two the read's walk reaches, so a stored parent of either is that row.
 *  An ask anchored on any other block states no `asked_on`, and a block the read gives no
 *  `document`, one of the issue's own fragment, is drawn by its block id alone: neither shares a
 *  row with an ask. */
export function rowReadIds(held: TreeAnswer, id: string): readonly string[] {
  for (const { from, kind, to } of held.tuples ?? []) {
    if (kind !== "asked_on" || from.kind !== "ask" || (from.id !== id && to.id !== id)) continue;
    const block = held.nodes?.find((node) => node.id === to.id);
    if (block?.supertag !== "ask" || block.document === undefined) continue;
    return [id, from.id === id ? to.id : from.id];
  }
  return [id];
}

/** Retype (R5): on a prose block that is one paragraph alone, each kind its place can give, with
 *  the attributes that keep the node where it stands. */
function retype({ held, walk, node }: Focus, kinds: readonly RetypeKind[]): Verdict {
  if (classOf(node) !== "prose" || !isOneParagraph(node)) return grey("grey_not_paragraph");
  const parent = parentOf(node);
  const address = (): string => {
    const above = walk.byId.get(parent.id);
    if (above === undefined) throw new Error(`the held read holds no parent ${parent.id}`);
    return blockAddress(above, held);
  };
  const item = itemAddress(parent, held);
  const to = (attributes: RetypeAttributes): RetypeOption => ({ attributes, kind: "to" });
  const refused = (reason: GreyReason): RetypeOption => ({ kind: "grey", reason });
  const view = typeof held.view_of === "string" && held.view_of !== "";
  const options: Readonly<Record<RetypeKind, () => RetypeOption>> = {
    // A child that is a view of its ancestor's spec has it written by its agent (root R32).
    criterion: () => {
      if (view) return refused("grey_view_criterion");
      return item === undefined ? refused("grey_criterion_parent") : to({ parent: item });
    },
    statement: () => {
      if (parent.supertag === "criterion") return to({ serves: address() });
      return item === undefined ? refused("grey_statement_parent") : to({ parent: item });
    },
    check: () =>
      parent.supertag === "requirement" ? to({ serves: address() }) : refused("grey_check_parent"),
    plan_task: () =>
      parent.supertag === "requirement"
        ? to({ serves: address() })
        : refused("grey_plan_task_parent"),
  };
  const offered: { -readonly [Kind in RetypeKind]?: RetypeOption } = {};
  for (const kind of RETYPE_KINDS) {
    if (kinds.includes(kind)) offered[kind] = options[kind]();
  }
  return { kind: "retype", kinds: offered };
}
