package store

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/sjawhar/envoy/internal/pgmigrate"
)

// checkSubjectVersion is 20261125125701_graph_supertags_check_subject. Main shipped it first in
// legion-envoy-v35.18.0, where it also moved the five block type supertag nodes to version 6. It
// sorts above the two type migrations that move them to 11 and 12 (planTaskMergesVersion and
// planTaskOwnerVersion), and no migration above it may write graph_nodes over a store the backfill
// filled while btreeGinVersion is pending, so this tree edits it, under its replaces line, to move
// no version.
const checkSubjectVersion = 20261125125701

// planTaskMergesVersion moves the block types to 11 (20261125012814_graph_supertags_plan_task_merges)
// and planTaskOwnerVersion to 12 (20261125034832_graph_supertags_plan_task_owner): the version
// pmdoc/schema/blocks.json declares. No release of main holds either.
const (
	planTaskMergesVersion = 20261125012814
	planTaskOwnerVersion  = 20261125034832
)

// mainCheckSubjectFile is main's text of checkSubjectVersion, byte for byte, which this tree's file
// names in its first replaces line (mainCheckSubjectText holds the two to one checksum). It sits
// beside this file rather than in a testdata directory, which the CI filters of the store's jobs
// do not hold.
const mainCheckSubjectFile = "check_subject_edit_migration_test.main.sql"

// checkSubjectBlockTypes are the supertag nodes that declare a block type, each of which carries
// the block schema's version.
var checkSubjectBlockTypes = []string{"ask", "callout", "check", "plan_task", "requirement"}

// checkSubjectAttributes are the attribute nodes of both sides of the merge: main's
// checkSubjectVersion adds check/subject and check/program, and planTaskMergesVersion and
// planTaskOwnerVersion add plan_task/merges and plan_task/owner.
var checkSubjectAttributes = []string{"check/program", "check/subject", "plan_task/merges", "plan_task/owner"}

// TestEveryBlockTypeEndsAtTheFilesVersionWhetherOrNotMainsCheckSubjectRan holds the edit of
// checkSubjectVersion to its purpose: each database this tree migrates ends with the five block
// type nodes at version 12, the version pmdoc/schema/blocks.json declares, on their rows and as
// their newest version tuples, beside the attribute nodes of both sides of the merge.
//
//   - A fresh database applies the edited file after planTaskMergesVersion and planTaskOwnerVersion,
//     and records the edited text as the text it applied.
//   - A database a release of main migrated ran main's text, which left the five nodes at 6. The
//     runner accepts that text through the replaces line, keeps its checksum as the text applied,
//     and applies the two type migrations after it, as versions it has not recorded.
//
// The control is a fresh database given main's text in place of the edit: it ends at 6, so the
// check fails on the database the edit exists to end.
func TestEveryBlockTypeEndsAtTheFilesVersionWhetherOrNotMainsCheckSubjectRan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mainText, edited := mainCheckSubjectText(t)
	const want = "12"

	t.Run("a fresh database", func(t *testing.T) {
		t.Parallel()
		store := openMigratedTestStore(t, latestVersion)
		requireBlockTypesAtVersion(t, store, want)
		requireBothSidesAttributes(t, store)
		requireCheckSubjectRecorded(t, store, edited.RecordedIdentity(), edited.Checksum)
	})

	t.Run("a database a release of main migrated", func(t *testing.T) {
		t.Parallel()
		store := openMigratedTestStore(t, planTaskMergesVersion-1)
		if err := store.migrate(ctx, mainRelease(t, mainText, edited)); err != nil {
			t.Fatalf("migrate through %d with main's text of %d: %v", btreeGinVersion, checkSubjectVersion, err)
		}
		requireBlockTypesAtVersion(t, store, "6")
		if err := store.Migrate(ctx); err != nil {
			t.Fatalf("migrate the database main's release migrated with this tree's set: %v", err)
		}
		requireBlockTypesAtVersion(t, store, want)
		requireBothSidesAttributes(t, store)
		requireCheckSubjectRecorded(t, store, edited.RecordedIdentity(), pgmigrate.Checksum(mainText))
	})

	t.Run("control: a fresh database given main's text in place of the edit", func(t *testing.T) {
		t.Parallel()
		store := openMigratedTestStore(t, checkSubjectVersion-1)
		set := migrationsThrough(t, latestVersion)
		set["migrations/"+edited.Name] = &fstest.MapFile{Data: mainText}
		if err := store.migrate(ctx, set); err != nil {
			t.Fatalf("migrate with main's text of %d in place of the edit: %v", checkSubjectVersion, err)
		}
		requireBlockTypesAtVersion(t, store, "6")
	})
}

// mainCheckSubjectText reads main's text of checkSubjectVersion (mainCheckSubjectFile) and this
// tree's migration of that version, and fails unless the migration's first replaces line names
// that text under the migration's own name and the migration's own text differs from it: a run
// over any other text proves nothing about a database main's release migrated.
func mainCheckSubjectText(t *testing.T) ([]byte, pgmigrate.Migration) {
	t.Helper()
	text, err := os.ReadFile(mainCheckSubjectFile)
	if err != nil {
		t.Fatalf("read main's text of %d: %v", checkSubjectVersion, err)
	}
	migrations := shippedMigrations(t)
	at := slices.IndexFunc(migrations, func(m pgmigrate.Migration) bool { return m.Version == checkSubjectVersion })
	if at < 0 {
		t.Fatalf("this tree has no migration of version %d", checkSubjectVersion)
	}
	edited := migrations[at]
	mainIdentity := pgmigrate.Identity{Name: edited.Name, Checksum: pgmigrate.Checksum(text)}
	if len(edited.Replaces) == 0 || edited.Replaces[0] != mainIdentity {
		t.Fatalf("%s declares the replaces lines %v, want the first to name main's text, %s", edited.Name, edited.Replaces, mainIdentity)
	}
	if edited.Checksum == mainIdentity.Checksum {
		t.Fatalf("%s is main's text itself, so no edit is under test", edited.Name)
	}
	return text, edited
}

// mainRelease is the set a release of main that holds main's text of checkSubjectVersion migrates
// a database with, as far as the block types go: this tree's migrations through btreeGinVersion,
// without planTaskMergesVersion and planTaskOwnerVersion, which no release of main holds, and with
// main's text of checkSubjectVersion in place of the edit.
func mainRelease(t *testing.T, mainText []byte, edited pgmigrate.Migration) fstest.MapFS {
	t.Helper()
	set := migrationsThrough(t, btreeGinVersion)
	for _, version := range []int{planTaskMergesVersion, planTaskOwnerVersion} {
		prefix := fmt.Sprintf("migrations/%d_", version)
		removed := 0
		for name := range set {
			if strings.HasPrefix(name, prefix) {
				delete(set, name)
				removed++
			}
		}
		if removed == 0 {
			t.Fatalf("this tree has no file of version %d to leave out of main's release", version)
		}
	}
	set["migrations/"+edited.Name] = &fstest.MapFile{Data: mainText}
	return set
}

// requireBlockTypesAtVersion holds the supertag nodes that declare a block type to being the five
// of checkSubjectBlockTypes, each carrying version want on its row and as its newest asserted
// version tuple.
func requireBlockTypesAtVersion(t *testing.T, store *Store, want string) {
	t.Helper()
	ctx := context.Background()
	rows, err := store.Pool.Query(ctx, `
		select n.id, coalesce(n.attributes->>'version', ''),
		       coalesce((select t.to_id from graph_tuples t
		                 where t.from_kind = n.kind and t.from_id = n.id and t.kind = 'version'
		                   and t.to_kind = 'value' and t.asserted
		                 order by t.seq desc limit 1), '')
		from graph_nodes n
		where n.kind = 'supertag' and (n.attributes->>'block_type')::boolean
		order by n.id`)
	if err != nil {
		t.Fatalf("read the block type nodes: %v", err)
	}
	defer rows.Close()
	var types []string
	for rows.Next() {
		var id, row, tuple string
		if err := rows.Scan(&id, &row, &tuple); err != nil {
			t.Fatalf("read a block type node: %v", err)
		}
		types = append(types, id)
		if row != want || tuple != want {
			t.Errorf("block type %s carries version %q on its row and %q as its newest version tuple, want %q on both", id, row, tuple, want)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the block type nodes: %v", err)
	}
	if !slices.Equal(types, checkSubjectBlockTypes) {
		t.Errorf("the supertag nodes that declare a block type are %q, want %q", types, checkSubjectBlockTypes)
	}
}

// requireBothSidesAttributes holds the store to holding the attribute nodes of
// checkSubjectAttributes, those of both sides of the merge.
func requireBothSidesAttributes(t *testing.T, store *Store) {
	t.Helper()
	var attributes []string
	if err := store.Pool.QueryRow(context.Background(), `
		select coalesce(array_agg(id order by id), '{}') from graph_nodes where kind = 'attribute' and id = any($1)
	`, checkSubjectAttributes).Scan(&attributes); err != nil {
		t.Fatalf("read the attribute nodes: %v", err)
	}
	if !slices.Equal(attributes, checkSubjectAttributes) {
		t.Errorf("the store holds the attribute nodes %q of %q", attributes, checkSubjectAttributes)
	}
}

// requireCheckSubjectRecorded holds schema_migrations' row of checkSubjectVersion to recording
// identity, the first identity of the version, and applied, the checksum of the text the runner
// applied under it.
func requireCheckSubjectRecorded(t *testing.T, store *Store, identity pgmigrate.Identity, applied string) {
	t.Helper()
	var name, checksum, appliedChecksum string
	if err := store.Pool.QueryRow(context.Background(), `
		select name, checksum, applied_checksum from schema_migrations where version = $1::bigint
	`, checkSubjectVersion).Scan(&name, &checksum, &appliedChecksum); err != nil {
		t.Fatalf("read the row of %d: %v", checkSubjectVersion, err)
	}
	if got := (pgmigrate.Identity{Name: name, Checksum: checksum}); got != identity || appliedChecksum != applied {
		t.Errorf("version %d is recorded as %s with the applied text %s, want %s with the applied text %s", checkSubjectVersion, got, appliedChecksum, identity, applied)
	}
}
