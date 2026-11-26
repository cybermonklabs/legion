import { type ReactNode, useState } from "react";

import type { IssuePriority } from "../../api/types";
import { type ListMarks, MultiSelect } from "../../components/MultiSelect";
import { QueryError } from "../../components/QueryError";
import { usePageWords } from "../../components/tooltips";
import {
  badgeLow,
  pageInkText,
  pageLabelChip,
  pagePaneBg,
  pageQuietText,
  priorityBadge,
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
import { type IssuePriorityWrite, useIssuePriority } from "./useIssuePriority";

/** The option that stands for no priority: a pick of it unsets the issue's priority. */
const UNSET = "";

/** The options in the order the list offers them: Unset, then P0 to P3 by their digits. */
const PRIORITY_OPTIONS: readonly string[] = [UNSET, "0", "1", "2", "3"];

const PRIORITY_TRIGGER_ATTRIBUTE = "data-issue-priority";
const PRIORITY_MARKS: ListMarks = { trigger: { [PRIORITY_TRIGGER_ATTRIBUTE]: "" } };
/** The priority list's trigger, which the Board's `p` and the issue page's `p`, `0`-`3` and
 *  Escape find it by: the shared list hands out no element of its own. */
export const PRIORITY_TRIGGER_SELECTOR = `[${PRIORITY_TRIGGER_ATTRIBUTE}]`;

/**
 * The priority editor for a surface that owns no write of its own - a list row, a board card,
 * an Inbox row - where this control is the only thing that writes that issue's priority.
 */
export function PriorityControl({
  disabled,
  issueKey,
  priority,
}: {
  disabled?: boolean;
  issueKey: string;
  priority: IssuePriority | null;
}): ReactNode {
  const write = useIssuePriority(issueKey, priority);
  return <PriorityEditor disabled={disabled} issueKey={issueKey} write={write} />;
}

/**
 * The single priority editor: shows the issue's priority as the `P0`–`P3` badge (a muted
 * "Priority" tag when unset), and the badge is the trigger of the shared list's single pick
 * (`MultiSelect`) of Unset and P0–P3, whose text box narrows them as the reader types. It is the
 * same list on desktop and iPhone. The trigger fills the wrapper's tap target, 44 px (32 px from
 * `md`), while the badge keeps its size, and the wrapper draws the keyboard focus ring. The
 * trigger is named `Priority of <KEY>` and then the badge's words, and it carries the mark
 * `PRIORITY_TRIGGER_SELECTOR` finds: the board's `p` reaches it through the focused card, and the
 * issue page's keys reach it in the header.
 *
 * `write` goes through `useIssuePriority`, so every surface gets the same optimistic update,
 * rollback and list refetch. A pick of the priority the issue holds writes nothing, as a
 * `<select>` set to the value it holds fires nothing. The trigger takes a press while a save is
 * in flight, and a pick made meanwhile is queued behind the save rather than refused. While the
 * host's `disabled` holds, the trigger is `aria-disabled` and never `disabled`, so it keeps the
 * reader's focus: a press of it opens no list, and a pick from a list that stood open as the hold
 * began writes nothing. A press on the trigger, or in the list it opens, stays out of the row,
 * card or header under it (`badgeReachHandlers`): a board card is a drag activator, and a held
 * tap on the badge must open the list, not lift the card.
 *
 * The host passes the write when it has other ways to set the same priority: the issue page
 * creates one for the page and shares it with the keyboard's `0`–`3` and the palette's Set
 * priority rows, so one refusal is reported once, here, by the control the reader is looking at.
 * The priority drawn is the write's (`IssueFieldWrite.shown`), so a pick from any of them shows
 * from its press.
 *
 * `variant="header"` draws the new issue page header's shape (the prototype's `.prio`): three
 * bars filled by how urgent the priority is, beside `P1` in the ink, or the word Priority in the
 * quiet grey while it has none. The list under it is the same either way.
 */
export function PriorityEditor({
  disabled = false,
  issueKey,
  variant = "badge",
  write,
}: {
  disabled?: boolean;
  issueKey: string;
  variant?: "badge" | "header";
  write: IssuePriorityWrite;
}): ReactNode {
  const page = usePageWords();
  const [open, setOpen] = useState(false);
  const priority = write.shown;
  const value = priority === null ? UNSET : String(priority);
  const tone = priority === null ? badgeLow : priorityBadge[priority];
  // Inside `NoTooltips` the badge is the new issue page's label chip, and the word Priority its
  // quiet words on the same ground, as the header variant says it in the quiet grey: each colour
  // there is one of that page's. The prototype's `.prio` draws how urgent a priority is by its
  // bars and its word, never by a colour, so P0 and P3 wear the same chip.
  const badgeTone =
    page === null
      ? `${tone.bg} ${tone.text}`
      : priority === null
        ? `${pagePaneBg} ${pageQuietText}`
        : pageLabelChip;
  const label = priority === null ? (page?.PROPERTY_LABELS.priority ?? "Priority") : `P${priority}`;
  const mark = priority === null ? page?.pageChrome : page?.authorText;
  // Inside `NoTooltips` the priority is a value, so it keeps `max-w-full` and breaks anywhere,
  // and its wrapper and the trigger in it narrow to the line rather than keep their width. The
  // header variant is the new page's shape alone, so it wraps on every surface.
  return (
    <>
      <span
        className={`${variant === "header" ? badgeSelectHeaderWrapper : page === null ? badgeSelectWrapper : badgeSelectPageWrapper} ${disabled ? "opacity-50" : ""}`}
        {...badgeReachHandlers()}
      >
        <MultiSelect
          ariaDisabled={disabled}
          chevron={false}
          emptyMessage="No priority matches that."
          label={`Priority of ${issueKey}`}
          marks={PRIORITY_MARKS}
          mode="single"
          onOpenChange={(next) => setOpen(next && !disabled)}
          onPick={(picked) => {
            if (disabled || picked === value) return;
            write.submit(picked === UNSET ? null : (Number(picked) as IssuePriority));
          }}
          open={open}
          optionLabel={(option) => (option === UNSET ? "Unset" : `P${option}`)}
          options={PRIORITY_OPTIONS}
          searchLabel="Search priorities"
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
              className={`inline-flex min-w-0 max-w-full items-center gap-[5px] text-sm leading-5 font-semibold [overflow-wrap:anywhere] ${
                priority === null ? pageQuietText : pageInkText
              }`}
              {...mark}
            >
              <PriorityBars priority={priority} />
              {label}
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
          message={write.stopped ?? `Could not update the priority of ${issueKey}.`}
          onRetry={write.retry}
          retrying={write.pending}
        />
      ) : null}
    </>
  );
}

/** The prototype's priority shape: three rising bars, as many filled as the priority is urgent
 *  - three for P0, none for P3 or none set. `P1` beside it says the same in words. */
function PriorityBars({ priority }: { priority: IssuePriority | null }): ReactNode {
  const filled = priority === null ? 0 : 3 - priority;
  return (
    <svg aria-hidden="true" className="size-3.5 shrink-0" viewBox="0 0 14 14">
      {[
        { height: 5, x: 1, y: 8 },
        { height: 8, x: 5.5, y: 5 },
        { height: 11, x: 10, y: 2 },
      ].map((bar, index) => (
        <rect
          className={index < filled ? "fill-page-ink" : "fill-page-hair"}
          height={bar.height}
          key={bar.x}
          rx="1"
          width="3"
          x={bar.x}
          y={bar.y}
        />
      ))}
    </svg>
  );
}
