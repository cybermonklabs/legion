/**
 * Where one run of the command bar stands for the person, and what each event does to it: the
 * state table `run-states.test.ts` holds row by row. The run's own frames (`run-frames.ts`) and
 * the person's presses are the events; the table answers the next state and the one request the
 * press sends, if any. Nothing here sends anything.
 */

import {
  GOING_ERROR_ANSWER_WITHDRAWN,
  GOING_ERROR_ANSWER_WITHDRAWN_BY_RUN,
} from "@legion/contracts";

/** Where a run stands: none started yet (idle), working, waiting for the person's approval of its
 *  plan, done, stopped at a pause the server offers Continue on (`stopped_bound`: at a bound of
 *  its level, or where the model's provider refused part of what it found once it had withheld
 *  some), stopped otherwise (`stopped_person`: by its person, whose own Stop offers Continue, by
 *  its grant, or by the provider's refusal of its words alone, which offer nothing), or failed. */
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
 * a bound or a stop stopped; whether its end offers Continue is its end's to say (`endWords`).
 * Closing the bar changes nothing: the run goes on in the server, and a plan that waits shows in
 * the Inbox. An ended run stays as it ended.
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
    case "stopped_person":
      return event === "continue" ? { send: "continue", state: "working" } : { state };
    default:
      return { state };
  }
}

/** The one action a run's end offers (design v2, state 9, rule 7): Continue on a stop that offers
 *  it, Try again on a failure, a new conversation where this one grew too long to go on, or
 *  none. */
export type EndAction = "continue" | "retry" | "new" | undefined;

/** How a run's end reads for its person: one sentence and its one action. `continueInDeep` says
 *  whether "Continue in Deep" stands beside Continue: at a bound of a level below Deep, whose
 *  larger bounds may get past it, and never at a refusal of the model's provider, which Deep
 *  would meet again. `noModel` marks the failure of a Dispatch with no answering model, which
 *  says the one sentence of design v2, state 13, and offers nothing. */
export interface EndWords {
  readonly text: string;
  readonly action: EndAction;
  readonly continueInDeep: boolean;
  readonly noModel: boolean;
}

/** The code a stop or failure reason starts with: "TIME_BUDGET" of "TIME_BUDGET: this run
 *  worked…", "STOPPED" of "STOPPED: its person stopped it"; "" for a reason with none. */
export function reasonCode(reason: string): string {
  const code = reason.split(":", 1)[0]?.trim() ?? "";
  return /^[A-Z][A-Z0-9_]*$/u.test(code) ? code : "";
}

/** The levels as the mode control names them. */
const LEVEL_WORDS: Readonly<Record<string, string>> = {
  deep: "Deep",
  fast: "Fast",
  standard: "Standard",
};

/** Each bound of a level, by its stop reason's code (`going/agent/levels.go`). */
const BOUND_WORDS: Readonly<Record<string, string>> = {
  CALL_BUDGET: "step limit",
  OUTPUT_TOKEN_BUDGET: "reply length limit",
  TIME_BUDGET: "time limit",
};

/** Whether `reason`, a stop's, is the bound that cut the reply of the run's latest model call at
 *  the output tokens a call of its level may write (`OUTPUT_TOKEN_BUDGET`, `Level.cut` in
 *  `going/agent/levels.go`): the agent keeps the text that call wrote, which ends where the cut
 *  fell, mid-sentence as often as not. */
export function cutsReply(reason: string): boolean {
  return reasonCode(reason) === "OUTPUT_TOKEN_BUDGET";
}

/** How long the model was busy, "52s" of a time bound's reason that says so ("…, and the model
 *  was busy for 52s of it; …", `busyWords` in `going/agent/levels.go`): the agent's pacer held
 *  the run's model calls for more than half its time. */
const BUSY_FOR = /, and the model was busy for (\S+) of it;/u;

/** Why a run's grant stopped it, by its reason's code (`api/going_grant.go`). */
const GRANT_WORDS: Readonly<Record<string, string>> = {
  MEMBERSHIP_ENDED: "you may no longer use Dispatch",
  SESSION_ENDED: "you signed out",
  SIGN_IN_EXPIRED: "your sign-in expired",
  TOKEN_REVOKED: "the agent token it acted with was revoked",
};

/** The code of a stop the model's provider refused: its content filter would not read a model
 *  call of the run, even with every result the agent could not vouch for withheld
 *  (`filterStopped`, `going/agent/run.go`). */
const CONTENT_FILTER = "MODEL_CONTENT_FILTER";

/** What the reason of such a stop says where the refused call held nothing the run found, only
 *  the person's words, the page and the agent's rules: it withheld no result, as it had none to
 *  withhold (`filterStopped`). */
const REFUSED_WORDS_ALONE = /held nothing the run found/u;

/** Whether `reason`, a stop's, is the model's provider's refusal (`CONTENT_FILTER`). */
export function providerRefused(reason: string): boolean {
  return reasonCode(reason) === CONTENT_FILTER;
}

/** What a stop the model's provider refused says first, wherever it is named, by what its
 *  reason says the refused call held: the person's words alone, or part of what the run found. */
export function providerRefusedWords(reason: string): string {
  return REFUSED_WORDS_ALONE.test(reason)
    ? "The model's provider refused to read what you asked"
    : "The model's provider refused to read part of what this run found";
}

/** What a failure says, by its reason's code (`going/agent/run.go`, `api/going_agent.go`), and its
 *  action where it is not Try again. */
const FAILURE_WORDS: Readonly<
  Record<string, { readonly text: string; readonly action?: EndAction }>
> = {
  AGENT_KILLED: { text: "Dispatch's agent stopped while it worked on this run." },
  HISTORY_TOO_LARGE: {
    action: "new",
    text: "This conversation grew too long for the model to read.",
  },
  MODEL_CALL_TIME: { text: "The model took too long to answer." },
  MODEL_FAILED: { text: "Dispatch could not reach the model." },
  MODEL_REFUSED: { text: "The model refused to answer." },
  MODEL_REPLY_TOO_LARGE: { text: "The model's reply was too long to read." },
};

/** The sentence of a Dispatch with no answering model (design v2, state 13). */
export const NO_MODEL_WORDS = "Answers are off on this Dispatch: it has no answering model.";

/** How an answer was withdrawn (dispatch://INFRA-1523, R3): a comment or an issue message it read
 *  was deleted by another writer (`read_deleted`), or only by deletes the run's own approved plan
 *  made (`run_deleted`). */
export type Withdrawal = "read_deleted" | "run_deleted";

/** The sentence of a withdrawn answer, by how it was withdrawn (`Withdrawal`): a run, or a short
 *  answer, whose words Dispatch then gives none of. It stands in place of the answer, its steps and
 *  its blocks, and offers no next step. A run its own delete withdrew keeps its plan's card beside
 *  it, which shows no word of the post it deleted. */
export const WITHDRAWN_WORDS: Readonly<Record<Withdrawal, string>> = {
  read_deleted: "Dispatch withdrew this answer because a comment or message it read was deleted.",
  run_deleted:
    "Dispatch withdrew this answer because this run deleted a comment or message it read.",
};

/** How `reason`, a run's stop reason or a short answer's error code, withdraws its answer: by its
 *  whole code (`reasonCode`), `GOING_ERROR_ANSWER_WITHDRAWN` or
 *  `GOING_ERROR_ANSWER_WITHDRAWN_BY_RUN`, so neither is ever read as the other; undefined for a
 *  reason that withdraws nothing. */
export function withdrawalOf(reason: string): Withdrawal | undefined {
  switch (reasonCode(reason)) {
    case GOING_ERROR_ANSWER_WITHDRAWN:
      return "read_deleted";
    case GOING_ERROR_ANSWER_WITHDRAWN_BY_RUN:
      return "run_deleted";
    default:
      return undefined;
  }
}

/**
 * How the end of a run in `state`, stopped or failed with `reason` (its end frame's, or its
 * record's), reads for its person (design v2, state 9, rule 7): never a code or the reason's own
 * words, which are written for a developer, and one next step. A bound is a pause: "Stopped at
 * Standard's time limit", with how long the model was busy where the reason says it was busy for
 * more than half the time ("· the model was busy for 52s of it"), what the run did so far
 * (`didSoFar`, "read 14 items") and Continue, with Continue in Deep below Deep. A stop the model's
 * provider refused says what it refused to read, as its reason says (`providerRefusedWords`), and
 * what the person can do: Continue, or ask in other words, where its end offered Continue, else
 * ask in other words. A Stop is the person's own pause, so it says what the run did before it,
 * "Stopped · read 1 item", and offers Continue; one its grant ended says why, and offers nothing
 * a lapsed grant would refuse again. A failure is one sentence and Try again, or the sentence of
 * state 13 for a Dispatch with no answering model. A withdrawn run, stopped or failed, says its
 * withdrawal's sentence (`WITHDRAWN_WORDS`) and offers nothing. Undefined for a run that has not
 * stopped or failed.
 */
export function endWords(
  state: RunState,
  reason: string,
  level: string,
  didSoFar: string
): EndWords | undefined {
  const code = reasonCode(reason);
  const ended = state === "stopped_bound" || state === "stopped_person" || state === "failed";
  const withdrawal = ended ? withdrawalOf(reason) : undefined;
  if (withdrawal !== undefined) {
    return {
      action: undefined,
      continueInDeep: false,
      noModel: false,
      text: WITHDRAWN_WORDS[withdrawal],
    };
  }
  switch (state) {
    case "stopped_bound": {
      if (code === CONTENT_FILTER) {
        return {
          action: "continue",
          continueInDeep: false,
          noModel: false,
          text: `${providerRefusedWords(reason)}. Continue, or ask in other words.`,
        };
      }
      const named = LEVEL_WORDS[level] ?? "its level";
      const bound = BOUND_WORDS[code];
      const busy = code === "TIME_BUDGET" ? BUSY_FOR.exec(reason)?.[1] : undefined;
      const at =
        bound === undefined ? `Stopped at a limit of ${named}` : `Stopped at ${named}'s ${bound}`;
      const stopped = busy === undefined ? at : `${at} · the model was busy for ${busy} of it`;
      return {
        action: "continue",
        continueInDeep: level !== "deep",
        noModel: false,
        text: didSoFar === "" ? stopped : `${stopped} · ${didSoFar} so far`,
      };
    }
    case "stopped_person": {
      if (code === CONTENT_FILTER) {
        return {
          action: undefined,
          continueInDeep: false,
          noModel: false,
          text: `${providerRefusedWords(reason)}. Ask in other words.`,
        };
      }
      const grant = GRANT_WORDS[code];
      if (grant !== undefined) {
        return {
          action: undefined,
          continueInDeep: false,
          noModel: false,
          text: `Stopped because ${grant} · kept what it did`,
        };
      }
      return {
        action: "continue",
        continueInDeep: false,
        noModel: false,
        text: didSoFar === "" ? "Stopped · kept what it did" : `Stopped · ${didSoFar}`,
      };
    }
    case "failed": {
      if (code === "NO_MODEL") {
        return { action: undefined, continueInDeep: false, noModel: true, text: NO_MODEL_WORDS };
      }
      const failure = FAILURE_WORDS[code];
      return {
        action: failure?.action ?? "retry",
        continueInDeep: false,
        noModel: false,
        text: failure?.text ?? "Dispatch's agent failed on this run.",
      };
    }
    default:
      return undefined;
  }
}
