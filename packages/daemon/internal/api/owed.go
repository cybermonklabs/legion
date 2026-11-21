package api

import (
	"context"
	"fmt"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// OwedRequest is the body of POST /legion/v1/claims/owed: the claim the asking session holds,
// that session, and the secret the claim's registration issued it — the three fields
// claims/activity carries, and no generation, since the question changes nothing.
type OwedRequest struct {
	ClaimToken claim.Token `json:"claimToken"`
	SessionID  string      `json:"sessionId"`
	Secret     string      `json:"secret"`
}

// OwedResponse is the body POST /legion/v1/claims/owed answers: whether the claim owes work now,
// and what that work is called, as a person reads it in a reminder — "the testing phase of
// LEGION-208", "the production check of LEGION-208". Work is empty exactly when Owes is false, so
// a reminder renders the phrase it is given and never one of its own.
//
// Architect is the architect claim that owns the claim now (owningArchitect): the issue's own
// architect while its claim holds its role, else the nearest one above it whose claim does, else
// the tree root's. It is named for a phase worker whose issue the workflow records, whether or not
// the claim owes, and left out for an architect, the controller, a failed claim and an issue the
// workflow does not record. The reminder's architect notice goes to it (`tellArchitect` in
// packages/pi-shared/src/phase-reminder.ts), and so does the stall watch's notice of a stuck call
// (packages/pi-legion/src/stall-evidence.ts), so both reach the architect the daemon's own notices
// of the issue reach, without a read of the daemon's whole state.
type OwedResponse struct {
	Owes      bool        `json:"owes"`
	Work      string      `json:"work"`
	Architect claim.Token `json:"architect,omitempty"`
}

// claimOwed is a live role's session asking, at the end of its turn, what its claim owes now. It
// is what the end-of-turn reminder renders in both harnesses, so the session keeps no model of
// the issue's phase: a process restarted or replaced asks again and reads the same answer, which
// a reminder state held in the process cannot give (dispatch://INFRA-2746).
//
// It authenticates as claims/activity does — a blank field by its name with 400, a claim this
// daemon does not hold or a wrong secret with claim.InvalidSecret, a stale plugin with the
// stale-contract 409 — and holds the session to the one the claim records as well, which activity
// reads no further than its field. A session the claim does not record gets claim.InvalidSecret
// too: the answer names the claim's work, and the route tells it to no process but the one whose
// turn it is for, nor to a caller that cannot prove it holds the claim.
//
// A claim the daemon has failed is the one exception, and it is answered that it owes nothing.
// The failure is the daemon's own word that the session is finished: a retry starts the phase
// again in a session of its own, and a reminder written into this one reaches nobody who can act
// on it. A failed claim holds no secret any more (supervise.NormalizeClaim drops the capability
// of every state that holds none), so its session cannot prove it holds the claim, and a refusal
// would read to the reminder as a read that failed. A failed read reminds no one
// (dispatch://INFRA-2746 R7), but it is no answer: the reminder reads it as a daemon that could
// not say what the claim owes (UNREAD_CHECK in packages/pi-shared/src/phase-reminder.ts), where
// this daemon can. So a claim this daemon holds failed, asked from the session it records, is
// answered that nothing is owed without the secret: the answer names no work, and that the claim
// is failed is what the unauthenticated GET /legion/v1/state already serves. That is the answer
// that ends the reminding of a session whose provider refuses every turn: the daemon fails the
// claim once the run of refused turns reported over `POST /legion/v1/claims/turn-outcome` reaches
// `launch_failure_limit`, and the one delayed reminder each refused turn arms asks this route
// before it goes.
//
// A daemon that reads no issue record, or does not know which role works a phase
// (Options.Pool, Options.RoleFor), answers 500 rather than "nothing owed": the answer says only
// what the record says, and an empty answer would give a guess as the record's word. The reminder
// reads the 500 as a read that failed, which reminds no one and arms no reminder
// (dispatch://INFRA-2746 R7). A phase worker that owes its phase and goes quiet is sent its task
// again by the daemon's silence sweep (tellSilentPhaseWorkers in internal/daemon/silence.go), not
// by a reminder.
func (s *server) claimOwed(w http.ResponseWriter, r *http.Request) {
	var req OwedRequest
	if !s.readBody(w, r, &req, s.claimSecretCheck) || !s.requireFields(w, r,
		field{"claimToken", string(req.ClaimToken)}, field{"sessionId", req.SessionID}, field{"secret", req.Secret},
	) {
		return
	}
	_, c, ok := s.authenticated(req.ClaimToken, req.Secret)
	if !ok {
		if failed, held := s.supervisor.Machine(req.ClaimToken); held {
			if c := failed.Claim(); c.State == supervise.StateFailed && (c.Session == "" || c.HoldsSession(req.SessionID)) {
				s.writeAnswer(w, r, AnswerOwed, http.StatusOK, OwedResponse{})
				return
			}
		}
		s.writeAnswer(w, r, AnswerRefusal, claim.InvalidSecret.Status, claim.InvalidSecret)
		return
	}
	// The claim is read once: the session it is held to, the stale check and the answer all come
	// from the claim the secret was checked against (authenticated). The claim holds its session as
	// the store keeps it, so the request's is compared as the store would keep it (HoldsSession).
	if c.Session != "" && !c.HoldsSession(req.SessionID) {
		s.writeAnswer(w, r, AnswerRefusal, claim.InvalidSecret.Status, claim.InvalidSecret)
		return
	}
	if s.refuseIfStaleClaim(w, r, c) {
		return
	}
	// The architect and the controller work no phase of their own, so they owe no phase's work
	// and no look of a check, and no architect above them is named: nothing is read for them.
	if !isPhaseWorker(c.Role) {
		s.writeAnswer(w, r, AnswerOwed, http.StatusOK, OwedResponse{})
		return
	}
	if s.pool == nil || s.roleFor == nil {
		s.refuse(w, r, http.StatusInternalServerError, "the daemon reads no issue record, so it cannot say what this claim owes")
		return
	}
	position, err := s.handoffPosition(r.Context(), c.Issue, c.Role)
	if err != nil {
		s.logRequestFailure(r, err, "api: the record of a claim's issue could not be read for what it owes", "claim", req.ClaimToken)
		s.refuse(w, r, http.StatusInternalServerError, "read the issue record: "+err.Error())
		return
	}
	var owner claim.Token
	if position.issue != nil {
		if owner, err = s.owningArchitect(r.Context(), *position.issue); err != nil {
			s.logRequestFailure(r, err, "api: the architect that owns a claim could not be read for what it owes", "claim", req.ClaimToken)
			s.refuse(w, r, http.StatusInternalServerError, "read the architect that owns the claim: "+err.Error())
			return
		}
	}
	s.writeAnswer(w, r, AnswerOwed, http.StatusOK, owedBy(c, position, owner, s.roleFor))
}

// owningArchitect is the architect claim that owns a phase worker on issue now (OwedResponse's
// Architect): the end of the owner walk from issue itself (record.OwnerWalk.Architect), over the
// architect claims of issue's tree as the store records them. That is the walk the daemon's own
// notices about a phase worker take (workflowRuntime.owningArchitect in
// internal/daemon/specs.go): the issue's own architect while its claim holds its role, else the
// nearest one above it that does, else the tree root's. It reads the tree's issues and their
// architect claims, so it grows with the tree and not with the deployment, in a read-only
// transaction of its own, which the completion route, sharing handoffPosition, does not pay for.
func (s *server) owningArchitect(ctx context.Context, issue record.Issue) (claim.Token, error) {
	var owner claim.Token
	err := pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		walk, err := record.ReadOwnerWalk(ctx, s.records, tx, s.project, issue.Tree)
		if err != nil {
			return err
		}
		owner, err = walk.Architect(issue, false)
		return err
	})
	return owner, err
}

// owedBy is what claim c owes with its issue's record standing at position, roleFor naming the
// role that works each phase (phase.RoleFor), and owner the architect that owns c
// (owningArchitect), which every answer about a recorded issue names, owed or not.
//
// The answer reads the record one way (dispatch://INFRA-2746 R1). Every claim it tells it owes a
// completion would have that completion taken by the completion route (answerCompletion), the
// work done, at the head the pull request stands at: it is never refused as of no run
// (HANDOFF_NO_RUN), of a run the issue has left (HANDOFF_STALE_GENERATION), of an entry the phase
// has moved past (HANDOFF_STALE_ENTRY) or of a phase the role does not work
// (HANDOFF_NOT_CURRENT_PHASE). The other way does not hold, and it is not wanted: the route also
// records a completion that arrives after the daemon moved the issue out of the role's phase
// (workflow's leftItsPhase), and a phase the daemon ended is not reminded (R8).
//
// The record decides, and not what the claim was handed or remembers. The pending delivery is a
// copy of the workflow's decision taken when the task was queued, and the process's memory of its
// turn is a copy taken later still; the record is what the workflow writes in the very transaction
// that takes a completion or moves a phase. The claim says only which kind of work its turn is on,
// and which run and entry it is on, each read exactly as the completion route reads it.
//
// A claim serving a look of a production check owes that look and nothing of a phase. A look is
// owed while the record holds it open — the check open, its look open, the look the claim serves
// being the check's current one of this check instance, and the check the claim's role's — and a
// look the check has left owes nothing. A look the record holds open for the claim's role while
// the claim serves none — its task not yet taken, or a testing turn still running behind a merged
// close — is not owed yet: a report sent from that turn is refused as no report on production
// (CHECK_LOOK_NOT_SERVED), and the look's task comes as the next prompt.
//
// Every other claim owes its phase while the five terms of R1 all hold, and nothing otherwise. The
// second term is this function's own but for its last clause; that clause and the other four are
// record.PhaseOwed, the one rule the idle release's reminder hold and the idle backstop read too
// (workflow's IdleRelease, internal/daemon's getsBackstop), so a pod is held for a reminder only
// of a claim this route tells it owes its phase:
//
//   - The claim serves a run, and that run is the issue's current generation. The run and the
//     entry are claimServes's, the one function the completion route (answerCompletion) reads for
//     its HANDOFF_NO_RUN and HANDOFF_STALE_GENERATION refusals and for its entry fence, so the two
//     routes read one rule. The daemon's own delivery of a relaunch's record (supervise.HoldsTask)
//     carries no run, no entry and no look, so a claim that holds it and was left serving no run
//     serves none, confirmed or not; and a wake's task names no phase, so a claim that holds one
//     unconfirmed and has served no run serves none either.
//   - The claim's role works the phase the issue stands on (roleFor), the tree does not linger,
//     where a completion records nothing, and the phase is not merging with its pull request
//     merged (record.MergedInMerging), which is what finishes merging (workflow.PhaseFinished)
//     while a merged root stands in merging until the rest of its tree leaves. So a role whose
//     phase has ended owes nothing, whatever it was handed: the issue moved to another role's
//     phase, to one no role works (awaiting_person, done, held), or its tree closed. A merger in
//     merging whose pull request is not merged owes the merge, since the merger merges.
//   - The claim is neither behind nor above the entry its role's phases row stands on
//     (record.PhaseRow.Entry), where both are set; where either is 0 nothing is compared. The
//     route's entry fence (entryFence) reads the same predicate (record.EntriesDiffer). A claim
//     behind its entry owes nothing: the route refuses its completion HANDOFF_STALE_ENTRY
//     wherever behindEntryClause gives a clause, and the daemon sends that entry's task itself
//     (the delivery it already queued, the owed-entry repair, the silence sweep), so the claim
//     stands level once a turn confirms that task. The entry read is the one the claim serves, not
//     the one it was given (supervise.Claim.GivenEntry): a task queued and not yet confirmed
//     leaves the claim behind for the route.
//   - The row does not record the claim's completion of that entry (record.PhaseRow.Completed). A
//     reviewer whose round waits for a review, or a merger whose completion the workflow recorded
//     without moving the issue, owes nothing, and the daemon sends a level claim no entry's work.
//   - No merge forward holds the claim's role (record.MergeForwardHolds): the daemon asked the
//     implementer to merge the base forward onto the head the pull request stands at, in the phase
//     the issue stands in, the merge has not landed, and the ask's hold told this role — the
//     tester, the reviewer or the merger of that phase — to stop and to wait for the new head. The
//     completion fence of a merge forward (workflow's mergedForwardRefusal) is the other reader of
//     that rule. The hold reaches no implementer: an ask in implementing or in retro is its own
//     open work, which is what a reminder is for.
//
// A prompt of the task Oh My Pi acknowledged and has not answered (supervise's
// Machine.outstanding) makes a late turn the task's, so the confirmation it gives is what
// ServingCheck, ServingRun and ServingEntry, and the completion route with them, read here.
func owedBy(c supervise.Claim, position completionPosition, owner claim.Token, roleFor func(phase.Phase) claim.Role) OwedResponse {
	issue := position.issue
	if issue == nil {
		return OwedResponse{}
	}
	nothing := OwedResponse{Architect: owner}
	if look := c.ServingCheck(); look.Look > 0 {
		check := position.check
		if check == nil || !check.Open() || !check.LookOpen || check.Looks != look.Look ||
			!check.NamedBy(look.OpenedAt) || check.Role != c.Role {
			return nothing
		}
		return OwedResponse{Owes: true, Work: "the production check of " + issue.Key, Architect: owner}
	}
	if position.lingers || roleFor(issue.Phase) != c.Role {
		return nothing
	}
	serving, entry, _ := claimServes(c, *issue)
	if !record.PhaseOwed(serving, issue.Generation, entry, position.entry, position.completed,
		record.MergeForwardHolds(position.pullRequest, issue.Phase, c.Role),
		record.MergedInMerging(position.pullRequest, issue.Phase)) {
		return nothing
	}
	return OwedResponse{Owes: true, Work: fmt.Sprintf("the %s phase of %s", issue.Phase, issue.Key), Architect: owner}
}
