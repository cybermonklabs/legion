package store

import (
	"context"
	"fmt"
	"slices"

	"github.com/sjawhar/legion/daemon/internal/store/migrations"
)

// stackMigrations are the versions cybermonklabs/legion#5169 and the pull requests it stands on
// (#5164, #5156, #5165, #5168, and dispatch://INFRA-5910's rekey of pull requests) add, which
// neither main nor INFRA-3320's branch recorded: the embedded versions main at dfe2c546f21f does
// not hold. A database either of those left lacks each of them numbered at or below its newest. A
// migration this stack adds goes in this list, or a test that builds main's database
// (MigrateMainsThrough) builds one that holds it, which no deployment of main has.
var stackMigrations = []int{
	20261117213545, 20261117223524, 20261117224125, 20261121165047, 20261121183336,
	20261122072945, 20261122074350, 20261122122826, 20261122131321, 20261123072431,
	20261123154051, 20261123200630, 20261123221142, 20261124023617, 20261126144204,
	20261126234607, 20261127142558, 20261127234212,
}

// MigrateMainsThrough applies every embedded migration numbered up to and including version but the
// stack's (stackMigrations), each as Migrate applies it: the database main's daemon left while
// version was its newest, which a test of an upgrade seeds with the rows that daemon wrote and then
// has this release's Migrate upgrade. It is the tests' alone: no daemon calls it.
func (s *Store) MigrateMainsThrough(ctx context.Context, version int) error {
	all, err := migrations.All()
	if err != nil {
		return err
	}
	for _, migration := range all {
		if migration.Version > version {
			return nil
		}
		if slices.Contains(stackMigrations, migration.Version) {
			continue
		}
		if _, err := s.apply(ctx, migration); err != nil {
			return fmt.Errorf("apply %s as main's run did: %w", migration.Name, err)
		}
	}
	return nil
}
