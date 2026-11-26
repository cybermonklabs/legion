import type * as ProseMirrorState from "@milkdown/kit/prose/state";
import { useQueryClient } from "@tanstack/react-query";
import { type RefObject, useCallback, useEffect, useMemo, useRef } from "react";

import type { Actor } from "../../../api/types";
import { focusOnDocument } from "../../shell/roving";
import type { TreeNode } from "../issue-tree";
import { loadEditsHold } from "../node-caret";
import { rowSelectorOf } from "../tree-focus";
import { type DrawnRow, placesAlong } from "../tree-fold";
import { rowKeyOf } from "../tree-keys";
import { outlineQuery, type TreeAnswer } from "../tree-read/tree-read";
import type { EditedRow, MoveKeyBridge, NodeEditing } from "../useNodeEditing";
import { moveToRow } from "../useTreeKeymap";
import {
  drawNewRow,
  dropNewRow,
  heldWithNewRows,
  type NewRow,
  nameNewRow,
  newRowLine,
  newRowsOf,
  newRowUnplaced,
  subscribeRefusedPlacings,
  typeNewRow,
} from "./new-rows";
import type { NewEntry } from "./node-writes";
import type { NewPlace, PlaceLine } from "./placement";
import { writeNewBlock } from "./room-writes";
import type { RowEditor, RowReveal } from "./row-edits";
import { ownPathTo } from "./row-reveal";

/**
 * How Graph's outline makes and edits a new row (INFRA-5705's R3), with the tree's own editing
 * (`useNodeEditing.ts`) and the issue's one room: the room editor the node menu and the editor's
 * keys are given (`row-edits.tsx`' `RowEditor`), and the editing's way of putting the caret in a
 * row whose block the room does not hold yet (`MoveKeyBridge`'s `fresh` and `write`).
 *
 * A pick of New row below or New row under, or Enter at the end of a block R2 admits, draws a new
 * row at the place `placement.ts` gave it (`new-rows.ts`' `withNewRows`, which `IssueTree.tsx`
 * draws the tree through), with a block id the page chooses. In the render that draws it, the
 * row takes the focus and its editing begins, as Enter's does, in the tree artifact - the issue's
 * own fragment, whose editor the page draws for it (`room-target.ts`), one read and one socket
 * where none is open - with no row's caret needed first: an ask, a comment or a message takes New
 * row under from its row alone. While the room holds no block of the row's id, the editor's box
 * stays shut and its editable holds the focus, and no edit goes in (`holdEditsToBlock`); the
 * reader's first character is written as the block, one paragraph at the end of the fragment
 * (`room-writes.ts`), which the editor's hold then holds them to, so the next letter follows it.
 * A row left before its first character - Escape, a press elsewhere, the focus going to another
 * row - goes, with nothing written, and the focus it held goes back to the row it was made from.
 *
 * A placing `PATCH` that is not taken leaves the node where the server keeps it: the focus the
 * reader held on the row goes with it (`RowReveal.kept`), and where the row keeps its key but is
 * drawn elsewhere among its siblings, the focus the move dropped comes back to it.
 */

/** One new row this outline made: the key of the row the focus was on as it was made, and how
 *  far its editing has come - asked for once the row is drawn (`starting`), asked for (`asked`,
 *  with the folded branches above it opened once where they kept it from being drawn), then
 *  under way in its row (`edited`). */
interface Made {
  readonly origin: string | undefined;
  phase: "asked" | "edited" | "starting";
  key?: string;
  unfolded?: true;
}

/** The ProseMirror state module the editor's hold is built of (`loadEditsHold`), once loaded: the
 *  first character's write is made with it, in the key's own task. */
let proseState: typeof ProseMirrorState | undefined;

/** The drawn row of the new row `blockId`: its own, never a second row under another node. */
function drawnRowOf(
  rows: readonly DrawnRow[],
  blockId: string
): Extract<DrawnRow, { kind: "node" }> | undefined {
  return rows.find(
    (row): row is Extract<DrawnRow, { kind: "node" }> =>
      row.kind === "node" && row.node.id === blockId && !row.node.transcluded
  );
}

/** The path from `node` down to the own row of the node the menu and the keys name by `readId`,
 *  its placement that is no transclusion (part T's second rows): the node the read gives that id,
 *  or a new row named by its block id while the read holds none of it (`RowEditor.readIdOf`).
 *  Undefined where the tree under `node` draws none. */
function ownPathOfRead(node: TreeNode, readId: string): TreeNode[] | undefined {
  if ((node.readId ?? node.id) === readId) return [node];
  for (const child of node.children) {
    if (child.transcluded) continue;
    const below = ownPathOfRead(child, readId);
    if (below !== undefined) return [node, ...below];
  }
  return undefined;
}

/**
 * Graph's new rows for the outline of `issue` (Graph's alone: undefined anywhere else, where
 * nothing is set): sets `bridge`'s `fresh` and `write`, starts the editing of each row the menu
 * or a key made once it is drawn, and drops each such row left before its first character.
 * Answers the room editor the node menu and the editor's keys take, over `newRows`, the issue's
 * new rows as the tree drew them (`useNewRows`).
 */
export function useNewRowEditing({
  body,
  bridge,
  closed,
  edit,
  editing,
  issue,
  newRows,
  reader,
  reveal,
  root,
  rows,
  unfoldPath,
}: {
  /** The tree's own box, which the rows are found inside. */
  readonly body: RefObject<HTMLElement | null>;
  readonly bridge: MoveKeyBridge;
  /** The places of the rows the outline draws folded (`tree-fold.ts`' `OpenedBranches`). */
  readonly closed: ReadonlySet<string>;
  readonly edit: NodeEditing["edit"];
  readonly editing: EditedRow | undefined;
  readonly issue: string | undefined;
  readonly newRows: readonly NewRow[];
  /** The reader, the author of each row they make. */
  readonly reader: string | undefined;
  readonly reveal: RowReveal | undefined;
  /** The tree the outline draws, its new rows among it. */
  readonly root: TreeNode;
  readonly rows: readonly DrawnRow[];
  readonly unfoldPath: (above: readonly string[]) => void;
}): RowEditor | undefined {
  const client = useQueryClient();
  const made = useRef(new Map<string, Made>());
  // The key of the row a refused placing left the focus on, for the render that draws its node
  // where the server keeps it.
  const kept = useRef<string | undefined>(undefined);

  bridge.fresh =
    issue === undefined
      ? undefined
      : (row) => newRowsOf(client, issue).some((each) => each.rowKey === row);
  bridge.write =
    issue === undefined
      ? undefined
      : (row, words, view) => {
          const at = newRowsOf(client, issue).find((each) => each.rowKey === row);
          // A first key that types no word - a space - writes no block, which would be empty.
          if (at?.stage !== "drawn" || words.trim() === "" || proseState === undefined) {
            return false;
          }
          if (!writeNewBlock(view, proseState, at.blockId, words)) return false;
          typeNewRow(client, issue, at.blockId, words);
          return true;
        };

  const newRow = useCallback(
    (node: string, entry: NewEntry, place: NewPlace, line: PlaceLine | undefined) => {
      if (issue === undefined) return;
      const held = client.getQueryData<TreeAnswer>(outlineQuery(issue, "graph").queryKey);
      const parent =
        entry === "new_under" ? node : held?.nodes?.find((each) => each.id === node)?.parent?.id;
      // The row is drawn under the row the tree draws its place's parent in, found from the own
      // row of the node it is made from, by the drawn id that the read's `parent` need not share.
      const path = ownPathOfRead(root, node);
      const under = (entry === "new_under" ? path : path?.slice(0, -1))?.at(-1)?.id;
      if (parent === undefined || under === undefined) return;
      const blockId = crypto.randomUUID();
      const from = document.activeElement?.closest<HTMLElement>("[data-node-row]") ?? null;
      const origin = body.current?.contains(from) === true ? from?.dataset.rowKey : undefined;
      made.current.set(blockId, { origin, phase: "starting" });
      void loadEditsHold().then(
        ([state]) => {
          proseState = state;
        },
        // The row's editing answers for a hold that did not load (`useNodeEditing.ts`).
        () => undefined
      );
      const author: Actor | undefined =
        reader === undefined ? undefined : { id: reader, kind: "user" };
      drawNewRow(client, issue, {
        ...(author === undefined ? {} : { author }),
        blockId,
        entry,
        from: node,
        ...(line === undefined ? {} : { line }),
        parent,
        place,
        under,
      });
    },
    [body, client, issue, reader, root]
  );

  // Each render: the focus a refused placing's move dropped comes back to its row, each new row
  // drawn takes the focus and its editing, and each left before its first character goes.
  useEffect(() => {
    if (issue === undefined) return;
    const box = body.current;
    const rowOf = (key: string | undefined) =>
      key === undefined ? null : (box?.querySelector<HTMLElement>(rowSelectorOf(key)) ?? null);
    const keep = rowOf(kept.current);
    kept.current = undefined;
    if (keep !== null && focusOnDocument()) moveToRow(keep);
    for (const [blockId, mine] of made.current) {
      const row = newRows.find((each) => each.blockId === blockId);
      if (row === undefined) {
        made.current.delete(blockId);
        continue;
      }
      const left = () => {
        const origin = rowOf(mine.origin);
        if (rowOf(mine.key)?.contains(document.activeElement) === true && origin !== null) {
          moveToRow(origin);
        }
        made.current.delete(blockId);
        dropNewRow(client, issue, blockId);
      };
      if (mine.phase === "starting") {
        const drawn = drawnRowOf(rows, blockId);
        const key = drawn === undefined ? undefined : rowKeyOf(drawn);
        const element = rowOf(key);
        if (drawn === undefined || key === undefined || element === null) {
          // A folded branch above the row's place is opened once; a place the outline draws
          // nowhere - outside the zoom - takes no row.
          const above = (ownPathTo(root, blockId) ?? []).slice(0, -1).map((each) => each.id);
          if (
            mine.unfolded === undefined &&
            placesAlong(above).some((place) => closed.has(place))
          ) {
            mine.unfolded = true;
            unfoldPath(above);
          } else {
            left();
          }
          continue;
        }
        mine.phase = "asked";
        mine.key = key;
        nameNewRow(client, issue, blockId, key);
        moveToRow(element);
        edit(key, drawn.node);
      } else if (editing?.row === mine.key) {
        mine.phase = "edited";
      } else if (row.stage === "drawn") {
        // Edited and left, or never edited - a row its editing refused.
        left();
      }
    }
  });

  // A placing `PATCH` that was not taken: the focus on the row goes with its node to where the
  // server keeps it (`RowReveal.kept`), or comes back to its row where a move drew it elsewhere
  // under the same key (above).
  useEffect(() => {
    if (issue === undefined) return;
    return subscribeRefusedPlacings(client, issue, (node, blockId) => {
      const focused = document.activeElement?.closest<HTMLElement>("[data-node-row]");
      if (focused?.dataset.nodeId !== blockId) return;
      kept.current = focused.dataset.rowKey;
      reveal?.kept(node);
    });
  }, [client, issue, reveal]);

  // The outline going takes each row it made and nobody has typed in with it.
  useEffect(() => {
    const mine = made.current;
    return () => {
      if (issue === undefined) return;
      for (const blockId of mine.keys()) {
        const row = newRowsOf(client, issue).find((each) => each.blockId === blockId);
        if (row?.stage === "drawn") dropNewRow(client, issue, blockId);
      }
    };
  }, [client, issue]);

  return useMemo(() => {
    if (issue === undefined) return undefined;
    const editor: RowEditor = {
      held: (answer) => heldWithNewRows(answer, newRows),
      lineOf: (node) => newRowLine(newRows, node.id),
      newRow,
      readIdOf: (id) => (newRows.some((row) => row.blockId === id) ? id : undefined),
      unplaced: (node) =>
        newRowUnplaced(
          newRows,
          client.getQueryData<TreeAnswer>(outlineQuery(issue, "graph").queryKey),
          node
        ),
    };
    return editor;
  }, [client, issue, newRow, newRows]);
}
