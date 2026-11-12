import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { prioritiesQuery } from "../../api/queries";
import { LoadingSkeleton } from "../../components/LoadingSkeleton";
import { QueryError } from "../../components/QueryError";
import { textPrimaryOnCanvas, textSecondaryOnCanvas } from "../../theme/classes";
import { FaultList } from "./FaultList";
import { countsLine } from "./fault-model";
import { PrioritiesTable } from "./PrioritiesTable";

/** What each fault is, in the words a reader judges it by. Neither is a judgement: both are
 *  true or false of an issue as it stands now. */
const NO_PRIORITY_RULE =
  "Work in progress, testing, review or retro that advances no priority, its own or one it inherits.";
const PARKED_RULE =
  "Work that advances a priority and stands in the backlog or the icebox, with the time it was moved there and whoever moved it. No reason is recorded for a move yet.";

/** How many rows of a fault the read carries, which is what the count is compared against to
 *  say a list is the newest of more (`GET /api/v1/priorities`). */
function showing(count: number, rows: number): string {
  return rows < count ? `${rows} of ${count} shown, the newest first` : "";
}

/**
 * `/priorities`: the priorities themselves, in their one order, and under them the two faults.
 * A priority belongs to no project, so this page names none: every count, every row and both
 * fault lists are the whole deployment's.
 */
export function PrioritiesPage(): ReactNode {
  const priorities = useQuery(prioritiesQuery());
  if (priorities.isPending) return <LoadingSkeleton label="Loading priorities" />;
  if (priorities.isError)
    return (
      <QueryError
        error={priorities.error}
        message="Could not load the priorities."
        onRetry={() => void priorities.refetch()}
        retrying={priorities.isFetching}
      />
    );
  const body = priorities.data;
  const rows = body.priorities ?? [];
  const unpriorited = body.work_under_no_priority;
  const parked = body.priority_work_parked;
  const unprioritedRows = unpriorited.issues ?? [];
  const parkedRows = parked.issues ?? [];
  // Every column and every count line of this page names a priority by the title a person
  // wrote, never by the id, which is a slug of that title and no name at all to a reader.
  const titles = new Map(rows.map((row) => [row.id, row.title]));
  const parkedByPriority = Object.fromEntries(
    Object.entries(parked.by_priority ?? {}).map(([id, count]) => [titles.get(id) ?? id, count])
  );
  return (
    <div className="space-y-8">
      <header className="space-y-1">
        <h1 className={`text-2xl font-semibold ${textPrimaryOnCanvas}`}>Priorities</h1>
        <p className={textSecondaryOnCanvas}>
          Every project, read now. The order is the one their owner drags.
        </p>
      </header>

      {rows.length === 0 ? (
        <p className={textSecondaryOnCanvas}>{body.empty ?? "No priority has been written yet."}</p>
      ) : (
        <PrioritiesTable rows={rows} />
      )}

      <section aria-label="In flight under no priority" className="space-y-2">
        <h2 className={`text-lg font-semibold ${textPrimaryOnCanvas}`}>
          In flight under no priority
        </h2>
        <p className={textSecondaryOnCanvas}>{NO_PRIORITY_RULE}</p>
        <p className={textSecondaryOnCanvas}>
          {unpriorited.count} {unpriorited.count === 1 ? "issue" : "issues"}
          {countsLine(unpriorited.by_project) === ""
            ? ""
            : ` · ${countsLine(unpriorited.by_project)}`}
          {showing(unpriorited.count, unprioritedRows.length) === ""
            ? ""
            : ` · ${showing(unpriorited.count, unprioritedRows.length)}`}
        </p>
        <FaultList
          issues={unprioritedRows}
          kind="no-priority"
          label="In flight under no priority"
          prefix="n"
          titles={titles}
        />
      </section>

      <section aria-label="Priority work in backlog or icebox" className="space-y-2">
        <h2 className={`text-lg font-semibold ${textPrimaryOnCanvas}`}>
          Priority work in backlog or icebox
        </h2>
        <p className={textSecondaryOnCanvas}>{PARKED_RULE}</p>
        <p className={textSecondaryOnCanvas}>
          {parked.count} {parked.count === 1 ? "issue" : "issues"}
          {countsLine(parkedByPriority) === "" ? "" : ` · ${countsLine(parkedByPriority)}`}
          {showing(parked.count, parkedRows.length) === ""
            ? ""
            : ` · ${showing(parked.count, parkedRows.length)}`}
        </p>
        <FaultList
          issues={parkedRows}
          kind="parked"
          label="Priority work in backlog or icebox"
          prefix="k"
          titles={titles}
        />
      </section>
    </div>
  );
}
