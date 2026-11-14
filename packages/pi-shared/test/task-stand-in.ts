// A stand-in for the daemon's task routes, shared by the tests that run the real Oh My Pi binary
// (each plugin's `*-omp.test.ts`, the harness's `runLegionPane`): the daemon pushes no task, it
// writes the number of the task a claim is owed as the claim's value, which the session takes once
// its host is idle (`src/task-take.ts`). This stand-in holds one claim's value, answers the
// registration's `task` and `finished`, `claims/task`, `claims/task-seen` and `claims/take` as the
// daemon's record answers them, and finishes a task when a turn-outcome report names it. A task
// given after the session registered reaches it as the listener relays a value: a task frame on
// its deliver subject (`publishTaskFrame`).
import { deliverSubject, TaskHeader } from "@legion/envoy-client/consumer-delivery";
import { headers, type NatsConnection } from "nats";

/** A claim's task value: its number, and the bucket's epoch and revision of the write. */
export interface StandInTaskValue {
  readonly number: number;
  readonly epoch: string;
  readonly revision: number;
}

export interface TaskStandIn {
  /** The registration answer's `task` and `finished`, as the record holds them now. */
  readonly registered: () => {
    readonly task: StandInTaskValue | null;
    readonly finished: number;
  };
  /** Queues a new task whose text is `text`: the next number, written at a new revision. */
  readonly give: (text: string) => StandInTaskValue;
  /**
   * Answers a request to one of the task routes, or undefined for any other path. A turn-outcome
   * report is read, finishing the task it names unless it carries a provider error, and left for
   * the caller to answer.
   */
  readonly answer: (path: string, body: Record<string, unknown>) => Response | undefined;
  /** Every take the session made, its body, in order. */
  readonly takes: () => readonly Record<string, unknown>[];
}

/** The bucket epoch every stand-in's values carry. */
export const STAND_IN_TASK_EPOCH = "2026-11-13T16:21:44.123456789Z";

export function taskStandIn(): TaskStandIn {
  let revision = 0;
  let written = 0;
  let finished = 0;
  let current: { readonly value: StandInTaskValue; readonly text: string } | undefined;
  const takes: Record<string, unknown>[] = [];
  return {
    registered: () => ({ task: current?.value ?? null, finished }),
    give: (text) => {
      revision += 1;
      written += 1;
      const value = { number: written, epoch: STAND_IN_TASK_EPOCH, revision };
      current = { value, text };
      return value;
    },
    answer: (path, body) => {
      switch (path) {
        case "/legion/v1/claims/task":
          return Response.json({ task: current?.value ?? null, finished });
        case "/legion/v1/claims/task-seen":
          return new Response(null, { status: 204 });
        case "/legion/v1/claims/take":
          takes.push(body);
          // A take of the number the record owes is accepted, again with the same text for a take
          // sent again; any other number is not owed.
          return current !== undefined && body.number === current.value.number
            ? Response.json({ outcome: "accepted", text: current.text })
            : Response.json({ outcome: "refused", hold: "not-owed" });
        case "/legion/v1/claims/turn-outcome":
          if (
            current !== undefined &&
            body.task === current.value.number &&
            body.providerError === undefined
          ) {
            finished = current.value.number;
            current = undefined;
          }
          return undefined;
        default:
          return undefined;
      }
    },
    takes: () => takes,
  };
}

/**
 * Publishes `value`, `claim`'s task value, to `sessionID` as the listener's task relay does: a core
 * message on the session's deliver subject carrying the four task headers and the value's bytes
 * (`legiontasks` in `packages/envoy`).
 */
export function publishTaskFrame(
  connection: NatsConnection,
  sessionID: string,
  claim: string,
  value: StandInTaskValue
): void {
  const frame = headers();
  frame.set(TaskHeader.claim, claim);
  frame.set(TaskHeader.epoch, value.epoch);
  frame.set(TaskHeader.revision, String(value.revision));
  frame.set(TaskHeader.op, "put");
  connection.publish(
    deliverSubject(sessionID),
    new TextEncoder().encode(JSON.stringify({ task: value.number })),
    { headers: frame }
  );
}
