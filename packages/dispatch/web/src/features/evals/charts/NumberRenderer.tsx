// The renderer of a declared `number` (dispatch://INFRA-2918/artifact/plan-md, D10): its tiles, or
// its headline with the numbers beside it, then each of its sections, a title over a row of tiles
// and a line of counts. A small number draws its value large, its total after "of", its date where
// the model gives one, its label, and a grey line of its count ("from 90 tasks"), its interval and
// its reading's note; a tile's value is written in its state's colour, and a count's dot
// takes its row's colour. While the compare range is shown, each value prints its compare value
// and its change beside it. Each figure names itself as the chart's table names it (`FigureText`).
import type { ReactNode } from "react";

import { textMutedOnSurface, textPrimaryOnSurface } from "../../../theme/classes";
import type { ChartRendererProps } from "../chart-model";
import { ComparedText, type FigurePlace, FigureText } from "./FigureText";
import { STATE_TEXT, toneClasses } from "./kind";
import { isNumberModel, type NumberModel, type NumberPoint } from "./number-model";

function placeOf(model: NumberModel, point: NumberPoint): FigurePlace {
  return { chart: model.name, point, series: model.series[0]?.name ?? "" };
}

/** A value and, where it has one, "of" its total. */
function Valued(props: { className: string; place: FigurePlace }): ReactNode {
  const { place } = props;
  return (
    <>
      <FigureText className={props.className} place={place} range="window" statistic="value" />
      {place.point.values.of === undefined ? null : (
        <>
          <span className={`text-xs ${textMutedOnSurface}`}>of</span>
          <FigureText place={place} range="window" statistic="of" />
        </>
      )}
    </>
  );
}

/** A small number's grey line: its count, its interval and its reading's note. */
function GreyLine(props: { place: FigurePlace; interval: string | null }): ReactNode {
  const { place } = props;
  const { values } = place.point;
  const note = values.value?.note ?? null;
  const counted = values.n !== undefined;
  const interval = props.interval !== null && values.ci_low?.value != null;
  if (!counted && !interval && note === null) return null;
  return (
    <span className={`flex flex-wrap gap-x-2 text-xs ${textMutedOnSurface}`}>
      {counted ? (
        <span>
          {"from "}
          <FigureText place={place} range="window" statistic="n" />
        </span>
      ) : null}
      {interval ? (
        <span>
          {"["}
          <FigureText place={place} range="window" statistic="ci_low" />
          {", "}
          <FigureText place={place} range="window" statistic="ci_high" />
          {`] (${props.interval})`}
        </span>
      ) : null}
      {note === null ? null : <span>{note}</span>}
    </span>
  );
}

/** A small number at the top or in a section: the headline, a number beside it, or a tile. */
function SmallNumber(props: {
  compare: boolean;
  model: NumberModel;
  point: NumberPoint;
}): ReactNode {
  const { point } = props;
  const place = placeOf(props.model, point);
  return (
    <div
      className="flex min-w-0 gap-2"
      data-number-part={point.part}
      data-number-place={point.x}
      data-state={point.state ?? undefined}
      title={point.state ?? undefined}
    >
      <div className="flex min-w-0 flex-col">
        <span className={`flex flex-wrap items-baseline gap-x-1 ${textPrimaryOnSurface}`}>
          <Valued
            className={`${point.part === "headline" ? "text-[22px]" : "text-[15px]"} font-semibold ${
              point.state === null ? "" : STATE_TEXT[point.state]
            }`}
            place={place}
          />
          {point.date === null ? null : (
            <span className={`text-xs ${textMutedOnSurface}`}>{point.date}</span>
          )}
          <ComparedText compare={props.compare} place={place} statistic="value" />
        </span>
        <span className={`text-xs ${textPrimaryOnSurface}`}>{point.label}</span>
        <GreyLine interval={point.interval} place={place} />
      </div>
    </div>
  );
}

/** A row of small numbers. */
function NumberRow(props: {
  compare: boolean;
  model: NumberModel;
  points: readonly NumberPoint[];
}): ReactNode {
  if (props.points.length === 0) return null;
  return (
    <div className="flex flex-wrap items-start gap-x-6 gap-y-3">
      {props.points.map((point) => (
        <SmallNumber compare={props.compare} key={point.x} model={props.model} point={point} />
      ))}
    </div>
  );
}

/** A section's counts, one line: each row's dot, name, value, total and date. */
function Counts(props: {
  compare: boolean;
  model: NumberModel;
  points: readonly NumberPoint[];
}): ReactNode {
  if (props.points.length === 0) return null;
  return (
    <ul className={`flex flex-wrap gap-x-4 gap-y-1 text-sm ${textPrimaryOnSurface}`}>
      {props.points.map((point) => {
        const place = placeOf(props.model, point);
        return (
          <li
            className="flex items-baseline gap-1"
            data-number-part="count"
            data-number-place={point.x}
            key={point.x}
          >
            <span
              aria-hidden="true"
              className={`size-2 shrink-0 self-center rounded-full ${toneClasses(point.tone ?? "other", false).swatch}`}
            />
            <span>{`${point.label}:`}</span>
            <Valued className="font-semibold" place={place} />
            {point.date === null ? null : (
              <span className={`text-xs ${textMutedOnSurface}`}>{point.date}</span>
            )}
            <ComparedText compare={props.compare} place={place} statistic="value" />
          </li>
        );
      })}
    </ul>
  );
}

/**
 * A declared number (plan D10): its tiles, else its headline and the numbers beside it, then each
 * of its sections; a section's counts series with no reading that has a value draws its line in
 * the counts' place. On the tab each part draws apart (`numberParts`): the top in the number's
 * place, and each section alone after the chart its `after` names, where the chart's caption is
 * the section's title, so a section drawn alone draws no heading of its own.
 */
export function NumberRenderer({ compare, model }: ChartRendererProps): ReactNode {
  if (!isNumberModel(model)) return null;
  const points = model.series[0]?.points ?? [];
  const top = points.filter((point) => point.section === null);
  const alone = top.length === 0 && model.number.sections.length === 1;
  return (
    <div className="flex flex-col gap-4" data-number={model.name}>
      {top.length === 0 ? null : model.number.tiles ? (
        <NumberRow compare={compare} model={model} points={top} />
      ) : (
        <div className="flex flex-wrap items-end gap-x-6 gap-y-2">
          <NumberRow
            compare={compare}
            model={model}
            points={top.filter((point) => point.part === "headline")}
          />
          <NumberRow
            compare={compare}
            model={model}
            points={top.filter((point) => point.part === "beside")}
          />
        </div>
      )}
      {model.number.sections.map((section, index) => {
        const own = points.filter((point) => point.section === index);
        return (
          <section
            aria-label={section.title}
            className="flex flex-col gap-2"
            data-after={section.after}
            data-number-section={index}
            // biome-ignore lint/suspicious/noArrayIndexKey: a section's place is its identity; two may share a title.
            key={index}
          >
            {alone ? null : (
              <h5 className={`text-sm font-semibold ${textPrimaryOnSurface}`}>{section.title}</h5>
            )}
            {section.subtitle === null ? null : (
              <p className={`text-xs ${textMutedOnSurface}`}>{section.subtitle}</p>
            )}
            <NumberRow
              compare={compare}
              model={model}
              points={own.filter((point) => point.part === "tile")}
            />
            <Counts
              compare={compare}
              model={model}
              points={own.filter((point) => point.part === "count")}
            />
            {section.countsEmpty === null ? null : (
              <p className={`text-xs ${textMutedOnSurface}`} data-number-empty={section.counts}>
                {section.countsEmpty}
              </p>
            )}
          </section>
        );
      })}
    </div>
  );
}
