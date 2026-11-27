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
 * presses it (WCAG 2.5.3) and every reader hears one sentence: "Add issues CORE-1 waits on".
 * The page reaches the control by its mark instead (`propertyControl`): the add menu
 * (`IssueAddMenu.tsx`), and Details' hand-off of the focus from Blocked by's Retry
 * (`IssueMargin.tsx`).
 */
export function blockedByControlName(issueKey: string, blockerCount: number): string {
  const verb = blockerCount === 0 ? CONTROL_LABELS.add : CONTROL_LABELS.edit;
  return `${verb} issues ${issueKey} waits on`;
}

/** The text and name of Referenced by's count: its label and how many reference the issue. */
export function referencedByControlName(count: number): string {
  return `${PROPERTY_LABELS.referenced_by} (${count})`;
}
