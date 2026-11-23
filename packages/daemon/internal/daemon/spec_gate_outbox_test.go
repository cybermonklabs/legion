package daemon

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/wait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// specGateRoot is the root every case here runs, built at run time from the project and a number.
var specGateRoot = fmt.Sprintf("%s-%d", "LEGION", 610)

// The two heads its pull request stands at in turn.
const (
	specGateHead     = "1111111111111111111111111111111111111111"
	specGateNextHead = "2222222222222222222222222222222222222222"
)

// specDispatch is Dispatch's change-request routes as a case sets them: the change request of
// each issue it answers (none for a route an older Dispatch lacks), or changeErr in its place, the
// ending it answers, or endingErr in its place, with each read counted.
type specDispatch struct {
	outboxDispatch
	changeRequests map[string]dispatch.ChangeRequest
	changeErr      error
	changeReads    []string
	ending         *dispatch.SpecEnding
	endingErr      error
	endingReads    []string
}

func (d *specDispatch) ChangeRequest(ctx context.Context, key string, version int) (dispatch.ChangeRequest, error) {
	d.changeReads = append(d.changeReads, fmt.Sprintf("%s@%d", key, version))
	if d.changeErr != nil {
		return dispatch.ChangeRequest{}, d.changeErr
	}
	answer, ok := d.changeRequests[key]
	if !ok {
		return d.outboxDispatch.ChangeRequest(ctx, key, version)
	}
	answer.Issue, answer.Version = key, version
	return answer, nil
}

func (d *specDispatch) SpecEnding(ctx context.Context, key, head string) (dispatch.SpecEnding, error) {
	d.endingReads = append(d.endingReads, key+"@"+head)
	if d.endingErr != nil {
		return dispatch.SpecEnding{}, d.endingErr
	}
	if d.ending == nil {
		return d.outboxDispatch.SpecEnding(ctx, key, head)
	}
	answer := *d.ending
	answer.Issue, answer.Head = key, head
	return answer, nil
}

// exemptGateRig is a migrated store holding the root at phase, its spec gate under rule, open by
// exemption at version 3 under the rule of a change request, and its pull request at
// specGateHead, with the outbox and the engine that run it.
type exemptGateRig struct {
	t       *testing.T
	pool    *store.Pool
	records record.Store
	client  *specDispatch
	runner  *outbox
}

func newExemptGateRig(t *testing.T, at phase.Phase, rule record.GateRule) *exemptGateRig {
	t.Helper()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	putOutboxIssue(t, pool, records, record.Issue{Key: specGateRoot, Tree: specGateRoot, Project: "LEGION", Title: "root", Phase: at, Generation: 1,
		Status: "in_progress", Rank: "U", HandedOver: true, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"})
	gate := record.DesignGate{Issue: specGateRoot, Step: record.DesignStepSpec, ArtifactID: "the-roots-spec", LatestVersion: 3, Rule: rule, EverOpened: true}
	three := 3
	if rule == record.GateRuleChangeRequest {
		gate.ExemptVersion = &three
	} else {
		gate.ApprovedVersion = &three
	}
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		if err := records.PutGate(context.Background(), tx, gate); err != nil {
			return err
		}
		return records.PutPullRequest(context.Background(), tx, record.PullRequest{Issue: specGateRoot, Repo: "acme/widgets", Number: 42,
			Branch: "legion/" + specGateRoot, Base: "main", HeadSHA: specGateHead, State: record.PullRequestOpen, Generation: 1})
	}); err != nil {
		t.Fatalf("seed the root's gate and pull request: %v", err)
	}
	engine := workflow.New(records, workflow.Config{Project: "LEGION", Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps()}, quietLogger())
	client := &specDispatch{}
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", project: "legion", records: records, dispatch: client,
		handlers: []intake.Handler{engine}, now: time.Now, gatedSteps: config.DesignGateRootIssues.Steps()}
	return &exemptGateRig{t: t, pool: pool, records: records, client: client, runner: runner}
}

// gate is the root's spec gate as the record holds it.
func (r *exemptGateRig) gate() record.DesignGate {
	r.t.Helper()
	var gate *record.DesignGate
	if err := pgx.BeginFunc(context.Background(), r.pool, func(tx pgx.Tx) error {
		gates, err := r.records.GatesOf(context.Background(), tx, specGateRoot)
		gate = record.SpecGate(gates)
		return err
	}); err != nil || gate == nil {
		r.t.Fatalf("read the root's gate: %+v, %v", gate, err)
	}
	return *gate
}

// reading is the root's reading of the ending condition, nil for none.
func (r *exemptGateRig) reading() *record.SpecEnding {
	r.t.Helper()
	var ending *record.SpecEnding
	if err := pgx.BeginFunc(context.Background(), r.pool, func(tx pgx.Tx) error {
		var err error
		ending, err = r.records.SpecEnding(context.Background(), tx, specGateRoot)
		return err
	}); err != nil {
		r.t.Fatalf("read the root's reading: %v", err)
	}
	return ending
}

// count is how many outbox rows of the root the query's condition names.
func (r *exemptGateRig) count(condition string) int {
	r.t.Helper()
	var n int
	if err := r.pool.QueryRow(context.Background(), `select count(*) from outbox where issue = $1 and `+condition, specGateRoot).Scan(&n); err != nil {
		r.t.Fatalf("count the outbox rows where %s: %v", condition, err)
	}
	return n
}

// moveHead moves the root's recorded pull request to head.
func (r *exemptGateRig) moveHead(head string) {
	r.t.Helper()
	if _, err := r.pool.Exec(context.Background(), `update pull_requests set head_sha = $2 where issue = $1`, specGateRoot, head); err != nil {
		r.t.Fatalf("move the head: %v", err)
	}
}

// The gate seed reads the change request of a version it applies to a gate under the rule of a
// change request (R23): a version that proposes nothing moves the exemption with it, one that
// proposes something closes the gate. A gate under the rule of a person's approval reads none.
func TestTheGateSeedReadsTheChangeRequestOfAVersionItApplies(t *testing.T) {
	for _, tc := range []struct {
		name   string
		rule   record.GateRule
		answer *dispatch.ChangeRequest
		reads  int
		open   bool
	}{
		{name: "a version that proposes nothing", rule: record.GateRuleChangeRequest, answer: &dispatch.ChangeRequest{SpecSource: true}, reads: 1, open: true},
		{name: "a version that proposes a statement", rule: record.GateRuleChangeRequest, answer: &dispatch.ChangeRequest{SpecSource: true, Proposes: true}, reads: 1},
		{name: "an older Dispatch", rule: record.GateRuleChangeRequest, reads: 1},
		{name: "a gate under the rule of a person's approval", rule: record.GateRuleApproval, answer: &dispatch.ChangeRequest{SpecSource: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newExemptGateRig(t, phase.Planning, tc.rule)
			r.client.approval = dispatch.Approval{State: "none", LatestVersion: 4}
			if tc.answer != nil {
				r.client.changeRequests = map[string]dispatch.ChangeRequest{specGateRoot: *tc.answer}
			}
			if err := r.runner.execute(context.Background(), mustOutboxRow(t, specGateRoot,
				record.GateSeed{ArtifactID: "the-roots-spec", Step: record.DesignStepSpec, Version: 3, Generation: 1}, time.Now())); err != nil {
				t.Fatalf("execute the gate seed: %v", err)
			}
			if len(r.client.changeReads) != tc.reads {
				t.Fatalf("change request reads = %q, want %d", r.client.changeReads, tc.reads)
			}
			if gate := r.gate(); gate.LatestVersion != 4 || record.GateOpen(gate) != tc.open {
				t.Fatalf("the gate after the seed = %+v, want version 4 open=%t", gate, tc.open)
			}
		})
	}
}

// The live event's prepare stage reads the change request of a version of a document a gate under
// the rule of a change request holds, and carries what it read. A version of another document of
// the issue — its Verification document, from which the request reads its checks, and the event
// names no document's role — reads the request at the gate's current version (R25); a changes
// request reads nothing; a read an older Dispatch answers with no route carries none; and a read
// Dispatch fails fails the preparation, so the event is delivered again.
func TestPrepareArtifactReadsTheChangeRequestOfAVersionOfAnExemptRulesDocument(t *testing.T) {
	r := newExemptGateRig(t, phase.Planning, record.GateRuleChangeRequest)
	w := &workflowRuntime{pool: r.pool, records: r.records, dispatch: r.client, log: quietLogger()}
	version := intake.DispatchArtifact{Key: specGateRoot, ArtifactID: "the-roots-spec", Kind: intake.DispatchArtifactVersion, Version: 4}
	r.client.changeRequests = map[string]dispatch.ChangeRequest{specGateRoot: {SpecSource: true, VerificationVersion: 2}}
	got, err := w.prepareArtifact(context.Background(), version)
	if err != nil || got.ChangeRequest == nil || !got.ChangeRequest.ExemptsVersion(4) {
		t.Fatalf("prepareArtifact = %+v, %v; want the read of version 4 carried", got.ChangeRequest, err)
	}
	other := version
	other.ArtifactID = "artifact-verification"
	got, err = w.prepareArtifact(context.Background(), other)
	if err != nil || got.ChangeRequest == nil || got.ChangeRequest.Version != 3 || got.ChangeRequest.VerificationVersion != 2 ||
		!slices.Equal(r.client.changeReads, []string{specGateRoot + "@4", specGateRoot + "@3"}) {
		t.Fatalf("a version of another document = %+v, %v, reads %q; want the read at the gate's version 3 carried", got.ChangeRequest, err, r.client.changeReads)
	}
	changes := version
	changes.Kind = intake.DispatchArtifactChangesRequested
	if got, err := w.prepareArtifact(context.Background(), changes); err != nil || got.ChangeRequest != nil || len(r.client.changeReads) != 2 {
		t.Fatalf("a changes request = %+v, %v, reads %q; want no read", got.ChangeRequest, err, r.client.changeReads)
	}
	r.client.changeRequests = nil
	if got, err := w.prepareArtifact(context.Background(), version); err != nil || got.ChangeRequest != nil {
		t.Fatalf("a version an older Dispatch answers = %+v, %v; want none carried", got.ChangeRequest, err)
	}
	failing := &failingChangeRequest{specDispatch: r.client}
	w.dispatch = failing
	if _, err := w.prepareArtifact(context.Background(), version); err == nil || !strings.Contains(err.Error(), specGateRoot) {
		t.Fatalf("a read Dispatch failed = %v, want the preparation failed naming %s", err, specGateRoot)
	}
}

// failingChangeRequest is Dispatch failing every read of a change request.
type failingChangeRequest struct{ *specDispatch }

func (failingChangeRequest) ChangeRequest(context.Context, string, int) (dispatch.ChangeRequest, error) {
	return dispatch.ChangeRequest{}, &dispatch.Error{Status: 502, Message: "bad gateway"}
}

// A read of the change request Dispatch refuses for good — a version whose text does not parse
// (409 CHANGE_REQUEST_UNREADABLE), or a version it does not hold (404 VERSION_NOT_FOUND) — is a
// version no read will ever show as proposing nothing. The gate seed applies it unread, so an
// exempt gate closes as it does on a version an older Dispatch could not read, and the live
// event's preparation carries no read and fails nothing, so the event is not delivered again for
// ever.
func TestAVersionWhoseChangeRequestDispatchRefusesForGoodClosesTheGate(t *testing.T) {
	for _, refusal := range []*dispatch.Error{
		{Status: 409, Code: "CHANGE_REQUEST_UNREADABLE", Message: "version 4 of the spec does not parse"},
		{Status: 404, Code: "VERSION_NOT_FOUND", Message: "the spec holds no version 4"},
	} {
		t.Run(refusal.Code, func(t *testing.T) {
			r := newExemptGateRig(t, phase.Planning, record.GateRuleChangeRequest)
			r.client.approval = dispatch.Approval{State: "none", LatestVersion: 4}
			r.client.changeErr = refusal
			if err := r.runner.execute(context.Background(), mustOutboxRow(t, specGateRoot,
				record.GateSeed{ArtifactID: "the-roots-spec", Step: record.DesignStepSpec, Version: 3, Generation: 1}, time.Now())); err != nil {
				t.Fatalf("the gate seed under a refused read = %v, want the version applied unread", err)
			}
			if gate := r.gate(); gate.LatestVersion != 4 || record.GateOpen(gate) {
				t.Fatalf("the gate after the seed = %+v, want version 4 closed", gate)
			}

			w := &workflowRuntime{pool: r.pool, records: r.records, dispatch: r.client, log: quietLogger()}
			version := intake.DispatchArtifact{Key: specGateRoot, ArtifactID: "the-roots-spec", Kind: intake.DispatchArtifactVersion, Version: 4}
			got, err := w.prepareArtifact(context.Background(), version)
			if err != nil || got.ChangeRequest != nil {
				t.Fatalf("prepareArtifact under a refused read = %+v, %v; want no read carried and no failure", got.ChangeRequest, err)
			}
			if len(r.client.changeReads) != 2 {
				t.Fatalf("change request reads = %q, want one by the seed and one by the preparation", r.client.changeReads)
			}
		})
	}
}

// The outbox's read of the ending condition applies what Dispatch answered for the head it names
// (D10): a waiting answer is recorded with each reason told to the root's architect. A row whose
// head the pull request moved past, and a row an older Dispatch answers with no route, finish
// without recording anything.
func TestTheSpecEndingReadAppliesWhatDispatchAnswersForItsHead(t *testing.T) {
	r := newExemptGateRig(t, phase.Merging, record.GateRuleChangeRequest)
	r.client.ending = &dispatch.SpecEnding{State: "waiting", Version: 3, Reasons: []dispatch.EndingReason{{Code: "no_conclusion", Detail: "No conclusion."}}}
	read := func(head string) {
		t.Helper()
		if err := r.runner.execute(context.Background(), mustOutboxRow(t, specGateRoot,
			record.SpecEndingRead{Head: head, Cause: record.SpecEndingCauseOutcomes}, time.Now())); err != nil {
			t.Fatalf("execute the ending read at %s: %v", head, err)
		}
	}
	read(specGateHead)
	if reading := r.reading(); !reading.WaitsAt(specGateHead) || len(reading.Told) != 1 {
		t.Fatalf("the reading = %+v, want it waiting at %s with no_conclusion told", reading, specGateHead)
	}
	if told := r.count(`kind = 'notice' and payload->>'kind' = 'spec-ending-waits'`); told != 1 {
		t.Fatalf("spec-ending-waits notices = %d, want 1", told)
	}
	r.moveHead(specGateNextHead)
	read(specGateHead)
	if len(r.client.endingReads) != 1 {
		t.Fatalf("ending reads after the head moved past the row = %q, want the first alone", r.client.endingReads)
	}
	r.client.ending, r.client.endingErr = nil, nil
	read(specGateNextHead)
	if reading := r.reading(); reading.Head != specGateHead {
		t.Fatalf("the reading after an older Dispatch's answer = %+v, want the first kept", reading)
	}
}

// A read queued because the spec gate turned exempt (record.SpecEndingCauseExempted) tells a
// merger already running the ending step, so the outbox reads it at the pull request's current
// head when the head moved past the row, rather than finish it unread: the read the move queued
// may find the reading unchanged and tell nothing. A row of any other cause still finishes
// unread when the head moved past it.
func TestAnExemptedEndingReadWhoseHeadMovedReadsTheCurrentHead(t *testing.T) {
	r := newExemptGateRig(t, phase.Merging, record.GateRuleChangeRequest)
	r.client.ending = &dispatch.SpecEnding{State: "ready", Version: 3}
	// The reading an earlier time the gate stood exempt left, at the head the pull request moves to.
	if err := pgx.BeginFunc(context.Background(), r.pool, func(tx pgx.Tx) error {
		return r.records.PutSpecEnding(context.Background(), tx, record.SpecEnding{Issue: specGateRoot, Head: specGateNextHead,
			State: record.SpecEndingReady, ReadAt: time.Now()})
	}); err != nil {
		t.Fatalf("record the earlier reading: %v", err)
	}
	r.moveHead(specGateNextHead)
	read := func(cause record.SpecEndingCause) {
		t.Helper()
		if err := r.runner.execute(context.Background(), mustOutboxRow(t, specGateRoot,
			record.SpecEndingRead{Head: specGateHead, Cause: cause}, time.Now())); err != nil {
			t.Fatalf("execute the %s read at %s: %v", cause, specGateHead, err)
		}
	}
	read(record.SpecEndingCauseExempted)
	if want := []string{specGateRoot + "@" + specGateNextHead}; !slices.Equal(r.client.endingReads, want) {
		t.Fatalf("Dispatch reads of a read of the turn to exemption whose head moved = %q, want %q", r.client.endingReads, want)
	}
	if told := r.count(`kind = 'role_message'`); told != 1 {
		t.Fatalf("role messages after that read = %d, want the merger told the ending step once", told)
	}
	read(record.SpecEndingCauseOutcomes)
	if len(r.client.endingReads) != 1 {
		t.Fatalf("Dispatch reads after an outcome list's read whose head moved = %q, want none more", r.client.endingReads)
	}
}

// The merger's start fence reads the ending condition at the merging issue's head when its tree's
// spec gate stands open by exemption, records it, and holds the start while it waits (D10); a
// ready answer lets the start go, and a gate a person opened asks Dispatch nothing.
func TestTheMergerStartWaitsWhileTheEndingWaits(t *testing.T) {
	r := newExemptGateRig(t, phase.Merging, record.GateRuleChangeRequest)
	var issue *record.Issue
	if err := pgx.BeginFunc(context.Background(), r.pool, func(tx pgx.Tx) error {
		var err error
		issue, err = r.records.Issue(context.Background(), tx, specGateRoot)
		return err
	}); err != nil || issue == nil {
		t.Fatalf("read the root: %v", err)
	}
	row := mustOutboxRow(t, specGateRoot, record.SuperviseRequest{Op: "start", Tree: specGateRoot, Role: claim.RoleMerger, Generation: 1}, time.Now())
	row.ID = 71
	r.client.ending = &dispatch.SpecEnding{State: "waiting", Version: 3, Reasons: []dispatch.EndingReason{{Code: "no_final_list_for_head", Detail: "No final list."}}}
	err := r.runner.mergerWaitsForTheDesignGate(context.Background(), row, *issue, claim.RoleMerger)
	if !errors.Is(err, wait.ErrWaiting) || !strings.Contains(err.Error(), "no_final_list_for_head") {
		t.Fatalf("the fence while the ending waits = %v, want a wait naming the reason", err)
	}
	if reading := r.reading(); !reading.WaitsAt(specGateHead) {
		t.Fatalf("the reading the fence recorded = %+v, want it waiting at %s", reading, specGateHead)
	}
	r.client.ending = &dispatch.SpecEnding{State: "ready", Version: 3}
	row.Attempts++
	if err := r.runner.mergerWaitsForTheDesignGate(context.Background(), row, *issue, claim.RoleMerger); err != nil {
		t.Fatalf("the fence once the ending holds = %v, want the start let go", err)
	}
	if err := r.runner.mergerWaitsForTheDesignGate(context.Background(), row, *issue, claim.RoleTester); err != nil || len(r.client.endingReads) != 2 {
		t.Fatalf("the fence of another role = %v, reads %q; want no read", err, r.client.endingReads)
	}

	approved := newExemptGateRig(t, phase.Merging, record.GateRuleApproval)
	if err := approved.runner.mergerWaitsForTheDesignGate(context.Background(), row, *issue, claim.RoleMerger); err != nil || len(approved.client.endingReads) != 0 {
		t.Fatalf("the fence under a gate a person opened = %v, reads %q; want no read", err, approved.client.endingReads)
	}
}

// A merger the start row starts under a tree whose spec gate stands open by exemption is given a
// task that ends with the paragraph of the ending step (D10), whatever wrote the task: here the
// resume task a re-admission hands a merger mid-phase (admit's startMidPhaseMembers), written
// while the re-admitted tree held no gate, so it names no step. The start fence's own reading
// tells the merger nothing beside it: the task says it. Under a gate a person opened the task is
// given as it was written.
func TestAMergerStartUnderAnExemptGateGivesTheEndingStep(t *testing.T) {
	for _, tc := range []struct {
		name string
		rule record.GateRule
		step bool
	}{
		{name: "a gate open by exemption", rule: record.GateRuleChangeRequest, step: true},
		{name: "a gate a person opened", rule: record.GateRuleApproval},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			r := newExemptGateRig(t, phase.Merging, tc.rule)
			r.client.ending = &dispatch.SpecEnding{State: "ready", Version: 3}
			var issue *record.Issue
			if err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
				var err error
				issue, err = r.records.Issue(ctx, tx, specGateRoot)
				return err
			}); err != nil || issue == nil {
				t.Fatalf("read the root: %v", err)
			}
			sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
			token, err := claim.NewToken("legion", specGateRoot, claim.RoleMerger)
			if err != nil {
				t.Fatalf("claim token: %v", err)
			}
			machine, _, err := sup.Create(ctx, supervise.Claim{Token: token, Project: "legion", Tree: specGateRoot, Issue: specGateRoot,
				Role: claim.RoleMerger, State: supervise.StateQueued}, "")
			if err != nil {
				t.Fatalf("create the merger's claim: %v", err)
			}
			r.runner.supervisor, r.runner.tokens, r.runner.stateDir = sup, outboxTokens{}, t.TempDir()
			resume := workflow.ResumePhaseTask(*issue)
			row := mustOutboxRow(t, specGateRoot, record.SuperviseRequest{Op: "start", Tree: specGateRoot, Role: claim.RoleMerger,
				Generation: 1, Phase: phase.Merging, Task: resume, ResumeTask: true}, time.Now())
			row.ID = 93
			if err := r.runner.execute(ctx, row); err != nil {
				t.Fatalf("run the merger's start row: %v", err)
			}
			want := resume
			if tc.step {
				want += " The design gate of " + specGateRoot + " opened with no person's approval: its change request proposes nothing. " +
					"Before each merge and each enqueue, run `dispatch spec-ending --issue " + specGateRoot + " --head <head>`, " +
					"and merge or enqueue only when its first line reads `ready`."
			}
			if got := machine.Claim().Pending; got == nil || got.Task != want {
				t.Fatalf("the merger's task after its start = %+v, want %q", got, want)
			}
			if told := r.count(`kind = 'role_message'`); told != 0 {
				t.Fatalf("role messages after the merger's start = %d, want none: the start's own task names the step", told)
			}
		})
	}
}

// A delivery that wakes a released merger waits while the recorded ending waits at the pull
// request's current head; a reading of a head the pull request moved past, a ready reading and a
// gate a person opened hold nothing.
func TestAHeldDeliveryWaitsWhileTheRecordedEndingWaitsAtTheHead(t *testing.T) {
	r := newExemptGateRig(t, phase.Merging, record.GateRuleChangeRequest)
	issue := record.Issue{Key: specGateRoot, Tree: specGateRoot}
	holds := func() bool {
		t.Helper()
		held, err := r.runner.endingHoldsMerger(context.Background(), issue)
		if err != nil {
			t.Fatalf("read the ending hold: %v", err)
		}
		return held
	}
	if holds() {
		t.Fatalf("a merger with no reading is held")
	}
	put := func(head, state string) {
		t.Helper()
		if err := pgx.BeginFunc(context.Background(), r.pool, func(tx pgx.Tx) error {
			return r.records.PutSpecEnding(context.Background(), tx, record.SpecEnding{Issue: specGateRoot, Head: head, State: state, ReadAt: time.Now()})
		}); err != nil {
			t.Fatalf("record the reading: %v", err)
		}
	}
	put(specGateHead, record.SpecEndingWaiting)
	if !holds() {
		t.Fatalf("a merger whose reading waits at its head is not held")
	}
	r.moveHead(specGateNextHead)
	if holds() {
		t.Fatalf("a merger whose reading is of a head the pull request moved past is held")
	}
	put(specGateNextHead, record.SpecEndingReady)
	if holds() {
		t.Fatalf("a merger whose reading is ready is held")
	}
}

// A spec-request-stale notice whose architect will not hold its role again goes to the controller
// in its place (D8), once however many runs and rows carry it, and the copy gives the controller
// its own step: name an owner, who writes the request again and opens a new approval request.
func TestAStaleRequestGoesToTheControllerWhenItsArchitectIsGone(t *testing.T) {
	r := newExemptGateRig(t, phase.Planning, record.GateRuleApproval)
	notice := record.Notice{Kind: record.SpecRequestStaleNotice, Role: claim.RoleArchitect, Reason: "stale",
		Summary: "Dispatch refused the approval. Write the request again against the approved spec. Then open a new approval request."}
	for range 2 {
		if err := r.runner.parkDecision(context.Background(), specGateRoot, specGateRoot, notice, record.OutboxKey(81), 81); err != nil {
			t.Fatalf("hand the stale request on: %v", err)
		}
	}
	if sent := r.count(`kind = 'controller_notice' and payload->>'kind' = 'spec-request-stale'`); sent != 1 {
		t.Fatalf("controller notices of the stale request = %d, want 1", sent)
	}
	var summary string
	if err := r.pool.QueryRow(context.Background(), `select payload->>'summary' from outbox where issue = $1 and kind = 'controller_notice'`, specGateRoot).Scan(&summary); err != nil {
		t.Fatalf("read the controller's copy: %v", err)
	}
	want := "Dispatch refused the approval. Name an owner for " + specGateRoot +
		". The owner writes the request again against the approved spec and opens a new approval request."
	if summary != want {
		t.Fatalf("the controller's copy says %q, want %q", summary, want)
	}
}
