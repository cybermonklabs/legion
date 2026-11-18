// Package migrations carries the daemon's schema as SQL embedded in the binary,
// so a daemon can migrate a database with nothing but itself. Migrations are
// forward-only: each is applied once, in version order, and never reversed.
//
// A migration's file is named <version>_<slug>.up.sql, and its version is the
// leading digits read as an integer. The numbered versions are the sequence the
// daemon grew up with; each of those files keeps the number it landed with.
// Every migration added since is named for the UTC moment its author created
// it, 14 digits YYYYMMDDHHMMSS (date -u +%Y%m%d%H%M%S), so two branches open at
// the same time cannot both claim one version the way two branches each taking
// the next number did. A timestamp sorts after every numbered migration and
// timestamps sort among themselves by time, and migrationid.Check holds a set
// to that rule: the daemon's numbered series closed at
// 0067_phase_assignment.up.sql (migrationid's closings), so a set holding a
// number above it is refused.
package migrations

import (
	"cmp"
	"embed"
	"fmt"
	"io/fs"
	"slices"
	"strconv"
	"strings"

	"github.com/sjawhar/envoy/migrationid"
)

//go:embed *.up.sql
var files embed.FS

// Migration is one forward migration, identified by the version its file name
// carries.
type Migration struct {
	Version int
	Name    string
	SQL     string
}

// All reads every embedded migration in ascending version order.
func All() ([]Migration, error) {
	return Load(files)
}

// StepRecordsRollForward is the version of 20261117064152_step_starts_roll_forward.up.sql, the roll
// forward of a rollback of the release that records each step, whose header carries that
// release's way back (WayBack): the one spelling of it every test that runs, reads or re-arms that
// block names.
const StepRecordsRollForward = 20261117064152

// WayBack is the way back the header of migration version carries, as an operator runs it with
// the daemon stopped: the run of "-- " comment lines that closes with the line deleting that
// version's schema_version row, without the markers. A blank "--" line, or any line that is no
// such comment, ends the run before it, so the prose above the block is none of it. The header of
// StepRecordsRollForward carries one; docs/kubernetes.md carries the same block, and
// internal/textcheck holds the two to one text. A header with no such run, and a version not
// embedded, are errors, so no caller runs an empty block against a store.
func WayBack(version int) (string, error) {
	all, err := All()
	if err != nil {
		return "", err
	}
	return wayBack(all, version)
}

// wayBack is WayBack over the set all, which a test crafts.
func wayBack(all []Migration, version int) (string, error) {
	mark := fmt.Sprintf("delete from schema_version where version = %d;", version)
	for _, migration := range all {
		if migration.Version != version {
			continue
		}
		var block []string
		for _, line := range strings.Split(migration.SQL, "\n") {
			statement, comment := strings.CutPrefix(line, "-- ")
			if !comment {
				block = block[:0]
				continue
			}
			block = append(block, statement)
			if statement == mark {
				return strings.Join(block, "\n"), nil
			}
		}
		return "", fmt.Errorf("%s carries no way back closing with %q", migration.Name, mark)
	}
	return "", fmt.Errorf("no migration has version %d", version)
}

// Load reads every *.up.sql in fsys in ascending version order. A file whose
// name carries no version, and two files sharing one, are refused by name:
// either would otherwise apply in an order nobody chose or be skipped in
// silence. So is a set migrationid.Check refuses, which is what keeps a daemon
// carrying a version two branches could collide on from booting at all.
func Load(fsys fs.FS) ([]Migration, error) {
	return load(fsys, migrationid.Check)
}

// load is Load with the identifier check given, so a test can read a crafted set
// of its own few numbered migrations against its own closing number
// (migrationid.CheckClosedAt) rather than one shaped like the embedded set.
func load(fsys fs.FS, check func(map[int]string) error) ([]Migration, error) {
	names, err := fs.Glob(fsys, "*.up.sql")
	if err != nil {
		return nil, fmt.Errorf("list migrations: %w", err)
	}
	all := make([]Migration, 0, len(names))
	for _, name := range names {
		version, err := versionOf(name)
		if err != nil {
			return nil, err
		}
		sql, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, fmt.Errorf("read migration %s: %w", name, err)
		}
		all = append(all, Migration{Version: version, Name: name, SQL: string(sql)})
	}
	slices.SortFunc(all, func(a, b Migration) int { return cmp.Compare(a.Version, b.Version) })
	versions := make(map[int]string, len(all))
	for i, migration := range all {
		if i > 0 && migration.Version == all[i-1].Version {
			return nil, fmt.Errorf("migrations %s and %s share version %d", all[i-1].Name, migration.Name, migration.Version)
		}
		versions[migration.Version] = migration.Name
	}
	if err := check(versions); err != nil {
		return nil, err
	}
	return all, nil
}

func versionOf(name string) (int, error) {
	stem, ok := strings.CutSuffix(name, ".up.sql")
	if !ok {
		return 0, fmt.Errorf("migration %q does not end in .up.sql", name)
	}
	digits, _, ok := strings.Cut(stem, "_")
	if !ok {
		return 0, fmt.Errorf("migration %q is not named <version>_<slug>.up.sql", name)
	}
	version, err := strconv.Atoi(digits)
	if err != nil || version < 1 {
		return 0, fmt.Errorf("migration %q does not begin with a version number", name)
	}
	return version, nil
}
