import { createContext, type ReactNode, useCallback, useMemo, useRef, useState } from "react";
import { createPortal, flushSync } from "react-dom";

import { ConfirmDialog } from "../../project/ConfirmDialog";
import type { LensAct } from "../lenses/LensMenu";
import { ANSWER_LINES, CONTROL_LABELS } from "../page-words";
import type { DrawnRow } from "../tree-fold";
import { type NodeWritesIssue, useNodeWrites } from "./node-writes";
import type { NewPlace, PlaceLine, PlacementRow, RetypeAttributes, RetypeKind } from "./placement";

/**
 * What the rows of Graph's outline share for the node menu of the focused row
 * (`graph-edit/RowMenu.tsx`, INFRA-5705's R1 to R8): the issue the writes are for, the delete's
 * confirm, which outlives the menu and the row's focus, and the outline's way of taking the reader
 * to a row. `TreeRows.tsx` makes them (`useRowEdits`) in Graph alone, where the Lens act leaves
 * the row for the menu; Conversation and every filter draw their rows as today, with no menu.
 */

/** The reader taken to a row of the outline (`IssueTree.tsx`'s, `useRowReveal`). */
export interface RowReveal {
  /** Once the outline draws the node `id` (`TreeNode.id`) under the node of read id `parent`,
   *  where a taken move put it, unfolds every branch above that node's own row, a folded new
   *  parent among them, and gives the row the focus; until then, and unless the reader acts
   *  first, it waits for the tree that draws it (R4). */
  readonly moved: (id: string, parent: string) => void;
  /** Takes the reader to the row of the node of read id `node` wherever it stands, as the
   *  outline's keys do: the row a refusal's line names (R8). */
  readonly toRow: (node: string) => void;
}

/**
 * What the issue's room editor gives the node menu (stage B: INFRA-5705's R3 and R5, through part
 * Y's room): the entries that write in the room, and what the menu's verdicts read of it. Where
 * none is given (`RowEditorContext`), New row below, New row under and Retype are grey on a row
 * that takes them, with `ANSWER_LINES.grey_no_editor`, and the menu writes nothing for them.
 */
export interface RowEditor {
  /** The kinds the editor's schema for the node's fragment lets it make (R5, `placement.ts`'
   *  `kinds`); each of the four where it gives none. */
  readonly kinds?: readonly RetypeKind[];
  /** Whether the node of read id `node` is a new row the reader typed in that no `PATCH` has
   *  placed yet (R3, `placement.ts`' `unplaced`). */
  readonly unplaced?: (node: string) => boolean;
  /** Draws a new row at `place`, from the row of read id `node`, with the caret in it (R3), and
   *  the line `line` where the new row stands after the spec's blocks. */
  readonly newRow: (node: string, place: NewPlace, line: PlaceLine | undefined) => void;
  /** Gives the block of read id `node` the kind `kind`, with the attributes that keep it where it
   *  stands (R5). */
  readonly retype: (node: string, kind: RetypeKind, attributes: RetypeAttributes) => void;
}

/** The room editor the page gives the node menu, where it gives one (`RowEditor`). */
export const RowEditorContext = createContext<RowEditor | undefined>(undefined);

/** What every row of Graph's outline shares for its menu. */
export interface OutlineEdits {
  /** The issue the writes are for, as `useNodeWrites` takes it. */
  readonly issue: NodeWritesIssue;
  /** Opens the delete's confirm for the node of read id `node`, naming `count` (R6). */
  readonly confirmDelete: (node: string, count: number) => void;
  /** The outline's way of taking the reader to a row; undefined for rows drawn with no outline
   *  around them. */
  readonly reveal: RowReveal | undefined;
}

/** The node menu of one row of Graph: what its outline shares, and which of the node's rows it
 *  is (`placement.ts`' `PlacementRow`): its own, or a second row under a node it serves. */
export interface RowEdit {
  readonly outline: OutlineEdits;
  readonly row: PlacementRow;
}

/** One row of the outline that stands for a node. */
type NodeDrawnRow = Extract<DrawnRow, { kind: "node" }>;

/** The issue's status as the menu's verdicts read it (`placement.ts`: `"done"` closes it), from
 *  the outline's own rule for a closed issue (`isClosed`, its `closed_at`): the writes a row
 *  offers are refused on a closed issue and on no other, so any other status reads as open. */
const CLOSED_STATUS = "done";
const OPEN_STATUS = "open";

/** A delete the reader picked, which its confirm names: the node's read id and the count the dry
 *  run answered. Each pick is a new one, and its confirm is drawn anew. */
interface DeleteAsk {
  readonly id: number;
  readonly node: string;
  readonly count: number;
}

/**
 * The node menus of the rows `TreeRows.tsx` draws (`act`, its `useLensAct`, says where): in Graph,
 * each row's `RowEdit`, and the delete's confirm while one is open; anywhere else, no row's and
 * none. `zoomRoot` is the id of the node the outline is zoomed to (`TreeNode.id`), whose read id
 * the verdicts read; `reveal` is the outline's (`IssueTree.tsx`).
 */
export function useRowEdits({
  act,
  isClosed,
  reveal,
  rows,
  zoomRoot,
}: {
  readonly act: LensAct | undefined;
  readonly isClosed: boolean;
  readonly reveal: RowReveal | undefined;
  readonly rows: readonly DrawnRow[];
  readonly zoomRoot: string | undefined;
}): { readonly confirm: ReactNode; readonly editOf: (row: NodeDrawnRow) => RowEdit | undefined } {
  const key = act?.outline === "graph" ? act.issueKey : undefined;
  const [asked, setAsked] = useState<DeleteAsk | undefined>(undefined);
  const asks = useRef(0);
  const confirmDelete = useCallback((node: string, count: number) => {
    asks.current += 1;
    setAsked({ count, id: asks.current, node });
  }, []);
  const done = useCallback((ask: DeleteAsk) => {
    setAsked((held) => (held === ask ? undefined : held));
  }, []);
  // The read id of each node the rows draw, by its `TreeNode.id`: the node a second row stands
  // under, and the node the outline is zoomed to, are named by it. Read in Graph alone.
  const readIds = new Map<string, string>();
  if (key !== undefined) {
    for (const row of rows) {
      if (row.kind === "node" && row.node.readId !== undefined) {
        readIds.set(row.node.id, row.node.readId);
      }
    }
  }
  const zoomRead = zoomRoot === undefined ? undefined : readIds.get(zoomRoot);
  const status = isClosed ? CLOSED_STATUS : OPEN_STATUS;
  const outline = useMemo(
    () =>
      key === undefined
        ? undefined
        : {
            confirmDelete,
            issue: { key, status, ...(zoomRead === undefined ? {} : { zoomRoot: zoomRead }) },
            reveal,
          },
    [confirmDelete, key, reveal, status, zoomRead]
  );
  const own = useMemo(
    () => (outline === undefined ? undefined : { outline, row: "own" as const }),
    [outline]
  );
  const editOf = (row: NodeDrawnRow): RowEdit | undefined => {
    if (outline === undefined) return undefined;
    // A second row of a node stands under a node it serves (part T), which the verdicts name by
    // its read id; a node drawn once is drawn on its own row.
    const above = row.trail.at(-1);
    if (!row.node.transcluded || above === undefined) return own;
    return { outline, row: { under: readIds.get(above) ?? above } };
  };
  const confirm =
    outline === undefined || asked === undefined ? null : (
      <DeleteConfirm ask={asked} issue={outline.issue} key={asked.id} onDone={done} />
    );
  return { confirm, editOf };
}

/**
 * The delete's confirm (R6): `features/project/ConfirmDialog.tsx`, over the page, never
 * `window.confirm`, titled `ANSWER_LINES.delete_confirm_title` and naming how many rows the delete
 * takes. Cancel, Escape and a press beside it close it and send nothing. Its confirm sends the
 * counted delete (`NodeWrites.remove`), once the node's moves before it have ended, and the
 * confirm stays open while it is out, taking no second confirm. Where the server answers that the
 * count moved (`409 DELETE_COUNT_MOVED`), the same confirm names the new count and waits for the
 * reader to confirm that one or cancel; any other answer closes it, a refusal's line drawn on the
 * row (`NodeWrites.lines`).
 *
 * It is closed in the task of its answer (`flushSync`), so the focus it took goes back to the row
 * at once (`useDialog`), before a read of the tree the taken delete starts can draw the tree
 * without that row: the row then leaves the tree with the focus, and the outline's drop rule gives
 * the focus to the nearest row (`IssueTree.tsx`, `moveToRow`). Closed in a later task, it could
 * give the focus back to a row a read had already taken away, which leaves it on the page's body.
 */
function DeleteConfirm({
  ask,
  issue,
  onDone,
}: {
  ask: DeleteAsk;
  issue: NodeWritesIssue;
  onDone: (ask: DeleteAsk) => void;
}): ReactNode {
  const writes = useNodeWrites(issue);
  const [count, setCount] = useState(ask.count);
  const sending = useRef(false);
  const close = () => onDone(ask);
  const confirm = () => {
    if (sending.current) return;
    sending.current = true;
    void writes.remove(ask.node, count).then((outcome) => {
      sending.current = false;
      if (outcome.status === "count_moved") {
        setCount(outcome.count);
        return;
      }
      flushSync(close);
    });
  };
  const unit = count === 1 ? CONTROL_LABELS.delete_row : CONTROL_LABELS.delete_rows;
  // The confirm stands over the page, but in the outline's React tree, where a press on it would
  // bubble to the outline's own press handling (`usePressFocus`) and put off the focus the confirm
  // gives back to the row as it closes: its presses stop at its root.
  return createPortal(
    // biome-ignore lint/a11y/noStaticElementInteractions: nothing is made interactive here - the handler only keeps the dialog's own presses out of the outline's.
    <div onMouseDown={(event) => event.stopPropagation()}>
      <ConfirmDialog
        body={`${ANSWER_LINES.confirm_takes} ${count.toLocaleString()} ${unit}`}
        confirmLabel={CONTROL_LABELS.confirm_delete}
        onCancel={close}
        onConfirm={confirm}
        title={ANSWER_LINES.delete_confirm_title}
      />
    </div>,
    document.body
  );
}
