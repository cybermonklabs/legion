/** Whether a visual seed keeps every day heading its rows shoot on the day the baselines show.
 *
 *  The Activity tab and the priority margin head each day of events "Today", "Yesterday" or the
 *  date written out, against the page's own `new Date()` (`web/src/features/activity/
 *  activity-model.ts`, `dayLabel`). In the visual rows that clock is pinned a minute after the
 *  seed was written (`seededAt`, e2e/visual.e2e.ts) and runs on for as long as the row does, so
 *  a seed written in the last minutes of a UTC day draws its own events under "Yesterday": run
 *  44641726709 seeded at 23:59:46Z, its rows read the page's day as 00:00:46Z's, and the
 *  priority and issue-activity shots each differed from their baselines by that one heading.
 *  The rows take their shots in UTC (the visual projects' `timezoneId`, e2e/playwright.config.ts),
 *  so the day here is the UTC day. */

/** The minute after `seededAt` at which each visual row pins its page clock (e2e/visual.e2e.ts),
 *  so every relative label reads the same gap from the seed on every run. */
export const CLOCK_AFTER_SEED_MS = 60_000;

/** How far past `seededAt` a row's page clock can run: the minute it is pinned after the seed
 *  and the 60 s every row is given, which is `testTimeout` in e2e/playwright.config.ts. Written
 *  out rather than imported, because the config's own imports would then come into the seed
 *  script that reads this file. */
export const ROW_CLOCK_REACH_MS = CLOCK_AFTER_SEED_MS + 60_000;

export function utcDay(ms: number): string {
  return new Date(ms).toISOString().slice(0, 10);
}

/** `null` when a seed that began writing at `startedAt` and was written at `seededAt` keeps its
 *  first write and the furthest moment a row's clock reaches on one UTC day; else the midnight
 *  between them, after which a new seed has the whole day ahead of it. The midnight can already
 *  be past when the seed itself straddled it. */
export function midnightToSeedAfter(startedAt: number, seededAt: number): number | null {
  const reach = seededAt + ROW_CLOCK_REACH_MS;
  const day = utcDay(reach);
  return utcDay(startedAt) === day ? null : Date.parse(`${day}T00:00:00Z`);
}
