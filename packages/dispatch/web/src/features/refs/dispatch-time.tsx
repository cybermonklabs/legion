import { Fragment, type ReactNode } from "react";

import { timeTitle } from "./Timestamp";

/**
 * A time the command bar's agent writes in its words: a Markdown link whose words are the time as
 * the page shows it and whose address is the instant, under the `dispatch-time:` scheme -
 * `[5 days ago](dispatch-time:2026-11-18T11:35:00Z)`. The page draws it as a `<time>` of that
 * instant showing the link's words - the words the agent wrote, never words made from the
 * instant - with the instant whole on hover, as every other time the page draws in words has it
 * (`Timestamp`). An address that names no instant draws the words alone.
 */

/** The scheme of a time's address. */
const DISPATCH_TIME = "dispatch-time:";

/** The attribute a Markdown body's drawing of a time link carries (`markdown-serializer.ts`), by
 *  which the body finds the time links it drew: one whose words are an issue's key draws as that
 *  issue's chip in an answer of the command bar (`MarkdownBody`). */
export const TIME_LINK_ATTRIBUTE = "data-dispatch-time";

/** An instant as RFC 3339 writes it, which is how the agent is told to write one: a date, a time
 *  to the minute or finer, and its zone. */
const INSTANT = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}(?::\d{2}(?:\.\d+)?)?(?:Z|[+-]\d{2}:\d{2})$/u;

/** A time link written in plain text: its words, then its address after the scheme. A link
 *  whose words hold a bracket is no time the agent writes, and stays as written. */
const TIME_LINK = /\[([^[\]\n]*)\]\(\s*dispatch-time:([^)\s]*)\s*\)/giu;

/** The instant `written` is, as it is written; undefined where it is none. */
function writtenInstant(written: string): string | undefined {
  return INSTANT.test(written) && !Number.isNaN(Date.parse(written)) ? written : undefined;
}

/** Whether `href` is a time's address, whether or not it names an instant. */
export function isTimeAddress(href: unknown): href is string {
  return typeof href === "string" && href.trim().toLowerCase().startsWith(DISPATCH_TIME);
}

/** The instant a time's address names, as it writes it; undefined for an address that names
 *  none, such as `dispatch-time:last week`. */
export function instantOf(href: string): string | undefined {
  return writtenInstant(href.trim().slice(DISPATCH_TIME.length));
}

/** `markdown` with each time link written as its words alone, as the page shows the time: what
 *  Copy puts on the clipboard, which a reader pastes as the words they saw. */
export function timeWords(markdown: string): string {
  return markdown.replace(TIME_LINK, (_link, words: string) => words);
}

/** A time the agent wrote: a `<time>` of `instant` showing `words`, the instant whole on hover;
 *  the words alone where its address named no instant. */
function AgentTime({
  instant,
  words,
}: {
  readonly instant: string | undefined;
  readonly words: string;
}): ReactNode {
  if (instant === undefined) return words;
  return (
    <time dateTime={instant} title={timeTitle(new Date(instant))}>
      {words}
    </time>
  );
}

/** Words the agent wrote that the page draws as plain text - a plan's summary, the line it
 *  writes beside its calls - with each time link in them drawn as its time. */
export function TimedWords({ text }: { readonly text: string }): ReactNode {
  const parts: ReactNode[] = [];
  let at = 0;
  for (const link of text.matchAll(TIME_LINK)) {
    parts.push(
      <Fragment key={link.index}>
        {text.slice(at, link.index)}
        <AgentTime instant={writtenInstant(link[2] ?? "")} words={link[1] ?? ""} />
      </Fragment>
    );
    at = link.index + link[0].length;
  }
  if (parts.length === 0) return text;
  parts.push(text.slice(at));
  return parts;
}
