package signals

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/prodevents"
)

// changeRowsOf is how many change rows of the record's kind name id, pending or drained.
func (r *rig) changeRowsOf(id string) int {
	r.t.Helper()
	var count int
	if err := r.pool.QueryRow(r.ctx, `select
		(select count(*) from delivery_changes_pending where kind = 'production_events' and ref = $1)
		+ (select count(*) from delivery_changes where kind = 'production_events' and ref = $1)`, id).Scan(&count); err != nil {
		r.t.Fatalf("count the change rows of %s: %v", id, err)
	}
	return count
}

// TestADeployRunWhoseProductionJobAndApplyStepSucceedWritesOneDeployRow: a deploy run on main
// whose production job and apply step succeed is one deploy row of the repository's owner, keyed
// by its run, started at the run's creation, finished at its production job's completion, holding
// the run's head and the merge commit of the pull request it ships, and the pull request. A second
// delivery of the same run, written again by a run writer, writes no second row, moves none, and
// logs no change.
func TestADeployRunWhoseProductionJobAndApplyStepSucceedWritesOneDeployRow(t *testing.T) {
	r := newRig(t)
	r.pullRequest(7, at(30), sha("7"), "")
	r.deployRun(100, at(60), sha("a"), prodJob, success, success)
	r.passDeploys()

	rows := r.deploys()
	if len(rows) != 1 {
		t.Fatalf("deploy rows = %+v, want one", rows)
	}
	row := rows[0]
	if row.ProjectKey != alpha || row.Kind != prodevents.KindDeploy || row.Source != prodevents.SourceGitHub ||
		row.SourceRef != "github:100" || row.Status != prodevents.StatusSucceeded || row.Repo != widgets ||
		row.URL != "https://github.com/acme/widgets/actions/runs/100" || row.Actor != Actor {
		t.Fatalf("deploy row = %+v", row)
	}
	if !row.StartedAt.Equal(at(60)) || !row.FinishedAt.Equal(at(70)) {
		t.Errorf("deploy row times = %v .. %v, want the run's creation %v and the job's completion %v", row.StartedAt, row.FinishedAt, at(60), at(70))
	}
	if !slices.Equal(row.Commits, []string{sha("a"), sha("7")}) || !slices.Equal(row.PullRequests, []string{"acme/widgets#7"}) {
		t.Errorf("deploy row ships %v at %v, want acme/widgets#7 at the head and its merge commit", row.PullRequests, row.Commits)
	}
	changes := r.changeRowsOf(row.ID)
	if changes != 1 {
		t.Errorf("the deploy row's write logged %d change rows, want 1", changes)
	}

	r.deployRun(100, at(60), sha("a"), prodJob, success, success)
	r.passDeploys()
	again := r.deploys()
	if len(again) != 1 || again[0].ID != row.ID || !again[0].FinishedAt.Equal(*row.FinishedAt) {
		t.Fatalf("after a second delivery, deploy rows = %+v, want the one row %s unmoved", again, row.ID)
	}
	if got := r.changeRowsOf(row.ID); got != changes {
		t.Errorf("a second delivery logged %d change rows of the deploy row, want none", got-changes)
	}
}

// TestARunWhoseApplyStepFailedWritesNoDeployRow: a run whose production job succeeded but whose
// apply step, the step the rule names, did not, applied nothing, and makes no deploy row; nor does
// a run of a job the rule does not name.
func TestARunWhoseApplyStepFailedWritesNoDeployRow(t *testing.T) {
	r := newRig(t)
	r.deployRun(110, at(60), sha("b"), prodJob, success, failure)
	r.deployRun(111, at(90), sha("c"), canaryJob, success, success)
	r.passDeploys()
	if rows := r.deploys(); len(rows) != 0 {
		t.Fatalf("deploy rows = %+v, want none", rows)
	}
}

// TestAJobWriteMakesTheDeployRowOnceTheProductionJobFinishes: the job intake writes a running
// deploy run's jobs as each finishes (delivery.UpsertRunJobs). While the production job runs, the
// run is no deploy and has no row; the job write that records its success makes the row, from the
// change that write logs once it has committed.
func TestAJobWriteMakesTheDeployRowOnceTheProductionJobFinishes(t *testing.T) {
	r := newRig(t)
	r.deployRun(120, at(60), sha("d"), prodJob, nil, nil)
	r.passDeploys()
	if rows := r.deploys(); len(rows) != 0 {
		t.Fatalf("with the production job running, deploy rows = %+v, want none", rows)
	}
	r.deployRun(120, at(60), sha("d"), prodJob, success, success)
	r.passDeploys()
	if row := r.deployOf(120); !row.FinishedAt.Equal(at(70)) {
		t.Errorf("the deploy row finished at %v, want the job's completion %v", row.FinishedAt, at(70))
	}
}

// TestARuleChangeDerivesTheRepositorysDeployRowsAgain: a save that changes the repository's rule
// derives its GitHub deploy rows again on the next pass. A run the new rule calls a deploy gains
// its row; a row the new rule does not call a deploy is deleted; and one a failure blames stays,
// dropped, with each failure on it rejected for a reason that names the rule change.
func TestARuleChangeDerivesTheRepositorysDeployRowsAgain(t *testing.T) {
	r := newRig(t)
	r.deployRun(130, at(60), sha("e"), prodJob, success, success)
	r.deployRun(131, at(120), sha("f"), prodJob, success, success)
	r.deployRun(132, at(180), sha("1"), canaryJob, success, success)
	r.passDeploys()
	if rows := r.deploys(); len(rows) != 2 {
		t.Fatalf("under the first rule, deploy rows = %+v, want runs 130 and 131", rows)
	}
	blamed, unblamed := r.deployOf(130), r.deployOf(131)
	flagged, _, err := prodevents.Write(r.ctx, r.pool, prodevents.Event{
		ProjectKey: alpha, Kind: prodevents.KindFailure, Source: prodevents.SourcePerson, SourceRef: prodevents.PersonSourceRef(blamed.ID, "alice"),
		Actor: person, DeployID: new(blamed.ID), Signal: new(prodevents.SignalPerson), DetectedAt: new(at(200)),
	})
	if err != nil {
		t.Fatalf("flag the deploy of run 130: %v", err)
	}

	r.saveRule(canaryJob)
	r.passDeploys()

	added := r.deployOf(132)
	if added.ProjectKey != alpha || added.Status != prodevents.StatusSucceeded {
		t.Errorf("the run the new rule calls a deploy = %+v", added)
	}
	if _, err := prodevents.Get(r.ctx, r.pool, alpha, unblamed.ID); !errors.Is(err, prodevents.ErrNotFound) {
		t.Errorf("the row of run 131, which nothing blames, stayed")
	}
	kept, err := prodevents.Get(r.ctx, r.pool, alpha, blamed.ID)
	if err != nil {
		t.Fatalf("the row of run 130, which a failure blames, is gone: %v", err)
	}
	if kept.Status != prodevents.StatusDropped || !strings.Contains(kept.Note, "deploy rule of acme/widgets changed") {
		t.Errorf("the blamed row = status %q, note %q; want dropped, naming the rule change", kept.Status, kept.Note)
	}
	rejected, err := prodevents.Get(r.ctx, r.pool, alpha, flagged.ID)
	if err != nil {
		t.Fatalf("read the failure: %v", err)
	}
	if rejected.State != prodevents.StateRejected || rejected.StateReason == nil ||
		!strings.Contains(*rejected.StateReason, "deploy rule of acme/widgets changed") ||
		!strings.Contains(*rejected.StateReason, canaryJob) || rejected.StateBy == nil || *rejected.StateBy != Actor {
		t.Errorf("the failure on the dropped row = state %q, reason %v, by %v; want rejected naming the rule change", rejected.State, rejected.StateReason, rejected.StateBy)
	}
	if _, held, err := prodevents.DeployForCommit(r.ctx, r.pool, sha("e")); err != nil || held {
		t.Errorf("a signal finds the dropped deploy by its commit (%v, %v): no signal blames it", held, err)
	}
}

// TestTheDeployRowWriteTakesNoChangeLogLock: the adapter writes a deploy row in a transaction of its
// own that takes the delivery change log's lock nowhere (dispatch://INFRA-978, D8 and R13), so the
// row commits while another transaction holds that lock, with its change row waiting in the outbox;
// only the drain after the commit waits for the lock, and moves the change row once it is free.
func TestTheDeployRowWriteTakesNoChangeLogLock(t *testing.T) {
	r := newRig(t)
	r.deployRun(140, at(60), sha("9"), prodJob, success, success)

	gate, err := r.pool.Begin(r.ctx)
	if err != nil {
		t.Fatalf("begin the gate: %v", err)
	}
	defer gate.Rollback(r.ctx)
	if _, err := gate.Exec(r.ctx, `select pg_advisory_xact_lock(6900, 0)`); err != nil {
		t.Fatalf("take the change log's lock: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- passDeploys(r.ctx, r.pool, time.Now) }()

	var row prodevents.Event
	for deadline := time.Now().Add(30 * time.Second); ; {
		if rows := r.deploys(); len(rows) == 1 {
			row = rows[0]
			break
		}
		select {
		case err := <-done:
			t.Fatalf("the pass ended (%v) with no deploy row while the change log's lock was held", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("no deploy row committed in 30 s while another transaction held the change log's lock")
		}
		time.Sleep(20 * time.Millisecond)
	}
	var pending int
	if err := r.pool.QueryRow(r.ctx, `select count(*) from delivery_changes_pending where kind = 'production_events' and ref = $1`,
		row.ID).Scan(&pending); err != nil || pending != 1 {
		t.Fatalf("with the lock held, the row's pending change rows = %d (%v), want 1 waiting in the outbox", pending, err)
	}
	if err := gate.Rollback(r.ctx); err != nil {
		t.Fatalf("release the change log's lock: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the pass: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("the pass did not end within 30 s of the lock's release")
	}
	var drained int
	if err := r.pool.QueryRow(r.ctx, `select count(*) from delivery_changes where kind = 'production_events' and ref = $1`,
		row.ID).Scan(&drained); err != nil || drained != 1 {
		t.Errorf("once the lock is free, the row's drained change rows = %d (%v), want 1", drained, err)
	}
}
