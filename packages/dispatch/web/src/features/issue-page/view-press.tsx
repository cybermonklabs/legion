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

import { askForTask, holdSlices } from "./drawn-in-slices";
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
 * the view the press left still drawn as it was and drawing no further row (`holdSlices`); the
 * address moves once the last of those reads answers or is refused, in the task of that answer:
 * one commit takes the busy mark off and the next draws the view (`flushSync`), with no frame
 * painted between them. A press on another view while one waits replaces it, and the answer the
 * first waited on is kept for its view.
 *
 * So no render of the view is split over tasks, between which a read's answer or a slice would
 * start it again, and the commit that draws the view is the one that takes the view it left off
 * the screen (`slots/stage.tsx`' `StageViews`). The view pressed, while one waits, is a store of
 * its own rather than a context's value: only the view row and the stage's box read it
 * (`usePendingView`), and a change of it makes React walk no other part of the page.
 */
export interface ViewPress {
  /** Turns `to` on by `move`, which moves the address: at once where none of `sent`, the reads the
   *  press sent, is out with no answer held, and else once they answer. */
  readonly press: (to: Preset, move: () => void, sent: readonly Query[]) => void;
  /** The view a press turned on while it waits on that view's reads; undefined while none does. */
  readonly pending: () => Preset | undefined;
  /** Calls `listener` each time `pending` changes; answers how to stop. */
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
  let stopWait: (() => void) | undefined;
  const listeners = new Set<() => void>();
  // While a press waits, the view it left draws no row (`holdSlices`).
  const setPending = (to: Preset | undefined) => {
    if (to === pending) return;
    pending = to;
    holdSlices(to !== undefined);
    for (const listener of listeners) listener();
  };
  const stop = () => {
    stopWait?.();
    stopWait = undefined;
  };
  const press: PagePress = {
    pending: () => pending,
    press: (to, move, sent) => {
      stop();
      const out = sent.filter(readOut);
      if (out.length === 0) {
        if (pending !== undefined) {
          flushSync(() => {
            setPending(undefined);
            move();
          });
        } else {
          move();
        }
        return;
      }
      setPending(to);
      let moved = false;
      const finish = () => {
        if (moved) return;
        moved = true;
        stop();
        flushSync(() => {
          setPending(undefined);
          move();
        });
      };
      // After the press's own loading frame paints, unmount the old view and mount the new view
      // in its loading state while the read is still in flight, so neither the old view's unmount
      // nor its remaining work runs after the read answers. If the read answers before that frame,
      // move immediately in the task of that answer.
      let cancelTask: (() => void) | undefined;
      const frame = window.requestAnimationFrame(() => {
        cancelTask = askForTask(finish);
      });
      const unsubscribe = queryClient.getQueryCache().subscribe((event) => {
        if (event.type !== "updated" || !out.includes(event.query) || out.some(readOut)) return;
        stop();
        queueMicrotask(finish);
      });
      stopWait = () => {
        window.cancelAnimationFrame(frame);
        cancelTask?.();
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

const NO_SUBSCRIPTION = () => () => {};
const NO_PENDING = () => undefined;
