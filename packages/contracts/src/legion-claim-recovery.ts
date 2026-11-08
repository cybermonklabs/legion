import { z } from "zod";
import { LEGION_ROLES } from "./legion-roles";

/**
 * The two claim recoveries the controller asks for, `fresh_session` and `compact`: the body they
 * share, and the answer each gives through the controller's door and the operator's alike.
 *
 * Go owns all three shapes, `packages/daemon/internal/api/freshsession.go` (`ClaimRequest`,
 * `FreshSessionResponse`) and `compact.go` (`CompactResponse`), and the fixtures pin them as they
 * pin the rest of the API (`legion-api.test.ts`). Importers read them through
 * `@legion/contracts/legion-api`, which re-exports all three; this module has no export entry of
 * its own (`package.json`).
 */

const nonEmptyString = z.string().min(1);

/** `api.ClaimRequest`, what either of the controller's two claim recoveries names: the claim on
 * `role` of `issue`, under the caller's controller grant. The project's controller is no role
 * here, so no body can name its claim (`INVALID_ROLE`); `NOT_THE_CONTROLLER`, the daemon's
 * refusal of the controller's own claim, belongs to the operator's door, and this one never
 * gives it.
 *
 * `fresh_session` has that claim leave the session it recorded behind and start again in a new
 * one, the effect `legion claims retry --fresh-session` runs for an operator. It reaches every
 * claim of the project that is in no turn, has no launch and no parked relaunch in flight, has no
 * suspension held over it, and is not left working over a process the daemon's last report of it
 * found gone, a root architect included: one idle on the provider-error ladder, one whose agent
 * has registered and is not ready yet, and one that is suspended, stopped or failed.
 *
 * `compact` shortens the session instead, the effect `legion claims compact` runs for one: the
 * claim keeps its session, its workspace and everything it knows of its issue, and whatever task
 * it owed goes to it again. It reaches a narrower set, a claim whose agent is ready or idle and
 * no suspension is held over, since there has to be a running Oh My Pi session to ask.
 *
 * The daemon refuses each with the code and the sentence the controller reads. Each sentence
 * says what the claim is doing when the refusal is given, which operation reaches each state a
 * caller can read of it in `legion claims list`, and nothing of what the next turn, hold,
 * report or relaunch will make of it. Both give `CLAIM_IN_A_TURN` for a claim the daemon
 * records as working, since cutting a running turn off would lose the turn that may be the one
 * ending the run of provider errors; the claim is not always in a turn, and the sentence says
 * what the daemon's record holds of it — that it is stored working, that a suspension is held
 * over it, or that the process it was left over was found gone by the last report and its
 * relaunch has not run. Both give `CLAIM_UNSETTLED` for a claim with no settled
 * agent to act on: one queued, one with a launch in flight, one a suspension is held over in
 * any state but working, and the one working claim that takes this code rather than the turn's,
 * left working under a relaunch the daemon is pacing. The compaction adds
 * three refusals of its own:
 * `CLAIM_NOT_RUNNING` for a suspended, stopped or failed claim, which runs no session at all and
 * which `fresh_session` reaches; `CLAIM_NOT_ON_OMP` for a role running on Claude Code, which has
 * no Oh My Pi session; and `COMPACTION_FAILED` for an ask that did not happen, an end the daemon
 * could not write, a claim that lost its process while its session was being compacted, and an
 * end a suspension held over the claim took, which suspends the claim instead. None of those
 * four gave the claim back, so none sent the task it owes. A registered claim no suspension is
 * held over is the compaction's `CLAIM_UNSETTLED` and no refusal at all for `fresh_session`. */
export const LegionClaimRequest = z.strictObject({
  grantId: nonEmptyString,
  issue: nonEmptyString,
  role: z.enum(LEGION_ROLES),
});

/** `api.FreshSessionResponse`, what became of a fresh session. It is empty wherever the claim
 * came back and its start ran, which is every answer but one: `phaseNotRestarted` is why nothing
 * restarted the claim's phase, present only where the claim came back in a new session owing no
 * task and the workflow refused the restart that would have handed it one. The claim started
 * either way, which is why that refusal rides a success; a reader of the claim cannot tell it,
 * since the state records nowhere that the restart did not run. */
export const LegionFreshSessionResponse = z.object({
  phaseNotRestarted: nonEmptyString.optional(),
});

/** `api.CompactResponse`, what became of a compaction. `compacted` is whether Oh My Pi
 * compacted the session, and `reason` why it did not: Oh My Pi's own words for a compaction it
 * refused or skipped ("Already compacted", "Nothing to compact (session too small)"), and the
 * daemon's for a wait that ran out. The daemon cuts it to 200 bytes, since the words are a
 * model's and reach the controller's next prompt. A compaction that did not run is this answer
 * rather than a failure, because the claim came back and whatever task it owed has gone to it
 * again. */
export const LegionCompactResponse = z.object({
  compacted: z.boolean(),
  reason: z.string().optional(),
});
