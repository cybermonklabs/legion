import { type FormEvent, type ReactNode, useEffect, useRef } from "react";

import type { IssueDetails } from "../../api/types";
import {
  focusBorder,
  focusVisibleRing,
  pageFailedText,
  pageHairBorder,
  pageInkText,
  pageLineBorder,
  pageMutedText,
  pagePaperBg,
  pageQuietText,
} from "../../theme/classes";
import type { IssueDrafts } from "../issue/useIssueDrafts";
import { HELP_LINES, PROPERTY_LABELS, pageChrome } from "./page-structure";
import { CONTROL_LABELS } from "./page-words";
import type { IssueHeaderWrites } from "./useIssueHeaderWrites";

/**
 * The colours of each editor and its buttons, the new issue page's, which alone draws them, in a
 * row of Details: the box as the page's other boxes in Details are drawn (`RepoControl.tsx`'s box,
 * `AskCard.tsx`'s answer box) - the page's hairline rule, its paper and its ink, the placeholder in
 * the quiet grey, and `main`'s focus rule, the application's one focus indicator - and the route's
 * refusal in the failed red. Save is the page's bordered action (`ApprovalChip.tsx`'s): its rule,
 * its paper and the muted grey, which turns ink under the pointer while Save takes a press and
 * quiet while it refuses one. Cancel is the page's quiet act (`AskCard.tsx`'s): the quiet grey,
 * which turns muted under the pointer while Cancel takes a press. `classes.ts` registers each
 * pair of these in `CONTRAST_CHECKS`, which `shared-controls.tooltips.test.tsx` holds.
 */
const ON_PAGE = {
  box: `${pageHairBorder} ${pagePaperBg} ${pageInkText} placeholder:text-page-quiet ${focusBorder}`,
  cancel: `${pageQuietText} not-refused:hover:text-page-muted`,
  failure: pageFailedText,
  save: `${pageLineBorder} ${pagePaperBg} ${pageMutedText} not-refused:hover:text-page-ink refused:text-page-quiet`,
} as const;

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

/** The buttons every editor of an issue's fields ends on: save, then cancel. Every editor draws
 *  this one pair, so no two editors' buttons can drift apart. `saveLabel` is the editor's own word
 *  for its save, drawn on Save whether or not a save is out: a save that is out only makes Save a
 *  button that refuses a press (`saving`). Each button that refuses is `aria-disabled` with its
 *  press ignored, never `disabled`: a `disabled` button drops the focus to the body in a browser,
 *  so a reader who pressed Save, or Enter in the box, would lose their place. Each draws `main`'s
 *  focus ring, the application's one focus indicator. */
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
        className={`min-h-11 rounded border px-2 py-1 text-sm font-medium outline-none refused:cursor-not-allowed focus-visible:ring-2 md:min-h-8 ${ON_PAGE.save} ${focusVisibleRing}`}
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
        className={`min-h-11 rounded px-2 py-1 text-sm font-medium outline-none refused:cursor-not-allowed focus-visible:ring-2 md:min-h-8 ${ON_PAGE.cancel} ${focusVisibleRing}`}
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

/** The form every editor of an issue's fields is drawn in, around the editor's label, box, buttons
 *  and help (`children`). Escape in it, from the box, Save or Cancel, is the editor's: it closes
 *  the editor (`cancel`) and nothing around it. On a phone the editor sits in Details, a sheet that
 *  closes on Escape through the key registry's dialog binding (`useDialog`), which takes a press
 *  inside a field and passes over one already `defaultPrevented` (`keymap.ts`); unmarked, one
 *  Escape would close the editor and the whole sheet with it. A press stopped here reaches no
 *  listener above the form either. A press that ends an IME composition gives up what the reader
 *  was composing, not the editor: the form leaves it alone, unmarked, as the registry does. */
function EditorForm({
  cancel,
  children,
  onSubmit,
}: {
  cancel: () => void;
  children: ReactNode;
  onSubmit: (event: FormEvent<HTMLFormElement>) => void;
}): ReactNode {
  return (
    <form
      className="flex w-full min-w-0 flex-wrap items-center gap-2"
      onKeyDown={(event) => {
        if (event.key !== "Escape" || event.nativeEvent.isComposing) return;
        event.preventDefault();
        event.stopPropagation();
        cancel();
      }}
      onSubmit={onSubmit}
    >
      {children}
    </form>
  );
}

/** Where the issue's messages go: the box for the route, its buttons, and the words that say what
 *  the box takes, drawn here, where a reader is deciding what to set. */
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
    <EditorForm cancel={cancel} onSubmit={save}>
      {/* The row of Details this editor is drawn in draws the property's label already, so this
          one is for a screen reader alone: it names the box, and the row's words are drawn once. */}
      <label className="sr-only" htmlFor="issue-route" {...pageChrome}>
        {PROPERTY_LABELS.route}
      </label>
      <input
        aria-describedby="issue-route-help"
        className={`w-full min-w-0 rounded-sm border px-2 py-1 text-sm outline-none focus-visible:ring-2 ${ON_PAGE.box} ${focusVisibleRing}`}
        disabled={isClosed}
        id="issue-route"
        onChange={(event) => drafts.writeRoute(event.target.value)}
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
        className={drafts.routeIsValid ? "sr-only" : `text-sm ${ON_PAGE.failure}`}
        id="issue-route-help"
        {...pageChrome}
      >
        {HELP_LINES.route_shape}
      </span>
    </EditorForm>
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
    <EditorForm cancel={cancel} onSubmit={save}>
      {/* Named for a screen reader alone, as the route box is: the row draws the label. */}
      <label className="sr-only" htmlFor="issue-parent" {...pageChrome}>
        {PROPERTY_LABELS.parent}
      </label>
      <input
        aria-describedby="issue-parent-help"
        className={`w-full min-w-0 rounded-sm border px-2 py-1 text-sm outline-none focus-visible:ring-2 ${ON_PAGE.box} ${focusVisibleRing}`}
        disabled={isClosed}
        id="issue-parent"
        onChange={(event) => drafts.writeParent(event.target.value)}
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
        Parent issue key, in {issue.project} or another project; leave empty to clear the parent.
      </span>
    </EditorForm>
  );
}
