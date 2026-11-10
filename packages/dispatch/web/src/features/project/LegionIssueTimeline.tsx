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
  borderStrong,
  chartParts,
  legionPhaseScale,
  surfaceBg,
  surfaceMutedBg,
  textMutedOnSurface,
  textPrimaryOnCanvas,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { legionPhaseLabel } from "./issues-table-model";
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
  waiting_on_person: legionPhaseScale.awaiting_person ?? "",
  in_a_turn: chartParts[2],
  ci_wait: chartParts[1],
  waiting_on_children: legionPhaseScale.waiting_on_children ?? "",
  idle: chartParts[6],
  // Unknown hours draw as an outlined stretch, visible on the lane's own muted track.
  unknown: `${surfaceBg} border border-dashed ${borderStrong}`,
};
/** An ask's own stretch: a darker rose than the waiting-on-a-person hours it causes. */
const ASK_FILL = chartParts[4];
const SOURCE_LABEL: Record<LegionTimelineSpan["source"], string> = {
  observed: "the daemon's observed events",
  status_history: "the issue's status changes",
  asks: "Dispatch's asks",
};

/** The phases after implementing that a return to implementing counts as a send-back from. */
const SENT_BACK_FROM = new Set(["testing", "reviewing", "accepting", "retro", "merging"]);

/** One stretch of a lane: its label and fill, hatched where a root's phase overlaps the hours it
 *  waited on its children, or grey and hatched where the issue was parked. */
interface LaneSpan {
  readonly span: LegionTimelineSpan;
  readonly label: string;
  readonly fill: string;
  readonly hatch?: "children" | "parked";
}

/** One lane of the axis: its name and its stretches. */
interface Lane {
  readonly name: string;
  readonly spans: readonly LaneSpan[];
}

/** The hatch drawn over a phase's hours a root spent waiting on its children. */
const HATCH = {
  backgroundImage:
    "repeating-linear-gradient(135deg, rgba(255, 255, 255, 0.6) 0 3px, transparent 3px 7px)",
};

/** Splits each phase stretch where it overlaps one of `waits`, so the Phase lane tells the same
 *  story as the lanes under it: a root's hours waiting on its children read "planning · waiting
 *  on children", and an issue's parked hours read "parked", in grey. */
function splitBy(
  spans: readonly LaneSpan[],
  waits: readonly LegionTimelineSpan[],
  hatch: "children" | "parked"
): LaneSpan[] {
  const cuts = waits.map((wait) => [Date.parse(wait.from), Date.parse(wait.to)] as const);
  return spans.flatMap((one) => {
    const from = Date.parse(one.span.from);
    const to = Date.parse(one.span.to);
    const bounds = [
      ...new Set([from, to, ...cuts.flat().filter((point) => point > from && point < to)]),
    ].sort((a, b) => a - b);
    const pieces: LaneSpan[] = [];
    for (let index = 0; index + 1 < bounds.length; index++) {
      const start = bounds[index] as number;
      const end = bounds[index + 1] as number;
      const hatched = cuts.some(([a, b]) => a <= start && end <= b);
      const previous = pieces.at(-1);
      if (previous !== undefined && (previous.hatch === hatch) === hatched) {
        pieces[pieces.length - 1] = {
          ...previous,
          span: {
            ...previous.span,
            to: new Date(end).toISOString(),
            hours: (end - Date.parse(previous.span.from)) / 3_600_000,
          },
        };
        continue;
      }
      pieces.push({
        ...one,
        ...(hatched
          ? hatch === "children"
            ? { hatch, label: `${one.label.toLowerCase()} · waiting on children` }
            : { hatch, label: "parked", fill: chartParts[6] }
          : {}),
        span: {
          ...one.span,
          from: new Date(start).toISOString(),
          to: new Date(end).toISOString(),
          hours: (end - start) / 3_600_000,
        },
      });
    }
    return pieces;
  });
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
        // The status history's `retro` status stands for the merger's whole stretch (retro,
        // merging and awaiting merge), so only an observed `retro` reads "Merging (retro)".
        label:
          span.source === "observed"
            ? legionPhaseLabel(column?.label ?? span.name, span.name)
            : (column?.label ?? span.name),
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
      {
        name: "Phase",
        spans: splitBy(
          splitBy(
            timeline.phases.map(phase),
            timeline.segments.filter((span) => span.name === "waiting_on_children"),
            "children"
          ),
          timeline.parked,
          "parked"
        ),
      },
      {
        name: "Where the hours went",
        // A row that is all unknown says nothing: it is left out and said in one sentence.
        spans: timeline.segments.some((span) => span.name !== "unknown")
          ? timeline.segments.map((span) => ({
              span,
              label: `${SEGMENT_LABEL[span.name] ?? span.name} (${span.bar ?? ""})`,
              fill: SEGMENT_FILL[span.name] ?? surfaceMutedBg,
            }))
          : [],
      },
      ...roles.map((role) => ({
        name: `Turn: ${role}`,
        spans: timeline.turns.filter((span) => span.name === role).map(plain(chartParts[2])),
      })),
      { name: "CI wait", spans: timeline.ci_waits.map(plain(chartParts[1])) },
      {
        name: "Asks to a person",
        spans: timeline.asks.map(plain(ASK_FILL)),
      },
      { name: "Parked", spans: timeline.parked.map(plain(chartParts[6])) },
      ...timeline.children.map((child) => ({
        name: `Child ${child.key}`,
        spans: child.phases.map(phase),
      })),
    ];
  }, [timeline]);
  // A lane with nothing recorded is left out and named once under the axis.
  const empty = lanes
    .filter((lane) => lane.spans.length === 0 && lane.name !== "Where the hours went")
    .map((lane) => lane.name);
  const split = lanes.some((lane) => lane.name === "Where the hours went" && lane.spans.length > 0);
  const shown = lanes.filter((lane) => lane.spans.length > 0);
  const all = shown.flatMap((lane) => lane.spans.map((one) => one.span));
  // The colour key: each phase once in the order drawn (the Phase lane and the child lanes), then
  // each segment of "Where the hours went", then the turns, CI waits, asks and parked stretches.
  const segmentNames = new Set(
    shown
      .filter((lane) => lane.name === "Where the hours went")
      .flatMap((lane) => lane.spans.map((one) => one.span.name))
  );
  const has = (name: string) => shown.some((lane) => lane.name === name);
  const phaseKey = [
    ...new Map([
      ...shown
        .filter((lane) => lane.name === "Phase" || lane.name.startsWith("Child "))
        .flatMap((lane) => lane.spans.filter((one) => one.hatch === undefined))
        // The key names the phase; "(retro)" stays on the retro blocks themselves.
        .map((one) => [one.label.replace(/ \(retro\)$/, ""), one.fill] as const),
      ...Object.entries(SEGMENT_LABEL)
        .filter(([name]) => segmentNames.has(name))
        .map(([name, label]) => [label, SEGMENT_FILL[name] ?? surfaceMutedBg] as const),
      ...(shown.some((lane) => lane.name.startsWith("Turn: "))
        ? [["A role in a turn", chartParts[2]] as const]
        : []),
      ...(has("CI wait") ? [["CI wait", chartParts[1]] as const] : []),
      ...(has("Asks to a person") ? [["An ask to a person", ASK_FILL] as const] : []),
      ...(has("Parked") ? [["Parked", chartParts[6]] as const] : []),
    ]),
  ];
  const hatchedAny = shown.some((lane) => lane.spans.some((one) => one.hatch === "children"));
  const parkedAny = shown.some((lane) => lane.spans.some((one) => one.hatch === "parked"));
  const start = Math.min(
    ...all.map((span) => Date.parse(span.from)),
    timeline.started_at === null ? Number.POSITIVE_INFINITY : Date.parse(timeline.started_at)
  );
  const end = Math.max(...all.map((span) => Date.parse(span.to)), Date.parse(timeline.ended_at));
  const length = Math.max(end - start, 1);
  const at = (iso: string) => ((Date.parse(iso) - start) / length) * 100;
  const hours = timeline.segment_hours;
  // Each return to implementing from a later phase is one send-back.
  const sentBack = timeline.phases.filter(
    (span, index) =>
      span.name === "implementing" && SENT_BACK_FROM.has(timeline.phases[index - 1]?.name ?? "")
  ).length;
  const figures = [
    ["Work time", formatHours(timeline.work_hours)],
    ["Parked", formatHours(timeline.parked_hours)],
    ["Queued", formatHours(timeline.queued_hours)],
    ["Review rounds", `${timeline.review_rounds}`],
  ];
  const story = [
    `Phases read from ${SOURCE_LABEL[timeline.source]}.`,
    sentBack > 0
      ? `Sent back to implementing ${sentBack === 1 ? "once" : `${sentBack} times`} over ${timeline.review_rounds} review round${timeline.review_rounds === 1 ? "" : "s"}.`
      : timeline.sent_back
        ? "Sent back to implementing at least once."
        : "Never sent back to implementing.",
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
      <p className={`text-xs ${textSecondaryOnCanvas}`} data-legion-timeline-story>
        {story.join(" ")}
      </p>
      <p className={`text-xs ${textMutedOnSurface}`} data-legion-timeline-segments>
        {split
          ? `The bars' hours: ${Object.entries(SEGMENT_LABEL)
              .filter(([key]) => (hours[key as keyof typeof hours] ?? 0) > 0)
              .map(
                ([key, label]) => `${label} ${formatHours(hours[key as keyof typeof hours] ?? 0)}`
              )
              .join(" · ")}.`
          : "Nothing recorded splits this issue's hours into turns, CI waits, asks or children, so the row of where the hours went is left out."}
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
        {shown.map((lane) => (
          <LaneRow at={at} key={lane.name} lane={lane} onOpenIssue={onOpenIssue} />
        ))}
      </div>
      <ul className="flex flex-wrap gap-x-3 gap-y-1 text-[11px]" data-legion-timeline-key>
        {phaseKey.map(([label, fill]) => (
          <li className="flex items-center gap-1" key={label}>
            <span aria-hidden className={`inline-block h-3 w-3 rounded-sm ${fill}`} />
            <span className={textSecondaryOnCanvas}>{label}</span>
          </li>
        ))}
        {hatchedAny ? (
          <li className="flex items-center gap-1">
            <span
              aria-hidden
              className={`inline-block h-3 w-3 rounded-sm ${legionPhaseScale.planning ?? ""}`}
              style={HATCH}
            />
            <span className={textSecondaryOnCanvas}>hatched: the root waiting on its children</span>
          </li>
        ) : null}
        {parkedAny ? (
          <li className="flex items-center gap-1">
            <span
              aria-hidden
              className={`inline-block h-3 w-3 rounded-sm ${chartParts[6]}`}
              style={HATCH}
            />
            <span className={textSecondaryOnCanvas}>hatched grey: parked</span>
          </li>
        ) : null}
      </ul>
      {empty.length === 0 ? null : (
        <p className={`text-xs ${textMutedOnSurface}`} data-legion-timeline-empty>
          Nothing recorded: {empty.join(", ")}.
        </p>
      )}
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
              {shown.flatMap((lane) =>
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
        {lane.spans.map(({ span, label, fill, hatch }) => {
          const left = at(span.from);
          const width = Math.max(at(span.to) - left, 0.3);
          return (
            <span
              className={`absolute top-0 h-5 overflow-hidden rounded-sm px-0.5 text-[10px] leading-5 whitespace-nowrap ${fill}`}
              data-legion-hatched={hatch}
              data-legion-span={span.name}
              key={`${span.from}-${label}`}
              style={{
                left: `${left}%`,
                width: `${width}%`,
                ...(hatch === undefined ? {} : HATCH),
              }}
              title={`${label}: ${when(span.from)} to ${when(span.to)}, ${formatHours(span.hours)}, from ${SOURCE_LABEL[span.source]}`}
            >
              {/* An unknown stretch carries no text: the key names it. */}
              {span.name === "unknown"
                ? ""
                : width > 12
                  ? label
                  : width > 6
                    ? (label.split(" · ")[0] ?? "")
                    : ""}
            </span>
          );
        })}
      </div>
    </>
  );
}
