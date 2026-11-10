import { type ReactNode, useEffect, useId, useRef, useState } from "react";

import {
  borderDefault,
  card,
  focusVisibleRing,
  secondaryButtonBorder,
  secondaryButtonDisabledText,
  secondaryButtonHoverBorder,
  secondaryButtonText,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { pageChrome } from "./page-structure";
import { CONTROL_LABELS } from "./page-words";

/**
 * The one control row one keeps the issue's actions behind: close, reopen, and the control that
 * gives the issue a parent (dispatch://INFRA-1196, the identity block's row one). It holds
 * actions, never a property: every property with a value is drawn, and a property with none is
 * reached from the add control (R11, which bans a "more" that hides a property).
 *
 * Each action is a plain `button`, in a labelled group rather than a `role="menu"` of
 * `menuitem`s: these are the controls the old header drew as buttons, and a reader - or the
 * check that reads every control the old page offered - finds them under the same role and the
 * same name as before.
 */
export function IssueActionsMenu({ actions }: { actions: readonly IssueAction[] }): ReactNode {
  const [open, setOpen] = useState(false);
  const panelId = useId();
  const wrapper = useRef<HTMLDivElement>(null);
  // A press outside the group, or Escape, closes it: an open group would otherwise cover the
  // page's own first row until the reader pressed the control again.
  useEffect(() => {
    if (!open) return;
    const closeOutside = (event: PointerEvent) => {
      if (!(event.target instanceof Node) || wrapper.current?.contains(event.target) !== true) {
        setOpen(false);
      }
    };
    const closeOnEscape = (event: KeyboardEvent) => {
      if (event.key === "Escape") setOpen(false);
    };
    document.addEventListener("pointerdown", closeOutside);
    document.addEventListener("keydown", closeOnEscape);
    return () => {
      document.removeEventListener("pointerdown", closeOutside);
      document.removeEventListener("keydown", closeOnEscape);
    };
  }, [open]);

  if (actions.length === 0) return null;
  return (
    <div className="relative shrink-0" ref={wrapper}>
      <button
        aria-controls={panelId}
        aria-expanded={open}
        aria-label={CONTROL_LABELS.issue_actions}
        className={`min-h-11 rounded-lg px-2 text-sm font-medium md:min-h-8 ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder} ${focusVisibleRing}`}
        onClick={() => setOpen((shown) => !shown)}
        type="button"
      >
        <span aria-hidden="true" {...pageChrome}>
          ⋯
        </span>
      </button>
      {open ? (
        <div
          className={`absolute right-0 z-30 mt-1 flex w-56 flex-col gap-1 rounded-lg border p-2 shadow-lg ${borderDefault} ${card}`}
          id={panelId}
        >
          {actions.map((action) => (
            <button
              className={`min-h-11 rounded-lg px-2 py-2 text-left text-sm font-medium ${textSecondaryOnSurface} ${secondaryButtonDisabledText}`}
              disabled={action.disabled}
              key={action.text}
              onClick={() => {
                setOpen(false);
                action.run();
              }}
              type="button"
              {...pageChrome}
            >
              {action.text}
            </button>
          ))}
        </div>
      ) : null}
    </div>
  );
}

/** One action of the issue: the words the control shows a reader, and what it does. The words
 *  are its name too - the control carries no `aria-label` - so a voice user who says what it
 *  reads presses it (WCAG 2.5.3, axe's `label-content-name-mismatch`). Reopening says which
 *  status it reopens into, where a tooltip used to (R11), and that whole sentence is the name. */
export interface IssueAction {
  readonly disabled?: boolean;
  readonly run: () => void;
  readonly text: string;
}
