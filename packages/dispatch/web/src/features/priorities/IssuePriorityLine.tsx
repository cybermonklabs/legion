import { MAX_ISSUE_ADVANCES } from "@legion/contracts";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { type FormEvent, type ReactNode, useId, useMemo, useState } from "react";
import { Link } from "react-router-dom";

import { apiErrorMessage, issuePatchRequest } from "../../api/client";
import { prioritiesQuery } from "../../api/queries";
import type { Issue, PriorityRow } from "../../api/types";
import {
  dangerText,
  dismissButtonText,
  inputClasses,
  linkHoverText,
  linkText,
  secondaryButtonBorder,
  secondaryButtonDisabledText,
  secondaryButtonText,
  surfaceMutedStrongBg,
  textMutedHoverToSecondary,
  textMutedOnSurface,
  textSecondaryHoverToPrimary,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { useKeyedWrite } from "../writes/useKeyedWrite";
import { orderLabel } from "./priorities-model";
import { priorityPath } from "./priority-routes";

/** What the line calls each priority the read answered, by its id: its order and its title, as
 *  the priorities page names it ("P7 Rocq and Lean speed work restarted"). The read answers the
 *  priorities in rank order, so the order is the row's place in that one list, named by the one
 *  rule the table names it by (`orderLabel`). A priority this map does not hold is named by its
 *  own id, so a line never waits on a second read to say which priority it means. */
function priorityLabels(rows: readonly PriorityRow[]): ReadonlyMap<string, string> {
  return new Map(rows.map((row, index) => [row.id, `${orderLabel(index)} ${row.title}`]));
}

/**
 * The `Advances` item of the issue header's metadata rail (INFRA-2947, R16 and R3): each
 * priority this issue advances, named by its order and title and linking to that priority's own
 * page, with the control that adds one beside them and a remove on each.
 *
 * An issue that advances none carries no label and no empty line — only the control that places
 * it under a priority, which is how a person acts on a row of the page's first fault list. A
 * priority inherited from an issue above (R4) says so and carries no remove: that relation is
 * the ancestor's, and a relation on a child never places the issues above it.
 *
 * Every press writes one relation: `PATCH /api/v1/issues/{key}` carrying the whole of the
 * issue's own set, which moves one relation and so needs no `advances_revision` — the server's
 * rule is that a writer sending no revision asserts or retracts one relation at a time
 * (`api/issue_advances.go`, `refuseAdvancesRevision`). The list of priorities to choose from is
 * read only once the line has an entry to name or the control is open, as the components line
 * reads the architecture tree only while its picker is open.
 */
export function IssuePriorityLine({
  disabled,
  issue,
}: {
  disabled: boolean;
  issue: Pick<Issue, "advances" | "key">;
}): ReactNode {
  const advances = issue.advances ?? [];
  const own = advances.filter((entry) => !entry.inherited).map((entry) => entry.id);
  const [adding, setAdding] = useState(false);
  const [picked, setPicked] = useState("");
  const selectId = useId();
  const queryClient = useQueryClient();
  const priorities = useQuery({
    ...prioritiesQuery(),
    enabled: adding || advances.length > 0,
  });
  const rows = useMemo(() => priorities.data?.priorities ?? [], [priorities.data]);
  const labels = useMemo(() => priorityLabels(rows), [rows]);
  const write = useKeyedWrite({
    // The line says why a write was refused, beside the control it came from.
    inline: true,
    label: () => `Advances of ${issue.key}`,
    // One write to the issue, in press order with every other (`issue:<KEY>`).
    target: () => `issue:${issue.key}`,
    request: (ids: string[]) => issuePatchRequest(issue.key, { advances: ids }),
    onSettled: () => {
      // Every open issue, this one included: a descendant inherits the change with no event of
      // its own. The priorities body and the rail's two counts move with the relation as well.
      void queryClient.invalidateQueries({ queryKey: ["issue"] });
      void queryClient.invalidateQueries({ queryKey: ["priorities"] });
    },
  });
  const busy = disabled || write.isPending;
  // A priority the issue already advances is no choice: the line names it, whether the relation
  // is the issue's own or an ancestor's.
  const choices = rows.filter((row) => !advances.some((entry) => entry.id === row.id));
  const atCap = own.length >= MAX_ISSUE_ADVANCES;

  const add = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (picked === "") return;
    write.mutate([...own, picked]);
    setPicked("");
    setAdding(false);
  };

  return (
    <div
      className={`flex shrink-0 items-center gap-2 text-sm ${textSecondaryOnSurface}`}
      data-testid="issue-advances"
    >
      {advances.length === 0 ? null : <span className="font-medium">Advances</span>}
      {advances.map((entry) => {
        const label = labels.get(entry.id) ?? entry.id;
        return (
          <span className="flex shrink-0 items-center gap-1" key={entry.id}>
            <Link
              className={`dispatch-inline-link shrink-0 whitespace-nowrap underline ${linkText} ${linkHoverText}`}
              title={label}
              to={priorityPath(entry.id)}
            >
              {label}
            </Link>
            {entry.inherited ? (
              <span className={`shrink-0 text-xs ${textMutedOnSurface}`}>inherited</span>
            ) : (
              <button
                aria-label={`Remove ${label} from ${issue.key}`}
                className={`shrink-0 ${dismissButtonText}`}
                disabled={busy}
                onClick={() => write.mutate(own.filter((id) => id !== entry.id))}
                type="button"
              >
                ✕
              </button>
            )}
          </span>
        );
      })}
      {atCap ? (
        <span className={`shrink-0 text-xs ${textMutedOnSurface}`}>
          An issue advances at most {MAX_ISSUE_ADVANCES} priorities.
        </span>
      ) : adding ? (
        <form className="flex shrink-0 items-center gap-2" onSubmit={add}>
          <label className="sr-only" htmlFor={selectId}>
            Priority to advance
          </label>
          <select
            className={`min-h-11 max-w-56 min-w-0 shrink-0 truncate rounded-lg px-2 py-1 text-sm md:min-h-8 ${inputClasses(true)}`}
            id={selectId}
            onChange={(event) => setPicked(event.target.value)}
            value={picked}
          >
            <option value="">
              {priorities.data === undefined ? "Loading priorities…" : "Choose a priority"}
            </option>
            {choices.map((row) => (
              <option key={row.id} value={row.id}>
                {labels.get(row.id) ?? row.id}
              </option>
            ))}
          </select>
          <button
            className={`min-h-11 rounded px-2 py-1 text-sm font-medium disabled:cursor-not-allowed md:min-h-8 ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonDisabledText}`}
            disabled={busy || picked === ""}
            type="submit"
          >
            Add
          </button>
          <button
            className={`min-h-11 rounded px-2 py-1 text-sm font-medium md:min-h-8 ${textMutedHoverToSecondary}`}
            onClick={() => {
              setPicked("");
              setAdding(false);
            }}
            type="button"
          >
            Cancel
          </button>
        </form>
      ) : (
        <button
          // One character and its name, as the remove control beside it is: an issue that
          // advances nothing still carries this control, and a word here puts the issue
          // header's metadata rail over its width on a desktop, which would leave every bare
          // issue's rail scrolling and faded (e2e/issue.e2e.ts, "the details rail fades its
          // trailing edge only while it has somewhere to scroll"). A control whose whole label
          // is an icon carries no visible text for WCAG 2.5.3 to match. The character is a
          // plain "+" (U+002B): the fullwidth "＋" this line held draws as an empty box where
          // no CJK font is installed, which is this pod's Chromium and some readers' browsers.
          aria-label={`Add a priority to ${issue.key}`}
          className={`inline-flex min-h-11 shrink-0 items-center rounded-full px-2 py-2 text-sm whitespace-nowrap outline-none ${surfaceMutedStrongBg} ${textSecondaryOnSurface} ${textSecondaryHoverToPrimary}`}
          disabled={busy}
          onClick={() => setAdding(true)}
          title={`Add a priority to ${issue.key}`}
          type="button"
        >
          +
        </button>
      )}
      {priorities.isError ? (
        <span className={`shrink-0 text-xs ${dangerText}`} role="alert">
          Could not load the priorities.
        </span>
      ) : null}
      {write.error === null ? null : (
        <span className={`shrink-0 text-xs ${dangerText}`} role="alert">
          {apiErrorMessage(write.error, "Could not save what this issue advances.")}
        </span>
      )}
    </div>
  );
}
