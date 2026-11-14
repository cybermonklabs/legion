// The ask card demo's clock (packages/dispatch/e2e/ask-card-demo/README.md): the page reads the
// time as if it were the fixtures' moment (`DEMO_CLOCK`), plus the time since the page loaded,
// so every age it draws ("42 minutes ago") is the same whenever the demo is opened. install.ts
// imports this first, so the app's modules all read the demo's Date. `performance.now()` is left
// alone: the app reads it only to measure intervals, never as a time of day.
import { DEMO_CLOCK } from "./fixtures";

const origin = Date.parse(DEMO_CLOCK);
const NativeDate = Date;
const loadedAt = performance.now();
const now = () => origin + (performance.now() - loadedAt);
// A function rather than a subclass, so `Date()` called without `new` still answers the string a
// Date gives, of the demo's time. Every instance is a native Date, so `instanceof` and every
// method are the browser's own.
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
