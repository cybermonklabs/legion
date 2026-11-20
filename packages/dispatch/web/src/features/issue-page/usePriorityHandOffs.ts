import { useQueryClient } from "@tanstack/react-query";
import { type RefCallback, type RefObject, useRef } from "react";

import type { Issue, IssueDetails, UpdateIssueInput } from "../../api/types";
import { useFocusHandOff } from "../../hooks/useFocusHandOff";
import { addPriorityControlName } from "../priorities/IssuePriorityLine";
import type { KeyedPress } from "../writes/useKeyedWrite";
import { firstUsable } from "./usable-control";

/** The priority line's write as the mutation cache holds it (`IssuePriorityLine`): the press its
 *  keyed write made (`KeyedPress`), with the set of the issue's own priorities it sends and the
 *  revision of the read that set was drawn from, as the issue's PATCH body names them. */
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

/**
 * The hand-offs of the priority's line in Details (`IssueMargin.tsx`), which draws that line in
 * one of two places: beside Add while the issue advances none, and in the Advances row once it
 * advances one. The first priority the reader adds and the last one they remove take the line out
 * of the place that held the focus, as the issue read after that write is drawn, and the focus
 * goes on to the line in its other place rather than to the page. That is the reader's own write
 * only where the newest priority write - the mutation cache lists mutations in the order they were
 * made, and no other line of priorities is drawn on the page - sent the issue's own set now drawn,
 * and is still out, or landed with an answer that holds the revision of that set the issue now
 * drawn holds: a change made elsewhere after it landed draws a newer revision. An answer Dispatch
 * did not give (`null`: a write it made and kept no answer for) is matched on the set alone. The
 * line's own `aria-disabled` cannot tell, since the write and the read it sets off as it settles
 * can end while the line is still drawn in its old place, the issue Details draws being drawn
 * after them. A write Dispatch refused (409 ADVANCES_REVISION) or that failed moves nothing once
 * it has ended: the read it sets off as it settles draws the set another session saved, and an
 * event read that lands while the reader's write is out draws another session's set too, so the
 * focus stays on the page's body. While that read is out the write still counts as out, so a
 * read that draws the very set it sent is handed on, since the line cannot tell that set from
 * its own write's.
 *
 * Out of the place beside Add the focus goes to the Advances row's add control: the add control
 * beside Add is where the focus is once an add lands, so the reader stays on the control of that
 * name, whose next press adds another, with Remove of what they added just before it. An add from
 * beside Add sends one priority, under the cap, so the row draws that control; where the read
 * after it also draws the issue closed, which disables it, the focus goes to the link of the
 * priority they added. Out of the Advances row it goes to the add control beside Add, the one
 * control the line draws there, and where a close drawn in the same read disables that, to
 * `developmentHeading`, the button that opens and closes the section these rows are in. Each
 * passes over a control that takes no press (`usableControl`).
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
  const addSelector = `[aria-label="${CSS.escape(addPriorityControlName(issue.key))}"]`;
  const movedByOwnWrite = (): boolean => {
    const newest = queryClient
      .getMutationCache()
      .findAll({ predicate: (mutation) => advancesSent(mutation.state.variables) !== undefined })
      .at(-1);
    const sent = advancesSent(newest?.state.variables);
    const own = (issue.advances ?? []).filter((entry) => !entry.inherited).map((entry) => entry.id);
    // Compared in any order: the read need not answer the set in the order the write sent it.
    const sameSet = sent?.length === own.length && sent.every((id) => own.includes(id));
    if (newest === undefined || !sameSet) return false;
    if (newest.state.status === "pending") return true;
    // Dispatch answers the PATCH with the issue it wrote, whose revision is that of the set saved.
    const answer = newest.state.data as Pick<Issue, "advances_revision"> | null | undefined;
    return (
      newest.state.status === "success" &&
      (answer == null || answer.advances_revision === issue.advances_revision)
    );
  };
  const besideAddLeft = useFocusHandOff(() =>
    movedByOwnWrite()
      ? firstUsable(
          advancesValue.current?.querySelector<HTMLElement>(addSelector),
          advancesValue.current?.querySelector<HTMLElement>("a[href]")
        )
      : null
  );
  const rowLeft = useFocusHandOff(() =>
    movedByOwnWrite()
      ? firstUsable(
          addRow.current?.querySelector<HTMLElement>(addSelector),
          developmentHeading.current
        )
      : null
  );
  return { addRow, advancesValue, besideAddLeft, rowLeft };
}
