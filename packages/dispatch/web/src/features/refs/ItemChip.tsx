import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";

import {
  borderDefault,
  surfaceMutedBg,
  textPrimaryOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { statusTone } from "../issue-page/issue-status";
import { statusText } from "../project/board-model";
import { issueSummaryQuery } from "./reference-target";

/**
 * An issue that the command bar names, drawn as a chip (design v3, state 4): its key, a dot in
 * its status's colour and its title, on one line. Every place of the bar draws an issue with this
 * one chip: an answer, a short answer, a plan's row and a table's cell. The status and the title
 * come from the issue's summary (`issueSummaryQuery`, `GET /api/v1/issues/{key}/summary`), the one
 * read that every reference to the issue on the page shares. Until that read answers, and for an
 * issue the reader cannot read, the chip is the key alone. The status's word is the dot's
 * tooltip, and a screen reader hears it in place of the dot.
 *
 * The chip is one box that never breaks inside itself (an `inline-flex` box): where the rest of a
 * line has no room for it, it goes to the next line whole, and a title longer than the whole line
 * is cut with "…", the full title showing on hover. Being its own box, it takes no underline from
 * the link it sits in, so it reads as one thing to click, not as a key and a title that are two
 * links. The caller keeps the punctuation after it on its line (`MarkdownBody`). A chip the
 * caller gives a `hover` shows that on hover after the whole title ("<title> · <hover>"), and
 * alone until the summary answers: a chip drawn for the words of a time link shows the time
 * (`MarkdownBody`, `TimedWords`), and keeps the title the chip may cut whole beside it.
 */
export function ItemChip({
  hover,
  issue,
}: {
  /** What the chip shows on hover after the issue's title. */
  readonly hover?: string;
  readonly issue: string;
}): ReactNode {
  const summary = useQuery(issueSummaryQuery(issue)).data;
  return (
    <span
      className={`not-prose inline-flex max-w-full items-center gap-1.5 rounded-md border px-1.5 align-middle text-sm leading-5 font-normal whitespace-nowrap no-underline ${borderDefault} ${surfaceMutedBg}`}
      data-testid="item-chip"
      title={
        hover === undefined || summary === undefined
          ? (hover ?? summary?.title)
          : `${summary.title} · ${hover}`
      }
    >
      <span className={`shrink-0 font-semibold ${textPrimaryOnSurface}`}>{issue}</span>
      {summary === undefined ? null : (
        <>
          <span
            aria-hidden="true"
            className={`size-2 shrink-0 rounded-full bg-current ${statusTone(summary.status)}`}
            data-status={summary.status}
            data-testid="item-chip-status"
            title={statusText(summary.status)}
          />
          <span className="sr-only">{statusText(summary.status)}</span>
          <span
            className={`min-w-0 truncate ${textSecondaryOnSurface}`}
            data-testid="item-chip-title"
          >
            {summary.title}
          </span>
        </>
      )}
    </span>
  );
}
