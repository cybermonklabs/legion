import {
  GOING_ERROR_ANSWER_DID_NOT_ARRIVE,
  GOING_ERROR_ANSWER_FAILED,
  GOING_ERROR_NO_MODEL_CONFIGURED,
} from "@legion/contracts";
import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import type { GoingCapability } from "../../api/types";
import {
  borderDefault,
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

/** What the bar cannot do on this Dispatch, one line for each part the capability read says is
 *  missing, each with the test id a check reads it by. */
export function capabilityGaps(
  capability: GoingCapability
): readonly { readonly id: string; readonly text: string }[] {
  const gaps: { id: string; text: string }[] = [];
  if (!capability.answer.configured) {
    gaps.push({
      id: "going-cannot-answer",
      text: "No answering model is configured, so the bar searches and answers nothing.",
    });
  } else if (capability.answer.blocks === false) {
    gaps.push({
      id: "going-no-blocks",
      text: "The answering model refused structured output, so answers come without blocks.",
    });
  }
  if (!capability.decision.configured) {
    gaps.push({
      id: "going-cannot-decide",
      text: "No deciding model is configured, so the bar runs no action for you.",
    });
  } else if (!capability.decision.reachable) {
    gaps.push({
      id: "going-cannot-decide",
      text: "The deciding model cannot be reached now, so the bar runs no action for you.",
    });
  }
  if (!capability.session.configured) {
    gaps.push({
      id: "going-no-session",
      text: "No session is named for a new issue, so an issue the bar files starts no conversation.",
    });
  }
  if (!capability.label.configured) {
    gaps.push({
      id: "going-no-label",
      text: "No label is named for a new issue, so an issue the bar files carries none.",
    });
  }
  return gaps;
}

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
 * it rests on no source, how fresh the roster it read was, or that it failed. Below them, what
 * this Dispatch cannot do (`capabilityGaps`).
 *
 * The decision and the proposal are the bar's operations' to draw (`GoingAnswer`), so nothing of
 * them shows here.
 */
export function AnswerPanel({
  answer,
  gaps,
}: {
  readonly answer: GoingAnswer | undefined;
  readonly gaps: readonly { readonly id: string; readonly text: string }[];
}): ReactNode {
  const answering = answer !== undefined && !answerSettled(answer);
  return (
    <section
      aria-busy={answering}
      aria-label="Answer"
      className={`min-h-16 max-h-[40vh] shrink overflow-y-auto border-t px-3 py-3 text-sm ${borderDefault}`}
      data-run={answer?.runId}
      data-state={answer?.state}
      data-testid="going-answer"
    >
      {answer === undefined ? null : <AnswerBody answer={answer} />}
      {gaps.length === 0 ? null : (
        <ul
          className={`${answer === undefined ? "" : "mt-3"} space-y-1`}
          data-testid="going-capability"
        >
          {gaps.map((gap) => (
            <li className={noteClassName} data-testid={gap.id} key={gap.id}>
              {gap.text}
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}

function AnswerBody({ answer }: { readonly answer: GoingAnswer }): ReactNode {
  const answering = !answerSettled(answer);
  // An answer that ended citing nothing rests on no source of Dispatch's; one the model wrote no
  // text for says that instead, and its end frame's count of nothing says nothing more.
  const restsOnNothing = answer.end !== undefined && answer.end.items === 0 && !answer.none;
  return (
    <>
      {answer.replayed ? (
        <p className={`mb-2 ${noteClassName}`} data-testid="going-answer-replayed">
          Your last question: “{answer.words}”
        </p>
      ) : null}
      {answering && answer.text === "" ? (
        <p className={noteClassName} data-testid="going-answer-status" role="status">
          Answering…
        </p>
      ) : null}
      {answer.text === "" ? null : (
        <div className={textPrimaryOnSurface} data-testid="going-answer-text">
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
    </>
  );
}
