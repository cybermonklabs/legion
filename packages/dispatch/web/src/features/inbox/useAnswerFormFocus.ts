import { useCallback, useEffect, useLayoutEffect, useRef } from "react";

import type { ApiError } from "../../api/client";
import { focusIsFreeFor } from "../doc/form-refusals";

/** Moves focus from inside the form to the form's nearest focusable ancestor instead of letting
 *  it fall to the document body, so j/k/Enter/Escape still start from the same place: whoever
 *  caused the loss, and with no scroll. In the Inbox's list layout that ancestor is the ask's own
 *  row, the reader's own place, so the focus goes to no other row; a row whose ask was answered
 *  or withdrawn elsewhere stays in the list, with the card's closed state, while it holds the
 *  focus. In the reading column it is the column's own focus stop. The Inbox's j, k and Enter
 *  answer from either. */
export function handOffFocus(form: HTMLElement): void {
  if (!form.contains(document.activeElement)) return;
  form.parentElement?.closest<HTMLElement>("[tabindex]")?.focus({ preventScroll: true });
}

/** The answer form as the ask body (`ask-body/AskBody.tsx`) holds it: the form while it is in the
 *  page, and the callback ref that the body puts on its `<form>`. */
export type AnswerForm = [
  formRef: { current: HTMLFormElement | null },
  trackForm: (form: HTMLFormElement | null) => (() => void) | undefined,
];

/** The form's callback ref. When the form leaves while a control inside it has focus - the body
 *  swapping to its record once the ask is answered, or resolved elsewhere - its cleanup runs the
 *  hand-off without any event, which moves the focus to the form's nearest focusable ancestor,
 *  the list layout's row or the reading column's focus stop, with no scroll (`handOffFocus`);
 *  React detaches a ref before removing its node, so the form is still in the document when this
 *  cleanup runs. The form is also kept in `formRef` for the disabled-control hand-off below. The
 *  callback must be stable across renders: React re-runs a changed callback ref's cleanup on
 *  every commit, and that cleanup can move focus. */
function useFormRef(): AnswerForm {
  const formRef = useRef<HTMLFormElement | null>(null);
  const track = useCallback((form: HTMLFormElement | null) => {
    formRef.current = form;
    if (form === null) return undefined;
    return () => {
      formRef.current = null;
      handOffFocus(form);
    };
  }, []);
  return [formRef, track];
}

interface AnswerFormFocusOptions {
  /** Dispatch's refusal of this card's Approve for the form of The Spec, kept for the ask. */
  formRefusal: ApiError | null;
  /** Whether the card's answer is out. */
  pending: boolean;
  /** Whether this card takes the focus that the kept refusal owes (`useAskAnswerForm`). */
  takeFormRefusalFocus: (root: Element | null) => boolean;
}

/**
 * Where the focus goes as the answer form of the ask body changes under it: the form, and the
 * form's callback ref, which the body puts on its `<form>`. The focus leaves a vanishing form for
 * the nearest focusable ancestor (`useFormRef`), leaves a control that disables itself under it
 * the same way, and comes back to the pressed control after a refusal of the form.
 */
export function useAnswerFormFocus({
  formRefusal,
  pending,
  takeFormRefusalFocus,
}: AnswerFormFocusOptions): AnswerForm {
  // A control that disables itself under the reader's focus - Ask back once its text is sent and
  // the form no longer refuses - would have the browser drop focus to the document body (in the
  // Inbox, out of the row, which then lets the row go) with nothing the body could act on
  // afterwards; the same hand-off as a vanishing form, in the commit that disables it, before the
  // browser looks. While the form refuses a press (`refusing`, `useAskAnswerForm`), its controls
  // are `aria-disabled`, not `disabled`, so this moves no focus then. The control it took the
  // focus from is kept for a refusal of the form, below.
  const [formRef, trackForm] = useFormRef();
  const handedOffFrom = useRef<HTMLElement | null>(null);
  useLayoutEffect(() => {
    const form = formRef.current;
    const active = document.activeElement;
    if (form !== null && active instanceof HTMLElement && active.matches(":disabled")) {
      handedOffFrom.current = active;
      handOffFocus(form);
    }
  });
  // A refusal of the form is an answer the reader acts on in this form, so the focus is on the
  // control they pressed once the refusal shows. The form holds its controls while the answer is
  // out (`refusing`), and a held control keeps the focus, so a focus still inside the form stays
  // where it is: on the pressed control, or where the reader moved it in the form. A control that
  // disabled itself under the focus meanwhile gets the focus back once it is enabled again. A
  // body drawn again after the press - the host took the card that wrote out of the page - holds
  // no pressed control, and takes the focus the press owes onto Answer, the press the refusal's
  // next step names, unless the reader has put the focus elsewhere; a pressed control no longer
  // in the page stands for the same kind of control in the form: Answer, or the answer field.
  // Another card of the same ask on the page shows the refusal and takes no focus. An answer that
  // lands leaves the focus where the hand-off put it, as every answered form does.
  useEffect(() => {
    if (pending) return;
    const pressed = handedOffFrom.current;
    handedOffFrom.current = null;
    if (formRefusal === null || !takeFormRefusalFocus(formRef.current)) return;
    if (pressed === null && formRef.current?.contains(document.activeElement) === true) return;
    if (pressed === null && !focusIsFreeFor(formRef.current)) return;
    const control =
      pressed?.isConnected === true
        ? pressed
        : formRef.current?.querySelector<HTMLElement>(
            pressed === null || pressed.matches('[type="submit"]')
              ? '[type="submit"]'
              : "[data-ask-answer]"
          );
    if (control?.matches(":disabled") === false) control.focus();
  }, [formRef, formRefusal, pending, takeFormRefusalFocus]);
  return [formRef, trackForm];
}
