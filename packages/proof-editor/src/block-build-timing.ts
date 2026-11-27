import {
  editorStateTimerCtx,
  editorViewTimerCtx,
  initTimerCtx,
  schemaTimerCtx,
} from "@milkdown/core";
import { createTimer, type MilkdownPlugin } from "@milkdown/ctx";

/** The measure each task of the block editor's work records, which the issue page reads (R7). */
const TASK_MEASURE = "block-editor-task";

/**
 * Milkdown's dependency timer lists, in the order its build resolves them, each with the phase
 * whose work waits for that list and the gate this module adds to it: the init plugin's remark
 * setup, the schema with the parser, serializer, commands and keymap that wait for it, the editor
 * state, and the editor view.
 */
const GATES = [
  { slice: initTimerCtx, phase: "init", gate: createTimer("BlockBuildInitGate") },
  { slice: schemaTimerCtx, phase: "schema", gate: createTimer("BlockBuildSchemaGate") },
  { slice: editorStateTimerCtx, phase: "editor state", gate: createTimer("BlockBuildStateGate") },
  { slice: editorViewTimerCtx, phase: "editor view", gate: createTimer("BlockBuildViewGate") },
];

/** Cuts a block editor's build into tasks, each recording its own measure. */
export interface BlockBuildTiming {
  /** Gates each of Milkdown's dependency timer lists, and releases it in a task of its own once
   *  every other timer in it has resolved. */
  readonly plugin: MilkdownPlugin;
  /** Ends the running phase's measure, then answers in a new task, where `phase` begins; on a
   *  hidden page it answers at once. */
  nextTask(phase: string): Promise<void>;
  /** Ends the running phase's measure. */
  finish(): void;
}

/**
 * A block editor's cold build ran as one long task: Milkdown resolves each dependency timer
 * (`initTimerCtx`, `schemaTimerCtx`, `editorStateTimerCtx`, `editorViewTimerCtx`) in a microtask
 * of the task that resolved the timers it waits for. This adds a gate of its own to each of those
 * lists and resolves it in a new task, by a zero-delay timer, so each phase's work starts a task
 * of its own; the plugins and their order are Milkdown's. Each phase records one
 * `block-editor-task` measure, from when its task begins to the next yield.
 *
 * A hidden page releases each phase at once instead: browsers run a hidden page's timers at most
 * once a second, and Milkdown rejects a timer still waited for 3 s after its first wait (the
 * collaboration plugin waits for the view from the build's first task), so four yields would
 * fail the build there. A hidden page has no one to keep responsive, and each phase still
 * records its measure.
 */
export function createBlockBuildTiming(): BlockBuildTiming {
  let running: { readonly phase: string; readonly start: number } | undefined;

  function finish(): void {
    if (running === undefined) return;
    performance.measure(TASK_MEASURE, {
      start: running.start,
      end: performance.now(),
      detail: { phase: running.phase },
    });
    running = undefined;
  }

  function nextTask(phase: string): Promise<void> {
    finish();
    const { promise, resolve } = Promise.withResolvers<void>();
    const begin = () => {
      running = { phase, start: performance.now() };
      resolve();
    };
    if (document.visibilityState === "hidden") begin();
    else setTimeout(begin, 0);
    return promise;
  }

  const plugin: MilkdownPlugin = (ctx) => {
    for (const { slice, gate } of GATES) {
      ctx.record(gate);
      ctx.update(slice, (timers) => [...timers, gate]);
    }
    return async () => {
      for (const { slice, phase, gate } of GATES) {
        await Promise.all(
          ctx
            .get(slice)
            .filter((timer) => timer !== gate)
            .map((timer) => ctx.wait(timer))
        );
        await nextTask(phase);
        ctx.done(gate);
      }
      return () => {
        for (const { slice, gate } of GATES) {
          ctx.update(slice, (timers) => timers.filter((timer) => timer !== gate));
          ctx.clearTimer(gate);
        }
      };
    };
  };

  return { plugin, nextTask, finish };
}
