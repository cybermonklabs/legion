import { useCallback, useMemo, useRef } from "react";

/** A write whose Retry the guard holds: a keyed write (`useKeyedWrite`), whose `retry` makes its
 *  newest press again - under that press's key where the reader stopped it, which a `mutate` of
 *  the same variables never reaches, since a `mutate` is always a press of its own. */
export interface RetryableMutation {
  retry: () => void;
  variables: unknown;
}

export interface SubmitGuard {
  /** Runs `fn` unless a previous guarded call is still in flight; a disabled-button
   * re-render lags a same-tick double click or Retry press, so this gates on a ref that
   * updates synchronously instead of on a mutation's (React-state) `isPending`. */
  guard: (fn: () => void) => boolean;
  /** Whether a guarded call is in flight. Unlike mutation state, this is current in the same task
   * that began the call, so a caller can hold every control before React has re-rendered it. */
  held: () => boolean;
  /** Clears the guard. A caller that allows one press per request calls it from the mutation's
   * `onSettled`; one that only drops a same-tick double press, letting the next press through
   * while the first is still out (the broadcast queue, `features/agents/BroadcastSends.tsx`),
   * calls it on the next task. */
  release: () => void;
  /** Retries the mutation's newest press (`retry`) if one was made and a request is not already
   *  in flight. */
  retryLast: (mutation: RetryableMutation) => boolean;
}

export function useSubmitGuard(): SubmitGuard {
  const inFlight = useRef(false);
  const guard = useCallback((fn: () => void): boolean => {
    if (inFlight.current) {
      return false;
    }
    inFlight.current = true;
    fn();
    return true;
  }, []);
  const held = useCallback(() => inFlight.current, []);
  const release = useCallback(() => {
    inFlight.current = false;
  }, []);
  const retryLast = useCallback(
    ({ retry, variables }: RetryableMutation): boolean => {
      if (variables === undefined) {
        return false;
      }
      return guard(retry);
    },
    [guard]
  );
  return useMemo(() => ({ guard, held, release, retryLast }), [guard, held, release, retryLast]);
}
