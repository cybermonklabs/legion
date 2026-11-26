import { expect, test } from "bun:test";
import { formatDuration, formatMinutes, pct } from "./format";
import {
  type Judge,
  judgeFigure,
  judgesStatistic,
  meetsTarget,
  ownVerdict,
  shownJudges,
  type Target,
  targetLabel,
  targetLine,
} from "./target";

/** The statistics each chart's bars draw a day's value of: the deploys chart's deploys a day, the
 *  run success chart's success rate, and a lead-time chart's median and p90 (`SpreadBars`). */
const DEPLOYS: Judge[] = ["deploys.per_day"];
const RUN_SUCCESS: Judge[] = ["runs.rate"];
const LEAD_TIMES: Judge[] = ["minutes.median", "minutes.p90"];

/** deploy-frequency's target as the catalogue seeds it, at least 20 deploys a day, with `over`. */
function deploys(over: Partial<NonNullable<Target>> = {}): NonNullable<Target> {
  return {
    value: 20,
    inclusive: true,
    direction: "higher_better",
    judges: ["deploys.per_day"],
    ...over,
  };
}

/** A lead-time target, under 45 minutes, judging `judges`, with `over`. */
function minutes(judges: Judge[], over: Partial<NonNullable<Target>> = {}): NonNullable<Target> {
  return { value: 45, inclusive: false, direction: "lower_better", judges, ...over };
}

test("targetLabel: the target's own direction phrases it, at least or above for higher_better, at most or under for lower_better, and off where the target is", () => {
  expect(targetLabel(deploys())).toBe("\u2265 20/day");
  expect(targetLabel(deploys({ inclusive: false }))).toBe("> 20/day");
  // The same value and judge under the other direction read the other way, with no argument.
  expect(targetLabel(deploys({ direction: "lower_better" }))).toBe("\u2264 20/day");
  expect(targetLabel(deploys({ direction: "lower_better", inclusive: false }))).toBe("< 20/day");
  expect(targetLabel(minutes(["minutes.median"], { inclusive: true }))).toBe("median \u2264 45m");
  expect(targetLabel(minutes(["minutes.median"]))).toBe("median < 45m");
  expect(targetLabel(null)).toBe("off");
});

test("a caption names the statistics the target judges, in the order the server judges them, each value in its statistic's unit", () => {
  expect(targetLabel(minutes(["minutes.p90"], { value: 720, inclusive: true }))).toBe(
    "p90 \u2264 720m"
  );
  // Two statistics neither of which implies the other are both named.
  expect(targetLabel(minutes(["minutes.median", "minutes.p90"]))).toBe("median and p90 < 45m");
  // A count is no duration, and a deploy with a PR is no deploy.
  expect(targetLabel(minutes(["minutes.n"], { value: 5, direction: "higher_better" }))).toBe(
    "count > 5"
  );
  expect(targetLabel(deploys({ judges: ["deploys.with_prs_per_day"] }))).toBe(
    "deploys with a PR \u2265 20/day"
  );
  expect(targetLabel(deploys({ judges: ["deploys.with_prs"] }))).toBe(
    "deploys with a PR \u2265 20"
  );
  expect(
    targetLabel(
      deploys({
        value: 0.05,
        direction: "lower_better",
        judges: ["per_deploy.rate", "per_deploy.failed"],
      })
    )
  ).toBe("failure rate and deploys with a failure \u2264 5% per deploy");
});

test("a target judging its card's own figure alone names no statistic, and names it beside another", () => {
  expect(targetLabel(deploys({ value: 0.9, judges: ["runs.rate"] }))).toBe("\u2265 90%");
  expect(
    targetLabel(
      deploys({
        value: 0.05,
        inclusive: false,
        direction: "lower_better",
        judges: ["per_deploy.rate"],
      })
    )
  ).toBe("< 5% per deploy");
  expect(targetLabel(deploys({ judges: ["deploys.per_day", "deploys.with_prs_per_day"] }))).toBe(
    "deploys and deploys with a PR \u2265 20/day"
  );
});

test("a judge this page has no word for reads as the server wrote it, and a target with no judges as its value", () => {
  // A server newer than the page's contract can judge a statistic the contract does not name.
  const newer = "latency.p99" as string as Judge;
  expect(targetLabel(deploys({ judges: [newer] }))).toBe("latency.p99 \u2265 20");
  expect(targetLabel(deploys({ judges: null }))).toBe("\u2265 20");
  expect(shownJudges(deploys({ judges: null }))).toEqual([]);
  expect(shownJudges(null)).toEqual([]);
});

// Under lower_better a series' longest value meeting the target means its median and p90 meet it,
// its missing it misses the target whatever they do, and the three have a value or none together:
// the server's verdict over [max, median] is its verdict over [max], so the page names the max.
test("a judged max implies the same series' median and p90 under lower_better, and nothing under higher_better", () => {
  const both = minutes(["minutes.max", "minutes.median"]);
  expect(shownJudges(both)).toEqual(["minutes.max"]);
  expect(
    shownJudges(minutes(["minutes.p90", "minutes.n", "minutes.max", "minutes.median"]))
  ).toEqual(["minutes.n", "minutes.max"]);
  // The answer's own list is the eval's whole list, and every figure is judged by all of it.
  expect(both.judges).toEqual(["minutes.max", "minutes.median"]);
  expect(judgesStatistic(both, "minutes.median")).toBe(true);
  // No max is judged, or the max is another series': nothing is implied.
  expect(shownJudges(minutes(["minutes.median", "minutes.p90"]))).toEqual([
    "minutes.median",
    "minutes.p90",
  ]);
  // Another series' max, which the contract names for no delivery target: the rule reads the
  // series a judge is of, not its statistic alone.
  const otherMax = "job:lint.max" as string as Judge;
  expect(shownJudges(minutes([otherMax, "minutes.median"]))).toEqual([otherMax, "minutes.median"]);
  // Better higher, a longest value over the target says nothing of the median.
  const higher = minutes(["minutes.max", "minutes.median"], { direction: "higher_better" });
  expect(shownJudges(higher)).toEqual(["minutes.max", "minutes.median"]);
  expect(targetLabel(higher)).toBe("longest and median > 45m");

  // The caption follows the reduced list; the dashed line, as every tone, the whole list: the
  // median bars a target of the longest change and the median judges carry its line.
  expect(targetLabel(both)).toBe("< 45m for every change");
  expect(targetLine(both, ["minutes.median"])).toBe(45);
});

// The strings the page wrote at 3140ce34, before a target carried its judges, where
// MeasuresPanel.tsx and PipelinePage.tsx fixed each card's statistic by hand.
test("the catalogue's six targets read as the page wrote them before targets carried their judges", () => {
  const lower = { inclusive: false, direction: "lower_better" } as const;
  const merge = minutes(["minutes.max", "minutes.median"]);
  const opened = minutes(["minutes.median"], { value: 60 });
  expect([
    targetLabel(deploys()),
    targetLabel({ ...lower, value: 0.05, judges: ["per_deploy.rate"] }),
    targetLabel(merge),
    targetLabel(opened),
    targetLabel(deploys({ value: 0.9, judges: ["runs.rate"] })),
    targetLabel({ ...lower, value: 0, inclusive: true, judges: ["issues.value"] }),
  ]).toEqual([
    "\u2265 20/day",
    "< 5% per deploy",
    "< 45m for every change",
    "median < 60m",
    "\u2265 90%",
    "0",
  ]);
});

test("an inclusive target of 0 on an eval better lower reads as the value alone, since none goes below 0", () => {
  const unowned = deploys({ value: 0, direction: "lower_better", judges: ["issues.value"] });
  expect(targetLabel(unowned)).toBe("0");
  expect(targetLabel({ ...unowned, inclusive: false })).toBe("< 0");
  expect(targetLabel({ ...unowned, direction: "higher_better" })).toBe("\u2265 0");
});

// R3-C1, the architect's rule for review round 3: a target is a value a person set, so a caption
// writes it exactly. A rate's target rounded to a whole percent reads "> 76%" for 0.755, beside a
// run success of 75.6% its own tone says meets it.
test("a caption writes its target exactly: a rate as its percentage, 0.755 as 75.5% and 0.05 as 5%, with no float noise", () => {
  const success = (value: number, inclusive = true) =>
    deploys({ value, inclusive, judges: ["runs.rate"] });
  expect(targetLabel(success(0.755, false))).toBe("> 75.5%");
  expect(targetLabel(success(0.7602))).toBe("\u2265 76.02%");
  expect(targetLabel(success(0.9))).toBe("\u2265 90%");
  const failures = (value: number, judges: Judge[]) =>
    deploys({ value, inclusive: false, direction: "lower_better", judges });
  expect(targetLabel(failures(0.05, ["per_deploy.rate"]))).toBe("< 5% per deploy");
  expect(targetLabel(failures(0.0496, ["per_deploy.rate"]))).toBe("< 4.96% per deploy");
  // 0.07 x 100 is 7.000000000000001 in floating point: the noise is not the person's.
  expect(targetLabel(failures(0.07, ["per_deploy.rate", "per_deploy.total"]))).toBe(
    "failure rate and deploys < 7% per deploy"
  );
  // Minutes, deploys a day and counts are written as the number the target holds, exact already.
  expect(targetLabel(minutes(["minutes.median"], { value: 44.5 }))).toBe("median < 44.5m");
  expect(targetLabel(deploys({ value: 0.25 }))).toBe("\u2265 0.25/day");
  expect(targetLabel(minutes(["minutes.n"], { value: 3, direction: "higher_better" }))).toBe(
    "count > 3"
  );
});

// R3-C1 (dispatch://INFRA-2916, test v13): a rate's target is written as the digits it is stored
// with, its decimal point moved two places to the right as text. Scaled by 100 and rounded to
// twelve significant digits, 0.1 + 0.2 was captioned "≥ 30%" beside a value of 0.3 its tone says
// misses it, written "30.0%".
test("a caption writes a rate's target as the shortest text that reads back as it: its own digits, the decimal point moved two places", () => {
  const success = (value: number) => targetLabel(deploys({ value, judges: ["runs.rate"] }));
  expect(
    [0.755, 0.05, 0.07, 0.1 + 0.2, 0.7602339181286549, 1e-7, 1.5e-7, 0, 1].map(success)
  ).toEqual([
    "\u2265 75.5%",
    "\u2265 5%",
    "\u2265 7%",
    "\u2265 30.000000000000004%",
    "\u2265 76.02339181286549%",
    "\u2265 0.00001%",
    "\u2265 0.000015%",
    "\u2265 0%",
    "\u2265 100%",
  ]);
  expect(
    targetLabel(
      deploys({
        value: 0.1 + 0.2,
        direction: "lower_better",
        judges: ["per_deploy.rate", "per_deploy.total"],
      })
    )
  ).toBe("failure rate and deploys \u2264 30.000000000000004% per deploy");
});

test("a caption's rate target, read as a decimal and divided by 100 exactly, is the stored target, in no more digits than its own shortest text, for doubles all over [0, 1]", () => {
  // mulberry32, a small seeded generator, so every run draws the same doubles.
  let state = 0x2916;
  const draw = () => {
    state = (state + 0x6d2b79f5) | 0;
    let t = Math.imul(state ^ (state >>> 15), 1 | state);
    t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
    return (t ^ (t >>> 14)) >>> 0;
  };
  const bits = new DataView(new ArrayBuffer(8));
  const values = [0, 1, Number.MIN_VALUE, 0.7644000000000001];
  for (let i = 0; i < 2000; i++) {
    // Any double under 1 by its bits, so every exponent is drawn, the ones JavaScript writes in
    // e-notation among them.
    bits.setUint32(0, draw() % 0x3ff00000);
    bits.setUint32(4, draw());
    values.push(bits.getFloat64(0));
    // Any 53-bit fraction of 1, the long decimals a share computed from counts has.
    values.push(((draw() >>> 11) * 2 ** 32 + draw()) / 2 ** 53);
    // A short decimal, as a person sets one.
    const places = 1 + (draw() % 6);
    values.push(Number(`${draw() % 10 ** places}e-${places}`));
  }
  /** A decimal's significant digits: its digits less the point and the zeros either side. */
  const significant = (decimal: string) => decimal.replace(".", "").replace(/^0+|0+$/g, "");
  const misread: string[] = [];
  for (const value of values) {
    const caption = targetLabel(deploys({ value, judges: ["runs.rate"] }));
    const written = /^\u2265 ((?:0|[1-9]\d*)(?:\.\d*[1-9])?)%$/.exec(caption)?.[1];
    const [shortest = ""] = value.toExponential().split("e");
    if (
      written === undefined ||
      Number(`${written}e-2`) !== value ||
      significant(written) !== significant(shortest)
    ) {
      misread.push(`${value}: ${caption}`);
    }
  }
  expect(values).toHaveLength(6004);
  expect(misread).toEqual([]);
});

test("meetsTarget: a value at an inclusive target meets it, at a strict one misses it, in the target's own direction, and an off target judges nothing", () => {
  expect(meetsTarget(20, deploys())).toBe(true);
  expect(meetsTarget(20, deploys({ inclusive: false }))).toBe(false);
  expect(meetsTarget(21, deploys({ inclusive: false }))).toBe(true);
  expect(meetsTarget(45, minutes(["minutes.median"], { inclusive: true }))).toBe(true);
  expect(meetsTarget(45, minutes(["minutes.median"]))).toBe(false);
  expect(meetsTarget(44.9, minutes(["minutes.median"]))).toBe(true);
  // One value against one target value: the direction alone decides which side meets it.
  expect(meetsTarget(10, deploys())).toBe(false);
  expect(meetsTarget(10, deploys({ direction: "lower_better" }))).toBe(true);
  expect(meetsTarget(1, null)).toBeNull();
});

// R5 (1): an eval with no target, which a person turned off, judges nothing.
test("an off target judges no statistic: no figure, day or bar takes a verdict of it, and no chart draws its line", () => {
  for (const statistic of ["deploys.per_day", "minutes.median", "minutes.n", "issues.value"]) {
    expect(judgesStatistic(null, statistic)).toBe(false);
    expect(ownVerdict(null, statistic, "window", 1)).toBeNull();
    expect(ownVerdict(null, statistic, "day", 1)).toBeNull();
  }
  expect([DEPLOYS, RUN_SUCCESS, LEAD_TIMES].map((drawn) => targetLine(null, drawn))).toEqual([
    undefined,
    undefined,
    undefined,
  ]);
  // A target with no judges, as a server older than them answers, judges nothing either.
  expect(ownVerdict(deploys({ judges: null }), "deploys.per_day", "window", 25)).toBeNull();
  expect(targetLine(deploys({ judges: null }), DEPLOYS)).toBeUndefined();
});

// R5 (3): a figure takes a verdict only where the target's whole list of judges names its
// statistic, and then of its own value; a statistic no judge names takes none.
test("ownVerdict: a figure is judged by its own value where the target's whole list of judges names its statistic, and by nothing where no judge names it or it has no value", () => {
  const both = minutes(["minutes.max", "minutes.median"]);
  // The caption names the longest change alone; the median is judged all the same, by its own
  // value, whatever the longest change does.
  expect(shownJudges(both)).toEqual(["minutes.max"]);
  expect(ownVerdict(both, "minutes.median", "window", 30)).toBe(true);
  expect(ownVerdict(both, "minutes.median", "window", 45)).toBe(false);
  expect(ownVerdict(both, "minutes.max", "window", 52)).toBe(false);
  expect(ownVerdict(both, "minutes.max", "window", 40)).toBe(true);
  // No judge names the p90: no verdict, under the target or over it.
  expect(judgesStatistic(both, "minutes.p90")).toBe(false);
  expect(ownVerdict(both, "minutes.p90", "window", 10)).toBeNull();
  expect(ownVerdict(both, "minutes.p90", "day", 90)).toBeNull();
  expect(ownVerdict(both, "minutes.median", "window", null)).toBeNull();
  // In the target's own direction and inclusiveness.
  expect(ownVerdict(deploys(), "deploys.per_day", "window", 20)).toBe(true);
  expect(ownVerdict(deploys({ inclusive: false }), "deploys.per_day", "window", 20)).toBe(false);
  expect(ownVerdict(deploys({ direction: "lower_better" }), "deploys.per_day", "day", 25)).toBe(
    false
  );
});

// R5 (4): a day's value is judged only of a rate or a quantile of that day's samples; a count or
// a sum over the window judges the window's total, which no day is a share of.
test("ownVerdict: a day is judged only by a rate or a quantile of its samples, and a count or a sum only over the window", () => {
  const rates: [Judge, NonNullable<Target>, number, boolean][] = [
    ["deploys.per_day", deploys({ value: 1 }), 0, false],
    ["runs.rate", deploys({ value: 0.9, judges: ["runs.rate"] }), 1, true],
    ["minutes.median", minutes(["minutes.median"]), 30, true],
    ["minutes.p90", minutes(["minutes.p90"]), 50, false],
    ["minutes.max", minutes(["minutes.max"]), 30, true],
  ];
  for (const [statistic, target, value, met] of rates) {
    expect([statistic, ownVerdict(target, statistic, "day", value)]).toEqual([statistic, met]);
    expect([statistic, ownVerdict(target, statistic, "window", value)]).toEqual([statistic, met]);
  }
  const counts: [Judge, NonNullable<Target>, number, boolean][] = [
    ["deploys.deploys", deploys({ value: 1, judges: ["deploys.deploys"] }), 0, false],
    ["runs.concluded", deploys({ value: 2, judges: ["runs.concluded"] }), 3, true],
    [
      "runs.reached_production",
      deploys({ value: 2, judges: ["runs.reached_production"] }),
      1,
      false,
    ],
    ["runs.cancelled", minutes(["runs.cancelled"], { value: 2 }), 1, true],
    ["minutes.n", minutes(["minutes.n"], { value: 2, direction: "higher_better" }), 1, false],
    ["per_deploy.failed", minutes(["per_deploy.failed"], { value: 1 }), 0, true],
  ];
  for (const [statistic, target, value, met] of counts) {
    expect([statistic, ownVerdict(target, statistic, "day", value)]).toEqual([statistic, null]);
    expect([statistic, ownVerdict(target, statistic, "window", value)]).toEqual([statistic, met]);
  }
});

// R5 (5): a chart draws its dashed line exactly where its bars take a tone.
test("targetLine: a chart draws the target's value as its line exactly where its bars take a tone, where the target judges a day's statistic its bars draw", () => {
  expect(targetLine(deploys(), DEPLOYS)).toBe(20);
  expect(targetLine(deploys({ judges: ["deploys.per_day", "deploys.deploys"] }), DEPLOYS)).toBe(20);
  // The window's count of deploys and its deploys with a PR a day: no bar draws either.
  expect(targetLine(deploys({ judges: ["deploys.deploys"] }), DEPLOYS)).toBeUndefined();
  expect(targetLine(deploys({ judges: ["deploys.with_prs_per_day"] }), DEPLOYS)).toBeUndefined();
  expect(targetLine(deploys({ value: 0.9, judges: ["runs.rate"] }), RUN_SUCCESS)).toBe(0.9);
  expect(targetLine(deploys({ judges: ["runs.concluded"] }), RUN_SUCCESS)).toBeUndefined();
  // A lead time's bars draw its median and its p90, and no bar its longest change or its count.
  expect(targetLine(minutes(["minutes.max", "minutes.median"]), LEAD_TIMES)).toBe(45);
  expect(targetLine(minutes(["minutes.p90"]), LEAD_TIMES)).toBe(45);
  expect(targetLine(minutes(["minutes.max"]), LEAD_TIMES)).toBeUndefined();
  expect(targetLine(minutes(["minutes.n"]), LEAD_TIMES)).toBeUndefined();
  expect(targetLine(minutes(["minutes.n", "minutes.max"]), LEAD_TIMES)).toBeUndefined();
  // A judge the charts' list names that no day is judged of draws no line either.
  expect(targetLine(minutes(["minutes.n"]), ["minutes.n"])).toBeUndefined();
});

// R3-C1, the architect's rule of 2026-11-19 for review round 3: a toned figure's text lies on the
// same side of its target as its value, with more digits where its usual ones would not.
test("judgeFigure: a toned figure takes the fewest more decimals that print it on its value's side of the target, and a figure that takes no verdict keeps its usual ones", () => {
  const underFive = deploys({
    value: 0.05,
    inclusive: false,
    direction: "lower_better",
    judges: ["per_deploy.rate"],
  });
  const rate = (value: number) =>
    judgeFigure(underFive, "per_deploy.rate", "window", value, (extra) => pct(value, 1 + extra));
  // Just under 5%: one decimal would print 5.0%, the target itself.
  expect(rate(0.0496)).toEqual({ met: true, text: "4.96%" });
  expect(rate(0.049999)).toEqual({ met: true, text: "4.9999%" });
  // Not near it, or its usual digits already on its side: as usual.
  expect(rate(0.02)).toEqual({ met: true, text: "2.0%" });
  expect(rate(0.0449)).toEqual({ met: true, text: "4.5%" });
  // On the target itself, a strict target missed: the target itself is the value's side.
  expect(rate(0.05)).toEqual({ met: false, text: "5.0%" });

  // Just past an inclusive target better higher: 90.0% would read as meeting at least 90%.
  const success = deploys({ value: 0.9, judges: ["runs.rate"] });
  expect(
    judgeFigure(success, "runs.rate", "day", 0.8996, (extra) => pct(0.8996, 1 + extra))
  ).toEqual({ met: false, text: "89.96%" });

  // Minutes in each unit they are written in, read back as minutes.
  const underFortyFive = minutes(["minutes.max", "minutes.median"]);
  expect(
    judgeFigure(underFortyFive, "minutes.max", "window", 44.6, (extra) =>
      formatDuration(44.6, extra)
    )
  ).toEqual({ met: true, text: "44.6m" });
  expect(
    judgeFigure(underFortyFive, "minutes.median", "window", 44.4, (extra) =>
      formatMinutes(44.4, extra)
    )
  ).toEqual({ met: true, text: "44m" });
  const underADay = minutes(["minutes.median"], { value: 1440 });
  expect(
    judgeFigure(underADay, "minutes.median", "window", 1439.9, (extra) =>
      formatMinutes(1439.9, extra)
    )
  ).toEqual({ met: true, text: "23.998h" });
  // Deploys a day, written to one decimal.
  expect(
    judgeFigure(
      deploys(),
      "deploys.per_day",
      "window",
      19.96,
      (extra) => `${(19.96).toFixed(1 + extra)}/day`
    )
  ).toEqual({ met: false, text: "19.96/day" });

  // No judge names the p90, and an off target judges nothing: the usual digits, whatever they
  // print.
  expect(
    judgeFigure(underFortyFive, "minutes.p90", "window", 44.6, (extra) =>
      formatDuration(44.6, extra)
    )
  ).toEqual({ met: null, text: "45m" });
  expect(
    judgeFigure(null, "minutes.max", "window", 44.6, (extra) => formatDuration(44.6, extra))
  ).toEqual({ met: null, text: "45m" });
});

// R3-C1: a written figure is read back as the decimal it writes, exactly. Read back by a
// floating-point product, 95% as 95 x 0.01 is 0.9500000000000001, past a target of 0.95, so a value
// on its target read as past it and took decimals to the cap: "95.000000000000000000%".
test("judgeFigure: a value on its target reads back as the target itself, so its usual digits stand on the side of its own verdict", () => {
  const success = (value: number, over: Partial<NonNullable<Target>> = {}) =>
    deploys({ value, inclusive: false, judges: ["runs.rate"], ...over });
  const rate = (target: NonNullable<Target>) =>
    judgeFigure(target, "runs.rate", "window", target.value, (extra) =>
      pct(target.value, 1 + extra)
    );
  expect(rate(success(0.95))).toEqual({ met: false, text: "95.0%" });
  expect(rate(success(0.7, { inclusive: true, direction: "lower_better" }))).toEqual({
    met: true,
    text: "70.0%",
  });
  expect(rate(success(0.57))).toEqual({ met: false, text: "57.0%" });
  // 4.1h, 246 minutes: missed under "< 246m" and met under "<= 246m".
  for (const [inclusive, met] of [
    [false, false],
    [true, true],
  ] as const) {
    expect(
      judgeFigure(
        minutes(["minutes.median"], { value: 246, inclusive }),
        "minutes.median",
        "window",
        246,
        (extra) => formatMinutes(246, extra)
      )
    ).toEqual({ met, text: "4.1h" });
  }
});

// R3-C1: every tenth a rate or a lead time is written to reads back as the double of that decimal
// in its target's unit. A value equal to the target misses both a strict target above it and one
// below it, so a reading a hair over or under it would take decimals under one of the two.
test("judgeFigure: every tenth of 0-100.0%, of 0-240.0h and of 0-240.0d reads back as that decimal exactly", () => {
  const sides = (value: number, judges: Judge[]) =>
    (["higher_better", "lower_better"] as const).map((direction) =>
      deploys({ value, inclusive: false, direction, judges })
    );
  const misread: string[] = [];
  const sweep = (tenths: number, unit: string, value: (tenth: number) => number, judge: Judge) => {
    for (let tenth = 0; tenth <= tenths; tenth++) {
      const written = `${Math.floor(tenth / 10)}.${tenth % 10}${unit}`;
      const exact = value(tenth);
      for (const target of sides(exact, [judge])) {
        // Written as `written` alone: a figure that takes a decimal reads "past it".
        const { text } = judgeFigure(target, judge, "window", exact, (extra) =>
          extra === 0 ? written : "past it"
        );
        if (text !== written) misread.push(`${written} ${target.direction}`);
      }
    }
  };
  // The decimal itself in the target's unit: a rate's share, and a lead time's minutes.
  sweep(1000, "%", (tenth) => Number(`${tenth}e-3`), "runs.rate");
  sweep(2400, "h", (tenth) => tenth * 6, "minutes.median");
  sweep(2400, "d", (tenth) => tenth * 144, "minutes.median");
  expect(misread).toEqual([]);
});
