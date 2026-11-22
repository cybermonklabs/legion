package store

import (
	"io/fs"
	"path"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/sjawhar/envoy/internal/pgmigrate"
)

// The migrations this binary embeds pass every check pgmigrate.Load makes. Migrate reads them with
// pgmigrate.LoadBuilt, which leaves out Load's parse of each non-transactional migration, so a boot
// does not compile libpg_query; this test is where the whole check is made of the set Migrate
// applies, at every change to it. The planted migration proves it can fail: a non-transactional
// migration that is not one idempotent concurrent index build, which Load refuses and LoadBuilt,
// by design, does not.
func TestTheEmbeddedMigrationsPassEveryCheckLoadMakes(t *testing.T) {
	if _, err := pgmigrate.Load(migrationFiles, "migrations"); err != nil {
		t.Fatalf("the embedded migrations fail pgmigrate.Load, which Migrate leaves to this test: %v", err)
	}
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	planted := fstest.MapFS{}
	for _, entry := range entries {
		name := path.Join("migrations", entry.Name())
		data, err := migrationFiles.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		planted[name] = &fstest.MapFile{Data: data}
	}
	const plantedName = "29991231235959_planted_index.up.sql"
	// No IF NOT EXISTS: a build this mode cannot repeat safely, which concurrentIndex refuses.
	planted[path.Join("migrations", plantedName)] = &fstest.MapFile{
		Data: []byte("-- pgmigrate:non-transactional\ncreate index concurrently planted_index on issues (key);\n"),
	}
	if _, err := pgmigrate.Load(planted, "migrations"); err == nil || !strings.Contains(err.Error(), plantedName) {
		t.Fatalf("pgmigrate.Load of the embedded set with %s planted answered %v, want a refusal naming it", plantedName, err)
	}
	if _, err := pgmigrate.LoadBuilt(planted, "migrations"); err != nil {
		t.Fatalf("pgmigrate.LoadBuilt refused the planted migration (%v): it should leave that parse to this test and to the migration's own run", err)
	}
}
