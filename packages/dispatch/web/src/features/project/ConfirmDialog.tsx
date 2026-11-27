import { type ReactNode, useRef } from "react";

import { textPrimaryOnSurface } from "../../theme/classes";
import {
  backdrop50,
  borderDefault,
  card,
  primaryButtonBg,
  primaryButtonEnabledHoverBg,
  secondaryButtonBorder,
  secondaryButtonHoverBorder,
  secondaryButtonText,
  textSecondaryOnSurface,
} from "../../theme/shell-classes";
import { useDialog } from "../shell/useDialog";

/**
 * The page's own "are you sure", for a table action that closes an issue or that first moves an
 * epic into work: the same small modal `EpicNameDialog` uses, so it traps focus, closes on
 * Escape and gives focus back to the control the reader came from. Never `window.confirm`, which
 * a browser may suppress and which cannot name what is about to happen in the page's own words.
 */
export function ConfirmDialog({
  title,
  body,
  changes,
  confirmLabel,
  onCancel,
  onConfirm,
}: {
  /** The dialog's heading and accessible name. */
  title: string;
  /** What the action will do, in full: what it touches and what it closes. */
  body: ReactNode;
  /** Each change the action makes before the one the reader asked for, one line each. */
  changes?: readonly string[];
  confirmLabel: string;
  onCancel: () => void;
  onConfirm: () => void;
}): ReactNode {
  const confirmRef = useRef<HTMLButtonElement>(null);
  const dialog = useDialog<HTMLDivElement>({
    initialFocusRef: confirmRef,
    onClose: onCancel,
    open: true,
  });
  return (
    <>
      <div aria-hidden="true" className={`fixed inset-0 z-40 ${backdrop50}`} onClick={onCancel} />
      <div className="pointer-events-none fixed inset-0 z-50 flex items-start justify-center xl:p-4 xl:pt-20">
        <div
          aria-label={title}
          aria-modal="true"
          className={`pointer-events-auto max-h-[100dvh] w-full space-y-3 overflow-y-auto border p-4 shadow-2xl xl:max-h-[calc(100dvh-6rem)] xl:max-w-lg xl:rounded-lg ${card} ${borderDefault}`}
          ref={dialog.containerRef}
          role="dialog"
        >
          <h2 className={`text-base font-semibold ${textPrimaryOnSurface}`}>{title}</h2>
          <p className={`text-sm ${textSecondaryOnSurface}`}>{body}</p>
          {changes === undefined ? null : (
            <ul
              aria-label="Changes"
              className={`list-disc space-y-1 pl-5 text-sm ${textPrimaryOnSurface}`}
            >
              {changes.map((change) => (
                <li key={change}>{change}</li>
              ))}
            </ul>
          )}
          <div className="flex items-center justify-end gap-2">
            <button
              className={`min-h-11 rounded-lg border px-3 py-2 text-sm font-medium ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder}`}
              onClick={onCancel}
              type="button"
            >
              Cancel
            </button>
            <button
              className={`min-h-11 rounded-lg px-3 py-2 text-sm font-semibold ${primaryButtonBg} ${primaryButtonEnabledHoverBg}`}
              onClick={onConfirm}
              ref={confirmRef}
              type="button"
            >
              {confirmLabel}
            </button>
          </div>
        </div>
      </div>
    </>
  );
}
