package textcheck

import (
	"testing"

	"github.com/sjawhar/envoy/sourcecheck"
)

// Every database a Go file of this module creates is a copy of its source's files: a string
// literal that opens a database's creation holds `strategy file_copy` on the same line, beside it
// or in a literal after it (docs/testing.md, "A database a Go test creates"). The check reads every
// Go file of the module, tests included and infra with them, since most such statements live in a
// test's own helper, so a new helper written without the clause, or an edit that drops it, fails
// here naming its file and line, with no database needed. storetest's own test measures what one
// clone writes. packages/daemon/internal/textcheck runs the same check over that module.
func TestEveryCreatedDatabaseIsAFileCopy(t *testing.T) {
	sourcecheck.CheckFileCopies(t, "../..")
}
