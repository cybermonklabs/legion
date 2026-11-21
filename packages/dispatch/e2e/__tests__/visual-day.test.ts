import { describe, expect, test } from "bun:test";

import { dayLabel } from "../../web/src/features/activity/activity-model";
import { midnightToSeedAfter, ROW_CLOCK_REACH_MS, utcDay } from "../visual-day";

const at = (iso: string): number => Date.parse(iso);

/** The heading a visual row draws over an event of the seed: the event's day against the day of
 *  the row's page clock at its furthest, as the Activity tab and the priority margin read it.
 *  Each day is the UTC day, which is the local day of a page drawn in UTC, as the rows are; so
 *  the reading does not move with the zone this test runs in. */
function headingAtReach(eventAt: number, seededAt: number): string {
  return dayLabel(utcDay(eventAt), utcDay(seededAt + ROW_CLOCK_REACH_MS));
}

describe("a visual seed keeps its day headings on the day it was written", () => {
  // Run 44641726709 (job 135974266127): its reverse-order seed was written at 23:59:46Z, and its
  // priority and issue-activity shots each read the seed's own events under "Yesterday".
  test("the seed of run 44641726709 is written again after its midnight", () => {
    const startedAt = at("2026-11-19T23:59:40Z");
    const seededAt = at("2026-11-19T23:59:46Z");
    expect(headingAtReach(startedAt, seededAt)).toBe("Yesterday");
    expect(midnightToSeedAfter(startedAt, seededAt)).toBe(at("2026-11-20T00:00:00Z"));
  });

  test("a seed whose rows' clocks all end before midnight is kept, and heads its events Today", () => {
    const startedAt = at("2026-11-19T23:57:50Z");
    const seededAt = at("2026-11-19T23:57:59.999Z");
    expect(midnightToSeedAfter(startedAt, seededAt)).toBeNull();
    expect(headingAtReach(startedAt, seededAt)).toBe("Today");
  });

  test("a row's clock that would reach midnight exactly is a day too far", () => {
    expect(midnightToSeedAfter(at("2026-11-19T23:57:55Z"), at("2026-11-19T23:58:00Z"))).toBe(
      at("2026-11-20T00:00:00Z")
    );
  });

  test("a seed that straddles midnight is written again at once", () => {
    const midnight = midnightToSeedAfter(at("2026-11-19T23:59:59Z"), at("2026-11-20T00:00:04Z"));
    expect(midnight).toBe(at("2026-11-20T00:00:00Z"));
  });

  test("the seed written again after midnight is kept, and heads its events Today", () => {
    const startedAt = at("2026-11-20T00:00:00.5Z");
    const seededAt = at("2026-11-20T00:00:07Z");
    expect(midnightToSeedAfter(startedAt, seededAt)).toBeNull();
    expect(headingAtReach(startedAt, seededAt)).toBe("Today");
  });
});
