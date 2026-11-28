package store

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/pgmigrate"
)

// Dispatch's migrations are written for schema public, and one of them names it
// (20261115200014_owner_lock_legacy_gate.up.sql). Under a connection whose search_path puts
// another schema that exists ahead of public, as a login's own schema does under the default
// "$user", public, the runner applied every migration before that one in the other schema and
// then failed (42P01) on the first object of public it named, which left the database half
// migrated. Migrate refuses such a connection before it creates schema_migrations: the refusal
// names the schema it found and public, and nothing is created in either schema.
//
// The search_path here is a connection option, "<other>,public", which keeps its value under the
// values that RESET gives (pgmigrate.Pin), so the other schema is the one the runner would create
// every object in, while public stays on the path as it does under "$user". The control reads
// current_schema() in a transaction pinned as the runner pins its own and finds the other schema
// there: without it, the row could pass on a connection whose migrations would have gone to public
// anyway, and prove nothing.
func TestMigrateRefusesAConnectionWhoseSchemaIsNotPublicAndAppliesNothing(t *testing.T) {
	ctx := context.Background()
	database := openEmptyTestStore(t)
	schema := "other_" + randomDatabaseSuffix(t)
	if _, err := database.Pool.Exec(ctx, "create schema "+schema); err != nil {
		t.Fatalf("create schema %s: %v", schema, err)
	}
	withSearchPath, err := url.Parse(database.Pool.Config().ConnString())
	if err != nil {
		t.Fatalf("parse the test database's URL: %v", err)
	}
	// No space after the comma: url.Values.Encode writes a space as "+", which pgx reads back as
	// a "+", so the path would name a schema "+public" in place of public.
	query := withSearchPath.Query()
	query.Set("search_path", schema+",public")
	withSearchPath.RawQuery = query.Encode()
	store, err := Open(ctx, withSearchPath.String())
	if err != nil {
		t.Fatalf("open a store whose connections name search_path %s,public: %v", schema, err)
	}
	t.Cleanup(store.Pool.Close)

	var pinned string
	if err := pgx.BeginFunc(ctx, store.Pool, func(tx pgx.Tx) error {
		if err := pgmigrate.Pin(ctx, tx); err != nil {
			return err
		}
		return tx.QueryRow(ctx, "select coalesce(current_schema(), '')").Scan(&pinned)
	}); err != nil {
		t.Fatalf("control: read current_schema() under the runner's pin: %v", err)
	}
	if pinned != schema {
		t.Fatalf("control: current_schema() under the runner's pin is %q, want %q: the connection's schema did not move, so the row proves nothing", pinned, schema)
	}

	err = store.Migrate(ctx)
	if err == nil || !strings.Contains(err.Error(), `schema "`+schema+`"`) ||
		!strings.Contains(err.Error(), "name schema public") || !strings.Contains(err.Error(), "none applied") {
		t.Errorf("Migrate = %v; want a refusal that names schema %q and public and says none was applied", err, schema)
	}

	rows, err := database.Pool.Query(ctx, `select n.nspname || '.' || c.relname
		from pg_class c join pg_namespace n on n.oid = c.relnamespace
		where n.nspname in ('public', $1)
		order by 1`, schema)
	if err != nil {
		t.Fatalf("list the relations of public and %s: %v", schema, err)
	}
	relations, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("list the relations of public and %s: %v", schema, err)
	}
	if len(relations) > 0 {
		t.Errorf("public and %s hold %d relations after Migrate, want none (no schema_migrations, no table); the first of them: %v", schema, len(relations), relations[:min(len(relations), 10)])
	}
}
