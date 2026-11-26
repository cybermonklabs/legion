import {
  GOING_ERROR_ANSWER_DID_NOT_ARRIVE,
  GOING_ERROR_ANSWER_FAILED,
  GOING_ERROR_ANSWER_WITHDRAWN,
  GOING_ERROR_NO_MODEL_CONFIGURED,
} from "@legion/contracts";
import { type ReactNode, useMemo } from "react";
import { Link } from "react-router-dom";

import {
  borderDefault,
  borderStrong,
  dangerText,
  linkHoverText,
  linkText,
  secondaryButtonCompact,
  textMutedOnSurface,
  textPrimaryOnSurface,
} from "../../theme/classes";
import { endWords } from "../going/run-states";
import { ItemChip } from "../refs/ItemChip";
import { MarkdownBody } from "../refs/MarkdownBody";
import { nodeRouteFromHref } from "../refs/routes";
import { AnswerBlock } from "./blocks/AnswerBlock";
import { answerSettled, type GoingAnswer } from "./useGoingAnswer";

const noteClassName = `text-xs ${textMutedOnSurface}`;

/** The run's failure (`run-states.ts`) that each failure of the short answer is, by the code its
 *  `error` frame carries: the model's time ran out, the model failed or refused, the Dispatch
 *  has no answering model, or the answer is withdrawn, as a comment or an issue message it read
 *  was deleted since, which a run reads in the same words (`WITHDRAWN_WORDS`). */
const RUN_FAILURES: Readonly<Record<string, string>> = {
  [GOING_ERROR_ANSWER_DID_NOT_ARRIVE]: "MODEL_CALL_TIME",
  [GOING_ERROR_ANSWER_FAILED]: "MODEL_FAILED",
  [GOING_ERROR_ANSWER_WITHDRAWN]: GOING_ERROR_ANSWER_WITHDRAWN,
  [GOING_ERROR_NO_MODEL_CONFIGURED]: "NO_MODEL",
};

/** The one next step a failed short answer offers: the run of the words, which the line Ask
 *  starts. */
const FULL_ANSWER_STEP = "Ask for the full answer.";

/**
 * A failed short answer in the reader's words (design v3, rules 1 and 7), by the code its `error`
 * frame carries: why, in the sentence a run's same failure says (`endWords`), then its one next
 * step, the full answer. A code this page does not know, one a later Dispatch added, is said in
 * words too, never as the code. A Dispatch with no answering model says the one sentence of
 * state 13 and offers no step, as a run does.
 */
function errorText(code: string): string {
  const reason = RUN_FAILURES[code];
  const words = reason === undefined ? undefined : endWords("failed", reason, "", "");
  if (words?.noModel === true) return words.text;
  return `${words?.text ?? "Dispatch could not write a short answer."} ${FULL_ANSWER_STEP}`;
}

/** What the server did to the answer, in the reader's words: `dropped` pieces of it - a link, an
 *  image, a tag, an address - were not passed on as the model wrote them. */
function changedText(dropped: number): string {
  return dropped === 1
    ? "the answer changed: a link was dropped"
    : `the answer changed: ${dropped} links, images or tags were dropped`;
}

/** Whether `text`, a text frame's markdown with every punctuation byte escaped, writes `key` as a
 *  word of its own once its escapes are read. */
function writesKey(text: string, key: string): boolean {
  return text
    .replace(/\\(.)/gu, "$1")
    .split(/(?<![\w-])([A-Z][A-Z0-9]{1,9}-[1-9][0-9]*)(?![\w-])/u)
    .some((part, index) => index % 2 === 1 && part === key);
}

/** The issue an item's `href` belongs to - an issue's own page, its spec, one of its documents,
 *  asks, comments or messages - and undefined for an item of no issue, such as a project's
 *  document. */
function issueOf(href: string): string | undefined {
  const route = nodeRouteFromHref(href);
  return route !== undefined && "key" in route ? route.key : undefined;
}

/**
 * Dispatch's short answer to the words in the bar (R3), drawn as a compact card above the hits
 * (the one layout C1 shows; the card never sits among the rows of the list, so no option of the
 * listbox holds a button). Its text shows at most 3 lines, whatever the answer's length: the card
 * is small by its layout, not by selecting, shortening or rewriting an answer, and the whole
 * answer is the panel's. Under the text, `Open in panel` (`onOpenInPanel`) is a button of its own,
 * which the bar offers only for an answer to the words in the box now - never for one typed past
 * (`stale`), whose words are no longer the question - and which submits those words, as Enter on
 * the line Ask does. Nothing the card draws writes anything or opens the panel by itself.
 *
 * The text is the stream's `text` frames drawn through the page's markdown renderer as plain
 * text: the server escaped every punctuation byte of it, so no link, image, tag or code span
 * comes of anything the model wrote. The text's links are its `item` frames: an issue an item
 * frame names is its chip (design v3, state 4), once, in the text where the text writes its key -
 * the one reference the renderer reads in it - and beneath the text where it does not, as is every
 * other item, by its title, but for an item of an issue the text draws as its chip. The block,
 * where the answer draws one, follows the text, as its frame follows the text's; a table's column
 * of issue keys links each issue too (`TableBlock`), a key the server's steps put there from the
 * run's own reads. Each line below says what the stream said about the answer: that it was cut,
 * that the server changed it, that the model wrote none, that it rests on no source, how fresh the
 * roster it read was, or that it failed. A failure says why and its own next step
 * (`errorText`), which the button is.
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
  onOpenInPanel,
  stale,
}: {
  readonly answer: GoingAnswer;
  /** Submits the words in the box as a run in the panel; undefined where the card offers none. */
  readonly onOpenInPanel: (() => void) | undefined;
  /** Whether `answer` answers words the person has typed past. */
  readonly stale: boolean;
}): ReactNode {
  const answering = !stale && !answerSettled(answer);
  // A Dispatch with no answering model says its one sentence and offers no step.
  const offered =
    onOpenInPanel !== undefined && !stale && answer.error !== GOING_ERROR_NO_MODEL_CONFIGURED;
  return (
    <section
      aria-busy={answering}
      aria-label="Answer"
      className={`shrink-0 border-b ${borderDefault}`}
      data-run={answer.runId}
      data-state={stale ? "stale" : answer.state}
      data-testid="going-answer"
    >
      {/* The text scrolls inside its own box, short of the button, so a tall block or a long
          list of sources never pushes the button out of reach. Its padding keeps a link's focus
          ring clear of the box's edge, which would clip it. */}
      <div
        className={`max-h-[28vh] overflow-y-auto px-3 pt-3 text-sm ${offered ? "pb-1" : "pb-3"}`}
        data-testid="going-answer-body"
      >
        <AnswerBody answer={answer} stale={stale} />
      </div>
      {offered ? (
        <div className="flex justify-end px-3 pt-1 pb-3">
          <button
            className={secondaryButtonCompact}
            data-testid="going-answer-open"
            onClick={onOpenInPanel}
            type="button"
          >
            Open in panel
          </button>
        </div>
      ) : null}
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
  // The issues of the item frames whose keys the text writes are drawn there, as their chips,
  // each once however often the text writes its key (`MarkdownBody`); every other item is drawn
  // beneath it, but for an item of an issue the text draws as its chip, such as that issue's spec:
  // the server names such an item by its issue's title, so it would draw that title a second
  // time, as a link of its own, where the chip already opens the issue.
  const inText = useMemo(
    () =>
      answer.items
        .filter((item) => item.kind === "issue" && writesKey(answer.text, item.id))
        .map((item) => item.id),
    [answer.items, answer.text]
  );
  const beneath = answer.items.filter(
    (item) => !inText.includes(item.kind === "issue" ? item.id : (issueOf(item.href) ?? ""))
  );
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
          // The answer carries the ✦ the line on top marks the agent's own words with, so it
          // reads as the agent's answer and not as one more hit (design v3, state 1).
          <div className="flex gap-2">
            <span
              aria-hidden="true"
              className={`shrink-0 text-sm ${textMutedOnSurface}`}
              data-testid="going-answer-mark"
            >
              ✦
            </span>
            <div
              className={`line-clamp-3 min-w-0 flex-1 ${stale ? textMutedOnSurface : textPrimaryOnSurface}`}
              data-testid="going-answer-text"
            >
              <MarkdownBody
                chips
                knownIssueKeys={inText}
                markdown={answer.text}
                referenceText={inText.join(" ")}
                softBreaks="line"
                surface="agent-turn-text"
              />
            </div>
          </div>
        )}
        {answer.block === undefined ? null : (
          // The block is its answer's: what the reader did with it - a table's Show all - goes
          // with that answer, so the next run's table, whose first words and block can reach
          // the page in one render, starts as a table of its own, at its first rows.
          <AnswerBlock block={answer.block} key={answer.runId} />
        )}
        {beneath.length === 0 ? null : (
          <ul
            aria-label="Sources"
            className="mt-2 flex flex-wrap items-center gap-x-3 gap-y-1 text-xs"
            data-testid="going-answer-items"
          >
            {beneath.map((item) => (
              <li key={item.href}>
                <Link
                  className={
                    item.kind === "issue"
                      ? "no-underline"
                      : `underline ${linkText} ${linkHoverText}`
                  }
                  data-kind={item.kind}
                  data-testid="going-answer-item"
                  to={item.href}
                >
                  {item.kind === "issue" ? (
                    <ItemChip issue={item.id} />
                  ) : item.title === "" ? (
                    item.id
                  ) : (
                    item.title
                  )}
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
              This answer rests on no source.
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
