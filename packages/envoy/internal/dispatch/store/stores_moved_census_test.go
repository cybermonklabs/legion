package store_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/storesmove"
	"github.com/sjawhar/envoy/internal/pgmigrate"
)

// storesMovedMigration is the migration that writes the supertags of the moved stores' kinds by
// bounded inserts into graph_nodes and graph_tuples.
const storesMovedMigration = "20261123134318_stores_moved.up.sql"

// artifactIndexName is the name, after its version, of this issue's concurrent build of
// graph_nodes_artifact_id, 20261123185607_graph_nodes_artifact_id_index, which sorts after
// storesMovedMigration. The test finds it by that name, so a version that sorts it before the
// supertag migration fails on the order and not on a missing name.
const artifactIndexName = "_graph_nodes_artifact_id_index.up.sql"

// TestStoresMovedPassesTheCensusAtAOneByteLimit is c-moved-bounded's: the pre-deploy census admits
// dispatch://INFRA-1098's own pending migrations at any size of the two graph tables. Over a
// database of its own, it applies every migration of the store but this issue's own, which the
// rehearsal's one list names (storesmove.OwnMigrations), recording each version, writes one row
// into each of graph_nodes and graph_tuples, and takes the census of every migration of the store
// with the limit at one byte, so the pending migrations are this issue's own and every table that
// holds a row is above the limit. It fails on each refusal of each pending migration, and holds
// the census of the supertag migration to graph_nodes and graph_tuples standing above the limit and
// admitted for bounded writes. The census admits a table for bounded writes only where no earlier
// pending migration touched it with more than bounded writes (changedBy).
//
// It also reads the names of this issue's own migrations and asserts that the concurrent build of
// graph_nodes_artifact_id, 20261123185607_graph_nodes_artifact_id_index, sorts after the supertag
// migration, printing both. The order no longer rests on a refusal of a concurrent build:
// dispatch://INFRA-1950's 59fc9e43, which da4952b0e3 of dispatch://INFRA-1150's
// cybermonklabs/legion#4173 carries word for word, makes the census read one non-unique concurrent
// index build as no change of its table. Until this line holds that change, a concurrent build
// versioned before the supertag migration still makes changedBy refuse graph_nodes, so the order
// keeps the census of this line passing on either side of that merge.
func TestStoresMovedPassesTheCensusAtAOneByteLimit(t *testing.T) {
	ctx := context.Background()
	own := storesmove.OwnMigrations()
	holdStoresMovedOrder(t, own)
	database := store.OpenEmptyTestStore(t)
	if err := database.MigrateWithout(ctx, own); err != nil {
		t.Fatalf("apply every migration but this issue's own: %v", err)
	}
	for _, write := range []string{
		`insert into graph_nodes (kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event)
		 values ('issue', 'BOUND-1', 'issue', 'project', 'live', '{}', now(), '{"kind":"user","id":"alice"}', 0)`,
		`insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, asserted)
		 values ('issue', 'BOUND-1', 'state', 'value', 'live', now(), '{"kind":"user","id":"alice"}', 0, true)`,
	} {
		if _, err := database.Pool.Exec(ctx, write); err != nil {
			t.Fatalf("write a row of the store: %v", err)
		}
	}

	report, err := store.CensusOfEveryMigration(ctx, database.Pool.Config().ConnString(), pgmigrate.CensusOptions{TableLimit: 1})
	if err != nil {
		t.Fatalf("census: %v", err)
	}
	var written strings.Builder
	report.Write(&written)
	t.Logf("the census at a one-byte limit:\n%s", written.String())

	var pending []string
	for _, entry := range report.Pending {
		pending = append(pending, entry.Migration.Name)
		for _, refusal := range entry.Refusals {
			t.Errorf("the census refuses %s: %s", entry.Migration.Name, refusal)
		}
	}
	if !slices.Equal(pending, own) {
		t.Fatalf("the pending migrations are %v, want this issue's own, %v", pending, own)
	}
	for _, entry := range report.Pending {
		if entry.Migration.Name != storesMovedMigration {
			continue
		}
		for _, table := range []string{"graph_nodes", "graph_tuples"} {
			index := slices.IndexFunc(entry.Tables, func(read pgmigrate.TableCensus) bool { return read.Name == table })
			if index < 0 {
				t.Errorf("the census of %s reads no %s", storesMovedMigration, table)
				continue
			}
			read := entry.Tables[index]
			if !read.Exists || read.Bytes <= 1 || read.BoundedWrites == 0 {
				t.Errorf("the census of %s reads %s as %+v, want it standing, above the one-byte limit and admitted for bounded writes",
					storesMovedMigration, table, read)
			}
			for _, line := range strings.Split(written.String(), "\n") {
				if strings.HasPrefix(line, "census: "+storesMovedMigration+" touches "+table+":") {
					t.Logf("%s", line)
				}
			}
		}
	}
}

// holdStoresMovedOrder holds own, this issue's own migrations, to the order the census of this line
// rests on: the concurrent build of graph_nodes_artifact_id (artifactIndexName) sorts after the
// supertag migration (storesMovedMigration). It prints both names in the order they sort, and fails
// where the build sorts first or where own lacks either.
func holdStoresMovedOrder(t *testing.T, own []string) {
	t.Helper()
	index := slices.IndexFunc(own, func(name string) bool { return strings.HasSuffix(name, artifactIndexName) })
	if index < 0 || !slices.Contains(own, storesMovedMigration) {
		t.Errorf("this issue's own migrations %v hold no %s or no migration named %s after its version", own, storesMovedMigration, artifactIndexName)
		return
	}
	build := own[index]
	order := []string{storesMovedMigration, build}
	slices.Sort(order)
	t.Logf("c-moved-bounded: this issue's own migrations sort %s, then %s", order[0], order[1])
	if order[0] == build {
		t.Errorf("%s sorts before %s: the concurrent build of graph_nodes_artifact_id must sort after the supertag migration, "+
			"or on a line without da4952b0e3 changedBy refuses graph_nodes in the census of %s", build, storesMovedMigration, storesMovedMigration)
	}
}
