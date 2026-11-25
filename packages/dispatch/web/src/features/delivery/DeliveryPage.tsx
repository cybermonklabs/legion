import { useQuery } from "@tanstack/react-query";
import type { ColumnFiltersState, OnChangeFn, SortingState } from "@tanstack/react-table";
import { type ReactNode, useCallback, useEffect, useMemo, useRef, useState } from "react";

import { isDeliveryNotConfigured } from "../../api/client";
import { queryString } from "../../api/paths";
import { deliveryMeasuresQuery, deliveryTimelineQuery } from "../../api/queries";
import type { DeliverySessionGroup, DeliveryTimelineWithMeasures } from "../../api/types";
import { ControlRow } from "../../components/ControlRow";
import type { FilterChip } from "../../components/filter-chips";
import { ARM_ON_PRESS, useFocusPlace } from "../../components/focus-place";
import { MultiSelect } from "../../components/MultiSelect";
import { QueryError } from "../../components/QueryError";
import { useNestedPanels } from "../../components/useNestedPanels";
import { useSettled } from "../../hooks/useSettled";
import {
  textParam,
  useAddressParams,
  useAddressUpdate,
  useRepeatableAddressParams,
} from "../../lib/page-address";
import {
  borderStrong,
  dragHandleBg,
  linkText,
  surfaceBg,
  surfaceMutedBg,
  switchOffBg,
  switchOnBg,
  switchThumbBg,
  textMutedOnCanvas,
  textPrimaryOnCanvas,
  textPrimaryOnSurface,
  textPrimaryOnSurfaceMuted,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { useDocumentTitle } from "../shell/useDocumentTitle";
import { DeliverySettingsForm } from "./DeliverySettingsForm";
import { DrillDown } from "./DrillDown";
import {
  buildColorScale,
  COLOR_COUNT_KEYS,
  COLOR_FACET_OPTIONS,
  type ColorFacet,
} from "./lib/colorScale";
import {
  FACET_KEYS,
  FACET_PARAMS,
  type FacetKey,
  type Filters,
  ownEntry,
  PLACEHOLDER_LABELS,
} from "./lib/facets";
import { parentAgentLabel } from "./lib/parentAgent";
import {
  DEFAULT_WINDOW_PRESET,
  hasMalformedWindowParams,
  liveWindow,
  readBrush,
  readWindow,
  type WindowChoice,
} from "./lib/window";
import { MeasuresPanel } from "./MeasuresPanel";
import {
  COLUMN_FACETS,
  type ColumnFacet,
  columnFacetValue,
  columnHeader,
  FIXED_FACET_VALUES,
  isColumnFacet,
  PRList,
  SORTABLE_COLUMNS,
} from "./PRList";
import { SessionGroupsEditor } from "./SessionGroupsEditor";
import { SourceFreshness } from "./SourceFreshness";
import { Timeline, type TimelineSelection } from "./Timeline";
import { WINDOW_TRIGGER_SELECTOR, WindowControl } from "./WindowControl";

/** A facet the page's one filter panel holds in both views: the four that are no column of the
 *  list. */
type PanelFacet = Exclude<FacetKey, ColumnFacet>;

/** How long the search waits after the last keystroke before the server is asked again. */
const SEARCH_DEBOUNCE_MS = 250;

const COLOR_FACETS = COLOR_FACET_OPTIONS.map((option) => option.value);

/** The colour-by facet the page uses when its URL names none, as the prototype does. */
const DEFAULT_COLOR_BY: ColorFacet = "repo";

/** No Parent agent value chosen: one array, so a render that chooses none hands the colour scale
 *  the same dependency. */
const NO_AGENTS: readonly string[] = [];

type Mode = "timeline" | "list";

/** How the filtered PRs are shown: dots on the timeline or a sortable list, the facet the merges
 *  are coloured by, whether the timeline splits them into one column per value, and, coloured by
 *  Parent agent, the values chosen to have their own colour and lane (`agents`). */
interface DeliveryView {
  colorBy: ColorFacet;
  lanes: boolean;
  mode: Mode;
  agents: string[];
}

/** The repeatable URL parameter that names the Parent agent values given their own colour and
 *  lane, kept apart from the facet filters, which narrow the population. */
const AGENTS_PARAM = ["agents"] as const;

/** A sub-window of the read window a brush drag set (`ws`/`we`): it narrows what the chart, the
 *  list and the count show without a new read, so the facets keep counting the whole window. */
export interface BrushWindow {
  start: string;
  end: string;
}

interface DeliveryUrlState {
  /** The window the URL names: a rolling preset, whose end Dispatch keeps at the present, or a
   *  custom range. The range a preset resolved to is the answer's `window`. */
  window: WindowChoice;
  filters: Filters;
  /** What the list is ordered by, as the table holds it. */
  sorting: SortingState;
  view: DeliveryView;
  brush: BrushWindow | null;
}

/** The order the list opens in, newest merge first, which is left out of the address: `?sort=`
 *  is absent until the reader presses a header, and goes again when they come back to this. */
const DEFAULT_SORT: SortingState = [{ id: "merged", desc: true }];

/** `?sort=` as the shared grammar spells one: the sorted columns in order, `-` for descending
 *  (`lib/page-address.ts`'s `tableAddressGrammar`). This page writes it beside its own facet
 *  parameters rather than taking that whole grammar, which would rename them. */
const SORT_PARAM = textParam();

/** `?sort=`'s text as the sorted columns it names, leaving out every id that is no sortable
 *  column of the list (`SORTABLE_COLUMNS`), which the table could not draw a sort on; the page's
 *  opening order while it names none of them. Takes the text rather than the parameters so a
 *  caller can memoize on it: the object the router hands out is new on every write of the
 *  address, the text only on a new sort. */
function readSorting(sorted: string): SortingState {
  const columns = sorted
    .split(",")
    .map((part) => ({ id: part.replace(/^-/, ""), desc: part.startsWith("-") }))
    .filter(({ id }) => SORTABLE_COLUMNS.includes(id));
  return columns.length === 0 ? DEFAULT_SORT : columns;
}

function sortingText(sorting: SortingState): string {
  return sorting.map(({ id, desc }) => `${desc ? "-" : ""}${id}`).join(",");
}

/** The delivery page's full state, read from and written to the address — the same pattern
 *  `features/project/issue-filters.ts`'s `useIssueFilters` uses: repeatable facet params through
 *  the shared `useRepeatableAddressParams`, one `update` (`useAddressUpdate`) funnelling every
 *  write through one `replace`d write of the address. `window` and `filters` are memoized on the
 *  address's own identity, so a render with an unchanged address builds the same live view. */
function useDeliveryUrlState(): [DeliveryUrlState, (next: DeliveryUrlState) => void] {
  const searchParams = useAddressParams();
  const update = useAddressUpdate();
  const malformedWindow = hasMalformedWindowParams(searchParams);
  const windowChoice = useMemo(() => readWindow(searchParams), [searchParams]);
  useEffect(() => {
    if (!malformedWindow) return;
    update((params) => {
      params.delete("from");
      params.delete("to");
      params.delete("window");
      params.delete("ws");
      params.delete("we");
      return params;
    });
  }, [malformedWindow, update]);
  const search = searchParams.get("q") ?? "";
  const colorByParam = searchParams.get("colorBy");
  const colorBy = COLOR_FACETS.find((facet) => facet === colorByParam) ?? DEFAULT_COLOR_BY;
  const lanes = searchParams.get("lanes") === "1";
  const mode: Mode = searchParams.get("mode") === "list" ? "list" : "timeline";
  const { agents: agentParams } = useRepeatableAddressParams(AGENTS_PARAM);
  const agents = useMemo(() => [...new Set(agentParams)], [agentParams]);
  const view = useMemo(() => ({ colorBy, lanes, mode, agents }), [colorBy, lanes, mode, agents]);
  const ws = searchParams.get("ws");
  const we = searchParams.get("we");
  const brush = useMemo(() => readBrush(ws, we), [ws, we]);
  // A pair that names no brush (`readBrush`) leaves the address as the malformed window does, so
  // the page shows its whole window and a reload or a shared link does not carry the pair on.
  const malformedBrush = (ws !== null || we !== null) && brush === null;
  useEffect(() => {
    if (!malformedBrush) return;
    update((params) => {
      params.delete("ws");
      params.delete("we");
      return params;
    });
  }, [malformedBrush, update]);
  // On the `?sort=` text, not on the parameters: the search box writes `?q=` on every keystroke
  // and hands out a new `URLSearchParams` each time, while TanStack compares its sorted row
  // model's dependencies by reference and would re-sort the whole window per letter typed.
  const sortText = SORT_PARAM.read(searchParams, "sort");
  const sorting = useMemo(() => readSorting(sortText), [sortText]);
  const arrayParams = useRepeatableAddressParams(FACET_KEYS);
  // Every facet keeps every value the address names, one that names no value of the facet
  // included: Dispatch selects no row by it, so `?priority=P9` alone lists none and beside a known
  // value lists what the known one does, and the page draws it as a chip the reader can take off.
  const filters: Filters = useMemo(() => ({ ...arrayParams, search }), [arrayParams, search]);

  const setState = useCallback(
    (next: DeliveryUrlState) => {
      update((params) => {
        if (next.window.kind === "custom") {
          params.set("from", next.window.from);
          params.set("to", next.window.to);
          params.delete("window");
        } else {
          params.delete("from");
          params.delete("to");
          if (next.window.preset === DEFAULT_WINDOW_PRESET) params.delete("window");
          else params.set("window", next.window.preset);
        }
        for (const key of FACET_KEYS) {
          params.delete(key);
          for (const value of next.filters[key]) params.append(key, value);
        }
        const sorted = sortingText(next.sorting);
        SORT_PARAM.write(params, "sort", sorted === sortingText(DEFAULT_SORT) ? "" : sorted);
        if (next.filters.search.trim() === "") params.delete("q");
        else params.set("q", next.filters.search);
        if (next.view.colorBy === DEFAULT_COLOR_BY) params.delete("colorBy");
        else params.set("colorBy", next.view.colorBy);
        if (next.view.lanes) params.set("lanes", "1");
        else params.delete("lanes");
        if (next.view.mode === "list") params.set("mode", "list");
        else params.delete("mode");
        params.delete("agents");
        for (const agent of next.view.agents) params.append("agents", agent);
        if (next.brush === null) {
          params.delete("ws");
          params.delete("we");
        } else {
          params.set("ws", next.brush.start);
          params.set("we", next.brush.end);
        }
        return params;
      });
    },
    [update]
  );

  return [{ window: windowChoice, filters, sorting, view, brush }, setState];
}

/** The Timeline | List segmented toggle. */
function ModeToggle({ mode, onChange }: { mode: Mode; onChange: (mode: Mode) => void }): ReactNode {
  const item = (value: Mode, label: string) => (
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
 *  share one Other colour and lane; with none chosen, the colours and lanes are the facet's usual.
 *  A value's count is of every pull request of the window, with no facet or search applied
 *  (`deliveryColorCounts` in Dispatch's `api/delivery.go`), so a facet or a search changes none.
 *  It is drawn only while `counted` holds, the page's word that the answer on screen is the one
 *  for the address: while a read is out the previous answer stays on screen, and where the
 *  address names another window, its counts are the previous window's. */
function OwnColourPicker({
  counted,
  counts,
  groups,
  selected,
  onChange,
}: {
  counted: boolean;
  counts: Readonly<Record<string, number>>;
  groups: readonly DeliverySessionGroup[];
  selected: readonly string[];
  onChange: (next: string[]) => void;
}): ReactNode {
  const [open, setOpen] = useState(false);
  const options = Object.keys(counts)
    .filter((value) => !Object.hasOwn(PLACEHOLDER_LABELS, value))
    .sort((a, b) => (counts[b] ?? 0) - (counts[a] ?? 0));
  const state = selected.length === 0 ? "every value" : `${selected.length} chosen`;
  // A chosen agent comes from the address, so the counts are read by their own keys alone.
  const countOf = (value: string) => ownEntry(counts, value) ?? 0;
  return (
    <MultiSelect
      emptyMessage="No matches."
      label="Own colour and lane"
      onChange={onChange}
      onOpenChange={setOpen}
      open={open}
      optionDetail={counted ? (value) => String(countOf(value)) : undefined}
      optionDetailLabel={
        counted ? (value) => `${countOf(value)} PR${countOf(value) === 1 ? "" : "s"}` : undefined
      }
      optionLabel={(value) => parentAgentLabel(value, groups)}
      options={[...new Set([...selected, ...options])]}
      searchLabel="Search parent agents…"
      selected={selected}
      size="compact"
      triggerAriaLabel={`Own colour and lane: ${state}`}
      triggerClassName={`flex items-center rounded border text-sm ${borderStrong} ${surfaceBg} ${textPrimaryOnSurface}`}
    >
      <span className="truncate">Own colour: {state}</span>
    </MultiSelect>
  );
}

/** The four facets that are no column of the list, so the one trigger of the control row holds
 *  them in both views. */
const PANEL_FACETS: { key: PanelFacet; label: string }[] = [
  { key: "repo", label: "Repository" },
  { key: "session", label: "Session" },
  { key: "priority", label: "Priority" },
  { key: "component", label: "Component" },
];

/** Every facet the page narrows by, in the order its panel lists them and its chips are drawn:
 *  the four above, then the five that are columns of the list (`PRList`'s `COLUMN_FACETS`),
 *  each named by its column's header. */
const FACETS: { key: FacetKey; label: string }[] = [
  ...PANEL_FACETS,
  ...COLUMN_FACETS.map((key) => ({ key, label: columnHeader(key) })),
];

const facetLabelClass = `mb-1 block text-xs tracking-wide uppercase ${textMutedOnCanvas}`;
const facetTriggerClass = `flex w-full items-center justify-between rounded border text-left text-xs ${borderStrong} ${surfaceBg} ${textPrimaryOnSurface}`;

/** One facet's searchable checkbox list: "Any <facet>" until something is picked, each value
 *  with its count, in the order `values` gives. A facet with no values in the window and nothing
 *  picked is not shown. Until the answer for the address is in (`counts` undefined) a value is
 *  drawn with no count: nothing has counted it yet, a 0 there would say the window holds no such
 *  pull request, and the previous answer's count, kept on screen while the new read is out, would
 *  be the last filter's. */
function Facet({
  label,
  values,
  counts,
  selected,
  valueLabel,
  open,
  onOpenChange,
  onChange,
}: {
  label: string;
  values: readonly string[];
  counts: Readonly<Record<string, number>> | undefined;
  selected: readonly string[];
  valueLabel: (value: string) => string;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onChange: (next: string[]) => void;
}): ReactNode {
  if (values.length === 0 && selected.length === 0) return null;
  const options = [...new Set([...selected, ...values])];
  const state =
    selected.length === 0 ? `Any ${label.toLowerCase()}` : `${selected.length} selected`;
  // A selected value comes from the address, so the counts are read by their own keys alone.
  const countOf = (value: string) =>
    (counts === undefined ? undefined : ownEntry(counts, value)) ?? 0;
  return (
    <div className="w-full">
      <span className={facetLabelClass}>{label}</span>
      <MultiSelect
        emptyMessage="No matches."
        label={label}
        onChange={onChange}
        onOpenChange={onOpenChange}
        open={open}
        optionDetail={counts === undefined ? undefined : (value) => String(countOf(value))}
        optionDetailLabel={
          counts === undefined
            ? undefined
            : (value) => {
                const count = countOf(value);
                return `${count} PR${count === 1 ? "" : "s"}`;
              }
        }
        optionLabel={valueLabel}
        options={options}
        searchLabel={`Search ${label.toLowerCase()}\u2026`}
        selected={selected}
        size="compact"
        triggerAriaLabel={`${label}: ${state}`}
        triggerClassName={facetTriggerClass}
      >
        <span className="truncate">{state}</span>
      </MultiSelect>
    </div>
  );
}

/**
 * The control bar the page's controls sit in (`ControlRow`), handed each control as its props:
 * the search box, **Filter** over the facets, and a chip for every value that is on, which the
 * trigger's panel lists at its top too, so every facet can be set and taken off from this bar in
 * either view. The page has no **Clear filters**: each chip takes its own value off.
 *
 * The panel holds the four facets that are no column of the list. While the list is drawn
 * (`columnsDrawn`, the List view) the other five sit on the headers of the columns they narrow,
 * where `ColumnFilter` draws them; while it is not (the Timeline) no header is there to hold
 * them, so the panel holds all nine. A chip is drawn for every facet that is on, whatever set it
 * (the panel, a column's header or a link), and the trigger counts them all. A chip's press
 * takes its value off the page's filters, which every column's filter state is read from, so it
 * comes off the header too.
 *
 * Each facet is counted by Dispatch with every other facet applied and its own ignored, so the
 * counts here are the answer's, never the page's. A value's count is drawn only while the answer
 * on screen is the one for the window, filters and search the address names (`counted`): while a
 * new read is out the previous answer stays on screen, and its counts would be the last filter's.
 * Under Parent agent's old place, **Edit groups** opened the session-groups editor; it sits at
 * the foot of this panel now, since that facet is a column. The editor it opens stands under the
 * bar, outside it.
 *
 * The row is drawn from the address alone, before the first answer and after a first read that
 * failed (`data` undefined), so a link's narrowing has its chip, its count and its way off while
 * the page has nothing else to show. A value is named in the words the answer gives it - an
 * issue's title, a group's or a component's name - only once there is an answer; until then it
 * reads as the address spells it. The panel then draws no count beside a value, since nothing has
 * counted them, and no **Edit groups**, whose editor edits the groups the answer lists, and what
 * it offers depends on the view. On the Timeline it holds all nine facets, so it offers what is
 * picked and Rework's and Deployed's every verdict. In the List it holds only the four facets that
 * are no column; the five that are get their headers only once the list is drawn, with the
 * answer, so until then a value the address picks in one of those comes off by its chip alone.
 */
function DeliveryControls({
  columnsDrawn,
  counted,
  data,
  filters,
  onFilters,
}: {
  /** Whether the list is drawn, whose headers hold the five facets that are its columns. */
  columnsDrawn: boolean;
  /** Whether `data` is the answer for the window, filters and search the address names, the
   *  page's own gate on its count. */
  counted: boolean;
  /** The answer the panel counts and names values from; undefined until the first one. */
  data:
    | Pick<
        DeliveryTimelineWithMeasures,
        | "facet_counts"
        | "components"
        | "issue_titles"
        | "session_groups"
        | "session_titles"
        | "session_pr_counts"
      >
    | undefined;
  filters: Filters;
  onFilters: (next: Filters) => void;
}): ReactNode {
  const [panelOpen, setPanelOpen] = useState(false);
  const facetPanel = useNestedPanels<FacetKey>(panelOpen);
  const [editingGroups, setEditingGroups] = useState(false);
  const countsOf = (key: FacetKey) => data?.facet_counts?.[FACET_PARAMS[key]] ?? {};
  // Rework and Deployed offer every verdict in their own order, as their headers do; every other
  // facet offers what the window counts, busiest first.
  const valuesOf = (key: FacetKey): readonly string[] => {
    const fixed = isColumnFacet(key) ? FIXED_FACET_VALUES[key] : undefined;
    if (fixed !== undefined) return fixed;
    const counts = countsOf(key);
    return Object.keys(counts).sort((a, b) => (counts[b] ?? 0) - (counts[a] ?? 0));
  };
  // How many sub-components each component has in the window's projects: selecting a parent
  // selects them too (the server expands it), and the prototype lists components flat, so the
  // option says so. One walk up from each component counts it under every ancestor, rather than
  // one descendant walk per component as architecture-model.ts's descendantComponents would take.
  const subComponents = useMemo(() => {
    const counts = new Map<string, number>();
    for (const component of Object.values(data?.components ?? {})) {
      const seen = new Set<string>();
      for (let parent = component.parent; parent !== null && !seen.has(parent); ) {
        seen.add(parent);
        counts.set(parent, (counts.get(parent) ?? 0) + 1);
        parent = data?.components?.[parent]?.parent ?? null;
      }
    }
    return counts;
  }, [data?.components]);
  // A column facet's value reads as its header's filter reads it. Before the first answer an
  // issue has no title to read beside its key, so it reads as its key alone.
  const valueLabel = (key: FacetKey, value: string): string => {
    if (isColumnFacet(key)) {
      if (data === undefined && key === "issue")
        return ownEntry(PLACEHOLDER_LABELS, value) ?? value;
      return columnFacetValue(key, value, {
        issueTitles: data?.issue_titles ?? {},
        sessionGroups: data?.session_groups ?? [],
      });
    }
    const placeholder = ownEntry(PLACEHOLDER_LABELS, value);
    if (placeholder !== undefined) return placeholder;
    if (key === "component") {
      const title = data?.components?.[value]?.title ?? value;
      const children = subComponents.get(value) ?? 0;
      if (children === 0) return title;
      return `${title} (includes ${children} sub-component${children === 1 ? "" : "s"})`;
    }
    return value;
  };
  const chosen: FilterChip[] = FACETS.flatMap(({ key, label }) =>
    filters[key].map((value) => ({
      key: `${key}:${value}`,
      label: `${label}: ${valueLabel(key, value)}`,
      remove: () => onFilters({ ...filters, [key]: filters[key].filter((one) => one !== value) }),
    }))
  );
  const inPanel = columnsDrawn ? PANEL_FACETS : FACETS;
  return (
    <>
      <ControlRow
        chips={chosen}
        search={{
          label: "Search pull requests",
          onChange: (value) => onFilters({ ...filters, search: value }),
          placeholder: "Title or PR id",
          value: filters.search,
        }}
        size="compact"
        trigger={{
          children: () => (
            <>
              {inPanel.map(({ key, label }) => (
                <Facet
                  {...facetPanel(key)}
                  counts={data === undefined || !counted ? undefined : countsOf(key)}
                  key={key}
                  label={label}
                  onChange={(next) => onFilters({ ...filters, [key]: next })}
                  selected={filters[key]}
                  valueLabel={(value) => valueLabel(key, value)}
                  values={valuesOf(key)}
                />
              ))}
              {data === undefined ? null : (
                <button
                  className={`text-xs underline ${linkText}`}
                  onClick={() => setEditingGroups(true)}
                  type="button"
                >
                  Edit groups
                </button>
              )}
            </>
          ),
          count: chosen.length,
          label: "Filter",
          onOpenChange: setPanelOpen,
          open: panelOpen,
          stacked: true,
        }}
      />
      {/* What Edit groups opens stands under the bar, outside it: a dialog at the window's
          right edge, so it takes no room in the page's column. */}
      {editingGroups && data !== undefined ? (
        <SessionGroupsEditor
          counts={data.session_pr_counts ?? {}}
          groups={data.session_groups ?? []}
          onClose={() => setEditingGroups(false)}
          titles={data.session_titles ?? {}}
        />
      ) : null}
    </>
  );
}

/** `/delivery`: successful production deploys, pipeline failures, and merged PRs labelled by
 *  agent session, Dispatch issue, priority, and architectural component, as the prototype's
 *  timeline view shows them. Until the timeline's configuration is set, Dispatch refuses the
 *  page's live view with `DELIVERY_NOT_CONFIGURED` and the page shows the settings form in the
 *  timeline's place; once a save configures it, Dispatch answers the same subscription with the
 *  timeline. */
export function DeliveryPage(): ReactNode {
  useDocumentTitle("Delivery · Dispatch");
  const [state, setState] = useDeliveryUrlState();
  const [selection, setSelection] = useState<TimelineSelection | null>(null);
  const search = useSettled(state.filters.search.trim(), SEARCH_DEBOUNCE_MS);

  // Every active facet and the search, as the query string the GET route would take without its
  // window: Dispatch applies them, so `query.data.prs` is already the filtered list.
  const liveQuery = useMemo(() => {
    const values: Record<string, string | string[]> = {};
    if (search !== "") values.q = search;
    for (const key of FACET_KEYS) values[FACET_PARAMS[key]] = [...state.filters[key]];
    return queryString(values);
  }, [state.filters, search]);
  const query = useQuery(
    deliveryTimelineQuery({ kind: "timeline", window: liveWindow(state.window), query: liveQuery })
  );
  const data = query.data;
  // The measures follow a brush drag as the prototype's panel does (its App.tsx: computeDora over
  // the brush window), so they read the brush where the timeline keeps reading the whole window.
  // With no brush the timeline's own answer carries them (`measures`), so the page subscribes its
  // population once; only a brush subscribes a measures view of its own, for its window.
  const brushed = useQuery({
    ...deliveryMeasuresQuery({
      kind: "measures",
      window:
        state.brush === null
          ? liveWindow(state.window)
          : { from: state.brush.start, to: state.brush.end },
      query: liveQuery,
    }),
    enabled: state.brush !== null,
  });
  const measures = state.brush === null ? data?.measures : brushed.data;
  // When the answer on screen last changed (its snapshot or its latest delta). While a facet change
  // waits for its snapshot, the previous answer stays on screen (`keepPreviousData`) and the new
  // query reports `dataUpdatedAt` 0, so this keeps the last time an answer arrived instead of
  // reading that as the epoch.
  const answeredAtRef = useRef(0);
  if (query.dataUpdatedAt > 0) answeredAtRef.current = query.dataUpdatedAt;
  const answeredAt = answeredAtRef.current;

  const activeWindow = useMemo(
    () =>
      state.brush ?? (data === undefined ? null : { start: data.window.from, end: data.window.to }),
    [state.brush, data]
  );
  const shownPRs = useMemo(() => {
    const prs = data?.prs ?? [];
    if (state.brush === null) return prs;
    const startMs = Date.parse(state.brush.start);
    const endMs = Date.parse(state.brush.end);
    return prs.filter((pr) => {
      if (pr.merged_at === null) return false;
      const mergedMs = Date.parse(pr.merged_at);
      return mergedMs >= startMs && mergedMs <= endMs;
    });
  }, [data, state.brush]);
  // The waiting line's population, whatever the brush: the pull requests still waiting at the
  // read window's start, and every tracked merge of the read window. The brush narrows the line's
  // time range, never what it counts, so a merge before the brush that has not shipped holds the
  // line up across it.
  const waitingMerges = useMemo(
    () => [
      ...(data?.waiting ?? []),
      ...(data?.prs ?? []).filter((pr) => pr.deployed_status !== "not_tracked"),
    ],
    [data]
  );
  // Coloured by Parent agent, the chosen values take the palette and the rest share Other.
  const chosenAgents = state.view.colorBy === "parentAgent" ? state.view.agents : NO_AGENTS;
  const colorScale = useMemo(
    () =>
      buildColorScale(
        data?.color_counts?.[COLOR_COUNT_KEYS[state.view.colorBy]] ?? {},
        chosenAgents
      ),
    [data, state.view.colorBy, chosenAgents]
  );
  const allRepos = useMemo(() => Object.keys(data?.color_counts?.repo ?? {}), [data]);

  const drillDownSelection = useMemo((): TimelineSelection | null => {
    if (selection === null) return null;
    if (selection.kind !== "pr") return selection;
    return (data?.prs ?? []).some((pr) => pr.id === selection.id) ? selection : null;
  }, [selection, data]);

  const setView = (view: DeliveryView) => setState({ ...state, view });
  const setFilters = (filters: Filters) => setState({ ...state, filters });

  // The five facets that are columns, as TanStack holds a column's filter, and back again: the
  // adapter is the page's, so the header's filter writes the page's own parameter (`parentAgent`
  // keeps its camelCase spelling, whatever the server's is) and the shared control reads no
  // query of its own (decision 7).
  // On what the five facets hold, written out as the address writes them, rather than on
  // `state.filters`: that object is rebuilt on every write of the address - the search box
  // writes `?q=` per keystroke - while TanStack compares its filtered row model's dependencies
  // by reference and would re-filter the whole window per letter typed.
  const columnFilterKey = COLUMN_FACETS.map((key) => `${key}=${state.filters[key].join(",")}`).join(
    "&"
  );
  // biome-ignore lint/correctness/useExhaustiveDependencies: `columnFilterKey` stands in for `state.filters`, which is a new object on every address write
  const columnFilters = useMemo<ColumnFiltersState>(
    () =>
      COLUMN_FACETS.flatMap((key) =>
        state.filters[key].length === 0 ? [] : [{ id: key, value: [...state.filters[key]] }]
      ),
    [columnFilterKey]
  );
  const setColumnFilters: OnChangeFn<ColumnFiltersState> = (update) => {
    const next = typeof update === "function" ? update(columnFilters) : update;
    const picked = new Map(next.map((filter) => [filter.id, filter.value as string[]]));
    setFilters({
      ...state.filters,
      author: picked.get("author") ?? [],
      deployed: picked.get("deployed") ?? [],
      issue: picked.get("issue") ?? [],
      parentAgent: picked.get("parentAgent") ?? [],
      rework: picked.get("rework") ?? [],
    });
  };
  const setSorting: OnChangeFn<SortingState> = (update) => {
    const next = typeof update === "function" ? update(state.sorting) : update;
    // A third press on a header takes TanStack's sort off the column; this list has always
    // cycled between the two directions instead, so a sort it removed goes back to the
    // ascending one the column offers first.
    setState({
      ...state,
      sorting: next.length > 0 ? next : state.sorting.map(({ id }) => ({ id, desc: false })),
    });
  };

  // Whether the page shows the setup form: the last settled timeline read's answer, held while a
  // read is in flight. A refetch with nothing cached clears `query.error` the moment it starts
  // (the window regaining focus or a live-socket gap refresh), so a body read live from the query
  // would unmount the form, and the draft typed into it, on every tab switch. A read that settles
  // on anything but `DELIVERY_NOT_CONFIGURED` shows that instead: the timeline a save brings, or a
  // failure with Retry. Set during render (React's pattern for state derived from props), so the
  // render that settles already shows the new body.
  const [notConfigured, setNotConfigured] = useState(false);
  if (query.fetchStatus === "idle" && isDeliveryNotConfigured(query.error) !== notConfigured) {
    setNotConfigured(!notConfigured);
  }
  // The header's **clear brush window** goes with the brush it clears, and the focus it held goes
  // to the control now at its place in the line it sits in, which holds no other, else to the
  // Window list's trigger while any part of it is in the reader's sight, scrolled the least
  // distance that shows it whole, else to the main content's own stop with no scroll
  // (`useFocusPlace`). So a reader scrolled below the trigger is not pulled back up to it. The
  // trigger is found in the header by its mark (`WINDOW_TRIGGER_SELECTOR`), since the shared list
  // hands out no element of its own. The Window list itself stays mounted across a change of the
  // window (`WindowControl`), so a pick keeps the focus where it was.
  const generatedLine = useRef<HTMLDivElement>(null);
  const noteFocus = useFocusPlace(
    (element) => (generatedLine.current?.contains(element) ? generatedLine.current : null),
    (line) => line.closest("header")?.querySelector<HTMLElement>(WINDOW_TRIGGER_SELECTOR) ?? null
  );
  const [colorListOpen, setColorListOpen] = useState(false);
  // The list's own body is drawn once an answer is in; the control row is drawn before it
  // (`DeliveryControls`), at the one place in the tree, so the answer arriving does not remount
  // it and a link's narrowing has its chip and its way off while the page reads.
  const drawn = data !== undefined && activeWindow !== null;
  // The count is the answer's for the filter and the window the address names, so it is drawn
  // only while the answer on screen is that one: not while a new filter's or a new window's read
  // is out and the previous answer stays on screen (`isPlaceholderData`), nor while a search the
  // reader is typing waits out its debounce before it is asked for. A value's count in the Filter
  // panel, in the Own colour list and in a List column's header filter is drawn under the same
  // gate.
  const counted = !query.isPlaceholderData && search === state.filters.search.trim();
  return (
    // From `xl` the page is the window's height and the chart takes what its header leaves, never
    // less than its own `min-h-[28rem]`. Where the window is too short for both - 1280x800 and
    // 1280x720 - the page scrolls inside this box, rather than spill past the shell, which made
    // the document scroll on past it and took the shell's Navigation off the top of the screen.
    <section className="flex flex-col gap-3 xl:h-[calc(100dvh-3rem)] xl:overflow-y-auto">
      {/* biome-ignore lint/a11y/noStaticElementInteractions: nothing here is made interactive - the focus handler only notes which control of the header took the focus, for the hand-off when a commit takes that control away (`useFocusPlace`), and the capture handlers only arm that hand-off at a press inside the header (`armOnPress`), so the header needs no role of its own. */}
      <header
        className="flex flex-wrap items-baseline justify-between gap-x-4 gap-y-1"
        {...ARM_ON_PRESS}
        onFocus={noteFocus}
      >
        <h1 className={`text-xl font-bold ${textPrimaryOnCanvas}`}>Delivery timeline</h1>
        {notConfigured ? null : (
          <WindowControl
            onChange={(window) => setState({ ...state, window, brush: null })}
            size="compact"
            value={state.window}
          />
        )}
        {data === undefined ? null : (
          <div className={`text-xs ${textMutedOnCanvas}`} ref={generatedLine}>
            Generated {new Date(answeredAt).toLocaleString()} · window{" "}
            {new Date(data.window.from).toLocaleDateString()}–
            {new Date(data.window.to).toLocaleDateString()}
            {state.brush === null ? null : (
              <button
                className={`ml-3 underline ${linkText}`}
                onClick={() => setState({ ...state, brush: null })}
                type="button"
              >
                clear brush window
              </button>
            )}
          </div>
        )}
      </header>

      {notConfigured ? (
        <section aria-labelledby="delivery-setup-heading" className="max-w-4xl">
          <h2
            className={`text-base font-semibold ${textPrimaryOnCanvas}`}
            id="delivery-setup-heading"
          >
            Set up the delivery timeline
          </h2>
          <p className={`mt-1 text-sm ${textSecondaryOnCanvas}`}>
            Once set, this page shows the merged pull requests of the authors you name, the
            production deploys that shipped them, and the pipeline failures in between.
          </p>
          <DeliverySettingsForm initial={null} />
        </section>
      ) : (
        <>
          {data === undefined ? null : (
            <SourceFreshness freshness={data.freshness} readAtMs={answeredAt} />
          )}

          {query.isPending ? <p className={textMutedOnCanvas}>Loading delivery timeline…</p> : null}
          {query.isError ? (
            <QueryError
              message="Couldn't load the delivery timeline."
              onRetry={() => void query.refetch()}
            />
          ) : null}

          {data === undefined ? null : state.brush !== null && brushed.isError ? (
            <QueryError
              message="Couldn't load the delivery measures."
              onRetry={() => void brushed.refetch()}
            />
          ) : (
            <MeasuresPanel data={measures} />
          )}

          {drawn ? (
            <div className="flex flex-wrap items-center gap-2 text-sm">
              <ModeToggle
                mode={state.view.mode}
                onChange={(mode) => setView({ ...state.view, mode })}
              />
              <div className={`flex items-center gap-2 ${textMutedOnCanvas}`}>
                {/* The list is named for the field itself, so its words here are for the eye
                    alone. A pick of the choice it shows writes nothing, as a select set to the
                    value it holds fires nothing. */}
                <span aria-hidden="true">Color merges by</span>
                <MultiSelect
                  emptyMessage="No choice matches that."
                  label="Color merges by"
                  mode="single"
                  onOpenChange={setColorListOpen}
                  onPick={(picked) => {
                    const colorBy = COLOR_FACETS.find((facet) => facet === picked);
                    if (colorBy !== undefined && colorBy !== state.view.colorBy) {
                      setView({ ...state.view, colorBy });
                    }
                  }}
                  open={colorListOpen}
                  optionLabel={(facet) =>
                    COLOR_FACET_OPTIONS.find((option) => option.value === facet)?.label
                  }
                  options={COLOR_FACETS}
                  searchLabel="Search color choices"
                  size="compact"
                  triggerClassName={`rounded border text-left ${surfaceMutedBg} ${borderStrong} ${textPrimaryOnCanvas}`}
                  value={state.view.colorBy}
                />
              </div>
              {state.view.colorBy === "parentAgent" ? (
                <OwnColourPicker
                  counted={counted}
                  counts={data.color_counts?.parent_agent ?? {}}
                  groups={data.session_groups ?? []}
                  onChange={(agents) => setView({ ...state.view, agents })}
                  selected={state.view.agents}
                />
              ) : null}
              {state.view.mode === "timeline" ? (
                <LanesSwitch
                  checked={state.view.lanes}
                  onChange={(lanes) => setView({ ...state.view, lanes })}
                />
              ) : null}
              <span className={`ml-4 ${textMutedOnCanvas}`}>
                {counted
                  ? `${shownPRs.length} PRs in current filter/window`
                  : "Counting PRs in current filter/window…"}
              </span>
            </div>
          ) : null}

          <DeliveryControls
            columnsDrawn={state.view.mode === "list"}
            counted={counted}
            data={data}
            filters={state.filters}
            onFilters={setFilters}
          />

          {drawn ? (
            <div className="flex min-h-0 flex-1 flex-col gap-4 xl:flex-row">
              <div className="flex h-[75dvh] min-h-[28rem] min-w-0 flex-col xl:h-auto xl:flex-1">
                {state.view.mode === "timeline" ? (
                  <Timeline
                    chosen={chosenAgents}
                    colorBy={state.view.colorBy}
                    colorScale={colorScale}
                    components={data.components ?? {}}
                    filters={state.filters}
                    lanes={state.view.lanes}
                    onBrush={(brush) => setState({ ...state, brush })}
                    onSelect={setSelection}
                    prs={shownPRs}
                    runs={data.runs ?? []}
                    sessionGroups={data.session_groups ?? []}
                    waiting={waitingMerges}
                    window={activeWindow}
                  />
                ) : (
                  <PRList
                    allRepos={allRepos}
                    colorBy={state.view.colorBy}
                    colorScale={colorScale}
                    columnFilters={columnFilters}
                    counted={counted}
                    facetCounts={data.facet_counts ?? {}}
                    issueTitles={data.issue_titles ?? {}}
                    onColumnFiltersChange={setColumnFilters}
                    onSelect={(id) => setSelection({ kind: "pr", id })}
                    onSelectRun={(id) => setSelection({ kind: "deploy", id })}
                    onSortingChange={setSorting}
                    prs={shownPRs}
                    selectedId={selection?.kind === "pr" ? selection.id : undefined}
                    sessionGroups={data.session_groups ?? []}
                    sorting={state.sorting}
                  />
                )}
              </div>
              <DrillDown
                components={data.components ?? {}}
                onClose={() => setSelection(null)}
                prs={data.prs ?? []}
                runs={data.runs ?? []}
                selection={drillDownSelection}
                sessionGroups={data.session_groups ?? []}
                sessionTitles={data.session_titles ?? {}}
              />
            </div>
          ) : null}
        </>
      )}
    </section>
  );
}
