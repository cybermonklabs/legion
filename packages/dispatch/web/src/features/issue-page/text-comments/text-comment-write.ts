import type { Actor } from "../../../api/types";
import type { ComposerAnchor, ReplyTarget } from "../../conversation/composer-model";
import { blockIdOfNode } from "../decision-review";
import type { TreeAnswerNode } from "../tree-read/tree-read";
import { GATES, type GateName } from "./gates";

/**
 * What a comment on selected words writes (R5): each node kind's write, by the place its node
 * stands in, and the request each write sends. The server refuses an anchor by the artifact and
 * the quote alone, never by a block's type (INFRA-1253 `api/anchors.go`), so a block of a
 * document writes the same request whatever its type, and so does a block written in the room.
 * A block inside an ask, a comment or a message is drawn in that item's row, so its selection is
 * the item's and writes the item's reply with that block (`QuotedWords.block`). This module says
 * where a write goes - the anchor or the reply `MentionComposer` sends it with
 * (`composerAddressOf`) - and sends nothing: the send is the composer's own (`send-request.ts`).
 */

/** Where a node stands, which decides its write: as a block of a document the issue holds, as a
 *  block written in the room (the issue's `tree`), or as an item of its own (an ask, a comment, a
 *  message, a lens, a finding, a risk, a constraint or a result). */
export type Place = "document" | "item" | "room";

/** The request a selection of words is written with:
 *  - `block`: `POST /api/v1/issues/{key}/comments` with `anchor {artifact, block_id, quote,
 *    occurrence}`;
 *  - `tree_block`: the same with `artifact` the issue's `tree` artifact;
 *  - `ask_part`: a comment with `ask_id`, `reply_to_block`, `quote` and `occurrence`;
 *  - `comment`: a comment with `reply_to`, `reply_to_block`, `quote` and `occurrence`;
 *  - `message`: `POST /api/v1/issues/{key}/messages` with `in_reply_to`, `in_reply_to_block`,
 *    `quote` and `occurrence`;
 *  - `node`: a comment with `anchor {node, quote, occurrence}`. */
export type TextRequest = "ask_part" | "block" | "comment" | "message" | "node" | "tree_block";

/** One write of a kind: the request, the place that makes it the kind's write, and the gate
 *  that must be on the base for the control to stand (`GATES`). The control also waits for
 *  `COMPOSER`. */
export interface TextWrite {
  readonly gate: GateName;
  readonly place: Place;
  readonly request: TextRequest;
}

/** What a node kind writes: its writes, one for each place it can stand in, or no control with the
 *  reason. */
export type KindWrites =
  | { readonly control: false; readonly reason: string }
  | { readonly control: true; readonly writes: readonly TextWrite[] };

function write(place: Place, request: TextRequest, gate: GateName): TextWrite {
  return { gate, place, request };
}

function writes(...each: TextWrite[]): KindWrites {
  return { control: true, writes: each };
}

function none(reason: string): KindWrites {
  return { control: false, reason };
}

/** A block of a document, whatever its type: anchored in that document. */
const DOCUMENT_BLOCK = write("document", "block", "COMPOSER");
/** A block written in the room, whatever its type: anchored in the issue's `tree` (G3a). */
const ROOM_BLOCK = write("room", "tree_block", "TREE_ARTIFACT_ANCHOR");
/** A lens, a finding, a risk, a constraint or a result: anchored on the node itself (G2). */
const NODE = write("item", "node", "NODE_ANCHOR");

/**
 * Each node kind's write (`NodeType` of `issue-tree.ts` and each kind of 1253 R30), or no control
 * with its reason. A kind with no row is a defect (`kinds.test.ts`).
 */
export const WRITES: Readonly<Record<string, KindWrites>> = {
  ask: writes(write("item", "ask_part", "REPLY_QUOTE")),
  callout: writes(DOCUMENT_BLOCK, ROOM_BLOCK),
  check: writes(DOCUMENT_BLOCK),
  child_issue: none(
    "A child issue's row draws that issue's title and state, which are no text of a node here."
  ),
  comment: writes(write("item", "comment", "REPLY_QUOTE")),
  constraint: writes(NODE),
  criterion: writes(ROOM_BLOCK),
  decision: writes(DOCUMENT_BLOCK),
  document: none("No view draws a document as a row (root R3)."),
  finding: writes(NODE),
  handoff: none("The tree read's walk leaves a handoff out (1253 R8), so no view draws its row."),
  image: none("A picture draws no text to select."),
  issue: none("The issue is the page's head, which no view draws as a row."),
  lens: writes(NODE),
  message: writes(write("item", "message", "REPLY_QUOTE")),
  plan_task: writes(DOCUMENT_BLOCK),
  prose: writes(DOCUMENT_BLOCK, ROOM_BLOCK),
  reading: writes(DOCUMENT_BLOCK, ROOM_BLOCK),
  requirement: writes(DOCUMENT_BLOCK),
  result: writes(NODE),
  risk: writes(NODE),
};

/** The node a control is asked for: its kind, the place it stands in, its visibility, and the
 *  issue it belongs to (the page's own, or another issue's where the view draws a node of it). */
export interface WriteSubject {
  readonly issue: string;
  readonly kind: string;
  /** The issue whose page draws the node, where it is known: a node of another issue takes a
   *  write only once its issue's comments are in the answer that draws it (G3c). */
  readonly page?: string;
  readonly place: Place;
  readonly visibility: "author" | "project";
}

/** A write that stands: the kind's row, and the issue whose route (`/api/v1/issues/{key}/...`)
 *  it goes to, which is the node's own issue. */
export interface LiveWrite extends TextWrite {
  readonly issue: string;
}

/**
 * The write of `subject` where its gate and `COMPOSER` are on in `gates` (`GATES` where none is
 * given), or null: no control on a node whose visibility is `author`, on a child issue, on a node
 * of another issue before G3c, or on a kind whose gate is off.
 */
export function writeFor(
  subject: WriteSubject,
  gates: Readonly<Record<GateName, boolean>> = GATES
): LiveWrite | null {
  const row = WRITES[subject.kind];
  if (subject.visibility === "author" || row === undefined || !row.control || !gates.COMPOSER) {
    return null;
  }
  const foreign = subject.page !== undefined && subject.issue !== subject.page;
  if (foreign && !gates.OTHER_ISSUE_COMMENTS) return null;
  const found = row.writes.find((one) => one.place === subject.place);
  return found === undefined || !gates[found.gate] ? null : { ...found, issue: subject.issue };
}

/** The kinds that stand as items of their own: an ask, a comment, a message, and a lens with its
 *  findings, risks, constraints and results. Each is read-only in the room (R6, root R30), so Enter
 *  on its row puts the read-only caret in its text (`read-only-caret.ts`), where a block of a
 *  document takes its editor's caret instead. */
export const ITEM_PLACE_KINDS: Readonly<Record<string, true>> = {
  ask: true,
  comment: true,
  constraint: true,
  finding: true,
  lens: true,
  message: true,
  result: true,
  risk: true,
};

/** The place `node` stands in: an item of its own, a block of the document the tree read names
 *  (`document`), or else a block written in the room. */
function placeOf(node: Pick<TreeAnswerNode, "document" | "supertag">): Place {
  if (ITEM_PLACE_KINDS[node.supertag] === true) return "item";
  return node.document === undefined ? "room" : "document";
}

/** `node` as `writeFor` asks for it on the page of the issue `page`: its kind, its place, its
 *  visibility and its own issue, which is the page's where the read names none. A finding of a
 *  lens reads with `supertag` `"block"` or `"lens_block"` under its lens (`lens-drafts.ts`), so
 *  it maps to `"finding"` here. */
export function subjectOf(
  node: Pick<TreeAnswerNode, "document" | "issue" | "supertag" | "visibility"> & {
    readonly parent?: TreeAnswerNode["parent"];
  },
  page: string
): WriteSubject {
  const kind =
    (node.supertag === "block" || node.supertag === "lens_block") &&
    node.parent?.supertag === "lens"
      ? "finding"
      : node.supertag;
  return {
    issue: node.issue ?? page,
    kind,
    page,
    place: placeOf({ document: node.document, supertag: kind }),
    visibility: node.visibility,
  };
}

/** Selected words as a write names them: the words as plain text reads them, which match of them
 *  the selection is (`occurrence`), and the item's block they are in, null for a node that has
 *  one text. */
export interface QuotedWords {
  readonly block: string | null;
  readonly occurrence: number;
  readonly quote: string;
}

/** The node the words are in, as the tree read answers it, with what the page knows beside it:
 *  the `tree` artifact of the issue it belongs to, which a block written in the room is anchored
 *  in (G3a). */
export interface TextWriteTarget {
  /** The item's authors as the read lists them: a reply names the first as whom it answers. */
  readonly author?: readonly Actor[] | null;
  readonly block_id?: string;
  readonly document?: { readonly artifact_id: string };
  /** The node's id in the read: the item a reply answers, or the node a node anchor names. */
  readonly id: string;
  /** The node's store kind in the tree read (`lens`, `risk`, `constraint`, `result`, or `block`
   *  for a finding), used to form the store address `anchor.node` takes when `id` is bare. */
  readonly kind?: string;
  readonly tree?: string | null;
}

/** Where a composer's send on selected words goes, as `MentionComposer` takes it: the anchor that
 *  names the words, or the reply to the item they are in with the item's block and the words. */
export interface ComposerAddress {
  readonly anchor: ComposerAnchor | undefined;
  readonly replyTo: ReplyTarget | null;
}

/** The post each reply answers, as the composer's reply target names it (`parentKind`). */
const REPLY_PARENTS = { ask_part: "ask", comment: "comment", message: "message" } as const;

/**
 * Where the composer's send on `words` of `at` goes by `write` (R5), on the route of the write's
 * issue, which `send-request.ts` builds the request of: a comment anchored in the block of the
 * node's document, or the room's, or on the node; or a reply with the quote to the ask, the
 * comment or the message the words are in. Null where the read gives the write nothing to name:
 * a block with no artifact to anchor in, or an item with no author to answer.
 */
export function composerAddressOf(
  write: LiveWrite,
  at: TextWriteTarget,
  words: QuotedWords
): ComposerAddress | null {
  const { occurrence, quote } = words;
  if (write.request === "block" || write.request === "tree_block") {
    const artifact = write.request === "block" ? at.document?.artifact_id : at.tree;
    if (artifact === undefined || artifact === null) return null;
    return {
      anchor: { artifact, block_id: blockIdOfNode(at), occurrence, quote },
      replyTo: null,
    };
  }
  if (write.request === "node") {
    // A node anchor names no document (`artifact: ""`), and a reference in the comment is read in
    // the issue's spec (`draftSource`).
    return {
      anchor: { artifact: "", node: nodeAddressOf(at), occurrence, quote },
      replyTo: null,
    };
  }
  const actor = at.author?.[0];
  if (actor === undefined || words.block === null) return null;
  return {
    anchor: undefined,
    replyTo: {
      actor,
      excerpt: "",
      id: at.id,
      parentKind: REPLY_PARENTS[write.request],
      quoted: { block: words.block, occurrence, quote },
    },
  };
}
/** The store address `POST /api/v1/issues/{key}/comments`'s `anchor.node` takes: a finding's read
 *  id (`lens/<uuid>#<block>`) and an already-addressed id stay as given; a bare id of a lens,
 *  risk, constraint or result takes `${kind}/${id}` (`api/comment_create.go`'s `anchoredNode`). */
function nodeAddressOf(at: Pick<TextWriteTarget, "id" | "kind">): string {
  if (at.id.includes("/") || at.id.includes("#")) return at.id;
  if (at.kind === undefined || at.kind === "" || at.kind === "block") return at.id;
  return `${at.kind}/${at.id}`;
}
