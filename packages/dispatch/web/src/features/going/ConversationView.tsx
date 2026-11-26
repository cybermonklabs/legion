import {
  type ReactNode,
  type RefObject,
  useEffect,
  useLayoutEffect,
  useRef,
  useState,
} from "react";
import { createPortal } from "react-dom";

import type { GoingRun } from "../../api/types";
import { CopyButton } from "../../components/CopyButton";
import {
  borderDefault,
  borderStrong,
  dangerText,
  linkHoverText,
  linkText,
  primaryButton,
  secondaryButtonCompact,
  secondaryButtonPaired,
  surfaceBg,
  surfaceMutedBg,
  textMutedOnSurface,
  textPrimaryOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { timeWords } from "../refs/dispatch-time";
import { MarkdownBody } from "../refs/MarkdownBody";
import { AnswerBlock } from "../search/blocks/AnswerBlock";
import {
  asksPerson,
  dropQueued,
  endReason,
  newConversation,
  newestRun,
  type QueuedWords,
  type RunEntry,
  runWithdrawal,
  type StartingRun,
  shownState,
  useGoing,
  useGoingValue,
} from "./going-store";
import { PlanCard } from "./PlanRows";
import { appliedWords, planWaits, undoneStateWords, undoRecord } from "./plan-rows";
import { RunUndo } from "./RunUndo";
import {
  cutsReply,
  type EndWords,
  endWords,
  type RunState,
  runEnded,
  WITHDRAWN_WORDS,
} from "./run-states";
import { foldWords, RunTimer, StepSection } from "./step-section/StepSection";
import { type PlanAnswering, usePlan } from "./usePlan";

/** How a run that has not stopped or failed stands, in the person's words, for the line under its
 *  steps; a stop and a failure read as their end says (`endWords`), and a plan that waits as its
 *  card does, with no line of its own. */
const STATE_TEXT: Readonly<Record<RunState, string>> = {
  done: "Done",
  failed: "Failed",
  idle: "",
  stopped_bound: "Stopped",
  stopped_person: "Stopped",
  waiting: "",
  working: "Working…",
};

/** How near the foot of the conversation, in pixels, the person's scroll still follows it. */
const FOOT_SLACK = 24;

/** The attribute of the turn the conversation's scroll follows no further than: the newest
 *  run's, while its plan waits (design v3, state 6). */
const FOLLOW_ANCHOR = "data-follow-anchor";

/** The line in place of "Done" under a run the person started from the line Do that ended done
 *  with no plan (`changedNothing`): whatever its answer says, nothing was changed. */
const NOTHING_CHANGED = "Nothing changed: no plan was proposed.";

/** The states of a plan frame once the plan's rows were sent to run: approved, running, or run. */
const PLAN_SENT: Readonly<Record<string, true>> = { approved: true, done: true, running: true };

/**
 * The live line under a question until its run's first step or answer comes (design v2, rule 4):
 * "Thinking…" and the time since the person asked, counted each second (`RunTimer`), at the size
 * and the grey of the step lines that take its place, so a run that has only begun reads as
 * working. A screen reader hears "Thinking…" once.
 */
function Thinking({ since }: { readonly since: number }): ReactNode {
  return (
    <p className={`text-[13px] ${textSecondaryOnSurface}`} data-testid="going-thinking">
      <span role="status">Thinking…</span>{" "}
      <RunTimer className={textMutedOnSurface} since={since} testId="going-thinking-timer" />
    </p>
  );
}

/** The person's words, as a filled grey bubble: a question of the conversation; or, `queued`,
 *  words that wait for the run to end, in a dashed outline with no fill, so they never read as
 *  sent. */
function Words({
  queued = false,
  testId,
  words,
}: {
  readonly queued?: boolean;
  readonly testId: string;
  readonly words: string;
}): ReactNode {
  const look = queued
    ? `border border-dashed ${borderStrong} ${textSecondaryOnSurface}`
    : `${surfaceMutedBg} ${textPrimaryOnSurface}`;
  return (
    <p
      className={`w-fit max-w-full rounded-lg px-3 py-2 text-sm break-words whitespace-pre-wrap ${look}`}
      data-queued={queued ? "true" : undefined}
      data-testid={testId}
    >
      {words}
    </p>
  );
}

/** Whether `run` works and nothing of it has come yet: no step, no words of the model, no block
 *  and no plan. */
function thinking(run: RunEntry): boolean {
  const frames = run.frames;
  return (
    shownState(run) === "working" &&
    frames.steps.length === 0 &&
    frames.replies.length === 0 &&
    frames.callsHeader === "" &&
    frames.blocks.length === 0 &&
    frames.plan === undefined
  );
}

/** Whether `run` was started from the line Do, as its record says, and ended done, as its end
 *  frame says, with no plan and no question for its person: it changed nothing, whatever its
 *  answer reads as. A run whose frames have not come yet is not known to have had no plan. */
function changedNothing(run: RunEntry): boolean {
  const frames = run.frames;
  return (
    run.record.line === "do" &&
    frames.end?.state === "done" &&
    frames.plan === undefined &&
    !frames.waitsForPerson
  );
}

/**
 * How long `run` worked, in milliseconds, for the line its steps fold into: the working time its
 * record says, where the record was read once the run no longer worked (History lists it so, and
 * a stop answers it); else the page's clock from the person's question to the frame that put the
 * run's work to rest, for a run whose record was read while it worked or that started here.
 * Undefined while it works, and where neither says.
 */
function workedMs(run: RunEntry): number | undefined {
  const { record } = run;
  const recordEnded = record.state !== "queued" && record.state !== "working";
  if (recordEnded && record.used.working_ms > 0) {
    return record.used.working_ms;
  }
  return run.settledAt !== undefined && (!recordEnded || run.startedHere)
    ? run.settledAt - run.since
    : undefined;
}

/**
 * When something of `record`'s run happened, for the words that say when: the time this page read
 * it (`read`), or the time the record says the run finished, whichever is earlier. Nothing of a
 * run happens after it finished, so the record's time is the one for frames read again after the
 * run ended, as History opens them; undefined where neither is known.
 */
function earliest(read: number | undefined, record: GoingRun): number | undefined {
  const finished = record.finished_at === null ? undefined : Date.parse(record.finished_at);
  if (read === undefined || finished === undefined) return read ?? finished;
  return Math.min(read, finished);
}

/**
 * What stands under an answer (design v2, state 4), small and grey - the page's grey, which no
 * reader takes for a link: `status` ("Done", "Waits for your answer" where the answer asks the
 * person a question (R8), or that nothing changed); and on hover or focus, Copy, which copies the
 * answer's text with each time in it as the words it shows (`timeWords`), and Retry, a new run of
 * the same words.
 */
function AnswerFoot({
  answer,
  onRetry,
  status,
}: {
  readonly answer: string;
  readonly onRetry: () => void;
  readonly status: string;
}): ReactNode {
  return (
    <div className="group flex flex-wrap items-center gap-1" data-testid="going-run-foot">
      <p className={`text-xs ${textMutedOnSurface}`} data-testid="going-run-state" role="status">
        {status}
      </p>
      <div className="flex items-center opacity-0 transition-opacity group-hover:opacity-100 focus-within:opacity-100 pointer-coarse:opacity-100">
        <CopyButton label="Copy the answer" value={timeWords(answer)} what="answer" />
        <button
          className={`inline-flex min-h-11 items-center rounded-lg px-2 text-xs md:min-h-8 ${linkText} ${linkHoverText}`}
          data-testid="going-run-retry"
          onClick={onRetry}
          type="button"
        >
          Retry
        </button>
      </div>
    </div>
  );
}

/**
 * How a run that stopped or failed ended (design v2, state 9, rule 7), as one grey card whatever
 * the end - a bound, a Stop or a failure look alike, a pause and not an alarm - under what the run
 * found so far (its steps folded into one line, then any answer it wrote): one sentence
 * (`endWords`) and, on the conversation's newest run (`actions`), its one next step. A bound
 * (`bound`) reads "Stopped at Standard's time limit · read 14 items so far", then Continue (↵,
 * which Enter in the empty box also does) and, below Deep, "Continue in Deep" at the same height;
 * the model's provider refusing part of what the run found reads "…, Continue, or ask in other
 * words", with Continue alone. A Stop reads "Stopped · read 1 item", then Continue (↵), since the
 * person stopped it on purpose. A failure is its sentence and Try again, or "Start a new
 * conversation" where this one grew too long. A Dispatch with no answering model says so in the
 * one sentence of state 13, and offers nothing. While a start is out (`starting`), its buttons are
 * off: a second Continue would start a second follow-up, which would take the first one's place
 * on the screen and hide it.
 */
function RunEnd({
  actions,
  bound,
  onContinue,
  onRetry,
  words,
}: {
  readonly actions: boolean;
  readonly bound: boolean;
  readonly onContinue: (level?: GoingRun["level"]) => void;
  readonly onRetry: () => void;
  readonly words: EndWords;
}): ReactNode {
  const starting = useGoingValue((of) => of.starting !== undefined);
  if (words.noModel) {
    return (
      <p
        className={`text-sm ${textSecondaryOnSurface}`}
        data-testid="going-cannot-answer"
        role="status"
      >
        {words.text}
      </p>
    );
  }
  const action = words.action;
  let next: ReactNode = null;
  if (actions && action === "continue") {
    next = (
      <>
        <button
          className={primaryButton}
          data-testid="going-run-continue"
          disabled={starting}
          onClick={() => onContinue()}
          type="button"
        >
          Continue
          <span aria-hidden="true" className="ml-2 font-normal">
            ↵
          </span>
        </button>
        {words.continueInDeep ? (
          <button
            className={secondaryButtonPaired}
            data-testid="going-run-continue-deep"
            disabled={starting}
            onClick={() => onContinue("deep")}
            type="button"
          >
            Continue in Deep
          </button>
        ) : null}
      </>
    );
  } else if (actions && action !== undefined) {
    next = (
      <button
        className={primaryButton}
        data-testid="going-run-action"
        disabled={starting}
        onClick={action === "new" ? newConversation : onRetry}
        type="button"
      >
        {action === "new" ? (
          "Start a new conversation"
        ) : (
          <>
            Try again
            <span aria-hidden="true" className="ml-2 font-normal">
              ↵
            </span>
          </>
        )}
      </button>
    );
  }
  return (
    <div
      className={`space-y-2 rounded-lg border p-3 ${borderDefault} ${surfaceMutedBg}`}
      data-action={action}
      data-testid={bound ? "going-run-bound" : "going-run-end"}
    >
      <p
        className={`text-sm ${textSecondaryOnSurface}`}
        data-testid="going-run-state"
        role="status"
      >
        {words.text}
      </p>
      {next === null ? null : <div className="flex flex-wrap items-center gap-2">{next}</div>}
    </div>
  );
}

/** A block the conversation shows whole over its surface (the bar, or the panel): its run, and
 *  its place among the run's blocks. */
interface WholeBlock {
  readonly run: string;
  readonly block: number;
}

/** One run of the conversation: the person's words, its steps, the answer with its items as
 *  chips and blocks, its plan - as the card of the newest run (the one `answering` is given for),
 *  folded to one line in an older one - how it ended, with Continue on a bound or a Stop and Try
 *  again on a failure, "Nothing changed" in place of "Done" on a run of the line Do that proposed
 *  no plan, and Undo of a run with no plan that changed something. Its steps' fold line, its
 *  answer and its plan's card come in that order, as the answer (state 4) then the applied plan
 *  (state 7) do. Once its plan's rows ran, its fold line says so ("applied 3 changes") in place of
 *  "proposed a plan", which is no longer the last thing it did, and an Undo since leaves that
 *  line as it is: it is the run's work, and the card, its rows and the line under the answer say
 *  what was undone. Once an undo of the run landed, the line under its answer says what Dispatch's
 *  record of the run says (`undoRecord`): "Undone", with the answer, which speaks of its changes,
 *  turned grey, only where none of them stays; else "Partly undone · 1 change stays", whether Undo
 *  can still take that change back, which the card then offers, or tried and could not. A message
 *  is sent, not undone, and counts as no change that stays: its row says so. A run that stopped
 *  or failed keeps its steps folded, as an answered run does, and shows any answer it wrote above
 *  how it ended (rule 3: fold the work, never the result), an answer its reply length limit cut
 *  ending with "…". A withdrawn run (`runWithdrawal`)
 *  draws its withdrawal's one line in place of its steps, its answer and its blocks, which may
 *  quote the deleted post: one another writer's delete withdrew, that line alone; one its own
 *  plan's delete withdrew, its plan's card too, whose rows and results show no word of a deleted
 *  post, so the person still reads what the run changed and that its delete is deleted, not
 *  undone. The newest run's turn, while its plan waits, is where the conversation's scroll stops
 *  (`FOLLOW_ANCHOR`). `asks` is whether its answer asks the person a question and waits for their
 *  answer; `onShowAll` opens one of its tables whole over the bar or the panel. */
function RunTurn({
  answering,
  asks,
  onContinue,
  onRetry,
  onShowAll,
  run,
}: {
  readonly answering: PlanAnswering | undefined;
  readonly asks: boolean;
  readonly onContinue: (run: string, level?: GoingRun["level"]) => void;
  readonly onRetry: (words: string) => void;
  readonly onShowAll: (whole: WholeBlock) => void;
  readonly run: RunEntry;
}): ReactNode {
  const newest = answering !== undefined;
  const own = usePlan(newest ? undefined : run.id, newest ? 0 : run.frames.planReads);
  const plan = answering ?? own;
  const state = shownState(run);
  const answer = run.frames.replies.at(-1);
  const waitsForSteps = thinking(run);
  const working = state === "working";
  // Whether an undo of the run landed, and how many of its changes stay, are Dispatch's word,
  // read off the run's record (`undoRecord`); an answer of this page's Undo says only what became
  // of each change, on the card.
  const recorded = undoRecord(run.record);
  const landed = recorded.at !== undefined;
  const undone = landed && recorded.stays === 0;
  // What this page's Undo of the run answered, where it was pressed here.
  const lines = useGoingValue((of) => of.undone.get(run.id));
  const pressed = lines !== undefined;
  const withdrawal = runWithdrawal(run);
  if (withdrawal !== undefined) {
    // A withdrawn run draws its words and one line in place of its steps, its answer and its
    // blocks, all of which may quote the deleted post. A run its own plan's delete withdrew keeps
    // its plan's card, whose rows show the delete's mark in place of each deleted post's words; a
    // run another writer's delete withdrew draws no plan, which may quote the post. Its Undo stays,
    // as Undo reads and shows none of what the run wrote: the card's, else its own.
    const card = withdrawal === "run_deleted";
    return (
      <li className="space-y-2" data-run={run.id} data-state={state} data-testid="going-run">
        <Words testId="going-run-words" words={run.record.words} />
        <p
          className={`text-sm ${textSecondaryOnSurface}`}
          data-testid="going-run-withdrawn"
          role="status"
        >
          {WITHDRAWN_WORDS[withdrawal]}
        </p>
        {card ? (
          <PlanCard
            answering={plan}
            appliedAt={earliest(run.endedAt, run.record)}
            folded={!newest}
          />
        ) : null}
        {(!card || run.frames.plan === undefined) && (run.record.undoable || pressed) ? (
          <RunUndo run={run.id} />
        ) : null}
      </li>
    );
  }
  // A call with no words draws no line, and nor does a change of an approved plan, which its
  // row's tick on the plan's card shows.
  const steps = run.frames.steps.filter((step) => step.label !== "" && step.kind !== "write");
  const reason = endReason(run);
  const ended = endWords(state, reason, run.record.level, foldWords(steps));
  // A bound that cut the reply of the run's latest model call at its output tokens cut the
  // answer, where that call wrote it: the text ends where the cut fell, mid-sentence as often as
  // not, so it ends with "…" where it is drawn and never reads as whole (review M3).
  const cut = answer !== undefined && run.frames.answerLast && cutsReply(reason);
  const shownPlan = plan.plan;
  const planWords = shownPlan === undefined ? undefined : appliedWords(shownPlan);
  return (
    <li
      className="space-y-2"
      data-run={run.id}
      data-state={state}
      data-testid="going-run"
      {...(newest && shownPlan !== undefined && planWaits(shownPlan)
        ? { [FOLLOW_ANCHOR]: "" }
        : {})}
    >
      <Words testId="going-run-words" words={run.record.words} />
      {waitsForSteps ? <Thinking since={run.since} /> : null}
      <StepSection
        // The steps fold into one line, closed, once the run no longer works (design v3, rule 3)
        // - its answer or its plan came, or it stopped or failed, whose end then counts what that
        // line counts (state 9) - and stay folded while the rows of its approved plan run, which
        // the plan's card shows as its rows' ticks (state 7).
        folded={PLAN_SENT[run.frames.plan?.state ?? ""] === true || !working}
        live={working}
        planWords={planWords}
        since={run.since}
        steps={steps}
        workedMs={workedMs(run)}
      />
      {run.reconnecting ? (
        <p
          className={`text-xs ${textMutedOnSurface}`}
          data-testid="going-run-reconnecting"
          role="status"
        >
          Reconnecting…
        </p>
      ) : null}
      {answer === undefined ? null : (
        // A line of a list whose first word is a chip - a line of a tight list, or the first
        // paragraph of a loose list's line - is marked so by the body (`data-chip-leads`,
        // MarkdownBody's `markChipLine`) whatever follows the chip, and draws no list mark, since
        // the chip marks the line as a bullet would: a list of chips draws no bullet beside each
        // (design v3, state 4). A line of words keeps its bullet, whatever chip its words reach.
        <div
          className={`text-sm ${undone ? textMutedOnSurface : textPrimaryOnSurface} [&_li[data-chip-leads]]:list-none`}
          data-cut={cut ? "true" : undefined}
          data-testid="going-run-answer"
          data-undone={undone ? "true" : undefined}
        >
          <MarkdownBody
            chips
            markdown={cut ? `${answer.trimEnd()}…` : answer}
            referenceText=""
            softBreaks="line"
            surface="agent-turn-text"
          />
        </div>
      )}
      {run.frames.withheld > 0 ? (
        <p className={`text-xs ${textSecondaryOnSurface}`} data-testid="going-run-withheld">
          {run.frames.withheld === 1
            ? "The model's content filter held back one result, so this answer may miss it."
            : `The model's content filter held back ${run.frames.withheld} results, so this answer may miss them.`}
        </p>
      ) : null}
      {run.frames.blocks.map((block, index) => (
        <AnswerBlock
          asOf={earliest(run.blocksAt[index], run.record)}
          block={block}
          // biome-ignore lint/suspicious/noArrayIndexKey: a block is its place in the run, and blocks are only ever added
          key={index}
          onShowAll={() => onShowAll({ block: index, run: run.id })}
        />
      ))}
      <PlanCard answering={plan} appliedAt={earliest(run.endedAt, run.record)} folded={!newest} />
      {state === "done" && answer !== undefined ? (
        <AnswerFoot
          answer={answer}
          onRetry={() => onRetry(run.record.words)}
          status={
            landed
              ? undoneStateWords(recorded.stays)
              : asks
                ? "Waits for your answer"
                : changedNothing(run)
                  ? NOTHING_CHANGED
                  : "Done"
          }
        />
      ) : ended !== undefined ? (
        <RunEnd
          actions={newest}
          bound={state === "stopped_bound"}
          onContinue={(level) => onContinue(run.id, level)}
          onRetry={() => onRetry(run.record.words)}
          words={ended}
        />
      ) : // While the run works, "Thinking…" or its steps' lines with the run's timer say so, and
      // a plan that waits says so by its card.
      waitsForSteps || (working && steps.length > 0) || STATE_TEXT[state] === "" ? null : (
        <p
          className={`text-xs ${textSecondaryOnSurface}`}
          data-testid="going-run-state"
          role="status"
        >
          {state === "done" && changedNothing(run) ? NOTHING_CHANGED : STATE_TEXT[state]}
        </p>
      )}
      {run.frames.plan === undefined &&
      runEnded(state) &&
      (run.record.issues ?? []).length > 0 &&
      (pressed || !undone) ? (
        <RunUndo run={run.id} />
      ) : null}
      {run.unreadable === undefined ? null : (
        <p className={`text-xs ${dangerText}`} role="alert">
          {run.unreadable}
        </p>
      )}
    </li>
  );
}

/** The question of a run whose start is out, with its "Thinking…" line from the moment of Enter
 *  (design v2, rule 4). */
function StartingTurn({ starting }: { readonly starting: StartingRun }): ReactNode {
  return (
    <li className="space-y-2" data-testid="going-run-starting">
      <Words testId="going-run-words" words={starting.words} />
      <Thinking since={starting.at} />
    </li>
  );
}

/** The words the person sent while the run worked (design v2, state 3): a dashed bubble at the
 *  foot of the conversation, "Sends when this run ends", and × to drop them. */
function QueuedTurn({ queued }: { readonly queued: QueuedWords }): ReactNode {
  return (
    <div className="mt-4 space-y-1" data-testid="going-queued">
      <Words queued testId="going-queued-words" words={queued.words} />
      <p className={`flex items-center gap-1 text-xs ${textMutedOnSurface}`}>
        Sends when this run ends
        <button
          aria-label="Drop these words"
          className="rounded px-1 hover:underline"
          data-testid="going-queued-drop"
          onClick={() => dropQueued(queued)}
          title="Drop these words"
          type="button"
        >
          ×
        </button>
      </p>
    </div>
  );
}

/** How far down `element` scrolls for the turn that holds `FOLLOW_ANCHOR` to stand at its top,
 *  under the conversation's own top padding, as the first turn stands; undefined where no turn
 *  holds it. */
function anchorTop(element: HTMLElement): number | undefined {
  const anchor = element.querySelector(`[${FOLLOW_ANCHOR}]`);
  if (anchor === null) return undefined;
  const padding = Number.parseFloat(getComputedStyle(element).paddingTop) || 0;
  const below = anchor.getBoundingClientRect().top - element.getBoundingClientRect().top;
  return Math.max(0, below + element.scrollTop - padding);
}

/** Where the conversation follows to: its foot, or, while a plan waits, no further down than its
 *  turn's top, so its card shows from its question down, with its Approve pinned at the foot of
 *  the view, rather than from the middle, under the line its steps fold into, cut in half under
 *  the header (design v3, states 6 and 8). The bar and the panel scroll alike. */
function followTop(element: HTMLElement): number {
  const anchored = anchorTop(element);
  return anchored === undefined ? element.scrollHeight : Math.min(element.scrollHeight, anchored);
}

/** Whether anything is scrolled out of view above: nothing is where the view stands at the
 *  anchor's turn, whose top then sits where the first turn's does. */
function cutAbove(element: HTMLElement): boolean {
  const anchored = anchorTop(element);
  return (
    element.scrollTop > 0 && (anchored === undefined || Math.abs(element.scrollTop - anchored) > 1)
  );
}

/**
 * The scroll of the conversation (design v2, state 8): it follows new output - `output` counts
 * what came, and `anchor` names the plan that waits, "" for none - while the person is at its
 * foot, or, while a plan waits, at its turn's top (`followTop`). Once they scroll elsewhere it
 * stays where they are, and `behind` turns true at the next output, for the "↓ New reply" pill;
 * `follow` takes them back and follows again. `cut` says whether anything is scrolled out of view
 * above, where the conversation fades out under its header rather than show a bubble cut in half.
 * How far the person follows is of one conversation (`conversation`, its id, "" for none): where
 * another opens under a view that stays, as the Inbox's Review opens one in the side panel, it
 * starts followed, at its foot or its waiting plan's turn, with nothing to be behind on.
 */
function useFollow(
  conversation: string,
  output: number,
  anchor: string
): {
  readonly behind: boolean;
  readonly cut: boolean;
  readonly follow: () => void;
  readonly onScroll: () => void;
  readonly scroller: RefObject<HTMLDivElement | null>;
} {
  const scroller = useRef<HTMLDivElement>(null);
  const following = useRef(true);
  const followed = useRef(conversation);
  const [behind, setBehind] = useState(false);
  const [cut, setCut] = useState(false);
  // biome-ignore lint/correctness/useExhaustiveDependencies: each new output, a plan that comes to wait, and another conversation are the reasons to follow
  useLayoutEffect(() => {
    if (followed.current !== conversation) {
      followed.current = conversation;
      following.current = true;
      setBehind(false);
    }
    const element = scroller.current;
    if (element === null) return;
    if (following.current) {
      element.scrollTop = followTop(element);
      setCut(cutAbove(element));
    } else {
      setBehind(true);
    }
  }, [conversation, output, anchor]);
  return {
    behind,
    cut,
    follow: () => {
      following.current = true;
      setBehind(false);
      const element = scroller.current;
      if (element !== null) element.scrollTop = followTop(element);
    },
    onScroll: () => {
      const element = scroller.current;
      if (element === null) return;
      const anchored = anchorTop(element);
      const atFoot =
        element.scrollHeight - element.scrollTop - element.clientHeight <= FOOT_SLACK ||
        (anchored !== undefined && Math.abs(element.scrollTop - anchored) <= FOOT_SLACK);
      following.current = atFoot;
      setCut(cutAbove(element));
      if (atFoot) setBehind(false);
    },
    scroller,
  };
}

/**
 * The open conversation, as the bar over the page and the panel at the side both draw it (design
 * v2, states 2 to 9): one column, newest at the bottom, that fills the space its surface gives it
 * and scrolls inside it, following new output until the person scrolls up, when "↓ New reply"
 * brings them back; while the newest run's plan waits, it follows no further than that run's
 * question, so its card reads from the top. Each run in order, its question first in a grey
 * bubble, "Thinking…" with a timer until its first step or answer comes, its steps live as they
 * arrive (`StepSection`), "Reconnecting…" while its stream is dropped, its answer with each issue
 * it names as a chip, its blocks with their captions, its plan's card - an old turn's folded to
 * one line - and how each ended, with "Done", "Waits for your answer" or, for a run of the line
 * Do that proposed no plan, "Nothing changed: no plan was proposed." and Copy and Retry under an
 * answer, Continue at a bound or a Stop and Try again after a failure; then the question of a run
 * whose start is out, and the words that wait for the newest run. A table's "Show all" opens it
 * whole over the bar or the panel that draws the conversation, and its Back or Esc closes it.
 * `answering` is the newest run's plan as the surface that draws this answers it, so Enter in that
 * surface's box and the card's Approve are one answer; `onRetry` starts a new run of a run's
 * words, as that surface's box starts one; `onContinue` continues a run a bound or its person
 * stopped, at `level` where one is given.
 */
export function ConversationView({
  answering,
  onContinue,
  onRetry,
}: {
  readonly answering: PlanAnswering;
  readonly onContinue: (run: string, level?: GoingRun["level"]) => void;
  readonly onRetry: (words: string) => void;
}): ReactNode {
  const going = useGoing();
  const starting = going.starting;
  const [whole, setWhole] = useState<WholeBlock>();
  // A run that starts a conversation of its own takes the place of the one open.
  const ids =
    starting !== undefined && starting.follows === undefined ? [] : (going.conversation ?? []);
  const runs = ids.flatMap((id) => {
    const run = going.runs.get(id);
    return run === undefined ? [] : [run];
  });
  // A table opened whole is of one run: it goes once that run's turn leaves the screen, as
  // another conversation opens under a view that stays, and does not come back with it.
  if (whole !== undefined && !runs.some((run) => run.id === whole.run)) {
    setWhole(undefined);
  }
  const queued =
    going.queued !== undefined && going.queued.run === newestRun(going)?.id
      ? going.queued
      : undefined;
  const shownPlan = answering.plan;
  const waiting = shownPlan !== undefined && planWaits(shownPlan) ? shownPlan : undefined;
  const scroll = useFollow(
    runs[0]?.record.conversation ?? "",
    runs.reduce((sum, run) => sum + run.read + 1, 0) +
      (starting === undefined ? 0 : 1) +
      (queued === undefined ? 0 : 1),
    waiting === undefined ? "" : `${waiting.run}:${waiting.plan}`
  );
  const wholeRun = whole === undefined ? undefined : going.runs.get(whole.run);
  const wholeBlock = whole === undefined ? undefined : wholeRun?.frames.blocks[whole.block];
  // The table opens over the button that opened it, so the focus moves onto its Back.
  const back = useRef<HTMLButtonElement>(null);
  useEffect(() => {
    if (whole !== undefined) back.current?.focus();
  }, [whole]);
  // The surface that holds the conversation - the bar's dialog, or the panel at the side - which
  // a table opened whole covers; the conversation's own area where it stands in neither.
  const view = useRef<HTMLDivElement>(null);
  const [surface, setSurface] = useState<HTMLElement | null>(null);
  useLayoutEffect(() => {
    setSurface(view.current?.parentElement?.closest<HTMLElement>('[role="dialog"], aside') ?? null);
  }, []);
  const wholeTable =
    whole === undefined || wholeRun === undefined || wholeBlock === undefined ? null : (
      <section
        aria-label="The whole table"
        className={`absolute inset-0 z-20 flex flex-col ${surfaceBg}`}
        data-testid="going-block-whole"
        onKeyDown={(event) => {
          if (event.key !== "Escape") return;
          // Esc closes the table first; the next one is the bar's.
          event.preventDefault();
          event.stopPropagation();
          setWhole(undefined);
        }}
      >
        <div className={`flex shrink-0 items-center border-b px-3 py-1.5 ${borderDefault}`}>
          <button
            className={secondaryButtonCompact}
            data-testid="going-block-whole-close"
            onClick={() => setWhole(undefined)}
            ref={back}
            type="button"
          >
            ‹ Back to the conversation
          </button>
        </div>
        <div className="min-h-0 flex-1 overflow-auto px-3 pb-3">
          <AnswerBlock
            asOf={earliest(wholeRun.blocksAt[whole.block], wholeRun.record)}
            block={wholeBlock}
            whole
          />
        </div>
      </section>
    );
  return (
    <div className="relative flex min-h-0 flex-1 flex-col" ref={view}>
      <div
        // What is scrolled out of view above fades out over the top 24 px rather than stop at a
        // hard line under the header, where a bubble read as cut in half (design v3, state 6).
        // The scroll keeps no padding of its own at its foot: a browser pins what sticks there -
        // a waiting plan's Approve and Discard (`PlanCard`) - to the edge of the scroll's padding,
        // so a foot padding left a strip under them where the next row showed (review R1). What
        // it holds keeps that space instead.
        className={`min-h-0 flex-1 overflow-y-auto px-3 pt-3 ${scroll.cut ? "[mask-image:linear-gradient(to_bottom,transparent,#000_24px)]" : ""}`}
        data-cut={scroll.cut ? "true" : undefined}
        data-testid="going-conversation-view"
        onScroll={scroll.onScroll}
        ref={scroll.scroller}
      >
        <div className="pb-3" data-testid="going-conversation-turns">
          {runs.length === 0 && starting === undefined ? null : (
            <ol aria-label="Conversation" className="space-y-4" data-testid="going-conversation">
              {runs.map((run, index) => (
                <RunTurn
                  answering={index === runs.length - 1 ? answering : undefined}
                  asks={index === runs.length - 1 && asksPerson(going)}
                  key={run.id}
                  onContinue={onContinue}
                  onRetry={onRetry}
                  onShowAll={setWhole}
                  run={run}
                />
              ))}
              {starting === undefined ? null : <StartingTurn starting={starting} />}
            </ol>
          )}
          {queued === undefined ? null : <QueuedTurn queued={queued} />}
        </div>
      </div>
      {scroll.behind ? (
        <button
          className={`absolute bottom-3 left-1/2 -translate-x-1/2 rounded-full border px-3 py-1 text-xs font-medium shadow ${borderDefault} ${surfaceBg} ${textPrimaryOnSurface}`}
          data-testid="going-new-reply"
          onClick={scroll.follow}
          type="button"
        >
          ↓ New reply
        </button>
      ) : null}
      {wholeTable === null || surface === null ? wholeTable : createPortal(wholeTable, surface)}
    </div>
  );
}
