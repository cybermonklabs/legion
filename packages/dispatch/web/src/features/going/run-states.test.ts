import { expect, test } from "bun:test";

import { STARTED_RUN, withRunFrame } from "./run-frames";
import { type RunEvent, type RunRequest, type RunState, runNext } from "./run-states";

// The state table of one run of the command bar (plan.md t15): each state - idle, working,
// waiting for a plan, done, stopped at a bound, stopped by the person, failed - under each event -
// a step, propose_plan, approve, Stop, Esc in the empty box, a bound, Continue, the bar closing -
// and the start and the ends that reach the states the table names. Each row is a test: the next
// state, and the request the event sends where it sends one.

type Row = readonly [RunState, RunEvent, RunState, RunRequest?];

const rows: readonly Row[] = [
  // Before Enter there is no run: nothing but the start does anything.
  ["idle", "start", "working", "start"],
  ["idle", "step", "idle"],
  ["idle", "propose_plan", "idle"],
  ["idle", "approve", "idle"],
  ["idle", "stop", "idle"],
  ["idle", "esc", "idle"],
  ["idle", "bound", "idle"],
  ["idle", "continue", "idle"],
  ["idle", "bar_closed", "idle"],
  // A run that works shows each step, waits on its plan, and stops at once on Stop or Esc.
  ["working", "step", "working"],
  ["working", "propose_plan", "waiting"],
  ["working", "approve", "working"],
  ["working", "stop", "stopped_person", "stop"],
  ["working", "esc", "stopped_person", "stop"],
  ["working", "bound", "stopped_bound"],
  ["working", "continue", "working"],
  ["working", "bar_closed", "working"],
  ["working", "end_done", "done"],
  ["working", "end_failed", "failed"],
  // A run whose plan waits: approval runs it, Stop stops it, and Esc only closes the bar.
  ["waiting", "step", "working"],
  ["waiting", "propose_plan", "waiting"],
  ["waiting", "approve", "working", "approve"],
  ["waiting", "stop", "stopped_person", "stop"],
  ["waiting", "esc", "waiting"],
  ["waiting", "bound", "stopped_bound"],
  ["waiting", "continue", "waiting"],
  ["waiting", "bar_closed", "waiting"],
  ["waiting", "end_stopped", "stopped_person"],
  // A run that is done stays done: a follow-up is another run.
  ["done", "step", "done"],
  ["done", "propose_plan", "done"],
  ["done", "approve", "done"],
  ["done", "stop", "done"],
  ["done", "esc", "done"],
  ["done", "bound", "done"],
  ["done", "continue", "done"],
  ["done", "bar_closed", "done"],
  // A bound kept what the run did and offers Continue, which starts a follow-up.
  ["stopped_bound", "step", "stopped_bound"],
  ["stopped_bound", "propose_plan", "stopped_bound"],
  ["stopped_bound", "approve", "stopped_bound"],
  ["stopped_bound", "stop", "stopped_bound"],
  ["stopped_bound", "esc", "stopped_bound"],
  ["stopped_bound", "bound", "stopped_bound"],
  ["stopped_bound", "continue", "working", "continue"],
  ["stopped_bound", "bar_closed", "stopped_bound"],
  // The person's Stop is final.
  ["stopped_person", "step", "stopped_person"],
  ["stopped_person", "propose_plan", "stopped_person"],
  ["stopped_person", "approve", "stopped_person"],
  ["stopped_person", "stop", "stopped_person"],
  ["stopped_person", "esc", "stopped_person"],
  ["stopped_person", "bound", "stopped_person"],
  ["stopped_person", "continue", "stopped_person"],
  ["stopped_person", "bar_closed", "stopped_person"],
  // So is a failure.
  ["failed", "step", "failed"],
  ["failed", "propose_plan", "failed"],
  ["failed", "approve", "failed"],
  ["failed", "stop", "failed"],
  ["failed", "esc", "failed"],
  ["failed", "bound", "failed"],
  ["failed", "continue", "failed"],
  ["failed", "bar_closed", "failed"],
];

for (const [state, event, next, send] of rows) {
  test(`a run ${state}, on ${event}, is ${next}${send === undefined ? " and sends nothing" : ` and sends ${send}`}`, () => {
    expect(runNext(state, event)).toEqual(
      send === undefined ? { state: next } : { send, state: next }
    );
  });
}

// The frames of a run's stream are the table's events: a reply that calls tools shows each call as
// a step that runs until its own frame says how it ended, a plan frame that waits is propose_plan,
// and an end frame that offers Continue is a bound.

test("a reply's tool calls show as running steps until each call's frame ends it", () => {
  const replied = withRunFrame(STARTED_RUN, "reply", {
    step: 1,
    text: "Reading the issue.",
    tools: ["call_tool"],
  });
  expect(replied.steps).toEqual([
    { id: 0, note: "", refused: false, running: true, step: undefined, tool: "call_tool" },
  ]);
  const ended = withRunFrame(replied, "tool", {
    error: false,
    note: "call_tool dispatch_read: CORE-1",
    step: 2,
    tool: "call_tool",
  });
  expect(ended.steps).toEqual([
    {
      id: 0,
      note: "call_tool dispatch_read: CORE-1",
      refused: false,
      running: false,
      step: 2,
      tool: "call_tool",
    },
  ]);
  expect(ended.replies).toEqual(["Reading the issue."]);
  expect(ended.state).toBe("working");
});

test("a plan frame that waits is propose_plan, and an end that offers Continue is a bound", () => {
  const planned = withRunFrame(STARTED_RUN, "plan", { plan: 1, state: "waiting" });
  expect(planned.state).toBe("waiting");
  expect(planned.plan).toEqual({ number: 1, state: "waiting" });
  const bounded = withRunFrame(STARTED_RUN, "end", {
    offers: ["continue"],
    reason: "CALL_BUDGET: the run made its 6 model calls",
    state: "stopped",
  });
  expect(bounded.state).toBe("stopped_bound");
  const stopped = withRunFrame(STARTED_RUN, "end", {
    offers: [],
    reason: "STOPPED: its person stopped it",
    state: "stopped",
  });
  expect(stopped.state).toBe("stopped_person");
});
