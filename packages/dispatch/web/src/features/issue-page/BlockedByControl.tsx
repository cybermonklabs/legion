import { type FormEvent, type ReactNode, type RefObject, useEffect, useRef } from "react";
import { Link } from "react-router-dom";

import type { IssueDetails } from "../../api/types";
import { inputMethodKey } from "../../lib/input-method-key";
import {
  pageFailedText,
  pageField,
  pageHeaderControl,
  pageInkText,
  pageQuietText,
} from "../../theme/classes";
import { propertyControl } from "../issue/property-control";
import { buildIssuePath } from "../refs/routes";
import { focusOnDocument } from "../shell/roving";
import { blockedByControlName } from "./details-names";
import { FormActions } from "./IssueFieldForms";
import { authorText, HELP_LINES, pageChrome } from "./page-structure";
import { ANSWER_LINES, BLOCKED_BY_REFUSALS, CONTROL_LABELS } from "./page-words";
import { ROW_LINK } from "./row-references";
import { firstUsable } from "./usable-control";
import type { BlockedByWrite } from "./useIssueBlockedBy";

/** The issues `value` names, in the order they were typed, with every repeat and blank dropped:
 *  what a reader types into the blocked-by editor as the model's own list. */
function issueKeysIn(value: string): readonly string[] {
  const keys = value
    .split(/[\s,]+/u)
    .map((key) => key.toUpperCase())
    .filter((key) => key !== "");
  return [...new Set(keys)];
}

/**
 * The issues this one waits on, in their row of Details beside the repository and the
 * components, which carries the label: every blocker as a link to it with its title after it,
 * and one control that opens the editor for them - the same control whether the issue waits on
 * nothing or on five, so a set value can be changed and cleared and not only set once.
 *
 * The editor is here, where the value is, as the repository's and the components' are. It
 * stays open until Dispatch answers and says there what it refused - a key it has no issue
 * for, or the issue itself - in the page's own sentence with the key in it
 * (`BLOCKED_BY_REFUSALS`), with the keys the reader typed still in the box, so a refused
 * list is mended rather than retyped (`useIssueBlockedBy.ts`).
 *
 * While that save is out nothing closes the editor: Cancel is held, and drawn so, and Escape is
 * refused, because a reader who closed it a moment before the refusal landed would be shown no
 * refusal at all. Opening the editor forgets the last refusal, so the one drawn is always the
 * one the reader is looking at.
 *
 * Whether the editor is open, the list typed into it and its write are the panel's
 * (`IssueMarginPanel`), handed down here. A closed section of Details keeps its rows on the
 * page, hidden, so a reader who closes Development while a save is out still finds the editor,
 * the list they typed and the refusal that landed meanwhile when they open it again.
 */
export function BlockedByControl({
  draft,
  editing,
  focusFallback,
  issue,
  onDraft,
  onEditing,
  titleOf,
  write,
}: {
  /** The list typed into the editor. */
  draft: string;
  /** Whether the editor is open. */
  editing: boolean;
  /** Where the focus goes as the editor closes when the row holds nothing that takes it: on a
   *  closed issue that waits on none, the row draws no control and no issue. */
  focusFallback?: RefObject<HTMLElement | null>;
  issue: IssueDetails;
  onDraft: (draft: string) => void;
  onEditing: (editing: boolean) => void;
  /** The title of the issue `key`, once Details has read it. */
  titleOf: (key: string) => string | undefined;
  /** The write that saves the list (`useIssueBlockedBy`), which closes the editor on a landing. */
  write: BlockedByWrite;
}): ReactNode {
  const blockers = issue.blocked_by ?? [];
  const input = useRef<HTMLInputElement>(null);
  const control = useRef<HTMLButtonElement>(null);
  const row = useRef<HTMLDivElement>(null);
  // Whether the editor was open at the last commit, so that its close - by Cancel, by Escape or
  // by a save that lands - is told apart from the row's first draw, with the editor closed.
  const wasEditing = useRef(editing);
  const isClosed = issue.closed_at !== null;
  // The editor opens with the issue's own list in it, and the focus in the box: the control
  // that opened it is still on the page, so a reader on the keyboard would otherwise have to
  // find the box themselves. It closes with the focus back on that control, where the focus
  // fell to the document with the box, the one focus the page takes back (`shell/roving.ts`).
  // It passes over a control that takes no press (`usableControl`): on a closed issue the row
  // draws no control, so the focus goes to the first issue the row waits on, and where it waits
  // on none, to `focusFallback`.
  useEffect(() => {
    const closed = wasEditing.current && !editing;
    wasEditing.current = editing;
    if (closed) {
      if (focusOnDocument()) {
        firstUsable(
          control.current,
          row.current?.querySelector<HTMLElement>("a[href]"),
          focusFallback?.current
        )?.focus();
      }
      return;
    }
    if (editing) input.current?.focus();
  }, [editing, focusFallback]);
  const open = () => {
    write.forgetRefusal();
    onDraft(blockers.join(" "));
    onEditing(true);
  };
  // The save holds the editor open: the refusal it may bring is drawn in this form and nowhere
  // else, so a close taken meanwhile would throw it away.
  const close = () => {
    if (!write.pending) onEditing(false);
  };
  const save = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    write.save(issueKeysIn(draft));
  };
  return (
    <div className={`flex w-full min-w-0 flex-col gap-1 text-sm ${pageInkText}`} ref={row}>
      <div className="flex min-w-0 flex-wrap items-center gap-2">
        {blockers.length === 0 ? (
          <span className={pageQuietText} {...pageChrome}>
            {CONTROL_LABELS.none}
          </span>
        ) : (
          blockers.map((key) => <IssueLine issueKey={key} key={key} title={titleOf(key)} />)
        )}
        {isClosed || editing ? null : (
          <button
            aria-label={blockedByControlName(issue.key, blockers.length)}
            // The add menu reaches this control by this mark rather than drawing a second copy of
            // it, and Details' Retry hands the focus back to it by the same mark (`property-control.ts`).
            {...propertyControl("blocked_by", issue.key)}
            className={pageHeaderControl}
            onClick={open}
            ref={control}
            type="button"
            {...pageChrome}
          >
            {blockers.length === 0 ? CONTROL_LABELS.add : CONTROL_LABELS.edit}
          </button>
        )}
      </div>
      {editing ? (
        <form
          className="flex min-w-0 flex-wrap items-center gap-2"
          onKeyDown={(event) => {
            // A press that ends an IME composition gives up what the reader was composing, not
            // the editor: the form leaves it alone, unmarked, as the key registry does. Safari
            // sends it with `isComposing` false and keyCode 229 (`inputMethodKey`).
            if (event.key !== "Escape" || inputMethodKey(event.nativeEvent)) return;
            // Escape anywhere in the editor, its box, its Save or its Cancel, is Cancel's, and is
            // held as Cancel is while the save is out. It is marked taken: the page's own Escape,
            // which closes Details and the phone's Details sheet with it, passes over a press a
            // control has taken (`shell/keymap.ts`, `useDialog.ts`).
            event.preventDefault();
            close();
          }}
          onSubmit={save}
        >
          <input
            aria-describedby="issue-blocked-by-help"
            aria-label={`Issues ${issue.key} waits on`}
            className={`w-full rounded-[7px] border px-2 py-1 text-sm outline-none ${pageField}`}
            onChange={(event) => onDraft(event.target.value)}
            placeholder={`${issue.project}-12 ${issue.project}-13`}
            ref={input}
            value={draft}
          />
          <FormActions
            cancel={close}
            saveLabel={write.pending ? ANSWER_LINES.saving_field : CONTROL_LABELS.save_blocked_by}
            saving={write.pending}
            waiting={write.pending}
          />
          {/* What the box takes, drawn rather than left to a placeholder a reader on the
              keyboard never hears. */}
          <span className="sr-only" id="issue-blocked-by-help" {...pageChrome}>
            {HELP_LINES.blocked_by}
          </span>
          {/* What Dispatch refused, where the reader is typing: the page's sentence, with the key
              it is about as the reader's own text; a refusal the page has no sentence for is
              Dispatch's own words. A save the reader stopped is no refusal: it may have been
              saved, and the page says so in its own words, as it says a save that ended in an
              error of its own, which holds no word of Dispatch's. */}
          {write.unansweredLine !== undefined ? (
            <span className={`basis-full text-sm ${pageFailedText}`} role="alert" {...pageChrome}>
              {write.unansweredLine}
            </span>
          ) : write.error === undefined ? null : typeof write.error === "string" ? (
            <span className={`basis-full text-sm ${pageFailedText}`} role="alert" {...authorText}>
              {write.error}
            </span>
          ) : (
            <span className={`basis-full text-sm ${pageFailedText}`} role="alert" {...pageChrome}>
              {BLOCKED_BY_REFUSALS[write.error.words].before}
              {write.error.key === undefined ? null : (
                <span {...authorText}>{write.error.key}</span>
              )}
              {BLOCKED_BY_REFUSALS[write.error.words].after}
            </span>
          )}
        </form>
      ) : null}
    </div>
  );
}

/** One issue in a row of Details: its key, bold, as the way to it, and its title after it (the
 *  prototype's `.lnkrow`). */
export function IssueLine({
  issueKey,
  title,
}: {
  issueKey: string;
  title: string | undefined;
}): ReactNode {
  return (
    <span className="block w-full min-w-0 [overflow-wrap:anywhere]">
      <Link
        className={`dispatch-inline-link mr-1.5 font-semibold ${ROW_LINK} ${pageInkText}`}
        to={buildIssuePath({ key: issueKey, kind: "issue" })}
        {...authorText}
      >
        {issueKey}
      </Link>
      {title === undefined || title === "" ? null : <span {...authorText}>{title}</span>}
    </span>
  );
}
