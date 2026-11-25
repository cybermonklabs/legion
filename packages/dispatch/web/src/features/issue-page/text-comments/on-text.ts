import type { Anchor, MessageRead } from "../../../api/types";
import { firstOfEachId } from "../decision-review";
import type { IssueTreeReads } from "../issue-tree";
import type { TreeAnswer, TreeAnswerNode } from "../tree-read/tree-read";
import { GATES } from "./gates";

/**
 * The one rule of what is on text (R1): which comment or message stands in a thread beside the
 * words it is on, rather than as a row of Conversation, a filter or Graph. Part T's `draftTurn`
 * drafts no row for an item it answers for, and the margin draws each such item in its thread
 * (`text-threads.ts`), so each view draws each item once.
 *
 * A comment is a thread's root where its anchor names a block of a document (`artifact_id` and
 * `block_id`, orphaned or not) or, from G2, a node; a comment or a message is one where it answers
 * an item by one of that item's blocks with a quote (`reply_block.quote`, from G4) and that item is
 * in no thread on text. Every other item whose chain of answers reaches a root is in that root's
 * thread: a comment's `answers` (the comment it answers, which the server keeps beside `reply_to`,
 * the head of the post's thread), a message's `in_reply_to`. A chain that runs in a circle ends
 * where it comes round.
 *
 * The rule reads the tree read's `anchor` of the item's node first, and the comments read's
 * `anchor` wherever the node carries none (part T's test answers carry it there alone).
 */

/** A comment as the comments read answers it, with its body's reading. */
export type ReadComment = NonNullable<IssueTreeReads["comments"]>[number];

/** A message as the messages read answers it, a reply among them. */
export type ReadMessage = MessageRead["message"];

/** A block of an item that a reply answers, as the comments and messages reads answer it. */
type ReplyBlock = NonNullable<ReadComment["reply_block"] | ReadMessage["reply_block"]>;

/** An item the rule is asked about: a comment or a message, by its id and its kind as the tree
 *  read names it. An item of any other kind is on no text. */
export type OnTextItem = Pick<TreeAnswerNode, "id" | "supertag">;

/** A reply's block as INFRA-5703 answers it (G4): with the words the reply quotes and their match,
 *  which `quotedBlock` reads before the generated contract declares them. */
export type QuotedReplyBlock = ReplyBlock & {
  readonly occurrence?: number;
  readonly quote?: string;
};

/** Where a thread's root is on text: what its words are in, and the words. */
export type TextPlace =
  /** A block of a document (`anchor` with `artifact_id` and `block_id`), with the node of the
   *  nearest typed block that holds an orphaned anchor's paragraph (`container`, INFRA-1253, once
   *  the tree read answers it) and the match of the quote the read names (`occurrence`, G3b). */
  | {
      readonly anchor: Anchor;
      readonly container: string | undefined;
      readonly kind: "block";
      readonly occurrence: number | undefined;
    }
  /** A node (INFRA-1253 R31's node anchor, G2), by its read id. */
  | {
      readonly kind: "node";
      readonly node: string;
      readonly occurrence: number;
      readonly orphaned: boolean;
      readonly quote: string;
    }
  /** A block of the item a reply answers, by that item's id, with the words it quotes (G4). */
  | {
      readonly block: QuotedReplyBlock;
      readonly item: string;
      readonly kind: "item";
      readonly occurrence: number;
      readonly quote: string;
    };

/** What the rule answers for an item on text: the root of a thread and where it is, or a reply
 *  in the thread of the root `root` names. */
export type OnText =
  | { readonly kind: "root"; readonly place: TextPlace }
  | { readonly kind: "reply"; readonly root: string };

/** What the rule reads, each read by id (`onTextReads`). */
export interface OnTextReads {
  /** The node of each typed block of the answer, by its document and its block (`blockKey`). */
  readonly blocks: ReadonlyMap<string, TreeAnswerNode>;
  readonly comments: ReadonlyMap<string, ReadComment>;
  /** Each message of the messages read, each reply among them. */
  readonly messages: ReadonlyMap<string, ReadMessage>;
  /** Each node of the tree read's answer by its read id, the first the walk holds. */
  readonly nodes: ReadonlyMap<string, TreeAnswerNode>;
}

/** The members of a tree read's `anchor` that this base's contract does not declare, each read
 *  where the server answers it: `node`, the node a node anchor names (G2, INFRA-1253 R31);
 *  `occurrence`, the match of the quote the anchor names (G2 for a node anchor, G3b for a
 *  document anchor); `container_node_id`, the node id of the nearest typed block that holds an
 *  orphaned document anchor's paragraph (INFRA-1253). */
interface AnchorExtras {
  readonly container_node_id?: string;
  readonly node?: { readonly id: string };
  readonly occurrence?: number | null;
}

/** A comment of the comments read as a caller that sends `?node_anchor=true` reads it (G2,
 *  INFRA-1253 R31): `node_anchor`, on a comment whose anchor names a node or a block. */
interface NodeAnchored {
  readonly node_anchor?: {
    readonly node: { readonly id: string };
    readonly occurrence: number | null;
    readonly orphaned: boolean;
    readonly quote: string;
  };
}

/** The kinds of node an item answers: what the tree read hangs a reply under. */
const ITEM_KINDS: Readonly<Record<string, true>> = { ask: true, comment: true, message: true };

/** The key of a typed block in `OnTextReads.blocks`: its document's id and its block id. */
function blockKey(artifactId: string, blockId: string): string {
  return `${artifactId}\u0000${blockId}`;
}

/** The answer and the comments and messages reads as the rule reads them, each by id. */
export function onTextReads(
  answer: TreeAnswer | undefined,
  reads: Pick<IssueTreeReads, "comments" | "messages">
): OnTextReads {
  const nodes = firstOfEachId(answer?.nodes ?? []);
  const blocks = new Map<string, TreeAnswerNode>();
  for (const node of nodes.values()) {
    if (node.document === undefined || node.block_id === undefined) continue;
    const key = blockKey(node.document.artifact_id, node.block_id);
    if (!blocks.has(key)) blocks.set(key, node);
  }
  const messages = new Map<string, ReadMessage>();
  for (const thread of reads.messages ?? []) {
    messages.set(thread.message.id, thread.message);
    for (const reply of thread.replies ?? []) messages.set(reply.id, reply);
  }
  return {
    blocks,
    comments: new Map((reads.comments ?? []).map((comment) => [comment.id, comment])),
    messages,
    nodes,
  };
}

/**
 * Whether `item` is on text, and how: the root of a thread with the place of its words, a reply
 * in a root's thread, or null for every other item - a post on the issue, a reply that names a
 * block with no quote, and a reply whose quoted item was deleted, which reads with no quote
 * (INFRA-5703 leaves `quote` out where `reply_block.deleted_at` is set), so each stays a row.
 *
 * Until G5 (`GATES.COMPOSER`) one more item stays a row, as part T's rule drew it: a comment
 * anchored in a block that no node of the answer stands for (by `container_node_id` or by its
 * block), which the tree read hangs under the issue - a comment on the text of a node the answer
 * lacks, and a Change of a `decisions.md` section, which the outline draws under its decision.
 * Part T's row helper (`e2e/issue-page-t-rows.ts`) and part Q's row of a Change
 * (`e2e/issue-page-q.e2e.ts`), which change at G5, read them as rows.
 */
export function onTextOf(item: OnTextItem, reads: OnTextReads): OnText | null {
  return ruleOf(item, reads, new Set());
}

function ruleOf(item: OnTextItem, reads: OnTextReads, passed: Set<string>): OnText | null {
  if ((item.supertag !== "comment" && item.supertag !== "message") || passed.has(item.id)) {
    return null;
  }
  passed.add(item.id);
  const anchored = item.supertag === "comment" ? anchorPlace(item.id, reads) : undefined;
  if (anchored !== undefined) {
    return rowUntilComposer(item.id, anchored, reads) ? null : { kind: "root", place: anchored };
  }
  const answered = answeredOf(item, reads);
  if (answered === undefined) return null;
  const above = ruleOf(answered, reads, passed);
  if (above !== null) {
    return { kind: "reply", root: above.kind === "root" ? answered.id : above.root };
  }
  const quoted = GATES.REPLY_QUOTE ? quotedPlace(item, answered.id, reads) : undefined;
  return quoted === undefined ? null : { kind: "root", place: quoted };
}

/** The place a comment's anchor names: the tree read's anchor of its node, else the comments
 *  read's `anchor`, else, from G2, its `node_anchor`; undefined where none names a block or a
 *  node. */
function anchorPlace(id: string, reads: OnTextReads): TextPlace | undefined {
  const anchor = reads.nodes.get(id)?.anchor;
  if (anchor !== undefined) return anchorPlaceOf(anchor);
  const comment = reads.comments.get(id);
  if (comment === undefined) return undefined;
  if (comment.anchor !== null) return anchorPlaceOf(comment.anchor);
  if (!GATES.NODE_ANCHOR) return undefined;
  const held: ReadComment & NodeAnchored = comment;
  const named = held.node_anchor;
  return named === undefined
    ? undefined
    : {
        kind: "node",
        node: named.node.id,
        occurrence: named.occurrence ?? 0,
        orphaned: named.orphaned,
        quote: named.quote,
      };
}

/** The place `anchor` names: from G2 the node a node anchor names, else the block of a document
 *  anchor that names one; undefined for an anchor that names neither. */
function anchorPlaceOf(anchor: Anchor): TextPlace | undefined {
  const extras: Anchor & AnchorExtras = anchor;
  if (GATES.NODE_ANCHOR && extras.node !== undefined) {
    return {
      kind: "node",
      node: extras.node.id,
      occurrence: extras.occurrence ?? 0,
      orphaned: anchor.orphaned,
      quote: anchor.quote,
    };
  }
  if (anchor.artifact_id === "" || anchor.block_id === null || anchor.block_id === "") {
    return undefined;
  }
  return {
    anchor,
    container: extras.container_node_id,
    kind: "block",
    occurrence: extras.occurrence ?? undefined,
  };
}

/** Whether the comment anchored at `place` stays a row until G5 (`onTextOf`): a block anchor that
 *  no node of the answer stands for, on a comment the tree read hangs under the issue. */
function rowUntilComposer(id: string, place: TextPlace, reads: OnTextReads): boolean {
  if (GATES.COMPOSER || place.kind !== "block") return false;
  const parent = reads.nodes.get(id)?.parent ?? null;
  return (parent === null || parent.supertag === "issue") && placeNodes(place, reads).length === 0;
}

/** The item `item` answers: a comment's `answers`, else the head of its thread (`reply_to`),
 *  else the ask it answers; a message's `in_reply_to`. Where neither read holds the item, the
 *  item the tree read hangs it under. */
function answeredOf(item: OnTextItem, reads: OnTextReads): OnTextItem | undefined {
  if (item.supertag === "comment") {
    const comment = reads.comments.get(item.id);
    if (comment !== undefined) {
      const answered = comment.answers ?? comment.reply_to;
      if (answered !== null) return { id: answered, supertag: "comment" };
      return comment.ask_id === null ? undefined : { id: comment.ask_id, supertag: "ask" };
    }
  } else {
    const message = reads.messages.get(item.id);
    if (message !== undefined) {
      return message.in_reply_to === null
        ? undefined
        : { id: message.in_reply_to, supertag: "message" };
    }
  }
  const parent = reads.nodes.get(item.id)?.parent ?? null;
  return parent !== null && ITEM_KINDS[parent.supertag] === true
    ? { id: parent.id, supertag: parent.supertag }
    : undefined;
}

/** The place of a reply that quotes a block of the item `answered` names (G4); undefined where
 *  its block holds no quote. */
function quotedPlace(
  item: OnTextItem,
  answered: string,
  reads: OnTextReads
): TextPlace | undefined {
  const block =
    item.supertag === "comment"
      ? reads.comments.get(item.id)?.reply_block
      : reads.messages.get(item.id)?.reply_block;
  if (block == null) return undefined;
  const quoted = quotedBlock(block);
  return quoted.quote === undefined || quoted.quote === ""
    ? undefined
    : {
        block: quoted,
        item: answered,
        kind: "item",
        occurrence: quoted.occurrence ?? 0,
        quote: quoted.quote,
      };
}

/** A reply's block with the quote INFRA-5703 answers beside it (G4), which the generated contract
 *  of this base does not declare: read as an optional member. */
export function quotedBlock(block: ReplyBlock): QuotedReplyBlock {
  return block;
}

/**
 * The nodes of the answer that `place` names, in the order R2 tries them: for a block, the node
 * of its nearest typed block where the read names one (`container_node_id`), then the node of
 * the block itself (its document's id and its block id); the node a node anchor names; the item a
 * reply answers. A place whose words are in a block no node stands for names none.
 */
export function placeNodes(place: TextPlace, reads: OnTextReads): TreeAnswerNode[] {
  const named: TreeAnswerNode[] = [];
  const add = (node: TreeAnswerNode | undefined): void => {
    if (node !== undefined) named.push(node);
  };
  switch (place.kind) {
    case "block": {
      if (place.container !== undefined) add(reads.nodes.get(place.container));
      const blockId = place.anchor.block_id;
      if (blockId !== null) add(reads.blocks.get(blockKey(place.anchor.artifact_id, blockId)));
      break;
    }
    case "node":
      add(reads.nodes.get(place.node));
      break;
    case "item":
      add(reads.nodes.get(place.item));
      break;
  }
  return named;
}
