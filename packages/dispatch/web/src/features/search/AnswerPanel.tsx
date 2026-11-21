import {
  GOING_ERROR_ANSWER_DID_NOT_ARRIVE,
  GOING_ERROR_ANSWER_FAILED,
  GOING_ERROR_NO_MODEL_CONFIGURED,
} from "@legion/contracts";
import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import {
  borderDefault,
  borderStrong,
  dangerText,
  linkHoverText,
  linkText,
  textMutedOnSurface,
  textPrimaryOnSurface,
} from "../../theme/classes";
import { MarkdownBody } from "../refs/MarkdownBody";
import { AnswerBlock } from "./blocks/AnswerBlock";
import { answerSettled, type GoingAnswer } from "./useGoingAnswer";

const noteClassName = `text-xs ${textMutedOnSurface}`;

/** A failed run in the reader's words, by the code its `error` frame carries. */
function errorText(code: string): string {
  switch (code) {
    case GOING_ERROR_NO_MODEL_CONFIGURED:
      return "No answer: this Dispatch has no answering model configured.";
    case GOING_ERROR_ANSWER_DID_NOT_ARRIVE:
      return "No answer: the answer did not arrive in time.";
    case GOING_ERROR_ANSWER_FAILED:
      return "No answer: the answering model failed.";
    default:
      return `No answer: the run failed (${code}).`;
  }
}

/** What the server did to the answer, in the reader's words: `dropped` pieces of it - a link, an
 *  image, a tag, an address - were not passed on as the model wrote them. */
function changedText(dropped: number): string {
  return dropped === 1
    ? "the answer changed: a link was dropped"
    : `the answer changed: ${dropped} links, images or tags were dropped`;
}

/**
 * Dispatch's answer to the words in the bar, in its own panel beside the hits. The text is the
 * stream's `text` frames drawn through the page's markdown renderer as plain text: the server
 * escaped every punctuation byte of it, so no link, image, tag or code span comes of anything the
 * model wrote, and the renderer reads no reference in it either. The answer's links are its
 * `item` frames, listed beneath it, and nothing else. The block, where the answer draws one,
 * follows the text, as its frame follows the text's. Each line below says what the stream said
 * about the answer: that it was cut, that the server changed it, that the model wrote none, that
 * it rests on no source, how fresh the roster it read was, or that it failed. What this Dispatch
 * cannot do is the bar's foot's to say (`CapabilityNote`), once, and never under an answer.
 *
 * An answer to words the person has typed past (`stale`) is drawn as it stood when they typed
 * on, its links, block and lines with it, inside a dashed outline with its text muted, under a
 * line naming the words it answers - and, where its run was still writing it then, saying it
 * stopped before it finished, so part of an answer never reads as the whole - until the next
 * answer has something of its own to draw. That line is a status, so a screen reader hears it
 * once, and the answer, which is in no live region, is not read out again; the panel is not busy
 * while it shows, as nothing more of it is coming.
 *
 * The decision the stream carries is no part of the answer, so nothing of it shows here.
 */
export function AnswerPanel({
  answer,
  stale,
}: {
  readonly answer: GoingAnswer;
  /** Whether `answer` answers words the person has typed past. */
  readonly stale: boolean;
}): ReactNode {
  const answering = !stale && !answerSettled(answer);
  return (
    <section
      aria-busy={answering}
      aria-label="Answer"
      className={`min-h-16 max-h-[40vh] shrink overflow-y-auto border-t px-3 py-3 text-sm ${borderDefault}`}
      data-run={answer.runId}
      data-state={stale ? "stale" : answer.state}
      data-testid="going-answer"
    >
      <AnswerBody answer={answer} stale={stale} />
    </section>
  );
}

function AnswerBody({
  answer,
  stale,
}: {
  readonly answer: GoingAnswer;
  readonly stale: boolean;
}): ReactNode {
  const stopped = stale && !answerSettled(answer);
  // An answer that ended citing nothing rests on no source of Dispatch's; one the model wrote no
  // text for says that instead, and its end frame's count of nothing says nothing more.
  const restsOnNothing = answer.end !== undefined && answer.end.items === 0 && !answer.none;
  // The wrapper is drawn whether or not the answer is stale, so going stale moves no part of the
  // answer to another element: only the wrapper's classes change.
  return (
    <>
      {answer.replayed ? (
        <p className={`mb-2 ${noteClassName}`} data-testid="going-answer-replayed">
          Your last question: “{answer.words}”
        </p>
      ) : null}
      {stale ? (
        <p className={`mb-2 ${noteClassName}`} data-testid="going-answer-stale" role="status">
          Answer to your earlier words: “{answer.words}”
          {stopped ? ", stopped before it finished" : null}
        </p>
      ) : null}
      <div
        className={stale ? `rounded-lg border border-dashed px-3 py-2 ${borderStrong}` : undefined}
      >
        {!answerSettled(answer) && answer.text === "" ? (
          <p className={noteClassName} data-testid="going-answer-status" role="status">
            Answering…
          </p>
        ) : null}
        {answer.text === "" ? null : (
          <div
            className={stale ? textMutedOnSurface : textPrimaryOnSurface}
            data-testid="going-answer-text"
          >
            <MarkdownBody markdown={answer.text} referenceText="" softBreaks="line" />
          </div>
        )}
        {answer.block === undefined ? null : <AnswerBlock block={answer.block} />}
        {answer.items.length === 0 ? null : (
          <ul
            aria-label="Sources"
            className="mt-2 flex flex-wrap gap-x-3 gap-y-1 text-xs"
            data-testid="going-answer-items"
          >
            {answer.items.map((item) => (
              <li key={item.href}>
                <Link
                  className={`underline ${linkText} ${linkHoverText}`}
                  data-kind={item.kind}
                  data-testid="going-answer-item"
                  to={item.href}
                >
                  {item.title === "" ? item.id : item.title}
                </Link>
              </li>
            ))}
          </ul>
        )}
        <div className="mt-2 space-y-1">
          {answer.none ? (
            <p className={noteClassName} data-testid="going-answer-none" role="status">
              no answer found
            </p>
          ) : null}
          {answer.cut === undefined ? null : (
            <p className={noteClassName} data-testid="going-answer-cut">
              answer cut to {answer.cut} words
            </p>
          )}
          {answer.changed === undefined ? null : (
            <p className={noteClassName} data-testid="going-answer-changed">
              {changedText(answer.changed)}
            </p>
          )}
          {restsOnNothing ? (
            <p className={noteClassName} data-testid="going-answer-no-source">
              rests on no source
            </p>
          ) : null}
          {answer.roster === undefined ? null : (
            <p className={noteClassName} data-testid="going-answer-roster">
              {answer.roster.source === "stored"
                ? "live roster unavailable"
                : `roster ${Math.round(answer.roster.age_ms / 1000)} s old`}
            </p>
          )}
          {answer.error === undefined ? null : (
            <p className={`text-xs ${dangerText}`} data-testid="going-answer-error" role="alert">
              {errorText(answer.error)}
            </p>
          )}
          {answer.unreachable === undefined ? null : (
            <p
              className={`text-xs ${dangerText}`}
              data-testid="going-answer-unreachable"
              role="alert"
            >
              Could not ask for an answer: {answer.unreachable}
            </p>
          )}
        </div>
      </div>
    </>
  );
}
