package replay

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/store/migrations"
)

// A replay's database is copied from the template the key of the daemon's migrations and the
// replay's own tables names (templateKey), so a migration added, removed, renamed or rewritten, a
// byte moved from a migration's name to its SQL, or a table of the replay's own rewritten or added,
// names another template, and a template made for other migrations is never copied. The test keys
// the embedded set changed each of those ways and wants each key apart from the set's own and from
// every other change's, and the set's own key the same when read again. It then plants a database
// under the key of the embedded set with its newest migration rewritten, holding a table no
// migration makes, and makes a replay's database (openReplayDatabase): it fails where that
// database holds the planted table, or lacks a version of the embedded set or a table of the
// replay's own. The rewrite carries a random comment, so two runs of the test on one server plant
// two databases.
func TestATemplateMadeForOtherMigrationsIsNeverCopied(t *testing.T) {
	t.Parallel()
	dsn := replayDSN(t)
	ctx := context.Background()
	all, err := migrations.All()
	if err != nil {
		t.Fatalf("read the embedded migrations: %v", err)
	}
	if len(all) < 2 || len(replayTables) == 0 {
		t.Fatalf("the embedded set holds %d migrations and the replay %d tables of its own, too few to change", len(all), len(replayTables))
	}
	key := templateKey(all, replayTables)
	if again := templateKey(slices.Clone(all), slices.Clone(replayTables)); again != key {
		t.Errorf("the embedded set keys %s, then %s", key, again)
	}
	keys := map[string]string{key: "the embedded set"}
	last := len(all) - 1
	changed := func(change func([]migrations.Migration) []migrations.Migration) []migrations.Migration {
		return change(slices.Clone(all))
	}
	rewritten := changed(func(set []migrations.Migration) []migrations.Migration {
		set[last].SQL += "\n-- " + randomName("rewritten ") + "\n"
		return set
	})
	tables := slices.Clone(replayTables)
	tables[len(tables)-1] += " -- rewritten"
	for _, c := range []struct {
		name   string
		all    []migrations.Migration
		tables []string
	}{
		{"the newest migration rewritten", rewritten, replayTables},
		{"the first migration renamed", changed(func(set []migrations.Migration) []migrations.Migration {
			set[0].Name = "0001_renamed.up.sql"
			return set
		}), replayTables},
		{"a migration added", changed(func(set []migrations.Migration) []migrations.Migration {
			return append(set, migrations.Migration{Version: set[last].Version + 1, Name: "99999999999999_added.up.sql", SQL: "select 1;"})
		}), replayTables},
		{"the newest migration removed", changed(func(set []migrations.Migration) []migrations.Migration { return set[:last] }), replayTables},
		{"a byte moved from the first migration's name to its SQL", changed(func(set []migrations.Migration) []migrations.Migration {
			name := set[0].Name
			set[0].Name, set[0].SQL = name[:len(name)-1], name[len(name)-1:]+set[0].SQL
			return set
		}), replayTables},
		{"a table of the replay's own rewritten", all, tables},
		{"a table of the replay's own added", all, append(slices.Clone(replayTables), "create table replay_added (issue text primary key)")},
	} {
		got := templateKey(c.all, c.tables)
		if was, seen := keys[got]; seen {
			t.Errorf("the embedded set with %s keys %s, the key of %s", c.name, got, was)
		}
		keys[got] = "the embedded set with " + c.name
	}

	stale := replayDatabase{name: replayTemplatePrefix + templateKey(rewritten, replayTables), admin: dsn}
	if stale.url, err = databaseURL(dsn, stale.name); err != nil {
		t.Fatal(err)
	}
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to make the stale template: %v", err)
	}
	_, err = admin.Exec(ctx, "create database "+pgx.Identifier{stale.name}.Sanitize()+" template template1 strategy file_copy")
	_ = admin.Close(ctx)
	if err != nil {
		t.Fatalf("make the stale template %s: %v", stale.name, err)
	}
	t.Cleanup(func() {
		if err := stale.drop(ctx); err != nil {
			t.Errorf("drop the stale template: %v", err)
		}
	})
	planted, err := pgx.Connect(ctx, stale.url)
	if err != nil {
		t.Fatalf("connect to the stale template: %v", err)
	}
	_, err = planted.Exec(ctx, "create table replay_stale_template_mark (made text)")
	_ = planted.Close(ctx)
	if err != nil {
		t.Fatalf("mark the stale template: %v", err)
	}

	_, st := replayStore(t, dsn)
	var path string
	if err := st.Pool().QueryRow(ctx, "show search_path").Scan(&path); err != nil || path != "public,pg_temp" {
		t.Fatalf("replay search_path = %q, err = %v; want public and then pg_temp", path, err)
	}
	var marked bool
	if err := st.Pool().QueryRow(ctx, "select to_regclass('replay_stale_template_mark') is not null").Scan(&marked); err != nil {
		t.Fatalf("read whether the replay's database holds the stale template's mark: %v", err)
	}
	if marked {
		t.Errorf("the replay's database holds the table of the template %s, made for other migrations", stale.name)
	}
	var versions []int
	if err := st.Pool().QueryRow(ctx, "select coalesce(array_agg(version order by version), '{}') from schema_version").Scan(&versions); err != nil {
		t.Fatalf("read the replay's database's migrations: %v", err)
	}
	want := make([]int, len(all))
	for i, migration := range all {
		want[i] = migration.Version
	}
	if !slices.Equal(versions, want) {
		t.Errorf("the replay's database records the migrations %v, want every embedded one, %v", versions, want)
	}
	for _, table := range replayTables {
		name := strings.Fields(table)[2]
		var made bool
		if err := st.Pool().QueryRow(ctx, "select to_regclass($1) is not null", name).Scan(&made); err != nil {
			t.Fatalf("read whether the replay's database holds %s: %v", name, err)
		}
		if !made {
			t.Errorf("the replay's database lacks the replay's own table %s", name)
		}
	}
}
