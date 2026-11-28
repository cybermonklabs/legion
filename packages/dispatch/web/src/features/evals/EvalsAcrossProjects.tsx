import { useQueries, useQuery } from "@tanstack/react-query";
import { type ReactNode, useId, useMemo } from "react";
import { Link, useLocation } from "react-router-dom";

import { evalsAcrossProjectsQuery } from "../../api/queries";
import type { EvalListItem, EvalListResponse } from "../../api/types";
import { LoadingSkeleton } from "../../components/LoadingSkeleton";
import { QueryError } from "../../components/QueryError";
import {
  borderDefault,
  linkText,
  surfaceBg,
  textMutedOnCanvas,
  textMutedOnSurface,
  textPrimaryOnCanvas,
  textPrimaryOnSurface,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { compareRange, windowDays } from "../delivery/lib/window";
import { buildProjectPath } from "../refs/routes";
import { evalName, everyEvalModel } from "./chart-model";
import { PerDayLine } from "./chart-renderers";
import { EvalChart } from "./EvalChart";
import { EvalsHeader } from "./EvalsHeader";
import { useEvalsUrlState } from "./evals-url";
import { useUtcDayClock } from "./useEvalsScope";
import { laterOffsets } from "./useEvalsViews";

/** One number of the page: its eval's chart (`EvalChart`, each listed series a day at a time) and
 *  the eval's answer beside it, as the eval platform's front page lists each number with its
 *  answer. Never the eval's declared charts, which its project's tab draws. */
function ProjectEval({
  answer,
  item,
}: {
  answer: EvalListResponse;
  item: EvalListItem;
}): ReactNode {
  const model = useMemo(() => everyEvalModel(answer, item, Date.now()), [answer, item]);
  const name = evalName(item);
  const written = item.definition.answer;
  return (
    <article
      aria-label={name}
      className={`@container min-w-0 rounded border px-2.5 py-2 ${surfaceBg} ${borderDefault}`}
      data-eval={item.definition.slug}
    >
      <div className="flex min-w-0 flex-col gap-2 @min-[36rem]:flex-row @min-[36rem]:items-start">
        <div className="min-w-0 flex-1">
          {model === null ? (
            <>
              <span className={`text-xs ${textMutedOnSurface}`}>{name}</span>
              <p className={`text-sm ${textMutedOnSurface}`}>no data</p>
            </>
          ) : (
            <EvalChart model={model} renderer={PerDayLine} />
          )}
        </div>
        {written === "" ? (
          <p className={`text-sm @min-[36rem]:w-2/5 ${textMutedOnSurface}`}>
            No answer is written for this eval.
          </p>
        ) : (
          <p className={`text-sm @min-[36rem]:w-2/5 ${textPrimaryOnSurface}`} data-answer>
            {written}
          </p>
        )}
      </div>
    </article>
  );
}

/** One project's numbers, under its key, which links to the project's Evals tab on the same
 *  window, compare range and repositories (`search`). */
function ProjectEvals({
  answer,
  items,
  project,
  search,
}: {
  answer: EvalListResponse;
  items: readonly EvalListItem[];
  project: string;
  search: string;
}): ReactNode {
  const headingId = useId();
  return (
    <section aria-labelledby={headingId} className="flex flex-col gap-2" data-project={project}>
      <h2 className={`text-[15px] font-semibold ${textPrimaryOnCanvas}`} id={headingId}>
        <Link
          className={`hover:underline ${linkText}`}
          to={`${buildProjectPath({ kind: "evals", project })}${search}`}
        >
          {project}
        </Link>
      </h2>
      <div className="flex flex-col gap-2">
        {items.map((item) => (
          <ProjectEval answer={answer} item={item} key={item.id} />
        ))}
      </div>
    </section>
  );
}

/**
 * Every project's numbers on one page (`/evals`, dispatch://INFRA-2918/spec R11, plan D11), for a
 * reader with no project in mind: one request to `GET /api/v1/evals` naming no project, so every
 * project the reader may read is in it, in the item shape and envelope a project's tab reads.
 * The numbers are grouped by project in the order the read answers them, each project named and
 * linked to its own tab, under the tab's header (window, compare range and repositories, in the
 * URL), each number drawn with `EvalChart` and its eval's answer beside it. A read of more evals
 * than one page holds reads each further page once the first has said how many there are.
 */
export function EvalsAcrossProjects(): ReactNode {
  const [state, setState] = useEvalsUrlState();
  const now = useUtcDayClock();
  const { search } = useLocation();
  const { compare, repos, window: choice } = state;
  const read = useMemo(
    () => ({
      compare: compareRange(compare, choice, now),
      repos,
      window: windowDays(choice, now),
    }),
    [choice, compare, repos, now]
  );
  const first = useQuery(evalsAcrossProjectsQuery({ ...read, offset: 0 }));
  const rest = useQueries({
    queries: laterOffsets(first.data?.total ?? 0).map((offset) =>
      evalsAcrossProjectsQuery({ ...read, offset })
    ),
  });
  const answer = first.data;
  const refused = rest.filter((page) => page.isError);
  const byProject = new Map<string, EvalListItem[]>();
  for (const item of [first, ...rest].flatMap((page) => page.data?.items ?? [])) {
    const group = byProject.get(item.definition.project_key);
    if (group === undefined) byProject.set(item.definition.project_key, [item]);
    else group.push(item);
  }
  return (
    <div className="flex flex-col gap-6">
      <header className="flex flex-col gap-1">
        <h1 className={`text-[22px] font-semibold ${textPrimaryOnCanvas}`}>Evals</h1>
        <p className={`text-sm ${textSecondaryOnCanvas}`}>
          Every project's numbers, each with its eval's answer. A project's own Evals tab holds its
          sections and charts.
        </p>
      </header>
      <EvalsHeader
        compare={answer?.compare}
        onChange={setState}
        state={state}
        window={answer?.window}
      />
      {first.isError ? (
        <QueryError
          error={first.error}
          message="Could not load the evals."
          onRetry={() => void first.refetch()}
          retrying={first.isFetching}
        />
      ) : answer === undefined ? (
        <LoadingSkeleton label="Loading evals" />
      ) : (
        <>
          {refused.length === 0 ? null : (
            <QueryError
              error={refused[0]?.error}
              message="Could not load every project's evals."
              onRetry={() => {
                for (const page of refused) void page.refetch();
              }}
            />
          )}
          {byProject.size === 0 ? (
            <p className={`text-sm ${textMutedOnCanvas}`}>No project has an eval yet.</p>
          ) : (
            [...byProject].map(([project, items]) => (
              <ProjectEvals
                answer={answer}
                items={items}
                key={project}
                project={project}
                search={search}
              />
            ))
          )}
        </>
      )}
    </div>
  );
}
