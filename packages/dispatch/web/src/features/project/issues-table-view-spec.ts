import { LIFECYCLE_ORDER } from "./issues-table-lifecycle";
import type { ColumnId } from "./issues-table-model";

export type BuiltInViewId =
  | "open"
  | "triage"
  | "waiting"
  | "in-flight"
  | "slots"
  | "finished"
  | "mine";

export interface BuiltInView {
  readonly id: BuiltInViewId;
  readonly name: string;
  readonly params: (hasLegion: boolean) => URLSearchParams;
  readonly cols: readonly ColumnId[];
  readonly bar: "stage" | "waiting_reason" | "slot_use" | null;
  readonly legionOnly: boolean;
}

const inFlightParams = (hasLegion: boolean) => {
  if (!hasLegion)
    return new URLSearchParams([
      ["status", "in_progress"],
      ["status", "testing"],
      ["status", "needs_review"],
      ["status", "retro"],
      ["sort", "stage"],
    ]);
  const first = LIFECYCLE_ORDER.indexOf("status:in_progress");
  const last = LIFECYCLE_ORDER.indexOf("status:retro");
  return new URLSearchParams([
    ...LIFECYCLE_ORDER.slice(first, last + 1).map((stage): [string, string] => ["f.stage", stage]),
    ["sort", "stage,-time_in_phase"],
  ]);
};

export const BUILT_IN_VIEWS: readonly BuiltInView[] = [
  {
    id: "open",
    name: "All open",
    params: () => new URLSearchParams([["sort", "queue"]]),
    cols: [
      "priority",
      "queue",
      "key",
      "title",
      "status",
      "phase",
      "epic",
      "assignee",
      "claim",
      "asks",
      "created",
    ],
    bar: "stage",
    legionOnly: false,
  },
  {
    id: "triage",
    name: "Triage",
    params: () =>
      new URLSearchParams([
        ["status", "triage"],
        ["sort", "-created"],
      ]),
    cols: ["priority", "key", "title", "author", "labels", "created"],
    bar: null,
    legionOnly: false,
  },
  {
    id: "waiting",
    name: "Waiting line",
    params: () =>
      new URLSearchParams([
        ["f.waiting_reason", "ready"],
        ["f.waiting_reason", "slot"],
        ["f.waiting_reason", "blocked"],
        ["f.waiting_reason", "person"],
        ["sort", "queue"],
      ]),
    cols: ["queue", "priority", "key", "title", "phase", "waited", "waiting_reason", "epic"],
    bar: "waiting_reason",
    legionOnly: true,
  },
  {
    id: "in-flight",
    name: "In flight",
    params: inFlightParams,
    cols: ["priority", "key", "title", "phase", "slot", "roles", "ci", "phase_hours", "assignee"],
    bar: "stage",
    legionOnly: false,
  },
  {
    id: "slots",
    name: "Slots",
    params: () =>
      new URLSearchParams([
        ["f.slot_use", "working"],
        ["f.slot_use", "between_turns"],
        ["f.slot_use", "held_children"],
        ["f.slot_use", "held_person"],
        ["f.slot_use", "held_idle"],
        ["sort", "slot"],
      ]),
    cols: ["slot", "key", "title", "phase", "slot_use", "ci", "phase_hours"],
    bar: "slot_use",
    legionOnly: true,
  },
  {
    id: "finished",
    name: "Finished",
    params: (hasLegion) =>
      new URLSearchParams(
        hasLegion
          ? [
              ["status", "done"],
              ["f.closed", "28d"],
              ["f.legion", "worked"],
              ["sort", "-closed"],
            ]
          : [
              ["status", "done"],
              ["f.closed", "28d"],
              ["sort", "-closed"],
            ]
      ),
    cols: ["key", "title", "closed", "work_hours", "phase_hours", "review_rounds", "assignee"],
    bar: null,
    legionOnly: false,
  },
  {
    id: "mine",
    name: "Created by me",
    params: () =>
      new URLSearchParams([
        ["f.author", "@me"],
        ["sort", "-created"],
      ]),
    cols: ["priority", "key", "title", "status", "phase", "created"],
    bar: "stage",
    legionOnly: false,
  },
];

export const DEFAULT_VIEW: BuiltInViewId = "open";

function allOpenView(): BuiltInView {
  const view = BUILT_IN_VIEWS.find((candidate) => candidate.id === DEFAULT_VIEW);
  if (view === undefined) throw new Error("The All open view is missing");
  return view;
}

export function viewOf(search: URLSearchParams): BuiltInView {
  const id = search.get("view");
  return BUILT_IN_VIEWS.find((view) => view.id === id) ?? allOpenView();
}

const PRESERVED_VIEW_KEYS: Readonly<Record<string, true>> = {
  cols: true,
  exclude_tree: true,
  only: true,
  onlyLabel: true,
  page: true,
  q: true,
  size: true,
  view: true,
};

/** Expands a selected view before the model reads its ordinary table controls. */
export function expandView(search: URLSearchParams, hasLegion: boolean): URLSearchParams {
  const selected = viewOf(search);
  const base = selected.legionOnly && !hasLegion ? allOpenView() : selected;
  const expanded = base.params(hasLegion);
  for (const key of new Set(search.keys())) {
    if (PRESERVED_VIEW_KEYS[key]) continue;
    const values = search.getAll(key);
    expanded.delete(key);
    if (key === "nofilter" && values.includes("1")) expanded.delete("status");
    if (values.length === 1 && values[0] === "") {
      if (key === "status") expanded.set("nofilter", "1");
      else if (key === "sort") expanded.set("sort", "");
      continue;
    }
    for (const value of values) expanded.append(key, value);
  }
  for (const key of ["page", "size", "q"]) {
    for (const value of search.getAll(key)) expanded.append(key, value);
  }
  return expanded;
}

/** Returns the project Issues URL for a view link outside the table. */
export function issuesViewHref(
  project: string,
  view: BuiltInViewId,
  overrides: URLSearchParams = new URLSearchParams()
): string {
  const search = new URLSearchParams(view === DEFAULT_VIEW ? undefined : [["view", view]]);
  overrides.forEach((value, key) => {
    search.append(key, value);
  });
  const query = search.toString();
  return `/projects/${project}/issues${query === "" ? "" : `?${query}`}`;
}
