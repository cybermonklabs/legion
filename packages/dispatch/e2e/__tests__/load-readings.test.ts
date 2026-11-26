/** The readers of `e2e/load-instruments.ts`, which turn the entries Chromium's performance
 *  observers give a load - long tasks, Element Timing, Event Timing, Resource Timing, the
 *  largest contentful paint, `web-vitals`' layout shift - and the frames the page's recorder
 *  stamps into the readings the load rows and the production script hold to their bounds. Each
 *  reader takes plain arrays of those entries, as a load's capture serialises them, so these
 *  cases write the entries out by hand, at the edges each rule draws. */
import { expect, test } from "bun:test";

import {
  actReading,
  type BoxEntry,
  boundHolds,
  boxesMoved,
  type FrameEntry,
  firstDrawing,
  firstScreenPaint,
  fourthSmallest,
  largestContentfulPaint,
  layoutShift,
  loadEnd,
  longestEvent,
  longestTaskAfter,
  type RequestSpan,
  type ResourceEntry,
  readingLine,
} from "../load-instruments";

/** A frame stamped `time` by `requestAnimationFrame` and read at `at`, with the header's foot at
 *  120 px of a 900 px viewport. */
function frame(time: number, marks: Partial<FrameEntry> = {}): FrameEntry {
  return { at: time + 1, foot: 120, height: 900, rows: false, time, viewRow: false, ...marks };
}

/** A read the page sent, as Resource Timing gives it. */
function read(path: string, startTime: number, responseEnd: number): ResourceEntry {
  return {
    duration: responseEnd - startTime,
    initiatorType: "fetch",
    name: `http://127.0.0.1:8080${path}`,
    requestStart: startTime,
    responseEnd,
    responseStart: responseEnd - 1,
    startTime,
  };
}

test("the load ends one second after the last task over 50 ms, and an open WebSocket is no request in flight", () => {
  const tasks = [
    { duration: 120, startTime: 100 },
    { duration: 80, startTime: 900 },
    // A task of 50 ms is no long task: the end counts only a task longer than 50 ms.
    { duration: 50, startTime: 1500 },
  ];
  const requests: RequestSpan[] = [
    { end: 700, kind: "request", start: 50, url: "http://127.0.0.1:8080/api/v1/issues/LAY-1" },
    // The page's live socket stays open for the page's life, and holds no load open.
    { end: null, kind: "websocket", start: 600, url: "ws://127.0.0.1:8080/ws/live" },
  ];
  expect(loadEnd(tasks, requests, 1979)).toBeNull();
  expect(loadEnd(tasks, requests, 1980)).toBe(1980);
  expect(loadEnd(tasks, requests, 9000)).toBe(1980);

  // A request still in flight holds the load open whatever time has passed.
  const open: RequestSpan = { end: null, kind: "request", start: 1000, url: "/api/v1/held" };
  expect(loadEnd(tasks, [...requests, open], 9000)).toBeNull();
  // A request that ends after the last long task moves the end a second past it.
  const late: RequestSpan = { end: 1400, kind: "request", start: 1000, url: "/api/v1/late" };
  expect(loadEnd(tasks, [...requests, late], 9000)).toBe(2400);
  // With nothing at all, the load ends one second after the navigation started.
  expect(loadEnd([], [], 1000)).toBe(1000);
});

test("the fourth smallest of five readings is what a bound is held to, and a load that gave none counts as over it", () => {
  expect(fourthSmallest([5, 1, 4, 2, 3])).toBe(4);
  expect(fourthSmallest([2500, 2600, 100, 2400, 3000])).toBe(2600);
  expect(fourthSmallest([100, null, 120, 130, 140])).toBe(140);
  expect(fourthSmallest([null, null, 1, 2, 3])).toBeNull();
  expect(() => fourthSmallest([1, 2, 3, 4])).toThrow(/five readings/);
});

test("a reading line names the rate, the five readings, the limit and the fourth smallest", () => {
  const holding = {
    bound: "first-screen.conversation.paint-ms",
    limit: 2500,
    rate: 2,
    readings: [1200, 1300.25, 1250, 2700, 1100],
  };
  expect(readingLine(holding)).toBe(
    "first-screen.conversation.paint-ms rate=2 readings=1200,1300.25,1250,2700,1100 limit=2500 fourth=1300.25 pass"
  );
  expect(boundHolds(holding)).toBe(true);

  const failing = { ...holding, readings: [2600, 2700, 100, 2800, 2550] };
  expect(readingLine(failing)).toBe(
    "first-screen.conversation.paint-ms rate=2 readings=2600,2700,100,2800,2550 limit=2500 fourth=2700 fail"
  );
  expect(boundHolds(failing)).toBe(false);

  // A load that gave no reading prints `none`, and two of them leave no fourth smallest.
  const unread = { ...holding, rate: 4, readings: [null, null, 1, 2, 3] };
  expect(readingLine(unread)).toBe(
    "first-screen.conversation.paint-ms rate=4 readings=none,none,1,2,3 limit=2500 fourth=none fail"
  );
  expect(boundHolds(unread)).toBe(false);

  // The bound is one word, so a reader of the line splits it on spaces.
  expect(() => readingLine({ ...holding, bound: "first screen" })).toThrow(/one word/);
  expect(() => readingLine({ ...holding, readings: [1, 2] })).toThrow(/five readings/);
});

test("the first drawings are the first frame that holds the view row's controls and the first that holds a row", () => {
  const frames = [
    frame(16),
    frame(33, { viewRow: true }),
    frame(50, { viewRow: true }),
    frame(66, { rows: true, viewRow: true }),
  ];
  const drawn = firstDrawing(frames);
  expect(drawn.viewRow?.time).toBe(33);
  expect(drawn.rows?.time).toBe(66);
  expect(firstDrawing([frame(16)])).toEqual({ rows: null, viewRow: null });
});

test("the first screen is painted at the latest paint of its rows, and a row with no paint is named", () => {
  expect(
    firstScreenPaint([
      { id: 1, label: "comment c-1", paint: 900 },
      { id: 2, label: "comment c-2", paint: 1400 },
      { id: 3, label: "child LAY-2", paint: 1100 },
    ])
  ).toEqual({ reading: 1400, unpainted: [] });
  expect(
    firstScreenPaint([
      { id: 1, label: "comment c-1", paint: 900 },
      { id: 2, label: "ask a-1", paint: null },
    ])
  ).toEqual({ reading: 900, unpainted: ["ask a-1"] });
  expect(firstScreenPaint([])).toEqual({ reading: null, unpainted: [] });
});

test("a change of a box in a frame where the typeface's loadingdone fired is the swap; any other change is a move naming the element and its box before and after", () => {
  const frames = [frame(16), frame(33), frame(50), frame(66), frame(83), frame(100)];
  const spec = { group: "view row's controls", id: 1, label: "Spec" };
  const row = { group: "rows in the viewport", id: 2, label: "comment c-1" };
  const box = (y: number) => ({ height: 20, width: 80, x: 10, y });
  const boxes: BoxEntry[] = [
    { ...spec, box: box(100), frame: 0 },
    { ...row, box: box(300), frame: 1 },
    // The typeface's face loaded between frame 1 and frame 3, so frame 2's change is its swap.
    { ...spec, box: box(101), frame: 2 },
    // Frame 4 is two frames past that loadingdone: both of its changes are moves, each named
    // with the read that answered since frame 3.
    { ...row, box: box(340), frame: 4 },
    { ...spec, box: box(130), frame: 4 },
    // After the load has ended a box may move: the reader reads up to the end it is given.
    { ...spec, box: box(150), frame: 5 },
  ];
  const moves = boxesMoved(
    {
      boxes,
      fontsDone: [45],
      frames,
      resource: [read("/api/v1/issues/LAY-1/tree?view=spec", 60, 70)],
    },
    90
  );
  expect(moves).toEqual([
    {
      after: box(340),
      at: 84,
      before: box(300),
      frame: 4,
      group: "rows in the viewport",
      label: "comment c-1",
      reads: ["/api/v1/issues/LAY-1/tree?view=spec"],
    },
    {
      after: box(130),
      at: 84,
      before: box(101),
      frame: 4,
      group: "view row's controls",
      label: "Spec",
      reads: ["/api/v1/issues/LAY-1/tree?view=spec"],
    },
  ]);
  // With no loadingdone at all, frame 2's change is a move too.
  expect(
    boxesMoved({ boxes, fontsDone: [], frames, resource: [] }, 90).map((move) => move.frame)
  ).toEqual([2, 4, 4]);
});

test("an act reads from its event's timeStamp to the first frame whose document holds its result, and a read sent after the event splits it in three", () => {
  const frames = [frame(990), frame(1006), frame(1040), frame(1200), frame(1400), frame(1500)];
  const acts = [
    {
      event: { timeStamp: 1000, type: "pointerdown" },
      loading: 2,
      measured: true,
      name: "a press on Spec",
      result: 5,
    },
  ];
  const resource = [
    // Sent with the load, before the press: it is no read of the press.
    read("/api/v1/issues/LAY-1", 900, 1200),
    read("/api/v1/issues/LAY-1/tree?view=spec", 1010, 1400),
  ];
  expect(actReading({ acts, frames, resource }, 0)).toEqual({
    act: "a press on Spec",
    loading: 40,
    read: { drawn: 100, sent: 10, url: "/api/v1/issues/LAY-1/tree?view=spec" },
    shown: 500,
  });

  // A press whose result needs no read: the reading is the event to the result's frame alone.
  expect(actReading({ acts, frames, resource: resource.slice(0, 1) }, 0)).toEqual({
    act: "a press on Spec",
    loading: 40,
    read: null,
    shown: 500,
  });

  // An act whose result no frame showed gives no reading.
  const unshown = [{ ...acts[0], loading: null, result: null }];
  expect(actReading({ acts: unshown, frames, resource: [] }, 0)).toEqual({
    act: "a press on Spec",
    loading: null,
    read: null,
    shown: null,
  });
  // An act the page never received gives none either.
  const unpressed = [{ ...acts[0], event: null, loading: null, result: null }];
  expect(actReading({ acts: unpressed, frames, resource }, 0).shown).toBeNull();
});

test("the longest task counts only tasks after the first drawing, and the longest event and paint are the load's largest", () => {
  const tasks = [
    { duration: 400, startTime: 200 },
    { duration: 120, startTime: 900 },
    { duration: 90, startTime: 1500 },
  ];
  expect(longestTaskAfter(tasks, 800)).toBe(120);
  expect(longestTaskAfter(tasks, 2000)).toBe(0);
  expect(
    longestEvent([
      {
        duration: 24,
        interactionId: 1,
        name: "pointerdown",
        processingEnd: 20,
        processingStart: 12,
        startTime: 10,
      },
      {
        duration: 96,
        interactionId: 2,
        name: "keydown",
        processingEnd: 110,
        processingStart: 34,
        startTime: 30,
      },
    ])
  ).toBe(96);
  expect(longestEvent([])).toBe(0);
  const paint = { element: "p", loadTime: 0, url: "" };
  const lcp = [
    { ...paint, renderTime: 700, size: 1000, startTime: 700 },
    { ...paint, renderTime: 1600, size: 9000, startTime: 1600 },
    { ...paint, renderTime: 4000, size: 9500, startTime: 4000 },
  ];
  expect(largestContentfulPaint(lcp, 3000)).toBe(1600);
  expect(largestContentfulPaint([], 3000)).toBeNull();
  expect(
    layoutShift([
      { delta: 0, shifts: [], value: 0 },
      {
        delta: 0.04,
        shifts: [{ hadRecentInput: false, startTime: 1200, value: 0.04 }],
        value: 0.04,
      },
    ])
  ).toBe(0.04);
  expect(layoutShift([])).toBeNull();
});
