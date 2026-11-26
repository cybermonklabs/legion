import type { UseQueryResult } from "@tanstack/react-query";
import { type ReactNode, useRef } from "react";

import type { WriteBounds } from "../../api/client";
import { useReadBound } from "../../api/read-bound";
import { ARM_ON_PRESS, useFocusPlace } from "../../components/focus-place";
import { heldPress } from "../../components/held-press";
import { QueryError } from "../../components/QueryError";
import {
  TableBody,
  TableCell,
  TableEmpty,
  TableHead,
  TableHeadCell,
  TableRow,
  TableShell,
} from "../../components/TableShell";
import { useReleaseWhenLate, useSubmitGuard } from "../../hooks/useSubmitGuard";
import {
  dangerText,
  hoverToDangerText,
  secondaryButtonBorder,
  secondaryButtonDisabledText,
  secondaryButtonHoverBorder,
  secondaryButtonText,
  textMutedOnCanvas,
  textMutedOnSurface,
  textPrimaryOnCanvas,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { SendStoppedError, stoppedWriteNote } from "../writes/keyed-write";
import { refusedNote, useKeyedWrite, useRefusalOfSubject } from "../writes/useKeyedWrite";

/** One column of a `RevocableList`: its header, and what a row shows under it. The first column
 *  names the row. */
export interface RevocableColumn<Row> {
  cell: (row: Row) => ReactNode;
  header: string;
}

/**
 * The revocable table `GrantsSection` and `MachineLoginsSection` both render: a heading and what
 * the list holds, the list's loading, failed and listed states, one row per item with a Revoke
 * button, and a refused revoke's broker message under the table (a `NOT_APPROVER` 403 among them),
 * so no refusal is silent. One line under the table speaks for every row, so it names the row the
 * revoke was of (`rowName`), and it goes once that row leaves the list - revoked elsewhere, or
 * expired - rather than stand under rows it was never about. Each section hands over its own
 * query, columns and revoke call, and `confirm` when a revoke asks first; the list owns the one
 * revoke in flight: every Revoke button is held while it is pending (`heldPress`), never disabled,
 * so the pressed Revoke keeps the focus and a press of any Revoke does nothing, and the submit
 * guard drops a second press that lands before that re-render (a same-tick double click).
 * `onRevoked` runs once a revoke succeeds, and answers the read that refreshes what it changed:
 * the buttons stay held through that read, up to the request deadline (`useReadBound`), and a
 * read that has not settled by then lets them go, with the line under the table saying why the
 * list is not read yet. That read takes the revoked row away, and with it the Revoke that held the
 * focus, which the browser would leave on the page's body. So the list hands that focus on itself
 * (`useFocusPlace`), to the control now at the Revoke's place, or the nearest one before it, that
 * the reader can see: the press arms that hand-off at the list's root (`ARM_ON_PRESS`), and the
 * hold through the read keeps the press the cause of the row's going. A section may also list
 * rows the broker lets nobody revoke: `unrevocable` names why for such a row, which is then
 * read-only, with that line where its button would be, so the page offers no button that can
 * only fail.
 */
export function RevocableList<Row>({
  className,
  columns,
  confirm,
  description,
  empty,
  heading,
  headingId,
  noun,
  onRevoked,
  query,
  revoke,
  rowKey,
  rowName,
  unrevocable,
}: {
  className: string;
  columns: readonly RevocableColumn<Row>[];
  confirm?: (row: Row) => string;
  description: ReactNode;
  empty: string;
  heading: string;
  headingId: string;
  noun: string;
  /** Refreshes what a revoke that succeeded changed, and answers that read, waited on through a
   *  fetch the network has paused (`invalidateWaitingForPaused`). */
  onRevoked: () => Promise<unknown>;
  query: UseQueryResult<readonly Row[]>;
  /** One attempt of `row`'s revoke - a `POST` of its revoke route - under the write's key. */
  revoke: (row: Row, bounds: WriteBounds) => Promise<void>;
  rowKey: (row: Row) => string;
  /** `row` as the list's sentences of its revoke name it: `the machine login for devbox`. */
  rowName: (row: Row) => string;
  /** Why nobody may revoke `row`, in place of its Revoke button, and null for a row anybody
   *  listed here may revoke. A section gives it where the broker refuses every person's revoke
   *  of some rows it lists, so the page never offers a button that can only fail. */
  unrevocable?: (row: Row) => string | null;
}): ReactNode {
  const submitGuard = useSubmitGuard();
  // Why the read the latest landed revoke made has not settled, once its hold ran out: the next
  // revoke takes it away as it lands (`useReadBound`).
  const [unsettled, holdThrough] = useReadBound();
  // What the list says of a read that failed, and of the read a landed revoke made that has not
  // settled.
  const notLoaded = `Couldn't load ${noun}.`;
  // The list shows a refused revoke itself, under the table.
  const mutation = useKeyedWrite({
    inline: true,
    label: (row: Row) => `${heading}: revoke ${rowKey(row)}`,
    onSettled: () => submitGuard.release(),
    // Pending, and every Revoke held, through the read that takes the revoked row away.
    onSuccess: () => holdThrough(onRevoked()),
    method: "POST",
    readsLate: true,
    write: revoke,
  });
  useReleaseWhenLate(submitGuard, mutation.late);
  const refused = useRefusalOfSubject(
    mutation,
    (row) => query.data?.some((listed) => rowKey(listed) === rowKey(row)) === true
  )
    ? mutation.variables
    : undefined;
  const press = (row: Row) => {
    if (confirm === undefined || window.confirm(confirm(row))) {
      submitGuard.guard(() => mutation.mutate(row));
    }
  };
  // The list's own hand-off for the Revoke its row takes away, counted among the list's controls;
  // it names no fallback, so a list left with no control before that place walks back over the
  // page's main content.
  const list = useRef<HTMLElement>(null);
  const noteFocus = useFocusPlace(
    () => list.current,
    () => null
  );

  return (
    <section
      aria-labelledby={headingId}
      className={className}
      {...ARM_ON_PRESS}
      onFocus={noteFocus}
      ref={list}
    >
      <h2 className={`text-xl font-semibold ${textPrimaryOnCanvas}`} id={headingId}>
        {heading}
      </h2>
      <p className={`mt-1 text-sm ${textSecondaryOnCanvas}`}>{description}</p>

      {query.isPending ? <p className={`mt-6 ${textMutedOnCanvas}`}>Loading {noun}…</p> : null}
      {query.isError ? (
        <div className="mt-6">
          <QueryError
            message={notLoaded}
            onRetry={() => void query.refetch()}
            retrying={query.isFetching}
          />
        </div>
      ) : null}
      {query.isSuccess ? (
        <TableShell className="mt-6" label={heading}>
          <TableHead>
            {columns.map((column) => (
              <TableHeadCell key={column.header}>{column.header}</TableHeadCell>
            ))}
            <TableHeadCell>
              <span className="sr-only">Actions</span>
            </TableHeadCell>
          </TableHead>
          <TableBody>
            {query.data.length === 0 ? (
              <TableEmpty colSpan={columns.length + 1}>{empty}</TableEmpty>
            ) : (
              query.data.map((row) => (
                <TableRow key={rowKey(row)}>
                  {columns.map((column, index) => (
                    <TableCell
                      key={column.header}
                      tone={index === 0 ? "primary" : "secondary"}
                      weight={index === 0 ? "medium" : "normal"}
                    >
                      {column.cell(row)}
                    </TableCell>
                  ))}
                  <TableCell align="right">
                    {unrevocable?.(row) ? (
                      <span className={`text-xs whitespace-nowrap ${textMutedOnSurface}`}>
                        {unrevocable(row)}
                      </span>
                    ) : (
                      <button
                        className={`min-h-11 rounded-lg border px-3 py-1 text-xs font-medium whitespace-nowrap refused:cursor-not-allowed refused:opacity-50 ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder} ${secondaryButtonDisabledText} ${hoverToDangerText}`}
                        {...heldPress(mutation.isPending && !mutation.late, () => press(row))}
                        type="button"
                      >
                        Revoke
                      </button>
                    )}
                  </TableCell>
                </TableRow>
              ))
            )}
          </TableBody>
        </TableShell>
      ) : null}
      {/* A revoke the reader stopped may have been made, and Revoke is a press of its own, under
          a key of its own: it never makes the stopped revoke again. */}
      {refused === undefined ? null : (
        <p className={`mt-2 ${dangerText}`}>
          {mutation.error instanceof SendStoppedError
            ? stoppedWriteNote(`the revoke of ${rowName(refused)}`, "made", {
                anew: "Pressing Revoke again sends a new revoke.",
              })
            : refusedNote(`revoke ${rowName(refused)}`, mutation.error)}
        </p>
      )}
      {/* A list read that has not settled is said as the list says a list that failed to load. */}
      {refused === undefined && unsettled !== undefined ? (
        <p className={`mt-2 ${dangerText}`}>
          {notLoaded} {unsettled}.
        </p>
      ) : null}
    </section>
  );
}
