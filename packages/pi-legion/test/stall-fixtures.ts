import type { ClaimCapability } from "@legion/pi-shared/legion-phase-ops";
import type { StallCall, StallEvidence } from "@legion/pi-shared/stall-watchdog";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";

/** The stuck call, its evidence and its session's claim the stall watch's tests share: the
 * evidence's own (`src/stall-evidence.test.ts`) and the teller's (`src/stall-teller.test.ts`). */

export const SECOND = 1_000;
export const MINUTE = 60 * SECOND;
export const START = 1_793_000_000_000;

/** One overdue call, with whatever the case needs changed. */
export function callOf(over: Partial<StallCall> = {}): StallCall {
  return {
    toolCallId: "toolu_stuck",
    toolName: "bash",
    input: { command: "sleep 100000", pty: true },
    startedAt: START,
    deadlineMs: undefined,
    step: 0,
    steppedAt: undefined,
    told: undefined,
    ...over,
  };
}

export function evidenceOf(over: Partial<StallEvidence> = {}): StallEvidence {
  return {
    call: callOf(),
    overdue: {
      rule: { kind: "bound", boundMs: 60 * MINUTE },
      openMs: 65 * MINUTE,
      pastMs: 5 * MINUTE,
    },
    tree: { read: true, processes: [] },
    ...over,
  };
}

/** The claim's registration secret: random, as the daemon's is, since the pane holds it. */
const CLAIM_SECRET = fixtureSecret("claim");

export function claimOf(over: Partial<ClaimCapability> = {}): ClaimCapability {
  return {
    sessionID: "ses_worker",
    claimToken: "claim-token",
    generation: 3,
    tree: "INFRA-1000",
    issue: "INFRA-1274",
    role: "implementer",
    secret: CLAIM_SECRET,
    ...over,
  };
}
