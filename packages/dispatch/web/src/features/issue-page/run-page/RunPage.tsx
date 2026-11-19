import {
  type ReactNode,
  type RefObject,
  useId,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import { Link, useParams } from "react-router-dom";

import type { GraphReferences } from "../../../api/types";
import { pageInkText, pageQuietText } from "../../../theme/classes";
import { MarkdownBody } from "../../refs/MarkdownBody";
import { Timestamp } from "../../refs/Timestamp";
import { NotFoundPage } from "../../shell/NotFoundPage";
import { useDocumentTitle } from "../../shell/useDocumentTitle";
import { DetailsErrorLine } from "../details-error";
import { NoTooltips } from "../NoTooltips";
import { authorText, pageChrome } from "../page-structure";
import {
  ANSWER_LINES,
  CONTROL_LABELS,
  NODE_REFERENCES_WORDS,
  RUN_PAGE_WORDS,
  sentenceCase,
} from "../page-words";
import { ROW_LINK } from "../row-references";
import { type RunPart, runView, useRun } from "./run-page";

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
  // A retry of a read that holds no answer takes its failure away as it starts, and the focus
  // on its Retry would fall to the page's body with it, where no keyboard action may leave it.
  // So the line of a Retry the reader pressed stays drawn, its Retry disabled, until the read that
  // Retry sent ends. An answer then replaces the line and the focus goes to the page's heading, as
  // the history's Retry hands it to its own (`IssueHistory.tsx`); a read that fails again leaves
  // the line, and its Retry has the focus; a read the server answers 400 or 404 draws the
  // not-found page, and the focus goes to its link. Only a focus left on the body moves. Once a
  // Retry was pressed, the not-found page is drawn inside a wrapper the effect finds its link
  // through, and the wrapper stays, so the page is not drawn anew when `pressed` clears; a first
  // read's 404 draws the page bare.
  const [pressed, setPressed] = useState(false);
  const [retried, setRetried] = useState(false);
  const retrying = pressed && query.isFetching;
  const heading = useRef<HTMLHeadingElement>(null);
  const failure = useRef<HTMLDivElement>(null);
  const notFoundPage = useRef<HTMLDivElement>(null);
  useLayoutEffect(() => {
    if (!pressed || query.isFetching) return;
    setPressed(false);
    const active = document.activeElement;
    if (active !== null && active !== document.body) return;
    (
      heading.current ??
      failure.current?.querySelector("button") ??
      notFoundPage.current?.querySelector<HTMLElement>("a[href]")
    )?.focus({ preventScroll: true });
  });
  if (notFound) {
    return retried ? (
      <div className="contents" ref={notFoundPage}>
        <NotFoundPage />
      </div>
    ) : (
      <NotFoundPage />
    );
  }
  const failed = query.isError || retrying;
  return (
    <div data-issue-page="">
      <NoTooltips>
        {failed ? (
          <div ref={failure}>
            <DetailsErrorLine
              error={query.error}
              message={ANSWER_LINES.run_unreadable}
              onRetry={() => {
                setPressed(true);
                setRetried(true);
                void query.refetch();
              }}
              retrying={retrying}
            />
          </div>
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
          {RUN_PAGE_WORDS.question}
        </h2>
        {/* The question as the asker typed it: their characters, never read as markdown. */}
        <p className="whitespace-pre-wrap [overflow-wrap:anywhere]" {...authorText}>
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
        {view.changed.length === 0 ? (
          <p className={`text-sm ${pageQuietText}`} {...pageChrome}>
            {CONTROL_LABELS.none}
          </p>
        ) : (
          <ul className="flex min-w-0 flex-col gap-0.5">
            {view.changed.map((node) => {
              // A node is named by its first line, beside its kind's word where the page has
              // one; a node whose read carries no words is named by the word alone, which is
              // then its link. A node with neither is no entry (`runView`).
              const named = node.line !== "";
              const text = named ? node.line : node.kind;
              return (
                <li className="flex min-w-0 items-baseline gap-2 text-sm" key={node.key}>
                  {named && node.kind !== "" ? (
                    <span className={`shrink-0 text-xs ${pageQuietText}`} {...pageChrome}>
                      {node.kind}
                    </span>
                  ) : null}
                  {node.href === undefined ? (
                    <span className="min-w-0 truncate" {...(named ? authorText : pageChrome)}>
                      {text}
                    </span>
                  ) : (
                    <Link
                      className={`min-w-0 truncate ${LINK}`}
                      to={node.href}
                      {...(named ? authorText : pageChrome)}
                    >
                      {text}
                    </Link>
                  )}
                </li>
              );
            })}
          </ul>
        )}
      </section>
    </article>
  );
}

/** One part of the answer: the model's text through the one markdown renderer, a thing it cites
 *  as a link named by its title, or a line the page says of the answer. */
function RunAnswerPart({ part }: { part: RunPart }): ReactNode {
  switch (part.kind) {
    case "answer":
      return (
        <div {...authorText}>
          <MarkdownBody markdown={part.markdown} softBreaks="line" />
        </div>
      );
    case "item":
      return (
        <p className="text-sm">
          {part.href.startsWith("/") ? (
            <Link className={LINK} to={part.href} {...authorText}>
              {part.title}
            </Link>
          ) : (
            <a className={LINK} href={part.href} {...authorText}>
              {part.title}
            </a>
          )}
        </p>
      );
    case "line":
      return (
        <p className={`text-sm ${pageQuietText}`} {...pageChrome}>
          {part.text}
        </p>
      );
  }
}
