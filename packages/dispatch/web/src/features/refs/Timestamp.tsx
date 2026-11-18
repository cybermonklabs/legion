import { type ReactNode, useEffect, useState } from "react";

const relativeFormatter = new Intl.RelativeTimeFormat(undefined, { numeric: "auto" });

const RELATIVE_UNITS: Array<{ ms: number; unit: Intl.RelativeTimeFormatUnit }> = [
  { ms: 365 * 24 * 60 * 60 * 1000, unit: "year" },
  { ms: 30 * 24 * 60 * 60 * 1000, unit: "month" },
  { ms: 7 * 24 * 60 * 60 * 1000, unit: "week" },
  { ms: 24 * 60 * 60 * 1000, unit: "day" },
  { ms: 60 * 60 * 1000, unit: "hour" },
  { ms: 60 * 1000, unit: "minute" },
];

export const TICK_INTERVAL_MS = 30_000;

// Every mounted <Timestamp> subscribes to one shared tick instead of running its own
// setInterval, so a page with hundreds of timestamps (a long log, a comment thread) still
// pays for exactly one timer.
const listeners = new Set<() => void>();
let pendingTick: ReturnType<typeof setTimeout> | undefined;

function scheduleTick(): void {
  pendingTick = setTimeout(() => {
    if (listeners.size === 0) {
      pendingTick = undefined;
      return;
    }
    for (const listener of listeners) {
      listener();
    }
    scheduleTick();
  }, TICK_INTERVAL_MS);
}

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  if (pendingTick === undefined) {
    scheduleTick();
  }
  return () => {
    listeners.delete(listener);
  };
}

/** Calls off the shared tick, so the next `<Timestamp>` to mount arms a fresh one. The unit-test
 *  preload calls it after each test, since the tick outlives the test that armed it: it would
 *  fire inside whichever later test has a `<Timestamp>` mounted then, and one armed on a fake clock
 *  that test put back never fires, so no later `<Timestamp>` would tick at all. */
export function resetTimestampTick(): void {
  clearTimeout(pendingTick);
  pendingTick = undefined;
}

/** "4 minutes ago", "just now": the relative form every `<Timestamp>` shows, for a caller that
 *  needs it as text (an attribute). */
export function relativeTimeLabel(at: Date, now: number): string | undefined {
  const deltaMs = at.getTime() - now;
  if (Math.abs(deltaMs) < 60_000) {
    return "just now";
  }
  for (const { ms, unit } of RELATIVE_UNITS) {
    if (Math.abs(deltaMs) >= ms) {
      return relativeFormatter.format(Math.round(deltaMs / ms), unit);
    }
  }
}

export interface TimestampProps {
  at: string;
  className?: string;
}

/** Renders a relative time ("4 minutes ago") that keeps itself current, with the absolute
 * value available on hover (`title`) and to assistive tech (`datetime`). */
export function Timestamp({ at, className }: TimestampProps): ReactNode {
  const [, forceUpdate] = useState(0);
  useEffect(() => subscribe(() => forceUpdate((count) => count + 1)), []);

  const date = new Date(at);
  if (Number.isNaN(date.getTime())) {
    return <time className={className}>{at}</time>;
  }
  return (
    <time className={className} dateTime={at} title={date.toLocaleString()}>
      {relativeTimeLabel(date, Date.now())}
    </time>
  );
}
