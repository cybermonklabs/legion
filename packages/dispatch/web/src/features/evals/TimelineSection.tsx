import { useQuery, useQueryClient } from "@tanstack/react-query";
import type { ColumnDef } from "@tanstack/react-table";
import { type ReactNode, useEffect, useId, useLayoutEffect, useMemo, useState } from "react";

import {
  growProjectTimelineView,
  PROJECT_TIMELINE_PAGE,
  type ProjectTimelineViewParams,
  projectTimelineViewQuery,
} from "../../api/queries";
import type {
  DeliverySessionGroup,
  ProjectTimelineMergeRow,
  ProjectTimelineRow,
  ProjectTimelineRunRow,
} from "../../api/types";
import { MultiSelect } from "../../components/MultiSelect";
import { QueryError } from "../../components/QueryError";
import {
  borderStrong,
  dragHandleBg,
  linkText,
  secondaryButtonCompact,
  surfaceBg,
  surfaceMutedBg,
  switchOffBg,
  switchOnBg,
  switchThumbBg,
  textMutedOnCanvas,
  textMutedOnSurface,
  textPrimaryOnCanvas,
  textPrimaryOnSurface,
  textPrimaryOnSurfaceMuted,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { DrillDown } from "../delivery/DrillDown";
import { FacetPanel } from "../delivery/FacetPanel";
import { buildColorScale, COLOR_COUNT_KEYS, COLOR_FACET_OPTIONS } from "../delivery/lib/colorScale";
import {
  DEPLOYED_LABELS,
  NO_ISSUE,
  PLACEHOLDER_LABELS,
  shortRepoLabel,
} from "../delivery/lib/facets";
import { parentAgentLabel } from "../delivery/lib/parentAgent";
import { PRList } from "../delivery/PRList";
import { Timeline, type TimelineSelection } from "../delivery/Timeline";
import { type ChartMode, ChartModeToggle } from "./EvalChart";
import { EvalTable } from "./EvalTable";
import type { BrushWindow } from "./evals-url";
import {
  type TimelineMode,
  type TimelineView,
  timelineQuery,
  useSettledSearch,
  useTimelineUrlState,
} from "./timeline-url";
import type { EvalsScope } from "./useEvalsViews";

/** No Parent agent value chosen: one array, so a render that chooses none hands the colour scale
 *  the same dependency. */
const NO_AGENTS: readonly string[] = [];
const NO_ROWS: ProjectTimelineRow[] = [];

/** Where the section stands: closed (no view, no row), subscribed until its first snapshot,
 *  painted, growing while a growth's rows are out, or failed while its view is refused. */
type TimelineState = "closed" | "subscribed" | "painted" | "growing" | "failed";

const KIND_LABELS: Readonly<Record<ProjectTimelineRow["kind"], string>> = {
  merge: "Merge",
  deploy: "Deploy",
  failed_run: "Failed run",
};

const dateTime = new Intl.DateTimeFormat(undefined, {
  month: "short",
  day: "numeric",
  hour: "numeric",
  minute: "2-digit",
});

function isMerge(row: ProjectTimelineRow): row is ProjectTimelineMergeRow {
  return row.kind === "merge";
}

function isRun(row: ProjectTimelineRow): row is ProjectTimelineRunRow {
  return row.kind !== "merge";
}

/** A row's time, as the timeline orders it: a merge's merge, a run's creation. */
function rowTime(row: ProjectTimelineRow): string | null {
  return isMerge(row) ? row.merged_at : row.created_at;
}

/** The drill-down a row opens: its pull request's, its deploy's, or its failed run's. */
function selectionOf(row: ProjectTimelineRow): TimelineSelection {
  if (isMerge(row)) return { kind: "pr", id: row.id };
  return { kind: row.kind === "deploy" ? "deploy" : "failure", id: row.id };
}

/** The table mode's columns: one row per merge, deploy or failed run, each its time, kind, name
 *  (the pull request, or the run) linking to it, what it is, its repository, a merge's author and
 *  issue, and where it stands. */
function timelineColumns(allRepos: readonly string[]): ColumnDef<ProjectTimelineRow>[] {
  return [
    {
      accessorFn: (row) => {
        const at = rowTime(row);
        return at === null ? undefined : Date.parse(at);
      },
      cell: ({ getValue }) => {
        const at = getValue<number | undefined>();
        return at === undefined ? null : (
          <span title={new Date(at).toLocaleString()}>{dateTime.format(at)}</span>
        );
      },
      enableColumnFilter: false,
      header: "Time",
      id: "at",
      size: 128,
      sortUndefined: "last",
    },
    {
      accessorFn: (row) => KIND_LABELS[row.kind],
      enableColumnFilter: false,
      header: "Kind",
      id: "kind",
      size: 96,
    },
    {
      accessorFn: (row) =>
        isMerge(row) ? `${shortRepoLabel(allRepos, row.repo)}#${row.number}` : `Run #${row.id}`,
      cell: ({ getValue, row }) => (
        <a
          className={`hover:underline ${linkText}`}
          href={row.original.url}
          onClick={(event) => event.stopPropagation()}
          rel="noreferrer"
          target="_blank"
        >
          {getValue<string>()}
        </a>
      ),
      enableColumnFilter: false,
      header: "Name",
      id: "name",
      size: 120,
    },
    {
      accessorFn: (row) => {
        if (isMerge(row)) return row.title;
        if (row.kind === "deploy") return `Shipped ${(row.prs ?? []).length} PR(s)`;
        return `Failed: ${row.root_failing_job?.name ?? row.production?.conclusion ?? "failure"}`;
      },
      cell: ({ getValue }) => <span title={getValue<string>()}>{getValue<string>()}</span>,
      enableColumnFilter: false,
      header: "Title",
      id: "title",
      size: 320,
    },
    {
      accessorFn: (row) => row.repo,
      enableColumnFilter: false,
      header: "Repository",
      id: "repo",
      size: 160,
    },
    {
      accessorFn: (row) => (isMerge(row) ? row.author : undefined),
      enableColumnFilter: false,
      header: "Author",
      id: "author",
      size: 120,
      sortUndefined: "last",
    },
    {
      accessorFn: (row) => (isMerge(row) ? (row.issue ?? undefined) : undefined),
      cell: ({ row }) =>
        isMerge(row.original) ? (
          <span className={row.original.issue === null ? textMutedOnSurface : ""}>
            {row.original.issue ?? PLACEHOLDER_LABELS[NO_ISSUE]}
          </span>
        ) : null,
      enableColumnFilter: false,
      header: "Issue",
      id: "issue",
      size: 112,
      sortUndefined: "last",
    },
    {
      accessorFn: (row) => {
        if (isMerge(row)) return DEPLOYED_LABELS[row.deployed_status] ?? row.deployed_status;
        if (row.kind === "failed_run") return "failed";
        return row.production?.conclusion === "success"
          ? "deployed"
          : (row.conclusion ?? "running");
      },
      enableColumnFilter: false,
      header: "Status",
      id: "status",
      size: 104,
    },
  ];
}

/** The Timeline | List segmented toggle: the chart, or the merged pull request list. */
function ViewToggle({
  mode,
  onChange,
}: {
  mode: TimelineMode;
  onChange: (mode: TimelineMode) => void;
}): ReactNode {
  const item = (value: TimelineMode, label: string) => (
    <button
      aria-pressed={mode === value}
      className={`h-8 px-3 text-sm font-medium first:rounded-l-md last:rounded-r-md ${
        mode === value ? `${surfaceMutedBg} ${textPrimaryOnSurfaceMuted}` : textSecondaryOnCanvas
      }`}
      onClick={() => onChange(value)}
      type="button"
    >
      {label}
    </button>
  );
  return (
    <fieldset aria-label="View" className={`mr-4 flex rounded-md border ${borderStrong}`}>
      {item("timeline", "Timeline")}
      <span aria-hidden="true" className={`w-px ${dragHandleBg}`} />
      {item("list", "List")}
    </fieldset>
  );
}

/** The Swimlanes switch. */
function LanesSwitch({
  checked,
  onChange,
}: {
  checked: boolean;
  onChange: (checked: boolean) => void;
}): ReactNode {
  return (
    <label className="ml-4 flex cursor-pointer items-center gap-2">
      <button
        aria-checked={checked}
        className={`inline-flex h-5 w-9 shrink-0 items-center rounded-full transition-colors ${checked ? switchOnBg : switchOffBg}`}
        onClick={() => onChange(!checked)}
        role="switch"
        type="button"
      >
        <span
          className={`block h-4 w-4 rounded-full shadow-lg transition-transform ${switchThumbBg} ${checked ? "translate-x-4.5" : "translate-x-0.5"}`}
        />
      </button>
      <span className={textPrimaryOnCanvas}>Swimlanes</span>
    </label>
  );
}

/** Coloured by Parent agent: which values get their own colour and lane, busiest first. The rest
 *  share one Other colour and lane; with none chosen, the colours and lanes are the facet's usual. */
function OwnColourPicker({
  counts,
  groups,
  selected,
  onChange,
}: {
  counts: Readonly<Record<string, number>>;
  groups: readonly DeliverySessionGroup[];
  selected: readonly string[];
  onChange: (next: string[]) => void;
}): ReactNode {
  const [open, setOpen] = useState(false);
  const options = Object.keys(counts)
    .filter((value) => !(value in PLACEHOLDER_LABELS))
    .sort((a, b) => (counts[b] ?? 0) - (counts[a] ?? 0));
  const state = selected.length === 0 ? "every value" : `${selected.length} chosen`;
  const countOf = (value: string) => counts[value] ?? 0;
  return (
    <MultiSelect
      emptyMessage="No matches."
      label="Own colour and lane"
      onChange={onChange}
      onOpenChange={setOpen}
      open={open}
      optionDetail={(value) => String(countOf(value))}
      optionDetailLabel={(value) => `${countOf(value)} PR${countOf(value) === 1 ? "" : "s"}`}
      optionLabel={(value) => parentAgentLabel(value, groups)}
      options={[...new Set([...selected, ...options])]}
      searchLabel="Search parent agents…"
      selected={selected}
      triggerAriaLabel={`Own colour and lane: ${state}`}
      triggerClassName={`flex h-8 items-center rounded border px-2 text-sm ${borderStrong} ${surfaceBg} ${textPrimaryOnSurface}`}
    >
      <span className="truncate">Own colour: {state}</span>
    </MultiSelect>
  );
}

/**
 * The open merge timeline (plan D8): the project-timeline view of the header's window, its facets
 * and its search, holding the PROJECT_TIMELINE_PAGE newest rows when it opens. Once the reader
 * reaches the end of the rows it holds, the one view grows by PROJECT_TIMELINE_PAGE rows, up to
 * the window's total, and Dispatch sends only the rows it lacked, so the rows on screen stay. The
 * facets as the Delivery page drew them (`FacetPanel`, decision 2), colour-by, the lanes, the brush
 * and the drill-down work on the rows it holds; the chart's table mode holds one row per merge,
 * deploy or failed run, and the list the merged pull requests. The timeline takes no compare
 * range (spec R7).
 */
function OpenTimeline({
  brush,
  onBrush,
  onState,
  project,
  scope,
}: {
  brush: BrushWindow | null;
  onBrush: (brush: BrushWindow | null) => void;
  onState: (state: TimelineState) => void;
  project: string;
  scope: EvalsScope;
}): ReactNode {
  const queryClient = useQueryClient();
  const titleId = useId();
  const [url, setUrl] = useTimelineUrlState();
  const [selection, setSelection] = useState<TimelineSelection | null>(null);
  const [chartMode, setChartMode] = useState<ChartMode>("graph");
  const search = useSettledSearch(url.filters);
  const filterQuery = timelineQuery(url.filters, search);
  const params = useMemo(
    (): ProjectTimelineViewParams => ({ project, query: filterQuery, window: scope.window }),
    [project, filterQuery, scope.window]
  );
  const query = useQuery(projectTimelineViewQuery(params));
  const data = query.data;
  // The view of `params` as Dispatch last answered it; a previous view's rows, kept on screen
  // while its snapshot is out, are not it.
  const answered = query.isPlaceholderData ? undefined : data;

  // The limit last asked of the view of `params` (`viewKey`): the view is growing while Dispatch
  // has not yet answered it.
  const viewKey = JSON.stringify(params);
  const [asked, setAsked] = useState<{ readonly key: string; readonly limit: number } | null>(null);
  const growing =
    answered !== undefined &&
    asked !== null &&
    asked.key === viewKey &&
    asked.limit > answered.limit;
  // Whether the end of the rows held is on screen.
  const [end, setEnd] = useState<HTMLElement | null>(null);
  const [atEnd, setAtEnd] = useState(false);
  useEffect(() => {
    // happy-dom has no IntersectionObserver; a test that scrolls installs one.
    if (end === null || typeof IntersectionObserver === "undefined") return;
    const observer = new IntersectionObserver((entries) => {
      for (const entry of entries) setAtEnd(entry.isIntersecting);
    });
    observer.observe(end);
    return () => observer.disconnect();
  }, [end]);
  useEffect(() => {
    if (!atEnd || answered === undefined || growing || answered.limit >= answered.total) return;
    const limit = Math.min(answered.limit + PROJECT_TIMELINE_PAGE, answered.total);
    if (growProjectTimelineView(queryClient, params, limit)) setAsked({ key: viewKey, limit });
  }, [atEnd, answered, growing, params, queryClient, viewKey]);

  const state: TimelineState = query.isError
    ? "failed"
    : answered === undefined
      ? "subscribed"
      : growing
        ? "growing"
        : "painted";
  useLayoutEffect(() => onState(state), [onState, state]);

  const items = data?.items ?? NO_ROWS;
  // The rows the brush spans, every row the view holds without one.
  const shown = useMemo(() => {
    if (brush === null) return items;
    const startMs = Date.parse(brush.start);
    const endMs = Date.parse(brush.end);
    return items.filter((row) => {
      const at = rowTime(row);
      if (at === null) return false;
      const ms = Date.parse(at);
      return ms >= startMs && ms <= endMs;
    });
  }, [items, brush]);
  const prs = useMemo(() => items.filter(isMerge), [items]);
  const runs = useMemo(() => items.filter(isRun), [items]);
  const shownPRs = useMemo(() => shown.filter(isMerge), [shown]);
  // The waiting line's population, whatever the brush: the pull requests still waiting at the
  // window's start, which Dispatch sends once the view reaches it, and every tracked merge held.
  const waitingMerges = useMemo(
    () => [...(data?.waiting ?? []), ...prs.filter((pr) => pr.deployed_status !== "not_tracked")],
    [data, prs]
  );
  const chosenAgents = url.view.colorBy === "parentAgent" ? url.view.agents : NO_AGENTS;
  const colorScale = useMemo(
    () =>
      buildColorScale(data?.color_counts?.[COLOR_COUNT_KEYS[url.view.colorBy]] ?? {}, chosenAgents),
    [data, url.view.colorBy, chosenAgents]
  );
  const allRepos = useMemo(() => Object.keys(data?.color_counts?.repo ?? {}), [data]);
  const columns = useMemo(() => timelineColumns(allRepos), [allRepos]);
  const drillDownSelection = useMemo((): TimelineSelection | null => {
    if (selection === null || selection.kind !== "pr") return selection;
    return prs.some((pr) => pr.id === selection.id) ? selection : null;
  }, [selection, prs]);
  const setView = (view: TimelineView) => setUrl({ ...url, view });
  // A refused view is subscribed again from its newest rows, so no growth is left waiting.
  const retry = () => {
    setAsked(null);
    void query.refetch();
  };

  if (data === undefined) {
    return query.isError ? (
      <QueryError error={query.error} message="Merge timeline: could not load." onRetry={retry} />
    ) : (
      <p className={`text-sm ${textMutedOnCanvas}`} role="status">
        Merge timeline: loading…
      </p>
    );
  }
  const span = brush ?? { start: data.window.from, end: data.window.to };
  return (
    <div className="flex flex-col gap-3">
      {query.isError ? (
        <QueryError error={query.error} message="Merge timeline: could not load." onRetry={retry} />
      ) : null}
      <div className="flex flex-wrap items-center gap-2 text-sm">
        <ViewToggle mode={url.view.mode} onChange={(mode) => setView({ ...url.view, mode })} />
        <label className={`flex items-center gap-2 ${textMutedOnCanvas}`}>
          Color merges by
          <select
            className={`rounded border px-2 py-1 ${surfaceMutedBg} ${borderStrong} ${textPrimaryOnCanvas}`}
            onChange={(event) => {
              const colorBy = COLOR_FACET_OPTIONS.find(
                (option) => option.value === event.target.value
              )?.value;
              if (colorBy !== undefined) setView({ ...url.view, colorBy });
            }}
            value={url.view.colorBy}
          >
            {COLOR_FACET_OPTIONS.map((option) => (
              <option key={option.value} value={option.value}>
                {option.label}
              </option>
            ))}
          </select>
        </label>
        {url.view.colorBy === "parentAgent" ? (
          <OwnColourPicker
            counts={data.color_counts?.parent_agent ?? {}}
            groups={data.session_groups ?? []}
            onChange={(agents) => setView({ ...url.view, agents })}
            selected={url.view.agents}
          />
        ) : null}
        {url.view.mode === "timeline" ? (
          <LanesSwitch
            checked={url.view.lanes}
            onChange={(lanes) => setView({ ...url.view, lanes })}
          />
        ) : null}
        <span className={`ml-4 ${textMutedOnCanvas}`}>
          {shownPRs.length} PRs in current filter/window
        </span>
      </div>
      <div className="flex min-w-0 flex-col gap-4 xl:flex-row">
        <div className="shrink-0 xl:w-56">
          <FacetPanel
            data={data}
            filters={url.filters}
            onChange={(filters) => setUrl({ ...url, filters })}
          />
        </div>
        <div className="flex min-w-0 flex-1 flex-col gap-2">
          {url.view.mode === "timeline" ? (
            <figure
              aria-labelledby={titleId}
              className="m-0 flex min-w-0 flex-col gap-1"
              data-chart="timeline"
              data-chart-kind="timeline"
            >
              <div className="flex flex-wrap items-center gap-2">
                <figcaption className={`mr-auto text-xs ${textMutedOnSurface}`} id={titleId}>
                  Merges, deploys and failed runs
                </figcaption>
                <ChartModeToggle mode={chartMode} onChange={setChartMode} />
              </div>
              {chartMode === "graph" ? (
                <div className="flex h-[75dvh] min-h-[28rem] min-w-0 flex-col">
                  <Timeline
                    chosen={chosenAgents}
                    colorBy={url.view.colorBy}
                    colorScale={colorScale}
                    components={data.components ?? {}}
                    filters={url.filters}
                    lanes={url.view.lanes}
                    onBrush={onBrush}
                    onSelect={setSelection}
                    prs={shownPRs}
                    runs={runs}
                    sessionGroups={data.session_groups ?? []}
                    waiting={waitingMerges}
                    window={span}
                  />
                </div>
              ) : (
                <EvalTable
                  cells="text"
                  columns={columns}
                  empty="No rows in the current filter/window."
                  getRowId={(row) => String(row.id)}
                  label="Merge timeline rows"
                  layoutKey="evals-timeline"
                  noun={{ many: "rows", one: "row" }}
                  rows={shown}
                  scrollTestId="timeline-table-scroll"
                  select={{
                    onSelect: (row) => setSelection(selectionOf(row)),
                    selectedId: selection === null ? undefined : String(selection.id),
                  }}
                />
              )}
            </figure>
          ) : (
            <PRList
              allRepos={allRepos}
              colorBy={url.view.colorBy}
              colorScale={colorScale}
              onSelect={(id) => setSelection({ kind: "pr", id })}
              onSelectRun={(id) => setSelection({ kind: "deploy", id })}
              prs={shownPRs}
              selectedId={selection?.kind === "pr" ? selection.id : undefined}
              sessionGroups={data.session_groups ?? []}
            />
          )}
          <p
            className={`text-xs ${textMutedOnCanvas}`}
            data-timeline-end
            ref={setEnd}
            role="status"
          >
            {data.total === 0
              ? "No merge, deploy or failed run in the window."
              : items.length >= data.total
                ? `All ${data.total} rows of the window loaded.`
                : `The newest ${items.length} of ${data.total} rows loaded${growing ? "; loading older rows…" : "; older rows load as you scroll."}`}
          </p>
        </div>
        <DrillDown
          components={data.components ?? {}}
          onClose={() => setSelection(null)}
          project={project}
          prs={prs}
          runs={runs}
          selection={drillDownSelection}
          sessionGroups={data.session_groups ?? []}
          sessionTitles={data.session_titles ?? {}}
        />
      </div>
    </div>
  );
}

/**
 * The tab's merge timeline section (dispatch://INFRA-2918/spec R13, R6, R7; plan D8). Closed on
 * the first paint: it subscribes nothing and holds no row until the reader opens it
 * (`OpenTimeline`), and closing it lets its view go. While the brush is set, the section says so
 * and clears it, whether open or closed, since the brush sets the target cards' days too.
 */
export function TimelineSection({
  brush,
  onBrush,
  project,
  scope,
}: {
  /** The brush the URL holds, which the target cards read too. */
  brush: BrushWindow | null;
  onBrush: (brush: BrushWindow | null) => void;
  project: string;
  scope: EvalsScope;
}): ReactNode {
  const headingId = useId();
  const [open, setOpen] = useState(false);
  const [state, setState] = useState<TimelineState>("subscribed");
  return (
    <section
      aria-labelledby={headingId}
      className="flex flex-col gap-2"
      data-section="timeline"
      data-state={open ? state : "closed"}
    >
      <div className="flex flex-wrap items-center gap-3">
        <h2 className={`text-[15px] font-semibold ${textPrimaryOnCanvas}`} id={headingId}>
          Merge timeline
        </h2>
        <button
          aria-expanded={open}
          className={secondaryButtonCompact}
          onClick={() => {
            setState("subscribed");
            setOpen(!open);
          }}
          type="button"
        >
          {open ? "Hide the merge timeline" : "Show the merge timeline"}
        </button>
        {brush === null ? null : (
          <span className={`text-xs ${textMutedOnCanvas}`}>
            {`Brushed ${new Date(brush.start).toLocaleString()} to ${new Date(brush.end).toLocaleString()}`}
            <button
              className={`ml-3 underline ${linkText}`}
              onClick={() => onBrush(null)}
              type="button"
            >
              clear brush window
            </button>
          </span>
        )}
      </div>
      {open ? (
        <OpenTimeline
          brush={brush}
          onBrush={onBrush}
          onState={setState}
          project={project}
          scope={scope}
        />
      ) : null}
    </section>
  );
}
