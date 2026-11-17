import {
  createContext,
  type ReactNode,
  type RefObject,
  useCallback,
  useContext,
  useMemo,
  useRef,
  useState,
} from "react";

import type { OutlineRequest } from "./IssueTree";

/**
 * What the Contents list asks of the outline, and its Focus branch: the list is drawn in the
 * Contents pane from 1024 px up and in the Outline sheet below it, and the outline is drawn in
 * the body, so the request and Focus branch are held above all three, in `slots/providers.tsx`,
 * and a width that swaps the pane for the sheet keeps both.
 *
 * Each call of `useAskOutline()`'s function is a new request with a `seq` one past the last, and
 * an outline acts on each `seq` once (`useTakeOutlineRequest`). What was acted on is held here
 * too, so an outline drawn again - a view turned on and off again - does not act on the last
 * request a second time.
 */
interface OutlineRequests {
  readonly ask: (id: string | undefined, focusBranch: boolean) => void;
  readonly focusBranch: boolean;
  readonly request: OutlineRequest | undefined;
  readonly setFocusBranch: (next: boolean) => void;
  /** The `seq` of the newest request an outline has acted on. */
  readonly taken: RefObject<number>;
}

const OutlineRequestContext = createContext<OutlineRequests | undefined>(undefined);

/** Holds the Contents list's requests and its Focus branch for one issue page. */
export function OutlineRequestProvider({ children }: { children: ReactNode }): ReactNode {
  const [request, setRequest] = useState<OutlineRequest | undefined>(undefined);
  const [focusBranch, setFocusBranch] = useState(false);
  const taken = useRef(0);
  const ask = useCallback(
    (id: string | undefined, branch: boolean) =>
      setRequest((held) => ({ focusBranch: branch, id, seq: (held?.seq ?? 0) + 1 })),
    []
  );
  const value = useMemo(
    () => ({ ask, focusBranch, request, setFocusBranch, taken }),
    [ask, focusBranch, request]
  );
  return <OutlineRequestContext.Provider value={value}>{children}</OutlineRequestContext.Provider>;
}

/** Asks the outline to go to the node `id` and, with `focusBranch`, to draw that node's branch
 *  alone; `undefined` takes the outline back to the whole issue. Each call is a new request.
 *  With no provider above it, it asks nothing. */
export function useAskOutline(): (id: string | undefined, focusBranch: boolean) => void {
  return useContext(OutlineRequestContext)?.ask ?? askNothing;
}

/** The last request the Contents list made, or `undefined` before its first. */
export function useOutlineRequest(): OutlineRequest | undefined {
  return useContext(OutlineRequestContext)?.request;
}

/** Whether Focus branch is on, and its setter, which the Contents pane and the Outline sheet
 *  share. With no provider above it, it is off and its setter does nothing. */
export function useFocusBranch(): readonly [boolean, (next: boolean) => void] {
  const held = useContext(OutlineRequestContext);
  return held === undefined ? FOCUS_BRANCH_OFF : [held.focusBranch, held.setFocusBranch];
}

/**
 * Takes a request for the outline that calls it: true the first time a `seq` is taken, and false
 * for that `seq` or an older one after, so an outline acts on each request once. The provider
 * holds what was taken, so an outline drawn again does not act on a request twice; an outline
 * with no provider above it - one drawn alone, as in a unit test - holds its own.
 */
export function useTakeOutlineRequest(): (request: OutlineRequest) => boolean {
  const own = useRef(0);
  const taken = useContext(OutlineRequestContext)?.taken ?? own;
  return useCallback(
    (request: OutlineRequest) => {
      if (request.seq <= taken.current) return false;
      taken.current = request.seq;
      return true;
    },
    [taken]
  );
}

function askNothing(): void {}

const FOCUS_BRANCH_OFF: readonly [boolean, (next: boolean) => void] = [false, () => {}];
