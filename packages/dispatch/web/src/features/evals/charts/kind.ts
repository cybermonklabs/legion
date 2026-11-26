// What every kind of declared chart is (dispatch://INFRA-2918/artifact/plan-md, D4, D10): the
// items views its spec reads, a pure model function from the spec and those readings to the model
// `EvalChart` draws, and the renderer that draws that model; with the readers of a spec's fields,
// the texts every kind draws around its chart, and the colour each group of a chart takes. Pure.
import {
  chartGroup,
  chartGroupOther,
  chartNotStartedGrey,
  chartState,
  chartStateAttentionText,
  chartStateContextText,
  chartStateNotStartedText,
  chartStatePartialText,
  chartStateProvedText,
} from "../../../theme/classes";
import type { ChartModel, ChartRenderer, EvalRange } from "../chart-model";
import type { DeclaredReading } from "./readings";

/** One declared chart as its eval's detail answers it: an element of `charts`, which the register
 *  write validated (`internal/dispatch/evals/charts`) before it stored it. */
export type DeclaredSpec = Readonly<Record<string, unknown>>;

/** What a kind's model draws from: the eval it is of (its slug), the header's window and compare
 *  range as the items views answered them, and the readings those views hold, oldest first. */
export interface DeclaredInput {
  readonly slug: string;
  readonly window: EvalRange;
  readonly compare: EvalRange | null;
  readonly readings: readonly DeclaredReading[];
}

/** One items view a chart reads: the readings of one series, or of one row kind. */
export interface ChartRead {
  readonly series?: string;
  readonly rowKind?: string;
}

/**
 * A kind of declared chart, as `chartKinds` (registry.ts) holds it:
 * - `reads`: the items views its spec reads, each opened once however many charts name it;
 * - `model`: the chart's model (D4) from its spec and the readings of those views, each value with
 *   its label, state and note, in the order it draws, and with the compare range's readings as D10
 *   says the kind draws them;
 * - `renderer`: draws that model, and names every value it draws as the table does
 *   (`data-chart`, `data-series`, `data-statistic`, `data-x`, `data-range`, `data-value`), so the
 *   table holds every number the graph draws.
 */
export interface ChartKind {
  readonly reads: (spec: DeclaredSpec) => readonly ChartRead[];
  readonly model: (spec: DeclaredSpec, input: DeclaredInput) => ChartModel;
  readonly renderer: ChartRenderer;
}

/** A spec field that is an object, else an empty one. */
export function specObject(value: unknown): DeclaredSpec {
  return typeof value === "object" && value !== null && !Array.isArray(value)
    ? (value as DeclaredSpec)
    : {};
}

/** A spec field that is text, else null. */
export function specText(value: unknown): string | null {
  return typeof value === "string" ? value : null;
}

/** A spec field that is a finite number, else null. */
export function specNumber(value: unknown): number | null {
  return typeof value === "number" && Number.isFinite(value) ? value : null;
}

/** A spec field that is a list, else an empty one. */
export function specList(value: unknown): readonly unknown[] {
  return Array.isArray(value) ? value : [];
}

/** A spec field that is one line of text or a list of them, as its lines. */
export function specLines(value: unknown): string[] {
  return (typeof value === "string" ? [value] : specList(value)).flatMap((line) =>
    typeof line === "string" && line !== "" ? [line] : []
  );
}

/** The name of a declared chart in the page (`data-chart`): its eval's slug and its own id, which
 *  is unique in the eval. */
export function declaredChartName(slug: string, spec: DeclaredSpec): string {
  return `${slug}/${specText(spec.id) ?? ""}`;
}

/** The words every kind draws around its chart: a heading above its title, the lines under its
 *  title, its footnote under its plot, its side note (beside a half-width plot, else under the
 *  footnote) and its note, and whether it takes half the row. */
export interface ChartTexts {
  readonly heading: string | null;
  readonly subtitle: readonly string[];
  readonly footnote: readonly string[];
  readonly sideNote: readonly string[];
  readonly note: string | null;
  readonly half: boolean;
}

export function chartTexts(spec: DeclaredSpec): ChartTexts {
  const note = specText(spec.note);
  return {
    footnote: specLines(spec.footnote),
    half: spec.width === "half",
    heading: specText(spec.heading),
    note: note === null || note === "" ? null : note,
    sideNote: specLines(spec.side_note),
    subtitle: specLines(spec.subtitle),
  };
}

/** The five states a group, a rule or an annotation of a chart may name. */
export const MARK_STATES = ["proved", "partial", "attention", "context", "not-started"] as const;
export type MarkState = (typeof MARK_STATES)[number];

/** A spec field that names one of the five states, else null. */
export function markState(value: unknown): MarkState | null {
  return MARK_STATES.find((state) => state === value) ?? null;
}

/** A group's colour: its place among the chart's groups (the palette's first ten, then "other"),
 *  a state, or "overall", the near black of the median over every run. */
export type Tone = number | MarkState | "other" | "overall";

/** How many groups take a hue of their own; a group past them draws as "other". */
export const PALETTE_SIZE = chartGroup.length;

/**
 * Each of `names`, the groups of a chart in the order of their first readings, with its colour
 * and in the order it draws: the groups `order` names first, in its order, then the rest as they
 * came. A group's place is its place in `order`, whether or not the chart draws the groups before
 * it, so charts that declare one order draw each group in one colour; `states` gives a group its
 * state's colour, and `highlight` draws its group in the palette's first colour and every other
 * in grey.
 */
export function groupTones(
  spec: DeclaredSpec,
  names: readonly string[]
): { readonly name: string; readonly tone: Tone }[] {
  const order = specList(spec.order).filter((name): name is string => typeof name === "string");
  const states = specObject(spec.states);
  const highlight = specText(spec.highlight);
  const unnamed = names.filter((name) => !order.includes(name));
  const placed = names.map((name) => ({
    name,
    place: order.includes(name) ? order.indexOf(name) : order.length + unnamed.indexOf(name),
  }));
  return placed
    .sort((a, b) => a.place - b.place)
    .map(({ name, place }) => {
      const state = markState(states[name]);
      if (state !== null) return { name, tone: state };
      if (highlight !== null) return { name, tone: name === highlight ? 0 : "other" };
      return { name, tone: place < PALETTE_SIZE ? place : "other" };
    });
}

/** The classes a mark of `tone` draws with: its stroke, fill, faint band fill and key swatch; a
 *  not-started outline grey where the chart sets `not_started_outline: "grey"`. */
export function toneClasses(
  tone: Tone,
  notStartedGrey: boolean
): {
  readonly stroke: string;
  readonly fill: string;
  readonly band: string;
  readonly swatch: string;
} {
  if (typeof tone === "number") return chartGroup[tone] ?? chartGroupOther;
  if (tone === "other") return chartGroupOther;
  if (tone === "not-started" && notStartedGrey) {
    return { ...chartState["not-started"], stroke: chartNotStartedGrey };
  }
  return chartState[tone];
}

/** The colour of the text that states a mark's state (a grid's cell, a lane's end text, a
 *  number's tile), as the eval platform colours the text rather than drawing a swatch beside it. */
export const STATE_TEXT: Readonly<Record<MarkState, string>> = {
  attention: chartStateAttentionText,
  context: chartStateContextText,
  "not-started": chartStateNotStartedText,
  partial: chartStatePartialText,
  proved: chartStateProvedText,
};
