import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useState } from "react";

import { usersQuery } from "../../api/queries";
import { QueryError } from "../../components/QueryError";
import { usePageWords } from "../../components/tooltips";
import { badgeLow, badgePrimary, pageAvatar, pageLabelChip, pagePaneBg } from "../../theme/classes";
import { pageInkText, pageQuietText } from "../../theme/shell-classes";
import {
  badgeSelectBadge,
  badgeSelectHeaderWrapper,
  badgeSelectOverlay,
  badgeSelectPageBadge,
  badgeSelectPageWrapper,
  badgeSelectWrapper,
} from "./badge-select";
import { useIssueAssignee } from "./useIssueAssignee";

/** The name the assignee picker answers to, in its words on every page: the property it sets
 *  and the issue it is on, so a voice user saying what they read presses it (WCAG 2.5.3). The
 *  new issue page's add menu reaches this picker by this name rather than drawing a second
 *  one, so a word changed here changes both sides at once. */
export function assigneeControlName(issueKey: string): string {
  return `Assignee of ${issueKey}`;
}

/**
 * The single assignee editor: shows who answers the issue's asks as a badge (their lowercase
 * email, or a muted "Unassigned" tag) and, on click, tap, Enter or Space, opens a native
 * `<select>` over everyone who has signed in (`GET /users`) with Unassigned first — the same
 * picker on desktop and iPhone, laid invisibly over the badge exactly as `PriorityControl` does.
 * The list is read only once the reader reaches for the control (pointer over it, a
 * touch on it, or focus) and then kept for the session: a fresh issue load makes no request
 * for it, and the badge never waits on it. Anyone signed in may reassign. The write
 * goes through `useIssueAssignee`, so the issue detail, the project lists and the inbox's
 * partitions update at once and roll back together; a refused person shows the server's
 * reason. Like `PriorityControl`, the select stays enabled while a save is in flight - a
 * disabled control drops the focus it holds and leaves the tab order - and a pick made meanwhile
 * is queued behind the save. Named by `assigneeControlName`.
 *
 * `variant="header"` draws the new issue page header's shape (the prototype's `.person`): the
 * person's initial in a filled circle beside their name in the ink, or Unassigned in the quiet
 * grey. The select over it is the same either way.
 */
export function AssigneeControl({
  assignee: host,
  disabled = false,
  issueKey,
  variant = "badge",
}: {
  assignee: string | null;
  disabled?: boolean;
  issueKey: string;
  variant?: "badge" | "header";
}): ReactNode {
  const page = usePageWords();
  const [wanted, setWanted] = useState(false);
  const users = useQuery({ ...usersQuery(), enabled: wanted });
  const want = () => setWanted(true);
  const write = useIssueAssignee(issueKey, host);
  /** The assignee as the badge and the select draw it (`IssueFieldWrite.shown`). */
  const assignee = write.shown;
  const logins = (users.data ?? []).map((user) => user.login);
  // An assignee the list does not carry still names who holds the issue; keep it selectable
  // so the control never shows a value its options lack.
  const options = assignee === null || logins.includes(assignee) ? logins : [assignee, ...logins];
  const tone = assignee === null ? badgeLow : badgePrimary;
  // Inside `NoTooltips` the badge is the new issue page's label chip, and Unassigned its quiet
  // words on the same ground, as the header variant says it in the quiet grey: each colour there
  // is one of that page's.
  const badgeTone =
    page === null
      ? `${tone.bg} ${tone.text}`
      : assignee === null
        ? `${pagePaneBg} ${pageQuietText}`
        : pageLabelChip;
  const label = assignee ?? page?.CONTROL_LABELS.unassigned ?? "Unassigned";
  const mark = assignee === null ? page?.pageChrome : page?.authorText;
  // Inside `NoTooltips` the person is a value, so it keeps `max-w-full` and breaks anywhere, and
  // its wrapper narrows to the line rather than keep its width. The header variant is the new
  // page's shape alone, so it wraps on every surface.
  return (
    <>
      <span
        className={`${variant === "header" ? badgeSelectHeaderWrapper : page === null ? badgeSelectWrapper : badgeSelectPageWrapper} ${disabled ? "opacity-50" : ""}`}
      >
        {variant === "header" ? (
          <span
            aria-hidden="true"
            className={`inline-flex min-w-0 max-w-full items-center gap-1.5 text-sm leading-5 [overflow-wrap:anywhere] ${
              assignee === null ? pageQuietText : pageInkText
            }`}
            {...mark}
          >
            {assignee === null ? null : (
              // The initial is drawn, not written: the name beside it is the value a reader and
              // a reading of the badge take, so the circle adds no letter to its text.
              <span
                className={`inline-grid size-5 shrink-0 place-items-center rounded-full text-xs leading-5 font-semibold before:content-[attr(data-initial)] ${pageAvatar}`}
                data-initial={assignee.charAt(0).toUpperCase()}
              />
            )}
            <span className="min-w-0 [overflow-wrap:anywhere]">{label}</span>
          </span>
        ) : (
          <span
            aria-hidden="true"
            className={`${page === null ? badgeSelectBadge : badgeSelectPageBadge} ${badgeTone}`}
            {...mark}
          >
            {label}
          </span>
        )}
        <select
          aria-label={assigneeControlName(issueKey)}
          className={badgeSelectOverlay}
          disabled={disabled}
          onFocus={want}
          onMouseDown={(event) => event.stopPropagation()}
          onPointerDown={(event) => event.stopPropagation()}
          onPointerEnter={want}
          onTouchStart={(event) => {
            event.stopPropagation();
            want();
          }}
          onChange={(event) => {
            const { value } = event.target;
            write.submit(value === "" ? null : value);
          }}
          value={assignee ?? ""}
        >
          <option value="">Unassigned</option>
          {options.map((login) => (
            <option key={login} value={login}>
              {login}
            </option>
          ))}
        </select>
      </span>
      {write.failed ? (
        <QueryError
          message={write.error ?? `Could not update the assignee of ${issueKey}.`}
          onRetry={write.retry}
          retrying={write.pending}
        />
      ) : null}
    </>
  );
}
