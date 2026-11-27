// The prototype's web/src/components/DoraPanel.tsx, ported as it is: the six KPI cards, the
// headline strip, and the "Definitions & alternatives" fold with its eight definition cards and
// per-day table. Every number, every target and each card's met/missed badge come from
// GET /api/v1/delivery/measures; nothing is computed here but the tone of a figure, a count, a
// day's bar or a per-day cell, each by its own value against the target of the eval it belongs to
// and only where that target judges the statistic it shows (`lib/target.ts`'s `ownVerdict`,
// dispatch://INFRA-2916, R5), and the digits a toned figure is written with, which print it on its
// value's side of the target (`judgeFigure`). Each such element names the eval, the statistic and
// whether its value is one day's or the window's (`data-eval`, `data-statistic`, `data-span`), and
// each card's caption names the statistics its target judges, so it reads as the badge beside it
// is judged. Three differences, each from LEGION-567's slice 2 plan: the change-failure-rate card
// says flags arrive with flag confirmation while the server stores none; the P0 card links each
// issue inside Dispatch and dates its count by the response's computed_at; and the cards and the
// strip lay themselves out for the panel's own width, since Dispatch's sidebar leaves it narrower
// than the prototype's page and a phone narrower still, where the prototype is desktop-only.
import { type ReactNode, useMemo, useState } from "react";
import { Link } from "react-router-dom";

import type { DeliveryMeasuresResponse, DeliveryTargets } from "../../api/types";
import { DisclosureToggle } from "../../components/DisclosureToggle";
import {
  borderDefault,
  linkText,
  surfaceBg,
  surfaceMutedBg,
  textMutedOnCanvas,
  textMutedOnSurface,
  textMutedOnSurfaceMuted,
  textPrimaryOnSurface,
  textPrimaryOnSurfaceMuted,
} from "../../theme/classes";
import { buildIssuePath } from "../refs/routes";
import { JudgedFigure, tone } from "./JudgedFigure";
import { formatMinutes, NO_VALUE, pct } from "./lib/format";
import {
  count,
  EVAL_SLUGS,
  type Judge,
  judgeFigure,
  type Measure,
  targetLabel,
  type WriteFigure,
} from "./lib/target";
import { Sparkbars } from "./Sparkbars";
import { TargetBadge } from "./TargetBadge";

const UNOWNED_P0_SHOWN = 6;

/** What a pending read shows in each number's place. */
const PENDING = "\u2026";

/** The change-failure-rate card's breakdown: deploys with a confirmed break, and how many more
 *  have only unreviewed (pending) flags. `confirmedOrPending` counts each deploy once (the
 *  prototype's web/src/lib/dora.ts:297-305). The confirmed count is the eval's
 *  per_deploy.confirmed, toned by its own verdict where the target judges it; the count of deploys
 *  with only unreviewed flags is confirmedOrPending less it, which no judge names
 *  (per_deploy.pending counts every deploy with a pending flag), so it takes no tone. */
function FailureRateBreakdown(props: {
  confirmed: number;
  confirmedOrPending: number;
  targets: DeliveryTargets;
}): ReactNode {
  const onlyPending = props.confirmedOrPending - props.confirmed;
  return (
    <>
      <WindowFigure
        measure="change_failure_rate"
        statistic="per_deploy.confirmed"
        targets={props.targets}
        value={props.confirmed}
        write={count(props.confirmed)}
      />
      {` ${props.confirmed === 1 ? "deploy" : "deploys"} with a confirmed break; `}
      {onlyPending === 0 ? "none" : `${onlyPending} more`}
      {" with only unreviewed flags"}
    </>
  );
}

type FailureRate = DeliveryMeasuresResponse["measures"]["change_failure_rate"];

/** Whether a failure source measured the change failure rate. Every rate is null while no failure
 *  source exists (flags_source `none`): a rate nothing measured is no 0, so a card reads no value
 *  from it. */
function measured(f: FailureRate): f is FailureRate & {
  per_pr: { rate: number };
  per_deploy: { rate: number; upper_bound_rate: number };
} {
  return (
    f.per_pr.rate !== null && f.per_deploy.rate !== null && f.per_deploy.upper_bound_rate !== null
  );
}

/** A figure of the whole window that the target of `measure`'s eval may judge (`JudgedFigure`),
 *  toned by its own value and written on its side of the target. It reads `plain` where that
 *  target judges another statistic or is off, so a card judged by its p90 does not colour its
 *  median and change-fail-rate judged by its per-PR rate colours neither its per-deploy rate nor
 *  its upper bound; and `pending`, muted unless given, while the read is pending (`targets`
 *  undefined), when no target judges it. */
function WindowFigure(props: {
  measure: Measure;
  statistic: Judge;
  targets: DeliveryTargets | undefined;
  value: number | null | undefined;
  write: WriteFigure;
  className?: string;
  plain?: string;
  pending?: string;
}): ReactNode {
  const { targets } = props;
  return (
    <JudgedFigure
      className={props.className}
      evalSlug={EVAL_SLUGS[props.measure]}
      plain={targets === undefined ? (props.pending ?? textMutedOnSurface) : props.plain}
      span="window"
      statistic={props.statistic}
      target={targets?.[props.measure] ?? null}
      value={props.value ?? null}
      write={props.write}
    />
  );
}

function KpiCard(props: {
  title: string;
  target: string;
  // Undefined while the read is pending: no badge rather than a verdict nobody has reached.
  met: boolean | null | undefined;
  children: ReactNode;
}): ReactNode {
  return (
    <div
      className={`flex min-w-0 flex-col gap-1 rounded border px-2.5 py-2 ${surfaceBg} ${borderDefault}`}
      data-kpi={props.title}
    >
      <div className="flex items-start justify-between gap-2">
        {/* Two lines at most: a narrow card (Dispatch's sidebar leaves the grid narrower than the
            prototype's page) shows its whole title rather than cutting it to a word. */}
        <span className={`line-clamp-2 min-w-0 text-xs ${textMutedOnSurface}`}>{props.title}</span>
        {props.met === undefined ? null : (
          <TargetBadge met={props.met} metLabel={"\u2713 met"} missedLabel={"\u2717 missed"} />
        )}
      </div>
      {props.children}
      <div className={`text-[11px] ${textMutedOnSurface}`}>target {props.target}</div>
    </div>
  );
}

/** A headline figure (`WindowFigure`) of a card or of the strip, plain where it takes no tone. */
function BigNumber(props: {
  measure: Measure;
  statistic: Judge;
  targets: DeliveryTargets | undefined;
  value: number | null | undefined;
  write: WriteFigure;
  pending?: string;
}): ReactNode {
  return <WindowFigure className="text-lg font-semibold" plain={textPrimaryOnSurface} {...props} />;
}

function DefinitionCard(props: {
  title: string;
  value: ReactNode;
  children: ReactNode;
}): ReactNode {
  return (
    <div className={`rounded p-3 ${surfaceMutedBg}`}>
      <div className={`text-xs uppercase ${textMutedOnSurfaceMuted}`}>{props.title}</div>
      <div className={`text-xl font-semibold ${textPrimaryOnSurfaceMuted}`}>{props.value}</div>
      <div className={`text-xs ${textMutedOnSurfaceMuted}`}>{props.children}</div>
    </div>
  );
}

/** One figure of the headline strip; `judged` names the statistic of a delivery eval it shows,
 *  where it shows one. */
interface StripStat {
  label: string;
  write: WriteFigure;
  judged?: { measure: Measure; statistic: Judge; value: number | null | undefined };
}

/** The delivery measures for the page's window and facets: Sami's six KPI targets, then one
 *  compact strip of the DORA headline numbers; every alternative definition and breakdown lives
 *  behind the "Definitions & alternatives" fold, so the timeline keeps the bulk of the page.
 *  `data` is undefined while the read is pending, when the cards show their frame. */
export function MeasuresPanel({ data }: { data: DeliveryMeasuresResponse | undefined }): ReactNode {
  const [open, setOpen] = useState(false);
  // A figure of the answer, and `PENDING` while the read is pending.
  const show =
    (format: (response: DeliveryMeasuresResponse, extra: number) => string): WriteFigure =>
    (extra) =>
      data === undefined ? PENDING : format(data, extra);
  const targets = data?.targets;
  const status = data?.status;
  const measures = data?.measures;
  const failures = measures?.change_failure_rate;

  const strip: StripStat[] = [
    {
      label: "Deploy freq",
      write: show((d, extra) => `${d.measures.deploy_frequency.per_day.toFixed(2 + extra)}/day`),
      judged: {
        measure: "deploys_per_day",
        statistic: "deploys.per_day",
        value: measures?.deploy_frequency.per_day,
      },
    },
    {
      label: "Lead time (merge\u2192prod)",
      write: show((d, extra) =>
        formatMinutes(d.measures.lead_time.merge_to_production.median_minutes, extra)
      ),
      judged: {
        measure: "merge_to_production_minutes",
        statistic: "minutes.median",
        value: measures?.lead_time.merge_to_production.median_minutes,
      },
    },
    {
      label: "CFR (per PR)",
      write: show((d, extra) =>
        !measured(d.measures.change_failure_rate)
          ? NO_VALUE
          : pct(d.measures.change_failure_rate.per_pr.rate, 1 + extra)
      ),
      judged: {
        measure: "change_failure_rate",
        statistic: "per_pr.rate",
        value: measures?.change_failure_rate.per_pr.rate,
      },
    },
    {
      label: "Time to restore",
      write: show((d) => formatMinutes(d.measures.time_to_restore.median_minutes)),
    },
    { label: "Rework share", write: show((d) => pct(d.measures.rework_share)) },
  ];

  return (
    <section className={`@container rounded border ${borderDefault}`}>
      {/* Columns follow the panel's own width, not the viewport's, since the page's sidebar takes a
          share of the viewport that varies with the breakpoint. A card needs 212px for its badge
          and a title of at most two lines ("Deploy runs reaching production" at text-xs), so six
          columns need 83.5rem, three 42.25rem and two 28.5rem, padding and gaps included. */}
      <fieldset
        aria-label="KPI targets"
        className="grid min-w-0 grid-cols-1 gap-2 px-3 pt-2 text-sm @min-[28.5rem]:grid-cols-2 @min-[42.25rem]:grid-cols-3 @min-[83.5rem]:grid-cols-6"
      >
        <KpiCard
          met={status?.deploys_per_day}
          target={targets === undefined ? PENDING : targetLabel(targets.deploys_per_day)}
          title="Deploys a day"
        >
          <div className="flex items-baseline gap-2">
            <BigNumber
              measure="deploys_per_day"
              statistic="deploys.per_day"
              targets={targets}
              value={measures?.deploy_frequency.per_day}
              write={show(
                (d, extra) => `${d.measures.deploy_frequency.per_day.toFixed(1 + extra)}/day`
              )}
            />
            {measures === undefined ? null : (
              <span className={`text-xs ${textMutedOnSurface}`}>
                <WindowFigure
                  measure="deploys_per_day"
                  statistic="deploys.deploys"
                  targets={targets}
                  value={measures.deploy_frequency.successful_deploys}
                  write={count(measures.deploy_frequency.successful_deploys)}
                />{" "}
                deploys
              </span>
            )}
          </div>
          {measures === undefined || targets === undefined ? null : (
            <Sparkbars
              judged={{
                evalSlug: EVAL_SLUGS.deploys_per_day,
                statistic: "deploys.per_day",
                target: targets.deploys_per_day,
              }}
              points={(measures.daily ?? []).map((d) => ({
                label: d.day,
                value: d.deploys,
                partial: d.partial,
              }))}
              write={(v) => () => `${v} deploys`}
            />
          )}
        </KpiCard>

        <KpiCard
          met={status?.change_failure_rate}
          target={targets === undefined ? PENDING : targetLabel(targets.change_failure_rate)}
          title="Change failure rate"
        >
          <div className="flex items-baseline gap-2">
            {/* Two decimals: 4.96% meets the target, 5.00% does not. */}
            <BigNumber
              measure="change_failure_rate"
              statistic="per_deploy.rate"
              targets={targets}
              value={
                measures === undefined || !measured(measures.change_failure_rate)
                  ? null
                  : measures.change_failure_rate.per_deploy.rate
              }
              write={show((d, extra) =>
                !measured(d.measures.change_failure_rate)
                  ? NO_VALUE
                  : pct(d.measures.change_failure_rate.per_deploy.rate, 2 + extra)
              )}
            />
            {data !== undefined && !measured(data.measures.change_failure_rate) ? null : (
              <span className={`text-xs ${textMutedOnSurface}`}>confirmed</span>
            )}
          </div>
          {failures === undefined || targets === undefined ? null : !measured(failures) ? (
            <div className={`text-xs ${textMutedOnCanvas}`}>
              Flags arrive with flag confirmation
            </div>
          ) : (
            <>
              <div className={`text-xs ${textMutedOnSurface}`}>
                up to{" "}
                <WindowFigure
                  className="font-semibold"
                  measure="change_failure_rate"
                  plain={textPrimaryOnSurface}
                  statistic="upper_bound.rate"
                  targets={targets}
                  value={failures.per_deploy.upper_bound_rate}
                  write={(extra) => pct(failures.per_deploy.upper_bound_rate, 2 + extra)}
                />{" "}
                if every pending flag is confirmed
              </div>
              <div className={`text-[11px] ${textMutedOnSurface}`}>
                <FailureRateBreakdown
                  confirmed={failures.per_deploy.confirmed}
                  confirmedOrPending={failures.per_deploy.confirmed_or_pending}
                  targets={targets}
                />
              </div>
            </>
          )}
        </KpiCard>

        <KpiCard
          met={status?.merge_to_production}
          target={
            targets === undefined ? PENDING : targetLabel(targets.merge_to_production_minutes)
          }
          title={"Merge \u2192 production"}
        >
          <div className="flex flex-wrap items-baseline gap-x-3">
            <span className="flex items-baseline gap-1">
              <BigNumber
                measure="merge_to_production_minutes"
                statistic="minutes.median"
                targets={targets}
                value={measures?.lead_time.merge_to_production.median_minutes}
                write={show((d, extra) =>
                  formatMinutes(d.measures.lead_time.merge_to_production.median_minutes, extra)
                )}
              />
              <span className={`text-xs ${textMutedOnSurface}`}>median</span>
            </span>
            <span className="flex items-baseline gap-1">
              <BigNumber
                measure="merge_to_production_minutes"
                statistic="minutes.max"
                targets={targets}
                value={measures?.lead_time.merge_to_production.max_minutes}
                write={show((d, extra) =>
                  formatMinutes(d.measures.lead_time.merge_to_production.max_minutes, extra)
                )}
              />
              <span className={`text-xs ${textMutedOnSurface}`}>max</span>
            </span>
          </div>
          <div className={`text-[11px] ${textMutedOnSurface}`}>
            PRs merged in the window and deployed
          </div>
        </KpiCard>

        <KpiCard
          met={status?.opened_to_merge}
          target={
            targets === undefined ? PENDING : targetLabel(targets.opened_to_merge_median_minutes)
          }
          title={"PR open \u2192 merge"}
        >
          <div className="flex flex-wrap items-baseline gap-x-3">
            <span className="flex items-baseline gap-1">
              <BigNumber
                measure="opened_to_merge_median_minutes"
                statistic="minutes.median"
                targets={targets}
                value={measures?.lead_time.opened_to_merge.median_minutes}
                write={show((d, extra) =>
                  formatMinutes(d.measures.lead_time.opened_to_merge.median_minutes, extra)
                )}
              />
              <span className={`text-xs ${textMutedOnSurface}`}>median</span>
            </span>
            <span className="flex items-baseline gap-1">
              <BigNumber
                measure="opened_to_merge_median_minutes"
                statistic="minutes.p90"
                targets={targets}
                value={measures?.lead_time.opened_to_merge.p90_minutes}
                write={show((d, extra) =>
                  formatMinutes(d.measures.lead_time.opened_to_merge.p90_minutes, extra)
                )}
              />
              <span className={`text-xs ${textMutedOnSurface}`}>p90</span>
            </span>
          </div>
          <div className={`text-[11px] ${textMutedOnSurface}`}>every PR merged in the window</div>
        </KpiCard>

        <KpiCard
          met={status?.deploy_run_success}
          target={targets === undefined ? PENDING : targetLabel(targets.deploy_run_success_rate)}
          title="Deploy runs reaching production"
        >
          <div className="flex items-baseline gap-2">
            <BigNumber
              measure="deploy_run_success_rate"
              statistic="runs.rate"
              targets={targets}
              value={measures?.deploy_run_success.rate}
              write={show((d, extra) =>
                d.measures.deploy_run_success.rate === null
                  ? NO_VALUE
                  : pct(d.measures.deploy_run_success.rate, 1 + extra)
              )}
            />
            {measures === undefined ? null : (
              <span className={`text-xs ${textMutedOnSurface}`}>
                <WindowFigure
                  measure="deploy_run_success_rate"
                  statistic="runs.reached_production"
                  targets={targets}
                  value={measures.deploy_run_success.reached_production}
                  write={count(measures.deploy_run_success.reached_production)}
                />{" "}
                of{" "}
                <WindowFigure
                  measure="deploy_run_success_rate"
                  statistic="runs.concluded"
                  targets={targets}
                  value={measures.deploy_run_success.concluded}
                  write={count(measures.deploy_run_success.concluded)}
                />{" "}
                concluded
              </span>
            )}
          </div>
          {measures === undefined || targets === undefined ? null : (
            <>
              <Sparkbars
                judged={{
                  evalSlug: EVAL_SLUGS.deploy_run_success_rate,
                  statistic: "runs.rate",
                  target: targets.deploy_run_success_rate,
                }}
                max={1}
                points={(measures.daily ?? []).map((d) => ({
                  label: d.day,
                  value: d.run_success_rate,
                  partial: d.partial,
                }))}
                write={(v) => (extra) => pct(v, 1 + extra)}
              />
              <div className={`text-[11px] ${textMutedOnSurface}`}>
                <WindowFigure
                  measure="deploy_run_success_rate"
                  statistic="runs.cancelled"
                  targets={targets}
                  value={measures.deploy_run_success.cancelled}
                  write={count(measures.deploy_run_success.cancelled)}
                />{" "}
                cancelled runs not counted
              </div>
            </>
          )}
        </KpiCard>

        <KpiCard
          met={status?.unowned_p0}
          target={targets === undefined ? PENDING : targetLabel(targets.unowned_p0)}
          title="P0 issues with no owner"
        >
          <div className="flex items-baseline gap-2">
            <BigNumber
              measure="unowned_p0"
              statistic="issues.value"
              targets={targets}
              value={data === undefined ? undefined : (data.unowned_p0 ?? []).length}
              write={show((d) => `${(d.unowned_p0 ?? []).length}`)}
            />
            {data === undefined ? null : (
              <span className={`text-xs ${textMutedOnSurface}`}>
                no claim or route, as of{" "}
                {new Date(data.computed_at).toLocaleTimeString([], {
                  hour: "2-digit",
                  minute: "2-digit",
                })}
              </span>
            )}
          </div>
          {data === undefined ? null : (
            <div className="flex flex-wrap gap-x-2 text-xs">
              {(data.unowned_p0 ?? []).slice(0, UNOWNED_P0_SHOWN).map((issue) => (
                <Link
                  className={`hover:underline ${linkText}`}
                  key={issue.key}
                  title={issue.title}
                  to={buildIssuePath({ key: issue.key, kind: "issue" })}
                >
                  {issue.key}
                </Link>
              ))}
              {(data.unowned_p0 ?? []).length > UNOWNED_P0_SHOWN ? (
                <span
                  className={textMutedOnSurface}
                  title={(data.unowned_p0 ?? [])
                    .slice(UNOWNED_P0_SHOWN)
                    .map((issue) => issue.key)
                    .join(", ")}
                >
                  +{(data.unowned_p0 ?? []).length - UNOWNED_P0_SHOWN} more
                </span>
              ) : null}
            </div>
          )}
        </KpiCard>
      </fieldset>

      {/* The prototype's strip scrolls sideways and its fold link follows the stats. In the
          narrower panel Dispatch's sidebar leaves, a scrolling strip hides stats behind the link,
          so the stats wrap, each whole, and the link takes its own line where they fill the row. */}
      <div className="flex flex-wrap items-center gap-x-6 gap-y-1 px-3 py-2 text-sm">
        <div className="flex flex-wrap items-center gap-x-6 gap-y-1" data-testid="measures-strip">
          {strip.map((stat) => (
            <div className="flex items-baseline gap-2 whitespace-nowrap" key={stat.label}>
              {stat.judged === undefined ? (
                <span className={`text-lg font-semibold ${textPrimaryOnSurface}`}>
                  {stat.write(0)}
                </span>
              ) : (
                <BigNumber
                  {...stat.judged}
                  pending={textPrimaryOnSurface}
                  targets={targets}
                  write={stat.write}
                />
              )}
              <span className={`text-xs ${textMutedOnCanvas}`}>{stat.label}</span>
            </div>
          ))}
        </div>
        {data === undefined ? null : (
          <div className="ml-auto shrink-0 whitespace-nowrap">
            <DisclosureToggle
              expanded={open}
              label="Definitions & alternatives"
              onToggle={() => setOpen(!open)}
              textClassName={linkText}
            />
          </div>
        )}
      </div>

      {data === undefined || !open ? null : <Definitions data={data} />}
    </section>
  );
}

/** One day's cell of the fold's per-day table that the target of `measure`'s eval may judge: it
 *  names the eval and the statistic, and is toned by its own value where that target judges a
 *  day's value of `statistic`, plain otherwise, an off target included, and written on its side
 *  of the target (`judgeFigure`). A partial day's cell is judged by no target, so it takes no
 *  tone, its row reading muted. */
function DayCell(props: {
  measure: Measure;
  statistic: Judge;
  targets: DeliveryTargets;
  partial: boolean;
  value: number | null;
  write: WriteFigure;
}): ReactNode {
  const { met, text } = judgeFigure(
    props.partial ? null : props.targets[props.measure],
    props.statistic,
    "day",
    props.value,
    props.write
  );
  return (
    <td
      className={`px-2 py-0.5 text-right ${tone(met)}`}
      data-eval={EVAL_SLUGS[props.measure]}
      data-span="day"
      data-statistic={props.statistic}
    >
      {text}
    </td>
  );
}

/** The fold's eight definition cards and the per-day table, newest first. */
function Definitions({ data }: { data: DeliveryMeasuresResponse }): ReactNode {
  const { measures, targets } = data;
  // Newest first, copied once per answer rather than on every render of the open fold.
  const newestFirst = useMemo(() => [...(measures.daily ?? [])].reverse(), [measures.daily]);
  // Each card's figure of a statistic a delivery eval's target may judge is toned by its own value
  // under that target (`WindowFigure`); a figure of no eval's statistic (the lead times from first
  // commit and from PR opened, the time to restore, the rework share) takes no tone.
  const figure = (
    measure: Measure,
    statistic: Judge,
    value: number | null,
    write: WriteFigure
  ): ReactNode => (
    <WindowFigure
      measure={measure}
      statistic={statistic}
      targets={targets}
      value={value}
      write={write}
    />
  );
  const frequency = measures.deploy_frequency;
  const merge = measures.lead_time.merge_to_production;
  const lead = measures.lead_time;
  const failures = measures.change_failure_rate;
  return (
    <>
      <div
        className={`grid grid-cols-2 gap-4 border-t px-3 pt-3 pb-3 text-sm md:grid-cols-3 ${borderDefault}`}
      >
        <DefinitionCard
          title="Deploy frequency"
          value={figure(
            "deploys_per_day",
            "deploys.per_day",
            frequency.per_day,
            (extra) => `${frequency.per_day.toFixed(2 + extra)}/day`
          )}
        >
          {figure(
            "deploys_per_day",
            "deploys.deploys",
            frequency.successful_deploys,
            count(frequency.successful_deploys)
          )}{" "}
          successful (
          {figure(
            "deploys_per_day",
            "deploys.with_prs",
            frequency.deploys_with_prs,
            count(frequency.deploys_with_prs)
          )}{" "}
          shipped a PR,{" "}
          {figure(
            "deploys_per_day",
            "deploys.with_prs_per_day",
            frequency.with_prs_per_day,
            (extra) => frequency.with_prs_per_day.toFixed(2 + extra)
          )}
          /day)
        </DefinitionCard>

        <DefinitionCard
          title={"Lead time (merge \u2192 prod)"}
          value={figure(
            "merge_to_production_minutes",
            "minutes.median",
            merge.median_minutes,
            (extra) => formatMinutes(merge.median_minutes, extra)
          )}
        >
          p90{" "}
          {figure("merge_to_production_minutes", "minutes.p90", merge.p90_minutes, (extra) =>
            formatMinutes(merge.p90_minutes, extra)
          )}
          , max{" "}
          {figure("merge_to_production_minutes", "minutes.max", merge.max_minutes, (extra) =>
            formatMinutes(merge.max_minutes, extra)
          )}
        </DefinitionCard>

        <DefinitionCard
          title={"Lead time (first commit \u2192 prod)"}
          value={formatMinutes(lead.first_commit_to_production.median_minutes)}
        >
          p90 {formatMinutes(lead.first_commit_to_production.p90_minutes)}
        </DefinitionCard>

        <DefinitionCard
          title={"Lead time (PR opened \u2192 prod)"}
          value={formatMinutes(lead.opened_to_production.median_minutes)}
        >
          p90 {formatMinutes(lead.opened_to_production.p90_minutes)}
        </DefinitionCard>

        {!measured(failures) ? (
          <>
            <DefinitionCard title="Change failure rate (per PR)" value={NO_VALUE}>
              No failure source yet: flags arrive with flag confirmation. Incidents: no source yet.
            </DefinitionCard>
            <DefinitionCard title="Change failure rate (per deploy)" value={NO_VALUE}>
              No failure source yet: flags arrive with flag confirmation.
            </DefinitionCard>
          </>
        ) : (
          <>
            <DefinitionCard
              title="Change failure rate (per PR)"
              value={figure("change_failure_rate", "per_pr.rate", failures.per_pr.rate, (extra) =>
                pct(failures.per_pr.rate, 1 + extra)
              )}
            >
              {figure(
                "change_failure_rate",
                "per_pr.confirmed",
                failures.per_pr.confirmed,
                count(failures.per_pr.confirmed)
              )}{" "}
              confirmed (
              {figure(
                "change_failure_rate",
                "per_pr.reverts",
                failures.per_pr.reverts,
                count(failures.per_pr.reverts)
              )}{" "}
              reverts),{" "}
              {figure(
                "change_failure_rate",
                "per_pr.pending",
                failures.per_pr.pending,
                count(failures.per_pr.pending)
              )}{" "}
              pending,{" "}
              {figure(
                "change_failure_rate",
                "per_pr.rejected",
                failures.per_pr.rejected,
                count(failures.per_pr.rejected)
              )}{" "}
              rejected of{" "}
              {figure(
                "change_failure_rate",
                "per_pr.total",
                failures.per_pr.total,
                count(failures.per_pr.total)
              )}{" "}
              PRs. Incidents: no source yet.
            </DefinitionCard>

            <DefinitionCard
              title="Change failure rate (per deploy)"
              value={figure(
                "change_failure_rate",
                "per_deploy.rate",
                failures.per_deploy.rate,
                (extra) => pct(failures.per_deploy.rate, 1 + extra)
              )}
            >
              {figure(
                "change_failure_rate",
                "per_deploy.confirmed",
                failures.per_deploy.confirmed,
                count(failures.per_deploy.confirmed)
              )}{" "}
              of{" "}
              {figure(
                "change_failure_rate",
                "per_deploy.total",
                failures.per_deploy.total,
                count(failures.per_deploy.total)
              )}{" "}
              deploys;{" "}
              {figure(
                "change_failure_rate",
                "upper_bound.rate",
                failures.per_deploy.upper_bound_rate,
                (extra) => pct(failures.per_deploy.upper_bound_rate, 1 + extra)
              )}{" "}
              if every pending flag is confirmed
            </DefinitionCard>
          </>
        )}

        <DefinitionCard
          title="Time to restore"
          value={formatMinutes(measures.time_to_restore.median_minutes)}
        >
          median, broken PR deploy {"\u2192"} fix deploy
        </DefinitionCard>

        <DefinitionCard title="Rework share" value={pct(measures.rework_share)}>
          of merged PRs in the current filter/window
        </DefinitionCard>
      </div>

      <div className="px-3 pb-3 text-sm">
        <div className={`mb-1 text-xs uppercase ${textMutedOnCanvas}`}>
          Per day (UTC), newest first
        </div>
        <div className={`max-h-48 overflow-y-auto rounded border ${borderDefault}`}>
          <table className="w-full text-xs tabular-nums">
            <thead className={`sticky top-0 ${surfaceBg} ${textMutedOnSurface}`}>
              <tr>
                <th className="px-2 py-1 text-left font-normal">Day</th>
                <th className="px-2 py-1 text-right font-normal">Deploys</th>
                <th className="px-2 py-1 text-right font-normal">Concluded runs</th>
                <th className="px-2 py-1 text-right font-normal">Reached production</th>
                <th className="px-2 py-1 text-right font-normal">Run success</th>
                <th className="px-2 py-1 text-right font-normal">Cancelled</th>
              </tr>
            </thead>
            <tbody>
              {newestFirst.map((d) => (
                <tr
                  className={`border-t ${borderDefault} ${d.partial ? textMutedOnSurface : textPrimaryOnSurface}`}
                  key={d.day}
                >
                  <td className="px-2 py-0.5">
                    {d.day}
                    {d.partial ? " (partial)" : ""}
                  </td>
                  <DayCell
                    measure="deploys_per_day"
                    partial={d.partial}
                    statistic="deploys.per_day"
                    targets={targets}
                    value={d.deploys}
                    write={count(d.deploys)}
                  />
                  <td className="px-2 py-0.5 text-right">{d.concluded}</td>
                  <td className="px-2 py-0.5 text-right">{d.reached_production}</td>
                  <DayCell
                    measure="deploy_run_success_rate"
                    partial={d.partial}
                    statistic="runs.rate"
                    targets={targets}
                    value={d.run_success_rate}
                    write={(extra) =>
                      d.run_success_rate === null ? NO_VALUE : pct(d.run_success_rate, 1 + extra)
                    }
                  />
                  <td className="px-2 py-0.5 text-right">{d.cancelled}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </div>
    </>
  );
}
