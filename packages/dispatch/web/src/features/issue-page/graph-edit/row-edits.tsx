import type { QueryClient } from "@tanstack/react-query";
import { type ReactNode, useCallback, useLayoutEffect, useMemo, useRef, useState } from "react";
import { createPortal, flushSync } from "react-dom";

import { ConfirmDialog } from "../../project/ConfirmDialog";
import type { TreeNode } from "../issue-tree";
import type { LensAct } from "../lenses/LensMenu";
import { ANSWER_LINES, CONTROL_LABELS } from "../page-words";
import type { DrawnRow } from "../tree-fold";
import { readNextTreePage, type TreeAnswer } from "../tree-read/tree-read";
import { READER_ACTS } from "../useNodeEditing";
import { heldRead, type NewEntry, type NodeWritesIssue, useNodeActs } from "./node-writes";
import {
  type NewPlace,
  type PlaceLine,
  type Placement,
  type PlacementRow,
  placementOf,
  type RetypeAttributes,
  type RetypeKind,
  type Verdict,
} from "./placement";

/**
 * What the rows of Graph's outline share for the node menu of the focused row
 * (`graph-edit/RowMenu.tsx`, INFRA-5705's R1 to R8): the issue the writes are for, the delete's
 * confirm, which outlives the menu and the row's focus, and the outline's way of taking the reader
 * to a row. `TreeRows.tsx` makes them (`useRowEdits`) in Graph alone, where the Lens act leaves
 * the row for the menu; Conversation and every filter draw their rows as today, with no menu.
 */

/** One open delete confirm's hold on where its row stands in the outline (`RowReveal.deleting`),
 *  so the outline can move the focus to the nearest surviving row once the delete takes the row
 *  out, whether the row drops before or after the confirm closes. */
export interface RowDelete {
  /** The confirm closed on a cancel or a refusal that keeps the row: drop the hold and move no
   *  focus. */
  readonly cancel: () => void;
  /** The delete was taken, or answered gone (`409 NODE_GONE` or `404`), and the confirm has
   *  closed: move the focus to the nearest surviving row now, or as soon as a read drops it. */
  readonly gone: () => void;
}

/** The reader taken to a row of the outline (`IssueTree.tsx`'s, `useRowReveal`). */
export interface RowReveal {
  /** Once the outline draws the node `id` (`TreeNode.id`) under a row read as one of `parents` -
   *  the read ids that name the row of the node a taken move put it under (`placement.ts`'
   *  `rowReadIds`) - unfolds every branch above that node's own row, a folded new parent among
   *  them, and gives the row the focus; until then, and unless the reader acts first, it waits for
   *  the tree that draws it (R4). */
  readonly moved: (id: string, parents: readonly string[]) => void;
  /** Keeps the focus the reader holds on a row of the node of read id `node` on the node's own
   *  row, after a write of it that was refused or dropped: once a read draws that row under
   *  another key - the node where the server holds it, under another parent - it takes the focus
   *  there, unfolding what stands above it, unless the reader acts first; a read that lacks the
   *  node leaves the focus to the outline's drop rule (R8). Nothing where the focus is on no row
   *  of the node. */
  readonly kept: (node: string) => void;
  /** Begins tracking the focused row of the node of read id `node` while its delete confirm is
   *  open, so `RowDelete.gone` still knows where the row stood if a read drops or redraws the tree
   *  before the confirm closes (R6, R8). */
  readonly deleting: (node: string) => RowDelete | undefined;
  /** Takes the reader to the row of the node of read id `node` wherever it stands, as the
   *  outline's keys do: the row a refusal's line names (R8). */
  readonly toRow: (node: string) => void;
}

/**
 * What the issue's room editor gives the node menu and the editor's keys (INFRA-5705's
 * R3 and R5, through part Y's room; `IssueTree.tsx` gives Graph's, `new-row-editing.ts`): the
 * entries that write in the room, and what the menu's verdicts read of it. Where none is given
 * (`OutlineEdits.editor`), New row below, New row under and Retype are grey on a row that takes
 * them, with `ANSWER_LINES.grey_no_editor`, and the menu writes nothing for them; so is Retype
 * where the editor gives no `retype`.
 */
export interface RowEditor {
  /** The kinds the editor's schema for the node's fragment lets it make (R5, `placement.ts`'
   *  `kinds`); each of the four where it gives none. */
  readonly kinds?: readonly RetypeKind[];
  /** Whether the node of read id `node` is a new row the reader made that no `PATCH` has placed
   *  yet (R3, `placement.ts`' `unplaced`). */
  readonly unplaced?: (node: string) => boolean;
  /** The read id the menu and the keys name the node of the row `id` (`TreeNode.id`) by, where
   *  the node carries none: a new row the read does not hold yet (R3). */
  readonly readIdOf?: (id: string) => string | undefined;
  /** Graph's held read as the verdicts read it: with a node for each new row it does not hold
   *  yet (R3). */
  readonly held?: (answer: TreeAnswer) => TreeAnswer;
  /** The line the row of `node` draws as a new row (R3): that it stands after the spec's blocks,
   *  or that its block is saved and not yet placed; undefined for every other row. */
  readonly lineOf?: (node: TreeNode) => string | undefined;
  /** Draws a new row that `entry` makes at `place`, from the row of read id `node`, with the
   *  caret in it (R3), and the line `line` where the new row stands after the spec's blocks. */
  readonly newRow: (
    node: string,
    entry: NewEntry,
    place: NewPlace,
    line: PlaceLine | undefined
  ) => void;
  /** Gives the block of read id `node` the kind `kind` in the room, with the attributes that keep
   *  it where it stands, in the editing of the node's own row, which it opens where no caret is
   *  in the node (R5). */
  readonly retype?: (node: string, kind: RetypeKind, attributes: RetypeAttributes) => void;
}

/** What every row of Graph's outline shares for its menu. */
export interface OutlineEdits {
  /** The issue the writes are for, as `useNodeWrites` takes it. */
  readonly issue: NodeWritesIssue;
  /** Opens the delete's confirm for the node of read id `node`, naming `count` (R6). */
  readonly confirmDelete: (node: string, count: number) => void;
  /** The issue's room editor the menu writes in (`RowEditor`), where the outline gives one. */
  readonly editor: RowEditor | undefined;
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
export type NodeDrawnRow = Extract<DrawnRow, { kind: "node" }>;

/** The issue's status as the menu's verdicts read it (`placement.ts`: `"done"` closes it), from
 *  the outline's own rule for a closed issue (`isClosed`, its `closed_at`): the writes a row
 *  offers are refused on a closed issue and on no other, so any other status reads as open. */
const CLOSED_STATUS = "done";
const OPEN_STATUS = "open";

/** No read id: the rows' read ids outside Graph, which no row reads. */
const NO_READ_IDS: ReadonlyMap<string, string> = new Map();

/** The read id of each node `rows` draw, by its `TreeNode.id`: the node a second row stands
 *  under, and the node the outline is zoomed to, are named by it. */
export function readIdsOf(rows: readonly DrawnRow[]): ReadonlyMap<string, string> {
  const readIds = new Map<string, string>();
  for (const row of rows) {
    if (row.kind === "node" && row.node.readId !== undefined) {
      readIds.set(row.node.id, row.node.readId);
    }
  }
  return readIds;
}

/** The read id the verdicts name the node the outline is zoomed to by (`zoomRoot`, its
 *  `TreeNode.id`), from the rows' read ids (`readIdsOf`); undefined where it is zoomed to none. */
export function zoomReadOf(
  readIds: ReadonlyMap<string, string>,
  zoomRoot: string | undefined
): string | undefined {
  return zoomRoot === undefined ? undefined : readIds.get(zoomRoot);
}

/** The issue `key` Graph's rows write for, as `useNodeWrites` takes it: its status as the
 *  verdicts read it for an issue closed or not (`isClosed`), and the read id of the node the
 *  outline is zoomed to (`zoomReadOf`). */
export function writesIssueOf(
  key: string,
  isClosed: boolean,
  zoomRead: string | undefined
): NodeWritesIssue {
  return {
    key,
    status: isClosed ? CLOSED_STATUS : OPEN_STATUS,
    ...(zoomRead === undefined ? {} : { zoomRoot: zoomRead }),
  };
}

/** Which of its node's rows `row` is, as the verdicts take it (`PlacementRow`): a second row of a
 *  node stands under a node it serves (part T), which the verdicts name by its read id
 *  (`readIds`); a node drawn once is drawn on its own row. */
export function placementRowOf(
  row: NodeDrawnRow,
  readIds: ReadonlyMap<string, string>
): PlacementRow {
  const above = row.trail.at(-1);
  return !row.node.transcluded || above === undefined
    ? "own"
    : { under: readIds.get(above) ?? above };
}

/** The read ids of each held read's nodes, made once for each read (`editingRead`): every row of
 *  Graph asks whether the read holds its node on each draw of the outline. */
const heldIds = new WeakMap<TreeAnswer, ReadonlySet<string>>();

/**
 * Graph's held read of the outline's issue as the verdicts read it - with a node for each new row
 * it does not hold yet (`RowEditor.held`) - where it is of a converted issue and holds the node of
 * read id `readId`; undefined anywhere else, where the node menu holds Lens alone and the editor's
 * keys take nothing.
 */
export function editingRead(
  client: QueryClient,
  outline: Pick<OutlineEdits, "editor" | "issue">,
  readId: string
): TreeAnswer | undefined {
  const read = heldRead(client, outline.issue.key);
  const held = read === undefined ? undefined : (outline.editor?.held?.(read) ?? read);
  if (held?.converted !== true) return undefined;
  let ids = heldIds.get(held);
  if (ids === undefined) {
    ids = new Set((held.nodes ?? []).map((node) => node.id));
    heldIds.set(held, ids);
  }
  return ids.has(readId) ? held : undefined;
}

/** What a node's verdicts are asked for (`verdictsFor`): its read id, the row it is asked from,
 *  the kinds Retype offers where the asker gives them (all four where it gives none), and whether
 *  it is a new row no `PATCH` has placed yet (`RowEditor.unplaced`). */
export interface VerdictsAsk {
  readonly node: string;
  readonly row: PlacementRow;
  readonly kinds?: readonly RetypeKind[];
  readonly unplaced: boolean;
}

/** Each entry's verdict for the node `ask` names (`placementOf`), from Graph's held read as it
 *  stands now (`editingRead`), with that read; undefined where that read is of no converted issue
 *  or does not hold the node. The node menu and the editor's keys read the verdicts so. */
export function verdictsFor(
  client: QueryClient,
  outline: Pick<OutlineEdits, "editor" | "issue">,
  ask: VerdictsAsk
): { readonly held: TreeAnswer; readonly verdicts: Placement } | undefined {
  const held = editingRead(client, outline, ask.node);
  if (held === undefined) return undefined;
  const verdicts = placementOf({
    held,
    kinds: ask.kinds,
    node: ask.node,
    row: ask.row,
    status: outline.issue.status,
    unplaced: ask.unplaced ? true : undefined,
    zoomRoot: outline.issue.zoomRoot,
  });
  return { held, verdicts };
}

/**
 * Makes the new row `entry` gives the node of read id `node` through `editor` (R3,
 * `RowEditor.newRow`), at the place its verdict names (`verdictOf`, asked now): where that place
 * may stand on a page of the outline read of `key` the page has not read (`read_next`), that
 * page is read first, and the verdict is asked again from the read that holds it. A page that is
 * refused makes no row.
 */
export async function placeNewRow(
  client: QueryClient,
  key: string,
  editor: RowEditor,
  node: string,
  entry: NewEntry,
  verdictOf: () => Verdict | undefined
): Promise<void> {
  let verdict = verdictOf();
  while (verdict?.kind === "read_next") {
    await readNextTreePage(client, key, "graph");
    verdict = verdictOf();
  }
  if (verdict?.kind === "new") editor.newRow(node, entry, verdict.place, verdict.line);
}

/** The unit a delete's count names its rows by (R6): one row, or rows. */
export function rowsUnit(count: number): string {
  return count === 1 ? CONTROL_LABELS.delete_row : CONTROL_LABELS.delete_rows;
}

/** A delete the reader picked, which its confirm names: the node's read id and the count the dry
 *  run answered. Each pick is a new one, and its confirm is drawn anew. */
interface DeleteAsk {
  readonly id: number;
  readonly hold: RowDelete | undefined;
  readonly node: string;
  readonly count: number;
}

/**
 * The node menus of the rows `TreeRows.tsx` draws (`act`, its `useLensAct`, says where): in Graph,
 * each row's `RowEdit`, and the delete's confirm while one is open; anywhere else, no row's and
 * none. `zoomRoot` is the id of the node the outline is zoomed to (`TreeNode.id`), whose read id
 * the verdicts read; `reveal` is the outline's (`IssueTree.tsx`), and `editor` the room editor it
 * gives its rows (`RowEditor`).
 */
export function useRowEdits({
  act,
  editor,
  isClosed,
  reveal,
  rows,
  zoomRoot,
}: {
  readonly act: LensAct | undefined;
  readonly editor: RowEditor | undefined;
  readonly isClosed: boolean;
  readonly reveal: RowReveal | undefined;
  readonly rows: readonly DrawnRow[];
  readonly zoomRoot: string | undefined;
}): { readonly confirm: ReactNode; readonly editOf: (row: NodeDrawnRow) => RowEdit | undefined } {
  const key = act?.outline === "graph" ? act.issueKey : undefined;
  const [asked, setAsked] = useState<DeleteAsk | undefined>(undefined);
  const asks = useRef(0);
  const confirmDelete = useCallback(
    (node: string, count: number) => {
      asks.current += 1;
      const hold = reveal?.deleting(node);
      setAsked({ count, hold, id: asks.current, node });
    },
    [reveal]
  );
  const done = useCallback((ask: DeleteAsk) => {
    setAsked((held) => (held === ask ? undefined : held));
  }, []);
  // The rows' read ids (`readIdsOf`), read in Graph alone.
  const readIds = useMemo(() => (key === undefined ? NO_READ_IDS : readIdsOf(rows)), [key, rows]);
  const zoomRead = zoomReadOf(readIds, zoomRoot);
  const outline = useMemo(
    () =>
      key === undefined
        ? undefined
        : { confirmDelete, editor, issue: writesIssueOf(key, isClosed, zoomRead), reveal },
    [confirmDelete, editor, isClosed, key, reveal, zoomRead]
  );
  const own = useMemo(
    () => (outline === undefined ? undefined : { outline, row: "own" as const }),
    [outline]
  );
  const editOf = (row: NodeDrawnRow): RowEdit | undefined => {
    if (outline === undefined) return undefined;
    const placement = placementRowOf(row, readIds);
    return placement === "own" ? own : { outline, row: placement };
  };
  const confirm =
    outline === undefined || asked === undefined ? null : (
      <DeleteConfirm
        ask={asked}
        issue={outline.issue}
        key={asked.id}
        onDone={done}
        reveal={outline.reveal}
      />
    );
  return { confirm, editOf };
}

/**
 * A counted delete keeps its confirm open until the answer. If the count changes, the reader
 * confirms the new count or cancels. The dialog restores focus on close, but the row might
 * already be gone, or `IssueTree` might have forgotten its position while the dialog had focus.
 * After the dialog restores focus, a successful delete or a gone refusal uses the saved position
 * to hand focus on when the row is gone. Other outcomes keep their existing focus policy.
 */
function DeleteConfirm({
  ask,
  issue,
  onDone,
  reveal,
}: {
  ask: DeleteAsk;
  issue: NodeWritesIssue;
  onDone: (ask: DeleteAsk) => void;
  reveal: RowReveal | undefined;
}): ReactNode {
  const writes = useNodeActs(issue);
  const [count, setCount] = useState(ask.count);
  const sending = useRef(false);
  const closed = useRef(false);
  const portal = useRef<HTMLDivElement>(null);
  const actedOutside = useRef(false);
  useLayoutEffect(() => {
    closed.current = false;
    const listening = new AbortController();
    const outside = (event: Event) => {
      if (event.target instanceof Node && !portal.current?.contains(event.target)) {
        actedOutside.current = true;
      }
    };
    for (const type of READER_ACTS) {
      window.addEventListener(type, outside, { capture: true, signal: listening.signal });
    }
    return () => {
      closed.current = true;
      listening.abort();
    };
  }, []);
  const close = () => {
    closed.current = true;
    onDone(ask);
  };
  const cancel = () => {
    ask.hold?.cancel();
    close();
  };
  const confirm = () => {
    if (sending.current) return;
    sending.current = true;
    void writes.remove(ask.node, count).then((outcome) => {
      sending.current = false;
      if (closed.current) {
        ask.hold?.cancel();
        return;
      }
      if (outcome.status === "count_moved") {
        setCount(outcome.count);
        return;
      }
      const gone =
        outcome.status === "taken" || (outcome.status === "refused" && outcome.line.gone);
      const active = document.activeElement;
      const outside =
        active instanceof HTMLElement &&
        active !== document.body &&
        !portal.current?.contains(active)
          ? active
          : undefined;
      flushSync(close);
      if (outside?.isConnected === true) outside.focus({ preventScroll: true });
      const keepFocus = !actedOutside.current && outside === undefined;
      if (gone && keepFocus) {
        ask.hold?.gone();
        return;
      }
      ask.hold?.cancel();
      if (
        keepFocus &&
        (outcome.status === "dropped" || (outcome.status === "refused" && outcome.line.reads))
      ) {
        reveal?.kept(ask.node);
      }
    });
  };
  const unit = rowsUnit(count);
  // The confirm stands over the page, but in the outline's React tree, where a press on it would
  // bubble to the outline's own press handling (`usePressFocus`) and put off the focus the confirm
  // gives back to the row as it closes: its presses stop at its root.
  return createPortal(
    // biome-ignore lint/a11y/noStaticElementInteractions: nothing is made interactive here - the handler only keeps the dialog's own presses out of the outline's.
    <div onMouseDown={(event) => event.stopPropagation()} ref={portal}>
      <ConfirmDialog
        body={`${ANSWER_LINES.confirm_takes} ${count.toLocaleString()} ${unit}`}
        confirmLabel={CONTROL_LABELS.confirm_delete}
        onCancel={cancel}
        onConfirm={confirm}
        title={ANSWER_LINES.delete_confirm_title}
      />
    </div>,
    document.body
  );
}
