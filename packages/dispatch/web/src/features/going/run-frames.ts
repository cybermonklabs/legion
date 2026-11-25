import type { GoingBlockData, GoingRun } from "../../api/types";
import { objectOf, textOf } from "./fields";
import { type RunEvent, type RunState, runEnded, runNext } from "./run-states";

/**
 * One run as its frames have built it so far (`GET /api/v1/going/runs/{id}/events`): its steps -
 * each tool call, working from the frame that names it as it starts until its own frame says how
 * it ended - the prose the model wrote, the blocks the server drew, the plan it proposed and how
 * its turn ended. Every field is a frame's; nothing is guessed.
 */

/** A tool call of a run: its place among the run's steps (`id`, which never changes), its tool,
 *  the words the bar shows for it, the kind of step its frame says it is, the prose that heads it,
 *  how many results it answered, the items it named and answered, whether it was refused, and
 *  whether it was a slip. */
export interface RunStep {
  readonly id: number;
  readonly tool: string;
  /** The step the call is, once its call frame or its tool frame named it; undefined for a call
   *  only a reply named, as a run recorded before calls had call frames does. */
  readonly step: number | undefined;
  /** The call's words: the present form of its label while it runs, from its call frame or from
   *  the reply that made it; once its own frame came, the past form, or the one sentence of a
   *  call that failed. Empty for a call that has none, such as one of a run recorded before calls
   *  had words. */
  readonly label: string;
  /** What the call is - read, search, look, chart, plan, review or write - as its call frame or
   *  its tool frame says it; empty for a call only a reply named. A write is a change of an
   *  approved plan, which the plan's card shows. A call that drew a block is a chart until its
   *  block comes, and then the kind of block it drew: a chart, a table or a metric. */
  readonly kind: string;
  /** The prose the model wrote beside this call and the others of the same model call, which the
   *  bar shows as their header line: the first call frame's of that model call, or its reply's.
   *  Empty where it wrote none, and for a call no model call made. */
  readonly header: string;
  /** How many results the call answered, as its tool frame says, for a call that lists or
   *  searches; 0 for any other, and while it runs. */
  readonly count: number;
  /** The keys and references of the items the call named and answered. */
  readonly items: readonly string[];
  /** Whether the call failed in a way its person can act on, such as no access, as its tool
   *  frame says; its label is then the one sentence that says why. */
  readonly refused: boolean;
  readonly running: boolean;
  /** Whether the call was a slip, as its tool frame says: a guess of the model's that failed - a
   *  step Dispatch does not have, arguments its tool refuses, a change outside a plan - which the
   *  model mends itself and its person cannot act on. A slip is never refused, and its label says
   *  what the model tried ("Tried a step Dispatch does not have"). */
  readonly slip: boolean;
}

/** How a run's turn ended, as its `end` frame says it: its state, why, what it offers next, and
 *  the key of the issue a new-issue run filed, "" for none. */
export interface RunEnd {
  readonly state: string;
  readonly reason: string;
  readonly offers: readonly string[];
  readonly issue: string;
}

/** A run as its frames say it is. */
export interface RunFrames {
  readonly state: RunState;
  readonly steps: readonly RunStep[];
  /** The prose of each reply that called no tool, in order: the last is the answer. */
  readonly replies: readonly string[];
  /** Whether the newest reply frame wrote the answer: the run's latest model call wrote the last
   *  of `replies`, and no reply with calls or with no prose came after it. A bound that cut that
   *  call's reply at its output tokens (`cutsReply`) cut the answer then, which ends where the cut
   *  fell. */
  readonly answerLast: boolean;
  /** The prose the model wrote beside the calls that run now, which the bar shows as their
   *  header line while they run: the first call frame's of a model call, then its reply's. It is
   *  no part of the answer. */
  readonly callsHeader: string;
  /** How many tool results the run withheld from the model, as the reply that ended its turn
   *  says: the answer may miss what they hold. */
  readonly withheld: number;
  /** Whether the answer that ended the run's turn asks its person a question and waits for their
   *  answer (R8: the agent asks which changes to make, and shows no plan), as the reply that ended
   *  the turn says, or its end frame. */
  readonly waitsForPerson: boolean;
  readonly blocks: readonly GoingBlockData[];
  /** The id of the step the newest tool frame ended, which the block frame of a call that drew
   *  one follows at once (the agent stores the two together); undefined before any. */
  readonly lastEnded: number | undefined;
  /** The plan's number and state as the last plan frame said them. */
  readonly plan: { readonly number: number; readonly state: string } | undefined;
  /** How many frames told the bar to read the plan again: each plan frame, the end of a run that
   *  made a plan, whose rows have run by then, and the end of a turn that ran the rows of its
   *  approved plan and waits again, for a row whose item changed after the approval (no plan
   *  frame says a row ran). */
  readonly planReads: number;
  readonly end: RunEnd | undefined;
}

/** A run that has just started, with no frame yet. */
export const STARTED_RUN: RunFrames = {
  answerLast: false,
  blocks: [],
  callsHeader: "",
  end: undefined,
  lastEnded: undefined,
  plan: undefined,
  planReads: 0,
  replies: [],
  state: "working",
  steps: [],
  waitsForPerson: false,
  withheld: 0,
};

/** The texts of a frame's list field, none where it holds no list. */
function textsOf(value: unknown): readonly string[] {
  return Array.isArray(value)
    ? value.filter((item): item is string => typeof item === "string")
    : [];
}

/** A frame's number field, undefined where it holds none. */
function numberOf(value: unknown): number | undefined {
  return typeof value === "number" ? value : undefined;
}

/** The kinds of block a call of a run draws (`going.RunBlockKinds`), each the kind its step takes
 *  once the block comes. */
const DRAWN_KINDS: Readonly<Record<string, true>> = { chart: true, metric: true, table: true };

/** The id `run`'s next step takes: one past its last step's, so no two of its steps share one,
 *  though an end drops the steps that never ended. */
function nextId(run: RunFrames): number {
  return (run.steps.at(-1)?.id ?? -1) + 1;
}

/** The run's steps with a reply frame's calls: one running step added for each of its calls that
 *  no call frame or tool frame named yet, headed by `header`, the prose the reply wrote beside
 *  them, which also heads a step a frame named before with none of its own. Such a frame is of the
 *  call's tool, and its step comes after the reply's own, by at most as many as the reply made
 *  calls; each frame names one call, in the order of the calls. A reply with no step names none,
 *  as a run recorded before replies carried one. */
function withReplySteps(
  run: RunFrames,
  reply: number | undefined,
  tools: readonly string[],
  labels: readonly string[],
  header: string
): readonly RunStep[] {
  const named = new Set<number>();
  const added: RunStep[] = [];
  tools.forEach((tool, index) => {
    const known =
      reply === undefined
        ? undefined
        : run.steps.find(
            (step) =>
              step.step !== undefined &&
              step.step > reply &&
              step.step <= reply + tools.length &&
              step.tool === tool &&
              !named.has(step.id)
          );
    if (known !== undefined) {
      named.add(known.id);
      return;
    }
    added.push({
      count: 0,
      header,
      id: nextId(run) + added.length,
      items: [],
      kind: "",
      label: labels[index] ?? "",
      refused: false,
      running: true,
      slip: false,
      step: undefined,
      tool,
    });
  });
  return [
    ...run.steps.map((step) =>
      named.has(step.id) && step.header === "" && header !== "" ? { ...step, header } : step
    ),
    ...added,
  ];
}

/** The event an `end` frame is to the run's state: a turn that waits on its plan, done, failed,
 *  stopped where it offers Continue - a bound of its level, or the model's provider refusing part
 *  of what the run found, whose words its reason gives (`endWords`) - or stopped with nothing to
 *  continue. */
function endEvent(end: RunEnd): RunEvent {
  switch (end.state) {
    case "waiting":
      return "propose_plan";
    case "done":
      return "end_done";
    case "failed":
      return "end_failed";
    default:
      return end.offers.includes("continue") ? "bound" : "end_stopped";
  }
}

/** The run with one more frame, `name` and `data` as its stream carried them. A frame of a name
 *  this page does not know is one a later Dispatch added, and changes nothing. Steps are keyed by
 *  their step number: a call frame adds its call as running, a tool frame ends the step of its own
 *  number whether its call frame came before it or not, and a reply frame adds only the calls no
 *  call or tool frame named, since the agent saves a reply once its calls ran. */
export function withRunFrame(run: RunFrames, name: string, data: unknown): RunFrames {
  const fields = objectOf(data);
  switch (name) {
    case "call": {
      const step = numberOf(fields.step);
      // The first call frame of a model call carries the prose it wrote so far beside its calls,
      // which heads them; a later one carries none.
      const header = typeof fields.text === "string" ? fields.text : run.callsHeader;
      const at = step === undefined ? -1 : run.steps.findIndex((known) => known.step === step);
      // A call whose tool frame came first has ended, and keeps its one line, headed by this
      // frame's prose where it has none. A step of its number that still runs is a call cut with
      // its agent, which the model call made again under the same number: this call takes its
      // line.
      const held = run.steps[at];
      if (held?.running === false) {
        return held.header === "" && header !== ""
          ? {
              ...run,
              callsHeader: header,
              steps: run.steps.map((other, index) => (index === at ? { ...held, header } : other)),
            }
          : { ...run, callsHeader: header };
      }
      const started: RunStep = {
        count: 0,
        header,
        id: held?.id ?? nextId(run),
        items: [],
        kind: textOf(fields.kind),
        label: textOf(fields.label),
        refused: false,
        running: true,
        slip: false,
        step,
        tool: textOf(fields.tool),
      };
      return {
        ...run,
        callsHeader: header,
        state: runNext(run.state, "step").state,
        steps:
          at === -1
            ? [...run.steps, started]
            : run.steps.map((known, index) => (index === at ? started : known)),
      };
    }
    case "reply": {
      const tools = textsOf(fields.tools);
      const reply = textOf(fields.text);
      const calls = tools.length > 0;
      return {
        ...run,
        answerLast: !calls && reply !== "",
        callsHeader: calls ? reply : "",
        replies: calls || reply === "" ? run.replies : [...run.replies, reply],
        state: runNext(run.state, "step").state,
        steps: withReplySteps(
          run,
          numberOf(fields.step),
          tools,
          textsOf(fields.labels),
          calls ? reply : ""
        ),
        // A reply that made calls ends no turn; the one that does says whether it asks.
        waitsForPerson: calls ? run.waitsForPerson : fields.waits_for_person === true,
        withheld: typeof fields.withheld === "number" ? fields.withheld : run.withheld,
      };
    }
    case "tool": {
      const tool = textOf(fields.tool);
      const step = numberOf(fields.step);
      // The call's frame ends the step of its own number; else the first call of its tool a
      // reply named that no frame numbered. A call nothing named (a row of an approved plan the
      // runner made, the review of a plan, a call whose call frame was lost) is a step of its own.
      const numbered =
        step === undefined ? -1 : run.steps.findIndex((known) => known.step === step);
      const at =
        numbered !== -1
          ? numbered
          : run.steps.findIndex(
              (known) => known.running && known.step === undefined && known.tool === tool
            );
      const ended: RunStep = {
        count: numberOf(fields.count) ?? 0,
        header: run.steps[at]?.header ?? "",
        id: run.steps[at]?.id ?? nextId(run),
        items: textsOf(fields.items),
        kind: textOf(fields.kind),
        label: textOf(fields.label),
        refused: fields.error === true,
        running: false,
        slip: fields.slip === true,
        step,
        tool,
      };
      const steps =
        at === -1
          ? [...run.steps, ended]
          : run.steps.map((known, index) => (index === at ? ended : known));
      return { ...run, lastEnded: ended.id, state: runNext(run.state, "step").state, steps };
    }
    case "block": {
      if (typeof data !== "object" || data === null) {
        return run;
      }
      const block = data as GoingBlockData;
      // The step that drew the block says what it drew, so its fold names a table a table: its
      // tool frame's kind is the label's, chart, whatever it drew.
      const drawn = DRAWN_KINDS[block.kind] === true ? block.kind : undefined;
      return {
        ...run,
        blocks: [...run.blocks, block],
        steps:
          drawn === undefined
            ? run.steps
            : run.steps.map((step) =>
                step.id === run.lastEnded && step.kind === "chart" ? { ...step, kind: drawn } : step
              ),
      };
    }
    case "plan": {
      const number = typeof fields.plan === "number" ? fields.plan : 0;
      const state = textOf(fields.state);
      return {
        ...run,
        plan: { number, state },
        planReads: run.planReads + 1,
        state: runNext(run.state, state === "waiting" ? "propose_plan" : "step").state,
      };
    }
    case "end": {
      const end: RunEnd = {
        issue: textOf(fields.issue),
        offers: textsOf(fields.offers),
        reason: textOf(fields.reason),
        state: textOf(fields.state),
      };
      const state = runNext(run.state, endEvent(end)).state;
      // A run whose end comes is no longer running anything: a call no tool frame ended was cut,
      // and draws no line.
      const steps = run.steps.filter((step) => !step.running);
      // No plan frame says a row ran, so the plan is read again once the run's rows have run: at
      // the run's end, and where a turn that worked ends waiting again - its approved rows ran,
      // and a row whose item changed after the approval waits to be ticked again (design v3,
      // state 6). The end of the turn that proposed the plan waits on the plan its plan frame
      // read, and reads nothing more.
      const reread =
        run.plan !== undefined &&
        (runEnded(state) ? !runEnded(run.state) : state === "waiting" && run.state === "working");
      return {
        ...run,
        end,
        planReads: reread ? run.planReads + 1 : run.planReads,
        state,
        steps,
        waitsForPerson:
          typeof fields.waits_for_person === "boolean"
            ? fields.waits_for_person
            : run.waitsForPerson,
      };
    }
    default:
      return run;
  }
}

/** The codes of the reasons a bound of its level stops a run with (`going/agent/levels.go`): such
 *  a run offers Continue. Every other stop is its person's or its grant's, which offers none. */
const BOUND_REASONS: Readonly<Record<string, true>> = {
  CALL_BUDGET: true,
  OUTPUT_TOKEN_BUDGET: true,
  TIME_BUDGET: true,
};

/** The run's state as its record says it, for a run whose frames are not read yet (History): a
 *  stop is a bound's only where its reason's code is one of `BOUND_REASONS`. The record holds no
 *  offers, so a stop the model's provider refused reads as one with nothing to continue - "Ask in
 *  other words" - until its end frame says whether it offers Continue. */
export function stateOfRecord(
  state: GoingRun["state"],
  stopReason: GoingRun["stop_reason"]
): RunState {
  switch (state) {
    case "queued":
    case "working":
      return "working";
    case "waiting":
      return "waiting";
    case "done":
      return "done";
    case "failed":
      return "failed";
    case "stopped":
      return BOUND_REASONS[stopReason?.split(":", 1)[0] ?? ""] === true
        ? "stopped_bound"
        : "stopped_person";
    default:
      return "idle";
  }
}
