import { type FormEvent, type ReactNode, useEffect, useRef, useState } from "react";
import { Link } from "react-router-dom";

import type { IssueDetails } from "../../api/types";
import {
  dangerText,
  inputClasses,
  linkHoverText,
  linkText,
  pageQuietText,
  secondaryButtonBorder,
  secondaryButtonText,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { buildIssuePath } from "../refs/routes";
import { FormActions } from "./IssueFieldForms";
import { authorText, HELP_LINES, pageChrome } from "./page-structure";
import { ANSWER_LINES, BLOCKED_BY_REFUSALS, CONTROL_LABELS } from "./page-words";
import { useIssueBlockedBy } from "./useIssueBlockedBy";

/** The issues `value` names, in the order they were typed, with every repeat and blank dropped:
 *  what a reader types into the blocked-by editor as the model's own list. */
export function issueKeysIn(value: string): readonly string[] {
  const keys = value
    .split(/[\s,]+/u)
    .map((key) => key.trim().toUpperCase())
    .filter((key) => key !== "");
  return [...new Set(keys)];
}

/**
 * The name the blocked-by control answers to: the word it shows and what it opens, in the words
 * the box it opens is named by ("Issues CORE-1 waits on"), so a voice user saying what they read
 * presses it (WCAG 2.5.3) and every reader hears one sentence: "Add issues CORE-1 waits on". The
 * add menu reaches the control by this name rather than drawing a second copy of it, and calls
 * this function for it (`IssueAddMenu.tsx`), so the two sides cannot drift apart: a word changed
 * here changes both.
 */
export function blockedByControlName(issueKey: string, blockerCount: number): string {
  const verb = blockerCount === 0 ? CONTROL_LABELS.add : CONTROL_LABELS.edit;
  return `${verb} issues ${issueKey} waits on`;
}

/**
 * The issues this one waits on, in their row of Details beside the repository and the
 * components (dispatch://INFRA-1196 R13), which carries the label: every blocker as a link to
 * it with its title after it, and one control that opens the editor for them - the same control
 * whether the issue waits on nothing or on five, so a set value can be changed and cleared and
 * not only set once.
 *
 * The editor is here, where the value is, as the repository's and the components' are. It
 * stays open until Dispatch answers and says there what it refused - a key it has no issue
 * for, the issue itself, a blocker in another project - in the page's own sentence with the key
 * in it (`BLOCKED_BY_REFUSALS`), with the keys the reader typed still in the box, so a refused
 * list is mended rather than retyped (`useIssueBlockedBy.ts`).
 *
 * While that save is out nothing closes the editor: Cancel is held, and drawn so, and Escape is
 * refused, because a reader who closed it a moment before the refusal landed would be shown no
 * refusal at all. Opening the editor forgets the last refusal, so the one drawn is always the
 * one the reader is looking at.
 */
export function BlockedByControl({
  issue,
  titleOf,
}: {
  issue: IssueDetails;
  /** The title of the issue `key`, once Details has read it. */
  titleOf: (key: string) => string | undefined;
}): ReactNode {
  const blockers = issue.blocked_by ?? [];
  const [editing, setEditing] = useState(false);
  const [draft, setDraft] = useState("");
  const input = useRef<HTMLInputElement>(null);
  const write = useIssueBlockedBy(issue.key, () => setEditing(false));
  const isClosed = issue.closed_at !== null;
  // The editor opens with the issue's own list in it, and the focus in the box: the control
  // that opened it is still on the page, so a reader on the keyboard would otherwise have to
  // find the box themselves.
  useEffect(() => {
    if (editing) input.current?.focus();
  }, [editing]);
  const open = () => {
    write.forgetRefusal();
    setDraft(blockers.join(" "));
    setEditing(true);
  };
  // The save holds the editor open: the refusal it may bring is drawn in this form and nowhere
  // else, so a close taken meanwhile would throw it away.
  const close = () => {
    if (!write.pending) setEditing(false);
  };
  const save = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    write.save(issueKeysIn(draft));
  };
  return (
    <div className={`flex w-full min-w-0 flex-col gap-1 text-sm ${textSecondaryOnSurface}`}>
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
            // The name is `blockedByControlName`'s, which the add menu reaches this control by
            // rather than drawing a second copy of it (`IssueAddMenu.tsx`).
            aria-label={blockedByControlName(issue.key, blockers.length)}
            className={`min-h-11 rounded border px-2 text-sm font-medium md:min-h-8 ${secondaryButtonBorder} ${secondaryButtonText}`}
            onClick={open}
            type="button"
            {...pageChrome}
          >
            {blockers.length === 0 ? CONTROL_LABELS.add : CONTROL_LABELS.edit}
          </button>
        )}
      </div>
      {editing ? (
        <form className="flex min-w-0 flex-wrap items-center gap-2" onSubmit={save}>
          <input
            aria-describedby="issue-blocked-by-help"
            aria-label={`Issues ${issue.key} waits on`}
            className={`w-full rounded px-2 py-1 text-sm outline-none ${inputClasses(true)}`}
            id="issue-blocked-by"
            onChange={(event) => setDraft(event.target.value)}
            onKeyDown={(event) => {
              if (event.key === "Escape") close();
            }}
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
              keyboard never hears (R11). */}
          <span className="sr-only" id="issue-blocked-by-help" {...pageChrome}>
            {HELP_LINES.blocked_by}
          </span>
          {/* What Dispatch refused, where the reader is typing: the page's sentence, with the key
              it is about as the reader's own text (R21); a refusal the page has no sentence for
              is Dispatch's own words. */}
          {write.error === undefined ? null : typeof write.error === "string" ? (
            <span className={`basis-full text-sm ${dangerText}`} role="alert" {...authorText}>
              {write.error}
            </span>
          ) : (
            <span className={`basis-full text-sm ${dangerText}`} role="alert" {...pageChrome}>
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
        className={`dispatch-inline-link mr-1.5 font-semibold underline ${linkText} ${linkHoverText}`}
        to={buildIssuePath({ key: issueKey, kind: "issue" })}
        {...authorText}
      >
        {issueKey}
      </Link>
      {title === undefined || title === "" ? null : <span {...authorText}>{title}</span>}
    </span>
  );
}
