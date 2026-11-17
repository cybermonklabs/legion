import { useQuery } from "@tanstack/react-query";
import { Fragment, type ReactNode } from "react";

import { issueStepsQuery } from "../../api/queries";
import { EmptyState } from "../../components/EmptyState";
import { QueryError } from "../../components/QueryError";
import {
  badgeMed,
  borderDefault,
  card,
  dangerText,
  successText,
  surfaceMutedBg,
  textMutedOnCanvas,
  textMutedOnSurface,
  textPrimaryOnSurface,
  textSecondaryOnCanvas,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { type StepRow, stepTasks } from "./steps-model";

const COLUMNS = [
  "Step",
  "Role",
  "Type",
  "Agent",
  "Model",
  "Attempt",
  "Started",
  "Ended",
  "Result",
] as const;

/** A field the record does not hold: a merge GitHub made has no agent, a run that has not
 *  answered yet has no model, a run that started before the release that records steps has no
 *  start. */
const NONE = "—";

function Cell({ children, className = "" }: { children: ReactNode; className?: string }) {
  return <td className={`px-1.5 py-1.5 align-top ${className}`}>{children}</td>;
}

/** A run's start and end are read against each other, down the table, so each is the instant
 *  itself (`Nov 16 8:00 AM`) rather than a distance from now, which would make a forty-minute run
 *  and the one after it read alike. The date and the time each stay whole, so a narrow column
 *  puts the time under the date and never splits either; the year shows when it is not this one,
 *  and the whole date and time is the title. */
const day = new Intl.DateTimeFormat(undefined, { day: "numeric", month: "short" });
const dayOfAnotherYear = new Intl.DateTimeFormat(undefined, {
  day: "numeric",
  month: "short",
  year: "numeric",
});
const timeOfDay = new Intl.DateTimeFormat(undefined, { hour: "numeric", minute: "2-digit" });

function Time({ at }: { at: string | null }): ReactNode {
  if (at === null) return NONE;
  const date = new Date(at);
  if (Number.isNaN(date.getTime())) return <time>{at}</time>;
  const dayFormat = date.getFullYear() === new Date().getFullYear() ? day : dayOfAnotherYear;
  return (
    <time dateTime={at} title={date.toLocaleString()}>
      <span className="whitespace-nowrap">{dayFormat.format(date)}</span>{" "}
      <span className="whitespace-nowrap">{timeOfDay.format(date)}</span>
    </time>
  );
}

/** A part of an id that cannot stand on a line alone: a number (`2`, `3570`, `6.1`) or one
 *  character. It stays with the part before it, so `core-2` and `gpt-6.1` never break. */
const CLINGS = /^(?:[\d.]+|.)$/u;

/** One `/`-free piece of an id: its hyphen-joined parts in groups, a group being a part and every
 *  clinging part after it. The hyphen between two groups is where a line may break; inside a
 *  group it may not. Each group is keyed by its first part's place in the piece. */
function HyphenGroups({ piece }: { piece: string }): ReactNode {
  const groups: { at: number; parts: string[] }[] = [];
  for (const [at, part] of piece.split("-").entries()) {
    const last = groups.at(-1);
    if (last !== undefined && CLINGS.test(part)) last.parts.push(part);
    else groups.push({ at, parts: [part] });
  }
  return groups.map(({ at, parts }, index) => (
    <Fragment key={at}>
      {index === 0 ? null : "-"}
      {parts.length === 1 ? parts[0] : <span className="whitespace-nowrap">{parts.join("-")}</span>}
    </Fragment>
  ));
}

/** An agent token or a model id, or the dash for one the record does not hold. A token breaks at
 *  its hyphens; a model id (`anthropic/claude-opus-5`) is also offered a break after each slash,
 *  so neither forces its column wider than its longest part. A number or a single letter keeps
 *  to the part before it, so no line holds it alone (`legion-core-core-2-…` never leaves `2-`). */
function Identifier({ value }: { value: string }): ReactNode {
  if (value === "") return NONE;
  const pieces = value.split("/");
  // Each piece is keyed by the text up to and including it, which no other piece of the id shares.
  return pieces.map((piece, index) => {
    const through = pieces.slice(0, index + 1).join("/");
    return (
      <Fragment key={through}>
        {index === 0 ? null : (
          <>
            <wbr />/
          </>
        )}
        <HyphenGroups piece={piece} />
      </Fragment>
    );
  });
}

/** How the run ended, or that it runs: a failed or stopped run that sent its task back names the
 *  step it sent it to, and a result this build has no word for shows the server's own. `step 2`
 *  stays whole, as the times do, so a narrow column never leaves the number on a line alone. */
function Result({ row }: { row: StepRow }): ReactNode {
  switch (row.state) {
    case "running":
      return (
        <span
          className={`rounded-sm px-1.5 py-0.5 text-xs font-medium ${badgeMed.bg} ${badgeMed.text}`}
        >
          running
        </span>
      );
    case "passed":
      return <span className={`font-medium ${successText}`}>passed</span>;
    case "other":
      return row.result;
    case "failed":
    case "stopped":
      return (
        <>
          <span className={`font-medium ${row.state === "failed" ? dangerText : ""}`}>
            {row.state}
          </span>
          {row.returnedTo === null ? null : (
            <span className={textMutedOnSurface}>
              , returned to <span className="whitespace-nowrap">step {row.returnedTo}</span>
            </span>
          )}
        </>
      );
  }
}

/** One task's runs, one row each, in the order the server read them. */
function TaskTable({ rows, task }: { rows: readonly StepRow[]; task: string }): ReactNode {
  return (
    <section className="min-w-0 space-y-2">
      <h2 className={`text-sm font-semibold ${textSecondaryOnCanvas}`}>Task {task}</h2>
      {/* `relative` keeps the scrolled table's cells inside this box, as the Settings tables do
          (`features/settings/classes.ts`); the table scrolls sideways where the page is narrower
          than its columns. */}
      <div className={`relative overflow-x-auto rounded-xl border ${card}`}>
        <table aria-label={`Steps of task ${task}`} className="w-full text-left text-xs">
          <thead
            className={`border-b ${surfaceMutedBg} ${borderDefault} ${textSecondaryOnSurface}`}
          >
            <tr>
              {COLUMNS.map((column) => (
                <th
                  className="px-1.5 py-1.5 font-semibold whitespace-nowrap"
                  key={column}
                  scope="col"
                >
                  {column}
                </th>
              ))}
            </tr>
          </thead>
          <tbody>
            {rows.map((row) => (
              <tr className={`border-b last:border-0 ${borderDefault}`} key={row.id}>
                <Cell className={`font-medium whitespace-nowrap ${textPrimaryOnSurface}`}>
                  {row.label}
                </Cell>
                <Cell className={`whitespace-nowrap ${textPrimaryOnSurface}`}>{row.role}</Cell>
                <Cell className={`whitespace-nowrap ${textSecondaryOnSurface}`}>
                  {row.type === "" ? NONE : row.type}
                </Cell>
                <Cell className={`font-mono break-words ${textSecondaryOnSurface}`}>
                  <Identifier value={row.agent} />
                </Cell>
                <Cell className={`font-mono break-words ${textSecondaryOnSurface}`}>
                  <Identifier value={row.model} />
                </Cell>
                <Cell className={`tabular-nums ${textSecondaryOnSurface}`}>{row.attempt}</Cell>
                <Cell className={textMutedOnSurface}>
                  <Time at={row.started} />
                </Cell>
                <Cell className={textMutedOnSurface}>
                  <Time at={row.ended} />
                </Cell>
                <Cell className={textPrimaryOnSurface}>
                  <Result row={row} />
                </Cell>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </section>
  );
}

/**
 * The steps Legion ran on the issue (`GET /issues/{key}/steps`): one table per task, in the order
 * of each task's first step, and one row per run of a step - its role, type, agent, model,
 * attempt, start, end and result. The records are the Legion daemon's; this tab only reads them.
 */
export function StepsTab({ issueKey }: { issueKey: string }): ReactNode {
  const query = useQuery(issueStepsQuery(issueKey));
  if (query.isPending) {
    return <p className={textMutedOnCanvas}>Loading steps…</p>;
  }
  if (query.isError) {
    return (
      <QueryError
        message={`Couldn't load the steps of ${issueKey}: ${query.error.message}`}
        onRetry={() => void query.refetch()}
        retrying={query.isFetching}
      />
    );
  }
  const tasks = stepTasks(query.data);
  if (tasks.length === 0) {
    return (
      <EmptyState label="Steps empty state" message="No step of this issue has run in Legion." />
    );
  }
  return (
    <div className="min-w-0 space-y-6">
      {tasks.map(({ rows, task }) => (
        <TaskTable key={task} rows={rows} task={task} />
      ))}
    </div>
  );
}
