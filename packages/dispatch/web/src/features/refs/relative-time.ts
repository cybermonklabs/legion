/**
 * The relative form of a time, "4 minutes ago" or "just now", as text.
 *
 * WHY A MODULE OF ITS OWN. The entry chunk draws one relative time: the state mark of the rail's
 * pinned agents says when a session was last seen (`SessionName.tsx`). The entry is what every
 * page parses before it draws anything (`web/src/asset-sizes-in-build.ts` holds it to its
 * budget). `<Timestamp>` (`Timestamp.tsx`) and its shared tick are drawn by the pages' chunks
 * alone, so the entry imports this rule and not that module.
 */

const relativeFormatter = new Intl.RelativeTimeFormat(undefined, { numeric: "auto" });

const RELATIVE_UNITS: Array<{ ms: number; unit: Intl.RelativeTimeFormatUnit }> = [
  { ms: 365 * 24 * 60 * 60 * 1000, unit: "year" },
  { ms: 30 * 24 * 60 * 60 * 1000, unit: "month" },
  { ms: 7 * 24 * 60 * 60 * 1000, unit: "week" },
  { ms: 24 * 60 * 60 * 1000, unit: "day" },
  { ms: 60 * 60 * 1000, unit: "hour" },
  { ms: 60 * 1000, unit: "minute" },
];

/** "4 minutes ago", "just now": the relative form every `<Timestamp>` shows, for a caller that
 *  needs it as text (an attribute). */
export function relativeTimeLabel(at: Date, now: number): string | undefined {
  const deltaMs = at.getTime() - now;
  if (Math.abs(deltaMs) < 60_000) {
    return "just now";
  }
  for (const { ms, unit } of RELATIVE_UNITS) {
    if (Math.abs(deltaMs) >= ms) {
      return relativeFormatter.format(Math.round(deltaMs / ms), unit);
    }
  }
}
