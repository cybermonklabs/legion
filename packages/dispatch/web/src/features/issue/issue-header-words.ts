import type { IssueStatus } from "@legion/contracts/dispatch-tools";

import type { IssuePriority } from "../../api/types";
import { issueStatuses, openIssueStatuses } from "../project/board-model";

/**
 * The words the issue header's state row draws, and the rule for which statuses its pick offers,
 * in one place, because two surfaces draw that row: the header itself (`IssueHeader.tsx` and the
 * badge controls it holds), and the row the page keeps for it until the header lands
 * (`IssuePage.tsx`'s `WhatTheHeaderWouldSay`, dispatch://INFRA-341).
 *
 * Each control in the row is as wide as its words, and the row wraps at a phone width, so a word
 * that differs between the two is a row more or fewer in the kept space, and the document under
 * the header moves by that row as the header lands. The kept row copied these words by hand; both
 * read them here now. `issue-header-classes.ts` is the same for the row's boxes.
 *
 * Words and rules only: the controls that draw them stay in their own chunks.
 */

/** The statuses the header's status pick offers: every one on a closed issue, and every one but
 *  `done` on an open issue, which Close sets instead. A `<select>` is as wide as its widest
 *  option, so the kept row's pick offers the same list. */
export function selectableStatuses(isClosed: boolean): readonly IssueStatus[] {
  return isClosed ? issueStatuses : openIssueStatuses;
}

/** The priority badge's word: the level, or a muted `Priority` tag when it is unset. */
export function priorityBadgeWords(priority: IssuePriority | null): string {
  return priority === null ? "Priority" : `P${priority}`;
}

/** What the assignee badge says of an issue with no assignee, and the picker's first option. */
export const unassignedWords = "Unassigned";

/** The assignee badge's word: who answers the issue's asks, or `unassignedWords`. */
export function assigneeBadgeWords(assignee: string | null): string {
  return assignee ?? unassignedWords;
}

/** The Release button's word, on an issue an agent has claimed. */
export const releaseClaimWords = "Release";

/** Whose turn the header's Attention group says it is, from the issue's open asks: "Waiting on
 *  you" while any of them waits on a person, else "Waiting on agents", each with its count; null
 *  with none open, or with one whose turn the read does not say, so the line is left out rather
 *  than drawn wrong. The space the page keeps for the header draws the same line (`IssuePage.tsx`),
 *  since on a phone it is a line of the header of its own. */
export function whoseTurnWords(
  openAsks: number,
  waitingOnPeople: number,
  knowsWhoseTurn: boolean
): string | null {
  if (openAsks === 0 || !knowsWhoseTurn) return null;
  return waitingOnPeople > 0
    ? `Waiting on you (${waitingOnPeople})`
    : `Waiting on agents (${openAsks - waitingOnPeople})`;
}

/** Close's word at each width: the short one below `sm` and the full one from `sm` up, each in
 *  the span that shows it only there. The button is as wide as the word it shows. */
export const closeIssueWords: readonly { className: string; words: string }[] = [
  { className: "sm:hidden", words: "Close" },
  { className: "hidden sm:inline", words: "Close issue" },
];
