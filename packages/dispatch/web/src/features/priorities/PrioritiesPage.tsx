import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useMemo } from "react";
import { prioritiesQuery } from "../../api/queries";
import { LoadingSkeleton } from "../../components/LoadingSkeleton";
import { QueryError } from "../../components/QueryError";
import { textPrimaryOnCanvas, textSecondaryOnCanvas } from "../../theme/classes";
import { IssuesTable } from "../project/IssuesTable";
import type { ShownColumnId } from "../project/issues-table-columns";
import type { IssueListing } from "../project/issues-table-data";
import { runningIssueStatuses } from "../project/issues-table-model";
import { NewPriority } from "./NewPriority";
import { PrioritiesTable } from "./PrioritiesTable";
import { PARKED_STATUSES } from "./priorities-model";

/** What each fault is, in the words a reader judges it by. Neither is a judgement: both are
 *  true or false of an issue as it stands now. */
const NO_PRIORITY_RULE =
  "Work in progress, testing, review or retro that advances no priority, its own or one it inherits.";
const PARKED_RULE =
  "Work that advances a priority and stands in the backlog or the icebox, with the time it was moved there and whoever moved it. No reason is recorded for a move yet.";

/** The two faults, as the issue listing answers each: the work being done under no priority,
 *  in the statuses an issue is worked in, and the work under a priority standing in backlog or
 *  icebox, with the move that put it there. */
const NO_PRIORITY: IssueListing = { advances: "none", statuses: runningIssueStatuses };
const PARKED: IssueListing = { advances: "any", statuses: PARKED_STATUSES, moved: true };

/** Each fault list's own stored layout and the columns it opens with shown. Both name each row's
 *  project, since neither stands on one, and when the issue last changed, which is how long it
 *  has stood as it is (R6); the parked list names what the work advances and when it was moved
 *  there besides. The list under no priority has no Advances column at all: every row of it
 *  advances nothing. */
const NO_PRIORITY_LAYOUT = {
  key: "priorities-no-priority",
  shown: ["project", "updated"] satisfies ShownColumnId[],
};
const PARKED_LAYOUT = {
  key: "priorities-parked",
  shown: ["project", "advances", "moved", "updated"] satisfies ShownColumnId[],
};

/** The per-project or per-priority counts under a fault's rule, in plain words: `CORE 12 ·
 *  OPS 7`, greatest first, which is what says where the fault is. */
function countsLine(by: Readonly<Record<string, number>> | null): string {
  return Object.entries(by ?? {})
    .sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0]))
    .map(([name, count]) => `${name} ${count}`)
    .join(" · ");
}

/**
 * `/priorities`: the priorities themselves, in their one order, and under them the two faults.
 * A priority belongs to no project, so this page names none: every count, every row and both
 * fault lists are the whole deployment's. Each fault list is the one Issues table, over the
 * issue listing the server narrows for it, under its own prefix in the address.
 */
export function PrioritiesPage(): ReactNode {
  const priorities = useQuery(prioritiesQuery());
  // What the page draws of one read, built once that read answers rather than once a render:
  // the titles are what the parked list's columns are declared over, so a map built afresh
  // would rebuild that table's columns on every refetch of anything.
  //
  // Every column and every count line of this page names a priority by the title a person
  // wrote, never by the id, which is a slug of that title and no name at all to a reader.
  const { parkedByPriority, rows, titles } = useMemo(() => {
    const read = priorities.data;
    const priorityRows = read?.priorities ?? [];
    const byId = new Map(priorityRows.map((row) => [row.id, row.title]));
    return {
      rows: priorityRows,
      titles: byId,
      parkedByPriority: Object.fromEntries(
        Object.entries(read?.priority_work_parked.by_priority ?? {}).map(([id, count]) => [
          byId.get(id) ?? id,
          count,
        ])
      ),
    };
  }, [priorities.data]);
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
  const unpriorited = body.work_under_no_priority;
  const parked = body.priority_work_parked;
  const unprioritedCounts = countsLine(unpriorited.by_project);
  const parkedCounts = countsLine(parkedByPriority);
  return (
    <div className="space-y-8">
      <header className="space-y-1">
        <h1 className={`text-2xl font-semibold ${textPrimaryOnCanvas}`}>Priorities</h1>
        <p className={textSecondaryOnCanvas}>
          Every project, read now. The order is the one their owner drags.
        </p>
      </header>

      {rows.length === 0 ? (
        <div className="space-y-3">
          <p className={textSecondaryOnCanvas}>
            {body.empty ?? "No priority has been written yet."}
          </p>
          {/* The page with no priority is where the first is written. */}
          <div className="flex flex-wrap items-center gap-2">
            <NewPriority />
          </div>
        </div>
      ) : (
        <PrioritiesTable rows={rows} titles={titles} />
      )}

      <section aria-label="In flight under no priority" className="space-y-2">
        <h2 className={`text-lg font-semibold ${textPrimaryOnCanvas}`}>
          In flight under no priority
        </h2>
        <p className={textSecondaryOnCanvas}>{NO_PRIORITY_RULE}</p>
        <p className={textSecondaryOnCanvas}>
          {unpriorited.count} {unpriorited.count === 1 ? "issue" : "issues"}
          {unprioritedCounts === "" ? "" : ` · ${unprioritedCounts}`}
        </p>
        <IssuesTable
          label="Issues in flight under no priority"
          layout={NO_PRIORITY_LAYOUT}
          listing={NO_PRIORITY}
          stateKey="n"
        />
      </section>

      <section aria-label="Priority work in backlog or icebox" className="space-y-2">
        <h2 className={`text-lg font-semibold ${textPrimaryOnCanvas}`}>
          Priority work in backlog or icebox
        </h2>
        <p className={textSecondaryOnCanvas}>{PARKED_RULE}</p>
        <p className={textSecondaryOnCanvas}>
          {parked.count} {parked.count === 1 ? "issue" : "issues"}
          {parkedCounts === "" ? "" : ` · ${parkedCounts}`}
        </p>
        <IssuesTable
          label="Priority issues in backlog or icebox"
          layout={PARKED_LAYOUT}
          listing={PARKED}
          priorityTitles={titles}
          stateKey="k"
        />
      </section>
    </div>
  );
}
