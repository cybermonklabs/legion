package main

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc/schemanodes"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// issueTreeMigration is the name every migration of dispatch://INFRA-1253 takes,
// <timestamp>_issue_tree_<what>.up.sql, as store's TestThisChildsMigrationsTouchNoTableAboveTheCensusLimit
// finds them: the type migrations this branch adds are the ones of typeMigrations it names.
var issueTreeMigration = regexp.MustCompile(`^[0-9]{14}_issue_tree_[a-z0-9_]+\.up\.sql$`)

// The branch moves the block schema one version, once (dispatch://INFRA-1253/spec R21): its
// elements stand at the version after the one the type migrations before it carry, however many
// type migrations it adds. The test reads the version blocks.json gives at this head and the one
// the store's type nodes build once every migration has run, which must agree, and then runs again,
// in order, the type migrations of typeMigrations this branch did not add - the seed and each
// later one, which every set the version on every block type - and reads the version the nodes
// build then, which is the version the store held before this branch's type migrations. A second
// bump - blocks.json, or a type migration of this branch, moving past the version after that one
// - is one step too many, and so is none.
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
	for _, migration := range typeMigrations {
		if issueTreeMigration.MatchString(filepath.Base(migration)) {
			ours = append(ours, filepath.Base(migration))
			continue
		}
		before = append(before, migration)
	}
	if len(ours) == 0 {
		t.Fatal("typeMigrations names no type migration of this branch, so nothing moves the block schema's version")
	}
	for _, migration := range before {
		text, err := os.ReadFile(filepath.Clean(migration))
		if err != nil {
			t.Fatalf("read the type migration %s: %v", filepath.Base(migration), err)
		}
		if _, err := database.Pool.Exec(ctx, string(text)); err != nil {
			t.Fatalf("run the type migration %s again: %v", filepath.Base(migration), err)
		}
	}
	earlier, err := schemanodes.Read(ctx, database.Pool)
	if err != nil {
		t.Fatalf("build the schema from the store's nodes as the type migrations before this branch's leave them: %v", err)
	}
	t.Logf("the type migrations before this branch's (%d of them) carry version %d; this branch's %v and %s carry version %d",
		len(before), earlier.Version, ours, blockSchemaFile, file.Version)
	if file.Version != earlier.Version+1 {
		t.Fatalf("%s is at version %d and the type migrations before this branch's carry version %d: the branch moves the block schema %d steps, want one",
			blockSchemaFile, file.Version, earlier.Version, file.Version-earlier.Version)
	}
}
