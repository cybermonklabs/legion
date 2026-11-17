import type { ReactNode } from "react";

import { type PageWords, PageWordsProvider } from "../../components/tooltips";
import { authorText, PROPERTY_LABELS, pageChrome } from "./page-structure";
import { ANSWER_LINES, AUTHOR_KIND_WORDS, CONTROL_LABELS, STATE_LINES } from "./page-words";

/** The words and marks the shared controls draw inside `NoTooltips`, from the page's one list.
 *  `PageWords` names the keys the controls read, so a key this list lacks fails here. */
const WORDS: PageWords = {
  ANSWER_LINES,
  AUTHOR_KIND_WORDS,
  CONTROL_LABELS,
  PROPERTY_LABELS,
  STATE_LINES,
  authorText,
  pageChrome,
};

/**
 * The issue page's surface: every shared control inside draws no `title`, draws the fact a
 * `title` held as visible text or as its accessible name, and says the page's words with their
 * marks (`components/tooltips.tsx`). A part that fills a slot of the page wraps what it draws
 * in this.
 *
 * It imports the page's word list, so only the lazily loaded page imports it: no file that
 * today's pages load names this module.
 */
export function NoTooltips({ children }: { children: ReactNode }): ReactNode {
  return <PageWordsProvider words={WORDS}>{children}</PageWordsProvider>;
}
