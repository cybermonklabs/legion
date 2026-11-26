import {
  keepPreviousData,
  type UseQueryResult,
  useQueries,
  useQuery,
  useQueryClient,
} from "@tanstack/react-query";
import { type ReactNode, useMemo } from "react";

import { EVALS_VIEW_LIMIT, evalDetailQuery, itemsViewQuery } from "../../api/queries";
import type {
  EvalDetailResponse,
  EvalItemsResponse,
  EvalListItem,
  EvalListResponse,
} from "../../api/types";
import { QueryError } from "../../components/QueryError";
import { textMutedOnSurface, textPrimaryOnSurface } from "../../theme/classes";
import { type ChartModel, type ChartRenderer, evalName } from "./chart-model";
import {
  type ChartRead,
  chartTexts,
  type DeclaredInput,
  type DeclaredSpec,
  specObject,
  specText,
} from "./charts/kind";
import { isNumberModel, type NumberSectionPart, numberParts } from "./charts/number-model";
import { compareReadings, type DeclaredReading, readingsOf } from "./charts/readings";
import { chartKinds } from "./charts/registry";
import { EvalChart } from "./EvalChart";
import { EvalTitle } from "./EvalPanel";
import { type EvalsScope, laterOffsets } from "./useEvalsViews";

/** The charts an eval's newest version declares, as its detail read answers them (plan D10): none
 *  until the detail has answered, or where the eval declares none. */
export function declaredCharts(detail: EvalDetailResponse | undefined): readonly DeclaredSpec[] {
  const charts = detail?.items?.[0]?.charts;
  return Array.isArray(charts) ? charts.map(specObject) : [];
}

/** The kind a declared chart names, where the tab draws it. */
function kindOf(spec: DeclaredSpec) {
  const name = specText(spec.kind) ?? "";
  return Object.hasOwn(chartKinds, name) ? chartKinds[name] : undefined;
}

/** A read's key among the eval's reads, so two charts that read one series share its view. */
function readKey(read: ChartRead): string {
  return `${read.series ?? ""}\n${read.rowKind ?? ""}`;
}

/** One read's readings: every page of its items view, and whether each has landed. */
interface ReadState {
  readonly landed: boolean;
  readonly rows: readonly DeclaredReading[];
}

/** What the tab keeps of a set of items views: each page as answered (none until it lands),
 *  which views stand refused, and the first refusal. `useQueries` shares it structurally, so it
 *  changes only as a page lands or a view is refused. */
function pagesOf(results: UseQueryResult<EvalItemsResponse>[]) {
  return {
    error: results.find((result) => result.isError)?.error ?? null,
    failed: results.map((result) => result.isError),
    pages: results.map((result) => result.data),
  };
}

/**
 * The readings `reads` name of eval `slug` over the header's window and compare range (plan D9,
 * D10): one items view per read, each a page of EVALS_VIEW_LIMIT rows (the items route's most, as
 * the eval list's), and one view more at each next offset where its first page counts more. Every
 * component that reads one view shares its one subscription, through its query key. No view names
 * the header's repositories: a logged eval's readings belong to no repository, and the items route
 * keeps only the readings whose `attributes.repo` a `repo` names, so one would drop them all.
 */
function useReadings(scope: EvalsScope, slug: string, reads: readonly ChartRead[]) {
  const client = useQueryClient();
  const view = (read: ChartRead, offset: number) =>
    itemsViewQuery({
      compare: scope.compare,
      limit: EVALS_VIEW_LIMIT,
      offset,
      project: scope.project,
      repos: [],
      rowKind: read.rowKind,
      series: read.series,
      slug,
      window: scope.window,
    });
  const firstViews = reads.map((read) => view(read, 0));
  const firsts = useQueries({ combine: pagesOf, queries: firstViews });
  const laterViews = reads.flatMap((read, index) =>
    laterOffsets(firsts.pages[index]?.total ?? 0).map((offset) => view(read, offset))
  );
  const rest = useQueries({ combine: pagesOf, queries: laterViews });
  const states = useMemo(() => {
    const held = new Map<string, ReadState>();
    // The later pages stand in `rest` read by read, each read's in the order of its offsets.
    let at = 0;
    reads.forEach((read, index) => {
      const first = firsts.pages[index];
      const pages = [first];
      for (const _offset of laterOffsets(first?.total ?? 0)) pages.push(rest.pages[at++]);
      held.set(readKey(read), {
        landed: pages.every((page) => page !== undefined),
        rows: readingsOf(pages.flatMap((page) => page?.items ?? [])),
      });
    });
    return held;
  }, [reads, firsts.pages, rest.pages]);
  return {
    error: firsts.error ?? rest.error,
    retry: () => {
      const refused = [
        ...firstViews.filter((_, index) => firsts.failed[index]),
        ...laterViews.filter((_, index) => rest.failed[index]),
      ];
      for (const one of refused)
        void client.refetchQueries({ exact: true, queryKey: one.queryKey });
    },
    states,
  };
}

/** One declared chart: its heading, then `model` drawn by its kind through `EvalChart`, with its
 *  subtitle under its title and its footnote, side note and note after it, or a line saying its
 *  readings are being read while `model` is null; a kind the tab does not draw, a line naming the
 *  chart and its kind. */
function DeclaredChart({
  model,
  slug,
  spec,
}: {
  model: ChartModel | null;
  slug: string;
  spec: DeclaredSpec;
}): ReactNode {
  const kind = kindOf(spec);
  const title = specText(spec.title) ?? specText(spec.id) ?? "";
  const texts = chartTexts(spec);
  const id = specText(spec.id) ?? "";
  const kindName = specText(spec.kind) ?? "";
  const lines = (part: string, text: readonly string[]) =>
    text.map((line) => (
      <p className={`text-xs ${textMutedOnSurface}`} data-chart-text={part} key={`${part}:${line}`}>
        {line}
      </p>
    ));
  if (kind === undefined) {
    return (
      <p
        className={`text-xs ${textMutedOnSurface} @min-[42rem]:col-span-2`}
        data-declared-chart={id}
        data-declared-kind={kindName}
      >
        {`${title}: the tab draws no chart of kind ${kindName === "" ? "(none)" : kindName}.`}
      </p>
    );
  }
  return (
    <section
      aria-label={title}
      className={`flex min-w-0 flex-col gap-1 ${texts.half ? "" : "@min-[42rem]:col-span-2"}`}
      data-declared-chart={id}
      data-declared-kind={kindName}
    >
      {texts.heading === null ? null : (
        <h4 className={`text-xs font-semibold uppercase ${textMutedOnSurface}`}>{texts.heading}</h4>
      )}
      {model === null ? (
        <p className={`text-xs ${textMutedOnSurface}`} role="status">
          {`Reading the readings of ${title}…`}
        </p>
      ) : (
        <EvalChart
          footer={
            texts.footnote.length + texts.sideNote.length === 0 &&
            texts.note === null ? undefined : (
              <div className="flex flex-col gap-0.5">
                {lines("footnote", texts.footnote)}
                {lines("side_note", texts.sideNote)}
                {texts.note === null ? null : lines("note", [texts.note])}
              </div>
            )
          }
          model={model}
          opensPanel={slug}
          renderer={kind.renderer}
          subtitle={
            texts.subtitle.length === 0 ? undefined : (
              <div className="flex flex-col gap-0.5">{lines("subtitle", texts.subtitle)}</div>
            )
          }
        />
      )}
    </section>
  );
}

/** A section of a declared number, drawn alone through `EvalChart` with the number's renderer, at
 *  the full width of the eval's grid. */
function DeclaredNumberSection({
  part,
  renderer,
  slug,
}: {
  part: NumberSectionPart;
  renderer: ChartRenderer;
  slug: string;
}): ReactNode {
  return (
    <div className="min-w-0 @min-[42rem]:col-span-2" data-declared-section={part.model.name}>
      <EvalChart model={part.model} opensPanel={slug} renderer={renderer} />
    </div>
  );
}

/** One block of an eval's charts as they draw: a declared chart and its model (null until its
 *  readings land; a number's top alone), or a section of a number with its kind's renderer. */
type Block =
  | {
      readonly kind: "chart";
      readonly key: string;
      readonly model: ChartModel | null;
      readonly spec: DeclaredSpec;
    }
  | {
      readonly kind: "section";
      readonly key: string;
      readonly part: NumberSectionPart;
      readonly renderer: ChartRenderer;
    };

/**
 * The blocks of an eval's charts in the order they draw (spec R20, as the platform's page draws
 * them): each chart in the order its declaration lists it, but every `prose` chart after all the
 * others, in its own order, and each section of a number after the chart its `after` names, those
 * after one chart in the order their numbers and sections are declared; a section whose `after`
 * names no chart of the eval stands after its own number.
 */
function blocksOf(specs: readonly DeclaredSpec[], models: readonly (ChartModel | null)[]): Block[] {
  const charts = specs.map((spec, index) => {
    const model = models[index] ?? null;
    const renderer = kindOf(spec)?.renderer;
    const parts =
      renderer !== undefined && model !== null && isNumberModel(model) ? numberParts(model) : null;
    return {
      key: specText(spec.id) ?? String(index),
      model: parts?.top ?? model,
      prose: specText(spec.kind) === "prose",
      sections: (parts?.sections ?? []).flatMap((part) =>
        renderer === undefined
          ? []
          : [{ key: part.model.name, kind: "section" as const, part, renderer }]
      ),
      spec,
    };
  });
  const named = new Set(charts.map((chart) => chart.key));
  const after = new Map<string, Block[]>();
  for (const chart of charts) {
    for (const section of chart.sections) {
      const place = named.has(section.part.after) ? section.part.after : chart.key;
      after.set(place, [...(after.get(place) ?? []), section]);
    }
  }
  return [
    ...charts.filter((chart) => !chart.prose),
    ...charts.filter((chart) => chart.prose),
  ].flatMap((chart): Block[] => [
    { key: chart.key, kind: "chart", model: chart.model, spec: chart.spec },
    ...(after.get(chart.key) ?? []),
  ]);
}

/**
 * An eval that declares its own charts, drawn in place of the per-day line (dispatch://INFRA-2918/
 * artifact/plan-md, D7, D10): its answer above its charts, then each chart through `EvalChart`
 * with its kind's model and renderer, from the readings of one `items` view per series its charts
 * name, over the header's window and compare range, each section of a number after the chart it
 * names (`blocksOf`). Nothing of it is in the first paint: it is drawn once its evals view has
 * painted, and it subscribes its items views once its detail read, which holds its charts, has
 * answered. The detail is read at the version the evals view answers, so a register write that
 * moves the charts or the answer reads it again (D11a), the charts drawn so far kept on screen
 * until the new ones land.
 */
export function DeclaredEval({
  answer,
  item,
  scope,
}: {
  answer: EvalListResponse;
  item: EvalListItem;
  scope: EvalsScope;
}): ReactNode {
  const { slug, version } = item.definition;
  const detail = useQuery({
    ...evalDetailQuery(scope.project, slug, version),
    placeholderData: keepPreviousData,
    // A version's charts and answer never change (a write of either writes a new version), so
    // the read the eval's box made is this one's too.
    staleTime: Number.POSITIVE_INFINITY,
  });
  const specs = useMemo(() => declaredCharts(detail.data), [detail.data]);
  const reads = useMemo(() => {
    const byKey = new Map<string, ChartRead>();
    for (const spec of specs) {
      for (const read of kindOf(spec)?.reads(spec) ?? []) byKey.set(readKey(read), read);
    }
    return [...byKey.values()];
  }, [specs]);
  const readings = useReadings(scope, slug, reads);
  const answerText = detail.data?.items?.[0]?.definition.answer ?? item.definition.answer;
  const inputs = useMemo(
    () =>
      specs.map((spec): DeclaredInput | null => {
        const states = (kindOf(spec)?.reads(spec) ?? []).map((read) =>
          readings.states.get(readKey(read))
        );
        if (states.some((state) => state === undefined || !state.landed)) return null;
        return {
          compare: answer.compare,
          readings: states.flatMap((state) => state?.rows ?? []).sort(compareReadings),
          slug,
          window: answer.window,
        };
      }),
    [answer.compare, answer.window, readings.states, slug, specs]
  );
  const blocks = useMemo(
    () =>
      blocksOf(
        specs,
        specs.map((spec, index) => {
          const kind = kindOf(spec);
          const input = inputs[index] ?? null;
          return kind === undefined || input === null ? null : kind.model(spec, input);
        })
      ),
    [inputs, specs]
  );
  return (
    <div className="@container flex min-w-0 flex-col gap-2" data-declared={slug}>
      <span className={`text-xs ${textMutedOnSurface}`}>
        <EvalTitle slug={slug} title={evalName(item)} />
      </span>
      {answerText === "" ? null : (
        <p className={`text-sm ${textPrimaryOnSurface}`} data-answer>
          {answerText}
        </p>
      )}
      {detail.isPlaceholderData ? (
        <p className={`text-xs ${textMutedOnSurface}`} role="status">
          Reading the charts…
        </p>
      ) : null}
      {readings.error === null ? null : (
        <QueryError
          error={readings.error}
          message="Could not read the readings of this eval's charts."
          onRetry={readings.retry}
        />
      )}
      <div className="grid grid-cols-1 gap-3 @min-[42rem]:grid-cols-2">
        {blocks.map((block) =>
          block.kind === "chart" ? (
            <DeclaredChart key={block.key} model={block.model} slug={slug} spec={block.spec} />
          ) : (
            <DeclaredNumberSection
              key={block.key}
              part={block.part}
              renderer={block.renderer}
              slug={slug}
            />
          )
        )}
      </div>
    </div>
  );
}
