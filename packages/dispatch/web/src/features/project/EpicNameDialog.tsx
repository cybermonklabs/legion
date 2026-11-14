import { ISSUE_TITLE_MAX } from "@legion/contracts/dispatch-api-constants";
import { type FormEvent, type ReactNode, useRef, useState } from "react";

import {
  backdrop50,
  borderDefault,
  card,
  inputClasses,
  primaryButtonBg,
  primaryButtonDisabled,
  primaryButtonEnabledHoverBg,
  secondaryButtonBorder,
  secondaryButtonHoverBorder,
  secondaryButtonText,
  textPrimaryOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { useDialog } from "../shell/useDialog";

/**
 * Naming an epic, in the page: the same small modal create-issue uses, so naming and renaming
 * an epic trap focus, close on Escape and return focus to the row the reader came from. A
 * browser may suppress `window.prompt` outright, and the prompt took the page with it.
 */
export function EpicNameDialog({
  title,
  initial = "",
  submitLabel,
  onCancel,
  onSubmit,
}: {
  /** The dialog's own heading and accessible name. */
  title: string;
  initial?: string;
  submitLabel: string;
  onCancel: () => void;
  onSubmit: (name: string) => void;
}): ReactNode {
  const nameRef = useRef<HTMLInputElement>(null);
  const dialog = useDialog<HTMLFormElement>({
    initialFocusRef: nameRef,
    onClose: onCancel,
    open: true,
  });
  const [name, setName] = useState(initial);
  const trimmed = name.trim();
  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (trimmed !== "") onSubmit(trimmed);
  };
  return (
    <>
      <div aria-hidden="true" className={`fixed inset-0 z-40 ${backdrop50}`} onClick={onCancel} />
      <div className="pointer-events-none fixed inset-0 z-50 flex items-start justify-center xl:p-4 xl:pt-20">
        <form
          aria-label={title}
          aria-modal="true"
          className={`pointer-events-auto max-h-[100dvh] w-full space-y-3 overflow-y-auto border p-4 shadow-2xl xl:max-h-[calc(100dvh-6rem)] xl:max-w-lg xl:rounded-lg ${card} ${borderDefault}`}
          onSubmit={submit}
          ref={dialog.containerRef}
          role="dialog"
        >
          <h2 className={`text-base font-semibold ${textPrimaryOnSurface}`}>{title}</h2>
          <label className={`block text-sm font-medium ${textSecondaryOnSurface}`}>
            Name
            <input
              className={`mt-1 block min-h-11 w-full rounded-lg border px-3 py-2 text-sm font-normal outline-none ${inputClasses(true)}`}
              maxLength={ISSUE_TITLE_MAX}
              onChange={(event) => setName(event.target.value)}
              ref={nameRef}
              type="text"
              value={name}
            />
          </label>
          <div className="flex items-center justify-end gap-2">
            <button
              className={`min-h-11 rounded-lg border px-3 py-2 text-sm font-medium ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder}`}
              onClick={onCancel}
              type="button"
            >
              Cancel
            </button>
            <button
              className={`min-h-11 rounded-lg px-3 py-2 text-sm font-semibold ${primaryButtonBg} ${primaryButtonEnabledHoverBg} ${primaryButtonDisabled}`}
              disabled={trimmed === ""}
              type="submit"
            >
              {submitLabel}
            </button>
          </div>
        </form>
      </div>
    </>
  );
}
