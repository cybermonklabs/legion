package store

// production_events_migration_test.go holds the stored shape of the record of deploys and failed
// changes (dispatch://INFRA-978, R6 and R13) to the root spec's two Design sections that name it,
// "Failed changes" and "Settings and the production event", as the plan's D4 maps them into
// columns (migrations 20261123002035 and 20261123002036).

import (
	"context"
	"regexp"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"
)

// productionEventsField is one field of the record: its column, the type the catalog reports for
// it (information_schema.columns' udt_name), whether it may be null, and the sentence of the root
// spec or of D4 that names it.
type productionEventsField struct {
	column, udt string
	nullable    bool
	named       string
}

// productionEventsFields is the record's fields, one row a field. A deploy and a failure share one
// table, so the fields only a failure carries are null on a deploy (the production_events_shape
// check holds which), and a deploy's finished_at is null on a failure.
var productionEventsFields = []productionEventsField{
	{"id", "uuid", false, `Settings and the production event: "production_events holds id"`},
	{"project_key", "text", false, `Settings and the production event: "project_key"`},
	{"kind", "text", false, `Settings and the production event: "kind (deploy or failure)"`},
	{"environment", "text", false, `Settings and the production event: "environment (default production)"`},
	{"status", "text", false, `Settings and the production event: "status"`},
	{"source", "text", false, `Settings and the production event: "source", whose writers D4 lists`},
	{"source_ref", "text", false, `Settings and the production event: "source and source_ref (unique per project)"`},
	{"started_at", "timestamptz", true, `Settings and the production event: "started_at"`},
	{"finished_at", "timestamptz", true, `Settings and the production event: "finished_at"`},
	{"repo", "text", false, `D4: "repo", the repository of the run a deploy row is`},
	{"commits", "_text", false, `Settings and the production event: "commits"`},
	{"pull_requests", "_text", false, `Settings and the production event: "pull_requests"`},
	{"url", "text", false, `Settings and the production event: "url"`},
	{"actor", "jsonb", false, `Settings and the production event: "actor"; Failed changes: "actor"`},
	{"created_at", "timestamptz", false, `Settings and the production event: "created_at"`},
	{"deploy_id", "uuid", true, `Failed changes: "deploy_id"`},
	{"signal", "text", true, `Failed changes: "signal (check_fail, revert, incident_issue, api, person)"`},
	{"detected_at", "timestamptz", true, `Failed changes: "detected_at"`},
	{"state", "text", false, `Failed changes: "state (counted or rejected, with who and when)"`},
	{"state_reason", "text", true, `D4: "state_reason", the reason a reject or a re-count keeps (R8)`},
	{"state_by", "jsonb", true, `Failed changes: "with who": the writer of the state`},
	{"state_at", "timestamptz", true, `Failed changes: "and when": the time of the state`},
	{"restored_at", "timestamptz", true, `Failed changes: "restored_at"`},
	{"restored_by", "jsonb", true, `Failed changes: "restored_by"`},
	{"restored_by_deploy", "uuid", true, `D4: "restored_by_deploy (the deploy that restored it)"`},
	{"note", "text", false, `INFRA-978 R7 (v20): the note of a person's flag, which the record keeps as sent`},
}

// TestTheProductionEventsTableIsTheRecordTheRootNames: after every migration the table's columns
// are the fields the root spec's Design names as D4 maps them, one column a field and no other,
// each of the type and nullability its field has; id is its primary key and a uuid;
// unique (project_key, source_ref) stands, so one signal makes one row; the index on
// (project_key, kind, finished_at desc), the read of a project's deploys in a window, stands; and
// the kind checks of delivery_changes and of its outbox, delivery_changes_pending, accept
// production_events, which they admit by their definition and by a row of that kind, while a kind
// no one names is still refused by each.
func TestTheProductionEventsTableIsTheRecordTheRootNames(t *testing.T) {
	ctx := context.Background()
	database := migratedEmptyStore(t)

	rows, err := database.Pool.Query(ctx, `
		select column_name::text, udt_name::text, is_nullable = 'YES'
		from information_schema.columns
		where table_schema = current_schema() and table_name = 'production_events'
		order by ordinal_position
	`)
	if err != nil {
		t.Fatalf("read the columns of production_events: %v", err)
	}
	stored, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (productionEventsField, error) {
		var field productionEventsField
		err := row.Scan(&field.column, &field.udt, &field.nullable)
		return field, err
	})
	if err != nil {
		t.Fatalf("read the columns of production_events: %v", err)
	}
	if len(stored) == 0 {
		t.Fatal("production_events has no columns in the catalog, so this test examined nothing")
	}
	byColumn := map[string]productionEventsField{}
	for _, field := range stored {
		byColumn[field.column] = field
	}
	named := map[string]bool{}
	for _, want := range productionEventsFields {
		named[want.column] = true
		got, held := byColumn[want.column]
		switch {
		case !held:
			t.Errorf("production_events has no column %s; want one for the field the root spec names (%s)", want.column, want.named)
		case got.udt != want.udt || got.nullable != want.nullable:
			t.Errorf("production_events.%s is %s, nullable %t; want %s, nullable %t (%s)",
				want.column, got.udt, got.nullable, want.udt, want.nullable, want.named)
		}
	}
	for _, field := range stored {
		if !named[field.column] {
			t.Errorf("production_events has the column %s, which no field of the root spec's Design or of D4 names", field.column)
		}
	}
	t.Logf("production_events has %d columns, for the %d fields the spec names", len(stored), len(productionEventsFields))

	primary, err := database.Pool.Query(ctx, `
		select a.attname::text, format_type(a.atttypid, a.atttypmod)
		from pg_constraint c
		join pg_attribute a on a.attrelid = c.conrelid and a.attnum = any (c.conkey)
		where c.conrelid = 'production_events'::regclass and c.contype = 'p'
	`)
	if err != nil {
		t.Fatalf("read the primary key of production_events: %v", err)
	}
	type keyColumn struct{ column, sqlType string }
	key, err := pgx.CollectRows(primary, func(row pgx.CollectableRow) (keyColumn, error) {
		var column keyColumn
		err := row.Scan(&column.column, &column.sqlType)
		return column, err
	})
	if err != nil {
		t.Fatalf("read the primary key of production_events: %v", err)
	}
	if !slices.Equal(key, []keyColumn{{"id", "uuid"}}) {
		t.Errorf("the primary key of production_events is %v, want id of type uuid alone", key)
	}

	var unique []string
	uniqueRows, err := database.Pool.Query(ctx, `
		select pg_get_constraintdef(oid) from pg_constraint
		where conrelid = 'production_events'::regclass and contype = 'u'
	`)
	if err != nil {
		t.Fatalf("read the unique constraints of production_events: %v", err)
	}
	if unique, err = pgx.CollectRows(uniqueRows, pgx.RowTo[string]); err != nil {
		t.Fatalf("read the unique constraints of production_events: %v", err)
	}
	if !slices.Equal(unique, []string{"UNIQUE (project_key, source_ref)"}) {
		t.Errorf("the unique constraints of production_events are %q, want UNIQUE (project_key, source_ref) alone", unique)
	}

	indexRows, err := database.Pool.Query(ctx, `
		select indexdef from pg_indexes where schemaname = current_schema() and tablename = 'production_events' order by indexname
	`)
	if err != nil {
		t.Fatalf("read the indexes of production_events: %v", err)
	}
	indexes, err := pgx.CollectRows(indexRows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the indexes of production_events: %v", err)
	}
	window := regexp.MustCompile(`USING btree \(project_key, kind, finished_at DESC\)$`)
	if !slices.ContainsFunc(indexes, window.MatchString) {
		t.Errorf("no index of production_events ends USING btree (project_key, kind, finished_at DESC), with no predicate: %q", indexes)
	}

	for _, table := range []string{"delivery_changes", "delivery_changes_pending"} {
		definitionRows, err := database.Pool.Query(ctx, `
			select pg_get_constraintdef(oid) from pg_constraint
			where conrelid = $1::regclass and contype = 'c' and pg_get_constraintdef(oid) like 'CHECK ((kind = ANY%'
		`, table)
		if err != nil {
			t.Fatalf("read the kind check of %s: %v", table, err)
		}
		definitions, err := pgx.CollectRows(definitionRows, pgx.RowTo[string])
		if err != nil {
			t.Fatalf("read the kind check of %s: %v", table, err)
		}
		if len(definitions) != 1 || !regexp.MustCompile(`'production_events'::text`).MatchString(definitions[0]) {
			t.Errorf("the kind checks of %s are %q, want one that lists 'production_events'", table, definitions)
		}
		for _, kind := range []string{"production_events", "no_such_kind"} {
			tx, err := database.Pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin a write of kind %s to %s: %v", kind, table, err)
			}
			_, err = tx.Exec(ctx, `insert into `+table+` (kind, repo, ref) values ($1, 'ACME', 'a-row')`, kind)
			if rollback := tx.Rollback(ctx); rollback != nil {
				t.Fatalf("roll back the write of kind %s to %s: %v", kind, table, rollback)
			}
			code, _ := sqlState(err)
			switch {
			case kind == "production_events" && err != nil:
				t.Errorf("%s refused a row of kind production_events: %v", table, err)
			case kind != "production_events" && code != "23514":
				t.Errorf("%s took a row of kind %s (SQLSTATE %q, err %v), want it refused as a check violation: the kind check must still bound the kinds", table, kind, code, err)
			}
		}
	}
}
