import { focusManager, useQuery, useQueryClient } from "@tanstack/react-query";
import { Fragment, type ReactNode, useEffect, useSyncExternalStore } from "react";

import { issueStepsQuery } from "../../api/queries";
import { ownQuery } from "../../api/query-refresh";
import { EmptyState } from "../../components/EmptyState";
import { QueryError } from "../../components/QueryError";
import { useScrollFadeTrailing } from "../../hooks/useScrollFadeTrailing";
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
import {
  type PlannedStep,
  type ShownState,
  type StepRow,
  type StepTaskRows,
  stepTasks,
  stepVariant,
} from "./steps-model";

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

/** The words and colours of a state a run's result and a planned step share (`ShownState`, built
 *  from the contract's words in steps-model): `running` as a badge, `passed` and `failed` in their
 *  colours, `stopped` in weight alone, and a result this build has no word for in the server's own
 *  word. A state the contract adds fails to compile here until this draws it. */
function StateLabel({ shown }: { shown: ShownState }): ReactNode {
  switch (shown.state) {
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
    case "failed":
      return <span className={`font-medium ${dangerText}`}>failed</span>;
    case "stopped":
      return <span className="font-medium">stopped</span>;
    case "other":
      return shown.result;
  }
  return shown satisfies never;
}

/** How the run ended, or that it runs (StateLabel): a failed or stopped run that sent its task back
 *  also names where it sent it (`StepEnd.returnedTo`). `step 2` stays whole, as the times do, so a
 *  narrow column never leaves the number on a line alone. A state `StepEnd` adds fails to compile
 *  until it is drawn here. */
function Result({ row }: { row: StepRow }): ReactNode {
  switch (row.state) {
    case "running":
    case "passed":
    case "other":
      return <StateLabel shown={row} />;
    case "failed":
    case "stopped":
      return (
        <>
          <StateLabel shown={row} />
          {row.returnedTo === null ? null : (
            <span className={textMutedOnSurface}>
              , returned to <span className="whitespace-nowrap">{row.returnedTo}</span>
            </span>
          )}
        </>
      );
  }
  return row satisfies never;
}

/** Where a step of the plan stands: `not started` before any run of it, then its newest run's
 *  state in the words and colours a run's result uses (StateLabel). */
function PlannedState({ step }: { step: PlannedStep }): ReactNode {
  if (step.state === "not started") {
    return <span className={textMutedOnSurface}>not started</span>;
  }
  return <StateLabel shown={step} />;
}

/** One task: the tasks of the plan it needs - nothing for a task that needs none or that the plan
 *  does not hold, and `Needs not recorded` where its plan node recorded none - each step the plan
 *  gives it with where it stands, then its runs, one row each, in the order the server read them.
 *  A task none of whose steps has run says so in place of the table. */
function TaskSection({ task: { needs, planned, rows, task } }: { task: StepTaskRows }): ReactNode {
  const needsLine =
    needs === null
      ? planned.length === 0
        ? null
        : "Needs not recorded"
      : needs.length === 0
        ? null
        : `Needs ${needs.join(", ")}`;
  return (
    <section aria-label={`Task ${task}`} className="min-w-0 space-y-2">
      <h2 className={`text-sm font-semibold ${textSecondaryOnCanvas}`}>Task {task}</h2>
      {needsLine === null ? null : (
        <p className={`text-xs ${textSecondaryOnCanvas}`}>{needsLine}</p>
      )}
      {planned.length === 0 ? null : (
        <ol aria-label={`Planned steps of task ${task}`} className="flex flex-wrap gap-2 text-xs">
          {planned.map((step) => (
            <li className={`rounded-md border px-2 py-1 ${card}`} key={step.step}>
              <span className={`tabular-nums ${textMutedOnSurface}`}>{step.step}</span>{" "}
              <span className={`font-medium ${textPrimaryOnSurface}`}>{step.name}</span>{" "}
              <PlannedState step={step} />
            </li>
          ))}
        </ol>
      )}
      {rows.length === 0 ? (
        <p className={`text-xs ${textMutedOnCanvas}`}>{`No step of task ${task} has run yet.`}</p>
      ) : (
        <RunsTable rows={rows} task={task} />
      )}
    </section>
  );
}

/** One task's runs, one row each, in the order the server read them. */
function RunsTable({ rows, task }: { rows: readonly StepRow[]; task: string }): ReactNode {
  const tableFade = useScrollFadeTrailing();
  return (
    // The table scrolls sideways inside the card where the page is narrower than its columns (a
    // phone): a data table is the layout rule's exception. While columns lie past the card's edge
    // it wears a trailing fade, so the cut column reads as more to scroll rather than as clipped
    // text. The scroll box sits inside the card so the fade masks the table and not the card's
    // border; `overflow-hidden` keeps the scrolled rows inside the card's rounded corners.
    <div className={`overflow-hidden rounded-xl border ${card}`}>
      <div className={`overflow-x-auto ${tableFade.fade}`} ref={tableFade.ref}>
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
    </div>
  );
}

/** When the shown tab reads again after a catch-up starts, in ms from the start; it also reads at
 *  once. A step record leaves the daemon on an outbox lane of its own, which neither waits for
 *  the issue's events nor sends one, so the event that starts a catch-up can reach the page before
 *  its record. A write that lane fails waits at most 64 s before it is tried again, so the list
 *  covers a record that needed one or two retries. A record later than the last read shows at the
 *  next start. */
const CATCH_UP_READS_MS = [2_000, 5_000, 15_000, 45_000, 120_000, 300_000] as const;

/**
 * Reads the issue's records at once and at each of `CATCH_UP_READS_MS` after every start: the tab
 * shown, the page shown again, and each refresh that reaches this read - an event of the issue
 * that comes with a phase moving (`changesSteps` in `api/event-refresh.ts`), a refresh of every
 * `["issue"]` read, which the feed asks where an event may have moved any issue (a parent,
 * components or repository move, an event it cannot read), or a refresh of the whole cache. A
 * start replaces the catch-up running, so a tab holds one list of timers at most; a tab not
 * shown, or a hidden page, reads nothing and holds no timer. Each read is asked of the event
 * refresh's scheduler, which holds the read while the tab is shown (`ownQuery` in
 * `api/query-refresh.ts`): a read never cancels one already out, and the tab reads at most once a
 * second, however its starts and the feed's batches fall.
 */
function useCatchUp(issueKey: string, shown: boolean): void {
  const queryClient = useQueryClient();
  // Whether the page is shown, as TanStack reads `visibilitychange` for its own refetch on focus.
  const visible = useSyncExternalStore(focusManager.subscribe, () => focusManager.isFocused());
  useEffect(() => {
    if (!shown || !visible) {
      return;
    }
    let timers: number[] = [];
    const stop = () => {
      for (const timer of timers) {
        window.clearTimeout(timer);
      }
    };
    const start = () => {
      stop();
      owner.read();
      timers = CATCH_UP_READS_MS.map((ms) => window.setTimeout(owner.read, ms));
    };
    const owner = ownQuery(queryClient, issueStepsQuery(issueKey).queryKey, start);
    start();
    return () => {
      owner.stop();
      stop();
    };
  }, [issueKey, queryClient, shown, visible]);
}

/**
 * The steps of the issue (`GET /issues/{key}/steps`): one section per task, in the server's order
 * - the tasks of the issue's plan first, then each other task that ran - each with the tasks it
 * needs, each step the plan gives it and where that step stands, and one row per run of a step:
 * its role, type, agent, model, attempt, start, end and result. Where the records of the issue's
 * newest run name an experiment, the tab shows `Experiment <experiment>: variant <variant>` once
 * above its tasks. The records are the Legion daemon's; this tab only reads them, and only
 * through its catch-up (`useCatchUp`) and Retry, so the query fetches nothing on its own: it is
 * never enabled, and a read that failed is asked again by the catch-up's next read, not by the
 * app's retry policy, whose retries no cleanup of the tab could stop. A failed read shows alone
 * only while the tab has drawn nothing; after a read that answered, the failure and its Retry
 * stand above the rows that read drew, since nothing may read again until the next start or
 * Retry.
 */
export function StepsTab({ active, issueKey }: { active: boolean; issueKey: string }): ReactNode {
  const query = useQuery({ ...issueStepsQuery(issueKey), enabled: false, retry: false });
  useCatchUp(issueKey, active);
  const failure = query.isError ? (
    <QueryError
      message={`Couldn't load the steps of ${issueKey}: ${query.error.message}`}
      onRetry={() => void query.refetch()}
      retrying={query.isFetching}
    />
  ) : null;
  if (query.data === undefined) {
    return failure ?? <p className={textMutedOnCanvas}>Loading steps…</p>;
  }
  const tasks = stepTasks(query.data);
  // The experiment a draw put the issue in, with the variant it drew, once above the tasks.
  const variant = stepVariant(query.data);
  return (
    <div className="min-w-0 space-y-6">
      {failure}
      {variant === null ? null : <p className={`text-sm ${textSecondaryOnCanvas}`}>{variant}</p>}
      {tasks.length === 0 ? (
        <EmptyState label="Steps empty state" message="No step of this issue has run in Legion." />
      ) : (
        tasks.map((task) => <TaskSection key={task.task} task={task} />)
      )}
    </div>
  );
}
