package graphcensus

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest/lockstep"
)

// TestTheVersionBatchTakesTheStoreLockOrder holds writeVersionBatch (dispatch://INFRA-1253) to the
// store's lock order, as TestEveryBackfillBatchTakesTheStoreLockOrder holds the other batches: the
// owner row its version's document stands under, the issue's row, first, then the version's node
// row. A version's row names no node in a column of its own, so the batch writes no projection row.
// The document and its issue are this build's, so no pass but the version pass reads a row of
// them; the project's row is held throughout, and no batch here takes it.
func TestTheVersionBatchTakesTheStoreLockOrder(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	seedLockProject(t, database)
	alice := model.Actor{Kind: "user", ID: "alice"}
	const refKey = "LOCK-1/notes-md"
	var document string
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		now := time.Now().UTC()
		if err := graphstore.Write(ctx, tx, []graphstore.Node{
			graphstore.IssueNode("LOCK-1", "LOCK", alice, now),
			graphstore.ArtifactNode(refKey, "LOCK", "", alice, now),
		}, []graphstore.Tuple{graphstore.ArtifactPartOf(refKey, "LOCK-1", alice, now)}); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			insert into issues (key, project_key, number, title, created_by, rank, node_kind, node_address)
			values ('LOCK-1', 'LOCK', 1, 'One', '{"kind":"user","id":"alice"}', 'U', 'issue', 'LOCK-1')
		`); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			insert into artifacts (issue_key, project_key, slug, name, kind, created_by, node_kind, node_address)
			values ('LOCK-1', 'LOCK', 'notes-md', 'notes.md', 'doc', '{"kind":"user","id":"alice"}', 'artifact', $1)
			returning id::text
		`, refKey).Scan(&document)
	}); err != nil {
		t.Fatalf("seed the issue and its document: %v", err)
	}
	// An older build's version: its row, and no node.
	var at time.Time
	if err := database.Pool.QueryRow(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, authors)
		values ($1, 1, 'Written by an older build.', '[{"kind":"user","id":"alice"}]')
		returning created_at
	`, document).Scan(&at); err != nil {
		t.Fatalf("seed the older build's version: %v", err)
	}

	owner, project := holdLockOwner(t, database)
	versionNode, _, _ := graphstore.VersionFacts(nil, graphstore.VersionOwner{RefKey: refKey, Project: "LOCK"}, 1, alice, at)
	node := holdWrite(t, database, &lockstep.Hold{
		Name: "the version's node row", Step: lockstep.StepNode, Taken: true, Kind: graphstore.KindVersion,
		ID: versionNode.ID, Statement: "with incoming as (",
	}, []graphstore.Node{versionNode}, nil)

	report := followTheBackfill(t, database, "writeVersionBatch", []*lockstep.Hold{owner, project, node})
	var partOf int
	if err := database.Pool.QueryRow(ctx, `
		select count(*) from graph_tuples
		where from_kind = $1 and from_id = $2 and kind = $3 and to_kind = $4 and to_id = $5 and asserted
	`, graphstore.KindVersion, versionNode.ID, graphstore.TuplePartOf, graphstore.KindArtifact, refKey).Scan(&partOf); err != nil {
		t.Fatalf("read the version's part_of: %v", err)
	}
	if partOf != 1 {
		t.Errorf("the version's node states %d part_of tuple(s) to its document, want one: the run the test followed is not one that committed", partOf)
	}
	if done := reportedKind(t, report, graphstore.KindVersion); done.Rows != 1 || !done.Complete {
		t.Errorf("the backfill reports %#v for versions, want 1 row and none remaining", done)
	}
}

// TestTheVersionBackfillRestatesAGoneNodeARowAtItsAddressOwes closes the rolling deploy of
// dispatch://INFRA-1253: an erase took a document's newest version, stating its node gone in the
// transaction that deleted its row, and an older build, which writes no version node, then wrote
// the next version at the freed number. That row stands at the address of a node held gone, which
// the census reads as a row naming a node the store holds gone; the backfill counts it owed
// (presentVersionNode) and restates the node live with the row's number and created_at, after
// which the census reads zero and a second run writes nothing.
func TestTheVersionBackfillRestatesAGoneNodeARowAtItsAddressOwes(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	alice := model.Actor{Kind: "user", ID: "alice"}
	const refKey = "FREE-1/notes-md"
	owner := graphstore.VersionOwner{RefKey: refKey, Project: "FREE"}
	var document string
	var node graphstore.Node
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		now := time.Now().UTC()
		if _, err := tx.Exec(ctx, `insert into projects (key, name, default_assignee) values ('FREE', 'Freed', 'alice')`); err != nil {
			return err
		}
		if err := graphstore.Write(ctx, tx, []graphstore.Node{
			graphstore.IssueNode("FREE-1", "FREE", alice, now),
			graphstore.ArtifactNode(refKey, "FREE", "", alice, now),
		}, []graphstore.Tuple{graphstore.ArtifactPartOf(refKey, "FREE-1", alice, now)}); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			insert into issues (key, project_key, number, title, created_by, rank, node_kind, node_address)
			values ('FREE-1', 'FREE', 1, 'One', '{"kind":"user","id":"alice"}', 'U', 'issue', 'FREE-1')
		`); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			insert into artifacts (issue_key, project_key, slug, name, kind, created_by, node_kind, node_address)
			values ('FREE-1', 'FREE', 'notes-md', 'notes.md', 'doc', '{"kind":"user","id":"alice"}', 'artifact', $1)
			returning id::text
		`, refKey).Scan(&document); err != nil {
			return err
		}
		var at time.Time
		if err := tx.QueryRow(ctx, `
			insert into artifact_versions (artifact_id, number, markdown, authors)
			values ($1, 1, 'Written by this build.', '[{"kind":"user","id":"alice"}]')
			returning created_at
		`, document).Scan(&at); err != nil {
			return err
		}
		var facts []graphstore.Tuple
		node, facts, _ = graphstore.VersionFacts(nil, owner, 1, alice, at)
		return graphstore.Write(ctx, tx, []graphstore.Node{node}, facts)
	}); err != nil {
		t.Fatalf("seed the document and its first version's node: %v", err)
	}
	// The erase of the version's text: its node stated gone, its row deleted, in one transaction.
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		gone := node
		gone.State, gone.MadeAt = graphstore.StateGone, time.Now().UTC()
		if err := graphstore.Write(ctx, tx, []graphstore.Node{gone}, nil); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `delete from artifact_versions where artifact_id = $1 and number = 1`, document)
		return err
	}); err != nil {
		t.Fatalf("end the first version as an erase does: %v", err)
	}
	// An older build's next version, at the freed number, with no node of its own.
	var at time.Time
	if err := database.Pool.QueryRow(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, authors)
		values ($1, 1, 'Written by an older build.', '[{"kind":"user","id":"alice"}]')
		returning created_at
	`, document).Scan(&at); err != nil {
		t.Fatalf("write the older build's version at the freed number: %v", err)
	}
	if versions := censusKind(t, storeCensusOf(t, database), graphstore.KindVersion); versions.RowDifferences != 1 {
		t.Fatalf("before the backfill the census reads %d version row difference(s) %+v, want the one row at the gone node's address",
			versions.RowDifferences, versions.ReportedRows)
	}

	report, err := Backfill(ctx, database, io.Discard)
	if err != nil {
		t.Fatalf("run the backfill: %v", err)
	}
	if done := reportedKind(t, report, graphstore.KindVersion); done.Rows != 1 || !done.Complete {
		t.Errorf("the backfill reports %#v for versions, want the row at the freed address written and none remaining", done)
	}
	id := graphstore.VersionID(refKey, 1)
	restated, held, err := graphstore.ReadNode(ctx, database.Pool, graphstore.KindVersion, id)
	if err != nil || !held || restated.State != graphstore.StateLive {
		t.Errorf("after the backfill the store holds %s as %+v (held %t, %v), want it live again", id, restated, held, err)
	}
	if number, stated, err := graphstore.CurrentValue(ctx, database.Pool, graphstore.KindVersion, id, graphstore.AttributeNumber); err != nil || !stated || number != "1" {
		t.Errorf("after the backfill %s states number %q (stated %t, %v), want 1", id, number, stated, err)
	}
	created, stated, err := graphstore.CurrentValue(ctx, database.Pool, graphstore.KindVersion, id, graphstore.AttributeCreatedAt)
	if err != nil || !stated {
		t.Fatalf("read the created_at of %s: stated %t, %v", id, stated, err)
	}
	if value, err := time.Parse(time.RFC3339Nano, created); err != nil || !value.Equal(at) {
		t.Errorf("after the backfill %s states created_at %q (%v), want the older build's row's %s", id, created, err, at)
	}
	if after := storeCensusOf(t, database); after.Differences() != 0 {
		var written bytes.Buffer
		after.Write(&written)
		t.Errorf("after the backfill the census reads %d difference(s):\n%s", after.Differences(), written.String())
	}
	again, err := Backfill(ctx, database, io.Discard)
	if err != nil {
		t.Fatalf("run the backfill again: %v", err)
	}
	if done := reportedKind(t, again, graphstore.KindVersion); done.Rows != 0 || done.Tuples != 0 || !done.Complete {
		t.Errorf("the second run reports %#v for versions, want nothing written and nothing remaining", done)
	}
}
