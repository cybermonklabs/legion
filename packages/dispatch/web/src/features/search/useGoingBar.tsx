import { useQuery } from "@tanstack/react-query";
import { useEffect, useState } from "react";

import { goingCapabilityQuery } from "../../api/queries";
import type { GoingContextInput, GoingLine } from "../../api/types";
import { type CapabilityGap, capabilityGaps } from "./CapabilityNote";
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
  /** The answer to draw beside the hits: undefined until the hits it sits beside are shown, but
   *  for the last answer drawn again as the bar opens, and for an answer to words the person
   *  typed past, which stays until the answer to the words in the box has something of its own
   *  to draw (`useGoingAnswer`'s `earlier`). */
  readonly shownAnswer: GoingAnswer | undefined;
  /** Whether `shownAnswer` answers words the person has typed past. */
  readonly stale: boolean;
  /** What this Dispatch cannot do, which the bar's foot says once from the moment it opens
   *  (`CapabilityNote`): none until the capability read answers. */
  readonly gaps: readonly CapabilityGap[];
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
  const { answer, earlier } = useGoingAnswer({
    context,
    enabled: enabled && answers,
    login,
    project,
    words,
  });
  // The line's state is held beside the words it is about, and worked out in the render from
  // what that render holds: new words start it over, the hits on the screen, the run of these
  // words asked on the pause and the line its decision frame names each move it on (`lineNext`).
  const [held, setHeld] = useState<{ readonly state: LineState; readonly words: string }>(() => ({
    state: LINE_TYPING,
    words,
  }));
  let state = held.words === words ? held.state : lineNext(held.state, { kind: "keystroke" });
  if (showHits) {
    state = lineNext(state, { kind: "hits" });
  }
  if (answer !== undefined && !answer.replayed) {
    state = lineNext(state, { kind: "pause" });
    if (answer.line !== undefined) {
      state = lineNext(state, { kind: "jev", line: answer.line });
    }
  }
  if (held.words !== words || held.state !== state) {
    setHeld({ state, words });
  }
  // `LINE_WAIT_MS` after the pause asked Jev, the line reads Ask for these words for good.
  const waiting = state.kind === "pending";
  useEffect(() => {
    if (!waiting) {
      return;
    }
    const timer = window.setTimeout(() => {
      setHeld((now) =>
        now.words === words ? { state: lineNext(now.state, { kind: "wait" }), words } : now
      );
    }, LINE_WAIT_MS);
    return () => window.clearTimeout(timer);
  }, [waiting, words]);
  return {
    answers,
    enterLine: () => {
      setHeld({ state: lineNext(state, { kind: "enter" }), words });
      return lineShown(state);
    },
    gaps: capability.data === undefined ? [] : capabilityGaps(capability.data),
    line: lineShown(state),
    shownAnswer:
      earlier ?? (answer !== undefined && (answer.replayed || showHits) ? answer : undefined),
    stale: earlier !== undefined,
  };
}
