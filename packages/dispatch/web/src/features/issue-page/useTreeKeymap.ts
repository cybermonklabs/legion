import { type RefObject, useEffect, useRef } from "react";

import { type KeyBinding, useKeymap } from "../shell/keymap";
import { focusOnDocument } from "../shell/roving";
import { type RowAction, rowActionSelector } from "./DecisionActions";
import { bringOnScreen } from "./identity-block";
import type { TreeNode } from "./issue-tree";
import { type OutlineWriter, replyItemOf, takesAnswer } from "./outline-writer";
import { KEY_REPORTS } from "./page-structure";
import { CONTROL_LABELS } from "./page-words";
import { type DrawnRow, type PageRow, pathTo } from "./tree-fold";
import { decisionBehind, nextQuestion, rowKeyOf } from "./tree-keys";
import { inputMethodKey } from "./useNodeEditing";

/**
 * The tree's keyboard: every key of the issue page's key table that acts on the tree itself,
 * and `t h`, which takes the reader to the issue's children, the outline's own rows. The chords
 * that open a view, Details or the node menu are the page's (`useIssueKeymap.ts`), because each
 * of those is an address the outline does not draw.
 *
 * Every binding is registered in the page's own `issue` scope, so the registry suspends all of
 * them while a caret is in a node's text: reading mode is where no caret is, which the keymap
 * already enforces inside an editable and this module does not restate. Escape is the one key
 * that fires there, because leaving the caret is what it is for.
 *
 * The keys move the focus by focusing a row, never by writing the tree's own state behind the
 * browser's back: a row that takes the focus tells the tree (`NodeRow`'s `onFocus`), which
 * scrolls it clear of the sticky identity block and leaves the tab stop on it. So a key does
 * exactly what a reader's pointer does, and the tree has one answer to where the reader is.
 *
 * The controls a row draws are worked by keys too, and for the same reason the moves are: the
 * tree is one tab stop, so the only tab stops inside a row are the focused row's scrolling
 * blocks, and Tab from a row leaves the tree past them. The arrows and `o` press the fold
 * control - in a view whose rows keep a body open, `o` presses the control that keeps it open -
 * `r` and `a` Reply and Answer - the line `a` sets draws an open question's options as chips of
 * its own - `Shift+K` presses Keep, `Shift+C` presses Change and puts the caret in the form it
 * opens, and `f` steps through the row's links and then through every other control no key
 * presses - Copy reference, Change answer, Edit, a command's Copy - where Enter follows or
 * presses the one it stopped on. The controls that draw more of the outline in place stand
 * beside the rows, and `j` and `k` press them from the row beside each (`tree-next`,
 * `tree-previous`). So every control of the focused row is reachable from the keyboard without
 * a second tab stop (WCAG 2.1.1), and `?` names each key by what it does, as it names every
 * other.
 *
 * Every block of the focused row's words whose content runs past its box - a code block, a
 * table, a diagram's preview - keeps a tab stop of its own, so a reader on the keyboard can
 * scroll it; one whose content fits has nowhere to scroll and keeps none (`RowText.tsx`'s
 * `useOutOfTabOrder`). On that block, or inside it, the keys the browser scrolls by are the
 * browser's: the tree moves, folds and says nothing for them (`scrollsABlock`), and Escape gives
 * the row back, as it does from any control inside a row.
 */
export interface TreeKeyboard {
  /** The element the rows are drawn in, which is the tree itself. */
  readonly container: RefObject<HTMLElement | null>;
  /** Whether a row of the tree is being edited, which Escape in its editor leaves
   *  (`tree-leave-caret`). */
  readonly editing: boolean;
  /** Whether the page takes Escape from a row before the tree is left: a filter by words to
   *  clear or a filter to turn off comes first in the key table's order (`slots/body.tsx`). */
  readonly holdsEscape: boolean;
  /** Takes the reader to the issue's children (`t h`): the focus on the group's first row, and
   *  their own address. Undefined where the tree holds no child. */
  readonly onChildren?: () => void;
  /** What `z` does: in the outline, unfolds every node the reader folded (R11); a view that
   *  draws more or less of itself by the key - the Decisions view's foot - draws it so. */
  readonly onCycle: () => void;
  /** Puts the caret in one node's own text, in the row the key was pressed on. */
  readonly onEdit: (row: string, node: TreeNode) => void;
  /** Says the caret has left a node's text, so the row gives the document back and takes the
   *  focus back. */
  readonly onEditEnd: () => void;
  /** Opens or closes one branch. */
  readonly onFold: (id: string, open: boolean) => void;
  /** Reads the read's next page, as the control after the last row does, and puts the focus on
   *  the first row it drew: what `j` and Down on the last row do while that control is drawn and
   *  no row that pages a node's children follows that row. Undefined where it is not drawn. */
  readonly onMore?: () => void;
  /** Reads the next page of the outline read's older comments, as the control above the first
   *  row does, and puts the focus on the first row it drew: what `k` and Up on the first row do
   *  while that control is drawn. Undefined where it is not. */
  readonly onOlder?: () => void;
  /** Draws the next page of a node's children, as the row that pages them does, and puts the
   *  focus on the first row it drew: what `j` and Down on the row above that row do. Undefined
   *  for a view whose rows draw no such row. */
  readonly onPage?: (row: PageRow) => void;
  /** Keeps one row's body open, or lets it close, in a view whose rows keep a body open by `o`
   *  (the Decisions view's). Where it is not given, `o` folds or unfolds the focused row
   *  (`onFold`), as the outline's does (R11). */
  readonly onPin?: (row: string) => void;
  /** Says where a key could not go, which the tree draws at the row the key was pressed on, and
   *  clears that line with `""` whenever a key does go somewhere. */
  readonly onReport: (line: string) => void;
  /** Unfolds the path down to one node and puts the focus on it. */
  readonly onReveal: (id: string) => void;
  /** Reads the read's next pages until a node `q` or `w` looks for is loaded, and lands on it, or
   *  reports none once no page is left: called where the loaded pages hold no such node and the
   *  read still names a next page. Undefined where no next page remains. */
  readonly onSearch?: (search: TreeSearch) => void;
  /** Roots the tree at one node; the issue's own id is the whole tree. */
  readonly onZoom: (id: string) => void;
  /** Whether every read the tree is built from holds its answer (`slots/body.tsx`). Until then
   *  the next open question or the decision behind a row may be a node that has not arrived, so
   *  `q` and `w` with nowhere to go say nothing rather than an absence that may be false. */
  readonly readsHeld: boolean;
  /** The rows as drawn, in the order the keys move through them. */
  readonly rows: readonly DrawnRow[];
  /** The tree the preset left, which the keys that look past the drawn rows walk. */
  readonly root: TreeNode;
  /** The node the rows are drawn from where that is not the issue, and undefined where the
   *  whole tree is drawn from the issue. */
  readonly zoom: string | undefined;
  /** Where the reader writes, which `r` and `a` set to reply to the focused row and to answer
   *  the focused open question. It is provided wherever the page's writer provider stands above
   *  the tree (`slots/providers.tsx`), a page that draws no line to write in yet included, and
   *  undefined only for a tree drawn with no such provider above it. */
  readonly writer: OutlineWriter | undefined;
}

/** What `q` or `w` looks for past the pages loaded (`TreeKeyboard.onSearch`). */
export type TreeSearch =
  | { readonly from: string | undefined; readonly kind: "question" }
  | { readonly from: string; readonly kind: "decision" };

/** One row of the tree that stands for a node. The row that pages a node's children stands for
 *  no node and takes no focus: `j` and Down on the row above it press it (`tree-next`). */
type NodeRow = Extract<DrawnRow, { kind: "node" }>;

const NODE_ROW = "[data-node-row]";

/** How far a tree reaches when a key steps to its end. */
const END = Number.MAX_SAFE_INTEGER;

/** A block of a row's words that scrolls its own content and keeps a tab stop of its own while
 *  its row holds the focus: each code block, table and diagram's preview. It is known by the mark
 *  `RowText.tsx`'s `useOutOfTabOrder` puts on each scroller `keepScrollers` keeps,
 *  `data-kept-scroller`, and takes off wherever it leaves the tab order or the hook lets it go. */
const KEPT_SCROLLER = `${NODE_ROW} [data-kept-scroller]`;

/** The keys of this keymap the browser also scrolls by: the arrows, Home and End. It scrolls the
 *  box the focus is in where that box scrolls the key's way - Right a wide code block - and the
 *  page where it does not: Home and End go up and down alone. Space and the page keys are the
 *  browser's already, since no binding here takes them. */
const SCROLL_KEYS: Record<string, true> = {
  ArrowDown: true,
  ArrowLeft: true,
  ArrowRight: true,
  ArrowUp: true,
  End: true,
  Home: true,
};

/** The marks of the controls a row draws that a key of this keymap presses, which `f` leaves to
 *  that key: the fold control (the arrows), Keep and Change (`Shift+K`, `Shift+C`), Reply and
 *  Answer (`r`, `a`), and an open question's options, which the line `a` sets draws as chips of
 *  its own (`NodeRow.tsx`'s `NodeOptions`). */
const KEYED_CONTROLS = [
  "[data-node-bullet]",
  rowActionSelector("keep"),
  rowActionSelector("change"),
  '[data-row-act="reply"]',
  '[data-row-act="answer"]',
  "[data-pick-option]",
].join(", ");

/** Whether a key of this keymap presses `control`: one of `KEYED_CONTROLS`, or the control that
 *  keeps a body open in a view whose rows keep one (`o`), which carries no mark of its own and is
 *  known by its words, the page's one name for it (`NodeRow.tsx`'s `KeepOpen`). */
function pressedByAKey(control: HTMLElement): boolean {
  if (control.matches(KEYED_CONTROLS)) return true;
  const words = control.textContent;
  return words === CONTROL_LABELS.keep_open || words === CONTROL_LABELS.let_close;
}

/**
 * The key that enters a tree drawn as one tab stop: Tab or Shift+Tab pressed on the page
 * itself enters it at the row that holds the stop, the one the reader last stood on, or at its
 * first row where none holds it yet. Tab from inside the tree is the browser's own, which leaves
 * the tree because every row but that one takes no focus. `rows` answers the tree's rows as the
 * browser holds them, in order, and `moveTo` moves the reader to the row at an index of them.
 *
 * It is exported for the Plan view's chart, which is to enter its rows as the outline is
 * entered.
 */
export function treeEnterKey({
  moveTo,
  rows,
}: {
  readonly rows: () => readonly HTMLElement[];
  readonly moveTo: (index: number) => void;
}): KeyBinding {
  return {
    id: "tree-enter",
    keys: ["Tab", "Shift+Tab"],
    label: "Move into the tree",
    palette: false,
    run: () => {
      const stop = rows().findIndex((row) => row.tabIndex === 0);
      moveTo(stop === -1 ? 0 : stop);
    },
    when: () => focusOnDocument() && rows().length > 0,
  };
}

/**
 * The move a key makes to `target`, one row of a tree drawn as one tab stop: the focus goes to
 * it, with `options`, and the row places itself on screen as it takes the focus. Where the row
 * already holds the focus or a control inside it does - a key that moves no focus, Home on the
 * first row or End on the last, or one pressed from a link `f` stopped on - the row's own
 * placing may not run, and a row the reader scrolled off would stay off, so it is brought on
 * screen here.
 *
 * It is exported so another drawing of the page moves to its rows as the outline does.
 */
export function moveToRow(target: HTMLElement, options?: FocusOptions): void {
  if (target.contains(document.activeElement)) bringOnScreen(target);
  target.focus(options);
}

export function useTreeKeymap(tree: TreeKeyboard): void {
  // The key being pressed, noted as it sets out for its target: the keymap asks a binding's
  // `when` with no key in hand, and a binding of two keys - `j` and Down - leaves the one that
  // scrolls a block to the browser while the other still moves the reader.
  const pressing = useRef<KeyboardEvent | null>(null);
  useEffect(() => {
    const note = (event: KeyboardEvent): void => {
      pressing.current = event;
    };
    window.addEventListener("keydown", note, true);
    return () => window.removeEventListener("keydown", note, true);
  }, []);
  /** Whether the key being pressed now is one the browser scrolls by, pressed on a block that
   *  keeps its tab stop or inside one (`KEPT_SCROLLER`): the browser scrolls the block, or the
   *  page, so the tree takes none of it, and the keymap prevents nothing. A key the event has
   *  finished with is pressed no longer, so while `?` reads the bindings this is false. */
  const scrollsABlock = (): boolean => {
    const event = pressing.current;
    return (
      event !== null &&
      event.eventPhase !== Event.NONE &&
      SCROLL_KEYS[event.key] === true &&
      event.target instanceof Element &&
      event.target.closest(KEPT_SCROLLER) !== null
    );
  };

  /** The rows as the browser holds them, beside the rows as the fold drew them: the nth element
   *  is the nth node row, so one index answers for both. */
  const drawn = (): { elements: HTMLElement[]; rows: NodeRow[] } => ({
    elements: [...(tree.container.current?.querySelectorAll<HTMLElement>(NODE_ROW) ?? [])],
    rows: tree.rows.filter((row): row is NodeRow => row.kind === "node"),
  });
  /** Which row the reader is on, or -1 when the focus is not on one. A control inside a row
   *  answers with that row: the keys that move the reader are hers wherever she stands in it. */
  const at = (elements: readonly HTMLElement[]): number => {
    const active = document.activeElement;
    return active === null ? -1 : elements.findIndex((row) => row.contains(active));
  };
  /** Which row holds the focus itself, or -1 when a control inside one holds it instead: the
   *  row is the reader's place in the tree, and a control in it - a chip, Keep, Change, the
   *  form that sends a change - is a thing of its own, which takes its own keys. */
  const onRowItself = (elements: readonly HTMLElement[]): number =>
    elements.indexOf(document.activeElement as HTMLElement);
  const inTheTree = (): boolean => {
    const active = document.activeElement;
    return active !== null && tree.container.current?.contains(active) === true;
  };
  /** Whether the reader is reading: on the page itself, or on a row of the tree. A key that
   *  moves the tree is not taken from a control somewhere else on the page. */
  const reading = (): boolean => focusOnDocument() || inTheTree();
  /** The node the reader is on, which is what the keys that act on one node ask for. */
  const focusedRow = (): NodeRow | undefined => {
    const { elements, rows } = drawn();
    return rows[at(elements)];
  };
  /** The element of the focused row, or null where the focus is on no row. */
  const rowElement = (): HTMLElement | null => {
    const { elements } = drawn();
    return elements[at(elements)] ?? null;
  };
  /** One control the focused row draws, by the mark `DecisionActions.tsx` puts on it, or null
   *  where this row draws none or it would refuse a press: a key is offered exactly while the
   *  control it presses would take a click. */
  const rowAction = (action: RowAction): HTMLButtonElement | null => {
    const control = rowElement()?.querySelector<HTMLButtonElement>(rowActionSelector(action));
    return control == null || control.disabled ? null : control;
  };
  /** What `f` steps through on the focused row: first the chips - the edges the row names, each
   *  a target of its own - then the links its words hold, which the row draws above its chips
   *  (`NodeRow.tsx`), then each other control of the row that no key here presses
   *  (`pressedByAKey`). None of them is in the tab order, the tree being one tab stop, so this
   *  key is how a reader reaches one; Enter on a link the key left the focus on follows it, and
   *  Enter or Space on a button presses it. */
  const rowStops = (): HTMLElement[] => {
    const row = rowElement();
    if (row === null) return [];
    const chips = [...row.querySelectorAll<HTMLElement>("[data-node-chip] a[href]")];
    const rest = [...row.querySelectorAll<HTMLElement>("a[href]")];
    const controls = [...row.querySelectorAll<HTMLButtonElement>("button")].filter(
      (control) => control.tabIndex < 0 && !control.disabled && !pressedByAKey(control)
    );
    return [...chips, ...rest.filter((link) => !chips.includes(link)), ...controls];
  };
  /** Moves to the row `index`, clamped to the tree's ends (`moveToRow`), and clears whatever a
   *  key reported. Where that row is the one holding the focus itself, the key that asked for it
   *  stands at the tree's end and moved nowhere: it says `atTheEnd` at the row instead, and the
   *  row is still brought on screen. */
  const moveTo = (index: number, atTheEnd?: string): void => {
    const { elements } = drawn();
    if (elements.length === 0) return;
    const target = elements[Math.min(Math.max(index, 0), elements.length - 1)];
    if (target === undefined) return;
    tree.onReport(atTheEnd !== undefined && target === document.activeElement ? atTheEnd : "");
    moveToRow(target);
  };
  /** Takes the reader to `id`, unfolding whatever stands between them and it. */
  const goTo = (id: string): void => {
    tree.onReport("");
    tree.onReveal(id);
  };
  /** The row that pages a node's children drawn right after the node row `on`, which `j` and
   *  Down there press; undefined where the next row stands for a node, or none follows. */
  const pageAfter = (on: number): PageRow | undefined => {
    let seen = -1;
    for (const [index, row] of tree.rows.entries()) {
      if (row.kind === "node") seen += 1;
      if (seen !== on) continue;
      const next = tree.rows[index + 1];
      return next?.kind === "page" ? next : undefined;
    }
    return undefined;
  };

  useKeymap("issue", [
    // The tree is one tab stop, which Tab from the page itself enters (`treeEnterKey`).
    treeEnterKey({ moveTo, rows: () => drawn().elements }),
    {
      id: "tree-next",
      keys: ["j", "ArrowDown"],
      label: "Move to the next row",
      palette: false,
      run: () => {
        const { elements } = drawn();
        const on = at(elements);
        // The row that pages the children of a node is pressed from the row above it: it draws
        // the next page of them, and the focus goes to the first row it drew.
        const page = on === -1 ? undefined : pageAfter(on);
        if (page !== undefined && tree.onPage !== undefined) {
          tree.onReport("");
          tree.onPage(page);
          return;
        }
        // The control after the last row reads the read's next page from it: its rows are drawn
        // where the read's order puts them, and the focus goes to the first row it drew.
        if (on !== -1 && on === elements.length - 1 && tree.onMore !== undefined) {
          tree.onReport("");
          tree.onMore();
          return;
        }
        moveTo(on === -1 ? 0 : on + 1, KEY_REPORTS.last_row);
      },
      when: () => reading() && !scrollsABlock() && drawn().elements.length > 0,
    },
    {
      id: "tree-previous",
      keys: ["k", "ArrowUp"],
      label: "Move to the previous row",
      palette: false,
      run: () => {
        const { elements } = drawn();
        const on = at(elements);
        // The control above the first row reads the older comments from it: they are drawn
        // where the read's order puts them, and the focus goes to the first row it drew.
        if (on === 0 && tree.onOlder !== undefined) {
          tree.onReport("");
          tree.onOlder();
          return;
        }
        moveTo(on === -1 ? 0 : on - 1, KEY_REPORTS.first_row);
      },
      when: () => reading() && !scrollsABlock() && drawn().elements.length > 0,
    },
    {
      // Home and End are the page's own scroll keys everywhere else, so they move through the
      // tree only while the reader is standing in it.
      id: "tree-first",
      keys: "Home",
      label: "Move to the first row",
      palette: false,
      run: () => moveTo(0, KEY_REPORTS.first_row),
      when: () => inTheTree() && !scrollsABlock(),
    },
    {
      id: "tree-last",
      keys: "End",
      label: "Move to the last row",
      palette: false,
      run: () => moveTo(END, KEY_REPORTS.last_row),
      when: () => inTheTree() && !scrollsABlock(),
    },
    {
      // Right unfolds what is closed and then steps into it, which is the WAI-ARIA tree
      // pattern's rule for the key.
      id: "tree-unfold",
      keys: "ArrowRight",
      label: "Unfold the focused row, or move into it",
      palette: false,
      run: () => {
        const { elements, rows } = drawn();
        const on = at(elements);
        const row = rows[on];
        if (row === undefined) return;
        tree.onReport("");
        // A row with nothing under it gives the key nowhere to go, the row the tree is zoomed to
        // as much as any other, and the key says so at the row.
        if (row.expanded === undefined) {
          tree.onReport(KEY_REPORTS.nothing_under);
          return;
        }
        if (row.expanded === false) {
          tree.onFold(row.node.id, true);
          return;
        }
        // Every level is drawn, so an open row's first child is the row below it.
        if ((rows[on + 1]?.level ?? 0) > row.level) moveTo(on + 1);
      },
      when: () => !scrollsABlock() && at(drawn().elements) !== -1,
    },
    {
      id: "tree-fold",
      keys: "ArrowLeft",
      label: "Fold the focused row, or move to its parent",
      palette: false,
      run: () => {
        const { elements, rows } = drawn();
        const on = at(elements);
        const row = rows[on];
        if (row === undefined) return;
        tree.onReport("");
        // The row the tree is zoomed to stays open over what it holds and its parent is above
        // the zoom, which no row draws: the key has nowhere to go, and says so at the row.
        if (row.node.id === tree.zoom) {
          tree.onReport(KEY_REPORTS.already_zoomed);
          return;
        }
        if (row.expanded === true) {
          tree.onFold(row.node.id, false);
          return;
        }
        for (let above = on - 1; above >= 0; above -= 1) {
          if ((rows[above]?.level ?? 0) === row.level - 1) {
            moveTo(above);
            return;
          }
        }
        // A top-level row of the whole tree, with nothing to fold, has the issue for its parent,
        // which no row draws: the key has nowhere to go, and says so at the row. The zoom is
        // undefined while the rows are drawn from the issue.
        if (tree.zoom === undefined && row.trail.at(-1) === tree.root.id) {
          tree.onReport(KEY_REPORTS.whole_tree);
        }
      },
      when: () => !scrollsABlock() && at(drawn().elements) !== -1,
    },
    {
      // `o` is the fold control's key on the outline (R11): it folds the focused row where it
      // is open and unfolds it where it is folded. A view whose rows keep a body open by the
      // key keeps it open there instead (`onPin`).
      id: "tree-toggle",
      keys: "o",
      label:
        tree.onPin === undefined
          ? "Fold or unfold the focused row"
          : "Keep the focused row's body open",
      palette: false,
      run: () => {
        const row = focusedRow();
        if (row === undefined) return;
        tree.onReport("");
        if (tree.onPin !== undefined) {
          tree.onPin(rowKeyOf(row));
          return;
        }
        if (row.expanded === undefined) {
          tree.onReport(KEY_REPORTS.nothing_under);
          return;
        }
        if (row.node.id === tree.zoom) {
          tree.onReport(KEY_REPORTS.already_zoomed);
          return;
        }
        tree.onFold(row.node.id, row.expanded === false);
      },
      when: () => at(drawn().elements) !== -1,
    },
    {
      // Keep and Change are controls a row draws, and the tree is one tab stop, so no Tab
      // reaches them: each takes a key instead, and a reader on the keyboard works every
      // control of the row they are standing on (WCAG 2.1.1). The key presses the control the
      // row draws, which is what a pointer does, and is offered exactly while that control is
      // drawn and would take a click.
      id: "tree-keep",
      keys: "Shift+K",
      label: `${CONTROL_LABELS.keep_decision} the focused decision`,
      run: () => {
        const control = rowAction("keep");
        if (control === null) return;
        tree.onReport("");
        control.click();
      },
      when: () => rowAction("keep") !== null,
    },
    {
      id: "tree-change",
      keys: "Shift+C",
      label: `${CONTROL_LABELS.change_decision} the focused decision`,
      run: () => {
        const control = rowAction("change");
        if (control === null) return;
        tree.onReport("");
        control.click();
        // A reader who asked to change a decision is there to type, and the field the form
        // opens with takes the caret as it mounts (`DecisionActions.tsx`'s `ChangeComposer`),
        // so this key leaves it there whatever frame React commits the form on, as a pointer
        // does. The form is the one thing inside a row that takes the tab order while it is
        // open, so Tab from the field reaches Send change and Cancel.
      },
      when: () => rowAction("change") !== null,
    },
    {
      // Each press moves to the next stop of the row - a link, then a control no other key
      // presses - and the last comes back to the first, so a row that points at several things
      // is read through them all; Enter on the one the key left the focus on is the browser's
      // own, and follows the link or presses the control (`tree-edit` leaves it Enter).
      id: "tree-link",
      keys: "f",
      label: "Move to the next link or control on the focused row",
      palette: false,
      run: () => {
        const stops = rowStops();
        if (stops.length === 0) return;
        tree.onReport("");
        const on = stops.indexOf(document.activeElement as HTMLElement);
        stops[(on + 1) % stops.length]?.focus();
      },
      when: () => rowStops().length > 0,
    },
    {
      id: "tree-zoom-in",
      keys: ".",
      label: "Zoom into the focused row",
      palette: false,
      run: () => {
        const row = focusedRow();
        if (row === undefined) return;
        // The row the tree is zoomed to already is the zoom the key asks for, so the key moves
        // nothing there, and says so at the row.
        if (row.node.id === tree.zoom) {
          tree.onReport(KEY_REPORTS.already_zoomed);
          return;
        }
        tree.onReport("");
        tree.onZoom(row.node.id);
        tree.onReveal(row.node.id);
      },
      when: () => at(drawn().elements) !== -1,
    },
    {
      id: "tree-zoom-out",
      keys: ",",
      label: "Zoom back out one level",
      run: () => {
        const zoomed = tree.zoom;
        if (zoomed === undefined || zoomed === tree.root.id) {
          tree.onReport(KEY_REPORTS.whole_tree);
          return;
        }
        tree.onReport("");
        // The key walks the breadcrumb back up a crumb at a time, so every node of the issue is
        // reachable from any other (R11).
        const above = pathTo(tree.root, zoomed)?.at(-2)?.id ?? tree.root.id;
        tree.onZoom(above);
        tree.onReveal(zoomed);
      },
      when: reading,
    },
    {
      id: "tree-question",
      keys: "q",
      label: "Move to the next open question",
      run: () => {
        const from = focusedRow()?.node.id;
        const question = nextQuestion(tree.root, from);
        if (question === undefined) {
          if (tree.onSearch !== undefined) {
            tree.onReport("");
            tree.onSearch({ from, kind: "question" });
            return;
          }
          tree.onReport(tree.readsHeld ? KEY_REPORTS.no_next_question : "");
          return;
        }
        goTo(question.id);
      },
      when: reading,
    },
    {
      id: "tree-decision",
      keys: "w",
      label: "Move into the decision behind the focused row",
      run: () => {
        const node = focusedRow()?.node;
        const decision = node === undefined ? undefined : decisionBehind(tree.root, node.id);
        if (decision === undefined) {
          if (node !== undefined && tree.onSearch !== undefined) {
            tree.onReport("");
            tree.onSearch({ from: node.id, kind: "decision" });
            return;
          }
          tree.onReport(tree.readsHeld ? KEY_REPORTS.no_decision : "");
          return;
        }
        goTo(decision.id);
      },
      when: reading,
    },
    {
      // Reply to the focused row: a comment, a message or an ask whatever its state sets
      // the line at the outline's foot to reply to it, as the row's own Reply does; the caret
      // goes to the line. It is offered while the row draws its Reply, so a row that takes none
      // - a message's context row, whose thread is replied to in Threads (R9) - takes no `r`.
      id: "tree-reply",
      keys: "r",
      label: `${CONTROL_LABELS.reply} to the focused row`,
      run: () => {
        const node = focusedRow()?.node;
        const item = node === undefined ? undefined : replyItemOf(node);
        if (item === undefined || tree.writer === undefined) return;
        tree.onReport("");
        tree.writer.replyTo(item);
      },
      when: () => {
        const node = focusedRow()?.node;
        return (
          tree.writer !== undefined &&
          node !== undefined &&
          replyItemOf(node) !== undefined &&
          rowElement()?.querySelector('[data-row-act="reply"]') != null
        );
      },
    },
    {
      // Answer the focused open question: the line answers it, its options its chips.
      id: "tree-answer",
      keys: "a",
      label: `${CONTROL_LABELS.answer} the focused question`,
      run: () => {
        const node = focusedRow()?.node;
        if (node === undefined || tree.writer === undefined) return;
        tree.onReport("");
        tree.writer.answerNode(node);
      },
      when: () => {
        const node = focusedRow()?.node;
        return tree.writer !== undefined && node !== undefined && takesAnswer(node);
      },
    },
    {
      id: "tree-cycle",
      keys: "z",
      label:
        tree.onPin === undefined
          ? "Unfold every row you folded"
          : "Cycle how much of the tree is drawn",
      run: () => {
        tree.onReport("");
        tree.onCycle();
      },
      when: reading,
    },
    {
      // The children are rows of the outline, one group under the issue, which the header
      // stands for and draws no row of, so the chord that reaches them is the tree's: it puts
      // the focus on the group's first row and opens their own address, `/children`
      // (`IssueTree`'s `onChildren`).
      id: "tree-children",
      keys: "t h",
      label: "Move to the issue's children",
      run: () => {
        tree.onReport("");
        tree.onChildren?.();
      },
      when: () => tree.onChildren !== undefined,
    },
    {
      // The caret goes into the node's own text where it lives, which is its document's block
      // drawn in this row (`IssueTree`'s `onEdit`); Escape below brings the reader back to
      // this row and gives the document back to whatever was drawing it.
      //
      // Only from the row itself. Enter on a control inside the row is that control's own -
      // the form that sends a change is pressed by it (`DecisionActions.tsx`), a chip `f`
      // stopped on is followed by it, a button is pressed by it - and a key that took Enter
      // from them would leave a reader who reached them with no way to press them at all
      // (WCAG 2.1.1).
      id: "tree-edit",
      keys: "Enter",
      label: "Put the caret in the focused row",
      palette: false,
      run: () => {
        const { elements, rows } = drawn();
        const on = onRowItself(elements);
        const row = rows[on];
        if (row === undefined) return;
        tree.onReport("");
        tree.onEdit(rowKeyOf(row), row.node);
      },
      when: () => onRowItself(drawn().elements) !== -1,
    },
    {
      // Escape leaves the caret for the row it came from, and leaves the tree from the row
      // (below). It extends the Escape the page already binds for its header controls, which
      // answers for a focused select and never for these two. The Escape that ends an input
      // method's composition is the input method's and leaves nothing (`inputMethodKey`): the
      // keymap passes over one marked `isComposing`, and Safari's comes unmarked. It is offered
      // from an editable inside the tree alone, while a row is edited: an editor elsewhere on the
      // page has keys of its own, and no row's caret to leave.
      id: "tree-leave-caret",
      inEditable: true,
      keys: "Escape",
      label: "Leave the row's text",
      palette: false,
      run: (event) => {
        if (inputMethodKey(event)) return;
        (document.activeElement as HTMLElement | null)?.blur();
        // The editing's end gives the row the caret came from the focus back (`useNodeEditing`).
        tree.onEditEnd();
      },
      when: () => {
        const editable = document.activeElement?.closest('[contenteditable="true"]');
        return (
          tree.editing && editable != null && tree.container.current?.contains(editable) === true
        );
      },
    },
    {
      // From a control the reader reached inside a row - a link or a control `f` stopped on,
      // Keep, Change, a block Tab stopped on to scroll it - Escape gives the row back, because
      // the row is where the keys that move through the tree are; from the row itself it leaves
      // the tree, once the page has cleared its filter by words and turned its filter off, which
      // come first in the key table's order.
      id: "tree-leave",
      keys: "Escape",
      label: "Leave the control, then the tree",
      palette: false,
      run: () => {
        const { elements } = drawn();
        if (onRowItself(elements) !== -1) {
          (document.activeElement as HTMLElement | null)?.blur();
          return;
        }
        elements[at(elements)]?.focus();
      },
      when: () => {
        const { elements } = drawn();
        return at(elements) !== -1 && !(tree.holdsEscape && onRowItself(elements) !== -1);
      },
    },
  ]);
}
