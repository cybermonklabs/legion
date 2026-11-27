import { DISPATCH_EVENT_BODY_MAX, dispatchWritingBox } from "@legion/contracts";
import { useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useContext, useId, useRef, useState } from "react";

import { apiErrorMessage, type DeclineAskInput, type WriteBounds } from "../../api/client";
import { replaceQueryData } from "../../api/query-client";
import type { Ask, AskRead } from "../../api/types";
import { heldPress } from "../../components/held-press";
import { QueryError } from "../../components/QueryError";
import { submitOnModifiedEnter } from "../../hooks/submitOnModifiedEnter";
import { type SubmitGuard, useSubmitGuard } from "../../hooks/useSubmitGuard";
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
import type { AskPress } from "./ask-writes";
import {
  AskDeclineWritesContext,
  type OutstandingAnswerWrite,
  type OutstandingAnswerWriteOptions,
  useInboxAnswerWrite,
  useOwnAnswerWrite,
} from "./outstanding-answers";
import { invalidateOwnerReads } from "./owner-reads";
import { useQuestionLabel } from "./question-label";

/**
 * What a refused decline says: the server's own words where it gave any - the note is too long,
 * the ask is an approval request, it is already closed - since each says what to do next. A
 * decline the reader stopped may have been made, and Decline to answer is a press of its own,
 * under a key of its own. The dialog says it, and inside an Inbox so do the card drawn for the
 * ask again and the ask's short row (`AskShortRow`).
 */
export function declineRefusal(error: Error): string {
  return error instanceof SendStoppedError
    ? stoppedWriteNote("your decline", "made", {
        anew: "Pressing Decline to answer again sends a new decline.",
      })
    : apiErrorMessage(error, "Could not decline this question. Try again.");
}

/**
 * What **Decline to answer** opens: the person's note, which is optional, and the two controls.
 * It follows the document's **Request changes** dialog (`doc/ApprovalChip.tsx`) - the shared
 * field and the picker in it, so a note names a party as every other box does - and adds the
 * count of what they may still write, in the UTF-16 units the server counts
 * (`DISPATCH_EVENT_BODY_MAX`), which the field names as its own description so a reader who
 * cannot see it is told it too. Typing stops at that bound, where the count reaches zero. A
 * pick writes into the box without typing, so a reference taken near the bound can pass it:
 * the count then reads below zero and **Decline to answer** is disabled until the note is
 * shorter, so the bound is met here rather than as a refusal from the server. While the
 * decline is out the note and both controls are held (`held-press.ts`), never disabled: the one
 * the reader pressed keeps the focus, the dialog's Tab cycle keeps all three, so a Tab never
 * leaves the open dialog, and a press of either control does nothing.
 */
function DeclineDialog({
  error,
  initialNote,
  issueKey,
  onClose,
  onSubmit,
  project,
  saving,
}: {
  /** What the server answered the last decline with, or null while none was refused. */
  error: Error | null;
  /** The note the dialog opens with: the refused decline's, so pressing Decline to answer again
   *  sends the same decline. */
  initialNote: string;
  /** The issue the question was asked on, where it has one: the picker offers that issue's
   *  people. */
  issueKey?: string;
  onClose: () => void;
  onSubmit: (note: string) => void;
  /** The project of the document the question hangs from, whose components the picker offers. */
  project?: string;
  saving: boolean;
}): ReactNode {
  const [note, setNote] = useState(initialNote);
  const countId = useId();
  // The dialog moves the focus into the box it opens over, which the field hands back as its
  // own element (`MentionFieldHandle`).
  const textareaRef = useRef<HTMLTextAreaElement>(null);
  // Every way out of the dialog is this one: Cancel, Escape (`useDialog`) and the backdrop. While
  // the decline is out it closes nothing, as the held Cancel button refuses to: the dialog shows
  // the refusal that may follow over the note it keeps, and outside an Inbox it is the one
  // surface that refusal is shown on.
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
          <MentionField
            body={note}
            className="text-sm"
            describedBy={[countId]}
            fieldRef={(handle) => {
              textareaRef.current = handle?.textarea() ?? null;
            }}
            held={saving}
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
            readOnly={saving}
            writing={dispatchWritingBox("ask-decline-note", "ask-decline")}
          />
          <p className={`text-xs ${textMutedOnSurface}`} id={countId}>
            {left} {left === 1 ? "character" : "characters"} left
          </p>
          {error === null ? null : (
            <p className={`text-sm ${dangerText}`} role="alert">
              {declineRefusal(error)}
            </p>
          )}
          <div className="flex items-center justify-end gap-2">
            <button
              className={`rounded-lg border px-3 py-1.5 text-sm font-medium ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder}`}
              {...heldPress(saving, close)}
              type="button"
            >
              Cancel
            </button>
            <button
              className={`rounded-lg px-3 py-1.5 text-sm font-medium ${primaryButtonBg} ${primaryButtonEnabledHoverBg} ${primaryButtonDisabled}`}
              disabled={left < 0}
              {...heldPress(saving, () => onSubmit(note.trim()))}
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
 * The write takes the row out of the Inbox the moment it goes, as the answer write does
 * (`useOutstandingAnswerWrite`). Inside an Inbox the Inbox holds the write, by ask, as it holds
 * each row's snooze (`useAskSnoozeWrites`), and keeps the row where the reader saw it while the
 * write is out, though the reading column's card goes whenever the reader moves the selection.
 * The write and its refusal outlive the card: the card drawn for the ask again says the refusal
 * under Decline to answer, which opens the dialog on the refused decline's note, and the ask's
 * short row says it too. It reaches the Inbox through the context the Inbox provides, since this
 * card is drawn on surfaces that take no prop of the Inbox's, a document's own decision block
 * among them (`useInboxAnswerWrite`). Outside an Inbox the control makes its own write
 * (`OwnDeclineControl`), and the dialog is the one surface a refusal is shown on.
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
  // the shell's indicator carries it only while it is out. Each press carries the ask it
  // declines (`AskPress`), so its landing reads again the ask it was made on, whichever ask the
  // card is drawn for by then. Its label holds the question's plain-text projection, the words
  // the card draws: the reading list's `write-label` (`DISPATCH_READING_SURFACES`) says why. The
  // press carries that line as it reads at the press, so it names its own ask's question too.
  const options: OutstandingAnswerWriteOptions<Ask, DeclinePress> = {
    label: (press) => `Decline of “${press.questionWords}”`,
    method: "POST",
    write: (press, bounds) => decline(press.ask.id, press.input, bounds),
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
  };
  const inbox = useInboxAnswerWrite(useContext(AskDeclineWritesContext), ask.id, options);
  const drawn = { ask, open, setOpen, submitGuard };
  return inbox === undefined ? (
    <OwnDeclineControl {...drawn} options={options} />
  ) : (
    <DeclineButton {...drawn} write={inbox} />
  );
}

/** What `DeclineButton` draws from beside the write: the ask, whether the dialog is open, and
 *  the guard that lets one press through at a time. */
interface DeclineDrawn {
  readonly ask: Ask;
  readonly open: boolean;
  readonly setOpen: (open: boolean) => void;
  readonly submitGuard: SubmitGuard;
}

/** The decline of a card drawn outside an Inbox, as the control's own write
 *  (`useOwnAnswerWrite`). */
function OwnDeclineControl({
  options,
  ...drawn
}: DeclineDrawn & { options: OutstandingAnswerWriteOptions<Ask, DeclinePress> }): ReactNode {
  return <DeclineButton {...drawn} write={useOwnAnswerWrite(options)} />;
}

/** **Decline to answer**, the refusal under it and the dialog it opens, drawn from `write`: the
 *  Inbox's decline of the ask, or the control's own. The button is held while the decline is out
 *  (`heldPress`), never disabled, so the dialog that closes hands the focus back to it. */
function DeclineButton({
  ask,
  open,
  setOpen,
  submitGuard,
  write,
}: DeclineDrawn & { write: OutstandingAnswerWrite<DeclinePress> }): ReactNode {
  // The question as the press's label names it (`DeclinePress`'s `questionWords`), read where
  // the press is made.
  const question = useQuestionLabel(ask);
  return (
    <div className="mt-3">
      {question.reads}
      <button
        className={`min-h-11 rounded-lg border px-3 py-2 text-sm font-medium ${borderDefault} ${textSecondaryOnSurface} ${cardHoverBorder}`}
        {...heldPress(write.isPending, () => setOpen(true))}
        type="button"
      >
        {write.isPending ? "Declining…" : "Decline to answer"}
      </button>
      {/* A decline refused while the dialog was not drawn: the Inbox holds the write, and this
          card was drawn for the ask again after the reader moved away. */}
      {open || write.error === null ? null : (
        <div className="mt-2">
          <QueryError message={declineRefusal(write.error)} />
        </div>
      )}
      {open ? (
        <DeclineDialog
          error={write.isError ? write.error : null}
          initialNote={write.error === null ? "" : (write.variables?.input.note ?? "")}
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
