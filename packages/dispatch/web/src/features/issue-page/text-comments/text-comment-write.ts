import type { GateName } from "./gates";

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

/**
 * Each node kind's write (`NodeType` of `issue-tree.ts` and each kind of 1253 R30), or no control
 * with its reason. A kind with no row is a defect (`kinds.test.ts`). Built by t8; it holds no row
 * until then.
 */
export const WRITES: Readonly<Record<string, KindWrites>> = {};

/** The node a control is asked for: its kind, the place it stands in, its visibility, and the
 *  issue it belongs to (the page's own, or another issue's where the view draws a node of it). */
export interface WriteSubject {
  readonly issue: string;
  readonly kind: string;
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
 * given), or null: no control on a node whose visibility is `author`, on a child issue, or on a
 * kind whose gate is off. Built by t8; it answers null until then.
 */
export function writeFor(
  _subject: WriteSubject,
  _gates?: Readonly<Record<GateName, boolean>>
): LiveWrite | null {
  return null;
}
