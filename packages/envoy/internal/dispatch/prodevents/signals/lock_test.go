package signals

import (
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// newFailure answers, for the writer about to run, the id of the failure row it makes: the one row
// the record holds beyond those it holds now.
func (r *rig) newFailure() func() (string, bool) {
	known := map[string]bool{}
	for _, row := range r.failures() {
		known[row.ID] = true
	}
	return func() (string, bool) {
		for _, row := range r.failures() {
			if !known[row.ID] {
				return row.ID, true
			}
		}
		return "", false
	}
}

// underChangeLogLock runs write, one writer of the signals, while another transaction holds the
// delivery change log's lock (pg_advisory_xact_lock(6900, 0), which the drain takes and no writer
// of the record does), and holds that the write committed all the same: landed answers the id of
// the failure row once it is stored, polled for 30 s while the lock is held; that row's change
// rows are then waiting in delivery_changes_pending and none of them went into the log; and once
// the lock is released the write returns and every one of those change rows is in the log, none
// left pending (dispatch://INFRA-978, D8 and R13).
func (r *rig) underChangeLogLock(what string, write func() error, landed func() (string, bool)) {
	r.t.Helper()
	gate, err := r.pool.Begin(r.ctx)
	if err != nil {
		r.t.Fatalf("%s: begin the gate: %v", what, err)
	}
	defer gate.Rollback(r.ctx)
	if _, err := gate.Exec(r.ctx, `select pg_advisory_xact_lock(6900, 0)`); err != nil {
		r.t.Fatalf("%s: take the change log's lock: %v", what, err)
	}
	done := make(chan error, 1)
	go func() { done <- write() }()

	var id string
	for deadline := time.Now().Add(30 * time.Second); ; {
		if got, ok := landed(); ok {
			id = got
			break
		}
		select {
		case err := <-done:
			r.t.Fatalf("%s: the write ended (%v) with no failure row stored while the change log's lock was held", what, err)
		default:
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("%s: no failure row committed in 30 s while another transaction held the change log's lock", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
	changeRows := func(table string) int {
		r.t.Helper()
		var count int
		if err := r.pool.QueryRow(r.ctx, `select count(*) from `+table+` where kind = 'production_events' and ref = $1`, id).Scan(&count); err != nil {
			r.t.Fatalf("%s: count the change rows of %s in %s: %v", what, id, table, err)
		}
		return count
	}
	pending, logged := changeRows("delivery_changes_pending"), changeRows("delivery_changes")
	if pending == 0 {
		r.t.Fatalf("%s: with the lock held, row %s has no change row waiting in the outbox, want its changes there", what, id)
	}
	r.t.Logf("%s: row %s committed with the change log's lock held, its %d change rows waiting in the outbox, %d already in the log", what, id, pending, logged)
	if err := gate.Rollback(r.ctx); err != nil {
		r.t.Fatalf("%s: release the change log's lock: %v", what, err)
	}
	select {
	case err := <-done:
		if err != nil {
			r.t.Fatalf("%s: the write: %v", what, err)
		}
	case <-time.After(30 * time.Second):
		r.t.Fatalf("%s: the write did not return within 30 s of the lock's release", what)
	}
	if left, now := changeRows("delivery_changes_pending"), changeRows("delivery_changes"); left != 0 || now-logged != pending {
		r.t.Errorf("%s: once the lock is free, row %s has %d change rows pending and %d more in the log, want none pending and its %d moved", what, id, left, now-logged, pending)
	}
}

// TestTheSignalsWritesTakeNoChangeLogLock: each automatic signal's write of a failure row commits
// while another transaction holds the delivery change log's lock, its change rows waiting in
// delivery_changes_pending until the drain after the lock is released, as
// TestTheDeployRowWriteTakesNoChangeLogLock holds for the adapter's deploy row: a check_fail
// written by the record route's handler (RecordCheckFailure) and by the checks feed, a revert
// written by the deploys feed once the deploy that ships it has its row, and an incident_issue
// written by the incidents feed. The routes' writes and the signals' own row writes, which need
// nothing of this package, are held by prodevents' TestTheRoutesAndTheSignalsWritesTakeNoChangeLogLock.
func TestTheSignalsWritesTakeNoChangeLogLock(t *testing.T) {
	r := newRig(t)
	issue := r.issue(1)
	r.pullRequest(8, at(30), sha("8"), issue)
	r.pullRequest(5, at(31), sha("5"), "")
	r.pullRequest(11, at(32), sha("b"), "")
	r.deployRun(200, at(60), sha("a"), prodJob, success, success)
	r.deployRun(201, at(120), sha("c"), prodJob, success, success)
	r.passDeploys()
	if rows := r.deploys(); len(rows) != 2 {
		t.Fatalf("deploy rows = %+v, want runs 200 and 201", rows)
	}

	handled := r.checkResult(issue, "c1", sha("8"), verdictFail, model.CheckWhenAfterMerge)
	r.underChangeLogLock("the record route's check_fail", func() error {
		_, err := RecordCheckFailure(r.ctx, r.pool, handled)
		return err
	}, r.newFailure())

	r.checkResult(issue, "c2", sha("8"), verdictFail, model.CheckWhenAfterMerge)
	safe := r.safe()
	r.underChangeLogLock("the checks feed's check_fail", func() error { return passChecks(r.ctx, r.pool, safe) }, r.newFailure())

	if err := delivery.RecordRevert(r.ctx, r.pool, delivery.Revert{
		Repo: widgets, Number: 9, RevertsRepo: widgets, RevertsNumber: 5, RevertsMergeCommitSHA: new(sha("5")),
		MergedAt: at(90), MergeCommitSHA: new(sha("9")),
	}); err != nil {
		t.Fatalf("record the revert: %v", err)
	}
	r.underChangeLogLock("the deploys feed's revert", func() error { return passDeploys(r.ctx, r.pool, time.Now) }, r.newFailure())

	incident := r.issue(10, "incident")
	r.link(incident, "https://github.com/acme/widgets/pull/11")
	r.issueEvent(incident, "issue.created", []string{"incident"}, []string{"https://github.com/acme/widgets/pull/11"}, nil)
	safe = r.safe()
	r.underChangeLogLock("the incidents feed's incident_issue", func() error { return passIncidents(r.ctx, r.pool, safe) }, r.newFailure())
}
