import { type RefObject, useEffect, useRef } from "react";
import { flushSync } from "react-dom";
import { useNavigate } from "react-router-dom";

import type { IssueRoute } from "../refs/routes";
import { useKeymap } from "../shell/keymap";
import { useChipRecord } from "./chip-return";
import { DOCUMENTS_LIST_ATTRIBUTE } from "./DocumentsList";
import { escapeWayBack, useFollowWayBack, useOutlineAddress } from "./document-address";
import { WORDS_FILTER_ATTRIBUTE } from "./IssuePresets";
import { presetPath } from "./issue-address";
import { CONTROL_LABELS } from "./page-words";
import { nodeRowSelectorOf } from "./tree-focus";
import { useWriteWords } from "./view-row-tools";

/** The filter by words open as its box, and the small control it shuts into (`IssuePresets.tsx`). */
const WORDS_BOX = `input[${WORDS_FILTER_ATTRIBUTE}]`;
const WORDS_CONTROL = `button[${WORDS_FILTER_ATTRIBUTE}]`;

/** A row of the outline or of a view. */
const ROW = "[data-node-row]";

/** The outline, by its name: the Contents pane's list is a tree too. A view drawn in the outline's
 *  place is a tree named as the outline is. */
export const OUTLINE_TREE = `[role="tree"][aria-label="${CONTROL_LABELS.tree}"]`;

/** The stage's own tab stop: the tab-stop row of the outline, or of the view drawn in its place
 *  (`t c` reaches the same row, `useIssueKeymap.ts`). */
const STAGE_TAB_STOP = `${OUTLINE_TREE} ${ROW}[tabindex="0"]`;

/** The Verification view's body, which binds no Escape of its own (`VerificationFilterBody.tsx`):
 *  nothing gives a row of it back from a control inside it, so the view's Escape is next there. */
const VERIFICATION_BODY = "[data-verification-body]";

/** Documents' list's "Filter files and documents" (`DocumentsList.tsx`), the one text box an
 *  Escape leaves the list from. */
const DOCUMENTS_FILTER = `[${DOCUMENTS_LIST_ATTRIBUTE}] input[type="search"]`;

/** A box that takes typing, whose Escape is its own, as an editor's is. */
const TYPING = "input, textarea, select";

/**
 * The page's own Escape steps between a caret and the outline (dispatch://INFRA-1196 part N's R1,
 * R2, the plan's ST-2). One press does the first step whose state holds:
 *
 * 1. a caret in a row's editor is left (`useTreeKeymap.ts`'s `tree-leave-caret`), and a caret in
 *    an opened document's text by the document itself (`OpenedDocument.tsx`);
 * 2. an opened document closes (`document-close`), by its way back (`document-address.ts`'
 *    `escapeWayBack`): a document a chip opened returns to the view and the row that hold the
 *    chip, with the focus on the chip once it is drawn again (`chip-return.ts`); one Documents'
 *    list opened returns to the list, with the focus on its row; one opened by its own address,
 *    to the list, with the focus on its first row;
 * 3. on Documents' list, the focus in "Filter files and documents" with words in it clears them,
 *    and a staged upload is cancelled, as Cancel does - each the list's own Escape
 *    (`DocumentsList.tsx`), which this step leaves to it; else the list is left
 *    (`documents-leave`) for the address it was opened from (the location's `outline`, the
 *    control that was active before it), or for Conversation where the list is the first view of
 *    the load;
 * 4. the focus in the words box leaves it (`words-leave`): for the row it was on before it went
 *    to the box, else - where it was on no row, or that row is no longer drawn - for the box's
 *    Filter control where the box holds no words, which then shuts into it, and for the stage's
 *    own tab stop where it holds words, which stay; never for the page's body;
 * 5. words in `?q=`, with the focus outside the box, are cleared (`words-clear`), which shuts the
 *    box: the box's own write, a replace marked with the following it was typed in, so the view
 *    stays and the following with it (`slots/following.ts`);
 * 6. a view on is turned off (`filters-off`), which is Conversation's address; the stage then
 *    asks the outline for the node the reader was last on in the view, which it draws focused
 *    (R14, `stage-landing.tsx`'s `ViewNodeKeeper`, which `t c` and Conversation reach too);
 * 7. a row of the outline gives the outline up (`useTreeKeymap.ts`'s `tree-leave`).
 *
 * Steps 2 to 6 are these. The keymap runs the first binding of a scope whose `when` holds, and
 * the order bindings are registered in follows mount timing, so each step holds only where every
 * step above it does not: a document open, a menu open over the page (whose press it is), or the
 * focus on a control inside a row - which a tree's own Escape gives the row back from first - each
 * leaves the words' steps to the step above, and a view's rows and the outline's hold their own
 * Escape from the row itself while a view or words are on (`holdsEscape`). Details, a menu, the
 * version control's panel, the Outline sheet and a dialog each take their own Escape before any of
 * these, as on today's page: the keymap answers no press one of them has taken.
 *
 * The way back from a document a chip opened is the page's to keep (`useChipRecord`) while it is
 * drawn, as this is.
 */
export function useEscapeOrder({
  documents,
  filter,
  issueKey,
  opened,
  viewOn,
}: {
  /** Whether Documents' list is drawn in place of the outline. */
  documents: boolean;
  /** What the filter by words holds, which is `?q=`. */
  filter: string;
  issueKey: string;
  /** The route of the document opened in place of the outline, whose close comes first. */
  opened: Extract<IssueRoute, { kind: "artifact" }> | undefined;
  /** Whether a view is on. */
  viewOn: boolean;
}): void {
  const navigate = useNavigate();
  const outline = useOutlineAddress(issueKey);
  const chip = useChipRecord(issueKey);
  const followWayBack = useFollowWayBack();
  const before = useRowBeforeWords();
  // Clearing the words is the box's own write of none (`view-row-tools.tsx`' `useWriteWords`).
  const writeWords = useWriteWords(issueKey);
  const pageEscapes = () => {
    if (opened !== undefined || document.querySelector('[role="menu"]') !== null) return false;
    const active = document.activeElement;
    const row = active?.closest(ROW);
    return (
      row === null || row === undefined || row === active || row.closest(VERIFICATION_BODY) !== null
    );
  };
  useKeymap("issue", [
    {
      id: "document-close",
      keys: "Escape",
      label: CONTROL_LABELS.close_opened_document,
      palette: false,
      run: () => {
        if (opened !== undefined) {
          followWayBack(escapeWayBack(issueKey, opened.slug, outline, chip()));
        }
      },
      when: () => opened !== undefined,
    },
    {
      id: "documents-leave",
      // The list's filter takes the press where it holds no words; every other box that takes
      // typing keeps its own Escape.
      inEditable: true,
      keys: "Escape",
      label: CONTROL_LABELS.leave_documents,
      palette: false,
      run: () => navigate(outline),
      when: () => {
        if (!documents || document.querySelector('[role="menu"]') !== null) return false;
        const active = document.activeElement;
        if (!(active instanceof HTMLElement) || active.matches(DOCUMENTS_FILTER)) return true;
        return !active.isContentEditable && !active.matches(TYPING);
      },
    },
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
      run: () => writeWords(""),
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
