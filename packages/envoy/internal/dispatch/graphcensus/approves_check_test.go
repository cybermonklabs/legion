package graphcensus

import (
	"context"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest/lockstep"
)

// c-approval-bytes, the census's side (R14 of the issue-tree spec). What a request asks approval
// for is a tuple: one approves tuple out of the request's ask node to each query version node it
// names, and the asks row's approval.documents is their projection for good. The census kind
// approves holds each entry of every approval ask's documents to the tuples its node states, in
// both directions, and a bounded pass of the backfill states the tuples of a row the release
// before wrote, which names no node until the asks' pass gives it one.

// approvalRequest is the id of the approval ask the census test seeds.
const approvalRequest = "a0000000-0000-4000-8000-000000000001"

// seedApprovalRequest writes issue LOCK-1 and its spec (seedLockSpec), then an approval request of
// the spec's version 1 as the release before this one writes it: the four top-level fields of the
// approval and no documents array, which asks_normalize_approval_documents writes, and no node. It
// answers the spec's id and its ref_key.
func seedApprovalRequest(t *testing.T, database *store.Store) (document, refKey string) {
	t.Helper()
	document, refKey, _ = seedLockSpec(t, database)
	ctx := context.Background()
	drivenTx(t, database, "write the approval request as the release before writes it", func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			insert into asks (id, issue_key, author, question, kind, approval)
			values ($1::uuid, 'LOCK-1', '{"kind":"session","id":"ses_requester"}', 'Approve spec.md (version 1)?', 'approval',
			        jsonb_build_object('artifact_id', $2::text, 'name', 'spec.md', 'version', 1, 'requested_version', 1))
		`, approvalRequest, document)
		return err
	})
	var documents int
	if err := database.Pool.QueryRow(ctx, `select jsonb_array_length(approval -> 'documents') from asks where id = $1::uuid`, approvalRequest).Scan(&documents); err != nil || documents != 1 {
		t.Fatalf("the row the release before writes holds %d documents (%v), want the one asks_normalize_approval_documents writes", documents, err)
	}
	return document, refKey
}

// approvesOf is the version nodes the node of the approval request states an approves tuple to now,
// in order, read as every reader of a current relation reads it (graphstore.CurrentOut).
func approvesOf(t *testing.T, database *store.Store) []string {
	t.Helper()
	held, err := graphstore.CurrentOut(context.Background(), database.Pool, graphstore.KindAsk, approvalRequest, []string{TupleApproves})
	if err != nil {
		t.Fatalf("read the approves tuples of the request %s: %v", approvalRequest, err)
	}
	var versions []string
	for _, tuple := range held {
		if tuple.ToKind != graphstore.KindVersion {
			t.Errorf("the request %s states an approves tuple to a %s node %s, want a version node", approvalRequest, tuple.ToKind, tuple.ToID)
		}
		versions = append(versions, tuple.ToID)
	}
	return versions
}

// requireApprovesDifference fails t unless the census kind approves reads at least one difference
// naming the request while broken holds.
func requireApprovesDifference(t *testing.T, database *store.Store, broken string) {
	t.Helper()
	found, reported, _ := censusDifferences(t, database, TupleApproves)
	named := slices.ContainsFunc(reported, func(difference Difference) bool {
		return strings.Contains(difference.At, approvalRequest) || strings.Contains(difference.Cause, approvalRequest)
	})
	if found == 0 || !named {
		t.Errorf("with %s the census reads %d difference(s) of kind %s, %v, want the ones naming the request %s",
			broken, found, TupleApproves, reported, approvalRequest)
	}
}

// requireApprovesAgrees fails t unless the census kind approves reads no difference, over at least
// one row, after the step after names.
func requireApprovesAgrees(t *testing.T, database *store.Store, after string) {
	t.Helper()
	found, reported, rows := censusDifferences(t, database, TupleApproves)
	if found != 0 || rows == 0 {
		t.Errorf("after %s the census reads %d difference(s) of kind %s, %v, over %d rows, want none over the request %s's entry",
			after, found, TupleApproves, reported, rows, approvalRequest)
	}
}

// TestCensusHoldsEveryApprovalRequestsDocumentsToItsApprovesTuples is c-approval-bytes' census case
// (R14, R21). It seeds an approval request of version 1 of LOCK-1's spec as the release before
// writes it. The census kind approves reads no difference before the backfill, the row naming no
// node, and the backfill, after the asks' pass gave the request its node, states the one approves
// tuple to the version node `<ref_key>@v1`, so the census reads no difference over one row. A move
// of the row to version 2 by SQL, as a process of the release before moves it, is a difference in
// both directions, and the next pass asserts the tuple to version 2 and retracts the one to version
// 1, so the census reads zero. A retraction of the tuple by the store's own writer is a difference
// too, which the next pass states again.
func TestCensusHoldsEveryApprovalRequestsDocumentsToItsApprovesTuples(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	document, refKey := seedApprovalRequest(t, database)
	backfill := func(over string) {
		t.Helper()
		if _, err := Backfill(ctx, database, io.Discard); err != nil {
			t.Fatalf("run the backfill over %s: %v", over, err)
		}
	}
	requireVersions := func(what string, want ...string) {
		t.Helper()
		if got := approvesOf(t, database); !slices.Equal(got, want) {
			t.Errorf("%s the request's node states approves tuples to %v, want %v", what, got, want)
		}
	}

	// A row the release before wrote, which names no node: counted as a class of its own, no difference.
	if found, reported, _ := censusDifferences(t, database, TupleApproves); found != 0 {
		t.Errorf("before the backfill the census reads %d difference(s) of kind %s, %v, want none: a row naming no node is a class of its own", found, TupleApproves, reported)
	}
	backfill("the approval request the release before wrote")
	requireVersions("after the backfill", graphstore.VersionID(refKey, 1))
	requireApprovesAgrees(t, database, "the backfill over the request the release before wrote")

	// The row moved to version 2 by a process of the release before: the tuple names the version it left.
	if _, err := database.Pool.Exec(ctx, `
		update asks set approval = jsonb_set(jsonb_set(approval, '{documents,0,version}', '2'), '{version}', '2') where id = $1::uuid
	`, approvalRequest); err != nil {
		t.Fatalf("move the request %s of document %s to version 2: %v", approvalRequest, document, err)
	}
	requireApprovesDifference(t, database, "the row naming version 2 while its node approves version 1")
	backfill("the row moved to version 2")
	requireVersions("after the backfill over the row moved to version 2", graphstore.VersionID(refKey, 2))
	requireApprovesAgrees(t, database, "the backfill over the row moved to version 2")

	// The tuple retracted by the store's own writer: the row names a version its node no longer approves.
	drivenTx(t, database, "retract the request's approves tuple", func(tx pgx.Tx) error {
		return graphstore.Write(ctx, tx, nil, []graphstore.Tuple{{
			FromKind: graphstore.KindAsk, FromID: approvalRequest, Kind: TupleApproves,
			ToKind: graphstore.KindVersion, ToID: graphstore.VersionID(refKey, 2),
			At: time.Now().UTC(), Who: backfillActor, SourceEvent: graphstore.NoEvent, Asserted: false,
		}})
	})
	requireApprovesDifference(t, database, "the tuple retracted while the row names version 2")
	backfill("the retracted tuple")
	requireVersions("after the backfill over the retracted tuple", graphstore.VersionID(refKey, 2))
	requireApprovesAgrees(t, database, "the backfill over the retracted tuple")

	// A second run reads no request a first one levelled.
	report, err := Backfill(ctx, database, io.Discard)
	if err != nil {
		t.Fatalf("run the backfill again: %v", err)
	}
	if pass := reportedKind(t, report, TupleApproves); pass.Rows != 0 || pass.Tuples != 0 || !pass.Complete {
		t.Errorf("a second backfill over a levelled request reads %+v of kind %s, want nothing owed", pass, TupleApproves)
	}
}

// seedApprovalAsk writes request, an approval request of issue in project naming version 1 of the
// document whose id is document and whose name is name, as this build writes one: its node first,
// made by the requesting session at createdAt, then its row naming that node. Its node states no
// approves tuple, as a process of the release before leaves a request it moves.
func seedApprovalAsk(t *testing.T, database *store.Store, project, issue, document, name, request string, createdAt time.Time) {
	t.Helper()
	ctx := context.Background()
	drivenTx(t, database, "write the approval request with its node", func(tx pgx.Tx) error {
		node := graphstore.AskNode(request, project, nil, model.Actor{Kind: "session", ID: "ses_requester"}, createdAt)
		if err := graphstore.Write(ctx, tx, []graphstore.Node{node}, nil); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			insert into asks (id, issue_key, author, question, kind, approval, created_at, node_kind, node_address)
			values ($1::uuid, $2, '{"kind":"session","id":"ses_requester"}', 'Approve ' || $3::text || ' (version 1)?', 'approval',
			        jsonb_build_object('artifact_id', $4::text, 'name', $3::text, 'version', 1, 'requested_version', 1),
			        $5, $6, $1::text)
		`, request, issue, name, document, createdAt, graphstore.KindAsk)
		return err
	})
}

// TestTheApprovesBatchTakesTheStoreLockOrder holds writeApprovesBatch to the store's lock order, as
// TestTheAnchorBatchTakesTheStoreLockOrder holds the anchor batch: the owner row the request
// stands under, its issue's row, first, then its approves tuple, which is stated at the sentinel
// that no repeat key holds, so no hold stands on it, and the batch takes no project row. The
// request is one this build wrote with its node, naming version 1 of LOCK-1's spec in its row
// whose node states no approves tuple, as a process of the release before leaves a request it moves.
// After the run the node states one approves tuple to the version node `<ref_key>@v1`, dated at the
// row's created_at, and the backfill reports the request levelled with that one tuple.
func TestTheApprovesBatchTakesTheStoreLockOrder(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	document, refKey, _ := seedLockSpec(t, database)
	createdAt := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Microsecond)
	seedApprovalAsk(t, database, "LOCK", "LOCK-1", document, "spec.md", approvalRequest, createdAt)

	owner, project := holdLockOwner(t, database)
	report := followTheRun(t, database, "writeApprovesBatch", onePass(backfillApproves), []*lockstep.Hold{owner, project})
	if got, want := approvesOf(t, database), []string{graphstore.VersionID(refKey, 1)}; !slices.Equal(got, want) {
		t.Errorf("after a run the test followed that committed, the request's node states approves tuples to %v, want %v", got, want)
	}
	var at time.Time
	if err := database.Pool.QueryRow(ctx, `
		select at from graph_tuples where from_kind = $1 and from_id = $2 and kind = $3
	`, graphstore.KindAsk, approvalRequest, TupleApproves).Scan(&at); err != nil || !at.Equal(createdAt) {
		t.Errorf("the approves tuple is dated %v (%v), want the request's created_at %v", at, err, createdAt)
	}
	if done := reportedKind(t, report, TupleApproves); done.Rows != 1 || done.Tuples != 1 || !done.Complete {
		t.Errorf("the backfill reports %#v for approves tuples, want 1 request, its one tuple, and none left", done)
	}
}
