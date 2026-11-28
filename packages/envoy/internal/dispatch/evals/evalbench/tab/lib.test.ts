// The split run's reading of Chromium's trace (lib.ts's mainThreadTasks and busyIn), over a trace
// written by hand in Chromium's JSON form; and the rule each time is judged by (INFRA-979's ruling,
// dispatch://INFRA-2918/comment/75af800e-9db9-47d9-9c4d-427b0cff36c0): the host's speed and each
// moment's judged time (speedOf, judge), the first screen (inFirstScreen, screenPaint) and a
// section box's size (boxChange).
import { describe, expect, test } from "bun:test";

import {
  boxChange,
  busyIn,
  inFirstScreen,
  judge,
  mainThreadTasks,
  type SectionPaint,
  screenPaint,
  speedOf,
} from "./lib";

const PATH = "/projects/INFRA/evals?window=84d";

/** A complete event of the page's main thread (pid 7, tid 1), times in microseconds. */
function span(name: string, ts: number, dur: number, data: Record<string, unknown> = {}) {
  return { name, ph: "X", ts, dur, pid: 7, tid: 1, args: { data } };
}

const trace = {
  traceEvents: [
    { name: "thread_name", ph: "M", ts: 0, pid: 7, tid: 1, args: { name: "CrRendererMain" } },
    // An earlier document of the same tab, and another process's navigation: neither is the page.
    {
      name: "navigationStart",
      ph: "R",
      ts: 400_000,
      pid: 7,
      tid: 1,
      args: { data: { documentLoaderURL: "http://127.0.0.1:58111/" } },
    },
    {
      name: "navigationStart",
      ph: "R",
      ts: 1_000_000,
      pid: 7,
      tid: 1,
      args: { data: { documentLoaderURL: `http://127.0.0.1:58111${PATH}` } },
    },
    span("RunTask", 900_000, 50_000),
    // A frame's message: its handler's script, a layout the script forces (a begin and an end), and
    // a garbage collection inside the script.
    span("RunTask", 1_010_000, 30_000),
    span("EventDispatch", 1_010_100, 20_000, { type: "message" }),
    span("FunctionCall", 1_010_200, 19_000),
    { name: "Layout", ph: "B", ts: 1_015_000, pid: 7, tid: 1, args: {} },
    { name: "Layout", ph: "E", ts: 1_019_000, pid: 7, tid: 1, args: {} },
    span("MinorGC", 1_025_000, 2_000),
    // A frame's drawing, with a task Chromium runs inside it.
    span("ThreadControllerImpl::RunTask", 1_050_000, 10_000),
    span("RunTask", 1_050_050, 100),
    span("UpdateLayoutTree", 1_050_200, 3_000),
    span("Paint", 1_053_200, 4_000),
    // A timer's task: what started it.
    span("RunTask", 1_070_000, 5_000),
    span("TimerFire", 1_070_100, 4_000),
    // Another thread's task is not the page's.
    { name: "RunTask", ph: "X", ts: 1_020_000, dur: 90_000, pid: 7, tid: 2, args: {} },
  ],
};

describe("mainThreadTasks", () => {
  test("every task of the page's main thread from its navigation, each by part and by what started it", () => {
    const read = mainThreadTasks(trace, PATH);
    expect(read?.events).toBe(trace.traceEvents.length);
    expect(read?.tasks).toEqual([
      {
        at: 10,
        ms: 30,
        parts: { script: 14, style_layout: 4, paint: 0, gc: 2, parse: 0, other: 10 },
        started: "message",
      },
      {
        at: 50,
        ms: 10,
        parts: { script: 0, style_layout: 3, paint: 4, gc: 0, parse: 0, other: 3 },
        started: "render",
      },
      {
        at: 70,
        ms: 5,
        parts: { script: 4, style_layout: 0, paint: 0, gc: 0, parse: 0, other: 1 },
        started: "timer",
      },
    ]);
  });

  test("a trace that marks no navigation of the page answers none", () => {
    expect(mainThreadTasks(trace, "/projects/OTHER/evals")).toBeNull();
    expect(mainThreadTasks({}, PATH)).toBeNull();
  });
});

describe("busyIn", () => {
  test("the tasks' time within a span, each task's parts in the share of it the span holds", () => {
    const tasks = mainThreadTasks(trace, PATH)?.tasks ?? [];
    expect(busyIn(tasks, 0, 55)).toEqual({
      ms: 35,
      parts: { script: 14, style_layout: 5.5, paint: 2, gc: 2, parse: 0, other: 11.5 },
    });
    expect(busyIn(tasks, 41, 49).ms).toBe(0);
  });
});

describe("speedOf and judge", () => {
  test("above 1500 the page is throttled at index ÷ 1500 and each time is judged as measured", () => {
    const speed = speedOf(2400);
    expect(speed).toEqual({ index: 2400, throttle: 1.6, scale: null });
    expect(judge(500, 300, speed)).toEqual({ raw: 500, judged: 500, waits: null, main: null });
    // Throttled, no main-thread time is needed.
    expect(judge(500, null, speed)?.judged).toBe(500);
  });

  test("at or below 1500 each time is its waits plus its main-thread time × index ÷ 1500", () => {
    const speed = speedOf(1200);
    expect(speed).toEqual({ index: 1200, throttle: null, scale: 0.8 });
    const judged = judge(500, 300, speed);
    expect(judged).toEqual({ raw: 500, judged: 440, waits: 200, main: 300 });
    // Not the main-thread time × 1500 ÷ index, the checks' negative control: 200 + 375.
    expect(judged?.judged).not.toBe(575);
    // The reference itself runs unthrottled and is judged as measured.
    expect(speedOf(1500)).toEqual({ index: 1500, throttle: null, scale: 1 });
    expect(judge(500, 300, speedOf(1500))?.judged).toBe(500);
  });

  test("a moment's main-thread time is the trace's busy time before it", () => {
    const tasks = mainThreadTasks(trace, PATH)?.tasks ?? [];
    // 35 ms busy of the first 55 (busyIn above), at index 750: 20 + 35 × 0.5.
    expect(judge(55, busyIn(tasks, 0, 55).ms, speedOf(750))).toEqual({
      raw: 55,
      judged: 37.5,
      waits: 20,
      main: 35,
    });
  });

  test("no index, no moment, or unthrottled no main-thread time: no judged time", () => {
    expect(speedOf(null)).toBeNull();
    expect(speedOf(0)).toBeNull();
    expect(speedOf(Number.NaN)).toBeNull();
    expect(judge(500, 300, null)).toBeNull();
    expect(judge(null, 300, speedOf(1200))).toBeNull();
    expect(judge(500, null, speedOf(1200))).toBeNull();
  });
});

describe("the first screen", () => {
  test("a section with any part in 1440×900 at the top of the page lies in it", () => {
    expect(inFirstScreen({ top: 0, bottom: 300, left: 0, right: 1440 })).toBe(true);
    expect(inFirstScreen({ top: 899, bottom: 1300, left: 16, right: 1424 })).toBe(true);
    expect(inFirstScreen({ top: 900, bottom: 1300, left: 16, right: 1424 })).toBe(false);
    expect(inFirstScreen({ top: 400, bottom: 400, left: 16, right: 1424 })).toBe(false);
  });

  test("the first paint ends once each section in it is drawn and each other shows its box", () => {
    const sections: SectionPaint[] = [
      { id: "cards", shown: 120, drawn: 120, inView: true },
      { id: "legion", shown: 150, drawn: 310, inView: true },
      { id: "ci", shown: 160, drawn: 1500, inView: false },
      { id: "deploys", shown: 170, drawn: 1200, inView: false },
    ];
    expect(screenPaint(sections)).toEqual({ firstPaint: 310, everySection: 1500 });
    // A section below the first screen that set no box mark counts at its drawing.
    expect(
      screenPaint([...sections, { id: "logged", shown: null, drawn: 900, inView: false }])
    ).toEqual({ firstPaint: 900, everySection: 1500 });
  });

  test("a section not drawn yet, or none at all, ends neither", () => {
    expect(
      screenPaint([
        { id: "cards", shown: 120, drawn: null, inView: true },
        { id: "ci", shown: 160, drawn: 1500, inView: false },
      ])
    ).toEqual({ firstPaint: null, everySection: null });
    expect(
      screenPaint([
        { id: "cards", shown: 120, drawn: 120, inView: true },
        { id: "ci", shown: 160, drawn: null, inView: false },
      ])
    ).toEqual({ firstPaint: 160, everySection: null });
    expect(screenPaint([])).toEqual({ firstPaint: null, everySection: null });
  });
});

describe("boxChange", () => {
  const loading = { at: 100, width: 1408, height: 40 };
  const box = { at: 150, width: 1408, height: 320 };

  test("a box drawn at the size it was first shown at has not changed", () => {
    expect(boxChange([loading, box], 170, 420)).toEqual({ shown: box, drawn: box, changed: false });
    // A section drawn at once, with no box before: the loading line's size is no box's.
    expect(boxChange([loading, box], 170, 170)).toEqual({ shown: box, drawn: box, changed: false });
  });

  test("a box that grows or shrinks when its section draws has changed", () => {
    const grown = { at: 410, width: 1408, height: 480 };
    expect(boxChange([loading, box, grown], 170, 420)).toEqual({
      shown: box,
      drawn: grown,
      changed: true,
    });
    // Back at its size by the drawing, it still moved in between.
    const wider = { at: 300, width: 1424, height: 320 };
    expect(boxChange([loading, box, wider, { ...box, at: 350 }], 170, 420).changed).toBe(true);
    // A change after the drawing is not the drawing's.
    expect(boxChange([loading, box, { ...grown, at: 500 }], 170, 420).changed).toBe(false);
  });

  test("a moment or a size not read leaves the change unread", () => {
    expect(boxChange([loading, box], undefined, 420)).toEqual({
      shown: null,
      drawn: box,
      changed: null,
    });
    expect(boxChange([box], 120, 420).changed).toBeNull();
    expect(boxChange([], 170, 420).changed).toBeNull();
  });
});
