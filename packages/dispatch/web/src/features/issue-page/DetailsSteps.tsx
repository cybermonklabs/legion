import { useQuery } from "@tanstack/react-query";
import { Fragment, type ReactNode, useLayoutEffect, useRef } from "react";

import { issueStepsQuery } from "../../api/queries";
import {
  pageInkText,
  pageLineBorder,
  pageMutedText,
  pagePaperBg,
  pageQuietText,
  pageStateText,
  pageStatusProgressText,
} from "../../theme/classes";
import { useCatchUp } from "../steps/StepsTab";
import { type StepRow, stepTasks } from "../steps/steps-model";
import { DetailsErrorLine } from "./details-error";
import { authorText, pageChrome } from "./page-structure";
import { ANSWER_LINES, CONTROL_LABELS } from "./page-words";

/** The label of each field of a run, and how a run ended, in the page's words. */
const FIELDS = CONTROL_LABELS.step_fields;
const RESULTS = CONTROL_LABELS.step_results;

/** A field the record does not hold - a merge GitHub made names no agent, a run that has not
 *  answered names no model, a run that has not ended no end - read as every row of Details reads
 *  a property with no value. */
function None(): ReactNode {
  return (
    <span className={pageQuietText} {...pageChrome}>
      {CONTROL_LABELS.none}
    </span>
  );
}

/** A run's start and end are read against each other, down the runs, so each is the instant
 *  itself (`Nov 16 8:00 AM`), as the Steps tab draws it, rather than a distance from now. The
 *  date and the time each stay whole, so a narrow column puts the time under the date and never
 *  splits either; the year shows when it is not this one, and the whole date and time is the
 *  title. */
const day = new Intl.DateTimeFormat(undefined, { day: "numeric", month: "short" });
const dayOfAnotherYear = new Intl.DateTimeFormat(undefined, {
  day: "numeric",
  month: "short",
  year: "numeric",
});
const timeOfDay = new Intl.DateTimeFormat(undefined, { hour: "numeric", minute: "2-digit" });

function Time({ at }: { at: string | null }): ReactNode {
  if (at === null) return <None />;
  const date = new Date(at);
  if (Number.isNaN(date.getTime())) return <time {...authorText}>{at}</time>;
  const dayFormat = date.getFullYear() === new Date().getFullYear() ? day : dayOfAnotherYear;
  return (
    <time dateTime={at} title={date.toLocaleString()} {...authorText}>
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

/** An agent token or a model id, the Steps tab's rule for one: a token breaks at its hyphens,
 *  and a model id (`anthropic/claude-opus-5`) is also offered a break after each slash, so
 *  neither runs past the width Details gives it; a number or a single letter keeps to the part
 *  before it, so no line holds it alone. */
function Identifier({ value }: { value: string }): ReactNode {
  if (value === "") return <None />;
  const pieces = value.split("/");
  return (
    <span className="font-mono" {...authorText}>
      {/* Each piece is keyed by the text up to and including it, which no other piece shares. */}
      {pieces.map((piece, index) => {
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
      })}
    </span>
  );
}

/** How the run ended, or that it runs, in the colour Details' other states are drawn in: a run
 *  that runs in the in-progress blue, one that passed in the passed green, one that failed in
 *  the failed red. A result this build has no word for shows the server's own. A state
 *  `StepEnd` adds fails to compile until it is drawn here. */
function Result({ row }: { row: StepRow }): ReactNode {
  switch (row.state) {
    case "running":
      return (
        <span className={`font-medium ${pageStatusProgressText}`} {...pageChrome}>
          {RESULTS.running}
        </span>
      );
    case "passed":
      return (
        <span className={`font-medium ${pageStateText.passed}`} {...pageChrome}>
          {RESULTS.passed}
        </span>
      );
    case "failed":
      return (
        <span className={`font-medium ${pageStateText.failed}`} {...pageChrome}>
          {RESULTS.failed}
        </span>
      );
    case "stopped":
      return (
        <span className="font-medium" {...pageChrome}>
          {RESULTS.stopped}
        </span>
      );
    case "other":
      return <span {...authorText}>{row.result}</span>;
  }
  return row satisfies never;
}

/** One labelled value of a run: its label in the quiet grey, as Details labels its rows, and the
 *  value in the ink beside it, given the width Details has left so it wraps rather than runs
 *  past it. `field` names it for a reader of the page's markup. */
function Field({
  children,
  field,
  label,
}: {
  children: ReactNode;
  field: string;
  label: string;
}): ReactNode {
  return (
    <div className="grid min-w-0 grid-cols-[88px_minmax(0,1fr)] gap-2" data-step-field={field}>
      <dt className={pageQuietText} {...pageChrome}>
        {label}
      </dt>
      <dd className={`min-w-0 break-words ${pageInkText}`}>{children}</dd>
    </div>
  );
}

/** One run of one step, as a list of labelled values rather than a row of a table, so each
 *  value has the width of Details to wrap in. A failed or stopped run that sent its task back
 *  names where it sent it (`StepEnd.returnedTo`). */
function Run({ row }: { row: StepRow }): ReactNode {
  return (
    <li
      className={`min-w-0 rounded-lg border px-3 py-2 ${pageLineBorder} ${pagePaperBg}`}
      data-step-run={row.id}
    >
      <dl className="flex min-w-0 flex-col gap-0.5 text-xs leading-[18px]">
        <Field field="step" label={FIELDS.step}>
          <span className="font-semibold" {...authorText}>
            {row.label}
          </span>
        </Field>
        <Field field="role" label={FIELDS.role}>
          {row.role === "" ? <None /> : <span {...authorText}>{row.role}</span>}
        </Field>
        <Field field="type" label={FIELDS.type}>
          {row.type === "" ? <None /> : <span {...authorText}>{row.type}</span>}
        </Field>
        <Field field="agent" label={FIELDS.agent}>
          <Identifier value={row.agent} />
        </Field>
        <Field field="model" label={FIELDS.model}>
          <Identifier value={row.model} />
        </Field>
        <Field field="attempt" label={FIELDS.attempt}>
          <span className="tabular-nums" {...authorText}>
            {row.attempt}
          </span>
        </Field>
        <Field field="started" label={FIELDS.started}>
          <Time at={row.started} />
        </Field>
        <Field field="ended" label={FIELDS.ended}>
          <Time at={row.ended} />
        </Field>
        <Field field="result" label={FIELDS.result}>
          <Result row={row} />
        </Field>
        {(row.state === "failed" || row.state === "stopped") && row.returnedTo !== null ? (
          <Field field="returned_to" label={FIELDS.returned_to}>
            <span className="whitespace-nowrap" {...authorText}>
              {row.returnedTo}
            </span>
          </Field>
        ) : null}
      </dl>
    </li>
  );
}

/**
 * The steps Legion ran on the issue (`GET /issues/{key}/steps`), under the history in Details,
 * where today's page has a Steps tab: headed by how many runs it lists, then one group per task
 * in the order of each task's first step (`stepTasks`), each run of the task in the order the
 * server read them, with its step, role, type, agent, model, attempt, start, end and result.
 *
 * It reads through the Steps tab's own catch-up (`useCatchUp` in `features/steps/StepsTab.tsx`)
 * while Details is open (`open`), and through its Retry, and in no other way, so a load that
 * leaves Details shut costs no request for it. A step record leaves the daemon on a lane of its
 * own, so the event that comes with it can reach the page before the record reaches the server:
 * the catch-up reads at once and then a few times more, and starts again at each refresh the
 * event feed sends the query (`changesSteps` in `api/event-refresh.ts`), which reaches it through
 * the catch-up's hold on the query (`ownQuery` in `api/query-refresh.ts`). The query itself is
 * never enabled - it would read on its own as Details opens, in the commit the catch-up reads in,
 * and the scheduler, which takes only a read begun after the one asked for as its answer, would
 * send the catch-up's as a second request - and has no retry policy: the catch-up's next read
 * asks again for a read that failed, where the app's retries would go on after Details shuts.
 * What a read answered stays drawn when a later read fails, with the failure and its Retry above
 * it; the failure is drawn alone only while no read has answered. Retry joins a read already out
 * (`cancelRefetch: false`) and, once a read lands and takes the line with it, hands the focus it
 * held to the heading rather than to the page.
 */
export function DetailsSteps({ issueKey, open }: { issueKey: string; open: boolean }): ReactNode {
  const query = useQuery({ ...issueStepsQuery(issueKey), enabled: false, retry: false });
  useCatchUp(issueKey, open);
  const heading = useRef<HTMLHeadingElement>(null);
  const retried = useRef<Element | null>(null);
  useLayoutEffect(() => {
    if (retried.current === null || retried.current.isConnected) return;
    retried.current = null;
    if (document.activeElement === null || document.activeElement === document.body) {
      heading.current?.focus({ preventScroll: true });
    }
  });
  // The line stays while the read its Retry sent is out, even where no read has answered: a
  // fetch of a read that holds no answer puts it back to pending as it starts, which would take
  // the line, and its Retry with the focus, away at the press.
  const failed = query.isError || (query.isPending && query.errorUpdateCount > 0);
  const tasks = query.data === undefined ? undefined : stepTasks(query.data);
  const runs = tasks?.reduce((count, task) => count + task.rows.length, 0);
  return (
    <section
      aria-label={CONTROL_LABELS.steps}
      className="flex min-w-0 flex-col gap-1.5"
      data-testid="issue-steps"
    >
      <h2
        className={`text-sm font-semibold outline-none ${pageInkText}`}
        ref={heading}
        tabIndex={-1}
        {...pageChrome}
      >
        {CONTROL_LABELS.steps}
        {runs === undefined ? null : (
          <>
            {" ("}
            <span {...authorText}>{runs}</span>
            {")"}
          </>
        )}
      </h2>
      {failed ? (
        <DetailsErrorLine
          error={query.error}
          failure={query.errorUpdateCount}
          message={ANSWER_LINES.steps_unreadable}
          onRetry={() => {
            retried.current = document.activeElement;
            void query.refetch({ cancelRefetch: false });
          }}
          retrying={query.isFetching}
        />
      ) : null}
      {tasks === undefined ? (
        failed ? null : (
          <p className={`text-xs ${pageQuietText}`} {...pageChrome}>
            {ANSWER_LINES.loading_steps}
          </p>
        )
      ) : runs === 0 ? (
        <p className="text-sm">
          <None />
        </p>
      ) : (
        tasks.map(({ rows, task }) => (
          <section
            aria-label={`${CONTROL_LABELS.steps_task} ${task}`}
            className="flex min-w-0 flex-col gap-1.5"
            data-step-task={task}
            key={task}
          >
            <h3 className={`text-xs leading-[18px] font-semibold ${pageMutedText}`} {...pageChrome}>
              {CONTROL_LABELS.steps_task} <span {...authorText}>{task}</span>
            </h3>
            <ol className="flex min-w-0 flex-col gap-1.5">
              {rows.map((row) => (
                <Run key={row.id} row={row} />
              ))}
            </ol>
          </section>
        ))
      )}
    </section>
  );
}
