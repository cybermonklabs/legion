import { type QueryClient, useQueryClient } from "@tanstack/react-query";
import {
  type KeyboardEvent,
  type ReactNode,
  type RefObject,
  useContext,
  useEffect,
  useId,
  useRef,
  useState,
} from "react";

import {
  pageFailedText,
  pageHairBorder,
  pageMenu,
  pageMenuItem,
  pageMutedText,
  pageRowAct,
} from "../../../theme/classes";
import { dialogOpen } from "../../shell/useDialog";
import type { TreeNode } from "../issue-tree";
import {
  type LensAct,
  LensActContext,
  LensKinds,
  LensRefusal,
  lensActFor,
} from "../lenses/LensMenu";
import { pageChrome } from "../page-structure";
import { ANSWER_LINES, CONTROL_LABELS, LENS_WORDS } from "../page-words";
import {
  outlineQuery,
  readNextTreePage,
  type TreeAnswer,
  type TreeAnswerNode,
} from "../tree-read/tree-read";
import { abortOnReaderAct } from "../useNodeEditing";
import {
  type DeleteCount,
  type DeleteOutcome,
  type MoveEntry,
  type MoveOutcome,
  useNodeWrites,
} from "./node-writes";
import {
  ENTRIES,
  type Entry,
  type GreyReason,
  type Placement,
  placementOf,
  RETYPE_KINDS,
  type RetypeKind,
  type RetypeOption,
} from "./placement";
import { type RowEdit, RowEditorContext } from "./row-edits";

/**
 * The node menu of Graph's focused row (INFRA-5705's R1 to R8): the WAI-ARIA menu button and
 * menu, as `lenses/LensMenu.tsx` draws its own, which `NodeRow.tsx`'s `RowActs` draws after Copy
 * reference on the focused row in Graph alone, where part W's Lens act leaves the row for the
 * menu's last entry. Every other row of Graph keeps the control's place (`RowMenuPlace`), so the
 * focus moving changes no row's height. Its names avoid `nodeMenu`, the presets row's documents
 * menu.
 *
 * On a converted issue (the tree read's `converted`) the menu holds R1's entries in R1's order,
 * each as `placement.ts` answers it for the node from Graph's held read as it stands, on the row
 * it was opened from (`RowEdit.row`); on any other issue, and for a node the held read does not
 * hold, it holds Lens alone. Lens is drawn where part W offers its act (`lensActFor`), and its
 * submenu is part W's kinds and write (`LensKinds`). A grey entry is drawn as
 * `components/RefusableButton.tsx` draws a refused control: `aria-disabled`, its reason under its
 * words, still reached by the arrows, and a pick of it does nothing. New row below, New row under
 * and Retype write in the issue's room, through the editor the page gives the menu
 * (`RowEditorContext`); with none given, each is grey where the node takes it, and says so
 * (`ANSWER_LINES.grey_no_editor`).
 *
 * A press on its control opens it, and so does `m` while the row holds the focus
 * (`useTreeKeymap.ts`'s `tree-menu`), and the focus goes to its first entry. Up and Down move,
 * wrapping, Home and End go to the ends, Enter or a press picks, Right opens Retype's or Lens's
 * submenu and Left closes it, giving the focus back to its entry, and every other key's default is
 * prevented, so no key of the tree under it acts. Escape closes the menu from anywhere in it and
 * gives the focus back to the row, read in the capture phase as `LensMenu.tsx` reads it; Tab
 * closes it with the focus on the control, and so does a press outside it. A pick closes the menu
 * and gives the focus back to the row before its act goes. A submenu is drawn inside the menu,
 * under its entry, so the menu and its submenus stay inside a 390 px page.
 *
 * The moves go through the node's queue (`node-writes.ts`), whose verdict is made again as each
 * one's turn comes; a taken move keeps its row in view and focused (`keepInView`). Delete asks
 * the server what it takes as the menu opens on a node R2 admits (`NodeWrites.count`, once an
 * opening) and nothing on any other; until it answers, Delete says it is counting and cannot be
 * picked; then it names the count, or is grey with the refusal's words, or with a line under it
 * that says the count could not be read, whose pick asks again. A closed menu drops the answer
 * it was waiting for. Delete's pick opens the confirm (`row-edits.tsx`'s `DeleteConfirm`).
 */

/** The data attribute that marks the menu's control, as `data-row-lens` marks part W's act. */
export const ROW_MENU_ATTRIBUTE = "data-row-menu";

const CONTROL_MARK = { [ROW_MENU_ATTRIBUTE]: "" } as const;

/** The submenus the menu draws under their entries. */
type Submenu = "lens" | "retype";

/** The entries that draw a new row through the room's editor. */
type NewEntry = Extract<Entry, "new_below" | "new_under">;

/** What Delete reads from the opening until its dry run answers, and then the answer. */
type DeleteState = { readonly status: "counting" } | DeleteCount;

const COUNTING: DeleteState = { status: "counting" };

/** One entry of the menu as it is drawn. */
interface MenuEntry {
  /** R1's entry, or the line under a Delete whose count could not be read, which asks again. */
  readonly id: Entry | "count_again";
  readonly words: string;
  /** Why the entry is grey, drawn under its words. */
  readonly reason?: string;
  readonly grey: boolean;
  readonly submenu?: Submenu;
  readonly pick?: () => void;
  /** The id of the entry's element, where another element names it. */
  readonly elementId?: string;
  /** The id of the line that says why it is grey, where that line is drawn apart from it. */
  readonly describedBy?: string;
}

const LENS_ENTRY: MenuEntry = { grey: false, id: "lens", submenu: "lens", words: LENS_WORDS.act };

/** How a submenu stands under its entry: inside the menu, set in by a rule at its left. */
const SUBMENU = `ml-2 flex flex-col border-l pl-1 ${pageHairBorder}`;

/** An entry of the menu or a submenu, whose words are quiet while it is grey (the `refused:`
 *  variant of `styles.css`). */
const ITEM = `${pageMenuItem} flex flex-col gap-0.5 refused:text-page-quiet`;

/** The reason a grey entry draws under its words. */
const REASON = `break-words text-xs font-normal ${pageMutedText}`;

/** The menu's panel, hung from the left edge of the line under the row's acts, as part W's menu
 *  hangs, and never wider than that line: `pageMenu`'s 260 px floor gives way where the row's
 *  column is narrower, a deep row at 390 px, since a floor wins over `max-w-full` and the panel
 *  would stand past the column's right edge. The `!` sets this floor over `pageMenu`'s own. */
const PANEL = `${pageMenu} left-0 max-w-full min-w-[min(260px,100%)]! text-left`;

/** A menu's own entries, those of a submenu inside it left out. */
function ownItems(list: HTMLElement): HTMLElement[] {
  return [...list.querySelectorAll<HTMLElement>('[role="menuitem"]')].filter(
    (item) => item.closest('[role="menu"]') === list
  );
}

/** Where the focus goes from item `at` of `count` on `key`, as `LensMenu.tsx`'s menu has it: Up
 *  and Down step one, wrapping, and Home and End go to the ends; from the menu itself, Down goes
 *  to the first and Up to the last. Undefined for any other key. */
function step(key: string, at: number, count: number): number | undefined {
  if (count === 0) return undefined;
  if (key === "Home") return 0;
  if (key === "End") return count - 1;
  if (key === "ArrowDown") return at < 0 ? 0 : (at + 1) % count;
  if (key === "ArrowUp") return at < 0 ? count - 1 : (at - 1 + count) % count;
  return undefined;
}

/** Gives the focus back to the row the control `inside` stands in, which is focusable. */
function focusRowOf(inside: HTMLElement | null): void {
  inside?.closest<HTMLElement>("[data-node-row]")?.focus();
}

/** The node of read id `node` as Graph's held read of issue `key` holds it now. */
function heldNode(client: QueryClient, key: string, node: string): TreeAnswerNode | undefined {
  const held = client.getQueryData<TreeAnswer>(outlineQuery(key, "graph").queryKey);
  return held?.nodes?.find((each) => each.id === node);
}

/** Lens's entry of the menu of `node`: the act part W offers on it, where it offers one
 *  (`lensActFor`), on the node's address. */
function lensOf(
  act: LensAct | undefined,
  node: TreeNode
): { readonly act: LensAct; readonly on: string } | undefined {
  const on = node.reference;
  return act !== undefined && on !== undefined && lensActFor(node, act) ? { act, on } : undefined;
}

/** Whether the menu holds R1's editing entries for the node of read id `readId`: where Graph's
 *  held read of issue `key` is of a converted issue and holds that node. */
function holdsEditing(client: QueryClient, key: string, readId: string | undefined): boolean {
  const held = client.getQueryData<TreeAnswer>(outlineQuery(key, "graph").queryKey);
  return (
    held?.converted === true &&
    readId !== undefined &&
    (held.nodes ?? []).some((each) => each.id === readId)
  );
}

/**
 * Keeps the row of a node an act moves in view and focused (R4, R8). Made as the act is pressed,
 * it notes the node's `placement_seq` as Graph's held read holds it; once the act is taken with a
 * new one - a move, whose answer `node-writes.ts` writes into the held read - and the reader has
 * not acted since, the outline unfolds every branch above the node's own row under its new
 * parent, a folded new parent among them, and gives that row the focus once it is drawn
 * (`RowReveal.moved`). A taken delete leaves the sequence as it was: the outline's drop rule moves
 * the focus once a read draws the tree without the node. An act refused with a refusal that reads
 * the tree again (a 412, a gone node), and an act dropped unsent - a move queued behind a refused
 * one, whose own pick ended the earlier act's hold on the reader's acts - keeps the focus on the
 * node's row where that read draws it (`RowReveal.kept`), so a `412 PRECONDITION_FAILED` whose
 * read draws the node under the parent the server holds leaves the reader on its row.
 */
function keepInView(
  client: QueryClient,
  edit: RowEdit,
  id: string,
  node: string
): (outcome: MoveOutcome | DeleteOutcome) => void {
  const { key } = edit.outline.issue;
  const before = heldNode(client, key, node)?.placement_seq;
  const acts = abortOnReaderAct();
  return (outcome) => {
    const acted = acts.signal.aborted;
    acts.abort();
    if (acted) return;
    if (outcome.status === "dropped" || (outcome.status === "refused" && outcome.line.reads)) {
      edit.outline.reveal?.kept(node);
      return;
    }
    const now = heldNode(client, key, node);
    const parent = now?.parent?.id;
    if (outcome.status !== "taken" || parent === undefined) return;
    if (now?.placement_seq !== before) edit.outline.reveal?.moved(id, parent);
  };
}

/** The node menu of the focused row of Graph, and its control (`RowActs`). */
export function RowMenu({
  edit,
  node,
  tabStops,
}: {
  edit: RowEdit;
  node: TreeNode;
  tabStops: boolean;
}): ReactNode {
  const act = useContext(LensActContext);
  const editor = useContext(RowEditorContext);
  const writes = useNodeWrites(edit.outline.issue);
  const client = useQueryClient();
  const [open, setOpen] = useState(false);
  const [submenu, setSubmenu] = useState<Submenu | undefined>(undefined);
  const [counted, setCounted] = useState<DeleteState>(COUNTING);
  // Where Lens's submenu draws its line: beside the menu, not inside it (`LensKinds`).
  const [lineHost, setLineHost] = useState<HTMLSpanElement | null>(null);
  const control = useRef<HTMLButtonElement>(null);
  const panel = useRef<HTMLSpanElement>(null);
  const menu = useRef<HTMLSpanElement>(null);
  const retypeMenu = useRef<HTMLSpanElement>(null);
  // The dry run out for this opening, which a close or a newer ask drops.
  const out = useRef<AbortController | undefined>(undefined);
  const controlId = useId();
  const retypeId = useId();
  const failedId = useId();
  const { key, status, zoomRoot } = edit.outline.issue;
  const readId = node.readId;
  const lens = lensOf(act, node);

  /** Each entry's verdict for the node from Graph's held read as it stands now, or undefined
   *  where the menu holds Lens alone: an issue the conversion has not reached, and a node the
   *  held read does not hold. */
  const verdictsNow = (): Placement | undefined => {
    const held = client.getQueryData<TreeAnswer>(outlineQuery(key, "graph").queryKey);
    if (held?.converted !== true || readId === undefined) return undefined;
    if (!(held.nodes ?? []).some((each) => each.id === readId)) return undefined;
    return placementOf({
      held,
      kinds: editor?.kinds,
      node: readId,
      row: edit.row,
      status,
      unplaced: editor?.unplaced?.(readId) === true ? true : undefined,
      zoomRoot,
    });
  };

  useEffect(() => () => out.current?.abort(), []);

  // The menu puts the focus on its first entry as it opens, and Retype's submenu on its first
  // kind (`LensKinds` places its own).
  useEffect(() => {
    if (open) menu.current?.querySelector<HTMLElement>('[role="menuitem"]')?.focus();
  }, [open]);
  useEffect(() => {
    if (submenu === "retype") {
      retypeMenu.current?.querySelector<HTMLElement>('[role="menuitem"]')?.focus();
    }
  }, [submenu]);

  // A press outside the control and the menu closes it and leaves the focus to the press. Escape
  // is read in the capture phase, as `LensMenu.tsx` reads it, so it is marked before any other
  // listener of the page's and the tree's own Escape passes it by; a dialog over the page takes
  // its own presses and its own Escape (`dialogOpen`).
  useEffect(() => {
    if (!open) return;
    const inDialog = (target: EventTarget | null) =>
      target instanceof Element && target.closest('[role="dialog"]') !== null;
    const close = () => {
      out.current?.abort();
      setSubmenu(undefined);
      setOpen(false);
    };
    const closeOutside = (event: PointerEvent) => {
      const target = event.target;
      if (inDialog(target)) return;
      const inside =
        target instanceof Node &&
        (panel.current?.contains(target) === true || control.current?.contains(target) === true);
      if (!inside) close();
    };
    const closeOnEscape = (event: globalThis.KeyboardEvent) => {
      if (event.key !== "Escape" || inDialog(event.target)) return;
      if (dialogOpen()) return;
      event.preventDefault();
      focusRowOf(control.current);
      close();
    };
    document.addEventListener("pointerdown", closeOutside);
    document.addEventListener("keydown", closeOnEscape, true);
    return () => {
      document.removeEventListener("pointerdown", closeOutside);
      document.removeEventListener("keydown", closeOnEscape, true);
    };
  }, [open]);

  // Delete's dry run (R6), sent with this opening's signal: an answer that comes once the menu
  // has closed, or once a newer ask has gone out, is drawn nowhere.
  const askCount = (nodeId: string) => {
    out.current?.abort();
    const asked = new AbortController();
    out.current = asked;
    setCounted(COUNTING);
    void writes.count(nodeId, asked.signal).then(
      (answer) => {
        if (!asked.signal.aborted) setCounted(answer);
      },
      // Dropped: the menu that asked has closed, or asked again.
      () => undefined
    );
  };

  const openMenu = () => {
    if (readId !== undefined && verdictsNow()?.delete.kind === "ask") askCount(readId);
    setSubmenu(undefined);
    setOpen(true);
  };
  const closeMenu = () => {
    out.current?.abort();
    setSubmenu(undefined);
    setOpen(false);
  };
  /** A pick's end: the menu closes and the focus goes back to the row, before the act goes. */
  const finish = () => {
    focusRowOf(control.current);
    closeMenu();
  };

  const pickMove = (entry: MoveEntry) => {
    if (readId === undefined) return;
    finish();
    const kept = keepInView(client, edit, node.id, readId);
    void writes.move(readId, entry, edit.row).then(kept);
  };

  // A new row's place may stand on a page the page has not read (`read_next`): that page is read
  // first, and the place is asked again from the read that holds it. A refused page draws no
  // row, and leaves the next page's control for another press, as the outline's own reads of a
  // page do.
  const placeNew = async (entry: NewEntry): Promise<void> => {
    if (editor === undefined || readId === undefined) return;
    let verdict = verdictsNow()?.[entry];
    while (verdict?.kind === "read_next") {
      await readNextTreePage(client, key, "graph");
      verdict = verdictsNow()?.[entry];
    }
    if (verdict?.kind === "new") editor.newRow(readId, verdict.place, verdict.line);
  };
  const pickNew = (entry: NewEntry) => {
    finish();
    void placeNew(entry).catch(() => undefined);
  };

  const pickRetype = (kind: RetypeKind, option: RetypeOption) => {
    if (option.kind === "grey" || editor === undefined || readId === undefined) return;
    finish();
    editor.retype(readId, kind, option.attributes);
  };

  // A Lens pick is answered once the tree is read again: the menu closes then, as part W's does,
  // giving the focus back to the row where it was still in the menu.
  const lensAnswered = () => {
    if (panel.current?.contains(document.activeElement) === true) focusRowOf(control.current);
    closeMenu();
  };

  const openSubmenu = (which: Submenu) => {
    if (submenu === which) {
      const selector = `[data-row-submenu="${which}"] [role="menuitem"]`;
      panel.current?.querySelector<HTMLElement>(selector)?.focus();
      return;
    }
    setSubmenu(which);
  };
  /** Left in a submenu: the focus goes back to its entry, and then the submenu closes. */
  const back = (which: Submenu) => {
    menu.current?.querySelector<HTMLElement>(`[data-row-menu-entry="${which}"]`)?.focus();
    setSubmenu(undefined);
  };

  const verdicts = open ? verdictsNow() : undefined;
  const retypeVerdict = verdicts?.retype;
  const retypeOptions = retypeVerdict?.kind === "retype" ? retypeVerdict.kinds : undefined;

  /** The entry `id` grey with `reason`. */
  const greyEntry = (id: Exclude<Entry, "lens">, reason: GreyReason): MenuEntry => ({
    grey: true,
    id,
    reason: ANSWER_LINES[reason],
    words: CONTROL_LABELS[`edit_${id}`],
  });

  /** Delete's entries, as its dry run stands (R6): counting, its count, its refusal, or the line
   *  that says the count could not be read and asks again. */
  const deleteEntries = (nodeId: string): MenuEntry[] => {
    const words = CONTROL_LABELS.edit_delete;
    switch (counted.status) {
      case "counting": {
        const counting = `${words} ${CONTROL_LABELS.delete_counting}`;
        return [{ grey: true, id: "delete", words: counting }];
      }
      case "counted": {
        const { count } = counted;
        const unit = count === 1 ? CONTROL_LABELS.delete_row : CONTROL_LABELS.delete_rows;
        const pick = () => {
          finish();
          edit.outline.confirmDelete(nodeId, count);
        };
        const named = `${words} ${count.toLocaleString()} ${unit}`;
        return [{ grey: false, id: "delete", pick, words: named }];
      }
      case "refused":
        return [{ grey: true, id: "delete", reason: counted.line.text, words }];
      case "failed": {
        // The line goes as the count is asked again: the focus goes to Delete first.
        const again = () => {
          menu.current?.querySelector<HTMLElement>('[data-row-menu-entry="delete"]')?.focus();
          askCount(nodeId);
        };
        return [
          { describedBy: failedId, grey: true, id: "delete", words },
          {
            elementId: failedId,
            grey: false,
            id: "count_again",
            pick: again,
            words: ANSWER_LINES.delete_count_failed,
          },
        ];
      }
    }
  };

  /** R1's entries before Lens, on a converted issue, each as its verdict stands. */
  const editingEntries = (placement: Placement, nodeId: string): MenuEntry[] =>
    ENTRIES.flatMap((id): MenuEntry[] => {
      if (id === "lens") return [];
      const verdict = placement[id];
      if (verdict.kind === "grey") return [greyEntry(id, verdict.reason)];
      const words = CONTROL_LABELS[`edit_${id}`];
      switch (id) {
        case "new_below":
        case "new_under":
          if (editor === undefined) return [greyEntry(id, "grey_no_editor")];
          return [{ grey: false, id, pick: () => pickNew(id), words }];
        case "retype":
          if (editor === undefined) return [greyEntry(id, "grey_no_editor")];
          return [{ elementId: retypeId, grey: false, id, submenu: "retype", words }];
        case "delete":
          return deleteEntries(nodeId);
        default:
          return [{ grey: false, id, pick: () => pickMove(id), words }];
      }
    });

  const entries: readonly MenuEntry[] = [
    ...(verdicts === undefined || readId === undefined ? [] : editingEntries(verdicts, readId)),
    ...(lens === undefined ? [] : [LENS_ENTRY]),
  ];

  const choose = (entry: MenuEntry) => {
    if (entry.grey) return;
    if (entry.submenu !== undefined) {
      openSubmenu(entry.submenu);
      return;
    }
    entry.pick?.();
  };

  // Every key but Tab is the menu's while it is open, its default prevented, so the page's keymap,
  // which passes a marked key by, moves nothing of the tree under it (`keymap.ts`). A key held
  // with Control, Meta or Alt is no key of the menu. Tab closes the menu with the focus on its
  // control, so Tab moves on from there. Escape is the capture listener's above. Lens's submenu
  // takes its own keys (`LensKinds`); Retype's are read here.
  const walk = (event: KeyboardEvent<HTMLElement>) => {
    if (event.ctrlKey || event.metaKey || event.altKey) return;
    const target = event.target;
    if (!(target instanceof HTMLElement)) return;
    if (event.key === "Tab") {
      control.current?.focus();
      closeMenu();
      return;
    }
    if (event.key === "Escape") return;
    const list = target.closest<HTMLElement>('[role="menu"]');
    if (list === null || (list !== menu.current && list !== retypeMenu.current)) return;
    event.preventDefault();
    const items = ownItems(list);
    const at = items.indexOf(target);
    if (list === retypeMenu.current) {
      const kind = RETYPE_KINDS.find((each) => each === target.dataset.retypeKind);
      const option = kind === undefined ? undefined : retypeOptions?.[kind];
      if (event.key === "ArrowLeft") {
        back("retype");
        return;
      }
      if (event.key === "Enter") {
        if (kind !== undefined && option !== undefined) pickRetype(kind, option);
        return;
      }
      const to = step(event.key, at, items.length);
      if (to !== undefined) items[to]?.focus();
      return;
    }
    const entry = entries.find((each) => each.id === target.dataset.rowMenuEntry);
    if (event.key === "Enter") {
      if (entry !== undefined) choose(entry);
      return;
    }
    if (event.key === "ArrowRight") {
      if (entry?.submenu !== undefined && !entry.grey) openSubmenu(entry.submenu);
      return;
    }
    const to = step(event.key, at, items.length);
    if (to !== undefined) items[to]?.focus();
  };

  if (!holdsEditing(client, key, readId) && lens === undefined) return null;

  return (
    <>
      {/* A press leaves the focus where it is, as the row's other acts do; the menu takes it once
          it opens. */}
      <button
        aria-expanded={open}
        aria-haspopup="menu"
        className={`dispatch-inline-link ${pageRowAct}`}
        id={controlId}
        onClick={() => (open ? finish() : openMenu())}
        onMouseDown={(event) => event.preventDefault()}
        ref={control}
        tabIndex={tabStops ? undefined : -1}
        type="button"
        {...CONTROL_MARK}
        {...pageChrome}
      >
        {CONTROL_LABELS.edit_menu}
      </button>
      {lens === undefined ? null : <LensRefusal act={lens.act} on={lens.on} />}
      {open ? (
        // A line of its own under the acts, which the menu hangs from, as part W's does.
        <span className="relative block h-0 basis-full">
          <span className={PANEL} ref={panel}>
            <span
              aria-labelledby={controlId}
              className="flex flex-col"
              onKeyDown={walk}
              ref={menu}
              role="menu"
              tabIndex={-1}
            >
              {entries.map((entry) => (
                <MenuItem
                  entry={entry}
                  expanded={entry.submenu === undefined ? undefined : submenu === entry.submenu}
                  key={entry.id}
                  onPick={() => choose(entry)}
                >
                  {entry.id === "retype" && submenu === "retype" && retypeOptions !== undefined ? (
                    <RetypeKinds
                      labelledBy={retypeId}
                      list={retypeMenu}
                      onPick={pickRetype}
                      options={retypeOptions}
                    />
                  ) : null}
                  {entry.id === "lens" && submenu === "lens" && lens !== undefined ? (
                    <span className={SUBMENU} data-row-submenu="lens">
                      <LensKinds
                        act={lens.act}
                        lineHost={lineHost}
                        on={lens.on}
                        onAnswered={lensAnswered}
                        onBack={() => back("lens")}
                      />
                    </span>
                  ) : null}
                </MenuItem>
              ))}
            </span>
            <span ref={setLineHost} />
          </span>
        </span>
      ) : null}
    </>
  );
}

/**
 * What a row of Graph that does not hold the focus draws where the focused row draws the node
 * menu's control (`RowMenu`): the control's words in the control's box, hidden and inert, on the
 * rows that draw the control once focused, so the focus moving changes no row's text or height
 * (part T's R11, as part W's Lens act stands on every row for it); and the line of Lens's refused
 * pick, which stands on the row whatever the focus, as part W's does. Only the focused row has
 * the menu (R1): no press, key or screen reader reaches this.
 */
export function RowMenuPlace({ edit, node }: { edit: RowEdit; node: TreeNode }): ReactNode {
  const act = useContext(LensActContext);
  const client = useQueryClient();
  const lens = lensOf(act, node);
  if (!holdsEditing(client, edit.outline.issue.key, node.readId) && lens === undefined) {
    return null;
  }
  return (
    <>
      <span
        aria-hidden="true"
        className={`dispatch-inline-link invisible ${pageRowAct}`}
        {...pageChrome}
      >
        {CONTROL_LABELS.edit_menu}
      </span>
      {lens === undefined ? null : <LensRefusal act={lens.act} on={lens.on} />}
    </>
  );
}

/**
 * One entry of the menu: its words, its grey reason under them where it is grey, and its
 * submenu after it where that is open (`children`).
 */
function MenuItem({
  children,
  entry,
  expanded,
  onPick,
}: {
  children: ReactNode;
  entry: MenuEntry;
  expanded: boolean | undefined;
  onPick: () => void;
}): ReactNode {
  return (
    <>
      <button
        aria-describedby={entry.describedBy}
        aria-disabled={entry.grey ? true : undefined}
        aria-expanded={expanded}
        aria-haspopup={entry.submenu === undefined ? undefined : "menu"}
        className={ITEM}
        data-row-menu-entry={entry.id}
        id={entry.elementId}
        onClick={onPick}
        role="menuitem"
        tabIndex={-1}
        type="button"
        {...pageChrome}
      >
        <span className="break-words">{entry.words}</span>
        {/* Why it is grey, the page's or the server's sentence drawn as its characters: the
            reading list's `node-row-write-line`. */}
        {entry.reason === undefined ? null : <span className={REASON}>{entry.reason}</span>}
      </button>
      {children}
    </>
  );
}

/**
 * Retype's submenu (R5): each kind the editor's schema for the node's fragment lets it make, in
 * R5's order, as `placement.ts` answers it, a grey one with its reason under its words. Its keys
 * are `RowMenu`'s (`walk`), read through `list`.
 */
function RetypeKinds({
  labelledBy,
  list,
  onPick,
  options,
}: {
  labelledBy: string;
  list: RefObject<HTMLSpanElement | null>;
  onPick: (kind: RetypeKind, option: RetypeOption) => void;
  options: Readonly<Partial<Record<RetypeKind, RetypeOption>>>;
}): ReactNode {
  return (
    <span className={SUBMENU} data-row-submenu="retype">
      <span
        aria-labelledby={labelledBy}
        className="flex flex-col"
        ref={list}
        role="menu"
        tabIndex={-1}
      >
        {RETYPE_KINDS.flatMap((kind) => {
          const option = options[kind];
          if (option === undefined) return [];
          return [
            <button
              aria-disabled={option.kind === "grey" ? true : undefined}
              className={ITEM}
              data-retype-kind={kind}
              key={kind}
              onClick={() => onPick(kind, option)}
              role="menuitem"
              tabIndex={-1}
              type="button"
              {...pageChrome}
            >
              <span className="break-words">{CONTROL_LABELS[`retype_${kind}`]}</span>
              {option.kind === "grey" ? (
                <span className={REASON}>{ANSWER_LINES[option.reason]}</span>
              ) : null}
            </button>,
          ];
        })}
      </span>
    </span>
  );
}

/**
 * The line a refused or dropped write of the node menu draws on the row (R4, R6, R8), in the
 * page's words, with the role `status`, until the node's next act (`NodeWrites.lines`): drawn on
 * every row of the node in Graph, whether or not it holds the focus, so a refusal that comes once
 * the reader moved on is still said where it happened. The fallback line is a press that makes
 * the act again from the tree as it stands then, and where that is a delete whose count moved,
 * the confirm opens again with the new count; a line that names a container is a press that takes
 * the reader to the container's row (`RowReveal.toRow`). Out of the tab order unless `tabStops`
 * holds, as the row's other controls are.
 */
export function RowEditLine({
  edit,
  node,
  tabStops,
}: {
  edit: RowEdit;
  node: TreeNode;
  tabStops: boolean;
}): ReactNode {
  const writes = useNodeWrites(edit.outline.issue);
  const client = useQueryClient();
  const readId = node.readId;
  const line = readId === undefined ? undefined : writes.lines.get(readId);
  if (line === undefined || readId === undefined) return null;
  const { again, container } = line;
  let press: (() => void) | undefined;
  if (again !== undefined) {
    press = () => {
      const kept = keepInView(client, edit, node.id, readId);
      void again().then((outcome) => {
        if (outcome.status === "count_moved") edit.outline.confirmDelete(readId, outcome.count);
        kept(outcome);
      });
    };
  } else if (container !== undefined) {
    press = () => edit.outline.reveal?.toRow(container);
  }
  return (
    <p
      className={`text-xs leading-5 ${pageFailedText}`}
      data-row-edit-line=""
      role="status"
      {...pageChrome}
    >
      {press === undefined ? (
        // The line drawn as its characters: the reading list's `node-row-write-line`.
        line.text
      ) : (
        <button
          className="dispatch-inline-link text-left underline underline-offset-[3px]"
          onClick={press}
          onMouseDown={(event) => event.preventDefault()}
          tabIndex={tabStops ? undefined : -1}
          type="button"
        >
          {/* The same line, as a press: `node-row-write-line` again. */}
          {line.text}
        </button>
      )}
    </p>
  );
}
