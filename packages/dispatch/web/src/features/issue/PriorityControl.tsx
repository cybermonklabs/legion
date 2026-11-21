import type { ReactNode } from "react";

import type { IssuePriority } from "../../api/types";
import { ChoiceMenu, type MenuChoice } from "../../components/ChoiceMenu";
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
  badgeMenuHeaderTrigger,
  badgeMenuPageTrigger,
  badgeMenuTrigger,
  badgeSelectBadge,
  badgeSelectPageBadge,
} from "./badge-select";
import { type IssuePriorityWrite, useIssuePriority } from "./useIssuePriority";

/** The priority menu: Unset, then P0 to P3 on their own digits. A choice's value is the priority
 *  as text, `""` for none. */
const priorityChoices: readonly MenuChoice<string>[] = [
  { label: "Unset", value: "" },
  { digit: "0", label: "P0", value: "0" },
  { digit: "1", label: "P1", value: "1" },
  { digit: "2", label: "P2", value: "2" },
  { digit: "3", label: "P3", value: "3" },
];

/**
 * The priority editor for a surface that owns no write of its own - a list row, a board card,
 * an Inbox row - where this control is the only thing that writes that issue's priority.
 */
export function PriorityControl({
  disabled,
  issueKey,
  priority,
  shortcutKeys,
}: {
  disabled?: boolean;
  issueKey: string;
  priority: IssuePriority | null;
  /** The keys that open the menu, where the host draws them on the trigger (the Inbox's reading
   *  pane's head: `p`, or the person's own). */
  shortcutKeys?: readonly string[];
}): ReactNode {
  const write = useIssuePriority(issueKey, priority);
  return (
    <PriorityEditor
      disabled={disabled}
      issueKey={issueKey}
      shortcutKeys={shortcutKeys}
      write={write}
    />
  );
}

/**
 * The single priority editor: shows the issue's priority as the `P0`–`P3` badge (a muted
 * "Priority" tag when unset), and a click, a tap, Enter or Space on it opens the priority menu
 * (`ChoiceMenu`) of Unset and P0–P3, with the current one checked and `0`–`3` writing P0–P3 -
 * the same menu on desktop and iPhone. The badge is the 44 px tap target (32 px from `md`) and
 * draws the keyboard focus ring. `write` goes through `useIssuePriority`, so every surface gets
 * the same optimistic update, rollback and list refetch. The trigger stays enabled while a save
 * is in flight: a disabled control drops the focus it holds and leaves the tab order, and a pick
 * made meanwhile is queued behind the save rather than refused. A press on the badge or in its
 * menu stays there: a board card is a drag activator, and a held tap on the badge must open the
 * menu, not lift the card. The trigger is named `Priority of <KEY>` and then the priority it
 * shows (`Priority of CORE-1 P2`; an unset one by the label alone) and marked
 * `data-priority-menu`, which the Inbox's, the issue page's and the Board's `p` press.
 *
 * The host passes the write when it has other ways to set the same priority: the issue page
 * creates one for the page and shares it with the keyboard's `0`–`3` and the palette's Set
 * priority rows, so one refusal is reported once, here, by the control the reader is looking at.
 * The priority drawn is the write's (`IssueFieldWrite.shown`), so a pick from any of them shows
 * from its press.
 * `held` is the host's own write out (the issue page's other field writes): the trigger keeps the
 * focus and its Tab stop and opens nothing (`ChoiceMenu`'s `held`).
 *
 * `variant="header"` draws the new issue page header's shape (the prototype's `.prio`): three
 * bars filled by how urgent the priority is, beside `P1` in the ink, or the word Priority in the
 * quiet grey while it has none, on a trigger of the header line's 24 px
 * (`badgeMenuHeaderTrigger`). The menu it opens, and the trigger's name, are the same either way.
 */
export function PriorityEditor({
  disabled = false,
  held = false,
  issueKey,
  shortcutKeys,
  variant = "badge",
  write,
}: {
  disabled?: boolean;
  held?: boolean;
  issueKey: string;
  /** The keys that open the menu, named on its trigger and the first drawn inside it. */
  shortcutKeys?: readonly string[];
  variant?: "badge" | "header";
  write: IssuePriorityWrite;
}): ReactNode {
  const page = usePageWords();
  const priority = write.shown;
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
  // and its trigger narrows to the line rather than keep its width. The header variant is the
  // new page's shape alone, so it wraps on every surface.
  const trigger =
    variant === "header"
      ? badgeMenuHeaderTrigger
      : page === null
        ? badgeMenuTrigger
        : badgeMenuPageTrigger;
  return (
    <>
      <ChoiceMenu
        checked={priority === null ? "" : `${priority}`}
        choices={priorityChoices}
        disabled={disabled}
        face={label}
        held={held}
        label={`Priority of ${issueKey}`}
        marks={{ "data-priority-menu": "" }}
        onChoose={(value) => write.submit(value === "" ? null : (Number(value) as IssuePriority))}
        shortcutKeys={shortcutKeys}
        triggerClassName={trigger}
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
      </ChoiceMenu>
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
