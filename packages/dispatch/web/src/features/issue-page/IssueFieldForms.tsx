import { type FormEvent, type ReactNode, useEffect, useRef } from "react";

import type { IssueDetails } from "../../api/types";
import {
  borderTransparent,
  dangerText,
  inputClasses,
  secondaryButtonBorder,
  secondaryButtonDisabledText,
  secondaryButtonText,
  textMutedHoverToSecondary,
  textSecondaryOnSurface,
} from "../../theme/classes";
import type { IssueDrafts } from "../issue/useIssueDrafts";
import { HELP_LINES, PROPERTY_LABELS, pageChrome } from "./page-structure";
import { CONTROL_LABELS } from "./page-words";
import type { IssueHeaderWrites } from "./useIssueHeaderWrites";

/**
 * A property of the issue the identity block opens an editor for, one editor at a time: the
 * two the reader types rather than picks. Every other property is edited by the control that
 * draws it, as the repository and the components are, so it needs no editor of its own.
 */
export type IssueField = "parent" | "route";

export function IssueFieldForms({
  field,
  isClosed,
  issue,
  onClose,
  writes,
}: {
  /** Which editor is open, and `null` while none is. */
  field: IssueField | null;
  isClosed: boolean;
  issue: IssueDetails;
  onClose: () => void;
  writes: IssueHeaderWrites;
}): ReactNode {
  if (field === "route") {
    return (
      <RouteForm
        drafts={writes.drafts}
        isClosed={isClosed}
        onClose={onClose}
        saving={writes.updateIssue.isPending}
      />
    );
  }
  if (field === "parent") {
    return (
      <ParentForm
        drafts={writes.drafts}
        isClosed={isClosed}
        issue={issue}
        onClose={onClose}
        saving={writes.updateIssue.isPending}
      />
    );
  }
  return null;
}

/** The buttons every editor of an issue's fields ends on: save, then cancel. The editors a later
 *  part adds share these, so every editor draws one pair of buttons and none can drift apart.
 *  `saveLabel` is the editor's own word for its save, drawn on Save whether or not a save is
 *  out: a save that is out only makes Save a button that refuses a press (`saving`). Each button
 *  that refuses is `aria-disabled` with its press ignored, never `disabled`: a `disabled` button
 *  drops the focus to the body in a browser, so a reader who pressed Save, or Enter in the box,
 *  would lose their place. */
export function FormActions({
  cancel,
  saveLabel,
  saving,
  waiting = false,
}: {
  cancel: () => void;
  saveLabel: string;
  /** Whether Save refuses a press: a save is out, or the value cannot be saved. Save stays
   *  focusable then, drawn and announced as refused, and a press of it, or Enter in the box,
   *  submits nothing. */
  saving: boolean;
  /** Whether the editor waits for the answer to a save that is out, and nothing closes it until
   *  that lands: an editor that draws its refusal in itself. Cancel refuses a press with Save
   *  then, announced as a control that cannot be pressed rather than left to look pressable: it
   *  stays focusable, and a press of it cancels nothing. */
  waiting?: boolean;
}): ReactNode {
  return (
    <>
      <button
        aria-disabled={saving || undefined}
        className={`min-h-11 rounded px-2 py-1 text-sm font-medium refused:cursor-not-allowed md:min-h-8 ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonDisabledText}`}
        onClick={(event) => {
          // A submit button: Enter in a field of the form clicks it too, and a click whose
          // default is prevented submits nothing.
          if (saving) event.preventDefault();
        }}
        type="submit"
        {...pageChrome}
      >
        {saveLabel}
      </button>
      <button
        aria-disabled={waiting || undefined}
        className={`min-h-11 rounded border px-2 py-1 text-sm font-medium refused:cursor-not-allowed md:min-h-8 ${borderTransparent} ${textMutedHoverToSecondary} ${secondaryButtonDisabledText}`}
        onClick={() => {
          if (!waiting) cancel();
        }}
        type="button"
        {...pageChrome}
      >
        {CONTROL_LABELS.cancel}
      </button>
    </>
  );
}

/** Where the issue's messages go. The sentence that says what a route is and what it is not was
 *  a tooltip: it is drawn here, where a reader is deciding what to set. */
function RouteForm({
  drafts,
  isClosed,
  onClose,
  saving,
}: {
  drafts: IssueDrafts;
  isClosed: boolean;
  onClose: () => void;
  saving: boolean;
}): ReactNode {
  const input = useRef<HTMLInputElement>(null);
  // Focus the input once when the editor opens: the trigger button unmounts, so without this
  // focus would drop to the body and a keyboard user would have to find the form again.
  useEffect(() => {
    input.current?.focus();
  }, []);
  const save = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (drafts.requestRouteSubmit()) {
      onClose();
    }
  };
  const cancel = () => {
    drafts.discardRoute();
    onClose();
  };
  return (
    <form className="flex w-full min-w-0 flex-wrap items-center gap-2" onSubmit={save}>
      <label
        className={`text-sm font-medium ${textSecondaryOnSurface}`}
        htmlFor="issue-route"
        {...pageChrome}
      >
        {PROPERTY_LABELS.route}
      </label>
      <input
        aria-describedby="issue-route-help"
        className={`w-full rounded px-2 py-1 text-sm outline-none ${inputClasses(true)}`}
        disabled={isClosed}
        id="issue-route"
        onChange={(event) => drafts.writeRoute(event.target.value)}
        onKeyDown={(event) => {
          if (event.key === "Escape") {
            // The press is the box's: it closes the box and nothing around it. On a phone the
            // box sits in Details, a sheet that closes on Escape through the key registry's
            // dialog binding (`useDialog`), which takes a press inside a field and passes over
            // one already `defaultPrevented` (`keymap.ts`); unmarked, one Escape would close the
            // box and the whole sheet with it.
            event.preventDefault();
            cancel();
          }
        }}
        placeholder="role:legion-controller-core"
        ref={input}
        value={drafts.route}
      />
      <FormActions
        cancel={cancel}
        saveLabel={CONTROL_LABELS.save_route}
        saving={isClosed || !drafts.routeIsValid || saving}
      />
      {/* What the box takes, in words (`HELP_LINES.route_shape`): a refusal while the draft is
          one Dispatch would not take, and the box's description otherwise. */}
      <span
        className={drafts.routeIsValid ? "sr-only" : `text-sm ${dangerText}`}
        id="issue-route-help"
        {...pageChrome}
      >
        {HELP_LINES.route_shape}
      </span>
    </form>
  );
}

/** The issue this one sits under. */
function ParentForm({
  drafts,
  isClosed,
  issue,
  onClose,
  saving,
}: {
  drafts: IssueDrafts;
  isClosed: boolean;
  issue: IssueDetails;
  onClose: () => void;
  saving: boolean;
}): ReactNode {
  const input = useRef<HTMLInputElement>(null);
  useEffect(() => {
    input.current?.focus();
  }, []);
  const save = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (drafts.requestParentSubmit()) {
      onClose();
    }
  };
  const cancel = () => {
    drafts.discardParent();
    onClose();
  };
  return (
    <form className="flex w-full min-w-0 flex-wrap items-center gap-2" onSubmit={save}>
      <label
        className={`text-sm font-medium ${textSecondaryOnSurface}`}
        htmlFor="issue-parent"
        {...pageChrome}
      >
        {PROPERTY_LABELS.parent}
      </label>
      <input
        aria-describedby="issue-parent-help"
        className={`w-full rounded px-2 py-1 text-sm outline-none ${inputClasses(true)}`}
        disabled={isClosed}
        id="issue-parent"
        onChange={(event) => drafts.writeParent(event.target.value)}
        onKeyDown={(event) => {
          if (event.key === "Escape") {
            // Escape closes the box alone, as in the route box above: marked, the press passes
            // the dialog binding that would close the Details sheet around it.
            event.preventDefault();
            cancel();
          }
        }}
        placeholder={`${issue.project}-12`}
        ref={input}
        value={drafts.parent}
      />
      <FormActions
        cancel={cancel}
        saveLabel={CONTROL_LABELS.save_parent}
        saving={isClosed || saving}
      />
      <span className="sr-only" id="issue-parent-help">
        Parent issue key in {issue.project}; leave empty to clear the parent.
      </span>
    </form>
  );
}
