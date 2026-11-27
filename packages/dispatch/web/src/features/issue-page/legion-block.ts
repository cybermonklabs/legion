import type { LegionClaimState, LegionPhase } from "@legion/contracts/legion-api";

import { CONTROL_LABELS, PHASE_WORDS, ROLE_STATE_WORDS } from "./page-words";

/**
 * The Legion block of Details (dispatch://INFRA-4870/spec#b-r-block), in the words of the
 * parent's "Words for the state" (dispatch://INFRA-1336/spec): the word of a phase, the word of
 * a claim state, and the ending of a place in the line. The phase words and the claim-state words
 * are `page-words-dashboard.ts`'s, which this module declares none of. Pure: no React and no DOM.
 */

/** The word of each phase the daemon names (`LEGION_PHASES`) and of `awaiting_merge`, the old
 *  value daemons before contract 86 send, which reads "Merging"; `null` for `done` and
 *  `unrecorded`, which draw no block. A phase the daemon adds fails the type check here until
 *  it has its word. */
const PHASE_WORD: Readonly<Record<LegionPhase | "awaiting_merge", string | null>> = {
  admitted: PHASE_WORDS.steps.admitted,
  awaiting_person: PHASE_WORDS.awaiting_person,
  awaiting_children: PHASE_WORDS.awaiting_children,
  planning: PHASE_WORDS.steps.planning,
  implementing: PHASE_WORDS.steps.implementing,
  testing: PHASE_WORDS.steps.testing,
  reviewing: PHASE_WORDS.steps.reviewing,
  accepting: PHASE_WORDS.steps.accepting,
  retro: PHASE_WORDS.steps.retro,
  merging: PHASE_WORDS.steps.merging,
  integrating: PHASE_WORDS.integrating,
  done: null,
  held: PHASE_WORDS.held,
  unrecorded: null,
  awaiting_merge: PHASE_WORDS.steps.merging,
};

/** The word of each claim state the daemon names (`LEGION_CLAIM_STATES`), and `null` for a
 *  `retired` claim, which is not listed. A state the daemon adds fails the type check here until
 *  it has its word. */
const CLAIM_STATE_WORD: Readonly<Record<LegionClaimState, keyof typeof ROLE_STATE_WORDS | null>> = {
  queued: "starting",
  launch_uncertain: "starting",
  launching: "starting",
  shim_connected: "starting",
  registered: "idle",
  ready: "idle",
  working: "in_turn",
  idle: "idle",
  suspended: "paused",
  failed: "failed",
  retired: null,
};

/** The word of the phase `phase`, "In another phase" for a phase the daemon's list does not
 *  hold, and undefined for `done` and `unrecorded`, which draw no block. */
export function phaseWord(phase: string): string | undefined {
  if (!Object.hasOwn(PHASE_WORD, phase)) return PHASE_WORDS.another;
  return PHASE_WORD[phase as keyof typeof PHASE_WORD] ?? undefined;
}

/** The word of the claim state `state`, "in another state" for a state the daemon's list does
 *  not hold, and undefined for a `retired` claim, which is not listed. */
export function claimStateWord(state: string): string | undefined {
  if (!Object.hasOwn(CLAIM_STATE_WORD, state)) return ROLE_STATE_WORDS.another;
  const word = CLAIM_STATE_WORD[state as LegionClaimState];
  return word === null ? undefined : ROLE_STATE_WORDS[word];
}

/** The ending English gives the place `place` ("st" of 1st, 21st and 101st, "th" of 11th to
 *  13th and 111th), which the block draws after the number: "3rd of 5 in line". */
export function ordinalEnding(place: number): string {
  const endings = CONTROL_LABELS.legion.ordinal_endings;
  const lastTwo = place % 100;
  if (lastTwo >= 11 && lastTwo <= 13) return endings.other;
  switch (place % 10) {
    case 1:
      return endings.one;
    case 2:
      return endings.two;
    case 3:
      return endings.three;
    default:
      return endings.other;
  }
}
