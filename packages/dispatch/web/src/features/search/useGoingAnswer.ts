import {
  GOING_EVENT_BLOCK,
  GOING_EVENT_CHANGED,
  GOING_EVENT_CUT,
  GOING_EVENT_DECISION,
  GOING_EVENT_END,
  GOING_EVENT_ERROR,
  GOING_EVENT_ITEM,
  GOING_EVENT_NONE,
  GOING_EVENT_PROPOSAL,
  GOING_EVENT_ROSTER,
  GOING_EVENT_TEXT,
  GOING_REQUEST_TTL_MS,
} from "@legion/contracts";
import { useCallback, useEffect, useRef, useState } from "react";

import { ApiError, api, REQUEST_DEADLINE_MS, type WriteBounds } from "../../api/client";
import type {
  GoingBlockData,
  GoingContextInput,
  GoingDecisionEvent,
  GoingEndEvent,
  GoingEvent,
  GoingItemEvent,
  GoingProposalEvent,
  GoingRosterEvent,
} from "../../api/types";
import { newSendKey } from "../../lib/send-key";
import { refusalText } from "./offer";
import { MEANING_PAUSE_MS } from "./useTieredSearch";

/** Where one answer stands: asked and not yet streaming, streaming, ended with its `end` frame,
 *  or failed - an `error` frame, or a request that never reached the stream. */
export type GoingAnswerState = "asking" | "streaming" | "ended" | "failed";

/**
 * One run of Dispatch's answer to the words in the bar, as the frames of its stream have built
 * it so far. Every field is a frame's: the text is the `text` frames joined, the items are the
 * `item` frames in order, and so on. `decision` and `proposal` are what the decision model chose
 * and the arguments of each action it offered, which the bar's operations are drawn from; the
 * answer itself draws neither.
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
  readonly decision: GoingDecisionEvent["data"] | undefined;
  readonly proposal: GoingProposalEvent["data"] | undefined;
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
export function lastAnsweredRun(now: number = Date.now()): AnsweredRun | undefined {
  if (lastAnswered === undefined) {
    return undefined;
  }
  const created = Date.parse(lastAnswered.createdAt);
  if (Number.isNaN(created) || now - created >= GOING_REQUEST_TTL_MS) {
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
    decision: undefined,
    end: undefined,
    error: undefined,
    items: [],
    none: false,
    proposal: undefined,
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
export function withFrame(answer: GoingAnswer, frame: GoingEvent): GoingAnswer {
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
      return { ...answer, decision: frame.data, state: "streaming" };
    case GOING_EVENT_PROPOSAL:
      return { ...answer, proposal: frame.data };
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

/** The run the bar has out: at most one per bar. `id` is unset while its request is out. */
interface OutRun {
  readonly controller: AbortController;
  readonly replayed: boolean;
  readonly words: string;
  id: string | undefined;
  ended: boolean;
}

/** What `useGoingAnswer` answers: the answer, and how a submit inside the pause asks at once. */
export interface GoingAnswerHook {
  readonly answer: GoingAnswer | undefined;
  /** Asks for the words in the box at once where the pause is still standing, and answers
   *  whether an answer to them is coming or came: false where there is nothing to ask. */
  readonly askNow: () => boolean;
}

/**
 * Dispatch's answer to the words in the bar. A run starts on the pause the bar already waits for
 * before it searches by meaning (`MEANING_PAUSE_MS`), never on a keystroke, so a person typing
 * without stopping costs no model call; the bar has one run out at most, and the next keystroke
 * and closing the bar each cancel it (`POST /api/v1/going/requests/{id}/cancel`), so a run the
 * person typed past stops spending and its frames never reach the panel. A question goes with the
 * chip's `context`, and is asked only where `enabled` says this Dispatch answers at all.
 *
 * A run that answered is kept outside the bar (`lastAnsweredRun`), and the bar opening again with
 * an empty box draws it from the first frame of its stream, as Dispatch still serves it.
 *
 * Answers the run of the words in the box, the replay while the box is empty, or undefined, and
 * `askNow`: a submit made inside the pause (decision 4) asks at once rather than waiting it out.
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
  const out = useRef<OutRun | null>(null);
  // The pause standing for the words in the box, which a submit inside it cuts short.
  const pause = useRef<{ readonly timer: number; readonly ask: () => void } | null>(null);
  // The chip and the asker are read as a request goes, not watched: moving the chip between
  // keystrokes is no new question, and the run asked before the move keeps the chip it was asked
  // with.
  const contextNow = useRef(context);
  contextNow.current = context;
  const loginNow = useRef(login);
  loginNow.current = login;
  const wordsNow = useRef(words);
  wordsNow.current = words;
  // Each request under a key of its own, carrying the asker's login and held to the page's
  // silence deadline, as the one keyed path takes every write of the page.
  const bounds = useCallback(
    (): WriteBounds => ({
      deadlineMs: REQUEST_DEADLINE_MS,
      key: newSendKey(),
      ...(loginNow.current === undefined ? {} : { login: loginNow.current }),
    }),
    []
  );

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
        update((current) => ({
          ...current,
          state: "failed",
          unreachable: refusalText(error, "Dispatch did not answer."),
        }));
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
              ? {
                  ...current,
                  state: "failed",
                  unreachable: refusalText(error, "Dispatch did not answer."),
                }
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
    // What the run before drew answers words the person has typed past, so it goes with them;
    // the replay stays only while the box is still empty.
    if (out.current?.replayed !== true || words !== "") {
      stop();
    }
    setAnswer((current) => (current?.replayed === true && words === "" ? current : undefined));
    if (!enabled) {
      return;
    }
    const asking = () => {
      pause.current = null;
      ask(words, project);
    };
    const standing = { ask: asking, timer: window.setTimeout(asking, MEANING_PAUSE_MS) };
    pause.current = standing;
    return () => {
      window.clearTimeout(standing.timer);
      if (pause.current === standing) pause.current = null;
    };
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

  const askNow = useCallback((): boolean => {
    const standing = pause.current;
    if (standing !== null) {
      window.clearTimeout(standing.timer);
      standing.ask();
      return true;
    }
    // No pause standing: the words were asked already, or there is nothing to ask.
    const run = out.current;
    return run !== null && !run.replayed && run.words === wordsNow.current;
  }, []);

  if (answer === undefined) {
    return { answer: undefined, askNow };
  }
  if (answer.replayed) {
    return { answer: words === "" ? answer : undefined, askNow };
  }
  return { answer: answer.words === words ? answer : undefined, askNow };
}
