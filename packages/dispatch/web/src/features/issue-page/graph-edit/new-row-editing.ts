import type * as ProseMirrorState from "@milkdown/kit/prose/state";
import { useQueryClient } from "@tanstack/react-query";
import { type RefObject, useCallback, useEffect, useMemo, useRef } from "react";

import type { Actor, BlockSchema } from "../../../api/types";
import { editorBlockSchema } from "../../doc/schema";
import { importWhenOnline } from "../../shell/DeploymentResilience";
import { focusOnDocument } from "../../shell/roving";
import type { TreeNode } from "../issue-tree";
import { loadEditsHold } from "../node-caret";
import { rowSelectorOf } from "../tree-focus";
import { type DrawnRow, placesAlong } from "../tree-fold";
import { rowKeyOf } from "../tree-keys";
import type { TreeAnswer } from "../tree-read/tree-read";
import {
  abortOnReaderAct,
  type EditedRow,
  type MoveKeyBridge,
  type NodeEditing,
} from "../useNodeEditing";
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
import { heldRead, type NewEntry, readAfterRoomWrite } from "./node-writes";
import {
  type NewPlace,
  type PlaceLine,
  type RetypeAttributes,
  type RetypeKind,
  rowReadIds,
} from "./placement";
import { retypeKinds, writeNewBlock, writeRetype } from "./room-writes";
import type { RowEditor, RowReveal } from "./row-edits";
import { ownPathTo, ownPathWhere } from "./row-reveal";

/**
 * How Graph's outline makes and edits a new row (INFRA-5705's R3), and retypes a prose block (R5),
 * with the tree's own editing (`useNodeEditing.ts`) and the issue's one room: the room editor the
 * node menu and the editor's keys are given (`row-edits.tsx`' `RowEditor`), the editing's way of
 * putting the caret in a row whose block the room does not hold yet (`MoveKeyBridge`'s `fresh`
 * and `write`), and what it tells of a caret it has put in (`MoveKeyBridge.caretIn`).
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
 *
 * A pick of a kind in Retype's submenu (R5) - the kinds the editor's schema for the issue lets it
 * make, the schema the page gives that editor (`doc/schema.ts`' `editorBlockSchema`: a child that
 * is a view of its ancestor's spec makes no statement, decision 359) - asks the tree's own editing
 * for the caret in the node's own row, as Enter does (part Y's first Enter, one read and one
 * socket where none is open), and once the caret is in the node's block, retypes the block there
 * with the editor's typed-block commands, made for this page's meta key (`room-writes.ts`'
 * `writeRetype`). It sends no request of its own. Once the editor has taken it, the outline reads
 * are read again once (R7, `node-writes.ts`' `readAfterRoomWrite`), and the editing it opened
 * ends with the focus on the row, as Escape leaves it, which draws the new kind once a read
 * answers it. A reader who acts before the retype is written has moved on, and it is not written.
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

/** A retype the node menu asked for (R5), until the caret its editing asked for goes into the row
 *  of key `key`: the node's read id, the kind and the attributes `placement.ts` gave, the schema
 *  the page gives the editor, and what the reader's first act since aborts. */
interface Retyping {
  readonly acts: AbortController;
  readonly attributes: RetypeAttributes;
  readonly key: string;
  readonly kind: RetypeKind;
  readonly node: string;
  readonly schema: BlockSchema;
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

/** `place` as the outline draws it among the children of the row `under`: right after the row
 *  that its `after`, a node of `held`, is drawn in, named by the read id that row carries. The
 *  outline draws an ask and the block it was asked on in one row, read as the first of the two
 *  the read's walk reaches, so the block the place names can be drawn in a row read as the ask
 *  (`placement.ts`' `rowReadIds`); a row read as the node itself is the one it is drawn after. */
function drawnPlace(place: NewPlace, under: TreeNode, held: TreeAnswer | undefined): NewPlace {
  if (place.after === null || held === undefined) return place;
  for (const after of rowReadIds(held, place.after)) {
    const drawn = under.children.some(
      (child) => !child.transcluded && (child.readId ?? child.id) === after
    );
    if (drawn) return after === place.after ? place : { ...place, after };
  }
  return place;
}

/**
 * Graph's new rows and retypes for the outline of `issue` (Graph's alone: undefined anywhere else,
 * where nothing is set): sets `bridge`'s `fresh`, `write` and `caretIn`, starts the editing of each
 * row the menu or a key made once it is drawn, and drops each such row left before its first
 * character. Answers the room editor the node menu and the editor's keys take, over `newRows`, the
 * issue's new rows as the tree drew them (`useNewRows`).
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
  stopEditing,
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
  readonly stopEditing: NodeEditing["stopEditing"];
  readonly unfoldPath: (above: readonly string[]) => void;
}): RowEditor | undefined {
  const client = useQueryClient();
  const made = useRef(new Map<string, Made>());
  // The key of the row a refused placing left the focus on, for the render that draws its node
  // where the server keeps it.
  const kept = useRef<string | undefined>(undefined);
  // The retype waiting for its caret, and what it reads as the editing goes on: the rows drawn,
  // the tree's own editing and the row it edits.
  const retyping = useRef<Retyping | undefined>(undefined);
  const latest = useRef({ edit, editing, rows, stopEditing });
  latest.current = { edit, editing, rows, stopEditing };
  // The schema the page gives the editor of the issue's documents, from what Graph's read
  // answers as `view_of` (decision 359).
  const schema = editorBlockSchema(
    issue === undefined ? undefined : heldRead(client, issue)?.view_of
  );

  // A placed node: its row leaves the new rows once the PATCH is taken, and the outline draws the
  // node where the answer put it in the held read. Where that is under another key than the row
  // edited - a place the answer gives other than the one the row stood at - the editing follows
  // the node's own row there in this render, with the document's one editor and the caret where
  // the reader had it, as after a taken move (`move-keys.ts`), before the tree's drop rule reads
  // the edited row as gone and ends the editing (`IssueTree.tsx`). A refused placing lets its row
  // go first (below).
  if (issue !== undefined && editing !== undefined) {
    for (const [blockId, mine] of made.current) {
      if (mine.phase !== "edited" || mine.key !== editing.row) continue;
      if (newRows.some((row) => row.blockId === blockId)) continue;
      if (rows.some((row) => row.kind === "node" && rowKeyOf(row) === mine.key)) continue;
      const drawn = drawnRowOf(rows, blockId);
      if (drawn === undefined) continue;
      const from = mine.key;
      mine.key = rowKeyOf(drawn);
      bridge.follow?.(from, mine.key);
    }
  }

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
  bridge.caretIn =
    issue === undefined
      ? undefined
      : (row, blockId, view) => {
          const asked = retyping.current;
          if (asked?.key !== row) return;
          retyping.current = undefined;
          // The editor library's typed-block commands sit behind the editor's own `import()`
          // (`doc/editor.ts`), loaded already where a row's editor has been made of it.
          const commands = importWhenOnline(() => import("@legion/proof-editor"));
          void Promise.all([loadEditsHold(), commands]).then(
            ([[state], { createTypedBlockCommands: create }]) => {
              // A reader who acted since the retype was asked has moved on from it.
              const movedOn = asked.acts.signal.aborted;
              asked.acts.abort();
              if (movedOn) return;
              const { attributes, kind, node, schema: given } = asked;
              if (writeRetype(view, state, create, given, blockId, kind, attributes)) {
                readAfterRoomWrite(client, issue, node);
              }
              // The editing the retype opened ends with the focus on the row, as Escape leaves it.
              if (latest.current.editing?.row !== row) return;
              const drawn = body.current?.querySelector<HTMLElement>(rowSelectorOf(row));
              if (drawn?.contains(document.activeElement) === true) {
                (document.activeElement as HTMLElement | null)?.blur();
                drawn.focus();
              }
              latest.current.stopEditing();
            },
            () => asked.acts.abort()
          );
        };

  const retype = useCallback(
    (node: string, kind: RetypeKind, attributes: RetypeAttributes) => {
      // The node's own row, never a second row under another node: by the read id the read gives
      // it, or by the block id of a new row the read holds none of (`RowEditor.readIdOf`).
      const row = latest.current.rows.find(
        (each): each is Extract<DrawnRow, { kind: "node" }> =>
          each.kind === "node" &&
          !each.node.transcluded &&
          (each.node.readId ?? each.node.id) === node
      );
      if (row === undefined) return;
      const key = rowKeyOf(row);
      retyping.current?.acts.abort();
      retyping.current = { acts: abortOnReaderAct(), attributes, key, kind, node, schema };
      latest.current.edit(key, row.node);
    },
    [schema]
  );

  const newRow = useCallback(
    (node: string, entry: NewEntry, place: NewPlace, line: PlaceLine | undefined) => {
      if (issue === undefined) return;
      const held = heldRead(client, issue);
      const parent =
        entry === "new_under" ? node : held?.nodes?.find((each) => each.id === node)?.parent?.id;
      // The row is drawn under the row the tree draws its place's parent in, found from the own
      // row of the node it is made from, by the drawn id that the read's `parent` need not share:
      // the node the read gives that read id, or a new row named by its block id while the read
      // holds none of it (`RowEditor.readIdOf`); and right after the row its place names there.
      const path = ownPathWhere(root, (each) => (each.readId ?? each.id) === node);
      const under = (entry === "new_under" ? path : path?.slice(0, -1))?.at(-1);
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
        place: drawnPlace(place, under, held),
        under: under.id,
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

  // A placing `PATCH` that was not taken: the row is no longer this outline's to follow (above),
  // and the focus on it goes with its node to where the server keeps it (`RowReveal.kept`), or
  // comes back to its row where a move drew it elsewhere under the same key (above).
  useEffect(() => {
    if (issue === undefined) return;
    return subscribeRefusedPlacings(client, issue, (node, blockId) => {
      made.current.delete(blockId);
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
  // The outline going takes with it a retype still waiting for its caret.
  useEffect(() => () => retyping.current?.acts.abort(), []);

  return useMemo(() => {
    if (issue === undefined) return undefined;
    // Graph's held read with the new rows, made once for each answer while the rows stand: every
    // row of the outline reads it on each draw (`RowMenu.tsx`' `RowMenuPlace`).
    const views = new WeakMap<TreeAnswer, TreeAnswer>();
    const editor: RowEditor = {
      held: (answer) => {
        let view = views.get(answer);
        if (view === undefined) {
          view = heldWithNewRows(answer, newRows);
          views.set(answer, view);
        }
        return view;
      },
      kinds: retypeKinds(schema),
      lineOf: (node) => newRowLine(newRows, node.id),
      newRow,
      readIdOf: (id) => (newRows.some((row) => row.blockId === id) ? id : undefined),
      retype,
      unplaced: (node) => newRowUnplaced(newRows, heldRead(client, issue), node),
    };
    return editor;
  }, [client, issue, newRow, newRows, retype, schema]);
}
