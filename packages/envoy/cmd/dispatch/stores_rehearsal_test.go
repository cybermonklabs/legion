package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/storesmove"
	"github.com/sjawhar/envoy/internal/dispatch/storesmove/storesmovetest"
)

// storesRehearsalDatabase is a database every migration but dispatch://INFRA-1098's own built, as
// production's stands before this issue's deploy, with the four tables of the priority store and
// the workflow store still tables: its pending migrations are this issue's own
// (storesmove.OwnMigrations), which sort after every other. It migrates through the newest
// embedded migration below this issue's first, since store.MigrateThrough takes only a version a
// migration carries.
func storesRehearsalDatabase(t *testing.T) *store.Store {
	t.Helper()
	first, _, _ := strings.Cut(storesmove.OwnMigrations()[0], "_")
	own, err := strconv.Atoi(first)
	if err != nil {
		t.Fatalf("read the version of this issue's first migration: %v", err)
	}
	migrations, err := store.Migrations()
	if err != nil {
		t.Fatalf("read the embedded migrations: %v", err)
	}
	before := 0
	for _, migration := range migrations {
		if migration.Version < own && migration.Version > before {
			before = migration.Version
		}
	}
	if before == 0 {
		t.Fatalf("no embedded migration sorts before this issue's first, %s", storesmove.OwnMigrations()[0])
	}
	return storetest.OpenFrom(t, "every migration before dispatch://INFRA-1098's own", func(url string) error {
		ctx := context.Background()
		database, err := store.Open(ctx, url)
		if err != nil {
			return err
		}
		defer database.Pool.Close()
		return database.MigrateThrough(ctx, before)
	})
}

// storesSnapshot is what a rehearsal must leave as it found: the count and digest of every row of
// graph_nodes, graph_tuples and the four old tables, the versions schema_migrations records, and
// whether the tables the pending migrations create stand.
func storesSnapshot(t *testing.T, database *store.Store) string {
	t.Helper()
	var snapshot string
	if err := database.Pool.QueryRow(context.Background(), `
		select concat_ws(' | ',
			(select count(*) || ' ' || md5(coalesce(string_agg(row(r.*)::text, E'\n' order by kind, id), '')) from graph_nodes r),
			(select count(*) || ' ' || md5(coalesce(string_agg(row(r.*)::text, E'\n' order by seq), '')) from graph_tuples r),
			(select count(*) || ' ' || md5(coalesce(string_agg(row(r.*)::text, E'\n' order by kind, id), '')) from priority_nodes r),
			(select count(*) || ' ' || md5(coalesce(string_agg(row(r.*)::text, E'\n' order by seq), '')) from priority_tuples r),
			(select count(*) || ' ' || md5(coalesce(string_agg(row(r.*)::text, E'\n' order by kind, id), '')) from workflow_nodes r),
			(select count(*) || ' ' || md5(coalesce(string_agg(row(r.*)::text, E'\n' order by seq), '')) from workflow_tuples r),
			(select string_agg(version::text, ',' order by version) from schema_migrations),
			to_regclass('stores_moved_seqs')::text, to_regclass('graph_edges_retirements')::text)
	`).Scan(&snapshot); err != nil {
		t.Fatalf("snapshot the stores: %v", err)
	}
	return snapshot
}

// rehearse runs `envoy-dispatch census` over database with options, as the census Job runs it,
// and answers its exit and its output, which it logs.
func rehearse(t *testing.T, database *store.Store, options storesmove.RehearsalOptions) (int, string) {
	t.Helper()
	var out, errOut strings.Builder
	code := censusWith(context.Background(), nil, database.Pool.Config().ConnString(), options, &out, &errOut)
	t.Logf("census exited %d:\n%s%s", code, out.String(), errOut.String())
	return code, out.String()
}

// TestThePreDeployCensusRehearsesTheMoveAndChangesNothing runs `envoy-dispatch census` over a
// database whose pending migrations are this issue's own and whose four tables hold rows the
// release before wrote (storesmovetest.Write), as the deploy's census Job runs it before the image
// moves. Its rehearsal runs this issue's own migrations it applies and leaves out the rest, copies
// every old tuple and takes the census before the drop in one transaction it rolls back: it prints
// its copy's time and each store's counts, exits 0, and leaves every row, every recorded version
// and every table as it found them. Over the same database, a planted plan whose issue attribute
// names another issue than its part_of makes the rehearsal refuse on a line of its own, naming the
// plan, with exit 1; a stop at its bound before its census ends does the same; a pending migration
// of another tree that touches graph_nodes, or that replaces the function of a trigger on it,
// refuses the rehearsal with exit 2; and after the boot's swap the census prints no rehearsal and
// exits 0. Each run leaves the store as it found it.
func TestThePreDeployCensusRehearsesTheMoveAndChangesNothing(t *testing.T) {
	t.Run("a rehearsal that reads no difference", func(t *testing.T) {
		database := storesRehearsalDatabase(t)
		storesmovetest.Write(t, database.Pool)
		var priorityTuples, workflowTuples int
		if err := database.Pool.QueryRow(context.Background(), `select (select count(*) from priority_tuples), (select count(*) from workflow_tuples)`).Scan(&priorityTuples, &workflowTuples); err != nil {
			t.Fatal(err)
		}
		before := storesSnapshot(t, database)
		code, out := rehearse(t, database, storesmove.RehearsalOptions{Explain: true})
		if code != 0 {
			t.Fatalf("the census exited %d, want 0", code)
		}
		for _, want := range []string{
			"census: rehearsal: it applies the pending migration 20261126000100_graph_edges_retirements.up.sql, of this issue's own",
			"census: rehearsal: it skips the pending migration 20261126000200_graph_edges_one_tuple_arm.up.sql, of this issue's own",
			"census: rehearsal: it applies the pending migration 20261126000300_stores_moved.up.sql, of this issue's own",
			"census: rehearsal: it skips the pending migration 20261126000400_graph_nodes_artifact_id_index.up.sql, of this issue's own",
			"census: rehearsal: it skips the pending migration 20261126000500_graph_tuples_workflow_inbound_index.up.sql, of this issue's own",
			"census: rehearsal: its copy ended: " + strconv.Itoa(priorityTuples+workflowTuples) + " tuples of the four tables in ",
			"census: rehearsal: priority: tuples: old " + strconv.Itoa(priorityTuples) + " ",
			"census: rehearsal: workflow: tuples: old " + strconv.Itoa(workflowTuples) + " ",
			"census: rehearsal: plan of the census of the priority tuples:",
			"census: rehearsal: no difference; it rolled back, so it changed nothing",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("the census's output holds no %q", want)
			}
		}
		if after := storesSnapshot(t, database); after != before {
			t.Errorf("the rehearsal changed the store:\nbefore %s\nafter  %s", before, after)
		}
	})

	t.Run("a planted plan whose issue attribute names another issue than its part_of", func(t *testing.T) {
		database := storesRehearsalDatabase(t)
		fixtures := storesmovetest.Write(t, database.Pool)
		const planted = "0f1e2d3c-0000-4000-8000-0000000000d1"
		storesmovetest.PutNode(t, database.Pool, "plan", planted, "MOVE-1", []storesmovetest.Attribute{
			storesmovetest.Text("issue", "MOVE-2"), storesmovetest.Text("project", fixtures.Project), storesmovetest.Text("source", "default"),
		}, nil, fixtures.Who, fixtures.At)
		before := storesSnapshot(t, database)
		code, out := rehearse(t, database, storesmove.RehearsalOptions{})
		if code != 1 {
			t.Errorf("the census exited %d over a planted difference, want 1", code)
		}
		if !strings.Contains(out, "census: REFUSED rehearsal: the census before the drop read a difference") || !strings.Contains(out, "id="+planted) {
			t.Errorf("the census's output names no refusal of the rehearsal at the planted plan %s", planted)
		}
		if after := storesSnapshot(t, database); after != before {
			t.Errorf("the refused rehearsal changed the store:\nbefore %s\nafter  %s", before, after)
		}
	})

	t.Run("a rehearsal that stops at its bound", func(t *testing.T) {
		database := storesRehearsalDatabase(t)
		storesmovetest.Write(t, database.Pool)
		before := storesSnapshot(t, database)
		started := time.Now()
		code, out := rehearse(t, database, storesmove.RehearsalOptions{
			Stop: 2 * time.Second, HoldFile: filepath.Join(t.TempDir(), "never-made"),
		})
		if code != 1 || !strings.Contains(out, "census: REFUSED rehearsal: it stopped after 2s, its bound") {
			t.Errorf("a rehearsal held past its 2s bound exited %d, want 1 on a refusal that names the stop", code)
		}
		if took := time.Since(started); took > 20*time.Second {
			t.Errorf("the rehearsal held past its 2s bound took %s to end", took)
		}
		if after := storesSnapshot(t, database); after != before {
			t.Errorf("the stopped rehearsal changed the store:\nbefore %s\nafter  %s", before, after)
		}
	})

	t.Run("a pending migration of another tree that touches graph_nodes", func(t *testing.T) {
		database := storesRehearsalDatabase(t)
		storesmovetest.Write(t, database.Pool)
		if _, err := database.Pool.Exec(context.Background(), `delete from schema_migrations where version = 20261109051232`); err != nil {
			t.Fatalf("leave 20261109051232 pending: %v", err)
		}
		before := storesSnapshot(t, database)
		code, out := rehearse(t, database, storesmove.RehearsalOptions{})
		if code != 2 || !strings.Contains(out, "census: the rehearsal refuses the pending migration 20261109051232_graph_supertags.up.sql\n") {
			t.Errorf("the census exited %d over a pending migration of another tree that touches graph_nodes, want 2 and the line that names it", code)
		}
		if after := storesSnapshot(t, database); after != before {
			t.Errorf("the refused rehearsal changed the store:\nbefore %s\nafter  %s", before, after)
		}
	})

	t.Run("a pending migration of another tree that replaces the function of a trigger of graph_nodes", func(t *testing.T) {
		database := storesRehearsalDatabase(t)
		storesmovetest.Write(t, database.Pool)
		extra := filepath.Join(t.TempDir(), "20991231000000_block_schema_changed.up.sql")
		if err := os.WriteFile(extra, []byte(`create or replace function block_schema_changed() returns trigger language plpgsql as $$
begin
  perform pg_notify('dispatch_block_schema', '');
  return null;
end
$$;
`), 0o600); err != nil {
			t.Fatal(err)
		}
		code, out := rehearse(t, database, storesmove.RehearsalOptions{ExtraMigration: extra})
		if code != 2 || !strings.Contains(out, "census: the rehearsal refuses the pending migration "+extra+"\n") {
			t.Errorf("the census exited %d over a migration that replaces block_schema_changed, want 2 and the line that names it", code)
		}
	})

	t.Run("a census after the swap", func(t *testing.T) {
		database := storetest.Open(t)
		storesmovetest.Write(t, database.Pool)
		if _, err := storesmove.Move(context.Background(), database.Pool.Config().ConnString(), storesmove.Options{}); err != nil {
			t.Fatalf("move: %v", err)
		}
		code, out := rehearse(t, database, storesmove.RehearsalOptions{})
		if code != 0 || strings.Contains(out, "rehearsal") {
			t.Errorf("the census after the swap exited %d and printed a rehearsal (%t), want 0 and none", code, strings.Contains(out, "rehearsal"))
		}
	})
}
