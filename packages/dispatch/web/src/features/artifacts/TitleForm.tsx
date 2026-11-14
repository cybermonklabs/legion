import { type FormEvent, type ReactNode, useEffect, useRef } from "react";

import { RefusableButton } from "../../components/RefusableButton";
import { dangerText, inputClasses, linkHoverText, linkText } from "../../theme/classes";
import { focusOnDocument } from "../shell/roving";

/**
 * The strip one title control holds, in both its states: closed it shows `idle`, the single
 * button the host offers; open it takes the line it is on and shows the form that types a title
 * and submits it. The control that makes a document (`CreateDocument`) and the one that corrects
 * a title (`CorrectTitle`) write the same form, and differ only in what it is called, the words
 * on its submit, and what each does with the title, all of which stay with the host.
 *
 * `problem` is the answer to the press the host has, shown where the person pressed; `refusal` is
 * why no press is taken at all, which the submit says (`RefusableButton`). `children` is what the
 * host holds in the strip beside the control, in either state.
 *
 * Only a change of `open` moves the focus. Opening puts it in the Title box, so a keyboard reader
 * types at once. Closing gives it back to `idle`, the control that opened the form - the strip's
 * first button - when the focus went with the form, which leaves it on the document, or is still
 * in the strip; a reader who has gone elsewhere on the page keeps their place, and a form drawn
 * open takes no focus.
 */
export function TitleForm({
  busyLabel,
  children,
  formLabel,
  idle,
  onCancel,
  onSubmit,
  onTitleChange,
  open,
  pending,
  problem,
  refusal,
  refusalShownBy,
  submitLabel,
  title,
}: {
  /** The label the submit carries while a press is in flight. */
  readonly busyLabel: string;
  readonly children?: ReactNode;
  /** What the form is called, which a reader reaches it by. */
  readonly formLabel: string;
  /** The control the strip holds while the form is closed. */
  readonly idle: ReactNode;
  readonly onCancel: () => void;
  readonly onSubmit: (event: FormEvent<HTMLFormElement>) => void;
  readonly onTitleChange: (title: string) => void;
  readonly open: boolean;
  readonly pending: boolean;
  /** What the write answered, or undefined while it has answered nothing. */
  readonly problem?: string;
  readonly refusal: string | null;
  /** The id of the line on screen that already says `refusal`. */
  readonly refusalShownBy?: string;
  readonly submitLabel: string;
  readonly title: string;
}): ReactNode {
  const strip = useRef<HTMLDivElement>(null);
  const titleBox = useRef<HTMLInputElement>(null);
  const wasOpen = useRef(open);
  useEffect(() => {
    if (open === wasOpen.current) return;
    wasOpen.current = open;
    if (open) {
      titleBox.current?.focus();
      return;
    }
    const node = strip.current;
    if (node !== null && (focusOnDocument() || node.contains(document.activeElement))) {
      node.querySelector("button")?.focus();
    }
  }, [open]);

  return (
    <div
      className={`flex min-w-0 flex-wrap items-center gap-2 ${open ? "w-full" : "shrink-0"}`}
      ref={strip}
    >
      {open ? (
        <form
          aria-label={formLabel}
          className="flex w-full min-w-0 flex-wrap items-center gap-2"
          onSubmit={onSubmit}
        >
          <label className="block min-w-48 flex-1">
            <span className="sr-only">Title</span>
            <input
              aria-label="Title"
              className={`block min-h-11 w-full rounded-lg border px-3 py-2 text-sm ${inputClasses(true)}`}
              onChange={(event) => onTitleChange(event.target.value)}
              placeholder="Title"
              ref={titleBox}
              required
              value={title}
            />
          </label>
          <RefusableButton
            busy={pending ? busyLabel : undefined}
            className="min-h-11 shrink-0"
            refusal={refusal}
            refusalShownBy={refusalShownBy}
            type="submit"
          >
            {submitLabel}
          </RefusableButton>
          <button
            className={`min-h-11 shrink-0 text-sm font-medium ${linkText} ${linkHoverText}`}
            disabled={pending}
            onClick={onCancel}
            type="button"
          >
            Cancel
          </button>
          {problem === undefined ? null : (
            <p className={`w-full text-sm ${dangerText}`} role="alert">
              {problem}
            </p>
          )}
        </form>
      ) : (
        idle
      )}
      {children}
    </div>
  );
}
