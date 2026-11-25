import { type KeyboardEvent, type ReactNode, useRef, useState } from "react";

import { useTooltips } from "../../components/tooltips";
import { inputMethodKey } from "../../lib/input-method-key";
import {
  backdrop40,
  card,
  dangerText,
  inputClasses,
  primaryButtonBg,
  primaryButtonDisabled,
  primaryButtonEnabledHoverBg,
  secondaryButtonBorder,
  secondaryButtonHoverBorder,
  secondaryButtonText,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { useDialog } from "../shell/useDialog";
import { SendStoppedError, stoppedWriteNote } from "../writes/keyed-write";

export interface NameVersionDialogProps {
  /** What the last Save was answered with, or null while none failed. */
  error: Error | null;
  onClose: () => void;
  onSave: (summary: string) => void;
  open: boolean;
  saving: boolean;
}

/**
 * The "Name version" toolbar action's confirmation dialog — an in-app dialog rather than
 * `window.prompt`, matching the app's own focus-trap/Escape/backdrop dialog
 * convention (see ReferencePicker for the same shape). Unmounts entirely while closed so
 * `useDialog`'s `open` effect (focus trap, Escape, scroll lock) only ever runs while a caller
 * actually wants it showing.
 */
export function NameVersionDialog({
  error,
  onClose,
  onSave,
  open,
  saving,
}: NameVersionDialogProps): ReactNode {
  const [summary, setSummary] = useState("");
  const inputRef = useRef<HTMLInputElement>(null);
  const dialog = useDialog<HTMLDivElement>({ initialFocusRef: inputRef, onClose, open });
  // Inside the new issue page's `NoTooltips`, the one place that draws this dialog in a
  // document opened in place (decisions 223, 245 and 362), a name being saved leaves Save and the
  // text box focusable: the lines spread after their kept `disabled` below mark them busy and
  // refused, and `commit` ignores their press. A disabled control drops the focus to the page's
  // body. The caller keeps `saving` until the versions' re-read is drawn, and the dialog's close
  // then gives the focus back once, to its opener (`useDialog`). Today's pages keep `disabled`.
  const tooltips = useTooltips();

  if (!open) {
    return null;
  }

  const trimmed = summary.trim();
  /** What the box and Save are inside `NoTooltips`, over their `disabled`: busy and refused while
   *  the name is saved, never disabled for it. The box takes no typing meanwhile, and Save stays
   *  disabled with nothing to save, as today. Outside, nothing, which keeps today's `disabled`. */
  const boxWhileSaving = tooltips
    ? {}
    : {
        "aria-busy": saving || undefined,
        "aria-disabled": saving || undefined,
        disabled: false,
        readOnly: saving,
      };
  const saveWhileSaving = tooltips
    ? {}
    : {
        "aria-busy": saving || undefined,
        "aria-disabled": saving || undefined,
        disabled: trimmed === "",
      };

  const commit = (): void => {
    if (trimmed === "" || saving) {
      return;
    }
    onSave(trimmed);
  };

  const handleInputKeyDown = (event: KeyboardEvent<HTMLInputElement>): void => {
    // The Enter that ends an input method's composition takes the word composed, and is no
    // Enter of the reader's: it saves nothing. Safari sends it after its compositionend, with
    // `isComposing` false and keyCode 229 (`inputMethodKey`).
    if (event.key !== "Enter" || inputMethodKey(event.nativeEvent)) {
      return;
    }
    event.preventDefault();
    commit();
  };

  return (
    <>
      <div aria-hidden="true" className={`fixed inset-0 z-40 ${backdrop40}`} onClick={onClose} />
      <div className="pointer-events-none fixed inset-0 z-40 flex items-start justify-center p-4 pt-20">
        <div
          aria-label="Name this version"
          aria-modal="true"
          className={`pointer-events-auto w-full max-w-sm space-y-3 rounded-lg border p-4 shadow-xl ${card}`}
          ref={dialog.containerRef}
          role="dialog"
        >
          <h2 className={`text-sm font-semibold ${textSecondaryOnSurface}`}>Name this version</h2>
          <label className={`block text-sm font-medium ${textSecondaryOnSurface}`}>
            What changed?
            <input
              className={`mt-1 block w-full rounded-lg border px-2 py-1.5 text-sm font-normal outline-none ${inputClasses(false)}`}
              disabled={saving}
              {...boxWhileSaving}
              onChange={(event) => setSummary(event.target.value)}
              onKeyDown={handleInputKeyDown}
              ref={inputRef}
              type="text"
              value={summary}
            />
          </label>
          {error === null ? null : (
            <p className={`text-sm ${dangerText}`} role="alert">
              {/* A version the reader stopped may have been named, and Save is a press of its
                  own, under a key of its own. */}
              {error instanceof SendStoppedError
                ? stoppedWriteNote("the new version", "made", {
                    anew: "Saving it again names a new version.",
                  })
                : "Could not name this version. Try again."}
            </p>
          )}
          <div className="flex items-center justify-end gap-2">
            <button
              className={`rounded-lg border px-3 py-1.5 text-sm font-medium ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder}`}
              onClick={onClose}
              type="button"
            >
              Cancel
            </button>
            <button
              className={`rounded-lg px-3 py-1.5 text-sm font-medium ${primaryButtonBg} ${primaryButtonEnabledHoverBg} ${primaryButtonDisabled}`}
              disabled={trimmed === "" || saving}
              {...saveWhileSaving}
              onClick={commit}
              type="button"
            >
              {saving ? "Saving…" : "Save"}
            </button>
          </div>
        </div>
      </div>
    </>
  );
}
