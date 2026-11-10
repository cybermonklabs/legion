// One issue's Legion timeline (dispatch://INFRA-979), from GET /api/v1/legion/issues/<KEY>/timeline,
// which the metrics' own computation answers: the issue's phases on a time axis, the segments its
// bars' hours went to, its role turns, CI waits, asks to a person, parked spans, and its
// children's phases under it. Every stretch names its hours and its source, on the axis (its
// title) and in the table under it. A phase is drawn with the issues table's label and colour:
// the board column the route names for it, coloured by `legionPhaseScale`.
import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useMemo } from "react";

import { legionTimelineQuery } from "../../api/queries";
import type { LegionTimelineSpan, LegionIssueTimeline as Timeline } from "../../api/types";
import { QueryError } from "../../components/QueryError";
import {
  borderDefault,
  chartParts,
  legionPhaseScale,
  surfaceBg,
  surfaceMutedBg,
  textMutedOnSurface,
  textPrimaryOnCanvas,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { formatHours } from "./LegionMetrics";

const SEGMENT_LABEL: Record<string, string> = {
  waiting_on_person: "Waiting on a person",
  in_a_turn: "In a turn",
  ci_wait: "CI wait",
  waiting_on_children: "Waiting on children",
  idle: "Idle",
  unknown: "Unknown (not observed)",
};
const SEGMENT_FILL: Record<string, string> = {
  waiting_on_person: chartParts[3],
  in_a_turn: chartParts[2],
  ci_wait: chartParts[1],
  waiting_on_children: chartParts[5],
  idle: chartParts[6],
  unknown: surfaceMutedBg,
};
const SOURCE_LABEL: Record<LegionTimelineSpan["source"], string> = {
  observed: "observed events",
  status_history: "status-history fallback",
  asks: "Dispatch's asks",
};

/** One lane of the axis: its name and its stretches, each with its label and fill. */
interface Lane {
  readonly name: string;
  readonly spans: readonly { span: LegionTimelineSpan; label: string; fill: string }[];
}

function when(iso: string): string {
  return `${iso.slice(0, 10)} ${iso.slice(11, 16)}`;
}

export function LegionIssueTimeline({
  issueKey,
  onClose,
  onOpenIssue,
}: {
  readonly issueKey: string;
  readonly onClose: () => void;
  readonly onOpenIssue: (key: string) => void;
}): ReactNode {
  const query = useQuery(legionTimelineQuery(issueKey));
  return (
    <section
      aria-label={`${issueKey} timeline`}
      className={`flex flex-col gap-2 rounded border-2 px-3 py-2 ${surfaceBg} ${borderDefault}`}
      data-legion-timeline={issueKey}
    >
      <header className="flex flex-wrap items-baseline justify-between gap-2">
        <h2 className={`text-sm font-semibold ${textPrimaryOnCanvas}`}>
          {issueKey}
          {query.data === undefined ? "" : ` · ${query.data.title}`}
        </h2>
        <button className={`text-xs ${textSecondaryOnCanvas}`} onClick={onClose} type="button">
          Close timeline
        </button>
      </header>
      {query.isError ? (
        <QueryError
          message={`Couldn't load ${issueKey}'s timeline.`}
          onRetry={() => void query.refetch()}
          retrying={query.isFetching}
        />
      ) : query.data === undefined ? (
        <p className={`text-sm ${textMutedOnSurface}`}>Loading the timeline…</p>
      ) : (
        <TimelineBody onOpenIssue={onOpenIssue} timeline={query.data} />
      )}
    </section>
  );
}

function TimelineBody({
  onOpenIssue,
  timeline,
}: {
  onOpenIssue: (key: string) => void;
  timeline: Timeline;
}): ReactNode {
  const lanes = useMemo((): Lane[] => {
    const phase = (span: LegionTimelineSpan) => {
      const column = timeline.columns[span.name];
      return {
        span,
        label: column?.label ?? span.name,
        fill: legionPhaseScale[column?.id ?? ""] ?? surfaceMutedBg,
      };
    };
    const plain = (fill: string) => (span: LegionTimelineSpan) => ({
      span,
      label: span.name,
      fill,
    });
    const roles = [...new Set(timeline.turns.map((span) => span.name))].sort();
    return [
      { name: "Phase", spans: timeline.phases.map(phase) },
      {
        name: "Where the hours went",
        spans: timeline.segments.map((span) => ({
          span,
          label: `${SEGMENT_LABEL[span.name] ?? span.name} (${span.bar ?? ""})`,
          fill: SEGMENT_FILL[span.name] ?? surfaceMutedBg,
        })),
      },
      ...roles.map((role) => ({
        name: `Turn: ${role}`,
        spans: timeline.turns.filter((span) => span.name === role).map(plain(chartParts[2])),
      })),
      { name: "CI wait", spans: timeline.ci_waits.map(plain(chartParts[1])) },
      { name: "Asks to a person", spans: timeline.asks.map(plain(chartParts[3])) },
      { name: "Parked", spans: timeline.parked.map(plain(chartParts[6])) },
      ...timeline.children.map((child) => ({
        name: `Child ${child.key}`,
        spans: child.phases.map(phase),
      })),
    ];
  }, [timeline]);
  const all = lanes.flatMap((lane) => lane.spans.map((one) => one.span));
  const start = Math.min(
    ...all.map((span) => Date.parse(span.from)),
    timeline.started_at === null ? Number.POSITIVE_INFINITY : Date.parse(timeline.started_at)
  );
  const end = Math.max(...all.map((span) => Date.parse(span.to)), Date.parse(timeline.ended_at));
  const length = Math.max(end - start, 1);
  const at = (iso: string) => ((Date.parse(iso) - start) / length) * 100;
  const hours = timeline.segment_hours;
  const figures = [
    ["Work time", formatHours(timeline.work_hours)],
    ["Parked", formatHours(timeline.parked_hours)],
    ["Queued", formatHours(timeline.queued_hours)],
    ["Review rounds", `${timeline.review_rounds}`],
    ["Sent back", timeline.sent_back ? "yes" : "no"],
    ["Phases from", SOURCE_LABEL[timeline.source]],
  ];
  return (
    <>
      <dl className="flex flex-wrap gap-x-4 gap-y-1 text-xs">
        {figures.map(([label, value]) => (
          <div className="flex items-baseline gap-1" key={label}>
            <dt className={textMutedOnSurface}>{label}</dt>
            <dd
              className={`font-semibold ${textPrimaryOnCanvas}`}
              data-legion-timeline-figure={label}
            >
              {value}
            </dd>
          </div>
        ))}
        {timeline.root !== timeline.key ? (
          <div className="flex items-baseline gap-1">
            <dt className={textMutedOnSurface}>Tree</dt>
            <dd>
              <button
                className="font-mono underline decoration-dotted"
                onClick={() => onOpenIssue(timeline.root)}
                type="button"
              >
                {timeline.root}
              </button>
            </dd>
          </div>
        ) : null}
      </dl>
      <p className={`text-xs ${textMutedOnSurface}`} data-legion-timeline-segments>
        The bars' hours:{" "}
        {Object.entries(SEGMENT_LABEL)
          .map(([key, label]) => `${label} ${formatHours(hours[key as keyof typeof hours] ?? 0)}`)
          .join(" · ")}
      </p>
      <div
        className="grid grid-cols-[minmax(7rem,11rem)_1fr] items-center gap-x-2 gap-y-1"
        data-legion-timeline-axis
      >
        <span />
        <div className={`flex justify-between text-[11px] ${textMutedOnSurface}`}>
          <span>{when(new Date(start).toISOString())}</span>
          <span>{when(new Date(start + length / 2).toISOString())}</span>
          <span>{when(new Date(end).toISOString())}</span>
        </div>
        {lanes.map((lane) => (
          <LaneRow at={at} key={lane.name} lane={lane} onOpenIssue={onOpenIssue} />
        ))}
      </div>
      <details>
        <summary className={`cursor-pointer text-xs ${textSecondaryOnCanvas}`}>
          Every stretch, with its hours and its source
        </summary>
        <div className="max-h-72 overflow-auto">
          <table className="w-full text-left text-xs" data-legion-timeline-table>
            <thead className={textMutedOnSurface}>
              <tr>
                <th className="px-1 font-normal">Lane</th>
                <th className="px-1 font-normal">What</th>
                <th className="px-1 font-normal">From</th>
                <th className="px-1 font-normal">To</th>
                <th className="px-1 text-right font-normal">Hours</th>
                <th className="px-1 font-normal">Source</th>
              </tr>
            </thead>
            <tbody>
              {lanes.flatMap((lane) =>
                lane.spans.map(({ span, label }) => (
                  <tr
                    className={`border-t ${borderDefault}`}
                    key={`${lane.name}-${span.from}-${label}`}
                  >
                    <td className="px-1">{lane.name}</td>
                    <td className="px-1">{label}</td>
                    <td className="px-1">{when(span.from)}</td>
                    <td className="px-1">{when(span.to)}</td>
                    <td className="px-1 text-right">{formatHours(span.hours)}</td>
                    <td className="px-1">{SOURCE_LABEL[span.source]}</td>
                  </tr>
                ))
              )}
            </tbody>
          </table>
        </div>
      </details>
    </>
  );
}

function LaneRow({
  at,
  lane,
  onOpenIssue,
}: {
  at: (iso: string) => number;
  lane: Lane;
  onOpenIssue: (key: string) => void;
}): ReactNode {
  const child = lane.name.startsWith("Child ") ? lane.name.slice("Child ".length) : null;
  return (
    <>
      <span className={`truncate text-xs ${textMutedOnSurface}`} title={lane.name}>
        {child === null ? (
          lane.name
        ) : (
          <button
            className="font-mono underline decoration-dotted"
            onClick={() => onOpenIssue(child)}
            type="button"
          >
            {child}
          </button>
        )}
      </span>
      <div className={`relative h-5 rounded-sm ${surfaceMutedBg}`} data-legion-lane={lane.name}>
        {lane.spans.map(({ span, label, fill }) => {
          const left = at(span.from);
          const width = Math.max(at(span.to) - left, 0.3);
          return (
            <span
              className={`absolute top-0 h-5 overflow-hidden rounded-sm px-0.5 text-[10px] leading-5 whitespace-nowrap ${fill}`}
              data-legion-span={span.name}
              key={`${span.from}-${label}`}
              style={{ left: `${left}%`, width: `${width}%` }}
              title={`${label}: ${when(span.from)} to ${when(span.to)}, ${formatHours(span.hours)}, from ${SOURCE_LABEL[span.source]}`}
            >
              {width > 6 ? label : ""}
            </span>
          );
        })}
      </div>
    </>
  );
}
