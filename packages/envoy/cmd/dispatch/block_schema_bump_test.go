package main

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc/schemanodes"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// issueTreeMigration is the name every migration of the issue-tree spec takes,
// <timestamp>_issue_tree_<what>.up.sql, as store's TestThisChildsMigrationsTouchNoTableAboveTheCensusLimit
// finds them: the spec's type migrations are the ones of store.TypeMigrations it names.
var issueTreeMigration = regexp.MustCompile(`^[0-9]{14}_issue_tree_[a-z0-9_]+\.up\.sql$`)

// specChangeMigration is the name every type migration of the change whose approvals write the
// approved spec of The Spec takes, <timestamp>_spec_change_<what>.up.sql: the change request's type
// migrations are the ones of store.TypeMigrations it names.
var specChangeMigration = regexp.MustCompile(`^[0-9]{14}_spec_change_[a-z0-9_]+\.up\.sql$`)

// blockSchemaBumpBounds is the two type migrations of migrations the bump test migrates a store
// through: spec, the newest of the issue-tree spec's own (issueTreeMigration), and before, the
// newest of the others numbered below it. A type migration numbered after spec, which a later
// change adds with its own move of blocks.json, is neither, so that move is no step of the spec's.
func blockSchemaBumpBounds(migrations []string) (before, spec string, err error) {
	return typeMigrationBumpBounds(migrations, issueTreeMigration, "the issue-tree spec")
}

// typeMigrationBumpBounds is blockSchemaBumpBounds for the change whose type migrations own names:
// newest, the newest of the change's own, and before, the newest of the others numbered below it.
func typeMigrationBumpBounds(migrations []string, own *regexp.Regexp, change string) (before, newest string, err error) {
	for _, name := range migrations {
		if own.MatchString(name) {
			newest = max(newest, name)
		}
	}
	if newest == "" {
		return "", "", fmt.Errorf("store.TypeMigrations names no type migration of %s, so nothing moves the block schema's version", change)
	}
	for _, name := range migrations {
		if !own.MatchString(name) && name < newest {
			before = max(before, name)
		}
	}
	if before == "" {
		return "", "", fmt.Errorf("store.TypeMigrations names no type migration before %s, so no version stands before %s's", newest, change)
	}
	return before, newest, nil
}

// migrationVersion is the version of the migration name: the 14 digits it opens with.
func migrationVersion(t *testing.T, name string) int {
	t.Helper()
	version, err := strconv.Atoi(name[:14])
	if err != nil {
		t.Fatalf("read the version of %s: %v", name, err)
	}
	return version
}

// blockSchemaVersion is the version the store's type nodes build on database, which this tree's
// runner has migrated through the migration name and no further.
func blockSchemaVersion(t *testing.T, database *store.Store, name string) int {
	t.Helper()
	schema, err := schemanodes.Read(context.Background(), database.Pool)
	if err != nil {
		t.Fatalf("build the schema from the store's nodes through %s: %v", name, err)
	}
	return schema.Version
}

// The issue-tree spec moves the block schema one version, once (R21 of the spec): its elements
// stand at the version after the one the type migrations before the spec's own carry, however many
// type migrations it adds. The test reads the version blocks.json gives at this head and the one
// the store's type nodes build once every migration has run, which must agree. It then reads the
// version the nodes build on one store this tree's runner migrated, at two points
// (blockSchemaBumpBounds): through the newest type migration of store.TypeMigrations that is not
// the spec's and is numbered below the spec's newest, the version the store held before the spec's
// type migrations moved it, and then on through the spec's newest, as a deployment's database
// moves forward. A type migration of the spec that moves the version is numbered after every one
// before it that it did not add, each of which sets the version on every block type, so the store
// at the first point holds none of the spec's moves. A second bump between the two points is one
// step too many, and so is none; and a move of the spec numbered before the first point's
// migration shows here as a version the store builds there already. A type migration numbered
// after the spec's newest is at neither point, so its own move of blocks.json is no step here.
func TestTheIssueTreeSpecMovesTheBlockSchemaOneVersion(t *testing.T) {
	database := storetest.Open(t)
	file := readBlockSchemaFile(t)

	head, err := schemanodes.Read(context.Background(), database.Pool)
	if err != nil {
		t.Fatalf("build the schema from the store's nodes at this head: %v", err)
	}
	if head.Version != file.Version {
		t.Fatalf("the store's type nodes build version %d at this head, and %s is at version %d: a type migration and the file disagree",
			head.Version, blockSchemaFile, file.Version)
	}

	before, spec, err := blockSchemaBumpBounds(store.TypeMigrations)
	if err != nil {
		t.Fatal(err)
	}
	through := migrationVersion(t, before)
	migrated := openMigratedThrough(t, fmt.Sprintf("this tree's runner through %d (the block schema bump)", through), through)
	earlier := blockSchemaVersion(t, migrated, before)
	if err := migrated.MigrateThrough(context.Background(), migrationVersion(t, spec)); err != nil {
		t.Fatalf("migrate the store through %s on through %s: %v", before, spec, err)
	}
	moved := blockSchemaVersion(t, migrated, spec)
	t.Logf("a store migrated through %s, the newest type migration before the issue-tree spec's, builds version %d; through %s, the spec's newest, version %d; %s is at version %d",
		before, earlier, spec, moved, blockSchemaFile, file.Version)
	if moved != earlier+1 {
		t.Fatalf("a store migrated through %s builds version %d and one migrated through %s, the newest type migration before the issue-tree spec's, version %d: the spec moves the block schema %d steps, want one",
			spec, moved, before, earlier, moved-earlier)
	}
}

// The bump test reads its store at the two points the issue-tree spec's type migrations leave it,
// also once a later change adds a type migration of its own after them: that migration, which
// moves blocks.json again, is no bound of the test, so the test does not read the later move as a
// second step of the spec's, or the store before the spec's moves as one past them all.
func TestTheBlockSchemaBumpBoundsLeaveOutALaterTypeMigration(t *testing.T) {
	before, spec, err := blockSchemaBumpBounds(store.TypeMigrations)
	if err != nil {
		t.Fatal(err)
	}
	later := slices.Concat(store.TypeMigrations, []string{"29991231235959_graph_supertags_later_change.up.sql"})
	laterBefore, laterSpec, err := blockSchemaBumpBounds(later)
	if err != nil {
		t.Fatal(err)
	}
	if laterBefore != before || laterSpec != spec {
		t.Errorf("with a type migration added after the issue-tree spec's, the bump test migrates through %s and %s, want %s and %s as without it",
			laterBefore, laterSpec, before, spec)
	}
}

// The change request of The Spec moves the block schema one version, once, as the issue-tree spec
// does: a store this tree's runner migrated through the newest type migration that is not the
// change's and is numbered below the change's newest (typeMigrationBumpBounds) builds one version,
// and the same store migrated on through the change's newest builds the next. The number is the
// next one free when the change took it, and each branch chose its own against the tree it held,
// so the test asserts the step and not the number. A type migration numbered after the change's
// newest is at neither point.
func TestTheSpecChangeMovesTheBlockSchemaOneVersion(t *testing.T) {
	before, change, err := typeMigrationBumpBounds(store.TypeMigrations, specChangeMigration, "the change request")
	if err != nil {
		t.Fatal(err)
	}
	through := migrationVersion(t, before)
	migrated := openMigratedThrough(t, fmt.Sprintf("this tree's runner through %d (the change request's bump)", through), through)
	earlier := blockSchemaVersion(t, migrated, before)
	if err := migrated.MigrateThrough(context.Background(), migrationVersion(t, change)); err != nil {
		t.Fatalf("migrate the store through %s on through %s: %v", before, change, err)
	}
	moved := blockSchemaVersion(t, migrated, change)
	t.Logf("a store migrated through %s, the newest type migration before the change request's, builds version %d; through %s, the change's newest, version %d",
		before, earlier, change, moved)
	if moved != earlier+1 {
		t.Fatalf("a store migrated through %s builds version %d and one migrated through %s, the newest type migration before the change request's, version %d: the change moves the block schema %d steps, want one",
			change, moved, before, earlier, moved-earlier)
	}
}
