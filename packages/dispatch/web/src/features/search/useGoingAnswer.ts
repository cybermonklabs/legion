import {
  GOING_EVENT_BLOCK,
  GOING_EVENT_CHANGED,
  GOING_EVENT_CUT,
  GOING_EVENT_DECISION,
  GOING_EVENT_END,
  GOING_EVENT_ERROR,
  GOING_EVENT_ITEM,
  GOING_EVENT_NONE,
  GOING_EVENT_ROSTER,
  GOING_EVENT_TEXT,
  GOING_REQUEST_TTL_MS,
} from "@legion/contracts";
import { useCallback, useEffect, useRef, useState } from "react";

import { ApiError, api, type WriteBounds } from "../../api/client";
import type {
  GoingBlockData,
  GoingContextInput,
  GoingEndEvent,
  GoingEvent,
  GoingItemEvent,
  GoingLine,
  GoingRosterEvent,
} from "../../api/types";
import { goingBounds } from "../going/going-store";
import { refusalText } from "./offer";
import { MEANING_PAUSE_MS } from "./useTieredSearch";

/** Where one answer stands: asked and not yet streaming, streaming, ended with its `end` frame,
 *  or failed - an `error` frame, or a request that never reached the stream. */
export type GoingAnswerState = "asking" | "streaming" | "ended" | "failed";

/** Whether the answer has settled: ended or failed, so no frame of it is still to come. */
export function answerSettled(answer: GoingAnswer): boolean {
  return answer.state === "ended" || answer.state === "failed";
}

/** Whether the answer has come to something of its own for the panel to draw: its first words,
 *  the model writing none, or its end - an `end` or an `error` frame, or a request that never
 *  reached the stream. Until then the panel keeps the answer to the words before (`earlier`). */
function answerArrived(answer: GoingAnswer): boolean {
  return answer.text !== "" || answer.none || answerSettled(answer);
}

/** What the panel keeps once the person types past `before`, the answer to the words they typed
 *  past, where it kept `kept` until then: `before` where it has words of its own, as it stood at
 *  the keystroke; nothing where it came to no words - none, or a failure - since that took the
 *  place of whatever was kept; and `kept` while it had come to nothing yet. A replay answers no
 *  words the person typed, so it is never kept. */
function keptAnswer(
  before: GoingAnswer | undefined,
  kept: GoingAnswer | undefined
): GoingAnswer | undefined {
  if (before === undefined || before.replayed) {
    return kept;
  }
  if (before.text !== "") {
    return before;
  }
  return answerArrived(before) ? undefined : kept;
}

/** What the panel keeps (`keptAnswer`), and the words, project and `enabled` of the render that
 *  worked it out: a change of any of them works it out again. */
interface Keeping {
  readonly answer: GoingAnswer | undefined;
  readonly words: string;
  readonly project: string;
  readonly enabled: boolean;
}

/** `answer` where it answers `words`, the words in the box: the replay while the box is empty,
 *  else the run of those very words. */
function answerTo(answer: GoingAnswer | undefined, words: string): GoingAnswer | undefined {
  if (answer === undefined) {
    return undefined;
  }
  if (answer.replayed) {
    return words === "" ? answer : undefined;
  }
  return answer.words === words ? answer : undefined;
}

/**
 * One run of Dispatch's answer to the words in the bar, as the frames of its stream have built
 * it so far. Every field is a frame's: the text is the `text` frames joined, the items are the
 * `item` frames in order, and so on. The decision model's `decision` frame names the line on top
 * (`line`), which the bar draws above the hits and the panel does not.
 */
export interface GoingAnswer {
  /** The run, which addresses its stream; undefined while the request that starts it is out. */
  readonly runId: string | undefined;
  /** The words this answer answers. */
  readonly words: string;
  /** Whether this is the person's last answered run, drawn again from its stream as the bar
   *  opens with an empty box, rather than an answer to the words in the box. */
  readonly replayed: boolean;
  readonly state: GoingAnswerState;
  /** The answer's text so far: plain text whose every ASCII punctuation byte is escaped. */
  readonly text: string;
  /** What the answer cites, each its own link: the only links an answer carries. */
  readonly items: readonly GoingItemEvent["data"][];
  /** The words the answer was cut to, where it was cut. */
  readonly cut: number | undefined;
  /** How many pieces of the answer the server did not pass on as the model wrote them. */
  readonly changed: number | undefined;
  /** Whether the model wrote no answer text at all. */
  readonly none: boolean;
  /** What the answer came to, once it ended. */
  readonly end: GoingEndEvent["data"] | undefined;
  /** The `GOING_ERROR_*` code of a run that failed. */
  readonly error: string | undefined;
  /** Why the page could not ask or read the answer: a request that failed short of the stream. */
  readonly unreachable: string | undefined;
  readonly roster: GoingRosterEvent["data"] | undefined;
  readonly block: GoingBlockData | undefined;
  /** The line on top the decision model's action gives (`decision` frame), undefined until that
   *  frame comes, and for a run that carries none. */
  readonly line: GoingLine | undefined;
}

/** The last run of this page that answered, kept outside the bar so that reopening the bar draws
 *  it again from its stream while Dispatch still keeps it (`GOING_REQUEST_TTL_MS`), and nothing
 *  of it - not a chart, not a copy of its text - is stored anywhere else. */
interface AnsweredRun {
  readonly id: string;
  readonly words: string;
  readonly createdAt: string;
}

let lastAnswered: AnsweredRun | undefined;

/** The run reopening the bar draws again: the last that answered, while Dispatch keeps it. */
function lastAnsweredRun(): AnsweredRun | undefined {
  if (lastAnswered === undefined) {
    return undefined;
  }
  const created = Date.parse(lastAnswered.createdAt);
  if (Number.isNaN(created) || Date.now() - created >= GOING_REQUEST_TTL_MS) {
    lastAnswered = undefined;
  }
  return lastAnswered;
}

/** Forgets the last answered run: a test's reset, and a replay Dispatch no longer serves. */
export function forgetAnsweredRun(): void {
  lastAnswered = undefined;
}

function freshAnswer(words: string, replayed: boolean, runId?: string): GoingAnswer {
  return {
    block: undefined,
    changed: undefined,
    cut: undefined,
    end: undefined,
    error: undefined,
    items: [],
    line: undefined,
    none: false,
    replayed,
    roster: undefined,
    runId,
    state: "asking",
    text: "",
    unreachable: undefined,
    words,
  };
}

/** The answer with one more frame of its stream. */
function withFrame(answer: GoingAnswer, frame: GoingEvent): GoingAnswer {
  switch (frame.event) {
    case GOING_EVENT_TEXT:
      return { ...answer, state: "streaming", text: answer.text + frame.data.text };
    case GOING_EVENT_ITEM:
      return answer.items.some((item) => item.href === frame.data.href)
        ? answer
        : { ...answer, items: [...answer.items, frame.data], state: "streaming" };
    case GOING_EVENT_CUT:
      return { ...answer, cut: frame.data.words };
    case GOING_EVENT_CHANGED:
      return { ...answer, changed: frame.data.dropped };
    case GOING_EVENT_NONE:
      return { ...answer, none: true };
    case GOING_EVENT_DECISION:
      return { ...answer, line: frame.data.line };
    case GOING_EVENT_BLOCK:
      return { ...answer, block: frame.data };
    case GOING_EVENT_ROSTER:
      return { ...answer, roster: frame.data };
    case GOING_EVENT_END:
      return { ...answer, end: frame.data, state: "ended" };
    case GOING_EVENT_ERROR:
      return { ...answer, error: frame.data.code, state: "failed" };
  }
}

/** The answer as it stands once the page could not ask for it or read it, saying why. */
function unreachableAnswer(answer: GoingAnswer, error: unknown): GoingAnswer {
  return {
    ...answer,
    state: "failed",
    unreachable: refusalText(error, "Dispatch did not answer."),
  };
}

/** The run the bar has out: at most one per bar. `id` is unset while its request is out. */
interface OutRun {
  readonly controller: AbortController;
  readonly replayed: boolean;
  readonly words: string;
  id: string | undefined;
  ended: boolean;
}

/** What `useGoingAnswer` answers: the answer, and the earlier answer the panel keeps in its
 *  place. */
export interface GoingAnswerHook {
  readonly answer: GoingAnswer | undefined;
  /** The answer to words the person has typed past, as it stood at the keystroke, which the
   *  panel keeps marked as theirs while `answer` has come to nothing of its own
   *  (`answerArrived`); undefined while the box is empty, and never a replay. */
  readonly earlier: GoingAnswer | undefined;
}

/**
 * Dispatch's answer to the words in the bar. A run starts on the pause the bar already waits for
 * before it searches by meaning (`MEANING_PAUSE_MS`), never on a keystroke, so a person typing
 * without stopping costs no model call; the bar has one run out at most, and the next keystroke
 * and closing the bar each cancel it (`POST /api/v1/going/requests/{id}/cancel`), so a run the
 * person typed past stops spending and no frame of it reaches the panel after the keystroke. A
 * question goes with the chip's `context`, and is asked only where `enabled` says this Dispatch
 * answers at all.
 *
 * What the panel drew of the run typed past stays, as it stood at the keystroke, as the `earlier`
 * answer, until the answer to the words in the box has words of its own, or finds none, or
 * fails. A run the person typed past with no words drawn yet leaves what was kept before it;
 * emptying the box, asking the same words of another project and closing the bar keep nothing.
 *
 * A run that answered is kept outside the bar (`lastAnsweredRun`), and the bar opening again with
 * an empty box draws it from the first frame of its stream, as Dispatch still serves it.
 *
 * Answers the run of the words in the box, the replay while the box is empty, or undefined, and
 * the earlier answer.
 */
export function useGoingAnswer({
  context,
  enabled,
  login,
  project,
  words,
}: {
  /** The chip, as the question is asked with it. */
  readonly context: GoingContextInput;
  /** Whether the words are a question to ask: a query the bar searches, on a Dispatch that
   *  answers. */
  readonly enabled: boolean;
  /** The person asking, whose login each request carries (`Dispatch-Login`), as every write of
   *  the page does; undefined while the page has not read it. */
  readonly login: string | undefined;
  /** The project the words are asked of, `""` for every project: the filter chip's. */
  readonly project: string;
  /** The words in the box, trimmed. */
  readonly words: string;
}): GoingAnswerHook {
  const [answer, setAnswer] = useState<GoingAnswer | undefined>(undefined);
  // What the panel keeps of the answer to words the person typed past, worked out once, in the
  // render the words change in: that render still holds the answer they typed past, which the
  // effect below lets go only after it, so the render already draws what is kept. Where the
  // person typed on, the panel keeps what it drew of that run (`keptAnswer`); where they emptied
  // the box, the chip asks the same words of another project or the bar stops asking, it keeps
  // none.
  const [keeping, setKeeping] = useState<Keeping>(() => ({
    answer: undefined,
    enabled,
    project,
    words,
  }));
  if (keeping.words !== words || keeping.project !== project || keeping.enabled !== enabled) {
    setKeeping({
      answer:
        keeping.words !== words && words !== "" ? keptAnswer(answer, keeping.answer) : undefined,
      enabled,
      project,
      words,
    });
  }
  const out = useRef<OutRun | null>(null);
  // The chip and the asker are read as a request goes, not watched: moving the chip between
  // keystrokes is no new question, and the run asked before the move keeps the chip it was asked
  // with.
  const contextNow = useRef(context);
  contextNow.current = context;
  const loginNow = useRef(login);
  loginNow.current = login;
  // Each request under a key of its own, carrying the asker's login and held to the page's
  // silence deadline, as the one keyed path takes every write of the page.
  const bounds = useCallback((): WriteBounds => goingBounds(loginNow.current), []);

  const stop = useCallback(() => {
    const run = out.current;
    out.current = null;
    if (run === null) {
      return;
    }
    run.controller.abort();
    // A run still answering is cancelled on Dispatch, so its model call ends. One whose request
    // is still out is cancelled as that request answers (`ask`), and one that ended, or a replay
    // of one, has nothing left to stop.
    if (run.id !== undefined && !run.ended && !run.replayed) {
      api.cancelGoingRequest(run.id, bounds()).catch(() => undefined);
    }
  }, [bounds]);

  const follow = useCallback((run: OutRun, id: string, createdAt: string | undefined) => {
    const update = (next: (current: GoingAnswer) => GoingAnswer) => {
      if (out.current !== run) {
        return;
      }
      setAnswer((current) => (current?.runId === id ? next(current) : current));
    };
    api
      .streamGoingAnswer(
        id,
        (frame) => {
          if (out.current !== run) {
            return;
          }
          if (frame.event === GOING_EVENT_END || frame.event === GOING_EVENT_ERROR) {
            run.ended = true;
          }
          if (frame.event === GOING_EVENT_END && createdAt !== undefined) {
            lastAnswered = { createdAt, id, words: run.words };
          }
          update((current) => withFrame(current, frame));
        },
        run.controller.signal
      )
      .catch((error: unknown) => {
        if (run.controller.signal.aborted || out.current !== run) {
          return;
        }
        run.ended = true;
        if (run.replayed) {
          // A run Dispatch no longer serves - swept, or another person's since a sign-in - is
          // no answer to draw again, and the bar opens as it would with none kept.
          if (error instanceof ApiError && error.status === 404) {
            forgetAnsweredRun();
          }
          setAnswer(undefined);
          return;
        }
        update((current) => unreachableAnswer(current, error));
      });
  }, []);

  const ask = useCallback(
    (asked: string, askedOf: string) => {
      const run: OutRun = {
        controller: new AbortController(),
        ended: false,
        id: undefined,
        replayed: false,
        words: asked,
      };
      out.current = run;
      setAnswer(freshAnswer(asked, false));
      // The request is not aborted with the run: Dispatch may already have started it, so a run
      // the person typed past while it was out is cancelled the moment its id is known.
      const input = { context: contextNow.current, project: askedOf, words: asked };
      api.createGoingRequest(input, bounds()).then(
        (request) => {
          run.id = request.id;
          if (out.current !== run) {
            api.cancelGoingRequest(request.id, bounds()).catch(() => undefined);
            return;
          }
          setAnswer((current) =>
            current?.words === asked && !current.replayed
              ? { ...current, runId: request.id }
              : current
          );
          follow(run, request.id, request.created_at);
        },
        (error: unknown) => {
          if (out.current !== run) {
            return;
          }
          run.ended = true;
          setAnswer((current) =>
            current?.words === asked && !current.replayed
              ? unreachableAnswer(current, error)
              : current
          );
        }
      );
    },
    [bounds, follow]
  );

  // Every change of the words cancels the run out, and the pause after the last of them asks
  // the next: the same wait the meaning search makes, so a question is asked only of words the
  // person stopped on.
  useEffect(() => {
    // The run before answers words the person has typed past, so it is cancelled and its answer
    // let go; the replay stays only while the box is still empty. What the panel keeps of it the
    // render the words changed in worked out already (`keeping`).
    if (out.current?.replayed !== true || words !== "") {
      stop();
    }
    setAnswer((current) => (current?.replayed === true && words === "" ? current : undefined));
    if (!enabled) {
      return;
    }
    const timer = window.setTimeout(() => ask(words, project), MEANING_PAUSE_MS);
    return () => window.clearTimeout(timer);
  }, [ask, enabled, project, stop, words]);

  // The bar opens with an empty box: the last run that answered is drawn again, from its stream's
  // first frame, until the person types.
  useEffect(() => {
    const kept = lastAnsweredRun();
    if (kept === undefined) {
      return;
    }
    const run: OutRun = {
      controller: new AbortController(),
      ended: false,
      id: kept.id,
      replayed: true,
      words: kept.words,
    };
    out.current = run;
    setAnswer(freshAnswer(kept.words, true, kept.id));
    follow(run, kept.id, undefined);
  }, [follow]);

  // Closing the bar cancels the run it has out.
  useEffect(() => stop, [stop]);

  const current = answerTo(answer, words);
  return {
    answer: current,
    earlier: current !== undefined && answerArrived(current) ? undefined : keeping.answer,
  };
}
