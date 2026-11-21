/**
 * Where one run of the command bar stands for the person, and what each event does to it: the
 * state table `run-states.test.ts` holds row by row. The run's own frames (`run-frames.ts`) and
 * the person's presses are the events; the table answers the next state and the one request the
 * press sends, if any. Nothing here sends anything.
 */

/** Where a run stands: none started yet (idle), working, waiting for the person's approval of its
 *  plan, done, stopped at a bound of its level (which offers Continue), stopped by the person, or
 *  failed. */
export type RunState =
  | "idle"
  | "working"
  | "waiting"
  | "done"
  | "stopped_bound"
  | "stopped_person"
  | "failed";

/** What can happen to a run: its person starts it (Enter), one of its steps arrives (a tool call
 *  or a model reply), it proposes a plan, the person approves that plan, presses Stop, presses Esc
 *  in the empty box, a bound of its level stops it, the person presses Continue, the bar closes, or
 *  its end arrives done or failed. */
export type RunEvent =
  | "start"
  | "step"
  | "propose_plan"
  | "approve"
  | "stop"
  | "esc"
  | "bound"
  | "continue"
  | "bar_closed"
  | "end_done"
  | "end_failed"
  | "end_stopped";

/** The request a press sends: Stop's, the approval's, or the follow-up run Continue starts. */
export type RunRequest = "start" | "stop" | "approve" | "continue";

/** A transition: the next state, and the request the event sends where it sends one. */
export interface RunTransition {
  readonly state: RunState;
  readonly send?: RunRequest;
}

/** Whether a run in `state` has ended for good: nothing more of it comes, and a follow-up may
 *  continue its conversation. */
export function runEnded(state: RunState): boolean {
  return (
    state === "done" ||
    state === "stopped_bound" ||
    state === "stopped_person" ||
    state === "failed"
  );
}

/**
 * The next state of a run in `state` on `event`. A working run stops at once on Stop or on Esc in
 * the empty box, and sends its stop; one that waits for its plan stops on Stop alone, since Esc
 * there only closes the bar. Approval runs a waiting plan. Continue starts the follow-up of a run
 * a bound stopped. Closing the bar changes nothing: the run goes on in the server, and a plan
 * that waits shows in the Inbox. An ended run stays as it ended.
 */
export function runNext(state: RunState, event: RunEvent): RunTransition {
  switch (state) {
    case "idle":
      return event === "start" ? { send: "start", state: "working" } : { state };
    case "working":
      switch (event) {
        case "propose_plan":
          return { state: "waiting" };
        case "stop":
        case "esc":
          return { send: "stop", state: "stopped_person" };
        case "bound":
          return { state: "stopped_bound" };
        case "end_done":
          return { state: "done" };
        case "end_failed":
          return { state: "failed" };
        case "end_stopped":
          return { state: "stopped_person" };
        default:
          return { state };
      }
    case "waiting":
      switch (event) {
        case "step":
          // The run took up its plan again: approved here, in the Inbox or in another tab.
          return { state: "working" };
        case "approve":
          return { send: "approve", state: "working" };
        case "stop":
          return { send: "stop", state: "stopped_person" };
        case "bound":
          return { state: "stopped_bound" };
        case "end_done":
          return { state: "done" };
        case "end_failed":
          return { state: "failed" };
        case "end_stopped":
          return { state: "stopped_person" };
        default:
          return { state };
      }
    case "stopped_bound":
      return event === "continue" ? { send: "continue", state: "working" } : { state };
    default:
      return { state };
  }
}
