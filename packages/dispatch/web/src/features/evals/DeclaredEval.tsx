import {
  keepPreviousData,
  type UseQueryResult,
  useQueries,
  useQuery,
  useQueryClient,
} from "@tanstack/react-query";
import {
  type ReactNode,
  useCallback,
  useEffect,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
} from "react";

import { EVALS_VIEW_LIMIT, evalDetailQuery, itemsViewQuery } from "../../api/queries";
import type { EvalItemsResponse, EvalListItem, EvalListResponse } from "../../api/types";
import { QueryError } from "../../components/QueryError";
import { textMutedOnSurface, textPrimaryOnSurface } from "../../theme/classes";
import { type ChartModel, type ChartRenderer, evalName } from "./chart-model";
import {
  type ChartRead,
  chartTexts,
  type DeclaredInput,
  type DeclaredSpec,
  declaredCharts,
  specText,
} from "./charts/kind";
import { isNumberModel, type NumberSectionPart, numberParts } from "./charts/number-model";
import { type StepGroup, stepReads } from "./charts/plot";
import { compareReadings, type DeclaredReading, readingsOf } from "./charts/readings";
import { chartKinds } from "./charts/registry";
import { EvalChart } from "./EvalChart";
import { EvalTitle } from "./EvalPanel";
import { type EvalsScope, laterOffsets } from "./useEvalsViews";

/** The kind a declared chart names, where the tab draws it. */
function kindOf(spec: DeclaredSpec) {
  const name = specText(spec.kind) ?? "";
  return Object.hasOwn(chartKinds, name) ? chartKinds[name] : undefined;
}

/** A declared chart's key among its eval's charts: its id, else its place in the list. */
function chartKey(spec: DeclaredSpec, index: number): string {
  return specText(spec.id) ?? String(index);
}

/** The least and the most spans a bounded read takes, and the step its count is rounded up to
 *  (plan D21). */
const BUCKETS_MIN = 160;
const BUCKETS_MAX = 2_000;
const BUCKETS_STEP = 80;

/** The `buckets` a step chart drawn `width` CSS pixels wide reads its series at (plan D21): the
 *  width rounded up to the next multiple of 80, at least 160 and at most 2,000, so each span is at
 *  most one CSS pixel of its plot, which the chart's width holds. */
export function bucketsOf(width: number): number {
  return Math.min(
    BUCKETS_MAX,
    Math.max(BUCKETS_MIN, Math.ceil(width / BUCKETS_STEP) * BUCKETS_STEP)
  );
}

/** One items view an eval's charts read: a kind's read, and for a series a step chart draws, the
 *  group its bounded read takes (plan D21); null for a read of every reading, page by page. */
interface EvalRead {
  readonly read: ChartRead;
  readonly group: StepGroup | null;
}

/** The reads of `spec`: each its kind names, a series it draws by the step rule read bounded. */
function readsOf(spec: DeclaredSpec): EvalRead[] {
  const stepped = new Map(stepReads(spec).map(({ group, series }) => [series, group]));
  return (kindOf(spec)?.reads(spec) ?? []).map((read) => ({
    group: (read.series === undefined ? undefined : stepped.get(read.series)) ?? null,
    read,
  }));
}

/** A read's key among the eval's reads, so two charts that read one series alike share its view. */
function readKey({ group, read }: EvalRead): string {
  return `${read.series ?? ""}\n${read.rowKind ?? ""}\n${group ?? ""}`;
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
 * D10, D21). A series a step chart draws is read through one bounded items view, at the `buckets`
 * its widest chart's width names (`buckets`, by read; none until a chart that draws it has laid
 * out) and its chart's group, whatever the count of its readings; a new count replaces the view.
 * Every other read is one items view, each a page of EVALS_VIEW_LIMIT rows (the items route's
 * most, as the eval list's), and one view more at each next offset where its first page counts
 * more; it leaves out the readings before the window, which only a step line draws. Every
 * component that reads one view shares its one subscription, through its query key. No view names
 * the header's repositories: a logged eval's readings belong to no repository, and the items route
 * keeps only the readings whose `attributes.repo` a `repo` names, so one would drop them all.
 */
function useReadings(
  scope: EvalsScope,
  slug: string,
  reads: readonly EvalRead[],
  buckets: ReadonlyMap<string, number>
) {
  const client = useQueryClient();
  const view = (
    read: ChartRead,
    page: { limit: number; offset: number } | { buckets: number; group: StepGroup }
  ) =>
    itemsViewQuery({
      compare: scope.compare,
      project: scope.project,
      repos: [],
      rowKind: read.rowKind,
      series: read.series,
      slug,
      window: scope.window,
      ...page,
    });
  const paged = useMemo(() => reads.filter((read) => read.group === null), [reads]);
  const bounded = useMemo(
    () =>
      reads.flatMap((read) => {
        const count = buckets.get(readKey(read));
        return read.group === null || count === undefined
          ? []
          : [{ count, group: read.group, read }];
      }),
    [buckets, reads]
  );
  const firstViews = paged.map(({ read }) => view(read, { limit: EVALS_VIEW_LIMIT, offset: 0 }));
  const firsts = useQueries({ combine: pagesOf, queries: firstViews });
  const laterViews = paged.flatMap(({ read }, index) =>
    laterOffsets(firsts.pages[index]?.total ?? 0).map((offset) =>
      view(read, { limit: EVALS_VIEW_LIMIT, offset })
    )
  );
  const rest = useQueries({ combine: pagesOf, queries: laterViews });
  const boundedViews = bounded.map(({ count, group, read }) =>
    view(read.read, { buckets: count, group })
  );
  const bounds = useQueries({ combine: pagesOf, queries: boundedViews });
  // What each bounded read last drew, in the scope it was read in: while a resize's new view is
  // read, the chart draws it, so it neither blanks nor loses its reader's Graph or Table.
  const scopeKey = JSON.stringify([scope.project, slug, scope.window, scope.compare]);
  const lastDrawn = useRef(new Map<string, { scope: string; state: ReadState }>());
  const states = useMemo(() => {
    const held = new Map<string, ReadState>();
    // The later pages stand in `rest` read by read, each read's in the order of its offsets.
    let at = 0;
    paged.forEach((read, index) => {
      const first = firsts.pages[index];
      const pages = [first];
      for (const _offset of laterOffsets(first?.total ?? 0)) pages.push(rest.pages[at++]);
      held.set(readKey(read), {
        landed: pages.every((page) => page !== undefined),
        rows: readingsOf(pages.flatMap((page) => page?.items ?? [])).filter(
          (reading) => reading.beforeWindow !== true
        ),
      });
    });
    bounded.forEach(({ read }, index) => {
      const page = bounds.pages[index];
      const key = readKey(read);
      const last = lastDrawn.current.get(key);
      held.set(
        key,
        page !== undefined
          ? { landed: true, rows: readingsOf(page.items ?? []) }
          : last?.scope === scopeKey
            ? last.state
            : { landed: false, rows: [] }
      );
    });
    return held;
  }, [bounded, bounds.pages, firsts.pages, paged, rest.pages, scopeKey]);
  useEffect(() => {
    for (const { read } of bounded) {
      const state = states.get(readKey(read));
      if (state?.landed) lastDrawn.current.set(readKey(read), { scope: scopeKey, state });
    }
  }, [bounded, scopeKey, states]);
  return {
    error: firsts.error ?? rest.error ?? bounds.error,
    retry: () => {
      const refused = [
        ...firstViews.filter((_, index) => firsts.failed[index]),
        ...laterViews.filter((_, index) => rest.failed[index]),
        ...boundedViews.filter((_, index) => bounds.failed[index]),
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
 *  chart and its kind. A step chart measures the width it is drawn at once it lays out, and again
 *  each time that changes, and names the `buckets` it reads its series at (`measure`, plan D21). */
function DeclaredChart({
  chart,
  measure,
  model,
  slug,
  spec,
}: {
  /** Its key among the eval's charts (`chartKey`). */
  chart: string;
  measure: (chart: string, buckets: number) => void;
  model: ChartModel | null;
  slug: string;
  spec: DeclaredSpec;
}): ReactNode {
  const kind = kindOf(spec);
  const title = specText(spec.title) ?? specText(spec.id) ?? "";
  const texts = chartTexts(spec);
  const id = specText(spec.id) ?? "";
  const kindName = specText(spec.kind) ?? "";
  const stepped = stepReads(spec).length > 0;
  const section = useRef<HTMLElement>(null);
  useLayoutEffect(() => {
    const element = section.current;
    if (!stepped || element === null) return;
    const read = () => measure(chart, bucketsOf(element.getBoundingClientRect().width));
    read();
    if (typeof ResizeObserver === "undefined") return;
    const sizes = new ResizeObserver(read);
    sizes.observe(element);
    return () => sizes.disconnect();
  }, [chart, measure, stepped]);
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
      ref={section}
    >
      {texts.heading === null ? null : (
        // Its definition's words, drawn as plain text: `eval-chart-heading`
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
      key: chartKey(spec, index),
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
 * names (`blocksOf`). A series a step chart draws is read through one bounded view at the chart's
 * drawn width (D21), opened once the chart has laid out and replaced when a resize changes its
 * `buckets`. Nothing of it is in the first paint: it is drawn once its evals view has painted, and
 * it subscribes its items views once its detail read, which holds its charts, has answered. The
 * detail is read at the version the evals view answers, so a register write that moves the charts
 * or the answer reads it again (D11a), the charts drawn so far kept on screen until the new ones
 * land.
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
  // The buckets each step chart reads at, by its key, once it has laid out.
  const [measured, setMeasured] = useState<ReadonlyMap<string, number>>(() => new Map());
  const measure = useCallback(
    (chart: string, buckets: number) =>
      setMeasured((held) =>
        held.get(chart) === buckets ? held : new Map(held).set(chart, buckets)
      ),
    []
  );
  const reads = useMemo(() => {
    const byKey = new Map<string, EvalRead>();
    for (const spec of specs) for (const read of readsOf(spec)) byKey.set(readKey(read), read);
    return [...byKey.values()];
  }, [specs]);
  // Each bounded read's buckets: of the step charts that draw its series, the widest's.
  const buckets = useMemo(() => {
    const widest = new Map<string, number>();
    specs.forEach((spec, index) => {
      const count = measured.get(chartKey(spec, index));
      if (count === undefined) return;
      for (const read of readsOf(spec)) {
        const key = readKey(read);
        if (read.group !== null) widest.set(key, Math.max(widest.get(key) ?? 0, count));
      }
    });
    return widest;
  }, [measured, specs]);
  const readings = useReadings(scope, slug, reads, buckets);
  const answerText = detail.data?.items?.[0]?.definition.answer ?? item.definition.answer;
  const inputs = useMemo(
    () =>
      specs.map((spec): DeclaredInput | null => {
        const states = readsOf(spec).map((read) => readings.states.get(readKey(read)));
        if (states.some((state) => state === undefined || !state.landed)) return null;
        return {
          compare: answer.compare,
          now: Date.now(),
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
            <DeclaredChart
              chart={block.key}
              key={block.key}
              measure={measure}
              model={block.model}
              slug={slug}
              spec={block.spec}
            />
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
