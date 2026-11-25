import type { LegionClaimState } from "@legion/contracts/legion-api";

import type { LegionRoleClaim } from "../../api/legion-issue";
import { ROLE_STATE_WORDS } from "../issue-page/page-words";

/**
 * The agents at work (dispatch://INFRA-5702/spec#b-r-agents): each role of the issue's Legion
 * state that holds a claim, with its claim state in INFRA-1336's words for the state. Pure: it
 * reads the roles `legionIssueQuery` holds and draws nothing.
 */

/** The word of each claim state the daemon names (`LEGION_CLAIM_STATES`), and `null` for a
 *  `retired` claim, which is not listed. A state the daemon adds fails the type check here until
 *  it has its word. */
const CLAIM_STATE_WORDS: Readonly<Record<LegionClaimState, keyof typeof ROLE_STATE_WORDS | null>> =
  {
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

function isClaimState(state: string): state is LegionClaimState {
  return Object.hasOwn(CLAIM_STATE_WORDS, state);
}

/** One line of the agents at work: the role, then its role words. */
export interface AgentLine {
  readonly role: string;
  readonly words: string;
}

/** The role words of the claim state `state`, "in another state" for a state the daemon's list
 *  does not hold, and undefined for a `retired` claim, which is not listed. */
export function roleStateWord(state: string): string | undefined {
  if (!isClaimState(state)) return ROLE_STATE_WORDS.another;
  const word = CLAIM_STATE_WORDS[state];
  return word === null ? undefined : ROLE_STATE_WORDS[word];
}

/** Each role of `roles` that holds a claim, in the order the state gives them, with its role
 *  words: a `retired` claim is left out. */
export function agentsAtWork(
  roles: readonly LegionRoleClaim[] | null | undefined
): readonly AgentLine[] {
  return (roles ?? []).flatMap(({ role, state }) => {
    const words = roleStateWord(state);
    return words === undefined ? [] : [{ role, words }];
  });
}
