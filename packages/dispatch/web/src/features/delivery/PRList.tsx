import type { ColumnDef } from "@tanstack/react-table";
import { type ReactNode, useMemo, useRef } from "react";

import type { DeliveryPR, DeliverySessionGroup } from "../../api/types";
import {
  dangerText,
  inlineWarningText,
  linkText,
  successText,
  textMutedOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { EvalTable } from "../evals/EvalTable";
import { type ColorFacet, colorKeyFor, PLACEHOLDER_COLOR } from "./lib/colorScale";
import { buildPRListRows, compareIssueSort, formatMinutes, type PRListRow } from "./lib/prList";

const DEPLOYED_TONE: Record<DeliveryPR["deployed_status"], string> = {
  deployed: successText,
  waiting: inlineWarningText,
  not_tracked: textMutedOnSurface,
};

const dateTime = new Intl.DateTimeFormat(undefined, {
  month: "short",
  day: "numeric",
  hour: "numeric",
  minute: "2-digit",
});

/** Newest merge first until the reader sorts by another column. */
const NEWEST_FIRST = [{ desc: true, id: "merged" }];

/** The list's columns, each starting at the width its usual content takes at text-xs ("deployed
 *  Sep 26, 10:48 PM", "ACME-1028 · No priority", a parent agent's title); the table core draws
 *  none narrower than its header. A row with no value for a column (no issue, not deployed, a
 *  placeholder agent) sorts last in either direction, and text compares numeric runs as numbers. */
function prColumns(
  colorBy: ColorFacet,
  colorScale: Map<string, string>,
  selectRun: (runId: number) => void
): ColumnDef<PRListRow>[] {
  return [
    {
      cell: ({ row }) => (
        <span
          aria-hidden="true"
          className="block size-2 rounded-full"
          style={{
            backgroundColor:
              colorScale.get(colorKeyFor(row.original.pr, colorBy)) ?? PLACEHOLDER_COLOR,
          }}
        />
      ),
      enableColumnFilter: false,
      enableSorting: false,
      header: "",
      id: "color",
      size: 28,
    },
    {
      accessorFn: (row) => row.mergedMs,
      cell: ({ row }) =>
        row.original.mergedMs === undefined ? null : (
          <span title={new Date(row.original.mergedMs).toLocaleString()}>
            {dateTime.format(row.original.mergedMs)}
          </span>
        ),
      enableColumnFilter: false,
      header: "Merged",
      id: "merged",
      size: 112,
      sortUndefined: "last",
    },
    {
      accessorFn: (row) => row.prLabel,
      cell: ({ row }) => (
        <a
          className={`hover:underline ${linkText}`}
          href={row.original.pr.url}
          onClick={(event) => event.stopPropagation()}
          rel="noreferrer"
          target="_blank"
          title={row.original.pr.id}
        >
          {row.original.prLabel}
        </a>
      ),
      enableColumnFilter: false,
      header: "PR",
      id: "pr",
      size: 96,
    },
    {
      accessorFn: (row) => row.pr.title,
      cell: ({ row }) => <span title={row.original.pr.title}>{row.original.pr.title}</span>,
      enableColumnFilter: false,
      header: "Title",
      id: "title",
      size: 320,
    },
    {
      accessorFn: (row) => row.issueSort,
      cell: ({ row }) => (
        <span className={row.original.issueSort === undefined ? textMutedOnSurface : ""}>
          {row.original.issueLabel}
        </span>
      ),
      enableColumnFilter: false,
      header: "Issue",
      id: "issue",
      size: 160,
      sortingFn: (a, b) =>
        a.original.issueSort === undefined || b.original.issueSort === undefined
          ? 0
          : compareIssueSort(a.original.issueSort, b.original.issueSort),
      sortUndefined: "last",
    },
    {
      accessorFn: (row) => row.parentAgentSort,
      cell: ({ row }) => (
        <span
          className={row.original.parentAgentSort === undefined ? textMutedOnSurface : ""}
          title={row.original.parentAgentLabel}
        >
          {row.original.parentAgentLabel}
        </span>
      ),
      enableColumnFilter: false,
      header: "Parent agent",
      id: "parentAgent",
      size: 112,
      sortUndefined: "last",
    },
    {
      accessorFn: (row) => row.pr.author,
      enableColumnFilter: false,
      header: "Author",
      id: "author",
      size: 116,
    },
    {
      accessorFn: (row) => row.size,
      cell: ({ row }) => {
        const { additions, deletions } = row.original.pr;
        return additions === null || deletions === null ? null : (
          <span className="tabular-nums">
            <span className={successText}>+{additions}</span>{" "}
            <span className={dangerText}>-{deletions}</span>
          </span>
        );
      },
      enableColumnFilter: false,
      header: "Size",
      id: "size",
      size: 88,
      sortUndefined: "last",
    },
    {
      accessorFn: (row) => row.deployedMs,
      cell: ({ row }) => {
        const { deployedLabel, deployedMs, pr } = row.original;
        const runId = pr.deploy_run;
        const label = <span className={DEPLOYED_TONE[pr.deployed_status]}>{deployedLabel}</span>;
        return (
          <>
            {runId === null ? (
              label
            ) : (
              // The list's way to a deploy's drill-down, which the chart offers only to a pointer.
              <button
                aria-label={`Open deploy run #${runId}`}
                className="underline"
                onClick={(event) => {
                  event.stopPropagation();
                  selectRun(runId);
                }}
                type="button"
              >
                {label}
              </button>
            )}
            {deployedMs === undefined ? null : (
              <span className={textSecondaryOnSurface}> {dateTime.format(deployedMs)}</span>
            )}
          </>
        );
      },
      enableColumnFilter: false,
      header: "Deployed",
      id: "deployed",
      size: 164,
      sortUndefined: "last",
    },
    {
      accessorFn: (row) => row.leadTimeMinutes,
      cell: ({ row }) =>
        row.original.leadTimeMinutes === undefined ? null : (
          <span className="tabular-nums" title="Merge to production">
            {formatMinutes(row.original.leadTimeMinutes)}
          </span>
        ),
      enableColumnFilter: false,
      header: "Lead time",
      id: "leadTime",
      size: 84,
      sortUndefined: "last",
    },
    {
      accessorFn: (row) => row.pr.rework,
      cell: ({ row }) =>
        row.original.pr.rework ? <span className={inlineWarningText}>rework</span> : null,
      enableColumnFilter: false,
      header: "Rework",
      id: "rework",
      size: 72,
    },
  ];
}

/** The filtered PRs as a table of the one table core (`EvalTable`, plan D5), one row per PR,
 *  coloured by the same facet as the timeline, newest merge first until the reader sorts by
 *  another column; no column is drawn narrower than its header (spec R7). A row opens the
 *  DrillDown, and a deployed row's "deployed" opens its deploy run's. The rows are one keyboard
 *  stop: arrow up and down move between them, Home and End jump to the ends, and Enter opens the
 *  focused row. */
export function PRList({
  prs,
  allRepos,
  colorBy,
  colorScale,
  sessionGroups,
  selectedId,
  onSelect,
  onSelectRun,
}: {
  prs: readonly DeliveryPR[];
  allRepos: readonly string[];
  colorBy: ColorFacet;
  colorScale: Map<string, string>;
  sessionGroups: readonly DeliverySessionGroup[];
  selectedId: string | undefined;
  onSelect: (id: string) => void;
  onSelectRun: (runId: number) => void;
}): ReactNode {
  // Read when a row's "deployed" is pressed, so a caller's new handler draws no new columns.
  const selectRunRef = useRef(onSelectRun);
  selectRunRef.current = onSelectRun;
  const rows = useMemo(
    () => buildPRListRows(prs, allRepos, sessionGroups),
    [prs, allRepos, sessionGroups]
  );
  const columns = useMemo(
    () => prColumns(colorBy, colorScale, (runId) => selectRunRef.current(runId)),
    [colorBy, colorScale]
  );
  return (
    <EvalTable
      cells="text"
      columns={columns}
      empty="No PRs in the current filter/window."
      getRowId={(row) => row.pr.id}
      initialSorting={NEWEST_FIRST}
      label="Merged pull requests"
      layoutKey="delivery-pr-list"
      noun={{ many: "pull requests", one: "pull request" }}
      rows={rows}
      scrollTestId="pr-list-scroll"
      select={{ onSelect: (row) => onSelect(row.pr.id), selectedId }}
    />
  );
}
