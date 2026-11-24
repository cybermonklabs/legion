package schemanodes

import (
	"context"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A migration that commits while a Read is under way is read whole by the next Read and not at
// all by this one. Read makes one query for the attribute nodes, one for the supertag nodes and
// one per supertag for its tuples, so a migration committing between the first two - a new type
// with one attribute, tied to it by its tuple - would give reads outside one snapshot the type
// with its supertag and its tuple but without the attribute node read before it: a type of no
// attributes, which no migration ever wrote. Read's one repeatable-read transaction reads all
// three at the snapshot of its first query.
func TestAMigrationCommittingDuringAReadIsReadWholeOrNotAtAll(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	pool := database.Pool
	const writer = `{"kind": "system", "id": "schemanodes-snapshot-test"}`
	migrated := false
	afterAttributeNodesRead = func() {
		if migrated {
			return
		}
		migrated = true
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Errorf("begin the migration: %v", err)
			return
		}
		defer func() { _ = tx.Rollback(context.Background()) }()
		for _, statement := range []string{
			`insert into graph_nodes (kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event)
			values
				('supertag', 'claim', 'supertag', 'project', 'live',
					jsonb_build_object('block_type', true, 'content', 'paragraph+', 'render', 'host',
						'version', 1, 'position', 99),
					now(), $1::jsonb, 0),
				('attribute', 'claim/supports', 'attribute', 'project', 'live',
					jsonb_build_object('name', 'supports', 'store_name', 'supports', 'position', 0, 'kind', 'string'),
					now(), $1::jsonb, 0)`,
			`insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted)
			values ('attribute', 'claim/supports', 'attribute_of', 'supertag', 'claim', now(), $1::jsonb, 0, null, true)`,
		} {
			if _, err := tx.Exec(ctx, statement, writer); err != nil {
				t.Errorf("write the migration: %v", err)
				return
			}
		}
		if err := tx.Commit(ctx); err != nil {
			t.Errorf("commit the migration: %v", err)
		}
	}
	t.Cleanup(func() { afterAttributeNodesRead = nil })

	during, err := Read(ctx, pool)
	if err != nil {
		t.Fatalf("read the schema while the migration commits: %v", err)
	}
	if !migrated {
		t.Fatalf("the migration never ran: Read made no read after its first")
	}
	if claim, held := typeOf(during, "claim"); held {
		t.Errorf("the read during which the migration committed holds the claim type with attributes %v; want no claim type at all, the schema before the migration: one read reads one snapshot",
			attributeNames(claim))
	}

	after, err := Read(ctx, pool)
	if err != nil {
		t.Fatalf("read the schema after the migration: %v", err)
	}
	claim, held := typeOf(after, "claim")
	if !held || !slices.Equal(attributeNames(claim), []string{"supports"}) {
		t.Errorf("the read after the migration holds the claim type %t with attributes %v, want it with supports", held, attributeNames(claim))
	}
}

func typeOf(value pmdoc.BlockSchema, name string) (pmdoc.BlockTypeSchema, bool) {
	index := slices.IndexFunc(value.Types, func(typ pmdoc.BlockTypeSchema) bool { return typ.Name == name })
	if index < 0 {
		return pmdoc.BlockTypeSchema{}, false
	}
	return value.Types[index], true
}

func attributeNames(typ pmdoc.BlockTypeSchema) []string {
	var names []string
	for _, attribute := range typ.Attributes {
		names = append(names, attribute.Name)
	}
	return names
}
