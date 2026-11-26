import { useQueryClient } from "@tanstack/react-query";
import { useEffect, useRef, useState } from "react";

import { focusOnDocument } from "../../shell/roving";
import type { TreeNode } from "../issue-tree";
import { useLensAct } from "../lenses/LensMenu";
import type { EndKey, MoveKey } from "../node-caret";
import { type DrawnRow, placesAlong } from "../tree-fold";
import { rowKeyOf } from "../tree-keys";
import { outlineQuery, readNextTreePage, type TreeAnswer } from "../tree-read/tree-read";
import type { EditedRow, MoveKeyBridge } from "../useNodeEditing";
import { useNodeWrites } from "./node-writes";
import {
  type GreyReason,
  type Placement,
  type PlacementRow,
  placementOf,
  type Verdict,
} from "./placement";
import type { RowEditor } from "./row-edits";
import { ownPathTo } from "./row-reveal";

/**
 * Graph's move keys in a row's editor (INFRA-5705's R4): with the caret in the text of a block R2
 * admits, Tab indents the node, Shift+Tab outdents it, Alt+Shift+Up moves it up and Alt+Shift+Down
 * moves it down, in a list item of the block too. The row's editor hands each such key here before
 * the editor or the browser acts on it (`node-caret.ts`' `confineKey`, through `useNodeEditing.ts`'
 * `MoveKeyBridge`); Conversation and every filter give it nowhere, and keep today's keys.
 *
 * A key reads the node menu's own verdict for the node, from Graph's held read as it stands, on the
 * row the caret is in (`placement.ts`' `placementOf`, as `RowMenu.tsx` reads it): a move goes into
 * the node's queue (`node-writes.ts`' `move`), which makes its body again as its turn comes, as
 * the menu's entries do; at a grey edge the key writes nothing, the caret stays, and the row draws
 * the entry's reason (`NodeWrites.grey`). A block R2 does not admit - a kind it does not edit, a node
 * of another issue, a closed issue - takes none of the keys, which do there what they do today.
 *
 * A taken move draws the node under its new parent, which is a row of another key (`rowKeyOf`): the
 * editing follows the node there in the render that draws it (`MoveKeyBridge.follow`), with the
 * document's one editor and the caret where the reader had it, so the reader goes on typing; a
 * folded branch above that row - a folded new parent - is unfolded first, in the same render, so
 * no render draws the tree with the edited row gone, which would end the editing
 * (`IssueTree.tsx`).
 *
 * Two keys of a new row (R3) go the same way, with the caret at the end of the block's text:
 * Enter makes a new row below, as the menu's New row below does, through the room editor the
 * outline gives (`RowEditor.newRow`), and is grey where that entry is, the row showing why - on a
 * block of the Verification document or the plan, and on a new row not placed yet; and Shift+Enter
 * in a typed block makes a paragraph inside it, which Enter does there today. Enter elsewhere in
 * the text, and both keys on a block R2 does not admit, do what they do today. No word of this
 * file is a literal: the reasons are `ANSWER_LINES`'.
 */

/** The reasons R2 greys every editing entry of a block it does not admit, where the move keys are
 *  no keys of this part's and keep today's behaviour (R10): every other grey reason is an edge of
 *  a block it admits, where a key writes nothing and the row shows why (R4). */
const NOT_ADMITTED: Readonly<Partial<Record<GreyReason, true>>> = {
  grey_closed: true,
  grey_kind: true,
  grey_own_issue: true,
};

/** The supertags of R2's typed blocks - a criterion, a statement, a check, a plan task - inside
 *  which Shift+Enter at the end makes a paragraph (R3). */
const TYPED: Readonly<Record<string, true>> = {
  check: true,
  criterion: true,
  plan_task: true,
  requirement: true,
};

/** One row of the outline that stands for a node. */
type NodeDrawnRow = Extract<DrawnRow, { kind: "node" }>;

/** The issue's status as the verdicts read it: `"done"` closes it (`row-edits.tsx`). */
const CLOSED_STATUS = "done";
const OPEN_STATUS = "open";

/** The bridge between a tree's row editing and its move keys, one for the tree's life. */
export function useMoveKeyBridge(): MoveKeyBridge {
  return useState<MoveKeyBridge>(() => ({}))[0];
}

/**
 * Graph's move keys for the rows `rows` of the tree `root` (`IssueTree.tsx`): sets the bridge's
 * `take` in Graph alone, and follows a node a move key moved to its new row. `closed` is the set
 * of places of the rows the outline draws folded (`tree-fold.ts`' `OpenedBranches`), and
 * `unfoldPath` how it unfolds a path; `zoomRoot` is the id of the node the outline is zoomed to
 * (`TreeNode.id`); `editor` is the room editor the outline gives its rows (`RowEditor`), which
 * Enter's new row is made through.
 */
export function useMoveKeys({
  bridge,
  closed,
  editing,
  editor,
  isClosed,
  root,
  rows,
  unfoldPath,
  zoomRoot,
}: {
  readonly bridge: MoveKeyBridge;
  readonly closed: ReadonlySet<string>;
  readonly editing: EditedRow | undefined;
  readonly editor: RowEditor | undefined;
  readonly isClosed: boolean;
  readonly root: TreeNode;
  readonly rows: readonly DrawnRow[];
  readonly unfoldPath: (above: readonly string[]) => void;
  readonly zoomRoot: string | undefined;
}): void {
  const act = useLensAct();
  const key = act?.outline === "graph" ? act.issueKey : undefined;
  const client = useQueryClient();
  const readIds = new Map<string, string>();
  for (const row of rows) {
    if (row.kind === "node" && row.node.readId !== undefined) {
      readIds.set(row.node.id, row.node.readId);
    }
  }
  const zoomRead = zoomRoot === undefined ? undefined : readIds.get(zoomRoot);
  const status = isClosed ? CLOSED_STATUS : OPEN_STATUS;
  const writes = useNodeWrites({
    key: key ?? "",
    status,
    ...(zoomRead === undefined ? {} : { zoomRoot: zoomRead }),
  });
  // The node a move key of the edited row went for (`TreeNode.id`), and the row it is edited in
  // now, while that editing lasts.
  const moving = useRef<{ readonly id: string; row: string } | undefined>(undefined);
  // Whether the next commit is one a move draws: a row a taken move moved in the page, or one the
  // editing followed, which drops the focus as the editor's box leaves the page (`restore`).
  const restoreNext = useRef(false);
  const drawn = (rowKey: string) =>
    rows.some((row) => row.kind === "node" && rowKeyOf(row) === rowKey);

  bridge.take =
    key === undefined
      ? undefined
      : (rowKey: string, move: MoveKey | EndKey): boolean => {
          const row = rows.find(
            (each): each is NodeDrawnRow => each.kind === "node" && rowKeyOf(each) === rowKey
          );
          // A new row the read does not hold yet is named by the block id the page chose for it.
          const readId =
            row === undefined ? undefined : (row.node.readId ?? editor?.readIdOf?.(row.node.id));
          if (row === undefined || readId === undefined) return false;
          // A second row of a node stands under a node it serves, which the verdicts name by its
          // read id, as the menu's row does (`row-edits.tsx`).
          const above = row.trail.at(-1);
          const from: PlacementRow =
            !row.node.transcluded || above === undefined
              ? "own"
              : { under: readIds.get(above) ?? above };
          // The verdicts from Graph's held read as it stands, with the new rows it does not hold
          // yet (`RowEditor.held`), or undefined where it does not hold the node.
          const verdictsNow = (): { held: TreeAnswer; verdicts: Placement } | undefined => {
            const read = client.getQueryData<TreeAnswer>(outlineQuery(key, "graph").queryKey);
            const held = read === undefined ? undefined : (editor?.held?.(read) ?? read);
            if (held?.converted !== true) return undefined;
            if (!(held.nodes ?? []).some((node) => node.id === readId)) return undefined;
            const verdicts = placementOf({
              held,
              node: readId,
              row: from,
              status,
              unplaced: editor?.unplaced?.(readId) === true ? true : undefined,
              zoomRoot: zoomRead,
            });
            return { held, verdicts };
          };
          const now = verdictsNow();
          if (now === undefined) return false;
          if (move === "paragraph") {
            const below = now.verdicts.new_below;
            const supertag = now.held.nodes?.find((node) => node.id === readId)?.supertag ?? "";
            return (
              TYPED[supertag] === true &&
              !(below.kind === "grey" && NOT_ADMITTED[below.reason] === true)
            );
          }
          const verdict = now.verdicts[move];
          if (verdict.kind === "grey") {
            if (NOT_ADMITTED[verdict.reason] === true) return false;
            writes.grey(readId, verdict.reason);
            return true;
          }
          if (move === "new_below") {
            if (editor === undefined) return false;
            // A place on a page the page has not read is asked again once that page is read, as
            // the menu's entry does.
            void (async () => {
              let next: Verdict | undefined = verdict;
              while (next?.kind === "read_next") {
                await readNextTreePage(client, key, "graph");
                next = verdictsNow()?.verdicts.new_below;
              }
              if (next?.kind === "new") editor.newRow(readId, "new_below", next.place, next.line);
            })().catch(() => undefined);
            return true;
          }
          moving.current = { id: row.node.id, row: rowKey };
          void writes.move(readId, move, from).then((outcome) => {
            if (outcome.status === "taken") restoreNext.current = true;
          });
          return true;
        };

  // The editing follows the node a move key moved, in this render: where the edited row is no
  // longer drawn and the node's own row is, under another key, the editing goes there; where a
  // folded branch above that row keeps it from being drawn, the branch is unfolded, and the render
  // that draws it follows. Each update is made only where it changes what is drawn, so the render
  // it asks for asks for none.
  if (editing === undefined) moving.current = undefined;
  const held = moving.current;
  if (key !== undefined && held !== undefined && editing?.row === held.row && !drawn(held.row)) {
    const path = ownPathTo(root, held.id);
    const node = path?.at(-1);
    if (path !== undefined && node !== undefined) {
      const trail = path.slice(0, -1).map((above) => above.id);
      const to = rowKeyOf({ kind: "node", level: path.length, node, trail });
      if (to !== held.row && drawn(to)) {
        const from = held.row;
        held.row = to;
        restoreNext.current = true;
        bridge.follow?.(from, to);
      } else if (placesAlong(trail).some((place) => closed.has(place))) {
        unfoldPath(trail);
      }
    }
  }

  // The commit a move draws takes the editor's box out of the page and puts it back - in the row
  // the editing followed, or in its own row moved among its siblings - which drops the focus to the
  // page. It comes back in this commit's effects, with the caret where the reader had it: after the
  // row's editing listens on the row it follows (`useNodeEditing.ts`, whose effects the tree runs
  // first), so the focus coming into that row ends nothing, and before the tree's drop rule, which
  // would read a focus fallen to nothing as the reader's row gone (`IssueTree.tsx`).
  useEffect(() => {
    if (!restoreNext.current) return;
    restoreNext.current = false;
    if (focusOnDocument()) bridge.restore?.();
  });
}
