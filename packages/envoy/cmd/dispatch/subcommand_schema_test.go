package main

import (
	"bytes"
	"context"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc/pmdoctest"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The subcommands that parse, render or write documents hold them to the block schema the store's
// type nodes build, as the server does, not to the file this binary embeds: backfill-block-ids,
// backfill-anchor-blocks, backfill-graph-store and rebuild-refs open their database through
// openMigrated, which installs the store's schema after its migration run, and the erase installs
// it once it has connected (grapherase.Erase). The store here declares a typed block, claim, that
// the file does not, so a command holds claim as a typed block only where it read the store.
func TestTheDocumentSubcommandsInstallTheStoresSchema(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	databaseURL := database.Pool.Config().ConnString()
	file := decodeSchema(t, pmdoc.SchemaJSON())
	if _, err := database.Pool.Exec(ctx, `
		insert into graph_nodes (kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event)
		values ('supertag', 'claim', 'supertag', 'project', 'live',
			jsonb_build_object('block_type', true, 'content', 'paragraph+', 'render', 'host',
				'version', $1::int, 'position', 99),
			now(), '{"kind": "system", "id": "subcommand-schema-test"}'::jsonb, 0)`, pmdoc.BuiltSchemaVersion()); err != nil {
		t.Fatalf("add the claim type to the store: %v", err)
	}

	// Each run starts from the file's schema, which the hook also puts back once the test is over.
	pmdoctest.UseBlockSchema(t, file)
	var out bytes.Buffer
	opened, ok := openMigrated(ctx, "backfill-block-ids", databaseURL, &out)
	if !ok {
		t.Fatalf("open the database as a document subcommand does: %s", out.String())
	}
	opened.Pool.Close()
	if !pmdoc.IsTypedBlock("claim") {
		t.Errorf("a subcommand opened its database through openMigrated and holds no claim type: it reads documents against the file, not the store")
	}

	pmdoctest.UseBlockSchema(t, file)
	var listing, refusal bytes.Buffer
	// The node is one the store does not hold; what the erase answers of it is not this test's
	// concern, only that it read the store's schema before it read anything else.
	code := erase(ctx, []string{"--node", "comment:00000000-0000-0000-0000-000000000000"}, databaseURL, &listing, &refusal)
	if !pmdoc.IsTypedBlock("claim") {
		t.Errorf("the erase ran (exit %d: %s%s) and holds no claim type: it reads documents against the file, not the store",
			code, listing.String(), refusal.String())
	}
}
