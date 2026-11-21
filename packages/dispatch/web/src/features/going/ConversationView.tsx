import type { ReactNode } from "react";

import {
  borderDefault,
  dangerText,
  secondaryButtonCompact,
  surfaceMutedBg,
  textMutedOnSurface,
  textPrimaryOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { MarkdownBody } from "../refs/MarkdownBody";
import { AnswerBlock } from "../search/blocks/AnswerBlock";
import { type RunEntry, shownState, useGoing } from "./going-store";
import { PlanCard } from "./PlanRows";
import { RunUndo } from "./RunUndo";
import { type RunState, runEnded } from "./run-states";
import { type PlanAnswering, usePlan } from "./usePlan";

/** How a run stands, in the person's words, for the line under its steps. */
const STATE_TEXT: Readonly<Record<RunState, string>> = {
  done: "Done",
  failed: "Failed",
  idle: "",
  stopped_bound: "Stopped at a bound",
  stopped_person: "Stopped",
  waiting: "A plan waits for you",
  working: "Working…",
};

/** The steps of a run: each tool call, working from the reply that made it until its own frame
 *  says how it ended. */
function Steps({ run }: { readonly run: RunEntry }): ReactNode {
  const steps = run.frames.steps;
  if (steps.length === 0) {
    return null;
  }
  return (
    <ol aria-label="Steps" className="space-y-0.5" data-testid="going-steps">
      {steps.map((step) => (
        <li
          className={`truncate text-xs ${step.refused ? dangerText : textMutedOnSurface}`}
          data-running={String(step.running)}
          data-testid="going-step"
          data-tool={step.tool}
          key={step.id}
          title={step.note}
        >
          {step.running ? `${step.tool}: running…` : step.note === "" ? step.tool : step.note}
        </li>
      ))}
    </ol>
  );
}

/** One run of the conversation: the person's words, its steps, the answer and blocks, its plan
 *  where it is the newest (the one `answering` is given for), how it ended with Continue on a
 *  bound, and Undo once it changed something. */
function RunTurn({
  answering,
  onContinue,
  run,
}: {
  readonly answering: PlanAnswering | undefined;
  readonly onContinue: (run: string) => void;
  readonly run: RunEntry;
}): ReactNode {
  const newest = answering !== undefined;
  const own = usePlan(newest ? undefined : run.id, newest ? 0 : run.frames.planReads);
  const plan = answering ?? own;
  const state = shownState(run);
  const answer = run.frames.replies.at(-1);
  const end = run.frames.end;
  // Undo is offered on a run that changed something: a request of it landed, or a row of its
  // plan ran.
  const changed =
    (run.record.issues ?? []).length > 0 ||
    (plan.plan?.rows ?? []).some((row) => row.state === "done");
  return (
    <li className="space-y-2" data-run={run.id} data-state={state} data-testid="going-run">
      <p
        className={`rounded-lg px-3 py-2 text-sm ${surfaceMutedBg} ${textPrimaryOnSurface}`}
        data-testid="going-run-words"
      >
        {run.record.words}
      </p>
      <Steps run={run} />
      {answer === undefined ? null : (
        <div className={`text-sm ${textPrimaryOnSurface}`} data-testid="going-run-answer">
          <MarkdownBody markdown={answer} referenceText="" softBreaks="line" />
        </div>
      )}
      {run.frames.blocks.map((block, index) => (
        // biome-ignore lint/suspicious/noArrayIndexKey: a block is its place in the run, and blocks are only ever added
        <AnswerBlock block={block} key={index} />
      ))}
      {answering === undefined ? null : <PlanCard answering={answering} />}
      <p
        className={`text-xs ${state === "failed" ? dangerText : textSecondaryOnSurface}`}
        data-testid="going-run-state"
        role="status"
      >
        {STATE_TEXT[state]}
        {end !== undefined && end.reason !== "" && state !== "done" && state !== "waiting"
          ? `: ${end.reason}`
          : null}
      </p>
      {state === "stopped_bound" ? (
        <button
          className={secondaryButtonCompact}
          data-testid="going-run-continue"
          onClick={() => onContinue(run.id)}
          type="button"
        >
          Continue
        </button>
      ) : null}
      {runEnded(state) && changed ? <RunUndo run={run.id} /> : null}
      {run.unreadable === undefined ? null : (
        <p className={`text-xs ${dangerText}`} role="alert">
          {run.unreadable}
        </p>
      )}
    </li>
  );
}

/**
 * The open conversation, as the bar over the page and the panel at the side both draw it: each run
 * in order, with its steps live as they arrive, its answer with its links, its blocks, the plan
 * card of the newest run, and how each ended. `answering` is the newest run's plan as the surface
 * that draws this answers it, so Enter in that surface's box and the card's Approve are one
 * answer.
 */
export function ConversationView({
  answering,
  onContinue,
}: {
  readonly answering: PlanAnswering;
  readonly onContinue: (run: string) => void;
}): ReactNode {
  const going = useGoing();
  const ids = going.conversation ?? [];
  const runs = ids.flatMap((id) => {
    const run = going.runs.get(id);
    return run === undefined ? [] : [run];
  });
  if (runs.length === 0) {
    return null;
  }
  return (
    <ol
      aria-label="Conversation"
      className={`max-h-[50vh] min-h-0 space-y-4 overflow-y-auto border-b px-3 py-3 ${borderDefault}`}
      data-testid="going-conversation"
    >
      {runs.map((run, index) => (
        <RunTurn
          answering={index === runs.length - 1 ? answering : undefined}
          key={run.id}
          onContinue={onContinue}
          run={run}
        />
      ))}
    </ol>
  );
}
