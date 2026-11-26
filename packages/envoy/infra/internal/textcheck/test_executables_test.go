package textcheck

//ci:reads packages/envoy/infra/**/*.go TestEveryProgramFileIsWrittenUnderTheForkLock calls testexec.RequireGuarded on ../.., whose Unguarded (packages/envoy/testexec/unguarded.go) walks it, passes over node_modules, vendor, testdata and dot directories, and through unguardedInPackage parses every .go file under packages/envoy/infra it finds, test files included

import (
	"testing"

	"github.com/sjawhar/envoy/testexec"
)

// Every file this module writes and then runs, or hands to a program that runs it, is written
// under the fork lock (docs/testing.md at the repository's root, "A file a test runs").
// testexec.RequireGuarded says what it reads and how it fails.
func TestEveryProgramFileIsWrittenUnderTheForkLock(t *testing.T) {
	testexec.RequireGuarded(t, "../..")
}
