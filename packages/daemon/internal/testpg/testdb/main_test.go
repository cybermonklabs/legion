package testdb

import (
	"os"
	"testing"
)

// The pool hands a database to the next test only where Main runs (give), so this package's own
// tests of the pool run under it, as every package that takes a database does.
func TestMain(m *testing.M) {
	os.Exit(Main(m.Run))
}
