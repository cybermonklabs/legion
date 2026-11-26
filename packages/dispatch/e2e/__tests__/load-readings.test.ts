/** The readers of `e2e/load-instruments.ts`, which turn the entries Chromium's performance
 *  observers give a load - long tasks, Element Timing, Event Timing, Resource Timing, the
 *  largest contentful paint, `web-vitals`' layout shift - and the frames the page's recorder
 *  stamps into the readings the load rows and the production script hold to their bounds. Each
 *  reader takes plain arrays of those entries, as a load's capture serialises them. The first
 *  cases write the entries out by hand, at the edges each rule draws; the last read four real
 *  loads, which the capture row of `e2e/issue-page-load.e2e.ts` printed in a builds Job, from
 *  `fixtures/load-readings/`, and assert what each reader must take from them, never a bound. */
import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { errors, type Page } from "@playwright/test";

import {
  HEADER_SELECTORS,
  LOAD_ACTS,
  LOAD_ROW_SELECTOR,
  makeSteps,
  VIEW_ROW_SELECTOR,
} from "../issue-page-load";
import {
  actBounds,
  actReading,
  BOX_GROUPS,
  type BoxEntry,
  boundHolds,
  boxesMoved,
  type FrameEntry,
  firstDrawing,
  firstScreenPaint,
  fourthSmallest,
  groupsUndrawn,
  instrumentPage,
  type LoadCapture,
  largestContentfulPaint,
  layoutShift,
  loadEnd,
  longestEvent,
  longestTaskAfter,
  longestTaskAfterViewRow,
  NAMED_GROUP,
  NOT_APPLICABLE,
  type PageConfig,
  type RequestSpan,
  type ResourceEntry,
  ROWS_GROUP,
  readingLine,
  watchLoadRequests,
} from "../load-instruments";

/** A frame stamped `time` by `requestAnimationFrame`, read at `at` in its callback and shown 10 ms
 *  after its start, with the header's foot at 120 px of a 900 px viewport. */
function frame(time: number, marks: Partial<FrameEntry> = {}): FrameEntry {
  return {
    at: time + 1,
    foot: 120,
    height: 900,
    rows: false,
    shown: time + 10,
    time,
    viewRow: false,
    ...marks,
  };
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

test("a request's span runs from when the page sent it to when its answer or failure reached the page, on the page's timeline, whatever a route did with it", () => {
  // The page's `performance.timeOrigin`, and the wall clock as each of Playwright's reports
  // arrives: `at` ms into the page's life.
  const origin = 1_764_150_000_000;
  let clock = origin;
  const handlers = new Map<string, ((value: unknown) => void)[]>();
  const page = {
    on: (event: string, handler: (value: unknown) => void) => {
      handlers.set(event, [...(handlers.get(event) ?? []), handler]);
    },
  };
  const report = (event: string, value: unknown, at: number) => {
    clock = origin + at;
    for (const handler of handlers.get(event) ?? []) handler(value);
  };
  // A request with the timing Playwright gives it, which the span must not read.
  const request = (url: string, timing: { startTime: number; responseEnd: number }) => ({
    timing: () => timing,
    url: () => url,
  });
  const requests = watchLoadRequests(page as unknown as Page, () => clock);

  // A route continues the issue read, fulfils the identity read and aborts a write.
  const issue = request("http://127.0.0.1:8080/api/v1/issues/LAY-1", {
    responseEnd: 108,
    startTime: origin + 897,
  });
  const whoami = request("http://127.0.0.1:8080/auth/whoami", {
    responseEnd: -1,
    startTime: origin + 900,
  });
  const write = request("http://127.0.0.1:8080/api/v1/issues/LAY-1/read", {
    responseEnd: -1,
    startTime: origin + 1200,
  });
  // A route holds the typeface's file back 3 s, then continues it. Its timing is the one
  // Playwright gave the file in `typeface-held-back.json`: the start when the route let it go,
  // the end counted from when the page sent it, which together put it at 4,517 to 7,725 ms.
  const font = request("http://127.0.0.1:8080/assets/InterVariable-subset.woff2", {
    responseEnd: 3207.5,
    startTime: origin + 4517.25,
  });
  const socket = { on: () => {}, url: () => "ws://127.0.0.1:8080/ws/live" };
  report("request", issue, 897);
  report("request", whoami, 900);
  report("requestfinished", whoami, 903);
  report("websocket", socket, 968.5);
  report("requestfinished", issue, 1005);
  report("request", write, 1200);
  report("requestfailed", write, 1201);
  report("request", font, 1314.25);

  // While the route holds the file back, it is a request in flight, and the load has not ended.
  clock = origin + 2500;
  expect(requests.spans(origin).find((span) => span.url.endsWith(".woff2"))?.end).toBeNull();
  expect(loadEnd([], requests.spans(origin), 2500)).toBeNull();

  report("requestfinished", font, 4520.25);
  expect(requests.spans(origin)).toEqual([
    { end: 1005, kind: "request", start: 897, url: "http://127.0.0.1:8080/api/v1/issues/LAY-1" },
    { end: 903, kind: "request", start: 900, url: "http://127.0.0.1:8080/auth/whoami" },
    { end: null, kind: "websocket", start: 968.5, url: "ws://127.0.0.1:8080/ws/live" },
    {
      end: 1201,
      kind: "request",
      start: 1200,
      url: "http://127.0.0.1:8080/api/v1/issues/LAY-1/read",
    },
    {
      end: 4520.25,
      kind: "request",
      start: 1314.25,
      url: "http://127.0.0.1:8080/assets/InterVariable-subset.woff2",
    },
  ]);
  // The load ends a second after the file reached the page, not a second after 7,725 ms.
  expect(loadEnd([], requests.spans(origin), 9000)).toBe(5520.25);
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

test("an act reads from its event's timeStamp to the stamp of the frame that shows its result, taken after that frame's rendering, not to the frame's start", () => {
  // The press is handled inside the frame that starts at 990, before that frame's callbacks, so
  // its result is in the document when the callback looks: the frame's start is before the
  // event, and the stamp of its end, at 1030, is what the reading runs to.
  const frames = [frame(970), frame(990, { shown: 1030 }), frame(1040)];
  const acts = [
    {
      event: { timeStamp: 1000, type: "keydown" },
      loading: 1,
      measured: true,
      name: "j",
      result: 1,
    },
  ];
  expect(actReading({ acts, frames, resource: [] }, 0)).toEqual({
    act: "j",
    loading: 30,
    read: null,
    shown: 30,
  });

  // An act whose result no frame showed gives no reading.
  const unshown = [{ ...acts[0], loading: null, result: null }];
  expect(actReading({ acts: unshown, frames, resource: [] }, 0)).toEqual({
    act: "j",
    loading: null,
    read: null,
    shown: null,
  });
  // An act the page never received gives none either.
  const unpressed = [{ ...acts[0], event: null, loading: null, result: null }];
  expect(actReading({ acts: unpressed, frames, resource: [] }, 0).shown).toBeNull();
});

test("an act refuses a frame stamped before its event, and reads the first frame after the event that shows its result", () => {
  // The frame the result was recorded at was shown at 995, before the event's timeStamp: it cannot
  // show what the event did. The frame after it has no stamp yet, and the one after that, shown at
  // 1060, is the first that shows the result after the event.
  const frames = [
    frame(960, { shown: 995 }),
    frame(1010, { shown: null }),
    frame(1040, { shown: 1060 }),
  ];
  const acts = [
    {
      event: { timeStamp: 1000, type: "pointerdown" },
      loading: 0,
      measured: true,
      name: "a press on Details",
      result: 0,
    },
  ];
  expect(actReading({ acts, frames, resource: [] }, 0)).toEqual({
    act: "a press on Details",
    loading: 60,
    read: null,
    shown: 60,
  });
  // No frame from the recorded one on was shown after the event: no reading.
  expect(actReading({ acts, frames: frames.slice(0, 2), resource: [] }, 0).shown).toBeNull();
});

// The plan's case of a view press that needs a read, written by hand: no capture holds such a
// press yet. The first capture's press, on Spec, drew its result from what the page held before
// its read answered (the case of `view-press-read.json` below), and the capture row now presses a
// view whose read the load did not send, so a later capture can hold one.
test("a view press that needs a read is read in three parts", () => {
  const frames = [frame(990), frame(1006), frame(1040), frame(1200), frame(1400), frame(1500)];
  const acts = [
    {
      event: { timeStamp: 1000, type: "pointerdown" },
      loading: 2,
      measured: true,
      name: "a press on Graph",
      result: 5,
    },
  ];
  // Sent with the load, before the press: it is no read of the press.
  const before = read("/api/v1/issues/LAY-1", 900, 1200);
  const graph = read("/api/v1/issues/LAY-1/tree", 1010, 1400);
  // The result's frame found the result in its callback at 1501 and was shown at 1510; the
  // view's read answered at 1400, before that callback, so the result waited for it.
  expect(actReading({ acts, frames, resource: [before, graph] }, 0)).toEqual({
    act: "a press on Graph",
    loading: 50,
    read: { drawn: 110, sent: 10, url: "/api/v1/issues/LAY-1/tree" },
    shown: 510,
  });

  // The read answered at 1505: after the callback at 1501 that found the result in the
  // document, though before that frame was shown. It cannot have drawn what the document held,
  // so the result needed no read.
  const afterCallback = read("/api/v1/issues/LAY-1/tree", 1010, 1505);
  expect(actReading({ acts, frames, resource: [before, afterCallback] }, 0)).toEqual({
    act: "a press on Graph",
    loading: 50,
    read: null,
    shown: 510,
  });
  // The read answered after the frame was shown: no read is part of the result either.
  const late = read("/api/v1/issues/LAY-1/tree", 1010, 1600);
  expect(actReading({ acts, frames, resource: [before, late] }, 0).read).toBeNull();
  // Only a read sent before the press answered: none is the press's.
  expect(actReading({ acts, frames, resource: [before] }, 0).read).toBeNull();
});

test("an act's bounds read the result of a load that needed no read to its result's frame, and its two read bounds n/a there", () => {
  const needed = { drawn: 110, sent: 10, url: "/api/v1/issues/LAY-1/tree?view=spec" };
  const readings = [
    { act: "a press on Spec", loading: 50, read: needed, shown: 510 },
    { act: "a press on Spec", loading: 40, read: { ...needed, drawn: 260 }, shown: 620 },
    { act: "a press on Spec", loading: 60, read: null, shown: 90 },
    { act: "a press on Spec", loading: 70, read: null, shown: 120 },
    { act: "a press on Spec", loading: null, read: null, shown: null },
  ];
  const bounds = actBounds("during-load.a-press-on-spec", 2, 200, readings);
  expect(bounds.map(readingLine)).toEqual([
    "during-load.a-press-on-spec.result-ms rate=2 readings=50,40,90,120,none limit=200 fourth=120 pass",
    "during-load.a-press-on-spec.read-sent-ms rate=2 readings=10,10,n/a,n/a,n/a limit=200 fourth=10 pass",
    // A load that needed no read meets the read bounds, so of the two that needed one, one may
    // read over the limit, as one load in five may for any bound.
    "during-load.a-press-on-spec.read-drawn-ms rate=2 readings=110,260,n/a,n/a,n/a limit=200 fourth=110 pass",
  ]);
  const slower = readings.map((reading) =>
    reading.read === null ? reading : { ...reading, read: { ...reading.read, drawn: 310 } }
  );
  expect(actBounds("during-load.a-press-on-spec", 2, 200, slower).map(readingLine)[2]).toBe(
    "during-load.a-press-on-spec.read-drawn-ms rate=2 readings=310,310,n/a,n/a,n/a limit=200 fourth=310 fail"
  );

  // An act no load of which needed a read: its two read bounds read n/a five times, and pass.
  const held = actBounds("after-load.j", 4, 200, readings.slice(2).concat(readings.slice(2, 4)));
  expect(held.slice(1).map(readingLine)).toEqual([
    "after-load.j.read-sent-ms rate=4 readings=n/a,n/a,n/a,n/a,n/a limit=200 fourth=n/a pass",
    "after-load.j.read-drawn-ms rate=4 readings=n/a,n/a,n/a,n/a,n/a limit=200 fourth=n/a pass",
  ]);
  expect(held.every(boundHolds)).toBe(true);
});

test("a load a bound does not apply to reads n/a, which meets the bound, where a load that gave no reading is over it", () => {
  expect(fourthSmallest([NOT_APPLICABLE, 300, NOT_APPLICABLE, 100, 250])).toBe(250);
  expect(
    fourthSmallest([NOT_APPLICABLE, NOT_APPLICABLE, NOT_APPLICABLE, NOT_APPLICABLE, 900])
  ).toBe(NOT_APPLICABLE);
  expect(fourthSmallest([NOT_APPLICABLE, null, null, 1, NOT_APPLICABLE])).toBeNull();
  const bound = { bound: "after-load.k.read-drawn-ms", limit: 200, rate: 2 };
  expect(boundHolds({ ...bound, readings: [NOT_APPLICABLE, 300, NOT_APPLICABLE, 100, 150] })).toBe(
    true
  );
  expect(boundHolds({ ...bound, readings: [NOT_APPLICABLE, 300, NOT_APPLICABLE, 100, 250] })).toBe(
    false
  );
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

// ---------------------------------------------------------------------------------------------
// The init script's readers, against stand-ins of the browser's observers.
// ---------------------------------------------------------------------------------------------

/** A stand-in of `PerformanceObserver` that hands its entries to its callback only when told
 *  (`deliver`), as the browser hands them in a task of their own some time after it queued them,
 *  and answers those not yet handed over from `takeRecords`. */
class QueuedObserver {
  static readonly supportedEntryTypes = [
    "element",
    "event",
    "largest-contentful-paint",
    "longtask",
    "resource",
  ];
  /** The observer of each entry type the init script observes, newest last. */
  static readonly observing = new Map<string, QueuedObserver>();
  private queued: PerformanceEntry[] = [];

  constructor(private readonly callback: (list: { getEntries(): PerformanceEntry[] }) => void) {}

  observe(options: { readonly type: string }): void {
    QueuedObserver.observing.set(options.type, this);
  }

  /** Queues `entry`, which the callback has not been handed yet. */
  queue(entry: object): void {
    this.queued.push(entry as PerformanceEntry);
  }

  /** Hands every queued entry to the callback. */
  deliver(): void {
    const entries = this.takeRecords();
    this.callback({ getEntries: () => entries });
  }

  takeRecords(): PerformanceEntry[] {
    const entries = this.queued;
    this.queued = [];
    return entries;
  }
}

/** The init script's config, as `installInstruments` gives it from the load rows' selectors. */
const PAGE_CONFIG: PageConfig = {
  boxes: "none",
  groups: BOX_GROUPS,
  header: HEADER_SELECTORS,
  named: null,
  namedGroup: NAMED_GROUP,
  rows: LOAD_ROW_SELECTOR,
  rowsGroup: ROWS_GROUP,
  viewRow: BOX_GROUPS["view row's controls"].join(", "),
  viewRows: VIEW_ROW_SELECTOR,
};

/** Runs the init script (`instrumentPage`) against stand-ins of the page's globals: `QueuedObserver`
 *  and the `window` and `document` given, and a message channel and a frame loop that never
 *  deliver unless given too. Hands `body` what the script put on the window, and puts the globals
 *  back after. */
async function withStandIns(
  page: {
    readonly MessageChannel?: unknown;
    readonly document: object;
    readonly requestAnimationFrame?: unknown;
    readonly window: Record<string, unknown>;
  },
  body: (instruments: NonNullable<Window["__loadInstruments"]>) => Promise<void> | void
): Promise<void> {
  page.window.top = page.window;
  const standIns: Record<string, unknown> = {
    MessageChannel: class {
      port1 = { onmessage: null };
      port2 = { postMessage: () => {} };
    },
    PerformanceObserver: QueuedObserver,
    requestAnimationFrame: () => 0,
    ...page,
  };
  const saved = Object.keys(standIns).map(
    (name) => [name, Object.getOwnPropertyDescriptor(globalThis, name)] as const
  );
  try {
    Object.assign(globalThis, standIns);
    QueuedObserver.observing.clear();
    instrumentPage(PAGE_CONFIG);
    const instruments = (page.window as unknown as Window).__loadInstruments;
    if (instruments === undefined) throw new Error("the init script put nothing on the window");
    await body(instruments);
  } finally {
    for (const [name, descriptor] of saved) {
      if (descriptor === undefined) Reflect.deleteProperty(globalThis, name);
      else Object.defineProperty(globalThis, name, descriptor);
    }
  }
}

test("the init script's readers take the entries an observer has queued and not yet handed over, so a load's end counts a long task whose callback has not run", async () => {
  const quiet = {
    document: {
      fonts: { addEventListener: () => {} },
      querySelector: () => null,
      querySelectorAll: () => [],
    },
    window: { addEventListener: () => {}, innerHeight: 900 },
  };
  await withStandIns(quiet, (instruments) => {
    const tasks = QueuedObserver.observing.get("longtask");
    const resource = QueuedObserver.observing.get("resource");
    if (tasks === undefined || resource === undefined) {
      throw new Error("the init script observes no long tasks or no resources");
    }
    // The long tasks of `conversation-390.json`'s last second: the first handed over, the second
    // ended and queued, its callback not yet run, when the probe of that capture answered.
    tasks.queue({ duration: 302, entryType: "longtask", name: "self", startTime: 5194.1 });
    tasks.deliver();
    tasks.queue({ duration: 184, entryType: "longtask", name: "self", startTime: 6422.1 });
    const probed = instruments.probe();
    expect(probed.tasks).toEqual([
      { duration: 302, name: "self", startTime: 5194.1 },
      { duration: 184, name: "self", startTime: 6422.1 },
    ]);
    // So the load had not ended a second after the first: it ends a second after the second.
    expect(loadEnd(probed.tasks, [], 6496.1)).toBeNull();
    expect(loadEnd(probed.tasks, [], 8000) ?? Number.NaN).toBeCloseTo(7606.1, 6);
    // A queued entry taken is handed over once: the callback that runs later finds nothing more.
    tasks.deliver();
    expect(instruments.probe().tasks).toHaveLength(2);

    // The record flushes every observer, so a read that answered just before it was taken is in it.
    resource.queue({
      duration: 10,
      initiatorType: "fetch",
      name: "http://127.0.0.1:8080/api/v1/issues/LAY-1/tree",
      requestStart: 5501,
      responseEnd: 5510,
      responseStart: 5509,
      startTime: 5500,
    });
    expect(instruments.record().resource.map((entry) => entry.name)).toEqual([
      "http://127.0.0.1:8080/api/v1/issues/LAY-1/tree",
    ]);
  });
});

/** A box as `getBoundingClientRect` answers it, of the stand-in page below. */
interface StandInBox {
  readonly bottom: number;
  readonly height: number;
  readonly left: number;
  readonly right: number;
  readonly top: number;
  readonly width: number;
  readonly x: number;
  readonly y: number;
}

/** The box at `x`, `y` of `width` by `height`. */
function boxAt(x: number, y: number, width: number, height: number): StandInBox {
  return { bottom: y + height, height, left: x, right: x + width, top: y, width, x, y };
}

// Row 5 of builds Job `e2e-rows-7pffd`, at 1440 px, made a fold, an unfold, `j` and `k`, and then
// read no result of Keys or of any view press. Its first outline row stood at 714 px and the
// check under it ran to 985 px, below the 900 px viewport, so `j` scrolled the page about 93 px to
// bring that check on screen. From 761 px up the band of the issue's key and title stays at the
// top and the view row scrolls with the page, so the view row was then under the band, and a
// press at Keys' centre as the page stood landed on the band. The stand-in page is that page: the
// band of 63 px, and the view row at 97 px on a page at its top, scrolled 93 px.
test("an act after the load presses its control where a reader can, scrolling it back from under the band an earlier act scrolled it to", async () => {
  const keys = LOAD_ACTS[1440].find((act) => act.kind === "keys");
  const step = keys?.steps[0];
  if (
    keys === undefined ||
    step?.input.kind !== "press" ||
    step.input.target.kind !== "control" ||
    step.result.kind !== "present"
  ) {
    throw new Error("the acts at 1440 px hold no press on Keys that opens a list");
  }
  let scrolled = 93;
  let open = false;
  let scrolls = 0;
  const pressed: string[] = [];
  const element = (
    name: string,
    words: string,
    box: () => StandInBox,
    scrollIntoView = () => {}
  ) => {
    const drawn = {
      contains: (other: unknown) => other === drawn,
      getBoundingClientRect: box,
      name,
      querySelector: () => null,
      scrollIntoView: () => {
        scrolls += 1;
        scrollIntoView();
      },
      textContent: words,
    };
    return drawn;
  };
  const band = element("the band", "LAY›LAY-1", () => boxAt(56, 0, 1384, 63));
  const viewRow = element("the view row", "", () => boxAt(56, 97 - scrolled, 1384, 34));
  // Scrolled into view, Keys would come to the middle of the viewport, as far as the page's top
  // lets it: back to the page's top.
  const keysButton = element(
    "Keys",
    "Keys",
    () => boxAt(1340, 98 - scrolled, 56, 32),
    () => {
      scrolled = Math.max(0, 98 + 16 - 450);
    }
  );
  const list = element("the list of keys", "", () => boxAt(420, 200, 600, 400));
  const elementFromPoint = (x: number, y: number) =>
    [...(open ? [list] : []), band, keysButton, viewRow].find((drawn) => {
      const box = drawn.getBoundingClientRect();
      return x >= box.left && x < box.right && y >= box.top && y < box.bottom;
    }) ?? null;
  const selected: Record<string, () => unknown[]> = {
    "[data-filter-row]": () => [viewRow],
    "[data-identity-block]": () => [band],
    [step.input.target.selector]: () => [keysButton],
    [step.result.selector]: () => (open ? [list] : []),
  };
  // The page's frames, each run when the stand-in of Playwright waits on the page, and the
  // messages each frame posts, delivered once its callbacks have run.
  const callbacks: ((time: number) => void)[] = [];
  const messages: number[] = [];
  const port: { onmessage: ((message: { readonly data: number }) => void) | null } = {
    onmessage: null,
  };
  const frame = () => {
    for (const callback of callbacks.splice(0)) callback(performance.now());
    for (const index of messages.splice(0)) port.onmessage?.({ data: index });
  };
  const listeners = new Map<string, ((event: unknown) => void)[]>();
  const playwright = {
    keyboard: { press: async () => {} },
    mouse: {
      // A press reaches what is topmost at its point; the page hears its pointerdown whatever it
      // reached.
      click: async (x: number, y: number) => {
        const hit = elementFromPoint(x, y);
        pressed.push(hit?.name ?? "nothing");
        for (const listener of listeners.get("pointerdown") ?? []) {
          listener({ isTrusted: true, timeStamp: performance.now(), type: "pointerdown" });
        }
        if (hit === keysButton) open = true;
      },
    },
    waitForFunction: async (answers: (argument: unknown) => unknown, argument: unknown) => {
      for (let frames = 0; frames < 10; frames += 1) {
        frame();
        const answer = answers(argument);
        if (answer) return { jsonValue: async () => answer };
      }
      throw new errors.TimeoutError("no frame of the stand-in page answered");
    },
  };
  await withStandIns(
    {
      MessageChannel: class {
        port1 = port;
        port2 = { postMessage: (index: number) => messages.push(index) };
      },
      document: {
        elementFromPoint,
        fonts: { addEventListener: () => {} },
        querySelector: (selector: string) => selected[selector]?.()[0] ?? null,
        querySelectorAll: (selector: string) => selected[selector]?.() ?? [],
      },
      requestAnimationFrame: (callback: (time: number) => void) => callbacks.push(callback),
      window: {
        addEventListener: (type: string, listener: (event: unknown) => void) => {
          listeners.set(type, [...(listeners.get(type) ?? []), listener]);
        },
        innerHeight: 900,
      },
    },
    async (instruments) => {
      // As the page stands, a press at Keys' centre lands on the band.
      expect(elementFromPoint(1340 + 28, 98 - scrolled + 16)?.name).toBe("the band");
      const made = await makeSteps(playwright as unknown as Page, 0, keys.steps, "now");
      expect(scrolls, "Keys was not scrolled back into view once").toBe(1);
      expect(pressed, "the press did not reach Keys").toEqual(["Keys"]);
      const reading = actReading(instruments.record(), made.last);
      expect(reading.act).toBe("a press on Keys");
      expect(reading.read).toBeNull();
      expect(reading.shown, "the press on Keys read no result").not.toBeNull();
    }
  );
});

// ---------------------------------------------------------------------------------------------
// Four real loads.
// ---------------------------------------------------------------------------------------------

/** The loads the capture row printed in builds Job `e2e-rows-zmhbt` at the base: Conversation at
 *  1440 px at rate 2 and at 390 px at rate 4, Conversation at 1440 px with the typeface's file
 *  answered 3 s late, and a press on Spec once a load of Conversation at 1440 px had ended. */
const CAPTURES = [
  "conversation-1440",
  "conversation-390",
  "typeface-held-back",
  "view-press-read",
] as const;

type CaptureName = (typeof CAPTURES)[number];

const captures = Object.fromEntries(
  CAPTURES.map((name) => [
    name,
    JSON.parse(
      readFileSync(new URL(`./fixtures/load-readings/${name}.json`, import.meta.url), "utf8")
    ) as LoadCapture,
  ])
) as Record<CaptureName, LoadCapture>;

/** The loads whose frame recorder kept every box: all but the press's, which kept none. */
const BOXED: readonly CaptureName[] = [
  "conversation-1440",
  "conversation-390",
  "typeface-held-back",
];

test("on each captured load the view row is drawn before the rows, each frame is stamped from its start to its callback to its showing, and the box recorder first kept the view row's controls in the view row's first frame", () => {
  for (const name of CAPTURES) {
    const { boxes, frames } = captures[name].record;
    const drawn = firstDrawing(frames);
    const viewRow = drawn.viewRow === null ? -1 : frames.indexOf(drawn.viewRow);
    const rows = drawn.rows === null ? -1 : frames.indexOf(drawn.rows);
    expect(viewRow, `${name} drew no view row`).toBeGreaterThan(0);
    expect(rows, `${name} drew its rows before its view row, or none`).toBeGreaterThan(viewRow);
    expect(
      frames.filter(
        (frame) => frame.at < frame.time || (frame.shown !== null && frame.shown < frame.at)
      ),
      `${name}: frames whose stamps run out of order`
    ).toEqual([]);
    expect(
      frames.slice(0, -1).filter((frame) => frame.shown === null),
      `${name}: frames before the last with no stamp of their showing`
    ).toEqual([]);
    if (BOXED.includes(name)) {
      const first = Math.min(
        ...boxes
          .filter((entry) => entry.group === "view row's controls")
          .map((entry) => entry.frame)
      );
      expect(first, `${name}: the view row's first boxes were kept in another frame`).toBe(viewRow);
    }
  }
});

test("on each captured load each row of the first screen has the Element Timing paint of its first line, and the first screen's paint is the latest of them", () => {
  for (const name of CAPTURES) {
    const { firstScreen, record } = captures[name];
    expect(firstScreen.length, `${name} has no first screen`).toBeGreaterThan(0);
    expect(
      record.element.filter((entry) => entry.identifier !== "issue-page-row" || entry.row === null),
      `${name}: Element Timing entries that are no row's first line`
    ).toEqual([]);
    expect(
      firstScreen.map((row) => [row.label, row.paint]),
      `${name}: a first-screen row's paint is not the earliest paint of its entries`
    ).toEqual(
      firstScreen.map((row) => [
        row.label,
        Math.min(
          ...record.element
            .filter((entry) => entry.row === row.id)
            .map((entry) => entry.renderTime || entry.loadTime || entry.startTime)
        ),
      ])
    );
    expect(firstScreenPaint(firstScreen), name).toEqual({
      reading: Math.max(...firstScreen.map((row) => row.paint ?? 0)),
      unpainted: [],
    });
  }
});

test("on each captured load the largest contentful paint is the later of its two candidates, the larger, reported by the load's end", () => {
  for (const name of CAPTURES) {
    const { end, record } = captures[name];
    expect(record.lcp.map((candidate) => candidate.element)).toEqual([
      name === "conversation-390" ? "span" : "aside",
      "li",
    ]);
    const [smaller, larger] = record.lcp;
    expect((larger?.size ?? 0) > (smaller?.size ?? 0), name).toBe(true);
    expect(largestContentfulPaint(record.lcp, end), name).toBe(larger?.startTime ?? null);
  }
});

test("on each captured load the longest task after the first drawing of the view row leaves out the tasks that started before that frame's callback", () => {
  for (const name of CAPTURES) {
    const { record } = captures[name];
    const viewRow = firstDrawing(record.frames).viewRow;
    if (viewRow === null) throw new Error(`${name} drew no view row`);
    const after = record.longtask.filter((task) => task.startTime > viewRow.at);
    expect(after.length, `${name}: no long task ran before the view row was drawn`).toBeLessThan(
      record.longtask.length
    );
    expect(longestTaskAfterViewRow(record), name).toBe(
      Math.max(...after.map((task) => task.duration))
    );
  }
});

test("on each captured load the layout shift is onCLS's last report, which its reports' deltas sum to, and no shift it counted followed an input", () => {
  for (const name of CAPTURES) {
    const { cls } = captures[name].record;
    expect(cls.length, `${name}: onCLS reported no shift`).toBeGreaterThan(1);
    const shift = layoutShift(cls);
    expect(shift, name).toBe(cls.at(-1)?.value ?? null);
    expect(
      cls.reduce((sum, report) => sum + report.delta, 0),
      name
    ).toBeCloseTo(shift ?? Number.NaN, 12);
    expect(
      cls.flatMap((report) => report.shifts).filter((entry) => entry.hadRecentInput),
      name
    ).toEqual([]);
  }
});

test("on each captured load the end is a second after its last long task or request, with its live socket open throughout, and the 390 px capture's end came before a long task it holds", () => {
  for (const name of CAPTURES) {
    const { end, record, requests } = captures[name];
    expect(
      requests
        .filter((request) => request.kind === "websocket")
        .map((socket) => [new URL(socket.url).pathname, socket.end]),
      `${name}: the live socket`
    ).toEqual([["/ws/live", null]]);
    expect(
      requests.filter((request) => request.kind === "request" && request.end === null),
      `${name}: requests in flight at the end`
    ).toEqual([]);
    const last = Math.max(
      ...record.longtask
        .filter((task) => task.duration > 50)
        .map((task) => task.startTime + task.duration),
      ...requests.flatMap((request) =>
        request.kind === "request" && request.end !== null ? [request.end] : []
      )
    );
    const ended = loadEnd(record.longtask, requests, Number.POSITIVE_INFINITY);
    expect(ended, name).toBe(last + 1000);
    if (name === "conversation-390") {
      // This capture took its end, 6,496.1 ms, before the long-task observer had handed over the
      // task from 6,422.1 to 6,606.1 ms that the record holds: from every task the load holds, it
      // had not ended then. The probe now flushes that observer before it answers (the case of
      // the init script's readers above), so a capture taken since takes the later end.
      expect(loadEnd(record.longtask, requests, end)).toBeNull();
    } else {
      // The spans are the capture's own. The held typeface's capture read its font's span from
      // `Request.timing()`, 4,517.4 to 7,724.9 ms, where the page's Resource Timing has the file
      // from 1,314.2 to 4,520.2 ms, so its end is a second after a misplaced end; the spans are
      // now read from Playwright's reports (the case of a request's span above).
      expect(end, name).toBeCloseTo(ended ?? Number.NaN, 6);
    }
  }
});

test("on each captured load the box recorder drew every group and kept a box only where it changed, and a move is each change outside the frames of the typeface's swap, named with its box before and after", () => {
  for (const name of BOXED) {
    const { end, record } = captures[name];
    expect(groupsUndrawn(record.boxes), name).toEqual([]);
    // Each entry after an element's first, with the box it changed from.
    const last = new Map<number, BoxEntry>();
    const changes: { readonly before: BoxEntry["box"]; readonly entry: BoxEntry }[] = [];
    for (const entry of record.boxes) {
      const before = last.get(entry.id);
      last.set(entry.id, entry);
      if (before === undefined) continue;
      expect(entry.box, `${name}: ${entry.label} kept again at frame ${entry.frame}`).not.toEqual(
        before.box
      );
      changes.push({ before: before.box, entry });
    }
    // A frame of the swap: a `loadingdone` came after the frame before it and by the frame after.
    const swap = (frame: number) =>
      record.fontsDone.some(
        (done) =>
          (record.frames[frame - 1]?.at ?? Number.NEGATIVE_INFINITY) < done &&
          done <= (record.frames[frame + 1]?.at ?? Number.POSITIVE_INFINITY)
      );
    expect(
      boxesMoved(record, end).map((move) => [move.frame, move.label, move.before, move.after]),
      name
    ).toEqual(
      changes
        .filter((change) => !swap(change.entry.frame))
        .map((change) => [change.entry.frame, change.entry.label, change.before, change.entry.box])
    );
    // With no `loadingdone`, every change is a move.
    expect(boxesMoved({ ...record, fontsDone: [] }, end), name).toHaveLength(changes.length);
  }
  // The held typeface swapped during its load, and boxes changed in the frames of that swap.
  const held = captures["typeface-held-back"];
  expect(
    boxesMoved({ ...held.record, fontsDone: [] }, held.end).length -
      boxesMoved(held.record, held.end).length
  ).toBeGreaterThan(0);
});

test("the press on Spec of the first capture drew its result from what the page held: its view read answered after the callback of the frame that found the result", () => {
  const { rate, record } = captures["view-press-read"];
  expect(record.acts.map((act) => act.name)).toEqual(["a press on Spec"]);
  const [act] = record.acts;
  const frame = act?.result == null ? undefined : record.frames[act.result];
  if (act?.event == null || frame === undefined || frame.shown === null) {
    throw new Error("the capture holds no press with the frame of its result");
  }
  const event = act.event.timeStamp;
  // The one read the page's code sent after the press is Spec's view read.
  const sent = record.resource.filter((entry) => entry.startTime >= event);
  expect(
    sent.map((entry) => [
      new URL(entry.name).pathname + new URL(entry.name).search,
      entry.initiatorType,
    ])
  ).toEqual([["/api/v1/issues/LAY-1/tree?view=spec", "fetch"]]);
  // It answered after the callback that found the result in the document, before that frame was
  // shown: the rows were there before its answer was, so the result needed no read.
  const [spec] = sent;
  expect((spec?.responseEnd ?? 0) > frame.at && (spec?.responseEnd ?? 0) < frame.shown).toBe(true);
  const reading = actReading(record, 0);
  expect(reading).toEqual({
    act: "a press on Spec",
    loading: frame.shown - event,
    read: null,
    shown: frame.shown - event,
  });
  // So its bounds read the event to the result's frame, and its two read bounds n/a.
  expect(
    actBounds("after-load.a-press-on-spec", rate, 200, [
      reading,
      reading,
      reading,
      reading,
      reading,
    ])
      .slice(1)
      .map((bound) => bound.readings)
  ).toEqual([Array(5).fill(NOT_APPLICABLE), Array(5).fill(NOT_APPLICABLE)]);
});
