import { type ReactNode, useEffect, useState } from "react";

import { usePageWords, useTooltips } from "../../components/tooltips";

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

// Every mounted <Timestamp> - or, for one handed its clock reading (`now`), the host that handed
// it - subscribes to one shared tick instead of running its own setInterval, so a page with
// hundreds of timestamps (a long log, a comment thread) still pays for exactly one timer.
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

/**
 * Re-renders the caller on the one tick every `<Timestamp>` follows, so a surface that draws a
 * relative time as text of its own - the `title` over a reference picker row's detail line, which
 * a `<Timestamp>` is part of (`RefPickerList.tsx`'s `RefRowDetail`) - says what the line says for
 * as long as it is on the screen. Such a surface hands the `<Timestamp>` the clock reading its own
 * text is measured from (`TimestampProps.now`), so the two read one clock on one subscription.
 *
 * `enabled` is false where that text is not drawn, so a surface subscribes only while it shows
 * one.
 */
export function useRelativeTimeTick(enabled = true): void {
  const [, forceUpdate] = useState(0);
  useEffect(() => {
    if (!enabled) {
      return;
    }
    return subscribe(() => forceUpdate((count) => count + 1));
  }, [enabled]);
}

/** The instant as words, its date and its time in the reader's locale, for a surface with no
 *  tooltip to carry it (the new issue page). */
export function exactTimeLabel(at: Date): string {
  return at.toLocaleString(undefined, { dateStyle: "short", timeStyle: "short" });
}

export interface TimestampProps {
  at: string;
  className?: string;
  /** Inside `NoTooltips`, draws the instant itself in brackets after the relative age, for a
   *  time whose clock time a reader weighs (a claim's age decides whether it is stale). Where a
   *  tooltip carries the instant it is not drawn as well: the two say the same thing, and the
   *  drawn one would widen a chip a narrow host - a Board card, a List row - has no room for. */
  exact?: boolean;
  /** The clock reading the relative words are measured from, for a host that draws the same
   *  relative time as text of its own beside them (`RefPickerList.tsx`'s `RefRowDetail`): the
   *  host follows the tick (`useRelativeTimeTick`) and hands each render's reading to both, so the
   *  two say the same thing. Absent, the timestamp follows the tick and reads the clock itself. */
  now?: number;
}

/** Renders a relative time ("4 minutes ago") that keeps itself current - or, handed `now`, that
 * the host handing it keeps current - with the absolute value available on hover (`title`) and to
 * assistive tech (`datetime`). Inside `NoTooltips` (`components/tooltips.tsx`, the new issue page)
 * it carries no `title`, and with `exact` it says the instant in words instead. */
export function Timestamp({ at, className, exact = false, now }: TimestampProps): ReactNode {
  const tooltips = useTooltips();
  const page = usePageWords();
  useRelativeTimeTick(now === undefined);
  // Inside `NoTooltips` an instant is a value the page draws whole, so it keeps `max-w-full` and
  // breaks anywhere, as every value there does; outside it keeps the host's class alone.
  const shownClassName =
    page === null
      ? className
      : `${className === undefined ? "" : `${className} `}max-w-full [overflow-wrap:anywhere]`;

  const date = new Date(at);
  if (Number.isNaN(date.getTime())) {
    return (
      <time className={shownClassName} {...page?.authorText}>
        {at}
      </time>
    );
  }
  return (
    <time
      className={shownClassName}
      dateTime={at}
      title={tooltips ? date.toLocaleString() : undefined}
      {...page?.authorText}
    >
      {relativeTimeLabel(date, now ?? Date.now())}
      {exact && !tooltips ? ` (${exactTimeLabel(date)})` : null}
    </time>
  );
}
