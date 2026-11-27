import { expect, test } from "bun:test";
import { QueryClient } from "@tanstack/react-query";

import { onFakeClock, settle, tick } from "../../__tests__/fake-clock";
import { watchLoadEnd } from "./load-end";

const START = "block-editor-start";

function onLoad(run: (fixture: ReturnType<typeof pendingLoad>) => Promise<void>, longTasks = true) {
  return onFakeClock(async () => {
    const fixture = pendingLoad(longTasks);
    try {
      await run(fixture);
    } finally {
      fixture.load.destroy();
      fixture.client.clear();
    }
  });
}

function pendingLoad(longTasks: boolean) {
  const client = new QueryClient({
    defaultOptions: { queries: { gcTime: Infinity, retry: false } },
  });
  let drawn = false;
  let fontStatus: FontFaceSetLoadStatus = "loading";
  const listeners = new Set<() => void>();
  const observers = new Map<string, (end: number) => void>();
  const starts: string[] = [];
  const marks: string[] = [];
  const at = Date.now();
  const now = () => Date.now() - at;
  const changed = () => {
    for (const listener of listeners) listener();
  };
  const load = watchLoadEnd({
    queryClient: client,
    ready: () => drawn && fontStatus === "loaded",
    subscribeReady(listener: () => void) {
      listeners.add(listener);
      return () => listeners.delete(listener);
    },
    now,
    observe(type: "longtask" | "resource", report: (end: number) => void) {
      if (type === "longtask" && !longTasks) return;
      observers.set(type, report);
      return () => observers.delete(type);
    },
    mark: (name: string) => marks.push(name),
    start: () => starts.push("started"),
  });
  return {
    load,
    client,
    starts,
    marks,
    observers,
    rowsDrawn() {
      drawn = true;
      changed();
    },
    fontsLoaded() {
      fontStatus = "loaded";
      changed();
    },
    entry(type: "longtask" | "resource", end = now()) {
      observers.get(type)?.(end);
    },
    fetch() {
      const deferred = Promise.withResolvers<number>();
      const fetch = client.fetchQuery({ queryKey: ["one"], queryFn: () => deferred.promise });
      return async () => {
        deferred.resolve(1);
        await fetch;
      };
    },
  };
}

function neverStarted(load: ReturnType<typeof pendingLoad>) {
  expect(load.starts).toEqual([]);
  expect(load.marks).toEqual([]);
}
function startedOnce(load: ReturnType<typeof pendingLoad>) {
  expect(load.starts).toEqual(["started"]);
  expect(load.marks).toEqual([START]);
}

test("the last query cannot start the quiet second before the last rows and fonts", () =>
  onLoad(async (f) => {
    const answer = f.fetch();
    await answer();
    await tick(2_000);
    neverStarted(f);
    f.rowsDrawn();
    await tick(2_000);
    neverStarted(f);
    f.fontsLoaded();
    await tick(1_497);
    neverStarted(f);
    await tick(10);
    startedOnce(f);
  }));

test("with fonts and queries ready, the last slice starts the quiet second", () =>
  onLoad(async (f) => {
    f.fontsLoaded();
    await tick(2_000);
    neverStarted(f);
    f.rowsDrawn();
    await tick(1_500);
    startedOnce(f);
  }));

test("with queries and rows ready, the fonts start the quiet second", () =>
  onLoad(async (f) => {
    f.rowsDrawn();
    await tick(2_000);
    neverStarted(f);
    f.fontsLoaded();
    await tick(1_500);
    startedOnce(f);
  }));

test("a long task or resource ending restarts the quiet second from its end", () =>
  onLoad(async (f) => {
    f.rowsDrawn();
    f.fontsLoaded();
    await tick(900);
    f.entry("longtask");
    await tick(900);
    neverStarted(f);
    f.entry("resource");
    await tick(1_497);
    neverStarted(f);
    await tick(10);
    startedOnce(f);
  }));

test("a new query stops the quiet second until its answer", () =>
  onLoad(async (f) => {
    f.rowsDrawn();
    f.fontsLoaded();
    await tick(900);
    const answer = f.fetch();
    await settle();
    await tick(2_000);
    neverStarted(f);
    await answer();
    await tick(1_497);
    neverStarted(f);
    await tick(10);
    startedOnce(f);
  }));

test("the mark is set only after the additional 500 ms", () =>
  onLoad(async (f) => {
    f.rowsDrawn();
    f.fontsLoaded();
    await tick(1_000);
    neverStarted(f);
    await tick(495);
    neverStarted(f);
    await tick(10);
    startedOnce(f);
  }));

test("an early Enter or click starts the work before the query or fonts", () =>
  onLoad(async (f) => {
    const answer = f.fetch();
    f.load.startNow();
    startedOnce(f);
    f.rowsDrawn();
    f.fontsLoaded();
    await answer();
    await tick(2_000);
    startedOnce(f);
  }));

test("without longtask observation, resources and queries still hold the quiet second", () =>
  onLoad(async (f) => {
    expect(f.observers.has("longtask")).toBe(false);
    f.rowsDrawn();
    f.fontsLoaded();
    await tick(900);
    f.entry("resource");
    await tick(1_497);
    neverStarted(f);
    await tick(10);
    startedOnce(f);
  }, false));

test("once the work starts, neither another act nor a later load end starts it again", () =>
  onLoad(async (f) => {
    f.rowsDrawn();
    f.fontsLoaded();
    await tick(1_500);
    startedOnce(f);
    f.entry("longtask");
    f.load.startNow();
    await tick(2_000);
    startedOnce(f);
  }));
