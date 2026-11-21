/** A record of every commit of a control: the value its host passed it and what it drew, read
 *  from the DOM React has just committed; and TanStack Query's notifications to its caches'
 *  observers, held back as a browser's timer holds them and told one at a time. */
import { notifyManager } from "@tanstack/react-query";
import { act } from "@testing-library/react";
import { Profiler, type ReactNode } from "react";

/** One commit of a recorded control: the value its host passed it, and what it drew. */
export interface Commit<Host> {
  readonly drawn: string;
  readonly host: Host;
}

/** Every commit of one control, as `recordCommits` records it. */
export interface Commits<Host> {
  /** Draws `children`, the control, recording each commit of it with `host`, the value its host
   *  passed it in that render. */
  readonly Recorded: (props: { children: ReactNode; host: Host }) => ReactNode;
  /** Marks the record at the next `type` event on `target`, before React hears it (a listener on
   *  the target itself, where React listens on its root), and gives the commits from that mark on,
   *  throwing while the event has not come. */
  readonly markAt: (target: EventTarget, type: string) => () => readonly Commit<Host>[];
}

/** Records every commit of a control, what it drew read by `draw` from the DOM React has just
 *  committed. */
export function recordCommits<Host>(draw: () => string): Commits<Host> {
  const commits: Commit<Host>[] = [];
  return {
    Recorded: ({ children, host }) => (
      <Profiler id="recorded" onRender={() => commits.push({ drawn: draw(), host })}>
        {children}
      </Profiler>
    ),
    markAt: (target, type) => {
      let mark: number | undefined;
      target.addEventListener(
        type,
        () => {
          mark = commits.length;
        },
        { capture: true, once: true }
      );
      return () => {
        if (mark === undefined) throw new Error(`no ${type} event has marked the commits yet`);
        return commits.slice(mark);
      };
    },
  };
}

/** What a picker's trigger draws - the priority's menu (`ChoiceMenu`) or a single pick's list
 *  (`MultiSelect`): the words its name says after its label, the value it holds as a `<select>`
 *  says its value, and its text. A trigger that shows its label's first words - an unset
 *  priority's `Priority` - is named by its label alone, and its name says nothing after it. */
export function drawnPick(trigger: HTMLElement): string {
  const [, ...shown] = trigger.getAttribute("aria-labelledby")?.split(" ") ?? [];
  const named = shown.map((id) => document.getElementById(id)?.textContent).join(" ");
  return `${named} ${trigger.textContent}`;
}

/** TanStack Query's notifications to the observers of its caches, held back from now on. */
export interface HeldNotifications {
  /** How many notifications wait. */
  readonly held: () => number;
  /** Tells the notifications held one at a time, in the order they were queued, each in an `act`
   *  of its own, so React commits after each as a browser does after each timer; from then on it
   *  tells each at once again, as `setup.ts` has every case do. A case calls it in its `finally`
   *  as well. */
  readonly tell: () => void;
}

/** Holds back what TanStack Query tells the observers of its caches, as a browser holds it until
 *  the timer its notify manager sets, a frame or more after the write it tells of - or a
 *  background tab longer - until `tell`. */
export function holdNotifications(): HeldNotifications {
  const untold: (() => void)[] = [];
  notifyManager.setScheduler((tell) => {
    untold.push(tell);
  });
  return {
    held: () => untold.length,
    tell: () => {
      notifyManager.setScheduler((tell) => tell());
      for (const tell of untold.splice(0)) act(() => tell());
    },
  };
}
