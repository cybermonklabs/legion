package pgmigrate

import (
	"os"
	"testing"

	pgparser "github.com/wasilibs/go-pgquery/parser"
)

// TestMain parses one statement with the census's parser before any test runs. That parser is
// libpg_query run as WebAssembly (github.com/wasilibs/go-pgquery/parser), which compiles its
// module on the process's first parse (wasmInitOnce.Do(initWASM) in getABI), about 1.7 s of CPU
// (set.go, the doc of LoadBuilt). The compile is setup that every test that parses shares: without
// this parse the process pays it inside whichever test parses first, and a test that parses
// beside that one waits on the same sync.Once, so a test's time would hold setup that is not its
// own. Here the package's time holds it once and no test's does. A parse that fails here would fail
// every test that parses, so it ends the binary before any test runs, naming the parser's error.
func TestMain(m *testing.M) {
	if _, err := pgparser.ParseToJSON("select 1"); err != nil {
		panic("parse a statement with libpg_query before the tests: " + err.Error())
	}
	os.Exit(m.Run())
}
