package main

import (
	"context"
	"errors"
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

// blockSchemaBumpBounds is the two type migrations of migrations the bump test migrates a store
// through: spec, the newest of the issue-tree spec's own (issueTreeMigration), and before, the
// newest of the others numbered below it. A type migration numbered after spec, which a later
// change adds with its own move of blocks.json, is neither, so that move is no step of the spec's.
func blockSchemaBumpBounds(migrations []string) (before, spec string, err error) {
	for _, name := range migrations {
		if issueTreeMigration.MatchString(name) {
			spec = max(spec, name)
		}
	}
	if spec == "" {
		return "", "", errors.New("store.TypeMigrations names no type migration of the issue-tree spec, so nothing moves the block schema's version")
	}
	for _, name := range migrations {
		if !issueTreeMigration.MatchString(name) && name < spec {
			before = max(before, name)
		}
	}
	if before == "" {
		return "", "", fmt.Errorf("store.TypeMigrations names no type migration before %s, so no version stands before the issue-tree spec's", spec)
	}
	return before, spec, nil
}

// blockSchemaVersionThrough is the version the store's type nodes build on a store this tree's
// runner migrated through the migration name and no further.
func blockSchemaVersionThrough(t *testing.T, name string) int {
	t.Helper()
	ctx := context.Background()
	through, err := strconv.Atoi(name[:14])
	if err != nil {
		t.Fatalf("read the version of %s: %v", name, err)
	}
	migrated := storetest.OpenFrom(t, fmt.Sprintf("this tree's runner through %d (the block schema bump)", through), func(databaseURL string) error {
		database, err := store.Open(ctx, databaseURL)
		if err != nil {
			return fmt.Errorf("open the database: %w", err)
		}
		defer database.Pool.Close()
		return database.MigrateThrough(ctx, through)
	})
	schema, err := schemanodes.Read(ctx, migrated.Pool)
	if err != nil {
		t.Fatalf("build the schema from the store's nodes through %s: %v", name, err)
	}
	return schema.Version
}

// The issue-tree spec moves the block schema one version, once (R21 of the spec): its elements
// stand at the version after the one the type migrations before the spec's own carry, however many
// type migrations it adds. The test reads the version blocks.json gives at this head and the one
// the store's type nodes build once every migration has run, which must agree. It then reads the
// version the nodes build on two stores this tree's runner migrated (blockSchemaBumpBounds): one
// through the newest type migration of store.TypeMigrations that is not the spec's and is numbered
// below the spec's newest, the version the store held before the spec's type migrations moved it,
// and one through the spec's newest. A type migration of the spec that moves the version is
// numbered after every one before it that it did not add, each of which sets the version on every
// block type, so the first store holds none of the spec's moves. A second bump between the two
// stores is one step too many, and so is none; and a move of the spec numbered before the first
// store's migration shows here as a version that store builds already. A type migration numbered
// after the spec's newest is in neither store, so its own move of blocks.json is no step here.
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
	earlier, moved := blockSchemaVersionThrough(t, before), blockSchemaVersionThrough(t, spec)
	t.Logf("a store migrated through %s, the newest type migration before the issue-tree spec's, builds version %d; through %s, the spec's newest, version %d; %s is at version %d",
		before, earlier, spec, moved, blockSchemaFile, file.Version)
	if moved != earlier+1 {
		t.Fatalf("a store migrated through %s builds version %d and one migrated through %s, the newest type migration before the issue-tree spec's, version %d: the spec moves the block schema %d steps, want one",
			spec, moved, before, earlier, moved-earlier)
	}
}

// The bump test's two stores stand where the issue-tree spec's type migrations leave them, also
// once a later change adds a type migration of its own after them: that migration, which moves
// blocks.json again, is no bound of the test, so the test does not read the later move as a second
// step of the spec's, or the store before the spec's moves as one past them all.
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
