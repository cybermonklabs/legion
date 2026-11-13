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
  textMutedOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { HELP_LINES, PROPERTY_LABELS, pageChrome } from "./page-structure";
import { CONTROL_LABELS } from "./page-words";
import type { IssueDrafts } from "./useIssueDrafts";
import type { IssueHeaderWrites } from "./useIssueHeaderWrites";

/**
 * A property of the issue the identity block opens an editor for, one editor at a time: the
 * two the reader types rather than picks. Every other property is edited by the control that
 * draws it, so it needs no editor of its own - the issues this one waits on are edited beside
 * their own value in the panel (`BlockedByControl.tsx`), as the repository and the components
 * are.
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

/** The buttons every editor of an issue's fields ends on: save, then cancel. The panel's own
 *  editors end on these too (`BlockedByControl.tsx`), so the two draw one pair of buttons and
 *  cannot drift apart. `saveLabel` is the editor's own word for its save, which is what says
 *  a save is out ("Saving…"). */
export function FormActions({
  cancel,
  saveLabel,
  saving,
  waiting = false,
}: {
  cancel: () => void;
  saveLabel: string;
  /** Whether Save cannot be pressed: a save is out, or the value cannot be saved. */
  saving: boolean;
  /** Whether the editor waits for the answer to a save that is out, and nothing closes it until
   *  that lands: the blocked-by editor, whose refusal is drawn in it. Cancel is held with Save
   *  then, drawn and announced as a control that cannot be pressed rather than one whose press
   *  does nothing. */
  waiting?: boolean;
}): ReactNode {
  return (
    <>
      <button
        className={`min-h-11 rounded px-2 py-1 text-sm font-medium disabled:cursor-not-allowed md:min-h-8 ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonDisabledText}`}
        disabled={saving}
        type="submit"
        {...pageChrome}
      >
        {saveLabel}
      </button>
      <button
        className={`min-h-11 rounded border px-2 py-1 text-sm font-medium disabled:cursor-not-allowed md:min-h-8 ${borderTransparent} ${textMutedHoverToSecondary} ${secondaryButtonDisabledText}`}
        disabled={waiting}
        onClick={cancel}
        type="button"
        {...pageChrome}
      >
        {CONTROL_LABELS.cancel}
      </button>
    </>
  );
}

/** Where the issue's messages go. The sentence that says what a route is and what it is not was
 *  a tooltip: it is drawn here, where a reader is deciding what to set (R11). */
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
  return (
    <form className="mt-2 flex flex-wrap items-center gap-2" onSubmit={save}>
      <label
        className={`text-sm font-medium ${textSecondaryOnSurface}`}
        htmlFor="issue-route"
        {...pageChrome}
      >
        {PROPERTY_LABELS.route}
      </label>
      <input
        aria-label={CONTROL_LABELS.message_route}
        aria-describedby="issue-route-hint issue-route-help"
        className={`w-full rounded px-2 py-1 text-sm outline-none md:w-64 ${inputClasses(true)}`}
        disabled={isClosed}
        id="issue-route"
        onChange={(event) => drafts.writeRoute(event.target.value)}
        onKeyDown={(event) => {
          if (event.key === "Escape") {
            drafts.discardRoute();
            onClose();
          }
        }}
        placeholder="role:legion-controller-core"
        ref={input}
        value={drafts.route}
      />
      <FormActions
        cancel={() => {
          drafts.discardRoute();
          onClose();
        }}
        saveLabel="Save route"
        saving={isClosed || !drafts.routeIsValid || saving}
      />
      <span
        className={drafts.routeIsValid ? "sr-only" : `text-sm ${dangerText}`}
        id="issue-route-help"
      >
        A message route must be role:[a-z0-9-]+ or session:[0-9a-f-]{`{16,}`}.
      </span>
      <p
        className={`basis-full text-sm ${textMutedOnSurface}`}
        id="issue-route-hint"
        {...pageChrome}
      >
        {HELP_LINES.route}
      </p>
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
  return (
    <form className="mt-2 flex flex-wrap items-center gap-2" onSubmit={save}>
      <label
        className={`text-sm font-medium ${textSecondaryOnSurface}`}
        htmlFor="issue-parent"
        {...pageChrome}
      >
        {PROPERTY_LABELS.parent}
      </label>
      <input
        aria-describedby="issue-parent-help"
        className={`w-full rounded px-2 py-1 text-sm outline-none md:w-64 ${inputClasses(true)}`}
        disabled={isClosed}
        id="issue-parent"
        onChange={(event) => drafts.writeParent(event.target.value)}
        onKeyDown={(event) => {
          if (event.key === "Escape") {
            drafts.discardParent();
            onClose();
          }
        }}
        placeholder={`${issue.project}-12`}
        ref={input}
        value={drafts.parent}
      />
      <FormActions
        cancel={() => {
          drafts.discardParent();
          onClose();
        }}
        saveLabel="Save parent"
        saving={isClosed || saving}
      />
      <span className="sr-only" id="issue-parent-help">
        Parent issue key in {issue.project}; leave empty to clear the parent.
      </span>
    </form>
  );
}
