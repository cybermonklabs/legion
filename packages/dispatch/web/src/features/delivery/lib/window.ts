// The Delivery page's window: a rolling preset (the URL keeps `window=7d`, so a shared link always
// shows the latest days) or a custom range of whole UTC days (the URL keeps `from` and `to`). A
// preset is subscribed by its days, so Dispatch keeps its end at the present; the page reads the
// range it resolved from the answer's `window`. The server refuses a window over 84 days
// (`maxDeliveryWindow`, api/delivery.go); a custom range longer than that is refused here first,
// with the limit stated. Pure: no React.
import type { DeliveryLiveWindow } from "../../../api/types";

export const WINDOW_PRESETS = ["7d", "14d", "28d", "84d"] as const;
export type WindowPreset = (typeof WINDOW_PRESETS)[number];

/** The preset the page shows when its URL names none: the prototype's 28 days. */
export const DEFAULT_WINDOW_PRESET: WindowPreset = "28d";

/** The longest window the server reads (`maxDeliveryWindow`). */
export const MAX_WINDOW_DAYS = 84;

export type WindowChoice =
  | { kind: "preset"; preset: WindowPreset }
  | { kind: "custom"; from: string; to: string };

/** A whole day in milliseconds, as every window of whole UTC days is measured in. */
export const DAY_MS = 24 * 60 * 60 * 1000;

const PRESET_DAYS = { "7d": 7, "14d": 14, "28d": 28, "84d": 84 } as const satisfies Record<
  WindowPreset,
  number
>;

export const PRESET_LABELS: Record<WindowPreset, string> = {
  "7d": "Last 7 days",
  "14d": "Last 14 days",
  "28d": "Last 28 days",
  "84d": "Last 84 days",
};

function isPreset(value: string | null): value is WindowPreset {
  return WINDOW_PRESETS.some((preset) => preset === value);
}

function isCustomWindow(from: string, to: string): boolean {
  const fromMs = Date.parse(from);
  const toMs = Date.parse(to);
  return (
    Number.isFinite(fromMs) &&
    Number.isFinite(toMs) &&
    toMs > fromMs &&
    toMs - fromMs <= MAX_WINDOW_DAYS * DAY_MS
  );
}

/** Whether the address names a malformed window. A valid custom range wins over an accompanying
 *  preset for backward compatibility; partial, unparseable, inverted and overlong custom values
 *  are malformed, as is an unknown preset with no valid custom range. */
export function hasMalformedWindowParams(params: URLSearchParams): boolean {
  const from = params.get("from");
  const to = params.get("to");
  if (from !== null || to !== null) {
    return from === null || to === null || !isCustomWindow(from, to);
  }
  const preset = params.get("window");
  return preset !== null && !isPreset(preset);
}

/** The window the URL names: a valid `from` and `to` together are a custom range, else `window`
 *  names a preset, else (absent or malformed) the 28-day preset. */
export function readWindow(params: URLSearchParams): WindowChoice {
  const from = params.get("from");
  const to = params.get("to");
  if (from !== null && to !== null && isCustomWindow(from, to)) {
    return { kind: "custom", from, to };
  }
  const preset = params.get("window");
  return { kind: "preset", preset: isPreset(preset) ? preset : DEFAULT_WINDOW_PRESET };
}

/** The live socket's window for a choice: a preset by its days, whose end Dispatch keeps at the
 *  present, or a custom range by its exact ends. */
export function liveWindow(choice: WindowChoice): DeliveryLiveWindow {
  return choice.kind === "custom"
    ? { from: choice.from, to: choice.to }
    : { preset: PRESET_DAYS[choice.preset] };
}

/** A custom range from two `YYYY-MM-DD` dates read as whole UTC days: it starts at the first
 *  day's midnight and ends at the midnight after the last, so the last day is included. Refused,
 *  with the reason, when a date is missing, the end comes before the start, or it spans more
 *  than MAX_WINDOW_DAYS. */
export function customRange(
  fromDate: string,
  toDate: string
): { from: string; to: string } | { refused: string } {
  const fromMs = Date.parse(`${fromDate}T00:00:00Z`);
  const lastMs = Date.parse(`${toDate}T00:00:00Z`);
  if (Number.isNaN(fromMs) || Number.isNaN(lastMs)) {
    return { refused: "Choose both a start and an end date." };
  }
  if (lastMs < fromMs) return { refused: "The window's end must not come before its start." };
  const days = Math.round((lastMs - fromMs) / DAY_MS) + 1;
  if (days > MAX_WINDOW_DAYS) {
    return {
      refused: `The window can span at most ${MAX_WINDOW_DAYS} days; this range spans ${days}.`,
    };
  }
  return { from: new Date(fromMs).toISOString(), to: new Date(lastMs + DAY_MS).toISOString() };
}

/** A `YYYY-MM-DD` date input's value for an instant, in UTC. */
export function utcDate(iso: string): string {
  return new Date(iso).toISOString().slice(0, 10);
}

/** The last day a custom range's exclusive `to` includes, as a date input's value. */
export function lastUtcDate(toIso: string): string {
  return new Date(Date.parse(toIso) - DAY_MS).toISOString().slice(0, 10);
}

/** What the Evals tab compares its window with: by default the range just before the window, as
 *  long as it (`previous`, which the URL names by naming nothing), nothing (`compare=off`), or a
 *  custom range (`compare_from` and `compare_to`, read as a custom window is). */
export type CompareChoice =
  | { kind: "previous" }
  | { kind: "off" }
  | { kind: "custom"; from: string; to: string };

/** The compare range the URL names: a valid `compare_from` and `compare_to` together are a custom
 *  range, else `compare=off` names none, else (absent or malformed) the previous range. */
export function readCompare(params: URLSearchParams): CompareChoice {
  const from = params.get("compare_from");
  const to = params.get("compare_to");
  if (from !== null && to !== null && isCustomWindow(from, to)) {
    return { kind: "custom", from, to };
  }
  return params.get("compare") === "off" ? { kind: "off" } : { kind: "previous" };
}

/** The whole UTC days a window covers at `nowMs`, as Dispatch counts an eval read's window
 *  (`evalDaysOf`, api/evals.go): from the midnight of its first instant's day to the midnight
 *  after its last instant's day. A preset's instants are its days ending at `nowMs`, so a preset
 *  that ends inside a day covers one day more than it names. */
export function windowDays(choice: WindowChoice, nowMs: number): { from: string; to: string } {
  const fromMs =
    choice.kind === "custom"
      ? Date.parse(choice.from)
      : nowMs - PRESET_DAYS[choice.preset] * DAY_MS;
  const toMs = choice.kind === "custom" ? Date.parse(choice.to) : nowMs;
  const first = Math.floor(fromMs / DAY_MS) * DAY_MS;
  const end = (Math.floor((toMs - 1) / DAY_MS) + 1) * DAY_MS;
  return { from: new Date(first).toISOString(), to: new Date(end).toISOString() };
}

/** The range `compare` names for `window` at `nowMs`, as an eval read's `compare_from` and
 *  `compare_to`, or null for none. A custom range is its own; the previous range is the whole UTC
 *  days just before the window's first, as many as the window covers (`windowDays`), and at most
 *  MAX_WINDOW_DAYS, the longest range Dispatch reads: the 84-day preset that ends inside a day
 *  covers 85, and is compared with the 84 before it. */
export function compareRange(
  compare: CompareChoice,
  window: WindowChoice,
  nowMs: number
): { from: string; to: string } | null {
  if (compare.kind === "off") return null;
  if (compare.kind === "custom") return { from: compare.from, to: compare.to };
  const days = windowDays(window, nowMs);
  const first = Date.parse(days.from);
  const length = Math.min(Date.parse(days.to) - first, MAX_WINDOW_DAYS * DAY_MS);
  return { from: new Date(first - length).toISOString(), to: days.from };
}
