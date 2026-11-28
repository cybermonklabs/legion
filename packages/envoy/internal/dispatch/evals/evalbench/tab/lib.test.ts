// The split run's reading of Chromium's trace (lib.ts's mainThreadTasks and busyIn), over a trace
// written by hand in Chromium's JSON form.
import { describe, expect, test } from "bun:test";

import { busyIn, mainThreadTasks } from "./lib";

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
