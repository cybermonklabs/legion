package main

import (
	"context"
	"regexp"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc/schemanodes"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// specChangeMigration is the name every type migration of the change whose approvals write the
// approved spec takes, <timestamp>_spec_change_<what>.up.sql: the type migrations this branch adds
// are the ones of store.TypeMigrations it names. The type migrations of the change before it,
// <timestamp>_issue_tree_<what>.up.sql, which moved the block schema to version 6, are the ones
// before this branch's.
var specChangeMigration = regexp.MustCompile(`^[0-9]{14}_spec_change_[0-9a-z_]+\.up\.sql$`)

// The branch moves the block schema one version, once (dispatch://INFRA-1253/spec R21): its
// elements stand at the version after the one the type migrations before it carry, however many
// type migrations it adds. The test reads the version blocks.json gives at this head and the one
// the store's type nodes build once every migration has run, which must agree, and then runs again,
// in order, the type migrations of store.TypeMigrations this branch did not add - the seed and each
// later one, which every set the version on every block type - through runTypeMigrationsAgain,
// which takes out first the rows of a literal one among them, and reads the version the nodes
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
	for _, name := range store.TypeMigrations {
		if specChangeMigration.MatchString(name) {
			ours = append(ours, name)
			continue
		}
		before = append(before, name)
	}
	if len(ours) == 0 {
		t.Fatal("store.TypeMigrations names no type migration of this branch, so nothing moves the block schema's version")
	}
	runTypeMigrationsAgain(t, ctx, database.Pool, before)
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
