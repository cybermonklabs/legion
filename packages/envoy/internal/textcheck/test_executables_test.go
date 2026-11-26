package textcheck

//ci:reads packages/envoy/**/*.go !packages/envoy/infra/** !packages/envoy/testdata/** TestEveryProgramFileIsWrittenUnderTheForkLock calls testexec.RequireGuarded on ../.., whose Unguarded (testexec/unguarded.go) walks it, passes over node_modules, vendor, testdata and dot directories and infra, a directory holding a go.mod of its own, and through unguardedInPackage parses every .go file it finds, test files included

import (
	"testing"

	"github.com/sjawhar/envoy/testexec"
)

// Every file this module writes and then runs, or hands to a program that runs it, is written
// under the fork lock (docs/testing.md, "A file a test runs"). testexec.RequireGuarded says what
// it reads and how it fails. The infra module below is read by its own check.
func TestEveryProgramFileIsWrittenUnderTheForkLock(t *testing.T) {
	testexec.RequireGuarded(t, "../..")
}
