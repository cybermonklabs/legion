package workflow

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// MinWaitAhead and MaxWaitAhead are the bounds a declared wait's time must fall within, measured
// from its declaration: less than a minute ahead is too soon, and more than a day ahead too far
// (dispatch://INFRA-2040 R2, D10). They are the daemon's own, the same on every deployment, so a
// worker with a wrong wait, or a message whose wake is lost, goes unseen for a day at most.
const (
	MinWaitAhead = time.Minute
	MaxWaitAhead = 24 * time.Hour
)

// The causes WaitEndCause names, one for each term of WaitStands a wait with no end can fail, and
// the one each such end is written with (record.ClaimWait.EndCause). A phase that moved names
// where it moved to (WaitEndPhaseMoved, WaitEndEntryMoved).
const (
	// WaitEndCompleted: the role's phases row records its completion of the phase.
	WaitEndCompleted = "completed"
	// WaitEndAnotherRolesPhase: the phase the issue stands in is not the wait's role's to work.
	WaitEndAnotherRolesPhase = "another role's phase"
	// WaitEndLeftWorkflow: the issue is out of the workflow (record.InWorkflow).
	WaitEndLeftWorkflow = "left the workflow"
	// WaitEndTreeLingers: the issue's tree root lingers after its close.
	WaitEndTreeLingers = "tree lingers"
	// WaitEndNewGeneration: the issue runs a generation the wait was not declared in.
	WaitEndNewGeneration = "new generation"
	// WaitEndFreshSession: the claim runs a session the wait was not declared in.
	WaitEndFreshSession = "fresh session"
	// WaitEndDesignGateClosed: the wait is a merger's, and the design gate holds it
	// (HeldByDesignGate).
	WaitEndDesignGateClosed = "design gate closed"
)

// WaitEndPhaseMoved is the cause of a wait whose issue moved from the wait's phase to another,
// naming the phase it moved to: a completion that moved it, a backward move, a move to held.
func WaitEndPhaseMoved(to phase.Phase) string { return "phase moved to " + string(to) }

// WaitEndEntryMoved is the cause of a wait whose issue stands in the wait's phase on a new entry
// of the role's phases row (record.PhaseRow.Entry), naming that entry: the phase was started
// again for the role, which hands it work it had not taken when it declared.
func WaitEndEntryMoved(entry int64) string { return fmt.Sprintf("phase entry moved to %d", entry) }

// WaitStands is whether the claim's wait stands now, and that wait when it does (the zero wait
// when it does not). It is the one rule every reader of a wait asks, so that what a wait does
// never depends on its end being written: a wait whose phase moved, whose session was replaced or
// whose merger the design gate holds stops counting the moment the fact that did it commits, as
// a task stops being worked the moment its phase moves (supervise.Delivery.StillWorked). The end
// such a wait is given is written with the cause WaitEndCause names (record.Store.EndWait), and
// this rule does not wait for it.
//
// issue is the claim's issue as recorded now, root its tree root, row the wait's role's phases
// row (record.PhaseRowOf), run the claim's run, of which only the claim is read (its session and
// its wait with no end, record.RoleClaim's Session and Wait), and gate the tree's design gate
// with the steps the deployment gates. It is true only while every one of these holds, in the
// order WaitEndCause asks them:
//   - the claim has a wait row with no end;
//   - the role's phases row records no completion (record.PhaseRow.CompletedAt);
//   - the issue stands in the wait's phase;
//   - that phase is the wait's role's to work (OpenPhaseRole);
//   - the issue is in the workflow (record.InWorkflow), and its tree root does not linger;
//   - the issue runs the generation the wait was declared in, and the role's phases row is on
//     the entry it was on at the declaration;
//   - the claim runs the session the wait was declared in, since a fresh session knows nothing
//     of the wait;
//   - the wait is not a merger's the design gate holds (HeldByDesignGate).
func WaitStands(issue, root record.Issue, row record.PhaseRow, run record.RoleRun, gate record.TreeGate) (record.ClaimWait, bool) {
	wait := unendedWait(run)
	if wait == nil || WaitEndCause(issue, root, row, run, gate) != "" {
		return record.ClaimWait{}, false
	}
	return *wait, true
}

// WaitEndCause is the cause that ends the claim's wait now: the first term of WaitStands that
// fails, asked in one fixed order — completed, the phase moved (and to what), another role's
// phase, left the workflow, the tree lingers, a new generation, a new entry of the same phase, a
// fresh session, and last the design gate holding a merger — and "" while the wait stands. A claim
// with no wait row that has no end has no wait to end, and answers "" too: WaitStands is the
// question of whether a wait stands, and this one is asked of a wait known to have no end.
//
// The order names what the worker did before what was done around it, and the phase before the
// tree: a completion that also moved the issue is a completion, a move into done is a move of the
// phase, and a re-admission that also moved the phase is the phase's move. The cause is what such
// a wait's end is written with (record.Store.EndWait), so one fact ends one wait with one cause
// whichever of its terms it broke.
func WaitEndCause(issue, root record.Issue, row record.PhaseRow, run record.RoleRun, gate record.TreeGate) string {
	wait := unendedWait(run)
	if wait == nil {
		return ""
	}
	switch working := OpenPhaseRole(issue.Phase); {
	case !row.CompletedAt.IsZero():
		return WaitEndCompleted
	case issue.Phase != wait.Phase:
		return WaitEndPhaseMoved(issue.Phase)
	case working == "" || working != wait.Role:
		return WaitEndAnotherRolesPhase
	case !record.InWorkflow(issue):
		return WaitEndLeftWorkflow
	case root.Lingers():
		return WaitEndTreeLingers
	case issue.Generation != wait.Generation:
		return WaitEndNewGeneration
	case row.Entry != wait.PhaseEntry:
		return WaitEndEntryMoved(row.Entry)
	case run.Claim.Session != wait.Session:
		return WaitEndFreshSession
	}
	if _, held := HeldByDesignGate(issue, wait.Role, gate); held {
		return WaitEndDesignGateClosed
	}
	return ""
}

// unendedWait is the wait row of run's claim that has no end, nil for a run with no claim and a
// claim with none.
func unendedWait(run record.RoleRun) *record.ClaimWait {
	if run.Claim == nil || run.Claim.Wait == nil || !run.Claim.Wait.EndedAt.IsZero() {
		return nil
	}
	return run.Claim.Wait
}

// waitDeclared applies one intake.WaitDeclared, a phase worker's declaration of a wait, deciding
// on the record read in the fact's own transaction. That transaction holds the lock every fact
// applies under (intake.ApplyFact), the design gate's closing among them, so a declaration and a
// closing made at once are applied one after the other, each on what the other wrote: a
// declaration applied after the closing is refused, and a wait recorded before it stops standing
// the moment the closing commits (WaitStands).
//
// It refuses, recording nothing (D10 of dispatch://INFRA-2040's plan):
//   - WAIT_TEXT_REQUIRED (400): what the wait waits on is empty once trimmed;
//   - WAIT_TOO_SOON and WAIT_TOO_FAR (400): its time is less than MinWaitAhead, or more than
//     MaxWaitAhead, after the declaration (fact.At);
//   - WAIT_NOT_OPEN_PHASE (409): the claim does not work its issue's open phase — the record
//     holds no such issue, root or claim, the role's phases row records its completion, the phase
//     is another role's or no phase worker's (a look of a done issue's production check is none),
//     the issue is out of the workflow, its tree lingers, the claim's worker is on another run
//     than the issue's, or the claim runs another session now;
//   - WAIT_HELD_BY_DESIGN_GATE (409): the claim is a merger the design gate holds
//     (HeldByDesignGate), named with the gated step, its document and its version.
//
// The record's refusals are the terms of WaitStands asked of the wait as it would stand once
// recorded (WaitEndCause), so the one rule that says whether a wait stands also says whether one
// may be declared, and no wait is recorded that would not stand at once. Otherwise the wait is
// recorded (record.Store.DeclareWait), ending the claim's standing wait as replaced. Every line
// is logged once the fact commits, so a fact rolled back logs no declaration.
func (e *Engine) waitDeclared(ctx context.Context, tx pgx.Tx, fact intake.WaitDeclared) (intake.Result, error) {
	if refusal := declarationRefusal(fact); refusal != nil {
		return e.refuseWait(ctx, fact, refusal), nil
	}
	issue, err := e.store.Issue(ctx, tx, fact.Issue)
	if err != nil {
		return intake.Result{}, err
	}
	if issue == nil {
		return e.refuseWait(ctx, fact, notOpenPhase(fmt.Sprintf("the workflow records no issue %s", fact.Issue))), nil
	}
	root := issue
	if issue.Tree != issue.Key {
		if root, err = e.store.Issue(ctx, tx, issue.Tree); err != nil {
			return intake.Result{}, err
		}
		if root == nil {
			return e.refuseWait(ctx, fact, notOpenPhase(fmt.Sprintf("the workflow records no root %s of the tree of %s", issue.Tree, issue.Key))), nil
		}
	}
	rows, err := e.store.Phases(ctx, tx, issue.Key)
	if err != nil {
		return intake.Result{}, err
	}
	gates, err := e.store.GatesOf(ctx, tx, issue.Tree)
	if err != nil {
		return intake.Result{}, err
	}
	claims, err := e.store.RoleClaims(ctx, tx, []claim.Token{fact.Token})
	if err != nil {
		return intake.Result{}, err
	}
	held := claims[fact.Token]
	if held == nil {
		return e.refuseWait(ctx, fact, notOpenPhase(fmt.Sprintf("the daemon records no claim %s", fact.Token))), nil
	}
	project, err := claim.ProjectToken(e.cfg.Project)
	if err != nil {
		return intake.Result{}, err
	}
	row, gate := record.PhaseRowOf(rows, fact.Role), record.TreeGate{Steps: e.cfg.Steps, Rows: gates}
	// The wait as it would be recorded. Its generation is the run the claim's worker is on, which
	// WaitEndCause holds to the issue's own, so a wait that is recorded carries the issue's.
	wait := record.ClaimWait{ID: fact.WaitID, Project: project, Token: fact.Token, Issue: issue.Key, Role: fact.Role,
		Generation: fact.Generation, Phase: issue.Phase, PhaseEntry: row.Entry, TakenEntry: fact.TakenEntry,
		Session: fact.Session, WaitingOn: fact.WaitingOn, WakeAt: fact.WakeAt, DeclaredAt: fact.At}
	declaring := *held
	declaring.Wait = &wait
	if cause := WaitEndCause(*issue, *root, row, record.RoleRun{Claim: &declaring}, gate); cause != "" {
		return e.refuseWait(ctx, fact, recordRefusal(cause, *issue, gate, fact, held.Session)), nil
	}
	replaced, err := e.store.DeclareWait(ctx, tx, wait)
	if err != nil {
		return intake.Result{}, err
	}
	if replaced != nil {
		e.logOnCommit(ctx, "wait: ended", "claim", fact.Token, "issue", issue.Key, "role", fact.Role, "wait", replaced.ID,
			"cause", record.WaitReplaced, "ended_by", wait.ID)
	}
	e.logOnCommit(ctx, "wait: declared", "claim", fact.Token, "issue", issue.Key, "role", fact.Role, "wait", wait.ID,
		"wake_at", wait.WakeAt.UTC().Format(time.RFC3339), "taken_entry", wait.TakenEntry)
	return intake.Result{}, nil
}

// refuseWait is fact's refusal, logged with its code once the fact commits. Every refusal comes
// before the declaration's one write, so nothing else is written.
func (e *Engine) refuseWait(ctx context.Context, fact intake.WaitDeclared, refusal *intake.Refusal) intake.Result {
	e.logOnCommit(ctx, "wait: refused", "claim", fact.Token, "issue", fact.Issue, "role", fact.Role, "wait", fact.WaitID,
		"code", refusal.Code, "reason", refusal.Message)
	return intake.Result{Refusal: refusal}
}

// declarationRefusal is the refusal a declaration earns on its own, before the record is read:
// nothing it waits on, or a time outside the bounds measured from the declaration; nil for none.
// The bounds are inclusive: a wait MinWaitAhead or MaxWaitAhead ahead is recorded.
func declarationRefusal(fact intake.WaitDeclared) *intake.Refusal {
	switch ahead := fact.WakeAt.Sub(fact.At); {
	case strings.TrimSpace(fact.WaitingOn) == "":
		return &intake.Refusal{Status: 400, Code: "WAIT_TEXT_REQUIRED", Message: "waitingOn is empty: say what the wait " +
			"waits on, such as the answer, the run or the person it is for; this declaration recorded nothing"}
	case ahead < MinWaitAhead:
		return &intake.Refusal{Status: 400, Code: "WAIT_TOO_SOON", Message: fmt.Sprintf(
			"wakeAt %s is less than %s after this declaration at %s: name a time at least %s ahead; this declaration recorded nothing",
			fact.WakeAt.UTC().Format(time.RFC3339), aheadWords(MinWaitAhead), fact.At.UTC().Format(time.RFC3339), aheadWords(MinWaitAhead))}
	case ahead > MaxWaitAhead:
		return &intake.Refusal{Status: 400, Code: "WAIT_TOO_FAR", Message: fmt.Sprintf(
			"wakeAt %s is more than %s after this declaration at %s: name a time at most %s ahead, and declare the wait again "+
				"then if it still stands; this declaration recorded nothing",
			fact.WakeAt.UTC().Format(time.RFC3339), aheadWords(MaxWaitAhead), fact.At.UTC().Format(time.RFC3339), aheadWords(MaxWaitAhead))}
	}
	return nil
}

// aheadWords is a bound of a wait's time in words: whole hours in hours, anything else in
// minutes.
func aheadWords(d time.Duration) string {
	n, unit := int64(d/time.Minute), "minute"
	if d%time.Hour == 0 {
		n, unit = int64(d/time.Hour), "hour"
	}
	if n != 1 {
		unit += "s"
	}
	return fmt.Sprintf("%d %s", n, unit)
}

// recordRefusal is the refusal a declaration earns from the record: cause is the term of
// WaitStands the wait would fail the moment it was recorded (WaitEndCause), named here in the
// record's own terms. A merger the design gate holds is WAIT_HELD_BY_DESIGN_GATE, naming the gate
// row that holds it; every other term is WAIT_NOT_OPEN_PHASE. session is the session the record
// says the claim runs now.
func recordRefusal(cause string, issue record.Issue, gate record.TreeGate, fact intake.WaitDeclared, session string) *intake.Refusal {
	working := OpenPhaseRole(issue.Phase)
	switch {
	case cause == WaitEndDesignGateClosed:
		closed, _ := HeldByDesignGate(issue, fact.Role, gate)
		return &intake.Refusal{Status: 409, Code: "WAIT_HELD_BY_DESIGN_GATE", Message: fmt.Sprintf(
			"the design gate of %s holds this merger: version %d of its %s document (%s) is with its reviewer, and %s merges "+
				"nothing until every gated step stands open again. The daemon tells this merger when the gate opens, so the hold "+
				"needs no wait of its own; this declaration recorded nothing",
			issue.Tree, closed.LatestVersion, closed.Step.OrSpec(), closed.ArtifactID, issue.Key)}
	case cause == WaitEndCompleted:
		return notOpenPhase(fmt.Sprintf("the %s's phases row of %s records the completion of its phase", fact.Role, issue.Key))
	case cause == WaitEndAnotherRolesPhase && issue.Phase == phase.Done:
		return notOpenPhase(fmt.Sprintf("%s is done, so no phase of it is open, and a look of its production check is no phase", issue.Key))
	case cause == WaitEndAnotherRolesPhase && working == "":
		return notOpenPhase(fmt.Sprintf("%s stands in %s, which no phase worker works", issue.Key, issue.Phase))
	case cause == WaitEndAnotherRolesPhase:
		return notOpenPhase(fmt.Sprintf("%s stands in %s, which is the %s's phase, not the %s's", issue.Key, issue.Phase, working, fact.Role))
	case cause == WaitEndLeftWorkflow:
		return notOpenPhase(fmt.Sprintf("%s is out of the workflow (Dispatch status %s)", issue.Key, issue.Status))
	case cause == WaitEndTreeLingers:
		return notOpenPhase(fmt.Sprintf("the tree of %s lingers after its close, and nothing in it moves", issue.Tree))
	case cause == WaitEndNewGeneration && fact.Generation == 0:
		return notOpenPhase(fmt.Sprintf("this claim's worker has taken no task of run %d of %s", issue.Generation, issue.Key))
	case cause == WaitEndNewGeneration:
		return notOpenPhase(fmt.Sprintf("this claim's worker is on run %d of %s, and the issue runs %d now",
			fact.Generation, issue.Key, issue.Generation))
	case cause == WaitEndFreshSession:
		return notOpenPhase(fmt.Sprintf("the claim runs session %s now, not %s, which declared this wait", session, fact.Session))
	}
	return notOpenPhase(fmt.Sprintf("the wait would end the moment it was recorded (%s)", cause))
}

// notOpenPhase is the WAIT_NOT_OPEN_PHASE refusal, fault being what the record shows.
func notOpenPhase(fault string) *intake.Refusal {
	return &intake.Refusal{Status: 409, Code: "WAIT_NOT_OPEN_PHASE", Message: fault +
		": a wait is declared only by the worker of its issue's open phase, and this declaration recorded nothing"}
}

// WaitReconciler is the fact handler that ends, in each fact's own transaction, every wait the
// fact stopped standing (Engine.EndStaleWaits): R3's fourth and fifth ends, a claim that stops
// working its issue's open phase — a completion, a move of the phase, a move to held, the issue
// leaving the workflow, its tree's linger, a new generation — and a claim that runs a fresh
// session. No one function moves a phase, so no write of one ends a wait itself: this handler
// reads, once every other handler of the fact has written, what each wait's terms read now.
//
// Its place is last among the daemon's fact handlers (internal/daemon's factHandlers), after the
// engine and after admission, which writes inside the same fact (admit.Admission.Apply): a
// re-admission's new generation and a promotion end the waits they stop in that fact too.
type WaitReconciler struct{ engine *Engine }

var _ intake.Handler = WaitReconciler{}

// NewWaitReconciler is the handler that ends, through engine, the waits each fact stops.
func NewWaitReconciler(engine *Engine) WaitReconciler { return WaitReconciler{engine: engine} }

// Apply ends the waits the fact stopped, each ended by that fact (factEndedBy). It refuses
// nothing, whatever the fact.
func (r WaitReconciler) Apply(ctx context.Context, tx pgx.Tx, _ intake.Fact) (intake.Result, error) {
	return intake.Result{}, r.engine.EndStaleWaits(ctx, tx, factEndedBy(ctx))
}

// EndStaleWaits ends, in tx, every wait of the project's claims that a term of the record no
// longer lets stand (WaitEndCause), with that term's cause, ended by endedBy at the engine's now,
// and logs each `wait: ended` once tx commits. The waits are read in one statement
// (record.Store.StaleWaits), which keeps only those whose record moved under them, so the cost is
// one indexed query whatever the number of waits.
//
// endedBy names what ended them: the fact's source and event id inside a fact (WaitReconciler),
// or the pass that writes outside one (internal/daemon's reconcileListing, the boot's and a
// recovery's admission of a Dispatch listing).
//
// Every term is asked but the design gate's: each wait is judged under no gate
// (record.TreeGate{}), so no merger the gate holds is ended here. The gate's closing ends the
// waits of the mergers it holds in its own fact, where it closes (putGateTold,
// endHeldMergersWaits), so the end names the fact that closed the gate. A hold that began with
// no gate row written — the steps `gates.design` names, changed across a restart — has no fact
// that closed it, and is not ended under the name of whatever fact ran next: WaitStands answers
// false for that wait from the moment the hold began, so nothing waits on its end, and its time
// ends it, with the cause WaitEndCause names then (D5 of dispatch://INFRA-2040's plan).
func (e *Engine) EndStaleWaits(ctx context.Context, tx pgx.Tx, endedBy string) error {
	project, err := claim.ProjectToken(e.cfg.Project)
	if err != nil {
		return err
	}
	stale, err := e.store.StaleWaits(ctx, tx, project)
	if err != nil {
		return err
	}
	for _, reading := range stale {
		cause := WaitEndCause(reading.Issue, reading.Root, reading.Row, reading.Run(), record.TreeGate{})
		if cause == "" {
			continue
		}
		if err := e.endWait(ctx, tx, *reading.Claim.Wait, cause, endedBy); err != nil {
			return err
		}
	}
	return nil
}

// endHeldMergersWaits ends the wait of every merger of key's tree that the closing of the tree's
// standing design gate now holds, in the transaction of the fact that closed it (putGateTold):
// R3's seventh end, written with the cause WaitEndDesignGateClosed and ended by that fact. A tree
// holds a merger in merging for each member that has reached its merge (dispatch://BET-790's held
// six), and the closing ends every one's wait. gates is the tree's rows as the closing left them.
//
// Each merger's wait is judged by the one rule under the closed gate (WaitEndCause), and ended
// here only where the gate is the term that ends it. A wait an earlier term stopped first — a
// session the claim replaced since the last fact, say — is the reconciler's to end, with that
// term's cause, at this same fact's end (WaitReconciler).
func (e *Engine) endHeldMergersWaits(ctx context.Context, tx pgx.Tx, key string, gates map[record.DesignStep]record.DesignGate) error {
	gated, err := e.store.Issue(ctx, tx, key)
	if err != nil || gated == nil {
		return err
	}
	members, err := e.store.TreeIssues(ctx, tx, gated.Tree)
	if err != nil {
		return err
	}
	project, err := claim.ProjectToken(e.cfg.Project)
	if err != nil {
		return err
	}
	type merger struct {
		issue record.Issue
		token claim.Token
	}
	var root record.Issue
	var mergers []merger
	tokens := make([]claim.Token, 0, len(members))
	for _, member := range members {
		if member.Key == gated.Tree {
			root = member
		}
		if member.Phase != phase.Merging {
			continue
		}
		// A key that yields no claim token has no claim, so no wait (phaseWorkerRecord).
		token, err := claim.NewToken(project, member.Key, claim.RoleMerger)
		if err != nil {
			continue
		}
		mergers = append(mergers, merger{issue: member, token: token})
		tokens = append(tokens, token)
	}
	if len(mergers) == 0 {
		return nil
	}
	claims, err := e.store.RoleClaims(ctx, tx, tokens)
	if err != nil {
		return err
	}
	gate, endedBy := record.TreeGate{Steps: e.cfg.Steps, Rows: gates}, factEndedBy(ctx)
	for _, m := range mergers {
		held := claims[m.token]
		if held == nil || held.Wait == nil {
			continue
		}
		rows, err := e.store.Phases(ctx, tx, m.issue.Key)
		if err != nil {
			return err
		}
		if WaitEndCause(m.issue, root, record.PhaseRowOf(rows, claim.RoleMerger), record.RoleRun{Claim: held}, gate) != WaitEndDesignGateClosed {
			continue
		}
		if err := e.endWait(ctx, tx, *held.Wait, WaitEndDesignGateClosed, endedBy); err != nil {
			return err
		}
	}
	return nil
}

// endWait ends wait with cause, ended by endedBy at the engine's now (record.Store.EndWait), and
// logs `wait: ended` once the transaction commits, so a fact rolled back logs no end. A wait
// another transaction ended first is left as that one ended it, and logged by it.
func (e *Engine) endWait(ctx context.Context, tx pgx.Tx, wait record.ClaimWait, cause, endedBy string) error {
	ended, err := e.store.EndWait(ctx, tx, wait.Project, wait.Token, wait.ID, cause, endedBy, e.now())
	if err != nil || !ended {
		return err
	}
	e.logOnCommit(ctx, "wait: ended", "claim", wait.Token, "issue", wait.Issue, "role", wait.Role, "wait", wait.ID,
		"cause", cause, "ended_by", endedBy)
	return nil
}

// factEndedBy is how a wait's end names the fact whose transaction ctx scopes
// (record.ClaimWait.EndedBy): its source and event id, as ApplyFact applies it under them
// (intake.AppliedFact), and "" outside a fact, which EndWait refuses to write.
func factEndedBy(ctx context.Context) string {
	source, eventID, ok := intake.AppliedFact(ctx)
	if !ok {
		return ""
	}
	return source + "/" + eventID
}
