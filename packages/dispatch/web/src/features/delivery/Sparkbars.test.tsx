import { afterEach, expect, test } from "bun:test";
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { chartBar } from "../../theme/classes";
import { formatDuration, pct } from "./lib/format";
import type { Judge, Target, WriteFigure } from "./lib/target";
import { type SparkbarPoint, Sparkbars } from "./Sparkbars";
import { type SpreadBarDay, SpreadBars } from "./SpreadBars";

afterEach(cleanup);

/** One bar a chart draws, read off the subpath of its fill's `<path>` that draws it
 *  (`barPath`), answering as a bar's own `<rect>` would: its place and size (`x`, `y`, `width`,
 *  `height`), and its path's class, fade (`opacity`) and names. */
interface Bar {
  getAttribute(name: string): string | null;
}

/** The drawn bars of a chart, in the order of its days, a day's bars in the order drawn. */
function bars(svg: Element): Bar[] {
  const drawn: { at: number; order: number; bar: Bar }[] = [];
  for (const path of svg.querySelectorAll("path")) {
    const subpaths = (path.getAttribute("d") ?? "").matchAll(
      /M(-?[\d.]+) (-?[\d.]+)h(-?[\d.]+)v(-?[\d.]+)h-?[\d.]+z/g
    );
    for (const [, x = "", y = "", width = "", height = ""] of subpaths) {
      const own: Record<string, string> = { height, width, x, y };
      drawn.push({
        at: Number(x),
        bar: { getAttribute: (name) => own[name] ?? path.getAttribute(name) },
        order: drawn.length,
      });
    }
  }
  return drawn.sort((a, b) => a.at - b.at || a.order - b.order).map(({ bar }) => bar);
}

/** The chart's title with the pointer over each of its `count` days in turn, as it lays them
 *  across its width: the one title it draws, of the day under the pointer. */
async function titles(svg: Element, count: number): Promise<string[]> {
  svg.getBoundingClientRect = () => new DOMRect(0, 0, count * 10, 28);
  const user = userEvent.setup();
  const read: string[] = [];
  for (let day = 0; day < count; day += 1) {
    await user.pointer({ coords: { clientX: day * 10 + 5 }, target: svg });
    read.push(...[...svg.querySelectorAll("title")].map((title) => title.textContent ?? ""));
  }
  await user.pointer({ target: document.body });
  return read;
}

/** What each drawn bar names: its eval, its statistic and its span. */
function named(svg: Element): (string | null)[][] {
  return bars(svg).map((bar) => [
    bar.getAttribute("data-eval"),
    bar.getAttribute("data-statistic"),
    bar.getAttribute("data-span"),
  ]);
}

/** A day's deploys as the number itself, exact whatever decimals it is offered. */
function asNumber(value: number): WriteFigure {
  return () => `${value}/day`;
}

/** A target of `value`, at least it unless `over` says otherwise, judging `judges`. */
function target(
  value: number,
  judges: Judge[],
  over: Partial<NonNullable<Target>> = {}
): NonNullable<Target> {
  return { value, inclusive: true, direction: "higher_better", judges, ...over };
}

/** The deploys chart judged by deploy-frequency's target, at least 20 deploys a day. */
const deploysJudged = {
  evalSlug: "deploy-frequency",
  statistic: "deploys.per_day",
  target: target(20, ["deploys.per_day"]),
} as const;

const points: SparkbarPoint[] = [
  { label: "2026-09-25", value: 25, partial: false },
  { label: "2026-09-26", value: 10, partial: false },
  { label: "2026-09-27", value: null, partial: true },
];

test("Sparkbars draws a bar per day and one dashed target line", () => {
  render(<Sparkbars judged={deploysJudged} max={0} points={points} write={asNumber} />);
  const svg = screen.getByRole("img", { name: "per-day trend" });

  expect(svg.getAttribute("viewBox")).toBe("0 0 30 28");
  expect(svg.getAttribute("preserveAspectRatio")).toBe("none");
  expect(bars(svg)).toHaveLength(3);
  const lines = svg.querySelectorAll("line");
  expect(lines).toHaveLength(1);
  const line = lines[0];
  expect(line?.getAttribute("class")).toBe(chartBar.targetLine);
  expect(line?.getAttribute("stroke-dasharray")).toBe("3 2");
  // The scale's top is the largest of max, 1.25 x the target and the tallest bar: 25 here.
  expect(Number(line?.getAttribute("y1"))).toBeCloseTo(28 - (20 / 25) * 28);
});

test("Sparkbars colours a day whose value meets the target met and one whose value misses it missed", () => {
  render(<Sparkbars judged={deploysJudged} max={0} points={points} write={asNumber} />);
  const [meets, misses] = bars(screen.getByRole("img", { name: "per-day trend" }));

  expect(meets?.getAttribute("class")).toBe(chartBar.met);
  expect(meets?.getAttribute("height")).toBe("28");
  expect(misses?.getAttribute("class")).toBe(chartBar.missed);
  expect(Number(misses?.getAttribute("height"))).toBeCloseTo((10 / 25) * 28);
});

test("Sparkbars draws a one-pixel stub on a day with nothing to measure", async () => {
  render(<Sparkbars judged={deploysJudged} max={0} points={points} write={asNumber} />);
  const svg = screen.getByRole("img", { name: "per-day trend" });
  const stub = bars(svg)[2];

  expect(stub?.getAttribute("class")).toBe(chartBar.empty);
  expect(stub?.getAttribute("height")).toBe("1");
  expect(stub?.getAttribute("y")).toBe("27");
  expect(await titles(svg, 3)).toEqual([
    "2026-09-25: 25/day",
    "2026-09-26: 10/day",
    "2026-09-27 (partial day): no runs",
  ]);
  expect(stub?.getAttribute("opacity")).toBe("0.4");
});

// R3-Q1: the chart judges each bar itself, from the target and the statistic it draws, in the
// target's own direction, rather than by where the bar stands against the line.
test("Sparkbars judges each bar by its own value in its target's direction: under 'at most 20' a tall bar misses and a short one meets, and each bar names its eval and statistic", () => {
  render(
    <Sparkbars
      judged={{
        ...deploysJudged,
        target: target(20, ["deploys.per_day"], { direction: "lower_better" }),
      }}
      max={0}
      points={[
        { label: "2026-09-25", value: 25, partial: false },
        { label: "2026-09-26", value: 10, partial: false },
        { label: "2026-09-27", value: 20, partial: false },
      ]}
      write={asNumber}
    />
  );
  const svg = screen.getByRole("img", { name: "per-day trend" });

  expect(bars(svg).map((bar) => bar.getAttribute("class"))).toEqual([
    chartBar.missed,
    chartBar.met,
    chartBar.met,
  ]);
  expect(named(svg)).toEqual([
    ["deploy-frequency", "deploys.per_day", "day"],
    ["deploy-frequency", "deploys.per_day", "day"],
    ["deploy-frequency", "deploys.per_day", "day"],
  ]);
});

test("Sparkbars no eval judges, or judged by a target of another statistic or none, draws every bar neutral and no line", () => {
  render(<Sparkbars max={30} points={points.slice(0, 2)} write={asNumber} />);
  const svg = screen.getByRole("img", { name: "per-day trend" });

  expect(bars(svg).map((bar) => bar.getAttribute("class"))).toEqual([
    chartBar.neutral,
    chartBar.neutral,
  ]);
  expect(svg.querySelectorAll("line")).toHaveLength(0);
  // With no target the top of the scale is max (30), above the tallest bar (25).
  expect(Number(bars(svg)[0]?.getAttribute("height"))).toBeCloseTo((25 / 30) * 28);
  // A chart no eval judges names no statistic.
  expect(svg.querySelectorAll("[data-statistic]")).toHaveLength(0);
  cleanup();

  // The window's count of deploys judges no day, and an off target judges nothing.
  for (const judging of [target(20, ["deploys.deploys"]), null]) {
    render(
      <Sparkbars
        judged={{ ...deploysJudged, target: judging }}
        points={points.slice(0, 2)}
        write={asNumber}
      />
    );
    const judged = screen.getByRole("img", { name: "per-day trend" });
    expect(bars(judged).map((bar) => bar.getAttribute("class"))).toEqual([
      chartBar.neutral,
      chartBar.neutral,
    ]);
    expect(judged.querySelectorAll("line")).toHaveLength(0);
    cleanup();
  }
});

test("Sparkbars with no max scales a judged chart to its target line, and one with no line to 1 or its tallest bar", () => {
  const low: SparkbarPoint[] = [{ label: "2026-09-25", value: 0.5, partial: false }];
  render(
    <Sparkbars
      judged={{ ...deploysJudged, target: target(0.4, ["deploys.per_day"]) }}
      points={low}
      write={asNumber}
    />
  );
  // The top is the line, 0.4, raised to 1.25 x it, 0.5, which the bar reaches.
  expect(bars(screen.getByRole("img", { name: "per-day trend" }))[0]?.getAttribute("height")).toBe(
    "28"
  );
  cleanup();

  render(<Sparkbars points={low} write={asNumber} />);
  // No line: the top is 1, which the bar reaches half of.
  expect(
    Number(bars(screen.getByRole("img", { name: "per-day trend" }))[0]?.getAttribute("height"))
  ).toBeCloseTo(14);
});

// R3-C1 (dispatch://INFRA-2916, test v13): a toned bar's title is the one figure it writes, so it
// is written on its value's side of the target, by the verdict its fill takes (`judgeFigure`): a
// day's run success of 0.9504 above a strict 95% is green, and titled 95.04%, never 95.0%.
test("Sparkbars writes a toned bar's title on its value's side of a strict target, and an untoned bar's as usual", async () => {
  const days: SparkbarPoint[] = [
    { label: "2026-09-25", value: 0.9504, partial: false },
    { label: "2026-09-26", value: 0.9496, partial: false },
    { label: "2026-09-27", value: 0.95, partial: false },
  ];
  const chart = async (target?: Target) => {
    render(
      <Sparkbars
        judged={
          target === undefined
            ? undefined
            : { evalSlug: "deploy-run-success", statistic: "runs.rate", target }
        }
        max={1}
        points={days}
        write={(value) => (extra) => pct(value, 1 + extra)}
      />
    );
    const svg = screen.getByRole("img", { name: "per-day trend" });
    const drawn = {
      bars: bars(svg).map((bar) => bar.getAttribute("class")),
      titles: await titles(svg, days.length),
    };
    cleanup();
    return drawn;
  };

  // Above 95%: the day over it meets, and its usual 95.0% would read as the target itself, missed;
  // the day under it and the day on it miss, and their usual 95.0% reads missed too.
  expect(await chart(target(0.95, ["runs.rate"], { inclusive: false }))).toEqual({
    bars: [chartBar.met, chartBar.missed, chartBar.missed],
    titles: ["2026-09-25: 95.04%", "2026-09-26: 95.0%", "2026-09-27: 95.0%"],
  });
  // Under 95%: the other way round.
  expect(
    await chart(target(0.95, ["runs.rate"], { inclusive: false, direction: "lower_better" }))
  ).toEqual({
    bars: [chartBar.missed, chartBar.met, chartBar.missed],
    titles: ["2026-09-25: 95.0%", "2026-09-26: 94.96%", "2026-09-27: 95.0%"],
  });
  // No eval judges the chart, or its target judges a statistic no day is judged of: each bar is
  // neutral and titled with its usual digits.
  for (const untoned of [undefined, target(0.95, ["runs.concluded"], { inclusive: false })]) {
    expect(await chart(untoned)).toEqual({
      bars: [chartBar.neutral, chartBar.neutral, chartBar.neutral],
      titles: ["2026-09-25: 95.0%", "2026-09-26: 95.0%", "2026-09-27: 95.0%"],
    });
  }
});

/** A day's minutes as the number itself, exact whatever decimals it is offered. */
function asMinutes(minutes: number): WriteFigure {
  return () => `${minutes}m`;
}

/** A full day with `median` and `p90` minutes, which a test dates. */
function dayOf(median: number, p90: number): SpreadBarDay {
  return { day: "", n: 3, median_minutes: median, p90_minutes: p90, partial: false };
}

/** A lead-time chart judged by merge-to-production's target, under 45m, of `judges`. */
function leadJudged(
  judges: Judge[],
  over: Partial<NonNullable<Target>> = {}
): { evalSlug: string; target: Target } {
  return {
    evalSlug: "merge-to-production",
    target: target(45, judges, { inclusive: false, direction: "lower_better", ...over }),
  };
}

const days: SpreadBarDay[] = [
  { day: "2026-09-25", n: 4, median_minutes: 30, p90_minutes: 40, partial: false },
  { day: "2026-09-26", n: 3, median_minutes: 50, p90_minutes: 80, partial: false },
  { day: "2026-09-27", n: 0, median_minutes: null, p90_minutes: null, partial: true },
];

/** The median and the p90 under 45m. */
const bothUnder = leadJudged(["minutes.median", "minutes.p90"]);

test("SpreadBars draws the p90 behind the median each day and one dashed target line", () => {
  render(<SpreadBars days={days} judged={bothUnder} noun="runs" write={asMinutes} />);
  const svg = screen.getByRole("img", { name: "per-day median and p90" });

  expect(svg.getAttribute("viewBox")).toBe("0 0 36 36");
  expect(svg.getAttribute("preserveAspectRatio")).toBe("none");
  // Two bars on each measured day and the stub on the empty one.
  expect(bars(svg)).toHaveLength(5);
  const [p90, median] = bars(svg);
  // Drawn later is drawn in front: the p90 first, the median over it.
  expect(Number(p90?.getAttribute("height"))).toBeCloseTo((40 / 80) * 36);
  expect(Number(median?.getAttribute("height"))).toBeCloseTo((30 / 80) * 36);
  const lines = svg.querySelectorAll("line");
  expect(lines).toHaveLength(1);
  expect(lines[0]?.getAttribute("class")).toBe(chartBar.targetLine);
  expect(Number(lines[0]?.getAttribute("y1"))).toBeCloseTo(36 - (45 / 80) * 36);
});

test("SpreadBars colours each bar met where its own value meets the target and missed where it misses it", () => {
  render(<SpreadBars days={days} judged={bothUnder} noun="runs" write={asMinutes} />);
  const [p90Under, medianUnder, p90Over, medianOver] = bars(
    screen.getByRole("img", { name: "per-day median and p90" })
  );

  expect(p90Under?.getAttribute("class")).toBe(chartBar.metFaint);
  expect(medianUnder?.getAttribute("class")).toBe(chartBar.met);
  expect(p90Over?.getAttribute("class")).toBe(chartBar.missedFaint);
  expect(medianOver?.getAttribute("class")).toBe(chartBar.missed);
});

// R3-Q1: the chart judges each bar itself, in the target's own direction and inclusiveness,
// rather than by where the bar stands against the line.
test("SpreadBars judges each bar in its target's direction: under 'above 45m' bars under the line miss, bars over it meet, and bars at a strict target miss", () => {
  render(
    <SpreadBars
      days={[
        { ...dayOf(30, 40), day: "2026-09-25" },
        { ...dayOf(50, 80), day: "2026-09-26" },
        { ...dayOf(45, 45), day: "2026-09-27" },
      ]}
      judged={leadJudged(["minutes.median", "minutes.p90"], { direction: "higher_better" })}
      noun="runs"
      write={asMinutes}
    />
  );

  expect(
    bars(screen.getByRole("img", { name: "per-day median and p90" })).map((bar) =>
      bar.getAttribute("class")
    )
  ).toEqual([
    chartBar.missedFaint,
    chartBar.missed,
    chartBar.metFaint,
    chartBar.met,
    chartBar.missedFaint,
    chartBar.missed,
  ]);
});

// R5: a target may judge a day's median and not its p90, or judge both and see one meet and the
// other miss, so each of a day's two bars takes its own verdict.
test("SpreadBars gives each of a day's two bars its own verdict: a neutral p90 beside a met median, a missed p90 beside a met median", () => {
  const apart = [
    { ...dayOf(30, 40), day: "2026-09-25" },
    { ...dayOf(30, 50), day: "2026-09-26" },
  ];
  render(
    <SpreadBars days={apart} judged={leadJudged(["minutes.median"])} noun="PRs" write={asMinutes} />
  );
  const medianOnly = screen.getByRole("img", { name: "per-day median and p90" });
  expect(bars(medianOnly).map((bar) => bar.getAttribute("class"))).toEqual([
    chartBar.neutralFaint,
    chartBar.met,
    chartBar.neutralFaint,
    chartBar.met,
  ]);
  // Each bar names the eval whose target may judge it and the statistic it draws, a day's value
  // of it.
  expect(named(medianOnly)).toEqual([
    ["merge-to-production", "minutes.p90", "day"],
    ["merge-to-production", "minutes.median", "day"],
    ["merge-to-production", "minutes.p90", "day"],
    ["merge-to-production", "minutes.median", "day"],
  ]);
  cleanup();

  render(<SpreadBars days={apart} judged={bothUnder} noun="PRs" write={asMinutes} />);
  expect(
    bars(screen.getByRole("img", { name: "per-day median and p90" })).map((bar) =>
      bar.getAttribute("class")
    )
  ).toEqual([chartBar.metFaint, chartBar.met, chartBar.missedFaint, chartBar.met]);
});

test("SpreadBars draws a stub on a day with nothing to measure, naming the noun and today", async () => {
  render(<SpreadBars days={days} judged={bothUnder} noun="runs" write={asMinutes} />);
  const svg = screen.getByRole("img", { name: "per-day median and p90" });
  const stub = bars(svg)[4];

  expect(stub?.getAttribute("class")).toBe(chartBar.empty);
  expect(stub?.getAttribute("height")).toBe("1");
  expect(stub?.getAttribute("y")).toBe("35");
  expect(await titles(svg, 3)).toEqual([
    "2026-09-25: median 30m, p90 40m, 4 runs",
    "2026-09-26: median 50m, p90 80m, 3 runs",
    "2026-09-27 (today so far): no runs",
  ]);
  expect(stub?.getAttribute("opacity")).toBe("0.55");
});

test("SpreadBars no eval judges, or judged by a target of a statistic no bar draws, draws the neutral pair and no line", async () => {
  render(<SpreadBars days={days.slice(0, 1)} noun="merges" write={asMinutes} />);
  const svg = screen.getByRole("img", { name: "per-day median and p90" });

  expect(bars(svg).map((bar) => bar.getAttribute("class"))).toEqual([
    chartBar.neutralFaint,
    chartBar.neutral,
  ]);
  expect(svg.querySelectorAll("line")).toHaveLength(0);
  expect(await titles(svg, 1)).toEqual(["2026-09-25: median 30m, p90 40m, 4 merges"]);
  // A chart no eval judges names no statistic.
  expect(svg.querySelectorAll("[data-statistic]")).toHaveLength(0);
  cleanup();

  // The longest change, which no bar draws.
  render(
    <SpreadBars
      days={days.slice(0, 1)}
      judged={leadJudged(["minutes.max"])}
      noun="merges"
      write={asMinutes}
    />
  );
  const longest = screen.getByRole("img", { name: "per-day median and p90" });
  expect(bars(longest).map((bar) => bar.getAttribute("class"))).toEqual([
    chartBar.neutralFaint,
    chartBar.neutral,
  ]);
  expect(longest.querySelectorAll("line")).toHaveLength(0);
});

// R3-C1 (dispatch://INFRA-2916, test v13): each of a day's two values in a bar's title is written
// on its value's side of the target by the verdict its own bar takes (`judgeFigure`): a median of
// 44.6m under a strict 45m is a green bar, titled 44.6m, never 45m.
test("SpreadBars writes each toned bar's value in its title on its value's side of a strict target, and an untoned bar's as usual", async () => {
  const spread: SpreadBarDay[] = [
    { ...dayOf(44.6, 44.6), day: "2026-09-25" },
    { ...dayOf(45.4, 44.6), day: "2026-09-26" },
  ];
  const chart = async (judged?: { evalSlug: string; target: Target }) => {
    render(
      <SpreadBars
        days={spread}
        judged={judged}
        noun="PRs"
        write={(minutes) => (extra) => formatDuration(minutes, extra)}
      />
    );
    const svg = screen.getByRole("img", { name: "per-day median and p90" });
    const drawn = {
      bars: bars(svg).map((bar) => bar.getAttribute("class")),
      titles: await titles(svg, spread.length),
    };
    cleanup();
    return drawn;
  };

  // Median under 45m: the median under it meets, and its usual 45m would read as the target
  // itself, missed; the median over it misses, as its 45m reads; the p90, which the target does
  // not judge, is neutral and written as usual.
  expect(await chart(leadJudged(["minutes.median"]))).toEqual({
    bars: [chartBar.neutralFaint, chartBar.met, chartBar.neutralFaint, chartBar.missed],
    titles: ["2026-09-25: median 44.6m, p90 45m, 3 PRs", "2026-09-26: median 45m, p90 45m, 3 PRs"],
  });
  // Median and p90 above 45m: each value by its own bar's verdict, the met one past the target.
  expect(
    await chart(leadJudged(["minutes.median", "minutes.p90"], { direction: "higher_better" }))
  ).toEqual({
    bars: [chartBar.missedFaint, chartBar.missed, chartBar.missedFaint, chartBar.met],
    titles: ["2026-09-25: median 45m, p90 45m, 3 PRs", "2026-09-26: median 45.4m, p90 45m, 3 PRs"],
  });
  // No eval judges the chart: every value as usual.
  expect((await chart()).titles).toEqual([
    "2026-09-25: median 45m, p90 45m, 3 PRs",
    "2026-09-26: median 45m, p90 45m, 3 PRs",
  ]);
});

/** 84 days, every one with a value, and as many compare days, two of them with none. */
const QUARTER = Array.from({ length: 84 }, (_, index) => ({
  day: `2026-07-${String(index).padStart(3, "0")}`,
  value: (index % 7) + 1,
}));

// dispatch://INFRA-2918, T33: the bars of one fill are one path, so a chart of 84 days with a
// compare range draws a few nodes, not six a day, and names its values on the group of each range.
test("an 84-day chart with a compare range draws one path a fill, and names each range's values on its group", async () => {
  const compare = QUARTER.map(({ value }, index) => (index < 2 ? null : value * 2));
  render(
    <Sparkbars
      compare={compare}
      judged={deploysJudged}
      marks={{
        chart: "deploy-frequency",
        series: "deploys",
        statistic: "per_day",
        x: QUARTER.map(({ day }) => day),
      }}
      points={QUARTER.map(({ day, value }) => ({ label: day, partial: false, value }))}
      write={asNumber}
    />
  );
  const svg = screen.getByRole("img", { name: "per-day trend" });
  // Inside the svg: the compare group and its path, the window group and its path of missed bars
  // (every value under the target of 20), and the target line.
  expect(svg.querySelectorAll("*")).toHaveLength(5);
  expect(bars(svg)).toHaveLength(84 + 82);
  expect(svg.querySelectorAll("[data-value]")).toHaveLength(0);
  const range = (name: string) => svg.querySelector(`[data-series-range="${name}"]`);
  expect(range("window")?.getAttribute("data-values")).toBe(
    QUARTER.map(({ value }) => value).join(" ")
  );
  expect(range("window")?.getAttribute("data-xs")).toBe(QUARTER.map(({ day }) => day).join(" "));
  expect(range("compare")?.getAttribute("data-values")).toBe(compare.slice(2).join(" "));
  expect(range("compare")?.getAttribute("data-xs")).toBe(
    QUARTER.slice(2)
      .map(({ day }) => day)
      .join(" ")
  );
  expect(range("compare")?.getAttribute("data-range")).toBe("compare");
  expect(range("window")?.getAttribute("data-statistic")).toBe("per_day");
  // No title until the pointer is over a day; then the one of that day, with its compare value.
  expect(svg.querySelectorAll("title")).toHaveLength(0);
  expect((await titles(svg, 84)).slice(0, 3)).toEqual([
    "2026-07-000: 1/day; compare no data",
    "2026-07-001: 2/day; compare no data",
    "2026-07-002: 3/day; compare 6/day",
  ]);
  expect(svg.querySelectorAll("title")).toHaveLength(0);
});

test("an 84-day spread with a compare range draws one path a fill and names each statistic's values", () => {
  render(
    <SpreadBars
      compare={QUARTER.map(({ value }) => ({ median: value, p90: value * 2 }))}
      days={QUARTER.map(({ day, value }) => ({
        day,
        median_minutes: value * 10,
        n: 2,
        p90_minutes: value * 20,
        partial: false,
      }))}
      judged={bothUnder}
      marks={{ chart: "merge-to-production", series: "minutes", statistic: "median", x: [] }}
      noun="PRs"
      write={asMinutes}
    />
  );
  const svg = screen.getByRole("img", { name: "per-day median and p90" });
  // Inside the svg: each compare statistic's group and path; the window's p90 group with its met
  // and missed paths and its median group with its met and missed paths; and the target line.
  expect(svg.querySelectorAll("*")).toHaveLength(11);
  const values = (range: string, statistic: string) =>
    svg
      .querySelector(`[data-series-range="${range}"][data-statistic="${statistic}"]`)
      ?.getAttribute("data-values");
  expect(values("window", "median")).toBe(QUARTER.map(({ value }) => value * 10).join(" "));
  expect(values("window", "p90")).toBe(QUARTER.map(({ value }) => value * 20).join(" "));
  expect(values("compare", "median")).toBe(QUARTER.map(({ value }) => value).join(" "));
  expect(values("compare", "p90")).toBe(QUARTER.map(({ value }) => value * 2).join(" "));
});

// While the Evals tab holds a section's charts back, each chart draws its frame alone: the same
// element at its size, with no bar, line or title in it.
test("a chart not drawn yet draws its frame alone, and the same chart drawn fills it", async () => {
  const props = {
    days: QUARTER.map(({ day, value }) => ({
      day,
      median_minutes: value,
      n: 1,
      p90_minutes: value,
      partial: false,
    })),
    noun: "PRs",
    write: asMinutes,
  };
  const { rerender } = render(<SpreadBars {...props} drawn={false} />);
  const spread = screen.getByRole("img", { name: "per-day median and p90" });
  expect(spread.childElementCount).toBe(0);
  expect(spread.getAttribute("class")).toBe("block h-9 w-full");
  spread.getBoundingClientRect = () => new DOMRect(0, 0, 840, 36);
  await userEvent.setup().pointer({ coords: { clientX: 5 }, target: spread });
  expect(spread.querySelectorAll("title")).toHaveLength(0);
  rerender(<SpreadBars {...props} />);
  expect(screen.getByRole("img", { name: "per-day median and p90" })).toBe(spread);
  expect(bars(spread)).toHaveLength(168);
  cleanup();

  const points = QUARTER.map(({ day, value }) => ({ label: day, partial: false, value }));
  const again = render(<Sparkbars drawn={false} points={points} write={asNumber} />);
  const trend = screen.getByRole("img", { name: "per-day trend" });
  expect(trend.childElementCount).toBe(0);
  expect(trend.getAttribute("class")).toBe("w-full h-7 block");
  again.rerender(<Sparkbars points={points} write={asNumber} />);
  expect(bars(trend)).toHaveLength(84);
});
