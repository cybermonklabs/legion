package api

import (
	"fmt"
	"net/http"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
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
type OwedResponse struct {
	Owes bool   `json:"owes"`
	Work string `json:"work"`
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
// of every state that holds none), so its session cannot prove it holds the claim, and refusing
// it would read to the reminder as an answer that did not arrive, which nudges
// (UNREAD_CHECK in packages/pi-shared/src/phase-reminder.ts). So a claim this daemon
// holds failed, asked from the session it records, is answered that nothing is owed without the
// secret: the answer names no work, and that the claim is failed is what the unauthenticated
// GET /legion/v1/state already serves. That is what ends the reminding of a session whose
// provider refuses every turn: the daemon fails the claim once the run of refused turns reported
// over `POST /legion/v1/claims/turn-outcome` reaches `launch_failure_limit`, and the one delayed
// reminder each refused turn arms asks this route before it goes.
//
// A daemon that reads no issue record, or does not know which role works a phase
// (Options.Pool, Options.RoleFor), answers 500 rather than "nothing owed": the reminder that
// reads a failure still nudges and says it could not read the record, where an empty answer
// would silence it on a guess.
func (s *server) claimOwed(w http.ResponseWriter, r *http.Request) {
	var req OwedRequest
	if !s.readBody(w, r, &req, s.claimSecretCheck) || !requireFields(w,
		field{"claimToken", string(req.ClaimToken)}, field{"sessionId", req.SessionID}, field{"secret", req.Secret},
	) {
		return
	}
	_, c, ok := s.authenticated(req.ClaimToken, req.Secret)
	if !ok {
		if failed, held := s.supervisor.Machine(req.ClaimToken); held {
			if c := failed.Claim(); c.State == supervise.StateFailed && (c.Session == "" || c.Session == req.SessionID) {
				writeJSON(w, http.StatusOK, OwedResponse{})
				return
			}
		}
		writeJSON(w, claim.InvalidSecret.Status, claim.InvalidSecret)
		return
	}
	// The claim is read once: the session it is held to, the stale check and the answer all come
	// from the claim the secret was checked against (authenticated).
	if c.Session != "" && c.Session != req.SessionID {
		writeJSON(w, claim.InvalidSecret.Status, claim.InvalidSecret)
		return
	}
	if s.refuseIfStaleClaim(w, r, c) {
		return
	}
	// The architect and the controller work no phase of their own, so they owe no phase's work
	// and no look of a check: nothing is read for them.
	if !isPhaseWorker(c.Role) {
		writeJSON(w, http.StatusOK, OwedResponse{})
		return
	}
	if s.pool == nil || s.roleFor == nil {
		writeJSON(w, http.StatusInternalServerError, errorBody("the daemon reads no issue record, so it cannot say what this claim owes"))
		return
	}
	position, err := s.handoffPosition(r.Context(), c.Issue, c.Role, phase.Merging)
	if err != nil {
		s.logRequestFailure(r, err, "api: the record of a claim's issue could not be read for what it owes", "claim", req.ClaimToken)
		writeJSON(w, http.StatusInternalServerError, errorBody("read the issue record: "+err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, owedBy(c, position, s.roleFor))
}

// owedBy is what claim c owes with its issue's record standing at position, roleFor naming the
// role that works each phase (phase.RoleFor). Three sources can each seem to say: the claim's
// pending delivery and what it was left serving (supervise.Claim.Pending, ServingCheck), the
// issue's phase with the role's own row of it (record.PhaseRow.Entry and Completed) against the
// entry the claim was given (supervise.Claim.GivenEntry), and the issue's production check. They
// disagree, and the rule is this.
//
// The record decides whether anything is owed. The pending delivery is a copy of the workflow's
// decision taken when the task was queued, and the process's memory of its turn is a copy taken
// later still; the record is what the workflow writes in the very transaction that takes a
// completion or moves a phase. So a role whose phase has finished owes nothing whatever it was
// handed or remembers: the issue has moved on to another role's phase (RoleFor names another
// role), to one no role works (awaiting_person, done, held), or its tree lingers, where a
// completion records nothing; or the phase is merging and its pull request is merged, which is
// what finishes that phase (workflow.PhaseFinished) while a merged root stands in merging until
// the rest of its tree leaves.
//
// Within the role's own open phase, the two numbers the daemon closes the difference between
// decide, read as the daemon reads them, so this answer and the daemon's sending of the entry's
// work cannot part. A claim behind the entry its row names — the entry it was given is below the
// row's, the first term of workflow.OwedEntry, which admission, the entry sweep, a ready and the
// silence repair all send that entry's work on — owes that entry's work whatever the row records,
// because the daemon is sending it: the task arrives as the next prompt, or is already queued,
// which is the one term of OwedEntry this does not read, since a queued start leaves the work
// just as owed. A claim level with its row owes the phase while the row records no completion,
// whether or not its task has reached the session yet, and nothing once it records the
// completion the workflow took (a reviewer whose round waits for a review, a merger whose
// completion the workflow recorded without moving the issue): the daemon sends a level claim no
// entry's work, so the row is all that says the work stands. A merger in merging whose pull
// request is not merged owes the merge, since the merger merges.
//
// The claim decides one thing only, which kind of work its turn is on, read exactly as the
// completion route reads it (handoffComplete): a claim serving a look of a production check owes
// that look and nothing of a phase, and every other claim owes its phase or nothing. A look is
// owed while the record holds it open — the check open, its look open, the look the claim serves
// being the check's current one of this check instance, and the check the claim's role's — and a
// look the check has left owes nothing. A look the record holds open for
// the claim's role while the claim serves none — its task not yet taken, or a testing turn still
// running behind a merged close — is not owed yet: a report sent from that turn is refused as no
// report on production (CHECK_LOOK_NOT_SERVED), and the look's task comes as the next prompt.
func owedBy(c supervise.Claim, position completionPosition, roleFor func(phase.Phase) claim.Role) OwedResponse {
	issue := position.issue
	if issue == nil {
		return OwedResponse{}
	}
	if look := c.ServingCheck(); look.Look > 0 {
		check := position.check
		if check == nil || !check.Open() || !check.LookOpen || check.Looks != look.Look ||
			!check.NamedBy(look.OpenedAt) || check.Role != c.Role {
			return OwedResponse{}
		}
		return OwedResponse{Owes: true, Work: "the production check of " + issue.Key}
	}
	if position.lingers || roleFor(issue.Phase) != c.Role || position.merged() {
		return OwedResponse{}
	}
	if behind := c.GivenEntry < position.entry; !behind && position.completed {
		return OwedResponse{}
	}
	return OwedResponse{Owes: true, Work: fmt.Sprintf("the %s phase of %s", issue.Phase, issue.Key)}
}
