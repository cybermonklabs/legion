package store

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
)

// deployerAdvisoryKey reads the key out of a pg_try_advisory_lock call written in a shell script,
// which is how the auto-deployer and its own test write theirs: one call, one integer literal.
// The test below reads the key the script uses rather than asserting a number of its own, so the
// relation it states is "the deployer takes the key this package defines" rather than a fourth
// copy of 8150001.
var deployerAdvisoryKey = regexp.MustCompile(`pg_try_advisory_lock\(\s*(-?[0-9]+)\s*\)`)

// deployerKeyScripts are the shell files that name the migration advisory key, as paths from this
// test's own directory.
//
// autodeploy_test.sh is checked beside autodeploy.sh because its copies are keys in the same
// sense: its fake psql answers the exact statement text `select pg_try_advisory_lock(<key>)` and
// exits 3 on any other statement, and its expectation line asserts that text. That makes the
// shell suite loud about drift between the two scripts, but only about drift between them - both
// could move away from migrationLockKey together and every check of this package would stay
// green. Checking all four literals in one Go test is what pins the deployer's world to the
// runner's, and it is the cheaper half of the job: the Go test needs no database and no shell.
var deployerKeyScripts = []string{
	filepath.Join("..", "..", "..", "deploy", "scripts", "autodeploy.sh"),
	filepath.Join("..", "..", "..", "deploy", "scripts", "autodeploy_test.sh"),
}

// The deployer reads schema_migrations under the migration advisory key, and the key it takes is
// a literal in its shell script, far from the Go constant every runner takes. Nothing but this
// test holds the two together: with the constant changed alone, the runner takes one key and the
// deployer polls another, so the deployer's read is granted at once while a migration is in
// flight and it reads a half-applied set.
//
// A script whose shape changed so far that no key can be read fails here too. A test that found
// nothing and passed would report that the keys agree when it had not looked at one.
func TestTheDeployerTakesTheMigrationKeyThisPackageDefines(t *testing.T) {
	for _, script := range deployerKeyScripts {
		text, err := os.ReadFile(script)
		if err != nil {
			t.Errorf("read the deployer script that takes the migration key: %v", err)
			continue
		}
		calls := deployerAdvisoryKey.FindAllStringSubmatch(string(text), -1)
		if len(calls) == 0 {
			t.Errorf("%s takes no pg_try_advisory_lock(<key>) this test can read, so nothing holds its key to migrationLockKey (%d); the script's shape changed and this test must be taught the new one",
				script, migrationLockKey)
			continue
		}
		for _, call := range calls {
			key, err := strconv.ParseInt(call[1], 10, 64)
			if err != nil {
				t.Errorf("%s takes advisory key %q, which is no int64: %v", script, call[1], err)
				continue
			}
			if key != migrationLockKey {
				t.Errorf("%s takes advisory key %d; migrationLockKey is %d. The deployer reads schema_migrations under its key and every runner holds migrations under this one, so the two must be the same number",
					script, key, migrationLockKey)
			}
		}
	}
}
