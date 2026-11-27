/**
 * The names of Details' controls that a test or another control finds them by, in a module that
 * imports only the page's words: the browser tests import these, and `IssueMargin.tsx`, which
 * draws Referenced by's count, reaches the document schema's JSON (through `ReferencedBy`),
 * which Playwright's Node does not load.
 */

import { PROPERTY_LABELS } from "./page-structure";
import { CONTROL_LABELS } from "./page-words";

/**
 * The name the blocked-by control answers to: the word it shows and what it opens, in the words
 * the box it opens is named by ("Issues CORE-1 waits on"), so a voice user saying what they read
 * presses it (WCAG 2.5.3) and every reader hears one sentence: "Add issues CORE-1 waits on". The
 * add menu reaches the control by this name rather than drawing a second copy of it, and calls
 * this function for it (`IssueAddMenu.tsx`), so the two sides cannot drift apart: a word changed
 * here changes both.
 */
export function blockedByControlName(issueKey: string, blockerCount: number): string {
  const verb = blockerCount === 0 ? CONTROL_LABELS.add : CONTROL_LABELS.edit;
  return `${verb} issues ${issueKey} waits on`;
}

/** The text and name of Referenced by's control: its label, and how many rows the list it opens
 *  shows - its own (`listedEdges`) and the reader's command bar runs (`useRunRows`) - once both
 *  reads have answered; undefined before then, when the control reads its label alone. Details
 *  draws the control at every count, zero included: a run is in no reader's
 *  `referenced_by_count`. */
export function referencedByControlName(count: number | undefined): string {
  return count === undefined
    ? PROPERTY_LABELS.referenced_by
    : `${PROPERTY_LABELS.referenced_by} (${count})`;
}
