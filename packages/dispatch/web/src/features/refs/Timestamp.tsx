import { type ReactNode, useEffect, useState } from "react";

import { usePageWords, useTooltips } from "../../components/tooltips";
import { relativeTimeLabel } from "./relative-time";

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
}

/** Renders a relative time ("4 minutes ago") that keeps itself current, with the absolute
 * value available on hover (`title`) and to assistive tech (`datetime`). Inside `NoTooltips`
 * (`components/tooltips.tsx`, the new issue page) it carries no `title`, and with `exact` it
 * says the instant in words instead. */
export function Timestamp({ at, className, exact = false }: TimestampProps): ReactNode {
  const tooltips = useTooltips();
  const page = usePageWords();
  const [, forceUpdate] = useState(0);
  useEffect(() => subscribe(() => forceUpdate((count) => count + 1)), []);
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
      {relativeTimeLabel(date, Date.now())}
      {exact && !tooltips ? ` (${exactTimeLabel(date)})` : null}
    </time>
  );
}
