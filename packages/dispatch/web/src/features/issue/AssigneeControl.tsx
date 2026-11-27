import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useState } from "react";

import { usersQuery } from "../../api/queries";
import { MultiSelect } from "../../components/MultiSelect";
import { QueryError } from "../../components/QueryError";
import { usePageWords } from "../../components/tooltips";
import {
  badgeLow,
  badgePrimary,
  pageAvatar,
  pageInkText,
  pageLabelChip,
  pagePaneBg,
  pageQuietText,
} from "../../theme/classes";
import {
  badgeReachHandlers,
  badgeSelectBadge,
  badgeSelectHeaderTrigger,
  badgeSelectHeaderWrapper,
  badgeSelectPageBadge,
  badgeSelectPageTrigger,
  badgeSelectPageWrapper,
  badgeSelectTrigger,
  badgeSelectWrapper,
} from "./badge-select";
import { useIssueAssignee } from "./useIssueAssignee";

/** The option that stands for nobody: the issue goes back to having no assignee. */
const UNASSIGNED = "";

/** The name the assignee picker answers to, in its words on every page: the property it sets
 *  and the issue it is on, so a voice user saying what they read presses it (WCAG 2.5.3). The
 *  new issue page's add menu reaches this picker by this name rather than drawing a second
 *  one, so a word changed here changes both sides at once. */
export function assigneeControlName(issueKey: string): string {
  return `Assignee of ${issueKey}`;
}

/**
 * The single assignee editor: shows who answers the issue's asks as a badge (their lowercase
 * email, or a muted "Unassigned" tag), and the badge is the trigger of the shared list's single
 * pick (`MultiSelect`) over everyone who has signed in (`GET /users`) with Unassigned first,
 * whose text box narrows the people as the reader types - the same list on desktop and iPhone,
 * laid over the badge as the ask card's hand-on is. The list is read only once the reader
 * reaches for the control (pointer over it, a touch on it, or focus) and then kept for the
 * session: a fresh issue load makes no request for it, and the badge never waits on it; until it
 * answers the list holds Unassigned and the person the issue names, says the people are loading
 * where a typed word leaves nothing, and says so where the read failed. Anyone signed in may
 * reassign: a pick writes at once, and a pick of the person the issue already names writes
 * nothing, as a `<select>` set to the value it holds fires nothing. The write goes through
 * `useIssueAssignee`, so the issue detail, the project lists and the inbox's partitions update
 * at once and roll back together; a refused person shows the server's reason. Like
 * `PriorityControl`, the control stays enabled while a save is in flight - a disabled control
 * drops the focus it holds and leaves the tab order - and a pick made meanwhile is queued behind
 * the save. Named by `assigneeControlName` and then the badge's words.
 *
 * `variant="header"` draws the new issue page header's shape (the prototype's `.person`): the
 * person's initial in a filled circle beside their name in the ink, or Unassigned in the quiet
 * grey. The list under it is the same either way.
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
  const [open, setOpen] = useState(false);
  const write = useIssueAssignee(issueKey, host);
  /** The assignee as the badge and the list draw it (`IssueFieldWrite.shown`). */
  const assignee = write.shown;
  const logins = (users.data ?? []).map((user) => user.login);
  // An assignee the list does not carry still names who holds the issue; keep it selectable
  // so the control never shows a value its options lack.
  const people = assignee === null || logins.includes(assignee) ? logins : [assignee, ...logins];
  const value = assignee ?? UNASSIGNED;
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
  // its wrapper and the trigger in it narrow to the line rather than keep their width. The header
  // variant is the new page's shape alone, so it wraps on every surface.
  return (
    <>
      <span
        className={`${variant === "header" ? badgeSelectHeaderWrapper : page === null ? badgeSelectWrapper : badgeSelectPageWrapper} ${disabled ? "opacity-50" : ""}`}
        {...badgeReachHandlers(() => setWanted(true))}
      >
        <MultiSelect
          chevron={false}
          disabled={disabled}
          emptyMessage={users.isPending ? "Loading people…" : "Nobody matches that."}
          error={users.isError ? "Could not load the people list." : null}
          label={assigneeControlName(issueKey)}
          mode="single"
          onOpenChange={setOpen}
          onPick={(picked) => {
            if (picked !== value) write.submit(picked === UNASSIGNED ? null : picked);
          }}
          open={open}
          optionLabel={(login) => (login === UNASSIGNED ? "Unassigned" : login)}
          options={[UNASSIGNED, ...people]}
          searchLabel="Search people"
          triggerClassName={
            variant === "header"
              ? badgeSelectHeaderTrigger
              : page === null
                ? badgeSelectTrigger
                : badgeSelectPageTrigger
          }
          value={value}
        >
          {variant === "header" ? (
            <span
              className={`inline-flex min-w-0 max-w-full items-center gap-1.5 text-sm leading-5 [overflow-wrap:anywhere] ${
                assignee === null ? pageQuietText : pageInkText
              }`}
              {...mark}
            >
              {assignee === null ? null : (
                // The initial is drawn, not written: the name beside it is the value a reader and
                // a reading of the badge take, so the circle adds no letter to its text, and it is
                // hidden from the trigger's name, which a browser would read its drawn letter in.
                <span
                  aria-hidden="true"
                  className={`inline-grid size-5 shrink-0 place-items-center rounded-full text-xs leading-5 font-semibold before:content-[attr(data-initial)] ${pageAvatar}`}
                  data-initial={assignee.charAt(0).toUpperCase()}
                />
              )}
              <span className="min-w-0 [overflow-wrap:anywhere]">{label}</span>
            </span>
          ) : (
            <span
              className={`${page === null ? badgeSelectBadge : badgeSelectPageBadge} ${badgeTone}`}
              {...mark}
            >
              {label}
            </span>
          )}
        </MultiSelect>
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
