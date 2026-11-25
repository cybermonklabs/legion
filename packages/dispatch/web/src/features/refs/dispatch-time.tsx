import { Fragment, type ReactNode } from "react";

import { ItemChip } from "./ItemChip";
import { buildReferencePath } from "./routes";
import { timeTitle } from "./Timestamp";
import { instantOf, timeLinkIssue } from "./time-link";

/**
 * A time the command bar's agent writes in its words: a Markdown link whose words are the time as
 * the page shows it and whose address is the instant, under the `dispatch-time:` scheme -
 * `[5 days ago](dispatch-time:2026-11-18T11:35:00Z)`. The page draws it as a `<time>` of that
 * instant showing the link's words - the words the agent wrote, never words made from the
 * instant - with the instant whole on hover, as every other time the page draws in words has it
 * (`Timestamp`). An address that names no instant draws the words alone. Words that are an
 * issue's key draw as that issue's chip, the time on its hover. The address and the words are
 * read in `time-link.ts` (`instantOf`, `timeLinkIssue`).
 */

/** A time link written in plain text: its words, then its address. A link whose words hold a
 *  bracket is no time the agent writes, and stays as written. */
const TIME_LINK = /\[([^[\]\n]*)\]\(\s*(dispatch-time:[^)\s]*)\s*\)/giu;

/** `markdown` with each time link written as its words alone, as the page shows the time: what
 *  Copy puts on the clipboard, which a reader pastes as the words they saw. */
export function timeWords(markdown: string): string {
  return markdown.replace(TIME_LINK, (_link, words: string) => words);
}

/** A time the agent wrote: a `<time>` of `instant` showing `words`, the instant whole on hover;
 *  the words alone where its address named no instant. Words that are an issue's key
 *  (`timeLinkIssue`) show as that issue's chip, a link to it, with the time on its hover. */
function AgentTime({
  instant,
  words,
}: {
  readonly instant: string | undefined;
  readonly words: string;
}): ReactNode {
  const hover = instant === undefined ? undefined : timeTitle(new Date(instant));
  const issue = timeLinkIssue(words);
  const shown =
    issue === undefined ? (
      words
    ) : (
      <a className="inline-flex max-w-full" href={buildReferencePath(issue)}>
        <ItemChip hover={hover} issue={issue.key} />
      </a>
    );
  if (instant === undefined) return shown;
  return (
    <time dateTime={instant} title={hover}>
      {shown}
    </time>
  );
}

/** Words the agent wrote that the page draws as plain text - a plan's summary, the line it
 *  writes beside its calls - with each time link in them drawn as its time, or as its issue's
 *  chip where its words are that issue's key. */
export function TimedWords({ text }: { readonly text: string }): ReactNode {
  const parts: ReactNode[] = [];
  let at = 0;
  for (const link of text.matchAll(TIME_LINK)) {
    parts.push(
      <Fragment key={link.index}>
        {text.slice(at, link.index)}
        <AgentTime instant={instantOf(link[2] ?? "")} words={link[1] ?? ""} />
      </Fragment>
    );
    at = link.index + link[0].length;
  }
  if (parts.length === 0) return text;
  parts.push(text.slice(at));
  return parts;
}
