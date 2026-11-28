import { type Query, type QueryClient, useQueryClient } from "@tanstack/react-query";
import {
  createContext,
  type ReactNode,
  useContext,
  useEffect,
  useState,
  useSyncExternalStore,
} from "react";
import { flushSync } from "react-dom";

import { afterPaint, askForTask, holdSlices } from "./drawn-in-slices";
import type { Preset } from "./page-words";
import { readOut } from "./tree-read/tree-read";

/**
 * A press on the view row (R2; the plan's state table "A press on a view whose rows need a read").
 *
 * The press's handler has sent the reads of the view that no one had sent (`tree-read.ts`'
 * `startPressedViewRead`). Where none of them is out with no answer held, the press moves the
 * address at once, and its commit, in the task of its event, draws the view's first screen from
 * what the page holds, a read the load sent and that is still out included, which the view draws
 * as loading itself. Where one is out, the press's commit has the control read as current and the
 * view's place read busy, with
 * the view the press left still drawn as it was and drawing no further row (`holdSlices`). Once
 * the press's frame is painted, or once those reads answer where they answer first, the view the
 * press left goes off the page in a task of its own (`ViewPress.left`), its place still busy, and
 * the next task moves the address: one commit takes the busy mark off and draws the view
 * pressed, in its loading state while its read is out, or its first screen where the read has
 * answered. So taking the old view down and drawing the new one are never one task. A press on
 * another view while one waits replaces it, and the answer the first waited on is kept for its
 * view.
 *
 * So no render of the view is split over tasks, between which a read's answer or a slice would
 * start it again. The view pressed and whether the view left is off, while one waits, are a store
 * of their own rather than a context's value: only the view row and the stage's box read them
 * (`usePendingView`, `useViewLeft`), and a change of them makes React walk no other part of the
 * page.
 */
export interface ViewPress {
  /** Turns `to` on by `move`, which moves the address: at once where none of `sent`, the reads the
   *  press sent, is out with no answer held, and else in two tasks after the press's frame. */
  readonly press: (to: Preset, move: () => void, sent: readonly Query[]) => void;
  /** The view a press turned on while it waits on that view's reads; undefined while none does. */
  readonly pending: () => Preset | undefined;
  /** Whether the view a waiting press left is off the page, its place empty until the address
   *  moves. */
  readonly left: () => boolean;
  /** Calls `listener` each time `pending` or `left` changes; answers how to stop. */
  readonly subscribe: (listener: () => void) => () => void;
}

/** The page's press, with how to call its wait off. */
interface PagePress extends ViewPress {
  readonly stop: () => void;
}

const ViewPressContext = createContext<ViewPress | undefined>(undefined);

/** The page's one press on the view row, for the view row's controls and the stage's box. */
export function ViewPressProvider({ children }: { children: ReactNode }): ReactNode {
  const queryClient = useQueryClient();
  const [press] = useState(() => pagePress(queryClient));
  useEffect(
    () => () => {
      press.stop();
      holdSlices(false);
    },
    [press]
  );
  return <ViewPressContext.Provider value={press}>{children}</ViewPressContext.Provider>;
}

/** A press and its one wait. */
function pagePress(queryClient: QueryClient): PagePress {
  let pending: Preset | undefined;
  let left = false;
  let stopWait: (() => void) | undefined;
  const listeners = new Set<() => void>();
  const tell = () => {
    for (const listener of listeners) listener();
  };
  // While a press waits, the view it left draws no row (`holdSlices`).
  const setPending = (to: Preset | undefined) => {
    if (to === pending) return;
    pending = to;
    holdSlices(to !== undefined);
    tell();
  };
  const setLeft = (off: boolean) => {
    if (off === left) return;
    left = off;
    tell();
  };
  const stop = () => {
    stopWait?.();
    stopWait = undefined;
  };
  const press: PagePress = {
    left: () => left,
    pending: () => pending,
    press: (to, move, sent) => {
      stop();
      const out = sent.filter(readOut);
      if (out.length === 0) {
        if (pending !== undefined) {
          flushSync(() => {
            setPending(undefined);
            setLeft(false);
            move();
          });
        } else {
          move();
        }
        return;
      }
      setPending(to);
      let leaving = false;
      // The view pressed, in the task after the one that took the view left off the page.
      const arrive = () => {
        leaving = true;
        stopWait = undefined;
        flushSync(() => {
          setPending(undefined);
          setLeft(false);
          move();
        });
      };
      // The view left goes off the page in a task of its own, its place still busy, and the view
      // pressed is drawn in the next, ahead of the page's work asked for at background priority -
      // its publication of the tree, its slices, its counts.
      const leave = () => {
        if (leaving) return;
        leaving = true;
        stop();
        flushSync(() => setLeft(true));
        stopWait = askForTask(arrive);
      };
      // Once the press's own loading frame is painted, take the old view down and then mount the
      // new view in its loading state while the read is still in flight, so neither the old view's
      // unmount nor its remaining work runs after the read answers. If the read answers before
      // that frame, take it down in the task of that answer. A read refused before that frame
      // leaves the view no rows to draw, only its failed read's line, so the address moves at
      // once in the task of the refusal, as it did before the split.
      const cancelLeave = afterPaint(leave);
      const unsubscribe = queryClient.getQueryCache().subscribe((event) => {
        if (event.type !== "updated" || !out.includes(event.query) || out.some(readOut)) return;
        stop();
        const refused = out.some((query) => query.state.data === undefined);
        queueMicrotask(refused ? () => leaving || arrive() : leave);
      });
      stopWait = () => {
        cancelLeave();
        unsubscribe();
      };
    },
    stop,
    subscribe: (listener) => {
      listeners.add(listener);
      return () => listeners.delete(listener);
    },
  };
  return press;
}

/** The page's press on the view row, or undefined outside the page (`ViewPressProvider`), where a
 *  press moves the address at once. */
export function useViewPress(): ViewPress | undefined {
  return useContext(ViewPressContext);
}

/** The view a press turned on while it waits on that view's reads (`ViewPress.pending`);
 *  undefined while none does, and outside the page. */
export function usePendingView(): Preset | undefined {
  const press = useViewPress();
  return useSyncExternalStore(press?.subscribe ?? NO_SUBSCRIPTION, press?.pending ?? NO_PENDING);
}

/** Whether the view a waiting press left is off the page (`ViewPress.left`), so the stage's box
 *  draws nothing in its place; false outside the page. */
export function useViewLeft(): boolean {
  const press = useViewPress();
  return useSyncExternalStore(press?.subscribe ?? NO_SUBSCRIPTION, press?.left ?? NOT_LEFT);
}

const NO_SUBSCRIPTION = () => () => {};
const NO_PENDING = () => undefined;
const NOT_LEFT = () => false;
