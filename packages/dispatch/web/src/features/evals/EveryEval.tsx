import { keepPreviousData, useQuery } from "@tanstack/react-query";
import { type ReactNode, useMemo } from "react";

import { type EvalKind, evalDetailQuery } from "../../api/queries";
import type { EvalListItem, EvalListResponse } from "../../api/types";
import { QueryError } from "../../components/QueryError";
import {
  borderDefault,
  surfaceBg,
  textMutedOnCanvas,
  textMutedOnSurface,
} from "../../theme/classes";
import { targetLabel } from "../delivery/lib/target";
import { CI_CHARTS } from "./CiHealth";
import { CODE_SURVIVAL } from "./CodeSurvivalCard";
import { evalName, everyEvalModel, targetOf } from "./chart-model";
import { PerDayLine } from "./chart-renderers";
import { DeclaredEval, declaredCharts } from "./DeclaredEval";
import { DEPLOY_CHARTS } from "./DeploysSection";
import { EvalChart } from "./EvalChart";
import { EvalTitle } from "./EvalPanel";
import { EvalRows } from "./EvalRows";
import { LEGION_CHARTS } from "./LegionSection";
import { POST_MERGE_REWORK } from "./PostMergeReworkCard";
import { TARGET_CARDS } from "./TargetCards";
import type { EvalsScope, EvalsViewState } from "./useEvalsViews";

/** How many of a logged eval's rows its box lists a page. */
const LOGGED_ROWS = 10;

/** Whether an eval's values are readings an agent or a job logs, rather than numbers Dispatch
 *  computes (its source is `logged`, the one source a register write gives). */
export function isLogged(item: EvalListItem): boolean {
  return item.definition.source === "logged";
}

/** The evals each kind's section draws by name: its own charts, and the target cards of its kind;
 *  Deploys and lead time's, the two code cards too. No section draws an experiment by name; the
 *  Logged section draws every one. */
const NAMED: Readonly<Record<EvalKind, ReadonlySet<string>>> = {
  ci: new Set(CI_CHARTS.map((chart) => chart.slug)),
  delivery: new Set([
    ...[...TARGET_CARDS.filter((card) => card.kind === "delivery"), ...DEPLOY_CHARTS].map(
      (chart) => chart.slug
    ),
    CODE_SURVIVAL,
    POST_MERGE_REWORK,
  ]),
  experiment: new Set(),
  legion: new Set(
    [...TARGET_CARDS.filter((card) => card.kind === "legion"), ...LEGION_CHARTS].map(
      (chart) => chart.slug
    )
  ),
};

/** What the Logged section keeps of each page of a view (plan D7): every logged eval, whatever
 *  its kind (a register write may give one kind `ci`, `legion` or `delivery`), and every eval of
 *  kind `experiment`, which no other section draws. */
export function keepLogged(answer: EvalListResponse): EvalListResponse {
  return {
    ...answer,
    items: (answer.items ?? []).filter(
      (item) => isLogged(item) || item.definition.kind === "experiment"
    ),
  };
}

/**
 * One eval no section draws by name, in a box naming it (`data-eval`): its per-day line, each
 * listed series a line of its leading statistic, under EvalChart's Graph | Table and Compare; its
 * unit and its target; and, where `rows`, the rows behind it (the items view). An eval whose
 * detail declares charts draws them in place of the line (`DeclaredEval`, plan D10): the detail is
 * read at the version the evals view answers, so a register write that declares charts reads it
 * again (D11a), the box drawing what the version before declared until the new detail lands.
 */
function EvalBox({
  answer,
  item,
  rows,
  scope,
}: {
  answer: EvalListResponse;
  item: EvalListItem;
  rows: boolean;
  scope: EvalsScope;
}): ReactNode {
  const { slug, unit, version } = item.definition;
  const detail = useQuery({
    ...evalDetailQuery(scope.project, slug, version),
    placeholderData: keepPreviousData,
  });
  const model = useMemo(() => everyEvalModel(answer, item, Date.now()), [answer, item]);
  const target = useMemo(() => targetOf(item.verdict), [item.verdict]);
  const name = evalName(item);
  return (
    <article
      aria-label={name}
      className={`flex min-w-0 flex-col gap-1 rounded border px-2.5 py-2 ${surfaceBg} ${borderDefault}`}
      data-eval={slug}
    >
      {declaredCharts(detail.data).length > 0 ? (
        <DeclaredEval answer={answer} item={item} scope={scope} />
      ) : model === null ? (
        <>
          <span className={`text-xs ${textMutedOnSurface}`}>
            <EvalTitle slug={slug} title={name} />
          </span>
          <p className={`text-sm ${textMutedOnSurface}`}>no data</p>
        </>
      ) : (
        <EvalChart model={model} opensPanel={slug} renderer={PerDayLine} />
      )}
      {detail.isError ? (
        <QueryError
          error={detail.error}
          message="Could not read whether this eval declares its own charts."
          onRetry={() => void detail.refetch()}
        />
      ) : null}
      <div className={`text-[11px] ${textMutedOnSurface}`} data-eval-facts>
        {`${unit === "" ? "" : `in ${unit} \u00b7 `}${target === null ? "no target" : `target ${targetLabel(target)}`}`}
      </div>
      {rows ? (
        <EvalRows label={`Rows of ${name}`} limit={LOGGED_ROWS} scope={scope} slug={slug} />
      ) : null}
    </article>
  );
}

/** Evals each drawn in a box of its own, side by side where the section is wide enough. */
function EvalBoxes({
  drawn,
  rows,
  scope,
}: {
  drawn: readonly { answer: EvalListResponse; item: EvalListItem }[];
  rows: boolean;
  scope: EvalsScope;
}): ReactNode {
  return (
    <div className="@container">
      <div className="grid grid-cols-1 gap-2 @min-[42rem]:grid-cols-2">
        {drawn.map(({ answer, item }) => (
          <EvalBox answer={answer} item={item} key={item.id} rows={rows} scope={scope} />
        ))}
      </div>
    </div>
  );
}

/**
 * The computed evals of `kind`'s view that no section draws by name, such as one a later
 * catalogue adds (plan D7, spec R19), drawn at the end of that kind's section, each as the Logged
 * section draws an eval but for its rows, which its panel holds.
 */
export function UnnamedEvals({
  kind,
  scope,
  views,
}: {
  kind: EvalKind;
  scope: EvalsScope;
  views: readonly EvalsViewState[];
}): ReactNode {
  const view = views.find((candidate) => candidate.kind === kind);
  const answer = view?.answer;
  if (view === undefined || answer === undefined) return null;
  const drawn = view.items.flatMap((item) =>
    isLogged(item) || NAMED[kind].has(item.definition.slug) ? [] : [{ answer, item }]
  );
  return drawn.length === 0 ? null : <EvalBoxes drawn={drawn} rows={false} scope={scope} />;
}

/**
 * The Logged section, last on the tab, after the merge timeline (plan D7; spec R2, R19): every
 * eval its views keep (`keepLogged`), whatever its kind, in the order the list read answers them,
 * by slug, each with its per-day line, its table and its rows, so an eval an agent or a job logs
 * shows with no code of its own. Each view's evals draw as that view lands. "no data" once every
 * view has answered and none holds such an eval.
 */
export function LoggedEvals({
  scope,
  views,
}: {
  scope: EvalsScope;
  views: readonly EvalsViewState[];
}): ReactNode {
  const drawn = views
    .flatMap(({ answer, items }) =>
      answer === undefined ? [] : items.map((item) => ({ answer, item }))
    )
    // The list read orders a project's evals by slug, byte by byte (collate "C").
    .sort((a, b) =>
      a.item.definition.slug < b.item.definition.slug
        ? -1
        : a.item.definition.slug > b.item.definition.slug
          ? 1
          : 0
    );
  if (drawn.length > 0) return <EvalBoxes drawn={drawn} rows scope={scope} />;
  return views.every((view) => view.status === "painted") ? (
    <p className={`text-sm ${textMutedOnCanvas}`}>no data</p>
  ) : null;
}
