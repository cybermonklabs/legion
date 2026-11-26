package graphcensus

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

func seedDocumentSearch(t *testing.T, database *store.Store) string {
	t.Helper()
	ctx := t.Context()
	var id, key string
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `insert into projects (key, name, default_assignee) values ('DSR', 'Search census', 'alice')`); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			insert into artifacts (project_key, kind, slug, name, created_by)
			values ('DSR', 'doc', 'field-md', 'field.md', '{"kind":"user","id":"alice"}') returning id::text, ref_key`).Scan(&id, &key); err != nil {
			return err
		}
		node := graphstore.ArtifactNode(key, "DSR", "", model.Actor{Kind: "user", ID: "alice"}, time.Now().UTC())
		if err := graphstore.Write(ctx, tx, []graphstore.Node{node}, nil); err != nil {
			return err
		}
		// This is an older writer: its own pass stood the block index at version 2, which migration
		// 0122's trigger left owed at the insert, and neither stated the search row.
		if _, err := tx.Exec(ctx, `insert into artifact_versions (artifact_id, number, markdown, authors)
			values ($1, 1, 'Theodolite calibration', '[]'), ($1, 2, 'New astrolabe readings', '[]')`, id); err != nil {
			return err
		}
		stood, err := tx.Exec(ctx, `update document_block_versions set version = 2 where artifact_id = $1`, id)
		if err == nil && stood.RowsAffected() != 1 {
			err = fmt.Errorf("stood the block index of %d documents, want the one seeded", stood.RowsAffected())
		}
		return err
	}); err != nil {
		t.Fatalf("seed an unprojected document: %v", err)
	}
	return key
}

func readDocumentSearchCensus(t *testing.T, database *store.Store) KindReport {
	t.Helper()
	ctx := t.Context()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	for _, projection := range searchProjections {
		if projection.name == documentSearchNodeKind {
			report, err := censusOfKind(ctx, tx, projection, nil)
			if err != nil {
				t.Fatalf("read %s: %v", projection.name, err)
			}
			return report
		}
	}
	t.Fatalf("no projection registered for %s", documentSearchNodeKind)
	return KindReport{}
}

func TestDocumentSearchCensusFindsDifferencesBothWays(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	key := seedDocumentSearch(t, database)
	if _, err := database.Pool.Exec(t.Context(), `
		insert into node_search (node_kind, node_address, text, text_seq, search)
		values ('artifact', 'DSR/extra-md', '', 1, to_tsvector('english', 'extra'))`); err != nil {
		t.Fatal(err)
	}
	// A node difference is named at the node's kind and id.
	kind := readDocumentSearchCensus(t, database)
	if kind.RowDifferences != 1 || len(kind.ReportedRows) != 1 || !strings.Contains(kind.ReportedRows[0].At, "DSR/extra-md") ||
		kind.NodeDifferences != 1 || len(kind.ReportedNodes) != 1 || kind.ReportedNodes[0].At != graphstore.KindArtifact+" "+key {
		t.Fatalf("%s: %+v; want the extra search row and the missing row of %s", kind.Kind, kind, key)
	}
	// A document that owes its block index is on neither side: the pass that indexes it writes its
	// row, so the missing row is no difference until that pass has run.
	if _, err := database.Pool.Exec(t.Context(), `
		update document_block_versions set version = null
		where artifact_id = (select id from artifacts where ref_key = $1)`, key); err != nil {
		t.Fatal(err)
	}
	if owed := readDocumentSearchCensus(t, database); owed.RowDifferences != 1 || owed.NodeDifferences != 0 || owed.Nodes != 0 {
		t.Fatalf("%s with %s owing its block index: %+v; want the extra search row alone and no node", owed.Kind, key, owed)
	}
}

func TestBackfillDocumentSearchRepairsTheVector(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	key := seedDocumentSearch(t, database)
	ctx := t.Context()
	for _, stage := range []string{"missing", "wrong vector at the same version"} {
		if stage != "missing" {
			if _, err := database.Pool.Exec(ctx, `update node_search set search = ''::tsvector
				where node_kind = 'artifact' and node_address = $1`, key); err != nil {
				t.Fatal(err)
			}
			if kind := readDocumentSearchCensus(t, database); kind.RowDifferences != 1 {
				t.Fatalf("the corrupted vector did not produce one row difference: %+v", kind)
			}
		}
		filled, err := backfillDocumentSearch(ctx, database)
		if err != nil || !filled.Complete || filled.Rows != 1 {
			t.Fatalf("backfill (%s): %+v, %v; want one row and no remaining work", stage, filled, err)
		}
		var matched bool
		if err := database.Pool.QueryRow(ctx, `
			select s.text_seq = 2 and s.text = '' and s.search = search_vector('', search_document_text('New astrolabe readings'))
			from node_search s where s.node_kind = 'artifact' and s.node_address = $1`, key).Scan(&matched); err != nil || !matched {
			t.Fatalf("read %s at its latest text after backfill (%s): matched=%t, %v", key, stage, matched, err)
		}
	}
	if kind := readDocumentSearchCensus(t, database); kind.Rows != 1 || kind.Nodes != 1 || kind.RowDifferences != 0 || kind.NodeDifferences != 0 {
		t.Fatalf("after backfill the census reads %+v; want one document and no differences", kind)
	}
	if again, err := backfillDocumentSearch(ctx, database); err != nil || !again.Complete || again.Rows != 0 {
		t.Fatalf("second backfill: %+v, %v; want no work", again, err)
	}
}
