package store

import (
	"testing"

	"github.com/sjawhar/envoy/internal/pgmigrate"
)

// The migrations this binary embeds pass every check pgmigrate.Load makes: a start reads them with
// pgmigrate.LoadBuilt, so this test is where the whole check is made of the set Migrate applies,
// at every change to it. pgmigrate's TestLoadRefusesUnsafeNonTransactionalMigrations holds Load to
// refusing the unsafe marked statements this check exists for.
func TestTheEmbeddedMigrationsPassEveryCheckLoadMakes(t *testing.T) {
	if _, err := pgmigrate.Load(migrationFiles, "migrations"); err != nil {
		t.Fatalf("the embedded migrations fail pgmigrate.Load, which a start leaves to this test: %v", err)
	}
}
