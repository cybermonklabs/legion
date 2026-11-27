import { useQuery } from "@tanstack/react-query";
import { useEffect, useState } from "react";

import { goingCapabilityQuery } from "../../api/queries";
import type { GoingContextInput, GoingLine } from "../../api/types";
import { type GoingAnswer, useGoingAnswer } from "./useGoingAnswer";

/** How long the line on top waits for the decision model Jev once the pause has asked it (R2):
 *  past it the line reads Ask for those words, and a later answer is dropped. */
export const LINE_WAIT_MS = 500;

/**
 * Where the line on top stands for the words in the box (R2): the words typed and their hits
 * not shown yet (`typing`); their hits shown, before the pause asks Jev (`hits`); Jev asked and
 * not answered, under `LINE_WAIT_MS` (`pending`); Jev answered in time (`answered`, with the
 * line its action gives); Ask for good, because `LINE_WAIT_MS` passed or Enter came first
 * (`ask`); and Jev's answer dropped for coming after that (`dropped`).
 */
export type LineState =
  | { readonly kind: "typing" }
  | { readonly kind: "hits" }
  | { readonly kind: "pending" }
  | { readonly kind: "answered"; readonly line: GoingLine }
  | { readonly kind: "ask" }
  | { readonly kind: "dropped" };

/** What moves the line on top: a keystroke, the hits arriving, the pause asking Jev, Jev's
 *  answer, `LINE_WAIT_MS` passing since the pause, and Enter on the line. */
export type LineEvent =
  | { readonly kind: "keystroke" }
  | { readonly kind: "hits" }
  | { readonly kind: "pause" }
  | { readonly kind: "jev"; readonly line: GoingLine }
  | { readonly kind: "wait" }
  | { readonly kind: "enter" };

/** The line on top's state before the words in the box have hits, a pause or an answer. */
export const LINE_TYPING: LineState = { kind: "typing" };
const LINE_HITS: LineState = { kind: "hits" };
const LINE_PENDING: LineState = { kind: "pending" };
const LINE_ASK: LineState = { kind: "ask" };
const LINE_DROPPED: LineState = { kind: "dropped" };

/**
 * The line on top's next state on `event`: `state` itself where the event changes nothing, so a
 * render can tell that nothing moved. Jev's answer counts only while it is awaited: before the
 * pause there is none for these words, and once the line reads Ask for good it is dropped, so
 * the line never changes under the person's Enter. Enter settles the line as it reads.
 */
export function lineNext(state: LineState, event: LineEvent): LineState {
  switch (event.kind) {
    case "keystroke":
      return LINE_TYPING;
    case "hits":
      return state.kind === "typing" ? LINE_HITS : state;
    case "pause":
      return state.kind === "typing" || state.kind === "hits" ? LINE_PENDING : state;
    case "jev":
      if (state.kind === "pending") {
        return { kind: "answered", line: event.line };
      }
      return state.kind === "ask" ? LINE_DROPPED : state;
    case "wait":
      return state.kind === "pending" ? LINE_ASK : state;
    case "enter":
      return state.kind === "answered" || state.kind === "dropped" ? state : LINE_ASK;
  }
}

/** What the line on top reads in `state`: the line Jev's answer gives where it came in time, and
 *  Ask otherwise. */
export function lineShown(state: LineState): GoingLine {
  return state.kind === "answered" ? state.line : "ask";
}

/** What the command bar draws of Dispatch's answer to the words in the box. */
export interface GoingBar {
  /** Whether this Dispatch answers at all: its agent runs, and the line on top offers them. */
  readonly answers: boolean;
  /** Whether this Dispatch is known to have no answering model, which the bar says in the line
   *  on top's place (design v3, state 13); false until the capability read answers. */
  readonly answersOff: boolean;
  /** Whether this Dispatch is known to have no deciding model, so the line on top is always Ask
   *  and never File as a new issue (R2): the bar then offers words that matched nothing as a new
   *  issue below the hits, as main does. False until the capability read answers, and false
   *  for a deciding model that is configured, whether or not it answers in time. */
  readonly decidesOff: boolean;
  /** The answer to draw beside the hits: undefined until the hits it sits beside are shown, but
   *  for the last answer drawn again as the bar opens, and for an answer to words the person
   *  typed past, which stays until the answer to the words in the box has something of its own
   *  to draw (`useGoingAnswer`'s `earlier`). */
  readonly shownAnswer: GoingAnswer | undefined;
  /** Whether `shownAnswer` answers words the person has typed past. */
  readonly stale: boolean;
  /** Whether Jev may still name another line for the words in the box: they are asked of a
   *  model, this Dispatch has a deciding model it reaches, and the line is not settled yet. The
   *  line on top reads Ask with a faint pulsing dot meanwhile; with no deciding model it reads
   *  Ask for good, and nothing says so. */
  readonly deciding: boolean;
  /** What the line on top reads for the words in the box (`lineShown`). */
  readonly line: GoingLine;
  /** Enter on the line on top: settles it as it reads, so a later answer of Jev's for these
   *  words is dropped, and answers what it read. */
  readonly enterLine: () => GoingLine;
}

/**
 * Dispatch's short answer to the words in the command bar, and the line on top Jev chooses for
 * them (R2): asked on the same pause the meaning search waits for and only where this Dispatch
 * answers at all, with the chip the bar acts through and the project the filter chip narrows to.
 * The answer is drawn once the hits it sits beside are on the screen (`showHits`), so the list
 * paints first and the answer joins it; the last answer drawn again as the bar opens waits on
 * nothing, since an empty box lists no hits, and nor does an answer to words the person typed
 * past, which the panel keeps through the pause and the new words' hits until the next answer
 * replaces it. The line reads Ask until Jev's answer, the run's `decision` frame, names another;
 * an answer that comes `LINE_WAIT_MS` after the pause or later is dropped (`lineNext`). The short
 * answer only reads; Enter on the line on top starts a run of the agent (`features/going`).
 */
export function useGoingBar({
  context,
  enabled,
  login,
  project,
  showHits,
  words,
}: {
  /** The chip, as the question is asked with it. */
  readonly context: GoingContextInput;
  /** Whether the words are a query the bar asks a model about. */
  readonly enabled: boolean;
  /** The person asking; undefined while the page has not read it. */
  readonly login: string | undefined;
  /** The project the words are asked of, `""` for every project: the filter chip's. */
  readonly project: string;
  /** Whether the hits that answer the words are on the screen. */
  readonly showHits: boolean;
  /** The words in the box, trimmed. */
  readonly words: string;
}): GoingBar {
  const capability = useQuery(goingCapabilityQuery());
  const answers = capability.data?.answer.configured === true;
  const asked = enabled && answers;
  const { answer, earlier } = useGoingAnswer({ context, enabled: asked, login, project, words });
  // The line's state is held beside the question it is about - the words, the project they are
  // asked of and whether they are asked at all, the keys `useGoingAnswer` asks a new run under -
  // and worked out in the render from what that render holds: a new question starts it over, the
  // hits on the screen, the run of the question asked on the pause and the line its decision
  // frame names each move it on (`lineNext`). The render a new question comes in still holds the
  // answer to the one before, the old run's, until `useGoingAnswer` lets it go: that answer
  // (`before`) moves the new question's line no more.
  const [held, setHeld] = useState<{
    readonly state: LineState;
    readonly words: string;
    readonly project: string;
    readonly asked: boolean;
    readonly before: GoingAnswer | undefined;
  }>(() => ({ asked, before: undefined, project, state: LINE_TYPING, words }));
  const same = held.words === words && held.project === project && held.asked === asked;
  const before = same ? held.before : answer;
  let state = same ? held.state : lineNext(held.state, { kind: "keystroke" });
  if (showHits) {
    state = lineNext(state, { kind: "hits" });
  }
  if (answer !== undefined && answer !== before && !answer.replayed) {
    state = lineNext(state, { kind: "pause" });
    if (answer.line !== undefined) {
      state = lineNext(state, { kind: "jev", line: answer.line });
    }
  }
  if (!same || held.state !== state) {
    setHeld({ asked, before, project, state, words });
  }
  // `LINE_WAIT_MS` after the pause asked Jev, the line reads Ask for this question for good.
  const waiting = state.kind === "pending";
  useEffect(() => {
    if (!waiting) {
      return;
    }
    const timer = window.setTimeout(() => {
      setHeld((now) =>
        now.words === words && now.project === project && now.asked === asked
          ? { ...now, state: lineNext(now.state, { kind: "wait" }) }
          : now
      );
    }, LINE_WAIT_MS);
    return () => window.clearTimeout(timer);
  }, [asked, project, waiting, words]);
  const decision = capability.data?.decision;
  const unsettled = state.kind === "typing" || state.kind === "hits" || state.kind === "pending";
  return {
    answers,
    answersOff: capability.data !== undefined && !answers,
    decidesOff: decision?.configured === false,
    deciding:
      asked && decision?.configured === true && decision.reachable && words !== "" && unsettled,
    enterLine: () => {
      setHeld({ asked, before, project, state: lineNext(state, { kind: "enter" }), words });
      return lineShown(state);
    },
    line: lineShown(state),
    shownAnswer:
      earlier ?? (answer !== undefined && (answer.replayed || showHits) ? answer : undefined),
    stale: earlier !== undefined,
  };
}
