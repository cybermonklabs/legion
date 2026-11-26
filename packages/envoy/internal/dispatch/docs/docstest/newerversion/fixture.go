package newerversion

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// StoreNewerVersion stages a later build's version and live tree without using this build's
// document write routes. The fixture's callers use fresh documents with uncompacted histories.
func StoreNewerVersion(t *testing.T, database interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}, id, markdown string) int {
	t.Helper()
	ctx := t.Context()
	tree, err := pmdoc.ParseRendering(markdown)
	if err != nil {
		t.Fatalf("parse the later build's rendering: %v", err)
	}
	markdown, err = pmdoc.Render(tree)
	if err != nil {
		t.Fatalf("render the later build's version: %v", err)
	}
	tx, err := database.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	var compacted bool
	if err := tx.QueryRow(ctx, `select exists (select 1 from doc_checkpoints where artifact_id = $1)`, id).Scan(&compacted); err != nil || compacted {
		t.Fatalf("read uncompacted fixture history: checkpoint %t, error %v", compacted, err)
	}
	doc := crdt.New()
	rows, err := tx.Query(ctx, `select update from doc_updates where artifact_id = $1 order by version`, id)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var update []byte
		if err := rows.Scan(&update); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		if err := crdt.ApplyUpdateV1(doc, update, nil); err != nil {
			rows.Close()
			t.Fatalf("fold the fixture history: %v", err)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	before := doc.StateVector()
	fragment := doc.GetXmlFragment("prosemirror")
	if err := doc.TransactE(func(txn *crdt.Transaction) error {
		return pmdoc.UpdateHolding(txn, fragment, tree, tree)
	}); err != nil {
		t.Fatalf("encode the later build's update: %v", err)
	}
	var number int
	if err := tx.QueryRow(ctx, `
		with updated as (
			insert into doc_updates (artifact_id, version, update, content_changed)
			values ($1, (select coalesce(max(version), 0) + 1 from doc_updates where artifact_id = $1), $2, true)
			returning version
		)
		insert into artifact_versions (artifact_id, number, markdown, authors, doc_update_version)
		select $1, (select coalesce(max(number), 0) + 1 from artifact_versions where artifact_id = $1), $3, '[]', version from updated
		returning number
	`, id, crdt.EncodeStateAsUpdateV1(doc, before), markdown).Scan(&number); err != nil {
		t.Fatalf("store the later build's version: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return number
}
