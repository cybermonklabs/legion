import { DISPATCH_EVENT_BODY_MAX, dispatchWritingBox } from "@legion/contracts";
import { useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useId, useRef, useState } from "react";
import { createPortal } from "react-dom";

import { apiErrorMessage, type DeclineAskInput, type WriteBounds } from "../../api/client";
import { replaceQueryData } from "../../api/query-client";
import type { Ask, AskRead } from "../../api/types";
import { submitOnModifiedEnter } from "../../hooks/submitOnModifiedEnter";
import { useSubmitGuard } from "../../hooks/useSubmitGuard";
import {
  backdrop40,
  borderDefault,
  card,
  cardHoverBorder,
  dangerText,
  primaryButtonBg,
  primaryButtonDisabled,
  primaryButtonEnabledHoverBg,
  secondaryButtonBorder,
  secondaryButtonHoverBorder,
  secondaryButtonText,
  textMutedOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { MentionField } from "../conversation/MentionField";
import { useDialog } from "../shell/useDialog";
import { SendStoppedError, stoppedWriteNote } from "../writes/keyed-write";
import { type AskPress, useOutstandingAnswerWrite } from "./outstanding-answers";
import { invalidateOwnerReads } from "./owner-reads";
import { useQuestionLabel } from "./question-label";

/**
 * What **Decline to answer** opens: the person's note, which is optional, and the two controls.
 * It follows the document's **Request changes** dialog (`doc/ApprovalChip.tsx`) - the shared
 * field and the picker in it, so a note names a party as every other box does - and adds the
 * count of what they may still write, in the UTF-16 units the server counts
 * (`DISPATCH_EVENT_BODY_MAX`), which the field names as its own description so a reader who
 * cannot see it is told it too. Typing stops at that bound, where the count reaches zero. A
 * pick writes into the box without typing, so a reference taken near the bound can pass it:
 * the count then reads below zero and **Decline to answer** is held until the note is shorter,
 * so the bound is met here rather than as a refusal from the server.
 */
function DeclineDialog({
  error,
  inDocumentRoot,
  issueKey,
  onClose,
  onSubmit,
  project,
  saving,
}: {
  /** What the server answered the last decline with, or null while none was refused. */
  error: Error | null;
  /** Draws the dialog into `document.body` rather than where it is mounted (`DeclineControl`'s
   *  `look`). */
  inDocumentRoot: boolean;
  /** The issue the question was asked on, where it has one: the picker offers that issue's
   *  people. */
  issueKey?: string;
  onClose: () => void;
  onSubmit: (note: string) => void;
  /** The project of the document the question hangs from, whose components the picker offers. */
  project?: string;
  saving: boolean;
}): ReactNode {
  const [note, setNote] = useState("");
  const countId = useId();
  // The dialog moves the focus into the box it opens over, which the field hands back as its
  // own element (`MentionFieldHandle`).
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
  // One answer for both ways out of this box, the button and Ctrl/Cmd+Enter: a decline is sent
  // while none is out and the note is within the bound a pick may have carried it past.
  const canSend = !saving && left >= 0;

  const drawn = (
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
          <MentionField
            body={note}
            className="text-sm"
            describedBy={[countId]}
            disabled={saving}
            fieldRef={(handle) => {
              textareaRef.current = handle?.textarea() ?? null;
            }}
            issueKey={issueKey}
            label="Note (optional)"
            maxLength={DISPATCH_EVENT_BODY_MAX}
            onChange={(change) => setNote(change.body)}
            onKeyDown={(event) =>
              submitOnModifiedEnter(event, {
                onFormlessSubmit: () => {
                  if (canSend) onSubmit(note.trim());
                },
              })
            }
            project={project}
            writing={dispatchWritingBox("ask-decline-note", "ask-decline")}
          />
          <p className={`text-xs ${textMutedOnSurface}`} id={countId}>
            {left} {left === 1 ? "character" : "characters"} left
          </p>
          {error === null ? null : (
            <p className={`text-sm ${dangerText}`} role="alert">
              {/* The server's own words where it gave any - the note is too long, the ask is an
                  approval request, it is already closed - since each says what to do next. A
                  decline the reader stopped may have been made, and the button below is a press
                  of its own, under a key of its own. */}
              {error instanceof SendStoppedError
                ? stoppedWriteNote("your decline", "made", {
                    anew: "Pressing Decline to answer again sends a new decline.",
                  })
                : apiErrorMessage(error, "Could not decline this question. Try again.")}
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
              disabled={!canSend}
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
  return inDocumentRoot ? createPortal(drawn, document.body) : drawn;
}

/** One press of the decline: the ask it is of (`AskPress`), what it sends, and its question as
 *  the label names it (`questionWords`). */
interface DeclinePress extends AskPress {
  readonly input: DeclineAskInput;
  readonly questionWords: string;
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
  look,
  onDeclined,
}: {
  ask: Ask;
  decline: (id: string, input: DeclineAskInput, bounds: WriteBounds) => Promise<Ask>;
  /** The control's own box and the classes of its button, where a host draws it in a look of its
   *  own: the issue page's answer line draws it as one of its quiet controls, inside the box held
   *  at the screen's foot. There the dialog is drawn into `document.body`, so it is no part of the
   *  answer's form - Ctrl/Cmd+Enter in its note declines rather than sending the answer - and it
   *  stands over the box's sticky layer rather than inside it. React still bubbles its keys to the
   *  host's handlers, which leave a key inside `[role="dialog"]` to it. A card's own look, and the
   *  dialog in place, otherwise. */
  look?: { readonly box: string; readonly button: string };
  /** Called once the server has recorded the decline, as an answer from this card calls it. */
  onDeclined?: (id: string) => void;
}): ReactNode {
  const queryClient = useQueryClient();
  const submitGuard = useSubmitGuard();
  const [open, setOpen] = useState(false);
  const question = useQuestionLabel(ask);
  // The dialog shows a refused decline itself, over the note it keeps, so the write is inline:
  // the shell's indicator carries it only while it is out. Each press carries the ask it
  // declines (`AskPress`), so its landing reads again the ask it was made on, whichever ask the
  // card is drawn for by then. Its label holds the question's plain-text projection, the words
  // the card draws: the reading list's `write-label` (`DISPATCH_READING_SURFACES`) says why. The
  // press carries that line as it reads at the press, so it names its own ask's question too.
  const write = useOutstandingAnswerWrite({
    label: (press: DeclinePress) => `Decline of “${press.questionWords}”`,
    method: "POST",
    write: (press: DeclinePress, bounds) => decline(press.ask.id, press.input, bounds),
    onSettled: () => submitGuard.release(),
    onSuccess: (declined, press) => {
      setOpen(false);
      // The card reads its ask from the thread, so the record it swaps to is this decline, over
      // any read of the thread still out, which was asked before it and would put back the open
      // ask (`replaceQueryData`).
      if (declined !== null) {
        replaceQueryData<AskRead>(queryClient, ["ask-thread", press.ask.id], (current) =>
          current === undefined ? undefined : { ...current, ask: declined }
        );
      }
      void queryClient.invalidateQueries({ queryKey: ["ask-thread", press.ask.id] });
      invalidateOwnerReads(queryClient, press.ask);
      onDeclined?.(press.ask.id);
    },
  });

  return (
    <div className={look?.box ?? "mt-3"}>
      {question.reads}
      <button
        className={
          look?.button ??
          `min-h-11 rounded-lg border px-3 py-2 text-sm font-medium ${borderDefault} ${textSecondaryOnSurface} ${cardHoverBorder}`
        }
        disabled={write.isPending}
        onClick={() => setOpen(true)}
        type="button"
      >
        {write.isPending ? "Declining…" : "Decline to answer"}
      </button>
      {open ? (
        <DeclineDialog
          error={write.isError ? write.error : null}
          inDocumentRoot={look !== undefined}
          issueKey={ask.issue_key ?? undefined}
          onClose={() => {
            write.reset();
            setOpen(false);
          }}
          onSubmit={(note) =>
            submitGuard.guard(() =>
              write.mutate({
                ask,
                input: note === "" ? {} : { note },
                questionWords: question.line(),
              })
            )
          }
          project={ask.anchor_artifact?.project}
          saving={write.isPending}
        />
      ) : null}
    </div>
  );
}
