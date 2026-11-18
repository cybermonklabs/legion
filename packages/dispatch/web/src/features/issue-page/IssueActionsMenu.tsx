import { type ReactNode, useEffect, useId, useRef, useState } from "react";

import {
  pageHairBorder,
  pageHeaderControl,
  pageInkText,
  pagePaperBg,
  pageRowHoverBg,
  secondaryButtonDisabledText,
} from "../../theme/classes";
import { pageChrome } from "./page-structure";
import { CONTROL_LABELS } from "./page-words";

/**
 * The header's one overflow menu: the pin, close, reopen, and the control that gives the issue a
 * parent. It holds actions, never a property: every property with a value is drawn, and a
 * property with none is reached from Details' one add control, since a "more" that hides a
 * property is not allowed. It is the prototype's `#more-btn` and `#menu`.
 *
 * Each action is a plain `button`, in a group rather than a `role="menu"` of `menuitem`s: these
 * are the controls the old header drew as buttons, and a reader - or the check that reads every
 * control the old page offered - finds them under the same role. The words are not all the old
 * ones: reopening now reads "Reopen into Backlog" (`CONTROL_LABELS.reopen_into_backlog`),
 * because a control says what it does where a tooltip used to and its words are its name
 * (`IssueHeader.tsx`, WCAG 2.5.3).
 *
 * The group is drawn while it is shut too, hidden, so the keys that act on one of its controls -
 * `Shift+P`, the pin (`useHeaderKeys.ts`) - press the same control a reader presses.
 *
 * Escape, and an action pressed from the open group, give the focus back to the group's control,
 * 'Issue actions', as the ARIA menu-button pattern does (WCAG 2.4.3): the group hides the focused
 * action as it closes, and with no control focused the browser would leave the focus on the
 * page's body, so the next Tab would start again from the top of the page. A press outside the
 * group leaves the focus where the press lands, and `Shift+P`, which presses the hidden pin while
 * the group is shut, leaves it where it was.
 */
export function IssueActionsMenu({ actions }: { actions: readonly IssueAction[] }): ReactNode {
  const [open, setOpen] = useState(false);
  const panelId = useId();
  const wrapper = useRef<HTMLDivElement>(null);
  const control = useRef<HTMLButtonElement>(null);
  // A press outside the group, or Escape, closes it: an open group would otherwise cover the
  // page's own first row until the reader pressed the control again.
  useEffect(() => {
    if (!open) return;
    const closeOutside = (event: PointerEvent) => {
      if (!(event.target instanceof Node) || wrapper.current?.contains(event.target) !== true) {
        setOpen(false);
      }
    };
    // The press is the open group's alone, as a menu's is: the page's own Escape - which closes
    // a document opened in place of the outline (`OpenedDocument.tsx`) - waits for the next one.
    const closeOnEscape = (event: KeyboardEvent) => {
      if (event.key !== "Escape") return;
      event.preventDefault();
      setOpen(false);
      control.current?.focus();
    };
    document.addEventListener("pointerdown", closeOutside);
    document.addEventListener("keydown", closeOnEscape);
    return () => {
      document.removeEventListener("pointerdown", closeOutside);
      document.removeEventListener("keydown", closeOnEscape);
    };
  }, [open]);

  return (
    // A flex box, so the control is the wrapper's whole height: as an inline box in a block it
    // sat on a text line 2 px taller than itself, and the header's first row with it.
    <div className="relative flex shrink-0" ref={wrapper}>
      <button
        aria-controls={panelId}
        aria-expanded={open}
        aria-label={CONTROL_LABELS.issue_actions}
        className={pageHeaderControl}
        onClick={() => setOpen((shown) => !shown)}
        ref={control}
        type="button"
      >
        <span aria-hidden="true" {...pageChrome}>
          ⋯
        </span>
      </button>
      {/* The panel hangs from a zero-size anchor at the wrapper's bottom left, so it opens under
          the control: placed from the flex wrapper itself, its top was the wrapper's top and it
          covered the control, which then could not be pressed again to close it. At 760 px and
          below the panel is fixed at the anchor's height and spans the screen less the page's
          14 px insets, so it never reaches past the right edge; from 761 px it is absolute
          at the anchor, at the prototype's menu width. Each action keeps `main`'s 44 px floor
          below 1280 px, where that floor applies, as a menu a header control opens does. */}
      <div className="absolute top-full left-0">
        <div
          className={`fixed inset-x-3.5 z-30 mt-1 flex flex-col rounded-[10px] border p-1.5 shadow-[0_12px_30px_rgba(28,25,23,0.18)] min-[761px]:absolute min-[761px]:inset-x-auto min-[761px]:top-0 min-[761px]:left-0 min-[761px]:w-64 ${pageHairBorder} ${pagePaperBg}`}
          hidden={!open}
          id={panelId}
        >
          {actions.map((action) => (
            <button
              aria-pressed={action.pressed}
              data-issue-action={action.id}
              className={`flex min-h-11 w-full items-center rounded-[7px] px-2 py-[7px] text-left text-sm font-medium min-[1280px]:min-h-0 ${pageRowHoverBg} ${pageInkText} ${secondaryButtonDisabledText}`}
              disabled={action.disabled}
              key={action.id}
              onClick={() => {
                // `Shift+P` presses the hidden pin while the group is shut (`useHeaderKeys.ts`):
                // that press moves no focus.
                const fromTheMenu = open;
                setOpen(false);
                action.run();
                if (fromTheMenu) control.current?.focus();
              }}
              type="button"
              {...pageChrome}
            >
              {action.text}
            </button>
          ))}
        </div>
      </div>
    </div>
  );
}

/** One action of the issue: the words the control shows a reader, and what it does. The words
 *  are its name too - the control carries no `aria-label` - so a voice user who says what it
 *  reads presses it (WCAG 2.5.3, axe's `label-content-name-mismatch`). Reopening says which
 *  status it reopens into, where a tooltip used to, and that whole sentence is the name. A
 *  toggle - the pin - says what is so with `pressed`. */
export interface IssueAction {
  readonly disabled?: boolean;
  /** Which action this is, apart from the words it shows, which change as it saves. It is the
   *  control's `data-issue-action`, which a key finds the control by (`Shift+P` presses
   *  `button[data-issue-action="pin"]`, `useHeaderKeys.ts`), so the four ids are a contract. */
  readonly id: "pin" | "close" | "reopen" | "parent";
  readonly pressed?: boolean;
  readonly run: () => void;
  readonly text: string;
}
