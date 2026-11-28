import type { UseQueryResult } from "@tanstack/react-query";
import { type ReactNode, type RefObject, useMemo } from "react";

import type { HandoffSummary, IssueSteps } from "../../api/types";
import { pageInkText, pageLineBorder, pageMutedText, pageQuietText } from "../../theme/classes";
import { standsFailed } from "../refs/reference-target";
import { noRunLine, plannedStepsName, taskNeedsLine } from "../steps/StepsTab";
import { type PlannedStep, type StepTaskRows, stepTasks, stepVariant } from "../steps/steps-model";
import { DetailsErrorLine, retryRead, useRetryHandOff } from "./details-error";
import { StateWords } from "./history-entries";
import { authorText, pageChrome } from "./page-structure";
import { ANSWER_LINES, CONTROL_LABELS } from "./page-words";

/**
 * What the history says under its heading, above the list, of the runs and the handoffs it draws:
 * where a read of either failed, with a Retry; that the handoff a page address names does not
 * exist; the experiment of the newest run; and one group for each task of the runs read that the
 * plan says something of or that has no run (`TaskGroup`), in the server's order. The runs' and
 * the handoffs' reads are the history's own (`IssueHistory`), handed in.
 */
export function HistoryTasks({
  handoffId,
  handoffs,
  heading,
  runs,
}: {
  /** The handoff the page address names, where it names one: said to be none under the heading
   *  once the list of handoffs has answered without it. */
  handoffId: string | undefined;
  handoffs: UseQueryResult<readonly HandoffSummary[]>;
  /** The history's heading, which takes the focus a Retry held as its line goes. */
  heading: RefObject<HTMLElement | null>;
  runs: UseQueryResult<IssueSteps>;
}): ReactNode {
  // A failed read of the runs, or of the list of handoffs, is said under the heading while it
  // holds no answer (`standsFailed`), and every entry the other reads give stays drawn: the
  // runs' line is the one the Steps section drew, with its Retry, which reads them again, joins a
  // read already out (`retryRead`), and hands the focus it held to the heading as its line goes.
  const runsRetry = useRetryHandOff(() => heading.current);
  const handoffsRetry = useRetryHandOff(() => heading.current);
  // The experiment and the variant the newest run's records name, once, above the list: the
  // steps model's own line (`stepVariant`), as today's Steps tab draws it.
  const variant = runs.data === undefined ? null : stepVariant(runs.data);
  // The groups of the tasks of the runs read, under that line and above the list (`TaskGroup`):
  // what today's Steps tab draws of a task besides its runs, which are entries of the list. Each
  // task in the server's order (`stepTasks`), the tasks of the issue's plan first, with a line of
  // needs (`taskNeedsLine`), with steps the plan gives it, or none of whose steps has run has a
  // group; a task with none of the three - one the plan does not hold, which ran - has none: its
  // runs' entries are all the history draws of it.
  const groups = useMemo(
    () =>
      (runs.data === undefined ? [] : stepTasks(runs.data)).filter(
        (task) => task.rows.length === 0 || task.planned.length > 0 || taskNeedsLine(task) !== null
      ),
    [runs.data]
  );
  // A handoff's page address whose id the answered list does not hold says that no such handoff
  // exists, under the heading; while the list holds no answer, its own line says why instead.
  const handoffMissing =
    handoffId !== undefined &&
    handoffs.data !== undefined &&
    !handoffs.data.some((handoff) => handoff.id === handoffId);
  return (
    <>
      {standsFailed(runs) ? (
        <DetailsErrorLine
          error={runs.error}
          failure={runs.errorUpdateCount}
          message={ANSWER_LINES.steps_unreadable}
          onRetry={retryRead(runs)}
          {...runsRetry}
          retrying={runs.isFetching}
        />
      ) : null}
      {standsFailed(handoffs) ? (
        <DetailsErrorLine
          error={handoffs.error}
          failure={handoffs.errorUpdateCount}
          message={ANSWER_LINES.handoffs_unreadable}
          onRetry={retryRead(handoffs)}
          {...handoffsRetry}
          retrying={handoffs.isFetching}
        />
      ) : null}
      {handoffMissing ? (
        <p
          className={`text-xs ${pageMutedText}`}
          data-handoff-missing=""
          role="status"
          {...pageChrome}
        >
          {ANSWER_LINES.handoff_missing}
        </p>
      ) : null}
      {variant === null ? null : (
        <p
          className={`min-w-0 text-xs leading-[18px] break-words ${pageMutedText}`}
          data-step-variant=""
          {...authorText}
        >
          {variant}
        </p>
      )}
      {groups.map((task) => (
        <TaskGroup key={task.task} task={task} />
      ))}
    </>
  );
}

/**
 * One task's group under the History heading, above the list (decision 559 of INFRA-1196): its
 * heading; the tasks of the plan it needs and the steps the plan gives it, as today's Steps tab
 * says them (`taskNeedsLine`, `PlannedSteps`); and, for a task none of whose steps has run, the
 * tab's line saying so (`noRunLine`). Its runs are entries of the list, not of the group, so each
 * line names its task as the tab's do: the list and the line of no run in their own words, and
 * the line of needs under the group's heading. The tab's two lines hold the record's task names
 * inside the tab's own words, as the experiment line does, so each is text the page did not
 * author.
 */
function TaskGroup({ task }: { task: StepTaskRows }): ReactNode {
  const needs = taskNeedsLine(task);
  return (
    <section
      aria-label={`${CONTROL_LABELS.steps_task} ${task.task}`}
      className="flex min-w-0 flex-col gap-1.5"
      data-step-task={task.task}
    >
      <h3 className={`text-xs leading-[18px] font-semibold ${pageMutedText}`} {...pageChrome}>
        {CONTROL_LABELS.steps_task} <span {...authorText}>{task.task}</span>
      </h3>
      {needs === null ? null : (
        <p
          className={`min-w-0 text-xs leading-[18px] break-words ${pageMutedText}`}
          data-step-needs=""
          {...authorText}
        >
          {needs}
        </p>
      )}
      <PlannedSteps planned={task.planned} task={task.task} />
      {task.rows.length === 0 ? (
        <p
          className={`min-w-0 text-xs leading-[18px] break-words ${pageQuietText}`}
          data-step-no-run=""
          {...authorText}
        >
          {noRunLine(task.task)}
        </p>
      ) : null}
    </section>
  );
}

/** Where a step of the plan stands, as the Steps tab says it: `not started` before any run of it,
 *  in the quiet grey, then where its newest run stands, in the words and the colour of a run's
 *  result (`StateWords`). */
function PlannedState({ step }: { step: PlannedStep }): ReactNode {
  if (step.state === "not started") {
    return (
      <span className={pageQuietText} {...pageChrome}>
        {CONTROL_LABELS.step_not_started}
      </span>
    );
  }
  return <StateWords shown={step} />;
}

/** The steps the plan gives a task, in the plan's order, each its place, its name and where it
 *  stands (`PlannedState`), named as the Steps tab names the list (`plannedStepsName`), which
 *  names the task, since the list stands in the task's group above the entries and not beside its
 *  runs; nothing for a task the plan gives no step. A step's place and name are the plan's, so
 *  text the page did not author. */
function PlannedSteps({
  planned,
  task,
}: {
  planned: readonly PlannedStep[];
  task: string;
}): ReactNode {
  if (planned.length === 0) return null;
  return (
    <ol
      aria-label={plannedStepsName(task)}
      className="flex min-w-0 flex-wrap gap-1.5 text-xs leading-[18px]"
      data-step-planned=""
    >
      {planned.map((step) => (
        <li
          className={`min-w-0 rounded-md border px-2 py-0.5 break-words ${pageLineBorder}`}
          key={step.step}
        >
          <span className={`tabular-nums ${pageQuietText}`} {...authorText}>
            {step.step}
          </span>{" "}
          <span className={`font-medium ${pageInkText}`} {...authorText}>
            {step.name}
          </span>{" "}
          <PlannedState step={step} />
        </li>
      ))}
    </ol>
  );
}
