package api

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/credential"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store/storetext"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// HandoffCompleteRequest is what a worker reports when its phase is done, or what the checking
// role reports on a look of its closed issue's production check. A phase with a handoff (planning,
// implementing, testing, reviewing; phase.Handoff) ends with a handoff record on the Dispatch
// issue, which the worker writes with `dispatch handoff-write` under its own session, and its
// completion names it: HandoffID and HandoffVersion, which the `legion` tool's handoff_complete
// reads as the newest record of the phase before posting. The daemon reads that record back from
// Dispatch and holds it to the issue, the phase's word, the version and the claim's session
// (handoffRecord); it reads no body, which is the next role's to read. Commit is the commit the
// completion is of: for the planner, the implementer's implementing round, the tester and the
// reviewer the head the handoff record names, the commit their work was done against, which the
// `legion` tool reads out of the record's body and, for the implementer, holds to be its pushed
// @- on legion/<issue>@origin; the commit the implementer stands on for retro; and the commit the
// merger's own completion stands on. The entry fence reads it against the head the record holds
// (intake.BehindEntryClause). The workflow refuses a record the role already named for its previous
// phase (HANDOFF_NOT_NEW). The implementer's completion of implementing is also held to an open
// pull request on its branch, or a merged one recorded (implementationPullRequest:
// HANDOFF_NO_PULL_REQUEST), and for an issue with plan rows, a plan of one task among them, to an
// open one against the base the plan gives the task (HANDOFF_PULL_REQUEST_BASE); an issue with no
// plan rows reads no base. A check report carries no commit and no record (nothing was pushed or
// written) and a Verdict of pass, pending or fail; a tester's completion a Verdict of pass or fail.
// WakeAt is the check report's alone: on a pending it is when the checking agent asks to be woken
// for its next look, which no other verdict and no phase completion carries (checkReport). It is a
// pointer so that a report naming no time is told from one naming a time at all: with the field
// left out it decodes to nil, and a pending of it is refused (CHECK_WAKE_REQUIRED). A time on the
// wire is a named time whatever it reads — `"0001-01-01T00:00:00Z"` decodes to a non-nil pointer
// and passes that refusal — and the workflow moves a time already past out to the wake floor, a
// minute from now (workflow.CheckWakeFloor), so such a report starts the next look a minute on.
// PlanRating and PlanReason are the rating of the plan the issue ran, which the completion that
// ends the retro carries and no other completion does (PlanRatingContract). Both are pointers for
// the reason WakeAt is: a completion naming no rating is told from one naming 0, which is refused
// as outside 1 to 5, and a completion naming no reason from one naming "", which with a rating is
// refused as blank, so a completion that names either member with a value is held to the rule of
// the rating.
// A member written null reads as left out, since encoding/json decodes null into a pointer as nil;
// the contract's LegionHandoffCompleteRequest, which the plugins' daemon client
// (packages/pi-shared/src/daemon-client.ts) parses each completion with, refuses null, so no
// plugin sends one. ReturnTo is the step of the issue's running task that a failed
// step names as the one its task returns to, by its place in the task, 1 for its first: only
// a tester's completion with the verdict fail names one (returnToRefusal), and it is a pointer for
// the reason PlanRating is, so a completion naming 0 is refused rather than read as naming none.
// Which step it may name is the workflow's to judge against the task's steps (RETURN_REFUSED).
type HandoffCompleteRequest struct {
	GrantID        string     `json:"grantId"`
	Summary        string     `json:"summary"`
	Verdict        string     `json:"verdict"`
	Commit         string     `json:"commit,omitempty"`
	HandoffID      string     `json:"handoffId,omitempty"`
	HandoffVersion int        `json:"handoffVersion,omitempty"`
	WakeAt         *time.Time `json:"wakeAt,omitempty"`
	PlanRating     *int       `json:"planRating,omitempty"`
	PlanReason     *string    `json:"planReason,omitempty"`
	ReturnTo       *int       `json:"returnTo,omitempty"`
	// Ready is read only so that the body of a plugin below contract 86 is read: such a plugin
	// sends `ready` on every call, and readBody refuses a member no field names. What each value
	// means is the sentence of the field's row in requests.go's AcceptedByName.
	Ready *bool `json:"ready,omitempty"`
}

// readyRetired is the refusal of a completion that sends `ready: true`. Accepting it would change
// what the call means: at 85 it armed the daemon's merge, and on this daemon nothing would merge.
const readyRetired = "this completion sends ready: true, which at contract 85 and below armed the daemon's merge of the pull request. " +
	"No daemon at contract 86 or later acts on READY: the merger merges the pull request itself, and the daemon closes " +
	"the issue when it reads that merge. Nothing was recorded. Merge the pull request, then complete again without ready"

// HandoffCompleteResponse confirms that the completion fact committed. It carries nothing: the
// daemon merges nothing and reads no head, so no completion has anything to answer with.
type HandoffCompleteResponse struct{}

// handoffComplete is the completion route: it answers the call (answerCompletion) and writes one
// line in the daemon's log for that answer, `api: handoff completion answered`, naming the issue,
// the role and the claim of the grant the call redeemed (empty for a call refused before it
// redeemed one), the record and the commit the call named, and the status and the refusal code
// it was answered with. Every return of answerCompletion writes its answer through the one
// completionAnswer, so every answer is one line and one call is one line: a client's retry is a
// line of its own, which is how a reader of the log counts the requests one tool call made. The
// status is the first one the answer's WriteHeader named, or net/http's 200 when it named none.
// The code is the one writeFailure, which writes every coded refusal of this route, tells the
// writer as it writes the refusal (refusalNoter); it is "" for a success and for a refusal that
// carries none, an undecodable body's (refuseBadBody). Nothing reads the answer's body back.
func (s *server) handoffComplete(w http.ResponseWriter, r *http.Request) {
	answer := &completionAnswer{ResponseWriter: w}
	grant, req := s.answerCompletion(answer, r)
	s.log.Info("api: handoff completion answered", "issue", grant.Issue, "role", grant.Role, "claim", grant.Claim,
		"handoffId", req.HandoffID, "handoffVersion", req.HandoffVersion, "commit", req.Commit,
		"status", answer.answered(), "code", answer.code)
}

// completionAnswer is the writer answerCompletion answers through: it keeps the first status
// written and the code writeFailure tells it of the refusal it writes (noteRefusal), which the
// route's line names. It has no Write of its own, so the body goes to the wrapped writer as
// writeAnswerFrom wrote it, under the Content-Type writeAnswerFrom set.
type completionAnswer struct {
	http.ResponseWriter
	status int
	code   string
}

func (a *completionAnswer) WriteHeader(status int) {
	if a.status == 0 {
		a.status = status
	}
	a.ResponseWriter.WriteHeader(status)
}

// noteRefusal is writeFailure telling the answer the code (Failure.Code) of the refusal it writes.
func (a *completionAnswer) noteRefusal(code string) {
	a.code = code
}

// answered is the status the answer went out with: the first WriteHeader named, and
// http.StatusOK, which net/http sends, when none was named.
func (a *completionAnswer) answered() int {
	if a.status == 0 {
		return http.StatusOK
	}
	return a.status
}

// answerCompletion records a phase completion or a production check report (checkReport), and
// answers the grant it redeemed — the zero grant for a call refused before it redeemed one — and
// the request as it decoded it, for the route's line (handoffComplete). The grant is redeemed and
// the record read before the two are told apart, so an invalid grant is refused before a field
// only one of them requires. Which of the two a call is, is what the caller's own
// live assignment says: a claim whose worker took a look of its issue's production check
// (supervise.Claim.ServingCheck) reports on that look, so the report lands where the check's own
// refusals are written — CHECK_NOT_OPEN when the issue has no open check, CHECK_ROLE_MISMATCH
// when the caller is not the role the check records — and every other call is a phase completion.
// The issue's phase is not what decides it: under `production_check: tester` a merged close
// spares the tester's claim and opens a look behind its running testing turn, and that turn's own
// verdict — pass or fail, which a check report also names — would be recorded as a report on
// production nobody made. A phase completion is held to its commit (the implementer's and the
// merger's), the Dispatch handoff record a file-backed phase ends with (handoffRecord), its
// role's verdict rule, the wake no phase carries (CHECK_WAKE_UNEXPECTED), the run and the phase
// entry its claim serves, the implementer's in implementing to an open pull request on its
// branch or a merged one recorded (implementationPullRequest), and one of a phase its role does
// not run is refused HANDOFF_NOT_CURRENT_PHASE: by the entry fence (intake.EntryFence), before the fact,
// when the entry its claim serves differs from the role's phases row, and otherwise by the engine,
// which records instead a completion that arrives after the daemon moved the issue on by an event
// (workflow's leftItsPhase). A completion of a done issue, which runs no phase, by a session
// working no look is neither of the two: CHECK_LOOK_NOT_SERVED.
func (s *server) answerCompletion(w http.ResponseWriter, r *http.Request) (grant credential.Grant, req HandoffCompleteRequest) {
	if !s.readBody(w, r, &req, s.redeemIdentityCheck) || !s.requireFailureFields(w, r, field{"grantId", req.GrantID}, field{"summary", req.Summary}) {
		return
	}
	grant, ok := s.redeem(w, r, req.GrantID)
	if !ok {
		return
	}
	if grant.Controller {
		s.writeFailure(w, r, http.StatusForbidden, "GRANT_INVALID", "grant is unavailable")
		return
	}
	if req.Ready != nil && *req.Ready {
		s.writeFailure(w, r, http.StatusBadRequest, "READY_RETIRED", readyRetired)
		return
	}
	if s.pool == nil || s.records == nil {
		s.writeFailure(w, r, http.StatusInternalServerError, "FACTS_UNAVAILABLE", "fact intake is unavailable")
		return
	}
	machine, running := s.supervisor.Machine(grant.Claim)
	if !running {
		s.writeFailure(w, r, http.StatusConflict, "HANDOFF_NO_CLAIM", "the daemon supervises no claim for this grant")
		return
	}
	// The run, the phase entry and the production check look this completion belongs to are the
	// task's that the worker took, which the claim answers (supervise.Claim.ServingRun,
	// ServingEntry, ServingCheck) and nothing on the request says. The claim is read once,
	// here, and the route, the run, the entry, the check and the queued task the refusals name all
	// come from that one read: Claim() waits for the decision in flight, so two reads can straddle
	// one and answer from different decisions — a run taken before a delivery was confirmed beside
	// the entry that confirmation left.
	c := machine.Claim()
	look := c.ServingCheck()
	position, err := s.handoffPosition(r.Context(), grant.Issue, grant.Role, grant.Claim, look.OpenedAt)
	if err != nil {
		s.writeFailure(w, r, http.StatusInternalServerError, "RECORD_UNAVAILABLE", "could not read the issue record")
		return
	}
	issue := position.issue
	if issue == nil {
		s.writeFailure(w, r, http.StatusNotFound, "ISSUE_NOT_FOUND", "issue is not recorded")
		return
	}
	// A completion is a check report when the caller's own live assignment is a look of its
	// issue's production check (the doc above), and a phase completion otherwise.
	if look.Look > 0 {
		s.checkReport(w, r, grant, position.check, look, req)
		return
	}
	// A done issue runs no phase, so a completion from a session working no look of its
	// production check is neither of the two, and the cause is named here rather than left to
	// the phase path below: that path holds an implementer's and a merger's completion to a
	// commit (MISSING_FIELD), which the plugin's done branch never sends, since it sends what a
	// check report sends (pi-shared's legion-phase-ops.ts, completeHandoff). A late implementer
	// or merger turn after the close lands here, an architect's on a done root does, and so does
	// a look a restart of the daemon left unrecorded on its claim. Nothing of this call is
	// recorded either way, so the answer is the whole of it, and it says who reports here and
	// who has nothing to: the check names one role, and only a turn of that role's own look task
	// reports on it.
	if issue.Phase == phase.Done {
		whose := "it has no open production check, so there is nothing here for any role to report"
		if position.check != nil && position.check.Open() {
			whose = fmt.Sprintf(
				"its production check is the %s's, and only a turn of that role's own look task reports on it, naming the look and a verdict of pass, pending or fail; every other role has nothing to report here",
				position.check.Role)
		}
		s.writeFailure(w, r, http.StatusConflict, "CHECK_LOOK_NOT_SERVED", fmt.Sprintf(
			"%s is done: no phase of it is open, and this session works no look of its production check. This completion reported on nothing and recorded nothing: %s.",
			grant.Issue, whose))
		return
	}
	// A completion of an issue with plan rows is of one task of its plan (handoffPosition: the task
	// of the step its claim's agent runs, else of the step the latest phases row naming its claim
	// runs, else the task that merges), and issue stands where it stands for that task from here
	// on. A completion of no task of the plan completes no phase of it, and nothing is recorded.
	if position.noStep {
		refusal := intake.NotCurrentPhase(grant.Role, issue.Phase, grant.Issue)
		s.writeFailure(w, r, refusal.Status, refusal.Code, refusal.Message)
		return
	}
	// A role the deployment defines reports its step as a tester does: with the verdict pass or
	// fail and the commit it read, and with no handoff record (the phase step ends with none). The
	// rules read the role the grant's claim holds, never the settings: a role outside Legion's six
	// is a role of the deployment, whichever agent's claim, named for that role or keyed and on a
	// step of it, holds it.
	deploymentRole := !claim.IsBuiltin(grant.Role)
	if (grant.Role == claim.RoleImplementer || grant.Role == claim.RoleMerger || deploymentRole) && !s.requireFailureFields(w, r, field{"commit", req.Commit}) {
		return
	}
	if grant.Role == claim.RoleTester && req.Verdict != "pass" && req.Verdict != "fail" {
		s.writeFailure(w, r, http.StatusBadRequest, "TESTER_VERDICT_REQUIRED", "tester verdict must be pass or fail")
		return
	}
	if deploymentRole && req.Verdict != "pass" && req.Verdict != "fail" {
		s.writeFailure(w, r, http.StatusBadRequest, "STEP_VERDICT_REQUIRED", fmt.Sprintf("a step of the role %s reports the verdict pass or fail; nothing was recorded", grant.Role))
		return
	}
	if grant.Role != claim.RoleTester && !deploymentRole && req.Verdict != "" {
		s.writeFailure(w, r, http.StatusBadRequest, "VERDICT_ROLE_FORBIDDEN", "only a tester or a role of the deployment reports a verdict")
		return
	}
	// The step a failed step's task returns to is named by a fail alone, the tester's or a role of
	// the deployment's (returnToRefusal), refused here on every other completion before anything is
	// read or recorded.
	if refusal := returnToRefusal(req, req.Verdict == "fail", fmt.Sprintf("this completion of phase %s of %s", issue.Phase, grant.Issue)); refusal != nil {
		s.writeFailure(w, r, refusal.Status, refusal.Code, refusal.Message)
		return
	}
	// A wake is a production check's pending report alone: it names when that check's next look is
	// due, and a phase ends with no look to follow it. The contract says so for every client
	// (contracts' LegionHandoffCompleteRequest), so the daemon holds a phase completion to it
	// rather than dropping the field unread. Written before the fact is applied, as every refusal
	// of this route is, so nothing is recorded and the same completion without a time is applied.
	if req.WakeAt != nil {
		s.writeFailure(w, r, http.StatusBadRequest, "CHECK_WAKE_UNEXPECTED", fmt.Sprintf(
			"this completion of phase %s of %s names wakeAt, which belongs to the pending report of a production check's look and to nothing else; send it again without a time, and nothing was recorded",
			issue.Phase, grant.Issue))
		return
	}
	serving, entry := c.ServingRun(), c.ServingEntry()
	// A claim that has retired no run and holds a task its agent neither confirmed nor was marked
	// as reading is working that held task: that is exactly the state a busy refusal, a late
	// refusal, a lost prompt or a failed adoption leaves behind while the agent goes on and does
	// the phase's work in a turn of its own, and refusing its completion costs the issue a phase
	// for nothing. So the run and the entry are taken from the task the claim is holding: that
	// task names the generation and the phase itself, so the route can answer from it rather
	// than from the turn that never announced itself.
	//
	// The fallback applies only when that task is the issue's own current work. A task of another
	// run, of another phase, or of no phase at all — an operator's own, which carries generation 0
	// — is none of the work a completion could be of, and is refused below, whether the claim
	// answered for it (supervise's serving reads such a task too, for a claim that has served
	// nothing) or answered for nothing. Why the held task is not the issue's current task is one
	// question with one answer (heldTaskNotCurrent), and this route is its one reader, so the 409
	// can never describe a rule this route does not apply. The refusal asks it too when the
	// fallback did not: a claim that has retired no run and holds a task the agent read or a turn
	// confirmed, which carries no run — an operator's own, or the daemon's own delivery of what its
	// relaunch ended (supervise.Delivery.Relaunch), which the turn that reads it confirms — serves
	// no run either, and the refusal says what the claim holds rather than giving no reason
	// (noRunCompletion).
	//
	// What the narrowness does not buy is a claim with no earlier run to confuse this completion
	// with. Nothing on this route confirms a delivery or retires a run, so a claim whose held task
	// stays unconfirmed still answers from that same task after a completion this fallback
	// admitted, and a later entry of the same generation and the same phase takes it again. What
	// keeps such an entry's completion from being the entry before it is the held task's own
	// entry — each entry hands the role a task of its own number, and the entry fence below
	// compares the task's against the row's — and, for a phase that ends with a handoff record,
	// the workflow's HANDOFF_NOT_NEW, which refuses a completion naming the record the role
	// wrote for its previous entry. Neither fence is this fallback's doing; they are the ones
	// every completion passes.
	//
	// Every other fence reads what it read before. The adopted run is the issue's own generation,
	// so the stale-generation refusal and its two tails never fire for it, and the phase the
	// engine holds the completion to, the session the handoff record was written under, the
	// record's own checks, the implementer's pull-request check and the idempotency key, whose
	// run is the issue's generation, are untouched.
	var notCurrent string
	if c.ServingGeneration == 0 && (c.Pending == nil || (c.Pending.ConfirmedAt.IsZero() && c.Pending.DeliveredAt.IsZero())) {
		if notCurrent = heldTaskNotCurrent(grant.Issue, issue.Generation, issue.Phase, c.Pending); notCurrent == "" {
			serving, entry = c.Pending.Generation, c.Pending.Entry
		} else {
			serving, entry = 0, 0
		}
	}
	if serving == 0 {
		if notCurrent == "" {
			notCurrent = heldTaskNotCurrent(grant.Issue, issue.Generation, issue.Phase, c.Pending)
		}
		s.writeFailure(w, r, http.StatusConflict, "HANDOFF_NO_RUN", noRunCompletion(grant.Issue, issue.Generation, issue.Phase, notCurrent))
		return
	}
	// A completion of a run that is over is refused here, before the fact is applied, so nothing is
	// recorded under its key: a refusal the workflow committed would be stored as processed, and the
	// worker's retry at the same commit would be answered HANDOFF_ALREADY_RECORDED, "received", while
	// the phase waits. The worker's turn began before the current run's task reached it; that task
	// arrives as its next prompt, and the same completion in its turn is applied.
	if serving != issue.Generation {
		s.writeFailure(w, r, http.StatusConflict, "HANDOFF_STALE_GENERATION", staleCompletion(grant.Issue, issue.Generation, serving, c.Pending))
		return
	}
	// The entry fence holds the completion to the phase entry its claim serves (intake.EntryFence),
	// on this route's read, before the fact's lock: the refusal here is the early one, and the
	// workflow judges the same rule again on the row its own transaction reads.
	if refusal := intake.EntryFence(*issue, grant.Role, entry, position.entry, position.entryMovedBack, req.Commit, position.pullRequest, c.Pending); refusal != nil {
		s.writeFailure(w, r, refusal.Status, refusal.Code, refusal.Message)
		return
	}
	// The phase this completion is of is the issue's where the role works it, a running step of the
	// role standing there (record.Issue.RunningAt: at step, the roles of the running steps, so a role
	// the deployment defines is current only at its own step), the phase the role left otherwise
	// (phase.LeftBy), and the issue's for the
	// architect, which leaves none. The record a phase with a handoff ends with is read back and
	// held to the completion before anything is recorded: always when the role works the issue's
	// phase, and for a phase it left only when the completion names one. A completion of a left
	// phase that names none is the engine's to decide (workflow's leftItsPhase), which answers
	// HANDOFF_NOT_CURRENT_PHASE when the role's completion of that phase already stands or no role
	// works the issue's phase. A phase that writes none takes no record, and a completion of such a
	// phase that names one is refused.
	of := phase.LeftBy(grant.Role, issue.Phase)
	if slices.ContainsFunc(issue.RunningAt(issue.Phase), func(step record.RunningStep) bool { return step.Role == grant.Role }) {
		of = issue.Phase
	}
	if of == "" {
		of = issue.Phase
	}
	// The rating of the plan is held here to its form alone (planRatingOf), which no state of the
	// issue changes; which completion ends the retro, and so carries a rating or must, is the
	// workflow's to judge where it ends the retro, on the issue its transaction reads under the
	// fact's lock (PlanRatingContract). It is read past the fences above, which decide whether
	// this completion is the turn's own work at all, and before the handoff record and the pull
	// request are read, so a rating of the wrong form costs no read of Dispatch or GitHub.
	rating, refusal := planRatingOf(req)
	if refusal != nil {
		s.writeFailure(w, r, refusal.Status, refusal.Code, refusal.Message)
		return
	}
	word, writer, handoffPhase := phase.Handoff(of)
	namesRecord := req.HandoffID != "" || req.HandoffVersion != 0
	recordBacked := handoffPhase && writer == grant.Role && (of == issue.Phase || namesRecord)
	switch {
	case recordBacked:
		if !s.handoffRecord(w, r, c.Session, issue, of, word, req) {
			return
		}
	case !handoffPhase && namesRecord:
		s.writeFailure(w, r, http.StatusConflict, "HANDOFF_RECORD_MISMATCH", fmt.Sprintf(
			"phase %s of %s ends with no handoff record, and this completion names one: complete without handoffId and handoffVersion; this completion changed nothing", of, issue.Key))
		return
	}
	treeGeneration, pass := position.treeGeneration, position.pass
	// One phase's completion is identified by where the issue stands — its generation, phase, and
	// pass through that phase — with the role and what it reported: the handoff record a
	// file-backed phase names, or the commit every other phase reports, and the verdict. A
	// retried call for the same phase is the same fact, and so is one naming the same record at
	// another commit (the record is the phase's handoff); the next phase's completion at the same
	// commit (the implementer's retro, standing on its implementing pass's last push) is a
	// different one, since the phase and the role are in the key; and so is a second report of the
	// same phase naming another verdict — the tester's corrected pass or fail, the only field a
	// role may report differently of one handoff — since a refusal the workflow commits is
	// recorded as processed, and a corrected report must not be answered as that refusal's
	// duplicate. The summary is left out on purpose: a call that only rewords it is the same
	// completion, and the first one is what the workflow took. The position is read before the
	// fact's own transaction: should the issue move in between, the engine re-reads
	// it there and refuses a completion whose role no longer owns the phase, or whose run is no
	// longer the issue's (workflow's HANDOFF_STALE_GENERATION guard).
	//
	// The run the completion is attributed to is part of the key for that guard's sake: a completion
	// the route let through and the engine then refused as stale is recorded under it, and without
	// the run it would take the key of the new run's completion of the same phase with the same
	// report, which would be answered ALREADY_RECORDED and never reach the workflow. With it, that
	// recorded refusal blocks no later call: its key carries the issue generation this route read,
	// the one the issue has since left, so a retry is refused here (recording nothing) and the new
	// run's completion carries the new issue generation and serving run, another key. The tree's
	// generation is part of it for the same reason: re-admission starts a new generation of the tree
	// by bumping only the root's, and restarts a child's worker in the phase it stood in, so a
	// child's completion refused before (while the tree lingered, or while the old gate was closed)
	// would otherwise take the new generation's key.
	//
	// For an issue with plan rows the pass is the completing task's (completionPosition), and the
	// task is in the key before it, so a new task at pass 0 takes no key an earlier task of the
	// issue used. The phase in the key is that task's own, so two tasks' completions of one role and
	// one pass take two keys, and for a plan of one task it is the issue's, as before tasks. An
	// issue with no plan rows keeps today's key, so a completion retried across the upgrade keeps
	// its key.
	//
	// The step a fail names its task's return to (returnTo) ends the key when the completion
	// names one, for the reason the verdict is in it: the workflow commits its refusal of a step it
	// may not name (RETURN_REFUSED) as processed, so the same fail naming another step must be a
	// fact of its own. A completion naming none keeps the key it had before returnTo existed.
	reported, received := req.Commit, "at commit "+req.Commit
	if recordBacked {
		reported, received = req.HandoffID, fmt.Sprintf("naming handoff record %s (%s version %d)", req.HandoffID, word, req.HandoffVersion)
	}
	counted := fmt.Sprint(pass)
	if position.task != "" {
		counted = position.task + ":" + counted
	}
	eventID := fmt.Sprintf("handoff:%s:%d:%d:%d:%s:%s:%s:%s:%s", grant.Issue, treeGeneration, issue.Generation, serving, grant.Role, issue.Phase, counted, reported, req.Verdict)
	returnTo := 0
	if req.ReturnTo != nil {
		returnTo = *req.ReturnTo
		eventID += fmt.Sprintf(":%d", returnTo)
	}
	// The phase's last handoff is stored text, so what this completion reports is compared as the
	// store would keep it (storetext.Text).
	if grant.Role == claim.RoleImplementer && issue.Phase == phase.Implementing && !position.lingers && storetext.Text(reported) != position.lastHandoff {
		if !s.implementationPullRequest(w, r, position, eventID) {
			return
		}
	}
	// The rating rides the fact as the completion named it, beside whether the claim must rate,
	// and the workflow judges both (PlanRatingContract). It is left out of the key as the summary
	// is: the first completion the workflow took is what stands, so a retry that rates again is
	// answered already received and the first rating stands, and a completion the workflow
	// refused for its rating leaves the key unspent, so the same completion, corrected, applies.
	result, err := s.apply(r.Context(), "api", eventID, intake.HandoffComplete{Generation: serving, Issue: grant.Issue, Role: grant.Role, Claim: grant.Claim, Task: position.ofTask,
		Summary: req.Summary, Verdict: req.Verdict, Commit: req.Commit,
		HandoffRecord: req.HandoffID, HandoffVersion: req.HandoffVersion, PlanRating: rating, MustRate: c.PluginContract >= PlanRatingContract,
		ReturnTo: returnTo, Entry: entry})
	if err != nil {
		s.writeFailure(w, r, http.StatusInternalServerError, "FACT_APPLY_FAILED", "could not apply handoff fact")
		return
	}
	if result.Duplicate {
		// The pass is the completing task's own (completionPosition), so for an issue with plan
		// rows the refusal names that task beside its phase; one with none names no task, as before.
		phaseOf := string(issue.Phase)
		if position.task != "" {
			phaseOf += " of task " + position.task
		}
		s.writeFailure(w, r, http.StatusConflict, "HANDOFF_ALREADY_RECORDED", fmt.Sprintf(
			"the %s completion of phase %s (pass %d through it) %s was already received; this call changed nothing",
			grant.Role, phaseOf, pass, received))
		return
	}
	if result.Refusal != nil {
		s.writeFailure(w, r, result.Refusal.Status, result.Refusal.Code, result.Refusal.Message)
		return
	}
	// The completion is recorded and its transaction committed, so the daemon's read of the pull
	// request of the task whose step completed judges what this phase left; the hook only queues
	// that read, which runs on the daemon's own goroutine, so this answer waits for no GitHub call.
	if s.handoffCompleted != nil {
		s.handoffCompleted(grant.Issue, position.ofTask)
	}
	s.writeAnswer(w, r, AnswerHandoffComplete, http.StatusOK, HandoffCompleteResponse{})
	return
}

// returnToRefusal holds a completion's returnTo to its rule, and is nil when the completion
// names none or names one it may. failsAStep is whether the completion fails a step (the verdict
// fail of a tester or of a role of the deployment, the roles VERDICT_ROLE_FORBIDDEN leaves a
// verdict to), and what names the completion in a refusal. Every refusal is a 400 its caller
// writes before anything is recorded, so the same completion sent again, corrected, is taken:
//
//   - INVALID_RETURN_TO: returnTo below 1, which names no step of a task.
//   - RETURN_TO_NEEDS_FAIL: a completion that fails no step names returnTo: a pass, another role's
//     completion, or a production check's report, which is no step. Dropping it unread would let
//     an agent believe it had sent the task back.
func returnToRefusal(req HandoffCompleteRequest, failsAStep bool, what string) *intake.Refusal {
	switch {
	case req.ReturnTo == nil:
		return nil
	case *req.ReturnTo < 1:
		return &intake.Refusal{Status: http.StatusBadRequest, Code: "INVALID_RETURN_TO", Message: fmt.Sprintf(
			"returnTo is %d, and it names a step of the task by its place, 1 for its first; nothing was recorded", *req.ReturnTo)}
	case !failsAStep:
		return &intake.Refusal{Status: http.StatusBadRequest, Code: "RETURN_TO_NEEDS_FAIL", Message: fmt.Sprintf(
			"%s names returnTo, and only the completion of a step that failed, with the verdict fail, names the step its task returns to: send it again without returnTo; nothing was recorded", what)}
	}
	return nil
}

// handoffRecord reads the Dispatch handoff record a completion of issue's file-backed phase of,
// whose handoff word is word, names, and holds it to the completion: the record must be the
// issue's, of the phase's word, at the version reported, and written by session, the session the
// claim registered. Every refusal is written before the fact is applied, so nothing is recorded
// and the corrected or retried call is applied:
//
//   - HANDOFF_RECORD_REQUIRED: the completion names no record (handoffId) or no positive version.
//   - HANDOFF_RECORD_NOT_FOUND: Dispatch has no such record (its 404, or another refusal of the id).
//   - DISPATCH_READ_FAILED: Dispatch did not answer the read, or answered it with a refusal that
//     says nothing of the id (a 5xx, a credential answer; dispatch.PermanentRefusal); the worker
//     completes again once Dispatch answers.
//   - HANDOFF_RECORD_MISMATCH: the record is another issue's, another phase's, or another version.
//   - HANDOFF_SESSION_MISMATCH: another session wrote it, or no session did. A record carried
//     from before handoffs moved to the Dispatch issue is under the daemon's own session
//     (dispatch.DaemonSession), so it is refused this way too, and the refusal says to read it
//     and write it again from the worker's session.
//
// The claim holds session as the store keeps it (supervise's register), so the record Dispatch
// answers is compared as the store would keep it (storetext.Strings).
func (s *server) handoffRecord(w http.ResponseWriter, r *http.Request, session string, issue *record.Issue, of phase.Phase, word string, req HandoffCompleteRequest) bool {
	write := fmt.Sprintf("`dispatch handoff-write --issue %s --phase %s --body-file <file>`", issue.Key, word)
	read := fmt.Sprintf("`dispatch handoff-read --issue %s --phase %s`", issue.Key, word)
	if req.HandoffID == "" || req.HandoffVersion < 1 {
		s.writeFailure(w, r, http.StatusBadRequest, "HANDOFF_RECORD_REQUIRED", fmt.Sprintf(
			"phase %s of %s ends with its %s handoff recorded on the Dispatch issue, and this completion names no record (handoffId and a positive handoffVersion): write it with %s, then complete again; this completion changed nothing",
			of, issue.Key, word, write))
		return false
	}
	if s.dispatch == nil {
		s.writeFailure(w, r, http.StatusInternalServerError, "DISPATCH_UNAVAILABLE", "Dispatch is unavailable")
		return false
	}
	handoff, err := s.dispatch.Handoff(r.Context(), req.HandoffID)
	if err != nil {
		if _, permanent := dispatch.PermanentRefusal(err); permanent || dispatch.Missing(err) {
			s.writeFailure(w, r, http.StatusNotFound, "HANDOFF_RECORD_NOT_FOUND", fmt.Sprintf(
				"Dispatch has no handoff record %s (%v): name the record %s shows, or write one with %s, then complete again; this completion changed nothing",
				req.HandoffID, err, read, write))
			return false
		}
		s.log.Warn("api: read the handoff record a completion names", "issue", issue.Key, "handoff", req.HandoffID, "error", err)
		s.writeFailure(w, r, http.StatusBadGateway, "DISPATCH_READ_FAILED", fmt.Sprintf(
			"Dispatch did not answer the read of handoff record %s (%v): complete again once Dispatch answers; this completion changed nothing",
			req.HandoffID, err))
		return false
	}
	handoff = storetext.Strings(handoff)
	if handoff.IssueKey != issue.Key || handoff.Phase != word || handoff.Version != req.HandoffVersion {
		s.writeFailure(w, r, http.StatusConflict, "HANDOFF_RECORD_MISMATCH", fmt.Sprintf(
			"handoff record %s is version %d of %s's %s handoff, and this completion is of %s's %s handoff at version %d: name the record %s shows; this completion changed nothing",
			req.HandoffID, handoff.Version, handoff.IssueKey, handoff.Phase, issue.Key, word, req.HandoffVersion, read))
		return false
	}
	if handoff.Author.IsSession(session) {
		return true
	}
	if handoff.Author.IsSession(dispatch.DaemonSession(issue.Key)) {
		s.writeFailure(w, r, http.StatusForbidden, "HANDOFF_SESSION_MISMATCH", fmt.Sprintf(
			"handoff record %s (%s version %d) was written by the daemon's own session %s, as a record carried from before handoffs moved to the Dispatch issue is, not by this claim's session %s: read it with %s, write it again with %s from this session, then complete naming the new record; this completion changed nothing",
			req.HandoffID, word, handoff.Version, handoff.Author.ID, session, read, write))
		return false
	}
	s.writeFailure(w, r, http.StatusForbidden, "HANDOFF_SESSION_MISMATCH", fmt.Sprintf(
		"handoff record %s (%s version %d) was written by %s %s, not by this claim's session %s: write the handoff again with %s from this session, then complete naming the new record; this completion changed nothing",
		req.HandoffID, word, handoff.Version, handoff.Author.Kind, handoff.Author.ID, session, write))
	return false
}

// heldTaskNotCurrent says why the task a claim is holding is not the issue's current task — it is
// of another run, of another phase, or there is no task at all — and answers "" when it is that
// task: one of the issue's own generation and its own phase. The one rule has two readers, and
// this is the one place it is written. The completion route's fallback to the held task's run and
// entry applies exactly on the empty answer (answerCompletion), and the HANDOFF_NO_RUN
// refusal is written from the answer itself (noRunCompletion), so a change to which held task may
// back a completion cannot leave the 409 describing a rule the route no longer applies.
//
// An empty answer also says the claim holds a task at all, which is what lets the route read
// Pending on the strength of it: the first case asks supervise.HoldsTask, so every case after it
// reads a task. A claim holds none when it holds nothing and when it holds only the daemon's own
// delivery of a relaunch's record (supervise.Delivery.Relaunch), which carries the sentence of
// what the relaunch ended and no work; the reason says which, so that delivery is said as what it
// is rather than read as a task of no run, which is an operator's own.
func heldTaskNotCurrent(issue string, current uint64, p phase.Phase, pending *supervise.Delivery) string {
	switch {
	case !supervise.HoldsTask(pending):
		if pending == nil {
			return fmt.Sprintf("this claim holds no task at all, and %s is on generation %d in %s", issue, current, p)
		}
		return fmt.Sprintf("this claim holds no task, only the daemon's own delivery of what its relaunch ended (%s), and %s is on generation %d in %s",
			pending.ID, issue, current, p)
	case pending.Generation != current:
		run := fmt.Sprintf("generation %d's", pending.Generation)
		if pending.Generation == 0 {
			run = "no run's (an operator's own task carries no generation)"
		}
		return fmt.Sprintf("the task this claim holds (%s) is %s, and %s is on generation %d", pending.ID, run, issue, current)
	case pending.Phase != p:
		of := "names no phase"
		if pending.Phase != "" {
			of = fmt.Sprintf("is the %s phase's", pending.Phase)
		}
		return fmt.Sprintf("the task this claim holds (%s) %s, and %s is in %s", pending.ID, of, issue, p)
	}
	return ""
}

// noRunCompletion is the refusal of a completion the route can attribute to no run: the claim has
// retired none, and held is heldTaskNotCurrent's answer for the task it is holding — the very
// reason the fallback that would have taken the run and the entry from that task does not
// apply. It says what the claim holds, that nothing was recorded, and what the worker does next,
// which is what skills/legion-worker/SKILL.md tells it for this code: push nothing further, end
// the turn without completing, tell the architect, and call the same completion in the turn the
// issue's own task arrives in rather than redoing the work.
func noRunCompletion(issue string, current uint64, p phase.Phase, held string) string {
	return fmt.Sprintf("this completion belongs to no run of %s: %s; nothing was recorded. Push nothing further, end this turn without completing and tell the architect; if the daemon queues this claim a generation-%d %s task (none may: this run's phase can be another claim's) and its work and handoff are already done, call the same handoff_complete in that task's turn, and never redo the work or write a second handoff",
		issue, held, current, p)
}

// staleCompletion is the refusal of a completion attributed to a run the issue has left. It names
// the current run's task when the claim holds one, which the worker's turn began before: that task
// is sent as the worker's next prompt, and the same completion in its turn is applied.
func staleCompletion(issue string, current, serving uint64, pending *supervise.Delivery) string {
	stale := fmt.Sprintf("%s is on generation %d and this completion is generation %d's; nothing was recorded", issue, current, serving)
	if pending != nil && pending.Generation == current {
		return fmt.Sprintf("%s: your turn began before the daemon's generation-%d task (%s) reached this session. End this turn without completing; that task arrives as your next prompt, and the same handoff_complete in its turn is applied",
			stale, current, pending.ID)
	}
	return fmt.Sprintf("%s: the daemon has queued no task of generation %d for this claim, and may queue none (the new run's phase may be another claim's). End this turn without completing and tell the architect; if that task arrives, call the same handoff_complete in its turn", stale, current)
}

// implementationPullRequest is the implementer's completion's read of GitHub: in implementing, a
// completion moves its issue to its task's next step (testing, in the default steps) only on a
// pull request recorded open or merged for it (workflow's TriggerImplementationReady), and only
// the opened webhook, or the daemon's per-issue read standing in for one that webhook lost
// (readIssuePullRequest), records one open. So before the completion is applied, the open pull
// request on the branch of the completion's task (position.branch: the task's own,
// record.PlanTask.BranchOf, and the issue's, claim.IssueBranch, for an issue with no plan rows,
// completionPosition), is read as the implement App, minted for the owner of the issue's
// repository in GitHub's spelling (record.Issue.GitHubRepo, which the webhook's facts name it
// by), and it says whether the completion goes on:
//
//   - one open against the base the plan gives the task (position.base, record.StoredPlan.BaseOf),
//     or against any base where none is read (an issue with no plan rows): when the record does
//     not already hold it open, the intake.PullRequestOpened its webhook would have carried is
//     applied first, a reopen when the record holds the same pull request closed, under the
//     read's stable id (intake.OpenedRead). The completion then moves the issue to its task's
//     next step. A failure to apply it is FACT_APPLY_FAILED, with the completion not applied.
//   - one open against another base: HANDOFF_PULL_REQUEST_BASE, 409, naming the task's base and
//     the command that sets it, and nothing is applied. Legion merges no task's work into a base
//     the plan does not give, and the review of the task reads its pull request, which against
//     another base shows work that is not the task's.
//   - none, with the record holding the pull request merged: the completion is applied and moves
//     the issue to its task's next step. The work landed, its merge told the architect
//     (pr-merged), and the workflow runs on.
//   - none otherwise: HANDOFF_NO_PULL_REQUEST, 409, and nothing is applied, so the completion at
//     the same commit is applied once the pull request is open. When the record holds the pull
//     request closed without merging, the refusal names it: the close told the architect, which
//     decides the phase.
//   - unreadable (a repository the record cannot name, a failed token mint or a failed read,
//     GitHub's rate limit among them): logged WARN, and the completion is applied as it was before
//     this read, so GitHub's trouble never blocks it; the per-issue read records the pull request
//     when it finds one.
//
// A completion the workflow refuses whatever GitHub shows reads nothing: the route calls this only
// for one that is not already applied (eventID, its key, read with intake.Applied), whose tree does
// not linger and whose handoff record is not the one the implementer named for its previous phase
// (workflow's HANDOFF_NOT_NEW). It returns false when it wrote the answer.
func (s *server) implementationPullRequest(w http.ResponseWriter, r *http.Request, position completionPosition, eventID string) bool {
	issue, branch, recorded := position.issue, position.branch, position.pullRequest
	ctx := r.Context()
	applied, err := intake.Applied(ctx, s.pool, "api", []string{eventID})
	if err != nil {
		s.logRequestFailure(r, err, "api: read whether a completion is already applied", "issue", issue.Key)
		s.writeFailure(w, r, http.StatusInternalServerError, "RECORD_UNAVAILABLE", "could not read whether this completion was already received")
		return false
	}
	if applied[eventID] {
		return true
	}
	if s.tokens == nil {
		s.writeFailure(w, r, http.StatusInternalServerError, "GITHUB_TOKEN_SOURCE_UNAVAILABLE", "GitHub token source is unavailable")
		return false
	}
	logUnread := func(err error) {
		attributes := []any{"issue", issue.Key, "repo", issue.GitHubRepo, "branch", branch, "error", err}
		var answer *githubrest.Answer
		if errors.As(err, &answer) {
			attributes = append(attributes, "status", answer.Status)
		}
		s.log.Warn("api: read the open pull request on a completing implementer's branch; the completion is applied unread, and the per-issue read records the pull request it finds", attributes...)
	}
	recordedRepo, err := ghrepo.Parse("the GitHub repository of "+issue.Key, issue.GitHubRepo)
	if err != nil {
		logUnread(err)
		return true
	}
	// This read is GitHub's own, so it is minted for and addressed at the repository GitHub holds
	// the recorded one under now (ghrepo.Renames.Serving, whose doc says why both halves are
	// taken): the token is the half the old owner cannot supply, since after a transfer the
	// installation covering the repository is the owner it moved to's. The log line above and the
	// fact applied below go on naming issue.GitHubRepo, the name the record is held under.
	repository := s.serving(recordedRepo)
	lease, err := s.tokens.Token(ctx, appauth.Implement, repository.Owner())
	if err != nil {
		logUnread(fmt.Errorf("mint the implement App token for %s: %w", repository, err))
		return true
	}
	github := githubrest.Client{Token: lease.Token, API: githubrest.RepositoryAPI(s.githubAPI, repository)}
	// The owner this asks under is the resolved repository's, and it has to be: the read is the
	// `head=<owner>:<branch>` query, and GitHub translates no part of it across a transfer, so a
	// query naming the owner a record was written under answers an empty list on the repository
	// that moved rather than the pull request on it. That empty list is the production refusal
	// this rule was changed for: every implementer handoff of a tree opened before the transfer
	// was refused HANDOFF_NO_PULL_REQUEST below.
	pull, found, err := github.OpenPullRequestOn(ctx, repository, branch)
	if err != nil {
		logUnread(err)
		return true
	}
	if !found {
		// This read is GitHub's own answer on an open pull request, so of the states that let the
		// implementation move on (record.LetsImplementationMove) only merged still does here: a
		// record holding the pull request open is one GitHub closed or merged since the daemon last
		// heard, and the completion waits for whichever of the two the daemon records next.
		if record.LetsImplementationMove(recorded) && recorded.State != record.PullRequestOpen {
			return true
		}
		message := fmt.Sprintf("no open pull request on %s %s; nothing was recorded. Open the pull request and complete again.", issue.GitHubRepo, branch)
		if recorded != nil && recorded.State == record.PullRequestClosed {
			message += fmt.Sprintf(" pull request #%d is closed; your architect was told and decides this issue's phase.", recorded.Number)
		}
		s.writeFailure(w, r, http.StatusConflict, "HANDOFF_NO_PULL_REQUEST", message)
		return false
	}
	if position.base != "" && pull.Base.Ref != position.base {
		s.writeFailure(w, r, http.StatusConflict, "HANDOFF_PULL_REQUEST_BASE", fmt.Sprintf(
			"pull request #%d on %s is against %s, and the base of task %s is %s: set it with gh pr edit %d --base %s, then complete again.",
			pull.Number, branch, pull.Base.Ref, position.ofTask, position.base, pull.Number, position.base))
		return false
	}
	if recorded != nil && recorded.State == record.PullRequestOpen && recorded.Number == pull.Number {
		return true
	}
	fact, read := intake.OpenedRead(issue.Project, issue.GitHubRepo, branch, pull, issue.Generation, recorded)
	if _, err := s.apply(ctx, "github", read, fact); err != nil {
		s.logRequestFailure(r, err, "api: apply the open pull request a completing implementer's branch shows", "issue", issue.Key, "pullRequest", pull.Number)
		s.writeFailure(w, r, http.StatusInternalServerError, "FACT_APPLY_FAILED", "could not apply the open pull request read from GitHub; this completion changed nothing")
		return false
	}
	return true
}

// checkReport records the checking role's report on look, the look of its closed issue's
// production check the caller's own live assignment is (supervise.Claim.ServingCheck, which the
// route read before it sent the call here). Every refusal below is written before the fact is
// applied — the apply marks the event processed before any handler runs (intake.ApplyFact) — so a
// refused report changes nothing, leaves the look open, and the report sent again, corrected, is
// recorded:
//
//   - CHECK_NOT_OPEN: the check of the caller's look is not open (closed by a report, its last
//     look or a later run's merge), or the issue has none (nothing merged). The check is read by
//     the moment it opened, which the claim's look names (supervise.CheckLook.OpenedAt, written by
//     workflow's startLook), so a report is held to its own check whatever other checks of the
//     issue are open.
//   - CHECK_ROLE_MISMATCH: the grant is not the check's own role (record.ProductionCheck.Role,
//     the role the check opened with). The API is the only layer that holds the caller's role —
//     intake.CheckReport carries a claim token, not a role — so this is the one place that can
//     hold a report to the role the check records. What it guards is one path: a start row
//     queued before start rows carried the check instance (record.SuperviseRequest.CheckOpenedAt),
//     whose look names none, so the issue's newest check is read for it
//     (record.Store.ProductionCheckOf). That row's look is an earlier check's, worked by the role
//     that check opened with, and the newest — a later merge's, under a project whose
//     `production_check` names another role — records the other. Every look the running daemon
//     starts is the check's own role's (startLook starts check.Role) and names its instance.
//   - CHECK_VERDICT_REQUIRED: the verdict is not one of pass, pending or fail.
//   - CHECK_WAKE_REQUIRED: a pending on a look that is not the check's last (Last) names no
//     wakeAt. The checking agent names its own next look, and nothing else would start one.
//   - CHECK_WAKE_UNEXPECTED: a wakeAt where no look follows it — on a pass or a fail, each of
//     which closes the check, or on a pending on the check's last look, after which the check
//     closes as not run.
//   - PLAN_RATING_UNEXPECTED: a planRating or a planReason, whatever its form, which only the
//     completion that ends the retro carries (intake.PlanRatingUnexpected).
//
// No commit is read: the look pushed nothing. No run is read either: the check fences itself on
// the issue and the look, since the look's task is no phase's run and a woken session may hold an
// older one. The look sent is the caller's own, not whichever look the check has reached, so a
// report from a turn that outlived its look is held to the look it was given and the engine
// refuses it (CHECK_LOOK_NOT_OPEN) rather than recording it as the current look's. The event is
// the issue, the check (record.ProductionCheck.ID: each check of an issue counts its looks from
// one, so a report on one check's look 1 is another fact than one on another's), the look and
// the verdict: a retried report of the same look is the same
// fact, and one the engine refuses — the look ended by its bound before the report landed — is
// left unmarked (intake's ApplyFact), so the checking role's next send of it applies, and the
// report of the next look is a new fact.
func (s *server) checkReport(w http.ResponseWriter, r *http.Request, grant credential.Grant, check *record.ProductionCheck, look supervise.CheckLook, req HandoffCompleteRequest) {
	if check == nil || !check.Open() {
		s.writeFailure(w, r, http.StatusConflict, "CHECK_NOT_OPEN", fmt.Sprintf("%s has no open production check; this completion changed nothing", grant.Issue))
		return
	}
	if grant.Role != check.Role {
		s.writeFailure(w, r, http.StatusConflict, "CHECK_ROLE_MISMATCH", fmt.Sprintf(
			"%s's production check is the %s's, and this completion is the %s's: only the role the check records reports on it; the record is unchanged, so the check's own role may send this report",
			grant.Issue, check.Role, grant.Role))
		return
	}
	if req.Verdict != "pass" && req.Verdict != "pending" && req.Verdict != "fail" {
		s.writeFailure(w, r, http.StatusBadRequest, "CHECK_VERDICT_REQUIRED", "a production check report names a verdict of pass, pending or fail")
		return
	}
	if req.Verdict == "pending" && req.WakeAt == nil && !check.Last {
		s.writeFailure(w, r, http.StatusBadRequest, "CHECK_WAKE_REQUIRED", fmt.Sprintf(
			"a pending report names wakeAt, when the next look of %s's production check is due; look %d stays open and nothing was recorded, so send this report again with a time",
			grant.Issue, look.Look))
		return
	}
	if req.WakeAt != nil && (req.Verdict != "pending" || check.Last) {
		why := fmt.Sprintf("a %s report closes the check", req.Verdict)
		if req.Verdict == "pending" {
			why = fmt.Sprintf("look %d is the check's last, after which it closes as not run", look.Look)
		}
		s.writeFailure(w, r, http.StatusBadRequest, "CHECK_WAKE_UNEXPECTED", fmt.Sprintf(
			"this report names wakeAt and no look follows it: %s; send it again without a time, and nothing was recorded", why))
		return
	}
	what := fmt.Sprintf("this report on look %d of %s's production check", look.Look, grant.Issue)
	if req.PlanRating != nil || req.PlanReason != nil {
		refusal := intake.PlanRatingUnexpected(what)
		s.writeFailure(w, r, refusal.Status, refusal.Code, refusal.Message)
		return
	}
	// A look is no step of a task, so its report fails none and names no step to return to.
	if refusal := returnToRefusal(req, false, what); refusal != nil {
		s.writeFailure(w, r, refusal.Status, refusal.Code, refusal.Message)
		return
	}
	eventID := fmt.Sprintf("check:%s:%s:%d:%s", grant.Issue, check.ID(), look.Look, req.Verdict)
	result, err := s.apply(r.Context(), "api", eventID, intake.CheckReport{Issue: grant.Issue, Claim: grant.Claim, Look: look.Look,
		Verdict: req.Verdict, Summary: req.Summary, WakeAt: req.WakeAt, OpenedAt: check.OpenedAt})
	if err != nil {
		s.writeFailure(w, r, http.StatusInternalServerError, "FACT_APPLY_FAILED", "could not apply handoff fact")
		return
	}
	if result.Duplicate {
		s.writeFailure(w, r, http.StatusConflict, "HANDOFF_ALREADY_RECORDED", fmt.Sprintf(
			"the %s report on look %d of %s's production check was already received; this call changed nothing", req.Verdict, look.Look, grant.Issue))
		return
	}
	if result.Refusal != nil {
		s.writeFailure(w, r, result.Refusal.Status, result.Refusal.Code, result.Refusal.Message)
		return
	}
	s.writeAnswer(w, r, AnswerHandoffComplete, http.StatusOK, HandoffCompleteResponse{})
}
