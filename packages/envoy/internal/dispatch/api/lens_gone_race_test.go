package api

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// holdGone takes the node target names to gone in a transaction it holds open - writeNodeState,
// which takes the issue's owner lock first and then moves the node, and its whole part_of subtree
// with it - sends post meanwhile, waits until post waits on that transaction or answers,
// commits it, and answers what post answered. name names the post in a failure.
func holdGone(t *testing.T, rig reasoningRig, name string, target stateTarget, post func() *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	t.Helper()
	ctx := context.Background()
	tx, err := rig.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the gone of %s: %v", target.address, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	held, found, err := graphstore.ReadNode(ctx, tx, target.end.kind, target.end.id)
	if err != nil || !found {
		t.Fatalf("read the %s %s: found %t, %v", target.kind, target.address, found, err)
	}
	at, err := transactionNow(ctx, tx)
	if err != nil {
		t.Fatalf("read the gone's time: %v", err)
	}
	if _, _, err := writeNodeState(ctx, tx, nodeStateWrite{
		issue: rig.issue, target: target,
		state: graphstore.StateGone, who: held.MadeBy, at: at,
		allowed: func(graphstore.Node) error { return nil },
	}); err != nil {
		t.Fatalf("take %s to gone: %v", target.address, err)
	}
	// The sessions already waiting on the gone's owner lock before the post is sent: a pass of
	// the server's own may queue there too, and the post is the one more that waits.
	var gone int64
	if err := tx.QueryRow(ctx, `select pg_backend_pid()`).Scan(&gone); err != nil {
		t.Fatalf("read the gone's backend: %v", err)
	}
	waitingOnGone := func() int {
		t.Helper()
		var waiting int
		if err := rig.database.Pool.QueryRow(ctx, `
			select count(*) from pg_stat_activity
			where datname = current_database() and $1::integer = any(pg_blocking_pids(pid)) and query = $2
		`, gone, store.OwnerLockStatement).Scan(&waiting); err != nil {
			t.Fatalf("read what waits on the gone: %v", err)
		}
		return waiting
	}
	before := waitingOnGone()
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() { answered <- post() }()
	var answer *httptest.ResponseRecorder
	waitUntil(t, 30*time.Second, name+" waiting on the gone of "+target.address+", or answering", func() bool {
		select {
		case answer = <-answered:
			return true
		default:
		}
		// The post waits at the issue's owner lock (lockIssueOwner), the lock the gone holds:
		// only then has it found what it names live and not yet written. It may wait there
		// behind a pass of the server's own that queued at that lock first, so it is found as
		// one more session waiting on the gone at the owner lock's statement.
		return waitingOnGone() > before
	})
	if answer != nil {
		t.Errorf("%s answered %d %s while the gone of %s was still open, want it held at the issue's owner lock",
			name, answer.Code, answer.Body.String(), target.address)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the gone of %s: %v", target.address, err)
	}
	if answer == nil {
		select {
		case answer = <-answered:
		case <-time.After(30 * time.Second):
			t.Fatalf("%s did not answer within 30 s of the gone's commit", name)
		}
	}
	return answer
}

// lensTarget is the lens invoked answers as the state route names it.
func lensTarget(lens lensInvoked) stateTarget {
	return stateTarget{kind: graphstore.KindLens, address: lens.Address, end: storedEnd{kind: graphstore.KindLens, id: lens.ID}}
}

// TestAFindingOrARiskWrittenWhileItsLensIsTakenGoneIsRefused holds open, in a transaction, the
// gone of a lens (holdGone), posts a finding to that lens through POST .../nodes/{id}/findings
// meanwhile, and then commits the gone; and the same for a risk under a pre-mortem lens through
// POST .../risks. Each post finds its lens live before the gone commits and then waits at the
// issue's owner lock, so it reads the lens again once the gone has committed (refuseGoneParents)
// and is refused 409 NODE_GONE naming the lens, and no finding and no risk stands under the gone
// lens. A write that did not read its lens again under that lock would wait there and then write
// under the gone lens, and its finding or its risk would stand live under it, since the gone read
// what stood under the lens before the post wrote anything.
func TestAFindingOrARiskWrittenWhileItsLensIsTakenGoneIsRefused(t *testing.T) {
	t.Parallel()
	rig := newReasoningRig(t)
	ctx := context.Background()
	// requireNothingUnder holds the lens to gone, with no node standing under it by a part_of tuple.
	requireNothingUnder := func(lens lensInvoked) {
		t.Helper()
		if node, found, err := graphstore.ReadNode(ctx, rig.database.Pool, graphstore.KindLens, lens.ID); err != nil || !found || node.State != graphstore.StateGone {
			t.Fatalf("after the gone committed %s reads %+v (found %t, %v), want it gone", lens.Address, node, found, err)
		}
		if under := countRows(t, rig.database, `select count(*) from graph_tuples where kind = $1 and to_kind = $2 and to_id = $3`,
			graphstore.TuplePartOf, graphstore.KindLens, lens.ID); under != 0 {
			t.Fatalf("%d nodes stand under the gone lens %s by a part_of tuple, want none", under, lens.Address)
		}
	}

	lens := rig.invoke(rig.spec, "trade-off", "alice", nil)
	finding := holdGone(t, rig, "the finding added to "+lens.Address, lensTarget(lens), func() *httptest.ResponseRecorder {
		return rig.post(nodePath(lens.Address, "findings"), map[string]any{"text": "A finding written as its lens is taken out."}, "alice")
	})
	requireGoneRefused(t, "the finding added to "+lens.Address+" as it is taken gone", finding, lens.Address)
	requireNothingUnder(lens)

	premortem := rig.invoke(rig.spec, graphstore.LensKindPreMortem, "alice", nil)
	risk := holdGone(t, rig, "the risk written under "+premortem.Address, lensTarget(premortem), func() *httptest.ResponseRecorder {
		return rig.post("/risks", map[string]any{
			"lens": premortem.Address, "heading": "A risk written as its lens is taken out",
			"early_warning": "The lens reads gone", "prevention": "The write reads its lens again",
		}, "alice")
	})
	requireGoneRefused(t, "the risk written under "+premortem.Address+" as it is taken gone", risk, premortem.Address)
	requireNothingUnder(premortem)
}
