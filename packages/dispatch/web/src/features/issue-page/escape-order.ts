import { type RefObject, useEffect, useRef } from "react";
import { flushSync } from "react-dom";
import { useLocation, useNavigate } from "react-router-dom";

import { useKeymap } from "../shell/keymap";
import { useOutlineAddress } from "./document-address";
import { WORDS_FILTER_ATTRIBUTE } from "./IssuePresets";
import { presetPath } from "./issue-address";
import { CONTROL_LABELS } from "./page-words";
import { useFollowing } from "./slots/following";
import { nodeRowSelectorOf } from "./tree-focus";

/** The filter by words open as its box, and the small control it shuts into (`IssuePresets.tsx`). */
const WORDS_BOX = `input[${WORDS_FILTER_ATTRIBUTE}]`;
const WORDS_CONTROL = `button[${WORDS_FILTER_ATTRIBUTE}]`;

/** A row of the outline or of a view. */
const ROW = "[data-node-row]";

/** The stage's own tab stop: the tab-stop row of the outline, or of the view drawn in its place,
 *  each a tree named as the outline is, which the Contents pane's list is not (`t c` reaches the
 *  same row, `useIssueKeymap.ts`). */
const STAGE_TAB_STOP = `[role="tree"][aria-label="${CONTROL_LABELS.tree}"] ${ROW}[tabindex="0"]`;

/** The Verification view's body, which binds no Escape of its own (`VerificationFilterBody.tsx`):
 *  nothing gives a row of it back from a control inside it, so the view's Escape is next there. */
const VERIFICATION_BODY = "[data-verification-body]";

/**
 * The page's own Escape steps between a caret and the outline (dispatch://INFRA-1196 part N's R1,
 * the plan's ST-2). One press does the first step whose state holds:
 *
 * 1. a caret in a row's editor is left (`useTreeKeymap.ts`'s `tree-leave-caret`), and a caret in
 *    an opened document's text by the document itself;
 * 2. an opened document closes, back to the outline address it was opened from
 *    (`OpenedDocument.tsx`);
 * 3. the focus in the words box leaves it (`words-leave`): for the row it was on before it went
 *    to the box, else - where it was on no row, or that row is no longer drawn - for the box's
 *    Filter control where the box holds no words, which then shuts into it, and for the stage's
 *    own tab stop where it holds words, which stay; never for the page's body;
 * 4. words in `?q=`, with the focus outside the box, are cleared (`words-clear`), which shuts the
 *    box: the box's own write, a replace marked with the following it was typed in, so the view
 *    stays and the following with it (`slots/following.ts`);
 * 5. a view on is turned off (`filters-off`), which is Conversation's address; the stage then
 *    asks the outline for the node the reader was last on in the view, which it draws focused
 *    (R14, `stage-landing.tsx`'s `ViewNodeKeeper`, which `t c` and Conversation reach too);
 * 6. a row of the outline gives the outline up (`useTreeKeymap.ts`'s `tree-leave`).
 *
 * Steps 3 to 5 are these. The keymap runs the first binding of a scope whose `when` holds, and
 * the order bindings are registered in follows mount timing, so each step holds only where every
 * step above it does not: a document open, a menu open over the page (whose press it is), or the
 * focus on a control inside a row - which a tree's own Escape gives the row back from first - each
 * leaves the three to the step above, and a view's rows and the outline's hold their own Escape
 * from the row itself while a view or words are on (`holdsEscape`). Details, a menu, the version
 * control's panel, the Outline sheet and a dialog each take their own Escape before any of these,
 * as on today's page: the keymap answers no press one of them has taken.
 */
export function useEscapeOrder({
  documentOpen,
  filter,
  issueKey,
  viewOn,
}: {
  /** Whether a document is opened in place of the outline, whose close comes first. */
  documentOpen: boolean;
  /** What the filter by words holds, which is `?q=`. */
  filter: string;
  issueKey: string;
  /** Whether a view is on. */
  viewOn: boolean;
}): void {
  const navigate = useNavigate();
  const { hash, pathname, search } = useLocation();
  const following = useFollowing();
  const outline = useOutlineAddress(issueKey);
  const before = useRowBeforeWords();
  // Clearing the words is the box's own write (`view-row-tools.tsx`): `?q=` taken out in place,
  // by a replace marked with the following the words were typed in, which keeps the outline an
  // opened document goes back to and leaves out a press of the count of what waits, which acts
  // once.
  const clearWords = () => {
    const query = new URLSearchParams(search);
    query.delete("q");
    const rest = query.toString();
    navigate(`${pathname}${rest === "" ? "" : `?${rest}`}${hash}`, {
      replace: true,
      state: { outline, words: following },
    });
  };
  const pageEscapes = () => {
    if (documentOpen || document.querySelector('[role="menu"]') !== null) return false;
    const active = document.activeElement;
    const row = active?.closest(ROW);
    return (
      row === null || row === undefined || row === active || row.closest(VERIFICATION_BODY) !== null
    );
  };
  useKeymap("issue", [
    {
      id: "words-leave",
      inEditable: true,
      keys: "Escape",
      label: "Leave the filter by words",
      palette: false,
      run: () => {
        const box = document.activeElement as HTMLElement;
        const row = before.current;
        const back =
          row === null
            ? null
            : row.isConnected
              ? row
              : document.querySelector<HTMLElement>(nodeRowSelectorOf(row.dataset.nodeId ?? ""));
        if (back !== null) {
          back.focus();
          return;
        }
        if (filter === "") {
          // The box shuts as it is left empty, in this press, so its control is there to take
          // the focus.
          flushSync(() => box.blur());
          document.querySelector<HTMLElement>(WORDS_CONTROL)?.focus();
          return;
        }
        // A box that holds words stays open, and the reader goes back to where the stage's keys
        // work. With no row drawn to take the focus, it stays in the box.
        document.querySelector<HTMLElement>(STAGE_TAB_STOP)?.focus();
      },
      when: () => document.activeElement?.matches(WORDS_BOX) === true,
    },
    {
      id: "words-clear",
      keys: "Escape",
      label: "Clear the filter by words",
      palette: false,
      run: clearWords,
      when: () => filter !== "" && pageEscapes(),
    },
    {
      id: "filters-off",
      keys: "Escape",
      label: "Turn every view off",
      palette: false,
      run: () => navigate(presetPath(issueKey, "conversation")),
      when: () => viewOn && filter === "" && pageEscapes(),
    },
  ]);
}

/**
 * The row the focus was on before it went to the filter by words, which leaving the box gives it
 * back to (ST-1): where the focus last stood outside the box and its control, held while the
 * focus is in either. None where that was no row, or where the focus went to nothing - a press on
 * the page's blank space - before it went to the box.
 */
function useRowBeforeWords(): RefObject<HTMLElement | null> {
  const row = useRef<HTMLElement | null>(null);
  useEffect(() => {
    const inWords = (target: EventTarget | null) =>
      target instanceof Element && target.closest(`[${WORDS_FILTER_ATTRIBUTE}]`) !== null;
    const took = (event: FocusEvent) => {
      if (inWords(event.target)) return;
      row.current = event.target instanceof Element ? event.target.closest<HTMLElement>(ROW) : null;
    };
    const lost = (event: FocusEvent) => {
      if (event.relatedTarget === null && !inWords(event.target)) row.current = null;
    };
    document.addEventListener("focusin", took);
    document.addEventListener("focusout", lost);
    return () => {
      document.removeEventListener("focusin", took);
      document.removeEventListener("focusout", lost);
    };
  }, []);
  return row;
}
