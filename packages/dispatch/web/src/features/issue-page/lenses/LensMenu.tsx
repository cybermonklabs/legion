import { useQuery } from "@tanstack/react-query";
import {
  createContext,
  type KeyboardEvent,
  type ReactNode,
  useContext,
  useEffect,
  useId,
  useRef,
  useState,
} from "react";

import { ApiError } from "../../../api/client";
import {
  pageFailedText,
  pageMenu,
  pageMenuItem,
  pageMutedText,
  pageQuietText,
  pageRowAct,
} from "../../../theme/classes";
import { storeAddressReference } from "../../refs/store-address";
import type { TreeNode } from "../issue-tree";
import { authorText, pageChrome } from "../page-structure";
import { LENS_ANSWERS, LENS_WORDS, type NodeType } from "../page-words";
import { lensKindsQuery, type StandingAddress, useInvokeLens } from "./lens-reads";

/**
 * What a focused row needs to offer the Lens act: the issue the page is on, whether the
 * address's preset is the outline, the one place the act is drawn, and where the reader stands
 * now, which a pick's answer reads as it lands. `TreeRows.tsx` provides it from the address; a
 * route that names no issue provides none, and `LensMenu` then draws no act.
 */
export interface LensAct {
  readonly issueKey: string;
  readonly outline: boolean;
  readonly standing: StandingAddress;
}

export const LensActContext = createContext<LensAct | undefined>(undefined);

/** Whether the invoke route takes a node of each kind: a block of one of the issue's documents,
 *  an ask, a comment, a message, a lens, a finding, a risk and a constraint. Every kind is
 *  named, so a kind the page learns to draw says here whether it takes the act. */
const INVOKED_ON: Readonly<Record<NodeType, boolean>> = {
  ask: true,
  callout: true,
  check: true,
  child_issue: false,
  comment: true,
  constraint: true,
  // A criterion and a reading are blocks of the issue's documents, as a statement is.
  criterion: true,
  decision: true,
  document: false,
  finding: true,
  handoff: false,
  image: true,
  issue: false,
  lens: true,
  message: true,
  plan_task: true,
  reading: true,
  requirement: true,
  result: false,
  risk: true,
};

/** The kinds of the node store the route takes by their uuid address: a lens, a block of a lens
 *  (a finding), a risk and a constraint (`store-address.ts`). */
const STORE_KINDS = ["lens", "risk", "constraint"] as const;

/**
 * Whether a row offers the Lens act: in the outline alone, on a node of a kind the invoke route
 * takes, whose address names this issue - `dispatch://<key>` and then `/` or `#`, so a key that
 * merely starts with this one is another issue - or is a uuid address of the node store. A child
 * issue, a result, the issue, a document, a handoff and a node of another issue get none. A
 * closed issue keeps the act: the server decides.
 */
export function lensActFor(node: TreeNode, act: Pick<LensAct, "issueKey" | "outline">): boolean {
  const reference = node.reference;
  if (!act.outline || !INVOKED_ON[node.type] || reference === undefined) return false;
  const issue = `dispatch://${act.issueKey}`;
  if (reference.startsWith(`${issue}/`) || reference.startsWith(`${issue}#`)) return true;
  const stored = storeAddressReference(reference);
  return (
    stored !== undefined && STORE_KINDS.some((kind) => stored.startsWith(`dispatch://${kind}/`))
  );
}

/**
 * The Lens act of a focused row and the menu of lens kinds it opens, on a row that offers the
 * act (`lensActFor`); on any other row, nothing.
 *
 * The act is one of `RowActs`' controls, drawn the way they are and out of the tab order unless
 * `tabStops` holds, so `f` reaches it as a control no key presses (`useTreeKeymap.ts`'s
 * `rowStops`). It carries `data-row-lens` and not `data-row-act`, which names the writer's acts
 * alone. A press opens the menu, titled `LENS_WORDS.menu_title`, which reads the lens catalogue
 * as it opens - once in a load, and again after a read that failed - and lists each kind in the
 * catalogue's order by its words and, under them, its first source's standard. While the
 * catalogue is read, once its read failed, and while a pick is out, the menu says so on one line
 * with the role `status` beside it, in the panel the menu stands in
 * (`LENS_ANSWERS.loading_kinds`, `kinds_failed`, `examining`).
 *
 * The menu is drawn with phrasing elements, since it stands in `RowActs`' `<p>`, on a line of its
 * own under the acts, hung from that line's left edge and no wider than it, so it stays inside
 * a 390 px page. Its keys are `IssuePresets.tsx`'s menu's: it takes the focus as it opens, and
 * the first item once the catalogue answers; Up and Down move, wrapping, Home and End go to the
 * ends, Enter or a press picks, Escape closes it and gives the focus back to the row, Tab closes
 * it, and so does a press outside it.
 *
 * A pick sends the node's address and the kind's id, nothing more (`useInvokeLens`). A refusal
 * closes the menu, gives the focus back to the row and says so after the act until the menu
 * opens again on that node. The refusal, and whether the pick's write is out, are kept with the
 * node's invocations, not in the menu, since the row losing the focus unmounts `RowActs`: a row
 * that lost the focus while its pick was out says the refusal once it is focused again, and,
 * while the write is still out, its menu says the pick is out and takes no other.
 */
export function LensMenu({ node, tabStops }: { node: TreeNode; tabStops: boolean }): ReactNode {
  const act = useContext(LensActContext);
  const on = node.reference;
  if (act === undefined || on === undefined || !lensActFor(node, act)) return null;
  return <ActAndMenu act={act} on={on} tabStops={tabStops} />;
}

/** What a refused invocation says: the node is gone (404), or anything else. */
type Refusal = "gone" | "failed";

function refusalOf(error: Error | undefined): Refusal | undefined {
  if (error === undefined) return undefined;
  return error instanceof ApiError && error.status === 404 ? "gone" : "failed";
}

function ActAndMenu({
  act,
  on,
  tabStops,
}: {
  act: LensAct;
  on: string;
  tabStops: boolean;
}): ReactNode {
  const invocation = useInvokeLens(act.issueKey, on, act.standing);
  const refusal = refusalOf(invocation.refusal);
  const [open, setOpen] = useState(false);
  // Whether a pick is out, as the menu's line says it and as `pick` reads it. On the menu that
  // made the pick, `picked` holds from the press until `invoke` answers, once the tree is read
  // again. The node's write (`invocation.pending`) holds until it settles, on any menu: a menu
  // drawn again after its row lost the focus starts with `picked` unset.
  const [picked, setPicked] = useState(false);
  const examining = picked || invocation.pending;
  const kinds = useQuery(lensKindsQuery(open));
  const listed = kinds.data;
  const control = useRef<HTMLButtonElement>(null);
  const menu = useRef<HTMLSpanElement>(null);
  const titleId = useId();

  // The menu takes the focus as it opens, and its first item once the catalogue answers; an
  // item the reader moved to keeps it.
  useEffect(() => {
    const list = menu.current;
    if (!open || list === null) return;
    const first = list.querySelector<HTMLElement>('[role="menuitem"]');
    if (listed === undefined || first === null) {
      list.focus();
      return;
    }
    const focus = document.activeElement;
    if (focus === list || !list.contains(focus)) first.focus();
  }, [open, listed]);

  // A press outside the act and the menu closes it - a press on the line beside the menu too,
  // which takes no focus - and leaves the focus to the press. Escape is read in the capture
  // phase, as `IssuePresets.tsx`'s menu reads it, so it is marked before any other listener of
  // the page's: the tree's own Escape passes a marked key by (`keymap.ts`). A dialog over the
  // page takes its own presses and its own Escape.
  useEffect(() => {
    if (!open) return;
    const inDialog = (target: EventTarget | null) =>
      target instanceof Element && target.closest('[role="dialog"]') !== null;
    const closeOutside = (event: PointerEvent) => {
      const target = event.target;
      if (inDialog(target)) return;
      const inside =
        target instanceof Node &&
        (menu.current?.contains(target) === true || control.current?.contains(target) === true);
      if (!inside) setOpen(false);
    };
    const closeOnEscape = (event: globalThis.KeyboardEvent) => {
      if (event.key !== "Escape" || inDialog(event.target)) return;
      if (document.querySelector('[role="dialog"][aria-modal="true"]') !== null) return;
      event.preventDefault();
      focusRow(control.current);
      setOpen(false);
    };
    document.addEventListener("pointerdown", closeOutside);
    document.addEventListener("keydown", closeOnEscape, true);
    return () => {
      document.removeEventListener("pointerdown", closeOutside);
      document.removeEventListener("keydown", closeOnEscape, true);
    };
  }, [open]);

  const openMenu = () => {
    invocation.forget();
    setOpen(true);
  };

  /** Closes the menu once an invocation is answered, giving the focus back to the row where it
   *  was still in the menu. A refusal is the invocation's to keep (`useInvokeLens`). */
  const answered = () => {
    setPicked(false);
    if (menu.current?.contains(document.activeElement) === true) focusRow(control.current);
    setOpen(false);
  };

  // One invocation on the node at a time: a pick sends nothing while the menu says one is out
  // (`examining`). A menu drawn again after its row lost the focus says so until the write
  // settles, not while the tree is read again after it, and takes a pick from then on.
  const pick = (kind: string) => {
    if (examining) return;
    setPicked(true);
    // By the time `useInvokeLens` answers, it has read the tree again and, where Dispatch
    // answered the lens it made and the reader still stands on the outline, moved the address
    // to the new lens's row.
    void invocation.invoke(kind).then(answered, answered);
  };

  // Every key but Tab is the menu's while it is open, its default prevented, so the page's
  // keymap, which passes a marked key by, moves nothing of the tree under it (`keymap.ts`); no
  // React handler of the row or the tree reads keys. A key held with Control, Meta or Alt is no
  // key of the tree (the keymap matches those modifiers exactly), so the browser keeps its own
  // shortcuts, zoom and reload among them. Tab closes the menu: the focus goes back to the act
  // first, so Tab moves on from the act, as from the row's other controls.
  const walk = (event: KeyboardEvent<HTMLElement>) => {
    if (event.ctrlKey || event.metaKey || event.altKey) return;
    if (event.key === "Tab") {
      control.current?.focus();
      setOpen(false);
      return;
    }
    event.preventDefault();
    const items = [...event.currentTarget.querySelectorAll<HTMLElement>('[role="menuitem"]')];
    const at = event.target instanceof HTMLElement ? items.indexOf(event.target) : -1;
    if (event.key === "Enter") {
      const kind = listed?.[at];
      if (kind !== undefined) pick(kind.id);
      return;
    }
    const to = step(event.key, at, items.length);
    if (to !== undefined) items[to]?.focus();
  };

  const line = menuLine(examining, listed !== undefined, kinds);

  return (
    <>
      {/* A press leaves the focus where it is, as the row's other acts do; the menu takes it
          once it opens. */}
      <button
        aria-expanded={open}
        aria-haspopup="menu"
        className={`dispatch-inline-link ${pageRowAct}`}
        data-row-lens=""
        onClick={() => {
          if (!open) {
            openMenu();
            return;
          }
          focusRow(control.current);
          setOpen(false);
        }}
        onMouseDown={(event) => event.preventDefault()}
        ref={control}
        tabIndex={tabStops ? undefined : -1}
        type="button"
        {...pageChrome}
      >
        {LENS_WORDS.act}
      </button>
      {refusal === undefined ? null : (
        <span className={pageFailedText} role="status" {...pageChrome}>
          {refusal === "gone" ? LENS_ANSWERS.gone : LENS_ANSWERS.failed}
        </span>
      )}
      {open ? (
        // A line of its own under the acts, which the menu hangs from.
        <span className="relative block h-0 basis-full">
          <span className={`${pageMenu} left-0 max-w-full text-left`}>
            <span
              aria-labelledby={titleId}
              className="flex flex-col"
              onKeyDown={walk}
              ref={menu}
              role="menu"
              tabIndex={-1}
            >
              <span
                className={`block px-2 pt-1 pb-1.5 text-xs font-semibold ${pageMutedText}`}
                id={titleId}
                {...pageChrome}
              >
                {LENS_WORDS.menu_title}
              </span>
              {(listed ?? []).map((kind) => {
                const standard = kind.sources?.[0]?.standard ?? "";
                return (
                  <button
                    className={`${pageMenuItem} flex flex-col gap-0.5`}
                    key={kind.id}
                    onClick={() => pick(kind.id)}
                    role="menuitem"
                    tabIndex={-1}
                    type="button"
                  >
                    <span className="break-words" data-lens-kind-words="" {...authorText}>
                      {kind.words}
                    </span>
                    {standard === "" ? null : (
                      <span
                        className={`break-words text-xs font-normal ${pageMutedText}`}
                        data-lens-kind-standard=""
                        {...authorText}
                      >
                        {standard}
                      </span>
                    )}
                  </button>
                );
              })}
            </span>
            {/* What the menu says of its reads stands beside it, not in it, on one line with the
                role `status`: the menu holds the focus while the catalogue is read, and a screen
                reader reads a status out as it changes, which it does not do for words inside
                the focused menu. The line is drawn while the menu is open, empty while it says
                nothing, so it is in place before it first speaks. */}
            <span
              className={
                line === undefined
                  ? undefined
                  : `block px-2 py-[7px] text-sm ${line.failed ? pageFailedText : pageQuietText}`
              }
              data-lens-menu-line=""
              role="status"
              {...pageChrome}
            >
              {line?.words}
            </span>
          </span>
        </span>
      ) : null}
    </>
  );
}

/** Gives the focus back to the row the control `inside` stands in, which is focusable. */
function focusRow(inside: HTMLElement | null): void {
  inside?.closest<HTMLElement>("[data-node-row]")?.focus();
}

/** Where the focus goes from item `at` of `count` on `key`: Up and Down step one, wrapping, and
 *  Home and End go to the ends; from the menu itself, Down goes to the first and Up to the last.
 *  Undefined for any other key, and in a menu with no item. */
function step(key: string, at: number, count: number): number | undefined {
  if (count === 0) return undefined;
  if (key === "Home") return 0;
  if (key === "End") return count - 1;
  if (key === "ArrowDown") return at < 0 ? 0 : (at + 1) % count;
  if (key === "ArrowUp") return at < 0 ? count - 1 : (at - 1 + count) % count;
  return undefined;
}

/** One line the menu says of its reads: its words, and whether they say a read failed. */
interface MenuLine {
  readonly failed: boolean;
  readonly words: string;
}

/** What the menu's line says: that a pick is out, else, while the menu lists no kind, that the
 *  catalogue's read failed or is out; nothing once the kinds are listed. */
function menuLine(
  examining: boolean,
  listed: boolean,
  read: { readonly isError: boolean; readonly isFetching: boolean }
): MenuLine | undefined {
  if (examining) return { failed: false, words: LENS_ANSWERS.examining };
  if (listed) return undefined;
  if (read.isError && !read.isFetching) return { failed: true, words: LENS_ANSWERS.kinds_failed };
  return { failed: false, words: LENS_ANSWERS.loading_kinds };
}
