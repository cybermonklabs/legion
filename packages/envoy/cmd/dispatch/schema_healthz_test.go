package main

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc/pmdoctest"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc/schemanodes"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// schemaHealthWait bounds the wait for a running process to read a schema write: the write
// announces itself on the schema channel at commit, and the process's installer reads the store
// again on that wake, so this is the listener's round trip with room for a loaded pod.
const schemaHealthWait = 30 * time.Second

// A store schema whose version is above the one this binary was built for is a store the next
// release migrated, and this process keeps serving over it (c-schema-holder): it installs the
// store at its own version, with the elements that version reads - the claim type the migration
// adds, read from version 1 - and without the one the newer version introduces, and GET /healthz
// answers 200 throughout, so a release that ships a type migration takes no process of the
// previous build out of service. The store is written by the test alone while the process serves,
// as a deployment's newer pod would write it, so what is read is the server's own wake, its own
// installer and its own probe.
func TestAStoreSchemaAboveTheBuildAnswersHealthzOK(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database := storetest.Open(t)
	server := startDispatchProcess(t, database.Pool.Config().ConnString())
	server.waitHealthy(t)
	built := pmdoc.BuiltSchemaVersion()
	if status, health := server.schemaHealth(t); status != http.StatusOK || !health.OK {
		t.Fatalf("/healthz before the store's schema moves = %d %+v, want 200 ok", status, health)
	}

	moveStoreAboveTheBuild(t, ctx, database.Pool, built)
	served := server.waitForServedSchema(t, "the claim type the store's migration added", func(served pmdoc.BlockSchema) bool {
		return servedType(served, "claim")
	})
	if served.Version != built {
		t.Errorf("with the store at version %d the process serves version %d, want this build's own %d", built+1, served.Version, built)
	}
	if servedType(served, "future") {
		t.Errorf("with the store at version %d the process serves the typed block future, which that version introduces", built+1)
	}
	status, health := server.schemaHealth(t)
	if status != http.StatusOK || !health.OK || !health.DB {
		t.Errorf("/healthz with the store's schema above this build = %d %+v, want 200 ok with the database answering", status, health)
	}
}

// A reinstall that moves what this process may create writes the numbers it serves now into its
// row of the server process registry, which every process's document socket admission reads
// (docs.readSchemaFleet), this process's own among them; a row left at its boot's numbers would
// admit the reconnects the switch's socket close brings against a schema the process no longer
// serves. The store's migration here moves, to creatable only from the next version, every element
// that gives this build its creatable number - each block type, attribute, choice and item whose
// since is that number, an item being an entry of its attribute's item_versions - and raises the
// store to that version: this build installs it at its own version and may no longer create
// those elements. The migration finds them by their since in the store, so it names none: which
// element is newest changes with blocks.json. The numbers the row must reach are the ones this
// build's own install of that store answers, read in the test's process.
func TestAReinstallRefreshesThisProcesssRegistryRow(t *testing.T) {
	// Not parallel: it installs pmdoc's process-wide block schema.
	ctx := context.Background()
	database := storetest.Open(t)
	server := startDispatchProcess(t, database.Pool.Config().ConnString())
	server.waitHealthy(t)
	built := pmdoc.BuiltSchemaVersion()
	before := registeredSchema(t, ctx, database.Pool)
	if before != (schemaNumbers{built, pmdoc.SchemaCreatable()}) {
		t.Fatalf("the process registered schema numbers %+v at boot, want this build's version %d and creatable %d", before, built, pmdoc.SchemaCreatable())
	}

	if _, err := database.Pool.Exec(ctx, `
		update graph_nodes
		set attributes = case when kind = 'supertag' then jsonb_set(attributes, '{version}', to_jsonb($1::int)) else attributes end
			|| case when coalesce((attributes->>'since')::int, 1) = $2::int then jsonb_build_object('creatable_since', $1::int) else '{}'::jsonb end
			|| case when kind = 'attribute' and jsonb_typeof(attributes->'choice_versions') = 'object' and attributes->'choice_versions' <> '{}'::jsonb
				then jsonb_build_object('choice_versions', (
					select jsonb_object_agg(choice, case when coalesce((versions->>'since')::int, 1) = $2::int
						then versions || jsonb_build_object('creatable_since', $1::int) else versions end)
					from jsonb_each(attributes->'choice_versions') as entry(choice, versions)))
				else '{}'::jsonb end
			|| case when kind = 'attribute' and jsonb_typeof(attributes->'item_versions') = 'object' and attributes->'item_versions' <> '{}'::jsonb
				then jsonb_build_object('item_versions', (
					select jsonb_object_agg(item, case when coalesce((versions->>'since')::int, 1) = $2::int
						then versions || jsonb_build_object('creatable_since', $1::int) else versions end)
					from jsonb_each(attributes->'item_versions') as entry(item, versions)))
				else '{}'::jsonb end
		where kind = 'attribute' or (kind = 'supertag' and coalesce((attributes->>'block_type')::boolean, false))
	`, built+1, before.creatable); err != nil {
		t.Fatalf("move the elements read from version %d to creatable from version %d: %v", before.creatable, built+1, err)
	}
	want := installedNumbersOf(t, ctx, database.Pool)
	if want == before {
		t.Fatalf("this build's install of the migrated store answers the boot's numbers %+v: the migration moved nothing the row holds", want)
	}
	deadline := time.Now().Add(schemaHealthWait)
	for {
		held := registeredSchema(t, ctx, database.Pool)
		if held == want {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the process's registry row holds schema numbers %+v %v after the reinstall, want %+v, what it serves now:\n%s",
				held, schemaHealthWait, want, server.Output.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// schemaNumbers is what a registry row holds of the document schema: the version its process
// reads and the highest since among the elements it may create.
type schemaNumbers struct{ version, creatable int }

// registeredSchema reads the schema numbers of the one live process registered on the database.
func registeredSchema(t *testing.T, ctx context.Context, pool *store.Pool) schemaNumbers {
	t.Helper()
	var held schemaNumbers
	if err := pool.QueryRow(ctx, `select schema_version, schema_creatable from server_processes`).Scan(&held.version, &held.creatable); err != nil {
		t.Fatalf("read the process's registry row: %v", err)
	}
	return held
}

// installedNumbersOf is the schema numbers this build serves over the store as it stands: the
// test's own process installs it, as the server does, and the schema it served before is put back
// once the test is over.
func installedNumbersOf(t *testing.T, ctx context.Context, pool *store.Pool) schemaNumbers {
	t.Helper()
	pmdoctest.UseBlockSchema(t, decodeSchema(t, pmdoc.SchemaJSON()))
	if err := schemanodes.InstallFromStore(ctx, pool); err != nil {
		t.Fatalf("install the store's schema in the test's process: %v", err)
	}
	return schemaNumbers{pmdoc.SchemaVersion(), pmdoc.SchemaCreatable()}
}

// moveStoreAboveTheBuild applies, in one transaction, a type migration of the release after this
// build: it raises every block type's version to one past this build's, adds the typed block
// future, which that version introduces, and adds the typed block claim, which declares no since
// and so is read from version 1. The commit is the one moment the store changes, and the triggers
// of 20261109051232 announce it once.
func moveStoreAboveTheBuild(t *testing.T, ctx context.Context, pool *store.Pool, built int) {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the type migration: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	const writer = `{"kind": "system", "id": "cmd-schema-test"}`
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`insert into graph_nodes (kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event)
		values
			('supertag', 'future', 'supertag', 'project', 'live',
				jsonb_build_object('block_type', true, 'content', 'paragraph+', 'render', 'host',
					'version', $2::int, 'position', 98, 'since', $2::int, 'creatable_since', $2::int),
				now(), $1::jsonb, 0),
			('supertag', 'claim', 'supertag', 'project', 'live',
				jsonb_build_object('block_type', true, 'content', 'paragraph+', 'render', 'host',
					'version', $2::int, 'position', 99),
				now(), $1::jsonb, 0)`, []any{writer, built + 1}},
		{`update graph_nodes set attributes = jsonb_set(attributes, '{version}', to_jsonb($1::int))
		where kind = $2 and coalesce((attributes->>'block_type')::boolean, false)`, []any{built + 1, schemanodes.KindSupertag}},
	} {
		if _, err := tx.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatalf("write the type migration: %v", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the type migration: %v", err)
	}
}

// servedType reports whether served declares the typed block name.
func servedType(served pmdoc.BlockSchema, name string) bool {
	return slices.ContainsFunc(served.Types, func(typ pmdoc.BlockTypeSchema) bool { return typ.Name == name })
}

// servedSchema reads the schema the process serves (GET /api/v1/schema/blocks, the installed value).
func (p *dispatchProcess) servedSchema(t *testing.T) pmdoc.BlockSchema {
	t.Helper()
	return decodeSchema(t, p.call(t, http.MethodGet, "/api/v1/schema/blocks", "", http.StatusOK))
}

// waitForServedSchema reads the served schema until reached holds of it, and fails the test with
// the last one and the process's output once schemaHealthWait has passed.
func (p *dispatchProcess) waitForServedSchema(t *testing.T, what string, reached func(pmdoc.BlockSchema) bool) pmdoc.BlockSchema {
	t.Helper()
	deadline := time.Now().Add(schemaHealthWait)
	for {
		served := p.servedSchema(t)
		if reached(served) {
			return served
		}
		if time.Now().After(deadline) {
			t.Fatalf("the process never served %s within %v; last version %d:\n%s", what, schemaHealthWait, served.Version, p.Output.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// schemaHealthBody is the part of /healthz this test reads: whether the process says it can
// serve and whether its database answered.
type schemaHealthBody struct {
	OK bool `json:"ok"`
	DB bool `json:"db"`
}

// schemaHealth reads /healthz once, bounded so a probe that hangs fails the test.
func (p *dispatchProcess) schemaHealth(t *testing.T) (int, schemaHealthBody) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, p.url("/healthz"), nil)
	if err != nil {
		t.Fatalf("build the /healthz request: %v", err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer response.Body.Close()
	var health schemaHealthBody
	if err := json.NewDecoder(response.Body).Decode(&health); err != nil {
		t.Fatalf("decode /healthz: %v", err)
	}
	return response.StatusCode, health
}
