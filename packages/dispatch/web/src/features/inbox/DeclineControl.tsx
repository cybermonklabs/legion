import { DISPATCH_EVENT_BODY_MAX } from "@legion/contracts";
import { useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useId, useRef, useState } from "react";

import { apiErrorMessage, type DeclineAskInput, type WriteBounds } from "../../api/client";
import type { Ask, AskRead } from "../../api/types";
import { submitOnModifiedEnter } from "../../hooks/submitOnModifiedEnter";
import { useSubmitGuard } from "../../hooks/useSubmitGuard";
import {
  backdrop40,
  borderDefault,
  card,
  cardHoverBorder,
  dangerText,
  inputClasses,
  primaryButtonBg,
  primaryButtonDisabled,
  primaryButtonEnabledHoverBg,
  secondaryButtonBorder,
  secondaryButtonHoverBorder,
  secondaryButtonText,
  textMutedOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { useDialog } from "../shell/useDialog";
import { useOutstandingAnswerWrite } from "./outstanding-answers";
import { invalidateOwnerReads } from "./owner-reads";

/**
 * What **Decline to answer** opens: the person's note, which is optional, and the two controls.
 * It follows the document's **Request changes** dialog (`doc/ApprovalChip.tsx`) and adds the
 * count of what they may still write, in the UTF-16 units the server counts
 * (`DISPATCH_EVENT_BODY_MAX`). The field stops at that bound, which is where the count reaches
 * zero, so a long note meets the bound here rather than as a refusal from the server.
 */
function DeclineDialog({
  error,
  onClose,
  onSubmit,
  saving,
}: {
  /** What the server answered the last decline with, or null while none was refused. */
  error: Error | null;
  onClose: () => void;
  onSubmit: (note: string) => void;
  saving: boolean;
}): ReactNode {
  const [note, setNote] = useState("");
  const noteId = useId();
  const countId = `${noteId}-left`;
  const textareaRef = useRef<HTMLTextAreaElement>(null);
  // Every way out of the dialog is this one: Cancel, Escape (`useDialog`) and the backdrop. While
  // the decline is out it closes nothing, as the disabled Cancel button already refused to: the
  // close lets the Inbox hold go, and this dialog is the one surface the refusal that may follow
  // is shown on, so a dialog taken away mid-write would lose it.
  const close = () => {
    if (!saving) onClose();
  };
  const dialog = useDialog<HTMLDivElement>({
    initialFocusRef: textareaRef,
    onClose: close,
    open: true,
  });
  const left = DISPATCH_EVENT_BODY_MAX - note.length;

  return (
    <>
      <div aria-hidden="true" className={`fixed inset-0 z-40 ${backdrop40}`} onClick={close} />
      <div className="pointer-events-none fixed inset-0 z-40 flex items-start justify-center p-4 pt-20">
        <div
          aria-label="Decline to answer"
          aria-modal="true"
          className={`pointer-events-auto w-full max-w-sm space-y-3 rounded-lg border p-4 shadow-xl ${card}`}
          ref={dialog.containerRef}
          role="dialog"
        >
          <h2 className={`text-sm font-semibold ${textSecondaryOnSurface}`}>Decline to answer</h2>
          <p className={`text-sm ${textMutedOnSurface}`}>
            The question closes with no answer. Dispatch tells the agent that asked to decide
            without one.
          </p>
          <label className={`block text-sm font-medium ${textSecondaryOnSurface}`} htmlFor={noteId}>
            Note (optional)
            <textarea
              aria-describedby={countId}
              className={`mt-1 block w-full rounded-lg border px-2 py-1.5 text-sm font-normal outline-none ${inputClasses(false)}`}
              disabled={saving}
              id={noteId}
              maxLength={DISPATCH_EVENT_BODY_MAX}
              onChange={(event) => setNote(event.target.value)}
              onKeyDown={(event) =>
                submitOnModifiedEnter(event, {
                  onFormlessSubmit: () => {
                    if (!saving) onSubmit(note.trim());
                  },
                })
              }
              ref={textareaRef}
              value={note}
            />
          </label>
          <p className={`text-xs ${textMutedOnSurface}`} id={countId}>
            {left} {left === 1 ? "character" : "characters"} left
          </p>
          {error === null ? null : (
            <p className={`text-sm ${dangerText}`} role="alert">
              {/* The server's own words where it gave any - the note is too long, the ask is an
                  approval request, it is already closed - since each says what to do next. */}
              {apiErrorMessage(error, "Could not decline this question. Try again.")}
            </p>
          )}
          <div className="flex items-center justify-end gap-2">
            <button
              className={`rounded-lg border px-3 py-1.5 text-sm font-medium ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder}`}
              disabled={saving}
              onClick={close}
              type="button"
            >
              Cancel
            </button>
            <button
              className={`rounded-lg px-3 py-1.5 text-sm font-medium ${primaryButtonBg} ${primaryButtonEnabledHoverBg} ${primaryButtonDisabled}`}
              disabled={saving}
              onClick={() => onSubmit(note.trim())}
              type="button"
            >
              {saving ? "Declining…" : "Decline to answer"}
            </button>
          </div>
        </div>
      </div>
    </>
  );
}

/**
 * The person's **Decline to answer**: they close an open question with no answer, and Dispatch
 * tells the agent that asked to decide without one. It is drawn on an open question alone -
 * an approval request is answered with Approve or Request changes, and the server refuses a
 * decline of one - and, since every surface that shows an ask shows `AskCard`, which draws this,
 * writing it here reaches all of them: the Inbox row, the reading pane, the Conversation, a
 * document's decision block, the margin and the Answers page.
 *
 * The write takes the row out of the Inbox the moment it goes, as the answer write does, so the
 * ask is held while the server decides it (`useOutstandingAnswerWrite`): the reading column keeps
 * the question where the reader pressed, and the refusal is shown by the card that wrote it.
 * The hold is that shared store rather than a report up to the Inbox, as `SnoozeControl` makes
 * one, for the reason the store exists: this card is drawn on surfaces that take no prop of the
 * Inbox's, a document's own decision block among them.
 */
export function DeclineControl({
  ask,
  decline,
  onDeclined,
}: {
  ask: Ask;
  decline: (id: string, input: DeclineAskInput, bounds: WriteBounds) => Promise<Ask>;
  /** Called once the server has recorded the decline, as an answer from this card calls it. */
  onDeclined?: (id: string) => void;
}): ReactNode {
  const queryClient = useQueryClient();
  const submitGuard = useSubmitGuard();
  const [open, setOpen] = useState(false);
  // The dialog shows a refused decline itself, over the note it keeps, so the write is inline:
  // the shell's indicator carries it only while it is out.
  const write = useOutstandingAnswerWrite({
    askId: ask.id,
    label: () => `Decline of “${ask.question}”`,
    write: (input: DeclineAskInput, bounds) => decline(ask.id, input, bounds),
    onSettled: () => submitGuard.release(),
    onSuccess: (declined) => {
      setOpen(false);
      // The card reads its ask from the thread, so the record it swaps to is this decline.
      if (declined !== null) {
        queryClient.setQueryData<AskRead>(["ask-thread", ask.id], (current) =>
          current === undefined ? undefined : { ...current, ask: declined }
        );
      }
      void queryClient.invalidateQueries({ queryKey: ["ask-thread", ask.id] });
      invalidateOwnerReads(queryClient, ask);
      onDeclined?.(ask.id);
    },
  });

  return (
    <div className="mt-3">
      <button
        className={`min-h-11 rounded-lg border px-3 py-2 text-sm font-medium ${borderDefault} ${textSecondaryOnSurface} ${cardHoverBorder}`}
        disabled={write.isPending}
        onClick={() => setOpen(true)}
        type="button"
      >
        {write.isPending ? "Declining…" : "Decline to answer"}
      </button>
      {open ? (
        <DeclineDialog
          error={write.isError ? write.error : null}
          onClose={() => {
            write.reset();
            setOpen(false);
          }}
          onSubmit={(note) => submitGuard.guard(() => write.mutate(note === "" ? {} : { note }))}
          saving={write.isPending}
        />
      ) : null}
    </div>
  );
}
