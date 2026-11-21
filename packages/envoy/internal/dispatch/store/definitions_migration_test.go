package store

import (
	"context"
	"strings"
	"testing"
)

const rewritesAndDefinitionsVersion = 20261119050006

// 20261119050006 indexes the two columns an ask's delete and a document's removal cascade to the
// definitions by, ask_id and artifact_id, for the rows that name one: the cascade's lookup of the
// definitions to delete reads the index, not the whole table.
func TestMigrationIndexesTheDefinitionsCascadingColumns(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	if err := store.migrate(ctx, migrationsThrough(t, rewritesAndDefinitionsVersion)); err != nil {
		t.Fatalf("migrate through %d: %v", rewritesAndDefinitionsVersion, err)
	}
	tx, err := store.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	// A table this small reads fastest whole, so the plan is asked with the scan of the whole
	// table priced out: what remains is the index the cascade can read, or that scan where there
	// is none.
	if _, err := tx.Exec(ctx, `set local enable_seqscan = off`); err != nil {
		t.Fatal(err)
	}
	for column, index := range map[string]string{"ask_id": "definitions_ask_id", "artifact_id": "definitions_artifact_id"} {
		var plan strings.Builder
		rows, err := tx.Query(ctx, `explain (costs off) delete from definitions where `+column+` = '00000000-0000-0000-0000-000000000001'`)
		if err != nil {
			t.Fatalf("explain the delete by %s: %v", column, err)
		}
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatal(err)
			}
			plan.WriteString(line + "\n")
		}
		rows.Close()
		if !strings.Contains(plan.String(), index) {
			t.Errorf("the delete of the definitions by %s reads no index %s:\n%s", column, index, plan.String())
		}
	}
	var predicates []string
	if err := tx.QueryRow(ctx, `
		select array_agg(pg_get_expr(i.indpred, i.indrelid) order by c.relname)
		from pg_index i join pg_class c on c.oid = i.indexrelid
		where c.relname in ('definitions_ask_id', 'definitions_artifact_id')`).Scan(&predicates); err != nil {
		t.Fatalf("read the indexes' predicates: %v", err)
	}
	if want := []string{"(artifact_id IS NOT NULL)", "(ask_id IS NOT NULL)"}; len(predicates) != 2 || predicates[0] != want[0] || predicates[1] != want[1] {
		t.Errorf("the indexes' predicates = %q, want %q: each holds the rows that name one", predicates, want)
	}
}
