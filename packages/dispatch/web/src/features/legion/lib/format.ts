// What the Legion view writes for the few numbers and words it shows: how long an issue has been
// in its phase, a setting's value, where that value came from, and what the daemon decided about a
// change of it. Pure: no React, no cache, so the board's tests read the rule rather than the
// markup.
import type { LegionSettingSource, LegionSettingVerdict } from "../../../api/types";

/** How long the issue has been in its phase, from the moment the daemon's state first showed it
 *  there: minutes under an hour, hours and minutes under a day, then days and hours. An issue
 *  whose phase entry no read has recorded shows an em dash rather than a time nobody measured. */
export function formatPhaseTime(since: string | null, nowMs: number): string {
  if (since === null) return "—";
  const sinceMs = Date.parse(since);
  if (Number.isNaN(sinceMs)) return "—";
  const minutes = Math.floor(Math.max(0, nowMs - sinceMs) / 60_000);
  if (minutes < 60) return `${minutes} m`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours} h ${minutes % 60} m`;
  return `${Math.floor(hours / 24)} d ${hours % 24} h`;
}

/** One setting's value as the panel shows it and as its input takes it back: JSON, because the
 *  daemon's values are whatever `legion.yaml` holds — a number, a string, a list, a map — and
 *  Dispatch models none of them. */
export function formatSettingValue(value: unknown): string {
  return JSON.stringify(value) ?? "null";
}

/** Where a setting's value came from, in the reader's words: the daemon's configuration file, or
 *  a change someone made through this control, with who made it and when. */
export function describeSettingSource(source: LegionSettingSource | undefined): string {
  if (source === undefined) return "Source unknown";
  const when = Number.isNaN(Date.parse(source.at))
    ? source.at
    : new Date(source.at).toLocaleString();
  if (source.source === "route") {
    return source.by === null ? `Changed here, ${when}` : `Changed here by ${source.by}, ${when}`;
  }
  return `From the daemon's configuration file, ${when}`;
}

/** What the daemon decided about one key of a change, and whether that is a failure the reader
 *  must not dress as a success: `at-once`, `rebuilt` and `next-launch` are the three ways it
 *  applies one, a refusal carries the daemon's own reason and changed nothing, and `failed` is a
 *  change it took into its own settings and could not carry into the thing that holds a copy of
 *  the value — the daemon runs on the new value, that holder kept the old one, and the text is
 *  the holder's own error (the daemon's `api.SettingResult`). A verdict word this build does not
 *  know is shown as the daemon wrote it, never dropped.
 *
 *  The failure names no holder of its own, because the verdict's `holder` is not the one that
 *  kept the old value: the daemon sets it to the row's named holder whether or not that holder
 *  failed, and a row reaches more than one (`config.Row.Also` — `worker_rpc_timeout_seconds` is
 *  the worker stream's and NATS's, `repository_renames` four things'). The one that failed is
 *  named inside `failed`, which the daemon writes as `<holder>: <error>`
 *  (`internal/daemon/holders.go`), so the text carries it once and correctly. */
export function describeVerdict(verdict: LegionSettingVerdict): {
  readonly failure: boolean;
  readonly text: string;
} {
  if (verdict.refused !== undefined) {
    return { failure: true, text: `Refused: ${verdict.refused}` };
  }
  if (verdict.failed !== undefined) {
    return {
      failure: true,
      text: `The daemon took the new value, and what holds it kept the old one: ${verdict.failed}`,
    };
  }
  switch (verdict.applied) {
    case "at-once":
      return { failure: false, text: "Applied at once" };
    case "rebuilt":
      return { failure: false, text: "Applied by rebuilding what held it" };
    case "next-launch":
      return { failure: false, text: "Applied at the next worker launch" };
    case undefined:
      return { failure: false, text: "The daemon named no result for this setting" };
    default:
      return { failure: false, text: `Applied: ${verdict.applied}` };
  }
}
