import { type QueryKey, type UseQueryOptions, useQuery } from "@tanstack/react-query";
import { type ReactNode, useLayoutEffect } from "react";

/** The step a test has `BeforeEffects` run (`stepBeforeEffects`). */
export interface StepBeforeEffects {
  next?: () => void;
}

/** A step for `BeforeEffects` to read, and how a test sets it: `beforeEffects(run)`, then the
 *  change the next commit draws, in which `run` runs once. */
export function stepBeforeEffects(): {
  beforeEffects: (run: () => void) => void;
  step: StepBeforeEffects;
} {
  const step: StepBeforeEffects = {};
  return {
    beforeEffects: (run) => {
      step.next = run;
    },
    step,
  };
}

/**
 * Runs `step.next`, once, in the next commit that draws it: once every component of that commit
 * has rendered and its DOM is in place, and before any passive effect of the commit runs. That is
 * where a page can act on what it drew before its effects take it in: a timer that fires between
 * a commit and its passive effects, or a blur an earlier effect of the same commit raises as it
 * turns a control read-only. It is drawn after the components a test holds, under a parent they
 * render with, so it commits with them.
 */
export function BeforeEffects({ step }: { step: StepBeforeEffects }): null {
  useLayoutEffect(() => {
    const run = step.next;
    step.next = undefined;
    run?.();
  });
  return null;
}

/** `BeforeEffects` drawn beside a page, with `read`, the options of one of the page's own reads,
 *  which it reads too but never starts: it renders, and so commits, with every part of the page
 *  a new answer of that read draws. */
export function BeforeEffectsOfRead<Data, Key extends QueryKey>({
  read,
  step,
}: {
  read: UseQueryOptions<Data, Error, Data, Key>;
  step: StepBeforeEffects;
}): ReactNode {
  const answered = useQuery({ ...read, enabled: false }).data !== undefined;
  return answered ? <BeforeEffects step={step} /> : null;
}
