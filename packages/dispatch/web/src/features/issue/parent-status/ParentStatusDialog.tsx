import { type ReactNode, useId, useRef } from "react";

import type { StatusChangeEntry } from "../../../api/types";
import {
  backdrop50,
  borderDefault,
  card,
  dangerText,
  secondaryButtonBorder,
  secondaryButtonDisabledText,
  secondaryButtonHoverBorder,
  secondaryButtonText,
  textMutedOnSurface,
  textPrimaryOnSurface,
  textSecondaryOnSurface,
} from "../../../theme/classes";
import { IN_WORK_STATUSES, statusText } from "../../project/board-model";
import { useCloseOnNavigation, useDialog } from "../../shell/useDialog";
import type {
  ParentStatusChoice,
  ParentStatusOption,
  ParentStatusQuestion,
} from "./useParentStatusMove";

/** What each choice's button says. */
const CHOICE_LABELS: Readonly<Record<ParentStatusOption, string>> = {
  include_children: "Move them with it",
  include_parent: "Move the parents into work too",
  unlink_children: "Unlink them",
};

/** What each choice does, in a sentence under its button. Move them with it moves only the open
 *  issues whose status is in another group than `status` (not started, in work, done): an open
 *  issue is either not started or in work. */
function choiceSentence(option: ParentStatusOption, key: string, status: string): string {
  switch (option) {
    case "include_children": {
      const which =
        status === "done"
          ? ""
          : IN_WORK_STATUSES.has(status)
            ? " that is not in work"
            : " that is in work";
      return `Dispatch moves each open issue under ${key}${which} to ${statusText(status)} too.`;
    }
    case "unlink_children":
      return `Dispatch removes ${key} as the parent of each open child. Each child keeps its status.`;
    case "include_parent":
      return `Dispatch moves each parent of ${key} that is not in work to ${statusText("in_progress")}.`;
  }
}

/** One issue as a status change leaves it: its status, and its parent or none. */
function entryText(entry: StatusChangeEntry): string {
  const parent = entry.parent === null ? "no parent" : `under ${entry.parent}`;
  return `${entry.key}: ${statusText(entry.status)}, ${parent}`;
}

const buttonClasses = `min-h-11 rounded-lg border px-3 py-2 text-sm font-medium ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder} ${secondaryButtonDisabledText}`;

function ChoiceRow({
  choice,
  question,
}: {
  choice: ParentStatusChoice;
  question: ParentStatusQuestion;
}): ReactNode {
  const sentenceId = useId();
  const { check, option } = choice;
  const label = CHOICE_LABELS[option];
  const writing = question.writing === option;
  return (
    <li className="space-y-1">
      <button
        aria-describedby={sentenceId}
        className={buttonClasses}
        disabled={
          check.state !== "ready" ||
          question.writing !== undefined ||
          question.refusal !== undefined
        }
        onClick={() => question.choose(option)}
        type="button"
      >
        {writing ? "Saving…" : label}
      </button>
      <p className={`text-sm ${textSecondaryOnSurface}`} id={sentenceId}>
        {choiceSentence(option, question.key, question.status)}
      </p>
      {check.state === "checking" ? (
        <p className={`text-xs ${textMutedOnSurface}`}>Checking what this changes…</p>
      ) : check.state === "refused" ? (
        <p className={`text-xs ${dangerText}`}>{check.message}</p>
      ) : (
        <ul aria-label={`What ${label} changes`} className={`text-xs ${textSecondaryOnSurface}`}>
          {check.changed.map((entry) => (
            <li key={entry.key}>{entryText(entry)}</li>
          ))}
        </ul>
      )}
    </li>
  );
}

/**
 * The question a status write asks when Dispatch's rule would refuse it (R24): a sibling of
 * `ConfirmDialog` on `useDialog`, so it traps focus, closes on Escape and gives focus back to the
 * control the person came from. A navigation closes it too, as Keep as it is does
 * (`useCloseOnNavigation`). It names the issues that hold the write, offers each choice those
 * issues allow with what its dry run says it changes, and "Keep as it is" and Cancel, which write
 * nothing. A choice is pressed only once its dry run has answered.
 */
export function ParentStatusDialog({ question }: { question: ParentStatusQuestion }): ReactNode {
  const keepRef = useRef<HTMLButtonElement>(null);
  const dialog = useDialog<HTMLDivElement>({
    initialFocusRef: keepRef,
    onClose: question.close,
    open: true,
  });
  useCloseOnNavigation(true, question.close);
  const title = `Move ${question.key} to ${statusText(question.status)}?`;
  const closing = question.writing !== undefined;
  return (
    <>
      <div
        aria-hidden="true"
        className={`fixed inset-0 z-40 ${backdrop50}`}
        onClick={question.close}
      />
      <div className="pointer-events-none fixed inset-0 z-50 flex items-start justify-center xl:p-4 xl:pt-20">
        <div
          aria-label={title}
          aria-modal="true"
          className={`pointer-events-auto max-h-[100dvh] w-full space-y-3 overflow-y-auto border p-4 shadow-2xl xl:max-h-[calc(100dvh-6rem)] xl:max-w-lg xl:rounded-lg ${card} ${borderDefault}`}
          ref={dialog.containerRef}
          role="dialog"
        >
          <h2 className={`text-base font-semibold ${textPrimaryOnSurface}`}>{title}</h2>
          <p className={`text-sm ${textSecondaryOnSurface}`}>
            A parent is done only when each child is done. While a child is in work, its parent is
            in work too. These issues stop this change:
          </p>
          <ul aria-label="Issues that stop this change" className="space-y-1 text-sm">
            {question.issues.map((issue) => (
              <li className={textPrimaryOnSurface} key={issue.key}>
                <span className="font-semibold">{issue.key}</span> {issue.title}{" "}
                <span className={textMutedOnSurface}>
                  ({issue.relation}, {statusText(issue.status)})
                </span>
              </li>
            ))}
          </ul>
          {question.changed.length === 0 ? null : (
            <>
              <p className={`text-sm ${textSecondaryOnSurface}`}>
                Dispatch already made these changes:
              </p>
              <ul aria-label="Changes already made" className={`text-sm ${textSecondaryOnSurface}`}>
                {question.changed.map((entry) => (
                  <li key={entry.key}>{entryText(entry)}</li>
                ))}
              </ul>
            </>
          )}
          <ul aria-label="Choices" className="space-y-3">
            {question.choices.map((choice) => (
              <ChoiceRow choice={choice} key={choice.option} question={question} />
            ))}
          </ul>
          {question.refusal === undefined ? null : (
            <p className={`text-sm ${dangerText}`} role="alert">
              {question.refusal}
            </p>
          )}
          <div className="flex items-center justify-end gap-2">
            <button
              className={buttonClasses}
              disabled={closing}
              onClick={question.close}
              ref={keepRef}
              type="button"
            >
              Keep as it is
            </button>
            <button
              className={buttonClasses}
              disabled={closing}
              onClick={question.close}
              type="button"
            >
              Cancel
            </button>
          </div>
        </div>
      </div>
    </>
  );
}
