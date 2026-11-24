package textcheck

//ci:reads packages/daemon/**/*.go TestEveryProgramFileIsWrittenUnderTheForkLock calls testexec.RequireGuarded on ../.., whose Unguarded (packages/envoy/testexec/testexec.go:145) walks it (unguarded.go:85), passes over node_modules, vendor, testdata and dot directories (:94), and :169 parses every .go file under packages/daemon it finds, test files included

import (
	"testing"

	"github.com/sjawhar/envoy/testexec"
)

// Every file this module writes and then runs, or hands to a program that runs it, is written
// under the fork lock (docs/testing.md, "A file a test runs"). testexec.RequireGuarded says what
// it reads and how it fails.
func TestEveryProgramFileIsWrittenUnderTheForkLock(t *testing.T) {
	testexec.RequireGuarded(t, "../..")
}
