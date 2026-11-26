import { type ReactNode, type RefObject, useId, useMemo, useRef } from "react";
import { Link, useParams } from "react-router-dom";

import type { GraphReferences } from "../../../api/types";
import { pageInkText, pageQuietText } from "../../../theme/classes";
import { MarkdownBody } from "../../refs/MarkdownBody";
import { standsFailed } from "../../refs/reference-target";
import { Timestamp } from "../../refs/Timestamp";
import { NotFoundPage } from "../../shell/NotFoundPage";
import { useDocumentTitle } from "../../shell/useDocumentTitle";
import { DetailsErrorLine, retryRead, useRetryHandOff } from "../details-error";
import { drawnLine, firstLineOf } from "../issue-tree";
import { NoTooltips } from "../NoTooltips";
import { referenceStandIns } from "../one-line";
import { authorText, pageChrome } from "../page-structure";
import {
  ANSWER_LINES,
  CONTROL_LABELS,
  NODE_REFERENCES_WORDS,
  RUN_PAGE_WORDS,
  sentenceCase,
} from "../page-words";
import { ROW_LINK } from "../row-references";
import { RunReferenceNames } from "../run-references/run-references";
import { type RunChange, type RunPart, runView, useRun } from "./run-page";

/** The page's heading: the word for what a run is. */
const RUN_WORD = sentenceCase(NODE_REFERENCES_WORDS.kinds.interaction);

/** The look of a section's heading, as Details draws its sections' names. */
const SECTION_HEADING = `text-xs leading-4 font-semibold tracking-[0.08em] uppercase ${pageQuietText}`;

/** A link the page draws inside the run's own words. */
const LINK = `dispatch-inline-link ${ROW_LINK} ${pageInkText}`;

/**
 * A command bar run's own page at `/runs/<id>`: the asker's question, the answer frame by frame,
 * and what the run changed. The server answers a run only to the person who asked it, so an id
 * that names no run, and a run the reader may not open, draw the app's not-found page and
 * nothing of this one.
 */
export function RunPage(): ReactNode {
  const { id } = useParams();
  const { notFound, query } = useRun(id);
  useDocumentTitle(`${RUN_WORD} · Dispatch`, !notFound);
  // The line stands while the run's read stands failed (`standsFailed`), as each line of Details
  // does: a read that holds no answer has its failure cleared as it runs again, which would take
  // the line, and its Retry with the focus, away at the press. While the read is out the line
  // stays, its Retry busy and keeping the focus; a read that fails again says its failure again
  // in the same line (`failure`), and Retry keeps the focus. An answer replaces the line, and so
  // does the not-found page where the server answers 400 or 404 (`notFound`, read from the
  // failure the read ended in). Where the reader's press of Retry took the line away with the
  // focus on it, the focus goes on (`useRetryHandOff`) to what that commit drew: the page's
  // heading, as the history's Retry hands it to its own (`IssueHistory.tsx`), or the not-found
  // page's link, found through the wrapper the not-found page is always drawn in.
  const heading = useRef<HTMLHeadingElement>(null);
  const notFoundPage = useRef<HTMLDivElement>(null);
  const retry = useRetryHandOff(
    () => heading.current ?? notFoundPage.current?.querySelector<HTMLElement>("a[href]")
  );
  if (notFound) {
    return (
      <div className="contents" ref={notFoundPage}>
        <NotFoundPage />
      </div>
    );
  }
  const failed = standsFailed(query);
  return (
    <div data-issue-page="">
      <NoTooltips>
        {failed ? (
          <DetailsErrorLine
            error={query.error}
            failure={query.errorUpdateCount}
            message={ANSWER_LINES.run_unreadable}
            onRetry={retryRead(query)}
            {...retry}
            retrying={query.isFetching}
          />
        ) : null}
        {query.data !== undefined ? (
          <RunAnswer answer={query.data} heading={heading} />
        ) : failed ? null : (
          <p className={`text-sm ${pageQuietText}`} {...pageChrome}>
            {ANSWER_LINES.loading_older}
          </p>
        )}
      </NoTooltips>
    </div>
  );
}

/** The run as its read answered it. `heading` is the page's `h1`, which takes the focus a
 *  Retry hands on. */
function RunAnswer({
  answer,
  heading,
}: {
  answer: GraphReferences;
  heading: RefObject<HTMLHeadingElement | null>;
}): ReactNode {
  const view = useMemo(() => runView(answer), [answer]);
  const questionHeading = useId();
  const changedHeading = useId();
  return (
    <article className={`flex min-w-0 max-w-3xl flex-col gap-6 ${pageInkText}`}>
      <header className="flex flex-wrap items-baseline gap-x-3 gap-y-1">
        <h1
          className="text-xl font-semibold outline-none"
          ref={heading}
          tabIndex={-1}
          {...pageChrome}
        >
          {RUN_WORD}
        </h1>
        {view.at === null ? null : (
          <Timestamp at={view.at} className={`text-xs ${pageQuietText}`} />
        )}
      </header>
      <section aria-labelledby={questionHeading} className="flex min-w-0 flex-col gap-1.5">
        <h2 className={SECTION_HEADING} id={questionHeading} {...pageChrome}>
          {RUN_PAGE_WORDS.question_heading}
        </h2>
        <p className="whitespace-pre-wrap [overflow-wrap:anywhere]" {...authorText}>
          {/* The question as the asker typed it, her characters and never markdown: the reading
              list's `run-page-question`. */}
          {view.question}
        </p>
      </section>
      <div className="flex min-w-0 flex-col gap-3">
        {view.parts.map((part, index) => (
          // biome-ignore lint/suspicious/noArrayIndexKey: a part is its place in the answer, which its frames' order fixes.
          <RunAnswerPart key={index} part={part} />
        ))}
      </div>
      <section aria-labelledby={changedHeading} className="flex min-w-0 flex-col gap-1.5">
        <h2 className={SECTION_HEADING} id={changedHeading} {...pageChrome}>
          {RUN_PAGE_WORDS.what_changed}
        </h2>
        <ChangedNodes changed={view.changed} />
      </section>
    </article>
  );
}

/** Cut the first line before substituting reference names, so no cut falls inside a name.
 *  Twin of `spec-filter/spec-decided-by.ts`'s `namedFirstLine`. */
function namedFirstLine(words: string, nameOf: (reference: string) => string | undefined): string {
  const references = referenceStandIns(nameOf);
  return references.named(drawnLine(firstLineOf(references.standIn(words))));
}

/**
 * The nodes the run changed (`RunChange`), each by its first line, beside its kind's word where
 * the page has one, as a link to the page it opens where it opens one. The line is cut by R1's
 * first-line rule and each reference in it named as every one-line place of the page names one
 * (`namedFirstLine`): an issue by its key, any other target by the page's name for it, and by its
 * short form where the page knows none, never as the address its author typed. A node whose words
 * hold none is named by its kind's word alone, which is then its link, or left out if it has neither.
 */
function ChangedNodes({ changed }: { changed: readonly RunChange[] }): ReactNode {
  const words = useMemo(() => changed.map((node) => node.words), [changed]);
  return (
    <RunReferenceNames
      draw={(nameOf) => <ChangedNodeList changed={changed} nameOf={nameOf} />}
      texts={words}
    />
  );
}

function ChangedNodeList({
  changed,
  nameOf,
}: {
  changed: readonly RunChange[];
  nameOf: (reference: string) => string | undefined;
}): ReactNode {
  const entries = useMemo(
    () =>
      changed.flatMap((node) => {
        // The node's first line: the reading list's `run-page-changed-line`.
        const first = namedFirstLine(node.words, nameOf);
        const line = first || node.kind;
        return line === "" ? [] : [{ named: first !== "", node, line }];
      }),
    [changed, nameOf]
  );
  if (entries.length === 0) {
    return (
      <p className={`text-sm ${pageQuietText}`} {...pageChrome}>
        {CONTROL_LABELS.none}
      </p>
    );
  }
  return (
    <ul className="flex min-w-0 flex-col gap-0.5">
      {entries.map(({ named, node, line }) => {
        return (
          <li className="flex min-w-0 items-baseline gap-2 text-sm" key={node.key}>
            {named && node.kind !== "" ? (
              <span className={`shrink-0 text-xs ${pageQuietText}`} {...pageChrome}>
                {node.kind}
              </span>
            ) : null}
            {node.href === undefined ? (
              <span className="min-w-0 truncate" {...(named ? authorText : pageChrome)}>
                {line}
              </span>
            ) : (
              <Link
                className={`min-w-0 truncate ${LINK}`}
                to={node.href}
                {...(named ? authorText : pageChrome)}
              >
                {line}
              </Link>
            )}
          </li>
        );
      })}
    </ul>
  );
}

/** One part of the answer: the model's text through the one markdown renderer, a thing it cites
 *  named by its title, as a link to the page it opens where it opens one, or a line the page says
 *  of the answer. */
function RunAnswerPart({ part }: { part: RunPart }): ReactNode {
  switch (part.kind) {
    case "answer":
      return (
        <div {...authorText}>
          <MarkdownBody markdown={part.markdown} softBreaks="line" surface="run-page-answer" />
        </div>
      );
    case "item":
      return (
        <p className="text-sm">
          {part.href === undefined ? (
            <span {...authorText}>{part.title}</span>
          ) : (
            <Link className={LINK} to={part.href} {...authorText}>
              {part.title}
            </Link>
          )}
        </p>
      );
    case "line":
      return (
        <p className={`text-sm ${pageQuietText}`} {...pageChrome}>
          {part.words}
        </p>
      );
  }
}
