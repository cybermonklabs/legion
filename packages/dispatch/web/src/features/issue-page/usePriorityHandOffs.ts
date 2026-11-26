import { useQueryClient } from "@tanstack/react-query";
import { type RefCallback, type RefObject, useRef } from "react";

import type { Issue, IssueDetails, UpdateIssueInput } from "../../api/types";
import { useFocusHandOff } from "../../hooks/useFocusHandOff";
import { propertyControlSelector } from "../issue/property-control";
import type { KeyedPress } from "../writes/useKeyedWrite";
import { canHoldFocus } from "./focus-keeper";
import { firstUsable } from "./usable-control";

/** The priority line's write as the mutation cache holds it (`useIssuePriorityWrite`): the press
 *  its keyed write made (`KeyedPress`), with the set of the issue's own priorities it sends and
 *  the revision of the read that set was drawn from, as the issue's PATCH body names them. */
type AdvancesSave = KeyedPress<Pick<UpdateIssueInput, "advances" | "advances_revision">>;

/** The set of priorities `press` sent, where it is a priority line's write (`AdvancesSave`), else
 *  `undefined`. That write carries no mutation key, so it is told from every other write by the
 *  revision only it sends. */
function advancesSent(press: unknown): readonly string[] | undefined {
  const save = press as Partial<AdvancesSave> | undefined;
  return typeof save?.variables?.advances_revision === "number"
    ? (save.variables.advances ?? undefined)
    : undefined;
}

/** Whether `a` and `b`, lists that hold no member twice (a set's ids, a set's id and inheritance
 *  pairs), hold the same members, each matched by `same`, in any order. */
function sameMembers<T>(a: readonly T[], b: readonly T[], same: (x: T, y: T) => boolean): boolean {
  return a.length === b.length && a.every((x) => b.some((y) => same(x, y)));
}

/**
 * The hand-offs of the priority's line in Details (`IssueMargin.tsx`), which draws that line in
 * one of two places: beside Add while the issue advances none, and in the Advances row once it
 * advances one. The first priority the reader adds and the last one they remove take the line out
 * of the place that held the focus, as the issue read after that write is drawn, and the focus
 * goes on to the line in its other place rather than to the page. That is the reader's own write
 * only where the newest priority write - the mutation cache lists mutations in the order they were
 * made, and no other line of priorities is drawn on the page - sent the issue's own set now drawn,
 * and is still out, or landed with an answer that is this issue's and holds the revision of that
 * set the issue now drawn holds and every priority it now draws, its own and those an issue above
 * it states: a write to another issue, made on that issue's page, can send the same set and be
 * answered the same revision, a change made elsewhere after it landed draws a newer revision, and
 * a priority an issue above it gains or loses moves the line with neither the issue's own set nor
 * its revision changed. A write still out, or one Dispatch gave no answer for (`null`: a write it
 * made and kept no answer for), cannot be told by issue, since its press carries no more than the
 * set and the revision, so it is matched on the set alone. The line's own `aria-disabled` cannot
 * tell, since the write and the read it sets off as it settles can end while the line is still
 * drawn in its old place, the issue Details draws being drawn after them. A write Dispatch refused
 * (409 ADVANCES_REVISION) or that failed moves nothing once it has ended: the read it sets off as
 * it settles draws the set another session saved, and an event read that lands while the write is
 * out and draws another set moves nothing either. While the write is out, which includes the
 * reread after a refusal, a read that draws the very set it sent is handed on, since the line
 * cannot tell that set from its own write's.
 *
 * Out of the place beside Add the focus goes to the Advances row's picker trigger, "Edit
 * priorities": the trigger beside Add is where the focus is once an add lands, as the picker gives
 * it back as it closes, so the reader stays on the line's one control, whose next press adds
 * another or takes one off. Where the read after the add also draws the issue closed, which
 * disables that trigger, the focus goes to the link of the priority they added. Out of the
 * Advances row it goes to the trigger beside Add, "Add a priority to CORE-1", the one control the
 * line draws there, and where a close drawn in the same read disables that, to
 * `developmentHeading`, the button that opens and closes the section these rows are in. Each
 * trigger is found by its property mark. Details holds the line's write for both places
 * (`useIssuePriorityWrite`), so the trigger in the place the line moves to is held
 * (`aria-disabled`) while the write that moved it still holds its reads, and it takes the focus
 * all the same: a held control keeps the focus by design (`components/held-press.ts`). A trigger
 * that can take no focus - disabled, or under a hidden element (`canHoldFocus`) - passes it on,
 * and the link and the heading take it only where they take a press (`usableControl`).
 *
 * Answers the refs that take the box of the Advances row's value (`advancesValue`) and the row the
 * line stands in beside Add (`addRow`), and the refs that take the line in each place.
 */
export function usePriorityHandOffs(
  issue: Pick<IssueDetails, "advances" | "advances_revision" | "key">,
  developmentHeading: RefObject<HTMLElement | null>
): {
  readonly addRow: RefObject<HTMLDivElement | null>;
  readonly advancesValue: RefObject<HTMLDivElement | null>;
  readonly besideAddLeft: RefCallback<HTMLElement>;
  readonly rowLeft: RefCallback<HTMLElement>;
} {
  const queryClient = useQueryClient();
  const advancesValue = useRef<HTMLDivElement>(null);
  const addRow = useRef<HTMLDivElement>(null);
  const trigger = propertyControlSelector("advances", issue.key);
  const movedByOwnWrite = (): boolean => {
    const newest = queryClient
      .getMutationCache()
      .findAll({ predicate: (mutation) => advancesSent(mutation.state.variables) !== undefined })
      .at(-1);
    const sent = advancesSent(newest?.state.variables);
    const drawn = issue.advances ?? [];
    const own = drawn.filter((entry) => !entry.inherited).map((entry) => entry.id);
    // Compared in any order: the read need not answer the set in the order the write sent it.
    const sameSet = sent !== undefined && sameMembers(sent, own, (x, y) => x === y);
    if (newest === undefined || !sameSet) return false;
    if (newest.state.status === "pending") return true;
    // Dispatch answers the PATCH with the issue it wrote, named by its key, whose revision is that
    // of the set saved and whose priorities are every one it advanced then, inherited ones
    // included.
    const answer = newest.state.data as
      | Pick<Issue, "advances" | "advances_revision" | "key">
      | null
      | undefined;
    const answered = answer?.advances ?? [];
    return (
      newest.state.status === "success" &&
      (answer == null ||
        (answer.key === issue.key &&
          answer.advances_revision === issue.advances_revision &&
          // Each by its id and whether it is inherited, in any order, as the own set above.
          sameMembers(answered, drawn, (x, y) => x.id === y.id && x.inherited === y.inherited)))
    );
  };
  // The line's trigger in `place`, held or not, where it can take the focus.
  const triggerIn = (place: HTMLElement | null): HTMLElement | undefined => {
    const found = place?.querySelector<HTMLElement>(trigger);
    return canHoldFocus(found) ? found : undefined;
  };
  const besideAddLeft = useFocusHandOff(() =>
    movedByOwnWrite()
      ? (triggerIn(advancesValue.current) ??
        firstUsable(advancesValue.current?.querySelector<HTMLElement>("a[href]")))
      : null
  );
  const rowLeft = useFocusHandOff(() =>
    movedByOwnWrite()
      ? (triggerIn(addRow.current) ?? firstUsable(developmentHeading.current))
      : null
  );
  return { addRow, advancesValue, besideAddLeft, rowLeft };
}
