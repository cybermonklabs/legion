/**
 * What each tool call of this process spent waiting for a daemon that was not serving, which is
 * the one thing the worker's own stall watch must not count against that call
 * (`src/stall-watchdog.ts`, R1: "the outage charges the worker nothing … no action of its stall
 * watchdog").
 *
 * A `legion` tool call that waits out an hour of daemon restart is a call that has run for an
 * hour with no step, which is exactly the shape the watch judges — so without this record a
 * daemon outage would end the very turn that was waiting for the daemon to come back. The
 * watch therefore judges a call by the time it was open **less** the time that call spent
 * waiting, and the daemon's own backstop floor is raised by the same hour
 * (`packages/daemon/internal/config/stallbackstop.go`, StallLadderFor), so a paused watch still
 * finishes first.
 *
 * Each wait is the waiting call's alone, by its tool call id. A shell call stuck beside it in the
 * same turn is stuck for its own reasons, and so is any call open while a wait no tool call owns
 * runs in the background — the boot's, a role regain's `claims/ready`, a turn's outcome — so
 * none of those pauses anything, and the watch still reaches every call that is not itself
 * waiting.
 *
 * A call holds one record for its whole wait, however many attempts it makes. A step of the call
 * that is no request to the daemon — `handoff_complete`'s report read in the pane — is the call's
 * own time, which the watch counts, so it is marked inside that one record (`DaemonWaitRecord`'s
 * `paused` and `resumed`) rather than ending the record and opening another: a call of many
 * attempts stays one entry, and the bound below never drops a part of a wait still running.
 *
 * It is module state and not a field, because the two sides never meet: the wait is recorded in
 * `src/daemon-client.ts`, by whichever call is waiting, and read by the watchdog the plugin
 * built at `session_start`. Both run in one bundle and one process (pi-legion's), so one module
 * instance serves both; a `task` subagent's extension instance shares it, as it shares every
 * other process-wide record here. The module imports nothing.
 *
 * Every moment here is read from the caller's own clock — the retry's `now`, the watchdog's
 * `now` — which in production is `Date.now` on both sides.
 */

/** One stretch of a wait that was the call's own: `to` is undefined while the step still runs. */
interface OwnStep {
  readonly from: number;
  to: number | undefined;
}

/** One window a tool call waited in; `to` is undefined while the wait is still running. `steps`
 * are the call's own stretches inside it, in order, each no wait for the daemon. */
interface DaemonWait {
  readonly toolCallId: string;
  readonly from: number;
  to: number | undefined;
  readonly steps: OwnStep[];
}

/** How many waits the record holds. Nothing clears it for the life of the process, and a call
 * open now cares only about its own waits inside its own window, so the oldest ended wait is
 * dropped once the record is full; a wait that has not ended is never dropped, since that is
 * the one a call open now may be waiting on, steps and all. */
const MOST_RECORDED_WAITS = 32;

const waits: DaemonWait[] = [];

/** What a call does with the record of its wait (`daemonWaitBegan`): `paused` at a step of its own
 * that begins, `resumed` when that step ends and the wait goes on, and `ended` when the wait is
 * over. Calling any of them twice, or never, is safe: a wait never ended reads as running up to
 * whatever moment it is asked about, which is what a process ended mid-wait leaves behind, and a
 * step never resumed reads as running to the wait's end. */
export interface DaemonWaitRecord {
  readonly paused: (at: number) => void;
  readonly resumed: (at: number) => void;
  readonly ended: (at: number) => void;
}

/** Records a wait of `toolCallId` that began at `from`, and answers what marks the call's own
 * steps inside it and ends it. */
export function daemonWaitBegan(toolCallId: string, from: number): DaemonWaitRecord {
  const wait: DaemonWait = { toolCallId, from, to: undefined, steps: [] };
  waits.push(wait);
  if (waits.length > MOST_RECORDED_WAITS) {
    const oldestEnded = waits.findIndex((recorded) => recorded.to !== undefined);
    if (oldestEnded !== -1) waits.splice(oldestEnded, 1);
  }
  return {
    paused: (at) => {
      const last = wait.steps.at(-1);
      if (last !== undefined && last.to === undefined) return;
      wait.steps.push({ from: Math.max(at, last?.to ?? from), to: undefined });
    },
    resumed: (at) => {
      const last = wait.steps.at(-1);
      if (last !== undefined && last.to === undefined) last.to = Math.max(at, last.from);
    },
    ended: (at) => {
      wait.to = Math.max(at, from);
    },
  };
}

/** The stretches of `wait` that were time on the daemon, up to `until` for a wait still running:
 * the wait's window less each step of the call's own, and nothing after a step that has not
 * ended. */
function waitedWindows(wait: DaemonWait, until: number): { from: number; to: number }[] {
  const end = wait.to ?? until;
  const windows: { from: number; to: number }[] = [];
  let start = wait.from;
  for (const step of wait.steps) {
    windows.push({ from: start, to: Math.min(step.from, end) });
    if (step.to === undefined) return windows;
    start = step.to;
  }
  windows.push({ from: start, to: end });
  return windows;
}

/** How much of `[from, to]` the call `toolCallId` spent waiting for a daemon that was not
 * serving. One call can wait more than once, and two of its waits that overlap stopped it once,
 * not twice, so the windows are merged rather than summed. */
export function daemonWaitMs(toolCallId: string, from: number, to: number): number {
  if (to <= from) return 0;
  const clipped = waits
    .filter((wait) => wait.toolCallId === toolCallId)
    .flatMap((wait) => waitedWindows(wait, to))
    .map((window) => ({
      from: Math.max(window.from, from),
      to: Math.min(window.to, to),
    }))
    .filter((window) => window.to > window.from)
    .sort((left, right) => left.from - right.from);
  let waited = 0;
  let mergedTo = from;
  for (const wait of clipped) {
    const start = Math.max(wait.from, mergedTo);
    if (wait.to > start) waited += wait.to - start;
    mergedTo = Math.max(mergedTo, wait.to);
  }
  return waited;
}

/** Forgets every recorded wait. The one test seam: nothing in a running session clears the
 * record, which is why it is bounded above. */
export function forgetDaemonWaitsForTests(): void {
  waits.length = 0;
}
