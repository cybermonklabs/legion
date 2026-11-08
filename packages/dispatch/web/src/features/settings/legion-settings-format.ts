// What the Legion daemon settings control writes: a setting's value, where that value came from,
// and what the daemon decided about a change of it. Pure: no React, no cache.
import type { LegionSettingSource, LegionSettingVerdict } from "../../api/types";

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

/** What the daemon decided about one key of a change, and whether that is a refusal: `at-once`,
 *  `rebuilt` and `next-launch` are the three ways it applies one, and a refusal carries the
 *  daemon's own reason. A verdict word this build does not know is shown as the daemon wrote it,
 *  never dropped. */
export function describeVerdict(verdict: LegionSettingVerdict): {
  readonly refused: boolean;
  readonly text: string;
} {
  if (verdict.refused !== undefined) {
    return { refused: true, text: `Refused: ${verdict.refused}` };
  }
  switch (verdict.applied) {
    case "at-once":
      return { refused: false, text: "Applied at once" };
    case "rebuilt":
      return { refused: false, text: "Applied by rebuilding what held it" };
    case "next-launch":
      return { refused: false, text: "Applied at the next worker launch" };
    case undefined:
      return { refused: false, text: "The daemon named no result for this setting" };
    default:
      return { refused: false, text: `Applied: ${verdict.applied}` };
  }
}
