package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/files/filestest"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc/pmdoctest"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The subcommands that parse, render or write documents hold them to the block schema the store's
// type nodes build, as the server does, not to the file this binary embeds: backfill-block-ids,
// backfill-anchor-blocks, backfill-graph-store and rebuild-refs open their database through
// openMigrated, which installs the store's schema after its migration run. The store here
// declares a typed block, claim, that the file does not, so a command holds claim as a typed block
// only where it read the store.
func TestTheDocumentSubcommandsInstallTheStoresSchema(t *testing.T) {
	// Not parallel: it installs pmdoc's process-wide block schema.
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
}

// backfill-files moves file bytes and parses no document, so it opens its database without the
// block schema (openMigratedWithoutBlockSchema) and runs on a store whose types this build cannot
// install, where a document subcommand refuses to start. The store here declares a typed block,
// claim, with a render no build serves, so the install refuses the store's schema: the document
// subcommand's open shows that refusal, and backfill-files then moves the row's bytes all the same.
func TestBackfillFilesRunsOnAStoreWhoseSchemaThisBuildCannotInstall(t *testing.T) {
	// Not parallel: it sets environment variables for the whole process; and it installs pmdoc's
	// process-wide block schema.
	ctx := context.Background()
	database := storetest.Open(t)
	databaseURL := database.Pool.Config().ConnString()
	if _, err := database.Pool.Exec(ctx, `
		insert into graph_nodes (kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event)
		values ('supertag', 'claim', 'supertag', 'project', 'live',
			jsonb_build_object('block_type', true, 'content', 'paragraph+', 'render', 'inline',
				'version', $1::int, 'position', 99),
			now(), '{"kind": "system", "id": "subcommand-schema-test"}'::jsonb, 0)`, pmdoc.BuiltSchemaVersion()); err != nil {
		t.Fatalf("add a claim type no build renders to the store: %v", err)
	}
	// A refused install installs nothing; the hook puts the file's schema back should one install.
	pmdoctest.UseBlockSchema(t, decodeSchema(t, pmdoc.SchemaJSON()))

	var refusal bytes.Buffer
	if opened, ok := openMigrated(ctx, "backfill-block-ids", databaseURL, &refusal); ok {
		opened.Pool.Close()
		t.Fatalf("a document subcommand opened a store whose claim type no build renders: this build installs that schema, so the run below proves nothing")
	}
	if !strings.Contains(refusal.String(), "install the block schema from the store") {
		t.Fatalf("a document subcommand refused the store with %q, want the install's refusal", refusal.String())
	}

	const bucket = "example-files-bucket"
	filestest.ServeS3(t, bucket)
	seedBackfillFile(t, database)
	if code, out := runBackfillFiles(t, database, bucket); code != 0 || rowHoldsBytes(t, database, 1) {
		t.Fatalf("backfill-files on a store whose schema this build cannot install: exit %d, row holds bytes %t, want exit 0 with the row cleared:\n%s",
			code, rowHoldsBytes(t, database, 1), out)
	}
}
