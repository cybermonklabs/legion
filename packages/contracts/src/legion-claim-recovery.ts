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

/** `api.ClaimRequest`, what either of the two claim recoveries names: the claim on `role` of
 * `issue`, under the caller's grant, the project controller's or an architect's for the claims of
 * the issues it owns (below). The controller's four claim operations
 * (`POST /legion/v1/claims/stop|retry|suspend|resume`, contract 212) take the same body and the
 * same door, and answer `LegionClaimOperationResponse` (`legion-api.ts`). The project's controller
 * is no role here, so no body can name its claim (`INVALID_ROLE`); `NOT_THE_CONTROLLER`, the
 * daemon's refusal of the controller's own claim, belongs to the operator's door, and this one
 * never gives it. Nor can a body name another project's claim: the daemon builds the token from
 * its own project, so such a claim is one it does not supervise (`NO_CLAIM`).
 *
 * The controller's door gives `SUSPENDED_BY_OPERATOR` (409) to every request that would get past
 * a suspension the operator holds, held for the claim's turn or landed, before it reads anything
 * else of the claim: a resume of it, a suspension of a claim it holds, and a stop, a retry or a
 * `fresh_session` of one. Only the operator lifts the operator's suspension. Nothing of the claim
 * changes but where `fresh_session` went further before that suspension took the claim over: its
 * own suspension of the claim ran, which that suspension then took over, and the claim keeps the
 * session it recorded; or it left behind the session the claim recorded, so the claim starts in a
 * new session once it is started. `compact` alone is let past the operator's suspension, since a
 * compaction lifts nothing: a claim that suspension holds takes the compaction's own refusal
 * below, whose sentence names the operator's `legion claims resume` as what starts the claim.
 *
 * The same six operations take an architect's grant for the claims of the issues it owns alone
 * (contract 212): an issue the daemon does not record is `ISSUE_NOT_FOUND` (404), one of another
 * tree `ISSUE_OUTSIDE_TREE` (403), one of its own tree it does not own (for a sub-architect, its
 * tree's root and a sibling's issues) `ISSUE_NOT_OWNED` (403), and the architect's own claim
 * `OWN_CLAIM` (403). Each is refused `SUSPENDED_BY_OPERATOR` (409) for a claim the operator's
 * suspension holds and `SUSPENDED_BY_CONTROLLER` (409) for one the controller's holds, held or
 * landed, and the architect's resume, suspend, stop and `fresh_session` are refused
 * `SUSPENDED_BY_DAEMON` (409) for a claim the daemon suspended: an architect's resume lifts an
 * architect's suspension alone, and its suspend and stop reach a released claim and no other
 * suspension of the daemon's. The architect's retry and `fresh_session` of a claim that failed or
 * that a stop retired are refused `ISSUE_OUT_OF_WORKFLOW` (409) while the claim's issue is out of
 * the workflow or its tree lingers, since the daemon's own rules start that work again. Its
 * suspension is recorded as `architect`, which the controller's `resume_claim` lifts too.
 *
 * While the project is paused, every operation of the door that would start a turn of a claim —
 * `resume_claim`, `retry` and `fresh_session`, which start its session again, and `compact`,
 * whose end sends its task again — is refused `PROJECT_PAUSED` (409, contract 212) with the
 * `pause` that stands, for the controller and an architect alike, and nothing of the claim
 * changes: a claim the pause stopped is the project's resume's to bring back. A suspension above
 * the caller is refused in its own code first, bar under the controller's `compact`, which the
 * door lets past it. `stop` and `suspend` start nothing and still run.
 *
 * `fresh_session` has that claim leave the session it recorded behind and start again in a new
 * one, the effect `legion claims retry --fresh-session` runs for an operator. It reaches a claim
 * whose agent is registered, ready or idle, one that is suspended, failed or retired, and one
 * stored working that no process stands behind, with no suspension held over it, a root
 * architect included: one idle on the provider-error ladder, one whose agent has registered and
 * is not ready yet, a suspended one, a failed one, one an operator stopped, which retires it,
 * and a dead architect its project controller has no other recovery for. That last claim is
 * suspended where the request arrives, leaves the session it recorded behind, and is started
 * again with the task its dead turn held. The daemon tells it from a claim in a turn by reading
 * the claim's process at the request: the record says `working` for both, and what the daemon
 * last heard of that process is memory a restart forgets.
 *
 * `compact` shortens the session instead, the effect `legion claims compact` runs for one: the
 * claim keeps its session, its workspace and everything it knows of its issue, and whatever task
 * it owed goes to it again. It reaches a narrower set, a claim whose agent is ready or idle and
 * no suspension is held over, since there has to be a running Oh My Pi session to ask.
 *
 * The daemon refuses each with the code and the sentence the controller reads. Each sentence
 * says what the claim is doing when the refusal is given, which operation reaches each state a
 * caller can read of it in `legion claims list`, and nothing of what the next turn, hold,
 * report or relaunch will make of it. Both give `CLAIM_IN_A_TURN` for a claim whose agent runs,
 * since cutting a running turn off would lose the turn that may be the one ending the run of
 * provider errors. Two claims take it: one stored working whose process the read at the request
 * finds running, and one whose session is being compacted, which is not read at all. A claim
 * stored working that a suspension is held over takes that code too, in the words of the hold,
 * except where the door refuses that suspension first, held or landed, because it outranks the
 * caller: the operator's for the controller's `fresh_session` (`SUSPENDED_BY_OPERATOR`), and the
 * operator's or the controller's for an architect's `fresh_session` and `compact`
 * (`SUSPENDED_BY_OPERATOR`, `SUSPENDED_BY_CONTROLLER`). No claim with no process behind it takes
 * it at either door, and nor does a claim whose process nobody read. Both give `CLAIM_UNSETTLED`
 * for a claim with no settled state to act on: one queued, one with a launch in flight, one a
 * suspension is held over in any state but working (a suspension the door refuses first again
 * excepted), and three claims stored working that take this
 * code rather than the turn's — one left working under a relaunch the daemon is pacing, one
 * whose process the runtime would not answer for, and one that records no process to read. The
 * last two are left exactly as the request found them and told which read did not happen;
 * `fresh_session` gives the same code, with nothing changed, for a claim that moved under the
 * request and that a second read finds no longer working or gone once more; a claim that moved
 * is otherwise answered as that second read finds it, and the compaction gives it for a claim
 * that moved out of working while the request was made. The compaction adds three refusals of
 * its own: `CLAIM_NOT_RUNNING` for a suspended, failed or retired claim, and for one stored
 * working that no process stands behind, which has no agent to ask either; all four run no
 * session at all and `fresh_session` reaches them, bar a claim a suspension that outranks the
 * caller holds, which `fresh_session` would be refused and whose sentence names that
 * suspension's resume instead. `CLAIM_NOT_ON_OMP` is for a role running on Claude Code, which
 * has no Oh My Pi session; and `COMPACTION_FAILED` for an ask that did not happen, an end the
 * daemon could not write, a claim that lost its process while its session was being compacted,
 * and an end a suspension held over the claim took, which suspends the claim instead. None of
 * those four gave the claim back, so none sent the task it owes. A registered claim no
 * suspension is held over is the compaction's `CLAIM_UNSETTLED` and no refusal at all for
 * `fresh_session`. */
export const LegionClaimRequest = z.strictObject({
  grantId: nonEmptyString,
  issue: nonEmptyString,
  role: z.enum(LEGION_ROLES),
});

/** `api.FreshSessionResponse`, what became of a fresh session. It is empty wherever the claim
 * came back and its start ran. `phaseNotRestarted` carries one of two refusals. The first is the
 * workflow's: the claim came back in a new session owing no task, and the workflow refused the
 * restart that would have handed it one, so the claim is running and holds no work. The second
 * is the machine's: this request stopped the claim, and a step that would have brought it back
 * was refused — the forget of the session it recorded, or the start of it in a new session. That
 * sentence names the state the step left the claim in: a suspended claim is one nothing starts
 * by itself, so a fresh session asked again is what starts it, and a claim in any other state is
 * read in `read_state` before anything more is asked of it. Both refusals ride a success answer,
 * because the request changed the claim either way. A reader of the claim cannot tell the first
 * one, since the state records nowhere that the restart did not run. */
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
  compacted: z.boolean().optional(),
  reason: z.string().optional(),
});
