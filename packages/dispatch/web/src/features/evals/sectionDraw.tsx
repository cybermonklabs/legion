import {
  createContext,
  type ReactNode,
  type RefObject,
  useCallback,
  useContext,
  useLayoutEffect,
  useRef,
  useState,
} from "react";
import { flushSync } from "react-dom";

/** How long a section waiting below the first screen waits for the page to be idle before it draws
 *  anyway (`requestIdleCallback`'s `timeout`), so every section is drawn within the ruling's 2 s
 *  of the navigation with no scroll, however busy the page stays. */
const IDLE_TIMEOUT_MS = 250;

/** How long a section that shows its box waits for every section to be settled before the page's
 *  idle time draws it anyway: a view that never answers (refused late, or held back) must not
 *  keep the sections around it from drawing within the ruling's 2 s. */
const SETTLE_WAIT_MS = 1000;

/**
 * Whether the charts of the section around draw their marks (a chart's bars, lines and points),
 * or only their frame: the same elements at the same size, with nothing drawn in them. A chart
 * outside a section that holds its drawing back (anywhere but the Evals tab, or a part a test
 * renders alone) draws at once.
 */
export const ChartsDrawn = createContext(true);

/** Whether the charts here draw their marks (`ChartsDrawn`). */
export function useChartsDrawn(): boolean {
  return useContext(ChartsDrawn);
}

/** Holds a section's box back while something in it still waits for an answer that will change
 *  its size (a CI job list's items view): the hold, given back by the function it returns. */
type HoldBox = () => () => void;

const SectionBoxHold = createContext<HoldBox | null>(null);

/**
 * Holds the box of the section around back while `waiting`: the section shows its box, and is
 * planned, only once nothing in it waits any more, so its box has the size it has once drawn
 * (INFRA-979's ruling (A), item 4). Outside a section it holds nothing.
 */
export function useHoldBox(waiting: boolean): void {
  const hold = useContext(SectionBoxHold);
  useLayoutEffect(() => (waiting && hold !== null ? hold() : undefined), [hold, waiting]);
}

/** One section of the plan: its element, where it stands, and what draws it. */
interface PlannedSection {
  element: HTMLElement;
  /** Whether it lies where it stays for every section below it: its views have answered or been
   *  refused, or none is served. A part that still holds its box back (`useHoldBox`) only grows
   *  it, which moves a section below that lies out of the viewport further out. */
  settled: boolean;
  /** Whether it shows its box and has not drawn its charts yet. */
  waiting: boolean;
  /** Draws its charts. */
  readonly draw: () => void;
}

/** The plan of the tab's sections' drawing, which each section tells where it stands. */
interface DrawPlan {
  readonly update: (section: PlannedSection) => void;
  readonly remove: (section: PlannedSection) => void;
  readonly stop: () => void;
}

const SectionDrawPlan = createContext<DrawPlan | null>(null);

/** Whether any part of `element` lies in the viewport now: a section with a part in it draws its
 *  charts at once. An element the DOM lays out nowhere (a unit test's) lies at the viewport's top
 *  corner, so it is in it. */
function inViewport(element: Element): boolean {
  const rect = element.getBoundingClientRect();
  return (
    rect.top < window.innerHeight &&
    rect.bottom >= 0 &&
    rect.left < window.innerWidth &&
    rect.right >= 0
  );
}

/** Runs `run` once the page is idle, or after IDLE_TIMEOUT_MS at the latest; where the browser
 *  has no idle callback, on the next turn of the event loop. The function it returns cancels it. */
function whenIdle(run: () => void): () => void {
  if (typeof requestIdleCallback === "function") {
    const handle = requestIdleCallback(run, { timeout: IDLE_TIMEOUT_MS });
    return () => cancelIdleCallback(handle);
  }
  const handle = setTimeout(run, 0);
  return () => clearTimeout(handle);
}

/**
 * The plan by which the Evals tab draws its sections (INFRA-979's ruling (A)). A section whose
 * views have answered shows its header and its box: everything it draws, each chart's frame at its
 * drawn size, and none of the charts' marks (`ChartsDrawn`). Once every section above it is
 * settled, so that it lies where it stays or only moves down, a section with any part in the
 * viewport draws its charts at once, in the commit that showed its box, before the browser
 * paints. Every other one waits: once every section is settled, or SETTLE_WAIT_MS after the first
 * box was shown while one still is not, the waiting ones draw one at a time in the page's idle
 * time, in the order of the page, each in an idle callback of its own; and one the reader scrolls
 * into view before then draws as it comes into view.
 */
function createDrawPlan(): DrawPlan {
  const sections = new Set<PlannedSection>();
  let cancelIdle: (() => void) | null = null;
  let observer: IntersectionObserver | null = null;
  // Whether the plan still waits for every section to be settled before it draws the waiting
  // ones, and the timer that ends that wait.
  let patient = true;
  let patience: ReturnType<typeof setTimeout> | null = null;
  const inOrder = () =>
    [...sections].sort((a, b) =>
      a.element.compareDocumentPosition(b.element) & Node.DOCUMENT_POSITION_FOLLOWING ? -1 : 1
    );
  const drawNow = (section: PlannedSection, draw: (run: () => void) => void) => {
    section.waiting = false;
    observer?.unobserve(section.element);
    draw(section.draw);
  };
  /** Whether every section above `section` is settled, or the plan waits for that no more. */
  const settledAbove = (section: PlannedSection) => {
    const ordered = inOrder();
    return !patient || ordered.slice(0, ordered.indexOf(section)).every((one) => one.settled);
  };
  const watch = () => {
    if (observer !== null || typeof IntersectionObserver === "undefined") return observer;
    observer = new IntersectionObserver((entries) => {
      for (const entry of entries) {
        const section = [...sections].find((one) => one.element === entry.target);
        if (entry.isIntersecting && section?.waiting && settledAbove(section))
          drawNow(section, (draw) => draw());
      }
    });
    return observer;
  };
  const drainWhenIdle = () => {
    if (cancelIdle !== null) return;
    cancelIdle = whenIdle(() => {
      cancelIdle = null;
      const next = inOrder().find((section) => section.waiting);
      if (next === undefined) return;
      // Drawn inside this idle callback, so the next waits for the page to be idle again.
      drawNow(next, (draw) => flushSync(draw));
      if ([...sections].some((section) => section.waiting)) drainWhenIdle();
    });
  };
  const replan = () => {
    let above = true;
    for (const section of inOrder()) {
      // A section's layout effect plans, so a draw here renders before the browser paints.
      if (section.waiting && above && inViewport(section.element))
        drawNow(section, (draw) => draw());
      if (section.waiting) watch()?.observe(section.element);
      above &&= section.settled;
    }
    const waiting = [...sections].some((section) => section.waiting);
    if (waiting && (above || !patient)) drainWhenIdle();
    else if (waiting && patience === null)
      patience = setTimeout(() => {
        patient = false;
        replan();
      }, SETTLE_WAIT_MS);
  };
  return {
    remove: (section) => {
      if (!sections.delete(section)) return;
      observer?.unobserve(section.element);
      replan();
    },
    stop: () => {
      cancelIdle?.();
      cancelIdle = null;
      clearTimeout(patience ?? undefined);
      patience = null;
      observer?.disconnect();
      observer = null;
      sections.clear();
    },
    update: (section) => {
      sections.add(section);
      replan();
    },
  };
}

/** Draws the Evals tab's sections inside by one plan (`createDrawPlan`), as INFRA-979's ruling
 *  (A) on where the tab's first paint ends says
 *  (dispatch://INFRA-2918/comment/75af800e-9db9-47d9-9c4d-427b0cff36c0). */
export function SectionDrawing({ children }: { children: ReactNode }): ReactNode {
  const [plan] = useState(createDrawPlan);
  useLayoutEffect(() => () => plan.stop(), [plan]);
  return <SectionDrawPlan.Provider value={plan}>{children}</SectionDrawPlan.Provider>;
}

/** What `useSectionDraw` gives a section: whether it draws its charts, and the hold its parts
 *  take on its box (`SectionCharts`). */
interface SectionDraw {
  readonly drawn: boolean;
  readonly hold: HoldBox;
}

/** What a section's parts read of it: whether its charts draw (`useChartsDrawn`), and the hold
 *  they take on its box (`useHoldBox`). */
export function SectionCharts({
  children,
  draw,
}: {
  children: ReactNode;
  draw: SectionDraw;
}): ReactNode {
  return (
    <SectionBoxHold.Provider value={draw.hold}>
      <ChartsDrawn.Provider value={draw.drawn}>{children}</ChartsDrawn.Provider>
    </SectionBoxHold.Provider>
  );
}

/**
 * Where one section of the Evals tab stands in the tab's plan (`createDrawPlan`): whether it draws
 * its charts yet (`drawn`, at once outside a plan). Its element (`element`) is `settled` once its
 * views have answered or been refused, or none is served, and it shows its `box` once it is
 * settled and drawn (not hidden) and nothing in it holds the box back (`useHoldBox`); the plan
 * draws its charts from then on. The commit in which it first shows its box is marked `boxMark` on
 * the page's performance timeline, from its layout effect, as `useDrawnMark` marks a drawing; the
 * mark goes once it stops showing it or unmounts.
 */
export function useSectionDraw(
  element: RefObject<HTMLElement | null>,
  { box, boxMark, settled }: { box: boolean; boxMark: string; settled: boolean }
): SectionDraw {
  const plan = useContext(SectionDrawPlan);
  const [drawn, setDrawn] = useState(plan === null);
  const holds = useRef(0);
  const [, setReleased] = useState(0);
  const hold = useCallback<HoldBox>(() => {
    holds.current += 1;
    return () => {
      holds.current -= 1;
      // The section plans again: a part's own commit runs none of the section's effects.
      setReleased((released) => released + 1);
    };
  }, []);
  const planned = useRef<PlannedSection | null>(null);
  const marked = useRef(false);
  // Every commit: a part's hold, taken in its own layout effect, runs before this one.
  useLayoutEffect(() => {
    const free = holds.current === 0;
    const shown = box && free;
    if (shown !== marked.current) {
      if (shown) performance.mark(boxMark);
      else performance.clearMarks(boxMark);
      marked.current = shown;
    }
    if (plan === null) return;
    const node = element.current;
    const entry = planned.current;
    if (node === null) {
      if (entry !== null) plan.remove(entry);
      return;
    }
    const now = { settled, waiting: shown && !drawn };
    if (
      entry !== null &&
      entry.element === node &&
      entry.settled === now.settled &&
      entry.waiting === now.waiting
    )
      return;
    if (entry === null) planned.current = { ...now, draw: () => setDrawn(true), element: node };
    else Object.assign(entry, now, { element: node });
    if (planned.current !== null) plan.update(planned.current);
  });
  useLayoutEffect(
    () => () => {
      if (marked.current) performance.clearMarks(boxMark);
      marked.current = false;
      if (planned.current !== null) plan?.remove(planned.current);
    },
    [boxMark, plan]
  );
  return { drawn, hold };
}
