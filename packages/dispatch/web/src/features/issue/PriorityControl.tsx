import { type ReactNode, useEffect } from "react";

import type { IssuePriority } from "../../api/types";
import { QueryError } from "../../components/QueryError";
import { badgeLow, priorityBadge } from "../../theme/classes";
import { badgeSelectBadge, badgeSelectOverlay, badgeSelectWrapper } from "./badge-select";
import { reservedClosedSet } from "./reserved-width";
import { type IssuePriorityWrite, useIssuePriority } from "./useIssuePriority";

const priorityOptions: readonly { label: string; value: IssuePriority | null }[] = [
  { label: "Unset", value: null },
  { label: "P0", value: 0 },
  { label: "P1", value: 1 },
  { label: "P2", value: 2 },
  { label: "P3", value: 3 },
];

/** The badge's word for `value`: the muted tag `Priority` while unset, else `P0` to `P3`. */
function badgeLabel(value: IssuePriority | null): string {
  return value === null ? "Priority" : `P${value}`;
}

/**
 * The priority editor for a surface that owns no write of its own - a list row, a board card,
 * an Inbox row - where this control is the only thing that writes that issue's priority.
 *
 * `onLive` tells a host whose own placement reads the priority - the Inbox, which bands its
 * rows by it and may filter by it - while this control's write is in flight or refused, so the
 * host keeps the row where it stood and this control, with its refusal and Retry, mounted. It
 * is told at the press: the store composes the pick over the host's read as the write joins,
 * before TanStack tells this control its own press is pending, so a registration made from that
 * state a commit later would come after the row had moved. Unregistered once the write settles
 * cleanly or the control unmounts.
 */
export function PriorityControl({
  disabled,
  issueKey,
  onLive,
  priority,
}: {
  disabled?: boolean;
  issueKey: string;
  onLive?: (live: boolean) => void;
  priority: IssuePriority | null;
}): ReactNode {
  const write = useIssuePriority(issueKey);
  const live = write.pending || write.failed;
  useEffect(() => {
    if (!live || onLive === undefined) return;
    onLive(true);
    return () => onLive(false);
  }, [live, onLive]);
  const held: IssuePriorityWrite =
    onLive === undefined
      ? write
      : {
          ...write,
          submit: (next) => {
            onLive(true);
            write.submit(next);
          },
        };
  return (
    <PriorityEditor disabled={disabled} issueKey={issueKey} priority={priority} write={held} />
  );
}

/**
 * The single priority editor: shows the issue's priority as the `P0`–`P3` badge (a muted
 * "Priority" tag when unset) and, on click, tap, Enter or Space, opens a native `<select>` to
 * pick P0–P3 or Unset — the same picker on desktop and iPhone. In the issue header (`reserve`)
 * the badge draws the word of every option in one cell (`reservedClosedSet`), its value's word
 * visible and the rest invisible, so the badge is as wide as the widest of them at every value
 * and a pick shown before the server answers it moves nothing beside the control; everywhere
 * else - a list row, a board card, an Inbox row or reading column - it draws its value's word
 * alone, at that word's own width, as it always has. Each word is `aria-hidden` like the badge
 * around it, so the select's label stays the control's one name and the words never reach a
 * screen reader. The select is laid invisibly over the badge so the tap target stays 44 px (32
 * px from `md`) while the badge keeps its size, and the wrapper draws the keyboard focus ring.
 * `write` goes through `useIssuePriority`, so every surface shows the pick at once, stops
 * showing it if it is refused, and reads the lists again. The select stays
 * enabled while a save is in flight: a disabled control drops the focus it holds and leaves the
 * tab order, and a pick made meanwhile is queued behind the save rather than refused. A press on
 * the select stays on the select: a board card is a drag activator, and a held tap on the badge
 * must open the picker, not lift the card. The select is labelled `Priority of <KEY>`; the
 * board's `p` reaches it through the focused card.
 *
 * The host passes the write when it has other ways to set the same priority: the issue page
 * creates one for the page and shares it with the keyboard's `0`–`3` and the palette's Set
 * priority rows, so one refusal is reported once, here, by the control the reader is looking at.
 */
export function PriorityEditor({
  disabled = false,
  issueKey,
  priority,
  reserve = false,
  write,
}: {
  disabled?: boolean;
  issueKey: string;
  priority: IssuePriority | null;
  /** Set only by the issue header, the one place dispatch://INFRA-299/spec#b-r5 holds the
   *  badge to one box at every value. */
  reserve?: boolean;
  write: IssuePriorityWrite;
}): ReactNode {
  const tone = priority === null ? badgeLow : priorityBadge[priority];
  const shown = badgeLabel(priority);
  return (
    <>
      <span className={`${badgeSelectWrapper} ${disabled ? "opacity-50" : ""}`}>
        <span
          aria-hidden="true"
          className={`${badgeSelectBadge} ${reserve ? reservedClosedSet.cell : ""} ${tone.bg} ${tone.text}`}
        >
          {reserve
            ? priorityOptions.map((option) => {
                const label = badgeLabel(option.value);
                return (
                  <span
                    aria-hidden="true"
                    className={`${reservedClosedSet.value} ${label === shown ? "" : reservedClosedSet.other}`}
                    key={label}
                  >
                    {label}
                  </span>
                );
              })
            : shown}
        </span>
        <select
          aria-label={`Priority of ${issueKey}`}
          className={badgeSelectOverlay}
          disabled={disabled}
          onMouseDown={(event) => event.stopPropagation()}
          onPointerDown={(event) => event.stopPropagation()}
          onTouchStart={(event) => event.stopPropagation()}
          onChange={(event) => {
            const { value } = event.target;
            write.submit(value === "" ? null : (Number(value) as IssuePriority));
          }}
          value={priority ?? ""}
        >
          {priorityOptions.map((option) => (
            <option key={option.label} value={option.value ?? ""}>
              {option.label}
            </option>
          ))}
        </select>
      </span>
      {write.failed ? (
        <QueryError
          message={`Could not update the priority of ${issueKey}.`}
          onRetry={write.retry}
          retrying={write.pending}
        />
      ) : null}
    </>
  );
}
