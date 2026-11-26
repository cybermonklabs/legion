import { useQuery, useQueryClient } from "@tanstack/react-query";
import {
  createContext,
  type KeyboardEvent,
  type ReactNode,
  type RefObject,
  useContext,
  useEffect,
  useId,
  useMemo,
  useRef,
  useState,
} from "react";
import { useLocation } from "react-router-dom";

import { ApiError } from "../../../api/client";
import {
  pageFailedText,
  pageMenu,
  pageMenuItem,
  pageMutedText,
  pageQuietText,
  pageRowAct,
} from "../../../theme/classes";
import { dialogOpen } from "../../shell/useDialog";
import type { TreeNode } from "../issue-tree";
import { authorText, pageChrome } from "../page-structure";
import { LENS_ANSWERS, LENS_WORDS, type NodeType } from "../page-words";
import { lensStoreAddress } from "./lens-drafts";
import {
  forgetInvocations,
  lensKindsQuery,
  lensPlaceOf,
  type NodeInvocations,
  type Outline,
  type StandingAddress,
  useInvokeLens,
  useLensInvocations,
  useStandingAddress,
} from "./lens-reads";

/**
 * What each row needs to offer the Lens act: the issue the page is on, the outline the
 * address opens - Conversation or Graph, the two places the act is drawn (R4) - where the
 * reader stands now, which a pick's answer reads as it lands, and what has been invoked on each
 * node of the issue, read once for every row. `TreeRows.tsx` provides it (`useLensAct`); a route
 * that names no issue, and an address that opens no outline - a filter or any other view -
 * provide none, and `LensMenu` then draws no act.
 */
export interface LensAct {
  /** Lets go of every invocation on the node at `on` that has its answer, and so of its
   *  refusal (`forgetInvocations`). */
  readonly forget: (on: string) => void;
  /** What has been invoked on each node of the issue, by the node's address
   *  (`useLensInvocations`). */
  readonly invocations: ReadonlyMap<string, NodeInvocations>;
  readonly issueKey: string;
  readonly outline: Outline;
  readonly standing: StandingAddress;
}

export const LensActContext = createContext<LensAct | undefined>(undefined);

/**
 * The Lens act each row offers, whatever the focus, as `TreeRows.tsx` provides it to its rows:
 * the issue the address names and the outline it opens, read as the page's slots read their address
 * (`lensPlaceOf`), where the reader stands now, and what has been invoked on each node of the
 * issue, read here once for every row (`useLensInvocations`). A route that names no issue, and an
 * address that opens no outline, offer no act. One value while the address stands and no
 * invocation of a lens changes, so a focus move draws no row again for it.
 */
export function useLensAct(): LensAct | undefined {
  const { pathname, search } = useLocation();
  const place = lensPlaceOf(pathname, search);
  const issueKey = place?.issueKey;
  const outline = place?.outline;
  // Where the reader stands now, which a pick's answer reads as it lands, kept for as long as
  // the caller is drawn: the rows, not the row that made the pick, which a fold or filter can
  // unmount while the write is out.
  const standing = useStandingAddress();
  const invocations = useLensInvocations(issueKey);
  const queryClient = useQueryClient();
  return useMemo(
    () =>
      issueKey === undefined || outline === undefined
        ? undefined
        : {
            forget: (on: string) => forgetInvocations(queryClient, issueKey, on),
            invocations,
            issueKey,
            outline,
            standing,
          },
    [invocations, issueKey, outline, queryClient, standing]
  );
}

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
  // A criterion, a reading and a plain paragraph (prose) are blocks of the issue's documents, as a
  // statement is.
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
  prose: true,
  reading: true,
  requirement: true,
  result: false,
  risk: true,
};

/**
 * Whether a row offers the Lens act, which an address of Conversation or Graph alone provides
 * (`useLensAct`): on a node of a kind the invoke route takes, whose address names this issue -
 * `dispatch://<key>` and then `/` or `#`, so a key that merely starts with this one is another
 * issue - or is a uuid address of the node store that the route takes: a lens, a block of a lens
 * (a finding), a risk or a constraint (`lensStoreAddress`), so not an interaction's or a lens
 * kind's. A child issue, a result, the issue, a document, a handoff and a node of another issue
 * get none. A closed issue keeps the act: the server decides.
 */
export function lensActFor(node: TreeNode, act: Pick<LensAct, "issueKey">): boolean {
  const reference = node.reference;
  if (!INVOKED_ON[node.type] || reference === undefined) return false;
  const issue = `dispatch://${act.issueKey}`;
  if (reference.startsWith(`${issue}/`) || reference.startsWith(`${issue}#`)) return true;
  return lensStoreAddress(reference) !== undefined;
}

/**
 * The Lens act of a row and the menu of lens kinds it opens, on every row of Conversation and
 * Graph whose node the invoke route takes (`lensActFor`), whatever the focus; on any other row,
 * nothing. The menu opens only on a press and reads the catalogue only then.
 *
 * The act is one of `RowActs`' controls, drawn the way they are and out of the tab order unless
 * `tabStops` holds, so `f` reaches it as a control no key presses (`useTreeKeymap.ts`'s
 * `rowStops`). It carries `data-row-lens`, not `data-row-act`, which names Settle and the writer's
 * acts. A press opens the menu, titled `LENS_WORDS.menu_title`, which reads the lens catalogue
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
 * it, and so does a press or focus moving outside it.
 *
 * A pick sends the node's address and the kind's id, nothing more (`useInvokeLens`), and lands
 * in the outline it was made in. A refusal closes the menu, gives the focus back to the row
 * where the focus was still in the menu, and says so after the act on that row until the menu
 * opens again on that node. The refusal, and whether the pick's write is out, are kept with the
 * node's invocations, not only in local state, so they outlive the menu closing or the row
 * unmounting: while the write is still out, opening its menu again says the pick is out and
 * takes no other, and a refusal stands on the row until the menu opens again.
 *
 * The act stands on every such row and a focus move draws every row again, so a row reads
 * nothing of the query client itself: its node's invocations come with the act, read once for
 * all the rows (`useLensAct`), and the pick's write and the catalogue's read are the open menu's
 * (`KindsMenu`), drawn only while it is open. The write and its landing outlive the menu.
 */
export function LensMenu({ node, tabStops }: { node: TreeNode; tabStops: boolean }): ReactNode {
  const act = useContext(LensActContext);
  const on = node.reference;
  if (act === undefined || on === undefined || !lensActFor(node, act)) return null;
  return <ActAndMenu act={act} on={on} tabStops={tabStops} />;
}

/** What a refused invocation says: the node is gone (404, or 409 `NODE_GONE`), or anything else. */
type Refusal = "gone" | "failed";

function refusalOf(error: Error | undefined): Refusal | undefined {
  if (error === undefined) return undefined;
  if (!(error instanceof ApiError)) return "failed";
  return error.status === 404 || (error.status === 409 && error.code === "NODE_GONE")
    ? "gone"
    : "failed";
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
  const invocations = act.invocations.get(on);
  const refusal = refusalOf(invocations?.refusal);
  const [open, setOpen] = useState(false);
  // Whether a pick is out, as the menu's line says it and as its pick reads it. On the row that
  // made the pick, `picked` holds from the press until `invoke` answers, once the tree is read
  // again, whether or not its menu is still open. The node's write (`pending`) holds until it
  // settles, on any row: a row drawn again after it unmounted starts with `picked` unset.
  const [picked, setPicked] = useState(false);
  const examining = picked || invocations?.pending === true;
  const control = useRef<HTMLButtonElement>(null);
  const menu = useRef<HTMLSpanElement>(null);

  // A press outside the act and the menu closes it - a press on the line beside the menu too,
  // which takes no focus - and leaves the focus to the press; focus moving outside the act and
  // the menu (for example to another row) closes it too, unless a dialog over the page holds it.
  // Escape is read in the capture phase, as `IssuePresets.tsx`'s menu reads it, so it is marked
  // before any other listener of the page's: the tree's own Escape passes a marked key by
  // (`keymap.ts`). A dialog over the page takes its own presses and its own Escape, wherever the
  // focus is - on the dialog, which takes it as it opens, or on the page after a press on the
  // dialog's words: whether one is open is `useDialog`'s registry's answer (`dialogOpen()`), as
  // `IssuePresets.tsx`'s menu asks it.
  useEffect(() => {
    if (!open) return;
    const inDialog = (target: EventTarget | null) =>
      target instanceof Element && target.closest('[role="dialog"]') !== null;
    const isInside = (target: EventTarget | null) =>
      target instanceof Node &&
      (menu.current?.contains(target) === true || control.current?.contains(target) === true);
    const closeOutside = (event: PointerEvent) => {
      if (inDialog(event.target)) return;
      if (!isInside(event.target)) setOpen(false);
    };
    const closeOnFocusOutside = (event: FocusEvent) => {
      if (inDialog(event.target) || dialogOpen()) return;
      if (!isInside(event.target)) setOpen(false);
    };
    const closeOnEscape = (event: globalThis.KeyboardEvent) => {
      if (event.key !== "Escape" || inDialog(event.target)) return;
      if (dialogOpen()) return;
      event.preventDefault();
      focusRow(control.current);
      setOpen(false);
    };
    document.addEventListener("pointerdown", closeOutside);
    document.addEventListener("focusin", closeOnFocusOutside);
    document.addEventListener("keydown", closeOnEscape, true);
    return () => {
      document.removeEventListener("pointerdown", closeOutside);
      document.removeEventListener("focusin", closeOnFocusOutside);
      document.removeEventListener("keydown", closeOnEscape, true);
    };
  }, [open]);

  const openMenu = () => {
    act.forget(on);
    setOpen(true);
  };

  /** Closes the menu once an invocation is answered, giving the focus back to the row where it
   *  was still in the menu. A refusal is the node's invocations' to keep
   *  (`useLensInvocations`). */
  const answered = () => {
    setPicked(false);
    if (menu.current?.contains(document.activeElement) === true) focusRow(control.current);
    setOpen(false);
  };

  /** Says a pick is out from its press until `answer`, the pick's invocation, answers. */
  const picking = (answer: Promise<unknown>) => {
    setPicked(true);
    void answer.then(answered, answered);
  };

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
        <KindsMenu
          act={act}
          examining={examining}
          menu={menu}
          on={on}
          onPick={picking}
          onTab={() => {
            control.current?.focus();
            setOpen(false);
          }}
        />
      ) : null}
    </>
  );
}

/**
 * The open menu of lens kinds on the node at `on`, drawn only while its act's menu is open: it
 * reads the catalogue (`lensKindsQuery`) and holds the node's write (`useInvokeLens`), which a
 * row whose menu is closed does not. `menu` is the act's handle on it. A pick hands its
 * invocation to `onPick`, and the write and its landing go on once the menu is gone; Tab is
 * `onTab`'s.
 */
function KindsMenu({
  act,
  examining,
  menu,
  on,
  onPick,
  onTab,
}: {
  act: LensAct;
  examining: boolean;
  menu: RefObject<HTMLSpanElement | null>;
  on: string;
  onPick: (answer: Promise<unknown>) => void;
  onTab: () => void;
}): ReactNode {
  const invoke = useInvokeLens(act.issueKey, on, act.outline, act.standing);
  const kinds = useQuery(lensKindsQuery());
  const listed = kinds.data;
  const titleId = useId();

  // The menu takes the focus as it opens, and its first item once the catalogue answers; an
  // item the reader moved to keeps it.
  useEffect(() => {
    const list = menu.current;
    if (list === null) return;
    const first = list.querySelector<HTMLElement>('[role="menuitem"]');
    if (listed === undefined || first === null) {
      list.focus();
      return;
    }
    const focus = document.activeElement;
    if (focus === list || !list.contains(focus)) first.focus();
  }, [listed, menu]);

  // One invocation on the node at a time: a pick sends nothing while the menu says one is out
  // (`examining`).
  const pick = (kind: string) => {
    if (examining) return;
    // By the time `invoke` answers, it has read the outline again and, where Dispatch answered
    // the lens it made and the reader still stands in the outline of the pick, moved the address
    // to the new lens's row.
    onPick(invoke(kind));
  };

  // Every key but Tab is the menu's while it is open, its default prevented, so the page's
  // keymap, which passes a marked key by, moves nothing of the tree under it (`keymap.ts`); no
  // React handler of the row or the tree reads keys. A key held with Control, Meta or Alt is no
  // key of the tree (the keymap matches those modifiers exactly), so the browser keeps its own
  // shortcuts, zoom and reload among them. Tab closes the menu: the focus goes back to the act
  // first, so Tab moves on from the act, as from the row's other controls (`onTab`). Escape is
  // the act's capture listener's, which marks it as it closes the menu, or leaves it, unmarked,
  // to a dialog open over the page, wherever the focus is.
  const walk = (event: KeyboardEvent<HTMLElement>) => {
    if (event.ctrlKey || event.metaKey || event.altKey) return;
    if (event.key === "Tab") {
      onTab();
      return;
    }
    if (event.key === "Escape") return;
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
