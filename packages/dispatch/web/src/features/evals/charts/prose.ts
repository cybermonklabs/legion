// The `prose` kind (dispatch://INFRA-2918/artifact/plan-md, D10): a text section, its title the
// section's title and its paragraphs the team's words in body text, as the eval platform (at
// commit 4f9223b9) draws one. It reads no readings and holds no number, so the compare range
// changes nothing: its text draws once, and its table holds each paragraph as a row, with no
// compare value or change, since none exists. Pure.
import type { ChartModel, ChartSeries } from "../chart-model";
import { type ChartKind, type DeclaredSpec, declaredChartName, specList, specText } from "./kind";
import { ProseRenderer } from "./ProseRenderer";

/** The name of the one series a prose model holds, its paragraphs: no eval series, and it holds
 *  no number, so no `data-value` names it. */
export const PARAGRAPHS = "paragraphs";

/** The model of a `prose` spec: one series of its paragraphs in the order the spec lists them, a
 *  row of the table each, every paragraph's text its row's value. No compare value and no change:
 *  prose holds no number. */
export function proseModel(
  spec: DeclaredSpec,
  slug: string,
  compare: ChartModel["compareRange"]
): ChartModel {
  const paragraphs = specList(spec.paragraphs).filter(
    (paragraph): paragraph is string => typeof paragraph === "string" && paragraph !== ""
  );
  const series: ChartSeries = {
    dataNote: null,
    headline: [],
    label: "Paragraphs",
    name: PARAGRAPHS,
    noun: "paragraphs",
    points: paragraphs.map((paragraph, index) => ({
      change: null,
      compare: null,
      label: String(index + 1),
      partial: false,
      values: { text: { label: paragraph, note: null, state: "plain", value: null } },
      x: String(index + 1),
    })),
    repositoryNote: null,
    statistics: [{ id: "text", label: "Text" }],
    target: null,
    unit: "",
    window: {
      change: null,
      compare: null,
      label: "Window",
      partial: false,
      values: {},
      x: "window",
    },
  };
  return {
    axis: { label: "Paragraph", many: "paragraphs", one: "paragraph" },
    comparable: true,
    compareRange: compare,
    kind: "prose",
    name: declaredChartName(slug, spec),
    series: [series],
    title: specText(spec.title) ?? specText(spec.id) ?? "",
  };
}

export const proseKind: ChartKind = {
  model: (spec, input) => proseModel(spec, input.slug, input.compare),
  reads: () => [],
  renderer: ProseRenderer,
};
