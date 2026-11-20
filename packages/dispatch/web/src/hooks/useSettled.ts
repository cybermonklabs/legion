import { useEffect, useState } from "react";

/**
 * `value`, once it has held still for `delayMs`: a read that follows the reader's typing asks
 * once for the text the typing rested on, rather than once for every text it passed through. A
 * `delayMs` of zero is no wait at all: the value is settled in the render that brings it, which
 * is how a caller lets one value through at once - an emptied draft takes its pills with it.
 */
export function useSettled<T>(value: T, delayMs: number): T {
  const [settled, setSettled] = useState(value);
  const atOnce = delayMs === 0;
  // Set during render (React's pattern for state derived from props), so a value let through at
  // once is what the wait starts from when the next value has to wait again.
  if (atOnce && !Object.is(settled, value)) setSettled(value);
  useEffect(() => {
    if (atOnce) return;
    const timer = window.setTimeout(() => setSettled(value), delayMs);
    return () => window.clearTimeout(timer);
  }, [atOnce, delayMs, value]);
  return atOnce ? value : settled;
}
