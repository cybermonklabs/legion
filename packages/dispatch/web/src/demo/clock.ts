// The views demo's clock (packages/dispatch/e2e/views-demo/README.md): the page reads the time
// as if it were the moment the fixtures were read, plus the time since the page loaded, so every
// age it draws (Waited, In phase, Closed) and every window it reads (Finished's 28 days) is the
// same whenever the demo is opened. vite.demo.config.ts writes that moment into the page as
// `window.__DISPATCH_DEMO_CLOCK__` before any module runs; install.ts imports this first, so the
// app's modules all read the demo's Date. `performance.now()` is left alone: the app reads it only
// to measure intervals, never as a time of day.
declare global {
  interface Window {
    /** The fixtures' read time (manifest.daemon_read_at), RFC 3339. */
    __DISPATCH_DEMO_CLOCK__?: string;
  }
}

const origin = Date.parse(window.__DISPATCH_DEMO_CLOCK__ ?? "");
if (Number.isNaN(origin)) {
  console.error("[views demo] no fixed clock: the page reads the real time");
} else {
  const NativeDate = Date;
  const loadedAt = performance.now();
  const now = () => origin + (performance.now() - loadedAt);
  // A function rather than a subclass, so `Date()` called without `new` still answers the
  // string a Date gives, of the demo's time. Every instance is a native Date, so `instanceof`
  // and every method are the browser's own.
  function DemoDate(this: unknown, ...args: unknown[]): Date | string {
    if (new.target === undefined) return new NativeDate(now()).toString();
    // Any of Date's own forms (a time, a string, the parts of a day), passed on as they came.
    return args.length === 0 ? new NativeDate(now()) : Reflect.construct(NativeDate, args);
  }
  DemoDate.prototype = NativeDate.prototype;
  DemoDate.now = () => Math.floor(now());
  DemoDate.parse = NativeDate.parse;
  DemoDate.UTC = NativeDate.UTC;
  // The page's Date from here on; the browser's own stays reachable through any instance.
  window.Date = DemoDate as unknown as DateConstructor;
}

export {};
