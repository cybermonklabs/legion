package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// TestAPreventedByAppendedWhileItsCheckIsMarkedGoneIsNotLeftStanding holds open, in a transaction,
// the index pass of a version of a check's document that no longer holds the check - the owner lock
// of the document first, as a version write takes it, then refs.IndexBlocks, which marks the check
// gone and retracts every prevented_by it reads at it - appends a prevented_by from a risk of
// another issue to that check through the pointer route meanwhile, and then commits the pass. A
// pass that marks a check gone takes its prevented_by tuples with it (spec requirement
// r-constraint-risk), and an append takes the owner lock of the check's document before it reads
// whether the check is gone, so the append is held until the pass commits and is then refused,
// naming the check, and the risk holds no prevented_by to it. An append that read the check with
// no lock would read it live before the pass committed, and the pass would read the risk's tuples
// before the append committed, so both would commit and the tuple would stand at a gone check.
func TestAPreventedByAppendedWhileItsCheckIsMarkedGoneIsNotLeftStanding(t *testing.T) {
	rig := newPremortemRig(t)
	ctx := context.Background()
	premortem := rig.invoke(rig.spec, graphstore.LensKindPreMortem, "alice", nil)
	written := rig.post("/risks", map[string]any{
		"lens": premortem.Address, "heading": "A job runs twice.", "early_warning": "Two drainers.", "prevention": "One drainer.",
	}, "alice")
	if written.Code != http.StatusCreated {
		t.Fatalf("write the risk: status=%d body=%s", written.Code, written.Body.String())
	}
	risk := decodeBody[riskWritten](t, written)

	tx, err := rig.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the index pass: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	owner := events.OwnerRows{}
	owner.Add(rig.otherIssue, "", "the document of "+rig.otherIssueCheck)
	if err := events.LockOwnerRows(ctx, tx, owner); err != nil {
		t.Fatalf("take the owner lock of the check's document: %v", err)
	}
	const kept = "A paragraph the next version keeps."
	_, won, stated, err := refs.IndexBlocks(ctx, tx, rig.otherIssueDocumentID, 2, time.Now().UTC(), kept+"\n",
		[]model.ArtifactBlock{{ID: "kept", Type: "paragraph", From: 0, To: len(kept)}}, nil, nil, refs.TypedFacts{})
	if err != nil {
		t.Fatalf("index version 2 of the check's document: %v", err)
	}
	if !won {
		t.Fatal("the index of version 2 lost the revision fence, so it marked nothing gone")
	}
	if err := graphstore.Caused(ctx, tx, stated, graphstore.NoEvent); err != nil {
		t.Fatalf("append what version 2 stated: %v", err)
	}
	if node, held, err := graphstore.ReadNode(ctx, tx, refs.BlockNodeKind, rig.otherIssueCheckNode); err != nil || !held || node.State != graphstore.StateGone {
		t.Fatalf("inside the pass the check %s reads %+v (held %t, %v), want it gone", rig.otherIssueCheckNode, node, held, err)
	}

	var pass uint32
	if err := tx.QueryRow(ctx, `select pg_backend_pid()`).Scan(&pass); err != nil {
		t.Fatalf("read the index pass's backend: %v", err)
	}
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		answered <- rig.post(nodePath(risk.Address, "pointers"), map[string]any{
			"kind": graphstore.TuplePreventedBy, "action": "append", "to": rig.otherIssueCheck,
		}, "alice")
	}()
	var appended *httptest.ResponseRecorder
	waitUntil(t, 30*time.Second, "the append to "+rig.otherIssueCheck+" waiting on the index pass, or answering", func() bool {
		select {
		case appended = <-answered:
			return true
		default:
		}
		var waiting bool
		if err := rig.database.Pool.QueryRow(ctx, `
			select exists (select 1 from pg_stat_activity where $1::integer = any(pg_blocking_pids(pid)))
		`, int64(pass)).Scan(&waiting); err != nil {
			t.Fatalf("read what waits on the index pass: %v", err)
		}
		return waiting
	})
	if appended != nil {
		t.Errorf("the append to %s answered %d %s while the index pass marking it gone was still open, want it held at the owner lock of the check's document",
			rig.otherIssueCheck, appended.Code, appended.Body.String())
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the index pass: %v", err)
	}
	if appended == nil {
		select {
		case appended = <-answered:
		case <-time.After(30 * time.Second):
			t.Fatalf("the append to %s did not answer within 30 s of the index pass's commit", rig.otherIssueCheck)
		}
	}

	if node, held, err := graphstore.ReadNode(ctx, rig.database.Pool, refs.BlockNodeKind, rig.otherIssueCheckNode); err != nil || !held || node.State != graphstore.StateGone {
		t.Fatalf("after the pass committed the check %s reads %+v (held %t, %v), want it gone", rig.otherIssueCheckNode, node, held, err)
	}
	if held := rig.current(graphstore.KindRisk, risk.ID, graphstore.TuplePreventedBy); len(held) != 0 {
		t.Errorf("after the index pass marked %s gone, the risk %s is prevented by %v, a check no version holds "+
			"(the append answered %d)", rig.otherIssueCheck, risk.Address, held, appended.Code)
	}
	// The route reads a check no current version holds as a far end that does not exist
	// (pointerEnd, nodeNotFound), so the append that waited on the pass and then found its check
	// gone is the 404 such a far end gets, naming the check by the address the append gave.
	requireRefused(t, "the append to "+rig.otherIssueCheck+", a check the index pass marked gone", appended,
		http.StatusNotFound, "NOT_FOUND", fmt.Sprintf("no node %q under %s", rig.otherIssueCheck, rig.issue))
}
