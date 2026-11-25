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

// issueTreeMigration is the name every migration of dispatch://INFRA-1253 takes,
// <timestamp>_issue_tree_<what>.up.sql, as store's TestThisChildsMigrationsTouchNoTableAboveTheCensusLimit
// finds them: the type migrations this branch adds are the ones of store.TypeMigrations it names.
var issueTreeMigration = regexp.MustCompile(`^[0-9]{14}_issue_tree_[a-z0-9_]+\.up\.sql$`)

// The branch moves the block schema one version, once (dispatch://INFRA-1253/spec R21): its
// elements stand at the version after the one the type migrations before it carry, however many
// type migrations it adds. The test reads the version blocks.json gives at this head and the one
// the store's type nodes build once every migration has run, which must agree, and then the version
// the nodes build on a store this tree's runner migrated through the newest type migration of
// store.TypeMigrations this branch did not add and no further: the version the store held before
// this branch's type migrations moved it. A type migration of this branch that moves the version
// is numbered after every one it did not add, each of which sets the version on every block type,
// so that store holds none of this branch's moves. A second bump - blocks.json, or a type
// migration of this branch, moving past the version after that one - is one step too many, and so
// is none; and a move of this branch numbered before that migration shows here as a version that
// store builds already.
func TestTheBlockSchemaMovesOneVersionPastTheTypeMigrationsBeforeThisBranch(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	file := readBlockSchemaFile(t)

	head, err := schemanodes.Read(ctx, database.Pool)
	if err != nil {
		t.Fatalf("build the schema from the store's nodes at this head: %v", err)
	}
	if head.Version != file.Version {
		t.Fatalf("the store's type nodes build version %d at this head, and %s is at version %d: a type migration of this branch and the file disagree",
			head.Version, blockSchemaFile, file.Version)
	}

	var before, ours []string
	for _, name := range store.TypeMigrations {
		if issueTreeMigration.MatchString(name) {
			ours = append(ours, name)
			continue
		}
		before = append(before, name)
	}
	if len(ours) == 0 {
		t.Fatal("store.TypeMigrations names no type migration of this branch, so nothing moves the block schema's version")
	}
	newest := slices.Max(before)
	through, err := strconv.Atoi(newest[:14])
	if err != nil {
		t.Fatalf("read the version of %s: %v", newest, err)
	}
	earlierStore := storetest.OpenFrom(t, fmt.Sprintf("this tree's runner through %d (the block schema bump)", through), func(databaseURL string) error {
		database, err := store.Open(ctx, databaseURL)
		if err != nil {
			return fmt.Errorf("open the database: %w", err)
		}
		defer database.Pool.Close()
		return database.MigrateThrough(ctx, through)
	})
	earlier, err := schemanodes.Read(ctx, earlierStore.Pool)
	if err != nil {
		t.Fatalf("build the schema from the store's nodes through %s, the newest type migration before this branch's: %v", newest, err)
	}
	t.Logf("the type migrations before this branch's (%d of them) carry version %d; this branch's %v and %s carry version %d",
		len(before), earlier.Version, ours, blockSchemaFile, file.Version)
	if file.Version != earlier.Version+1 {
		t.Fatalf("%s is at version %d and the type migrations before this branch's carry version %d: the branch moves the block schema %d steps, want one",
			blockSchemaFile, file.Version, earlier.Version, file.Version-earlier.Version)
	}
}
